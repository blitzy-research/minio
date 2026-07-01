# How MinIO Decides It Is "Healthy": Quorum, Disk Loss, and Recovery in a Four‑Drive Erasure Deployment

This document answers, with evidence, how a MinIO server running in erasure‑coded mode over four
directories decides that it is "healthy," what it assumes about the required number of drives, and how it
behaves as drives are lost to a permission change and later recovered. Every behavioral claim below is
paired with a **verbatim line** captured from a live server and a precise **`file:line`** citation into the
MinIO source in this checkout. Where an observed value differs from what one might expect, it is reported
exactly as observed.

## Runtime substrate (how these observations were produced)

- **System under observation.** MinIO server built from source at commit `c07e5b49d`, reporting its version at
  startup as `DEVELOPMENT.GOGET (go1.23.12 linux/amd64)`. The authoritative toolchain constraint in the
  repository is `go 1.23` (`go.mod:L3`); this environment's build used the highest available `1.23.x` patch,
  `go1.23.12`, which is what the binary reports and what is quoted below (reported exactly as observed rather
  than as a fixed patch number).
- **Topology, and why one node is a faithful substrate.** The server was launched over four directories,
  which it formats as exactly **one erasure set of four drives**:

  ```text
  INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
  Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
  ```

  A four‑drive set defaults to `EC:2` (two data + two parity), which — as derived in Q7 — yields a **read
  quorum of 2** and a **write quorum of 3**: the exact thresholds these questions ask about. Quorum, health,
  and healing in MinIO are evaluated *per erasure set*, so a single‑node four‑drive set exercises the same
  code paths as a multi‑host cluster. The design doc states this directly:

  > "Write and Read quorum are required to be satisfied only across the erasure set for an object. Healing is
  > also done per object within the erasure set which contains the object." (`docs/distributed/DESIGN.md:L99`)

- **Non‑root execution is mandatory.** The permission scenarios only work if the MinIO process cannot bypass
  filesystem mode bits. A root process ignores `chmod 000`, so the server was run as the unprivileged
  `nobody` user (uid 65534). The launch command was:

  ```bash
  setpriv --reuid=65534 --regid=65534 --clear-groups \
    env HOME=/tmp/minio-home MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    /tmp/minio-bin/minio server \
      /tmp/minio-data/disk1 /tmp/minio-data/disk2 /tmp/minio-data/disk3 /tmp/minio-data/disk4 \
      --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
  ```

  That the mode change genuinely denies the server was verified directly: as `nobody`, the drive directory is
  unreadable after `chmod 000`:

  ```text
  $ setpriv --reuid=65534 --regid=65534 --clear-groups ls /tmp/minio-data/disk4
  ls: cannot open directory '/tmp/minio-data/disk4': Permission denied
  ```

- **Tooling.** `curl` was used to read health‑endpoint status lines and headers; the `mc` client
  (`RELEASE.2025-08-13T08-35-41Z`) was used for object `PUT`/`GET` and `mc admin heal`; `mc cp --json`
  captured the exact S3 error `Code` on a failed write.

All temporary artifacts (the built binary, the `mc` client, four scratch directories, and observation
scripts) lived under `/tmp`, outside the repository, and were removed afterward; this document is the only
file added.

---

## Q1 — How does MinIO decide everything is "healthy," and what does it assume about the number of drives?

**Answer.** With all four drives online, MinIO reports healthy because a single decision function,
`erasureServerPools.Health()`, counts how many drives are in the OK state and requires that count to be at
least the per‑set **write quorum** (for the write/`cluster` verdict) and at least the **read quorum** (for the
`cluster/read` verdict). For four drives it assumes the `EC:2` layout — two data and two parity — so it needs
**3 drives online to accept writes** and **2 drives online to serve reads**.

**Observed (baseline, all four drives online):**

```text
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Storage-Class-Defaults: false
X-Minio-Write-Quorum: 3

$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
X-Minio-Storage-Class-Defaults: false
```

The two liveness/readiness probes were also `200`:

```text
$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:9000/minio/health/live
HTTP 200
$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:9000/minio/health/ready
HTTP 200
```

**Why (grounded in code):**

- The health verdict is computed in `Health()` — `func (z *erasureServerPools) Health(...)`
  (`cmd/erasure-server-pool.go:L2679`). A drive is counted as online only when its state is OK:
  `if disk.State == madmin.DriveStateOk { ... si.online++ }` (`cmd/erasure-server-pool.go:L2707`).
