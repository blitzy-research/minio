# MinIO Runtime Fault Tolerance: Erasure Coding, Quorum, and Self-Healing

This document answers, with evidence, how MinIO behaves at runtime when drives in a four-directory, erasure-coded deployment become inaccessible and later recover. It addresses eight sub-questions: how MinIO decides it is "healthy" and what it assumes about the required number of disks (Q1); whether it adapts or refuses to write when one directory becomes inaccessible (Q2); the contrast between staying above the quorum threshold and dropping below it (Q3); whether the logs name the failing disk by path and attempt recovery while live (Q4); whether MinIO self-detects a restored directory via polling or must be pushed into healing (Q5); how objects written while a disk was down are repaired once it returns (Q6); where the quorum decision lives in code and how the "enough disks vs. stop" threshold is calculated (Q7); and how the whole explanation is grounded in observable health-endpoint and write-attempt behavior (Q8).

**Every claim below is grounded in either a `file:line` citation into the MinIO source or in verbatim runtime output captured from a live four-drive erasure deployment.** The investigation was performed *run-first*: MinIO was built, run as a non-root user on a four-drive erasure set, degraded via `chmod 000`, recovered via `chmod 755`, and its real output captured. Where a value is quoted (a status code, a header, a log line, a quorum number), it is reproduced exactly as observed; where behavior differs from a naive expectation, that is stated honestly rather than glossed over.

## Methodology

**Deployment model.** The user's "distributed mode with four directories" is faithfully realized as a **single-node, four-drive erasure set** — the correct local equivalent for observing per-set quorum behavior. A multi-host cluster is unnecessary to answer these questions because quorum is a *per-erasure-set* property. The server was started with:

```
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  minio server "/tmp/mtest2/data{1...4}" \
  --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
```

This forms **one** erasure set of four drives at the default `EC:2` (2 data + 2 parity). The startup banner confirms it (captured verbatim):

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.2 linux/amd64)

API: http://127.0.0.1:9000
WebUI: http://127.0.0.1:9001
```

The banner line `Formatting %s pool, %v set(s), %v drives per set.` is emitted from `cmd/prepare-storage.go:194`.

**Build.** MinIO was built with the project's pinned Go toolchain (`go 1.23` in `go.mod`; the investigation used **Go 1.23.2**) via `go build ./` with `GOFLAGS=-mod=mod`. `go.mod` and `go.sum` were left byte-for-byte unchanged (verified by `sha256sum` before and after), and `git status --porcelain` reported empty throughout the investigation.

**Non-root execution is MANDATORY.** MinIO was run as a **non-root** user (`miniotest`, uid 1001). A root process bypasses POSIX permission checks, so `chmod 000` on a data directory would have **no** effect and writes would still succeed — the "inaccessible directory" simulation is only valid when the server runs as a non-root user. This was verified directly: after `chmod 000 /tmp/mtest2/data2`,

```
$ su miniotest -c 'ls /tmp/mtest2/data2'
ls: cannot open directory '/tmp/mtest2/data2': Permission denied
```

**Degradation / recovery steps.**

- One disk down = `chmod 000 /tmp/mtest2/data2` → 3 of 4 online (**above** write quorum 3).
- Second disk down = `chmod 000 /tmp/mtest2/data3` → 2 of 4 online (**below** write quorum 3).
- Recover = `chmod 755` on both directories → back to 4 online.

**Observation tooling (temporary, removed afterward).** S3 `PUT`/`GET` were issued via a small `boto3` script (`s3v4` signing, path-style addressing, endpoint `http://127.0.0.1:9000`, credentials `minioadmin:minioadmin`, bucket `testbucket`); the health endpoints were probed with `curl`; the raw `503` XML body was captured via `boto3` with the `botocore.parsers` DEBUG logger enabled. All observation scripts, the built binary, the data directories, and the captured logs live outside the repository under `/tmp` and were removed afterward, so the repository is left unchanged apart from this document.

The S3 client tool referenced throughout as `python3 s3put.py <all|put|get> <key>` is the following script (`/tmp/s3put.py`). It PUTs a deterministic per-key payload (`key + " erasure-set payload"`, which is 33 bytes for `obj-1down.txt` and 36 bytes for `obj-baseline.txt`) and prints the HTTP status, the `ETag` on `PUT`, and the byte count on `GET`, verbatim:

```python
#!/usr/bin/env python3
import sys, boto3
from botocore.client import Config
from botocore.exceptions import ClientError

ENDPOINT = "http://127.0.0.1:9000"
BUCKET   = "testbucket"
s3 = boto3.client(
    "s3", endpoint_url=ENDPOINT,
    aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
    region_name="us-east-1",
)

def body_for(key):                       # deterministic payload => reproducible ETag/bytes
    return (key + " erasure-set payload").encode()

def mb():
    try:
        s3.create_bucket(Bucket=BUCKET); print("CREATE_BUCKET code=OK http=200")
    except ClientError as e:
        print("CREATE_BUCKET code=%s http=%s" % (
            e.response["Error"]["Code"], e.response["ResponseMetadata"]["HTTPStatusCode"]))

def put(key):
    try:
        r = s3.put_object(Bucket=BUCKET, Key=key, Body=body_for(key))
        print('PUT key=%s status=%s etag=%s' % (
            key, r["ResponseMetadata"]["HTTPStatusCode"], r["ETag"]))
    except ClientError as e:
        print("PUT key=%s ERROR http=%s Code=%s Message=%s" % (
            key, e.response["ResponseMetadata"]["HTTPStatusCode"],
            e.response["Error"]["Code"], e.response["Error"]["Message"]))

def get(key):
    try:
        r = s3.get_object(Bucket=BUCKET, Key=key); n = len(r["Body"].read())
        print("GET key=%s status=%s bytes=%d" % (
            key, r["ResponseMetadata"]["HTTPStatusCode"], n))
    except ClientError as e:
        print("GET key=%s ERROR http=%s Code=%s" % (
            key, e.response["ResponseMetadata"]["HTTPStatusCode"], e.response["Error"]["Code"]))

cmd = sys.argv[1]; key = sys.argv[2] if len(sys.argv) > 2 else "obj.txt"
if   cmd == "all": mb(); put(key); get(key)
elif cmd == "put": put(key)
elif cmd == "get": get(key)
elif cmd == "mb":  mb()
```

The health endpoints were probed with `curl -s -o /dev/null -D - <url>` (headers) or `curl -s -o /dev/null -w "%{http_code}" <url>` (status only). Drive degradation/recovery used `chmod 000`/`chmod 755` on the data directories. The `xl.meta` shard map used a shell loop over `/tmp/mtest2/data{1..4}` (shown in Q6). Each of these commands is repeated inline next to the output it produced.

## Q1 — How does MinIO decide it is "healthy", and what does it assume about the required number of disks?

**Answer.** For four drives, MinIO auto-selects the default erasure config `EC:2` (2 data + 2 parity), which sets **read quorum = 2** and **write quorum = 3**. It reports the cluster healthy-for-writes only while at least `write quorum` drives (3) are online, and healthy-for-reads while at least `read quorum` drives (2) are online. The write-quorum threshold is surfaced directly on the health endpoint as the header `X-Minio-Write-Quorum: 3`.

**Observed evidence (healthy, 4/4 online).**

Command:

```
curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/cluster
```

