# MinIO Healing: Reconstruct vs. Purge-as-Dangling on a 4-Disk (EC:2) Instance

On a single-node, 4-disk erasure-coded instance, when an object lands in an inconsistent state across drives — some drives hold valid data, some hold corrupted data, and some hold nothing — and healing runs, does MinIO *always* reconstruct the object from whatever valid shards remain, or are there cases where it decides the object should stay deleted or remain degraded? This document answers that question from **observed runtime behavior** on a real build, cross-referenced to the exact decision functions in the source.

**Headline answer: No — MinIO does not always reconstruct.** The heal path contains two safety gates — a parity/quorum gate inside `healObject` (`cmd/erasure-healing.go:428`) and the authoritative `isObjectDangling` predicate (`cmd/erasure-healing.go:968`) — that instead **purge an object as "dangling,"** or **leave it degraded and unreadable**, when too few valid shards survive. Corruption is always healed; only *missing* metadata/parts *beyond parity* cause a purge. The rest of this document demonstrates *both* outcomes (reconstruct and purge/degrade) with verbatim captured output and answers each of the eight decomposed questions (Q1–Q8) explicitly.

## Investigation environment

All evidence below was captured on a MinIO server built from source and run as a single-node 4-drive erasure set inside the designated container. The exact environment:

```text
Source:      module github.com/minio/minio  (go.mod:1), built at commit c07e5b49d
Binary:      minio version DEVELOPMENT.GOGET
             Runtime: go1.23.2 linux/amd64
Topology:    ./minio server /tmp/data{1,2,3,4}
             server log: "Formatting 1st pool, 1 set(s), 4 drives per set."
Profile:     EC:2  (2 data blocks + 2 parity blocks)
             - confirmed by backend layout: a 5 MiB object is stored as
               xl.meta (364 B) + a single part.1 (2,621,600 B) on all 4 drives
               under one shared datadir UUID
             - confirmed by DefaultParityBlocks(4) == 2
               (internal/config/storageclass/storage-class.go:355; case 4, 5: return 2 at :361-362)
Client:      mc version RELEASE.2025-08-13T08-35-41Z
Deployment:  1211f45d-e820-4ef8-b80d-a14b934b1afb
Audit sink:  MINIO_AUDIT_WEBHOOK_ENABLE_sink=on
             MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999
             (required to capture the purge "why" log — see Q5)
```

The 4-disk single-node erasure profile is exactly EC:2 — 2 data + 2 parity. Every boundary result in this document (heal succeeds with up to 2 damaged drives; purge/degrade beyond that) is specific to this profile and is derived from `DefaultParityBlocks(4) == 2`.

## How MinIO decides: reconstruct, purge, or leave degraded

Object healing is driven by `healObject` (`cmd/erasure-healing.go:258`, `func (er *erasureObjects) healObject(...)`). It reads every disk's metadata, classifies each drive, and then routes to one of three outcomes: **reconstruct** the object from surviving shards, **purge** it as dangling, or **leave it degraded** (return a read-quorum error and touch nothing). The control flow is:

```mermaid
flowchart TD
    A[Heal request for object] --> B[readAllFileInfo across 4 disks]
    B --> C{isAllNotFound?}
    C -- yes --> Z1[return errFileNotFound: nothing to heal]
    C -- no --> D[objectQuorumFromMeta: derive readQuorum]
    D --> E{quorum error?}
    E -- yes --> P[deleteIfDangling -> isObjectDangling]
    E -- no --> F[disksWithAllParts: classify each disk]
    F --> G[shouldHealObjectOnDisk per disk -> DriveState]
    G --> H{disksToHealCount == 0?}
    H -- yes --> OK1[Nothing to heal; drives all Ok]
    H -- no --> I{cannotHeal? disksToHealCount > ParityBlocks}
    I -- no --> R[Reconstruct missing/corrupt shards from survivors]
    I -- "yes, and quorumETag empty" --> P
    R --> S[After.Drives state = DriveStateOk]
    P --> Q{isObjectDangling true?}
    Q -- yes --> PD[Purge object as dangling + audit log]
    Q -- no --> RQ[return errErasureReadQuorum: leave degraded]
```

The gates, in the order `healObject` evaluates them:

- **`readAllFileInfo` + `isAllNotFound`** (`cmd/erasure-healing.go:296-305`): after taking the lock, `healObject` re-reads all four disks with `readAllFileInfo` (`cmd/erasure-healing.go:296`). If every disk reports not-found — `if isAllNotFound(errs)` (`cmd/erasure-healing.go:297`) — the object is already fully gone, so healing returns `errFileNotFound` (or `errFileVersionNotFound` when a version ID was supplied). There is nothing to heal.

- **`objectQuorumFromMeta`** (`cmd/erasure-metadata.go:531`): derives `readQuorum` (equal to `dataBlocks`) from the surviving metadata. If quorum cannot be derived, it returns `InsufficientReadQuorum{Err: errErasureReadQuorum, Type: RQInsufficientOnlineDrives}` (`cmd/erasure-metadata.go:552`). In `healObject`, that error (checked at `cmd/erasure-healing.go:308`) routes immediately to the *early* `deleteIfDangling` (`cmd/erasure-healing.go:309`) — the object is either purged or left degraded, without any reconstruction attempt.

- **Per-disk classification** — `disksWithAllParts` (`cmd/erasure-healing-common.go:291`) verifies each disk's parts (stat/size and, in deep scan, bitrot), and `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`) decides, per drive, whether that drive needs healing and *why*. The "why" is mapped to a drive-state string by the switch at `cmd/erasure-healing.go:383-393` (see Q4).

- **The `cannotHeal` gate** (`cmd/erasure-healing.go:428`). The exact expression is:

  ```go
  cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
  ```

  with an immediate override at `cmd/erasure-healing.go:429`:

  ```go
  if cannotHeal && quorumETag != "" {
      // This is an object that is supposed to be removed by the dangling code
      // but we noticed that ETag is the same for all objects, let's give it a shot
      cannotHeal = false
  }
  ```

  When `cannotHeal` is true, `healObject` does **not** reconstruct — it calls `deleteIfDangling` (`cmd/erasure-healing.go:438`) instead. Note the `!latestMeta.Deleted` term: a **delete marker is excluded from this gate entirely** (see Q8).

- **`isObjectDangling`** (`cmd/erasure-healing.go:968`) — the authoritative purge predicate, invoked by `deleteIfDangling`. Its contract comment (`cmd/erasure-healing.go:965-967`) states verbatim:

  > Object is considered dangling/corrupted if and only if total disks - a combination of corrupted and missing files is lesser than number of data blocks.

- **The crux — this is what answers Q2 at the code level.** `isObjectDangling` returns `false` (do **not** purge; attempt to heal) whenever there are any *non-actionable* errors. The guard is:

  ```go
  if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
      return validMeta, false
  }
  ```

  **Corruption is non-actionable → the object is healed, never purged.** Only *missing* metadata or parts that exceed `ParityBlocks` make a regular object dangling. The classification of "missing" vs. "non-actionable" is done by `danglingMetaErrsCount` (`cmd/erasure-healing.go:934`) and `danglingPartErrsCount` (`cmd/erasure-healing.go:950`).

## Answers to Q1–Q8 with captured evidence

### Reproduction setup

Every scenario below starts from the same base: a single-node 4-drive erasure set (EC:2) with one 5 MiB object written so its shard layout is deterministic.

```bash
# Build + run a single-node 4-drive erasure set (EC:2)
make build                       # or: CGO_ENABLED=0 go build -o ./minio ./
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="
# audit sink to capture the purge "why" log (see Q5)
export MINIO_AUDIT_WEBHOOK_ENABLE_sink=on MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999
./minio server /tmp/data1 /tmp/data2 /tmp/data3 /tmp/data4 --address ":9100" &
export MC_HOST_local="http://minioadmin:minioadmin@127.0.0.1:9100"
mc mb local/healbucket
head -c 5242880 /dev/urandom > /tmp/obj1.bin
mc cp /tmp/obj1.bin local/healbucket/obj1     # 5 MiB → xl.meta + part.1 on all 4 drives
```

The backend layout confirmed EC:2: each of `/tmp/data1..4` held `xl.meta` (364 B) and `part.1` (2,621,600 B) under a shared datadir UUID. (The `MINIO_KMS_SECRET_KEY` value above is an ephemeral, local-only demo key used solely to mirror the canonical `go-healing.yml` harness; it is not a real credential.)

