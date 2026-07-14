# MinIO Fault-Tolerance Behavior in a Four-Directory Erasure-Coded Deployment

> **Grounded in a built-and-run MinIO.** The investigated source is the MinIO tree at commit
> `c07e5b49d477b0774f23db3b290745aef8c01bd2` (the parent of branch `minio_c07e5b49d477`). The server was
> compiled and executed as an unprivileged user over four local directories; the default storage class
> yields **EC:2** (2 data + 2 parity). Every **behavioral** claim below is shown next to the **actual,
> unedited output** that produced it — the command that produced it (or, where a step aggregates several
> commands, a clearly-labeled descriptive capture of that step), its output (shown in full, or as a
> clearly-labeled representative excerpt or derived count when the raw output is long or repetitive), and its
> exit status where applicable; every **code** claim carries an exact `file:line` citation verified against
> this source. Statements that could not be surfaced at runtime are explicitly labeled **(inferred, from
> code)**.

> **Provenance note on the version banner (read this before §6.0).** The server was compiled from the
> **investigated MinIO source** `c07e5b49d477b0774f23db3b290745aef8c01bd2` with the canonical build flags
> (`CGO_ENABLED=0 go build -tags kqueue -trimpath`; see §3 and §6.0). Its version banner is stamped from that
> commit's own git metadata — `buildscripts/gen-ldflags.go` derives the version from the built commit's
> committer date (`2024-11-25T17:10:22Z`) and its hash — so `./minio --version` reports
> `DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477…)`, exactly as captured in §6.0. This branch adds
> on top of `c07e5b49d477…` **only** this one Markdown document and **no** MinIO source file, so the Go source
> that was compiled and run is byte-for-byte identical to `c07e5b49d477…`; every `file:line` citation therefore
> resolves against that source, and §6.0 shows the `git diff` that proves the byte-identity.

## 1. Title & Scope

This document answers, from a **compiled, running** MinIO server (not from code reading alone), how MinIO
behaves when data directories become inaccessible in a four-directory erasure-coded deployment. It covers
eight questions: (Q1) how MinIO decides it is "healthy" and what it assumes about the minimum number of
disks; (Q2) what happens the instant one directory becomes inaccessible mid-operation; (Q3) the explicit
difference between staying **above** the quorum threshold (one disk down) and dropping **below** it (a
second disk down); (Q4) whether the logs name the failing disk by its filesystem path and whether recovery
is attempted while the server stays live; (Q5) whether MinIO self-detects a restored directory via polling
or needs an external push; (Q6) how objects written while a disk was down get repaired once it returns;
(Q7) exactly where in the source the quorum decision lives and how the threshold is computed; and (Q8) the
grounding of every conclusion in the health-endpoint response and a real write/read result.

The failure is injected **externally** with an operating-system permission change (`chmod 000`) on a data
directory and reversed with `chmod 755`; MinIO itself is never modified. The topology is one erasure set of
four drives at default parity **EC:2**, which yields a **write quorum of 3** and a **read quorum of 2** — the
two thresholds that govern every behavior described here. All conclusions are scoped to this single-node,
four-directory, default-parity topology.

## 2. TL;DR — Answers at a Glance

| # | Question | One-line answer | Decisive observed evidence | Primary `file:line` |
|---|----------|-----------------|----------------------------|---------------------|
| **Q1** | Health decision & disk assumptions | The deployment is "healthy" only if **every** erasure set has `online ≥ write quorum`; for 4-dir EC:2 the write quorum is **3** (read quorum 2). `Health()` derives the thresholds itself from `BackendInfo()`. | `GET /minio/health/cluster` → **200** with `X-Minio-Write-Quorum: 3` | `Health()` [cmd/erasure-server-pool.go:L2679]; quorum calc [L2719-L2727] |
| **Q2** | Live permission-loss behavior | It **keeps serving writes** while still at/above write quorum, and **refuses writes** the moment it drops below it (reads continue while read quorum holds). | 3 online → PUT exit 0; 2 online → PUT exit 1 `SlowDownWrite` | PUT quorum check [cmd/erasure-object.go:L1304-L1308] |
| **Q3** | Above vs below threshold | **Above** (3 online): `/cluster` 200, writes succeed. **Below** (2 online): `/cluster` 503, writes refused, **reads still succeed**. | §6.2 vs §6.3 + `FatalKind` log | `Health()` [cmd/erasure-server-pool.go:L2791,L2794-L2795] |
| **Q4** | Path-named logs & live recovery | **Yes**, the disk is named by its exact path. For a *permission* fault the disk is excluded through the **DiskInfo/Healing** path; the dedicated `monitorDiskWritable` offline/online lines do **not** fire (observed count = 0). | `endpoint="/tmp/ec/data1"`; `.healing.bin … permission denied`; offline/online counts = **0** | runtime perm map [cmd/xl-storage.go:L802-L826]; offline log [cmd/xl-storage-disk-id-check.go:L1015] |
| **Q5** | Self-detection of restored dir | **Automatic**, reflected on the **next health probe** (observed **~11–15 ms** in a tight poll) — **no restart, no external push**. Driven by the on-demand DiskInfo re-read (1 s cache), not the 5/10/15 s pollers. | poll shows `/cluster` 200 + 4 online on the first 5 s poll, stable through t+45s; same server PID | DiskInfo cache [cmd/xl-storage.go:L326] |
| **Q6** | Repair of objects written during outage | **Timing-dependent.** If the drive returns within the ~1 s MRF retry window the missing shard is rebuilt **automatically** (short outage); if it returns later the single retry is spent and the shard stays missing until a heal — an on-demand `mc admin heal` or the slower scanner (long outage). Readable throughout. | short: shard auto-restored ~t+1–2 s (3/3); long: still missing at t+45 s (3/3) → `mc admin heal` `Yellow→Green` | MRF `healRoutine()` [cmd/mrf.go:L220-L254]; `healFreshDisk()` [cmd/background-newdisks-heal-ops.go:L419] |
| **Q7** | Location of the quorum decision | Cluster health computes quorum in `Health()` via `BackendInfo()`; the **object write path** uses `defaultWQuorum()`/per-object `FileInfo.WriteQuorum()`. Both apply a **+1 split-brain guard** when `data == parity`. | `X-Minio-Write-Quorum: 3`; `expected write quorum: 3` log | quorum calc [cmd/erasure-server-pool.go:L2722-L2727]; `defaultWQuorum()` [cmd/erasure.go:L85] |
| **Q8** | Grounding | Every behavioral conclusion is paired with a health-endpoint status/headers **and** a real S3 write/read result from the running server. | all §6 evidence blocks | `ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] |

## 3. Methodology

The answer was derived from a **compiled, running** server, then written from the captured output. The
steps below reproduce it end-to-end and are the exact steps used.

> **Critical, non-obvious insight — run MinIO as a NON-ROOT user.** Under `root`, `chmod 000` is bypassed
> (root holds the `DAC_OVERRIDE` capability), so the permission fault is invisible and the scenario cannot
> be reproduced. This investigation ran the server as the unprivileged user `tester` (uid 1001) with the
> data directories owned by that user. The non-root identity and directory ownership are proven with raw
> `id`/`ps`/`stat` output in §6.0.

**Isolation and default-configuration choices (why these are canonical):**

- **Default configuration only.** No `MINIO_CI_CD` (or any other behavior-altering) override is set. The
  only environment set is the root credential pair; the storage class is left at its default, giving EC:2
  for four drives.
- **Loopback bind.** The API and Console are bound to `127.0.0.1` (`--address 127.0.0.1:9000`,
  `--console-address 127.0.0.1:9001`) so nothing is exposed off-host.
- **Ephemeral credentials.** `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` are random per-run values (not the
  well-known `minioadmin/minioadmin`), generated at launch and discarded at cleanup.
- **Isolated client config.** `mc` is pointed at an isolated `--config-dir` (a per-run scratch directory
  under the tester-owned working area) that is removed during cleanup, so no shared client state is touched.
- **Non-orchestrated run.** The Kubernetes service-env variables that the shell inherits are unset for the
  server process, so MinIO runs in the ordinary (non-orchestrated) mode a normal local user gets.

**Observation tooling / provenance.** The `mc` client used is the environment's pre-provisioned binary,
identified exactly as **`RELEASE.2025-08-13T08-35-41Z`** (commit-id `7394ce0dd2a80935aded936b09fa12cbb3cb8096`,
runtime `go1.24.6`), `sha256 = 01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891`. It was
not downloaded by this investigation; it is a verified, pre-provisioned tool. `curl` is the system-provided
binary. Neither alters the repository. On-disk `xl.meta` parity was decoded with the repository's own
`docs/debugging/xl-meta` tool (built by `make build`).

1. **Build.** Install Go matching the module directive `go 1.23` [go.mod:L3] (used `go1.23.12`) and build via
   the canonical target:

   ```sh
   make build
   # runs: CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio   [Makefile:L177-L179]
   ```

   Capture the version banner with `./minio --version` (see §6.0). The real entry point is
   `main.go` → `minio.Main(os.Args)` [main.go:L30], importing `github.com/minio/minio/cmd` [main.go:L26].

2. **Launch (as non-root, default config).** Run the real server entry point over four local directories
   (the launch idiom documented at [docs/erasure/README.md:L44] as `minio server /data{1...12}`; here with
   four directories):

   ```sh
   # data dirs owned by tester (uid 1001); K8s service env unset for a non-orchestrated run
   MINIO_ROOT_USER="$EPH_USER" MINIO_ROOT_PASSWORD="$EPH_PASS" \
     ./minio server /tmp/ec/data{1...4} --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
   ```

   Confirm the banner reports `1 set(s), 4 drives per set` (single EC:2 erasure set). Point `mc` at the
   server with an isolated config dir and an alias.

3. **Probe at every boundary.** At each state, capture verbatim:
   - `curl -sI …/minio/health/cluster` (a **HEAD** request) **and** `curl -sS -D- -o /dev/null -X GET
     …/minio/health/cluster` and `.../cluster/read` (real **GET** requests) — status code + `X-Minio-*`
     headers;
   - a real S3 **PUT**/**GET** via `mc cp` / `mc cat` (full output + exit status);
   - the drive online/offline count via `mc admin info`.

4. **Inject faults (external only).** `chmod 000 /tmp/ec/data1` (one down → still above threshold), then
   `chmod 000 /tmp/ec/data2` (two down → below threshold). Restore with `chmod 755` on both **without
   restarting** the server (the unchanged server PID is verified before and after).

5. **Observe long enough / confirm stability.** Each state was exercised **at least twice**; the recovery
   latency was measured across **three** restore trials. The qualitative signals (the `200`→`503`
   transition, the `SlowDownWrite` write refusal, the path-named disk logs, the automatic reconnect) were
   **stable across runs** (§6). Run-dependent *counts* (e.g., how many polling cycles logged during an
   outage window) vary because they depend on elapsed polls; those counts are reported as observed and are
   not load-bearing for any conclusion.

6. **Cleanup.** Stop the server by its **specific PID** (never a broad process-kill pattern), remove
   `/tmp/ec`, the isolated `mc` config, the `./minio` binary, the `docs/debugging` helper binaries, the
   `mc` copy, and all scripts/logs. Verify `git status --porcelain` is empty except the added document
   (§6.7).

## 4. Quorum Math for This Topology (4-dir EC:2)

For four directories, the default `STANDARD` storage-class parity is **EC:2**
([internal/config/storageclass/storage-class.go:L355], `case 4, 5: return 2` at
[internal/config/storageclass/storage-class.go:L361-L362]; documented as "5 or fewer ⇒ EC:2" at
[docs/erasure/storage-class/README.md]). The set therefore has `setDriveCount = 4` and
`defaultParityCount = 2`. Two independent code sites compute the read/write quorums from those two numbers
and reach the **same** values:

| Quantity | Formula | Value | Object-path source | Cluster-health source |
|----------|---------|-------|--------------------|------------------------|
| Data blocks | `setDriveCount − parity` = 4 − 2 | **2** | [cmd/erasure.go:L86] | `StandardSCData` [cmd/erasure-server-pool.go:L694] |
| Parity blocks | `DefaultParityBlocks(4)` | **2** | [internal/config/storageclass/storage-class.go:L361-L362] | `StandardSCParity` [cmd/erasure-server-pool.go:L700] |
| **Read quorum** | data blocks | **2** | `defaultRQuorum()` [cmd/erasure.go:L94-L96] | `poolReadQuorums[i] = data` [cmd/erasure-server-pool.go:L2723] |
| **Write quorum** | data `(+1 when data == parity)` | **3** | `defaultWQuorum()` [cmd/erasure.go:L85-L91] | `if data == parity { data + 1 }` [cmd/erasure-server-pool.go:L2725-L2726] |

The **+1 split-brain guard** is the key subtlety. On the object path, `defaultWQuorum()` computes
`dataCount = 2`, and because `dataCount == defaultParityCount` (2 == 2) it returns `dataCount + 1 = 3`
[cmd/erasure.go:L85-L91]; the per-object `FileInfo.WriteQuorum()` mirrors this, starting from
`fi.Erasure.DataBlocks` and incrementing when `DataBlocks == ParityBlocks`
[cmd/storage-datatypes.go:L298-L308]. The cluster-health path applies the identical `data + 1` rule inside
`Health()` [cmd/erasure-server-pool.go:L2722-L2727]. This guarantees a write is acknowledged only when a
**strict majority** of the four drives (3 of 4) persisted it, so two disjoint halves can never both believe
they hold the authoritative copy. The read quorum has no +1 and equals the data-block count, **2**
[cmd/erasure.go:L94-L96], [cmd/storage-datatypes.go:L310-L316].

```go
// cmd/erasure.go:L85-L96 (object-path formula; verified against the investigated source)
func (er erasureObjects) defaultWQuorum() int {
	dataCount := er.setDriveCount - er.defaultParityCount
	if dataCount == er.defaultParityCount {
		return dataCount + 1
	}
	return dataCount
}