Response headers:

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE0CC00EFF109B
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Wed, 01 Jul 2026 03:28:41 GMT
```

Commands (`/minio/health/live` and `/minio/health/cluster/read`):

```
curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/live | head -1
curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/cluster/read | head -1
```

Both return:

```
HTTP/1.1 200 OK   (for /minio/health/live)
HTTP/1.1 200 OK   (for /minio/health/cluster/read)
```

**Code.**

- Default parity for four drives: `internal/config/storageclass/storage-class.go:355` `func DefaultParityBlocks(drive int) int` — `case 4, 5:` (`:361`) `return 2` (`:362`) → `EC:2`. It is applied at `internal/config/storageclass/storage-class.go:391` `cfg.Standard.Parity = DefaultParityBlocks(setDriveCount)`.
- Cluster health decision: `cmd/erasure-server-pool.go:2679` `func (z *erasureServerPools) Health(ctx context.Context, opts HealthOptions) HealthResult`. The per-pool quorums are computed at `cmd/erasure-server-pool.go:2722-2727`:
  ```go
  poolWriteQuorums[i] = data
  if data == b.StandardSCParity {
      poolWriteQuorums[i] = data + 1
  }
  ```
  The healthy-for-writes test is `cmd/erasure-server-pool.go:2783` `Healthy: erasureSetUpCount[poolIdx][setIdx].online >= poolWriteQuorums[poolIdx]`, and read-health uses `poolReadQuorums` at `cmd/erasure-server-pool.go:2784`.
- Health HTTP probe + header: `cmd/healthcheck-handler.go:56` `func ClusterCheckHandler(...)`; the header is set at `cmd/healthcheck-handler.go:72` `w.Header().Set(xhttp.MinIOWriteQuorum, strconv.Itoa(result.WriteQuorum))`; the `200` path is `cmd/healthcheck-handler.go:89` `writeResponse(w, http.StatusOK, nil, mimeNone)`. The header constant is `internal/http/headers.go:193` `MinIOWriteQuorum = "x-minio-write-quorum"` — note the source declares it lowercase, and Go's `http.Header` canonicalizes it to the on-the-wire form `X-Minio-Write-Quorum`, which is what `curl` shows above.
- Corroborating in-repo docs: `docs/erasure/storage-class/README.md` documents `EC:2` as the default for five-or-fewer-drive sets; `docs/metrics/healthcheck/README.md` documents that `/minio/health/cluster` returns `200 OK` when the cluster has write quorum and shows `X-Minio-Write-Quorum: 3` in its own example.

**Rationale.** With four drives MinIO stripes each object into 2 data + 2 parity shards; it can therefore lose up to two shards and still reconstruct the object (read), but it insists on being able to durably place a strict-majority write set (3) before accepting a write. "Healthy" on `/minio/health/cluster` therefore means, literally, "online drives ≥ write quorum".

## Q2 — When one directory becomes inaccessible during operation, does MinIO adapt and keep going, or refuse to write?

**Answer.** With **one** directory lost (3 of 4 online, still ≥ write quorum 3), MinIO **adapts and keeps going** — writes still succeed with HTTP `200`.

**Observed evidence (one down, after `chmod 000 /tmp/mtest2/data2`).**

Command:

```
python3 s3put.py all obj-1down.txt
```

Output:

```
CREATE_BUCKET code=BucketAlreadyOwnedByYou http=409
PUT key=obj-1down.txt status=200 etag="78620bf84c381a7f6e03ff7efe1edc70"
GET key=obj-1down.txt status=200 bytes=33
```

A prior object remains readable — command `python3 s3put.py get obj-baseline.txt`:

```
GET key=obj-baseline.txt status=200 bytes=36
```

**Code.** The S3 `PUT` is routed to the object's erasure set by `cmd/erasure-sets.go:747` `func (s *erasureSets) PutObject(...)` → `set := s.getHashedSet(object)` (`cmd/erasure-sets.go:748`) → `return set.PutObject(...)` (`cmd/erasure-sets.go:749`), which lands in `cmd/erasure-object.go:1240` `func (er erasureObjects) PutObject(...)` → `return er.putObject(...)` (`cmd/erasure-object.go:1241`). The write path itself is `cmd/erasure-object.go:1245` `func (er erasureObjects) putObject(...)`, which computes the write quorum for this PUT at `cmd/erasure-object.go:1323-1325`:

```go
writeQuorum := dataDrives          // L1323
if dataDrives == parityDrives {    // L1324  (2 == 2)
    writeQuorum++                  // L1325  -> 3
}
```

and enforces it when erasure-encoding the shards at `cmd/erasure-object.go:1425` `n, erasureErr := erasure.Encode(ctx, toEncode, writers, buffer, writeQuorum)`. The threshold is applied inside the encoder's per-stripe writer `cmd/erasure-encode.go:59-65` — `nilCount := countErrs(p.errs, nil)` then `if nilCount >= p.writeQuorum { return nil }`, otherwise `reduceWriteQuorumErrs(ctx, p.errs, objectOpIgnoredErrs, p.writeQuorum)` returns a quorum error. With 3 drives online ≥ `writeQuorum` 3, enough shard writes succeed, so the PUT returns `200`.

**Rationale.** Because write quorum is 3 and three drives remain online, MinIO can still place enough shards to satisfy durability, so it does not draw a hard line — it writes the object (with the shard that would have gone to the down disk queued for later healing; see Q6).

## Q3 — Contrast still-above-threshold (one disk lost) vs below-threshold (a second disk lost).

**Answer.** Above the threshold (3 online): writes succeed (`200`) and `/minio/health/cluster` is `200`. Below the threshold (2 online, < write quorum 3): MinIO **draws a hard line and refuses writes** with HTTP **`503 SlowDownWrite`**, and `/minio/health/cluster` returns **`503`** — while **reads still succeed** (`GET` `200`, `/minio/health/cluster/read` `200`) because read quorum (2) is still met.

**Observed evidence (two down, after `chmod 000 /tmp/mtest2/data3`; `data2`+`data3` down, `data1`+`data4` online).**

Command:

```
python3 s3put.py put obj-2down.txt
```

Output:

```
PUT key=obj-2down.txt ERROR http=503 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate
```

Raw `503` XML body — captured with this `boto3` script (`/tmp/raw503.py`), which attaches the `botocore.parsers` DEBUG logger (that logger records the verbatim response body) and then does a `PUT`:

```python
#!/usr/bin/env python3
import logging, io, sys, boto3
from botocore.client import Config
from botocore.exceptions import ClientError

buf = io.StringIO(); h = logging.StreamHandler(buf); h.setLevel(logging.DEBUG)
logging.getLogger("botocore.parsers").addHandler(h)
logging.getLogger("botocore.parsers").setLevel(logging.DEBUG)

s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
    aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
    region_name="us-east-1")
key = sys.argv[1]
try:
    s3.put_object(Bucket="testbucket", Key=key, Body=(key + " erasure-set payload").encode())
except ClientError as e:
    print("HTTP", e.response["ResponseMetadata"]["HTTPStatusCode"])
for line in buf.getvalue().splitlines():           # print the logged raw XML body
    i = line.find("<?xml")
    if i != -1: print(line[i:].rstrip("'\""))
```

Run as `python3 raw503.py obj-raw503.txt`; it prints `HTTP 503` and the exact XML body MinIO returned (`RequestId` is server-assigned per request; `HostId` is deployment-stable):

```
HTTP 503
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownWrite</Code><Message>Resource requested is unwritable, please reduce your request rate</Message><Key>obj-raw503.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/obj-raw503.txt</Resource><RequestId>18BE16B2A0630378</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Reads are still served — command `python3 s3put.py get obj-baseline.txt`:

```
GET key=obj-baseline.txt status=200 bytes=36
```

Health probes — commands `curl -s -o /dev/null -D - .../minio/health/cluster` (and `/cluster/read`, `/live`):

```
HTTP/1.1 503 Service Unavailable      (/minio/health/cluster; header still X-Minio-Write-Quorum: 3)
HTTP/1.1 200 OK                       (/minio/health/cluster/read)
HTTP/1.1 200 OK                       (/minio/health/live)
```

**Code.**