Fault injection manipulates those backend files directly. The mechanics are governed by `checkPart()` (`cmd/xl-storage.go:2372`): **truncating** a `part.N` below its expected size yields `checkPartFileCorrupt` (`if st.Size() < expectedSize` at `cmd/xl-storage.go:2398`, `resp = checkPartFileCorrupt` at `:2399`); **deleting** the part datadir yields `checkPartFileNotFound` (`cmd/xl-storage.go:2389` / `:2394`). The `checkPart*` return codes are defined at `cmd/storage-datatypes.go:536-544`:

```go
checkPartUnknown        int = iota   // 0  (storage-datatypes.go:536)
checkPartSuccess                     // 1  (storage-datatypes.go:540)
checkPartDiskNotFound                // 2  (storage-datatypes.go:541)
checkPartVolumeNotFound              // 3  (storage-datatypes.go:542)
checkPartFileNotFound                // 4  (storage-datatypes.go:543)
checkPartFileCorrupt                 // 5  (storage-datatypes.go:544)
```

These numeric codes appear verbatim in the purge audit log's `derrs` tag (Q5).

### Q1 — Inconsistent-state behavior (mix of valid/corrupted/missing shards)

**Answer.** When heal runs on an object whose shards differ across drives, MinIO first classifies each drive as `ok` / `missing` / `corrupt` / `offline`, then — provided the number of drives needing repair does **not** exceed parity — **reconstructs** the missing/corrupt shards from the survivors using Reed-Solomon, rewriting each bad drive so all drives return to `ok`. The per-drive verdict is produced by `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`); the verdict is mapped to a drive-state string by the switch at `cmd/erasure-healing.go:383-393`; and the reconstruction itself happens in the Reed-Solomon decode path (`cmd/erasure-decode.go`).

**Scenario A — one truncated shard.** Corrupt one `part.1` in place, then heal:

```bash
dd if=/dev/urandom of=/tmp/data1/healbucket/obj1/<datadir>/part.1 bs=1 count=1000 conv=notrunc
mc admin heal -r --json local/healbucket/obj1
```

```text
BEFORE color=yellow online=3 offline=0 missing=1 corrupted=0 states=['missing','ok','ok','ok']
AFTER  color=green  online=4 offline=0 missing=0 corrupted=0 states=['ok','ok','ok','ok']
summary: {"objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":5242880,"duration":1}
```

**Important nuance.** A **truncated** part produces the drive-state **`missing`**, not `corrupt`. This is because `shouldHealObjectOnDisk` maps a bad part to `errPartMissingOrCorrupt`, and the switch at `cmd/erasure-healing.go:389` maps `errPartMissingOrCorrupt` to `madmin.DriveStateMissing`. To elicit the literal drive-state **`corrupt`**, you must corrupt the `xl.meta` metadata itself — that is Scenario A2 (below), whose bad metadata falls through to the switch `default` at `cmd/erasure-healing.go:392` → `madmin.DriveStateCorrupt`.

### Q2 — Does MinIO ALWAYS reconstruct?

**Answer: No.** There are two distinct non-reconstruct outcomes, both demonstrated in this document:

- **Reconstruct** (Scenarios A, A2, B): as long as the surviving shards ≥ `dataBlocks`, the object is rebuilt. Heal summary reports `"objects_healed":1`, and every drive returns to `ok`.
- **Purge as dangling** (Scenario Q7 / `obj2`): when parts are missing on more drives than parity, the object is *removed* from all drives. Heal summary reports `"objects_healed":0`, and a `DeleteDanglingObject` audit record is emitted.
- **Leave degraded** (also Scenario Q7): at the S3 read path, an object that has lost quorum but is not purged returns HTTP `503 SlowDownRead` and is left untouched on the backend.

The two governing gates are `cannotHeal` (`cmd/erasure-healing.go:428`) and `isObjectDangling` (`cmd/erasure-healing.go:968`). The crux from the decision model applies directly here: **corruption is non-actionable and is therefore always healed; only missing-beyond-parity is purged.**

**Observable distinction** (this is what makes Q3/Q4 verifiable):

- Reconstruct ⇒ heal summary `"objects_healed":1` + `before.drives` show `missing`/`corrupt` → `after.drives` all `ok`, and the data remains intact on the backend.
- Purge ⇒ heal summary `"objects_healed":0` + heal item `detail:"Object not found: ..."` + the object is physically removed from all backend drives + a `DeleteDanglingObject` audit record is emitted.

### Q3 — Evidence per case (verbatim, not narrative)

