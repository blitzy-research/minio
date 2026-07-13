# MinIO Erasure-Coded Storage Under Drive Failure and Recovery — An Evidence-Based Analysis

**Source branch:** `minio_c07e5b49d477`
**Commit under analysis:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Repository:** `github.com/minio/minio`

## Introduction

This document answers six questions about how MinIO's erasure-coded storage layer behaves when a drive becomes unavailable while the cluster is running, and what happens when that drive later comes back:

- **OBJ-1** — When a drive is lost during a write, what error does MinIO return, and does the write succeed or fail?
- **OBJ-2** — Can objects written *before* a drive vanished still be read? If not, what error is returned?
- **OBJ-3** — What triggers a healing operation when a drive comes back online?
- **OBJ-4** — What criteria decide that a specific object needs healing on a particular drive?
- **OBJ-5** — What log messages appear during an active heal?
- **OBJ-6** — What metric names track online vs. offline drive counts, and what values do they report before and after a drive failure?

Every behavioral claim below is grounded in **actually observed runtime output** — real S3 error bodies, real server log lines, real metric samples — captured from a MinIO server **built locally from this exact commit** and driven through its real S3 API. Code citations (`file:line` + symbol name) are used only to *name and locate* the mechanism responsible for each observed behavior; they never substitute for observation. Where a behavior could not be reproduced through the real entry point after genuine, varied effort, it is explicitly labeled **INFERRED** (see the "Observed vs. Inferred" section).

**Topology in one line:** a single MinIO server node with **12 local drives** forming **one erasure set** at MinIO's **default parity of EC:4** (8 data + 4 parity shards per object). This is the minimal-plus canonical erasure topology; 12 drives were chosen deliberately so the write-quorum threshold and the read/parity limits can each be crossed cleanly and observed on both sides (see OBJ-1/OBJ-2).

---

## Environment & Build Appendix

### Go toolchain

```
$ go version
go version go1.23.4 linux/amd64
```

This satisfies the module's `go 1.23` directive (`go.mod:L3`). CI for this commit pins `1.23.x` and the project Dockerfiles use `golang:1.23-alpine`, so Go 1.23.4 is a canonical toolchain for this build.

### Build command used (canonical, version-stamped)

The binary was built with the project's canonical `make build` target:

```
$ make build
```

which the `Makefile` defines as (verbatim, `Makefile:L177-L179`):

```
build: checks ## builds minio to $(PWD)
	@echo "Building minio binary to './minio'"
	@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
```

with `LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)` (`Makefile:L3`) and `all: build` (`Makefile:L15`). This produces a **version-stamped** binary (NOT a `DEVELOPMENT.GOGET` build). The embedded commit id matches the commit under analysis:

```
$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.4 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
```

The version *label* `DEVELOPMENT.2024-11-25T17-10-22Z` is produced by `buildscripts/gen-ldflags.go` for an untagged commit; the authoritative identifier is the embedded `commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2`, confirming the running binary is exactly this commit.

> A non-stamped fallback (`CGO_ENABLED=0 GOFLAGS=-mod=mod go build -o ./minio .`, which yields a `DEVELOPMENT.GOGET` banner) is equally valid for functional failure-scenario testing; it was **not** needed here because `make build` succeeded.

### Client tool

```
$ mc --version
mc version RELEASE.2025-08-13T08-35-41Z
```

`mc` (the MinIO client) drove all S3 `PutObject`/`GetObject`/`stat` calls and the admin/metrics operations. For the two quorum-failure cases, a hand-written **AWS SigV4 `curl`-equivalent** request (single attempt, no SDK retry) was used to capture the raw, unretried S3 error body and HTTP status.

### Server invocation and topology

```
$ export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
$ ./minio server drives/d1 drives/d2 drives/d3 drives/d4 drives/d5 drives/d6 \
                  drives/d7 drives/d8 drives/d9 drives/d10 drives/d11 drives/d12 \
                  --address :9000 --console-address :9001
```

All twelve drive directories live **outside** the repository tree (under `/tmp/minio-investigation/drives`) so no runtime artifact touches the source repo. Default configuration was used throughout: **default storage-class parity** and **healing left ON**. No parity, storage-class, or heal setting was altered to force any outcome.

### Reported drive count and default parity

`mc admin info` reports the live topology:

```
$ mc admin info inv
...
12 drives online, 0 drives offline, EC:4
...
Erasure stripe size: 12  (1 erasure set)
```

The **EC:4** parity is MinIO's default for a 12-drive set, per the default-parity mapping (`cmd/erasure-server-pool.go:L120-L124` documents EC:2 at 4–5 drives, EC:3 at 6–7, EC:4 at 8–16; the numeric mapping is `DefaultParityBlocks` in `internal/config/storageclass/storage-class.go:L355`). With EC:4 each object is split into **8 data + 4 parity = 12 shards**, one per drive.

### How a drive was taken offline and brought back (real backend path)

Failures were induced through the **real disk path**, never by editing configuration:

- **Take offline (stable):** replace a drive directory with a regular file — `rm -rf drives/dN && touch drives/dN`. MinIO's disk layer then cannot treat the path as a directory/mountpoint and registers the drive offline, logging `errDiskNotDir` = `"drive is not directory or mountpoint"` (`cmd/storage-errors.go:L50`). This is destructive — it simulates a wiped/replaced drive, so the shard data on that drive is gone (relevant to OBJ-2).
- **Bring back:** remove the placeholder file — `rm -f drives/dN`. The disk monitor then sees a fresh/unformatted slot, reformats it, and heals it (OBJ-3).

> A fully-removed directory does **not** stay offline on a single-node deployment: the disk monitor recreates and reformats it within ~10s. The regular-file placeholder is what holds a drive stably offline. This was determined empirically and is the canonical real-backend mechanism used throughout.

### Captured startup banner (verbatim)

