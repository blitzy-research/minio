# MinIO Runtime Security-Behavior Investigation — Branch `minio_c07e5b49d477`

**Repository:** `github.com/minio/minio` · **HEAD:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Methodology:** RUN-FIRST — every answer below is written from **observed runtime output** captured by building and running the real server / real request paths. Code reading is used only to *locate and explain* the observed behavior. Each factual claim carries a `file:line` reference against the HEAD above, and every observation block shows the exact command/payload that produced it.

This document answers five security-behavior questions:

| # | Question | Evidence surface |
|---|----------|------------------|
| Q1 | Encryption-vs-policy precedence on an unencrypted PUT by a broad-write user | live server + admin trace stream (`madmin` `ServiceTrace`) + raw HEAD/StatObject |
| Q2 | Object-lock delete logging (legal hold / compliance / governance + bypass) | live server + admin trace stream + raw S3 error XML |
| Q3 | Bit-rot detection on GET after on-disk corruption | live 4-drive erasure server + admin trace stream (storage-type) + `madmin` `Heal` |
| Q4 | STS session-policy enforcement (intersection semantics) | genuine `go test` harness driving real `AssumeRole` with an inline session policy |
| Q5 | Privilege-escalation prevention via user→policy mapping + root cause | genuine `go test` harness driving the real admin policy-attach API as a basic user |

---

## 1. Introduction — Canonical Build, Launch, and Runtime Configuration

### 1.1 Toolchain

```
$ go version
go version go1.23.12 linux/amd64
```

The module declares `go 1.23` — `go.mod:3` (`go 1.23`) — so `go1.23.12` satisfies the directive. Every `go` command was run with:

```
export PATH=/usr/local/go/bin:/root/go/bin:$PATH
export GOROOT=/usr/local/go GOPATH=/root/go GOMODCACHE=/root/go/pkg/mod GO111MODULE=on
```

`go mod download` completed with exit 0 (required both to build and to read the Q5 admin-action constants from the module cache, `github.com/minio/pkg/v3 v3.0.22` — `go.mod:55`).

### 1.2 Canonical build

The Makefile build recipe (`Makefile:179`) is `@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null`. LDFLAGS are optional; an un-stamped build reports version `DEVELOPMENT.GOGET` (`cmd/build-constants.go:32`, `Version = "DEVELOPMENT.GOGET"`). The exact command used (built out-of-tree to keep the repo pristine):

```
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-build/minio ./
```

```
$ /tmp/minio-build/minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

The `mc` command-line client is **not** installed in this environment and cannot be built here (it lives in a separate repository, `github.com/minio/mc`, which is not present in the module cache and cannot be fetched — this environment has no internet access). Per AAP §0.2.2, the sanctioned alternative real path is to subscribe to the admin trace endpoint **directly through the `madmin` SDK**. All "server TRACE logs" in this document were therefore captured by a small out-of-repository Go program, `tracecap`, that calls `madmin.AdminClient.ServiceTrace` — the identical operational path `mc admin trace` uses, subscribing to `/minio/admin/v3/trace` (route registered at `cmd/admin-router.go:410` → `adminMiddleware(adminAPI.TraceHandler, noObjLayerFlag)`). `tracecap` requests every trace type (`ServiceTraceOpts{S3, Internal, Storage, OS, Scanner, Healing: true, Threshold: 0}`) so that both HTTP request/response traces and storage-layer traces (with their `Error` field) are surfaced. It was built offline against the repository's own pinned dependency versions (`github.com/minio/madmin-go/v3 v3.0.77` — `go.mod`) and removed after the investigation; it adds nothing to the repository. The `tracecap` invocation is shown inline with each capture below.

### 1.3 Canonical launch (KMS-enabled, four-drive erasure)

```
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin
# KMS key required for SSE-S3 / auto-encryption (Q1). Format is <key-name>:<base64 32-byte key>.
# This is MinIO's own well-known, non-secret TEST key, committed verbatim throughout the
# repository's own scripts (e.g. docs/bucket/versioning/versioning-tests.sh:35) — it is a
# public test value, not a production credential:
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="
export MINIO_KMS_AUTO_ENCRYPTION=on          # for the Q1 auto-encryption interpretation
export MINIO_API_REQUESTS_MAX=10000

/tmp/minio-build/minio server /tmp/minio-data/disk1 /tmp/minio-data/disk2 \
                              /tmp/minio-data/disk3 /tmp/minio-data/disk4 \
                              --address :9000 --console-address :9001
```

- `MINIO_KMS_SECRET_KEY` env key defined at `internal/kms/config.go:63` (`EnvKMSSecretKey = "MINIO_KMS_SECRET_KEY"`); format `<key-name>:<base64 32-byte key>`. The literal value shown (`my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=`) is the exact value used to launch this server; it is MinIO's own public test key, committed verbatim in the repository's scripts (e.g. `docs/bucket/versioning/versioning-tests.sh:35`, `docs/distributed/decom-encrypted-sse-s3.sh:16`), not a production credential.
- `MINIO_KMS_AUTO_ENCRYPTION` env key at `internal/crypto/auto-encryption.go:31` (`EnvKMSAutoEncryption = "MINIO_KMS_AUTO_ENCRYPTION"`).
- Default root credentials are `minioadmin:minioadmin`.

**Startup banner (verbatim, `/tmp/minio-investigation/logs/server.stdout.log`):**

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
API: http://10.236.12.248:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
WebUI: http://10.236.12.248:9001 http://172.17.0.1:9001 http://127.0.0.1:9001
Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

The line `Formatting 1st pool, 1 set(s), 4 drives per set.` proves the four-drive erasure backend. Querying the erasure layout through the real admin path (`madmin.AdminClient.StorageInfo`, the same call `mc admin info` uses) confirms it — the exact program output was:

```
$ /tmp/minio-investigation/bin/srvinfo        # calls madmin.AdminClient.StorageInfo against 127.0.0.1:9000
Backend type: 2
Standard storage class: EC:2 (parity), data disks=[2]
Disks reported: 4
  disk[0] endpoint=/tmp/minio-data/disk1 state=ok drivePath=/tmp/minio-data/disk1
  disk[1] endpoint=/tmp/minio-data/disk2 state=ok drivePath=/tmp/minio-data/disk2
  disk[2] endpoint=/tmp/minio-data/disk3 state=ok drivePath=/tmp/minio-data/disk3
  disk[3] endpoint=/tmp/minio-data/disk4 state=ok drivePath=/tmp/minio-data/disk4
online=4 offline=0
```

`Backend type: 2` is `madmin.Erasure` (`BackendType` const — `github.com/minio/madmin-go/v3/info-commands.go`). `EC:2` (`StandardSCParity = 2`, `StandardSCData = [2]`) means **2 data + 2 parity** shards across the 4 drives — the server can lose or corrupt up to 2 shards and still reconstruct. This is the configuration Q3 requires.

**Readiness (verbatim commands + results):**

```
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/live
200
```

The readiness path constant is `healthCheckReadinessPath = "/ready"` (`cmd/healthcheck-router.go:29`), served under the `/minio/health` prefix.

### 1.4 Why the prerequisites are required

- **Four-drive erasure (Q3):** bit-rot *detection and reconstruction* only manifests on a multi-drive erasure backend. A single-drive/FS deployment cannot reconstruct and would surface a plain read error instead of self-healing.
- **KMS (Q1):** SSE-S3 and auto-encryption require a configured KMS. `globalAutoEncryption = crypto.LookupAutoEncryption()` (`cmd/config-current.go:532`) and the server enforces `GlobalKMS != nil` when auto-encryption is on (`cmd/config-current.go:533`).

### 1.5 Trace-capture method

The primary "server TRACE logs" surface is the operational admin trace stream. Because `mc` is unavailable here (see §1.2), the stream is captured with the out-of-repository `tracecap` program, which subscribes to the same endpoint `mc admin trace` uses (`/minio/admin/v3/trace` — `cmd/admin-router.go:410`) via `madmin.AdminClient.ServiceTrace`. Typical invocation:

```
$ /tmp/minio-investigation/bin/tracecap -endpoint 127.0.0.1:9000 -all -v \
      -out /tmp/minio-investigation/logs/<capture>.trace.log