Verbatim heal output is provided in each scenario's own section: Scenario A (Q1), Scenario B (Q6), and the Scenario Q7 purge (Q7). Below is the full raw `HealResultItem` JSON for **Scenario A2** — corrupting the `xl.meta` on one drive — which is the canonical shape referenced throughout Q4.

```bash
printf 'GARBAGECORRUPTxlmeta' > /tmp/data1/healbucket/obj1/xl.meta
mc admin heal -r --json local/healbucket/obj1
```

```json
{
  "status": "success",
  "type": "object",
  "name": "healbucket/obj1",
  "before": {
    "color": "yellow", "offline": 0, "online": 3, "missing": 0, "corrupted": 1,
    "drives": [
      {"uuid": "", "endpoint": "/tmp/data1", "state": "corrupt"},
      {"uuid": "", "endpoint": "/tmp/data2", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/data3", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/data4", "state": "ok"}
    ]
  },
  "after": {
    "color": "green", "offline": 0, "online": 4, "missing": 0, "corrupted": 0,
    "drives": [
      {"uuid": "", "endpoint": "/tmp/data1", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/data2", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/data3", "state": "ok"},
      {"uuid": "", "endpoint": "/tmp/data4", "state": "ok"}
    ]
  },
  "size": 5242880
}
```

The corrupted `xl.meta` on `/tmp/data1` is reported as `"state": "corrupt"` before heal and `"state": "ok"` after — the object was reconstructed, confirming that **corruption is healed, not purged** (Q2).


### Q4 — Output indicators (status showing before/after state)

**Answer: Yes.** Each healed object yields a `madmin.HealResultItem` that carries `Before.Drives` and `After.Drives`, each entry being a `HealDriveInfo{UUID, Endpoint, State}`. These items are surfaced to the client as `Items []madmin.HealResultItem` (`cmd/admin-heal-ops.go:87`); the overall sequence status uses the `healStatusSummary` constants at `cmd/admin-heal-ops.go:40-43` (`"not started"`, `"running"`, `"stopped"`, `"finished"`).

The per-drive `State` strings are assigned by the switch in `healObject` (`cmd/erasure-healing.go:383-393`):

- `reason == nil` → `madmin.DriveStateOk` (`cmd/erasure-healing.go:385`)
- `errDiskNotFound` → `madmin.DriveStateOffline` (`cmd/erasure-healing.go:387`)
- `errFileNotFound` / `errFileVersionNotFound` / `errVolumeNotFound` / `errPartMissingOrCorrupt` / `errOutdatedXLMeta` / `errLegacyXLMeta` → `madmin.DriveStateMissing` (`cmd/erasure-healing.go:389`)
- default (all remaining, i.e. corrupt data/metadata) → `madmin.DriveStateCorrupt` (`cmd/erasure-healing.go:392`)

After a successful heal, each healed drive's entry is set to `madmin.DriveStateOk` (`cmd/erasure-healing.go:651`).

**Drive-state string literals (re-confirmed against the module cache).** The state strings are defined in `github.com/minio/madmin-go/v3@v3.0.77/heal-commands.go` and were re-verified line-by-line against the container's Go module cache (`/root/go/pkg/mod/github.com/minio/madmin-go/v3@v3.0.77/heal-commands.go`):

```go
DriveStateOk          string = "ok"                // heal-commands.go:120
DriveStateOffline            = "offline"           // heal-commands.go:121
DriveStateCorrupt            = "corrupt"           // heal-commands.go:122
DriveStateMissing            = "missing"           // heal-commands.go:123
DriveStatePermission         = "permission-denied" // heal-commands.go:124
DriveStateFaulty             = "faulty"            // heal-commands.go:125
DriveStateRootMount          = "root-mount"        // heal-commands.go:126
DriveStateUnknown            = "unknown"           // heal-commands.go:127
DriveStateUnformatted        = "unformatted"       // heal-commands.go:128 (only returned by disk)
```

The `HealDriveInfo` struct (`heal-commands.go:132-136`):

```go
type HealDriveInfo struct {
	UUID     string `json:"uuid"`
	Endpoint string `json:"endpoint"`
	State    string `json:"state"`
}
```

The heal-item type constant is `HealItemObject = "object"` (`heal-commands.go:115`) — this is the `"type": "object"` field seen in the Scenario A2 JSON above.

**Before/after transitions across scenarios:**

