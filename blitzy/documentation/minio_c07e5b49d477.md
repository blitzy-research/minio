# How MinIO's Healing Subsystem Resolves an Ambiguous 4‑Disk (EC:2) Object — Reconstruct, Leave‑as‑is, or Purge

> **Onboarding question answered here:** *When a 4‑disk erasure‑coded MinIO deployment reaches an ambiguous state (some drives hold valid shards, some corrupted, some empty), how does the healing subsystem decide whether to **reconstruct**, **leave‑as‑is**, or **purge** the object? Give actual runtime evidence of conflict resolution, not theory.*

This is an **evidence‑backed** answer. Every behavioral claim below is paired with the exact command that produced it and its **full, unedited output**, and is grounded in a `file:line` reference into the MinIO source at commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`. Anything not directly observed at runtime is explicitly labelled **(inferred)**.

---

## 1. TL;DR — the direct answer

**MinIO does _not_ always reconstruct.** The healing decision is a *deterministic* function of **how many drives need healing versus the object's parity count**, producing exactly **three** outcomes:

| Outcome | When it happens (EC:2 ⇒ parity = 2) | Observed signal |
|---|---|---|
| **Reconstruct** | `disksToHealCount ≤ parity` (≥ `dataBlocks`=2 valid shards remain) | `HealObject` returns `err == nil`; per‑drive state goes `missing`/`corrupt` → `ok`; object preserved |
| **Leave‑as‑is** | Too few shards to rebuild **but** the object is *not* provably dangling (non‑actionable errors, e.g. corruption) | `HealObject`/`deleteIfDangling` returns `errErasureReadQuorum` ("Read failed. Insufficient number of drives online"); object untouched on disk |
| **Purge** | Too few shards to rebuild **and** the object *is* dangling (remnant that can never reach quorum) | Dangling remnant is `DeleteVersion`‑ed on all disks, a `DeleteDanglingObject` audit event fires, and `HealObject` returns `errFileNotFound` / `errFileVersionNotFound` |

The single determinative branch is one expression in `healObject` — `cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks` [`cmd/erasure-healing.go:428`]. When it is `false`, MinIO reconstructs; when it is `true`, MinIO delegates to the dangling handler, which either purges (if `isObjectDangling` is `true`) or leaves the object alone (returning read‑quorum error).

The rest of this document proves each of the three outcomes with real output, derives the EC:2 boundary numbers from runtime observation, shows the `Before`/`After` decision signals, captures the audit "why", and demonstrates the write‑vs‑delete divergence.

---

## 2. Environment & how to reproduce

**Container / toolchain (as used):**

- Canonical build/run container image: `andrewparkscaleai/coding-agent:minio__minio__c07e5b49d477b0774f23db3b290745aef8c01bd2` (from `ghcr.io/scaleapi/swe-atlas:swe_atlas_QnA_minio_minio_1.0`).
- Go toolchain: `go 1.23` [`go.mod:3`], CI‑pinned to `1.23.2`. Observed at runtime: `go version go1.23.2 linux/amd64`.
- Key deps (unchanged, read‑only): `github.com/klauspost/reedsolomon v1.12.4` [`go.mod`], `github.com/minio/madmin-go/v3 v3.0.77` [`go.mod`].
- Repo root (module `github.com/minio/minio`): `/tmp/blitzy/minio/blitzy-629729d3-cb15-4820-bb3e-f65517d9c2ea_444e13`.
- `mc` (MinIO Client): `RELEASE.2025-08-13T08-35-41Z` at `/usr/local/bin/mc`.

**Build the server _outside_ the repo** (`.gitignore` line 4 is `minio`, so the binary must not land in the tree):

```bash
$ CGO_ENABLED=0 go build -tags kqueue -o /tmp/minio_bin/minio .
# exit 0
$ ls -la /tmp/minio_bin/minio
-rwxr-xr-x 1 root root 156750807 Jul  6 22:57 /tmp/minio_bin/minio
$ file /tmp/minio_bin/minio
/tmp/minio_bin/minio: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, Go BuildID=7GA04ogtF4TmoEsk-bwo/4Y4tIFWU6Z-P_rI1k6PT/nlYUf3rsMRguGv2zsbsM/BkpoFBDA4XpXwnff4GaH, with debug_info, not stripped
$ /tmp/minio_bin/minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.2 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

> The version string is `DEVELOPMENT.GOGET` because we built with a plain `go build` rather than the Makefile's `gen-ldflags` target; this is expected and does not affect the healing behavior. The build command matches the Makefile build target `CGO_ENABLED=0 go build -tags kqueue` [`Makefile:177-179`].

**Two complementary observation harnesses were used** — both exercise the *real* `ObjectLayer.HealObject` entry point [`cmd/object-api-interface.go:300`], never a synthetic stand‑in:

1. **In‑repo Go heal tests** (deterministic, no cluster):
   ```bash
   $ CGO_ENABLED=0 go test -v -run 'TestHeal|TestIsObjectDangling' -tags kqueue -timeout 900s ./cmd/
   ```
   Plus a **temporary scratch test** (`cmd/zz_blitzy_scratch_test.go`, since deleted — see §14) built on the repo's own verified helper `prepareErasure(ctx, 4)` [`cmd/test-utils_test.go:211`], which yields a **clean 4‑disk EC:2** object layer (the exact topology the question asks about; the shipped `TestHeal*` tests use 16/32‑disk sets — see §5 caveat).

2. **Live 4‑drive single‑node server** (`ErasureSetupType`) driven by the operator CLI (an audit‑webhook sink on `:9500` captures the `DeleteDanglingObject` event of §7):
   ```bash
   $ MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
     MINIO_AUDIT_WEBHOOK_ENABLE_blitzy=on MINIO_AUDIT_WEBHOOK_ENDPOINT_blitzy=http://127.0.0.1:9500/ \
     /tmp/minio_bin/minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address :9000 --console-address :9001 &
   # server log: "INFO: Formatting 1st pool, 1 set(s), 4 drives per set."
   $ mc --config-dir /tmp/mc-config alias set local http://127.0.0.1:9000 minioadmin minioadmin
   $ mc --config-dir /tmp/mc-config admin heal --json --force local/<bucket>/<object>
   ```

