# MinIO Runtime Behavior Investigation — branch `minio_c07e5b49d477`

## Preface

This document is a **read-only investigation** of MinIO runtime behavior. It answers five
questions about the MinIO object storage server (`github.com/minio/minio`, module declared at
[`go.mod:1`]) by **building and running the code first**, capturing the **actual observed
output**, and then explaining it — with every behavioral claim grounded in an exact `file:line`
citation and paired with the specific observed line that demonstrates it.

- **The only file added to the repository is this document** (`blitzy/documentation/minio_c07e5b49d477.md`).
  No existing repository source file was created-over, edited, or deleted.
- All temporary observation artifacts (server data directories, deliberately corrupted shard
  files, and the temporary in-process Go probe test) were removed after use. The working tree is
  otherwise unchanged — `git status` is clean apart from the new `blitzy/` directory.
- **`go.mod` / `go.sum` are untouched.** No dependency was added, upgraded, or removed.

### Methodology (user rule set "SWE-AtlasQnA-Repo")

1. **Investigate by running first, then write.** Each answer presents the reproduction command,
   then the verbatim output, then the explanation.
2. **One claim, one piece of evidence.** Every behavioral statement is placed next to the exact
   observed output line (or exact `file:line`) that proves it — never batched, never paraphrased.
3. **Be exact and grounded.** Literals (identifiers, string values, error/status codes, headers,
   config keys, measured numbers) are cited with a `file:line` reference. Observed output is
   reproduced exactly as seen, including the honest, slightly surprising nuances.
4. **Answer every named part.** Each section ends with a per-question coverage note; a consolidated
   Coverage Checklist appears at the end.
5. **Reasoning is stated** for each answer.

> **Note on `file:line` citations.** Line numbers below were re-confirmed against the source in
> this working tree (the code is the source of truth). A few line numbers differ by one or two
> lines from earlier drafts because a return statement or an assignment sits on a slightly
> different line than a surrounding brace or condition; the corrected numbers are used throughout.

---

## Environment

MinIO was **built from source with Go 1.23.2** and run locally. The built binary self-reports
(verbatim):

```text
minio_bin version DEVELOPMENT.GOGET
Runtime: go1.23.2 linux/amd64
```

- `go.mod` declares the language version `go 1.23` — [`go.mod:3`].
- The host toolchain is `go version go1.23.2 linux/amd64`, matching the binary's reported runtime.

**Run modes used:**

- **Live server over a 4-drive erasure set** for Q1, Q2, and Q3:
  `minio server /tmp/minio-data/d1 /tmp/minio-data/d2 /tmp/minio-data/d3 /tmp/minio-data/d4`,
  which the server reports as `1 set(s), 4 drives per set` with parity `EC:2`. A multi-drive
  erasure layout is **required** for Q3 because bit-rot protection is a per-shard property.
- **MinIO's in-process Go test harness** (`TestSuiteIAM`) for Q4 and Q5, driven via
  `go test ./cmd/`. This is the most reliable and reproducible source of the "test output" those
  two questions ask for and needs no external tooling.

**Evidence channels:**

- **Trace** (what the questions call "server trace logs") was captured with the real MinIO client
  `mc admin trace`. A subscriber was attached **before** each operation, because the server only
  emits trace while a subscriber is attached — the subscriber gate is
  `if globalTrace.NumSubscribers(madmin.TraceS3|madmin.TraceInternal) == 0` at
  [`cmd/http-tracer.go:92`].
- S3 / STS / admin operations were driven by `mc` and by `boto3` (Python).
- **KMS had to be enabled** (`MINIO_KMS_SECRET_KEY`) before bucket SSE-S3 could be configured; the
  first attempt failed without it. This honest detail is reported in Q1.

> `mc` and the AWS CLI are not pre-installed in the base environment; `mc` was used for the
> trace-driven scenarios (Q1–Q3) and the in-process Go test harness for the test-driven scenarios
> (Q4–Q5).

---

## Q1 — A bucket encryption requirement takes precedence over broad write permission during an unencrypted upload

**Question (verbatim):** *"what happens when a bucket level encryption requirement takes precedence
over a user's broad write permissions during an unencrypted upload. Identify the specific runtime
execution sequence captured in the server trace logs."*

### Answer thesis

Permission and encryption are **independent, sequential** concerns. Authorization decides whether
the write is **accepted**; the bucket default-encryption configuration independently governs
**encryption-at-rest**. A user holding broad write permission (`s3:PutObject`, granted via the
built-in `readwrite` policy) successfully uploads an object with **no** client-side SSE header, and
the server still injects server-side encryption — so the object is stored encrypted with `AES256`.
"Precedence" means the encryption requirement is enforced independently of (and after) the
permission decision: broad write permission does **not** let a user store plaintext when the bucket
mandates SSE.

### The runtime execution sequence (in order, grounded)

**Step 1 — the request enters through the outermost middleware, the tracer, so the entire request
lifecycle is traced.** `httpTracerMiddleware` is the first entry in the generic middleware list, and
the source comment states it "needs to be the first middleware to catch all requests." It is
registered immediately **before** the auth middleware (`setAuthMiddleware`):

```go
// cmd/routers.go:53-65 (excerpt)
var globalMiddlewares = []mux.MiddlewareFunc{
	// set x-amz-request-id header and others
	addCustomHeadersMiddleware,
	// The generic tracer needs to be the first middleware to catch all requests
	// ...
	httpTracerMiddleware,          // cmd/routers.go:60
	// Auth middleware verifies incoming authorization headers ...
	setAuthMiddleware,             // cmd/routers.go:66
	...
}
```

- Claim: the tracer is registered first, before auth → [`cmd/routers.go:60`] (`httpTracerMiddleware,`)
  precedes [`cmd/routers.go:66`] (`setAuthMiddleware,`).
- Claim: trace is emitted by the tracer middleware → `func httpTracerMiddleware(h http.Handler) http.Handler`
  at [`cmd/http-tracer.go:69`], publishing via `globalTrace.Publish(t)` at [`cmd/http-tracer.go:172`],
  gated on an attached subscriber at [`cmd/http-tracer.go:92`].

**Step 2 — the authorization gate resolves FIRST inside the handler.** `PutObjectHandler`
([`cmd/object-handlers.go:1745`]) evaluates the permission gate before any business logic:

```go
// cmd/object-handlers.go:1836
if s3Err = isPutActionAllowed(ctx, rAuthType, bucket, object, r, policy.PutObjectAction); s3Err != ErrNone {
```

- Claim: the write permission is checked here → `isPutActionAllowed(..., policy.PutObjectAction)` at
  [`cmd/object-handlers.go:1836`]; the function is defined at [`cmd/auth-handler.go:749`]
  (`func isPutActionAllowed(...)`).

**Step 3 — the bucket default-encryption apply runs AFTER the gate.** Later in the same handler:

```go
// cmd/object-handlers.go:1893-1897
// Check if bucket encryption is enabled
sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
sseConfig.Apply(r.Header, sse.ApplyOptions{
	AutoEncrypt: globalAutoEncryption,
})
```

- Claim: the bucket SSE config is fetched from the in-memory cache accessor → `globalBucketSSEConfigSys.Get(bucket)`
  at [`cmd/object-handlers.go:1894`], whose accessor is `func (sys *BucketSSEConfigSys) Get(bucket string)`
  at [`cmd/bucket-encryption.go:36`].
