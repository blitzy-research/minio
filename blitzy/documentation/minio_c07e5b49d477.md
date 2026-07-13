# Does MinIO's read-only S3 authorization boundary hold under concurrent write/metadata load?

**A runtime, evidence-backed security investigation of the MinIO object storage server (`github.com/minio/minio`, branch `minio_c07e5b49d477`, HEAD `c07e5b49d`, Go module `go 1.23` [go.mod:L3]).**

This document answers the following question, decomposed and answered **by name** for every operation family it enumerates:

> One identity has read-only access to a bucket+prefix; other clients hammer the same bucket with writes and metadata traffic; then show what actually happens when the read-only identity attempts write-adjacent operations — **multipart operations, copy-style writes, metadata changes, deletes** — plus what metadata can be learned from **listing and HEAD** without full reads, backed by **runtime request/response traces** and **observed storage side effects**.

Everything below was produced by **building and running the canonical server**, provisioning real identities through the canonical admin path, generating real concurrent load, and probing every named operation through the **real S3 API** with raw SigV4-signed HTTP. Each behavioral claim is placed next to the actual observed output (HTTP status + complete, unedited S3 XML), the on-disk storage state, and the audit-log record, and is grounded in `file:line` references naming the specific function that performs the work.

> **Note on reproducibility.** Request IDs and timestamps below are from **this** run. `HostId` observed: `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`. Any value labeled *inferred* is called out explicitly; everything else is observed.

---

## 1. Direct answer / verdict

**The read-only authorization boundary HOLDS — completely and deterministically — under concurrent write/metadata load.** Across a 12-second window of 32 concurrent read-write clients issuing **807 `PutObject` + 704 `PutObjectTagging`** operations against the same bucket, the read-only identity (`rouser`) was **denied every single write-adjacent operation it attempted**, and every denial left **zero storage side effects**. The outcome was **identical across 16 probe-matrix passes within one run and across a second independent invocation** (all 31 distinct probes matched on status **and** error code).

The result rests on three mechanisms, each proven at runtime and grounded in code:

1. **Deny-by-default PBAC.** The terminal authorization decision is `IAMSys.IsAllowed` [cmd/iam.go:L2437]. For a regular user it fetches attached policies with `PolicyDBGet`; if none match the requested action it falls through to **`return false`** ([cmd/iam.go:L2476] `if len(policies) == 0`, else [cmd/iam.go:L2482] `GetCombinedPolicy(policies...).IsAllowed(args)`). The read-only policy grants only `s3:GetObject` (bucket+prefix) and `s3:ListBucket` (bucket) — it grants **none** of the write-adjacent actions, so each is denied.
2. **A distinct action guards each operation.** Every write-adjacent operation checks a **different** `policy.Action` *in its handler, before the object layer runs* — e.g. multipart create → `PutObjectAction` [cmd/object-multipart-handlers.go:L83], single delete → `DeleteObjectAction` [cmd/object-handlers.go:L2528], tagging → `PutObjectTaggingAction` [cmd/object-handlers.go:L3151]. Because the guard precedes any namespace lock or shard write, a correct denial cannot mutate storage.
3. **Concurrency and TOCTOU never downgrade the decision.** The decision is computed from the request's fixed *action + resource + principal* before the object layer executes, so there is no check-vs-use window. Probing the **exact key** other clients were actively mutating produced the same `403 AccessDenied` as under quiescence.

The three independent evidence streams captured for every result are:

| Stream | What it proves | Source |
|--------|----------------|--------|
| **Wire trace** | The server returned `HTTP 403` + S3 `AccessDenied` XML to the read-only principal | raw SigV4 HTTP response (status + full body + `X-Amz-Request-Id`) |
| **Storage side-effect** | The denied write created/mutated nothing on the erasure backend | `<datadir>/testbucket/<object>/xl.meta`, part files, `.minio.sys/multipart` |
| **Audit-log denial** | The server logged the denial with principal/action/resource | `logger.AuditLog` [cmd/auth-handler.go:L636] → webhook sink |

**Information-disclosure nuance (answered in full in §6):** listing and HEAD are *read* surfaces the read-only identity legitimately holds. `HeadObject`/`GetObject` inside the prefix succeed and disclose size, ETag, content-type, timestamp and user metadata; outside the prefix they are `403`. Crucially, `s3:ListBucket` is **bucket-scoped**, so `ListObjectsV2`/`ListObjectsV1` **without a prefix** (or with a prefix outside the grant) **do disclose keys outside the granted prefix** (including `private/secret.txt`) — an information-disclosure property to be aware of, mitigable with an `s3:prefix` `Condition`. `ListMultipartUploads` is itself **denied** (it needs `ListBucketMultipartUploadsAction`).

The remainder of this document proves each of these claims with complete, unedited runtime output.

---

## 2. Exact build & invocation commands (default, canonical configuration)

**Toolchain:** Go **1.23.12** (`linux/amd64`) — the highest documented `1.23.x`, consistent with `go 1.23` [go.mod:L3] and the CI pins.

**Build (verbatim command run):**

```bash
CGO_ENABLED=0 go build -o /tmp/minio-bin .
```