**Fault‑injection recipe** (the repo's own technique, from `cmd/erasure-healing_test.go`):

- **Missing shard** → delete the metadata file: `os.RemoveAll(path.Join(drive, bucket, object, "xl.meta"))` (live: `rm -f /tmp/dN/<bucket>/<object>/xl.meta`). Classified `Missing`.
- **Corrupt shard** → overwrite a part with junk: `os.WriteFile(path.Join(dir, "part.1"), []byte("some data"), 0o644)`. Classified `Corrupt` under a deep scan (bitrot/HighwayHash detection).
- **Heal call**: `HealObject(ctx, bucket, object, "", madmin.HealOpts{ScanMode: <HealNormalScan|HealDeepScan>, Remove: <true|false>})`.
- Sweep the count of damaged drives **1 → 3** to straddle the EC:2 parity boundary (parity = 2).

---

## 3. Canonical configuration & boundary math (derived, not assumed)

A 4‑drive set defaults to **EC:2** — 2 data + 2 parity — because `DefaultParityBlocks(4)` returns `2`:

```go
// internal/config/storageclass/storage-class.go:355
func DefaultParityBlocks(drive int) int {
	switch drive {
	case 1:
		return 0
	case 3, 2:
		return 1
	case 4, 5:        // ← 4 drives ⇒ parity 2
		return 2
	case 6, 7:
		return 3
	default:
		return 4
	}
}
```

The read/write quorum is computed by `objectQuorumFromMeta` [`cmd/erasure-metadata.go:531`]:

```go
// cmd/erasure-metadata.go:555-564
dataBlocks := len(partsMetaData) - parityBlocks

writeQuorum := dataBlocks
if dataBlocks == parityBlocks {
	writeQuorum++
}

// Since all the valid erasure code meta updated at the same time are equivalent, pass dataBlocks
// from latestFileInfo to get the quorum
return dataBlocks, writeQuorum, nil
```

For 4 disks with parity 2: `dataBlocks = 4 − 2 = 2`; since `dataBlocks == parityBlocks`, `writeQuorum = 2 + 1 = 3`. **⇒ read quorum = 2, write quorum = 3.**

**These values were _observed at runtime_, not assumed.** From the EC:2 scratch harness:

```text
==================== BLITZY ENV: set topology ====================
BLITZY set drive count = 4, defaultParityCount = 2

==================== BLITZY QUORUM (observed) ====================
BLITZY fi.Erasure.DataBlocks   = 2
BLITZY fi.Erasure.ParityBlocks = 2
BLITZY objectQuorumFromMeta => readQuorum=2 writeQuorum=3 err=<nil>
```

Every `madmin.HealResultItem` emitted below carries `"parityBlocks": 2, "dataBlocks": 2, "diskCount": 4`, and the live server independently confirms EC:2:

```text
$ mc --config-dir /tmp/mc-config admin info local
●  127.0.0.1:9000
   Uptime: 7 minutes
   Version: <development>
   Network: 1/1 OK
   Drives: 4/4 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.7% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

4.0 MiB Used, 1 Bucket, 4 Objects
4 drives online, 0 drives offline, EC:2
```

These two numbers — **read quorum 2** and **parity 2** — frame every boundary answer that follows. Corroboration (documentation, supplementing the runtime evidence): MinIO documents that when parity `M` is exactly half the erasure‑set size the write quorum is `K+1`, matching the observed `writeQuorum = 3` for EC:2; and that an object which has lost read quorum can no longer be reconstructed.

---

## 4. The decision, distilled (with the enclosing code)

`HealObject` (the real `ObjectLayer` entry point) reaches `healObject` [`cmd/erasure-healing.go:258`], which reads metadata from all four disks, computes quorum, classifies each disk, and then takes the determinative `cannotHeal` branch.

**Step 1 — classify each drive** into one of four states [`cmd/erasure-healing.go:382-404`]:

```go
// cmd/erasure-healing.go:382-404
driveState := ""
switch {
case reason == nil:
	driveState = madmin.DriveStateOk
case IsErr(reason, errDiskNotFound):
	driveState = madmin.DriveStateOffline
case IsErr(reason, errFileNotFound, errFileVersionNotFound, errVolumeNotFound, errPartMissingOrCorrupt, errOutdatedXLMeta, errLegacyXLMeta):
	driveState = madmin.DriveStateMissing
default:
	// all remaining cases imply corrupt data/metadata
	driveState = madmin.DriveStateCorrupt
}

result.Before.Drives = append(result.Before.Drives, madmin.HealDriveInfo{
	UUID:     "",
	Endpoint: storageEndpoints[i].String(),
	State:    driveState,
})
result.After.Drives = append(result.After.Drives, madmin.HealDriveInfo{
	UUID:     "",
	Endpoint: storageEndpoints[i].String(),
	State:    driveState,
})
```

Note that `Before.Drives` and `After.Drives` are **seeded identically**; only a successful reconstruction later upgrades individual `After` entries to `ok` (Step 3). The `madmin` drive‑state constants serialize to lowercase JSON: `ok`, `offline`, `corrupt`, `missing` (`madmin-go/v3/heal-commands.go`).

**Step 2 — the determinative branch** [`cmd/erasure-healing.go:428-455`]:

```go
// cmd/erasure-healing.go:428
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
if cannotHeal && quorumETag != "" {
	// This is an object that is supposed to be removed by the dangling code
	// but we noticed that ETag is the same for all objects, let's give it a shot
	cannotHeal = false
}

if cannotHeal {
	// Allow for dangling deletes, on versions that have DataDir missing etc.
	// this would end up restoring the correct readable versions.
	m, err := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs, dataErrsByPart, ObjectOptions{
		VersionID: versionID,
	})
	errs = make([]error, len(errs))
	if err == nil {
		err = errFileNotFound
		if versionID != "" {
			err = errFileVersionNotFound
		}
		// We did find a new danging object
		return er.defaultHealResult(m, storageDisks, storageEndpoints,
			errs, bucket, object, versionID), err
	}
	for i := range errs {
		errs[i] = err
	}
	return er.defaultHealResult(m, storageDisks, storageEndpoints,
		errs, bucket, object, versionID), err
}
```

**Step 3 — reconstruct** (when `cannotHeal == false`): rebuilt shards are written with `RenameData`, and the healed drive's `After` state is upgraded to `ok` [`cmd/erasure-healing.go:649-653`]:

```go
// cmd/erasure-healing.go:649-653
for i, v := range result.Before.Drives {
	if v.Endpoint == disk.Endpoint().String() {
		result.After.Drives[i].State = madmin.DriveStateOk
	}
}
```

**Step 4 — the dangling handler** decides leave‑as‑is vs. purge. `deleteIfDangling` [`cmd/erasure-object.go:482`] asks `isObjectDangling` and, if the object is **not** provably dangling, refuses to act and returns read‑quorum error (**leave‑as‑is**):

```go
// cmd/erasure-object.go:482-490
func (er erasureObjects) deleteIfDangling(ctx context.Context, bucket, object string, metaArr []FileInfo, errs []error, dataErrsByPart map[int][]int, opts ObjectOptions) (FileInfo, error) {
	m, ok := isObjectDangling(metaArr, errs, dataErrsByPart)
	if !ok {
		// We only come here if we cannot figure out if the object
		// can be deleted safely, in such a scenario return ReadQuorum error.
		return FileInfo{}, errErasureReadQuorum
	}
	tags := make(map[string]string, 16)
	tags["set"] = strconv.Itoa(er.setIndex)
```

When `ok == true`, the function proceeds to build the audit tag map (`set`, `pool`, `merrs`, `derrs`, `sz`, `mt`, `d:p`, `offline`, `caller`) and then calls `DeleteVersion` on every disk to remove the dangling remnant (the **purge**; tag details and the delete loop are shown in §7).

There is also an **early dangling path**: if `objectQuorumFromMeta` itself fails (too few valid metadata copies to even establish quorum), `healObject` calls `deleteIfDangling` immediately [`cmd/erasure-healing.go:307-312`]. This is the path most of the 3‑missing‑meta purges below actually take (its audit `caller` tag is `erasure-healing.go:309`):

```go
// cmd/erasure-healing.go:307-312
readQuorum, _, err := objectQuorumFromMeta(ctx, partsMetadata, errs, er.defaultParityCount)
if err != nil {
	m, derr := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs, nil, ObjectOptions{
		VersionID: versionID,
	})
	errs = make([]error, len(errs))
```

**Control flow (reproduced from the source):**

```
HealObject [erasure-healing.go:1039]  (real ObjectLayer entry point)
  → healObject [erasure-healing.go:258]
     → readAllFileInfo across all 4 disks
     → objectQuorumFromMeta [erasure-metadata.go:531]
         └─ err? → deleteIfDangling(partsMetadata, errs, dataErrsByPart=nil) EARLY [erasure-healing.go:309]
     → classify each disk Ok/Missing/Corrupt/Offline [erasure-healing.go:382-404]   (Before/After drives)
     → disksToHealCount == 0? → "object is healthy, nothing to heal"
     → cannotHeal := disksToHealCount > parity [erasure-healing.go:428]
          (override: if all ETags agree (quorumETag != ""), cannotHeal=false [L429-433])
        ├─ false → RECONSTRUCT: RenameData, After[i]=Ok [erasure-healing.go:649-653]
        └─ true  → deleteIfDangling [erasure-object.go:482]
                     → isObjectDangling [erasure-healing.go:968]?
                        ├─ false → return errErasureReadQuorum         (LEAVE-AS-IS)
                        └─ true  → DeleteVersion on all disks
                                   + audit "DeleteDanglingObject"
                                   → errFileNotFound/errFileVersionNotFound  (PURGE)
```

---

## 5. Q1 + Q2 — the three outcomes, each with runtime evidence

> **Caveat about the shipped tests (stated honestly):** the repository's own `TestHeal*` tests run on **16‑ or 32‑disk** sets (EC:4, or EC:2‑on‑32), and `TestHealingDanglingObject` explicitly forces parity to 4. They demonstrate the *same mechanism* (`disksToHealCount > parity`) but not the exact 4‑drive numeric boundary the question asks about. To pin the exact **EC:2** boundary we use (a) the naturally‑EC:2 **live server** and (b) a **temporary scratch test** built on `prepareErasure(ctx, 4)`. Where a shipped test's parity differs, the mechanism is identical and only the numeric threshold scales with parity.

All shipped heal tests pass. Full, unedited `go test -v` output (every `=== RUN` line and every subtest, not just the summary):

```text
$ CGO_ENABLED=0 go test -v -run 'TestHeal|TestIsObjectDangling' -tags kqueue -timeout 900s ./cmd/
=== RUN   TestIsObjectDangling
=== RUN   TestIsObjectDangling/FileInfoExists-case1
=== RUN   TestIsObjectDangling/FileInfoExists-case2
=== RUN   TestIsObjectDangling/FileInfoUndecided-case1
=== RUN   TestIsObjectDangling/FileInfoUndecided-case2
=== RUN   TestIsObjectDangling/FileInfoUndecided-case3(file_deleted)
=== RUN   TestIsObjectDangling/FileInfoUnDecided-case4
=== RUN   TestIsObjectDangling/FileInfoUnDecided-case5-(ignore_errFileCorrupt_error)
=== RUN   TestIsObjectDangling/FileInfoUnDecided-case6-(data-dir_intact)
=== RUN   TestIsObjectDangling/FileInfoDecided-case1
=== RUN   TestIsObjectDangling/FileInfoDecided-case2-delete-marker
=== RUN   TestIsObjectDangling/FileInfoDecided-case3-(enough_data-dir_missing)
=== RUN   TestIsObjectDangling/FileInfoDecided-case4-(missing_data-dir_for_part_2)
=== RUN   TestIsObjectDangling/FileInfoDecided-case4-(enough_data-dir_existing_for_each_part)
--- PASS: TestIsObjectDangling (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoExists-case1 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoExists-case2 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUndecided-case1 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUndecided-case2 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUndecided-case3(file_deleted) (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUnDecided-case4 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUnDecided-case5-(ignore_errFileCorrupt_error) (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoUnDecided-case6-(data-dir_intact) (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoDecided-case1 (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoDecided-case2-delete-marker (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoDecided-case3-(enough_data-dir_missing) (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoDecided-case4-(missing_data-dir_for_part_2) (0.00s)
    --- PASS: TestIsObjectDangling/FileInfoDecided-case4-(enough_data-dir_existing_for_each_part) (0.00s)
=== RUN   TestHealing
--- PASS: TestHealing (0.11s)
=== RUN   TestHealingVersioned
--- PASS: TestHealingVersioned (0.12s)
=== RUN   TestHealingDanglingObject
--- PASS: TestHealingDanglingObject (0.20s)
=== RUN   TestHealCorrectQuorum
--- PASS: TestHealCorrectQuorum (0.58s)
=== RUN   TestHealObjectCorruptedPools
--- PASS: TestHealObjectCorruptedPools (0.32s)
=== RUN   TestHealObjectCorruptedXLMeta
--- PASS: TestHealObjectCorruptedXLMeta (0.24s)
=== RUN   TestHealObjectCorruptedParts
--- PASS: TestHealObjectCorruptedParts (0.35s)
=== RUN   TestHealObjectErasure
--- PASS: TestHealObjectErasure (0.22s)
=== RUN   TestHealEmptyDirectoryErasure
--- PASS: TestHealEmptyDirectoryErasure (0.08s)
=== RUN   TestHealLastDataShard
=== RUN   TestHealLastDataShard/4KiB
=== RUN   TestHealLastDataShard/64KiB
=== RUN   TestHealLastDataShard/128KiB
=== RUN   TestHealLastDataShard/1MiB
=== RUN   TestHealLastDataShard/5MiB
=== RUN   TestHealLastDataShard/10MiB
=== RUN   TestHealLastDataShard/5MiB-1KiB
=== RUN   TestHealLastDataShard/10MiB-1Kib
--- PASS: TestHealLastDataShard (1.08s)
    --- PASS: TestHealLastDataShard/4KiB (0.09s)
    --- PASS: TestHealLastDataShard/64KiB (0.08s)
    --- PASS: TestHealLastDataShard/128KiB (0.06s)
    --- PASS: TestHealLastDataShard/1MiB (0.08s)
    --- PASS: TestHealLastDataShard/5MiB (0.16s)
    --- PASS: TestHealLastDataShard/10MiB (0.23s)
    --- PASS: TestHealLastDataShard/5MiB-1KiB (0.17s)
    --- PASS: TestHealLastDataShard/10MiB-1Kib (0.20s)
PASS
ok  	github.com/minio/minio/cmd	3.609s
```

### 5.1 Outcome A — RECONSTRUCT (via the real `HealObject`, EC:2, 1 drive `xl.meta` missing)

**Scratch test** (`obj.HealObject`, deep scan, `Remove=true`), full unedited output:

```text
==================== BLITZY OUTCOME reconstruct: 1 drive xl.meta MISSING (deep scan, Remove=true) ====================
BLITZY object presence BEFORE heal: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
BLITZY[reconstruct-1missing] HealObject err = <nil>
BLITZY[reconstruct-1missing] HealResultItem (JSON) =
{
  "resultId": 0,
  "type": "object",
  "bucket": "blitzybucket",
  "object": "reconstruct-1missing",
  "versionId": "null",
  "detail": "",
  "parityBlocks": 2,
  "dataBlocks": 2,
  "diskCount": 4,
  "setCount": 0,
  "before": {
    "drives": [
      { "uuid": "", "endpoint": "/tmp/minio-1815770780", "state": "missing" },
      { "uuid": "", "endpoint": "/tmp/minio-2552471093", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-3641865182", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-3226884936", "state": "ok" }
    ]
  },
  "after": {
    "drives": [
      { "uuid": "", "endpoint": "/tmp/minio-1815770780", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-2552471093", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-3641865182", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-3226884936", "state": "ok" }
    ]
  },
  "objectSize": 1048576
}
BLITZY[reconstruct-1missing] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[reconstruct-1missing] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY object presence AFTER  heal: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
```

**Reading:** `err == nil`; drive 0 transitions `missing → ok`; the object is present with the **same size and ETag** before and after. `disksToHealCount = 1`, `1 > 2` is `false`, so `cannotHeal == false` → reconstruct path [`cmd/erasure-healing.go:428`], rebuilt shard renamed into place and `After[0].State = ok` [`cmd/erasure-healing.go:649-653`].

**Same outcome through the live operator path** (`mc admin heal --json --force`, drive `d1` `xl.meta` removed). Full unedited object result line:

```json
{"status":"success","type":"object","name":"healbucket2/recon3.bin","before":{"color":"yellow","offline":0,"online":3,"missing":1,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/d1","state":"missing"},{"uuid":"","endpoint":"/tmp/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/d4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/d4","state":"ok"}]},"size":1048576}
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":1048576,"duration":1}
```

Post‑heal checks (unedited):

```text
--- d1 xl.meta restored? ---
/tmp/d1/healbucket2/recon3.bin/xl.meta
--- data integrity: orig=39dbe3cd0f41bad0a50dc8d17082d25f3e5117872269a7921f7dc93f3de435dc healed=39dbe3cd0f41bad0a50dc8d17082d25f3e5117872269a7921f7dc93f3de435dc match=YES ---
```

The reconstructed object's bytes are **identical** to the original (SHA‑256 match), and the missing `xl.meta` on `d1` was physically restored. The human‑readable `mc` view of the same event:

```text
$ mc --config-dir /tmp/mc-config admin heal --force local/healbucket2/humanrecon.bin
[Green  ->  Green] healbucket2/
[Yellow ->  Green] healbucket2/humanrecon.bin
Healed:	1/1 objects; 1024 KiB in 1s
```

The `mc` **color** categories (`yellow → green`) are a **client‑side presentation** rendered by `mc` over the server's structured drive `state` field; the server result itself carries only `ok`/`missing`/`corrupt`/`offline`.

### 5.2 Outcome B — LEAVE‑AS‑IS (cannot rebuild, but not provably dangling → `errErasureReadQuorum`)

The canonical leave‑as‑is signal is `deleteIfDangling` returning `errErasureReadQuorum` **without touching any disk**, when `isObjectDangling` is `false`. Exercised directly on a non‑dangling object (three drives report non‑actionable *corruption*, one is intact — the object cannot be rebuilt from 1 shard, yet the corruption is not a provable "missing beyond parity" condition):

```text
==================== BLITZY PURE leave-as-is: deleteIfDangling on NON-dangling object ====================
BLITZY[leaveasis-pure] deleteIfDangling err = Read failed. Insufficient number of drives online
BLITZY[leaveasis-pure] err == errErasureReadQuorum ? true
```

This is the exact return at [`cmd/erasure-object.go:487`] (`return FileInfo{}, errErasureReadQuorum`). Because the function returns **before** the delete loop, no shard is removed — the object is left exactly as found.

A second, operator‑visible flavour of leave‑as‑is: when **three of four data shards are corrupt** (only 1 valid shard, below `dataBlocks=2`) while the metadata copies all agree, `cannotHeal` is `true` [`cmd/erasure-healing.go:428`] and — because the derived `quorumETag` is *empty*, the override at [`cmd/erasure-healing.go:429-433`] does **not** fire — it stays `true`. The object is nonetheless **not** provably dangling (a corrupt part is non‑actionable, so `isObjectDangling` is `false` [`cmd/erasure-healing.go:1008-1009`]), so `deleteIfDangling` returns `errErasureReadQuorum` and the object stays in place — neither reconstructed nor purged. The returned `HealResultItem` then reports all four drives `corrupt` (via `defaultHealResult`'s default mapping [`cmd/erasure-healing.go:820-826`]). Full output and the step‑by‑step mechanism are in §8, sweep case 3.

A third flavour: corrupt **metadata** on 3 of 4 drives yields `errFileCorrupt` and the object is left (not purged):

```text
==================== BLITZY OUTCOME leave-as-is: 3 drives xl.meta CORRUPT (deep scan, Remove=true) ====================
BLITZY object presence BEFORE heal: ABSENT (GetObjectInfo err = file is corrupted)
BLITZY[leaveasis-3corruptmeta] HealObject err = file is corrupted
BLITZY[leaveasis-3corruptmeta] Before.Drives states = "ok" "ok" "ok" "ok"
BLITZY[leaveasis-3corruptmeta] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY object presence AFTER  heal: ABSENT (GetObjectInfo err = file is corrupted)
```

### 5.3 Outcome C — PURGE (cannot rebuild AND provably dangling → remnant deleted)

**Set‑level `er.HealObject` (EC:2, 3 `xl.meta` missing → only 1 valid remnant on disk 3), with on‑disk before/after proof** — full unedited output:

```text
==================== BLITZY SET-LEVEL purge via er.HealObject (3 xl.meta MISSING, Remove=true) ====================
BLITZY[setlevel-purge] disk 0 xl.meta present BEFORE = false
BLITZY[setlevel-purge] disk 1 xl.meta present BEFORE = false
BLITZY[setlevel-purge] disk 2 xl.meta present BEFORE = false
BLITZY[setlevel-purge] disk 3 xl.meta present BEFORE = true
BLITZY[setlevel-purge] er.HealObject (SET-LEVEL) err = Version not found: blitzysignals/setlevel-purge-3missing(null)
BLITZY[setlevel-purge] err.Error() == VersionNotFound form ? true
BLITZY[setlevel-purge] Before.Drives states = "ok" "ok" "ok" "ok"
BLITZY[setlevel-purge] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[setlevel-purge] disk 0 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 1 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 2 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 3 xl.meta present AFTER  = false
```

**Reading:** before heal, disk 3 still held the valid `xl.meta` (the dangling remnant); after heal, **all four disks have no `xl.meta`** — the remnant was physically `DeleteVersion`‑ed. The returned error is the object‑API **`Version not found:` form** — a `VersionNotFound` wrap of the underlying `errFileVersionNotFound` (a direct `==` comparison to the bare sentinel is therefore `false`; the `err.Error()`‑prefix check above is `true`). The empty `versionID` is normalized to `nullVersionID` [`cmd/erasure-healing.go:1062`], and the code returns `errFileVersionNotFound` on a successful dangling delete [`cmd/erasure-healing.go:442-445`], which the object layer surfaces as this `Version not found:` string.

> **Honesty note on the `Before/After` array for purge:** in the purge path the `errs` slice is reset to `nil` before `defaultHealResult` builds the drive array, so the reported states read `ok`/`ok`/`ok`/`ok` — i.e. the drive‑state array is **not** the purge signal. The authoritative purge signals are (1) the **error return** (the `Version not found:` / `errFileVersionNotFound` form, or `errFileNotFound` for a non‑versioned object) and (2) the **physical deletion** of the remnant, both shown above.

**Same outcome through the live operator path** (`mc admin heal --json --force`, 3 drives' `xl.meta` removed, `d4` left as the valid remnant). On‑disk before/after and the full unedited heal JSON:

```text
--- after removing xl.meta on d1,d2,d3 (d4 remains = the valid remnant) ---
d1: absent
d2: absent
d3: absent
d4: xl.meta PRESENT
```

```json
{"status":"success","error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0","detail":"Object not found: healbucket2/purge3.bin","type":"object","name":"/","before":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"after":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"size":0}
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":0,"items_scanned":2,"items_healed":0,"size":0,"duration":1}
```

```text
--- on-disk AFTER heal: d4 valid remnant purged? ---
d1: dir present, NO xl.meta (orphan data)
d2: dir present, NO xl.meta (orphan data)
d3: dir present, NO xl.meta (orphan data)
d4: DIR GONE (purged)
```

The operator‑visible signal is `detail: "Object not found: healbucket2/purge3.bin"` with `objects_healed: 0`, and the on‑disk proof is that `d4` (the authoritative remnant) is **physically removed**. This confirms Q1: **MinIO leaves the object "deleted/degraded" — it does not resurrect a remnant that can never reach quorum.**

> **What that `error` field actually is — and is *not*.** The string `Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0` (full text in the JSON above) is **not** produced by the server and is **not** a `reedsolomon` decode error. Verified: the distinctive token `surplusShardsBeforeHeal` appears **nowhere** in the minio server tree, nor in `github.com/klauspost/reedsolomon@v1.12.4` (both `grep` searches return no match); it appears only in the **`mc` client**. It is emitted client‑side while `mc` computes the *display color* for the heal result: `getHColCode` returns `fmt.Errorf("Invalid parity shard count/surplus shard count given")` whenever `parityShards < 1` [`mc/cmd/admin-heal-ui.go:53-55`], and its caller `getObjectHCCChange` wraps that error with `": surplusShardsBeforeHeal: %d, parityShards: %d"` [`mc/cmd/admin-heal-result-item.go:47-48`]. Because the object was **purged**, the server's returned result item carries `parityShards = 0` (`< 1`), so the client's color arithmetic trips this error. The *decision* (purge) is carried instead by `detail: "Object not found: healbucket2/purge3.bin"` plus the physical deletion of `d4` — which correspond to the server's `errFileVersionNotFound`/`errFileNotFound` return.

---

## 6. Q3 — Observable decision signals (the `Before` / `After` drive‑state arrays)

The authoritative decision signal returned by the server is the `madmin.HealResultItem`'s per‑drive **`Before.Drives[].State`** vs. **`After.Drives[].State`** arrays. The per‑object erasure‑heal classifier emits **four** state constants, serialized lowercase: **`ok`**, **`missing`**, **`corrupt`**, **`offline`**. (`madmin` actually defines **nine** `DriveState*` constants [`madmin-go/v3/heal-commands.go:120-128`]: the four above plus `permission-denied`, `faulty`, `root-mount`, `unknown`, and `unformatted`. Those other five are used elsewhere in admin/disk reporting but are **not** produced by this per‑object shard classifier — a `grep` of `cmd/erasure-healing.go` finds only `DriveStateOk`, `DriveStateMissing`, `DriveStateCorrupt`, and `DriveStateOffline`.) They are assembled by the classifier in §4, Step 1 [`cmd/erasure-healing.go:382-404`].

**For a healed (reconstructed) object, the transition is `missing`/`corrupt` → `ok`.** Observed (EC:2 scratch, 1 missing shard, from §5.1):

```text
BLITZY[reconstruct-1missing] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[reconstruct-1missing] After.Drives  states = "ok" "ok" "ok" "ok"
```

and at the exact parity boundary (2 missing shards — see §8):

```text
BLITZY[reconstruct-2missing] Before.Drives states = "missing" "missing" "ok" "ok"
BLITZY[reconstruct-2missing] After.Drives  states = "ok" "ok" "ok" "ok"
```

**And for a `corrupt` (not merely `missing`) shard, the transition is `corrupt` → `ok`.** To exercise the classifier's `default` branch [`cmd/erasure-healing.go:390-392`] — the one that produces `corrupt` rather than `missing` — I corrupted **one** drive's `xl.meta` with junk bytes using the canonical technique from `TestHealObjectCorruptedXLMeta` [`cmd/erasure-healing_test.go:1253`], `firstDisk.WriteAll(context.Background(), bucket, pathJoin(object, xlStorageFormatFile), []byte("abcd"))`, leaving the other three drives intact. Because `disksToHealCount = 1 ≤ parity = 2`, `cannotHeal == false` and the object is reconstructed. Observed through the **real** `er.HealObject` entry (deep scan, `Remove=true`) — full unedited `HealResultItem`:

```text
==================== BLITZY Q3 Corrupt->Ok: 1 drive CORRUPT xl.meta (junk), 3 intact (deep scan, Remove=true) ====================
BLITZY object presence BEFORE heal: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
BLITZY[corrupt-1xlmeta] HealObject err = <nil>
BLITZY[corrupt-1xlmeta] HealResultItem (JSON) =
{
  "resultId": 0,
  "type": "object",
  "bucket": "blitzycorrupt",
  "object": "corrupt-1xlmeta",
  "versionId": "null",
  "detail": "",
  "parityBlocks": 2,
  "dataBlocks": 2,
  "diskCount": 4,
  "setCount": 0,
  "before": {
    "drives": [
      {
        "uuid": "",
        "endpoint": "/tmp/minio-3404828048",
        "state": "corrupt"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-2545624221",
        "state": "ok"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-797473030",
        "state": "ok"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-2166782108",
        "state": "ok"
      }
    ]
  },
  "after": {
    "drives": [
      {
        "uuid": "",
        "endpoint": "/tmp/minio-3404828048",
        "state": "ok"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-2545624221",
        "state": "ok"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-797473030",
        "state": "ok"
      },
      {
        "uuid": "",
        "endpoint": "/tmp/minio-2166782108",
        "state": "ok"
      }
    ]
  },
  "objectSize": 1048576
}
BLITZY[corrupt-1xlmeta] Before.Drives states = "corrupt" "ok" "ok" "ok"
BLITZY[corrupt-1xlmeta] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY object presence AFTER  heal: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
```

**Reading:** the tampered drive is classified **`corrupt`** in `Before` — *not* `missing` — which confirms it took the `default` classifier branch [`cmd/erasure-healing.go:390-392`] (a junk `xl.meta` fails to decode and does not match any of the `missing`‑list errors). The heal returns `err = <nil>`, `After` shows all four drives **`ok`**, and the object stays `PRESENT` with an unchanged size and ETag (`b561f87202d04959e37588ee05cf5b10`) — the observed **`corrupt` → `ok`** transition. (The `/tmp/minio-*` endpoints in this item are a fresh set of `os.MkdirTemp` paths from a separate capture run; per §13 the temp‑dir endpoints and random version IDs vary between independent runs while the decision fields — states, `err`, parity/data blocks, ETag — do not.)

Live, the same signal appears in the JSON `before`/`after` `drives[].state` fields, and `mc` additionally computes a summary `color`/`online`/`missing`/`corrupted` header **client‑side** from those states. This is the same full object‑heal item shown verbatim in §5.1 (`healbucket2/recon3.bin`, 1 drive `xl.meta` removed), with all four drives listed (no field elided):

```json
"before":{"color":"yellow","offline":0,"online":3,"missing":1,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/d1","state":"missing"},{"uuid":"","endpoint":"/tmp/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/d4","state":"ok"}]}
"after": {"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/d4","state":"ok"}]}
```

The per‑drive `state` goes `missing → ok` on `/tmp/d1`; `mc`'s client‑side summary header goes `color: yellow (online 3, missing 1) → color: green (online 4, missing 0)`. (Independently reproduced this session on a second live instance — see §13.)

**How each state is produced** (all in the classifier switch [`cmd/erasure-healing.go:383-394`]):

| Observed `state` | `madmin` constant | Trigger (reason) |
|---|---|---|
| `ok` | `DriveStateOk` | `reason == nil` (shard present and valid) |
| `offline` | `DriveStateOffline` | `errDiskNotFound` (drive not reachable) |
| `missing` | `DriveStateMissing` | any of `errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errPartMissingOrCorrupt`, `errOutdatedXLMeta`, `errLegacyXLMeta` |
| `corrupt` | `DriveStateCorrupt` | default (any remaining error implies corrupt data/metadata) |

Note that a **corrupted data part under a deep scan** surfaces as `errPartMissingOrCorrupt` (`= "part missing or corrupt"` [`cmd/erasure-healing.go:152`]) and is therefore classified **`missing`**, which is why the sweep of corrupt *data* shards (§8) reports `missing` in the `Before` array. A shard whose *metadata* is corrupt/undecodable falls to the `default` → `corrupt` (as in the all‑corrupt sweep case 3).

---

## 7. Q4 — Rationale in the logs (does MinIO explain *why*?)

**Yes — for a purge, MinIO emits a structured `DeleteDanglingObject` audit event whose tag set encodes the reasoning.** It is produced by `auditDanglingObjectDeletion` [`cmd/erasure-object.go:451`], which only fires when an audit target is configured:

```go
// cmd/erasure-object.go:451-465
func auditDanglingObjectDeletion(ctx context.Context, bucket, object, versionID string, tags map[string]string) {
	if len(logger.AuditTargets()) == 0 {
		return
	}
	opts := AuditLogOptions{
		Event:     "DeleteDanglingObject",
		Bucket:    bucket,
		Object:    object,
		VersionID: versionID,
		Tags:      tags,
	}
	auditLogInternal(ctx, opts)
}
```

Captured live via an audit webhook during the purge of `purge3.bin` (full, unedited event):

```json
{
    "version": "1",
    "deploymentid": "d2d8f1b5-0d61-4d1b-beb8-27aedd179bb5",
    "time": "2026-07-06T22:16:14.42344104Z",
    "event": "DeleteDanglingObject",
    "trigger": "DeleteDanglingObject",
    "api": {
        "bucket": "healbucket2",
        "objects": [
            {
                "objectName": "purge3.bin"
            }
        ],
        "rx": 0,
        "tx": 0
    },
    "tags": {
        "caller": "/tmp/blitzy/minio/blitzy-629729d3-cb15-4820-bb3e-f65517d9c2ea_444e13/cmd/erasure-healing.go:309",
        "d:p": "2:2",
        "ddisk-0": "file version not found",
        "ddisk-1": "file version not found",
        "ddisk-2": "file version not found",
        "ddisk-3": "<nil>",
        "derrs": "map[]",
        "merrs": "",
        "mt": "20260706T221613Z",
        "pool": "0",
        "set": "0",
        "sz": "1048576"
    }
}
```

**What each tag reveals about the "why"** (tags built in `deleteIfDangling` [`cmd/erasure-object.go:491-528`]):

| Tag | Observed value | Meaning / source |
|---|---|---|
| `caller` | `/tmp/blitzy/minio/blitzy-629729d3-cb15-4820-bb3e-f65517d9c2ea_444e13/cmd/erasure-healing.go:309` | `runtime.Caller(1)` [`cmd/erasure-object.go:526-528`] — proves this purge took the **early quorum‑failure path** [`cmd/erasure-healing.go:309`], not the main `cannotHeal` branch at L438. (Only one valid metadata copy remains, which is below read quorum 2 ⇒ `objectQuorumFromMeta` errored ⇒ early `deleteIfDangling`.) |
| `d:p` | `2:2` | `fmt.Sprintf("%d:%d", m.Erasure.DataBlocks, m.Erasure.ParityBlocks)` [`cmd/erasure-object.go:497`] — the object's **data:parity ratio = EC:2**, confirming the config *in the audit trail itself*. |
| `ddisk-0`, `ddisk-1`, `ddisk-2` | `file version not found` | per‑disk `DeleteVersion` result from the delete loop [`cmd/erasure-object.go:551-560`] — those drives had nothing to delete (their metadata was already gone). |
| `ddisk-3` | `<nil>` | disk 3 held the valid remnant; its `DeleteVersion` **succeeded** (nil error) — this is the shard that was actually purged. |
| `sz` | `1048576` | object size [`cmd/erasure-object.go:495`]. |
| `mt` | `20260706T221613Z` | object mod‑time [`cmd/erasure-object.go:496`]. |
| `set` / `pool` | `0` / `0` | erasure‑set and pool index. |
| `derrs` | `map[]` | `fmt.Sprintf("%v", dataErrsByPart)` [`cmd/erasure-object.go:493`]; empty because the early path passes `dataErrsByPart = nil` [`cmd/erasure-healing.go:309`]. |
| `merrs` | `` (empty) | `joinErrs(errs)` [`cmd/erasure-object.go:492`]. **Observed empty** — and this is a genuine source bug, confirmed at runtime: `joinErrs` iterates `for i := range s` over its *empty local string* `s` instead of over `errs`, so it always returns `""` [`cmd/erasure-object.go`]. Reported here as observed, not paraphrased. |

**Why there is no `offline` tag in this event.** The `offline` tag is emitted **conditionally**: `deleteIfDangling` counts the disks reporting `errDiskNotFound` (or a part `checkPartDiskNotFound`) and adds the tag only `if offline > 0` [`cmd/erasure-object.go:503-524`]. In this capture all four drives were **online** (`admin info` reported "4 drives online, 0 drives offline"), so `offline == 0` and the tag is **legitimately absent** — and that absence is itself informative: it shows the purge was driven by *missing metadata*, not by offline drives. (The complete tag vocabulary `deleteIfDangling` can emit is `set`, `pool`, `merrs`, `derrs`, then either `sz`/`mt`/`d:p` for a valid `FileInfo` or `invalid`+`d:p` otherwise, the conditional `offline`, and `caller` [`cmd/erasure-object.go:489-528`].)

The `caller`, `d:p`, and per‑disk `ddisk-*` tags together answer *why* the object was purged: parity is 2, and only one of the four metadata copies remained readable — which is below read quorum 2 — so the object could never reach quorum and the sole remnant was deleted.

**Heal trace** (`mc admin trace --call healing --json`) captures the per‑object heal invocation itself, emitted via `healTrace → madmin.TraceHealing` [`cmd/erasure-healing.go:1090`] (full, unedited):

```json
{"status":"success","host":"127.0.0.1:9000","time":"2026-07-06T22:10:50.223841694Z","client":"","duration":15791105,"timeToFirstByte":0,"api":"heal.Object","path":"healbucket/trace.bin","query":"","statusCode":0,"statusMsg":"","type":"Healing","size":1048576,"error":"","extra":{"disks":"4","dry":"false","mode":"1","remove":"false","version-id":"null"}}
```

Here `type:"Healing"`, `api:"heal.Object"`, `extra.disks:"4"`, and `extra.mode:"1"` (the `HealNormalScan` scan‑mode value — the raw integer emitted by `fmt.Sprint(opts.ScanMode)` [`cmd/erasure-healing.go:1103`], where `HealScanMode` is defined `HealUnknownScan=0`, `HealNormalScan=1`, `HealDeepScan=2` [`madmin-go/v3/heal-commands.go:37-44`]; this capture is a default `mc admin heal` — note `remove:"false"` in the same trace — so `HealDeepScan` would serialize as `"2"`) show the heal path, scope, and scan mode for the object.

> **For reconstruct and leave‑as‑is, there is no dedicated "reason" audit event** — the rationale is carried by the `Before`/`After` state transition (reconstruct) or by the returned error (`errErasureReadQuorum` for leave‑as‑is). The `DeleteDanglingObject` event is specific to the purge decision.

---

## 8. Q5 — Boundary A: how many valid shards must exist for healing to succeed?

**Answer: at least `dataBlocks` intact shards — i.e. 2 for EC:2 — equivalently, healing succeeds while `disksToHealCount ≤ parityBlocks` (2) and fails once `disksToHealCount > parityBlocks`** [`cmd/erasure-healing.go:428`]. Demonstrated by sweeping the number of *corrupted data shards* from 1 → 3 (metadata intact on all four, deep scan, `Remove=true`), full unedited output per case:

**Sweep case 1 — 1 corrupt (3 valid) → RECONSTRUCT** (the `DRYRUN classifier` line is a dry‑run `HealObject` that returns immediately after the drive‑state classifier [`cmd/erasure-healing.go:424`], so it shows exactly what the classifier saw *before* any repair — the ground‑truth count of surviving shards):

```text
==================== BLITZY SWEEP: 1 of 4 data shards CORRUPT (valid shards=3, parity=2, deep scan) ====================
BLITZY[sweep-1] presence BEFORE: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
BLITZY[sweep-1] DRYRUN classifier states (valid=ok) = "missing" "ok" "ok" "ok"
BLITZY[sweep-1] HealObject err = <nil>
BLITZY[sweep-1] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[sweep-1] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[sweep-1] presence AFTER : PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
```

**Sweep case 2 — 2 corrupt (2 valid = the boundary) → RECONSTRUCT:**

```text
==================== BLITZY SWEEP: 2 of 4 data shards CORRUPT (valid shards=2, parity=2, deep scan) ====================
BLITZY[sweep-2] presence BEFORE: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
BLITZY[sweep-2] DRYRUN classifier states (valid=ok) = "missing" "missing" "ok" "ok"
BLITZY[sweep-2] HealObject err = <nil>
BLITZY[sweep-2] Before.Drives states = "missing" "missing" "ok" "ok"
BLITZY[sweep-2] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[sweep-2] presence AFTER : PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
```

**Sweep case 3 — 3 corrupt (only 1 valid, below `dataBlocks=2`) → CANNOT HEAL (object left in place):**

```text
==================== BLITZY SWEEP: 3 of 4 data shards CORRUPT (valid shards=1, parity=2, deep scan) ====================
BLITZY[sweep-3] presence BEFORE: PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
BLITZY[sweep-3] DRYRUN classifier states (valid=ok) = "missing" "missing" "missing" "ok"
BLITZY[sweep-3] HealObject err = Storage resources are insufficient for the read operation blitzysweep/sweep-3corrupt
BLITZY[sweep-3] Before.Drives states = "corrupt" "corrupt" "corrupt" "corrupt"
BLITZY[sweep-3] After.Drives  states = "corrupt" "corrupt" "corrupt" "corrupt"
BLITZY[sweep-3] presence AFTER : PRESENT (size=1048576, etag=b561f87202d04959e37588ee05cf5b10)
```

**Reading case 3 — why the `DRYRUN classifier` line shows exactly one valid shard, yet the final `Before`/`After` arrays report all four drives `corrupt`.** These two views are *not* contradictory; together they are the boundary proof, and each is grounded in a distinct code path:

- The **dry‑run classifier** view (`"missing" "missing" "missing" "ok"`) is the ground truth of surviving shards: the three corrupted data parts are classified as needing heal, the one intact drive as `ok`. Exactly **1 valid shard** remains — below `dataBlocks = 2` — so `disksToHealCount (3) > parityBlocks (2)` and `cannotHeal` is `true` [`cmd/erasure-healing.go:428`].
- All four **metadata** copies are intact and agree, so `objectQuorumFromMeta` succeeds (`readQuorum = 2`); but the derived `quorumETag` is **empty**, so the override that could flip `cannotHeal` back to `false` does **not** fire [`cmd/erasure-healing.go:429-433`] — `cannotHeal` stays `true`.
- The object is **not provably dangling**: a *corrupt* part is not the same as a *missing* one, so it is counted non‑actionable and `isObjectDangling` returns `false` [`cmd/erasure-healing.go:1008-1009`]. The `cannotHeal` branch therefore calls `deleteIfDangling`, which — finding the object not dangling — returns `errErasureReadQuorum` **without deleting anything** [`cmd/erasure-object.go:483,487`]. The object is **left in place** (presence AFTER = PRESENT, same ETag) — a leave‑as‑is outcome surfaced to the client as `InsufficientReadQuorum` (the `Storage resources are insufficient for the read operation blitzysweep/sweep-3corrupt` message shown verbatim in the block above).
- The **final** `HealResultItem` reports every drive `corrupt` because `defaultHealResult` derives each drive's reported state from its per‑drive error: `nil → ok`, `errFileNotFound`/`errVolumeNotFound → missing`, and *everything else* (here `errErasureReadQuorum`, stamped on all four drives by the leave‑as‑is path) `→ corrupt` [`cmd/erasure-healing.go:820-826`]. That is why the ground‑truth "1 valid" (dry‑run) and the result "all four corrupt" (final) are both correct and consistent.

**The boundary is exactly at 2 valid shards:** 3 or 2 valid → reconstruct; 1 valid → cannot heal. This is precisely `disksToHealCount > parityBlocks` (`2 > 2` is false → heal; `3 > 2` is true → cannot).

Documentation corroboration (supplementing the runtime evidence): MinIO states "There must be an intact parity shard available for each lost or damaged data shard, otherwise the object cannot be recovered" — the documentary statement of the same `disksToHealCount > parityBlocks` cutoff.

---

## 9. Q6 — Boundary B: the failure/leave‑as‑is error strings (verbatim)

Three distinct error strings appear when healing cannot recover an object, depending on which sub‑decision is reached. Each is quoted **exactly** from source and matched to the observed runtime string.

**1) Leave‑as‑is (internal) — `errErasureReadQuorum`:**

```go
// cmd/erasure-errors.go:23
var errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")
// cmd/erasure-errors.go:26
var errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")
```

Observed exactly (from §5.2):

```text
BLITZY[leaveasis-pure] deleteIfDangling err = Read failed. Insufficient number of drives online
BLITZY[leaveasis-pure] err == errErasureReadQuorum ? true
```

**2) Post‑purge returns — `errFileNotFound` / `errFileVersionNotFound`:**

```go
// cmd/storage-errors.go:71
var errFileNotFound = StorageErr("file not found")
// cmd/storage-errors.go:74
var errFileVersionNotFound = StorageErr("file version not found")
// cmd/storage-errors.go:104
var errFileCorrupt = StorageErr("file is corrupted")
```

`healObject` chooses which to return after a successful dangling delete [`cmd/erasure-healing.go:442-445`]: `errFileNotFound` when `versionID == ""`, else `errFileVersionNotFound`. Observed at the set level (empty versionID normalized to `nullVersionID`, so the *version* form appears):

```text
BLITZY[setlevel-purge] er.HealObject (SET-LEVEL) err = Version not found: blitzysignals/setlevel-purge-3missing(null)
```

and `errFileCorrupt` ("file is corrupted") on the corrupt‑metadata leave‑as‑is (from §5.2):

```text
BLITZY[leaveasis-3corruptmeta] HealObject err = file is corrupted
```

**3) Client‑facing — `InsufficientReadQuorum`:**

