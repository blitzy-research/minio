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
-rwxr-xr-x 1 root root 156750807 ... /tmp/minio_bin/minio
$ file /tmp/minio_bin/minio
/tmp/minio_bin/minio: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, Go BuildID=..., with debug_info, not stripped
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
   Plus a **temporary scratch test** (`cmd/zz_blitzy_scratch_test.go`, since deleted — see §13) built on the repo's own verified helper `prepareErasure(ctx, 4)` [`cmd/test-utils_test.go:211`], which yields a **clean 4‑disk EC:2** object layer (the exact topology the question asks about; the shipped `TestHeal*` tests use 16/32‑disk sets — see §5 caveat).

2. **Live 4‑drive single‑node server** (`ErasureSetupType`) driven by the operator CLI:
   ```bash
   $ MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
     /tmp/minio_bin/minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address :9000 --console-address :9001 &
   # server log: "Formatting 1st pool, 1 set(s), 4 drives per set."
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
// cmd/erasure-metadata.go:555-565
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

These two numbers — **read quorum 2** and **parity 2** — frame every boundary answer that follows. Corroboration (documentation, supplementing the runtime evidence): MinIO documents that when parity `M` is exactly half the erasure‑set size the write quorum is `K+1`, matching the observed `writeQuorum = 3` for EC:2; and that an object "cannot [be] reconstruct[ed]…that has lost read quorum."

---

## 4. The decision, distilled (with the enclosing code)

`HealObject` (the real `ObjectLayer` entry point) reaches `healObject` [`cmd/erasure-healing.go:258`], which reads metadata from all four disks, computes quorum, classifies each disk, and then takes the determinative `cannotHeal` branch.

**Step 1 — classify each drive** into one of four states [`cmd/erasure-healing.go:383-404`]:

```go
// cmd/erasure-healing.go:383-404
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
// cmd/erasure-object.go:482-488
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
         └─ err? → deleteIfDangling(…, dataErrsByPart=nil) EARLY [erasure-healing.go:309]
     → classify each disk Ok/Missing/Corrupt/Offline [erasure-healing.go:383-404]   (Before/After drives)
     → disksToHealCount == 0? → "object is healthy, nothing to heal"
     → cannotHeal := disksToHealCount > parity [erasure-healing.go:428]
          (override: if all ETags agree, cannotHeal=false [L429-432])
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

All shipped heal tests pass:

```text
$ CGO_ENABLED=0 go test -v -run 'TestHeal|TestIsObjectDangling' -tags kqueue -timeout 900s ./cmd/
--- PASS: TestIsObjectDangling (0.00s)
--- PASS: TestHealing (0.09s)
--- PASS: TestHealingVersioned (0.13s)
--- PASS: TestHealingDanglingObject (0.18s)
--- PASS: TestHealCorrectQuorum (0.39s)
--- PASS: TestHealObjectCorruptedPools (0.60s)
--- PASS: TestHealObjectCorruptedXLMeta (0.30s)
--- PASS: TestHealObjectCorruptedParts (0.21s)
--- PASS: TestHealObjectErasure (0.19s)
--- PASS: TestHealEmptyDirectoryErasure (0.06s)
--- PASS: TestHealLastDataShard (1.25s)
PASS
ok  	github.com/minio/minio/cmd	(cached)
```

### 5.1 Outcome A — RECONSTRUCT (via the real `HealObject`, EC:2, 1 drive `xl.meta` missing)

**Scratch test** (`obj.HealObject`, deep scan, `Remove=true`), full unedited output:

```text
==================== BLITZY OUTCOME reconstruct: 1 drive xl.meta MISSING (deep scan, Remove=true) ====================
BLITZY object presence BEFORE heal: PRESENT (size=1048576, etag=8f293a2f6c19b345152f7a49bb4c643c)
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
      { "uuid": "", "endpoint": "/tmp/minio-4199478986", "state": "missing" },
      { "uuid": "", "endpoint": "/tmp/minio-1324648754", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-2689418314", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-2155655378", "state": "ok" }
    ]
  },
  "after": {
    "drives": [
      { "uuid": "", "endpoint": "/tmp/minio-4199478986", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-1324648754", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-2689418314", "state": "ok" },
      { "uuid": "", "endpoint": "/tmp/minio-2155655378", "state": "ok" }
    ]
  },
  "objectSize": 1048576
}
BLITZY[reconstruct-1missing] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[reconstruct-1missing] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY object presence AFTER  heal: PRESENT (size=1048576, etag=8f293a2f6c19b345152f7a49bb4c643c)
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

A second, operator‑visible flavour of leave‑as‑is: when **all four data shards report corrupt** (only 1 valid, below `dataBlocks=2`) but the metadata copies all agree (so the `quorumETag` override at [`cmd/erasure-healing.go:429-432`] flips `cannotHeal` back to `false` and MinIO *attempts* reconstruction), the rebuild fails for lack of data shards and surfaces the client‑facing read‑quorum error — the object stays in place, neither reconstructed nor purged (full output in §8, sweep case 3).

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
BLITZY[setlevel-purge] err == errFileNotFound ? false
BLITZY[setlevel-purge] HealObject err = Version not found: blitzysignals/setlevel-purge-3missing(null)
BLITZY[setlevel-purge] Before.Drives states = "ok" "ok" "ok" "ok"
BLITZY[setlevel-purge] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[setlevel-purge] disk 0 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 1 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 2 xl.meta present AFTER  = false
BLITZY[setlevel-purge] disk 3 xl.meta present AFTER  = false
```