// defaultRQuorum read quorum based on setDriveCount and defaultParityCount
func (er erasureObjects) defaultRQuorum() int {
	return er.setDriveCount - er.defaultParityCount
}
```

**Distinguishing the three "tolerances" (they are not the same number):**

- **Write availability** is governed by the **write quorum (3)**. Losing a *second* drive (2 online) drops
  below it, so writes are refused.
- **Read availability** is governed by the **read quorum (2)**. Two online still meets it, so reads persist.
- **Redundancy / durability** is governed by **parity (2)**. Each offline or missing shard *reduces*
  redundancy; an object stays reconstructable while at least `data` (2) shards survive, but it is not at
  *full* redundancy again until it is healed.

**Consequence for this topology:** with four drives online the deployment is healthy; losing one drive (3
online) still meets the write quorum of 3, so writes continue (at reduced redundancy); losing a second (2
online) is **below** write quorum 3 but still **at** read quorum 2 — hence writes are refused while reads
persist.

## 5. Endpoint Contract

Health is exposed under the `/minio/health` prefix [cmd/healthcheck-router.go:L32]. The two probes relevant
here are registered for both `GET` and `HEAD`:

- `healthCheckClusterPath = "/cluster"` [cmd/healthcheck-router.go:L30]
- `healthCheckClusterReadPath = "/cluster/read"` [cmd/healthcheck-router.go:L31]
- registered for `GET` and `HEAD` [cmd/healthcheck-router.go:L41-L44]

`ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] first runs `checkHealth()` [cmd/healthcheck-handler.go:L32],
then calls `objLayer.Health(...)`, then:

- sets `X-Minio-Write-Quorum` from `result.WriteQuorum` [cmd/healthcheck-handler.go:L72];
- sets `X-Minio-Storage-Class-Defaults` from `result.UsingDefaults` [cmd/healthcheck-handler.go:L73];
- if any drives are healing, sets `X-Minio-Healing-Drives` [cmd/healthcheck-handler.go:L75-L77];
- returns **412 Precondition Failed** when `maintenance=true` **and** unhealthy [cmd/healthcheck-handler.go:L82-L83],
  **503 Service Unavailable** when simply unhealthy [cmd/healthcheck-handler.go:L85], otherwise
  **200 OK** [cmd/healthcheck-handler.go:L89].

`ClusterReadCheckHandler` [cmd/healthcheck-handler.go:L93] mirrors this using `result.HealthyRead` and sets
`X-Minio-Read-Quorum` [cmd/healthcheck-handler.go:L109]. The header string constants live in
`internal/http/headers.go`: `MinIOServerStatus` [L170], `MinIOWriteQuorum="x-minio-write-quorum"` [L193],
`MinIOReadQuorum="x-minio-read-quorum"` [L196], `MinIOStorageClassDefaults` [L200], and
`MinIOHealingDrives` [L203]. The operator-facing contract (200 on write quorum / 503 otherwise / 412 for
maintenance) is also documented at [docs/metrics/healthcheck/README.md].

**Pre-quorum 503 branches (before quorum is ever evaluated).** `checkHealth()` returns **503** *before*
computing quorum if the object layer is not yet initialized (`x-minio-server-status: offline`)
[cmd/healthcheck-handler.go:L34-L36], if bucket metadata is not initialized (`bucket-metadata-offline`)
[cmd/healthcheck-handler.go:L40-L42], or if IAM is not initialized (`iam-offline`)
[cmd/healthcheck-handler.go:L47-L48]. The table below therefore describes a **fully initialized,
non-maintenance** server (the state in which all evidence in §6 was captured).

| Endpoint | Healthy condition (initialized, non-maintenance) | Status when met | Status when not met | Quorum header |
|----------|-------------------|-----------------|---------------------|---------------|
| `/minio/health/cluster` | every set: `online ≥ write quorum` | 200 | 503 (or 412 if `maintenance=true`) | `X-Minio-Write-Quorum` |
| `/minio/health/cluster/read` | every set: `online ≥ read quorum` | 200 | 503 (or 412 if `maintenance=true`) | `X-Minio-Read-Quorum` |

Additionally, when `maintenance=true`, `Health()` also treats any in-progress healing as unhealthy
(`result.Healthy = result.Healthy && drivesHealing == 0`) [cmd/erasure-server-pool.go:L2808-L2812]; the
evidence below uses the default (non-maintenance) probe, so that branch is not exercised.


## 6. Evidence (Verbatim Captured Output)

Each block below is the **actual, unedited** output captured from the running server, shown **before** its
explanation. A block shows either the literal command or a clearly-labeled descriptive capture of the step;
long or repetitive output is shown as a clearly-labeled representative excerpt or a derived count (with the
total stated), and exit status is included where applicable. States are ordered: Baseline → One-down (above
threshold) → Two-down (below threshold) → Q4 logs → Restored → Healed → Repository pristine.

### 6.0 Non-root identity, build, version, and commit provenance