- Internal quorum error → S3 error: `cmd/api-errors.go:2192-2193` `case errErasureWriteQuorum:` → `apiErr = ErrSlowDownWrite`. The `ErrSlowDownWrite` definition is `cmd/api-errors.go:874-877`:
  ```go
  ErrSlowDownWrite: {
      Code:           "SlowDownWrite",
      Description:    "Resource requested is unwritable, please reduce your request rate",
      HTTPStatusCode: http.StatusServiceUnavailable,
  },
  ```
  (`Code` at `:875`, `Description` at `:876`, `HTTPStatusCode` at `:877`.) The object layer maps `errErasureWriteQuorum → InsufficientWriteQuorum` at `cmd/object-api-errors.go:164` (`case errErasureWriteQuorum.Error():`), whose `Unwrap()` returns `errErasureWriteQuorum` (`cmd/object-api-errors.go:253-254`); `InsufficientWriteQuorum` also maps to `ErrSlowDownWrite` at `cmd/api-errors.go:2314-2315`.
- Health-endpoint `503` path: `cmd/healthcheck-handler.go:85` `writeResponse(w, http.StatusServiceUnavailable, nil, mimeNone)`.

**Rationale.** Losing the second disk drops online drives to 2, below the write quorum of 3, so MinIO can no longer guarantee a durable write and deliberately rejects the write rather than accept an under-protected object. The reduced-but-sufficient read quorum (2) still allows reconstruction of existing objects, so reads and `/minio/health/cluster/read` stay healthy.

## Q4 — Do the logs name the failing disk directly by path, and is any recovery attempted while live?

**Answer.** **Yes on both counts.** The logs name the failing directory **directly by path** — both as `endpoint="/tmp/mtest2/data2"` in the reconnect loop and as the full path in the per-disk healing probe — and there is a clear sign of **recovery being attempted while live**: the disk-reconnect loop repeatedly probes the down drive on its own.

**Observed evidence (one-down state, `/tmp/minio.log`).**

Reconnect loop naming the endpoint by path (this *is* the live recovery attempt) — extracted with `grep -B3 -A6 "drive access denied" /tmp/minio.log` (the `API`/`Time`/`DeploymentID` lines precede the `Error:` line; intermediate stack frames trimmed for brevity):

```
API: SYSTEM.peers
Time: 03:31:09 UTC 07/01/2026
DeploymentID: 1f466ebb-ad9c-4a1e-b650-a4a9d3a883a9
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/mtest2/data2"
       2: .../cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: .../cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

Per-disk `Healing()` probe naming the full path — extracted with `grep -A6 "healing.bin: permission denied" /tmp/minio.log`:

```
Error: unable to read /tmp/mtest2/data2/.minio.sys/buckets/.healing.bin: open /tmp/mtest2/data2/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
       8: .../cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       7: .../cmd/xl-storage.go:352:cmd.newXLStorage.func2()
       4: .../cmd/xl-storage.go:781:cmd.(*xlStorage).DiskInfo()
       2: .../cmd/erasure.go:192:cmd.getDisksInfo.func1()
```

(Intermediate stack frames are trimmed; the `file:line:func` frames shown runtime-confirm the citations below.)

**Code.**

- The error string `drive access denied` is the storage constant `cmd/storage-errors.go:68` `var errDiskAccessDenied = StorageErr("drive access denied")` (siblings: `errUnformattedDisk` `:38`, `errDiskFull` `:47`, `errDiskNotFound` `:53`, `errFaultyDisk` `:65`).
- The reconnect log that names the endpoint: `cmd/prepare-storage.go:51` `peersLogAlwaysIf(ctx, err)`, inside `printEndpointError` (defined at `cmd/prepare-storage.go:35`), which tags the endpoint via `(&logger.ReqInfo{}).AppendTags("endpoint", endpoint.String())` (`cmd/prepare-storage.go:40`). It is invoked from `connectDisks` at `cmd/erasure-sets.go:230` `printEndpointError(endpoint, err, true)`.
- The full-path healing probe log: `cmd/xl-storage.go:436` `internalLogIf(GlobalContext, fmt.Errorf("unable to read %s: %w", healingFile, err))`, where `healingFile := pathJoin(s.drivePath, minioMetaBucket, bucketMetaPrefix, healingTrackerFilename)` (built at `cmd/xl-storage.go:431-432`; `func (s *xlStorage) Healing()` signature at `cmd/xl-storage.go:430`). The path components are `minioMetaBucket = ".minio.sys"` (`cmd/object-api-utils.go:60`), `bucketMetaPrefix = "buckets"` (`cmd/object-api-common.go:40`), and `healingTrackerFilename = ".healing.bin"` (`cmd/background-newdisks-heal-ops.go:41`) → `/tmp/mtest2/data2/.minio.sys/buckets/.healing.bin`. It is reached via `func (s *xlStorage) DiskInfo(...)` at `cmd/xl-storage.go:780` (whose `info, err = s.diskInfoCache.GetWithCtx(ctx)` is `:781`) and the DiskInfo cache closure at `cmd/xl-storage.go:352`.

**Rationale.** MinIO's periodic disk-info and reconnect probes touch the drive; when the POSIX `open` fails with `permission denied`, MinIO logs the exact path it tried, which is why the failing directory is named explicitly. The recurring `connectDisks` "drive access denied" entries are the live, automatic attempt to re-attach the drive while the system keeps serving traffic.

## Q5 — When the directory becomes accessible again, does MinIO self-detect via polling, or must something push it into healing?

**Answer.** MinIO **self-detects** the restored drive through its own periodic polling — **no external push or manual heal command is required**. For a permission-flip specifically, recovery is effectively **immediate** (measured ~0.01 s), because a `chmod` failure never fully tears down the local disk object.

**Observed evidence.**

Post-recovery write succeeds — command `python3 s3put.py put obj-postrecover.txt`:

```
PUT key=obj-postrecover.txt status=200 etag="d47ddcdb4c23bbe43bc3d054fb6f8f3a"
```

Timed cycle measured with this script (`/tmp/timing.py`), which drops below write quorum (`chmod 000` on two drives), polls `/minio/health/cluster` until it flips to `503`, then restores (`chmod 755`) and polls until it flips back to `200`:

```python
#!/usr/bin/env python3
import os, time, urllib.request, urllib.error
URL = "http://127.0.0.1:9000/minio/health/cluster"
D2, D3 = "/tmp/mtest2/data2", "/tmp/mtest2/data3"

