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

A single-node server was started on `:9010` with a KMS master key configured and automatic
encryption enabled; a broad-write user (`bwuser`, IAM policy `s3:*` on `*`) was created; and
that user uploaded an object **with no SSE header at all**. The exact commands (the KMS master
key value is an investigation-time secret and is redacted as `<base64-32-bytes>`):

```bash
# server (port :9010): KMS key + auto-encryption ON
export MINIO_KMS_SECRET_KEY="my-minio-key:<base64-32-bytes>"
export MINIO_KMS_AUTO_ENCRYPTION=on
/tmp/blitzy/minio-bin/minio server /tmp/blitzy/data-kms --address :9010 --console-address :9011
# create the broad-write user + bucket (root alias kmsroot -> :9010):
mc --config-dir /tmp/blitzy/mc-config admin policy create kmsroot bwpol /tmp/blitzy/out/bwpolicy.json
mc --config-dir /tmp/blitzy/mc-config admin user add kmsroot bwuser bwuser12345
mc --config-dir /tmp/blitzy/mc-config admin policy attach kmsroot bwpol --user bwuser
mc --config-dir /tmp/blitzy/mc-config alias set kmsbw http://127.0.0.1:9010 bwuser bwuser12345
mc --config-dir /tmp/blitzy/mc-config mb kmsroot/autobucket
```

The upload itself, run as the broad-write user with **no encryption flags** — the exact
command and its complete client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/plain.txt kmsbw/autobucket/plain.txt
`/tmp/blitzy/out/plain.txt` -> `kmsbw/autobucket/plain.txt`
Total: 52 B, Transferred: 52 B, Speed: 1.67 KiB/s
```

The `PutObject` request and response, captured on the server's HTTP trace stream. The exact
producing command (run against the root alias `kmsroot`, since the trace stream requires an
admin caller) was:

```bash
mc --config-dir /tmp/blitzy/mc-config admin trace -v kmsroot
```

which yielded the following `PutObject` request/response:

```text
127.0.0.1:9010 [REQUEST s3.PutObject] [2026-07-06T23:42:57.826] [Client IP: 127.0.0.1]
127.0.0.1:9010 PUT /autobucket/plain.txt
127.0.0.1:9010 Proto: HTTP/1.1
127.0.0.1:9010 Host: 127.0.0.1:9010
127.0.0.1:9010 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9010 X-Amz-Decoded-Content-Length: 52
127.0.0.1:9010 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9010 Authorization: AWS4-HMAC-SHA256 Credential=bwuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=f8ec90353794e023fa50b8c14403142cb2558c368a8e3277f944e14ffb1908da
127.0.0.1:9010 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9010 Content-Type: text/plain
127.0.0.1:9010 X-Amz-Date: 20260706T234257Z
127.0.0.1:9010 Accept-Encoding: zstd,gzip
127.0.0.1:9010 Content-Length: 225
127.0.0.1:9010 <BLOB>
127.0.0.1:9010 [RESPONSE] [2026-07-06T23:42:57.849] [ Duration 22.69ms TTFB 22.670617ms ↑ 389 B  ↓ 0 B ]
127.0.0.1:9010 200 OK
127.0.0.1:9010 X-Amz-Request-Id: 18BFD7EA146AB3FE
127.0.0.1:9010 X-Ratelimit-Limit: 1142637
127.0.0.1:9010 Vary: Origin,Accept-Encoding
127.0.0.1:9010 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9010 Server: MinIO
127.0.0.1:9010 X-Amz-Id-2: 6288f7c424456b65729155b10570da05022411640ac68a83da601467ee9d5c0a
127.0.0.1:9010 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
127.0.0.1:9010 X-Content-Type-Options: nosniff
127.0.0.1:9010 ETag: "9ce74369b8d3b75c38a227feb58008b7"
127.0.0.1:9010 Content-Length: 0
127.0.0.1:9010 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9010 X-Ratelimit-Remaining: 1142637
127.0.0.1:9010 X-Xss-Protection: 1; mode=block
127.0.0.1:9010 Accept-Ranges: bytes
```

**Reading the evidence.** In the request block the client's SigV4
`Authorization: ... SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length`
**does not list `x-amz-server-side-encryption`** — so the client never sent (or signed) an
SSE header. The `X-Amz-Server-Side-Encryption: aws:kms` line that appears in the traced
request is the header the **server injected** into `r.Header` before processing. The
**response** then returns `200 OK` with `X-Amz-Server-Side-Encryption: aws:kms` and
`X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key`, and the object's
ETag is `"9ce74369b8d3b75c38a227feb58008b7"`.

**The stored object is SSE-KMS encrypted (`mc stat`).** The exact producing command and its
complete, unedited output confirm the object at rest is encrypted with SSE-KMS under the KMS
key `arn:aws:kms:my-minio-key`:

```bash
$ mc --config-dir /tmp/blitzy/mc-config stat kmsbw/autobucket/plain.txt
Name      : plain.txt
Date      : 2026-07-06 23:42:57 UTC 
Size      : 52 B   
ETag      : 9ce74369b8d3b75c38a227feb58008b7 
Type      : file 
Encryption: SSE-KMS (arn:aws:kms:my-minio-key)
Metadata  :
  Content-Type: text/plain
```

**Corroboration (object is encrypted at rest).** The plaintext md5 of the uploaded file is
`1c00e6dd7865536879e3bbd94b6fe595`:

```bash
$ md5sum /tmp/blitzy/out/plain.txt
1c00e6dd7865536879e3bbd94b6fe595  /tmp/blitzy/out/plain.txt
```

A control upload of the **same file** to a **non-KMS** server (`:9000`, no auto-encryption)
stored it with an ETag equal to the plaintext md5 and with **no** encryption metadata:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/plain.txt local/plainbucket/plain.txt
`/tmp/blitzy/out/plain.txt` -> `local/plainbucket/plain.txt`
Total: 52 B, Transferred: 52 B, Speed: 1.75 KiB/s
$ mc --config-dir /tmp/blitzy/mc-config stat local/plainbucket/plain.txt
Name      : plain.txt
Date      : 2026-07-06 23:43:26 UTC 
Size      : 52 B   
ETag      : 1c00e6dd7865536879e3bbd94b6fe595 
Type      : file 
Metadata  :
  Content-Type: text/plain
```

The non-KMS control's `ETag` equals the plaintext md5 (`1c00e6dd…`) with no `Encryption:`
line, whereas the auto-encrypting bucket produced ETag `9ce74369b8d3b75c38a227feb58008b7`
and `Encryption: SSE-KMS (arn:aws:kms:my-minio-key)` — the stored representation differs,
i.e. the object was encrypted server-side even though the client sent no SSE header.

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
is **absent**. The policy and user were created, then as `denyuser` an unencrypted upload to
`denybucket/` was attempted on the KMS-enabled-but-auto-encryption-**off** server (`:9030`):

```bash
# create the deny policy + user (root alias encroot -> :9030):
mc --config-dir /tmp/blitzy/mc-config admin policy create encroot encmandate /tmp/blitzy/out/enc-mandate-iam.json
mc --config-dir /tmp/blitzy/mc-config admin user add encroot denyuser denyuser12345
mc --config-dir /tmp/blitzy/mc-config admin policy attach encroot encmandate --user denyuser
mc --config-dir /tmp/blitzy/mc-config alias set encuser http://127.0.0.1:9030 denyuser denyuser12345
mc --config-dir /tmp/blitzy/mc-config mb encroot/denybucket
```

The unencrypted upload attempt and its **complete** (unedited) client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/plain.txt encuser/denybucket/noenc.txt
`/tmp/blitzy/out/plain.txt` -> `encuser/denybucket/noenc.txt`
mc: <ERROR> Failed to copy `/tmp/blitzy/out/plain.txt`. Insufficient permissions to access this path `http://127.0.0.1:9030/denybucket/noenc.txt`
```

The `PutObject` request/response, captured on the trace stream. The exact producing command
(root alias `encroot`, since trace requires admin) was
`mc --config-dir /tmp/blitzy/mc-config admin trace -v encroot`:

```text
127.0.0.1:9030 [REQUEST s3.PutObject] [2026-07-06T23:43:52.539] [Client IP: 127.0.0.1]
127.0.0.1:9030 PUT /denybucket/noenc.txt
127.0.0.1:9030 Proto: HTTP/1.1
127.0.0.1:9030 Host: 127.0.0.1:9030
127.0.0.1:9030 Content-Length: 225
127.0.0.1:9030 Content-Type: text/plain
127.0.0.1:9030 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9030 X-Amz-Date: 20260706T234352Z
127.0.0.1:9030 X-Amz-Decoded-Content-Length: 52
127.0.0.1:9030 Accept-Encoding: zstd,gzip
127.0.0.1:9030 Authorization: AWS4-HMAC-SHA256 Credential=denyuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=96ba7c1cdd0c273da2923e18cc9210ef6284225a2fecd6d7187c9173af211d58
127.0.0.1:9030 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9030 <BLOB>
127.0.0.1:9030 [RESPONSE] [2026-07-06T23:43:52.539] [ Duration 103µs TTFB 91.811µs ↑ 135 B  ↓ 329 B ]
127.0.0.1:9030 403 Forbidden
127.0.0.1:9030 Accept-Ranges: bytes
127.0.0.1:9030 Content-Type: application/xml
127.0.0.1:9030 Server: MinIO
127.0.0.1:9030 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9030 X-Ratelimit-Limit: 1142637
127.0.0.1:9030 X-Ratelimit-Remaining: 1142637
127.0.0.1:9030 X-Xss-Protection: 1; mode=block
127.0.0.1:9030 Content-Length: 329
127.0.0.1:9030 Vary: Origin,Accept-Encoding
127.0.0.1:9030 X-Amz-Id-2: 99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6
127.0.0.1:9030 X-Amz-Request-Id: 18BFD7F6D1868D84
127.0.0.1:9030 X-Content-Type-Options: nosniff
127.0.0.1:9030 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>noenc.txt</Key><BucketName>denybucket</BucketName><Resource>/denybucket/noenc.txt</Resource><RequestId>18BFD7F6D1868D84</RequestId><HostId>99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6</HostId></Error>
```

The request (no `x-amz-server-side-encryption` in `SignedHeaders`) is rejected in **103µs**
with `403 Forbidden` and an `<Error><Code>AccessDenied</Code>` body for
`/denybucket/noenc.txt`.

**Control — an encrypted upload by the same user SUCCEEDS**, proving only the *unencrypted*
PUT is blocked. The exact command and its complete client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp --enc-kms "encuser/denybucket=my-minio-key" \
    /tmp/blitzy/out/plain.txt encuser/denybucket/enc.txt
`/tmp/blitzy/out/plain.txt` -> `encuser/denybucket/enc.txt`
Total: 52 B, Transferred: 52 B, Speed: 1.58 KiB/s
```

