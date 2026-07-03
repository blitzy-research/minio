# How MinIO's Erasure‑Coding Healing Decides *Repair vs. Leave‑Alone vs. Purge*

**A runtime‑evidenced investigation on a 4‑disk single‑node erasure set (default EC:2 = 2 data + 2 parity), MinIO commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.**

## 1. Summary

This document answers, with **captured runtime evidence**, how MinIO's healing subsystem decides what to do with an object whose shards are in a conflicting/ambiguous state — some drives holding valid data, some corrupted, some empty — across a **4‑disk single‑node erasure set**. Every behavioral claim below sits immediately next to the **verbatim output** that demonstrates it, plus a `file:line` citation into the source at the pinned commit. Statements that were derived by reading code rather than observed at runtime are explicitly labeled **"inferred."**

The short answer: MinIO does **not** always reconstruct. For the default **EC:2** layout (proven below) it resolves ambiguity by comparing surviving shards against a **quorum threshold** computed from parity:

- **≥ 2 valid shards** (read quorum) → the object is **reconstructed** (Reed‑Solomon rebuild) and drive states flip to `ok`.
- **≤ 1 valid shard** and the object is judged **dangling** → it is **purged** by heal `--remove` and **stays deleted** (a client GET returns a read‑quorum error).
- **Insufficient evidence to declare it dangling** (e.g., every `xl.meta` unreadable but no missing parts) → the object is **left alone** (degraded, not purged), and the dangling‑delete path returns `errErasureReadQuorum` without deleting anything.

A partially failed **write** and a partially failed **delete** heal through **different thresholds** (parts‑vs‑parity for writes; metadata‑vs‑data‑blocks for delete markers), which is why the two failure modes are reconciled differently.

All evidence was produced by **building and running MinIO from source** and driving the **real server heal path**, then reading verbatim output — never from code reading alone.

---

## 2. Environment & Canonical Build / Run

**Where this ran.** All build/run steps execute **inside the pinned container** `andrewparkscaleai/coding-agent:minio__minio__c07e5b49d477b0774f23db3b290745aef8c01bd2` (sourced from `ghcr.io/scaleapi/swe-atlas:swe_atlas_QnA_minio_minio_1.0`), whose in‑image source checkout is at repository `HEAD = c07e5b49d477b0774f23db3b290745aef8c01bd2` — identical to the commit under study.

**Canonical build command** (`make build`; target at `Makefile:L177`, command at `Makefile:L179`):

```
@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
```

**Version banner produced by *this* build** (`./minio --version`, verbatim):

```
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.24.3 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

This is the canonical default‑config output at this commit: there is no release git‑tag here, so `gen-ldflags.go` derives a `DEVELOPMENT.<UTC timestamp>` version. The `DEVELOPMENT.2024-11-25T17-10-22Z` tag and `commit-id=c07e5b49d477…` match exactly.

> **Canonical‑configuration caveat (stated explicitly).** The Go runtime in this pinned image is **`go1.24.3`**. An architecture‑phase cross‑check of the *identical* commit reported `go1.23.12` (built in a public `golang:1.23` container). The healing runtime behavior is **commit‑identical**; only the Go patch version differs. I report **my** observed value (`go1.24.3`) and note the discrepancy rather than substituting the reference. `go.mod` declares `go 1.23` [go.mod:L3].

**Canonical run command (4‑disk single‑node → EC:2).** Launched with default credentials/CI flag:

```
export MINIO_ROOT_USER=minio MINIO_ROOT_PASSWORD=minio123 MINIO_CI_CD=1
./minio server /tmp/disk1 /tmp/disk2 /tmp/disk3 /tmp/disk4 --address :9000
```

Startup banner (verbatim) — confirms the single‑node 4‑drive erasure set (`ErasureSetupType`, `cmd/setup-type.go`):

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.24.3 linux/amd64)
```

### 2.1 Heal entry point used — and an important tooling caveat

The intended real entry point is `mc admin heal`, which reaches the server HTTP handler `HealHandler` [cmd/admin-handlers.go:L1308]. **However**, the `mc` client shipped in this image is **`RELEASE.2025-08-13T08-35-41Z`**, in which `mc admin heal` was reworked into a **monitoring‑only** command. Verbatim from `mc admin heal --help`:

```
NAME:
  mc admin heal - monitor healing for bucket(s) and object(s) on MinIO server
```

The classic `-r/--recursive`, `--remove`, and `--scan deep|normal` flags are **absent** (only `--force`, `--verbose`, `--all-drives`, `--json`, etc. remain). Because the CLI can no longer *initiate* a repair/purge, I drove the **identical server heal path** programmatically through the `madmin-go/v3` admin SDK — exactly as the in‑repo reference driver `buildscripts/heal-manual.go` does — using:

```go
opts := madmin.HealOpts{Recursive: true, Remove: <bool>, ScanMode: madmin.HealDeepScan}
start, _, err := madmClnt.Heal(ctx, bucket, prefix, opts, "", false, false)
// poll madmClnt.Heal(..., start.ClientToken, ...) until status.Summary == "finished"
```

This is **not** a bypassing or synthetic interface: `madmClnt.Heal(...)` issues the same admin HTTP request that `mc admin heal` used to issue, landing in the same `HealHandler` [cmd/admin-handlers.go:L1308] and the same heal‑sequence state machine (`cmd/admin-heal-ops.go`) that produces `madmin.HealResultItem` results. Every heal result quoted below is the JSON encoding of a real `madmin.HealResultItem` returned by that path. The temporary driver was compiled against the repository's own `go.mod` (`madmin-go/v3 v3.0.77` [go.mod:L52]) and removed on completion.

**Stability.** Every scenario below was run **at least twice** (in practice 2 runs on a first server instance plus 1 run on a freshly rebuilt binary — 3 total). Reported drive states, error strings, and counts were **identical across runs**; the behavior is deterministic because it is driven by quorum arithmetic, not timing.

---

## 3. R1 — The decision logic under ambiguity

Healing makes **two** distinct decisions. First, **per disk**, "does this copy need repair?" Second, **per object**, "if the object is broken across the set, is it *dangling* (safe to purge) or should it be *left alone*?"

### 3.1 Per‑disk "needs heal" classification — `shouldHealObjectOnDisk` [cmd/erasure-healing.go:L156]

This function returns `(true, <reason>)` when a disk's copy must be healed. The complete set of triggers, each named:

- **`errFileNotFound`, `errFileVersionNotFound`, `errFileCorrupt`** — if the read error is any of these, the disk needs heal [cmd/erasure-healing.go:L157].
- **`errLegacyXLMeta`** — when the metadata read fine but is legacy XLV1 format (`meta.XLV1`) → "heal always."
- **`errOutdatedXLMeta`** — when this disk's metadata does not equal the latest (`!latestMeta.Equals(meta)`).
- **`errPartMissingOrCorrupt`** — when metadata is fine and the object is neither Deleted nor Remote, but a part check returned `checkPartFileNotFound` or `checkPartFileCorrupt`.

*Evidence (this per‑disk classification is exactly what the `Before` drive states expose).* In Case A, damaging the **part** on disk1 and the **metadata** on disk2 produced two different Before states in the real heal output:

```
{"...","before":{"drives":[{"uuid":"","endpoint":"/tmp/disk1","state":"missing"},{"uuid":"","endpoint":"/tmp/disk2","state":"corrupt"},{"uuid":"","endpoint":"/tmp/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/disk4","state":"ok"}]},...}
```

disk1's corrupted **part** surfaced as `errPartMissingOrCorrupt` → `missing`; disk2's corrupted **`xl.meta`** surfaced as `errFileCorrupt` → `corrupt` (state mapping detailed in §6).

### 3.2 Per‑object "purge vs. leave alone" — `isObjectDangling` [cmd/erasure-healing.go:L968]

The governing definition is stated verbatim in the source comment [cmd/erasure-healing.go:L965-L967]:

```
// Object is considered dangling/corrupted if and only
// if total disks - a combination of corrupted and missing
// files is lesser than number of data blocks.
```

The function has three decision regions (all exercised or source‑verified below):

- **No valid metadata** (`!validMeta.IsValid()`): compute `dataBlocks := (len(metaArr) + 1) / 2` [cmd/erasure-healing.go:L991]; if `notFoundPartsErrs > dataBlocks` [cmd/erasure-healing.go:L992] the object is dangling (**purge**); otherwise the *"We have no idea what this file is, leave it as is."* comment applies [cmd/erasure-healing.go:L1004] → `return validMeta, false` [cmd/erasure-healing.go:L1005] (**leave alone**).
- **Valid delete marker** (`validMeta.Deleted`) [cmd/erasure-healing.go:L1012]: `dataBlocks := (len(errs) + 1) / 2` [cmd/erasure-healing.go:L1015]; `return validMeta, notFoundMetaErrs > dataBlocks` [cmd/erasure-healing.go:L1016] — **parts are ignored**.
- **Valid non‑deleted object**: dangling if `notFoundMetaErrs > validMeta.Erasure.ParityBlocks` [cmd/erasure-healing.go:L1025] **or** if `notFoundPartsErrs > validMeta.Erasure.ParityBlocks` [cmd/erasure-healing.go:L1030]; else `return validMeta, false` [cmd/erasure-healing.go:L1035].

The caller that acts on this decision is `deleteIfDangling` [cmd/erasure-object.go:L482]: if `isObjectDangling` says *not* safely deletable, it returns `errErasureReadQuorum` [cmd/erasure-object.go:L487] (comment: "we cannot figure out if the object can be deleted safely"); if dangling, it purges every copy via `DeleteVersion` and emits a `DeleteDanglingObject` audit event.