```go
// cmd/object-api-errors.go:236-237
func (e InsufficientReadQuorum) Error() string {
	return "Storage resources are insufficient for the read operation " + e.Bucket + "/" + e.Object
// cmd/object-api-errors.go:248-249 (InsufficientWriteQuorum)
	return "Storage resources are insufficient for the write operation " + e.Bucket + "/" + e.Object
```

`InsufficientReadQuorum` unwraps to `errErasureReadQuorum`. Observed exactly at the top level (sweep case 3, §8):

```text
BLITZY[sweep-3] HealObject err = Storage resources are insufficient for the read operation blitzysweep/sweep-3corrupt
```

Note the client‑facing string is **not** the same text as the internal `errErasure*` — it is the bucket/object‑qualified message above, captured here from the actual return rather than assumed.

**Top‑level vs set‑level nuance (observed):** the top‑level `ObjectLayer.HealObject` wrapper collapses an all‑not‑found purge to an empty `HealResultItem{}` plus an `ObjectNotFound`‑type error when `versionID == ""`:

```text
BLITZY[purge-3missing] HealObject err = Object not found: blitzybucket/purge-3missing
BLITZY[purge-3missing] Before.Drives states =
BLITZY[purge-3missing] After.Drives  states =
```

whereas the set‑level `er.HealObject` returns the populated result and the `Version not found` (`errFileVersionNotFound`) form shown above. Both are the **purge** decision; they differ only in how the wrapper surfaces the post‑purge "gone" state.

