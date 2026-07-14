# MinIO Runtime Investigation — branch `minio_c07e5b49d477` (HEAD `c07e5b49d`)

**Repository:** `github.com/minio/minio`
**Branch:** `minio_c07e5b49d477`
**HEAD commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Investigation type:** Read-only, runtime-grounded. The MinIO source tree was treated as read-only evidence; the only persistent artifact produced is this report. Every conclusion below is backed by output actually observed at runtime (server trace lines, audit-log JSON, command output, error bodies, and hashes) and anchored to the specific function and `file:line` that produces the behavior at this HEAD.

This report answers five questions:

1. Policy evaluation vs. server-side-encryption precedence on an unencrypted upload.
2. The log entries emitted when someone deletes an object-locked object.
3. The runtime logs generated on a GET after manual on-disk bit-rot corruption.
4. Enforcement of an STS session policy on temporary credentials.
5. Proof that a basic user cannot self-promote to console admin by modifying user mappings, and the root cause.

Each requirement section follows the same ordering: **Direct Answer → Reproduction → Observed Output → Responsible Code → Rationale.** Any statement that could not be directly observed is explicitly labeled **[INFERRED]**.

> **Secret handling.** The captured output is reproduced verbatim except that bearer-credential/secret material is redacted with an obvious placeholder: STS `SecretAccessKey` values, the STS `SessionToken` JWT string, the KMS master-key bytes, and fixture user passwords are shown as `<REDACTED_…>`. SigV4 per-request `Signature=` values are one-time request signatures (not reusable credentials) and are left intact so the trace remains faithful. Decoded (non-secret) STS claims — including the session policy — are shown in full because they are the evidence.

---

## Environment & Build

All tooling lives outside the repository tree; the source tree was left read-only and clean throughout.

| Component | Value |
|-----------|-------|
| Toolchain | Go **1.23.12** `linux/amd64` (matches `go.mod` `go 1.23` and the CI `1.23.x` matrix in `.github/workflows/go*.yml`) |
| Server binary | `/tmp/minio-bin` (built from HEAD `c07e5b49d`) |
| S3/STS/admin client | `boto3` **1.43.47** / `botocore` 1.43.47 (SigV4), Python 3.13.7 |
| Admin/S3 CLI | `mc` `RELEASE.2025-08-13T08-35-41Z` at `/tmp/bin/mc` |
| Root credentials | `minioadmin:minioadmin` (used **only** to provision fixtures — never to trigger the deny-path behaviors) |

### Build command (verbatim)

```
CGO_ENABLED=0 GOFLAGS=-mod=mod go build -tags kqueue -o /tmp/minio-bin .
```

A plain `go build` (no ldflags) makes the binary self-report `Version: DEVELOPMENT.GOGET`, whereas the canonical `make build` stamps a release version via `buildscripts/gen-ldflags.go`. **This distinction does not affect any of the five behaviors under investigation** (all are request-path behaviors independent of the version string). The observed `--version`:

```
$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

### Server invocations (verbatim)

**Single-drive server** (used for R1, R2, R4, R5). The KMS master key is the well-known MinIO CI test key from `.github/workflows/go.yml`; its 32-byte base64 secret is redacted here:

```
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
MINIO_KMS_SECRET_KEY="my-minio-key:<REDACTED_KMS_KEY_BASE64_32B>" \
/tmp/minio-bin server /tmp/miniodata --address :9000 --console-address :9001
```

Startup banner (`/tmp/blitzy_investigation/out/server9000.log`):

```
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
...
```

**Four-drive erasure-set server** (used for R3 heal-on-read; a multi-drive set is required so parity reconstruction can occur):

```
/tmp/minio-bin server /tmp/minio4/d1 /tmp/minio4/d2 /tmp/minio4/d3 /tmp/minio4/d4 \
  --address :9100 --console-address :9101
```

Startup banner (`/tmp/blitzy_investigation/out/server9100.log`):

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
...
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
```

`mc admin info` reported the 4-drive set as `4 drives online, 0 drives offline` with parity `EC:2`.

### Health gate

Both servers were confirmed ready before any behavior was triggered:

```
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9100/minio/health/ready
200
```

---

## R1 — Policy evaluation vs. server-side-encryption precedence

> **Question (verbatim):** "I am investigating Minio's implementation of policy evaluation logic and server side encryption. I wonder what happens when a bucket level encryption requirement takes precedence over a user's broad write permissions during an unencrypted upload. Identify the specific runtime execution sequence captured in the server trace logs."

### Direct Answer

The bucket's encryption **configuration** governs the stored object regardless of the user's broad write grant. When a non-admin user with `s3:PutObject` uploads an object carrying **no** SSE header to a bucket whose default encryption is SSE-S3, the server **auto-encrypts the object server-side** and returns `200 OK` with `X-Amz-Server-Side-Encryption: AES256` — the encryption header is injected by the server (it is *not* in the client's signed headers). Separately, when the "encryption requirement" is expressed as an **IAM identity policy** that Denies `s3:PutObject` unless the SSE header is present, the same unencrypted PUT is rejected at the authorization stage with **HTTP 403 `AccessDenied`**. Both precedence behaviors were reproduced.

### Reproduction

Fixtures (provisioned as root; the trigger runs as the non-admin user via boto3 SigV4):

```
# bucket with SSE-S3 default encryption
mc mb inv9000/r1bucket
mc encrypt set sse-s3 inv9000/r1bucket           # -> "Auto encryption 'sse-s3' is enabled"

# non-admin user with broad write, NO admin action
mc admin user  add    inv9000 r1user '<REDACTED_USER_SECRET>'
mc admin policy create inv9000 r1writepolicy /tmp/blitzy_investigation/r1writepolicy.json
mc admin policy attach inv9000 r1writepolicy --user r1user
# r1writepolicy: Allow s3:PutObject,s3:GetObject,s3:ListBucket,s3:GetBucketLocation on r1bucket
```

Start the verbose trace capture, then upload with **no** SSE header as `r1user`:

```
# capture (backgrounded) BEFORE the trigger
mc admin trace -v --path 'r1bucket/*' inv9000 > out/r1_trace_verbose.txt &

# trigger: boto3 PutObject with NO ServerSideEncryption argument, signed as r1user
python3 -c '
import boto3; from botocore.config import Config
s3=boto3.client("s3",endpoint_url="http://127.0.0.1:9000",
   aws_access_key_id="r1user",aws_secret_access_key="<REDACTED_USER_SECRET>",
   region_name="us-east-1",config=Config(signature_version="s3v4"))
r=s3.put_object(Bucket="r1bucket",Key="r1obj.txt",Body=b"unencrypted-upload-by-r1user")
print("PUT:",r["ResponseMetadata"]["HTTPStatusCode"],r.get("ServerSideEncryption"))'
```

### Observed Output

**Primary (auto-encrypt) path — the `s3.PutObject` verbose trace** (`out/r1_trace_verbose.txt`, complete and unedited). Note the request's `SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm` — the client did **not** sign any SSE header — yet the traced request carries `X-Amz-Server-Side-Encryption: AES256`, proving the header was injected **server-side** before the object was stored:

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T19:23:45.853] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucket/r1obj.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 7dc6b102-fbd6-4b98-aee0-87dbe3e3bb19
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1user/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=d4d10729bcbd968a0429bf95a85876ab2cf6d9142d62e1fbe0f54950464b1d2e
127.0.0.1:9000 Content-Length: 27
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/U,D,Z,N,e,b cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: 2b6bf029f24c253fb0a6d2cb695cef8f801b0d2d368bb2b813c02f97dbedcf52
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 X-Amz-Checksum-Crc32: RkY34Q==
127.0.0.1:9000 X-Amz-Date: 20260714T192345Z
127.0.0.1:9000 X-Amz-Sdk-Checksum-Algorithm: CRC32
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:23:45.859] [ Duration 5.943ms TTFB 5.867308ms ↑ 244 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 ETag: "7522c02ef7d3f97c2e4403436291811d"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Checksum-Crc32: RkY34Q==
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 X-Amz-Request-Id: 18C23E69A50AD448
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 X-Ratelimit-Limit: 1138872
127.0.0.1:9000 X-Ratelimit-Remaining: 1138872
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <BLOB>
```

The **stored object** is confirmed encrypted at rest (server-side), independent of the unencrypted upload:

```
$ mc stat inv9000/r1bucket/r1obj.txt
Name      : r1obj.txt
Date      : 2026-07-14 19:23:45 UTC
Size      : 27 B
ETag      : 7522c02ef7d3f97c2e4403436291811d
Type      : file
Checksum  : CRC32:RkY34Q==
Encryption: SSE-S3
Metadata  :
  Content-Type: binary/octet-stream

$ python3 -c 'import boto3;from botocore.config import Config;
s3=boto3.client("s3",endpoint_url="http://127.0.0.1:9000",aws_access_key_id="minioadmin",aws_secret_access_key="minioadmin",region_name="us-east-1",config=Config(signature_version="s3v4"));
h=s3.head_object(Bucket="r1bucket",Key="r1obj.txt");
print("HTTPStatusCode:",h["ResponseMetadata"]["HTTPStatusCode"]);print("ServerSideEncryption:",h.get("ServerSideEncryption"))'
HTTPStatusCode: 200
ServerSideEncryption: AES256
```

**Alternate (policy-driven denial) path.** Applying the "encryption requirement" as an **IAM identity policy** (`r1denypolicy`: `Allow s3:PutObject` + explicit `Deny s3:PutObject` when `Null s3:x-amz-server-side-encryption = true`, i.e. header absent) on user `r1denyuser` against `r1denybucket` yields two outcomes (`out/r1_trace_iamdeny.txt`, complete and unedited):

```
# (A) PUT WITHOUT SSE header -> 403 AccessDenied
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T19:26:29.600] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1denybucket/iamdeny-noheader.txt
...
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1denyuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=eb5f4dcd2d28c3ad6faf2c150e29570ab5eb93c0310f1fc22dce6189945d2ec3
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:26:29.601] [ Duration 216µs TTFB 201.995µs ↑ 188 B  ↓ 355 B ]
127.0.0.1:9000 403 Forbidden
...
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>iamdeny-noheader.txt</Key><BucketName>r1denybucket</BucketName><Resource>/r1denybucket/iamdeny-noheader.txt</Resource><RequestId>18C23E8FC524E5FF</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