**Reading:** before heal, disk 3 still held the valid `xl.meta` (the dangling remnant); after heal, **all four disks have no `xl.meta`** — the remnant was physically `DeleteVersion`‑ed. The error is `Version not found` (= `errFileVersionNotFound`), because the empty `versionID` is normalized to `nullVersionID` [`cmd/erasure-healing.go:1062`] and the code returns `errFileVersionNotFound` on a successful dangling delete [`cmd/erasure-healing.go:442-445`].

> **Honesty note on the `Before/After` array for purge:** in the purge path the `errs` slice is reset to `nil` before `defaultHealResult` builds the drive array, so the reported states read `ok`/`ok`/`ok`/`ok` — i.e. the drive‑state array is **not** the purge signal. The authoritative purge signals are (1) the **error return** (`errFileVersionNotFound`/`errFileNotFound`) and (2) the **physical deletion** of the remnant, both shown above.

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

> The `mc`‑side `error` string `"Invalid parity shard count/surplus shard count given…"` is the reedsolomon library complaining that a 0‑parity remnant can't be decoded; the *decision* (purge) is carried by `detail: Object not found` plus the deletion, which correspond to the server's `errFileVersionNotFound`/`errFileNotFound` return.

---

## 6. Q3 — Observable decision signals (the `Before` / `After` drive‑state arrays)

The authoritative decision signal returned by the server is the `madmin.HealResultItem`'s per‑drive **`Before.Drives[].State`** vs. **`After.Drives[].State`** arrays. Values are exactly the four `madmin.DriveState*` constants, serialized lowercase: **`ok`**, **`missing`**, **`corrupt`**, **`offline`** (`madmin-go/v3/heal-commands.go`). They are assembled by the classifier in §4, Step 1 [`cmd/erasure-healing.go:383-404`].

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

Live, the same signal appears in the JSON `before`/`after` `drives[].state` fields, and `mc` additionally computes a summary `color`/`online`/`missing`/`corrupted` header **client‑side** from those states:

```json
"before":{"color":"yellow","online":3,"missing":1,"corrupted":0,"drives":[{"endpoint":"/tmp/d1","state":"missing"}, …]}
"after": {"color":"green","online":4,"missing":0,"corrupted":0,"drives":[{"endpoint":"/tmp/d1","state":"ok"}, …]}
```

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
| `caller` | `…/cmd/erasure-healing.go:309` | `runtime.Caller(1)` [`cmd/erasure-object.go:526-528`] — proves this purge took the **early quorum‑failure path** [`cmd/erasure-healing.go:309`], not the main `cannotHeal` branch at L438. (3 missing metas < read quorum 2 ⇒ `objectQuorumFromMeta` errored ⇒ early `deleteIfDangling`.) |
| `d:p` | `2:2` | `fmt.Sprintf("%d:%d", m.Erasure.DataBlocks, m.Erasure.ParityBlocks)` [`cmd/erasure-object.go:497`] — the object's **data:parity ratio = EC:2**, confirming the config *in the audit trail itself*. |
| `ddisk-0`, `ddisk-1`, `ddisk-2` | `file version not found` | per‑disk `DeleteVersion` result from the delete loop [`cmd/erasure-object.go:551-560`] — those drives had nothing to delete (their metadata was already gone). |
| `ddisk-3` | `<nil>` | disk 3 held the valid remnant; its `DeleteVersion` **succeeded** (nil error) — this is the shard that was actually purged. |
| `sz` | `1048576` | object size [`cmd/erasure-object.go:495`]. |
| `mt` | `20260706T221613Z` | object mod‑time [`cmd/erasure-object.go:496`]. |
| `set` / `pool` | `0` / `0` | erasure‑set and pool index. |
| `derrs` | `map[]` | `fmt.Sprintf("%v", dataErrsByPart)` [`cmd/erasure-object.go:493`]; empty because the early path passes `dataErrsByPart = nil` [`cmd/erasure-healing.go:309`]. |
| `merrs` | `` (empty) | `joinErrs(errs)` [`cmd/erasure-object.go:492`]. **Observed empty** — and this is a genuine source bug, confirmed at runtime: `joinErrs` iterates `for i := range s` over its *empty local string* `s` instead of over `errs`, so it always returns `""` [`cmd/erasure-object.go`]. Reported here as observed, not paraphrased. |