```
$ id tester
uid=1001(tester) gid=1001(tester) groups=1001(tester)

$ make build
Checking dependencies
Building minio binary to './minio'
BUILD_EXIT=0

# gen-ldflags.go stamps the banner from the BUILT commit's committer date + hash; the investigated build
# target is c07e5b49d477, so its version is DEVELOPMENT.2024-11-25T17-10-22Z (commit-id c07e5b49d477):
$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.

# Byte-identity of the compiled source to the investigated commit c07e5b49d477: on this branch the ONLY path
# added on top of c07e is this Markdown document; ZERO MinIO source or dependency files differ.
$ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2 HEAD
A	blitzy/documentation/minio_c07e5b49d477.md

# restrict the same diff to the MinIO source/dependency trees — empty output confirms no source changed:
$ git diff --stat c07e5b49d477b0774f23db3b290745aef8c01bd2 HEAD -- cmd internal docs Makefile go.mod go.sum buildscripts main.go
(no output — zero MinIO source/dependency files differ)
```

The `--version` banner above is the canonical build of the **investigated commit** `c07e5b49d477…`:
`buildscripts/gen-ldflags.go` stamps the version from the built commit's committer date and hash, so building
`c07e5b49d477…` yields `DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477…)` with `Copyright: 2015-2024`.
The `git diff` above is taken against the **absolute** investigated commit and shows the **only** path added on
top of it is this Markdown document and that **zero** MinIO source files differ — so the compiled Go source is
byte-for-byte identical to `c07e5b49d477…` and all `file:line` citations resolve against it. (This branch
carries the document as commits layered on top of `c07e5b49d477…`; building the branch tip instead would stamp
that tip's hash in the banner, but because only this document differs the compiled server is identical and its
behavior and citations are unchanged.) The server was launched as the non-root user `tester` and reported a
single four-drive set:

```
## whoami / id (non-root proof)
uid=1001(tester) gid=1001(tester) groups=1001(tester)

## server PID
188172
## ps identity
    PID USER     COMMAND
 188172 tester   /tmp/ec-run2/minio server /tmp/ec/data{1...4} --address 127.0.0.1:9000 --console-address 127.0.0.1:9001

## startup banner (first 30 lines of server.log)
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)

API: http://127.0.0.1:9000
WebUI: http://127.0.0.1:9001

Docs: https://docs.min.io

## data directory ownership/mode
/tmp/ec/data1 owner=tester:tester mode=755
/tmp/ec/data2 owner=tester:tester mode=755
/tmp/ec/data3 owner=tester:tester mode=755
/tmp/ec/data4 owner=tester:tester mode=755
```

The server ran as `uid=1001(tester)` (**PID 188172**), which is what makes the `chmod 000` fault effective
(root would bypass it). The exact binary produced by `make build` (`./minio`) was copied to a tester-owned
directory (`/tmp/ec-run2/minio`) and launched from there as `tester`; the `ps` line shows that real path.
`1 set(s), 4 drives per set` confirms the single EC:2 erasure set. This block is what makes the answer
"built-and-run" rather than code-reading.

### 6.1 Baseline — 4 drives online

```
########## STATE: baseline_run1 @ 2026-07-13T19:18:05Z ##########
----- HEAD /minio/health/cluster (curl -sI) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF85DFC1D80D
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:05 GMT

[curl-exit=0]
----- GET  /minio/health/cluster (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF85E01BF941
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:05 GMT

[curl-exit=0]
----- GET  /minio/health/cluster/read (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF85E06FC381
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:05 GMT

[curl-exit=0]
----- mc admin info eclab -----
●  127.0.0.1:9000
   Uptime: 1 second 
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK 
   Drives: 4/4 OK 
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.8% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

4 drives online, 0 drives offline, EC:2
[mc-exit=0]
```

```
----- mc cp /tmp/ec-run2/obj-baseline.txt eclab/testbucket/obj-baseline.txt -----
`/tmp/ec-run2/obj-baseline.txt` -> `eclab/testbucket/obj-baseline.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 16 B  │ 16 B        │ 00m00s   │ 1.43 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
[put-exit=0]
----- mc --json cp /tmp/ec-run2/obj-baseline.txt eclab/testbucket/obj-baseline.txt -----
{"status":"success","source":"/tmp/ec-run2/obj-baseline.txt","target":"eclab/testbucket/obj-baseline.txt","size":16,"totalCount":1,"totalSize":0}
{"status":"success","total":16,"transferred":16,"duration":12314401,"speed":1299.291780412218}
[put-json-exit=0]
----- mc cat eclab/testbucket/obj-baseline.txt -----
baseline-content[cat-exit=0]
```

With all four drives online the deployment reports healthy: `/cluster` returns **200** and advertises the
write quorum (**3**) on both HEAD and GET, `/cluster/read` returns **200** and advertises the read quorum
(**2**), all 4 drives are `EC:2` with a single erasure set of stripe size 4, and both a write and a read
succeed. (`mc cat` prints the 16-byte object with no trailing newline, so `[cat-exit=0]` appears on the same
line as `baseline-content`.) A second run (`baseline_run2`) returned an identical `200` / `4 drives online`.
This is the reference state for Q1/Q8.

### 6.2 Above threshold — `chmod 000 /tmp/ec/data1` (3 online)

```
chmod 000 /tmp/ec/data1 @ 2026-07-13T19:18:05Z
/tmp/ec/data1 mode=0 owner=tester
```

```
########## STATE: onedown_run1 @ 2026-07-13T19:18:08Z ##########
----- HEAD /minio/health/cluster (curl -sI) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF869F1391D4
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:08 GMT

