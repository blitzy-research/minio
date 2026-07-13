# MinIO Erasure-Coded Storage Under Drive Failure and Recovery — An Evidence-Based Analysis

## Introduction

This document answers six questions about how MinIO's erasure-coding storage layer behaves when a drive fails while data is being written, when a drive is missing for pre-existing reads, and when a drive is brought back and healed. Every answer is grounded in runtime output captured from a MinIO server **built and run from the exact source commit under analysis**, `c07e5b49d477b0774f23db3b290745aef8c01bd2`, and driven through its real S3 and admin entry points.

Each objective section gives (1) the direct answer, (2) the `file:line` code reference naming the specific function/struct, (3) the exact command(s) used, (4) the complete, unedited captured output, and (5) the cause-and-effect reasoning. All analysis text is kept **outside** the fenced code blocks; every fenced block contains only genuine captured output. Claims that could not be reproduced canonically against this commit are labeled **INFERRED** or **SOURCE-ONLY**; observations obtained through a non-default trigger are labeled **SUPPLEMENTARY / NON-CANONICAL**.

The six questions:

- **OBJ-1** — If a drive becomes unavailable while data is actively being written, what error does MinIO return, and does the write succeed or fail?
- **OBJ-2** — For objects already stored before a drive vanished, can they still be read? If not, what error is returned?
- **OBJ-3** — What event or condition triggers a healing operation when a drive comes back online after being offline?
- **OBJ-4** — What criteria does the system use to decide that a specific object needs healing on a particular drive?
- **OBJ-5** — What log messages appear during an active healing operation?
- **OBJ-6** — What are the metric names that track online vs. offline drive counts, and what values do they report before and after a drive failure?

## Environment & Build Appendix

All investigation work was performed under the single disposable root `/tmp/minio-investigation`; the source repository at `/tmp/blitzy/minio/blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc_5ba4b2` was never modified. The transcript below is chronological.

### Toolchain, client, canonical build, and default configuration

The toolchain, the `go.mod` directive, the verbatim canonical build target (`Makefile:L177`, which depends on both `checks` and `build-debugging` — the latter also builds the `xl-meta` helper used below), the version-stamped binary, the dedicated block device that lets a single-host multi-drive server start with **no CI override**, and the client alias:

```
===CMD: go version
go version go1.23.4 linux/amd64

===CMD: mc --version (line 1)
mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0dd2a80935aded936b09fa12cbb3cb8096)

===CMD: sed -n 3p go.mod
go 1.23

===CMD: sed -n 177,181p Makefile (build target verbatim)
build: checks build-debugging ## builds minio to $(PWD)
	@echo "Building minio binary to './minio'"
	@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null

hotfix-vars:

===CMD: ./minio --version (canonical worktree binary)
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.4 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.

===CMD: df /tmp (dedicated block device, not container root)
/dev/nvme0n1p1 /tmp

===CMD: mc alias ls inv (endpoint)
inv
  URL       : http://127.0.0.1:9000
  AccessKey : minioadmin
  SecretKey : (hidden)
  API       : s3v4
  Path      : auto
  Src       : /root/.mc/config.json
```

The `commit-id` in the banner is exactly the commit under analysis. Two points of methodology: (a) a plain build at the repository HEAD would stamp the HEAD commit, so the binary was built from a **detached git worktree pinned to `c07e5b49d477`** (`git worktree add --detach /tmp/minio-investigation/minio-src c07e5b49d477b0774f23db3b290745aef8c01bd2 && make build`), which makes the banner reproducibly identify the analysis commit; (b) `MINIO_CI_CD` was **not** set — the server runs in true default configuration. The only default-mode reason a single-host multi-drive server refuses to start is the root-disk guard, which does not fire here because `/tmp` is on the dedicated device `/dev/nvme0n1p1` shown above. The `make build` output itself:

```
$ make build
Checking dependencies
Building minio binary to './minio'
```

### Server invocation and topology

The server was launched with twelve local drives — the minimal canonical erasure topology exercising the default `EC:4` parity (eight data + four parity shards) — using only the two documented default credential variables:

```
$ MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    /tmp/minio-investigation/minio-src/minio server \
    /tmp/minio-investigation/drives/d{1,2,3,4,5,6,7,8,9,10,11,12} \
    --address :9000 --console-address :9001 > /tmp/minio-investigation/server.log 2>&1 &
```

The verbatim startup banner (first 16 lines of `server.log`), then the readiness/cluster-info handshake, the object listing, baseline checksums, and the on-disk shard layout of `obj1.bin` across all twelve drives. Readiness is established by the authenticated `mc admin info inv` call succeeding — it reports `Network: 1/1 OK` and `Drives: 12/12 OK`, values that only return once the server has finished formatting and is accepting requests, so a successful `admin info` is a strictly stronger readiness proof than a bare `/minio/health/ready` probe. The client alias itself was set with `mc alias set inv http://127.0.0.1:9000 minioadmin minioadmin` and is shown resolved by the `mc alias ls inv` block in the build listing above.

```
===CMD: sed -n 1,16p server.log (startup banner, verbatim)
INFO: Formatting 1st pool, 1 set(s), 12 drives per set.
INFO: WARNING: Host local has more than 4 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.4 linux/amd64)

API: http://10.236.12.19:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
WebUI: http://10.236.12.19:9001 http://172.17.0.1:9001 http://127.0.0.1:9001

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
INFO:
 You are running an older version of MinIO released 9 months before the latest release
 Update: Run `mc admin update ALIAS`

===CMD: mc admin info inv
●  127.0.0.1:9000
   Uptime: 49 minutes
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK
   Drives: 12/12 OK
   Pool: 1

┌──────┬───────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage          │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.3% (total: 192 TiB) │ 12                  │ 1            │
└──────┴───────────────────────┴─────────────────────┴──────────────┘

40 MiB Used, 1 Bucket, 5 Objects
12 drives online, 0 drives offline, EC:4

===CMD: mc ls inv/ectest/
[2026-07-13 18:52:02 UTC] 8.0MiB STANDARD obj1.bin
[2026-07-13 18:52:02 UTC] 8.0MiB STANDARD obj2.bin
[2026-07-13 18:52:02 UTC] 8.0MiB STANDARD obj3.bin
[2026-07-13 18:52:03 UTC] 8.0MiB STANDARD obj4.bin
[2026-07-13 18:52:03 UTC] 8.0MiB STANDARD obj5.bin

===CMD: cat baseline_checksums.txt
7e79b53e400e431c4266913e5692bfdba9804cc8b2a774a378f90beeb0d3a3e6  obj1.bin
791d6a32659caf5f9a5997f357f0d1835b284fed1d0cad5713e3ef9f3bf7e984  obj2.bin
5dfa87c389a491c7748209e15b8656c9cdf6d8091b989094cd0d7c4e31d9fc1e  obj3.bin
006448d7bdf68df42b3990c63f641a5450f61ceec18e1aa98ca2f93fb08ec7c0  obj4.bin
486b9066317f4f120f65e7d7ce9d0fa6128ac2288b57fcffe3de703265529b9e  obj5.bin

===CMD: obj1 shard layout across all 12 drives
d1: xl.meta=yes part.1=1048832 bytes
d2: xl.meta=yes part.1=1048832 bytes
d3: xl.meta=yes part.1=1048832 bytes
d4: xl.meta=yes part.1=1048832 bytes
d5: xl.meta=yes part.1=1048832 bytes
d6: xl.meta=yes part.1=1048832 bytes
d7: xl.meta=yes part.1=1048832 bytes
d8: xl.meta=yes part.1=1048832 bytes
d9: xl.meta=yes part.1=1048832 bytes
d10: xl.meta=yes part.1=1048832 bytes
d11: xl.meta=yes part.1=1048832 bytes
d12: xl.meta=yes part.1=1048832 bytes
```

The stripe size is twelve, there is one erasure set, and the effective parity is `EC:4` — consistent with the default-parity mapping at `cmd/erasure-server-pool.go:L120-L124` (EC:2 for 4–5 drives, EC:3 for 6–7, EC:4 for 8–16). Each drive holds `obj1.bin`'s `xl.meta` plus a `part.1` shard of 1,048,832 bytes (a 1-MiB erasure block plus bitrot-checksum overhead). Five 8-MiB random objects (`obj1`–`obj5`) form the baseline; their SHA-256 checksums above are the reference for every read/reconstruction integrity check.

### How a drive was taken offline and brought back (real backend path)

Drive failure was produced through the **real disk path**, never by editing parity or storage-class settings. Two mechanisms were used and are labeled accurately throughout:

- **Stable offline (non-destructive)** — used for the OBJ-1/OBJ-2 quorum experiments. The drive directory is renamed aside and a regular-file placeholder is left at the mount path (`mv drives/dN drives/dN.saved && : > drives/dN`). MinIO's disk monitor then finds a non-directory at the path and marks the drive offline; the condition is stable (does not auto-recreate) and non-destructive (data preserved in `dN.saved`, so restore is instant with no heal needed).
- **Fresh/replaced drive** — used for the OBJ-3/OBJ-4/OBJ-5 healing experiments. The placeholder is removed and an **empty** directory is created (`rm -f drives/dN && mkdir drives/dN`), modeling a blank replacement disk and triggering the automatic fresh-disk heal.

Two mechanisms were evaluated before settling on the placeholder for the stable-offline experiments. A bare rename (`mv drives/dN drives/dN.saved`) with **no** placeholder does make the drive momentarily absent, but the connect-and-heal monitors recreate/reconnect the path within their poll interval, so the drive does not stay offline long enough to drive the quorum experiments deterministically. Leaving a regular-file placeholder at the mount path holds the drive stably offline (the monitor keeps finding a non-directory) without destroying any data. The fresh-drive experiments instead use an empty directory precisely because the monitor treats it as an unformatted replacement and heals it.