The `caller`, `d:p`, and per‑disk `ddisk-*` tags together answer *why* the object was purged: parity is 2, three of four metadata copies were unrecoverable (below read quorum), so the object could never reach quorum and the sole remnant was deleted.

**Heal trace** (`mc admin trace --call healing --json`) captures the per‑object heal invocation itself, emitted via `healTrace → madmin.TraceHealing` [`cmd/erasure-healing.go:1090`] (full, unedited):

```json
{"status":"success","host":"127.0.0.1:9000","time":"2026-07-06T22:10:50.223841694Z","client":"","duration":15791105,"timeToFirstByte":0,"api":"heal.Object","path":"healbucket/trace.bin","query":"","statusCode":0,"statusMsg":"","type":"Healing","size":1048576,"error":"","extra":{"disks":"4","dry":"false","mode":"1","remove":"false","version-id":"null"}}
```

Here `type:"Healing"`, `api:"heal.Object"`, `extra.disks:"4"`, and `extra.mode:"1"` (the `HealDeepScan` scan‑mode value) show the heal path, scope, and scan mode for the object.

> **For reconstruct and leave‑as‑is, there is no dedicated "reason" audit event** — the rationale is carried by the `Before`/`After` state transition (reconstruct) or by the returned error (`errErasureReadQuorum` for leave‑as‑is). The `DeleteDanglingObject` event is specific to the purge decision.

---

## 8. Q5 — Boundary A: how many valid shards must exist for healing to succeed?

**Answer: at least `dataBlocks` intact shards — i.e. 2 for EC:2 — equivalently, healing succeeds while `disksToHealCount ≤ parityBlocks` (2) and fails once `disksToHealCount > parityBlocks`** [`cmd/erasure-healing.go:428`]. Demonstrated by sweeping the number of *corrupted data shards* from 1 → 3 (metadata intact on all four, deep scan, `Remove=true`), full unedited output per case:

**Sweep case 1 — 1 corrupt (3 valid) → RECONSTRUCT:**

```text
==================== BLITZY SWEEP: 1 of 4 data shards CORRUPT (valid shards=3, parity=2, deep scan) ====================
BLITZY[sweep-1] presence BEFORE: PRESENT (size=1048576, etag=26e2437d8c01f4bceded84f9309ac88f)
BLITZY[sweep-1] HealObject err = <nil>
BLITZY[sweep-1] Before.Drives states = "missing" "ok" "ok" "ok"
BLITZY[sweep-1] After.Drives  states = "ok" "ok" "ok" "ok"
```

**Sweep case 2 — 2 corrupt (2 valid = the boundary) → RECONSTRUCT:**

```text
==================== BLITZY SWEEP: 2 of 4 data shards CORRUPT (valid shards=2, parity=2, deep scan) ====================
BLITZY[sweep-2] presence BEFORE: PRESENT (size=1048576, etag=26e2437d8c01f4bceded84f9309ac88f)
BLITZY[sweep-2] HealObject err = <nil>
BLITZY[sweep-2] Before.Drives states = "missing" "missing" "ok" "ok"
BLITZY[sweep-2] After.Drives  states = "ok" "ok" "ok" "ok"
BLITZY[sweep-2] presence AFTER : PRESENT (size=1048576, etag=26e2437d8c01f4bceded84f9309ac88f)
```

**Sweep case 3 — 3 corrupt (only 1 valid, below `dataBlocks=2`) → CANNOT HEAL (object left in place):**

```text
==================== BLITZY SWEEP: 3 of 4 data shards CORRUPT (valid shards=1, parity=2, deep scan) ====================
BLITZY[sweep-3] presence BEFORE: PRESENT (size=1048576, etag=26e2437d8c01f4bceded84f9309ac88f)
BLITZY[sweep-3] HealObject err = Storage resources are insufficient for the read operation blitzysweep/sweep-3corrupt
BLITZY[sweep-3] Before.Drives states = "corrupt" "corrupt" "corrupt" "corrupt"
BLITZY[sweep-3] After.Drives  states = "corrupt" "corrupt" "corrupt" "corrupt"
BLITZY[sweep-3] presence AFTER : PRESENT (size=1048576, etag=26e2437d8c01f4bceded84f9309ac88f)
```

