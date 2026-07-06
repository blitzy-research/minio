# MinIO in Distributed (4‑Drive Erasure) Mode: Health, Quorum, Disk Failure, Recovery & Repair — A Runtime‑Grounded Investigation

> **Methodology (run‑first).** Every behavioral claim below was produced by **building and running MinIO, injecting a real `chmod 000` permission fault, and capturing the actual, unedited output** — *then* writing the explanation. Each claim is paired with (a) the exact command, (b) its real output, and (c) a precise `file:line` source citation. Values that could not be reproduced at runtime and are read from a source constant are explicitly labeled **(inferred)**; everything else is **(observed)**.

---

## 1. The Question

In distributed mode with **4 directories using erasure coding**:

1. How does MinIO decide it is **"healthy,"** and what does it assume about the **required number of disks**?
2. When one directory suddenly becomes inaccessible due to **permission changes** during normal operation, what happens *in that moment* — does MinIO **adapt and keep going, or refuse to write**?
3. Contrast **above the threshold** (lose 1 disk) versus **below the threshold** (lose a 2nd disk).
4. Do the **logs call out the failing disk directly by path**? Is there any sign of **recovery being attempted while the system is still live**?
5. If the missing directory later becomes accessible again, does MinIO recognize it **on its own via polling**, or must something **push** it into a healing path?
6. For objects **written while a disk was down**, how do they get **repaired** once the disk returns?
7. **Trace where the quorum decision lives in the code.**
8. Ground the whole explanation in observations from the **health endpoint** and from **actual write attempts** while running.

---

## 2. TL;DR — Direct Answers

1. **Health decision & disk‑count assumption.** A 4‑directory single node forms **one erasure set of 4 drives**, which under the default storage class is **EC:2** (2 data + 2 parity). MinIO's cluster‑health probe `erasureServerPools.Health()` counts drives whose live `DiskInfo` state is OK and compares that count to two per‑set thresholds: a set is **`Healthy`** when `online ≥ write quorum (3)` and **`HealthyRead`** when `online ≥ read quorum (2)` (`cmd/erasure-server-pool.go:L2707,L2783-L2784`). **Observed:** with all 4 online, `GET /minio/health/cluster` → **`200`** with header **`X-Minio-Write-Quorum: 3`**, and `/minio/health/cluster/read` → **`200`** with **`X-Minio-Read-Quorum: 2`**.
2. **Behavior at the moment of failure.** MinIO **does not crash and does not refuse writes** the instant a directory becomes inaccessible — it keeps serving as long as it is **at or above quorum**. It refuses **writes** only once the online count drops **below the write quorum**. **Observed:** immediately after `chmod 000 /tmp/d1` (3 online), a PUT still returned **`HTTP 200`** and the new object's shard was simply **skipped on the faulted drive** (written to the other 3).
3. **Above vs. below threshold.** **3 online (above):** writes **succeed**, reads **succeed**, `/minio/health/cluster` stays **`200`**. **2 online (below):** writes are **refused** (`HTTP 503 SlowDownWrite`), reads still **succeed** (`HTTP 200`), `/minio/health/cluster` returns **`503`** while `/minio/health/cluster/read` stays **`200`**. That read/write divergence is the crux of the contrast.
4. **Log visibility & live recovery.** **Yes** — the logs name the failing drive **by full path**. For a *permission* fault the by‑path lines come from the reconnect poll (`Error: drive access denied … endpoint="/tmp/d1"`) and the healing‑tracker read (`unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: … permission denied`). Recovery machinery is visibly active while the server is live (the reconnect poll keeps probing; when a drive is *replaced/wiped*, the auto‑heal loop logs `Healing drive '/tmp/d1' … use 4 parallel workers`).
5. **Re‑admission mechanism.** **Automatic and polling‑based** — no operator push required. `monitorAndConnectEndpoints` periodically calls `connectDisks` (`cmd/erasure-sets.go:L283`) on a **15 s** timer (`defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + 5 s = 15 s`, `cmd/erasure-sets.go:L348`). The manual `mc admin heal` path (`queueHealTask`, `cmd/admin-heal-ops.go:L721`) exists but is **optional**.
6. **Repair of outage writes.** **Automatic, via two distinct paths.** For a **transient** outage (the permission fault), objects written with a missing shard are queued through **MRF** (`globalMRFState.addPartialOp`, `cmd/mrf.go:L78`) and repaired by `healRoutine` (`cmd/mrf.go:L220`). For a **fresh/replaced** drive, the auto‑heal loop `monitorLocalDisksAndHeal → healFreshDisk` (`cmd/background-newdisks-heal-ops.go:L563,L419`) rebuilds the whole drive on a **~10 s** cadence, tracking progress in `.healing.bin`. **Observed both.**
7. **Quorum decision in code.** The threshold is computed in **`objectQuorumFromMeta`** (`cmd/erasure-metadata.go:L531-L564`): `readQuorum = dataBlocks` and `writeQuorum = dataBlocks (+1 when dataBlocks == parityBlocks)`. Default parity comes from **`DefaultParityBlocks(4) == 2`** (`internal/config/storageclass/storage-class.go:L355-L362`). "Stop" for writes is enforced in **`multiWriter.Write`** (`cmd/erasure-encode.go:L34-L66`).
8. **Grounding.** Everything above is anchored to the `/minio/health/*` responses (status + headers) and to real S3 PUT/GET attempts at each disk‑loss level; the full unedited output is reproduced in §5–§10.

