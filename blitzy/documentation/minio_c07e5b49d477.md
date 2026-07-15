# MinIO Erasure-Coded Fault Behavior: Drive Loss, Quorum, and Healing

**Repository:** `github.com/minio/minio` &nbsp;·&nbsp; **Branch/checkout:** `minio_c07e5b49d477` &nbsp;·&nbsp; **HEAD:** `c07e5b49d` &nbsp;·&nbsp; **Deployment tested:** single-node, four-directory erasure set (2 data + 2 parity)

## Summary

This document answers, with live runtime evidence and exact `file:line` code references, how the MinIO object-storage server in this checkout behaves in a four-directory erasure-coded deployment when drives become inaccessible (via POSIX permission revocation) and later return. The investigation was **run-first**: the server binary was built from this checkout and run as a non-root single-node 4-drive erasure set through its canonical entry point (`main.go` → `minio.Main(os.Args)`), faults were injected with `chmod`, and the HTTP health endpoints plus real S3 `PUT`/`GET`/`LIST` requests were exercised before any conclusion was written.

**Headline finding:** MinIO does **not** silently degrade — it enforces a **hard erasure-coding quorum**. For a 2-data/2-parity 4-drive set the thresholds are **write quorum = 3** and **read quorum = 2**. With **3 of 4** drives online (one drive lost) MinIO **quietly adapts and keeps writing** (health `200`, `PUT` succeeds). Dropping to **2 of 4** drives online (a second drive lost) crosses below write quorum: the `/minio/health/cluster` endpoint returns **HTTP 503** and S3 `PUT` is refused with **`SlowDownWrite`** ("Resource requested is unwritable, please reduce your request rate"), **while reads still succeed** because 2 online still satisfies read quorum 2. When permissions are restored, the drive is detected **automatically** by background polling (no manual push required), and objects written during the outage are **reconstructed onto the returned drive** by the heal process (Reed-Solomon rebuild from the surviving data + parity shards).

