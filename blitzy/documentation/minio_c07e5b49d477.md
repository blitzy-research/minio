# MinIO Read-Only Principal Authorization Boundary — An Evidence-Backed Investigation

## Introduction and Scope

This document answers a single security question empirically: **does MinIO's authorization layer correctly contain a read-only principal — one scoped to a single bucket and key prefix — so that it cannot mutate data through any "write-adjacent" S3 operation, even while the same bucket is under concurrent write and metadata traffic from other clients? And exactly what metadata can that principal learn from listing and `HEAD` behavior without ever reading object bodies?**

Every finding below was produced by **building and running MinIO from source at this branch's `HEAD`, provisioning real identities, generating real concurrent load, and probing the live server** — then quoting the observed output verbatim. Nothing here is inferred from reading code alone; the source is cited (as `file:line`) only to explain *why* the observed behavior occurs. Where a value is quoted (a status code, an error body, a header, a byte count, an ETag), it is the literal output the running system produced.

The investigation is deliberately **read-only with respect to the repository**: no source file was modified, and the only artifact added is this document. The MinIO binary, its data directory, the client tooling, and the observation scripts all lived outside the repository tree in a temporary working area (`/tmp/minio-inv`) and were removed afterward.

The question is decomposed into six sub-requirements, each answered explicitly and cross-checked in the coverage pass (§9):

| Ref | Sub-question |
|-----|--------------|
| **R1** | Stand up a read-only identity (Identity A) scoped to one bucket + prefix (object reads + prefix listing only). |
| **R2** | Have other clients (Identity B) concurrently hammer the same bucket with writes and metadata traffic. |
| **R3** | From Identity A, attempt and trace operations adjacent to writes: multipart, copy-style writes, metadata changes, deletes. |
| **R4** | Determine what metadata Identity A can learn purely from `LIST` and `HEAD`, without reading object bodies. |
| **R5** | Provide concrete runtime evidence — request/response traces and observed storage side effects. |
| **R6** | Conclude whether the read-only boundary holds, or whether a bypass hides in the corners. |

---

## 0. Verdict at a Glance

**The read-only boundary holds.** Under sustained concurrent write load, Identity A — a non-root user holding a custom policy of exactly `s3:GetObject`, `s3:ListBucket`, and `s3:GetBucketLocation` — was denied **every** write-adjacent operation with HTTP `403 AccessDenied`, and a byte-level inspection of the erasure-coded data directory proved it mutated **nothing**. The only "corners" worth calling out are three behaviors that *look* surprising but are correct and non-exploitable:

1. **Batch delete returns HTTP `200`** with a per-key `<Error><Code>AccessDenied</Code>` for each key — no object is deleted (§5.7).
2. **`PutObjectRetention` returns `400 InvalidRequest` (not `403`)** on a bucket without object-lock — a *configuration* rejection that precedes authorization, and which even the root user receives (§5.6).
3. **`ListBucket` is bucket-wide**: Identity A can enumerate the *names/sizes/ETags* of objects in prefixes it cannot read (e.g. `private/`), but cannot `HEAD` or `GET` them (§6.3).

The full reasoning and evidence for each follow.

---

## 1. Reproduction Environment (foundation for R1)

### 1.1 Building MinIO from source at HEAD

The server under test is this repository built at its current `HEAD`. The module declares its toolchain floor and its authorization dependency:

```
go.mod:3    go 1.23
go.mod:55       github.com/minio/pkg/v3 v3.0.22
```
> *Source: `go.mod:3`, `go.mod:55`*

Go 1.23.12 (satisfying the `go 1.23` floor) was installed into the temporary working area, and the server was built with all Go caches redirected outside the repository:

```
$ go version
go version go1.23.12 linux/amd64

$ go build -o /tmp/minio-inv/bin/minio .      # from the repo root
# exit 0, ~59s, binary = 156,984,473 bytes

$ /tmp/minio-inv/bin/minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

Immediately after building, `git status --porcelain` inside the repository was **empty** — the build produced no in-tree changes (the module cache is external).

### 1.2 Launching the server and confirming readiness

```
$ export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin
$ /tmp/minio-inv/bin/minio server data/disk1 data/disk2 data/disk3 data/disk4 \
      --address :9000 --console-address :9001
```

Startup log (verbatim excerpts):

```
Formatting 1st pool, 1 set(s), 4 drives per set.
...
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
```

This is a single-pool, 4-drive **erasure-coded** backend (relevant to the storage side-effect evidence in §5 and §7). Readiness was confirmed on the documented health endpoint:

```
$ curl -sS -i http://127.0.0.1:9000/minio/health/live
HTTP/1.1 200 OK
Accept-Ranges: bytes
Server: MinIO
X-Amz-Request-Id: 18BE0CEE57A035B8
...
```

---

## 2. Identity A and the Read-Only Policy (R1)

### 2.1 Why the built-in `readonly` canned policy is insufficient

MinIO installs three built-in policies at startup:

> *Source: `cmd/iam-store.go:569` (`setDefaultCannedPolicies`)*

Their action sets are defined in the vendored policy library. Critically, the `readonly` policy grants only **`GetBucketLocation` + `GetObject`** and **omits `ListBucket`**:

```
// github.com/minio/pkg/v3@v3.0.22/policy/constants.go  (readonly, lines 53-64)
Name: "readonly",
...
Actions: NewActionSet(GetBucketLocationAction, GetObjectAction),   // constants.go:60
Resources: NewResourceSet(NewResource("*")),                       // constants.go:61
```
> *Source: `github.com/minio/pkg/v3@v3.0.22/policy/constants.go:53-64` (Actions at `:60`, Resources at `:61`)*

Because R4 requires the principal to **list**, the canned `readonly` policy cannot be used — it would make listing impossible. A **custom** least-privilege policy is required. (This exactly matches MinIO's documented custom-policy recipe in `docs/multi-user/README.md`, which builds a `getonly` policy and attaches it with `mc admin policy create/attach`.)

### 2.2 The custom least-privilege policy

Identity A was bound to this policy (`ro-prefix`), scoped to one bucket (`vault`) and one prefix (`shared/`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::vault", "arn:aws:s3:::vault/shared/*"]
    }
  ]
}
```

