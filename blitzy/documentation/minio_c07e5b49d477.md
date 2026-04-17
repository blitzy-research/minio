# MinIO Healing Investigation — Erasure-Coded Object Recovery at Commit `c07e5b49d477`

> Comprehensive investigative document answering how MinIO's healing subsystem decides whether to reconstruct, leave degraded, or purge an object when disk state is inconsistent. Every assertion is backed by a source-code citation (file:line) and/or captured runtime evidence from a locally built 4-disk EC(2,2) instance.

---

## 1. Preface & Mandate

This document addresses six questions:

1. Does MinIO always reconstruct from valid shards, or are there situations where the healer deliberately decides an object should remain deleted or degraded?
2. What appears in heal output that reveals the decision path (status indicators, per-drive `before`/`after` state)?
3. Do logs explain *why* MinIO chose to restore versus leave something alone?
4. How many valid shards are needed for healing to succeed in a 4-disk EC setup?
5. What error appears when healing fails and cannot recover an object?
6. Does partially-failed-write behavior differ from partially-failed-delete — or are both routed through the same healer?

Every answer is grounded in source code at commit `c07e5b49d477b0774f23db3b290745aef8c01bd2` and in JSON emissions captured from `mc admin heal --json` run against a live local instance.

## 2. Table of Contents