An empirically-verified nuance about the offline signal: `errDiskNotFound` ("drive not found", `cmd/storage-errors.go:L53`) is the internal marker for a genuinely absent path, but under the placeholder mechanism the monitor records the closely-related `errDiskNotDir` ("drive is not directory or mountpoint", `cmd/storage-errors.go:L50`). This is why the placeholder mechanism is labeled accurately as producing `errDiskNotDir`, **not** `errDiskNotFound`. Neither string is written verbatim as a one-line server-log message — a whole-log `grep` for the literal `drive not found` returns `0` (shown in the OBJ-1 evidence). What the server logs is the underlying filesystem probe (`lstat <drive>/.minio.sys/format.json: not a directory`, shown verbatim in the OBJ-1 W2 evidence); the offline state itself is observed through `mc admin info` and the metrics of OBJ-6.

### S3 request tooling (single-shot SigV4)

Several decisive results (the `SlowDownWrite`/`SlowDownRead` 503s and the degraded-write boundary) are captured with a hand-written single-shot SigV4 client, `sigv4_oneshot.py`, rather than through `mc`, so the exact HTTP method, URL, signed headers, payload size, credential source, and single-attempt (no-retry) behavior are all visible in the request block. The tool: computes an `AWS4-HMAC-SHA256` signature for `service=s3`, `region=us-east-1`; uses **no** AWS SDK and performs exactly **one** `http.client` request with **no retry**; reads credentials from the environment (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, set to `minioadmin`/`minioadmin`); and prints the request line, the signed headers (including the full `Authorization` signature and `Payload bytes`), followed by the complete response status line, headers, and body. Its invocation is `python3 sigv4_oneshot.py METHOD HOST PORT BUCKET KEY [PAYLOAD_FILE]`, and every use below shows both the emitted `=== REQUEST (single shot, no retry) ===` block and the full `=== RESPONSE ===` block plus the process exit line (`SIGV4-PUT-EXIT`/`SIGV4-GET-EXIT`).

## OBJ-1 — Write path under drive loss

### Direct answer

It depends on **how many drives are offline at write time**, and the deciding threshold is half the set. With **fewer than half** the drives offline, the write **succeeds** in a degraded mode: MinIO's default availability-optimized storage class raises the object's parity by one per offline drive (capped at half the set) and records an `x-minio-internal-erasure-upgraded` marker. With **at least half** the drives offline, the write **fails** and the client receives S3 error **`SlowDownWrite`** at **HTTP 503**. For this twelve-drive set the failure threshold is `(12+1)/2 = 6`; this was confirmed empirically at 4, 5, and 6 offline.

### Code reference

- `cmd/erasure-object.go:L1291-L1319` — inside `erasureObjects.putObject`: the availability-optimized parity upgrade and the write-quorum decision. The half-the-set check (`L1304-L1308`) returns `errErasureWriteQuorum` when the number of offline drives is at least `(len(storageDisks)+1)/2`; the upgrade marker is written at `L1316`.
- `cmd/erasure-metadata.go:L531` — `objectQuorumFromMeta` computes read/write quorum from the object's data/parity counts.
- `cmd/erasure-errors.go:L25-L26` — `errErasureWriteQuorum = errors.New("Write failed. Insufficient number of drives online")`.
- `cmd/api-errors.go:L2192-L2193` maps `errErasureWriteQuorum` to `ErrSlowDownWrite`; `cmd/api-errors.go:L874-L878` defines `ErrSlowDownWrite` at HTTP `503`.
- Observed server-side chain: `PutObjectHandler` (`cmd/object-handlers.go:L2057`) -> `erasureServerPools.PutObject` (`cmd/erasure-server-pool.go:L1091`) -> `erasureSets.PutObject` (`cmd/erasure-sets.go:L749`) -> `erasureObjects.putObject` (`cmd/erasure-object.go:L1297`).

### Regime 1 — degraded SUCCESS (4 of 12 offline)

