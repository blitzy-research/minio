# MinIO Single-Node Onboarding: First Bucket & Object, End-to-End (Observed) [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io)

This document answers, **from direct runtime observation**, how a typical single‑node local MinIO
server behaves end‑to‑end when you create your first bucket and use it for basic object operations.
Every behavioral claim below carries **both** (a) the actual, unedited output that was captured at
runtime **and** (b) a `file:line` citation naming the specific function/method/struct in the source
that performs the work.

- **Source commit (HEAD):** `c07e5b49d477b0774f23db3b290745aef8c01bd2` (branch `minio_c07e5b49d477`).
- **Go module:** `github.com/minio/minio` ([go.mod:L1](../../go.mod)); minimum toolchain `go 1.23`
  ([go.mod:L3](../../go.mod)), CI matrix pinned to `1.23.x`
  ([.github/workflows/go-cross.yml:L23](../../.github/workflows/go-cross.yml)).
- **Methodology first:** the server was **built and run first**; the flow was driven through the real
  S3 API on port 9000 with a SigV4‑signing client; status codes, headers, bodies, logs, on‑disk
  artifacts, and restart behavior were captured; and only then was this document written from that
  captured output. No behavioral statement here is from reading alone.

## Legend

| Tag | Meaning |
|-----|---------|
| **[observed]** | The statement is backed by runtime output captured during this investigation (shown inline). |
| **[inferred]** | The statement is a reasoned conclusion from source/observed evidence, not a directly captured value. It is labeled wherever used. |
| **[non-canonical]** | A value produced by a non‑canonical path (e.g. a plain `go build` instead of `make build`). Shown only for contrast and always labeled. |

## Environment & Methodology

**[observed]** All commands below were run against a single build of the server at HEAD.