def code():
    try:    return urllib.request.urlopen(URL, timeout=2).getcode()
    except urllib.error.HTTPError as e: return e.code
    except Exception: return 0

os.chmod(D2, 0o755); os.chmod(D3, 0o755)           # ensure healthy
while code() != 200: time.sleep(0.02)
t0 = time.time()                                   # DETECTION: drop below quorum
os.chmod(D2, 0); os.chmod(D3, 0)
while code() != 503: time.sleep(0.005)
det = time.time() - t0
t1 = time.time()                                   # RECOVERY: restore
os.chmod(D2, 0o755); os.chmod(D3, 0o755)
while code() != 200: time.sleep(0.005)
rec = time.time() - t1
print("DETECTION: /minio/health/cluster flipped 200 -> 503 after %.2fs" % det)
print("RECOVERY:  /minio/health/cluster flipped 503 -> 200 after %.2fs" % rec)
```

Running `python3 timing.py` (timings vary slightly run-to-run; detection tracks the 1-second DiskInfo cache TTL, recovery is near-instant) — a representative cycle measured:

```
DETECTION: /minio/health/cluster flipped 200 -> 503 after 1.04s
RECOVERY:  /minio/health/cluster flipped 503 -> 200 after 0.01s
```

A write-recovery cycle measured `PUT -> 200 after 0.01s` immediately after `chmod 755`.

Autonomous polling is visible in the log without any client action: the reconnect "drive access denied" entries recur on their own (e.g., `Time: 03:31:09` and `Time: 03:32:09`), and the `.healing.bin` disk-info probes recur at ~13–14 s intervals.

**Code (the self-detection pollers).**

- Reconnect poller: `cmd/erasure-sets.go:283` `func (s *erasureSets) monitorAndConnectEndpoints(ctx context.Context, monitorInterval time.Duration)`, started at `cmd/erasure-sets.go:479` `go s.monitorAndConnectEndpoints(ctx, defaultMonitorConnectEndpointInterval)`, at interval `cmd/erasure-sets.go:348` `const defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` (= 15 s); it calls `connectDisks` (`cmd/erasure-sets.go:194`).
- New-disk heal poller: `cmd/background-newdisks-heal-ops.go:40` `defaultMonitorNewDiskInterval = time.Second * 10`; `cmd/background-newdisks-heal-ops.go:563` `func monitorLocalDisksAndHeal(...)` → `cmd/background-newdisks-heal-ops.go:419` `func healFreshDisk(...)`, with the format re-init call `z.HealFormat(context.Background(), false)` at `cmd/background-newdisks-heal-ops.go:581`.
- Detection latency is bounded by the DiskInfo cache TTL: `cmd/xl-storage.go:326` `s.diskInfoCache.InitOnce(time.Second, ...)` (1 second).

**Rationale (stated honestly, including where it differs from the naive expectation).** A `chmod 000` makes I/O fail but does **not** cause MinIO to drop or disconnect the local `xlStorage` object, so the moment `chmod 755` restores access the very next probe/write succeeds — hence the near-instant (~0.01 s) recovery. The ~1.04 s detection latency is simply the 1-second DiskInfo cache refreshing. The dedicated 10 s (`monitorLocalDisksAndHeal`) and 15 s (`monitorAndConnectEndpoints`) pollers are the mechanisms that re-attach and heal a drive that was **fully dropped or replaced** (e.g., a brand-new or reformatted disk). Explicitly: the anticipated "~10 s" recovery was **not** observed for a permission flip; the measured value is near-instant, and the ~10 s / 15 s constants govern full re-integration of a dropped or fresh drive, not the permission-restore fast path.

## Q6 — For objects written while a disk was down, how are they repaired once the disk returns?

**Answer.** They are **automatically healed** — the shard missing on the returned drive is reconstructed from the surviving data + parity shards. This was directly observed: an object written while `data2` was down had no shard on `data2` after recovery, and the shard reappeared after the object was accessed.

**Observed evidence.**

Shard map immediately after recovery (before any access) — produced by this shell loop (a shard exists on a drive iff that drive holds an `xl.meta` for the object) plus an `ls` on the recovered drive:

```bash
for d in data1 data2 data3 data4; do
  [ -e "/tmp/mtest2/$d/testbucket/obj-1down.txt/xl.meta" ] && s=YES || s=no
  printf "%s=%s  " "$d" "$s"