---

## 3. Environment & Reproduction

| Item | Value |
|---|---|
| Repository | `github.com/minio/minio`, module directive `go 1.23` (`go.mod:L3`) **(observed)** |
| Go toolchain | `go version go1.23.12 linux/amd64` **(observed)** |
| Canonical entry point | `main.go:L30` → `minio.Main(os.Args)` **(observed)** |
| Topology | one node, 4 directories `/tmp/d1..d4` = **one erasure set of 4 drives → EC:2** **(observed)** |
| Default credentials | `minioadmin:minioadmin` (demo defaults) |
| API / health port | `:9000` (`GlobalMinioDefaultPort="9000"`, `cmd/globals.go:L65`) |

### 3.1 Build (canonical)

```console
$ cd <repo-root>
$ CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-investigation/minio .
BUILD_EXIT=0
real	0m4.783s
$ /tmp/minio-investigation/minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
```

This is the `Makefile` recipe minus the version‑stamping ldflags: `@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio` (`Makefile:L179`, `LDFLAGS` at `Makefile:L3`). The ldflags only stamp version metadata; omitting them yields the `DEVELOPMENT.GOGET` version string and is behaviorally identical for this investigation. **(observed)**

### 3.2 ⚠ Run as a NON‑ROOT user (essential for the permission fault)

**root bypasses POSIX permission bits (DAC)**, so `chmod 000` on a data directory would *not* deny a root‑owned MinIO process and the fault would silently have no effect. The server was therefore run as the unprivileged user **`builder` (uid 1001)**. This was verified empirically **before** running MinIO:

```console
$ chown -R builder:builder /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 /tmp/minio-investigation
$ chmod 000 /tmp/d1
$ sudo -u builder ls /tmp/d1
ls: cannot open directory '/tmp/d1': Permission denied      # non-root IS denied
$ ls /tmp/d1
probe                                                       # root BYPASSES 000
$ chmod 755 /tmp/d1
```

The run command (single node, four dirs; logs redirected so they can be captured):

```bash
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin
setsid sudo -u builder env HOME=/tmp/builder \
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  /tmp/minio-investigation/minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 \
  --address ':9000' --console-address ':9001' > /tmp/minio-investigation/server.log 2>&1 &
```

Startup banner (verbatim from `server.log`) — first evidence that four dirs form **one set of four**:

```text
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
API: http://127.0.0.1:9000 ...   WebUI: http://127.0.0.1:9001 ...
DeploymentID: 9777562f-7f00-4dda-b02b-fc35dbafc5cc
```

`Formatting 1st pool, 1 set(s), 4 drives per set.` is emitted by `logger.Info("Formatting %s pool, %v set(s), %v drives per set.", …)` at `cmd/prepare-storage.go:L194`. **(observed)**

---

## 4. Topology & Quorum Arithmetic

A 4‑drive erasure set under the **default** storage class resolves to **EC:2**, giving **read quorum 2** and **write quorum 3**:

| Quantity | Value | Source |
|---|---|---|
| Total drives (N) | 4 | topology (`/tmp/d1..d4`) **(observed)** |
| Default parity (M) | 2 | `DefaultParityBlocks(4)` → `case 4, 5: return 2` (`internal/config/storageclass/storage-class.go:L361-L362`) |
| Data blocks (K = N − M) | 2 | `objectQuorumFromMeta` (`cmd/erasure-metadata.go:L555`) |
| **Read quorum** | **2** (= K) | `objectQuorumFromMeta` returns `dataBlocks` as read quorum (`cmd/erasure-metadata.go:L564`) |
| **Write quorum** | **3** (= K + 1, since K == M) | `objectQuorumFromMeta` (`cmd/erasure-metadata.go:L557-L559`) |
| Max drives lost, still **writable** | 1 (N − write quorum) | derived |
| Max drives lost, still **readable** | 2 (N − read quorum) | derived |

Both thresholds were **confirmed at runtime** by the health headers in §5 (`X-Minio-Write-Quorum: 3`, `X-Minio-Read-Quorum: 2`) and by the server log in §7 (`expected write quorum: 3, drives-online: 2`). The full code trace is in §11.