- The per‑set thresholds are derived from the standard‑storage‑class data count. Read quorum is the data
  count and write quorum starts at the data count: `poolReadQuorums[i] = data` and
  `poolWriteQuorums[i] = data` (`cmd/erasure-server-pool.go:L2723-L2724`); write quorum is then bumped by one
  when parity equals the data count (the `EC:2`, four‑drive case): `if data == b.StandardSCParity {
  poolWriteQuorums[i] = data + 1 }` (`cmd/erasure-server-pool.go:L2725-L2726`).
- The healthy/unhealthy booleans are pure threshold comparisons per set:
  `healthy := ...online >= poolWriteQuorums[poolIdx]` (`cmd/erasure-server-pool.go:L2791`) and
  `healthyRead := ...online >= poolReadQuorums[poolIdx]` (`cmd/erasure-server-pool.go:L2799`), combined into
  `result.Healthy` (`cmd/erasure-server-pool.go:L2797`) and `result.HealthyRead`
  (`cmd/erasure-server-pool.go:L2805`).
- The concrete numbers `3` and `2` reach the wire as headers set by the handlers:
  `w.Header().Set(xhttp.MinIOWriteQuorum, strconv.Itoa(result.WriteQuorum))`
  (`cmd/healthcheck-handler.go:L72`) and `w.Header().Set(xhttp.MinIOReadQuorum, strconv.Itoa(result.ReadQuorum))`
  (`cmd/healthcheck-handler.go:L109`). The header names are declared as `MinIOWriteQuorum =
  "x-minio-write-quorum"` (`internal/http/headers.go:L193`) and `MinIOReadQuorum = "x-minio-read-quorum"`
  (`internal/http/headers.go:L196`); Go canonicalizes them on the wire to `X-Minio-Write-Quorum` /
  `X-Minio-Read-Quorum`. The `X-Minio-Storage-Class-Defaults: false` header comes from
  `MinIOStorageClassDefaults = "x-minio-storage-class-defaults"` (`internal/http/headers.go:L200`).
- The assumption that four drives means two parity comes from `func DefaultParityBlocks(drive int) int`
  (`internal/config/storageclass/storage-class.go:L355`), whose `case 4, 5:` returns `2`
  (`internal/config/storageclass/storage-class.go:L361`). With four drives that fixes **data = 2, parity = 2**.

So "healthy" is not a heuristic: it is the boolean `online >= writeQuorum` (and `online >= readQuorum`)
evaluated for the erasure set, with `writeQuorum = 3` and `readQuorum = 2` for this four‑drive `EC:2`
deployment — exactly the values echoed back in the `X-Minio-Write-Quorum: 3` and `X-Minio-Read-Quorum: 2`
headers above.

---

## Q2 — One directory suddenly becomes inaccessible: quietly adapt, or draw a hard line?

**Answer.** MinIO draws a **hard line for the affected drive** — it marks that drive as not‑OK and stops
counting it toward quorum — but it does **not** refuse service as long as the remaining online drives still
meet quorum. In other words: a hard line for the *drive*, graceful continuation for the *cluster* (while it
can). A permission failure is classified specifically as "drive access denied," not as a transient hiccup.

**Observed.** Immediately after `chmod 000 /tmp/minio-data/disk4` (one drive lost, three still online), the
server emitted an error that names the failure kind and the drive:

```text
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/minio-data/disk4"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
```

At the same time the cluster stayed writable — health remained `200` and a `PUT` succeeded (shown in Q3) —
demonstrating that the "hard line" is scoped to the drive, not the whole server.

**Why (grounded in code):**

- The permission error is mapped deterministically: when the storage layer opens the drive and the OS returns
  a permission error, MinIO converts it to a specific sentinel — `if os.IsPermission(err) { return s,
  errDiskAccessDenied }` (`cmd/xl-storage.go:L276-L277`).