- Claim: encryption is applied by mutating the request header → `sseConfig.Apply(r.Header, ...)` at
  [`cmd/object-handlers.go:1895`], passing `AutoEncrypt: globalAutoEncryption`
  (`globalAutoEncryption` is assigned at [`cmd/config-current.go:532`]).

**Step 4 — `Apply` injects the SSE header (and never overwrites a user-supplied one).**

```go
// internal/bucket/encryption/bucket-sse-config.go:135-153
func (b *BucketSSEConfig) Apply(headers http.Header, opts ApplyOptions) {
	if crypto.Requested(headers) {          // :136 — client already asked for SSE → do nothing
		return
	}
	if b == nil {
		if opts.AutoEncrypt {
			headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)   // :141
		}
		return
	}
	switch b.Algo() {
	case xhttp.AmzEncryptionAES:
		headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)       // :148 (SSE-S3 → AES256)
	case xhttp.AmzEncryptionKMS:
		headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)       // :150
		headers.Set(xhttp.AmzServerSideEncryptionKmsID, b.KeyID())              // :151
	}
}
```

- Claim: if the client already requested SSE, `Apply` returns early and does not overwrite → the
  guard `if crypto.Requested(headers)` at [`internal/bucket/encryption/bucket-sse-config.go:136`].
- Claim: for a bucket configured with SSE-S3, `Apply` sets the header to `AES256` →
  [`internal/bucket/encryption/bucket-sse-config.go:148`].
- The exact header literals injected (cite [`internal/http/headers.go`]):
  - `AmzServerSideEncryption = "X-Amz-Server-Side-Encryption"` → [`internal/http/headers.go:142`]
  - `AmzEncryptionAES = "AES256"` → [`internal/http/headers.go:152`]
  - `AmzEncryptionKMS = "aws:kms"` → [`internal/http/headers.go:153`]

### Reproduction

```bash
# 1. Bucket + default SSE-S3 (requires KMS enabled on the server, see note below)
mc mb local/enc-bucket
mc encrypt set sse-s3 local/enc-bucket

# 2. A user with BROAD write permission
mc admin user add local writer writer-secret-123
mc admin policy attach local readwrite --user writer      # grants s3:PutObject

# 3. Attach a trace subscriber BEFORE the upload (trace is only emitted while subscribed)
mc admin trace -v --call s3 local &

# 4. Unencrypted PUT via boto3 — NO SSE parameters passed by the client
python3 put_unencrypted.py     # boto3 put_object(Bucket=..., Key='plain.txt', Body=...)
```

### Verbatim evidence

**Precondition — bucket default encryption enabled:**

```text
Auto encryption configuration has been set successfully for local/enc-bucket
Auto encryption 'sse-s3' is enabled
```

Honest nuance (reported exactly as observed): the **first** attempt to enable SSE-S3 failed until
KMS was configured —

```text
mc: <ERROR> Unable to enable auto encryption. Server side encryption specified but KMS is not configured.
```

After setting `MINIO_KMS_SECRET_KEY` and restarting, the KMS self-check reported:

```text
Key: my-minio-key - Encryption ✔ - Decryption ✔
```

**Definitive proof the client sent NO SSE header yet the server applied it** (captured with a boto3
`before-send` hook that inspects exactly what bytes the client transmits):

```text
REQUEST SSE headers actually sent by client: NONE
HTTP status: 200
RESPONSE x-amz-server-side-encryption: AES256
boto ServerSideEncryption field: AES256
```

- Claim: the client transmitted **no** SSE header → `REQUEST SSE headers actually sent by client: NONE`.
- Claim: the upload nonetheless **succeeded** → `HTTP status: 200`.
- Claim: the server **applied** server-side encryption with the literal value `AES256` →
  `RESPONSE x-amz-server-side-encryption: AES256` (matches [`internal/http/headers.go:152`]).

**Object is encrypted at rest** (verbatim from `mc stat local/enc-bucket/plain.txt`):

```text
Encryption: SSE-S3
```

**Trace excerpt** (from `mc admin trace -v --call s3 local`) — the traced `PutObject` request block
carries the server-applied header:

```text
127.0.0.1:9000 [REQUEST s3.PutObject] ...
127.0.0.1:9000 PUT /enc-bucket/plain.txt
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=writer/.../s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, ...
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 [RESPONSE] ... 200 OK ...
```

**Important honest nuance about the trace.** `mc admin trace` reads `r.Header` at trace-**publish**
time — i.e. *after* the handler body has already run. Because `sseConfig.Apply` mutates `r.Header`
at [`cmd/object-handlers.go:1895`] **before** `httpTracerMiddleware` publishes the record at
[`cmd/http-tracer.go:172`], the injected `X-Amz-Server-Side-Encryption: AES256` line appears inside
the traced **request** block even though the boto3 before-send hook proved the client never sent it.
The proof that the header was server-injected (not client-sent) is that the client's
`Authorization` line lists `SignedHeaders=...` that **do not include** the SSE header — an
SSE header the client actually sent would have to be signed.

### Rationale / conclusion

The `s3:PutObject` permission is the gate at [`cmd/object-handlers.go:1836`]; it grants **access
only**. The bucket default-encryption apply at [`cmd/object-handlers.go:1895`] runs unconditionally
for a bucket with default SSE, regardless of the caller's permissions, and injects
`X-Amz-Server-Side-Encryption: AES256`. Thus a *permitted-but-unencrypted* upload is still stored
encrypted at rest. Broad write permission and the encryption requirement are decided at different
points in the request and neither overrides the other — the encryption requirement is simply
enforced independently and later, which is what "takes precedence" means here.

### Q1 coverage note

| Named item | Evidence / citation |
| --- | --- |
| Bucket-level encryption requirement | `mc encrypt set sse-s3`; `Apply` sets `AES256` at [`internal/bucket/encryption/bucket-sse-config.go:148`] |
| User's broad write permission (`s3:PutObject` / `readwrite`) | `mc admin policy attach local readwrite --user writer`; gate at [`cmd/object-handlers.go:1836`] |
| Unencrypted upload | `REQUEST SSE headers actually sent by client: NONE` + `HTTP status: 200` |
| Runtime execution sequence | tracer [`cmd/routers.go:60`] → auth gate [`cmd/object-handlers.go:1836`] → SSE apply [`cmd/object-handlers.go:1895`] → header inject [`internal/bucket/encryption/bucket-sse-config.go:148`] |
| Server trace logs | `mc admin trace -v --call s3`; emission at [`cmd/http-tracer.go:69`], publish [`cmd/http-tracer.go:172`], subscriber gate [`cmd/http-tracer.go:92`] |
| Injected header value | `X-Amz-Server-Side-Encryption: AES256` → [`internal/http/headers.go:142`],[`internal/http/headers.go:152`] |
| Supporting: bucket-encryption REST handlers | `PutBucketEncryptionHandler` [`cmd/bucket-encryption-handlers.go:43`]; `GetBucketEncryptionHandler` [`cmd/bucket-encryption-handlers.go:129`] |
| Supporting: SSE request parsing / `crypto.Requested` | `EncryptRequest` [`cmd/encryption-v1.go:466`]; early-return guard [`internal/bucket/encryption/bucket-sse-config.go:136`] |
| Supporting: `globalAutoEncryption` | assigned at [`cmd/config-current.go:532`], passed via `sse.ApplyOptions{AutoEncrypt: ...}` [`cmd/object-handlers.go:1896`] |

---


## Q2 — The specific log entries when deleting locked objects

**Question (verbatim):** *"when object locking on a bucket is enabled, what are the specific log
entries that appear when someone tries to delete the locked objects? I want you to give me runtime
log output to show this."*

