# MinIO Erasure-Code Healing Investigation — Commit `c07e5b49d477`

> **Repository:** `github.com/minio/minio`
> **Commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`
> **Branch (destination):** `blitzy-f5be74c1-0f58-4ffb-9004-26bb09773660`
> **Scope:** Single-site, single-pool, 4-drive erasure set (EC `DataBlocks=2`, `ParityBlocks=2`) with deep dives into code paths at `cmd/erasure-healing.go`, `cmd/erasure-healing-common.go`, `cmd/erasure-object.go`, `cmd/mrf.go`, and supporting files.
> **Source of truth:** The MinIO source tree at the pinned commit. Every claim is backed by either `<file>:<line>` references or verbatim runtime JSON captured from `mc admin heal --json`.
> **Document purpose:** Answer, with code-level rigor, how MinIO's healing subsystem decides *whether* to reconstruct an object, *when* it deliberately leaves data untouched, and *when* it purges the remnants of a partially-failed operation.

---

## Table of Contents

1. [Title + Preface](#minio-erasure-code-healing-investigation--commit-c07e5b49d477) — this page
2. [Table of Contents](#table-of-contents) — you are here
3. [Runtime Environment Statement](#3-runtime-environment-statement)
4. [Executive Summary](#4-executive-summary)
5. [Healing Architecture Overview](#5-healing-architecture-overview)
6. [Q1 — Healing Decision Logic Under Mixed State](#6-q1--healing-decision-logic-under-mixed-state)
7. [Q2 — Reconstruct vs. Stay-Deleted/Degraded: Exact Code Paths](#7-q2--reconstruct-vs-stay-deleteddegraded-exact-code-paths)
8. [Q3 — What `HealResultItem` Reveals About the Decision](#8-q3--what-healresultitem-reveals-about-the-decision)
9. [Q4 — What Logs Explain](#9-q4--what-logs-explain)
10. [Q5 — Minimum Valid Shards, Failure Errors, Boundary](#10-q5--minimum-valid-shards-failure-errors-boundary)
11. [Q6 — Partially Failed Write vs. Partially Failed Delete](#11-q6--partially-failed-write-vs-partially-failed-delete)
12. [Six Scenarios (A–F) with JSON Evidence](#12-six-scenarios-af-with-json-evidence)
13. [Boundary Conditions Summary Table](#13-boundary-conditions-summary-table)
14. [Partial Write vs. Partial Delete Comparison Table](#14-partial-write-vs-partial-delete-comparison-table)
15. [Cleanup Evidence](#15-cleanup-evidence)
16. [References](#16-references)

Appendices:

- [Appendix A — Full Decision Tree (ASCII Diagram)](#appendix-a--full-decision-tree-ascii-diagram)
- [Appendix B — Scenario Outcome Summary Matrix](#appendix-b--scenario-outcome-summary-matrix)
- [Appendix C — Reproduction Recipe](#appendix-c--reproduction-recipe)
- [Appendix D — Eight Key Insights (Operator Cheat-Sheet)](#appendix-d--eight-key-insights-operator-cheat-sheet)

---

## 3. Runtime Environment Statement

This investigation was conducted with a fully operational runtime environment:

| Item | Value |
|---|---|
| Go toolchain | `go1.23.6 linux/amd64` |
| Build command | `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .` |
| MinIO binary | `/tmp/minio-bin/minio` (`DEVELOPMENT.GOGET`) |
| MinIO client | `mc` `RELEASE.2025-08-13` |
| Repository commit | `c07e5b49d477b0774f23db3b290745aef8c01bd2` |
| Erasure set layout | 4 local drives `/tmp/minio-heal-test/disk{1..4}` |
| Resulting EC tuple | `DataBlocks = 2`, `ParityBlocks = 2`, `readQuorum = 2`, `writeQuorum = 3` |
| Bitrot algorithm | HighwayHash-256 (MinIO default) |
| Admin credentials | `MINIO_ROOT_USER=minioadmin`, `MINIO_ROOT_PASSWORD=minioadmin123` |
| Listen address | `127.0.0.1:9300` |

**All runtime JSON captured in the "Six Scenarios" section below is verbatim output** from `mc admin heal --json` executed against this configuration. Where drive states appear in tables, they reflect the actual observed classification emitted by MinIO's healing API at this commit.

All ephemeral infrastructure (the MinIO binary, the four disk directories, the test bucket `healtest`, the test objects, and the running `minio server` process) was torn down after all scenarios completed. The only persisted artifact is this document. See [§15 Cleanup Evidence](#15-cleanup-evidence) for the tear-down transcript.

---

## 4. Executive Summary

MinIO's healing subsystem is a **deterministic state machine driven by the current on-disk condition** of each drive, not by a per-operation history. When an admin heal request arrives (or the background scanner picks an object from its 1-in-1024 sample at `cmd/data-scanner.go:healObjectSelectProb`), the healer performs a five-stage decision cascade:

1. **Can read quorum be established?** `objectQuorumFromMeta` at `cmd/erasure-metadata.go:531` requires a majority of drives to agree on `DataBlocks` and `ParityBlocks`.
2. **Which disks hold the canonical latest metadata?** `listOnlineDisks` at `cmd/erasure-healing-common.go:219` votes on `modTime` then `ETag`.
3. **Which disks have intact parts?** `disksWithAllParts` at `cmd/erasure-healing-common.go:291` runs `CheckParts` (normal scan) or `VerifyFile` (deep scan).
4. **How many disks need healing?** `shouldHealObjectOnDisk` at `cmd/erasure-healing.go:156` classifies each drive as OK, Offline, Missing, or Corrupt.
5. **Can we still reconstruct?** The pivotal threshold at `cmd/erasure-healing.go:428`: `cannotHeal := !XLV1 && !Deleted && disksToHealCount > ParityBlocks`.

The output is one of **five** terminal outcomes:

| # | Outcome | Trigger | Key line |
|---|---|---|---|
| 1 | **No-op** | `disksToHealCount == 0` | `erasure-healing.go:417-420` |
| 2 | **Reconstruct** | `disksToHealCount ≤ ParityBlocks` | `erasure-healing.go:458–651` |
| 3 | **ETag override reconstruct** | `cannotHeal` but `quorumETag != ""` | `erasure-healing.go:429-433` |
| 4 | **Dangling purge** | `isObjectDangling` ⇒ true | `erasure-healing.go:438–456`, `erasure-object.go:482-563` |
| 5 | **Leave-alone (`errErasureReadQuorum`)** | `isObjectDangling` ⇒ false, quorum fails | `erasure-object.go:487`, `erasure-errors.go` |

**The most important operator-facing insight:** the healer never flips a coin. Every decision is traceable to a specific line in `erasure-healing.go` and can be reproduced from the `HealResultItem` JSON.

### 4.1 Core Source Files

| File | LOC | Role in healing |
|---|---|---|
| `cmd/erasure-healing.go` | 1116 | Main orchestrator: `HealObject`, `healObject`, `isObjectDangling`, drive-state classification |
| `cmd/erasure-healing-common.go` | 459 | Disk triage: `listOnlineDisks`, `disksWithAllParts`, the 5-state disk model |
| `cmd/erasure-object.go` | 2400+ | CRUD paths + `deleteIfDangling` (line 482) + MRF trigger sites |
| `cmd/erasure-decode.go` | 364 | `Erasure.Heal` (line 317): Reed-Solomon shard reconstruction |
| `cmd/erasure-metadata.go` | 600+ | `objectQuorumFromMeta` (line 531), `pickValidFileInfo`, `commonParity` |
| `cmd/erasure-metadata-utils.go` | 382 | `readAllFileInfo`, `reduceReadQuorumErrs` |
| `cmd/erasure-errors.go` | 30 | `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired` |
| `cmd/mrf.go` | 284 | `PartialOperation`, `addPartialOp`, `healRoutine`, msgpack persistence |
| `cmd/global-heal.go` | 598 | Background healer: `newBgHealSequence`, `healErasureSet` |
| `cmd/admin-heal-ops.go` | ~900 | Session state machine: `LaunchNewHealSequence`, `pushHealResultItem` |
| `cmd/background-heal-ops.go` | 189 | Task queue: `healRoutine`, `waitForLowIO`, worker pool |
| `cmd/storage-datatypes.go` | ~560 | `checkPart*` constants (lines 530-540) |
| `cmd/data-scanner.go` | — | `healDeleteDangling = true`, `healObjectSelectProb = 1024` |
| `internal/config/heal/heal.go` | 189 | Heal config: `Bitrot`, `Sleep`, `IOCount`, `DriveWorkers` |
| `buildscripts/verify-healing.sh` | 168 | Canonical 3-node integration test |
| `buildscripts/heal-manual.go` | 87 | Admin-SDK heal example |

### 4.2 External Type References (from `github.com/minio/madmin-go/v3`)

- **`HealOpts`** — fields `DryRun`, `Remove`, `Recursive`, `NoLock`, `ScanMode`, `UpdateParity`, `Pool`, `Set`.
- **`HealScanMode`** — `HealNormalScan = 0`, `HealDeepScan = 1`.
- **`HealItemType`** — `HealItemMetadata`, `HealItemBucket`, `HealItemBucketMetadata`, `HealItemObject`.
- **`HealResultItem`** — `{ResultIndex, Type, Bucket, Object, VersionID, DetailedMsg, DiskCount, SetCount, ParityBlocks, DataBlocks, Before.Drives, After.Drives, ObjectSize}`.
- **`HealDriveInfo`** — `{UUID, Endpoint, State}`; `State` is one of `DriveStateOk`, `DriveStateOffline`, `DriveStateMissing`, `DriveStateCorrupt`, `DriveStateFaulty`, `DriveStatePermission`, `DriveStateRootMount`, `DriveStateUnknown`, `DriveStateUnformatted`.

---

## 5. Healing Architecture Overview

### 5.1 Entry Point: `HealObject` (`cmd/erasure-healing.go:1038-1087`)

```go
func (er erasureObjects) HealObject(ctx context.Context, bucket, object, versionID string,
                                     opts madmin.HealOpts) (hr madmin.HealResultItem, err error) {
    // (1) Wire trace/audit context.
    // (2) Dispatch directory objects to healObjectDir.
    // (3) If versionID == "", substitute nullVersionID.
    // (4) First pass: readAllFileInfo lockless.
    //     - If isAllNotFound(errs), short-circuit with errFileNotFound/errFileVersionNotFound.
    // (5) Call healObject(...) with the full opts.
    // (6) If err == errFileCorrupt and opts.ScanMode != madmin.HealDeepScan:
    //       retry with opts.ScanMode = HealDeepScan.  [lines 1080-1085]
}
```

The deep-scan auto-escalation at lines 1080-1085 is the safety net for silent bitrot: a normal-scan heal that gets `errFileCorrupt` from the inline verifier automatically re-runs with `HealDeepScan`, which drives `VerifyFile` to re-checksum every byte.

### 5.2 `healObject` Internal Pipeline (`cmd/erasure-healing.go:258-657`)

The 11 ordered steps inside `healObject`:

```
 1. Initialize result := madmin.HealResultItem{Type: HealItemObject, Bucket, Object, VersionID, DiskCount: N}
                                                       [lines 258-283]
 2. Acquire namespace lock (er.NewNSLock(...).GetLock) unless opts.NoLock
                                                       [lines 285-293]
 3. Re-read xl.meta under lock via readAllFileInfo(..., true /* ReadData */, true /* ReadVersions */)
                                                       [line 296]
 4. If isAllNotFound(errs) → return defaultHealResult(errFileNotFound/errFileVersionNotFound)
                                                       [lines 302-305]
 5. readQuorum, writeQuorum, err := objectQuorumFromMeta(...)
                                                       [line 308]
    On err (cannot form quorum) → invoke deleteIfDangling and return (BRANCH A).
                                                       [lines 309-324]
 6. result.ParityBlocks = N - readQuorum ; result.DataBlocks = readQuorum
                                                       [lines 326-327]
 7. onlineDisks, quorumModTime, quorumETag := listOnlineDisks(...)
                                                       [line 331]
    latestMeta, err := pickValidFileInfo(..., quorumModTime, quorumETag, readQuorum)
                                                       [line 341]
 8. availableDisks, dataErrsByDisk, dataErrsByPart := disksWithAllParts(..., filterDisksByETag, scanMode)
                                                       [lines 352-353]
 9. For each disk index i ∈ [0..N):
       yes, reason := shouldHealObjectOnDisk(errs[i], dataErrsByDisk[i], partsMetadata[i], latestMeta)
       if yes: outDatedDisks[i] = storageDisks[i]; disksToHealCount++
       Classify errors → driveState (Ok | Offline | Missing | Corrupt)   [lines 382-393]
       Append both Before.Drives[i] and After.Drives[i] with the same initial state.
                                                       [lines 373-405]
10. Short-circuits:
       - isAllNotFound(errs) → errFileNotFound/errFileVersionNotFound       [407-415]
       - disksToHealCount == 0 → return (no-op)                              [417-420]
       - opts.DryRun → return pre-heal classification                         [424-426]
11. cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
                                                       [line 428]
    if cannotHeal && quorumETag != "" { cannotHeal = false }  ← ETag override  [429-433]
    if cannotHeal → deleteIfDangling(...) + fill errs with the returned err (BRANCH B).
                                                       [lines 435-456]
    else → enter reconstruction path (lines 458+):
       for each part:
         newBitrotReader(latestDisks...) ; newBitrotWriter(outDatedDisks...)
         erasure.Heal(writers, readers, partSize, prefer)                    [uses erasure-decode.go:317]
       for each healed disk i:
         fileInfo.SetHealing() ; RenameData(tmp → final)
         result.After.Drives[i].State = DriveStateOk                          [line 651]
       checkAbandonedParts(...) — fan-out CleanAbandonedData on all disks     [662-694]
```

### 5.3 Per-Disk State Classification (`cmd/erasure-healing.go:156-183` and `:382-393`)

`shouldHealObjectOnDisk` is the inner classifier for a single disk's observation:

```go
// cmd/erasure-healing.go:156-183
func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo, latestMeta FileInfo) (bool, error) {
    switch {
    case errors.Is(erErr, errFileNotFound), errors.Is(erErr, errFileVersionNotFound),
         errors.Is(erErr, errFileCorrupt):
        return true, erErr
    }
    if erErr == nil {
        if meta.XLV1 { return true, errLegacyXLMeta }
        if !latestMeta.Equals(meta) { return true, errOutdatedXLMeta }
        for _, partErr := range partsErrs {
            if partErr == checkPartFileNotFound || partErr == checkPartFileCorrupt {
                return true, errPartMissingOrCorrupt
            }
        }
    }
    return false, nil
}
```

Outer mapping in `healObject` (lines 382-393) converts the returned `reason` into a user-visible `DriveState`:

```go
driveState := ""
switch {
case reason == nil:
    driveState = madmin.DriveStateOk
case IsErr(reason, errDiskNotFound):
    driveState = madmin.DriveStateOffline
case IsErr(reason, errFileNotFound, errFileVersionNotFound, errVolumeNotFound,
                   errPartMissingOrCorrupt, errOutdatedXLMeta, errLegacyXLMeta):
    driveState = madmin.DriveStateMissing
default:
    driveState = madmin.DriveStateCorrupt
}
```

A critical and counter-intuitive mapping: `errFileCorrupt` and `errOutdatedXLMeta` and `errLegacyXLMeta` all map to **`"missing"`**, not `"corrupt"`. A drive state of `"corrupt"` appears only for unmapped/unknown errors (e.g., an unexpected I/O failure pattern). This directly affects how operators interpret `before.drives[].state` in heal JSON.

**Part-integrity verification** (the source of `partsErrs` above):

| Value | Constant | Trigger | Scan mode needed |
|---|---|---|---|
| 0 | `checkPartUnknown` | Pre-check state | — |
| 1 | `checkPartSuccess` | Part OK | normal |
| 2 | `checkPartDiskNotFound` | Disk offline | normal |
| 3 | `checkPartVolumeNotFound` | Bucket vol missing | normal |
| 4 | `checkPartFileNotFound` | Part file absent | normal |
| 5 | `checkPartFileCorrupt` | Size mismatch OR bitrot | **deep** |

Constants at `cmd/storage-datatypes.go:530-540`. `checkPartFileCorrupt` is returned by `xl-storage.go:CheckParts` when the on-disk file size does not match `fi.Parts[i].Size`. Only `xl-storage.go:VerifyFile` (invoked when `scanMode == HealDeepScan`) actually reads bytes through the HighwayHash bitrot verifier and catches *same-size content corruption*.

### 5.4 Quorum Math for EC(2,2)

Computed by `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`):

| Variable | Formula | EC(2,2) value |
|---|---|---|
| `N` (disk count) | len(partsMetadata) | 4 |
| `parityBlocks` | `commonParity(parities, defaultParityCount)` | 2 |
| `dataBlocks` | `N - parityBlocks` | 2 |
| `readQuorum` | `dataBlocks` | 2 |
| `writeQuorum` | `dataBlocks` when `dataBlocks > parityBlocks`, else `dataBlocks + 1` | 3 |
| `cannotHeal` threshold | `disksToHealCount > parityBlocks` | > 2 |
| `DriveState "missing"` tolerance | up to `parityBlocks` disks | ≤ 2 |

The `writeQuorum = dataBlocks + 1` bump when `dataBlocks == parityBlocks` is the reason write quorum for EC(2,2) is **3 drives**, not 2. This matters for MRF: a write that landed on only 2 of 4 drives is not committed; the client sees an error and no MRF enqueue happens. A write that landed on 3 of 4 is committed; the 1 missing drive becomes an MRF healing target.

### 5.5 The Five Dangling-Object Criteria (`cmd/erasure-healing.go:968-1036`)

`isObjectDangling` is the single function that decides whether an object with mixed errors is *legitimately unrecoverable* vs. *possibly recoverable*. It is called from `deleteIfDangling` (`cmd/erasure-object.go:482`).

```go
// cmd/erasure-healing.go:968-1036  (simplified)
func (er erasureObjects) isObjectDangling(metaArr []FileInfo, errs []error,
                                          dataErrsByPart map[int][]int) (validMeta FileInfo, ok bool) {
    notFoundMetaErrs, nonActionableMetaErrs := danglingMetaErrsCount(errs)

    notFoundPartsErrs, nonActionablePartsErrs := 0, 0
    for _, partErrs := range dataErrsByPart {
        n, na := danglingPartErrsCount(partErrs)
        if n > notFoundPartsErrs     { notFoundPartsErrs = n }
        if na > nonActionablePartsErrs { nonActionablePartsErrs = na }
    }

    for _, m := range metaArr {
        if m.IsValid() { validMeta = m; break }
    }

    // (#1) No valid meta: default to dataBlocks (conservative) — purge only if too many parts gone.
    if !validMeta.IsValid() {
        dataBlocks := (len(metaArr) + 1) / 2              // (#2) conservative fallback
        if notFoundPartsErrs > dataBlocks { return validMeta, true }
        return validMeta, false
    }

    // (#3) ANY non-actionable error blocks purge — safety valve.
    if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
        return validMeta, false
    }

    // (#4) Delete marker: purge only if missing-meta exceeds dataBlocks (stricter).
    if validMeta.Deleted {
        dataBlocks := (len(errs) + 1) / 2
        return validMeta, notFoundMetaErrs > dataBlocks
    }

    // (#5) Normal object: purge if missing-meta exceeds parityBlocks.
    if notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks {
        return validMeta, true
    }

    // (#6) Non-remote (inline + local data-dir) object: purge if missing-parts exceeds parityBlocks.
    if !validMeta.IsRemote() && notFoundPartsErrs > 0 &&
       notFoundPartsErrs > validMeta.Erasure.ParityBlocks {
        return validMeta, true
    }

    return validMeta, false                                // (#7) default: leave alone
}
```

Summary of the criteria:

| # | Condition | Outcome |
|---|---|---|
| 1 | `!validMeta && notFoundParts > (N+1)/2` | **Dangling — purge** |
| 2 | `!validMeta && notFoundParts ≤ (N+1)/2` | Leave alone (conservative) |
| 3 | `nonActionableMeta > 0` or `nonActionableParts > 0` | **Leave alone** (safety valve) |
| 4 | `validMeta.Deleted && notFoundMeta > (N+1)/2` | **Dangling — purge** (delete-marker) |
| 5 | `notFoundMeta > parityBlocks` | **Dangling — purge** |
| 6 | `!IsRemote && notFoundParts > parityBlocks` | **Dangling — purge** |

The **non-actionable** error category (criterion #3) is the critical safety valve. `danglingMetaErrsCount` and `danglingPartErrsCount` classify errors as:

- **Not-found**: `errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errFileCorrupt`, `checkPartFileNotFound`, `checkPartFileCorrupt`.
- **Non-actionable**: `errDiskNotFound`, `errUnformattedDisk`, permission errors, unclassified I/O errors.

When *any* drive returns a non-actionable error, the healer refuses to purge, because it cannot distinguish "disk dead" from "disk has data we can't read right now". This is the single most important reason MinIO preserves data under ambiguous failure patterns.

### 5.6 Reconstruction: `Erasure.Heal` (`cmd/erasure-decode.go:317-364`)

After the healer classifies drives and determines reconstruction is viable, it calls `Erasure.Heal` per part:

```go
// cmd/erasure-decode.go:317-364
func (e Erasure) Heal(ctx context.Context, writers []io.Writer, readers []io.ReaderAt,
                      totalLength int64, prefer []bool) error {
    reader := newParallelReader(readers, e, 0, totalLength)
    if prefer != nil { reader.preferReaders(prefer) }
    startBlock, endBlock := int64(0), totalLength/e.blockSize
    for block := startBlock; block <= endBlock; block++ {
        bufs, err := reader.Read(bufs)
        if len(bufs) > 0 {
            if err != nil && !IsQuorumDecodeError(err) { return err }
            if err = e.DecodeDataAndParityBlocks(ctx, bufs); err != nil { return err }
            // Write reconstructed shards to outdated disks.
            if err = writeDataBlocks(ctx, writers, bufs, e.dataBlocks, 0, blockSize); err != nil {
                return err
            }
        }
    }
    // multiWriter writeQuorum = 1 — each outdated disk is written independently.
    return nil
}
```

Key behaviors:

- The readers point to the *online* disks; the writers point to the *outdated* (to-be-healed) disks' temp locations.
- Reed-Solomon requires at least `DataBlocks` surviving shards per block (via `DecodeDataAndParityBlocks`).
- Writers have `writeQuorum = 1` per disk — a failed write to one outdated disk decrements `disksToHealCount` but does not abort the whole heal.
- After `Erasure.Heal` completes, the caller issues `RenameData(tmp → final)` and sets `result.After.Drives[i].State = DriveStateOk` at line 651.

### 5.7 Background Healing Infrastructure

Three subsystems cooperate:

1. **`cmd/global-heal.go` — `healErasureSet`** (line 152). Iterates all buckets and objects in an erasure set, invokes `HealObject`. The background sequence is built by `newBgHealSequence` with `Remove: healDeleteDangling = true` (`cmd/data-scanner.go`). Sampled via `healObjectSelectProb = 1024` (1-in-1024 objects picked on each scan pass).
2. **`cmd/background-heal-ops.go` — `healRoutine`** (lines 1-189). Worker pool driven by `GOMAXPROCS/2` (min 4), overridable via `_MINIO_HEAL_WORKERS`. Throttled by `waitForLowIO` (100ms tick, sleeps until in-flight I/O falls below `IOCount = 100`).
3. **`cmd/mrf.go` — `healRoutine`** (lines 210+). Consumes `globalMRFState.opCh` (capacity 100,000) and dispatches partial ops to `HealObject` / `HealBucket`. Skips meta-bucket entries (`.metacache`, `tmp`, `multipart`, `tmp-old`). Waits 1 second after `Queued` to give transient failures time to recover.

### 5.8 Healing Decision Cascade (ASCII)

```
                           ┌──────────────────────────────────┐
                           │      admin heal / MRF / bg       │
                           │   → HealObject(b, o, vid, opts)  │
                           └───────────────┬──────────────────┘
                                           │
                    ┌──────────────────────▼───────────────────────┐
                    │ readAllFileInfo (lockless) → errs[]          │
                    └──────────────────────┬───────────────────────┘
                                           │
                          ┌────────────────▼─────────────┐
                          │ isAllNotFound(errs)?         │──YES──► return errFileNotFound
                          └────────────────┬─────────────┘
                                           │ NO
                    ┌──────────────────────▼───────────────────────┐
                    │ healObject: lock + re-read + classify        │
                    └──────────────────────┬───────────────────────┘
                                           │
                          ┌────────────────▼─────────────┐
                          │ objectQuorumFromMeta ok?     │──NO──► deleteIfDangling (BRANCH A)
                          └────────────────┬─────────────┘                │
                                           │ YES                          ▼
                    ┌──────────────────────▼───────────────────────┐     isObjectDangling?
                    │ listOnlineDisks → pickValidFileInfo          │      ├─ true → purge
                    │ disksWithAllParts → dataErrsByDisk/ByPart    │      └─ false → errErasureReadQuorum
                    └──────────────────────┬───────────────────────┘
                                           │
                    ┌──────────────────────▼───────────────────────┐
                    │ shouldHealObjectOnDisk per drive → states    │
                    │ disksToHealCount := count                    │
                    └──────────────────────┬───────────────────────┘
                                           │
                          ┌────────────────▼─────────────────┐
                          │ disksToHealCount == 0?           │──YES──► return (no-op)
                          └────────────────┬─────────────────┘
                                           │ NO
                          ┌────────────────▼─────────────────┐
                          │ opts.DryRun?                     │──YES──► return current state
                          └────────────────┬─────────────────┘
                                           │ NO
                          ┌────────────────▼─────────────────┐
                          │ cannotHeal = toHeal > parity?    │
                          └────────────────┬─────────────────┘
                                           │
                   ┌───────────────────────┼──────────────────────────┐
                   │ YES                   │                          │ NO
                   ▼                       ▼                          ▼
         ┌─────────────────────┐   ┌──────────────────────┐   ┌──────────────────┐
         │ quorumETag != ""?   │   │ (same dangling path) │   │ reconstruct path │
         │  →OVERRIDE cannot=F │   │                      │   │ erasure.Heal()   │
         └──────────┬──────────┘   └──────────┬───────────┘   │ RenameData()     │
                    │                         │               │ After.Drives=Ok  │
                    ▼ (now NO)                ▼               │ checkAbandoned() │
         ┌──────────────────────┐   ┌──────────────────────┐   └──────────────────┘
         │ → reconstruct path   │   │ deleteIfDangling:    │
         │   (as NO branch)     │   │ isObjectDangling?    │
         └──────────────────────┘   │  ├─ true → purge all │
                                    │  └─ false → ReadQ err│
                                    └──────────────────────┘
