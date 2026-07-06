# MinIO Security Behavior Investigation — Runtime Evidence

**Source repository:** `github.com/minio/minio`
**Source branch:** `minio_c07e5b49d477`
**HEAD commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`

## Overview

This document answers five security-behavior questions about the MinIO object-storage
server using **actual captured runtime evidence**. The investigation is **read-only** and
**run-first**: the MinIO server was built from source in its canonical configuration and
run as a normal operator would; each behavior was then triggered through the real entry
point (the `mc` client, the `minio-go`/`madmin-go` SDKs, or a raw signed HTTP request);
and the complete, unedited output (server HTTP trace lines, client errors, SDK/test
stdout, on-disk effects) was captured next to each claim. Reading the source is used only
to **root-cause** each behavior and cite it at `file:line`.

No MinIO source, test, config, or build file was modified. The only durable artifact of
this task is this document. All temporary servers, data directories, scripts, and captured
logs lived outside the repository tree under `/tmp/blitzy/...` and were removed at the end.

The five questions:

- **Q1** — What happens when a bucket-level encryption requirement takes precedence over a
  user's broad write permission during an unencrypted upload? (server trace evidence)
- **Q2** — With object locking enabled, what log entries appear when someone tries to delete
  locked objects? (runtime log output)
- **Q3** — How does the system handle manual data corruption in the storage backend? Trigger a
  bit-rot detection event and identify the runtime logs during a subsequent GET.
- **Q4** — Prove, with runtime test output, that MinIO enforces a session policy on temporary
  credentials.
- **Q5** — Prove, with test output, that a basic user cannot promote itself to console admin by
  modifying user mappings, and identify the root cause.

> **Evidence provenance.** Every evidence block below was captured freshly during this
> investigation by building and running MinIO at commit `c07e5b49d477`. Fenced code blocks
> are verbatim; each is labelled with the exact command that produced it and the capture
> file it came from. Any statement not directly observed is labelled **(inferred)**; any
> value obtained from a non-default path is labelled **(non-canonical)**. Secret key
> material (the investigation-time KMS master key) is redacted as `<base64-32-bytes>`.

---

## 1. Environment & Build (canonical)

The server was built with the repository's canonical `build:` target
[`Makefile:177-179`]. The exact recipe at `Makefile:179` is:

```makefile
# Makefile:177-179
build: checks build-debugging ## builds minio to $(PWD)
	@echo "Building minio binary to './minio'"
	@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
```

`LDFLAGS` is produced by `go run buildscripts/gen-ldflags.go`. The exact commands used to
build the out-of-tree binary (the in-tree `./minio` output is git-ignored [`.gitignore:4`],
so an in-place build also leaves `git status` clean) were:

```bash
# run from the repository root
export GOROOT=/usr/local/go GOPATH=/root/go PATH=/usr/local/go/bin:/root/go/bin:$PATH
LDFLAGS="$(go run buildscripts/gen-ldflags.go)"
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$LDFLAGS" -o /tmp/blitzy/minio-bin/minio
```

The generated `LDFLAGS` (verbatim) embed the version metadata:

```text
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z -X github.com/minio/minio/cmd.CopyrightYear=2024 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 -X github.com/minio/minio/cmd.GOPATH=/root/go -X github.com/minio/minio/cmd.GOROOT=/usr/local/go
```

**Version derivation (verified).** This checkout has **no git tags**, so
`buildscripts/gen-ldflags.go` cannot use `git describe --tags`; it falls back to the HEAD
commit time. HEAD commit time `2024-11-25T17:10:22Z` becomes the `ReleaseTag`
`DEVELOPMENT.2024-11-25T17-10-22Z` (colons rewritten to dashes; the `DEVELOPMENT` prefix is
used because `MINIO_RELEASE` is unset). This is the canonical build path for this checkout.

`minio --version` (verbatim):

```text
$ /tmp/blitzy/minio-bin/minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

**Go toolchain:** `go version go1.23.12 linux/amd64` (highest 1.23.x; matches `go.mod`'s
`go 1.23` and the repository CI pin).

**Single-node server** (used for Q1, Q2, Q4, Q5) — startup banner and default-credentials
warning, verbatim:

```text
$ /tmp/blitzy/minio-bin/minio server /tmp/blitzy/data --address :9000 --console-address :9001
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)

API: http://10.236.7.92:9000  http://172.17.0.1:9000  http://127.0.0.1:9000 
WebUI: http://10.236.7.92:9001 http://172.17.0.1:9001 http://127.0.0.1:9001 

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
INFO: 
 You are running an older version of MinIO released 9 months before the latest release 
 Update: Run `mc admin update ALIAS` 


```

**Defaults observed:** credentials `minioadmin:minioadmin`; S3 API on `:9000`, console on
`:9001`. A single drive yields deployment type `ErasureSDSetupType` (`EC:0`) — deployment
modes are defined in `cmd/setup-type.go` (`FSSetupType` :28, `ErasureSDSetupType` :31,
`ErasureSetupType` :34, `DistErasureSetupType` :37). For scenario isolation, additional
servers were run on other ports (KMS + auto-encryption on `:9010`; KMS without
auto-encryption on `:9030`; a four-drive erasure set on `:9020`); each is introduced in its
question below with its own banner where relevant.

**`mc` client:** built from `github.com/minio/mc`, invoked as
`/tmp/blitzy/tools/bin/mc --config-dir /tmp/blitzy/mc-config <subcommand>`. Global flags
(e.g. `--config-dir`) always precede the subcommand; placing them after the positional
arguments silently stores empty credentials, turning every request anonymous.

The HTTP trace stream used as the primary evidence source for several questions is produced
by `httpTracerMiddleware` [`cmd/http-tracer.go:69`], which publishes each event via
`globalTrace.Publish(t)` [`cmd/http-tracer.go:172`] to subscribers; `mc admin trace`
subscribes to it.

---

## Q1 — Bucket encryption requirement vs. broad write permission

> **Question (verbatim):** *"what happens when a bucket level encryption requirement takes
> precedence over a user's broad write permissions during an unencrypted upload. Identify
> the specific runtime execution sequence captured in the server trace logs."*

**"What happens" has two distinct answers**, because MinIO has two different "bucket-level
encryption requirement" mechanisms, and they behave oppositely. Both were exercised.

### Path A — bucket default / automatic encryption ⇒ the upload SUCCEEDS, transparently encrypted

A server was started with a KMS master key configured and automatic encryption enabled, and
a broad-write user (`bwuser`, IAM policy `s3:*` on `*`) uploaded an object **with no SSE
header at all**:

```bash
# server (port :9010): KMS key + auto-encryption ON
export MINIO_KMS_SECRET_KEY="my-minio-key:<base64-32-bytes>"
export MINIO_KMS_AUTO_ENCRYPTION=on
/tmp/blitzy/minio-bin/minio server /tmp/blitzy/data-kms --address :9010 --console-address :9011
# upload as the broad-write user, NO encryption flags:
mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/plain.txt kmsbw/autobucket/plain.txt
```

Trace captured with `mc --config-dir /tmp/blitzy/mc-config admin trace -v kms` (file
`q1-autoenc-trace.txt`, lines 113–142 — the `PutObject` request and response):

```text
127.0.0.1:9010 [REQUEST s3.PutObject] [2026-07-06T22:39:19.241] [Client IP: 127.0.0.1]
127.0.0.1:9010 PUT /autobucket/plain.txt
127.0.0.1:9010 Proto: HTTP/1.1
127.0.0.1:9010 Host: 127.0.0.1:9010
127.0.0.1:9010 Content-Length: 228
127.0.0.1:9010 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9010 X-Amz-Decoded-Content-Length: 55
127.0.0.1:9010 Accept-Encoding: zstd,gzip
127.0.0.1:9010 Content-Type: text/plain
127.0.0.1:9010 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9010 X-Amz-Date: 20260706T223919Z
127.0.0.1:9010 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9010 Authorization: AWS4-HMAC-SHA256 Credential=bwuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=3994c58d125fb216755b632087c655381e39aa6a2318c5915335e07dd0b4a8dc
127.0.0.1:9010 <BLOB>
127.0.0.1:9010 [RESPONSE] [2026-07-06T22:39:19.264] [ Duration 23.176ms TTFB 23.147902ms ↑ 392 B  ↓ 0 B ]
127.0.0.1:9010 200 OK
127.0.0.1:9010 Accept-Ranges: bytes
127.0.0.1:9010 Server: MinIO
127.0.0.1:9010 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9010 X-Amz-Request-Id: 18BFD470FF026F2A
127.0.0.1:9010 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9010 X-Xss-Protection: 1; mode=block
127.0.0.1:9010 Content-Length: 0
127.0.0.1:9010 ETag: "16a38aaea5adf189fd307e16f5baa576"
127.0.0.1:9010 Vary: Origin,Accept-Encoding
127.0.0.1:9010 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
127.0.0.1:9010 X-Ratelimit-Remaining: 1141076
127.0.0.1:9010 X-Ratelimit-Limit: 1141076
127.0.0.1:9010 X-Amz-Id-2: 6288f7c424456b65729155b10570da05022411640ac68a83da601467ee9d5c0a
127.0.0.1:9010 X-Content-Type-Options: nosniff
```

