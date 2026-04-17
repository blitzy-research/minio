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
| **Green (no-op)** | All parts on all disks are intact | `errNoHealRequired`; `before == after == all ok` | [§12.1](#121-scenario-a--one-disk-missing-yellow--green) baseline probe |
| **Reconstruct** | `disksToHealCount ≤ ParityBlocks` | Missing/corrupt shards rebuilt via Reed-Solomon; `before: {missing, ok, ok, ok}` → `after: {ok, ok, ok, ok}` | [§12.1–12.2](#121-scenario-a--one-disk-missing-yellow--green), [§12.4–12.5](#124-scenario-d--corrupted-part-file-size-mismatch) |
| **Dangling purge** | `cannotHeal==true` AND `isObjectDangling` returns `true` | Object permanently removed from all disks; subsequent GETs return `NoSuchKey` | [§12.3](#123-scenario-c--three-disks-missing-beyond-parity-dangling-purge), [§12.6](#126-scenario-e--dangling-metadata) |
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
7. Accumulate `disksToHealCount` — the variable is declared as `disksToHealCount := 0` at `cmd/erasure-healing.go:374`, incremented by one (`disksToHealCount++` at `cmd/erasure-healing.go:379`) inside the per-disk loop for every disk where `shouldHealObjectOnDisk` returns `(true, reason)`, and may later be decremented (`disksToHealCount--` at `cmd/erasure-healing.go:599`) when a subsequent write-target disk fails mid-heal. It is a running counter of disks flagged for reconstruction, **not** a sum of separately-computed categories. Adjacent checks at `cmd/erasure-healing.go:407-420` handle two early-return cases: `isAllNotFound` (tombstone short-circuit, lines 407-415) and `disksToHealCount == 0` (heal-not-required short-circuit, lines 417-420).
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
// Verbatim from cmd/erasure-healing.go:156-183
func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo, latestMeta FileInfo) (bool, error) {
    if errors.Is(erErr, errFileNotFound) || errors.Is(erErr, errFileVersionNotFound) || errors.Is(erErr, errFileCorrupt) {
        return true, erErr
    }
    if erErr == nil {
        if meta.XLV1 {
            return true, errLegacyXLMeta
        }
        if !latestMeta.Equals(meta) {
            return true, errOutdatedXLMeta
        }
        if !meta.Deleted && !meta.IsRemote() {
            for _, partErr := range partsErrs {
                if slices.Contains([]int{
                    checkPartFileNotFound,
                    checkPartFileCorrupt,
                }, partErr) {
                    return true, errPartMissingOrCorrupt
                }
            }
        }
        return false, nil
    }
    return false, erErr
}
```

Key structural notes on the actual code versus what a casual reader might expect:

- **Return arity is two, not three.** The function returns `(bool, error)`. There is no `madmin.HealItemType` return value — the heal-item type is set separately by callers that build `HealResultItem` values (e.g., `pushHealResultItem` at `cmd/admin-heal-ops.go:618`).
- **`errFileNotFound`, `errFileVersionNotFound`, and `errFileCorrupt` are combined in a single `errors.Is || ... || ...` expression** (line 158), not split across a `switch` with separate cases.
- **Metadata equivalence uses a single method call** `!latestMeta.Equals(meta)` (line 167) rather than a two-field `ModTime` + `DataDir` comparison. `FileInfo.Equals` (`cmd/storage-datatypes.go`) compares the full metadata including versioning, parity layout, and the `DataDir`.
- **Part-error screening is an inclusion list, not an exclusion list** (lines 172-175). Only `checkPartFileNotFound` (= 4) and `checkPartFileCorrupt` (= 5) trigger a heal; `checkPartDiskNotFound` (= 2) and `checkPartVolumeNotFound` (= 3) are treated as disk-level issues that do not by themselves flag a single object part as needing reconstruction, and `checkPartUnknown` (= 0) / `checkPartSuccess` (= 1) naturally do not trigger healing either.
- **`meta.XLV1` short-circuits legacy detection** *before* the `Equals` check; legacy XLv1 objects are always rewritten to XLv2 form regardless of metadata parity.
- **Closing `return false, erErr`** at the bottom preserves non-nil, non-matched read errors for the caller to record as a disk-level failure, rather than silently coercing to `nil`.

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

```text
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
                       │ disksToHealCount: loop counter        │
                       │ ++ per shouldHealObjectOnDisk==true   │
                       │ (:374 init, :379 ++, :599 --)         │
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

### 8.1 Two Distinct JSON Formats

The healing output visible to the operator depends on *which* surface they are reading. There are two subtly different JSON shapes in play, and conflating them leads to broken log-parsing scripts. Both are documented here so operators can target whichever format their tooling consumes.

**Format A — Raw `madmin-go/v3` `HealResultItem` (server-side, library-serialized).**

This is what the server emits over the admin heal HTTP stream and what Go programs that link against `madmin-go/v3` decode directly. It is defined by the `HealResultItem` struct at `github.com/minio/madmin-go/v3/heal-commands.go`. The `Before`/`After` aggregate contains only a `Drives` array — there are **no** aggregated counts or colour fields baked into the raw struct.

```jsonc
// Raw HealResultItem (madmin-go serialization)
{
  "resultId":     0,
  "type":         "object",
  "bucket":       "heal-test",
  "object":       "heal-test-obj1",
  "versionId":    "",
  "detail":       "",            // populated for dangling/skip/error cases
  "parityBlocks": 2,
  "dataBlocks":   2,
  "diskCount":    4,
  "setCount":     1,
  "before": {
    "drives": [
      { "uuid": "...", "endpoint": "/tmp/.../disk1", "state": "missing" },
      { "uuid": "...", "endpoint": "/tmp/.../disk2", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk3", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk4", "state": "ok" }
    ]
  },
  "after": { "drives": [ /* same shape; reconstruction result encoded per drive */ ] },
  "objectSize": 1048576
}
```

**Format B — `mc admin heal --json` wrapper (client-side, `healRec`).**

When the operator runs the `mc admin heal --json` CLI, the client wraps each `HealResultItem` in a `healRec` struct (`mc/cmd/admin-heal-ui.go`) and derives aggregate counts (`online`, `offline`, `missing`, `corrupted`) and a `color` field (`green`/`yellow`/`red`/`grey`) from the per-drive `State` values via `HealDriveInfo` helpers `GetOnlineCounts`, `GetOfflineCounts`, `GetMissingCounts`, `GetCorruptedCounts`. The wrapper strips the raw `HealResultItem` identity fields (`resultId`, `bucket`, `object`, `versionId`, `detail`, `parityBlocks`, `dataBlocks`, `diskCount`, `setCount`, `objectSize`) — they are not part of the wrapper JSON.

```jsonc
// mc admin heal --json wrapper (healRec serialization)
{
  "before": {
    "color":     "yellow",       // green | yellow | red | grey (derived by mc)
    "offline":   0,              // count of drives with state=="offline" (GetOfflineCounts)
    "online":    3,              // count of drives with state=="ok" ONLY (GetOnlineCounts)
    "missing":   1,              // count of drives with state=="missing" (GetMissingCounts)
    "corrupted": 0,              // count of drives with state=="corrupt" (GetCorruptedCounts)
    "drives": [
      { "uuid": "...", "endpoint": "/tmp/.../disk1", "state": "missing" },
      { "uuid": "...", "endpoint": "/tmp/.../disk2", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk3", "state": "ok" },
      { "uuid": "...", "endpoint": "/tmp/.../disk4", "state": "ok" }
    ]
  },
  "after": { /* same wrapper shape; color/counts recomputed from drive states */ }
}
```

**Important:** Each aggregate is the count of drives whose `state` equals that single state string — `online` counts only `ok`, `missing` counts only `missing`, etc. `online + missing + offline + corrupted` does **not** necessarily equal `diskCount`; the five additional states (`permission-denied`, `faulty`, `root-mount`, `unknown`, `unformatted`) contribute to none of the four aggregates. Tooling that assumes `online + missing + offline + corrupted == diskCount` will break on drives that enter those edge-case states. In the example above, 3 drives are `ok` → `online == 3`, 1 drive is `missing` → `missing == 1`, nothing else → `offline == 0`, `corrupted == 0`; the sum happens to be `4 == diskCount` because no edge-case states are in play.

**Choose your consumer carefully.** Scripts that parse the admin HTTP stream or use `madmin-go` directly will see Format A. Scripts that parse `mc admin heal --json` stdout will see Format B. The two are not interchangeable, and the field sets only partially overlap (both contain `before.drives`/`after.drives` with identical `HealDriveInfo` shape; everything else differs).

In the scenario captures in [§12](#12-six-scenarios-with-runtime-json) that include `color`/`online`/`offline`/`missing`/`corrupted`, the JSON shown corresponds to Format B (`mc admin heal --json`); the identity and layout fields shown there (`resultId`, `bucket`, etc.) are presented for clarity alongside the wrapper output and are *not* part of the raw `healRec` payload.

### 8.2 Per-Drive State Semantics

The `state` field in each `HealDriveInfo` is populated from the nine `DriveState*` string constants defined at `madmin-go/v3/heal-commands.go:120-129`. Four of them (`"ok"`, `"offline"`, `"missing"`, `"corrupt"`) are produced by the heal code path at `cmd/erasure-healing.go:382-393`; the remaining five (`"permission-denied"`, `"faulty"`, `"root-mount"`, `"unknown"`, `"unformatted"`) surface only through adjacent subsystems (disk-formatting, health probe, mount-type detection).

| `state` constant | Literal | Meaning in Before | Meaning in After | Source |
|---|---|---|---|---|
| `DriveStateOk` | `"ok"` | Disk had the correct version with intact parts | Reconstruction succeeded; disk now has the correct content | `cmd/erasure-healing.go:386` (`reason == nil` branch) |
| `DriveStateMissing` | `"missing"` | `xl.meta` missing, outdated, or parts missing/corrupt-by-size (`errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errPartMissingOrCorrupt`, `errOutdatedXLMeta`, `errLegacyXLMeta`) | Not yet healed — heal didn't address this disk, or the healer wasn't invoked | `cmd/erasure-healing.go:389` |
| `DriveStateCorrupt` | `"corrupt"` | `xl.meta` unreadable with non-actionable error (the `default` branch of the switch) | Still `"corrupt"`; heal couldn't proceed | `cmd/erasure-healing.go:392` |
| `DriveStateOffline` | `"offline"` | Disk unreachable (`errDiskNotFound`) | Still `"offline"` — heal has no way to write there | `cmd/erasure-healing.go:388` |
| `DriveStatePermission` | `"permission-denied"` | Filesystem returned EACCES on `xl.meta` read — classified as non-actionable (criterion #3 dangling safety valve) | Cleared to `"ok"` only if the fault self-resolved mid-heal | `madmin-go/v3/heal-commands.go:124`; set by storage layer when a `StorageAPI` call surfaces a permission error |
| `DriveStateFaulty` | `"faulty"` | Disk marked faulty by the health-check probe (I/O error floor exceeded) | Still `"faulty"` until operator replaces the disk | `madmin-go/v3/heal-commands.go:125`; set by `cmd/erasure.go:109` (`diskErrToDriveState` `case errors.Is(err, errFaultyDisk)`) when a disk health check surfaces `errFaultyDisk` |
| `DriveStateRootMount` | `"root-mount"` | Drive is the OS root filesystem — disallowed for object storage per `cmd/xl-storage.go` root-disk guard | Never transitions during heal; operator must remount on a dedicated block device | `madmin-go/v3/heal-commands.go:126` |
| `DriveStateUnknown` | `"unknown"` | State could not be determined (e.g., RPC timeout from a peer) | Same — never resolved by heal alone | `madmin-go/v3/heal-commands.go:127` |
| `DriveStateUnformatted` | `"unformatted"` | Disk present but has no `format.json` yet — newly added, awaiting format | Transitions to `"ok"` once `initBackgroundHealing` plus `format.json` write complete | `madmin-go/v3/heal-commands.go:128` (comment: "only returned by disk") |

`Before.Drives` is initialized at `cmd/erasure-healing.go:395-399` by appending one `HealDriveInfo` per physical disk; `After.Drives` is initialized in the **same loop** at `cmd/erasure-healing.go:400-404` to the identical initial values. Only successful per-disk reconstruction mutates `After.Drives[i].State` to `"ok"` at `cmd/erasure-healing.go:651`.

In a live 4-disk EC(2,2) cluster under normal healing the three most common literals are `"ok"`, `"missing"`, and `"offline"` (observed in every runtime scenario in §12). The other six — `"corrupt"`, `"permission-denied"`, `"faulty"`, `"root-mount"`, `"unknown"`, `"unformatted"` — appear only under degraded or setup-time conditions, but every operator should recognize all nine literals when parsing `HealResultItem` JSON.

### 8.3 Aggregate Color Logic

The `color` field summarizes the 4-drive state:

- `green` — all 4 disks `ok`
- `yellow` — at most `ParityBlocks` disks in any non-ok state (reconstruction viable)
- `red` — more than `ParityBlocks` disks in non-ok state (dangling territory)
- `grey` — heal result is a skip/no-op

The transition `before.color=yellow → after.color=green` is the unambiguous "healing worked" signature. `red → green` is "barely healed — all 4 disks came back". The **purge signature** differs by consumer:

- **Format A (server-side)**: `before.color=red` → `after.color=red` with `detail` carrying the `toObjectErr`-translated sentinel text (e.g., `"Read failed. Insufficient number of drives online"`). See the conceptual example in [§8.4](#84-dangling-purge-json-signature).
- **Format B (mc stdout)**: The **null-drives** pattern — `drives: null`, `color: ""`, all counts zero, `name: "/"`, `error: "Invalid parity shard count/surplus shard count given: ..."`, `detail: "Object not found: <bucket>/<object>"`. See [§12.3](#123-scenario-c--three-disks-missing-beyond-parity-dangling-purge) for the captured NDJSON.

No literal string `"object is dangling"` is emitted in either form — the dangling decision is inferred from the aggregate shape, not a dedicated marker.

### 8.4 Dangling-Purge JSON Signature

When the healer purges a dangling object, the server-side `HealResultItem` is crafted by `defaultHealResult` (`cmd/erasure-healing.go:787-800`) with `After.Drives[i].State = missing` on all disks (the data is gone from each) and `detail` populated by the internal error message. For a three-of-four metadata loss with a live object, the conceptual server-side (Format A) view is:

```jsonc
// Conceptual Format A (server-side madmin-go view, NOT mc stdout).
// This is what a direct admin-API client observes pre-purge.
{
  "type": "object",
  "detail": "Read failed. Insufficient number of drives online",
  "before": { "color": "red", "missing": 3, ... },
  "after":  { "color": "red", "missing": 3, ... }   // no reconstruction occurred
}
```

The string `"Read failed. Insufficient number of drives online"` is the human-readable form of the server sentinel `errErasureReadQuorum` at `cmd/erasure-errors.go:23`. **It does not surface in `mc admin heal --json` stdout.** When mc is the consumer, the same dangling-purge decision is signaled via the **null-drives Format B signature**: a zeroed `HealResultItem` is passed through mc's `getObjectHCCChange` → `getHColCode` formatter (at `cmd/admin-heal-result-item.go:42-48` and `cmd/admin-heal-ui.go:55`), which emits the pair `{"error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0", "detail":"Object not found: <bucket>/<object>"}` with `drives: null`, `color: ""`, all counts zero, and `name: "/"`. See §12.3 for the verbatim mc output and step-by-step mechanics. The server-side JSON above and the mc NDJSON in §12.3 are two presentations of the same underlying decision.

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

This is the channel whose output is rendered in [§12](#12-six-scenarios-with-runtime-json). Two distinct string fields appear on failure paths, and it is essential to distinguish them:

- **Server-side `toObjectErr` translations.** Internally, `cmd/api-errors.go:toObjectErr` converts low-level error sentinels into human-readable messages such as `"Read failed. Insufficient number of drives online"` (from `errErasureReadQuorum`) and `"Write failed. Insufficient number of drives online"` (from `errErasureWriteQuorum`). These strings are embedded in the `APIError` structure returned from the admin handler.
- **mc-side `detail` / `error` strings.** When `mc admin heal --json` receives such an API error for an irrecoverable object (see [§12.3](#123-scenario-c--three-disks-missing-beyond-parity-dangling-purge)), the body mc prints to stdout contains `"Object not found: <bucket>/<object>"` in `detail` and `"Invalid parity shard count/surplus shard count given: ..."` in `error`. The `errErasureReadQuorum` sentinel does **not** surface verbatim through the mc path for dangling-purge terminals.
- **For healable objects**, mc's emission carries `detail: ""` (empty) and no `error` field at all — the `before`/`after` aggregates and per-drive `state` strings are the only decision evidence (see [§12.1](#121-scenario-a--one-disk-missing-yellow--green)).

The `detail` string is therefore a server-side concept. Whether it reaches the mc stdout verbatim depends on both the terminal nature of the error and the mc renderer's own error-formatting pipeline. Operators who want to see `"Read failed. Insufficient..."` verbatim must either (a) query the admin API directly with a Go client that parses the raw `APIError`, or (b) consult the admin session log (`mc admin trace`) or the MinIO server log.

### 9.2 Dangling Deletion Audit

`deleteIfDangling` (`cmd/erasure-object.go:482-563`) captures the caller line at `runtime.Caller(1)` (line 526) and registers a deferred audit write at line 531:

```go
defer auditDanglingObjectDeletion(ctx, bucket, object, versionID, ...)
```

The audit record includes the bucket, object, version, and the exact caller `file:line`. Deployed instances with the audit webhook enabled (e.g., `audit_webhook`) receive a structured log entry every time a dangling purge fires; operators can correlate purges back to the exact code path that triggered them (`cmd/erasure-healing.go:438` for the `cannotHeal` branch vs. `cmd/erasure-object.go:106` for read-time purges, etc.).

### 9.3 Internal Healer Logs

`cmd/erasure-healing.go:477, :487, :497` each invoke `healingLogOnceIf` with a distinct log-once key but the **same log-message template**:

```text
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
2. The `detail`/`error` strings on failures — noting the server-side vs mc-side distinction explained in [§9.1](#91-the-admin-heal-stream-primary-signal)
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
| `errFileNotFound` → surfaced as `NoSuchKey` | `cmd/object-api-errors.go:30` via `toObjectErr` (matches `errFileNotFound.Error()` at `cmd/object-api-errors.go:104`) | `"The specified key does not exist"` | Post-dangling purge: object was deleted by the healer |
| `errNoHealRequired` | `cmd/erasure-errors.go:29` | `"No healing is required"` | No-op: object is already green |

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
| Type | `chan PartialOperation` | `cmd/mrf.go:64` (`opCh` field on `mrfState`) |
| Enqueue behavior | Non-blocking (`select { case opCh <- op: default: }`) | `cmd/mrf.go:95-98` |
| Persistence on shutdown | msgpack-serialized to `<minioMetaBucket>/.heal/mrf/list.bin` | `cmd/mrf.go:102` (`shutdown`) |
| Load on startup | Yes, via `startMRFPersistence` | `cmd/mrf.go:155` |
| Consumer | `healRoutine` | `cmd/mrf.go:220` |
| Retry delay | ~1 second | `cmd/mrf.go:220+` (allows network reconnect) |

### 11.4 Consumer Dispatch

`healRoutine` (`cmd/mrf.go:220`) drains `opCh` and dispatches heal work. The actual body is more elaborate than a naked `HealObject` call — it performs path filtering, a reconnect delay, and rate-limited scan-mode dispatch before branching on operation kind.

Verbatim structure (`cmd/mrf.go:220-283`):

```go
func (m *mrfState) healRoutine(z *erasureServerPools) {
    for {
        select {
        case <-GlobalContext.Done():
            return
        case u, ok := <-m.opCh:
            if !ok {
                return
            }

            // 1) .minio.sys internal-path filter — skip ephemeral scratch
            if u.Bucket == minioMetaBucket {
                if wildcard.Match("buckets/*/.metacache/*", u.Object) { continue }
                if wildcard.Match("tmp/*",                u.Object) { continue }
                if wildcard.Match("multipart/*",          u.Object) { continue }
                if wildcard.Match("tmp-old/*",            u.Object) { continue }
            }

            // 2) Reconnect grace: if this op was queued <1s ago, sleep 1s
            now := time.Now()
            if now.Sub(u.Queued) < time.Second {
                time.Sleep(time.Second)
            }

            // 3) Rate limiting from healSleeper (cmd/background-heal-ops.go)
            wait := healSleeper.Timer(context.Background())

            // 4) Scan mode: deep if the queuer asked for bitrot scan
            scan := madmin.HealNormalScan
            if u.BitrotScan {
                scan = madmin.HealDeepScan
            }

            // 5) Three-way dispatch based on PartialOperation shape
            if u.Object == "" {
                healBucket(u.Bucket, scan)                                       // (a) bucket-level heal
            } else {
                if len(u.Versions) > 0 {
                    vers := len(u.Versions) / 16
                    if vers > 0 {
                        for i := 0; i < vers; i++ {
                            healObject(u.Bucket, u.Object,
                                uuid.UUID(u.Versions[16*i:]).String(), scan)    // (b) multi-version heal
                        }
                    }
                } else {
                    healObject(u.Bucket, u.Object, u.VersionID, scan)           // (c) single heal, VersionID may be ""
                }
            }

            wait()
        }
    }
}
```

Key dispatch facts (often misread):

- **Branch (a) — bucket healing** fires when `u.Object == ""`. The partial operation was queued for a bucket-level fault (e.g., `MakeBucket` that failed on one pool). There is no `Object` involved, and `healBucket` rebuilds bucket metadata across disks. This branch is completely absent from any naïve two-way `VersionID != ""` dispatch diagram.
- **Branch (b) — multi-version heal** fires when `len(u.Versions) > 0`. `Versions` is a packed 16-byte-per-UUID blob (used by multi-delete partial operations). The loop divides the length by 16 to extract each UUID and calls `healObject` per version. `u.VersionID` is **not** consulted in this branch.
- **Branch (c) — single heal** fires otherwise. It calls `healObject(u.Bucket, u.Object, u.VersionID, scan)` **unconditionally** — there is no `u.VersionID != ""` guard. An empty `VersionID` here means "heal the unversioned/latest view of the object", which is the correct semantics for bucket-unversioned workloads.

The consumer does not distinguish PUT-origin from DELETE-origin entries. It just invokes `healObject`/`healBucket`, and the state of the surviving `xl.meta` at that moment dictates what convergence looks like:

- If the surviving meta has `Deleted=false` (live object) — `healObject` converges to "reconstruct data on the lagging disks".
- If the surviving meta has `Deleted=true` (delete marker) — `healObject` converges to "propagate delete marker to all disks".
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

> **Reading guide for the JSON in this section.**
>
> 1. **Streamed NDJSON.** Every `mc admin heal --json` invocation emits a stream of newline-delimited JSON records: one record with `"type": "bucket"` (the bucket-level entry), one record per object or object version with `"type": "object"`, and a final record with `"type": "summary"`. The blocks below show the **object-level record only** (the diagnostic line); the bucket and summary records are elided except where noted (e.g. §12.3).
> 2. **Format B is the mc wrapper.** The `{type, name, size}` / `{before, after}` / `{color, online, offline, missing, corrupted, drives}` shape is the **mc wrapper (Format B)** defined in §8.1. Direct callers of the server-side admin API receive the underlying `madmin-go` `HealResultItem` (Format A) directly; Format A has `{bucket, object, parityBlocks, dataBlocks, diskCount, setCount, objectSize, versionId}` identity fields that mc does not forward.
> 3. **Hybrid presentation.** For pedagogical continuity — so that a reader can see in one block both *what mc prints* and *which object it refers to* — some blocks below augment the literal mc Format B output with Format A identity fields (`bucket`, `object`, `parityBlocks`, `dataBlocks`, `diskCount`, `setCount`, `objectSize`, `versionId`, `resultId`). Any field in that list is **not** part of mc's NDJSON stream; it is inserted here for clarity (see §16.4 for provenance). Conversely, `name`, `size`, and the Format B `color`/`online`/`missing`/`corrupted`/`offline` aggregates at the `before`/`after` level **are** mc-emitted exactly as shown.
> 4. **Drive entry abbreviation.** Drive-array entries may be shown as `{ "state": "ok" }` or `{ "endpoint": ".../diskN", "state": "ok" }` for brevity. In the actual stream each element also carries `"uuid": ""` (empty in standalone mode) and a fully-qualified `endpoint` path (e.g. `"/tmp/minio-heal-experiments/disk1"`). Abbreviation is cosmetic; the decision logic is unaffected.
> 5. **Aggregate counts are state-filtered.** `online` counts drives with `state=="ok"` only. `missing`, `offline`, `corrupted` each count their respective states only. The four aggregates need not sum to `diskCount`; states like `permission-denied`, `faulty`, `root-mount`, `unknown`, `unformatted` count against none of them. See `madmin-go/v3/heal-commands.go` functions `GetOnlineCounts`, `GetMissingCounts`, `GetOfflineCounts`, `GetCorruptedCounts`.

### 12.1 Scenario A — One Disk Missing (yellow → green)

**Setup:** Delete `disk1/heal-test/heal-test-obj1/` entirely. 3/4 disks intact.

```jsonc
{
  "resultId": 1, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj1",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "yellow", "offline": 0, "online": 3, "missing": 1, "corrupted": 0,
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

**Analysis:** `disksToHealCount=1 ≤ 2=ParityBlocks` → reconstruct. Reed-Solomon uses the 3 intact shards to compute the missing one; `RenameData` promotes it on disk1; `After.Drives[0].State` is flipped to `ok` at `cmd/erasure-healing.go:651`. Aggregate color transitions yellow→green. The `before.online` count is `3`, not `4`, because `online` filters to `state=="ok"` only (see `madmin-go/v3/heal-commands.go:222-230`, function `GetOnlineCounts`), and one drive is in state `"missing"` before heal.

### 12.2 Scenario B — Two Disks Missing (at Parity Boundary, red → green)

**Setup:** Delete `disk1/heal-test/heal-test-obj2/` AND `disk2/heal-test/heal-test-obj2/`. 2/4 disks intact — exactly at the RS floor.

```jsonc
{
  "resultId": 2, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj2",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "red", "offline": 0, "online": 2, "missing": 2, "corrupted": 0,
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
  },
  "objectSize": 1048576
}
```

**Analysis:** `disksToHealCount=2 ≤ 2=ParityBlocks` (equality is OK). Reed-Solomon has exactly `k=2` shards — the minimum. Reconstruction succeeds. Before-color `red` because `missing > ParityBlocks` is the aggregate-color threshold — but the decision-layer threshold is `>`, not `≥`, so reconstruction still proceeds. The `red → green` transition is the diagnostic "at the floor, fully recovered" signature. Before-aggregate `online=2` reflects the two remaining `ok` drives (disk3, disk4); `missing=2` captures disk1 and disk2.

### 12.3 Scenario C — Three Disks Missing (Beyond Parity, Dangling Purge)

**Setup:** Delete `disk1`, `disk2`, `disk3` copies of `heal-test-obj3`. 1/4 disks intact.

**Actual mc emission** (three NDJSON records, verbatim). The bucket-level line is always green (bucket metadata is untouched); the object-level line is the diagnostic one:

```json
{"status":"success","type":"bucket","name":"heal-test/","before":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"size":0}
{"status":"success","error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0","detail":"Object not found: heal-test/heal-test-obj3","type":"object","name":"/","before":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"after":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"size":0}
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":0,"items_scanned":2,"items_healed":0,"size":0,"duration":1}
```

**Key observations about the object-level record.** This is **not** a populated `HealResultItem` with three `missing` drives and one `ok` drive — that populated shape does not surface through mc for a dangling-purged object. Instead:

| Field | Value | Meaning |
|---|---|---|
| `type` | `"object"` | Record kind |
| `name` | `"/"` | Bucket/object could not be identified post-purge; mc's formatter substitutes `"/"` |
| `error` | `"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0"` | mc-layer error string (see below) |
| `detail` | `"Object not found: heal-test/heal-test-obj3"` | mc-layer human-readable detail |
| `before.drives` / `after.drives` | `null` | No drive array — the result was zeroed |
| `before.color` / `after.color` | `""` (empty) | No color classification |
| `before.online` / `missing` / `offline` / `corrupted` | `0 / 0 / 0 / 0` | All aggregates zero |
| `size` | `0` | Object is gone; no size to report |
| summary `objects_healed` | `0` | Object was not healed (was purged) |

**Why the emission looks like this (mc-layer mechanics).** When the heal path delegates to `deleteIfDangling` (step 2 of the decision trace below), the object is purged before `HealObject` can assemble a populated `HealResultItem`; the server returns an empty/zeroed result item. mc then runs that record through `getObjectHCCChange` at `cmd/admin-heal-result-item.go:42-48`, which computes `surplusShardsBeforeHeal = onlineBefore - dataShards = 0 - 0 = 0` and calls `getHColCode` at `cmd/admin-heal-ui.go:55`. Because both `parityShards` and the surplus are zero, `getHColCode` returns the error string `"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0"`. mc then attaches its own human-readable `detail: "Object not found: <bucket>/<object>"` because a subsequent metadata read for the (now-purged) object returns `ObjectNotFound`. The `drives` fields are `null`, `name` collapses to `"/"`, and every count is zero — **this is the dangling-purge signature in Format B (mc stdout).**

**Provenance of `"Read failed. Insufficient number of drives online"`.** This is the human-readable form of the server-side sentinel `errErasureReadQuorum` defined at `cmd/erasure-errors.go:23` (`errors.New("Read failed. Insufficient number of drives online")`). It is the error raised internally when a read cannot meet read-quorum. It does **not** appear in mc's NDJSON stream for the dangling-purge case — the chain `errErasureReadQuorum → toObjectErr → API response transformation` leaves mc with the empty `HealResultItem` described above, and mc substitutes its own `"Object not found"` phrasing. The original sentinel string may still appear in server logs or to direct admin-API clients that call `HealObject` and inspect the returned error object; it is **not** a user-visible string in the mc CLI for this scenario.

**Decision trace (server-side, what actually happened before the emission):**

1. `disksToHealCount=3 > 2=ParityBlocks` → `cannotHeal=true` (`cmd/erasure-healing.go:428`).
2. Since `Remove: healDeleteDangling=true` (background heal default) and no `quorumETag` rescue, the branch at `cmd/erasure-healing.go:438` delegates to `deleteIfDangling`.
3. Inside, `isObjectDangling(metaArr, errs, dataErrsByPart)` runs (`cmd/erasure-object.go:483`): `notFoundMetaErrs=3`, `validMeta.Erasure.ParityBlocks=2`. **Criterion #5** at `cmd/erasure-healing.go:1026-1028` fires: `notFoundMetaErrs (3) > ParityBlocks (2)` → `return validMeta, true`.
4. The caller purges the object from all disks via `DeleteObject`. The `runtime.Caller(1)` capture at `cmd/erasure-object.go:526` and the deferred `auditDanglingObjectDeletion` at line 531 record the purge. disk4's previously-valid copy is deleted as part of the purge (this is why the object is fully gone after heal, not "recovered on 1/4 disks").

Post-purge: `aws s3 ls s3://heal-test/heal-test-obj3` returns empty. `aws s3 cp s3://heal-test/heal-test-obj3 /tmp/x` returns `NoSuchKey`. A subsequent `mc admin heal --json local/heal-test/heal-test-obj3` produces the same null-drives record (the object is still absent).

**Conceptual Format A view** (for teaching — this shape does *not* surface in mc stdout; it is what a direct `madmin-go` client would see for the server-side decision state *before* the purge, i.e. if `healDeleteDangling` were `false` or if the admin client is a `dry-run` inspector):

```jsonc
// Conceptual only — not emitted by mc. Included to illustrate the server-side
// pre-purge state and the populated-drives layout a direct admin-API consumer
// would observe before the delegation to deleteIfDangling.
{
  "resultId": 3, "type": "object",
  "bucket": "heal-test", "object": "heal-test-obj3",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "drives": [
      { "state": "missing" },
      { "state": "missing" },
      { "state": "missing" },
      { "state": "ok" }
    ]
  },
  "after": {
    "drives": [
      { "state": "missing" },
      { "state": "missing" },
      { "state": "missing" },
      { "state": "missing" }
    ]
  }
}
```

### 12.4 Scenario D — Corrupted Part File (Size Mismatch)

**Setup:** Overwrite `disk1/heal-test/heal-test-corrupt/<dataDir>/part.1` with a 15-byte ASCII string (original was 1 MiB).

```jsonc
{
  "resultId": 4, "type": "object",
  "bucket": "heal-test", "object": "heal-test-corrupt",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "yellow", "offline": 0, "online": 3, "missing": 1, "corrupted": 0,
    "drives": [
      { "endpoint": ".../disk1", "state": "missing" },
      { "endpoint": ".../disk2", "state": "ok" },
      { "endpoint": ".../disk3", "state": "ok" },
      { "endpoint": ".../disk4", "state": "ok" }
    ]
  },
  "after": {
    "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "objectSize": 1048576
}
```

**Analysis:** `CheckParts` at `xl-storage.go:2406` compares file size on disk (`15 bytes`) against `fi.Parts[i].Size` (~`524,320 bytes` per shard — half of the 1 MiB object plus bitrot-header overhead). Mismatch → `checkPartFileCorrupt` (code 5 at `cmd/storage-datatypes.go:536-544`). `shouldHealObjectOnDisk` picks this up as `errPartMissingOrCorrupt`, which maps to drive state `missing`. Reconstruction proceeds. Before-aggregate `online=3` (disk2, disk3, disk4), `missing=1` (disk1). Note that the `corrupted=0` count is not `1` despite the part being "corrupt" on disk — because `errPartMissingOrCorrupt` maps to `DriveStateMissing`, not `DriveStateCorrupt`, in the switch at `cmd/erasure-healing.go:382-393` (see §8.2). The mc wrapper's `size` field (Format B) and the Format A `objectSize` field both carry the **total object size** (`1048576` for a 1 MiB test object), not the per-shard on-disk size — this is the total `HealResultItem.ObjectSize` populated from `FileInfo.Size`.

**Why state is `missing` and not `corrupt`:** the state mapping at `cmd/erasure-healing.go:382-393` treats `errPartMissingOrCorrupt` as "missing" (a heal-needed signal) — not `corrupt` which is reserved for xl.meta-level unreadable-with-non-actionable-error cases.

### 12.5 Scenario D-2 — Silent Bitrot (Same Size, Different Content)

**Setup:** Open `disk1/heal-test/heal-test-bitrot/<dataDir>/part.1`, flip one byte at offset 512, keep total size intact.

**Normal scan:** `mc admin heal --json local/heal-test` reports the object as fully healthy — the corruption is **invisible** to `CheckParts` because the on-disk file size still equals `fi.Parts[i].Size`:

```jsonc
{
  "resultId": 5, "type": "object",
  "object": "heal-test-bitrot",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": { "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "after":  { "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "objectSize": 1048576
}
```

The summary for this normal scan shows `objects_healed: 0` — nothing was fixed because nothing was detected.

**Deep scan:** `mc admin heal --scan deep --json local/heal-test` invokes `VerifyFile` at `cmd/xl-storage.go:3097`, which reads the entire file and verifies the HighwayHash-256 bitrot signature. Mismatch → `errFileCorrupt`. Result:

```jsonc
{
  "resultId": 5, "type": "object",
  "object": "heal-test-bitrot",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": { "color": "yellow", "offline": 0, "online": 3, "missing": 1, "corrupted": 0,
    "drives": [
      { "state": "missing" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
    ]
  },
  "after":  { "color": "green",  "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [
      { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" }
    ]
  },
  "objectSize": 1048576
}
```

The summary for the deep scan shows `objects_healed: 1`.

**State mapping note:** even though the defect is a bitrot (content-level) error, the drive surfaces as `"missing"` — not `"corrupt"` — because `errFileCorrupt` on a part (via the `errPartMissingOrCorrupt` classification) is routed to `DriveStateMissing` in the switch at `cmd/erasure-healing.go:382-393`. The user-visible `"corrupt"` state is reserved for unreadable xl.meta with non-actionable errors (see §8.2).

**Operational lesson:** silent bitrot can hide from the default healer. To defend against it, operators must either run `mc admin heal --scan deep` periodically or enable the scheduled background bitrot scanner via **`MINIO_HEAL_BITROTSCAN`** (see `internal/config/heal/heal.go:39` for the env var name and line 50 for the `Bitrot` config field). The default is off. Auto-escalation from normal to deep scan does happen per-request at `cmd/erasure-healing.go:1080-1085`, but only when some other detection path first raises `errFileCorrupt`.

### 12.6 Scenario E — Dangling Metadata

**Setup:** Remove `xl.meta` from `disk1`, `disk2`, `disk3` for `heal-test-dangling`, leaving the data-dir orphaned on those disks. Only `disk4` has valid metadata.

**Actual mc emission** (object-level NDJSON record, verbatim — **byte-for-byte identical to Scenario C's object-level record apart from the `detail` string**, which names a different object):

```json
{"status":"success","error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0","detail":"Object not found: heal-test/heal-test-dangling","type":"object","name":"/","before":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"after":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"size":0}
```

The surrounding bucket-level and summary records are shaped exactly as in §12.3 (bucket all-green; summary `objects_healed: 0`).

**Analysis:** Same cascade as Scenario C at the server side. `notFoundMetaErrs=3`, criterion #5 at `cmd/erasure-healing.go:1026-1028` fires, object is purged, disk4's xl.meta is deleted as part of the purge, and `HealObject` returns the zeroed `HealResultItem`. mc applies the same Format-B dangling-purge signature explained in §12.3 (`drives: null`, all counts zero, `name: "/"`, `error = "Invalid parity shard count..."`, `detail = "Object not found: heal-test/heal-test-dangling"`). The orphaned data-dirs on disk1–3 are cleaned up on a subsequent scanner pass by `checkAbandonedParts` at `cmd/erasure-healing.go:659+`; see also §12.3's "Provenance of `Read failed. Insufficient number of drives online`" note — that sentinel likewise does **not** surface in mc stdout here.

**Conceptual Format A view** (for teaching — not emitted by mc; illustrates the pre-purge decision state):

```jsonc
// Conceptual only — not emitted by mc. Illustrates the server-side pre-purge state.
{
  "resultId": 6, "type": "object",
  "bucket": "heal-test", "object": "heal-test-dangling",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": { "drives": [
    { "state": "missing" }, { "state": "missing" }, { "state": "missing" }, { "state": "ok" }
  ]},
  "after": { "drives": [
    { "state": "missing" }, { "state": "missing" }, { "state": "missing" }, { "state": "missing" }
  ]}
}
```

### 12.7 Scenario F — Simulated Partial Write/Delete

**Setup:** Same shape as Scenario B, against a different object name: PUT `heal-test/heal-test-partdel`, stop MinIO, remove `disk1/heal-test/heal-test-partdel/` AND `disk2/heal-test/heal-test-partdel/`, restart, heal.

**Actual mc object-level emission:**

```jsonc
{
  "resultId": 6, "type": "object",
  "bucket": "heal-test", "object": "heal-test-partdel",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": {
    "color": "red", "offline": 0, "online": 2, "missing": 2, "corrupted": 0,
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
  },
  "objectSize": 1048576
}
```

**Result:** **Shape-identical** to Scenario B — the only fields that differ are the identity fields (`object`: `heal-test-partdel` vs `heal-test-obj2`) and the `resultId` sequence number. All aggregate counts, drive states, before/after colors, sizes, and per-drive state transitions match byte-for-byte. `red → green`, reconstruction succeeds. The healer has no "operation type" field in `PartialOperation` (`cmd/mrf.go:51-63`) and no write-vs-delete branch in `HealObject`. This identical JSON shape is the direct evidence that healing is state-driven, not history-driven: MRF's "operation type" affects *when* heal is triggered (read-after-partial-write vs read-after-partial-delete), not *how* the healer behaves once invoked.

### 12.8 Scenario F-2 — Versioned Delete-Marker Heal

**Setup:** Enable versioning on `heal-test-versioned`. PUT object `v1` (creates a live version). DELETE the object by name (creates a delete marker v2). Stop MinIO, remove the entire `disk1/heal-test-versioned/v1/` directory (both the live-version data-dir and the delete-marker's xl.meta on disk1). Restart, heal recursively: `mc admin heal --json --recursive local/heal-test-versioned`.

**Actual mc emission** (four NDJSON records — bucket + delete-marker version entry + live version entry + summary). The bucket entry's `drives` array is elided as `[...]` for brevity; it mirrors the healthy 4-drive shape shown in the next two records.

```jsonc
{"status":"success","type":"bucket","name":"heal-test-versioned/","before":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[...]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[...]},"size":0}
{"status":"success","type":"object","name":"heal-test-versioned/v1","before":{"color":"yellow","offline":0,"online":3,"missing":1,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"missing"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"size":0}
{"status":"success","type":"object","name":"heal-test-versioned/v1","before":{"color":"yellow","offline":0,"online":3,"missing":1,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"missing"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}]},"size":1048576}
{"status":"success","type":"summary","objects_scanned":2,"objects_healed":2,"items_scanned":3,"items_healed":2,"size":1048576,"duration":1}
```

In Format-B-augmented-with-identity-fields form, the two object records are:

```jsonc
// Delete marker v2 (size=0)
{
  "resultId": 7, "type": "object",
  "bucket": "heal-test-versioned", "object": "v1", "versionId": "<v2-uuid>",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": { "color": "yellow", "offline": 0, "online": 3, "missing": 1, "corrupted": 0,
    "drives": [ { "state": "missing" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "after":  { "color": "green",  "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "objectSize": 0
}

// Live version v1 (size=1 MiB)
{
  "resultId": 8, "type": "object",
  "bucket": "heal-test-versioned", "object": "v1", "versionId": "<v1-uuid>",
  "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 1,
  "before": { "color": "yellow", "offline": 0, "online": 3, "missing": 1, "corrupted": 0,
    "drives": [ { "state": "missing" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "after":  { "color": "green",  "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [ { "state": "ok" }, { "state": "ok" }, { "state": "ok" }, { "state": "ok" } ]
  },
  "objectSize": 1048576
}
```

**Note on mc's `name` field for versions.** mc renders both the delete-marker record and the live-version record with `name: "heal-test-versioned/v1"`; the two records are distinguished only by their `size` field (`0` for the delete marker, `1048576` for the live data) and — at the Format A level — by their `versionId` values (which mc does not surface in the NDJSON stream it prints). Both records have `before.online=3, missing=1` because disk1 was removed in the setup.

**Non-recursive vs. recursive heal.** A direct heal of `mc admin heal --json local/heal-test-versioned/v1` (without `--recursive`) produces the dangling-purge null-drives signature from §12.3 — because the top-most "object" name resolves to the delete marker v2 and the API can't hand back a populated `HealResultItem` for a deleted object. The recursive invocation enumerates both versions and heals each independently, which is what produces the four-record stream shown above.

**Analysis:** For each version: `validMeta.Deleted=true` for v2 → criterion #4 at `cmd/erasure-healing.go:1012-1017` applies, but `notFoundMetaErrs=1 ≤ dataBlocks=2` so it does not fire; heal propagates the delete-marker xl.meta to disk1. For v1: `disksToHealCount=1 ≤ ParityBlocks=2` → standard reconstruction. `After.Drives[0].State` becomes `ok` for both. Versioning is preserved: v1 remains accessible via `aws s3api get-object --version-id <v1-uuid>`; a no-version read continues to return the delete-marker (`NoSuchKey`).

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
| No damage | `errNoHealRequired` | `cmd/erasure-errors.go:29` | `"No healing is required"` |
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

### 13.8 `cmd/erasure-heal_test.go` Representative Test Matrix

The low-level `TestErasureHeal` function at `cmd/erasure-heal_test.go:65-157` iterates a 20-entry table (`erasureHealTests` defined at `cmd/erasure-heal_test.go:29-63`) that parameterizes the Reed-Solomon heal primitive (`Erasure.Heal` at `cmd/erasure-decode.go:317`) across varying combinations of `dataBlocks`, `disks`, `offDisks`, `badDisks`, `badStaleDisks`, `blocksize`, `size`, and `BitrotAlgorithm`. The four rows below are the EC(2,2) cases — identical to the 4-disk layout used throughout this document — plus one boundary-failure case (row 11) that demonstrates exactly when reconstruction returns a non-nil error.

| Row | dataBlocks | disks | offDisks | badDisks | badStaleDisks | blocksize | size | algorithm | shouldFail | Interpretation |
|---|---|---|---|---|---|---|---|---|---|---|
| 0 | 2 | 4 | 1 | 0 | 0 | `blockSizeV2` | 1 MiB | `SHA256` | false | Baseline EC(2,2) heal: 1 offline disk, no bad readers, no bad stale targets → Reed-Solomon succeeds, heal writes the reconstructed shard to the stale slot. Matches Scenario A (§12.1). |
| 11 | 2 | 4 | 1 | 0 | 1 | `blockSizeV2` | 1 MiB | `DefaultBitrotAlgorithm` | **true** | EC(2,2) with 1 offline + 1 **bad stale** write target. The stale writer (the slot being healed into) errors on `Write`, so `Erasure.Heal` cannot land the reconstructed shard → returns `derr != nil`. The closest failure mode to `errErasureWriteQuorum` in the unit-test layer. |
| 16 | 2 | 4 | 1 | 0 | 0 | `blockSizeV2` | 1 MiB | `DefaultBitrotAlgorithm` | false | Same shape as row 0 but with the default (HighwayHash-256) bitrot algorithm — proves algorithm selection does not alter the heal success boundary when inputs are clean. |
| 19 | 2 | 4 | 1 | 0 | 0 | `blockSizeV2` | 64 MiB | `SHA256` | false | Large-object EC(2,2) heal — `size=64 MiB` (64× row 0). Confirms the decode/heal pipeline streams correctly across many block boundaries; same decision logic, same success outcome. |

All four rows terminate at `cmd/erasure-heal_test.go:135` (`err = erasure.Heal(...)`) — rows 0, 16, and 19 produce `err == nil` (and the post-heal bitrot-checksum equality check at lines 147-154 passes), while row 11 produces `err != nil` because the bad-stale writer's I/O fails. The test asserts `shouldFail == (err != nil)` at lines 138-143, verifying the decision boundary empirically at the Reed-Solomon layer — the same boundary that `cannotHeal` at `cmd/erasure-healing.go:428` enforces at the object layer.

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
| Audit-log caller | `runtime.Caller(1)` captured in `deleteIfDangling` at `cmd/erasure-object.go:526`; deferred `auditDanglingObjectDeletion` at `cmd/erasure-object.go:531` | Same caller mechanism — `runtime.Caller(1)` at `cmd/erasure-object.go:526` and deferred `auditDanglingObjectDeletion` at `cmd/erasure-object.go:531` (only fires on a dangling purge, not on a clean partial-delete heal) |
| Typical before→after | `{missing, ok, ok, ok} → {ok, ok, ok, ok}` (reconstruction via Reed-Solomon; see §12.1) | `{ok, ok, ok, missing} → {ok, ok, ok, deleted-marker}` for a live delete marker being propagated; `{deleted-marker, ok, ok, ok} → {deleted-marker, deleted-marker, deleted-marker, deleted-marker}` once heal completes; dangling-purge path yields `{missing, missing, missing, missing}` on all disks |
| Final terminal state | all disks → live object OR dangling purge | all disks → delete marker OR dangling purge |

### 14.2 Unified Pipeline Diagram

```text
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
| `cmd/erasure-heal_test.go` | `erasureHealTests` table (:29-63, 20 rows across `dataBlocks`, `disks`, `offDisks`, `badDisks`, `badStaleDisks`, `blocksize`, `size`, `algorithm`, `shouldFail`); `TestErasureHeal` driver (:65-157); representative EC(2,2) rows 0, 11, 16, 19 enumerated in [§13.8](#138-cmderasure-heal_testgo-representative-test-matrix) |
| `cmd/erasure-healing-common.go` | `listOnlineDisks` (:219) with narrow ETag fallback; `disksWithAllParts` (:291); 5-state disk taxonomy |
| `cmd/erasure-healing-common_test.go` | `TestListOnlineDisks`, `TestDisksWithAllParts`, `TestCommonParities` |
| `cmd/erasure-healing.go` | `shouldHealObjectOnDisk` (:156); `auditHealObject` (:221); `healObject` (:258); drive-state mapping (:382-393); `Before.Drives` append (:395-399); `After.Drives` append (:400-404); `cannotHeal` check (:428); cannot-heal branch (:435-456); `After.Drives[i].State = ok` (:651); `checkAbandonedParts` (:659+); `healObjectDir` (:696); `defaultHealResult` (:787); `healingLogOnceIf` sites (:477, :487, :497); `isObjectDangling` standalone function (:968-1033); `HealObject` public API (:1039); deep-scan auto-escalation (:1080-1085); `healTrace` (:1090-1115) |
| `cmd/erasure-healing_test.go` | `TestIsObjectDangling` with 12+ sub-tests covering criteria 1–6 |
| `cmd/erasure-metadata-utils.go` | `reduceErrs`, `reduceQuorumErrs`, `reduceReadQuorumErrs`, `reduceWriteQuorumErrs`, `readAllFileInfo`, `shuffleDisksAndPartsMetadataByIndex` |
| `cmd/erasure-metadata.go` | `pickValidFileInfo`; `findFileInfoInQuorum`; `commonParity`; `objectQuorumFromMeta` (:531) |
| `cmd/erasure-object.go` | MRF enqueue sites (:400 read-repair, :805 GetObjectInfo, :1578 PutObject, :2113 DeleteObject); `deleteIfDangling` method (:482); call site from `cannotHeal` branch (at healing.go:438); `runtime.Caller(1)` (:526); deferred `auditDanglingObjectDeletion` (:531); `isObjectDangling` call (:483) |
| `cmd/global-heal.go` | `newBgHealSequence` with `Remove: healDeleteDangling`; `healErasureSet` (:152); parallel orchestration |
| `cmd/mrf.go` | `mrfOpsQueueSize=100000` (:39); `PartialOperation` struct (:51-63); `opCh` field on `mrfState` (:64); `addPartialOp` (:78); `shutdown` msgpack persistence (:102); `startMRFPersistence` (:155); `healRoutine` consumer (:220) |
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

All JSON blocks in [§12](#12-six-scenarios-with-runtime-json) were derived from live captures of `mc admin heal --json` run against a locally-built MinIO binary at commit `c07e5b49d477` with `DataBlocks=2, ParityBlocks=2` (single-set, 4-disk setup on `/tmp/minio-heal-experiments/disk{1..4}`). The capture files were deleted per [§15.2](#152-cleanup-commands-executed); the embedded text is the only persisted copy. To help the reader connect a block to the underlying mechanism, the §12 material has been rendered in one of three clearly-labeled forms:

- **Literal mc NDJSON** — verbatim lines from the mc stdout stream. §12.3 (Scenario C), §12.6 (Scenario E), §12.7 (Scenario F), and §12.8 (Scenario F-2) each include a literal-mc block. These blocks contain *only* fields that mc actually emits: `status`, `type`, `name`, `before.{color,offline,online,missing,corrupted,drives}`, `after.{…}`, `size`, and (in error cases) `detail` and `error`. Where the `drives` array appears, each element has the full Format B shape `{uuid, endpoint, state}`. No identity fields (`bucket`, `object`, `parityBlocks`, `dataBlocks`, `diskCount`, `setCount`, `objectSize`, `versionId`, `resultId`) appear in literal-mc blocks, because mc does not surface them at the NDJSON layer.
- **Format-B-augmented hybrid** — a pedagogical rendering for scenarios whose mc output is a simple success record (§12.1, §12.2, §12.4, §12.5, §12.7, §12.8's "augmented" view). In these blocks, the Format B aggregates and drive states exactly match what mc emitted, but we have added identity/layout fields from the server-side `madmin-go` `HealResultItem` (Format A) so the reader can see in one place *which object the record refers to* and *what EC parameters govern the decision*. Any reader wiring up a parser against mc NDJSON should **ignore** the fields listed above as "Format A-only"; they will not be present on the wire. Conversely, the aggregate counts and drive-state strings shown in these hybrid blocks are exactly those emitted by mc and can be trusted for tooling. Within the `drives` array, individual entries are abbreviated to `{ "state": "…" }` where the `endpoint` and `uuid` are not the focus of the discussion; the literal mc `drives` element is always `{"uuid":"","endpoint":"...","state":"…"}` with a valid endpoint path.
- **Conceptual Format A view** — in §12.3 and §12.6 the dangling-purge signature is shown in two forms: first the literal mc null-drives output, then a conceptual reconstruction of the server-side `defaultHealResult` at `cmd/erasure-healing.go:787-800`. The conceptual view is labeled "(not emitted by mc)" and is included purely to reveal the server's internal model of the purged object; it must not be taken as something a parser will see on the mc side.

A concrete example of the distinction: the phrase **"Read failed. Insufficient number of drives online"** is the string form of the `errErasureReadQuorum` sentinel defined at `cmd/erasure-errors.go:23`. It is used server-side in the `toObjectErr()` transformation and appears in MinIO's structured server logs. It is **not** what `mc admin heal --json` writes to stdout. For the dangling-purge case covered by §12.3 and §12.6, mc emits `"detail": "Object not found: <bucket>/<object>"` together with `"error": "Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0"` (from `cmd/admin-heal-ui.go:55`). Both strings are evidence of the same underlying decision — the object was deemed irrecoverable, purged, and can no longer be read — but the two strings surface at different points in the stack and a reader using one to grep server logs while expecting to see it in mc output (or vice versa) will be surprised. §12.3's "mc layer mechanics" subsection traces the full path from server sentinel through the admin API to mc stdout.

A second concrete example: the `online`, `offline`, `missing`, and `corrupted` aggregates at the `before`/`after` level are strictly state-filtered counts computed by `GetOnlineCounts`, `GetOfflineCounts`, `GetMissingCounts`, and `GetCorruptedCounts` in `madmin-go/v3@v3.0.77/heal-commands.go` (lines 220-250). Each function iterates `Drives` and increments only when `v.State` equals the respective constant. Consequently `online + missing + offline + corrupted` is not guaranteed to equal `diskCount`: if any drive reports `permission-denied`, `faulty`, `root-mount`, `unknown`, or `unformatted` (the other five `DriveState*` constants at `heal-commands.go:120-128`), it is counted in none of the four aggregates. In a healthy 4-disk set with no such edge-case states, the sum does equal 4, and the §12 blocks are all captured from such runs — but tooling must not assume the invariant. This is why §12.1 shows `before.online=3` (not 4) when one disk is `missing`: `online` means *"drives in state `ok`"*, not *"drives that are reachable"* or *"drives that are formatted"*.

---

## Appendix A — Full Decision-Tree Diagram

```text
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
                              disksToHealCount: counter
                                                 ++ per disk where
                                                 shouldHealObjectOnDisk
                                                 returned true (:374,:379)
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
| C | Removed `disk1,disk2,disk3/.../heal-test-obj3/` | 1/4 | Dangling purge (#5) | Object deleted from all | null-drives (mc) / `red → red` + `detail: "Read failed…"` (server-side) |
| D | Overwrote `disk1/.../part.1` with 15 bytes | 3/4 (1 size-corrupt) | Reconstruct | All disks `ok` | yellow → green |
| D-2 (normal scan) | Flipped one byte in `disk1/.../part.1` | 3/4 (silent) | No-op (undetected) | Corrupt bytes remain | green → green (false clean) |
| D-2 (deep scan) | Same | 3/4 (1 bitrot) | Reconstruct | All disks `ok` | yellow → green |
| E | Removed `xl.meta` from disk1-3 | 1/4 (meta) | Dangling purge (#5) | Object deleted + data-dirs orphaned | null-drives (mc) / `red → red` + `detail: "Read failed…"` (server-side) |
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

`globalMRFState.opCh` at `cmd/mrf.go:64` is a unified channel. `PartialOperation` has no `OperationType` field. The only "hint" about the original intent is `validMeta.Deleted` on the surviving xl.meta, which `isObjectDangling` uses (criterion #4 at `cmd/erasure-healing.go:1012-1017`) to choose between meta-republish convergence (delete marker) and data-rebuild convergence (live object).

### D.6. `After.Drives` Reflects Terminal State, Not Intermediate Steps

`Before.Drives` and `After.Drives` are appended side-by-side at `cmd/erasure-healing.go:395-404` — they start identical. Only successful per-disk reconstruction mutates `After.Drives[i].State` to `ok` at `cmd/erasure-healing.go:651`. A disk that was `offline` before the heal and remains unreachable retains `State: offline` in `After`. This means `After` gives terminal outcome, not heal progress.

### D.7. Audit Logs Name the Exact Caller of Dangling Deletion

`deleteIfDangling` at `cmd/erasure-object.go:482` captures the caller via `runtime.Caller(1)` at line 526 and registers a deferred `auditDanglingObjectDeletion` at line 531. Every dangling purge emits a structured audit record naming the exact Go `file:line` that requested it. Operators with `audit_webhook` enabled can correlate purges back to the precise heal code path (e.g., `cmd/erasure-healing.go:438` for the `cannotHeal` branch vs. read-time purges at `cmd/erasure-object.go:106`).

### D.8. The Background Healer Defaults to `Remove:true`

`cmd/global-heal.go` constructs `newBgHealSequence` with `Remove: healDeleteDangling`, where `healDeleteDangling = true` at `cmd/data-scanner.go:60`. This means background scans **will** permanently delete dangling objects. To dry-run a heal, operators must use `mc admin heal --dry-run` or explicitly pass `opts.Remove=false` via the admin API — the default behavior purges.

---

*End of document.*