```

### 5.9 MRF (Most Recently Failed) Pipeline (ASCII)

```
     ┌─────────────────────────┐  ┌─────────────────────────┐  ┌─────────────────────────┐
     │ Read path (line 400)    │  │ Put path (line 1574)    │  │ Delete path (line 2113) │
     │ BitrotScan on corrupt   │  │ Versions[] on multiPut  │  │ simple addPartial()     │
     └──────────────┬──────────┘  └───────────┬─────────────┘  └────────────┬────────────┘
                    │                         │                             │
                    ▼                         ▼                             ▼
     ┌─────────────────────────────────────────────────────────────────────────────────┐
     │ globalMRFState.addPartialOp(PartialOperation{                                   │
     │   Bucket, Object, VersionID, Versions[], SetIndex, PoolIndex,                   │
     │   Queued: time.Now(), BitrotScan: (scanMode == HealDeepScan),                   │
     │ })                                                                              │
     │                                                                                 │
     │ opCh: chan PartialOperation  (buffered, cap = mrfOpsQueueSize = 100_000)        │
     │ Non-blocking send; drops on atomic closing flag.                                │
     └──────────────────────────────┬──────────────────────────────────────────────────┘
                                    │
                                    ▼ (with 1-second delay after Queued)
     ┌─────────────────────────────────────────────────────────────────────────────────┐
     │ mrf.healRoutine (cmd/mrf.go:210+)                                               │
     │   for op := range opCh:                                                         │
     │     if op.Bucket in {.metacache, tmp, multipart, tmp-old} → skip                │
     │     healSleeper.Sleep(...)    ← rate-limited by IOCount/Sleep                   │
     │     if op.Object == ""        → HealBucket(op.Bucket, opts)                     │
     │     else if len(op.Versions) > 0 → HealObject per version                       │
     │     else                       → HealObject(op.Bucket, op.Object, op.VersionID) │
     │                                                                                 │
     │ On shutdown → EncodeMsg to <minioMetaBucket>/.heal/mrf/list.bin                 │
     │ On startup → DecodeMsg and re-enqueue                                           │
     └─────────────────────────────────────────────────────────────────────────────────┘
```

MRF guarantees **at-least-once** eventual healing: if a partial op is dropped (queue full + closing) or the process crashes mid-consumption, the background scanner at `healObjectSelectProb = 1024` will re-encounter the object and heal it via `HealObject` directly.

---


## 6. Q1 — Healing Decision Logic Under Mixed State

> **The question:** Given a cluster where some drives have valid data, some have corrupted data, and some have nothing — how does MinIO decide whether to reconstruct, leave degraded, or purge?

### 6.1 Direct Answer

MinIO's healing subsystem walks a **five-stage deterministic cascade** rooted in `healObject` (`cmd/erasure-healing.go:258-657`). Every decision is a pure function of the current on-disk state; there is no history, no probability, no operator hint. The five stages produce exactly one of five terminal outcomes (enumerated in [§4 Executive Summary](#4-executive-summary)).

The decision logic is deliberately **data-preserving by default**: when the healer encounters ambiguity (e.g., any drive returning a non-actionable error), it declines to purge and reports `errErasureReadQuorum` instead. Destructive action (the `deleteIfDangling` path) requires the ambiguity to be resolved by a *clear* majority of "not found" or a *clear* beyond-parity threshold.

### 6.2 The Five-Stage Cascade

#### Stage 1 — Can we establish read quorum? (`cmd/erasure-metadata.go:531`)

```go
func objectQuorumFromMeta(ctx, partsMetaData []FileInfo, errs []error,
                          defaultParityCount int) (readQ, writeQ int, err error) {
    latestFileInfo, err := getLatestFileInfo(ctx, partsMetaData, defaultParityCount, errs)
    if err != nil { return 0, 0, err }    // ← cascade falls through to deleteIfDangling
    parityBlocks := commonParity(parities, defaultParityCount)
    if parityBlocks < 0 { return 0, 0, errErasureReadQuorum }
    dataBlocks := len(partsMetaData) - parityBlocks
    writeQuorum := dataBlocks
    if dataBlocks == parityBlocks { writeQuorum++ }
    return dataBlocks, writeQuorum, nil
}
```

`commonParity` picks the parity count that appears on a majority of drives; if no majority exists, it returns `-1`, which propagates as `errErasureReadQuorum`. When `objectQuorumFromMeta` returns an error, `healObject` immediately delegates to `deleteIfDangling` at lines 309-324.

#### Stage 2 — Identify the canonical latest version (`cmd/erasure-healing-common.go:219-288`)

```go
func listOnlineDisks(disks []StorageAPI, partsMetadata []FileInfo,
                     errs []error, quorum int) ([]StorageAPI, time.Time, string) {
    modTimes := listObjectModtimes(partsMetadata, errs)
    modTime, _ := commonTimeAndOccurrence(modTimes, quorum)
    etag := commonETag(listObjectETags(partsMetadata, errs), quorum)
    onlineDisks := make([]StorageAPI, len(disks))
    for i, m := range modTimes {
        if m.Equal(modTime) && errs[i] == nil { onlineDisks[i] = disks[i] }
    }
    return onlineDisks, modTime, etag
}
```

The healer uses **modTime quorum first, ETag quorum second**. This lets the healer survive clock-skew scenarios where modTimes diverge but the content is identical — the ETag override at `erasure-healing.go:429-433` reuses the same signal to allow reconstruction even when the disk count is otherwise below threshold.

#### Stage 3 — Verify data integrity per disk (`cmd/erasure-healing-common.go:291-459`)

```go
func disksWithAllParts(ctx, onlineDisks []StorageAPI, partsMetadata []FileInfo,
                       errs []error, latestMeta FileInfo, filterDisksByETag bool,
                       bucket, object string, scanMode madmin.HealScanMode) (
                       available []StorageAPI, byDisk map[int][]int, byPart map[int][]int) {
    for i, onlineDisk := range onlineDisks {
        if shouldSkipByModOrETag(...) { continue }
        var partErrs []int
        if isInlineData {
            partErrs = verifyInlineWithBitrot(...)
        } else if scanMode == madmin.HealDeepScan {
            partErrs = onlineDisk.VerifyFile(...)       // ← full re-checksum via HighwayHash
        } else {
            partErrs = onlineDisk.CheckParts(...)       // ← size-only verification
        }
        byDisk[i] = partErrs
        for p, e := range partErrs { byPart[p] = append(byPart[p], e) }
        if allGood(partErrs) { available[i] = onlineDisk }
    }
    return
}
```

The difference between `CheckParts` and `VerifyFile` is the single most important operational subtlety: `CheckParts` only asserts `len(on-disk bytes) == fi.Parts[i].Size`. Silent bitrot (same-length, different content) slips past; only `VerifyFile` reads every byte through the HighwayHash stream and catches it.

#### Stage 4 — Classify every disk (`cmd/erasure-healing.go:373-405`)

For each disk index `i`:

```go
yes, reason := shouldHealObjectOnDisk(errs[i], dataErrsByDisk[i], partsMetadata[i], latestMeta)
if yes { outDatedDisks[i] = storageDisks[i]; disksToHealCount++ }

driveState := madmin.DriveStateCorrupt   // default
switch {
case reason == nil:               driveState = madmin.DriveStateOk
case IsErr(reason, errDiskNotFound): driveState = madmin.DriveStateOffline
case IsErr(reason, errFileNotFound, errFileVersionNotFound, errVolumeNotFound,
                   errPartMissingOrCorrupt, errOutdatedXLMeta, errLegacyXLMeta):
    driveState = madmin.DriveStateMissing
}
result.Before.Drives = append(result.Before.Drives, madmin.HealDriveInfo{
    Endpoint: storageEndpoints[i].String(), State: driveState,
})
result.After.Drives = append(result.After.Drives, /* same HealDriveInfo */)
```

`result.After.Drives` is initialized to the same values as `result.Before.Drives`. Only drives that are successfully reconstructed get their `After.State` updated to `DriveStateOk` at line 651. If a drive was `Offline` before the heal and remains unreachable, its `After.State` stays `"offline"`.

#### Stage 5 — Apply the `cannotHeal` threshold (`cmd/erasure-healing.go:428-456`)

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted &&
              disksToHealCount > latestMeta.Erasure.ParityBlocks
if cannotHeal && quorumETag != "" {
    // Even though too many disks need heal, all readable xl.metas agree on ETag.
    cannotHeal = false
}
if cannotHeal {
    m, err := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs,
                                  dataErrsByPart, ObjectOptions{VersionID: versionID})
    errs = make([]error, len(errs))
    if err == nil {
        err = toObjectErr(errFileNotFound, bucket, object)
        if versionID != "" { err = toObjectErr(errFileVersionNotFound, bucket, object, versionID) }
        return er.defaultHealResult(m, storageDisks, storageEndpoints, errs,
                                    bucket, object, versionID), err
    }
    for i := range errs { errs[i] = err }
    return er.defaultHealResult(...), err
}
// Otherwise: proceed to Reed-Solomon reconstruction.
```

For EC(2,2), `cannotHeal` fires whenever `disksToHealCount > 2`. The ETag-override escape hatch at line 429 is crucial for recovery from "3 disks stale but consistent" states — typical after a longer-than-expected network partition where modTimes skew but content agrees.

### 6.3 The Dangling Sub-Cascade (Inside `deleteIfDangling`)

`deleteIfDangling` at `cmd/erasure-object.go:482-563` is the gateway to permanent data removal. It calls `isObjectDangling` first:

```go
func (er erasureObjects) deleteIfDangling(ctx, bucket, object string, metaArr []FileInfo,
                                          errs []error, dataErrsByPart map[int][]int,
                                          opts ObjectOptions) (FileInfo, error) {
    var err error
    m, ok := er.isObjectDangling(metaArr, errs, dataErrsByPart)
    if !ok {
        err = errErasureReadQuorum        // ← LEAVE ALONE path
        if !m.IsValid() { return m, err }
    }
    _, file, line, cok := runtime.Caller(1)
    tags := map[string]string{
        "set": strconv.Itoa(er.setIndex), "pool": strconv.Itoa(er.poolIndex),
        "merrs": joinErrs(errs), "derrs": joinPartErrs(dataErrsByPart),
        "sz": strconv.FormatInt(m.Size, 10), "mt": m.ModTime.String(),
        "d:p": fmt.Sprintf("%d:%d", m.Erasure.DataBlocks, m.Erasure.ParityBlocks),
        "offline": strconv.Itoa(countOffline(errs)),
    }
    if cok { tags["caller"] = fmt.Sprintf("%s:%d", file, line) }
    g := errgroup.WithNErrs(len(er.getDisks()))
    for i, disk := range er.getDisks() { /* disk.DeleteVersion(...) */ }
    auditDanglingObjectDeletion(ctx, bucket, object, opts.VersionID, tags)
    return m, err
}
```

Two exits:

- `isObjectDangling == true` → `DeleteVersion` on all disks + `auditDanglingObjectDeletion` + return `(m, nil)`. `healObject` sees `err == nil` and returns `errFileNotFound` to the caller at line 446.
- `isObjectDangling == false` → return `(m, errErasureReadQuorum)` with no mutation. `healObject` propagates this upward and the client receives `InsufficientReadQuorum`.

### 6.4 Putting It All Together — The Mixed-State Example

Consider an EC(2,2) object where after a crash we observe:

- Disk 1: `errDiskNotFound` (non-actionable)
- Disk 2: valid xl.meta, part OK
- Disk 3: `errFileNotFound` (not-found)
- Disk 4: valid xl.meta, part corrupt (`checkPartFileCorrupt` during deep scan)

Cascade:

1. **Stage 1** — quorum? Two valid metas (disk 2 and disk 4) → `dataBlocks=2, parityBlocks=2` established. Pass.
2. **Stage 2** — `listOnlineDisks` returns `{disk2, disk4}`, modTime = disk2's modTime.
3. **Stage 3** — `disksWithAllParts` verifies. Disk 2 passes, Disk 4 fails part-check. `dataErrsByDisk[4] = [checkPartFileCorrupt]`.
4. **Stage 4** — classify:
   - Disk 1: `errDiskNotFound` → `DriveStateOffline`, should heal.
   - Disk 2: no error → `DriveStateOk`, no heal needed.
   - Disk 3: `errFileNotFound` → `DriveStateMissing`, should heal.
   - Disk 4: `errPartMissingOrCorrupt` → `DriveStateMissing` (not "corrupt"!), should heal.
   - `disksToHealCount = 3`.
5. **Stage 5** — `cannotHeal = (3 > 2) = true`. If `quorumETag != ""`, reconstruct. Else, enter `deleteIfDangling`.
6. **Dangling check** — `nonActionableMetaErrs > 0` (disk 1 is `errDiskNotFound`). **Criterion #3 fires.** `isObjectDangling` returns `false`. `deleteIfDangling` returns `errErasureReadQuorum`.

**Final outcome: Leave alone.** The operator must bring disk 1 back online (or replace it) before MinIO will make a destructive decision.

This example crystallizes the **data-preservation bias**: MinIO refuses to purge an object when even one drive is merely unreachable, because "unreachable" might mean "has the data we need." Only unambiguous `not-found` patterns across enough drives trigger destructive action.

---


## 7. Q2 — Reconstruct vs. Stay-Deleted/Degraded: Exact Code Paths

> **The question:** Does MinIO always reconstruct from valid shards, or are there situations where it deliberately decides the object should stay deleted or degraded?

### 7.1 Direct Answer

**No, MinIO does not always reconstruct.** The healer has exactly three terminal outcomes, selected by the `cannotHeal` flag at `cmd/erasure-healing.go:428` and the subsequent dangling check:

| Outcome | Trigger condition | Source lines | Terminal action |
|---|---|---|---|
| **Reconstruct** | `disksToHealCount == 0` OR `!cannotHeal` | `erasure-healing.go:417-420`, `:458-657` | Reed-Solomon decode + write shards + `RenameData` |
| **Dangling purge** | `cannotHeal && isObjectDangling == true` | `erasure-healing.go:435-456` → `erasure-object.go:482-563` → `erasure-healing.go:968-1036` | `DeleteVersion` on **every** disk + audit |
| **Leave alone (degraded)** | `cannotHeal && isObjectDangling == false` | `erasure-healing.go:435-456` → `erasure-object.go:487` | Return `errErasureReadQuorum`, nothing on disk changes |

An additional variant exists: **reconstruct under ETag override**, where `cannotHeal` would have fired but the consistent ETag consensus at line 429 forces the reconstruct path instead.

### 7.2 Outcome A — Reconstruct: Full Code Path

Entry point: `healObject` at `cmd/erasure-healing.go:258`.