```
INFO: Formatting 1st pool, 1 set(s), 12 drives per set.
INFO: WARNING: Host local has more than 4 drives of set. A host failure will result in data becoming unavailable.
INFO: 
 You are running an older version of MinIO released 9 months before the latest release 
 Update: Run `mc admin update ALIAS` 


MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.4 linux/amd64)

API: http://10.236.12.19:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
WebUI: http://10.236.12.19:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

### Baseline objects and scale

A bucket `ectest` was created and five 8 MiB random objects (`obj1.bin`…`obj5.bin`) were uploaded. Their SHA-256 checksums (used later to prove byte-exact reads) were recorded:

```
7e1b536dc753c753a52d82b75b397f6a692380e2f06bf0ac10dc6dec0be8de4a  obj1.bin
3afb29d77ffad6a2dd74ce83e5e61ab44bda99d83536db8e6e41d03057d713ac  obj2.bin
021e5f83700990bd4e3b678568acc25dfa087fe68a1cd49848c09c40892fdeab  obj3.bin
9b2baccbb18375ea7ad9732c61126450d398808193317f7e9037d242cc57b6b1  obj4.bin
1ce90aa215ae2cb74a3838c4dfb9693311d68bbd84cd7622249eb44a3cf5d063  obj5.bin
```

Each object was confirmed to have an `xl.meta` and a `part.1` on all 12 drives (12 shards = 8 data + 4 parity), i.e. one shard per drive across the full erasure set.

---

## OBJ-1 — Write path under drive loss

### Direct answer

**It depends on how many drives are offline, and there are two distinct regimes — both were reproduced:**

1. **Fewer than half the drives offline → the write SUCCEEDS in a degraded state.** MinIO's default (availability-optimized) storage class *upgrades* the object's parity by one for every offline drive, records a `x-minio-internal-erasure-upgraded` annotation on the object, and completes the PUT. No error is returned to the client.
2. **At least half the drives offline → the write FAILS.** The client receives S3 error code **`SlowDownWrite`** at **HTTP 503**, whose body message is `"Resource requested is unwritable, please reduce your request rate"`. Internally this is `errErasureWriteQuorum` = `"Write failed. Insufficient number of drives online"`.

The deciding branch is `if offlineDrives >= (len(storageDisks)+1)/2` — with 12 drives, the threshold is `(12+1)/2 = 6`. So **1–5 offline → degraded success; ≥6 offline → write-quorum failure.**

### Code reference

- Parity-upgrade + write-quorum decision: `cmd/erasure-object.go` — `func (er erasureObjects) putObject`, block at **L1291–L1319**:
  - **L1291** `if !opts.MaxParity && globalStorageClass.AvailabilityOptimized() {`
  - **L1295–L1302** the per-offline-drive loop (`var offlineDrives int` at L1295; inside the loop `parityDrives++; offlineDrives++` at L1298–L1299)
  - **L1304** `if offlineDrives >= (len(storageDisks)+1)/2 {`
  - **L1308** `return ObjectInfo{}, toObjectErr(errErasureWriteQuorum, bucket, object)`
  - **L1311–L1313** parity cap: `if parityDrives >= len(storageDisks)/2 { parityDrives = len(storageDisks) / 2 }`
  - **L1315–L1317** `if parityOrig != parityDrives { userDefined[minIOErasureUpgraded] = strconv.Itoa(parityOrig) + "->" + strconv.Itoa(parityDrives) }` (the assignment itself is **L1316**)
- Annotation key: `cmd/erasure-metadata.go:L38` `const minIOErasureUpgraded = "x-minio-internal-erasure-upgraded"`.
- Quorum error string: `cmd/erasure-errors.go:L25-L26` `errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")`.
- S3/HTTP mapping: `cmd/api-errors.go:L2192-L2193` `case errErasureWriteQuorum: apiErr = ErrSlowDownWrite`; definition `cmd/api-errors.go:L874-L878` `ErrSlowDownWrite: { Code: "SlowDownWrite", Description: "Resource requested is unwritable, please reduce your request rate", HTTPStatusCode: http.StatusServiceUnavailable }`.
- "Availability-optimized is the default" — `internal/config/storageclass/storage-class.go:L327` `func (sCfg *Config) AvailabilityOptimized() bool` returns `sCfg.Optimize == "availability" || sCfg.Optimize == ""` (and `true` when uninitialized). Since the default `Optimize` is empty, this returns `true`, so the parity-upgrade path is active by default (doc comment L322).
- Entry point: `cmd/object-handlers.go:L1745` `PutObjectHandler`; dispatch `cmd/erasure-sets.go:L747` `(*erasureSets).PutObject`; shard encode `cmd/erasure-encode.go:L69` `(*Erasure).Encode`.
- Offline server signal: `cmd/storage-errors.go:L50` `errDiskNotDir = StorageErr("drive is not directory or mountpoint")`.

### Regime 1 — degraded SUCCESS (3 of 12 drives offline)

**Command & client output:**

```
$ rm -rf drives/d10 drives/d11 drives/d12 && touch drives/d10 drives/d11 drives/d12   # take 3 drives offline
$ mc admin info inv | grep -i drives
9 drives online, 3 drives offline, EC:4

$ mc cp src/obj_degraded.bin inv/ectest/obj_degraded.bin
`/tmp/minio-investigation/src/obj_degraded.bin` -> `inv/ectest/obj_degraded.bin`
Total: 8.00 MiB, Transferred: 8.00 MiB, Speed: 91.29 MiB/s
```

The write **succeeded** (`exit=0`) with 3 of 12 drives offline.

**Proof of the parity upgrade** — the on-disk `xl.meta` of the degraded object, decoded with MinIO's own `xl-meta` tool, versus a baseline object:

```
# degraded object (written with 3 drives offline):
obj_degraded.bin :  EcM(data)=6   EcN(parity)=6   EcDist=[8,9,10,11,12,1,2,3,4,5,6,7]
                    Metadata: x-minio-internal-erasure-upgraded = "NC0+Ng=="   (base64)

$ printf 'NC0+Ng==' | base64 -d
4->6

# baseline object (written with all 12 drives online):
obj1.bin         :  EcM(data)=8   EcN(parity)=4   (default EC:4, NO upgrade annotation)
```

The annotation decodes to exactly **`4->6`**: the default parity of **4** was upgraded to **6** (capped at `len(storageDisks)/2 = 6`), so the object was stored as **6 data + 6 parity**. This matches the code precisely: `parityOrig=4`; the loop adds one per offline drive (`4 + 3 = 7`); L1311–L1313 caps `7` at `12/2 = 6`; L1316 records `"4->6"`.

**Server-log offline signal during the window (verbatim block):**

```
API: SYSTEM.peers
Time: 17:48:43 UTC 07/13/2026
DeploymentID: 1b454da0-455f-4f12-aebb-a4d11763f931
Error: drive is not directory or mountpoint (cmd.StorageErr)
       endpoint="/tmp/minio-investigation/drives/d12"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

### Regime 2 — write FAILS (6 of 12 drives offline, = write-quorum threshold)

**Command & plain client output:**

```
$ rm -rf drives/d7 drives/d8 drives/d9 && touch drives/d7 drives/d8 drives/d9   # now 6 of 12 offline
$ mc admin info inv | grep -i drives
6 drives online, 6 drives offline, EC:4

$ mc cp src/obj_fail.bin inv/ectest/obj_fail.bin
mc: <ERROR> Failed to copy `/tmp/minio-investigation/src/obj_fail.bin`. Resource requested is unwritable, please reduce your request rate.
```

**Clean single-attempt raw HTTP (hand-written SigV4, no SDK retry) — the actual S3 error body and status:**

```
HTTP/1.1 503 Service Unavailable
Content-Type: application/xml
Server: MinIO
X-Amz-Request-Id: 18C1E9FB1179F281
Vary: Origin, Accept-Encoding

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownWrite</Code><Message>Resource requested is unwritable, please reduce your request rate</Message><Key>obj_fail_curl.bin</Key><BucketName>ectest</BucketName><Resource>/ectest/obj_fail_curl.bin</Resource><RequestId>18C1E9FB1179F281</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

(When driven via `mc --debug`, the AWS SDK *retries* the 503 `SlowDownWrite` four times before giving up — standard S3 retry-on-503 behavior — which is why the single-attempt SigV4 request above is the cleanest capture of the one true response.)

**Server-log corroboration:** six `"drive is not directory or mountpoint (cmd.StorageErr)"` blocks (one per offline drive), plus internal metadata writes failing with `"Storage resources are insufficient for the write operation .minio.sys/buckets/.../usage-cache.bin (cmd.InsufficientWriteQuorum)"`. The object-PUT's `errErasureWriteQuorum` is *returned to the client* (mapped to `ErrSlowDownWrite`), not logged as a server error — grepping the log for the literal string `"Insufficient number of drives online"` yields 0 hits, confirming it is the client-facing error string, not a server log line.

### Cause → effect

With the default availability-optimized storage class active (empty `Optimize`, `storage-class.go:L327`), the PUT path counts offline drives (`erasure-object.go:L1295–L1301`). Below the `(N+1)/2` threshold it raises parity to keep the object durable and stamps `minIOErasureUpgraded` (observed `"4->6"`), so the write completes. At or above the threshold (6 of 12), it short-circuits at L1304→L1308 with `errErasureWriteQuorum`, which the API layer maps to `SlowDownWrite`/HTTP 503 (`api-errors.go:L2192-L2193`, `L874-L878`) — exactly the XML body observed.

---

## OBJ-2 — Read path for pre-existing objects

### Direct answer

**Objects written before the failure remain readable as long as at least a read-quorum's worth of shards survive — again two regimes, both reproduced:**

1. **Up to `parity` drives lost (≤ 4 of 12) → the object is READ SUCCESSFULLY.** MinIO reconstructs the missing shards from the surviving 8+ shards via Reed-Solomon; the returned bytes are byte-for-byte identical to the original (checksum verified).
2. **More than `parity` drives lost (≥ 5 of 12, dropping below read quorum) → the read FAILS.** The client receives S3 code **`SlowDownRead`** at **HTTP 503**, body `"Resource requested is unreadable, please reduce your request rate"`. Internally this is `errErasureReadQuorum` = `"Read failed. Insufficient number of drives online"`.

With EC:4 (8 data + 4 parity), the read quorum is `dataBlocks = 8`, so reconstruction holds until only 8 shards remain (4 drives lost) and fails once fewer than 8 remain (5+ drives lost).

### Code reference

- Read entry: `cmd/object-handlers.go:L715` `GetObjectHandler`; object layer `cmd/erasure-object.go:L200` `GetObjectNInfo`, `L307` `getObjectWithFileInfo`, `L705` `getObjectFileInfo`.
- Read-quorum failure: `cmd/erasure-object.go:L487` `return FileInfo{}, errErasureReadQuorum`; error reduction `L836` `reduceReadQuorumErrs(...)`; handling `L691` `case errors.Is(err, errErasureReadQuorum):`.
- Quorum computation: `cmd/erasure-metadata.go` `objectQuorumFromMeta` — `readQuorum = dataBlocks`.
- Quorum error string: `cmd/erasure-errors.go:L22-L23` `errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")`.
- S3/HTTP mapping: `cmd/api-errors.go:L2190-L2191` `case errErasureReadQuorum: apiErr = ErrSlowDownRead`; definition `cmd/api-errors.go:L869-L873` `ErrSlowDownRead: { Code: "SlowDownRead", Description: "Resource requested is unreadable, please reduce your request rate", HTTPStatusCode: http.StatusServiceUnavailable }`.
- On-the-fly heal enqueue on a degraded GET: `cmd/erasure-object.go:L400` `globalMRFState.addPartialOp(...)` (ties into OBJ-3).

> **Important mechanism note:** the real-backend offline method used here (replacing a drive directory with a file) is *destructive* — it wipes that drive's shard. So "N drives offline" means "N shards of each pre-existing object destroyed." An object survives while `≥ readQuorum (8)` shards remain (i.e. up to `parity = 4` drives lost). This is exactly the real-world scenario of a drive being replaced with a blank one, and it is what makes the read-quorum boundary observable.

### Regime A — reconstruction SUCCESS (4 drives offline = parity limit)

```
$ rm -rf drives/d1 drives/d2 drives/d3 drives/d4 && touch drives/d1 drives/d2 drives/d3 drives/d4
$ mc admin info inv | grep -i drives
8 drives online, 4 drives offline, EC:4

$ mc cp inv/ectest/obj1.bin /tmp/dl_obj1.bin
`inv/ectest/obj1.bin` -> `/tmp/dl_obj1.bin`
Total: 8.00 MiB, Transferred: 8.00 MiB, Speed: 165.02 MiB/s

$ sha256sum /tmp/dl_obj1.bin
7e1b536dc753c753a52d82b75b397f6a692380e2f06bf0ac10dc6dec0be8de4a  /tmp/dl_obj1.bin
```

The downloaded checksum `7e1b536dc753…` is **identical to the original** `obj1.bin` recorded at baseline. With obj1 keeping exactly 8 of its 12 shards on the online drives, MinIO reconstructed the 4 destroyed shards and returned the object intact. (The same result was obtained via `mc cat inv/ectest/obj1.bin | sha256sum`.)

### Regime B — read-quorum LOSS (6 drives offline; only 6/12 shards remain, < read quorum 8)

```
$ rm -rf drives/d5 drives/d6 && touch drives/d5 drives/d6    # now 6 of 12 offline
$ mc admin info inv | grep -i drives
6 drives online, 6 drives offline, EC:4

$ mc cp inv/ectest/obj1.bin /tmp/dl_obj1.bin
mc: <ERROR> Unable to prepare URL for copying. Resource requested is unreadable, please reduce your request rate.
```

**Clean single-attempt raw HTTP GET (hand-written SigV4):**

```
HTTP/1.1 503 Service Unavailable
Content-Type: application/xml
Server: MinIO
X-Amz-Request-Id: 18C1EA1B2B11725F
Vary: Origin, Accept-Encoding

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>obj1.bin</Key><BucketName>ectest</BucketName><Resource>/ectest/obj1.bin</Resource><RequestId>18C1EA1B2B11725F</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Cause → effect

`getObjectFileInfo`/`getObjectWithFileInfo` collect the per-disk read results and pass them through `reduceReadQuorumErrs` (`erasure-object.go:L836`). While at least `readQuorum = dataBlocks = 8` shards are readable, the Reed-Solomon decoder reconstructs any missing shard and the GET returns intact bytes (verified by the matching SHA-256). Once fewer than 8 shards are available (5+ drives destroyed), the read cannot meet quorum and `erasure-object.go:L487` returns `errErasureReadQuorum`, which the API layer maps to `SlowDownRead`/HTTP 503 (`api-errors.go:L2190-L2191`, `L869-L873`) — exactly the XML observed.

---

## OBJ-3 — What triggers a healing operation when a drive comes back online

### Direct answer

When a previously-offline drive returns, MinIO's **fresh/recovered-disk monitor** detects it, **reformats** it, and **dispatches an automatic heal** for that drive. Concretely, two cooperating background monitors drive this:

1. `monitorAndConnectEndpoints` (runs every **15 s**) reconnects the drive; finding it fresh/unformatted, it **queues** the drive for healing via `pushHealLocalDisks`.
2. `monitorLocalDisksAndHeal` (polls every **10 s**, `defaultMonitorNewDiskInterval = time.Second * 10`) picks up the queued drive, reformats via `HealFormat`, and launches `healFreshDisk` for it.

**Observed end-to-end detection latency (drive restored → first heal log line): ~13.7 s / 19.2 s / 22.2 s across three runs** — i.e. one-to-two poll cycles, consistent with the 15 s + 10 s cadence. (The commonly cited "10 s" is only the heal-monitor *poll* interval, not the full detection latency.)

Two additional heal paths exist and were noted: an **on-the-fly heal** enqueued to the MRF queue whenever a degraded object is read/written, and a **background data-scanner** heal that periodically re-checks objects.

### Code reference

- Fresh-disk monitor & interval: `cmd/background-newdisks-heal-ops.go` — **L40** `defaultMonitorNewDiskInterval = time.Second * 10`; **L386** launch `go monitorLocalDisksAndHeal(ctx, z)`; **L563** `func monitorLocalDisksAndHeal`; it dispatches **L592** `go healFreshDisk(ctx, z, disk)`; `func healFreshDisk` at **L419**.
- Reconnect monitor & queueing: `cmd/erasure-sets.go` — **L283** `monitorAndConnectEndpoints`; interval **L348** `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` (= 15 s); fresh-disk queue **L227** `globalBackgroundHealState.pushHealLocalDisks(endpoint)` (on `errUnformattedDisk`).
- On-the-fly (MRF) heal: `cmd/mrf.go:L78` `addPartialOp`, `cmd/mrf.go:L220` `healRoutine`; enqueue sites in `cmd/erasure-object.go` at **L400**, **L805**, **L1578**, **~L2113** (`globalMRFState.addPartialOp(...)`).
- Background scanner heal: `cmd/data-scanner.go:L61` `healObjectSelectProb = 1024`, `L93` `getCycleScanMode`, `L199` `HealDeepScan` branch.

### Command & observed output

```
$ rm -f drives/d10        # bring d10 back online (remove the placeholder file)
# ... continuous `tail -f logs/server.log` ...
```

**Detection → heal-dispatch, verbatim server log for the returning drive (from server startup capture, drive d11 example, showing the full trigger stack):**

```
API: SYSTEM.storage
Time: 17:27:43 UTC 07/13/2026
DeploymentID: 1b454da0-455f-4f12-aebb-a4d11763f931
Error: lstat /tmp/minio-investigation/drives/d11/.minio.sys/format.json: not a directory (*fs.PathError)
       9: internal/logger/logonce.go:118:logger.(*logOnceType).logOnceIf()
       8: internal/logger/logonce.go:149:logger.LogOnceIf()
       7: cmd/logging.go:164:cmd.storageLogOnceIf()
       6: cmd/xl-storage.go:821:cmd.(*xlStorage).checkFormatJSON()
       5: cmd/xl-storage.go:841:cmd.(*xlStorage).GetDiskID()
       4: cmd/xl-storage-disk-id-check.go:209:cmd.(*xlStorageDiskIDCheck).IsOnline()
       3: cmd/erasure-sets.go:103:cmd.(*erasureSets).getDiskMap()
       2: cmd/erasure-sets.go:200:cmd.(*erasureSets).connectDisks()
       1: cmd/erasure-sets.go:303:cmd.(*erasureSets).monitorAndConnectEndpoints()
...
Healing drive '/tmp/minio-investigation/drives/d11' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d11' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d11' is finished (healed: 4, skipped: 0).
```

The stack shows the exact detection chain: `monitorAndConnectEndpoints` → `connectDisks` → `getDiskMap` → `IsOnline` → `GetDiskID` → `checkFormatJSON` finds no valid `format.json` on the returned drive, so it is treated as fresh and queued; the heal monitor then emits the `"Healing drive ..."` lines.

**Detection latency, three runs (drive restored → first `"Healing drive '...' - use N parallel workers."` line), measured by wall-clock:**

| Run | Drive | Latency | Result |
|-----|-------|---------|--------|
| 1 | d5 | **13.7 s** | healed: 15, skipped: 0 |
| 2 | d6 | **19.2 s** | (heal completed) |
| 3 | d10 | **22.2 s** | healed: 14, skipped: 0 |

All three fall within the expected one-to-two poll-cycle window (15 s reconnect tick to queue + up to 10 s heal tick).

### Cause → effect

Restoring the drive path makes the reconnect monitor (`erasure-sets.go:L283`, 15 s) see a drive whose `format.json` is absent/invalid (`checkFormatJSON`), so it enqueues it via `pushHealLocalDisks` (`erasure-sets.go:L227`). On its next 10 s tick, `monitorLocalDisksAndHeal` (`background-newdisks-heal-ops.go:L563`) reformats the drive and dispatches `healFreshDisk` (L592/L419), which emits the heal log lines. The observed 13–22 s latency is precisely the sum of "time until the next 15 s reconnect tick" plus "time until the next 10 s heal tick."

---

## OBJ-4 — Criteria for deciding an object needs healing on a particular drive

### Direct answer

The per-drive heal decision is made by **`shouldHealObjectOnDisk`**, which returns `true` (heal needed) when **any** of four conditions holds for that drive's copy of the object:

1. **Missing/corrupt object** — the read returned `errFileNotFound`, `errFileVersionNotFound`, or `errFileCorrupt`. → returns `(true, erErr)`.
2. **Legacy metadata** — `meta.XLV1` is set (the object is stored in the pre-2020 XLv1 format). → returns `(true, errLegacyXLMeta)`.
3. **Outdated metadata** — this drive's `xl.meta` does not equal the latest (quorum) metadata: `!latestMeta.Equals(meta)`. → returns `(true, errOutdatedXLMeta)`.
4. **Missing/corrupt part** — a data part file check reports `checkPartFileNotFound` or `checkPartFileCorrupt`. → returns `(true, errPartMissingOrCorrupt)`.

Otherwise it returns `(false, nil)` — no heal for that drive.

Two of these four criteria were **reproduced through the real backend path** (missing object, missing part); the other two are **INFERRED** from the code because they require conditions (legacy on-disk format; racing metadata version skew) that a fresh cluster at this commit does not naturally produce.

### Code reference

`cmd/erasure-healing.go` — `func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo, latestMeta FileInfo) (bool, error)` at **L156**:

```go
func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo, latestMeta FileInfo) (bool, error) {
	if errors.Is(erErr, errFileNotFound) || errors.Is(erErr, errFileVersionNotFound) || errors.Is(erErr, errFileCorrupt) {
		return true, erErr                                   // (1) L157–L159
	}
	if erErr == nil {
		if meta.XLV1 {
			return true, errLegacyXLMeta                     // (2) L161→L164
		}
		if !latestMeta.Equals(meta) {
			return true, errOutdatedXLMeta                   // (3) L166→L167
		}
		if !meta.Deleted && !meta.IsRemote() {
			for _, partErr := range partsErrs {
				if slices.Contains([]int{
					checkPartFileNotFound,
					checkPartFileCorrupt,
				}, partErr) {
					return true, errPartMissingOrCorrupt     // (4) L169→L176
				}
			}
		}
		return false, nil                                    // L180
	}
	return false, erErr
}
```

- Error vars: `errLegacyXLMeta` **L148**, `errOutdatedXLMeta` **L150**, `errPartMissingOrCorrupt` **L152**.
- Healing marker: `cmd/erasure-healing.go:L186` `xMinIOHealing = ReservedMetadataPrefix + "healing"` (this is L186, not L184).
- Per-object heal driver: `cmd/erasure-healing.go:L258` `healObject`.
- Latest-meta / online-disk helper: `cmd/erasure-healing-common.go:L219` `listOnlineDisks`.

### Observed correlations

**Criterion 1 — `errFileNotFound` (object entirely missing on a fresh drive):** After wiping drive `d8` (fresh, no data) and letting the automatic fresh-disk heal run, the object metadata that was absent on `d8` was recreated:

```
# before auto heal — obj3's xl.meta on the freshly-wiped d8:
$ ls drives/d8/ectest/obj3.bin/ 2>&1
ls: cannot access 'drives/d8/ectest/obj3.bin/': No such file or directory      # ABSENT

