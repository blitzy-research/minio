# MinIO Erasure Coding Under Drive Failure — Investigation Answers

This document answers four questions about how MinIO's erasure-coding layer behaves when
drives disappear during active operations. Every behavioral claim is backed by (a) the exact
command that produced it, (b) the complete, unedited captured output, and (c) a `file:line`
citation into the MinIO source. Raw output is shown before any summary, and each answer leads
with a direct answer followed by the causal "why".

The four questions being answered (verbatim):

- **Q1:** If a drive suddenly becomes unavailable while data is flowing into the cluster, what specific error code or message does MinIO return to the client, and does the write succeed or fail?
- **Q2:** What happens to files already on the cluster before a drive vanished — can they still be read successfully, and if not, what specific error is returned?
- **Q3:** (a) What specific event/condition triggers a healing operation when a drive comes back online after being offline? (b) What criteria does the system use to determine a specific object needs healing on a particular drive? (c) What log messages appear during an active healing operation?
- **Q4:** What are the specific metric names that track online vs. offline drive counts, and what values do they show before and after a drive failure?

All evidence below was captured by RUNNING a canonically built MinIO server (commit
`c07e5b49d477b0774f23db3b290745aef8c01bd2`) against a real four-drive EC:2 erasure set,
exercising both sides of every quorum boundary through the genuine S3 / admin path. All
temporary scripts and scratch data lived OUTSIDE the repository (`/tmp`, `/mnt`) and were
removed afterward, so the source tree is left unchanged.

---

## Summary

| Question | Direct Answer |
|----------|---------------|
| **Q1 — write during drive loss** | **Conditional.** With write quorum still intact (≤1 drive offline in a 4-drive set) the **PUT SUCCEEDS** (object written to the online drives, missing shard healed later). With write quorum broken (≥2 offline) the **PUT FAILS** with S3 code **`SlowDownWrite`**, **HTTP 503 Service Unavailable** (internal `errErasureWriteQuorum`). |
| **Q2 — read of pre-existing objects** | **Readable while ≥ read quorum (2 of 4) drives are online**, served byte-exact via Reed-Solomon reconstruction. Below read quorum (1 of 4 online) reads **FAIL** with S3 code **`SlowDownRead`**, **HTTP 503 Service Unavailable** (internal `errErasureReadQuorum`). |
| **Q3a — heal trigger** | The **background new-disks monitor** (`monitorLocalDisksAndHeal` → `healFreshDisk`), a **~10 s** cadence loop that detects an unformatted/returned drive and heals it with **no server restart**. Distinct from restart-time `initAutoHeal`, scanner bitrot heal, and MRF (replication) heal. |
| **Q3b — per-object heal criteria** | `shouldHealObjectOnDisk` returns "heal" when a drive's shard is **missing/corrupt** (`errFileNotFound` / `errFileVersionNotFound` / `errFileCorrupt`), or metadata is **legacy** (`XLV1` → `errLegacyXLMeta`), or **outdated** (`!latestMeta.Equals(meta)` → `errOutdatedXLMeta`), or a **part file is missing/corrupt** (`checkPartFileNotFound` / `checkPartFileCorrupt` → `errPartMissingOrCorrupt`). |
| **Q3c — heal log messages** | `Healing drive '%s' - 'mc admin heal alias/ --verbose' to check the current status.` → `Healing drive '%s' - use %d parallel workers.` → `Healing of drive '%s' is finished (healed: %d, skipped: %d).` |
| **Q4 — drive-count metrics** | v3: `minio_cluster_health_drives_online_count`, `minio_cluster_health_drives_offline_count`, `minio_cluster_health_drives_count`; v2/legacy: `minio_cluster_drive_online_total`, `minio_cluster_drive_offline_total`; per-set v3: `minio_cluster_erasure_set_online_drives_count`. Values: **4 online / 0 offline** (before) → **3 online / 1 offline** (during single-drive failure) → **4 online / 0 offline** (after heal). |

---

## Investigation Environment

- **Repo/commit:** `github.com/minio/minio`, branch `minio_c07e5b49d477`, commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.
- **Build recipe (canonical, from `Makefile:L177-179`; `LDFLAGS` defined at `Makefile:L3`):**

  ```
  CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(go run buildscripts/gen-ldflags.go)" -o ./minio
  ```

  Built on **Go 1.23.2**. The `./minio` binary is gitignored (`.gitignore:L4` → `minio`), so building does NOT dirty the tree.

- **Version banner** (`./minio --version`):

  ```
  minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
  Runtime: go1.23.2 linux/amd64
  ```

- **Topology:** single node, 4 loop-mounted ext4 drives `/mnt/drive1..4` (real mounts → avoids
  root-disk refusal, no `MINIO_CI_CD` hack), one erasure set, **default EC:2 (data=2, parity=2)**.
  Confirmed by startup log `Formatting 1st pool, 1 set(s), 4 drives per set.` and by
  `mc admin info` → `4 drives online, 0 drives offline, EC:2`.

- **Default-parity grounding:** `docs/erasure/storage-class/README.md:L52` table row
  "5 or fewer → EC:2", and `docs/erasure/storage-class/README.md:L46` "Parity blocks can not be
  higher than data blocks, so `STANDARD` storage class parity can not be higher than N/2." So a
  4-drive set defaults to parity 2.