| Scenario | fault | before states | after states | outcome |
|----------|-------|---------------|--------------|---------|
| A  | truncate 1 part | `['missing','ok','ok','ok']` (yellow) | `['ok','ok','ok','ok']` (green) | reconstruct |
| A2 | corrupt 1 `xl.meta` | `['corrupt','ok','ok','ok']` (yellow) | `['ok','ok','ok','ok']` (green) | reconstruct |
| B  | 2 damaged shards | `['missing','missing','ok','ok']` (red) | `['ok','ok','ok','ok']` (green) | reconstruct (boundary) |
| Q7 | data lost on 3 drives | n/a (purged) | n/a | **purge (dangling)** |

### Q5 — Do the logs explain WHY? (reconstruct vs. leave alone)

**Answer: Yes, for the purge decision.** `deleteIfDangling` (`cmd/erasure-object.go:482`) emits a `DeleteDanglingObject` audit record via the deferred helper `auditDanglingObjectDeletion` (defined at `cmd/erasure-object.go:451`, deferred at `cmd/erasure-object.go:531`, with `Event: "DeleteDanglingObject"` set at `cmd/erasure-object.go:457`). Its tag keys are: `set`, `pool`, `merrs`, `derrs`, `sz`, `mt`, `d:p`, `invalid`, `offline`, `caller`, and per-disk `ddisk-N`. The captured record for the `obj2` purge (Q7) is, verbatim:

```json
{"version":"1","deploymentid":"1211f45d-e820-4ef8-b80d-a14b934b1afb","time":"2026-07-01T03:42:09.719819483Z","event":"DeleteDanglingObject","trigger":"DeleteDanglingObject","api":{"bucket":"healbucket","objects":[{"objectName":"obj2"}],"rx":0,"tx":0},"tags":{"caller":"/tmp/blitzy/minio/minio_c07e5b49d477_1c89e5/cmd/erasure-healing.go:438","d:p":"2:2","ddisk-0":"<nil>","ddisk-1":"<nil>","ddisk-2":"<nil>","ddisk-3":"<nil>","derrs":"map[0:[4 4 4 1]]","merrs":"","mt":"20260701T034124Z","pool":"0","set":"0","sz":"3000000"}}
```

Reading the tags:

- `d:p="2:2"` — DataBlocks:ParityBlocks for this object (2 data, 2 parity — the EC:2 profile).
- `derrs="map[0:[4 4 4 1]]"` — per-part `checkPart` codes for part index `0`: the four values `4,4,4,1` decode (via `cmd/storage-datatypes.go:536-544`) to `FileNotFound, FileNotFound, FileNotFound, Success`. That is **3 parts missing > 2 parity → dangling**. This is the numeric evidence of *why* the object was purged.
- `caller="...cmd/erasure-healing.go:438"` — the purge was invoked from the `cannotHeal` branch's `deleteIfDangling` call site (`cmd/erasure-healing.go:438`), reached because `cannotHeal` (`cmd/erasure-healing.go:428`) was true.
- `sz="3000000"` — object size in bytes.
- `mt="20260701T034124Z"` — object mod-time.
- `ddisk-N="<nil>"` — each of the four drives' delete operations returned no error (`<nil>`), i.e. the purge succeeded on all four.

**Two honest caveats (both required, and both observed):**

1. **`merrs` is always empty (`""`).** This is a real bug in the source, which is **documented here, not fixed** (this investigation is read-only). The helper `joinErrs` (`cmd/erasure-object.go:467-480`) iterates `for i := range s` (`cmd/erasure-object.go:469`) — that is, it ranges over the *empty result string* `s` (declared `var s string` immediately above) instead of over the `errs` slice. Because `s` starts empty, the loop body never executes and the function always returns `""`. Consequently the `merrs` (meta-errors) tag is always empty in the audit record, exactly as observed above.

2. **The audit record is only emitted when an audit target is configured.** `auditDanglingObjectDeletion` returns early if `len(logger.AuditTargets()) == 0`. That is precisely why the reproduction runs with `MINIO_AUDIT_WEBHOOK_ENABLE_sink=on` / `MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=...` set; without an audit target, the "why" log is silently dropped. Also note: this audit explains the **purge** decision only. A **reconstruct** does *not* emit a comparable per-object "why did I heal" audit — as observed, the reconstruct path's rationale must be inferred from the `HealResultItem` before/after states (Q4), not from a dedicated log line.

### Q6 — Boundary condition (how many valid shards to succeed)