Full run W1: reset to all-online, take four drives offline through the real backend path, `PutObject` via `mc`, decode the on-disk `xl.meta` with the `xl-meta` helper to expose the parity upgrade, decode the raw upgrade marker, and prove integrity by downloading and comparing SHA-256. The complete captured transcript (the `mc cp` result is `mc`'s native box-table; `EcM`/`EcN` are both 6, i.e. parity raised from the default 4 to 6; the marker `NC0+Ng==` base64-decodes to `4->6`):

```
=== STEP 0: reset boundary - confirm all drives online ===
12 drives online, 0 drives offline, EC:4

=== STEP 1: generate fresh 8MiB payload w1obj.bin ===
source sha256: c3a242997bbce145dde9cfe5db7373808277a64b880b3bc9caa6502e9d582989

=== STEP 2: take drives d9 d10 d11 d12 offline (4 offline; (12+1)/2=6 threshold) ===
offline d9 (data preserved in d9.saved)
offline d10 (data preserved in d10.saved)
offline d11 (data preserved in d11.saved)
offline d12 (data preserved in d12.saved)
8 drives online, 4 drives offline, EC:4

=== STEP 3: mark server.log position, then PutObject via mc cp ===
`/tmp/minio-investigation/src/w1obj.bin` -> `inv/ectest/w1obj.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 90.31 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
mc cp exit=0
--- confirm object listed ---
[2026-07-13 19:02:58 UTC] 8.0MiB STANDARD w1obj.bin

=== STEP 4: locate on-disk xl.meta for w1obj.bin across online drives ===
/tmp/minio-investigation/drives/d1/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d2/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d3/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d4/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d5/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d6/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d7/ectest/w1obj.bin/xl.meta
/tmp/minio-investigation/drives/d8/ectest/w1obj.bin/xl.meta

=== STEP 5: raw xl-meta decode of one shard's xl.meta (d1) ===
xl.meta path: /tmp/minio-investigation/drives/d1/ectest/w1obj.bin/xl.meta
--- xl-meta full JSON output ---
{
  "Versions": [
    {
      "Header": {
        "EcM": 6,
        "EcN": 6,
        "Flags": 2,
        "ModTime": "2026-07-13T19:02:58.401986268Z",
        "Signature": "4c193ae7",
        "Type": 1,
        "VersionID": "00000000000000000000000000000000"
      },
      "Idx": 0,
      "Metadata": {
        "Type": 1,
        "V2Obj": {
          "CSumAlgo": 1,
          "DDir": "9wmLhgWrTVSCfol6M02L/Q==",
          "EcAlgo": 1,
          "EcBSize": 1048576,
          "EcDist": [
            11,
            12,
            1,
            2,
            3,
            4,
            5,
            6,
            7,
            8,
            9,
            10
          ],
          "EcIndex": 11,
          "EcM": 6,
          "EcN": 6,
          "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
          "MTime": 1783969378401986268,
          "MetaSys": {
            "x-minio-internal-erasure-upgraded": "NC0+Ng=="
          },
          "MetaUsr": {
            "content-type": "application/octet-stream",
            "etag": "e27f6e5905a7445add9253be003b45d8"
          },
          "PartASizes": [
            8388608
          ],
          "PartETags": null,
          "PartNums": [
            1
          ],
          "PartSizes": [
            8388608
          ],
          "Size": 8388608
        },
        "v": 1732554622
      }
    }
  ]
}

=== STEP 6: decode the raw x-minio-internal-erasure-upgraded base64 value ===

decoded string: 4->6

=== STEP 7: prove integrity - GET degraded object back and compare sha256 ===
`inv/ectest/w1obj.bin` -> `/tmp/minio-investigation/src/w1obj.download.bin`
┌──────────┬─────────────┬──────────┬──────────────┐
│ Total    │ Transferred │ Duration │ Speed        │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 112.89 MiB/s │
└──────────┴─────────────┴──────────┴──────────────┘
source   sha256: c3a242997bbce145dde9cfe5db7373808277a64b880b3bc9caa6502e9d582989
download sha256: c3a242997bbce145dde9cfe5db7373808277a64b880b3bc9caa6502e9d582989
INTEGRITY: MATCH (degraded write fully readable)

=== STEP 8: server-log lines emitted during the offline+write window ===
(from byte offset  to EOF; LOGPOS captured before PUT)

=== STEP 6b: hex of decoded marker (od, since xxd absent) ===
  34  2d  3e  36
   4   -   >   6

=== STEP 8b: server.log lines during W1 window (19:02:5x - 19:03) ===
--- any drive/disk error strings in whole log so far ---
4

=== STEP 8c: full multi-line drive-offline error block(s) during W1 window ===
(MinIO logs errDiskNotDir as 'drive is not directory or mountpoint (cmd.StorageErr)')
API: SYSTEM.peers
Time: 18:54:52 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: drive is not directory or mountpoint (cmd.StorageErr)
       endpoint="/tmp/minio-investigation/drives/d11"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
----
API: SYSTEM.peers
Time: 19:02:07 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: drive is not directory or mountpoint (cmd.StorageErr)
       endpoint="/tmp/minio-investigation/drives/d12"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
----
API: SYSTEM.peers
Time: 19:03:07 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: drive is not directory or mountpoint (cmd.StorageErr)
       endpoint="/tmp/minio-investigation/drives/d10"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
----
API: SYSTEM.peers
Time: 19:03:07 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: drive is not directory or mountpoint (cmd.StorageErr)
       endpoint="/tmp/minio-investigation/drives/d9"
       4: internal/logger/logger.go:258:logger.LogAlwaysIf()
       3: cmd/logging.go:65:cmd.peersLogAlwaysIf()
       2: cmd/prepare-storage.go:51:cmd.init.func22.1()
       1: cmd/erasure-sets.go:230:cmd.(*erasureSets).connectDisks.func2()
----
```

The two SHA-256 values are identical, so the degraded write is fully readable. The final block above is the genuine server-log signal emitted while the drives were offline: the `errDiskNotDir` probe surfaced through `erasureSets.connectDisks.func2` (`cmd/erasure-sets.go:L230` via `cmd/prepare-storage.go:L51`).

### Regime 1 boundary — 5 of 12 offline still SUCCEEDS

Five offline drives (still below the six-drive threshold) also succeed, again with parity upgraded `4->6`, captured with the single-shot SigV4 tool described in the environment appendix ("S3 request tooling"):

```
=== take 5 drives offline d8 d9 d10 d11 d12 ===
offline d8 (data preserved in d8.saved)
offline d9 (data preserved in d9.saved)
offline d10 (data preserved in d10.saved)
offline d11 (data preserved in d11.saved)
offline d12 (data preserved in d12.saved)
7 drives online, 5 drives offline, EC:4

=== single-shot SigV4 PUT (19 bytes) at 5 offline ===
=== REQUEST (single shot, no retry) ===
PUT /ectest/w3.bin HTTP/1.1
Host: 127.0.0.1:9000
x-amz-date: 20260713T190619Z
x-amz-content-sha256: 40aba5baad361b497e9fe49b0d3804e5da10822de29592fc5f8f1dba9e889ee4
Content-Length: 18
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=e99979e8ee9c22d28583b9b1a2e38898245f58c992cba7ff61742536e46ae6fa
Payload bytes: 18

=== RESPONSE ===
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
ETag: "07b7b7e393049b35bdc04d773aa40436"
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EEE158D88EE4
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 240762
X-Ratelimit-Remaining: 240762
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:06:19 GMT

SIGV4-PUT-EXIT=0

=== check parity upgrade on the 5-offline object (raw xl-meta erasure-upgraded marker) ===
xl.meta: /tmp/minio-investigation/drives/d1/ectest/w3.bin/xl.meta
        "EcM": 6,
        "EcN": 6,
          "EcM": 6,
          "EcN": 6,
            "x-minio-internal-erasure-upgraded": "NC0+Ng==",
--- decode upgraded marker ---
raw base64: NC0+Ng==  decoded: 4->6
=== restore ===
restored d8
restored d9
restored d10
restored d11
restored d12
12 drives online, 0 drives offline, EC:4
```

### Regime 2 — write FAILS (6 of 12 offline = the write-quorum threshold)

At six offline drives (`offlineDrives >= (12+1)/2`) the write fails. The full W2 run: reset, take six drives offline, then issue a **hand-written single-shot SigV4 PUT** (no SDK, no retries) so the exact HTTP status and XML are unambiguous. An 8-MiB body is early-rejected before the body is consumed (a real behavior — the connection is reset, full Python traceback captured); re-issuing with a 19-byte body returns the clean 503. The `mc` client is shown reporting the same condition (both JSON and plain forms), and a whole-log count confirms the literal quorum string is never logged — the server logs the per-drive `format.json` probe with the full `putObject` call chain instead:

```
=== STEP 0: reset boundary - restore d9-d12, confirm all 12 online ===
restored d9
restored d10
restored d11
restored d12
12 drives online, 0 drives offline, EC:4

=== STEP 1: take 6 drives offline d7 d8 d9 d10 d11 d12 (>= (12+1)/2 = 6 threshold) ===
offline d7 (data preserved in d7.saved)
offline d8 (data preserved in d8.saved)
offline d9 (data preserved in d9.saved)
offline d10 (data preserved in d10.saved)
offline d11 (data preserved in d11.saved)
offline d12 (data preserved in d12.saved)
6 drives online, 6 drives offline, EC:4

=== STEP 2: single-shot SigV4 PutObject (no SDK, no retry) -> expect 503 SlowDownWrite ===
=== REQUEST (single shot, no retry) ===
PUT /ectest/w2obj.bin HTTP/1.1
Host: 127.0.0.1:9000
x-amz-date: 20260713T190447Z
x-amz-content-sha256: 7455069e7495377184ca5850dbe85c9734805b478b8cae3c0b873c3dec98078c
Content-Length: 8388608
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=0c93a20456ab9ef6b6add487b736538e3656e07ea9243f21f24309cb418ebc7c
Payload bytes: 8388608
Traceback (most recent call last):
  File "/tmp/minio-investigation/sigv4_oneshot.py", line 83, in <module>
    main()
    ~~~~^^
  File "/tmp/minio-investigation/sigv4_oneshot.py", line 67, in main
    conn.request(method, canonical_uri, body=body, headers=headers)
    ~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/usr/lib/python3.13/http/client.py", line 1358, in request
    self._send_request(method, url, body, headers, encode_chunked)
    ~~~~~~~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/usr/lib/python3.13/http/client.py", line 1404, in _send_request
    self.endheaders(body, encode_chunked=encode_chunked)
    ~~~~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/usr/lib/python3.13/http/client.py", line 1353, in endheaders
    self._send_output(message_body, encode_chunked=encode_chunked)
    ~~~~~~~~~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/usr/lib/python3.13/http/client.py", line 1152, in _send_output
    self.send(chunk)
    ~~~~~~~~~^^^^^^^
  File "/usr/lib/python3.13/http/client.py", line 1077, in send
    self.sock.sendall(data)
    ~~~~~~~~~~~~~~~~~^^^^^^
ConnectionResetError: [Errno 104] Connection reset by peer
SIGV4-PUT-EXIT=1

=== STEP 2b: NOTE - 8MiB body caused early-reject Connection reset (server aborts before consuming body). ===
=== Re-issue single-shot SigV4 PUT with small body so full request is sent -> clean 503 XML ===
--- confirm still 6 offline before request ---
6 drives online, 6 drives offline, EC:4
--- single-shot SigV4 PUT (19-byte body) ---
=== REQUEST (single shot, no retry) ===
PUT /ectest/w2small.bin HTTP/1.1
Host: 127.0.0.1:9000
x-amz-date: 20260713T190505Z
x-amz-content-sha256: 79f3218008de2f378c5e77eff6cf4712b95129a815cb28e820aba5940857e21a
Content-Length: 19
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=fa1f200d0b4358ab7ad597517bcdb877107ea779ccbb81225d95292aa47d9eeb
Payload bytes: 19

=== RESPONSE ===
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 377
Content-Type: application/xml
Retry-After: 60
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EED0271E35D6
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 240762
X-Ratelimit-Remaining: 240762
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:05:05 GMT

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownWrite</Code><Message>Resource requested is unwritable, please reduce your request rate</Message><Key>w2small.bin</Key><BucketName>ectest</BucketName><Resource>/ectest/w2small.bin</Resource><RequestId>18C1EED0271E35D6</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
SIGV4-PUT-EXIT=1

=== STEP 3: mc client PutObject under same 6-offline condition (client-visible error) ===
6 drives online, 6 drives offline, EC:4
{"status":"success","source":"/tmp/minio-investigation/src/w2small.bin","target":"inv/ectest/w2mc.bin","size":19,"totalCount":1,"totalSize":0}
{"status":"error","error":{"message":"Failed to copy `/tmp/minio-investigation/src/w2small.bin`.","cause":{"message":"Resource requested is unwritable, please reduce your request rate","error":{"Code":"SlowDownWrite","Message":"Resource requested is unwritable, please reduce your request rate","BucketName":"ectest","Key":"w2mc.bin","Resource":"/ectest/w2mc.bin","RequestID":"18C1EED43757F9C3","HostID":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8","Region":"","Server":"MinIO"}},"type":"error"}}
mc-cp-exit=1
--- also plain (non-json) mc error text ---
`/tmp/minio-investigation/src/w2small.bin` -> `inv/ectest/w2mc.bin`
mc: <ERROR> Failed to copy `/tmp/minio-investigation/src/w2small.bin`. Resource requested is unwritable, please reduce your request rate
mc-cp-exit=1

=== STEP 4: server.log quorum signal during W2 window ===
--- full most-recent error block mentioning write/quorum ---
9201:       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
9202:       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()
9218:       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
9219:       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()
9235:       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
9236:       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()
9252:       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
9253:       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()
9269:       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
9270:       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()

=== STEP 4b: full server.log error block(s) for the write-quorum failure ===
       2: cmd/object-handlers.go:2057:cmd.objectAPIHandlers.PutObjectHandler()
       1: net/http/server.go:2220:http.HandlerFunc.ServeHTTP()

API: SYSTEM.storage
Time: 19:05:25 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: lstat /tmp/minio-investigation/drives/d7/.minio.sys/format.json: not a directory (*fs.PathError)
      12: internal/logger/logonce.go:118:logger.(*logOnceType).logOnceIf()
      11: internal/logger/logonce.go:149:logger.LogOnceIf()
      10: cmd/logging.go:164:cmd.storageLogOnceIf()
       9: cmd/xl-storage.go:821:cmd.(*xlStorage).checkFormatJSON()
       8: cmd/xl-storage.go:841:cmd.(*xlStorage).GetDiskID()
       7: cmd/xl-storage-disk-id-check.go:209:cmd.(*xlStorageDiskIDCheck).IsOnline()
       6: cmd/erasure-object.go:1297:cmd.erasureObjects.putObject()
       5: cmd/erasure-object.go:1241:cmd.erasureObjects.PutObject()
       4: cmd/erasure-sets.go:749:cmd.(*erasureSets).PutObject()
       3: cmd/erasure-server-pool.go:1091:cmd.(*erasureServerPools).PutObject()
       2: cmd/object-handlers.go:2057:cmd.objectAPIHandlers.PutObjectHandler()

=== STEP 4c: does the literal errErasureWriteQuorum string ever appear in server.log? ===
count of "Insufficient number of drives online": 0
count of "Write failed": 0
=== STEP 5: cleanup W2 offline drives -> restore to 12 online ===
restored d7
restored d8
restored d9
restored d10
restored d11
restored d12
12 drives online, 0 drives offline, EC:4
```

### Cause -> effect (OBJ-1)

Below half the set, the availability-optimized storage class trades data shards for parity shards so a full complement of shards still lands on the online drives; the object is stored with elevated parity (`4->6`) and reconstructs normally. At half the set or more, no shard layout satisfies write quorum, so `putObject` returns `errErasureWriteQuorum`, which the API layer maps to `SlowDownWrite` / HTTP 503 with a `Retry-After: 60` header. The literal error text is returned up the stack and mapped at `cmd/api-errors.go:L2192-L2193`, not logged verbatim (whole-log count = 0).

## OBJ-2 — Read path for pre-existing objects

### Direct answer

Yes — a pre-existing object stays readable as long as the number of **online** drives meets read quorum, which for the default `EC:4` layout equals the eight data shards. With four drives offline (eight online) `obj1.bin` is reconstructed on the fly via Reed-Solomon and served byte-for-byte identically. Once online drives drop below the data count — five offline (seven online) — the read **fails** with S3 error **`SlowDownRead`** at **HTTP 503**. Both regimes were exercised on the **same unchanged** pre-existing object.

### Code reference

- `cmd/erasure-object.go:L200` — `erasureObjects.GetObjectNInfo`, the read entry point.
- `cmd/erasure-object.go:L705` — `getObjectFileInfo`; the read-quorum failure surfaces from its inline worker `getObjectFileInfo.func1` (`cmd/erasure-object.go:L738`, seen in the server-log stack below).
- `cmd/erasure-object.go:L487` — the `errErasureReadQuorum` return site; `cmd/erasure-object.go:L836` — `reduceReadQuorumErrs` aggregates per-drive errors into the quorum decision.
- `cmd/erasure-errors.go:L22-L23` — `errErasureReadQuorum = errors.New("Read failed. Insufficient number of drives online")`.
- `cmd/api-errors.go:L2190-L2191` maps `errErasureReadQuorum` to `ErrSlowDownRead`; `cmd/api-errors.go:L869-L873` defines `ErrSlowDownRead` at HTTP `503`.

### Regime 1 — reconstruction SUCCEEDS (4 offline, 8 online)

Full run R1: reset to all-online, confirm the baseline SHA-256 of `obj1.bin`, take four drives offline (eight online == the eight data shards), GET via `mc` (Reed-Solomon reconstruction), compare checksums, and also issue a single-shot SigV4 GET showing HTTP 200 with the full 8-MiB `Content-Length`:

```
=== STEP 0: reset boundary - confirm 12 online ===
12 drives online, 0 drives offline, EC:4

=== baseline sha256 of obj1 (recorded in baseline_checksums.txt) ===
7e79b53e400e431c4266913e5692bfdba9804cc8b2a774a378f90beeb0d3a3e6  obj1.bin

=== STEP 1: take 4 drives offline d9 d10 d11 d12 (8 online == dataBlocks=8 read quorum) ===
offline d9 (data preserved in d9.saved)
offline d10 (data preserved in d10.saved)
offline d11 (data preserved in d11.saved)
offline d12 (data preserved in d12.saved)
8 drives online, 4 drives offline, EC:4

=== STEP 2: GET obj1 via mc (Reed-Solomon reconstruction from remaining shards) ===
`inv/ectest/obj1.bin` -> `/tmp/minio-investigation/src/obj1.r1.bin`
┌──────────┬─────────────┬──────────┬──────────────┐
│ Total    │ Transferred │ Duration │ Speed        │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 262.33 MiB/s │
└──────────┴─────────────┴──────────┴──────────────┘
mc-cp-exit=0
reconstructed sha256: 7e79b53e400e431c4266913e5692bfdba9804cc8b2a774a378f90beeb0d3a3e6
baseline      sha256: 7e79b53e400e431c4266913e5692bfdba9804cc8b2a774a378f90beeb0d3a3e6
READ RECONSTRUCTION: MATCH (object served with 4 drives offline)

=== STEP 3: also single-shot SigV4 GET at 4 offline (status + headers, body length only) ===
=== REQUEST (single shot, no retry) ===
GET /ectest/obj1.bin HTTP/1.1
Host: 127.0.0.1:9000
x-amz-date: 20260713T190646Z
x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
Content-Length: 0
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=dfd65b6ad35d7bc81173fcf7f7afd351e55b86e84720ac7f59b09434dcfb31ff
Payload bytes: 0

=== RESPONSE ===
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 8388608
Content-Type: application/octet-stream
ETag: "80fe03eeccddbf1bc4a92e5baf1ce7a0"
Last-Modified: Mon, 13 Jul 2026 18:52:02 GMT
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
```

The reconstructed SHA-256 (`7e79b53e400e431c4266913e5692bfdba9804cc8b2a774a378f90beeb0d3a3e6`) equals the baseline `obj1.bin` checksum recorded in the Environment appendix.

### Regime 2 — read FAILS (5 offline, 7 online) on the SAME object

Full run R2: without altering the object, extend to five offline (seven online, below the eight data shards) and reissue the identical single-shot SigV4 GET — HTTP 503 `SlowDownRead`. `mc` reports the same; the whole-log count of the literal read-quorum string is 0; the genuine server-log read-path stack (`getObjectFileInfo.func1` at `cmd/erasure-object.go:L738`) is captured:

```
=== STEP 1: extend to 5 drives offline (add d8; d8..d12 offline) ===
offline d8 (data preserved in d8.saved)
7 drives online, 5 drives offline, EC:4

=== STEP 2: single-shot SigV4 GET obj1 -> expect 503 SlowDownRead ===
=== REQUEST (single shot, no retry) ===
GET /ectest/obj1.bin HTTP/1.1
Host: 127.0.0.1:9000
x-amz-date: 20260713T190701Z
x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
Content-Length: 0
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=2c6d2bd19b6f701a548f37b5bfb52a6cfaf03e326ea3bb89613648b6e83c8271
Payload bytes: 0

=== RESPONSE ===
HTTP/1.1 503 Service Unavailable
Accept-Ranges: bytes
Content-Length: 370
Content-Type: application/xml
Retry-After: 60
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EEEB22E30C85
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 240762
X-Ratelimit-Remaining: 240762
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 19:07:01 GMT

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>obj1.bin</Key><BucketName>ectest</BucketName><Resource>/ectest/obj1.bin</Resource><RequestId>18C1EEEB22E30C85</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
SIGV4-GET-EXIT=1

=== STEP 3: mc GET same condition (client-visible error) ===
mc: <ERROR> Unable to prepare URL for copying. Resource requested is unreadable, please reduce your request rate
mc-cp-exit=1

=== STEP 4: server.log read-path signal during R2 window (GetObject stack) ===
13142:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13154:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13166:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13178:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13190:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13202:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13214:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
13226:       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
--- full most-recent GetObject error block ---
(no GetObjectHandler block found)

count of literal "Read failed. Insufficient number of drives online": 0

=== STEP 5: restore all -> 12 online ===
restored d8
restored d9
restored d10
restored d11
restored d12
12 drives online, 0 drives offline, EC:4

=== STEP 4b: full server.log read-path error block (around line 13142) ===
       3: cmd/xl-storage.go:841:cmd.(*xlStorage).GetDiskID()
       2: cmd/xl-storage-disk-id-check.go:209:cmd.(*xlStorageDiskIDCheck).IsOnline()
       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()

API: SYSTEM.storage
Time: 19:07:04 UTC 07/13/2026
DeploymentID: 9c3ab6b6-31d4-498b-808d-70099759e89a
Error: lstat /tmp/minio-investigation/drives/d8/.minio.sys/format.json: not a directory (*fs.PathError)
       7: internal/logger/logonce.go:118:logger.(*logOnceType).logOnceIf()
       6: internal/logger/logonce.go:149:logger.LogOnceIf()
       5: cmd/logging.go:164:cmd.storageLogOnceIf()
       4: cmd/xl-storage.go:821:cmd.(*xlStorage).checkFormatJSON()
       3: cmd/xl-storage.go:841:cmd.(*xlStorage).GetDiskID()
       2: cmd/xl-storage-disk-id-check.go:209:cmd.(*xlStorageDiskIDCheck).IsOnline()
       1: cmd/erasure-object.go:738:cmd.erasureObjects.getObjectFileInfo.func1()
```

### Cause -> effect (OBJ-2)

While at least the eight data shards are reachable, `getObjectFileInfo`/`GetObjectNInfo` reconstruct any missing shards and stream the object; the served bytes match the original checksum exactly. When online drives fall below the data count, no set of shards can reconstruct the object, `reduceReadQuorumErrs` yields `errErasureReadQuorum`, and the API layer maps it to `SlowDownRead` / HTTP 503.

## OBJ-3 — What triggers healing when a drive comes back online

### Direct answer

The trigger for a **recovered or replaced drive** is the local disk monitor `monitorLocalDisksAndHeal`, which polls on a **10-second** interval, detects that a previously-offline drive is reachable again (or that a fresh, unformatted drive is present), formats it if necessary, and dispatches `healFreshDisk` to heal every object the drive should hold. Measured across three runs the restore-to-heal-completion latency was **11.2 s, 24.3 s, and 12.2 s** — quantized to the 10-second poll (roughly one cycle for H1/H3, two for H2). Two additional healing paths exist and are classified explicitly below: the **MRF** on-the-fly heal enqueued by GET/PUT, and the **background data scanner**.

### Code reference

- `cmd/background-newdisks-heal-ops.go:L40` — `defaultMonitorNewDiskInterval = time.Second * 10`.
- `cmd/background-newdisks-heal-ops.go:L563` — `monitorLocalDisksAndHeal`, launched at `L386`.
- `cmd/background-newdisks-heal-ops.go:L419` — `healFreshDisk`, the per-drive dispatch.
- `cmd/global-heal.go:L152` — `healErasureSet`, which walks the set and heals each object; `cmd/global-heal.go:L210` emits the "use N parallel workers" line.
- MRF path: `cmd/mrf.go:L78` `addPartialOp` (enqueue), `cmd/mrf.go:L220` `healRoutine` (drain); GET/PUT enqueue site `cmd/erasure-object.go:L2113`.
- Scanner path: `cmd/data-scanner.go:L61` `healObjectSelectProb = 1024`; deep-scan mode plumbed at `cmd/data-scanner.go:L93,L199`.

### Observation — recovered/fresh-drive detection and dispatch (3 timestamped runs)

For each run drive `d12` was taken offline, then brought back as an **empty replacement directory** (the canonical fresh-disk case); the server log was watched for the reconnect, the fresh-disk dispatch, the "use N parallel workers" banner, and the completion line, and restore-to-completion latency was computed from the real log timestamps. Note that `mc admin info` reports the drive "online" within ~17 ms of the directory reappearing (connectivity), whereas the **heal** completes 11–24 s later — the distinction OBJ-6 makes precise. Runs H1, H2, H3 in full:

```
=== STEP 0: reset boundary - ensure 12/12 online before run ===
state: 12 drives online, 0 drives offline, EC:4

=== STEP 1: take d12 offline (placeholder), confirm offline ===
offline trigger at: 2026-07-13T19:11:05.612Z  (mv d12 -> d12.gone + placeholder)
state: 11 drives online, 1 drive offline, EC:4

=== STEP 2: THE TRIGGER - bring d12 back as FRESH empty unformatted drive ===
server.log line count before trigger: 13420
RECOVERED-DRIVE trigger at: 2026-07-13T19:11:06.699Z  (rm placeholder + mkdir empty d12)
d12 is now present but unformatted (no .minio.sys/format.json) => fresh-disk heal path

=== STEP 3: poll for reconnect + heal dispatch + completion (timestamped) ===
[2026-07-13T19:11:06.716Z] RECONNECT observed: 12 drives online, 0 drives offline, EC:4
[2026-07-13T19:11:17.872Z] HEAL DISPATCH: Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
[2026-07-13T19:11:17.872Z] HEAL FINISH: Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 16, skipped: 0).

=== STEP 4: timestamps + computed latencies ===
T_trigger  (drive back online): 2026-07-13T19:11:06.699Z
T_reconnect(12/12 online)     : 2026-07-13T19:11:06.716Z
T_dispatch (use N workers)    : 2026-07-13T19:11:17.872Z
T_finish   (is finished)      : 2026-07-13T19:11:17.872Z
latency trigger->dispatch : 11.2 s
latency trigger->finish   : 11.2 s

=== STEP 5: full new server.log segment for this run (from line 13421) ===
Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 16, skipped: 0).
(segment appended; 3 lines)
```

```
=== STEP 0: reset boundary - ensure 12/12 online before run ===
state: 12 drives online, 0 drives offline, EC:4

=== STEP 1: take d12 offline (placeholder), confirm offline ===
offline trigger at: 2026-07-13T19:12:52.957Z  (mv d12 -> d12.gone + placeholder)
state: 11 drives online, 1 drive offline, EC:4

=== STEP 2: THE TRIGGER - bring d12 back as FRESH empty unformatted drive ===
server.log line count before trigger: 13561
RECOVERED-DRIVE trigger at: 2026-07-13T19:12:54.045Z  (rm placeholder + mkdir empty d12)
d12 is now present but unformatted (no .minio.sys/format.json) => fresh-disk heal path

=== STEP 3: poll for reconnect + heal dispatch + completion (timestamped) ===
[2026-07-13T19:12:54.062Z] RECONNECT observed: 12 drives online, 0 drives offline, EC:4
[2026-07-13T19:13:17.373Z] HEAL DISPATCH: Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
[2026-07-13T19:13:18.392Z] HEAL FINISH: Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).

=== STEP 4: timestamps + computed latencies ===
T_trigger  (drive back online): 2026-07-13T19:12:54.045Z
T_reconnect(12/12 online)     : 2026-07-13T19:12:54.062Z
T_dispatch (use N workers)    : 2026-07-13T19:13:17.373Z
T_finish   (is finished)      : 2026-07-13T19:13:18.392Z
latency trigger->dispatch : 23.3 s
latency trigger->finish   : 24.3 s

=== STEP 5: full new server.log segment for this run (from line 13562) ===
Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).
(segment appended; 3 lines)
```

```
=== STEP 0: reset boundary - ensure 12/12 online before run ===
state: 12 drives online, 0 drives offline, EC:4

=== STEP 1: take d12 offline (placeholder), confirm offline ===
offline trigger at: 2026-07-13T19:13:34.436Z  (mv d12 -> d12.gone + placeholder)
state: 11 drives online, 1 drive offline, EC:4

=== STEP 2: THE TRIGGER - bring d12 back as FRESH empty unformatted drive ===
server.log line count before trigger: 13702
RECOVERED-DRIVE trigger at: 2026-07-13T19:13:35.522Z  (rm placeholder + mkdir empty d12)
d12 is now present but unformatted (no .minio.sys/format.json) => fresh-disk heal path

=== STEP 3: poll for reconnect + heal dispatch + completion (timestamped) ===
[2026-07-13T19:13:35.540Z] RECONNECT observed: 12 drives online, 0 drives offline, EC:4
[2026-07-13T19:13:47.713Z] HEAL DISPATCH: Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
[2026-07-13T19:13:47.713Z] HEAL FINISH: Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).

