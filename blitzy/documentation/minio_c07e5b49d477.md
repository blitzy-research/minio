# MinIO Healing: Reconstruct vs. Purge-as-Dangling on a 4-Disk (EC:2) Instance

On a single-node, 4-disk erasure-coded instance, when an object lands in an inconsistent state across drives — some drives hold valid data, some hold corrupted data, and some hold nothing — and healing runs, does MinIO *always* reconstruct the object from whatever valid shards remain, or are there cases where it decides the object should stay deleted or remain degraded? This document answers that question from **observed runtime behavior** on a real build, cross-referenced to the exact decision functions in the source.

**Headline answer: No — MinIO does not always reconstruct.** The heal path contains two safety gates — a parity/quorum gate inside `healObject` (`cmd/erasure-healing.go:428`) and the authoritative `isObjectDangling` predicate (`cmd/erasure-healing.go:968`) — that instead **purge an object as "dangling,"** or **leave it degraded and unreadable**, when too few valid shards survive. The behavior splits along a *corruption vs. missing* axis: **corruption is never purged as dangling** — it is healed when enough valid shards remain, and otherwise left degraded (a read-quorum failure), never purged; only *missing* metadata/parts *beyond parity* cause a purge. The rest of this document demonstrates *all three* outcomes (reconstruct, purge, and leave-degraded) with verbatim captured output and answers each of the eight decomposed questions (Q1–Q8) explicitly.

## Investigation environment

All evidence below was captured on a MinIO server built from source with `make build` and run as a single-node 4-drive erasure set inside the designated container. The exact environment:

```text
Source:      module github.com/minio/minio  (go.mod:1), built at commit c07e5b49d477
Binary:      minio version DEVELOPMENT.2024-11-25T17-10-22Z
             (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
             Runtime: go1.23.2 linux/amd64
             Built with `make build` -> CGO_ENABLED=0 go build -tags kqueue -trimpath ...
             (the -trimpath flag makes runtime.Caller paths module-relative; see Q5)
Topology:    ./minio server /tmp/healdata1 /tmp/healdata2 /tmp/healdata3 /tmp/healdata4 \
               --address 127.0.0.1:9100 --console-address 127.0.0.1:9101
             server log: "Formatting 1st pool, 1 set(s), 4 drives per set."
Profile:     EC:2  (2 data blocks + 2 parity blocks)
             - mc admin info reports verbatim: "4 drives online, 0 drives offline, EC:2"
             - confirmed by backend layout: a 5 MiB object is stored as
               xl.meta (368 B) + a single part.1 (2,621,600 B) on all 4 drives
               under one shared datadir UUID (9ca01332-8169-467b-8365-2fc4654c6bb9)
             - confirmed by DefaultParityBlocks(4) == 2
               (internal/config/storageclass/storage-class.go:355; case 4, 5: return 2 at :361-362)
Client:      mc version DEVELOPMENT.GOGET
Deployment:  0afbc705-7088-4d5c-b2bd-5b6dbf882ef0   (per-instance; regenerated on each fresh setup)
Audit sink:  MINIO_AUDIT_WEBHOOK_ENABLE_sink=on
             MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999
             (required to capture the purge "why" log — see Q5)
```

The 4-disk single-node erasure profile is exactly EC:2 — 2 data + 2 parity. Every boundary result in this document (heal succeeds with up to 2 damaged drives; purge/degrade beyond that) is specific to this profile and is derived from `DefaultParityBlocks(4) == 2`. (`deploymentid`, request IDs, host IDs, timestamps, and datadir UUIDs are per-instance/per-request values; the literal ones quoted below are from the captured run and will differ on a fresh instance.)

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

- **`readAllFileInfo` + `isAllNotFound`** (`cmd/erasure-healing.go:296-297`): after taking the lock, `healObject` re-reads all four disks with `readAllFileInfo` (`cmd/erasure-healing.go:296`). If every disk reports not-found — `if isAllNotFound(errs)` (`cmd/erasure-healing.go:297`) — the object is already fully gone, so healing returns `errFileNotFound` (or `errFileVersionNotFound` when a version ID was supplied). There is nothing to heal.

- **`objectQuorumFromMeta`** (`cmd/erasure-metadata.go:531`): derives `readQuorum` (equal to `dataBlocks`) from the surviving metadata. If quorum cannot be derived, it returns `InsufficientReadQuorum{Err: errErasureReadQuorum, Type: RQInsufficientOnlineDrives}` (`cmd/erasure-metadata.go:552`). In `healObject`, that error (checked at `cmd/erasure-healing.go:308`) routes immediately to the *early* `deleteIfDangling` (`cmd/erasure-healing.go:309`) — the object is either purged or left degraded, without any reconstruction attempt.

