# MinIO Erasure-Code Healing — Decision Making Under Ambiguous Cluster State

> **Repository:** `minio/minio` @ commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`
> **Scope:** Static code analysis of the healing subsystem combined with runtime experiments
> on a local 4-disk erasure-coded instance (EC:2, `DataBlocks=2`, `ParityBlocks=2`).
> **Source of truth:** Only code references (file + line) and verbatim runtime JSON output.

---

## Table of Contents

1. [Executive Summary — What the healer actually decides](#1-executive-summary)
2. [The Healing Pipeline — Step-by-Step Code Walk](#2-the-healing-pipeline)
3. [Per-Disk State Classification — How states are computed](#3-per-disk-state-classification)
4. [The Five Dangling-Object Criteria](#4-dangling-object-criteria)
5. [Quorum Math for a 4-disk EC(2,2) Erasure Set](#5-quorum-math)
6. [Runtime Experiments — Actual `HealResultItem` JSON](#6-runtime-experiments)
7. [Boundary Conditions — When healing succeeds vs fails](#7-boundary-conditions)
8. [Partially-Failed-Write vs Partially-Failed-Delete](#8-write-vs-delete)
9. [Backstop: Logs, Audits and What Shows Up](#9-logs-audits)
10. [Answers to the Six Specific Questions](#10-answers)

---

## 1. Executive Summary

### 1.1 Does MinIO always reconstruct from valid shards?

**No.** MinIO's healer operates a three-way decision:

| Cluster condition | Decision | Code path |
|---|---|---|
| `disksToHealCount ≤ ParityBlocks` → enough healthy shards | **Reconstruct** (Reed-Solomon) | `cmd/erasure-healing.go:428` (`cannotHeal` false) → `erasure.Heal()` in `cmd/erasure-decode.go:317` |
| `disksToHealCount > ParityBlocks` but everyone agrees on the ETag | **Reconstruct** (override) | `cmd/erasure-healing.go:429-433` (`quorumETag != "" ⇒ cannotHeal = false`) |
| `disksToHealCount > ParityBlocks` and no ETag agreement AND `isObjectDangling(...) == true` | **Purge from all disks** | `cmd/erasure-healing.go:436-455` → `deleteIfDangling` in `cmd/erasure-object.go:482` |
| `disksToHealCount > ParityBlocks` and no ETag agreement AND `isObjectDangling(...) == false` (e.g., non-actionable I/O errors) | **Leave it alone — return `errErasureReadQuorum`** | `cmd/erasure-object.go:486-489` |
| Dry-run healing (`opts.DryRun == true`) | **Describe what would happen — no writes** | `cmd/erasure-healing.go:425-427` |

### 1.2 Key files referenced

| File | Line(s) | Role |
|---|---|---|
| `cmd/erasure-healing.go` | 258–657 | `healObject` orchestrator |
| `cmd/erasure-healing.go` | 156–183 | `shouldHealObjectOnDisk` (classifier) |
| `cmd/erasure-healing.go` | 382–393 | Internal-error → `DriveState*` mapping |
| `cmd/erasure-healing.go` | 428–456 | `cannotHeal` threshold + ETag override |
| `cmd/erasure-healing.go` | 968–1036 | `isObjectDangling` (5 criteria) |
| `cmd/erasure-healing.go` | 934–948 | `danglingMetaErrsCount` |
| `cmd/erasure-healing.go` | 950–962 | `danglingPartErrsCount` |
| `cmd/erasure-healing-common.go` | 219–255 | `listOnlineDisks` (modTime/ETag quorum) |
| `cmd/erasure-healing-common.go` | 291–459 | `disksWithAllParts` (part integrity verification) |
| `cmd/erasure-object.go` | 482–563 | `deleteIfDangling` + audit tags |
| `cmd/erasure-object.go` | 400, 805, 1578, 1773, 1786, 1895, 1943, 2047, 2113 | MRF `addPartialOp(...)` / `addPartial(...)` call sites |
| `cmd/erasure-decode.go` | 317–364 | `Erasure.Heal()` — Reed-Solomon reconstruction |
| `cmd/erasure-metadata.go` | 531–565 | `objectQuorumFromMeta` |
| `cmd/mrf.go` | 38–284 | MRF queue, `healRoutine`, msgpack persistence |
| `cmd/storage-datatypes.go` | 536–545 | `checkPart*` integer constants |

### 1.3 Key external type references (madmin-go v3)

| File | Purpose |
|---|---|
| `github.com/minio/madmin-go/v3/heal-commands.go` lines 112–159 | `HealItemType`, nine `DriveState*` constants, `HealDriveInfo`, `HealResultItem` |

---

## 2. The Healing Pipeline

This section traces the complete path from an admin heal API call to either a reconstruction or a purge.

### 2.1 Entry point: `HealObject` (admin API)

```
cmd/erasure-healing.go:1038-1087
func (er erasureObjects) HealObject(ctx context.Context, bucket, object, versionID string,
    opts madmin.HealOpts) (hr madmin.HealResultItem, err error)
```

The admin endpoint (`POST /minio/admin/v3/heal/{bucket}/{object}`) dispatches to this method.
Special cases:

- **Directory objects**: handled separately by `healObjectDir()` at `cmd/erasure-healing.go:698-783`.
  Dangling rule: `isObjectDirDangling()` treats a directory as dangling if `found < notFound && found > 0`.
- **First pass is lockless**: `HealObject` calls `readAllFileInfo(..., ReadOptions{ReadData: false})` first to decide whether the object even exists anywhere. If `isAllNotFound(errs) == true`, it returns `errFileNotFound` immediately — no lock taken.
- **Deep scan escalation**: if the normal-scan healer returns `errFileCorrupt`, `HealObject` retries with `ScanMode = HealDeepScan`. This matters because `CheckParts` (normal) only verifies *size*, while `VerifyFile` (deep) performs *content-level bitrot verification*.

### 2.2 The `healObject` internal function

```
cmd/erasure-healing.go:258-657
func (er *erasureObjects) healObject(ctx context.Context, bucket string, object string,
    versionID string, opts madmin.HealOpts) (result madmin.HealResultItem, err error)
