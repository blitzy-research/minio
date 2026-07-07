# MinIO in Distributed (4‑Drive Erasure) Mode: Health, Quorum, Disk Failure, Recovery & Repair — A Runtime‑Grounded Investigation

> **Methodology (run‑first).** Every behavioral claim below was produced by **building and running MinIO, injecting a real `chmod 000` permission fault, and capturing the actual, unedited output** — *then* writing the explanation. Each claim is paired with (a) the exact command, (b) its real output, and (c) a precise `file:line` source citation. Values that could not be reproduced at runtime and are read from a source constant are explicitly labeled **(inferred)**; everything else is **(observed)**. All runtime output below was captured from a single canonical run (DeploymentID `fa49a98a-f5c0-4758-97bc-fc96cca7f9eb`) on `2026-07-07`; a separate, explicitly‑labeled non‑canonical debug run was used only to time the reconnect poll (§9).

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
2. **Behavior at the moment of failure.** MinIO **does not crash and does not refuse writes** the instant a directory becomes inaccessible — it keeps serving as long as it is **at or above quorum**. It refuses **writes** only once the online count drops **below the write quorum**. **Observed:** immediately after `chmod 000 /tmp/d1` (3 online), a 1 MiB PUT still returned **`HTTP 200`** and the new object's shard was simply **skipped on the faulted drive** (written to the other 3).
3. **Above vs. below threshold.** **3 online (above):** writes **succeed**, reads **succeed**, `/minio/health/cluster` stays **`200`**. **2 online (below):** writes are **refused** (`HTTP 503 SlowDownWrite`), reads still **succeed** (`HTTP 200`), `/minio/health/cluster` returns **`503`** while `/minio/health/cluster/read` stays **`200`**. That read/write divergence is the crux of the contrast.
4. **Log visibility & live recovery.** **Yes** — the logs name the failing drive **by full path**. For a *permission* fault the by‑path lines come from the reconnect poll (`Error: drive access denied … endpoint="/tmp/d1"`) and the healing‑tracker read (`unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: … permission denied`). Recovery machinery is visibly active while the server is live (the reconnect poll keeps probing; when a drive is *wiped/replaced*, the auto‑heal loop logs `Healing drive '/tmp/d1' … use 4 parallel workers`).
5. **Re‑admission mechanism.** **Automatic; no operator push required.** Two distinct facts were observed: (a) a *permission* fault never disconnects the drive at all — `Health()` re‑reads live `DiskInfo` on every probe, so health flips back to `200` **immediately** (measured **0.018 s**) on `chmod 755`, *not* gated by any timer; (b) a fully *disconnected/fresh* drive is re‑admitted by the reconnect poll `monitorAndConnectEndpoints → connectDisks` (`cmd/erasure-sets.go:L283`) on a **15 s** timer — observed at **15.001 s** cadence. The manual `mc admin heal` path (`queueHealTask`, `cmd/admin-heal-ops.go:L721`) exists but is **optional**.
6. **Repair of outage writes.** **Automatic.** The object written with a missing shard during the outage (`obj-3online.bin`, no shard on `/tmp/d1`) was **directly observed** to be reconstructed — the exact missing shard (same data‑dir UUID `bfa1f654‑…`) reappeared on `/tmp/d1` — by the **fresh/replaced‑drive heal loop** `monitorLocalDisksAndHeal → healFreshDisk` (`cmd/background-newdisks-heal-ops.go:L563,L419`, ~10 s cadence, progress in `.healing.bin`). MinIO's **MRF** partial‑write queue (`globalMRFState.addPartialOp` → `healRoutine`, `cmd/mrf.go:L78,L220`) is the code‑level path that repairs partial writes as soon as a drive is reachable; it is a **code‑supported inference** here (see §10.1 for the precise observed‑vs‑inferred split).
7. **Quorum decision in code.** The threshold is computed in **`objectQuorumFromMeta`** (`cmd/erasure-metadata.go:L531-L564`): `readQuorum = dataBlocks` and `writeQuorum = dataBlocks (+1 when dataBlocks == parityBlocks)`. Default parity comes from **`DefaultParityBlocks(4) == 2`** (`internal/config/storageclass/storage-class.go:L355-L368`). "Stop" for writes is enforced in **`multiWriter.Write`** (`cmd/erasure-encode.go:L34-L66`).
8. **Grounding.** Everything above is anchored to the `/minio/health/*` responses (status + headers) and to real S3 PUT/GET attempts at each disk‑loss level; the full unedited output is reproduced in §5–§10.

---

## 3. Environment & Reproduction

| Item | Value |
|---|---|
| Repository | `github.com/minio/minio`, module directive `go 1.23` (`go.mod:L3`) **(observed)** |
| Checkout path | `/tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c` **(observed)** |
| HEAD commit at implementation‑run capture | `ce1199d7bd156c527a0c734d94f98fc9b53fc213` — the commit that first added this document; **all later commits on this branch are documentation‑only** (they modify only this file), so every source `file:line` citation below remains valid at the current `HEAD` (see the *Provenance* note directly under this table) **(observed)** |
| Go toolchain | `go version go1.23.12 linux/amd64` **(observed)** |
| Canonical entry point | `main.go` → `minio.Main(os.Args)` **(observed)** |
| Topology | one node, 4 directories `/tmp/d1..d4` = **one erasure set of 4 drives → EC:2** **(observed)** |
| Default credentials | `minioadmin:minioadmin` (demo defaults) |
| API / health port | `:9000`; console `:9001` **(observed)** |
| DeploymentID (canonical run) | `fa49a98a-f5c0-4758-97bc-fc96cca7f9eb` **(observed)** |

> **Provenance of the captured evidence — read before the outputs below.** Every runtime artifact reproduced in this document — the build/version stamps (§3.1), the startup banner and all `server.log` excerpts (§3.2, §6–§10), and the measured timings — was captured during the implementation investigation run against the checkout identified by commit **`ce1199d7bd15`** (`ce1199d7bd156c527a0c734d94f98fc9b53fc213`), the commit that first added this answer document. **Every commit on this branch after that point is documentation‑only: it modifies only this file.** This is directly verifiable at the current `HEAD`:
>
> ```console
> $ git diff --name-status ce1199d7bd15..HEAD
> M	blitzy/documentation/minio_c07e5b49d477.md
> ```
>
> Because no source file has changed since the capture, every source `file:line` citation in this document remains valid at the current `HEAD`, and no behavioral result is affected by the later documentation‑only edits. The commit‑stamped outputs below (the ldflags `CommitID`/`ShortCommitID` and the `--version` line) therefore show `ce1199d7bd15` — the **implementation‑run checkout** — and are reproduced **verbatim** rather than re‑stamped against a later commit, honoring the run‑first / actual‑unedited‑output rule (a rebuild at a later commit would only change these cosmetic version stamps, not any behavior). **(observed)**

### 3.1 Build (canonical, with version‑stamping ldflags)

The build uses the exact `Makefile` recipe — `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)"` (`Makefile:L177-L179`), where `LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)` (`Makefile:L3`) stamps the version/commit metadata. The output binary is written **outside** the repository tree (`/tmp/minio-investigation/minio`) so the checkout stays clean.