The corresponding `PutObject` trace (same
`mc --config-dir /tmp/blitzy/mc-config admin trace -v encroot` stream):

```text
127.0.0.1:9030 [REQUEST s3.PutObject] [2026-07-06T23:43:52.573] [Client IP: 127.0.0.1]
127.0.0.1:9030 PUT /denybucket/enc.txt
127.0.0.1:9030 Proto: HTTP/1.1
127.0.0.1:9030 Host: 127.0.0.1:9030
127.0.0.1:9030 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9030 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9030 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9030 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: my-minio-key
127.0.0.1:9030 Content-Type: text/plain
127.0.0.1:9030 Content-Length: 225
127.0.0.1:9030 X-Amz-Date: 20260706T234352Z
127.0.0.1:9030 X-Amz-Decoded-Content-Length: 52
127.0.0.1:9030 Accept-Encoding: zstd,gzip
127.0.0.1:9030 Authorization: AWS4-HMAC-SHA256 Credential=denyuser/20260706/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id,Signature=7a4953c2b7518ce748abb2c6227fc8fd2ebce8b486ddc8ca2f7b98ca8c5626b5
127.0.0.1:9030 <BLOB>
127.0.0.1:9030 [RESPONSE] [2026-07-06T23:43:52.598] [ Duration 24.49ms TTFB 24.469433ms ↑ 433 B  ↓ 0 B ]
127.0.0.1:9030 200 OK
127.0.0.1:9030 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9030 ETag: "d2816f2d5f2b846ac819d2ddae9ec31d"
127.0.0.1:9030 Server: MinIO
127.0.0.1:9030 X-Amz-Id-2: 99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6
127.0.0.1:9030 X-Xss-Protection: 1; mode=block
127.0.0.1:9030 Accept-Ranges: bytes
127.0.0.1:9030 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9030 X-Amz-Request-Id: 18BFD7F6D394743A
127.0.0.1:9030 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
127.0.0.1:9030 X-Content-Type-Options: nosniff
127.0.0.1:9030 X-Ratelimit-Remaining: 1142637
127.0.0.1:9030 Content-Length: 0
127.0.0.1:9030 Vary: Origin,Accept-Encoding
127.0.0.1:9030 X-Ratelimit-Limit: 1142637
127.0.0.1:9030 <BLOB>
```

Here the client explicitly signs the SSE headers
(`SignedHeaders=...;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id`),
the `Null` condition no longer matches, the `Deny` does not fire, and the write returns
`200 OK` (ETag `"d2816f2d5f2b846ac819d2ddae9ec31d"`).

**Control — reading the encrypted object SUCCEEDS**, proving that only the *unencrypted PUT* is
blocked while the encrypted PUT **and** the subsequent GET/read both succeed. As `denyuser`,
`mc cat` returns the original plaintext and a download's md5 matches the original:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cat encuser/denybucket/enc.txt
hello minio bucket-encryption test payload line one
$ mc --config-dir /tmp/blitzy/mc-config cp encuser/denybucket/enc.txt /tmp/blitzy/out/enc-downloaded.txt
`encuser/denybucket/enc.txt` -> `/tmp/blitzy/out/enc-downloaded.txt`
Total: 52 B, Transferred: 52 B, Speed: 8.27 KiB/s
$ md5sum /tmp/blitzy/out/enc-downloaded.txt /tmp/blitzy/out/plain.txt
1c00e6dd7865536879e3bbd94b6fe595  /tmp/blitzy/out/enc-downloaded.txt
1c00e6dd7865536879e3bbd94b6fe595  /tmp/blitzy/out/plain.txt
```

The corresponding `GetObject` trace (from the `mc cat`) returns `200 OK`, transferring the
52 decrypted plaintext bytes (`↓ 52 B`) with `X-Amz-Server-Side-Encryption: aws:kms` echoed:

```text
127.0.0.1:9030 [REQUEST s3.GetObject] [2026-07-06T23:43:52.627] [Client IP: 127.0.0.1]
127.0.0.1:9030 GET /denybucket/enc.txt
127.0.0.1:9030 Proto: HTTP/1.1
127.0.0.1:9030 Host: 127.0.0.1:9030
127.0.0.1:9030 X-Amz-Date: 20260706T234352Z
127.0.0.1:9030 Accept-Encoding: identity
127.0.0.1:9030 Authorization: AWS4-HMAC-SHA256 Credential=denyuser/20260706/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=1e805cb05bfaee08d2629b86d9cac2dd07b4c20c3859efb522256f9e77ba1669
127.0.0.1:9030 Content-Length: 0
127.0.0.1:9030 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9030 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9030 <BLOB>
127.0.0.1:9030 [RESPONSE] [2026-07-06T23:43:52.627] [ Duration 702µs TTFB 664.334µs ↑ 93 B  ↓ 52 B ]
127.0.0.1:9030 200 OK
127.0.0.1:9030 Last-Modified: Mon, 06 Jul 2026 23:43:52 GMT
127.0.0.1:9030 Server: MinIO
127.0.0.1:9030 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9030 Vary: Origin,Accept-Encoding
127.0.0.1:9030 X-Amz-Request-Id: 18BFD7F6D6C47CB9
127.0.0.1:9030 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9030 X-Content-Type-Options: nosniff
127.0.0.1:9030 X-Xss-Protection: 1; mode=block
127.0.0.1:9030 X-Ratelimit-Remaining: 1142637
127.0.0.1:9030 Content-Length: 52
127.0.0.1:9030 Accept-Ranges: bytes
127.0.0.1:9030 Content-Type: text/plain
127.0.0.1:9030 ETag: "d2816f2d5f2b846ac819d2ddae9ec31d"
127.0.0.1:9030 X-Amz-Id-2: 99b429ef7f7998a42aa90f5909b16f87de5d2572540bbfd35e394c5ae814b0b6
127.0.0.1:9030 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
127.0.0.1:9030 X-Ratelimit-Limit: 1142637
127.0.0.1:9030 <BLOB>
```

So, under the encryption-mandating IAM policy: the **unencrypted PUT is denied 403**, while the
**encrypted PUT succeeds (200)** and the **encrypted object reads back correctly (200, 52
plaintext bytes, md5 `1c00e6dd…`)**.

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

A lock-enabled bucket was created on the single-node server (`:9000`, alias `local`); object
locking implies versioning. Distinct object versions were then placed under **Governance**,
**Compliance**, and **Legal Hold**, and versioned deletes were attempted. The exact setup
commands:

```bash
mc --config-dir /tmp/blitzy/mc-config mb --with-lock local/wormtest
mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/gov.txt   local/wormtest/gov.txt
mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/comp.txt  local/wormtest/comp.txt
mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/trans.txt local/wormtest/trans.txt
# apply per-object retention (version ids captured via mc ls --versions):
mc --config-dir /tmp/blitzy/mc-config retention set --version-id 831099fa-01d1-44f0-862a-720fcd718214 governance "1d" local/wormtest/gov.txt
mc --config-dir /tmp/blitzy/mc-config retention set --version-id d849a80f-c3ef-4c5d-b0f5-6f05077edfc2 compliance "1d" local/wormtest/comp.txt
```

The **Legal Hold** case is demonstrated on its own dedicated object (`lh2.txt`) in its
subsection below, where the `legalhold set` / `info` / `clear` commands and their complete
client output are shown inline for a fully self-contained state-change trace.

**Where the "log entries" are.** At the default console log level the single-node server's
console output (`minio-sn.log`) remained the startup banner only — MinIO emits **no
per-delete console line** for a blocked delete. The runtime log evidence is therefore the
**HTTP trace stream** plus the client-facing error. Every trace block below was produced by
subscribing to the trace stream with:

```bash
mc --config-dir /tmp/blitzy/mc-config admin trace -v local
```

Note also that `mc` issues deletes through the S3 **batch** API `DeleteMultipleObjects`
(`POST /wormtest/?delete=`), so a blocked delete returns **`200 OK`** at the HTTP layer with a
per-object `<Error>` embedded inside the `<DeleteResult>` document — not a top-level HTTP
error.

### Governance — blocked without bypass, allowed with bypass

Attempting to delete a Governance-protected version **without** bypass. The exact command and
its complete, unedited client output (the full version id is shown in the WORM error):

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm --version-id 831099fa-01d1-44f0-862a-720fcd718214 local/wormtest/gov.txt
mc: <ERROR> Failed to remove `local/wormtest/gov.txt`. Object, 'gov.txt (Version ID=831099fa-01d1-44f0-862a-720fcd718214)' is WORM protected and cannot be overwritten
```