=== STEP 4: timestamps + computed latencies ===
T_trigger  (drive back online): 2026-07-13T19:13:35.522Z
T_reconnect(12/12 online)     : 2026-07-13T19:13:35.540Z
T_dispatch (use N workers)    : 2026-07-13T19:13:47.713Z
T_finish   (is finished)      : 2026-07-13T19:13:47.713Z
latency trigger->dispatch : 12.2 s
latency trigger->finish   : 12.2 s

=== STEP 5: full new server.log segment for this run (from line 13703) ===
Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).
(segment appended; 3 lines)
```

The three latencies (11.2 s, 24.3 s, 12.2 s) cluster around one and two 10-second poll cycles, exactly as expected for the 10-second `defaultMonitorNewDiskInterval`.

### Classification of the three healing paths

- **`monitorLocalDisksAndHeal` -> `healFreshDisk` — OBSERVED (canonical).** Automatic and timestamped above; this is the recovered/replaced-drive trigger the question asks about.
- **MRF on-the-fly heal — OBSERVED STATE + SOURCE.** The MRF queue (`cmd/mrf.go:L78,L220`) is enqueued from the GET/PUT path (`cmd/erasure-object.go:L2113`) when a read/write sees a missing shard. In these runs the admin heal status reported `"mrf": null` (empty queue) because the fresh-disk monitor healed the objects first; the mechanism is confirmed in source and by the queue-state field, and no separate canonical trigger was required.
- **Background data scanner — OBSERVED (indirect).** The scanner (`cmd/data-scanner.go:L61,L199`) advanced its item count across runs (`ScannedItemsCount` moved 245 -> 246, shown under OBJ-4); it heals opportunistically on its own cadence rather than as an immediate drive-recovery trigger.

## OBJ-4 — Criteria for deciding an object needs healing on a drive

### Direct answer

The per-drive decision is made by `shouldHealObjectOnDisk`. It flags an object for healing on a given drive when any of: (1) the drive returns `errFileNotFound`/`errFileVersionNotFound` (metadata absent); (2) a data part is missing or fails its bitrot check (`errPartMissingOrCorrupt`); (3) the drive's metadata is legacy XLv1 (`errLegacyXLMeta`); or (4) the drive's metadata is out of date relative to the latest quorum metadata (`errOutdatedXLMeta`). Case (1) was observed canonically via the fresh-drive heal; case (2) was reproduced by bitrot simulation and confirmed via deep-scan heal (labeled SUPPLEMENTARY / NON-CANONICAL); cases (3)–(4) are SOURCE-ONLY/INFERRED on this commit.

### Code reference

`cmd/erasure-healing.go:L156-L183` — `shouldHealObjectOnDisk`: `errFileNotFound`/`errFileVersionNotFound` (`L157-L159`); legacy `errLegacyXLMeta` (`L161-L164`, defined `L148`); outdated `errOutdatedXLMeta` (`L166-L167`, defined `L150`); missing/corrupt parts `errPartMissingOrCorrupt` (`L169-L176`, defined `L152`). The in-progress marker `xMinIOHealing` is at `cmd/erasure-healing.go:L186`; the repair itself is `erasureObjects.healObject` at `cmd/erasure-healing.go:L258`.

### Observation 1 — absent metadata (`errFileNotFound`): CANONICAL fresh-drive heal + status reconciliation

The fresh-drive runs of OBJ-3 are exactly this case: a replaced drive has none of the object metadata, so every object trips the `errFileNotFound` branch and is healed. The admin heal status (via the canonical admin API) reconciles the `healed: N` counts — the healed drive holds seven **user** objects (`objects_total_count: 7`) but `items_healed` (15) also includes system metadata under `.minio.sys/config` and `.minio.sys/buckets` (the three `healed_buckets`), which is why the completion line's `healed:` figure exceeds seven. `sc_parity` independently confirms `STANDARD: 4` (i.e. `EC:4`); `mrf: null` and `ScannedItemsCount` corroborate the OBJ-3 classifications:

```
full JSON saved to heal_status_full.json (14904 bytes)