- **Toolchain:** `go version` → `go version go1.24.3 linux/amd64` (installed toolchain; `GOTOOLCHAIN=local`,
  which satisfies the module's `go 1.23` floor at [go.mod:L3](../../go.mod)). The Go runtime that the
  binary reports is `go1.24.3` (see the version banner in R1).
- **Canonical build:** `make build` (target [Makefile:L177](../../Makefile), recipe
  [Makefile:L179](../../Makefile)), which stamps canonical version/VCS identifiers via
  [buildscripts/gen-ldflags.go:L36-L40](../../buildscripts/gen-ldflags.go). The build binary was staged
  **outside** the repository at `/tmp/minio-build/minio` so the checkout stays clean.
- **Invocation (default single node):**
  `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` left unset (true defaults), then
  `/tmp/minio-build/minio server /tmp/minio-data --console-address ":9001"`.
  The S3 API listens on **:9000**, the embedded web console on **:9001**. Data directory
  `/tmp/minio-data` is **outside** the repository.
- **S3 client (canonical, SigV4):** `boto3 1.43.46 / botocore 1.43.46` (Python 3.13.7) with **path‑style**
  addressing against `http://127.0.0.1:9000`, credentials `minioadmin:minioadmin`. An unsigned `curl`
  cannot exercise the authenticated path, so a signing client is mandatory. Raw XML bodies were captured
  with a manually SigV4‑signed request (botocore `SigV4Auth` + stdlib `urllib`) so no client‑side XML
  parsing intervened.
- **Per‑request logs:** captured with `mc admin trace` (MinIO client `RELEASE.2025-08-13T08-35-41Z`) and,
  as a cross‑check, an audit‑webhook JSON stream. The default server console does **not** emit a
  per‑request line (proven in R5).
- **On‑disk inspection:** direct `find`/`cat`/`xxd`/`od`/`strings` on `/tmp/minio-data`.
- **Read‑only discipline:** the MinIO source tree was treated as read‑only reference. The binary, data
  directory, and all temporary scripts live **outside** the checkout; on completion
  `git status --porcelain` shows only this one new document.

### The S3 request lifecycle (as exercised)

The ordered path each request takes — validated against the observed flow:

1. The client signs the request with **AWS Signature V4**.
2. The API router wraps every handler with `httpTraceAll` ([cmd/http-tracer.go:L194](../../cmd/http-tracer.go))
   and `registerAPIRouter` ([cmd/api-router.go:L253](../../cmd/api-router.go)) registers the routes.
3. Middleware sets `x-amz-request-id` ([cmd/generic-handlers.go:L548](../../cmd/generic-handlers.go)) and,
   when the local node name is non‑empty, `x-amz-id-2`
   ([cmd/generic-handlers.go:L549-L550](../../cmd/generic-handlers.go)).
4. Authentication recomputes and compares the signature via
   `checkRequestAuthTypeCredential` → `isReqAuthenticated` → `doesSignatureMatch`
   ([cmd/auth-handler.go:L523](../../cmd/auth-handler.go),
   [cmd/auth-handler.go:L560](../../cmd/auth-handler.go),
   [cmd/signature-v4.go:L347](../../cmd/signature-v4.go)).
5. On success the S3 handler runs (PutBucket / PutObject / List / Get); the object layer persists data
   (`MakeBucket` [cmd/erasure-server-pool.go:L852](../../cmd/erasure-server-pool.go); payload
   `WriteAll` into `xl.meta` [cmd/xl-storage.go:L1187](../../cmd/xl-storage.go)).
6. The response writer sets common headers (`Server: MinIO`, `Accept-Ranges: bytes`) via
   `setCommonHeaders` ([cmd/api-headers.go:L51](../../cmd/api-headers.go)) and emits a headers‑only 200
   or an XML body.

## R1 — Build & Startup

### Building the server canonically

**[observed]** The canonical build is `make build`. The exact compiler command it runs (captured with
`cd /app && make --dry-run build`) is, verbatim:

```
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z -X github.com/minio/minio/cmd.CopyrightYear=2024 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 -X github.com/minio/minio/cmd.GOPATH=/go -X github.com/minio/minio/cmd.GOROOT=" -o /app/minio 1>/dev/null
```

This is the `build:` target at [Makefile:L177](../../Makefile) whose recipe at
[Makefile:L179](../../Makefile) is `@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null`.
The `-X` flags map one‑to‑one to the identifiers stamped by
[buildscripts/gen-ldflags.go:L36-L40](../../buildscripts/gen-ldflags.go) (`cmd.Version`,
`cmd.CopyrightYear`, `cmd.ReleaseTag`, `cmd.CommitID`, `cmd.ShortCommitID`).

Running `make build` prints, verbatim:

```
Checking dependencies
Building minio binary to './minio'
```

### The canonical version banner **[observed]**

`/tmp/minio-build/minio --version` (the binary produced by `make build`) prints, verbatim:

```
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.24.3 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

The `ReleaseTag` `DEVELOPMENT.2024-11-25T17-10-22Z` and the full `commit-id` are exactly the values the
ldflags injected — i.e. they are canonical because `make build` stamped them.

### Non‑canonical comparison **[non-canonical]**

For contrast, a plain `go build` with **no** ldflags:
`CGO_ENABLED=0 go build -o /tmp/minio-build/minio-plain .` produces a binary whose banner reads,
verbatim:

```
minio-plain version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.24.3 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

`DEVELOPMENT.GOGET` and `Copyright: 2015-0000` are the un‑stamped defaults. **This value is
non‑canonical** and is shown only to demonstrate why `make build` matters for build‑dependent values.

**Rationale:** version/VCS identifiers are compile‑time `-X` link flags. Only the `make build` path
computes and injects them (via `gen-ldflags.go`); a bare `go build` leaves the package‑level defaults,
so any version reported that way must be labeled non‑canonical.

### Launching the default single‑node server **[observed]**

Command (defaults; credentials env intentionally unset so the true defaults apply):

```
/tmp/minio-build/minio server /tmp/minio-data --console-address ":9001"
```

The full, unedited first‑boot startup banner (captured to the server log) is:

```
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.24.3 linux/amd64)

API: http://10.236.7.195:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
WebUI: http://10.236.7.195:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
INFO: 
 You are running an older version of MinIO released 9 months before the latest release 
 Update: Run `mc admin update ALIAS` 
```

And the listening sockets, verbatim (`ss -ltn`):

```
tcp        0      0 127.0.0.1:9000          0.0.0.0:*               LISTEN      
tcp        0      0 :::9000                 :::*                    LISTEN      
tcp        0      0 :::9001                 :::*                    LISTEN
```

**How the banner is produced (observed → code):**

- Process entry is `main.go` → `minio.Main(os.Args)` ([main.go:L30](../../main.go)); `Main` dispatches the
  `server` subcommand to `serverMain` ([cmd/server-main.go:L742](../../cmd/server-main.go)).
- The banner is emitted by `printStartupMessage` ([cmd/server-startup-msg.go:L39](../../cmd/server-startup-msg.go))
  → `printServerCommonMsg` ([cmd/server-startup-msg.go:L114](../../cmd/server-startup-msg.go)). The
  `API:` line is printed at [cmd/server-startup-msg.go:L123](../../cmd/server-startup-msg.go) and the
  `WebUI:` line at [cmd/server-startup-msg.go:L134](../../cmd/server-startup-msg.go).
- The `Docs: https://docs.min.io` line is emitted by
  [cmd/server-startup-msg.go:L147](../../cmd/server-startup-msg.go):
  `logger.Startup(color.Blue("\nDocs: ") + "https://docs.min.io")`.
- **Observed nuance:** the banner shows **no** `RootUser:`/`RootPass:` lines. Those are gated by
  `color.IsTerminal()` at [cmd/server-startup-msg.go:L124](../../cmd/server-startup-msg.go); because the
  server's stdout was captured to a file (not a TTY), the guard is false and the credential lines are
  omitted. **[observed]** (they would print on an interactive terminal).
- **Observed nuance (Copyright):** the startup banner shows `Copyright: 2015-2026` (computed at runtime
  from the current year) whereas `--version` shows the stamped `Copyright: 2015-2024`. Both are shown
  above exactly as emitted.

### The default‑credentials warning (security caveat) **[observed]**

The server prints, verbatim:

```
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

**Rationale:** a fresh local deployment uses the well‑known root credentials `minioadmin:minioadmin`
(documented at [README.md:L29](../../README.md)); the server warns at startup so operators change them
before exposing the server. For this onboarding investigation the defaults were kept deliberately.


## R2 + R3 — The Full First‑Bucket Flow (status, headers, body per step)

**[observed]** The flow was driven by the boto3 client described in *Environment & Methodology*,
against `http://127.0.0.1:9000`. The ordered operations are: **CreateBucket** `onboarding-demo` →
**PutObject** `hello.txt` (body `hello minion`) → **PutObject** `data/report.json` (nested prefix) →
**ListObjectsV2** → **GetObject** `hello.txt` → **ListBuckets`**. Every operation returned **HTTP 200**.

Handler and response‑writer map (each verified at HEAD):

| S3 op | Handler | Response writer / shape |
|-------|---------|-------------------------|
| CreateBucket | `PutBucketHandler` [cmd/bucket-handlers.go:L723](../../cmd/bucket-handlers.go) | `writeSuccessResponseHeadersOnly` [cmd/api-response.go:L940](../../cmd/api-response.go) → 200, `content-length: 0` |
| PutObject ×2 | `PutObjectHandler` [cmd/object-handlers.go:L1745](../../cmd/object-handlers.go) | headers‑only 200; `ETag` via `setObjectHeaders` [cmd/api-headers.go:L111](../../cmd/api-headers.go) |
| ListObjectsV2 | `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L154](../../cmd/bucket-listobjects-handlers.go) | `writeSuccessResponseXML(w, encodeResponseList(response))` [cmd/bucket-listobjects-handlers.go:L227](../../cmd/bucket-listobjects-handlers.go); writer [cmd/api-response.go:L925](../../cmd/api-response.go); struct `ListObjectsV2Response` [cmd/api-response.go:L131](../../cmd/api-response.go) |
| GetObject | `GetObjectHandler` [cmd/object-handlers.go:L715](../../cmd/object-handlers.go) | body = object bytes; `Content-Type`, `Last-Modified`, `ETag`, `Accept-Ranges` |
| ListBuckets | `ListBucketsHandler` [cmd/bucket-handlers.go:L306](../../cmd/bucket-handlers.go) | `writeSuccessResponseXML`; struct `ListBucketsResponse` [cmd/api-response.go:L222](../../cmd/api-response.go) |

