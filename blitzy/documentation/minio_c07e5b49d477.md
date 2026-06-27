# MinIO Runtime Security Investigation — branch `minio_c07e5b49d477` (HEAD `c07e5b49d`)

> A code-as-truth investigation report answering five security questions about the MinIO
> object storage server. Every behavioral claim is anchored to a verified `file:line` at
> HEAD `c07e5b49d477b0774f23db3b290745aef8c01bd2` and confirmed by running the system.

---

## Preamble

### Scope & constraints

This document is the **single deliverable** of this task. It answers five distinct
questions about MinIO's runtime security behavior, each backed by captured runtime evidence
and traced to its precise source-code cause.

The following constraints governed the entire investigation and are honored throughout:

1. **The MinIO source tree is STRICTLY READ-ONLY.** This Markdown file (in the *destination*
   repository's `blitzy/documentation/` directory) is the **only** file created by the task.
   No file under `cmd/**`, `internal/**`, `docs/**`, `Makefile`, `go.mod`/`go.sum`,
   `.github/**`, or any test was added, modified, or deleted.
2. **Code-as-truth, no assumptions.** Every claim cites a verified `file:line` at HEAD
   `c07e5b49d` **and** is confirmed by running the system (server trace/log output for
   Requirements 1–3; Go test output for Requirements 4–5).
3. **Ephemeral artifacts removed; system tools left in place.** The genuinely temporary
   artifacts this task produced — the compiled `./minio` binary (built at the repo root and
   git-ignored), the throwaway server data directories and ephemeral STS reproduction module
   under `/tmp`, and the scratch scripts and captured trace/test logs — were all deleted after
   the evidence was captured. The **Go 1.23.12 toolchain** (`/usr/local/go`) and the **`mc`**
   client (`/usr/local/bin/mc`) are external, system-level tools *outside* the repository: they
   were used, not created or deleted, by this task. The end-state `git status --porcelain` in
   the source tree is **empty** (see [§6](#6-cleanup--source-tree-integrity)).

### Environment

| Component | Value |
|-----------|-------|
| Module | `github.com/minio/minio` (`go.mod:L1`) |
| Go toolchain | **Go 1.23.12** — the highest documented `go 1.23` patch satisfying the `go 1.23` directive (`go.mod:L3`) |
| Build command | `make build` → `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio` (`Makefile:L177-L179`) |
| Server topology | `./minio server /tmp/mdata/d{1...4}` — a single-node 4-drive erasure set, under `/tmp` |
| Admin/runtime client | external `mc` client `RELEASE.2025-08-13T08-35-41Z` (for `mc admin trace` + policy setup) |
| Raw S3 client | `boto3` (used to issue requests that deliberately do **not** request SSE, and to set Object-Lock retention) |
| Test build tags | `kqueue,dev` (`Makefile:L53`) |

### Methodology

For each question we (a) reproduce the scenario against a live, source-built MinIO; (b)
capture the runtime evidence — `mc admin trace -v` output and/or server log lines for
Requirements 1–3, Go test PASS output for Requirements 4–5; (c) trace the observed behavior
to the exact `file:line` in the read-only source that produces it; and (d) explain *why* the
behavior occurs. Server-side tracing is produced by `httpTracerMiddleware`
(`cmd/http-tracer.go:L69`) and surfaced to operators via `mc admin trace`.

### ⚠️ Code-vs-AAP corrections (CODE WINS)

During verification, four statements in the originating plan (AAP) were found to disagree
with the actual source at HEAD `c07e5b49d`. In every case **the code is authoritative** and
this report documents the *actual* behavior. They are summarized here and repeated inline in
the relevant sections.

| # | Topic | AAP prose said… | Code/runtime shows… (CODE WINS) |
|---|-------|-----------------|---------------------------------|
| **1** | Req 2 — Object-Lock HTTP status | `ErrObjectLocked` "returned as HTTP 403" | `cmd/api-errors.go:L1059-L1063` defines it as `Code:"InvalidRequest"`, `HTTPStatusCode: http.StatusBadRequest` ⇒ **HTTP 400, not 403** (confirmed at runtime). Enum at `:L206`; mapping `ObjectLocked`→`ErrObjectLocked` at `:L2298-L2299`. |
| **2** | Req 5 — file attribution | `SetPolicyForUserOrGroup` at `cmd/iam.go:L1770` | `cmd/iam.go:L1770` is **LDAP DN-normalization** code. `SetPolicyForUserOrGroup` is a deprecated admin handler at **`cmd/admin-handlers-users.go:L1770`** (gated by `policy.AttachPolicyAdminAction` `:L1773`). The IAM-store policy mutator is `PolicyDBSet` at `cmd/iam.go:L1928`. The Req 5 **root cause is unaffected**; only the admin-gating citation is corrected. |
| **3** | Req 4 — constant location | `maxSTSSessionPolicySize` (2048) lives in `cmd/globals.go` | The constant is defined in **`cmd/sts-handlers.go:L89`** (used at `:L122-L123`); it is **not** in `cmd/globals.go`. The value `2048` is correct. |
| **4** | Req 4 & 5 — test invocation & evidence | `go test … -run TestSTS ./cmd` / `-run TestUserPolicyEscalationBug ./cmd` | Both are **methods on the `*TestSuiteIAM` suite**, not top-level test functions; the AAP commands yield `ok … [no tests to run]` (suite runners: `TestIAMInternalIDPSTSServerSuite` / `TestIAMInternalIDPServerSuite`). For **Req 5** the suite runner is the proof (PASS shown). For **Req 4**, code-as-truth revealed that *no* in-tree STS test supplies an inline session `Policy` — so `TestSTS` proves only inherited parent-policy behavior; session-policy **intersection** is therefore proven by a dedicated ephemeral `/tmp` reproduction (§4), not by `TestSTS`. |

### Master `file:line` anchor map

| Req | Behavior | Primary anchors |
|-----|----------|-----------------|
| 1 | Encryption enforced *after* authorization | `cmd/object-handlers.go:L1745` (handler), `:L1836` (authz), `:L1893-L1897` (SSE apply); `cmd/iam.go:L2437` (`IsAllowed`); `internal/bucket/encryption/bucket-sse-config.go:L135-L151`; `internal/crypto/auto-encryption.go:L31,L37`; `cmd/http-tracer.go:L69` |
| 2 | Object-Lock DELETE enforcement | `cmd/bucket-object-lock.go:L54,L84,L101,L117,L121,L143,L147,L153,L245`; `cmd/object-handlers.go:L2509,L2601`; `cmd/bucket-handlers.go:L416,L573`; `cmd/api-errors.go:L206,L1059-L1063,L2298-L2299` |
| 3 | Bitrot detection + heal on read | `cmd/bitrot.go:L158`; `cmd/erasure-object.go:L398,L407`; `cmd/erasure-healing.go:L152,L238,L243`; `cmd/xl-storage.go:L2678,L2687` |
| 4 | STS session-policy intersection | `cmd/iam.go:L2242,L2136,L2310-L2312,L2317,L2381,L2386`; `cmd/sts-handlers.go:L89,L122-L123`. In-tree STS tests supply **no** inline session policy (`cmd/sts-handlers_test.go:L130,L253,L357,L447,L546,L593` set only AccessKey/SecretKey/Location; `L393,L473` / `L478,L573` prove inherited parent-policy behavior only); inline-session-policy intersection is proven by an ephemeral `/tmp` reproduction (see §4) |
| 5 | Privilege-escalation prevention | `cmd/admin-handlers-users.go:L444,L495-L542`; `cmd/iam-store.go:L2659,L2672`; `cmd/iam.go:L1340,L1928`; `cmd/admin-handlers-users_test.go:L192,L205,L313,L422` |

---

## 1. Encryption precedence vs. broad IAM write permission

### The Question (verbatim)

> "I am investigating Minio's implementation of policy evaluation logic and server side encryption. I wonder what happens when a bucket level encryption requirement takes precedence over a user's broad write permissions during an unencrypted upload. Identify the specific runtime execution sequence captured in the server trace logs."

### Reproduction

The server was started with KMS auto-encryption **on** and a local KMS key, so that any
request not already requesting SSE is forced to SSE-S3/SSE-KMS at rest:

```bash
KEY=$(head -c 32 /dev/urandom | base64)
export MINIO_KMS_SECRET_KEY="minio-test-key:$KEY"
export MINIO_KMS_AUTO_ENCRYPTION=on
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
./minio server /tmp/mdata/d{1...4} --address 127.0.0.1:9100
```

A user holding **broad write permission** (`s3:*`) was created and a bucket provisioned:

```bash
mc alias set local http://127.0.0.1:9100 minioadmin minioadmin
mc mb local/databucket
# broadwrite.json: {"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}]}
mc admin policy create local broadwrite broadwrite.json
mc admin user add local writer writer12345
mc admin policy attach local broadwrite --user writer
```

Tracing was attached, then a **truly unencrypted** `PutObject` was issued as `writer` via
`boto3` with **no** `ServerSideEncryption` parameter (the client neither requests nor signs
any SSE header), followed by a HEAD to confirm at-rest encryption:

```bash
mc admin trace -v --call s3 local        # default --call is s3; -v prints full req/resp
# boto3: s3.put_object(Bucket="databucket", Key="plain-true.txt", Body=b"...plaintext...")
#        (NO ServerSideEncryption kwarg)
# boto3: s3.head_object(Bucket="databucket", Key="plain-true.txt")
```

### Captured Evidence

`boto3` client view — the client requested **no** encryption, yet the stored object is
SSE-KMS:

```text
PUT HTTPStatusCode: 200
PUT resp x-amz-server-side-encryption: aws:kms
PUT resp x-amz-server-side-encryption-aws-kms-key-id: arn:aws:kms:minio-test-key
HEAD x-amz-server-side-encryption: aws:kms
HEAD ServerSideEncryption field: aws:kms | SSEKMSKeyId: arn:aws:kms:minio-test-key
```

`mc admin trace -v` block for the `PutObject` call. The **REQUEST** shows
`X-Amz-Server-Side-Encryption: aws:kms`, but the Authorization `SignedHeaders` list does
**not** include it — i.e. the client never signed or sent that header; the **server**
injected it:

```text
127.0.0.1:9100 [REQUEST s3.PutObject] [Client IP: 127.0.0.1]
127.0.0.1:9100 PUT /databucket/plain-true.txt
127.0.0.1:9100 Content-Type: text/plain
127.0.0.1:9100 X-Amz-Content-Sha256: 9db752b9...e6be
127.0.0.1:9100 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=writer/.../s3/aws4_request,
               SignedHeaders=content-type;host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm, Signature=...
127.0.0.1:9100 [RESPONSE] [ Duration 64.054ms ... ]
127.0.0.1:9100 200 OK
127.0.0.1:9100 X-Amz-Server-Side-Encryption: aws:kms
127.0.0.1:9100 X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id: arn:aws:kms:minio-test-key
```

**Key observation.** The `SignedHeaders` set proves the SSE header was **not** part of the
client's signed request — it was injected server-side by auto-encryption. The object is
persisted as SSE-KMS even though the upload itself carried no encryption directive, and the
broad `s3:*` permission did nothing to bypass this.

### Code-Truth Root Cause

The execution sequence inside `PutObjectHandler` (`cmd/object-handlers.go:L1745`) is two
**orthogonal, sequential gates** — authorization first, encryption second:

1. **Authorization FIRST** — `isPutActionAllowed(ctx, rAuthType, bucket, object, r, policy.PutObjectAction)`
   at **`cmd/object-handlers.go:L1836`** routes to the IAM gate `IsAllowed` at
   **`cmd/iam.go:L2437`**. The broad `s3:*` grant authorizes the *action* here — and only here.
2. **Encryption ENFORCED AFTER** — further down the *same* handler, at
   **`cmd/object-handlers.go:L1893-L1897`**, the bucket SSE configuration is fetched and
   applied to the request headers:
   ```go
   // Check if bucket encryption is enabled
   sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
   sseConfig.Apply(r.Header, sse.ApplyOptions{AutoEncrypt: globalAutoEncryption})
   ```
3. **Header injection** — `func (b *BucketSSEConfig) Apply(headers http.Header, opts ApplyOptions)`
   at **`internal/bucket/encryption/bucket-sse-config.go:L135`**:
   - `:L136-L138` — `if crypto.Requested(headers) { return }` (if the client *already*
     requested SSE, the existing choice is left untouched);
   - `:L140-L141` — when `opts.AutoEncrypt` is true and there is no explicit bucket config,
     it calls `headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)` → sets
     `X-Amz-Server-Side-Encryption: aws:kms` **on the request**;
   - `:L148` / `:L150-L151` — when a bucket default-SSE config exists, it sets `AES256`
     (SSE-S3) or `aws:kms` plus the configured KMS key id.
4. `globalAutoEncryption` is populated by `LookupAutoEncryption()` at
   **`internal/crypto/auto-encryption.go:L37`** from the `EnvKMSAutoEncryption` env var
   (`:L31`); the comment at `:L26-L29` states auto-encryption "turns any non-SSE-C request
   into an SSE-S3 request."
5. Header constants live in `internal/http/headers.go`:
   `AmzServerSideEncryption = "X-Amz-Server-Side-Encryption"` (`:L142`),
   `AmzEncryptionKMS = "aws:kms"` (`:L153`), `AmzServerSideEncryptionKmsID` (`:L143`).
6. The trace surface that produced the evidence above is `httpTracerMiddleware`
   (**`cmd/http-tracer.go:L69`**), registered early in the middleware chain and consumed by
   `mc admin trace`.

| Step | Source anchor | Role |
|------|---------------|------|
| Authorization | `cmd/object-handlers.go:L1836` → `cmd/iam.go:L2437` | Grants the *action* (`s3:*` matches) |
| SSE apply | `cmd/object-handlers.go:L1893-L1897` | Invokes bucket-SSE/auto-encrypt **after** authz |
| Header inject | `internal/bucket/encryption/bucket-sse-config.go:L140-L151` | Forces `aws:kms`/`AES256` on the request |
| Toggle source | `internal/crypto/auto-encryption.go:L31,L37` | Reads `MINIO_KMS_AUTO_ENCRYPTION` |
| Trace surface | `cmd/http-tracer.go:L69` | Feeds `mc admin trace` |

### Rationale / Thinking

Authorization and encryption are **independent gates evaluated in sequence**: the policy
engine decides only whether the *action* is permitted (`cmd/object-handlers.go:L1836` →
`cmd/iam.go:L2437`); the SSE layer *then* independently forces encryption-at-rest
(`:L1893-L1897` → `bucket-sse-config.go:L140-L151`). Because line `L1836` strictly precedes
line `L1893` in the same handler, a broad write permission can never "outrun" or bypass the
encryption requirement — at most it allows the principal to *store* an object, which the
server stores **encrypted**. The `SignedHeaders` evidence is the clincher: the SSE header
was server-injected, not client-supplied, so the upload was genuinely unencrypted on the
wire yet SSE-KMS at rest.

Documentation corroboration (in-repo): `docs/security/README.md` describes SSE-S3 as a mode
that "en/decrypts an object with a secret key managed by a KMS," and notes a valid KMS
configuration is required. Trace-tooling corroboration: `docs/debugging/README.md` states
"HTTP tracing can be enabled by using `mc admin trace`"; MinIO's `mc` CLI confirms the
`--call` flag defaults to `s3` ("(default: s3)") and `-v` prints the full request/response
("print verbose trace").

---

## 2. Object Lock DELETE enforcement

### The Question (verbatim)

> "I wonder when object locking on a bucket is enabled, what are the specific log entries that appear when someone tries to delete the locked objects? I want you to give me runtime log output to show this."

### Reproduction

A lock-enabled bucket was created, an object written under a **COMPLIANCE** retention one day
in the future, and a delete then attempted against that version:

```bash
mc mb --with-lock local/lockbucket
# boto3 put_object(Bucket="lockbucket", Key="single.txt", Body=b"...",
#                  ObjectLockMode="COMPLIANCE",
#                  ObjectLockRetainUntilDate=<now + 1 day>)
```

Two delete paths were exercised:

- **Single-object delete** — `boto3 delete_object(Bucket, Key, VersionId=<version>)`.
- **Multi-object delete** — `mc rm …` which issues `s3.DeleteMultipleObjects`
  (`POST /lockbucket/?delete=`).

### Captured Evidence

Single-object DELETE — the `boto3` surfaced error and the matching trace:

```text
HTTP_STATUS  : 400
ERROR_CODE   : InvalidRequest
ERROR_MESSAGE: Object is WORM protected and cannot be overwritten

[REQUEST s3.DeleteObject] DELETE /lockbucket/single.txt?versionId=<id>
[RESPONSE] 400 Bad Request
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>...</Error>
```

Multi-object delete (`mc rm` → `s3.DeleteMultipleObjects`) — the batch POST returns
**HTTP 200** with a per-object `<Error>` element inside the result body:

```text
[REQUEST s3.DeleteMultipleObjects] POST /lockbucket/?delete=
[RESPONSE] 200 OK
<DeleteResult ...><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>worm.txt</Key><VersionId>...</VersionId></Error></DeleteResult>
```

> **🚩 Discrepancy #1 (CODE WINS).** The AAP prose said `ErrObjectLocked` is "returned as
> HTTP 403." The code and the runtime evidence show the status is **HTTP 400 /
> `InvalidRequest`** with message "Object is WORM protected and cannot be overwritten."
> See `cmd/api-errors.go:L1059-L1063` below.

### Code-Truth Root Cause

- **Single-object path.** `DeleteObjectHandler` (declared `cmd/object-handlers.go:L2509`)
  performs retention enforcement at the call site **`cmd/object-handlers.go:L2601`**
  (`enforceRetentionBypassForDelete`); the typed `ObjectLocked{}` condition is returned from
  inside that helper in `cmd/bucket-object-lock.go` (at **`:L101`, `:L117`, `:L121`, `:L143`,
  `:L147`**), not from the object handler itself.
- **Multi-object path.** `DeleteMultipleObjectsHandler` (declared `cmd/bucket-handlers.go:L416`)
  enforces at the call site **`:L573`**; the per-object errors are returned **inside a
  200 batch response**, which is why the POST itself is `200 OK`.
- **Enforcement logic.** `enforceRetentionForDeletion` at **`cmd/bucket-object-lock.go:L54`**,
  `enforceRetentionBypassForDelete` at **`:L84`** (governance-bypass permission check at
  **`:L153`**), and `checkPutObjectLockAllowed` at **`:L245`**.
- **Error surface.** The `ObjectLocked` condition maps to `ErrObjectLocked` at
  **`cmd/api-errors.go:L2298-L2299`**; the enum value is declared at **`:L206`**; and the
  definition at **`cmd/api-errors.go:L1059-L1063`** is:
  ```go
  ErrObjectLocked: {
      Code:           "InvalidRequest",
      Description:    "Object is WORM protected and cannot be overwritten",
      HTTPStatusCode: http.StatusBadRequest,   // == HTTP 400
  },
  ```
- **Lifecycle/quota deletes** also route through `enforceRetentionForDeletion` from the
  scanner at `cmd/data-scanner.go:L1078` and `:L1246`. The retention / legal-hold metadata
  model lives in `internal/bucket/object/lock/lock.go`.

| Path | Handler | Enforcement call site | Result shape |
|------|---------|----------------------|--------------|
| Single | `cmd/object-handlers.go:L2509` | `:L2601` (cond returned in `cmd/bucket-object-lock.go:L101-L147`) | **HTTP 400** top-level `InvalidRequest` |
| Multi | `cmd/bucket-handlers.go:L416` | `:L573` | **HTTP 200** batch, per-object `<Error>InvalidRequest` |

### Rationale / Thinking

WORM immutability is enforced at the delete handler **regardless** of the principal's delete
permission. A COMPLIANCE-mode retention cannot be bypassed by anyone; a GOVERNANCE-mode
retention can be bypassed only with the special `s3:BypassGovernanceRetention` permission,
checked at `cmd/bucket-object-lock.go:L153`. The single-delete path returns a **top-level
HTTP 400 `InvalidRequest`**, while the batch path wraps the *identical*
`InvalidRequest` / "Object is WORM protected and cannot be overwritten" message **per object**
inside an HTTP 200 multi-delete response (this is S3-batch semantics — the batch request
succeeds, individual items report their own errors). The error string and status both trace
to the single definition at `cmd/api-errors.go:L1059-L1063`, which is **HTTP 400, not 403** —
the one place the AAP's prose was wrong.

Documentation corroboration (in-repo): `docs/bucket/retention/README.md` notes that legal
hold "disallows all deletes of an object under legal hold," and that retained WORM versions
are not deletable until expiry.

---

## 3. Bitrot detection on the read path

### The Question (verbatim)

> "I also want you to analyze how the system handles unauthorized manual data corruption within the storage backend. Trigger a bit rot detection event and identify the specific runtime logs generated during a subsequent get request."

### Reproduction

A 5 MiB object was written to the 4-drive erasure set, where it is stored as a 2+2 erasure
layout (`part.1` of ~2.5 MiB per drive, under an object-UUID directory). "Unauthorized manual
corruption" was then simulated by zeroing a 128 KiB region in the middle of the shard on
drive 1, followed by a GET and a deep heal scan:

```bash
# erasure server on /tmp/mdata/d{1...4}; object stored as d1..d4/<bucket>/<obj>/<uuid>/part.1
# zero a 128 KiB mid-shard region on drive 1 (out-of-band, behind the server's back):
dd if=/dev/zero of="<d1>/.../part.1" bs=1 seek=1000000 count=131072 conv=notrunc

# subsequent GET:
mc cp local/<bucket>/<obj> /tmp/out.bin     # (or boto3 get_object)

# observe healing:
mc admin trace --call storage,scanner -v local     # (or: mc admin scanner trace local)
mc admin heal -r --scan deep --json local/<bucket>
```

### Captured Evidence

The GET after corrupting `d1/part.1` returned **byte-identical** data (reconstructed from the
remaining data + parity shards on the read path), and a deep heal scan reported the corrupted
drive as repaired:

```text
# GET after zeroing d1/part.1 returned correct bytes:
sha256(downloaded) = 96e412cf6ff9623447164b9e656a378d6ea753e523f5a72ae986d75e845d6adf
sha256(original)   = 96e412cf6ff9623447164b9e656a378d6ea753e523f5a72ae986d75e845d6adf   # MATCH

# deep heal scan result:
OBJECT before: drives=[d1=missing, d2=ok, d3=ok, d4=ok]
OBJECT after : drives=[d1=ok,      d2=ok, d3=ok, d4=ok]   # d1/part.1 corrupt region restored
```

> **Honesty note (no fabricated log line).** With the *default* trace/log verbosity, a
> *successful* read-path reconstruction-and-heal is **quiet**: the explicit string
> `"unable to heal %d corrupted blocks on drives"` (`cmd/erasure-healing.go:L238`) is emitted
> only on heal **failure**. The proof of detection-and-repair is therefore twofold and
> behavioral, not a single log string: **(a)** the GET returns correct bytes despite a zeroed
> shard, and **(b)** the deep-heal scan reports the corrupted drive as needing/receiving
> repair. This is stated transparently rather than inventing a log line that the code does not
> emit on the success path.

### Code-Truth Root Cause

- **Checksum verification on read.** `bitrotVerify` at **`cmd/bitrot.go:L158`** recomputes and
  compares the shard checksum and returns `errFileCorrupt` on mismatch (the `errFileCorrupt`
  returns are at `:L163`, `:L166`, `:L178`, `:L201`, `:L206`).
- **Read-path detection + heal trigger.** In the GET/erasure read path, the `errFileCorrupt`
  check at **`cmd/erasure-object.go:L398`** leads to heal options being set with
  **`BitrotScan: errors.Is(err, errFileCorrupt)`** at **`:L407`** — i.e. a deep, bitrot-aware
  heal is requested precisely when corruption is detected.
- **Heal logging / audit (failure path).** In `cmd/erasure-healing.go`, the
  `errPartMissingOrCorrupt` variable is at `:L152`, the string
  `"unable to heal %d corrupted blocks on drives"` is at **`:L238`**, and
  `"...missing blocks on drives"` is at `:L243`. These are emitted only when a heal cannot be
  completed.
- **Backend corruption logging.** The string `"Data appears corrupt. Drop data."` appears at
  **`cmd/xl-storage.go:L2678` and `:L2687`**.
- **Variants & harness.** Streaming/whole-file checksum variants live in
  `cmd/bitrot-streaming.go` and `cmd/bitrot-whole.go`; the existing unit harness is
  `cmd/bitrot_test.go`.

| Stage | Source anchor | Behavior |
|-------|---------------|----------|
| Verify | `cmd/bitrot.go:L158` | HighwayHash recompute → `errFileCorrupt` on mismatch |
| Detect on read | `cmd/erasure-object.go:L398` | Recognizes `errFileCorrupt` during GET |
| Schedule heal | `cmd/erasure-object.go:L407` | `BitrotScan: errors.Is(err, errFileCorrupt)` |
| Failure log only | `cmd/erasure-healing.go:L238,L243` | Strings emitted **only** if heal fails |
| Backend log | `cmd/xl-storage.go:L2678,L2687` | "Data appears corrupt. Drop data." |

### Rationale / Thinking

MinIO protects each shard with a HighwayHash bitrot checksum. On read, `bitrotVerify`
(`cmd/bitrot.go:L158`) recomputes and compares; a mismatch yields `errFileCorrupt`. The
erasure read path **tolerates** the bad shard by reconstructing the object from the remaining
data + parity shards, so the GET returns correct bytes, and **simultaneously** schedules a
bitrot-aware heal (`BitrotScan` true at `cmd/erasure-object.go:L407`) to rewrite the corrupted
shard. Thus "unauthorized manual data corruption" is both transparently **survived** and
self-**repaired**. The absence of an error log on the success path is itself consistent with
the code: the explicit corruption-heal failure strings (`cmd/erasure-healing.go:L238,L243`)
are reserved for the case where reconstruction cannot complete.

Documentation corroboration (in-repo): `docs/erasure/README.md` states MinIO's backend "uses
high speed HighwayHash checksums to protect against Bit Rot." Tooling corroboration: MinIO's
`mc` exposes a dedicated `scanner` trace call type — `mc admin scanner trace` (equivalently
`mc admin trace --call scanner`) — which the `mc` CLI maps to "Trace Scanner calls."

---

## 4. STS session-policy enforcement (intersection)

### The Question (verbatim)

> "I want you to verify that when a user gets temporary credentials, minio is able to enforce the session policy on that user. You need to give me runtime test output to prove this behavior."

### Reproduction

The requirement is specifically about an **inline session policy** attached to *temporary*
credentials, so the proof must AssumeRole **with** an inline `Policy` and show that an action
the **parent** policy *allows* is *denied* under the temporary credentials. Critically, **no
in-tree STS test supplies an inline session policy** — every `cr.STSAssumeRoleOptions` literal
in `cmd/sts-handlers_test.go` (at `:L130`, `:L253`, `:L357`, `:L447`, `:L546`, `:L593`) sets
only `AccessKey`, `SecretKey`, and `Location`, never `Policy`. The behavior was therefore
proven with a **dedicated, ephemeral reproduction** run under `/tmp` (cleaned up afterward;
**no source file was added or modified**).

> **🚩 Discrepancy #4 (CODE WINS).** The AAP cited `TestSTS` / `TestSTSWithGroupPolicy`
> `"Access Denied."` assertions (`cmd/sts-handlers_test.go:L473`, `:L573`) as proof of
> session-policy *intersection*. Code-as-truth shows those prove only **inherited
> parent-policy** behavior, not session-policy enforcement: `TestSTS` (a method on
> `*TestSuiteIAM`, `:L393`) assumes a role with **no** inline `Policy`, and its parent policy
> grants only `s3:PutObject`/`s3:GetObject`/`s3:ListBucket` (`:L403-L420`) — so the
> `RemoveObject`→`"Access Denied."` at `:L473` is simply an *ungranted* action being denied;
> `:L573` is the analogous assertion in `TestSTSWithGroupPolicy` (`:L478`), also with no inline
> session policy. (Both are suite methods, so the AAP's `go test … -run TestSTS ./cmd` matches
> nothing and prints `ok … [no tests to run]`; the suite runner is
> `TestIAMInternalIDPSTSServerSuite`, `:L52` → `runAllIAMSTSTests` → `suite.TestSTS(c)`, `:L44`.)
> To actually prove the requirement, the inline-session-policy reproduction below was used.

**Reproduction (ephemeral; `/tmp` only):**

1. Build and launch a throwaway single-node, 4-drive erasure server:

   ```bash
   make build                                        # CGO_ENABLED=0 go build -tags kqueue
   ./minio server /tmp/blitzy_minio_data/{1,2,3,4} \
       --address 127.0.0.1:9100                      # root creds: minioadmin/minioadmin
   ```

2. Create a **broad parent canned policy** (`s3:*` on the bucket), a user, and attach it; then
   AssumeRole with an **inline session policy** that allows only `s3:GetObject` +
   `s3:ListBucket` (deliberately excluding `s3:PutObject`). Using
   `github.com/minio/minio-go/v7/pkg/credentials`:

   ```go
   // parent canned policy "broadpolicy": Allow s3:*  on arn:aws:s3:::stsbucket[/*]
   // inline SESSION policy:               Allow ONLY s3:GetObject + s3:ListBucket
   ar := cr.STSAssumeRole{
       Client:      &http.Client{Transport: http.DefaultTransport},
       STSEndpoint: "http://127.0.0.1:9100",
       Options: cr.STSAssumeRoleOptions{
           AccessKey: "writer12345", SecretKey: "writer12345-secret",
           Policy: inlineSessionPolicy,   // <<< the inline session policy (no s3:PutObject)
       },
   }
   val, _ := ar.Retrieve()                // temporary creds carrying the session policy
   ```

   The reproduction then asserts: the **parent** user *can* PutObject (broad write is real);
   the **STS** creds *can* List/Get (inside the intersection); the **STS** creds *cannot*
   PutObject (excluded by the session policy even though the parent allows `s3:*`). It fails
   loudly if PutObject is **not** denied.

### Captured Evidence

Four independent, mutually corroborating layers were captured from the live server.

**(1) Go test output — genuine `--- PASS`:**

```text
=== RUN   TestInlineSTSSessionPolicyIntersection
    sts_session_policy_test.go:139: setup: parent canned policy 'broadpolicy' (s3:*) attached to user "writer12345"
    sts_session_policy_test.go:154: PARENT creds: PutObject SUCCEEDED -> broad write (s3:*) is genuinely granted by the parent policy
    sts_session_policy_test.go:174: AssumeRole returned temporary creds (AK=EQN4UZ..., session-token present) with inline session policy = {GetObject, ListBucket} only
    sts_session_policy_test.go:196: STS creds: ListObjects SUCCEEDED (allowed by both parent and session policy)
    sts_session_policy_test.go:207: STS creds: GetObject SUCCEEDED (allowed by both parent and session policy)
    sts_session_policy_test.go:219: STS creds: PutObject DENIED -> "Access Denied."
    sts_session_policy_test.go:220: PROOF: parent grants s3:* (incl. PutObject) yet temporary creds are DENIED PutObject because the inline session policy excludes it => effective permission = INTERSECTION(parent, session).
--- PASS: TestInlineSTSSessionPolicyIntersection (0.27s)
PASS
ok  	blitzyrepro	0.289s
```

**(2) `mc admin trace -v` — the `AssumeRole` request carries the inline `Policy` form parameter
and succeeds (`200 OK`):**

```text
127.0.0.1:9100 [REQUEST sts.AssumeRole] [Client IP: 127.0.0.1]
127.0.0.1:9100 POST /
127.0.0.1:9100 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=writer12345/20260626//sts/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=…
127.0.0.1:9100 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%0A+%22Version%22%3A+%222012-10-17%22%2C…%22s3%3AGetObject%22%2C+%22s3%3AListBucket%22…%7D&Version=2011-06-15
127.0.0.1:9100 [RESPONSE] [ Duration 2.434ms … ↑ 495 B  ↓ 1.1 KiB ]
127.0.0.1:9100 200 OK
```

URL-decoding the `Policy=` form parameter yields exactly the inline session policy that was
sent (read/list only — **no** `s3:PutObject`):

```json
{ "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::stsbucket", "arn:aws:s3:::stsbucket/*"] } ] }
```

**(3) `mc admin trace -v` — the STS-credentialed `PutObject` is rejected `403 / AccessDenied`:**

```text
127.0.0.1:9100 [REQUEST s3.PutObject] [Client IP: 127.0.0.1]
127.0.0.1:9100 PUT /stsbucket/sts-write.txt
127.0.0.1:9100 X-Amz-Security-Token: eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9…   (STS session token)
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=EQN4UZW2Y0I8STARBLVO/20260626/us-east-1/s3/aws4_request,…
127.0.0.1:9100 [RESPONSE] [ Duration 220µs … ↑ 140 B  ↓ 335 B ]
127.0.0.1:9100 403 Forbidden
127.0.0.1:9100 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>sts-write.txt</Key><BucketName>stsbucket</BucketName>…</Error>
```

**(4) The temporary credential itself embeds the session policy.** Base64-decoding the JWT
payload of the `X-Amz-Security-Token` shown above reveals a `sessionPolicy` claim bound to the
parent `writer12345`:

```json
{ "parent": "writer12345",
  "accessKey": "EQN4UZW2Y0I8STARBLVO",
  "sessionPolicy": "<base64>" }     // decodes to: Allow { s3:GetObject, s3:ListBucket }
```

This is the server-side embedding that `IsAllowedSTS` later extracts (via
`args.Claims[sessionPolicyNameExtracted]`) and ANDs with the parent policy. Across all four
layers the outcome is identical and reproducible: an action (`PutObject`) the **parent** policy
permits (`s3:*`) is **denied** under the temporary credentials because the **inline session
policy** excludes it — the effective permission set is the **intersection**.

### Code-Truth Root Cause

- **STS evaluation entrypoint.** `IsAllowedSTS` at **`cmd/iam.go:L2242`**; the parent's
  session-policy-name constant `sessionPolicyNameExtracted` at `:L2136`.
- **Intersection logic** at **`cmd/iam.go:L2310-L2312`**:
  ```go
  hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
  if hasSessionPolicy {
      return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))
  }
  ```
  The request must be allowed by **both** the inline session policy (`isAllowedSP`) **and**
  the parent/canned policy (`combinedPolicy.IsAllowed(args)`) — the **intersection**.
- **Where the session policy is read.** `isAllowedBySessionPolicy` at **`cmd/iam.go:L2381`**
  pulls the embedded policy from the STS token via `args.Claims[sessionPolicyNameExtracted]`
  (**`:L2386`**) and evaluates it. This is exactly the `sessionPolicy` claim observed inside the
  captured `X-Amz-Security-Token` (Evidence #4), confirming the wire ⇄ code linkage.
- **No-session inherited path** returns `isOwnerDerived || combinedPolicy.IsAllowed(args)` at
  **`cmd/iam.go:L2317`** (when no session policy is attached, the parent policy alone governs).
- **Session-policy size limit.** `maxSTSSessionPolicySize = 2048` is defined at
  **`cmd/sts-handlers.go:L89`** (enforced at `:L122-L123`).

> **🚩 Discrepancy #3 (CODE WINS).** The AAP placed `maxSTSSessionPolicySize` in
> `cmd/globals.go`. It is actually defined in **`cmd/sts-handlers.go:L89`**; the value `2048`
> is correct, the file was wrong.

- The six STS flows (AssumeRole / WebIdentity / ClientGrants / LDAP / Certificate /
  CustomToken) live in `cmd/sts-handlers.go`. A related negative test,
  `TestSTSWithDenyDeleteVersion`, is at `cmd/sts-handlers_test.go:L180`.

### Rationale / Thinking

Temporary credentials **cannot exceed** the parent's permissions because the evaluator ANDs
the session-policy decision with the parent/canned-policy decision
(`cmd/iam.go:L2310-L2312`). The captured reproduction demonstrates this end-to-end against a
live in-process server: the parent user *can* PutObject (broad `s3:*` write is genuinely
granted), yet the temporary credentials — whose inline session policy lists only
`s3:GetObject` + `s3:ListBucket` — are **denied** PutObject (`403 / AccessDenied`) while
List and Get succeed. The denied action is precisely the one *allowed by the parent but
excluded by the inline session policy*, so the effective permission set is the
**intersection**, never the union. (By contrast, the in-tree `TestSTS` /
`TestSTSWithGroupPolicy` `"Access Denied."` assertions at `cmd/sts-handlers_test.go:L473`,
`:L573` use **no** inline session policy and therefore prove only inherited parent-policy
behavior — which is why the dedicated reproduction above was required.)

Documentation corroboration: MinIO's `docs/sts/assume-role.md` states the session's
permissions are "the intersection of the canned policy name and the policy set here," that
you cannot grant more than the canned policy allows, and gives the inline policy a "Maximum
length of 2048" — matching the code's `maxSTSSessionPolicySize`. The AWS STS reference agrees:
inline/managed session policy plaintext "can't exceed 2,048 characters," and the resulting
permissions are "the intersection of the role's identity-based policy and the session
policies."

---

## 5. Privilege-escalation prevention + root cause

### The Question (verbatim)

> "Show me test output to prove that a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user mappings modification behavior that you observe."

### Reproduction

The proof is the in-process test `TestUserPolicyEscalationBug`, which has a limited user
attempt to attach `consoleAdmin` to itself through the add-user API and verifies the user
gains **no** new privileges.

> **🚩 Discrepancy #4 (CODE WINS).** `TestUserPolicyEscalationBug` is a **method on the
> `*TestSuiteIAM` suite** (`cmd/admin-handlers-users_test.go:L313`), not a top-level test
> function; `go test … -run TestUserPolicyEscalationBug ./cmd` prints `ok … [no tests to run]`.
> The correct top-level runner is `TestIAMInternalIDPServerSuite`
> (`cmd/admin-handlers-users_test.go:L192`), which invokes
> `suite.TestUserPolicyEscalationBug(c)` at `:L205`.

Correct command:

```bash
go test -tags kqueue,dev -v -run TestIAMInternalIDPServerSuite ./cmd
```

### Captured Evidence

```text
--- PASS: TestIAMInternalIDPServerSuite (14.22s)
    --- PASS: .../Test:_1,_ServerType:_ErasureSD (~3.6s)
    --- SKIP: .../Test:_2,_ServerType:_ErasureSD_(with_etcd_backend) (0.00s)
    --- PASS: .../Test:_3,_ServerType:_ErasureSD (~3.5s)
    --- SKIP: .../Test:_4 (etcd)
    --- PASS: .../Test:_5,_ServerType:_Erasure (~3.5s)
    --- SKIP: .../Test:_6 (etcd)
    --- PASS: .../Test:_7,_ServerType:_ErasureSet (~3.5s)
    --- SKIP: .../Test:_8 (etcd)
PASS
ok  	github.com/minio/minio/cmd	14.506s
```

The test logic (`cmd/admin-handlers-users_test.go:L313-L423`) is:

1. Create a limited user whose policy allows only `s3:ListBucket` / `s3:PutObject` /
   `s3:GetObject`.
2. Confirm that the limited user's `RemoveBucket` is denied → `"Access Denied."`.
3. As the limited user, send a raw `PUT /minio/admin/v3/add-user` carrying
   `UserInfo{PolicyName:"consoleAdmin"}` (`:L390-L394`, with `PolicyName` at `:L392`); the
   request returns **HTTP 200**.
4. Re-check `RemoveBucket` — it is **still** `"Access Denied."`. The fatal guard at
   **`:L422`** (`c.Fatalf("User was able to escalate privileges (Err=%v)!", err)`, guarded by
   `if err == nil || err.Error() != "Access Denied."`) would fail the test if the user had
   gained admin rights.

PASS ⇒ the escalation was prevented.

### Code-Truth Root Cause

There are **two independent reasons** self-promotion fails.

**(1) The add-user store path never reads `PolicyName` — the root cause.**

- The add-user handler `AddUser` at **`cmd/admin-handlers-users.go:L444`** allows a
  *self-targeting* call: it reduces to a `checkDenyOnly` check (`~:L495`/`:L499`), a DenyOnly
  `IsAllowed` (`~:L509`), and then `CreateUser` (`~:L542`). A basic user **can** create/update
  *its own* credentials — which is why the request legitimately returns HTTP 200.
- **ROOT CAUSE:** the store operation `AddUser` at **`cmd/iam-store.go:L2659`** builds the
  identity via
  `newUserIdentity(auth.Credentials{AccessKey: accessKey, SecretKey: ureq.SecretKey, Status: ...})`
  at **`cmd/iam-store.go:L2672`** and **never reads `ureq.PolicyName`**. Verified by scanning
  the entire function body (`L2659-L2692`): the token `PolicyName` does not appear anywhere.
  The submitted `PolicyName:"consoleAdmin"` is therefore **silently ignored** at the store
  layer. (`newUserIdentity` helper is at `cmd/iam-store.go:L163`; `CreateUser` is at
  `cmd/iam.go:L1340`.)

**(2) Policy *attachment* is decoupled behind admin-only actions a basic user lacks.**

> **🚩 Discrepancy #2 (CODE WINS).** The AAP cited `SetPolicyForUserOrGroup` at
> `cmd/iam.go:L1770`, but `cmd/iam.go:L1770` is **LDAP DN-normalization** code. The corrected
> attribution is:
>
> | Function | Location | Admin gate |
> |----------|----------|------------|
> | `PolicyDBSet` (IAM-store policy mutator) | `cmd/iam.go:L1928` | — |
> | `SetPolicyForUserOrGroup` (deprecated admin handler) | `cmd/admin-handlers-users.go:L1770` | `policy.AttachPolicyAdminAction` (`:L1773`) |
> | `AddCannedPolicy` | `cmd/admin-handlers-users.go:L1701` | `CreatePolicyAdminAction` (`:L1704`) |
> | `AttachDetachPolicyBuiltin` | `cmd/admin-handlers-users.go:L1908` | `AttachPolicyAdminAction` (`:L1912`) |
>
> The Req 5 root cause (PolicyName ignored at `cmd/iam-store.go:L2672`) is **unaffected** by
> this correction; only the admin-gating citation is fixed.

Because changing a user's policy mapping is only possible through these dedicated, admin-gated
APIs — and the add-user path simply does not honor `PolicyName` — a basic user has no route to
attach `consoleAdmin` to itself.

### Rationale / Thinking

Self-promotion fails for two complementary reasons proven above: **(1)** the add-user store
path does not consult `PolicyName`, so embedding `"consoleAdmin"` in the request is a no-op at
the persistence layer (`cmd/iam-store.go:L2672`); and **(2)** even the dedicated policy-attach
APIs are gated behind admin actions (`AttachPolicyAdminAction`, `CreatePolicyAdminAction`)
that a basic user is not authorized to perform. The passing test demonstrates the user is no
more privileged after the call than before — `RemoveBucket` remains `"Access Denied."` both
times — which is exactly what the fatal guard at `cmd/admin-handlers-users_test.go:L422`
enforces.

Documentation context: `docs/iam/` and `docs/multi-user/` describe MinIO's admin-gated
policy-management model, under which attaching or creating policies is an administrative
action distinct from creating/updating a user's own credentials.

---

## 6. Cleanup & Source-Tree Integrity

Reproduction relied on two distinct classes of resource — **temporary artifacts** that were
created and then deleted, and **external system-level tools** that live outside the repository
and were merely *used* (never created or deleted):

- *Temporary artifacts (created, then deleted after capture):* the compiled `./minio` binary
  and the `make build` debugging-tool binaries (built at the repo root; git-ignored); the
  throwaway server data directories under `/tmp` (e.g. `/tmp/mdata/d{1..4}`); the ephemeral STS
  reproduction module under `/tmp`; and all scratch reproduction scripts and captured
  trace/log/test output files under `/tmp`.
- *External system tools (used, not created or deleted):* the **Go 1.23.12** toolchain at
  `/usr/local/go`, the **`mc`** client at `/usr/local/bin/mc`, and `boto3` — all installed
  system-wide, outside the MinIO repository.

The temporary artifacts were all deleted after the evidence was captured; the external system
tools were left untouched in place. No file was added, modified, or deleted anywhere in the
MinIO source tree; the only artifact produced by this task is **this report**, which lives in
the *destination* repository's `blitzy/documentation/` directory.

A cleanliness caveat discovered during the investigation: invoking `go` with `-mod=mod` can
append lines to `go.sum`. The investigation therefore relied on the default `-mod=readonly`
behavior, which keeps `go.sum` pristine; any accidental modification is reverted with
`git checkout -- go.sum`. The module cache was pre-warmed and `go mod verify` reported all
modules verified, so no network or manifest mutation was required.

Final verification of the source tree:

```bash
$ git status --porcelain
$            # (empty — no source file added, modified, or deleted)

$ git rev-parse HEAD
c07e5b49d477b0774f23db3b290745aef8c01bd2
```

The empty `git status --porcelain` confirms the MinIO source tree is **byte-for-byte
unchanged** at HEAD `c07e5b49d`. The investigation was strictly observational: the source
repository was treated as a read-only reference corpus throughout, and the five behaviors were
**explained**, never **changed**.

---

### Appendix — Evidence ⇄ citation consistency

| # | Captured evidence | Authoritative `file:line` |
|---|-------------------|---------------------------|
| 1 | Server-injected `X-Amz-Server-Side-Encryption: aws:kms` on an unsigned request | `internal/bucket/encryption/bucket-sse-config.go:L140-L141`; toggle `internal/crypto/auto-encryption.go:L31,L37`; authz precedes at `cmd/object-handlers.go:L1836` → `cmd/iam.go:L2437` |
| 2 | `400 InvalidRequest` "Object is WORM protected and cannot be overwritten" | `cmd/api-errors.go:L1059-L1063` (def), `:L2298-L2299` (mapping), `:L206` (enum); enforcement `cmd/bucket-object-lock.go:L84` |
| 3 | Correct bytes after zeroing a shard + deep-heal repair | `cmd/bitrot.go:L158` (`errFileCorrupt`); `cmd/erasure-object.go:L407` (`BitrotScan`); failure-only logs `cmd/erasure-healing.go:L238,L243` |
| 4 | `--- PASS: TestInlineSTSSessionPolicyIntersection`; STS-credentialed `PutObject` → `403 AccessDenied` while parent allows `s3:*`; `Policy=` form param + `sessionPolicy` token claim captured in trace | intersection `cmd/iam.go:L2310-L2312`; session policy read at `:L2381,L2386`; limit `cmd/sts-handlers.go:L89` (ephemeral `/tmp` reproduction — **no source test modified**) |
| 5 | `--- PASS: TestIAMInternalIDPServerSuite`; `RemoveBucket` stays `"Access Denied."` | `PolicyName` ignored `cmd/iam-store.go:L2672`; admin gates `cmd/admin-handlers-users.go:L1704,L1773,L1912`; fatal guard `cmd/admin-handlers-users_test.go:L422` |

*Report complete. Source tree unchanged; this document is the sole deliverable.*

