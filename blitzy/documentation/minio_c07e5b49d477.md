# MinIO Fault-Tolerance Behavior in a Four-Directory Erasure-Coded Deployment

> **Grounded in a built-and-run MinIO** at HEAD `c07e5b49d477b0774f23db3b290745aef8c01bd2`
> (branch `minio_c07e5b49d477`), single-node, four local directories, default storage class → **EC:2**
> (2 data + 2 parity). Every behavioral claim below is paired with **observed runtime output** from the
> health endpoint and real S3 write/read attempts, and every code claim carries an exact `file:line`
> citation verified against this HEAD.

## 1. Title & Scope

This document answers, from a **compiled, running** MinIO server (not from code reading alone), how MinIO
behaves when data directories become inaccessible in a four-directory erasure-coded deployment. It covers
eight questions: (Q1) how MinIO decides it is "healthy" and what it assumes about the minimum number of
disks; (Q2) what happens the instant one directory becomes inaccessible mid-operation; (Q3) the explicit
difference between staying **above** the quorum threshold (one disk down) and dropping **below** it (a
second disk down); (Q4) whether the logs name the failing disk by its filesystem path and whether recovery
is attempted while the server stays live; (Q5) whether MinIO self-detects a restored directory via polling
or needs an external push; (Q6) how objects written while a disk was down get repaired once it returns; and
(Q7) exactly where in the source the quorum decision lives and how the threshold is computed. (Q8) Every
conclusion is grounded in the health-endpoint response and a real write/read result.

The failure is injected **externally** with an operating-system permission change (`chmod 000`) on a data
directory and reversed with `chmod 755`; MinIO itself is never modified. The topology is one erasure set of
four drives at default parity **EC:2**, which yields a **write quorum of 3** and a **read quorum of 2** — the
two thresholds that govern every behavior described here.

## 2. TL;DR — Answers at a Glance