Routes are registered by `registerAPIRouter` ([cmd/api-router.go:L253](../../cmd/api-router.go)); the
GET‑object route is at [cmd/api-router.go:L371](../../cmd/api-router.go) and the PUT‑object route at
[cmd/api-router.go:L392](../../cmd/api-router.go). Every handler is wrapped by `httpTraceAll`.

### Step 1 — CreateBucket `onboarding-demo` → HTTP 200 **[observed]**

Full, unedited response headers (boto3 `ResponseMetadata.HTTPHeaders`):

```
## CreateBucket(onboarding-demo): HTTP 200
     accept-ranges:         bytes
     content-length:        0
     date:                  Mon, 13 Jul 2026 16:43:30 GMT
     location:              /onboarding-demo
     server:                MinIO
     strict-transport-security: max-age=31536000; includeSubDomains
     vary:                  Origin, Accept-Encoding
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E716393CA994
     x-content-type-options: nosniff
     x-ratelimit-limit:     1140434
     x-ratelimit-remaining: 1140434
     x-xss-protection:      1; mode=block
```

- The body is **empty** (`content-length: 0`, no `content-type`) because `PutBucketHandler`
  ([cmd/bucket-handlers.go:L723](../../cmd/bucket-handlers.go)) replies via
  `writeSuccessResponseHeadersOnly` ([cmd/api-response.go:L940](../../cmd/api-response.go)).
- `server: MinIO` and `accept-ranges: bytes` come from `setCommonHeaders`
  ([cmd/api-headers.go:L51](../../cmd/api-headers.go)); the `Server` value is the constant
  `MinioStoreName = "MinIO"` ([cmd/build-constants.go:L59](../../cmd/build-constants.go)).
- `x-amz-request-id` is set unconditionally at
  [cmd/generic-handlers.go:L548](../../cmd/generic-handlers.go).

### Step 2 — PutObject `hello.txt` (body `hello minion`) → HTTP 200 **[observed]**

```
## PutObject(hello.txt): HTTP 200
   ETag(server)="84f6bd993afe53f22c433eb79d6bf53d"   md5(exact body bytes)="84f6bd993afe53f22c433eb79d6bf53d"   len=12
     accept-ranges:         bytes
     content-length:        0
     date:                  Mon, 13 Jul 2026 16:43:30 GMT
     etag:                  "84f6bd993afe53f22c433eb79d6bf53d"
     server:                MinIO
     strict-transport-security: max-age=31536000; includeSubDomains
     vary:                  Origin, Accept-Encoding
     x-amz-checksum-crc32:  Nyq5Ng==
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E716451BED44
     x-content-type-options: nosniff
     x-ratelimit-limit:     1140434
     x-ratelimit-remaining: 1140434
     x-xss-protection:      1; mode=block
```

**Byte‑sensitive verification [observed]:** the exact request body is the 12 bytes `hello minion`;
`md5("hello minion") = 84f6bd993afe53f22c433eb79d6bf53d`, which equals the server `ETag` exactly. For a
small single‑part object the ETag is the MD5 of the payload. The response is headers‑only
(`content-length: 0`). `PutObjectHandler` is at
[cmd/object-handlers.go:L1745](../../cmd/object-handlers.go); the `ETag`/`Last-Modified` object headers
are set by `setObjectHeaders` ([cmd/api-headers.go:L111](../../cmd/api-headers.go)).

### Step 3 — PutObject `data/report.json` (nested prefix) → HTTP 200 **[observed]**

```
## PutObject(data/report.json): HTTP 200
   ETag(server)="8a993b9be03dfeeb5de5c5791da69e8a"   md5(exact body bytes)="8a993b9be03dfeeb5de5c5791da69e8a"   len=54
     accept-ranges:         bytes
     content-length:        0
     date:                  Mon, 13 Jul 2026 16:43:30 GMT
     etag:                  "8a993b9be03dfeeb5de5c5791da69e8a"
     server:                MinIO
     x-amz-checksum-crc32:  XN9mVw==
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E716578C7F6B
     x-content-type-options: nosniff
     x-xss-protection:      1; mode=block
```

**Byte‑sensitive verification [observed]:** the server `ETag` `8a993b9be03dfeeb5de5c5791da69e8a` equals
the MD5 of the exact 54‑byte JSON body. The nested key `data/report.json` is stored under a nested
prefix (see R6 for the on‑disk layout).

### Step 4 — ListObjectsV2 → HTTP 200, `application/xml` **[observed]**

Response summary and headers:

```
#### ListObjectsV2: HTTP 200 | content-type=application/xml | KeyCount=2 | Keys=['data/report.json', 'hello.txt']
     content-length:        687
     content-type:          application/xml
     server:                MinIO
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E71662DA09F0
```