*Cause → effect summary:* `shouldHealObjectOnDisk` decides **which drives** are wrong; `isObjectDangling` decides whether the object as a whole is **recoverable (repair), unrecoverable‑and‑safe‑to‑delete (purge), or unrecoverable‑but‑ambiguous (leave alone)**. The three runtime outcomes in §5 map one‑to‑one onto these branches.

---

## 4. R2 — The 4‑disk inconsistent‑state scenario (and why it is EC:2)

**Why the layout is EC:2.** For a 4‑drive set, `DefaultParityBlocks(4)` returns **2**: the switch case `case 4, 5:` [internal/config/storageclass/storage-class.go:L361] returns `2` [internal/config/storageclass/storage-class.go:L362]. (Full table for context: `case 1`→`0` [L358]; `case 3, 2`→`1` [L360]; `case 4, 5`→`2` [L362]; `case 6, 7`→`3` [L364]; `default`→`4` [L366].) So the default layout is **2 data + 2 parity = EC:2**.

**On‑disk shard layout.** After `mc cp` of a 1 MiB object to `local/testbucket/testobj`, each of the four drives holds one `xl.meta` (368 bytes) plus the **same** datadir UUID containing exactly one `part.1` (524320 bytes). Verbatim enumeration:

```
--- disk1 ---
  /tmp/disk1/testbucket/testobj/91f39d23-b56a-4706-865f-5e46fbd0683c/part.1  (524320 bytes)
  /tmp/disk1/testbucket/testobj/xl.meta  (368 bytes)
--- disk2 ---
  /tmp/disk2/testbucket/testobj/91f39d23-b56a-4706-865f-5e46fbd0683c/part.1  (524320 bytes)
  /tmp/disk2/testbucket/testobj/xl.meta  (368 bytes)
--- disk3 ---
  /tmp/disk3/testbucket/testobj/91f39d23-b56a-4706-865f-5e46fbd0683c/part.1  (524320 bytes)
  /tmp/disk3/testbucket/testobj/xl.meta  (368 bytes)
--- disk4 ---
  /tmp/disk4/testbucket/testobj/91f39d23-b56a-4706-865f-5e46fbd0683c/part.1  (524320 bytes)
  /tmp/disk4/testbucket/testobj/xl.meta  (368 bytes)
```

*(The datadir UUID is random per PUT; re‑runs produce different UUIDs but the identical structure and the identical 524320‑byte part size.)*

**Shard math proving EC:2 (verbatim computation):**

```
part.1 size on disk      = 524320 bytes
minus HighwayHash cksum  = 32 bytes/shard overhead
=> data per shard        = 524288 bytes
object size 1 MiB        = 1048576 bytes
1048576 / data-per-shard = 2 => object split into 2 data blocks
4 drives, 1 shard each   => 2 data + 2 parity => EC:2  (== DefaultParityBlocks(4)=2)
```

That is: `524320 = 524288 + 32`, where `524288 = 1048576 / 2` (the object is split into **2 data blocks**) and the extra `32` bytes are the per‑shard **HighwayHash bitrot checksum** overhead (bitrot machinery in `cmd/bitrot.go`, `cmd/bitrot-streaming.go`, `cmd/bitrot-whole.go`). Four drives holding one shard each ⇒ **2 data + 2 parity = EC:2**.

**EC:2 is also echoed by the heal path itself.** Healing the healthy object returns a `madmin.HealResultItem` that carries `parityBlocks` and `dataBlocks` directly (verbatim):

```
{"resultId":2,"type":"object","bucket":"testbucket","object":"testobj","versionId":"null","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/disk4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/disk4","state":"ok"}]},"objectSize":1048576}
```

`"parityBlocks":2,"dataBlocks":2,"diskCount":4` confirms EC:2 across the 4‑disk set at runtime, independent of the shard‑byte arithmetic above.

**The inconsistent‑state substrate.** To synthesize the user's "some disks have data, some corrupted, some nothing" condition, the backend `part.N` / `xl.meta` files under each drive are mutated directly:

- **valid** → leave `xl.meta` + `part.1` intact;
- **corrupted data** → overwrite bytes inside `part.1` (triggers HighwayHash bitrot → `errFileCorrupt`, classified as `errPartMissingOrCorrupt`);
- **corrupted metadata** → overwrite `xl.meta` with garbage (→ `errFileCorrupt`);
- **nothing** → delete `part.1` or the whole object directory (→ `errFileNotFound` / `errFileVersionNotFound`).

§5 applies these mutations in specific combinations and heals through the real path.

---

## 5. R3 / R4 — Does MinIO *always* reconstruct? No. Three outcomes, each with evidence