- That sentinel is `errDiskAccessDenied = StorageErr("drive access denied")`
  (`cmd/storage-errors.go:L68`) — the exact string `drive access denied` seen in the log above. It is one of a
  small taxonomy of drive errors MinIO distinguishes, alongside
  `errUnformattedDisk = StorageErr("unformatted drive found")` (`cmd/storage-errors.go:L38`),
  `errDiskNotFound = StorageErr("drive not found")` (`cmd/storage-errors.go:L53`), and
  `errFaultyDisk = StorageErr("drive is faulty")` (`cmd/storage-errors.go:L65`). A permission change is thus
  reported as *access denied*, distinct from a missing or unformatted or faulty drive.
- Because the drive is not in the OK state, `Health()` simply does not increment `online` for it
  (`cmd/erasure-server-pool.go:L2707`), which is precisely how one lost drive lowers the online count without
  taking the cluster down — provided the survivors still satisfy quorum.

---

## Q3 — Show both scenarios: still above the threshold (one disk lost) vs. dropping below it (a second disk lost)

**Answer.** With `EC:2`, the write threshold is **3 online drives** and the read threshold is **2**. Losing
one drive (3 online) stays *above* the write threshold: writes and reads both succeed and health is `200`.
Losing a second drive (2 online) drops *below* the write threshold but still meets the read threshold: writes
are refused with HTTP `503` and the S3 error `SlowDownWrite`, while reads keep succeeding with `200`.

| Aspect | Scenario A — one drive lost (3 online) | Scenario B — second drive lost (2 online) |
|---|---|---|
| Online vs. write quorum (3) | `3 >= 3` — met | `2 >= 3` — **not met** |
| Online vs. read quorum (2) | `3 >= 2` — met | `2 >= 2` — met |
| `GET /minio/health/cluster` | `HTTP/1.1 200 OK` | `HTTP/1.1 503 Service Unavailable` |
| `GET /minio/health/cluster/read` | `HTTP/1.1 200 OK` | `HTTP/1.1 200 OK` |
| Write (`mc cp`) | **succeeds** | **fails** — `SlowDownWrite` |
| Read (`mc cat`) | succeeds | succeeds |

**Observed — Scenario A (after `chmod 000 /tmp/minio-data/disk4`, 3 online == write quorum 3):**

```text
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 200 OK
X-Minio-Write-Quorum: 3

$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
```

The write succeeded and the object appears in the listing:

```text
$ mc cp /tmp/objA.txt local/testbucket/objA.txt   # PUT objA: SUCCEEDED
$ mc ls local/testbucket/
[... UTC]    31B STANDARD obj1.txt
[... UTC]    36B STANDARD objA.txt
```

**Observed — Scenario B (after also `chmod 000 /tmp/minio-data/disk3`, 2 online < write quorum 3):**

```text
$ curl -sI http://127.0.0.1:9000/minio/health/cluster
HTTP/1.1 503 Service Unavailable
X-Minio-Write-Quorum: 3

$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read
HTTP/1.1 200 OK
X-Minio-Read-Quorum: 2
```

The `PUT` was refused; `mc cp --json` reported the exact S3 error `Code`:

```json
{"status":"error","error":{"message":"Failed to copy `/tmp/objB.txt`.","cause":{"message":"Resource requested is unwritable, please reduce your request rate","error":{"Code":"SlowDownWrite","Message":"Resource requested is unwritable, please reduce your request rate","BucketName":"testbucket","Key":"objB.txt"}}}}
```

A `GET` of an object written earlier still succeeded, because read quorum (2) was met:

```text
$ mc cat local/testbucket/obj1.txt
hello-minio-baseline-1782937283
```

**Why (grounded in code):**

- The write threshold of `3` is computed as *data blocks, plus one when parity equals the data count*. In the
  per‑object quorum function `objectQuorumFromMeta(...)` (`cmd/erasure-metadata.go:L531`):

  ```go
  writeQuorum := dataBlocks
  if dataBlocks == parityBlocks {
      writeQuorum++
  }
  ```

  (`cmd/erasure-metadata.go:L557-L559`). With `dataBlocks == parityBlocks == 2`, `writeQuorum` becomes `3`,
  while read quorum stays at the data count (`2`). The write path computes the same value with the same rule,
  under the comment `// writeQuorum is dataBlocks + 1`:

  ```go
  writeQuorum := dataDrives
  if dataDrives == parityDrives {
      writeQuorum++
  }
  ```

  (`cmd/erasure-object.go:L1115-L1119`).
- Below write quorum, the object layer returns the write‑quorum failure
  `errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")`
  (`cmd/erasure-errors.go:L26`) — the read‑side analogue is
  `errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")`
  (`cmd/erasure-errors.go:L23`).