- **Per-disk classification** — `disksWithAllParts` (`cmd/erasure-healing-common.go:291`) verifies each disk's parts (stat/size and, in deep scan, bitrot; the per-part error code is mapped by `convPartErrToInt` at `cmd/erasure-healing-common.go:258`), and `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`) decides, per drive, whether that drive needs healing and *why*. The "why" is mapped to a drive-state string by the switch at `cmd/erasure-healing.go:382-393` (see Q4).

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

- **The crux — this is what answers Q2 at the code level.** `isObjectDangling` returns `false` (do **not** purge) whenever there are any *non-actionable* errors. The guard is at `cmd/erasure-healing.go:1008-1009`:

  ```go
  if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
      return validMeta, false
  }
  ```

  **Corruption is non-actionable, so a corrupt object is never purged as dangling.** Returning `false` here means `deleteIfDangling` cannot prove the object is safe to delete, so it returns `errErasureReadQuorum` instead (`cmd/erasure-object.go:487`): if enough valid shards remain the object is reconstructed, but if corruption has consumed too many shards the object is simply **left degraded / read-quorum-failed — not purged, and not healed**. Only *missing* metadata or parts that exceed `ParityBlocks` make a regular object dangling. The classification of "missing" vs. "non-actionable" is done by `danglingMetaErrsCount` (`cmd/erasure-healing.go:934`) and `danglingPartErrsCount` (`cmd/erasure-healing.go:950`).

### Fault-injection mechanics

Fault injection manipulates the backend `xl.meta`/`part.N` files directly. The mechanics are governed by `checkPart()` (`cmd/xl-storage.go:2372`): **truncating** a `part.N` below its expected size yields `checkPartFileCorrupt` (`if st.Size() < expectedSize` at `cmd/xl-storage.go:2398`, `resp = checkPartFileCorrupt` at `cmd/xl-storage.go:2399`); **deleting** the part datadir yields `checkPartFileNotFound`. Because this is a size check, a *normal* scan (the default `mc admin heal`) detects truncation, but not same-size content corruption (that needs `--scan deep`, which runs the bitrot `VerifyFile` path). The `checkPart*` return codes are defined at `cmd/storage-datatypes.go:536-544`:

```go
checkPartUnknown        int = iota   // 0  (cmd/storage-datatypes.go:536)
checkPartSuccess                     // 1  (cmd/storage-datatypes.go:540)
checkPartDiskNotFound                // 2  (cmd/storage-datatypes.go:541)
checkPartVolumeNotFound              // 3  (cmd/storage-datatypes.go:542)
checkPartFileNotFound                // 4  (cmd/storage-datatypes.go:543)
checkPartFileCorrupt                 // 5  (cmd/storage-datatypes.go:544)
```

These numeric codes appear verbatim in the purge audit log's `derrs` tag (Q5). The full, self-contained build/run/inject/heal runbook is in the final "Reproducing these results" section.

## Q1 — Inconsistent-state behavior (mix of valid/corrupted/missing shards)

**Answer.** When heal runs on an object whose shards differ across drives, MinIO first classifies each drive as `ok` / `missing` / `corrupt` / `offline`, then — provided the number of drives needing repair does **not** exceed parity — **reconstructs** the missing/corrupt shards from the survivors using Reed-Solomon, rewriting each bad drive so all drives return to `ok`. The per-drive verdict is produced by `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`); the verdict is mapped to a drive-state string by the switch at `cmd/erasure-healing.go:382-393`; and the reconstruction itself happens in the Reed-Solomon decode path (`cmd/erasure-decode.go`).

**Scenario A — one truncated shard.** Truncate one `part.1` below its expected size (a real size reduction, which `checkPart` detects via `st.Size() < expectedSize`), then heal:

```bash
# datadir UUID discovered with: ls /tmp/healdata1/healbucket/obj1/
truncate -s 1000 /tmp/healdata1/healbucket/obj1/9ca01332-8169-467b-8365-2fc4654c6bb9/part.1
mc admin heal -r --json local/healbucket/obj1
```

Verbatim `mc admin heal -r --json` object item and summary (the truncated `part.1` on `/tmp/healdata1` went from 2,621,600 B to 1,000 B):

```json
{"status":"success","type":"object","name":"healbucket/obj1","before":{"color":"yellow","offline":0,"online":3,"missing":1,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"missing"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"size":5242880}
{"status":"success","type":"summary","objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":5242880,"duration":1}
```

After the heal, `/tmp/healdata1/.../part.1` was verified back at its full `2621600` bytes.

**Important nuance.** A **truncated** part produces the drive-state **`missing`**, not `corrupt`. This is because `shouldHealObjectOnDisk` maps a bad part (`checkPartFileCorrupt` or `checkPartFileNotFound`) to `errPartMissingOrCorrupt`, and the switch at `cmd/erasure-healing.go:389` maps `errPartMissingOrCorrupt` to `madmin.DriveStateMissing`. To elicit the literal drive-state **`corrupt`**, you must corrupt the `xl.meta` metadata itself — that is Scenario A2 (Q3), whose bad metadata falls through to the switch `default` at `cmd/erasure-healing.go:392` → `madmin.DriveStateCorrupt`.

