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

- **Source commit under investigation vs. destination-branch HEAD (build provenance).** The version
  banner above is the *authentic* output of a canonical build **at the source commit under
  investigation**, `c07e5b49d477b0774f23db3b290745aef8c01bd2`. Its Git committer date
  `2024-11-25T09:10:22 -0800` = `2024-11-25T17:10:22Z` (UTC) is exactly the date `gen-ldflags`
  stamps as the version, so `DEVELOPMENT.2024-11-25T17-10-22Z` is reproducible from that commit.
  That commit is the code whose behavior every answer below documents, which is why this banner is
  retained verbatim. This document is *delivered* on a destination branch whose HEAD is that same
  source commit **plus additive, documentation-only commits** (this file). Two consequences follow,
  and neither changes the code under test:
  - **`buildscripts/gen-ldflags.go` derives the version string from the current Git HEAD**, not from
    a fixed commit: the commit-id comes from `git log --format=%H -n1`
    (`buildscripts/gen-ldflags.go:L74-88`, consumed at `L39-40`) and the version date from
    `git log --format=%cI -n1`, converted to UTC (`buildscripts/gen-ldflags.go:L90-110` — note the
    `t.UTC()` at `L109` — consumed as the default version at `L112-118`). A canonical rebuild from
    the destination checkout therefore stamps the **destination HEAD** into the banner, not
    `c07e5b49d477`. For example, at destination HEAD `947d4c17210a68e3b09c9c15572605f464c884ad`
    (committer date `2026-07-08T06:58:50Z`) the same build recipe prints:

    ```
    minio version DEVELOPMENT.2026-07-08T06-58-50Z (commit-id=947d4c17210a68e3b09c9c15572605f464c884ad)
    Runtime: go1.23.2 linux/amd64
    ```

    This hash is *illustrative, not fixed*: it advances with every subsequent documentation commit
    (including the one that finalizes this file), so the exact value a reader's own rebuild prints
    will generally be a still-later documentation descendant of `c07e5b49d477`. Consequently
    `git rev-parse HEAD` is *expected* NOT to equal `c07e5b49d477`.
  - **The MinIO source tree is byte-identical between `c07e5b49d477` and the destination HEAD** — the
    only difference is the addition of this one document:

    ```
    $ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2..HEAD
    A	blitzy/documentation/minio_c07e5b49d477.md
    ```

    Zero `.go`, `go.mod`, or `go.sum` files differ, so the compiled server behavior is *provably
    identical* and **every runtime observation in this document remains attributable to the code at
    `c07e5b49d477`**. The invariant that endures is this byte-identical source tree, not the HEAD
    hash a given build happens to stamp.

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

- **Safety note on `MINIO_PROMETHEUS_AUTH_TYPE=public` (investigation/test-only):** this setting is
  used here **solely** to make this read-only investigation's metric scrapes reproducible with plain
  `curl` (no bearer token to mint), and it exposes the Prometheus endpoints **without authentication**.
  It is **not a blanket production recommendation**. MinIO's canonical default is JWT-authenticated
  scraping (`cmd/metrics-router.go:L56`); production deployments should retain that default (or restrict
  the endpoints by network policy) rather than adopt `public`. The auth type affects only *who may
  scrape* the endpoint — it never changes the metric values reported below.

- **Drive-loss mechanism (storage layer only — NEVER edits shard files, matching
  `buildscripts/verify-healing.sh` guidance):**
  - fail a drive: `umount -l /mnt/driveN`
  - return drive WITH its data: `mount -o loop /mnt/miniodata/diskN.img /mnt/driveN`
  - return drive AS FRESH (unformatted): `mkfs.ext4 -F -q diskN.img` then `mount -o loop diskN.img /mnt/driveN`

- **Test object:** a 1 MiB file (`/tmp/blitzy-repro/obj1mb.bin`), md5
  `d1897826d5343e70d38ff11e37708e39` (`md5sum /tmp/blitzy-repro/obj1mb.bin`), uploaded into bucket
  `myminio/q1bucket`.

- **Deployment ID (this run):** `829673bc-44fc-42c4-afb8-d5c5867216bb`; server `HostId`
  (`X-Amz-Id-2`, deterministic hex of the local node name via `globalLocalNodeNameHex`,
  `cmd/generic-handlers.go:L550`) = `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`.

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

Induce the loss, then confirm the degraded state (complete `mc admin info` output):

```
$ umount -l /mnt/drive4
$ /tmp/mc admin info myminio
●  127.0.0.1:9000
   Uptime: 2 minutes 
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK 
   Drives: 3/4 OK 
   Pool: 1

┌──────┬───────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage          │ Erasure stripe size │ Erasure sets │
│ 1st  │ 0.2% (total: 1.8 GiB) │ 4                   │ 1            │
└──────┴───────────────────────┴─────────────────────┴──────────────┘

3 drives online, 1 drive offline, EC:2
```

Perform the write (complete `mc cp` output; shell exit status printed after):

```
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin myminio/q1bucket/onedrive-off.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `myminio/q1bucket/onedrive-off.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 1.00 MiB │ 1.00 MiB    │ 00m00s   │ 38.53 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
$ echo "exit=$?"
exit=0
```

A second write during the same 1-drive outage behaves identically (complete `mc cp` output):

```
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin myminio/q1bucket/onedrive-off-2.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `myminio/q1bucket/onedrive-off-2.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 1.00 MiB │ 1.00 MiB    │ 00m00s   │ 32.36 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
```