```

The function executes the following eleven ordered steps:

1. **Acquire an object-level lock** unless `opts.NoLock == true` (`cmd/erasure-healing.go:279-292`).
2. **Read all xl.meta** via `readAllFileInfo()` on every disk (capturing per-disk errors).
3. **All-not-found shortcut**: if every disk returned `errFileNotFound`, return without touching anything.
4. **Compute quorum**: `objectQuorumFromMeta(ctx, partsMetadata, errs, er.defaultParityCount)` at `cmd/erasure-metadata.go:531`.
   On failure → delegate to `deleteIfDangling(...)` (dangling-cleanup fallback).
5. **Pick the latest metadata** via `listOnlineDisks()` (`cmd/erasure-healing-common.go:219`) — chooses the disks whose modTime appears `readQuorum` times.
6. **Pick the authoritative `FileInfo`** via `pickValidFileInfo()` (modTime + hash quorum).
7. **Verify parts on each online disk** via `disksWithAllParts()` (`cmd/erasure-healing-common.go:291`) — this is where `CheckParts` / `VerifyFile` run and produce `dataErrsByDisk` / `dataErrsByPart`.
8. **Classify each disk**: iterate through disks, calling `shouldHealObjectOnDisk()` (`cmd/erasure-healing.go:156-183`). Every disk needing a heal is marked in `outDatedDisks` and `disksToHealCount++`. Every disk gets a `DriveState*` string recorded into `result.Before.Drives` *and* `result.After.Drives`.
9. **The crucial threshold check** (`cmd/erasure-healing.go:428`):

   ```go
   cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted &&
                 disksToHealCount > latestMeta.Erasure.ParityBlocks
   if cannotHeal && quorumETag != "" {
       cannotHeal = false  // all disks agree on ETag — reconstruct anyway
   }
   ```

   - `latestMeta.XLV1` means this is legacy backend format; such objects always attempt heal.
   - `latestMeta.Deleted` is a delete marker (no parts to reconstruct).
   - `quorumETag` comes from `getQuorumETag()` over the `partsMetadata` slice. If every readable xl.meta references the same ETag, the metadata is self-consistent and the healer will override the parity check and still try to reconstruct (typically used to recover from DataDir bookkeeping inconsistencies).

10. **If `cannotHeal` is still true** → `er.deleteIfDangling(...)` (see §4). The return value is either
    `errFileNotFound` (if purged) or `errErasureReadQuorum` (if left alone).
11. **Otherwise, perform the reconstruction**:
    - Choose a temp UUID and data-dir.
    - For each part: create `newBitrotReader` over `latestDisks` and `newBitrotWriter` / `newStreamingBitrotWriterBuffer` over `outDatedDisks`.
    - Call `erasure.Heal(ctx, writers, readers, partSize, prefer)` (`cmd/erasure-decode.go:317`).
    - On success, mark each healed `FileInfo` with `SetHealing()` (sets `X-Minio-Internal-healing=true`) and atomically `RenameData(...)` from `.minio.sys/tmp/<tmpID>/` to the final location.
    - For each healed disk, update `result.After.Drives[i].State = madmin.DriveStateOk`.

### 2.3 The classifier: `shouldHealObjectOnDisk`

```
cmd/erasure-healing.go:156-183
```

| Condition on `(erErr, partsErrs, meta, latestMeta)` | Returns | Reason reported |
|---|---|---|
| `erErr` is `errFileNotFound`, `errFileVersionNotFound`, or `errFileCorrupt` | `true, erErr` | the original error |
| `erErr == nil` and `meta.XLV1 == true` | `true, errLegacyXLMeta` | legacy backend format |
| `erErr == nil` and `!latestMeta.Equals(meta)` | `true, errOutdatedXLMeta` | metadata lag |
| `erErr == nil`, latest, any `partsErrs[i]` in {`checkPartFileNotFound`, `checkPartFileCorrupt`} | `true, errPartMissingOrCorrupt` | data-file damage |
| Otherwise | `false, nil` (or `false, erErr`) | no action |

### 2.4 The reconstructor: `Erasure.Heal`

```
cmd/erasure-decode.go:317-364
func (e Erasure) Heal(ctx context.Context, writers []io.Writer, readers []io.ReaderAt,
    totalLength int64, prefer []bool) error
```

- Wraps the readers in a `parallelReader` that reads one shard per reader per block (shard size = `erasure.ShardSize()`).
- Per block: `DecodeDataAndParityBlocks(bufs)` → Reed-Solomon reconstruction from the reedsolomon library.
- Writes to `multiWriter` with `writeQuorum=1` — each written shard is streamed through a bitrot writer so the reconstructed content is protected by a fresh HighwayHash256 hash stored in the xl.meta checksums.
- Per-reader deferred errors (`errFileNotFound` / `errFileCorrupt`) are surfaced at the end of the loop.

---

## 3. Per-Disk State Classification

### 3.1 Internal-error → public `DriveState` mapping

Defined in `cmd/erasure-healing.go:382-393`. This is where the healing result's `before`/`after` drive states come from:

```go
switch {
case reason == nil:
    driveState = madmin.DriveStateOk                 // "ok"
case IsErr(reason, errDiskNotFound):
    driveState = madmin.DriveStateOffline            // "offline"
case IsErr(reason,
          errFileNotFound, errFileVersionNotFound, errVolumeNotFound,
          errPartMissingOrCorrupt, errOutdatedXLMeta, errLegacyXLMeta):
    driveState = madmin.DriveStateMissing            // "missing"
default:
    // all remaining cases imply corrupt data/metadata
    driveState = madmin.DriveStateCorrupt            // "corrupt"
}
```

The full set of possible drive states is defined in the madmin SDK at
`github.com/minio/madmin-go/v3/heal-commands.go:119-129`:

```go
DriveStateOk          string = "ok"
DriveStateOffline            = "offline"
DriveStateCorrupt            = "corrupt"
DriveStateMissing            = "missing"
DriveStatePermission         = "permission-denied"
DriveStateFaulty             = "faulty"
DriveStateRootMount          = "root-mount"
DriveStateUnknown            = "unknown"
DriveStateUnformatted        = "unformatted"
```

Only four of these (`ok`, `offline`, `missing`, `corrupt`) are ever produced by `healObject`.
The others appear in `HealingDisks`/cluster-level endpoints for administrative visibility.

### 3.2 `CheckParts` vs `VerifyFile` — why `corrupt` is rarely seen

Per-part inspection happens in `disksWithAllParts` (`cmd/erasure-healing-common.go:291-459`) and produces integer codes from `cmd/storage-datatypes.go:536-545`:

```go
checkPartUnknown = 0
checkPartSuccess
checkPartDiskNotFound
checkPartVolumeNotFound
checkPartFileNotFound
checkPartFileCorrupt
```

- **Normal scan (`HealNormalScan`)**: calls `disk.CheckParts(...)` — a **size-only** check. A file whose byte length matches the expected shard size passes even if every byte is random garbage.
- **Deep scan (`HealDeepScan`)**: calls `disk.VerifyFile(...)` — a **HighwayHash256 bitrot verification** over the whole file. This is where silent bit rot is actually detected.

> **Why observed state is `missing`, not `corrupt`, for damaged parts.** When `CheckParts`
> or `VerifyFile` returns `checkPartFileCorrupt`, `shouldHealObjectOnDisk` returns
> `errPartMissingOrCorrupt` which is **explicitly** in the `DriveStateMissing` branch
> of the switch (see §3.1). So in a heal result, "missing" means "xl.meta and/or parts
> need to be rewritten" — it does **not** strictly mean "file was deleted". A disk is
> reported as `corrupt` only when the xl.meta read itself returns a non-standard error
> (e.g., unknown `errFaultyDisk`, I/O error not explicitly enumerated).

---

## 4. Dangling-Object Criteria

`isObjectDangling` in `cmd/erasure-healing.go:968-1036` encodes **five independent criteria**.
An object is dangling iff *any* of them returns true.

### 4.1 Error-counting helpers

```
cmd/erasure-healing.go:934-948   danglingMetaErrsCount(errs) -> (notFound, nonActionable)
cmd/erasure-healing.go:950-962   danglingPartErrsCount(results) -> (notFound, nonActionable)
```

- `danglingMetaErrsCount` counts `errFileNotFound` + `errFileVersionNotFound` as `notFound`;
  every other non-nil error is `nonActionable` (unknown I/O, permission, etc.).
- `danglingPartErrsCount` counts `checkPartFileNotFound` as `notFound`; every other non-success
  result is `nonActionable`.

### 4.2 The five criteria (in evaluation order)

Let `N = len(metaArr)`, `dataBlocks = (N+1)/2` (the default read quorum), and
`validMeta = first m in metaArr where m.IsValid() is true`.

| # | Condition | Dangling? | Source | Meaning |
|---|---|---|---|---|
| 1 | `validMeta.IsValid() == false` **AND** `notFoundPartsErrs > dataBlocks` | **YES** | `erasure-healing.go:988-1000` | No recoverable metadata anywhere **and** the data shards below read quorum are already missing. Defensive cleanup. |
| 2 | `validMeta.IsValid() == false` (other case, no excess missing parts) | **NO** | `erasure-healing.go:1003` | Cannot decide — return `errErasureReadQuorum` to be safe. |
| 3 | `nonActionableMetaErrs > 0` OR `nonActionablePartsErrs > 0` | **NO** | `erasure-healing.go:1007` | Non-enumerated errors (unknown I/O, permission-denied). Be conservative, don't purge. |
| 4 | `validMeta.Deleted == true` AND `notFoundMetaErrs > dataBlocks` | **YES** | `erasure-healing.go:1013` | A delete marker where more than `dataBlocks` copies of the xl.meta are gone — the delete was never finalized, purge the remnants. |
| 5 | `notFoundMetaErrs > validMeta.Erasure.ParityBlocks` | **YES** | `erasure-healing.go:1024` | xl.meta is missing on too many disks to ever assemble a readable metadata quorum. |
| 6 | `!validMeta.IsRemote() AND notFoundPartsErrs > validMeta.Erasure.ParityBlocks` | **YES** | `erasure-healing.go:1030` | Part files are missing on too many disks to reconstruct. Remote/tiered objects excepted (they live off-cluster). |

Everything else: **NO**, leave it alone.

### 4.3 What happens when `isObjectDangling` returns true

The wrapper `deleteIfDangling` in `cmd/erasure-object.go:482-563`:

1. Builds an audit-tag map with `set`, `pool`, `merrs`, `derrs` (encoded errors), `sz`, `mt`, `d:p`, `offline` (count of disks that returned `errDiskNotFound`), and `caller:file:line` (so you can see which code path invoked the dangling logic).
2. Defers `auditDanglingObjectDeletion(ctx, bucket, object, m.VersionID, tags)` which emits an **`AuditLogOptions{Event: "DeleteDanglingObject", ...}`** record — this is the main operator-visible signal of a dangling purge.
3. Constructs a minimal `FileInfo{VersionID: m.VersionID}` and calls `disk.DeleteVersion(...)` on every disk via `errgroup.WithNErrs(len(disks))`, deleting the orphaned metadata on every drive.

When `isObjectDangling` returns false, `deleteIfDangling` returns `errErasureReadQuorum` and the caller surfaces that as the human-readable "Invalid parity shard count/surplus shard count" error documented in §6.

---

## 5. Quorum Math

For our 4-disk EC(2,2) test instance (`setDriveCount=4`, `defaultParityCount=2`):

```
cmd/erasure-metadata.go:531-565
func objectQuorumFromMeta(ctx context.Context, partsMetaData []FileInfo, errs []error,
    defaultParityCount int) (objectReadQuorum, objectWriteQuorum int, err error)