---

## 10. Q7 — Boundary C: does behavior differ between a partial *write* and a partial *delete*?

**Yes — and the difference is proven below through the real `er.HealObject` entry point, not merely inferred.** Two things differ between a partial *write* (a normal data object) and a partial *delete* (a delete‑marker):

1. **The recovery action differs.** A recoverable *normal object* is **reconstructed** — missing/corrupt data shards are rebuilt from parity (Outcome A, §5.1). A recoverable *delete‑marker* has no data parts, so healing simply **propagates the marker metadata** to the drives missing it. Beyond the recoverable threshold, **both** are purged.
2. **The dangling test uses different arithmetic.** `isObjectDangling` [`cmd/erasure-healing.go:968`] takes a **meta‑only** branch for a delete‑marker versus a **meta‑and‑parts** branch for a normal object (shown after the runtime proof).

### 10.1 Runtime proof through the real `HealObject` path

All three scenarios below call `er.HealObject(ctx, bucket, object, versionID, madmin.HealOpts{ScanMode: HealDeepScan, Remove: true})` — the same `erasureObjects.HealObject` [`cmd/erasure-healing.go:1039`] that `mc admin heal` reaches (§12) — after inducing the remnant on a canonical EC:2 set. Full unedited output:

**W — partial WRITE (normal object, `Deleted=false`), 3 of 4 `xl.meta` missing (beyond parity) ⇒ PURGED:**