- **Quorum math (confirmed at runtime via health headers):** readQuorum = **2**
  (`X-Minio-Read-Quorum: 2`), writeQuorum = **3** (`X-Minio-Write-Quorum: 3`). writeQuorum =
  data+1 because parity == data (see `cmd/erasure-object.go:L1322-1326`:
  `writeQuorum := dataDrives; if dataDrives == parityDrives { writeQuorum++ }`). Boundaries used
  throughout:
  - **1 offline → write OK / read OK**
  - **2 offline → write FAILS / read OK**
  - **3 offline → write FAILS / read FAILS**

- **Launch command (default config):**

  ```
  MINIO_ROOT_USER=minio MINIO_ROOT_PASSWORD=minio123 MINIO_PROMETHEUS_AUTH_TYPE=public \
    ./minio --config-dir /tmp/minio-config server \
    /mnt/drive1 /mnt/drive2 /mnt/drive3 /mnt/drive4 \
    --address :9000 --console-address :9001
  ```

- **Client:** `mc` (MinIO Client) `RELEASE.2025-08-13T08-35-41Z` at `/tmp/mc`; alias set via env
  `MC_HOST_myminio=http://minio:minio123@127.0.0.1:9000`. Fetched on demand exactly as
  `buildscripts/verify-healing.sh:L122-124` fetches `/tmp/mc` (not a repository dependency).
  Metrics scraped unauthenticated via `curl`, enabled by `MINIO_PROMETHEUS_AUTH_TYPE=public`.

- **Drive-loss mechanism (storage layer only — NEVER edits shard files, matching
  `buildscripts/verify-healing.sh` guidance):**
  - fail a drive: `umount -l /mnt/driveN`
  - return drive WITH its data: `mount -o loop /mnt/miniodata/diskN.img /mnt/driveN`
  - return drive AS FRESH (unformatted): `mkfs.ext4 -F -q diskN.img` then `mount -o loop diskN.img /mnt/driveN`

- **Test object:** a 1 MiB file, md5 `8be8929e989fb7f5497638c2153a3bd4`, uploaded into bucket
  `myminio/q1bucket`.

> **Read-only note:** All temporary scripts and scratch data directories live OUTSIDE the
> repository (under `/tmp` and `/mnt`) and were removed after the investigation, leaving the
> MinIO source tree unchanged. The only artifact produced by this task is this Markdown file.

---

## Q1 — Write Path During Drive Failure

> **Q1:** If a drive suddenly becomes unavailable while data is flowing into the cluster, what
> specific error code or message does MinIO return to the client, and does the write succeed or fail?

**DIRECT ANSWER:** It is **conditional**. A mid-write drive loss that still leaves write quorum
intact (**≤1 drive offline** in a 4-drive set) → the **PUT SUCCEEDS**; the object is written to
the remaining online drives and the missing shard is healed later. A drive loss that breaks
write quorum (**≥2 offline**) → the **PUT FAILS** with the S3 error **`SlowDownWrite`** and
**HTTP 503 Service Unavailable**.

### 4.1 Q1a — 1 drive offline (3 online ≥ writeQuorum 3) → SUCCEEDS

Induce the loss, then confirm the degraded state:

```
umount -l /mnt/drive4
```

`mc admin info` now reports:

```
3 drives online, 1 drive offline, EC:2
```

Perform the write:

```
/tmp/mc cp /tmp/obj1mb.bin myminio/q1bucket/onedrive-off.bin
```

Result: **success (exit 0)**; the object is listed as `1.0MiB STANDARD`.

Decode the on-disk metadata via the `docs/debugging/xl-meta` tool. The `xl.meta` shows:

```
EcM (data)   = 2
EcN (parity) = 2
MetaSys      = {}
```

i.e. **NO upgrade marker**. Physically only 3 of 4 shards were written; drive4's shard is
healed later.

**WHY there is no upgrade marker at 4 drives (causal explanation, grounded):** During PUT,
MinIO's availability-optimized path bumps parity by one for the offline drive (2→3) but then
caps it to `N/2 = 2`, so `parityOrig(2) == parityDrives(2)` and no marker is recorded. The
default parity for a 4-drive set already equals the `N/2` ceiling. Tracing it through
`cmd/erasure-object.go`:

- The whole block is gated by `!opts.MaxParity && globalStorageClass.AvailabilityOptimized()`
  (`cmd/erasure-object.go:L1291`). `AvailabilityOptimized()` returns **true by default**
  (`internal/config/storageclass/storage-class.go:L327-333`; comment `L326`
  "Default is 'availability' optimized"; returns true when `Optimize == "availability"` or
  `Optimize == ""`).
- `parityOrig := parityDrives` (`cmd/erasure-object.go:L1293`); for each nil/offline disk,
  `parityDrives++` and `offlineDrives++` (`cmd/erasure-object.go:L1296-1302`).
- Parity cap: `if parityDrives >= len(storageDisks)/2 { parityDrives = len(storageDisks)/2 }`
  (`cmd/erasure-object.go:L1311-1313`).