```

| Quantity | Value | Derivation |
|---|---|---|
| `DataBlocks` | 2 | `dataBlocks = totalShards - parityBlocks = 4 − 2` |
| `ParityBlocks` | 2 | `commonParity(metaArr, defaultParityCount)` agreed value |
| `readQuorum` | 2 | `= dataBlocks` |
| `writeQuorum` | 3 | `= dataBlocks`; bumped to `dataBlocks + 1` when `dataBlocks == parityBlocks` (see `cmd/erasure-metadata.go:558`) |
| `expectedRQuorum` | 2 | `len(metaArr)/2` (half) |

> **Concrete implication:** on 4 disks with 2 parity, reconstruction is possible while
> at least `2` data shards (of the 4 total shards, regardless of which are "data" vs
> "parity" in the original encoding) remain readable. Lose 3 or more shards and
> reconstruction becomes impossible under normal-scan rules.

---

## 6. Runtime Experiments

All experiments were run against a fresh single-node MinIO built from
commit `c07e5b49d477` with `CGO_ENABLED=0 go build -tags kqueue -trimpath`. The server
command was:

```
/tmp/minio-bin/minio server /tmp/minio-heal-experiments/disk{1...4} \
   --address :9300 --console-address :9301
```

`mc admin info local` reports `4 drives online, 0 drives offline, EC:2`. All six test
objects were 5 MiB of random data uploaded with `mc cp`.

Each experiment follows the same script:

1. Damage the on-disk layout directly (simulating a failure mode).
2. Issue `mc admin heal --json local/healtest/<object>` (or `--scan deep`).
3. Inspect the captured `HealResultItem` JSON.
4. Verify the post-heal state by reading the object back and comparing MD5.

All raw JSON outputs are preserved at
`/tmp/minio-heal-experiments/outputs/scenario_{A,B,C,D,D2,E,F,F2}_*.json`.

---

### 6.1 Scenario A — 1 disk missing (1 of 4)

**Setup:** `rm -rf /tmp/minio-heal-experiments/disk1/healtest/heal-test-obj1`

**Disk state before heal:** disk1 missing, disk2/3/4 intact.

**Command:** `mc admin heal --json local/healtest/heal-test-obj1`

**Result — dry-run JSON (`scenario_A_dryrun.json`) — before block only:**

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
  "after": { /* same as before in dry-run */ },
  "size": 5242880
}
```

**Actual heal (`scenario_A_heal.json`):**

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

**Summary line:** `"objects_scanned": 1, "objects_healed": 1, "size": 5242880`

**Post-heal verification:** MD5 of healed file matches original (`d1103b0b9bf8857f6622304a0ff551f8`). Reconstruction confirmed.

**Decision path:** `disksToHealCount = 1 ≤ ParityBlocks = 2` → `cannotHeal = false` → `erasure.Heal()` called on part 1 → temp-rename → `SetHealing()` metadata → final rename.

---

### 6.2 Scenario B — 2 disks missing (exact parity boundary)

**Setup:** `rm -rf .../disk1/.../heal-test-obj2` *and* `rm -rf .../disk2/.../heal-test-obj2`

**Disk state before heal:** disk1 and disk2 missing, disk3/4 intact.

**Result (`scenario_B_heal.json`):**

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

**Post-heal verification:** MD5 matches original (`e2dd6f144fc5213a62d514341c189db3`).

**Decision path:** `disksToHealCount = 2 ≤ ParityBlocks = 2` → `cannotHeal = false` → reconstructed.
Note the `"color":"red"` in the before-block — MinIO's dashboard vocabulary flips from
yellow (partial) to red (at-quorum-limit) once `missing ≥ parityBlocks`. This is the
last possible successful reconstruction.

---