### Answer thesis

With Object Lock (WORM) enabled and an object under **retention** or **legal hold**, `DeleteObject`
is refused with the `ErrObjectLocked` API error. The specific entries are the literal triple:
`Code: "InvalidRequest"`, message `Object is WORM protected and cannot be overwritten`, and HTTP
status `400` (Bad Request).

### Grounding (exact literals)

**Enforcement entry point.** `DeleteObjectHandler` ([`cmd/object-handlers.go:2509`]) wires a
retention-bypass evaluation hook and then calls the enforcement function:

```go
// cmd/object-handlers.go:2598  (hook wired)
opts.SetEvalRetentionBypassFn(func(goi ObjectInfo, gerr error) (err error) {
	...
	// cmd/object-handlers.go:2601  (enforcement invoked)
	err := enforceRetentionBypassForDelete(ctx, r, bucket, ObjectToDelete{ ... }, goi, gerr)
	...
})
```

- Claim: the delete handler defers to retention enforcement → `enforceRetentionBypassForDelete(...)`
  at [`cmd/object-handlers.go:2601`], defined at [`cmd/bucket-object-lock.go:84`].

**The three lock kinds, all enforced in `enforceRetentionBypassForDelete`** ([`cmd/bucket-object-lock.go:84`]):

- **Legal hold** → returns `ObjectLocked{}` when the object's legal-hold status is `ON`:
  `if lhold.Status.Valid() && lhold.Status == objectlock.LegalHoldOn` at
  [`cmd/bucket-object-lock.go:100`] (the status constant `LegalHoldOn = "ON"` is
  [`internal/bucket/object/lock/lock.go:86`]).
- **Compliance mode** → immutable for **all** principals (including root) until `RetainUntilDate`
  passes: `case objectlock.RetCompliance:` at [`cmd/bucket-object-lock.go:107`]
  (`RetCompliance = "COMPLIANCE"` at [`internal/bucket/object/lock/lock.go:59`]).
- **Governance mode** → bypassable **only** with both the bypass header and the
  `s3:BypassGovernanceRetention` permission: `case objectlock.RetGovernance:` at
  [`cmd/bucket-object-lock.go:124`] (`RetGovernance = "GOVERNANCE"` at
  [`internal/bucket/object/lock/lock.go:56`]); the permission is checked via
  `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, ...)` at
  [`cmd/bucket-object-lock.go:153`]. The bypass header literal is
  `AmzObjectLockBypassRetGovernance = "X-Amz-Bypass-Governance-Retention"` at
  [`internal/bucket/object/lock/lock.go:113`].

**Fail-closed on time.** Retention comparisons use trusted NTP time `objectlock.UTCNowNTP()`
(defined at [`internal/bucket/object/lock/lock.go:145`]); it is called throughout the enforcement
path (e.g. [`cmd/bucket-object-lock.go:114`], [`cmd/bucket-object-lock.go:170`]). If the time
cannot be resolved, the code fails **closed** (treats the object as locked) rather than allowing
the delete.

**The error string and its API mapping.**

```go
// cmd/object-api-errors.go:339-341
func (e ObjectLocked) Error() string {
	return "Object is WORM protected and cannot be overwritten: " + e.Bucket + "/" + e.Object + "(" + e.VersionID + ")"
}
```

```go
// cmd/api-errors.go:2298-2299  (internal error → API error code)
case ObjectLocked:
	apiErr = ErrObjectLocked
```

```go
// cmd/api-errors.go:1059-1063  (the API error definition — the "specific log entries")
ErrObjectLocked: {
	Code:           "InvalidRequest",
	Description:    "Object is WORM protected and cannot be overwritten",
	HTTPStatusCode: http.StatusBadRequest,
},
```

- Claim: the internal `ObjectLocked` error carries the WORM message →
  [`cmd/object-api-errors.go:340`].
- Claim: it maps to `ErrObjectLocked` → [`cmd/api-errors.go:2298`].
- Claim: `ErrObjectLocked` is `InvalidRequest` / `Object is WORM protected and cannot be overwritten`
  / HTTP `400` → [`cmd/api-errors.go:1059`]–[`cmd/api-errors.go:1063`]
  (`http.StatusBadRequest` is `400`).

### Reproduction

```bash
# Bucket WITH object lock
mc mb --with-lock local/lockbucket

# Place three objects under the three protections (retention set via mc — see nuance below)
mc cp compliance.txt  local/lockbucket/compliance-obj
mc retention set --mode compliance --recursive=false 3d local/lockbucket/compliance-obj
mc cp governance.txt  local/lockbucket/governance-obj
mc retention set --mode governance --recursive=false 3d local/lockbucket/governance-obj
mc cp legalhold.txt   local/lockbucket/legalhold-obj
mc legalhold set local/lockbucket/legalhold-obj

# Trace subscriber attached BEFORE the deletes
mc admin trace -v --call s3 local &

# Attempt versioned deletes via boto3 delete_object; also a governance delete WITH bypass header
python3 try_deletes.py
```

Honest nuance (reported exactly): the initial attempt to set retention **via boto3**
(`put_object_retention`) failed —

```text
An error occurred (MissingContentMD5) when calling the PutObjectRetention operation:
Missing required header for this request: Content-Md5.
```

— so retention was set with `mc retention set` instead.

### Verbatim evidence (the delete attempts)

```text
[compliance-obj]         DENIED: Code='InvalidRequest' Message='Object is WORM protected and cannot be overwritten' HTTP=400
[governance-obj]         DENIED: Code='InvalidRequest' Message='Object is WORM protected and cannot be overwritten' HTTP=400
[governance-obj/bypass]  DELETE SUCCEEDED (version c7427c35)
[legalhold-obj]          DENIED: Code='InvalidRequest' Message='Object is WORM protected and cannot be overwritten' HTTP=400
```

- Claim: a compliance-protected delete is refused with the exact triple →
  `[compliance-obj] DENIED: Code='InvalidRequest' Message='Object is WORM protected and cannot be overwritten' HTTP=400`.
- Claim: a governance-protected delete **without** bypass is refused with the same triple →
  `[governance-obj] DENIED: ... HTTP=400`.
- Claim: a governance-protected delete **with** the bypass header + permission **succeeds** →
  `[governance-obj/bypass] DELETE SUCCEEDED (version c7427c35)`.
- Claim: a legal-hold-protected delete is refused with the same triple →
  `[legalhold-obj] DENIED: ... HTTP=400`.

**Trace excerpt** — the `s3.DeleteObject` request returns `400 Bad Request`, and the XML error body
contains the specific log entries (shown for the compliance, governance-without-bypass, and
legal-hold deletes):

```text
127.0.0.1:9000 [REQUEST s3.DeleteObject] ...
127.0.0.1:9000 DELETE /lockbucket/compliance-obj?versionId=...
127.0.0.1:9000 [RESPONSE] ... 400 Bad Request ...
127.0.0.1:9000 <Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>...</Error>
```

- Claim: the response body carries the literal code and message →
  `<Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>`
  (matches [`cmd/api-errors.go:1060`]–[`cmd/api-errors.go:1061`]).

### Rationale / conclusion

The "specific log entries" that appear are the `ErrObjectLocked` API error: `InvalidRequest` /
`Object is WORM protected and cannot be overwritten` / HTTP `400`. **Compliance mode and legal hold
block all principals**; **governance mode is bypassable only** when the request carries both
`X-Amz-Bypass-Governance-Retention: true` and the caller holds `s3:BypassGovernanceRetention` — which
is exactly why the `[governance-obj/bypass]` delete succeeded while the others were denied.