- That internal insufficiency is mapped to the client‑facing S3 error: `case InsufficientWriteQuorum: apiErr =
  ErrSlowDownWrite` (`cmd/api-errors.go:L2314-L2315`), whose definition is
  `Code: "SlowDownWrite"`, `Description: "Resource requested is unwritable, please reduce your request rate"`,
  `HTTPStatusCode: http.StatusServiceUnavailable` (`cmd/api-errors.go:L874-L877`) — matching the JSON body and
  the `503` verdict above.
- The health `503` itself is the same threshold crossing evaluated in `Health()`: the set is unhealthy for
  writes because `2 >= 3` is false (`cmd/erasure-server-pool.go:L2791`), and the handler turns an unhealthy
  result into `http.StatusServiceUnavailable` (`cmd/healthcheck-handler.go:L85`). Reads stay `200` because
  `2 >= 2` holds (`cmd/erasure-server-pool.go:L2799`) and the read handler returns `http.StatusOK`
  (`cmd/healthcheck-handler.go:L126`).

---

## Q4 — Do the logs call out the failing disk by path, and is recovery attempted while live?

**Answer.** Yes on both counts. Every disk error is tagged with the drive's endpoint, so the failing
directory is named **by its full path**. And while the server is still live, it repeatedly probes each drive's
healing state by trying to read that drive's `.healing.bin` marker — a recovery‑oriented check that keeps
running during the outage.

**Observed — the failing directory named by full path:**

```text
Error: drive access denied (cmd.StorageErr)
       endpoint="/tmp/minio-data/disk4"
```

**Observed — recovery/healing being probed while live** (the server keeps trying to read the per‑drive
healing marker, which is itself permission‑denied on the downed drive):

```text
Error: unable to read /tmp/minio-data/disk4/.minio.sys/buckets/.healing.bin: open /tmp/minio-data/disk4/.minio.sys/buckets/.healing.bin: permission denied (*fmt.wrapError)
       8: cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()
       ...
       1: cmd/erasure.go:301:cmd.erasureObjects.getOnlineDisksWithHealingAndInfo.func1()
```

**Why (grounded in code):**

- Log messages are tagged with the drive path by `printEndpointError`, which builds request‑info tags with the
  endpoint string: `reqInfo := (&logger.ReqInfo{}).AppendTags("endpoint", endpoint.String())`
  (`cmd/prepare-storage.go:L40`); the surrounding closure is declared at `cmd/prepare-storage.go:L35`. That is
  exactly why the log shows `endpoint="/tmp/minio-data/disk4"`. The stack trace above confirms the message
  originates from a call site in `cmd/erasure-sets.go` — the `printEndpointError(...)` calls during disk
  (re)connection are at `cmd/erasure-sets.go:L230`, `cmd/erasure-sets.go:L244`, and `cmd/erasure-sets.go:L254`
  (the observed frame is `cmd/erasure-sets.go:230`).
- The `.healing.bin` probe is the live recovery signal. It comes from `func (s *xlStorage) Healing()`
  (`cmd/xl-storage.go:L430`), which reads the drive's healing‑tracker file and logs `unable to read %s` on
  failure at `cmd/xl-storage.go:L436` — matching the captured frame `cmd/xl-storage.go:436:cmd.(*xlStorage).Healing()`.
  This runs as part of gathering online drives and their healing status while serving
  (`getOnlineDisksWithHealingAndInfo`, seen at `cmd/erasure.go:301` in the trace), i.e., the server is checking
  "is anyone healing?" continuously, live.
- If the same endpoint error keeps recurring, `printEndpointError` escalates with a de‑duplicating message —
  `fmt.Errorf("Following error has been printed %d times.. %w", ...)` (`cmd/prepare-storage.go:L63`) — so the
  logs remain readable rather than flooding.


---

## Q5 — When the directory becomes accessible again, does MinIO notice on its own, or must something push it?

**Answer.** MinIO notices on its **own**. After permissions were restored, the cluster health returned to
`200` without any manual action — the server re‑connects the drive through its periodic disk polling. There is
also a dedicated background loop, `monitorLocalDisksAndHeal`, that runs on a **10‑second** timer and heals
*fresh/replaced* drives. An important, reported‑as‑observed nuance: a drive that was merely **permission‑
restored keeps its existing format**, so it simply reconnects online; the fresh‑disk auto‑heal loop does not
necessarily fire for it (there is no format to heal). A manual push is available but not required —
`mc admin heal`.