Both objects list as `1.0MiB STANDARD` (complete `mc ls` output):

```
$ /tmp/mc ls myminio/q1bucket/
[2026-07-08 05:41:06 UTC] 1.0MiB STANDARD onedrive-off.bin
[2026-07-08 05:41:06 UTC] 1.0MiB STANDARD onedrive-off-2.bin
```

Decode the on-disk metadata with the `docs/debugging/xl-meta` tool (`xl.meta` is written
identically on every online drive; drive1 shown). Complete, unedited tool output:

```
$ go run ./docs/debugging/xl-meta /mnt/drive1/q1bucket/onedrive-off.bin/xl.meta
{
    "Versions": [
        {
            "Header": {
                "EcM": 2,
                "EcN": 2,
                "Flags": 2,
                "ModTime": "2026-07-08T05:41:06.020362005Z",
                "Signature": "4ef99539",
                "Type": 1,
                "VersionID": "00000000000000000000000000000000"
            },
            "Idx": 0,
            "Metadata": {
                "Type": 1,
                "V2Obj": {
                    "CSumAlgo": 1,
                    "DDir": "0H9rXh+URhyRFNN4paQcIQ==",
                    "EcAlgo": 1,
                    "EcBSize": 1048576,
                    "EcDist": [
                        2,
                        3,
                        4,
                        1
                    ],
                    "EcIndex": 2,
                    "EcM": 2,
                    "EcN": 2,
                    "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
                    "MTime": 1783489266020362005,
                    "MetaSys": {},
                    "MetaUsr": {
                        "content-type": "application/octet-stream",
                        "etag": "d1897826d5343e70d38ff11e37708e39"
                    },
                    "PartASizes": [
                        1048576
                    ],
                    "PartETags": null,
                    "PartNums": [
                        1
                    ],
                    "PartSizes": [
                        1048576
                    ],
                    "Size": 1048576
                },
                "v": 1732554622
            }
        }
    ]
}
```

Reading the raw output: `"EcM": 2` (data blocks), `"EcN": 2` (parity blocks), and
`"MetaSys": {}` is **empty** → **NO upgrade marker**. The stored `"etag"` equals the source md5
`d1897826d5343e70d38ff11e37708e39`, confirming the correct object was written. On-disk inspection
confirms drives 1/2/3 each hold a data-dir UUID + `xl.meta` while **drive4 has no object
directory at all** (it missed the write); drive4's shard is reconstructed later by healing
(see Q3). Physically only 3 of 4 shards were written.

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
upgrade marker is observable. **Launch (non-canonical — 12 plain directories, `MINIO_CI_CD=1`,
port 9010):**

```
$ MINIO_ROOT_USER=minio MINIO_ROOT_PASSWORD=minio123 MINIO_PROMETHEUS_AUTH_TYPE=public \
    MINIO_CI_CD=1 ./minio --config-dir /tmp/minio-config-ec server \
    /tmp/blitzy-repro/ec-data/disk{1..12} --address :9010 --console-address :9011
```

The full MinIO startup banner for this non-canonical supplementary cluster is standard server
boilerplate (a `Version:` line, the `API:`/`WebUI:` endpoint list, and the docs link) and is not
reproduced here, as it bears on none of the parity-upgrade behavior under study. The 12-drive
**EC:4** topology this command forms is confirmed independently, without relying on the banner, by
the `xl.meta` header of the healthy PUT below (`"EcM": 8` / `"EcN": 4`) and by `mc admin info` in
the degraded step below (`11 drives online, 1 drive offline, EC:4`).

Alias for this cluster: `MC_HOST_ec=http://minio:minio123@127.0.0.1:9010`.

**Drive-loss mechanism for this single-node local backend (exact commands + rationale).** A plain
`rm -rf`/`mv` of a drive directory does **not** simulate a lost drive here: MinIO's local backend
auto-recreates a missing local path on the next access, so the "offline" drive silently comes
back and all 12 shards get written. To make the drive genuinely un-writable, the real data is
moved aside and the drive path is replaced by a **broken symlink** (a dangling target MinIO
cannot recreate):

```
$ mv  /tmp/blitzy-repro/ec-data/disk12 /tmp/blitzy-repro/ec-data/disk12.off
$ ln -s /tmp/blitzy-repro/ec-data/__does_not_exist__ /tmp/blitzy-repro/ec-data/disk12
$ ls -la /tmp/blitzy-repro/ec-data/disk12
lrwxrwxrwx 1 root root 44 Jul  8 05:50 /tmp/blitzy-repro/ec-data/disk12 -> /tmp/blitzy-repro/ec-data/__does_not_exist__
```

**Healthy PUT (all 12 online).** Bucket create, upload, and complete `xl-meta` decode:

```
$ /tmp/mc mb ec/ecbucket
Bucket created successfully `ec/ecbucket`.
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin ec/ecbucket/healthy.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `ec/ecbucket/healthy.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 1.00 MiB │ 1.00 MiB    │ 00m00s   │ 26.83 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
$ go run ./docs/debugging/xl-meta /tmp/blitzy-repro/ec-data/disk1/ecbucket/healthy.bin/xl.meta
{
    "Versions": [
        {
            "Header": {
                "EcM": 8,
                "EcN": 4,
                "Flags": 6,
                "ModTime": "2026-07-08T05:50:37.740934948Z",
                "Signature": "d1fd7f90",
                "Type": 1,
                "VersionID": "00000000000000000000000000000000"
            },
            "Idx": 0,
            "Metadata": {
                "Type": 1,
                "V2Obj": {
                    "CSumAlgo": 1,
                    "DDir": "ekU7A66dQkSjWw9F81oHOw==",
                    "EcAlgo": 1,
                    "EcBSize": 1048576,
                    "EcDist": [ 10, 11, 12, 1, 2, 3, 4, 5, 6, 7, 8, 9 ],
                    "EcIndex": 10,
                    "EcM": 8,
                    "EcN": 4,
                    "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
                    "MTime": 1783489837740934948,
                    "MetaSys": {
                        "x-minio-internal-inline-data": "dHJ1ZQ=="
                    },
                    "MetaUsr": {
                        "content-type": "application/octet-stream",
                        "etag": "d1897826d5343e70d38ff11e37708e39"
                    },
                    "PartASizes": [ 1048576 ],
                    "PartETags": null,
                    "PartNums": [ 1 ],
                    "PartSizes": [ 1048576 ],
                    "Size": 1048576
                },
                "v": 1732554622
            }
        }
    ]
}
```

Reading it: `"EcM": 8` / `"EcN": 4` (default EC:4). `MetaSys` contains **only**
`"x-minio-internal-inline-data": "dHJ1ZQ=="` (base64 decodes to `true` — the small object is
inlined; `echo -n dHJ1ZQ== | base64 -d` → `true`); there is **NO**
`x-minio-internal-erasure-upgraded` key → no parity upgrade, as expected when all drives are
online.

**Degraded PUT (11 online, disk12 replaced by the broken symlink above).** `mc admin info` first,
then upload and complete `xl-meta` decode:

```
$ /tmp/mc admin info ec | tail -1
11 drives online, 1 drive offline, EC:4
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin ec/ecbucket/degraded.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `ec/ecbucket/degraded.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 1.00 MiB │ 1.00 MiB    │ 00m00s   │ 30.64 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
$ go run ./docs/debugging/xl-meta /tmp/blitzy-repro/ec-data/disk1/ecbucket/degraded.bin/xl.meta
{
    "Versions": [
        {
            "Header": {
                "EcM": 7,
                "EcN": 5,
                "Flags": 2,
                "ModTime": "2026-07-08T05:50:49.797291888Z",
                "Signature": "de2760cd",
                "Type": 1,
                "VersionID": "00000000000000000000000000000000"
            },
            "Idx": 0,
            "Metadata": {
                "Type": 1,
                "V2Obj": {
                    "CSumAlgo": 1,
                    "DDir": "OSaPJrDZTpumSKvcHMZlhA==",
                    "EcAlgo": 1,
                    "EcBSize": 1048576,
                    "EcDist": [ 8, 9, 10, 11, 12, 1, 2, 3, 4, 5, 6, 7 ],
                    "EcIndex": 8,
                    "EcM": 7,
                    "EcN": 5,
                    "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
                    "MTime": 1783489849797291888,
                    "MetaSys": {
                        "x-minio-internal-erasure-upgraded": "NC0+NQ=="
                    },
                    "MetaUsr": {
                        "content-type": "application/octet-stream",
                        "etag": "d1897826d5343e70d38ff11e37708e39"
                    },
                    "PartASizes": [ 1048576 ],
                    "PartETags": null,
                    "PartNums": [ 1 ],
                    "PartSizes": [ 1048576 ],
                    "Size": 1048576
                },
                "v": 1732554622
            }
        }
    ]
}
```

Reading it: the PUT **SUCCEEDED** with the drive offline, and `"EcM": 7` / `"EcN": 5` shows parity
was upgraded from 4 to 5. `MetaSys` now carries
`"x-minio-internal-erasure-upgraded": "NC0+NQ=="`; base64-decoding the value proves the upgrade:

```
$ echo -n 'NC0+NQ==' | base64 -d
4->5
```

Exactly **11 shards** are physically present (disk12, the broken symlink, holds none):

```
$ ls /tmp/blitzy-repro/ec-data/disk{1..12}/ecbucket/degraded.bin/*/part.1 2>/dev/null | wc -l
11
```

The upgraded object is still fully readable and byte-identical to the source (md5 matches the
1 MiB test object):

```
$ /tmp/mc cat ec/ecbucket/degraded.bin | md5sum
d1897826d5343e70d38ff11e37708e39  -
```

This demonstrates the `parityOrig != parityDrives` branch (`cmd/erasure-object.go:L1315-1316`)
recording the upgrade (`4->5`) when default parity < `N/2`. In the canonical 4-drive set that
branch is a no-op because default parity already equals the ceiling — hence §4.1's empty
`MetaSys`.

### 4.3 Q1b — 2 drives offline (2 online < writeQuorum 3) → FAILS (client-visible)

Induce the second failure (drive4 already offline), then confirm the degraded state (complete
`mc admin info` output):