**Reading the evidence.** In the request block the client's SigV4
`Authorization: ... SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length`
**does not list `x-amz-server-side-encryption`** — so the client never sent (or signed) an
SSE header. The `X-Amz-Server-Side-Encryption: aws:kms` line that appears in the traced
request is the header the **server injected** into `r.Header` before processing. The
**response** then returns `200 OK` with `X-Amz-Server-Side-Encryption: aws:kms` and
`X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key`, and the object's
ETag is `"16a38aaea5adf189fd307e16f5baa576"`.

**Corroboration (object is encrypted at rest).** The plaintext md5 of the uploaded file is
`5402b2e15e4c253088e517b55862bb02` (`md5sum plain.txt`). A control upload of the same file
to a **non-KMS** server (`:9000`, no auto-encryption) stored it with ETag
`5402b2e15e4c253088e517b55862bb02` and no encryption metadata, whereas the auto-encrypting
bucket produced ETag `16a38aaea5adf189fd307e16f5baa576` — the stored bytes differ, i.e. the
object was encrypted server-side even though the client sent no SSE header.

**Root cause / execution sequence (Path A).**
The toggle is `MINIO_KMS_AUTO_ENCRYPTION`, whose env name is
`EnvKMSAutoEncryption` [`internal/crypto/auto-encryption.go:31`] and which is read by
`LookupAutoEncryption()` [`internal/crypto/auto-encryption.go:37`]; the result is wired into
the server global at `globalAutoEncryption = crypto.LookupAutoEncryption()`
[`cmd/config-current.go:532`]. On the write path `PutObjectHandler`
[`cmd/object-handlers.go:1745`], the `AutoEncrypt` option is applied
(set at [`cmd/object-handlers.go:1233`, `:1896`, `:2272`]); the default-encryption config
is applied by `BucketSSEConfig.Apply` [`internal/bucket/encryption/bucket-sse-config.go:135`],
which — when the request carries no SSE header (`crypto.Requested` is false
[`cmd/object-handlers.go:1999`]) and auto-encryption is on — sets
`X-Amz-Server-Side-Encryption: aws:kms` on the request
[`internal/bucket/encryption/bucket-sse-config.go:140-141`]. The stream is then encrypted by
`EncryptRequest(...)` [`cmd/object-handlers.go:2015` → `cmd/encryption-v1.go:466`]. The
bucket-default encryption API itself is `PutBucketEncryptionHandler`
[`cmd/bucket-encryption-handlers.go:43`] (Get `:129`, Delete `:172`).

**Documented behavior (confirmation).** The in-repo KMS docs enable this with
`export MINIO_KMS_AUTO_ENCRYPTION=on` (`docs/kms/README.md:99`) and note that
auto-encryption only affects requests that do not already carry S3 encryption headers
(`docs/kms/README.md:104`) — matching the observed injection-only-when-absent behavior.

### Path B — an encryption-mandating IAM policy ⇒ the upload is REJECTED with `AccessDenied`

The second mechanism forbids unencrypted writes. The **encryption-mandating deny must live in
the user's IAM policy** (or an STS session policy), not a bucket/resource policy — see the
correction below. The following IAM policy was created and attached to user `denyuser`
(file `enc-mandate-iam.json`):

```json
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]},
 {"Effect":"Deny","Action":["s3:PutObject"],"Resource":["arn:aws:s3:::denybucket/*"],"Condition":{"Null":{"s3:x-amz-server-side-encryption":["true"]}}}
]}
```

The `Null` condition on `s3:x-amz-server-side-encryption` matches exactly when the SSE header
is **absent**. As `denyuser`, an unencrypted upload to `denybucket/` was attempted on the
KMS-enabled-but-auto-encryption-**off** server (`:9030`):

```bash
mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/plain.txt encuser/denybucket/noenc.txt
# client result:
# mc: <ERROR> Failed to copy `/tmp/blitzy/out/plain.txt`. Insufficient permissions ...
```

Trace (`q1-deny-iam-trace.txt`, lines 113–141 — the unencrypted `PutObject` rejected):

```text
127.0.0.1:9030 [REQUEST s3.PutObject] [2026-07-06T22:42:05.721] [Client IP: 127.0.0.1]
127.0.0.1:9030 PUT /denybucket/noenc.txt
127.0.0.1:9030 Proto: HTTP/1.1
127.0.0.1:9030 Host: 127.0.0.1:9030
127.0.0.1:9030 Accept-Encoding: zstd,gzip
127.0.0.1:9030 Content-Length: 228
127.0.0.1:9030 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9030 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9030 X-Amz-Date: 20260706T224205Z
127.0.0.1:9030 Authorization: AWS4-HMAC-SHA256 Credential=denyuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=514e6e38f3611d76c53f6056cb539048288615c874a838d0c299a4bf478ce878
127.0.0.1:9030 Content-Type: text/plain
127.0.0.1:9030 X-Amz-Decoded-Content-Length: 55
127.0.0.1:9030 <BLOB>
127.0.0.1:9030 [RESPONSE] [2026-07-06T22:42:05.721] [ Duration 117µs TTFB 99.16µs ↑ 135 B  ↓ 329 B ]
127.0.0.1:9030 403 Forbidden
127.0.0.1:9030 Content-Type: application/xml
127.0.0.1:9030 Server: MinIO
127.0.0.1:9030 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9030 X-Amz-Request-Id: 18BFD497C1FB995E
127.0.0.1:9030 X-Content-Type-Options: nosniff
127.0.0.1:9030 X-Ratelimit-Remaining: 1140956
127.0.0.1:9030 Accept-Ranges: bytes
127.0.0.1:9030 Content-Length: 329
127.0.0.1:9030 Vary: Origin,Accept-Encoding
127.0.0.1:9030 X-Amz-Id-2: 99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6
127.0.0.1:9030 X-Ratelimit-Limit: 1140956
127.0.0.1:9030 X-Xss-Protection: 1; mode=block
127.0.0.1:9030 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>noenc.txt</Key><BucketName>denybucket</BucketName><Resource>/denybucket/noenc.txt</Resource><RequestId>18BFD497C1FB995E</RequestId><HostId>99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6</HostId></Error>
```

The request (no `x-amz-server-side-encryption` in `SignedHeaders`) is rejected in **117µs**
with `403 Forbidden` and body
`<Error><Code>AccessDenied</Code>...<Resource>/denybucket/noenc.txt</Resource>...</Error>`.

**Control — an encrypted upload by the same user SUCCEEDS**, proving only the *unencrypted*
PUT is blocked (`q1-deny-iam-trace.txt`, lines 254–286):

```bash
mc --config-dir /tmp/blitzy/mc-config cp --enc-kms "encuser/denybucket=my-minio-key" \
    /tmp/blitzy/out/plain.txt encuser/denybucket/enc.txt
```

```text
127.0.0.1:9030 
127.0.0.1:9030 [REQUEST s3.PutObject] [2026-07-06T22:42:05.752] [Client IP: 127.0.0.1]
127.0.0.1:9030 PUT /denybucket/enc.txt
127.0.0.1:9030 Proto: HTTP/1.1
127.0.0.1:9030 Host: 127.0.0.1:9030
127.0.0.1:9030 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9030 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9030 Accept-Encoding: zstd,gzip
127.0.0.1:9030 Authorization: AWS4-HMAC-SHA256 Credential=denyuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id,Signature=e151c4eb9d3cbec7f179a30391a9aa33f58a2b29902a05f32e6dd86290d1746e
127.0.0.1:9030 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9030 X-Amz-Date: 20260706T224205Z
127.0.0.1:9030 X-Amz-Decoded-Content-Length: 55
127.0.0.1:9030 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: my-minio-key
127.0.0.1:9030 Content-Length: 228
127.0.0.1:9030 Content-Type: text/plain
127.0.0.1:9030 <BLOB>
127.0.0.1:9030 [RESPONSE] [2026-07-06T22:42:05.775] [ Duration 22.774ms TTFB 22.754315ms ↑ 436 B  ↓ 0 B ]
127.0.0.1:9030 200 OK
127.0.0.1:9030 Content-Length: 0
127.0.0.1:9030 X-Amz-Id-2: 99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6
127.0.0.1:9030 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9030 X-Xss-Protection: 1; mode=block
127.0.0.1:9030 Server: MinIO
127.0.0.1:9030 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9030 X-Content-Type-Options: nosniff
127.0.0.1:9030 X-Ratelimit-Limit: 1140956
127.0.0.1:9030 ETag: "114861f311f6f8e7ec93b3124da0e3aa"
127.0.0.1:9030 X-Amz-Request-Id: 18BFD497C3D6FD2E
127.0.0.1:9030 Accept-Ranges: bytes
127.0.0.1:9030 Vary: Origin,Accept-Encoding
127.0.0.1:9030 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
127.0.0.1:9030 X-Ratelimit-Remaining: 1140956
127.0.0.1:9030 <BLOB>
```