[curl-exit=0]
----- GET  /minio/health/cluster (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF869F851EF5
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:08 GMT

[curl-exit=0]
----- GET  /minio/health/cluster/read (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF869FED727C
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:08 GMT

[curl-exit=0]
----- mc admin info eclab -----
●  127.0.0.1:9000
   Uptime: 4 seconds 
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK 
   Drives: 3/4 OK 
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.8% (total: 24 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

3 drives online, 1 drive offline, EC:2
[mc-exit=0]
```

```
----- mc cp /tmp/ec-run2/obj-1down.txt eclab/testbucket/obj-1down.txt -----
`/tmp/ec-run2/obj-1down.txt` -> `eclab/testbucket/obj-1down.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 23 B  │ 23 B        │ 00m00s   │ 2.24 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
[put-exit=0]
----- mc --json cp /tmp/ec-run2/obj-1down.txt eclab/testbucket/obj-1down.txt -----
{"status":"success","source":"/tmp/ec-run2/obj-1down.txt","target":"eclab/testbucket/obj-1down.txt","size":23,"totalCount":1,"totalSize":0}
{"status":"success","total":23,"transferred":23,"duration":12682310,"speed":1813.5497397556123}
[put-json-exit=0]
----- mc cat eclab/testbucket/obj-baseline.txt -----
baseline-content[cat-exit=0]
----- mc cat eclab/testbucket/obj-1down.txt -----
above-threshold-content[cat-exit=0]
```

One directory is now inaccessible, leaving **3 drives online**. Because `3 ≥ write quorum 3`, `/cluster`
stays **200** on both HEAD and GET (`X-Minio-Write-Quorum: 3`), `/cluster/read` stays **200**
(`X-Minio-Read-Quorum: 2`), and a write still succeeds — MinIO keeps serving. The offline drive is reflected
in `mc admin info` as `3 drives online, 1 drive offline` (`Drives: 3/4 OK`). A second run (`onedown_run2`)
returned an identical `200` / `3 drives online`, confirming stability.

**Parity of `obj-1down` and `obj-baseline` (decoded with the repo's `docs/debugging/xl-meta`):**

```
### xl.meta parity decode (docs/debugging/xl-meta, built by make build) ###
-- obj-1down (data2) --
        "EcM": 2,
        "EcN": 2,
-- obj-baseline (data2) --
        "EcM": 2,
        "EcN": 2,
```

The object written during the one-down window has parity `EcM=2, EcN=2` — **not** upgraded. This is
expected: MinIO's availability-optimized PUT path does increment parity per offline drive, but caps parity
at half the set (`len/2 = 2`) [cmd/erasure-object.go:L1311-L1313]; since the default parity is already 2,
four-drive EC:2 is already at **maximum** parity and cannot be upgraded further. Its shard is *missing on the
down disk* `data1` (present on the other three) — the degraded-redundancy state whose repair, and its
timing dependence on how quickly the drive returns, is demonstrated end-to-end in §6.6.

### 6.3 Below threshold — additionally `chmod 000 /tmp/ec/data2` (2 online)

```
chmod 000 /tmp/ec/data2 @ 2026-07-13T19:18:11Z
/tmp/ec/data2 mode=0 owner=tester
```

```
########## STATE: twodown_run1 @ 2026-07-13T19:18:14Z ##########
----- HEAD /minio/health/cluster (curl -sI) -----
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF87D9EF2199
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:14 GMT

[curl-exit=0]
----- GET  /minio/health/cluster (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF87DA50A493
X-Content-Type-Options: nosniff
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:14 GMT

[curl-exit=0]
----- GET  /minio/health/cluster/read (curl -sS -D- -o /dev/null -X GET) -----
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EF87DAC35880
X-Content-Type-Options: nosniff
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:18:14 GMT

[curl-exit=0]
----- mc admin info eclab -----
●  127.0.0.1:9000
   Uptime: 10 seconds 
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK 
   Drives: 2/4 OK 
   Pool: 1

┌──────┬──────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage │ Erasure stripe size │ Erasure sets │
│ 1st  │              │ 4                   │ 1            │
└──────┴──────────────┴─────────────────────┴──────────────┘

2 drives online, 2 drives offline, EC:2
[mc-exit=0]
```

```
----- mc cp /tmp/ec-run2/obj-2down.txt eclab/testbucket/obj-2down.txt -----
`/tmp/ec-run2/obj-2down.txt` -> `eclab/testbucket/obj-2down.txt`
mc: <ERROR> Failed to copy `/tmp/ec-run2/obj-2down.txt`. Resource requested is unwritable, please reduce your request rate
[put-exit=1]
----- mc --json cp /tmp/ec-run2/obj-2down.txt eclab/testbucket/obj-2down.txt -----
{"status":"success","source":"/tmp/ec-run2/obj-2down.txt","target":"eclab/testbucket/obj-2down.txt","size":23,"totalCount":1,"totalSize":0}
{"status":"error","error":{"message":"Failed to copy `/tmp/ec-run2/obj-2down.txt`.","cause":{"message":"Resource requested is unwritable, please reduce your request rate","error":{"Code":"SlowDownWrite","Message":"Resource requested is unwritable, please reduce your request rate","BucketName":"testbucket","Key":"obj-2down.txt","Resource":"/testbucket/obj-2down.txt","RequestID":"18C1EF8A0EBE098F","HostID":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8","Region":"","Server":"MinIO"}},"type":"error"}}
[put-json-exit=1]

### reads of objects written earlier ###
----- mc cat eclab/testbucket/obj-baseline.txt -----
baseline-content[cat-exit=0]
----- mc cat eclab/testbucket/obj-1down.txt -----
above-threshold-content[cat-exit=0]
```

Server log — the exact `logger.FatalKind` line emitted by `Health()` (recurs once per health poll; this run
logged it **6** times, a run-dependent count that scales with how many polls occur during the outage window).
Because every occurrence is byte-identical, **three representative lines are shown, followed by the full
occurrence count**:

```
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
occurrences: 6
```

A second directory is now inaccessible, leaving **2 drives online**. Because `2 < write quorum 3`,
`/cluster` returns **503** on both HEAD and GET and the write is **refused** with the S3 error `SlowDownWrite`
(HTTP 503, exit 1) — the `--json` form shows the full error object (`Code: SlowDownWrite`,
`Message: Resource requested is unwritable…`, `Key: obj-2down.txt`). Because `2 ≥ read quorum 2`,
`/cluster/read` stays **200** and reads still **succeed** — including `obj-1down`, which was written during
the one-down window and is fully readable while two disks are down. This is the "refuse writes, keep reads"
outcome. The `FatalKind` log line names the pool/set and the exact arithmetic
(`expected write quorum: 3, drives-online: 2`). A second run (`twodown_run2`) reproduced the same result
(503 / read-200 / PUT exit 1 / GET exit 0). (In the `--json` PUT the first `status:success` line is the
client-side local file read; the write itself is the `status:error` object that follows, hence
`[put-json-exit=1]`.)

### 6.4 Path-named log evidence + the detection nuance (Q4)

The following block reports counts taken over the **full** server log for four log-line families — the two
dedicated `monitorDiskWritable`/`monitorDiskStatus` lines and the two permission-path lines (each `grep`'s
own exit status is shown, where exit 1 means "no match"):

```
### 'taking drive offline' (monitorDiskWritable) ###
count=0
[grep-exit=1]
### 'bringing drive online' (monitorDiskStatus) ###
count=0
[grep-exit=1]
### 'drive access denied' ###
count=2
[grep-exit=0]
### '.healing.bin ... permission denied' ###
count=54
[grep-exit=0]
```

The two dedicated monitor lines are absent (`grep-exit=1` = no match). Instead, the permission fault named
the failing disk **by its filesystem path** through two live code paths. First, the set-level reconnect
(`connectDisks`) logs the endpoint by path — **both occurrences (`count=2`) are shown in full**, one per
failed disk (`data2` and `data1`):

```
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/ec/data2"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
--
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/ec/data1"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

Second, the DiskInfo/Healing probe (the path `Health()` consults) names the exact file, with the full
runtime stack — **one representative occurrence is shown in full** (this run logged 54 such lines; the total
and its run-dependence are noted below):

```
Error: unable to read /tmp/ec/data1/.minio.sys/buckets/.healing.bin: open /tmp/ec/data1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
      10: internal/logger/logger.go:268:logger.LogIf()
       9: cmd/logging.go:112:cmd.internalLogIf()
       8: cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       7: cmd/xl-storage.go:352:cmd.newXLStorage.func2()
       6: internal/cachevalue/cache.go:143:cachevalue.(*Cache[...]).update()
       5: internal/cachevalue/cache.go:128:cachevalue.(*Cache[...]).GetWithCtx()
       4: cmd/xl-storage.go:781:cmd.(*xlStorage).DiskInfo()
       3: cmd/xl-storage-disk-id-check.go:329:cmd.(*xlStorageDiskIDCheck).DiskInfo()
       2: cmd/erasure.go:192:cmd.getDisksInfo.func1()
       1: github.com/minio/pkg/v3@v3.0.22/sync/errgroup/errgroup.go:123:errgroup.(*Group).Go.func1()

API: SYSTEM.internal
```

Counts over the outage window: `endpoint="/tmp/ec/data2"` and `…/data1` each appear at fault time;
`"drive access denied"` = **2** (one per failed disk); `.healing.bin … permission denied` = **54** in this
run (the absolute count is run-dependent — a function of how many polling cycles elapse — but it is always
non-zero and always names the disk by path; a prior run logged 176). The stack trace is the runtime proof
that the permission error surfaces through the **1-second DiskInfo cache** (`cachevalue` →
`newXLStorage.func2` at [cmd/xl-storage.go:L352] → `Healing()` at [cmd/xl-storage.go:L436]), reached from
`Health()` via `getDisksInfo` [cmd/erasure.go:L192] → `DiskInfo()` [cmd/xl-storage-disk-id-check.go:L329],
[cmd/xl-storage.go:L781].

### 6.5 Self-detection / recovery (Q5) — `chmod 755` both, NO restart

```
### PID before restore ###
188172
    PID USER         ELAPSED
 188172 tester         00:22
### chmod 755 both @ 2026-07-13T19:18:26Z ###
/tmp/ec/data1 mode=755
/tmp/ec/data2 mode=755
### recovery durability — poll /cluster every 5s after restore (no restart) ###
poll t+0s  : /cluster=200 : 4 drives online
poll t+5s  : /cluster=200 : 4 drives online
poll t+10s : /cluster=200 : 4 drives online
poll t+15s : /cluster=200 : 4 drives online
poll t+20s : /cluster=200 : 4 drives online
poll t+25s : /cluster=200 : 4 drives online
poll t+30s : /cluster=200 : 4 drives online
poll t+35s : /cluster=200 : 4 drives online
poll t+40s : /cluster=200 : 4 drives online
poll t+45s : /cluster=200 : 4 drives online
### recovery latency (ms to first /cluster=200), 3 trials ###
trial 1: 12 ms
trial 2: 13 ms
trial 3: 11 ms
### PID after restore ###
188172
PID continuity: before=188172 after=188172 same=YES
```

Recovery is **automatic** and reflected on the **next health probe**, and it is **durable**: polling
`/cluster` every 5 s after the `chmod 755` shows the endpoint **already back at `200` with 4 drives online on
the very first poll (t+0s) and stable there through t+45s** — with **no restart and no external push**, and the
server **PID unchanged** (`188172` before and after), proving it all happened in the same live process. The
flip itself is fast: a tight polling loop that briefly re-faults `data1`, restores it, and times the return to
`200` measured **~11–15 ms** across runs — 12/13/11 ms in the documented three trials, 15/14/14 ms in a prior
run, and 13/13/14 ms in a fresh re-verification run, all in the low-tens of milliseconds. The
optional manual push was also demonstrated in §6.6, but it is **not** required for the drives to be
re-detected.

### 6.6 Repair of objects written during the outage (Q6)

Whether an object written while a drive was down is repaired **automatically** turns out to depend on
**timing**, so this question was isolated in a dedicated experiment on the **same canonical build**
(`DEVELOPMENT.2026-07-13T21-56-03Z`, `go1.23.12`) over the **same four-directory `/tmp/ec` layout**, run as
the same unprivileged `tester` (uid 1001). This experiment uses a **fresh server instance** — noted here
because it post-dates the main §6.1–§6.5 run — whose PID is recorded and which is stopped by that exact PID
with its scratch removed at cleanup (§6.7). Each branch was run **three times** to confirm the result is
stable, not a one-off. For every trial the drive was first **confirmed offline** (`3 drives online` on the
health-derived `mc admin info`) *before* the write — so the write genuinely skipped `data1` and enqueued a
repair entry — and the only variable was how quickly `data1` was restored **after** the PUT.

> **Why the pre-PUT offline check matters (observed, then reasoned).** MinIO holds an open descriptor to
> each drive root and issues I/O with `openat` relative to it, so a bare `chmod 000` on the drive root does
> **not** instantly stop the running server from writing there; the drive is only skipped once MinIO's own
> health monitor has marked it offline. Unless that offline state is confirmed first, a PUT can still land
> the shard directly on `data1` and no repair entry is queued. Each trial below therefore waits for
> `3 drives online` before writing.

**Branch A — short outage (drive restored <1 s after the write): the shard is repaired automatically.**
The complete transcript (per trial: confirmed-offline pre-PUT state, the write and its exit, the on-disk
shard at t=0, the immediate restore, the polled on-disk result, and a read-back):

```
==================== SHORT-OUTAGE (restore <1s after PUT) ====================
----- trial 1 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-short-1.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
$ chmod 755 /tmp/ec/data1        -> restored immediately (t=0)
RESULT: data1 shard AUTO-RESTORED at ~t+1s (425 B xl.meta) via MRF — NO manual heal
$ mc cat eclab/testbucket/obj-short-1.txt -> short-outage-1 [exit=0]

----- trial 2 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-short-2.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
$ chmod 755 /tmp/ec/data1        -> restored immediately (t=0)
RESULT: data1 shard AUTO-RESTORED at ~t+1s (425 B xl.meta) via MRF — NO manual heal
$ mc cat eclab/testbucket/obj-short-2.txt -> short-outage-2 [exit=0]

----- trial 3 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-short-3.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
$ chmod 755 /tmp/ec/data1        -> restored immediately (t=0)
RESULT: data1 shard AUTO-RESTORED at ~t+2s (425 B xl.meta) via MRF — NO manual heal
$ mc cat eclab/testbucket/obj-short-3.txt -> short-outage-3 [exit=0]
```

In all three trials the shard was **absent** on `data1` at t=0 (the write skipped the down drive) and then
**reappeared on its own within ~1–2 s** with no `mc admin heal` ever issued. This is the most-recent-failure
(MRF) path: the PUT recorded the skipped drive and enqueued the object, and the MRF consumer re-attempted the
write ~1 s later — by which time `data1` was back — reconstructing the missing shard automatically.

**Branch B — long outage (drive held down 20 s after the write): the shard is NOT repaired automatically.**
Identical steps, but `data1` is held down for 20 s after the PUT before being restored, then polled for 45 s:

```
==================== LONG-OUTAGE (hold 20s down after PUT) ====================
----- trial 1 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-long-1.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
... holding data1 DOWN 20s (MRF ~1s retry fires while disk still offline -> single retry consumed) ...
$ chmod 755 /tmp/ec/data1        -> restored after 20s outage (t=0)
RESULT: data1 shard STILL ABSENT through t+45s — MRF did NOT auto-restore (manual heal required)

----- trial 2 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-long-2.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
... holding data1 DOWN 20s (MRF ~1s retry fires while disk still offline -> single retry consumed) ...
$ chmod 755 /tmp/ec/data1        -> restored after 20s outage (t=0)
RESULT: data1 shard STILL ABSENT through t+45s — MRF did NOT auto-restore (manual heal required)

----- trial 3 -----
$ mc admin info eclab            -> data1 offline (3 drives online) [pre-PUT]
$ mc cp obj-long-3.txt eclab/testbucket/  -> [exit=0]
t=0 data1 shard: ABSENT (skipped at PUT; MRF entry queued)
... holding data1 DOWN 20s (MRF ~1s retry fires while disk still offline -> single retry consumed) ...
$ chmod 755 /tmp/ec/data1        -> restored after 20s outage (t=0)
RESULT: data1 shard STILL ABSENT through t+45s — MRF did NOT auto-restore (manual heal required)
```

In all three trials the shard was **still missing** on `data1` 45 s after restore. The single MRF retry had
already been spent ~1 s after the PUT — while `data1` was still down — so it was consumed with nothing to
write, and no background poller re-queued a drive whose permissions were merely restored (there was no
unformatted drive and no `.healing.bin` tracker; see the detection nuance in §6.4/§7 Q5). The on-disk shard
census across the four data roots confirms the split — short-outage objects present on **all four** drives,
long-outage objects missing their `data1` shard (derived from `ls` on each `data{1..4}/testbucket/<obj>/`;
`Y` = `xl.meta` present, `-` = absent):

```
$ for o in obj-short-{1,2,3} obj-long-{1,2,3}; do printf '%-14s ' "$o:"; \
    for d in 1 2 3 4; do [ -f /tmp/ec/data$d/testbucket/$o.txt/xl.meta ] && printf "data$d=Y " || printf "data$d=- "; done; echo; done
obj-short-1:   data1=Y data2=Y data3=Y data4=Y
obj-short-2:   data1=Y data2=Y data3=Y data4=Y
obj-short-3:   data1=Y data2=Y data3=Y data4=Y
obj-long-1:    data1=- data2=Y data3=Y data4=Y
obj-long-2:    data1=- data2=Y data3=Y data4=Y
obj-long-3:    data1=- data2=Y data3=Y data4=Y
```

**A manual `mc admin heal` repairs the long-outage objects** (`Yellow → Green`), while the already-repaired
short-outage objects and the fully-healthy baseline are reported `Green → Green` — one transcript that shows
both branches at once:

```
$ mc admin heal -r --force eclab/testbucket
[Green  ->  Green] testbucket/
[Green  ->  Green] testbucket/obj-baseline.txt
[Yellow ->  Green] testbucket/obj-long-1.txt
[Yellow ->  Green] testbucket/obj-long-2.txt
[Yellow ->  Green] testbucket/obj-long-3.txt
[Green  ->  Green] testbucket/obj-short-1.txt
[Green  ->  Green] testbucket/obj-short-2.txt
[Green  ->  Green] testbucket/obj-short-3.txt
Healed:	3/7 objects; 97 B in 1s
[heal-exit=0]
```

After the heal, every long-outage object has its `data1` shard back, byte-sized identically to the surviving
shards, and the reconstructed shard decodes to the same EC:2 geometry (`EcM=2` data, `EcN=2` parity):

```
$ ls -l /tmp/ec/data1/testbucket/obj-long-1.txt/
total 4
-rw-r--r-- 1 tester tester 425 Jul 14 00:28 xl.meta

$ xl-meta /tmp/ec/data1/testbucket/obj-long-1.txt/xl.meta | grep -E '"Ec[MN]"|"EcAlgo"|"EcBSize"|"EcIndex"'
        "EcM": 2,
        "EcN": 2,
          "EcAlgo": 1,
          "EcBSize": 1048576,
          "EcIndex": 3,
```

Throughout **both** branches the object stayed **readable** — it was always reconstructable from the
surviving `data2`/`data3`/`data4` shards (read quorum 2). What differs is only *when full redundancy on
`data1` is restored*. If the drive is back within the ~1 s MRF window, the missing shard is rebuilt
**automatically** (short outage, Branch A); otherwise the single MRF retry is consumed with the drive still
offline and the shard stays missing until a heal runs — an on-demand `mc admin heal` (Branch B), or, more
slowly, the periodic data scanner. Because the drive here was only *permission-restored* (not reformatted and
carrying no `.healing.bin` tracker), the dedicated fresh-disk heal poller did not adopt it, which is why the
long-outage shard persisted until the manual heal.

### 6.7 Cleanup and repository left pristine

```
########## GUARDED CLEANUP TRANSCRIPT @ 2026-07-13T19:21:54Z ##########
### server PID captured at launch ###
MINIO_PID=188172
### stop server by EXACT pid (SIGTERM); never pkill/killall ###
[kill-exit=0]
process 188172 gone (confirmed)
### confirm no tester minio remains ###
(no matching minio process)
### remove data dirs ###
ls: cannot access '/tmp/ec': No such file or directory
### remove built binary + docs/debugging helper binaries from repo root ###
(repo binaries removed)
### git status --porcelain (only the doc should differ) ###
 M blitzy/documentation/minio_c07e5b49d477.md
### only this document differs from the investigated MinIO source c07e5b49d477 (NO source/dependency change) ###
$ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2 HEAD
A	blitzy/documentation/minio_c07e5b49d477.md
```

The server was stopped by its **exact PID** (`188172`, via `kill`; never a broad `pkill`/`killall`) and
confirmed gone. The data directories (`/tmp/ec`), the `./minio` binary, and every `docs/debugging` helper
binary built by `make build` (`xl-meta`, `s3-check-md5`, `healing-bin`, `reorder-disks`, `hash-set`,
`inspect`, `pprofgoparser`, `s3-verify`, `xattr`) were physically removed. `git status --porcelain` shows
**only** `blitzy/documentation/minio_c07e5b49d477.md` — this document, the single intended artifact — and no
other change; the MinIO source tree is untouched. The remaining transient artifacts kept only to author this
document — the isolated `mc` config, the ephemeral credentials, the run-local `mc` copy, the captured
evidence files, and scratch logs (all under a tester-owned scratch directory outside the repository) — are
deleted at the very end of the investigation. The **dedicated Q6 timing experiment (§6.6)** was run
afterward on a second, equally short-lived server instance over the same `/tmp/ec` layout as the same
`tester` user; it created **no** repository files (every artifact lived under `/tmp`), and that instance was
likewise stopped by its **exact PID** and its scratch (`/tmp/ec`, its run/log directory, and its isolated
`mc` config) removed — so the final `git status --porcelain` still shows only this document. The
pre-provisioned `mc` (see §3 provenance) is environment-supplied tooling, is not part of the repository, and
does not affect repository cleanliness.


## 7. Per-Question Answers (Q1–Q8)

Each answer states the **observed** result, the **`file:line`** citation(s), and the **cause → effect**
rationale. Statements derived from reading code rather than from observation are explicitly labeled
**(inferred, from code)**.

### Q1 — Health decision & disk assumptions

**Answer.** MinIO reports the deployment "healthy" only if **every** erasure set has at least *write-quorum*
drives online. For this four-directory EC:2 topology the write quorum is **3** and the read quorum is **2**.

**Observed (§6.1).** `GET /minio/health/cluster` → **200** with `X-Minio-Write-Quorum: 3`;
`GET /minio/health/cluster/read` → **200** with `X-Minio-Read-Quorum: 2`; `mc admin info` → `4 drives
online, 0 drives offline, EC:2`.

**Code.** `Health()` [cmd/erasure-server-pool.go:L2679] iterates the drives from `StorageInfo` and counts a
drive online **only** when its state is `madmin.DriveStateOk` [cmd/erasure-server-pool.go:L2707-L2709]. It
then derives the per-pool quorums **itself** from `z.BackendInfo()` [cmd/erasure-server-pool.go:L2719]:
`poolReadQuorums[i] = data` and `poolWriteQuorums[i] = data` with a `+1` when `data == StandardSCParity`
[cmd/erasure-server-pool.go:L2720-L2727], where `data = setDriveCount − scParity`
[cmd/erasure-server-pool.go:L694] and `scParity` is the default parity `2` [cmd/erasure-server-pool.go:L686-L688].
Each set is healthy iff `online ≥ poolWriteQuorums[poolIdx]` [cmd/erasure-server-pool.go:L2791], and the
deployment is the AND across all sets (`result.Healthy = result.Healthy && healthy`)
[cmd/erasure-server-pool.go:L2797]. The handler surfaces the decision as the HTTP status and the
`X-Minio-Write-Quorum` header [cmd/healthcheck-handler.go:L72,L85,L89].

**Cause → effect.** "Healthy" is not "all disks present" — it is "every set can still safely accept a
write," i.e. `online ≥ 3`. That is why 4/4 online is 200: the assumed minimum number of disks to *proceed*
with writes is the write quorum (3), not the full four.

### Q2 — Live permission-loss behavior

**Answer.** It depends entirely on which side of the write quorum the loss leaves you on. While still at or
above write quorum, MinIO **keeps serving** (including writes). The moment the loss drops the set **below**
write quorum, MinIO **refuses writes** (reads continue while read quorum still holds).

**Observed.** One directory lost → 3 online → `/cluster` **200**, PUT exit 0 (§6.2). Second directory lost →
2 online → `/cluster` **503**, PUT exit 1 with `SlowDownWrite` (§6.3).

**Code (the actual PUT path).** In `putObject` [cmd/erasure-object.go:L1245], the availability-optimized
block counts offline drives and, if `offlineDrives ≥ (len(storageDisks)+1)/2`, returns immediately with
`toObjectErr(errErasureWriteQuorum, …)` [cmd/erasure-object.go:L1304-L1308] — with two of four offline,
`2 ≥ (4+1)/2 = 2`, so this is the branch that fires. Otherwise the write proceeds with
`writeQuorum = dataDrives (+1 when dataDrives == parityDrives)` [cmd/erasure-object.go:L1322-L1326] and
`erasure.Encode(…, writeQuorum)` [cmd/erasure-object.go:L1425]; inside `multiWriter.Write`, if fewer than
`writeQuorum` shards are written, `reduceWriteQuorumErrs` yields `errErasureWriteQuorum`
[cmd/erasure-encode.go:L61-L65]. Either way `toObjectErr` maps `errErasureWriteQuorum` to
`InsufficientWriteQuorum{}` [cmd/object-api-errors.go:L164-L172] (`errErasureWriteQuorum` = "Write failed.
Insufficient number of drives online" [cmd/erasure-errors.go:L26]), which the S3 layer maps to
`ErrSlowDownWrite` — code `SlowDownWrite`, HTTP 503 [cmd/api-errors.go:L2314-L2315], [cmd/api-errors.go:L874-L877].

**Cause → effect.** MinIO does not tolerate an arbitrary number of failures — it tolerates exactly as many
as the write quorum allows. Above the line it keeps writing (at reduced redundancy); at the line it stops
writing to protect durability.

### Q3 — Above vs below threshold (both demonstrated)

**Answer.**
- **Above threshold (one disk down, 3 online):** `/cluster` **200**; writes **succeed**; reads succeed.
- **Below threshold (two disks down, 2 online):** `/cluster` **503**; writes **refused** (`SlowDownWrite`);
  **reads still succeed** because 2 online meets read quorum 2, so `/cluster/read` stays **200**.

**Observed.** §6.2 (above) vs §6.3 (below), plus the exact `FatalKind` server-log line
`Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2`, and a
real GET of `obj-1down` succeeding during the two-down state.

**Code.** The per-set decision and both log lines are in `Health()`: write-quorum failure is checked at
[cmd/erasure-server-pool.go:L2791] and logged with `logger.FatalKind` [cmd/erasure-server-pool.go:L2794-L2795];
read-quorum health is checked at [cmd/erasure-server-pool.go:L2799] and logged at
[cmd/erasure-server-pool.go:L2802]. The read path returns data while `online ≥ read quorum`
(read quorum = data blocks = 2) [cmd/erasure-server-pool.go:L2723], [cmd/storage-datatypes.go:L310-L316].

**Cause → effect.** Two online is simultaneously **below** write quorum 3 and **at** read quorum 2. That one
arithmetic fact is why the same two-disk-down state produces a 503 on `/cluster` (write availability lost)
but a 200 on `/cluster/read` (read availability retained) — writes need a strict majority (with the +1
guard), reads only need the data blocks.

### Q4 — Path-named logs & live recovery

**Answer.** **Yes — the logs name the failing disk by its exact filesystem path**, and re-probing runs while
the server stays live. There is an important nuance: for a pure **permission** fault the disk is excluded
from the online count through the **DiskInfo / Healing** path (which names the path), while the **dedicated**
`monitorDiskWritable` offline / `monitorDiskStatus` online log lines do **not** fire.

**Observed (§6.4).** The path appears as `endpoint="/tmp/ec/data1"` and in
`/tmp/ec/data1/.minio.sys/buckets/.healing.bin: … permission denied`, with a runtime stack trace. The counts
of `"taking drive … offline"` and `"bringing drive … online"` were both **0**.

**Code — what fired (observed).** At runtime a `chmod 000` surfaces as `errDiskAccessDenied` from
`checkFormatJSON()`, which does `Lstat(s.formatFile)` and maps `osIsPermission(err) → errDiskAccessDenied`
[cmd/xl-storage.go:L802-L826]; this is reached from `GetDiskID()` [cmd/xl-storage.go:L828] and from the
1-second **DiskInfo cache** [cmd/xl-storage.go:L326-L359] whose updater also calls `Healing()`
[cmd/xl-storage.go:L436] (the observed `.healing.bin … permission denied` stack). `errDiskAccessDenied`
= "drive access denied" [cmd/storage-errors.go:L68]; `diskErrToDriveState` maps it to
`madmin.DriveStatePermission` [cmd/erasure.go:L98,L107], which is **not** `DriveStateOk`, so `Health()`
excludes the drive from the online count [cmd/erasure-server-pool.go:L2707-L2709]. (This is the runtime path;
the superficially similar mapping in `formatErasureMigrate` at [cmd/xl-storage.go:L276-L279] runs only at
disk **initialization**, not on a live permission change.)

**Code — what did NOT fire, and why.** The dedicated live monitor logs the disk by path via
`p.storage.String()`: `"node(%s): taking drive %s offline: %v"` [cmd/xl-storage-disk-id-check.go:L1015] and
`"node(%s): … bringing drive %s online"` [cmd/xl-storage-disk-id-check.go:L956]. The offline line lives
inside `goOffline`, which is invoked from **three** places in `monitorDiskWritable`: a **timeout** branch
when a write+read exceeds the max drive timeout [cmd/xl-storage-disk-id-check.go:L1034-L1035], and two
`errFaultyDisk` branches on write/read errors [cmd/xl-storage-disk-id-check.go:L1044-L1045,L1051-L1052].
A permission error maps to `errFileAccessDenied` (not `errFaultyDisk`), and the probe write returns
immediately (so the timeout branch does not fire either) — hence `goOffline` is never called and both
path-named monitor lines stay at 0, exactly as observed. **(inferred, from code):** for a genuine
`errFaultyDisk` (I/O) fault or a probe that exceeds the timeout, these `monitorDiskWritable`/
`monitorDiskStatus` offline/online lines *would* fire; that fault type was not injected here, so the claim
is labeled inferred.

**Cause → effect.** "Does the log call out the failing disk by path?" — yes, unambiguously
(`endpoint="/tmp/ec/data1"` and the `.healing.bin` path). "Is recovery attempted while live?" — yes: the
DiskInfo/Healing machinery re-probes on every health/StorageInfo call (§Q5), so no restart is needed. The
subtlety is simply *which* code path names the disk, and that depends on the fault *type* (permission vs
I/O/timeout).

### Q5 — Self-detection of a restored directory

**Answer.** MinIO recognizes a restored directory **on its own, automatically**, reflected on the **next
health probe** after permissions are restored — on the first 5 s poll the endpoint is already back at
`200`/4-online and stays there, and the flip itself is **~11–15 ms** in a tight poll — **no restart and no
external push** are required. A manual push (`mc admin heal`) exists but is optional and is a separate concern
(it repairs data; it is not needed for re-detection).

**Observed (§6.5).** After `chmod 755` (no restart), the very first `/cluster` GET returned **200** with
`4 drives online, 0 drives offline`, and it stayed there: polling every 5 s showed `200` / 4 online from the
first poll (t+0s) through t+45s. The flip is fast — three tight-loop trials measured 12/13/11 ms to the first
200 (a prior run 15/14/14 ms, a fresh re-verification run 13/13/14 ms; **~11–15 ms** overall) — and the server
PID was unchanged (`188172`) across every fault cycle.

**Code — the mechanism actually responsible (observed).** The health endpoint calls `Health()` →
`StorageInfo` → `getDisksInfo` [cmd/erasure.go:L192] → `DiskInfo()`
[cmd/xl-storage-disk-id-check.go:L329], [cmd/xl-storage.go:L781], which reads through the **1-second DiskInfo
cache** [cmd/xl-storage.go:L326-L359]. Once permissions are restored, the next probe re-reads the disk,
`checkFormatJSON`/`GetDiskID` succeed, the state becomes `DriveStateOk`, and the drive is counted online
again. This is why recovery tracks the health-probe cadence (observed ~11–15 ms), not any fixed background
interval.

**Code — the periodic pollers and their actual eligibility (for completeness).**

| Poller | Interval | Eligibility / role | Source |
|--------|----------|--------------------|--------|
| DiskInfo cache re-read | ≤ 1 s | Re-probes disk state on each `Health`/`StorageInfo` call; **this is what flipped the endpoint back to 200** | [cmd/xl-storage.go:L326-L359] |
| `monitorDiskStatus` | 5 s | Re-probes a drive and brings it online — **but only starts after `goOffline`**, which the permission fault never triggered (§Q4); therefore **not** the mechanism here | [cmd/xl-storage-disk-id-check.go:L930-L931] |
| `monitorLocalDisksAndHeal` | 10 s | Heals **only** disks queued in the heal state — i.e. unformatted disks or disks with a `.healing.bin` tracker (`getLocalDisksToHeal`); a permission-restored disk is neither | [cmd/background-newdisks-heal-ops.go:L40,L563], [cmd/background-newdisks-heal-ops.go:L393-L406] |
| `monitorAndConnectEndpoints` | 15 s | Calls `connectDisks` to reconnect disconnected set endpoints | [cmd/erasure-sets.go:L348,L283], [cmd/erasure-sets.go:L194] |

**(inferred, from code):** the exact instant at which each background poller re-includes the drive was not
separately isolated in the log; what is **directly observed** is that the health endpoint returned to 200 on
the next probe (~11–15 ms), driven by the on-demand DiskInfo re-read. The 5/10/15 s pollers are described from
code with their actual eligibility; no single background poller is claimed to be the "winner."

**Cause → effect.** Because the DiskInfo path re-probes on every health/StorageInfo call, restoring the OS
permission is sufficient — the next probe sees the drive readable again, re-includes it in the online count,
and the endpoint flips back to 200 without operator intervention.

### Q6 — Repair of objects written during the outage

**Answer.** An object written while a drive was down is **missing its shard on that drive**, and whether it
is repaired **automatically** depends on **how quickly the drive returns** relative to the ~1 s
most-recent-failure (MRF) retry window. If the drive is back within ~1 s of the write (**short outage**), the
MRF retry rebuilds the missing shard **automatically** — no operator action. If the drive stays down past
that window (**long outage**), the single MRF retry is consumed while the drive is still offline and the
shard remains missing until a heal runs — an on-demand `mc admin heal` or, more slowly, the periodic data
scanner. The object stays **readable** the whole time (reconstructed from the surviving shards).

**Observed (§6.6, 3 trials each).** *Short outage* — the shard was **absent** on `data1` at t=0 and
**reappeared on its own within ~1–2 s** (425 B `xl.meta`) with no heal issued, so all three `obj-short-*`
ended up present on all four drives. *Long outage* — after holding `data1` down 20 s, the shard was **still
missing 45 s after restore** for all three `obj-long-*`. A single `mc admin heal -r --force` then reported
`Yellow → Green` for the three long-outage objects and `Green → Green` for the already-repaired short-outage
objects (`Healed: 3/7 objects; 97 B in 1s`); afterwards each long-outage `data1` shard was present (425 B)
and decoded to `EcM=2, EcN=2`.

**Code.** Objects whose write saw an offline drive are enqueued for most-recent-failure (MRF) repair at PUT
time (`er.addPartial` / `globalMRFState.addPartialOp`) [cmd/erasure-object.go:L1566-L1585], and the MRF
consumer `healRoutine()` [cmd/mrf.go:L220] drains the queue: it waits ~1 s after the entry was queued
[cmd/mrf.go:L250-L254] (the comment notes this is to *"let recently failed networks reconnect"*) and then
calls `healObject` [cmd/mrf.go:L276] — this is what rebuilds the shard automatically in the short-outage
case, when `data1` is back in time. The background fresh-disk heal `healFreshDisk()`
[cmd/background-newdisks-heal-ops.go:L419], scheduled by `monitorLocalDisksAndHeal()`
[cmd/background-newdisks-heal-ops.go:L563], acts **only** on disks queued by `getLocalDisksToHeal()`, which
requires an **unformatted** disk or one carrying an unfinished `.healing.bin` tracker
[cmd/background-newdisks-heal-ops.go:L393-L406] — neither is true for a drive that merely had its permissions
restored, which is why the long-outage shard is not auto-adopted for healing (the single MRF retry having
already fired while the drive was still down). The on-demand admin heal is `HealHandler`
[cmd/admin-handlers.go:L1308] (`mc admin heal`); the data scanner provides an additional, slower background
heal path **(inferred, from code — not separately observed in this run)**.

**Parity note (observed).** The healed shards remained `EcM=2, EcN=2` (§6.6 `xl-meta` decode). This is
expected, not an anomaly: the availability-optimized PUT increments parity per offline drive but caps it at
half the set (`len/2 = 2`) [cmd/erasure-object.go:L1291-L1316]; four-drive EC:2 is already at **maximum**
parity, so there is no headroom to upgrade.

**Cause → effect.** Erasure coding meant the object was **reconstructable** while `data1` was down (2 data +
2 parity, 3 of 4 shards present), so reads never failed. Its **redundancy was degraded** (a shard was
missing) until a heal rebuilt it. The MRF retry closes that gap **automatically within ~1 s when the drive
returns quickly** (short outage); when the drive returns later, full redundancy is restored by the next heal
— an on-demand `mc admin heal` here — which rebuilds the missing shard from the survivors and writes it back
to the recovered drive, returning the object to `Green`.

### Q7 — Location of the quorum decision in code

**Answer.** There are **two** quorum sites, and they agree by construction:

1. **Cluster health** computes the threshold inside `Health()` from `BackendInfo()`
   [cmd/erasure-server-pool.go:L2719-L2727] — it does **not** call `defaultWQuorum()`.
2. **The object write path** uses `defaultWQuorum()`/`defaultRQuorum()` [cmd/erasure.go:L85-L96] and the
   per-object `FileInfo.WriteQuorum()`/`ReadQuorum()` [cmd/storage-datatypes.go:L298-L316].

Both apply the **+1 split-brain guard**: write quorum = data blocks, **plus one** when `data == parity`.

**Observed.** The endpoint header `X-Minio-Write-Quorum: 3` (§6.1) and the log line
`expected write quorum: 3, drives-online: 2` (§6.3) both surface the exact computed value.

**Code (walk-through).**
1. Default parity for 4 drives = 2 → `DefaultParityBlocks(4)`
   [internal/config/storageclass/storage-class.go:L355,L361-L362].
2. **Cluster-health site:** `Health()` reads `b := z.BackendInfo()` [cmd/erasure-server-pool.go:L2719],
   where `StandardSCData = setDriveCount − scParity = 2` [cmd/erasure-server-pool.go:L694] and
   `StandardSCParity = 2` [cmd/erasure-server-pool.go:L700]; then `poolReadQuorums = 2` and
   `poolWriteQuorums = 2 (+1 because data == parity) = 3` [cmd/erasure-server-pool.go:L2720-L2727]. The
   per-set test `online ≥ poolWriteQuorums` [cmd/erasure-server-pool.go:L2791] logs failures with
   `logger.FatalKind` [cmd/erasure-server-pool.go:L2794-L2795].
3. **Object-write site:** `defaultRQuorum()` returns `2` [cmd/erasure.go:L94-L96]; `defaultWQuorum()` returns
   `dataCount + 1 = 3` because `dataCount == defaultParityCount` [cmd/erasure.go:L85-L91]; the per-object
   rule mirrors it (`if DataBlocks == ParityBlocks { quorum++ }`) [cmd/storage-datatypes.go:L298-L308].
4. On the write path an unmet quorum becomes `errErasureWriteQuorum`
   [cmd/erasure-object.go:L1308], [cmd/erasure-encode.go:L61-L65] → `InsufficientWriteQuorum{}`
   [cmd/object-api-errors.go:L164-L172] → S3 `SlowDownWrite` (503) [cmd/api-errors.go:L2314-L2315]. The
   error reduction against the threshold is `reduceWriteQuorumErrs` [cmd/erasure-metadata-utils.go].

**Cause → effect.** The threshold is not a magic constant — it is derived from the set size and parity at
both sites. The +1 rule when parity is exactly half the set is precisely the "risk too high, stop" line: it
forbids acknowledging a write that only a tie-breakable half of the drives received.

### Q8 — Grounding in the health endpoint and real writes

**Answer.** Every behavioral conclusion in this document is paired with **both** a health-endpoint response
(status + `X-Minio-*` headers) **and** a real S3 write/read result from the running server.

**Observed pairing (each cell is backed by the raw output in §6):**

| State | `/cluster` | `/cluster/read` | PUT | GET | Drives |
|-------|-----------|-----------------|-----|-----|--------|
| Baseline (§6.1) | 200 (`WQ 3`) | 200 (`RQ 2`) | success (exit 0) | `baseline-content` (exit 0) | 4 online |
| One down / above (§6.2) | 200 (`WQ 3`) | 200 (`RQ 2`) | success (exit 0) | `baseline-content` + `above-threshold-content` (exit 0) | 3 online, 1 offline |
| Two down / below (§6.3) | **503** (`WQ 3`) | 200 (`RQ 2`) | **fail** `SlowDownWrite` (exit 1) | `baseline-content` + `above-threshold-content` (exit 0) | 2 online, 2 offline |
| Restored (§6.5/§6.6) | 200 | 200 | writable again; `mc admin heal` exit 0 | `above-threshold-content` (exit 0) | 4 online |

**Code.** The endpoint that produces these codes/headers is `ClusterCheckHandler`
[cmd/healthcheck-handler.go:L56] (and `ClusterReadCheckHandler` [cmd/healthcheck-handler.go:L93]); the write
result is produced by the object layer's quorum enforcement (Q7). The two signals always agree: a 503 on
`/cluster` co-occurs with a `SlowDownWrite` PUT, and a 200 on `/cluster/read` co-occurs with a succeeding
GET.

**Cause → effect.** Grounding both sides matters because the health endpoint is a *prediction* and the
write/read attempt is the *reality*; observing them together confirms the endpoint's 200/503 truly tracks
whether an S3 write will be accepted or refused.


## 8. Code-Trace — The Quorum Decision, End to End

The decision "do we have enough disks to proceed?" flows through **two** chains that share the same
threshold: the cluster-health chain (what the endpoint reports) and the object-write chain (what a PUT
returns). Line numbers are verified against the investigated source.

```
                       default parity for 4 drives = 2
   DefaultParityBlocks(4)  ── internal/config/storageclass/storage-class.go:L355 (case 4,5: return 2, L361-L362)
                                         │
        ┌────────────────────────────────┴─────────────────────────────────┐
        ▼  CLUSTER-HEALTH CHAIN                                              ▼  OBJECT-WRITE CHAIN
   Health(): b := z.BackendInfo()   ── cmd/erasure-server-pool.go:L2719     putObject()  ── cmd/erasure-object.go:L1245
     StandardSCData = 4-2 = 2       ── :L694                                  parityDrives default 2; availability-
     StandardSCParity = 2           ── :L700                                  optimized upgrade capped at len/2=2
     poolReadQuorums  = 2           ── :L2723                                 ── cmd/erasure-object.go:L1291-L1316
     poolWriteQuorums = 2 (+1) = 3  ── :L2722-L2727                          if offlineDrives >= (N+1)/2:
                                         │                                      return errErasureWriteQuorum ── :L1308
     count online (DriveStateOk only)── :L2707-L2709                          else writeQuorum = data(+1)=3  ── :L1322-L1326
     healthy := online >= WQuorum   ── :L2791                                 erasure.Encode(..., writeQuorum) ── :L1425
     if !healthy: log FatalKind     ── :L2794-L2795                             multiWriter.Write:
     "expected write quorum: 3,                                                  nilCount < wq -> reduceWriteQuorumErrs
      drives-online: 2"                                                          -> errErasureWriteQuorum
                                         │                                        ── cmd/erasure-encode.go:L61-L65
                                         ▼                                       │
   ClusterCheckHandler:                                                         ▼
     set X-Minio-Write-Quorum; 200 if healthy else 503 (412 if maintenance)   toObjectErr(errErasureWriteQuorum)
     ── cmd/healthcheck-handler.go:L72, L82-L83, L85, L89                       -> InsufficientWriteQuorum{}
                                                                                ── cmd/object-api-errors.go:L164-L172
   errErasureWriteQuorum = "Write failed. Insufficient number of drives         (Unwrap -> errErasureWriteQuorum ── :L253-L254)
   online"  ── cmd/erasure-errors.go:L26                                        │
                                                                                ▼
                                            S3 mapping: InsufficientWriteQuorum -> ErrSlowDownWrite (HTTP 503)
                                              ── cmd/api-errors.go:L2314-L2315 ; def L874-L877 (Code "SlowDownWrite")
```

**Reading the chains.** The parity default (2) and the set size (4) fully determine the two thresholds. The
write threshold picks up the **+1 split-brain guard** because parity equals half the set. `Health()` is the
cluster-level aggregator that turns per-set online counts into the 200/503 the endpoint reports (deriving
the quorum from `BackendInfo`, not `defaultWQuorum`), and it is also where the `FatalKind` diagnostic is
logged. On the data path the very same numeric threshold, when unmet, becomes `errErasureWriteQuorum` →
`InsufficientWriteQuorum` → the client-visible `SlowDownWrite` (503). The read side is the mirror image with
no +1: read quorum 2, surfaced as `InsufficientReadQuorum` [cmd/object-api-errors.go:L228-L242] →
`ErrSlowDownRead` [cmd/api-errors.go:L2316-L2317] only when fewer than two drives remain.

## 9. Coverage Pass

| Objective / mechanism | Value | `file:line` | Evidence | Rationale |
|-----------------------|-------|-------------|----------|-----------|
| **Q1** health/quorum decision | healthy ⇔ every set `online ≥ 3` | `Health()` [cmd/erasure-server-pool.go:L2679]; quorum calc [L2719-L2727] | §6.1 (200, `X-Minio-Write-Quorum: 3`) | §7 Q1 |
| **Q2** live permission-loss | keep writing (3 online) vs refuse (2 online) | PUT quorum check [cmd/erasure-object.go:L1304-L1308] | §6.2 / §6.3 | §7 Q2 |
| **Q3** above vs below threshold | 200/PUT-ok vs 503/`SlowDownWrite`/GET-ok | `Health()` [cmd/erasure-server-pool.go:L2791,L2794-L2795] | §6.2 / §6.3 + `FatalKind` | §7 Q3 |
| **Q4** path-named logs & live recovery | disk named by path; offline/online monitor lines = 0 for permission fault | runtime perm map [cmd/xl-storage.go:L802-L826]; offline log [cmd/xl-storage-disk-id-check.go:L1015]; goOffline branches [L1034-L1052] | §6.4 (`endpoint="/tmp/ec/data1"`, `.healing.bin`, counts 0) | §7 Q4 — I/O/timeout route **(inferred, from code)** |
| **Q5** self-detection | automatic; next probe (~11–15 ms), no restart/push | DiskInfo cache [cmd/xl-storage.go:L326-L359]; pollers [cmd/xl-storage-disk-id-check.go:L930-L931], [cmd/background-newdisks-heal-ops.go:L40,L563], [cmd/erasure-sets.go:L348] | §6.5 (first 5 s poll → 200/4-online, stable through t+45s, same PID; 12/13/11 ms, prior 15/14/14, re-verify 13/13/14) | §7 Q5 — poller isolation **(inferred, from code)** |
| **Q6** repair of outage writes | **timing-dependent**: MRF auto-repairs if the drive returns <~1 s (short); otherwise manual/scanner heal (long) | MRF enqueue [cmd/erasure-object.go:L1566-L1585]; `healRoutine()` + ~1 s retry [cmd/mrf.go:L220-L254]; `healFreshDisk()`/`getLocalDisksToHeal` [cmd/background-newdisks-heal-ops.go:L419,L393-L406] | §6.6 (short: auto ~t+1–2 s, 3/3; long: missing t+45 s, 3/3 → `mc admin heal` `Yellow→Green`) | §7 Q6 — scanner path **(inferred, from code)** |
| **Q7** quorum decision location | cluster: `BackendInfo`; object: `defaultWQuorum`; both +1 → 3 | quorum calc [cmd/erasure-server-pool.go:L2722-L2727]; `defaultWQuorum()` [cmd/erasure.go:L85]; `FileInfo.WriteQuorum` [cmd/storage-datatypes.go:L298-L308] | §6.1 header, §6.3 log | §7 Q7 / §8 |
| **Q8** grounding | endpoint code/headers + real write/read at every state | `ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] | §6.1–§6.6, §7 Q8 table | §7 Q8 |
| **quorum decision** | 3 / 2 | cluster [cmd/erasure-server-pool.go:L2722-L2727]; object [cmd/erasure.go:L85-L96] | §4, §6.1 | §7 Q7 |
| **health endpoint** | 200 / 503 / 412 + quorum headers; pre-quorum 503 branches | [cmd/healthcheck-handler.go:L32-L89], [cmd/healthcheck-router.go:L30-L44] | §5, §6.1–§6.3 | §5 |
| **path-named logs** | disk named by path | connectDisks [cmd/erasure-sets.go:L230]; Healing [cmd/xl-storage.go:L436]; offline/online [cmd/xl-storage-disk-id-check.go:L1015,L956] | §6.4 | §7 Q4 |
| **polling (1 s / 5 s / 10 s / 15 s)** | DiskInfo cache 1 s; 5/10/15 s pollers | [cmd/xl-storage.go:L326]; [cmd/xl-storage-disk-id-check.go:L930-L931]; [cmd/background-newdisks-heal-ops.go:L40]; [cmd/erasure-sets.go:L348] | §6.5 | §7 Q5 |
| **healing (`healFreshDisk`)** | conditional (unformatted or `.healing.bin` only); long-outage shard re-created by manual heal | [cmd/background-newdisks-heal-ops.go:L419]; gate [L393-L406] | §6.6 | §7 Q6 |
| **MRF** | most-recent-failure repair; ~1 s retry auto-heals the short outage | enqueue [cmd/erasure-object.go:L1566-L1585]; consumer + ~1 s wait [cmd/mrf.go:L220-L254] | §6.6 (short 3/3 auto) | §7 Q6 |

**Labeling summary.** Behavioral claims are backed by the observed output in §6. The statements explicitly
labeled **(inferred, from code)** are: the `errFaultyDisk`/timeout `goOffline` route in **Q4** (only a
permission fault, not an I/O/timeout fault, was injected); the precise per-poller re-inclusion instant in
**Q5** (only the on-demand DiskInfo re-read was directly timed); and the background data-scanner heal path in
**Q6** (both the MRF auto-repair and the manual `mc admin heal` were directly observed; only the slower background data-scanner heal path was not).

---

*Scope note: this document is the sole committed artifact of the investigation. The MinIO source tree was
read only and left unchanged; all temporary artifacts (the compiled binary and `docs/debugging` helpers, the
`mc` client copy, the `/tmp/ec/data{1..4}` directories, the isolated `mc` config, observation scripts, and
logs) were removed after the investigation, and `git status --porcelain` is empty aside from this file (§6.7).*