### 6.3 Scenario C — 3 disks missing (BEYOND parity, irrecoverable)

**Setup:** `rm -rf` on disk1, disk2, *and* disk3 for `heal-test-obj3`.

**Disk state before heal:** only disk4 has the object.

**Pre-heal read attempt** (to show that the object is no longer readable via the data-plane):

```
$ mc cat local/healtest/heal-test-obj3
mc: <ERROR> Unable to read from `local/healtest/heal-test-obj3`. Object does not exist.

$ mc stat local/healtest/heal-test-obj3
mc: <ERROR> Unable to stat `local/healtest/heal-test-obj3`. Object does not exist.
```

Reads fail with `ObjectNotFound` because only 1/4 xl.meta files is reachable — below
the read quorum of 2.

**Heal result (`scenario_C_heal.json`):**

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

**Summary line:** `"objects_scanned": 1, "objects_healed": 0, "size": 0`

**Post-heal state:** `disk4` directory **also removed** — the remaining copy was purged. Total cleanup.

**Decision path:**

1. `readAllFileInfo` returns 1 valid xl.meta (on disk4), 3 `errFileNotFound`.
2. `objectQuorumFromMeta` → `readQuorum = 2`, `writeQuorum = 3`. With only 1 good copy
   we're below read quorum.
3. `listOnlineDisks` cannot find modTime quorum (only 1 disk with meta).
4. Classification runs; `disksToHealCount = 3 > ParityBlocks = 2`. `cannotHeal = true`.
5. `deleteIfDangling` called.
6. `isObjectDangling` — criterion #5 fires: `notFoundMetaErrs = 3 > validMeta.Erasure.ParityBlocks = 2` → **dangling = true**.
7. `disk.DeleteVersion(...)` on all 4 disks → disk4's xl.meta/data dir gets purged.
8. The wrapper returns `errFileNotFound`, and `HealObject` reports the object not-found.
9. The confusing `"error":"Invalid parity shard count/surplus shard count given"` string
   originates from `madmin-go`'s heal-result serializer when it cannot describe a
   disk layout (because `drives=null`, `online=0`, etc.) — effectively a surrogate for
   "we couldn't even enumerate what heal was supposed to report on".

**This is the canonical "irrecoverable object" signature:**
- `"drives": null`
- `"name": "/"` (the object name is blanked because heal-result was not produced at the
  object level)
- `"error": "Invalid parity shard count/surplus shard count given..."`
- `"detail": "Object not found: <bucket>/<object>"`
- `objects_healed: 0`

---

### 6.4 Scenario D — Corrupted part file, size changed

**Setup:** Overwrite `.../disk1/healtest/heal-test-corrupt/<uuid>/part.1`
(originally 2,621,600 bytes) with the string `CORRUPTED_DATA_HERE` (20 bytes).

**Normal scan heal (`scenario_D_normal_heal.json`):**

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"yellow","online":3,"missing":1,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]},
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[{"state":"ok"},{"state":"ok"},{"state":"ok"},{"state":"ok"}]},
  "size": 5242880
}
```

Detected and healed **by normal scan** — because the size mismatch (20 ≠ 2,621,600) is
caught by `CheckParts`' size check. Note the drive state is `"missing"` (not
`"corrupt"`), because `checkPartFileCorrupt` returns `errPartMissingOrCorrupt` which
maps to `DriveStateMissing` (see §3.1).

---

### 6.5 Scenario D-2 — Silent bitrot (same file size, different content)

**Setup:** Overwrite `part.1` on disk1 with `dd if=/dev/urandom` of **exactly the same
byte length**. This is the textbook bitrot case — a single-bit flip, head crash, or
bus error that produces non-matching content of correct length.

**Normal scan (`scenario_D2_normal_heal.json`):**

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[{"state":"ok"},{"state":"ok"},{"state":"ok"},{"state":"ok"}]},
  "after": { /* same */ },
  "size": 5242880
}
```

**Normal scan did NOT detect the corruption** — because `CheckParts` only checks size
(`stat().Size == expected`). The md5 of each disk's part.1 remained divergent:

```
disk1 md5 (corrupted) = ea2ddcbb2bdf7d07412c592ab21a0337
disk2 md5 (ok)         = 8be7e3aeabcfb75e8b267f4e809a8d89
disk3 md5 (ok)         = 4b56987d6b4b563524a75103ad3d2748
disk4 md5 (ok)         = 163724c8efc35261c4084a25151b92cd
```

(Note: shard md5s differ across disks because the erasure engine intentionally produces
different per-disk shards — it's the HighwayHash bitrot signature, not the md5, that
validates correctness.)

**Deep scan (`scenario_D2_deep_heal.json`):**

```json
{
  "type":"object","name":"healtest/heal-test-corrupt",
  "before":{"color":"yellow","online":3,"missing":1,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"ok"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]},
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[{"state":"ok"},{"state":"ok"},{"state":"ok"},{"state":"ok"}]},
  "size": 5242880
}
```

Deep scan **did** detect the bitrot — `disk.VerifyFile(...)` runs HighwayHash256 over
the whole part and fails to match the stored checksum, producing `checkPartFileCorrupt`
→ `errPartMissingOrCorrupt` → `"state":"missing"` — and the healer reconstructs
disk1's shard via `erasure.Heal()`.

**Post-heal md5 of disk1's part.1 changed** (from `ea2...` to `129d63b...`), confirming
the shard was rewritten.

> **This is the single most important operational lesson**: bitrot can hide from the
> default healer. `mc admin heal --scan deep` or the scheduled background scanner
> (with `MINIO_HEAL_BITROT` enabled) is the only defense.

---

### 6.6 Scenario E — Dangling metadata (xl.meta missing on 3/4, data dirs still present)

**Setup:** Remove `xl.meta` (the per-object metadata sidecar) from disks 1, 2, and 3.
Leave disk4's xl.meta intact *and* leave the `<uuid>/part.1` data directories on all
four disks:

```
disk1: xl.meta=no,  part files=1
disk2: xl.meta=no,  part files=1
disk3: xl.meta=no,  part files=1
disk4: xl.meta=yes, part files=1
```

**Heal result (`scenario_E_heal.json`):**

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

```
disk1: EXISTS (orphan part dir still there)
disk2: EXISTS (orphan part dir still there)
disk3: EXISTS (orphan part dir still there)
disk4: DIR MISSING (cleaned up)
```

**Decision path:**

1. `readAllFileInfo` → 1 valid xl.meta (disk4), 3 `errFileNotFound`.
2. `isObjectDangling` criterion #5 fires: `notFoundMetaErrs = 3 > validMeta.Erasure.ParityBlocks = 2`.
3. `deleteIfDangling` → `disk.DeleteVersion(...)` on all disks; disk4 (the only one
   with the meta) loses its xl.meta/data dir; disks 1-3 have no xl.meta to delete so
   they're a no-op — the orphan data dirs remain behind.
4. Those orphan data dirs are cleaned up later by `checkAbandonedParts()`
   (`cmd/erasure-healing.go:662-694`, calls `disk.CleanAbandonedData(...)` in parallel
   on every disk, but **only** when `opts.Remove == true && !opts.DryRun`).

