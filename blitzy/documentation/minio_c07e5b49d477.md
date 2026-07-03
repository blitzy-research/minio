# MinIO Drive-Failure Behavior at Runtime — A Four-Directory Erasure Investigation

> **Scope of this document.** This is an *evidence-grounded* answer to nine questions about how MinIO behaves when erasure-coded drives become inaccessible at runtime in a **single-node, four-directory** deployment. Every behavioral claim below sits next to the **verbatim output** that demonstrates it, and every code claim carries an exact **`file:line`** citation. All observations were produced by **building and running the actual MinIO binary**, injecting permission faults against live data directories, and exercising the **real S3 write path** and the **real `/minio/health/*` HTTP endpoints** — not by reading the source alone. The MinIO source tree was treated as read-only; nothing in it was modified.
>
> Repository: `github.com/minio/minio` · HEAD `c07e5b49d477b0774f23db3b290745aef8c01bd2` · build banner `DEVELOPMENT.2024-11-25T17-10-22Z`.

---

## TL;DR

- **Topology.** Four directories become **one erasure set of four drives** with **default parity 2** (data 2 + parity 2). Observed at startup: `INFO: Formatting 1st pool, 1 set(s), 4 drives per set.`
- **Quorum.** From the code, this yields **read quorum = 2** and **write quorum = 3**. Write quorum is `dataBlocks` (2) **plus one** because `dataBlocks == parityBlocks` — a split-brain guard [`cmd/erasure-metadata.go:557-560`]. Confirmed on the wire: `X-Minio-Write-Quorum: 3`, `X-Minio-Read-Quorum: 2`.
- **Lose one directory (3 online ≥ write quorum 3).** MinIO **adapts and keeps writing**. A real `PutObject` returned **HTTP 200**; the health endpoint stayed **200**; the failing disk was logged **by path**.
- **Lose a second directory (2 online < write quorum 3).** MinIO **draws a hard line**: a real `PutObject` was **refused with HTTP 503 `SlowDownWrite`**, while **reads still succeeded** (2 online ≥ read quorum 2). `GET /minio/health/cluster` flipped to **503** but `GET /minio/health/cluster/read` stayed **200**.
- **Path logging.** Yes — disk errors name the failing directory directly, e.g. `endpoint="/tmp/minio-obs.zvAv/data1"`, via `printEndpointError` [`cmd/prepare-storage.go:35-72`].
- **Recovery is self-driven by polling.** When permissions are restored, a background loop re-admits the drive **on its own** and **pushes** it into a healing path. The reconnect poll interval is **runtime-confirmed ≈15.0 s** (two runs, 10 intervals, 15.000–15.002 s, mean 15.001 s), matching the code constant `10s + 5s` [`cmd/erasure-sets.go:348`], [`cmd/background-newdisks-heal-ops.go:40`].
- **Stale objects are repaired.** An object written while a drive was down had its missing shard **reconstructed onto the returned drive** — proven by before/after on-disk inspection — via the MRF (Most-Recent-Failures) queue and background healing [`cmd/mrf.go:78,220`], [`cmd/erasure-healing.go:258`].

---

## Table of Contents