The direct answer to the user's question — *"Does MinIO always reconstruct… or are there situations where it decides the object should stay deleted or degraded?"* — is: **it does not always reconstruct.** There are three distinct outcomes, demonstrated below. A fourth sub‑case (D) contrasts partial‑write vs. partial‑delete healing.

### 5.1 Case A — Reconstructable (repair)

**Setup:** on **disk1** overwrite ~20 bytes of `part.1` (bitrot → corrupt part); on **disk2** overwrite `xl.meta` with garbage; leave **disk3/disk4** intact (≥ 2 valid shards remain). Heal with deep scan, `Remove:false`.

**Observed heal result (verbatim `madmin.HealResultItem`):**

```
{"resultId":2,"type":"object","bucket":"testbucket","object":"testobj","versionId":"null","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/disk1","state":"missing"},{"uuid":"","endpoint":"/tmp/disk2","state":"corrupt"},{"uuid":"","endpoint":"/tmp/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/disk4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/disk1","state":"ok"},{"uuid":"","endpoint":"/tmp/disk2","state":"ok"},{"uuid":"","endpoint":"/tmp/disk3","state":"ok"},{"uuid":"","endpoint":"/tmp/disk4","state":"ok"}]},"objectSize":1048576}
```

Distilled (Before → After, disk1..disk4), **identical across runs**:

```
BEFORE : ['missing', 'corrupt', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
AFTER  : ['ok', 'ok', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
```

**Data integrity after heal (verbatim):**

```
original=78e672d4ca019e7c99cc4965a4bf82b9  got=78e672d4ca019e7c99cc4965a4bf82b9  match=YES
```

*Interpretation:* with 2 valid shards surviving, heal **reconstructed** the object via Reed‑Solomon `e.DecodeDataAndParityBlocks(ctx, bufs)` [cmd/erasure-decode.go:L348] (backed by `reedsolomon v1.12.4` [go.mod:L40]), rewrote the missing/corrupt shards, and flipped both bad drives to `ok`. The post‑heal GET md5 equals the original ⇒ full recovery. This is the **repair** outcome.

### 5.2 Case B — Unrecoverable / dangling ⇒ **stays deleted**

**Setup:** keep `xl.meta` intact on all 4 drives, delete `part.1` on disks **1, 2, 3** (only disk4 retains a data shard → **1** valid data shard, below `dataBlocks = 2`). GET, then heal with `Remove:true`.

**GET at lost read quorum (verbatim client error):**

```
mc: <ERROR> Unable to read from `local/testbucket/testobj`. Resource requested is unreadable, please reduce your request rate.
```

**The same GET with `--debug` (verbatim HTTP status + S3 error body):**

```
mc: <DEBUG> HTTP/1.1 503 Service Unavailable
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>testobj</Key><BucketName>testbucket</BucketName>...</Error>
```

This is the client rendering of the internal `errErasureReadQuorum` = `"Read failed. Insufficient number of drives online"` [cmd/erasure-errors.go:L23], mapped by `toAPIError` (`case errErasureReadQuorum: apiErr = ErrSlowDownRead`) [cmd/api-errors.go:L2190-L2191] to `ErrSlowDownRead` — `Code:"SlowDownRead"`, HTTP `503 Service Unavailable` [cmd/api-errors.go:L869-L872].

**Heal `Remove:true` purges the dangling object (verbatim `HealResultItem`):**

```
{"resultId":2,"type":"object","bucket":"","object":"","versionId":"","detail":"Object not found: testbucket/testobj","diskCount":0,"setCount":0,"before":{"drives":null},"after":{"drives":null},"objectSize":0}
```

**On‑disk after purge (verbatim):**

```
disk1: GONE
disk2: GONE
disk3: GONE
disk4: GONE
```

**Post‑purge GET (verbatim) — it stays deleted:**

```
mc: <ERROR> Unable to read from `local/testbucket/testobj`. Object does not exist.
```

*Interpretation:* with only 1 valid data shard (< `dataBlocks = 2`), `isObjectDangling` returns *true* via the non‑deleted parts branch `notFoundPartsErrs > validMeta.Erasure.ParityBlocks` (3 > 2) [cmd/erasure-healing.go:L1030]. `deleteIfDangling` [cmd/erasure-object.go:L482] then purges every copy. The object **stays deleted** — this is the "should stay deleted" outcome the user asked about.

### 5.3 Case C — Leave‑alone / degraded (**not** purged)

**Setup:** corrupt `xl.meta` on **all 4** drives, leave every `part.1` intact. Heal with `Remove:true` — it must **not** purge.

**Observed heal result (verbatim, distilled):**

```
meta   : {"detail": "file is corrupted", "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "objectSize": 0}
BEFORE : ['ok', 'ok', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
AFTER  : ['ok', 'ok', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
```

**On‑disk after heal — still present on all 4 drives (verbatim):**