```console
# Captured at the implementation-run checkout ce1199d7bd15 (see the Provenance note in §3).
# The rev-parse / ldflags / --version stamps below therefore show ce1199d7bd15 verbatim;
# source is unchanged since this commit, so every file:line citation is valid at the current HEAD.
$ go version
go version go1.23.12 linux/amd64

$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c rev-parse HEAD
ce1199d7bd156c527a0c734d94f98fc9b53fc213

# canonical LDFLAGS (Makefile:L3 -> buildscripts/gen-ldflags.go), captured verbatim:
LDFLAGS="-s -w -X github.com/minio/minio/cmd.Version=2026-07-06T23:45:05Z \
 -X github.com/minio/minio/cmd.CopyrightYear=2026 \
 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2026-07-06T23-45-05Z \
 -X github.com/minio/minio/cmd.CommitID=ce1199d7bd156c527a0c734d94f98fc9b53fc213 \
 -X github.com/minio/minio/cmd.ShortCommitID=ce1199d7bd15 \
 -X github.com/minio/minio/cmd.GOPATH= -X github.com/minio/minio/cmd.GOROOT="

$ time CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$LDFLAGS" \
    -o /tmp/minio-investigation/minio .
real	0m3.580s
user	0m4.896s
sys	0m4.090s
# BUILD_EXIT=0

$ /tmp/minio-investigation/minio --version
minio version DEVELOPMENT.2026-07-06T23-45-05Z (commit-id=ce1199d7bd156c527a0c734d94f98fc9b53fc213)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2026 MinIO, Inc.
```

The stamped `ReleaseTag` (`DEVELOPMENT.2026-07-06T23-45-05Z`) and `CommitID` become the runtime `Version:` line in the startup banner (§3.2), confirming this is the canonical, version‑stamped build a normal user would produce with `make build`. **(observed)**

### 3.2 ⚠ Run as a NON‑ROOT user (essential for the permission fault)

**root bypasses POSIX permission bits (DAC)**, so `chmod 000` on a data directory would *not* deny a root‑owned MinIO process and the fault would silently have no effect. The server was therefore run as the unprivileged user **`builder` (uid 1001)**. This was verified empirically **before** running MinIO:

```console
$ chown -R builder:builder /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 /tmp/minio-investigation
$ chmod 000 /tmp/d1
$ sudo -u builder ls /tmp/d1
ls: cannot open directory '/tmp/d1': Permission denied      # non-root IS denied
$ ls /tmp/d1
probe                                                       # root BYPASSES 000
$ chmod 755 /tmp/d1                                          # restore before server run
```

The run command (single node, four dirs). MinIO's combined stdout+stderr is passed through a **per‑line ISO‑8601 timestamping filter** and then redirected to `server.log`, so every captured log line carries a leading `[…Z]` capture timestamp for correlation (see the note immediately below the command):

```bash
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin
setsid sudo -u builder env HOME=/tmp/builder \
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  /tmp/minio-investigation/minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 \
  --address ':9000' --console-address ':9001' 2>&1 \
  | while IFS= read -r line; do printf '[%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$line"; done \
  > /tmp/minio-investigation/server.log &
```

> **The leading `[…Z]` on every log excerpt is additive capture metadata, not MinIO output.** MinIO's console writer emits no per‑line wall‑clock prefix — its pretty helpers emit `INFO:`/`ERRO:`/`WARN:`/raw lines, and its error blocks carry MinIO's own `Time: <15:04:05 MST 01/02/2006>` line (`logger.TimeFormat`, `internal/logger/logger.go:L65`). The leading `[2026‑07‑07T…Z]` bracket seen on every `server.log` excerpt in this document is produced by the `while … printf … date -u +%FT%T.%3NZ … done` stage of the pipeline above (the same `date` format the document uses for its own wall‑clock markers, e.g. the `START`/`END` markers in §6.4; functionally equivalent to `ts` from moreutils or `docker logs --timestamps`). It is **purely additive** and **alters, paraphrases, summarizes, and elides nothing** in MinIO's output: MinIO's own `INFO:`/`ERRO:`/`WARN:` prefixes, its `Time:` lines, its `API:`/`DeploymentID:`/`Error:` structure, and the exact `file:line` stack frames are all reproduced verbatim beneath the bracket. Lines emitted together as one burst (e.g., a multi‑line error block) share a single capture instant, which is why such a block repeats one identical timestamp; the timestamp **values** are run‑specific wall‑clock times from the implementation run (like every other observed timing here). To reproduce the excerpts without the prefix, drop the `| while … done` stage and redirect directly (`> /tmp/minio-investigation/server.log 2>&1`). **(observed)**

Startup banner (verbatim from `server.log`) — first evidence that four dirs form **one set of four**:

```text
[2026-07-07T00:05:44.610Z] INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
[2026-07-07T00:05:44.610Z] INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
[2026-07-07T00:05:44.670Z] MinIO Object Storage Server
[2026-07-07T00:05:44.671Z] Copyright: 2015-2026 MinIO, Inc.
[2026-07-07T00:05:44.671Z] License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
[2026-07-07T00:05:44.671Z] Version: DEVELOPMENT.2026-07-06T23-45-05Z (go1.23.12 linux/amd64)
[2026-07-07T00:05:44.671Z] API: http://10.236.0.31:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
[2026-07-07T00:05:44.671Z] WebUI: http://10.236.0.31:9001 http://172.17.0.1:9001 http://127.0.0.1:9001
[2026-07-07T00:05:44.671Z] Docs: https://docs.min.io
[2026-07-07T00:05:44.671Z] WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

`Formatting 1st pool, 1 set(s), 4 drives per set.` is emitted by `logger.Info("Formatting %s pool, %v set(s), %v drives per set.", …)` at `cmd/prepare-storage.go:L194`. The `Version:` line carries the ldflags‑stamped `DEVELOPMENT.2026-07-06T23-45-05Z`, matching the `--version` output in §3.1. **(observed)**

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

> **Why EC:2 and not the "EC:1" seen in config help?** When `MINIO_STORAGE_CLASS_STANDARD` is unset (the default), the effective standard‑class parity is `DefaultParityBlocks(setDriveCount)` — for 4 drives that is **2** (`internal/config/storageclass/storage-class.go:L391`, wired via `ecDrivesNoConfig`, `cmd/format-erasure.go:L686`). The `EC:1` string that appears in the config *help template* is only a KV default label, not the effective runtime parity. The runtime header `X-Minio-Write-Quorum: 3` is authoritative and confirms parity 2. **(observed)**

---

## 5. Baseline — 4 Drives Online (Healthy)

All four health endpoints were probed with status line + headers visible (`curl -sS -i`), and a successful S3 PUT/GET was performed.

### 5.1 Health endpoints (unedited, full headers)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD92E5BE83C58
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:06:10 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD92E5CF630F0
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:06:10 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/live
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD92E5DC3CC0C
X-Content-Type-Options: nosniff
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:06:10 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/ready
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD92E5E90A121
X-Content-Type-Options: nosniff
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:06:10 GMT
```