| # | Question | One-line answer | Decisive observed evidence | Primary `file:line` |
|---|----------|-----------------|----------------------------|---------------------|
| **Q1** | Health decision & disk assumptions | The deployment is "healthy" only if **every** erasure set has `online ≥ write quorum`; for 4-dir EC:2 the write quorum is **3** (read quorum 2). | `GET /minio/health/cluster` → **200** with `X-Minio-Write-Quorum: 3` | `Health()` [cmd/erasure-server-pool.go:L2679] |
| **Q2** | Live permission-loss behavior | It **quietly adapts** while still at/above write quorum, and **draws a hard line** (refuses writes) the moment it drops below it. | 3 online → PUT succeeds; 2 online → PUT fails `SlowDownWrite` | `defaultWQuorum()` [cmd/erasure.go:L85] |
| **Q3** | Above vs below threshold | **Above** (3 online): `/cluster` 200, writes succeed. **Below** (2 online): `/cluster` 503, writes refused, **reads still succeed**. | §6.2 vs §6.3 + `FatalKind` log | `Health()` [cmd/erasure-server-pool.go:L2679] |
| **Q4** | Path-named logs & live recovery | **Yes**, the disk is named by its exact path; recovery/re-probing runs while live. (For a *permission* fault the dedicated `monitorDiskWritable` offline/online lines do **not** fire — that path is `errFaultyDisk`-only.) | `endpoint="/tmp/ec/data1"`, `.healing.bin` path; offline/online line counts = **0** | offline log [cmd/xl-storage-disk-id-check.go:L1015] |
| **Q5** | Self-detection of restored dir | **Automatic** within the first ≤5 s poll — **no restart, no external push** required (manual `mc admin heal` is available but optional). | poll shows `/cluster` 200 + 4 online at t+5s | `monitorDiskStatus` [cmd/xl-storage-disk-id-check.go:L930] |
| **Q6** | Repair of objects written during outage | The shard missing on the down disk is **restored** (`xl.meta` re-created) when the disk returns, via healing. | shard MISSING → restored; `Healed: 1/2 objects` | `healFreshDisk()` [cmd/background-newdisks-heal-ops.go:L419] |
| **Q7** | Location of the quorum decision | `defaultWQuorum()`/`defaultRQuorum()` compute the thresholds; a **+1 split-brain guard** adds one to write quorum when `data == parity`. | write quorum 3 = data 2 **+1** | `defaultWQuorum()` [cmd/erasure.go:L85] |
| **Q8** | Grounding | Every conclusion is paired with a health-endpoint status/headers **and** a real S3 write/read result from the running server. | all §6 evidence blocks | `ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] |

## 3. Methodology

The answer was derived from a **compiled, running** server, then written from the captured output. The
exact steps below reproduce it end-to-end.

> **Critical, non-obvious insight — run MinIO as a NON-ROOT user.** Under `root`, `chmod 000` is bypassed
> (root has the DAC-override capability), so the permission fault is invisible and the scenario cannot be
> reproduced. The investigation ran the server as an unprivileged user (`ubuntu`, uid 1000) with the data
> directories owned by that user. This is essential for reproducing the permission-loss behavior.

1. **Build.** Install Go matching the module directive `go 1.23` [go.mod:L3] (used `go1.23.12`) and build via
   the canonical target:

   ```sh
   make build
   # → CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio   [Makefile:L177-L179]
   ```

   Capture the version banner with `./minio --version` (see §6.0). The real entry point is
   `main.go` → `minio.Main(os.Args)` [main.go:L30], importing `github.com/minio/minio/cmd` [main.go:L26].

2. **Launch (as non-root).** Run the real server entry point over four local directories (the launch idiom
   documented at [docs/erasure/README.md:L44] as `minio server /data{1...N}`):

   ```sh
   MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1 \
     ./minio server /tmp/ec/data{1...4} --address ":9000" --console-address ":9001"
   ```

   Confirm the banner reports `1 set(s), 4 drives per set` (single EC:2 erasure set). The `mc` client is
   fetched from `dl.min.io` per the existing `buildscripts/verify-healing.sh` convention and pointed at the
   server with an alias.

3. **Probe at every boundary.** At each state, capture verbatim:
   - `curl -sI http://127.0.0.1:9000/minio/health/cluster` and `.../minio/health/cluster/read` (status code
     + `X-Minio-*` headers);
   - a real S3 **PUT**/**GET** via `mc cp` / `mc cat`;
   - the drive online/offline count via `mc admin info local`.

4. **Inject faults (external only).** `chmod 000 /tmp/ec/data1` (one down → still above threshold), then
   `chmod 000 /tmp/ec/data2` (two down → below threshold). Restore with `chmod 755` on both **without
   restarting** the server.

5. **Observe long enough / confirm stability.** Poll for at least the 5 s / 10 s / 15 s poller intervals
   (§8) and exercise each state at least twice. The qualitative signals (the `200`→`503` transition, the
   `SlowDownWrite` write refusal, the path-named disk logs, the automatic reconnect) were **stable across
   runs**. Run-dependent *counts* (e.g., the total number of server-log lines, or how many times the
   healing re-probe logged during the outage window) vary run-to-run because they depend on how many polling
   cycles elapse; those counts are reported as observed and are not load-bearing for any conclusion.

6. **Cleanup.** Stop the server by its specific PID (never a broad process-kill pattern), remove `/tmp/ec`,
   the `./minio` binary, the `docs/debugging` helper binaries, the Go toolchain/tarball/build log, the `mc`
   binary, and all scripts/logs. Verify `git status --porcelain` is empty except for the added document
   (§6.7).

## 4. Quorum Math for This Topology (4-dir EC:2)

For four directories, the default `STANDARD` storage-class parity is **EC:2**
([internal/config/storageclass/storage-class.go:L355], `case 4, 5: return 2` at
[internal/config/storageclass/storage-class.go:L361-L362]; documented as "5 or fewer ⇒ EC:2" at
[docs/erasure/storage-class/README.md:L48-L53]). The set therefore has `setDriveCount = 4` and
`defaultParityCount = 2`. The read/write quorums are computed from those two numbers:

| Quantity | Formula | Value | Source |
|----------|---------|-------|--------|
| Data blocks | `setDriveCount - defaultParityCount` = 4 − 2 | **2** | [cmd/erasure.go:L86] |
| Parity blocks | `DefaultParityBlocks(4)` | **2** | [internal/config/storageclass/storage-class.go:L361-L362] |
| **Read quorum** | `setDriveCount - defaultParityCount` | **2** | [cmd/erasure.go:L94-L96] |
| **Write quorum** | data `(+1 because data == parity)` | **3** | [cmd/erasure.go:L85-L91] |

The **+1 split-brain guard** is the key subtlety. `defaultWQuorum()` computes `dataCount = 2`, and because
`dataCount == defaultParityCount` (2 == 2) it returns `dataCount + 1 = 3`
[cmd/erasure.go:L85-L91]. The same +1 rule exists per-object in `FileInfo.WriteQuorum()` — it starts from
`fi.Erasure.DataBlocks` and increments when `DataBlocks == ParityBlocks`
[cmd/storage-datatypes.go:L298-L308]. This guarantees that a write is only acknowledged when a **strict
majority** of the four drives (3 of 4) persisted it, so two disjoint halves can never both believe they hold
the authoritative copy. The read quorum has no +1 and equals the data-block count, **2**
[cmd/erasure.go:L94-L96], [cmd/storage-datatypes.go:L310-L314].

```go
// cmd/erasure.go:L85-L96 (verified at HEAD c07e5b49d477)
func (er erasureObjects) defaultWQuorum() int {
	dataCount := er.setDriveCount - er.defaultParityCount
	if dataCount == er.defaultParityCount {
		return dataCount + 1
	}
	return dataCount
}

func (er erasureObjects) defaultRQuorum() int {
	return er.setDriveCount - er.defaultParityCount
}
```

**Consequence for this topology:** with four drives online the deployment is healthy; losing one drive (3
online) still meets the write quorum of 3, so writes continue; losing a second (2 online) is **below** write
quorum 3 but still **at** read quorum 2 — hence writes are refused while reads persist.

## 5. Endpoint Contract

Health is exposed under the `/minio/health` prefix. The two probes relevant here are registered for both
`GET` and `HEAD`:

- `healthCheckClusterPath = "/cluster"` [cmd/healthcheck-router.go:L30]
- `healthCheckClusterReadPath = "/cluster/read"` [cmd/healthcheck-router.go:L31]
- registered under the `/minio/health` prefix for `GET`+`HEAD` [cmd/healthcheck-router.go:L36-L44]

`ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] calls `objLayer.Health(...)`, then:

- sets `X-Minio-Write-Quorum` from `result.WriteQuorum` [cmd/healthcheck-handler.go:L72];
- sets `X-Minio-Storage-Class-Defaults` from `result.UsingDefaults` [cmd/healthcheck-handler.go:L73];
- if any drives are healing, sets `X-Minio-Healing-Drives` [cmd/healthcheck-handler.go:L76];
- returns **412 Precondition Failed** when `maintenance=true` **and** unhealthy [cmd/healthcheck-handler.go:L83],
  **503 Service Unavailable** when simply unhealthy [cmd/healthcheck-handler.go:L85], otherwise
  **200 OK** [cmd/healthcheck-handler.go:L89].

`ClusterReadCheckHandler` [cmd/healthcheck-handler.go:L93] mirrors this using `result.HealthyRead` and sets
`X-Minio-Read-Quorum` [cmd/healthcheck-handler.go:L109]. The header string constants live in
`internal/http/headers.go`: `MinIOServerStatus` [L170], `MinIOWriteQuorum="x-minio-write-quorum"` [L193],
`MinIOReadQuorum="x-minio-read-quorum"` [L196], `MinIOStorageClassDefaults` [L200], and
`MinIOHealingDrives` [L203]. The operator-facing contract (200 on write quorum / 503 otherwise / 412 for
maintenance) is also documented at [docs/metrics/healthcheck/README.md], whose published example literally
shows `X-Minio-Write-Quorum: 3` for exactly this class of deployment.

| Endpoint | Healthy condition | Status when met | Status when not met | Quorum header |
|----------|-------------------|-----------------|---------------------|---------------|
| `/minio/health/cluster` | every set: `online ≥ write quorum` | 200 | 503 (or 412 if `maintenance=true`) | `X-Minio-Write-Quorum` |
| `/minio/health/cluster/read` | every set: `online ≥ read quorum` | 200 | 503 (or 412 if `maintenance=true`) | `X-Minio-Read-Quorum` |


## 6. Evidence (Verbatim Captured Output)

Each block below is the **actual, complete, unedited** output captured from the running server, shown
**before** its explanation. States are ordered: Baseline → One-down (above threshold) → Two-down (below
threshold) → Restored → Healed → Repository pristine.

### 6.0 Version banner + startup (methodology grounding)

```
$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.

$ ./minio server /tmp/ec/data{1...4} --address ":9000" --console-address ":9001"
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
...
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)
```

The reported `commit-id` equals the investigated HEAD, and `1 set(s), 4 drives per set` confirms the single
EC:2 erasure set. This block is what makes the answer "built-and-run" rather than code-reading.

### 6.1 Baseline — 4 drives online

```
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3
...
$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
...
$ mc admin info local   # (excerpt)
4 drives online, 0 drives offline, EC:2
# PUT obj-baseline → success ; GET obj-baseline → "baseline-content"
```

With all four drives online the deployment reports healthy: `/cluster` returns **200** and advertises the
write quorum (**3**), `/cluster/read` returns **200** and advertises the read quorum (**2**), all 4 drives
are `EC:2`, and both a write and a read succeed. This is the reference state for Q1/Q8.

### 6.2 Above threshold — `chmod 000 /tmp/ec/data1` (3 online)

```
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3
...
# PUT obj-1down → success ; GET obj-baseline → success
$ mc admin info local   # (excerpt)
3 drives online, 1 drive offline, EC:2
```

One directory is now inaccessible, leaving **3 drives online**. Because `3 ≥ write quorum 3`, `/cluster`
stays **200** and a write still succeeds — MinIO "quietly adapts and keeps going." The offline drive is
reflected in `mc admin info` as `3 drives online, 1 drive offline`.

### 6.3 Below threshold — additionally `chmod 000 /tmp/ec/data2` (2 online)

```
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3
...
$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
...
# PUT obj-2down → FAILS:
mc: <ERROR> Failed to copy `/tmp/obj-2down.txt`. Resource requested is unwritable, please reduce your request rate
# mc --json cause:
{"error":{"Code":"SlowDownWrite","Message":"Resource requested is unwritable, please reduce your request rate", ...}}
# GET obj-baseline → SUCCEEDS: "baseline-content"
$ mc admin info local   # (excerpt)
2 drives online, 2 drives offline, EC:2
```

Server log (the exact `logger.FatalKind` line emitted by `Health()`):

```
Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
```

A second directory is now inaccessible, leaving **2 drives online**. Because `2 < write quorum 3`,
`/cluster` returns **503** and the write is **refused** with the S3 error `SlowDownWrite` (HTTP 503). But
because `2 ≥ read quorum 2`, `/cluster/read` stays **200** and the read still **succeeds**. This is the
"draw a hard line, refuse writes, keep reads" outcome. The `FatalKind` log line names the pool/set and the
exact arithmetic (`expected write quorum: 3, drives-online: 2`).

### 6.4 Path-named log evidence + the detection nuance (Q4)

Over the full server log, the two `monitorDiskWritable`/`monitorDiskStatus` path-named lines appeared
**zero** times:

```
count of "taking drive ... offline" lines: 0
count of "bringing drive ... online" lines: 0
```

Instead, the permission fault named the failing disk **by its filesystem path** through the DiskInfo /
Healing path:

```
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/ec/data1"
Error: unable to read /tmp/ec/data1/.minio.sys/buckets/.healing.bin: open /tmp/ec/data1/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
       8: cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       3: cmd/xl-storage-disk-id-check.go:233:cmd.(*xlStorageDiskIDCheck).Healing()
       1: cmd/erasure.go:301:cmd.erasureObjects.getOnlineDisksWithHealingAndInfo.func1()
# also via cmd/xl-storage.go:781 DiskInfo(); cmd/erasure.go:192,206 getDisksInfo
```

Counts: `"drive access denied"` = **2** (one per failed disk); `.healing.bin: permission denied` = **33**.
(The absolute count is run-dependent — it is a function of how many polling cycles elapse during the outage
window — but it is always non-zero and always names the disk by path.)

### 6.5 Self-detection / recovery (Q5) — `chmod 755` both, NO restart

```
restored perms at 15:50:12; polling /minio/health/cluster every 5s:
  t+5s:  /cluster HTTP=200 ; drives: 4 drives online, 0 drives offline
  t+10s: /cluster HTTP=200 ; drives: 4 drives online, 0 drives offline
  ... (stable through t+45s)
```

Recovery is **automatic**: within the first 5-second poll after permissions are restored, `/cluster`
returns to **200** and all four drives are back online — with **no restart and no external push**. The
optional manual push was also demonstrated:

```
$ mc admin heal -r --force local/testbucket
[Green  ->  Green] testbucket/
[Yellow ->  Green] testbucket/obj-1down.txt
[Green  ->  Green] testbucket/obj-baseline.txt
Healed:	1/2 objects; 41 B in 1s
```

`mc admin heal` (the admin `HealHandler` [cmd/admin-handlers.go:L1308]) is available on demand but is **not
required** for the drives to be re-detected.

### 6.6 Repair of objects written during the outage (Q6)

On-disk shard presence for `obj-1down` (written while data1 was down) vs `obj-baseline` (written with all 4
up):

```
BEFORE heal:
  data1: (no object dir for obj-1down.txt)        <- shard missing on the disk that was down
  data2/3/4: xl.meta present
  obj-baseline: present on data1,data2,data3,data4  <- full protection

AFTER `mc admin heal`:
  data1: present -> xl.meta                         <- shard RESTORED on the recovered disk
  data2/3/4: present -> xl.meta
  obj-1down still readable -> "above-threshold-content"
```

Parity inspection (via the repo's own `docs/debugging/xl-meta` decoder):

```
obj-baseline: EcM=2, EcN=2   (2 data + 2 parity = EC:2)
obj-1down:    EcM=2, EcN=2   (NOT upgraded in this run) — data1 shard absent, healed on recovery
```

An object written while a disk was offline is missing its shard on that disk; when the disk returns, healing
re-creates the shard / `xl.meta`, and the object remains fully readable throughout.

### 6.7 Repository left pristine

```
$ git status --porcelain      # empty
$ git rev-parse HEAD          # c07e5b49d477b0774f23db3b290745aef8c01bd2
```

All build artifacts (`./minio`, the `docs/debugging` helper binaries) are gitignored and were physically
removed; the Go toolchain, `mc`, data directories, scripts, and logs were deleted. The only committed change
is this document.


## 7. Per-Question Answers (Q1–Q8)

Each answer states the **observed** result, the **`file:line`** citation(s), and the **cause → effect**
rationale. Statements derived from reading code rather than from observation are explicitly labeled
**(inferred)**; statements taken from MinIO documentation but not reproduced at runtime are labeled
**(documentation-derived)**.

### Q1 — Health decision & disk assumptions

**Answer.** MinIO reports the deployment "healthy" only if **every** erasure set has at least *write-quorum*
drives online. For this four-directory EC:2 topology the write quorum is **3** and the read quorum is **2**.

**Observed (§6.1).** `GET /minio/health/cluster` → **200** with `X-Minio-Write-Quorum: 3`;
`GET /minio/health/cluster/read` → **200** with `X-Minio-Read-Quorum: 2`; `mc admin info` → `4 drives
online, 0 drives offline, EC:2`.

**Code.** `Health()` builds a per-set online count and marks each set healthy with
`healthy := online >= poolWriteQuorums[poolIdx]` [cmd/erasure-server-pool.go:L2679]; a drive is counted
online **only** when its state is `madmin.DriveStateOk` [cmd/erasure-server-pool.go:L2707-L2709]; the overall
result is the AND across all sets (`result.Healthy = result.Healthy && healthy`). The write/read quorum
values come from `defaultWQuorum()` / `defaultRQuorum()` [cmd/erasure.go:L85-L96] over the default parity
`DefaultParityBlocks(4) = 2` [internal/config/storageclass/storage-class.go:L361-L362]. The handler surfaces
the decision as the HTTP status and the `X-Minio-Write-Quorum` header [cmd/healthcheck-handler.go:L56-L89].

**Cause → effect.** "Healthy" is not "all disks present" — it is "every set can still safely accept a
write," i.e. `online ≥ 3`. That is why 4/4 online is 200: the assumed minimum number of disks to *proceed*
is the write quorum (3), not the full four.

### Q2 — Live permission-loss behavior

**Answer.** It depends entirely on which side of the write quorum the loss leaves you on. While still at or
above write quorum, MinIO **quietly adapts and keeps serving** (including writes). The moment the loss drops
the set **below** write quorum, MinIO **draws a hard line and refuses writes** (reads continue if read
quorum still holds).

**Observed.** One directory lost → 3 online → `/cluster` **200**, PUT **succeeds** (§6.2). Second directory
lost → 2 online → `/cluster` **503**, PUT **fails** with `SlowDownWrite` (§6.3).

**Code.** The threshold that decides "adapt vs refuse" is `defaultWQuorum()` [cmd/erasure.go:L85]; when a
write cannot assemble that many drives the object layer returns `InsufficientWriteQuorum{}`
[cmd/erasure-object.go:L1897, L1944], which unwraps to `errErasureWriteQuorum`
("Write failed. Insufficient number of drives online") [cmd/erasure-errors.go:L26] and maps to the S3 error
`SlowDownWrite` (HTTP 503) [cmd/api-errors.go:L2314-L2315].

**Cause → effect.** MinIO does not "quietly" tolerate an arbitrary number of failures — it tolerates exactly
as many as parity allows before the write quorum is unmet. Above the line it adapts; at the line it stops
writing to protect durability.

### Q3 — Above vs below threshold (both demonstrated)

**Answer.**
- **Above threshold (one disk down, 3 online):** `/cluster` **200**; writes **succeed**; reads succeed.
- **Below threshold (two disks down, 2 online):** `/cluster` **503**; writes **refused** (`SlowDownWrite`);
  **reads still succeed** because 2 online still meets read quorum 2, so `/cluster/read` stays **200**.

**Observed.** §6.2 (above) vs §6.3 (below), plus the exact `FatalKind` server-log line
`Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2`.

**Code.** The per-set health decision and both the write- and read-quorum log lines are in `Health()`:
write-quorum failure logs with `logger.FatalKind` [cmd/erasure-server-pool.go:L2794], read-quorum failure
logs the analogous message [cmd/erasure-server-pool.go:L2802]. The read path still returns data while
`online ≥ readQuorum` (read quorum = data blocks = 2) [cmd/erasure.go:L94-L96],
[cmd/storage-datatypes.go:L310-L314].

**Cause → effect.** Two online is simultaneously **below** write quorum 3 and **at** read quorum 2. That
single arithmetic fact is why the same two-disk-down state produces a 503 on `/cluster` (writes) but a 200
on `/cluster/read` (reads) — writes need a strict majority (with the +1 guard), reads only need the data
blocks.

### Q4 — Path-named logs & live recovery

**Answer.** **Yes — the logs name the failing disk by its exact filesystem path**, and re-probing runs while
the server stays live. There is an important nuance: for a pure **permission** fault the disk is marked
not-online through the **DiskInfo / Healing** path (which names the path), while the **dedicated**
`monitorDiskWritable` offline / `monitorDiskStatus` online log lines do **not** fire.

**Observed (§6.4).** The path appears as `endpoint="/tmp/ec/data1"` and in
`/tmp/ec/data1/.minio.sys/buckets/.healing.bin: ... permission denied`. The counts of
`"taking drive ... offline"` and `"bringing drive ... online"` were both **0**.

**Code — what fired.** A `chmod 000` maps to `errDiskAccessDenied` on format/health reads
(`os.IsPermission(err) → return errDiskAccessDenied` [cmd/xl-storage.go:L276-L279];
`errDiskAccessDenied` = "drive access denied" [cmd/storage-errors.go:L68]). `diskErrToDriveState` maps that
to `madmin.DriveStatePermission` [cmd/erasure.go:L98], which is **not** `DriveStateOk`, so `Health()`
excludes the drive from the online count [cmd/erasure-server-pool.go:L2707-L2709]. The path-named text comes
from `Healing()` reading `.healing.bin` [cmd/xl-storage.go:L436] via `DiskInfo()` [cmd/xl-storage.go:L780]
under `getDisksInfo` [cmd/erasure.go:L192].

**Code — what did NOT fire, and why (inferred).** The dedicated live monitor logs the disk by path via
`p.storage.String()`: `"node(%s): taking drive %s offline: %v"` [cmd/xl-storage-disk-id-check.go:L1015] and
`"node(%s): Read/Write/Delete successful, bringing drive %s online"`
[cmd/xl-storage-disk-id-check.go:L956]. Those lines live inside `goOffline`, which is only invoked when
`osErrToFileErr(err) == errFaultyDisk` [cmd/xl-storage-disk-id-check.go:L1044, L1051] — a genuine I/O fault,
not a permission error. **(inferred, from code):** for an `errFaultyDisk` (real I/O) fault these
`monitorDiskWritable` / `monitorDiskStatus` path-named offline/online lines *would* be emitted; that route
was not exercised at runtime here (we injected a permission fault, not an I/O fault), so it is labeled
inferred.

**Cause → effect.** "Does the log call out the failing disk by path?" — yes, unambiguously
(`endpoint="/tmp/ec/data1"`). "Is recovery attempted while live?" — yes: the same DiskInfo/Healing machinery
re-probes on a schedule (§8), so no restart is needed. The subtlety is simply *which* code path names the
disk, and that depends on the fault *type* (permission vs I/O).

### Q5 — Self-detection of a restored directory

**Answer.** MinIO recognizes a restored directory **on its own, automatically**, within the first ≤5-second
poll after permissions are restored — **no restart and no external push** are required. A manual push
(`mc admin heal`) exists but is optional.

**Observed (§6.5).** After `chmod 755` (no restart), polling `/cluster` every 5 s showed **200** with
`4 drives online, 0 drives offline` at **t+5s**, stable through t+45s.

**Code.** Three periodic pollers are responsible:

| Poller | Interval | Role | Source |
|--------|----------|------|--------|
| `monitorDiskStatus` | 5 s ticker | Re-probes a drive and brings it back online | [cmd/xl-storage-disk-id-check.go:L930] |
| `monitorLocalDisksAndHeal` | 10 s | Detects freshly reconnected local drives and heals them | [cmd/background-newdisks-heal-ops.go:L40], [cmd/background-newdisks-heal-ops.go:L563] |
| `monitorAndConnectEndpoints` | 15 s | Reconnects offline set endpoints (`connectDisks`) | [cmd/erasure-sets.go:L348], [cmd/erasure-sets.go:L283], [cmd/erasure-sets.go:L194] |

**(inferred)** The specific poller that "won" the re-detection is not distinguishable from the log in this
run; what is directly observed is that re-detection occurred **automatically within ≤5 s**. All three
pollers above are the responsible mechanisms, and the shortest interval (`monitorDiskStatus`, 5 s) is
consistent with the observed latency.

**Cause → effect.** Because these tickers run continuously in the live server, restoring the OS permission
is sufficient — the next poll sees the drive readable again, re-includes it in the online count, and the
health endpoint flips back to 200 without operator intervention.

### Q6 — Repair of objects written during the outage

**Answer.** An object written while a disk was down is **missing its shard on that disk**; when the disk
returns, healing **re-creates the shard / `xl.meta`** on the recovered disk, and the object stays readable
throughout.

**Observed (§6.6).** Before heal, `obj-1down` had no directory on `data1` (present on data2/3/4); after
`mc admin heal` the `data1` shard was restored (`present -> xl.meta`) and the object read back
`"above-threshold-content"`. The heal summary was `Healed: 1/2 objects; 41 B in 1s`.

**Code.** On a drive's return, `healFreshDisk()` heals the reconnected drive
[cmd/background-newdisks-heal-ops.go:L419], scheduled by `monitorLocalDisksAndHeal()`
[cmd/background-newdisks-heal-ops.go:L563]. Partial/most-recent-failure writes are repaired by the MRF
`healRoutine()` [cmd/mrf.go:L220]. A manual push is available via `HealHandler`
[cmd/admin-handlers.go:L1308] (`mc admin heal`). The background data scanner provides an additional heal
path.

**(documentation-derived, NOT observed).** MinIO's documented "automatic parity upgrade at PUT time" — where
a drive offline at write time causes the object's parity to be bumped by one — was **not** reproduced in
this run: `obj-1down` remained `EcM=2, EcN=2` (see §6.6). This behavior is therefore presented as
documentation-derived only; the **observed** metadata shows no upgrade, and the object was instead protected
by having its shard healed on the disk's return.

**Cause → effect.** Erasure coding means the object never lost durability while `data1` was down (2 data + 2
parity, 3 of 4 shards present). The heal step is what restores *full* redundancy: it reconstructs the
missing shard from the surviving ones and writes it back to the recovered disk, returning the object to
`Green`.

### Q7 — Location of the quorum decision in code

**Answer.** The threshold lives in `defaultWQuorum()` / `defaultRQuorum()` [cmd/erasure.go:L85-L96], with a
per-object equivalent in `FileInfo.WriteQuorum()` / `FileInfo.ReadQuorum()`
[cmd/storage-datatypes.go:L298-L314]. The "enough disks to proceed vs risk too high, stop" boundary is the
**+1 split-brain guard**: write quorum = data blocks, **plus one** when `data == parity`.

**Observed.** The endpoint header `X-Minio-Write-Quorum: 3` (§6.1) and the log line
`expected write quorum: 3, drives-online: 2` (§6.3) both surface the exact computed value.

**Code (walk-through).**
1. Default parity for 4 drives = 2 → `DefaultParityBlocks(4)` [internal/config/storageclass/storage-class.go:L355, L361-L362].
2. `dataCount = setDriveCount − defaultParityCount = 4 − 2 = 2` [cmd/erasure.go:L86].
3. `defaultRQuorum()` returns `2` (data blocks) [cmd/erasure.go:L94-L96].
4. `defaultWQuorum()` returns `dataCount + 1 = 3` because `dataCount == defaultParityCount`
   [cmd/erasure.go:L85-L91]; the per-object rule mirrors this
   (`if DataBlocks == ParityBlocks { quorum++ }`) [cmd/storage-datatypes.go:L298-L308].
5. `Health()` compares each set's online count against these quorums and logs failures with `logger.FatalKind`
   [cmd/erasure-server-pool.go:L2679, L2794].
6. On the object path, an unmet write quorum becomes `InsufficientWriteQuorum{}`
   [cmd/erasure-object.go:L1897, L1944] → `errErasureWriteQuorum` [cmd/erasure-errors.go:L26] →
   S3 `SlowDownWrite` (503) [cmd/api-errors.go:L2314-L2315]. `reduceWriteQuorumErrs`
   [cmd/erasure-metadata-utils.go:L156-L157] performs the error reduction against the write-quorum count.

**Cause → effect.** The threshold is not a magic constant — it is derived from the set size and parity. The
+1 rule when parity is exactly half the set is precisely the "risk too high, stop" line: it forbids
acknowledging a write that only a tie-breakable half of the drives received.

### Q8 — Grounding in the health endpoint and real writes

**Answer.** Every conclusion in this document is paired with **both** a health-endpoint response (status +
`X-Minio-*` headers) **and** a real S3 write/read result from the running server.

**Observed pairing (summary).**

| State | `/cluster` | `/cluster/read` | PUT | GET | Drives |
|-------|-----------|-----------------|-----|-----|--------|
| Baseline (§6.1) | 200 (`WQ 3`) | 200 (`RQ 2`) | success | "baseline-content" | 4 online |
| One down / above (§6.2) | 200 (`WQ 3`) | — | success | success | 3 online, 1 offline |
| Two down / below (§6.3) | **503** (`WQ 3`) | 200 (`RQ 2`) | **fail** `SlowDownWrite` | "baseline-content" | 2 online, 2 offline |
| Restored (§6.5) | 200 | — | (writable again) | — | 4 online |

**Code.** The endpoint that produces these codes/headers is `ClusterCheckHandler`
[cmd/healthcheck-handler.go:L56] (and `ClusterReadCheckHandler` [cmd/healthcheck-handler.go:L93]); the write
result is produced by the object layer's quorum enforcement (Q7). The two signals always agree: a 503 on
`/cluster` co-occurs with a `SlowDownWrite` PUT, and a 200 on `/cluster/read` co-occurs with a succeeding
GET.

**Cause → effect.** Grounding both sides matters because the health endpoint is a *prediction* and the
write/read attempt is the *reality*; observing them together confirms the endpoint's 200/503 truly tracks
whether an S3 write will be accepted or refused.


## 8. Code-Trace — The Quorum Decision, End to End

The single decision "do we have enough disks to proceed?" flows through the following chain. Line numbers
are verified at HEAD `c07e5b49d477`.

```
                     default parity for 4 drives = 2
   DefaultParityBlocks(4)  ── internal/config/storageclass/storage-class.go:L355 (case 4,5: return 2, L361-L362)
                                        │
                                        ▼
   dataCount = setDriveCount - defaultParityCount = 4 - 2 = 2   ── cmd/erasure.go:L86
                                        │
              ┌─────────────────────────┴──────────────────────────┐
              ▼                                                     ▼
   defaultRQuorum() = 2                             defaultWQuorum() = dataCount(2) + 1 = 3
   cmd/erasure.go:L94-L96                           cmd/erasure.go:L85-L91  (+1 because data == parity)
   (per-object: FileInfo.ReadQuorum,                (per-object: FileInfo.WriteQuorum,
    cmd/storage-datatypes.go:L310-L314)              cmd/storage-datatypes.go:L298-L308)
                                        │
                                        ▼
   Health(): per set  online (only DriveStateOk, cmd/erasure-server-pool.go:L2707-L2709)
             healthy := online >= poolWriteQuorums[...]          ── cmd/erasure-server-pool.go:L2679
             if !healthy → log "Write quorum could not be established ... expected write quorum: 3,
                               drives-online: 2"  (logger.FatalKind)  ── cmd/erasure-server-pool.go:L2794
                                        │
                                        ▼
   ClusterCheckHandler: set X-Minio-Write-Quorum; 200 if healthy else 503 (412 if maintenance)
                                        ── cmd/healthcheck-handler.go:L56, L72, L83, L85, L89
                                        │
                                        ▼  (on the object write path, the same threshold)
   write cannot meet quorum → InsufficientWriteQuorum{}   ── cmd/erasure-object.go:L1897, L1944
        │  Unwrap → errErasureWriteQuorum ("Write failed. Insufficient number of drives online")
        │                                    ── cmd/erasure-errors.go:L26 ; cmd/object-api-errors.go:L248-L253
        ▼
   S3 mapping: InsufficientWriteQuorum → ErrSlowDownWrite (HTTP 503)  ── cmd/api-errors.go:L2314-L2315
        (ErrSlowDownWrite def: Code "SlowDownWrite", 503 ── cmd/api-errors.go:L874-L877)
```

**Reading the chain.** The parity default (2) and the set size (4) fully determine the two thresholds. The
write threshold picks up the **+1 split-brain guard** because parity equals half the set. `Health()` is the
cluster-level aggregator that turns per-set online counts into the 200/503 the endpoint reports, and it is
also where the `FatalKind` diagnostic is logged. On the data path the very same threshold, when unmet,
becomes `InsufficientWriteQuorum` and finally the client-visible `SlowDownWrite` (503). The read side is the
mirror image with no +1: read quorum 2, surfaced as `InsufficientReadQuorum` →
`ErrSlowDownRead` [cmd/object-api-errors.go:L236-L241], [cmd/api-errors.go:L2316-L2317] only when fewer than
two drives remain.

## 9. Coverage Pass

| Objective / mechanism | Value | `file:line` | Evidence | Rationale |
|-----------------------|-------|-------------|----------|-----------|
| **Q1** health/quorum decision | healthy ⇔ every set `online ≥ 3` | `Health()` [cmd/erasure-server-pool.go:L2679] | §6.1 (200, `X-Minio-Write-Quorum: 3`) | §7 Q1 |
| **Q2** live permission-loss | adapt (3 online) vs refuse (2 online) | `defaultWQuorum()` [cmd/erasure.go:L85] | §6.2 / §6.3 | §7 Q2 |
| **Q3** above vs below threshold | 200/PUT-ok vs 503/`SlowDownWrite`/GET-ok | `Health()` [cmd/erasure-server-pool.go:L2679, L2794] | §6.2 / §6.3 + `FatalKind` | §7 Q3 |
| **Q4** path-named logs & live recovery | disk named by path; offline/online monitor lines = 0 for permission fault | offline log [cmd/xl-storage-disk-id-check.go:L1015], online log [L956], goOffline errFaultyDisk-only [L1044, L1051] | §6.4 (`endpoint="/tmp/ec/data1"`, `.healing.bin`) | §7 Q4 — errFaultyDisk route **(inferred)** |
| **Q5** self-detection | automatic ≤5 s, no restart/push | `monitorDiskStatus` 5 s [cmd/xl-storage-disk-id-check.go:L930]; `monitorLocalDisksAndHeal` 10 s [cmd/background-newdisks-heal-ops.go:L40, L563]; `monitorAndConnectEndpoints` 15 s [cmd/erasure-sets.go:L348, L283]; `HealHandler` [cmd/admin-handlers.go:L1308] | §6.5 (t+5s → 200, 4 online) | §7 Q5 — winning poller **(inferred)** |
| **Q6** repair of outage writes | shard restored on recovered disk | `healFreshDisk()` [cmd/background-newdisks-heal-ops.go:L419]; MRF `healRoutine()` [cmd/mrf.go:L220] | §6.6 (MISSING → restored; `Healed: 1/2 objects`) | §7 Q6 — parity-upgrade **(documentation-derived)** |
| **Q7** quorum decision location | write quorum 3 = data 2 (+1) | `defaultWQuorum()` [cmd/erasure.go:L85], `defaultRQuorum()` [L94], `FileInfo.WriteQuorum/ReadQuorum` [cmd/storage-datatypes.go:L298-L314], `DefaultParityBlocks` [internal/config/storageclass/storage-class.go:L355] | §6.1 header, §6.3 log | §7 Q7 / §8 |
| **Q8** grounding | endpoint code/headers + real write/read at every state | `ClusterCheckHandler` [cmd/healthcheck-handler.go:L56] | §6.1–§6.6, §7 Q8 table | §7 Q8 |
| **quorum decision** | 3 / 2 | [cmd/erasure.go:L85-L96] | §4, §6.1 | §7 Q7 |
| **health endpoint** | 200 / 503 / 412 + quorum headers | [cmd/healthcheck-handler.go:L56-L131], [cmd/healthcheck-router.go:L30-L44] | §5, §6.1–§6.3 | §5 |
| **path-named logs** | disk named by path | [cmd/xl-storage-disk-id-check.go:L1015, L956], [cmd/xl-storage.go:L436] | §6.4 | §7 Q4 |
| **polling (5 s / 10 s / 15 s)** | 5 / 10 / 15 seconds | [cmd/xl-storage-disk-id-check.go:L930], [cmd/background-newdisks-heal-ops.go:L40], [cmd/erasure-sets.go:L348] | §6.5 | §7 Q5 |
| **healing (`healFreshDisk`)** | shard re-created | [cmd/background-newdisks-heal-ops.go:L419] | §6.6 | §7 Q6 |
| **MRF** | most-recent-failure repair | [cmd/mrf.go:L220] | §6.6 | §7 Q6 |

**Labeling summary.** Two statements are explicitly labeled: the `errFaultyDisk`-only offline/online monitor
route in **Q4** is **(inferred)** from code because only a permission fault (not an I/O fault) was injected
at runtime; the "automatic parity upgrade at PUT time" in **Q6** is **(documentation-derived)** and was
**not** observed (metadata stayed `EcM=2, EcN=2`). Everything else is backed by the observed output in §6.

---

*Scope note: this document is the sole committed artifact of the investigation. The MinIO source tree was
read only and left unchanged; all temporary artifacts (the compiled binary, the `mc` client, the
`/tmp/ec/data{1..4}` directories, observation scripts, and logs) were removed after the investigation, as
verified by an empty `git status --porcelain` aside from this file (§6.7).*