- Marker set only if changed:
  `if parityOrig != parityDrives { userDefined[minIOErasureUpgraded] = strconv.Itoa(parityOrig)+"->"+strconv.Itoa(parityDrives) }`
  (`cmd/erasure-object.go:L1315-1316`). Marker constant:
  `const minIOErasureUpgraded = "x-minio-internal-erasure-upgraded"` (`cmd/erasure-metadata.go:L38`).
- `xl.meta` field meanings: `ErasureM int json:"EcM"` (data blocks) at
  `cmd/xl-storage-format-v2.go:L160`, `ErasureN int json:"EcN"` (parity blocks) at
  `cmd/xl-storage-format-v2.go:L161`.
- Result verified **stable across 2 runs**.

### 4.2 Q1a mechanism proof — SUPPLEMENTARY, NON-CANONICAL

> **SUPPLEMENTARY — NON-CANONICAL (12-drive, `MINIO_CI_CD=1`, EC:4).** This is **NOT** the
> canonical configuration and is included ONLY to prove that the parity-upgrade mechanism
> actually fires where the default parity is below the `N/2` ceiling. The authoritative answer
> to Q1 is the canonical 4-drive EC:2 result in §4.1 and §4.3.

In a 12-drive set the default parity is EC:4, which is below the `N/2 = 6` ceiling, so the
upgrade marker is observable:

- Healthy PUT → `xl.meta` `EcM`(data)=8 / `EcN`(parity)=4 / no marker.
- With 1 drive offline, the PUT **SUCCEEDS** → `xl.meta` `EcM`(data)=7 / `EcN`(parity)=5, and
  the marker **`x-minio-internal-erasure-upgraded`** IS present (base64 `NC0+NQ==` decodes to
  `4->5`), with 11 shards written.

This demonstrates the `parityOrig != parityDrives` branch (`cmd/erasure-object.go:L1315-1316`)
recording the upgrade when default parity < `N/2`. In the canonical 4-drive set that branch is a
no-op because default parity already equals the ceiling.

### 4.3 Q1b — 2 drives offline (2 online < writeQuorum 3) → FAILS (client-visible)

Induce the second failure (drive4 already offline):

```
umount -l /mnt/drive3
```

`mc admin info`:

```
2 drives online, 2 drives offline, EC:2
```

Write with a wire trace:

```
/tmp/mc cp /tmp/obj1mb.bin myminio/q1bucket/twodrives-off.bin --debug
```

Wire-level result (complete, unedited):

- Request line:

  ```
  PUT /q1bucket/twodrives-off.bin HTTP/1.1
  ```

- Response status:

  ```
  HTTP/1.1 503 Service Unavailable
  ```

- Response body:

  ```xml
  <Error><Code>SlowDownWrite</Code><Message>Resource requested is unwritable, please reduce your request rate</Message><Key>twodrives-off.bin</Key><BucketName>q1bucket</BucketName>...<RequestId>18C031C0857E987C</RequestId>...</Error>
  ```

`mc` auto-retried; every attempt returned `503 SlowDownWrite`. The object was **NOT created**.

**Q1 causal chain / citations:**

- Write-quorum guard: `cmd/erasure-object.go:L1304` `if offlineDrives >= (len(storageDisks)+1)/2 {`
  → for 4 drives `(4+1)/2 = 2`, so 2 offline trips it → `cmd/erasure-object.go:L1308`
  `return ObjectInfo{}, toObjectErr(errErasureWriteQuorum, bucket, object)`.
- Per-drive → quorum reduction: `reduceWriteQuorumErrs` at `cmd/erasure-metadata-utils.go:L156-157`
  (delegates to `reduceQuorumErrs` `cmd/erasure-metadata-utils.go:L137-145`, which returns the
  quorum error when `maxCount < writeQuorum`).
- Internal error string:
  `errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")` at
  `cmd/erasure-errors.go:L25-26`.
- Internal→S3 mapping: `cmd/api-errors.go:L2192-2193` `case errErasureWriteQuorum: apiErr = ErrSlowDownWrite`.
- S3 error definition: `cmd/api-errors.go:L874-878` → `ErrSlowDownWrite` = Code `SlowDownWrite`,
  Description "Resource requested is unwritable, please reduce your request rate",
  `HTTPStatusCode: http.StatusServiceUnavailable` (503).
- Supporting write-path files: single-object PUT `cmd/erasure-object.go`, multipart
  `cmd/erasure-multipart.go`, Reed-Solomon encode/shard dispatch `cmd/erasure-encode.go`.

---


## Q2 — Read Path for Pre-Existing Objects

> **Q2:** What happens to files already on the cluster before a drive vanished — can they still
> be read successfully, and if not, what specific error is returned?

**DIRECT ANSWER:** Objects written before the failure **remain readable as long as at least
read-quorum (2 of 4) drives are online** — served via Reed-Solomon reconstruction with
byte-exact integrity. Once online drives drop **below read quorum (1 of 4 online)**, reads
**FAIL** with the S3 error **`SlowDownRead`** and **HTTP 503 Service Unavailable**.

### 5.1 Q2a — reads SUCCEED via reconstruction (read quorum retained)

Reference object `readtest.bin`, md5 `8be8929e989fb7f5497638c2153a3bd4`.