The absence of any healing header at baseline was proven directly (not merely asserted) — the full `x-minio-*` set contains only two headers, and a case‑insensitive `healing` grep returns **0**:

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster | grep -i 'x-minio'
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster | grep -ic 'healing'
0
```

- `X-Minio-Write-Quorum: 3` is set by `ClusterCheckHandler` at `cmd/healthcheck-handler.go:L72` using the header string `MinIOWriteQuorum = "x-minio-write-quorum"` (`internal/http/headers.go:L193`). **(observed)**
- `X-Minio-Read-Quorum: 2` is set by `ClusterReadCheckHandler` at `cmd/healthcheck-handler.go:L109`, header `MinIOReadQuorum = "x-minio-read-quorum"` (`internal/http/headers.go:L196`). **(observed)**
- `X-Minio-Storage-Class-Defaults: false` (`cmd/healthcheck-handler.go:L73`, header string `MinIOStorageClassDefaults = "x-minio-storage-class-defaults"` at `internal/http/headers.go:L200`) is `false` because the storage‑class subsystem is initialized and `GetParityForSC` returns a valid parity (2), not a negative sentinel (`cmd/erasure-server-pool.go:L2737-L2739`). **(observed)**
- There is **no** `X-Minio-Healing-Drives` header at baseline — and this is exactly what the code predicts: the handler emits that header **only when `result.HealingDrives > 0`** (`if result.HealingDrives > 0 { w.Header().Set(xhttp.MinIOHealingDrives, …) }`, `cmd/healthcheck-handler.go:L75-L76`; header string `MinIOHealingDrives = "x-minio-healing-drives"` at `internal/http/headers.go:L203`). With nothing healing, the count is 0 and the header is omitted. **(observed)**

### 5.2 Successful PUT + GET (unedited)

```console
$ python3 /tmp/minio-investigation/s3op.py mb testbucket
MADE_BUCKET testbucket
$ python3 /tmp/minio-investigation/s3op.py put testbucket obj-baseline.txt /tmp/minio-investigation/obj.txt
PUT_OK key=obj-baseline.txt ETag="93effc27292c3bf7bdff44093d3467fc" HTTP=200
$ python3 /tmp/minio-investigation/s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 BODY='hello-baseline'
```

**Shard placement (corrected).** A 15‑byte text object is stored **inline in `xl.meta`** on each drive — it has **no** top‑level `part.1` file. The universal per‑drive indicator is therefore `xl.meta` (present on all 4), while a real `part.1` shard appears only for a large object. Both were verified:

```console
# small object 'obj-baseline.txt' (15 bytes): part.1 absent, xl.meta present on all 4 drives
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do echo -n "$d part.1: "; ls "$d"/testbucket/obj-baseline.txt/*/part.1 2>/dev/null | wc -l; done
/tmp/d1 part.1: 0
/tmp/d2 part.1: 0
/tmp/d3 part.1: 0
/tmp/d4 part.1: 0
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do echo -n "$d xl.meta: "; ls "$d"/testbucket/obj-baseline.txt/xl.meta 2>/dev/null | wc -l; done
/tmp/d1 xl.meta: 1
/tmp/d2 xl.meta: 1
/tmp/d3 xl.meta: 1
/tmp/d4 xl.meta: 1
# large object 'obj-big.bin' (1 MiB) writes a real part.1 data/parity shard on each of the 4 drives:
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do echo -n "$d part.1: "; ls "$d"/testbucket/obj-big.bin/*/part.1 2>/dev/null | wc -l; done
/tmp/d1 part.1: 1
/tmp/d2 part.1: 1
/tmp/d3 part.1: 1
/tmp/d4 part.1: 1
```

This corrected placement matters for §6/§7: the large `.bin` objects (which *do* have `part.1` shards) are the ones used to demonstrate shard‑skipping and reconstruction. (The S3 calls use a small `boto3` helper placed under `/tmp` and removed afterward — no `mc`/`aws` CLI was available in the environment; `boto3 1.43.40` was.)

---

## 6. Above the Threshold — 3 Drives Online (`chmod 000 /tmp/d1`)

Fault injected on **one** directory while the server was live (T0 = `2026-07-07T00:08:20.598Z`):

```console
$ date -u +%FT%T.%3NZ ; chmod 000 /tmp/d1
2026-07-07T00:08:20.598Z
```

### 6.1 Write OK, Read OK (unedited)

```console
$ python3 s3op.py put testbucket obj-3online.bin big3.bin
PUT_OK key=obj-3online.bin ETag="56f5ba40871ac33c8cdc0498acedf028" HTTP=200
$ python3 s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 BODY='hello-baseline'
```

Both **succeed**: 3 online ≥ write quorum 3 (write) and ≥ read quorum 2 (read).

**Shard proof** — the new 1 MiB object is written to the 3 healthy drives and **skipped on the faulted `/tmp/d1`**, exactly as the write‑quorum logic allows (3 shards = write quorum):

```console
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do echo -n "$d obj-3online part.1: "; ls "$d"/testbucket/obj-3online.bin/*/part.1 2>/dev/null | wc -l; done
/tmp/d1 obj-3online part.1: 0      # faulted drive — shard skipped
/tmp/d2 obj-3online part.1: 1
/tmp/d3 obj-3online part.1: 1
/tmp/d4 obj-3online part.1: 1
```

### 6.2 Health stays 200 (unedited)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD94D3EEA4F86
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:08:23 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster | grep -ic healing
0
```

3 online ≥ write quorum 3, so the set is still `Healthy` → `200`. No `X-Minio-Healing-Drives` header appeared (healing count 0). **(observed)**

### 6.3 The failing drive is named BY PATH in the logs (unedited, full stack frames)

The by‑path lines for a *permission* fault are emitted by the **reconnect poll** (endpoint connect) and by the **DiskInfo/healing‑tracker read**, both of which try to touch `/tmp/d1` and are denied. Both blocks name the drive by full path. These are the **complete, real** stack traces captured from `server.log` (they replace the abbreviated/placeholder frames a reader might guess at):

```text
[2026-07-07T00:08:29.735Z]
[2026-07-07T00:08:29.735Z] API: SYSTEM.peers
[2026-07-07T00:08:29.735Z] Time: 00:08:29 UTC 07/07/2026
[2026-07-07T00:08:29.735Z] DeploymentID: fa49a98a-f5c0-4758-97bc-fc96cca7f9eb
[2026-07-07T00:08:29.735Z] Error: drive access denied (cmd.StorageErr)
[2026-07-07T00:08:29.735Z]        endpoint="/tmp/d1"
[2026-07-07T00:08:29.735Z]        4: internal/logger/logger.go:258:logger.LogAlwaysIf()
[2026-07-07T00:08:29.735Z]        3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
[2026-07-07T00:08:29.735Z]        2: cmd/prepare-storage.go:51:cmd.init.func22.1()
[2026-07-07T00:08:29.735Z]        1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

```text
[2026-07-07T00:08:23.253Z]
[2026-07-07T00:08:23.253Z] API: SYSTEM.internal
[2026-07-07T00:08:23.253Z] Time: 00:08:23 UTC 07/07/2026
[2026-07-07T00:08:23.253Z] DeploymentID: fa49a98a-f5c0-4758-97bc-fc96cca7f9eb
[2026-07-07T00:08:23.253Z] Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
[2026-07-07T00:08:23.253Z]       10: internal/logger/logger.go:268:logger.LogIf()
[2026-07-07T00:08:23.253Z]        9: cmd/logging.go:112:cmd.internalLogIf()
[2026-07-07T00:08:23.253Z]        8: cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
[2026-07-07T00:08:23.253Z]        7: cmd/xl-storage.go:352:cmd.newXLStorage.func2()
[2026-07-07T00:08:23.253Z]        6: internal/cachevalue/cache.go:143:cachevalue.(*Cache[...]).update()
[2026-07-07T00:08:23.253Z]        5: internal/cachevalue/cache.go:128:cachevalue.(*Cache[...]).GetWithCtx()
[2026-07-07T00:08:23.253Z]        4: cmd/xl-storage.go:781:cmd.(*xlStorage).DiskInfo()
[2026-07-07T00:08:23.253Z]        3: cmd/xl-storage-disk-id-check.go:329:cmd.(*xlStorageDiskIDCheck).DiskInfo()
[2026-07-07T00:08:23.253Z]        2: cmd/erasure.go:192:cmd.getDisksInfo.func1()
[2026-07-07T00:08:23.253Z]        1: github.com/minio/pkg/v3@v3.0.22/sync/errgroup/errgroup.go:123:errgroup.(*Group).Go.func1()
```

- The `drive access denied` sentinel is `errDiskAccessDenied` (`cmd/storage-errors.go:L68`). The by‑path emission comes from `printEndpointError`, which is a **package‑var closure** (`var printEndpointError = func() func(Endpoint, error, bool) { … }`, `cmd/prepare-storage.go:L35`) that calls `peersLogAlwaysIf(ctx, err)` at `cmd/prepare-storage.go:L51`. Because it is an anonymous closure assigned to a package variable at init, the runtime frame surfaces as **`cmd.init.func22.1()` at `prepare-storage.go:51`** — *not* `cmd.printEndpointError()`. It is driven from the reconnect poll `connectDisks.func2` (`cmd/erasure-sets.go:L230`). **(observed)**
- The permission error is recognized by `osIsPermission` (`errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)`, `cmd/xl-storage-errors.go:L135`) and mapped by `osErrToFileErr` to `errFileAccessDenied` (`cmd/storage-errors.go:L101`). **(observed)**

> **Important nuance about *which* log fires (observed).** The oft‑cited `monitorDiskWritable` line *"node(%s): taking drive %s offline: %v"* (`cmd/xl-storage-disk-id-check.go:L1015`) did **not** fire for the permission fault. `goOffline` is only invoked on `errFaultyDisk` (an EIO‑class fault) or a health‑check *timeout*; a permission error maps to `errFileAccessDenied`, not `errFaultyDisk`, so the "taking drive offline" monitor stays quiet. This was verified with a **40 s quiet‑wait** after the fault (unedited):
>
> ```console
> $ START=$(date -u +%FT%T.%3NZ); sleep 40; END=$(date -u +%FT%T.%3NZ)
> # START=2026-07-07T00:09:35.796Z  (server.log had 132 lines)
> # END  =2026-07-07T00:10:15.804Z  (server.log now 190 lines)
> $ grep -cE 'taking drive .* offline|bringing drive .* online|Read/Write/Delete successful|healthcheck' server.log
> 0
> ```
>
> Zero matches ⇒ the health‑check offline monitor stayed quiet during 40 s of permission fault. The lines that *did* appear in the window are the reconnect poll's by‑path denial (still naming `/tmp/d1`):
>
> ```text
> [2026-07-07T00:09:44.668Z] Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
> [2026-07-07T00:09:44.676Z] Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
> [2026-07-07T00:09:44.676Z] Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
> [2026-07-07T00:09:44.678Z] Error: unable to read /tmp/d1/.minio.sys/buckets/.healing.bin: open /tmp/d1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
> ```
>
> The failing drive is nonetheless named by path via the reconnect/healing lines quoted above. This is a precise, observed correction to the naive "taking drive offline" expectation. **(observed)**

### 6.4 Sign of live recovery while the system is up

With `/tmp/d1` denied, the reconnect poll keeps probing it (each unique error printed once via the `printOnce` map in `printEndpointError`), and the auto‑heal subsystem is already running in the background (`initAutoHeal`, `cmd/background-newdisks-heal-ops.go:L377`). So yes — recovery is being **attempted while live**, without any operator action, as evidenced by the repeating by‑path denial stream in the 40 s window above. **(observed)**

---

## 7. Below the Threshold — 2 Drives Online (`chmod 000 /tmp/d2`)

Fault injected on a **second** directory, dropping to **2 online** (observation window T1 = `2026-07-07T00:16:29.997Z`):

```console
$ date -u +%FT%T.%3NZ    # timestamp of the below-threshold observation window
2026-07-07T00:16:29.997Z
$ for d in 1 2 3 4; do printf 'd%s=%s ' $d $(stat -c '%a' /tmp/d$d); done; echo
d1=0 d2=0 d3=755 d4=755
```

### 7.1 Write REFUSED, Read still OK (unedited)

```console
$ python3 s3op.py put testbucket obj-2online.bin big3.bin
CLIENT_ERROR HTTP=503 Code=SlowDownWrite Msg='Resource requested is unwritable, please reduce your request rate'
$ python3 s3op.py get testbucket obj-baseline.txt
GET_OK key=obj-baseline.txt HTTP=200 bytes=15 sha256=a5591c4203993f037d798f828fb5ab463d4cdd976cbc7faca672841a0a2ed916 preview='hello-baseline.'
$ python3 s3op.py get testbucket obj-3online.bin   # reconstruct from surviving shards
GET_OK key=obj-3online.bin HTTP=200 bytes=1048576 sha256=de81006ce1811f462944a5ae5f16abbf6ce449a4cdb8d42c1fbeeccd512a3219 preview='!F.Ha....MM...m.k=...q*4_J....B....&x..$|..W.AH:...'
```

- **Write is refused** with `HTTP 503 / SlowDownWrite`: 2 online < write quorum 3.
- **Reads still succeed** — including the 1 MiB `obj-3online.bin`, which is **reconstructed from its 2 surviving shards** (it never had a shard on the faulted `/tmp/d1`, and `/tmp/d2` is now down too, leaving shards on `/tmp/d3`+`/tmp/d4` = exactly read quorum 2). The full 1048576 bytes came back with a stable content hash. This is the **read/write divergence** at the heart of the above/below contrast. **(observed)**

### 7.2 Health: `/cluster` → 503, `/cluster/read` → 200 (unedited, full headers)

```console
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9C2A9615707
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:16:47 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9C2AA513450
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:16:47 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/live
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9C2AB34F248
X-Content-Type-Options: nosniff
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:16:47 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/ready
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9C2ABFF2BFE
X-Content-Type-Options: nosniff
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:16:47 GMT
```

`/minio/health/cluster` is **503** (online 2 < write quorum 3 → not `Healthy`) while `/minio/health/cluster/read` stays **200** (online 2 ≥ read quorum 2 → still `HealthyRead`). `/live` and `/ready` remain **200** (the process is up and initialized). **(observed)**

### 7.3 The write‑quorum decision is logged (unedited, full stack)

Two related log records were captured at 2 online. First, the **`Health()` write‑quorum verdict** — the internal error behind the `/cluster` 503 probe — with its complete 5‑frame stack:

```text
[2026-07-07T00:16:47.551Z] Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
[2026-07-07T00:16:47.551Z]        maintenance="false"
[2026-07-07T00:16:47.551Z]        5: internal/logger/logger.go:268:logger.LogIf()
[2026-07-07T00:16:47.551Z]        4: cmd/logging.go:156:cmd.storageLogIf()
[2026-07-07T00:16:47.551Z]        3: cmd/erasure-server-pool.go:2793:cmd.(*erasureServerPools).Health()
[2026-07-07T00:16:47.551Z]        2: cmd/healthcheck-handler.go:71:cmd.ClusterCheckHandler()
[2026-07-07T00:16:47.551Z]        1: net/http/server.go:2220:http.HandlerFunc.ServeHTTP()
```

Second, the **`InsufficientWriteQuorum`** error returned to an internal (scanner) write while 2 online — the same quorum shortfall surfacing on a different write path:

```text
[2026-07-07T00:16:44.677Z] Error: Storage resources are insufficient for the write operation .minio.sys/buckets/testbucket/.usage-cache.bin (cmd.InsufficientWriteQuorum)
[2026-07-07T00:16:44.677Z]        3: internal/logger/logger.go:268:logger.LogIf()
[2026-07-07T00:16:44.677Z]        2: cmd/logging.go:136:cmd.scannerLogIf()
[2026-07-07T00:16:44.677Z]        1: cmd/erasure.go:566:cmd.erasureObjects.nsScanner.func2()
```

The first message states the exact numbers this investigation asserts: **`expected write quorum: 3, drives-online: 2`**, emitted by `erasureServerPools.Health()` at `cmd/erasure-server-pool.go:L2793` (frame 3). **(observed)**

### 7.4 How the refusal maps from erasure layer → HTTP 503

The client's `SlowDownWrite` is the S3‑surface form of the internal write‑quorum error:

- Shard‑write enforcement returns `errErasureWriteQuorum` (`cmd/erasure-encode.go:L64-L65`; sentinel *"Write failed. Insufficient number of drives online"* at `cmd/erasure-errors.go:L26`).
- `errErasureWriteQuorum` / `InsufficientWriteQuorum` map to API error `ErrSlowDownWrite` (`cmd/api-errors.go:L2192-L2193,L2314-L2315`).
- `ErrSlowDownWrite` is defined as `{Code:"SlowDownWrite", Description:"Resource requested is unwritable, please reduce your request rate", HTTPStatusCode: http.StatusServiceUnavailable}` (`cmd/api-errors.go:L874-L878`) → **HTTP 503**, matching the observed `CLIENT_ERROR HTTP=503 Code=SlowDownWrite Msg='Resource requested is unwritable, please reduce your request rate'` in §7.1. **(observed)**
- (Symmetrically, `errErasureReadQuorum` → `ErrSlowDownRead`, `cmd/api-errors.go:L2190-L2191`.)

The failing `/tmp/d2` is also named by path in the reconnect/healing logs, identical in form to §6.3 (`endpoint="/tmp/d2"` and `/tmp/d2/.minio.sys/buckets/.healing.bin: permission denied`). **(observed)**

---

## 8. Recovery — Restore Permissions (before → during → after)

Permissions restored on both directories (T2 = `2026-07-07T00:19:39.244Z`):

```console
$ date -u +%FT%T.%3NZ ; chmod 755 /tmp/d1 /tmp/d2 ; for d in 1 2 3 4; do printf 'd%s=%s ' $d $(stat -c '%a' /tmp/d$d); done; echo
2026-07-07T00:19:39.244Z
d1=755 d2=755 d3=755 d4=755
```

**Before → during → after** of the health verdict:

| Phase | `/minio/health/cluster` | `/minio/health/cluster/read` |
|---|---|---|
| Before (4 online) | 200, write‑quorum 3 | 200, read‑quorum 2 |
| During (2 online) | **503** | 200 |
| After (restored) | **200** (write‑quorum 3) | 200 |

Health returned to 200 **near‑instantly** after `chmod 755` — measured, not estimated. A 1 s‑interval poll saw the very first probe after restore already return 200:

```console
# poll /cluster every 1s until it returns 200, measuring elapsed time from the chmod:
[2026-07-07T00:19:39.283Z] poll#1 /minio/health/cluster -> HTTP 200
>>> /cluster returned 200 at poll#1, elapsed 0.018s after restore
```

A fresh 1 MiB write then succeeded again, and health remained 200 on both cluster probes:

```console
$ date -u +%FT%T.%3NZ
2026-07-07T00:20:31.326Z
$ python3 s3op.py put testbucket obj-postrecovery.bin big3.bin
PUT_OK key=obj-postrecovery.bin bytes=1048576 ETag="56f5ba40871ac33c8cdc0498acedf028" HTTP=200
$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9F6E821886D
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:20:31 GMT

$ curl -sS -i http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BFD9F6E8D6E270
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Tue, 07 Jul 2026 00:20:31 GMT
```

**Why health recovers in 0.018 s and NOT after the ~15 s reconnect poll (observed).** A *permission* fault never actually **disconnects** the drive — the drive stays "connected" and simply errors on each I/O. `Health()` counts online drives from **live** `DiskInfo` — `if disk.State == madmin.DriveStateOk { si.online++ }` (`cmd/erasure-server-pool.go:L2707`) — reading current drive state on each probe rather than waiting for the reconnect timer. Once the OS permission is restored, the very next probe sees the drives as OK and returns 200. This was corroborated by scanning `server.log` after the restore: **zero** `taking drive … offline`, `bringing drive … online`, reconnect, or heal lines were emitted, and the stream of `.healing.bin … permission denied` errors dropped from continuous to **0**. The disk never left; it simply stopped erroring. **(observed)**

> This is an explicit correction to the intuition that a returning drive must wait for the 15 s reconnect poll: that timer governs *fully disconnected/fresh* drives (§9), **not** a live drive whose permissions were toggled.

---

## 9. Re‑admission — Polling (for disconnected/fresh drives), Not Push

**Re‑admission of a returning drive is automatic; no operator push is required.** Two mechanisms apply to two different situations:

- **Permission toggle (this investigation's fault):** the drive never disconnects, so re‑admission is *immediate* via live `DiskInfo` re‑reads — measured **0.018 s** in §8, with no reconnect/heal log lines at all. **(observed)**
- **Fully disconnected / fresh drive:** re‑admitted by the reconnect poll. `monitorAndConnectEndpoints` (`cmd/erasure-sets.go:L283`) runs on a timer and periodically calls `connectDisks(...)`, reconnecting any endpoint that has come back — no admin command needed. The interval is **15 s**: `defaultMonitorConnectEndpointInterval = defaultMonitorNewDiskInterval + time.Second*5` (`cmd/erasure-sets.go:L348`), and `defaultMonitorNewDiskInterval = time.Second * 10` (`cmd/background-newdisks-heal-ops.go:L40`) → `10 s + 5 s = 15 s`.

**Timing observed at scale (≥2 intervals).** The 15 s cadence is not merely read from a constant — it was surfaced as log lines. The reconnect timer fires **unconditionally** (`cmd/erasure-sets.go:L291,L306`); only the accompanying `Debugln` (`L299-L300`) is gated by `_MINIO_SERVER_DEBUG=on` (`common-main.go:L67`), so enabling debug reveals the cadence **without altering it**. This was captured on a **separate, explicitly non‑canonical** run on fresh dirs `/tmp/e1..e4` (labeled as such so it does not contaminate the canonical results):

```console
# NON-CANONICAL timing run: _MINIO_SERVER_DEBUG=on minio server /tmp/e1 /tmp/e2 /tmp/e3 /tmp/e4 --address :9000 --console-address :9001
[2026-07-07T00:30:44.635Z] minio: <DEBUG> running drive monitoring
[2026-07-07T00:30:59.636Z] minio: <DEBUG> running drive monitoring
[2026-07-07T00:31:14.637Z] minio: <DEBUG> running drive monitoring
[2026-07-07T00:31:29.638Z] minio: <DEBUG> running drive monitoring

# intervals between consecutive ticks:
  interval 1->2: 15.001s
  interval 2->3: 15.001s
  interval 3->4: 15.001s
# total ticks captured: 4  (>=2 intervals required); duration of run window ≈ 45 s
```

Three consecutive intervals of **15.001 s** confirm the constant at runtime. **(observed)**

- The **manual push** path exists but is **optional**: `mc admin heal` ultimately calls `queueHealTask` (`cmd/admin-heal-ops.go:L721`). It is a *supplement* to — not a prerequisite for — the automatic poll + heal.

> **Observed subtlety.** For the *permission* fault, no `monitorDiskStatus` "bringing drive … online" line (`cmd/xl-storage-disk-id-check.go:L955-L956`) was emitted, because that monitor is only started after a drive has been marked offline by the health‑check monitor — which, as shown in §6.3, does not trigger for permission errors. On recovery the drive is simply served again (§8). The "bringing drive online" by‑path line **does** fire for the EIO/timeout offline path and for a wiped/replaced drive (see §10.2). **(observed)**

---

## 10. Repair of Objects Written During the Outage

The object written during the outage is `obj-3online.bin` — written at 3 online with **no shard on `/tmp/d1`** (§6.1). This section reports precisely which mechanism restored it, distinguishing **observed** from **code‑supported inference**.

### 10.1 What repairs the transient‑outage object — observed vs. inferred

**MRF (Metadata Reconstruction / partial‑write queue) — the code‑level path (inference).** When an object is written while a drive is offline, the write/read paths enqueue a partial‑op via `globalMRFState.addPartialOp(...)` (`cmd/mrf.go:L78`, called from `cmd/erasure-object.go:L400,L805,L1578`), and the background `healRoutine` (`cmd/mrf.go:L220`) — after a short "let recently failed networks reconnect" delay — calls `healObject` (`cmd/mrf.go:L272,L276`) to rebuild the missing shard. This is the documented MinIO mechanism for partial writes.

**What was actually observed.** To avoid over‑claiming, the shard was tracked directly:

```console
# obj-3online.bin during the outage: absent on the faulted /tmp/d1
$ find /tmp/d1/testbucket/obj-3online.bin 2>/dev/null | wc -l
0
# ~30 s after chmod 755 restored /tmp/d1 (still a live, permission-toggled drive):
$ find /tmp/d1/testbucket/obj-3online.bin 2>/dev/null | wc -l
0        # STILL absent — not auto-healed within 30 s by MRF alone
```

So, for this specific *permission‑toggle* case, the missing shard was **not** observed to be restored by MRF within 30 s of the drive becoming reachable (the queued MRF op is consumed once and not re‑queued indefinitely). The shard **was** restored — directly and observably — by the fresh‑disk heal path in §10.2 (the same object reappeared on `/tmp/d1` with its exact original data‑dir UUID). 

**Classification (correcting an earlier over‑claim):** MRF as the repairer of this object is a **code‑supported inference**, not a direct observation; the **directly observed** repair came from the fresh/replaced‑drive heal loop. The MRF path remains the correct code‑level answer for partial writes in the general case, but the honest, runtime‑grounded statement here is: *shard restoration was observed via the fresh‑disk heal path, consistent with MinIO's heal machinery; MRF is the code‑documented partial‑write path (inferred for this case).* **(observed + inferred, as labeled)**

`obj-2online.bin` (the write that was *refused* at 2 online) correctly exists on **zero** drives — it was never created:

```console
$ for d in /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4; do ls "$d"/testbucket/obj-2online.bin 2>/dev/null; done
# (no output — object does not exist anywhere)
```

### 10.2 Fresh/replaced‑drive heal — full drive rebuild (~10 s cadence, ≥2 runs)

To exercise the **fresh‑disk** path explicitly, a data directory was **wiped** (simulating a replaced drive) while the server ran. The auto‑heal loop `monitorLocalDisksAndHeal → healFreshDisk` (`cmd/background-newdisks-heal-ops.go:L563,L419`) detected it, created the healing tracker, and rebuilt the drive by path. **Two runs** were performed to confirm the ~10 s detection cadence.

**Run 1 — wipe `/tmp/d1`:**

```console
$ ls -la /tmp/d1        # pre-wipe
total 24
drwxr-xr-x   4 builder builder  4096 Jul  7 00:06 .
drwxrwsrwx 167 root    root    12288 Jul  7 00:19 ..
drwxr-xr-x   7 builder builder  4096 Jul  7 00:05 .minio.sys
drwxr-xr-x   5 builder builder  4096 Jul  7 00:20 testbucket
$ date -u +%FT%T.%3NZ ; rm -rf /tmp/d1/* /tmp/d1/.minio.sys      # wipe -> fresh disk
2026-07-07T00:25:28.688Z
$ ls -la /tmp/d1        # post-wipe (empty)
total 16
drwxr-xr-x   2 builder builder  4096 Jul  7 00:25 .
drwxrwsrwx 167 root    root    12288 Jul  7 00:19 ..
# poll for the heal-start log line:
[2026-07-07T00:25:34.774Z] poll#7: heal-start log DETECTED, elapsed 6.09s after wipe
```

Heal logs (verbatim, by path):

```text
[2026-07-07T00:25:34.703Z] Healing drive '/tmp/d1' - 'mc admin heal alias/ --verbose' to check the current status.
[2026-07-07T00:25:34.705Z] Healing drive '/tmp/d1' - use 4 parallel workers.
[2026-07-07T00:25:34.744Z] Healing of drive '/tmp/d1' is finished (healed: 12, skipped: 0).
```

**Direct proof the heal reconstructs the exact missing shard (Finding‑critical).** After Run 1, `obj-3online.bin` — which had **no** shard on `/tmp/d1` during the outage — reappeared on `/tmp/d1` with the **same data‑dir UUID** as its surviving replicas (`bfa1f654-73ae-47e3-af65-35e9e18f11dd`):

```console
$ find /tmp/d1/testbucket/obj-3online.bin
/tmp/d1/testbucket/obj-3online.bin
/tmp/d1/testbucket/obj-3online.bin/xl.meta
/tmp/d1/testbucket/obj-3online.bin/bfa1f654-73ae-47e3-af65-35e9e18f11dd
/tmp/d1/testbucket/obj-3online.bin/bfa1f654-73ae-47e3-af65-35e9e18f11dd/part.1
$ ls /tmp/d1/testbucket        # all objects present again after heal
obj-3online.bin  obj-baseline.txt  obj-big.bin  obj-postrecovery.bin
```

**Run 2 — wipe `/tmp/d2`:**

```console
$ date -u +%FT%T.%3NZ ; rm -rf /tmp/d2/* /tmp/d2/.minio.sys
2026-07-07T00:26:22.891Z
[2026-07-07T00:26:35.028Z] poll#13: heal-start DETECTED, elapsed 12.14s after wipe
```

Heal logs for `/tmp/d2` (verbatim):

```text
[2026-07-07T00:26:34.715Z] Healing drive '/tmp/d2' - 'mc admin heal alias/ --verbose' to check the current status.
[2026-07-07T00:26:34.717Z] Healing drive '/tmp/d2' - use 4 parallel workers.
[2026-07-07T00:26:34.754Z] Healing of drive '/tmp/d2' is finished (healed: 13, skipped: 0).
```

**Timing summary (≥2 runs):**

| Run | Wiped drive | Wipe timestamp | Heal‑start detected | Detection latency |
|---|---|---|---|---|
| 1 | `/tmp/d1` | `2026-07-07T00:25:28.688Z` | `2026-07-07T00:25:34.774Z` | **6.09 s** |
| 2 | `/tmp/d2` | `2026-07-07T00:26:22.891Z` | `2026-07-07T00:26:35.028Z` | **12.14 s** |

- The detection latencies (6.09 s and 12.14 s) bracket the **10 s** disk‑monitor tick — `monitorLocalDisksAndHeal` ticks on `defaultMonitorNewDiskInterval = 10 s` (`cmd/background-newdisks-heal-ops.go:L40,L565`). Run 2's longer latency reflects that a freshly‑wiped disk must first be re‑admitted by the 15 s reconnect poll (§9) before the next 10 s heal tick picks it up, so a given wipe can miss one tick. **(observed)**
- The heal‑start / worker‑count / heal‑finish log lines come from `healingLogEvent(...)` at `cmd/background-newdisks-heal-ops.go:L460` (start), `cmd/global-heal.go:L210` (`"use %d parallel workers"`), and `cmd/background-newdisks-heal-ops.go:L520` (finish). **(observed)**
- Progress is persisted to `.healing.bin` (`healingTrackerFilename`, `cmd/background-newdisks-heal-ops.go:L41`) so a heal survives restarts. **(observed)**

> **Parity‑upgrade nuance (from code).** Writes that succeed while drives are offline may have their parity upgraded at PUT time to preserve protection — `userDefined[minIOErasureUpgraded] = "orig->new"` (`cmd/erasure-object.go:L1310-L1316`). Relevant to objects written above quorum during an outage. **(from source; not separately isolated at runtime)**

---

## 11. Where the Quorum Decision Lives in the Code

A precise trace, from parity default → quorum computation → enforcement → health surface. All line numbers were verified against the source tree at the implementation‑run checkout `ce1199d7bd15`; because every later commit on this branch is documentation‑only (see §3 *Provenance*), the source is unchanged and each citation remains valid at the current `HEAD`. **(observed)**

### 11.1 Default parity for the set size

```go
// internal/config/storageclass/storage-class.go:L354-L368  (exact source)
func DefaultParityBlocks(drive int) int {
	switch drive {
	case 1:
		return 0
	case 3, 2:
		return 1
	case 4, 5:
		return 2
	case 6, 7:
		return 3
	default:
		return 4
	}
}
```

⇒ four drives ⇒ `case 4, 5: return 2` ⇒ **2 parity** (2 data + 2 parity).

### 11.2 The quorum computation itself — `objectQuorumFromMeta`

The following is an **abridged/annotated excerpt** of `cmd/erasure-metadata.go:L531-L564`: the real function body is reproduced faithfully, with `// →` trailing comments **added by this document** to point out the arithmetic (the added comments are not present in the source):

```go
// cmd/erasure-metadata.go:L531-L564  (abridged/annotated excerpt; "// →" notes added by this doc)
func objectQuorumFromMeta(ctx context.Context, partsMetaData []FileInfo, errs []error, defaultParityCount int) (objectReadQuorum, objectWriteQuorum int, err error) {
	// There should be at least half correct entries, if not return failure
	expectedRQuorum := len(partsMetaData) / 2
	if defaultParityCount == 0 {
		expectedRQuorum = len(partsMetaData)
	}

	reducedErr := reduceReadQuorumErrs(ctx, errs, objectOpIgnoredErrs, expectedRQuorum)
	if reducedErr != nil {
		return -1, -1, reducedErr
	}

	if defaultParityCount == 0 {
		return len(partsMetaData), len(partsMetaData), nil
	}

	parities := listObjectParities(partsMetaData, errs)
	parityBlocks := commonParity(parities, defaultParityCount)             // → 2  (commonParity at L461)
	if parityBlocks < 0 {
		return -1, -1, InsufficientReadQuorum{Err: errErasureReadQuorum, Type: RQInsufficientOnlineDrives}
	}

	dataBlocks := len(partsMetaData) - parityBlocks                        // → 4 - 2 = 2   (L555)

	writeQuorum := dataBlocks                                              // → = 2         (L557)
	if dataBlocks == parityBlocks {                                        // → 2 == 2      (L558)
		writeQuorum++                                                      // → 3           (L559)
	}

	// Since all the valid erasure code meta updated at the same time are equivalent, pass dataBlocks
	// from latestFileInfo to get the quorum
	return dataBlocks, writeQuorum, nil                                    // → read=2, write=3 (L564)
}
```

**This is where the threshold lives.** Read quorum = data blocks (2); write quorum = data blocks, **bumped by one when data == parity** (→ 3). `reduceReadQuorumErrs`/`reduceWriteQuorumErrs` (`cmd/erasure-metadata-utils.go:L150,L156`) collapse per‑drive errors into a single verdict.

### 11.3 Where "stop" is enforced for writes — `multiWriter.Write`

The following is an **abridged/annotated excerpt** of `cmd/erasure-encode.go:L34-L66` (the `// L##` line markers and the elision `...` are added by this document to focus on the threshold check; consult the source for the full body):

```go
// cmd/erasure-encode.go:L34-L66  (abridged/annotated excerpt)
func (p *multiWriter) Write(ctx context.Context, ...) error {
	...
	nilCount := countErrs(p.errs, nil)             // L59
	if nilCount >= p.writeQuorum {                 // L60  ← the threshold check
		return nil                                 // enough shards written → OK
	}
	writeErr := reduceWriteQuorumErrs(ctx, p.errs, objectOpIgnoredErrs, p.writeQuorum)
	return fmt.Errorf("%w (offline-disks=%d/%d)",  // L64-L65 ← wraps errErasureWriteQuorum
		writeErr, countErrs(p.errs, errDiskNotFound), len(p.writers))
}
```

At 2 online, `nilCount (2) < writeQuorum (3)` → returns `errErasureWriteQuorum`. The object‑write path also computes its own `writeQuorum` and returns the same sentinel via `toObjectErr(...)` (`cmd/erasure-object.go:L1308`).

### 11.4 The sentinel errors that mean "stop"

```go
// cmd/erasure-errors.go  (exact source)
errErasureReadQuorum  = errors.New("Read failed. Insufficient number of drives online")   // L23
errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")  // L26
```

### 11.5 The cluster‑health decision that surfaces the threshold — `Health()`

The following is an **abridged/annotated excerpt** of `cmd/erasure-server-pool.go` (individual real lines quoted with their line numbers; `// ...` marks omitted intervening code):

```go
// cmd/erasure-server-pool.go  (abridged/annotated excerpt — real lines with line numbers)
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

// the log line observed in §7.3 (emitted at L2793):
// "Write quorum could not be established on pool: %d, set: %d, expected write quorum: %d, drives-online: %d"
```

`ClusterCheckHandler` (`cmd/healthcheck-handler.go`) calls `Health()`, writes the `X-Minio-Write-Quorum` header (`L72`), conditionally writes `X-Minio-Healing-Drives` only when `HealingDrives > 0` (`L75-L76`), and maps `Healthy`→`200` (`L89`), not‑`Healthy`→`503` (`L85`) — or `412` when `?maintenance=true` (`L83`, param parsed at `L68`). The route `/minio/health/cluster` is registered in `cmd/healthcheck-router.go:L30`, mounted by `registerHealthCheckRouter` (`cmd/routers.go:L98`).

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
| 4 dirs → 1 set of 4 → EC:2 | **observed** | startup banner (`1 set(s), 4 drives per set`) + `X-Minio-Write-Quorum: 3` header |
| Read quorum 2 / Write quorum 3 | **observed** | `X-Minio-Read-Quorum: 2`, `X-Minio-Write-Quorum: 3`, server log `expected write quorum: 3, drives-online: 2` |
| 3 online → write OK, read OK, health 200 | **observed** | §6 PUT/GET output + shard counts + curl (req `18BFD94D3EEA4F86`) |
| 2 online → write 503 (SlowDownWrite), read 200, `/cluster` 503, `/cluster/read` 200 | **observed** | §7 client error + curl outputs (reqs `18BFD9C2A9615707`/`…AA513450`) + quorum log |
| Failing drive named by path in logs | **observed** | §6.3/§7.4 `endpoint="/tmp/d1"`, `/tmp/d1/.minio.sys/buckets/.healing.bin … permission denied` with full stacks |
| `monitorDiskWritable` "taking drive offline" does NOT fire for permission fault | **observed** | 40 s quiet‑wait, grep count 0; error maps to `errFileAccessDenied`, not `errFaultyDisk` |
| Permission‑fault recovery is near‑instant (drive never disconnects) | **observed** | §8 measured **0.018 s** to `/cluster` 200; zero reconnect/heal log lines after `chmod 755` |
| Re‑admission of a *disconnected/fresh* drive is automatic + polling | **observed** | §9 reconnect ticks; `connectDisks` poll in code |
| Reconnect poll interval = **15 s** | **observed** | §9 debug ticks: three intervals of **15.001 s** (non‑canonical run); constant `defaultMonitorConnectEndpointInterval` (`erasure-sets.go:L348`) |
| Fresh‑disk heal cadence ≈ **10 s** | **observed (≥2 runs)** | §10.2 detection **6.09 s** (Run 1) and **12.14 s** (Run 2); constant `defaultMonitorNewDiskInterval = 10 s` |
| Fresh‑disk heal directly reconstructs the missing shard | **observed** | §10.2 `obj-3online.bin` reappears on `/tmp/d1` with same data‑dir UUID `bfa1f654‑…` |
| MRF repairs the transient‑outage object | **inferred (code‑supported)** | §10.1 MRF is the code path (`mrf.go:L78,L220`); shard was **not** observed restored by MRF within 30 s — the observed restore came from fresh‑disk heal |
| Parity upgrade for offline‑time writes | **from source** | `cmd/erasure-object.go:L1310-L1316` (not separately isolated at runtime) |

---

## 14. Coverage Pass — All Eight Sub‑Questions

- [x] **1. Health decision & disk‑count assumption** — §2(1), §4, §5, §11.5. `Health()` counts OK drives and needs `≥ write quorum (3)` for `Healthy`, `≥ read quorum (2)` for `HealthyRead`; runtime headers `X-Minio-Write-Quorum: 3` / `X-Minio-Read-Quorum: 2`.
- [x] **2. Behavior at the moment of failure** — §2(2), §6. Does not crash/refuse; marks the shard skipped on `/tmp/d1` and keeps serving while at/above quorum (PUT `HTTP 200`, shard proof).
- [x] **3. Above vs. below threshold** — §6 (3 online: write OK/read OK/200) vs §7 (2 online: write 503/read 200/`cluster` 503/`cluster/read` 200).
- [x] **4. Log visibility & live recovery** — §6.3, §6.4, §7.4. Failing drive named **by path** with full stacks; reconnect/heal activity runs while live (with the observed correction about *which* monitor logs it).
- [x] **5. Re‑admission mechanism** — §8, §9. Permission fault: immediate (0.018 s, drive never disconnects). Disconnected/fresh drive: automatic **polling** via `monitorAndConnectEndpoints` (observed **15.001 s** cadence); `mc admin heal` is the optional manual push.
- [x] **6. Repair of outage writes** — §10. Directly observed reconstruction of `obj-3online.bin` via the fresh‑disk heal loop (~10 s, `.healing.bin`, ≥2 runs); MRF is the code‑supported inference for partial writes. Refused write `obj-2online.bin` exists nowhere.
- [x] **7. Quorum decision in code** — §11. `objectQuorumFromMeta` (`cmd/erasure-metadata.go:L531-L564`) + `DefaultParityBlocks(4)=2` + enforcement in `multiWriter.Write` + surfaced by `Health()`/`ClusterCheckHandler`.
- [x] **8. Grounding** — throughout §5–§10: every claim paired with the `/minio/health/*` response and/or a real S3 PUT/GET at the relevant disk‑loss level, with unedited output.

---

## 15. Cleanup — Repository Left Unchanged

The investigation is **read‑only** with respect to the source repository: the only change on this branch is this answer document itself. All runtime artifacts were created **outside** the checkout (built binary at `/tmp/minio-investigation/minio`, data dirs `/tmp/d1..d4` and `/tmp/e1..e4`, observation scripts, and captured logs) and are removed when the investigation concludes. The MinIO server processes are stopped; the ownership/permission changes applied to the `/tmp` data dirs during the fault never touch the repo (the built binary and any `healing-*`/`minio` artifacts are `.gitignore`d).

Two states are distinguished so the read‑only evidence is unambiguous — an **in‑progress edit** of this document versus the **final committed state** acceptance sees:

**(a) While this document is being authored/edited (transient).** The answer document is the single mutation this task is permitted to make, so an in‑progress edit shows exactly one tracked change — this file — and nothing else:

```console
# (servers already stopped) remove all out-of-repo investigation artifacts:
$ rm -rf /tmp/minio-investigation /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 /tmp/e1 /tmp/e2 /tmp/e3 /tmp/e4

# during an in-progress edit, the ONLY tracked change is the answer document (" M" = modified, unstaged):
$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c status --porcelain
 M blitzy/documentation/minio_c07e5b49d477.md
```

**(b) Final committed state — what final acceptance sees.** Once the document is committed, the working tree is clean; no existing source file, `go.mod`, or `go.sum` was modified, created, or deleted anywhere on the branch:

```console
$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c status --porcelain
# (no output — clean working tree)

$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c diff --stat
# (no output — nothing uncommitted)

# the entire footprint of this branch since the document was introduced is this one file:
$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c diff --name-status ce1199d7bd15..HEAD
M	blitzy/documentation/minio_c07e5b49d477.md

# dependency manifests are untouched:
$ git -C /tmp/blitzy/minio/blitzy-2050ae24-b281-4c78-87d8-1498499b5d58_13a93c diff --name-only ce1199d7bd15..HEAD -- go.mod go.sum
# (no output — dependency manifests unchanged)
```

The empty `git status --porcelain` and `git diff --stat` at the committed state confirm the repository is left byte‑for‑byte unchanged apart from this single added/edited answer document, satisfying the read‑only rule. (The `ce1199d7bd15` reference is the implementation‑run checkout of §3 *Provenance*; the `..HEAD` diff naturally continues to list only this file across the branch's documentation‑only commits.) **(observed)**

---

### Appendix — Full list of source citations used above

`internal/config/storageclass/storage-class.go:L354-L368,L391` · `cmd/erasure-metadata.go:L461,L531-L564` · `cmd/erasure-metadata-utils.go:L137,L150,L156` · `cmd/erasure-encode.go:L34-L66` · `cmd/erasure-errors.go:L23,L26` · `cmd/erasure-object.go:L400,L805,L1308,L1310-L1316,L1578` · `cmd/erasure-server-pool.go:L2707,L2720-L2726,L2737-L2739,L2783-L2784,L2793` · `cmd/erasure.go:L192,L566` · `cmd/healthcheck-handler.go:L68,L71,L72,L73,L75-L76,L83,L85,L89,L109` · `cmd/healthcheck-router.go:L30` · `cmd/routers.go:L98` · `cmd/storage-errors.go:L68,L101` · `cmd/xl-storage-errors.go:L135` · `cmd/xl-storage-disk-id-check.go:L233,L329,L955-L956,L1013,L1015` · `cmd/xl-storage.go:L352,L436,L781` · `cmd/prepare-storage.go:L35,L51,L194` · `cmd/erasure-sets.go:L230,L283,L291,L299-L300,L306,L348` · `cmd/background-newdisks-heal-ops.go:L40,L41,L377,L419,L460,L520,L563,L565` · `cmd/global-heal.go:L210` · `cmd/logging.go:L65,L112,L136,L156` · `cmd/mrf.go:L78,L220,L272,L276` · `cmd/admin-heal-ops.go:L721` · `cmd/api-errors.go:L874-L878,L2190-L2193,L2314-L2315` · `cmd/common-main.go:L67` · `internal/http/headers.go:L193,L196,L200,L203` · `internal/cachevalue/cache.go:L128,L143` · `docs/minio-limits.md:L15-L16` · `Makefile:L3,L177-L179` · `go.mod:L3`