=== top-level ScannedItemsCount (background data scanner — OBSERVED) ===
ScannedItemsCount: 245

=== top-level mrf field (MRF on-the-fly heal queue — OBSERVED state) ===

=== sc_parity (default storage-class parity — confirms EC:4 STANDARD) ===

=== d12 heal_info (RECONCILES healed count — F11) ===
  heal_id: b45bf644-20e9-4a89-9239-1d3f31c2594e
  started: 2026-07-13T19:13:47.37518189Z
  last_update: 2026-07-13T19:13:47.430799588Z
  objects_total_count: 7
  items_healed: 15
  objects_healed: 15
  items_failed: 0
  items_skipped: 0
  bytes_done: 50344117
  healed_buckets: ['.minio.sys/config', '.minio.sys/buckets', 'ectest']
  finished: True

=== CORRECTED: mrf + sc_parity at HealInfo level ===
mrf: null
sc_parity: {"REDUCED_REDUNDANCY": 1, "STANDARD": 4}
ScannedItemsCount: 245
HealDisks: null
offline_nodes: null
```

### Observation 2 — missing/corrupt part (`errPartMissingOrCorrupt`): SIMULATED + SUPPLEMENTARY deep-scan

To exercise branch (2) directly, the first 4096 bytes of `obj2.bin`'s `part.1` on drive `d1` were zeroed (bitrot), preserving the file size so only the shard checksum breaks. The full run captures three distinct facts, each labeled in the transcript: **(A, canonical)** a normal GET still returns the **correct object** (served SHA-256 matches baseline `obj2.bin`) because the bitrot shard is reconstructed at read time — but this canonical read does **not** persist a repair (the MRF queue stays `null`); **(B)** a **normal-mode** `mc admin heal` reports every drive `ok`, because normal-mode heal compares metadata, not part checksums; **(C, SUPPLEMENTARY / NON-CANONICAL** — a deep scan is not the default drive-recovery trigger) a `--scan deep` heal detects drive `d1`'s part state as `missing` and rebuilds it to the pristine shard SHA-256:

```
=== STEP 0: ensure 12/12 online ===
12 drives online, 0 drives offline, EC:4