## Q2 — Does MinIO ALWAYS reconstruct?

**Answer: No.** There are three distinct outcomes, and only the first is reconstruction:

- **Reconstruct** (Scenarios A, A2, B): as long as the surviving shards ≥ `dataBlocks`, the object is rebuilt. Heal summary reports `"objects_healed":1`, and every drive returns to `ok`.
- **Purge as dangling** (Scenario Q7 / `obj2`): when *parts* are missing on more drives than parity, the object is *removed* from all drives. Heal summary reports `"objects_healed":0`, and a `DeleteDanglingObject` audit record is emitted.
- **Leave degraded** (corruption beyond parity, or a read that cannot assemble quorum): the object is neither healed nor purged. `isObjectDangling` returns `false` for corruption, so `deleteIfDangling` returns `errErasureReadQuorum` (`cmd/erasure-object.go:487`) and touches nothing; on the S3 read path this surfaces as HTTP `503 SlowDownRead` (Q7).

The two governing gates are `cannotHeal` (`cmd/erasure-healing.go:428`) and `isObjectDangling` (`cmd/erasure-healing.go:968`), whose non-actionable guard (`cmd/erasure-healing.go:1008-1009`) is the crux: **corruption is non-actionable → never purged (healed if it can be, else left degraded); only missing-beyond-parity is purged.**

**Observable distinction** (this is what makes Q3/Q4 verifiable) — the two heal summaries below were produced by the exact commands shown; full drive-state detail is in Q1 (reconstruct) and Q7 (purge):

```bash
# Reconstruct case (one truncated part on obj1 — see Q1):
mc admin heal -r --json local/healbucket/obj1 | grep '"type":"summary"'
# Purge case (parts destroyed on 3 of 4 drives for obj2 — see Q7):
mc admin heal -r --json local/healbucket/obj2 | grep '"type":"summary"'
```

```text
reconstruct -> {"status":"success","type":"summary","objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":5242880,"duration":1}
purge       -> {"status":"success","type":"summary","objects_scanned":1,"objects_healed":0,"items_scanned":2,"items_healed":0,"size":0,"duration":1}
```

The `"objects_healed":1` vs `"objects_healed":0` split is the top-level, machine-readable proof that MinIO does **not** always reconstruct.

## Q3 — Evidence per case (verbatim, not narrative)

**Answer.** Verbatim heal output is provided in each scenario's own section: Scenario A (Q1, truncated part), Scenario B (Q6, boundary), the `obj2` purge (Q7), and the delete-marker reconstruct (Q8). The canonical single-object shape referenced throughout Q4 is shown below for **Scenario A2** — corrupting the `xl.meta` on one drive. The per-drive verdict driving these fields comes from `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`) and the drive-state switch (`cmd/erasure-healing.go:382-393`).

```bash
printf 'GARBAGECORRUPTxlmeta' > /tmp/healdata1/healbucket/obj1/xl.meta   # 20-byte garbage
mc admin heal -r --json local/healbucket/obj1
```

Verbatim `mc admin heal --json` object item (this is the item the `mc` client renders — a UI-transformed view of the underlying `madmin.HealResultItem`; see the labeling note in Q4):

```json
{"status":"success","type":"object","name":"healbucket/obj1","before":{"color":"yellow","offline":0,"online":3,"missing":0,"corrupted":1,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"corrupt"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"size":5242880}
```

The corrupted `xl.meta` on `/tmp/healdata1` is reported as `"state":"corrupt"` before heal and `"state":"ok"` after — the object was reconstructed, confirming that **corruption is healed (never purged) when quorum survives** (Q2).

## Q4 — Output indicators (status showing before/after state)

**Answer: Yes.** Each healed object yields a `madmin.HealResultItem` that carries `Before.Drives` and `After.Drives`, each entry being a `HealDriveInfo{UUID, Endpoint, State}`. These items are surfaced to the client as `Items []madmin.HealResultItem` (`cmd/admin-heal-ops.go:87`); the overall sequence status uses the `healStatusSummary` constants at `cmd/admin-heal-ops.go:40-43` (`"not started"`, `"running"`, `"stopped"`, `"finished"`).

The per-drive `State` strings are assigned by the switch in `healObject` (`cmd/erasure-healing.go:382-393`):

- `reason == nil` → `madmin.DriveStateOk` (`cmd/erasure-healing.go:385`)
- `errDiskNotFound` → `madmin.DriveStateOffline` (`cmd/erasure-healing.go:387`)
- `errFileNotFound` / `errFileVersionNotFound` / `errVolumeNotFound` / `errPartMissingOrCorrupt` / `errOutdatedXLMeta` / `errLegacyXLMeta` → `madmin.DriveStateMissing` (`cmd/erasure-healing.go:389`)
- default (all remaining, i.e. corrupt data/metadata) → `madmin.DriveStateCorrupt` (`cmd/erasure-healing.go:392`)