Here the client explicitly signs the SSE headers
(`SignedHeaders=...;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id`),
the `Null` condition no longer matches, the `Deny` does not fire, and the write returns
`200 OK`.

**Root cause (Path B — deny-wins).** For a regular authenticated user the authorization
dispatch is `IAMSys.IsAllowed` [`cmd/iam.go:2437`]: it returns `true` immediately for the
owner (`if args.IsOwner { return true }` [`cmd/iam.go:2448`]) and otherwise evaluates
**only the user's combined IAM policy** via `GetCombinedPolicy(...).IsAllowed(args)`
[`cmd/iam.go:2482`]. The policy evaluator itself is
`func (Policy) IsAllowed(args Args) bool`
[`github.com/minio/pkg/v3@v3.0.22 policy/policy.go:173`], which iterates **`Deny` statements
first and returns `false` on the first match, before any `Allow` is considered** — this is
why an encryption-mandating `Deny` "takes precedence over" the user's broad `s3:*` allow. The
condition key is `S3XAmzServerSideEncryption = "s3:x-amz-server-side-encryption"`
[`github.com/minio/pkg/v3@v3.0.22 policy/condition/keyname.go:66`].

> **Correction to the AAP phrasing (empirically verified).** The AAP body describes the deny
> path as a *bucket policy*. Observed behavior contradicts this: a **bucket/resource policy
> `Deny s3:PutObject` does NOT block an authenticated IAM user.** On `:9000` a bucket policy
> denying `s3:PutObject` for `Principal *` on `bpbucket/*` was set (confirmed via
> `mc anonymous get-json`), yet an authenticated IAM user with `s3:*` still uploaded
> successfully. The reason is the dispatch above: for a regular authenticated user, only the
> user's **combined IAM policy** is evaluated [`cmd/iam.go:2482`]; the bucket/resource policy
> (`globalPolicySys`) governs **anonymous / cross-account** requests. Therefore the
> encryption-mandating deny must be an **IAM/user (or STS session) policy**, as used above.

**Answer summary (Q1).** "What happens" depends on which requirement is in force:
(a) with bucket **default/auto encryption**, the unencrypted upload **succeeds** and is
transparently encrypted (SSE-KMS injected server-side); (b) with an encryption-mandating
**IAM deny**, the unencrypted upload is **rejected** `403 AccessDenied`, because explicit
deny wins in the policy evaluator regardless of the user's broad allow.

---

## Q2 — Deleting object-lock (WORM) protected objects

> **Question (verbatim):** *"when object locking on a bucket is enabled, what are the
> specific log entries that appear when someone tries to delete the locked objects? I want
> you to give me runtime log output to show this."*

A lock-enabled bucket was created with `mc mb --with-lock local/wormtest` (object locking
implies versioning). Distinct object versions were then placed under **Governance**,
**Compliance**, and **Legal Hold**, and versioned deletes were attempted.

**Where the "log entries" are.** At the default console log level the single-node server's
console output (`minio-sn.log`) remained the startup banner only — MinIO emits **no
per-delete console line** for a blocked delete. The runtime log evidence is therefore the
**HTTP trace stream** (`mc admin trace`) plus the client-facing error. Note also that `mc`
issues deletes through the S3 **batch** API `DeleteMultipleObjects` (`POST /wormtest/?delete=`),
so a blocked delete returns **`200 OK`** at the HTTP layer with a per-object `<Error>`
embedded inside the `<DeleteResult>` document — not a top-level HTTP error.

### Governance — blocked without bypass, allowed with bypass