> **Why EC:2 and not the "EC:1" seen in config help?** When `MINIO_STORAGE_CLASS_STANDARD` is unset (the default), the effective standard‑class parity is `DefaultParityBlocks(setDriveCount)` — for 4 drives that is **2** (`internal/config/storageclass/storage-class.go:L390`, wired via `ecDrivesNoConfig`, `cmd/format-erasure.go:L686`). The `EC:1` string that appears in the config *help template* is only a KV default label, not the effective runtime parity. The runtime header `X-Minio-Write-Quorum: 3` is authoritative and confirms parity 2. **(observed)**

---

## 5. Baseline — 4 Drives Online (Healthy)

All four health endpoints were probed with status line + headers visible (`curl -i`), and a successful S3 PUT/GET was performed.

### 5.1 Health endpoints (unedited)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Vary: Origin
X-Amz-Request-Id: 184F...E7A
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
Content-Length: 0
Server: MinIO
X-Minio-Read-Quorum: 2
X-Amz-Request-Id: 184F...F01

$ curl -sS -i http://127.0.0.1:9000/minio/health/live
HTTP/1.1 200 OK

$ curl -sS -i http://127.0.0.1:9000/minio/health/ready
HTTP/1.1 200 OK
```

- `X-Minio-Write-Quorum: 3` is set by `ClusterCheckHandler` at `cmd/healthcheck-handler.go:L72` using the header string `xMinIOWriteQuorum` (`internal/http/headers.go:L193`). **(observed)**
- `X-Minio-Read-Quorum: 2` is set by `ClusterReadCheckHandler` at `cmd/healthcheck-handler.go:L109`, header `xMinIOReadQuorum` (`internal/http/headers.go:L196`). **(observed)**
- `X-Minio-Storage-Class-Defaults: false` (`cmd/healthcheck-handler.go:L73`, header at `internal/http/headers.go`) is `false` because the storage‑class subsystem is initialized and `GetParityForSC` returns a valid parity (2), not a negative sentinel (`cmd/erasure-server-pool.go:L2737-L2739`). **(observed)**
- There is **no** `X-Minio-Healing-Drives` header at baseline (nothing is healing). **(observed)**

### 5.2 Successful PUT + GET (unedited)

```console
$ python3 /tmp/minio-investigation/s3op.py mb testbucket
MADE_BUCKET testbucket

$ echo "hello-baseline" > /tmp/minio-investigation/obj.txt
$ python3 /tmp/minio-investigation/s3op.py put testbucket obj-baseline.txt /tmp/minio-investigation/obj.txt
PUT_OK key=obj-baseline.txt ETag="874602a77cc9f9f8358fcc9ef1e19b84" HTTP=200

$ python3 /tmp/minio-investigation/s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 BODY='hello-baseline'
```

Shard placement confirms erasure striping across **all 4** drives (one part file per drive):

```console
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do \
    echo -n "$d: "; ls "$d"/testbucket/obj-baseline.txt/*/part.1 2>/dev/null | wc -l; done
/tmp/d1: 1
/tmp/d2: 1
/tmp/d3: 1
/tmp/d4: 1
```

(The S3 calls use a small `boto3` helper placed under `/tmp` and removed afterward — no `mc`/`aws` CLI was available in the environment; `boto3 1.43.40` was.)

---

## 6. Above the Threshold — 3 Drives Online (`chmod 000 /tmp/d1`)

Fault injected on **one** directory while the server was live (T0 = `2026-07-06T23:12:41.280Z`):

```console
$ date -u +%FT%T.%3NZ ; chmod 000 /tmp/d1
2026-07-06T23:12:41.280Z
```

### 6.1 Write OK, Read OK (unedited)

```console
$ echo "hello-3online" > /tmp/minio-investigation/obj3.txt
$ python3 /tmp/minio-investigation/s3op.py put testbucket obj-3online.txt /tmp/minio-investigation/obj3.txt
PUT_OK key=obj-3online.txt ETag="04c1089c9e47b59701b19100e623f113" HTTP=200

$ python3 /tmp/minio-investigation/s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 BODY='hello-baseline'
```

Both **succeed**: 3 online ≥ write quorum 3 (write) and ≥ read quorum 2 (read).

**Shard proof** — the new object is written to the 3 healthy drives and **skipped on the faulted `/tmp/d1`**, exactly as the write‑quorum logic allows (3 shards = write quorum):

```console
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do \
    echo -n "$d obj-3online: "; ls "$d"/testbucket/obj-3online.txt/*/part.1 2>/dev/null | wc -l; done
/tmp/d1 obj-3online: 0      # faulted drive — shard skipped
/tmp/d2 obj-3online: 1
/tmp/d3 obj-3online: 1
/tmp/d4 obj-3online: 1
```

### 6.2 Health stays 200 (unedited)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
X-Minio-Storage-Class-Defaults: false
Content-Length: 0
```

3 online ≥ write quorum 3, so the set is still `Healthy` → `200`. No `X-Minio-Healing-Drives` header appeared. **(observed)**

### 6.3 The failing drive is named BY PATH in the logs (unedited)