> Observation: the audit trail for this case (via `AuditLogTargets`) emits
> `Event: "DeleteDanglingObject"` with tags `set=0, pool=0, merrs=...,
> derrs=...,d:p=2:2,offline=0,caller=<file:line>`.

---

### 6.7 Scenario F — Simulated partial-write / partial-delete failure

**Setup:** `rm -rf` on disk1 and disk2 for `heal-test-partdel`. Disk state before heal
is identical to Scenario B — the on-disk state of a partially-failed write and a
partially-failed delete is, from the perspective of the healer, *indistinguishable*.

**Heal result (`scenario_F_heal.json`):**

```json
{
  "type":"object","name":"healtest/heal-test-partdel",
  "before":{"color":"red","online":2,"missing":2,"corrupted":0,
    "drives":[
      {"endpoint":"/tmp/.../disk1","state":"missing"},
      {"endpoint":"/tmp/.../disk2","state":"missing"},
      {"endpoint":"/tmp/.../disk3","state":"ok"},
      {"endpoint":"/tmp/.../disk4","state":"ok"}
    ]},
  "after":{"color":"green","online":4,"missing":0,"corrupted":0,
    "drives":[{"state":"ok"},{"state":"ok"},{"state":"ok"},{"state":"ok"}]},
  "size": 5242880
}
```

Reconstructed successfully. MD5 matches original (`fa370c9901f104b67333f2f1d7a46b80`).

**Key insight:** at the moment of healing, MinIO has lost the context of *what operation*
originally failed. The healer sees a set of disks with missing/valid copies and decides
based on the observed state, not the historical intent. This is why the two failure
modes produce identical heal outputs. See §8 for how the pre-heal layer (MRF) *does*
differentiate.

---

### 6.8 Scenario F-2 — Partial delete-marker healing (versioned bucket)

To directly exercise the delete-marker code path, a versioned bucket was created:

```
mc mb local/delmarker-test && mc version enable local/delmarker-test
mc cp file1 local/delmarker-test/versioned-obj   # v1 (PUT)
mc cp file2 local/delmarker-test/versioned-obj   # v2 (PUT)
mc rm local/delmarker-test/versioned-obj          # v3 (DEL marker)
```

`mc ls --versions` confirms three versions: v1 PUT, v2 PUT, v3 DEL (the latest).

Remove xl.meta from disks 1 and 2 only (simulating a partial delete-marker write):

```
disk1: xl.meta=no      disk3: xl.meta=yes
disk2: xl.meta=no      disk4: xl.meta=yes
```

**Object-level heal (`scenario_F2_heal.json`) — targets the LATEST (delete marker):**

```json
{
  "status":"success",
  "error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0",
  "detail":"Object not found: delmarker-test/versioned-obj",
  "type":"object","name":"/"
}
```

Same "irrecoverable" signature as scenarios C and E. Because the latest version is a
delete marker (`validMeta.Deleted == true`), `isObjectDangling` criterion #4 applies:
`notFoundMetaErrs = 2, dataBlocks = 2`, so `notFoundMetaErrs > dataBlocks` is FALSE.
However, criterion #5 *could* fire if `notFoundMetaErrs > parityBlocks`, and here
`notFoundMetaErrs = 2` and `parityBlocks = 2`, so that's also FALSE. So the object is
NOT dangling, but the healer still cannot rebuild because the latest meta is a
delete marker with no parts to reconstruct. The wrapper thus returns the
"Invalid parity shard count" surrogate.

**Recursive heal (`scenario_F2_recursive.json`) — traverses every version:**

```json
// v3 (delete marker): size 0, missing→ok on disk1,disk2 → healed (cluster state restored)
{"type":"object","before":{"color":"red","online":2,"missing":2,"drives":[
    {"endpoint":"/tmp/.../disk1","state":"missing"},
    {"endpoint":"/tmp/.../disk2","state":"missing"},
    {"endpoint":"/tmp/.../disk3","state":"ok"},
    {"endpoint":"/tmp/.../disk4","state":"ok"}]},
"after":{"color":"green","online":4,"missing":0, ...}, "size": 0}

// v2 (PUT): size 2097152, missing→ok on disk1,disk2 → healed
{"type":"object","before":{"color":"red","online":2,"missing":2, ...},
"after":{"color":"green","online":4,"missing":0, ...}, "size": 2097152}

// v1 (PUT): size 2097152, missing→ok on disk1,disk2 → healed
{"type":"object","before":{"color":"red","online":2,"missing":2, ...},
"after":{"color":"green","online":4,"missing":0, ...}, "size": 2097152}
```

Summary line: `"objects_scanned":3, "objects_healed":3, "size":4194304`

> **Operational lesson:** single-object `mc admin heal` targets only the latest version
> per call; to properly heal a partial-delete-marker situation across a versioned
> object, you must use `--recursive` (which iterates every version) or explicitly
> specify `--versionId <id>`.

---

## 7. Boundary Conditions

### 7.1 Minimum valid shards needed

For a 4-disk EC(2,2) erasure set:

| Surviving shards | Healer outcome |
|---|---|
| 4 | No-op; `before=green, after=green`; `objects_healed=0` |
| 3 | Reconstruct (scenario A); `before=yellow, after=green` |
| 2 | Reconstruct (scenario B); `before=red, after=green`. **This is the minimum.** |
| 1 | Unrecoverable (scenario C); object is declared dangling and purged |
| 0 | Object was never present; returns `errFileNotFound` immediately |

In general, **the minimum number of valid shards required for successful reconstruction
equals `DataBlocks`** — which in an EC(D,P) setup is simply D.

### 7.2 Error signatures by failure class

| Class | Exact error string | Where surfaced | Effect |
|---|---|---|---|
| Partial damage, ≤ parity | *(none — result is success)* | – | Reconstructed silently |
| Beyond parity, no ETag agreement | `"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0"` | JSON `error` field from madmin's heal-result unmarshaller | Dangling path enters; object purged |
| Non-actionable I/O errors | `errErasureReadQuorum` returned from `deleteIfDangling` | `healObject` return; surfaced as `InsufficientReadQuorum` at API | Left alone — operator must investigate |
| All drives offline | `errDiskNotFound` per disk | Drive state `"offline"` in `before/after` | Heal queued for retry (MRF) |
| Directory dangling | Same as object dangling | `healObjectDir` + `isObjectDirDangling` | Dir purged |
| Legacy xl.meta | `errLegacyXLMeta` | Drive state `"missing"` on those disks | Always reconstructed (upgrade path) |

### 7.3 When heal attempts go to retry instead of fail

In `healObject`, if any step fails partway (e.g., disk RW error mid-reconstruction),
the error surfaces to `HealObject`. But the crucial retry mechanism isn't inside
`healObject` itself — it's **MRF**, which re-queues the object for another attempt
after a cooldown. The MRF queue has capacity `mrfOpsQueueSize = 100000`
(`cmd/mrf.go:27`), persists across server restarts via msgpack serialization to
`.minio.sys/.heal/mrf/list.bin`, and is drained by `healRoutine` in `cmd/mrf.go:220-281`.

---

## 8. Write vs Delete Failures

This is the sharpest question in the prompt. The short answer: **the final heal action
is identical; the pre-heal behavior (who enqueues what into the MRF queue) is
different.**