```

`tracecap` renders each HTTP call as a `[REQUEST <s3.Func>] [Client: <ip>]` block with the method, path and full sorted request headers, followed by a `[RESPONSE] [ Duration … ↑ … ↓ … ]` block with the numeric status code, status text, response headers and body. Storage-layer traces (`storage.*`, `os.*`) are rendered on a single line ending with `ERROR=<err>` when the `TraceInfo.Error` field is populated (this is the surface used for Q3's on-GET bit-rot signal). A representative slice of live output — the readiness probe plus the storage reads triggered by a HEAD on a non-existent bucket — verifies the format end-to-end:

```
2026-07-08T06:10:07.384630Z [REQUEST health.Readiness] [Client: 127.0.0.1]
2026-07-08T06:10:07.384630Z GET /minio/health/ready
2026-07-08T06:10:07.384630Z Accept: */*
2026-07-08T06:10:07.384630Z Content-Length: 0
2026-07-08T06:10:07.384630Z Host: 127.0.0.1:9000
2026-07-08T06:10:07.384630Z User-Agent: curl/8.14.1
2026-07-08T06:10:07.384630Z [RESPONSE] [ Duration 81.151µs  ↑ 38 B  ↓ 0 B ]
2026-07-08T06:10:07.384630Z 200 OK
2026-07-08T06:10:07.384630Z Accept-Ranges: bytes
2026-07-08T06:10:07.384630Z Content-Length: 0
2026-07-08T06:10:07.384630Z Server: MinIO
2026-07-08T06:10:07.384630Z X-Amz-Request-Id: 18C03B9F33434FBC
2026-07-08T06:10:07.391753Z storage.ReadXL /tmp/minio-data/disk4 .minio.sys buckets/nonexistent-bucket-xyz/.metadata.bin 52.96µs  ERROR=file not found
```

Server stdout logs were also captured as the "server logs" interpretation (`/tmp/minio-investigation/logs/server.stdout.log`). For Q4/Q5, the sanctioned "test output" surface is a genuine `go test` harness (built out-of-repository against MinIO's own pinned dependencies), which boots a real server and drives it through the genuine STS/admin entry points.

---

## 2. Q1 — Encryption-vs-Policy Precedence on an Unencrypted PUT

**Question.** When a bucket enforces an encryption requirement and a user with broad write permissions performs an *unencrypted* PUT, what ordered runtime sequence occurs (in the TRACE stream), and which mechanism takes precedence — the bucket encryption requirement or the user's write grant — and via what concrete handler/function call order?

### 2.1 Direct answer

**Authorization runs first and is independent of encryption.** A broad write grant *admits* the request at `IAMSys.IsAllowed` (`cmd/iam.go:2437`). Two distinct "encryption requirement" mechanisms then behave differently:

- **(a) Transparent forced encryption** (bucket default-encryption and/or `MINIO_KMS_AUTO_ENCRYPTION=on`): the write is admitted, and the encryption requirement takes effect **inside the PUT handler** at `sseConfig.Apply` (`cmd/object-handlers.go:1894-1897`). The object is transparently encrypted even though the client sent no encryption header. The user's broad write grant does **not** let them store an object in violation of the bucket's encryption requirement.
- **(b) A policy `Deny` keyed on the SSE header**: the unencrypted PUT is rejected **at the authorization gate** `IsAllowed` (`cmd/iam.go:2437`) with `AccessDenied` (HTTP 403) — the request never reaches the handler body. Deny-over-Allow precedence is evaluated inside the external `github.com/minio/pkg/v3/policy` engine.

So the ordering is: **authorize (Deny can reject here) → if admitted, apply the bucket encryption requirement in the handler.** The encryption requirement is never subordinate to the write grant.

### 2.2 Ordered call chain (grounded)

```
client PUT
  └─ PutObjectHandler                         cmd/object-handlers.go:1745
       └─ isPutActionAllowed                   cmd/auth-handler.go:749 (called from the handler)
            └─ globalIAMSys.IsAllowed          cmd/iam.go:2437   ← AUTHORIZATION GATE (Deny rejects here)
       └─ (if admitted) globalBucketSSEConfigSys.Get(bucket)   cmd/object-handlers.go:1894
       └─ sseConfig.Apply(r.Header, sse.ApplyOptions{AutoEncrypt: globalAutoEncryption})
                                               cmd/object-handlers.go:1895-1897
            └─ BucketSSEConfig.Apply           internal/bucket/encryption/bucket-sse-config.go:135  ← ENCRYPTION REQUIREMENT
```

The same SSE application occurs on the copy path (`cmd/object-handlers.go:1231-1232`) and the multipart path (`cmd/object-multipart-handlers.go:89-90`).

**The handler call site (verbatim, `cmd/object-handlers.go:1893-1897` — no elision):**

```go
	// Check if bucket encryption is enabled
	sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
	sseConfig.Apply(r.Header, sse.ApplyOptions{
		AutoEncrypt: globalAutoEncryption,
	})
```

**The Apply logic (verbatim, `internal/bucket/encryption/bucket-sse-config.go:135-153` — no elision):**

```go
func (b *BucketSSEConfig) Apply(headers http.Header, opts ApplyOptions) {
	if crypto.Requested(headers) {
		return
	}
	if b == nil {
		if opts.AutoEncrypt {
			headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
		}
		return
	}

	switch b.Algo() {
	case xhttp.AmzEncryptionAES:
		headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
	case xhttp.AmzEncryptionKMS:
		headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
		headers.Set(xhttp.AmzServerSideEncryptionKmsID, b.KeyID())
	}
}
```

Cause→effect: if the client already requested SSE (`crypto.Requested(headers)`) Apply returns unchanged; otherwise, if there is no bucket config but auto-encrypt is on it **forces** `X-Amz-Server-Side-Encryption: aws:kms`; else it applies the bucket default rule (`AES256` for SSE-S3, or `aws:kms` + KMS key id). An unencrypted PUT is therefore transparently forced to encrypt.

### 2.3 Observed — interpretation (a): transparent forced encryption

**(a-i) Global auto-encryption (`MINIO_KMS_AUTO_ENCRYPTION=on`).** A broad-write user `q1user` (identity policy `q1broad` = `Allow s3:*` on `arn:aws:s3:::*`) performed an unencrypted `PutObject` — no SSE option — via a `minio-go/v7.0.80` driver. The server-side trace of that exact request (captured by `tracecap`, i.e. `madmin.ServiceTrace` on `/minio/admin/v3/trace`) is the definitive evidence of the "TRACE stream" the question asks about. **Note the `SignedHeaders` list in the `Authorization` header — it does *not* include `x-amz-server-side-encryption` — yet the request block contains `X-Amz-Server-Side-Encryption: aws:kms`. The client never sent that header; it was injected server-side by `sseConfig.Apply` after authorization admitted the request:**

```
2026-07-08T06:19:43.993826Z [REQUEST s3.PutObject] [Client: 127.0.0.1]
2026-07-08T06:19:43.993826Z PUT /q1-auto/auto-unencrypted.txt
2026-07-08T06:19:43.993826Z Accept-Encoding: gzip
2026-07-08T06:19:43.993826Z Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260708/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=e25a97047cd7c9d8e803e9088d5835f7745b3ed0927d124b0bfa0e35dccded12
2026-07-08T06:19:43.993826Z Content-Length: 430
2026-07-08T06:19:43.993826Z Content-Type: text/plain
2026-07-08T06:19:43.993826Z Host: 127.0.0.1:9000
2026-07-08T06:19:43.993826Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:19:43.993826Z X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
2026-07-08T06:19:43.993826Z X-Amz-Date: 20260708T061943Z
2026-07-08T06:19:43.993826Z X-Amz-Decoded-Content-Length: 256
2026-07-08T06:19:43.993826Z X-Amz-Server-Side-Encryption: aws:kms
2026-07-08T06:19:43.993826Z [request body 6 bytes]
<BLOB>
2026-07-08T06:19:43.993826Z [RESPONSE] [ Duration 3.395597ms  ↑ 594 B  ↓ 0 B ]
2026-07-08T06:19:43.993826Z 200 OK
2026-07-08T06:19:43.993826Z Accept-Ranges: bytes
2026-07-08T06:19:43.993826Z Content-Length: 0
2026-07-08T06:19:43.993826Z ETag: "76a351e405dc72ff92f2c10ac114c59d"
2026-07-08T06:19:43.993826Z Server: MinIO
2026-07-08T06:19:43.993826Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T06:19:43.993826Z Vary: Origin
2026-07-08T06:19:43.993826Z Vary: Accept-Encoding
2026-07-08T06:19:43.993826Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T06:19:43.993826Z X-Amz-Request-Id: 18C03C2573D9BB38
2026-07-08T06:19:43.993826Z X-Amz-Server-Side-Encryption: aws:kms
2026-07-08T06:19:43.993826Z X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
2026-07-08T06:19:43.993826Z X-Content-Type-Options: nosniff
2026-07-08T06:19:43.993826Z X-Ratelimit-Limit: 10000
2026-07-08T06:19:43.993826Z X-Ratelimit-Remaining: 10000
2026-07-08T06:19:43.993826Z X-Xss-Protection: 1; mode=block
<BLOB>
```

(The `<BLOB>` markers are MinIO's own trace rendering of a non-textual body; the request body line reports 6 bytes because the trace samples only the streaming chunk header.)

**Raw HEAD (`StatObject`) confirming the object is stored encrypted (F3 — client-captured, unedited):** the same driver then issued a `HEAD` and printed every SSE-related response header:

```
  [raw client-captured response] HEAD /q1-auto/auto-unencrypted.txt -> 200 OK
    X-Amz-Server-Side-Encryption: aws:kms
    X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
    Etag: "76a351e405dc72ff92f2c10ac114c59d"
    Content-Type: text/plain
    X-Amz-Request-Id: 18C03C2574120992
  StatObject(q1-auto/auto-unencrypted.txt): size=256 etag=76a351e405dc72ff92f2c10ac114c59d
    X-Amz-Server-Side-Encryption: aws:kms
    X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
```

**On-disk proof (stored ciphertext, not plaintext).** A companion run PUT a *known-plaintext* marker unencrypted, read it back through `GetObject` (transparent decrypt), and scanned all four drives for the marker string:

```
$ /tmp/minio-investigation/bin/q1disk
PUT etag=b5e90ffa725b189492bfc56a301cefae ; GET roundtrip matches plaintext=true ; SSE header="aws:kms"
$ grep -rl "PLAINTEXT_MARKER_UNIQUE_9c94aa53" /tmp/minio-data/disk{1,2,3,4}
NOT FOUND in plaintext on any drive (stored encrypted)
```

Because there is no bucket-level encryption rule on `q1-auto`, this is the `b == nil && opts.AutoEncrypt` branch of `Apply` (global auto-encryption forcing `aws:kms`), not a bucket rule. The `GET roundtrip matches plaintext=true` line proves transparent decryption on read, while the drive scan proves the object is at rest as ciphertext.

**(a-ii) Bucket-default encryption (SSE-S3, AES256).** With `SetBucketEncryption(q1-bkdef, sse.NewConfigurationSSES3())`, the same broad-write user's unencrypted PUT was transparently encrypted with `AES256`. Raw client-captured PUT and HEAD (unedited):

```
  [raw client-captured response] PUT /q1-bkdef/bkdef-unencrypted.txt -> 200 OK
    X-Amz-Server-Side-Encryption: AES256
    Etag: "213478da61a9d8ef7ff41fbf0156cbdd"
    X-Amz-Request-Id: 18C03C25869624CC
  [raw client-captured response] HEAD /q1-bkdef/bkdef-unencrypted.txt -> 200 OK
    X-Amz-Server-Side-Encryption: AES256
    Etag: "213478da61a9d8ef7ff41fbf0156cbdd"
    Content-Type: text/plain
    X-Amz-Request-Id: 18C03C2586DCB450
  StatObject(q1-bkdef/bkdef-unencrypted.txt): size=256 etag=213478da61a9d8ef7ff41fbf0156cbdd
    X-Amz-Server-Side-Encryption: AES256
```

This exercises the `switch b.Algo()` → `case xhttp.AmzEncryptionAES` branch of `Apply` (`internal/bucket/encryption/bucket-sse-config.go:147-148`), which sets `X-Amz-Server-Side-Encryption: AES256`.

### 2.4 Observed — interpretation (b): policy `Deny` keyed on the SSE header

A policy `Deny` keyed on the SSE header rejects the unencrypted PUT **at the authorization gate**, before the handler runs. There are two distinct authorization surfaces, and the runtime distinguishes them exactly as the code says:

- an **identity (user) policy** `Deny` is evaluated by `globalIAMSys.IsAllowed` (`cmd/iam.go:2437`) for an authenticated request, and
- a **bucket (resource) policy** `Deny` is evaluated by `globalPolicySys.IsAllowed` — and per `isPutActionAllowed` (`cmd/auth-handler.go:749`) the bucket-policy branch is only reached when the caller is **anonymous** (`cred.AccessKey == ""`, `cmd/auth-handler.go`). Both were reproduced below.

**(b-1) Identity-policy `Deny` (authenticated user).** Policy `q1denyunenc` = `Allow s3:*` **plus** `Deny s3:PutObject` with `Condition {Null: {s3:x-amz-server-side-encryption: [true]}}` (i.e. "deny when the SSE header is absent"), attached to `q1denyuser`. The unencrypted PUT was refused in **117.817µs** with a `349 B` error body; the server-side trace (note the `Authorization: …Credential=q1denyuser…` header — this is the authenticated path) is unedited:

```
2026-07-08T06:19:44.317313Z [REQUEST s3.PutObject] [Client: 127.0.0.1]
2026-07-08T06:19:44.317313Z PUT /q1-deny/denied-unencrypted.txt
2026-07-08T06:19:44.317313Z Accept-Encoding: gzip
2026-07-08T06:19:44.317313Z Authorization: AWS4-HMAC-SHA256 Credential=q1denyuser/20260708/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=3687696185563527c61f764c5df38a1db8d186eb7a53d39ea0f86b6f98112f8b
2026-07-08T06:19:44.317313Z Content-Length: 430
2026-07-08T06:19:44.317313Z Content-Type: text/plain
2026-07-08T06:19:44.317313Z Host: 127.0.0.1:9000
2026-07-08T06:19:44.317313Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:19:44.317313Z X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
2026-07-08T06:19:44.317313Z X-Amz-Date: 20260708T061944Z
2026-07-08T06:19:44.317313Z X-Amz-Decoded-Content-Length: 256
2026-07-08T06:19:44.317313Z [request body 6 bytes]
<BLOB>
2026-07-08T06:19:44.317313Z [RESPONSE] [ Duration 117.817µs  ↑ 135 B  ↓ 349 B ]
2026-07-08T06:19:44.317313Z 403 Forbidden
2026-07-08T06:19:44.317313Z Accept-Ranges: bytes
2026-07-08T06:19:44.317313Z Content-Length: 349
2026-07-08T06:19:44.317313Z Content-Type: application/xml
2026-07-08T06:19:44.317313Z Server: MinIO
2026-07-08T06:19:44.317313Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T06:19:44.317313Z Vary: Origin
2026-07-08T06:19:44.317313Z Vary: Accept-Encoding
2026-07-08T06:19:44.317313Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T06:19:44.317313Z X-Amz-Request-Id: 18C03C258721828B
2026-07-08T06:19:44.317313Z X-Content-Type-Options: nosniff
2026-07-08T06:19:44.317313Z X-Ratelimit-Limit: 10000
2026-07-08T06:19:44.317313Z X-Ratelimit-Remaining: 10000
2026-07-08T06:19:44.317313Z X-Xss-Protection: 1; mode=block
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>denied-unencrypted.txt</Key><BucketName>q1-deny</BucketName><Resource>/q1-deny/denied-unencrypted.txt</Resource><RequestId>18C03C258721828B</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

When the **same user** instead sent an **SSE-KMS** PUT, it **succeeded** (`200`, etag `5491f14cb51fb4d4c591342f5f57bb91`, response `X-Amz-Server-Side-Encryption: aws:kms`) because the `Null` condition no longer matched once the SSE header was present. This is the Deny-over-Allow precedence of the external `github.com/minio/pkg/v3/policy` engine, evaluated inside `IsAllowed`.

**Corrected observation (auto-encryption does *not* neutralise the Deny).** This run had `MINIO_KMS_AUTO_ENCRYPTION=on`, yet the unencrypted PUT was still rejected `403`. The reason is ordering: authorization (`isPutActionAllowed` → `IsAllowed`, `cmd/object-handlers.go:1836`) runs **before** `sseConfig.Apply` injects the SSE header (`cmd/object-handlers.go:1894`). At authorization time the request carries no SSE header, so the `Null` condition matches and the request is denied before the auto-encryption injection can occur. The Deny therefore fires regardless of auto-encryption — the two mechanisms do not interfere.

**(b-2) Bucket-policy `Deny` (anonymous request) — the genuine resource-policy path.** To exercise the `globalPolicySys.IsAllowed` bucket-policy branch, a bucket policy on `q1-anon` combined `Allow s3:PutObject` for `Principal *` with a `Deny s3:PutObject` for `Principal *` carrying the same `Null` SSE condition. An **anonymous** PUT (no credentials) was refused in **109.139µs**. The trace has **no `Authorization` header** — this is the anonymous branch, so the bucket policy (not any identity policy) is what denies it:

```
2026-07-08T06:19:44.632410Z [REQUEST s3.PutObject] [Client: 127.0.0.1]
2026-07-08T06:19:44.632410Z PUT /q1-anon/anon-unencrypted.txt
2026-07-08T06:19:44.632410Z Accept-Encoding: gzip
2026-07-08T06:19:44.632410Z Content-Length: 256
2026-07-08T06:19:44.632410Z Content-Type: text/plain
2026-07-08T06:19:44.632410Z Host: 127.0.0.1:9000
2026-07-08T06:19:44.632410Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:19:44.632410Z [request body 6 bytes]
<BLOB>
2026-07-08T06:19:44.632410Z [RESPONSE] [ Duration 109.139µs  ↑ 60 B  ↓ 345 B ]
2026-07-08T06:19:44.632410Z 403 Forbidden
2026-07-08T06:19:44.632410Z Accept-Ranges: bytes
2026-07-08T06:19:44.632410Z Content-Length: 345
2026-07-08T06:19:44.632410Z Content-Type: application/xml
2026-07-08T06:19:44.632410Z Server: MinIO
2026-07-08T06:19:44.632410Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T06:19:44.632410Z Vary: Origin
2026-07-08T06:19:44.632410Z Vary: Accept-Encoding
2026-07-08T06:19:44.632410Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T06:19:44.632410Z X-Amz-Request-Id: 18C03C2599E9BE27
2026-07-08T06:19:44.632410Z X-Content-Type-Options: nosniff
2026-07-08T06:19:44.632410Z X-Ratelimit-Limit: 10000
2026-07-08T06:19:44.632410Z X-Ratelimit-Remaining: 10000
2026-07-08T06:19:44.632410Z X-Xss-Protection: 1; mode=block
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>anon-unencrypted.txt</Key><BucketName>q1-anon</BucketName><Resource>/q1-anon/anon-unencrypted.txt</Resource><RequestId>18C03C2599E9BE27</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The **same anonymous principal** sending an **SSE-KMS** PUT to the same bucket **succeeded** (`200`, etag `4bef0bb13d593d69bccb2d13385918fd`, `X-Amz-Server-Side-Encryption: aws:kms`) — again the `Null` condition ceased to match. This is the true bucket-policy `Deny` path (`globalPolicySys.IsAllowed`) reproduced end-to-end; the only way to reach it, per `isPutActionAllowed`, is an anonymous request, which is why an authenticated same-account user is *not* subject to a bucket-policy `Deny` in MinIO.

### 2.5 Edge/error path — SSE-C over plain HTTP

SSE-C (customer-provided keys) requires TLS. A `minio-go` SSE-C `PutObject` over `http://127.0.0.1:9000` reached the server carrying the client-signed `X-Amz-Server-Side-Encryption-Customer-Algorithm/-Key/-Key-Md5` headers (note they *are* in the `SignedHeaders` list) and was rejected in **51.405µs**. Crucially, the trace func name is **`handler.ValidRequest`**, not `s3.PutObject` — the request is rejected inside `setRequestValidityMiddleware` **before** the s3-tagged handler runs. Full unedited trace:

```
2026-07-08T06:19:44.637265Z [REQUEST handler.ValidRequest] [Client: 127.0.0.1]
2026-07-08T06:19:44.637265Z PUT /q1-auto/ssec-over-http.txt
2026-07-08T06:19:44.637265Z Accept-Encoding: gzip
2026-07-08T06:19:44.637265Z Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260708/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-server-side-encryption-customer-algorithm;x-amz-server-side-encryption-customer-key;x-amz-server-side-encryption-customer-key-md5,Signature=5c0d7e0d61f477f0bf680e4da956c3123438f264115f5bbcf674930cc3b175de
2026-07-08T06:19:44.637265Z Content-Length: 430
2026-07-08T06:19:44.637265Z Content-Type: text/plain
2026-07-08T06:19:44.637265Z Host: 127.0.0.1:9000
2026-07-08T06:19:44.637265Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:19:44.637265Z X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
2026-07-08T06:19:44.637265Z X-Amz-Date: 20260708T061944Z
2026-07-08T06:19:44.637265Z X-Amz-Decoded-Content-Length: 256
2026-07-08T06:19:44.637265Z X-Amz-Server-Side-Encryption-Customer-Algorithm: AES256
2026-07-08T06:19:44.637265Z X-Amz-Server-Side-Encryption-Customer-Key: TxZUWQWcou9NvGmuEdvALWQFBYZ8Ycd+aVx9y7Tn18c=
2026-07-08T06:19:44.637265Z X-Amz-Server-Side-Encryption-Customer-Key-Md5: JDYTvgZBWZRM6nC6REhSPQ==
2026-07-08T06:19:44.637265Z [request body 6 bytes]
<BLOB>
2026-07-08T06:19:44.637265Z [RESPONSE] [ Duration 51.405µs  ↑ 271 B  ↓ 377 B ]
2026-07-08T06:19:44.637265Z 400 Bad Request
2026-07-08T06:19:44.637265Z Accept-Ranges: bytes
2026-07-08T06:19:44.637265Z Content-Length: 377
2026-07-08T06:19:44.637265Z Content-Type: application/xml
2026-07-08T06:19:44.637265Z Server: MinIO
2026-07-08T06:19:44.637265Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T06:19:44.637265Z Vary: Origin
2026-07-08T06:19:44.637265Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T06:19:44.637265Z X-Amz-Request-Id: 18C03C259A33D65B
2026-07-08T06:19:44.637265Z X-Content-Type-Options: nosniff
2026-07-08T06:19:44.637265Z X-Xss-Protection: 1; mode=block
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Requests specifying Server Side Encryption with Customer provided keys must be made over a secure connection.</Message><Resource>/q1-auto/ssec-over-http.txt</Resource><RequestId>18C03C259A33D65B</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

This matches `ErrInsecureSSECustomerRequest` (`cmd/api-errors.go:1181-1185`, `Code: "InvalidRequest"`, `HTTPStatusCode: http.StatusBadRequest`), written by `setRequestValidityMiddleware` (`cmd/generic-handlers.go:350`; the middleware's trace name is `handler.ValidRequest`, error written at `cmd/generic-handlers.go:445` and `:453`). The `handler.ValidRequest` func name in the trace confirms the rejection is in middleware, *before* the s3-tagged handler.

### 2.6 Precedence conclusion

Authorization is evaluated first and independently of encryption. A broad write grant admits the request at `IsAllowed` **unless** an explicit policy `Deny` keyed on the SSE header rejects it there (interpretation b, HTTP 403, no handler execution) — demonstrated both as an **identity-policy** `Deny` for an authenticated user (`globalIAMSys.IsAllowed`) and as a **bucket-policy** `Deny` for an anonymous request (`globalPolicySys.IsAllowed`). If admitted, the bucket encryption requirement takes effect **inside the handler** at `sseConfig.Apply` (interpretation a), transparently forcing encryption (`aws:kms` under auto-encryption, `AES256`/`aws:kms` under a bucket-default rule). Because authorization runs before `sseConfig.Apply` injects the header, a require-encryption `Deny` fires even when auto-encryption is enabled. In no case can the user's write grant store an object in violation of the encryption requirement — the encryption requirement is applied regardless of the grant. Corroborated by `docs/security/README.md` (SSE-C/SSE-S3, auto-encryption).

---

## 3. Q2 — Object-Lock Delete Logging (all three modes + governance bypass)

**Question.** With object locking enabled on a bucket, capture the specific runtime log/response entries emitted when a caller attempts to delete a locked object/version.

### 3.1 Direct answer

Every attempt to delete a locked version is refused with the S3 error **`InvalidRequest` / "Object is WORM protected and cannot be overwritten" / HTTP 400**. This is emitted for legal-hold `On`, compliance retention (future date), and governance retention (future date) without bypass. A governance delete **with** the `x-amz-bypass-governance-retention: true` header succeeds **only** if the caller holds `s3:BypassGovernanceRetention`; a caller lacking that permission gets `AccessDenied` / HTTP 403.

### 3.2 Enforcement and error rendering (grounded)

All delete-of-locked-version enforcement is in `enforceRetentionBypassForDelete` (`cmd/bucket-object-lock.go:84`). **Verbatim (no elision), `cmd/bucket-object-lock.go:99-159`:**

```go
	lhold := objectlock.GetObjectLegalHoldMeta(oi.UserDefined)
	if lhold.Status.Valid() && lhold.Status == objectlock.LegalHoldOn {
		return ObjectLocked{}
	}

	ret := objectlock.GetObjectRetentionMeta(oi.UserDefined)
	if ret.Mode.Valid() {
		switch ret.Mode {
		case objectlock.RetCompliance:
			// In compliance mode, a protected object version can't be overwritten
			// or deleted by any user, including the root user in your AWS account.
			// When an object is locked in compliance mode, its retention mode can't
			// be changed, and its retention period can't be shortened. Compliance mode
			// ensures that an object version can't be overwritten or deleted for the
			// duration of the retention period.
			t, err := objectlock.UTCNowNTP()
			if err != nil {
				internalLogIf(ctx, err, logger.WarningKind)
				return ObjectLocked{}
			}

			if !ret.RetainUntilDate.Before(t) {
				return ObjectLocked{}
			}
			return nil
		case objectlock.RetGovernance:
			// In governance mode, users can't overwrite or delete an object
			// version or alter its lock settings unless they have special
			// permissions. With governance mode, you protect objects against
			// being deleted by most users, but you can still grant some users
			// permission to alter the retention settings or delete the object
			// if necessary. You can also use governance mode to test retention-period
			// settings before creating a compliance-mode retention period.
			// To override or remove governance-mode retention settings, a
			// user must have the s3:BypassGovernanceRetention permission
			// and must explicitly include x-amz-bypass-governance-retention:true
			// as a request header with any request that requires overriding
			// governance mode.
			//
			byPassSet := objectlock.IsObjectLockGovernanceBypassSet(r.Header)
			if !byPassSet {
				t, err := objectlock.UTCNowNTP()
				if err != nil {
					internalLogIf(ctx, err, logger.WarningKind)
					return ObjectLocked{}
				}

				if !ret.RetainUntilDate.Before(t) {
					return ObjectLocked{}
				}
				return nil
			}
			// https://docs.aws.amazon.com/AmazonS3/latest/dev/object-lock-overview.html#object-lock-retention-modes
			// If you try to delete objects protected by governance mode and have s3:BypassGovernanceRetention, the operation will succeed.
			if checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, bucket, object.ObjectName) != ErrNone {
				return errAuthentication
			}
		}
	}
	return nil
}
```

- Legal hold `On` → `return ObjectLocked{}` (`cmd/bucket-object-lock.go:101`).
- Compliance with a future retain-until date → `return ObjectLocked{}` (`cmd/bucket-object-lock.go:121`; the NTP-failure fail-closed path is `:117`).
- Governance without bypass and a future retain-until date → `return ObjectLocked{}` (`cmd/bucket-object-lock.go:147`; fail-closed `:143`).
- Governance bypass guard → `if checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, bucket, object.ObjectName) != ErrNone { return errAuthentication }` (`cmd/bucket-object-lock.go:153-155`).
- The read-only deletion-check helper is `enforceRetentionForDeletion` (`cmd/bucket-object-lock.go:54`).

The error type and its rendering (verbatim):

```go
// cmd/object-api-errors.go:337
type ObjectLocked GenericError
```

```go
// cmd/api-errors.go:2298-2299
	case ObjectLocked:
		apiErr = ErrObjectLocked
```

```go
// cmd/api-errors.go:1059-1063
	ErrObjectLocked: {
		Code:           "InvalidRequest",
		Description:    "Object is WORM protected and cannot be overwritten",
		HTTPStatusCode: http.StatusBadRequest,
	},
```

### 3.3 Observed — bucket setup

The bucket `q2-lock` was created with `MakeBucketOptions{ObjectLocking: true}` (object locking auto-enables versioning; same pattern as `cmd/sts-handlers_test.go:184`). Deleting a *specific version* (`?versionId=`) routes through `enforceRetentionBypassForDelete`. Future retain-until dates were `now + 72h`.

### 3.4 Observed — the three lock modes (before / during / after)

Driver stdout — the before → during → after timeline plus the **full unedited response XML** captured for each blocked delete by the client `RoundTripper` (`/tmp/minio-investigation/logs/q2.driver.out`):

```
Bucket q2-lock created with ObjectLocking=true

=== (i) LEGAL HOLD On ===
  BEFORE: PUT legalhold.txt ok, versionId=fe7cd376-5981-489d-886b-3304d47560f7 etag=41e660e671533045e94b2f32e4113070
  DURING: legal hold = ON
  [raw DELETE response] DELETE /q2-lock/legalhold.txt?versionId=fe7cd376-5981-489d-886b-3304d47560f7 -> 400 Bad Request
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>legalhold.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/legalhold.txt</Resource><RequestId>18C03D3CA1A2ED96</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
  DELETE version (bypass=false) -> err=Object is WORM protected and cannot be overwritten
  AFTER: STILL PRESENT size=19

=== (ii) COMPLIANCE retention (future) ===
  BEFORE: PUT compliance.txt ok, versionId=1c98261e-a9e3-4991-b70b-847dc30881b7 etag=22657371a01b1fd880128c1fb2278e9e
  DURING: retention mode=COMPLIANCE until=2026-07-11T06:39:43Z
  [raw DELETE response] DELETE /q2-lock/compliance.txt?versionId=1c98261e-a9e3-4991-b70b-847dc30881b7 -> 400 Bad Request
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>compliance.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/compliance.txt</Resource><RequestId>18C03D3CA21BF177</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
  DELETE version (bypass=false) -> err=Object is WORM protected and cannot be overwritten
  AFTER: STILL PRESENT size=19

=== (iii) GOVERNANCE retention (future), delete WITHOUT bypass ===
  BEFORE: PUT governance.txt ok, versionId=d59adefa-4956-44b0-85cd-21faaa4c67c7 etag=fb29c6bb03e7f631d5135712bdfa43f7
  DURING: retention mode=GOVERNANCE until=2026-07-11T06:39:43Z
  [raw DELETE response] DELETE /q2-lock/governance.txt?versionId=d59adefa-4956-44b0-85cd-21faaa4c67c7 -> 400 Bad Request
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>governance.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/governance.txt</Resource><RequestId>18C03D3CA2A3BA05</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
  DELETE version (bypass=false) -> err=Object is WORM protected and cannot be overwritten
  AFTER: STILL PRESENT size=19
```

All three modes return the **identical** rendered error — `InvalidRequest` / "Object is WORM protected and cannot be overwritten" / HTTP 400 — differing only in the `<Key>`, because all three take a `return ObjectLocked{}` path (`:101`, `:121`, `:147`) that renders through the single `ErrObjectLocked` entry. Each object version is **STILL PRESENT** after its blocked delete.

The corresponding **server-side trace** (representative: the legal-hold delete; note there is **no** `x-amz-bypass-governance-retention` header, and the func is `s3.DeleteObject`) — full and unedited from `/tmp/minio-investigation/logs/q2.trace.log`:

```
2026-07-08T06:39:43.057861Z [REQUEST s3.DeleteObject] [Client: 127.0.0.1]
2026-07-08T06:39:43.057861Z DELETE /q2-lock/legalhold.txt?versionId=fe7cd376-5981-489d-886b-3304d47560f7
2026-07-08T06:39:43.057861Z Accept-Encoding: gzip
2026-07-08T06:39:43.057861Z Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=93a4e22a74712f265a1e131c495f5868a70e4d4c76501543bcc875d2498e0235
2026-07-08T06:39:43.057861Z Content-Length: 0
2026-07-08T06:39:43.057861Z Host: 127.0.0.1:9000
2026-07-08T06:39:43.057861Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:39:43.057861Z X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
2026-07-08T06:39:43.057861Z X-Amz-Date: 20260708T063943Z
2026-07-08T06:39:43.057861Z [RESPONSE] [ Duration 674.692µs  ↑ 93 B  ↓ 369 B ]
2026-07-08T06:39:43.057861Z 400 Bad Request
2026-07-08T06:39:43.057861Z Content-Length: 369
2026-07-08T06:39:43.057861Z Content-Type: application/xml
2026-07-08T06:39:43.057861Z Server: MinIO
2026-07-08T06:39:43.057861Z X-Amz-Request-Id: 18C03D3CA1A2ED96
2026-07-08T06:39:43.057861Z X-Ratelimit-Limit: 10000
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>legalhold.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/legalhold.txt</Resource><RequestId>18C03D3CA1A2ED96</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### 3.5 Observed — governance bypass (with and without permission)

The two runs below send the **identical** signed `X-Amz-Bypass-Governance-Retention: true` header; only the caller's permission differs. This isolates the guard at `cmd/bucket-object-lock.go:153` as the deciding factor — not the header.

**(iv) Bypass WITH permission (root) → 204, version removed.** Driver stdout:

```
=== (iv) GOVERNANCE bypass WITH permission (root) + x-amz-bypass-governance-retention ===
  [raw DELETE response] DELETE /q2-lock/governance.txt?versionId=d59adefa-4956-44b0-85cd-21faaa4c67c7 -> 204 No Content
  DELETE version (bypass=true) -> err=<nil>
  AFTER: NOT present: The specified version does not exist.
```

Server-side trace (the signed `x-amz-bypass-governance-retention` header is in `SignedHeaders`; response `204`) — full, unedited:

```
2026-07-08T06:39:43.076247Z [REQUEST s3.DeleteObject] [Client: 127.0.0.1]
2026-07-08T06:39:43.076247Z DELETE /q2-lock/governance.txt?versionId=d59adefa-4956-44b0-85cd-21faaa4c67c7
2026-07-08T06:39:43.076247Z Accept-Encoding: gzip
2026-07-08T06:39:43.076247Z Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=83a7a846d05e7f5948259c0734e6bcb7bf84e08151d6a98cf21666a5c501abd6
2026-07-08T06:39:43.076247Z Content-Length: 0
2026-07-08T06:39:43.076247Z Host: 127.0.0.1:9000
2026-07-08T06:39:43.076247Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:39:43.076247Z X-Amz-Bypass-Governance-Retention: true
2026-07-08T06:39:43.076247Z X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
2026-07-08T06:39:43.076247Z X-Amz-Date: 20260708T063943Z
2026-07-08T06:39:43.076247Z [RESPONSE] [ Duration 912.444µs  ↑ 127 B  ↓ 0 B ]
2026-07-08T06:39:43.076247Z 204 No Content
2026-07-08T06:39:43.076247Z Content-Length: 0
2026-07-08T06:39:43.076247Z Server: MinIO
2026-07-08T06:39:43.076247Z X-Amz-Request-Id: 18C03D3CA2BB79EC
```

This is the `cmd/bucket-object-lock.go:153` path: `checkRequestAuthType(..., policy.BypassGovernanceRetentionAction, ...) == ErrNone` → the bypass is authorized → the delete proceeds and the version is removed.

**(iv-contrast) Bypass by user `q2limited` LACKING `s3:BypassGovernanceRetention`** (its policy grants only `s3:DeleteObject`, `s3:DeleteObjectVersion`, `s3:GetObject`, `s3:GetBucketVersioning`, `s3:ListBucket`, `s3:GetObjectRetention`) **→ 403.** Driver stdout:

```
=== (iv-contrast) GOVERNANCE bypass by user LACKING s3:BypassGovernanceRetention ===
  BEFORE: PUT governance2.txt ok, versionId=38a2b8a3-942a-42fc-b719-2a1c75a2aaba etag=b0e7016f4bc0b4ccc376ee05fb4a1679
  DURING: retention mode=GOVERNANCE until=2026-07-11T06:39:43Z
  [raw DELETE response] DELETE /q2-lock/governance2.txt?versionId=38a2b8a3-942a-42fc-b719-2a1c75a2aaba -> 403 Forbidden
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>governance2.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/governance2.txt</Resource><RequestId>18C03D3CC24D0E08</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
  DELETE version (bypass=true, limited user) -> err=Access Denied.
  AFTER: STILL PRESENT size=21
```

Server-side trace — note the request carries the **same** signed `X-Amz-Bypass-Governance-Retention: true` header as the root run above, yet the response is `403`:

```
2026-07-08T06:39:43.605881Z [REQUEST s3.DeleteObject] [Client: 127.0.0.1]
2026-07-08T06:39:43.605881Z DELETE /q2-lock/governance2.txt?versionId=38a2b8a3-942a-42fc-b719-2a1c75a2aaba
2026-07-08T06:39:43.605881Z Accept-Encoding: gzip
2026-07-08T06:39:43.605881Z Authorization: AWS4-HMAC-SHA256 Credential=q2limited/20260708/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=d523da2f9ffc49393a63836f81e591e071ef4028ba8deee99d8cd141d1b92a42
2026-07-08T06:39:43.605881Z Content-Length: 0
2026-07-08T06:39:43.605881Z Host: 127.0.0.1:9000
2026-07-08T06:39:43.605881Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:39:43.605881Z X-Amz-Bypass-Governance-Retention: true
2026-07-08T06:39:43.605881Z X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
2026-07-08T06:39:43.605881Z X-Amz-Date: 20260708T063943Z
2026-07-08T06:39:43.605881Z [RESPONSE] [ Duration 695.348µs  ↑ 127 B  ↓ 335 B ]
2026-07-08T06:39:43.605881Z 403 Forbidden
2026-07-08T06:39:43.605881Z Content-Length: 335
2026-07-08T06:39:43.605881Z Content-Type: application/xml
2026-07-08T06:39:43.605881Z Server: MinIO
2026-07-08T06:39:43.605881Z X-Amz-Request-Id: 18C03D3CC24D0E08
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>governance2.txt</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/governance2.txt</Resource><RequestId>18C03D3CC24D0E08</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

This is the `cmd/bucket-object-lock.go:153-154` else-branch: the bypass header is set but `checkRequestAuthType` returns a non-`ErrNone` result (the user lacks `BypassGovernanceRetentionAction`), so `enforceRetentionBypassForDelete` returns `errAuthentication` → `AccessDenied` (403). The identical-header/different-outcome pair proves the permission — not the header — is the gate.

### 3.6 Note on "server log entries"

No *new server-stdout* lines are emitted during the WORM rejections: the block is returned to the caller as a normal S3 `400` client response, not logged as a server error. The only stdout path (`internalLogIf` at `cmd/bucket-object-lock.go:117`/`:143`) fires solely when `objectlock.UTCNowNTP()` fails (fail-closed trusted-time check), which did not occur here. The "runtime log/response entries" the question asks for are therefore the trace `REQUEST`/`RESPONSE` blocks plus the S3 error XML shown above. Corroborated by `docs/bucket/retention/README.md` (WORM modes, retention, legal hold).


---

## 4. Q3 — Bit-Rot Detection on GET (four-drive erasure backend)

**Question.** Demonstrate how the system handles unauthorized manual data corruption in the storage backend: corrupt on-disk data directly, then GET, and capture the runtime logs generated during that GET.

### 4.1 Direct answer

Corrupting an on-disk data shard is detected via a **HighwayHash256** checksum mismatch, which surfaces `errFileCorrupt` ("file is corrupted", `cmd/storage-errors.go:104`) from the streaming bit-rot reader (`cmd/bitrot-streaming.go:185`). The erasure decoder flags a heal and reconstructs the object from the surviving data shard + parity (`cmd/erasure-decode.go:197,228`), so the GET still returns the **correct** bytes (HTTP 200).

Two facts about the **runtime log surface at GET time** were observed directly and must be stated plainly:

- **No "file is corrupted" line is emitted during a plain GET** — neither on the server stdout nor in the admin trace stream. This is *not* an accident of log level; it is structural. The `storage.ReadFileStream` trace is published by the `defer done(length, &err)` in the disk wrapper (`cmd/xl-storage-disk-id-check.go:452`) at the moment the stream is **opened** — which *succeeds*, because a corrupt-but-present shard file opens without error. The HighwayHash mismatch (`errFileCorrupt`) only arises later, while `erasure.Decode` **consumes** the returned `io.ReadCloser` block-by-block (`cmd/bitrot-streaming.go:185`), long after that storage trace has already been published with an empty `Error` field. On the GET code path the sentinel is then **intentionally swallowed** (`cmd/erasure-object.go:414` sets `err = nil`) after an asynchronous MRF heal is queued (`BitrotScan: true`). So there is no code point on the GET path at which the sentinel is logged.
- **The runtime detection signature that *is* observable at GET time is the extra parity read.** A healthy GET of this object reads exactly the two data shards (disk1 + disk2); the GET issued while disk1 was corrupt read **three** shards — the two data shards *plus* one parity shard (disk3) — which is the decoder pulling parity to reconstruct the block that failed its checksum. Both were captured with the real admin trace stream (§4.5).

The **explicit** detection surface is the deep-scan heal (`madmin.Heal` with `HealDeepScan`, the same call `mc admin heal --scan deep` wraps), where `VerifyFile`→`bitrotVerify` re-checks every shard's HighwayHash and the corrupt shard is reported with drive `state:"missing"`; the heal then reconstructs and atomically reinstalls it (§4.6). Every surface below — the shard map, the before/during/after disk hashes, the GET reconstruction trace, and the heal — was observed at runtime through the real S3 and admin entry points; nothing here is `mc`-sourced (the `mc` client is not available in this environment) or reconstructed from reading alone.

### 4.2 Grounding (verbatim)

The sentinel and default algorithm:

```go
// cmd/storage-errors.go:104
var errFileCorrupt = StorageErr("file is corrupted")
```

```go
// cmd/bitrot.go:42 (default checksum algorithm)
	HighwayHash256:  "highwayhash256",
```

The streaming reader's per-block verification (verbatim, `cmd/bitrot-streaming.go:174-188`):

```go
	b.h.Reset()
	_, err = io.ReadFull(b.rc, b.hashBytes)
	if err != nil {
		return 0, err
	}
	_, err = io.ReadFull(b.rc, buf)
	if err != nil {
		return 0, err
	}
	b.h.Write(buf)
	if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {
		return 0, errFileCorrupt
	}
	b.currOffset += int64(len(buf))
	return len(buf), nil
```

The erasure decoder reacting to corruption (verbatim, `cmd/erasure-decode.go:192-229`):

```go
			n, err := rr.ReadAt(p.buf[bufIdx], p.offset)
			if err != nil {
				switch {
				case errors.Is(err, errFileNotFound):
					atomic.StoreInt32(&missingPartsHeal, 1)
				case errors.Is(err, errFileCorrupt):
					atomic.StoreInt32(&bitrotHeal, 1)
				case errors.Is(err, errDiskNotFound):
					atomic.AddInt32(&disksNotFound, 1)
				}

				// This will be communicated upstream.
				p.orgReaders[bufIdx] = nil
				if br, ok := p.readers[i].(io.Closer); ok {
					br.Close()
				}
				p.readers[i] = nil

				// Since ReadAt returned error, trigger another read.
				readTriggerCh <- true
				return
			}
			newBufLK.Lock()
			newBuf[bufIdx] = p.buf[bufIdx][:n]
			newBufLK.Unlock()
			// Since ReadAt returned success, there is no need to trigger another read.
			readTriggerCh <- false
		}(readerIndex)
		readerIndex++
	}
	wg.Wait()
	if p.canDecode(newBuf) {
		p.offset += p.shardSize
		if missingPartsHeal == 1 {
			return newBuf, errFileNotFound
		} else if bitrotHeal == 1 {
			return newBuf, errFileCorrupt
		}
```

The GET path that swallows `errFileCorrupt` and queues an async bit-rot heal (verbatim, `cmd/erasure-object.go:397-416`):

```go
			if written == partLength {
				if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {
					healOnce.Do(func() {
						globalMRFState.addPartialOp(PartialOperation{
							Bucket:     bucket,
							Object:     object,
							VersionID:  fi.VersionID,
							Queued:     time.Now(),
							SetIndex:   er.setIndex,
							PoolIndex:  er.poolIndex,
							BitrotScan: errors.Is(err, errFileCorrupt),
						})
					})
					// Healing is triggered and we have written
					// successfully the content to client for
					// the specific part, we should `nil` this error
					// and proceed forward, instead of throwing errors.
					err = nil
				}
			}
```

Storage-level verification is `ReadFile` (`cmd/xl-storage.go:1875`) and `bitrotVerify` (`cmd/xl-storage.go:3079`), the latter invoked by `VerifyFile` (`cmd/xl-storage.go:3097`) during a deep-scan heal.

### 4.3 Object layout and shard map (observed via the in-repo `xl-meta` debug tool)

A 5 MiB object `q3-bitrot/original.bin` was PUT via the real S3 path (minio-go `FPutObject`). Because the server runs with `MINIO_KMS_AUTO_ENCRYPTION=on`, the PUT was transparently encrypted (the HEAD response carries `X-Amz-Server-Side-Encryption: aws:kms`), so the on-disk shards are ciphertext — bit-rot protection sits *below* encryption and guards the ciphertext shards. The exact PUT and its response:

```
$ /tmp/minio-investigation/bin/q3 -op put
created bucket q3-bitrot
PUT ok: key=original.bin size=5242880 etag=be7921ff42acf745a7d7f0b5565ce72e versionId=
  [raw HEAD response] HEAD /q3-bitrot/original.bin -> 200 OK
    X-Amz-Server-Side-Encryption: aws:kms
    X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:my-minio-key
    Content-Length: 5242880
    ETag: "be7921ff42acf745a7d7f0b5565ce72e"
STAT: size=5242880 etag=be7921ff42acf745a7d7f0b5565ce72e versionId= sse="aws:kms" kmsKey="arn:aws:kms:my-minio-key"
```

The `xl-meta` debug tool (`docs/debugging/xl-meta`, a `package main` debug command in the main module) was built to `/tmp/minio-build/xl-meta` and used to decode the object's `xl.meta`. The erasure fields (verbatim, extracted from `Versions[0].Metadata.V2Obj`):

```
$ /tmp/minio-build/xl-meta /tmp/minio-data/disk1/q3-bitrot/original.bin/xl.meta
CSumAlgo   = 1              # HighwayHash256 (cmd/bitrot.go:42)
EcAlgo     = 1              # Reed-Solomon
EcBSize    = 1048576        # 1 MiB erasure block size
EcM        = 2              # data shards
EcN        = 2              # parity shards
EcDist     = [1, 2, 3, 4]   # shard-slot -> disk distribution
EcIndex    = 1              # this disk (disk1) holds shard index 1
PartASizes = [5242880]      # actual (plaintext) part size = 5 MiB
PartSizes  = [5245440]      # encrypted part size (+2560 B DARE envelope overhead)
Size       = 5245440
```

Reading each disk's `EcIndex` (shards with index 1..EcM are **data**, the rest **parity**) gives the exact placement:

```
disk1: EcIndex=1 -> DATA shard
disk2: EcIndex=2 -> DATA shard
disk3: EcIndex=3 -> PARITY shard
disk4: EcIndex=4 -> PARITY shard
```

All four `part.1` shard files are 2622912 B (Reed-Solomon data and parity shards are equal size). So `disk1` holds a **data** shard; the corruption (below) is written into disk1's shard-data region so that detection must fire when a plain GET reads it.

### 4.4 Observed — state timeline (before / during / after)

```
Q3 STATE TIMELINE (object q3-bitrot/original.bin, 5 MiB, EC: 2 data + 2 parity, disk1 = data shard)
object sha256 (plaintext, uploaded & every download) = 25b74b789ca5f2f2e61ed850282fc6981d7dd328fd79ecf47c20be752ef47092

BEFORE (healthy):     disk1 part.1 sha256 = 8836d76b86b790e95866bb211a6cede79996f0c2a049086babf865cb31524c08
                      healthy GET reads 2 shards (disk1 + disk2, both DATA); no parity read
DURING (corrupted):   disk1 part.1 sha256 = 113578b738eb93d897ac59f0ac8cdfa731c817a68ec2645bf8db1592d0080c8c
                      (64 bytes 0xFF written at file offset 100 == inside block-1 shard-data region)
GET (while corrupt):  200 OK; downloaded sha256 = 25b74b789ca5f2f2e61ed850282fc6981d7dd328fd79ecf47c20be752ef47092
                      (MATCHES original — reconstructed); GET reads 3 shards (disk1 + disk2 DATA + disk3 PARITY)
Deep-heal dry-run:    object type=object data=2 parity=2 disks=4; disk1 state="missing", disk2/3/4 state="ok"
                      (dry-run does NOT repair: disk1 part.1 sha256 still 113578b738eb93d897ac59f0ac8cdfa731c817a68ec2645bf8db1592d0080c8c)
Deep-heal real:       BEFORE disk1 state="missing" -> AFTER disk1 state="ok"; heal.Object 14.09ms, 5242880 B
AFTER (healed):       disk1 part.1 sha256 = 8836d76b86b790e95866bb211a6cede79996f0c2a049086babf865cb31524c08 (RESTORED)
                      post-heal GET reads 2 shards again (disk1 + disk2); correct bytes
```

The corrupted shard was flipped directly on disk (unauthorized manual corruption) with `dd`/`printf` writing 64 bytes of `0xFF` at offset 100 (inside the block-1 shard-data region, past the 32-byte HighwayHash header) under `/tmp/minio-data/disk1/q3-bitrot/original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1`. The plaintext object sha256 was identical on upload and on every download (healthy, corrupt-and-reconstructed, and post-heal), proving the client always received correct bytes.

### 4.5 Observed — GET reconstruction (the runtime detection signature)

Trace capture uses the in-process `tracecap` tool — a `madmin.ServiceTrace` subscriber on the real admin trace endpoint `/minio/admin/v3/trace`, with all trace types enabled (S3, internal, storage, OS) and `Threshold: 0` so even sub-millisecond storage calls surface. It is the same endpoint `mc admin trace` uses; `mc` itself is not available in this environment.

**Baseline — healthy GET.** Before corrupting anything, a GET of the object read exactly the two **data** shards and no parity (command + result):

```
$ setsid tracecap -endpoint 127.0.0.1:9000 -all -v -path q3-bitrot -out trace_healthy_get.log &
$ /tmp/minio-investigation/bin/q3 -op get -dst /tmp/minio-investigation/q3/downloaded_healthy.bin
GET ok: wrote /tmp/minio-investigation/q3/downloaded_healthy.bin size=5242880 sha256=25b74b789ca5f2f2e61ed850282fc6981d7dd328fd79ecf47c20be752ef47092

# storage.ReadFileStream disks in the healthy-GET trace (grep | uniq -c):
      1 disk1
      1 disk2
# ReadFileStream count = 2   (only the two DATA shards; no parity)
```

**Corrupt GET.** After flipping disk1's data shard on disk (§4.4), the *same* GET read **three** shards — the two data shards *plus* the disk3 parity shard — which is the runtime signature of bit-rot detection + reconstruction. The `storage.ReadFileStream` lines and the full `s3.GetObject` REQUEST/RESPONSE from `trace_corrupt_get.log` (verbatim, unedited):

```
2026-07-08T06:50:39.929997Z storage.ReadFileStream /tmp/minio-data/disk2 q3-bitrot original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1 39.005µs 2622912 B
2026-07-08T06:50:39.930001Z storage.ReadFileStream /tmp/minio-data/disk1 q3-bitrot original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1 36.041µs 2622912 B
2026-07-08T06:50:39.930679Z storage.ReadFileStream /tmp/minio-data/disk3 q3-bitrot original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1 36.051µs 2622912 B

2026-07-08T06:50:39.929385Z [REQUEST s3.GetObject] [Client: 127.0.0.1]
2026-07-08T06:50:39.929385Z GET /q3-bitrot/original.bin
2026-07-08T06:50:39.929385Z Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=397478280d572fda58e71db73c5d79f3fc188e93b09dc1cbd592f8e107e9b9be
2026-07-08T06:50:39.929385Z Host: 127.0.0.1:9000
2026-07-08T06:50:39.929385Z User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
2026-07-08T06:50:39.929385Z X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
2026-07-08T06:50:39.929385Z X-Amz-Date: 20260708T065039Z
2026-07-08T06:50:39.929385Z [RESPONSE] [ Duration 6.463219ms  ↑ 93 B  ↓ 5242880 B ]
2026-07-08T06:50:39.929385Z 200 OK
2026-07-08T06:50:39.929385Z Content-Length: 5242880
2026-07-08T06:50:39.929385Z Content-Type: application/octet-stream
2026-07-08T06:50:39.929385Z ETag: "be7921ff42acf745a7d7f0b5565ce72e"
```

The two data shards were opened first (disk2 + disk1 at `06:50:39.929997`/`.930001`); disk1's block-1 HighwayHash mismatch produced `errFileCorrupt` (`cmd/bitrot-streaming.go:185`) while `erasure.Decode` consumed the stream; the decoder nil'd disk1 and pulled the parity shard from disk3 (`cmd/erasure-decode.go:197,228`), which opened `~678 µs` later at `.930679`; `s3.GetObject` returned `200 OK` in `6.46 ms` with `↓ 5242880 B`, and the downloaded object's sha256 (`25b74b78...`) **matched** the original.

**F6 — the GET-time "file is corrupted" log, stated honestly.** A plain GET emits **no** "file is corrupted" line — this was verified directly: `grep -in "corrupt\|ERROR" trace_corrupt_get.log` returned nothing but `tracecap`'s own shutdown message (`# trace stream error: context canceled`), and `grep -in "file is corrupted\|bitrot" server.stdout.log` returned nothing (the server stdout is only its 10-line startup banner). This is *observed*, and the *reason* is grounded in code, not inferred loosely:

- The `storage.ReadFileStream` trace is published by `defer done(length, &err)` in the disk wrapper (`cmd/xl-storage-disk-id-check.go:452`) when the stream is **opened**. Opening a corrupt-but-present file succeeds, so `err` is nil and the three `ReadFileStream` lines above all show a full `2622912 B` read with **no** `Error` field.
- `errFileCorrupt` only arises later, while `erasure.Decode` reads the returned `io.ReadCloser` block-by-block (`cmd/bitrot-streaming.go:185`) — after that storage trace has already published.
- On the GET path the sentinel is then swallowed at `cmd/erasure-object.go:414` (`err = nil`) after queuing an async MRF heal (`BitrotScan: true`, `cmd/mrf.go` `healRoutine` sets `HealDeepScan` when `BitrotScan` is true). There is therefore **no** GET-path code point that logs the sentinel.

So the GET-time detection is *not* a log line; it is the **extra parity read** (3 shards vs 2) plus the fact that correct bytes are returned. The explicit, log-bearing detection lives on the deep-scan heal path (§4.6). This is reported exactly as observed: the plain-GET "corruption log" the question asks for is **not emitted**, and the section documents why and shows the detection signal that *is* present.

### 4.6 Observed — explicit detection + heal via the admin heal API (deep scan)

The canonical operational heal path surfaces the detection explicitly. It was driven through the real admin heal endpoint with `madmin.Heal(..., HealOpts{ScanMode: HealDeepScan}, ...)` — the exact API `mc admin heal --scan deep` wraps (the trace below shows the request landing on `POST /minio/admin/v3/heal/q3-bitrot/original.bin`).

**Dry-run (detect-only)** reported the object with `data=2 parity=2 disks=4` and classified the corrupt disk1 shard with drive `state:"missing"`, the other three `"ok"` — and, being a dry run, left disk1 corrupt on disk (a bit-rot-failed shard is reported as **"missing"**, not a distinct "corrupt" state). Verbatim driver output:

```
$ /tmp/minio-investigation/bin/q3heal -dry
=== madmin.Heal DRY-RUN (ScanMode=HealDeepScan) bucket=q3-bitrot object=original.bin ===
heal started: clientToken="19d9a56e-ac54-4e7f-b5b0-51d3c5f58dce"
heal summary="finished" unique-items=2
[DRY-RUN] type=object bucket=q3-bitrot object="original.bin" size=5242880 data=2 parity=2 disks=4 detail=""
    BEFORE drives:
      endpoint=/tmp/minio-data/disk1 state="missing" uuid=
      endpoint=/tmp/minio-data/disk2 state="ok" uuid=
      endpoint=/tmp/minio-data/disk3 state="ok" uuid=
      endpoint=/tmp/minio-data/disk4 state="ok" uuid=
    AFTER drives:
      endpoint=/tmp/minio-data/disk1 state="missing" uuid=
      endpoint=/tmp/minio-data/disk2 state="ok" uuid=
      endpoint=/tmp/minio-data/disk3 state="ok" uuid=
      endpoint=/tmp/minio-data/disk4 state="ok" uuid=
# disk1 part.1 sha256 after dry-run: 113578b738eb93d897ac59f0ac8cdfa731c817a68ec2645bf8db1592d0080c8c (still corrupt — dry-run does not repair)
```

**Real heal** transitioned disk1 `missing -> ok` and restored the shard:

```
$ /tmp/minio-investigation/bin/q3heal
=== madmin.Heal REAL (ScanMode=HealDeepScan) bucket=q3-bitrot object=original.bin ===
heal started: clientToken="34e8584d-c81f-4335-8273-55413ff63d44"
heal summary="finished" unique-items=2
[REAL] type=object bucket=q3-bitrot object="original.bin" size=5242880 data=2 parity=2 disks=4 detail=""
    BEFORE drives:
      endpoint=/tmp/minio-data/disk1 state="missing" uuid=
      endpoint=/tmp/minio-data/disk2 state="ok" uuid=
      endpoint=/tmp/minio-data/disk3 state="ok" uuid=
      endpoint=/tmp/minio-data/disk4 state="ok" uuid=
    AFTER drives:
      endpoint=/tmp/minio-data/disk1 state="ok" uuid=
      endpoint=/tmp/minio-data/disk2 state="ok" uuid=
      endpoint=/tmp/minio-data/disk3 state="ok" uuid=
      endpoint=/tmp/minio-data/disk4 state="ok" uuid=
# disk1 part.1 sha256 after heal: 8836d76b86b790e95866bb211a6cede79996f0c2a049086babf865cb31524c08 (RESTORED — matches the before-corruption value)
```

The heal trace (`trace_heal_real.log`, verbatim, condensed to the storage ops) shows the admin call, then `VerifyFile` across all four shards, then reconstruction reads from parity, then the atomic reinstall:

```
2026-07-08T06:53:11.923750Z [REQUEST admin.Heal] [Client: 127.0.0.1]
2026-07-08T06:53:11.923750Z POST /minio/admin/v3/heal/q3-bitrot/original.bin?forceStart=true
2026-07-08T06:53:11.925343Z storage.VerifyFile /tmp/minio-data/disk1 q3-bitrot original.bin 188.147µs
2026-07-08T06:53:11.925543Z storage.VerifyFile /tmp/minio-data/disk2 q3-bitrot original.bin 785.898µs
2026-07-08T06:53:11.926339Z storage.VerifyFile /tmp/minio-data/disk3 q3-bitrot original.bin 779.461µs
2026-07-08T06:53:11.927130Z storage.VerifyFile /tmp/minio-data/disk4 q3-bitrot original.bin 793.695µs
2026-07-08T06:53:11.928047Z storage.ReadFileStream /tmp/minio-data/disk3 q3-bitrot original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1 32.599µs 2622912 B
2026-07-08T06:53:11.928124Z storage.ReadFileStream /tmp/minio-data/disk2 q3-bitrot original.bin/af487cc6-358a-4a1b-9c86-563edfc4fb27/part.1 40.606µs 2622912 B
2026-07-08T06:53:11.937648Z storage.RenameData /tmp/minio-data/disk1 bbb8f298-2dee-42d1-a8f3-4687b94c5ccc af487cc6-358a-4a1b-9c86-563edfc4fb27 q3-bitrot original.bin 1.488767ms
2026-07-08T06:53:11.925147Z heal.Object q3-bitrot/original.bin 14.094414ms 5242880 B
```

Two observable details confirm the detection: (1) disk1's `VerifyFile` returns in **188 µs** — far faster than the ~780 µs the three healthy shards each take — because `bitrotVerify` bails at the first block whose HighwayHash fails; (2) the repair reads the surviving data shard (disk2) plus the parity shard (disk3) to reconstruct disk1, then `storage.RenameData` atomically swaps in the rebuilt shard (the detailed `os.Rename` sub-steps move the old corrupt data-dir to `.trash` and the freshly reconstructed data-dir + `xl.meta` from `.minio.sys/tmp/bbb8f298-.../` into place). `VerifyFile` (`cmd/xl-storage.go:3097`) loops the parts calling `s.bitrotVerify` (`cmd/xl-storage.go:3079`), which does block-by-block HighwayHash verification and returns `errFileCorrupt` on mismatch. After heal, disk1's `part.1` sha256 was **restored** to the original `8836d76b...`, and a subsequent healthy GET again read only the two data shards.

### 4.7 Cause→effect and prerequisite

HighwayHash256 checksum mismatch on the corrupted data shard → `errFileCorrupt` from the streaming reader → decoder flags heal and reconstructs from the surviving data shard + parity → the client still receives the correct bytes. This behavior only manifests on a **multi-drive erasure** backend (here EC:2 across four drives); a single-drive/FS deployment would surface a plain read error and could not reconstruct — hence the four-drive prerequisite. Reference: `buildscripts/verify-healing.sh` (a distributed 6-drive heal-verification flow; our single-node 4-drive deployment exercises the same bitrot/heal code path). Corroborated by `docs/erasure/README.md` (Reed-Solomon + HighwayHash bit-rot protection).


---

## 5. Q4 — STS Session-Policy Enforcement (intersection semantics)

**Question.** Verify with runtime test output that when a user obtains temporary credentials, MinIO enforces the *session policy* attached to those credentials (effective permissions are constrained by the session policy).

### 5.1 Direct answer

Effective permissions of STS temporary credentials are the **intersection** of the parent (canned) policy and the inline session policy, and can never exceed the parent. An action allowed by the parent but *absent from* (i.e. denied by) the session policy is refused with `AccessDenied`; an action allowed by both succeeds. This was proven in both directions by a **genuine `go test`** (out-of-repo, driving the real AssumeRole entry point with an inline session policy) whose full `go test -v` output is embedded in §5.4; the parent-policy enforcement layer is additionally corroborated by MinIO's own in-repo STS integration suite (§5.3).

### 5.2 Grounding (verbatim)

The session policy is parsed at AssumeRole and embedded in the token claim. `maxSTSSessionPolicySize = 2048` (`cmd/sts-handlers.go:89`); the handler is `AssumeRole` (`cmd/sts-handlers.go:256`). Verbatim (`cmd/sts-handlers.go:123-128`):

```go
	if len(policyBuf) > maxSTSSessionPolicySize {
		return errSessionPolicyTooLarge
	}

	c[policy.SessionPolicyName] = base64.StdEncoding.EncodeToString(policyBuf)
	return nil
```

Enforcement is the intersection computed in `IsAllowedSTS` (`cmd/iam.go:2242`). Verbatim (`cmd/iam.go:2309-2313`):

```go
	// Now check if we have a sessionPolicy.
	hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
	if hasSessionPolicy {
		return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))
	}
```

`isAllowedBySessionPolicy` is defined at `cmd/iam.go:2381`. The conjunction `isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))` is exactly why effective permissions are the intersection: an action must be allowed by the session policy **and** by the parent (or owner). Corroborated verbatim by `docs/sts/assume-role.md:39`:

> "The resulting session's permissions are the intersection of the canned policy name and the policy set here. You cannot use this policy to grant more permissions than those allowed by the canned policy name being assumed."

### 5.3 Observed — surface 1: MinIO's STS integration suite (`go test`)

Command (invocation pattern from `Makefile:96`):

```
MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 go test -timeout 15m -tags kqueue,dev -v -run TestIAMInternalIDPSTSServerSuite ./cmd
```

Full result (`/tmp/minio-investigation/q4/gotest_inrepo_sts.txt`), captured by re-running the suite in this environment:

```
=== RUN   TestIAMInternalIDPSTSServerSuite
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_1,_ServerType:_ErasureSD
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_2,_ServerType:_ErasureSD_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_3,_ServerType:_ErasureSD
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_4,_ServerType:_ErasureSD_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_5,_ServerType:_Erasure
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_6,_ServerType:_Erasure_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_7,_ServerType:_ErasureSet
=== RUN   TestIAMInternalIDPSTSServerSuite/Test:_8,_ServerType:_ErasureSet_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
--- PASS: TestIAMInternalIDPSTSServerSuite (13.91s)
    --- PASS: TestIAMInternalIDPSTSServerSuite/Test:_1,_ServerType:_ErasureSD (3.32s)
    --- SKIP: TestIAMInternalIDPSTSServerSuite/Test:_2,_ServerType:_ErasureSD_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPSTSServerSuite/Test:_3,_ServerType:_ErasureSD (3.43s)
    --- SKIP: TestIAMInternalIDPSTSServerSuite/Test:_4,_ServerType:_ErasureSD_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPSTSServerSuite/Test:_5,_ServerType:_Erasure (3.55s)
    --- SKIP: TestIAMInternalIDPSTSServerSuite/Test:_6,_ServerType:_Erasure_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPSTSServerSuite/Test:_7,_ServerType:_ErasureSet (3.61s)
    --- SKIP: TestIAMInternalIDPSTSServerSuite/Test:_8,_ServerType:_ErasureSet_(with_etcd_backend) (0.00s)
PASS
ok  	github.com/minio/minio/cmd	14.200s
```

Tests 1/3/5/7 PASS across backends {ErasureSD, ErasureSD, Erasure, ErasureSet}; 2/4/6/8 SKIP with the logged reason `admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.` The suite entry is `TestIAMInternalIDPSTSServerSuite` (`cmd/sts-handlers_test.go:52`) → `runAllIAMSTSTests` (`cmd/sts-handlers_test.go:40`), which invokes `TestSTSForRoot`, `TestSTS` (`:393`), `TestSTSWithDenyDeleteVersion` (`:180`), `TestSTSWithTags` (`:278`), `TestSTSServiceAccountsWithUsername`, and `TestSTSWithGroupPolicy` (`:478`). `TestSTSWithDenyDeleteVersion` proves a **Deny** carried in the STS credentials' effective scope (`c.mustNotDelete`). These suite tests call `assumeRole.Retrieve()` against the **parent** policy and thus demonstrate parent-policy enforcement; they do **not** pass an inline session `Policy`. That gap — proving the true session∩parent intersection — is closed by the genuine `go test` in §5.4.

### 5.4 Observed — surface 2: genuine `go test` exercising an inline session policy

This is the primary proof. A standard Go test (`TestSTSSessionPolicyIntersection`) lives **outside** the repository at `/tmp/minio-investigation/tools/q4test/sessionpolicy_test.go` (package `q4test`, in a throwaway module — nothing is added to the MinIO tree). It drives the **real** `sts.AssumeRole` HTTP entry point on the running server (port 9000) via the minio-go STS provider `credentials.NewSTSAssumeRole(..., STSAssumeRoleOptions{Policy: <inline session policy>})`, then performs real S3 operations with the returned temporary credentials and asserts the outcome. The parent canned policy is `Allow s3:*` on `q4t-bucket`; the CASE A session policy is strictly narrower (`Allow s3:GetObject, s3:ListBucket` only).

Command and full unedited `go test -v` output (`/tmp/minio-investigation/q4/gotest_sessionpolicy.txt`):

```
$ go test -v -count=1 -run TestSTSSessionPolicyIntersection ./q4test
=== RUN   TestSTSSessionPolicyIntersection
    sessionpolicy_test.go:175: SETUP: parent canned policy "q4tparent" = Allow s3:* on q4t-bucket; user "q4tuser" attached; bucket seeded with hello.txt
    sessionpolicy_test.go:179: CASE A: AssumeRole WITH inline session policy -> AccessKeyID=JNZJQ2NX8V085956CO7T sessionTokenLen=544 claims=[accessKey exp parent sessionPolicy]
=== RUN   TestSTSSessionPolicyIntersection/A_ListObjects_allowedByParentAndSession_SUCCEEDS
=== RUN   TestSTSSessionPolicyIntersection/A_GetObject_allowedByParentAndSession_SUCCEEDS
=== RUN   TestSTSSessionPolicyIntersection/A_PutObject_allowedByParent_DENIEDbySession_ACCESSDENIED
=== RUN   TestSTSSessionPolicyIntersection/A_RemoveObject_allowedByParent_DENIEDbySession_ACCESSDENIED
=== NAME  TestSTSSessionPolicyIntersection
    sessionpolicy_test.go:208: CASE B (control): AssumeRole WITHOUT session policy -> AccessKeyID=NE1CWY2GMBKGF590FAWC sessionTokenLen=220 claims=[accessKey exp parent]
=== RUN   TestSTSSessionPolicyIntersection/B_PutObject_noSessionPolicy_inheritsParent_SUCCEEDS
=== RUN   TestSTSSessionPolicyIntersection/B_RemoveObject_noSessionPolicy_inheritsParent_SUCCEEDS
--- PASS: TestSTSSessionPolicyIntersection (0.17s)
    --- PASS: TestSTSSessionPolicyIntersection/A_ListObjects_allowedByParentAndSession_SUCCEEDS (0.00s)
    --- PASS: TestSTSSessionPolicyIntersection/A_GetObject_allowedByParentAndSession_SUCCEEDS (0.00s)
    --- PASS: TestSTSSessionPolicyIntersection/A_PutObject_allowedByParent_DENIEDbySession_ACCESSDENIED (0.00s)
    --- PASS: TestSTSSessionPolicyIntersection/A_RemoveObject_allowedByParent_DENIEDbySession_ACCESSDENIED (0.00s)
    --- PASS: TestSTSSessionPolicyIntersection/B_PutObject_noSessionPolicy_inheritsParent_SUCCEEDS (0.00s)
    --- PASS: TestSTSSessionPolicyIntersection/B_RemoveObject_noSessionPolicy_inheritsParent_SUCCEEDS (0.00s)
PASS
ok  	investigation/q4test	0.179s
```

**Both directions of the intersection are proven, and every subtest is a hard assertion:**

- **CASE A (session policy present** — the token `claims=[accessKey exp parent sessionPolicy]`): `A_ListObjects` and `A_GetObject` **succeed** (allowed by parent `s3:*` *and* by the read-only session policy); `A_PutObject` and `A_RemoveObject` return **`AccessDenied`** (allowed by the parent but *absent from* the session policy). The subtests assert `errCode(err) == "AccessDenied"`, so they only PASS if the server actually refuses.
- **CASE B (control, no session policy** — the token `claims=[accessKey exp parent]`, no `sessionPolicy`): the **same** parent user's `B_PutObject` and `B_RemoveObject` **succeed**, because with no session policy the effective scope is the full parent `s3:*`.

The only variable between CASE A and CASE B is the inline session policy, and it is exactly what flips `PutObject`/`RemoveObject` from success to `AccessDenied` — i.e. effective = session ∩ parent.

### 5.5 Observed — the session policy on the wire and in the token

The `sts.AssumeRole` requests were captured with the admin trace (`/tmp/minio-investigation/q4/trace_assumerole.log`). CASE A carried the url-encoded `Policy=` form field; CASE B did not (verbatim request bodies):

```
# CASE A request body:
Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3AGetObject%22%2C%22s3%3AListBucket%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Aq4t-bucket%22%2C%22arn%3Aaws%3As3%3A%3A%3Aq4t-bucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15

# CASE B request body (no Policy):
Action=AssumeRole&DurationSeconds=3600&Version=2011-06-15
```

Decoding the returned session-token JWT payloads (base64url of the middle segment) confirms the session policy is stored in the `sessionPolicy` claim (per `cmd/sts-handlers.go:127`), present in CASE A and absent in CASE B:

```
# CASE A token claim keys: ['accessKey', 'exp', 'parent', 'sessionPolicy']
#   accessKey=JNZJQ2NX8V085956CO7T  parent=q4tuser
#   base64-decode of the sessionPolicy claim:
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:GetObject", "s3:ListBucket"], "Resource": ["arn:aws:s3:::q4t-bucket", "arn:aws:s3:::q4t-bucket/*"]}]}

# CASE B token claim keys: ['accessKey', 'exp', 'parent']   (NO sessionPolicy)
#   accessKey=NE1CWY2GMBKGF590FAWC  parent=q4tuser
```

The decoded CASE A `sessionPolicy` is byte-for-byte the read-only policy the test sent, and its `accessKey` (`JNZJQ2NX8V085956CO7T`) matches the `AccessKeyID` the test logged for CASE A — confirming the same credential whose `PutObject`/`RemoveObject` were denied above.

### 5.6 Cause→effect and caveat

The session policy is embedded at AssumeRole (`cmd/sts-handlers.go:127`) and evaluated at `IsAllowedSTS`; the conjunction `isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))` (`cmd/iam.go:2312`) requires an action to be allowed by **both** the session policy and the parent, so `PutObject`/`DeleteObject` — allowed by the parent `s3:*` but not by the read-only session policy — yields `isAllowedSP = false` → denied. Effective permissions are the intersection and can never exceed the parent. **Caveat (relevant because the harness is shared):** the suite's `TestMain` (`cmd/test-utils_test.go`) sets `globalIsTesting`/`globalIsCICD = true` and **unsets** `crypto.EnvKMSAutoEncryption`; any auto-encryption behavior exercised inside a test must be set explicitly (this affects Q1-in-test, not Q4).


---

## 6. Q5 — Privilege-Escalation Prevention via User→Policy Mapping (+ root cause)

**Question.** Show test output proving a user with only basic access cannot promote themselves to console administrator by modifying user→policy mappings, and identify the **root cause** of the observed user-mapping modification behavior.

### 6.1 Direct answer

A basic-access user cannot self-promote. Every attempt to attach an admin/`consoleAdmin` policy to itself through the real admin APIs (`AttachDetachPolicyBuiltin`, `SetPolicyForUserOrGroup`) is refused with **`AccessDenied` / HTTP 403**. The identical call performed by root **succeeds** — proving the block is purely authorization, not the mapping-mutation logic.

**Root cause.** Self-promotion requires attaching an admin policy through admin APIs that demand `admin:AttachUserOrGroupPolicy` and/or `admin:UpdatePolicyAssociation`. The basic user's policy grants **none** of those admin actions, so under MinIO's **deny-by-default** model the admin-action authorization guard `validateAdminReq` denies the request and writes `ErrAccessDenied` **before** any mapping-mutation code runs.

### 6.2 Grounding (verbatim)

The two user→policy mapping mutators and their guards:

```go
// cmd/admin-handlers-users.go:1770-1773
func (a adminAPIHandlers) SetPolicyForUserOrGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	objectAPI, _ := validateAdminReq(ctx, w, r, policy.AttachPolicyAdminAction)
```

```go
// cmd/admin-handlers-users.go:1908-1912
func (a adminAPIHandlers) AttachDetachPolicyBuiltin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	objectAPI, cred := validateAdminReq(ctx, w, r, policy.UpdatePolicyAssociationAction,
		policy.AttachPolicyAdminAction)
```

The guard itself (verbatim, `cmd/admin-handler-utils.go:45-60`):

```go
	for _, action := range actions {
		// Validate request signature.
		cred, adminAPIErr := checkAdminRequestAuth(ctx, r, action, "")
		switch adminAPIErr {
		case ErrNone:
			return objectAPI, cred
		case ErrAccessDenied:
			// Try another
			continue
		default:
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(adminAPIErr), r.URL)
			return nil, cred
		}
	}
	writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
	return nil, auth.Credentials{}
```

`validateAdminReq` (`cmd/admin-handler-utils.go:37`) iterates the required actions calling `checkAdminRequestAuth` (`cmd/auth-handler.go:189`) → `globalIAMSys.IsAllowed()`. Any single action returning `ErrNone` admits the request; if **every** required action is denied, the loop exhausts and it writes `ErrAccessDenied` (`cmd/admin-handler-utils.go:59-60`).

The required admin-action strings (exact, from the module cache `github.com/minio/pkg/v3@v3.0.22/policy/admin-action.go`):

```go
// :142
CreatePolicyAdminAction = "admin:CreatePolicy"
// :148
AttachPolicyAdminAction = "admin:AttachUserOrGroupPolicy"
// :151
UpdatePolicyAssociationAction = "admin:UpdatePolicyAssociation"
// :208
AllAdminActions = "admin:*"
```

The traced admin endpoints map to these handlers via `cmd/admin-router.go`: `/idp/builtin/policy/{operation}` → `AttachDetachPolicyBuiltin` (`:268`); `/set-user-or-group-policy` → `SetPolicyForUserOrGroup` (`:263-264`); `/list-canned-policies` → `ListCannedPolicies` (`:254`).

### 6.3 Observed — surface 1: admin integration suite (`go test`)

```
MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 go test -timeout 4m -tags kqueue,dev -v -run TestIAMInternalIDPServerSuite ./cmd
```

Full unedited result (`/tmp/minio-investigation/q5/gotest_inrepo_admin.txt`):

```
=== RUN   TestIAMInternalIDPServerSuite
=== RUN   TestIAMInternalIDPServerSuite/Test:_1,_ServerType:_ErasureSD
=== RUN   TestIAMInternalIDPServerSuite/Test:_2,_ServerType:_ErasureSD_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPServerSuite/Test:_3,_ServerType:_ErasureSD
=== RUN   TestIAMInternalIDPServerSuite/Test:_4,_ServerType:_ErasureSD_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPServerSuite/Test:_5,_ServerType:_Erasure
=== RUN   TestIAMInternalIDPServerSuite/Test:_6,_ServerType:_Erasure_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
=== RUN   TestIAMInternalIDPServerSuite/Test:_7,_ServerType:_ErasureSet
=== RUN   TestIAMInternalIDPServerSuite/Test:_8,_ServerType:_ErasureSet_(with_etcd_backend)
    admin-handlers-users_test.go:150: Skipping etcd backend IAM test as no etcd server is configured.
--- PASS: TestIAMInternalIDPServerSuite (14.05s)
    --- PASS: TestIAMInternalIDPServerSuite/Test:_1,_ServerType:_ErasureSD (3.50s)
    --- SKIP: TestIAMInternalIDPServerSuite/Test:_2,_ServerType:_ErasureSD_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPServerSuite/Test:_3,_ServerType:_ErasureSD (3.18s)
    --- SKIP: TestIAMInternalIDPServerSuite/Test:_4,_ServerType:_ErasureSD_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPServerSuite/Test:_5,_ServerType:_Erasure (3.74s)
    --- SKIP: TestIAMInternalIDPServerSuite/Test:_6,_ServerType:_Erasure_(with_etcd_backend) (0.00s)
    --- PASS: TestIAMInternalIDPServerSuite/Test:_7,_ServerType:_ErasureSet (3.63s)
    --- SKIP: TestIAMInternalIDPServerSuite/Test:_8,_ServerType:_ErasureSet_(with_etcd_backend) (0.00s)
PASS
ok  	github.com/minio/minio/cmd	14.337s
```

Suite entry `TestIAMInternalIDPServerSuite` (`cmd/admin-handlers-users_test.go:192`); the suite's `AccessDenied` assertion helper is `mustNotUpload` (`cmd/admin-handlers-users_test.go:1573`, check `if e.Code == "AccessDenied"` at `:1577`). Race-mode variant reference: `cmd/admin-handlers-users-race_test.go`. This suite proves the admin user/policy-management APIs work end-to-end, but it does **not** itself drive a *basic-user self-attach*; that specific gap is closed by the genuine out-of-repo `go test` in §6.4.

### 6.4 Observed — surface 2: genuine out-of-repo `go test` driving the real admin APIs (primary proof)

The direct proof for Q5 is a genuine `go test` that lives **outside** the repository (`/tmp/minio-investigation/tools/q5test/privesc_test.go`, module `investigation`, package `q5test`) and drives the **real** admin user→policy mapping endpoints on the live server through the canonical `github.com/minio/madmin-go/v3` SDK. It (1) creates a basic-access user whose policy grants only `s3:GetObject`/`s3:ListBucket` and **no** admin action, (2) has that user's *own* `madmin` client attempt to attach `consoleAdmin` (the built-in console-administrator policy) to itself via **both** mutators — `AttachDetachPolicyBuiltin` and `SetPolicyForUserOrGroup` — and (3) has root perform the identical mutations as a control. A capturing `http.RoundTripper` installed on the basic user's client (`madmin.SetCustomTransport`, `api.go:179`) logs the **full, unedited** server response body straight into the test output.

Build/run (server already running from §1.4; the harness is out-of-repo so the repository stays unchanged):

```
cd /tmp/minio-investigation/tools
export GOPROXY=off GOFLAGS=-mod=mod GOSUMDB=off
go test -v -count=1 ./q5test
```

Full unedited `go test -v` output (`/tmp/minio-investigation/q5/gotest_q5.txt`):

```
=== RUN   TestPrivilegeEscalationPrevention
    privesc_test.go:122: SETUP: basic user "q5basic" attached to canned policy "q5basiconly" (Allow s3:GetObject/s3:ListBucket only; NO admin actions); bucket "q5-bucket" seeded with hello.txt
=== RUN   TestPrivilegeEscalationPrevention/Sanity_basicUser_allowedS3Read_SUCCEEDS
    privesc_test.go:143: basic user listed its bucket successfully (creds valid; s3 read allowed)
=== RUN   TestPrivilegeEscalationPrevention/Escalation_A_AttachPolicyBuiltin_selfAttachConsoleAdmin_ACCESSDENIED
    privesc_test.go:58: [basic AttachDetachPolicyBuiltin: AttachPolicy(consoleAdmin -> self)]
                    POST /minio/admin/v3/idp/builtin/policy/attach -> 403 Forbidden
                    RESP BODY: {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C03F32C152E3CA","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
    privesc_test.go:153: DENIED as expected: AttachPolicy(consoleAdmin->self) code=AccessDenied err=Access Denied.
=== RUN   TestPrivilegeEscalationPrevention/Escalation_B_SetPolicyForUserOrGroup_selfConsoleAdmin_ACCESSDENIED
    privesc_test.go:58: [basic SetPolicyForUserOrGroup: SetPolicy(consoleAdmin, self)]
                    PUT /minio/admin/v3/set-user-or-group-policy -> 403 Forbidden
                    RESP BODY: {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C03F32C15F254D","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
    privesc_test.go:163: DENIED as expected: SetPolicy(consoleAdmin,self) code=AccessDenied err=Access Denied.
=== RUN   TestPrivilegeEscalationPrevention/Control_ROOT_AttachDetachPolicyBuiltin_consoleAdmin_SUCCEEDS_thenRevert
    privesc_test.go:173: ROOT AttachPolicy(consoleAdmin->q5basic) SUCCESS (mutation logic works for authorized caller)
    privesc_test.go:177: ROOT DetachPolicy(consoleAdmin->q5basic) SUCCESS (reverted)
=== RUN   TestPrivilegeEscalationPrevention/Control_ROOT_SetPolicyForUserOrGroup_consoleAdmin_SUCCEEDS_thenRevert
    privesc_test.go:184: ROOT SetPolicy(consoleAdmin,q5basic) SUCCESS
    privesc_test.go:188: ROOT SetPolicy(q5basiconly,q5basic) SUCCESS (reverted to basic-only)
--- PASS: TestPrivilegeEscalationPrevention (0.47s)
    --- PASS: TestPrivilegeEscalationPrevention/Sanity_basicUser_allowedS3Read_SUCCEEDS (0.00s)
    --- PASS: TestPrivilegeEscalationPrevention/Escalation_A_AttachPolicyBuiltin_selfAttachConsoleAdmin_ACCESSDENIED (0.05s)
    --- PASS: TestPrivilegeEscalationPrevention/Escalation_B_SetPolicyForUserOrGroup_selfConsoleAdmin_ACCESSDENIED (0.00s)
    --- PASS: TestPrivilegeEscalationPrevention/Control_ROOT_AttachDetachPolicyBuiltin_consoleAdmin_SUCCEEDS_thenRevert (0.29s)
    --- PASS: TestPrivilegeEscalationPrevention/Control_ROOT_SetPolicyForUserOrGroup_consoleAdmin_SUCCEEDS_thenRevert (0.01s)
PASS
ok  	investigation/q5test	0.487s
```

**Reading the result.** The basic user's `AttachDetachPolicyBuiltin` self-attach (`POST /minio/admin/v3/idp/builtin/policy/attach`) and `SetPolicyForUserOrGroup` self-set (`PUT /minio/admin/v3/set-user-or-group-policy`) are **both** refused with the full `AccessDenied` JSON body shown above (`Code`, `Message`, `Resource`, `RequestId`, `HostId`) and **HTTP 403**. The `Sanity` subtest first confirms the very same credentials succeed at an *allowed* S3 read — so the denials are **authorization** (deny-by-default), not a bad signature or invalid credential. Root then performs the **identical** two mutations successfully and reverts, proving the mapping-mutation logic works and the sole barrier is authorization.

**Server-side trace of the same run (corroboration).** While the `go test` ran, the admin trace stream (`madmin ServiceTrace`, all types) captured the two denials server-side (`/tmp/minio-investigation/q5/trace_q5.log`). The `Authorization` header proves the caller is `q5basic`; the `SetPolicyForUserOrGroup` query string proves the self-target (`policyName=consoleAdmin&userOrGroup=q5basic`); and the `X-Amz-Request-Id` values (`18C03F32C152E3CA`, `18C03F32C15F254D`) match the client-side capture above — same requests, same run.

```
2026-07-08T07:15:39.663073Z [REQUEST admin.AttachDetachPolicyBuiltin] [Client: 127.0.0.1]
2026-07-08T07:15:39.663073Z POST /minio/admin/v3/idp/builtin/policy/attach
2026-07-08T07:15:39.663073Z Accept-Encoding: gzip
2026-07-08T07:15:39.663073Z Authorization: AWS4-HMAC-SHA256 Credential=q5basic/20260708//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=0a69d6bcc816707fca40e2f5de90704c69d660e1e2ebe274aa0d331b9f943f97
2026-07-08T07:15:39.663073Z Content-Length: 103
2026-07-08T07:15:39.663073Z Content-Type: application/octet-stream
2026-07-08T07:15:39.663073Z Host: 127.0.0.1:9000
2026-07-08T07:15:39.663073Z User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
2026-07-08T07:15:39.663073Z X-Amz-Content-Sha256: fb62e0f85e24425652dcc8df1777b550b9fc354e433b0755111212695c46385d
2026-07-08T07:15:39.663073Z X-Amz-Date: 20260708T071539Z
2026-07-08T07:15:39.663073Z [request body 6 bytes]
<BLOB>
2026-07-08T07:15:39.663073Z [RESPONSE] [ Duration 249.446µs  ↑ 106 B  ↓ 213 B ]
2026-07-08T07:15:39.663073Z 403 Forbidden
2026-07-08T07:15:39.663073Z Accept-Ranges: bytes
2026-07-08T07:15:39.663073Z Content-Length: 213
2026-07-08T07:15:39.663073Z Content-Type: application/json
2026-07-08T07:15:39.663073Z Server: MinIO
2026-07-08T07:15:39.663073Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T07:15:39.663073Z Vary: Origin
2026-07-08T07:15:39.663073Z Vary: Accept-Encoding
2026-07-08T07:15:39.663073Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T07:15:39.663073Z X-Amz-Request-Id: 18C03F32C152E3CA
2026-07-08T07:15:39.663073Z X-Content-Type-Options: nosniff
2026-07-08T07:15:39.663073Z X-Xss-Protection: 1; mode=block
{"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C03F32C152E3CA","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

```
2026-07-08T07:15:39.663873Z [REQUEST admin.SetPolicyForUserOrGroup] [Client: 127.0.0.1]
2026-07-08T07:15:39.663873Z PUT /minio/admin/v3/set-user-or-group-policy?isGroup=false&policyName=consoleAdmin&userOrGroup=q5basic
2026-07-08T07:15:39.663873Z Accept-Encoding: gzip
2026-07-08T07:15:39.663873Z Authorization: AWS4-HMAC-SHA256 Credential=q5basic/20260708//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=4a9d496d103a3da3e4b9eb6998829887238338fb2095beaf4bbc6d6cea29e252
2026-07-08T07:15:39.663873Z Host: 127.0.0.1:9000
2026-07-08T07:15:39.663873Z Transfer-Encoding: chunked
2026-07-08T07:15:39.663873Z User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
2026-07-08T07:15:39.663873Z X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
2026-07-08T07:15:39.663873Z X-Amz-Date: 20260708T071539Z
2026-07-08T07:15:39.663873Z [request body 6 bytes]
<BLOB>
2026-07-08T07:15:39.663873Z [RESPONSE] [ Duration 121.661µs  ↑ 96 B  ↓ 212 B ]
2026-07-08T07:15:39.663873Z 403 Forbidden
2026-07-08T07:15:39.663873Z Accept-Ranges: bytes
2026-07-08T07:15:39.663873Z Content-Length: 212
2026-07-08T07:15:39.663873Z Content-Type: application/json
2026-07-08T07:15:39.663873Z Server: MinIO
2026-07-08T07:15:39.663873Z Strict-Transport-Security: max-age=31536000; includeSubDomains
2026-07-08T07:15:39.663873Z Vary: Origin
2026-07-08T07:15:39.663873Z Vary: Accept-Encoding
2026-07-08T07:15:39.663873Z X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
2026-07-08T07:15:39.663873Z X-Amz-Request-Id: 18C03F32C15F254D
2026-07-08T07:15:39.663873Z X-Content-Type-Options: nosniff
2026-07-08T07:15:39.663873Z X-Xss-Protection: 1; mode=block
{"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C03F32C15F254D","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

**Root control (contrast).** In the same trace, root's identical mutations return HTTP 200 (excerpt — headers elided for brevity; the full blocks are in `trace_q5.log`). The only difference from the denied requests is the `Credential=minioadmin` identity:

```
2026-07-08T07:15:39.684439Z [REQUEST admin.AttachDetachPolicyBuiltin] [Client: 127.0.0.1]
2026-07-08T07:15:39.684439Z POST /minio/admin/v3/idp/builtin/policy/attach
2026-07-08T07:15:39.684439Z Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260708//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=853fa2d2847776e44cc27dee0f1560e7bdc9e0e797736db7ea21893089767081
2026-07-08T07:15:39.684439Z [RESPONSE] [ Duration 95.298122ms  ↑ 193 B  ↓ 139 B ]
2026-07-08T07:15:39.684439Z 200 OK

2026-07-08T07:15:39.951773Z [REQUEST admin.SetPolicyForUserOrGroup] [Client: 127.0.0.1]
2026-07-08T07:15:39.951773Z PUT /minio/admin/v3/set-user-or-group-policy?isGroup=false&policyName=consoleAdmin&userOrGroup=q5basic
2026-07-08T07:15:39.951773Z [RESPONSE] [ Duration 8.731127ms  ↑ 80 B  ↓ 0 B ]
2026-07-08T07:15:39.951773Z 200 OK
```

**No mutation occurs on the denied path.** The IAM policy-DB object for the user (`.minio.sys/config/iam/policydb/users/q5basic.json`) is written (the `os.Mkdir`/`os.Rename` storage traces) only during setup and the root control — **never** between the two denied requests. This confirms the 403 is returned at the authorization guard **before** any mapping-mutation code runs, which is exactly the root cause detailed in §6.5.

### 6.5 Root-cause trace (cause→effect)

1. The basic user's policy `q5basiconly` grants only `s3:GetObject`/`s3:ListBucket` — **no** admin actions.
2. `AttachDetachPolicyBuiltin` requires `admin:UpdatePolicyAssociation` **or** `admin:AttachUserOrGroupPolicy`; `SetPolicyForUserOrGroup` requires `admin:AttachUserOrGroupPolicy` (`cmd/admin-handlers-users.go:1773`, `:1911`).
3. `validateAdminReq` (`cmd/admin-handler-utils.go:37`) loops each required action → `checkAdminRequestAuth` (`cmd/auth-handler.go:189`) → `globalIAMSys.IsAllowed`. For the basic user, **every** required admin action returns `ErrAccessDenied` (deny-by-default; the policy simply does not grant them).
4. With no action admitted, the loop exhausts and `validateAdminReq` writes `ErrAccessDenied` (`cmd/admin-handler-utils.go:59-60`), returning HTTP 403 **before** the `SetPolicyForUserOrGroup`/`AttachDetachPolicyBuiltin` bodies execute.

The user-mapping modification is therefore refused at the **admin-action authorization guard**, not anywhere in the mapping-mutation logic — which is the root cause of the observed behavior. Existing demonstrating tests: `cmd/admin-handlers-users_test.go` and `cmd/admin-handlers-users-race_test.go`.


---

## 7. Closing Coverage Pass

Each distinct named item from the five questions, with its concrete value, `file:line`, evidence surface, sibling variants, and label (Observed-at-runtime = O, Inferred-from-reading = I).

### Q1 — encryption vs policy precedence

| Named item | Value / behavior | file:line | Evidence | Label |
|---|---|---|---|---|
| `isPutActionAllowed` (PUT authorization gate) | admits or denies before the handler body runs | `cmd/object-handlers.go:1836` → def `cmd/auth-handler.go:749` | trace 403 in (b) | O |
| `IAMSys.IsAllowed` | evaluates identity policy (Deny-over-Allow) | `cmd/iam.go:2437` | (b) 403 in 196µs | O |
| `globalBucketSSEConfigSys.Get` + `sseConfig.Apply` | applies encryption in handler | `cmd/object-handlers.go:1894-1897` | (a) SSE headers on response | O |
| copy-path Apply | same application | `cmd/object-handlers.go:1231-1232` | grounded | I |
| multipart Apply | same application | `cmd/object-multipart-handlers.go:89-90` | grounded | I |
| `BucketSSEConfig.Apply` (forces/defers) | `aws:kms` (auto) / `AES256` (SSE-S3) | `internal/bucket/encryption/bucket-sse-config.go:135` | (a-i) KMS, (a-ii) AES256 | O |
| `globalAutoEncryption` wiring + KMS req | needs `GlobalKMS != nil` | `cmd/config-current.go:532-533` | server booted with KMS | O |
| `EnvKMSAutoEncryption` | `MINIO_KMS_AUTO_ENCRYPTION` | `internal/crypto/auto-encryption.go:31` | env set at launch | O |
| SSE-C over HTTP guard | `ErrInsecureSSECustomerRequest`, 400 | `cmd/generic-handlers.go:350`; render `cmd/api-errors.go:1182-1184` | 52µs 400 XML | O |
| Precedence | authorize first; encryption applied in handler regardless of grant | — | both interpretations | O |

### Q2 — object-lock delete logging

| Named item | Value / behavior | file:line | Evidence | Label |
|---|---|---|---|---|
| `enforceRetentionBypassForDelete` | returns `ObjectLocked{}` | `cmd/bucket-object-lock.go:84` | driver + trace | O |
| Legal hold `On` | blocked | `:101` | (i) 400 WORM | O |
| Compliance (future) | blocked | `:121` (fail-closed `:117`) | (ii) 400 WORM | O |
| Governance (future, no bypass) | blocked | `:147` (fail-closed `:143`) | (iii) 400 WORM | O |
| Governance bypass guard (`checkRequestAuthType`) | needs bypass header + `s3:BypassGovernanceRetention` | `cmd/bucket-object-lock.go:153` → `checkRequestAuthType` `cmd/auth-handler.go:339` | (iv) 204 success / (iv-contrast) 403 | O |
| `enforceRetentionForDeletion` (read-only check) | helper | `:54` | grounded | I |
| `type ObjectLocked` | error type | `cmd/object-api-errors.go:337` | grounded | I |
| mapping `case ObjectLocked` → `ErrObjectLocked` | — | `cmd/api-errors.go:2298-2299` | grounded | I |
| render: `InvalidRequest` / "Object is WORM protected and cannot be overwritten" / 400 | — | `cmd/api-errors.go:1059-1062` | observed XML matches | O |

### Q3 — bit-rot detection on GET

| Named item | Value / behavior | file:line | Evidence | Label |
|---|---|---|---|---|
| `errFileCorrupt` | `StorageErr("file is corrupted")` | `cmd/storage-errors.go:104` | grounded + heal detect | O/I |
| HighwayHash256 default | `"highwayhash256"` | `cmd/bitrot.go:42` | grounded | I |
| streaming reader mismatch → `errFileCorrupt` | per-block hash check | `cmd/bitrot-streaming.go:185` | 3rd parity read signature | O |
| decoder: `case errors.Is(err, errFileCorrupt)` → `bitrotHeal` | flags heal | `cmd/erasure-decode.go:197` | reconstruction observed | O |
| decoder returns `errFileCorrupt` after decode | reconstruct + heal signal | `cmd/erasure-decode.go:228` | trace | O |
| GET swallows `errFileCorrupt` (`err = nil`) + queues MRF | async heal, `BitrotScan:true` | `cmd/erasure-object.go:397-414` | no stdout line + no sync heal | I (corroborated O) |
| `ReadFile` / `bitrotVerify` / `VerifyFile` | storage-level verify | `cmd/xl-storage.go:1875`, `:3079`, `:3097` | heal trace `VerifyFile` | O |
| Drive state before/during/after (disk1 `part.1`) | `8836d76b…` → `113578b7…` → `8836d76b…` (restored) | — | timeline | O |
| Deep-scan heal classification | bit-rot shard = `state:"missing"` (not "corrupt") | — | dry-run JSON | O |

### Q4 — STS session-policy enforcement

| Named item | Value / behavior | file:line | Evidence | Label |
|---|---|---|---|---|
| `AssumeRole` handler | parses inline session policy | `cmd/sts-handlers.go:256` | trace `Policy=` | O |
| `maxSTSSessionPolicySize` | `2048` | `cmd/sts-handlers.go:89` (check `:123`) | grounded | I |
| session policy embedded in claim | `c[policy.SessionPolicyName] = base64(...)` | `cmd/sts-handlers.go:127` | decoded token claim | O |
| `IsAllowedSTS` | intersection evaluation | `cmd/iam.go:2242` | go test both directions | O |
| intersection `isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed)` | must satisfy both | `cmd/iam.go:2312` | CASE A deny / CASE B allow | O |
| `isAllowedBySessionPolicy` | reads session-policy claim | `cmd/iam.go:2381` | grounded | I |
| suite entry / runner | `TestIAMInternalIDPSTSServerSuite` / `runAllIAMSTSTests` | `cmd/sts-handlers_test.go:52` / `:40` | PASS 13.91s | O |
| `TestSTS` / `TestSTSWithDenyDeleteVersion` | parent-policy + Deny | `:393` / `:180` | PASS | O |
| docs intersection rule | "intersection … cannot grant more than the canned policy" | `docs/sts/assume-role.md:39` | quoted | O |

### Q5 — privilege-escalation prevention + root cause

| Named item | Value / behavior | file:line | Evidence | Label |
|---|---|---|---|---|
| `SetPolicyForUserOrGroup` + guard | requires `AttachPolicyAdminAction` | `cmd/admin-handlers-users.go:1770` / `:1773` | go test 403 | O |
| `AttachDetachPolicyBuiltin` + guard | requires `UpdatePolicyAssociationAction`, `AttachPolicyAdminAction` | `:1908` / `:1911` | go test 403 | O |
| `validateAdminReq` (loop + final deny) | writes `ErrAccessDenied` when all denied | `cmd/admin-handler-utils.go:37`, `:59-60` | 403 JSON | O |
| `checkAdminRequestAuth` → `IsAllowed` | deny-by-default | `cmd/auth-handler.go:189` | 403 | O |
| `admin:CreatePolicy` | `"admin:CreatePolicy"` | `pkg/v3@v3.0.22/policy/admin-action.go:142` | extracted | O |
| `admin:AttachUserOrGroupPolicy` | `"admin:AttachUserOrGroupPolicy"` | `:148` | extracted | O |
| `admin:UpdatePolicyAssociation` | `"admin:UpdatePolicyAssociation"` | `:151` | extracted | O |
| `admin:*` | `"admin:*"` | `:208` | extracted | O |
| suite entry / assertion | `TestIAMInternalIDPServerSuite` / `mustNotUpload` | `cmd/admin-handlers-users_test.go:192` / `:1577` | PASS 14.05s | O |
| Root cause | deny-by-default at `validateAdminReq`; mutation logic never runs | — | control (root succeeds) | O |

### Build/run items

| Item | Value | file:line / evidence |
|---|---|---|
| Go directive | `go 1.23` (built with go1.23.12) | `go.mod:3` |
| `pkg/v3` version | `v3.0.22` | `go.mod:55` |
| build recipe | `CGO_ENABLED=0 go build -tags kqueue -trimpath …` | `Makefile:179` |
| IAM test invocation | `-run TestIAM* ./cmd` | `Makefile:96` |
| version string | `DEVELOPMENT.GOGET` | `cmd/build-constants.go:32`; `--version` output |
| readiness path | `/minio/health/ready` | `cmd/healthcheck-router.go:29`; HTTP 200 |
| trace route | `/minio/admin/v3/trace` | `cmd/admin-router.go:410` |
| KMS env key | `MINIO_KMS_SECRET_KEY` | `internal/kms/config.go:63` |
| erasure layout | `EC:2` (2 data + 2 parity), 4 drives / 1 set | startup banner + `srvinfo` (`madmin` `StorageInfo`) |

**Coverage confirmation (evidence status per question).**

- **Q1 — Observed.** Both encryption-requirement interpretations were reproduced end-to-end: auto-encryption transparently forcing `X-Amz-Server-Side-Encryption: aws:kms` (with the raw HEAD response header), and bucket-default SSE-S3 forcing `AES256`. The precedence — authorization first, then encryption applied in the handler — is shown by the trace (the signed request carries no SSE header, yet the response does). Both the identity-policy `Deny` (authenticated user, 403) and the anonymous bucket-policy `Deny` (403) were reproduced, plus the SSE-C-over-HTTP edge (400 `ErrInsecureSSECustomerRequest`), each with full unedited error XML.
- **Q2 — Observed.** All three lock modes (legal hold `On`, Compliance future, Governance future) block the delete with the full `ErrObjectLocked` XML (HTTP 400), and governance bypass was exercised **both** with permission (root → 204, version removed) and without permission (limited user → 403), with before/during/after object state. The server emits no separate stdout log line for a WORM rejection — the response XML *is* the record — which was itself confirmed by grepping the server stdout.
- **Q3 — Observed detection; the plain-GET "file is corrupted" log line is a documented NON-REPRODUCTION.** Corrupting a data shard on disk and issuing a GET returns the correct reconstructed bytes; the runtime detection signature is the **extra parity read** (3 storage reads vs 2 on a healthy GET), and the `errFileCorrupt` sentinel is explicitly surfaced by a `madmin.Heal` deep scan (`VerifyFile` fast-fails the corrupt shard in ~188 µs). The plain GET itself emits **no** "file is corrupted" log line, because the streaming sentinel (`cmd/bitrot-streaming.go:185`) arises during `erasure.Decode` *after* the storage-layer trace has already published, and is then swallowed (`err = nil`) on the read path (`cmd/erasure-object.go:397-414`); this is analysed with verbatim code in §4.1–§4.2. Before/during/after drive state (`disk1` `part.1` sha256 `8836d76b…` → `113578b7…` → `8836d76b…`) is recorded.
- **Q4 — Observed.** Both intersection directions are proven by a **genuine out-of-repo `go test`** that drives the real `AssumeRole` handler with an inline session policy: action allowed by both parent and session succeeds; action allowed by the parent but denied by the session policy → `AccessDenied`; and a no-session-policy control inherits the full parent grant. The passing in-repo STS suite corroborates parent-policy enforcement.
- **Q5 — Observed.** Self-attach denial through **both** mutators (`AttachDetachPolicyBuiltin`, `SetPolicyForUserOrGroup`) is proven by a **genuine out-of-repo `go test`**: the basic user is refused with `AccessDenied`/403 (full JSON), while root performs the identical mutations successfully — isolating authorization as the sole barrier. The deny-by-default root cause (`validateAdminReq`) is grounded in code and corroborated by the trace showing the policy-DB write never occurs on the denied path. The passing in-repo admin suite corroborates.

Items labeled **I** (inferred-from-reading) in the tables above are code-path anchors that are structurally certain but not individually surfaced as a distinct runtime line; each is corroborated by an adjacent observed behavior as noted.

### Reproduction command index

- Build: `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-build/minio ./`
- Launch: `/tmp/minio-build/minio server /tmp/minio-data/disk{1..4} --address :9000 --console-address :9001` (with `MINIO_KMS_SECRET_KEY`, `MINIO_KMS_AUTO_ENCRYPTION=on`)
- Trace (real admin trace endpoint `/minio/admin/v3/trace` via `madmin` `ServiceTrace`; `mc` is not available offline, so a source-built subscriber `tracecap` is used): `/tmp/minio-investigation/bin/tracecap -endpoint 127.0.0.1:9000 -access minioadmin -secret minioadmin -all -v -out <log>`
- Q4 primary (genuine out-of-repo `go test`, inline session policy, both intersection directions): `cd /tmp/minio-investigation/tools && GOPROXY=off GOFLAGS=-mod=mod GOSUMDB=off go test -v -count=1 ./q4test`
- Q4 suite (in-repo corroboration): `MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 go test -timeout 4m -tags kqueue,dev -v -run TestIAMInternalIDPSTSServerSuite ./cmd`
- Q5 primary (genuine out-of-repo `go test`, basic-user self-attach denial via both mutators + root control): `cd /tmp/minio-investigation/tools && GOPROXY=off GOFLAGS=-mod=mod GOSUMDB=off go test -v -count=1 ./q5test`
- Q5 suite (in-repo corroboration): `MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 go test -timeout 4m -tags kqueue,dev -v -run TestIAMInternalIDPServerSuite ./cmd`
- Q3 heal (real admin heal endpoint `/minio/admin/v3/heal` via `madmin` `Heal`; `mc` unavailable offline, so a source-built driver `q3heal` deep-scans): `/tmp/minio-investigation/bin/q3heal -bucket q3-bitrot -object original.bin -deep`