```text
==================== BLITZY Q7-W partial WRITE (normal object, 3 xl.meta missing) via REAL er.HealObject ====================
BLITZY[q7-write] latest meta Deleted (delete-marker?) = false (false => WRITE/data object)
BLITZY[q7-write] disk 0 xl.meta present BEFORE = false
BLITZY[q7-write] disk 1 xl.meta present BEFORE = false
BLITZY[q7-write] disk 2 xl.meta present BEFORE = false
BLITZY[q7-write] disk 3 xl.meta present BEFORE = true
BLITZY[q7-write] REAL er.HealObject err = Version not found: blitzyq7/q7-write-3missing(null)
BLITZY[q7-write] Before.Drives states = "ok" "ok" "ok" "ok"
BLITZY[q7-write] disk 0 xl.meta present AFTER  = false
BLITZY[q7-write] disk 1 xl.meta present AFTER  = false
BLITZY[q7-write] disk 2 xl.meta present AFTER  = false
BLITZY[q7-write] disk 3 xl.meta present AFTER  = false
BLITZY[q7-write] presence AFTER = ABSENT (GetObjectInfo err = Object not found: blitzyq7/q7-write-3missing)
```

The surviving metadata is a normal data object (`Deleted=false`); with 3 of 4 `xl.meta` gone the object is dangling via the **normal‑object** branch, so `HealObject` purges the last remnant (disk 3 `xl.meta` goes `true → false`) and returns the null‑version `Version not found:` form (`Version not found: blitzyq7/q7-write-3missing(null)`, shown verbatim above); the object is subsequently `ABSENT`.