Observed: exit `0`; binary **156,743,642 bytes (~150 MB)**; the build resolved all modules from the committed `go.sum` **without modifying `go.mod`/`go.sum`** (both files' SHA-256 were byte-for-byte identical before and after: `go.mod` `b85e6896…`, `go.sum` `184a7add…`). Because a plain `go build` sets no linker flags, the binary self-reports the expected development version string:

```
$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
```

The output binary was written to `/tmp` (outside the repository) so no build artifact dirties the working tree.

**Run (verbatim invocation) — single-node, default root credentials:**

```bash
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  /tmp/minio-bin server /tmp/minio-data --address :9000 --console-address :9001
```

The datadir `/tmp/minio-data` is a single-node erasure backend under `/tmp` (outside the repo). To capture the third evidence stream, a standard **audit webhook** was enabled — a canonical audit target, not a bypass — pointing at a tiny local HTTP sink that appends each event to a file:

```bash
export MINIO_AUDIT_WEBHOOK_ENABLE_probe=on
export MINIO_AUDIT_WEBHOOK_ENDPOINT_probe=http://127.0.0.1:9099/
```

**Server startup log (excerpt) — confirms the API address and the default-credential warning:**

```
API: http://10.236.6.173:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these
      values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

**Health checks (both HTTP 200):**

```bash
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/live
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
```

**Anonymous baseline — proves the denial trace shape.** An unauthenticated `GET /` returns `HTTP 403` with the standard S3 `AccessDenied` XML envelope (the same envelope shape every denied write-adjacent operation below produces):

```bash
$ curl -s http://127.0.0.1:9000/
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Resource>/</Resource><RequestId>18C1E7129E0DB95E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

This anonymous request is itself audit-logged as `api=ListBuckets accessKey=None statusCode=403`, confirming the audit stream is live before provisioning.

> **Environment note (honest, observed):** `/tmp` here is a real disk (`ext4` on an NVMe-backed virtual block device) with a measured **~169 ms/fsync** latency. Because MinIO's erasure backend fsyncs `xl.meta` + data on every write, single-node write throughput is fsync-bound (measured ceiling ~124–222 `PutObject`/s at 32 workers). This affects only the *magnitude* of the background load (§4), not any authorization decision. All values below are what this default build actually produced.

---

## 3. Identity & policy provisioning (canonical admin path only)

`mc` (the MinIO Client) is **not installed** in this environment, so provisioning was done through the **`madmin` SDK** (`github.com/minio/madmin-go/v3 v3.0.77`), which drives the exact same admin REST endpoints `mc admin` does. These are the canonical entry points:

- `AddUser` — `adminAPIHandlers.AddUser` [cmd/admin-handlers-users.go:L444]
- `AddCannedPolicy` — `adminAPIHandlers.AddCannedPolicy` [cmd/admin-handlers-users.go:L1701]
- `SetPolicyForUserOrGroup` — `adminAPIHandlers.SetPolicyForUserOrGroup` [cmd/admin-handlers-users.go:L1770]

No on-disk config was edited; no debug hook, mock, or synthetic bypass was used. This mirrors the canonical `start → ready → add user → create policy → attach → exercise → assert → cleanup` sequence in the repository's own PBAC test harness `docs/iam/policies/pbac-tests.sh`, and the `getonly.json` provisioning convention in `docs/multi-user/README.md`.

**Two privilege tiers were provisioned:**

- **Read-write "stress" identity `rwuser`** → built-in `readwrite` canned policy. Its shape is confirmed in the repository at [cmd/sts-handlers_test.go:L830]: `{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}`. This identity generates the concurrent load.
- **Read-only identity `rouser`** → a **custom** bucket+prefix PBAC policy named `readonly-bp`.

**Why the built-in `readonly` policy is insufficient.** The canned `readonly` policy — verified verbatim at [cmd/sts-handlers_test.go:L830] — is:

```json
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetBucketLocation","s3:GetObject"],"Resource":["arn:aws:s3:::*"]}]}
```

It grants read on `arn:aws:s3:::*` — **every** bucket — and does **not** scope to a bucket+prefix. The user's scenario requires a genuinely constrained principal, so the read scope must be authored explicitly.

**The custom bucket+prefix policy `readonly-bp` (verbatim, exactly as created via `AddCannedPolicy`):**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["s3:GetObject"],  "Resource": ["arn:aws:s3:::testbucket/shared/*"] },
    { "Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::testbucket"] }
  ]
}
```

The `s3:GetObject` statement is modeled on the `getonly.json` pattern from `docs/multi-user/README.md` (which grants `s3:GetObject` on `arn:aws:s3:::my-bucketname/*`), narrowed here to the `shared/` prefix; `s3:ListBucket` is added at the bucket level so the identity can list.

**Provisioning confirmation (observed):**

```
PROVISION: readonly-bp present in ListCannedPolicies = true
PROVISION: canned policies = [consoleAdmin diagnostics readonly readonly-bp readwrite writeonly]
PROVISION: rouser->readonly-bp, rwuser->readwrite attached; seeds created
```

**Seed objects** (written by root so reads/listing return content). Each was given `Content-Type: text/plain`; `shared/a.txt` additionally carries user metadata `x-amz-meta-origin: seed` so HEAD disclosure is observable:

| Key | In grant? | Size | ETag (md5 of content) | Purpose |
|-----|-----------|------|-----------------------|---------|
| `shared/a.txt` | yes | 23 | `fd74ed357885ea3caeeef6d5af06f788` | primary read/HEAD/tag/delete target |
| `shared/b.txt` | yes | 23 | `2b5ecd05ccf9b6d7c6a9748c7353d140` | batch-delete target |
| `shared/sub/c.txt` | yes | 27 | — | nested-prefix read |
| `private/secret.txt` | **no (outside prefix)** | 29 | `d22a9ee43a7852899a6653bfac0cc50e` | disclosure test |
| `shared/contended.txt` | yes | (varies) | — | TOCTOU contention target |

On the erasure backend, a seeded object materializes as a directory containing `xl.meta`; e.g. `/tmp/minio-data/testbucket/shared/a.txt/xl.meta` was **457 bytes**. This is the baseline against which "zero storage side effect" is asserted after each denial (§5).

---

## 4. Concurrent-load description ("under stress") and repeatability

**Load generator.** 32 goroutines, each authenticated as `rwuser` (built-in `readwrite`), looping: a normal `PutObject` to a unique key under `shared/load/…`, a `PutObjectTagging` on that key, and — every 8th iteration — a `PutObject` to the single **contended** key `shared/contended.txt` (the exact key the TOCTOU probe targets, forcing real namespace-lock contention on one object).

**Observed scale (this run):**

```
LOAD: 32 workers, puts=807 tags=704 wall=12.001s
```

i.e. **807 `PutObject` + 704 `PutObjectTagging` = 1,511 write/metadata operations over ~12 s** of sustained contention against `testbucket`, with the read-only probe matrix running **concurrently** during the window. (The absolute count is bounded by the fsync-bound backend noted in §2; the security outcome is independent of it.)

**Repeatability / determinism (explicitly confirmed).** The full write-adjacent probe matrix was executed **16 times** inside the load window; every pass was **identical in HTTP status and S3 error `<Code>`**:

```
STABILITY: 16 write-adjacent matrix runs during load; all identical to run1 (status+code) = true
```

A **second, independent invocation** (16 workers, 3 s; 229 puts + 197 tags) was then run and compared probe-by-probe against the first:

```
distinct probes: 31
run1==run2 for every probe: True
all status/code pairs identical across both invocations
```

**Per-API `403 AccessDenied` tallies from the audit log** for this session (deterministic under concurrency — no authorization "downgrade" ever occurred). The counts scale with how many times each op appears across the 16 matrix passes (e.g. three delete probes/pass → `DeleteObject 49`; two copy probes/pass → `CopyObject 32`):

```
DeleteObject 49   CopyObject 32   PutObjectLegalHold 17   NewMultipartUpload 16
PutObjectPart 16  CopyObjectPart 16  CompleteMultipartUpload 16  AbortMultipartUpload 16
ListObjectParts 16  PutObjectTagging 16  DeleteObjectTagging 16
GetObject 1  HeadObject 1  ListMultipartUploads 1  PutObjectRetention 1
```

Total denials attributed to `accessKey=rouser`: **229** (plus the one anonymous `ListBuckets` denial from the §2 baseline).


---

## 5. Per-operation results (multipart, copy, metadata changes, deletes)

Every probe below was issued **from `rouser` through the real S3 API** using raw SigV4-signed HTTP built with `github.com/minio/minio-go/v7/pkg/signer` — `signer.SignV4` [request-signature-v4.go:L343] for signed requests and `signer.PreSignV4` [request-signature-v4.go:L208] for presigned — the same signer the official client uses. `X-Amz-Content-Sha256` was set to `hex(sha256(body))` before signing. All ran **during** the concurrent load of §4.

The denial envelope is identical in shape everywhere: `ErrAccessDenied` [cmd/api-errors.go:L86] maps to `HTTPStatusCode: http.StatusForbidden` [cmd/api-errors.go:L539]; the body is the `APIErrorResponse` struct [cmd/api-errors.go:L64] populated by `getAPIErrorResponse` [cmd/api-errors.go:L2597] and written by `writeErrorResponse` [cmd/api-response.go:L945].

### 5.1 Multipart operations

All six multipart operations are denied. Each is guarded by a distinct action in `cmd/object-multipart-handlers.go`, checked via `checkRequestAuthType` [cmd/auth-handler.go:L339] (which calls `authenticateRequest` [cmd/auth-handler.go:L358] → `IAMSys.IsAllowed` [cmd/iam.go:L2437]) **before** any upload-id is created.

**`CreateMultipartUpload`** — guard `PutObjectAction` [cmd/object-multipart-handlers.go:L83]:

```
REQUEST: POST http://127.0.0.1:9000/testbucket/shared/mp.txt?uploads
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7A199AC
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1E7B2B7A199AC</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**`UploadPart`** — guard `isPutActionAllowed` [cmd/auth-handler.go:L749] / `PutObjectAction` [cmd/object-multipart-handlers.go:L667]:

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/mp.txt?partNumber=1&uploadId=fakeUID
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7ACFB8E
<Error><Code>AccessDenied</Code>…<Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1E7B2B7ACFB8E</RequestId>…</Error>
```

**`UploadPartCopy`** — dst `PutObjectAction` [cmd/object-multipart-handlers.go:L268] + src `GetObjectAction` [cmd/object-multipart-handlers.go:L301]:

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/mp.txt?partNumber=1&uploadId=fakeUID
         (x-amz-copy-source: /testbucket/shared/a.txt)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7B323F4
```

**`CompleteMultipartUpload`** — guard `PutObjectAction` [cmd/object-multipart-handlers.go:L927]:

```
REQUEST: POST http://127.0.0.1:9000/testbucket/shared/mp.txt?uploadId=fakeUID
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7B9ADB5
```

**`AbortMultipartUpload`** — guard `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118]:

```
REQUEST: DELETE http://127.0.0.1:9000/testbucket/shared/mp.txt?uploadId=fakeUID
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7BEDAC2
```

**`ListParts`** — guard `ListMultipartUploadPartsAction` [cmd/object-multipart-handlers.go:L1162]:

```
REQUEST: GET http://127.0.0.1:9000/testbucket/shared/mp.txt?uploadId=fakeUID
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7C41E6F
```

- **Storage side-effect:** `/tmp/minio-data/testbucket/shared/mp.txt` is **ABSENT** (no directory, no `xl.meta`), and `/tmp/minio-data/.minio.sys/multipart` contains **0** upload-id directories. The denial in the handler prevented any multipart session from ever being created.
- **Audit-log record:** the multipart-create denial is logged as internal API name **`NewMultipartUpload`** — `{api=NewMultipartUpload, status=Forbidden, statusCode=403, accessKey=rouser, bucket=testbucket, object=shared/mp.txt, requestID=18C1E7B2B7A199AC}` — the `requestID` matching the wire trace's `X-Amz-Request-Id` exactly. The other five are logged as `PutObjectPart`, `CopyObjectPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListObjectParts`.

### 5.2 Copy-style writes — `CopyObject`

`CopyObjectHandler` [cmd/object-handlers.go:L1154] checks the destination `PutObjectAction` [cmd/object-handlers.go:L1173] first, then the source `GetObjectAction` [cmd/object-handlers.go:L1206]. The read-only policy grants neither on the destination, so it is denied:

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/copydst.txt
         (x-amz-copy-source: /testbucket/shared/a.txt)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7CA791D
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/copydst.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/copydst.txt</Resource><RequestId>18C1E7B2B7CA791D</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

- **Storage side-effect:** `/tmp/minio-data/testbucket/shared/copydst.txt` is **ABSENT**. Nothing was copied.
- **Audit-log record:** `{api=CopyObject, statusCode=403, accessKey=rouser, bucket=testbucket, object=shared/copydst.txt, requestID=18C1E7B2B7CA791D}`.

### 5.3 Metadata changes

**`PutObjectTagging`** — `PutObjectTaggingHandler` [cmd/object-handlers.go:L3122] → `PutObjectTaggingAction` [cmd/object-handlers.go:L3151]:

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/a.txt?tagging
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7D00F48
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1E7B2B7D00F48</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**`DeleteObjectTagging`** — `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235] → `DeleteObjectTaggingAction` [cmd/object-handlers.go:L3301]:

```
REQUEST: DELETE http://127.0.0.1:9000/testbucket/shared/a.txt?tagging
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7D593C9
```

**`PutObjectLegalHold`** — `PutObjectLegalHoldHandler` [cmd/object-handlers.go:L2698] → `PutObjectLegalHoldAction` [cmd/object-handlers.go:L2718] (sent with a valid `Content-Md5` so the IAM decision is reached):

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/a.txt?legal-hold   (Content-Md5: oK1+ndJbG6HuGA87LDxznw==)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7DF3154
```

**`PutObjectRetention`** — `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] validates the signature via `validateSignature` [cmd/object-handlers.go:L2874] and enforces the object-lock path through `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913]. On the non-object-lock `testbucket`, a **bucket-lock precheck fires first**, so the observed result on `testbucket` is *not* the IAM decision — an important nuance:

```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/a.txt?retention   (Content-Md5: f+jV5nz5be4Ld3UorctjIw==)
HTTP-STATUS: 400     X-Amz-Request-Id: 18C1E7B2B7E457AB
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Bucket is missing ObjectLockConfiguration</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1E7B2B7E457AB</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The clean read-only *authorization* denial for retention is therefore demonstrated on a real object-lock bucket in §7 (WORM), where it returns `403 AccessDenied`.

- **Storage side-effect (all metadata ops):** `shared/a.txt` is unchanged on disk — its `xl.meta` remained **457 bytes** after every denied tagging/legal-hold/retention attempt; no tags or hold were written.
- **Audit-log records:** `PutObjectTagging`, `DeleteObjectTagging`, `PutObjectLegalHold` each logged `statusCode=403 accessKey=rouser object=shared/a.txt`; e.g. tagging `requestID=18C1E7B2B7D00F48`, legal-hold `requestID=18C1E7B2B7DF3154`.

### 5.4 Deletes — single (`DeleteObject`) vs batch (`DeleteObjects`)

This is the one place where the HTTP-level shape differs, and the contrast is important.

**Single `DeleteObject`** — `DeleteObjectHandler` [cmd/object-handlers.go:L2509] → `DeleteObjectAction` [cmd/object-handlers.go:L2528]. Denied at the top level with `HTTP 403`:

```
REQUEST: DELETE http://127.0.0.1:9000/testbucket/shared/a.txt
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7EFFAB7
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1E7B2B7EFFAB7</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Batch `DeleteObjects`** — `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416] checks each object **individually** via `checkRequestAuthTypeWithVID` / `DeleteObjectAction` [cmd/bucket-handlers.go:L505]. The overall POST is well-formed, so the **HTTP status is `200`**; the denial appears **per-object** inside the `<DeleteResult>` body (sent with `Content-Md5`, which this API requires):

```
REQUEST: POST http://127.0.0.1:9000/testbucket?delete   (Content-Md5: wi4s+CUiZpmK1AEVNO7diw==, Content-Type: application/xml)
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B7FA6586
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><VersionId></VersionId></Error><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/b.txt</Key><VersionId></VersionId></Error></DeleteResult>
```

So: **single delete = top-level `HTTP 403 AccessDenied`; batch delete = `HTTP 200` carrying a per-object `<Error><Code>AccessDenied</Code>` for every key** — a different envelope but the same authorization outcome (nothing deleted).

**Content-Md5 nuance (not the auth decision).** The batch-delete and retention APIs require a `Content-Md5` header; omitting it yields `HTTP 400 MissingContentMD5`, validated **before** IAM:

```
REQUEST: POST http://127.0.0.1:9000/testbucket?delete   (no Content-Md5)
HTTP-STATUS: 400     X-Amz-Request-Id: 18C1E7B2B802403C
<Error><Code>MissingContentMD5</Code><Message>Missing required header for this request: Content-Md5.</Message>…</Error>
```
```
REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/a.txt?retention   (no Content-Md5)
HTTP-STATUS: 400     X-Amz-Request-Id: 18C1E7B2B7EA2D85
<Error><Code>MissingContentMD5</Code>…</Error>
```

This is a request-validation error, **not** the authorization decision; the probes above send a valid `Content-Md5` to reach the true decision.

- **Storage side-effect (single & batch):** After the denied single delete **and** the batch denial, `shared/a.txt/xl.meta` is still **PRESENT (457 B)** and `shared/b.txt/xl.meta` still **PRESENT (434 B)**. A root re-list confirms both keys remain. **Nothing was deleted.**
- **Audit-log records:** single delete `{api=DeleteObject, statusCode=403, accessKey=rouser, object=shared/a.txt, requestID=18C1E7B2B7EFFAB7}`. This `requestID` was located verbatim in the audit log, confirming wire↔audit correlation.


---

## 6. Listing & HEAD — metadata disclosure characterization

Listing and HEAD are the **read** surfaces the read-only identity legitimately holds. This section enumerates precisely what they disclose and where the scope boundary lies. Every probe ran as `rouser` during the load.

### 6.1 `HeadObject` and `GetObject`

**`HeadObject` inside the prefix → `HTTP 200`**, guarded by `GetObjectAction` in `headObjectHandler` [cmd/object-handlers.go:L760] (dispatched from [cmd/object-handlers.go:L744]). It discloses the following attributes without transferring the body:

```
REQUEST: HEAD http://127.0.0.1:9000/testbucket/shared/a.txt
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B823A07F
  Content-Length:  23
  Content-Type:    text/plain
  ETag:            "fd74ed357885ea3caeeef6d5af06f788"
  Last-Modified:   Mon, 13 Jul 2026 16:54:41 GMT
  Accept-Ranges:   bytes
  X-Amz-Meta-Origin: seed
```

**Disclosed by HEAD:** object **size** (`Content-Length`), **content type**, **ETag** (content MD5 for simple puts), **last-modified timestamp**, range support, and **user-defined metadata** (`x-amz-meta-*`). `GetObject` inside the prefix additionally returns the object bytes:

```
REQUEST: GET http://127.0.0.1:9000/testbucket/shared/a.txt
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B80EFA04
BODY: payload-of-shared/a.txt
```

**Outside the granted prefix, both are denied** — the `s3:GetObject` grant is scoped to `arn:aws:s3:::testbucket/shared/*`, so `private/secret.txt` is `403`:

```
GET  /testbucket/private/secret.txt → 403 AccessDenied   (X-Amz-Request-Id: 18C1E7B2B818BAB6)
HEAD /testbucket/private/secret.txt → 403                (X-Amz-Request-Id: 18C1E7B2B82D2D55)
```

### 6.2 `HeadBucket`

**`HeadBucket` → `HTTP 200`.** `HeadBucketHandler` [cmd/bucket-handlers.go:L1644] checks `ListBucketAction` [cmd/bucket-handlers.go:L1658], which the policy grants at the bucket level; it discloses only bucket existence/accessibility (no body):

```
REQUEST: HEAD http://127.0.0.1:9000/testbucket
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B835A154
```

### 6.3 `ListObjectsV2` / `ListObjectsV1` — and the bucket-scoped disclosure nuance

Both list handlers check `ListBucketAction` — `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L172] and `ListObjectsV1Handler` [cmd/bucket-listobjects-handlers.go:L287]. The grant `s3:ListBucket` on `arn:aws:s3:::testbucket` is **bucket-scoped, not prefix-scoped**.

**Listing *inside* the granted prefix → `HTTP 200`.** Each `<Contents>` entry discloses **Key, LastModified, ETag, Size, StorageClass**:

```
REQUEST: GET http://127.0.0.1:9000/testbucket?list-type=2&prefix=shared/
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B83B7D11
  <KeyCount>59</KeyCount>  <IsTruncated>false</IsTruncated>
  <Contents><Key>shared/a.txt</Key><LastModified>2026-07-13T16:54:41.309Z</LastModified>
            <ETag>&#34;fd74ed357885ea3caeeef6d5af06f788&#34;</ETag><Size>23</Size>
            <StorageClass>STANDARD</StorageClass></Contents>
  …  (private/secret.txt does NOT appear — count of "private/secret.txt" in body = 0)
```

**Listing the *whole bucket* (no prefix) → `HTTP 200`, and it DISCLOSES keys outside the granted prefix.** Because the check is on the bucket resource, `private/secret.txt` — which the identity **cannot read** — nonetheless appears in the listing:

```
REQUEST: GET http://127.0.0.1:9000/testbucket?list-type=2
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B874E863
  <KeyCount>60</KeyCount>
  <Key>private/secret.txt</Key><LastModified>2026-07-13T16:54:41.613Z</LastModified>
        <ETag>&#34;d22a9ee43a7852899a6653bfac0cc50e&#34;</ETag><Size>29</Size>
```

**`ListObjectsV1` with a prefix pointing *outside* the grant behaves the same, and additionally discloses object ownership** (`<Owner><ID>…<DisplayName>`):

```
REQUEST: GET http://127.0.0.1:9000/testbucket?prefix=private/
HTTP-STATUS: 200     X-Amz-Request-Id: 18C1E7B2B89CB5DA
  <Contents><Key>private/secret.txt</Key><LastModified>2026-07-13T16:54:41.613Z</LastModified>
            <ETag>&#34;d22a9ee43a7852899a6653bfac0cc50e&#34;</ETag><Size>29</Size>
            <Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID>
                   <DisplayName>minio</DisplayName></Owner>
            <StorageClass>STANDARD</StorageClass></Contents>
```

**Interpretation (information-disclosure vs mutation).** This is *not* a mutation bypass and *not* a read bypass of object **content** — `GetObject`/`HeadObject` on `private/secret.txt` remain `403` (§6.1). It is a **metadata information-disclosure** property inherent to bucket-scoped `s3:ListBucket`: the key names, sizes, ETags, timestamps and owner of objects *outside* the read prefix are enumerable. The documented mitigation, shown in `docs/multi-user/README.md`, is to attach an `s3:prefix` `Condition` to the `s3:ListBucket` statement so listing is confined to the intended prefix.

### 6.4 `ListMultipartUploads` — a list-style read that is DENIED

`ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251] checks `ListBucketMultipartUploadsAction` [cmd/bucket-handlers.go:L265] — a **distinct** action the read-only policy does **not** grant (it is not the same as `s3:ListBucket`). Hence it is denied even though ordinary listing succeeds:

```
REQUEST: GET http://127.0.0.1:9000/testbucket?uploads
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B8A9E990
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>testbucket</BucketName><Resource>/testbucket</Resource><RequestId>18C1E7B2B8A9E990</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Summary of disclosure:** `HeadObject`/`GetObject` → size, content-type, ETag, last-modified, user metadata, body — **only inside the prefix**. `HeadBucket` → existence. `ListObjects` (V1/V2) → key, size, ETag, last-modified, storage class, and (V1) owner — **for the whole bucket, including outside the grant**. `ListMultipartUploads` → **denied**.

---

## 7. Edge / variant coverage, TOCTOU, and WORM independence

### 7.1 Inside vs outside the granted prefix

Covered throughout: every write-adjacent op targeting `shared/…` is `403`; reads inside `shared/` succeed while reads of `private/secret.txt` are `403` (§6.1); whole-bucket listing discloses `private/` metadata but not its content (§6.3).

### 7.2 Signed vs presigned — identical decision

A **presigned** `DELETE` (query-string SigV4 via `signer.PreSignV4` [request-signature-v4.go:L208]) produces the **same** `403 AccessDenied` as the header-signed form. Authorization is independent of the SigV4 presentation:

```
REQUEST: DELETE http://127.0.0.1:9000/testbucket/shared/a.txt   (presigned, query-string auth)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B7F59AD3
<Error><Code>AccessDenied</Code>…<Key>shared/a.txt</Key>…<Resource>/testbucket/shared/a.txt</Resource>…</Error>
```

### 7.3 Single vs batch delete

Contrasted in full in §5.4: single delete → top-level `HTTP 403`; batch delete → `HTTP 200` with a per-object `<Error><Code>AccessDenied</Code>` for each key. Both leave the objects intact on disk.

### 7.4 TOCTOU — probing the exact keys the load is mutating

While the 32-worker load repeatedly rewrote `shared/contended.txt`, the read-only identity attempted a `DeleteObject` and a `CopyObject` (destination) **on that same key**. Both returned the same `403 AccessDenied` as under quiescence:

```
REQUEST: DELETE http://127.0.0.1:9000/testbucket/shared/contended.txt   (contended)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B806A80A

REQUEST: PUT http://127.0.0.1:9000/testbucket/shared/contended.txt      (copy dst, contended)
HTTP-STATUS: 403     X-Amz-Request-Id: 18C1E7B2B80AC894
```

**Why there is no check-vs-use window (reasoning).** The authorization decision is computed from the request's **fixed** action + resource + principal inside the handler — `checkRequestAuthType` [cmd/auth-handler.go:L339] → `authenticateRequest` [cmd/auth-handler.go:L358] → `IAMSys.IsAllowed` [cmd/iam.go:L2437] — **before** the object layer acquires any namespace lock or touches storage. Concurrent writes by other principals change object *data/version state*, but they do not and cannot change `rouser`'s policy set mid-request; the decision does not depend on the object's current bytes. Concurrent multi-writer contention in S3 surfaces as **data-consistency** responses (e.g. HTTP 409/412 on conditional writes), never as an authorization downgrade — and indeed the authorization outcome was bit-for-bit identical across all 16 in-load passes and the second invocation (§4).

### 7.5 WORM / object-lock independence from IAM

To exercise `PutObjectRetention` cleanly (it can't be reached on a non-lock bucket — see §5.3), an object-lock bucket `wormbucket` was created and root wrote a **GOVERNANCE**-locked object `shared/locked.txt` (72 h retention). The read-only policy was broadened to *read* `wormbucket/shared/*` so we can prove **read ≠ mutate**:

```
GET /wormbucket/shared/locked.txt → 200   (readable in prefix)
   Content-Length: 14   ETag: "aa54c95a6163d9076ec20693d215fc3d"   (X-Amz-Request-Id: 18C1E7B2CE1FF53A)

PUT /wormbucket/shared/locked.txt?retention   (Content-Md5 set) → 403 AccessDenied   (18C1E7B2CE41DAD4)
PUT /wormbucket/shared/locked.txt?legal-hold  (Content-Md5 set) → 403 AccessDenied   (18C1E7B2CE4D1FB6)
DELETE /wormbucket/shared/locked.txt  (x-amz-bypass-governance-retention: true) → 403 AccessDenied  (18C1E7B2CE547F71)
```

Root then confirmed the object is **intact**: `WORM.StatObject.root: intact size=14 etag=aa54c95a6163d9076ec20693d215fc3d`, and `/tmp/minio-data/wormbucket/shared/locked.txt/xl.meta` is present (568 B).

The object-lock enforcement in `cmd/bucket-object-lock.go` is **independent of the IAM allow set**: reducing/removing governance retention requires `BypassGovernanceRetentionAction` — checked in `enforceRetentionBypassForDelete` [cmd/bucket-object-lock.go:L84] via `checkRequestAuthType(..., policy.BypassGovernanceRetentionAction, ...)` [cmd/bucket-object-lock.go:L153]; `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] blocks shortening governance retention without the bypass (governance branch [cmd/bucket-object-lock.go:L193]) and makes compliance mode unchangeable by anyone ([cmd/bucket-object-lock.go:L215]); `checkPutObjectLockAllowed` [cmd/bucket-object-lock.go:L245] gates lock metadata on writes. A read-only identity holds **neither** the retention/legal-hold actions **nor** the bypass action, so it can neither **set** nor **bypass** retention. Even the explicit `x-amz-bypass-governance-retention: true` header does not help, because the delete itself first fails the `DeleteObjectAction` check.


---

## 8. Reasoning with `file:line` grounding

### 8.1 The enforcement chain (guard runs in the handler, before the object layer)

Every authenticated S3 request converges on one path:

1. **Classification.** `getRequestAuthType` [cmd/auth-handler.go:L124] classifies the request into one of the **12** auth types defined at [cmd/auth-handler.go:L109-L120] (`authTypeAnonymous`, `authTypePresigned`, `authTypeSigned`, `authTypeStreamingSigned`, … ). `rouser`'s header-signed probes are `authTypeSigned`; the presigned delete is `authTypePresigned`.
2. **Gate.** Handlers call `checkRequestAuthType` [cmd/auth-handler.go:L339] (or `checkRequestAuthTypeWithVID` [cmd/auth-handler.go:L349] for versioned/batch), which calls `authenticateRequest` [cmd/auth-handler.go:L358]. PUT-class operations additionally pass through `isPutActionAllowed` [cmd/auth-handler.go:L749].
3. **Terminal decision.** `authenticateRequest` invokes `IAMSys.IsAllowed` [cmd/iam.go:L2437]. Its flow, verified against this checkout:
   - OPA plugin short-circuit (not configured here);
   - `if args.IsOwner { return true }` [cmd/iam.go:L2448] — `rouser` is **not** owner;
   - temporary-credential path `IsAllowedSTS` [cmd/iam.go:L2242] — not an STS credential;
   - service-account path `IsAllowedServiceAccount` [cmd/iam.go:L2140] — not a service account;
   - regular user: `PolicyDBGet` → **`if len(policies) == 0 { return false }`** [cmd/iam.go:L2476], else `return sys.GetCombinedPolicy(policies...).IsAllowed(args)` [cmd/iam.go:L2482].
   `rouser` has exactly one attached policy (`readonly-bp`), which contains **no statement** allowing any write-adjacent action, so `GetCombinedPolicy(...).IsAllowed(args)` returns `false` for each → `403`.
4. **Denial surface.** `false` becomes `ErrAccessDenied` [cmd/api-errors.go:L86] → `http.StatusForbidden` [cmd/api-errors.go:L539]; the XML `APIErrorResponse` [cmd/api-errors.go:L64] is built by `getAPIErrorResponse` [cmd/api-errors.go:L2597] and emitted by `writeErrorResponse` [cmd/api-response.go:L945].
5. **Audit.** The denial is recorded by `logger.AuditLog` [cmd/auth-handler.go:L636] with principal, API name, bucket/object and request ID.

**Deny-by-default** is the crux: MinIO grants nothing implicitly. A read-only policy that allows only `s3:GetObject`+`s3:ListBucket` therefore denies `PutObjectAction`, `DeleteObjectAction`, `AbortMultipartUploadAction`, `PutObjectTaggingAction`, `DeleteObjectTaggingAction`, `PutObjectLegalHoldAction`, `PutObjectRetentionAction`, `ListMultipartUploadPartsAction`, and `ListBucketMultipartUploadsAction` — each of which guards exactly one of the operations probed. (These `policy.Action` symbols come from `github.com/minio/pkg/v3 v3.0.22` [go.mod:L55].)

### 8.2 Why the bucket-policy path is not involved

`PolicySys.IsAllowed` [cmd/bucket-policy.go:L48] takes `policy.BucketPolicyArgs` and evaluates **per-bucket JSON policy for anonymous callers**. `rouser` is an authenticated IAM principal, so its decision is made by `IAMSys.IsAllowed`, not by the bucket-policy path. (No bucket policy was set on `testbucket`, so anonymous callers are denied too — cf. the anonymous `GET /` baseline in §2.)

### 8.3 Why enforcement ordering guarantees zero storage side effects

The action check in each handler executes **before** the object layer acquires a namespace lock or writes any shard. Therefore a denial returns at the handler boundary and the erasure backend is never touched. This is exactly what the storage stream shows: after denials, `shared/mp.txt` and `shared/copydst.txt` are **absent**, `.minio.sys/multipart` holds **0** upload-id directories, and the pre-existing `shared/a.txt`/`shared/b.txt` `xl.meta` files are **unchanged** and still present. The on-disk erasure layout used for these assertions (`<datadir>/<bucket>/<object>/xl.meta` + part files) matches the layout exercised by the repository's own tests (`cmd/erasure-object_test.go`, `cmd/erasure-healing_test.go`).

### 8.4 Coverage pass — every named item, answered by name

| Named item | Handler → guard action (`file:line`) | Wire result | Storage side-effect | Audit (internal API) |
|------------|--------------------------------------|-------------|---------------------|----------------------|
| **Multipart** `CreateMultipartUpload` | `NewMultipartUploadHandler` → `PutObjectAction` [object-multipart-handlers.go:L83] | 403 AccessDenied | `shared/mp.txt` absent; 0 upload-ids | `NewMultipartUpload` |
| **Multipart** `UploadPart` | `PutObjectPartHandler` → `PutObjectAction` [object-multipart-handlers.go:L667] | 403 AccessDenied | no part written | `PutObjectPart` |
| **Multipart** `UploadPartCopy` | dst `PutObjectAction` [object-multipart-handlers.go:L268] + src `GetObjectAction` [L301] | 403 AccessDenied | no part written | `CopyObjectPart` |
| **Multipart** `CompleteMultipartUpload` | `PutObjectAction` [object-multipart-handlers.go:L927] | 403 AccessDenied | no object materialized | `CompleteMultipartUpload` |
| **Multipart** `AbortMultipartUpload` | `AbortMultipartUploadAction` [object-multipart-handlers.go:L1118] | 403 AccessDenied | n/a (no session) | `AbortMultipartUpload` |
| **Multipart** `ListParts` | `ListMultipartUploadPartsAction` [object-multipart-handlers.go:L1162] | 403 AccessDenied | read — none | `ListObjectParts` |
| **Copy** `CopyObject` | dst `PutObjectAction` [object-handlers.go:L1173] + src `GetObjectAction` [L1206] | 403 AccessDenied | `shared/copydst.txt` absent | `CopyObject` |
| **Metadata** `PutObjectTagging` | `PutObjectTaggingAction` [object-handlers.go:L3151] | 403 AccessDenied | `a.txt` xl.meta unchanged | `PutObjectTagging` |
| **Metadata** `DeleteObjectTagging` | `DeleteObjectTaggingAction` [object-handlers.go:L3301] | 403 AccessDenied | `a.txt` xl.meta unchanged | `DeleteObjectTagging` |
| **Metadata** `PutObjectLegalHold` | `PutObjectLegalHoldAction` [object-handlers.go:L2718] | 403 AccessDenied | `a.txt` xl.meta unchanged | `PutObjectLegalHold` |
| **Metadata** `PutObjectRetention` | `enforceRetentionBypassForPut` [object-handlers.go:L2913] | 403 AccessDenied (on lock bucket §7.5); 400 InvalidRequest on non-lock bucket | object intact | `PutObjectRetention` |
| **Delete** `DeleteObject` (single) | `DeleteObjectAction` [object-handlers.go:L2528] | 403 AccessDenied | `a.txt` present (457 B) | `DeleteObject` |
| **Delete** `DeleteObjects` (batch) | per-object `DeleteObjectAction` [bucket-handlers.go:L505] | 200 + per-object AccessDenied | `a.txt`,`b.txt` present | `DeleteObjects`/`DeleteObject` |
| **List** `ListObjectsV2` (in prefix) | `ListBucketAction` [bucket-listobjects-handlers.go:L172] | 200 (keys in `shared/`) | read — none | — |
| **List** `ListObjectsV2` (no prefix) | `ListBucketAction` [bucket-listobjects-handlers.go:L172] | 200 — **discloses `private/`** | read — none | — |
| **List** `ListObjectsV1` (outside prefix) | `ListBucketAction` [bucket-listobjects-handlers.go:L287] | 200 — discloses key+owner | read — none | — |
| **List** `ListMultipartUploads` | `ListBucketMultipartUploadsAction` [bucket-handlers.go:L265] | 403 AccessDenied | read — none | `ListMultipartUploads` |
| **HEAD** `HeadObject` (in prefix) | `GetObjectAction` [object-handlers.go:L760] | 200 — discloses size/ETag/type/meta | read — none | `HeadObject` |
| **HEAD** `HeadObject` (outside prefix) | `GetObjectAction` [object-handlers.go:L760] | 403 | read — none | `HeadObject` |
| **HEAD** `HeadBucket` | `ListBucketAction` [bucket-handlers.go:L1658] | 200 | read — none | — |

### 8.5 Bottom line

Under sustained concurrent write/metadata load, a MinIO identity scoped to `s3:GetObject` (bucket+prefix) + `s3:ListBucket` (bucket) is **strictly read-only for object content and metadata mutation**: every multipart, copy, tagging/legal-hold/retention, and delete operation is denied deny-by-default, with **zero storage side effects** and a matching audit record, deterministically and identically across repeated runs and under TOCTOU contention. The **only** caveat is an *information-disclosure* one, not a mutation one: bucket-scoped `s3:ListBucket` lets the identity enumerate key names/sizes/ETags/timestamps/owner across the whole bucket (including outside its read prefix), which should be constrained with an `s3:prefix` `Condition` if that enumeration is undesirable.

---

## Appendix — reproduction summary & repository integrity

- **Build:** `CGO_ENABLED=0 go build -o /tmp/minio-bin .` (Go 1.23.12), exit 0, ~150 MB, `go.mod`/`go.sum` unchanged.
- **Run:** `MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin /tmp/minio-bin server /tmp/minio-data --address :9000 --console-address :9001` + audit webhook to a local sink.
- **Provision (canonical admin path):** `madmin` `AddCannedPolicy`/`AddUser`/`SetPolicy` → `readonly-bp` (custom bucket+prefix) on `rouser`, `readwrite` on `rwuser`. `mc` was not installed.
- **Probe:** raw SigV4 (`minio-go/v7/pkg/signer` `SignV4`/`PreSignV4`) — the real S3 API path.
- **Scaffolding:** server binary, datadir, harness, audit sink and policy JSON lived entirely under `/tmp` and were removed after the run. The repository's only change is this document; `go.mod`/`go.sum` are byte-for-byte unchanged; `git status` shows only `blitzy/documentation/minio_c07e5b49d477.md`.
- **Non-canonical values:** none. Every value above was produced by the default build exercised through the real admin and S3 APIs. Request IDs/timestamps are specific to this run; `HostId=dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`.