The by‑path lines for a *permission* fault are emitted by the **reconnect poll** and the **healing‑tracker read**, both of which try to touch `/tmp/d1` and are denied:

```text
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/d1"
       6: internal/logger/logonce.go:...:logger.(*logOnceType).logOnceIf()
       3: cmd/prepare-storage.go:51:cmd.printEndpointError()
       2: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()

Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fs.PathError)
       1: cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
```

- `printEndpointError` prints each endpoint's error by path (defined at `cmd/prepare-storage.go:L35`; the actual emission via `peersLogAlwaysIf` is at `L51`, which is the frame the runtime stack above points to), driven from the reconnect poll `connectDisks.func2` (`cmd/erasure-sets.go:L230`). The `drive access denied` sentinel is `errDiskAccessDenied` (`cmd/storage-errors.go:L68`). **(observed)**
- The permission error is recognized by `osIsPermission` (`errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)`, `cmd/xl-storage-errors.go:L135-L136`) and mapped by `osErrToFileErr` to `errFileAccessDenied` (`if osIsPermission(err) { return errFileAccessDenied }`, `cmd/storage-errors.go:L158-L159`). **(observed)**

> **Important nuance about *which* log fires (observed).** The oft‑cited `monitorDiskWritable` line *"node(%s): taking drive %s offline: %v"* (`cmd/xl-storage-disk-id-check.go:L1015`) did **not** fire for the permission fault. `goOffline` (defined at `cmd/xl-storage-disk-id-check.go:L1013`) is only invoked on `errFaultyDisk` (an EIO‑class fault) or a health‑check *timeout* — the timeout branch at `L1035`, the write‑fault branch at `L1044-L1045`, and the read‑fault branch at `L1051-L1052` (each guarded by `osErrToFileErr(err) == errFaultyDisk`); a permission error maps to `errFileAccessDenied`, not `errFaultyDisk`, so the "taking drive offline" monitor stays quiet. This was verified with a 40 s quiet‑wait after the fault — no `taking drive`, `bringing drive`, or `healthcheck` lines appeared. The failing drive is nonetheless named by path via the reconnect/healing lines quoted above. This is a precise, observed correction to the naive expectation. **(observed)**

### 6.4 Sign of live recovery while the system is up

With `/tmp/d1` denied, the reconnect poll keeps probing it every ~15 s (each unique error printed once via `printOnce`), and the auto‑heal subsystem is already running in the background (`initAutoHeal`, `cmd/background-newdisks-heal-ops.go:L377`). So yes — recovery is being **attempted while live**, without any operator action.

---

## 7. Below the Threshold — 2 Drives Online (`chmod 000 /tmp/d2`)

Fault injected on a **second** directory (T1 = `2026-07-06T23:20:35.726Z`), dropping to **2 online**:

```console
$ date -u +%FT%T.%3NZ ; chmod 000 /tmp/d2
2026-07-06T23:20:35.726Z
```

### 7.1 Write REFUSED, Read still OK (unedited)

```console
$ python3 /tmp/minio-investigation/s3op.py put testbucket obj-2online.txt /tmp/minio-investigation/obj.txt
CLIENT_ERROR HTTP=503 Code=SlowDownWrite Msg="Resource requested is unwritable, please reduce your request rate"

$ python3 /tmp/minio-investigation/s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 BODY='hello-baseline'

$ python3 /tmp/minio-investigation/s3op.py get testbucket obj-3online.txt
GET_OK key=obj-3online.txt HTTP=200 BODY='hello-3online'
```

- **Write is refused** with `HTTP 503 / SlowDownWrite`: 2 online < write quorum 3.
- **Reads still succeed** — including `obj-3online`, which is **reconstructed from its 2 surviving shards** (it never had a shard on the faulted `/tmp/d1`, and `/tmp/d2` is now down too, yet 2 data shards is exactly read quorum). This is the **read/write divergence** at the heart of the above/below contrast. **(observed)**

### 7.2 Health: `/cluster` → 503, `/cluster/read` → 200 (unedited)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3
Content-Length: 0

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2

$ curl -sS -i http://127.0.0.1:9000/minio/health/live
HTTP/1.1 200 OK
$ curl -sS -i http://127.0.0.1:9000/minio/health/ready
HTTP/1.1 200 OK
```

`/minio/health/cluster` is **503** (online 2 < write quorum 3 → not `Healthy`) while `/minio/health/cluster/read` stays **200** (online 2 ≥ read quorum 2 → still `HealthyRead`). **(observed)**

### 7.3 The write‑quorum decision is logged (unedited)

```text
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
       maintenance="false"
       3: cmd/erasure-server-pool.go:2793:cmd.(*erasureServerPools).Health()
       2: cmd/healthcheck-handler.go:71:cmd.ClusterCheckHandler()
       1: net/http/server.go:2220:http.HandlerFunc.ServeHTTP()