**D — partial DELETE (delete‑marker as the sole surviving version, `Deleted=true`), 3 of 4 `xl.meta` missing (beyond threshold) ⇒ PURGED:**

```text
==================== BLITZY Q7-D partial DELETE (delete-marker sole version, 3 xl.meta missing) via REAL er.HealObject ====================
BLITZY[q7-delete] delete-marker version=7563045e-bd19-4296-9cb7-31353720b17f DeleteMarker=true
BLITZY[q7-delete] healed version meta Deleted (delete-marker?) = true (true => DELETE branch)
BLITZY[q7-delete] disk 0 xl.meta present BEFORE = false
BLITZY[q7-delete] disk 1 xl.meta present BEFORE = false
BLITZY[q7-delete] disk 2 xl.meta present BEFORE = false
BLITZY[q7-delete] disk 3 xl.meta present BEFORE = true
BLITZY[q7-delete] REAL er.HealObject(delete-marker version) err = Version not found: blitzyq7/q7-delete-marker(7563045e-bd19-4296-9cb7-31353720b17f)
BLITZY[q7-delete] Before.Drives states = "ok" "ok" "ok" "ok"
BLITZY[q7-delete] disk 0 xl.meta present AFTER  = false
BLITZY[q7-delete] disk 1 xl.meta present AFTER  = false
BLITZY[q7-delete] disk 2 xl.meta present AFTER  = false
BLITZY[q7-delete] disk 3 xl.meta present AFTER  = false
```

Here the surviving metadata's latest version is a **delete‑marker** (`Deleted=true`); with 3 of 4 copies gone it is dangling via the **meta‑only** branch, so `HealObject` purges the remnant marker (disk 3 `true → false`) and returns the versioned `Version not found:` form carrying the delete‑marker's version id (`Version not found: blitzyq7/q7-delete-marker(7563045e-bd19-4296-9cb7-31353720b17f)`, shown verbatim above).

**D2 — partial DELETE that is *recoverable* (delete‑marker, only 1 of 4 `xl.meta` missing) ⇒ PROPAGATED (healed), not purged:**

```text
==================== BLITZY Q7-D2 partial DELETE that HEALS (delete-marker, only 1 xl.meta missing) ====================
BLITZY[q7-delete-heal] delete-marker version=af203e3b-93f3-4728-89e4-b9177b383f78
BLITZY[q7-delete-heal] disk 0 xl.meta present BEFORE = false
BLITZY[q7-delete-heal] REAL er.HealObject err = <nil>
BLITZY[q7-delete-heal] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[q7-delete-heal] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[q7-delete-heal] disk 0 xl.meta present AFTER  = true (true => delete-marker PROPAGATED/healed)
```

This is the behavioral contrast: a delete‑marker within the recoverable threshold is **propagated** (its `xl.meta` is written back to the drive that was missing it — disk 0 `false → true`, `Before "missing" → After "ok"`, `err=nil`) rather than reconstructed from parity, because a marker carries no data shards. The analogous *normal‑object* case within parity reconstructs data shards instead (Outcome A, §5.1).

### 10.2 The mechanism behind the divergence — `isObjectDangling` arithmetic

The dangling test that drives both purge decisions above lives in `isObjectDangling` [`cmd/erasure-healing.go:968`] and branches on `validMeta.Deleted`:

```go
// cmd/erasure-healing.go:1008-1036 (verbatim)
	if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
		return validMeta, false
	}

	if validMeta.Deleted {
		// notFoundPartsErrs is ignored since
		// - delete marker does not have any parts
		dataBlocks := (len(errs) + 1) / 2
		return validMeta, notFoundMetaErrs > dataBlocks
	}

	// TODO: It is possible to replay the object via just single
	// xl.meta file, considering quorum number of data-dirs are still
	// present on other drives.
	//
	// However this requires a bit of a rewrite, leave this up for
	// future work.
	if notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks {
		// All xl.meta is beyond parity blocks missing, this is dangling
		return validMeta, true
	}

	if !validMeta.IsRemote() && notFoundPartsErrs > 0 && notFoundPartsErrs > validMeta.Erasure.ParityBlocks {
		// All data-dir is beyond parity blocks missing, this is dangling
		return validMeta, true
	}

	return validMeta, false
}
```

Annotating the branches (each mapped to a scenario above): the first `return validMeta, false` is the **undecidable / leave‑as‑is** guard; the `validMeta.Deleted` block is the **delete‑marker (meta‑only)** branch (scenario **D**); the `notFoundMetaErrs > ParityBlocks` block is the **normal‑object metadata‑beyond‑parity** branch (scenario **W**); the `notFoundPartsErrs > ParityBlocks` block is the **normal‑object data‑parts‑beyond‑parity** branch (case **[C]**).