**Answer.** For this 4-disk EC:2 profile, the minimum number of intact shards required for a **successful heal is `dataBlocks` = 2**, and an object survives the loss of up to **`parityBlocks` = 2** drives. The read-quorum floor equals `dataBlocks` and is derived by `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`); the parity count comes from `DefaultParityBlocks(4) == 2` (`internal/config/storageclass/storage-class.go:355`, `case 4, 5: return 2` at `:361-362`). The purge gate is the comparison `disksToHealCount > latestMeta.Erasure.ParityBlocks` in `cannotHeal` (`cmd/erasure-healing.go:428`).

**Scenario B — 2 damaged drives (= parity) → HEALS.** Delete the part on `data1` and truncate the part on `data2`, then heal:

```bash
rm -rf /tmp/data1/healbucket/obj1/<datadir>          # data1: part missing
dd if=/dev/urandom of=/tmp/data2/healbucket/obj1/<datadir>/part.1 bs=1 count=1000 conv=notrunc
mc admin heal -r --json local/healbucket/obj1
```

```text
BEFORE color=red   online=2 offline=0 missing=2 corrupted=0 states=['missing','missing','ok','ok']
AFTER  color=green online=4 offline=0 missing=0 corrupted=0 states=['ok','ok','ok','ok']
summary: {"objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":5242880,"duration":1}
```

With exactly 2 drives damaged (equal to parity) and 2 intact (equal to `dataBlocks`), the object still heals — `"objects_healed":1`, all drives return to `ok`.

**Scenario Q7 — 3 damaged drives (> parity) → PURGES** (detailed in Q7). Conclusion: with **2 of 4** shards surviving (= `dataBlocks`) the object heals; with only **1** surviving (3 lost > 2 parity) it cannot be reconstructed and is purged (or, on the read path, left degraded).


### Q7 — Unrecoverable error (what appears when heal cannot recover)

**Answer.** The unrecoverable condition surfaces at two layers.

**Internal Go error (source literal).** The core error is:

```go
// cmd/erasure-errors.go:23
var errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")
```

The Reed-Solomon reconstruction layer wraps it with an offline-drive count:

```go
// cmd/erasure-decode.go:234
return nil, fmt.Errorf("%w (offline-disks=%d/%d)", errErasureReadQuorum, disksNotFound, len(p.readers))
```

and quorum derivation returns it inside `InsufficientReadQuorum{Err: errErasureReadQuorum, Type: RQInsufficientOnlineDrives}` (`cmd/erasure-metadata.go:552`).

**S3-facing error (observed).** `errErasureReadQuorum` is mapped to the API error `ErrSlowDownRead`:

```go
// cmd/api-errors.go:2190-2191
case errErasureReadQuorum:
    apiErr = ErrSlowDownRead
```

The enum entry is at `cmd/api-errors.go:196`, and its definition (`cmd/api-errors.go:869-873`) is Code `SlowDownRead`, Description `Resource requested is unreadable, please reduce your request rate`, HTTP status `http.StatusServiceUnavailable` (**503**).

Verbatim S3 response for a presigned GET of an object whose data was destroyed on 3 of 4 drives:

```text
HTTP=503
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>obj2</Key><BucketName>healbucket</BucketName><Resource>/healbucket/obj2</Resource><RequestId>18BE0D71C4070F1A</RequestId><HostId>40fd399614142fea3be9690e18526c1881df2b9fc838b215f9c270b056695f9e</HostId></Error>
```

`mc admin trace --errors --json` during the GET confirmed: `api:"s3.GetObject", statusCode:503, statusMsg:"Service Unavailable"`.

And the heal-driven purge of that same dangling object removed it from all four drives:

```bash
mc admin heal -r --json local/healbucket/obj2
```

```text
summary: {"objects_scanned":1,"objects_healed":0,"items_scanned":2,"items_healed":0,"size":0,"duration":1}
```

with the heal item carrying `detail:"Object not found: healbucket/obj2"`. Note `"objects_healed":0` — nothing was reconstructed; the object was purged.

**Honest caveat (required).** The *internal* string `"Read failed. Insufficient number of drives online"` was **not** observed in the server console log (0 relevant lines under `MINIO_CI_CD=1`) nor in the `mc admin trace` `error` field (which was empty) — the storage layer maps it to the S3 API error deep before any logging occurs. It is therefore cited **from source** (`cmd/erasure-errors.go:23`); its observed runtime manifestation is the `SlowDownRead` / HTTP `503` shown above. Likewise, the `(offline-disks=%d/%d)` wrap (`cmd/erasure-decode.go:234`) is **source-verified but was not surfaced** at the S3/trace layer. Both are flagged here as source-verified rather than log-observed.