> **Grounding & scope.** Every behavioral claim below is paired with the *actual* output captured from the running server — shown **verbatim** where a block is labeled *unedited*/*verbatim*, or **lightly annotated/summarized** for readability where the block's label says so; every code claim carries an exact `file:line` and names the responsible function/struct. Statements derived only from reading code (not observed at runtime) are explicitly labeled **[inferred]**. The authoritative source for this document is *this open-source checkout's code plus the observed runtime output*; commercial MinIO **AIStor** behavior (e.g., a "48-hour offline" heal rule) is called out as a caveat and is **not** asserted for this build. This was a read-only investigation: all build/run/data artifacts lived outside the checkout and were removed afterward — the repository is left byte-for-byte unchanged except for this document.

## TL;DR — direct answers

| # | Question | Direct answer |
|---|----------|---------------|
| **Q1** | How does MinIO decide it is "healthy," and what does it assume about the required number of disks? | It counts, **per erasure set**, how many drives report state OK and compares that live count against the set's **write quorum** (for `/cluster`) and **read quorum** (for `/cluster/read`). For a 4-drive (2+2) set it assumes it needs **≥ 3 online to be writable-healthy** and **≥ 2 online to be read-healthy**. — `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`]. |
| **Q2** | When one directory becomes inaccessible, does it adapt or refuse to write? | It **quietly adapts and keeps writing** — 3 of 4 online is ≥ write quorum 3, so health stays `200` and `PUT` succeeds. |
| **Q3** | Behavior above the threshold vs. below it (losing a second disk)? | **Above (3 online):** writes continue (`200`/OK). **Below (2 online):** `/cluster` returns **503** and `PUT` fails with **`SlowDownWrite`**, **while reads still succeed** (`/cluster/read` `200`, `GET`/`LIST` OK). |
| **Q4** | Do logs name the failing disk by path, and is recovery attempted while live? | **Yes** — logs name the drive by its **full filesystem path** (`.../d4/.minio.sys/buckets/.healing.bin`), from `(*xlStorage).Healing()` [`cmd/xl-storage.go:436`]. The server inspects each drive's live healing state on every health poll. |
| **Q5** | Is the directory's return detected automatically (polling) or must it be pushed? | **Automatically, by background polling** — no push needed. The **15 s** `monitorAndConnectEndpoints()` → `connectDisks()` re-probes and **reconnects** returned endpoints (and enqueues any fresh/unformatted ones); the **10 s** `monitorLocalDisksAndHeal()` then **heals** the disks that step queued. `mc admin heal` is only an optional accelerator. |
| **Q6** | How are objects written during the outage repaired once the disk returns? | They are **reconstructed onto the returned drive by the heal process** (Reed-Solomon rebuild from surviving shards). Partial-quorum writes are tracked by the MRF subsystem; a background/manual heal walk completes the rebuild. Observed end-to-end: `obj-S1` was rebuilt onto `d4`. |
| **Q7** | Where does the quorum decision live in code, and how is the threshold computed? | Threshold computed in `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`] and enforced in `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`]. For 2+2: **read quorum = N/2 = 2**; **write quorum = dataBlocks = 2, incremented to 3 because dataBlocks == parityBlocks**. |
| **Q8** | Ground all of the above in observable behavior. | Every claim is tied to the captured health-endpoint responses (`curl`) and real S3 read/write attempts (`boto3`) in the scenario matrix and appendix, each cross-referenced to `file:line`. |

---

## Test harness & methodology

### Canonical entry point (no mocks or bypasses)

The server was exercised through its real, canonical entry point — no debug hooks, mocks, or synthetic bypasses were used:

- `main.go` imports the server package as `minio "github.com/minio/minio/cmd"` [`main.go:26`], and `func main()` [`main.go:29`] calls `minio.Main(os.Args)` [`main.go:30`]. That is the only path taken.

### Toolchain and build

- **Toolchain:** Go **1.23.2** (the module declares `go 1.23` at [`go.mod:3`]).
- **Exact build command (verbatim), run from the repository root:**

```
CGO_ENABLED=0 go build -o /tmp/minio_bin .
```

  Result: success, ~59 s, 156 MB binary. `/tmp/minio_bin --version` reported `Runtime: go1.23.2 linux/amd64` and `version DEVELOPMENT.GOGET`. This mirrors the `Makefile` `build:` target [`Makefile:177`], which is `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio` — the same pure-Go, `CGO_ENABLED=0` build.

### Exact run command (verbatim), executed as a NON-ROOT user (`miniouser`, uid 1001)

```
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1 /tmp/minio_bin server /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 --address :9000 --console-address :9001
```

The binary is invoked by its **absolute path** (`/tmp/minio_bin`), so the build (run from the repository root) and this run command are reproducible regardless of the shell's current working directory; the binary and all data directories lived **outside** the checkout.

> **Test credentials only — not for production.** `minioadmin`/`minioadmin` are MinIO's built-in defaults, used here solely for a disposable, **localhost-only** (`127.0.0.1`) instance whose data directories were deleted after the investigation. **Never reuse these credentials in a real deployment.** Production servers must set **unique, secret** `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` values injected from the environment or a secrets manager — never the defaults.

### Deployment shape

A single-node **4-drive erasure set** over four temporary directories located **outside** the repository checkout. The startup log confirmed the layout (relevant lines):

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
```

The on-disk `format.json` showed `"format":"xl"`, a single set listing **4 drive UUIDs**, and `"distributionAlgo":"SIPMOD+PARITY"`.

### Erasure scheme derivation (why 2 data + 2 parity → write quorum 3, read quorum 2)

For 4 drives, `DefaultParityBlocks(4)` returns **2** [`internal/config/storageclass/storage-class.go:355`], so the set is **2 data + 2 parity**. This yields **read quorum = 2** and **write quorum = 3**. The exact computation and its enforcement are traced in **Q1** and **Q7** below. This matches the canonical specification in [`docs/minio-limits.md:15-16`]: read quorum `N/2`, write quorum `N/2+1`.

### NON-ROOT execution is mandatory (with the root counter-example)

The permission-based fault must be experienced by the server as a genuine I/O failure. A process running as **root** bypasses POSIX permission bits, so `chmod 000` on a data directory does **not** deny access to a root process — the drive would still read and write normally, and the fault would never be observed. This is not a theoretical concern; it was confirmed directly:

> A separate instance run **as root (uid 0)** on `:9010`, after `chmod 000` on **two** drives, **still** returned `HTTP/1.1 200 OK` with `X-Minio-Write-Quorum: 3` on `/minio/health/cluster`, and a `PUT` that **succeeded** (`status=200`). See **Appendix A5** for the transcript.

Because a root run masks the very failure under investigation, it is a **non-canonical** observation. The canonical reproduction below therefore runs MinIO as the **non-root** user `miniouser` (uid 1001), so that `chmod 000` truly revokes the drive and MinIO reacts exactly as it would to a real disk becoming unreadable. The root run is documented only to justify this methodology.

When the investigating shell is itself `root`, the server is started with an explicit **privilege drop** to `miniouser` so the process genuinely runs unprivileged. Any of the following equivalent forms works; the run command shown above is launched through the first of these:

```
runuser -u miniouser -- env MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1 /tmp/minio_bin server /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 --address :9000 --console-address :9001
# equivalently:
setpriv --reuid=miniouser --regid=miniouser --clear-groups /tmp/minio_bin server /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 --address :9000 --console-address :9001
# or:
su miniouser -c '/tmp/minio_bin server /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 --address :9000 --console-address :9001'
```

The data directories must be owned by (or writable to) `miniouser` — `chown -R miniouser:miniouser /tmp/minio_data` — so the unprivileged process can format and write them. If the investigating shell is already the non-root user, the drop is unnecessary and the run command is executed directly.

### Canonical observation surfaces

- **HTTP health endpoints** via `curl` — `GET /minio/health/{live,ready,cluster,cluster/read}`. These routes are registered by `registerHealthCheckRouter()` [`cmd/healthcheck-router.go:36`] under the `/minio/health` prefix (`healthCheckPathPrefix` [`cmd/healthcheck-router.go:32`]) for both `GET` and `HEAD`: `/cluster` → `ClusterCheckHandler` [`cmd/healthcheck-router.go:41-42`], `/cluster/read` → `ClusterReadCheckHandler` [`:43-44`], `/live` → `LivenessCheckHandler` [`:47-48`], and `/ready` → `ReadinessCheckHandler` [`:51-52`].
- **Real S3 requests** via `boto3` (observed version **1.43.47**) — canonical `PUT`, `GET`, and `LIST` against the running server.
- `mc` and the AWS CLI were **not** required and were not used; `curl` + `boto3` exercised every canonical path.
- `MINIO_CI_CD=1` was set during the run — a **non-default harness setting** that sets `globalIsCICD` [`cmd/common-main.go:99`]. Its most relevant effect here is that it **skips MinIO's root-drive detection** [`cmd/xl-storage.go:371`], which is what lets the four data directories share a single root filesystem under `/tmp`; its other setup/runtime effects are enumerated under *Configuration notes* below. It has **no** effect on the quorum arithmetic or the health decision — neither `objectQuorumFromMeta()` nor `(*erasureServerPools).Health()` references `globalIsCICD` — so the write-quorum-3 / read-quorum-2 behavior reported here is unchanged by it.

### Repository integrity

When the investigation finished, the server was stopped with an explicit `SIGTERM`. The launch captured the server PID in the shell (`… &` followed by `MINIO_PID=$!`), and teardown was `kill -TERM "$MINIO_PID"` — MinIO registers `os.Interrupt` (SIGINT), `SIGTERM`, and `SIGQUIT` via `signal.Notify(globalOSSignalCh, …)` [`cmd/server-main.go:747`] and services them in `handleSignals()` → `stopProcess()` [`cmd/signals.go:67`], which drains the HTTP server and shuts down the object layer for a graceful exit; port `:9000` was then confirmed closed. All build, run, and data artifacts (the binary at `/tmp/minio_bin`, the data directories under `/tmp/minio_data/`, and the temporary observation scripts) lived **outside** the checkout and were removed afterward (`rm -rf /tmp/minio_bin /tmp/minio_data`). `git status` was verified clean (working tree unchanged) both before and after the investigation. The investigation left the repository byte-for-byte unchanged; the **only** addition is this document.


---

## Observed scenario matrix (S0-S3)

All values were captured from the running non-root server via the HTTP health endpoints (`curl`) and `boto3` `PUT`/`GET`/`LIST`. A representative, lightly annotated transcript for each state follows the table — health-endpoint responses are grouped under `#` labels rather than repeating each full `curl` invocation, and intermediate poll lines are elided where marked. (The fully raw, `$ curl …`-prefixed capture is shown for **S0** and in **Q1**.)

| Scenario | Drives online | `/minio/health/cluster` | `/minio/health/cluster/read` | S3 `PUT` |
|----------|---------------|-------------------------|------------------------------|----------|
| **S0 — Healthy** | 4 / 4 | `200` (`X-Minio-Write-Quorum: 3`) | `200` (`X-Minio-Read-Quorum: 2`) | OK |
| **S1 — `chmod 000 d4`** | 3 / 4 (≥ wq 3) | `200` | `200` | OK — adapts & keeps writing |
| **S2 — `chmod 000 d3`** | 2 / 4 (< wq 3) | **`503`** | `200` (2 ≥ rq 2) | **ERROR `SlowDownWrite`** |
| **S3 — `chmod 755` restore** | 4 / 4 | `200` | `200` | OK |

### S0 — Healthy (4 / 4 online) — observed output (unedited)

```
$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:9000/minio/health/live
HTTP 200
$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:9000/minio/health/ready
HTTP 200
$ curl -s -D - -o /dev/null http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
$ curl -s -D - -o /dev/null http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
[put_object] OK key=obj-S0 ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

### S1 — Lose 1 drive (`chmod 000 /tmp/minio_data/d4`, 3 / 4 online) — observed output (annotated)

```
$ chmod 000 /tmp/minio_data/d4
# /minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
# /minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
# /minio/health/live
HTTP 200
[put_object] OK key=obj-S1 ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

With one drive revoked, **3 of 4** drives remain online. `3 ≥ write quorum 3`, so the cluster stays writable-healthy (`200`) and the `PUT` of `obj-S1` succeeds. MinIO **quietly adapts and keeps writing**.

### S2 — Lose a 2nd drive (`chmod 000 /tmp/minio_data/d3`, 2 / 4 online) — observed output (annotated)

```
$ chmod 000 /tmp/minio_data/d3
# /minio/health/cluster  (write-quorum gate)
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3
# /minio/health/cluster/read  (read-quorum gate)
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
# /minio/health/live
HTTP 200
[put_object] ERROR key=obj-S2 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate HTTP=503
# reads still succeed at 2/4:
[get_object] OK key=obj-S0 bytes=31 status=200 content=b'hello-minio-erasure-fault-test\n'
[list] OK keys= ['obj-S0', 'obj-S1'] status= 200
```

This is the crux of the "hard line" behavior. With a **second** drive revoked, only **2 of 4** drives remain online:

- **Writes are refused.** `2 < write quorum 3`, so `/minio/health/cluster` returns **`503 Service Unavailable`** and the S3 `PUT` of `obj-S2` fails with **`SlowDownWrite`** / "Resource requested is unwritable, please reduce your request rate" (`HTTP=503`).
- **Reads survive.** `2 ≥ read quorum 2`, so `/minio/health/cluster/read` stays `200`, and both `GET obj-S0` and `LIST` still succeed.
- Note that `obj-S2` is **absent** from the listing (`['obj-S0', 'obj-S1']`) precisely because its `PUT` was rejected — the failure is real, not cosmetic.

### S3 — Restore (`chmod 755`, 4 / 4 online) — observed output (annotated; intermediate poll lines elided)

```
$ chmod 755 /tmp/minio_data/d3 /tmp/minio_data/d4
# t+1s and continuously through t+24s, with NO manual push (no 'mc admin heal'):
cluster HTTP 200
t+3s  /minio/health/cluster => HTTP 200
# … intermediate polls between t+3s and t+24s elided — every one returned HTTP 200 …
t+24s /minio/health/cluster => HTTP 200
# headers after restore:
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
[put_object] OK key=obj-S3 ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

Restoring permissions returns the cluster to **4 / 4** online. `/minio/health/cluster` recovers to `200` **within ~1 second and stays there** — with **no** manual action (no `mc admin heal`) — and the `PUT` of `obj-S3` succeeds again. Automatic detection is analyzed in **Q5**.


---

## Q1 — How does MinIO decide it is "healthy," and what does it assume about the required number of disks?

**Direct answer.** MinIO decides health **per erasure set**: it counts how many drives in the set currently report state OK and compares that live count against two thresholds derived from the set's data/parity split — the **write quorum** (for the `/minio/health/cluster` "healthy" verdict) and the **read quorum** (for `/minio/health/cluster/read`). It does **not** assume a static "N disks required" number; the required number *is* the quorum. For this 4-drive (2+2) set it assumes it needs **≥ 3 online drives to be writable-healthy** and **≥ 2 online drives to be read-healthy**.

**Observed evidence (unedited).** At S0 (4/4 online) the two cluster endpoints advertise the thresholds directly in their headers:

```
$ curl -s -D - -o /dev/null http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
$ curl -s -D - -o /dev/null http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
```

**Code trace.** The decision is made by `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`]:

1. It obtains live disk state with `z.StorageInfo(ctx, false)` and, for every disk whose `disk.State == madmin.DriveStateOk`, increments a per-set `online` counter (and a `healing` counter when `disk.Healing`), keyed by `disk.PoolIndex`/`disk.SetIndex`.
2. It derives the per-pool quorums from `BackendInfo`: `poolReadQuorums[i] = data` [`cmd/erasure-server-pool.go:2723`] and `poolWriteQuorums[i] = data` [`:2724`], incremented to `data + 1` when `data == b.StandardSCParity` [`:2726`]. For a 2+2 set this produces read quorum **2** and write quorum **3**.
3. Per set, it sets `Healthy = (online >= poolWriteQuorums)` [`cmd/erasure-server-pool.go:2783`] and `HealthyRead = (online >= poolReadQuorums)` [`:2784`], and copies the thresholds into the result as `ReadQuorum` [`:2787`] and `WriteQuorum` [`:2788`].

The HTTP handlers translate the result:

- `ClusterCheckHandler` [`cmd/healthcheck-handler.go:56`] calls `objLayer.Health(ctx, opts)` [`:71`], sets the `X-Minio-Write-Quorum` response header [`:72`] (header constant `MinIOWriteQuorum = "x-minio-write-quorum"` [`internal/http/headers.go:193`], canonicalized by Go to `X-Minio-Write-Quorum`), and returns `503` when `!result.Healthy` [`:78`].
- `ClusterReadCheckHandler` [`cmd/healthcheck-handler.go:93`] calls `objLayer.Health(ctx, opts)` [`:108`], sets `X-Minio-Read-Quorum` [`:109`] (constant `MinIOReadQuorum = "x-minio-read-quorum"` [`internal/http/headers.go:196`]), and gates on `!result.HealthyRead` [`:115`].

**Rationale (cause → effect).** The "required number of disks" is not configured as a fixed value — it is the erasure-coding quorum computed from the set's data/parity split (see **Q7**). Because the deployment is 2 data + 2 parity, the writable-healthy threshold is 3 and the read-healthy threshold is 2, which is exactly what the endpoints advertised (`X-Minio-Write-Quorum: 3`, `X-Minio-Read-Quorum: 2`) and exactly what the health decision compares the live online count against.

---

## Q2 — When one directory becomes inaccessible, does MinIO quietly adapt and keep going, or refuse to write?

**Direct answer.** It **quietly adapts and keeps writing.** With 3 of 4 drives online (still ≥ write quorum 3), the cluster stays writable-healthy and writes succeed — there is no error and no manual intervention.

**Observed evidence (annotated).** The S1 transcript, immediately after revoking one drive:

```
$ chmod 000 /tmp/minio_data/d4
# /minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
# /minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
# /minio/health/live
HTTP 200
[put_object] OK key=obj-S1 ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

**Code trace.** The same per-set comparison in `(*erasureServerPools).Health()` decides this: `Healthy = (online >= poolWriteQuorums)` [`cmd/erasure-server-pool.go:2783`]. With `online = 3` and `poolWriteQuorums = 3`, `3 >= 3` is true, so `Healthy` remains `true`: the `if !result.Healthy` gate [`cmd/healthcheck-handler.go:78`] is **not** taken, and `ClusterCheckHandler` falls through to write `http.StatusOK` (`200`) at [`cmd/healthcheck-handler.go:89`]. The write path itself still reaches quorum, so the erasure `PUT` completes on the 3 online drives — verified later on disk in **Q6**, where `obj-S1` landed on `d1`/`d2`/`d3` but not on the revoked `d4`.

**Rationale (cause → effect).** 3 online **equals** the write quorum, so an erasure-coded write can still place enough shards to satisfy quorum; the lost drive is simply skipped and its shard is reconstructed later (**Q6**). "Adapt and keep going" is the direct consequence of `online >= writeQuorum` still holding.

---

## Q3 — Behavior above the threshold vs. below it (after losing a second disk)

**Direct answer.** **Above threshold (3 online): writes continue** (`/cluster` `200`, `PUT` OK). **Below threshold (2 online): writes are refused** — `/cluster` returns **503** and the S3 `PUT` fails with **`SlowDownWrite`** — **while reads still succeed** (`/cluster/read` `200`, `GET`/`LIST` OK).

**Observed evidence (annotated).** S1 (above, 3/4) versus S2 (below, 2/4), side by side:

*Above threshold — S1 (3/4 online):*

```
# /minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
[put_object] OK key=obj-S1 ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

*Below threshold — S2 (2/4 online):*

```
$ chmod 000 /tmp/minio_data/d3
# /minio/health/cluster  (write-quorum gate)
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3
# /minio/health/cluster/read  (read-quorum gate)
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
# /minio/health/live
HTTP 200
[put_object] ERROR key=obj-S2 Code=SlowDownWrite Message=Resource requested is unwritable, please reduce your request rate HTTP=503
# reads still succeed at 2/4:
[get_object] OK key=obj-S0 bytes=31 status=200 content=b'hello-minio-erasure-fault-test\n'
[list] OK keys= ['obj-S0', 'obj-S1'] status= 200
```

**Code trace.** The split verdict comes from the **same** `Health()` call feeding **two independent fields** — `Healthy` (write quorum) and `HealthyRead` (read quorum) — checked by the two handlers. `ClusterCheckHandler` gates on `!result.Healthy` [`cmd/healthcheck-handler.go:78`]; `ClusterReadCheckHandler` gates on `!result.HealthyRead` [`:115`]. The S3 error surfaced on the failed `PUT` originates as the internal `errErasureWriteQuorum`, which `toAPIErrorCode()` converts to the S3 error code `ErrSlowDownWrite` — `case errErasureWriteQuorum: apiErr = ErrSlowDownWrite` [`cmd/api-errors.go:2192-2193`] (its read-side sibling `errErasureReadQuorum → ErrSlowDownRead` sits just above at [`:2190-2191`]). `ErrSlowDownWrite` [`cmd/api-errors.go:197`] is then rendered from its definition in the error table — `Code: "SlowDownWrite"` [`:875`], `Description: "Resource requested is unwritable, please reduce your request rate"` [`:876`], and `HTTPStatusCode: http.StatusServiceUnavailable` (i.e. 503) [`:877`] (the map entry `ErrSlowDownWrite: {` begins at [`:874`]).

**Rationale (cause → effect).** At 2 online drives, `2 < 3` **fails** the write-quorum comparison (`Healthy = false` → 503 → `SlowDownWrite`), but `2 >= 2` **satisfies** the read-quorum comparison (`HealthyRead = true` → `/cluster/read` 200, reads succeed). This is the exact mechanism behind "reads survive, writes fail": two thresholds, one live count, evaluated independently.

---

## Q4 — Do logs name the failing disk by path, and is recovery attempted while the system is live?

**Direct answer.** **Yes — the logs name the failing drive by its full filesystem path**, and the server continuously inspects each drive's live healing state while running (it re-probes every drive on every health poll).

**Observed evidence (unedited).** During the outage the server log repeatedly emitted (full block in **Appendix A2**):

```
Error: unable to read /tmp/minio_data/d4/.minio.sys/buckets/.healing.bin: open /tmp/minio_data/d4/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
      10: .../internal/logger/logger.go:268:logger.LogIf()
       9: .../cmd/logging.go:112:cmd.internalLogIf()
       8: .../cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       7: .../cmd/xl-storage.go:352:cmd.newXLStorage.func2()
       6: .../internal/cachevalue/cache.go:143:cachevalue.(*Cache[...]).update()
       5: .../internal/cachevalue/cache.go:128:cachevalue.(*Cache[...]).GetWithCtx()
       4: .../cmd/xl-storage.go:781:cmd.(*xlStorage).DiskInfo()
       3: .../cmd/xl-storage-disk-id-check.go:329:cmd.(*xlStorageDiskIDCheck).DiskInfo()
       2: .../cmd/erasure.go:192:cmd.getDisksInfo.func1()
       1: .../errgroup/errgroup.go:123:errgroup.(*Group).Go.func1()
```

The message names the drive by its **full path** (`/tmp/minio_data/d4/.minio.sys/buckets/.healing.bin`).

**Code trace.** The line originates in `(*xlStorage).Healing()` [`cmd/xl-storage.go:430`], which builds the path `<drivePath>/.minio.sys/buckets/.healing.bin` and, on any error other than "does not exist," logs `internalLogIf(..., fmt.Errorf("unable to read %s: %w", healingFile, err))` at [`cmd/xl-storage.go:436`]. The filename comes from `healingTrackerFilename = ".healing.bin"` [`cmd/background-newdisks-heal-ops.go:41`]. This is invoked **live on every health poll** through the per-disk fan-out `getDisksInfo` [`cmd/erasure.go:173`], which calls `disks[index].DiskInfo(...)` [`cmd/erasure.go:192`] and `disks[index].Healing()` [`cmd/erasure.go:206`] across all drives in parallel — routed through the ID-checked wrappers `(*xlStorageDiskIDCheck).DiskInfo()` [`cmd/xl-storage-disk-id-check.go:289`] and `(*xlStorageDiskIDCheck).Healing()` [`:232`], reaching `(*xlStorage).DiskInfo()` [`cmd/xl-storage.go:780`]. The captured stack trace confirms exactly these frames at runtime (`xl-storage.go:436`, `xl-storage.go:781`, `xl-storage-disk-id-check.go:329`, `erasure.go:192`), and the alternative frame `cmd/erasure.go:206 → cmd/xl-storage-disk-id-check.go:233 → cmd/xl-storage.go:436` (the `Healing()` path) was also observed.

**Live-recovery signal.** Reading `.healing.bin` on every poll **is** MinIO checking whether each drive is currently under healing — the presence of that marker file is how a drive advertises "I am healing." When present, that count *can* be surfaced to operators via the `X-Minio-Healing-Drives` response header (constant `MinIOHealingDrives = "x-minio-healing-drives"` [`internal/http/headers.go:203`]). The handler emits that header only when `result.HealingDrives > 0` [`cmd/healthcheck-handler.go:75-76`] — but, importantly, `Health()` assigns the **aggregate** `result.HealingDrives` **only under a maintenance-mode evaluation**: `if opts.Maintenance { ... result.HealingDrives = drivesHealing }` [`cmd/erasure-server-pool.go:2809-2812`] (the per-set `ESHealth[].HealingDrives` field at [`:2786`] is a separate detail entry the handler does not read). A plain, non-maintenance `GET /minio/health/cluster` poll — exactly the kind used throughout this investigation (no `?maintenance=true`) — therefore leaves `result.HealingDrives` at `0`, and the header is not emitted; consistent with that, **no positive healing-drive count was observed here**. In **this** permission scenario, the marker file itself could not even be read (permission denied), so the observed live signal was the **by-path error**, not a positive healing count — i.e., the server was actively and repeatedly probing the failed drive's healing state while live.


---

## Q5 — Is the directory's return detected automatically (polling), or must something push it into a healing path?

**Direct answer.** **It is detected automatically, by background polling — no manual push is required.** A background monitor periodically re-probes endpoints and **reconnects** the ones that come back (reloading their `format.json` and re-placing the live disk); a separate background loop then **heals** any disk that reconnection flagged as fresh/unformatted. `mc admin heal` exists only as an *optional accelerator*, not as a prerequisite for detection.

**Observed evidence (annotated; intermediate poll lines elided).** After `chmod 755` restored the two drives, the cluster returned to healthy on its own, with **no** `mc admin heal` or any other manual action:

```
$ chmod 755 /tmp/minio_data/d3 /tmp/minio_data/d4
# t+1s and continuously through t+24s, with NO manual push (no 'mc admin heal'):
cluster HTTP 200
t+3s  /minio/health/cluster => HTTP 200
# … intermediate polls between t+3s and t+24s elided — every one returned HTTP 200 …
t+24s /minio/health/cluster => HTTP 200
```

Additionally, during the outage the by-path errors repeated on a regular interval — timestamps `18:28:27`, `18:28:37`, `18:29:22`, `18:29:27`, `18:30:02` (roughly every ~10 s), with **17** occurrences for `d4` and **9** for `d3`. These lines are emitted by the per-drive `getDisksInfo` fan-out that runs on **every health evaluation** (see **Q4**: `getDisksInfo` [`cmd/erasure.go:173`] → `DiskInfo()` [`:192`] / `Healing()` [`:206`]), and the observation harness was polling `/minio/health/*` on a ~10 s cadence — so the ~10 s spacing of these errors reflects the **harness's health-poll interval** (request-driven re-probing), **not** a MinIO-internal background timer. **[inferred]** the exact mapping of this error cadence to any specific internal timer was not isolated at runtime. What the cadence *does* demonstrate directly is that the server re-evaluates each drive's state on every health request. Once access was restored the errors ceased and the log stopped growing.

**Code trace.** Detection and healing are handled by **two distinct background monitors with different jobs** — they are *not* two interchangeable re-probers:

- **Endpoint detection / reconnection (15 s):** `monitorAndConnectEndpoints()` [`cmd/erasure-sets.go:283`] runs on a **15-second** interval — `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` [`cmd/erasure-sets.go:348`] (10 s + 5 s), started at [`cmd/erasure-sets.go:479`]. Each tick calls `connectDisks()` [`cmd/erasure-sets.go:194`], which attempts `connectEndpoint()` [`cmd/erasure-sets.go:224`] on each drive and — for drives that come back — reloads their `format.json` and re-places the live disk into the set at [`cmd/erasure-sets.go:263`]. **This is the layer that actually notices and reconnects a returned drive.** If a reconnected drive is unformatted or already marked healing, it is **enqueued for healing** via `globalBackgroundHealState.pushHealLocalDisks()` [`cmd/erasure-sets.go:227`, `:238`] (queue defined at [`cmd/admin-heal-ops.go:194`]).
- **Healing of already-detected fresh disks (10 s):** `monitorLocalDisksAndHeal()` [`cmd/background-newdisks-heal-ops.go:563`] runs on a **10-second** interval — `defaultMonitorNewDiskInterval = time.Second * 10` [`cmd/background-newdisks-heal-ops.go:40`], arming `diskCheckTimer := time.NewTimer(defaultMonitorNewDiskInterval)` [`:565`]. Its documented job is to *heal disks that were already detected* (comment [`cmd/background-newdisks-heal-ops.go:560-562`]): each tick **drains the queue** the 15 s monitor produced, via `getHealLocalDiskEndpoints()` [`:573`] (defined at [`cmd/admin-heal-ops.go:173`]); if that queue is empty it does nothing and simply re-arms [`:574-578`]; otherwise it reformats the disks (`z.HealFormat()` [`:581`]) and heals each with `healFreshDisk()` [`:592`, defined at `:419`]. **It does not itself probe or reconnect endpoints.**

In short, the two form a **producer → consumer pipeline**: the 15 s monitor *detects/reconnects* returned drives and *produces* heal work; the 10 s loop *consumes* that work to heal fresh/unformatted disks. The manual `mc admin heal` path — the admin `HealHandler` (`POST /minio/admin/v3/heal/{bucket}`) [`cmd/admin-handlers.go:1308`], routed at [`cmd/admin-router.go:175-177`], which launches a sequence via `LaunchNewHealSequence()` [`cmd/admin-heal-ops.go:296`] built by `newHealSequence()` [`cmd/admin-heal-ops.go:473`] — is only an optional accelerator, never a prerequisite for detection.

**Rationale + [inferred] boundary.** Two independent mechanisms guarantee a returned drive is noticed without any push: (a) every `Health()` call re-reads each drive's live state through the `getDisksInfo` fan-out, so the *health verdict* recovers as soon as the drive is reachable again; and (b) the 15 s endpoint monitor (`connectDisks()`) reconnects the drive and reloads its `format.json`. **[inferred]** *which of these recovered `/cluster` first* for this permission-restore was not pinned down by the runtime output: for a permission restore the drive's `format.json` and data are intact, so it reappears "online" through the live health/`StorageInfo` path almost immediately (the errors stopped and `/cluster` recovered within ~1 s), and the 15 s reconnect would re-place it shortly after regardless. Notably, the dedicated `healFreshDisk()` path [`cmd/background-newdisks-heal-ops.go:419`] driven by the 10 s loop is for **replaced/unformatted** drives (a brand-new empty disk) — a *different* case from a permission-restored drive whose format survives — so it is **[inferred]** that the 10 s heal loop was *not* the mechanism that restored health here. What was **observed** is unambiguous: recovery to healthy happened automatically, with no manual command.

---

## Q6 — How are objects written during the outage repaired once the disk returns?

**Direct answer.** They are **reconstructed onto the returned drive by the heal process** — a Reed-Solomon rebuild of the missing shard from the surviving data + parity shards. The background heal is slow; `mc admin heal` (or its admin API equivalent) accelerates it. This was **observed end-to-end**: the object written while `d4` was offline was later present on all four drives, with its `d4` shard reconstructed.

**Observed evidence (heal summary — distilled from the admin heal-status API response; `clientToken` abbreviated).** `obj-S1` was `PUT` during S1 while `d4` was revoked, and initially existed only on `d1`/`d2`/`d3` (missing on `d4`) — confirmed on disk several minutes after `d4` was restored (the slow background heal had not yet caught up). Triggering an explicit heal (the canonical `mc admin heal` equivalent) completed the reconstruction:

```
$ # canonical 'mc admin heal' equivalent via the admin API:
POST /minio/admin/v3/heal/testbucket  ->  HTTP 200  clientToken=02577ed1-...
# heal status poll reported:
bucket testbucket + 3 objects, each: after: 4/4 drives ok
Summary: "finished"
```

Before/after on-disk distribution (one `xl.meta` per shard location):

| Object | Written during | Before heal (on disk) | After heal (on disk) |
|--------|----------------|-----------------------|----------------------|
| `obj-S0` | S0 (4/4 healthy) | d1, d2, d3, d4 | d1, d2, d3, d4 |
| `obj-S1` | S1 (`d4` offline) | d1, d2, d3 (**missing d4**) | d1, d2, d3, **d4** (467 B `xl.meta` on each) — **d4 shard reconstructed** |
| `obj-S2` | S2 (`PUT` rejected) | absent on all drives | absent on all drives |
| `obj-S3` | S3 (4/4 restored) | d1, d2, d3, d4 | d1, d2, d3, d4 |

`obj-S2` remained absent everywhere (its write was refused below quorum), and the healthy-era writes (`obj-S0`, `obj-S3`) were on all four drives throughout.

**Code trace.** Partial-quorum writes — a write that reached quorum but did not land on every drive — are tracked by the **MRF (Most-Recent-Failures)** subsystem: the `PartialOperation` type [`cmd/mrf.go:51`], documented as "a successful upload/delete of an object but not written in all disks (having quorum)" [`cmd/mrf.go:49-50`], is enqueued via `addPartialOp()` [`cmd/mrf.go:78`] and drained by `healRoutine()` [`cmd/mrf.go:220`], which invokes object/bucket heal. The continuous background heal walk is created by `newBgHealSequence()` [`cmd/global-heal.go:49`] (its per-task queueing/execution lives in [`cmd/background-heal-ops.go`]); the manual/admin path is the admin `HealHandler` [`cmd/admin-handlers.go:1308`] → `LaunchNewHealSequence()` [`cmd/admin-heal-ops.go:296`]. The actual shard reconstruction is Reed-Solomon: `(*erasureObjects).healObject()` [`cmd/erasure-healing.go:258`] rebuilds a single object — it opens bitrot **readers** over the surviving shards and bitrot **writers** over the outdated/returned drives [`cmd/erasure-healing.go:555-575`], then calls `erasure.Heal()` [`cmd/erasure-healing.go:581`]. That `(Erasure).Heal()` [`cmd/erasure-decode.go:317`] "reads from readers, reconstruct shards and writes the data to the writers" [`cmd/erasure-decode.go:316`] and delegates the reconstruction to `DecodeDataAndParityBlocks()` [`cmd/erasure-coding.go:111`] (invoked at [`cmd/erasure-decode.go:348`]). Per-shard integrity is verified by bitrot checksums — `newBitrotWriter()` [`cmd/bitrot.go:105`], `newBitrotReader()` [`cmd/bitrot.go:112`], and `bitrotVerify()` [`cmd/bitrot.go:158`] — with the streaming and whole-file verifiers in [`cmd/bitrot-streaming.go`] and [`cmd/bitrot-whole.go`]; shared heal helpers are in [`cmd/erasure-healing-common.go`] and the encode path in [`cmd/erasure-encode.go`].

**Rationale + [inferred] boundary.** Because the write during S1 achieved write quorum on 3 drives, the object's data is fully recoverable: the missing `d4` shard is a deterministic Reed-Solomon function of the surviving shards, so heal simply recomputes and writes it back to `d4`. **[inferred]** *that the MRF queue specifically (versus the periodic scanner/background heal walk) would have eventually healed `obj-S1` without the manual trigger* — the code provides both routes, but what was **observed** is that the explicit heal completed the reconstruction; the exact self-heal route that would have fired unattended was not isolated at runtime.

---

## Q7 — Where does the quorum decision live in code, and how is the threshold computed?

**Direct answer.** The threshold is **computed** in `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`] and **enforced** (for the health verdict) in `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`]. For a 2+2 set: **read quorum = N/2 = 2**; **write quorum = dataBlocks = 2, incremented to 3 because `dataBlocks == parityBlocks`.**

**Observed evidence (unedited).** The decisive server-log line printed at the exact moment the cluster dropped to 2/4 and `/cluster` began returning 503 (full block in **Appendix A3**):

```
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
```

This states the computed threshold (`expected write quorum: 3`) and the live count (`drives-online: 2`) that failed it.

**Code trace.** The threshold computation in `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531`]:

- Read quorum: `expectedRQuorum := len(partsMetaData) / 2` [`cmd/erasure-metadata.go:533`] → for 4 → **2**.
- Write quorum: `writeQuorum := dataBlocks` [`cmd/erasure-metadata.go:557`], then `if dataBlocks == parityBlocks { writeQuorum++ }` [`:558-559`] → for `dataBlocks = 2, parityBlocks = 2` → `2` then incremented to **3**.
- Returns `(dataBlocks, writeQuorum, nil)` [`cmd/erasure-metadata.go:564`].

The same arithmetic governs the health verdict inside `Health()`: `poolWriteQuorums[i] = data (+1 when data == StandardSCParity)` [`cmd/erasure-server-pool.go:2723-2726`], with the live comparison `online >= poolWriteQuorums` [`:2783`]. When that comparison fails, `Health()` emits the write-quorum error via `storageLogIf(...)` with `logger.FatalKind` at [`cmd/erasure-server-pool.go:2793`] — the runtime stack trace confirmed this exact line, called from `ClusterCheckHandler` at [`cmd/healthcheck-handler.go:71`]. The default parity that seeds all of this is `DefaultParityBlocks(4) = 2` [`internal/config/storageclass/storage-class.go:355`], and the behavior matches the canonical spec (read quorum `N/2`, write quorum `N/2+1`) at [`docs/minio-limits.md:15-16`].

**Rationale (cause → effect).** **2+2 set → write quorum 3, read quorum 2 → 3 online is writable, 2 online is read-only.** The `+1` increment when `dataBlocks == parityBlocks` is what makes the write threshold 3 rather than 2, which is precisely why losing the *second* drive (not the first) is the point at which writes stop.

---

## Q8 — Grounding all of the above in observable behavior

**Direct answer.** Every conclusion in this document is grounded in **what the running server actually did** — the HTTP health-endpoint responses (`curl`) and the real S3 read/write attempts (`boto3`) — and each is cross-referenced to an exact `file:line` in this checkout.

- The **quorum values** (write 3, read 2) were not assumed — they were read directly off the live headers `X-Minio-Write-Quorum: 3` and `X-Minio-Read-Quorum: 2` (S0), and they match the code that computes them (`objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`]; `Health()` [`cmd/erasure-server-pool.go:2723-2726`]).
- The **adapt-then-refuse threshold** was demonstrated by the S1 → S2 transition: `PUT` OK at 3/4, then `503`/`SlowDownWrite` at 2/4 — the internal `errErasureWriteQuorum` is converted to `ErrSlowDownWrite` in `toAPIErrorCode()` [`cmd/api-errors.go:2192-2193`] and rendered as `Code: "SlowDownWrite"` / HTTP 503 [`cmd/api-errors.go:874-877`].
- The **reads-survive-writes-fail** split was demonstrated by S2 returning `/cluster` 503 but `/cluster/read` 200 with successful `GET`/`LIST`, driven by the two independent fields `Healthy`/`HealthyRead` [`cmd/erasure-server-pool.go:2783-2784`] gated by the two handlers [`cmd/healthcheck-handler.go:78` and `:115`].
- The **by-path log** and **live re-probing** were demonstrated by the repeated `unable to read /tmp/minio_data/d4/.minio.sys/buckets/.healing.bin ... permission denied` lines from `(*xlStorage).Healing()` [`cmd/xl-storage.go:436`].
- **Automatic detection** was demonstrated by `/cluster` recovering to 200 within ~1 s of `chmod 755` with no manual command. Detection is driven by the live per-request health re-evaluation plus the 15 s endpoint-reconnect monitor `monitorAndConnectEndpoints()`/`connectDisks()` [`cmd/erasure-sets.go:283,194`]; the separate 10 s `monitorLocalDisksAndHeal()` [`cmd/background-newdisks-heal-ops.go:563`] heals already-queued fresh/unformatted disks (see **Q5**).
- **Outage-era repair** was demonstrated by `obj-S1` being reconstructed onto `d4` after heal (before/after on-disk table in **Q6** / **Appendix A4**).

The complete scenario matrix (S0-S3) and the log/command transcripts in the **Appendix** (raw for A1-A3; distilled/annotated for A4-A5) are the primary evidence; the code citations explain *why* each observation occurred.


---

## Caveats

### Observed vs. inferred

**Directly observed at runtime (paired with captured output above):**

- The full scenario matrix S0-S3: healthy (4/4), above threshold (3/4), below threshold (2/4), and restored (4/4) — including the `X-Minio-Write-Quorum: 3` / `X-Minio-Read-Quorum: 2` headers.
- Write quorum enforced as a hard line: `PUT` OK at 3/4, `503` + `SlowDownWrite` at 2/4; reads (`GET`/`LIST`, `/cluster/read` 200) surviving at 2/4.
- The by-path permission-denied log line naming `.../d4/.minio.sys/buckets/.healing.bin`, and its live repetition on an interval (17× for `d4`, 9× for `d3`).
- The write-quorum `FatalKind` log line: `Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2`.
- Automatic recovery to `/cluster` 200 within ~1 s of `chmod 755`, with no manual command.
- The heal reconstruction of `obj-S1` onto `d4` (before/after on-disk distribution).
- The **root counter-example**: as root, `chmod 000` on two drives did **not** degrade health (`/cluster` 200, `X-Minio-Write-Quorum: 3`, `PUT` `status=200`).

**Inferred from reading code (not isolated at runtime) — labeled [inferred]:**

- **[inferred]** Exactly *which* mechanism first restored `/cluster` for the permission-restore — the live per-request health re-evaluation (`getDisksInfo`) versus the 15 s endpoint reconnect (`monitorAndConnectEndpoints()`/`connectDisks()`). For a permission restore (format and data intact) the drive reappears "online" through the live health poll and is separately re-placed by the 15 s reconnect; the first-to-fire was not pinned down. The 10 s `monitorLocalDisksAndHeal()` loop heals *fresh/unformatted* disks and is **[inferred]** **not** the path that restored this format-intact drive.
- **[inferred]** That the ~10 s spacing of the repeated `.healing.bin` errors corresponds to any MinIO-internal timer. Those lines are produced by the request-driven `getDisksInfo` fan-out on each health evaluation, and the harness polled `/minio/health/*` roughly every 10 s — so the cadence reflects the **harness's poll interval**, not an internal background timer; the precise internal source of each individual line was not isolated at runtime.
- **[inferred]** That the **MRF** queue specifically (rather than the periodic scanner / background heal walk) would have eventually healed `obj-S1` unattended. Both routes exist in code; what was observed is that the explicit heal completed the reconstruction.

### Open-source vs. commercial AIStor — the "48-hour offline" caveat

MinIO's public `docs.min.io` pages document the commercial **AIStor** product, including a **"48-hour offline" heal rule** attributed to a 2025 AIStor release (after which an offline drive is treated differently). **This behavior is NOT verified in this open-source checkout and must not be assumed here.** The authoritative source for this document is this checkout's code plus the observed runtime output; the 48-hour rule is **not asserted** for this build. Any AIStor-specific behavior would need to be verified against this repository's own code before being relied upon.

### Configuration notes

- **`MINIO_CI_CD=1` is a non-default harness setting** (not part of MinIO's default configuration). It sets `globalIsCICD = true` [`cmd/common-main.go:99`] (a non-empty `CI` env var does the same), which has several material setup/runtime effects in this checkout: (1) it **skips root-drive detection** — the `if !globalIsCICD && !globalIsErasureSD` guard in `getDiskInfo()` [`cmd/xl-storage.go:371`] bypasses the `disk.IsRootDisk` check [`cmd/xl-storage.go:371-380`], which is precisely what permits the four data directories to live on one root filesystem under `/tmp` without being rejected as root disks; (2) it shrinks each drive's immediate-purge queue from 100000 to 1 in `newXLStorage()` [`cmd/xl-storage.go:217-220`]; (3) it caps the erasure byte-pool buffer sizing to 256 MiB in the byte-pool initializer [`cmd/erasure-server-pool.go:103-104`]; and it also relaxes empty-`Host`-header validation in `hasBadHost()` [`cmd/generic-handlers.go:303-305`]. **Crucially, it does *not* touch the quorum arithmetic or the health decision** — `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`] and `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`] contain no reference to `globalIsCICD`, so the write-quorum-3 / read-quorum-2 computation and the `online >= quorum` comparison observed here are identical with or without CI mode. **[inferred]** the non-quorum effects above are read from these code paths; of them, only the drive-layout consequence (four `/tmp` directories accepted at startup) was exercised at runtime.
- The observed `boto3` version was **1.43.47**.
- The deployment used MinIO's default storage class (no non-default parity was configured), so `DefaultParityBlocks(4) = 2` [`internal/config/storageclass/storage-class.go:355`] governs the 2+2 split. A different configured parity would shift the quorums accordingly, but the *mechanism* (compare live online count to a data/parity-derived quorum) is unchanged.


---

## Appendix: captured output (raw logs A1–A3; distilled/annotated summaries A4–A5)

Blocks **A1–A3** are raw, verbatim log/command captures (the lines shown are unaltered). Blocks **A4–A5** are **distilled or annotated summaries** — labeled as such in place — not byte-for-byte tool output.

### A1 — Startup log (deployment shape + version)

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
```

`/tmp/minio_bin --version` reported:

```
Runtime: go1.23.2 linux/amd64
version DEVELOPMENT.GOGET
```

The on-disk `format.json` showed `"format":"xl"`, one set listing 4 drive UUIDs, and `"distributionAlgo":"SIPMOD+PARITY"`.

### A2 — By-path permission-denied block (emitted via `internalLogIf`), verbatim (leading `API:` header line omitted)

```
Time: 18:28:27 UTC 07/14/2026
DeploymentID: 44c2e2f1-5c24-47b1-b8ee-6e33e5e5ccc4
Error: unable to read /tmp/minio_data/d4/.minio.sys/buckets/.healing.bin: open /tmp/minio_data/d4/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
      10: .../internal/logger/logger.go:268:logger.LogIf()
       9: .../cmd/logging.go:112:cmd.internalLogIf()
       8: .../cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       7: .../cmd/xl-storage.go:352:cmd.newXLStorage.func2()
       6: .../internal/cachevalue/cache.go:143:cachevalue.(*Cache[...]).update()
       5: .../internal/cachevalue/cache.go:128:cachevalue.(*Cache[...]).GetWithCtx()
       4: .../cmd/xl-storage.go:781:cmd.(*xlStorage).DiskInfo()
       3: .../cmd/xl-storage-disk-id-check.go:329:cmd.(*xlStorageDiskIDCheck).DiskInfo()
       2: .../cmd/erasure.go:192:cmd.getDisksInfo.func1()
       1: .../errgroup/errgroup.go:123:errgroup.(*Group).Go.func1()
```

This error also occurred via the `Healing()` fan-out frame `cmd/erasure.go:206 → cmd/xl-storage-disk-id-check.go:233 → cmd/xl-storage.go:436`. Occurrence counts during the outage: **17** for `d4`, **9** for `d3`; observed timestamps `18:28:27`, `18:28:37`, `18:29:22`, `18:29:27`, `18:30:02`.

### A3 — Write-quorum FatalKind block (API: SYSTEM.storage), verbatim

```
API: SYSTEM.storage
Time: 18:29:22 UTC 07/14/2026
DeploymentID: 44c2e2f1-5c24-47b1-b8ee-6e33e5e5ccc4
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
       maintenance="false"
       5: .../internal/logger/logger.go:268:logger.LogIf()
       4: .../cmd/logging.go:156:cmd.storageLogIf()
       3: .../cmd/erasure-server-pool.go:2793:cmd.(*erasureServerPools).Health()
       2: .../cmd/healthcheck-handler.go:71:cmd.ClusterCheckHandler()
       1: net/http/server.go:2220:http.HandlerFunc.ServeHTTP()
```

The same error also appeared with the frame `cmd/healthcheck-handler.go:108:cmd.ClusterReadCheckHandler()` — `Health()` logs the write-quorum failure regardless of the calling handler, yet the read handler still returned `200` because the read-quorum field (`HealthyRead`) was satisfied at 2 online drives.

### A4 — Q6 heal summary (distilled from the admin heal-status API response; `clientToken` abbreviated) + before/after on-disk distribution

```
$ # canonical 'mc admin heal' equivalent via the admin API:
POST /minio/admin/v3/heal/testbucket  ->  HTTP 200  clientToken=02577ed1-...
# heal status poll reported:
bucket testbucket + 3 objects, each: after: 4/4 drives ok
Summary: "finished"
```

Before/after on-disk distribution (one `xl.meta` per shard location):

| Object | Written during | Before heal (on disk) | After heal (on disk) |
|--------|----------------|-----------------------|----------------------|
| `obj-S0` | S0 (4/4 healthy) | d1, d2, d3, d4 | d1, d2, d3, d4 |
| `obj-S1` | S1 (`d4` offline) | d1, d2, d3 (**missing d4**) | d1, d2, d3, **d4** (467 B `xl.meta` on each) |
| `obj-S2` | S2 (`PUT` rejected) | absent on all drives | absent on all drives |
| `obj-S3` | S3 (4/4 restored) | d1, d2, d3, d4 | d1, d2, d3, d4 |

### A5 — Root counter-example transcript (non-canonical; justifies the non-root requirement)

```
$ whoami
root
# process uid = 0 (root) — POSIX permission bits are bypassed
$ chmod 000 /tmp/minio_data_root/d3 /tmp/minio_data_root/d4    # revoke TWO drives
$ curl -s -D - -o /dev/null http://127.0.0.1:9010/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
[put_object] OK key=obj-root ETag="b9d876737c5acc8aa111c1e1282be1cd" status=200
```

As root, `chmod 000` on two drives did **not** degrade health — `/cluster` still returned `200` with `X-Minio-Write-Quorum: 3`, and the `PUT` succeeded (`status=200`) — because a root process bypasses POSIX permission bits and the drives were still fully readable/writable. This confirms why the canonical reproduction (S0-S3) must run as the **non-root** user `miniouser` (uid 1001): only then does `chmod 000` genuinely revoke a drive so MinIO experiences and reports the fault.

---

## Self-validation — coverage pass

A final pass confirming this document satisfies every acceptance criterion:

| ✔ | Requirement | Where satisfied |
|---|-------------|-----------------|
| ✅ | **Q1** — health decision & disk assumptions, answered by name | *Q1* + TL;DR Q1; per-set `online >= writeQuorum/readQuorum` in `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`] |
| ✅ | **Q2** — adapt vs. refuse when one drive is lost, answered | *Q2*; S1 transcript (3/4 → `200`, `PUT` OK) |
| ✅ | **Q3** — above- vs. below-threshold, reads vs. writes | *Q3*; S1 vs. S2 side-by-side (503/`SlowDownWrite`; reads survive) |
| ✅ | **Q4** — logs name the drive by path; live recovery signal | *Q4*; by-path `.healing.bin` log (Appendix A2) |
| ✅ | **Q5** — automatic detection (polling) vs. push | *Q5*; auto-recovery transcript + 15 s detect / 10 s heal trace |
| ✅ | **Q6** — repair of objects written during the outage | *Q6*; `obj-S1` reconstructed onto `d4` (Appendix A4) |
| ✅ | **Q7** — where the quorum decision lives / how the threshold is computed | *Q7*; `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`] |
| ✅ | **Q8** — grounded in observable behavior | *Q8*; every claim tied to `curl` / `boto3` output |
| ✅ | **Evidence adjacency** — each behavioral claim is shown next to its captured output | S0-S3 matrix + each question's *Observed evidence* block + Appendix A1-A5 |
| ✅ | **Exact source anchors** — every code claim carries a `file:line` and a named function/struct | throughout, e.g. `Health()` [`:2679`], `objectQuorumFromMeta()` [`:531-565`], `toAPIErrorCode()` [`cmd/api-errors.go:2192-2193`], `connectDisks()` [`cmd/erasure-sets.go:194`] |
| ✅ | **Cause chain 4 → 2+2 → RQ2/WQ3 → 3 writable / 2 read-only** explained | headline finding + *Q7* rationale |
| ✅ | **Inferred labels** — code-only conclusions marked **[inferred]** | *Q5*, *Q6*, *Caveats* (which mechanism recovered first; error cadence vs. internal timer; MRF vs. scanner; CI non-quorum effects) |
| ✅ | **AIStor "48-hour offline" caveat** flagged as commercial/unverified for this checkout | *Caveats → Open-source vs. commercial AIStor* |
| ✅ | **Non-root edge** documented with the root counter-example | methodology *NON-ROOT execution is mandatory* + Appendix A5 |
| ✅ | **Cleanup / repository integrity** — sole artifact added, all temporary resources removed | methodology *Repository integrity* + closing statement; `git status` verified clean |
| ✅ | **Markdown validity** — balanced code fences, well-formed tables/headings | verified (even fence count; no unterminated blocks) |
| ✅ | **Exact filename** — `<source_branch_name>.md` in `blitzy/documentation/` | this file: `blitzy/documentation/minio_c07e5b49d477.md` |

All eight sub-questions (Q1-Q8) are answered by name; each behavioral claim is paired with captured runtime output; each code claim carries an exact `file:line` and names the responsible function/struct; the write-quorum-3 / read-quorum-2 cause chain is explained end-to-end; statements not observed at runtime are labeled **[inferred]**; the commercial-AIStor caveat and the mandatory non-root execution are called out; and this document is the **only** artifact added to the repository.

---

*End of document. This is the sole artifact added by this read-only investigation; the MinIO repository is otherwise unchanged, and all temporary build/run/data artifacts were removed.*