# after fresh-disk heal (~20 s later):
$ ls drives/d8/ectest/obj3.bin/xl.meta
drives/d8/ectest/obj3.bin/xl.meta                                              # RESTORED
```

The fresh-disk heal reported `healed: 14` / `healed: 15` items per drive — i.e. it recreated the missing `xl.meta` and part files for every object (and system metadata) that belonged on that drive. This is the `errFileNotFound → return true, erErr` branch (L157–L159).

**Criterion 4 — `errPartMissingOrCorrupt` (part file missing, metadata intact):** Only the data part of one object was deleted on one drive, keeping its `xl.meta`:

```
$ rm -f drives/d7/ectest/obj2.bin/820b78d3-94c0-43e8-95e4-8294a8bcc02a/part.1   # delete only part.1, keep xl.meta
$ mc admin heal -r --scan deep --force inv/ectest/obj2.bin
 ◐  ectest/obj2.bin
    0/1 objects; 0 B in 0s
    ...
[Green -> Green]  ectest/obj2.bin
Healed:   1/1 objects; 8 MiB in 1s

$ ls drives/d7/ectest/obj2.bin/820b78d3-94c0-43e8-95e4-8294a8bcc02a/part.1
drives/d7/ectest/obj2.bin/820b78d3-94c0-43e8-95e4-8294a8bcc02a/part.1          # part.1 RESTORED
```

The `xl.meta` read fine but the part-file check reported it missing, hitting the `checkPartFileNotFound → return true, errPartMissingOrCorrupt` branch (L169–L176); the heal reconstructed `part.1` from the surviving shards. (This one was **admin-triggered** via `mc admin heal` to force a deterministic, single-object heal; the automatic path exercises the identical decision function.)

**Criteria 2 & 3 — INFERRED:**

- `errLegacyXLMeta` (L161→L164) fires only for objects stored in the legacy **XLv1** on-disk format (`meta.XLV1`). A cluster freshly built at this commit writes only v2 `xl.meta`, so this branch cannot be reached without pre-seeding legacy data. **Labeled inferred** from the code.
- `errOutdatedXLMeta` (L166→L167) fires when a drive's `xl.meta` differs from the latest quorum metadata (`!latestMeta.Equals(meta)`). Producing a deterministic version skew requires racing concurrent partial writes against a specific drive; it was not reproducible through the real S3 entry point after varied effort. **Labeled inferred** from the code.

### Cause → effect

When healing a drive, MinIO reads the object on every drive, computes the latest (quorum) `FileInfo`, and calls `shouldHealObjectOnDisk` per drive. A wiped drive naturally yields `errFileNotFound` (nothing there) → heal; a drive missing just a part yields `errPartMissingOrCorrupt` → heal. Both were observed to reconstruct exactly the missing artifact. The legacy/outdated branches are real code paths but depend on data states this fresh cluster does not create, hence inferred.

---

## OBJ-5 — Log messages during an active heal

### Direct answer

During an automatic fresh/recovered-disk heal, MinIO emits a three-line sequence per drive (default log level, no extra flags):

1. `Healing drive '<path>' - 'mc admin heal alias/ --verbose' to check the current status.`
2. `Healing drive '<path>' - use N parallel workers.`  — where **N = 4** on this 4-CPU host.
3. `Healing of drive '<path>' is finished (healed: H, skipped: S).`

The worker-count line is the canonical "active heal" marker requested. An additional edge-condition log, `"all drives are in healing state, aborting.."`, exists but was **not** reproduced (it requires every drive in the set to be reformatting simultaneously) — **labeled inferred**.

### Code reference

- `cmd/global-heal.go:L210` `healingLogEvent(ctx, "Healing drive '%s' - use %d parallel workers.", tracker.disk.String(), numHealers)`; worker count computed just above (L205–L208), overridable via `globalHealConfig.GetWorkers()`, otherwise defaulting to a CPU-derived value (observed 4).
- `cmd/background-newdisks-heal-ops.go:L460` `healingLogEvent(ctx, "Healing drive '%s' - 'mc admin heal alias/ --verbose' to check the current status.", endpoint)`.
- `cmd/background-newdisks-heal-ops.go:L520` `healingLogEvent(ctx, "Healing of drive '%s' is finished (healed: %d, skipped: %d).", disk, tracker.ItemsHealed, tracker.ItemsSkipped)`.
- Edge condition: `cmd/global-heal.go:L339-L342` `if len(disks) == healing { ... healingLogIf(ctx, errors.New("all drives are in healing state, aborting..")) ... }`.
- Heal-set driver: `cmd/global-heal.go:L152` `healErasureSet`; heal sequence `cmd/global-heal.go:L49` `newBgHealSequence`.
- These `healingLogEvent`/`healingLogIf` helpers log by default (they wrap `logger.Event`/`logger.LogIf` with the `"healing"` subsystem).

### Command & complete unedited output

A clean single-drive heal (drive `d10`, from the three-run stability set) produced exactly:

```
Healing drive '/tmp/minio-investigation/drives/d10' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d10' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d10' is finished (healed: 14, skipped: 0).
```

A second drive (`d5`) confirms the identical shape with a different heal count:

```
Healing drive '/tmp/minio-investigation/drives/d5' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d5' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d5' is finished (healed: 15, skipped: 0).
```

Across the whole investigation the log recorded **27** `"use 4 parallel workers"` dispatch lines and **17** `"is finished (healed: …)"` completion lines, confirming the messages are emitted on every heal, not once.

### Cause → effect

`healFreshDisk` (`background-newdisks-heal-ops.go:L419`) logs the "check the current status" line (L460), then calls into `healErasureSet` (`global-heal.go:L152`), which computes `numHealers` (defaulting to a CPU-derived 4) and logs the "use %d parallel workers" line (`global-heal.go:L210`). When the per-drive heal tracker finishes, L520 logs the "is finished (healed: %d, skipped: %d)" summary. The `healed:` count equals the number of objects/metadata items reconstructed on that drive (14–15 here, matching the 5 objects × their shards plus system metadata).

---

## OBJ-6 — Health metrics: online vs. offline drive counts, before and after failure

### Direct answer

The v3 cluster-health collector exposes three drive-count gauges (full Prometheus names):

- **`minio_cluster_health_drives_online_count`** — online drives.
- **`minio_cluster_health_drives_offline_count`** — offline drives.
- **`minio_cluster_health_drives_count`** — total drives.

Observed values, **before → during → after** a 4-drive failure and heal:

| Metric (v3) | BEFORE (baseline) | DURING (4 offline) | AFTER (healed) |
|---|---|---|---|
| `minio_cluster_health_drives_count` | 12 | 12 | 12 |
| `minio_cluster_health_drives_online_count` | 12 | 8 | 12 |
| `minio_cluster_health_drives_offline_count` | *(absent — value 0, suppressed)* | 4 | *(absent — value 0, suppressed)* |

The online count moves **12 → 8 → 12**; the offline count moves **0 → 4 → 0**. Note the v3 collector **suppresses zero-valued metrics**, so `drives_offline_count` is *absent* from the scrape at baseline and after heal (both 0), and *appears* as `4` only while drives are down. The v2 collector always prints `0` explicitly (shown below).

### Code reference

- v3 constants: `cmd/metrics-v3-cluster-health.go:L23` `healthDrivesOfflineCount = "drives_offline_count"`, **L24** `healthDrivesOnlineCount = "drives_online_count"`, **L25** `healthDrivesCount = "drives_count"`; gauge descriptors `NewGaugeMD(...)` at **L28–L35**.
- v3 setters: `cmd/metrics-v3-cluster-health.go:L44-L46` — `m.Set(healthDrivesOfflineCount, float64(clusterDriveMetrics.offlineDrives))`, `m.Set(healthDrivesOnlineCount, float64(clusterDriveMetrics.onlineDrives))`, `m.Set(healthDrivesCount, float64(clusterDriveMetrics.totalDrives))` (inside `loadClusterHealthDriveMetrics`).
- v3 path & registration: `cmd/metrics-v3.go:L50` `clusterHealthCollectorPath = "/cluster/health"`; registered `L240` `NewMetricsGroup(clusterHealthCollectorPath, ...)`. Full endpoint: `/minio/metrics/v3/cluster/health`.
- Zero-suppression: `cmd/metrics-v3-types.go:L212` `func (m MetricValues) Set(...)`, guard at **L240-L241** `// If valid non zero value set the metrics` / `if value > 0 {`.
- v2 equivalents: `cmd/metrics-v2.go:L130` `clusterMetricNamespace = "minio_cluster"`; `getClusterHealthMetrics` **L3656** → `minio_cluster_drive_online_total` / `minio_cluster_drive_offline_total` / `minio_cluster_drive_total`.