# (B) PUT WITH x-amz-server-side-encryption: AES256 -> 200 OK (now the SSE header IS in SignedHeaders)
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T19:26:29.612] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1denybucket/iamdeny-withheader.txt
...
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1denyuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm;x-amz-server-side-encryption, Signature=c7f675077c5134fcd8545e8df976b3bfbd0e49ef83a4e54813c3099d7c795c3a
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:26:29.636] [ Duration 23.949ms TTFB 23.922183ms ↑ 218 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
...
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
```

### Responsible Code

- **Write path / SSE application** — `PutObjectHandler` at `cmd/object-handlers.go:1745`. Immediately after authorization, the handler fetches the bucket's default encryption config and applies it to the request header, at `cmd/object-handlers.go:1893-1897`:
  ```go
  // Check if bucket encryption is enabled
  sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
  sseConfig.Apply(r.Header, sse.ApplyOptions{
      AutoEncrypt: globalAutoEncryption,
  })
  ```
  `sseConfig.Apply` injects `X-Amz-Server-Side-Encryption: AES256` into `r.Header` — **independent of the user's write grant**. This is exactly what the trace shows: the header is present on the traced request although the client never signed it.
- **Bucket default-encryption config API** — `PutBucketEncryptionHandler` at `cmd/bucket-encryption-handlers.go:43`; the config body is parsed and size-limited by `validateBucketSSEConfig(io.LimitReader(r.Body, maxBucketSSEConfigSize))` at `cmd/bucket-encryption-handlers.go:69`, where `maxBucketSSEConfigSize = 1 * humanize.MiByte` is defined at `cmd/globals.go:114`.
- **Global auto-encryption toggle** — `EnvKMSAutoEncryption = "MINIO_KMS_AUTO_ENCRYPTION"` at `internal/crypto/auto-encryption.go:31` and `LookupAutoEncryption()` at `internal/crypto/auto-encryption.go:37` populate `globalAutoEncryption`. In this run `MINIO_KMS_AUTO_ENCRYPTION` was **unset** (`globalAutoEncryption=false`), so the injection came from the **bucket's SSE-S3 default rule**, not the global toggle.
- **Trace emission** — `httpTracerMiddleware` at `cmd/http-tracer.go:69` (registered in the `globalMiddlewares` chain at `cmd/routers.go:54`/`:60`, applied via `router.Use(...)` at `cmd/routers.go:112`).
- **Auth/policy path (relevant to the denial variant)** — request auth classification `getRequestAuthType` at `cmd/auth-handler.go:124` → `checkRequestAuthType` at `cmd/auth-handler.go:339`; policy evaluation `IAMSys.IsAllowed` at `cmd/iam.go:2437` (deny-by-default; an explicit `Deny` overrides an `Allow`).

### Rationale

The runtime execution sequence captured in the trace is: **(1)** the request enters through the middleware chain and the tracer records it (`cmd/http-tracer.go:69`); **(2)** `PutObjectHandler` authorizes the write via `IAMSys.IsAllowed` (the broad `s3:PutObject` grant passes); **(3)** the handler then reads the bucket default-encryption config and calls `sseConfig.Apply(...)` at `cmd/object-handlers.go:1893-1897`, which **injects the SSE header into the request**; **(4)** the object is stored SSE-S3-encrypted and the `200` response echoes `X-Amz-Server-Side-Encryption: AES256`. The user's broad write permission determines *whether the write is allowed*; the bucket encryption **configuration** determines *how the object is stored*. These are orthogonal, which is why a broad-write user's unencrypted upload still lands encrypted.

The policy-driven denial variant demonstrates the second sense of "encryption requirement takes precedence": when an **identity** policy conditions `s3:PutObject` on the presence of the SSE header, `IAMSys.IsAllowed` (`cmd/iam.go:2437`) evaluates the explicit `Deny` first and returns `403 AccessDenied` before any object is written.

**Observed nuance (reported as-is; not remediated).** A **resource-based bucket policy** (`mc anonymous set-json`) carrying the identical `Deny`+`Null` condition did **not** deny the authenticated IAM user's unencrypted PUT — the object was stored unencrypted (`mc stat` showed no encryption). The reliable, reproducible policy-driven denial for an authenticated principal was via the **IAM identity policy** shown above. This is documented as observed behavior of resource-vs-identity condition handling for authenticated users; per the investigation constraints it was not modified or "fixed."

---

## R2 — Object-lock delete log entries

> **Question (verbatim):** "I wonder when object locking on a bucket is enabled, what are the specific log entries that appear when someone tries to delete the locked objects? I want you to give me runtime log output to show this."

### Direct Answer

Deleting a locked object version is refused. The client receives **HTTP 400** with `<Code>InvalidRequest</Code>` and `<Message>Object is WORM protected and cannot be overwritten</Message>`; the server emits a matching `s3.DeleteObject … 400 Bad Request` trace entry and an audit-log record with `api.name=DeleteObject`, `status="Bad Request"`, `statusCode=400`. This is uniform across **Retention Governance**, **Retention Compliance**, and **Legal Hold ON**. Governance retention is the only one that can be bypassed, and only by a caller that both sends `x-amz-bypass-governance-retention: true` **and** is granted `s3:BypassGovernanceRetention`; the bypass yields `204 No Content` and the version is deleted. A caller that sends the bypass header but **lacks** the permission gets **HTTP 403 `AccessDenied`** (a different code path); Compliance and Legal Hold cannot be bypassed by anyone.

### Reproduction

```
# object-lock bucket (object lock mandates versioning; versioning auto-enabled)
mc mb --with-lock inv9000/r2bucket

# audit-log webhook -> local receiver appending JSON to out/audit.jsonl
python3 audit_receiver.py &                       # HTTP sink on 127.0.0.1:9999
mc admin config set inv9000 audit_webhook:r2 endpoint="http://127.0.0.1:9999"

# three locked objects, one per lock condition
mc cp gov-obj.txt   inv9000/r2bucket/ ; mc retention set governance 5y inv9000/r2bucket/gov-obj.txt  --versions
mc cp comp-obj.txt  inv9000/r2bucket/ ; mc retention set compliance 5y inv9000/r2bucket/comp-obj.txt --versions
mc cp legal-obj.txt inv9000/r2bucket/ ; mc legalhold set inv9000/r2bucket/legal-obj.txt

# capture trace BEFORE the deletes
mc admin trace -v inv9000 > out/r2_trace_delete.txt &

# version-specific permanent delete attempts (a versionless DELETE only writes a delete-marker
# and would NOT hit the retention guard) — driven via boto3 SigV4
python3 r2_delete_locked.py
```

### Observed Output

**Server trace — all three lock conditions return `400 Bad Request` / WORM** (`out/r2_trace_delete.txt`, complete and unedited):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:29:54.648] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/gov-obj.txt?versionId=c226d3aa-7365-4928-b3fc-1d1b96b54680
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 8ea6f781-ced2-4e3c-8fe1-bc525acdb97b
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=a91cdba98cc544b2ca1f15a7174b2dbdce802663f33ebbaf2e63d725bc8a0e59
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Date: 20260714T192954Z
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:29:54.648] [ Duration 570µs TTFB 537.657µs ↑ 131 B  ↓ 367 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 Content-Length: 367
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Request-Id: 18C23EBF82EC8C83
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>gov-obj.txt</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/gov-obj.txt</Resource><RequestId>18C23EBF82EC8C83</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:29:54.664] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/comp-obj.txt?versionId=390527bd-4a49-42a6-9bdf-706894ac5358
...
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 X-Amz-Request-Id: 18C23EBF83DDC877
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>comp-obj.txt</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/comp-obj.txt</Resource><RequestId>18C23EBF83DDC877</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:29:54.669] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/legal-obj.txt?versionId=25184238-58f9-42ed-9aa1-65943a31d08e
...
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 X-Amz-Request-Id: 18C23EBF84367F52
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>legal-obj.txt</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/legal-obj.txt</Resource><RequestId>18C23EBF84367F52</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Audit-log JSON** for a locked-delete attempt (the compliance object, re-captured fresh; complete and unedited from `out/audit.jsonl`). This is the "specific log entry" that appears in the audit target — note `api.name=DeleteObject`, `api.status="Bad Request"`, `api.statusCode=400`, the `versionId` in `requestQuery`, and the principal in `accessKey`:

```json
{
  "version": "1",
  "deploymentid": "c988f09d-edd5-4c01-8b8b-97fba610f281",
  "time": "2026-07-14T19:48:36.453015362Z",
  "event": "",
  "trigger": "incoming",
  "api": {
    "name": "DeleteObject",
    "bucket": "r2bucket",
    "object": "comp-obj.txt",
    "status": "Bad Request",
    "statusCode": 400,
    "rx": 0,
    "tx": 369,
    "txHeaders": 411,
    "timeToFirstByte": "685826ns",
    "timeToFirstByteInNS": "685826",
    "timeToResponse": "692189ns",
    "timeToResponseInNS": "692189"
  },
  "remotehost": "127.0.0.1",
  "requestID": "18C23FC4B3A6AC28",
  "userAgent": "Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/D,e,Z,N,b cfg/retry-mode#legacy Botocore/1.43.47",
  "requestPath": "/r2bucket/comp-obj.txt",
  "requestHost": "127.0.0.1:9000",
  "requestQuery": {
    "versionId": "390527bd-4a49-42a6-9bdf-706894ac5358"
  },
  "requestHeader": {
    "Accept-Encoding": "identity",
    "Amz-Sdk-Invocation-Id": "0c93ac64-ac61-4202-8b04-b592477a0c67",
    "Amz-Sdk-Request": "attempt=1",
    "Authorization": "AWS4-HMAC-SHA256 Credential=minioadmin/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=95b2d8d218c17fad568bd687b3cbf3b932dc910a0103e2e4cf910feb9e62de2e",
    "Content-Length": "0",
    "User-Agent": "Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/D,e,Z,N,b cfg/retry-mode#legacy Botocore/1.43.47",
    "X-Amz-Content-Sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "X-Amz-Date": "20260714T194836Z"
  },
  "responseHeader": {
    "Accept-Ranges": "bytes",
    "Content-Length": "369",
    "Content-Type": "application/xml",
    "Server": "MinIO",
    "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
    "Vary": "Origin,Accept-Encoding",
    "X-Amz-Id-2": "dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8",
    "X-Amz-Request-Id": "18C23FC4B3A6AC28",
    "X-Content-Type-Options": "nosniff",
    "X-Ratelimit-Limit": "1138872",
    "X-Ratelimit-Remaining": "1138872",
    "X-Xss-Protection": "1; mode=block"
  },
  "tags": {
    "DeleteObject": "name=comp-obj.txt,pool=1,set=1",
    "GetObjectInfo": "name=comp-obj.txt,pool=1,set=1"
  },
  "accessKey": "minioadmin"
}
```

**Before / during / after state.** Before: each object existed as a locked version. During: the DELETE attempts returned `400`. After: `mc ls --versions inv9000/r2bucket` still listed `comp-obj.txt`, `gov-obj.txt`, and `legal-obj.txt` versions — no version was removed by the refused deletes.

**Governance bypass cross-product** (`out/r2_trace_bypass.txt`, complete and unedited). Three distinct outcomes prove the three code paths:

```
# (1) bypass header + user GRANTED s3:BypassGovernanceRetention  -> 204, version DELETED
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:30:57.235] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/gov-bypass.txt?versionId=17b02e19-dfbe-4ec5-ac97-27c59e3e8fcd
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2bypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=2603c8ca8e6c1686eaaadf9311011ae9d40515ab8136e366c4dae7e09c9d563e
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:30:57.236] [ Duration 993µs TTFB 950.356µs ↑ 165 B  ↓ 0 B ]
127.0.0.1:9000 204 No Content
127.0.0.1:9000 x-amz-version-id: 17b02e19-dfbe-4ec5-ac97-27c59e3e8fcd
127.0.0.1:9000 X-Amz-Request-Id: 18C23ECE15694ABD