### Q8 — WRITE vs. DELETE (partial write vs. partial delete)

**Answer: Yes, the paths differ** — both structurally and in code.

**Structural difference.** A partial WRITE leaves data shards (`part.N` files) on the drives that received them. A delete marker (the result of a DELETE on a versioned object) is **metadata-only**: the observed backend for a versioned delete marker (`verbucket/dobj`) had `xl.meta` on all 4 drives and **no** `part.N` files at all. That `xl.meta` held both versions — v1 the PUT (`f1eed848-...`) and v2 the DELETE marker (`335a279e-...`).

**`isObjectDangling` uses two different branches** (`cmd/erasure-healing.go:968`):

- **Regular object** — dangling when `notFoundPartsErrs > validMeta.Erasure.ParityBlocks`. With parity = 2, that means you need ≥ 3 drives with a missing part before the object is considered dangling.
- **Delete marker** — guarded by `validMeta.Deleted`:

  ```go
  if validMeta.Deleted {
      // notFoundPartsErrs is ignored since
      // - delete marker does not have any parts
      dataBlocks := (len(errs) + 1) / 2
      return validMeta, notFoundMetaErrs > dataBlocks
  }
  ```

  With 4 disks, `dataBlocks = (4 + 1) / 2 = 2`, so a delete marker is dangling only when the marker's metadata is missing on ≥ 3 drives. This is a **different key** (metadata-not-found rather than parts-not-found) and a **different threshold formula** than the regular-object branch.

**The `cannotHeal` gate excludes delete markers entirely.** Recall `cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && ...` (`cmd/erasure-healing.go:428`). The `!latestMeta.Deleted` term means a **delete marker is NEVER purged through the `healObject` `cannotHeal` branch** — it is always steered toward reconstruction. A delete marker can only be purged via the *early* `deleteIfDangling` (`cmd/erasure-healing.go:309`), which is reached only when metadata read-quorum genuinely fails.

**Observed — reconstruct a partial DELETE.** Remove the delete marker's `xl.meta` on 2 of 4 drives, then heal:

```bash
rm -f /tmp/data1/verbucket/dobj/xl.meta /tmp/data2/verbucket/dobj/xl.meta
mc admin heal -r --json local/verbucket/dobj
```

```json
{"status":"success","type":"object","name":"verbucket/dobj","before":{"color":"red","offline":0,"online":2,"missing":2,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/data1","state":"missing"},{"uuid":"","endpoint":"/tmp/data2","state":"missing"},{"uuid":"","endpoint":"/tmp/data3","state":"ok"},{"uuid":"","endpoint":"/tmp/data4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/data1","state":"ok"},{"uuid":"","endpoint":"/tmp/data2","state":"ok"},{"uuid":"","endpoint":"/tmp/data3","state":"ok"},{"uuid":"","endpoint":"/tmp/data4","state":"ok"}]},"size":0}
```

The delete marker was reconstructed on all four drives (both versions preserved). In other words, the partial DELETE was **"rolled forward"** — completed across the drives that were missing it — whereas a partial WRITE beyond parity (Q7) was purged, i.e. it **"stayed deleted."** That contrast is the direct answer to Q8.

**Honest caveat (required).** I could **not** force a runtime `DeleteDanglingObject` purge of a *delete marker* in this harness. Removing the delete marker's `xl.meta` on 3 of 4 drives makes the object unlistable, so a recursive heal reports `objects_scanned:0`; a direct (non-recursive) heal returned `objects_healed:0` with server detail `"Object not found: verbucket/dobj"` and emitted **no** `DeleteDanglingObject` audit (the one surviving `xl.meta` copy on `data4` kept it out of the delete-marker dangling branch). The delete-marker dangling branch (`cmd/erasure-healing.go:968`, the `validMeta.Deleted` path) is therefore documented as **verified by code-reading**, not by an observed runtime purge. (Separately: the `mc`-side line `Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0` seen for a 0-parity delete marker is a client-side rendering artifact from `mc`'s heal UI, not a server error.)


## Coverage checklist

