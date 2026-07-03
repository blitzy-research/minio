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
- **Path logging.** Yes — disk errors name the failing directory directly, e.g. `endpoint="/tmp/minio-obs.f2FV/data1"`, via `printEndpointError` [`cmd/prepare-storage.go:35-72`].
- **Recovery is self-driven by polling.** When permissions are restored, a background loop re-admits the drive **on its own** and **pushes** it into a healing path. The reconnect poll interval is **runtime-confirmed ≈15.0 s** (two runs, eight intervals, 14.93–15.03 s), matching the code constant `10s + 5s` [`cmd/erasure-sets.go:348`], [`cmd/background-newdisks-heal-ops.go:40`].
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

**Fault injection.** A directory is made inaccessible with `chmod 000 <dir>` and restored with `chmod 755 <dir>`, matching the user's "permission changes" scenario. Data is never deleted.

**Timing method (magnitude rule).** To measure the disk-reconnect poll interval directly, the server was (for that measurement only) started with `_MINIO_SERVER_DEBUG=on` [`cmd/common-main.go:67`], which surfaces the internal monitor tick `console.Debugln("running drive monitoring")` [`cmd/erasure-sets.go:300`]. This toggles **logging verbosity only**; it does not change the polling behavior, and the interval itself is a fixed code constant. The inter-tick spacing was measured across **two independent windows**.