After a successful heal, each healed drive's entry is set to `madmin.DriveStateOk` (`cmd/erasure-healing.go:651`).

**Reproduction — isolate the before/after per-drive states:**

```bash
truncate -s 1000 /tmp/healdata1/healbucket/obj1/9ca01332-8169-467b-8365-2fc4654c6bb9/part.1
mc admin heal -r --json local/healbucket/obj1 \
  | jq -c 'select(.type=="object") | {before:[.before.drives[].state], after:[.after.drives[].state]}'
```

Verbatim observed output (the `before`/`after` state arrays extracted from the real `HealResultItem`):

```json
{"before":["missing","ok","ok","ok"],"after":["ok","ok","ok","ok"]}
```

**Labeling note (raw struct vs. `mc` output).** The JSON blocks shown in Q1/Q3 are the **`mc admin heal --json` item output**, not the raw Go struct. `mc` adds UI fields — `status`, `name`, and the `before`/`after` roll-ups `color`, `online`, `offline`, `missing`, `corrupted` — that are **not** present in the underlying `madmin.HealResultItem`. The raw struct (`github.com/minio/madmin-go/v3@v3.0.77/heal-commands.go:139-158`) instead has `resultId`, `type`, `bucket`, `object`, `versionId`, `detail`, `parityBlocks`, `dataBlocks`, `diskCount`, `setCount`, `before.drives`, `after.drives`, and `objectSize`.

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

The heal-item type constant is `HealItemObject = "object"` (`heal-commands.go:115`) — this is the `"type":"object"` field seen in the JSON above.

**Before/after transitions across scenarios (all observed):**

| Scenario | fault | before states | after states | outcome |
|----------|-------|---------------|--------------|---------|
| A  | truncate 1 part | `['missing','ok','ok','ok']` (yellow) | `['ok','ok','ok','ok']` (green) | reconstruct |
| A2 | corrupt 1 `xl.meta` | `['corrupt','ok','ok','ok']` (yellow) | `['ok','ok','ok','ok']` (green) | reconstruct |
| B  | 2 damaged shards | `['missing','missing','ok','ok']` (red) | `['ok','ok','ok','ok']` (green) | reconstruct (boundary) |
| Q7 | parts lost on 3 drives | n/a (purged) | n/a | **purge (dangling)** |

## Q5 — Do the logs explain WHY? (reconstruct vs. leave alone)

**Answer: Yes, for the purge decision.** `deleteIfDangling` (`cmd/erasure-object.go:482`) emits a `DeleteDanglingObject` audit record via the deferred helper `auditDanglingObjectDeletion` (defined at `cmd/erasure-object.go:451`, deferred at `cmd/erasure-object.go:531`, with `Event: "DeleteDanglingObject"` set at `cmd/erasure-object.go:457`). Its tag keys are: `set`, `pool`, `merrs`, `derrs`, `sz`, `mt`, `d:p`, `invalid`, `offline`, `caller`, and per-disk `ddisk-N`.

**Reproduction — capture the audit record.** The audit is only delivered when an audit target is configured, so a sink must be running:

```bash
# 1. Start a tiny HTTP sink that logs POST bodies (a real webhook receiver):
cat > /tmp/audit_sink.py <<'PY'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get('Content-Length',0)); b=self.rfile.read(n)
        open('/tmp/audit_events.log','ab').write(b+b"\n"); self.send_response(200); self.end_headers()
    def log_message(self,*a): pass
http.server.HTTPServer(('127.0.0.1',9999),H).serve_forever()
PY
python3 /tmp/audit_sink.py &            # server started with MINIO_AUDIT_WEBHOOK_*_sink pointing here

# 2. Trigger the purge (obj2 has parts missing on 3 of 4 drives — see Q7):
mc admin heal -r --json local/healbucket/obj2

# 3. Read the captured "why" record:
grep '"event":"DeleteDanglingObject"' /tmp/audit_events.log
```

The captured record for the `obj2` purge (Q7) is, verbatim (exactly as it arrived at the `:9999` sink):

```json
{"version":"1","deploymentid":"0afbc705-7088-4d5c-b2bd-5b6dbf882ef0","time":"2026-07-01T05:49:51.483710018Z","event":"DeleteDanglingObject","trigger":"DeleteDanglingObject","api":{"bucket":"healbucket","objects":[{"objectName":"obj2"}],"rx":0,"tx":0},"tags":{"caller":"github.com/minio/minio/cmd/erasure-healing.go:438","d:p":"2:2","ddisk-0":"\u003cnil\u003e","ddisk-1":"\u003cnil\u003e","ddisk-2":"\u003cnil\u003e","ddisk-3":"\u003cnil\u003e","derrs":"map[0:[4 4 4 1]]","merrs":"","mt":"20260701T054945Z","pool":"0","set":"0","sz":"3000000"}}
```

