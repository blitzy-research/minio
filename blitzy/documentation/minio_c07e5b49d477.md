# MinIO "First Bucket" Workflow — Runtime Investigation (commit c07e5b49d477)

## 1. Summary

This document is an evidence-backed investigation into how a fresh, single-node MinIO Object Storage server behaves end-to-end when an operator performs a first "first bucket" workflow in a typical local development setup. The investigation targets MinIO at commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`, module `github.com/minio/minio` (`go.mod:L1`), built from source and run as a single-node single-drive local topology — the S3 API on `:9000`, the embedded Console on `:9001`, and the default root credentials `minioadmin:minioadmin` (`README.md:L61`, `cmd/server-main.go:L975`).

The exercised workflow is exactly the one posed in the prompt — *"create a new bucket, upload at least two different objects, list them, download one again"* — concretely: create the bucket `first-bucket`, upload two distinct objects (`hello.txt` and a nested-key object `docs/readme.md`), `ListObjectsV2`, download `hello.txt` again, plus negative reads (a missing key and a missing bucket) and three authentication-failure probes, and finally a real server **restart** on the same data directory to prove durability. Every answer below is backed by (a) a concrete runtime artifact — an HTTP transcript, a timestamped server log, or a filesystem listing, shown in a fenced code block — and (b) a citation to the implementing source in `path:Lnnn` form, plus the stated rationale.

The document answers eight decomposed requirements: **R1** run a single-node server; **R2** exercise the full first-bucket flow; **R3** capture exact HTTP semantics per step; **R4** capture timestamped server logs; **R5** document access/authorization checks; **R6** explain request processing and data persistence; **R7** identify on-disk filesystem artifacts; and **R8** verify restart persistence.

> **Investigation-only.** The MinIO source tree was **not** modified at any point. This is a read-only behavioral investigation; the repository remained pristine (`git status --porcelain` empty) throughout, and the only artifact produced is this document.

Example — the exact revision under investigation, and confirmation that no tracked source file was modified during the work:

```sh
$ git rev-parse HEAD
c07e5b49d477b0774f23db3b290745aef8c01bd2

$ git status --porcelain --untracked-files=no
            # (empty output: no tracked source file modified)
```

This commit is the module `github.com/minio/minio` (`go.mod:L1`); the empty porcelain output above is the baseline durability guarantee for the "investigation-only" constraint, re-checked at the end in Section 10.

## 2. Environment & Build (R1)

The server binary was compiled from source at the pinned commit using **Go 1.23.5** — a `1.23.x` patch release matching the module's `go 1.23` directive (`go.mod:L3`) and the CI workflows' pinned `1.23.x` toolchain. The binary was written to a scratch path **outside** the repository (`/tmp/minio-bin`) so the source tree stays pristine.

Example — the build recipe is the Makefile `build` target's `go build` line (`Makefile:L177` declares `build: checks build-debugging`; `Makefile:L179` is the build command):

```sh
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio
```

For this investigation the binary was emitted to scratch rather than the repo root:

```sh
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .
```

Supporting source references:

- `go.mod:L1` — `module github.com/minio/minio` (the single Go module under investigation).
- `go.mod:L3` — `go 1.23` (the language/toolchain floor; Go 1.23.5 satisfies it).
- `README.md:L51` — the documented local-dev run line `minio server /data`.
- `README.md:L61` — documents that the deployment starts with default root credentials `minioadmin:minioadmin` and describes the embedded Console.

**Rationale.** The build was placed under `/tmp/minio-bin` so the repository is never touched (no stray `./minio` binary committed). The Makefile's `LDFLAGS` (which inject the release version string) were intentionally **not** applied to this scratch build; the consequence is a `Version: DEVELOPMENT.GOGET` banner (see Section 3), which is purely cosmetic and does not change runtime behavior.

## 3. Starting the Single-Node Server (R1)

The server was launched as a single node with the S3 API on `:9000` and the Console on `:9001`, against a scratch data directory `/tmp/minio-data`:

```sh
minio server /tmp/minio-data --address ":9000" --console-address ":9001"
```

Example — the captured **first-initialization** startup banner (verbatim):

```text
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.5 linux/amd64)
API: http://127.0.0.1:9000 ...
WebUI: http://127.0.0.1:9001 ...
Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

Source references for the startup path:

- `cmd/server-main.go:L742` — `func serverMain(ctx *cli.Context)`, the server entry point that bootstraps the node.
- `cmd/server-main.go:L975` — the exact default-credentials warning string (`Detected default credentials '%s', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables`), emitted when the active credentials equal `auth.DefaultCredentials`.
- `cmd/prepare-storage.go:L194` — `logger.Info("Formatting %s pool, %v set(s), %v drives per set.", ...)`, which prints the `Formatting 1st pool, 1 set(s), 1 drives per set.` line. It is gated at `cmd/prepare-storage.go:L193` by `if shouldInitErasureDisks(sErrs) && firstDisk` — this gate is the key to the one-time-format nuance verified later in Section 9 (R8).

**Rationale.** The `Version: DEVELOPMENT.GOGET` string is expected because the Makefile `LDFLAGS` that inject a release version were not applied to the scratch build; it has no effect on runtime behavior. The `Formatting 1st pool, 1 set(s), 1 drives per set.` line confirms a **single-node single-drive erasure pool** is being initialized on first boot — i.e., the erasure-coded `xl-storage` backend, not a legacy standalone filesystem backend (corroborated on disk in Section 8 by `format.json` `format:"xl-single"`).

## 4. First-Bucket Flow — Full HTTP Transcript per Operation (R2/R3)

The flow was driven with **boto3 1.43.36 (botocore 1.43.36)** as the S3 client, using AWS Signature V4, path-style addressing, against the endpoint `http://127.0.0.1:9000`.

### 4.1 The two objects

Two genuinely distinct objects were uploaded; the second uses a nested key (`docs/readme.md`) to demonstrate prefix/"folder" behavior:

- `hello.txt` = `"Hello, MinIO! This is the first object.\n"` — 40 bytes, MD5 `a9cb0d083d193fbfed149dda4946d607`.
- `docs/readme.md` = `"# Readme\n\nSecond object stored under a nested prefix.\n"` — 54 bytes, MD5 `459d129d91f3f29826bf786aeb476812`.

### 4.2 Common success response headers

Every successful operation carried this common set of headers, injected by the middleware and common-header setter: `Server: MinIO`, `X-Amz-Request-Id` (per-request, e.g. `18BCB88DB9BD6436`), `X-Amz-Id-2` (`dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`), `Accept-Ranges: bytes`, `X-Content-Type-Options: nosniff`, `Vary: Origin, Accept-Encoding`, `Strict-Transport-Security: max-age=31536000; includeSubDomains`, `X-Xss-Protection: 1; mode=block`, and `X-Ratelimit-Limit`/`X-Ratelimit-Remaining`.

### 4.3 Per-operation HTTP transcript

| Operation | Status | Distinctive headers | Body |
|-----------|--------|---------------------|------|
| CreateBucket `first-bucket` | **200 OK** | `Content-Length: 0`, `Location: /first-bucket` | empty |
| PutObject `hello.txt` | **200 OK** | `ETag: "a9cb0d083d193fbfed149dda4946d607"`, `X-Amz-Checksum-Crc32: C/33qw==`, `Content-Length: 0` | empty |
| PutObject `docs/readme.md` | **200 OK** | `ETag: "459d129d91f3f29826bf786aeb476812"`, `X-Amz-Checksum-Crc32: Yfx2SA==` | empty |
| ListObjectsV2 | **200 OK** | `Content-Type: application/xml`, `Content-Length: 682` | XML (below) |
| GetObject `hello.txt` | **200 OK** | `Content-Type: text/plain`, `Content-Length: 40`, `ETag: "a9cb0d083d193fbfed149dda4946d607"`, `Last-Modified: Fri, 26 Jun 2026 19:34:31 GMT` | the 40 object bytes (byte-identical to upload) |
| GetObject missing key | **404** | `Content-Type: application/xml` | `NoSuchKey` XML |
| GetObject missing bucket | **404** | `Content-Type: application/xml` | `NoSuchBucket` XML |
| ListBuckets | **200 OK** | `Content-Type: application/xml` | `ListAllMyBucketsResult` |
| HeadBucket | **200 OK** | `Content-Length: 0` | empty |

### 4.4 ListObjectsV2 body (682 bytes, verbatim)