**Reproducibility & cleanup.** Every value below is reproducible from the commands above. After the investigation, the server was stopped by its explicit PID, and the built binary, the temporary `$OBS` workspace (data directories, logs, and helper scripts) were removed so that `git status --porcelain` reports only this one new document. The cleanup proof is shown at the end of [§Q9](#q9--empirical-grounding-health-endpoint--real-writes).


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

**Evidence (the pivot in one line).** With one directory down, a real `PutObject` returned `PUT_OK … HTTP 200`; with a second directory down, the identical `PutObject` returned `HTTP=503 Code=SlowDownWrite`. Both are quoted in full in [§Q3](#q3--both-scenarios-above-vs-below-the-threshold).


---

## Q3 — Both scenarios: above vs. below the threshold

### Scenario A — one directory lost (3 online ≥ write quorum 3): writes CONTINUE

**Setup.** `chmod 000 $OBS/data1` while the server ran. Three drives (`data2`, `data3`, `data4`) remained online.

**Observed — real S3 write SUCCEEDS:**

```
PUT_OK obj-A.bin HTTP 200 ETag "8777e8cc7ba3e87f0749c9fde7d3f553"
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

**Why.** `writeQuorum = 3` is met by the three surviving shard writes, so `renameData` → `reduceWriteQuorumErrs(…, writeQuorum)` [`cmd/erasure-object.go:1054`] does not trip the sentinel. See [§Q8](#q8--where-the-quorum-decision-lives-and-how-the-threshold-is-computed).

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
       endpoint="/tmp/minio-obs.f2FV/data1"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

The permission fault surfaces the storage sentinel **`errDiskAccessDenied` = `StorageErr("drive access denied")`** [`cmd/storage-errors.go:68`] (comment L67: "we don't have write permissions on disk"). Internal read paths also name the path directly, e.g. `unable to read /tmp/minio-obs.f2FV/data1/.minio.sys/buckets/.healing.bin: … permission denied` from `xlStorage.Healing()`.

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

**Direct answer.** **Yes — recovery is visibly attempted while the server stays up.** Two background loops run continuously without any restart: a **drive-reconnect monitor** and a **heal task queue**. When a drive returns, the monitor re-admits it and heal tasks are enqueued for the affected objects — all while the server keeps answering health checks and reads.

**How it works (code).** The reconnect monitor `monitorAndConnectEndpoints` [`cmd/erasure-sets.go:283`] runs on a timer and calls `connectDisks(true)` [`cmd/erasure-sets.go:303`] each tick; the fresh/returned-disk healer `monitorLocalDisksAndHeal` [`cmd/background-newdisks-heal-ops.go:563`] runs on its own 10 s timer [`cmd/background-newdisks-heal-ops.go:565`]; and queued heal work is dispatched through `globalBackgroundHealRoutine.tasks`.

**Evidence (verbatim, captured live during a down→restore cycle, no restart).** The polling monitor ticking:

```
minio: <DEBUG> running drive monitoring
```

The path-tagged failure while the drive is down (recovery target identified):

```
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/minio-obs.f2FV/data1"
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

Heal tasks **queued** right after the drive is restored (recovery actively in progress):

```
Task in the queue: cmd.healTask{bucket:".minio.sys", object:"buckets/bucket/.usage-cache.bin", versionID:"", opts:madmin.HealOpts{Recursive:false, DryRun:false, Remove:true, Recreate:false, ScanMode:0, UpdateParity:false, NoLock:false, Pool:(*int)(nil), Set:(*int)(nil)}, respCh:(chan cmd.healResult)(0x…)}
```

That line is printed by `fmt.Printf("Task in the queue: %#v\n", task)` [`cmd/admin-heal-ops.go:741`] as tasks are pushed onto the background heal routine. Immediately afterward `GET /minio/health/cluster` returned `200`. (The `_MINIO_SERVER_DEBUG=on` toggle only made these existing internal events visible in the log; it did not create the behavior.)


---

## Q6 — Self-recognition (polling) vs. being pushed into healing

**Direct answer.** It is **both**: MinIO **self-recognizes** a returned drive through a **polling loop** (no external trigger needed), and that same loop then **pushes** the drive into a healing path. You do not have to restart the server or issue any command — leaving the drive accessible again is enough.

**How it works (code).**

- **Polling (self-recognition):** `monitorAndConnectEndpoints` [`cmd/erasure-sets.go:283`] loops forever, calling `connectDisks(true)` [`cmd/erasure-sets.go:303`] on every tick and resetting its timer [`cmd/erasure-sets.go:306`]. It is launched once at startup — `go s.monitorAndConnectEndpoints(ctx, defaultMonitorConnectEndpointInterval)` [`cmd/erasure-sets.go:479`].
- **Push into healing:** inside `connectDisks`, a reconnected local drive that is unformatted or has unfinished healing is pushed onto the heal queue — `globalBackgroundHealState.pushHealLocalDisks(endpoint)` [`cmd/erasure-sets.go:227`] and `pushHealLocalDisks(disk.Endpoint())` [`cmd/erasure-sets.go:238`]. The fresh/returned-disk healer `monitorLocalDisksAndHeal` [`cmd/background-newdisks-heal-ops.go:563`] then drains that queue.

**The interval (magnitude — runtime-confirmed).** The poll interval is the code constant `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` [`cmd/erasure-sets.go:348`], where `defaultMonitorNewDiskInterval = time.Second * 10` [`cmd/background-newdisks-heal-ops.go:40`] — i.e. **code-derived ≈ 15 s**. This was **runtime-confirmed** by measuring the spacing between successive `"running drive monitoring"` ticks [`cmd/erasure-sets.go:300`] across **two independent windows**:

| Run | Window | Measured intervals | Mean |
|---|---|---|---|
| 1 | ~80 s | 15.03 s, 15.03 s, 14.93 s, 15.03 s | 15.01 s |
| 2 | ~65 s | 15.03 s, 15.03 s, 15.03 s, 14.93 s | 15.01 s |

Eight intervals across both runs fell in **14.93 – 15.03 s** (mean 15.01 s), stable across runs — matching the code-derived 15 s.

**Reported exactly as observed — an important nuance.** The *health endpoint's* view of drives recovers **much faster** than this 15 s poll. Measuring the time from `chmod 755` to `GET /minio/health/cluster` returning `200` again gave **≈ 0–1 s** across three cycles. That is because `Health()` re-derives the online count from fresh per-drive `DiskInfo` probes, which are independent of the 15 s reconnect poll. So there are two distinct cadences: the health endpoint reflects drive accessibility within ~1 s, whereas the drive-reconnect/heal monitor sweeps every ~15 s. Both were observed; neither was assumed.

**Evidence (verbatim).** The polling tick, recurring every ~15 s:

```
minio: <DEBUG> running drive monitoring
```

After restoring permissions (no restart), a real S3 write resumed and health returned to green:

```
PUT_OK obj-C.bin HTTP 200 ETag "8777e8cc7ba3e87f0749c9fde7d3f553"
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

**Evidence — definitive before/after shard inspection.** With only `data1` down (still above write quorum), a new object `obj-mrf.bin` was written; it returned `PUT_OK … HTTP 200`. Inspecting the on-disk shards **while `data1` was still down** showed the shard was **absent** on `data1`:

```
BEFORE heal (data1 chmod 000):
  data1: MISSING
  data2: PRESENT
  data3: PRESENT
  data4: PRESENT
```

After restoring `data1` (`chmod 755`) and waiting for healing, the shard had been **reconstructed onto `data1`**:

```
AFTER restore data1:
  data1: PRESENT
  data2: PRESENT
  data3: PRESENT
  data4: PRESENT
```

Integrity of the healed object was confirmed through the real S3 read path:

```
GET_OK obj-mrf.bin HTTP 200 BYTES 1048576
```

**Corollary observed.** The write that was **refused** in Scenario B (`obj-B.bin`) was **never stored** — a later read returns `HTTP 404 Code=NoSuchKey` — confirming a below-quorum write leaves no partial object to heal:

```
S3_ERROR action=get HTTP=404 Code=NoSuchKey Message=The specified key does not exist.
```


---

## Q8 — Where the quorum decision lives and how the threshold is computed

**Direct answer.** The quorum threshold is **computed** in `objectQuorumFromMeta` (in `cmd/erasure-metadata.go`) and **enforced** in `reduceQuorumErrs` (in `cmd/erasure-metadata-utils.go`); the same K+1 rule is **mirrored** for health reporting in `Health()`. For four drives the default parity is 2, giving **read quorum = 2** and **write quorum = 3**.

**Step 1 — default parity for the set.** `DefaultParityBlocks(drive)` [`internal/config/storageclass/storage-class.go:355`] returns 2 for a four-drive set — `case 4, 5:` [`:361`] `return 2` [`:362`]. Parity is capped at half the set — `if ssParity > setDriveCount/2 {` [`internal/config/storageclass/storage-class.go:202`] — so for four drives parity is at most 2.

**Step 2 — compute read & write quorum.** In `objectQuorumFromMeta` [`cmd/erasure-metadata.go:531`] (doc comment L528-530: "readQuorum is the min required disks to read data. writeQuorum is the min required disks to write data."):

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

`reduceReadQuorumErrs` [`cmd/erasure-metadata-utils.go:150`] passes the sentinel `errErasureReadQuorum` [`:151`]; `reduceWriteQuorumErrs` [`cmd/erasure-metadata-utils.go:156`] passes `errErasureWriteQuorum` [`:157`]. On the write path this is wired as: `objectQuorumFromMeta` gives `writeQuorum` [`cmd/erasure-object.go:103`] → `renameData(…, writeQuorum)` [`cmd/erasure-object.go:1013`] → `reduceWriteQuorumErrs(ctx, errs, objectOpIgnoredErrs, writeQuorum)` [`cmd/erasure-object.go:1054`]. That is the exact point where a real `PutObject` is accepted or refused.

**Step 4 — the same K+1 at the health/pool level.** `Health()` recomputes it independently — `poolWriteQuorums[i] = data`; `if data == b.StandardSCParity { poolWriteQuorums[i] = data + 1 }` [`cmd/erasure-server-pool.go:2722-2727`] — so the health verdict and the write path agree on write quorum 3.

**Cross-validation (observed).** The computed thresholds match every runtime signal: `X-Minio-Write-Quorum: 3` and `X-Minio-Read-Quorum: 2` in the health headers; the server log `expected write quorum: 3, drives-online: 2`; and the client-side `HTTP 503 SlowDownWrite` when only two drives remained.

---

## Q9 — Empirical grounding (health endpoint + real writes)

Every stage below was produced by the **real S3 client** (`boto3`, SigV4) and the **real `/minio/health/*` endpoints** (`curl`). Health routes are registered under `/minio/health` [`cmd/healthcheck-router.go:27-31`, `41-52`]; the write-side handler is `ClusterCheckHandler` [`cmd/healthcheck-handler.go:56`] (200 [`:89`] / 503 [`:85`] / 412 maintenance [`:83`]) and the read-side handler is `ClusterReadCheckHandler` [`cmd/healthcheck-handler.go:93`], which keys off `HealthyRead` [`:115`].

| Stage | Drives online | `GET /cluster` | `X-Minio-Write-Quorum` | `GET /cluster/read` | `X-Minio-Read-Quorum` | Real S3 write | Real S3 read |
|---|---|---|---|---|---|---|---|
| Healthy baseline | 4 | **200** | 3 | **200** | 2 | `PUT_OK … HTTP 200` | `GET_OK … 1048576` |
| Scenario A (1 down) | 3 | **200** | 3 | **200** | 2 | `PUT_OK obj-A.bin HTTP 200` | `GET_OK obj-A.bin … 1048576` |
| Scenario B (2 down) | 2 | **503** | 3 | **200** | 2 | `HTTP 503 Code=SlowDownWrite` | `GET_OK obj-healthy.bin … 1048576` |
| Restored (heal) | 4 | **200** | 3 | **200** | 2 | `PUT_OK obj-C.bin HTTP 200` | `GET_OK obj-mrf.bin … 1048576` |

Supporting verbatim captures for each cell appear in [§Q1](#q1--how-minio-decidesreports-healthy-and-its-disk-assumption), [§Q3](#q3--both-scenarios-above-vs-below-the-threshold), [§Q6](#q6--self-recognition-polling-vs-being-pushed-into-healing), and [§Q7](#q7--repair-of-objects-written-while-a-disk-was-down). The healthy-baseline liveness/readiness probes:

```
# GET /minio/health/live
HTTP/1.1 200 OK
# GET /minio/health/ready
HTTP/1.1 200 OK
```

**Cleanup proof (read-only mandate).** After the investigation the server was stopped by its explicit PID, and the built `./minio` binary, the temporary `$OBS` workspace (data directories, logs, and the `boto3`/timing helper scripts) were removed. The only new path in the working tree is this document. Listing untracked files individually:

```
$ git status --porcelain --untracked-files=all
?? blitzy/documentation/minio_c07e5b49d477.md
```

(The default `git status --porcelain` collapses this to `?? blitzy/`, since the whole `blitzy/` directory is new; the built `./minio` binary never appears because it is git-ignored — `.gitignore:4`.) Crucially, restricting the view to tracked files confirms **zero** modifications or deletions in the MinIO source tree:

```
$ git status --porcelain --untracked-files=no
                       # (empty output — no tracked file created, modified, or deleted)
```

No file in the MinIO source tree was created, modified, or deleted.


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
| Q2 adapt vs refuse | threshold-gated on `writeQuorum` | `cmd/erasure-metadata.go:557-560` | A: `PUT_OK … 200`; B: `HTTP 503 SlowDownWrite` |
| Q3a above threshold | 3 online ≥ wq 3 → write ok | `cmd/erasure-object.go:1054` | `PUT_OK obj-A.bin HTTP 200` |
| Q3b below threshold | `errErasureWriteQuorum` = `"Write failed. Insufficient number of drives online"` | `cmd/erasure-errors.go:26` | `HTTP=503 Code=SlowDownWrite` |
| Q3 read survives | 2 online ≥ rq 2 | `cmd/erasure-server-pool.go:2784` | `GET_OK obj-healthy.bin … 1048576` |
| Q3 wire mapping | `ErrSlowDownWrite` = Code `"SlowDownWrite"`, 503 | `cmd/api-errors.go:874-878,2192-2193` | `Code=SlowDownWrite` |
| Q3 quorum-fail log | `"Write quorum could not be established … expected write quorum: %d, drives-online: %d"` (via `logger.FatalKind`, no crash) | `cmd/erasure-server-pool.go:2794-2795` | `… expected write quorum: 3, drives-online: 2`; process stayed alive |
| Q4 path logging | `AppendTags("endpoint", endpoint.String())` via `printEndpointError` | `cmd/prepare-storage.go:35,40,51` | `endpoint="/tmp/minio-obs.f2FV/data1"` |
| Q4 observed literal | `errDiskAccessDenied` = `"drive access denied"` | `cmd/storage-errors.go:68` | `Error: drive access denied (cmd.StorageErr)` |
| Q4 sibling literals | `errUnformattedDisk`/`errDiskNotFound`/`errDriveIsRoot`/`errFaultyDisk` | `cmd/storage-errors.go:38,53,59,65` | (enumerated in §Q4 table) |
| Q5 live recovery | `monitorLocalDisksAndHeal`; heal-task print | `cmd/background-newdisks-heal-ops.go:563`; `cmd/admin-heal-ops.go:741` | `Task in the queue: cmd.healTask{…}` |
| Q6 polling self-recognition | `monitorAndConnectEndpoints` → `connectDisks(true)` | `cmd/erasure-sets.go:283,303,479` | `minio: <DEBUG> running drive monitoring` |
| Q6 push into healing | `pushHealLocalDisks(...)` | `cmd/erasure-sets.go:227,238` | heal tasks enqueued after restore |
| Q6 interval (runtime-confirmed) | `defaultMonitorNewDiskInterval + time.Second*5` = 15 s | `cmd/erasure-sets.go:348`; `cmd/background-newdisks-heal-ops.go:40` | 8 intervals, 14.93–15.03 s (2 runs) |
| Q7 MRF queue | `addPartialOp` / `healRoutine` | `cmd/mrf.go:78,220`; `cmd/erasure-object.go:400,805,1578,2113`; `cmd/erasure-multipart.go:1409` | before: data1 MISSING → after: data1 PRESENT |
| Q7 fresh-disk heal | `healFreshDisk` via `monitorLocalDisksAndHeal` | `cmd/background-newdisks-heal-ops.go:419,563` | shard reconstructed on returned drive |
| Q7 object reconstruction | `healObject` / `HealObject`, `objectQuorumFromMeta` | `cmd/erasure-healing.go:258,307,1039` | `GET_OK obj-mrf.bin … 1048576` |
| Q8 threshold compute | `writeQuorum := dataBlocks` + K+1; `readQuorum := N - parity` | `cmd/erasure-metadata.go:557-560,476` | headers 3 / 2 |
| Q8 default parity | `DefaultParityBlocks` `case 4,5: return 2`; cap `> setDriveCount/2` | `internal/config/storageclass/storage-class.go:355,361-362,202` | write quorum 3, read quorum 2 |
| Q8 enforcement | `reduceQuorumErrs` `if maxCount >= quorum` | `cmd/erasure-metadata-utils.go:137,142,150,156` | write refused at 2 online |
| Q9 empirical table | routes + handlers | `cmd/healthcheck-router.go:27-31,41-52`; `cmd/healthcheck-handler.go:56,93` | full stage table above |
| Topology | `1 set(s), 4 drives per set` | (startup log) | `INFO: Formatting 1st pool, 1 set(s), 4 drives per set.` |
| Build banner | `DEVELOPMENT.2024-11-25T17-10-22Z`, `go1.23.12` | `Makefile:177,179`; `go.mod:3` | `./minio --version` output |
| Cleanup | repo unchanged apart from this doc | — | `git status --porcelain` → only this `.md` |

**Coverage statement.** All nine questions (Q1–Q9) and every named mechanism — quorum math, health endpoints and headers, path-tagged logging, the disk-error literals, the polling reconnect and the push-into-healing, the MRF queue, fresh-disk healing, and per-object reconstruction — are addressed above, each paired with an exact `file:line` citation and a verbatim observed evidence line. Timing was reported only after confirming stability across two runs; the ≈15 s value is runtime-confirmed. Values obtained from the real S3 path and the real health endpoints are canonical; no bypassing, fallback, or synthetic path was used to produce any reported value.

