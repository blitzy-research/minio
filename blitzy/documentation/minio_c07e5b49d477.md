# Single-Node MinIO — The "First Bucket" Lifecycle, Observed End-to-End

This document answers, **from direct runtime observation** (not code reading alone), how a fresh
single-node MinIO deployment behaves when a newcomer runs it locally as a small S3-compatible object
store. Every behavioral claim below is backed by the **actual, unedited output** the server produced,
together with the exact command that produced it and a `file:line` citation naming the function,
method, or struct responsible.

**Scope.** Single node, single data directory, default credentials, default configuration — the
"typical local dev setup". A single directory selects the single‑drive erasure backend
(`xl-single` / `ErasureSDSetupType`, [`cmd/setup-type.go:24-38`]); there is no legacy filesystem
backend. All evidence was produced by a binary built from commit
`c07e5b49d477b0774f23db3b290745aef8c01bd2` (`DEVELOPMENT.2024-11-25T17-10-22Z`).

**Methodology (run‑first).** The server was built and launched, a per‑request trace subscriber was
attached, an AWS Signature V4 client drove the full object lifecycle, the on‑disk artifacts were
inspected, and the server was restarted on the same directory — capturing real output at each step.
The data directory (`/tmp/minio-obs-data`) and the temporary observation scripts and logs
(`/tmp/minio-obs`) lived **outside** the repository. The compiled `./minio` binary was written to the
**repository root** — the Makefile `build` target emits `-o $(PWD)/minio` ([`Makefile:179`]) — but it
is **gitignored** ([`.gitignore:4`], the single line `minio`) and was removed at cleanup, so the
working tree is left unchanged (see [Reproduction & cleanliness](#reproduction-commands--cleanliness-proof)).

**A note on two consoles.** MinIO's *default console* prints the startup banner, warnings, and errors
— but **not** a line for every successful request. The timestamped per‑request log lines the question
asks for come from the **trace pub/sub subsystem** (`mc admin trace`), which this investigation
attached explicitly. This is explained and proven in [Q5 & Q7](#q5--q7--timestamped-per-request-logs--the-requestpersist-lifecycle).

---

## Table of contents

- [Environment & canonical build](#environment--canonical-build)
- [Q1 — Starting the single-node server](#q1--starting-the-single-node-server)
- [Q2–Q4 — The first-bucket flow: status codes, headers, and bodies](#q2q4--the-first-bucket-flow-status-codes-headers-and-bodies)
- [Q5 & Q7 — Timestamped per-request logs & the request/persist lifecycle](#q5--q7--timestamped-per-request-logs--the-requestpersist-lifecycle)
- [Q6 — Authorization checks](#q6--authorization-checks)
- [Q8 — Filesystem artifacts](#q8--filesystem-artifacts)
- [Q9 — Restart persistence](#q9--restart-persistence)
- [Coverage](#coverage)
- [Reproduction commands & cleanliness proof](#reproduction-commands--cleanliness-proof)

---

## Environment & canonical build

**Toolchain.** Go `go1.23.12 linux/amd64`, matching the module declaration `go 1.23` ([`go.mod:3`]).

**Build command (canonical).** The binary was built with the Makefile `build` target
([`Makefile:177-179`]), reproduced verbatim:

```sh
make build
# build: checks build-debugging                          (Makefile:177)
#   @echo "Building minio binary to './minio'"            (Makefile:178)
#   @CGO_ENABLED=0 go build -tags kqueue -trimpath \
#       --ldflags "$(LDFLAGS)" -o $(PWD)/minio            (Makefile:179)
```

Observed build output:

```text
Checking dependencies
Building minio binary to './minio'
```

`LDFLAGS` is produced by `genLDFlags` ([`buildscripts/gen-ldflags.go:32-44`]). When invoked with no
argument, `main` sets `version = commitTime().Format(time.RFC3339)` ([`buildscripts/gen-ldflags.go:112-121`]),
where `commitTime()` reads the HEAD commit time via `git log --format=%cI -n1`
([`buildscripts/gen-ldflags.go:90-110`]); `genLDFlags` then stamps `CommitID` from `git log --format=%H -n1`
via `commitID()` ([`buildscripts/gen-ldflags.go:74-87`]). For the **investigated commit `c07e5b49d477`**
these stamps are:

```text
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z \
 -X github.com/minio/minio/cmd.CopyrightYear=2024 \
 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z \
 -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 \
 -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 \
 -X github.com/minio/minio/cmd.GOPATH= -X github.com/minio/minio/cmd.GOROOT=
```

> **Build note (canonical vs. current HEAD — labeled per reproducibility).** Because `gen-ldflags`
> reads the *current* `HEAD`, a bare `make build` on the branch tip stamps whatever commit is at the
> tip **at build time**, not the investigated commit. On this branch the investigated commit
> `c07e5b49d477` (committed `2024-11-25T17:10:22Z`) is the ancestor of one or more *documentation
> commits* that add and refine this markdown file; the branch tip is therefore always a documentation
> commit whose hash and commit-time shift as further doc/review commits are layered on top. A bare
> build consequently reports a `DEVELOPMENT.<tip-commit-time> (commit-id=<current-tip>)` string that
> tracks the current tip rather than `c07e5b49d477`. Since every documentation commit changes **only**
> this markdown file and **no** server code (`git diff --name-only c07e5b49d477 HEAD` →
> `blitzy/documentation/minio_c07e5b49d477.md`), runtime behavior is byte-for-byte identical to the
> investigated commit. To attribute every value below to the investigated commit as the AAP requires,
> the binary was built with the `c07e5b49d477` stamps pinned explicitly (the exact `-ldflags` shown
> above), which reproduces the canonical `--version` output that follows.

**Version banner (attributes all later evidence to this exact build).** Produced by `versionBanner`
([`cmd/main.go:185-193`]), which prints the ldflag-stamped `CommitID` and `CopyrightYear`:

```sh
$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

The `commit-id` matches the investigated commit exactly. (Note: `--version` prints
`Copyright: 2015-2024` from the ldflag `CopyrightYear=2024`; the *server startup* banner instead
prints the current year — see Q1 — because `startupBanner` overwrites `CopyrightYear` with
`time.Now().Year()` at [`cmd/main.go:179`].)

---

## Q1 — Starting the single-node server

**Direct answer.** `minio server <DIR>` on a single directory starts a single‑node server backed by
the single‑drive erasure backend (`xl-single`), authenticates with AWS Signature V4 using the default
root credentials `minioadmin:minioadmin`, and serves the S3 API on `:9000` and the web console on
`:9001`. On the *first* boot it formats the drive and prints
`Formatting 1st pool, 1 set(s), 1 drives per set.`

**Exact command used** (data directory deliberately outside the repository; explicit ports make the
console deterministic — the canonical README form is `minio server /data`, [`README.md:44-100`]):

```sh
export MINIO_ROOT_USER=minioadmin        # equals the built-in default DefaultAccessKey
export MINIO_ROOT_PASSWORD=minioadmin    # equals the built-in default DefaultSecretKey
./minio server /tmp/minio-obs-data --address :9000 --console-address :9001
```

`minioadmin`/`minioadmin` are the built‑in defaults `DefaultAccessKey`/`DefaultSecretKey`
([`internal/auth/credentials.go:90-91`]); setting the env vars to the same values keeps the run
canonical.

### Complete startup banner (verbatim)

Two forms were captured because part of the banner is **gated on whether stdout is a terminal**.

**(a) As captured when stdout is redirected to a log file** (the mode used to drive the flow).
`color.IsTerminal()` is defined as `return !color.NoColor` ([`internal/color/color.go:30-32`],
wrapping `github.com/fatih/color`); under a non‑TTY (and with the container's `TERM=dumb`) it is
`false`, so the colorized `RootUser:`/`RootPass:` lines are suppressed:

```text
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)

API: http://10.236.0.195:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
WebUI: http://10.236.0.195:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
INFO:
 You are running an older version of MinIO released 9 months before the latest release
 Update: Run `mc admin update ALIAS`
```

The trailing `INFO:` update notice is printed asynchronously by the version‑check goroutine; the
`9 months` figure is **volatile** (it is computed from the current date against the release date of
`DEVELOPMENT.2024-11-25T17-10-22Z`) and is reported here exactly as observed.

**(b) As a user sees it on a normal terminal** (captured under a pseudo‑TTY with
`TERM=xterm-256color`). The `RootUser:`/`RootPass:` lines and the `CLI:` line now appear:

```text
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
INFO:
 You are running an older version of MinIO released 9 months before the latest release
 Update: Run `mc admin update ALIAS`

MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)

API: http://10.236.0.195:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
   RootUser: minioadmin 
   RootPass: minioadmin 

WebUI: http://10.236.0.195:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 
   RootUser: minioadmin 
   RootPass: minioadmin 

CLI: https://min.io/docs/minio/linux/reference/minio-mc.html#quickstart
   $ mc alias set 'myminio' 'http://10.236.0.195:9000' 'minioadmin' 'minioadmin'

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

**How the banner is produced.**

- The `MinIO Object Storage Server` / `Copyright:` / `License:` / `Version:` block is `startupBanner`
  ([`cmd/main.go:178-183`]). It sets `CopyrightYear = strconv.Itoa(time.Now().Year())`
  ([`cmd/main.go:179`]) — which is why the *server* banner shows `Copyright: 2015-2026` (the current
  year at run time) whereas `--version` shows `2015-2024` (the ldflag value).
- The `API:` / `RootUser:` / `RootPass:` / `WebUI:` lines are `printServerCommonMsg`
  ([`cmd/server-startup-msg.go:114`]), invoked via `printStartupMessage` from `serverMain`
  ([`cmd/server-main.go:1118`]). The credential lines are gated by
  `color.IsTerminal() && (!Anonymous && !JSON && permitRootAccess())`
  ([`cmd/server-startup-msg.go:124`]); this is the observed cause of their absence in form (a).
- Banner text is emitted through `logger.Startup` ([`internal/logger/console.go:246`]).
- The default‑credentials `WARN` line is the **security indicator** for Q6: the default deployment
  ships with well‑known credentials and says so, verbatim, at every startup.

### Proof of the single-drive erasure backend

The **first‑boot** `Formatting 1st pool, 1 set(s), 1 drives per set.` line is emitted by `logger.Info`
at [`cmd/prepare-storage.go:194`], **gated** by `if shouldInitErasureDisks(sErrs) && firstDisk`
([`cmd/prepare-storage.go:193`]) — it prints only when the drive is unformatted. (Its absence on a
restart is the persistence proof in [Q9](#q9--restart-persistence).)

The on‑disk `format.json` records the backend type verbatim:

```sh
$ cat /tmp/minio-obs-data/.minio.sys/format.json
{"version":"1","format":"xl-single","id":"4760978f-66e6-402a-aced-7d3e8d6d27f5","xl":{"version":"3","this":"317a1a5f-9deb-44b6-be37-4ed81c88c664","sets":[["317a1a5f-9deb-44b6-be37-4ed81c88c664"]],"distributionAlgo":"SIPMOD+PARITY"}}
```

`"format":"xl-single"` corresponds to `ErasureSDSetupType` in the `SetupType` enum
([`cmd/setup-type.go:24-38`]). There is no legacy `fs` backend (`cmd/fs-v1.go` does not exist).

**Health check** confirming readiness:

```sh
$ curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/live
200
```

---

## Q2–Q4 — The first-bucket flow: status codes, headers, and bodies

**Direct answer.** The full lifecycle — **CreateBucket → PutObject×2 → ListObjectsV2 → GetObject** —
was driven over HTTP with an AWS Signature V4 client (boto3), plus `HeadObject`/`HeadBucket` to round
out the flow. **Every operation returned HTTP `200`.** CreateBucket, PutObject, HeadObject and
HeadBucket return **empty bodies** (PutObject returns its MD5 `ETag` in a header); ListObjectsV2
returns an XML `ListBucketResult` document; GetObject returns the **raw object bytes**.

The client was constructed as:

```python
import boto3
from botocore.config import Config
s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
                  aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin",
                  region_name="us-east-1", config=Config(signature_version="s3v4"))
```

### Per-operation status / headers / body summary

| Operation (call) | Handler [`file:line`] | Status | Body | Key headers |
|---|---|---|---|---|
| `create_bucket(Bucket="firstbucket")` | `PutBucketHandler` [`cmd/bucket-handlers.go:723`] | **200** | empty (`content-length: 0`) | `Location: /firstbucket`, `Server: MinIO` |
| `put_object("hello.txt", text/plain)` | `PutObjectHandler` [`cmd/object-handlers.go:1745`] | **200** | empty | `ETag: "f6caa783ea9b10ab201921ee607099ce"`, `x-amz-checksum-crc32: p/sylw==` |
| `put_object("data.json", application/json)` | `PutObjectHandler` [`cmd/object-handlers.go:1745`] | **200** | empty | `ETag: "44244ce1a15ee6d4dc270001564cb759"`, `x-amz-checksum-crc32: 1KkIfg==` |
| `list_objects_v2(Bucket="firstbucket")` | `ListObjectsV2Handler` [`cmd/bucket-listobjects-handlers.go:154`] | **200** | XML `ListBucketResult` (675 B) | `Content-Type: application/xml` |
| `get_object("hello.txt")` | `GetObjectHandler` [`cmd/object-handlers.go:715`] | **200** | raw bytes `Hello, MinIO!\n` (14 B) | `Content-Type: text/plain`, `ETag`, `Last-Modified` |
| `head_object("hello.txt")` | `HeadObjectHandler` [`cmd/object-handlers.go:1009`] | **200** | empty (headers only) | `Content-Length: 14`, `Content-Type: text/plain` |
| `head_bucket("firstbucket")` | `HeadBucketHandler` [`cmd/bucket-handlers.go:1644`] | **200** | empty | `Content-Type: application/xml` |

**Where the common headers come from.**

- `Server: MinIO` and `Accept-Ranges: bytes` are set by `setCommonHeaders`
  ([`cmd/api-headers.go:51`]); `MinioStoreName = "MinIO"` ([`cmd/build-constants.go:59`]); header
  names `Server`/`Accept-Ranges` at [`internal/http/headers.go:35,33`].
- `x-amz-request-id` (unique per request) is set by
  `w.Header().Set(xhttp.AmzRequestID, mustGetRequestID(UTCNow()))` in `addCustomHeadersMiddleware`
  ([`cmd/generic-handlers.go:548`]; header name at [`internal/http/headers.go:160`]).
- `X-Content-Type-Options: nosniff` and `Strict-Transport-Security: max-age=31536000; includeSubDomains`
  (HSTS), plus `X-Xss-Protection: 1; mode=block`, are set at [`cmd/generic-handlers.go:539-541`].
- `x-amz-id-2` (`X-Amz-Request-Host-Id`) is **constant per node** across all requests
  (observed value `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`), whereas
  `x-amz-request-id` differs on every request.
- **`x-amz-bucket-region` is absent** — by default MinIO uses an empty region, so `setCommonHeaders`
  does not set it ([`cmd/api-headers.go:56-58`], `if region := globalSite.Region(); region != ""`).
  This is a *negative* result, confirmed by its absence in every captured header set.
- `X-Ratelimit-Limit` / `X-Ratelimit-Remaining` (observed `1140789`) come from MinIO's API request
  limiter; reported here because the requirement is to show the **complete** header set.

**boto3 checksum note (reported honestly).** botocore 1.43.42 defaults to
`request_checksum_calculation = "when_supported"`, so it added `X-Amz-Sdk-Checksum-Algorithm: CRC32`
and `X-Amz-Checksum-Crc32: …` on the PUT *requests*; MinIO accepted them (HTTP 200) and **echoed**
`x-amz-checksum-crc32` on the PutObject/GetObject *responses*. This is the documented client behavior,
not a MinIO error.

### CreateBucket — full response (boto3 `ResponseMetadata`)

```text
HTTP status: 200
accept-ranges: bytes
content-length: 0
date: Wed, 08 Jul 2026 05:32:40 GMT
location: /firstbucket
server: MinIO
strict-transport-security: max-age=31536000; includeSubDomains
vary: Origin, Accept-Encoding
x-amz-id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
x-amz-request-id: 18C03993F0A44E9D
x-content-type-options: nosniff
x-ratelimit-limit: 1140789
x-ratelimit-remaining: 1140789
x-xss-protection: 1; mode=block
--- body: <empty> (content-length: 0) ---
```

CreateBucket's success writer is `writeSuccessResponseHeadersOnly` ([`cmd/bucket-handlers.go:880`] →
[`cmd/api-response.go:940`], which calls `writeResponse(w, http.StatusOK, nil, mimeNone)`) — hence the
`200` with a nil body.

### PutObject ×2 — full responses

`hello.txt` (`text/plain`, body `Hello, MinIO!\n`, 14 bytes):

```text
HTTP status: 200
accept-ranges: bytes
content-length: 0
date: Wed, 08 Jul 2026 05:32:40 GMT
etag: "f6caa783ea9b10ab201921ee607099ce"
server: MinIO
strict-transport-security: max-age=31536000; includeSubDomains
vary: Origin, Accept-Encoding
x-amz-checksum-crc32: p/sylw==
x-amz-id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
x-amz-request-id: 18C03993F38AFA3A
x-content-type-options: nosniff
x-ratelimit-limit: 1140789
x-ratelimit-remaining: 1140789
x-xss-protection: 1; mode=block
--- body: <empty> (content-length: 0) ---
```

`data.json` (`application/json`, body `{"k":"v"}`, 9 bytes):

```text
HTTP status: 200
accept-ranges: bytes
content-length: 0
date: Wed, 08 Jul 2026 05:32:40 GMT
etag: "44244ce1a15ee6d4dc270001564cb759"
server: MinIO
strict-transport-security: max-age=31536000; includeSubDomains
vary: Origin, Accept-Encoding
x-amz-checksum-crc32: 1KkIfg==
x-amz-id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
x-amz-request-id: 18C03993F3CC9958
x-content-type-options: nosniff
x-ratelimit-limit: 1140789
x-ratelimit-remaining: 1140789
x-xss-protection: 1; mode=block
--- body: <empty> (content-length: 0) ---
```

The `ETag` is a header (set in `setPutObjHeaders`, [`cmd/object-handlers-common.go:355`]) and the body
is empty (final writer `writeSuccessResponseHeadersOnly`, [`cmd/object-handlers.go:2128`]). Each ETag
is the **MD5 of the payload**, confirmed independently:

```text
md5("Hello, MinIO!\n") = f6caa783ea9b10ab201921ee607099ce   == ETag "f6caa783ea9b10ab201921ee607099ce"
md5('{"k":"v"}')       = 44244ce1a15ee6d4dc270001564cb759   == ETag "44244ce1a15ee6d4dc270001564cb759"
```

### ListObjectsV2 — full response and raw XML body

boto3's client sends `list-type=2&encoding-type=url`; the raw wire response body is **675 bytes** of
`application/xml`:

```text
HTTP status: 200
Accept-Ranges: bytes
Content-Length: 675
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C03993F5228A13
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140789
X-Ratelimit-Remaining: 1140789
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:32:40 GMT
```

Raw XML body (verbatim, as emitted; `&#34;` is the XML entity for the double-quote wrapping each ETag):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>firstbucket</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>data.json</Key><LastModified>2026-07-08T05:32:40.052Z</LastModified><ETag>&#34;44244ce1a15ee6d4dc270001564cb759&#34;</ETag><Size>9</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-07-08T05:32:40.048Z</LastModified><ETag>&#34;f6caa783ea9b10ab201921ee607099ce&#34;</ETag><Size>14</Size><StorageClass>STANDARD</StorageClass></Contents><EncodingType>url</EncodingType></ListBucketResult>
```

Pretty-printed for readability (same bytes):

```xml
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>firstbucket</Name>
  <Prefix/>
  <KeyCount>2</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>data.json</Key>
    <LastModified>2026-07-08T05:32:40.052Z</LastModified>
    <ETag>"44244ce1a15ee6d4dc270001564cb759"</ETag>
    <Size>9</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
  <Contents>
    <Key>hello.txt</Key>
    <LastModified>2026-07-08T05:32:40.048Z</LastModified>
    <ETag>"f6caa783ea9b10ab201921ee607099ce"</ETag>
    <Size>14</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
  <EncodingType>url</EncodingType>
</ListBucketResult>
```

A plain `GET /firstbucket?list-type=2` (no `encoding-type`) returns the **same document minus the
`<EncodingType>url</EncodingType>` element** — exactly **643 bytes** (a 32‑byte difference, the length
of that element). The body is built by `generateListObjectsV2Response` ([`cmd/api-response.go:684`])
and written by `writeSuccessResponseXML` ([`cmd/bucket-listobjects-handlers.go:227`] →
[`cmd/api-response.go:925`], `mimeXML`).

### GetObject — full response and raw bytes

```text
HTTP status: 200
Accept-Ranges: bytes
Content-Length: 14
Content-Type: text/plain
ETag: "f6caa783ea9b10ab201921ee607099ce"
Last-Modified: Wed, 08 Jul 2026 05:32:40 GMT
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C03993F60484AF
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140789
X-Ratelimit-Remaining: 1140789
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:32:40 GMT
--- body (14 bytes) ---
Hello, MinIO!
```

On the non‑ranged success path MinIO does **not** call `WriteHeader` explicitly: `getObjectHandler`
([`cmd/object-handlers.go:312`], reached via the exported `GetObjectHandler` [`cmd/object-handlers.go:715`])
streams the object with `xioutil.Copy(httpWriter, gr)` ([`cmd/object-handlers.go:550`]), so Go's
`net/http` writes the implicit **`200`** on the first body byte (`statusCodeWritten` stays `false`,
[`cmd/object-handlers.go:542`]). Only a **ranged**/part request takes the explicit branch
`w.WriteHeader(http.StatusPartialContent)` (guarded by `if rs != nil || opts.PartNumber > 0`,
[`cmd/object-handlers.go:544-546`]), which is why a ranged GET yields `206` instead. The returned
bytes are **byte‑for‑byte identical** to what was uploaded:

```text
uploaded  = b'Hello, MinIO!\n'
downloaded = b'Hello, MinIO!\n'
sha256(downloaded) = fe177b7059b6529542eef0c184bb71b629c46830d5442a4e5bfc4c242fca266e
equals_uploaded = True
```

### HeadObject & HeadBucket — full responses

Both round out the flow and both return **`200`** with an empty body; they are shown in full here so
every operation named in the summary table has its complete, unedited response.

**`HEAD /firstbucket/hello.txt`** (`head_object`) — exported `HeadObjectHandler`
([`cmd/object-handlers.go:1009`]) delegates to the internal `headObjectHandler`
([`cmd/object-handlers.go:744`]), which sets the object headers via `setObjectHeaders`
([`cmd/object-handlers.go:950`]) and `setHeadGetRespHeaders` ([`cmd/object-handlers.go:961`]) and then
calls `w.WriteHeader(http.StatusOK)` ([`cmd/object-handlers.go:967`]) — a headers‑only `200` (the same
`Content-Length: 14` / `Content-Type: text/plain` / `ETag` / `Last-Modified` as GetObject, but no body):

```text
HTTP status: 200
accept-ranges: bytes
content-length: 14
content-type: text/plain
date: Wed, 08 Jul 2026 05:32:40 GMT
etag: "f6caa783ea9b10ab201921ee607099ce"
last-modified: Wed, 08 Jul 2026 05:32:40 GMT
server: MinIO
strict-transport-security: max-age=31536000; includeSubDomains
vary: Origin, Accept-Encoding
x-amz-id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
x-amz-request-id: 18C03993F6598558
x-content-type-options: nosniff
x-ratelimit-limit: 1140789
x-ratelimit-remaining: 1140789
x-xss-protection: 1; mode=block
--- body: <empty> (headers only) ---
```

**`HEAD /firstbucket`** (`head_bucket`) — `HeadBucketHandler` ([`cmd/bucket-handlers.go:1644`]), routed
via `s3APIMiddleware(api.HeadBucketHandler)` ([`cmd/api-router.go:565`]). Its success writer is
`writeResponse(w, http.StatusOK, nil, mimeXML)` ([`cmd/bucket-handlers.go:1670`]) — hence the `200`
carries `Content-Type: application/xml` and `Content-Length: 0` with an **empty body** (a bucket
existence/authorization probe):

```text
HTTP status: 200
accept-ranges: bytes
content-length: 0
content-type: application/xml
date: Wed, 08 Jul 2026 05:32:40 GMT
server: MinIO
strict-transport-security: max-age=31536000; includeSubDomains
vary: Origin, Accept-Encoding
x-amz-id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
x-amz-request-id: 18C03993F6814830
x-content-type-options: nosniff
x-ratelimit-limit: 1140789
x-ratelimit-remaining: 1140789
x-xss-protection: 1; mode=block
--- body: <empty> ---
```


---

## Q5 & Q7 — Timestamped per-request logs & the request/persist lifecycle

**Direct answer.** The **default console does not print a line for each successful request.** During
the entire flow above, the server's stdout log stayed at its startup size. The timestamped
per‑request log lines the question asks for come from MinIO's **trace pub/sub subsystem**, which was
subscribed to with `mc admin trace -v` **before** any traffic was driven. Each S3 request produces a
`[REQUEST …]` line (request received), a `[RESPONSE …]` line (operation completed, with duration), and
byte counters `↑ rx / ↓ tx` (data written/read); the `--all` variant additionally surfaces the
`[STORAGE …]` and `[OS …]` disk operations that persist the data.

### The default console is silent on per-request activity (proof)

After the full flow **and** the authorization tests the server's own log
(`/tmp/minio-obs/server.run1.log`) still held only its **startup banner** — the flow added **zero**
per‑request lines. The copy measured below was captured after the Q9 restart test, so its final
non‑empty line is the `SIGTERM` shutdown message (`INFO: Exiting on signal: TERMINATED`, see
[Q9](#q9--restart-persistence)); every other line is the original 15‑line banner, and the `grep` for
per‑request markers returns **0**:

```sh
$ wc -l /tmp/minio-obs/server.run1.log
18 /tmp/minio-obs/server.run1.log
$ grep -cE 'PutBucket|PutObject|ListObjects|GetObject|REQUEST|RESPONSE|firstbucket' /tmp/minio-obs/server.run1.log
0
```

**Why (causal, code‑grounded).** Audit logging is the mechanism that would record per‑request events,
and it is **gated off** when no audit target is configured: `AuditLog` returns immediately if
`len(auditTgts) == 0` ([`internal/logger/audit.go:63-66`]):

```go
func AuditLog(ctx context.Context, w http.ResponseWriter, r *http.Request, reqClaims map[string]interface{}, filterKeys ...string) {
	auditTgts := AuditTargets()   // internal/logger/audit.go:64
	if len(auditTgts) == 0 {      // internal/logger/audit.go:65
		return                    // internal/logger/audit.go:66 — early return when no audit target is configured
	}
```

HTTP logger webhooks are likewise disabled by default. Per‑request telemetry is instead published to
subscribers: `httpTracerMiddleware` builds a `madmin.TraceInfo` and calls `globalTrace.Publish`
([`cmd/http-tracer.go:69`], publish at [`cmd/http-tracer.go:172`]); the subscription is served at
`GET /minio/admin/v3/trace` ([`cmd/admin-router.go:410`] → `TraceHandler`
[`cmd/admin-handlers.go:2032`]). Operation names are sanitized by `getOpName`
([`cmd/http-tracer.go:49`], which strips the `github.com/minio/minio/cmd.` prefix and `Handler-fm`
suffix and maps `objectAPIHandlers` → `s3`) — hence `s3.PutBucket`, `s3.PutObject`, etc.

### The trace subscriber

```sh
mc alias set localobs http://127.0.0.1:9000 minioadmin minioadmin   # → "Added `localobs` successfully."
mc admin trace -v localobs      > /tmp/minio-obs/trace.run1.log     # verbose: full req/resp headers + bodies
mc admin trace -v --all localobs > /tmp/minio-obs/trace.all.run1.log # + OS/STORAGE disk operations
```

`mc` is external tooling (not added to the repository). The trace line format includes the node
address, a `[REQUEST s3.<Op>]`/`[RESPONSE]` marker, an ISO‑8601 timestamp with millisecond precision,
the client IP, and — on the response — `Duration`, `TTFB`, and `↑ rx / ↓ tx` byte counts.

### Timestamped lines mapped to (a) received / (b) completed / (c) data written/read

Summary of the flow (verbatim `[REQUEST]`/`[RESPONSE]` lines from `trace.run1.log`):

```text
127.0.0.1:9000 [REQUEST s3.PutBucket] [2026-07-08T05:32:39.998] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.044] [ Duration 45.081ms TTFB 45.029442ms ↑ 131 B  ↓ 0 B ]
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-08T05:32:40.047] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.050] [ Duration 2.57ms TTFB 2.540334ms ↑ 215 B  ↓ 0 B ]
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-08T05:32:40.051] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.054] [ Duration 2.49ms TTFB 2.469035ms ↑ 210 B  ↓ 0 B ]
127.0.0.1:9000 [REQUEST s3.ListObjectsV2] [2026-07-08T05:32:40.065] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.066] [ Duration 799µs TTFB 769.354µs ↑ 131 B  ↓ 675 B ]
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-08T05:32:40.089] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.089] [ Duration 591µs TTFB 540.433µs ↑ 82 B  ↓ 14 B ]
127.0.0.1:9000 [REQUEST s3.HeadObject] [2026-07-08T05:32:40.094] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.095] [ Duration 421µs TTFB 396.104µs ↑ 131 B  ↓ 0 B ]
127.0.0.1:9000 [REQUEST s3.HeadBucket] [2026-07-08T05:32:40.097] [Client IP: 127.0.0.1]
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.097] [ Duration 141µs TTFB 123.95µs ↑ 131 B  ↓ 0 B ]
```

- **(a) request received** = the `[REQUEST s3.<Op>] [<timestamp>]` line. Example — PutObject
  `hello.txt` received at `2026-07-08T05:32:40.047`.
- **(b) operation completing** = the `[RESPONSE] [<timestamp>] [ Duration … TTFB … ]` line. Example —
  the same PutObject completed at `2026-07-08T05:32:40.050`, `Duration 2.57ms`.
- **(c) data written/read** = the `↑ rx / ↓ tx` counters. PutObject shows `↑ 215 B ↓ 0 B` (the 14‑byte
  body plus request headers uploaded; empty response body); **GetObject shows `↑ 82 B ↓ 14 B`** — the
  `↓ 14 B` is exactly the object read back. ListObjectsV2 shows `↓ 675 B` (the XML body).

> **What `↑ rx` counts — and why its exact value is client‑header dependent.** The trace's `↑ rx` is
> not the object payload; it is `reqRecorder.Size()` (the request *body* bytes) plus, for every request
> header, `len(name) + len(values)` — the header *name* length plus the *number* of values in that
> header (not the value's byte length) — after the tracer re‑adds `Host` and `Content-Length` to the
> cloned header set ([`cmd/http-tracer.go:110-113`], with `Host`/`Content-Length` re‑added at `:104`/`:106`).
> Because it sums header‑*name* lengths and value *counts*, `↑ rx` scales with the **number** of request
> headers the client sends, so its exact value is client‑header‑composition dependent. The `↑ 82 B` here
> is the minimal **six‑header** signed GET shown in the verbose block below (`Host`, `Accept-Encoding`,
> `Authorization`, `Content-Length`, `X-Amz-Content-Sha256`, `X-Amz-Date`): name lengths
> `4+15+13+14+20+10 = 76`, plus one per header `= 82`. A **default boto3** client sends four additional
> headers (`User-Agent`, `X-Amz-Checksum-Mode`, `Amz-Sdk-Invocation-Id`, `Amz-Sdk-Request`) — ten headers
> total — yielding an observed `↑ 151 B` on the *identical* 14‑byte object (`mc admin trace -v`, same
> `hello.txt`). The data‑read quantity Q7 asks about is `↓ tx` — the response‑body bytes, `↓ 14 B` here —
> which equals the object size exactly and is **stable** across both header compositions.

Full verbose block for **PutObject `hello.txt`** (request received → operation completed), showing the
request body as `<BLOB>` and the empty response body:

```text
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-08T05:32:40.047] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /firstbucket/hello.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Sdk-Checksum-Algorithm: CRC32
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=2774490739616091289deaa69d1685d220d44bc4d58561380850cb2b41038d72
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 X-Amz-Checksum-Crc32: p/sylw==
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 427d6ae7-1694-4d98-aaea-ce237d86098c
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 14
127.0.0.1:9000 User-Agent: Boto3/1.43.42 md/Botocore#1.43.42 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/Z,D,b,N,U,e cfg/retry-mode#legacy Botocore/1.43.42
127.0.0.1:9000 X-Amz-Content-Sha256: fe177b7059b6529542eef0c184bb71b629c46830d5442a4e5bfc4c242fca266e
127.0.0.1:9000 X-Amz-Date: 20260708T053240Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.050] [ Duration 2.57ms TTFB 2.540334ms ↑ 215 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Checksum-Crc32: p/sylw==
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140789
127.0.0.1:9000 X-Ratelimit-Remaining: 1140789
127.0.0.1:9000 ETag: "f6caa783ea9b10ab201921ee607099ce"
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C03993F38AFA3A
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <BLOB>
```

Full verbose block for **GetObject** (note the response `↓ 14 B` and the `200 OK`):

```text
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-08T05:32:40.089] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /firstbucket/hello.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=2b3c185e772f3ce08010f04d26d5d66fde4f040d35c390881e7c7833165c69f4
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260708T053240Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-08T05:32:40.089] [ Duration 591µs TTFB 540.433µs ↑ 82 B  ↓ 14 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C03993F60484AF
127.0.0.1:9000 X-Ratelimit-Limit: 1140789
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 14
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 ETag: "f6caa783ea9b10ab201921ee607099ce"
127.0.0.1:9000 Last-Modified: Wed, 08 Jul 2026 05:32:40 GMT
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1140789
127.0.0.1:9000 <BLOB>
```

### How data is actually persisted (observed disk operations)

Because this is a single node, the object layer calls the local `xl-storage` driver **directly**
(there is no storage REST hop to trace). The `mc admin trace -v --all` stream nonetheless exposes the
`[STORAGE …]` and `[OS …]` operations with timestamps. Measured over the capture window that spans
the S3 flow (the subscriber was attached before traffic and killed immediately after HeadBucket), it
recorded exactly **72** disk‑trace lines with this histogram (counts are for that flow window and are
sensitive to the exact window; startup‑time disk activity before the window is excluded):

```text
# grep -oE '\[(STORAGE storage|OS os)\.[A-Za-z]+' trace.all.run1.log | sort | uniq -c | sort -rn
     15 [OS os.Mkdir]
     13 [OS os.OpenFileR]
      9 [OS os.Rename]
      6 [OS os.Access]
      4 [OS os.OpenFileW]
      3 [OS os.OpenFileRFd]
      3 [OS os.Remove]
      2 [OS os.Lstat]
      3 [STORAGE storage.WalkDir]
      3 [STORAGE storage.RenameData]
      3 [STORAGE storage.ReadXL]
      3 [STORAGE storage.DiskInfo]
      3 [STORAGE storage.Delete]
      1 [STORAGE storage.StatVol]
      1 [STORAGE storage.MakeVol]
```

The verbatim disk operations for the three writes and the read, with timestamps:

```text
# CreateBucket → make the bucket directory (MakeVol), then persist bucket metadata (.metadata.bin)
[OS os.Mkdir] [2026-07-08T05:32:39.999] /tmp/minio-obs-data/firstbucket 6.460462ms
[STORAGE storage.MakeVol] [2026-07-08T05:32:39.999] /tmp/minio-obs-data firstbucket total-errs-availability=0 total-errs-timeout=0 6.501804ms
[OS os.OpenFileR] [2026-07-08T05:32:40.005] /tmp/minio-obs-data/.minio.sys/buckets/firstbucket/.metadata.bin/xl.meta 24.591µs
[OS os.Mkdir] [2026-07-08T05:32:40.005] /tmp/minio-obs-data/.minio.sys/tmp/d4549828-4f8f-4f64-9152-7b1f78a97df5 37.68µs
[OS os.OpenFileW] [2026-07-08T05:32:40.006] /tmp/minio-obs-data/.minio.sys/tmp/d4549828-4f8f-4f64-9152-7b1f78a97df5/xl.meta 31.465µs
[STORAGE storage.RenameData] [2026-07-08T05:32:40.005] /tmp/minio-obs-data d4549828-4f8f-4f64-9152-7b1f78a97df5 248d3db8-a1e6-4f7a-96f5-7ce85a0fb805 .minio.sys buckets/firstbucket/.metadata.bin total-errs-availability=0 total-errs-timeout=0 34.889937ms
[OS os.Rename] [2026-07-08T05:32:40.040] /tmp/minio-obs-data/.minio.sys/tmp/d4549828-4f8f-4f64-9152-7b1f78a97df5/xl.meta -> /tmp/minio-obs-data/.minio.sys/buckets/firstbucket/.metadata.bin/xl.meta 638.604µs

# PutObject hello.txt → write xl.meta into a temp dir, then RenameData atomically into place
[OS os.OpenFileR] [2026-07-08T05:32:40.048] /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta 21.773µs
[OS os.Mkdir] [2026-07-08T05:32:40.048] /tmp/minio-obs-data/.minio.sys/tmp/879b9a8e-d134-4d5b-bd40-048c04f381c0 41.338µs
[OS os.OpenFileW] [2026-07-08T05:32:40.048] /tmp/minio-obs-data/.minio.sys/tmp/879b9a8e-d134-4d5b-bd40-048c04f381c0/xl.meta 28.916µs
[OS os.Mkdir] [2026-07-08T05:32:40.049] /tmp/minio-obs-data/firstbucket/hello.txt 684.059µs
[STORAGE storage.RenameData] [2026-07-08T05:32:40.048] /tmp/minio-obs-data 879b9a8e-d134-4d5b-bd40-048c04f381c0 c85bfc51-ce9d-47c8-adcd-69436eaa1cc2 firstbucket hello.txt total-errs-timeout=0 total-errs-availability=0 1.983072ms
[OS os.Rename] [2026-07-08T05:32:40.050] /tmp/minio-obs-data/.minio.sys/tmp/879b9a8e-d134-4d5b-bd40-048c04f381c0/xl.meta -> /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta 27.134µs

# PutObject data.json → same temp→final commit
[OS os.OpenFileR] [2026-07-08T05:32:40.052] /tmp/minio-obs-data/firstbucket/data.json/xl.meta 18.99µs
[OS os.Mkdir] [2026-07-08T05:32:40.052] /tmp/minio-obs-data/.minio.sys/tmp/6c3ebfb1-079f-4661-bf32-84d7c44d543d 39.041µs
[OS os.OpenFileW] [2026-07-08T05:32:40.052] /tmp/minio-obs-data/.minio.sys/tmp/6c3ebfb1-079f-4661-bf32-84d7c44d543d/xl.meta 28.12µs
[OS os.Mkdir] [2026-07-08T05:32:40.054] /tmp/minio-obs-data/firstbucket/data.json 32.566µs
[STORAGE storage.RenameData] [2026-07-08T05:32:40.052] /tmp/minio-obs-data 6c3ebfb1-079f-4661-bf32-84d7c44d543d ab392fb2-0504-4090-b7bb-02c40f175f6d firstbucket data.json total-errs-availability=0 total-errs-timeout=0 2.006162ms
[OS os.Rename] [2026-07-08T05:32:40.054] /tmp/minio-obs-data/.minio.sys/tmp/6c3ebfb1-079f-4661-bf32-84d7c44d543d/xl.meta -> /tmp/minio-obs-data/firstbucket/data.json/xl.meta 27.054µs

# GetObject hello.txt → open the metadata file (which carries the inline data) and read it back
[OS os.OpenFileR] [2026-07-08T05:32:40.089] /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta 27.202µs
[STORAGE storage.ReadXL] [2026-07-08T05:32:40.089] /tmp/minio-obs-data firstbucket hello.txt total-errs-availability=0 total-errs-timeout=0 61.777µs 457 B
```

This maps to the write path in the code: object‑layer entry `MakeBucket`
([`cmd/erasure-server-pool.go:852`]); physical drive I/O in `xl-storage.go` — `MakeVol`
([`cmd/xl-storage.go:925`]), `CreateFile` ([`cmd/xl-storage.go:2099`]), `writeAllMeta`
([`cmd/xl-storage.go:2219`]), `WriteMetadata` ([`cmd/xl-storage.go:1471`]), and `RenameData`
(temp `.minio.sys/tmp` → final, [`cmd/xl-storage.go:2564`]). Each `[STORAGE storage.RenameData]` line
carries the **source temp data‑dir UUID** and the **destination data‑dir UUID** (for `hello.txt`:
`879b9a8e-d134-4d5b-bd40-048c04f381c0` → `c85bfc51-ce9d-47c8-adcd-69436eaa1cc2`), and the paired
`os.Rename … .minio.sys/tmp/<uuid>/xl.meta -> firstbucket/<key>/xl.meta` is exactly that final commit
of the metadata file. The read path is the single `os.OpenFileR` + `storage.ReadXL` on
`firstbucket/hello.txt/xl.meta` (`457 B` — the whole object, inline).


---

## Q6 — Authorization checks

**Direct answer.** The default local setup authenticates with **AWS Signature V4**. A correctly signed
request using the root credentials succeeds (HTTP `200`); a request signed with the **wrong secret**
is rejected with **HTTP `403` `SignatureDoesNotMatch`**; an **anonymous/unsigned** request is rejected
with **HTTP `403` `AccessDenied`**. There is no anonymous access unless a bucket/object policy grants
it (none does by default).

**Auth code path (success and failure share it).** `checkRequestAuthType`
([`cmd/auth-handler.go:339`]) → `checkRequestAuthTypeCredential` ([`cmd/auth-handler.go:523`]) →
`isReqAuthenticated` ([`cmd/auth-handler.go:560`]) → SigV4 verification. The startup banner's
`WARN: Detected default credentials 'minioadmin:minioadmin' …` (see Q1) is the runtime **success‑side
security indicator**: the server is reachable only with those SigV4 credentials until they are changed.

### Success indicator (valid SigV4)

Every operation in [Q2–Q4](#q2q4--the-first-bucket-flow-status-codes-headers-and-bodies) returned
`200 OK`, and the corresponding trace lines show the `s3.<Op>` with no error — e.g.
`[REQUEST s3.PutObject] … [RESPONSE] … ↑ 215 B ↓ 0 B` then `200 OK`. That is the observed success
indicator: a valid `AWS4-HMAC-SHA256` `Authorization` header (visible in the trace request block)
yielding a `2xx`.

### Failure 1 — wrong secret → `403 SignatureDoesNotMatch`

Command: a signed `GET /firstbucket?list-type=2` using the correct access key `minioadmin` but the
**wrong secret** `WRONGSECRET`. Observed raw response (complete, verbatim):

```text
HTTP status: 403
Accept-Ranges: bytes
Content-Length: 409
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C03993F6F0107C
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140789
X-Ratelimit-Remaining: 1140789
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:32:40 GMT
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message><BucketName>firstbucket</BucketName><Resource>/firstbucket</Resource><RequestId>18C03993F6F0107C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

This matches the definition of `ErrSignatureDoesNotMatch` verbatim
([`cmd/api-errors.go:704-707`]): `Code: "SignatureDoesNotMatch"`,
`Description: "The request signature we calculated does not match the signature you provided. Check
your key and signing method."`, `HTTPStatusCode: http.StatusForbidden`.

### Failure 2 — anonymous / unsigned → `403 AccessDenied`

Command: an entirely unsigned request via `curl`. Observed raw response (list bucket):

```sh
$ curl -sS -i http://127.0.0.1:9000/firstbucket/
HTTP/1.1 403 Forbidden
Accept-Ranges: bytes
Content-Length: 302
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C039A4E4BD02A6
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140789
X-Ratelimit-Remaining: 1140789
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:33:52 GMT
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>firstbucket</BucketName><Resource>/firstbucket/</Resource><RequestId>18C039A4E4BD02A6</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

And an unsigned object GET:

```sh
$ curl -sS -i http://127.0.0.1:9000/firstbucket/hello.txt
HTTP/1.1 403 Forbidden
Accept-Ranges: bytes
Content-Length: 331
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C039A4E52F4CB2
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140789
X-Ratelimit-Remaining: 1140789
X-Xss-Protection: 1; mode=block
Date: Wed, 08 Jul 2026 05:33:52 GMT
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>hello.txt</Key><BucketName>firstbucket</BucketName><Resource>/firstbucket/hello.txt</Resource><RequestId>18C039A4E52F4CB2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Both match `ErrAccessDenied` verbatim ([`cmd/api-errors.go:539-542`]): `Code: "AccessDenied"`,
`Description: "Access Denied."`, `HTTPStatusCode: http.StatusForbidden`. In the `--all` trace these
anonymous requests appear as `s3.ListObjectsV1` (`↓ 302 B`) and `s3.GetObject` (`↓ 331 B`) — the tx
byte counts equal the two error‑body sizes above, confirming the correlation.

---

## Q8 — Filesystem artifacts

**Direct answer.** The bucket is a **directory** named after the bucket; each object is a
**directory** named after the key, containing a single `xl.meta` file; and system state lives under
`.minio.sys/`. For these small objects the payload is stored **inline** inside `xl.meta` (no separate
`part.1` file). All artifacts are inside the out‑of‑repo data directory `/tmp/minio-obs-data`.

### Directory tree after CreateBucket + two PutObjects

```sh
$ find /tmp/minio-obs-data | sort
/tmp/minio-obs-data
/tmp/minio-obs-data/.minio.sys
/tmp/minio-obs-data/.minio.sys/buckets
/tmp/minio-obs-data/.minio.sys/buckets/.bloomcycle.bin
/tmp/minio-obs-data/.minio.sys/buckets/.bloomcycle.bin/xl.meta
/tmp/minio-obs-data/.minio.sys/buckets/.usage-cache.bin
/tmp/minio-obs-data/.minio.sys/buckets/.usage-cache.bin.bkp
/tmp/minio-obs-data/.minio.sys/buckets/.usage-cache.bin.bkp/xl.meta
/tmp/minio-obs-data/.minio.sys/buckets/.usage-cache.bin/xl.meta
/tmp/minio-obs-data/.minio.sys/buckets/.usage.json
/tmp/minio-obs-data/.minio.sys/buckets/.usage.json/xl.meta
/tmp/minio-obs-data/.minio.sys/buckets/firstbucket
/tmp/minio-obs-data/.minio.sys/buckets/firstbucket/.metadata.bin
/tmp/minio-obs-data/.minio.sys/buckets/firstbucket/.metadata.bin/xl.meta
/tmp/minio-obs-data/.minio.sys/config
/tmp/minio-obs-data/.minio.sys/config/config.json
/tmp/minio-obs-data/.minio.sys/config/config.json/xl.meta
/tmp/minio-obs-data/.minio.sys/config/iam
/tmp/minio-obs-data/.minio.sys/config/iam/format.json
/tmp/minio-obs-data/.minio.sys/config/iam/format.json/xl.meta
/tmp/minio-obs-data/.minio.sys/format.json
/tmp/minio-obs-data/.minio.sys/multipart
/tmp/minio-obs-data/.minio.sys/pool.bin
/tmp/minio-obs-data/.minio.sys/pool.bin/xl.meta
/tmp/minio-obs-data/.minio.sys/tmp
/tmp/minio-obs-data/.minio.sys/tmp/.trash
/tmp/minio-obs-data/firstbucket
/tmp/minio-obs-data/firstbucket/data.json
/tmp/minio-obs-data/firstbucket/data.json/xl.meta
/tmp/minio-obs-data/firstbucket/hello.txt
/tmp/minio-obs-data/firstbucket/hello.txt/xl.meta
```

Key artifacts:

- **Bucket** → `firstbucket/` (a directory).
- **Objects** → `firstbucket/hello.txt/xl.meta` (457 B) and `firstbucket/data.json/xl.meta` (458 B).
  There are **no** `part.*` files (`find … -type f ! -name xl.meta` → 0), confirming inline storage.
- **System tree** under `.minio.sys/`: `format.json` (the `xl-single` backend descriptor);
  `pool.bin/xl.meta`; per‑bucket metadata `buckets/firstbucket/.metadata.bin/xl.meta`;
  `config/config.json/xl.meta`; `config/iam/format.json/xl.meta`; and the write staging /
  garbage areas `tmp/` and `tmp/.trash`. The names are the constants `minioMetaBucket = ".minio.sys"`
  ([`cmd/object-api-utils.go:60`]) and `minioMetaTmpBucket = ".minio.sys/tmp"`
  ([`cmd/object-api-utils.go:66`]); `bucketMetaPrefix = "buckets"` ([`cmd/object-api-common.go:40`]).

### `xl.meta` magic and version

```sh
$ head -c 16 /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta | od -A d -t x1z
0000000 58 4c 32 20 01 00 03 00 c6 00 00 01 80 03 02 01  >XL2 ............<
0000016
```

The first four bytes are `58 4c 32 20` = `XL2 ` — the file magic
`xlHeader = [4]byte{'X','L','2',' '}` ([`cmd/xl-storage-format-v2.go:44`]). The next four bytes
`01 00 03 00` are the little‑endian format version (major `1`, minor `3`); `xlVersionMajor = 1`
([`cmd/xl-storage-format-v2.go:59`]).

### Decoded `xl.meta` — proving inline storage

Using the in‑repo decoder (`docs/debugging/xl-meta`, whose `main` is at
[`docs/debugging/xl-meta/main.go:49`], built to `/tmp/minio-obs/xl-meta`):

```sh
$ go build -o /tmp/minio-obs/xl-meta ./docs/debugging/xl-meta
$ /tmp/minio-obs/xl-meta /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta
```
```json
{
    "Versions": [
        {
            "Header": {
                "EcM": 1,
                "EcN": 0,
                "Flags": 6,
                "ModTime": "2026-07-08T05:32:40.048059361Z",
                "Signature": "a09c8233",
                "Type": 1,
                "VersionID": "00000000000000000000000000000000"
            },
            "Idx": 0,
            "Metadata": {
                "Type": 1,
                "V2Obj": {
                    "CSumAlgo": 1,
                    "DDir": "yFv8Uc6dR8itzWlDbqocwg==",
                    "EcAlgo": 1,
                    "EcBSize": 1048576,
                    "EcDist": [
                        1
                    ],
                    "EcIndex": 1,
                    "EcM": 1,
                    "EcN": 0,
                    "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
                    "MTime": 1783488760048059361,
                    "MetaSys": {
                        "x-minio-internal-crc": "CKf7Mpc=",
                        "x-minio-internal-inline-data": "dHJ1ZQ=="
                    },
                    "MetaUsr": {
                        "content-type": "text/plain",
                        "etag": "f6caa783ea9b10ab201921ee607099ce"
                    },
                    "PartASizes": [
                        14
                    ],
                    "PartETags": null,
                    "PartNums": [
                        1
                    ],
                    "PartSizes": [
                        14
                    ],
                    "Size": 14
                },
                "v": 1732554622
            }
        }
    ]
}
```

Two things prove inline storage:

1. `MetaSys."x-minio-internal-inline-data"` = `"dHJ1ZQ=="`, whose base64 decode is **`true`**.
2. `EcM: 1, EcN: 0` (one data shard, zero parity — the single‑drive layout), `Size: 14`,
   `MetaUsr.etag` = the MD5 seen in the HTTP response.

The `-data` flag emits the **actual inlined bytes**, byte‑identical to the uploads:

```sh
$ /tmp/minio-obs/xl-meta -data /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta
{ "null": { "bitrot_valid": true, "bytes": 46, "data_base64": "SGVsbG8sIE1pbklPIQo=", "data_string": "Hello, MinIO!\n" } }

$ /tmp/minio-obs/xl-meta -data /tmp/minio-obs-data/firstbucket/data.json/xl.meta
{ "null": { "bitrot_valid": true, "bytes": 41, "data_base64": "eyJrIjoidiJ9", "data_string": "{\"k\":\"v\"}" } }
```

The objects (14 B and 9 B) are inlined because they are far below the default inline threshold of
**128 KiB** returned by `InlineBlock()` ([`internal/config/storageclass/storage-class.go:304`],
`return 128 * humanize.KiByte`). Larger objects would instead be written as separate `part.N` files.


---

## Q9 — Restart persistence

**Direct answer.** **Yes** — the bucket and both objects remain fully accessible after restarting the
server on the same directory. The proof is two‑fold: (1) the restart banner **omits** the
`Formatting …` line (the existing `format.json` is reused rather than re‑initialized), and (2)
re‑listing and re‑downloading return **identical ETags and identical bytes**.

### Before the restart (baseline)

```text
BEFORE list keys: [('data.json', 9, '"44244ce1a15ee6d4dc270001564cb759"'),
                   ('hello.txt', 14, '"f6caa783ea9b10ab201921ee607099ce"')]
BEFORE get hello.txt: b'Hello, MinIO!\n'  etag "f6caa783ea9b10ab201921ee607099ce"
                      sha256 fe177b7059b6529542eef0c184bb71b629c46830d5442a4e5bfc4c242fca266e
BEFORE get data.json: b'{"k":"v"}'        etag "44244ce1a15ee6d4dc270001564cb759"
```

The server was then stopped with `SIGTERM`; it shut down gracefully, appending one line to its log:

```text
INFO: Exiting on signal: TERMINATED
```

### Restart on the same directory

```sh
./minio server /tmp/minio-obs-data --address :9000 --console-address :9001 > /tmp/minio-obs/server.run2.log 2>&1 &
```

**Run 2 banner (verbatim, complete `server.run2.log` — 15 lines) — note the absent `Formatting` line:**

```text
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)

API: http://10.236.0.195:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
WebUI: http://10.236.0.195:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
INFO:
 You are running an older version of MinIO released 9 months before the latest release
 Update: Run `mc admin update ALIAS`
```

The first boot's `Formatting …`/`WARNING: Host local …` pair (run‑1 log lines 1–2) is **gone**; the
restart begins directly at the `MinIO Object Storage Server` line. The two trailing blank lines
(run‑2 lines 14–15) are the same spacing the version‑check goroutine leaves in run 1.

`diff` of the run‑1 vs run‑2 logs makes the omission unmistakable (`<` lines are present only in run 1):

```diff
1,2d0
< INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
< INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
18d15
< INFO: Exiting on signal: TERMINATED
```

Grep counts confirm it numerically:

```sh
$ grep -c 'Formatting 1st pool' /tmp/minio-obs/server.run1.log   # 1  (first boot)
$ grep -c 'Formatting 1st pool' /tmp/minio-obs/server.run2.log   # 0  (reused format.json)
```

**Causal reason.** The `Formatting …` line is gated by `shouldInitErasureDisks(sErrs) && firstDisk`
([`cmd/prepare-storage.go:193`]). On the second boot the drive is already formatted, so
`shouldInitErasureDisks` is false and the branch is skipped — the server reuses the existing
`.minio.sys/format.json` (unchanged `xl-single` descriptor) instead of re‑initializing it.

### After the restart (verified twice for stability)

```text
AFTER run1: KeyCount=2 keys=[('data.json', 9, '"44244ce1a15ee6d4dc270001564cb759"'),
                             ('hello.txt', 14, '"f6caa783ea9b10ab201921ee607099ce"')]
AFTER run1: hello.txt=b'Hello, MinIO!\n' etag "f6caa783ea9b10ab201921ee607099ce"
            sha256 fe177b7059b6529542eef0c184bb71b629c46830d5442a4e5bfc4c242fca266e
AFTER run1: data.json=b'{"k":"v"}'       etag "44244ce1a15ee6d4dc270001564cb759"
AFTER run2: KeyCount=2 keys=[('data.json', 9, '"44244ce1a15ee6d4dc270001564cb759"'),
                             ('hello.txt', 14, '"f6caa783ea9b10ab201921ee607099ce"')]
AFTER run2: hello.txt=b'Hello, MinIO!\n' etag "f6caa783ea9b10ab201921ee607099ce"
            sha256 fe177b7059b6529542eef0c184bb71b629c46830d5442a4e5bfc4c242fca266e
AFTER run2: data.json=b'{"k":"v"}'       etag "44244ce1a15ee6d4dc270001564cb759"
```

Before‑vs‑after equality check:

```text
hello.txt: before="f6caa783ea9b10ab201921ee607099ce" after="f6caa783ea9b10ab201921ee607099ce" IDENTICAL=True
data.json: before="44244ce1a15ee6d4dc270001564cb759" after="44244ce1a15ee6d4dc270001564cb759" IDENTICAL=True
```

**Stable vs volatile values.** The **persistent** values (object list, sizes, ETags, and downloaded
bytes) were identical across both post‑restart reads. The **volatile** values, as expected, are the
per‑request `x-amz-request-id`, the trace timestamps, the SigV4 signatures, and the internal object
data‑directory UUID (`DDir`) — none of which affect object identity or content.

---

## Coverage

Every distinct item the question asks for, mapped to the observed evidence and the code that produces
it.

| # | Question item | Observed answer | Evidence (command → where) | Key `file:line` |
|---|---|---|---|---|
| Q1 | Stand up a single‑node server | Runs `xl-single` single‑drive erasure, SigV4, creds `minioadmin:minioadmin`, API `:9000`/console `:9001`; first boot prints `Formatting 1st pool, 1 set(s), 1 drives per set.` | `./minio server /tmp/minio-obs-data …` → banner; `cat …/format.json` → `"format":"xl-single"` | `cmd/prepare-storage.go:194`, `cmd/setup-type.go:24-38`, `internal/auth/credentials.go:90-91`, `cmd/server-startup-msg.go:114` |
| Q2 | Full flow (create/upload×2/list/download) | CreateBucket, PutObject×2, ListObjectsV2, GetObject (+ HeadObject/HeadBucket) all `200` | boto3 SigV4 driver | handlers `cmd/bucket-handlers.go:723`, `cmd/object-handlers.go:1745,715,1009`, `cmd/bucket-listobjects-handlers.go:154` |
| Q3 | Exact status codes + full headers | All `200`; complete header sets captured per op; `Server: MinIO`, `x-amz-request-id` (unique), `x-amz-id-2` (constant), HSTS/nosniff/xss, ratelimit; **no** `x-amz-bucket-region` | boto3 `ResponseMetadata.HTTPHeaders` + raw wire captures | `cmd/api-headers.go:51,56-58`, `cmd/generic-handlers.go:539-541,548`, `internal/http/headers.go:33,35,160` |
| Q4 | Response bodies (bucket & object ops) | CreateBucket/PutObject/HeadObject/HeadBucket = empty; ListObjectsV2 = XML `ListBucketResult` (675/643 B); GetObject = raw 14 B | raw signed requests → `wire.*.txt` | `cmd/api-response.go:940,925,684`, `cmd/object-handlers.go:2128` |
| Q5 | Server‑side log messages **with timestamps** | Default console silent; `mc admin trace -v` shows `[REQUEST]`/`[RESPONSE]` lines with ISO‑8601 ms timestamps | `mc admin trace -v` → `trace.run1.log` | `cmd/http-tracer.go:49,69,172`, `cmd/admin-router.go:410`, `internal/logger/audit.go:63-66` |
| Q6 | Auth checks + exact error/success indicators | Valid SigV4 → `200`; wrong secret → `403 SignatureDoesNotMatch`; anonymous → `403 AccessDenied` (raw XML captured) | signed wrong‑secret; unsigned `curl` | `cmd/auth-handler.go:339,523,560`, `cmd/api-errors.go:704-707,539-542` |
| Q7 | Request received / completed / data written‑read | `[REQUEST]`=received; `[RESPONSE Duration…]`=completed; `↑ rx / ↓ tx`=bytes; `--all` shows `MakeVol`/`os.Rename` (write) & `os.OpenFileR` (read) | `trace.run1.log`, `trace.all.run1.log` | `cmd/http-tracer.go:69`, `cmd/erasure-server-pool.go:852`, `cmd/xl-storage.go:925,2099,2219,2564` |
| Q8 | Filesystem locations/artifacts | Bucket dir `firstbucket/`; per‑object `…/xl.meta` (`XL2 ` magic); inline data (`x-minio-internal-inline-data=true`); `.minio.sys/` tree | `find`; `od`; `xl-meta [-data]` | `cmd/xl-storage-format-v2.go:44,59`, `internal/config/storageclass/storage-class.go:304`, `cmd/object-api-utils.go:60,66` |
| Q9 | Restart persistence + evidence | **Yes**; run‑2 banner omits `Formatting`; identical ETags/bytes (verified twice) | stop/restart; before/after reads; `diff` | `cmd/prepare-storage.go:193` |

**Named sub‑items checklist:** status codes ✓ (all `200`, plus `403` on the two auth failures); full
response headers ✓ (per‑op, annotated to source); response bodies for bucket ops ✓ (empty / XML) and
object ops ✓ (empty / raw bytes); timestamped log lines ✓; access/authorization success indicator ✓
and exact error messages ✓ (`SignatureDoesNotMatch`, `AccessDenied`); request‑received / operation‑
completed / data‑written‑read lines ✓; filesystem locations/artifacts ✓; restart evidence ✓.

**Inferred (not directly observed) items, labeled as such:** the specific internal functions
`CreateFile`/`writeAllMeta`/`WriteMetadata`/`RenameData` are attributed from code reading; what was
*observed* at runtime is the equivalent `[STORAGE storage.MakeVol]`/`[STORAGE storage.RenameData]` and
`[OS os.Mkdir]`/`[OS os.Rename]`/`[OS os.OpenFileR]` trace events plus the resulting on‑disk `xl.meta`.

---

## Reproduction commands & cleanliness proof

The full sequence, runnable from the repository root (all runtime state lives outside the repo):

```sh
# 0. Toolchain
export PATH=/usr/local/go/bin:$PATH:/root/go/bin GOPATH=/root/go GOFLAGS=-mod=readonly

# 1. Build the binary (Makefile:177-179) and check the version. NOTE: gen-ldflags stamps the
#    version/commit from the CURRENT HEAD (see the build-stamp note in "Environment & canonical
#    build"); built at commit c07e5b49d477 this yields DEVELOPMENT.2024-11-25T17-10-22Z / c07e5b49d477.
make build
./minio --version

# 2. Create the OUT-OF-REPO working dirs, then start the single-node server (first boot formats it)
mkdir -p /tmp/minio-obs /tmp/minio-obs-data
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin
./minio server /tmp/minio-obs-data --address :9000 --console-address :9001 > /tmp/minio-obs/server.run1.log 2>&1 &
echo $! > /tmp/minio-obs/server.run1.pid       # capture the server PID for a clean shutdown later
sleep 1
cat /tmp/minio-obs-data/.minio.sys/format.json # "format":"xl-single"

# 3. Attach the per-request trace BEFORE driving traffic (capture PIDs so they can be stopped)
mc alias set localobs http://127.0.0.1:9000 minioadmin minioadmin
mc admin trace -v localobs        > /tmp/minio-obs/trace.run1.log      2>&1 & echo $! > /tmp/minio-obs/trace.s3.pid
mc admin trace -v --all localobs  > /tmp/minio-obs/trace.all.run1.log  2>&1 & echo $! > /tmp/minio-obs/trace.all.pid

# 4. Drive the S3 flow with a SigV4 client (boto3) — CreateBucket, PutObject x2, ListObjectsV2,
#    GetObject, HeadObject, HeadBucket (driver.py also issues the wrong-secret request of step 5a)
python3 /tmp/minio-obs/driver.py
kill "$(cat /tmp/minio-obs/trace.s3.pid)" "$(cat /tmp/minio-obs/trace.all.pid)"   # stop the subscribers

# 5. Authorization variants: valid (step 4) -> 200; wrong secret -> 403 SignatureDoesNotMatch;
#    anonymous/unsigned -> 403 AccessDenied.
#    5a. wrong secret: a SigV4-signed GET /firstbucket?list-type=2 with the correct access key but a
#        bad secret (exactly what driver.py:94 issues via botocore SigV4Auth):
python3 - <<'PY'
import boto3, botocore
from botocore.config import Config
c = boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
                 aws_access_key_id="minioadmin", aws_secret_access_key="WRONGSECRET",
                 region_name="us-east-1", config=Config(signature_version="s3v4"))
try:
    c.list_objects_v2(Bucket="firstbucket")
except botocore.exceptions.ClientError as e:
    print(e.response["Error"]["Code"], e.response["ResponseMetadata"]["HTTPStatusCode"])  # SignatureDoesNotMatch 403
PY
#    5b. anonymous (unsigned) list and object GET:
curl -sS -i http://127.0.0.1:9000/firstbucket/            # 403 AccessDenied (list)
curl -sS -i http://127.0.0.1:9000/firstbucket/hello.txt   # 403 AccessDenied (object)

# 6. Filesystem artifacts
find /tmp/minio-obs-data | sort
head -c 16 /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta | od -A d -t x1z   # 'XL2 '
go build -o /tmp/minio-obs/xl-meta ./docs/debugging/xl-meta
/tmp/minio-obs/xl-meta -data /tmp/minio-obs-data/firstbucket/hello.txt/xl.meta

# 7. Restart persistence — stop run 1, restart on the SAME directory, capture the new PID
kill "$(cat /tmp/minio-obs/server.run1.pid)"
./minio server /tmp/minio-obs-data --address :9000 --console-address :9001 > /tmp/minio-obs/server.run2.log 2>&1 &
echo $! > /tmp/minio-obs/server.run2.pid
diff /tmp/minio-obs/server.run1.log /tmp/minio-obs/server.run2.log   # run2 omits the Formatting line

# 8. Cleanup (leave the repository unchanged except this document)
kill "$(cat /tmp/minio-obs/server.run2.pid)"
rm -f ./minio                          # the compiled binary + debug helpers are gitignored (.gitignore:4)
rm -rf /tmp/minio-obs /tmp/minio-obs-data
git status --porcelain                 # must show ONLY the new documentation file
```

> Tooling note: `go`, `boto3`, and `mc` (the MinIO Client) are **environment tooling**, used only to
> build and observe the server. None of them — nor the compiled `./minio` binary (gitignored,
> [`.gitignore:4`]) — is added to the repository.

### Cleanliness proof (observed after cleanup)

After stopping the server and trace subscribers and removing the compiled binary, the out‑of‑repo data
directory (`/tmp/minio-obs-data`), and the temporary scripts/logs (`/tmp/minio-obs`), the working tree
is unchanged except for this one document. The compiled `./minio` binary and the debug helpers
(`xl-meta`, `s3-check-md5`, `healing-bin`, …) produced by `make build`'s `build-debugging` step are all
matched by `.gitignore` ([`.gitignore:4`] = `minio`; `**/*.test` and the per‑tool names) and therefore
never appear as changes:

```sh
$ git rev-parse --abbrev-ref HEAD
blitzy-a3b4c827-6890-4de0-8ae7-e19b209d24aa
$ git rev-parse HEAD
c07e5b49d477b0774f23db3b290745aef8c01bd2

$ git status --porcelain
?? blitzy/

$ git status --porcelain -uall
?? blitzy/documentation/minio_c07e5b49d477.md
```

The default `git status --porcelain` collapses the freshly‑created `blitzy/` directory to a single
`?? blitzy/` entry (Git reports the new directory rather than descending into it); the expanded
`-uall` form makes the single added file explicit. The empty sibling directories `blitzy/screenshots/`
and `blitzy/screen_recordings/` are not shown because Git does not track empty directories. This
confirms the repository is left unchanged apart from `blitzy/documentation/minio_c07e5b49d477.md`.

This snapshot is taken against the **investigated baseline commit** `c07e5b49d477`, the commit that
precedes the documentation commit(s) on this branch (see the build note in
[Environment & canonical build](#environment--canonical-build)); measured from that commit the
document is a brand‑new *untracked* addition, exactly matching the AAP's "one new file, everything
else read‑only" contract. On the branch itself the same single file is instead recorded by one or
more documentation commits layered on top of `c07e5b49d477`, so measured against the branch tip the
working tree is clean. Either way, no pre‑existing source, config, build, or test file is touched.