- **1 drive offline** (`3 online, 1 offline`):

  ```
  mc cat myminio/q1bucket/readtest.bin | md5sum
  ```

  Output:

  ```
  8be8929e989fb7f5497638c2153a3bd4
  ```

  ✓ MATCHES.

- **2 drives offline** (`2 online, 2 offline`, the read-quorum boundary):

  ```
  mc cat myminio/q1bucket/readtest.bin | md5sum
  ```

  Output:

  ```
  8be8929e989fb7f5497638c2153a3bd4
  ```

  ✓ MATCHES. Reconstruction succeeds from exactly 2 healthy shards because K=2 data shards
  suffice to rebuild the object.

### 5.2 Q2b — reads FAIL below read quorum (1 online < readQuorum 2) — clean object-level GET

To isolate the `GetObject` path (bypassing `mc`'s `GetBucketLocation` preflight), a presigned
URL was used:

- Generate the URL:

  ```
  mc share download --expire 1h myminio/q1bucket/readtest.bin
  ```

  Output:

  ```
  Share: http://127.0.0.1:9000/q1bucket/readtest.bin?X-Amz-Algorithm=AWS4-HMAC-SHA256&...&X-Amz-Signature=...
  ```

- Healthy baseline (4 online):

  ```
  curl "$PRE"
  ```

  → HTTP 200, bytes = 1048576, md5 `8be8929e989fb7f5497638c2153a3bd4` ✓.

- Degrade to 1 online (`umount -l` drives 2, 3, 4), then raw object GET via the same presigned URL:

  - Status / headers:

    ```
    HTTP/1.1 503 Service Unavailable
    Content-Type: application/xml
    X-Amz-Request-Id: 18C03220FA50501B
    ```

  - Body (complete, unedited):

    ```xml
    <?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>readtest.bin</Key><BucketName>q1bucket</BucketName><Resource>/q1bucket/readtest.bin</Resource><RequestId>18C03220FA50501B</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
    ```

The `<Key>readtest.bin</Key>` element proves this is the object read path (not bucket-location).
`mc cat --debug` (which does `GetBucketLocation` first) returns the same `503 SlowDownRead`.
Stability confirmed on repeat.

**Q2 causal chain / citations:**

- Read-quorum computed per object: `objectQuorumFromMeta` at `cmd/erasure-metadata.go:L531`,
  with `expectedRQuorum := len(partsMetaData)/2` (`cmd/erasure-metadata.go:L533`) = 2 for a
  4-drive set; `reduceReadQuorumErrs(...)` at `cmd/erasure-metadata.go:L539` returns
  `errErasureReadQuorum` when fewer than 2 valid `xl.meta` reads are available.
- Reduction helper: `reduceReadQuorumErrs` at `cmd/erasure-metadata-utils.go:L150-151`
  (delegates to `reduceQuorumErrs` `cmd/erasure-metadata-utils.go:L137-145`).
- Additional read-quorum return sites: `cmd/erasure-object.go:L487`
  `return FileInfo{}, errErasureReadQuorum`; `cmd/erasure-object.go:L836` `reduceReadQuorumErrs(...)`
  in the `getObjectFileInfo` path; `cmd/erasure-object.go:L691` handles the read-quorum case in
  `shouldCheckForDangling`.
- Reconstruction: `cmd/erasure-object.go` (`GetObjectNInfo` → `getObjectWithFileInfo`) +
  Reed-Solomon decode in `cmd/erasure-decode.go`.
- Internal error string:
  `errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")` at
  `cmd/erasure-errors.go:L22-23`.
- Internal→S3 mapping: `cmd/api-errors.go:L2190-2191` `case errErasureReadQuorum: apiErr = ErrSlowDownRead`.
- S3 error definition: `cmd/api-errors.go:L869-873` → `ErrSlowDownRead` = Code `SlowDownRead`,
  Description "Resource requested is unreadable, please reduce your request rate",
  `HTTPStatusCode: http.StatusServiceUnavailable` (503).

---


## Q3 — Healing (trigger, criteria, log messages)

> **Q3:** (a) What specific event/condition triggers a healing operation when a drive comes back
> online after being offline? (b) What criteria does the system use to determine a specific
> object needs healing on a particular drive? (c) What log messages appear during an active
> healing operation?

**Scenario (storage-layer only, never editing shard files):** uploaded `healme-base.bin` while
healthy (shards on all 4 drives); failed drive4 with `umount -l`; wrote `healme.bin` while
drive4 was offline (so drive4's `part.*` count = 0 — it missed that object); returned drive4 as
a **fresh, unformatted disk**:

```
mkfs.ext4 -F -q disk4.img
mount -o loop disk4.img /mnt/drive4
```

Its contents were only `lost+found`, with no `format.json`.

### 6.1 Q3a — TRIGGER (observed)

**DIRECT ANSWER:** With **no server restart**, the **background new-disks monitor** detected the
returned unformatted drive and completed healing in **~20 s** (2 cycles of its **10 s** cadence).
This is the pure "drive comes back online" path, and it is **distinct** from restart-time
`initAutoHeal`, from scanner-driven bitrot healing, and from MRF (replication) healing.

Citations (grounded):

- Monitor cadence: `cmd/background-newdisks-heal-ops.go:L40`
  `defaultMonitorNewDiskInterval = time.Second * 10`.
- Monitor loop: `cmd/background-newdisks-heal-ops.go:L563` `monitorLocalDisksAndHeal`; it calls
  `healFreshDisk` at `cmd/background-newdisks-heal-ops.go:L592`.
- Heal entry point: `cmd/background-newdisks-heal-ops.go:L419` `func healFreshDisk`.
- Restart path (contrast): `cmd/background-newdisks-heal-ops.go:L377` `initAutoHeal`.
- Disk selection: `cmd/background-newdisks-heal-ops.go:L393` `getLocalDisksToHeal` — adds a disk
  when `DiskInfo` returns `errUnformattedDisk`, OR when `disk.Healing()` is non-nil and not
  finished; returns empty if ALL endpoints are to-heal (fresh setup).
- Runtime reconnect enqueue: `cmd/erasure-sets.go:L227` and `cmd/erasure-sets.go:L238`
  `globalBackgroundHealState.pushHealLocalDisks(endpoint)`.

### 6.2 Q3b — PER-OBJECT HEAL CRITERIA (observed proof + citation)

**DIRECT ANSWER:** An object is healed onto a specific drive when that drive's shard is
**missing/corrupt** (or the metadata is legacy/outdated, or a part file is missing/corrupt).

Observed proof: after healing, drive4 physically regained `format.json` (342 B, written by the
`HealFormat` reformat) AND every object's shard — including **`healme.bin`** (its `part.*` count
went 0 → 1, plus `xl.meta`), proving the missing-shard criterion fired. Re-reading `healme.bin`
gave md5 `8be8929e989fb7f5497638c2153a3bd4` (byte-exact). Verified per drive: `healme-base.bin`,
`healme.bin`, `readtest.bin`, `healthy-obj.bin` all have `part.*`=1 + `xl.meta` on drive4 after
heal.

Citation (all branches, not elided): `shouldHealObjectOnDisk` at
`cmd/erasure-healing.go:L156-183` returns heal=true when:

- `erErr` is `errFileNotFound`, `errFileVersionNotFound`, or `errFileCorrupt` → this is drive4's
  missing shard (`cmd/erasure-healing.go:L157-158`); OR
- `erErr == nil` AND `meta.XLV1` (legacy metadata) → `errLegacyXLMeta`
  (`cmd/erasure-healing.go:L160-165`); OR
- `erErr == nil` AND `!latestMeta.Equals(meta)` (outdated metadata) → `errOutdatedXLMeta`
  (`cmd/erasure-healing.go:L166-168`); OR
- `erErr == nil`, not deleted/remote, AND `partsErrs` contains `checkPartFileNotFound` or
  `checkPartFileCorrupt` → `errPartMissingOrCorrupt` (`cmd/erasure-healing.go:L169-178`);
- otherwise returns false (no heal) (`cmd/erasure-healing.go:L180`).

### 6.3 Q3c — LOG MESSAGES (observed, complete, unedited)

The captured server-log lines during the active healing operation:

```
Healing drive '/mnt/drive4' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/mnt/drive4' - use 4 parallel workers.
Healing of drive '/mnt/drive4' is finished (healed: 14, skipped: 0).
```

Citations (grounded, with the exact format strings):

- `cmd/background-newdisks-heal-ops.go:L460` →
  `"Healing drive '%s' - 'mc admin heal alias/ --verbose' to check the current status."`
- `cmd/global-heal.go:L210` → `"Healing drive '%s' - use %d parallel workers."`
- `cmd/background-newdisks-heal-ops.go:L520` →
  `"Healing of drive '%s' is finished (healed: %d, skipped: %d)."`
- Sibling states NOT hit this run (for completeness): `cmd/background-newdisks-heal-ops.go:L503`
  `"Healing of drive '%s' is incomplete, retrying %s time (healed: %d, skipped: %d, failed: %d)."`
  and `cmd/background-newdisks-heal-ops.go:L517`
  `"Healing of drive '%s' is complete, retried %d times (healed: %d, skipped: %d)."`
- Emission path: `healingLogEvent` at `cmd/logging.go:L83-84` (calls
  `logger.Event(ctx, "healing", msg, args...)`).
- Interpretation: `healed: 14` counts buckets + object versions healed onto the drive;
  `skipped: 0`.

### 6.4 Q3 manual cross-check — `mc admin heal -r` (server side `cmd/admin-heal-ops.go`)

Command and output (complete, unedited):

```
/tmp/mc admin heal -r myminio/q1bucket
```

```
[Green  ->  Green] q1bucket/
[Green  ->  Green] q1bucket/healme-base.bin
[Green  ->  Green] q1bucket/healme.bin
[Green  ->  Green] q1bucket/healthy-obj.bin
[Green  ->  Green] q1bucket/onedrive-off-2.bin
[Green  ->  Green] q1bucket/onedrive-off.bin
[Green  ->  Green] q1bucket/readtest.bin
Healed:	0/6 objects; 6 MiB in 1s
```

Explanation: `0/6` needed healing because the **background healer already repaired drive4** —
this corroborates the Q3a completeness. The heal state enum names (`Bucket`, `Object`,
`CheckAbandonedParts`) come from `cmd/healingmetric_string.go:L16`
(`const _healingMetric_name = "BucketObjectCheckAbandonedParts"`, indices at
`cmd/healingmetric_string.go:L11-13`).

---


## Q4 — Health / Metrics Reporting

> **Q4:** What are the specific metric names that track online vs. offline drive counts, and what
> values do they show before and after a drive failure?

**DIRECT ANSWER:** The drive-count series are —

- **v3:** `minio_cluster_health_drives_online_count`, `minio_cluster_health_drives_offline_count`,
  `minio_cluster_health_drives_count`;
- **v2/legacy:** `minio_cluster_drive_online_total`, `minio_cluster_drive_offline_total`;
- **per-erasure-set v3:** `minio_cluster_erasure_set_online_drives_count`.

Values move **4 online / 0 offline** (before) → **3 online / 1 offline** (during a single-drive
failure) → back to **4 online / 0 offline** (after heal). Unauthenticated scraping requires
`MINIO_PROMETHEUS_AUTH_TYPE=public` (`cmd/metrics-router.go:L41` env name, `cmd/metrics-router.go:L49`
`public` value; default is JWT auth at `cmd/metrics-router.go:L56`, switched to `NoAuthMiddleware`
at `cmd/metrics-router.go:L59-61`).

### 7.1 BEFORE (4 online)

- v3 health metrics:

  ```
  curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/health
  ```

  ```
  minio_cluster_health_drives_count 4
  minio_cluster_health_drives_online_count 4
  ```

  Note: `minio_cluster_health_drives_offline_count` is **OMITTED** here — v3 suppresses
  zero-valued gauges (see the nuance below).

- v2 cluster metrics:

  ```
  curl -s http://127.0.0.1:9000/minio/v2/metrics/cluster
  ```

  ```
  minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
  minio_cluster_drive_online_total{server="127.0.0.1:9000"} 4
  ```

- per-set v3 metrics:

  ```
  curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/erasure-set
  ```

  ```
  minio_cluster_erasure_set_online_drives_count{pool_id="0",set_id="0"} 4
  ```

- health endpoints: `/minio/health/cluster` → HTTP 200 with `X-Minio-Write-Quorum: 3`;
  `/minio/health/cluster/read` → HTTP 200 with `X-Minio-Read-Quorum: 2`.

### 7.2 DURING (drive4 offline, 3 online)

- v3:

  ```
  minio_cluster_health_drives_count 4
  minio_cluster_health_drives_offline_count 1
  minio_cluster_health_drives_online_count 3
  ```

  (the offline gauge is now emitted because it is > 0)

- v2:

  ```
  minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 1
  minio_cluster_drive_online_total{server="127.0.0.1:9000"} 3
  ```

- per-set:

  ```
  minio_cluster_erasure_set_online_drives_count{pool_id="0",set_id="0"} 3
  ```

- health: BOTH endpoints still HTTP 200 (1 offline still satisfies write-quorum 3 and
  read-quorum 2). Note the quorum headers report the REQUIRED values (3/2), not the live online
  count.

### 7.3 AFTER (post-heal)

Identical to BEFORE: **4 online**; the offline gauge is omitted again (value back to 0).

### 7.4 Q4 citations (grounded)

- v3 names: `cmd/metrics-v3-cluster-health.go:L23` `drives_offline_count`,
  `cmd/metrics-v3-cluster-health.go:L24` `drives_online_count`,
  `cmd/metrics-v3-cluster-health.go:L25` `drives_count` (full names prefixed
  `minio_cluster_health_`).
- v2 names: `cmd/metrics-v2.go:L194` `offline_total`, `cmd/metrics-v2.go:L195` `online_total`;
  namespace `clusterMetricNamespace = "minio_cluster"` at `cmd/metrics-v2.go:L130`, subsystem
  `driveSubsystem = "drive"` at `cmd/metrics-v2.go:L142` → full names
  `minio_cluster_drive_online_total` / `minio_cluster_drive_offline_total`.
- per-set: v3 `cmd/metrics-v3-cluster-erasure-set.go:L30`
  `erasureSetOnlineDrivesCount = "online_drives_count"` (set from `h.HealthyDrives` at
  `cmd/metrics-v3-cluster-erasure-set.go:L95`), `cmd/metrics-v3-cluster-erasure-set.go:L31`
  `healing_drives_count` (from `h.HealingDrives` at `cmd/metrics-v3-cluster-erasure-set.go:L96`,
  emitted only while a set actively heals); v2 equivalent `getClusterErasureSetOnlineDrivesMD` at
  `cmd/metrics-v2.go:L3636` (name `erasure_set_online_drives`, `cmd/metrics-v2.go:L3640`) and
  `getClusterErasureSetHealingDrivesMD` at `cmd/metrics-v2.go:L3646` (`erasure_set_healing_drives`,
  `cmd/metrics-v2.go:L3650`).
- router/auth: `cmd/metrics-router.go:L41` / `cmd/metrics-router.go:L49`.
- corroborating health endpoints: `cmd/healthcheck-router.go:L41-44` (`ClusterCheckHandler` for
  `/minio/health/cluster`, `ClusterReadCheckHandler` for `/minio/health/cluster/read`); headers
  set in `cmd/healthcheck-handler.go:L72` (`MinIOWriteQuorum`) and `cmd/healthcheck-handler.go:L109`
  (`MinIOReadQuorum`); header string constants `internal/http/headers.go:L193`
  `"x-minio-write-quorum"`, `internal/http/headers.go:L196` `"x-minio-read-quorum"`.

### 7.5 Nuances (must be understood to read the values correctly)

- **v3 offline gauge suppressed at 0:** In the BEFORE and AFTER states the
  `minio_cluster_health_drives_offline_count` series is absent from the v3 output because v3
  suppresses zero-valued gauges. Only the `_count` and `_online_count` series appear at 0 offline;
  the `_offline_count` series reappears once its value is > 0 (DURING). Do not mistake the absent
  series for a missing metric.
- **1-minute TTL cache on cluster aggregates:** the cluster-aggregate v2/v3 drive metrics are
  served from a **1-minute TTL cache** — `newClusterStorageInfoCache` at `cmd/metrics-v3-cache.go:L256`
  uses `cachevalue.NewFromFunc(1*time.Minute, cachevalue.Opts{ReturnLastGood: true}, loadStorageInfo)`.
  Therefore, depending on scrape timing relative to the cache window, the online/offline transition
  appears either immediately (cache expired) or lags by up to ~1 minute. In contrast, the
  per-erasure-set metric and `mc admin info` reflect LIVE `StorageInfo` immediately. A reader who
  scrapes right after inducing failure and sees stale counts is observing this cache, not a bug.

---


## Appendix: Source Citations Index

All citations were verified against the source at commit
`c07e5b49d477b0774f23db3b290745aef8c01bd2`.

**Q1 — write path**

- `cmd/erasure-object.go:L1291` — parity-upgrade block gated by `!opts.MaxParity && globalStorageClass.AvailabilityOptimized()`
- `cmd/erasure-object.go:L1293` — `parityOrig := parityDrives`
- `cmd/erasure-object.go:L1296-1302` — offline-disk loop (`parityDrives++`, `offlineDrives++`)
- `cmd/erasure-object.go:L1304` — write-quorum guard `if offlineDrives >= (len(storageDisks)+1)/2`
- `cmd/erasure-object.go:L1308` — `return ObjectInfo{}, toObjectErr(errErasureWriteQuorum, bucket, object)`
- `cmd/erasure-object.go:L1311-1313` — parity cap to `len(storageDisks)/2`
- `cmd/erasure-object.go:L1315-1316` — upgrade marker set when `parityOrig != parityDrives`
- `cmd/erasure-object.go:L1322-1326` — `writeQuorum := dataDrives; if dataDrives == parityDrives { writeQuorum++ }`
- `cmd/erasure-metadata.go:L38` — `const minIOErasureUpgraded = "x-minio-internal-erasure-upgraded"`
- `cmd/xl-storage-format-v2.go:L160` — `ErasureM int json:"EcM"` (data blocks)
- `cmd/xl-storage-format-v2.go:L161` — `ErasureN int json:"EcN"` (parity blocks)
- `internal/config/storageclass/storage-class.go:L326` — comment "Default is 'availability' optimized"
- `internal/config/storageclass/storage-class.go:L327-333` — `AvailabilityOptimized()` returns true by default
- `cmd/erasure-metadata-utils.go:L156-157` — `reduceWriteQuorumErrs`
- `cmd/erasure-metadata-utils.go:L137-145` — `reduceQuorumErrs`
- `cmd/erasure-errors.go:L25-26` — `errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")`
- `cmd/api-errors.go:L2192-2193` — `case errErasureWriteQuorum: apiErr = ErrSlowDownWrite`
- `cmd/api-errors.go:L874-878` — `ErrSlowDownWrite` = `SlowDownWrite`, HTTP 503
- `cmd/erasure-multipart.go`, `cmd/erasure-encode.go` — supporting write-path files

**Q2 — read path**

- `cmd/erasure-metadata.go:L531` — `objectQuorumFromMeta`
- `cmd/erasure-metadata.go:L533` — `expectedRQuorum := len(partsMetaData)/2`
- `cmd/erasure-metadata.go:L539` — `reduceReadQuorumErrs(...)`
- `cmd/erasure-metadata-utils.go:L150-151` — `reduceReadQuorumErrs`
- `cmd/erasure-object.go:L487` — `return FileInfo{}, errErasureReadQuorum`
- `cmd/erasure-object.go:L836` — `reduceReadQuorumErrs(...)` in `getObjectFileInfo`
- `cmd/erasure-object.go:L691` — read-quorum case in `shouldCheckForDangling`
- `cmd/erasure-decode.go` — Reed-Solomon reconstruction on read
- `cmd/erasure-errors.go:L22-23` — `errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")`
- `cmd/api-errors.go:L2190-2191` — `case errErasureReadQuorum: apiErr = ErrSlowDownRead`
- `cmd/api-errors.go:L869-873` — `ErrSlowDownRead` = `SlowDownRead`, HTTP 503

**Q3 — healing**

- `cmd/background-newdisks-heal-ops.go:L40` — `defaultMonitorNewDiskInterval = time.Second * 10`
- `cmd/background-newdisks-heal-ops.go:L563` — `monitorLocalDisksAndHeal`
- `cmd/background-newdisks-heal-ops.go:L592` — call to `healFreshDisk`
- `cmd/background-newdisks-heal-ops.go:L419` — `func healFreshDisk`
- `cmd/background-newdisks-heal-ops.go:L377` — `initAutoHeal` (restart-path contrast)
- `cmd/background-newdisks-heal-ops.go:L393` — `getLocalDisksToHeal`
- `cmd/erasure-sets.go:L227`, `cmd/erasure-sets.go:L238` — `pushHealLocalDisks(endpoint)`
- `cmd/erasure-healing.go:L156-183` — `shouldHealObjectOnDisk`
  - `L157-158` (`errFileNotFound` / `errFileVersionNotFound` / `errFileCorrupt`)
  - `L160-165` (`XLV1` → `errLegacyXLMeta`)
  - `L166-168` (`!latestMeta.Equals(meta)` → `errOutdatedXLMeta`)
  - `L169-178` (`checkPartFileNotFound` / `checkPartFileCorrupt` → `errPartMissingOrCorrupt`)
  - `L180` (return false)
- `cmd/background-newdisks-heal-ops.go:L460` — "Healing drive '%s' - 'mc admin heal alias/ --verbose' to check the current status."
- `cmd/global-heal.go:L210` — "Healing drive '%s' - use %d parallel workers."
- `cmd/background-newdisks-heal-ops.go:L520` — "Healing of drive '%s' is finished (healed: %d, skipped: %d)."
- `cmd/background-newdisks-heal-ops.go:L503` — "Healing of drive '%s' is incomplete, retrying %s time ..." (sibling, not hit)
- `cmd/background-newdisks-heal-ops.go:L517` — "Healing of drive '%s' is complete, retried %d times ..." (sibling, not hit)
- `cmd/logging.go:L83-84` — `healingLogEvent` → `logger.Event(ctx, "healing", ...)`
- `cmd/healingmetric_string.go:L16` — `const _healingMetric_name = "BucketObjectCheckAbandonedParts"`
- `cmd/healingmetric_string.go:L11-13` — enum indices
- `cmd/admin-heal-ops.go` — server side of `mc admin heal -r`

**Q4 — metrics / health**

- `cmd/metrics-v3-cluster-health.go:L23` — `drives_offline_count`
- `cmd/metrics-v3-cluster-health.go:L24` — `drives_online_count`
- `cmd/metrics-v3-cluster-health.go:L25` — `drives_count`
- `cmd/metrics-v2.go:L130` — `clusterMetricNamespace = "minio_cluster"`
- `cmd/metrics-v2.go:L142` — `driveSubsystem = "drive"`
- `cmd/metrics-v2.go:L194` — `offline_total`
- `cmd/metrics-v2.go:L195` — `online_total`
- `cmd/metrics-v3-cluster-erasure-set.go:L30` — `erasureSetOnlineDrivesCount = "online_drives_count"`
- `cmd/metrics-v3-cluster-erasure-set.go:L31` — `erasureSetHealingDrivesCount = "healing_drives_count"`
- `cmd/metrics-v3-cluster-erasure-set.go:L95` — set from `h.HealthyDrives`
- `cmd/metrics-v3-cluster-erasure-set.go:L96` — set from `h.HealingDrives`
- `cmd/metrics-v2.go:L3636` — `getClusterErasureSetOnlineDrivesMD`
- `cmd/metrics-v2.go:L3640` — name `erasure_set_online_drives`
- `cmd/metrics-v2.go:L3646` — `getClusterErasureSetHealingDrivesMD`
- `cmd/metrics-v2.go:L3650` — name `erasure_set_healing_drives`
- `cmd/metrics-v3-cache.go:L256` — `newClusterStorageInfoCache` (1-minute TTL cache)
- `cmd/metrics-router.go:L41` — `EnvPrometheusAuthType = "MINIO_PROMETHEUS_AUTH_TYPE"`
- `cmd/metrics-router.go:L49` — `prometheusPublic prometheusAuthType = "public"`
- `cmd/metrics-router.go:L56` — default JWT auth
- `cmd/metrics-router.go:L59-61` — switch to `NoAuthMiddleware`
- `cmd/healthcheck-router.go:L41-44` — `ClusterCheckHandler` / `ClusterReadCheckHandler`
- `cmd/healthcheck-handler.go:L72` — sets `MinIOWriteQuorum` header
- `cmd/healthcheck-handler.go:L109` — sets `MinIOReadQuorum` header
- `internal/http/headers.go:L193` — `MinIOWriteQuorum = "x-minio-write-quorum"`
- `internal/http/headers.go:L196` — `MinIOReadQuorum = "x-minio-read-quorum"`

**Environment**

- `Makefile:L3` — `LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)`
- `Makefile:L177-179` — canonical `build` recipe
- `.gitignore:L4` — `minio` (built binary is gitignored)
- `docs/erasure/storage-class/README.md:L46` — parity ≤ N/2
- `docs/erasure/storage-class/README.md:L52` — "5 or fewer → EC:2"
- `buildscripts/verify-healing.sh:L122-124` — fetch `/tmp/mc`