```
disk1: PRESENT (e7217932-d945-4984-9158-a544a94a1a8c xl.meta )
disk2: PRESENT (e7217932-d945-4984-9158-a544a94a1a8c xl.meta )
disk3: PRESENT (e7217932-d945-4984-9158-a544a94a1a8c xl.meta )
disk4: PRESENT (e7217932-d945-4984-9158-a544a94a1a8c xl.meta )
```

**Degraded GET (verbatim):**

```
mc: <ERROR> Unable to read from `local/testbucket/testobj`. We encountered an internal error, please try again.: cause(file is corrupted).
```

*Interpretation:* every `xl.meta` is unreadable, so `isObjectDangling` takes the `!validMeta.IsValid()` path; but `notFoundPartsErrs (0) <= dataBlocks (2)`, so it hits the *"We have no idea what this file is, leave it as is."* branch [cmd/erasure-healing.go:L1004] → `return false` [cmd/erasure-healing.go:L1005]. `deleteIfDangling` therefore returns `errErasureReadQuorum` **without purging** [cmd/erasure-object.go:L487]. The object is **left in place, degraded** — the "or degraded" outcome. Even with `--remove`, MinIO refuses to delete because it cannot prove the object is dangling.

### 5.4 Case D — Partial WRITE vs. partial DELETE (see §10 for the full R7c treatment)

- **Partial write (observed):** Case B is a partially‑failed write (parts missing, no delete marker). The audit event (§7) shows `derrs = "map[0:[4 4 4 1]]"` ⇒ part FileNotFound on disks 1‑3, present on disk4 ⇒ `notFoundPartsErrs = 3 > parityBlocks = 2` ⇒ dangling via [cmd/erasure-healing.go:L1030].
- **Partial delete (source‑verified, with an observed anchor):** a delete on a versioned bucket creates a **0‑byte delete marker** as a separate version. Verbatim `mc ls --versions`:

```
[2026-07-02 23:51:19 UTC]     0B STANDARD c584d42f-9b55-473c-a99b-9cdc8d29a6ee v2 DEL vobj
[2026-07-02 23:51:19 UTC] 1.0MiB STANDARD c9b9aa95-d7cd-4b0a-a564-3cc7ddb05d33 v1 PUT vobj
```

The delete marker (`v2 DEL`, `0B`) has **no data parts**, which is exactly why the delete‑marker branch of `isObjectDangling` **ignores parts** and tests `notFoundMetaErrs > dataBlocks` [cmd/erasure-healing.go:L1012-L1016] instead. A clean runtime isolation of the on‑disk delete‑marker dangling decision was **muddied** because the prior `v1 PUT` data version is retained under versioning; therefore the delete‑marker threshold is presented as **source‑verified** (not independently observed), while the write‑side threshold is **observed** (Case B / audit `derrs`).

---

## 6. R5 — Healing output & the before/after status indicators

The decision is exposed through `madmin.HealResultItem` (type from `madmin-go/v3 v3.0.77` [go.mod:L52]), which carries a **`Before`** and an **`After`** set of per‑drive `HealDriveInfo{UUID, Endpoint, State}` records. In the JSON above, the per‑drive field is `"state"`. There are exactly **four** state literals, all set in `healObject` [cmd/erasure-healing.go:L258] by the Before‑state switch [cmd/erasure-healing.go:L383-L392], and I produced **each one at runtime** by damaging a different artifact:

- **`madmin.DriveStateOk`** = `"ok"` — set when `reason == nil` [cmd/erasure-healing.go:L385]; also the After state each healed drive is flipped to after a successful `RenameData` [cmd/erasure-healing.go:L651]. *Observed:* disk3/disk4 in Case A (`"state":"ok"`).
- **`madmin.DriveStateOffline`** = `"offline"` — set on `errDiskNotFound` [cmd/erasure-healing.go:L387]. *Observed by taking a drive offline* (moving its directory away) alongside a corrupt part on disk2:

```
BEFORE : ['offline', 'missing', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
AFTER  : ['offline', 'ok', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
```

Raw evidence for the offline drive (verbatim excerpt of the `before.drives` entry):

```
{"uuid":"","endpoint":"/tmp/disk1","state":"offline"}
```