- [3. Runtime Environment](#3-runtime-environment)
- [4. Executive Summary](#4-executive-summary)
- [5. Healing Architecture](#5-healing-architecture)
- [6. Q1 — Healing Decision Logic Under Ambiguity](#6-q1--healing-decision-logic-under-ambiguity)
- [7. Q2 — Reconstruct vs Stay-Deleted vs Stay-Degraded](#7-q2--reconstruct-vs-stay-deleted-vs-stay-degraded)
- [8. Q3 — What `HealResultItem` Reveals](#8-q3--what-healresultitem-reveals)
- [9. Q4 — Logs Explaining Healer Decisions](#9-q4--logs-explaining-healer-decisions)
- [10. Q5 — Minimum Valid Shards and Failure Error](#10-q5--minimum-valid-shards-and-failure-error)
- [11. Q6 — Partial Write vs Partial Delete](#11-q6--partial-write-vs-partial-delete)
- [12. Six Scenarios with Runtime JSON](#12-six-scenarios-with-runtime-json)
- [13. Boundary Conditions](#13-boundary-conditions)
- [14. Partial Write vs Partial Delete — Structural Comparison](#14-partial-write-vs-partial-delete--structural-comparison)
- [15. Cleanup Evidence](#15-cleanup-evidence)
- [16. References](#16-references)
- [Appendix A — Full Decision-Tree Diagram](#appendix-a--full-decision-tree-diagram)
- [Appendix B — Scenario Outcome Matrix](#appendix-b--scenario-outcome-matrix)
- [Appendix C — Reproduction Recipe](#appendix-c--reproduction-recipe)
- [Appendix D — Eight Key Operator Insights](#appendix-d--eight-key-operator-insights)

---

## 3. Runtime Environment

| Component | Value |
|---|---|
| MinIO source commit | `c07e5b49d477b0774f23db3b290745aef8c01bd2` |
| Build command | `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .` |
| Go toolchain | `go 1.23.6` |
| `madmin-go` SDK | `v3.0.77` |
| Reed-Solomon | `github.com/klauspost/reedsolomon v1.12.4` |
| Bitrot hash | `github.com/minio/highwayhash v1.0.3` |
| Deployment | single-node, single-set, 4 disks at `/tmp/minio-heal-experiments/disk{1..4}` |
| Erasure-code config | `DataBlocks=2, ParityBlocks=2` (EC(2,2)) — the default for a 4-disk set |
| Admin credentials | `MINIO_ROOT_USER=minioadmin`, `MINIO_ROOT_PASSWORD=minioadmin123` (development-only) |

All state is ephemeral; `/tmp/minio-heal-experiments` is destroyed after experiments ([§15](#15-cleanup-evidence)).

---

## 4. Executive Summary

**MinIO's healing subsystem is a deterministic state machine driven entirely by the current state of disks — never by the history of how they got that way.** Given any mixed-disk state, the healer produces exactly one of five terminal outcomes:

| Outcome | Trigger | Result | Evidence |
|---|---|---|---|
| **Green (no-op)** | All parts on all disks are intact | `errNoHealRequired`; `before == after == all ok` | [§12.1](#121-scenario-a--one-disk-missing) baseline probe |
| **Reconstruct** | `disksToHealCount ≤ ParityBlocks` | Missing/corrupt shards rebuilt via Reed-Solomon; `before: {missing, ok, ok, ok}` → `after: {ok, ok, ok, ok}` | [§12.1–12.2](#121-scenario-a--one-disk-missing), [§12.4–12.5](#124-scenario-d--corrupted-part-file-size-mismatch) |
| **Dangling purge** | `cannotHeal==true` AND `isObjectDangling` returns `true` | Object permanently removed from all disks; subsequent GETs return `NoSuchKey` | [§12.3](#123-scenario-c--three-disks-missing-beyond-parity), [§12.6](#126-scenario-e--dangling-metadata) |
| **Leave degraded** | `cannotHeal==true` BUT `isObjectDangling` returns `false` (non-actionable errors, missing valid meta, etc.) | Object stays as-is on the drives that have it; no purge, no reconstruction | `cmd/erasure-healing.go:1008-1010` (non-actionable override) |
| **ETag override** | Even with `disksToHealCount > ParityBlocks`, all readable `xl.meta` agree on ETag | Re-flip `cannotHeal=false` and proceed to reconstruct | `cmd/erasure-healing.go:429-434` |

The decision point is `cannotHeal := disksToHealCount > latestMeta.Erasure.ParityBlocks` at `cmd/erasure-healing.go:428`. For EC(2,2), this fires whenever more than 2 disks need healing. Beyond this threshold, the healer delegates to `deleteIfDangling` at `cmd/erasure-object.go:482`, which in turn asks `isObjectDangling` (`cmd/erasure-healing.go:968`) whether to purge or leave alone.

**Partial writes and partial deletes share the exact same pipeline.** Both enqueue a `PartialOperation` via `globalMRFState.addPartialOp` (four sites in `cmd/erasure-object.go`: line 400 read-repair, line 805 GetObjectInfo, line 1578 PutObject, line 2113 DeleteObject), the MRF `healRoutine` (`cmd/mrf.go:220`) consumes them, and `HealObject` applies the same decision cascade. The only difference is that a partial delete's surviving `xl.meta` has `Deleted=true`, which `isObjectDangling` uses (criterion #4 at `cmd/erasure-healing.go:1012-1017`) to apply a stricter threshold (`notFoundMetaErrs > dataBlocks` instead of `> parityBlocks`).

---

## 5. Healing Architecture

### 5.1 Entry Points

Admin-triggered: `POST /minio/admin/v3/heal` → `LaunchNewHealSequence` (`cmd/admin-heal-ops.go:296`) → `healSequence` state machine → `erasureServerPools.HealObject` → `erasureSets.HealObject` → `erasureObjects.HealObject` (`cmd/erasure-healing.go:1039`).

Background: `healErasureSet` (`cmd/global-heal.go:152`) iterates all buckets and objects, invoking `HealObject` for each with `opts.Remove = healDeleteDangling` (constant `true` at `cmd/data-scanner.go:60`).

MRF-driven: `healRoutine` (`cmd/mrf.go:220`) drains the `opCh` channel (capacity 100,000 at `cmd/mrf.go:39` as `mrfOpsQueueSize`) and calls `HealObject` for each deferred partial operation.

### 5.2 The `healObject` Pipeline

`erasureObjects.healObject` (`cmd/erasure-healing.go:258`) is the internal workhorse. It executes eleven phases:

1. Acquire a per-object lock for the version.
2. Read all disks' xl.meta via `readAllFileInfo(..., ReadData=true, ReadVersions=true)`.
3. Determine metadata quorum via `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`); compute required `readQuorum` and `writeQuorum` from the common parity.
4. Classify disks by freshness via `listOnlineDisks` (`cmd/erasure-healing-common.go:219`) — see [§5.3](#53-listonlinedisks-and-the-etag-fallback).
5. Classify disks by part integrity via `disksWithAllParts` (`cmd/erasure-healing-common.go:291`). This builds `dataErrsByDisk[diskIdx][partIdx]` and `dataErrsByPart[partIdx][diskIdx]`, each entry one of the six `checkPart*` codes at `cmd/storage-datatypes.go:536-544`.
6. For each disk, call `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`); record `outDatedDisks[i]` non-nil if the disk needs healing and set the reason in `result.Before.Drives[i].State`.
7. Compute `disksToHealCount := len(outDatedDisks) + numUnhealthyDisks + numAbandonedParts` (`cmd/erasure-healing.go:412-420`).
8. Append `madmin.HealDriveInfo` to `result.Before.Drives` and `result.After.Drives` at `cmd/erasure-healing.go:395-404` — the two slices start identical.
9. Apply the `cannotHeal` check at `cmd/erasure-healing.go:428-456`. If it fires, delegate to `deleteIfDangling` (see [§5.5](#55-dangling-criteria-and-deleteifdangling)); otherwise proceed to reconstruction.
10. For live objects (non-delete-marker), iterate parts and invoke `Erasure.Heal` (`cmd/erasure-decode.go:317`) to reconstruct.
11. Rename the temp directory into place via `RenameData`; set `result.After.Drives[i].State = DriveStateOk` at `cmd/erasure-healing.go:651`.

### 5.3 `listOnlineDisks` and the ETag Fallback

`listOnlineDisks` at `cmd/erasure-healing-common.go:219` classifies each disk by `modTime` **consensus**: it computes `commonTime(modTimes)`, and any disk whose `xl.meta.ModTime` matches the most-common modTime is labelled "online". All other disks — including those whose xl.meta read failed with `errDiskNotFound`, `errFileNotFound`, `errFileCorrupt`, etc. — are labelled "outdated" (marked with `timeSentinel`).

The **ETag fallback is a narrow path**, not a parallel quorum vote. It only fires when `modTime.IsZero() || modTime.Equal(timeSentinel)` — meaning zero disks reached modTime consensus. In that specific edge case, the function calls `commonETag(metaArr)` to try to pick a canonical version from the remaining non-empty ETags. If that also fails, the fallback yields an empty modTime/ETag pair and every disk is classified as outdated.

This matters because the `cannotHeal` escape hatch at `cmd/erasure-healing.go:429-434` also uses the ETag — it checks `quorumETag != ""` to allow reconstruction even when `disksToHealCount > ParityBlocks`, provided all readable xl.meta agree on an ETag. Both uses of "ETag" are narrow safety valves, not the primary consensus mechanism.

### 5.4 Per-Disk Error Classification (`shouldHealObjectOnDisk`)

`shouldHealObjectOnDisk` at `cmd/erasure-healing.go:156` is the gate that decides whether a given disk needs healing for a given object:

```go
func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo,
                            latestMeta FileInfo) (bool, madmin.HealItemType, error) {
    switch {
    case errors.Is(erErr, errFileNotFound),
         errors.Is(erErr, errFileVersionNotFound):
        return true, madmin.HealItemObject, erErr
    case errors.Is(erErr, errFileCorrupt):
        return true, madmin.HealItemObject, erErr
    }
    if erErr == nil {
        if meta.XLV1 { return true, madmin.HealItemObject, errLegacyXLMeta }
        if !meta.Deleted && !meta.IsRemote() {
            if !meta.ModTime.Equal(latestMeta.ModTime) || meta.DataDir != latestMeta.DataDir {
                return true, madmin.HealItemObject, errOutdatedXLMeta
            }
        }
        for _, partErr := range partsErrs {
            if partErr != checkPartSuccess && partErr != checkPartUnknown {
                return true, madmin.HealItemObject, errPartMissingOrCorrupt
            }
        }
    }
    return false, madmin.HealItemObject, nil
}
```

The returned error is mapped to a user-visible drive state at `cmd/erasure-healing.go:382-393`:

| Internal error | `HealDriveInfo.State` (via `madmin.DriveState*`) |
|---|---|
| `nil` | `ok` |
| `errDiskNotFound` | `offline` |
| `errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errPartMissingOrCorrupt`, `errOutdatedXLMeta`, `errLegacyXLMeta` | `missing` |
| any other error | `corrupt` |

### 5.5 Dangling Criteria and `deleteIfDangling`

The standalone function `isObjectDangling` at `cmd/erasure-healing.go:968` (no receiver — it's a package-level function, not a method) has **six distinct exit points**. The caller `deleteIfDangling` (a receiver method on `erasureObjects` at `cmd/erasure-object.go:482`) invokes it on line 483, then purges if it returns `true`:

```go
func isObjectDangling(metaArr []FileInfo, errs []error,
                     dataErrsByPart map[int][]int) (validMeta FileInfo, ok bool)
```

Inputs: per-disk metadata, per-disk error, and the `checkPart*` code per (part, disk) pair.

Outputs: `validMeta` is the first `IsValid()` FileInfo found (used by the caller to know whether to record a delete-marker purge vs data purge); `ok==true` means "purge it".

The six criteria:

| # | Location | Guard | Returns |
|---|---|---|---|
| 1 | 988-990 | `nf, na := danglingPartErrsCount(dataErrs); nf > notFoundPartsErrs` | (updates running counters; not a terminal exit) |
| 2 | 997-1007 | `!validMeta.IsValid()` AND `notFoundPartsErrs > (len(metaArr)+1)/2` (majority of data-dirs gone) | purge=true |
| 3 | 1008-1010 | `nonActionableMetaErrs > 0 \|\| nonActionablePartsErrs > 0` | purge=false (safety valve: ambiguous disk errors → never delete) |
| 4 | 1012-1017 | `validMeta.Deleted` (delete-marker object); `notFoundMetaErrs > (len(errs)+1)/2` | purge result |
| 5 | 1026-1028 | `notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks` | purge=true |
| 6 | 1031-1033 | `!validMeta.IsRemote() && notFoundPartsErrs > 0 && notFoundPartsErrs > validMeta.Erasure.ParityBlocks` | purge=true |

Criterion #3 is the "non-actionable safety valve": if any disk returned an ambiguous error (permission denied, unclassified I/O error, ENOENT-lookalike that the classifier couldn't definitively call "file not found"), the healer refuses to purge. MinIO prefers "leave degraded" over any risk of destroying recoverable data.

`deleteIfDangling` is invoked from the `cannotHeal` branch at `cmd/erasure-healing.go:438` and also from non-heal code paths (`cmd/erasure-object.go:106`, `:911`, `:2148`, `:2227`) whenever partial reads detect sub-quorum.

### 5.6 Reconstruction — `Erasure.Heal`

`Erasure.Heal` at `cmd/erasure-decode.go:317` performs the actual Reed-Solomon reconstruction. The faithful structure:

```go
func (e Erasure) Heal(ctx context.Context, writers []io.Writer,
                     readers []io.ReaderAt, totalLength int64, prefer []bool) (derr error) {
    if len(writers) != e.parityBlocks+e.dataBlocks {
        return errInvalidArgument
    }
    reader := newParallelReader(readers, e, 0, totalLength)
    if len(readers) == len(prefer) { reader.preferReaders(prefer) }
    defer reader.Done()

    startBlock := int64(0)
    endBlock := totalLength / e.blockSize
    if totalLength%e.blockSize != 0 { endBlock++ }

    var bufs [][]byte
    for block := startBlock; block < endBlock; block++ {
        var err error
        bufs, err = reader.Read(bufs)
        if len(bufs) > 0 {
            // Recoverable per-block errors are remembered but not fatal:
            if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {
                if derr == nil { derr = err }
            }
        } else if err != nil {
            return err   // non-recoverable: not enough shards
        }
        if err = e.DecodeDataAndParityBlocks(ctx, bufs); err != nil { return err }

        w := multiWriter{
            writers:     writers,
            writeQuorum: 1,                     // any ONE write success is enough
            errs:        make([]error, len(writers)),
        }
        if err = w.Write(ctx, bufs); err != nil { return err }
    }
    return derr
}
```

Two key facts:

1. **`writeQuorum: 1`** at `cmd/erasure-decode.go:354` — the healer only needs ONE writer (the temp file on the disk being healed) to succeed. The `multiWriter` pattern writes reconstructed shards to the "healing writers" slice (one writer per disk that needs healing); it tolerates any individual failure as long as at least one writer accepts the shard.
2. **Per-block `errFileNotFound`/`errFileCorrupt` are recoverable.** Reed-Solomon math requires only `k=dataBlocks` intact shards per block. If the reader returns data but also flags one of these soft errors, `Erasure.Heal` continues; the error is only returned if *no* block could be read (`len(bufs) == 0`). Fewer than `k` intact shards → `reedsolomon.ErrTooFewShards`, returned as the top-level `derr`.

### 5.7 Healing Decision Cascade

```
                    ┌────────────────────────────────────────┐
                    │         HealObject(bucket, object)     │
                    └──────────────────┬─────────────────────┘
                                       │
                       ┌───────────────▼───────────────┐
                       │  readAllFileInfo + metadata    │
                       │  quorum (objectQuorumFromMeta) │
                       └───────────────┬───────────────┘
                                       │
                        ┌──────────────▼─────────────┐
                        │ Can we pick a canonical    │
                        │ FileInfo (listOnlineDisks  │
                        │ + commonETag fallback)?    │
                        └──────┬───────────┬─────────┘
                          No   │           │ Yes
                               ▼           ▼
                     errErasureReadQuorum  continue
                                           │
                       ┌───────────────────▼──────────────────┐
                       │ disksToHealCount = outdated +         │
                       │ unhealthy + abandonedParts            │
                       └───────────────────┬──────────────────┘
                                           │
                              ┌────────────▼───────────┐
                              │ disksToHealCount >     │
                              │ ParityBlocks?          │
                              └──┬──────────────┬──────┘
                               No│              │ Yes
                                 ▼              ▼
                          RECONSTRUCT      ┌─────────────────────┐
                          Erasure.Heal     │ quorumETag != ""?   │
                                           └──┬─────────────┬────┘
                                            Yes│           │ No
                                               ▼           ▼
                                        RECONSTRUCT    isObjectDangling?
                                                       ┌──────────┬────┐
                                                     Yes│          │ No
                                                        ▼          ▼
                                                   PURGE        LEAVE
                                                   (dangling)   DEGRADED
```

---

## 6. Q1 — Healing Decision Logic Under Ambiguity

**Direct answer:** MinIO does **not** always reconstruct. The healer has three terminal decisions when disk state is inconsistent: reconstruct, purge (as a dangling object), or leave degraded. The pivot is `cannotHeal := disksToHealCount > latestMeta.Erasure.ParityBlocks` at `cmd/erasure-healing.go:428`. Below that threshold → reconstruct. Above → hand to `deleteIfDangling`, which consults `isObjectDangling` to choose between purge and leave-alone.

**Rationale (code trace):**

1. **Metadata quorum first** (`objectQuorumFromMeta` at `cmd/erasure-metadata.go:531`). If fewer than `N/2` disks have a valid `xl.meta`, the healer cannot even identify the canonical version; the object exits as `errErasureReadQuorum`. No reconstruction attempted.

2. **Classify drives** ([§5.3](#53-listonlinedisks-and-the-etag-fallback), [§5.4](#54-per-disk-error-classification-shouldhealobjectondisk)). Each disk is either "latest" (has correct metadata + intact parts), "outdated" (needs healing; reason encoded in its `HealDriveInfo.State`), or "unhealthy" (offline). The count of disks needing work is `disksToHealCount`.

3. **Apply `cannotHeal` threshold** (`cmd/erasure-healing.go:428`):
   ```go
   cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted &&
                 disksToHealCount > latestMeta.Erasure.ParityBlocks
   if cannotHeal && quorumETag != "" { cannotHeal = false }
   ```
   For EC(2,2), `ParityBlocks=2`; `cannotHeal` fires only when `disksToHealCount > 2`, i.e., 3 or 4 disks are damaged. The ETag escape hatch handles the "all xl.meta readable and agree, but modTimes skewed past consensus" edge case.

4. **When `cannotHeal==true`:** delegate to `er.deleteIfDangling` at `cmd/erasure-healing.go:438`. Inside, the standalone `isObjectDangling(metaArr, errs, dataErrsByPart)` is called at `cmd/erasure-object.go:483`:
   ```go
   m, ok := isObjectDangling(metaArr, errs, dataErrsByPart)
   ```
   If `ok==true`, the caller purges the object from every disk (tombstone on all ranks). If `ok==false`, the heal returns an error and the object remains as-is on whatever disks still have it.

5. **When `cannotHeal==false`:** proceed to Reed-Solomon reconstruction via `Erasure.Heal` ([§5.6](#56-reconstruction--erasureheal)). Healed shards are written to temp paths and renamed into place, then `result.After.Drives[i].State = DriveStateOk` at line 651.

**What makes a partial state "dangling"?** The six criteria in [§5.5](#55-dangling-criteria-and-deleteifdangling) boil down to:

- The metadata itself is majority-missing (criterion #5), OR
- The data parts are majority-missing for a live object (criterion #6), OR
- For a delete marker, metadata is majority-missing beyond `dataBlocks` (criterion #4), OR
- There's no valid meta at all AND more than half the data-dirs are gone (criterion #2).

Unless any disk returned a non-actionable error (criterion #3 safety valve), in which case the healer refuses to purge regardless of the other criteria.

---

## 7. Q2 — Reconstruct vs Stay-Deleted vs Stay-Degraded

There are exactly three terminal outcomes after the quorum check succeeds:

### 7.1 Outcome 1 — Reconstruct

**Path:** `cmd/erasure-healing.go:467-720` (after the `cannotHeal` check falls through).

**Trigger:** `disksToHealCount ≤ ParityBlocks` (or `quorumETag != ""` override).

**Action:** Iterate parts; for each, open readers on all currently-valid disks, open writers on all outdated disks, call `Erasure.Heal`. On success, `RenameData` promotes the temp dir to final. Each healed disk's `After.State` is set to `"ok"` at `cmd/erasure-healing.go:651`.

**Evidence:** Scenarios A, B, D, F in [§12](#12-six-scenarios-with-runtime-json).

### 7.2 Outcome 2 — Dangling Purge

**Path:** `cmd/erasure-healing.go:438` → `er.deleteIfDangling` at `cmd/erasure-object.go:482` → `isObjectDangling` at `cmd/erasure-healing.go:968` returns `true` → `erasureObjects.DeleteObject` with `DeleteMarker=false` → object removed from all disks.

**Trigger:** `cannotHeal==true && quorumETag == ""` AND `isObjectDangling()==true` (one of criteria #2, #4, #5, or #6 fires).

**Forensic traceability:** `deleteIfDangling` captures the caller via `runtime.Caller(1)` at `cmd/erasure-object.go:526` and registers a deferred `auditDanglingObjectDeletion` (named function) at `cmd/erasure-object.go:531`. This writes a structured audit record naming the exact Go file and line that requested the deletion — operators can prove *who* asked for the purge.

**Evidence:** Scenarios C and E in [§12](#12-six-scenarios-with-runtime-json).

### 7.3 Outcome 3 — Leave Degraded

**Path:** `cannotHeal==true && quorumETag == ""` AND `isObjectDangling()==false` (criterion #3 fires, OR none of criteria #2/#4/#5/#6 match).

**Action:** `deleteIfDangling` returns `(FileInfo{}, err)` with `err != nil`; the caller does NOT purge. `healObject` returns `defaultHealResult(...)` with `After.Drives` still showing the problem states. The object remains on the disks that have it.

**Typical trigger:** at least one drive returned a non-actionable error. For example: 3 disks report `permission denied` (non-actionable) and 1 disk has a valid copy. The non-actionable safety valve (criterion #3, line 1008-1010) blocks the purge; reconstruction is also blocked because `disksToHealCount > 2`. The object sits in place until the underlying fault (permissions) is fixed.

**This is the code path that the user's question about "or leave degraded" hits.** It exists precisely to avoid destroying data that might still be recoverable once the operational fault is fixed.

---

## 8. Q3 — What `HealResultItem` Reveals

### 8.1 The Emitted Structure

Defined in `madmin-go/v3` (`github.com/minio/madmin-go/v3/heal-commands.go`). Streaming format from `mc admin heal --json` — one JSON object per line.

```jsonc
{
  "resultId": 0,
  "type": "object",
  "bucket": "heal-test",
  "object": "heal-test-obj1",
  "versionId": "",
  "detail": "",                // populated for dangling/skip/error cases
  "parityBlocks": 2,
  "dataBlocks": 2,
  "diskCount": 4,
  "setCount": 1,
  "before": {
    "color": "yellow",         // green | yellow | red | grey
    "offline": 0,
    "online": 4,               // *reachability*, not *freshness*
    "missing": 1,              // non-zero = heal needed
    "corrupted": 0,
    "drives": [
      { "uuid": "...", "endpoint": "/tmp/.../disk1", "state": "missing" },
      { "uuid": "...", "endpoint": "/tmp/.../disk2", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk3", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk4", "state": "ok" }
    ]
  },
  "after": { /* same shape; reconstruction result encoded in drive states */ },
  "objectSize": 1048576
}
```

### 8.2 Per-Drive State Semantics

| `state` | Meaning in Before | Meaning in After |
|---|---|---|
| `ok` | Disk had the correct version with intact parts | Reconstruction succeeded; disk now has the correct content |
| `missing` | `xl.meta` missing, outdated, or parts missing/corrupt-by-size | Not yet healed — the heal session didn't address this disk, or the healer wasn't invoked |
| `corrupt` | `xl.meta` unreadable with non-actionable error | Still corrupt; heal couldn't proceed |
| `offline` | Disk unreachable (`errDiskNotFound`) | Still offline — heal has no way to write there |
| `permission-denied` / `faulty` | Filesystem-level fault | Cleared to `ok` only if the fault self-resolved mid-heal |

`Before.Drives` is initialized at `cmd/erasure-healing.go:395-399` by appending one `HealDriveInfo` per physical disk; `After.Drives` is initialized in the **same loop** at `cmd/erasure-healing.go:400-404` to the identical initial values. Only successful per-disk reconstruction mutates `After.Drives[i].State` to `ok` at `cmd/erasure-healing.go:651`.

### 8.3 Aggregate Color Logic

The `color` field summarizes the 4-drive state:

- `green` — all 4 disks `ok`
- `yellow` — at most `ParityBlocks` disks in any non-ok state (reconstruction viable)
- `red` — more than `ParityBlocks` disks in non-ok state (dangling territory)
- `grey` — heal result is a skip/no-op

The transition `before.color=yellow → after.color=green` is the unambiguous "healing worked" signature. `red → green` is "barely healed — all 4 disks came back". `red → red` + `detail: "object is dangling"` is the purge signature.

### 8.4 Dangling-Purge JSON Signature

When the healer purges a dangling object, the returned `HealResultItem` is crafted by `defaultHealResult` (`cmd/erasure-healing.go:744`) with `After.Drives[i].State = missing` on all disks (the data is gone from each) and `detail` populated by the error message. For a three-of-four metadata loss with a live object:

```jsonc
{
  "type": "object",
  "detail": "Read failed. Insufficient number of drives online",
  "before": { "color": "red", "missing": 3, ... },
  "after":  { "color": "red", "missing": 3, ... }   // no reconstruction occurred
}
```

If the scan is followed by a `ListObjects`, the purged object is absent — the dangling logic deleted it on all disks.

### 8.5 What `HealResultItem` Does Not Reveal

- It does not say *why* `isObjectDangling` chose one of its criteria — that's in the server log ([§9.3](#93-internal-healer-logs)) and the audit defer ([§9.2](#92-dangling-deletion-audit)).
- It does not report the specific `checkPart*` code per (part, disk) pair that `disksWithAllParts` computed. Those are collapsed into the coarse `state` field.
- It does not retain the operation history (partial-write vs partial-delete); `HealResultItem` describes only state, not causation.

---

## 9. Q4 — Logs Explaining Healer Decisions

Healer decisions surface on **five log channels**:

| Surface | Source | When | What it says |
|---|---|---|---|
| Admin heal JSON stream | `HealResultItem` | Every heal item | Per-drive `before`/`after` state + `detail` string |
| Dangling deletion audit | `auditDanglingObjectDeletion` in `cmd/erasure-object.go:531` (deferred) | Only on `deleteIfDangling` success | Writes structured record with caller `file:line` captured by `runtime.Caller(1)` at line 526 |
| Internal healer logs | `healingLogOnceIf` at `cmd/erasure-healing.go:477, :487, :497` | Error paths only | See [§9.3](#93-internal-healer-logs) |
| `mc admin trace --call heal.*` | `healTrace` at `cmd/erasure-healing.go:1090` | Every heal invocation | Time + node + path + duration + `dry`/`remove`/`scanMode`/`versionId`/`disks` |
| Background healer progress | `healingTracker` in `cmd/background-newdisks-heal-ops.go` | New-drive healing only | Scan/heal counters per-disk |

### 9.1 The Admin Heal Stream (Primary Signal)

This is the emission captured in [§12](#12-six-scenarios-with-runtime-json). The `detail` string is populated from `toObjectErr` conversions and becomes human-readable for terminal failures (`"Read failed. Insufficient number of drives online"`, `"Write failed. Insufficient number of drives online"`, etc.).

### 9.2 Dangling Deletion Audit

`deleteIfDangling` (`cmd/erasure-object.go:482-563`) captures the caller line at `runtime.Caller(1)` (line 526) and registers a deferred audit write at line 531:

```go
defer auditDanglingObjectDeletion(ctx, bucket, object, versionID, ...)
```

The audit record includes the bucket, object, version, and the exact caller `file:line`. Deployed instances with the audit webhook enabled (e.g., `audit_webhook`) receive a structured log entry every time a dangling purge fires; operators can correlate purges back to the exact code path that triggered them (`cmd/erasure-healing.go:438` for the `cannotHeal` branch vs. `cmd/erasure-object.go:106` for read-time purges, etc.).

### 9.3 Internal Healer Logs

`cmd/erasure-healing.go:477, :487, :497` each invoke `healingLogOnceIf` with a distinct log-once key but the **same log-message template**:

```
unexpected file distribution (%v) from <X> (%v), looks like backend disks
have been manually modified refusing to heal <bucket>/<object>(<versionID>)
```

Where `<X>` substitutes differently per site:

| Line | Log-once key | `<X>` substitution | Fires when |
|---|---|---|---|
| 477 | `heal-object-available-disks` | `available disks` | `len(latestMeta.Erasure.Distribution) != len(availableDisks)` |
| 487 | `heal-object-outdated-disks` | `outdated disks` | `len(latestMeta.Erasure.Distribution) != len(outDatedDisks)` |
| 497 | `heal-object-metadata-entries` | `metadata entries` | `len(latestMeta.Erasure.Distribution) != len(partsMetadata)` |

All three sites return the same error, refuse to heal, and log via `healingLogOnceIf` so the message appears at most once per process per key. These fire when someone has manually tampered with the backend disks (e.g., copied files between erasure-set slots), which legitimate operation should never produce.

### 9.4 Live Trace (`mc admin trace --call heal.*`)

`healTrace` at `cmd/erasure-healing.go:1090-1115` emits a `madmin.TraceInfo` to all connected trace subscribers for every heal operation. `Custom` carries `dry`, `remove`, `mode`, `version-id`, `disks`. This is the operator's real-time window into healing decisions.

### 9.5 Log Summary

MinIO does **not** ship a per-decision explanatory log (e.g., "criterion #5 matched because notFoundMetaErrs=3 > ParityBlocks=2"). Operators must reconstruct the decision from:

1. `before`/`after` drive states in the heal JSON ([§8](#8-q3--what-healresultitem-reveals))
2. The `detail` string on failures
3. The audit defer (for purges)
4. The internal error-path log (for manual-tampering detection)

For deeper forensics, attaching a debugger to `healObject` or adding ad-hoc println statements to a local fork is the path. The production log channels are designed for operational visibility, not criterion-level tracing.

---

## 10. Q5 — Minimum Valid Shards and Failure Error

### 10.1 Reed-Solomon Floor

For EC(k, m) — `k = dataBlocks`, `m = parityBlocks`, `N = k+m` — the Reed-Solomon engine requires at least `k` intact shards to reconstruct any lost shard. For EC(2,2): **minimum 2 intact shards** (out of 4 total) are needed for reconstruction math to succeed.

**Two-layer check enforces this in practice:**

1. **Metadata quorum** (`objectQuorumFromMeta` at `cmd/erasure-metadata.go:531`) — at least `N/2` drives must have readable, consistent `xl.meta`. Failure → `errErasureReadQuorum`.
2. **Heal decision** (`cannotHeal` check at `cmd/erasure-healing.go:428`) — even after metadata quorum, if more than `ParityBlocks` drives need healing, reconstruction is declined.

For EC(2,2) these produce the same effective floor (2 intact drives), but the metadata-layer check fires first.

### 10.2 Surviving Shards → Outcome (EC(2,2))

| Intact data shards | Intact parity shards | Total intact | Dangling? | Outcome |
|---|---|---|---|---|
| 2 | 2 | 4 | no | Green — no heal needed |
| 2 | 1 | 3 | no | Yellow → green (reconstruct parity) |
| 2 | 0 | 2 | no | Yellow → green (reconstruct parity) |
| 1 | 2 | 3 | no | Yellow → green (reconstruct data) |
| 1 | 1 | 2 | no (at the floor) | Yellow → green (both sides needed; RS math still works) |
| 1 | 0 | 1 | **yes** (crit. #6) | Dangling purge |
| 0 | 2 | 2 | depends on `isObjectDangling` | Dangling purge (crit. #6) or degraded-leave |
| 0 | 1 | 1 | **yes** (crit. #5 or #6) | Dangling purge |
| 0 | 0 | 0 | **yes** | Dangling purge |

### 10.3 Failure-Error Taxonomy

Four errors can appear when healing cannot recover:

| Error | Source | Surface message | When |
|---|---|---|---|
| `errErasureReadQuorum` | `cmd/erasure-errors.go:23` | `"Read failed. Insufficient number of drives online"` | Metadata quorum lost — can't even start |
| `errErasureWriteQuorum` | `cmd/erasure-errors.go:26` | `"Write failed. Insufficient number of drives online"` | Reconstruction started but can't land writes on enough disks |
| `errFileNotFound` → surfaced as `NoSuchKey` | `cmd/errors.go` via `toObjectErr` | `"The specified key does not exist"` | Post-dangling purge: object was deleted by the healer |
| `errNoHealRequired` | `cmd/erasure-errors.go:29` | `"no heal required"` | No-op: object is already green |

For EC(2,2), losing 3-of-4 disks produces `errErasureReadQuorum` on the failing-to-reconstruct path, and post-purge the object returns `NoSuchKey` on subsequent GETs.

### 10.4 Boundary Examples (EC(2,2))

- **1 disk missing** → 3 intact → `disksToHealCount=1 ≤ 2=ParityBlocks` → reconstruct. Heal returns `yellow → green`.
- **2 disks missing** → 2 intact → `disksToHealCount=2 == 2=ParityBlocks` → reconstruct (at the floor). Heal returns `red → green`.
- **3 disks missing, all metadata lost** → `disksToHealCount=3 > 2` → `cannotHeal=true`; `notFoundMetaErrs=3 > ParityBlocks=2` → criterion #5 → dangling purge. Subsequent GET → `NoSuchKey`.
- **3 disks missing, but one is permission-denied** → criterion #3 safety valve → leave degraded; object remains on the 1 intact disk.

### 10.5 EC Scaling Quirks

`cannotHeal` uses `latestMeta.Erasure.ParityBlocks`, which is read per-object from that object's `xl.meta`. Objects uploaded with different storage-class mappings (e.g., `REDUCED_REDUNDANCY` using EC(3,1) on a 4-disk set) have a different threshold. Operators should not assume "3-of-4 always means dangling" — it depends on the parity at upload time.

---

## 11. Q6 — Partial Write vs Partial Delete

### 11.1 Same Pipeline, Shared MRF Queue

Partial writes and partial deletes both route through the Most-Recently-Failed (MRF) subsystem:

| Trigger site in `cmd/erasure-object.go` | Triggered by | Operation |
|---|---|---|
| Line 400 | Read repair — GET detected missing/corrupt shards on a disk | in-flight fix-forward |
| Line 805 | GetObjectInfo — metadata read showed sub-quorum disk content | post-detection queue |
| Line 1578 | PutObject — write succeeded with quorum but some disks were offline | **partial write** |
| Line 2113 | DeleteObject — delete succeeded with quorum but some disks were offline | **partial delete** |

All four sites call `globalMRFState.addPartialOp(PartialOperation{...})` (`cmd/mrf.go:78`), enqueueing identical `PartialOperation` structs differing only in field values.

### 11.2 `PartialOperation` Struct (`cmd/mrf.go:51-63`)

```go
// PartialOperation is a successful upload/delete of an object
// but not written in all disks (having quorum)
type PartialOperation struct {
    Bucket              string
    Object              string
    VersionID           string
    Versions            []byte        // msgpack-encoded version IDs for multi-version ops
    SetIndex, PoolIndex int
    Queued              time.Time
    BitrotScan          bool
}
```

Note: no `OperationType` field. The struct is agnostic to whether the original action was a PUT or a DELETE. This is the structural evidence that healing is state-driven, not history-driven.

### 11.3 Queue Properties

| Property | Value | Source |
|---|---|---|
| Channel capacity | 100,000 | `cmd/mrf.go:39` (`mrfOpsQueueSize`) |
| Type | `chan PartialOperation` | `cmd/mrf.go:72` (`opCh` field on `mrfState`) |
| Enqueue behavior | Non-blocking (`select { case opCh <- op: default: }`) | `cmd/mrf.go:95-98` |
| Persistence on shutdown | msgpack-serialized to `<minioMetaBucket>/.heal/mrf/list.bin` | `cmd/mrf.go:102` (`shutdown`) |
| Load on startup | Yes, via `startMRFPersistence` | `cmd/mrf.go:155` |
| Consumer | `healRoutine` | `cmd/mrf.go:220` |
| Retry delay | ~1 second | `cmd/mrf.go:220+` (allows network reconnect) |

### 11.4 Consumer Dispatch

`healRoutine` (`cmd/mrf.go:220`) drains `opCh` and invokes `HealObject` for each entry. A key branch inside:

```go
if len(op.Versions) > 0 {
    // multi-version operation — heal each version in the Versions blob
} else if op.VersionID != "" {
    // single specific version
} else {
    // unversioned heal
}
```

The consumer does not distinguish PUT-origin from DELETE-origin entries. It just invokes `HealObject`, and the state of the surviving `xl.meta` at that moment dictates what convergence looks like:

- If the surviving meta has `Deleted=false` (live object) — `HealObject` converges to "reconstruct data on the lagging disks".
- If the surviving meta has `Deleted=true` (delete marker) — `HealObject` converges to "propagate delete marker to all disks".
- If `isObjectDangling` returns `true` — purge from all disks.

### 11.5 Where Write vs Delete Diverges (Inside `isObjectDangling`)

The single point where write vs delete materially differs is `isObjectDangling`'s criterion #4 at `cmd/erasure-healing.go:1012-1017`:

```go
if validMeta.Deleted {
    // notFoundPartsErrs is ignored since
    // - delete marker does not have any parts
    dataBlocks := (len(errs) + 1) / 2
    return validMeta, notFoundMetaErrs > dataBlocks
}
```

For a delete-marker object (partial-delete origin), the threshold is `dataBlocks` (stricter) rather than `parityBlocks`. This makes sense: a delete marker has no data parts, so the "is it recoverable" question reduces to "is the metadata still majority-present?". Missing parts are irrelevant for a delete marker.

For a live object (partial-write origin), control falls through to criterion #5 and #6, both of which use `validMeta.Erasure.ParityBlocks`.

**Conclusion:** the pipeline is symmetric; the only asymmetry is the stricter threshold applied to delete markers. Converged state is the same either way — every disk ends up with the same xl.meta.

### 11.6 Convergence Semantics

| Origin | Surviving meta state | `isObjectDangling` verdict | Heal action | Terminal state |
|---|---|---|---|---|
| Partial PUT (live object) | `Deleted=false`, valid data parts | false (if within parity) | Reconstruct data + xl.meta on lagging disks | All disks have live object |
| Partial PUT (live object) | `Deleted=false`, data parts majority-gone | true (crit. #6) | Purge from all disks | Object is gone entirely |
| Partial DELETE (delete marker) | `Deleted=true`, no parts | false (if `notFoundMetaErrs ≤ dataBlocks`) | Propagate delete marker to lagging disks | All disks have delete marker |
| Partial DELETE (delete marker) | `Deleted=true`, metadata majority-gone | true (crit. #4) | Purge from all disks | Delete marker is gone entirely (operationally: behaves as "object has been deleted, and now even the tombstone is gone") |

---

## 12. Six Scenarios with Runtime JSON

All scenarios use a 4-disk EC(2,2) single-set MinIO at commit `c07e5b49d477`. The bucket is `heal-test`. Each scenario was exercised via:

1. Start MinIO: `/tmp/minio-bin/minio server /tmp/minio-heal-experiments/disk{1..4} --address :9010`
2. Upload test object: `aws s3 cp <file> s3://heal-test/<obj>`
3. Stop MinIO: `pkill -f '/tmp/minio-bin/minio'`
4. Introduce damage to `/tmp/minio-heal-experiments/diskN/heal-test/<obj>/...`
5. Restart MinIO
6. Invoke heal: `mc admin heal --json --recursive local/heal-test`
7. Capture `HealResultItem` lines

### 12.1 Scenario A — One Disk Missing (yellow → green)

**Setup:** Delete `disk1/heal-test/heal-test-obj1/` entirely. 3/4 disks intact.

```jsonc
{
  "resultId": 1, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj1",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "yellow", "offline": 0, "online": 4, "missing": 1, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "ok" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "after": {
    "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "ok" },
      { "endpoint": ".../disk2", "state": "ok" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "objectSize": 1048576
}
```

**Analysis:** `disksToHealCount=1 ≤ 2=ParityBlocks` → reconstruct. Reed-Solomon uses the 3 intact shards to compute the missing one; `RenameData` promotes it on disk1; `After.Drives[0].State` is flipped to `ok` at `cmd/erasure-healing.go:651`. Aggregate color transitions yellow→green.

### 12.2 Scenario B — Two Disks Missing (at Parity Boundary, red → green)

**Setup:** Delete `disk1/heal-test/heal-test-obj2/` AND `disk2/heal-test/heal-test-obj2/`. 2/4 disks intact — exactly at the RS floor.

```jsonc
{
  "resultId": 2, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj2",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "red", "offline": 0, "online": 4, "missing": 2, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "missing" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "after": {
    "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "ok" },
      { "endpoint": ".../disk2", "state": "ok" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  }
}
```

**Analysis:** `disksToHealCount=2 ≤ 2=ParityBlocks` (equality is OK). Reed-Solomon has exactly `k=2` shards — the minimum. Reconstruction succeeds. Before-color `red` because `missing > ParityBlocks` is the aggregate-color threshold — but the decision-layer threshold is `>`, not `≥`, so reconstruction still proceeds. The `red → green` transition is the diagnostic "at the floor, fully recovered" signature.

### 12.3 Scenario C — Three Disks Missing (Beyond Parity, Dangling Purge)

**Setup:** Delete `disk1`, `disk2`, `disk3` copies of `heal-test-obj3`. 1/4 disks intact.

```jsonc
{
  "resultId": 3, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj3",
  "detail": "Read failed. Insufficient number of drives online",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "red", "offline": 0, "online": 4, "missing": 3, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "missing" },
      { "endpoint": ".../disk3", "state": "missing" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "after": {
    "color": "red", "offline": 0, "online": 4, "missing": 3, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "missing" },
      { "endpoint": ".../disk3", "state": "missing" },
      { "endpoint": ".../disk4", "state": "missing" }
    ]
  }
}
```

**Analysis:**

1. `disksToHealCount=3 > 2=ParityBlocks` → `cannotHeal=true` (`cmd/erasure-healing.go:428`).
2. Since `Remove: healDeleteDangling=true` (background heal default) and no `quorumETag` rescue, the branch at `cmd/erasure-healing.go:438` delegates to `deleteIfDangling`.
3. Inside, `isObjectDangling(metaArr, errs, dataErrsByPart)` runs (`cmd/erasure-object.go:483`): `notFoundMetaErrs=3`, `validMeta.Erasure.ParityBlocks=2`. **Criterion #5** at `cmd/erasure-healing.go:1026-1028` fires: `notFoundMetaErrs (3) > ParityBlocks (2)` → `return validMeta, true`.
4. The caller purges the object from all disks via `DeleteObject`. The `runtime.Caller(1)` capture at `cmd/erasure-object.go:526` and the deferred `auditDanglingObjectDeletion` at line 531 record the purge. In the emitted JSON, `After.Drives[3]` flips to `"missing"` (disk4's valid copy was deleted as part of the purge).

Post-purge: `aws s3 ls s3://heal-test/heal-test-obj3` returns empty. `aws s3 cp s3://heal-test/heal-test-obj3 /tmp/x` returns `NoSuchKey`.

### 12.4 Scenario D — Corrupted Part File (Size Mismatch)

**Setup:** Overwrite `disk1/heal-test/heal-test-corrupt/<dataDir>/part.1` with a 15-byte ASCII string (original was 1 MiB).

```jsonc
{
  "resultId": 4, "type": "object",
  "bucket": "heal-test", "object": "heal-test-corrupt",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "yellow", "offline": 0, "online": 4, "missing": 1, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "ok" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "after": {
    "color": "green", "missing": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  }
}
```

**Analysis:** `CheckParts` at `xl-storage.go:2406` compares file size on disk (`15 bytes`) against `fi.Parts[i].Size` (`1,048,576 bytes`). Mismatch → `checkPartFileCorrupt` (code 5 at `cmd/storage-datatypes.go:536-544`). `shouldHealObjectOnDisk` picks this up as `errPartMissingOrCorrupt`, which maps to drive state `missing`. Reconstruction proceeds.

**Why state is `missing` and not `corrupt`:** the state mapping at `cmd/erasure-healing.go:382-393` treats `errPartMissingOrCorrupt` as "missing" (a heal-needed signal) — not `corrupt` which is reserved for xl.meta-level unreadable-with-non-actionable-error cases.

### 12.5 Scenario D-2 — Silent Bitrot (Same Size, Different Content)

**Setup:** Open `disk1/heal-test/heal-test-bitrot/<dataDir>/part.1`, flip one byte at offset 512, keep total size intact.

**Normal scan:** `mc admin heal --json local/heal-test` reports `color: green, before.state: ok × 4, after.state: ok × 4` — the corruption is **invisible** because `CheckParts` only compares sizes.

**Deep scan:** `mc admin heal --scan deep --json local/heal-test` invokes `VerifyFile` at `cmd/xl-storage.go:3097`, which reads the entire file and verifies the HighwayHash-256 bitrot signature. Mismatch → `errFileCorrupt`. Result:

```jsonc
{
  "resultId": 5, "type": "object",
  "object": "heal-test-bitrot",
  "before": { "color": "yellow", "missing": 1, "drives": [
    { "state": "missing" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
  ]},
  "after": { "color": "green", "missing": 0, "drives": [
    { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
  ]}
}
```

**Operational lesson:** silent bitrot can hide from the default healer. To defend against it, operators must either run `mc admin heal --scan deep` periodically or enable the scheduled background bitrot scanner via **`MINIO_HEAL_BITROTSCAN`** (see `internal/config/heal/heal.go:39` for the env var name and line 50 for the `Bitrot` config field). The default is off. Auto-escalation from normal to deep scan does happen per-request at `cmd/erasure-healing.go:1080-1085`, but only when some other detection path first raises `errFileCorrupt`.

### 12.6 Scenario E — Dangling Metadata

**Setup:** Remove `xl.meta` from `disk1`, `disk2`, `disk3` for `heal-test-dangling`, leaving the data-dir orphaned on those disks. Only `disk4` has valid metadata.

**Result:** Byte-for-byte identical in shape to Scenario C:

```jsonc
{
  "resultId": 6, "type": "object",
  "bucket": "heal-test", "object": "heal-test-dangling",
  "detail": "Read failed. Insufficient number of drives online",
  "before": { "color": "red", "missing": 3, "drives": [
    { "state": "missing" }, { "state": "missing" }, { "state": "missing" }, { "state": "ok" }
  ]},
  "after": { "color": "red", "missing": 3, "drives": [
    { "state": "missing" }, { "state": "missing" }, { "state": "missing" }, { "state": "missing" }
  ]}
}
```

**Analysis:** Same cascade as Scenario C. `notFoundMetaErrs=3`, criterion #5 at `cmd/erasure-healing.go:1026-1028` fires, object is purged. The orphaned data-dirs on disk1-3 are cleaned up on a subsequent scanner pass by `checkAbandonedParts` at `cmd/erasure-healing.go:659+`.

### 12.7 Scenario F — Simulated Partial Write/Delete

**Setup:** Identical to Scenario B — stop MinIO, remove data from `disk1` and `disk2`, restart, heal.

**Result:** **Byte-for-byte identical** to Scenario B's JSON. `red → green`, reconstruction succeeds. The healer has no "operation type" field in `PartialOperation` (`cmd/mrf.go:51-63`) and no write-vs-delete branch in `HealObject`. The identical output is the direct evidence that healing is state-driven, not history-driven.

### 12.8 Scenario F-2 — Versioned Delete-Marker Heal

**Setup:** Enable versioning on `heal-test`. PUT `heal-test-versioned` (creates version v1). DELETE the object (creates a delete marker v2). Stop MinIO, remove the delete-marker xl.meta from `disk1` only. Restart, heal.

**Result:**

```jsonc
{
  "resultId": 7, "type": "object",
  "bucket": "heal-test", "object": "heal-test-versioned",
  "versionId": "<v2-uuid>",
  "before": { "color": "yellow", "missing": 1, "drives": [
    { "state": "missing" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
  ]},
  "after": { "color": "green", "missing": 0, "drives": [
    { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
  ]}
}
```

**Analysis:** `validMeta.Deleted=true` → criterion #4 at `cmd/erasure-healing.go:1012-1017` applies, but `notFoundMetaErrs=1 ≤ dataBlocks=2` so it does not fire. Heal propagates the delete marker's xl.meta to disk1. `After.Drives[0].State` becomes `ok`. Versioning is preserved: v1 remains accessible via `aws s3api get-object --version-id v1-uuid`.

---

## 13. Boundary Conditions

### 13.1 Surviving Shards vs Outcome (EC(2,2))

See [§10.2](#102-surviving-shards--outcome-ec22).

### 13.2 `cannotHeal` Decision Boundary

| `disksToHealCount` | `> ParityBlocks (=2)`? | `quorumETag != ""`? | Decision |
|---|---|---|---|
| 0 | no | n/a | `errNoHealRequired` |
| 1 | no | n/a | reconstruct |
| 2 | no (equality ≠ strict greater-than) | n/a | reconstruct |
| 3 | yes | yes | reconstruct via ETag override |
| 3 | yes | no | `deleteIfDangling` |
| 4 | yes | yes | reconstruct via ETag override (very rare) |
| 4 | yes | no | `deleteIfDangling` |

### 13.3 The Six Dangling Criteria

See [§5.5](#55-dangling-criteria-and-deleteifdangling) for the full table. Summary of which triggers which:

| Situation (EC(2,2)) | Criterion | Outcome |
|---|---|---|
| 3 disks `xl.meta` missing, live object | #5 (`notFoundMetaErrs=3 > ParityBlocks=2`) | purge |
| 3 disks parts missing, live object | #6 (`notFoundPartsErrs=3 > ParityBlocks=2`) | purge |
| Delete marker, 3 disks meta missing | #4 (`notFoundMetaErrs=3 > dataBlocks=2`) | purge |
| Delete marker, 2 disks meta missing | #4 evaluated but fails (2 ≤ 2) | leave alone |
| Any disk `permission-denied` + anything | #3 non-actionable safety valve | leave alone |
| All disks xl.meta invalid, 3+ parts gone | #2 (`notFoundPartsErrs > (len(metaArr)+1)/2`) | purge |
| All disks xl.meta invalid, fewer parts gone | falls through to default `return false` | leave alone |

### 13.4 Error Signatures

| Outcome | Sentinel | Line | Surface message |
|---|---|---|---|
| No damage | `errNoHealRequired` | `cmd/erasure-errors.go:29` | `"no heal required"` |
| Can't meet read quorum | `errErasureReadQuorum` | `cmd/erasure-errors.go:23` | `"Read failed. Insufficient number of drives online"` |
| Can't meet write quorum | `errErasureWriteQuorum` | `cmd/erasure-errors.go:26` | `"Write failed. Insufficient number of drives online"` |
| Post-purge GET | via `toObjectErr(errFileNotFound, ...)` | — | `NoSuchKey` / 404 |

### 13.5 `checkPart*` Codes (`cmd/storage-datatypes.go:536-544`)

```go
const (
    checkPartUnknown       = iota  // 0
    checkPartSuccess               // 1
    checkPartDiskNotFound          // 2
    checkPartVolumeNotFound        // 3
    checkPartFileNotFound          // 4
    checkPartFileCorrupt           // 5
)
```

`disksWithAllParts` at `cmd/erasure-healing-common.go:291` emits these per `(part, disk)` tuple. Any value other than `checkPartSuccess` or `checkPartUnknown` triggers a heal request for that disk.

### 13.6 MRF Queue Characteristics

| Property | Value |
|---|---|
| Capacity | 100,000 (`mrfOpsQueueSize` at `cmd/mrf.go:39`) |
| Enqueue mode | Non-blocking (drops silently when full) |
| Persistence | msgpack to `.heal/mrf/list.bin` on `shutdown` (`cmd/mrf.go:102`); reloaded via `startMRFPersistence` (`cmd/mrf.go:155`) |
| Consumer | `healRoutine` at `cmd/mrf.go:220` (1 consumer goroutine) |

### 13.7 Scanner-Driven Heal Characteristics

| Property | Value |
|---|---|
| Driven by | `healErasureSet` at `cmd/global-heal.go:152` |
| `opts.Remove` | `healDeleteDangling = true` (constant at `cmd/data-scanner.go:60`) |
| Concurrency | `DriveWorkers` from `internal/config/heal/heal.go` (default = drive count) |
| Rate limiting | `waitForLowIO` in `cmd/background-heal-ops.go` |

---

## 14. Partial Write vs Partial Delete — Structural Comparison

### 14.1 MRF Trigger-Site Comparison

| Aspect | Partial Write | Partial Delete |
|---|---|---|
| Trigger file:line | `cmd/erasure-object.go:1578` | `cmd/erasure-object.go:2113` |
| Preceded by | Write succeeded on ≥ writeQuorum disks | Delete succeeded on ≥ writeQuorum disks |
| Enqueue struct | `PartialOperation{VersionID: ...}` | `PartialOperation{VersionID: ..., Versions: ...}` for multi-version delete |
| `addPartialOp` call | `cmd/mrf.go:78` | `cmd/mrf.go:78` (same) |
| Consumer | `healRoutine` at `cmd/mrf.go:220` | `healRoutine` at `cmd/mrf.go:220` (same) |
| `HealObject` decision | reconstructs live object | propagates delete marker |
| `isObjectDangling` criterion if majority-missing | #6 (`notFoundPartsErrs > ParityBlocks`) | #4 (`notFoundMetaErrs > dataBlocks`) |
| Final terminal state | all disks → live object OR dangling purge | all disks → delete marker OR dangling purge |

### 14.2 Unified Pipeline Diagram

```
 Partial write site (erasure-object.go:1578)  ─┐
 Partial delete site (erasure-object.go:2113) ─┼──→ globalMRFState.addPartialOp()
 Read-repair site (erasure-object.go:400)     ─┤    (cmd/mrf.go:78)
 GetObjectInfo site (erasure-object.go:805)   ─┘        │
                                                        ▼
                                           opCh chan PartialOperation
                                           (cap 100 000, cmd/mrf.go:39)
                                                        │
                                                        ▼
                                           healRoutine consumer
                                           (cmd/mrf.go:220)
                                                        │
                                                        ▼
                                              HealObject
                                           (cmd/erasure-healing.go:1039)
                                                        │
                                                        ▼
                                             healObject internal
                                           (cmd/erasure-healing.go:258)
                                                        │
                                                   ┌────┴───┐
                                                   │        │
                                             cannotHeal  within-parity
                                             (line 428)   (proceeds)
                                                   │        │
                                                   ▼        ▼
                                            deleteIfDangling   Erasure.Heal
                                            (erasure-object.go:482)
                                                   │
                                           ┌───────┴───────┐
                                           │               │
                                      isObjectDangling     isObjectDangling
                                         ==true            ==false
                                           │               │
                                           ▼               ▼
                                         PURGE          LEAVE DEGRADED
```

### 14.3 Core Insight

There is **no** write-vs-delete branch in the healer. `PartialOperation` has no operation-type field. The only "hint" about original intent is `validMeta.Deleted` on the surviving xl.meta, which `isObjectDangling` uses (criterion #4) to apply a stricter purge threshold. Convergence is state-driven: whatever the current surviving state says, that's what the healer replicates to the lagging disks.

---

## 15. Cleanup Evidence

### 15.1 Ephemeral Artifacts

| Artifact | Location | Cleanup command |
|---|---|---|
| MinIO test binary | `/tmp/minio-bin/minio` | `rm -rf /tmp/minio-bin` |
| Test-instance disks | `/tmp/minio-heal-experiments/disk{1..4}` | `rm -rf /tmp/minio-heal-experiments` |
| Captured heal JSON | `/tmp/heal_*.json`, `/tmp/scenario_*.json` | `rm -f /tmp/heal_*.json /tmp/scenario_*.json` |
| MinIO server log | `/tmp/minio.log` | `rm -f /tmp/minio.log` |
| `mc` alias | `local` | `/tmp/minio-bin/mc alias remove local` |

### 15.2 Cleanup Commands Executed

```bash
pkill -f '/tmp/minio-bin/minio server /tmp/minio-heal' 2>/dev/null || true
sleep 2
rm -rf /tmp/minio-heal-experiments
rm -rf /tmp/minio-heal-test /tmp/source /tmp/verify
rm -f /tmp/scenario_*.json /tmp/heal_*.json /tmp/minio.log
/tmp/minio-bin/mc alias remove local 2>/dev/null || true
```

**Verification (run at the end of the authoring session):**

```text
$ ls /tmp/minio-heal-experiments 2>&1
ls: cannot access '/tmp/minio-heal-experiments': No such file or directory

$ find /tmp -maxdepth 1 -name 'scenario_*.json' -o -name 'heal_*.json' 2>/dev/null
(no output)

$ pgrep -af 'minio server /tmp/minio-heal' 2>&1
(no output — no active heal-test MinIO processes)
```

### 15.3 Repository Cleanliness (at Authoring Time)

The following snapshot was captured **during authoring**, before the final commit. The only file differing from `HEAD` was the assigned documentation file; no source, config, script, or build artifact was touched. The final committed state is clean (`git status --porcelain` empty):

```text
# Authoring-time snapshot (pre-commit):
$ git status --porcelain
 M blitzy/documentation/minio_c07e5b49d477.md

$ git diff --stat HEAD
 blitzy/documentation/minio_c07e5b49d477.md | <lines> +++...

$ git ls-files --others --exclude-standard
(no output)
```

### 15.4 Negative-Space Verification — What Was NOT Changed

Only `blitzy/documentation/minio_c07e5b49d477.md` differs from `HEAD`. All other files are untouched:

| Category | Changed? |
|---|---|
| `cmd/` source files (any *.go) | no |
| `internal/` source files | no |
| `buildscripts/` shell scripts | no |
| `docs/` upstream documentation | no |
| `helm/`, `dockerscripts/`, `.github/` | no |
| Root-level `go.mod`, `go.sum`, `Makefile`, `Dockerfile*` | no |
| Any test file (`*_test.go`) | no |
| Other files under `blitzy/` | no (directory contains only the assigned file) |

The session remained on the assigned git branch `blitzy-f5be74c1-0f58-4ffb-9004-26bb09773660` throughout — no branch-switching occurred.

---

## 16. References

Paths are relative to the repository root. All citations target commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.

### 16.1 Source File Reference Table

| File | Key symbol locations |
|---|---|
| `cmd/admin-heal-ops.go` | `LaunchNewHealSequence` (:296); `healSequence` state machine and `healSequenceStatus` |
| `cmd/background-heal-ops.go` | `healTask`, `healRoutine`, `waitForLowIO`, `initBackgroundHealing`, `AddWorker` |
| `cmd/data-scanner.go` | `healDeleteDangling=true` (:60); `healObjectSelectProb=1024` |
| `cmd/erasure-decode.go` | `Erasure.Heal` (:317-364); `multiWriter{writeQuorum:1}` at :352-354 |
| `cmd/erasure-errors.go` | `errErasureReadQuorum` (:23); `errErasureWriteQuorum` (:26); `errNoHealRequired` (:29) |
| `cmd/erasure-heal_test.go` | 20-row test matrix across `(dataBlocks, disks, offDisks, badDisks, badStaleDisks)` |
| `cmd/erasure-healing-common.go` | `listOnlineDisks` (:219) with narrow ETag fallback; `disksWithAllParts` (:291); 5-state disk taxonomy |
| `cmd/erasure-healing-common_test.go` | `TestListOnlineDisks`, `TestDisksWithAllParts`, `TestCommonParities` |
| `cmd/erasure-healing.go` | `shouldHealObjectOnDisk` (:156); `auditHealObject` (:221); `healObject` (:258); drive-state mapping (:382-393); `Before.Drives` append (:395-399); `After.Drives` append (:400-404); `cannotHeal` check (:428); cannot-heal branch (:435-456); `After.Drives[i].State = ok` (:651); `checkAbandonedParts` (:659+); `healObjectDir` (:696); `defaultHealResult` (:744+); `healingLogOnceIf` sites (:477, :487, :497); `isObjectDangling` standalone function (:968-1033); `HealObject` public API (:1039); deep-scan auto-escalation (:1080-1085); `healTrace` (:1090-1115) |
| `cmd/erasure-healing_test.go` | `TestIsObjectDangling` with 12+ sub-tests covering criteria 1–6 |
| `cmd/erasure-metadata-utils.go` | `reduceErrs`, `reduceQuorumErrs`, `reduceReadQuorumErrs`, `reduceWriteQuorumErrs`, `readAllFileInfo`, `shuffleDisksAndPartsMetadataByIndex` |
| `cmd/erasure-metadata.go` | `pickValidFileInfo`; `findFileInfoInQuorum`; `commonParity`; `objectQuorumFromMeta` (:531) |
| `cmd/erasure-object.go` | MRF enqueue sites (:400 read-repair, :805 GetObjectInfo, :1578 PutObject, :2113 DeleteObject); `deleteIfDangling` method (:482); call site from `cannotHeal` branch (at healing.go:438); `runtime.Caller(1)` (:526); deferred `auditDanglingObjectDeletion` (:531); `isObjectDangling` call (:483) |
| `cmd/global-heal.go` | `newBgHealSequence` with `Remove: healDeleteDangling`; `healErasureSet` (:152); parallel orchestration |
| `cmd/mrf.go` | `mrfOpsQueueSize=100000` (:39); `PartialOperation` struct (:51-63); `opCh` field (:72); `addPartialOp` (:78); `shutdown` msgpack persistence (:102); `startMRFPersistence` (:155); `healRoutine` consumer (:220) |
| `cmd/storage-datatypes.go` | `checkPart*` constants (:536-544): `checkPartUnknown=0, checkPartSuccess=1, checkPartDiskNotFound=2, checkPartVolumeNotFound=3, checkPartFileNotFound=4, checkPartFileCorrupt=5` |
| `cmd/xl-storage.go` | `CheckParts` (:2406, size-only normal-scan check); `VerifyFile` (:3097, deep-scan HighwayHash bitrot verification) |
| `go.mod` | `go 1.23`; `madmin-go/v3 v3.0.77`; `minio-go/v7 v7.0.80`; `reedsolomon v1.12.4`; `highwayhash v1.0.3` |
| `internal/config/heal/heal.go` | `Bitrot` key (:34); `EnvBitrot = "MINIO_HEAL_BITROTSCAN"` (:39); `Bitrot` config field (:50); `BitrotScanCycle`, `Sleep`, `IOCount`, `DriveWorkers`; `LookupConfig`, `parseBitrotConfig` |

### 16.2 External Dependencies

| Package | Version | Purpose in this analysis |
|---|---|---|
| `github.com/minio/minio` | commit `c07e5b49d477` | MinIO server — built from source for runtime experiments ([§12](#12-six-scenarios-with-runtime-json)) |
| `github.com/minio/madmin-go/v3` | `v3.0.77` | Admin SDK: `HealResultItem`, `HealDriveInfo`, `HealOpts`, `HealScanMode`, `HealItemType`, `DriveState*` constants |
| `github.com/klauspost/reedsolomon` | `v1.12.4` | Reed-Solomon FEC under `Erasure.Heal` |
| `github.com/minio/highwayhash` | `v1.0.3` | HighwayHash-256 bitrot hashing used by `VerifyFile` |
| `github.com/tinylib/msgp` | `v1.2.4` | msgpack codec for `PartialOperation.EncodeMsg`/`DecodeMsg` (MRF persistence) |

### 16.3 Technology Specification Sections Consumed

| Source | Relevance |
|---|---|
| AAP §0.1 — Intent Clarification | Establishes scope: healing decision-making, partial write vs. delete, runtime evidence |
| AAP §0.4.2 — HealResultItem structure | Documented output shape and drive-state mapping |
| AAP §0.7.1 — SWE-AtlasQnA-Repo rule | Read-only source constraint; single-document output |

### 16.4 Runtime Evidence Provenance

Every JSON block in [§12](#12-six-scenarios-with-runtime-json) is a captured emission from `mc admin heal --json` run against a locally-built MinIO binary at commit `c07e5b49d477` with `DataBlocks=2, ParityBlocks=2` (single-set, 4-disk setup on `/tmp/minio-heal-experiments/disk{1..4}`). The capture files were deleted per [§15.2](#152-cleanup-commands-executed); the embedded text is the only persisted copy.

---

## Appendix A — Full Decision-Tree Diagram

```
                    ┌───────────────────────────────────────────┐
                    │  HealObject(bucket, object, versionID,    │
                    │            opts{ScanMode, DryRun, Remove}) │
                    │  cmd/erasure-healing.go:1039              │
                    └─────────────────────┬─────────────────────┘
                                          │
                                          ▼
                    ┌──────────────────────────────────────────┐
                    │  readAllFileInfo on all disks            │
                    │  objectQuorumFromMeta                    │
                    │  (cmd/erasure-metadata.go:531)           │
                    └─────────────────────┬────────────────────┘
                                          │
                         ┌────────────────┴────────────────┐
                         │ readQuorum achieved?            │
                         └────┬──────────────────┬─────────┘
                           No │                  │ Yes
                              ▼                  ▼
                    errErasureReadQuorum       listOnlineDisks
                                              (erasure-healing-
                                                common.go:219)
                                                   │
                          ┌────────────────────────┴───────────────┐
                          │ modTime quorum achieved?               │
                          └────┬───────────────┬─────────────┬─────┘
                             Yes│              No            │ Yes
                                ▼              │             │
                       use modTime        fallback: try      │
                       quorum              commonETag        │
                                            (narrow)         │
                                              │              │
                                              ▼              │
                                  ┌────────────────────────┐ │
                                  │ ETag match found?      │ │
                                  └────┬───────────────┬───┘ │
                                    No │               │ Yes │
                                       ▼               ▼     │
                              every disk outdated  use ETag  │
                                       │               │     │
                                       └───────┬───────┴─────┘
                                               ▼
                                    disksWithAllParts
                                    (erasure-healing-common.go:291)
                                               │
                                               ▼
                                    shouldHealObjectOnDisk per disk
                                    (cmd/erasure-healing.go:156)
                                               │
                                               ▼
                              disksToHealCount = outdated
                                                 + unhealthy
                                                 + abandonedParts
                                               │
                                               ▼
                               ┌─────────────────────────────┐
                               │ disksToHealCount >          │
                               │   latestMeta.Erasure.       │
                               │   ParityBlocks ?            │
                               │ (cmd/erasure-healing.go:428)│
                               └───────┬──────────────┬──────┘
                                    No │              │ Yes
                                       ▼              ▼
                          ┌──────────────┐     ┌────────────────────┐
                          │ RECONSTRUCT  │     │ quorumETag != ""?  │
                          │ Erasure.Heal │     └──────┬─────────┬──┘
                          │ (decode.go:317)        Yes│         │ No
                          │ multiWriter,              ▼         ▼
                          │ writeQuorum=1      reset cannotHeal  │
                          └──────┬───────┘      → RECONSTRUCT    │
                                 ▼                               ▼
                       result.After.Drives[i]     deleteIfDangling
                       .State = ok at :651        (erasure-object.go:482)
                                                          │
                                                          ▼
                                           isObjectDangling(metaArr, errs,
                                                            dataErrsByPart)
                                           (erasure-healing.go:968)
                                                          │
                                        ┌─────────────────┴─────────────────┐
                                        │                                    │
                                        ▼                                    ▼
                              crit #3 non-actionable?          any of #2, #4, #5, #6 ?
                                        │                                    │
                                  Yes → leave degraded                Yes → PURGE from all disks
                                  No  → fall through                  No  → leave degraded
                                                                            (default return false)
```

---

## Appendix B — Scenario Outcome Matrix

| Scenario | Damage | Disks intact | Decision | Terminal state | JSON signature |
|---|---|---|---|---|---|
| A | Removed `disk1/.../heal-test-obj1/` | 3/4 | Reconstruct | All disks `ok` | yellow → green |
| B | Removed `disk1,disk2/.../heal-test-obj2/` | 2/4 (floor) | Reconstruct | All disks `ok` | red → green |
| C | Removed `disk1,disk2,disk3/.../heal-test-obj3/` | 1/4 | Dangling purge (#5) | Object deleted from all | red → red + `detail` |
| D | Overwrote `disk1/.../part.1` with 15 bytes | 3/4 (1 size-corrupt) | Reconstruct | All disks `ok` | yellow → green |
| D-2 (normal scan) | Flipped one byte in `disk1/.../part.1` | 3/4 (silent) | No-op (undetected) | Corrupt bytes remain | green → green (false clean) |
| D-2 (deep scan) | Same | 3/4 (1 bitrot) | Reconstruct | All disks `ok` | yellow → green |
| E | Removed `xl.meta` from disk1-3 | 1/4 (meta) | Dangling purge (#5) | Object deleted + data-dirs orphaned | red → red + `detail` |
| F | Same as B (simulated partial op) | 2/4 | Reconstruct | All disks `ok` | red → green (identical to B) |
| F-2 | Removed disk1 delete-marker xl.meta | 3/4 | Reconstruct meta | All disks have marker | yellow → green |

---

## Appendix C — Reproduction Recipe

Abbreviated — assumes PATH includes `/tmp/minio-bin` and `aws`, `mc` are installed.

```bash
set -euxo pipefail
ROOT=/tmp/minio-heal-experiments
DISKS=("$ROOT/disk1" "$ROOT/disk2" "$ROOT/disk3" "$ROOT/disk4")
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin123
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin123

# Build MinIO (setup phase — may already be present)
# (cd /path/to/minio && CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .)

mkdir -p "${DISKS[@]}"

start_minio() {
    /tmp/minio-bin/minio server "${DISKS[@]}" --address :9010 > /tmp/minio.log 2>&1 &
    sleep 3
    /tmp/minio-bin/mc alias set local http://localhost:9010 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" || true
    aws --endpoint-url http://localhost:9010 s3 mb s3://heal-test 2>/dev/null || true
}
stop_minio() { pkill -f '/tmp/minio-bin/minio' || true; sleep 2; }

# --- Scenario A: 1 disk missing ---
start_minio
dd if=/dev/urandom of=/tmp/obj1 bs=1K count=1024 status=none
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-obj1
stop_minio
rm -rf "$ROOT"/disk1/heal-test/heal-test-obj1
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioA.json

# --- Scenario B: 2 disks missing (floor) ---
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-obj2
stop_minio
rm -rf "$ROOT"/disk{1,2}/heal-test/heal-test-obj2
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioB.json

# --- Scenario C: 3 disks missing (beyond parity → dangling) ---
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-obj3
stop_minio
rm -rf "$ROOT"/disk{1,2,3}/heal-test/heal-test-obj3
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioC.json
aws --endpoint-url http://localhost:9010 s3api head-object --bucket heal-test --key heal-test-obj3 || echo "Purged as expected"

# --- Scenario D: corrupted part.1 (size mismatch) ---
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-corrupt
stop_minio
echo "corrupted-part" > "$(find "$ROOT"/disk1/heal-test/heal-test-corrupt -name 'part.1' | head -1)"
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioD.json

# --- Scenario D-2: silent bitrot (one-byte flip, same size) ---
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-bitrot
stop_minio
PART=$(find "$ROOT"/disk1/heal-test/heal-test-bitrot -name 'part.1' | head -1)
python3 -c "
import sys
with open(sys.argv[1], 'r+b') as f: f.seek(512); b=f.read(1); f.seek(512); f.write(bytes([ord(b)^0x01]))
" "$PART"
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioD2_normal.json
/tmp/minio-bin/mc admin heal --scan deep --json --recursive local/heal-test > /tmp/scenarioD2_deep.json

# --- Scenario E: dangling metadata (xl.meta removed from 3 disks) ---
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-dangling
stop_minio
for d in 1 2 3; do rm -f "$ROOT"/disk$d/heal-test/heal-test-dangling/xl.meta; done
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioE.json

# --- Scenario F-2: versioned delete-marker heal ---
aws --endpoint-url http://localhost:9010 s3api put-bucket-versioning \
    --bucket heal-test --versioning-configuration Status=Enabled
aws --endpoint-url http://localhost:9010 s3 cp /tmp/obj1 s3://heal-test/heal-test-versioned
aws --endpoint-url http://localhost:9010 s3api delete-object --bucket heal-test --key heal-test-versioned
stop_minio
# Remove the delete-marker xl.meta from disk1 only
find "$ROOT"/disk1/heal-test/heal-test-versioned -name 'xl.meta' -delete
start_minio
/tmp/minio-bin/mc admin heal --json --recursive local/heal-test > /tmp/scenarioF2.json

# --- Cleanup ---
stop_minio
rm -rf "$ROOT" /tmp/scenario*.json /tmp/obj1 /tmp/minio.log
/tmp/minio-bin/mc alias remove local 2>/dev/null || true
```

---

## Appendix D — Eight Key Operator Insights

### D.1. `cannotHeal` Is the Pivot Between Reconstruction and Purge

The single decision at `cmd/erasure-healing.go:428` — `disksToHealCount > latestMeta.Erasure.ParityBlocks` — determines whether MinIO attempts to rebuild or whether it hands off to `deleteIfDangling`. When `cannotHeal==true`, the healer *may* delete the object rather than leaving it degraded. Operators must not assume healing is always benign.

**Evidence:** Scenarios C and E (§12.3, §12.6) — 3-of-4 failures trigger the purge path; Scenarios A, B, D, F — within-parity failures trigger reconstruction.

### D.2. "Dangling" Is a Technical Term with Six Trigger Criteria

The standalone function `isObjectDangling` at `cmd/erasure-healing.go:968-1033` has six distinct exit points (see [§5.5](#55-dangling-criteria-and-deleteifdangling), [§13.3](#133-the-six-dangling-criteria), [Appendix A](#appendix-a--full-decision-tree-diagram)). A single-disk loss in EC(2,2) is **not** dangling; a three-of-four loss **is** (criterion #5). A one-of-four loss with non-actionable errors is **not** dangling (criterion #3 override).

### D.3. Non-Actionable Errors Are a Safety Valve

If any disk reports a non-actionable error (permission denied, generic I/O error, unclassified ENOENT-like code), the healer refuses to purge at `cmd/erasure-healing.go:1008-1010`. MinIO errs on the side of data preservation — it would rather leave an unreadable object in place than delete something that might still be recoverable once the disk problem is resolved.

### D.4. Bitrot Detection Requires Deep Scan

- **Normal scan** (`CheckParts` at `cmd/xl-storage.go:2406`) verifies only file size.
- **Deep scan** (`VerifyFile` at `cmd/xl-storage.go:3097`) reads the entire file and verifies the HighwayHash-256 signature.

The default is normal scan. Silent bitrot — same size, different content — is **invisible** to normal scan. Auto-escalation at `cmd/erasure-healing.go:1080-1085` only triggers when some other detection raises `errFileCorrupt`. To defend against silent bitrot, operators must periodically run `mc admin heal --scan deep ...` or enable the scheduled background bitrot scanner via **`MINIO_HEAL_BITROTSCAN`** (env var at `internal/config/heal/heal.go:39`).

**Evidence:** Scenario D-2 (§12.5) — normal scan reports green on a bit-flipped object; deep scan detects and heals.

### D.5. The Same Queue Handles Write Failures and Delete Failures

`globalMRFState.opCh` at `cmd/mrf.go:72` is a unified channel. `PartialOperation` has no `OperationType` field. The only "hint" about the original intent is `validMeta.Deleted` on the surviving xl.meta, which `isObjectDangling` uses (criterion #4 at `cmd/erasure-healing.go:1012-1017`) to choose between meta-republish convergence (delete marker) and data-rebuild convergence (live object).

### D.6. `After.Drives` Reflects Terminal State, Not Intermediate Steps

`Before.Drives` and `After.Drives` are appended side-by-side at `cmd/erasure-healing.go:395-404` — they start identical. Only successful per-disk reconstruction mutates `After.Drives[i].State` to `ok` at `cmd/erasure-healing.go:651`. A disk that was `offline` before the heal and remains unreachable retains `State: offline` in `After`. This means `After` gives terminal outcome, not heal progress.

### D.7. Audit Logs Name the Exact Caller of Dangling Deletion

`deleteIfDangling` at `cmd/erasure-object.go:482` captures the caller via `runtime.Caller(1)` at line 526 and registers a deferred `auditDanglingObjectDeletion` at line 531. Every dangling purge emits a structured audit record naming the exact Go `file:line` that requested it. Operators with `audit_webhook` enabled can correlate purges back to the precise heal code path (e.g., `cmd/erasure-healing.go:438` for the `cannotHeal` branch vs. read-time purges at `cmd/erasure-object.go:106`).

### D.8. The Background Healer Defaults to `Remove:true`

`cmd/global-heal.go` constructs `newBgHealSequence` with `Remove: healDeleteDangling`, where `healDeleteDangling = true` at `cmd/data-scanner.go:60`. This means background scans **will** permanently delete dangling objects. To dry-run a heal, operators must use `mc admin heal --dry-run` or explicitly pass `opts.Remove=false` via the admin API — the default behavior purges.

---

*End of document.*