=== STEP 1: locate obj2 part.1 on d1 and corrupt first 4096 bytes ===
part path: /tmp/minio-investigation/drives/d1/ectest/obj2.bin/ba0109cb-f299-44fd-9c58-c696dc57d20b/part.1
size before: 1048832 bytes; sha256 before: 7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6
size after : 1048832 bytes; sha256 after : a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8

=== STEP 2: mark log pos, GET obj2 via canonical S3 (mc) — expect success (heal from other shards) ===
`inv/ectest/obj2.bin` -> `/tmp/minio-investigation/src/obj2.bitrot.bin`
┌──────────┬─────────────┬──────────┬──────────────┐
│ Total    │ Transferred │ Duration │ Speed        │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 558.07 MiB/s │
└──────────┴─────────────┴──────────┴──────────────┘
mc-get-exit=0
served sha256  : 791d6a32659caf5f9a5997f357f0d1835b284fed1d0cad5713e3ef9f3bf7e984
baseline sha256: 791d6a32659caf5f9a5997f357f0d1835b284fed1d0cad5713e3ef9f3bf7e984
READ INTEGRITY: MATCH (bitrot shard bypassed/reconstructed transparently)

=== STEP 3: server.log signals after the bitrot GET (from line ; recompute) ===
--- bitrot / checksum / verify signals in log ---

=== STEP 4: was the corrupt shard on d1 auto-repaired by the read (MRF on-the-fly)? ===
part sha256 now: a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8
(pristine was 7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6)

=== STEP 5: re-scrape heal status - did mrf queue get a partial op? ===
mrf: null
ScannedItemsCount: 246

=== STEP 6: SUPPLEMENTARY (NON-CANONICAL explicit trigger) — mc admin heal obj2 ===
shard sha256 BEFORE admin heal: a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8  (corrupt)
status: success
  type: bucket
  before drive-states: [('d9', 'ok'), ('d10', 'ok'), ('d3', 'ok'), ('d5', 'ok'), ('d7', 'ok'), ('d8', 'ok'), ('d11', 'ok'), ('d12', 'ok'), ('d1', 'ok'), ('d2', 'ok'), ('d4', 'ok'), ('d6', 'ok')]
  after drive-states: [('d9', 'ok'), ('d10', 'ok'), ('d3', 'ok'), ('d5', 'ok'), ('d7', 'ok'), ('d8', 'ok'), ('d11', 'ok'), ('d12', 'ok'), ('d1', 'ok'), ('d2', 'ok'), ('d4', 'ok'), ('d6', 'ok')]
status: success
  type: object
  before drive-states: [('d1', 'ok'), ('d2', 'ok'), ('d3', 'ok'), ('d4', 'ok'), ('d5', 'ok'), ('d6', 'ok'), ('d7', 'ok'), ('d8', 'ok'), ('d9', 'ok'), ('d10', 'ok'), ('d11', 'ok'), ('d12', 'ok')]
  after drive-states: [('d1', 'ok'), ('d2', 'ok'), ('d3', 'ok'), ('d4', 'ok'), ('d5', 'ok'), ('d6', 'ok'), ('d7', 'ok'), ('d8', 'ok'), ('d9', 'ok'), ('d10', 'ok'), ('d11', 'ok'), ('d12', 'ok')]
status: success
  type: summary

shard sha256 AFTER admin heal : a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8
pristine target               : 7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6
shard state changed (see sha256)

=== STEP 7: deep-scan heal (--scan deep) verifies PART bitrot, not just metadata ===
shard sha256 BEFORE deep heal: a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8  (corrupt)
  type: object
  bucket: None
  object: None
  before: NON-OK drives = [('d1', 'missing')]
  after: NON-OK drives = none (all ok)

shard sha256 AFTER deep heal : 7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6
pristine target              : 7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6
SHARD REPAIRED: deep-scan detected part bitrot (errPartMissingOrCorrupt) and reconstructed d1 to pristine
```

In the transcript, the object SHA-256 `791d6a32659caf5f9a5997f357f0d1835b284fed1d0cad5713e3ef9f3bf7e984` is the reconstructed **object**; the shard SHA-256 (`7f33f3e90832ff4e31618666a1b641f8f1997458bf6d10438a3ae8056e092ca6` pristine, `a07314c31a395f2515bf9b88bef6b720f7d06330c0e9f67ca6548d3546ec75e8` after corruption) is the **on-disk part** on `d1`. Note that in STEP 6 the shard SHA-256 is identical before and after the normal-mode heal (both the post-corruption value), so the shard was **not** repaired — the script's generic "shard state changed" label is contradicted by the unchanged hash, confirming that normal-mode heal (metadata-only) leaves the bitrot part in place. Only the STEP 7 deep-scan pass detects `d1` as `missing` and restores the shard to the pristine value.

### Observation 3 — legacy / outdated metadata: SOURCE-ONLY / INFERRED

Branches (3) `errLegacyXLMeta` and (4) `errOutdatedXLMeta` require, respectively, an object written by a pre-XLv2 MinIO release and a metadata-version skew across drives. Neither can be produced through the canonical S3 API on this commit (there is no client-facing way to write XLv1 metadata or to desynchronize metadata versions without editing backend files, which is out of scope for a read-only, canonical investigation). Their existence and trigger conditions are cited from `cmd/erasure-healing.go:L161-L167`; any statement about their runtime effect is **INFERRED**, not observed.

### Cause -> effect (OBJ-4)

`shouldHealObjectOnDisk` is consulted per drive during a heal walk. If a drive lacks the metadata, has a bad part, or carries stale/legacy metadata, the object is scheduled for `healObject`, which reconstructs the missing/damaged shard from the surviving shards and rewrites it, marking progress with the `xMinIOHealing` metadata flag.

## OBJ-5 — Log messages during an active healing operation

### Direct answer

An active heal of a drive emits three characteristic lines per cycle, in order: a status pointer suggesting `mc admin heal alias/ --verbose`, the active-heal banner **"Healing drive '<path>' - use N parallel workers."**, and the completion line **"Healing of drive '<path>' is finished (healed: N, skipped: M)."**. On this 4-CPU host `N` was always **4** parallel workers, because the worker count has a floor of 4.

### Code reference

- `cmd/global-heal.go:L210` — the `"use %d parallel workers."` line (from `healErasureSet`).
- `cmd/background-newdisks-heal-ops.go:L460` — the status-pointer line.
- `cmd/background-newdisks-heal-ops.go:L520` — the `"is finished (healed:..)"` completion line.
- Worker-count floor of 4: `cmd/global-heal.go:L195-L208` (`numHealers`).

### Observation — exact grep commands, counts, and per-cycle correlation

Run live against the accumulated `server.log`. The `grep -c` counts are `6` because six heal cycles occurred over the whole investigation; each cycle contributes exactly one of each line. Note the heal lines carry **no** timestamp prefix in the log — they are emitted bare:

```
===CMD: grep -n 'use .* parallel workers' server.log
19:Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
286:Healing drive '/tmp/minio-investigation/drives/d11' - use 4 parallel workers.
13422:Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
13563:Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
13704:Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.
13923:Healing drive '/tmp/minio-investigation/drives/d12' - use 4 parallel workers.

===CMD: grep -c 'parallel workers' server.log
6

===CMD: grep -n 'is finished (healed:' server.log
20:Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 14, skipped: 0).
287:Healing of drive '/tmp/minio-investigation/drives/d11' is finished (healed: 13, skipped: 0).
13423:Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 16, skipped: 0).
13564:Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).
13705:Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 15, skipped: 0).
13924:Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 175, skipped: 0).

===CMD: grep -c 'is finished (healed:' server.log
6