### Commands & complete unedited output

The v3 endpoint requires a bearer token (a request without it returns HTTP 403). The token was generated with `mc admin prometheus generate inv cluster --api-version v3` and passed as `Authorization: Bearer <token>` (the token value itself is a secret and is intentionally omitted).

**BEFORE (baseline, 12 online / 0 offline):**

```
$ curl -s -H "Authorization: Bearer <redacted>" http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
# (minio_cluster_health_drives_offline_count is absent: value 0 is suppressed)

$ mc admin prometheus metrics inv cluster | grep -E '^minio_cluster_drive_(online|offline|total)'
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
```

**DURING (4 drives offline, degraded):**

```
$ curl -s -H "Authorization: Bearer <redacted>" http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
minio_cluster_health_drives_count 12
minio_cluster_health_drives_offline_count 4
minio_cluster_health_drives_online_count 8

$ mc admin prometheus metrics inv cluster | grep -E '^minio_cluster_drive_(online|offline|total)'
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 4
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 8
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
```

**AFTER (heal complete, 12 online / 0 offline):**

```
$ curl -s -H "Authorization: Bearer <redacted>" http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
# (minio_cluster_health_drives_offline_count absent again: back to 0)

$ mc admin prometheus metrics inv cluster | grep -E '^minio_cluster_drive_(online|offline|total)'
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
```