```
$ umount -l /mnt/drive3
$ /tmp/mc admin info myminio
●  127.0.0.1:9000
   Uptime: 4 minutes 
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK 
   Drives: 2/4 OK 
   Pool: 1

┌──────┬───────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage          │ Erasure stripe size │ Erasure sets │
│ 1st  │ 0.4% (total: 1.8 GiB) │ 4                   │ 1            │
└──────┴───────────────────────┴─────────────────────┴──────────────┘

5.0 MiB Used, 1 Bucket, 5 Objects
2 drives online, 2 drives offline, EC:2
```

Write through the **raw S3 API** using a boto3-generated presigned PUT URL and `curl -isS`, so the
exact wire response (status line, all headers, full body) is captured verbatim rather than
summarized by `mc`. The presigned URL used (expired; `X-Amz-Expires=300`):

```
$ URL='http://127.0.0.1:9000/q1bucket/twodrives-off.bin?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=minio%2F20260708%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260708T054251Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=a297489910e40bff1b436ee54d7c42cd8ec1e9451c0e6aeadc5bfb0d32911baa'
$ curl -isS -X PUT --data-binary @/tmp/blitzy-repro/obj1mb.bin "$URL"
```

Complete, unedited response — **status line + every response header** (Content-Length is `393`,
matching the body below byte-for-byte):

```
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 393
Content-Type: application/xml
Retry-After: 60
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C03A22510E6D80
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 561721
X-Ratelimit-Remaining: 561721
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:42:51 GMT
Connection: close
```

Complete, unedited **response body** — exactly 393 bytes: an XML declaration line, a newline, then
the single-line `<Error>` element, with no trailing newline (the saved response body measures
`wc -c` → `393`, matching the `Content-Length: 393` header above):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownWrite</Code><Message>Resource requested is unwritable, please reduce your request rate</Message><Key>twodrives-off.bin</Key><BucketName>q1bucket</BucketName><Resource>/q1bucket/twodrives-off.bin</Resource><RequestId>18C03A22510E6D80</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The `mc` client path corroborates it — every attempt returns `503 SlowDownWrite` (complete
`mc cp` output; exit status 1):

```
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin myminio/q1bucket/twodrives-off-mc.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `myminio/q1bucket/twodrives-off-mc.bin`
mc: <ERROR> Failed to copy `/tmp/blitzy-repro/obj1mb.bin`. Resource requested is unwritable, please reduce your request rate
```

The object was **NOT created** — a subsequent `mc ls myminio/q1bucket/twodrives-off.bin` returns
no entry (empty output).

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

Reference object `readtest.bin` — the same 1 MiB test object, uploaded into `myminio/q1bucket`
while all 4 drives were healthy (complete `mc cp` output):