The action strings are the literals defined in the vendored library:

| Action string | Constant | Definition |
|---------------|----------|------------|
| `s3:GetObject` | `GetObjectAction` | `policy/action.go:66` |
| `s3:ListBucket` | `ListBucketAction` | `policy/action.go:78` |
| `s3:GetBucketLocation` | `GetBucketLocationAction` | `policy/action.go:54` |
> *Source: `github.com/minio/pkg/v3@v3.0.22/policy/action.go:54,66,78`*

**The two-ARN shape is deliberate and is itself a finding (§6.3):** `arn:aws:s3:::vault` authorizes `ListBucket` at the *bucket* level (bucket-wide), while `arn:aws:s3:::vault/shared/*` authorizes `GetObject` only for keys under `shared/`. The consequence — listing sees everything, reads are prefix-locked — is characterized in §6.

### 2.3 Provisioning (via the `mc` control plane, built from source)

```
$ mc admin policy create local ro-prefix ro-prefix.json
Created policy `ro-prefix` successfully.

$ mc admin user add    local reader readerpass123
$ mc admin policy attach local ro-prefix --user=reader
$ mc admin user add    local writer writerpass123
$ mc admin policy attach local readwrite --user=writer

$ mc admin user list local
enabled    reader                ro-prefix
enabled    writer                readwrite
```

The server's own view of the stored policy (note it canonicalizes/re-orders the action list):

```
$ mc admin policy info local ro-prefix
{"PolicyName":"ro-prefix","Policy":{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
"Action":["s3:ListBucket","s3:GetBucketLocation","s3:GetObject"],
"Resource":["arn:aws:s3:::vault","arn:aws:s3:::vault/shared/*"]}]},
"CreateDate":"2026-07-01T03:34:08.195Z","UpdateDate":"2026-07-01T03:34:08.195Z"}
```

- **Identity A** = user `reader` / `readerpass123`, policy `ro-prefix` (the read-only principal under test).
- **Identity B** = user `writer` / `writerpass123`, canned `readwrite` (the concurrent writer, §3).

Making Identity A a **regular user with an attached policy** means its requests are evaluated on MinIO's canonical PBAC path (§4).

### 2.4 Seeded objects and the on-disk layout

Five objects were seeded (as root) — three under the readable `shared/` prefix and two under a sibling, non-readable `private/` prefix — each with user metadata and a content-type:

```
PUT vault/shared/report.txt         ETag="1390428cdb82fade915c54783ec10953" size=25   (text/plain,  meta: classification=public, author=alice)
PUT vault/shared/notes.md           ETag="e96a2d64bfd8f107cbd3aa85475b5902" size=20   (text/markdown, meta: reviewed=yes)
PUT vault/shared/nested/data.json   ETag="04735a9ed66041e20cbb671da6329429" size=17   (application/json, meta: schema=v2)
PUT vault/private/secret.txt        ETag="ca4f9749eabcc3e7b5e74863f1a79f85" size=32   (text/plain, meta: classification=confidential)
PUT vault/private/keys.pem          ETag="cb24d7259de4ab8fc591b64e64421cb6" size=43   (application/x-pem-file, meta: sensitivity=high)
```

On the erasure backend, **each object is stored as a directory containing an `xl.meta` descriptor** — not a flat file — replicated across all four drives:

```
$ ls -la data/disk1/vault/shared/report.txt/
drwxr-sr-x 2 root root 4096 ... .
-rw-r--r-- 1 root root  512 ... xl.meta

$ for d in 1 2 3 4; do stat -c '%s bytes' data/disk$d/vault/shared/report.txt/xl.meta; done
512 bytes
512 bytes
512 bytes
512 bytes

$ od -A d -t x1 data/disk1/vault/shared/report.txt/xl.meta | head -1
0000000 58 4c 32 20 01 00 03 00 ...        # magic "XL2 " (X L 2 space)
```

This layout is what makes the side-effect proof in §7 possible: to prove Identity A changed nothing, we checksum these `xl.meta` descriptors before and after its probes.

---

## 3. Concurrent Write / Metadata Load (R2)

While Identity A probed, Identity B (`writer`, `readwrite`) ran a **6-worker** harness hammering the *same* bucket with a continuous mix of `PutObject`, `PutObjectTagging`, `CopyObject`, multipart uploads, and `DeleteObject`. Final tallies for a 30-second run:

```
[load] starting 6 workers for 30.0s
[load] DONE in 35.1s totals={'put': 1861, 'copy': 1861, 'tag': 1861, 'del': 3722, 'mpu': 370, 'err': 0} approx 276 ops/s
```

That is **1,861 `PutObject`, 1,861 `CopyObject`, 1,861 `PutObjectTagging`, 3,722 `DeleteObject`, and 370 completed multipart uploads — 0 errors, ~276 ops/s** — sustained on `vault` while Identity A's probes executed. The concurrency is corroborated at the server in §5.8, where a storage read for a writer object (`load/w1-24.dat`) is interleaved at the same millisecond as a denied `reader` probe.

---

## 4. The Authorization Pipeline (why the outcomes below occur)

### 4.1 Request → verdict

Every authenticated S3 request passes through gate functions in `cmd/auth-handler.go`. Per-handler entry points call `checkRequestAuthType` (or the version-aware `checkRequestAuthTypeWithVID`), which validates the SigV4 signature and then evaluates the IAM policy for the required action:

```
getRequestAuthType            cmd/auth-handler.go:124
checkRequestAuthType          cmd/auth-handler.go:339
checkRequestAuthTypeWithVID   cmd/auth-handler.go:349
authenticateRequest           cmd/auth-handler.go:358
isReqAuthenticated            cmd/auth-handler.go:560
```
> *Source: `cmd/auth-handler.go:124,339,349,358,560`*

The signature check itself is constant-time (mitigating timing attacks):

```
// cmd/signature-v4.go:166
func compareSignatureV4(sig1, sig2 string) bool {
    ...
    return subtle.ConstantTimeCompare([]byte(sig1), []byte(sig2)) == 1   // signature-v4.go:169
}
```
> *Source: `cmd/signature-v4.go:166,169`*

### 4.2 Deny-by-default, with `Deny` overriding `Allow`

The IAM dispatch `IAMSys.IsAllowed` evaluates, in order: an external authZ plugin; the owner/root short-circuit; STS credentials; service accounts; and finally regular users via `PolicyDBGet` → `GetCombinedPolicy` → `IsAllowed`, returning **`false` if the user has no policies** (deny-by-default):

> *Source: `cmd/iam.go:2437` (dispatch); owner check `:2448`, STS `:2452`, service-account `:2461`, regular-user `PolicyDBGet` `:2471`, empty-policy deny `:2476`, combined evaluation `:2482`*

The decision function itself checks all `Deny` statements first and returns `false` if any matches, before considering `Allow`:

```
// github.com/minio/pkg/v3@v3.0.22/policy/policy.go:173
func (iamp Policy) IsAllowed(args Args) bool {
    // Check all deny statements. If any one statement denies, return false.
    ...
}
```
> *Source: `github.com/minio/pkg/v3@v3.0.22/policy/policy.go:173` (deny-over-allow at `:174-180`)*

A denial resolves to `ErrAccessDenied`, whose canonical mapping is:

```
// cmd/api-errors.go:539
ErrAccessDenied: {
    Code:           "AccessDenied",
    Description:    "Access Denied.",
    HTTPStatusCode: http.StatusForbidden,      // 403
},
```
> *Source: `cmd/api-errors.go:539-542`*

Because Identity A's policy contains only `GetObject`, `ListBucket`, and `GetBucketLocation`, it holds **none** of the write-adjacent actions, so deny-by-default rejects each one. The next section proves it does.

### 4.3 Enforcement map (operation → required action → line)

| S3 operation | Required `policy.Action` | Enforcement line |
|--------------|--------------------------|------------------|
| CreateMultipartUpload | `s3:PutObject` | `cmd/object-multipart-handlers.go:83` |
| UploadPart | `s3:PutObject` | `cmd/object-multipart-handlers.go:667` |
| UploadPartCopy | `s3:PutObject` (dst) + `s3:GetObject` (src) | `:268` / `:301` |
| CompleteMultipartUpload | `s3:PutObject` | `cmd/object-multipart-handlers.go:927` |
| AbortMultipartUpload | `s3:AbortMultipartUpload` | `cmd/object-multipart-handlers.go:1118` |
| ListParts | `s3:ListMultipartUploadParts` | `cmd/object-multipart-handlers.go:1162` |
| ListMultipartUploads | `s3:ListBucketMultipartUploads` | `cmd/bucket-handlers.go:265` |
| CopyObject | `s3:PutObject` (dst) + `s3:GetObject` (src) | `cmd/object-handlers.go:1173` / `:1206` |
| PutObject | `s3:PutObject` | `cmd/object-handlers.go:1836` |
| PutObjectTagging | `s3:PutObjectTagging` | `cmd/object-handlers.go:3151` |
| DeleteObjectTagging | `s3:DeleteObjectTagging` | `cmd/object-handlers.go:3301` |
| PutObjectLegalHold | `s3:PutObjectLegalHold` | `cmd/object-handlers.go:2718` |
| DeleteObject | `s3:DeleteObject` | `cmd/object-handlers.go:2528` |
| DeleteObjects (batch) | `s3:DeleteObject` (per object) | `cmd/bucket-handlers.go:505` |
| HeadObject | `s3:GetObject` | `cmd/object-handlers.go:760` / `:845` |
| HeadBucket | `s3:ListBucket` | `cmd/bucket-handlers.go:1658` |
| ListObjectsV2 | `s3:ListBucket` | `cmd/bucket-listobjects-handlers.go:172` |
| ListObjectsV1 | `s3:ListBucket` | `cmd/bucket-listobjects-handlers.go:287` |
| GetBucketLocation | `s3:GetBucketLocation` | `cmd/bucket-handlers.go:218` |
> *Sources: as cited per row; action literals at `github.com/minio/pkg/v3@v3.0.22/policy/action.go:32,51,54,66,78,87,98,116,167`.*