1. [Environment & Methodology](#1-environment--methodology)
2. [Q1 — How MinIO decides/reports "healthy" and its disk assumption](#q1--how-minio-decidesreports-healthy-and-its-disk-assumption)
3. [Q2 — Adapt vs. refuse at the moment a directory becomes inaccessible](#q2--adapt-vs-refuse-at-the-moment-a-directory-becomes-inaccessible)
4. [Q3 — Both scenarios: above vs. below the threshold](#q3--both-scenarios-above-vs-below-the-threshold)
5. [Q4 — Does the log name the failing disk by path?](#q4--does-the-log-name-the-failing-disk-by-path)
6. [Q5 — Signs of recovery while the system is live](#q5--signs-of-recovery-while-the-system-is-live)
7. [Q6 — Self-recognition (polling) vs. being pushed into healing](#q6--self-recognition-polling-vs-being-pushed-into-healing)
8. [Q7 — Repair of objects written while a disk was down](#q7--repair-of-objects-written-while-a-disk-was-down)
9. [Q8 — Where the quorum decision lives and how the threshold is computed](#q8--where-the-quorum-decision-lives-and-how-the-threshold-is-computed)
10. [Q9 — Empirical grounding (health endpoint + real writes)](#q9--empirical-grounding-health-endpoint--real-writes)
11. [Corroboration (secondary, non-authoritative)](#corroboration-secondary-non-authoritative)
12. [Final coverage checklist](#final-coverage-checklist)

---

## 1. Environment & Methodology

**Host.** `Linux 6.6.122+ x86_64 GNU/Linux`, Ubuntu 25.10. Single Go module `github.com/minio/minio` (`go 1.23` [`go.mod:3`]); toolchain `go1.23.12 linux/amd64`.

**Document name & source branch (provenance).** This file's name is derived from the repository's **source branch**. Authoring is performed on an isolated checkpoint branch that cannot be switched during the run, so `git rev-parse --abbrev-ref HEAD` reports that checkpoint branch; the source branch is resolved from its remote ref. Both were captured **verbatim**:

```bash
$ git rev-parse --abbrev-ref HEAD
blitzy-1b68cc1d-1444-4dcf-ad3e-a7949f2d66f0
$ git for-each-ref --format='%(refname:short)' refs/remotes/origin/minio_c07e5b49d477
origin/minio_c07e5b49d477
$ git rev-parse origin/minio_c07e5b49d477
c07e5b49d477b0774f23db3b290745aef8c01bd2
$ basename "$(git for-each-ref --format='%(refname:short)' refs/remotes/origin/minio_c07e5b49d477)"
minio_c07e5b49d477
```

The source branch (its `origin/` remote prefix removed) is **`minio_c07e5b49d477`** — the stem of this file's name — so the deliverable is **`blitzy/documentation/minio_c07e5b49d477.md`**. Its tip commit `c07e5b49d477b0774f23db3b290745aef8c01bd2` is exactly the `commit-id` in the version banner below, confirming the running binary and this document describe the same source revision.

**Build — canonical, default configuration.** The binary was built with the repository's own recipe [`Makefile:177` `build:` target; `Makefile:179` recipe; `Makefile:3` `LDFLAGS`]. Exact command executed:

```bash
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(go run buildscripts/gen-ldflags.go)" -o ./minio
```

Produced a 117,293,208-byte (~112 MiB) binary whose version banner (`./minio --version`) is quoted **verbatim**:

```
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

**Run — four-directory erasure server.** The four directories were passed as a single erasure set. The startup log confirms the topology **verbatim**:

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
```

> **Why the server runs as a non-root user.** Fault injection uses `chmod 000` on a data directory to make it inaccessible. The Linux **root** user bypasses permission bits (`CAP_DAC_OVERRIDE`), so `chmod 000` would *not* block a root-owned server — verified directly (root read a `chmod 000` file successfully; a non-root user got `Permission denied (os error 13)`). The server was therefore launched as the non-root user `miniotester` so the permission fault genuinely bites. The `chmod` operations themselves are performed as root (always permitted); the HTTP and S3 clients run as root against `127.0.0.1:9000`. Invocation (default config, default credentials):

```bash
# $OBS is a unique temp workspace: OBS=$(mktemp -d /tmp/minio-obs.XXXX); chown -R miniotester "$OBS"
su -s /bin/bash miniotester -c \
  'MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
   ./minio server $OBS/data1 $OBS/data2 $OBS/data3 $OBS/data4 --address :9000 \
   > $OBS/minio.log 2>&1 & echo $!'
```

**Real entry points only.**

- **Health** is observed through the real, unauthenticated HTTP routes under `/minio/health` [`cmd/healthcheck-router.go:27-31`, `41-52`], using `curl -sS -i`.
- **Writes/reads** are performed through the **real S3 API** using the `boto3` SDK (v1.43.39) with SigV4 request signing, path-style addressing, endpoint `http://127.0.0.1:9000`, and the default root credentials `minioadmin:minioadmin`. This is a genuine S3 client exercising the same `PutObject`/`GetObject` handlers a production client would hit. No admin bypass or internal test hook was used.

**S3 helper script (`s3op.py`).** Every `PUT_OK` / `GET_OK` / `S3_ERROR` line quoted below was produced by this single throwaway script. `boto3`/`botocore` (v1.43.39) are an **external, host-side observation tool — not a repository dependency**; nothing is added to `go.mod`, and the script lives outside the repository tree and is removed during cleanup. It uses a **deterministic 1 MiB payload**, so the content-MD5 `ETag` is reproducible across runs and identical for every object written here (`d9122db2eede81390065782dbabc5aba`):

```python
#!/usr/bin/env python3
# s3op.py -- exercise MinIO's REAL S3 write/read path with boto3 (SigV4, path-style).
# Not a repository dependency; a host-side observation tool only.
# Usage:
#   s3op.py mb  <bucket>
#   s3op.py put <bucket> <key>        # writes a deterministic 1 MiB object
#   s3op.py get <bucket> <key>        # reads it back, prints byte count
import sys
import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

ENDPOINT = "http://127.0.0.1:9000"
ACCESS, SECRET = "minioadmin", "minioadmin"
# Deterministic 1 MiB payload so the ETag (content MD5) is reproducible across runs.
PAYLOAD = (bytes(range(8)) * (1048576 // 8))   # 1048576 bytes, fixed content

def client():
    return boto3.client(
        "s3", endpoint_url=ENDPOINT,
        aws_access_key_id=ACCESS, aws_secret_access_key=SECRET,
        config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
        region_name="us-east-1",
    )

def main():
    op, bucket = sys.argv[1], sys.argv[2]
    c = client()
    try:
        if op == "mb":
            c.create_bucket(Bucket=bucket)
            print(f"MB_OK {bucket}")
        elif op == "put":
            key = sys.argv[3]
            r = c.put_object(Bucket=bucket, Key=key, Body=PAYLOAD)
            status = r["ResponseMetadata"]["HTTPStatusCode"]
            etag = r["ETag"].strip('"')
            print(f'PUT_OK {key} HTTP {status} ETag "{etag}"')
        elif op == "get":
            key = sys.argv[3]
            r = c.get_object(Bucket=bucket, Key=key)
            status = r["ResponseMetadata"]["HTTPStatusCode"]
            body = r["Body"].read()
            print(f"GET_OK {key} HTTP {status} BYTES {len(body)}")
    except ClientError as e:
        rm = e.response.get("ResponseMetadata", {})
        err = e.response.get("Error", {})
        print(f"S3_ERROR action={op} HTTP={rm.get('HTTPStatusCode')} "
              f"Code={err.get('Code')} Message={err.get('Message')}")
        sys.exit(0)

if __name__ == "__main__":
    main()
```

The invocation pattern (repeated next to each result below) is:

```bash
$ python3 s3op.py mb  bucket             # create the bucket once
$ python3 s3op.py put bucket obj-A.bin    # PutObject (1 MiB)  -> PUT_OK … / S3_ERROR …
$ python3 s3op.py get bucket obj-A.bin    # GetObject          -> GET_OK … / S3_ERROR …
```

**Fault injection.** A directory is made inaccessible with `chmod 000 <dir>` and restored with `chmod 755 <dir>`, matching the user's "permission changes" scenario. Data is never deleted.

**Timing method (magnitude rule).** To measure the disk-reconnect poll interval directly, the server was (for that measurement only) started with `_MINIO_SERVER_DEBUG=on` [`cmd/common-main.go:67`], which surfaces the internal monitor tick `console.Debugln("running drive monitoring")` [`cmd/erasure-sets.go:300`]. This toggles **logging verbosity only**; it does not change the polling behavior, and the interval itself is a fixed code constant. The inter-tick spacing was measured across **two independent windows**.

**Reproducibility & cleanup.** Every value below is reproducible from the commands above. After the investigation, the server was stopped by its explicit PID, and the built binary, the temporary `$OBS` workspace (data directories, logs, and helper scripts) were removed, leaving the source tree byte-for-byte unchanged and this document as the only added file. Because the built binary is git-ignored, artifact removal is proved with explicit absence checks (not `git status` alone); the full cleanup proof — absence checks, the `.gitignore:4` caveat, the source-revision diff, and the clean working tree — is shown at the end of [§Q9](#q9--empirical-grounding-health-endpoint--real-writes).


---

## Q1 — How MinIO decides/reports "healthy" and its disk assumption

**Direct answer.** MinIO decides cluster health by **counting how many drives are in the `OK` state and comparing that count against a per-pool write quorum and read quorum**. For a four-drive set the assumption is concrete: it needs **≥ 3 drives online to be *write*-healthy** and **≥ 2 drives online to be *read*-healthy**. It reports this over the unauthenticated HTTP endpoints `/minio/health/cluster` (write side) and `/minio/health/cluster/read` (read side), and it advertises the thresholds themselves in response headers.

**How it works (code).** The decision lives in `erasureServerPools.Health()` [`cmd/erasure-server-pool.go:2679`]:

- It counts online drives — `if disk.State == madmin.DriveStateOk { si.online++ }` [`cmd/erasure-server-pool.go:2707-2709`].
- It derives per-pool quorums from the backend storage-class data/parity — `poolReadQuorums[i] = data`, `poolWriteQuorums[i] = data`, then `if data == b.StandardSCParity { poolWriteQuorums[i] = data + 1 }` [`cmd/erasure-server-pool.go:2722-2727`]. For four drives the backend is `StandardSCData = setDriveCount - scParity = 2` [`cmd/erasure-server-pool.go:694`] and `StandardSCParity = scParity = 2` [`cmd/erasure-server-pool.go:700`] ⇒ read quorum 2, write quorum **2 + 1 = 3**.
- It sets the verdict per set — `Healthy: … online >= poolWriteQuorums[poolIdx]` [`cmd/erasure-server-pool.go:2783`] and `HealthyRead: … online >= poolReadQuorums[poolIdx]` [`cmd/erasure-server-pool.go:2784`].

The handler `ClusterCheckHandler` [`cmd/healthcheck-handler.go:56`] publishes the write quorum in a header — `w.Header().Set(xhttp.MinIOWriteQuorum, strconv.Itoa(result.WriteQuorum))` [`cmd/healthcheck-handler.go:72`] — and returns **200** when healthy [`:89`], **503** when not [`:85`]. The header constant is lowercase in source — `MinIOWriteQuorum = "x-minio-write-quorum"` [`internal/http/headers.go:193`] — and Go's `net/http` canonicalizes it on the wire to `X-Minio-Write-Quorum`. The read side is symmetric: `ClusterReadCheckHandler` [`cmd/healthcheck-handler.go:93`] sets `X-Minio-Read-Quorum` (`MinIOReadQuorum = "x-minio-read-quorum"` [`internal/http/headers.go:196`]) [`:109`] and keys its status off `result.HealthyRead` [`:115`].

**Evidence (verbatim, healthy four-drive instance).** `GET /minio/health/cluster` → **200**, advertising write quorum 3:

```
HTTP/1.1 200 OK
Server: MinIO
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
```

`GET /minio/health/cluster/read` → **200**, advertising read quorum 2:

```
HTTP/1.1 200 OK
Server: MinIO
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
```

`GET /minio/health/live` → **200** and `GET /minio/health/ready` → **200** (both quoted at [§Q9](#q9--empirical-grounding-health-endpoint--real-writes)).

**Reported exactly as observed.** The header `X-Minio-Storage-Class-Defaults: false` appears even though parity **is** the default 2. That flag reflects `UsingDefaults`, which is `true` only when the storage-class config was never initialized (`globalStorageClass.GetParityForSC(STANDARD) < 0`); on this running server the config is initialized to the concrete default value 2, so the flag is `false`. The write/read quorum values (3 and 2) still confirm default parity 2.

---

## Q2 — Adapt vs. refuse at the moment a directory becomes inaccessible

**Direct answer.** It does **both — the behavior is threshold-gated**. At the moment a directory becomes inaccessible, MinIO **quietly adapts and keeps writing** *as long as the number of still-online drives is ≥ the write quorum (3)*. The instant the online count drops **below** the write quorum, it **draws a hard line and refuses writes** (returning an S3 `SlowDownWrite` / HTTP 503), while continuing to serve reads as long as read quorum (2) is still met. In other words, losing the **first** of four directories is absorbed silently; losing the **second** trips the write-quorum guard and writes stop.

**Why (cause → effect).** A write must land enough shards to satisfy `writeQuorum = 3` [`cmd/erasure-metadata.go:557-560`]. With one directory down, three shard writes still succeed → quorum met → the write is accepted. With two directories down, only two shard writes can succeed → quorum **not** met → the write path returns `errErasureWriteQuorum` = `"Write failed. Insufficient number of drives online"` [`cmd/erasure-errors.go:26`], which the S3 layer maps to `ErrSlowDownWrite` [`cmd/api-errors.go:2192-2193`] (HTTP 503). Reads only need `readQuorum = 2`, so they survive the second failure. The two scenarios and their verbatim evidence are in [§Q3](#q3--both-scenarios-above-vs-below-the-threshold).

**Evidence (the pivot, verbatim and adjacent).** The two identical real `PutObject` calls straddling the threshold, quoted directly:

```
# one directory down (3 online >= write quorum 3) -> ADAPTS, keeps writing:
$ python3 s3op.py put bucket obj-A.bin
PUT_OK obj-A.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"

# a second directory down (2 online < write quorum 3) -> HARD LINE, refuses:
$ python3 s3op.py put bucket obj-B.bin
S3_ERROR action=put HTTP=503 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate
```

Reads continue across the pivot (read quorum 2 still met): `GET_OK obj-healthy.bin HTTP 200 BYTES 1048576`. The full per-scenario captures (health headers and quorum logs) follow in [§Q3](#q3--both-scenarios-above-vs-below-the-threshold).


---

## Q3 — Both scenarios: above vs. below the threshold

### Scenario A — one directory lost (3 online ≥ write quorum 3): writes CONTINUE

**Setup.** `chmod 000 $OBS/data1` while the server ran. Three drives (`data2`, `data3`, `data4`) remained online.

**Observed — real S3 write SUCCEEDS:**

```
PUT_OK obj-A.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
```

**Observed — read of the object also succeeds:**

```
GET_OK obj-A.bin HTTP 200 BYTES 1048576
```

**Observed — health endpoints stay green** (online 3 ≥ write quorum 3, exactly at the threshold):

```
# GET /minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
# GET /minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
```

**Why.** `writeQuorum = 3` is met by the three surviving shard writes, so the real `PutObject` commit step — `renameData(…, writeQuorum)` [`cmd/erasure-object.go:1535`] → `reduceWriteQuorumErrs(…, writeQuorum)` [`cmd/erasure-object.go:1054`] — does not trip the sentinel. See [§Q8](#q8--where-the-quorum-decision-lives-and-how-the-threshold-is-computed).

### Scenario B — a second directory lost (2 online < write quorum 3): writes REFUSED, reads CONTINUE

**Setup.** `chmod 000 $OBS/data2` as well. Now only two drives (`data3`, `data4`) are online.

**Observed — real S3 write is REFUSED (verbatim client error):**

```
S3_ERROR action=put HTTP=503 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate
```

This is exactly the internal sentinel `errErasureWriteQuorum` = `"Write failed. Insufficient number of drives online"` [`cmd/erasure-errors.go:26`] mapped to `ErrSlowDownWrite` [`cmd/api-errors.go:2192-2193`], whose definition is `Code: "SlowDownWrite"`, `Description: "Resource requested is unwritable, please reduce your request rate"`, `HTTPStatusCode: http.StatusServiceUnavailable` (503) [`cmd/api-errors.go:874-878`].

**Observed — reads STILL succeed (2 online ≥ read quorum 2):**

```
GET_OK obj-healthy.bin HTTP 200 BYTES 1048576
```

**Observed — health split: write side 503, read side 200:**

```
# GET /minio/health/cluster        -> not write-healthy
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3
# GET /minio/health/cluster/read   -> still read-healthy
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
# GET /minio/health/live           -> liveness is independent of quorum
HTTP/1.1 200 OK
```

**Observed — the server logs the quorum shortfall with the exact online count (verbatim):**

```
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
```

This matches the format string at `cmd/erasure-server-pool.go:2794`. **Reported exactly as observed:** although this line is emitted via `storageLogIf(…, logger.FatalKind)` [`cmd/erasure-server-pool.go:2795`], the server **did not crash** — the process stayed alive and continued serving reads and health requests. `FatalKind` here is a log-severity classification, not an `os.Exit`.

**Net edge case demonstrated.** Below write quorum but at/above read quorum, MinIO is a *read-only, degraded* store: it refuses new writes yet keeps serving existing objects. This is the specific behavior to internalize before relying on it for fault tolerance.

---

## Q4 — Does the log name the failing disk by path?

**Direct answer.** **Yes.** Disk errors are tagged with the failing drive's **endpoint path**, so the log points straight at the directory that went bad.

**How it works (code).** `printEndpointError` [`cmd/prepare-storage.go:35`] attaches the path as a request-info tag — `reqInfo := (&logger.ReqInfo{}).AppendTags("endpoint", endpoint.String())` [`cmd/prepare-storage.go:40`] — and emits the error via `peersLogAlwaysIf(ctx, err)` [`cmd/prepare-storage.go:51`]; a repeated error is rate-annotated with `"Following error has been printed %d times.. %w"` [`cmd/prepare-storage.go:63`]. It is invoked from `connectDisks` [`cmd/erasure-sets.go:230`] (and `:244`) as MinIO (re)connects drives.

**Evidence (verbatim, after `chmod 000 $OBS/data1`).** The failing directory is named by full path in the `endpoint=` tag, and the stack trace ends exactly at the `printEndpointError` call site:

```
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/minio-obs.zvAv/data1"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

The permission fault surfaces the storage sentinel **`errDiskAccessDenied` = `StorageErr("drive access denied")`** [`cmd/storage-errors.go:68`] (comment L67: "we don't have write permissions on disk"). Internal read paths also name the path directly, e.g. `unable to read /tmp/minio-obs.zvAv/data1/.minio.sys/buckets/.healing.bin: … permission denied` from `xlStorage.Healing()`.

**The full family of disk-error literals** that surface in logs (each an exact string):

| Literal | String value | `file:line` |
|---|---|---|
| `errDiskAccessDenied` | `"drive access denied"` | `cmd/storage-errors.go:68` |
| `errUnformattedDisk` | `"unformatted drive found"` | `cmd/storage-errors.go:38` |
| `errDiskNotFound` | `"drive not found"` | `cmd/storage-errors.go:53` |
| `errDriveIsRoot` | `"drive is part of root drive, will not be used"` | `cmd/storage-errors.go:59` |
| `errFaultyDisk` | `"drive is faulty"` | `cmd/storage-errors.go:65` |

> Note: the specific literal observed for a `chmod 000` permission fault is `errDiskAccessDenied` ("drive access denied"). The `errUnformattedDisk`/`errDiskNotFound`/`errDriveIsRoot`/`errFaultyDisk` literals cover the other drive-failure modes (fresh/returned drive, missing mount, root-partition guard, and I/O-faulty drive respectively) and are all routed through the same path-tagged logger.

---

## Q5 — Signs of recovery while the system is live

**Direct answer.** **Yes — recovery is visibly attempted while the server stays up**, and the strongest signals are visible under **default logging** with no diagnostic flags. Two background loops run continuously without any restart: a **drive-reconnect monitor** and a **heal task queue**. When a drive returns, the monitor re-admits it and heal tasks are enqueued for the affected objects — all while the server keeps answering health checks and reads.

**How it works (code).** The reconnect monitor `monitorAndConnectEndpoints` [`cmd/erasure-sets.go:283`] runs on a timer and calls `connectDisks(true)` [`cmd/erasure-sets.go:303`] each tick; the fresh/returned-disk healer `monitorLocalDisksAndHeal` [`cmd/background-newdisks-heal-ops.go:563`] runs on its own 10 s timer [`cmd/background-newdisks-heal-ops.go:565`]; and queued heal work is dispatched through `globalBackgroundHealRoutine.tasks`.

**Evidence — default-visible signals (no diagnostic flags; `curl` + `boto3` only).** These are what a normal operator sees. First, while the drive is down, the default log names the recovery target **directly by path** (from `printEndpointError` [`cmd/prepare-storage.go:35-72`]):

```
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/minio-obs.zvAv/data1"
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

Second, recovery is observable purely through the **real health endpoint and real writes**: starting from a below-quorum state (`data1`+`data2` down → `GET /minio/health/cluster` returns `503` and a PUT is refused with `HTTP 503 SlowDownWrite`), simply restoring one directory brings both back **on their own**, with no restart and no command. Measuring from `chmod 755 <dir>`:

```
$ chmod 755 /tmp/minio-obs.zvAv/data1
health cluster -> 200 after 0.02s
write resumed after 0.31s: PUT_OK obj-D.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
```

The `obj-D.bin` shard then landed on the just-restored `data1`, confirming the drive was re-admitted for live I/O without intervention.

**Evidence — diagnostic (non-default) observability (`_MINIO_SERVER_DEBUG=on`).** The two lines below are emitted **only** when the server is started with `_MINIO_SERVER_DEBUG=on` [`cmd/common-main.go:67`]; both are wrapped in `if serverDebugLog { … }` and are **not** part of default log output. They are shown here as diagnostic confirmation of the internal mechanism, not as default runtime behavior. The reconnect monitor ticking (`console.Debugln("running drive monitoring")` gated at [`cmd/erasure-sets.go:299-300`]):

```
minio: <DEBUG> running drive monitoring
```

A heal task **queued** for the exact object that had been written while `data1` was down (`fmt.Printf("Task in the queue: %#v\n", task)` gated at [`cmd/admin-heal-ops.go:740-741`]), captured ~5 s after restoring `data1`:

```
Task in the queue: cmd.healTask{bucket:"bucket2", object:"obj-h2.bin", versionID:"", opts:madmin.HealOpts{Recursive:false, DryRun:false, Remove:true, Recreate:false, ScanMode:0, UpdateParity:false, NoLock:false, Pool:(*int)(nil), Set:(*int)(nil)}, respCh:(chan cmd.healResult)(0xc001320000)}
```

That heal completed: `obj-h2.bin`'s shard reappeared on the returned `data1` (PRESENT on all four drives afterward).

**What was and was not visible under default logging.** Under default logging (no `_MINIO_SERVER_DEBUG`), the **path-tagged disk error** and the **health/write recovery** above are fully visible; the **per-tick monitor line** and the **per-task heal-enqueue struct** are **not** — they require `_MINIO_SERVER_DEBUG=on`. Enabling that flag only changes logging verbosity (both lines sit behind `if serverDebugLog`); it does not alter the reconnect or heal behavior itself.


---

## Q6 — Self-recognition (polling) vs. being pushed into healing

**Direct answer.** It is **both**: MinIO **self-recognizes** a returned drive through a **polling loop** (no external trigger needed), and that same loop then **pushes** the drive into a healing path. You do not have to restart the server or issue any command — leaving the drive accessible again is enough.

**How it works (code).**

- **Polling (self-recognition):** `monitorAndConnectEndpoints` [`cmd/erasure-sets.go:283`] loops forever, calling `connectDisks(true)` [`cmd/erasure-sets.go:303`] on every tick and resetting its timer [`cmd/erasure-sets.go:306`]. It is launched once at startup — `go s.monitorAndConnectEndpoints(ctx, defaultMonitorConnectEndpointInterval)` [`cmd/erasure-sets.go:479`].
- **Push into healing:** inside `connectDisks`, a reconnected local drive that is unformatted or has unfinished healing is pushed onto the heal queue — `globalBackgroundHealState.pushHealLocalDisks(endpoint)` [`cmd/erasure-sets.go:227`] and `pushHealLocalDisks(disk.Endpoint())` [`cmd/erasure-sets.go:238`]. The fresh/returned-disk healer `monitorLocalDisksAndHeal` [`cmd/background-newdisks-heal-ops.go:563`] then drains that queue.

**The interval (magnitude — runtime-confirmed).** The poll interval is the code constant `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` [`cmd/erasure-sets.go:348`], where `defaultMonitorNewDiskInterval = time.Second * 10` [`cmd/background-newdisks-heal-ops.go:40`] — i.e. **code-derived ≈ 15 s**. This was **runtime-confirmed** by capturing every `"running drive monitoring"` tick [`cmd/erasure-sets.go:300`] across **two independent ~95 s windows**. Because `console.Debugln` prints no built-in timestamp, each tick was arrival-timestamped externally (`date -u +%H:%M:%S.%3N`) as it was read from the log. The **raw captured ticks** were:

```
# Run 1 (fresh server, _MINIO_SERVER_DEBUG=on)
01:18:38.180 minio: <DEBUG> running drive monitoring
01:18:53.180 minio: <DEBUG> running drive monitoring
01:19:08.182 minio: <DEBUG> running drive monitoring
01:19:23.183 minio: <DEBUG> running drive monitoring
01:19:38.183 minio: <DEBUG> running drive monitoring
01:19:53.183 minio: <DEBUG> running drive monitoring
# Run 2 (second fresh server)
01:20:28.155 minio: <DEBUG> running drive monitoring
01:20:43.155 minio: <DEBUG> running drive monitoring
01:20:58.156 minio: <DEBUG> running drive monitoring
01:21:13.157 minio: <DEBUG> running drive monitoring
01:21:28.157 minio: <DEBUG> running drive monitoring
01:21:43.158 minio: <DEBUG> running drive monitoring
```

The inter-tick **deltas below are computed from those quoted raw timestamps** (consecutive differences):

| Run | Ticks | Deltas (s), derived from raw timestamps above | Mean (derived) |
|---|---|---|---|
| 1 | 6 | 15.000, 15.002, 15.001, 15.000, 15.000 | 15.001 |
| 2 | 6 | 15.000, 15.001, 15.001, 15.000, 15.001 | 15.001 |

Across both runs, **10 intervals** span **min 15.000 s, max 15.002 s, mean 15.001 s** — stable across the two runs and matching the code-derived 15 s (`10 s + 5 s`).

**Reported exactly as observed — an important nuance.** The *health endpoint's* view of drives recovers **much faster** than this ~15 s poll, and this is measurable under **default logging**. Measuring from `chmod 755 <dir>`, `GET /minio/health/cluster` returned `200` again after **0.02 s** and a real S3 write resumed after **0.31 s** (`PUT_OK obj-D.bin`, see [§Q5](#q5--signs-of-recovery-while-the-system-is-live)). That is because `Health()` re-derives the online count from fresh per-drive `DiskInfo` probes, which are independent of the ~15 s reconnect poll. So there are two distinct cadences: the health endpoint reflects drive accessibility sub-second (default-visible), whereas the drive-reconnect monitor sweeps every ~15 s (a cadence visible only via the diagnostic `_MINIO_SERVER_DEBUG=on` tick). Both were observed; neither was assumed.

**Evidence — diagnostic tick (`_MINIO_SERVER_DEBUG=on`).** The self-recognition poll is confirmed by the monitor tick, recurring every ~15 s. This line is **diagnostic-only** (gated by `if serverDebugLog` at [`cmd/erasure-sets.go:299-300`]) and is **not** emitted under default logging:

```
minio: <DEBUG> running drive monitoring
```

**Evidence — default-visible recovery (no diagnostic flags; `curl` + `boto3`).** After restoring permissions (no restart, no command), a real S3 write resumed and health returned to green on its own:

```
$ python3 s3op.py put bucket obj-D.bin
PUT_OK obj-D.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
# GET /minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
```

---

## Q7 — Repair of objects written while a disk was down

**Direct answer.** They are repaired by **healing that reconstructs the missing shard onto the returned drive**. Two complementary mechanisms drive this: (i) the **MRF (Most-Recent-Failures) queue**, which records writes that couldn't reach every drive and heals those specific objects as soon as the drive is back; and (ii) **fresh/returned-disk healing**, which sweeps a returned drive and heals what it is missing. Repair was demonstrated directly: an object written while a drive was down had **no shard on that drive**, and after the drive returned the shard **appeared** — reconstructed from the surviving shards.

**How it works (code).**

- **MRF queue.** A partial write (one or more drives offline) is recorded via `globalMRFState.addPartialOp(PartialOperation{…})` — comment "Add a partial S3 operation (put/delete) when one or more disks are offline" [`cmd/mrf.go:77`], method `addPartialOp` [`cmd/mrf.go:78`]. Write-path feed points: `cmd/erasure-object.go:400` (guarded by `healOnce.Do` on `errFileNotFound`/`errFileCorrupt` [`:398-399`]), `:805`, `:1578`, `:2113`, plus multipart `cmd/erasure-multipart.go:1409`. The consumer `healRoutine` [`cmd/mrf.go:220`] reads `m.opCh` [`cmd/mrf.go:225`] and issues heal requests; queue persistence is `startMRFPersistence` [`cmd/mrf.go:155`].
- **Fresh/returned-disk healing.** `monitorLocalDisksAndHeal` [`cmd/background-newdisks-heal-ops.go:563`] gets the queued endpoints via `getHealLocalDiskEndpoints` [`:573`], reformats via `HealFormat` [`:581`], and heals each via `healFreshDisk` [`:592`] (auto-heal init `initAutoHeal` [`:377`], candidate list `getLocalDisksToHeal` [`:393`]).
- **Per-object reconstruction.** `healObject` [`cmd/erasure-healing.go:258`] ("Heals an object by re-writing corrupt/missing erasure blocks") recomputes the quorum via `objectQuorumFromMeta(…)` [`cmd/erasure-healing.go:307`] and rebuilds missing shards; the admin entry point is `HealObject` [`cmd/erasure-healing.go:1039`].

**Evidence — the write itself (real S3 path, `data1` down).** With only `data1` inaccessible (three drives up ≥ write quorum 3), a new object `obj-mrf.bin` was written through the real S3 `PutObject` path. The full result, verbatim, with the exact `s3op.py` invocation that produced it:

```
$ python3 s3op.py put bucket obj-mrf.bin
PUT_OK obj-mrf.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
```

**Evidence — definitive before/after shard inspection.** Inspecting the on-disk shards **while `data1` was still down** showed the shard was **absent** on `data1` but present on each of the three online drives (each carrying the same part-directory UUID plus a 387-byte `xl.meta`):

```
BEFORE heal (data1 chmod 000; obj-mrf.bin written with 3 drives up):
  data1: MISSING   (ls: cannot access .../data1/bucket/obj-mrf.bin/: No such file or directory)
  data2: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5/  +  xl.meta 387B)
  data3: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5/  +  xl.meta 387B)
  data4: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5/  +  xl.meta 387B)
```

After restoring `data1` (`chmod 755`) and waiting for healing, the shard had been **reconstructed onto `data1`**:

```
AFTER restore data1 (healed, observed 01:14:35 UTC):
  data1: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5)   <- reconstructed onto the returned drive
  data2: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5)
  data3: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5)
  data4: PRESENT   (005e1281-6b25-4702-86d8-ddb66d4980c5)
```

The reconstructed shard on `data1` carries the **same part-directory UUID** (`005e1281-6b25-4702-86d8-ddb66d4980c5`) as the surviving three, i.e. the object was rebuilt from its surviving shards rather than re-uploaded. Integrity of the healed object was confirmed through the real S3 read path:

```
$ python3 s3op.py get bucket obj-mrf.bin
GET_OK obj-mrf.bin HTTP 200 BYTES 1048576
```

**Corollary observed.** The write that was **refused** in Scenario B (`obj-B.bin`) was **never stored** — a later read returns `HTTP 404 Code=NoSuchKey` — confirming a below-quorum write leaves no partial object to heal:

```
$ python3 s3op.py get bucket obj-B.bin
S3_ERROR action=get HTTP=404 Code=NoSuchKey Message=The specified key does not exist.
```


---

## Q8 — Where the quorum decision lives and how the threshold is computed

**Direct answer.** The quorum threshold follows one **K+1 rule** that is computed at two different sites and **enforced** in a single place. For a brand-new object, the real `PutObject` path computes write quorum **inline** in `putObject` [`cmd/erasure-object.go:1245`] (there is no prior metadata to read); for operations that already have on-disk metadata — reads, healing, `CopyObject`, multipart completion, and metadata/tag updates — the identical formula is derived by `objectQuorumFromMeta` [`cmd/erasure-metadata.go:531`]. In both cases the per-drive results are checked against that threshold by `reduceQuorumErrs` [`cmd/erasure-metadata-utils.go:137`], and the same K+1 rule is **mirrored** for health reporting in `Health()` [`cmd/erasure-server-pool.go:2722-2727`]. For four drives the default parity is 2, giving **read quorum = 2** and **write quorum = 3**.

**Step 1 — default parity for the set.** `DefaultParityBlocks(drive)` [`internal/config/storageclass/storage-class.go:355`] returns 2 for a four-drive set — `case 4, 5:` [`:361`] `return 2` [`:362`]. Parity is capped at half the set — `if ssParity > setDriveCount/2 {` [`internal/config/storageclass/storage-class.go:202`] — so for four drives parity is at most 2.

**Step 2 — the K+1 quorum formula.** The canonical form of the formula lives in `objectQuorumFromMeta` [`cmd/erasure-metadata.go:531`] (doc comment L528-530: "readQuorum is the min required disks to read data. writeQuorum is the min required disks to write data."), which is the site used by the **metadata-derived** paths (reads, healing, `CopyObject`, multipart, tag/metadata). The real new-object `PutObject` path computes the **same** formula inline instead (see Step 3b):

```go
dataBlocks := len(partsMetaData) - parityBlocks   // 4 - 2 = 2

writeQuorum := dataBlocks                          // = 2          [cmd/erasure-metadata.go:557]
if dataBlocks == parityBlocks {                    //   2 == 2     [cmd/erasure-metadata.go:558]
    writeQuorum++                                  // => 3         [cmd/erasure-metadata.go:559]
}
return dataBlocks, writeQuorum, nil                //              [cmd/erasure-metadata.go:564]
```

The **`writeQuorum++`** branch [`cmd/erasure-metadata.go:558-559`] is the crucial detail: **when parity is exactly half the set, write quorum becomes data + 1** (here 3). This is a **split-brain guard** — it prevents two equal halves from each believing they hold a valid write. Read quorum is `readQuorum := N - parity` [`cmd/erasure-metadata.go:476`] inside `commonParity` [`cmd/erasure-metadata.go:461`] → `4 - 2 = 2`.

**Step 3 — enforce it (the "enough disks vs. stop" decision).** Per-drive results are reduced against the threshold in `reduceQuorumErrs` [`cmd/erasure-metadata-utils.go:137`]:

```go
maxCount, maxErr := reduceErrs(errs, ignoredErrs)
if maxCount >= quorum {        // enough drives agree -> proceed   [cmd/erasure-metadata-utils.go:142]
    return maxErr
}
return quorumErr               // risk too high -> stop
```

`reduceReadQuorumErrs` [`cmd/erasure-metadata-utils.go:150`] passes the sentinel `errErasureReadQuorum` [`:151`]; `reduceWriteQuorumErrs` [`cmd/erasure-metadata-utils.go:156`] passes `errErasureWriteQuorum` [`:157`]. Two distinct families of callers feed a `writeQuorum` into this check, and they must **not** be conflated:

- **Metadata-derived paths (operating on an *existing* object).** Operations that already have on-disk `xl.meta` obtain their quorum from `objectQuorumFromMeta` [`cmd/erasure-metadata.go:531`]: `CopyObject` [`cmd/erasure-object.go:103`], `healObject` [`cmd/erasure-healing.go:307`], multipart completion [`cmd/erasure-multipart.go:79`], `PutObjectMetadata` [`cmd/erasure-object.go:2145`], and `PutObjectTags` [`cmd/erasure-object.go:2224`]. These are read/heal/copy/metadata reconstructions — they are **not** the acceptance decision for a brand-new object body.

**Step 3b — the real new-object `PutObject` write path (where a fresh PUT is accepted or refused).** A new `PutObject` [`cmd/erasure-object.go:1240`] delegates to `putObject` [`cmd/erasure-object.go:1245`], which does **not** call `objectQuorumFromMeta` (there is no prior metadata). Instead it derives and enforces the threshold itself:

1. **Adjust parity for offline drives + early no-quorum guard** [`cmd/erasure-object.go:1291-1308`]: under the availability-optimized storage class it raises `parityDrives` once per offline drive, and if `offlineDrives >= (len(storageDisks)+1)/2` it fails immediately with `errErasureWriteQuorum` [`cmd/erasure-object.go:1308`].
2. **Compute write quorum inline with the identical K+1 rule** — `// writeQuorum is dataBlocks + 1`, `writeQuorum := dataDrives`, `if dataDrives == parityDrives { writeQuorum++ }` [`cmd/erasure-object.go:1322-1326`].
3. **Encode the shards under that quorum** — `erasure.Encode(ctx, toEncode, writers, buffer, writeQuorum)` [`cmd/erasure-object.go:1425`].
4. **Commit by renaming the temp object**, passing the same `writeQuorum` — `renameData(ctx, onlineDisks, …, writeQuorum)` [`cmd/erasure-object.go:1535`]; inside `renameData` [`cmd/erasure-object.go:1013`] the per-drive rename results are reduced by `reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)` [`cmd/erasure-object.go:1054`].

**That commit-time `reduceWriteQuorumErrs` at `cmd/erasure-object.go:1054` (reached from `putObject` via `renameData` at `:1535`), together with the early guard at `:1308`, is the exact point where a real new-object `PutObject` is accepted or refused.** With three drives online the rename reaches `writeQuorum = 3` and the PUT is accepted (Scenario A: `PUT_OK obj-A.bin HTTP 200`); with only two online it cannot, so `errErasureWriteQuorum` propagates and the S3 layer returns `HTTP 503 SlowDownWrite` (Scenario B).

**Step 4 — the same K+1 at the health/pool level.** `Health()` recomputes it independently — `poolWriteQuorums[i] = data`; `if data == b.StandardSCParity { poolWriteQuorums[i] = data + 1 }` [`cmd/erasure-server-pool.go:2722-2727`] — so the health verdict and the write path agree on write quorum 3.

**Cross-validation (observed).** The computed thresholds match every runtime signal: `X-Minio-Write-Quorum: 3` and `X-Minio-Read-Quorum: 2` in the health headers; the server log `expected write quorum: 3, drives-online: 2`; and the client-side `HTTP 503 SlowDownWrite` when only two drives remained.

---

## Q9 — Empirical grounding (health endpoint + real writes)

Every stage below was produced by the **real S3 client** (`boto3`, SigV4) and the **real `/minio/health/*` endpoints** (`curl`). Health routes are registered under `/minio/health` [`cmd/healthcheck-router.go:27-31`, `41-52`]; the write-side handler is `ClusterCheckHandler` [`cmd/healthcheck-handler.go:56`] (200 [`:89`] / 503 [`:85`] / 412 maintenance [`:83`]) and the read-side handler is `ClusterReadCheckHandler` [`cmd/healthcheck-handler.go:93`], which keys off `HealthyRead` [`:115`].

| Stage | Drives online | `GET /cluster` | `X-Minio-Write-Quorum` | `GET /cluster/read` | `X-Minio-Read-Quorum` | Real S3 write | Real S3 read |
|---|---|---|---|---|---|---|---|
| Healthy baseline | 4 | **200** | 3 | **200** | 2 | `PUT_OK obj-healthy.bin HTTP 200` | `GET_OK obj-healthy.bin HTTP 200 BYTES 1048576` |
| Scenario A (1 down) | 3 | **200** | 3 | **200** | 2 | `PUT_OK obj-A.bin HTTP 200` | `GET_OK obj-A.bin HTTP 200 BYTES 1048576` |
| Scenario B (2 down) | 2 | **503** | 3 | **200** | 2 | `S3_ERROR action=put HTTP=503 Code=SlowDownWrite` (refused) | `GET_OK obj-healthy.bin HTTP 200 BYTES 1048576` |
| Restored (recovery/heal) | 4 | **200** | 3 | **200** | 2 | `PUT_OK obj-D.bin HTTP 200` (resumed) | `GET_OK obj-mrf.bin HTTP 200 BYTES 1048576` |

Every cell is backed by the verbatim capture below, each shown with the exact `s3op.py` / `curl` invocation that produced it.

**Healthy baseline (4 drives up).** Full write and read, plus all four health probes:

```
$ python3 s3op.py mb  bucket
MB_OK bucket
$ python3 s3op.py put bucket obj-healthy.bin
PUT_OK obj-healthy.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
$ python3 s3op.py get bucket obj-healthy.bin
GET_OK obj-healthy.bin HTTP 200 BYTES 1048576
# curl -sS -o /dev/null -w '%{http_code}' … :
GET /minio/health/cluster        -> HTTP 200   (X-Minio-Write-Quorum: 3)
GET /minio/health/cluster/read   -> HTTP 200   (X-Minio-Read-Quorum: 2)
GET /minio/health/live           -> HTTP 200
GET /minio/health/ready          -> HTTP 200
```

**Scenario A — one directory down (3 up ≥ write quorum 3).**

```
$ python3 s3op.py put bucket obj-A.bin
PUT_OK obj-A.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
$ python3 s3op.py get bucket obj-A.bin
GET_OK obj-A.bin HTTP 200 BYTES 1048576
cluster       HTTP 200
cluster/read  HTTP 200
live          HTTP 200
```

**Scenario B — two directories down (2 up < write quorum 3; still ≥ read quorum 2).**

```
$ python3 s3op.py put bucket obj-B.bin
S3_ERROR action=put HTTP=503 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate
$ python3 s3op.py get bucket obj-healthy.bin
GET_OK obj-healthy.bin HTTP 200 BYTES 1048576
cluster       HTTP 503
cluster/read  HTTP 200
live          HTTP 200
```

**Restored — permissions returned (recovery + heal; default logging, no debug).**

```
$ chmod 755 /tmp/minio-obs.zvAv/data1
health cluster -> 200 after 0.02s
write resumed after 0.31s: PUT_OK obj-D.bin HTTP 200 ETag "d9122db2eede81390065782dbabc5aba"
$ python3 s3op.py get bucket obj-mrf.bin
GET_OK obj-mrf.bin HTTP 200 BYTES 1048576
```

**Cleanup proof (read-only mandate).** After the investigation the server was stopped by its explicit PID and the ephemeral artifacts were removed: the built `./minio` binary, the temporary `$OBS` workspaces (data directories and server logs), and the `boto3`/timing helper scripts. Because the built binary is **git-ignored**, `git status` alone cannot prove it was removed — so each artifact is checked explicitly instead:

```
$ (ss -ltn 2>/dev/null | grep -q ':9000' && echo "PORT 9000 BUSY") || echo "port 9000 free"
port 9000 free

$ test ! -e ./minio && echo "ABSENT: ./minio"
ABSENT: ./minio

$ ls -d /tmp/minio-obs.*    2>/dev/null || echo "ABSENT: /tmp/minio-obs.* data dirs"
ABSENT: /tmp/minio-obs.* data dirs

$ ls -d /tmp/minio-timing.* 2>/dev/null || echo "ABSENT: /tmp/minio-timing.* data dirs"
ABSENT: /tmp/minio-timing.* data dirs

$ ls -d /tmp/minio-dbg.*    2>/dev/null || echo "ABSENT: /tmp/minio-dbg.* data dirs"
ABSENT: /tmp/minio-dbg.* data dirs
```

The git-ignore rule that makes the explicit binary check necessary:

```
$ git check-ignore -v minio
.gitignore:4:minio	minio
```

The read-only-source mandate is proved directly by diffing the source revision (`c07e5b49d477`) against `HEAD`: exactly **one** file is added and **zero** `.go` (or any other source/test/config/build) files are touched:

```
$ git diff c07e5b49d477 HEAD --name-status
A	blitzy/documentation/minio_c07e5b49d477.md

$ git diff c07e5b49d477 HEAD --name-only | grep -c '\.go$'
0
```

With that single deliverable committed, the working tree is clean — the whole-tree status (which *would* surface any stray artifact, since `--untracked-files=all` lists ignored-directory contents too) is empty:

```
$ git status --porcelain --untracked-files=all
                       # (empty output — clean working tree; only the deliverable is committed)
```

No file in the MinIO source tree was created, modified, or deleted; the only change to the repository is this one added document.


---

## Corroboration (secondary, non-authoritative)

The source code and the observed runtime behavior above are the authoritative source of truth. The following in-repo documentation is consistent with, and corroborates, that behavior:

- `docs/erasure/README.md:3` states that with the highest redundancy `"you may lose up to half (N/2) of the total drives and still be able to recover the data"` — consistent with read quorum 2 out of four drives (Scenario B: two drives lost, reads still succeed).
- `docs/erasure/README.md:9` states `"By default, MinIO shards the objects across N/2 data and N/2 parity drives."` — consistent with the observed default parity 2 (data 2 + parity 2) for a four-drive set.

MinIO's official online documentation (erasure-coding/quorum, degraded-mode reads, automatic parity upgrade during outages, and returned-drive vs. fresh-disk healing) is directionally consistent with these findings but is treated here strictly as external corroboration, not as evidence.

---

## Final coverage checklist

Each question and each named mechanism is answered with an exact literal, a `file:line`, and an observed evidence line.

| Item | Exact literal / value | `file:line` | Observed evidence |
|---|---|---|---|
| Q1 health decision | `Health()`, `DriveStateOk`, `Healthy = online >= poolWriteQuorums` | `cmd/erasure-server-pool.go:2679,2707,2783-2784` | `GET /cluster` → `200`, `X-Minio-Write-Quorum: 3` |
| Q1 write-quorum header | `MinIOWriteQuorum = "x-minio-write-quorum"` → wire `X-Minio-Write-Quorum` | `internal/http/headers.go:193`; `cmd/healthcheck-handler.go:72` | `X-Minio-Write-Quorum: 3` |
| Q1 read-quorum header | `MinIOReadQuorum = "x-minio-read-quorum"` → wire `X-Minio-Read-Quorum` | `internal/http/headers.go:196`; `cmd/healthcheck-handler.go:109` | `X-Minio-Read-Quorum: 2` |
| Q2 adapt vs refuse | threshold-gated on `writeQuorum` | `cmd/erasure-metadata.go:557-560` | A: `PUT_OK obj-A.bin HTTP 200`; B: `S3_ERROR HTTP=503 SlowDownWrite` |
| Q3a above threshold | 3 online ≥ wq 3 → write ok | `cmd/erasure-object.go:1054` | `PUT_OK obj-A.bin HTTP 200` |
| Q3b below threshold | `errErasureWriteQuorum` = `"Write failed. Insufficient number of drives online"` | `cmd/erasure-errors.go:26` | `HTTP=503 Code=SlowDownWrite` |
| Q3 read survives | 2 online ≥ rq 2 | `cmd/erasure-server-pool.go:2784` | `GET_OK obj-healthy.bin HTTP 200 BYTES 1048576` |
| Q3 wire mapping | `ErrSlowDownWrite` = Code `"SlowDownWrite"`, 503 | `cmd/api-errors.go:874-878,2192-2193` | `Code=SlowDownWrite` |
| Q3 quorum-fail log | `"Write quorum could not be established on pool: %d, set: %d, expected write quorum: %d, drives-online: %d"` (via `logger.FatalKind`, no crash) | `cmd/erasure-server-pool.go:2794-2795` | `expected write quorum: 3, drives-online: 2`; process stayed alive |
| Q4 path logging | `AppendTags("endpoint", endpoint.String())` via `printEndpointError` | `cmd/prepare-storage.go:35,40,51` | `endpoint="/tmp/minio-obs.zvAv/data1"` |
| Q4 observed literal | `errDiskAccessDenied` = `"drive access denied"` | `cmd/storage-errors.go:68` | `Error: drive access denied (cmd.StorageErr)` |
| Q4 sibling literals | `errUnformattedDisk`/`errDiskNotFound`/`errDriveIsRoot`/`errFaultyDisk` | `cmd/storage-errors.go:38,53,59,65` | (enumerated in §Q4 table) |
| Q5 live recovery | `monitorLocalDisksAndHeal`; heal-task print (diagnostic) | `cmd/background-newdisks-heal-ops.go:563`; `cmd/admin-heal-ops.go:740-741` | default: `health 200 @0.02s` + `PUT_OK obj-D.bin`; diagnostic: heal enqueued for `obj-h2.bin` |
| Q6 polling self-recognition | `monitorAndConnectEndpoints` → `connectDisks(true)` | `cmd/erasure-sets.go:283,303,479` | `minio: <DEBUG> running drive monitoring` |
| Q6 push into healing | `pushHealLocalDisks(...)` | `cmd/erasure-sets.go:227,238` | heal tasks enqueued after restore |
| Q6 interval (runtime-confirmed) | `defaultMonitorNewDiskInterval + time.Second*5` = 15 s | `cmd/erasure-sets.go:348`; `cmd/background-newdisks-heal-ops.go:40` | 10 intervals, 15.000–15.002 s, mean 15.001 (2 runs) |
| Q7 MRF queue | `addPartialOp` / `healRoutine` | `cmd/mrf.go:78,220`; `cmd/erasure-object.go:400,805,1578,2113`; `cmd/erasure-multipart.go:1409` | before: data1 MISSING → after: data1 PRESENT |
| Q7 fresh-disk heal | `healFreshDisk` via `monitorLocalDisksAndHeal` | `cmd/background-newdisks-heal-ops.go:419,563` | shard reconstructed on returned drive |
| Q7 object reconstruction | `healObject` / `HealObject`, `objectQuorumFromMeta` | `cmd/erasure-healing.go:258,307,1039` | `GET_OK obj-mrf.bin HTTP 200 BYTES 1048576` |
| Q8 threshold compute | `writeQuorum := dataBlocks` + K+1; `readQuorum := N - parity` | `cmd/erasure-metadata.go:557-560,476` | headers 3 / 2 |
| Q8 default parity | `DefaultParityBlocks` `case 4,5: return 2`; cap `> setDriveCount/2` | `internal/config/storageclass/storage-class.go:355,361-362,202` | write quorum 3, read quorum 2 |
| Q8 enforcement | `reduceQuorumErrs` `if maxCount >= quorum` | `cmd/erasure-metadata-utils.go:137,142,150,156` | write refused at 2 online |
| Q8 real `PutObject` accept/refuse | `PutObject`→`putObject`; offline-parity + no-quorum guard; inline `writeQuorum`; `Encode`; `renameData`→`reduceWriteQuorumErrs` | `cmd/erasure-object.go:1240,1245,1291-1308,1322-1326,1425,1535,1054` | A: `PUT_OK obj-A.bin HTTP 200`; B: `HTTP 503 SlowDownWrite` |
| Q8 metadata-derived quorum (read/heal/copy) | `objectQuorumFromMeta` callers (not new PUT) | `cmd/erasure-object.go:103`; `cmd/erasure-healing.go:307`; `cmd/erasure-multipart.go:79`; `cmd/erasure-object.go:2145,2224` | reconstruction paths only |
| Q9 empirical table | routes + handlers | `cmd/healthcheck-router.go:27-31,41-52`; `cmd/healthcheck-handler.go:56,93` | full stage table above |
| Topology | `1 set(s), 4 drives per set` | (startup log) | `INFO: Formatting 1st pool, 1 set(s), 4 drives per set.` |
| Build banner | `DEVELOPMENT.2024-11-25T17-10-22Z`, `go1.23.12` | `Makefile:177,179`; `go.mod:3` | `./minio --version` output |
| Cleanup | repo unchanged apart from this doc | `.gitignore:4` (binary git-ignored) | `test ! -e ./minio` → `ABSENT`; `git diff c07e5b49d477 HEAD --name-status` → single `A blitzy/documentation/minio_c07e5b49d477.md` (0 `.go`); `git status --porcelain` empty |

**Coverage statement.** All nine questions (Q1–Q9) and every named mechanism — quorum math, health endpoints and headers, path-tagged logging, the disk-error literals, the polling reconnect and the push-into-healing, the MRF queue, fresh-disk healing, and per-object reconstruction — are addressed above, each paired with an exact `file:line` citation and a verbatim observed evidence line. Timing was reported only after confirming stability across two runs; the ≈15 s value is runtime-confirmed. Values obtained from the real S3 path and the real health endpoints are canonical; no bypassing, fallback, or synthetic path was used to produce any reported value.