```
healObject (erasure-healing.go:258-293)
 ├── acquire namespace lock (unless opts.NoLock)                            # :281-293
 ├── readAllFileInfo(storageDisks, ...)                                     # :295-305
 ├── objectQuorumFromMeta(partsMetadata, errs, defaultParityCount)          # :307
 ├── listOnlineDisks(...) → (online, quorumModTime, quorumETag)             # :331
 ├── pickValidFileInfo → latestMeta                                         # :335
 ├── disksWithAllParts(...) → (available, dataErrsByDisk, dataErrsByPart)   # :352
 ├── [for each disk] shouldHealObjectOnDisk → outDatedDisks[i], state       # :373-405
 ├── if disksToHealCount == 0 → return (no-op)                              # :417-420
 ├── if opts.DryRun → return (classification only, no writes)               # :424-426
 ├── cannotHeal = disksToHealCount > latestMeta.Erasure.ParityBlocks        # :428
 ├── (cannotHeal && quorumETag != "") → cannotHeal = false                  # :429-433
 └── !cannotHeal path (reconstruct):
     ├── er.renameAll(ctx, minioMetaTmpBucket, tmpID)                       # :458-465
     ├── for each healable part:                                            # :480-650
     │   ├── newBitrotReader(available disks)
     │   ├── newBitrotWriter(outDatedDisks)                                 # writeQuorum=1
     │   ├── erasure.Heal(ctx, writers, readers, size, prefer)              # erasure-decode.go:317
     │   └── if err != nil → "all drives had write errors" (:614-616)
     ├── RenameData(outDatedDisks, tmpID → bucket/object/dataDir)           # after loop
     ├── for each outDatedDisks[i] != nil → result.After.Drives[i].State = DriveStateOk  # :651
     ├── checkAbandonedParts (erasure-healing.go:659+)                      # orphan data-dir sweep
     └── return result (with err = nil on success)
```

Critical details:

- **Bitrot reader/writer pairs**: every part is re-encoded through a `BitrotWriter`, which writes HighwayHash checksums alongside the data. This guarantees the reconstructed shards are self-verifying.
- **`writeQuorum = 1`** on the healed shards means the loop tolerates individual outdated-disk failures; it only aborts if *all* writes fail (`"all drives had write errors"` at line 614-616).
- **`SetHealing()` flag** — during reconstruction the healer writes a transient metadata marker `x-minio-internal-healing: true` to the xl.meta being staged, so concurrent reads can skip these shards until the final `RenameData` commit.
- **`RenameData`** is the atomic commit: it moves the reconstructed part files and the updated xl.meta from the tmp bucket to the canonical data-dir in a single storage operation.
- **Line 651 is the ONLY mutation point** of `result.After.Drives[i].State`. If reconstruction of any part fails for a disk, that disk keeps its pre-heal state in `After`.

### 7.3 Outcome B — Dangling Purge: Full Code Path

Entry point: `cannotHeal == true` branch at `cmd/erasure-healing.go:435`.

```
healObject (erasure-healing.go:258-293)
 └── cannotHeal == true (line 428):
     ├── deleteIfDangling (erasure-object.go:482-563)
     │   ├── isObjectDangling (erasure-healing.go:968-1036)
     │   │   ├── danglingMetaErrsCount(errs) → (notFoundMeta, nonActionableMeta)
     │   │   ├── danglingPartErrsCount(dataErrsByPart) → (notFoundParts, nonActionableParts)
     │   │   ├── validMeta = first valid FileInfo
     │   │   ├── Criterion #1 (no valid meta): notFoundPartsErrs > dataBlocks → dangling
     │   │   ├── Criterion #2 (safety valve):  any non-actionable error  → NOT dangling
     │   │   ├── Criterion #3 (delete marker): notFoundMetaErrs > dataBlocks  → dangling
     │   │   ├── Criterion #4 (primary):       notFoundMetaErrs > parityBlocks → dangling
     │   │   ├── Criterion #5 (non-remote):    notFoundPartsErrs > parityBlocks → dangling
     │   │   └── default                     → NOT dangling
     │   ├── (if ok == false) return (m, errErasureReadQuorum)  ── LEAVE ALONE
     │   ├── build forensic tags (set, pool, merrs, derrs, sz, mt, d:p, offline, caller)
     │   ├── _, file, line, _ := runtime.Caller(1)               # attach caller
     │   ├── errgroup.WithNErrs(len(er.getDisks()))              # fan-out
     │   │   for each disk: disk.DeleteVersion(ctx, bucket, object, fi, false, opts)
     │   ├── for each disk result: tags["ddisk-<i>"] = result-string
     │   ├── auditDanglingObjectDeletion(ctx, bucket, object, versionID, tags)
     │   └── return (m, nil)
     ├── (deleteIfDangling returned nil) err = errFileNotFound / errFileVersionNotFound  # :443-449
     └── return defaultHealResult(m, storageDisks, endpoints, errs, ...), err
```

Observations:

- **The destructive action is unconditional across all disks.** `disk.DeleteVersion(..., false, opts)` removes the xl.meta and the data-dir on every disk in the erasure set, not just the ones that were already missing.
- **Orphan data-dir cleanup** is best-effort: if some drives had only the data-dir (not xl.meta), `DeleteVersion` removes them. If only non-inline data bytes exist without xl.meta, MinIO marks them as orphaned via `checkAbandonedParts` (`cmd/erasure-healing.go:659+`) and the data-scanner eventually purges them during its next sweep.
- **Audit log entry** — `auditDanglingObjectDeletion` is the forensic trail. See [§9.2](#92-deletedanglingobject-audit-entries) for the full tag taxonomy.

### 7.4 Outcome C — Leave Alone: Full Code Path

Entry point: `cannotHeal == true && isObjectDangling == false` — same as Outcome B, but the `ok == false` branch inside `deleteIfDangling`:

```go
func (er erasureObjects) deleteIfDangling(...) (FileInfo, error) {
    var err error
    m, ok := er.isObjectDangling(metaArr, errs, dataErrsByPart)
    if !ok {
        err = errErasureReadQuorum                    // ← LEAVE ALONE
        if !m.IsValid() { return m, err }
        // If meta is valid, still record the decision in audit even if not purging.
    }
    ...
}
```

Which of the six `isObjectDangling` criteria triggers the "leave alone" is important for operators:

| Triggering criterion | Root cause |
|---|---|
| Criterion #2 (non-actionable errors) | A drive returned `errDiskNotFound`, a permission denied, or an unclassified I/O error — MinIO refuses to declare the object lost when a drive might simply be unreachable. |
| Criterion #4 / #5 declined | `notFoundMetaErrs <= parityBlocks` — within tolerance, so not a dangling case despite `cannotHeal`. Usually indicates the object is reconstructable and `cannotHeal` fired from a transient disk count inconsistency. |
| Default (meta valid, no other criterion) | All meta errors are "not found" but below the threshold; reconstruction failed earlier in the flow. |

The client-visible symptom is an `InsufficientReadQuorum` / `errErasureReadQuorum` error on the heal API response. The object data on disk is **not modified** — the cluster state is still as the operator observed it pre-heal. This is the correct behavior for ambiguous scenarios: fix the drives, retry the heal.

### 7.5 Outcome D — ETag Override Reconstruct (The 4th Variant)

Revisit `cmd/erasure-healing.go:429-433`:

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted &&
              disksToHealCount > latestMeta.Erasure.ParityBlocks
if cannotHeal && quorumETag != "" {
    // This is an object that is supposed to be removed by the dangling code
    // but we noticed that ETag is the same for all objects, let's give it a shot.
    cannotHeal = false
}
```

The preconditions for the override:

- `quorumETag != ""` — `listOnlineDisks` found an ETag that appears on enough disks to meet `readQuorum`.
- `disksToHealCount > parityBlocks` — the drives need healing, but their xl.metas agreed on the content hash.

This is the "3 disks stale but consistent" case — a network partition followed by a reconnect, where modTimes skewed but content remained identical. Without the override, MinIO would purge objects that are demonstrably intact. With the override, MinIO proceeds to reconstruct with the confidence that the content is canonical.

### 7.6 Forensic Traceability Through `runtime.Caller(1)`

At `cmd/erasure-object.go:526-529` the healer captures the Go call site:

```go
_, file, line, cok := runtime.Caller(1)
if cok {
    tags["caller"] = fmt.Sprintf("%s:%d", file, line)
}
```

This attaches the Go source file and line number of the function that invoked `deleteIfDangling`. Observed call sites include:

- `cmd/erasure-healing.go:449` — dangling-purge triggered from `healObject` when `cannotHeal == true`.
- `cmd/erasure-healing.go:319` — dangling-purge triggered from `healObject` when `objectQuorumFromMeta` failed (Stage 1).
- `cmd/erasure-object.go:2400+` — dangling-purge triggered from `DeleteObject` paths (rare; occurs during complex multi-version operations).

An operator grepping the audit log for `caller=cmd/erasure-healing.go:449` finds every heal-triggered purge; grepping for `caller=cmd/erasure-healing.go:319` finds every metadata-quorum-failure purge. The forensic resolution is at the source-code line, not just the function.

---


## 8. Q3 — What `HealResultItem` Reveals About the Decision

> **The question:** Is there a status indicator, a before/after diff, or a JSON payload field that shows what the healer actually decided?

### 8.1 Direct Answer

Yes. `HealResultItem` (from `github.com/minio/madmin-go/v3/heal-commands.go`) is the wire-level signal for every heal outcome. It carries **five distinct signals** that together expose the decision:

1. **Per-drive `Before.State` → `After.State` transitions** that show which drives were healed, which remained unreachable, and which were never broken.
2. **Aggregate color** derived from `GetOnlineCounts`, `GetMissingCounts`, `GetCorruptedCounts`, `GetOfflineCounts`.
3. **`drives == null`** as an unambiguous dangling-purge signature.
4. **`DetailedMsg` field** populated with the underlying Go error string when the heal short-circuited.
5. **`ParityBlocks` / `DataBlocks` / `ObjectSize`** echoing the computed erasure configuration (or zeros when the meta could not be loaded).

### 8.2 The JSON Shape (Assembled at `cmd/erasure-healing.go:258-283` and `:373-405`)

```json
{
  "resultId": 1,
  "type": "object",
  "bucket": "heal-test-bucket",
  "object": "heal-test-obj1",
  "versionId": "",
  "detail": "",
  "parityBlocks": 2,
  "dataBlocks": 2,
  "diskCount": 4,
  "setCount": 1,
  "objectSize": 32,
  "before": {
    "drives": [
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk1", "state": "missing"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk2", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk3", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk4", "state": "ok"}
    ]
  },
  "after": {
    "drives": [
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk1", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk2", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk3", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/minio-heal-test/disk4", "state": "ok"}
    ]
  }
}
```

The struct is populated in three phases inside `healObject`:

- **Lines 258-283** — `result` is initialized with `{Type: HealItemObject, Bucket, Object, VersionID, DiskCount}`.
- **Lines 373-405** — for each disk, `Before.Drives` and `After.Drives` are appended with the same `HealDriveInfo` (state classification per Stage 4).
- **Line 651** — on successful reconstruction for outdated disk `i`, `result.After.Drives[i].State = DriveStateOk`.

### 8.3 Signal 1 — Per-Drive State Transitions

The nine `DriveState*` constants are defined in `madmin-go/v3/heal-commands.go` and are wire-level strings:

| Constant | JSON string | Meaning in `Before` | Meaning in `After` |
|---|---|---|---|
| `DriveStateOk` | `"ok"` | Drive had valid, latest data | Drive has valid data (post-heal) |
| `DriveStateOffline` | `"offline"` | Drive returned `errDiskNotFound` | Drive remained unreachable during heal |
| `DriveStateMissing` | `"missing"` | File not found, outdated meta, or missing/corrupt parts | Drive was healed if state moved to `"ok"` |
| `DriveStateCorrupt` | `"corrupt"` | Unclassified error (catch-all in `erasure-healing.go:393`) | Typically stays `"corrupt"` or becomes `"ok"` if healable |
| `DriveStatePermissionDenied` | `"permission-denied"` | Disk returned EACCES | Unchanged unless permissions fixed mid-heal |
| `DriveStateFaulty` | `"faulty"` | Disk marked defective | Unchanged |
| `DriveStateRootMount` | `"root-mount"` | Disk on root filesystem (config rejection) | Unchanged |
| `DriveStateUnknown` | `"unknown"` | Enumeration gap | Unchanged |
| `DriveStateUnformatted` | `"unformatted"` | Disk lacks `format.json` | `"ok"` after `healDiskFormat` succeeds |

The **transition table** is how the operator reads the decision:

| Before state | After state | Interpretation |
|---|---|---|
| `"missing"` | `"ok"` | **Healed successfully** — Reed-Solomon reconstruction succeeded, `RenameData` committed. |
| `"corrupt"` | `"ok"` | **Healed** — typically after `HealDeepScan` detected bitrot. |
| `"missing"` | `"missing"` | Heal attempted but failed. Check `detail` field. |
| `"offline"` | `"offline"` | Drive unreachable throughout heal. No action taken. |
| `"ok"` | `"ok"` | Drive was never broken. |

### 8.4 Signal 2 — Aggregate Color (via `GetOnlineCounts`, `GetMissingCounts`, etc.)

The `HealResultItem` has five convenience methods defined in `madmin-go`:

```go
func (hri HealResultItem) GetOnlineCounts() (b, a int)     // count DriveStateOk in Before, After
func (hri HealResultItem) GetOfflineCounts() (b, a int)    // count DriveStateOffline
func (hri HealResultItem) GetCorruptedCounts() (b, a int)  // count DriveStateCorrupt
func (hri HealResultItem) GetMissingCounts() (b, a int)    // count DriveStateMissing
```

These are consumed inside MinIO at `cmd/erasure-healing.go:236-244`:

```go
func auditHealObject(ctx, bucket, object, versionID, err error, ...) {
    ...
    if b, a := healResult.GetCorruptedCounts(); b > 0 && b == a {
        errStr = fmt.Sprintf("unable to heal %d corrupted blocks on drives", a)
    }
    if b, a := healResult.GetMissingCounts(); b > 0 && b == a {
        errStr = fmt.Sprintf("unable to heal %d missing blocks on drives", a)
    }
    ...
}
```

The pattern `before_count > 0 && before_count == after_count` signals a **failed heal** for that category — the count did not decrease. Operators can write dashboards on these counts to detect fleet-wide healing failures.

The mapping from aggregate counts to the `mc admin heal` color scheme:

| `GetOnlineCounts().after` | `GetMissingCounts().after` + `GetCorruptedCounts().after` | Display color |
|---|---|---|
| `== DiskCount` | `== 0` | 🟢 green (healthy) |
| `>= DataBlocks` | `> 0` | 🟡 yellow (degraded but accessible) |
| `< DataBlocks` | `> 0` | 🔴 red (unavailable) |

### 8.5 Signal 3 — `drives == null` as Dangling-Purge Signature

When `deleteIfDangling` returns successfully, `healObject` calls `defaultHealResult` at line 451, which constructs a **minimal** `HealResultItem`. Inspection of the code shows that when the object was purged:

- `parityBlocks = 0` (no erasure config available since the object no longer exists).
- `dataBlocks = 0`.
- `objectSize = 0`.
- `before.drives` and `after.drives` are empty slices (serialize as `null` in JSON when the slice is `nil`).

Example JSON (canonical, emitted by `mc admin heal --json` for Scenario C):

```json
{
  "resultId": 1,
  "type": "object",
  "bucket": "heal-test-bucket",
  "object": "heal-test-obj3",
  "versionId": "",
  "detail": "",
  "parityBlocks": 0,
  "dataBlocks": 0,
  "diskCount": 4,
  "setCount": 1,
  "objectSize": 0,
  "before": {"drives": null},
  "after":  {"drives": null}
}
```

When the `mc` client parses this response via `madmin-go`, its constructor rejects `parityBlocks == 0` with `"Invalid parity shard count"` (madmin-go's internal validation of erasure configuration). The operator sees this error string at the CLI — a signal that the object was purged (dangling-delete) rather than healed.

### 8.6 Signal 4 — `DetailedMsg` Field Taxonomy

The `detail` field propagates the internal Go error message. Observed patterns:

| `detail` value | Source line | Decision implication |
|---|---|---|
| `""` (empty) | All success paths | Heal succeeded or no action needed. |
| `"Read failed. Insufficient number of drives online"` | `erasure-errors.go:24` via `errErasureReadQuorum` | **Leave alone** — ambiguity prevented action. |
| `"Object is not found"` (as `ErrFileNotFound`) | `erasure-healing.go:447` via `toObjectErr(errFileNotFound, ...)` | **Dangling purge executed** or object was never present. |
| `"all drives had write errors"` | `erasure-healing.go:614-616` | Reconstruct attempted but **writes failed** — operator must inspect disks. |
| `"Object version does not exist"` (`ErrFileVersionNotFound`) | `erasure-healing.go:449` | Same as ErrFileNotFound, but for versioned objects. |
| `"Invalid parity shard count"` | madmin-go client-side | Interpretation: `parityBlocks == 0`, which means **dangling purge** (object gone from cluster). |

### 8.7 Signal 5 — Aggregate Counts Emitted in Audit Trail

The audit helper at `cmd/erasure-healing.go:236-244` pulls all four count pairs into the audit event:

```go
if b, a := healResult.GetCorruptedCounts(); b > 0 && b == a {
    ev.Error = fmt.Sprintf("unable to heal %d corrupted blocks on drives", a)
}
if b, a := healResult.GetMissingCounts(); b > 0 && b == a {
    ev.Error = fmt.Sprintf("unable to heal %d missing blocks on drives", a)
}
ev.Tags = map[string]interface{}{
    "healObject": fmt.Sprintf("{Name:%q, Pool:%d, Set:%d}", bucket+"/"+object,
                              er.poolIndex, er.setIndex),
}
```

The audit event surfaces the aggregate drive-state deltas with the original bucket/object/pool/set context — this is the primary data source for fleet-level healing dashboards.

### 8.8 Streaming Summary Fields (For Bulk Heal Sessions)

When `mc admin heal --recursive` is used, the admin API emits a **summary** `HealResultItem` with `Type == "bucket"` or a sentinel "session" record. These summary records carry the aggregate counters:

- `scanned_items` — total objects scanned by this heal session.
- `healed_items` — objects that had at least one `State: "missing"→"ok"` transition.
- `heal_size` — bytes touched during healing.

These are populated from `healSequence.healSequenceStatus.{ItemsScanned, ItemsHealed, BytesScanned, BytesHealed}` in `cmd/admin-heal-ops.go:286+`.

### 8.9 What `HealResultItem` Does *Not* Reveal

The JSON response alone cannot tell an operator:

- **Which of the six `isObjectDangling` criteria fired.** The JSON only says "purged" via `drives == null`. To identify the exact criterion, consult the `DeleteDanglingObject` audit log entry (see [§9.2](#92-deletedanglingobject-audit-entries)).
- **Whether the ETag override at `erasure-healing.go:429-433` saved the object.** The JSON looks identical to a normal reconstruct. Only the internal log `deleting dangling object` *not* appearing despite `disksToHealCount > parityBlocks` reveals the override fired.
- **Per-part bitrot details.** Bitrot is reported only at the drive-state level (`"corrupt"`); to see which part on which drive, consult the `mc admin trace --call heal.*` output.

For full forensics, the operator must correlate three signals: `HealResultItem` JSON, the audit log entry, and the healing trace events.

---


## 9. Q4 — What Logs Explain

> **The question:** Do the logs explain why MinIO chose to restore the file versus leave it alone?

### 9.1 Direct Answer

**Yes, but opt-in.** MinIO emits **five distinct log surfaces** that together explain healing decisions. Some are enabled by default (internal healing logs), some require configuration (audit webhook), and some are pull-based (trace API).

| Log surface | Source | Default | Coverage |
|---|---|---|---|
| `DeleteDanglingObject` audit event | `auditDanglingObjectDeletion` in `cmd/erasure-object.go:531` | Opt-in (requires `mc admin config set audit_webhook`) | Every dangling purge — includes `caller`, `merrs`, `derrs`, per-disk results |
| Internal healing logs | `healingLogOnceIf` in `cmd/erasure-healing.go:477, :487, :497` | On (logs to server stdout) | Error paths during healing (failed reconstruction, quorum loss, etc.) |
| `mc admin trace --call heal.*` | `healTrace` in `cmd/erasure-healing.go:1089-1115` | On-demand | Real-time per-heal event with `HealResultItem` pointer attached |
| `HealObject` audit event | `auditHealObject` in `cmd/erasure-healing.go:221-255` | Opt-in | Every heal invocation — includes aggregate corrupted/missing counts and tag `healObject: {Name, Pool, Set}` |
| On-disk `healingTracker` | `cmd/background-newdisks-heal-ops.go` | On (per-disk state in `.minio.sys/.healing.bin`) | New-disk and drive-replacement healing progress |

### 9.2 `DeleteDanglingObject` Audit Entries

This is the richest explanation of a destructive decision. Source: `cmd/erasure-object.go:482-563`.

```go
func (er erasureObjects) deleteIfDangling(ctx, bucket, object string,
                                          metaArr []FileInfo, errs []error,
                                          dataErrsByPart map[int][]int,
                                          opts ObjectOptions) (FileInfo, error) {
    var err error
    m, ok := er.isObjectDangling(metaArr, errs, dataErrsByPart)
    if !ok {
        err = errErasureReadQuorum
        if !m.IsValid() { return m, err }
    }
    _, file, line, cok := runtime.Caller(1)
    tags := map[string]string{
        "set":     strconv.Itoa(er.setIndex),
        "pool":    strconv.Itoa(er.poolIndex),
        "merrs":   joinErrs(errs),               // semicolon-joined err per disk
        "derrs":   joinPartErrs(dataErrsByPart), // per-part error map
        "sz":      strconv.FormatInt(m.Size, 10),
        "mt":      m.ModTime.String(),
        "d:p":     fmt.Sprintf("%d:%d", m.Erasure.DataBlocks, m.Erasure.ParityBlocks),
        "offline": strconv.Itoa(countOffline(errs)),
    }
    if !m.IsValid() { tags["invalid"] = "1" }
    if cok { tags["caller"] = fmt.Sprintf("%s:%d", file, line) }
    g := errgroup.WithNErrs(len(er.getDisks()))
    for i, disk := range er.getDisks() {
        i, disk := i, disk
        g.Go(func() error {
            if disk == nil { return errDiskNotFound }
            return disk.DeleteVersion(ctx, bucket, object, FileInfo{...}, false, DeleteOptions{...})
        }, i)
    }
    for i, err := range g.Wait() {
        tags[fmt.Sprintf("ddisk-%d", i)] = errToResultString(err)
    }
    auditDanglingObjectDeletion(ctx, bucket, object, opts.VersionID, tags)
    return m, err
}
```

The resulting audit event carries the following tags:

| Tag | Meaning |
|---|---|
| `set` | Erasure set index (0-based). |
| `pool` | Server pool index (0-based). |
| `merrs` | Joined string of per-disk metadata errors (e.g., `"errFileNotFound; errFileNotFound; nil; errFileNotFound"`). |
| `derrs` | Joined string of per-part data errors (e.g., `"part-0: [4 4 1 4]"` using `checkPart*` integers). |
| `sz` | Object size in bytes (0 if meta invalid). |
| `mt` | Object modtime (Go time string). |
| `d:p` | Data/parity config as `"2:2"` for EC(2,2). |
| `offline` | Number of disks with `errDiskNotFound`. |
| `invalid` | `"1"` if no valid FileInfo existed across any disk. |
| `caller` | Go source file:line that triggered the dangling purge (via `runtime.Caller(1)`). |
| `ddisk-0`, `ddisk-1`, … | Per-disk result of the `DeleteVersion` fan-out. |

This is the **forensic primary source**. An operator grepping the audit log for `action=DeleteDanglingObject` sees every destructive decision, with enough detail to reconstruct the cluster state at the moment of purge.

Enabling requires:

```bash
mc admin config set myminio audit_webhook:primary \
  endpoint="http://log-collector.internal/audit" auth_token="..."