```
$ /tmp/mc cp /tmp/blitzy-repro/obj1mb.bin myminio/q1bucket/readtest.bin
`/tmp/blitzy-repro/obj1mb.bin` -> `myminio/q1bucket/readtest.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 1.00 MiB │ 1.00 MiB    │ 00m00s   │ 28.66 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
```

Its source md5 is `d1897826d5343e70d38ff11e37708e39`. Each read below is verified by downloading
the object to a file and hashing that file, so the `md5sum` output carries the filename field
(not a piped `-`).

- **1 drive offline** — drive-state confirmed by `mc admin info` (the complete block appears in
  §4.1: "3 drives online, 1 drive offline, EC:2"). Download + hash (complete, unedited output):

  ```
  $ /tmp/mc cat myminio/q1bucket/readtest.bin > /tmp/blitzy-repro/q2a_1off.bin
  $ md5sum /tmp/blitzy-repro/q2a_1off.bin
  d1897826d5343e70d38ff11e37708e39  /tmp/blitzy-repro/q2a_1off.bin
  ```

  ✓ MATCHES the source md5 — the read SUCCEEDS via reconstruction.

- **2 drives offline** (the read-quorum boundary) — drive-state confirmed by `mc admin info`
  (the complete block appears in §4.3: "2 drives online, 2 drives offline, EC:2"). Download +
  hash (complete, unedited output):

  ```
  $ /tmp/mc cat myminio/q1bucket/readtest.bin > /tmp/blitzy-repro/q2a_2off.bin
  $ md5sum /tmp/blitzy-repro/q2a_2off.bin
  d1897826d5343e70d38ff11e37708e39  /tmp/blitzy-repro/q2a_2off.bin
  ```

  ✓ MATCHES. Reconstruction succeeds from exactly 2 healthy shards because K=2 data shards
  suffice to rebuild the object.

### 5.2 Q2b — reads FAIL below read quorum (1 online < readQuorum 2) — clean object-level GET

To isolate the `GetObject` path (bypassing `mc`'s `GetBucketLocation` preflight), a
**boto3-generated presigned GET URL** was fetched with `curl`, so the exact wire response is
captured verbatim. A fresh URL is signed for each attempt (presigned URLs expire); the full
signed URLs are shown below (both now expired).

- **Healthy baseline (4 online)** — generate the presigned URL and fetch it. Complete, unedited
  output (full signed URL; `-D -` dumps the status line + all headers to stdout while the body is
  written to a file, then the file size and md5 are printed):

  ```
  $ URLH=$(python3 /tmp/blitzy-repro/presign.py get_object q1bucket readtest.bin 3600)
  $ echo "$URLH"
  http://127.0.0.1:9000/q1bucket/readtest.bin?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=minio%2F20260708%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260708T054031Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=f7911e4b230156217b087d67599a0dcb85055e30216fec963176da61eb0c9dd6
  $ curl -sS -D - -o /tmp/blitzy-repro/q2a_baseline.bin "$URLH"
  HTTP/1.1 200 OK
  Accept-Ranges: bytes
  Content-Length: 1048576
  Content-Type: application/octet-stream
  ETag: "d1897826d5343e70d38ff11e37708e39"
  Last-Modified: Wed, 08 Jul 2026 05:40:18 GMT
  Server: MinIO
  Strict-Transport-Security: max-age=31536000; includeSubDomains
  Vary: Origin
  Vary: Accept-Encoding
  X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
  X-Amz-Request-Id: 18C03A01A50EE4C7
  X-Content-Type-Options: nosniff
  X-Ratelimit-Limit: 561721
  X-Ratelimit-Remaining: 561721
  X-Xss-Protection: 1; mode=block
  Date: Wed, 08 Jul 2026 05:40:31 GMT
  $ wc -c < /tmp/blitzy-repro/q2a_baseline.bin
  1048576
  $ md5sum /tmp/blitzy-repro/q2a_baseline.bin
  d1897826d5343e70d38ff11e37708e39  /tmp/blitzy-repro/q2a_baseline.bin
  ```

  → HTTP 200, 1048576 bytes, md5 `d1897826d5343e70d38ff11e37708e39` ✓ (matches source): while
  healthy the object reads back byte-exact.

- **Degrade to 1 online** (`umount -l` drives 2, 3, 4). Drive-state (complete `mc admin info`
  output):

  ```
  $ /tmp/mc admin info myminio
  ●  127.0.0.1:9000
     Uptime: 5 minutes 
     Version: 2024-11-25T17:10:22Z
     Network: 1/1 OK 
     Drives: 1/4 OK 
     Pool: 1

  ┌──────┬───────────────────────┬─────────────────────┬──────────────┐
  │ Pool │ Drives Usage          │ Erasure stripe size │ Erasure sets │
  │ 1st  │ 0.4% (total: 906 MiB) │ 4                   │ 1            │
  └──────┴───────────────────────┴─────────────────────┴──────────────┘

  1 drive online, 3 drives offline, EC:2
  ```

- **Raw object GET below read quorum** — sign a fresh URL and fetch it. Complete status line +
  all response headers (Content-Length `382`, matching the body below byte-for-byte):

  ```
  $ URL=$(python3 /tmp/blitzy-repro/presign.py get_object q1bucket readtest.bin 300)
  $ echo "$URL"
  http://127.0.0.1:9000/q1bucket/readtest.bin?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=minio%2F20260708%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260708T054350Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=6fa8cd4c9cdd5ca021ce05751c27ba53b76d1c8560b96e00d3c0abf9d6540b0a
  $ curl -isS "$URL"
  HTTP/1.1 503 Service Unavailable
  Accept-Ranges: bytes
  Content-Length: 382
  Content-Type: application/xml
  Retry-After: 60
  Server: MinIO
  Strict-Transport-Security: max-age=31536000; includeSubDomains
  Vary: Origin
  Vary: Accept-Encoding
  X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
  X-Amz-Request-Id: 18C03A301CBD7EB5
  X-Content-Type-Options: nosniff
  X-Ratelimit-Limit: 561721
  X-Ratelimit-Remaining: 561721
  X-Xss-Protection: 1; mode=block
  Date: Wed, 08 Jul 2026 05:43:50 GMT
  ```

  Complete, unedited response body — exactly 382 bytes (declaration line, a newline, then the
  single-line `<Error>` element, no trailing newline; `wc -c` → `382`, matching
  `Content-Length: 382`):

  ```xml
  <?xml version="1.0" encoding="UTF-8"?>
  <Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>readtest.bin</Key><BucketName>q1bucket</BucketName><Resource>/q1bucket/readtest.bin</Resource><RequestId>18C03A301CBD7EB5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
  ```

The `<Key>readtest.bin</Key>` element proves this is the object read path (not bucket-location).
`mc cat --debug` (which does `GetBucketLocation` first) returns the same `503 SlowDownRead`.
Stability confirmed on repeat.

**Q2 causal chain / citations:**

- Read-quorum computed per object: `objectQuorumFromMeta` at `cmd/erasure-metadata.go:L531`,
  with `expectedRQuorum := len(partsMetaData)/2` (`cmd/erasure-metadata.go:L533`) = 2 for a
  4-drive set; `reduceReadQuorumErrs` at `cmd/erasure-metadata.go:L539` returns
  `errErasureReadQuorum` when fewer than 2 valid `xl.meta` reads are available.
- Reduction helper: `reduceReadQuorumErrs` at `cmd/erasure-metadata-utils.go:L150-151`
  (delegates to `reduceQuorumErrs` `cmd/erasure-metadata-utils.go:L137-145`).
- Additional read-quorum return sites: `cmd/erasure-object.go:L487`
  `return FileInfo{}, errErasureReadQuorum`; `cmd/erasure-object.go:L836` `reduceReadQuorumErrs`
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
a **fresh, unformatted disk** (`mkfs` wipes the backing loop image):

```
mkfs.ext4 -F -q /tmp/blitzy-repro/images/disk4.img
mount -o loop /tmp/blitzy-repro/images/disk4.img /mnt/drive4
```

Its contents were then only `lost+found`, with no `format.json` (shown with complete output in
§6.2 below). The granular §6.2 proof was captured on this same deployment
(`829673bc-44fc-42c4-afb8-d5c5867216bb`, same 6-object set) by failing drive4 and returning it
fresh, so the before/after drive4 inspection is directly observable.

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

**Observed proof — complete, unedited command output.** The healthy reference drive (drive1)
holds `healme.bin` as a data-dir shard plus `xl.meta`, and the cluster `format.json` is 342 bytes:

```
$ ls -la /mnt/drive1/.minio.sys/format.json
-rw-r--r--+ 1 root root 342 Jul  8 05:38 /mnt/drive1/.minio.sys/format.json
$ find /mnt/drive1/q1bucket/healme.bin -type f -printf '%s\t%p\n'
524320	/mnt/drive1/q1bucket/healme.bin/428b25fe-de14-4c6e-9e59-885be553f911/part.1
368	/mnt/drive1/q1bucket/healme.bin/xl.meta
```

**BEFORE heal** — fail drive4 and return it fresh; drive4 then has only `lost+found`, no
`format.json`, and `healme.bin`'s `part.*` count is **0**:

```
$ umount -l /mnt/drive4
$ mkfs.ext4 -F -q /tmp/blitzy-repro/images/disk4.img
$ mount -o loop /tmp/blitzy-repro/images/disk4.img /mnt/drive4
$ ls -la /mnt/drive4
total 24
drwxr-xr-x 3 root root  4096 Jul  8 06:14 .
drwxr-xr-x 1 root root  4096 Jul  8 05:47 ..
drwx------ 2 root root 16384 Jul  8 06:14 lost+found
$ ls -la /mnt/drive4/.minio.sys/format.json
ls: cannot access '/mnt/drive4/.minio.sys/format.json': No such file or directory
$ find /mnt/drive4/q1bucket/healme.bin -name 'part.*' | wc -l
0
```

**AFTER heal** (the background monitor healed drive4 with no restart — logs in §6.3) — drive4
regained `format.json` (342 B, written by the `HealFormat` reformat) and `healme.bin` regained
its `part.1` (count **0 → 1**) plus `xl.meta`, reconstructed into the **same data-dir UUID**
`428b25fe-de14-4c6e-9e59-885be553f911` as drive1 — proving the missing-shard criterion fired:

```
$ ls -la /mnt/drive4
total 32
drwxr-xr-x 5 root root  4096 Jul  8 06:14 .
drwxr-xr-x 1 root root  4096 Jul  8 05:47 ..
drwxr-xr-x 6 root root  4096 Jul  8 06:14 .minio.sys
drwx------ 2 root root 16384 Jul  8 06:14 lost+found
drwxr-xr-x 8 root root  4096 Jul  8 06:14 q1bucket
$ ls -la /mnt/drive4/.minio.sys/format.json
-rw-r--r--+ 1 root root 342 Jul  8 06:14 /mnt/drive4/.minio.sys/format.json
$ find /mnt/drive4/q1bucket/healme.bin -type f -printf '%s\t%p\n'
368	/mnt/drive4/q1bucket/healme.bin/xl.meta
524320	/mnt/drive4/q1bucket/healme.bin/428b25fe-de14-4c6e-9e59-885be553f911/part.1
$ find /mnt/drive4/q1bucket/healme.bin -name 'part.*' | wc -l
1
```

Every object regained its shard on drive4 (all `part.*`=1 and `xl.meta` present):

```
$ for o in healme-base.bin healme.bin healthy-obj.bin onedrive-off.bin onedrive-off-2.bin readtest.bin; do
    echo "$o: part.*=$(find /mnt/drive4/q1bucket/$o -name 'part.*' | wc -l) xl.meta=$(find /mnt/drive4/q1bucket/$o -name 'xl.meta' | wc -l)"
  done
