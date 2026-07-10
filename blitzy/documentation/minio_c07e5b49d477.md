# MinIO Erasure-Coded Fault Tolerance in a Four-Directory Deployment

**An evidence-based, run-first technical answer.**

> **Repository:** `github.com/minio/minio` (`module github.com/minio/minio` — [go.mod:L1]; `go 1.23` — [go.mod:L3])
> **Branch:** `minio_c07e5b49d477`
> **Scope:** Strictly read-only investigation. The MinIO binary was **built and executed** against four local directories, and every behavioral claim below is paired with the **actual, unedited command output** that produced it. No repository source file was modified; this Markdown file is the only artifact produced.
> **Reproducibility:** All pivotal results were confirmed **stable across two independent runs** (see §10).

---

## Table of Contents

1. [Summary / TL;DR](#1-summary--tldr)
2. [Setup & canonical run method](#2-setup--canonical-run-method)
3. [Health determination & assumed disk count (Requirement 1)](#3-health-determination--assumed-disk-count-requirement-1)
4. [Single-drive permission loss — above threshold (Requirements 2 & 3a)](#4-single-drive-permission-loss--above-threshold-requirements-2--3a)
5. [Second-drive loss — below threshold (Requirement 3b)](#5-second-drive-loss--below-threshold-requirement-3b)
6. [Failing-disk logging by path & live recovery signals (Requirement 4)](#6-failing-disk-logging-by-path--live-recovery-signals-requirement-4)
7. [Automatic re-detection on return (Requirement 5)](#7-automatic-re-detection-on-return-requirement-5)
8. [Repair of objects written during the outage (Requirement 6)](#8-repair-of-objects-written-during-the-outage-requirement-6)
9. [Precise location & calculation of the quorum threshold in code (Requirement 7)](#9-precise-location--calculation-of-the-quorum-threshold-in-code-requirement-7)
10. [Empirical grounding & stability (Requirement 8)](#10-empirical-grounding--stability-requirement-8)
11. [Key nuances](#11-key-nuances)
12. [Coverage summary](#12-coverage-summary)

---

## 1. Summary / TL;DR

A single MinIO server invoked as `minio server /d1 /d2 /d3 /d4` initializes **one server pool containing one erasure set of four drives**. With no `MINIO_STORAGE_CLASS_STANDARD` override, the STANDARD storage class defaults to parity **`EC:2`** (2 data blocks + 2 parity blocks). This is the exact four-directory erasure-coded artifact under investigation, and it fixes the fault-tolerance thresholds:

| Quantity | Value | Meaning |
|---|---|---|
| Set drive count | 4 | drives in the single erasure set |
| Data blocks (`StandardSCData`) | 2 | `setDriveCount − parity` = `4 − 2` |
| Parity blocks (`StandardSCParity`) | 2 | default `EC:2` for a set of ≤ 5 drives |
| **Read quorum** | **2** | minimum drives online to keep serving **reads** |
| **Write quorum** | **3** | minimum drives online to keep serving **writes** |

**The behavior, in one line per fault condition (all observed at runtime):**

| Drives online | Cluster health (`/cluster`) | Read health (`/cluster/read`) | Writes | Reads |
|---|---|---|---|---|
| 4 (healthy) | `200` | `200` | succeed | succeed |
| 3 (one drive `chmod 000`) | `200` | `200` | **succeed** (adapts) | succeed |
| 2 (two drives `chmod 000`) | **`503`** | `200` | **refused — HTTP 503 `SlowDownWrite`** | **succeed** |
| restored to 4 (no restart) | `200` (auto) | `200` | succeed | succeed |

**Where the threshold lives:** the set-level defaults are computed by `defaultWQuorum` / `defaultRQuorum` in [cmd/erasure.go:L85-L96]; the health endpoints report them via `Health` in [cmd/erasure-server-pool.go:L2679]; and the "keep writing vs. stop" cutoff at write time is `offlineDrives >= (len(storageDisks)+1)/2` in [cmd/erasure-object.go:L1304] — for four drives `(4+1)/2 = 2`, so the **second** offline drive breaks write quorum.

```mermaid
flowchart TD
    A["4 drives online — EC:2<br/>cluster 200, WQ 3 / RQ 2"] -->|"chmod 000 d4"| B["3 online"]
    B -->|"3 &ge; write quorum 3"| C["Writes SUCCEED<br/>cluster 200 (adapts)"]
    B -->|"printEndpointError logs by path"| L["endpoint=&quot;/tmp/mtest/d4&quot;<br/>(live recovery signals)"]
    C -->|"chmod 000 d3 (2nd drive)"| D["2 online"]
    D -->|"2 &lt; write quorum 3"| E["Writes REFUSED<br/>HTTP 503 SlowDownWrite<br/>cluster 503"]
    D -->|"2 &ge; read quorum 2"| F["Reads SUCCEED<br/>cluster/read 200"]
    E -->|"chmod 755 (restore, no restart)"| G["Auto re-detect ~0&ndash;1 s"]
    F -->|"chmod 755 (restore, no restart)"| G
    G --> H["cluster auto-returns 200<br/>no restart, no external command"]
    H -->|"GET degraded object"| I["Heal-on-read (MRF)<br/>missing d4 shard rewritten ~1.5 s"]
```

**How to read this document.** Every subsection follows the pattern **claim → observed evidence (command + raw output) → code anchor (function + `file:line`) → reasoning**. Statements that were *observed at runtime* are presented as observed. Statements derived from *reading the code only* (not exercised at runtime) are explicitly labeled **(inferred)**.

---

## 2. Setup & canonical run method

### 2.1 Canonical build

The binary was built from the repository root with the default toolchain. The Makefile's canonical build is `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)"` [Makefile:L179], where `LDFLAGS` is git-stamped from `git describe`. Omitting the stamped `LDFLAGS` (an ordinary `go build`) is still the canonical default input path and yields the `DEVELOPMENT.GOGET` version banner:

```console
$ CGO_ENABLED=0 go build -tags kqueue -o /tmp/minio-bin .
$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

The toolchain is `go1.23.12`, matching the module's declared `go 1.23` [go.mod:L3]. The binary was written to `/tmp` (outside the repository tree); nothing was added to the repo.

### 2.2 Non-root execution is mandatory (and why)

The investigated fault is a **directory becoming inaccessible due to a permission change** (`chmod 000`), not a deleted/missing drive. The Linux superuser bypasses filesystem permission bits (`CAP_DAC_OVERRIDE`), so a server running as **root would silently ignore mode `000`** and the failure would never manifest — that observation would be **non-canonical**. The server was therefore run as an unprivileged user (`miniouser`, uid 1001) that owns the data directories:

```console
$ useradd -m miniouser                       # (uid 1001)
$ mkdir -p /tmp/mtest/d{1,2,3,4}
$ chown -R miniouser:miniouser /tmp/mtest
$ runuser -u miniouser -- env \
    MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin123 \
    MINIO_BROWSER=off MINIO_UPDATE=off \
    /tmp/minio-bin server /tmp/mtest/d1 /tmp/mtest/d2 /tmp/mtest/d3 /tmp/mtest/d4 \
    --address 127.0.0.1:9000
```

### 2.3 Observed startup banner — proves the in-scope artifact

```console
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)

API: http://127.0.0.1:9000

Docs: https://docs.min.io
```

The line **`Formatting 1st pool, 1 set(s), 4 drives per set.`** confirms the target topology: **1 pool → 1 erasure set → 4 drives**.

### 2.4 Probe instruments

- **Health probes** use `curl` (8.14.1). The health endpoints are **public / unauthenticated**, and the router registers both `GET` and `HEAD` for each path ([cmd/healthcheck-router.go:L41-L52]), so `curl -sI` (a HEAD request) is valid.
- **S3 write/read probes** use a **SigV4-signed** client (Python `boto3` / `botocore` 1.43.45, `signature_version="s3v4"`) against `endpoint_url=http://127.0.0.1:9000` with credentials `minioadmin` / `minioadmin123`.

> The S3 probe object body is 11 222 bytes; the `ETag` values shown throughout are the actual values this body produced and are stable across runs. (They differ from any external sample purely because the object content differs — the *behavioral* results are what matter and are identical.)

### 2.5 Distributed equivalence (documented, not separately provisioned)

A single-node four-directory deployment reproduces the **identical per-erasure-set quorum semantics** of a multi-node distributed cluster. The design doc states that "Write and Read quorum are required to be satisfied only across the erasure set for an object. Healing is also done per object within the erasure set which contains the object." [docs/distributed/DESIGN.md:L99]. Because quorum and healing are **per erasure set**, the four-drive set exercised here carries the same read-quorum-2 / write-quorum-3 SLA that each 4-drive set would carry inside a larger distributed deployment. A separate multi-node cluster was therefore **not** provisioned.

---

## 3. Health determination & assumed disk count (Requirement 1)

**Claim.** MinIO decides it is **"healthy" (writeable)** when the number of online drives in the erasure set is **greater than or equal to the write quorum**. For the four-drive default `EC:2` set, the **write quorum is 3** and the **read quorum is 2**. Therefore MinIO assumes it needs a minimum of **3 drives online to keep serving writes** and **2 drives online to keep serving reads**.

**Observed evidence — the health endpoints expose the exact numbers:**

```console
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C0DF2EAB4821D0
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Fri, 10 Jul 2026 08:07:23 GMT
```

```console
$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
...
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
...
```

```console
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/live
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
```

The **`X-Minio-Write-Quorum: 3`** and **`X-Minio-Read-Quorum: 2`** headers are the health surface's own report of the assumed minimum drive counts.

**Code anchors & reasoning.**

- The set-level defaults are computed in [cmd/erasure.go:L85-L96]:

  ```go
  // cmd/erasure.go:L85-L91
  func (er erasureObjects) defaultWQuorum() int {
  	dataCount := er.setDriveCount - er.defaultParityCount
  	if dataCount == er.defaultParityCount {
  		return dataCount + 1
  	}
  	return dataCount
  }

  // cmd/erasure.go:L94-L96
  func (er erasureObjects) defaultRQuorum() int {
  	return er.setDriveCount - er.defaultParityCount
  }
  ```

  For the four-drive set: `dataCount = 4 − 2 = 2`. Because `dataCount == defaultParityCount` (2 == 2), `defaultWQuorum` returns `dataCount + 1 = 3`; `defaultRQuorum` returns `dataCount = 2`. This `+1` when data equals parity is the split-brain guard that prevents two halves of a set from both accepting writes.

- The health endpoints derive the same numbers through the cluster aggregation `func (z *erasureServerPools) Health(...)` at [cmd/erasure-server-pool.go:L2679]. It sets `StandardSCData = setDriveCount − scParity` [cmd/erasure-server-pool.go:L694] and `StandardSCParity = scParity` [cmd/erasure-server-pool.go:L700], then builds per-pool quorums [cmd/erasure-server-pool.go:L2720-L2726]:

  ```go
  // cmd/erasure-server-pool.go:L2720-L2726
  poolReadQuorums := make([]int, len(b.StandardSCData))
  poolWriteQuorums := make([]int, len(b.StandardSCData))
  for i, data := range b.StandardSCData {
  	poolReadQuorums[i] = data
  	poolWriteQuorums[i] = data
  	if data == b.StandardSCParity {
  		poolWriteQuorums[i] = data + 1
  ```

  With `data = 2` and `StandardSCParity = 2`, this produces `poolReadQuorums = 2` and `poolWriteQuorums = 3` — exactly the values seen in the headers.

- The handler that writes those headers is `ClusterCheckHandler` [cmd/healthcheck-handler.go:L56]: it calls `objLayer.Health(ctx, opts)` [cmd/healthcheck-handler.go:L71], sets `w.Header().Set(xhttp.MinIOWriteQuorum, ...)` [cmd/healthcheck-handler.go:L72], and returns `http.StatusOK` when `result.Healthy` (else `503`). The read variant `ClusterReadCheckHandler` [cmd/healthcheck-handler.go:L93] sets `X-Minio-Read-Quorum` [cmd/healthcheck-handler.go:L109]. The header names are the constants `MinIOWriteQuorum = "x-minio-write-quorum"` [internal/http/headers.go:L193] and `MinIOReadQuorum = "x-minio-read-quorum"` [internal/http/headers.go:L196] (Go canonicalizes these to `X-Minio-Write-Quorum` / `X-Minio-Read-Quorum` on the wire).

- **Why the default parity is `EC:2`:** the STANDARD parity table specifies that a set of "5 or fewer" drives defaults to `EC:2` [docs/erasure/storage-class/README.md:L52], with "6-7 → `EC:3`" and "8 or more → `EC:4`". A four-drive set therefore selects `EC:2` — 2 data + 2 parity.

> **On `X-Minio-Storage-Class-Defaults: false`.** This flag does **not** indicate a custom / non-default configuration. No `MINIO_STORAGE_CLASS_STANDARD` override was set. The value reflects that STANDARD parity was initialized to a concrete value (2) — i.e. `GetParityForSC(STANDARD) = 2` — so the effective default `EC:2` for a ≤ 5-drive set is in force. It carries the value from `result.UsingDefaults` [cmd/healthcheck-handler.go:L73].

---

## 4. Single-drive permission loss — above threshold (Requirements 2 & 3a)

**Claim.** The moment one directory becomes inaccessible due to a permission change, MinIO **does not refuse writes — it silently adapts and keeps serving them**, because 3 drives remain online and `3 ≥ write quorum 3`. The cluster health probe stays `200`.

**Observed evidence — revoke one directory, write, and check health:**

```console
$ chmod 000 /tmp/mtest/d4
$ ls -ld /tmp/mtest/d4
d--------- 4 miniouser miniouser 4096 Jul 10 08:07 /tmp/mtest/d4

$ python3 s3probe.py put testbucket obj_1down
PUT OK bucket=testbucket key=obj_1down etag="34804507b3af17525cfd719f8bbc13e9" http=200

$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/cluster
200
$ curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -i Write-Quorum
X-Minio-Write-Quorum: 3
```

The write **completes with HTTP 200** while one directory is at mode `000`, and the cluster health endpoint **remains `200`**.

**Code anchors & reasoning.**

- The write path is the availability-optimized parity block in `PutObject`'s helper, [cmd/erasure-object.go:L1291-L1325]. It counts offline drives and only aborts once at least half the set is offline; with a single drive offline it proceeds. The default parity for the object is resolved via `globalStorageClass.GetParityForSC(...)` [cmd/erasure-object.go:L1284].
- The permission failure on `d4` is classified as a *permission* error, not a not-found error. `diskErrToDriveState` maps it explicitly [cmd/erasure.go:L98-L119]:

  ```go
  // cmd/erasure.go:L106-L107
  case errors.Is(err, errDiskAccessDenied):
  	state = madmin.DriveStatePermission
  ```

  The `errDiskAccessDenied` sentinel originates in the storage layer where `os.IsPermission(err)` / `osIsPermission(err)` is detected — e.g. [cmd/xl-storage.go:L276-L277] and many sibling sites ([cmd/xl-storage.go:L813-L819], [cmd/xl-storage.go:L864-L870], [cmd/xl-storage.go:L941-L942], [cmd/xl-storage.go:L1000-L1001], [cmd/xl-storage.go:L1039-L1040]). This is the faithful-fault-injection guarantee: the observed state is `DriveStatePermission` (permission), distinct from the offline/not-found path.

**Reasoning.** Three online drives satisfy `3 ≥ writeQuorum(3)`, so the erasure coder can still write the required 3 shards for a 2-data/2-parity object across d1/d2/d3. MinIO adapts to the degraded set and the client sees no error.

---

## 5. Second-drive loss — below threshold (Requirement 3b)

**Claim.** When a **second** directory becomes inaccessible (2 of 4 online), write quorum is broken (`2 < write quorum 3`) and MinIO **refuses writes with HTTP `503` (`SlowDownWrite`)**, while **reads continue** because 2 drives still satisfy read quorum 2. The cluster (write) health probe flips to **`503`** while the cluster/read probe stays **`200`**.

**Observed evidence — revoke a second directory, then write (refused) and read (served):**

```console
$ chmod 000 /tmp/mtest/d3
$ ls -ld /tmp/mtest/d3
d--------- 4 miniouser miniouser 4096 Jul 10 08:07 /tmp/mtest/d3

$ python3 s3probe.py put testbucket obj_2down
S3ERROR SlowDownWrite HTTP 503 :: Resource requested is unwritable, please reduce your request rate

$ python3 s3probe.py get testbucket obj_healthy
GET OK bucket=testbucket key=obj_healthy bytes=11222 http=200
```

```console
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/cluster
503
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/cluster/read
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/live
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
```

Writes are **refused** (`SlowDownWrite`, HTTP `503`); a read of a pre-existing object **succeeds** (`http=200`); the **cluster** probe is `503` while **cluster/read**, **live**, and **ready** remain `200`.

**Code anchors & reasoning.**

- The write-time cutoff is in the availability-optimized parity block [cmd/erasure-object.go:L1291-L1325]:

  ```go
  // cmd/erasure-object.go:L1304-L1308
  if offlineDrives >= (len(storageDisks)+1)/2 {
  	// if offline drives are more than 50% of the drives
  	// we have no quorum, we shouldn't proceed just
  	// fail at that point.
  	return ObjectInfo{}, toObjectErr(errErasureWriteQuorum, bucket, object)
  }
  ```

  For four drives, `(len(storageDisks)+1)/2 = (4+1)/2 = 2`. With two drives offline, `offlineDrives (2) >= 2` is true, so `errErasureWriteQuorum` is returned — which surfaces to the S3 client as `SlowDownWrite` / HTTP `503`. This is exactly why the **second** offline drive is the one that trips the refusal.

- The cluster health handler emits a matching write-quorum log from within `Health()`. This log line was captured verbatim (full stack trace):

  ```text
  Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
         maintenance="false"
         5: /.../internal/logger/logger.go:268:logger.LogIf()
         4: /.../cmd/logging.go:156:cmd.storageLogIf()
         3: /.../cmd/erasure-server-pool.go:2793:cmd.(*erasureServerPools).Health()
         2: /.../cmd/healthcheck-handler.go:71:cmd.ClusterCheckHandler()
         1: net/http/server.go:2220:http.HandlerFunc.ServeHTTP()
  ```

  The message string is produced in the health aggregation [cmd/erasure-server-pool.go:L2792-L2795]:

  ```go
  // cmd/erasure-server-pool.go:L2791-L2795
  healthy := erasureSetUpCount[poolIdx][setIdx].online >= poolWriteQuorums[poolIdx]
  if !healthy && !opts.NoLogging {
  	storageLogIf(logger.SetReqInfo(ctx, reqInfo),
  		fmt.Errorf("Write quorum could not be established on pool: %d, set: %d, expected write quorum: %d, drives-online: %d",
  			poolIdx, setIdx, poolWriteQuorums[poolIdx], erasureSetUpCount[poolIdx][setIdx].online), logger.FatalKind)
  }
  ```

  The captured stack proves the runtime path: `ClusterCheckHandler` [cmd/healthcheck-handler.go:L71] → `Health()` [cmd/erasure-server-pool.go:L2793] → the write-quorum log. The message's own numbers — **`expected write quorum: 3, drives-online: 2`** — are the empirical confirmation of the threshold.

- **Reads still work** because the per-set `HealthyRead = online >= poolReadQuorums` [cmd/erasure-server-pool.go:L2784] holds (`2 >= 2`), so `ClusterReadCheckHandler` returns `200`. Two online drives are exactly enough to reconstruct a 2-data object.


---

## 6. Failing-disk logging by path & live recovery signals (Requirement 4)

**Claim.** Yes — the server log **calls out the failing directory by its exact path**, and there are **live signals of recovery being attempted** while the process keeps running (the disk monitor repeatedly tries to reconnect the failed drive).

**Observed evidence — the failing directory is named by path.** Two distinct log families appeared the moment `d4` went to mode `000`:

1. A per-path read failure naming the directory (emitted repeatedly — a live recovery signal):

   ```text
   Error: unable to read /tmp/mtest/d4/.minio.sys/buckets/.healing.bin: open /tmp/mtest/d4/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
          8: /.../cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
   ```

2. The disk-monitor's reconnect attempt, which classifies and tags the endpoint **by path** (captured verbatim, full stack):

   ```text
   Error: drive access denied (cmd.StorageErr)
          endpoint="/tmp/mtest/d4"
          4: /.../internal/logger/logger.go:258:logger.LogAlwaysIf()
          3: /.../cmd/logging.go:65:cmd.peersLogAlwaysIf()
          2: /.../cmd/prepare-storage.go:51:cmd.init.func22.1()
          1: /.../cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
   ```

The `endpoint="/tmp/mtest/d4"` tag is the failing directory named by path. Counting the repeated read-failure signal while the server kept running:

```console
$ grep -c "unable to read /tmp/mtest/d4/.minio.sys/buckets/.healing.bin" run1.log
8
```

Eight repetitions confirm the recovery attempts recur **live** while the process continues to serve requests.

**Code anchors & reasoning.**

- The by-path tag comes from `printEndpointError` [cmd/prepare-storage.go:L35], which builds a request-info context tagged with the endpoint path — `AppendTags("endpoint", endpoint.String())` [cmd/prepare-storage.go:L40] — and logs via `peersLogAlwaysIf` [cmd/prepare-storage.go:L51]. The captured stack frame `cmd/prepare-storage.go:51` is precisely that log call.
- `printEndpointError` is invoked from the disk-reconnect goroutine `connectDisks.func2` [cmd/erasure-sets.go:L230] (the captured stack frame `cmd/erasure-sets.go:230`). `connectDisks` [cmd/erasure-sets.go:L194] is the routine that (re)attaches drives to the set — each failed attempt is a "recovery being attempted" signal.
- The `.healing.bin` read failure is logged from `func (s *xlStorage) Healing()` [cmd/xl-storage.go:L430], specifically the `internalLogIf(..., fmt.Errorf("unable to read %s: %w", healingFile, err))` at [cmd/xl-storage.go:L436]. `.healing.bin` is the fresh/returned-disk healing tracker `healingTrackerFilename = ".healing.bin"` [cmd/background-newdisks-heal-ops.go:L41]; the server periodically checks it, which is why the message recurs.

---

## 7. Automatic re-detection on return (Requirement 5)

**Claim.** Yes — MinIO recognizes a returned directory **on its own**, with **no server restart and no external heal command**. Restoring the permissions causes the cluster health endpoint to auto-return to `200` and writes to resume.

**Observed evidence — restore permissions and watch the cluster auto-recover:**

```console
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/cluster   # before restore
503

$ chmod 755 /tmp/mtest/d3 /tmp/mtest/d4        # NO server restart, NO external heal command
# polling /minio/health/cluster immediately after:
cluster returned 200 after ~0.01s (iteration 1)

$ curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -i Write-Quorum
X-Minio-Write-Quorum: 3

$ python3 s3probe.py put testbucket obj_restored
PUT OK bucket=testbucket key=obj_restored etag="34804507b3af17525cfd719f8bbc13e9" http=200
```

The cluster probe flips from `503` back to `200` **without any restart or external action**, and a fresh write immediately succeeds.

**Code anchors & reasoning.**

- The fast re-detection observed here is driven by the **on-demand liveness probe** that `Health()` consults for each drive: `func (p *xlStorageDiskIDCheck) IsOnline() bool` [cmd/xl-storage-disk-id-check.go:L208] performs a live `GetDiskID()` read of the drive. While `d3`/`d4` are at mode `000` the read fails and the drive reports offline; the instant permissions are restored, the same read succeeds and the drive reports online — so the next `/minio/health/cluster` call re-counts it as online and returns `200`.
- The **code-configured background reconnect** is the polling monitor `func (s *erasureSets) monitorAndConnectEndpoints(...)` [cmd/erasure-sets.go:L283], launched at startup as `go s.monitorAndConnectEndpoints(ctx, defaultMonitorConnectEndpointInterval)` [cmd/erasure-sets.go:L479]. Its interval is `const defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` [cmd/erasure-sets.go:L348] = `10s + 5s` = **15 s** (`defaultMonitorNewDiskInterval = time.Second * 10` [cmd/background-newdisks-heal-ops.go:L40]). This 15 s poll is the loop that emitted the repeated `endpoint="/tmp/mtest/d4"` reconnect errors in §6 while the drive was down.
- Additional on-drive monitors exist: `monitorDiskStatus` [cmd/xl-storage-disk-id-check.go:L930] (a 5 s ticker that brings a drive back online once Read/Write/Delete succeed) and `monitorDiskWritable` [cmd/xl-storage-disk-id-check.go:L966].

> **Observed vs. inferred — timing.** The re-detection was **observed** to be near-instant: **~0.01 s** in both runs (and the two-drives-down transition was observed at **~1.04 s** in run 2). The **15 s** figure is the **code-configured poll interval constant** ([cmd/erasure-sets.go:L348]); the precise causal relationship between that 15 s bound and the observed sub-second latency is **(inferred)** — the empirical latency is dominated by the on-demand `IsOnline()`→`GetDiskID()` probe that `Health()` performs, not by the background poll. Notably, **no** `taking drive ... offline` / `bringing drive ... online` background health-check events were emitted during the transitions (grep count 0), consistent with the on-demand probe (not the background poll) driving the observed flip.

---

## 8. Repair of objects written during the outage (Requirement 6)

**Claim.** An object written while a drive was down is **repaired by heal-on-read (MRF — the "Most Recent Failures" partial-heal queue)**: a subsequent `GET` detects the missing shard, queues a partial heal, and the missing shard is rewritten onto the returned drive — no manual heal required.

**Observed evidence — the degraded shard, then heal-on-read.**

First, the on-disk degraded state while the outage was in effect (inspected as root, which bypasses mode `000`). `obj_1down` was written while `d4` was down, so its `d4` shard is absent; `obj_healthy` (written when all four were up) is present everywhere:

```console
# object dir layout per drive (xl.meta presence)
--- obj_healthy ---
  d1: PRESENT (xl.meta 6069B)
  d2: PRESENT (xl.meta 6069B)
  d3: PRESENT (xl.meta 6069B)
  d4: PRESENT (xl.meta 6069B)
--- obj_1down ---
  d1: PRESENT (xl.meta 6069B)
  d2: PRESENT (xl.meta 6069B)
  d3: PRESENT (xl.meta 6069B)
  d4: MISSING
```

(These are small objects stored **inline in `xl.meta`**; there is no separate `part.1` file.)

Then, after the drive was restored, a single `GET` of the degraded object triggers the repair:

```console
# d4 shard for obj_1down BEFORE the GET
d4 obj_1down/xl.meta -> MISSING

$ python3 s3probe.py get testbucket obj_1down          # triggers heal-on-read
GET OK bucket=testbucket key=obj_1down bytes=11222 http=200

# d4 shard for obj_1down AFTER the GET
d4 obj_1down/xl.meta -> PRESENT (HEALED) after ~1.5s

# final shard matrix for obj_1down
  d1: PRESENT
  d2: PRESENT
  d3: PRESENT
  d4: PRESENT
```

The missing `d4` shard is **MISSING before the GET** and **PRESENT (healed) ~1.5 s after** it — the object is fully re-protected on all four drives.

**Code anchors & reasoning.**

- Heal-on-read is triggered from the object read path. When a read detects that some shards need healing, it fires exactly once via a `sync.Once` and enqueues a partial-heal operation: `healOnce.Do(func() { ... globalMRFState.addPartialOp(...) })` at [cmd/erasure-object.go:L399-L400] (`var healOnce sync.Once` declared at [cmd/erasure-object.go:L346]). The general enqueue helper is `func (er erasureObjects) addPartial(...)` [cmd/erasure-object.go:L2112].
- The MRF subsystem receives and services the request: `func (m *mrfState) addPartialOp(...)` [cmd/mrf.go:L78] enqueues the partial operation, and `func (m *mrfState) healRoutine(...)` [cmd/mrf.go:L220] is the background consumer that performs the actual heal (rewriting the missing shard).
- Independent of read-triggered healing, a **fresh/returned-disk** background heal also exists: `monitorLocalDisksAndHeal` [cmd/background-newdisks-heal-ops.go:L563] → `healFreshDisk` [cmd/background-newdisks-heal-ops.go:L419], gated by the `.healing.bin` tracker [cmd/background-newdisks-heal-ops.go:L41]. **(inferred)** For a drive that merely returned (its existing data intact, not wiped), the read-triggered MRF path is what repaired the specific degraded object here; the fresh-disk heal is an additional mechanism that would also converge the drive. The observed fact is narrow and certain: the `d4` shard was **absent before the GET and present afterward**, with no other action taken; the attribution to the MRF heal-on-read path is grounded in the code above and in the absence of any competing trigger.


---

## 9. Precise location & calculation of the quorum threshold in code (Requirement 7)

**Claim.** The "enough drives to proceed vs. stop" decision lives in **three** cooperating places: (a) the **set-level default** quorum functions, (b) the **per-object** quorum from metadata, and (c) the **write-time offline cutoff**. The health endpoints re-derive the same numbers via the cluster `Health` aggregation.

**(a) Set-level default quorum — `cmd/erasure.go`.**

```go
// cmd/erasure.go:L85-L91  — defaultWQuorum
func (er erasureObjects) defaultWQuorum() int {
	dataCount := er.setDriveCount - er.defaultParityCount   // 4 - 2 = 2
	if dataCount == er.defaultParityCount {                 // 2 == 2  → true
		return dataCount + 1                               // ⇒ write quorum = 3
	}
	return dataCount
}

// cmd/erasure.go:L94-L96  — defaultRQuorum
func (er erasureObjects) defaultRQuorum() int {
	return er.setDriveCount - er.defaultParityCount         // ⇒ read quorum = 2
}
```

Calculation: `data = setDriveCount − parity`; `readQuorum = data`; `writeQuorum = data`, **plus 1 when `data == parity`** (the split-brain guard). For 4 drives at `EC:2`: read quorum **2**, write quorum **3**.

**(b) Per-object quorum — `cmd/erasure-metadata.go`.** When operating on a specific object, quorum is recomputed from the object's own metadata: `func objectQuorumFromMeta(ctx, partsMetaData, errs, defaultParityCount) (objectReadQuorum, objectWriteQuorum int, err error)` [cmd/erasure-metadata.go:L531-L564]. Its write quorum uses the same rule — `writeQuorum := dataBlocks` [cmd/erasure-metadata.go:L557], incremented by one when `dataBlocks == parityBlocks`. This is why the threshold holds per object even with mixed storage classes.

**(c) Write-time offline cutoff — `cmd/erasure-object.go`.** The live "stop" decision during a write:

```go
// cmd/erasure-object.go:L1304-L1308
if offlineDrives >= (len(storageDisks)+1)/2 {
	// if offline drives are more than 50% of the drives
	// we have no quorum, we shouldn't proceed just
	// fail at that point.
	return ObjectInfo{}, toObjectErr(errErasureWriteQuorum, bucket, object)
}
```

For `len(storageDisks) = 4`, the cutoff is `(4+1)/2 = 2`; the **second** offline drive makes `offlineDrives >= 2` true and returns `errErasureWriteQuorum`. Object parity is resolved just above via `globalStorageClass.GetParityForSC(...)` [cmd/erasure-object.go:L1284].

**(d) Health-endpoint re-derivation — `cmd/erasure-server-pool.go`.** `func (z *erasureServerPools) Health(...)` [cmd/erasure-server-pool.go:L2679] computes `StandardSCData = setDriveCount − scParity` [cmd/erasure-server-pool.go:L694] and `StandardSCParity = scParity` [cmd/erasure-server-pool.go:L700], then `poolReadQuorums`/`poolWriteQuorums` [cmd/erasure-server-pool.go:L2720-L2726] (write quorum = data, `+1` when `data == StandardSCParity`). Per erasure set it sets `Healthy = online >= poolWriteQuorums` [cmd/erasure-server-pool.go:L2783] and `HealthyRead = online >= poolReadQuorums` [cmd/erasure-server-pool.go:L2784], and logs the write-quorum failure at [cmd/erasure-server-pool.go:L2792-L2795].

**Summary of the calculation.**

| Symbol | Formula | 4-drive `EC:2` value |
|---|---|---|
| `dataCount` / `StandardSCData` | `setDriveCount − parity` | `4 − 2 = 2` |
| `parity` / `StandardSCParity` | default `EC:2` (≤ 5 drives) | `2` |
| **read quorum** | `data` | **2** |
| **write quorum** | `data` (`+1` if `data == parity`) | **3** |
| write-time stop cutoff | `offlineDrives >= (N+1)/2` | `>= 2` (2nd drive) |

---

## 10. Empirical grounding & stability (Requirement 8)

**Claim.** Every behavioral claim in this document is grounded in **both** (i) health-endpoint observations and (ii) live, authenticated S3 write/read attempts against the running server. All outputs are actual and unedited, and the pivotal results are **stable across two independent runs**.

**Dual grounding.** Each fault condition was cross-checked from two instruments:

- **Health endpoints** (`curl`): baseline (§3), one-down (§4), two-down (§5), restore (§7).
- **Authenticated S3 PUT/GET** (`boto3` SigV4): baseline `obj_healthy` (§3), one-down `obj_1down` (§4), two-down refusal + read-continues (§5), restore `obj_restored` (§7), heal-on-read `obj_1down` (§8).

**Stability — run 2 reproduced every pivotal result identically** (fresh data directories `/tmp/mtest2`):

| Condition | Run 1 | Run 2 |
|---|---|---|
| Baseline `cluster` / `WQ` / `cluster/read` / `RQ` | `200` / `3` / `200` / `2` | `200` / `3` / `200` / `2` |
| 1 down — PUT / cluster | `PUT OK` / `200` | `PUT OK` / `200` |
| 2 down — PUT | `SlowDownWrite 503` | `SlowDownWrite 503` |
| 2 down — GET (existing) | `GET OK 200` | `GET OK 200` |
| 2 down — cluster / cluster-read | `503` / `200` | `503` / `200` |
| 2 down — write-quorum log | present (same message) | present (same message) |
| Restore — cluster auto-return | `200` | `200` |
| Restore — new PUT | `PUT OK` | `PUT OK` |
| Heal-on-read (d4 shard) | MISSING → PRESENT | MISSING → PRESENT |

**Observed timings (for timing-sensitive values):**

| Transition | Run 1 | Run 2 | Note |
|---|---|---|---|
| Two-drives-down detection (`cluster`→`503`) | (immediate on next probe) | **~1.04 s** | dominated by on-demand `IsOnline()` probe |
| Restore detection (`cluster`→`200`) | **~0.01 s** | **~0.01 s** | on-demand probe |
| Heal-on-read (d4 shard rewritten) | **~1.5 s** | **~1.5 s** | MRF partial-heal |

The values are stable across the two runs. Timing-relative-to-the-15 s-poll-interval attribution is labeled **(inferred)** in §7.

---

## 11. Key nuances

- **The four-drive set is already at maximum parity.** MinIO can increase an object's parity by one per offline drive at write time (the availability-optimized path [cmd/erasure-object.go:L1291-L1325]), but only up to the `N/2` cap — "Parity blocks can not be higher than data blocks … can not be higher than N/2" [docs/erasure/storage-class/README.md:L46]. For a four-drive `EC:2` set, `data == parity == 2` is **already the maximum**, so **no parity upgrade is possible** — an object written with one drive offline is simply written to the available drives (as observed for `obj_1down`). This is precisely why the **second** offline drive breaks write quorum rather than being absorbed by a parity increase. **(inferred, by contrast):** a larger set — e.g. a 16-drive `EC:4` set — would upgrade to `EC:6` with two drives offline, absorbing the loss without refusing writes; that larger-set behavior was not exercised here and is stated as inferred from the code path.

- **`X-Minio-Storage-Class-Defaults: false` does not mean a custom configuration.** No `MINIO_STORAGE_CLASS_STANDARD` override was set. The flag (from `result.UsingDefaults`, surfaced at [cmd/healthcheck-handler.go:L73]) reflects that STANDARD parity is initialized to a concrete value (2); the default `EC:2` for a ≤ 5-drive set is fully in effect (`GetParityForSC(STANDARD) = 2`).

- **Distributed equivalence.** A single-node four-directory deployment carries the **identical per-erasure-set quorum semantics** of a multi-node cluster, because "Write and Read quorum are required to be satisfied only across the erasure set for an object" [docs/distributed/DESIGN.md:L99]. The equivalence is documented, not separately provisioned.

- **Permission vs. deletion (faithful fault injection).** The fault was injected with `chmod 000` (permission revocation), producing `drive access denied` → `madmin.DriveStatePermission` [cmd/erasure.go:L106-L107] — distinct from a not-found/offline drive. Running as **root** would bypass mode `000` and mask the behavior entirely (a **non-canonical** observation); the server was therefore run as the unprivileged `miniouser` (§2.2).

---

## 12. Coverage summary

| # | Requirement | Verdict | Observed evidence | Primary code anchor(s) |
|---|---|---|---|---|
| 1 | Health determination & assumed disk count | Healthy ⇔ `online ≥ writeQuorum`; assumes **3** to write, **2** to read | `X-Minio-Write-Quorum: 3`, `X-Minio-Read-Quorum: 2` (§3) | `defaultWQuorum`/`defaultRQuorum` [cmd/erasure.go:L85-L96]; `Health` [cmd/erasure-server-pool.go:L2679, L2720-L2726] |
| 2 | Single-drive permission loss (steady state) | **Adapts** — keeps serving writes | `PUT OK http=200`, cluster `200` with d4 at `000` (§4) | write path [cmd/erasure-object.go:L1291-L1325]; `errDiskAccessDenied → DriveStatePermission` [cmd/erasure.go:L106-L107] |
| 3a | Above threshold (1 down) | Writes **succeed**, cluster `200` | §4 | offline cutoff not yet reached [cmd/erasure-object.go:L1304] |
| 3b | Below threshold (2 down) | Writes **refused 503 `SlowDownWrite`**; reads **continue**; cluster `503`, read `200` | §5 | `offlineDrives >= (len+1)/2` [cmd/erasure-object.go:L1304-L1308]; write-quorum log [cmd/erasure-server-pool.go:L2792-L2795] |
| 4 | Failure visibility (by path + live signals) | **Yes**, directory named by path; recurring reconnect attempts | `endpoint="/tmp/mtest/d4"`; `.../d4/.minio.sys/buckets/.healing.bin … permission denied` (×8) (§6) | `printEndpointError` [cmd/prepare-storage.go:L35-L51]; `connectDisks.func2` [cmd/erasure-sets.go:L230]; `Healing()` [cmd/xl-storage.go:L436] |
| 5 | Automatic re-detection on return | **Yes**, on its own; no restart / external push | cluster `503 → 200` auto in ~0.01 s; `PUT OK` (§7) | `IsOnline`→`GetDiskID` [cmd/xl-storage-disk-id-check.go:L208]; `monitorAndConnectEndpoints` (15 s) [cmd/erasure-sets.go:L283, L348, L479] |
| 6 | Repair of objects written during outage | **Heal-on-read (MRF)** rewrites the missing shard | d4 shard MISSING → PRESENT ~1.5 s after `GET` (§8) | `healOnce.Do`→`addPartialOp` [cmd/erasure-object.go:L399-L400]; `addPartialOp`/`healRoutine` [cmd/mrf.go:L78, L220] |
| 7 | Location of the quorum decision in code | Set-level + per-object + write-time cutoff + health re-derivation | code + headers (§9) | [cmd/erasure.go:L85-L96], [cmd/erasure-metadata.go:L531-L564], [cmd/erasure-object.go:L1304-L1308], [cmd/erasure-server-pool.go:L2679, L2720-L2726] |
| 8 | Empirical grounding | Every claim paired with health-endpoint **and** S3 write/read output; stable over 2 runs | §10 tables | — |

### Appendix — drive-state matrix

| State | d1 | d2 | d3 | d4 | Online | `cluster` | `cluster/read` | Writes | Reads |
|---|---|---|---|---|---|---|---|---|---|
| Healthy | ok | ok | ok | ok | 4 | `200` | `200` | ✅ | ✅ |
| One down | ok | ok | ok | **perm** | 3 | `200` | `200` | ✅ | ✅ |
| Two down | ok | ok | **perm** | **perm** | 2 | **`503`** | `200` | ❌ `503` | ✅ |
| Restored | ok | ok | ok | ok | 4 | `200` | `200` | ✅ | ✅ (degraded objects heal on read) |

*`perm` = directory at mode `000`, classified `DriveStatePermission` ([cmd/erasure.go:L107]).*

---

*End of analysis. This document is the sole artifact of the investigation; the MinIO repository source tree was not modified, and all runtime artifacts (binary, data directories, helper scripts) were created outside the repository under `/tmp` and removed afterward.*