**Observed.** After `chmod 755 /tmp/minio-data/disk3 /tmp/minio-data/disk4`, polling health every three
seconds showed it back at `200` on its own, and staying there:

```text
t+3s:  /minio/health/cluster = HTTP 200 (X-Minio-Write-Quorum: 3)
t+6s:  /minio/health/cluster = HTTP 200 (X-Minio-Write-Quorum: 3)
t+9s:  /minio/health/cluster = HTTP 200 (X-Minio-Write-Quorum: 3)
...
t+24s: /minio/health/cluster = HTTP 200 (X-Minio-Write-Quorum: 3)
```

No manual push was required to regain online status; the recovery to write quorum `3` was self‑detected.

**Why (grounded in code):**

- The background monitor's cadence is a named constant: `defaultMonitorNewDiskInterval = time.Second * 10`
  (`cmd/background-newdisks-heal-ops.go:L40`), i.e. **10s**. The loop itself is
  `func monitorLocalDisksAndHeal(...)` (`cmd/background-newdisks-heal-ops.go:L563`), started at
  `go monitorLocalDisksAndHeal(ctx, z)` (`cmd/background-newdisks-heal-ops.go:L386`); when it finds a fresh or
  replaced drive it runs format healing plus `func healFreshDisk(...)`
  (`cmd/background-newdisks-heal-ops.go:L419`).
- The nuance is exactly why the observed recovery was *reconnect, not full‑disk heal*: `healFreshDisk` and the
  monitor target drives that need **format** healing (a wiped/replaced disk). A `chmod`‑restored disk still has
  its `format.json` and object metadata, so it rejoins as an already‑formatted member of the set and the
  health count rises again without a full‑disk heal being triggered. Per‑object gaps left during the outage are
  closed separately (see Q6).
- The manual push path is `mc admin heal`, which enters the admin‑heal code (see Q6) and can be used to force
  healing eagerly rather than waiting for lazy repair.

---

## Q6 — For objects written while a disk was down, how do they get repaired once the disk returns?

**Answer.** Per object, not by a blanket disk rebuild. An object that was written while a drive was offline is
missing its shard on that drive; the shard is restored the next time the object is **read** ("heal‑on‑read"),
or by the background scanner, or by the MRF (Most‑Recent‑Failures) re‑heal routine, and it can also be forced
eagerly with `mc admin heal`.

**Observed.** `objA.txt` was written during Scenario A while `disk4` was down, so its metadata shard was
absent on `disk4` and present on the other three (shard presence measured by counting `xl.meta` per drive;
small objects < 128 KB are stored inline in `xl.meta`, with no separate part file):

```text
BEFORE recovery (objA.txt xl.meta count):  disk1: 1  disk2: 1  disk3: 1  disk4: 0
```

A single `GET` of the object triggered heal‑on‑read; within a few seconds the missing shard reappeared on
`disk4`:

```text
$ mc cat local/testbucket/objA.txt
this-is-objA-written-with-disk4-down
after GET objA.txt (heal-on-read) + 6s:  disk4 objA.txt xl.meta = 0 -> 1
```

Forcing an explicit heal afterward confirmed nothing was left to repair — heal‑on‑read had already closed the
gap:

```text
$ mc admin heal --recursive --force local/testbucket
[Green  ->  Green] testbucket/
[Green  ->  Green] testbucket/obj1.txt
[Green  ->  Green] testbucket/objA.txt
Healed: 0/2 objects; 67 B in 1s
```

**Why (grounded in code):**

- The heal‑on‑read / core per‑object heal is `func (er *erasureObjects) healObject(...)`
  (`cmd/erasure-healing.go:L258`); whether a given disk needs repair for the object is decided by
  `func shouldHealObjectOnDisk(...)` (`cmd/erasure-healing.go:L156`). The admin‑heal entry point
  `func (er erasureObjects) HealObject(...)` (`cmd/erasure-healing.go:L1039`) — reached via `mc admin heal` —
  calls the same core `healObject`, which is why both the lazy and the eager path converge on identical repair
  logic.