done; echo
ls /tmp/mtest2/data2/testbucket/obj-1down.txt/
```

Output:

```
obj-1down.txt:  data1=YES  data2=no  data3=YES  data4=YES
# ls /tmp/mtest2/data2/testbucket/obj-1down.txt/  ->  No such file or directory
```

After a `GET` (`python3 s3put.py get obj-1down.txt` → `status=200 bytes=33`), the shard was reconstructed:

```
# ls /tmp/mtest2/data2/testbucket/obj-1down.txt/  ->  xl.meta
obj-1down.txt:  data1=YES  data2=YES  data3=YES  data4=YES
```

Heal integrity — measured per drive with `stat -c %s /tmp/mtest2/<d>/testbucket/obj-1down.txt/xl.meta` and `md5sum` on the same file: the healed `data2` `xl.meta` is `468` bytes, equal to the known-good `data1` `xl.meta` (`468` bytes). The per-drive `md5` legitimately differs — `data1=2756ef70...` vs `data2=1cc65bca...` — which is **expected**, because each drive's `xl.meta` stores that drive's own erasure shard; equal size plus a correct `bytes=33` read confirm a valid reconstruction.

**Code (the repair mechanisms).**

- Degraded writes are queued for repair **from the upload path itself**: while finishing a `PutObject`, `cmd/erasure-object.go:1566-1576` loops over the target disks and, if any disk "was initially or becomes offline during this upload", calls `er.addPartial(bucket, object, fi.VersionID)` at `cmd/erasure-object.go:1574`. That helper is `cmd/erasure-object.go:2112` `func (er erasureObjects) addPartial(bucket, object, versionID string)` → `globalMRFState.addPartialOp(PartialOperation{...})` (`cmd/erasure-object.go:2113`). This is the enqueue site for the object we wrote while `data2` was down.
- Distinct from the above, MinIO also enqueues heals from the **read** path when a `GET`/decode discovers a missing or corrupt shard: `cmd/erasure-object.go:400` (in `getObjectWithFileInfo`, `cmd/erasure-object.go:307`) calls `globalMRFState.addPartialOp(...)` after `erasure.Decode` if data blocks were missing, and `cmd/erasure-object.go:805` (in `getObjectFileInfo`, `cmd/erasure-object.go:705`) enqueues when reconstructable metadata is missing. These are read-triggered healing paths — **not** the degraded-write enqueue — and are exactly what healed the `data2` shard on the `GET` observed above.
- MRF (Most-Recent-Failures) queue: `cmd/mrf.go:78` `func (m *mrfState) addPartialOp(op PartialOperation)`; `cmd/mrf.go:220` `func (m *mrfState) healRoutine(z *erasureServerPools)`, which calls `healObject(...)` (`cmd/mrf.go:272` and `cmd/mrf.go:276`). Note: the `healObject` invoked by MRF is the free function `cmd/global-heal.go:591` `func healObject(bucket, object, versionID string, scan madmin.HealScanMode) error`, distinct from the `erasureObjects` method `cmd/erasure-healing.go:258` `func (er *erasureObjects) healObject(...)` (documented at `cmd/erasure-healing.go:257`, "Heals an object by re-writing corrupt/missing erasure blocks."); the exported entry point is `cmd/erasure-healing.go:1039` `func (er erasureObjects) HealObject(...)`, and the per-disk decision is `cmd/erasure-healing.go:156` `func shouldHealObjectOnDisk(...)`.
- A returned or replaced drive is also healed by the new-disk poller `cmd/background-newdisks-heal-ops.go:563` `monitorLocalDisksAndHeal` → `healFreshDisk` (`cmd/background-newdisks-heal-ops.go:419`).

**Rationale.** Because erasure coding retains enough shards (2 data + surviving parity) while a disk is down, MinIO can rebuild the missing shard deterministically. Repair happens through several complementary paths — read-triggered healing on access (observed above), the MRF queue that records exactly which objects were written while degraded, and the background new-disk scanner — all of them automatic.

## Q7 — Where does the quorum decision live in code, and how is the "enough disks vs stop" threshold calculated?

**Answer.** The per-object quorum decision lives in `objectQuorumFromMeta()` at **`cmd/erasure-metadata.go:531`**. For the four-drive `EC:2` set it computes **read quorum = 2** and **write quorum = 3**; the "enough vs stop" rule is: writes need ≥ 3 online drives, reads need ≥ 2.

**Observed evidence.** The computed write-quorum threshold is directly observable at runtime — no source reading required to confirm the number. The two lines below are re-quoted from evidence captured elsewhere in this document: the header was produced by `curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/cluster` (shown in full in [Q1](#q1--how-does-minio-decide-it-is-healthy-and-what-does-it-assume-about-the-required-number-of-disks)), and the below-threshold server log was extracted with `grep "Write quorum could not be established" /tmp/minio.log` (shown in [The quorum math](#the-quorum-math-consolidated)). Together they show the same value being enforced:

```
X-Minio-Write-Quorum: 3
Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2
```

**Code (the exact lines).**

`cmd/erasure-metadata.go:531` `func objectQuorumFromMeta(ctx context.Context, partsMetaData []FileInfo, errs []error, defaultParityCount int) (objectReadQuorum, objectWriteQuorum int, err error)`:

```go
dataBlocks := len(partsMetaData) - parityBlocks   // L555
                                                  //