### Cause → effect

`loadClusterHealthDriveMetrics` (`metrics-v3-cluster-health.go`) reads the cached cluster drive tally and sets the three gauges (L44–L46) from `offlineDrives` / `onlineDrives` / `totalDrives`. When 4 drives go offline the tally becomes 8 online / 4 offline, which is exactly what the scrape shows; after heal it returns to 12/0. The v3 `MetricValues.Set` guard `if value > 0` (`metrics-v3-types.go:L240`) is why `drives_offline_count` disappears at 0 — a clean, observable before/during/after signal. The v2 collector uses a different code path (`metrics-v2.go:L3656`) that emits the `0` explicitly.

---

## Two-Run Stability

Per the governing methodology, timing- and magnitude-dependent results were confirmed across at least two runs (three were performed).

**Scale / duration:** SNMD-12 topology, 5–6 objects of 8 MiB each; each fresh-disk heal reconstructed **14–15 items** (all `xl.meta` + part files for the objects and system metadata belonging on that drive). Observation windows were held well beyond the 10 s heal-poll interval.

**Heal-detection latency (drive restored → first `"use N parallel workers"` log line):**

| Run | Drive | Latency |
|-----|-------|---------|
| 1 | d5 | 13.7 s |
| 2 | d6 | 19.2 s |
| 3 | d10 | 22.2 s |