### Q2 coverage note

| Named item | Evidence / citation |
| --- | --- |
| Object locking enabled | `mc mb --with-lock`; enforcement at [`cmd/bucket-object-lock.go:84`] |
| "Specific log entries" (Code / Message / HTTP) | `Code='InvalidRequest' Message='Object is WORM protected and cannot be overwritten' HTTP=400` → [`cmd/api-errors.go:1059`]–[`cmd/api-errors.go:1063`] |
| Compliance (immutable) | denied triple for `[compliance-obj]`; [`cmd/bucket-object-lock.go:107`] |
| Governance (bypassable) | `[governance-obj]` denied, `[governance-obj/bypass]` succeeded; [`cmd/bucket-object-lock.go:124`],[`cmd/bucket-object-lock.go:153`] |
| Legal hold | denied triple for `[legalhold-obj]`; [`cmd/bucket-object-lock.go:100`], `LegalHoldOn` [`internal/bucket/object/lock/lock.go:86`] |
| `enforceRetentionBypassForDelete` | [`cmd/bucket-object-lock.go:84`], invoked from [`cmd/object-handlers.go:2601`] |
| `ErrObjectLocked` | [`cmd/api-errors.go:1059`]; mapping [`cmd/api-errors.go:2298`]; `ObjectLocked.Error()` [`cmd/object-api-errors.go:340`] |
| `UTCNowNTP` (fail-closed) | [`internal/bucket/object/lock/lock.go:145`]; used at [`cmd/bucket-object-lock.go:114`] |
| Retention / legal-hold metadata (`internal/bucket/object/lock/**`) | `RetGovernance` [`internal/bucket/object/lock/lock.go:56`], `RetCompliance` [`internal/bucket/object/lock/lock.go:59`], `LegalHoldOn` [`internal/bucket/object/lock/lock.go:86`], bypass header [`internal/bucket/object/lock/lock.go:113`] |

---


## Q3 — Bit rot detection on a subsequent GET, and healing

**Question (verbatim):** *"analyze how the system handles unauthorized manual data corruption within
the storage backend. Trigger a bit rot detection event and identify the specific runtime logs
generated during a subsequent get request."*

### Answer thesis

On an erasure-coded backend, MinIO protects each shard with a bitrot hash. Manually corrupting a
shard on disk is detected by **hash verification on read**; the corrupt shard is discarded and the
object is **reconstructed from parity**, so the GET still returns correct data. A **deep-scan heal**
then explicitly reports the object as corrupted (`Red`) and repairs it (`Green`), rewriting the
shard.

> **This requires a multi-drive erasure layout.** All Q3 evidence was captured with
> `minio server /tmp/minio-data/d1..d4` (EC:2). Bit-rot protection is a **per-shard** property and
> does not apply to a single-disk backend.

### Grounding — two-tier detection plus heal

**Corruption sentinel:**

```go
// cmd/storage-errors.go:104
var errFileCorrupt = StorageErr("file is corrupted")
```

**Read path used by GET — streaming bitrot verify** returns `errFileCorrupt` on a hash mismatch:

```go
// cmd/bitrot-streaming.go:183-186
b.h.Write(buf)
if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {   // :184
	return 0, errFileCorrupt                    // :185
}
```

- Claim: a data shard that fails its hash on read yields `errFileCorrupt` →
  [`cmd/bitrot-streaming.go:185`].

**Deep-scan verify** (used by the scanner / heal): `bitrotVerify` at [`cmd/bitrot.go:158`] returns
`errFileCorrupt` on mismatch at [`cmd/bitrot.go:206`] (and at [`cmd/bitrot.go:163`],
[`cmd/bitrot.go:166`], [`cmd/bitrot.go:178`], [`cmd/bitrot.go:201`]).

**Fast/size check** (why an in-place, size-preserving corruption is caught by the *hash* check, not
the size check): `checkPart` at [`cmd/xl-storage.go:2372`] only flags a part as corrupt when it is
**truncated**:

```go
// cmd/xl-storage.go:2398-2399
if st.Size() < expectedSize {
	resp = checkPartFileCorrupt
```

- Claim: a same-size in-place corruption passes the size check → the condition is
  `if st.Size() < expectedSize` at [`cmd/xl-storage.go:2398`]; it therefore relies on the hash
  verification above to detect same-size bit rot.

**Heal-on-read enqueue** — when a GET hits `errFileCorrupt`, a background heal is queued with a
bitrot deep scan:

```go
// cmd/erasure-object.go:398-408
if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {   // :398
	healOnce.Do(func() {                                                  // :399
		globalMRFState.addPartialOp(PartialOperation{                     // :400
			Bucket:     bucket,
			Object:     object,
			VersionID:  fi.VersionID,
			...
			BitrotScan: errors.Is(err, errFileCorrupt),                   // :407
		})
	})
```

- Claim: a corrupt shard on GET queues a heal with `BitrotScan` true → [`cmd/erasure-object.go:407`].

**Heal classification + reconstruction.** `shouldHealObjectOnDisk` ([`cmd/erasure-healing.go:156`])
treats `errFileCorrupt` as heal-needed:

```go
// cmd/erasure-healing.go:157
if errors.Is(erErr, errFileNotFound) || errors.Is(erErr, errFileVersionNotFound) || errors.Is(erErr, errFileCorrupt) {
	return true, erErr
}
```

For a bitrot-failed part the healer uses the sentinel
`var errPartMissingOrCorrupt = errors.New("part missing or corrupt")` at
[`cmd/erasure-healing.go:152`], and the drive-state switch groups it under **`missing`**:

```go
// cmd/erasure-healing.go:388-389
case IsErr(reason, errFileNotFound, errFileVersionNotFound, errVolumeNotFound, errPartMissingOrCorrupt, errOutdatedXLMeta, errLegacyXLMeta):
	driveState = madmin.DriveStateMissing
```

- Claim: bitrot-corrupt shards are reported as `state:"missing"` (not `"corrupted"`) →
  the `errPartMissingOrCorrupt` case maps to `madmin.DriveStateMissing` at
  [`cmd/erasure-healing.go:389`].