- [x] **Q1** — inconsistent-state behavior: classify then reconstruct; demonstrated by Scenario A (truncated part) and Scenario A2 (corrupt `xl.meta`).
- [x] **Q2** — not always reconstruct: both outcomes shown — reconstruct (Scenarios A / A2 / B) and purge (Scenario Q7), plus leave-degraded (HTTP 503).
- [x] **Q3** — verbatim evidence per case: heal `HealResultItem` JSON (A2), heal summaries (A, B, Q7), audit JSON (Q5), S3 503 XML (Q7), delete-marker heal JSON (Q8).
- [x] **Q4** — output indicators: `madmin.HealResultItem` with `Before.Drives[i].State` / `After.Drives[i].State` transitioning `corrupt`/`missing` → `ok`; drive-state literals re-confirmed against `heal-commands.go`.
- [x] **Q5** — log rationale: `DeleteDanglingObject` audit tags (`d:p`, `derrs`, `merrs`, `sz`, `mt`, `caller`, `ddisk-N`), plus the `merrs` bug caveat and the audit-target-required caveat.
- [x] **Q6** — boundary: `dataBlocks` = 2 minimum intact shards; 2 damaged drives heal (Scenario B), 3 damaged drives purge (Scenario Q7).
- [x] **Q7** — unrecoverable error: `"Read failed. Insufficient number of drives online"` (`cmd/erasure-errors.go:23`) → `SlowDownRead` / HTTP `503`, with the source-verified-not-log-observed caveat.
- [x] **Q8** — WRITE vs. DELETE divergence: regular-object vs. `validMeta.Deleted` branches in `isObjectDangling`, delete markers excluded from `cannotHeal`; includes the code-reading caveat for the delete-marker purge.

## Reproducing these results

All commands run inside the designated container, against a fresh single-node 4-drive erasure set. `<datadir>` denotes the per-object datadir UUID directory under each drive (discover it with `ls /tmp/data1/healbucket/obj1/`).

**Setup** (build, run, create bucket + object):

```bash
make build
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="
export MINIO_AUDIT_WEBHOOK_ENABLE_sink=on MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999
./minio server /tmp/data1 /tmp/data2 /tmp/data3 /tmp/data4 --address ":9100" &
export MC_HOST_local="http://minioadmin:minioadmin@127.0.0.1:9100"
mc mb local/healbucket
head -c 5242880 /dev/urandom > /tmp/obj1.bin
mc cp /tmp/obj1.bin local/healbucket/obj1
```

**Fault-injection one-liners:**

```bash
# Scenario A — corrupt (truncate-in-place) one shard  → drive-state "missing", heals
dd if=/dev/urandom of=/tmp/data1/healbucket/obj1/<datadir>/part.1 bs=1 count=1000 conv=notrunc

# Scenario A2 — corrupt the metadata on one drive       → drive-state "corrupt", heals
printf 'GARBAGECORRUPTxlmeta' > /tmp/data1/healbucket/obj1/xl.meta

# Scenario B — 2 damaged drives (= parity)              → heals (boundary)
rm -rf /tmp/data1/healbucket/obj1/<datadir>
dd if=/dev/urandom of=/tmp/data2/healbucket/obj1/<datadir>/part.1 bs=1 count=1000 conv=notrunc

# Scenario Q7 — data destroyed on 3 of 4 drives (> parity) → purged (dangling) / 503 on read
rm -rf /tmp/data1/healbucket/obj2/<datadir> \
       /tmp/data2/healbucket/obj2/<datadir> \
       /tmp/data3/healbucket/obj2/<datadir>

# Scenario Q8 — remove a delete marker's xl.meta on N drives → reconstructs (rolled forward)
rm -f /tmp/data1/verbucket/dobj/xl.meta /tmp/data2/verbucket/dobj/xl.meta
```

**Heal + observe** (per scenario):

```bash
mc admin heal -r --json local/healbucket/obj1      # or obj2 / verbucket/dobj
# purge "why" log arrives at the audit sink on :9999 (Q5)
# unrecoverable read manifests as HTTP 503 SlowDownRead (Q7)
```

**Final note.** Of everything above, only two items are cited from source rather than fully observed at runtime, and both are flagged inline where they appear: (1) the internal error string `"Read failed. Insufficient number of drives online"` (`cmd/erasure-errors.go:23`), whose observed runtime form is `SlowDownRead` / HTTP `503`; and (2) the delete-marker dangling-purge branch (`cmd/erasure-healing.go:968`, `validMeta.Deleted`), which was verified by code-reading because the harness could not force a runtime delete-marker purge. Every other value in this document is quoted verbatim from captured runtime output.