**The boundary is exactly at 2 valid shards:** 3 or 2 valid → reconstruct; 1 valid → cannot heal. This is precisely `disksToHealCount > parityBlocks` (`2 > 2` is false → heal; `3 > 2` is true → cannot). In case 3 the object is **left in place** (still present, same ETag) — a leave‑as‑is outcome surfaced as the client‑facing `InsufficientReadQuorum` error, because all metadata agreed so the `quorumETag` override attempted a rebuild that then failed for lack of data shards.

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

**Yes.** The divergence lives in `isObjectDangling` [`cmd/erasure-healing.go:968`], which takes a **different arithmetic branch for a delete‑marker (partial delete) than for a normal data object (partial write)**:

```go
// cmd/erasure-healing.go:1008-1035
if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
	return validMeta, false                                  // leave-as-is (undecidable)
}

if validMeta.Deleted {
	// notFoundPartsErrs is ignored since
	// - delete marker does not have any parts
	dataBlocks := (len(errs) + 1) / 2
	return validMeta, notFoundMetaErrs > dataBlocks          // DELETE-MARKER branch (meta-only)
}

// TODO: It is possible to replay the object via just single
// xl.meta file, considering quorum number of data-dirs are still
// present on other drives.
//
// However this requires a bit of a rewrite, leave this up for
// future work.
if notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks {
	return validMeta, true                                   // normal object: metadata beyond parity
}

if !validMeta.IsRemote() && notFoundPartsErrs > 0 && notFoundPartsErrs > validMeta.Erasure.ParityBlocks {
	return validMeta, true                                   // normal object: DATA parts beyond parity
}

return validMeta, false
```

- **Partial delete (delete‑marker):** the `validMeta.Deleted` branch [`cmd/erasure-healing.go:1012-1017`] uses a threshold of `dataBlocks = (len(errs)+1)/2` and **ignores part errors entirely** (a delete marker has no data parts).
- **Partial write (normal data object):** the object is dangling if *either* metadata errors exceed parity [`cmd/erasure-healing.go:1025`] *or* data‑part errors exceed parity [`cmd/erasure-healing.go:1030`] — it checks **both** meta and parts against `ParityBlocks`.

Exercised directly against `isObjectDangling` on a clean EC:2 `FileInfo` (parity = 2), full unedited output:

```text
==================== BLITZY Q7 write-vs-delete: isObjectDangling divergence (EC:2, parity=2) ====================
BLITZY[A normal-obj, 3 xl.meta missing]  parity=2  meta-threshold=parity(2)  => dangling=true (validMeta.Deleted=false)
BLITZY[B delete-marker, 3 xl.meta missing] meta-only branch  dataBlocks=(len(errs)+1)/2=2  => dangling=true (validMeta.Deleted=true)
BLITZY[C normal-obj, parts missing on 3/4, meta intact] parts-threshold=parity(2) => dangling=true (validMeta.Deleted=false)
BLITZY note: delete-marker branch IGNORES part errors (delete markers have no parts); normal-object path checks BOTH meta AND part errors vs parity.
```

**Reading the three cases:**
- **[A]** a partially‑written *normal object* with 3/4 `xl.meta` missing is dangling because `notFoundMetaErrs(3) > parity(2)` [`cmd/erasure-healing.go:1025`]; `validMeta.Deleted == false`.
- **[B]** a partial *delete* (a *delete‑marker* present on only 1/4 drives, missing on 3) is dangling via the **meta‑only** branch with threshold `(len(errs)+1)/2 = 2` and **no** part accounting; `validMeta.Deleted == true`.
- **[C]** a partially‑written normal object whose *metadata is intact* but whose *data parts* are missing on 3/4 drives is dangling via the **parts** branch `notFoundPartsErrs(3) > parity(2)` [`cmd/erasure-healing.go:1030`] — a check that does **not** exist for delete markers.

This is corroborated by the shipped `TestIsObjectDangling` (which uses `newFileInfo(_, 2, 2)` = **exact EC:2**) — its subcases include a normal‑object dangling case, a **delete‑marker** case, and part‑missing cases, all passing:

```text
--- PASS: TestIsObjectDangling/FileInfoDecided-case1 (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case2-delete-marker (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case3-(enough_data-dir_missing) (0.00s)
--- PASS: TestIsObjectDangling/FileInfoDecided-case4-(missing_data-dir_for_part_2) (0.00s)
--- PASS: TestIsObjectDangling/FileInfoUnDecided-case5-(ignore_errFileCorrupt_error) (0.00s)
```

So the **decision mechanism** (dangling ⇒ purge, non‑dangling ⇒ leave‑as‑is) is the same for writes and deletes, but the **dangling test itself uses different arithmetic**: a delete‑marker is judged on metadata copies alone, whereas a data object is judged on metadata *and* data parts against parity.

---

## 11. Entry points — when the same `healObject` decision is reached

The decision described above is reached through several real entry points, all of which funnel into the same `healObject` [`cmd/erasure-healing.go:258`]; none was bypassed with a synthetic stand‑in:

- **Admin heal API (operator `mc admin heal`):** `HealHandler` [`cmd/admin-handlers.go:1308`] → `newHealSequence` / `healSequenceStart` [`cmd/admin-heal-ops.go:473,678`] → `ObjectLayer.HealObject` [`cmd/object-api-interface.go:300`] → `erasureObjects.HealObject` [`cmd/erasure-healing.go:1039`] → `healObject`. (This is the path all the live `mc admin heal --json` evidence above exercised.)
- **Background scanner / fresh‑drive auto‑heal:** `healErasureSet` [`cmd/global-heal.go:152`] → background `healObject`.
- **Metadata‑Repair‑Follower (post‑write):** MRF triggers `healObject` [`cmd/mrf.go:272,276`].

Corroboration (documentation, supplementing runtime evidence): the scanner samples "one out of every 1,024 objects" per pass, and "By default, MinIO does not check for bit rot corruption using the scanner"; integrity is verified on the fly during GET/HEAD via HighwayHash. These facts frame *when* the same decision is reached but do not change it. A related boundary nuance (out of scope for the per‑object decision, noted here for completeness, **inferred** from docs not exercised at runtime): a drive offline for more than 48 hours may be treated as a fresh drive for whole‑drive healing — a drive‑lifecycle heuristic, not part of the per‑object shard decision.

---

## 12. Determinism & honesty note

- **Run‑to‑run stability (≥ 2 runs):** every reported value above was confirmed identical across at least two independent runs. The scratch‑test key values (observed quorum `readQuorum=2 writeQuorum=3`; all `Before`/`After` drive‑state arrays; the three error strings; the sweep boundary; the dangling booleans; the set‑level purge on‑disk before/after) were byte‑identical between run 1 and run 2 (`/tmp/blitzy_scratch_run1.log` vs `/tmp/blitzy_scratch_run2.log`; only randomized temp‑dir endpoint suffixes differ). The live `DeleteDanglingObject` audit event was captured twice (objects `purge.bin` and `purge3.bin`) with identical tags: `caller=…/erasure-healing.go:309`, `d:p=2:2`, `merrs=""`, `derrs=map[]`, `sz=1048576`, `set=0`, `pool=0`; the live reconstruct (`recon2.bin`, `recon3.bin`) and purge (`purge.bin`, `purge3.bin`) produced identical decisions across runs.
- **Observed vs. inferred:** every value in §§3–10 is *observed* runtime output or a directly‑quoted `file:line`. The two items explicitly labelled **(inferred)** are: the 48‑hour offline fresh‑drive heuristic (§11, from documentation, not exercised), and the general framing of scanner sampling frequency (§11, from documentation).
- **The `merrs` empty‑tag finding is a real, runtime‑confirmed source bug** (`joinErrs` iterates its empty local string), reported as observed rather than paraphrased (§7).

---

## 13. Cleanup performed (repository left read‑only)

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

Final repository state — no existing tracked file was modified, and no scratch file remains. `git diff --stat` (tracked‑file changes) is empty, and the only untracked entry is the one new answer document (its parent directories are new):

```text
$ git diff --stat
$                                   # (empty — zero tracked files modified)

$ git status --porcelain
?? blitzy/

$ git status --porcelain --untracked-files=all
?? blitzy/documentation/minio_c07e5b49d477.md
```

(`git status --porcelain` collapses the new tree to `?? blitzy/`; expanding untracked files shows the single new file `blitzy/documentation/minio_c07e5b49d477.md`. The compiled `minio` binary was built at `/tmp/minio_bin/minio`, outside the tree, and `.gitignore` line 4 (`minio`) ignores it in any case. The temporary scratch heal test `cmd/zz_blitzy_scratch_test.go` was removed and no longer appears in `git status`.)

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