writeQuorum := dataBlocks                         // L557
if dataBlocks == parityBlocks {                   // L558  (2 == 2)
    writeQuorum++                                 // L559  -> 3
}
                                                  //
return dataBlocks, writeQuorum, nil               // L564  -> (2, 3)
```

- `parityBlocks` originates from the default parity table `internal/config/storageclass/storage-class.go:355` `DefaultParityBlocks` (`case 4, 5: return 2`).
- The same "+1 when parity == data" rule is duplicated in the **multipart upload** path at `cmd/erasure-multipart.go:443-445` (`writeQuorum := dataDrives; if dataDrives == parityDrives { writeQuorum++ }`, inside `func (er erasureObjects) newMultipartUpload(...)` at `cmd/erasure-multipart.go:376`); in the **metacache-object** write path at `cmd/erasure-object.go:1116-1118` (inside `putMetacacheObject`, `cmd/erasure-object.go:1098` — an internal listing-cache object writer, not the multipart path); and in the **cluster-health** quorum computation at `cmd/erasure-server-pool.go:2722-2727`.

**Rationale.** Read quorum equals `dataBlocks` because you need at least the data-block count of shards to reconstruct the object. Write quorum is normally also `dataBlocks`, but when parity **equals** data (the `EC:2`-on-four-drives case, 2 == 2) MinIO bumps it by one (`writeQuorum++`) so a write must land on a strict majority (3 of 4). This prevents a split-brain where two half-sets each believe they hold the object. That single `if dataBlocks == parityBlocks { writeQuorum++ }` is the exact place the "enough disks to proceed vs. stop" threshold is set.

## Q8 — Ground the whole explanation in observable health-endpoint + actual write-attempt behavior while running.

**Answer.** Every claim above is anchored to observed `/minio/health/*` responses and real S3 `PUT`/`GET` attempts; the consolidated picture is the [Evidence matrix](#evidence-matrix) below, which reproduces the captured `curl` and S3 outcomes quoted in Q1–Q6.

**Observed evidence.** Rather than re-quote in full, this answer references the already-captured runtime output and its producing commands: the healthy-state `curl` headers with `X-Minio-Write-Quorum: 3`, produced by `curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/cluster` ([Q1](#q1--how-does-minio-decide-it-is-healthy-and-what-does-it-assume-about-the-required-number-of-disks)); the one-down `PUT ... status=200`, produced by `python3 s3put.py all obj-1down.txt` ([Q2](#q2--when-one-directory-becomes-inaccessible-during-operation-does-minio-adapt-and-keep-going-or-refuse-to-write)); and the decisive two-down side-by-side — the write attempt returns `503 SlowDownWrite` (produced by `python3 s3put.py put obj-2down.txt`) while the health probes disagree by concern. The three probe lines below were produced by `curl -s -o /dev/null -D - http://127.0.0.1:9000/minio/health/cluster` (and `/cluster/read`, `/live`), shown in full in [Q3](#q3--contrast-still-above-threshold-one-disk-lost-vs-below-threshold-a-second-disk-lost):

```
HTTP/1.1 503 Service Unavailable      (/minio/health/cluster; header still X-Minio-Write-Quorum: 3)
HTTP/1.1 200 OK                       (/minio/health/cluster/read)
HTTP/1.1 200 OK                       (/minio/health/live)
```

The full state-by-state summary is the [Evidence matrix](#evidence-matrix).

Two grounding points deserve explicit mention:

- `/minio/health/live` stays `200 OK` **even when the cluster is unwritable** — liveness is not the same as write-readiness. That endpoint is served by `cmd/healthcheck-handler.go:192` `func LivenessCheckHandler(...)`. This is why, in the two-down state, `/minio/health/cluster` is `503` while `/minio/health/live` remains `200`.
- The maintenance-mode `412` path exists but was **not** exercised by the permission scenario. It is `cmd/healthcheck-handler.go:83` `writeResponse(w, http.StatusPreconditionFailed, nil, mimeNone)`, taken only when the probe is called with `maintenance=true` and the node is unhealthy; the permission-loss experiment never sets that flag, so `412` was never observed.

The documented endpoint contract corroborating all of this is in `docs/metrics/healthcheck/README.md` (`/minio/health/cluster` returns `200` when it has write quorum and `503` when it has lost it; the `X-Minio-Write-Quorum` header; `/minio/health/cluster/read` for read quorum; `412` for maintenance).

**Code.** `cmd/healthcheck-handler.go:56` `ClusterCheckHandler` (writeable probe, sets `X-Minio-Write-Quorum` at `:72`, returns `200`/`503`/`412` at `:89`/`:85`/`:83`), `cmd/healthcheck-handler.go:93` `ClusterReadCheckHandler` (read probe), `cmd/healthcheck-handler.go:132` `ReadinessCheckHandler`, and `cmd/healthcheck-handler.go:192` `LivenessCheckHandler`.

**Rationale.** The health endpoints are a direct, external projection of the same internal quorum state that governs writes: `/minio/health/cluster` flips to `503` at exactly the moment writes begin returning `503 SlowDownWrite` (both driven by `online >= writeQuorum`), while `/minio/health/cluster/read` and `/minio/health/live` stay `200` because reads remain possible and the process is alive. That is why the observed endpoint behavior and the observed write behavior agree state-for-state.

## The quorum math (consolidated)

Tied directly to code:

- Four drives → `DefaultParityBlocks(4) = 2` → `EC:2` = 2 data + 2 parity (`internal/config/storageclass/storage-class.go:355`).
- `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`): `writeQuorum = dataBlocks` and `if dataBlocks == parityBlocks { writeQuorum++ }` → **read quorum = 2, write quorum = 3**.
- Therefore, by online-drive count:
  - 4 online → healthy (writes OK).
  - 3 online → still ≥ 3 → writes OK (**adapt**).
  - 2 online → < 3 → writes rejected with `503 SlowDownWrite`, but reads OK (≥ 2).
  - The header `X-Minio-Write-Quorum: 3` exposes the threshold to clients.
- Cluster `Health()` (`cmd/erasure-server-pool.go:2679`) applies the same per-pool quorums (`cmd/erasure-server-pool.go:2722-2727`, tested at `cmd/erasure-server-pool.go:2783`) and, when write quorum is unmet, logs (with the `maintenance="false"` tag) from `cmd/erasure-server-pool.go:2794`. The line below was extracted verbatim from the server log with `grep "Write quorum could not be established" /tmp/minio.log`:

```
Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2
```

## Evidence matrix

| State | Online | S3 PUT | S3 GET | `/health/cluster` | `/health/cluster/read` | `/health/live` |
|-------|--------|--------|--------|-------------------|------------------------|----------------|
| Healthy | 4 | `200` | `200` | `200` (`X-Minio-Write-Quorum: 3`) | `200` | `200` |
| One down (above threshold) | 3 | `200` | `200` | `200` | `200` | `200` |
| Two down (below threshold) | 2 | `503 SlowDownWrite` | `200` | `503` | `200` | `200` |
| Restored | 4 | `200` (≈0.01 s after `chmod 755`) | `200` | `200` | `200` | `200` |

## Coverage pass

- [x] Q1 — health/quorum decision + assumed disk count → §Q1 (+ [The quorum math](#the-quorum-math-consolidated))
- [x] Q2 — adapt vs refuse to write with one disk down → §Q2
- [x] Q3 — above-threshold vs below-threshold contrast → §Q3 (+ [Evidence matrix](#evidence-matrix))
- [x] Q4 — logs name the failing disk by path + live recovery attempt → §Q4
- [x] Q5 — self-detection via polling vs external push → §Q5
- [x] Q6 — repair of objects written while a disk was down → §Q6
- [x] Q7 — where the quorum decision lives + threshold calculation → §Q7 (+ [The quorum math](#the-quorum-math-consolidated))
- [x] Q8 — grounding in health-endpoint + write-attempt behavior → §Q8 (+ [Evidence matrix](#evidence-matrix))

All eight sub-questions are answered explicitly; none is omitted.

## Caveats and scope

- **Non-root requirement.** The server **must** run as a non-root user for `chmod 000` to take effect. A root process bypasses POSIX permission checks, so the "inaccessible directory" simulation would be a no-op and writes would still succeed. The investigation ran as `miniotest` (uid 1001); see the [Methodology](#methodology) verification (`ls: cannot open directory '/tmp/mtest2/data2': Permission denied`).
- **Single-node modeling.** The user's "distributed mode with four directories" is realized as a single-node, four-drive erasure set — the faithful local equivalent for observing per-set quorum behavior, since quorum is a per-erasure-set property. Standing up a multi-host cluster is unnecessary to answer these questions and is out of scope.
- **Honest limits (what differs from a naive expectation / could not be verified).**
  - The recovery time observed for a permission flip is **near-instant (~0.01 s)**, *not* the ~10 s one might expect from the new-disk poller. This is because a `chmod` failure does not tear down the local `xlStorage` object; the 10 s (`monitorLocalDisksAndHeal`) and 15 s (`monitorAndConnectEndpoints`) pollers govern fully-dropped or replaced drives, not the permission-restore fast path.
  - The `md5` of a healed `xl.meta` legitimately **differs** per drive (`data1=2756ef70...` vs `data2=1cc65bca...`) because each drive stores its own erasure shard; size equality (`468` bytes) plus a successful `bytes=33` read confirm the heal rather than an md5 match.
  - The maintenance-mode `412` path (`cmd/healthcheck-handler.go:83`) exists but was **not** exercised by the permission scenario, which never sets `maintenance=true`.
- **Read-only scope.** This document is the **only** repository change. MinIO source, tests, docs, configuration, and build files were consulted read-only as reference to ground the citations and were left byte-for-byte unchanged. All temporary build/run artifacts (the Go toolchain, the built binary, the observation scripts, the `/tmp` data directories, the captured logs, the module caches, and the test OS user) live outside the repository tree and were removed afterward; `git status` showed the repository unchanged apart from this document.
- **Versions.** Built and run with **Go 1.23.2** (the project pins `go 1.23` in `go.mod`); the server banner reports version `DEVELOPMENT.GOGET (go1.23.2 linux/amd64)`.