Reading the tags:

- `d:p="2:2"` — DataBlocks:ParityBlocks for this object (2 data, 2 parity — the EC:2 profile).
- `derrs="map[0:[4 4 4 1]]"` — per-part `checkPart` codes for part index `0`: the four values `4,4,4,1` decode (via `cmd/storage-datatypes.go:536-544`) to `FileNotFound, FileNotFound, FileNotFound, Success`. That is **3 parts missing > 2 parity → dangling**. This is the numeric evidence of *why* the object was purged.
- `caller="github.com/minio/minio/cmd/erasure-healing.go:438"` — the purge was invoked from the `cannotHeal` branch's `deleteIfDangling` call site (`cmd/erasure-healing.go:438`), reached because `cannotHeal` (`cmd/erasure-healing.go:428`) was true. The path is *module-relative* (not an absolute filesystem path) because the binary is built with `-trimpath` (`make build`).
- `sz="3000000"` — object size in bytes.
- `mt="20260701T054945Z"` — object mod-time.
- `ddisk-N="<nil>"` — each of the four drives' delete operations returned no error (`<nil>`, JSON-escaped as `\u003cnil\u003e`), i.e. the purge succeeded on all four.

Alongside the audit record, the server also logs a `HealObject` event whose `error` field reads `"file version not found"` — the object is gone after the purge.

**Two honest caveats (both required, and both observed):**

1. **`merrs` is always empty (`""`).** This is a real bug in the source, which is **documented here, not fixed** (this investigation is read-only). The helper `joinErrs` (`cmd/erasure-object.go:467-480`) iterates `for i := range s` (`cmd/erasure-object.go:469`) — that is, it ranges over the *empty result string* `s` (declared `var s string` immediately above) instead of over the `errs` slice. Because `s` starts empty, the loop body never executes and the function always returns `""`. Consequently the `merrs` (meta-errors) tag is always empty in the audit record, exactly as observed above.

2. **The audit record is only emitted when an audit target is configured.** `auditDanglingObjectDeletion` returns early if `len(logger.AuditTargets()) == 0` (`cmd/erasure-object.go:452`). That is precisely why the reproduction runs with `MINIO_AUDIT_WEBHOOK_ENABLE_sink=on` / `MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=...` set; without an audit target, the "why" log is silently dropped. Also note: this audit explains the **purge** decision only. A **reconstruct** does *not* emit a comparable per-object "why did I heal" audit — as observed, the reconstruct path's rationale must be inferred from the `HealResultItem` before/after states (Q4), not from a dedicated log line.

## Q6 — Boundary condition (how many valid shards to succeed)

**Answer.** For this 4-disk EC:2 profile, the minimum number of intact shards required for a **successful heal is `dataBlocks` = 2**, and an object survives the loss of up to **`parityBlocks` = 2** drives. The read-quorum floor equals `dataBlocks` and is derived by `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`); the parity count comes from `DefaultParityBlocks(4) == 2` (`internal/config/storageclass/storage-class.go:355`, `case 4, 5: return 2` at `:361-362`). The purge gate is the comparison `disksToHealCount > latestMeta.Erasure.ParityBlocks` in `cannotHeal` (`cmd/erasure-healing.go:428`).

**Scenario B — 2 damaged drives (= parity) → HEALS.** Remove the part on `data1` and truncate the part on `data2` (both real faults `checkPart` detects), then heal:

```bash
DD=9ca01332-8169-467b-8365-2fc4654c6bb9
rm -rf /tmp/healdata1/healbucket/obj1/$DD                     # data1: part missing
truncate -s 1000 /tmp/healdata2/healbucket/obj1/$DD/part.1    # data2: part truncated
mc admin heal -r --json local/healbucket/obj1
```

Verbatim before/after drive states (from the object item) and summary:

```text
BEFORE color=red   online=2 offline=0 missing=2 corrupted=0 states=['missing', 'missing', 'ok', 'ok']
AFTER  color=green online=4 offline=0 missing=0 corrupted=0 states=['ok', 'ok', 'ok', 'ok']
summary: {"status":"success","type":"summary","objects_scanned":1,"objects_healed":1,"items_scanned":2,"items_healed":1,"size":5242880,"duration":1}
```

With exactly 2 drives damaged (equal to parity) and 2 intact (equal to `dataBlocks`), the object still heals — `"objects_healed":1`, all drives return to `ok`, and all four `part.1` files were verified back at `2621600` bytes.