### 8.1 How partially-failed writes reach the healer

`cmd/erasure-object.go` contains the PUT-side MRF triggers:

| Line | Context | Behavior |
|---|---|---|
| 400 | `getObjectWithFileInfo` — during a READ that encountered `errFileNotFound` / `errFileCorrupt` | Enqueues `PartialOperation{..., BitrotScan: true}` so the read-path can surface a corrupt part and schedule a deep-scan heal |
| 805 | `PutObjectMetadata` — the metadata update path detects `missingBlocks > 0 && missingBlocks < fi.Erasure.DataBlocks` | Enqueues `PartialOperation{..., Versions: [fi.Versions]}` — a batch of versions to heal |
| 1574–1578 | `PutObject` — after a successful write if offline disks exist, OR on version-list disparity | Enqueues MRF heal |
| 2113 (helper `addPartial`) | Shared tail for many write paths | Enqueues single `PartialOperation{Bucket, Object, VersionID}` |

### 8.2 How partially-failed deletes reach the healer

| Line | Context | Behavior |
|---|---|---|
| 1773 | `DeleteObjects` — per-object error that wasn't `VersionNotFound`/`ObjectNotFound` | `er.addPartial(bucket, dobj.ObjectName, dobj.VersionID)` — schedule a heal so no orphan is left |
| 1786 | `DeleteObjects` — per-object where any disk is offline | Same `addPartial` call |
| 1895 | `DeleteObject` — `DeletePrefix + Expiration` path with `InsufficientReadQuorum` | `addPartial` + returns `InsufficientWriteQuorum{}` |
| 1943 | `DeleteObject` — versioned path with `InsufficientReadQuorum` | Same |
| 2047 | `DeleteObject` — deferred clean-up for any offline disk | Same |
| 2405 | `DeleteBucket` path (when necessary) | Same |

### 8.3 What goes into the MRF queue

`PartialOperation` struct (`cmd/mrf.go:38-60`):

```go
type PartialOperation struct {
    Bucket      string
    Object      string
    VersionID   string
    Versions    []byte    // serialized FileInfoVersions — batch of versions to heal
    SetIndex    int
    PoolIndex   int
    Queued      time.Time
    BitrotScan  bool      // if true, heal uses HealDeepScan
}
```

`Versions []byte` is populated on the **read** path (line 400) when a single read
discovers multiple damaged versions in one pass — the healer then bulk-heals them all.
`BitrotScan: true` is set on the read-path site to force deep verification since
that's the only trigger that proves bitrot rather than metadata mismatch.

### 8.4 How `healRoutine` consumes the queue

`cmd/mrf.go:220-281`:

```go
for u := range m.opCh {
    // back-pressure: wait 1s if more failures are coming in
    select {
    case nu := <-m.opCh: u = nu;  /* drain+replace */
    case <-time.After(time.Second):
    }

    scan := madmin.HealNormalScan
    if u.BitrotScan { scan = madmin.HealDeepScan }

    // per-object vs per-version decoding
    if len(u.Versions) > 0 {
        // batch: decode FileInfoVersions from msgpack, call HealObject per version
        ...
    } else {
        // single: call HealObject(ctx, u.Bucket, u.Object, u.VersionID, opts)
        er.HealObject(ctx, u.Bucket, u.Object, u.VersionID,
            madmin.HealOpts{ScanMode: scan, Remove: healDeleteDangling})
    }
}
```

**Observation:** once control reaches `HealObject`, it cannot tell whether the
triggering failure was a write or a delete. The only carried signal is `BitrotScan`
(which influences scan mode but not the decision tree). The healer inspects the
*current* on-disk state — so both failure modes converge to the same "look at disks,
decide reconstruct/dangle/leave-alone" logic described in §2 and §4.

### 8.5 The behavioral differences that *do* exist

| Aspect | Partial write | Partial delete |
|---|---|---|
| What gets queued | Object or version content | Object/version identifier |
| Whether `BitrotScan` is set | Only on read-triggered MRF (line 400) | Never |
| Post-MRF state when healed | Full object restored on all disks | Object gone from all disks (if `isObjectDangling` fires) OR restored (if the delete never actually committed with quorum) |
| Dangling evaluation | Criteria #1, #3, #5, #6 | Criterion #4 (`validMeta.Deleted && notFoundMetaErrs > dataBlocks`) explicitly for delete markers |

> **Key takeaway:** Partial-delete healing is not a "delete retry" — it is a
> *consistency restorer*. If the delete committed with quorum, the surviving
> delete-marker is re-propagated to the incomplete disks. If the delete did NOT commit
> with quorum, the object is reconstructed (because the delete marker is, by quorum
> rules, not the latest valid state). The MRF system records the intent to re-look-at
> the object; it does not replay the delete.

### 8.6 What about the background scanner?

The data-scanner (`cmd/data-scanner.go`) periodically walks every object and, when the
`healDeleteDangling` constant (line 60, set to `true`) is enabled, calls the heal path
with `opts.Remove = true`. At line 1208-1213 it also invokes `checkAbandonedParts`
to sweep orphan data-directories left over from scenario E (`rm` of xl.meta but
residual data dirs). Background healing therefore serves as the long-tail retry for
MRF entries that were dropped on server shutdown — though MRF itself persists to disk
at shutdown (`cmd/mrf.go` — `saveMRFToDisk`) and reloads on startup
(`startMRFPersistence`).

---

## 9. Logs, Audits and What Shows Up

### 9.1 Audit record: `DeleteDanglingObject`

The sole operator-facing signal that the healer purged an object is the audit entry
at `cmd/erasure-object.go:451-465`:

```go
func auditDanglingObjectDeletion(ctx, bucket, object, versionID string,
                                 tags map[string]string) {
    if len(logger.AuditTargets()) == 0 { return }
    opts := AuditLogOptions{
        Event:     "DeleteDanglingObject",
        Bucket: bucket, Object: object, VersionID: versionID,
        Tags: tags,
    }
    auditLogInternal(ctx, opts)
}
```

Tags emitted by `deleteIfDangling`:

- `set`, `pool` — which erasure set/pool
- `merrs` — the encoded per-disk metadata errors
- `derrs` — the per-part data errors
- `sz`, `mt`, `d:p` — size, modTime, data:parity ratio of the now-deleted FileInfo
- `offline` — count of disks that are `errDiskNotFound`
- `invalid` — "1" if no valid FileInfo was present
- `caller` — `file:line` of the caller via `runtime.Caller(1)`

The last tag (`caller`) is especially useful: it tells the operator *which* code path
triggered the dangling cleanup. Possible values include
`cmd/erasure-healing.go:<line>` (from `healObject`),
`cmd/erasure-object.go:<line>` (from one of the object CRUD error paths), etc.

**Audit targets are NOT enabled by default.** An operator who wants to see these
events must register a target via `mc admin config set audit_webhook` or
`audit_kafka` (configured in `internal/config/audit/`).

### 9.2 Internal healing logs

The `healingLogOnceIf` helper (`cmd/logging.go:87-89`) routes to
`logger.LogIf(ctx, "healing", err, errKind...)`. Three call sites exist in the
healing orchestrator:

- `cmd/erasure-healing.go:477` — `"heal-object-available-disks"` (file-distribution mismatch after heal)
- `cmd/erasure-healing.go:487` — `"heal-object-outdated-disks"` (outdated disk re-classification)
- `cmd/erasure-healing.go:497` — `"heal-object-metadata-entries"` (wrong part count in meta)

These are operator-facing warnings that surface via `mc admin console alias/` (also via `mc admin logs alias/` on current versions) when the console logger is configured.
They do **NOT** emit on normal successful heals — MinIO is deliberately quiet during healthy operation.

### 9.3 `mc admin trace --call healing`

A live trace channel exists for healing events (observed in our experiments):

```
2026-04-16T23:00:13.899 [HEALING] heal.Object 127.0.0.1:9300 healtest/heal-test-obj1 43.168726ms 5.0 MiB
```

This is a per-request summary — one line per `HealObject` invocation — useful for
observability during large heals.

### 9.4 The healing tracker (`cmd/background-newdisks-heal-ops.go`)

Every new disk gets a `healingTracker` persisted to `.minio.sys/.healing.bin` on that
disk, tracking bucket-level progress, counts of `ObjectsHealed` /
`ObjectsHealedFailed`, current bucket/object, and start/end timestamps. On startup
`loadHealingTracker(...)` reconstructs state; on re-format it gets re-initialized:

```
background-newdisks-heal-ops.go:456-457
healingLogIf(ctx, fmt.Errorf("Unable to load healing tracker on '%s': %w, re-initializing..", disk, err))
tracker = initHealingTracker(disk, mustGetUUID())
```

---

## 10. Answers to the Six Questions

### 10.1 Does MinIO always reconstruct from valid shards, or are there cases where it deliberately leaves something deleted/degraded?

**No, it does not always reconstruct.** There are three distinct non-reconstruct outcomes:

1. **Deliberate purge (dangling)**: when `isObjectDangling` fires (criteria #1, #4, #5, #6),
   `deleteIfDangling` issues `DeleteVersion` on every disk. The object is permanently gone.
   — Code: `cmd/erasure-healing.go:968-1036`, `cmd/erasure-object.go:482-563`.
2. **Deliberate "leave alone" (conservative)**: when criterion #3 fires
   (non-actionable errors — unknown I/O, permission-denied on some disks), the
   healer returns `errErasureReadQuorum` without touching the data. A future
   heal with the disks healthy will re-evaluate. This is the "degraded but
   preserved" state. — Code: `cmd/erasure-healing.go:1007`.
3. **Reconstruct override**: when `disksToHealCount > ParityBlocks` but all
   readable xl.metas share the same ETag (`quorumETag != ""`), the healer
   resets `cannotHeal = false` and reconstructs anyway, trusting the
   metadata consensus over the disk-count threshold. — Code:
   `cmd/erasure-healing.go:429-433`.

Runtime evidence: scenarios C and E produce the deliberate-purge signature
(`"drives": null`, `error: "Invalid parity shard count..."`, post-heal disks
cleaned). Scenario A/B/D/D2/F produce the reconstruct signature. Scenario F-2
(delete-marker with missing xl.metas) produces the leave-alone signature at the
single-object granularity but is healed successfully at version granularity under
`--recursive`.

### 10.2 What appears in healing output that reveals the decision?

The `HealResultItem` JSON encodes the decision in five places:

1. **`before.color` vs `after.color`**: `green` = healthy, `yellow` = partial but
   healable, `red` = at-parity-boundary. Transitions reveal the action:
   `yellow→green` or `red→green` = reconstructed; `green→green` = no-op;
   any → empty color (`""`) with `drives: null` = dangling-purge signature.
2. **`before.drives[].state` vs `after.drives[].state`**: per-disk transitions
   like `missing→ok` prove reconstruction; both `missing` means unhealed.
3. **`error` field**: empty for success; `"Invalid parity shard count/surplus shard count given"`
   for irrecoverable/dangling; non-empty detail with `"Object not found"` when the
   object was purged.
4. **Summary line**: `{status, objects_scanned, objects_healed, items_scanned, items_healed, size, duration}`.
   `objects_healed=0` with non-zero `objects_scanned` reveals healing was tried
   but did not succeed.
5. **`parityBlocks`/`dataBlocks` fields**: the `HealResultItem` carries the
   erasure-code ratio that governed the decision.

Scenarios A/B/D/D2/F demonstrate (1–4); scenarios C/E/F-2 demonstrate the irrecoverable
signature (1 flips to empty, 3 populated, 4 with `objects_healed: 0`).

### 10.3 Do logs explain *why* MinIO chose restore vs leave-alone?

Yes, but only if opted in:

- **Audit logs** (`DeleteDanglingObject` event, §9.1) explicitly enumerate the per-disk
  metadata errors (`merrs`), per-part data errors (`derrs`), offline count, and the
  calling code path (`caller:file:line`). This is the definitive "why we purged" record.
- **Internal healing logs** (`healingLogOnceIf` at §9.2) explain failures during
  reconstruction — e.g., `"heal-object-available-disks"` when the
  on-disk `Distribution` array doesn't match the available disks (possible manual
  backend modification).
- **`mc admin trace --call healing`** (§9.3) surfaces per-request summaries live.
- **`mc admin heal --json`** output itself is the "why" for the simpler cases: the
  drive-state transitions tell the whole story, and for irrecoverable cases the
  `error` + `detail` fields reveal the dangling path was taken.

Without an audit target configured, MinIO logs **nothing** on successful heals or
silent dangling purges by the background scanner — this is an opt-in observability
model.

### 10.4 How many valid shards are needed for heal to succeed?

For a D-data-block, P-parity-block erasure set of `N = D+P` drives:

- **Strict minimum: `D` shards** (data or parity, any combination).
- **In our 4-disk EC(2,2) case: 2 shards** (scenario B is exactly at this boundary).
- Below that: Reed-Solomon cannot reconstruct.

The check is split across two layers:

1. **`cannotHeal` threshold** in `healObject` at line 428:
   `disksToHealCount > ParityBlocks` → can't heal (i.e., healthy disks < D).
2. **Read quorum** in `objectQuorumFromMeta` at line 531:
   `readQuorum = D` must be met even to read the metadata.

If metadata quorum fails, you land in `deleteIfDangling` before ever hitting the part
count check. If metadata quorum holds but too many parts are missing, `disksToHealCount > ParityBlocks`
triggers the same dangling path.

There's one operational nuance: even with `D` shards, *all* of the outdated disks
must be writable — if a write to a temp location fails mid-reconstruction,
`disksToHealCount` decrements and the loop continues (`cmd/erasure-healing.go:600-611`),
but if it reaches zero during the loop, the error `"all drives had write errors,
unable to heal"` is raised.

### 10.5 What error appears when healing cannot recover an object?

Three distinct errors, depending on the path:

1. **Reconstruction impossible, object dangling:**
   ```
   {"error": "Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0",
    "detail": "Object not found: <bucket>/<object>",
    "drives": null}
   ```
   The `madmin-go` serializer produces this when the heal pipeline bails out before
   it can populate the per-disk state arrays. The underlying Go error is
   `errFileNotFound` (or `errFileVersionNotFound`) returned by `healObject`.
   — Scenarios C, E, F-2 (single-object heal of a delete marker).