Attempting to delete a Governance-protected version **without** bypass
(`mc rm --version-id <VID> local/wormtest/gov.txt`) — client result:
`mc: <ERROR> ... 'gov.txt (Version ID=0fd838a4-...)' is WORM protected and cannot be
overwritten.` Trace (`q2-gov-trace.txt`, lines 93–120):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:44:24.997] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 1e7ad4c645b0d4b8daab1d4e0e8649cf6951757ea8ef59b195bb8f141441f556
127.0.0.1:9000 X-Amz-Date: 20260706T224424Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=8cf81195a0e5dfa6e1fa0bb9536558bb5daeb394e2e497131e7c7ed148c57ec9
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 Content-Md5: PahMdjFrXSMoEdIWzIa3eg==
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>gov.txt</Key><VersionId>0fd838a4-56f7-47f8-8ae1-c19c80981144</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:44:24.998] [ Duration 370µs TTFB 354.607µs ↑ 236 B  ↓ 304 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4B82F7BC9EF
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 304
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>gov.txt</Key><VersionId>0fd838a4-56f7-47f8-8ae1-c19c80981144</VersionId></Error></DeleteResult>
```

Now **with** bypass (`mc rm --bypass --version-id <VID> local/wormtest/gov.txt`) — the
request carries `x-amz-bypass-governance-retention` in its signed headers and **succeeds**
(state change: version present → deleted). Trace (`q2-gov-trace.txt`, lines 302–330):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:44:25.053] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 Content-Md5: PahMdjFrXSMoEdIWzIa3eg==
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 1e7ad4c645b0d4b8daab1d4e0e8649cf6951757ea8ef59b195bb8f141441f556
127.0.0.1:9000 X-Amz-Date: 20260706T224425Z
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=26adfc2c82de8de0609dab7d3a606586de4a7b7ffc78fffa860f74b896043d3d
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>gov.txt</Key><VersionId>0fd838a4-56f7-47f8-8ae1-c19c80981144</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:44:25.053] [ Duration 784µs TTFB 768.084µs ↑ 270 B  ↓ 212 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4B832C91945
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 212
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>gov.txt</Key><VersionId>0fd838a4-56f7-47f8-8ae1-c19c80981144</VersionId></Deleted></DeleteResult>
```

The response body is now `<DeleteResult>...<Deleted><Key>gov.txt</Key><VersionId>0fd838a4-...
</VersionId></Deleted></DeleteResult>` — the same version id that was previously refused.

### Compliance — never bypassable

A Compliance-mode version (`X-Amz-Object-Lock-Mode: COMPLIANCE`, verified on the PUT) was
targeted with a delete that **includes** `--bypass` (as root, with the bypass header). It is
**still blocked** (`q2-comp-trace.txt`, lines 93–121):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:45:00.185] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=f4e1d91b60b1fcbb09b16914907e499d60de7761b9cbc2aa71e6f724bd2738b5
127.0.0.1:9000 Content-Length: 132
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: f4f9eb5e1b99310d28e62660322891f560d5c8005a6588c33f9190038e94e200
127.0.0.1:9000 X-Amz-Date: 20260706T224500Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Content-Md5: PIS4raFHAgT/fs+wastVwQ==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>comp.txt</Key><VersionId>b21bb9c0-bb4c-4116-b3e3-32b29d088940</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:45:00.185] [ Duration 308µs TTFB 299.212µs ↑ 271 B  ↓ 305 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4C060CF49D6
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 305
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>comp.txt</Key><VersionId>b21bb9c0-bb4c-4116-b3e3-32b29d088940</VersionId></Error></DeleteResult>
```

Even with the bypass header and root credentials, the `<Error>` with
`Object is WORM protected and cannot be overwritten` is returned for `comp.txt` — Compliance
cannot be bypassed by any user, including root. The object remained present after the failed
delete.

### Legal Hold — blocked while held, deletable after clearing

With a legal hold set on `lh.txt`, the versioned delete is blocked
(`q2-legalhold-trace.txt`, lines 91–119):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:45:19.729] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=cffa7a36eb380f844cb9a4b861d5bcd0e13a671a56dc4f5e2e98a0514c5c1552
127.0.0.1:9000 Content-Length: 130
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 Content-Md5: q64Jdef1hLzfhjzoesV0ow==
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: 43275a7467d84339d265c99ee890e6434ea319518f374a423ba5330f0ff8d807
127.0.0.1:9000 X-Amz-Date: 20260706T224519Z
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>lh.txt</Key><VersionId>70c3d788-b842-43bd-885e-da2a2aa2ae16</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:45:19.729] [ Duration 313µs TTFB 304.763µs ↑ 269 B  ↓ 303 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4C4EDBFB9E9
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Content-Length: 303
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>lh.txt</Key><VersionId>70c3d788-b842-43bd-885e-da2a2aa2ae16</VersionId></Error></DeleteResult>
```

After clearing the hold (`mc legalhold clear local/wormtest/lh.txt --version-id <VID>`, which
issues `s3.PutObjectLegalHold` setting status `OFF`), the identical delete now **succeeds**
(`q2-legalhold-trace.txt`, lines 463–490):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:45:19.850] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 43275a7467d84339d265c99ee890e6434ea319518f374a423ba5330f0ff8d807
127.0.0.1:9000 X-Amz-Date: 20260706T224519Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=36f7a6f24ea0a2ffcfac0314af2d11118a3ee3b44a0640fc5e90485828f3b871
127.0.0.1:9000 Content-Length: 130
127.0.0.1:9000 Content-Md5: q64Jdef1hLzfhjzoesV0ow==
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>lh.txt</Key><VersionId>70c3d788-b842-43bd-885e-da2a2aa2ae16</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:45:19.850] [ Duration 744µs TTFB 725.6µs ↑ 235 B  ↓ 211 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 211
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4C4F4F0F76F
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>lh.txt</Key><VersionId>70c3d788-b842-43bd-885e-da2a2aa2ae16</VersionId></Deleted></DeleteResult>
```

State change is explicit: same version id `70c3d788-...` refused while held, then reported
under `<Deleted>` once the hold is `OFF`.

### Transitional case — an *unversioned* delete creates a delete marker (not blocked)

On a versioned bucket, a delete **without** `--version-id` does not remove the protected
version; it creates a **delete marker** and is not itself blocked
(`q2-transitional-trace.txt`, lines 93–120):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T22:45:39.500] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=cbac4483d2967b83a9b56b93e88c5478798438013f3c536036d2ac0a986e1c07
127.0.0.1:9000 Content-Length: 74
127.0.0.1:9000 Content-Md5: 5vntUtVTbDRxYxmuYT6E0A==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 6de2624a1072a129c7d295a40475895f56d7cb3a97f5252770e865517e06e76e
127.0.0.1:9000 X-Amz-Date: 20260706T224539Z
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>trans.txt</Key></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:45:39.506] [ Duration 5.335ms TTFB 5.316494ms ↑ 179 B  ↓ 271 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD4C988361D81
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 Content-Length: 271
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><DeleteMarker>true</DeleteMarker><DeleteMarkerVersionId>e12b881a-43af-468f-b17c-5243772af1b8</DeleteMarkerVersionId><Key>trans.txt</Key></Deleted></DeleteResult>
```

`mc` reports `Created delete marker`. A subsequent `mc ls --versions` shows the new delete
marker as the latest version on top of the retained, still-protected original version
(`q2-transitional-trace.txt`, line 205 — the `ListVersions` result):

```text
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>wormtest</Name><Prefix>trans.txt</Prefix><KeyMarker></KeyMarker><NextVersionIdMarker></NextVersionIdMarker><VersionIdMarker></VersionIdMarker><MaxKeys>1000</MaxKeys><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated><DeleteMarker><Key>trans.txt</Key><LastModified>2026-07-06T22:45:39.501Z</LastModified><ETag></ETag><Size>0</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass><IsLatest>true</IsLatest><VersionId>e12b881a-43af-468f-b17c-5243772af1b8</VersionId></DeleteMarker><Version><Key>trans.txt</Key><LastModified>2026-07-06T22:45:37.403Z</LastModified><ETag>&#34;a1192ec4e348c86a2d14f2e272a2f528&#34;</ETag><Size>21</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass><IsLatest>false</IsLatest><VersionId>83159f82-8b86-4aef-802b-636793afed4a</VersionId></Version><EncodingType>url</EncodingType></ListVersionsResult>
```

The delete marker `e12b881a-...` is `IsLatest=true`; the original `83159f82-...` (21 bytes)
is retained beneath it.

**Root cause / the exact log entry.** The interactive versioned-delete path enforces
retention in `enforceRetentionBypassForDelete` [`cmd/bucket-object-lock.go:84`], invoked from
the single-object handler `DeleteObjectHandler` [`cmd/object-handlers.go:2509`] via the
`opts.SetEvalRetentionBypassFn` callback [`cmd/object-handlers.go:2598`] at call site
[`cmd/object-handlers.go:2601`], and from the batch handler `DeleteMultipleObjectsHandler`
[`cmd/bucket-handlers.go:416`] at [`cmd/bucket-handlers.go:573`]. That function checks Legal
Hold first, then Compliance (which it refuses for every caller including root), then
Governance (bypassable only with the `s3:BypassGovernanceRetention` permission plus the
`x-amz-bypass-governance-retention` header); the current lock state comes from
`BucketObjectLockSys.Get` [`cmd/bucket-object-lock.go:38`]. A blocked delete returns the
`ObjectLocked{}` error [`cmd/object-handlers.go:2386`], mapped to `ErrObjectLocked`
[`cmd/api-errors.go:2299`] (enum at [`cmd/api-errors.go:206`]), whose canonical definition is
Code `InvalidRequest`, Description **"Object is WORM protected and cannot be overwritten"**,
HTTP 400 — the map entry begins at [`cmd/api-errors.go:1059`] with the description at
[`cmd/api-errors.go:1061`]. (The related `enforceRetentionForDeletion`
[`cmd/bucket-object-lock.go:54`] is the lifecycle/ILM data-scanner path, called at
[`cmd/data-scanner.go:1078`] and [`cmd/data-scanner.go:1246`], not the interactive handler.)
The put-time guard is `checkPutObjectLockAllowed` [`cmd/bucket-object-lock.go:245`].

**Nuance honored.** A bucket *default* retention is inherited by newly-PUT objects; to
demonstrate Compliance-never-bypassable cleanly, Compliance was applied **per object** on a
bucket **without** a conflicting Governance default (otherwise the object would effectively be
Governance and `--bypass` would succeed). MinIO's `docs/bucket/retention/README.md` documents
Governance, Compliance, and Legal Hold, with per-object headers taking precedence over the
bucket default (legal hold `:46`, Compliance `:47`).

**Answer summary (Q2).** The specific runtime entry for a blocked delete is the
`DeleteMultipleObjects`/`DeleteObject` trace whose response embeds
`<Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>`
for the target key/version. It appears for Governance (no bypass), Compliance (always, even
with bypass/root), and Legal Hold (while held); it is absent for Governance-with-bypass, for
a cleared Legal Hold, and for an unversioned delete (which instead yields a delete marker).

---

## Q3 — Bit rot detection on read

> **Question (verbatim):** *"analyze how the system handles unauthorized manual data
> corruption within the storage backend. Trigger a bit rot detection event and identify the
> specific runtime logs generated during a subsequent get request."*

Bit-rot verification is a property of the **erasure-coded** read path, so a four-drive
deployment was used (a single-drive/FS backend does not exercise it):

```bash
/tmp/blitzy/minio-bin/minio server /tmp/blitzy/ec/d1 /tmp/blitzy/ec/d2 \
    /tmp/blitzy/ec/d3 /tmp/blitzy/ec/d4 --address :9020 --console-address :9021
```

The banner reports `Formatting 1st pool, 1 set(s), 4 drives per set`, and `mc admin info`
reports `4 drives online ... EC:2` (2 data + 2 parity). A 1 MiB object was written to
`ecbucket` (md5 `2dd35c41238dd1686cfc531341b00d21`) and stored as `part.1` on **all four**
drives under `ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1` — each shard
is 524320 bytes (512 KiB of data plus a 32-byte inline HighwayHash bit-rot checksum).

### Recoverable corruption (≤ parity shards) — GET returns correct bytes; shard reconstructed

One backend shard was zeroed directly on disk with
`dd if=/dev/zero of=/tmp/blitzy/ec/d2/ecbucket/bigobj.bin/774f9782-.../part.1 bs=1024 count=64 seek=100 conv=notrunc`.
A subsequent `GET` returned the **correct** bytes (md5 matched the original), and the trace
shows the read served from surviving drives plus an auto-heal-on-read
(`q3-trace.txt`, lines 142–204):

```text
127.0.0.1:9020  [STORAGE storage.ReadFileStream] [2026-07-06T22:48:06.819] /tmp/blitzy/ec/d2 ecbucket bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 total-errs-availability=0 total-errs-timeout=0 26.362µs 512 KiB
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:06.819] /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 28.423µs
127.0.0.1:9020  [STORAGE storage.ReadFileStream] [2026-07-06T22:48:06.819] /tmp/blitzy/ec/d1 ecbucket bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 total-errs-availability=0 total-errs-timeout=0 47.331µs 512 KiB
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:06.820] /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 19.491µs
127.0.0.1:9020  [STORAGE storage.ReadFileStream] [2026-07-06T22:48:06.820] /tmp/blitzy/ec/d3 ecbucket bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 total-errs-availability=0 total-errs-timeout=0 32.076µs 512 KiB
127.0.0.1:9020 [REQUEST s3.GetObject] [2026-07-06T22:48:06.819] [Client IP: 127.0.0.1]
127.0.0.1:9020 GET /ecbucket/bigobj.bin
127.0.0.1:9020 Proto: HTTP/1.1
127.0.0.1:9020 Host: 127.0.0.1:9020
127.0.0.1:9020 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9020 X-Amz-Date: 20260706T224806Z
127.0.0.1:9020 Accept-Encoding: identity
127.0.0.1:9020 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=858d5e30d4d04f4d89828a20e0357e78900a401af1f2be64252a5346a1207025
127.0.0.1:9020 Content-Length: 0
127.0.0.1:9020 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9020 <BLOB>
127.0.0.1:9020 [RESPONSE] [2026-07-06T22:48:06.833] [ Duration 14.171ms TTFB 13.261821ms ↑ 93 B  ↓ 1.0 MiB ]
127.0.0.1:9020 200 OK
127.0.0.1:9020 Accept-Ranges: bytes
127.0.0.1:9020 ETag: "2dd35c41238dd1686cfc531341b00d21"
127.0.0.1:9020 X-Content-Type-Options: nosniff
127.0.0.1:9020 Content-Type: application/octet-stream
127.0.0.1:9020 Last-Modified: Mon, 06 Jul 2026 22:47:39 GMT
127.0.0.1:9020 Server: MinIO
127.0.0.1:9020 Vary: Origin,Accept-Encoding
127.0.0.1:9020 X-Xss-Protection: 1; mode=block
127.0.0.1:9020 X-Amz-Id-2: 64084d77053f4fbd8e5405e6d076258766008f28b08394c799d1e979f6b5f3d6
127.0.0.1:9020 X-Amz-Request-Id: 18BFD4EBD5148FB7
127.0.0.1:9020 X-Ratelimit-Limit: 565440
127.0.0.1:9020 Content-Length: 1048576
127.0.0.1:9020 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9020 X-Ratelimit-Remaining: 565440
127.0.0.1:9020 <BLOB>
127.0.0.1:9020 
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4/.minio.sys/format.json 9.622µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2/.minio.sys/format.json 12.287µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3/.minio.sys/format.json 8.372µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4/ecbucket/bigobj.bin/xl.meta 30.552µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1/.minio.sys/format.json 10.584µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/xl.meta 31.692µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/xl.meta 25.237µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 103.068µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/xl.meta 32.107µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 103.233µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 92.047µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 80.698µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4/ecbucket/bigobj.bin/xl.meta 14.906µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/xl.meta 19.4µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 46.525µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/xl.meta 26.44µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2 ecbucket bigobj.bin total-errs-timeout=0 total-errs-availability=0 56.129µs
127.0.0.1:9020  [OS os.OpenFileR] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/xl.meta 26.758µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 82.834µs
127.0.0.1:9020  [STORAGE storage.ReadVersion] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 78.233µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 5.3µs
127.0.0.1:9020  [STORAGE storage.CheckParts] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d1 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 19.27µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 3.396µs
127.0.0.1:9020  [STORAGE storage.CheckParts] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d2 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 9.696µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 3.233µs
127.0.0.1:9020  [STORAGE storage.CheckParts] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d3 ecbucket bigobj.bin total-errs-timeout=0 total-errs-availability=0 23.271µs
127.0.0.1:9020  [OS os.Lstat] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4/ecbucket/bigobj.bin/774f9782-cf06-431d-8310-82e111d9277a/part.1 3.365µs
127.0.0.1:9020  [STORAGE storage.CheckParts] [2026-07-06T22:48:07.834] /tmp/blitzy/ec/d4 ecbucket bigobj.bin total-errs-availability=0 total-errs-timeout=0 8.385µs
127.0.0.1:9020  [HEALING heal.Object] [2026-07-06T22:48:07.834] ecbucket/bigobj.bin version-id=null disks=4 dry=false mode=0 remove=true 287.42µs 1.0 MiB
```

The `GetObject` returns `200 OK` with the original ETag `"2dd35c41238dd1686cfc531341b00d21"`,
and the `[HEALING heal.Object] ecbucket/bigobj.bin ... disks=4 ... remove=true` line shows
the object being reconstructed on read. **At the default console log level `errFileCorrupt`
is not printed** — reconstruction is transparent; the runtime evidence lives in the trace
stream (reads from surviving drives + auto-heal-on-read).

**Heal scan depth matters.** Running the healer on a recoverable object shows that the
**default (normal) scan is metadata-level** and does not repair in-place *data* bit rot,
while a **deep scan** detects and reconstructs the corrupted shard from parity
(`q3-heal.txt` — a clean re-demonstration on a fresh object `healdemo.bin`):

```text
$ mc admin heal -r ec/ecbucket/healdemo.bin        # default (normal, metadata-level) scan
[Green  ->  Green] ecbucket/
[Green  ->  Green] ecbucket/healdemo.bin
Healed:	0/1 objects; 1024 KiB in 1s

--- d2 shard md5 after NORMAL heal (still corrupted if metadata-only) ---
ecbb6321a3f18a4e43c6b0180e876e5b
$ mc admin heal -r --scan deep ec/ecbucket/healdemo.bin   # deep (data-level) scan
[Green  ->  Green] ecbucket/
[Yellow ->  Green] ecbucket/healdemo.bin
Healed:	1/1 objects; 1024 KiB in 1s

--- d2 shard md5 after DEEP heal (restored to healthy) ---
e00564d89b0377749f3c85d157814caf
```

The corrupted shard md5 is unchanged after the normal scan (`[Green -> Green]`,
`Healed: 0/1`) but is restored to its original healthy value after the deep scan
(`[Yellow -> Green]`, `Healed: 1/1`) — confirming that deep data bit rot is caught on the
**read path** (or by an explicit deep heal), not by the default metadata heal.

### Unrecoverable corruption (> parity shards) — client sees `SlowDownRead` / HTTP 503

Corrupting **3 of 4** shards (only `d4` intact — beyond the `EC:2` parity of 2) and issuing a
`GET` makes the object unreadable. The client fails with
`Resource requested is unreadable, please reduce your request rate`; the trace shows
`503 Service Unavailable` with `Retry-After: 60` and an S3 `SlowDownRead` error
(`q3-fail-trace.txt`, lines 149–178):

```text
127.0.0.1:9020 [REQUEST s3.GetObject] [2026-07-06T22:49:52.746] [Client IP: 127.0.0.1]
127.0.0.1:9020 GET /ecbucket/bigobj.bin
127.0.0.1:9020 Proto: HTTP/1.1
127.0.0.1:9020 Host: 127.0.0.1:9020
127.0.0.1:9020 Content-Length: 0
127.0.0.1:9020 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9020 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9020 X-Amz-Date: 20260706T224952Z
127.0.0.1:9020 Accept-Encoding: identity
127.0.0.1:9020 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=682d99d9349c7e576dd3413edfae27a229641f13be077e7d16cb498ea64d46e3
127.0.0.1:9020 <BLOB>
127.0.0.1:9020 [RESPONSE] [2026-07-06T22:49:52.748] [ Duration 1.616ms TTFB 1.603826ms ↑ 93 B  ↓ 378 B ]
127.0.0.1:9020 503 Service Unavailable
127.0.0.1:9020 Server: MinIO
127.0.0.1:9020 X-Ratelimit-Limit: 565440
127.0.0.1:9020 Accept-Ranges: bytes
127.0.0.1:9020 Content-Length: 378
127.0.0.1:9020 Retry-After: 60
127.0.0.1:9020 X-Xss-Protection: 1; mode=block
127.0.0.1:9020 Content-Type: application/xml
127.0.0.1:9020 Last-Modified: Mon, 06 Jul 2026 22:47:39 GMT
127.0.0.1:9020 Vary: Origin,Accept-Encoding
127.0.0.1:9020 ETag: "2dd35c41238dd1686cfc531341b00d21"
127.0.0.1:9020 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9020 X-Amz-Id-2: 64084d77053f4fbd8e5405e6d076258766008f28b08394c799d1e979f6b5f3d6
127.0.0.1:9020 X-Amz-Request-Id: 18BFD5047ED51573
127.0.0.1:9020 X-Content-Type-Options: nosniff
127.0.0.1:9020 X-Ratelimit-Remaining: 565440
127.0.0.1:9020 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>bigobj.bin</Key><BucketName>ecbucket</BucketName><Resource>/ecbucket/bigobj.bin</Resource><RequestId>18BFD5047ED51573</RequestId><HostId>64084d77053f4fbd8e5405e6d076258766008f28b08394c799d1e979f6b5f3d6</HostId></Error>
```

> **Correction to the AAP phrasing (observed).** The internal sentinel is
> `errFileCorrupt = StorageErr("file is corrupted")` [`cmd/storage-errors.go:104`], raised at
> the storage layer. But the **client-facing** result of unrecoverable corruption is S3
> `<Code>SlowDownRead</Code>` at **HTTP 503 Service Unavailable** (with `Retry-After: 60`),
> a *retryable* error — not a literal "file is corrupted" message. The two must not be
> conflated: `errFileCorrupt` is internal; `SlowDownRead`/503 is what the S3 client sees. The
> request was retried 10× by the client, each returning 503.

**Root cause / detection chain.** The per-shard verifier is built by `NewBitrotVerifier`
[`cmd/bitrot.go:83`] over the algorithm map `bitrotAlgorithms` [`cmd/bitrot.go:39`] and
evaluated by `bitrotVerify` [`cmd/bitrot.go:158`], which returns `errFileCorrupt` on a size
or checksum mismatch (returns at [`cmd/bitrot.go:163`, `:166`, `:178`, `:201`, `:206`);
sentinel at [`cmd/storage-errors.go:104`]). The read path that verifies during a GET is
`ReadFile(...verifier *BitrotVerifier)` [`cmd/xl-storage.go:1875`]; the streaming verifier
surfaces the mismatch at [`cmd/bitrot-streaming.go:185`]. Reconstruction from parity catches
`errors.Is(err, errFileCorrupt)` in the decoder [`cmd/erasure-decode.go:197`, `:228`, `:287`,
`:339`]; healing uses `errPartMissingOrCorrupt` [`cmd/erasure-healing.go:152`], with the
failure message "unable to heal %d corrupted blocks on drives" [`cmd/erasure-healing.go:238`]
and the object healer `HealObject` [`cmd/erasure-healing.go:1039`]. When corrupt shards exceed
parity, the read cannot reach quorum and the handler maps it to `ErrSlowDownRead` (Code
`SlowDownRead`, HTTP 503) — the map entry is at [`cmd/api-errors.go:869-872`] and the
`InsufficientReadQuorum → ErrSlowDownRead` mapping at [`cmd/api-errors.go:2316-2317`].
MinIO's erasure documentation describes this HighwayHash-based per-shard protection with
reconstruction from parity.

**Answer summary (Q3).** Manual on-disk corruption is detected on the read path by the
per-shard HighwayHash verifier (`bitrotVerify` → `errFileCorrupt`). If corrupt shards are
within parity, the GET still returns the **correct** bytes and the shard is reconstructed
(auto-heal-on-read; deep heal also repairs it); if corrupt shards exceed parity, the GET
fails with S3 `SlowDownRead` / HTTP 503.

---

## Q4 — STS session-policy enforcement (intersection)

> **Question (verbatim):** *"verify that when a user gets temporary credentials, minio is
> able to enforce the session policy on that user. You need to give me runtime test output
> to prove this behavior."*

A parent user `stsparent` was created with the built-in `readwrite` policy (`s3:*` on
everything). Temporary credentials were then obtained via STS `AssumeRole` while supplying a
**restrictive inline session policy** allowing only `s3:PutObject`/`s3:GetObject` on
`stsbucket/*`. The temporary credentials were then exercised. The test program
(`minio-go/v7 v7.0.80`) was run with:

```bash
cd /tmp/blitzy/scripts/q4 && ./q4prog        # builds against the repo's pinned module graph
```

Program stdout (verbatim, `q4-sts.txt`):

```text
PARENT-ACCESSKEY: stsparent
TEMP-ACCESSKEY: Y9ZODVYTJQVMILNIX5J3
SESSION-TOKEN-PRESENT: true
SIGNER-TYPE: S3v4
IN-SESSION  PutObject stsbucket/in.txt  => ALLOWED (success)
OUT-OF-SESSION PutObject otherbucket/out.txt => DENIED: Access Denied.
IN-SESSION  GetObject stsbucket/in.txt  => ALLOWED (read 31 bytes)
OUT-OF-SESSION ListBuckets            => DENIED: Access Denied.
```

Both **in-session** actions (PutObject/GetObject on `stsbucket`) are **ALLOWED**; both
**out-of-session** actions (PutObject on `otherbucket`, and `ListBuckets`) are **DENIED** —
even though the parent `readwrite` policy allows them. The effective permission set is the
**intersection** of the parent policy and the session policy.

**Server-side trace.** `AssumeRole` returns `200 OK` with a temporary access key and a
`SessionToken` JWT (`q4-trace.txt`, lines 74–99):

```text
127.0.0.1:9000  [STORAGE storage.Delete] [2026-07-06T22:55:28.918] /tmp/blitzy/data .minio.sys/tmp 9787ca07-2222-458a-80d2-d2165e342982 total-errs-availability=0 total-errs-timeout=0 38.251µs
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-06T22:55:28.915] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 299
127.0.0.1:9000 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Date: 20260706T225528Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=stsparent/20260706/us-east-1/sts/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=99af6ca58dd2ee7584fab4211aeab19b42ae9d313e845df55af964a156ca5034
127.0.0.1:9000 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3APutObject%22%2C%22s3%3AGetObject%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Astsbucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:55:28.918] [ Duration 2.557ms TTFB 2.552204ms ↑ 384 B  ↓ 1.0 KiB ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 1035
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Request-Id: 18BFD552C413C41D
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><AssumedRoleUser><Arn></Arn><AssumeRoleId></AssumeRoleId></AssumedRoleUser><Credentials><AccessKeyId>Y9ZODVYTJQVMILNIX5J3</AccessKeyId><SecretAccessKey><redacted-expired-STS-secret></SecretAccessKey><SessionToken>eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJZOVpPRFZZVEpRVk1JTE5JWDVKMyIsImV4cCI6MTc4MzM4MjEyOCwicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02UjJWMFQySnFaV04wSWl3aWN6TTZVSFYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted></SessionToken><Expiration>2026-07-06T23:55:28Z</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>18BFD552C413C41D</RequestId></ResponseMetadata></AssumeRoleResponse>
```

The `Policy=...` form field carries the URL-encoded session policy, and the request is signed
by the parent (`Credential=stsparent/.../sts/aws4_request`). The returned `SessionToken` is a
JWT; decoding its payload proves the session policy is embedded as a base64 claim:

```text
$ # base64url-decode the JWT payload of the returned SessionToken
JWT claims keys: ['accessKey', 'exp', 'parent', 'sessionPolicy']
parent: stsparent
decoded sessionPolicy: {"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::stsbucket/*"]}]}
```

**In-session PutObject → 200 OK** (`q4-trace.txt`, lines 149–172):

```text
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-06T22:55:28.920] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /stsbucket/in.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 204
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 31
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJZOVpPRFZZVEpRVk1JTE5JWDVKMyIsImV4cCI6MTc4MzM4MjEyOCwicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02UjJWMFQySnFaV04wSWl3aWN6TTZVSFYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=Y9ZODVYTJQVMILNIX5J3/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-security-token,Signature=cd61964bc3dc47c69878975e284c93532dfeb9973eb9ca168d91b09a84f40db5
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Date: 20260706T225528Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:55:28.923] [ Duration 3.437ms TTFB 3.418116ms ↑ 344 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 ETag: "6ea8c232b6d583f3bf97836b1097fcb6"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD552C456F44D
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
```

The request is signed with the **temporary** access key plus an `X-Amz-Security-Token`, and
returns `200 OK` with an ETag.

**Out-of-session PutObject → 403 AccessDenied** (`q4-trace.txt`, lines 206–234):

```text
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-06T22:55:28.924] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /otherbucket/out.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Date: 20260706T225528Z
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJZOVpPRFZZVEpRVk1JTE5JWDVKMyIsImV4cCI6MTc4MzM4MjEyOCwicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02UjJWMFQySnFaV04wSWl3aWN6TTZVSFYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=Y9ZODVYTJQVMILNIX5J3/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-security-token,Signature=bcdc43a6255b91205107f784358a19aef2916c1e05e1e2f967ba90e80734d0d9
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 31
127.0.0.1:9000 Content-Length: 204
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:55:28.924] [ Duration 210µs TTFB 202.491µs ↑ 140 B  ↓ 327 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Ratelimit-Limit: 1141211
127.0.0.1:9000 X-Ratelimit-Remaining: 1141211
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 327
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD552C49A1F87
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>out.txt</Key><BucketName>otherbucket</BucketName><Resource>/otherbucket/out.txt</Resource><RequestId>18BFD552C49A1F87</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Same temporary credentials, but writing to `otherbucket` (outside the session policy's
`stsbucket/*` resource) is rejected `403` with `<Code>AccessDenied</Code>` — despite the
parent `readwrite` policy allowing `s3:*`. (`ListBuckets` with the same credentials is
likewise denied — `q4-trace.txt` lines 269–297 — because the session policy does not grant
`s3:ListAllMyBuckets`.)

**Root cause / intersection semantics.** On the STS handler, the inline session policy is
read via `form.Get(stsPolicy)` [`cmd/sts-handlers.go:99`], parsed by `policy.ParseConfig`
[`cmd/sts-handlers.go:104`], size-checked against `maxSTSSessionPolicySize` (2048 bytes;
constant at [`cmd/sts-handlers.go:89`], check at [`cmd/sts-handlers.go:123`]), and embedded as
a base64 credential claim `c[policy.SessionPolicyName] = base64.StdEncoding.EncodeToString(...)`
[`cmd/sts-handlers.go:127`] inside `AssumeRole` [`cmd/sts-handlers.go:256`]. At authorization
time `IsAllowedSTS` [`cmd/iam.go:2242`] returns
`isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))`
[`cmd/iam.go:2311-2312`] — the logical **AND** of the session-policy decision and the parent
policy decision, i.e. the intersection. The session-policy decision is computed by
`isAllowedBySessionPolicy` [`cmd/iam.go:2381`], which reads the embedded claim named by
`sessionPolicyNameExtracted` [`cmd/iam.go:2136`]. (The parallel service-account helper
`isAllowedBySessionPolicyForServiceAccount` [`cmd/iam.go:2320`] is a different, non-AssumeRole
path.)

**Documented behavior (confirmation).** MinIO's `docs/sts/assume-role.md` states the session's
permissions are the intersection of the assumed policy and the inline session policy and that
the session policy cannot grant more than the parent (`:39`), with a maximum length of 2048
bytes (`:44`).

**Answer summary (Q4).** MinIO enforces the session policy: the temporary credentials can
perform an action only if **both** the parent policy and the session policy allow it. Proven
by runtime output — in-session PutObject/GetObject succeeded (`200`), while out-of-session
PutObject and ListBuckets were denied (`403 AccessDenied`).

---

## Q5 — Privilege escalation via user mappings

> **Question (verbatim):** *"Show me test output to prove that a user with basic access
> cannot promote themselves to a console admin by modifying the user mappings. Identify the
> root cause of the user mappings modification behavior that you observe."*

A basic, non-administrative user `basicuser` was created with an S3-only policy `basic-only`
(file `basic-policy.json`):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  {
   "Effect": "Allow",
   "Action": ["s3:GetObject", "s3:PutObject"],
   "Resource": ["arn:aws:s3:::smoke/*"]
  }
 ]
}
```

### Attack path 1 — admin API (attach `consoleAdmin` to self)

Using **its own** credentials, `basicuser` attempted to attach `consoleAdmin` to itself and
to perform other admin actions. Every attempt was denied (verbatim client output,
`q5-client.txt`):

```text
$ mc admin policy attach basic consoleAdmin --user basicuser
mc: <ERROR> Unable to make user/group policy association. Access Denied.
$ mc admin user add basic eviluser <redacted-test-pw>
mc: <ERROR> Unable to add new user. Access Denied.
$ mc admin policy list basic
mc: <ERROR> Unable to list policy. Access Denied.
$ mc admin user enable basic basicuser
mc: <ERROR> Unable to enable user. Access Denied.
```

Server-side trace of the primary attempt — `admin.AttachDetachPolicyBuiltin`
(`q5-trace.txt`, lines 58–83):

```text
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-06T22:57:49.000] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260706T225749Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=basicuser/20260706//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=cbc312bc31e352aa929d9f4dec74e2d5ca1e0db9829753fc6ad2e6c415842ae4
127.0.0.1:9000 Content-Length: 105
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 2089de70aaee5068d714b94c02e35bfc371076c8da60f5c0f9a95ba0f3e24fa4
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:57:49.001] [ Duration 292µs TTFB 288.989µs ↑ 106 B  ↓ 213 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 213
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD57361CE98B4
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18BFD57361CE98B4","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

```

The request is signed by `Credential=basicuser/...` and is rejected in **292µs** with
`403 Forbidden` and
`{"Code":"AccessDenied",...,"Resource":"/minio/admin/v3/idp/builtin/policy/attach"}` — the
sub-millisecond latency reflects that the guard denies **before** any mapping mutation (before
the request body is even read). The other three attempts fail identically (`q5-trace.txt` —
`admin.AddUser` → `PUT /minio/admin/v3/add-user`, `admin.ListCannedPolicies` →
`GET /minio/admin/v3/list-canned-policies`, `admin.SetUserStatus` →
`PUT /minio/admin/v3/set-user-status`), each returning `403` with the same `AccessDenied`
JSON keyed to its own resource path.

### Attack path 2 — direct storage (edit the mapping on disk)

User→policy mappings live under the reserved `.minio.sys` bucket. `basicuser` cannot reach it.
At the **client layer**, `mc` rejects the reserved name before sending
(`q5-directstore-client.txt`):

```text
$ mc cp evil-mapping.json basic/.minio.sys/policydb/users/basicuser/identity.json
`/tmp/blitzy/out/evil-mapping.json` -> `basic/.minio.sys/policydb/users/basicuser/identity.json`
mc: <ERROR> Failed to copy `/tmp/blitzy/out/evil-mapping.json`. Bucket name contains invalid characters
$ mc ls basic/.minio.sys
mc: <ERROR> Unable to list folder. Bucket name contains invalid characters
$ mc mb basic/.minio.sys
mc: <ERROR> Unable to make bucket `basic/.minio.sys`. Bucket name contains invalid characters
```

To confirm the **server** also blocks it (bypassing client-side name validation), a raw
SigV4-signed `PUT` targeting the exact on-disk mapping key was sent as `basicuser`
(program `q5raw`, using the `minio-go` signer). The server rejects it
(`q5-rawreserved.txt`):

```text
SIGNED-AS: basicuser
TARGET: http://127.0.0.1:9000/.minio.sys/policydb/users/basicuser/identity.json
HTTP-STATUS: 403 Forbidden
RESPONSE-BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AllAccessDisabled</Code><Message>All access to this resource has been disabled.</Message><Resource>/.minio.sys/policydb/users/basicuser/identity.json</Resource><RequestId>18BFD588DEA6C0F5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace of that raw request (`q5-rawreserved-trace.txt`, lines 58–81):

```text
127.0.0.1:9000 [REQUEST handler.ValidRequest] [2026-07-06T22:59:21.289] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /.minio.sys/policydb/users/basicuser/identity.json
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=basicuser/20260706/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=6b8de9bd332bacc4dbe7e96c0263b03098c3c31a707a8707f45eb943a4ffb55b
127.0.0.1:9000 Content-Length: 16
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Date: 20260706T225921Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-06T22:59:21.289] [ Duration 113µs TTFB 111.264µs ↑ 72 B  ↓ 340 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Length: 340
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD588DEA6C0F5
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AllAccessDisabled</Code><Message>All access to this resource has been disabled.</Message><Resource>/.minio.sys/policydb/users/basicuser/identity.json</Resource><RequestId>18BFD588DEA6C0F5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The server returns `403` with `<Code>AllAccessDisabled</Code>` for
`/.minio.sys/policydb/users/basicuser/identity.json` — the reserved bucket is unreachable by
any S3 caller.

**Root cause (the identified guard).** Every admin user-mapping API is gated by
`validateAdminReq` [`cmd/admin-handler-utils.go:37`]. For the self-promotion attempt,
`AttachDetachPolicyBuiltin` [`cmd/admin-handlers-users.go:1908`] begins with:

```go
// cmd/admin-handlers-users.go:1911-1915
objectAPI, cred := validateAdminReq(ctx, w, r, policy.UpdatePolicyAssociationAction,
    policy.AttachPolicyAdminAction)
if objectAPI == nil {
    return
}
```

`validateAdminReq` calls `checkAdminRequestAuth` [`cmd/auth-handler.go:189`] for the required
admin action(s); when the caller lacks permission it writes the `AccessDenied` response and
returns a **nil `ObjectLayer`** (and an empty `auth.Credentials{}`), so the `if objectAPI ==
nil { return }` guard aborts the handler **before any user→policy mapping is mutated** — matching
the observed 292µs early abort. The same guard fronts `SetPolicyForUserOrGroup`
[`cmd/admin-handlers-users.go:1770`], `AddUser` [`cmd/admin-handlers-users.go:444`], and
`SetUserStatus` [`cmd/admin-handlers-users.go:406`]. The direct-storage path is closed because
the mappings live under `minioMetaBucket = ".minio.sys"` [`cmd/object-api-utils.go:60`] at
`policyDBPrefix = "policydb/"` [`cmd/iam-object-store.go:478`] /
`policyDBUsersListKey = "policydb/users/"` [`cmd/iam-object-store.go:479`]; the predicate
`isReservedOrInvalidBucket` [`cmd/object-api-utils.go:472`] treats `.minio.sys` as reserved,
and access is refused with `ErrAllAccessDisabled` (Code `AllAccessDisabled`, HTTP 403; struct
[`cmd/api-errors.go:764-768`], enum [`cmd/api-errors.go:166`]).

**Answer summary (Q5).** A basic user cannot promote itself: the admin APIs that modify user
mappings are all gated by `validateAdminReq` [`cmd/admin-handler-utils.go:37`], which denies
the unprivileged caller and aborts before any mutation; and the on-disk mappings under
`.minio.sys/policydb/` are unreachable by S3 users (client rejects the name; server returns
`AllAccessDisabled`). The root cause of the observed "cannot modify user mappings" behavior is
the `validateAdminReq` authorization guard.

---

## Coverage Pass — every named item addressed

| Q | Mode / mechanism exercised | Observed result | Root cause `file:line` |
|---|---|---|---|
| Q1 | Bucket **default/auto** encryption (`MINIO_KMS_AUTO_ENCRYPTION=on`) | Unencrypted PUT **succeeds**, SSE-KMS injected; resp `X-Amz-Server-Side-Encryption: aws:kms` | `internal/crypto/auto-encryption.go:31,37`; `cmd/config-current.go:532`; `internal/bucket/encryption/bucket-sse-config.go:135,140-141`; `cmd/object-handlers.go:1745,1896,1999,2015`; `cmd/encryption-v1.go:466` |
| Q1 | Encryption-mandating **IAM deny** (`Null` on `s3:x-amz-server-side-encryption`) | Unencrypted PUT **403 AccessDenied**; encrypted PUT 200 | deny-wins `pkg/v3@v3.0.22 policy/policy.go:173`; cond key `policy/condition/keyname.go:66`; dispatch `cmd/iam.go:2437,2448,2482` |
| Q1 | Correction #1: bucket/resource policy vs IAM policy | Bucket-policy deny does **not** block authenticated user | `cmd/iam.go:2482` (regular user evaluates combined IAM policy only) |
| Q2 | **Governance** without bypass | Blocked: `Object is WORM protected and cannot be overwritten` | `cmd/bucket-object-lock.go:84`; `cmd/api-errors.go:1061,2299,206` |
| Q2 | **Governance** with `--bypass` + `x-amz-bypass-governance-retention` | **Deleted** | `cmd/bucket-object-lock.go:84` (governance branch) |
| Q2 | **Compliance** (even with bypass/root) | **Still blocked** | `cmd/bucket-object-lock.go:84` (compliance branch) |
| Q2 | **Legal Hold** set → cleared | Blocked while held → **deleted** after `OFF` | `cmd/bucket-object-lock.go:84` (legal-hold branch); `s3.PutObjectLegalHold` |
| Q2 | **Transitional** unversioned delete | **Delete marker** created, not blocked | handler `cmd/object-handlers.go:2509`; batch `cmd/bucket-handlers.go:416,573` |
| Q2 | Call sites / ILM note | interactive `cmd/object-handlers.go:2601`, `cmd/bucket-handlers.go:573`; ILM `cmd/data-scanner.go:1078,1246` | `cmd/bucket-object-lock.go:54` (scanner path) |
| Q3 | Erasure prerequisite (`EC:2`, 4 drives) | `part.1` on all 4 drives (512 KiB + 32B checksum) | `cmd/setup-type.go:28,31,34,37` |
| Q3 | **Recoverable** (≤ parity) corruption | GET returns **correct** bytes; auto-heal-on-read | `cmd/bitrot.go:83,158`; `cmd/xl-storage.go:1875`; `cmd/erasure-healing.go:1039` |
| Q3 | Default vs **deep** heal | normal `[Green→Green]` 0/1; deep `[Yellow→Green]` 1/1 | `cmd/erasure-healing.go:152,238` |
| Q3 | **Unrecoverable** (> parity) corruption | client **`SlowDownRead` / HTTP 503** (Correction #2) | internal `cmd/storage-errors.go:104`; client `cmd/api-errors.go:869-872,2316-2317` |
| Q3 | detection returns | `errFileCorrupt` on size/hash mismatch | `cmd/bitrot.go:163,166,178,201,206`; `cmd/bitrot-streaming.go:185`; `cmd/erasure-decode.go:197,228,287,339` |
| Q4 | AssumeRole + inline session policy | temp creds minted; session policy embedded as JWT claim | `cmd/sts-handlers.go:99,104,123,127,256`; size `:89` (2048) |
| Q4 | **In-session** PutObject/GetObject | **ALLOWED** (200) | `cmd/iam.go:2311-2312` (AND) |
| Q4 | **Out-of-session** PutObject/ListBuckets | **DENIED** (403) | `cmd/iam.go:2242,2381,2136` |
| Q5 | Admin API self-attach `consoleAdmin` (+ AddUser/List/SetUserStatus) | **403 AccessDenied**, aborts pre-mutation (292µs) | `cmd/admin-handler-utils.go:37`; `cmd/auth-handler.go:189`; `cmd/admin-handlers-users.go:1908,1770,444,406` |
| Q5 | Direct-storage into `.minio.sys` (client + server) | client "invalid characters"; server **`AllAccessDisabled`** 403 | `cmd/object-api-utils.go:60,472`; `cmd/iam-object-store.go:478,479`; `cmd/api-errors.go:764-768` |

## Notes — corrections, nuances, and inferred items

**Three corrections to the AAP body (all empirically verified):**

1. **Q1 deny must be an IAM/session policy, not a bucket/resource policy.** A bucket-policy
   `Deny s3:PutObject` does not block an authenticated IAM user, because for a regular user
   the dispatch evaluates only the user's combined IAM policy
   [`cmd/iam.go:2482`] (owner bypass at [`cmd/iam.go:2448`]); resource/bucket policies govern
   anonymous / cross-account requests. Verified on `:9000` (bucket-policy deny + IAM `s3:*`
   user still uploaded).
2. **Q3 unrecoverable corruption surfaces to the client as `SlowDownRead` / HTTP 503**, a
   retryable error — not a literal "file is corrupted" string. The internal sentinel
   `errFileCorrupt` [`cmd/storage-errors.go:104`] lives at the storage layer and is distinct
   from the client-facing `SlowDownRead` [`cmd/api-errors.go:869-872`].
3. **`mc` global flags precede the subcommand.** `mc --config-dir DIR alias set ...` is
   correct; placing `--config-dir` after the positionals stores empty credentials (anonymous
   requests → spurious `AccessDenied`). `mc rm` uses `--version-id VID` alone.

**Q2 call-site refinement.** The interactive versioned delete uses
`enforceRetentionBypassForDelete` [`cmd/bucket-object-lock.go:84`] (from
`cmd/object-handlers.go:2601` and `cmd/bucket-handlers.go:573`); `enforceRetentionForDeletion`
[`cmd/bucket-object-lock.go:54`] is the ILM/data-scanner path
[`cmd/data-scanner.go:1078,1246`].

**Verified anchor precision.** The exact-line anchors used here were grep-verified against the
checked-out source: the WORM message is at `cmd/api-errors.go:1061` (map entry begins
`:1059`); the `ObjectLocked`→`ErrObjectLocked` mapping at `cmd/api-errors.go:2299`; the
bit-rot read path at `cmd/xl-storage.go:1875`; and `HealObject` at
`cmd/erasure-healing.go:1039`.

**Inferred (not directly observed) items, explicitly labelled:**

- The precise handler line that emits `ErrObjectLocked` corresponds to the `ObjectLocked{}`
  return [`cmd/object-handlers.go:2386`]; the observed evidence is the client/trace error body,
  and the mapping to that struct is grounded in code (inferred linkage of body ↔ struct).
- The `[HEALING heal.Object]` trace line is the auto-heal-on-read; that it invokes
  `HealObject` [`cmd/erasure-healing.go:1039`] is grounded in code, while the trace line itself
  is the observed artifact.

**Secret handling (rule compliance).** The captured Q4 STS artifacts are expired, ephemeral, local-only credentials (minted `2026-07-06T22:55:28Z`, `exp` `2026-07-06T23:55:28Z`, from a throwaway `127.0.0.1` server that no longer exists). To comply with secret-sanitization policy, the STS `SecretAccessKey` value and the JWT **signature** segment were redacted, and a throwaway test password was replaced with `<redacted-test-pw>`. The evidentiary content is fully preserved: the JWT **header and payload** remain (they base64-decode to the `parent` and `sessionPolicy` claims — see the decoded block in Q4), and the temporary **AccessKeyId** (`Y9ZODVYTJQVMILNIX5J3`, an identifier, not a secret) is retained so the request/response blocks cross-reference correctly.

**Non-canonical values:** the Q1 KMS master key (`MINIO_KMS_SECRET_KEY`) is an
investigation-time key and is redacted as `<base64-32-bytes>`; it does not affect the observed
behavior. All version/banner values are from the canonical default build (Section 1).

**Repository integrity.** No MinIO source, test, config, or build file was modified. The only
file added to the repository is this document. All servers, data directories, scripts, and
captured logs were created under `/tmp/blitzy/...` outside the repository tree.