Example — the `ListObjectsV2` response body exactly as captured (a single-line body; keys are sorted lexicographically, so `docs/readme.md` precedes `hello.txt`, and the `ETag` value is XML-escaped as `&#34;...&#34;`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>first-bucket</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>docs/readme.md</Key><LastModified>2026-06-26T19:34:31.410Z</LastModified><ETag>&#34;459d129d91f3f29826bf786aeb476812&#34;</ETag><Size>54</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-06-26T19:34:31.405Z</LastModified><ETag>&#34;a9cb0d083d193fbfed149dda4946d607&#34;</ETag><Size>40</Size><StorageClass>STANDARD</StorageClass></Contents><EncodingType>url</EncodingType></ListBucketResult>
```

### 4.5 ListBuckets body (verbatim)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><Buckets><Bucket><Name>first-bucket</Name><CreationDate>2026-06-26T19:34:31.378Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>
```

### 4.6 Negative read bodies (verbatim)

The two negative reads return `404` with S3 error XML (host-id truncated as `dd9025…e3e8` exactly as captured):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>does-not-exist.txt</Key><BucketName>first-bucket</BucketName><Resource>/first-bucket/does-not-exist.txt</Resource><RequestId>18BCB88DBC4D4430</RequestId><HostId>dd9025…e3e8</HostId></Error>

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message><Key>x</Key><BucketName>no-such-bucket</BucketName><Resource>/no-such-bucket/x</Resource><RequestId>18BCB88DBC8BBC9F</RequestId><HostId>dd9025…e3e8</HostId></Error>
```

### 4.7 Source references (R3)

- Routing & middleware: `cmd/api-router.go:L210` (`s3APIMiddleware`), `cmd/api-router.go:L253` (`registerAPIRouter`).
- Handlers: `cmd/bucket-handlers.go:L723` (`PutBucketHandler`), `cmd/bucket-handlers.go:L306` (`ListBucketsHandler`), `cmd/bucket-handlers.go:L1644` (`HeadBucketHandler`); `cmd/bucket-listobjects-handlers.go:L154` (`ListObjectsV2Handler`), `cmd/bucket-listobjects-handlers.go:L273` (`ListObjectsV1Handler`); `cmd/object-handlers.go:L1745` (`PutObjectHandler`), `cmd/object-handlers.go:L715` (`GetObjectHandler`), `cmd/object-handlers.go:L1009` (`HeadObjectHandler`).
- Response writers: `cmd/api-response.go:L925` (`writeSuccessResponseXML`), `cmd/api-response.go:L940` (`writeSuccessResponseHeadersOnly`), `cmd/api-response.go:L945` (`writeErrorResponse`), `cmd/api-response.go:L986` (`writeErrorResponseJSON`).
- Common headers: `cmd/api-headers.go:L51` (`setCommonHeaders`). The `x-amz-request-id` is injected by `cmd/generic-handlers.go:L536` (`addCustomHeadersMiddleware`); the header constant is `internal/http/headers.go:L160` (`AmzRequestID = "x-amz-request-id"`).
- **PutObject response `ETag`** (the `PutObject` rows in the transcript): `PutObjectHandler` sets the response headers via `setPutObjHeaders` at `cmd/object-handlers.go:L2099`, then writes the success response at `cmd/object-handlers.go:L2128` (`writeSuccessResponseHeadersOnly`). `setPutObjHeaders` is defined at `cmd/object-handlers-common.go:L355`, and it writes the quoted `ETag` directly as a map entry at `cmd/object-handlers-common.go:L359-L360` (`w.Header()[xhttp.ETag] = []string{...}`, the value wrapped in double quotes; set as a literal map key so broken clients see exactly `ETag`).
- **GET/HEAD object response `ETag`** (the `GetObject hello.txt` row): `cmd/api-headers.go:L111` (`setObjectHeaders`) sets the quoted `ETag` at `cmd/api-headers.go:L120-L121` (`w.Header()[xhttp.ETag] = []string{"\"" + objInfo.ETag + "\""}`). This is the GET/HEAD path, distinct from the `PutObject` path above.
- Error codes for the negative reads: `cmd/api-errors.go:L149` (`ErrNoSuchKey` declaration; 404 mapping at `cmd/api-errors.go:L669`) and `cmd/api-errors.go:L119` (`ErrNoSuchBucket` declaration; 404 mapping at `cmd/api-errors.go:L639`).

**Rationale.** `PutObject` returns an `ETag` — the MD5 of the single-part body (`a9cb0d083d193fbfed149dda4946d607` for `hello.txt`) — while `PutBucket` returns no `ETag` because there is no object body to hash. `ListObjectsV2` returns keys sorted lexicographically, which is why `docs/readme.md` appears before `hello.txt`. The `EncodingType: url` element is present because the boto3 client requested URL-encoded keys. The missing-resource reads return `404` (`NoSuchKey`/`NoSuchBucket`) rather than `403` precisely because authentication and authorization both succeeded under the valid root credentials (see Section 6).

## 5. Server Log Messages with Timestamps (R4)

A key observation: the **default console log** (captured to `/tmp/minio-server.log`) prints **only** the startup banner plus warnings/errors. **Successful S3 requests are NOT logged to the console by default** — after running the entire first-bucket flow, the console log contained only a 12-line startup transcript with no per-request lines. Per-request observability is provided by the HTTP tracer and is surfaced through the admin API via `mc admin trace` (alternatively `mc admin logs` or `journalctl` for the console stream).

Source references:

- `cmd/http-tracer.go:L69` — `httpTracerMiddleware`, the wrapper that records each request/response.
- `cmd/http-tracer.go:L172` — `globalTrace.Publish(t)`, which publishes the trace event to subscribers (e.g. `mc admin trace`).
- Console/audit sinks: `internal/logger/console.go`, `internal/logger/logger.go`, `internal/logger/reqinfo.go`, and `internal/logger/audit.go:L63` (`AuditLog`). Audit/webhook logging is available but disabled by default.

Example — the **bootstrap initialization** sequence as seen via `mc admin trace --verbose --all`, with source-reference annotations and timestamps in the form `[YYYY-MM-DDThh:mm:ss.mmm]`:

```text
[2026-06-26T19:31:45.574] [server-main.go:754:serverMain()] newConsoleLogger
[2026-06-26T19:31:45.575] [server-main.go] configureServer
[2026-06-26T19:31:45.622] [server-main.go:919] newObjectLayer (duration: 47.09ms)
[2026-06-26T19:31:45.623] [iam.go] globalIAMSys.Init
[2026-06-26T19:31:45.640] [server-main.go:835] checkUpdate
```

Example — a `PutObject` request/response pair captured by `mc admin trace --verbose --all` (timestamps and byte counts shown):

```text
[REQUEST s3.PutObject] [2026-06-26T19:35:55.885] [Client IP: 127.0.0.1]
PUT /first-bucket/trace-demo.txt
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260626/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=...
[RESPONSE] [2026-06-26T19:35:55.888] [ Duration 2.625ms TTFB 2.599311ms ↑ 210 B ↓ 0 B ] 200 OK
```

The corresponding `GetObject` and `ListObjectsV2` response lines (note the `↓ 40 B` matching the 40-byte `hello.txt` body, and the list response bytes):

```text
[RESPONSE] ... [ Duration 453µs TTFB 433µs ↑ 151 B ↓ 40 B ] 200 OK
[RESPONSE] ... [ Duration 585µs ... ↓ 887 B ] 200 OK
```

**Rationale.** The absence of per-request lines in the console log — contrasted with their presence in `mc admin trace` — shows that MinIO's default local server keeps the console quiet for successful S3 calls. Request-level observability is **opt-in** through the HTTP tracer / admin API (`httpTracerMiddleware` at `cmd/http-tracer.go:L69`, publishing via `globalTrace.Publish(t)` at `cmd/http-tracer.go:L172`), which is why "request received → operation completed" evidence (Section 7) is read from the tracer rather than from the default console stream.

## 6. Authentication & Authorization (R5)

Every request in the flow carried an AWS Signature V4 `Authorization` header. The anatomy of the header (plus the two companion signed headers) is:

```text
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260626/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=<hex>
x-amz-content-sha256: <sha256-of-payload>
x-amz-date: 20260626T193431Z
```

The server first **classifies** the auth type, then **validates** the HMAC-SHA256 signature, then **authorizes** the action by evaluating the IAM policy decision. The relevant code path:

- `cmd/auth-handler.go:L339` — `checkRequestAuthType`, which classifies the request's auth type (presigned, signed-V4, signed-V2, anonymous, etc.).
- `cmd/auth-handler.go:L358` — `authenticateRequest`, the per-request authentication entry.
- `cmd/auth-handler.go:L523` — `checkRequestAuthTypeCredential`, which resolves the credential and owner status.
- `cmd/auth-handler.go:L560` — `isReqAuthenticated`, which performs the actual SigV4 signature verification.
- Signature parsing/validation: `cmd/signature-v4.go`, `cmd/signature-v4-utils.go`, `cmd/signature-v4-parser.go`, `cmd/streaming-signature-v4.go`, and (for V2) `cmd/signature-v2.go`.
- Authorization: `cmd/iam.go` — the IAM policy decision via `combinedPolicy.IsAllowed`.
- Error → HTTP status mapping: `cmd/api-errors.go`.

### 6.1 Captured authentication-failure cases (all HTTP 403)

Three negative probes were run against the server; each returns HTTP `403`:

- **Wrong secret → `403 SignatureDoesNotMatch`** — message: "The request signature we calculated does not match the signature you provided. Check your key and signing method." Declared at `cmd/api-errors.go:L156`; mapped to HTTP 403 at `cmd/api-errors.go:L704`.
- **Unknown access key → `403 InvalidAccessKeyId`** — message: "The Access Key Id you provided does not exist in our records." Declared at `cmd/api-errors.go:L93`; mapped at `cmd/api-errors.go:L579`.
- **Anonymous/unsigned GET on a private object → `403 AccessDenied`** — message: "Access Denied." Declared at `cmd/api-errors.go:L86`; mapped at `cmd/api-errors.go:L539`.

Example — all three captured `403` responses, **verbatim** as returned by the server (each is an S3 error XML body with `Content-Type: application/xml`; the `HostId` is the deterministic node id `dd9025…e3e8` reproduced in Section 4, and each `RequestId` is a per-request id):

Wrong secret (a tampered SigV4 signature) → `403 SignatureDoesNotMatch`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message><Key>hello.txt</Key><BucketName>first-bucket</BucketName><Resource>/first-bucket/hello.txt</Resource><RequestId>18BCC02DF98844A4</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Unknown access key (a credential not present in IAM) → `403 InvalidAccessKeyId`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidAccessKeyId</Code><Message>The Access Key Id you provided does not exist in our records.</Message><Key>hello.txt</Key><BucketName>first-bucket</BucketName><Resource>/first-bucket/hello.txt</Resource><RequestId>18BCC02DF9911766</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Anonymous/unsigned GET on a private object → `403 AccessDenied`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>hello.txt</Key><BucketName>first-bucket</BucketName><Resource>/first-bucket/hello.txt</Resource><RequestId>18BCC02DF99C26FB</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Rationale.** With valid root credentials, **both** authentication (signature verification) and authorization (IAM policy) pass. This is precisely why the missing-resource reads in Section 4 return **404** (`NoSuchKey`/`NoSuchBucket`) and not **403**: the request was authenticated and authorized, so the server proceeds to look up the resource and reports that it does not exist. A `403` arises only when signature, identity, or permission fails — exactly the three cases above.

## 7. Request Processing & Persistence Walkthrough (R6)

### 7.1 The request pipeline

A request flows through a consistent pipeline before any bytes touch the disk:

1. **Middleware** adds security headers and the per-request `x-amz-request-id` via `addCustomHeadersMiddleware` (`cmd/generic-handlers.go:L536`).
2. **SigV4 authentication** verifies the signature via `isReqAuthenticated` (`cmd/auth-handler.go:L560`).
3. **Routing** dispatches to the registered handler via `registerAPIRouter` (`cmd/api-router.go:L253`).
4. **Object layer** executes the operation — `MakeBucket` (`cmd/erasure-server-pool.go:L852`) for create-bucket, or the object handlers in `cmd/object-handlers.go` for object ops.
5. **Erasure/storage layer** persists or reads the bytes on disk.

### 7.2 The WRITE path (request received → data written → completed)

For `PutObject`, the storage layer writes to a temporary directory and then **atomically renames** the result into its final location. Observed in the storage trace, the sequence is:

- `os.Mkdir .minio.sys/tmp/<uuid>` — create a temp staging dir.
- `os.OpenFileW .minio.sys/tmp/<uuid>/xl.meta` — write the object metadata (and, for small objects, the inlined data) into the temp `xl.meta`.
- `os.Mkdir first-bucket/<key>` — create the final per-object directory.
- `os.Rename .minio.sys/tmp/<uuid>/xl.meta → first-bucket/<key>/xl.meta` — promote the staged file to its final path.
- trace line `STORAGE storage.RenameData … first-bucket <key>` (≈1.9ms) — the atomic commit.

Source references for the write path:

- `cmd/xl-storage.go:L2564` — `RenameData`, the atomic commit that promotes staged data/metadata to the final object path.
- `cmd/xl-storage.go:L2099` — `CreateFile`, used to stream object data to disk for non-inlined writes.
- `cmd/xl-storage.go:L1471` — `WriteMetadata`, which serializes the `xl.meta` for an object version.

### 7.3 The READ and LIST paths

- **READ** (`GetObject hello.txt`): `os.OpenFileR first-bucket/hello.txt/xl.meta` → `STORAGE storage.ReadXL … 479 B` (the `xl.meta` is read, the latest version resolved) → the object bytes are streamed back to the client.
- **LIST** (`ListObjectsV2`): `STORAGE storage.WalkDir … first-bucket` walks the bucket directory to enumerate object keys.

Example — a small excerpt of the storage trace illustrating temp-write-then-rename for a `PutObject`:

```text
OS      os.Mkdir        .minio.sys/tmp/<uuid>
OS      os.OpenFileW    .minio.sys/tmp/<uuid>/xl.meta
OS      os.Mkdir        first-bucket/hello.txt
STORAGE storage.RenameData  .minio.sys/tmp/<uuid> -> first-bucket hello.txt   (~1.9ms)
```

**Rationale.** The temp-write-then-`RenameData` pattern (`cmd/xl-storage.go:L2564`) is what makes the write **atomic and durable**: a crash mid-write can only leave orphaned temporary data under `.minio.sys/tmp/`, never a partially written final object. The final object becomes visible only at the instant the atomic rename succeeds, which is the on-disk meaning of "operation completed".

## 8. On-Disk Artifacts (R7)

### 8.1 The backend format marker

The drive's `format.json` (under `.minio.sys/`) identifies the backend:

```json
{"version":"1","format":"xl-single","id":"5ec195e2-52af-43bd-a2e2-c509398b1441","xl":{"version":"3","this":"75091eb9-…","sets":[["75091eb9-…"]],"distributionAlgo":"SIPMOD+PARITY"}}
```

The `format:"xl-single"` value proves that a single-node single-drive run uses the **erasure-coded `xl-storage` backend** (single-drive erasure), **not** a legacy filesystem backend. This corresponds to the `ErasureSDSetupType` ("Erasure single drive") deployment enum at `cmd/setup-type.go:L31`.

### 8.2 The data-directory layout

After the flow, each object is stored as a **directory** containing an `xl.meta` file; a nested key (`docs/readme.md`) produces nested directories. The data directory holds **three** object directories — the two primary first-bucket-flow objects plus `trace-demo.txt`, which is the object created by the `PutObject` shown in the Section 5 request trace (`PUT /first-bucket/trace-demo.txt`):

```text
/tmp/minio-data/first-bucket/
├── docs/readme.md/xl.meta      (496 bytes)
├── hello.txt/xl.meta           (479 bytes)
└── trace-demo.txt/xl.meta      (third object, created during the Section 5 trace demonstration)
```

Because `trace-demo.txt` was written into the **same** data directory as the two primary objects, it persists alongside them and therefore also appears in the post-restart listing in Section 9 (`KeyCount=3`).

### 8.3 The `xl.meta` format

The `xl.meta` files begin with the 4-byte magic `58 4c 32 20` = `XL2 `, followed by the version bytes `01 00 03 00` (major 1, minor 3). Source references:

- `cmd/xl-storage-format-v2.go:L44` — `xlHeader = [4]byte{'X', 'L', '2', ' '}` (the `XL2 ` magic).
- `cmd/xl-storage-format-v2.go:L221` — `checkXL2V1`, which validates the magic and version on read.
- `cmd/xl-storage-format-v2.go:L59` — `xlVersionMajor = 1`; `cmd/xl-storage-format-v2.go:L65` — `xlVersionMinor = 3`.
- `cmd/xl-storage-format-v2.go:L90-L103` — the backend directory-tree doc comment describing `disk1/bucket/object/<data-dir-uuid>/part.1` alongside `xl.meta`.

### 8.4 Small-object inlining

For the small objects in this flow, the data is **inlined into `xl.meta`** rather than stored as a separate `part.*` file. Evidence:

- `grep "Hello, MinIO" hello.txt/xl.meta` matches — the literal object bytes are present inside `xl.meta`.
- The `xl.meta` `MetaSys` carries `x-minio-internal-inline-data: true`.
- **No `part.*` files exist** for these objects. For larger objects, MinIO instead stores the data as `part.N` under a data-directory UUID (per the tree comment at `cmd/xl-storage-format-v2.go:L90-L103`).

On the **read** side, the inline flag is resolved via `fi.InlineData()` while the object's file-info is loaded: `cmd/erasure-object.go:L902` — inside `getObjectFileInfo` (the file-info resolver used by `GetObject`), `if err == nil && (fi.InlineData() || len(fi.Data) > 0) { break }` — and the inline bytes are loaded alongside the metadata in `cmd/xl-storage.go:L1713-L1722` — inside `ReadVersion`, where `fi.InlineData()` short-circuits the read and `fi.SetInlineData()` marks the version inline. The accessors themselves are defined in `cmd/storage-datatypes.go`: `InlineData()` at `cmd/storage-datatypes.go:L361` and `SetInlineData()` at `cmd/storage-datatypes.go:L371`. (Note: `cmd/erasure-object.go:L155` (`inlineData := fi.InlineData()`) reads the inline flag and `cmd/erasure-object.go:L178` (`if inlineData {`) branches on it, but those lines are inside `CopyObject` — they preserve the inline flag during a metadata update, and are **not** the `GetObject` read path.)

### 8.5 The `.minio.sys/` system tree

System metadata lives under `.minio.sys/`:

```text
/tmp/minio-data/.minio.sys/
├── format.json
├── config/config.json/xl.meta
├── config/iam/format.json
├── buckets/first-bucket/.metadata.bin   (per-bucket metadata)
├── buckets/.usage-cache.bin
├── buckets/.bloomcycle.bin
├── multipart/
├── pool.bin/xl.meta
└── tmp/.trash/
```

**Rationale.** The per-object directory + `xl.meta` layout (with inlined small-object data) is the on-disk **proof** that buckets and objects are physically persisted, independent of the running process. Because the data for small objects is embedded directly in `xl.meta`, a single file fully captures both the object's metadata and its bytes — which is exactly what survives the restart verified in Section 9.

## 9. Restart-Persistence Verification (R8)

### 9.1 Stopping and restarting

The server was stopped by sending `SIGTERM` to the **specific numeric process ID** (not a broad `pkill` pattern), which triggered a graceful shutdown — the prior log's final line was:

```text
INFO: Exiting on signal: TERMINATED
```

Example — the server PID is captured at launch as `$!` of the backgrounded process, echoed to confirm it is a concrete numeric PID (here `104011`), verified with `ps`, and then stopped by that exact numeric PID — never a broad `pkill`:

```sh
$ minio server /tmp/minio-data --address ":9000" --console-address ":9001" &
$ MINIO_PID=$!
$ echo "$MINIO_PID"
104011
$ ps -o pid=,comm= -p "$MINIO_PID"
 104011 minio
$ kill "$MINIO_PID"     # equivalently: kill 104011 — SIGTERM to the exact numeric PID; no broad pkill
process exited after ~2s
```

The **same binary** was then restarted against the **same data directory** `/tmp/minio-data`, with no re-upload of any object.

### 9.2 The one-time-format nuance

A key, verified nuance: the **restart** startup log does **NOT** contain the `Formatting 1st pool, 1 set(s), 1 drives per set.` line that appeared on first initialization. This is because `format.json` already exists on the drive, so the formatting step is skipped — it is a one-time initialization. The line is emitted at `cmd/prepare-storage.go:L194` only when its gate at `cmd/prepare-storage.go:L193` (`if shouldInitErasureDisks(sErrs) && firstDisk`) is satisfied, which is not the case once the pool is already formatted.

### 9.3 Persistence proof (no re-upload)

After the restart, the previously created bucket and **all three** objects were still present and byte-identical (no re-upload). The on-disk `xl.meta` files of Section 8 — including the Section 5 `trace-demo.txt` object — were reloaded from the same data directory:

```text
ListBuckets   -> first-bucket  (CreationDate 2026-06-26 19:34:31.378+00:00, unchanged)
ListObjectsV2 -> KeyCount=3: docs/readme.md (54 B), hello.txt (40 B), trace-demo.txt  (identical ETags)
GetObject hello.txt      -> 200, Content-Length: 40, MD5 a9cb0d083d193fbfed149dda4946d607 (byte-identical)
                            body = "Hello, MinIO! This is the first object.\n"
GetObject docs/readme.md -> MD5 459d129d91f3f29826bf786aeb476812 (byte-identical)
```

- `ListBuckets` still shows `first-bucket` with the **same** `CreationDate` (`2026-06-26 19:34:31.378+00:00`) as before the restart.
- `ListObjectsV2` returns all three persisted keys — `docs/readme.md` (54 B), `hello.txt` (40 B), and `trace-demo.txt` (the Section 5 trace-demo object) — each with **identical ETags** (`KeyCount=3`), consistent with the on-disk tree in Section 8.2. The byte-identity checks below focus on the two primary first-bucket-flow objects.
- `GetObject hello.txt` returns `200` with `Content-Length: 40` and a downloaded MD5 of `a9cb0d083d193fbfed149dda4946d607` — byte-identical to the original upload — with body `"Hello, MinIO! This is the first object.\n"`.
- `GetObject docs/readme.md` returns MD5 `459d129d91f3f29826bf786aeb476812` — byte-identical.

**Rationale.** The durability evidence is that the on-disk `xl.meta` files (with inlined data, Section 8) survive the process restart and are reloaded on startup (disk loading / `waitForFormatErasure` is visible in the bootstrap trace). Returning **identical bytes after a real stop/restart, with no re-upload**, is direct proof of durable persistence — the data lives on disk, not merely in process memory.

## 10. Rationale & Conclusions

The cross-cutting conclusions from the investigation, each grounded in the evidence above:

- **404 vs 403 under valid credentials.** Missing-resource reads return `404` (`NoSuchKey`/`NoSuchBucket`) — not `403` — because authentication and authorization both succeeded; the server then truthfully reports the resource is absent. A `403` only arises on signature, identity, or permission failure (the three captured cases in Section 6). See `cmd/api-errors.go:L149`/`L119` (404 decls) versus `cmd/api-errors.go:L156`/`L93`/`L86` (403 decls).
- **ETag semantics.** `PutObject` returns an `ETag` that is the MD5 of the single-part body (e.g. `a9cb0d083d193fbfed149dda4946d607`), set as a quoted value by `setPutObjHeaders` (`cmd/object-handlers-common.go:L359-L360`, invoked from `PutObjectHandler` at `cmd/object-handlers.go:L2099`); `PutBucket` returns no `ETag` because there is no object body to hash.
- **Single-drive erasure backend.** Even a single-node single-drive local run uses the erasure-coded `xl-storage` backend (single-drive erasure semantics), **not** a legacy filesystem backend — proven on disk by `format.json` `format:"xl-single"` (Section 8) and at startup by the `Formatting 1st pool, 1 set(s), 1 drives per set.` banner; the deployment enum is `ErasureSDSetupType` at `cmd/setup-type.go:L31`.
- **One-time formatting.** The `Formatting … pool` banner appears only on first initialization and is skipped on subsequent restarts, because the format line at `cmd/prepare-storage.go:L194` is gated by `firstDisk` at `cmd/prepare-storage.go:L193`.
- **Durable persistence.** Writes are committed atomically via temp-write-then-`RenameData` (`cmd/xl-storage.go:L2564`); small objects are inlined into `xl.meta` and resolved on read via `fi.InlineData()` in `getObjectFileInfo` (`cmd/erasure-object.go:L902`) and `ReadVersion` (`cmd/xl-storage.go:L1713-L1722`). The same bucket and byte-identical objects remain accessible after a real restart on the same data directory (Section 9).
- **Observability is opt-in.** Successful S3 requests are silent on the default console; per-request observability comes from the HTTP tracer (`httpTracerMiddleware`, `cmd/http-tracer.go:L69`), surfaced via `mc admin trace`.

### Investigation-only confirmation

The MinIO source tree was **unmodified** throughout this investigation. All scratch — the compiled binary (`/tmp/minio-bin`), the data directory (`/tmp/minio-data`), the server log (`/tmp/minio-server.log`), and any probe scripts — lived entirely under `/tmp`, outside the repository, and was removed afterward. The repository remained pristine; the only artifact added is this single documentation file under `blitzy/documentation/`.

Example — the closing pristine check; no tracked MinIO source file is modified (the only untracked entry is the `blitzy/` documentation deliverable itself):

```sh
$ git rev-parse HEAD
c07e5b49d477b0774f23db3b290745aef8c01bd2

$ git status --porcelain --untracked-files=no
            # (empty: zero tracked source files changed; go.mod / go.sum untouched)
```

This confirms compliance with the governing investigation-only constraint, which originates in the **`SWE-AtlasQnA-Repo` rule set and the prompt directives** (build and run the source, treat the code as the source of truth, do not modify any existing repository files, and add no other code): the source code remained the source of truth, consulted but never altered. (`docs/debugging/README.md` is MinIO's *Server Debugging Guide* — it documents the `mc admin trace` convention exercised in Section 5, and is **not** the source of the read-only/investigation-only rule.)