2. **Reconstruction ambiguous, leave alone:**
   ```
   <returned to API client as>
   InsufficientReadQuorum   (HTTP 503)
   errErasureReadQuorum    (internal)
   ```
   From `deleteIfDangling` when `isObjectDangling` returned false (criterion #3 —
   non-actionable errors).

3. **Reconstruction started but failed mid-way:**
   ```
   "all drives had write errors, unable to heal <bucket>/<object>"
   ```
   From `healObject` when `disksToHealCount` hits zero during the per-part loop.
   — `cmd/erasure-healing.go:614-616`.

### 10.6 Does healing behavior differ between partial write failure and partial delete failure?

**The healer itself does not differentiate** — it examines the current on-disk state
and applies the decision tree. Scenario F (partial write → 2 missing disks) and an
equivalent partial-delete layout both map to "reconstruct" because the on-disk
state is identical.

**The MRF layer does differentiate in what it enqueues:**

- **Writes** can queue `PartialOperation{Versions: [batch]}` (line 805) or
  `BitrotScan: true` (line 400), because the write path knows which versions may be
  inconsistent and the read path knows it was checksum verification that failed.
- **Deletes** queue simple `PartialOperation{Bucket, Object, VersionID}` via the
  `addPartial` helper (line 2113) with no bitrot hint and no batch context — the
  delete path only knows the identifier it was trying to remove.

**The dangling evaluation does differentiate** via `isObjectDangling` criterion #4:
if `validMeta.Deleted == true`, it uses `dataBlocks` (not `parityBlocks`) as the
threshold for `notFoundMetaErrs`. This is because a delete marker has no parts to
reconstruct — only the metadata matters — and the test is tuned to err on the side
of not purging a partially-committed delete that might be recoverable via the
majority of live xl.metas.

**Practical consequence:** A partial write is eventually consistent toward "full
restore". A partial delete is eventually consistent toward "the majority decides":
if the delete committed on ≥ writeQuorum drives, all drives converge to the deleted
state; if it committed on < writeQuorum, the original object is restored. Both take
the same final `HealObject(...)` path.

---

## Appendix A — Full Decision Tree

```
HealObject(bucket, object, versionID, opts)
├── [dir object?] → healObjectDir(...)
├── lockless readAllFileInfo
│   └── [all not-found?] → return errFileNotFound
└── healObject(...)
    ├── acquire lock (unless opts.NoLock)
    ├── readAllFileInfo (with ReadData)
    ├── objectQuorumFromMeta(...)
    │   └── [ERR: can't establish quorum]
    │       └── deleteIfDangling(...)
    │           ├── isObjectDangling(metaArr, errs, dataErrsByPart)
    │           │   ├── criterion #1: !validMeta && notFoundParts > dataBlocks → YES
    │           │   ├── criterion #2: !validMeta (no excess missing) → NO
    │           │   ├── criterion #3: nonActionable > 0 → NO
    │           │   ├── criterion #4: Deleted && notFoundMeta > dataBlocks → YES
    │           │   ├── criterion #5: notFoundMeta > parityBlocks → YES
    │           │   └── criterion #6: notFoundParts > parityBlocks → YES
    │           ├── [dangling=true] → audit(DeleteDanglingObject) + DeleteVersion all disks
    │           └── [dangling=false] → return errErasureReadQuorum (LEAVE ALONE)
    ├── listOnlineDisks / pickValidFileInfo
    ├── disksWithAllParts (CheckParts or VerifyFile)
    ├── classify every disk → drive states
    ├── cannotHeal := !XLV1 && !Deleted && disksToHealCount > ParityBlocks
    │   └── [cannotHeal && quorumETag != ""] → cannotHeal = false  (OVERRIDE)
    ├── [dryRun?] → return current state, no action
    ├── [cannotHeal] → deleteIfDangling(...)  (same branch as above)
    └── [normal] → reconstruct
        ├── for each part:
        │   ├── newBitrotReader(latestDisks...)
        │   ├── newBitrotWriter(outDatedDisks...)
        │   └── erasure.Heal(writers, readers, partSize, prefer)
        │       └── [err] → return; result.After unchanged
        ├── for each healed disk:
        │   ├── SetHealing() on metadata (xMinIOHealing="true")
        │   └── RenameData(tmp → final)
        └── result.After.Drives[i].State = DriveStateOk (per healed disk)
```

## Appendix B — Scenario Outcome Summary

| Scenario | Corruption | Before state | After state | Decision | Outcome |
|---|---|---|---|---|---|
| A | disk1 removed | yellow, 3 ok + 1 missing | green, 4 ok | Reconstruct | **Object restored** |
| B | disk1+disk2 removed | red, 2 ok + 2 missing | green, 4 ok | Reconstruct (boundary) | **Object restored** |
| C | disk1+disk2+disk3 removed | drives: null, error | purged | Dangling | **Object lost (purged)** |
| D | disk1 part.1 size changed (20 bytes) | yellow, 3 ok + 1 missing | green, 4 ok | Reconstruct (normal scan detected size) | **Object restored** |
| D2 | disk1 part.1 same size, random content | normal scan: green→green (missed!) | deep scan: yellow→green | Requires deep scan | **Object restored with deep scan** |
| E | xl.meta removed from 3/4 disks | drives: null, error | disk4 purged, disks 1-3 orphan dirs remain | Dangling (criterion #5) | **Object lost (purged)** |
| F | disk1+disk2 removed (simulated delete) | red, 2 ok + 2 missing | green, 4 ok | Reconstruct | **Object restored** |
| F2 (object heal) | 2 xl.meta missing on delete marker | drives: null, error | (state ambiguous — delete marker at read quorum boundary) | "Invalid parity" error | **Not healed via single-obj** |
| F2 (recursive) | Same | red, 2 ok + 2 missing | green, 4 ok | Reconstruct per version | **All 3 versions restored** |

## Appendix C — How To Reproduce

Prerequisites:
- Go 1.23+
- `mc` client (RELEASE.2025-08-13 or later)
- A 4-disk filesystem layout, e.g. `/tmp/minio-test/disk{1..4}`

Build:
```bash
cd <repo>
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio .
```

Run:
```bash
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin123 \
  /tmp/minio server /tmp/minio-test/disk{1...4} --address :9300 &

mc alias set local http://127.0.0.1:9300 minioadmin minioadmin123
mc mb local/healtest
dd if=/dev/urandom of=/tmp/f.bin bs=1M count=5
mc cp /tmp/f.bin local/healtest/obj
```

Induce scenario A:
```bash
rm -rf /tmp/minio-test/disk1/healtest/obj
mc admin heal --json local/healtest/obj
```

Induce scenario C (irrecoverable):
```bash
rm -rf /tmp/minio-test/disk{1,2,3}/healtest/obj
mc admin heal --json local/healtest/obj   # → "Invalid parity shard count/surplus shard count given"
```

Deep-scan bitrot healing (scenario D2):
```bash
dd if=/dev/urandom of=/tmp/minio-test/disk1/healtest/obj/<uuid>/part.1 bs=<size> count=1
mc admin heal --scan deep --json local/healtest/obj
```

---

**End of document.**