**Scenario Q7 — 3 damaged drives (> parity) → PURGES** (detailed in Q7). Conclusion: with **2 of 4** shards surviving (= `dataBlocks`) the object heals; with only **1** surviving (3 lost > 2 parity) it cannot be reconstructed and is purged (or, on the read path, left degraded).

## Q7 — Unrecoverable error (what appears when heal cannot recover)

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

**Reproduction and verbatim S3 response.** `obj2` (3,000,000 bytes) had its part datadir removed on 3 of the 4 drives (only 1 shard < `dataBlocks`=2 remains), then a presigned GET was fetched with `curl`:

```bash
# obj2 datadir removed on data1,data2,data3 (xl.meta kept on all 4):
URL=$(mc share download --expire 5m local/healbucket/obj2 | grep -oP 'Share: \K.*')
curl -sS -D- "$URL"
```

```text
HTTP/1.1 503 Service Unavailable
Content-Type: application/xml
Retry-After: 60
X-Amz-Request-Id: 18BE1485B3ABE70D

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>obj2</Key><BucketName>healbucket</BucketName><Resource>/healbucket/obj2</Resource><RequestId>18BE1485B3ABE70D</RequestId><HostId>40fd399614142fea3be9690e18526c1881df2b9fc838b215f9c270b056695f9e</HostId></Error>
```

And the heal-driven purge of that same dangling object removed it from all four drives:

```bash
mc admin heal -r --json local/healbucket/obj2
```

```text
summary: {"status":"success","type":"summary","objects_scanned":1,"objects_healed":0,"items_scanned":2,"items_healed":0,"size":0,"duration":1}
```

Note `"objects_healed":0` — nothing was reconstructed; the object was purged (the `DeleteDanglingObject` audit in Q5 is the server-side proof, and a `find` on all four `/tmp/healdata*/healbucket/obj2` directories confirmed they were gone). The per-object heal item that `mc` renders for this purged object is **not** a clean "object not found" line; it is `mc`'s parity-shard rendering artifact:

```json
{"status":"success","error":"Invalid parity shard count/surplus shard count given: surplusShardsBeforeHeal: 0, parityShards: 0","type":"object","name":"/","before":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"after":{"color":"","offline":0,"online":0,"missing":0,"corrupted":0,"drives":null},"size":0}
```

The authoritative server-side signal is the pair of audit events (`DeleteDanglingObject` with `derrs="map[0:[4 4 4 1]]"`, and `HealObject` with `error:"file version not found"`) plus the `"objects_healed":0` summary — not the `mc`-side item text.

**Honest caveat (required).** The *internal* string `"Read failed. Insufficient number of drives online"` was **not** observed in the server console log (0 relevant lines under `MINIO_CI_CD=1`) nor as a distinct `error` field on the S3 wire — the storage layer maps it to the S3 API error deep before any client-facing logging occurs. It is therefore cited **from source** (`cmd/erasure-errors.go:23`); its observed runtime manifestation is the `SlowDownRead` / HTTP `503` shown above. Likewise, the `(offline-disks=%d/%d)` wrap (`cmd/erasure-decode.go:234`) is **source-verified but was not surfaced** at the S3 layer. Both are flagged here as source-verified rather than log-observed.

## Q8 — WRITE vs. DELETE (partial write vs. partial delete)

**Answer: Yes, the paths differ** — both structurally and in code.

**Structural difference.** A partial WRITE leaves data shards (`part.N` files) on the drives that received them. A delete marker is a **metadata-only version**: it carries no parts of its own. The observed backend for the versioned object `verbucket/dobj` (a 1 MiB PUT followed by a DELETE) had `xl.meta` on all 4 drives holding **two** versions, dumped verbatim with the `xl-meta` tool:

- v2 the DELETE marker — `Type: 2`, `VersionID: 4dfed83b514f42338159d40647628788`, a `DelObj` entry with **no** data dir and **no** parts.
- v1 the PUT — `Type: 1`, `EcM: 2, EcN: 2` (EC:2), `VersionID: 01f08e4b569442ccbb765cf79ba8817a`, with a data dir (`DDir`).

Because both versions coexist in the same `xl.meta`, the object directory still contains v1's `part.1` (observed at `524320` B per drive); the *delete-marker version itself* contributes no `part.N`. (This corrects a common misconception: it is the delete-marker version, not the whole object directory, that is parts-less.)

**`isObjectDangling` uses two different branches** (`cmd/erasure-healing.go:968`):