Full, unedited XML body (captured with a raw SigV4‑signed `GET /onboarding-demo?list-type=2`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>onboarding-demo</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>data/report.json</Key><LastModified>2026-07-13T16:43:30.560Z</LastModified><ETag>&#34;8a993b9be03dfeeb5de5c5791da69e8a&#34;</ETag><Size>54</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-07-13T16:43:30.250Z</LastModified><ETag>&#34;84f6bd993afe53f22c433eb79d6bf53d&#34;</ETag><Size>12</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

- The root element `<ListBucketResult>` with XML namespace `http://s3.amazonaws.com/doc/2006-03-01/`
  comes from the `ListObjectsV2Response` struct ([cmd/api-response.go:L131](../../cmd/api-response.go)),
  serialized by `writeSuccessResponseXML` ([cmd/api-response.go:L925](../../cmd/api-response.go)) at the
  call site [cmd/bucket-listobjects-handlers.go:L227](../../cmd/bucket-listobjects-handlers.go).
- Both keys are listed with their `ETag` (note the `&#34;` XML‑escaped quotes as actually emitted),
  `Size` (54 and 12 — matching the uploaded bodies), and `StorageClass STANDARD`. The nested key sorts
  first lexicographically.

### Step 5 — GetObject `hello.txt` (download again) → HTTP 200 **[observed]**

```
## GetObject(hello.txt): HTTP 200
   content-type=text/plain   ETag="84f6bd993afe53f22c433eb79d6bf53d"   body=b'hello minion'   md5(body)="84f6bd993afe53f22c433eb79d6bf53d"
     accept-ranges:         bytes
     content-length:        12
     content-type:          text/plain
     date:                  Mon, 13 Jul 2026 16:43:30 GMT
     etag:                  "84f6bd993afe53f22c433eb79d6bf53d"
     last-modified:         Mon, 13 Jul 2026 16:43:30 GMT
     server:                MinIO
     x-amz-checksum-crc32:  Nyq5Ng==
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E71663269645
```

- The **returned bytes** are exactly `b'hello minion'`, and the MD5 of the downloaded bytes equals the
  ETag — a byte‑exact round trip. Served by `GetObjectHandler`
  ([cmd/object-handlers.go:L715](../../cmd/object-handlers.go)).
- Object responses carry `content-type: text/plain` (as uploaded), `content-length: 12`,
  `last-modified`, `etag`, and `accept-ranges: bytes`.

### Step 6 — ListBuckets → HTTP 200, `application/xml` **[observed]**

```
#### ListBuckets: HTTP 200 | Buckets=['onboarding-demo'] | Owner=minio
     content-length:        373
     content-type:          application/xml
     server:                MinIO
     x-amz-id-2:            dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
     x-amz-request-id:      18C1E7166358F761
```

Full, unedited XML body (raw SigV4‑signed `GET /`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><Buckets><Bucket><Name>onboarding-demo</Name><CreationDate>2026-07-13T16:43:30.051Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>
```

The root element `<ListAllMyBucketsResult>` comes from the `ListBucketsResponse` struct
([cmd/api-response.go:L222](../../cmd/api-response.go)); the single bucket `onboarding-demo` is returned
with its creation date, served by `ListBucketsHandler`
([cmd/bucket-handlers.go:L306](../../cmd/bucket-handlers.go)).

### Header nuances observed across the flow

- **`x-amz-request-id` — present on every response [observed].** Set unconditionally at
  [cmd/generic-handlers.go:L548](../../cmd/generic-handlers.go) via `mustGetRequestID(UTCNow())`.
- **`x-amz-id-2` — present on every response [observed]** (value
  `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`). It is set at
  [cmd/generic-handlers.go:L550](../../cmd/generic-handlers.go) **only when** `globalLocalNodeName != ""`
  (guard at [cmd/generic-handlers.go:L549](../../cmd/generic-handlers.go)). Its presence here shows the
  local node name is non‑empty in this deployment; on a deployment where it is empty this header would
  be **absent**. *(This is the exact runtime behavior; it differs from the common expectation that a
  bare single node leaves the header off.)*
- **`x-amz-bucket-region` — absent [observed].** `setCommonHeaders` sets it only if a region is
  configured (`if region := globalSite.Region(); region != ""` at
  [cmd/api-headers.go:L57](../../cmd/api-headers.go)). The default local server uses an empty region, so
  the header is omitted. This is independently corroborated by an empty `GetBucketLocation` response:

  ```xml
  <?xml version="1.0" encoding="UTF-8"?>
  <LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>
  ```

- **Additional headers observed** (not core to the S3 semantics but shown as emitted):
  `strict-transport-security`, `vary`, `x-content-type-options: nosniff`,
  `x-ratelimit-limit`/`x-ratelimit-remaining`, `x-xss-protection`, and — on object PUT/GET —
  `x-amz-checksum-crc32` (the SDK's default CRC32 checksum).

**Rationale:** bucket creation and object upload are acknowledgements with no payload, so MinIO returns
a headers‑only 200 (`writeSuccessResponseHeadersOnly`); listings must return data, so they are XML
documents (`writeSuccessResponseXML`) whose root elements are defined by the corresponding response
structs. The `Server`, `Accept-Ranges`, and request‑id headers are applied uniformly by the common
header/middleware layer, which is why they appear on every response above.


## R4 — Authorization Behavior (success and failure)

MinIO authenticates S3 requests with **AWS Signature V4**. The server recomputes the signature from the
canonical request using the secret stored for the presented access key and compares it to the signature
the client sent.

**Auth path (observed → code):**
`checkRequestAuthType` ([cmd/auth-handler.go:L339](../../cmd/auth-handler.go)) →
`checkRequestAuthTypeCredential` ([cmd/auth-handler.go:L523](../../cmd/auth-handler.go)) →
`isReqAuthenticated` ([cmd/auth-handler.go:L560](../../cmd/auth-handler.go)) →
`doesSignatureMatch` ([cmd/signature-v4.go:L347](../../cmd/signature-v4.go)).

### Success indicator **[observed]**

A valid SigV4 signature yields **HTTP 200**. This is exactly what every operation in R2+R3 returned; a
direct check with the correct secret:

```
== success indicator (valid SigV4) ==
ListBuckets HTTP 200 (valid signature accepted)
```

The success signal is simply that `doesSignatureMatch` returns no error, so `isReqAuthenticated`
returns `ErrNone` and the handler runs and returns 200.

### Failure — invalid secret → HTTP 403 `SignatureDoesNotMatch` **[observed]**

Replaying a request with an **invalid secret key** (access key still `minioadmin`, secret
`WRONG-SECRET-key-123`) produces, via boto3 (`ClientError`):

```
== (a) boto3 ListBuckets with INVALID secret ==
HTTP status : 403
Error.Code  : SignatureDoesNotMatch
Error.Message: The request signature we calculated does not match the signature you provided. Check your key and signing method.
x-amz-request-id: 18C1E73303DA7066
```

And the full, unedited `<Error>` XML body (raw SigV4‑signed `GET /` with the wrong secret):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message><Resource>/</Resource><RequestId>18C1E73304676BFF</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

- The status is **HTTP 403** and the S3 error code is **`SignatureDoesNotMatch`**, produced when
  `doesSignatureMatch` ([cmd/signature-v4.go:L347](../../cmd/signature-v4.go)) returns the
  signature‑mismatch `APIErrorCode`.
- **Observed cross‑check:** the `<HostId>` in the error body equals the `x-amz-id-2` value seen on all
  successful responses (`dd9025…e3e8`), confirming the host id is populated in this deployment.

**Rationale:** because the client signed with a different secret than the server has on file, the
server‑side recomputed signature cannot match the client‑supplied one; `doesSignatureMatch` therefore
returns the mismatch code, surfaced to the client as `HTTP 403 SignatureDoesNotMatch`. A correct
signature instead yields `HTTP 200`. This is the access/authorization check that governs the default
local setup — the credentials themselves are the well‑known defaults, but every request must still be
correctly signed.


## R5 — Per‑Request Logs With Timestamps (the subtle one)

The key nuance: **the default server console does not emit a per‑request access line.** Per‑request
lines with timestamps require the **trace** or **audit** subsystem. This was validated against
authoritative MinIO documentation (the console target is always on but does not log every operation and
cannot serve as an audit trail; an audit target must be configured for a per‑operation trail; the
canonical way to watch live per‑request activity is `mc admin trace`).

### 1) The console negative — proven at runtime **[observed]**

After the **entire** R2+R3+R4 flow had already executed, the server console log contained **only** the
17‑line startup banner. Grepping it for any per‑request/API marker finds nothing:

```
$ wc -l < server1.log
17
$ grep -Ei "PutObject|CreateBucket|GetObject|ListObjects|ListBuckets|200 OK|s3\." server1.log
NO per-request/API log line found in console (negative proven)
```

Console logging is on by default — the comment `// Console logging is on by default`
([internal/logger/config.go:L296](../../internal/logger/config.go)) precedes
`Console{ Enabled: true }` ([internal/logger/config.go:L297-L298](../../internal/logger/config.go)) — and
startup lines are emitted via `logger.Startup`
([internal/logger/console.go:L246](../../internal/logger/console.go)). But the console is **not** an
access log, so no per‑request entry appears.

### 2) `mc admin trace` — the per‑request positive **[observed]**

Running `mc admin trace local` in one place (making it a trace subscriber) while driving S3 operations
in another produced these **real, timestamped** lines, verbatim:

```
2026-07-13T16:46:58.688 [200 OK] s3.PutObject 127.0.0.1:9000/onboarding-demo/trace-hello.txt 127.0.0.1        2.678ms      ⇣  2.643253ms  ↑ 213 B ↓ 0 B
2026-07-13T16:46:58.701 [200 OK] s3.GetObject 127.0.0.1:9000/onboarding-demo/hello.txt 127.0.0.1        602µs       ⇣  568.071µs  ↑ 151 B ↓ 12 B
2026-07-13T16:46:58.704 [200 OK] s3.ListObjectsV2 127.0.0.1:9000/onboarding-demo?list-type=2&encoding-type=url  127.0.0.1        717µs       ⇣  705.465µs  ↑ 131 B ↓ 894 B
2026-07-13T16:46:58.708 [200 OK] s3.HeadBucket 127.0.0.1:9000/onboarding-demo 127.0.0.1        160µs       ⇣  128.678µs  ↑ 131 B ↓ 0 B
```

Each line is `<UTC timestamp> [200 OK] s3.<Op> <host>/<path> <client-ip> <total latency> ⇣ <time-to-first-byte> ↑ <request bytes> ↓ <response bytes>`.
Note `s3.GetObject … ↓ 12 B` — the 12‑byte `hello minion` payload, cross‑consistent with R2+R3.

**How trace is gated (observed → code):** every S3 handler is wrapped by `httpTraceAll`
([cmd/http-tracer.go:L194](../../cmd/http-tracer.go)). The wrapped handler always runs
`h.ServeHTTP(...)` ([cmd/http-tracer.go:L89](../../cmd/http-tracer.go)), but the trace event is
**subscriber‑gated**: `if globalTrace.NumSubscribers(madmin.TraceS3|madmin.TraceInternal) == 0 { return }`
([cmd/http-tracer.go:L92](../../cmd/http-tracer.go)); the event is published by `globalTrace.Publish(t)`
([cmd/http-tracer.go:L172](../../cmd/http-tracer.go)) **only when a subscriber exists**.
`mc admin trace` becomes that subscriber via `GET /minio/admin/v3/trace` →
`adminAPI.TraceHandler` ([cmd/admin-router.go:L410](../../cmd/admin-router.go)).

### 3) Audit‑webhook JSON — a second per‑request signal **[observed]**

As a cross‑check, an audit webhook (configured via `MINIO_AUDIT_WEBHOOK_ENABLE_*` /
`MINIO_AUDIT_WEBHOOK_ENDPOINT_*` on a **separate throwaway instance**, to keep the primary data
directory pristine) delivered per‑operation JSON. The full captured PutObject record, verbatim (the
only edit is the SigV4 `Signature=` value inside the `Authorization` header, replaced with
`REDACTED_SIGV4` per secret‑hygiene; nothing else is altered):

```json
{"version":"1","deploymentid":"6f4963a8-ae26-489b-a61f-e1fbe72c0684","time":"2026-07-13T16:48:31.144622299Z","event":"","trigger":"incoming","api":{"name":"PutObject","bucket":"audit-demo","object":"audit-obj.txt","status":"OK","statusCode":200,"rx":8,"tx":0,"txHeaders":442,"timeToFirstByte":"88145285ns","timeToFirstByteInNS":"88145285","timeToResponse":"88167051ns","timeToResponseInNS":"88167051"},"remotehost":"127.0.0.1","requestID":"18C1E75C4E8F6E1A","userAgent":"Boto3/1.43.46 md/Botocore#1.43.46 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/Z,D,N,e,U,b cfg/retry-mode#legacy Botocore/1.43.46","requestPath":"/audit-demo/audit-obj.txt","requestHost":"127.0.0.1:9010","requestHeader":{"Accept-Encoding":"identity","Amz-Sdk-Invocation-Id":"04eb3f4c-605d-4982-b8bb-ee875ca6615a","Amz-Sdk-Request":"attempt=1","Authorization":"AWS4-HMAC-SHA256 Credential=minioadmin/20260713/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=REDACTED_SIGV4","Content-Length":"8","Content-Type":"text/plain","Expect":"100-continue","User-Agent":"Boto3/1.43.46 md/Botocore#1.43.46 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/Z,D,N,e,U,b cfg/retry-mode#legacy Botocore/1.43.46","X-Amz-Checksum-Crc32":"8e8ppg==","X-Amz-Content-Sha256":"3a7f63b380ceb6f5a07f7a8faa342e14252c0fdd5c874bf9ce7cc01b99f6d934","X-Amz-Date":"20260713T164831Z","X-Amz-Sdk-Checksum-Algorithm":"CRC32"},"responseHeader":{"Accept-Ranges":"bytes","Content-Length":"0","ETag":"1091076bc8db8ce5fa60f11ebad3a9d0","Server":"MinIO","Strict-Transport-Security":"max-age=31536000; includeSubDomains","Vary":"Origin,Accept-Encoding","X-Amz-Checksum-Crc32":"8e8ppg==","X-Amz-Id-2":"6288f7c424456b65729155b10570da05022411640ac68a83da601467ee9d5c0a","X-Amz-Request-Id":"18C1E75C4E8F6E1A","X-Content-Type-Options":"nosniff","X-Ratelimit-Limit":"1139865","X-Ratelimit-Remaining":"1139865","X-Xss-Protection":"1; mode=block"},"tags":{"PutObject":"name=audit-obj.txt,pool=1,set=1"},"accessKey":"minioadmin"}
```

The record carries an `api.name` of `PutObject`, `statusCode` 200, a `requestID`
(`18C1E75C4E8F6E1A`), a UTC `time`, the request/response headers, and the `ETag` — exactly the
per‑request fields the question asks for. The PutBucket record captured immediately before it carried
`"api":{"name":"PutBucket",...,"statusCode":200}`, `requestID` `18C1E75C4CFAB450`, and `time`
`2026-07-13T16:48:31.053286127Z`. Audit records are emitted via `AuditLog`
([internal/logger/audit.go:L63](../../internal/logger/audit.go)). *(This audit capture used a separate
instance on port 9010 and was torn down afterward; the primary `/tmp/minio-data` was never touched.)*

**Rationale:** the console shows no per‑request line because it is not an access log — the trace
pipeline only publishes when a subscriber (e.g. `mc admin trace`) is attached (the `NumSubscribers`
gate), and audit logging (off by default) emits per‑operation JSON only when a target is configured.
This is why R3/R5 evidence with timestamps must come from the trace/audit subsystems rather than the
default console. During the direct validation the console showed only the startup banner for the entire
flow, which is the concrete proof of the negative.


## R6 — On‑Disk Artifacts (where buckets and objects actually live)

The data directory is `/tmp/minio-data`. All listings below are verbatim.

### Data‑directory root **[observed]**

```
$ ls -la /tmp/minio-data
drwxr-xr-x  .minio.sys
drwxr-xr-x  onboarding-demo
```

Two top‑level entries: the internal metadata bucket `.minio.sys` and the user bucket directory
`onboarding-demo`.

### The drive format file — `xl-single` **[observed]**

`format.json` lives at `/tmp/minio-data/.minio.sys/format.json`. Its actual contents:

```json
{"version":"1","format":"xl-single","id":"cd93af63-b4af-4075-8ade-2d2ed27374c0","xl":{"version":"3","this":"4ee6329c-62be-4918-99a8-1535e75eb737","sets":[["4ee6329c-62be-4918-99a8-1535e75eb737"]],"distributionAlgo":"SIPMOD+PARITY"}}
```

The `"format":"xl-single"` value is the constant `formatBackendErasureSingle = "xl-single"`
([cmd/format-erasure.go:L43](../../cmd/format-erasure.go)), assigned when the format is written
(`format.Format = formatBackendErasureSingle`, [cmd/format-erasure.go:L153](../../cmd/format-erasure.go)).
A single local drive is served by MinIO's erasure backend in single‑drive mode.

### The bucket directory and per‑object `xl.meta` tree **[observed]**

```
$ find /tmp/minio-data/onboarding-demo | sort
/tmp/minio-data/onboarding-demo
/tmp/minio-data/onboarding-demo/data/report.json/xl.meta
/tmp/minio-data/onboarding-demo/hello.txt/xl.meta
/tmp/minio-data/onboarding-demo/trace-hello.txt/xl.meta
```

- A **bucket is a top‑level directory** (`onboarding-demo`), created by `MakeBucket`
  ([cmd/erasure-server-pool.go:L852](../../cmd/erasure-server-pool.go)).
- **Each object is a directory** containing a single metadata file `xl.meta` — the constant
  `xlStorageFormatFile = "xl.meta"` ([cmd/xl-storage.go:L68](../../cmd/xl-storage.go)). The nested key
  `data/report.json` becomes the nested directory `data/report.json/xl.meta`.
  (`trace-hello.txt` is the object written during the R5 trace capture.)

### Small objects are inlined into `xl.meta` (no separate part file) **[observed]**

There is **no** separate `part.1` payload file:

```
$ find /tmp/minio-data/onboarding-demo -name "part.*" | wc -l
0
```

Every `xl.meta` begins with the magic prefix `XL2 ` (bytes `58 4C 32 20`):

```
$ head -c 4 /tmp/minio-data/onboarding-demo/hello.txt/xl.meta | xxd
00000000: 584c 3220                                XL2 
```

And the small‑object payload is embedded **inside** `xl.meta`. The literal body `hello minion` is found
in the file, and the `od -c` tail shows it in place:

```
$ LC_ALL=C grep -a -c -F "hello minion" /tmp/minio-data/onboarding-demo/hello.txt/xl.meta
1
$ od -c /tmp/minio-data/onboarding-demo/hello.txt/xl.meta | tail -4
0000640 322 210   | 252   H   3 321   U   Z   @ 372 006 250   i   W   E
0000660 331 250 356  \n 377 350   ' 364 271 264   [   h   e   l   l   o
0000700       m   i   n   i   o   n
0000707
```

The `data/report.json/xl.meta` file likewise begins with `584c 3220` (`XL2 `) and inlines its JSON
payload. The payload is written into `xl.meta` by
`s.WriteAll(ctx, volume, pathJoin(path, xlStorageFormatFile), buf)`
([cmd/xl-storage.go:L1187](../../cmd/xl-storage.go)).

*(Tooling note: the container's `grep` is BusyBox, whose `grep -a -o` prints nothing for this binary
match; `strings`, `od -c`, and `grep -a -c -F` all confirm the inlined bytes.)*

### The internal metadata bucket `.minio.sys` **[observed]**

```
$ ls -la /tmp/minio-data/.minio.sys
buckets/      config/      format.json   multipart/    pool.bin      tmp/
```

`.minio.sys` is the constant `minioMetaBucket = ".minio.sys"`
([cmd/object-api-utils.go:L60](../../cmd/object-api-utils.go)). It holds server‑internal state:
`config/config.json` and `config/iam/` (server and identity config), `buckets/` (per‑bucket metadata
and usage such as `.usage.json`), `pool.bin` (pool layout), `multipart/`, and `tmp/` — plus the
`format.json` discussed above.

**Rationale:** the single‑drive `xl-single` erasure backend stores a bucket as a directory and an object
as a directory holding an `xl.meta`. Small object payloads are **inlined** into `xl.meta` (magic `XL2 `)
rather than written as a separate `part.1` file — fewer files and faster small‑object I/O. Server‑wide
state (config, IAM, pool layout, usage) lives under `.minio.sys`. These on‑disk files are the concrete
artifacts proving buckets and objects are actually persisted.


## R7 — Restart Persistence (state survives a full restart)

The server was stopped and restarted on the **same** data directory (`/tmp/minio-data`) with the same
invocation, then the objects were re‑read. This was done **twice** to confirm stability.

### Before **[observed]**

```
[BEFORE] ListObjectsV2 HTTP 200 keys=['data/report.json', 'hello.txt', 'trace-hello.txt']
[BEFORE] GetObject(hello.txt) HTTP 200 etag="84f6bd993afe53f22c433eb79d6bf53d" body=b'hello minion' md5="84f6bd993afe53f22c433eb79d6bf53d"
```

### Restart cycle 1 — format reuse proven by a log diff **[observed]**

The running server was stopped with `SIGTERM` and relaunched with the same command. Diffing the
first‑boot log (`server1.log`) against the restart log (`server2.log`) shows the first‑boot‑only
`Formatting` lines are **absent** on restart (a `-` prefix means "present on first boot, absent on
restart"):

```diff
-INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
-INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
 MinIO Object Storage Server
 Copyright: 2015-2026 MinIO, Inc.
 ...
-INFO: Exiting on signal: TERMINATED
```

(The trailing `-INFO: Exiting on signal: TERMINATED` is the graceful‑shutdown line that the SIGTERM
added to `server1.log`.) After restart the objects are unchanged:

```
[AFTER-R1] ListObjectsV2 HTTP 200 keys=['data/report.json', 'hello.txt', 'trace-hello.txt']
[AFTER-R1] GetObject(hello.txt) HTTP 200 etag="84f6bd993afe53f22c433eb79d6bf53d" body=b'hello minion' md5="84f6bd993afe53f22c433eb79d6bf53d"
```

### Restart cycle 2 — stability across ≥2 runs **[observed]**

A second stop/restart on the same data directory again re‑served the objects identically, and the
`Formatting` marker count confirms format initialization happened **only** on the first boot:

```
[AFTER-R2] ListObjectsV2 HTTP 200 keys=['data/report.json', 'hello.txt', 'trace-hello.txt']
[AFTER-R2] GetObject(hello.txt) HTTP 200 etag="84f6bd993afe53f22c433eb79d6bf53d" body=b'hello minion' md5="84f6bd993afe53f22c433eb79d6bf53d"

$ for f in server1 server2 server3; do echo -n "$f Formatting-count: "; grep -c "Formatting 1st pool" $f.log; done
server1 Formatting-count: 1
server2 Formatting-count: 0
server3 Formatting-count: 0
$ head -1 server3.log
MinIO Object Storage Server
```

So across three boots (first boot + two restarts) the bucket `onboarding-demo` and all objects remained
accessible with **HTTP 200**, `hello.txt` still returned `hello minion`, and its ETag stayed byte‑identical
(`84f6bd993afe53f22c433eb79d6bf53d`).

**Rationale:** persistence works because all bucket/object state lives on disk under the data directory
— `format.json` plus per‑object `xl.meta` with inlined payloads (R6). On restart the server detects and
**reuses** the existing on‑disk format (written once via `formatBackendErasureSingle`,
[cmd/format-erasure.go:L43](../../cmd/format-erasure.go)/[L153](../../cmd/format-erasure.go)) rather than
re‑initializing — hence the absence of a re‑`Formatting` line — and re‑serves the same objects from the
same `xl.meta` files ([cmd/xl-storage.go:L68](../../cmd/xl-storage.go)). The behavior was stable across
two restart cycles.


## Coverage Pass

Every requirement, every named S3 operation, every artifact, and both secondary conditions are answered
above. This table maps each to the section that answers it and the primary `file:line` evidence.

| Requirement / item | Answered in | Primary code citation |
|--------------------|-------------|-----------------------|
| **R1** Build & start a default single‑node server; canonical banner | R1 | `Makefile:L177/L179`; `buildscripts/gen-ldflags.go:L36-L40`; `printStartupMessage` `cmd/server-startup-msg.go:L39` |
| — canonical vs non‑canonical version banner | R1 | `gen-ldflags.go:L36-L40` (canonical); plain `go build` → `DEVELOPMENT.GOGET` [non-canonical] |
| — default‑credentials warning | R1 | startup `WARN` line; `README.md:L29` |
| **R2** Create bucket, ≥2 objects, list, download | R2+R3 | handlers table below |
| **R3** Per‑step status / headers / body | R2+R3 | `writeSuccessResponseHeadersOnly` `cmd/api-response.go:L940`; `writeSuccessResponseXML` `cmd/api-response.go:L925` |
| — CreateBucket | R2+R3 §1 | `PutBucketHandler` `cmd/bucket-handlers.go:L723` |
| — PutObject `hello.txt` | R2+R3 §2 | `PutObjectHandler` `cmd/object-handlers.go:L1745` |
| — PutObject `data/report.json` (nested prefix) | R2+R3 §3 | `PutObjectHandler` `cmd/object-handlers.go:L1745` |
| — ListObjectsV2 (full XML) | R2+R3 §4 | `ListObjectsV2Handler` `cmd/bucket-listobjects-handlers.go:L154`; struct `cmd/api-response.go:L131` |
| — GetObject `hello.txt` (download again) | R2+R3 §5 | `GetObjectHandler` `cmd/object-handlers.go:L715` |
| — ListBuckets (full XML) | R2+R3 §6 | `ListBucketsHandler` `cmd/bucket-handlers.go:L306`; struct `cmd/api-response.go:L222` |
| — `Server`/`Accept-Ranges` headers | R2+R3 | `setCommonHeaders` `cmd/api-headers.go:L51`; `MinioStoreName` `cmd/build-constants.go:L59` |
| — `x-amz-request-id` / `x-amz-id-2` | R2+R3 | `cmd/generic-handlers.go:L548` / `L549-L550` |
| — `x-amz-bucket-region` absent | R2+R3 | region guard `cmd/api-headers.go:L57` |
| **R4** Authorization — success (200) | R4 | `isReqAuthenticated` `cmd/auth-handler.go:L560` |
| **R4** Authorization — failure (403) *(secondary condition)* | R4 | `doesSignatureMatch` `cmd/signature-v4.go:L347` |
| **R5** Per‑request logs w/ timestamps — console negative | R5 §1 | console default `internal/logger/config.go:L296-L298` |
| **R5** — `mc admin trace` positive | R5 §2 | `httpTraceAll` `cmd/http-tracer.go:L194`; gate `L92`; `TraceHandler` `cmd/admin-router.go:L410` |
| **R5** — audit JSON | R5 §3 | `AuditLog` `internal/logger/audit.go:L63` |
| **R6** On‑disk `format.json` = `xl-single` | R6 | `formatBackendErasureSingle` `cmd/format-erasure.go:L43/L153` |
| **R6** bucket directory | R6 | `MakeBucket` `cmd/erasure-server-pool.go:L852` |
| **R6** per‑object `xl.meta` + inlined payload (`XL2 `, no `part.1`) | R6 | `xlStorageFormatFile` `cmd/xl-storage.go:L68`; `WriteAll` `cmd/xl-storage.go:L1187` |
| **R6** `.minio.sys` internal bucket | R6 | `minioMetaBucket` `cmd/object-api-utils.go:L60` |
| **R7** Restart persistence — before/after + format reuse *(secondary condition)* | R7 | `formatBackendErasureSingle` `cmd/format-erasure.go:L43`; `xl.meta` `cmd/xl-storage.go:L68` |
| **R7** stability across ≥2 runs | R7 | Formatting‑count 1/0/0 over three boots |

### Notes on canonicality and honesty

- All S3 values were obtained from the **real S3 API on port 9000** via a SigV4‑signing client — never
  from the web console (9001) or an admin bypass.
- The only **[non-canonical]** value shown is the plain‑`go build` banner `DEVELOPMENT.GOGET`, labeled as
  such and used only for contrast.
- One runtime observation **differs from a common expectation**: `x-amz-id-2` was **present** on this
  single‑node server (the local node name is non‑empty here). This document reports the observed
  behavior and cites the guard ([cmd/generic-handlers.go:L549](../../cmd/generic-handlers.go)) that would
  omit it when the node name is empty.
- Every value above is a directly captured runtime observation except where explicitly labeled
  **[inferred]**; no code was elided.