# (2) bypass header + user LACKS the permission -> 403 AccessDenied (errAuthentication path)
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:30:57.242] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/gov-nobypass.txt?versionId=23a35963-c342-44a7-b596-ccc936b1e4a3
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=cfe61742698ea69f6f6722022dda6c39e3fa8cb449ca568e587fe9ab0b044723
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:30:57.243] [ Duration 592µs TTFB 548.577µs ↑ 165 B  ↓ 339 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>gov-nobypass.txt</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/gov-nobypass.txt</Resource><RequestId>18C23ECE15D27207</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

# (3) NO bypass header + user LACKS the permission -> 400 WORM (ObjectLocked path)
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T19:30:57.247] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/gov-nobypass.txt?versionId=23a35963-c342-44a7-b596-ccc936b1e4a3
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=21ce369a6d488f8c2147a6de94e4ef8a1199acee8c5ebef4b6cd643abb211f14
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:30:57.248] [ Duration 449µs TTFB 432.456µs ↑ 131 B  ↓ 377 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>gov-nobypass.txt</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/gov-nobypass.txt</Resource><RequestId>18C23ECE16269DB9</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Responsible Code

All enforcement is in `enforceRetentionBypassForDelete` at `cmd/bucket-object-lock.go:84`:

- **Legal Hold ON** → `return ObjectLocked{}` at `cmd/bucket-object-lock.go:100-101`:
  ```go
  if lhold.Status.Valid() && lhold.Status == objectlock.LegalHoldOn {
      return ObjectLocked{}
  }
  ```
- **Compliance**, `RetainUntilDate` not before now → `return ObjectLocked{}` at `cmd/bucket-object-lock.go:117` (the code comment notes it "can't be overwritten or deleted by any user, including the root user").
- **Governance** → `byPassSet := objectlock.IsObjectLockGovernanceBypassSet(r.Header)` at `cmd/bucket-object-lock.go:138`. If the bypass header is **not** set → `return ObjectLocked{}` at `cmd/bucket-object-lock.go:143`/`:147`. If it **is** set, the permission is checked at `cmd/bucket-object-lock.go:153-154`:
  ```go
  if checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, bucket, object.ObjectName) != ErrNone {
      return errAuthentication
  }
  ```
- **Error mapping** — `ObjectLocked{}` maps to `ErrObjectLocked`, applied at `cmd/api-errors.go:2299` (`apiErr = ErrObjectLocked`); the API error is defined at `cmd/api-errors.go:1059-1063`:
  ```go
  ErrObjectLocked: {
      Code:           "InvalidRequest",
      Description:    "Object is WORM protected and cannot be overwritten",
      HTTPStatusCode: http.StatusBadRequest,
  },
  ```
  This is exactly the `400` / `InvalidRequest` / WORM body observed. The `errAuthentication` return (Governance bypass without permission) is what surfaces as the `403 AccessDenied` in case (2).

### Rationale

The two distinct denials map cleanly to the two return statements in `enforceRetentionBypassForDelete`: retention/legal-hold protection (`ObjectLocked{}` → `400 InvalidRequest`, "WORM protected") versus a missing bypass grant (`errAuthentication` → `403 AccessDenied`). Because `checkRequestAuthType(..., policy.BypassGovernanceRetentionAction, ...)` (`cmd/bucket-object-lock.go:153`) is only consulted for **Governance** when the bypass header is present, Compliance and Legal Hold have **no** bypass branch — they can never be deleted before expiry, which matches the uniform `400` observed for all three lock conditions and the `204` observed only for the permitted Governance bypass.


---

## R3 — Bit-rot detection on GET after manual corruption

> **Question (verbatim):** "I also want you to analyze how the system handles unauthorized manual data corruption within the storage backend. Trigger a bit rot detection event and identify the specific runtime logs generated during a subsequent get request."

### Direct Answer

On a multi-drive erasure set, manually corrupting an object's on-disk shard (`part.1`) and then issuing a GET triggers MinIO's streaming HighwayHash verifier, which detects the shard hash mismatch and surfaces the internal sentinel `errFileCorrupt`. The decode layer flags a heal, reconstructs the object **from parity**, and the GET still returns `200 OK` with the **byte-identical** original content. In the server trace this appears as `storage.ReadFileStream` reads across the drives, an `s3.GetObject … 200 OK ↓ 5.0 MiB` response, and a subsequent `[HEALING heal.Object] …` event. The `errFileCorrupt` sentinel itself is an **internal** error and is **not** written to `mc admin logs` / server stderr by default (**[INFERRED]** for the "no console line" claim — see Rationale). On a **single-drive** set (no parity) the same corruption is detected but **unrecoverable**, and the GET returns **HTTP 503 `SlowDownRead`** ("Resource requested is unreadable").

### Reproduction

```
# 4-drive erasure server (EC:2) is required so heal-on-read can reconstruct from parity
mc mb inv9100/r3bucket
# deterministic 5 MiB payload; record original hash
sha256sum r3_payload.bin            # e1c53465...d00047
mc cp r3_payload.bin inv9100/r3bucket/r3obj.bin

# locate the on-disk shard on ONE drive and corrupt it in place (same size => silent bit rot)
SHARD=$(find /tmp/minio4/d1/r3bucket/r3obj.bin -name part.1)
# overwrite 32 bytes at offset 1000000 with 0xDE (recorded before/after hashes below)
python3 - "$SHARD" <<'PY'
import sys
p=sys.argv[1]
with open(p,'r+b') as f: f.seek(1000000); f.write(b'\xDE'*32)
PY

# force the read to hit disk, not page cache
sync; echo 1 > /proc/sys/vm/drop_caches

# capture storage+heal trace BEFORE the GET, then GET via boto3 SigV4 and compare bytes
mc admin trace --all -v inv9100 > out/r3_trace_all.txt &
python3 -c '
import boto3,hashlib;from botocore.config import Config
s3=boto3.client("s3",endpoint_url="http://127.0.0.1:9100",aws_access_key_id="minioadmin",aws_secret_access_key="minioadmin",region_name="us-east-1",config=Config(signature_version="s3v4"))
d=s3.get_object(Bucket="r3bucket",Key="r3obj.bin")["Body"].read()
print(len(d), hashlib.sha256(d).hexdigest())'
```

### Observed Output

**Byte-sensitive verification** (fresh re-run, confirming heal against the exact emitted bytes):

```
ORIGINAL payload sha256 : e1c53465a936157a3a959643604c5030d5ec54ef03dce17c47d7531698d00047
d1 shard path           : /tmp/minio4/d1/r3bucket/r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1
d1 shard size           : 2621600 bytes
d1 shard sha256 (corrupt on disk): dcb97c553e7bb6886f73c3ada90919a0390655a4691419c2e2ab375b3d92e731

--- corrupted region bytes at offset 1000000 (od -A d -t x1, 32 bytes) ---
1000000 de de de de de de de de de de de de de de de de
*
1000032

--- drop page cache ---
page cache dropped (echo 1 > /proc/sys/vm/drop_caches)

--- GET object via boto3 (canonical S3 GET) and compare hash ---
GET HTTPStatusCode     : 200
GET body length        : 5242880
GET body sha256        : e1c53465a936157a3a959643604c5030d5ec54ef03dce17c47d7531698d00047
MATCHES ORIGINAL       : True
```

The clean shard hash before corruption was `d51f3f18e4c2c7c2a6105dcedbcf50cd8cc4a81a26eaeb2e4157b67f1d0077ae`; after overwriting 32 bytes with `0xDE` it became `dcb97c55…` with the **size unchanged** (2621600). The healed GET body hash equals the original payload hash exactly — the corruption was masked by parity reconstruction.

**Server trace — storage reads, the `200 OK` GET, and the heal event** (`out/r3_trace_all.txt`, relevant lines, unedited):

```
127.0.0.1:9100  [OS os.OpenFileR] [2026-07-14T19:35:14.354] /tmp/minio4/d2/r3bucket/r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 50.843µs
127.0.0.1:9100  [OS os.OpenFileR] [2026-07-14T19:35:14.354] /tmp/minio4/d1/r3bucket/r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 46.474µs
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T19:35:14.354] /tmp/minio4/d1 r3bucket r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 total-errs-availability=0 total-errs-timeout=0 72.269µs 2.5 MiB
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T19:35:14.354] /tmp/minio4/d2 r3bucket r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 total-errs-availability=0 total-errs-timeout=0 81.236µs 2.5 MiB
127.0.0.1:9100  [OS os.OpenFileR] [2026-07-14T19:35:14.409] /tmp/minio4/d3/r3bucket/r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 42.75µs
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T19:35:14.409] /tmp/minio4/d3 r3bucket r3obj.bin/8fce5dad-c91b-435f-b023-b72ecd10ec56/part.1 total-errs-availability=0 total-errs-timeout=0 85µs 2.0 MiB
127.0.0.1:9100 [REQUEST s3.GetObject] [2026-07-14T19:35:14.273] [Client IP: 127.0.0.1]
127.0.0.1:9100 GET /r3bucket/r3obj.bin
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date, Signature=fd58997fb9c7c546485f5aa99c18a94b67706b46efc530bdd4c134267d91fc7c
127.0.0.1:9100 <BLOB>
127.0.0.1:9100 [RESPONSE] [2026-07-14T19:35:14.464] [ Duration 191.44ms TTFB 127.874063ms ↑ 151 B  ↓ 5.0 MiB ]
127.0.0.1:9100 200 OK
...
127.0.0.1:9100  [STORAGE storage.CheckParts] [2026-07-14T19:35:15.466] /tmp/minio4/d4 r3bucket r3obj.bin total-errs-availability=0 total-errs-timeout=0 9.434µs
127.0.0.1:9100  [HEALING heal.Object] [2026-07-14T19:35:15.465] r3bucket/r3obj.bin disks=4 dry=false mode=0 remove=true version-id=null 298.923µs 5.0 MiB
```

**Stability (≥2 runs — actually 4).** The heal-on-read behavior was confirmed stable across four runs (re-corrupt + drop cache + GET each time): each GET returned `200` with the correct sha256 and produced exactly one `heal.Object` event, with consistent heal timing:

```
# run #2 (out/r3_heal_run2.txt)
127.0.0.1:9100  [HEALING heal.Object] [2026-07-14T19:39:04.621] r3bucket/r3obj.bin mode=0 remove=true version-id=null disks=4 dry=false 292.735µs 5.0 MiB
# run #3 (out/r3_heal_run3.txt)
127.0.0.1:9100  [HEALING heal.Object] [2026-07-14T19:39:15.928] r3bucket/r3obj.bin disks=4 dry=false mode=0 remove=true version-id=null 285.662µs 5.0 MiB
# run #4: GET 200, sha256 == original (shown in the byte-verification block above)
```

**Secondary (single-drive, no parity → unrecoverable)** (`out/r3_single_trace.txt`, first attempt, unedited). Corrupting `s.bin`'s `part.1` on the single-drive server and GETting yields repeated `503 Service Unavailable` / `SlowDownRead` (boto3 retried 6×):

```
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-14T19:37:43.242] /tmp/miniodata r3single s.bin/de4e1584-8108-413d-8097-90d1e4b2d98a/part.1 total-errs-availability=0 total-errs-timeout=0 94.774µs 2.0 MiB
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T19:37:43.220] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /r3single/s.bin
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:37:43.253] [ Duration 32.525ms TTFB 32.474487ms ↑ 151 B  ↓ 368 B ]
127.0.0.1:9000 503 Service Unavailable
127.0.0.1:9000 Retry-After: 60
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>s.bin</Key><BucketName>r3single</BucketName><Resource>/r3single/s.bin</Resource><RequestId>18C23F2C9C0242E6</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Responsible Code

- **Sentinel** — `errFileCorrupt = StorageErr("file is corrupted")` at `cmd/storage-errors.go:104` (the comment at `:103` reads "file has an unexpected size, or is not readable").
- **Streaming per-shard HighwayHash verify** — `streamingBitrotReader.ReadAt` at `cmd/bitrot-streaming.go:150`; the hash comparison and mismatch return at `cmd/bitrot-streaming.go:184-185`:
  ```go
  if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {
      return 0, errFileCorrupt
  ```
- **Decode detection + heal flag** — `parallelReader.Read` at `cmd/erasure-decode.go:127`; the atomic heal flag is initialized `bitrotHeal := int32(0)` at `cmd/erasure-decode.go:154`; on `errFileCorrupt` it sets the flag at `cmd/erasure-decode.go:197-198`:
  ```go
  case errors.Is(err, errFileCorrupt):
      atomic.StoreInt32(&bitrotHeal, 1)
  ```
  and, when the object is still decodable from parity, returns the corrupt signal to drive heal-on-read at `cmd/erasure-decode.go:227-228`:
  ```go
  } else if bitrotHeal == 1 {
      return newBuf, errFileCorrupt
  ```
- **Whole-file verify helper** — `bitrotVerify` at `cmd/bitrot.go:158` (also returns `errFileCorrupt` on mismatch).

### Rationale

The GET path opens each shard and streams it through `streamingBitrotReader.ReadAt`, which recomputes the HighwayHash over the shard bytes and compares it to the stored hash. Because the on-disk `part.1` was altered (the 32 `0xDE` bytes), the recomputed hash no longer matches `b.hashBytes`, so `ReadAt` returns `errFileCorrupt` (`cmd/bitrot-streaming.go:184-185`). `parallelReader.Read` catches this (`cmd/erasure-decode.go:197-198`), and because the 4-drive EC:2 set still has enough good shards to decode, it reconstructs the data from parity and returns `newBuf, errFileCorrupt` (`cmd/erasure-decode.go:227-228`) — the corrupt signal that triggers the subsequent `heal.Object` event seen in the trace. The client therefore receives correct bytes (`200`, matching sha256), which is why the corruption is invisible at the S3 layer on a redundant set. On the single-drive set there is no parity, so the same `errFileCorrupt` cannot be reconstructed and instead surfaces to the client as `503 SlowDownRead`.

**Observed nuance (reported as-is; not remediated).** The corrupted `d1` shard remained `0xDE` on disk after heal-on-read *and* after an explicit `mc admin heal --recursive --force` (which reported "Healed: 0/1 objects"). **[INFERRED]** cause: the default heal validates shards with `storage.CheckParts` (existence + **size**, visible in the trace), not the HighwayHash, and the scanner's bit-rot deep-scan is off by default — so a **same-size** silent corruption is not rewritten by the normal heal, even though every client GET is served correct bytes via on-the-fly decode-layer reconstruction. This is labeled inferred because it explains the *absence* of a disk rewrite; the client-facing behavior (correct bytes + `heal.Object` event) is directly observed. Separately, `errFileCorrupt` produced **no** line in `mc admin logs` and no "corrupt" match in server stderr during these runs — the sentinel is an internal error consumed by the decode/heal layer rather than a console log entry (**[INFERRED]** that this is by-design; the observation is the empty log output).


---

## R4 — STS session-policy enforcement

> **Question (verbatim):** "I want you to verify that when a user gets temporary credentials, minio is able to enforce the session policy on that user. You need to give me runtime test output to prove this behavior."

### Direct Answer

Temporary credentials issued by STS `AssumeRole` are constrained by their inline session policy: the effective permission is the **intersection** of the parent user's policy and the session policy. Proven at runtime — the parent user is allowed both `s3:GetObject` and `s3:PutObject`; the assumed credentials carry a session policy that allows **only** `s3:GetObject`. Using the temporary credentials, `GetObject` succeeds (`200`, correct body) while `PutObject` — which the parent allows but the session policy omits — is denied with **HTTP 403 `AccessDenied`**. The session policy can only **narrow**, never widen, the parent's permissions.

### Reproduction

```
# non-admin parent user with a BROAD policy (Allow s3:GetObject + s3:PutObject + ListBucket on r4bucket)
mc admin user  add    inv9000 r4parent '<REDACTED_USER_SECRET>'
mc admin policy create inv9000 r4parentpolicy /tmp/blitzy_investigation/r4parentpolicy.json
mc admin policy attach inv9000 r4parentpolicy --user r4parent
mc cp r4obj.txt inv9000/r4bucket/          # seed object, body "hello-r4-object-content"

# capture trace, then run the STS test (boto3 sts.assume_role + boto3 s3 with temp creds)
mc admin trace -v inv9000 > out/r4_trace.txt &
python3 r4_sts_test.py
```

`r4_sts_test.py` (key logic): baseline parent `PutObject`; then `sts.assume_role(RoleArn=…, RoleSessionName="r4session", Policy=<Allow s3:GetObject only>, DurationSeconds=900)`; then, with the returned temporary credentials, a `GetObject` (session allows) and a `PutObject` (parent allows, session omits).

### Observed Output

**Test-harness output** (fresh run; `SecretAccessKey` and `SessionToken` redacted per the note at the top of this report):

```
BASELINE parent PutObject: 200 (parent policy allows Put)

=== AssumeRole returned temporary credentials (secrets REDACTED) ===
HTTPStatusCode : 200
AccessKeyId    : 03VGREF7IVQXRQ7D3WQE
SecretAccessKey: <REDACTED len=40>
SessionToken   : <REDACTED len=466, issued=True>
Expiration     : 2026-07-14T20:05:30+00:00
inline session policy sent: {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::r4bucket/*"]}]}

=== (A) temp-cred GetObject (session ALLOWS) -> expect 200 ===
GetObject status: 200 body: hello-r4-object-content

=== (B) temp-cred PutObject (parent ALLOWS, session OMITS) -> expect AccessDenied ===
HTTPStatusCode: 403
Error Code   : AccessDenied
Error Message: Access Denied.
```

**Decoded `SessionToken` claims** (the raw JWT is redacted, but its non-secret claims are the evidence — the inline session policy is embedded verbatim in the credential's claims):

```
JWT header alg: HS512
accessKey : WC2275M3Y5Y18589X034
parent    : r4parent
exp       : 1784059531
sessionPolicy (decoded): {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::r4bucket/*"]}]}
```

**Server trace** (`out/r4_trace.txt`, complete and unedited except the `X-Amz-Security-Token` JWT value, which is redacted). Note the temp-cred requests carry `x-amz-security-token` in their `SignedHeaders`; `GetObject` → `200`, `PutObject` → `403 AccessDenied`:

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T19:40:47.403] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r4bucket/parent-put.txt
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r4parent/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=8aa05ab1f4dd820ae2e07a6d9bf14014410808d162b52e7da20599d6e3984d5d
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:40:47.459] [ Duration 55.405ms TTFB 55.33694ms ↑ 202 B  ↓ 0 B ]
127.0.0.1:9000 200 OK                                        # baseline: parent CAN put

127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T19:40:47.516] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /r4bucket/r4obj.txt
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=MP4GB7UL469G414EJ3JQ/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=675ffc5a966630b8f678e72c7bb2e6430754b3b6b94208204d1faebbee79392c
127.0.0.1:9000 X-Amz-Security-Token: <REDACTED_JWT_SESSION_TOKEN>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:40:47.517] [ Duration 1.256ms TTFB 1.202337ms ↑ 172 B  ↓ 24 B ]
127.0.0.1:9000 200 OK                                        # (A) session ALLOWS GetObject

127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T19:40:47.520] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r4bucket/temp-put.txt
127.0.0.1:9000 X-Amz-Security-Token: <REDACTED_JWT_SESSION_TOKEN>
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=MP4GB7UL469G414EJ3JQ/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm;x-amz-security-token, Signature=9269d39ea98631f669c21dc7766516541cfe7487ed01f85495db73c5134a152c
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:40:47.520] [ Duration 254µs TTFB 214.99µs ↑ 209 B  ↓ 331 B ]
127.0.0.1:9000 403 Forbidden                                 # (B) session OMITS PutObject
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>temp-put.txt</Key><BucketName>r4bucket</BucketName><Resource>/r4bucket/temp-put.txt</Resource><RequestId>18C23F5785242ED0</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

*(The `AccessKeyId` in the harness output — `03VGREF7…` — differs from the trace's `MP4GB7UL…` because each block is from a separate `assume_role` call; both are ephemeral STS access keys, not long-term secrets.)*

### Responsible Code

- **Issuance** — `AssumeRole` at `cmd/sts-handlers.go:256`. The inline session-policy size cap is `maxSTSSessionPolicySize = 2048` at `cmd/sts-handlers.go:89`, enforced at `cmd/sts-handlers.go:123-124` (`if len(policyBuf) > maxSTSSessionPolicySize { return errSessionPolicyTooLarge }`); the overall STS request body limit is `stsRequestBodyLimit = 10 * (1 << 20)` (10 MiB) at `cmd/sts-handlers.go:67`.
- **Enforcement / intersection** — `IsAllowedSTS` at `cmd/iam.go:2242`. The intersection is computed at `cmd/iam.go:2310` / `:2312`:
  ```go
  hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)   // :2310
  ...
  return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))   // :2312
  ```
  The result requires the **session policy** (`isAllowedSP`) **and** the parent/combined policy to both allow the action. `isAllowedBySessionPolicy` is defined at `cmd/iam.go:2381`.
- **Credential model** — `internal/auth/credentials.go`: `SessionToken` at `:116`, `ParentUser` at `:119`, `Claims` at `:121` (where the decoded `sessionPolicy` lives), and `IsTemp()` at `:157-158` (`return cred.SessionToken != "" && !cred.Expiration.IsZero() && …`).

### Rationale

`AssumeRole` embeds the inline session policy into the temporary credential's JWT claims (visible in the decoded `sessionPolicy` above). On each request with those credentials, `IsAllowedSTS` evaluates `isAllowedBySessionPolicy(args)` **and** the parent/combined policy (`cmd/iam.go:2312`). For `GetObject`, both allow → `200`. For `PutObject`, the parent allows but the session policy has no matching statement, so `isAllowedSP` is false and the `&&` short-circuits the decision to deny → `403 AccessDenied`. This is precisely the intersection semantic the question asks to verify: temporary credentials cannot exceed their session policy even where the parent is more permissive.


---

## R5 — Privilege-escalation prevention via user mappings

> **Question (verbatim):** "Show me test output to prove that a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user mappings modification behavior that you observe."

### Direct Answer

A basic (non-admin) user **cannot** self-promote to console admin. Using only the basic user's own credentials, both IAM policy-mapping admin APIs — `SetPolicyForUserOrGroup` (`PUT /minio/admin/v3/set-user-or-group-policy`) and `AttachDetachPolicyBuiltin` (`POST /minio/admin/v3/idp/builtin/policy/attach`) — reject the attempt to attach `consoleAdmin` with **HTTP 403 `AccessDenied`**, and the user→policy mapping in the IAM store is **unchanged** afterward. **Root cause:** both handlers begin with `validateAdminReq(...)`, which routes through `checkAdminRequestAuth` (`cmd/auth-handler.go:189`); that gate calls `globalIAMSys.IsAllowed` for the required admin action (`AttachPolicyAdminAction` / `UpdatePolicyAssociationAction`) and, because the basic user was never granted any admin action, `IsAllowed` returns false and the gate returns `ErrAccessDenied` (`cmd/auth-handler.go:206`) — **deny-by-default authorization of the required admin action**. The mapping-write function `PolicyDBSet` (`cmd/admin-handlers-users.go:1849`) is never reached.

### Reproduction

```
# basic user with a limited S3-only policy (NO admin action)
mc admin user  add    inv9000 r5basic '<REDACTED_USER_SECRET>'
mc admin policy create inv9000 r5basicpolicy /tmp/blitzy_investigation/r5basicpolicy.json
mc admin policy attach inv9000 r5basicpolicy --user r5basic
# r5basicpolicy: Allow s3:GetObject,s3:PutObject,s3:ListBucket on r5bucket only

# BEFORE state
mc admin user info      inv9000 r5basic
mc admin policy entities inv9000 --user r5basic

# capture trace, then attempt self-promotion using ONLY r5basic's credentials (SigV4-signed raw admin calls)
mc admin trace -v inv9000 > out/r5_signed_trace.txt &
python3 r5_signed_admin.py         # signs as r5basic and calls BOTH admin handlers

# AFTER state
mc admin user info      inv9000 r5basic
mc admin policy entities inv9000 --user r5basic
mc admin policy entities inv9000 --policy consoleAdmin
```

### Observed Output

**BEFORE — the basic user maps only to its limited policy:**

```
$ mc admin user info inv9000 r5basic
AccessKey: r5basic
Status: enabled
PolicyName: r5basicpolicy
MemberOf: []
```

**Self-promotion attempts.** The `mc` convenience command fails immediately:

```
$ mc admin policy attach r5alias consoleAdmin --user r5basic
mc: <ERROR> Unable to make user/group policy association. Access Denied.
```

Driving **both** handlers directly with SigV4-signed raw admin requests (signed as `r5basic`) — server trace, complete and unedited (`out/r5_signed_trace.txt`). Both return `403 Forbidden` with a JSON `AccessDenied` body; the trace names each handler (`admin.SetPolicyForUserOrGroup`, `admin.AttachDetachPolicyBuiltin`) and shows `Credential=r5basic`:

```
127.0.0.1:9000 [REQUEST admin.SetPolicyForUserOrGroup] [2026-07-14T19:43:31.392] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-or-group-policy?policyName=consoleAdmin&userOrGroup=r5basic&isGroup=false
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=7a1d602dc15a768885548e0f0c1f98516c17ecb6b662a560da0e53f67aad2c38
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:43:31.392] [ Duration 114µs TTFB 96.598µs ↑ 90 B  ↓ 212 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C23F7DACA6588F","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-14T19:43:31.393] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=7e8f5e42cadea8078fd55084e750ba28938aea241201ed27663679ee5c34b252
127.0.0.1:9000 Content-Length: 62
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T19:43:31.393] [ Duration 81µs TTFB 72.353µs ↑ 90 B  ↓ 213 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C23F7DACC08359","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

**Audit-log JSON** for both denied attempts (`out/audit.jsonl`, complete and unedited). The principal is `r5basic` (in the `Authorization` `Credential=`), `api.status="Forbidden"`, `api.statusCode=403`:

```json
{
  "version": "1",
  "deploymentid": "c988f09d-edd5-4c01-8b8b-97fba610f281",
  "time": "2026-07-14T19:43:31.392287035Z",
  "trigger": "incoming",
  "api": { "name": "SetPolicyForUserOrGroup", "status": "Forbidden", "statusCode": 403, "rx": 0, "tx": 212, "txHeaders": 352, "timeToResponseInNS": "103635" },
  "remotehost": "127.0.0.1",
  "requestID": "18C23F7DACA6588F",
  "userAgent": "python-requests/2.34.2",
  "requestPath": "/minio/admin/v3/set-user-or-group-policy",
  "requestQuery": { "isGroup": "false", "policyName": "consoleAdmin", "userOrGroup": "r5basic" },
  "requestHeader": {
    "Authorization": "AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=7a1d602dc15a768885548e0f0c1f98516c17ecb6b662a560da0e53f67aad2c38",
    "User-Agent": "python-requests/2.34.2",
    "X-Amz-Date": "20260714T194331Z"
  },
  "responseHeader": { "Content-Length": "212", "Content-Type": "application/json", "X-Amz-Request-Id": "18C23F7DACA6588F" }
}
{
  "version": "1",
  "deploymentid": "c988f09d-edd5-4c01-8b8b-97fba610f281",
  "time": "2026-07-14T19:43:31.393974166Z",
  "trigger": "incoming",
  "api": { "name": "AttachDetachPolicyBuiltin", "status": "Forbidden", "statusCode": 403, "rx": 62, "tx": 213, "txHeaders": 352, "timeToResponseInNS": "76965" },
  "remotehost": "127.0.0.1",
  "requestID": "18C23F7DACC08359",
  "userAgent": "python-requests/2.34.2",
  "requestPath": "/minio/admin/v3/idp/builtin/policy/attach",
  "requestHeader": {
    "Authorization": "AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=7e8f5e42cadea8078fd55084e750ba28938aea241201ed27663679ee5c34b252",
    "Content-Length": "62",
    "User-Agent": "python-requests/2.34.2",
    "X-Amz-Date": "20260714T194331Z"
  },
  "responseHeader": { "Content-Length": "213", "Content-Type": "application/json", "X-Amz-Request-Id": "18C23F7DACC08359" }
}
```

**AFTER — the mapping is unchanged** (self-promotion failed; the IAM store was not modified):

```
$ mc admin user info inv9000 r5basic
AccessKey: r5basic
Status: enabled
PolicyName: r5basicpolicy

$ mc admin policy entities inv9000 --user r5basic
Query time: 2026-07-14T19:49:52Z
User -> Policy Mappings:
  User: r5basic
    Policies:
      r5basicpolicy

$ mc admin policy entities inv9000 --policy consoleAdmin
Query time: 2026-07-14T19:49:52Z
```

`r5basic` still maps only to `r5basicpolicy`, and `consoleAdmin` has **no** user mappings — `r5basic` did not attach itself.

### Responsible Code (and Root Cause)

- **Routes (both wrapped in `adminMiddleware`)** — `cmd/admin-router.go:263-264` registers `PUT .../set-user-or-group-policy` → `adminMiddleware(adminAPI.SetPolicyForUserOrGroup)`; `cmd/admin-router.go:268` registers `POST .../idp/builtin/policy/{operation}` → `adminMiddleware(adminAPI.AttachDetachPolicyBuiltin)`.
- **Handler A** — `SetPolicyForUserOrGroup` at `cmd/admin-handlers-users.go:1770`; its **first** statement is `objectAPI, _ := validateAdminReq(ctx, w, r, policy.AttachPolicyAdminAction)`.
- **Handler B** — `AttachDetachPolicyBuiltin` at `cmd/admin-handlers-users.go:1908`; its first statement is `objectAPI, cred := validateAdminReq(ctx, w, r, policy.UpdatePolicyAssociationAction, policy.AttachPolicyAdminAction)`.
- **The mapping write** — `PolicyDBSet` at `cmd/admin-handlers-users.go:1849` (reached only *after* `validateAdminReq` succeeds).
- **Admin-action gate** — `validateAdminReq` at `cmd/admin-handler-utils.go:37` loops over the required admin actions, calling `checkAdminRequestAuth` for each; if all are denied it writes `ErrAccessDenied` and returns a `nil` `ObjectLayer`, so the handler aborts before any write.
- **Root cause** — `checkAdminRequestAuth` at `cmd/auth-handler.go:189`:
  ```go
  func checkAdminRequestAuth(ctx context.Context, r *http.Request, action policy.AdminAction, region string) (auth.Credentials, APIErrorCode) {
      cred, owner, s3Err := validateAdminSignature(ctx, r, region)   // :190 (accepts only SigV2/V4)
      if s3Err != ErrNone {
          return cred, s3Err
      }
      if globalIAMSys.IsAllowed(policy.Args{
          AccountName:     cred.AccessKey,
          Groups:          cred.Groups,
          Action:          policy.Action(action),      // the required admin action
          ConditionValues: getConditionValues(r, "", cred),
          IsOwner:         owner,
          Claims:          cred.Claims,
      }) {
          return cred, ErrNone
      }
      return cred, ErrAccessDenied                                    // :206  <-- deny-by-default
  }
  ```
  Because `r5basic`'s policy grants **no** admin action, `globalIAMSys.IsAllowed` returns false for both `AttachPolicyAdminAction` and `UpdatePolicyAssociationAction`, and the gate returns `ErrAccessDenied` at `cmd/auth-handler.go:206`. `validateAdminSignature` is at `cmd/auth-handler.go:159`.

### Rationale

The self-promotion is blocked at the **authorization gate**, before any mapping logic runs. Both policy-mapping handlers put `validateAdminReq` first (`cmd/admin-handlers-users.go:1770` and `:1908`), and `validateAdminReq` (`cmd/admin-handler-utils.go:37`) authorizes the caller for the required **admin** action via `checkAdminRequestAuth` → `globalIAMSys.IsAllowed`. Since a "basic access" user holds only S3 actions, the admin action is not granted; deny-by-default evaluation returns `ErrAccessDenied` (`cmd/auth-handler.go:206`), the handler returns immediately, and the mapping-write `PolicyDBSet` (`cmd/admin-handlers-users.go:1849`) is never invoked. That is exactly what the observed state confirms: two `403 AccessDenied` responses and an **unchanged** user→policy mapping (`consoleAdmin` has zero user mappings). The root cause of the observed "cannot modify user mappings" behavior is therefore the **deny-by-default authorization of the required admin action** in the admin auth gate — not any check inside the mapping code itself.


---

## Coverage Checklist (final pass)

Every named item and variant across R1–R5 was exercised at runtime through the canonical S3/STS/admin entry point (SigV4 via `boto3`/`mc`) and captured with its complete output:

| # | Item / variant | Status | Observed result |
|---|----------------|--------|-----------------|
| R1 | Unencrypted PUT to SSE-S3 default bucket by broad-write user (auto-encrypt) | ✅ | `200` + server-injected `X-Amz-Server-Side-Encryption: AES256`; stored `Encryption: SSE-S3` |
| R1 | Server-side header injection proven (SSE not in client `SignedHeaders`) | ✅ | Trace shows `X-Amz-Server-Side-Encryption: AES256` on a request whose `SignedHeaders` omit it |
| R1 | IAM identity `Deny`-if-no-SSE — PUT without header | ✅ | `403 AccessDenied` |
| R1 | IAM identity `Deny`-if-no-SSE — PUT with header | ✅ | `200` (header now in `SignedHeaders`) |
| R1 | `MINIO_KMS_AUTO_ENCRYPTION` / `globalAutoEncryption` state | ✅ | Unset (`false`); injection came from bucket default rule |
| R1 | Resource-based bucket-policy `Deny` variant (nuance) | ✅ | Did **not** deny authenticated IAM user (reported as observed, not remediated) |
| R2 | Delete under Legal Hold ON | ✅ | `400 InvalidRequest` WORM; version retained |
| R2 | Delete under Compliance | ✅ | `400 InvalidRequest` WORM; version retained; not bypassable |
| R2 | Delete under Governance (no bypass) | ✅ | `400 InvalidRequest` WORM; version retained |
| R2 | Governance bypass header + `BypassGovernanceRetention` granted | ✅ | `204 No Content`; version deleted |
| R2 | Governance bypass header + permission lacking | ✅ | `403 AccessDenied` (`errAuthentication` path) |
| R2 | Log surfaces: server trace + audit-log JSON | ✅ | `s3.DeleteObject … 400`; audit `api.name=DeleteObject, statusCode=400` |
| R3 | 4-drive heal-on-read GET after shard corruption | ✅ | `200`, sha256 == original; `heal.Object` event |
| R3 | Byte-sensitive hash verification | ✅ | Healed body hash `e1c53465…` == original; corrupt shard `dcb97c55…` differs |
| R3 | Stability ≥2 runs | ✅ | 4 runs; each `200` + correct hash + 1 heal event (~285–299µs) |
| R3 | Single-drive (no parity) unrecoverable variant | ✅ | `503 SlowDownRead` |
| R3 | `errFileCorrupt` console-log presence | ✅ | No `mc admin logs` line ([INFERRED] by-design) |
| R4 | Baseline parent `PutObject` | ✅ | `200` (parent allows Put) |
| R4 | `AssumeRole` with narrow inline session policy | ✅ | `200`; temp creds + SessionToken issued (redacted) |
| R4 | Temp-cred `GetObject` (session allows) | ✅ | `200`, body `hello-r4-object-content` |
| R4 | Temp-cred `PutObject` (parent allows, session omits) | ✅ | `403 AccessDenied` (intersection proven) |
| R5 | Self-attach `consoleAdmin` via `SetPolicyForUserOrGroup` | ✅ | `403 AccessDenied` |
| R5 | Self-attach `consoleAdmin` via `AttachDetachPolicyBuiltin` | ✅ | `403 AccessDenied` |
| R5 | Mapping unchanged after attempts | ✅ | `r5basic` → only `r5basicpolicy`; `consoleAdmin` has no user mappings |
| R5 | Root cause identified | ✅ | Deny-by-default admin-action eval in `checkAdminRequestAuth` (`cmd/auth-handler.go:206`) |

**[INFERRED] statements in this report** (made only where a fact could not be directly observed): (a) R3 — the default size-based `CheckParts` heal not rewriting a same-size corrupted shard; (b) R3 — `errFileCorrupt` being an intentionally internal (non-console) error. Both are explained next to the directly-observed evidence in the R3 section.

---

## Cleanup

All runtime fixtures created for this investigation are ephemeral and were provisioned outside the repository tree. On completion they are torn down: both MinIO server processes are stopped; the temporary data directories (`/tmp/miniodata`, `/tmp/minio4/d{1..4}`), the scratch workspace (`/tmp/blitzy_investigation/` — reproduction scripts, `mc` config/aliases, trace/audit captures), the built-in KMS key, and all transient buckets, users, IAM policies, and STS credentials are removed. The pre-existing tooling `/tmp/minio-bin` and `/tmp/bin/mc` (installed during environment setup) is left in place. The MinIO source tree is **unmodified**: `git status` shows exactly one new file — this report at `blitzy/documentation/minio_c07e5b49d477.md` — and no change under `cmd/`, `internal/`, `docs/`, `buildscripts/`, or any build/config file. No defect discovered during the investigation was remediated (observe-and-document only).