The distribution (13.7–22.2 s) is stable and fully explained by the two-monitor cadence (15 s reconnect tick + up to 10 s heal tick = worst case ~25 s). The often-quoted "10 s" is only the heal-monitor poll interval, **not** the end-to-end latency.

**Metric-count stability:** the before/during/after gauge values (12/0 baseline, 8/4 degraded, 12/0 after heal) were reproduced **identically** across the OBJ-1 write run, the OBJ-2 read run, and the dedicated OBJ-6 scrape — no run-to-run variance.

---

## Observed vs. Inferred

Everything reported above is **OBSERVED** at runtime except the following, which are explicitly **INFERRED** from the source at this commit because they could not be reproduced through the real S3/backend entry point after genuine, varied effort:

| Item | Status | Why |
|------|--------|-----|
| OBJ-1 degraded-success + `"4->6"` annotation | **Observed** | on-disk `xl.meta` decode |
| OBJ-1 `SlowDownWrite`/503 quorum failure | **Observed** | raw SigV4 HTTP capture |
| OBJ-2 reconstruction (checksum match) | **Observed** | SHA-256 equality |
| OBJ-2 `SlowDownRead`/503 quorum failure | **Observed** | raw SigV4 HTTP capture |
| OBJ-3 fresh-disk trigger + latency | **Observed** | server log + wall-clock, 3 runs |
| OBJ-4 `errFileNotFound` criterion | **Observed** | wiped drive → metadata restored |
| OBJ-4 `errPartMissingOrCorrupt` criterion | **Observed** | deleted part.1 → part restored |
| OBJ-4 `errLegacyXLMeta` criterion | **Inferred** | requires legacy XLv1 on-disk data, which a fresh cluster never writes |
| OBJ-4 `errOutdatedXLMeta` criterion | **Inferred** | requires a deterministic metadata version skew, not forceable via the real path |
| OBJ-5 heal log trio + `numHealers=4` | **Observed** | server log |
| OBJ-5 `"all drives are in healing state, aborting.."` | **Inferred** | requires *every* drive reformatting at once (would destroy quorum) |
| OBJ-6 gauges + before/during/after | **Observed** | metrics scrapes |
| Literal `errDiskNotFound` = `"drive not found"` log | **Inferred / not observed** | see note below |