- Reconstruction is performed by `healObject` at [`cmd/erasure-healing.go:258`] ("Heals an object by
  re-writing corrupt/missing erasure blocks"), using Reed-Solomon
  (`github.com/klauspost/reedsolomon v1.12.4`, [`go.mod:40`]) via the decode path `Decode` at
  [`cmd/erasure-decode.go:239`].

### Reproduction

```bash
# 4-drive erasure server
minio server /tmp/minio-data/d1 /tmp/minio-data/d2 /tmp/minio-data/d3 /tmp/minio-data/d4 &

mc mb local/bitrot-bucket
head -c 5242880 /dev/urandom > br.bin       # 5 MiB, md5 bdb53b6b727020283f96fa1e0badce7f
mc cp br.bin local/bitrot-bucket/br.bin     # split into four part.1 shards (~2621600 bytes each)

# Locate the shards and corrupt bytes IN PLACE (size unchanged)
find /tmp/minio-data/d*/bitrot-bucket/br.bin/*/part.1
dd if=/dev/urandom of=/tmp/minio-data/d1/bitrot-bucket/br.bin/<uuid>/part.1 bs=1 seek=1000 count=64 conv=notrunc

# Attach trace BEFORE the GET
mc admin trace --all -v local &
mc cp local/bitrot-bucket/br.bin /tmp/out.bin       # subsequent GET

# Explicit deep-scan heal
mc admin heal -r --scan deep --force local/bitrot-bucket
```

### Verbatim evidence (reported exactly, including the honest nuances)

**Observation A — a corrupt shard NOT in the minimal read set is not encountered.** After corrupting
**only** the `d1` shard, the GET read exactly the two data shards it needed and returned the correct
object; `d1` was neither read nor healed:

```text
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d4/bitrot-bucket/br.bin/<uuid>/part.1 ...
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d3/bitrot-bucket/br.bin/<uuid>/part.1 ...
GET md5: bdb53b6b727020283f96fa1e0badce7f   (matches original)
```

- Claim: with EC:2 the minimal read touches only the two data shards it needs, so a corrupt shard
  outside that set is not encountered on this particular GET → trace shows `ReadFileStream` on `d4`
  and `d3` only, and `GET md5: bdb53b6b727020283f96fa1e0badce7f` matches the original.

**Observation B — bitrot detection during GET (the key runtime signature).** After restoring `d1`
and corrupting the **two shards that ARE read** (`d3` + `d4`), the GET **fanned out to all four**
`part.1` shards and still returned correct data:

```text
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d4/.../part.1 ...
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d3/.../part.1 ...
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d1/.../part.1 ...    # fan-out to parity
127.0.0.1:9000 [storage.ReadFileStream] /tmp/minio-data/d2/.../part.1 ...    # fan-out to parity
GET md5: bdb53b6b727020283f96fa1e0badce7f   (still correct)
```

- Claim: the corrupt data shards failed hash verification (`errFileCorrupt` at
  [`cmd/bitrot-streaming.go:185`]), forcing the decoder to additionally read the parity shards and
  reconstruct → the trace shows the read **fanning out** from `d4`,`d3` to also include `d1`,`d2`,
  and the returned `GET md5` still matches the original.
- Honest nuance: the trace's disk error counters stay at zero —
  `total-errs-availability=0 total-errs-timeout=0` — because bit rot is a **data-integrity** error,
  not an availability/timeout error.
- Honest nuance: at the default log level, **no literal `file is corrupted` line is printed to the
  console or trace during the GET**. The `errFileCorrupt` is handled internally by the erasure
  decoder; the observable GET-path signature is the **read fan-out to parity** shown above.

**Observation C — explicit deep-scan heal surfaces and repairs the corruption:**

```text
[Red    ->  Green] bitrot-bucket/br.bin
Healed:	1/1 objects; 5 MiB in 1s
```

- Claim: the deep heal classifies the object as corrupted then repaired → `[Red    ->  Green] bitrot-bucket/br.bin`.

**Observation D — per-drive heal states (raw `--json`), grounding the `missing` classification:**

```json
{"status":"success","type":"object","name":"bitrot-bucket/br.bin",
 "before":{"color":"red","offline":0,"online":2,"missing":2,"corrupted":0,
   "drives":[{"endpoint":"/tmp/minio-data/d1","state":"ok"},{"endpoint":"/tmp/minio-data/d2","state":"ok"},{"endpoint":"/tmp/minio-data/d3","state":"missing"},{"endpoint":"/tmp/minio-data/d4","state":"missing"}]},
 "after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,
   "drives":[{"endpoint":"/tmp/minio-data/d1","state":"ok"},{"endpoint":"/tmp/minio-data/d2","state":"ok"},{"endpoint":"/tmp/minio-data/d3","state":"ok"},{"endpoint":"/tmp/minio-data/d4","state":"ok"}]},
 "size":5242880}
```

- Claim: before the heal the object is `red` with `online:2, missing:2` and the two corrupt shards
  report `state:"missing"` → `"before":{"color":"red", ... "online":2,"missing":2, ...}` with `d3`
  and `d4` at `"state":"missing"`.
- Claim: after the heal the object is `green` with `online:4` and all drives `ok` →
  `"after":{"color":"green", ... "online":4,"missing":0, ...}`.
- Honest nuance (slightly surprising): the bitrot-corrupt shards are reported as `state:"missing"`
  (not `"corrupted"`) because `shouldHealObjectOnDisk` returns `errPartMissingOrCorrupt`
  ([`cmd/erasure-healing.go:152`]), which the switch at [`cmd/erasure-healing.go:388`] groups under
  `madmin.DriveStateMissing` ([`cmd/erasure-healing.go:389`]).

**Observation E — heal restored the shard.** After the heal the corruption markers are gone and the
repaired `d3` shard is byte-identical to the pre-corruption original:

```text
$ cmp /tmp/minio-data/d3/bitrot-bucket/br.bin/<uuid>/part.1 /tmp/pre-corruption-d3-part.1
(no output — files are identical)
```

- Claim: the healed shard matches the original byte-for-byte → `cmp` produced no output (identical).

### Rationale / conclusion

MinIO handles unauthorized manual corruption in three stages: **(1)** it detects a shard hash
mismatch on read (`errFileCorrupt`, [`cmd/bitrot-streaming.go:185`]); **(2)** it transparently
reconstructs the object from parity so the GET still succeeds with correct data (read fan-out in
Observation B; decode at [`cmd/erasure-decode.go:239`]); and **(3)** it queues/executes a
bitrot-scan heal (`BitrotScan: true`, [`cmd/erasure-object.go:407`]) that rewrites the corrupt shard
from parity via `healObject` ([`cmd/erasure-healing.go:258`]), moving the object `Red -> Green`.

### Q3 coverage note

| Named item | Evidence / citation |
| --- | --- |
| Unauthorized manual data corruption in the storage backend | `dd ... conv=notrunc` on `part.1`; size-check bypass at [`cmd/xl-storage.go:2398`] |
| Triggering a bit-rot detection event | hash mismatch → `errFileCorrupt` at [`cmd/bitrot-streaming.go:185`] |
| Specific runtime logs during a subsequent GET | read fan-out to parity (Observation B) + honest note that no literal `file is corrupted` line prints at default level; explicit deep-heal `[Red -> Green]` (Observation C) + JSON drive states (Observation D) |
| `errFileCorrupt` / `file is corrupted` | [`cmd/storage-errors.go:104`] |
| `bitrotVerify` | [`cmd/bitrot.go:158`], returns at [`cmd/bitrot.go:206`] |
| Healing (`healObject`, `errors.Is(..., errFileCorrupt)`) | [`cmd/erasure-healing.go:258`], [`cmd/erasure-healing.go:157`] |
| Requirement of a multi-drive erasure layout | 4-drive server, EC:2; per-shard classification at [`cmd/erasure-healing.go:388`] |

---


## Q4 — MinIO enforces the session policy on temporary credentials (test output)

**Question (verbatim):** *"verify that when a user gets temporary credentials, minio is able to
enforce the session policy on that user. You need to give me runtime test output to prove this
behavior."*

### Answer thesis

The effective permissions of STS temporary credentials are the **intersection** of the parent
policy and the inline **session policy**. An action allowed by the parent but omitted or denied by
the session policy is **denied**.

### Grounding (exact literals)

**The intersection decision** lives inside `IsAllowedSTS` ([`cmd/iam.go:2242`]):

```go
// cmd/iam.go:2311-2313  (inside IsAllowedSTS)
hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
if hasSessionPolicy {
	return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))   // :2312
}
```

- Claim: STS access requires the session policy AND (owner-derived OR the combined parent policy) to
  allow the action — a logical intersection → [`cmd/iam.go:2312`].

**The session policy restricts even an owner-derived principal.** Inside
`isAllowedBySessionPolicy` ([`cmd/iam.go:2381`]) the code forces `IsOwner=false` before evaluating:

```go
// cmd/iam.go:2413-2417
// As the session policy exists, even if the parent is the root account, it
// must be restricted by it. So, we set `.IsOwner` to false here
// unconditionally.
sessionPolicyArgs := args
sessionPolicyArgs.IsOwner = false   // :2417
```

- Claim: even a root/owner-derived caller is restricted by an attached session policy →
  `sessionPolicyArgs.IsOwner = false` at [`cmd/iam.go:2417`].

**Session policy plumbing.** The inline session policy is parsed and size-limited on the way in:

- `maxSTSSessionPolicySize = 2048` → [`cmd/sts-handlers.go:89`]
- `func (c stsClaims) populateSessionPolicy(form url.Values) error` → [`cmd/sts-handlers.go:94`],
  enforcing the size limit at [`cmd/sts-handlers.go:123`]
- `func (sts *stsAPIHandlers) AssumeRole(...)` → [`cmd/sts-handlers.go:256`], which calls
  `claims.populateSessionPolicy(r.Form)` at [`cmd/sts-handlers.go:296`].

**The in-repo test that proves it.** `TestSTSWithDenyDeleteVersion`
([`cmd/sts-handlers_test.go:180`]) creates a parent policy that **Allows** a set of object actions
(`s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`, `s3:GetObjectVersion`, …) plus an explicit
**`Deny`** on `s3:DeleteObjectVersion`, assumes a role, and asserts the deny is enforced under the
assumed credentials (`c.mustNotDelete(ctx, minioClient, bucket, versions[0])`). Its runner is
`TestIAMInternalIDPSTSServerSuite` ([`cmd/sts-handlers_test.go:52`]), which invokes
`suite.TestSTSWithDenyDeleteVersion(c)` at [`cmd/sts-handlers_test.go:45`].

### Reproduction

The behavior was exercised through MinIO's in-process harness via a **focused wrapper** — a
temporary probe test (since **removed**, so the working tree is left clean) that reused the in-repo
`TestSuiteIAM` (`newTestSuiteIAM`, `TestSuiteCommon{serverType:"ErasureSD", signer:signerV4}`):

```bash
go test ./cmd/ -run "TestBlitzyProbe" -v -count=1 -vet=off
```

### Verbatim evidence

**(a) The real suite method passing** (the probe invoked `TestSTSWithDenyDeleteVersion` on the
harness):

```text
=== RUN   TestBlitzyProbeQ4STSDenyDeleteVersion
--- PASS: TestBlitzyProbeQ4STSDenyDeleteVersion (0.47s)
```

- Claim: the explicit-`Deny` session/parent policy is enforced under assumed STS credentials →
  `--- PASS: TestBlitzyProbeQ4STSDenyDeleteVersion (0.47s)`.

**(b) Explicit intersection proof** — parent policy Allows Put/Get/Delete; the inline **session**
policy Allows only Put/Get:

```text
=== RUN   TestBlitzyProbeQ4SessionPolicyIntersection
    zz_blitzy_probe_test.go:79: Q4b PUT allowed (parent AND session both permit s3:PutObject) -> OK
    zz_blitzy_probe_test.go:82: Q4b DELETE (parent allows, session omits) -> err = Access Denied.
--- PASS: TestBlitzyProbeQ4SessionPolicyIntersection (0.63s)
```

- Claim: an action permitted by **both** parent and session policy is allowed →
  `Q4b PUT allowed (parent AND session both permit s3:PutObject) -> OK`.
- Claim: an action the parent allows but the session policy **omits** is denied →
  `Q4b DELETE (parent allows, session omits) -> err = Access Denied.`

**(c) Overall runner result:**

```text
ok  	github.com/minio/minio/cmd	2.248s
```

with `TEST_EXIT=0`.

### Rationale / conclusion

`PutObject` is permitted because **both** the parent and the session policy allow it; `DeleteObject`
is denied (`Access Denied.`) because the session policy **omits** it even though the parent allows
it — proving the session policy is enforced as the **intersection** at [`cmd/iam.go:2312`]. Because
`isAllowedBySessionPolicy` forces `IsOwner=false` ([`cmd/iam.go:2417`]), even a root/owner-derived
principal is restricted by an attached session policy.

### Q4 coverage note

| Named item | Evidence / citation |
| --- | --- |
| Temporary credentials (STS `AssumeRole`) | `AssumeRole` [`cmd/sts-handlers.go:256`]; probe assumes a role via `cr.STSAssumeRole` |
| Session policy enforcement | `--- PASS: TestBlitzyProbeQ4STSDenyDeleteVersion (0.47s)` |
| Intersection semantics | `Q4b DELETE (parent allows, session omits) -> err = Access Denied.`; [`cmd/iam.go:2312`] |
| Runtime TEST output (PASS markers + assertion lines) | the `=== RUN` / `--- PASS` markers and `zz_blitzy_probe_test.go:79/82` lines above; `ok github.com/minio/minio/cmd 2.248s` |
| `IsAllowedSTS` | [`cmd/iam.go:2242`] |
| `IsOwner=false` behavior | [`cmd/iam.go:2417`] |
| `maxSTSSessionPolicySize = 2048` | [`cmd/sts-handlers.go:89`] |
| `TestSTSWithDenyDeleteVersion` / `TestIAMInternalIDPSTSServerSuite` | [`cmd/sts-handlers_test.go:180`] / [`cmd/sts-handlers_test.go:52`] (invoked at [`cmd/sts-handlers_test.go:45`]) |

---


## Q5 — A basic user cannot self-promote to console admin via user mappings (test output + root cause)

**Question (verbatim):** *"Show me test output to prove that a user with basic access cannot promote
themselves to a console admin by modifying the user mappings. Identify the root cause of the user
mappings modification behavior that you observe."*

> **Clarification:** "console admin" here means the **administrative privilege level** (the ability
> to perform admin actions), not building or modifying a UI. This is a security-behavior
> investigation.

### Answer thesis

A basic (S3-only) user **cannot** self-promote. The admin user-mapping APIs are gated on admin
actions the basic user lacks (→ `403 AccessDenied`), and even the self-service `AddUser` path
returns `200` **without attaching any policy**, so no privilege is gained.

### Grounding (exact literals)

**The admin user-mapping APIs and the admin actions they require:**

- `AddUser` → [`cmd/admin-handlers-users.go:444`], requires `policy.CreateUserAdminAction`
  ([`cmd/admin-handlers-users.go:505`]).
- `SetPolicyForUserOrGroup` → [`cmd/admin-handlers-users.go:1770`], requires
  `policy.AttachPolicyAdminAction` via `validateAdminReq` ([`cmd/admin-handlers-users.go:1773`]).
- `AttachDetachPolicyBuiltin` → [`cmd/admin-handlers-users.go:1908`], requires
  `policy.AttachPolicyAdminAction` ([`cmd/admin-handlers-users.go:1912`]).

**Admin authorization and the denial error.** `checkAdminRequestAuth` ([`cmd/auth-handler.go:189`])
calls `globalIAMSys.IsAllowed(...)` ([`cmd/auth-handler.go:194`]) and returns `ErrAccessDenied` when
the action is not allowed:

```go
// cmd/auth-handler.go:194-206
if globalIAMSys.IsAllowed(policy.Args{ ... Action: policy.Action(action), ... }) {
	return cred, ErrNone
}
return cred, ErrAccessDenied   // :206
```

`validateAdminReq` ([`cmd/admin-handler-utils.go:37`]) is the entry point that writes that error to
the client. The error itself is HTTP `403`:

```go
// cmd/api-errors.go:539-543
ErrAccessDenied: {
	Code:           "AccessDenied",
	Description:    "Access Denied.",
	HTTPStatusCode: http.StatusForbidden,
},
```

- Claim: an admin call the caller is not allowed to make returns `ErrAccessDenied` →
  [`cmd/auth-handler.go:206`].
- Claim: `ErrAccessDenied` is `AccessDenied` / `Access Denied.` / HTTP `403` →
  [`cmd/api-errors.go:539`]–[`cmd/api-errors.go:543`] (`http.StatusForbidden` is `403`).

**ROOT CAUSE — two parts.**

**(1) Deny-by-default authorization.** `IsAllowed` ([`cmd/iam.go:2437`]) evaluates a regular user
via `PolicyDBGet`, and if the user has **no** matching policy it returns `false`:

```go
// cmd/iam.go:2476-2482
if len(policies) == 0 {
	// No policy found.               // :2477
	return false                       // deny-by-default
}
...
return sys.GetCombinedPolicy(policies...).IsAllowed(args)   // :2482
```

- Claim: a user whose policy set grants no admin action is denied by default →
  `if len(policies) == 0 { return false }` at [`cmd/iam.go:2476`]; otherwise the combined policy is
  evaluated at [`cmd/iam.go:2482`] and returns `false` for an admin action a basic policy does not
  grant. **There is no self-promotion bypass.**

**(2) The self-service `AddUser` path does not attach a policy.** `AddUser` sets
`checkDenyOnly=true` when the target access key is the caller's own key, which lets a user update
**their own** secret/status and can return `200`:

```go
// cmd/admin-handlers-users.go:495-499
checkDenyOnly := false
if accessKey == cred.AccessKey {
	// Check that there is no explicit deny - otherwise it's allowed
	// to change one's own password.
	checkDenyOnly = true
}
```

But `AddUser` **never applies a policy**. `CreateUser` ([`cmd/iam.go:1340`]) →
`store.AddUser` ([`cmd/iam-store.go:2659`]) builds the user identity from **only** the access key,
secret key, and status — **ignoring** `ureq.Policy`:

```go
// cmd/iam-store.go:2672-2676 (excerpt)
u := newUserIdentity(auth.Credentials{
	AccessKey: accessKey,
	SecretKey: ureq.SecretKey,
	Status:    func() string { ... }(),
})
```

- Claim: a self add-user with `PolicyName:"consoleAdmin"` returns `200` but grants nothing, because
  the persisted identity carries only `AccessKey`/`SecretKey`/`Status` → [`cmd/iam-store.go:2659`],
  [`cmd/iam-store.go:2672`]. Policy-to-user mapping is a **separate, admin-gated** operation.

### Reproduction

The behavior was exercised through the in-process harness via **focused wrappers** (temporary probe
tests, since **removed**):

```bash
go test ./cmd/ -run "TestBlitzyProbe" -v -count=1 -vet=off
```

The Q5 probe wrapped the real, permanent in-repo test `TestUserPolicyEscalationBug`
([`cmd/admin-handlers-users_test.go:313`]), which runs under the `TestSuiteIAM` harness (its runner
`TestIAMInternalIDPServerSuite` at [`cmd/admin-handlers-users_test.go:192`] invokes it at
[`cmd/admin-handlers-users_test.go:205`]). (A separate admin harness, `prepareAdminErasureTestBed`
at [`cmd/admin-handlers_test.go:50`], backs other admin tests; the user→policy mapping persistence
backends are [`cmd/iam-store.go`], `cmd/iam-object-store.go`, and `cmd/iam-etcd-store.go`.)

### Verbatim evidence

**(a) The real escalation-bug test passing:**

```text
=== RUN   TestBlitzyProbeQ5UserPolicyEscalation
--- PASS: TestBlitzyProbeQ5UserPolicyEscalation (0.48s)
```

What it asserts (from [`cmd/admin-handlers-users_test.go:313`]): a basic user self-sends
`PUT /minio/admin/v3/add-user?accessKey=<self>` with a body of
`madmin.UserInfo{SecretKey: ..., PolicyName: "consoleAdmin", Status: madmin.AccountEnabled}`; the
request returns **200** (the test would fail at `if resp.StatusCode != 200`), but the user **still**
gets `Access Denied.` on a privileged op (`uClient.RemoveBucket`). If it **had** escalated, the test
would fail with the literal `User was able to escalate privileges (Err=%v)!` at
[`cmd/admin-handlers-users_test.go:422`]. It passed → no escalation.

- Claim: the self add-user with `PolicyName:"consoleAdmin"` returns `200` yet grants nothing →
  the test passes rather than failing at [`cmd/admin-handlers-users_test.go:422`].

**(b) Explicit admin-API denial proof:**

```text
=== RUN   TestBlitzyProbeQ5AdminAPIDenied
    zz_blitzy_probe_test.go:121: Q5b AttachPolicy(consoleAdmin) as basic user -> err = Access Denied.
    zz_blitzy_probe_test.go:128: Q5b AddUser(newadmin) as basic user -> err = Access Denied.
--- PASS: TestBlitzyProbeQ5AdminAPIDenied (0.38s)
```

- Claim: a basic user calling the policy-attach admin API is denied →
  `Q5b AttachPolicy(consoleAdmin) as basic user -> err = Access Denied.` (gate at
  [`cmd/admin-handlers-users.go:1912`] → [`cmd/auth-handler.go:206`]).
- Claim: a basic user calling `add-user` to create a **new** admin is denied →
  `Q5b AddUser(newadmin) as basic user -> err = Access Denied.` (the non-self path requires
  `CreateUserAdminAction`, [`cmd/admin-handlers-users.go:505`]).

**(c) Overall result:**

```text
ok  	github.com/minio/minio/cmd	2.248s
```

with `TEST_EXIT=0`.

### Rationale / conclusion

The basic user cannot self-promote for two independent reasons:

1. **Direct admin user-mapping APIs** (`AttachDetachPolicyBuiltin`, `SetPolicyForUserOrGroup`,
   `AddUser` for another key) require admin actions the user lacks, so `checkAdminRequestAuth`
   returns `ErrAccessDenied` (403) — evidenced by
   `Q5b AttachPolicy(consoleAdmin) as basic user -> err = Access Denied.` The **root cause** is the
   **deny-by-default** authorization in `IAMSys.IsAllowed`:
   `if len(policies) == 0 { return false }` at [`cmd/iam.go:2476`], with the combined-policy
   evaluation at [`cmd/iam.go:2482`] returning `false` for admin actions a basic policy does not
   grant.
2. **The permitted self add-user path does not attach a policy** — `store.AddUser` ignores
   `ureq.Policy` and persists only access key / secret / status ([`cmd/iam-store.go:2659`],
   [`cmd/iam-store.go:2672`]) — so `add-user` with `PolicyName:"consoleAdmin"` returns 200 but grants
   nothing.

Self-promotion is blocked at admin authorization; there is **no special-case bypass**.

### Q5 coverage note

| Named item | Evidence / citation |
| --- | --- |
| Basic-access user | policy allows only ListBucket/PutObject/GetObject in `TestUserPolicyEscalationBug` [`cmd/admin-handlers-users_test.go:313`] |
| Self-promotion to "console admin" (admin privilege) | `--- PASS: TestBlitzyProbeQ5UserPolicyEscalation`; no escalation (assertion [`cmd/admin-handlers-users_test.go:422`]) |
| Modifying user mappings (`AddUser` / `SetPolicyForUserOrGroup` / `AttachDetachPolicyBuiltin`) | [`cmd/admin-handlers-users.go:444`] / [`cmd/admin-handlers-users.go:1770`] / [`cmd/admin-handlers-users.go:1908`] |
| Test output proving denial | `Q5b AttachPolicy(consoleAdmin) as basic user -> err = Access Denied.`; `Q5b AddUser(newadmin) as basic user -> err = Access Denied.` |
| `403 AccessDenied` (`Code:"AccessDenied"`, `Description:"Access Denied."`) | [`cmd/api-errors.go:539`]–[`cmd/api-errors.go:543`]; returned at [`cmd/auth-handler.go:206`] |
| ROOT CAUSE — deny-by-default `if len(policies)==0 { return false }` | [`cmd/iam.go:2476`]; combined eval [`cmd/iam.go:2482`] |
| ROOT CAUSE — `AddUser` ignores `ureq.Policy` | [`cmd/iam-store.go:2659`], [`cmd/iam-store.go:2672`] |
| Admin actions `CreateUserAdminAction` / `AttachPolicyAdminAction` | [`cmd/admin-handlers-users.go:505`] / [`cmd/admin-handlers-users.go:1773`],[`cmd/admin-handlers-users.go:1912`] |
| Harness `prepareAdminErasureTestBed` | [`cmd/admin-handlers_test.go:50`] (separate admin harness); escalation test itself runs under `TestSuiteIAM` [`cmd/admin-handlers-users_test.go:192`] |

---


## Final Coverage Pass

Re-reading each question and confirming every named item is addressed, with the exact evidence
line(s) and `file:line` citation(s) that answer it.

**Q1 — Encryption precedence over broad write permission (unencrypted upload):**
- Bucket encryption requirement enforced independently → `Apply` sets `AES256` at
  [`internal/bucket/encryption/bucket-sse-config.go:148`].
- Broad write permission (`s3:PutObject` / `readwrite`) is only the access gate →
  [`cmd/object-handlers.go:1836`], defined [`cmd/auth-handler.go:749`].
- Unencrypted upload still encrypted → `REQUEST SSE headers actually sent by client: NONE`,
  `HTTP status: 200`, `RESPONSE x-amz-server-side-encryption: AES256`.
- Runtime execution sequence in the trace → tracer first [`cmd/routers.go:60`] → auth gate
  [`cmd/object-handlers.go:1836`] → SSE apply [`cmd/object-handlers.go:1895`]; trace emission
  [`cmd/http-tracer.go:69`],[`cmd/http-tracer.go:172`], subscriber gate [`cmd/http-tracer.go:92`].
- Injected header literal `X-Amz-Server-Side-Encryption: AES256` →
  [`internal/http/headers.go:142`],[`internal/http/headers.go:152`].

**Q2 — Object Lock delete log entries:**
- The specific entries → `Code='InvalidRequest' Message='Object is WORM protected and cannot be
  overwritten' HTTP=400`, defined at [`cmd/api-errors.go:1059`]–[`cmd/api-errors.go:1063`].
- Compliance / governance / legal hold → `[compliance-obj]`, `[governance-obj]`,
  `[governance-obj/bypass] DELETE SUCCEEDED`, `[legalhold-obj]`; enforcement
  [`cmd/bucket-object-lock.go:84`] (compliance [`:107`], governance [`:124`], legal hold [`:100`]).
- `enforceRetentionBypassForDelete` [`cmd/bucket-object-lock.go:84`]; `ErrObjectLocked`
  [`cmd/api-errors.go:1059`]; `UTCNowNTP` fail-closed [`internal/bucket/object/lock/lock.go:145`].

**Q3 — Bit rot detection on GET + healing:**
- Unauthorized manual corruption → `dd ... conv=notrunc`; size-check bypass
  [`cmd/xl-storage.go:2398`].
- Detection event → `errFileCorrupt` on hash mismatch [`cmd/bitrot-streaming.go:185`]
  ([`cmd/storage-errors.go:104`]).
- Runtime logs on the subsequent GET → read fan-out to parity (Observation B) with correct
  `GET md5: bdb53b6b727020283f96fa1e0badce7f`; honest note that no literal `file is corrupted` line
  prints at default level; explicit deep-heal `[Red    ->  Green] bitrot-bucket/br.bin` and JSON
  drive states (`"before":{"color":"red",...}` / `"after":{"color":"green",...}`).
- Healing → `healObject` [`cmd/erasure-healing.go:258`]; `errors.Is(..., errFileCorrupt)`
  [`cmd/erasure-healing.go:157`]; `missing` classification [`cmd/erasure-healing.go:388`].
- Multi-drive erasure layout required → 4-drive server, EC:2.

**Q4 — STS session policy enforcement (test output):**
- Temporary credentials via `AssumeRole` [`cmd/sts-handlers.go:256`].
- Session policy enforced (intersection) → `Q4b DELETE (parent allows, session omits) -> err =
  Access Denied.`; decision at [`cmd/iam.go:2312`].
- Test output → `--- PASS: TestBlitzyProbeQ4STSDenyDeleteVersion (0.47s)`,
  `--- PASS: TestBlitzyProbeQ4SessionPolicyIntersection (0.63s)`,
  `ok  github.com/minio/minio/cmd  2.248s`.
- `IsAllowedSTS` [`cmd/iam.go:2242`]; `IsOwner=false` [`cmd/iam.go:2417`];
  `maxSTSSessionPolicySize = 2048` [`cmd/sts-handlers.go:89`]; `TestSTSWithDenyDeleteVersion`
  [`cmd/sts-handlers_test.go:180`] / runner [`cmd/sts-handlers_test.go:52`].

**Q5 — Basic user cannot self-promote via user mappings (test output + root cause):**
- Test output proving denial → `--- PASS: TestBlitzyProbeQ5UserPolicyEscalation (0.48s)`,
  `Q5b AttachPolicy(consoleAdmin) as basic user -> err = Access Denied.`,
  `Q5b AddUser(newadmin) as basic user -> err = Access Denied.`
- `403 AccessDenied` → [`cmd/api-errors.go:539`]–[`cmd/api-errors.go:543`], returned
  [`cmd/auth-handler.go:206`].
- User-mapping APIs → `AddUser` [`cmd/admin-handlers-users.go:444`], `SetPolicyForUserOrGroup`
  [`cmd/admin-handlers-users.go:1770`], `AttachDetachPolicyBuiltin`
  [`cmd/admin-handlers-users.go:1908`].
- **Root cause** → deny-by-default `if len(policies) == 0 { return false }` [`cmd/iam.go:2476`]
  (+ combined eval [`cmd/iam.go:2482`]) and `AddUser` ignoring `ureq.Policy`
  [`cmd/iam-store.go:2659`],[`cmd/iam-store.go:2672`].

### Investigation integrity statement

- **(a)** All runtime evidence in this document was captured against a **live Go 1.23.2 build** of
  the MinIO server (`Runtime: go1.23.2 linux/amd64`), over a 4-drive erasure set for Q1–Q3 and
  MinIO's in-process `TestSuiteIAM` harness for Q4–Q5.
- **(b)** The investigation was **read-only**. No existing repository source file was modified,
  added to, or deleted.
- **(c)** The **only** file added to the repository is this document
  (`blitzy/documentation/minio_c07e5b49d477.md`). Temporary observation artifacts — the server data
  directories, the deliberately corrupted shard files, and the temporary Go probe test
  (`zz_blitzy_probe_test.go`) — were removed, and **`go.mod` / `go.sum` are untouched**.