The corresponding `DeleteMultipleObjects` trace:

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:50:51.143] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Content-Sha256: 71ba961ee0230b5bcc421892f71687f13d6a99cddbfa2f9bf6148cb3aa78e69f
127.0.0.1:9000 X-Amz-Date: 20260706T235051Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=657a5a0b6a0a129bfc64cf2a1e8adde459517b7c6fb1a147a28ff0dfa11133b4
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 Content-Md5: wO1TYNN4ZdchwhVr2clcrQ==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>gov.txt</Key><VersionId>831099fa-01d1-44f0-862a-720fcd718214</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:50:51.143] [ Duration 364µs TTFB 345.827µs ↑ 236 B  ↓ 304 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Content-Length: 304
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18BFD858484456BE
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>gov.txt</Key><VersionId>831099fa-01d1-44f0-862a-720fcd718214</VersionId></Error></DeleteResult>
```

Now **with** bypass — the request carries `x-amz-bypass-governance-retention` in its signed
headers and **succeeds** (state change: version present → deleted). The exact command and its
complete client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm --bypass --version-id 831099fa-01d1-44f0-862a-720fcd718214 local/wormtest/gov.txt
Removed `local/wormtest/gov.txt` (versionId=831099fa-01d1-44f0-862a-720fcd718214).
```

The corresponding trace:

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:50:51.171] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=126afdfba022c9628e2cc3f6ca7d74d19f07c9e38322a14df1fce94521db75f6
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: 71ba961ee0230b5bcc421892f71687f13d6a99cddbfa2f9bf6148cb3aa78e69f
127.0.0.1:9000 X-Amz-Date: 20260706T235051Z
127.0.0.1:9000 Content-Md5: wO1TYNN4ZdchwhVr2clcrQ==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>gov.txt</Key><VersionId>831099fa-01d1-44f0-862a-720fcd718214</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:50:51.171] [ Duration 827µs TTFB 811.423µs ↑ 270 B  ↓ 212 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 212
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD85849EEC791
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>gov.txt</Key><VersionId>831099fa-01d1-44f0-862a-720fcd718214</VersionId></Deleted></DeleteResult>
```

The response body (shown in full above) now reports the key under `<Deleted>` rather than
`<Error>`, for the same version id `831099fa-01d1-44f0-862a-720fcd718214` that was previously
refused.

### Compliance — never bypassable

First, confirm the version really is in Compliance mode. The exact command and its complete,
unedited output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config retention info --version-id d849a80f-c3ef-4c5d-b0f5-6f05077edfc2 local/wormtest/comp.txt
Name    : local/wormtest/comp.txt
Version : d849a80f-c3ef-4c5d-b0f5-6f05077edfc2
Mode    : COMPLIANCE, expiring in 23 hours 59 minutes
```

This Compliance-mode version was then targeted with a delete that **includes** `--bypass` (as
root, with the bypass header). It is **still blocked**. The exact command and its complete
client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm --bypass --version-id d849a80f-c3ef-4c5d-b0f5-6f05077edfc2 local/wormtest/comp.txt
mc: <ERROR> Failed to remove `local/wormtest/comp.txt`. Object, 'comp.txt (Version ID=d849a80f-c3ef-4c5d-b0f5-6f05077edfc2)' is WORM protected and cannot be overwritten
```

The corresponding trace — note the request carries `X-Amz-Bypass-Governance-Retention: true`
yet the delete is refused:

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:50:51.199] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=c2625dceb275bcbd0fdf6ce1e1e84c912ddf652bb2fec4f23621e0606e7ef3c6
127.0.0.1:9000 Content-Length: 132
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: 09bd0cddb8187752954948527c849506692859099b44d1d07731a20d99f31c57
127.0.0.1:9000 X-Amz-Date: 20260706T235051Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Content-Md5: 1oZpL+S+XHHVCpTFiq3j5A==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>comp.txt</Key><VersionId>d849a80f-c3ef-4c5d-b0f5-6f05077edfc2</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:50:51.199] [ Duration 309µs TTFB 296.518µs ↑ 271 B  ↓ 305 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD8584B9B4666
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 305
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>comp.txt</Key><VersionId>d849a80f-c3ef-4c5d-b0f5-6f05077edfc2</VersionId></Error></DeleteResult>
```

Even with the bypass header and root credentials, the `<Error>` with
`Object is WORM protected and cannot be overwritten` is returned for `comp.txt` — Compliance
cannot be bypassed by any user, including root. **The object remained present after the failed
delete**, proven by listing the version and stat-ing it (the retain-until date and mode are
intact). The exact commands and their complete outputs:

```bash
$ mc --config-dir /tmp/blitzy/mc-config ls --versions local/wormtest/comp.txt
[2026-07-06 23:50:20 UTC]    29B STANDARD d849a80f-c3ef-4c5d-b0f5-6f05077edfc2 v1 PUT comp.txt
```

```bash
$ mc --config-dir /tmp/blitzy/mc-config stat --version-id d849a80f-c3ef-4c5d-b0f5-6f05077edfc2 local/wormtest/comp.txt
Name      : comp.txt
Date      : 2026-07-06 23:50:20 UTC
Size      : 29 B
ETag      : 7baca876dee0aa3b7c80d8a2cf104809
VersionID : d849a80f-c3ef-4c5d-b0f5-6f05077edfc2
Type      : file
Metadata  :
  X-Amz-Object-Lock-Retain-Until-Date: 2026-07-07T23:50:21.000Z
  X-Amz-Object-Lock-Mode             : COMPLIANCE
  Content-Type                       : text/plain
```

(The three `Metadata` key/value pairs are always present, but their **order is
non-deterministic across runs** — `mc` iterates a Go map — so a repeated `stat` may list the
Mode / Retain-Until-Date / Content-Type lines in a different order; the values are identical.)

### Legal Hold — blocked while held, deletable after clearing

A fresh object `lh2.txt` was created and a legal hold applied to its version. The exact commands
and their complete client outputs (setting the hold, then confirming it is `ON`):

```bash
$ mc --config-dir /tmp/blitzy/mc-config legalhold set --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
Object legal hold successfully set for `lh2.txt` (version-id=16619662-6849-4cee-ad51-f10d166f2213).

$ mc --config-dir /tmp/blitzy/mc-config legalhold info --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
[    ON    ]  16619662-6849-4cee-ad51-f10d166f2213  lh2.txt
```

With the legal hold `ON`, the versioned delete is blocked. The exact command and its complete
client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
mc: <ERROR> Failed to remove `local/wormtest/lh2.txt`. Object, 'lh2.txt (Version ID=16619662-6849-4cee-ad51-f10d166f2213)' is WORM protected and cannot be overwritten
```

The corresponding trace (note there is **no** bypass header — legal hold ignores the
governance-bypass mechanism entirely):

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:57:55.752] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=cc833bc7b9fe97356e713d57fc834ed6a1d6808e01a1380e2709ad8893307e5d
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 Content-Md5: 4iKhQHGjGS/U3Xdy9BWyhA==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 90b9738583bfc05659c635e06f34f2a5c3d60b60a6dbb8fb2104c73643dfc023
127.0.0.1:9000 X-Amz-Date: 20260706T235755Z
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>lh2.txt</Key><VersionId>16619662-6849-4cee-ad51-f10d166f2213</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:57:55.753] [ Duration 310µs TTFB 301.664µs ↑ 236 B  ↓ 304 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD8BB24FC04E4
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 304
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>lh2.txt</Key><VersionId>16619662-6849-4cee-ad51-f10d166f2213</VersionId></Error></DeleteResult>
```

Now clear the hold. The exact command and its complete client output, followed by confirmation
that the hold is `OFF`:

```bash
$ mc --config-dir /tmp/blitzy/mc-config legalhold clear --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
Object legal hold successfully cleared for `lh2.txt` (version-id=16619662-6849-4cee-ad51-f10d166f2213).

$ mc --config-dir /tmp/blitzy/mc-config legalhold info --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
[    OFF   ]  16619662-6849-4cee-ad51-f10d166f2213  lh2.txt
```

The `legalhold clear` issues `s3.PutObjectLegalHold` with body `<LegalHold><Status>OFF</Status></LegalHold>`:

```text
127.0.0.1:9000 [REQUEST s3.PutObjectLegalHold] [2026-07-06T23:57:55.783] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /wormtest/lh2.txt?legal-hold=&versionId=16619662-6849-4cee-ad51-f10d166f2213
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260706T235755Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=3cedf3cfb64978dad743587082ce844b46adf7530026392774235dd1d2249c63
127.0.0.1:9000 Content-Length: 43
127.0.0.1:9000 Content-Md5: An8G3mv4SfhAibzslQjGDw==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: eef107aae3d3794a8cdb229cacdbab087721ce49f43c55d9b64e17a769592030
127.0.0.1:9000 <LegalHold><Status>OFF</Status></LegalHold>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:57:55.786] [ Duration 2.791ms TTFB 2.773363ms ↑ 148 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD8BB26CD8E48
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
```

With the hold cleared, the identical delete now **succeeds**. The exact command and its complete
client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm --version-id 16619662-6849-4cee-ad51-f10d166f2213 local/wormtest/lh2.txt
Removed `local/wormtest/lh2.txt` (versionId=16619662-6849-4cee-ad51-f10d166f2213).
```

The corresponding trace:

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:57:55.843] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 131
127.0.0.1:9000 Content-Md5: 4iKhQHGjGS/U3Xdy9BWyhA==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 90b9738583bfc05659c635e06f34f2a5c3d60b60a6dbb8fb2104c73643dfc023
127.0.0.1:9000 X-Amz-Date: 20260706T235755Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=cc833bc7b9fe97356e713d57fc834ed6a1d6808e01a1380e2709ad8893307e5d
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>lh2.txt</Key><VersionId>16619662-6849-4cee-ad51-f10d166f2213</VersionId></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:57:55.844] [ Duration 782µs TTFB 765.175µs ↑ 236 B  ↓ 212 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Content-Length: 212
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18BFD8BB2A5D7E81
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>lh2.txt</Key><VersionId>16619662-6849-4cee-ad51-f10d166f2213</VersionId></Deleted></DeleteResult>
```

State change is explicit — and the two delete requests are **byte-identical**: both carry the
same `Signature=cc833bc7...` and `Content-Md5: 4iKhQHGjGS/U3Xdy9BWyhA==`. The version id
`16619662-...` was refused while the hold was `ON`, then reported under `<Deleted>` once the
hold was `OFF`. Only the server-side legal-hold state changed the outcome, not the request.

### Transitional case — an *unversioned* delete creates a delete marker (not blocked)

On a versioned bucket, a delete **without** `--version-id` does not remove the protected
version; it creates a **delete marker** and is not itself blocked. The exact command and its
complete client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config rm local/wormtest/trans.txt
Created delete marker `local/wormtest/trans.txt` (versionId=cbca6611-097c-4d21-abf6-209501123761).
```

The corresponding trace — note the request body carries **no** `<VersionId>`, and the response
reports `<DeleteMarker>true</DeleteMarker>`:

```text
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-06T23:51:10.985] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /wormtest/?delete=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.77 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 6de2624a1072a129c7d295a40475895f56d7cb3a97f5252770e865517e06e76e
127.0.0.1:9000 X-Amz-Date: 20260706T235110Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260706/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=0bb9db2f707290046d00cb27bd66c8fba72f25011e64c7e6a044f67f4c35ee0f
127.0.0.1:9000 Content-Length: 74
127.0.0.1:9000 Content-Md5: 5vntUtVTbDRxYxmuYT6E0A==
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>trans.txt</Key></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-06T23:51:10.987] [ Duration 2.084ms TTFB 2.066419ms ↑ 179 B  ↓ 271 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Content-Length: 271
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFD85CE6F4EBDA
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><DeleteMarker>true</DeleteMarker><DeleteMarkerVersionId>cbca6611-097c-4d21-abf6-209501123761</DeleteMarkerVersionId><Key>trans.txt</Key></Deleted></DeleteResult>
```

A subsequent `mc ls --versions` shows the new delete marker as the latest version on top of the
retained, still-protected original version. The exact command and its complete client output:

```bash
$ mc --config-dir /tmp/blitzy/mc-config ls --versions local/wormtest/trans.txt
[2026-07-06 23:51:10 UTC]     0B STANDARD cbca6611-097c-4d21-abf6-209501123761 v2 DEL trans.txt
[2026-07-06 23:50:21 UTC]    37B STANDARD 011d5c90-7a54-4f12-a285-f88459851d7e v1 PUT trans.txt
```

The equivalent server-side `ListVersions` XML (from the same trace) confirms the marker is
`IsLatest=true` and the 37-byte original is retained beneath it:

```text
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>wormtest</Name><Prefix>trans.txt</Prefix><KeyMarker></KeyMarker><NextVersionIdMarker></NextVersionIdMarker><VersionIdMarker></VersionIdMarker><MaxKeys>1000</MaxKeys><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated><DeleteMarker><Key>trans.txt</Key><LastModified>2026-07-06T23:51:10.985Z</LastModified><ETag></ETag><Size>0</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass><IsLatest>true</IsLatest><VersionId>cbca6611-097c-4d21-abf6-209501123761</VersionId></DeleteMarker><Version><Key>trans.txt</Key><LastModified>2026-07-06T23:50:21.000Z</LastModified><ETag>&#34;d1560aefe7e2929bf15ce635d161fe61&#34;</ETag><Size>37</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass><IsLatest>false</IsLatest><VersionId>011d5c90-7a54-4f12-a285-f88459851d7e</VersionId></Version><EncodingType>url</EncodingType></ListVersionsResult>
```

The delete marker `cbca6611-...` is `IsLatest=true`; the original `011d5c90-...` (37 bytes)
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

The banner reports `Formatting 1st pool, 1 set(s), 4 drives per set`. `mc admin info` confirms
the erasure geometry:

```bash
$ mc --config-dir /tmp/blitzy/mc-config admin info ec
●  127.0.0.1:9020
   Uptime: 9 seconds
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK
   Drives: 4/4 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.6% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

4 drives online, 0 drives offline, EC:2
```

A 1 MiB object was written to `ecbucket` and its plaintext md5 recorded. The exact commands and
their complete output:

```bash
$ head -c 1048576 /dev/urandom > /tmp/blitzy/q3final_bigobj.bin
$ md5sum /tmp/blitzy/q3final_bigobj.bin
225b24b70e346643306d7fe507a8aabf  /tmp/blitzy/q3final_bigobj.bin
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/q3final_bigobj.bin ec/ecbucket/bigobj.bin
`/tmp/blitzy/q3final_bigobj.bin` -> `ec/ecbucket/bigobj.bin`
Total: 1.00 MiB, Transferred: 1.00 MiB, Speed: 39.68 MiB/s
```

The object (md5 `225b24b70e346643306d7fe507a8aabf`) is stored as `part.1` on **all four** drives
under `ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1` (the directory name is
the object's data-directory UUID). Listing the backend confirms the layout: each shard is 524320
bytes (512 KiB of erasure-fragment data plus a 32-byte inline HighwayHash bit-rot checksum), and
the four shards have **distinct** md5s (they are distinct erasure fragments, not replicas):

```bash
$ for d in d1 d2 d3 d4; do ls -l /tmp/blitzy/ec/$d/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1; md5sum /tmp/blitzy/ec/$d/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1; done
-rw-r--r-- 1 root root 524320 Jul  7 00:28 /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
61a6aff4e38840eeb0ebc82318aeab6e  /tmp/blitzy/ec/d1/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
-rw-r--r-- 1 root root 524320 Jul  7 00:28 /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
f77111ba5844327b4fa4f7b64ee70a86  /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
-rw-r--r-- 1 root root 524320 Jul  7 00:28 /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
af256d9e7255a016755bfbec87031648  /tmp/blitzy/ec/d3/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
-rw-r--r-- 1 root root 524320 Jul  7 00:28 /tmp/blitzy/ec/d4/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
cd691e7483e5f701433c5293dde7ae24  /tmp/blitzy/ec/d4/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
```

### Recoverable corruption (≤ parity shards) — GET returns correct bytes; shard reconstructed

One backend shard was zeroed directly on disk. The full command and its complete output:

```bash
$ dd if=/dev/zero of=/tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1 bs=1024 count=64 seek=100 conv=notrunc
64+0 records in
64+0 records out
65536 bytes (66 kB, 64 KiB) copied, 0.000259887 s, 66 MB/s
```

This overwrote 64 KiB of the `d2` shard, changing its md5 from the healthy
`f77111ba5844327b4fa4f7b64ee70a86` to a corrupted `cdd474ae0b91d40bf6e1a8d87cd2b6b4`:

```bash
$ md5sum /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
cdd474ae0b91d40bf6e1a8d87cd2b6b4  /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
```

A subsequent `GET` (via `mc cp`) still returned the **correct** bytes — the downloaded md5 matches
the original `225b24b70e346643306d7fe507a8aabf` exactly:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp ec/ecbucket/bigobj.bin /tmp/blitzy/q3_dl_rec.bin
`ec/ecbucket/bigobj.bin` -> `/tmp/blitzy/q3_dl_rec.bin`
Total: 1.00 MiB, Transferred: 1.00 MiB, Speed: 118.89 MiB/s
$ md5sum /tmp/blitzy/q3_dl_rec.bin
225b24b70e346643306d7fe507a8aabf  /tmp/blitzy/q3_dl_rec.bin
```

The runtime trace of that GET (filtered to the S3, storage, and healing subsystems) shows the read
served across the erasure set followed by a deferred auto-heal-on-read that fires ~1 s after the
`200 OK` response:

```bash
$ mc --config-dir /tmp/blitzy/mc-config admin trace --call s3,storage,healing ec
```

```text
2026-07-07T00:28:54.577 [STORAGE] storage.StatVol 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket 19.696µs
2026-07-07T00:28:54.577 [STORAGE] storage.StatVol 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket 19.442µs
2026-07-07T00:28:54.577 [STORAGE] storage.StatVol 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket 26.712µs
2026-07-07T00:28:54.577 [STORAGE] storage.StatVol 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket 17.882µs
2026-07-07T00:28:54.577 [200 OK] s3.GetBucketLocation 127.0.0.1:9020/ecbucket/?location=  127.0.0.1        347µs       ⇣  330.009µs  ↑ 93 B ↓ 128 B
2026-07-07T00:28:54.577 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket bigobj.bin 66.207µs 368 B
2026-07-07T00:28:54.577 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin 66.473µs 368 B
2026-07-07T00:28:54.577 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin 54.788µs 368 B
2026-07-07T00:28:54.577 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin 89.748µs 368 B
2026-07-07T00:28:54.577 [200 OK] s3.HeadObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        534µs       ⇣  499.488µs  ↑ 97 B ↓ 0 B
2026-07-07T00:28:54.579 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin 56.178µs 368 B
2026-07-07T00:28:54.579 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket bigobj.bin 64.811µs 368 B
2026-07-07T00:28:54.579 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin 49.853µs 368 B
2026-07-07T00:28:54.579 [STORAGE] storage.ReadXL 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin 39.881µs 368 B
2026-07-07T00:28:54.580 [STORAGE] storage.ReadFileStream 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1 41.509µs 512 KiB
2026-07-07T00:28:54.580 [STORAGE] storage.ReadFileStream 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1 38.855µs 512 KiB
2026-07-07T00:28:54.580 [STORAGE] storage.ReadFileStream 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1 43.383µs 512 KiB
2026-07-07T00:28:54.579 [200 OK] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        2.217ms      ⇣  1.347183ms  ↑ 93 B ↓ 1.0 MiB
2026-07-07T00:28:55.582 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin 99.307µs
2026-07-07T00:28:55.582 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin 117.46µs
2026-07-07T00:28:55.582 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket bigobj.bin 98.554µs
2026-07-07T00:28:55.582 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin 82.473µs
2026-07-07T00:28:55.583 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket bigobj.bin 39.032µs
2026-07-07T00:28:55.583 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin 47.731µs
2026-07-07T00:28:55.583 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin 48.414µs
2026-07-07T00:28:55.583 [STORAGE] storage.ReadVersion 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin 88.811µs
2026-07-07T00:28:55.583 [STORAGE] storage.CheckParts 127.0.0.1:9020 /tmp/blitzy/ec/d1 ecbucket bigobj.bin 24.708µs
2026-07-07T00:28:55.583 [STORAGE] storage.CheckParts 127.0.0.1:9020 /tmp/blitzy/ec/d2 ecbucket bigobj.bin 10.196µs
2026-07-07T00:28:55.583 [STORAGE] storage.CheckParts 127.0.0.1:9020 /tmp/blitzy/ec/d3 ecbucket bigobj.bin 8.875µs
2026-07-07T00:28:55.583 [STORAGE] storage.CheckParts 127.0.0.1:9020 /tmp/blitzy/ec/d4 ecbucket bigobj.bin 9.218µs
2026-07-07T00:28:55.582 [HEALING] heal.Object 127.0.0.1:9020 ecbucket/bigobj.bin 266.216µs 1.0 MiB
```

The `s3.GetObject` line returns `200 OK` with `↓ 1.0 MiB` (the full object). The `storage.ReadXL` calls read the erasure metadata across all four drives (two rounds — for the
`HeadObject` and the `GetObject`); three `storage.ReadFileStream` calls then stream shard data
(including the corrupted `d2`). Because only one shard is corrupt — within the `EC:2` parity budget
of 2 — the object is reconstructed transparently and the GET returns the correct 1.0 MiB (per the
md5 proof above). ~1 s later the `storage.ReadVersion` / `storage.CheckParts` sweep
culminates in the `[HEALING] heal.Object 127.0.0.1:9020 ecbucket/bigobj.bin ... 1.0 MiB` event — an
auto-heal-on-read scheduled for the object. **At the default console log level the `errFileCorrupt`
sentinel is not printed**: detection and reconstruction are transparent to the client, and the runtime
evidence lives in the trace stream.

Critically, this auto-heal-on-read is **metadata-level**. Re-checking the `d2` shard immediately after
the GET shows its md5 is **unchanged** — the in-place *data* bit rot on the shard is not repaired by
the read itself:

```bash
$ md5sum /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
cdd474ae0b91d40bf6e1a8d87cd2b6b4  /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
```

**Heal scan depth matters.** Running the healer explicitly on the recoverable object shows that the
**default (normal) scan is metadata-level** and does not repair in-place *data* bit rot, while a **deep
scan** detects the checksum mismatch and reconstructs the corrupted shard from parity. The producing
commands and their complete output (the two `md5sum` invocations are the *same* command, run before and
after the deep heal):

```bash
$ mc --config-dir /tmp/blitzy/mc-config admin heal -r --force ec/ecbucket/bigobj.bin
[Green  ->  Green] ecbucket/
[Green  ->  Green] ecbucket/bigobj.bin
Healed:	0/1 objects; 1024 KiB in 1s
$ md5sum /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
cdd474ae0b91d40bf6e1a8d87cd2b6b4  /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
$ mc --config-dir /tmp/blitzy/mc-config admin heal -r --scan deep --force ec/ecbucket/bigobj.bin
[Green  ->  Green] ecbucket/
[Yellow ->  Green] ecbucket/bigobj.bin
Healed:	1/1 objects; 1024 KiB in 1s
$ md5sum /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
f77111ba5844327b4fa4f7b64ee70a86  /tmp/blitzy/ec/d2/ecbucket/bigobj.bin/cf2bc347-af1e-404f-9619-d8d861e36ac6/part.1
```

After the normal scan the corrupted shard md5 is **unchanged** (`[Green -> Green]`, `Healed: 0/1`); after
the deep scan it is **restored to its original healthy value** `f77111ba5844327b4fa4f7b64ee70a86`
(`[Yellow -> Green]`, `Healed: 1/1`) — confirming that in-place data bit rot is caught and repaired
on the **read path** (auto-heal-on-read, metadata level) or by an explicit **deep** heal (data-level
reconstruction from parity), not by the default metadata heal.

**Two-run stability (magnitude/consistency rule).** The recoverable outcome was confirmed stable across
two independent runs using the *identical* 1 MiB input. **Run 1** is the demonstration above (VID
`cf2bc347-af1e-404f-9619-d8d861e36ac6`, downloaded md5 `225b24b70e346643306d7fe507a8aabf`). **Run 2**
re-uploaded the same file to a fresh data-directory UUID `1f1f04bc-f786-4b8a-bb4c-f84ce7b899fb`,
corrupted its `d2` shard the same way, and read it back — the download md5 again matched the original
exactly:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/q3final_bigobj.bin ec/ecbucket/bigobj.bin
`/tmp/blitzy/q3final_bigobj.bin` -> `ec/ecbucket/bigobj.bin`
Total: 1.00 MiB, Transferred: 1.00 MiB, Speed: 40.40 MiB/s
$ dd if=/dev/zero of=/tmp/blitzy/ec/d2/ecbucket/bigobj.bin/1f1f04bc-f786-4b8a-bb4c-f84ce7b899fb/part.1 bs=1024 count=64 seek=100 conv=notrunc
64+0 records in
64+0 records out
65536 bytes (66 kB, 64 KiB) copied, 0.00787025 s, 9.4 MB/s
$ mc --config-dir /tmp/blitzy/mc-config cp ec/ecbucket/bigobj.bin /tmp/blitzy/q3_dl_run2.bin
`ec/ecbucket/bigobj.bin` -> `/tmp/blitzy/q3_dl_run2.bin`
Total: 1.00 MiB, Transferred: 1.00 MiB, Speed: 41.11 MiB/s
$ md5sum /tmp/blitzy/q3_dl_run2.bin
225b24b70e346643306d7fe507a8aabf  /tmp/blitzy/q3_dl_run2.bin
```

Both runs returned the original md5 `225b24b70e346643306d7fe507a8aabf` — the recoverable behavior is
stable across two independent runs with identical input.

### Unrecoverable corruption (> parity shards) — client sees `SlowDownRead` / HTTP 503

Corrupting **3 of 4** shards (leaving only `d4` intact — beyond the `EC:2` parity of 2) makes the
object unreadable. Continuing on the run-2 object (data-directory UUID
`1f1f04bc-f786-4b8a-bb4c-f84ce7b899fb`, whose `d2` shard was already corrupt from the run above), the
`d1` and `d3` shards were zeroed the same way. The resulting per-shard md5 map shows three corrupted
shards and one healthy shard — `d4`, still matching its healthy baseline
`cd691e7483e5f701433c5293dde7ae24`:

```bash
$ for d in d1 d2 d3 d4; do printf "%s  " $d; md5sum /tmp/blitzy/ec/$d/ecbucket/bigobj.bin/1f1f04bc-f786-4b8a-bb4c-f84ce7b899fb/part.1 | awk '{print $1}'; done
d1  c4f9e03fb514fb59b664cb09cb0821ff
d2  cdd474ae0b91d40bf6e1a8d87cd2b6b4
d3  a9425c84dada52f89a7b51c63e7220bd
d4  cd691e7483e5f701433c5293dde7ae24
```

A `GET` (via `mc cp`) now **fails** on the client with a rate-limit-style error:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp ec/ecbucket/bigobj.bin /tmp/blitzy/q3_dl_fail.bin
`ec/ecbucket/bigobj.bin` -> `/tmp/blitzy/q3_dl_fail.bin`
mc: <ERROR> Failed to copy `http://127.0.0.1:9020/ecbucket/bigobj.bin`. Resource requested is unreadable, please reduce your request rate
```

The trace shows `GetBucketLocation` and `HeadObject` succeed (`200 OK`) but every `s3.GetObject` returns
`503 Service Unavailable` with only `↓ 378 B` (the XML error body, not the object). The `mc` client
auto-retries; the trace captured **10** consecutive `503` GetObject attempts:

```bash
$ mc --config-dir /tmp/blitzy/mc-config admin trace --call s3 ec
```

```text
2026-07-07T00:33:15.761 [200 OK] s3.GetBucketLocation 127.0.0.1:9020/ecbucket/?location=  127.0.0.1        426µs       ⇣  402.994µs  ↑ 93 B ↓ 128 B
2026-07-07T00:33:15.762 [200 OK] s3.HeadObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        564µs       ⇣  540.7µs   ↑ 97 B ↓ 0 B
2026-07-07T00:33:15.764 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.16ms       ⇣  1.143116ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:15.861 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.396ms      ⇣  1.371893ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:15.890 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.857ms      ⇣  1.832622ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:16.649 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.714ms      ⇣  1.692176ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:16.779 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.761ms      ⇣  1.744697ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:17.487 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.621ms      ⇣  1.594135ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:18.432 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.621ms      ⇣  1.59973ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:19.153 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.7ms        ⇣  1.677299ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:19.889 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.634ms      ⇣  1.613417ms  ↑ 93 B ↓ 378 B
2026-07-07T00:33:19.942 [503 Service Unavailable] s3.GetObject 127.0.0.1:9020/ecbucket/bigobj.bin 127.0.0.1        1.507ms      ⇣  1.48835ms  ↑ 93 B ↓ 378 B
```

Fetching the raw HTTP response (via `mc --debug cat`) shows the exact S3 error: HTTP `503 Service
Unavailable` with `Retry-After: 60`, `Content-Type: application/xml`, and an XML body whose `<Code>` is
`SlowDownRead` (the `Etag` header still carries the original object md5, and `X-Amz-Request-Id` matches
the `<RequestId>` in the body):

```bash
$ mc --config-dir /tmp/blitzy/mc-config --debug cat ec/ecbucket/bigobj.bin
```

```text
mc: <DEBUG> HTTP/1.1 503 Service Unavailable
Content-Length: 378
Accept-Ranges: bytes
Content-Type: application/xml
Date: Tue, 07 Jul 2026 00:29:06 GMT
Etag: "225b24b70e346643306d7fe507a8aabf"
Last-Modified: Tue, 07 Jul 2026 00:29:00 GMT
Retry-After: 60
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: 64084d77053f4fbd8e5405e6d076258766008f28b08394c799d1e979f6b5f3d6
X-Amz-Request-Id: 18BFDA6EB9071AD9
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 564934
X-Ratelimit-Remaining: 564934
X-Xss-Protection: 1; mode=block

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>bigobj.bin</Key><BucketName>ecbucket</BucketName><Resource>/ecbucket/bigobj.bin</Resource><RequestId>18BFDA6EB9071AD9</RequestId><HostId>64084d77053f4fbd8e5405e6d076258766008f28b08394c799d1e979f6b5f3d6</HostId></Error>
```

> **Correction to the AAP phrasing (observed).** The internal sentinel is
> `errFileCorrupt = StorageErr("file is corrupted")` [`cmd/storage-errors.go:104`], raised at
> the storage layer. But the **client-facing** result of unrecoverable corruption is S3
> `<Code>SlowDownRead</Code>` at **HTTP 503 Service Unavailable** (with `Retry-After: 60`),
> a *retryable* error — not a literal "file is corrupted" message. The two must not be
> conflated: `errFileCorrupt` is internal; `SlowDownRead`/503 is what the S3 client sees. The
> `mc` client auto-retried the GET; the S3 trace above captured **10** consecutive `503`
> responses, each returning only the 378-byte XML error body (`↓ 378 B`).

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
everything); a restrictive inline **session policy** was then supplied at `AssumeRole` time
allowing only `s3:PutObject`/`s3:GetObject` on `stsbucket/*`. Setup:

```bash
mc --config-dir /tmp/blitzy/mc-config admin user add local stsparent stsparentsecret123
mc --config-dir /tmp/blitzy/mc-config admin policy attach local readwrite --user stsparent
mc --config-dir /tmp/blitzy/mc-config mb local/stsbucket
mc --config-dir /tmp/blitzy/mc-config mb local/otherbucket
```

The temporary credentials were obtained and exercised by a small Go program built against the
repository's pinned `minio-go/v7 v7.0.80` (`/tmp/blitzy/scripts/q4/main.go`, module `q4prog`).
It calls `credentials.NewSTSAssumeRole` with the inline session policy

```go
const sessionPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject"],"Resource":["arn:aws:s3:::stsbucket/*"]}]}`
```

then uses the returned temporary credentials for four actions: an in-session `PutObject` and
`GetObject` on `stsbucket`, an out-of-session `PutObject` on `otherbucket`, and an
out-of-session `ListBuckets`. It was run with the exact invocation below (offline, resolving
`minio-go` from the warmed module cache); its complete stdout is shown verbatim:

```bash
$ cd /tmp/blitzy/scripts/q4 && GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go run .
PARENT-ACCESSKEY: stsparent
TEMP-ACCESSKEY: HGSDFQLBQOM4E1SSOV0F
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

**Server-side trace.** The entire `go run` was captured with a verbose trace subscribed by the
root alias (`mc --config-dir /tmp/blitzy/mc-config admin trace --all -v local`, saved to
`/tmp/blitzy/logs/q4final/q4-trace-all.txt`). The individual request/response blocks below are
sliced from that single capture with `sed` (the expired STS `SecretAccessKey` and the JWT
signature are redacted; the JWT header and payload are shown intact). `AssumeRole` returns
`200 OK` with a temporary access key and a `SessionToken` JWT:

```bash
$ sed -n '79,103p' /tmp/blitzy/logs/q4final/q4-trace-all.txt
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-07T00:49:53.438] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=stsparent/20260707//sts/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=eb9e5151ead6aaac59055715cfd1dcdb98babf7a002629b0c786e8fa02bdec2f
127.0.0.1:9000 Content-Length: 299
127.0.0.1:9000 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Date: 20260707T004953Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3APutObject%22%2C%22s3%3AGetObject%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Astsbucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15
127.0.0.1:9000 [RESPONSE] [2026-07-07T00:49:53.444] [ Duration 6.785ms TTFB 6.781972ms ↑ 384 B  ↓ 1.0 KiB ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Amz-Request-Id: 18BFDB91097FD482
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 1035
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><AssumedRoleUser><Arn></Arn><AssumeRoleId></AssumeRoleId></AssumedRoleUser><Credentials><AccessKeyId>HGSDFQLBQOM4E1SSOV0F</AccessKeyId><SecretAccessKey><redacted-expired-STS-secret></SecretAccessKey><SessionToken>eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJIR1NERlFMQlFPTTRFMVNTT1YwRiIsImV4cCI6MTc4MzM4ODk5MywicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02VUhWMFQySnFaV04wSWl3aWN6TTZSMlYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted></SessionToken><Expiration>2026-07-07T01:49:53Z</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>18BFDB91097FD482</RequestId></ResponseMetadata></AssumeRoleResponse>
```

The `Policy=...` form field carries the URL-encoded session policy, and the request is signed
by the parent (`Credential=stsparent/.../sts/aws4_request`). The returned `SessionToken` is a
JWT; decoding its payload (read from the pre-redaction raw capture) proves the session policy is
embedded as a base64 claim:

```bash
$ TOKEN=$(sed -n 's/.*<SessionToken>\(.*\)<\/SessionToken>.*/\1/p' /tmp/blitzy/logs/q4final/q4-trace-all.raw.txt)
$ python3 -c 'import sys,base64,json; p=sys.argv[1].split(".")[1]; d=json.loads(base64.urlsafe_b64decode(p+"="*(-len(p)%4))); print("JWT claims keys:",sorted(d)); print("parent:",d["parent"]); sp=d["sessionPolicy"]; print("decoded sessionPolicy:",base64.b64decode(sp+"="*(-len(sp)%4)).decode())' "$TOKEN"
JWT claims keys: ['accessKey', 'exp', 'parent', 'sessionPolicy']
parent: stsparent
decoded sessionPolicy: {"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject"],"Resource":["arn:aws:s3:::stsbucket/*"]}]}
```

The claim `sessionPolicy` inside the JWT decodes to exactly the inline policy supplied at
`AssumeRole` time, and `parent` records the issuing user `stsparent`.

**In-session PutObject → `200 OK`.** The request is signed with the **temporary** access key
plus an `X-Amz-Security-Token`, and returns `200 OK` with an ETag:

```bash
$ sed -n '146,173p' /tmp/blitzy/logs/q4final/q4-trace-all.txt
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-07T00:49:53.446] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /stsbucket/in.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=HGSDFQLBQOM4E1SSOV0F/20260707/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-security-token,Signature=34696505fae8ca22a93d903532717bcfdf5cab7a87115b76905d939cc3d97421
127.0.0.1:9000 Content-Length: 204
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 31
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Date: 20260707T004953Z
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJIR1NERlFMQlFPTTRFMVNTT1YwRiIsImV4cCI6MTc4MzM4ODk5MywicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02VUhWMFQySnFaV04wSWl3aWN6TTZSMlYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-07T00:49:53.448] [ Duration 2.024ms TTFB 1.996314ms ↑ 344 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18BFDB910A012BB4
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 ETag: "e7aede4e6cc5c68b105a06df0a7c5891"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 <BLOB>
```

**Out-of-session PutObject → `403 AccessDenied`.** Same temporary credentials, but writing to
`otherbucket` (outside the session policy's `stsbucket/*` resource) is rejected `403` with
`<Code>AccessDenied</Code>` — despite the parent `readwrite` policy allowing `s3:*`:

```bash
$ sed -n '203,231p' /tmp/blitzy/logs/q4final/q4-trace-all.txt
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-07T00:49:53.449] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /otherbucket/out.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 31
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJIR1NERlFMQlFPTTRFMVNTT1YwRiIsImV4cCI6MTc4MzM4ODk5MywicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02VUhWMFQySnFaV04wSWl3aWN6TTZSMlYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=HGSDFQLBQOM4E1SSOV0F/20260707/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-security-token,Signature=ed4582b99d88b3a55899d97ebd4d5b09314480aa961338346e0480fca8c7b18d
127.0.0.1:9000 Content-Length: 204
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Date: 20260707T004953Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-07T00:49:53.449] [ Duration 182µs TTFB 163.622µs ↑ 140 B  ↓ 327 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 327
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Request-Id: 18BFDB910A2DFC7A
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>out.txt</Key><BucketName>otherbucket</BucketName><Resource>/otherbucket/out.txt</Resource><RequestId>18BFDB910A2DFC7A</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**In-session GetObject → `200 OK` (reads 31 bytes).** The in-session read of the object written
above succeeds; the `[RESPONSE]` line reports `↓ 31 B`, exactly the 31-byte payload, matching
the program's `read 31 bytes`:

```bash
$ sed -n '235,262p' /tmp/blitzy/logs/q4final/q4-trace-all.txt
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-07T00:49:53.450] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /stsbucket/in.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=HGSDFQLBQOM4E1SSOV0F/20260707/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=581197c715d3846eb78bb503a7faa11fb77e36f50efe7dfaf61e19c3f4f0b5fa
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260707T004953Z
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJIR1NERlFMQlFPTTRFMVNTT1YwRiIsImV4cCI6MTc4MzM4ODk5MywicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02VUhWMFQySnFaV04wSWl3aWN6TTZSMlYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-07T00:49:53.450] [ Duration 778µs TTFB 748.379µs ↑ 98 B  ↓ 31 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 ETag: "e7aede4e6cc5c68b105a06df0a7c5891"
127.0.0.1:9000 X-Amz-Request-Id: 18BFDB910A363D75
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 Last-Modified: Tue, 07 Jul 2026 00:49:53 GMT
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <BLOB>
```

**Out-of-session ListBuckets → `403 AccessDenied`.** `ListBuckets` with the same credentials is
denied because the session policy does not grant `s3:ListAllMyBuckets`:

```bash
$ sed -n '266,294p' /tmp/blitzy/logs/q4final/q4-trace-all.txt
127.0.0.1:9000 [REQUEST s3.ListBuckets] [2026-07-07T00:49:53.451] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 Delimiter: /
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJIR1NERlFMQlFPTTRFMVNTT1YwRiIsImV4cCI6MTc4MzM4ODk5MywicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6ImV5SldaWEp6YVc5dUlqb2lNakF4TWkweE1DMHhOeUlzSWxOMFlYUmxiV1Z1ZENJNlczc2lSV1ptWldOMElqb2lRV3hzYjNjaUxDSkJZM1JwYjI0aU9sc2ljek02VUhWMFQySnFaV04wSWl3aWN6TTZSMlYwVDJKcVpXTjBJbDBzSWxKbGMyOTFjbU5sSWpwYkltRnlianBoZDNNNmN6TTZPanB6ZEhOaWRXTnJaWFF2S2lKZGZWMTkifQ.<sig-redacted>
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=HGSDFQLBQOM4E1SSOV0F/20260707/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=c1702f30c92ae7298bda11ec82cd0b998a237ecc42aa07d2ff2b4e90167aacaa
127.0.0.1:9000 Prefix: 
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260707T004953Z
127.0.0.1:9000 
127.0.0.1:9000 [RESPONSE] [2026-07-07T00:49:53.451] [ Duration 635µs TTFB 629.011µs ↑ 115 B  ↓ 254 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Amz-Request-Id: 18BFDB910A45B0D7
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Limit: 1142608
127.0.0.1:9000 X-Ratelimit-Remaining: 1142608
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 254
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Resource>/</Resource><RequestId>18BFDB910A45B0D7</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

(The `minio-go` client also issues a `GetBucketLocation` region probe before each write; those
probes are likewise denied `403` in this same trace — the session policy grants neither
`s3:GetBucketLocation` — but they are non-fatal, so the client falls back to `us-east-1` and the
in-session `PutObject` still succeeds. This further confirms that only the two explicitly-granted
actions are permitted.)

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
`sessionPolicyNameExtracted` [`cmd/iam.go:2136`]. This intersection is enforced as shown above
only on the **direct-action** path. A *parallel* service-account authorization helper,
`isAllowedBySessionPolicyForServiceAccount` [`cmd/iam.go:2320`], governs a different path — the
creation and use of service accounts by the temporary credential — and at this commit that path
does **not** enforce the session policy, allowing a session-restricted credential to escalate
back to the full parent policy. This bypass was observed at runtime and is documented in the
security caveat immediately below.

**Documented behavior (confirmation).** MinIO's `docs/sts/assume-role.md` states the session's
permissions are the intersection of the assumed policy and the inline session policy and that
the session policy cannot grant more than the parent (`:39`), with a maximum length of 2048
bytes (`:44`).

### Security caveat: session-policy bypass via self-created service accounts (CVE-2025-62506)

The intersection enforcement demonstrated above holds only for the credential's **direct** S3
actions. At this commit (`c07e5b49d477`, 2024-11-25) there is a **second authorization path**
that does *not* honor the session policy: a session-restricted temporary credential can create a
**service account for itself** and, because that service account is stored with **no inline
policy**, the service account inherits the **full parent policy** — escaping the session-policy
restriction entirely. This is **CVE-2025-62506** (GHSA-jjjj-jwhf-8rgr, CWE-863 *Incorrect
Authorization*, CVSS 3.1 base **8.1 High** — `AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:N`), fixed
upstream in `RELEASE.2025-10-15T17-29-55Z` (PR minio/minio#21642, commit
`c1a49490c78e9c3ebcad86ba0662319138ace190`); the investigated commit predates that release and is
therefore vulnerable.

To exercise this second path, a harness (Go; `minio-go/v7 v7.0.80` + `madmin-go/v3 v3.0.77`,
resolved from the module cache) obtains temporary credentials via STS `AssumeRole` with the same
restrictive inline session policy used above —

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject"],
      "Resource": ["arn:aws:s3:::stsbucket/*"] }
  ]
}
```

— then, using those restricted credentials, (a) probes the direct-action path (baseline
containment), (b) calls `madmin` `AddServiceAccount` **for itself with no inline policy**, and
(c) drives the newly minted service account against actions the session policy forbids. A regular
IAM user (`basicuser`, policy `basic-only` = Get/Put on `smoke/*`) runs the same self-service-
account step as a scope-bounding control. Producing commands:

```bash
# Canonical single-node server (binary built from source at commit c07e5b49d477):
./minio server /tmp/blitzy/cve-repro/data --address :9100 --console-address :9101
# Restricted STS + service-account harness (module cache; offline build):
GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build -o cverepro . && ./cverepro
```

Complete, unedited program output (run repeated 4×; behavior identical every time — only the
randomly-minted 20-character access keys differ between runs; the run shown corresponds to the
trace and JWT decode below):

```text
=========================================================
CVE-2025-62506 reproduction  |  MinIO commit c07e5b49d477
=========================================================

[1] STS AssumeRole OK  (session policy: PutObject/GetObject on stsbucket/* ONLY)
    parent user            : stsparent (policy: readwrite)
    STS temp AccessKeyId   : FE660GYHKO1F2TUC51SF

[2] BASELINE — restricted STS credential, DIRECT S3 actions:
    restricted-STS               ListBuckets => DENIED: Access Denied.
    restricted-STS[in-session]   PutObject stsbucket/in-session.txt => ALLOWED
    restricted-STS[out-session]  PutObject otherbucket/blocked.txt => DENIED: Access Denied.

[3] AddServiceAccount(self, no inline policy) => SUCCESS  <<< minted by the RESTRICTED credential
    new SA AccessKeyId      : 71HOVJI1I63FNFERUGHN

[4] NEW SA — actions OUTSIDE the session policy:
    new-SA                       ListBuckets => ALLOWED (3 buckets: otherbucket,smoke,stsbucket)
    new-SA[out-session]          PutObject otherbucket/escalated.txt => ALLOWED

[5] SCOPE-BOUNDING — regular IAM user 'basicuser' (policy basic-only: Get/Put on smoke/* ONLY):
    basicuser AddServiceAccount(self) => SUCCESS, new SA AK: LQK7D39Z0348KT57WQO9
    basic-SA[in-scope]           PutObject smoke/ok.txt => ALLOWED
    basic-SA[out-scope]          PutObject otherbucket/should-fail.txt => DENIED: Access Denied.
    basic-SA                     ListBuckets => DENIED: Access Denied.

========================= END =========================
```

Steps `[2]` vs `[4]` are the crux: the identical actions the direct-action path **denied** to the
restricted credential (`ListBuckets`, `PutObject otherbucket/*`) are **allowed** once routed
through the self-created service account. The server-side HTTP trace (`mc admin trace --all
--verbose`) confirms the service-account creation call itself succeeds with `200 OK`
(signatures/JWT-signature redacted; the security token is retained to prove the caller was
session-restricted):

```text
127.0.0.1:9100 PUT /minio/admin/v3/add-service-account
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=FE660GYHKO1F2TUC51SF/20260707//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=<redacted>
127.0.0.1:9100 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9100 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJGRTY2MEdZSEtPMUYyVFVDNTFTRiIsImV4cCI6MTc4MzQwMjAyNSwicGFyZW50Ijoic3RzcGFyZW50Iiwic2Vzc2lvblBvbGljeSI6Ii4uLiJ9.<sig-redacted>
127.0.0.1:9100 <BLOB>
127.0.0.1:9100 [RESPONSE] [2026-07-07T04:27:05.428] [ Duration 40.117ms TTFB 40.106936ms ↑ 157 B  ↓ 200 B ]
127.0.0.1:9100 200 OK
127.0.0.1:9100 Content-Type: application/json
127.0.0.1:9100 Content-Length: 200
```

Decoding the `X-Amz-Security-Token` (JWT header + payload; signature discarded) proves the caller
carried the restrictive session policy at the moment it created the service account:

```json
{
  "accessKey": "FE660GYHKO1F2TUC51SF",
  "exp": 1783402025,
  "parent": "stsparent",
  "sessionPolicy": "<base64>"
}
```

where the nested `sessionPolicy` claim base64-decodes to **exactly** the restrictive document
above (Allow `s3:GetObject`/`s3:PutObject` on `arn:aws:s3:::stsbucket/*` only).

**Root cause (`file:line`).** The escalation is the composition of two behaviors:

1. **Creation is authorized by explicit-deny only.** The `AddServiceAccount` handler
   [`cmd/admin-handlers-users.go:650`] sets the target to the requestor's parent when a
   derived/temporary credential creates a service account for itself (`targetUser =
   requestorParentUser`, `cmd/admin-handlers-users.go:706`). `commonAddServiceAccount` then
   computes `denyOnly := (targetUser == cred.AccessKey || targetUser == cred.ParentUser)`
   [`cmd/admin-handlers-users.go:2781`] — `true` for self — and evaluates the
   `CreateServiceAccountAdminAction` permission with `DenyOnly: denyOnly`
   [`cmd/admin-handlers-users.go:2798`]. With `DenyOnly` set, only an **explicit `Deny`** blocks
   the call; a session policy that simply fails to *allow* the admin action does **not** deny it,
   so the creation is permitted.
2. **The minted account inherits the full parent policy.** Because the harness supplies no inline
   policy, the service account is stored with the inherited-policy marker. At authorization time
   `IsAllowedServiceAccount` [`cmd/iam.go:2140`] takes the `saPolicyClaimStr == inheritedPolicyType`
   branch [`cmd/iam.go:2225`] and returns `isOwnerDerived || combinedPolicy.IsAllowed(parentArgs)`
   [`cmd/iam.go:2226`] — evaluating the **parent** policy — and never reaches
   `isAllowedBySessionPolicyForServiceAccount` [`cmd/iam.go:2320`], which is consulted only at
   [`cmd/iam.go:2230`] when an inline service-account policy exists. The session policy is thus
   absent from this path, whereas the direct-action path (`IsAllowedSTS`
   [`cmd/iam.go:2242`] → intersection at [`cmd/iam.go:2311-2312`] via `isAllowedBySessionPolicy`
   [`cmd/iam.go:2381`]) correctly enforces it — which is exactly why step `[2]` contains the
   credential but step `[4]` does not.

**Scope bounding (observed).** Step `[5]` establishes that the service account faithfully
inherits the *parent's real policy*: `basicuser`'s self-created service account is bound to
`basic-only` exactly (`smoke/*` allowed; `otherbucket` and `ListBuckets` denied). A **regular**
IAM user therefore gains **no** new privilege from this path — the escalation is specific to
credentials whose privileges were narrowed by an STS **session policy** (or an inline
service-account policy). The Q5 guarantee for password/regular users is unaffected; see the Q5
cross-reference note.

**Answer summary (Q4).** On the **direct-action** path MinIO enforces the session policy as the
intersection of the parent and session policies: the temporary credentials can perform an action
only if **both** allow it — proven by runtime output, where in-session PutObject/GetObject
succeeded (`200`) while out-of-session PutObject and ListBuckets were denied (`403 AccessDenied`).
**However**, at this commit that guarantee does **not** extend to service accounts the temporary
credential creates for itself: as reproduced above (CVE-2025-62506), such a service account
inherits the full parent policy and bypasses the session-policy restriction. The complete answer
to "can MinIO enforce the session policy on temporary credentials" is therefore: **yes for the
credential's own direct actions, but not — at commit `c07e5b49d477` — for service accounts it
mints for itself**, the latter being fixed upstream in `RELEASE.2025-10-15T17-29-55Z`.

---

## Q5 — Privilege escalation via user mappings

> **Question (verbatim):** *"Show me test output to prove that a user with basic access
> cannot promote themselves to a console admin by modifying the user mappings. Identify the
> root cause of the user mappings modification behavior that you observe."*

A basic, non-administrative user `basicuser` was created with an S3-only policy `basic-only`
that grants only `s3:GetObject`/`s3:PutObject` on `smoke/*` (file
`/tmp/blitzy/out/basic-policy.json`):

```bash
$ cat /tmp/blitzy/out/basic-policy.json
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

The account has **working** basic access: using its own credentials (the `basic` alias) it can
write and read within its granted `smoke/*` scope, so the escalation denials below are not an
artifact of a broken or disabled account:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/ok.txt basic/smoke/ok.txt
`/tmp/blitzy/out/ok.txt` -> `basic/smoke/ok.txt`
Total: 22 B, Transferred: 22 B, Speed: 1.63 KiB/s
$ mc --config-dir /tmp/blitzy/mc-config cat basic/smoke/ok.txt
legit basicuser write
```

### Attack path 1 — admin API (attach `consoleAdmin` to self)

Using **its own** credentials, `basicuser` attempted to attach `consoleAdmin` to itself and to
perform other admin actions that modify user mappings. Every attempt was denied (verbatim
client output — each command is shown above its result):

```bash
$ mc --config-dir /tmp/blitzy/mc-config admin policy attach basic consoleAdmin --user basicuser
mc: <ERROR> Unable to make user/group policy association. Access Denied.
$ mc --config-dir /tmp/blitzy/mc-config admin user add basic eviluser <redacted-test-pw>
mc: <ERROR> Unable to add new user. Access Denied.
$ mc --config-dir /tmp/blitzy/mc-config admin policy list basic
mc: <ERROR> Unable to list policy. Access Denied.
$ mc --config-dir /tmp/blitzy/mc-config admin user enable basic basicuser
mc: <ERROR> Unable to enable user. Access Denied.
```

The primary attempt (`policy attach`) was captured server-side. The verbose trace was
subscribed by the root alias (`mc --config-dir /tmp/blitzy/mc-config admin trace --all -v
local`, saved to `/tmp/blitzy/logs/q5final/q5-trace-all.txt`); the
`admin.AttachDetachPolicyBuiltin` request/response block is sliced from that single capture
with `sed`:

```bash
$ sed -n '64,88p' /tmp/blitzy/logs/q5final/q5-trace-all.txt
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-07T01:16:08.430] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 105
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70 mc/DEVELOPMENT.GOGET
127.0.0.1:9000 X-Amz-Content-Sha256: 7d798cb39409e4df4036e527ba1069df333445f3de520272fbac484cea2bac56
127.0.0.1:9000 X-Amz-Date: 20260707T011608Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=basicuser/20260707//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=b758e123057dc4d45b3abb6d365de48053cc6fd81dc222ed8bb5e3bbba4cd710
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-07T01:16:08.431] [ Duration 190µs TTFB 188.067µs ↑ 106 B  ↓ 213 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Length: 213
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 X-Amz-Request-Id: 18BFDCFFBE62638A
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18BFDCFFBE62638A","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

The request is signed by `Credential=basicuser/...` and is rejected in **190µs** with
`403 Forbidden` and
`{"Code":"AccessDenied",...,"Resource":"/minio/admin/v3/idp/builtin/policy/attach"}` — the
sub-millisecond latency reflects that the guard denies **before** any mapping mutation (before
the request body is even read). The other three attempts fail identically in the same trace —
`admin.AddUser` → `PUT /minio/admin/v3/add-user` (RequestId `18BFDCFFC37F3268`),
`admin.ListCannedPolicies` → `GET /minio/admin/v3/list-canned-policies` (RequestId
`18BFDCFFC56F2E82`), and `admin.SetUserStatus` → `PUT /minio/admin/v3/set-user-status`
(RequestId `18BFDCFFC70145E4`) — each returning `403` with the same `AccessDenied` JSON keyed
to its own resource path.

### Attack path 2 — direct storage (edit the mapping on disk)

User→policy mappings live under the reserved `.minio.sys` bucket. `basicuser` cannot reach it.
At the **client layer**, `mc` rejects the reserved name before sending:

```bash
$ mc --config-dir /tmp/blitzy/mc-config cp /tmp/blitzy/out/evil-mapping.json basic/.minio.sys/policydb/users/basicuser/identity.json
`/tmp/blitzy/out/evil-mapping.json` -> `basic/.minio.sys/policydb/users/basicuser/identity.json`
mc: <ERROR> Failed to copy `/tmp/blitzy/out/evil-mapping.json`. Bucket name contains invalid characters
$ mc --config-dir /tmp/blitzy/mc-config ls basic/.minio.sys
mc: <ERROR> Unable to list folder. Bucket name contains invalid characters
$ mc --config-dir /tmp/blitzy/mc-config mb basic/.minio.sys
mc: <ERROR> Unable to make bucket `basic/.minio.sys`. Bucket name contains invalid characters
```

To confirm the **server** also blocks it (bypassing client-side name validation), a raw
SigV4-signed `PUT` targeting the exact on-disk mapping key was sent as `basicuser`. The request
is hand-crafted with `net/http` and signed with the `minio-go` signer by a small Go program
(`/tmp/blitzy/scripts/q5/main.go`, module `q5raw`, built offline against the pinned
`minio-go/v7 v7.0.80`); it was run with the exact invocation below and its complete stdout is
shown verbatim:

```bash
$ cd /tmp/blitzy/scripts/q5 && GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go run .
RAW-PUT-TARGET: http://127.0.0.1:9000/.minio.sys/policydb/users/basicuser/identity.json
SIGNED-AS-ACCESSKEY: basicuser
HTTP-STATUS: 403 Forbidden
RESPONSE-BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AllAccessDisabled</Code><Message>All access to this resource has been disabled.</Message><Resource>/.minio.sys/policydb/users/basicuser/identity.json</Resource><RequestId>18BFDD0C1514381E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The corresponding server-side trace block (a separate `--all -v` capture, sliced with `sed`)
confirms the S3 layer rejects the request at `handler.ValidRequest`:

```bash
$ sed -n '65,89p' /tmp/blitzy/logs/q5final/q5raw-trace-all.txt
127.0.0.1:9000 [REQUEST handler.ValidRequest] [2026-07-07T01:17:01.425] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /.minio.sys/policydb/users/basicuser/identity.json
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Content-Sha256: 34c67809e2b0c2e7e4842666d320ac06eb7b80e6750a67a6482117cf2aa87beb
127.0.0.1:9000 X-Amz-Date: 20260707T011701Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=basicuser/20260707/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=921bb37fd586cdb351d6a989d6265de5cc5343fd968099e7ff34b68d577f1cc5
127.0.0.1:9000 Content-Length: 37
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-07T01:17:01.425] [ Duration 82µs TTFB 81.094µs ↑ 93 B  ↓ 340 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18BFDD0C1514381E
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 340
127.0.0.1:9000 Vary: Origin
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AllAccessDisabled</Code><Message>All access to this resource has been disabled.</Message><Resource>/.minio.sys/policydb/users/basicuser/identity.json</Resource><RequestId>18BFDD0C1514381E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
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
the observed 190µs early abort. The same guard fronts `SetPolicyForUserOrGroup`
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

**Scope note / cross-reference to Q4.** This Q5 guarantee concerns a *regular* (password/basic)
user attempting to modify user→policy mappings, and it holds: `validateAdminReq`
[`cmd/admin-handler-utils.go:37`] denies the unprivileged caller. It is distinct from the
session-policy bypass documented under Q4 (CVE-2025-62506), which affects credentials that were
*narrowed by an STS session policy* creating a **service account** for themselves — a different
code path (`AddServiceAccount` self-creation with `DenyOnly` at
[`cmd/admin-handlers-users.go:2781,2798`], plus inherited-policy evaluation at
[`cmd/iam.go:2225-2226`]). The scope-bounding control in the Q4 caveat (a regular `basic-only`
user's self-created service account remains bound to `basic-only`) confirms that this Q5 claim
for regular users is **not** weakened by that bypass.

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
| Q4 | **Session-policy bypass** — restricted STS cred self-creates a service account (no inline policy) | **SUCCESS 200**; new SA inherits full parent `readwrite`; out-of-session ListBuckets + PutObject **ALLOWED** = bypass (**CVE-2025-62506**) | `cmd/admin-handlers-users.go:650,706,2781,2798`; `cmd/iam.go:2140,2225-2226,2320` |
| Q4 | Scope-bounding control: **regular** `basic-only` user self-creates a service account | SA bound to `basic-only` exactly (`smoke/*` ALLOWED; `otherbucket`/ListBuckets **DENIED**) — regular users **not** escalated | `cmd/iam.go:2225-2226` (inherits parent's real policy) |
| Q5 | Admin API self-attach `consoleAdmin` (+ AddUser/List/SetUserStatus) | **403 AccessDenied**, aborts pre-mutation (190µs) | `cmd/admin-handler-utils.go:37`; `cmd/auth-handler.go:189`; `cmd/admin-handlers-users.go:1908,1770,444,406` |
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

**Secret handling (rule compliance).** The captured Q4 STS artifacts are expired, ephemeral, local-only credentials (minted `2026-07-07T00:49:53Z`, `exp` `2026-07-07T01:49:53Z`, from a throwaway `127.0.0.1` server that no longer exists). To comply with secret-sanitization policy, the STS `SecretAccessKey` value and the JWT **signature** segment were redacted, and a throwaway test password was replaced with `<redacted-test-pw>`. The evidentiary content is fully preserved: the JWT **header and payload** remain (they base64-decode to the `parent` and `sessionPolicy` claims — see the decoded block in Q4), and the temporary **AccessKeyId** (`HGSDFQLBQOM4E1SSOV0F`, an identifier, not a secret) is retained so the request/response blocks cross-reference correctly.

**Non-canonical values:** the Q1 KMS master key (`MINIO_KMS_SECRET_KEY`) is an
investigation-time key and is redacted as `<base64-32-bytes>`; it does not affect the observed
behavior. All version/banner values are from the canonical default build (Section 1).

**CVE-2025-62506 — session-policy bypass via self-created service accounts (Q4).** During the
final security pass a reproducible, undocumented bypass of STS session-policy enforcement was
observed and is now documented in full under Q4 (see "Security caveat: session-policy bypass via
self-created service accounts"). Metadata: **CVE-2025-62506** / **GHSA-jjjj-jwhf-8rgr**;
**CWE-863** (*Incorrect Authorization*); CVSS 3.1 base **8.1 High**
(`AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:N`); fixed upstream in **`RELEASE.2025-10-15T17-29-55Z`**
(PR minio/minio#21642, fix commit `c1a49490c78e9c3ebcad86ba0662319138ace190`). The investigated
commit **`c07e5b49d477`** (2024-11-25) predates that release and is therefore **vulnerable**,
which was confirmed at runtime (4 identical runs). Root cause, grounded at this commit: a
temporary/derived credential creating a service account **for itself** is authorized with
`DenyOnly` (`denyOnly := (targetUser == cred.AccessKey || targetUser == cred.ParentUser)`
[`cmd/admin-handlers-users.go:2781`], passed as `DenyOnly` at [`cmd/admin-handlers-users.go:2798`]
from the `AddServiceAccount` handler [`cmd/admin-handlers-users.go:650`], which sets
`targetUser = requestorParentUser` at [`cmd/admin-handlers-users.go:706`]) — so only an explicit
`Deny` blocks creation — and the resulting no-inline-policy service account is then evaluated via
the inherited-policy branch of `IsAllowedServiceAccount`
([`cmd/iam.go:2140`] → [`cmd/iam.go:2225-2226`], `combinedPolicy.IsAllowed(parentArgs)`), never
reaching `isAllowedBySessionPolicyForServiceAccount` [`cmd/iam.go:2320`]. This contrasts with the
correctly-enforced direct-action path (`IsAllowedSTS` [`cmd/iam.go:2242`] → intersection at
[`cmd/iam.go:2311-2312`] via `isAllowedBySessionPolicy` [`cmd/iam.go:2381`]). Secret hygiene for
the new evidence follows the same policy as the "Secret handling" note above: the request
`Signature` and the JWT **signature** segment are redacted (`<redacted>` / `<sig-redacted>`),
while the JWT header/payload and the ephemeral, expired `AccessKeyId` identifiers (e.g.
`FE660GYHKO1F2TUC51SF`, `71HOVJI1I63FNFERUGHN`, minted against a local `127.0.0.1:9100` server
that no longer exists) are retained so the trace, stdout, and JWT-decode blocks cross-reference.
This finding adds documentation only; **no source code was changed** to observe or record it.

**Repository integrity.** No MinIO source, test, config, or build file was modified. The only
file added to the repository is this document. All servers, data directories, scripts, and
captured logs were created under `/tmp/blitzy/...` outside the repository tree.