- **Partial delete (delete‑marker):** the `validMeta.Deleted` branch [`cmd/erasure-healing.go:1012-1017`] uses a threshold of `dataBlocks = (len(errs)+1)/2` and **ignores part errors entirely** (a delete marker has no data parts).
- **Partial write (normal data object):** the object is dangling if *either* metadata errors exceed parity [`cmd/erasure-healing.go:1025`] *or* data‑part errors exceed parity [`cmd/erasure-healing.go:1030`] — it checks **both** meta and parts against `ParityBlocks`.

Exercised directly against `isObjectDangling` on a clean EC:2 `FileInfo` (parity = 2) as a supporting cross‑check of the branch arithmetic, full unedited output:

```text
==================== BLITZY Q7 supporting: isObjectDangling arithmetic (EC:2, parity=2) ====================
BLITZY[A normal-obj, 3 xl.meta missing]  parity=2 meta-threshold=parity => dangling=true (Deleted=false)
BLITZY[B delete-marker, 3 xl.meta missing] meta-only branch (len(errs)+1)/2=2 => dangling=true (Deleted=true)
BLITZY[C normal-obj, parts notFound 3/4, meta intact] parts-threshold=parity => dangling=true (Deleted=false)
BLITZY note: delete-marker branch IGNORES part errors (markers have no parts); normal-object path checks BOTH meta AND part errors vs parity.
```

**Reading the three cases:**
- **[A]** a partially‑written *normal object* with 3/4 `xl.meta` missing is dangling because `notFoundMetaErrs(3) > parity(2)` [`cmd/erasure-healing.go:1025`]; `validMeta.Deleted == false`. (This is the arithmetic behind scenario **W** above.)
- **[B]** a partial *delete* (a *delete‑marker* missing on 3/4 drives) is dangling via the **meta‑only** branch with threshold `(len(errs)+1)/2 = 2` and **no** part accounting; `validMeta.Deleted == true`. (This is the arithmetic behind scenario **D** above.)
- **[C]** a partially‑written normal object whose *metadata is intact* but whose *data parts* are missing on 3/4 drives is dangling via the **parts** branch `notFoundPartsErrs(3) > parity(2)` [`cmd/erasure-healing.go:1030`] — a check that does **not** exist for delete markers.

This is corroborated by the shipped `TestIsObjectDangling` (which uses `newFileInfo(_, 2, 2)` = **exact EC:2**) — its subcases include a normal‑object dangling case, a **delete‑marker** case, and part‑missing cases, all passing:

```text
--- PASS: TestIsObjectDangling/FileInfoDecided-case1 (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case2-delete-marker (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case3-(enough_data-dir_missing) (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case4-(missing_data-dir_for_part_2) (0.00s)
--- PASS: TestIsObjectDangling/FileInfoUnDecided-case5-(ignore_errFileCorrupt_error) (0.00s)
```

So the **decision mechanism** (dangling ⇒ purge, non‑dangling ⇒ reconstruct/propagate) is the same for writes and deletes, but the two differ in **what healing does when recoverable** (a normal object reconstructs data shards; a delete‑marker propagates its metadata — scenario **D2**) and in the **dangling arithmetic** (a delete‑marker is judged on metadata copies alone, whereas a data object is judged on metadata *and* data parts against parity — scenarios **W**/**D**/[C]).

---

## 11. Additional conditions exercised — scan mode (`HealNormalScan` vs `HealDeepScan`) and the `Remove` flag

The decision above was also exercised across the two `madmin.HealOpts` modifiers the investigation is required to cover — the **scan mode** and the **`Remove` flag** — to show how each does (or does not) change what heal observes and does. Both were driven through the real `er.HealObject` path [`cmd/erasure-healing.go:1039`].

### 11.1 Scan mode — `HealNormalScan` misses silent (same‑size) content corruption; `HealDeepScan` catches it

To isolate the scan modes, a single data part was corrupted **in place with same‑size garbage** (byte length preserved), then the object was healed once under each mode. Full unedited output:

```text
==================== BLITZY R5 scan-mode: NormalScan vs DeepScan on a SAME-SIZE (content-only) corrupt data part ====================
BLITZY[r5-normal] HealNormalScan err=<nil> Before="ok" "ok" "ok" "ok" After="ok" "ok" "ok" "ok"
BLITZY[r5-deep]   HealDeepScan   err=<nil> Before="missing" "ok" "ok" "ok" After="ok" "ok" "ok" "ok"
```

**Why they differ, at `file:line`.** `disksWithAllParts` [`cmd/erasure-healing-common.go:291`] branches on the scan mode when it inspects each part:

```go
// cmd/erasure-healing-common.go:418-425
		switch scanMode {
		case madmin.HealDeepScan:
			// disk has a valid xl.meta but may not have all the
			// parts. This is considered an outdated disk, since
			// it needs healing too.
			verifyResp, verifyErr = onlineDisk.VerifyFile(ctx, bucket, object, meta)
		default:
			verifyResp, verifyErr = onlineDisk.CheckParts(ctx, bucket, object, meta)
```

- `HealNormalScan` (the `default` branch) calls `onlineDisk.CheckParts` [`cmd/erasure-healing-common.go:425`], which checks only that each part **exists with the expected size** — it does *not* hash the content. A same‑size overwrite passes, so no drive is flagged and `Before` reads `"ok" "ok" "ok" "ok"` (the corruption is **undetected** — heal is a no‑op, `err=<nil>`).
- `HealDeepScan` calls `onlineDisk.VerifyFile` [`cmd/erasure-healing-common.go:423`], which **bitrot‑verifies** the part content against its stored HighwayHash checksum. The tampered part fails verification → `checkPartFileCorrupt` → `shouldHealObjectOnDisk` returns `errPartMissingOrCorrupt` [`cmd/erasure-healing.go:156`, string at `:152`], so that drive is classified `missing` in `Before` and then rebuilt (`After` = all `ok`).

This is the runtime basis for the documented fact that MinIO does not bit‑rot‑check via the ordinary scanner by default — only a deep scan (or an on‑the‑fly GET/HEAD read) detects silent content rot. The heal path also **self‑escalates**: if a normal scan hits `errFileCorrupt`, `HealObject` retries with `HealDeepScan` [`cmd/erasure-healing.go:1080-1083`].

### 11.2 The `Remove` flag does **not** gate the dangling‑object purge

It is natural to assume `Remove: true` is required before heal will delete a dangling remnant. For a dangling *object* that is **not** what the code does. The same genuinely‑dangling remnant (a normal object with 3 of 4 `xl.meta` missing) was healed once with `Remove: false` and once with `Remove: true`. Full unedited output:

```text
==================== BLITZY R5 Remove flag: false vs true on a dangling (3 xl.meta missing) NORMAL-object remnant ====================
BLITZY[r5-remove-false] disk 3 xl.meta present BEFORE = true
BLITZY[r5-remove-false] HealObject err=Version not found: blitzyremove/remove-false(null)
BLITZY[r5-remove-false] Before="ok" "ok" "ok" "ok" After="ok" "ok" "ok" "ok"
BLITZY[r5-remove-false] disk 3 xl.meta present AFTER  = false (dangling purge is NOT gated by Remove)
BLITZY[r5-remove-true]  disk 3 xl.meta present BEFORE = true
BLITZY[r5-remove-true]  HealObject err=Version not found: blitzyremove/remove-true(null)
BLITZY[r5-remove-true]  Before="ok" "ok" "ok" "ok" After="ok" "ok" "ok" "ok"
BLITZY[r5-remove-true]  disk 3 xl.meta present AFTER  = false (dangling remnant purged)
```

**Both** runs purged the surviving remnant (disk 3 `xl.meta` `true → false`) and **both** returned the same `Version not found:` error. Grounded in source: the `cannotHeal` branch calls `deleteIfDangling` with a **freshly‑constructed** `ObjectOptions{VersionID: versionID}` — `opts.Remove` is **not forwarded**:

```go
// cmd/erasure-healing.go:438-441
		m, err := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs, dataErrsByPart, ObjectOptions{
			VersionID: versionID,
		})
		errs = make([]error, len(errs))
```

`deleteIfDangling`, once `isObjectDangling` is true [`cmd/erasure-object.go:483`], unconditionally issues `DeleteVersion` on every disk [`cmd/erasure-object.go:548`] and collects each disk's result in the delete loop [`cmd/erasure-object.go:551-560`]. What `opts.Remove` *actually* gates is the removal of **abandoned/stray parts** in `checkAbandonedParts` [`cmd/erasure-healing.go:663`] and empty‑directory healing via `healObjectDir` [`cmd/erasure-healing.go:1053`] — not the dangling‑object purge.

**Reading the all‑`ok` drive arrays for a purge (an honesty note).** The `Before`/`After` arrays print `"ok" "ok" "ok" "ok"` even though three `xl.meta` copies were missing, because the purge branch **resets `errs` to a fresh all‑`nil` slice** (`errs = make([]error, len(errs))`, line 441 above) *before* returning `defaultHealResult`, which then renders every drive `ok` (the `nil → ok` mapping of §8). For this purge case the decisive evidence is therefore **not** the drive‑state array but (a) the on‑disk `xl.meta` going `true → false` on the surviving drive and (b) the `Version not found:` return. The empty versionID is normalized to the `null` version [`cmd/erasure-healing.go:1061`], which is why the error prints `(null)`; `toObjectErr` wraps the internal `errFileVersionNotFound` into the client‑facing `Version not found:` form.

---

## 12. Entry points — when the same `healObject` decision is reached

The decision described above is reached through several real entry points, all of which funnel into the same `healObject` [`cmd/erasure-healing.go:258`]; none was bypassed with a synthetic stand‑in:

- **Admin heal API (operator `mc admin heal`):** `HealHandler` [`cmd/admin-handlers.go:1308`] → `newHealSequence` / `healSequenceStart` [`cmd/admin-heal-ops.go:473,678`] → `ObjectLayer.HealObject` [`cmd/object-api-interface.go:300`] → `erasureObjects.HealObject` [`cmd/erasure-healing.go:1039`] → `healObject`. (This is the path all the live `mc admin heal --json` evidence above exercised.)
- **Background scanner / fresh‑drive auto‑heal:** `healErasureSet` [`cmd/global-heal.go:152`] → background `healObject`.
- **Metadata‑Repair‑Follower (post‑write):** MRF triggers `healObject` [`cmd/mrf.go:272,276`].