---

## 5. Write-Adjacent Probes from Identity A (R3)

### 5.1 Method

Each probe was issued as `reader` (Identity A) using a raw SigV4-signed HTTP client (botocore's `S3SigV4Auth` + `requests`), so the exact request line, status, headers, and body could be captured verbatim. Every probe below ran **while Identity B's load (§3) was in flight**. To make the multipart denials unambiguous, a **real, in-progress** multipart upload was first created by `writer`:

```
[setup] writer created LIVE multipart upload:
        key=shared/live-mpu.bin
        uploadId=ZmU2ZjIyOTUtYmYwMS00MDFmLTljZWYtMjljMDU3MGM5MDc0Lj...x178287718435804766
```

Identity A then attacked that **real** `uploadId` — so a `403` cannot be dismissed as "no such upload."

### 5.2 Baseline write — `PutObject` (in-prefix and out-of-prefix)

```
PUT /vault/shared/hack.txt              →  HTTP 403 Forbidden
PUT /vault/private/hack.txt             →  HTTP 403 Forbidden
```
Verbatim body (in-prefix case):
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/hack.txt</Key><BucketName>vault</BucketName><Resource>/vault/shared/hack.txt</Resource><RequestId>18BE0D5A6423BCE2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```
Being *inside* the readable prefix confers no write ability — `GetObject` on `shared/*` does not imply `PutObject`.

### 5.3 Multipart — all seven operations, against a real live upload

```
POST /vault/shared/x.bin?uploads                                  →  403 AccessDenied   (CreateMultipartUpload)
PUT  /vault/shared/live-mpu.bin?partNumber=1&uploadId=<LIVE>       →  403 AccessDenied   (UploadPart)
PUT  /vault/shared/live-mpu.bin?partNumber=1&uploadId=<LIVE>       →  403 AccessDenied   (UploadPartCopy, x-amz-copy-source)
POST /vault/shared/live-mpu.bin?uploadId=<LIVE>                    →  403 AccessDenied   (CompleteMultipartUpload)
DELETE /vault/shared/live-mpu.bin?uploadId=<LIVE>                  →  403 AccessDenied   (AbortMultipartUpload)
GET  /vault?uploads                                               →  403 AccessDenied   (ListMultipartUploads)
GET  /vault/shared/live-mpu.bin?uploadId=<LIVE>                    →  403 AccessDenied   (ListParts)
```

Two observations: (a) Identity A **cannot even begin** an upload (`CreateMultipartUpload` → 403), and (b) it cannot touch a writer's live upload — including **aborting** it. The authorization check fires *before* the upload's existence is consulted (the `403` is `AccessDenied`, not `NoSuchUpload`). The `ListMultipartUploads` error body carries no `<Key>` and `<Resource>/vault</Resource>` (a bucket-level operation):

```xml
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>vault</BucketName><Resource>/vault</Resource>...</Error>
```

### 5.4 Copy-style writes (including the metadata `REPLACE` directive)

```
PUT /vault/shared/copy.txt      (x-amz-copy-source: /vault/shared/report.txt)                          →  403 AccessDenied
PUT /vault/shared/report.txt    (x-amz-copy-source: .../report.txt, x-amz-metadata-directive: REPLACE, →  403 AccessDenied
                                 x-amz-meta-injected: by-reader)
```
Even though Identity A *can read* the copy **source** (`shared/report.txt`), the copy is denied because the destination requires `s3:PutObject`. The `REPLACE` variant — an attempt to rewrite an object's user metadata in place — is likewise denied.

### 5.5 Metadata mutation — tagging

```
PUT    /vault/shared/report.txt?tagging   →  403 AccessDenied   (PutObjectTagging)
DELETE /vault/shared/report.txt?tagging   →  403 AccessDenied   (DeleteObjectTagging)
```

### 5.6 Object-lock — the legal-hold vs. retention divergence

This is the subtlest corner, and the two operations behave **differently** — which is correct and worth understanding.

**`PutObjectLegalHold` → `403 AccessDenied`.** Its handler checks authorization *first*, before any object-lock configuration gate:

```
// cmd/object-handlers.go:2698 PutObjectLegalHoldHandler
if s3Err := checkRequestAuthType(ctx, r, policy.PutObjectLegalHoldAction, bucket, object); s3Err != ErrNone {   // :2718
    ...
}
...
if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {   // :2733
```
> *Source: `cmd/object-handlers.go:2698,2718,2733`*

Observed (with and without a `Content-MD5` header — identical result):
```
PUT /vault/shared/report.txt?legal-hold   →  HTTP 403 Forbidden
<Error><Code>AccessDenied</Code>...<Key>shared/report.txt</Key>...</Error>
```

**`PutObjectRetention` → `400 InvalidRequest` (not `403`).** Its handler performs **no entry-level policy action check**; it only *authenticates the signature*, then requires `Content-MD5`, then requires object-lock to be enabled:

```
// cmd/object-handlers.go:2855 PutObjectRetentionHandler
cred, owner, s3Err := validateSignature(getRequestAuthType(r), r)          // :2874  (authentication only)
...
if !hasContentMD5(r.Header) { ... ErrMissingContentMD5 ... }               // :2886
if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {   // :2890
    ... ErrInvalidBucketObjectLockConfiguration ...                        // :2891
}
```
> *Source: `cmd/object-handlers.go:2855,2874,2886,2890-2891`; bucket lock config via `cmd/bucket-object-lock.go:38`*

Observed, once a valid `Content-MD5` is supplied (the bucket `vault` has no object lock):
```
PUT /vault/shared/report.txt?retention   →  HTTP 400 Bad Request
<Error><Code>InvalidRequest</Code><Message>Bucket is missing ObjectLockConfiguration</Message>...</Error>
```
> *Error mapping: `cmd/api-errors.go:919` (`ErrInvalidBucketObjectLockConfiguration` → `Code:"InvalidRequest"`, 400). Without `Content-MD5`, the earlier gate returns `Code:"MissingContentMD5"`, 400 — `cmd/api-errors.go:624`.*

**Proof this is a *configuration* rejection, not an authorization decision:** the **root** user, issuing the identical request, receives the **same** `400 InvalidRequest`:
```
PUT /vault/shared/report.txt?retention   (as root)   →  HTTP 400  <Code>InvalidRequest</Code><Message>Bucket is missing ObjectLockConfiguration</Message>
```
So retention on a non-lock bucket never reaches an authorization verdict for *anyone*; it is rejected on configuration grounds. Identity A gains nothing from it. (This is the distinction the investigation was required to preserve: a config rejection is not an `AccessDenied`.)

### 5.7 Deletes — single, and the batch partial-result corner case

**Single delete** is a clean `403`:
```
DELETE /vault/shared/report.txt   →  HTTP 403 Forbidden
<Error><Code>AccessDenied</Code>...<Key>shared/report.txt</Key>...</Error>
```

**Batch delete is the notable corner.** `DeleteMultipleObjectsHandler` intentionally **ignores** the top-level auth check's verdict — it calls it only to populate the request's access key for the bucket lookup — and authorizes **per object** inside the loop:

```
// cmd/bucket-handlers.go
// Call checkRequestAuthType to populate ReqInfo.AccessKey before GetBucketInfo()
// Ignore errors here to preserve the S3 error behavior of GetBucketInfo()
checkRequestAuthType(ctx, r, policy.DeleteObjectAction, bucket, "")                                  // :471 (verdict discarded)
...
if checkRequestAuthTypeWithVID(ctx, r, policy.DeleteObjectAction, bucket, object.ObjectName, object.VersionID) != ErrNone {   // :505 (authoritative)
```
> *Source: `cmd/bucket-handlers.go:471` (verdict ignored), `:505` (authoritative per-object check)*

The observed result is an HTTP **`200`** whose body contains a per-key `AccessDenied` for every requested key — including a key in the readable prefix (`shared/report.txt`), a second in it (`shared/notes.md`), and one outside it (`private/secret.txt`):

```
POST /vault?delete   →  HTTP 200 OK
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/report.txt</Key><VersionId></VersionId></Error><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/notes.md</Key><VersionId></VersionId></Error><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>private/secret.txt</Key><VersionId></VersionId></Error></DeleteResult>
```

A `200` status here can *look* alarming, but it is S3-correct: the batch API reports per-object outcomes in the body, and **every object was denied and none was removed** (proven at the byte level in §7). No object slipped through under load.

### 5.8 Server-side corroboration (the decision, as the server logged it)

`mc admin trace` captured the server's own view of a denied `reader` probe. The request-id matches the client-side capture in §5.2 exactly (`18BE0D5A6423BCE2`), and a concurrent storage read for a **writer** object (`load/w1-24.dat`) appears at the same timestamp — evidence the denial happened *under load*:

```
[REQUEST s3.PutObject] [2026-07-01T03:39:44.363] [Client IP: 127.0.0.1]
PUT /vault/shared/hack.txt
Authorization: AWS4-HMAC-SHA256 Credential=reader/20260701/us-east-1/s3/aws4_request, ...
User-Agent: python-requests/2.34.2
[RESPONSE] [2026-07-01T03:39:44.363] [ Duration 143µs TTFB 135.106µs ↑ 111 B  ↓ 331 B ]
403 Forbidden
X-Amz-Request-Id: 18BE0D5A6423BCE2
...
# interleaved, same millisecond:
[STORAGE storage.ReadXL] [2026-07-01T03:39:44.363] .../vault load/w1-24.dat ... 29.061µs 477 B
```

### 5.9 Write-adjacent probe summary

Every probe status, verbatim from the run:

```
HTTP 403  [AccessDenied]      PutObject shared/hack.txt (in-prefix)
HTTP 403  [AccessDenied]      PutObject private/hack.txt (out-of-prefix)
HTTP 403  [AccessDenied]      CreateMultipartUpload shared/x.bin
HTTP 403  [AccessDenied]      UploadPart -> REAL uploadId
HTTP 403  [AccessDenied]      UploadPartCopy -> REAL uploadId
HTTP 403  [AccessDenied]      CompleteMultipartUpload -> REAL uploadId
HTTP 403  [AccessDenied]      AbortMultipartUpload -> REAL uploadId
HTTP 403  [AccessDenied]      ListMultipartUploads
HTTP 403  [AccessDenied]      ListParts -> REAL uploadId
HTTP 403  [AccessDenied]      CopyObject shared/report.txt -> shared/copy.txt
HTTP 403  [AccessDenied]      CopyObject onto report.txt (metadata REPLACE)
HTTP 403  [AccessDenied]      PutObjectTagging shared/report.txt
HTTP 403  [AccessDenied]      DeleteObjectTagging shared/report.txt
HTTP 403  [AccessDenied]      PutObjectLegalHold shared/report.txt
HTTP 400  [InvalidRequest]    PutObjectRetention shared/report.txt   (lock-config gate; see §5.6)
HTTP 403  [AccessDenied]      DeleteObject shared/report.txt
HTTP 200  [per-key AccessDenied]  DeleteObjects BATCH (no object removed; see §5.7)
```

Every write-adjacent operation was either denied (`403 AccessDenied`), rejected on configuration grounds before authorization (`400 InvalidRequest`, retention), or returned a `200` batch envelope in which **each** key was individually denied. **Not one mutated state.**

---

## 6. Metadata Disclosure via LIST and HEAD (R4)

All of §6 was obtained **without a single `GetObject` body read** used for metadata — the disclosure comes purely from `LIST` responses and `HEAD` headers.

### 6.1 What `LIST` discloses

`ListObjectsV2` (bucket-wide) succeeds for Identity A (it holds `s3:ListBucket`) and returns, for every object: **key, last-modified, ETag, size, storage class**, and — with `fetch-owner=true` — **owner ID and display name**:

```
GET /vault?list-type=2&fetch-owner=true   →  HTTP 200,  <KeyCount>375</KeyCount>
```
```xml
<Contents><Key>load/mp-w0-10.bin</Key><LastModified>2026-07-01T03:40:56.916Z</LastModified><ETag>"dd1cfa26e692566b61ab94407c987714-1"</ETag><Size>5242880</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass></Contents>
```
(The `375` keys are the 5 seed objects plus 370 completed multipart objects left by the writer load — Identity A sees them all.) A multipart-produced object is even distinguishable by its ETag suffix (`...-1`) and 5 MiB size.

`ListObjectsV1` returns the same per-object fields plus a `<Marker>` element. A delimiter list exposes the prefix structure:

```
GET /vault?list-type=2&delimiter=/   →  200
<CommonPrefixes><Prefix>load/</Prefix></CommonPrefixes>
<CommonPrefixes><Prefix>private/</Prefix></CommonPrefixes>
<CommonPrefixes><Prefix>shared/</Prefix></CommonPrefixes>
```

### 6.2 What `HEAD` discloses

`HeadObject` on an object **inside** the readable prefix succeeds and returns rich per-object metadata in headers — content type, length, ETag, last-modified, and **all `x-amz-meta-*` user metadata** — with an empty body:

```
HEAD /vault/shared/report.txt   →  HTTP 200 OK
Content-Type: text/plain
Content-Length: 25
ETag: "1390428cdb82fade915c54783ec10953"
Last-Modified: Wed, 01 Jul 2026 03:35:45 GMT
Accept-Ranges: bytes
x-amz-meta-classification: public
x-amz-meta-author: alice
```
```
HEAD /vault/shared/nested/data.json   →  HTTP 200 OK
Content-Type: application/json
Content-Length: 17
ETag: "04735a9ed66041e20cbb671da6329429"
x-amz-meta-schema: v2
```
(No `x-amz-version-id` appears — the bucket is unversioned; `STANDARD` storage class is not emitted as an explicit header.)

`HeadBucket` succeeds (it requires `s3:ListBucket`), and `GetBucketLocation` returns the empty location constraint (i.e. `us-east-1`):

```
HEAD /vault           →  HTTP 200,  Content-Length: 0
GET  /vault?location  →  HTTP 200,  <LocationConstraint xmlns="..."></LocationConstraint>
```

### 6.3 The prefix-scope nuance: LIST is bucket-wide, HEAD/GET are prefix-locked

This is the key subtlety of the two-ARN policy (§2.2). Because `s3:ListBucket` is granted at the **bucket** ARN, Identity A can enumerate the *names, sizes, ETags, timestamps, and owner* of objects in the `private/` prefix **that it cannot read**:

```
# via boto3 as reader
list_objects_v2(prefix='private/', fetch-owner)   →  HTTP 200, KeyCount=2
   private/keys.pem     Size=43  ETag="cb24d7259de4ab8fc591b64e64421cb6"  StorageClass=STANDARD  Owner=minio
   private/secret.txt   Size=32  ETag="ca4f9749eabcc3e7b5e74863f1a79f85"  StorageClass=STANDARD  Owner=minio
```

But **`HEAD` and `GET` on those same `private/` objects are denied**, because both require `s3:GetObject`, which is scoped to `shared/*`:

```
HEAD /vault/private/secret.txt   →  HTTP 403 Forbidden,  Content-Length: 0
GET  /vault/private/secret.txt   →  HTTP 403 Forbidden
    <Error><Code>AccessDenied</Code>...<Key>private/secret.txt</Key>...</Error>

# and, for completeness, reads WITHIN the prefix succeed:
GET  /vault/shared/report.txt    →  HTTP 200,  body = 'quarterly report body v1\n' (25 bytes)
```

So the disclosure surface is asymmetric and must be reasoned about deliberately when designing least-privilege policies:

- **Breadth (LIST):** every key name, size, ETag, last-modified, storage class, and owner **across the whole bucket**, including prefixes the principal cannot read.
- **Depth (HEAD):** full per-object metadata including `x-amz-meta-*` user metadata and content-type — **but only within the readable prefix**.
- **Bodies (GET):** only within the readable prefix; never disclosed elsewhere.

### 6.4 Disclosure inventory

| Field | Disclosed by LIST? | Disclosed by HEAD? | Notes |
|-------|:---:|:---:|-------|
| Object key / name | ✅ (bucket-wide) | — | incl. non-readable prefixes |
| Size / Content-Length | ✅ | ✅ | |
| ETag | ✅ | ✅ | multipart ETags show `-N` part suffix |
| Last-Modified | ✅ | ✅ | |
| Storage class | ✅ | (implicit) | `STANDARD` |
| Owner (ID + display name) | ✅ (`fetch-owner`) | — | |
| Content-Type | — | ✅ | e.g. `text/plain`, `application/json` |
| User metadata `x-amz-meta-*` | — | ✅ | **only within readable prefix** |
| Common prefixes (structure) | ✅ (`delimiter`) | — | `load/ private/ shared/` |
| Object body | ❌ (never) | ❌ (never) | HEAD body is empty by definition |

---

## 7. Storage Side-Effects (R5)

Traces prove what the *server said*; this section proves what the *disk did* — that Identity A's probes changed nothing.

### 7.1 The seed objects are byte-identical before and after

A checksum-of-checksums over all `xl.meta` descriptors of the seed objects (across all four drives) was taken at baseline and again after every read-only probe:

```
# baseline (right after seeding):  md5-of-md5s = 2a5308c2897fcc0e9150daf99bad4ff7   (20 files)
$ find data/disk*/vault/shared data/disk*/vault/private -name xl.meta | sort | xargs md5sum | md5sum
2a5308c2897fcc0e9150daf99bad4ff7  -                # after probes: IDENTICAL (count=20)
```

The digests match exactly — the five seed objects were **not** modified, re-tagged, re-metadata'd, or deleted. Admin corroboration on the object Identity A attacked most (`shared/report.txt`):

```
$ mc tag list local/vault/shared/report.txt
No tags found                                       # the denied PutObjectTagging added nothing

$ mc stat local/vault/shared/report.txt
Name      : report.txt
Size      : 25 B
ETag      : 1390428cdb82fade915c54783ec10953        # == seed ETag
Metadata  :
  X-Amz-Meta-Author        : alice                 # unchanged; the denied CopyObject-REPLACE
  X-Amz-Meta-Classification: public                 # did NOT inject x-amz-meta-injected
```

### 7.2 No object was created at any attempted path; no orphaned multipart parts

```
# none of Identity A's attempted write targets exist on disk:
shared/hack.txt      -> 0 entries (absent)
private/hack.txt     -> 0 entries (absent)
shared/x.bin         -> 0 entries (absent)
shared/copy.txt      -> 0 entries (absent)
shared/live-mpu.bin  -> 0 entries (absent)          # writer's upload was aborted; no residue

# the multipart staging area is empty on every drive — the denied multipart attempts left nothing:
disk1 .minio.sys/multipart entries: 0
disk2 .minio.sys/multipart entries: 0
disk3 .minio.sys/multipart entries: 0
disk4 .minio.sys/multipart entries: 0
```

This matters because uploaded-but-uncompleted multipart parts are invisible to a normal object listing and would persist in storage. Here there is nothing to persist — Identity A could not even initiate an upload (§5.3), and the direct inspection of `.minio.sys/multipart/` confirms zero residue.

### 7.3 Summary

Across the entire probe campaign, run under concurrent write load, Identity A produced **no create, no modify, no delete, and no orphaned part**. The storage layer's own bytes agree with the `403`/`AccessDenied` traces.

---

## 8. Verdict (R6)

**The read-only boundary holds. No bypass hides in the corners.**

- Identity A, holding only `s3:GetObject` + `s3:ListBucket` + `s3:GetBucketLocation` scoped to `vault` and `vault/shared/*`, was **denied every write-adjacent operation**: `PutObject`, all seven multipart operations (including attempts against a *real* live upload owned by another client), `CopyObject` (including the metadata-`REPLACE` variant), `PutObjectTagging`/`DeleteObjectTagging`, `PutObjectLegalHold`, and single `DeleteObject` were rejected with HTTP `403 AccessDenied`, while every key of a batch `DeleteObjects` was individually denied inside a `200` envelope that removed nothing (§5).
- These denials held **under sustained concurrent write/metadata load** (≈276 ops/s across 6 workers), and the server's own trace confirmed the deny decision at the same instant a writer operation was in flight (§5.8).
- A **byte-level inspection** of the erasure-coded data directory proved Identity A mutated nothing: the seed objects' `xl.meta` descriptors were unchanged, no object appeared at any attempted path, and the multipart staging area was empty (§7).
- The three "corners" are correct, explained, and non-exploitable: batch delete's `200`-with-per-key-`AccessDenied` envelope removes nothing (§5.7); retention's `400 InvalidRequest` is a configuration rejection that precedes authorization and applies even to root (§5.6); and `ListBucket`'s bucket-wide scope discloses object *names/sizes/ETags* outside the readable prefix but never their user metadata or bodies (§6.3).

The one **operational caveat** — not a MinIO defect, but a policy-design consideration — is the LIST/HEAD asymmetry: a bucket-scoped `s3:ListBucket` grant lets a read-only principal enumerate the existence, size, and ETag of every object in the bucket, including prefixes it cannot read. Operators who need to hide even the *names* of objects outside the readable prefix must constrain listing with an `s3:prefix` condition rather than granting bucket-wide `ListBucket`. Within the stated least-privilege model, however, the containment is complete: **a principal that should be read-only is, in fact, read-only.**

---

## 9. Coverage Pass (R1–R6)

| Ref | Requirement | Where answered | Result |
|-----|-------------|----------------|--------|
| **R1** | Read-only identity scoped to bucket + prefix | §2 | `reader` bound to custom `ro-prefix` (`GetObject`+`ListBucket`+`GetBucketLocation` on `vault` + `vault/shared/*`); canned `readonly` rejected because it omits `ListBucket` (`constants.go:53-64`). |
| **R2** | Concurrent writer/metadata load | §3 | 6-worker `writer` harness: 1,861 put / 1,861 copy / 1,861 tag / 3,722 delete / 370 multipart, 0 errors, ~276 ops/s. |
| **R3** | Write-adjacent probes traced | §5 | 13 operations → `403 AccessDenied`; retention → `400 InvalidRequest` (config gate); batch delete → `200` with per-key `AccessDenied`. Verbatim bodies + server trace. |
| **R4** | Metadata learnable via LIST/HEAD, no body read | §6 | Full inventory table; the bucket-wide-LIST vs prefix-locked-HEAD asymmetry characterized. |
| **R5** | Runtime evidence: traces + storage side effects | §5, §7 | Verbatim request/response traces; `xl.meta` md5 identical before/after; no created paths; empty multipart staging; admin re-list. |
| **R6** | Boundary verdict | §8 | Boundary holds; three corners explained; one policy-design caveat (bucket-wide listing). |

---

## Appendix A — Environment and Tooling

| Component | Version / value |
|-----------|-----------------|
| MinIO server | built from source at this `HEAD`; reports `DEVELOPMENT.GOGET`, `go1.23.12 linux/amd64` |
| Go toolchain | `go1.23.12 linux/amd64` (satisfies `go 1.23` floor, `go.mod:3`) |
| Authorization library | `github.com/minio/pkg/v3 v3.0.22` (vendored; `go.mod:55`) — unmodified |
| Backend | single pool, 1 set, 4 drives/set (erasure-coded); objects stored as `xl.meta` directories |
| Endpoint / health | `http://127.0.0.1:9000`; `GET /minio/health/live` → `200` |
| S3 client | `boto3`/`botocore` 1.43.36 (SigV4); raw `S3SigV4Auth` + `requests` 2.34.2 for verbatim wire capture |
| Control plane | `mc` (built from source) for `admin policy`/`user`; `mc admin trace` for server-side corroboration |
| Root credentials | `MINIO_ROOT_USER=minioadmin` / `MINIO_ROOT_PASSWORD=minioadmin` (disposable, local) |

> Note on tooling versions: the running `boto3`/`botocore` was **1.43.36** (as observed), and `mc`, built from `@latest`, bootstrapped a newer Go for its own compilation — neither affects the system under test, which is the source-built MinIO server on `go1.23.12`.

## Appendix B — Observation scripts (ephemeral, external to the repository)

All scripts lived under `/tmp/minio-inv` and were removed after the evidence was captured; none is part of the repository:

- `seed.py` — seeds the five objects with user metadata (as root).
- `probe_lib.py` — raw SigV4 signer/sender that returns the exact status, headers, and body.
- `writer_load.py` — the multi-worker Identity B load harness (§3).
- `probe_readonly.py` — the write-adjacent probe matrix run as Identity A (§5), which first creates a real live multipart upload as `writer`.
- `r4_disclosure.py` — the LIST/HEAD disclosure characterization (§6).

## Appendix C — Note on source-citation accuracy for object-lock handlers

Two enforcement details were verified directly against the source at this `HEAD` and are stated precisely in §5.6, because they are easy to get wrong:

- `PutObjectLegalHoldHandler` (`cmd/object-handlers.go:2698`) checks `policy.PutObjectLegalHoldAction` at `:2718` **before** the lock-config gate at `:2733` → a read-only principal gets `403 AccessDenied` even on a non-lock bucket.
- `PutObjectRetentionHandler` (`cmd/object-handlers.go:2855`) performs **no** entry-level policy action check; it authenticates the signature (`:2874`), requires `Content-MD5` (`:2886`), then requires lock to be enabled (`:2890-2891`) → on a non-lock bucket it returns `400 InvalidRequest` ("Bucket is missing ObjectLockConfiguration"), **not** `403` — for any identity, including root. The object-level retention authorization occurs later, only once lock configuration exists.