(The offline drive stays `offline` in `After` — heal cannot repair a drive that is not present — while disk2's corrupt part is reconstructed to `ok`.)

- **`madmin.DriveStateMissing`** = `"missing"` — set for `errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errPartMissingOrCorrupt`, `errOutdatedXLMeta`, or `errLegacyXLMeta` [cmd/erasure-healing.go:L389]. *Observed:* disk1 in Case A (corrupted **part** → `errPartMissingOrCorrupt` → `"missing"`).
- **`madmin.DriveStateCorrupt`** = `"corrupt"` — the `default` branch, covering `errFileCorrupt` [cmd/erasure-healing.go:L392]. *Observed:* disk2 in Case A (corrupted **`xl.meta`** → `errFileCorrupt` → `"corrupt"`).

**Key nuance (cause → effect): the `Before` state depends on *which artifact* is damaged.**

| Damaged artifact | Read error (reason) | `Before` state | file:line |
| --- | --- | --- | --- |
| `part.N` bytes overwritten/deleted | `errPartMissingOrCorrupt` | `missing` | L389 |
| `xl.meta` content garbage | `errFileCorrupt` | `corrupt` | L392 |
| whole object dir removed | `errFileNotFound`/`errFileVersionNotFound` | `missing` | L389 |
| drive offline (path gone) | `errDiskNotFound` | `offline` | L387 |
| intact | `nil` | `ok` | L385 |

So the healing output reveals the decision as a **before → after transition per drive**: `missing`/`corrupt`/`offline` entries in `Before` name the exact problem per disk; `ok` entries in `After` prove which ones were repaired. The `HealResultItem` also carries `detail` (e.g., `""` on success, `"file is corrupted"` in Case C, `"Object not found: testbucket/testobj"` on a dangling purge), plus `parityBlocks`, `dataBlocks`, `diskCount`, and `objectSize`.

---

## 7. R6 — Do the logs explain *why* MinIO restored vs. left something alone?

Yes — but the definitive "why" for a **purge** decision is emitted as a structured **audit** event, and it only fires when an audit target is configured. `auditDanglingObjectDeletion` [cmd/erasure-object.go:L451] returns early when there are no audit targets, so a sink must be enabled first:

```
export MINIO_AUDIT_WEBHOOK_ENABLE=on MINIO_AUDIT_WEBHOOK_ENDPOINT=http://127.0.0.1:8899/audit
```

With a local HTTP sink capturing the POSTed audit JSON, triggering the Case B purge produced this **`DeleteDanglingObject`** event (verbatim `tags`):

```json
{"caller": "github.com/minio/minio/cmd/erasure-healing.go:438", "d:p": "2:2", "ddisk-0": "<nil>", "ddisk-1": "<nil>", "ddisk-2": "<nil>", "ddisk-3": "<nil>", "derrs": "map[0:[4 4 4 1]]", "merrs": "", "mt": "20260702T234920Z", "pool": "0", "set": "0", "sz": "1048576"}
```

Decoding the "why" from this event:

- **`"d:p": "2:2"`** — the object's data:parity ratio, confirming **EC:2** in the audit stream.
- **`"derrs": "map[0:[4 4 4 1]]"`** — per‑part check codes across the 4 drives. Using the `checkPart*` constants [cmd/storage-datatypes.go:L536-L544] (`checkPartSuccess = 1`, `checkPartFileNotFound = 4`), `[4 4 4 1]` = part **FileNotFound** on disks 1‑3, **Success** on disk4. That is `notFoundPartsErrs = 3`, which exceeds `parityBlocks = 2` ⇒ the object is dangling ⇒ purge. This event *is* the recorded rationale for "why it was deleted."
- **`"merrs": ""`** — always empty. This is a genuine bug in `joinErrs` [cmd/erasure-object.go:L467]: the loop iterates `for i := range s` over the empty separator string `s` instead of over the `errs` slice, so the body never runs. Reported **as observed** (not "corrected").
- **`"sz": "1048576"`**, **`"pool": "0"`**, **`"set": "0"`**, **`"ddisk-0".."ddisk-3": "<nil>"`** — object size (1 MiB), pool/set indices, and per‑disk delete errors (`<nil>` = deleted without error on every drive).
- **`"caller": "…/cmd/erasure-healing.go:438"`** — the source location that raised the dangling deletion.

**Server log for the "left alone / degraded" case.** When a corrupt `xl.meta` is read (Case C), the server logs an error explaining the read failure. My **observed** log line (stable, appearing repeatedly), verbatim:

```
Error: file is corrupted (cmd.StorageErr)
       GetObjectInfo="name=testobj,pool=1,set=1"
       6: internal/logger/logger.go:268:logger.LogIf()
       5: cmd/logging.go:112:cmd.internalLogIf()
       4: cmd/api-errors.go:2581:cmd.toAPIError()
       3: cmd/object-handlers.go:876:cmd.objectAPIHandlers.headObjectHandler()
       2: cmd/object-handlers.go:1030:cmd.objectAPIHandlers.HeadObjectHandler()
```

> **Discrepancy reported honestly.** An architecture‑phase reference expected a JSON‑parser line of the form `readObjectStart: expect { or n, but found G, … GARBAGE-NOT-VALID-XLMETA`. That specific line was **not observed** in this build/run; instead the corrupt `xl.meta` surfaces to the client path as `Error: file is corrupted (cmd.StorageErr)` with the stack trace above (flowing through `cmd/api-errors.go:2581:cmd.toAPIError()`). I report what I actually observed rather than the reference wording.

*Net:* for a **purge**, the `DeleteDanglingObject` audit event records the exact per‑part evidence (`derrs`) and ratio (`d:p`) behind the decision; for a **leave‑alone/degraded** read, the server logs `file is corrupted`. Statements about audit fields not present in the captured event (none needed here) would be labeled *inferred*; everything quoted above was observed.

---

## 8. R7a — How many valid shards are needed for healing to succeed?

**Boundary: reconstruction succeeds with ≥ 2 valid shards and fails at ≤ 1**, because for EC:2 the **read quorum equals the data‑block count = 2**.

**Source of the threshold** — `objectQuorumFromMeta` [cmd/erasure-metadata.go:L531]:

```
dataBlocks := len(partsMetaData) - parityBlocks   // L555  -> 4 - 2 = 2
writeQuorum := dataBlocks                          // L557  -> 2
if dataBlocks == parityBlocks {                    // L558  (2 == 2, true)
    writeQuorum++                                  // L559  -> 3
}
return dataBlocks, writeQuorum, nil                // L564  -> readQuorum == dataBlocks == 2
```

So for the default 4‑disk EC:2 layout: **`readQuorum = dataBlocks = 2`** and **`writeQuorum = dataBlocks + 1 = 3`** (incremented precisely because `dataBlocks == parityBlocks`).

**Observed at the boundary — exactly 2 valid shards reconstructs.** Deleting `part.1` on 2 of 4 drives (leaving 2 valid shards == read quorum), the object is *readable before heal* and *both missing shards are reconstructed* (verbatim, distilled):

```
GET at exactly-quorum (before heal):
  md5 match before heal: YES
BEFORE : ['missing', 'missing', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
AFTER  : ['ok', 'ok', 'ok', 'ok'] (disk1,disk2,disk3,disk4)
  md5 match after heal:  YES
```

**Observed below the boundary — 1 valid shard fails.** Case B (only disk4 retains a data shard → 1 < 2) is unreadable (`SlowDownRead`/503) and heal declares it dangling and purges it (§5.2). Together: **2 valid shards → recovers; 1 valid shard → unrecoverable.** This also matches the design intent that at least *parity‑many* shards (2) must remain to reconstruct.

---

## 9. R7b — The exact error when healing cannot recover an object

The canonical internal literal is **`errErasureReadQuorum`** = `"Read failed. Insufficient number of drives online"` [cmd/erasure-errors.go:L23]. `deleteIfDangling` returns exactly this error when it cannot prove the object is safely deletable [cmd/erasure-object.go:L487].

**Observed client rendering** (from Case B's GET, verbatim): the internal error is translated by `toAPIError` [cmd/api-errors.go:L2190-L2191] to `ErrSlowDownRead` — `Code:"SlowDownRead"`, HTTP `503 Service Unavailable` [cmd/api-errors.go:L869-L872]:

```
mc: <DEBUG> HTTP/1.1 503 Service Unavailable
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message>...</Error>
```

**Sibling literals (named for completeness):**

- **`errErasureWriteQuorum`** = `"Write failed. Insufficient number of drives online"` [cmd/erasure-errors.go:L26].
- **`errNoHealRequired`** = `"No healing is required"` [cmd/erasure-errors.go:L28-L29].
- **Heal write‑failure edge case (source‑verified):** when reconstruction cannot write any healed shard, `healObject` returns `fmt.Errorf("all drives had write errors, unable to heal %s/%s", bucket, object)` [cmd/erasure-healing.go:L615]. *(This path was not triggered in the scenarios above — it requires all drives to fail writes during heal — so it is labeled **inferred/source‑verified**, not observed.)*

---

## 10. R7c — Does healing behave differently for a partial write vs. a partial delete? Yes.

They are judged by **two different `isObjectDangling` branches with different operands and thresholds**:

- **Partial write** (non‑deleted object with missing/corrupt parts): dangling iff **`notFoundPartsErrs > validMeta.Erasure.ParityBlocks`** [cmd/erasure-healing.go:L1030]. The operand is **missing PARTS**, compared against **parity** (2). **Observed** (Case B audit `derrs = "map[0:[4 4 4 1]]"`): `notFoundPartsErrs = 3 > parityBlocks = 2` ⇒ dangling ⇒ purge.
- **Partial delete** (a valid **delete marker** present on a subset): dangling iff **`notFoundMetaErrs > dataBlocks`**, with `dataBlocks := (len(errs) + 1) / 2` and **parts ignored** [cmd/erasure-healing.go:L1012-L1016]. The operand is **missing METADATA copies**, compared against **data blocks** (2). **Source‑verified**; the observed anchor is that a delete marker is a `0B` `v2 DEL` version with no parts (§5.4), which is *why* parts are ignored on this branch.

**Causal reason (cause → effect), stated plainly:** a partial **write** left an object whose recoverability depends on how many **data shards** survive, so heal measures **missing parts against parity**. A partial **delete** left a **delete marker** that has **no parts at all**, so heal instead measures **how many metadata copies of that marker survive against the data‑block count**, ignoring parts entirely. Different failure, different evidence, different threshold — hence different reconciliation.

---

## 11. Coverage checklist (R1–R7c)

| Req | Question item | Exact literal / value (verbatim) | file:line | Evidence |
| --- | --- | --- | --- | --- |
| R1 | repair‑vs‑leave decision functions | `shouldHealObjectOnDisk` / `isObjectDangling`; triggers `errFileNotFound`, `errFileVersionNotFound`, `errFileCorrupt`, `errLegacyXLMeta`, `errOutdatedXLMeta`, `errPartMissingOrCorrupt` | erasure-healing.go:L156 / L968 (comment L965‑967) | §3, Cases A/B/C |
| R2 | 4‑disk EC:2 scenario + on‑disk layout | `524320 = 524288 + 32`; `DefaultParityBlocks(4)=2`; heal `"dataBlocks":2,"parityBlocks":2` | storage-class.go:L362; erasure-metadata.go:L555 | §4 baseline |
| R3/R4 | reconstruct / stays‑deleted / leave‑alone | `DriveStateOk` after heal / dangling purge / "leave it as is" | erasure-healing.go:L651 / L1030 / L1004; erasure-object.go:L482 | §5 Cases A / B / C |
| R5 | Before/After per‑drive `State` strings (all four) | `ok`, `offline`, `missing`, `corrupt` | erasure-healing.go:L385 / L387 / L389 / L392, L651 | §6 (Cases A + offline demo) |
| R6 | logs/audit explaining "why" | `DeleteDanglingObject`; `d:p="2:2"`; `derrs="map[0:[4 4 4 1]]"`; `merrs=""`; `Error: file is corrupted (cmd.StorageErr)` | erasure-object.go:L451 (audit); observed server log | §7 (Case B audit + Case C log) |
| R7a | valid‑shard boundary | `readQuorum = dataBlocks = 2`; `writeQuorum = dataBlocks + 1 = 3` | erasure-metadata.go:L555‑L559 | §8 boundary test |
| R7b | unrecoverable error literal | `"Read failed. Insufficient number of drives online"`; client `SlowDownRead` / HTTP 503 | erasure-errors.go:L23; api-errors.go:L2190‑L2191, L869‑L872 | §9 (Case B GET) |
| R7c | partial‑write vs partial‑delete branches | `notFoundPartsErrs > parityBlocks` vs `notFoundMetaErrs > dataBlocks` (parts ignored) | erasure-healing.go:L1030 vs L1012‑L1016 | §10 (write observed, delete source‑verified) |

**Named items explicitly covered:** "some disks have data, some corrupted, some nothing" (§4 substrate + Case A/B/C); "always reconstruct?" → no (§5); "stay deleted" (Case B) and "degraded" (Case C); "status indicators showing before/after" (§6, all four `DriveState*`); "logs explain why" (§7 audit `DeleteDanglingObject` + corrupt‑meta log); "how many valid shards need to exist" (§8: ≥ 2); "what error appears when healing cannot recover" (§9: `errErasureReadQuorum` / `SlowDownRead` / 503); "partially failed write versus a partially failed delete" (§10).

### Dependency versions exercised (matching `go.mod` exactly)

- `github.com/klauspost/reedsolomon` **v1.12.4** [go.mod:L40] — Reed‑Solomon reconstruction primitive.
- `github.com/minio/highwayhash` **v1.0.3** [go.mod:L49] — bitrot checksum (the `32`‑byte per‑shard overhead).
- `github.com/minio/madmin-go/v3` **v3.0.77** [go.mod:L52] — `HealResultItem`, `HealDriveInfo`, `HealOpts`, `DriveState*` literals.
- `github.com/minio/minio-go/v7` **v7.0.80** [go.mod:L53] — S3 client SDK (PUT/GET).
- Module `github.com/minio/minio` [go.mod:L1]; `go 1.23` [go.mod:L3]; built/run under Go `go1.24.3` (this image).

### Read‑only & cleanup

The MinIO source tree was **not modified**; the only file added to the repository is this document (`blitzy/documentation/minio_c07e5b49d477.md`). All build/run artifacts — the 4 temporary disk directories, the test bucket, the `mc` alias, the temporary `madmin-go` heal driver, the audit sink, and all observation scripts — were created **outside** the repository (inside the ephemeral container / under `/tmp`) and removed on completion, leaving the repository and the pinned commit `c07e5b49d477b0774f23db3b290745aef8c01bd2` unchanged.