healme-base.bin: part.*=1 xl.meta=1
healme.bin: part.*=1 xl.meta=1
healthy-obj.bin: part.*=1 xl.meta=1
onedrive-off.bin: part.*=1 xl.meta=1
onedrive-off-2.bin: part.*=1 xl.meta=1
readtest.bin: part.*=1 xl.meta=1
```

Re-reading `healme.bin` through the S3 API returns byte-exact data (md5 matches the 1 MiB source
object `d1897826d5343e70d38ff11e37708e39`):

```
$ /tmp/mc cat myminio/q1bucket/healme.bin > /tmp/blitzy-repro/healme_reread.bin
$ md5sum /tmp/blitzy-repro/healme_reread.bin
d1897826d5343e70d38ff11e37708e39  /tmp/blitzy-repro/healme_reread.bin
```

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

> **Safety note (investigation/test-only):** `MINIO_PROMETHEUS_AUTH_TYPE=public` is set here **only**
> so this read-only investigation can scrape metrics with plain `curl` reproducibly; it disables
> authentication on the metrics endpoints and is **not recommended as a blanket production default**.
> The canonical default is JWT auth (`cmd/metrics-router.go:L56`). This flag changes only *who may
> scrape* the endpoint — the metric values reported below are identical regardless of auth type.

### 7.1 BEFORE (4 online)

Each metric block below shows, on its first line, the exact `curl`-piped-to-`grep` command that was
run, followed by its **complete, unedited output** — the raw endpoints emit the full Prometheus
exposition (hundreds of series), and the `grep` narrows to exactly the drive-count series that
answer Q4.

- v3 health metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
  minio_cluster_health_drives_count 4
  minio_cluster_health_drives_online_count 4
  ```

  Note: `minio_cluster_health_drives_offline_count` is **OMITTED** here — v3 suppresses
  zero-valued gauges (see §7.5 nuance + `cmd/metrics-v3-types.go:L239-245` citation).