Corroboration (documentation, supplementing runtime evidence): the scanner samples "one out of every 1,024 objects" per pass, and "By default, MinIO does not check for bit rot corruption using the scanner"; integrity is verified on the fly during GET/HEAD via HighwayHash. These facts frame *when* the same decision is reached but do not change it. A related boundary nuance (out of scope for the per‑object decision, noted here for completeness, **inferred** from docs not exercised at runtime): a drive offline for more than 48 hours may be treated as a fresh drive for whole‑drive healing — a drive‑lifecycle heuristic, not part of the per‑object shard decision.

---

## 13. Determinism & honesty note

- **Run-to-run stability (>= 2 runs, normalized decision-field diff):** the scratch test (real `HealObject` path, EC:2) was run twice with `-count=1` (forcing a fresh, non-cached execution each time), redirecting each run to its own log. The **raw** `go test` logs are *not* byte-for-byte identical — they carry a per-run timing line (observed `ok  github.com/minio/minio/cmd  0.391s` vs `0.396s`) and, whenever the harness is recompiled, per-run random identifiers (see the honesty caveat below). What **is** byte-for-byte identical across runs is the set of **decision-relevant** fields, which the scratch test prints on dedicated `BLITZY-DECISION` lines that carry no random identifiers. Diffing those lines between the two runs reports **no differences** (exit 0) and their SHA-256 is identical — actual, unedited output:

```text
$ for i in 1 2; do CGO_ENABLED=0 go test -v -count=1 -run TestZzBlitzyScratchEvidence -tags kqueue ./cmd/ \
      | grep BLITZY-DECISION > /tmp/blitzy_decisions_run$i.txt; done
$ diff /tmp/blitzy_decisions_run1.txt /tmp/blitzy_decisions_run2.txt ; echo "diff exit=$?"
diff exit=0
$ sha256sum /tmp/blitzy_decisions_run1.txt /tmp/blitzy_decisions_run2.txt
8e4a9f20687583d086fe1b38f3538646d345798a1044dff8538c78e340cda656  /tmp/blitzy_decisions_run1.txt
8e4a9f20687583d086fe1b38f3538646d345798a1044dff8538c78e340cda656  /tmp/blitzy_decisions_run2.txt
$ cat /tmp/blitzy_decisions_run1.txt
BLITZY-DECISION quorum readQuorum=2 writeQuorum=3 dataBlocks=2 parityBlocks=2 defaultParityCount=2 err=<nil>
BLITZY-DECISION reconstruct-1missing err=<nil> before="missing" "ok" "ok" "ok" after="ok" "ok" "ok" "ok" dataBlocks=2 parityBlocks=2 etagBefore=b561f87202d04959e37588ee05cf5b10 etagAfter=b561f87202d04959e37588ee05cf5b10 presentAfter=true
BLITZY-DECISION corrupt-1xlmeta err=<nil> before="corrupt" "ok" "ok" "ok" after="ok" "ok" "ok" "ok"
BLITZY-DECISION leave-3corrupt err=Storage resources are insufficient for the read operation blitzybucket/leave-3corrupt isReadQuorum=true presentAfter=true etagAfter=b561f87202d04959e37588ee05cf5b10
BLITZY-DECISION purge-3missing err=Version not found: blitzybucket/purge-3missing(null) disk3XlmetaBefore=true disk3XlmetaAfter=false
```

Every decision-relevant value is therefore stable across runs: the observed quorum (`readQuorum=2 writeQuorum=3`), `DataBlocks=2` / `ParityBlocks=2`, all `Before`/`After` drive-state arrays (`missing`/`corrupt` -> `ok`), the **form** of the error strings (the client-facing `InsufficientReadQuorum` wrap of `errErasureReadQuorum` for leave-as-is, and the `Version not found: <bucket>/<object>(<versionID>)` form for a purge), the reconstruct ETag `b561f87202d04959e37588ee05cf5b10` (a content-derived MD5, hence deterministic for the fixed 1 MiB `'x'` payload), the sweep boundary, the dangling booleans, and the set-level purge on-disk before/after (`disk3XlmetaBefore=true` -> `disk3XlmetaAfter=false`). **Honesty caveat — why the raw logs are *not* compared byte-for-byte:** two classes of value that appear in the *full* `HealResultItem` JSON (shown in §5/§6) are **per-run random identifiers, not part of the heal decision**: (i) the per-drive `endpoint` strings (e.g. `/tmp/minio-1815770780`), which are `os.MkdirTemp` paths created by the harness (`getRandomDisks`), and (ii) the randomly-generated **version UUIDs** (a delete-marker `versionId`, which also appears as the `(<versionID>)` component embedded inside a version-scoped `Version not found:` message). These change whenever the harness is recompiled — for example, the independent `corrupt`->`ok` capture embedded in §6 shows a *different* `/tmp/minio-*` endpoint family while **every decision field still matches**. The `BLITZY-DECISION` lines deliberately exclude those identifiers (fixed bucket/object names, no endpoints, no UUIDs), which is exactly why they diff clean and hash-match; on a real cluster the endpoints would be the actual drive paths. The stability guarantee is thus scoped to the decision fields above, not to these random identifiers. Separately, the live `DeleteDanglingObject` audit event reproduces the same **decision** tags on every capture — `caller=/tmp/blitzy/minio/blitzy-629729d3-cb15-4820-bb3e-f65517d9c2ea_444e13/cmd/erasure-healing.go:309`, `d:p=2:2`, `merrs=""`, `derrs=map[]`, `sz=1048576`, `set=0`, `pool=0` — with only per-run fields (`deploymentid`, `time`, `mt`) varying, and the live reconstruct and purge heals produced identical decisions across runs.
- **Observed vs. inferred:** every value in §§3–10 is *observed* runtime output or a directly‑quoted `file:line`. The two items explicitly labelled **(inferred)** are: the 48‑hour offline fresh‑drive heuristic (§12, from documentation, not exercised), and the general framing of scanner sampling frequency (§12, from documentation).
- **The `merrs` empty‑tag finding is a real, runtime‑confirmed source bug** (`joinErrs` iterates its empty local string), reported as observed rather than paraphrased (§7).

---

## 14. Cleanup performed (repository left read‑only)

This was a read‑only investigation. All runtime artifacts live **outside** the repository tree, and the temporary in‑repo scratch test was removed. Cleanup commands:

```bash
# stop the live server and the audit-webhook sink (only the PIDs we spawned)
$ kill "$(cat /tmp/blitzy_minio_pid)" "$(cat /tmp/blitzy_sink_pid)"
# remove the compiled binary, the 4-drive data dirs, the mc config, and all scratch artifacts
$ rm -rf /tmp/minio_bin /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 /tmp/mc-config \
         /tmp/blitzy_* /tmp/environments_files/blitzy_* 2>/dev/null
# remove the temporary scratch heal test from the repo
$ rm -f cmd/zz_blitzy_scratch_test.go
```

Final repository state — the read‑only constraint was honored: **no source, test, or configuration file was modified**, and the temporary in‑repo scratch heal test was removed. The **only** change to the repository is the answer document itself (which is the committed deliverable). Verified with actual output:

```text
$ git status --porcelain
$                                                  # (empty — working tree clean; the deliverable is committed at HEAD)

# exactly one file is added versus the source base commit — the answer document, and nothing else:
$ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2..HEAD
A	blitzy/documentation/minio_c07e5b49d477.md

# no source/test/config file is touched (scoped diff versus the base is empty):
$ git diff --stat c07e5b49d477b0774f23db3b290745aef8c01bd2..HEAD -- cmd internal buildscripts docs .github go.mod go.sum Makefile
$                                                  # (empty)

# the temporary scratch heal test is gone:
$ ls cmd/zz_blitzy_scratch_test.go
ls: cannot access 'cmd/zz_blitzy_scratch_test.go': No such file or directory
```

The answer document `blitzy/documentation/minio_c07e5b49d477.md` is a tracked file — it is the committed deliverable and the single added file in the source-base diff above (`A blitzy/documentation/minio_c07e5b49d477.md`), so `git status --porcelain` on the delivered tree is empty; every **other** tracked file is byte-for-byte unchanged (the scoped `git diff --stat` versus the base over `cmd`, `internal`, `buildscripts`, `docs`, `.github`, `go.mod`, `go.sum`, and `Makefile` is empty), and no untracked scratch file remains under the source tree. The compiled `minio` binary was built at `/tmp/minio_bin/minio`, outside the tree (and `.gitignore` line 4, `minio`, ignores it in any case); the 4-drive data directories, the `mc` config, the audit sink, and all captured logs live under `/tmp`, outside the repository. The temporary scratch heal test `cmd/zz_blitzy_scratch_test.go` was removed and no longer appears in `git status`.

---

### Appendix — full command list used to gather evidence

```bash
# build (outside repo)
CGO_ENABLED=0 go build -tags kqueue -o /tmp/minio_bin/minio .

# shipped heal tests + exact-EC:2 scratch test (real HealObject path)
CGO_ENABLED=0 go test -v -run 'TestHeal|TestIsObjectDangling' -tags kqueue -timeout 900s ./cmd/
CGO_ENABLED=0 go test -v -run 'TestZzBlitzy' -tags kqueue -timeout 900s ./cmd/   # temporary scratch (deleted)

# live 4-drive EC:2 server + operator heal
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  MINIO_AUDIT_WEBHOOK_ENABLE_blitzy=on MINIO_AUDIT_WEBHOOK_ENDPOINT_blitzy=http://127.0.0.1:9999/audit \
  /tmp/minio_bin/minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address :9000 --console-address :9001 &
mc --config-dir /tmp/mc-config alias set local http://127.0.0.1:9000 minioadmin minioadmin
mc --config-dir /tmp/mc-config admin info local
# reconstruct: rm xl.meta on 1 drive, then:
mc --config-dir /tmp/mc-config admin heal --json --force local/<bucket>/<obj>
# purge: rm xl.meta on 3 drives, then the same heal command
# rationale: audit webhook sink captures the DeleteDanglingObject event
mc --config-dir /tmp/mc-config admin trace --call healing --json local
```