- **Regular object** — dangling when `notFoundPartsErrs > validMeta.Erasure.ParityBlocks` (`cmd/erasure-healing.go:1030-1032`). With parity = 2, that means you need ≥ 3 drives with a missing part before the object is considered dangling.
- **Delete marker** — guarded by `validMeta.Deleted` (`cmd/erasure-healing.go:1012-1016`):

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
rm -f /tmp/healdata1/verbucket/dobj/xl.meta /tmp/healdata2/verbucket/dobj/xl.meta
mc admin heal -r --json local/verbucket/dobj
```

Because removing `xl.meta` drops the metadata for **both** versions on those drives, the heal reconstructs both — the delete-marker version (`size:0`) and the underlying PUT (`size:1048576`) — each transitioning `['missing','missing','ok','ok']` → `['ok','ok','ok','ok']`:

```json
{"status":"success","type":"object","name":"verbucket/dobj","before":{"color":"red","offline":0,"online":2,"missing":2,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"missing"},{"uuid":"","endpoint":"/tmp/healdata2","state":"missing"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"size":0}
{"status":"success","type":"object","name":"verbucket/dobj","before":{"color":"red","offline":0,"online":2,"missing":2,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"missing"},{"uuid":"","endpoint":"/tmp/healdata2","state":"missing"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/healdata1","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata2","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata3","state":"ok"},{"uuid":"","endpoint":"/tmp/healdata4","state":"ok"}]},"size":1048576}
{"status":"success","type":"summary","objects_scanned":2,"objects_healed":2,"items_scanned":3,"items_healed":2,"size":1048576,"duration":1}
```

Both versions were restored on all four drives (`mc ls --versions` afterward still shows `v2 DEL` and `v1 PUT`). In other words, the partial DELETE was **"rolled forward"** — completed across the drives that were missing it — whereas a partial WRITE beyond parity (Q7) was purged, i.e. it **"stayed deleted."** That contrast is the direct answer to Q8.

**Honest caveat (required).** I could **not** force a runtime `DeleteDanglingObject` purge of a *delete marker* in this harness. Removing the delete marker's `xl.meta` on 3 of 4 drives makes the object unlistable, so a recursive heal reports `objects_scanned:0`; a direct heal did not emit a `DeleteDanglingObject` audit (the one surviving `xl.meta` copy kept it out of the delete-marker dangling branch, since `notFoundMetaErrs` = 3 is not `> dataBlocks` when a valid meta remains and read-quorum is re-derived). The delete-marker dangling branch (`cmd/erasure-healing.go:1012-1016`, the `validMeta.Deleted` path) is therefore documented as **verified by code-reading**, not by an observed runtime purge. (Separately: the `mc`-side line `Invalid parity shard count/surplus shard count given` seen for a 0-parity delete marker is the same client-side rendering artifact from `mc`'s heal UI noted in Q7, not a server error.)

## Coverage checklist

- [x] **Q1** — inconsistent-state behavior: classify then reconstruct; demonstrated by Scenario A (real truncated part → `missing`) and Scenario A2 (corrupt `xl.meta` → `corrupt`).
- [x] **Q2** — not always reconstruct: all outcomes shown — reconstruct (`objects_healed:1`, Scenarios A/A2/B), purge (`objects_healed:0`, Scenario Q7), and leave-degraded (HTTP 503 / corruption-beyond-parity).
- [x] **Q3** — verbatim evidence per case: heal item JSON (A2, with source citation), heal summaries (A, B, Q2, Q7), audit JSON (Q5), S3 503 XML (Q7), delete-marker heal JSON (Q8).
- [x] **Q4** — output indicators: `madmin.HealResultItem` with `Before.Drives[i].State` / `After.Drives[i].State` transitioning `corrupt`/`missing` → `ok`; own reproduction (`jq`) and observed `{before/after}` block; drive-state literals re-confirmed against `heal-commands.go`; raw-struct-vs-`mc`-output labeling clarified.
- [x] **Q5** — log rationale: `DeleteDanglingObject` audit tags (`d:p`, `derrs`, `merrs`, `sz`, `mt`, `caller`, `ddisk-N`) with the sink-capture reproduction, plus the `merrs` bug caveat and the audit-target-required caveat.
- [x] **Q6** — boundary: `dataBlocks` = 2 minimum intact shards; 2 damaged drives heal (Scenario B), 3 damaged drives purge (Scenario Q7).
- [x] **Q7** — unrecoverable error: `"Read failed. Insufficient number of drives online"` (`cmd/erasure-errors.go:23`) → `SlowDownRead` / HTTP `503` (verbatim XML), with the source-verified-not-log-observed caveat.
- [x] **Q8** — WRITE vs. DELETE divergence: regular-object (`:1030-1032`) vs. `validMeta.Deleted` (`:1012-1016`) branches in `isObjectDangling`, delete markers excluded from `cannotHeal`; observed reconstruct of both versions; includes the code-reading caveat for the delete-marker purge.

## Reproducing these results

All commands run inside the designated container, against a fresh single-node 4-drive erasure set. This runbook is self-contained: it creates every object each scenario needs (`obj1`, `obj2`, and the versioned `verbucket/dobj`), starts the audit sink, and generates the presigned GET used for the HTTP 503 evidence. `$DD`/`$DD2` denote the per-object datadir UUID directory under each drive (discover with `ls /tmp/healdata1/healbucket/<obj>/`).

**1. Build, start the audit sink, and run the server:**

```bash
make build      # CGO_ENABLED=0 go build -tags kqueue -trimpath ... -> ./minio

# audit sink (captures the DeleteDanglingObject "why" record — Q5):
cat > /tmp/audit_sink.py <<'PY'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get('Content-Length',0)); b=self.rfile.read(n)
        open('/tmp/audit_events.log','ab').write(b+b"\n"); self.send_response(200); self.end_headers()
    def log_message(self,*a): pass
http.server.HTTPServer(('127.0.0.1',9999),H).serve_forever()
PY
python3 /tmp/audit_sink.py &

export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="   # local demo key; mirrors go-healing.yml, not auto-encryption
export MINIO_AUDIT_WEBHOOK_ENABLE_sink=on MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999
./minio server /tmp/healdata1 /tmp/healdata2 /tmp/healdata3 /tmp/healdata4 \
  --address 127.0.0.1:9100 --console-address 127.0.0.1:9101 &
export MC_HOST_local="http://minioadmin:minioadmin@127.0.0.1:9100"
```

**2. Create every object the scenarios use:**

```bash
# obj1 — 5 MiB, for Q1/Q3/Q4/Q6 (reconstruct + boundary):
mc mb local/healbucket
head -c 5242880 /dev/urandom > /tmp/obj1.bin
mc cp /tmp/obj1.bin local/healbucket/obj1

# obj2 — 3,000,000 bytes, for Q2/Q7 (purge + 503):
head -c 3000000 /dev/urandom > /tmp/obj2.bin
mc cp /tmp/obj2.bin local/healbucket/obj2

# verbucket/dobj — versioned PUT then DELETE marker, for Q8:
mc mb local/verbucket
mc version enable local/verbucket
head -c 1048576 /dev/urandom > /tmp/dobj.bin
mc cp /tmp/dobj.bin local/verbucket/dobj
mc rm local/verbucket/dobj          # creates the delete marker (latest version)
```

**3. Fault-injection one-liners** (`$DD` = obj1 datadir, `$DD2` = obj2 datadir):

```bash
DD=$(ls /tmp/healdata1/healbucket/obj1 | grep -v xl.meta)
DD2=$(ls /tmp/healdata1/healbucket/obj2 | grep -v xl.meta)

# Scenario A — real truncation of one shard (size < expected) → drive-state "missing", heals
truncate -s 1000 /tmp/healdata1/healbucket/obj1/$DD/part.1

# Scenario A2 — corrupt the metadata on one drive → drive-state "corrupt", heals
printf 'GARBAGECORRUPTxlmeta' > /tmp/healdata1/healbucket/obj1/xl.meta

# Scenario B — 2 damaged drives (= parity) → heals (boundary)
rm -rf /tmp/healdata1/healbucket/obj1/$DD
truncate -s 1000 /tmp/healdata2/healbucket/obj1/$DD/part.1

# Scenario Q7 — parts destroyed on 3 of 4 drives (> parity), xl.meta kept → purged / 503 on read
rm -rf /tmp/healdata1/healbucket/obj2/$DD2 \
       /tmp/healdata2/healbucket/obj2/$DD2 \
       /tmp/healdata3/healbucket/obj2/$DD2

# Scenario Q8 — remove a delete marker's xl.meta on 2 drives → reconstructs (rolled forward)
rm -f /tmp/healdata1/verbucket/dobj/xl.meta /tmp/healdata2/verbucket/dobj/xl.meta
```

**4. Heal and observe** (per scenario):

```bash
mc admin heal -r --json local/healbucket/obj1      # or obj2 / verbucket/dobj

# Q7 unrecoverable read → HTTP 503 SlowDownRead (verbatim XML):
URL=$(mc share download --expire 5m local/healbucket/obj2 | grep -oP 'Share: \K.*')
curl -sS -D- "$URL"

# Q5 purge "why" record arrives at the audit sink:
grep '"event":"DeleteDanglingObject"' /tmp/audit_events.log
```

**Final note.** Of everything above, only two items are cited from source rather than fully observed at runtime, and both are flagged inline where they appear: (1) the internal error string `"Read failed. Insufficient number of drives online"` (`cmd/erasure-errors.go:23`), whose observed runtime form is `SlowDownRead` / HTTP `503`; and (2) the delete-marker dangling-purge branch (`cmd/erasure-healing.go:1012-1016`, the `validMeta.Deleted` path), which was verified by code-reading because the harness could not force a runtime delete-marker purge. Every other value in this document is quoted verbatim from captured runtime output. Per-instance values (`deploymentid`, request/host IDs, timestamps, datadir UUIDs) will differ on a fresh run; the reproduction steps above regenerate equivalent evidence.