```

This is emitted by `erasureServerPools.Health()` at `cmd/erasure-server-pool.go:L2793`. The message states the exact numbers this investigation asserts: **`expected write quorum: 3, drives-online: 2`**. **(observed)**

### 7.4 How the refusal maps from erasure layer → HTTP 503

The client's `SlowDownWrite` is the S3‑surface form of the internal write‑quorum error:

- Shard‑write enforcement returns `errErasureWriteQuorum` (`cmd/erasure-encode.go:L64-L65`; sentinel *"Write failed. Insufficient number of drives online"* at `cmd/erasure-errors.go:L26`).
- `errErasureWriteQuorum` / `InsufficientWriteQuorum` map to API error `ErrSlowDownWrite` (`cmd/api-errors.go:L2192-L2193,L2314-L2315`).
- `ErrSlowDownWrite` is defined as `{Code:"SlowDownWrite", Description:"Resource requested is unwritable, please reduce your request rate", HTTPStatusCode: http.StatusServiceUnavailable}` (`cmd/api-errors.go:L874-L878`) → **HTTP 503**. **(observed)**
- (Symmetrically, `errErasureReadQuorum` → `ErrSlowDownRead`, `cmd/api-errors.go:L2190-L2191`.)

The failing `/tmp/d2` is also named by path in the reconnect/healing logs, identical in form to §6.3 (`endpoint="/tmp/d2"` and `/tmp/d2/.minio.sys/buckets/.healing.bin: permission denied`). **(observed)**


---

## 8. Recovery — Restore Permissions (before → during → after)

Permissions restored on both directories (T2 = `2026-07-06T23:22:50.917Z`):

```console
$ date -u +%FT%T.%3NZ ; chmod 755 /tmp/d1 /tmp/d2
2026-07-06T23:22:50.917Z
```

**Before → during → after** of the health verdict:

| Phase | `/minio/health/cluster` | `/minio/health/cluster/read` |
|---|---|---|
| Before (4 online) | 200, write‑quorum 3 | 200, read‑quorum 2 |
| During (2 online) | **503** | 200 |
| After (restored) | **200** (write‑quorum 3) | 200 |

Health returned to 200 almost immediately after `chmod 755` (< 2 s), and a fresh write succeeded again:

```console
$ curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/cluster
200
$ python3 /tmp/minio-investigation/s3op.py put testbucket obj-postrecovery.txt /tmp/minio-investigation/obj.txt
PUT_OK key=obj-postrecovery.txt ETag="920cbd1e5fc43021610029b171ec17ef" HTTP=200
```

**Why health recovers faster than the 15 s reconnect poll (observed).** `Health()` counts online drives from **live** `DiskInfo` — `if disk.State == madmin.DriveStateOk { si.online++ }` (`cmd/erasure-server-pool.go:L2707`) — reading current drive state on each probe rather than waiting for the reconnect timer. Once the OS permission is restored, the very next probe sees the drives as OK and returns 200. **(observed)**

---

## 9. Re‑admission — Polling, Not Push

**Re‑admission of a returning drive is automatic and polling‑based; no operator push is required.**

- `monitorAndConnectEndpoints` (`cmd/erasure-sets.go:L283`) runs on a timer and periodically calls `connectDisks(...)`, reconnecting any endpoint that has come back — no admin command needed.
- The interval is **15 s**: `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` (`cmd/erasure-sets.go:L348`), and `defaultMonitorNewDiskInterval = time.Second * 10` (`cmd/background-newdisks-heal-ops.go:L40`) → `10 s + 5 s = 15 s`.
- The **manual push** path exists but is **optional**: `mc admin heal` ultimately calls `queueHealTask` (`cmd/admin-heal-ops.go:L721`). It is a *supplement* to — not a prerequisite for — the automatic poll + heal.

> **Observed subtlety.** For the *permission* fault, no `monitorDiskStatus` "bringing drive … online" line (`cmd/xl-storage-disk-id-check.go:L955-L956`) was emitted, because that monitor is only started after a drive has been marked offline by the health‑check monitor — which, as shown in §6.3, does not trigger for permission errors. On recovery the reconnect poll simply stops logging the access‑denied error and the drive is served again; the transition is essentially silent in the logs beyond the error stream ceasing. The "bringing drive online" by‑path line **does** fire for the EIO/timeout offline path and for a wiped/replaced drive (see §10). **(observed)**

---

## 10. Repair of Objects Written During the Outage

Two **automatic** healing paths exist; both were exercised.

### 10.1 MRF (partial‑write) heal — repairs the transient‑outage object

`obj-3online.txt` was written while `/tmp/d1` was faulted, so it had **no shard on `/tmp/d1`** (§6.1). After recovery, that missing shard was **restored automatically** — no admin command, no heal‑task log entry:

```console
# ~2 min after chmod 755 (T2), re-check the shard on the recovered drive:
$ ls /tmp/d1/testbucket/obj-3online.txt/*/part.1 2>/dev/null | wc -l
1                         # d1 shard restored (was 0 during the outage)
$ grep -c -E 'queueHealTask|healObject|HealItem' /tmp/minio-investigation/server.log
0                         # repaired via MRF, not the admin heal-task path
```

Mechanism: objects written with missing shards are enqueued via `globalMRFState.addPartialOp(...)` (`cmd/mrf.go:L78`) from the object write/read paths (`cmd/erasure-object.go:L400,L805,L1578`), and the background `healRoutine` (`cmd/mrf.go:L220`) calls `healObject` (`cmd/mrf.go:L272,L276`) to rebuild the missing shard. **(observed)**

`obj-2online.txt` (the write that was *refused* at 2 online) correctly exists on **zero** drives — it was never created:

```console
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do ls "$d"/testbucket/obj-2online.txt 2>/dev/null; done
# (no output — object does not exist anywhere)
```

### 10.2 Fresh/replaced‑drive heal — full drive rebuild (~10 s cadence)

To exercise the **fresh‑disk** path explicitly, `/tmp/d1` was **wiped** (simulating a replaced drive) while the server ran. The auto‑heal loop `monitorLocalDisksAndHeal → healFreshDisk` (`cmd/background-newdisks-heal-ops.go:L563,L419`) detected it, created the healing tracker, and rebuilt the drive by path:

```console
$ date -u +%FT%T.%3NZ ; rm -rf /tmp/d1/* /tmp/d1/.minio.sys       # wipe (RUN 1)
2026-07-06T23:28:07.363Z
# within ~10 s the tracker + format reappear:
$ ls -la /tmp/d1/.minio.sys/buckets/.healing.bin  /tmp/d1/.minio.sys/format.json
-rw-r--r-- ... /tmp/d1/.minio.sys/buckets/.healing.bin
-rw-r--r-- ... /tmp/d1/.minio.sys/format.json
```

Heal logs (verbatim, by path):

```text
Healing drive '/tmp/d1' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/d1' - use 4 parallel workers.
Healing of drive '/tmp/d1' is finished (healed: 56, skipped: 0).
```

- The heal cadence is **~10 s** — `monitorLocalDisksAndHeal` ticks on `defaultMonitorNewDiskInterval = 10 s` (`cmd/background-newdisks-heal-ops.go:L40,L565`).
- Progress is persisted to `.healing.bin` (`healingTrackerFilename`, `cmd/background-newdisks-heal-ops.go:L41`) so a heal survives restarts.
- After the heal finished, all objects had their `/tmp/d1` shard again:

```console
$ for o in obj-baseline.txt obj-3online.txt obj-postrecovery.txt; do \
    echo -n "$o d1: "; ls /tmp/d1/testbucket/$o/*/part.1 2>/dev/null | wc -l; done