- The MRF re‑heal path records recent write failures and re‑heals them when drives reconnect: `type mrfState
  struct` (`cmd/mrf.go:L63`) and its `func (m *mrfState) healRoutine(...)` (`cmd/mrf.go:L220`).
- This per‑object model matches the design doc: "Healing is also done per object within the erasure set which
  contains the object." (`docs/distributed/DESIGN.md:L99`). The `Healed: 0/2` line is consistent with the
  disk4 shard already being restored on read — there was no remaining per‑object work for `mc admin heal` to do.

---

## Q7 — Where does the quorum decision live, and how is the threshold calculated?

**Answer.** The quorum decision is not in one place by accident — it is a short chain from a default‑parity
policy to a per‑object computation to write‑path enforcement to the cluster‑health verdict, and every link
uses the same arithmetic. For this four‑drive set the numbers are **data = 2, parity = 2, read quorum = 2,
write quorum = 3**. The write quorum is *data blocks + 1* specifically because parity equals exactly half the
set — the "`+1`" split‑brain rule.

**The chain, with the calculation at each link:**

1. **Default parity policy — how many parity blocks for N drives.**
   `func DefaultParityBlocks(drive int) int` (`internal/config/storageclass/storage-class.go:L355`); its
   `case 4, 5:` returns `2` (`internal/config/storageclass/storage-class.go:L361`). So a four‑drive set is
   `EC:2`: **data = 2, parity = 2**. The official table corroborates this — an erasure set of "5 or fewer"
   defaults to `EC:2` (`docs/erasure/storage-class/README.md:L52`), and "parity can not be higher than N/2"
   (`docs/erasure/storage-class/README.md:L46`).

2. **Per‑object quorum computation — the threshold formula.**
   `func objectQuorumFromMeta(...)` (`cmd/erasure-metadata.go:L531`) computes read quorum as the data‑block
   count and write quorum with the `+1` rule:

   ```go
   dataBlocks := len(partsMetaData) - parityBlocks

   writeQuorum := dataBlocks
   if dataBlocks == parityBlocks {
       writeQuorum++
   }
   ```

   (`cmd/erasure-metadata.go:L555-L559`). With `dataBlocks = 2` and `parityBlocks = 2`, read quorum = `2` and
   write quorum = `3`. (Parity itself is chosen by `commonParity`, `cmd/erasure-metadata.go:L461`.)

3. **Write‑path enforcement — the same formula guards writes.**
   The object write path recomputes the identical value under the comment `// writeQuorum is dataBlocks + 1`:

   ```go
   writeQuorum := dataDrives
   if dataDrives == parityDrives {
       writeQuorum++
   }
   ```

   (`cmd/erasure-object.go:L1115-L1119`); the same pattern recurs at `cmd/erasure-object.go:L1322-L1325` and
   `cmd/erasure-object.go:L2538-L2541`. If fewer than `writeQuorum` drives can accept the write, the write
   fails with `errErasureWriteQuorum` (`cmd/erasure-errors.go:L26`), surfaced to clients as `SlowDownWrite`
   (`cmd/api-errors.go:L2314-L2315`, `cmd/api-errors.go:L874-L877`).

4. **Cluster‑health verdict — the same threshold, at the server level.**
   `func (z *erasureServerPools) Health(...)` (`cmd/erasure-server-pool.go:L2679`) derives per‑pool quorums
   with the very same rule — `poolWriteQuorums[i] = data` then `if data == b.StandardSCParity {
   poolWriteQuorums[i] = data + 1 }` (`cmd/erasure-server-pool.go:L2724-L2726`) — counts OK drives
   (`cmd/erasure-server-pool.go:L2707`), and decides `healthy := online >= poolWriteQuorums[poolIdx]`
   (`cmd/erasure-server-pool.go:L2791`) / `healthyRead := online >= poolReadQuorums[poolIdx]`
   (`cmd/erasure-server-pool.go:L2799`). When write quorum is not met it logs the precise threshold and count:

   ```text
   Error: Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2 (*errors.errorString)
   ```

   emitted from `cmd/erasure-server-pool.go:L2794`.

**When does it decide the risk is too high and stop?** The moment the online count in a set falls below its
write quorum. In this deployment that is at **2 online drives** (since `2 >= 3` is false): writes are refused
and `/minio/health/cluster` reports `503`. Reads continue until the online count falls below the **read
quorum** of `2`. This is the observed threshold crossing between Scenario A (3 online → writable) and Scenario
B (2 online → writes refused).