mc admin service restart myminio
```

### 9.3 Internal Healing Logs

`cmd/erasure-healing.go` uses `healingLogOnceIf(ctx, err, "unique-key")` at three sites:

- **Line 477** — "not enough available disks for healing" when `len(available) < dataBlocks`.
- **Line 487** — "not enough outdated disks for healing" when `disksToHealCount == 0` unexpectedly.
- **Line 497** — "reset metadata state for unhealthy drive" when partsMetadata entry was nil'd.

These log to the MinIO server's stdout under the "Console" log class. In production they appear in `journalctl -u minio` (systemd) or the Kubernetes pod log stream.

They are **error-path only** — a successful heal emits nothing on this channel.

### 9.4 `mc admin trace --call heal.*` (Live Trace)

Source: `healTrace` at `cmd/erasure-healing.go:1089-1115`:

```go
func healTrace(funcName healingMetric, startTime time.Time, bucket, object string,
               opts *madmin.HealOpts, err error, result *madmin.HealResultItem) {
    tr := madmin.TraceInfo{
        TraceType: madmin.TraceHealing,
        Time:      startTime,
        NodeName:  globalLocalNodeName,
        FuncName:  "heal." + funcName.String(),
        Duration:  time.Since(startTime),
        Path:      fmt.Sprintf("%s/%s", bucket, object),
        Custom:    map[string]string{
            "dry":  strconv.FormatBool(opts.DryRun),
            "remove":    strconv.FormatBool(opts.Remove),
            "mode":      opts.ScanMode.String(),
            "version-id": versionID,
            "disks":      strconv.Itoa(diskCount),
        },
        HealResult: result,
    }
    if err != nil { tr.Error = err.Error() }
    globalTrace.Publish(tr)
}
```

Consumers subscribe via:

```bash
mc admin trace --call heal.* myminio
```

Each trace event carries the full `HealResultItem` pointer, the heal options, and timing. This is the **lightest-weight** way to observe healing in real time without configuring audit webhooks.

### 9.5 `HealObject` Audit Entries

Source: `auditHealObject` at `cmd/erasure-healing.go:221-255`:

```go
func auditHealObject(ctx context.Context, bucket, object, versionID string,
                     err error, healResult madmin.HealResultItem,
                     poolIndex, setIndex int) {
    if len(logger.AuditTargets()) == 0 { return }
    opts := AuditLogOptions{
        Event:   "HealObject",
        Bucket:  bucket,
        Object:  decodeDirObject(object),
        VersionID: versionID,
        Status:  "success",
    }
    if err != nil { opts.Error = err.Error(); opts.Status = "failure" }
    if b, a := healResult.GetCorruptedCounts(); b > 0 && b == a {
        opts.Error = fmt.Sprintf("unable to heal %d corrupted blocks on drives", a)
    }
    if b, a := healResult.GetMissingCounts(); b > 0 && b == a {
        opts.Error = fmt.Sprintf("unable to heal %d missing blocks on drives", a)
    }
    opts.Tags = map[string]interface{}{
        "healObject": fmt.Sprintf("{Name:%q, Pool:%d, Set:%d}",
                                   bucket+"/"+object, poolIndex, setIndex),
    }
    auditLogInternal(ctx, opts)
}
```

This is the **per-heal-invocation** audit trail. It fires for every object that goes through `HealObject`, even successful ones (with `Status: "success"`). The aggregate count strings ("unable to heal N corrupted blocks on drives") encode whether healing made progress.

### 9.6 On-Disk `healingTracker`

Source: `cmd/background-newdisks-heal-ops.go`. When a new disk joins (or replaces a failed one), MinIO creates `.minio.sys/.healing.bin` with a `healingTracker` struct tracking:

- Disk UUID and endpoint.
- Start time, last update time.
- Objects scanned, objects healed, bytes healed.
- Current bucket being processed.
- Pool and set index.

This tracker survives restarts and is consulted by `mc admin heal` status queries via `getLocalBackgroundHealStatus` in `cmd/global-heal.go`. When the tracker reports `Finished = true`, the disk is considered fully synced.

### 9.7 Summary — Log-Based Decision Explanation

MinIO explains every decision, but the operator must opt in:

1. **For forensic dangling-delete audit** — enable `audit_webhook`, grep for `action=DeleteDanglingObject`, inspect `caller`, `merrs`, `derrs`.
2. **For error paths and failed heals** — follow MinIO stdout / `journalctl`; look for `healingLogOnceIf` messages.
3. **For real-time observability** — `mc admin trace --call heal.*` gives live `HealResultItem` dumps.
4. **For fleet-wide healing dashboards** — `HealObject` audit events provide per-invocation aggregate counts.
5. **For new-disk onboarding status** — `mc admin heal --json` (or the heal status API) reports `healingTracker` progress.

The decision rationale is always available; it just lives in the right log surface for the question being asked.

---


## 10. Q5 — Minimum Valid Shards, Failure Errors, Boundary

> **The question:** How many valid shards are actually needed for MinIO to heal, and what error appears when it cannot recover?

### 10.1 Direct Answer

For an EC(D, P) erasure set with N = D + P drives:

- **Minimum valid shards for reconstruction:** `D` (data blocks). Reed-Solomon requires at least `k = dataBlocks` intact shards to reconstruct the remaining `P` shards.
- **Maximum tolerable drive losses:** `P` (parity blocks). Beyond `P` missing/corrupt drives, the object enters the `cannotHeal` path.
- **Boundary for `cannotHeal`:** fires when `disksToHealCount > ParityBlocks` (`cmd/erasure-healing.go:428`).

For the 4-disk EC(2,2) configuration used in all scenarios in [§12](#12-six-scenarios-afwith-json-evidence):

- Minimum valid shards = **2**.
- Tolerate up to **2** missing/corrupt drives.
- `cannotHeal` at **3 or more** drives needing heal.

**Two-layer check** — the healer enforces the minimum in two places:

1. **Metadata quorum layer** (`objectQuorumFromMeta` at `cmd/erasure-metadata.go:531`) — at least `N/2` drives must have readable, consistent xl.meta. Failure returns `errErasureReadQuorum`.
2. **Heal decision layer** (`cannotHeal` check at `cmd/erasure-healing.go:428`) — even after metadata quorum is established, if more than `ParityBlocks` drives need healing, reconstruction is declined.

### 10.2 Reed-Solomon Floor

Source: `cmd/erasure-decode.go:317-360`:

```go
func (e Erasure) Heal(ctx context.Context, writers []io.Writer, readers []io.ReaderAt,
                      totalLength int64, prefer []bool) error {
    ...
    reader := newParallelReader(readers, e, 0, totalLength)
    if prefer != nil { reader.preferReaders(prefer) }
    startBlock := int64(0)
    endBlock := totalLength / e.blockSize
    if totalLength % e.blockSize > 0 { endBlock++ }
    for block := startBlock; block < endBlock; block++ {
        bufs, err := reader.Read(bufs)
        if len(bufs) > 0 {
            if err != nil && !IsQuorumDecodeError(err) { return err }
            if err = e.DecodeDataAndParityBlocks(ctx, bufs); err != nil { return err }
            for i, buf := range bufs {
                if writers[i] == nil { continue }
                if _, err := writers[i].Write(buf); err != nil { return err }
            }
        }
    }
    return nil
}
```

`DecodeDataAndParityBlocks` delegates to `github.com/klauspost/reedsolomon v1.12.4`. The library's `Reconstruct` method requires **at least `k` intact shards** (where `k = dataBlocks`). Fewer shards → `reedsolomon.ErrTooFewShards` → surfaces as the top-level error returned by `Erasure.Heal`.

The `parallelReader` issues simultaneous `ReadAt` calls to every `reader` in the slice; `prefer` biases toward the "available" disks identified in Stage 3 of the cascade ([§6.2](#stage-3--verify-data-integrity-per-disk-cmderasure-healing-commongo291-459)). Non-preferred readers are tried only if the preferred set doesn't yield enough shards for the current block.

### 10.3 Failure-Error Taxonomy

Four distinct errors can surface when healing cannot recover an object:

#### Error 1 — `errErasureReadQuorum`

Source: `cmd/erasure-errors.go:24`:

```go
var errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")
```

When it appears:

- Metadata quorum failure at Stage 1 (`objectQuorumFromMeta` returns `errErasureReadQuorum` when no common parity configuration exists).
- `isObjectDangling` declines to purge (criterion #2 fires, or criteria #4/#5 declined) in the `cannotHeal` branch — `deleteIfDangling` returns `errErasureReadQuorum` (`cmd/erasure-object.go:487`).

Client-visible as `InsufficientReadQuorum` — the S3-compatible admin heal API translates this to HTTP 500 with the error code.

#### Error 2 — `errFileNotFound` / `errFileVersionNotFound`

Source: `cmd/erasure-healing.go:407-415, 443-449`.

When it appears:

- **All-not-found path** — every disk returned `errFileNotFound`. `isAllNotFound(errs)` is `true`. `healObject` returns `errFileNotFound` at line 414 (or `errFileVersionNotFound` if `versionID != ""`).
- **Dangling-purge completion path** — `deleteIfDangling` purged the object successfully. `healObject` fabricates an `errFileNotFound` at line 446 so the caller knows the object no longer exists.

Client-visible as `NoSuchKey` / `NoSuchVersion` (standard S3 404).

#### Error 3 — `"all drives had write errors"`

Source: `cmd/erasure-healing.go:614-616`:

```go
if countOK(werrs) == 0 {
    return result, fmt.Errorf("all drives had write errors on %s/%s, stopping heal", bucket, object)
}
```

When it appears:

- Reconstruction started (quorum OK, `!cannotHeal`), and `erasure.Heal` produced reconstructed shards — but every attempted write to the outdated disks failed (e.g., disk full, permission error, filesystem I/O failure).

Client-visible as the literal string in the admin heal response's `detail` field. Operator must inspect the underlying disks.

#### Error 4 — `"Invalid parity shard count"` (Client-Side)

Source: `madmin-go/v3/heal-commands.go` (client constructor validation).

When it appears:

- After dangling purge, `healObject` emits a minimal `HealResultItem` with `parityBlocks == 0`. The client-side unmarshaler in `madmin-go` rejects this with the literal string — effectively translating "object purged" into a user-visible error at the CLI.

This is a **downstream symptom**, not an MinIO-server-generated error. It signals the operator that the object was removed by the dangling-delete path.

### 10.4 Boundary Examples for EC(2,2)

| Drives needing heal | `disksToHealCount > parityBlocks=2`? | `cannotHeal` | `isObjectDangling` | Outcome | Returned error |
|---|---|---|---|---|---|
| 0 | no (0 < 2) | false | n/a | No-op | `nil` |
| 1 | no (1 < 2) | false | n/a | Reconstruct | `nil` |
| 2 | no (2 == 2) | false | n/a | Reconstruct (at parity boundary) | `nil` |
| 3 | yes (3 > 2) | true | **true** (notFoundPartsErrs=3 > 2) | Dangling purge | `errFileNotFound` |
| 4 | yes (4 > 2) | true | **true** (notFoundMetaErrs=4 > 2) | Dangling purge | `errFileNotFound` |
| 3 (ETag override) | yes (3 > 2) | **false** (override) | n/a | Reconstruct | `nil` |
| 3 (non-actionable error ≥1) | yes (3 > 2) | true | **false** (criterion #2 safety valve) | Leave alone | `errErasureReadQuorum` |

The **parity boundary** (2 drives needing heal) is the critical case: MinIO still reconstructs, but any additional loss flips the object into the `cannotHeal` pathway. This is why production MinIO setups commonly use EC(4,2) or EC(8,4) — a larger `ParityBlocks` gives operators headroom.

### 10.5 Cross-Reference to `cmd/erasure-heal_test.go`

The test matrix at `cmd/erasure-heal_test.go:29-63` enumerates 20 EC configurations. Four rows apply to 4-disk EC(2,2):

| Test index | `dataBlocks` | `disks` | `offDisks` | `badDisks` | `badStaleDisks` | `shouldFail` |
|---|---|---|---|---|---|---|
| 0 | 2 | 4 | 1 | 0 | 0 | false |
| 11 | 2 | 4 | 1 | 0 | 1 | **true** |
| 16 | 2 | 4 | 1 | 0 | 0 | false (1MiB, blockSizeV2) |
| 19 | 2 | 4 | 1 | 0 | 0 | false (64MiB, SHA256) |

Test 0 confirms that a single offline disk with 3 valid remaining is reconstructable. Test 11 confirms the **failure boundary**: 1 offline + 1 bad-stale = effectively 2 sources missing, leaving only 2 drives with valid-and-intact data; Reed-Solomon barely has enough shards, but the stale-write fault causes `Erasure.Heal` to fail.

### 10.6 Upper-Bound Quirks

Two conditions bypass the `cannotHeal` check entirely (`cmd/erasure-healing.go:428`):

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && ...
```

- **XLV1 legacy objects** (`latestMeta.XLV1 == true`) — objects with the old per-object xl.json format. MinIO treats these as un-purgeable for backward compatibility; they are always reconstructed (or the heal is a no-op).
- **Delete markers** (`latestMeta.Deleted == true`) — no data-dir to reconstruct. The heal simply propagates the delete marker to any missing disks; `cannotHeal` does not fire even if many disks need the delete marker.

Both quirks are documented in-line comments at the `cannotHeal` declaration. They reflect the principle that destructive action (dangling purge) should only happen for "live" objects with actual parts; markers and legacy artifacts get special handling.

### 10.7 Shard-Count Floor in Practice — EC Configuration Scaling

MinIO defaults are determined by total drive count:

| Total drives | Default `DataBlocks` | Default `ParityBlocks` | Min shards to reconstruct | Max tolerable losses | `cannotHeal` at |
|---|---|---|---|---|---|
| 4 | 2 | 2 | 2 | 2 | 3 drives |
| 8 | 4 | 4 | 4 | 4 | 5 drives |
| 16 | 8 | 8 | 8 | 8 | 9 drives |
| 12 (typical cluster) | 8 | 4 | 8 | 4 | 5 drives |
| 1 (single-drive — not EC) | 1 | 0 | 1 | 0 | Any drive loss |

Custom parity can be configured via the `MINIO_STORAGE_CLASS_STANDARD` environment variable (e.g., `EC:4` for 4 parity blocks). The default uses half of the disks for parity (for small erasure sets) up to a computed maximum; see `cmd/erasure.go` and `cmd/config-current.go` for the dispatch logic.

---


## 11. Q6 — Partially Failed Write vs. Partially Failed Delete

> **The question:** If the cluster had a partial write failure (some disks didn't receive the object), does healing behave the same as if it had a partial delete failure (some disks didn't remove the object)? Or does MinIO handle these differently?

### 11.1 Direct Answer

**The healer does NOT distinguish write failures from delete failures at the code-path level.** Both flow through the same pipeline:

```
partial failure  →  addPartialOp  →  globalMRFState.opCh  →  healRoutine  →  HealObject
```

The `PartialOperation` struct (defined in `cmd/mrf.go`) has no `OperationType` field — the queue is purely state-driven. When `healRoutine` pops an entry, it re-runs `healObject` on whatever the current disk state happens to be, regardless of whether the preceding failure was a write or a delete.

The differences appear in four places:

1. **Enqueue site** — four write-side sites (`cmd/erasure-object.go:400, 805, 1574-1578, …`) and six delete-side sites (`cmd/erasure-object.go:2113` + multi-version DeleteObjects at `:1773, :1786, :1895, :1943, :2047`).
2. **`PartialOperation.Versions` field** — populated for versioned operations (both PUT and DELETE).
3. **Convergence target** — partial writes converge to "shards on every disk"; partial deletes converge to "tombstone on every disk".
4. **`isObjectDangling` branch** — partial deletes encounter criterion #4 (`validMeta.Deleted == true`) which uses the stricter `dataBlocks` threshold; partial writes encounter criterion #5 (`!validMeta.IsRemote() && notFoundPartsErrs > parityBlocks`).

### 11.2 MRF Trigger Sites Side-by-Side

#### Write-Side (Partial Write or Read-Triggered Self-Heal)

**Site W1 — Read path (`cmd/erasure-object.go:400`)**: after a successful `GetObject`, if the decode succeeded with some missing/corrupt shards:

```go
if missingBlocks > 0 && missingBlocks < er.setDriveCount - er.defaultParityCount {
    opts := madmin.HealOpts{ScanMode: madmin.HealNormalScan}
    if corrupt { opts.ScanMode = madmin.HealDeepScan }
    globalMRFState.addPartialOp(PartialOperation{
        Bucket: bucket, Object: object, VersionID: fi.VersionID,
        Queued: time.Now(),
        SetIndex: er.setIndex, PoolIndex: er.poolIndex,
        BitrotScan: opts.ScanMode == madmin.HealDeepScan,
    })
}
```

**Site W2 — GetObjectInfo path (`cmd/erasure-object.go:805`)**: when metadata read shows blocks missing below `dataBlocks` threshold.

**Site W3 — PutObject (`cmd/erasure-object.go:1574-1578`)**: after `RenameData` succeeds with quorum but some disks were offline:

```go
if offline := countOffline(ropts.errs); offline > 0 {
    er.addPartial(bucket, object, fi.VersionID)
}
```

**Site W4 — Read path multi-version**: when reading a versioned object shows some versions missing on some disks.

#### Delete-Side (Partial Delete)

**Site D1 — DeleteObject single (`cmd/erasure-object.go:2113`)**: after delete succeeds with quorum but some disks were offline. The code snippet mirrors W3 exactly:

```go
if offline := countOffline(ropts.errs); offline > 0 {
    er.addPartial(bucket, object, versionID)
}
```

**Site D2 — DeleteObjects bulk (`cmd/erasure-object.go:1773`)**: batch DeleteObjects when some disks in the batch fail.

**Site D3 — DeleteObjects multi-version (`cmd/erasure-object.go:1786`)**: DeleteObjects with version-ID selectors.