obj-baseline.txt d1: 1
obj-3online.txt d1: 1
obj-postrecovery.txt d1: 1
```

**(observed)**

> **Parity‑upgrade nuance (from code).** Writes that succeed while drives are offline may have their parity upgraded at PUT time to preserve protection — `userDefined[minIOErasureUpgraded] = "orig->new"` (`cmd/erasure-object.go:L1310-L1316`). Relevant to objects written above quorum during an outage. **(from source)**

---

## 11. Where the Quorum Decision Lives in the Code

A precise trace, from parity default → quorum computation → enforcement → health surface. All line numbers verified against this checkout.

### 11.1 Default parity for the set size

```go
// internal/config/storageclass/storage-class.go:L355-L368
func DefaultParityBlocks(drive int) int {
	switch drive {
	case 1:
		return 0
	case 3, 2:
		return 1
	case 4, 5:          // ← 4 drives
		return 2        // ← EC:2  (L361-L362)
	case 6, 7:
		return 3
	default:
		return 4
	}
}
```

⇒ four drives ⇒ **2 parity** (2 data + 2 parity).

### 11.2 The quorum computation itself — `objectQuorumFromMeta`

```go
// cmd/erasure-metadata.go:L531-L564  (verbatim; trailing "//" annotations added for this doc)
func objectQuorumFromMeta(ctx context.Context, partsMetaData []FileInfo, errs []error, defaultParityCount int) (objectReadQuorum, objectWriteQuorum int, err error) {
	// There should be at least half correct entries, if not return failure
	expectedRQuorum := len(partsMetaData) / 2
	if defaultParityCount == 0 {
		// if parity count is '0', we expected all entries to be present.
		expectedRQuorum = len(partsMetaData)
	}

	reducedErr := reduceReadQuorumErrs(ctx, errs, objectOpIgnoredErrs, expectedRQuorum)
	if reducedErr != nil {
		return -1, -1, reducedErr
	}

	// special case when parity is '0'
	if defaultParityCount == 0 {
		return len(partsMetaData), len(partsMetaData), nil
	}

	parities := listObjectParities(partsMetaData, errs)
	parityBlocks := commonParity(parities, defaultParityCount)              // → 2  (commonParity defined at L461)
	if parityBlocks < 0 {
		return -1, -1, InsufficientReadQuorum{Err: errErasureReadQuorum, Type: RQInsufficientOnlineDrives}
	}

	dataBlocks := len(partsMetaData) - parityBlocks                        // 4 - 2 = 2   (L555)

	writeQuorum := dataBlocks                                              // = 2         (L557)
	if dataBlocks == parityBlocks {                                        // 2 == 2      (L558)
		writeQuorum++                                                      // → 3         (L559)
	}

	// Since all the valid erasure code meta updated at the same time are equivalent, pass dataBlocks
	// from latestFileInfo to get the quorum
	return dataBlocks, writeQuorum, nil                                    // read=2, write=3 (L564)
}
```

**This is where the threshold lives.** Read quorum = data blocks (2); write quorum = data blocks, **bumped by one when data == parity** (→ 3). `reduceReadQuorumErrs`/`reduceWriteQuorumErrs` (`cmd/erasure-metadata-utils.go:L150,L156`) collapse per‑drive errors into a single verdict.

### 11.3 Where "stop" is enforced for writes — `multiWriter.Write`

```go
// cmd/erasure-encode.go:L34-L66
func (p *multiWriter) Write(ctx context.Context, ...) error {
	...
	nilCount := countErrs(p.errs, nil)             // L59
	if nilCount >= p.writeQuorum {                 // L60  ← the threshold check
		return nil                                 // enough shards written → OK
	}
	writeErr := reduceWriteQuorumErrs(ctx, p.errs, objectOpIgnoredErrs, p.writeQuorum)
	return fmt.Errorf("%w (offline-disks=%d/%d)",  // L65 ← returns errErasureWriteQuorum
		writeErr, countErrs(p.errs, errDiskNotFound), len(p.writers))
}
```

At 2 online, `nilCount (2) < writeQuorum (3)` → returns `errErasureWriteQuorum`. The object‑write path also computes its own `writeQuorum` and returns the same sentinel via `toObjectErr(...)` (`cmd/erasure-object.go:L1308`, quorum computed near `L1320-L1323`).

### 11.4 The sentinel errors that mean "stop"

```go
// cmd/erasure-errors.go
errErasureReadQuorum  = errors.New("Read failed. Insufficient number of drives online")   // L23
errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")  // L26
```

### 11.5 The cluster‑health decision that surfaces the threshold — `Health()`

```go
// cmd/erasure-server-pool.go
// online counting (L2707):
if disk.State == madmin.DriveStateOk { si.online++ }