- v2 cluster metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/v2/metrics/cluster | grep -E '^minio_cluster_drive_(on|off)line_total'
  minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
  minio_cluster_drive_online_total{server="127.0.0.1:9000"} 4
  ```

- per-set v3 metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/erasure-set | grep '^minio_cluster_erasure_set_online_drives_count'
  minio_cluster_erasure_set_online_drives_count{pool_id="0",set_id="0"} 4
  ```

- health endpoints (complete `curl -sI` status line + quorum header for each):

  ```
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -E 'HTTP|Write-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Write-Quorum: 3
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster/read | grep -E 'HTTP|Read-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Read-Quorum: 2
  ```

### 7.2 DURING (drive4 offline, 3 online)

Same commands as §7.1. The cluster-aggregate v2/v3 series **and** the per-erasure-set series are
each served from a 1-minute TTL cache (§7.5), so all three were scraped after their cache window
rolled over so the transition is visible; only `mc admin info` reflects the change immediately — it
queries live per-node disk state, not the metrics cache (see §7.5).

- v3 health metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
  minio_cluster_health_drives_count 4
  minio_cluster_health_drives_offline_count 1
  minio_cluster_health_drives_online_count 3
  ```

  (the `_offline_count` gauge is now emitted because its value is > 0)

- v2 cluster metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/v2/metrics/cluster | grep -E '^minio_cluster_drive_(on|off)line_total'
  minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 1
  minio_cluster_drive_online_total{server="127.0.0.1:9000"} 3
  ```

- per-set v3 metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/erasure-set | grep '^minio_cluster_erasure_set_online_drives_count'
  minio_cluster_erasure_set_online_drives_count{pool_id="0",set_id="0"} 3
  ```

- health endpoints (both still HTTP 200 — 1 offline still satisfies write-quorum 3 and read-quorum
  2; the headers report the REQUIRED quorum, not the live online count):

  ```
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -E 'HTTP|Write-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Write-Quorum: 3
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster/read | grep -E 'HTTP|Read-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Read-Quorum: 2
  ```

### 7.3 AFTER (post-heal, 4 online)

After drive4 was healed (Q3), the drive-count series return to the BEFORE values — the v3
`_offline_count` gauge is again suppressed at 0, while v2 still emits `_offline_total 0`. Complete,
unedited outputs of the same commands:

- v3 health metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
  minio_cluster_health_drives_count 4
  minio_cluster_health_drives_online_count 4
  ```

- v2 cluster metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/v2/metrics/cluster | grep -E '^minio_cluster_drive_(on|off)line_total'
  minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
  minio_cluster_drive_online_total{server="127.0.0.1:9000"} 4
  ```

- per-set v3 metrics:

  ```
  $ curl -s http://127.0.0.1:9000/minio/metrics/v3/cluster/erasure-set | grep '^minio_cluster_erasure_set_online_drives_count'
  minio_cluster_erasure_set_online_drives_count{pool_id="0",set_id="0"} 4
  ```

- health endpoints:

  ```
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -E 'HTTP|Write-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Write-Quorum: 3
  $ curl -sI http://127.0.0.1:9000/minio/health/cluster/read | grep -E 'HTTP|Read-Quorum'
  HTTP/1.1 200 OK
  X-Minio-Read-Quorum: 2
  ```

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
  suppresses zero-valued gauges. The mechanism is in the v3 metric setter at
  `cmd/metrics-v3-types.go:L239-245`, which guards the append with the comment
  `// If valid non zero value set the metrics` and the condition `if value > 0 { m.values[name] = append(v, metricValue{Labels: labelMap, Value: value}) }` —
  a gauge series is recorded only when its value is strictly greater than 0, so a 0-valued
  `_offline_count` is never appended and therefore never exposed. Only the `_count` and
  `_online_count` series appear at 0 offline; the `_offline_count` series reappears once its value
  is > 0 (DURING). Do not mistake the absent series for a missing metric.
- **1-minute TTL cache on cluster aggregates:** the cluster-aggregate v2/v3 drive metrics are
  served from a **1-minute TTL cache**. The function `newClusterStorageInfoCache`
  (`cmd/metrics-v3-cache.go:L256`) builds it and, at `cmd/metrics-v3-cache.go:L273-275`, returns
  `cachevalue.NewFromFunc(1*time.Minute, cachevalue.Opts{ReturnLastGood: true}, loadStorageInfo)` —
  i.e. the `1*time.Minute` TTL (`L273`) and `ReturnLastGood: true` (`L274`) are set on that
  `NewFromFunc` call. Therefore, depending on scrape timing relative to the cache window, the
  online/offline transition appears either immediately (cache expired) or lags by up to ~1 minute.