**The `+1` split‑brain rule, explained.** When parity `M` equals exactly half the set size `N` (here
`M = 2`, `N = 4`), a plain "data‑block" write quorum of `2` would let two disjoint halves of the set each
believe they can accept writes during a partition — a split brain. Requiring `data + 1 = 3` guarantees any two
write‑accepting groups overlap on at least one drive, preventing divergent writes. This is exactly the
`if dataBlocks == parityBlocks { writeQuorum++ }` branch (`cmd/erasure-metadata.go:L557-L559`) and its twin in
the write path (`cmd/erasure-object.go:L1115-L1119`) and in `Health()`
(`cmd/erasure-server-pool.go:L2725-L2726`).

---

## Q8 — Grounding: what the health endpoint and real write/read attempts show

Every answer above is anchored to two kinds of live evidence gathered while the server ran: (1) the
**health‑endpoint status line and quorum headers**, and (2) the **result of actual `mc` write/read attempts**.
The raw grounding evidence, side by side (verbatim from the running server):

```text
# Baseline (all 4 drives online) — healthy for both write and read
$ curl -sI http://127.0.0.1:9000/minio/health/cluster       -> HTTP/1.1 200 OK   X-Minio-Write-Quorum: 3
$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read  -> HTTP/1.1 200 OK   X-Minio-Read-Quorum: 2

# Second drive lost (2 online < write quorum 3) — writes refused, reads still served
$ curl -sI http://127.0.0.1:9000/minio/health/cluster       -> HTTP/1.1 503 Service Unavailable
$ curl -sI http://127.0.0.1:9000/minio/health/cluster/read  -> HTTP/1.1 200 OK
$ mc cp objB.txt local/testbucket/objB.txt                  -> "Code":"SlowDownWrite"
$ mc cat local/testbucket/obj1.txt                          -> hello-minio-baseline-1782937283
```

Summarized:

- **Baseline / healthy (Q1).** `GET /minio/health/cluster` → `HTTP/1.1 200 OK` with `X-Minio-Write-Quorum: 3`;
  `GET /minio/health/cluster/read` → `HTTP/1.1 200 OK` with `X-Minio-Read-Quorum: 2`; a `PUT` succeeded and a
  `GET` returned `hello-minio-baseline-1782937283`.
- **One drive lost, still above threshold (Q2/Q3).** Health stayed `HTTP/1.1 200 OK` and a `PUT` of `objA.txt`
  (36B) succeeded; the log named the drive: `endpoint="/tmp/minio-data/disk4"` with `drive access denied`.
- **Second drive lost, below threshold (Q3).** `GET /minio/health/cluster` → `HTTP/1.1 503 Service
  Unavailable`; the `PUT` returned `"Code":"SlowDownWrite"` / `Resource requested is unwritable, please reduce
  your request rate`; but `GET /minio/health/cluster/read` stayed `HTTP/1.1 200 OK` and `mc cat` still returned
  `hello-minio-baseline-1782937283`. The health layer logged `Write quorum could not be established on pool: 0,
  set: 0, expected write quorum: 3, drives-online: 2`.
- **Recovery (Q5/Q6).** After restoring permissions, `/minio/health/cluster` returned to `HTTP 200` on its own
  (write quorum `3`), and the object written during the outage was repaired on read (`disk4` `xl.meta`
  `0 -> 1`), corroborated by `mc admin heal` reporting `[Green -> Green]` for every object.

Because each behavioral claim is paired with the exact observed status/header/error and the code location that
produces it, the explanation is grounded in what the running system does, not in what it "should" do.


---

## Coverage pass — every question and named mechanism, addressed by name

**Questions.**