===CMD: grep -n 'to check the current status' server.log
18:Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
285:Healing drive '/tmp/minio-investigation/drives/d11' - 'mc admin heal alias/ --verbose' to check the current status.
13421:Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
13562:Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
13703:Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
13922:Healing drive '/tmp/minio-investigation/drives/d12' - 'mc admin heal alias/ --verbose' to check the current status.
```

The six cycles correlate as: `L18-20`/`L285-287` — the two baseline-setup heals during environment preparation (d12 healed:14, d11 healed:13); `L13421-13423`/`L13562-13564`/`L13703-13705` — the three timestamped OBJ-3 runs (H1 healed:16, H2 healed:15, H3 healed:15); and `L13922-13924` — the OBJ-6 lifecycle heal over the larger dataset (d12 healed:175). Every cycle used `4` parallel workers, consistent with the worker-count floor on this 4-CPU host. The `healed:` counts include user objects plus the `.minio.sys/config` and `.minio.sys/buckets` system metadata (reconciled under OBJ-4).

### Cause -> effect (OBJ-5)

When `healFreshDisk` runs, `healErasureSet` logs the status pointer and the "use N parallel workers" banner, spins up N worker goroutines that call `healObject` for each object the drive should hold, and, when the walk completes, logs the "is finished (healed: N, skipped: M)" line with the tallies.

## OBJ-6 — Metric names for online vs. offline drive counts, and their values

### Direct answer

Two metric families report drive counts:

- **Metrics v3** (path `/minio/metrics/v3/cluster/health`): `minio_cluster_health_drives_online_count`, `minio_cluster_health_drives_offline_count`, and `minio_cluster_health_drives_count`.
- **Metrics v2** (path `/minio/v2/metrics/cluster`, namespace `minio_cluster`): `minio_cluster_drive_online_total`, `minio_cluster_drive_offline_total`, and `minio_cluster_drive_total`. A distinct heal-status gauge, `minio_cluster_health_erasure_set_healing_drives`, tracks drives actively healing.

Observed values: with all drives online, `minio_cluster_health_drives_online_count = 12`, `minio_cluster_health_drives_count = 12`, and `minio_cluster_health_drives_offline_count` is **absent** (the exporter suppresses a zero-valued gauge); with one drive settled offline, `minio_cluster_health_drives_offline_count = 1` and `minio_cluster_health_drives_online_count = 11`. A crucial finding: the count gauges measure **connectivity** and are refreshed on a ~10-second cache, so a freshly reconnected drive returns to `online` immediately while its data is still being healed. The actual heal-in-progress signal is the separate `minio_cluster_health_erasure_set_healing_drives` gauge, observed transitioning `0 -> 1 -> 0` around a heal.

### Code reference

- **v3 names** — `cmd/metrics-v3-cluster-health.go:L23-L25` (`drivesOfflineCount`, `drivesOnlineCount`, `drivesCount`); gauge descriptors via `NewGaugeMD` at `L29,L31,L33`; values set with `m.Set` at `L44-L46`.
- **v3 zero-suppression** — `MetricValues.Set` has a **pointer receiver** `func (m *MetricValues) Set` at `cmd/metrics-v3-types.go:L212`, and the load path guards with `if value > 0` at `cmd/metrics-v3-types.go:L240`, which is why a zero `offline_count` is omitted.
- **v3 path** — `clusterHealthCollectorPath = "/cluster/health"` at `cmd/metrics-v3.go:L50`, registered at `cmd/metrics-v3.go:L240`.
- **v2 source (correction)** — the v2 drive gauges are emitted by **`getClusterStorageMetrics` at `cmd/metrics-v2.go:L3794`** (online/offline counts derived at `L3805-L3806`, appended at `L3829,L3834,L3839`), using descriptor helpers `getClusterDrivesOfflineTotalMD` (`L578`), `getClusterDrivesOnlineTotalMD` (`L588`), `getClusterDrivesTotalMD` (`L598`); namespace `clusterMetricNamespace = "minio_cluster"` at `cmd/metrics-v2.go:L130`. The separate `getClusterHealthMetrics` at `cmd/metrics-v2.go:L3656` emits **different** gauges (write-quorum and the `erasure_set_*` health gauges, including `erasure_set_healing_drives`), not the drive counts.

### How the endpoints were scraped (executable; token in a shell variable, never printed)

Tokens were generated by `mc` into shell variables and passed to `curl` as Bearer tokens. The baseline scrape (all 12 online) includes the Prometheus `# HELP`/`# TYPE` descriptor lines; note the v3 output has **no** `offline_count` line at baseline (zero-suppressed):

```
===CMD: TOKV3=$(mc admin prometheus generate inv cluster --api-version v3 | awk '/bearer_token:/{print $2}')
===CMD: TOKV2=$(mc admin prometheus generate inv cluster | awk '/bearer_token:/{print $2}')
(tokens captured into shell variables; never printed)
token lengths (chars) for proof of capture: TOKV3=199 TOKV2=199

===CMD: curl -s -H "Authorization: Bearer $TOKV3" http://127.0.0.1:9000/minio/metrics/v3/cluster/health | grep '^minio_cluster_health_drives'
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12

===CMD: curl -s -H "Authorization: Bearer $TOKV2" http://127.0.0.1:9000/minio/v2/metrics/cluster | grep -E 'minio_cluster_drive_(online|offline)_total|minio_cluster_drive_total|erasure_set_healing_drives'
# HELP minio_cluster_drive_offline_total Total drives offline in this cluster
# TYPE minio_cluster_drive_offline_total gauge
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
# HELP minio_cluster_drive_online_total Total drives online in this cluster
# TYPE minio_cluster_drive_online_total gauge
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
# HELP minio_cluster_drive_total Total drives in this cluster
# TYPE minio_cluster_drive_total gauge
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
# HELP minio_cluster_health_erasure_set_healing_drives Get the count of healing drives of this erasure set
# TYPE minio_cluster_health_erasure_set_healing_drives gauge
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
```

### Values — the full failure/recovery lifecycle (connectivity vs. heal status)

To separate connectivity from heal progress, a larger (~10 GB) dataset was staged so the heal would overlap the 10-second gauge cache long enough to sample. The five-point capture around a fresh-drive replacement (each point shows `mc admin info`, the v3 drive gauges, and the v2 drive totals + the `erasure_set_healing_drives` gauge). The v3 `drives_online_count` stays at `12` throughout — a reconnected drive is "online" for connectivity the moment it is reachable — while the `erasure_set_healing_drives` gauge moves `0 -> 1 -> 0`; the heal completion line (`healed: 175`) is captured at POINT 5:

```
===== POINT: 1-BASELINE  (2026-07-13T19:23:37.206Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

===== POINT: 2-DRIVE-OFFLINE  (2026-07-13T19:23:38.315Z) =====
--- mc admin info ---
11 drives online, 1 drive offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

trigger (fresh d12) at: 2026-07-13T19:23:38.361Z
===== POINT: 3-AFTER-RECONNECT(pre/early-heal)  (2026-07-13T19:23:41.373Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

===== POINT: 4-DURING-HEAL  (2026-07-13T19:23:57.620Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 1
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

===== POINT: 4b-DURING-HEAL  (2026-07-13T19:24:01.671Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 1
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

===== POINT: 4c-DURING-HEAL  (2026-07-13T19:24:05.720Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 1
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12

heal finish observed at: 2026-07-13T19:24:57.397Z
finish line: Healing of drive '/tmp/minio-investigation/drives/d12' is finished (healed: 175, skipped: 0).
===== POINT: 5-AFTER-HEAL-COMPLETE  (2026-07-13T19:24:59.407Z) =====
--- mc admin info ---
12 drives online, 0 drives offline, EC:4
--- v3 /minio/metrics/v3/cluster/health (drive gauges) ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_online_count 12
--- v2 /minio/v2/metrics/cluster (drive totals + healing gauge) ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12
minio_cluster_drive_total{server="127.0.0.1:9000"} 12
minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
minio_cluster_health_erasure_set_online_drives{pool="0",server="127.0.0.1:9000",set="0"} 12
```

Because the count gauges are cached, an immediate scrape right after taking a drive offline (POINT 2) still shows `12/0`. Waiting past the ~10-second cache makes the offline transition visible: the settled capture below shows the v3 `drives_offline_count` reach `1` and `drives_online_count` fall to `11`; it also shows that after restore the gauge lags (~60 s) before returning to `online=12`:

```
=== take d12 offline (non-destructive), then WAIT 14s for gauge cache to refresh ===
offline at: 2026-07-13T19:25:54.571Z
scrape at : 2026-07-13T19:26:08.579Z  (14s after offline)
--- mc admin info ---
11 drives online, 1 drive offline, EC:4
--- v3 drive gauges ---
minio_cluster_health_drives_count 12
minio_cluster_health_drives_offline_count 1
minio_cluster_health_drives_online_count 11
--- v2 drive totals ---
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 1
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 11
minio_cluster_drive_total{server="127.0.0.1:9000"} 12

=== restore d12, WAIT 14s, confirm gauge returns to 12/0 ===
scrape at : 2026-07-13T19:26:22.649Z  (14s after restore)
12 drives online, 0 drives offline, EC:4
minio_cluster_health_drives_count 12
minio_cluster_health_drives_offline_count 1
minio_cluster_health_drives_online_count 11
minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 1
minio_cluster_drive_online_total{server="127.0.0.1:9000"} 11
minio_cluster_drive_total{server="127.0.0.1:9000"} 12

=== FINAL: confirm gauge returns to 12/0 after longer settle (poll every 3s up to 45s) ===
[2026-07-13T19:26:43.548Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:26:46.565Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:26:49.583Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:26:52.599Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:26:55.617Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:26:58.635Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:27:01.653Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:27:04.670Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:27:07.688Z] minio_cluster_health_drives_offline_count 1 minio_cluster_health_drives_online_count 11
[2026-07-13T19:27:10.706Z] minio_cluster_health_drives_online_count 12
>>> offline_count gauge dropped (=0, not emitted) => back to online=12
```

A tight ~0.5-second-cadence sample (120 samples spanning `19:23:38.364Z` to `19:24:39.649Z`) pinpoints the heal-gauge transition, ~19.6 s after the fresh-drive trigger (`19:23:38.361Z`). The three contiguous samples straddling the flip — the gauge is `0` at `19:23:57.412Z` and `1` at the very next sample `19:23:57.927Z`:

```
[2026-07-13T19:23:56.898Z] minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0 minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12 minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
[2026-07-13T19:23:57.412Z] minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0 minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12 minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 0
[2026-07-13T19:23:57.927Z] minio_cluster_drive_offline_total{server="127.0.0.1:9000"} 0 minio_cluster_drive_online_total{server="127.0.0.1:9000"} 12 minio_cluster_health_erasure_set_healing_drives{pool="0",server="127.0.0.1:9000",set="0"} 1
```

### Cause -> effect (OBJ-6)

The v3 collector's value setters (`m.Set`, `cmd/metrics-v3-cluster-health.go:L44-L46`) publish the current online/offline/total counts, but the counts reflect **connectivity** and are refreshed on a cache interval, so they revert to all-online as soon as a drive reconnects. Because `MetricValues.Set` guards with `if value > 0` (`cmd/metrics-v3-types.go:L240`), a zero offline count is omitted entirely. Heal *progress* is exposed separately as `erasure_set_healing_drives`, which is why it — not the count gauges — is the correct signal that a drive is actively being healed.

## Two-Run (and Three-Run) Stability

Timing- and magnitude-dependent results were confirmed across multiple identical runs:

| Result | Run 1 | Run 2 | Run 3 | Interpretation |
|---|---|---|---|---|
| Fresh-drive restore -> heal-complete latency (OBJ-3) | 11.2 s | 24.3 s | 12.2 s | Quantized to the 10 s `defaultMonitorNewDiskInterval`: ~1 cycle (H1/H3), ~2 cycles (H2) |
| Heal "use N parallel workers" (OBJ-5) | 4 | 4 | 4 | Stable; worker-count floor of 4 on a 4-CPU host |
| Write threshold (OBJ-1): offline drives to first failure | 6 | 6 | — | Stable `(12+1)/2 = 6`; 4 and 5 offline both succeed with parity `4->6` |
| Read threshold (OBJ-2): offline drives to first failure | 5 | 5 | — | Stable; fails once online < 8 data shards |
| v3 `drives_online_count` at baseline / 1 settled offline (OBJ-6) | 12 / 11 | 12 / 11 | — | Stable connectivity counts |

The write and read thresholds were re-observed with the same unchanged inputs and were identical each time. The heal latency varies only in whole 10-second poll cycles — the expected discretization for a fixed-interval monitor, not run-to-run nondeterminism.

## Observed vs. Inferred — Classification of Every Claim

| Claim / mechanism | Classification | Basis |
|---|---|---|
| Degraded write success + parity upgrade `4->6` (OBJ-1) | OBSERVED (canonical) | `mc`/SigV4 PUT at 4 & 5 offline; raw `xl-meta` `EcM:6 EcN:6`; marker decodes `4->6` |
| Write failure `SlowDownWrite` / HTTP 503 at 6 offline (OBJ-1) | OBSERVED (canonical) | Single-shot SigV4 full 503 XML; `mc` exit 1; threshold `(12+1)/2=6` |
| Read reconstruction at 4 offline (OBJ-2) | OBSERVED (canonical) | Reconstructed `obj1.bin` SHA-256 matches baseline; SigV4 GET 200 |
| Read failure `SlowDownRead` / HTTP 503 at 5 offline (OBJ-2) | OBSERVED (canonical) | Single-shot SigV4 full 503 XML on same object; `mc` exit 1 |
| Quorum strings never logged verbatim | OBSERVED | `grep -c` returns 0 for both strings; per-drive probe logged instead |
| `monitorLocalDisksAndHeal` -> `healFreshDisk` trigger (OBJ-3) | OBSERVED (canonical) | 3 timestamped runs; 11.2/24.3/12.2 s, quantized to 10 s poll |
| MRF on-the-fly heal path (OBJ-3) | OBSERVED STATE + SOURCE | `"mrf": null` queue-state field; enqueue site `cmd/erasure-object.go:L2113` |
| Background scanner heal (OBJ-3) | OBSERVED (indirect) | `ScannedItemsCount` advanced 245 -> 246 |
| `errFileNotFound` heal criterion (OBJ-4) | OBSERVED (canonical) | Fresh-drive heal repairs all objects; `objects_total_count:7` |
| `errPartMissingOrCorrupt` heal criterion (OBJ-4) | SIMULATED + SUPPLEMENTARY | Bitrot zeroing; canonical GET reconstructs; deep-scan (non-canonical) rebuilds shard |
| `errLegacyXLMeta` / `errOutdatedXLMeta` criteria (OBJ-4) | SOURCE-ONLY / INFERRED | Cited `cmd/erasure-healing.go:L161-L167`; not reproducible via canonical S3 |
| Heal log lines + "use 4 parallel workers" (OBJ-5) | OBSERVED (canonical) | `grep -n`/`grep -c`: 6 cycles, all 4 workers; per-cycle correlation |
| `healed:N` counts include system metadata (OBJ-4/5) | OBSERVED | `healed_buckets` = `.minio.sys/config`, `.minio.sys/buckets`, `ectest` |
| v3 / v2 drive-count metric names + values (OBJ-6) | OBSERVED (canonical) | `curl` scrapes at baseline (12/12) and 1 settled offline (11/1) |
| v3 zero-suppression of `offline_count` (OBJ-6) | OBSERVED | Absent at baseline; source guard `if value > 0` `cmd/metrics-v3-types.go:L240` |
| Count gauges = connectivity; `erasure_set_healing_drives` = heal status (OBJ-6) | OBSERVED (canonical) | Lifecycle capture: counts stay 12 while healing gauge `0 -> 1 -> 0` |
| `mc admin heal --scan deep` shard rebuild (OBJ-4) | SUPPLEMENTARY / NON-CANONICAL | Explicitly labeled; not the default drive-recovery trigger |

## Coverage Checklist

Every named mechanism, function, error, flag, and metric, with its concrete value, `file:line`, and evidence:

- [x] **OBJ-1 write outcome** — SUCCEEDS < 6 offline (parity `4->6`), FAILS >= 6 offline. `errErasureWriteQuorum` `cmd/erasure-errors.go:L25-L26` -> `ErrSlowDownWrite` HTTP 503 `cmd/api-errors.go:L874-L878,L2192-L2193`; decision `cmd/erasure-object.go:L1304-L1308`.
- [x] **`minIOErasureUpgraded` marker** — `x-minio-internal-erasure-upgraded = "4->6"` (base64 `NC0+Ng==`); written `cmd/erasure-object.go:L1316`; raw `xl-meta` shown.
- [x] **`objectQuorumFromMeta`** — `cmd/erasure-metadata.go:L531`.
- [x] **OBJ-2 read outcome** — reconstructs while online >= 8 data shards; FAILS at 5 offline. `errErasureReadQuorum` `cmd/erasure-errors.go:L22-L23` -> `ErrSlowDownRead` HTTP 503 `cmd/api-errors.go:L869-L873,L2190-L2191`; sites `cmd/erasure-object.go:L487,L738,L836`.
- [x] **OBJ-3 trigger** — `monitorLocalDisksAndHeal` (10 s, `cmd/background-newdisks-heal-ops.go:L40,L563`) -> `healFreshDisk` (`L419`); 3 timestamped runs.
- [x] **MRF path** — `addPartialOp` `cmd/mrf.go:L78`, `healRoutine` `cmd/mrf.go:L220`, enqueue `cmd/erasure-object.go:L2113`; observed `"mrf": null`.
- [x] **Scanner path** — `healObjectSelectProb=1024` `cmd/data-scanner.go:L61`, deep-scan `cmd/data-scanner.go:L93,L199`; `ScannedItemsCount` advanced.
- [x] **OBJ-4 criteria** — `shouldHealObjectOnDisk` `cmd/erasure-healing.go:L156-L183`: `errFileNotFound` (`L157-L159`, OBSERVED), `errPartMissingOrCorrupt` (`L169-L176`, SIMULATED+SUPPLEMENTARY), `errLegacyXLMeta` (`L161-L164`, SOURCE-ONLY), `errOutdatedXLMeta` (`L166-L167`, SOURCE-ONLY); `xMinIOHealing` `L186`; `healObject` `L258`.
- [x] **OBJ-5 logs** — `"use %d parallel workers."` `cmd/global-heal.go:L210` (= 4); status `cmd/background-newdisks-heal-ops.go:L460`; `"is finished (healed:..)"` `L520`; 6 cycles counted.
- [x] **OBJ-6 v3 metrics** — `minio_cluster_health_drives_{online,offline,count}` `cmd/metrics-v3-cluster-health.go:L23-L25,L44-L46`; path `cmd/metrics-v3.go:L50`; zero-suppression `cmd/metrics-v3-types.go:L212,L240`.
- [x] **OBJ-6 v2 metrics** — `minio_cluster_drive_{online,offline}_total`, `minio_cluster_drive_total` from `getClusterStorageMetrics` `cmd/metrics-v2.go:L3794` (append `L3829,L3834,L3839`; MD `L578,L588,L598`); namespace `cmd/metrics-v2.go:L130`; heal gauge from `getClusterHealthMetrics` `cmd/metrics-v2.go:L3656`.
- [x] **Before/during/after** — metrics 12/12 -> 11/1 settled; healing gauge `0->1->0`.
- [x] **Two/three-run stability** — thresholds re-observed identical; latency quantized to 10 s.
- [x] **Default parity `EC:4`** — `cmd/erasure-server-pool.go:L120-L124`; admin `sc_parity STANDARD:4`.
- [x] **Storage-class default** — availability-optimized `cmd/storage-class.go:L327`, `GetParityForSC`; confirmed by the `4->6` upgrade marker.

## Repository Cleanliness (final state)

All investigation artifacts are ephemeral and live entirely under `/tmp/minio-investigation` (the built binary, the `xl-meta` helper, the detached git worktree, the temporary drive directories, the server log, the captures, and the helper scripts); none are inside the destination repository. After the server was stopped, the detached build worktree removed, and the work root deleted, `git status` in the destination repository shows only the single deliverable document (`blitzy/documentation/minio_c07e5b49d477.md`); no build artifacts remain in the repo root, and `go.mod`/`go.sum` and every source file are unchanged. The full cleanup and verification transcript:

```
===CMD: stop the MinIO server by its exact PID (never pkill)
server.pid = 153069
server stopped cleanly

===CMD: confirm port 9000 no longer served
ready_http=000
ready_http=connection refused (server down)

===CMD: remove the detached build worktree registration
worktree removed
--- git worktree list after prune ---
/tmp/blitzy/minio/blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc_5ba4b2  6da7bf97b [blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc]

===CMD: remove the entire disposable work root (binary, xl-meta, drives, logs, captures, scripts)
removed /tmp/minio-investigation ; exists now? no

===CMD: git worktree list (only the main working tree remains)
/tmp/blitzy/minio/blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc_5ba4b2  6da7bf97b [blitzy-bfa49d51-b341-48e3-abc3-696af19a4ccc]

===CMD: confirm no build artifacts in repo root
no ./minio or ./xl-meta in repo root

===CMD: go.mod / go.sum are unchanged versus HEAD
go.mod+go.sum diff line count: 0

===CMD: final repository status (only the deliverable differs; nothing ignored/untracked left behind)
 M blitzy/documentation/minio_c07e5b49d477.md
```