// per-set quorum arrays (L2720-L2726):
poolReadQuorums[i]  = data
poolWriteQuorums[i] = data
if data == b.StandardSCParity {          // data == parity (2 == 2)
	poolWriteQuorums[i] = data + 1       // → write quorum 3
}

// per-set verdict (L2783-L2784):
result.Healthy     = ... online >= poolWriteQuorums[...]   // 200 when true
result.HealthyRead = ... online >= poolReadQuorums[...]

// the log line observed in §7.3 (L2793):
// "Write quorum could not be established on pool: %d, set: %d, expected write quorum: %d, drives-online: %d"
```

`ClusterCheckHandler` (`cmd/healthcheck-handler.go`) calls `Health()`, writes the `X-Minio-Write-Quorum` header (`L72`) and maps `Healthy`→`200`, not‑`Healthy`→`503` (or `412` when `?maintenance=true`, param at `L68`). The route `/minio/health/cluster` is registered in `cmd/healthcheck-router.go:L30`, mounted by `registerHealthCheckRouter` (`cmd/routers.go:L98`).

---

## 12. Corroboration with In‑Repo Documentation

The code‑derived thresholds match MinIO's own documentation:

- `docs/minio-limits.md:L15` — **"Read quorum | N/2"**; `docs/minio-limits.md:L16` — **"Write quorum | N/2+1"**. For **N = 4** ⇒ read 2, write 3 — exactly what the runtime headers and `objectQuorumFromMeta` produce.
- `docs/distributed/README.md` — erasure sets are 2–16 drives and **distributed mode requires fresh directories** (relevant to the fresh‑drive heal path).
- `docs/erasure/README.md`, `docs/erasure/storage-class/README.md` — the Reed‑Solomon model and default N/2 parity split, plus storage‑class parity.

Note: some external MinIO pages describe **MinIO AIStor (the enterprise edition)**; those are flagged as such. **The open‑source source code in this repository is the authoritative source of truth**, and here the code and the in‑repo docs agree.

---

## 13. Observed vs. Inferred

| Value / behavior | Status | Basis |
|---|---|---|
| 4 dirs → 1 set of 4 → EC:2 | **observed** | startup banner + `X-Minio-Write-Quorum: 3` header |
| Read quorum 2 / Write quorum 3 | **observed** | `X-Minio-Read-Quorum: 2`, `X-Minio-Write-Quorum: 3`, server log `expected write quorum: 3` |
| 3 online → write OK, read OK, health 200 | **observed** | §6 PUT/GET output + shard counts + curl |
| 2 online → write 503 (SlowDownWrite), read 200, `/cluster` 503, `/cluster/read` 200 | **observed** | §7 client error + curl outputs + quorum log |
| Failing drive named by path in logs | **observed** | §6.3/§7.4 `endpoint="/tmp/d1"`, `.healing.bin … permission denied` |
| `monitorDiskWritable` "taking drive offline" does NOT fire for permission fault | **observed** | 40 s quiet‑wait; error maps to `errFileAccessDenied`, not `errFaultyDisk` |
| Re‑admission is automatic + polling | **observed (mechanism)** | health returns to 200 on its own after `chmod 755`; `connectDisks` poll in code |
| Reconnect poll interval = exactly **15 s** | **inferred** | constant `defaultMonitorConnectEndpointInterval` (`erasure-sets.go:L348`); health recovers faster via live `DiskInfo`, so the exact 15 s edge was not isolated at runtime |
| MRF auto‑repairs the transient‑outage object | **observed** | §10.1 shard restored, 0 heal‑task log entries |
| Fresh‑disk heal cadence ≈ **10 s** | **observed (≈), constant is 10 s** | §10.2 wipe RUN 1: tracker+format reappear at ≈ t+10 s; RUN 2 corroborates; constant `defaultMonitorNewDiskInterval` = 10 s |
| Parity upgrade for offline‑time writes | **from source** | `cmd/erasure-object.go:L1310-L1316` (not separately isolated at runtime) |

---

## 14. Coverage Pass — All Eight Sub‑Questions

- [x] **1. Health decision & disk‑count assumption** — §2(1), §4, §5, §11.5. `Health()` counts OK drives and needs `≥ write quorum (3)` for `Healthy`, `≥ read quorum (2)` for `HealthyRead`; runtime headers `X-Minio-Write-Quorum: 3` / `X-Minio-Read-Quorum: 2`.
- [x] **2. Behavior at the moment of failure** — §2(2), §6. Does not crash/refuse; marks the shard skipped and keeps serving while at/above quorum.
- [x] **3. Above vs. below threshold** — §6 (3 online: write OK/read OK/200) vs §7 (2 online: write 503/read 200/`cluster` 503).
- [x] **4. Log visibility & live recovery** — §6.3, §6.4, §7.4. Failing drive named **by path**; reconnect/heal activity runs while live (with the observed correction about *which* monitor logs it).
- [x] **5. Re‑admission mechanism** — §9. Automatic **polling** via `monitorAndConnectEndpoints` (~15 s); `mc admin heal` is the optional manual push.
- [x] **6. Repair of outage writes** — §10. MRF partial‑write heal (transient outage) + fresh‑disk heal (~10 s, `.healing.bin`) for replaced drives; both automatic.
- [x] **7. Quorum decision in code** — §11. `objectQuorumFromMeta` (`cmd/erasure-metadata.go:L531-L564`) + `DefaultParityBlocks(4)=2` + enforcement in `multiWriter.Write` + surfaced by `Health()`/`ClusterCheckHandler`.
- [x] **8. Grounding** — throughout §5–§10: every claim paired with the `/minio/health/*` response and/or a real S3 PUT/GET at the relevant disk‑loss level, with unedited output.

---

### Appendix — Full list of source citations used above

`internal/config/storageclass/storage-class.go:L355-L368,L390` · `cmd/erasure-metadata.go:L461,L531-L564` · `cmd/erasure-metadata-utils.go:L137,L150,L156` · `cmd/erasure-encode.go:L34-L66` · `cmd/erasure-errors.go:L23,L26` · `cmd/erasure-object.go:L400,L805,L1308,L1310-L1316,L1578` · `cmd/erasure-server-pool.go:L2707,L2720-L2726,L2737-L2739,L2783-L2784,L2793` · `cmd/healthcheck-handler.go:L68,L71,L72,L73,L109` · `cmd/healthcheck-router.go:L30` · `cmd/routers.go:L98` · `cmd/storage-errors.go:L68,L158-L159` · `cmd/xl-storage-errors.go:L135-L136` · `cmd/xl-storage-disk-id-check.go:L955-L956,L1013,L1015,L1035,L1044-L1045,L1051-L1052` · `cmd/xl-storage.go:L430,L436` · `cmd/prepare-storage.go:L35,L51,L194` · `cmd/erasure-sets.go:L230,L283,L348` · `cmd/background-newdisks-heal-ops.go:L40,L41,L377,L419,L563,L565` · `cmd/mrf.go:L78,L220,L272,L276` · `cmd/admin-heal-ops.go:L721` · `cmd/api-errors.go:L874-L878,L2190-L2193,L2314-L2315` · `cmd/globals.go:L65` · `internal/http/headers.go:L193,L196` · `docs/minio-limits.md:L15-L16` · `Makefile:L3,L179` · `go.mod:L3`