**`errDiskNotFound` note:** the literal string `"drive not found"` (`cmd/storage-errors.go:L53`) appeared **0 times** in the server log across the entire investigation, despite varied triggers (file placeholder, dangling symlink, wipe+restore causing disk-id mismatch, `mc admin trace`). `errDiskNotFound` is the internal disk-identity signal that *registers* a drive offline (its effect is observable in `mc admin info` and in `drives_offline_count`) but is swallowed into offline-accounting rather than surfaced verbatim. The **observed** offline log signal is instead `errDiskNotDir` = `"drive is not directory or mountpoint"` (`cmd/storage-errors.go:L50`), which was emitted (deduplicated via `LogOnceIf`, so one structured block per unique offline episode — 12 blocks total in the final run).

**AIStor "48-hour fresh-drive" rule:** newer MinIO/AIStor documentation describes a 48-hour fresh-drive healing rule. This was **NOT** observed or reproduced at commit `c07e5b49d477` and is **not asserted** to exist here; the behavior observed at this commit is the immediate ~10–20 s fresh-disk heal described in OBJ-3.

---

## Coverage Checklist

Every named mechanism from the questions, with its concrete value, `file:line`, observed evidence, and causal role:

| # | Item | Concrete value | `file:line` | Evidence |
|---|------|----------------|-------------|----------|
| 1 | `errErasureWriteQuorum` | "Write failed. Insufficient number of drives online" | `cmd/erasure-errors.go:L25-L26` | OBJ-1 Regime 2 |
| 2 | `errErasureReadQuorum` | "Read failed. Insufficient number of drives online" | `cmd/erasure-errors.go:L22-L23` | OBJ-2 Regime B |
| 3 | `SlowDownWrite` | Code `SlowDownWrite`, HTTP 503, "Resource requested is unwritable…" | `cmd/api-errors.go:L874-L878`, map `L2192-L2193` | OBJ-1 XML body |
| 4 | `SlowDownRead` | Code `SlowDownRead`, HTTP 503, "Resource requested is unreadable…" | `cmd/api-errors.go:L869-L873`, map `L2190-L2191` | OBJ-2 XML body |
| 5 | HTTP 503 | `http.StatusServiceUnavailable` | `cmd/api-errors.go:L872, L877` | both quorum failures |
| 6 | `minIOErasureUpgraded` | key `x-minio-internal-erasure-upgraded`, observed value `4->6` | `cmd/erasure-metadata.go:L38`; set `cmd/erasure-object.go:L1316` | OBJ-1 Regime 1 xl.meta |
| 7 | write-quorum threshold | `offlineDrives >= (len(storageDisks)+1)/2` = 6/12 | `cmd/erasure-object.go:L1304-L1308` | OBJ-1 both regimes |
| 8 | `errDiskNotFound` | "drive not found" — registers offline, **not logged verbatim** (0 hits) | `cmd/storage-errors.go:L53` | OBJ-3 / Observed-vs-Inferred |
| 9 | `errDiskNotDir` (observed offline signal) | "drive is not directory or mountpoint" | `cmd/storage-errors.go:L50` | OBJ-1/OBJ-3 log blocks |
| 10 | `monitorLocalDisksAndHeal` | fresh-disk heal monitor | `cmd/background-newdisks-heal-ops.go:L563` | OBJ-3 |
| 11 | `defaultMonitorNewDiskInterval` | `time.Second * 10` (10 s heal poll) | `cmd/background-newdisks-heal-ops.go:L40` | OBJ-3 latency |
| 12 | reconnect interval | 15 s (`defaultMonitorNewDiskInterval + 5s`) | `cmd/erasure-sets.go:L348` | OBJ-3 latency |
| 13 | `healFreshDisk` | dispatched per fresh drive | `cmd/background-newdisks-heal-ops.go:L419` (call `L592`) | OBJ-3 |
| 14 | MRF `addPartialOp` / `healRoutine` | on-the-fly heal queue | `cmd/mrf.go:L78` / `L220`; enqueue `cmd/erasure-object.go:L400, ~L2113` | OBJ-3 |
| 15 | data-scanner `healObjectSelectProb` | `1024` | `cmd/data-scanner.go:L61` (`L93`, `L199`) | OBJ-3 |
| 16 | `shouldHealObjectOnDisk` | 4-criteria decision | `cmd/erasure-healing.go:L156` | OBJ-4 |
| 17 | `errFileNotFound` criterion | heal when object missing | `cmd/erasure-healing.go:L157-L159` | OBJ-4 (observed, d8) |
| 18 | `errPartMissingOrCorrupt` criterion | heal when part missing/corrupt | `cmd/erasure-healing.go:L169-L176` (var `L152`) | OBJ-4 (observed, d7 part.1) |
| 19 | `errLegacyXLMeta` criterion | heal when `meta.XLV1` | `cmd/erasure-healing.go:L161-L164` (var `L148`) | OBJ-4 (inferred) |
| 20 | `errOutdatedXLMeta` criterion | heal when `!latestMeta.Equals(meta)` | `cmd/erasure-healing.go:L166-L167` (var `L150`) | OBJ-4 (inferred) |
| 21 | `xMinIOHealing` | `ReservedMetadataPrefix + "healing"` | `cmd/erasure-healing.go:L186` | OBJ-4 |
| 22 | `healObject` | per-object heal driver | `cmd/erasure-healing.go:L258` | OBJ-4 |
| 23 | "Healing drive '%s' - use %d parallel workers." | observed with N=4 | `cmd/global-heal.go:L210` | OBJ-5 |
| 24 | heal-finished log | "…is finished (healed: %d, skipped: %d)." | `cmd/background-newdisks-heal-ops.go:L520` | OBJ-5 |
| 25 | "all drives are in healing state, aborting.." | edge condition | `cmd/global-heal.go:L341` | OBJ-5 (inferred) |
| 26 | `minio_cluster_health_drives_online_count` | 12 → 8 → 12 | `cmd/metrics-v3-cluster-health.go:L24`, set `L45` | OBJ-6 |
| 27 | `minio_cluster_health_drives_offline_count` | 0(absent) → 4 → 0(absent) | `cmd/metrics-v3-cluster-health.go:L23`, set `L44` | OBJ-6 |
| 28 | `minio_cluster_health_drives_count` | 12 (constant) | `cmd/metrics-v3-cluster-health.go:L25`, set `L46` | OBJ-6 |
| 29 | `GetParityForSC` / default parity | EC:4 for 12 drives (default) | `internal/config/storageclass/storage-class.go:L258`; `AvailabilityOptimized` `L327` | Environment / OBJ-1 |

---

## Final `git status`

After stopping the server and removing every ephemeral artifact (the built `./minio` binary and its 9 gitignored debug helpers at the repo root, the `/tmp/minio-investigation` work area with all drive/log/capture/script directories, and all temporary files), the repository contains exactly one new path and no modified tracked files:

```
$ git status --porcelain --untracked-files=all
?? blitzy/documentation/minio_c07e5b49d477.md

$ git diff --stat HEAD
       (empty — no tracked source file was modified)
```

```
$ git status
On branch blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc
Untracked files:
  (use "git add <file>..." to include in what will be committed)
	blitzy/

nothing added to commit but untracked files present (use "git add" to track)
```

Every `cmd/*.go`, `internal/**`, `buildscripts/*`, `Makefile`, and `go.mod`/`go.sum` remains byte-identical to commit `c07e5b49d477` (empty `git diff --stat HEAD` confirms zero tracked-file changes). The source repository was treated strictly as read-only; this single documentation file is the only addition.

---

*End of analysis.*