| Question | Where answered | One anchor (observed + code) |
|---|---|---|
| Q1 — health decision + disk assumptions | Q1 | `200` + `X-Minio-Write-Quorum: 3` / `X-Minio-Read-Quorum: 2`; `Health()` `cmd/erasure-server-pool.go:L2679` |
| Q2 — single‑disk permission loss | Q2 | `drive access denied` + `endpoint="/tmp/minio-data/disk4"`; `cmd/xl-storage.go:L276-L277` |
| Q3 — above vs. below threshold | Q3 | Scenario A `200` / Scenario B `503` + `SlowDownWrite`; `cmd/erasure-metadata.go:L557-L559` |
| Q4 — by‑path logging + live recovery signal | Q4 | `endpoint="/tmp/minio-data/disk4"`, `.healing.bin` probe; `cmd/prepare-storage.go:L40`, `cmd/xl-storage.go:L436` |
| Q5 — recovery detection | Q5 | health `200` at `t+3s` unaided; `cmd/background-newdisks-heal-ops.go:L40, L563` |
| Q6 — object repair | Q6 | `disk4` `xl.meta` `0 -> 1`; `cmd/erasure-healing.go:L258` |
| Q7 — quorum location + math | Q7 | data=2/parity=2 → WQ 3/RQ 2; chain L361 → L531 → L1115 → L2679 |
| Q8 — grounding | Q8 | each claim paired with health status/header + `mc` result |

**Named mechanisms.**

| Mechanism (by name) | Addressed in | Citation / observed literal |
|---|---|---|
| `/minio/health/cluster`, `/minio/health/cluster/read`, `/minio/health/live`, `/minio/health/ready` | Q1, Q8 | routes `cmd/healthcheck-router.go:L41-L44` (cluster/read), `L47-L48` (live), `L51-L52` (ready) |
| `Health()` | Q1, Q3, Q7 | `cmd/erasure-server-pool.go:L2679`; `online >= writeQuorum` at `L2791` |
| `DefaultParityBlocks` | Q1, Q7 | `internal/config/storageclass/storage-class.go:L355`; `case 4, 5:` → `2` at `L361` |
| `objectQuorumFromMeta` | Q3, Q7 | `cmd/erasure-metadata.go:L531` |
| `writeQuorum++` (the `+1` rule) | Q3, Q7 | `cmd/erasure-metadata.go:L557-L559`; `cmd/erasure-object.go:L1115-L1119` |
| `X-Minio-Write-Quorum` / `X-Minio-Read-Quorum` / `X-Minio-Healing-Drives` | Q1, Q8 | `internal/http/headers.go:L193`, `L196`, `L203`; healing header set only when `HealingDrives > 0` at `cmd/healthcheck-handler.go:L75-L76` |
| `X-Minio-Storage-Class-Defaults` | Q1 | `internal/http/headers.go:L200`; observed `false` |
| `errDiskAccessDenied` | Q2 | `cmd/storage-errors.go:L68` — `"drive access denied"` |
| `errUnformattedDisk` / `errDiskNotFound` / `errFaultyDisk` | Q2 | `cmd/storage-errors.go:L38`, `L53`, `L65` |
| `os.IsPermission` | Q2 | `cmd/xl-storage.go:L276-L277` |
| `printEndpointError` | Q4 | `cmd/prepare-storage.go:L35`, `L40`; call sites `cmd/erasure-sets.go:L230, L244, L254` |
| `.healing.bin` / `Healing()` | Q4 | `cmd/xl-storage.go:L430`, log at `L436` |
| `monitorLocalDisksAndHeal` (+ `defaultMonitorNewDiskInterval`, `healFreshDisk`) | Q5 | `cmd/background-newdisks-heal-ops.go:L563`, `L40` (`time.Second * 10`), `L419` |
| `healObject` / `HealObject` / `shouldHealObjectOnDisk` | Q6 | `cmd/erasure-healing.go:L258`, `L1039`, `L156` |
| MRF `healRoutine` / `mrfState` | Q6 | `cmd/mrf.go:L220`, `L63` |
| `SlowDownWrite` / `InsufficientWriteQuorum` | Q3 | `cmd/api-errors.go:L874-L877`, `L2314-L2315` |
| `errErasureReadQuorum` / `errErasureWriteQuorum` | Q3 | `cmd/erasure-errors.go:L23`, `L26` |
| `mc admin heal` | Q5, Q6 | admin entry `HealObject` `cmd/erasure-healing.go:L1039`; observed `[Green -> Green]`, `Healed: 0/2 objects` |

**Reproducibility note.** Observed values are reported exactly as captured in this environment. The build
reported `go1.23.12` (a valid `1.23.x` patch of the `go 1.23` constraint at `go.mod:L3`); the baseline object
`obj1.txt` was 31B; and `mc admin heal` reported `67 B` — these are the measured values from this run rather
than fixed constants.