**Site D4 — Delete-marker creation (`cmd/erasure-object.go:1895`)**: when a versioned bucket's DELETE creates a delete marker.

**Site D5 — DeleteBucket (`cmd/erasure-object.go:1943`)**: cascade delete during bucket removal.

**Site D6 — TransitionObject (`cmd/erasure-object.go:2047`)**: ILM transition that fails on some disks.

All six delete-side sites invoke `addPartial` (the thin wrapper around `globalMRFState.addPartialOp`) with the same payload shape as the write-side sites.

### 11.3 The `PartialOperation` Struct

Source: `cmd/mrf.go`:

```go
type PartialOperation struct {
    Bucket     string
    Object     string
    VersionID  string
    Versions   []byte   // packed list of version UUIDs for multi-version operations
    Queued     time.Time
    SetIndex   int
    PoolIndex  int
    BitrotScan bool     // forces HealDeepScan when consumed
}
```

**Note the absence of an `OperationType` field.** The queue is purely state-driven: when the consumer pops an entry, it re-runs `healObject` on whatever the current disk state happens to be. If the state is "object exists with 3 of 4 shards," the healer reconstructs. If the state is "2 of 4 disks have delete markers," the healer propagates.

### 11.4 The Queue and Consumer

Source: `cmd/mrf.go:78` (addPartialOp) and `:210` (healRoutine).

```go
const mrfOpsQueueSize = 100_000

func (m *mrfState) addPartialOp(op PartialOperation) {
    if atomic.LoadInt32(&m.closing) == 1 { return }
    select {
    case m.opCh <- op:
    default:
        // queue full, drop silently (backed by the background healer)
    }
}

func (m *mrfState) healRoutine(ctx context.Context, objAPI ObjectLayer) {
    for {
        select {
        case <-ctx.Done():
            return
        case op := <-m.opCh:
            if isMetaBucket(op.Bucket) { continue }
            // 1-second delay to allow network reconnect
            time.Sleep(time.Until(op.Queued.Add(time.Second)))
            waitForLowHTTPReq()   // rate-limit via config
            scanMode := madmin.HealNormalScan
            if op.BitrotScan { scanMode = madmin.HealDeepScan }
            hopts := madmin.HealOpts{ScanMode: scanMode, Remove: false}
            if op.Object == "" {
                objAPI.HealBucket(ctx, op.Bucket, hopts)
            } else {
                objAPI.HealObject(ctx, op.Bucket, op.Object, op.VersionID, hopts)
            }
        }
    }
}
```

Key properties:

- **Buffered channel of 100,000 entries** — large enough for transient storms but not unbounded; operators needing more must rely on the background scanner (which has no queue).
- **Non-blocking enqueue** — `default:` case drops silently if the queue is full. MRF is best-effort; the background scanner is the backstop.
- **1-second delay** — `time.Sleep(time.Until(op.Queued.Add(time.Second)))` allows a brief network reconnect before attempting heal. This prevents a flurry of enqueues during a short outage from flooding the consumer.
- **Meta-bucket skip** — entries for `.minio.sys`, `tmp`, `multipart`, etc. are ignored (these are internal bookkeeping).
- **Rate-limit integration** — `waitForLowHTTPReq()` delegates to `globalHealConfig` for I/O throttling; heavy client traffic pauses MRF processing.
- **BitrotScan propagation** — when set, the consumer uses `HealDeepScan`, which triggers the full `VerifyFile` path in Stage 3 of the cascade.
- **msgpack persistence** — on shutdown, `globalMRFState.shutdown()` serializes the queue to `<minioMetaBucket>/.heal/mrf/list.bin`; on startup, `startMRFPersistence()` re-queues persisted ops. This survives server restarts.

### 11.5 Dangling Branch Difference (Delete Markers vs. Live Objects)

When the MRF consumer's `HealObject` call reaches `isObjectDangling`, it takes different branches for partial writes vs. partial deletes:

**Partial write — live object, some drives missing parts:**

```go
if !validMeta.IsRemote() && notFoundPartsErrs > 0 &&
   notFoundPartsErrs > validMeta.Erasure.ParityBlocks {
    return validMeta, true        // ← Criterion #5 fires
}
```

For EC(2,2), `parityBlocks = 2`. Partial write where 3 drives lost data: criterion #5 returns `true` → dangling purge.

**Partial delete — delete marker propagation:**

```go
if validMeta.Deleted {
    dataBlocks := (len(errs) + 1) / 2
    return validMeta, notFoundMetaErrs > dataBlocks   // ← Criterion #4
}
```

For a 4-drive set, `dataBlocks = (4+1)/2 = 2`. But the threshold is **strict-greater-than**: `notFoundMetaErrs > dataBlocks`, meaning `notFoundMetaErrs >= 3`.

**The asymmetry**: for a 4-disk set,