- **The per-erasure-set metric is ALSO 1-minute-TTL cached — it is NOT live.** The per-set loader
  `loadClusterErasureSetMetrics` reads the drive count from a cache, not from a live call:
  `result, _ := c.esetHealthResult.Get()` (`cmd/metrics-v3-cluster-erasure-set.go:L84`), and the
  online count is set from `h.HealthyDrives` of that cached result at
  `cmd/metrics-v3-cluster-erasure-set.go:L95`. The `esetHealthResult` cache is built by
  `newESetHealthResultCache` (`cmd/metrics-v3-cache.go:L103-116`) as
  `cachevalue.NewFromFunc(1*time.Minute, cachevalue.Opts{ReturnLastGood: true}, loadHealth)`
  (`L113-115`), whose `loadHealth` calls `objLayer.Health(GlobalContext, HealthOptions{})` (`L110`)
  — i.e. it reads `Health()`, **not** `StorageInfo`. So this metric lags a drive-state change by up
  to ~1 minute, exactly like the v2/v3 aggregates above. Observed on this cluster (drive4 induced
  offline at t=0, then later restored; `mc admin info` scraped in parallel with the per-set metric
  `minio_cluster_erasure_set_online_drives_count` at each timestamp):

  ```
  # DEGRADE  (drive4 -> offline at t=0)
  t+3s    mc admin info: 3 drives online, 1 drive offline    per-set online_drives_count = 4  (stale)
  t+35s   mc admin info: 3 drives online, 1 drive offline    per-set online_drives_count = 4  (stale)
  t+45s   mc admin info: 3 drives online, 1 drive offline    per-set online_drives_count = 3  (updated)
  # RESTORE (drive4 -> back online at t=0)
  t+5s    mc admin info: 4 drives online, 0 drives offline   per-set online_drives_count = 3  (stale)
  t+55s   mc admin info: 4 drives online, 0 drives offline   per-set online_drives_count = 3  (stale)
  t+65s   mc admin info: 4 drives online, 0 drives offline   per-set online_drives_count = 4  (updated)
  ```

  In both transitions `mc admin info` reports the new count within a few seconds while the per-set
  gauge holds its stale value until the cache window rolls over (~1 minute).
- **Only `mc admin info` reflects the change immediately.** It is served by `getServerInfo`
  (`cmd/admin-handlers.go:L2369`), which builds the drive counts from a live per-node query —
  `globalNotificationSys.ServerInfo(...)` plus the local server's properties, reduced by
  `getOnlineOfflineDisksStats(allDisks)` (`cmd/admin-handlers.go:L2434`) — and therefore does
  **not** go through any of the 1-minute metrics caches. A reader who scrapes any of the drive-count
  metrics right after inducing failure and sees a stale count is observing these caches, not a bug;
  corroborate the live state with `mc admin info` (or the health endpoints).

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
- `cmd/erasure-metadata.go:L539` — `reduceReadQuorumErrs`
- `cmd/erasure-metadata-utils.go:L150-151` — `reduceReadQuorumErrs`
- `cmd/erasure-object.go:L487` — `return FileInfo{}, errErasureReadQuorum`
- `cmd/erasure-object.go:L836` — `reduceReadQuorumErrs` in `getObjectFileInfo`
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
- `cmd/background-newdisks-heal-ops.go:L503` — "Healing of drive '%s' is incomplete, retrying %s time (healed: %d, skipped: %d, failed: %d)." (sibling, not hit)
- `cmd/background-newdisks-heal-ops.go:L517` — "Healing of drive '%s' is complete, retried %d times (healed: %d, skipped: %d)." (sibling, not hit)
- `cmd/logging.go:L83-84` — `healingLogEvent` → `logger.Event(ctx, "healing", msg, args...)`
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
- `cmd/metrics-v3-cache.go:L256` — `newClusterStorageInfoCache` (builds the cluster storage-info cache)
- `cmd/metrics-v3-cache.go:L273-275` — `NewFromFunc(1*time.Minute, cachevalue.Opts{ReturnLastGood: true}, loadStorageInfo)` (1-minute TTL, `ReturnLastGood` on the drive-count aggregate cache)
- `cmd/metrics-v3-cluster-erasure-set.go:L84` — `result, _ := c.esetHealthResult.Get()` (the per-erasure-set loader reads the drive count from the 1-minute cached health result, NOT a live call)
- `cmd/metrics-v3-cache.go:L103-116` — `newESetHealthResultCache` builds the per-erasure-set health cache as `NewFromFunc(1*time.Minute, cachevalue.Opts{ReturnLastGood: true}, loadHealth)` (`L113-115`); `loadHealth` reads `objLayer.Health(GlobalContext, HealthOptions{})` (`L110`) — `Health()`, not `StorageInfo` — so the per-set drive-count metric is 1-minute-TTL cached exactly like the aggregates
- `cmd/admin-handlers.go:L2369` — `getServerInfo` builds `mc admin info` drive counts from a live per-node query (`globalNotificationSys.ServerInfo(...)` + local server properties), bypassing the metrics caches
- `cmd/admin-handlers.go:L2434` — `onlineDisks, offlineDisks := getOnlineOfflineDisksStats(allDisks)` reduces the live disks to the online/offline counts `mc admin info` reports immediately
- `cmd/metrics-v3-types.go:L239-245` — zero-gauge suppression guard: `// If valid non zero value set the metrics` + `if value > 0 { m.values[name] = append(v, metricValue{Labels: labelMap, Value: value}) }` (drops the `drives_offline_count` series while its value is 0)
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