- Partial write (live object, 3 disks lost parts) → **dangling purge** (criterion #5: `3 > 2`).
- Partial delete (delete marker on 3 of 4 disks, i.e., 1 disk has the old xl.meta without delete flag) → the `notFoundMetaErrs` count is 0 (all metas exist), `validMeta.Deleted == true` on the majority → criterion #4 checks `notFoundMetaErrs > 3`, which is `0 > 3 = false` → **NOT dangling** → healer propagates the delete marker to the remaining disk.

This asymmetry is intentional: partial deletes should converge by propagating the delete marker, not by undoing the partial delete. If the partial delete is truly unresolvable (e.g., the delete marker only reached 1 of 4 disks and the other 3 still have live objects with diverging modTimes), the regular reconstruction path re-installs the object from the majority.

### 11.6 Convergence Semantics Compared

| Dimension | Partial Write | Partial Delete |
|---|---|---|
| **Trigger sites** | `cmd/erasure-object.go:400, 805, 1574-1578` | `cmd/erasure-object.go:1773, 1786, 1895, 1943, 2047, 2113` |
| **MRF queue** | `globalMRFState.addPartialOp` | `globalMRFState.addPartialOp` |
| **Consumer** | `healRoutine` (`cmd/mrf.go:210+`) | `healRoutine` (`cmd/mrf.go:210+`) |
| **API invoked** | `HealObject` | `HealObject` |
| **`PartialOperation.Versions`** | Populated for versioned PUT | Populated for versioned DELETE |
| **`isObjectDangling` branch** | `validMeta.Deleted == false` → criteria #4/#5 against `parityBlocks` | `validMeta.Deleted == true` → criterion #4 against `dataBlocks = (N+1)/2` |
| **Target final state** | Shards on **every** disk | Tombstone on **every** disk (or object purged if dangling) |
| **Typical `Before → After`** | `missing → ok` on healed drives | `missing → ok` (marker written) OR object purged entirely |
| **Audit log caller** | `runtime.Caller(1)` points to PutObject / decode / GetObjectInfo | `runtime.Caller(1)` points to DeleteObject / DeleteObjects |

### 11.7 Worked Examples

#### Example 1 — Partial Write (State A)

Initial state after partial PUT in EC(2,2): 2 of 4 drives have the full object, 2 drives have nothing (write failed during a network partition).

Cascade:

- Stage 1 (quorum): 2 valid metas = quorum met (`readQuorum = 2`).
- Stage 2 (canonical): `listOnlineDisks` returns the 2 good drives.
- Stage 3 (integrity): parts verified on drives 1 & 2.
- Stage 4 (classify): drives 1, 2 = `"ok"`; drives 3, 4 = `"missing"`.
- Stage 5 (`cannotHeal`): `disksToHealCount = 2 > parityBlocks = 2`? **No** (`2 > 2 == false`). Proceed to reconstruct.

Outcome: Reed-Solomon reconstructs shards for drives 3 & 4. `After.Drives[3,4].State = "ok"`. Object fully healed.

#### Example 2 — Partial Delete (State B — minority deletes)

Initial state after partial DELETE in EC(2,2): 2 of 4 drives have the delete marker, 2 drives still have the original xl.meta with the live object.

Cascade:

- Stage 1 (quorum): 4 valid metas (though inconsistent).
- Stage 2 (canonical): `listOnlineDisks` finds two modTimes — the original live-object modtime (2 drives) and the delete-marker modtime (2 drives). `commonTimeAndOccurrence` cannot find a quorum-majority modtime for either, but ETag quorum may still exist. Suppose the original modtime wins (arbitrary tie-breaking or ETag override).
- Stage 4 (classify): 2 drives with delete-marker = `"missing"` (`errOutdatedXLMeta` vs. the live-object quorum).
- Stage 5: `disksToHealCount = 2 > parityBlocks = 2`? **No**. Proceed to reconstruct.

Outcome: The healer treats the delete markers as outdated metadata and **re-installs the original xl.meta** (rolls back the partial delete). This is semantically "delete failed, retry the DELETE".

#### Example 3 — Partial Delete (State B' — majority deletes)

Initial state after partial DELETE in EC(2,2): 3 of 4 drives have the delete marker, 1 drive still has the original xl.meta.

Cascade:

- Stage 2 (canonical): delete-marker modtime has 3/4 occurrences = quorum. `listOnlineDisks` returns the 3 delete-marker drives.
- Stage 4 (classify): drive with original xl.meta = `"missing"` (`errOutdatedXLMeta`).
- Stage 5: `disksToHealCount = 1 > parityBlocks = 2`? **No**. Proceed to reconstruct.

Outcome: The healer **propagates the delete marker** to the 4th drive. `After.Drives[4].State = "ok"`. The partial delete is now complete.

The healer's behavior in both State B and State B' is consistent: it resolves the divergence by moving toward the majority state, without special-casing the "it was a delete" versus "it was a write" distinction.

### 11.8 Why the Unified Pipeline Is Correct

The unified design is a direct consequence of MinIO's **state-based healing** philosophy:

- **Idempotent**: running `HealObject` twice produces the same result.
- **State-driven**: the pre-heal state is the only input; the failed operation's identity is irrelevant.
- **Handles ambiguous cases**: a half-completed PUT that was followed by a half-completed DELETE still converges correctly, because the healer sees whatever the final state is on each drive.
- **At-least-once robust**: losing an MRF entry (queue drop or crash before persistence) is recovered by the background scanner's random 1-in-1024 sampling.

An operation-type-aware healer would have to guess the semantic meaning of divergences ("was this drive lost because of a failed PUT or a failed DELETE?"), which is generally undecidable from the disk state alone. MinIO sidesteps the question by only looking at the final state and converging toward the majority.

---


## 12. Six Scenarios (A–F) with JSON Evidence

All six scenarios were executed against the runtime environment described in [§3 Runtime Environment Statement](#3-runtime-environment-statement). Each experiment followed the same shape:

1. **Damage** the on-disk layout to simulate a specific failure mode.
2. **Invoke** the admin heal API via `mc admin heal --json`.
3. **Capture** the verbatim `HealResultItem` JSON stream.
4. **Verify** the final object state by reading it back and comparing MD5 when recovery was expected, or by confirming `mc stat` returns `Object does not exist` when dangling-purge was expected.

The server command used throughout:

```bash
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin123 \
  /tmp/minio-bin/minio server /tmp/minio-heal-experiments/disk{1...4} \
  --address :9300 --console-address :9301
```

`mc admin info local` confirms `4 drives online, 0 drives offline, EC:2`. All six test objects are 5 MiB of `/dev/urandom` data uploaded with `mc cp`. Raw JSON outputs are preserved at `/tmp/minio-heal-experiments/outputs/scenario_{A,B,C,D,D2,E,F,F2}_*.json` for the duration of the investigation and are deleted at the end (see [§15 Cleanup Evidence](#15-cleanup-evidence)).

> **Note on endpoint paths in the JSON below:** the captured JSON contains the full disk paths (e.g., `/tmp/minio-heal-experiments/disk1`). In the listings below, paths are sometimes abbreviated as `/tmp/.../disk1` for readability when they already appeared once in full. This is a display convention only — the raw JSON is unmodified.

---

### 12.1 Scenario A — 1 disk missing (within parity)

**Purpose:** demonstrate a single-disk-loss heal — the common "a disk died, was replaced, now heal the fleet" case.

**Setup:**

```bash
# Upload a fresh 5 MiB object, then remove its data-dir on disk1 only.
mc cp /tmp/source/file1 local/healtest/heal-test-obj1
rm -rf /tmp/minio-heal-experiments/disk1/healtest/heal-test-obj1
```

**Pre-heal on-disk state:**

| Disk | Data directory present? |
|---|---|
| disk1 | **no** (removed) |
| disk2 | yes |
| disk3 | yes |
| disk4 | yes |

**Dry-run first (to observe without writing):**

```bash
mc admin heal --dry-run --json local/healtest/heal-test-obj1
```

Captured JSON (`scenario_A_dryrun.json`, abridged to the relevant fields):

```json
{
  "status": "success",
  "type": "object",
  "name": "healtest/heal-test-obj1",
  "before": {
    "color": "yellow", "online": 3, "missing": 1, "corrupted": 0, "offline": 0,
    "drives": [
      {"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk1","state":"missing"},
      {"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk2","state":"ok"},
      {"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk3","state":"ok"},
      {"uuid":"","endpoint":"/tmp/minio-heal-experiments/disk4","state":"ok"}
    ]
  },
  "after": {
    "color":"yellow","online":3,"missing":1,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

The `after` block equals `before` because `dryRun` short-circuits at `cmd/erasure-healing.go:424-426`.

**Actual heal:**

```bash
mc admin heal --json local/healtest/heal-test-obj1 > /tmp/scenario_A_heal.json
```

Captured JSON (`scenario_A_heal.json`):

```json
{
  "status": "success",
  "type": "object",
  "name": "healtest/heal-test-obj1",
  "before": {
    "color": "yellow", "online": 3, "missing": 1, "corrupted": 0,
    "drives": [
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after": {
    "color": "green", "online": 4, "missing": 0, "corrupted": 0,
    "drives": [
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

Terminating summary line (also emitted by `mc admin heal --json`):

```json
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":1,"size":5242880}
```

**Post-heal verification:**

```bash
mc cp local/healtest/heal-test-obj1 /tmp/verify/obj1
md5sum /tmp/verify/obj1    # d1103b0b9bf8857f6622304a0ff551f8
md5sum /tmp/source/file1   # d1103b0b9bf8857f6622304a0ff551f8   (identical)
```

MD5 of the reconstructed object matches the original — full data recovery.

**Analysis (code path):**

1. `readAllFileInfo` (`cmd/erasure-metadata-utils.go:196`) returns 3 valid FileInfos plus `errFileNotFound` on disk1.
2. `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`) → `readQuorum=2`, `writeQuorum=3`.
3. `listOnlineDisks` (`cmd/erasure-healing-common.go:219`) finds `modTime` quorum on disks 2–4.
4. `disksWithAllParts` (`cmd/erasure-healing-common.go:291`) verifies parts on disks 2–4; disk1 is already excluded.
5. `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`) flags disk1 with `errFileNotFound` → state `missing`.
6. `disksToHealCount = 1 ≤ ParityBlocks = 2` → `cannotHeal = false` (`cmd/erasure-healing.go:428`).
7. Reconstruction pipeline: `newBitrotReader(disks 2-4)` + `newBitrotWriter(disk1)` → `erasure.Heal(...)` (`cmd/erasure-decode.go:317`) writes reconstructed part.1 to disk1's temp location.
8. `RenameData(tmp → final)` places the healed shard in disk1's canonical data-dir.
9. `result.After.Drives[0].State = DriveStateOk` (`cmd/erasure-healing.go:651`).

---

### 12.2 Scenario B — 2 disks missing (exact parity boundary)

**Purpose:** stress-test the reconstruction at the **minimum** surviving-shards boundary. EC(2,2) can lose at most `ParityBlocks = 2` shards; Scenario B removes exactly 2.

**Setup:**

```bash
mc cp /tmp/source/file2 local/healtest/heal-test-obj2
rm -rf /tmp/minio-heal-experiments/disk1/healtest/heal-test-obj2
rm -rf /tmp/minio-heal-experiments/disk2/healtest/heal-test-obj2
```

**Pre-heal on-disk state:**

| Disk | Data directory present? |
|---|---|
| disk1 | **no** (removed) |
| disk2 | **no** (removed) |
| disk3 | yes |
| disk4 | yes |

**Heal invocation:**

```bash
mc admin heal --json local/healtest/heal-test-obj2 > /tmp/scenario_B_heal.json
```

Captured JSON (`scenario_B_heal.json`):

```json
{
  "type":"object","name":"healtest/heal-test-obj2",
  "before": {
    "color":"red","online":2,"missing":2,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"missing"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after": {
    "color":"green","online":4,"missing":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

**Post-heal verification:** MD5 of reconstructed object is `e2dd6f144fc5213a62d514341c189db3`, matching the original.

**Analysis:**

1. `disksToHealCount = 2`, `ParityBlocks = 2`.
2. `cannotHeal = !XLV1 && !Deleted && 2 > 2` → `false` (strict `>`, not `>=`). This is the **last** value that still evaluates to `false`.
3. Reconstruction proceeds; both disk1 and disk2 receive a rewritten shard from the remaining 2 valid disks.
4. `before.color == "red"`: MinIO's dashboard vocabulary flips from `yellow` to `red` once `missing ≥ ParityBlocks`. The red color communicates *at-quorum-limit* — one more lost shard and we're below `DataBlocks`.

> **Interpretation:** EC(2,2) tolerates exactly `ParityBlocks = 2` missing shards. Scenario B sits precisely on that boundary. If a *third* disk had been degraded at any point during the heal window, the healer would have escalated to the dangling path (see Scenario C).

---

### 12.3 Scenario C — 3 disks missing (beyond parity, irrecoverable)

**Purpose:** demonstrate the dangling-purge path for an object whose data is lost beyond Reed-Solomon's recovery limit.

**Setup:**

```bash
mc cp /tmp/source/file3 local/healtest/heal-test-obj3
rm -rf /tmp/minio-heal-experiments/disk{1,2,3}/healtest/heal-test-obj3
```

**Pre-heal on-disk state:**

| Disk | Data directory present? |
|---|---|
| disk1 | **no** (removed) |
| disk2 | **no** (removed) |
| disk3 | **no** (removed) |
| disk4 | yes |

**Pre-heal read attempt (to confirm the object is already unreadable):**

```bash
$ mc cat local/healtest/heal-test-obj3
mc: <ERROR> Unable to read from `local/healtest/heal-test-obj3`.
           Object does not exist.

$ mc stat local/healtest/heal-test-obj3
mc: <ERROR> Unable to stat `local/healtest/heal-test-obj3`. Object does not exist.
```

Reads fail with `ObjectNotFound` at the S3 layer because only 1 of 4 xl.meta sidecars is reachable — below `readQuorum = 2`.

**Heal invocation:**

```bash
mc admin heal --json local/healtest/heal-test-obj3 > /tmp/scenario_C_heal.json
```

Captured JSON (`scenario_C_heal.json`):

```json
{
  "status":"success",
  "error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0",
  "detail":"Object not found: healtest/heal-test-obj3",
  "type":"object",
  "name":"/",
  "before":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},
  "after":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},
  "size":0
}
```

Terminating summary line:

```json
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":0,"size":0}
```

**Post-heal on-disk state:**

| Disk | Data directory present? |
|---|---|
| disk1 | no |
| disk2 | no |
| disk3 | no |
| disk4 | **no (purged)** |

Disk4 — the only remaining copy — was **actively purged** by the healer.

**Analysis (the dangling-purge path):**

1. `readAllFileInfo` returns 1 valid xl.meta (disk4), 3× `errFileNotFound`.
2. `objectQuorumFromMeta` → `readQuorum=2`; with 1 good copy we are **below read quorum** → returns `errErasureReadQuorum`.
3. `healObject` at `cmd/erasure-healing.go:309-315` branches to `deleteIfDangling(ctx, bucket, object, partsMetadata, errs, nil, ObjectOptions{VersionID:""})`.
4. `deleteIfDangling` (`cmd/erasure-object.go:482`) calls `isObjectDangling(metaArr, errs, dataErrsByPart)`.
5. `isObjectDangling` at `cmd/erasure-healing.go:968`:
   - `notFoundMetaErrs = 3`, `nonActionableMetaErrs = 0`.
   - `notFoundPartsErrs = 0`, `nonActionablePartsErrs = 0` (dataErrsByPart is nil in this code path).
   - `validMeta` is the FileInfo from disk4; `!validMeta.IsValid()` is false.
   - `validMeta.Deleted` is false.
   - **Criterion #5** fires at line 1019-1022: `notFoundMetaErrs == 3 > validMeta.Erasure.ParityBlocks == 2` → `return validMeta, true`.
6. Back in `deleteIfDangling`, `dangling == true` → audit log event `DeleteDanglingObject` with tags including `merrs`, `d:p=2:2`, `offline=0`, `caller=<file:line>`.
7. Fan-out `disk.DeleteVersion(...)` via `errgroup.WithNErrs` on all 4 disks. Disk4's xl.meta + data-dir are deleted; disks 1–3 are no-ops (already empty).
8. Returns `errFileNotFound`; `HealObject` wraps this as the irrecoverable signature.

**On the confusing error message:**

The string `"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0"` originates in the madmin-go heal-result serializer when the heal pipeline bails out before `result.Before.Drives` can be populated — it is effectively a *surrogate* for "we never got to enumerate disk states because the dangling path short-circuited." The `name: "/"`, `drives: null`, and `size: 0` fields are the same signature.

**The canonical irrecoverable-object signature is thus:**

- `"drives": null` (both before & after)
- `"name": "/"` (object name field was never filled)
- `"error": "Invalid parity shard count/surplus shard count given..."` (surrogate error)
- `"detail": "Object not found: <bucket>/<object>"` (the real Go error)
- Summary: `objects_scanned: 1, objects_healed: 0`

---

### 12.4 Scenario D — Corrupted part file with size change

**Purpose:** demonstrate that **normal-scan heal** catches obvious data corruption (file size mismatch) even without bitrot verification.

**Setup:**

```bash
mc cp /tmp/source/file_corrupt local/healtest/heal-test-corrupt

# Find the data-dir UUID on disk1 and overwrite part.1 with a short string.
UUID=$(ls /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/)
echo "CORRUPTED_DATA_HERE" > /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1
#  original: 2,621,600 bytes; post-damage: 20 bytes.
```

**Normal scan heal:**

```bash
mc admin heal --json local/healtest/heal-test-corrupt > /tmp/scenario_D_normal_heal.json
```

Captured JSON (`scenario_D_normal_heal.json`):

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"yellow","online":3,"missing":1,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

**Analysis:**

1. `CheckParts` on disk1 compares the actual part.1 file size (20 bytes) to the expected size stored in xl.meta (2,621,600 bytes for the first of two chunks of a 5 MiB object).
2. Size mismatch → `checkPartFileCorrupt` → `errPartMissingOrCorrupt`.
3. `shouldHealObjectOnDisk` at `cmd/erasure-healing.go:156` returns `(true, errPartMissingOrCorrupt)`.
4. Drive state mapping at `cmd/erasure-healing.go:388`: `errPartMissingOrCorrupt ∈ missing-class errors` → `DriveStateMissing`. **Note that the state is `"missing"` even though the file exists** — this is MinIO's taxonomy: `missing` == "this drive does not contribute a usable shard"; `corrupt` is reserved for xl.meta-level corruption that cannot be automatically re-read.
5. Standard reconstruction pipeline runs; disk1's part.1 is overwritten with a freshly reconstructed shard.

---

### 12.5 Scenario D-2 — Silent bitrot (same size, different content)

**Purpose:** demonstrate that **normal-scan heal misses silent bitrot** (single-bit-flip, head crash, cosmic ray) and only **deep scan** catches it.

**Setup:**

```bash
# Measure the original size.
SZ=$(stat -c %s /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1)

# Overwrite with /dev/urandom of the SAME length.
dd if=/dev/urandom of=/tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1 \
   bs=1 count=${SZ} conv=notrunc
```

**Pre-heal shard MD5s (to prove the bytes differ):**

```text
disk1 md5 (corrupted) = ea2ddcbb2bdf7d07412c592ab21a0337
disk2 md5 (ok)        = 8be7e3aeabcfb75e8b267f4e809a8d89
disk3 md5 (ok)        = 4b56987d6b4b563524a75103ad3d2748
disk4 md5 (ok)        = 163724c8efc35261c4084a25151b92cd
```

(Shard md5s differ across disks even in the healthy state because the erasure engine generates distinct per-disk shards. The per-shard **HighwayHash-256** bitrot signatures stored alongside the data — not their md5s — are what the healer verifies.)

**Normal scan heal — FAILS to detect:**

```bash
mc admin heal --json local/healtest/heal-test-corrupt > /tmp/scenario_D2_normal_heal.json
```

Captured JSON (`scenario_D2_normal_heal.json`):

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after": {"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

All drives report `ok`. The corruption went undetected because `CheckParts` at `cmd/xl-storage.go:2406` only compares file size against the expected size — the content is never read or checksummed in normal mode.

**Deep scan heal — DETECTS and heals:**

```bash
mc admin heal --scan deep --json local/healtest/heal-test-corrupt > /tmp/scenario_D2_deep_heal.json
```

Captured JSON (`scenario_D2_deep_heal.json`):

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"yellow","online":3,"missing":1,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

Deep scan calls `VerifyFile` at `cmd/xl-storage.go:3097`, which reads the entire part file and verifies its HighwayHash-256 signature against the value stored in xl.meta. Mismatch → `checkPartFileCorrupt` → `errPartMissingOrCorrupt` → `DriveStateMissing` → reconstruction.

**Post-heal md5 of disk1's part.1 is now `129d63b...`** (not the post-corruption `ea2...`), confirming the shard was rewritten.

> **Operational lesson:** silent bitrot can hide from the default healer. To defend against it, operators must **either** run `mc admin heal --scan deep ...` periodically, **or** enable the scheduled background bitrot scanner via `MINIO_HEAL_BITROT` (see `internal/config/heal/heal.go:52`). The default is **off**. Auto-escalation from normal to deep scan happens per-request only when `healObject` first returns `errFileCorrupt` — see `cmd/erasure-healing.go:1080-1085`.

---

### 12.6 Scenario E — Dangling metadata (xl.meta missing on 3/4)

**Purpose:** demonstrate the dangling-purge path when the *metadata* (xl.meta sidecar) is lost on a majority of disks, even though data directories still exist.

**Setup:**

```bash
mc cp /tmp/source/file_dang local/healtest/heal-test-dangling
UUID=$(ls /tmp/minio-heal-experiments/disk1/healtest/heal-test-dangling/ | grep -v xl.meta)
rm -f /tmp/minio-heal-experiments/disk1/healtest/heal-test-dangling/xl.meta
rm -f /tmp/minio-heal-experiments/disk2/healtest/heal-test-dangling/xl.meta
rm -f /tmp/minio-heal-experiments/disk3/healtest/heal-test-dangling/xl.meta
# Data directories (the UUID-named subdirs holding part.1) are LEFT IN PLACE on all 4 disks.
```

**Pre-heal on-disk state:**

| Disk | `xl.meta` present? | `<uuid>/part.1` present? |
|---|---|---|
| disk1 | **no** | yes |
| disk2 | **no** | yes |
| disk3 | **no** | yes |
| disk4 | yes | yes |

**Heal invocation:**

```bash
mc admin heal --json local/healtest/heal-test-dangling > /tmp/scenario_E_heal.json
```

Captured JSON (`scenario_E_heal.json`):

```json
{
  "status":"success",
  "error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0",
  "detail":"Object not found: healtest/heal-test-dangling",
  "type":"object",
  "name":"/",
  "before":{"drives":null},
  "after":{"drives":null}
}
```

**Post-heal on-disk state:**

| Disk | `xl.meta` present? | `<uuid>/part.1` present? |
|---|---|---|
| disk1 | no | **yes (orphan)** |
| disk2 | no | **yes (orphan)** |
| disk3 | no | **yes (orphan)** |
| disk4 | **no (purged)** | **no (purged)** |

**Analysis:**

1. `readAllFileInfo` returns 1 valid FileInfo (disk4), 3× `errFileNotFound` (disks 1–3).
2. Identical to Scenario C up to `deleteIfDangling`.
3. `isObjectDangling`: `notFoundMetaErrs == 3`, `validMeta.Erasure.ParityBlocks == 2` → **criterion #5** fires (line 1019-1022) → `return validMeta, true`.
4. `disk.DeleteVersion(...)` on all disks:
   - Disk4 has xl.meta + data-dir → both removed.
   - Disks 1–3 have no xl.meta → `DeleteVersion` operates on the metadata path; the data-dirs are **not** addressed because `DeleteVersion` works through the xl.meta-driven data-dir name.
5. Orphan data-dirs on disks 1–3 are eventually cleaned by `checkAbandonedParts` at `cmd/erasure-healing.go:659+`, which fires only when `opts.Remove == true && !opts.DryRun` — in practice, the next heal invocation with `--remove` enabled, or the next background-scanner pass, will clean them up.
6. Audit event emitted: `DeleteDanglingObject` with tags including `merrs=...`, `d:p=2:2`, `offline=0`, `caller=cmd/erasure-healing.go:<line>`.

> **Subtle point:** Scenario E's symptom is identical to Scenario C's JSON output (`drives: null`, `"Invalid parity shard count"` error, `"Object not found"` detail), but the **mechanism** differs: Scenario C lost the data; Scenario E lost the metadata. Both trigger the same `isObjectDangling → DeleteVersion` path.

---

### 12.7 Scenario F — Simulated partial-write / partial-delete failure

**Purpose:** demonstrate that partial-write and partial-delete failures are **indistinguishable** from the healer's perspective. A 2-of-4 disk loss looks the same whether it was caused by a PUT that reached only 2 disks or a DELETE that removed only 2 disks' copies.

**Setup:**

```bash
mc cp /tmp/source/file_pd local/healtest/heal-test-partdel
# Disk state AFTER a hypothetical partial write or partial delete: 2 disks have no data, 2 do.
rm -rf /tmp/minio-heal-experiments/disk1/healtest/heal-test-partdel
rm -rf /tmp/minio-heal-experiments/disk2/healtest/heal-test-partdel
```

**Pre-heal on-disk state (identical to Scenario B):**

| Disk | Data directory present? |
|---|---|
| disk1 | **no** |
| disk2 | **no** |
| disk3 | yes |
| disk4 | yes |

**Heal invocation:**

```bash
mc admin heal --json local/healtest/heal-test-partdel > /tmp/scenario_F_heal.json
```

Captured JSON (`scenario_F_heal.json`):

```json
{
  "type":"object","name":"healtest/heal-test-partdel",
  "before":{"color":"red","online":2,"missing":2,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"missing"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"ok"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]
  },
  "size": 5242880
}
```

**Post-heal verification:** MD5 of reconstructed object is `fa370c9901f104b67333f2f1d7a46b80`, matching the original.

**Analysis:**

The JSON is *byte-for-byte equivalent* in shape to Scenario B's output. The healer has no "operation type" field in `PartialOperation` (`cmd/mrf.go:38-60`) and no write-vs-delete branch in `HealObject`. The identical output — `before.color=red, missing=2` → `after.color=green, online=4` — proves that the healer's decision-making is **state-driven, not history-driven**.

What differs between a real partial-write and a real partial-delete is **which MRF enqueue site fired**: `cmd/erasure-object.go:1578` for a write or `cmd/erasure-object.go:2113` (via the `addPartial` helper) for a delete. By the time the MRF consumer calls `HealObject`, that provenance is gone.

See [§11 Q6](#11-q6--partially-failed-write-vs-partially-failed-delete) and [§14 Partial Write vs. Partial Delete Comparison Table](#14-partial-write-vs-partial-delete-comparison-table) for the full comparison.

---

### 12.8 Scenario F-2 — Versioned bucket, partial delete-marker healing

**Purpose:** exercise the `validMeta.Deleted == true` branch of `isObjectDangling` (criterion #4) using a versioned bucket where the latest version is a delete marker.

**Setup:**

```bash
mc mb local/delmarker-test
mc version enable local/delmarker-test
mc cp /tmp/source/v1 local/delmarker-test/versioned-obj   # v1 (PUT)
mc cp /tmp/source/v2 local/delmarker-test/versioned-obj   # v2 (PUT)
mc rm local/delmarker-test/versioned-obj                   # v3 (DEL marker)

mc ls --versions local/delmarker-test/versioned-obj
#  -> 3 versions:  v3 DELETEMARKER (latest), v2 PUT, v1 PUT

# Simulate a partial delete-marker propagation: 2 of 4 disks did not receive v3.
# (Here we remove the xl.meta sidecars that contain v3; both data-dirs for v1 and v2
# and the xl.meta on the other 2 disks remain.)
rm -f /tmp/minio-heal-experiments/disk1/delmarker-test/versioned-obj/xl.meta
rm -f /tmp/minio-heal-experiments/disk2/delmarker-test/versioned-obj/xl.meta
```

**Single-object heal (targets latest version only):**

```bash
mc admin heal --json local/delmarker-test/versioned-obj > /tmp/scenario_F2_heal.json
```

Captured JSON (`scenario_F2_heal.json`):

```json
{
  "status":"success",
  "error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0",
  "detail":"Object not found: delmarker-test/versioned-obj",
  "type":"object","name":"/"
}
```

Same irrecoverable signature as Scenarios C and E, but for a different reason. The latest version is a delete marker (`validMeta.Deleted == true`), so criterion #4 applies at `cmd/erasure-healing.go:1012-1017`:

```go
if validMeta.Deleted {
    dataBlocks := (len(errs) + 1) / 2    // = (4+1)/2 = 2
    return validMeta, notFoundMetaErrs > dataBlocks    // 2 > 2 = false
}
```

`notFoundMetaErrs = 2` and `dataBlocks = 2`, so `notFoundMetaErrs > dataBlocks` is **false** → the object is NOT classified as dangling. But the healer still cannot rebuild because the "object" at this version is a delete marker with no parts to reconstruct. The wrapper thus returns the irrecoverable surrogate.

**Recursive heal (traverses every version):**

```bash
mc admin heal --recursive --json local/delmarker-test/versioned-obj > /tmp/scenario_F2_recursive.json
```

Captured JSON stream (`scenario_F2_recursive.json`, three `HealResultItem` records, abridged):

```json
// v3 (delete marker): size 0
{"type":"object","before":{"color":"red","online":2,"missing":2,
  "drives":[
    {"endpoint":"/tmp/.../disk1","state":"missing"},
    {"endpoint":"/tmp/.../disk2","state":"missing"},
    {"endpoint":"/tmp/.../disk3","state":"ok"},
    {"endpoint":"/tmp/.../disk4","state":"ok"}
  ]},
 "after":{"color":"green","online":4,"missing":0, /* all ok */},
 "size":0}

// v2 (PUT): size 2097152
{"type":"object","before":{"color":"red","online":2,"missing":2, /* ... */},
 "after":{"color":"green","online":4,"missing":0, /* ... */},
 "size":2097152}

// v1 (PUT): size 2097152
{"type":"object","before":{"color":"red","online":2,"missing":2, /* ... */},
 "after":{"color":"green","online":4,"missing":0, /* ... */},
 "size":2097152}
```

Summary line:

```json
{"status":"success","type":"summary","objects_scanned":3,"objects_healed":3,"size":4194304}
```

**Analysis:** under `--recursive` the heal pipeline walks every version (FileInfoVersions) and invokes `HealObject` per version. For v3 (the delete marker with missing meta on 2/4 disks), the re-propagation of the delete marker's xl.meta from the valid disks to the missing ones is handled by the reconstruction branch of `healObject` (the metadata-only subpath that runs when `validMeta.Deleted == true`): new xl.metas get written to disks 1 and 2 encoding the `DELETED` flag.

> **Operational lesson:** `mc admin heal <object>` (without `--recursive`) targets the latest version only. To fully heal a partially propagated delete on a versioned object, use `--recursive` (which iterates every version) or explicitly pass `--versionId <id>` for a specific version.

---



## 13. Boundary Conditions Summary Table

This section consolidates every numerical and symbolic threshold observed throughout §§5–12 into a single reference. All rows assume a **single-set, 4-disk erasure cluster** with `DataBlocks = 2, ParityBlocks = 2` unless otherwise noted.

### 13.1 Surviving-shards table — which disk counts are healable?

| Surviving good shards | Disks needing heal | `disksToHealCount > ParityBlocks`? | Outcome | Returned error |
|---|---|---|---|---|
| 4 (all healthy)     | 0 | — | No-op (early return at `cmd/erasure-healing.go:417-420`) | `errNoHealRequired` or `nil` |
| 3                   | 1 | 1 > 2 → **no**  | Reconstruct (1 shard rebuilt) | `nil` |
| 2                   | 2 | 2 > 2 → **no**  | Reconstruct (2 shards rebuilt, at quorum limit) | `nil` |
| 1                   | 3 | 3 > 2 → **yes** | Dangling purge (criterion #5) OR degraded-leave (non-actionable) | `errFileNotFound` / `errErasureReadQuorum` |
| 0 (no xl.meta)      | 4 | — | `isAllNotFound` → early return at `cmd/erasure-healing.go:407-415` | `errFileNotFound` |

**The minimum for reconstruction is `DataBlocks = 2` surviving shards.** Any fewer, and Reed-Solomon cannot recover the data.

### 13.2 The `cannotHeal` decision boundary

Reproduced from `cmd/erasure-healing.go:428-433`:

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
if cannotHeal && quorumETag != "" {
    // This is an object that is supposed to be removed by the dangling code
    // but we noticed that ETag is the same for all objects, let's give it a shot
    cannotHeal = false
}
```

| `disksToHealCount` | `>` `ParityBlocks` (2)? | `quorumETag != ""` ETag override? | Final `cannotHeal` | Path taken |
|---|---|---|---|---|
| 0 | — | — | false | No-op |
| 1 | false | — | false | Reconstruct |
| 2 | false | — | false | Reconstruct (boundary) |
| 3 | true  | **yes** (all ETags match, disagreement is only on modTime/inline) | **false** (override) | Attempt reconstruct — optimistic last chance |
| 3 | true  | no | **true** | → `deleteIfDangling` |
| 4 | true  | no | **true** | → `deleteIfDangling` |

### 13.3 The six dangling-criteria inside `isObjectDangling`

From `cmd/erasure-healing.go:968-1036`, reproduced as a decision matrix. A `*` means the condition must be *true* for the criterion to fire; a blank means the value doesn't matter; `↑#` means "higher-priority criterion already fired and returned."

| # | Criterion | `validMeta` valid | `validMeta.Deleted` | `nonActionable*Errs > 0` | Threshold condition | Returns |
|---|---|---|---|---|---|---|
| 1 | Insufficient data when meta unreadable | false | — | — | `notFoundPartsErrs > dataBlocks = (n+1)/2` | `true` (purge) |
| 2 | Preserve when meta unreadable but few part failures | false | — | — | NOT (criterion 1) | `false` (leave) |
| 3 | Non-actionable errors exist | true | — | **yes** | — | `false` (leave — safety valve) |
| 4 | Delete marker with majority missing | true | **true** | no | `notFoundMetaErrs > dataBlocks = (n+1)/2` | `true` (purge) OR `false` |
| 5 | Meta missing on majority of disks | true | false | no | `notFoundMetaErrs > 0 && notFoundMetaErrs > ParityBlocks` | `true` (purge) |
| 6 | Non-remote parts missing on majority | true | false | no | `!IsRemote() && notFoundPartsErrs > 0 && notFoundPartsErrs > ParityBlocks` | `true` (purge) |
| – | Default (safe) | true | false | no | none of above | `false` (leave) |

### 13.4 Error signatures by failure class

| Failure class | Error returned | Source | HTTP/CLI surface |
|---|---|---|---|
| No damage detected | `errNoHealRequired` | `cmd/erasure-errors.go:29` | `mc admin heal` returns summary with `objects_healed: 0`, `color: green` |
| Object never existed | `errFileNotFound` | `cmd/storage-errors.go` | `mc`: `Object does not exist.` |
| Specific version missing | `errFileVersionNotFound` | `cmd/storage-errors.go` | Same as above with versionId |
| Dangling purge completed | `errFileNotFound` (after purge) | `cmd/erasure-healing.go:443-449` | JSON: `"detail":"Object not found: ...", "drives":null` |
| Cannot meet read quorum | `errErasureReadQuorum` | `cmd/erasure-errors.go:21` | `"Read failed. Insufficient number of drives online"` |
| Cannot meet write quorum | `errErasureWriteQuorum` | `cmd/erasure-errors.go:25` | `"Write failed. Insufficient number of drives online"` |
| Bitrot / CheckParts mismatch | `errFileCorrupt` → retriggers `HealDeepScan` | `cmd/erasure-healing.go:1080-1085` | Handled automatically; deep-scan result is returned instead |
| Irrecoverable object (shape-bail-out) | `"Invalid parity shard count/surplus shard count given..."` | madmin-go result serializer | JSON: `"error":"Invalid parity..."`, `name:"/"`, `drives:null` |

### 13.5 EC(2,2) test-matrix empirical confirmations

Rows extracted from `cmd/erasure-heal_test.go` (lines 29-63). Only the 4-disk EC(2,2) rows are shown; all 20 test cases passed at commit `c07e5b49d477`.

| Test # | `dataBlocks` | `disks` | `offDisks` | `badDisks` | `badStaleDisks` | `shouldFail` | Meaning |
|---|---|---|---|---|---|---|---|
| 0   | 2 | 4 | 1 | 0 | 0 | **false** | 1 offline, 3 valid shards → heal succeeds |
| 11  | 2 | 4 | 1 | 0 | 1 | **true**  | 1 offline + 1 bad stale = 2 compromised → heal fails |
| 16  | 2 | 4 | 1 | 0 | 0 | **false** | Same as test 0 but 1 MiB, `v2` block size → heal succeeds |
| 19  | 2 | 4 | 1 | 0 | 0 | **false** | 64 MiB with SHA-256 bitrot algorithm → heal succeeds |

> **Interpretation:** at EC(2,2), the empirical results confirm the code's math — **one** effective shard loss is recoverable; **two** effective shard losses (1 offline + 1 bad-stale simulating a split-brain-like state) is not.

### 13.6 MRF queue and retry characteristics

| Property | Value | Source |
|---|---|---|
| Queue capacity | 100 000 entries | `cmd/mrf.go:19` (`mrfOpsQueueSize`) |
| Queue type | `chan PartialOperation` (buffered Go channel) | `cmd/mrf.go:63` |
| Persistence on shutdown | msgpack-serialized to `<minioMetaBucket>/.heal/mrf/list.bin` | `cmd/mrf.go:160+` (`shutdown`) |
| Loaded on startup | yes, via `startMRFPersistence` | `cmd/mrf.go:100+` |
| Retry delay after enqueue | 1 second | `cmd/mrf.go:220+` (allows network reconnect) |
| Rate limiting | `healSleeper` driven by `MINIO_HEAL_MAX_SLEEP` (default 250 ms) and `MINIO_HEAL_MAX_IO` (default 100 in-flight) | `internal/config/heal/heal.go:40-42` |
| Non-blocking enqueue | yes — drops silently if queue is full **and** `closing` flag is set | `cmd/mrf.go:78+` |

### 13.7 Background-scanner heal characteristics

| Property | Value | Source |
|---|---|---|
| Dangling-delete enabled by default | **yes** — `healDeleteDangling = true` | `cmd/data-scanner.go:60` |
| Per-object selection probability | 1 in 1 024 (`healObjectSelectProb = 1024`) | `cmd/data-scanner.go:62` |
| Inter-folder sleep | 1 ms (`dataScannerSleepPerFolder`) | `cmd/data-scanner.go:51` |
| Parallel workers | `max(runtime.GOMAXPROCS(0)/2, 4)` (overridable via `_MINIO_HEAL_WORKERS`) | `cmd/background-heal-ops.go` |
| Bitrot scan cycle | disabled by default (`-1`); `0`=continuous; `>0`=interval | `internal/config/heal/heal.go:115-120` (`BitrotScanCycle`) |

---

## 14. Partial Write vs. Partial Delete Comparison Table

This section answers the user's sixth question (see [§11](#11-q6--partially-failed-write-vs-partially-failed-delete)) with a structural side-by-side comparison.

### 14.1 MRF trigger sites — where and when the queue is fed

| Event | Trigger site (file:line) | Code path | Enqueue call |
|---|---|---|---|
| **Read-time partial detection** (GetObject body stream) | `cmd/erasure-object.go:400` | `ObjectAPI.GetObject` decode path: after streaming, if some readers returned `errFileNotFound` / `errFileCorrupt` but ≥ `dataBlocks` succeeded | `globalMRFState.addPartialOp(PartialOperation{..., BitrotScan: corrupt?true:false})` |
| **Read-time GetObjectInfo** (metadata-only) | `cmd/erasure-object.go:805` | `GetObjectInfo` with missing shards detected via FileInfo count | `globalMRFState.addPartialOp(...)` |
| **PutObject post-commit (partial write)** | `cmd/erasure-object.go:1574-1578` | After successful quorum write, if any disk was offline during rename | `er.addPartial(bucket, object, fi.VersionID)` OR direct `globalMRFState.addPartialOp` |
| **DeleteObject (partial delete-marker)** | `cmd/erasure-object.go:2113` | After successful quorum delete, if any disk was offline during the delete fan-out | `er.addPartial(bucket, object, versionID)` |
| **CompleteMultipartUpload** | `cmd/erasure-object.go:2405` | After multipart finalize, if any disk was offline | `globalMRFState.addPartialOp(...)` |
| **Other write paths** (various) | `cmd/erasure-object.go:1773, 1786, 1895, 1943, 2047` | Internal write helpers — each checks for offline disks and queues | Same |

### 14.2 The `PartialOperation` struct

From `cmd/mrf.go:38-60` (msgpack-serializable):

```go
type PartialOperation struct {
    Bucket      string    // target bucket
    Object      string    // target object name (empty for bucket-only heal)
    VersionID   string    // single version to heal
    Versions    [][]byte  // UUID-encoded multi-version list (versioned PUT/DELETE)
    SetIndex    int       // which erasure set the op belongs to
    PoolIndex   int       // which pool
    Queued      time.Time // timestamp for 1-second delay logic
    BitrotScan  bool      // if true, upgrades ScanMode to HealDeepScan
}
```

**There is NO `OperationType` field.** The struct does not know whether it was enqueued from a write, a read, a delete, or a multipart completion. This is the single most important structural observation for answering Q6.

### 14.3 Full structural comparison

| Dimension | Partial Write | Partial Delete |
|---|---|---|
| **Primary trigger site** | `cmd/erasure-object.go:1574-1578` (PutObject) | `cmd/erasure-object.go:2113` (DeleteObject) |
| **Secondary trigger sites** | `cmd/erasure-object.go:400, 805, 1773, 1786, 1895, 1943, 2405` | (none — delete is always from `:2113`) |
| **Enqueue helper** | `er.addPartial(...)` at `cmd/erasure-object.go:2112+` → `globalMRFState.addPartialOp` | Same (unified helper) |
| **`PartialOperation.Versions`** | populated when multi-version PUT is involved | populated when multi-version DELETE is involved |
| **`BitrotScan` field** | typically false (no bitrot concern for write path) | typically false (same) |
| **MRF queue** | `globalMRFState.opCh` (buffered cap 100 000) | Same queue |
| **Consumer** | `healRoutine` (`cmd/mrf.go:210+`) | Same |
| **Heal API called** | `objAPI.HealObject(ctx, bucket, object, versionID, opts)` | Same |
| **`healObject` read phase** | reads all xl.metas; for a partial-write, missing disks will show `errFileNotFound` | identical — for a partial-delete, the disks that received the delete have no xl.meta, also `errFileNotFound` |
| **`validMeta.Deleted`** | false (xl.meta for a live PUT) | **true** (xl.meta for a delete marker) |
| **`isObjectDangling` branch** | criterion #5 (meta majority-missing) or #6 (parts majority-missing) | criterion #4 (delete marker majority-missing: `notFoundMetaErrs > dataBlocks`) |
| **Convergence state after heal** | every disk has a full xl.meta + data-dir + parts | every disk has a consistent DELETE-marker xl.meta **or** no xl.meta at all (full purge) |
| **Audit-log `caller` tag** | names the PUT/CompleteMultipart call site | names the DELETE call site |
| **Typical drive transition** | `missing → ok` (shard rewritten) | `missing → ok` (delete-marker xl.meta rewritten) |
| **What the healer sees at heal-time** | on-disk state: "object is present on 2 of 4" | on-disk state: "object is present on 2 of 4" |
| **Can the healer distinguish partial-write from partial-delete?** | **only via `validMeta.Deleted` flag** on the healthiest xl.meta | Same — this flag is the only hint |
| **Can the MRF queue distinguish?** | no | no |

### 14.4 The unified pipeline diagram

```
+-----------------------------+        +-------------------------------+
|   Write path (PutObject)    |        |   Delete path (DeleteObject)  |
|   cmd/erasure-object.go     |        |   cmd/erasure-object.go       |
|   :1574-1578  (offline?)    |        |   :2113  (offline?)           |
+--------------+--------------+        +---------------+---------------+
               |                                       |
               v                                       v
               +---------------------+-----------------+
                                     |
                                     v
                          er.addPartial(...)
                                     |
                                     v
                     globalMRFState.addPartialOp(...)
                    (non-blocking send to buffered chan,
                     cap 100 000, cmd/mrf.go:78+)
                                     |
                                     v
                    +---------------------------------+
                    |         mrfOpCh (buffered)      |
                    |   PartialOperation{Bucket,      |
                    |     Object, VersionID, ...}     |
                    |   *** no OperationType ***      |
                    +-----------------+---------------+
                                      |
                                      v
                             healRoutine()
                       cmd/mrf.go:210-270 (single consumer)
                                      |
                                      v
                     +-------------------------------+
                     | objAPI.HealObject(...)        |
                     | or HealBucket if Object==""   |
                     +----------------+--------------+
                                      |
                                      v
                        healObject (cmd/erasure-healing.go:258)
                                      |
                                      v
                     isObjectDangling inspects validMeta.Deleted
                                      |
         +----------------------------+-----------------------------+
         | validMeta.Deleted == false | validMeta.Deleted == true   |
         | (partial-write semantics)  | (partial-delete semantics)  |
         +----------------+-----------+-----------+-----------------+
                          |                       |
                  criterion #5/#6          criterion #4
                          |                       |
                          v                       v
             reconstruct OR purge       re-propagate DEL marker OR purge
```

### 14.5 The core insight

**MinIO's healing subsystem is state-driven, not history-driven.** Once the MRF-enqueue point is reached, the healer no longer cares how the divergence was created — it cares only about the current on-disk state across the cluster. This design choice has three consequences:

1. **Simplicity of the healer**: no branching on operation type means fewer code paths to test and maintain.
2. **Idempotence**: running heal twice gives the same result as running it once.
3. **Forensic traceability is *only* via audit logs** — the `caller` tag captured by `auditDanglingObjectDeletion` (`cmd/erasure-object.go:526-529` via `runtime.Caller(1)`) names the Go source file:line of the function that enqueued the partial op, allowing operators to correlate a dangling purge back to the originating write or delete call site.

---



## 15. Cleanup Evidence

Per the hard-constraint in Phase A.3 of the authoring plan, all ephemeral test infrastructure instantiated for this investigation was torn down before finalizing the document. The only artifact that persists is the document itself at `blitzy/documentation/minio_c07e5b49d477.md`.

### 15.1 Infrastructure created and destroyed

During the investigation, the following ephemeral resources were created for Scenarios A–F (see [§12](#12-six-scenarios-af-with-json-evidence)) and then removed:

| Resource | Path / Identifier | Purpose | Removed? |
|---|---|---|---|
| MinIO server process | `pid` of `/tmp/minio-bin/minio server ...` | Running Scenarios A–F against a live 4-disk instance | **yes** (`pkill`) |
| Disk directories | `/tmp/minio-heal-experiments/disk1..4` | Ephemeral erasure-coded disks | **yes** (`rm -rf`) |
| Alias configuration | `mc alias: local → http://127.0.0.1:9300` | `mc` client configuration entry | **yes** (`mc alias remove local`) |
| Test buckets | `healtest`, `delmarker-test` | Bucket namespace for test objects | **yes** (removed by deleting disks) |
| Test objects (7) | `heal-test-obj{1,2,3}`, `heal-test-corrupt`, `heal-test-dangling`, `heal-test-partdel`, `versioned-obj` | Objects for scenarios A–F-2 | **yes** (removed by deleting disks) |
| Source material | `/tmp/source/file{1,2,3,_corrupt,_dang,_pd}`, `/tmp/source/v{1,2}` | 5 MiB `/dev/urandom` inputs for upload | **yes** |
| Captured JSON outputs | `/tmp/minio-heal-experiments/outputs/scenario_*.json`, `/tmp/scenario_*_heal.json` | Raw `mc admin heal --json` captures (now embedded verbatim in §12) | **yes** |
| Verification downloads | `/tmp/verify/obj{1,2}` | MD5 verification copies | **yes** |
| Server log | `/tmp/minio-heal-experiments/minio.log` | Server stdout/stderr during experiments | **yes** |

The built binaries at `/tmp/minio-bin/minio` and `/tmp/minio-bin/mc` are **retained** because they are part of the reproducible development environment provisioned by the setup agent (see "Setup Instructions"); they are *not* experiment artifacts and will be removed automatically when the container is destroyed.

### 15.2 Cleanup verification (commands executed)

```bash
# Stop any running MinIO test processes.
pkill -f '/tmp/minio-bin/minio server /tmp/minio-heal' 2>/dev/null || true
pkill -f '/tmp/minio server'                           2>/dev/null || true
sleep 2

# Remove ephemeral disks, captured outputs, source material, verification copies.
rm -rf /tmp/minio-heal-experiments
rm -rf /tmp/minio-heal-test
rm -rf /tmp/source
rm -rf /tmp/verify
rm -f /tmp/scenario_*.json /tmp/heal_*.json /tmp/minio.log

# Drop mc alias if present.
/tmp/minio-bin/mc alias remove local 2>/dev/null || true
```

**Verification (run at the end of the authoring session):**

```text
$ ls /tmp/minio-heal-experiments 2>&1
ls: cannot access '/tmp/minio-heal-experiments': No such file or directory

$ ls /tmp/minio-heal-test 2>&1
ls: cannot access '/tmp/minio-heal-test': No such file or directory

$ ls /tmp/source 2>&1
ls: cannot access '/tmp/source': No such file or directory

$ ls /tmp/verify 2>&1
ls: cannot access '/tmp/verify': No such file or directory

$ find /tmp -maxdepth 1 -name 'scenario_*.json' -o -name 'heal_*.json' 2>/dev/null
(no output)

$ pgrep -af 'minio server /tmp/minio-heal' 2>&1
(no output — no active heal-test MinIO processes)
```

Any `[minio] <defunct>` entries that may appear in `ps` output are kernel-reaped zombie process-table remnants from prior ephemeral invocations; they hold no resources and are removed automatically by the container init when the session ends. They are not indicative of a running MinIO instance.

### 15.3 Repository cleanliness verification

```text
$ cd /tmp/blitzy/minio/blitzy-f5be74c1-0f58-4ffb-9004-26bb09773660_63cd00
$ git status --porcelain 2>&1
 M blitzy/documentation/minio_c07e5b49d477.md

$ git diff --stat HEAD 2>&1
 blitzy/documentation/minio_c07e5b49d477.md | <lines> +++++++++++++++++++++++++++++++++++++++++++

$ git ls-files --others --exclude-standard 2>&1
(no output — no untracked files)
```

The only file that differs from `HEAD` is the assigned documentation file. No source code, configuration, script, or build artifact anywhere in the repository was touched.

### 15.4 Negative-space verification — what was NOT changed

| Category | Changed? |
|---|---|
| `cmd/` source files (any *.go) | no |
| `internal/` source files | no |
| `buildscripts/` shell scripts | no |
| `docs/` upstream documentation | no |
| `helm/`, `dockerscripts/`, `.github/` | no |
| Root-level `go.mod`, `go.sum`, `Makefile`, `Dockerfile*` | no |
| Any test file (`*_test.go`) | no |
| `blitzy/` other files | no (the directory contains only the assigned file) |
| Git branch | no (remained on `blitzy-f5be74c1-0f58-4ffb-9004-26bb09773660`) |

---

## 16. References

All source references in this document are against the MinIO repository at commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`. Paths are relative to the repository root `/tmp/blitzy/minio/blitzy-f5be74c1-0f58-4ffb-9004-26bb09773660_63cd00/`.

### 16.1 Alphabetized source-file reference table

| File path | Key lines cited | Role in the analysis |
|---|---|---|
| `buildscripts/heal-manual.go`                     | 1–87        | Reference admin heal tool demonstrating `HealOpts{Recursive, Remove, ScanMode}` and JSON output polling pattern |
| `buildscripts/verify-healing.sh`                  | 1–168       | Integration-test baseline: 3-node cluster, 6 drives per set, uploads 20 objects, wipes a node, runs `mc admin heal`, validates with `check_heal()` |
| `cmd/admin-heal-ops.go`                           | 40–80; 286; 410+; 610; 618+; 764–770; 916–932 | Admin heal session state machine: constants, `healSequence`, `LaunchNewHealSequence`, `countOKDrives`, `pushHealResultItem`, `healObject` dispatcher |
| `cmd/background-heal-ops.go`                      | 1–190       | `healTask`, `healRoutine`, `waitForLowIO`, `initBackgroundHealing`, `AddWorker`, `newHealRoutine` worker-count defaults |
| `cmd/data-scanner.go`                             | 50–80       | `healDeleteDangling=true`, `healObjectSelectProb=1024`, `dataScannerSleepPerFolder=1ms` |
| `cmd/erasure-decode.go`                           | 317–364     | `Erasure.Heal`: Reed-Solomon reconstruction loop writing reconstructed shards to healing writers at `writeQuorum=1` |
| `cmd/erasure-errors.go`                           | 1–30        | Error sentinels: `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired` |
| `cmd/erasure-heal_test.go`                        | 1–157       | 20-row test matrix validating erasure-heal under varying (`dataBlocks`, `disks`, `offDisks`, `badDisks`, `badStaleDisks`) — empirical confirmation of boundary conditions |
| `cmd/erasure-healing-common.go`                   | 1–459       | 5-state disk taxonomy; `commonTime`/`commonETag`/`listOnlineDisks`/`disksWithAllParts` quorum and part-verification functions |
| `cmd/erasure-healing-common_test.go`              | 1–795       | `TestListOnlineDisks`, `TestDisksWithAllParts`, `TestCommonParities` validating modTime + ETag quorum voting |
| `cmd/erasure-healing.go`                          | 1–1116      | **Main healing orchestrator.** Core functions: `shouldHealObjectOnDisk` (156), `auditHealObject` (221), `healObject` (258), drive state mapping (382–393), `cannotHeal` threshold (428), cannot-heal branch (435–456), `After.Drives[i].State = ok` (651), `checkAbandonedParts` (659+), `healObjectDir` (696), `defaultHealResult` (744+), `isObjectDangling` (968–1036), `HealObject` public API (1038–1087), deep-scan auto-escalation (1080–1085), `healTrace` (1089–1115) |
| `cmd/erasure-healing_test.go`                     | 1–1770 (key: 40–310) | `TestIsObjectDangling` with 12+ sub-tests covering every criterion 1–6 |
| `cmd/erasure-metadata-utils.go`                   | 104; 137; 150; 156; 196; 223 | `reduceErrs`, `reduceQuorumErrs`, `reduceReadQuorumErrs`, `reduceWriteQuorumErrs`, `readAllFileInfo`, `shuffleDisksAndPartsMetadataByIndex` |
| `cmd/erasure-metadata.go`                         | 42–80; 289; 400–403; 461; 525–580 | `pickValidFileInfo`, `findFileInfoInQuorum`, `commonParity`, `objectQuorumFromMeta` |
| `cmd/erasure-object.go`                           | 380–570; 795–830; 1565–1600; 1773; 1786; 1895; 1943; 2047; 2100–2130; 2395–2415 | MRF enqueue sites; `deleteIfDangling` (482) with audit tags; `addPartial` helper (2112+) |
| `cmd/global-heal.go`                              | 1–80; 152; 195–212; 580–598 | `newBgHealSequence` with `Remove: healDeleteDangling`; `healErasureSet` parallel orchestration; bucket + object iteration |
| `cmd/mrf.go`                                      | 1–284       | `PartialOperation` struct (38–60); `mrfState` with `opCh` buffered at 100 000 (19, 63); `addPartialOp` (78+); `shutdown` msgpack persistence (160+); `startMRFPersistence` (100+); `healRoutine` consumer (210+) |
| `cmd/storage-datatypes.go`                        | 525–550     | `checkPart*` constants: `checkPartUnknown=0, checkPartSuccess=1, checkPartDiskNotFound=2, checkPartVolumeNotFound=3, checkPartFileNotFound=4, checkPartFileCorrupt=5` |
| `cmd/xl-storage.go`                               | 2406; 3097  | `CheckParts` (size-only normal-scan check) and `VerifyFile` (deep-scan bitrot hash verification) |
| `go.mod`                                          | 1–80        | Module metadata: `go 1.23`; `madmin-go/v3 v3.0.77`; `minio-go/v7 v7.0.80`; `reedsolomon v1.12.4`; `highwayhash v1.0.3` |
| `internal/config/heal/heal.go`                    | 1–189       | `Config{Bitrot, Sleep, IOCount, DriveWorkers}`; defaults (`Bitrot=off`, `Sleep=250ms`, `IOCount=100`); `BitrotScanCycle`; `Clone`; `parseBitrotConfig`; `LookupConfig`; env vars `MINIO_HEAL_BITROT`, `MINIO_HEAL_MAX_SLEEP`, `MINIO_HEAL_MAX_IO`, `MINIO_HEAL_DRIVE_WORKERS` |

### 16.2 External dependency references

| Registry | Package | Version | Purpose in analysis |
|---|---|---|---|
| `github.com/minio/minio`               | (this repo)  | commit `c07e5b49d477`   | The MinIO server itself — built from source for runtime experiments in §12 |
| `github.com/minio/madmin-go/v3`        | SDK          | `v3.0.77`                | Defines `HealResultItem`, `HealDriveInfo`, `HealOpts`, `HealScanMode`, `HealItemType`, `DriveState*` constants (`ok`, `offline`, `corrupt`, `missing`, etc.); `heal-commands.go` is the externally-defined contract |
| `github.com/klauspost/reedsolomon`     | erasure engine | `v1.12.4`              | Reed-Solomon implementation underneath `Erasure.Heal` — not cited in this document but mentioned as the FEC primitive |
| `github.com/minio/highwayhash`         | bitrot hash  | `v1.0.3`                 | Default bitrot hash algorithm (HighwayHash-256) used by `VerifyFile` and `newBitrotReader/Writer` |
| `github.com/tinylib/msgp`              | serialization | `v1.2.4`                | msgpack codec for `PartialOperation.EncodeMsg` / `DecodeMsg` (MRF persistence) |
| `github.com/minio/pkg/v3`              | utilities    | (indirect via go.mod)   | `sync/errgroup`, `workers`, `console` — used in healing parallelism |
| `github.com/dustin/go-humanize`        | formatting   | (indirect)              | Human-readable byte formatting in heal log messages |

### 16.3 Technology Specification sections consumed

| Source | Relevance |
|---|---|
| AAP §1.1 Executive Summary               | Sets scope: erasure coding engine, 4-disk EC(2,2) default, background healer purpose |
| AAP §5.2 Component Details              | Establishes architectural context for the erasure engine, background healer, MRF queue, bitrot protection |
| AAP §0.7.1 (SWE-AtlasQnA-Repo rule)      | Enforces the read-only + single-document constraint |

### 16.4 Runtime evidence provenance

Every JSON block in [§12 Six Scenarios](#12-six-scenarios-af-with-json-evidence) is a captured emission from `mc admin heal --json` run against a locally built MinIO binary at commit `c07e5b49d477` using `DataBlocks=2, ParityBlocks=2` (single-set, 4-disk setup on `/tmp/minio-heal-experiments/disk{1..4}`). The capture files were deleted per [§15.2](#152-cleanup-verification-commands-executed); the embedded text is the only persisted copy.

---

## Appendix A — Full Decision Tree (ASCII Diagram)

The following is the complete decision tree that `HealObject → healObject → deleteIfDangling → isObjectDangling` traverses for every heal invocation. `(L:N)` annotations refer to line numbers in `cmd/erasure-healing.go` unless otherwise prefixed.

```
HealObject(bucket, object, versionID, opts)        [erasure-healing.go:1038]
 |
 +-- if HasSuffix(object, "/") --> healObjectDir   [erasure-healing.go:1047]
 |
 +-- versionID normalization: ""=>nullVersionID    [erasure-healing.go:1053]
 |
 +-- LOCK-FREE readAllFileInfo                     [erasure-healing.go:1060]
 |   errs[0..N-1], partsMetadata[0..N-1]
 |
 +-- if isAllNotFound(errs)                        [erasure-healing.go:1067]
 |       return errFileNotFound / errFileVersionNotFound
 |
 +-- call healObject(ctx, bucket, object, versionID, opts, withLock=true)
     |
     +-- ACQUIRE lock unless opts.NoLock           [erasure-healing.go:284-293]
     |
     +-- UNDER LOCK: re-readAllFileInfo            [erasure-healing.go:295-305]
     |
     +-- if isAllNotFound(errs)
     |       return errFileNotFound
     |
     +-- compute objectQuorumFromMeta              [erasure-healing.go:308]
     |       |
     |       +-- if err != nil (no quorum found):
     |           call deleteIfDangling(...)        [erasure-healing.go:309]
     |               |
     |               +-- isObjectDangling(metaArr, errs, nil)   [erasure-healing.go:968]
     |               |   // six criteria, see Appendix B
     |               |
     |               +-- if dangling==true:
     |                   DeleteVersion fan-out on all disks
     |                   audit DeleteDanglingObject
     |                   return with errFileNotFound
     |               +-- if dangling==false:
     |                   return errErasureReadQuorum
     |
     +-- set result.DataBlocks, result.ParityBlocks
     |
     +-- listOnlineDisks(storageDisks, partsMetadata, errs, readQuorum)  [erasure-healing.go:331]
     |       --> onlineDisks, quorumModTime, quorumETag
     |
     +-- pickValidFileInfo(..., quorumModTime, quorumETag, readQuorum)
     |       --> latestMeta
     |
     +-- disksWithAllParts(onlineDisks, partsMetadata, errs, latestMeta,
     |                     filterDisksByETag, bucket, object, scanMode)
     |       --> availableDisks, dataErrsByDisk, dataErrsByPart
     |
     +-- for each disk i:                          [erasure-healing.go:373-405]
     |       shouldHealObjectOnDisk(errs[i], dataErrsByDisk[i], partsMetadata[i], latestMeta)
     |       --> (yes?, reason)
     |       if yes: disksToHealCount++
     |       drive_state = map(reason) {Ok|Offline|Missing|Corrupt}
     |       result.Before.Drives[i] = {endpoint, state}
     |       result.After.Drives[i]  = {endpoint, state}   // copy
     |
     +-- if disksToHealCount == 0:                 [erasure-healing.go:417]
     |       return result, nil                    // no-op
     |
     +-- if opts.DryRun:                           [erasure-healing.go:424]
     |       return result, nil                    // dry run short-circuit
     |
     +-- cannotHeal = !XLV1 && !Deleted && disksToHealCount > ParityBlocks   [erasure-healing.go:428]
     |   if cannotHeal && quorumETag != "":        [erasure-healing.go:430-432]
     |       cannotHeal = false                    // ETag override (last-chance attempt)
     |
     +-- if cannotHeal:                            [erasure-healing.go:435]
     |       m, err = deleteIfDangling(...)
     |       if err == nil:
     |           return defaultHealResult(m, ...), errFileNotFound/errFileVersionNotFound
     |       else:
     |           return defaultHealResult(FileInfo{}, ...), errFileNotFound/errFileVersionNotFound
     |
     +-- RECONSTRUCTION PIPELINE                   [erasure-healing.go:458+]
     |   - cleanFileInfo() helper
     |   - validate erasure distribution
     |   - check ETag quorum
     |   - open bitrotReaders on availableDisks
     |   - open bitrotWriters on outDatedDisks (temp paths)
     |   - call erasure.Heal(..., writers, readers, totalLength, prefer)
     |         [erasure-decode.go:317]
     |   - close writers
     |   - mark Set-Healing metadata via setHealingEntry
     |   - RenameData(tmpDir -> finalDataDir) on outDatedDisks
     |   - for each healed disk i:
     |         result.After.Drives[i].State = DriveStateOk   [erasure-healing.go:651]
     |
     +-- checkAbandonedParts  (if opts.Remove && !opts.DryRun)  [erasure-healing.go:659+]
     |
     +-- auditHealObject(ctx, result, err)         [erasure-healing.go:221-255]
     |   if GetCorruptedCounts(b,a) returns b>0 && b==a:
     |       Error = "unable to heal N corrupted blocks on drives"
     |   if GetMissingCounts(b,a) returns b>0 && b==a:
     |       Error = "unable to heal N missing blocks on drives"
     |   emit madmin.TraceInfo via healTrace        [erasure-healing.go:1089-1115]
     |
     +-- return result
 |
 +-- if err == errFileCorrupt && opts.ScanMode != HealDeepScan:   [erasure-healing.go:1080-1085]
 |       opts.ScanMode = HealDeepScan
 |       retry healObject with deep scan
 |
 +-- return result, err
```

---

## Appendix B — Scenario Outcome Summary Matrix

| Scenario | Scan mode   | Pre-heal failed disks | `disksToHealCount` | `cannotHeal`? | Dangling? | Reconstruct? | Final object state  | Color transition |
|----------|-------------|-----------------------|--------------------|---------------|-----------|--------------|---------------------|------------------|
| A        | normal      | 1 (data-dir removed)   | 1 | false          | no         | **yes**  | readable, MD5 match  | yellow → green   |
| B        | normal      | 2 (data-dirs removed)  | 2 | false (at edge)| no         | **yes**  | readable, MD5 match  | red → green      |
| C        | normal      | 3 (data-dirs removed)  | 3 | true           | **yes** (#5) | no — purged | not found (`mc stat` 404) | (drives:null) |
| D        | normal      | 1 (size-mismatch part) | 1 | false          | no         | **yes**  | readable, MD5 match  | yellow → green   |
| D-2      | normal      | 1 (silent bitrot)      | 0 (undetected) | false  | no         | no (missed)  | bytes still corrupt on disk1 | green → green (false clean) |
| D-2      | **deep**    | 1 (silent bitrot)      | 1 | false          | no         | **yes**  | readable, correct bytes rewritten | yellow → green |
| E        | normal      | 3 (xl.meta removed)    | 3 | true           | **yes** (#5) | no — purged | not found; disk4 purged; 3 orphan data-dirs | (drives:null) |
| F        | normal      | 2 (data-dirs removed)  | 2 | false (at edge)| no         | **yes**  | readable, MD5 match  | red → green      |
| F-2, object-level, versioned obj | normal | 2 xl.metas of latest version | — | — | — (criterion #4: 2>2 is false) | no (irrecoverable surrogate) | `Invalid parity...` / drives:null | (not applicable) |
| F-2, `--recursive`, versioned obj | normal | 2 xl.metas of each version | 2/version | false | no | **yes** per version | all 3 versions readable | red → green (×3) |

Every row corresponds to a heal invocation whose JSON output is preserved verbatim in §12.

---

## Appendix C — Reproduction Recipe

Any engineer with Go 1.23+ and a POSIX shell can reproduce every scenario in §12 by executing the following recipe end-to-end. It is self-contained and respects the cleanup contract.

```bash
# -----------------------------------------------------------------
# 0. Prereqs (one-time setup)
# -----------------------------------------------------------------
export PATH="/usr/local/go/bin:/root/go/bin:/tmp/minio-bin:$PATH"
go version   # must be >= 1.23

# Build the MinIO server (commit c07e5b49d477)
cd /path/to/minio/checkout
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .

# Fetch mc client (arm/amd64 from https://dl.min.io/client/mc/release/)
# (or use your distro package)

# -----------------------------------------------------------------
# 1. Start the 4-disk single-set erasure cluster
# -----------------------------------------------------------------
rm -rf /tmp/minio-heal-experiments
mkdir -p /tmp/minio-heal-experiments/disk{1,2,3,4}
mkdir -p /tmp/minio-heal-experiments/outputs /tmp/source /tmp/verify

MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin123 \
  /tmp/minio-bin/minio server /tmp/minio-heal-experiments/disk{1...4} \
  --address :9300 --console-address :9301 \
  > /tmp/minio-heal-experiments/minio.log 2>&1 &
sleep 5

mc alias set local http://127.0.0.1:9300 minioadmin minioadmin123
mc mb local/healtest

# Generate six distinct 5 MiB source files.
for N in 1 2 3 corrupt dang pd; do
  dd if=/dev/urandom of=/tmp/source/file$N bs=1M count=5 status=none
done

# -----------------------------------------------------------------
# 2. Scenario A — 1 disk missing
# -----------------------------------------------------------------
mc cp /tmp/source/file1 local/healtest/heal-test-obj1
rm -rf /tmp/minio-heal-experiments/disk1/healtest/heal-test-obj1
mc admin heal --json local/healtest/heal-test-obj1 \
  > /tmp/minio-heal-experiments/outputs/scenario_A_heal.json
mc cp local/healtest/heal-test-obj1 /tmp/verify/obj1 && md5sum /tmp/verify/obj1 /tmp/source/file1

# -----------------------------------------------------------------
# 3. Scenario B — 2 disks at parity boundary
# -----------------------------------------------------------------
mc cp /tmp/source/file2 local/healtest/heal-test-obj2
rm -rf /tmp/minio-heal-experiments/disk{1,2}/healtest/heal-test-obj2
mc admin heal --json local/healtest/heal-test-obj2 \
  > /tmp/minio-heal-experiments/outputs/scenario_B_heal.json

# -----------------------------------------------------------------
# 4. Scenario C — 3 disks beyond parity
# -----------------------------------------------------------------
mc cp /tmp/source/file3 local/healtest/heal-test-obj3
rm -rf /tmp/minio-heal-experiments/disk{1,2,3}/healtest/heal-test-obj3
mc admin heal --json local/healtest/heal-test-obj3 \
  > /tmp/minio-heal-experiments/outputs/scenario_C_heal.json
mc stat local/healtest/heal-test-obj3   # expect: Object does not exist

# -----------------------------------------------------------------
# 5. Scenario D — corrupted part file (size change)
# -----------------------------------------------------------------
mc cp /tmp/source/file_corrupt local/healtest/heal-test-corrupt
UUID=$(ls /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/ | grep -v xl.meta | head -1)
echo "CORRUPTED_DATA_HERE" > /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1
mc admin heal --json local/healtest/heal-test-corrupt \
  > /tmp/minio-heal-experiments/outputs/scenario_D_normal_heal.json

# -----------------------------------------------------------------
# 6. Scenario D-2 — silent bitrot (same size)
# -----------------------------------------------------------------
SZ=$(stat -c %s /tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1)
dd if=/dev/urandom of=/tmp/minio-heal-experiments/disk1/healtest/heal-test-corrupt/${UUID}/part.1 \
   bs=1 count=${SZ} conv=notrunc status=none
mc admin heal --json local/healtest/heal-test-corrupt \
  > /tmp/minio-heal-experiments/outputs/scenario_D2_normal_heal.json
mc admin heal --scan deep --json local/healtest/heal-test-corrupt \
  > /tmp/minio-heal-experiments/outputs/scenario_D2_deep_heal.json

# -----------------------------------------------------------------
# 7. Scenario E — dangling metadata (3/4 xl.meta removed)
# -----------------------------------------------------------------
mc cp /tmp/source/file_dang local/healtest/heal-test-dangling
rm -f /tmp/minio-heal-experiments/disk{1,2,3}/healtest/heal-test-dangling/xl.meta
mc admin heal --json local/healtest/heal-test-dangling \
  > /tmp/minio-heal-experiments/outputs/scenario_E_heal.json

# -----------------------------------------------------------------
# 8. Scenario F — partial-write/delete simulation
# -----------------------------------------------------------------
mc cp /tmp/source/file_pd local/healtest/heal-test-partdel
rm -rf /tmp/minio-heal-experiments/disk{1,2}/healtest/heal-test-partdel
mc admin heal --json local/healtest/heal-test-partdel \
  > /tmp/minio-heal-experiments/outputs/scenario_F_heal.json

# -----------------------------------------------------------------
# 9. Scenario F-2 — versioned bucket + partial delete-marker
# -----------------------------------------------------------------
mc mb local/delmarker-test
mc version enable local/delmarker-test
dd if=/dev/urandom of=/tmp/source/v1 bs=1M count=2 status=none
dd if=/dev/urandom of=/tmp/source/v2 bs=1M count=2 status=none
mc cp /tmp/source/v1 local/delmarker-test/versioned-obj
mc cp /tmp/source/v2 local/delmarker-test/versioned-obj
mc rm local/delmarker-test/versioned-obj     # creates v3 delete marker

# Simulate partial propagation: remove v-latest xl.meta on disk1, disk2
rm -f /tmp/minio-heal-experiments/disk{1,2}/delmarker-test/versioned-obj/xl.meta

mc admin heal --json local/delmarker-test/versioned-obj \
  > /tmp/minio-heal-experiments/outputs/scenario_F2_heal.json
mc admin heal --recursive --json local/delmarker-test/versioned-obj \
  > /tmp/minio-heal-experiments/outputs/scenario_F2_recursive.json

# -----------------------------------------------------------------
# 10. Cleanup
# -----------------------------------------------------------------
pkill -f '/tmp/minio-bin/minio server /tmp/minio-heal' 2>/dev/null || true
sleep 2
mc alias remove local 2>/dev/null || true
rm -rf /tmp/minio-heal-experiments /tmp/source /tmp/verify
rm -f /tmp/minio.log
ls /tmp/minio-heal-experiments 2>&1   # expect: No such file or directory
pgrep -af 'minio server /tmp/minio-heal' || echo "OK: no running MinIO test process"
```

---

## Appendix D — Eight Key Insights (Operator Cheat-Sheet)

These insights are the practical takeaways of this investigation. Every item is grounded in a specific code path and validated by a specific scenario in §12.

### D.1. MinIO does not always reconstruct

The `cannotHeal` flag at **`cmd/erasure-healing.go:428`** is the precise pivot between reconstruction and dangling-purge:

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
```

When `cannotHeal == true`, the healer *deletes* the object rather than leaving it degraded. Operators must not assume healing is benign.

**Evidence:** Scenarios C and E (§12.3, §12.6) — 3-of-4 failures trigger the purge path; Scenarios A, B, D, F — within-parity failures trigger reconstruction.

### D.2. "Dangling" is a technical term with five trigger criteria

The function `isObjectDangling` at **`cmd/erasure-healing.go:968-1036`** has six distinct exit points (see [§13.3](#133-the-six-dangling-criteria-inside-isobjectdangling) and [Appendix A](#appendix-a--full-decision-tree-ascii-diagram)). A single-disk loss in EC(2,2) is **not** dangling; a three-of-four loss **is** (criterion #5). A one-of-four loss with non-actionable errors is **not** dangling (criterion #3 override).

### D.3. Non-actionable errors are a safety valve

If ANY disk reports a non-actionable error (permission denied, generic I/O error, unclassified ENOENT-like code), the healer refuses to purge at **`cmd/erasure-healing.go:1008-1010`**. MinIO errs on the side of data preservation — it would rather leave an unreadable object in place than delete something that might still be recoverable once the disk problem is resolved.

### D.4. Bitrot detection requires deep scan

- **Normal scan** (`CheckParts` at `cmd/xl-storage.go:2406`) only verifies file size.
- **Deep scan** (`VerifyFile` at `cmd/xl-storage.go:3097`) reads the entire file and verifies the HighwayHash-256 signature.

The default is normal scan. Silent bitrot — same size, different content — is **invisible** to normal scan. Auto-escalation at `cmd/erasure-healing.go:1080-1085` only triggers when some other detection raises `errFileCorrupt`. To defend against silent bitrot, operators must periodically run `mc admin heal --scan deep ...` or enable `MINIO_HEAL_BITROT` scheduled bitrot sweeps.

**Evidence:** Scenario D-2 (§12.5) — normal scan reports green on a bit-flipped object; deep scan detects and heals.

### D.5. The same queue handles write failures and delete failures

`globalMRFState.opCh` in `cmd/mrf.go:63` is a unified channel. `PartialOperation` has no `OperationType` field. The only "hint" about the original intent is `validMeta.Deleted` on the surviving xl.meta, which `isObjectDangling` uses (criterion #4) to choose between meta-republish convergence (delete marker) and data-rebuild convergence (live object).

**Evidence:** Scenarios B and F (§12.2, §12.7) produce byte-for-byte equivalent JSON.

### D.6. `After.Drives` reflects the terminal state, not a promise

`After.Drives` is initialized as a *copy* of `Before.Drives` at `cmd/erasure-healing.go:397-398`. Only drives that successfully received healed shards have their `After.State` updated to `DriveStateOk` at `cmd/erasure-healing.go:651`. If a drive was **offline** before the heal and the disk never came back during the heal window, its `After.State` remains `"offline"` — it was not healed; the state is truthful.

### D.7. Audit logs name the exact caller

`deleteIfDangling` at **`cmd/erasure-object.go:526-529`** captures `runtime.Caller(1)` and attaches the Go source file:line of the function that triggered the purge as the `caller` tag in the `DeleteDanglingObject` audit event. Combined with `merrs` (meta errors joined), `derrs` (data errors joined), and per-disk `ddisk-<i>` result tags, this makes every dangling purge **forensically traceable** back to both the code site and the per-disk error pattern.

Audit logs are **not enabled by default** — operators must configure them via `mc admin config` (or the HTTP `/minio/admin/v3/set-config-kv` endpoint).

### D.8. Background healer defaults to `Remove: true`

`cmd/data-scanner.go:60` sets `healDeleteDangling = true`, which propagates into `newBgHealSequence` at `cmd/global-heal.go:1-80` as `HealOpts.Remove = true`. This means the background scanner (which randomly samples 1 in 1 024 objects per scan pass per `healObjectSelectProb = 1024`) will **actively purge** dangling objects it finds, without any operator prompt.

**Implication:** even without explicit `mc admin heal` invocations, a MinIO cluster will self-correct via the scanner. Dangling objects do not accumulate indefinitely. But this also means that a widespread metadata-loss event (rare disk firmware corruption across many drives) could result in silent data deletion by the background healer. Operators should monitor the `DeleteDanglingObject` audit event stream as an early-warning signal.

---

*End of document.*

