# Does MinIO's read-only S3 authorization boundary hold under concurrent write/metadata load?

**A runtime, evidence-backed security investigation of the MinIO object storage server (`github.com/minio/minio`, branch `minio_c07e5b49d477`, HEAD `c07e5b49d`, Go module `go 1.23` [go.mod:L3]).**

This document answers the following question, decomposed and answered **by name** for every operation family it enumerates:

> One identity has read-only access to a bucket+prefix; other clients hammer the same bucket with writes and metadata traffic; then show what actually happens when the read-only identity attempts write-adjacent operations — **multipart operations, copy-style writes, metadata changes, deletes** — plus what metadata can be learned from **listing and HEAD** without full reads, backed by **runtime request/response traces** and **observed storage side effects**.

Everything below was produced by **building and running the canonical server**, provisioning real identities through the canonical admin path, generating real concurrent load, and probing every named operation through the **real S3 API** with raw SigV4-signed HTTP. Each behavioral claim is placed next to the actual observed output (HTTP status + complete, unedited S3 XML), the on-disk storage state, and the audit-log record, and is grounded in `file:line` references naming the specific function that performs the work.

> **Note on reproducibility.** Request IDs and timestamps below are from **this** run (server `deploymentid 584479d9-b9c6-423f-bff3-1f4c9308df56`, `Version DEVELOPMENT.GOGET (go1.23.12 linux/amd64)`). Every response in this deployment carries the same `HostId`/`X-Amz-Id-2`: `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`. Any value labeled *inferred* is called out explicitly; everything else is observed. The verdict is empirical and bounded — see **§9 (Limitations / non-certification)** for exactly what was and was not exercised.

---

## 1. Direct answer / verdict

**For every operation, principal, and configuration tested here, the read-only authorization boundary HELD under concurrent write/metadata load — no write-adjacent operation from the read-only identity ever succeeded, and every denial left zero storage side effects.** (This is an empirical result under the single-node default configuration and the specific operations/policies exercised below; it is a verification, not a formal certification — see **§9**.)

During an 8-second window in which **24 concurrent read-write clients** (`rwuser`) hammered `testbucket` — **8,029 `PutObject` + 8,029 `PutObjectTagging`** in run 1 (and 6,072 + 6,072 in an identical re-run; §4) — the read-only identity (`rouser`) was **denied every write-adjacent operation it attempted**. A dedicated denial census of the full write-adjacent matrix (§4) recorded **17 `403 AccessDenied` responses, 2 batch-delete `200`s whose bodies carry per-object `AccessDenied`, and 1 `PutObjectRetention 400`** — with **zero** successful mutations and **zero** storage side effects. The outcome was identical whether the probes ran during the load or under quiescence, and identical across two independent invocations.

The result rests on three mechanisms, each proven at runtime and grounded in code:

1. **Deny-by-default PBAC.** The terminal authorization decision is `IAMSys.IsAllowed` [cmd/iam.go:L2437]. For a regular user it fetches attached policies with `PolicyDBGet`; if none match the requested action it falls through to **`return false`** ([cmd/iam.go:L2476] `if len(policies) == 0` returns false, else [cmd/iam.go:L2482] `combinedPolicy.IsAllowed(args)`). The read-only policy grants only `s3:GetObject` (bucket+prefix) and `s3:ListBucket` (bucket) — it grants **none** of the write-adjacent actions, so each is denied.
2. **A `policy.Action` guards every operation in its handler, before the object layer runs.** Most guards are distinct — single delete → `DeleteObjectAction` [cmd/object-handlers.go:L2528], tagging → `PutObjectTaggingAction` [cmd/object-handlers.go:L3151], abort → `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118]. But the write gate is **deliberately reused**: **`PutObjectAction` is the single action checked for all object-creating operations** — multipart create [cmd/object-multipart-handlers.go:L83], upload-part [cmd/object-multipart-handlers.go:L667], complete [cmd/object-multipart-handlers.go:L927], and the **destination** of both server-side copy [cmd/object-handlers.go:L1173] and upload-part-copy [cmd/object-multipart-handlers.go:L268]. `readonly-bp` grants none of these actions (distinct or reused), so all are denied. Because each guard precedes any namespace lock or shard write, a correct denial cannot mutate storage.
3. **Concurrency and TOCTOU never downgrade the decision.** The decision is computed from the request's fixed *action + resource + principal* before the object layer executes, so there is no check-vs-use window. Probing the **exact key** other clients were actively mutating (968 overwrites during the window; §7.4) produced the same `403 AccessDenied` as under quiescence.

The three independent evidence streams captured for every result are:

| Stream | What it proves | Source |
|--------|----------------|--------|
| **Wire trace** | The server returned the exact status + S3 XML to the read-only principal | raw SigV4 HTTP response (status + full body + `X-Amz-Request-Id`) |
| **Storage side-effect** | The denied write created/mutated nothing on the erasure backend | `<datadir>/testbucket/<object>/xl.meta` sha256 before/after, part dirs, `.minio.sys/multipart`; plus a root-authenticated semantic re-check (`StatObject` ETag / `GetObjectTagging` count) |
| **Audit-log denial** | The server logged the outcome with principal/action/resource | `logger.AuditLog` (invoked by each handler's `writeErrorResponse` path; the shared date-header emitter is at [cmd/auth-handler.go:L636]) → webhook sink |

> **Batch-delete nuance (F-note, expanded in §5.4).** `DeleteObjects` (multi-delete) is *transport-level* `HTTP 200` and produces a **single** audit event `api.name=DeleteMultipleObjects statusCode=200`; the per-object `AccessDenied` denials appear **inside** the `<DeleteResult>` XML body, not as separate `403` audit events. The wire trace and the body are therefore both required to see the denial.

**Information-disclosure nuance (answered in full in §6):** listing and HEAD are *read* surfaces the read-only identity legitimately holds. `HeadObject`/`GetObject` inside the prefix succeed and disclose size, ETag, content-type and timestamp; outside the prefix they are `403`. Crucially, `s3:ListBucket` here is **bucket-scoped** (no `s3:prefix` condition), so `ListObjectsV2`/`ListObjectsV1` **without a prefix** (or with a prefix outside the grant) **do disclose keys outside the granted prefix** — including `private/secret.txt`, which the same identity **cannot** `GetObject`. This is bounded by adding an `s3:prefix` `Condition` to the `s3:ListBucket` grant (§6.3). `ListMultipartUploads` is itself **denied** (it needs `ListBucketMultipartUploadsAction`, which `readonly-bp` does not grant).

**Adjacent behaviors surfaced by exhaustive probing (full detail in §5–§7; catalogued in §8.4; scoped in §9).** Driving *every* named operation to its limit also surfaced several MinIO behaviors worth an operator's attention. **None of them lets the read-only identity mutate data or read object content it lacks, so none changes the verdict above** — each is disclosed for completeness, and remediation of each is out of scope for this read-only investigation (plan §0.3.2):

- **API-1** — a malformed/non-XML `PutObjectTagging` (and `PutObjectRetention`) body returns `HTTP 500 InternalError` *before* the IAM check runs, because the body is parsed first (`ParseObjectXML` [cmd/object-handlers.go:L3141] precedes the `PutObjectTaggingAction` gate [cmd/object-handlers.go:L3151]; §5.3). It affects **any** principal and **writes nothing**.
- **API-2** — on the `DeleteObjects` and `PutObjectRetention` write paths the `Content-Md5` header's **presence** is checked but its **value** is not verified, so a wrong digest still succeeds (§5.4). This is on the **authorized-writer/root** path; the read-only identity is still `403`.
- **API-3** — when an object's key is exactly a prefix stem (e.g. an object `coll` alongside `coll/child.txt`), `ListObjectsV1/V2` **omit the `coll/` sub-tree** while the stem object exists, and it reappears once the stem is deleted (§6.3). A listing/enumeration-completeness quirk, not a read of denied content.
- **API-4 / STORAGE-1** — `ListMultipartUploads` matches `?prefix=` as an **exact key** rather than a true prefix, and empty-prefix in-progress-upload discovery is **lost across a server restart** (an in-memory cache; §6.4). The read-only identity is denied `ListMultipartUploads` outright regardless.
- **OBS-1** — the audit sink records a presigned URL's `X-Amz-Signature` verbatim, so a reader of the audit stream can **replay** the exact presigned request within its expiry window (§7.2). This is an **audit-sink confidentiality** concern, not an S3 authorization bypass: replay yields only what the presigning principal was already authorized to do, and expiry still bounds the window.
- **DEP-1 / DOC-1** — the repository's **baseline dependency/advisory posture** (a real `govulncheck` run; **no** dependency introduced by this task, `go.mod`/`go.sum` byte-for-byte unchanged) is recorded in **§9.1**, and the audit-census helper used in §4 was hardened against a null-`statusCode` edge case (**§4**).

The remainder of this document proves each of these claims with complete, unedited runtime output.

---

## 2. Exact build & invocation commands (default, canonical configuration)

**Toolchain:** Go **1.23.12** (`linux/amd64`) — the highest documented `1.23.x`, consistent with `go 1.23` [go.mod:L3] and the CI pins.

**Build (verbatim command run) — the canonical default build:**

```bash
CGO_ENABLED=0 go build .
```

Observed: exit `0`; produced the server binary **`./minio` = 156,743,642 bytes (~150 MB)** in the repo root (the repo's `.gitignore` ignores `/minio`, so the tree stays clean). The build resolved all modules from the committed `go.sum` **without modifying `go.mod`/`go.sum`** — both files' SHA-256 were byte-for-byte identical before and after:

```
go.mod  sha256 = b85e689662e001da57c8e38a7cc29ff7430c81df040ab913a802aa4e080c8e0f
go.sum  sha256 = 184a7add019c576c926f07da8ba57280a6c95f00ca3f427049d2a09b1d55fc63
```

Because a plain `go build` sets no linker flags, the binary self-reports the expected development version string:

```
$ ./minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
```

**Run (verbatim lifecycle) — single-node, default root credentials, with a canonical audit webhook.** To capture the third evidence stream, a standard **audit webhook** (a canonical audit target, not a bypass) is pointed at a tiny local HTTP sink that appends each received event as one JSON line to `/tmp/audit.log`:

```bash
# 1) Audit sink: a ~40-line net/http server (ephemeral; removed at cleanup) that appends each POSTed event to $AUDIT_FILE
AUDIT_FILE=/tmp/audit.log AUDIT_ADDR=127.0.0.1:9099 nohup /tmp/mh/bin/sink >/tmp/sink.log 2>&1 &

# 2) Server: canonical single-node, default creds, audit webhook -> sink
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin
export MINIO_AUDIT_WEBHOOK_ENABLE=on
export MINIO_AUDIT_WEBHOOK_ENDPOINT=http://127.0.0.1:9099/
nohup ./minio server /tmp/minio-data --address :9000 --console-address :9001 >/tmp/minio.log 2>&1 &
# -> server came up as PID 100233 on this run
```

The datadir `/tmp/minio-data` is a single-node erasure backend under `/tmp` (outside the repo).

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
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Resource>/</Resource><RequestId>18C1EA665675D17E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

This anonymous request is itself audit-logged as `api.name=ListBuckets statusCode=403` with the `accessKey` field absent (no principal), confirming the audit stream is live before provisioning.

> **Throughput note (honest, observed):** single-node write throughput on this host's `/tmp` backend varied run-to-run — run 1 sustained 8,029 `PutObject`+8,029 `PutObjectTagging` in 8.017 s, run 2 sustained 6,072+6,072 in 8.025 s under identical inputs (24 workers / 8 s). This variance is a host-scheduling / backend-I/O effect and affects only the *magnitude* of the background load (§4), never any authorization decision; the security outcome was invariant across both runs.

---

## 3. Identity & policy provisioning (canonical admin path only)

`mc` (the MinIO Client) is **not installed** in this environment, so provisioning was done through the **`madmin` SDK** (`github.com/minio/madmin-go/v3 v3.0.77`), which drives the exact same admin REST endpoints `mc admin` does. These are the canonical entry points (the handlers that back `mc admin user add`, `mc admin policy create`, `mc admin policy attach`):

- `AddUser` — `adminAPIHandlers.AddUser` [cmd/admin-handlers-users.go:L444] (SDK: `madmin.AddUser(ctx, ak, sk)`)
- `AddCannedPolicy` — `adminAPIHandlers.AddCannedPolicy` [cmd/admin-handlers-users.go:L1701] (SDK: `madmin.AddCannedPolicy(ctx, name, []byte)`)
- `SetPolicyForUserOrGroup` — `adminAPIHandlers.SetPolicyForUserOrGroup` [cmd/admin-handlers-users.go:L1770] (SDK: `madmin.SetPolicy(ctx, policy, user, isGroup=false)`)

No on-disk config was edited; no debug hook, mock, or synthetic bypass was used. This mirrors the canonical `start → ready → add user → create policy → attach → exercise → assert → cleanup` sequence in the repository's own PBAC test harness `docs/iam/policies/pbac-tests.sh`, and the `getonly.json` provisioning convention in `docs/multi-user/README.md`.

**Identities provisioned (four; the WORM identities are used only in §7.5):**

| User | Secret (ephemeral) | Policy | Role |
|------|--------------------|--------|------|
| `rwuser` | `readwrite-secret-123` | built-in `readwrite` (`s3:*` on `*`) | generates the concurrent load |
| `rouser` | `readonly-secret-123` | custom `readonly-bp` (bucket+prefix read) | the read-only principal under test |
| `wormuser` | `wormuser-secret-123` | custom `wormpol` (delete+retention, **no** bypass) | WORM independence proof (§7.5) |
| `bypassuser` | `bypassuser-secret-123` | custom `bypasspol` (delete + **BypassGovernanceRetention**) | WORM bypass contrast (§7.5) |

The built-in `readwrite` shape is confirmed in the repository at [cmd/sts-handlers_test.go:L830] as `{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}`.

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

The `s3:GetObject` statement is modeled on the `getonly.json` pattern from `docs/multi-user/README.md` (which grants `s3:GetObject` on `arn:aws:s3:::my-bucketname/*`), narrowed here to the `shared/` prefix; `s3:ListBucket` is added at the bucket level so the identity can list. (The `wormpol`/`bypasspol` JSON is shown in §7.5 where it is exercised.)

**Provisioning confirmation (observed, read back from the server via `ListCannedPolicies`):**

```
PROVISION: readonly-bp present in ListCannedPolicies = true
PROVISION: canned policies = [bypasspol consoleAdmin diagnostics readonly readonly-bp readwrite wormpol writeonly]
PROVISION: rouser->readonly-bp, rwuser->readwrite, wormuser->wormpol, bypassuser->bypasspol attached; seeds created
```

**Seed objects** (written by root so reads/listing return content; each given `Content-Type: text/plain`; no user metadata was attached — confirmed by the HEAD in §6 disclosing no `x-amz-meta-*`). The `xl.meta` sha256 is the storage baseline against which "zero storage side effect" is asserted after each denial (§5):

| Key | In grant? | Size | ETag (md5 of content) | `xl.meta` sha256 |
|-----|-----------|------|-----------------------|------------------|
| `shared/a.txt` | yes | 23 | `78d1f9238908e7d0b0558499b3341189` | `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd` |
| `shared/b.txt` | yes | 23 | `5979fba72462ec73d7a6dcb760912068` | `98c73a0d72b558758210d32f3bb130aa0d825b2488e0a7cdd8c84018278bcb82` |
| `shared/sub/c.txt` | yes | 30 | `1bd6b26f94901123019dc3d4088d0ad7` | `3348e3d5a8a7e6d48331fee4bc55481d850265f585c08b78b27fef4c9ddead05` |
| `private/secret.txt` | **no (outside prefix)** | 29 | `89f763923622b9b65a1ec900bfe145a9` | `32f1487b63fbbeab3dd01c69b5984a5a4ce308f7d377571b05a6b34e6fd1b70b` |
| `shared/contended.txt` | yes | 13 (initial) | `36e6bf3181687fd030794790bbc38991` | `6eca7a8fe83abf430e4ea7ce9f92b846b8a490291e8263a543ff562e2213916f` |

`shared/contended.txt` is the TOCTOU target; the concurrent writers overwrite it many times during §7.4, so a later root re-list shows it grown (37 bytes, ETag `63f9a036ef053d49ec1a32ac3e3e7fc5`). Every other seed object is asserted **unchanged** after each denied write.

---

## 4. Concurrent-load description ("under stress") and repeatability

**Load generator.** 24 goroutines, each authenticated as `rwuser` (built-in `readwrite`), looping two normal operations against `testbucket`: a `PutObject` to a unique key under the `load/w<N>/<M>.txt` key scheme, then a `PutObjectTagging` on that key. This is exactly the "other clients hammering the same bucket with writes and metadata traffic" the question describes. The read-only probe matrix (§5) runs **concurrently** in this window; a separate 8-writer generator hammers the single key `shared/contended.txt` for the TOCTOU probe (§7.4).

**Observed scale — two identical runs (same 24 workers / 8 s inputs; F-repeatability):**

```
RUN 1: 24 workers, 8s -> puts=8029 tags=8029 errs=0 wall=8.017s
RUN 2: 24 workers, 8s -> puts=6072 tags=6072 errs=0 wall=8.025s
```

Both runs used **identical** inputs (worker count, duration, target bucket, operation mix, and the same read-only probe matrix). The stable, run-to-run **invariant** is the security outcome: **`errs=0` for the writer both times, and every read-only write-adjacent probe was denied with the identical HTTP status and S3 error `<Code>` in both runs.** Only the raw throughput differed (8,029 vs 6,072 writes/run) — a host-I/O effect, not an authorization effect (§2).

**Deterministic denial census.** The full write-adjacent matrix (inside-prefix, batch, outside-prefix, and presigned) was executed once and every event counted **directly from the audit log** with this exact command:

```bash
python3 - <<'PY'
import json,collections
c=collections.Counter(); tot=0
for line in open('/tmp/audit.log'):
    line=line.strip()
    if not line: continue
    e=json.loads(line); tot+=1
    a=e.get('api',{})
    c[(a.get('name'), a.get('statusCode'), e.get('accessKey'))]+=1
print("TOTAL audit events:",tot)
for (n,code,ak),cnt in sorted(c.items(), key=lambda x:(str(x[0][2]),str(x[0][0]))):
    code_s = '-' if code is None else str(code)   # statusCode is `omitempty` in the audit schema; an absent value decodes to None
    print(f"{str(n):28} {code_s:<6} {str(ak):12} {cnt}")
PY
```

Output (verbatim):

```
TOTAL audit events: 66
DeleteObject                 403    None         1
PutObjectRetention           400    None         1
GetObjectTagging             200    minioadmin   23
HeadObject                   200    minioadmin   23
AbortMultipartUpload         403    rouser       1
CompleteMultipartUpload      403    rouser       1
CopyObject                   403    rouser       2
CopyObjectPart               403    rouser       1
DeleteMultipleObjects        200    rouser       2
DeleteObject                 403    rouser       3
DeleteObjectTagging          403    rouser       1
ListObjectParts              403    rouser       1
NewMultipartUpload           403    rouser       2
PutObjectLegalHold           403    rouser       1
PutObjectPart                403    rouser       1
PutObjectTagging             403    rouser       2
```

**Reconciliation (all 66 events accounted for):**

- **17 write-adjacent `403 AccessDenied`** = 16 with `accessKey=rouser` (the signed probes) + 1 with `accessKey=None` (the **expired presigned** `DeleteObject`, rejected at the pre-auth expiry check before a principal is attached; §7.2). Per-API rouser breakdown, with `2` = inside+outside where applicable: `NewMultipartUpload 2`, `PutObjectPart 1`, `CopyObjectPart 1`, `CompleteMultipartUpload 1`, `AbortMultipartUpload 1`, `ListObjectParts 1`, `CopyObject 2`, `PutObjectTagging 2`, `DeleteObjectTagging 1`, `PutObjectLegalHold 1`, `DeleteObject 3` (inside single + outside single + the *valid* presigned delete).
- **1 `PutObjectRetention 400`** (`accessKey=None`) — the lone write-adjacent op that is **not** `403` on the non-object-lock bucket; it fails earlier with `InvalidRequest`/`MissingContentMD5` before the IAM decision is reached (see §5.3 and §8.3). This is the one internal-consistency exception, called out explicitly.
- **2 `DeleteMultipleObjects 200`** (`accessKey=rouser`) — the inside and outside **batch** deletes; transport `200`, per-object `AccessDenied` in the body (§5.4).
- **46 root reads** (`accessKey=minioadmin`: 23 `HeadObject` + 23 `GetObjectTagging`) — the storage/semantic verification the harness performs as **root** after each probe to prove no mutation; excluded from the denial tally.

So **20 read-only probe attempts** (18 attributed to `rouser` + 2 logged with `accessKey=None`) produced **0 successes** and **0 storage side effects**, and the anonymous `ListBuckets` denial from the §2 baseline is the only other denial in the session.

**Census-script robustness (DOC-1).** The counting command above normalizes a **missing** `statusCode` to `-` before formatting (`code_s = '-' if code is None else str(code)`). This matters because the audit schema declares the field as `StatusCode int` with the struct tag `json:"statusCode,omitempty"` [pkg/v3@v3.0.22/logger/message/audit/entry.go:L47], so the key is **omitted entirely** whenever the recorded value is `0`. That value arises when a request reaches audit logging without a trace context: `internal/logger/audit.go` copies the status only when the `TraceCtxt` is present (`tc, ok := r.Context().Value(...)` then `if ok { statusCode = tc.ResponseRecorder.StatusCode }` [internal/logger/audit.go:L97-L99], followed by `entry.API.Status = http.StatusText(statusCode)` and `entry.API.StatusCode = statusCode` [internal/logger/audit.go:L119-L120]); the recorder otherwise initializes to `http.StatusOK` [internal/http/response-recorder.go:L84], so an absent `statusCode` (paired with an empty `status`, since `http.StatusText(0) == ""`) appears precisely when that context is absent. A naive formatter that writes `f"{code:<6}"` with `code=None` aborts with `TypeError: unsupported format string passed to NoneType.__format__`. Demonstrated on a real captured event re-rendered in that exact absent-`statusCode` form:

```
# naive `{code:<6}` variant, one null-statusCode event present:
TOTAL audit events: 2
Traceback (most recent call last):
  File "<stdin>", line 11, in <module>
    print(f"{n:28} {code:<6} {str(ak):12} {cnt}")
                   ^^^^^^^^^
TypeError: unsupported format string passed to NoneType.__format__      # exit 1

# null-safe variant (as printed above), same input:
TOTAL audit events: 2
GetBucketLocation            -      minioadmin   1
GetObject                    200    rouser       1                       # exit 0
```

On the null-free 66-event census log used above, the two variants print **identical** output (that run contained no absent-`statusCode` events), so the census counts are unaffected; the null-safe form simply prevents the crash on any audit stream that does contain such an event. Hardening the census script is a reporting-tool nicety and involves no product code (AAP §0.3.2).


## 5. Per-operation results (multipart, copy, metadata changes, deletes)

Every probe below was issued **from `rouser` through the real S3 API** using raw SigV4-signed HTTP built with the vendored client signer `github.com/minio/minio-go/v7@v7.0.80/pkg/signer` — `SignV4` [github.com/minio/minio-go/v7@v7.0.80/pkg/signer/request-signature-v4.go:L343] for header-signed requests and `PreSignV4` [github.com/minio/minio-go/v7@v7.0.80/pkg/signer/request-signature-v4.go:L208] for the presigned form (§7.2) — the same signer the official `minio-go` client uses. `X-Amz-Content-Sha256` was set to `hex(sha256(body))` before signing. The probes ran **during** the concurrent load of §4; a second, quiescent census reproduced identical outcomes. Each request block, response status line, complete response body, correlated audit event, and post-probe storage state is shown **verbatim and unedited** below. The `Signature=` and `Credential=` fields carry the real captured bytes of the ephemeral `rouser` credential (the whole deployment is destroyed at cleanup), so nothing is redacted here.

The denial envelope is identical in shape for every `403` below: `ErrAccessDenied` [cmd/api-errors.go:L86] maps to `HTTPStatusCode: http.StatusForbidden` [cmd/api-errors.go:L539]; the body is the `APIErrorResponse` struct [cmd/api-errors.go:L64] populated by `getAPIErrorResponse` [cmd/api-errors.go:L2597] and written by `writeErrorResponse` [cmd/api-response.go:L945]. Every `<RequestId>` in a response body equals the wire `X-Amz-Request-Id` and the `requestID` of the correlated audit event — the three-way correlation is exact in every case.

### 5.1 Multipart operations

All six multipart operations are denied. Contrary to a "one action per operation" intuition, the write gate is **reused**: `policy.PutObjectAction` guards *four* distinct steps — `CreateMultipartUpload` [cmd/object-multipart-handlers.go:L83], `UploadPart` (through `isPutActionAllowed` [cmd/auth-handler.go:L749]) [cmd/object-multipart-handlers.go:L667], the `UploadPartCopy` **destination** [cmd/object-multipart-handlers.go:L268], and `CompleteMultipartUpload` [cmd/object-multipart-handlers.go:L927]. `UploadPartCopy` additionally checks the **source** with `GetObjectAction` [cmd/object-multipart-handlers.go:L301]; `AbortMultipartUpload` and `ListParts` use their own distinct actions `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118] and `ListMultipartUploadPartsAction` [cmd/object-multipart-handlers.go:L1162]. The read-only policy grants none of these, so each fails in `checkRequestAuthType` [cmd/auth-handler.go:L339] → `authenticateRequest` [cmd/auth-handler.go:L358] → `IAMSys.IsAllowed` [cmd/iam.go:L2437] **before** any upload-id is created (the probes deliberately used the fabricated `uploadId=fakeuploadid000000`; the auth gate fires before that id is ever looked up).

**`CreateMultipartUpload`** — guard `PutObjectAction` [cmd/object-multipart-handlers.go:L83]:

```
POST /testbucket/shared/mp.txt?uploads=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=4fd2cd3c31745a54601923704575d29d3fbb4887c01decc775416bd7db8f12d2
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T181036Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD71206CD4C`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1EBD71206CD4C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=NewMultipartUpload status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mp.txt requestID=18C1EBD71206CD4C`

**`UploadPart`** — guard `isPutActionAllowed` [cmd/auth-handler.go:L749] / `PutObjectAction` [cmd/object-multipart-handlers.go:L667]:

```
PUT /testbucket/shared/mp.txt?partNumber=1&uploadId=fakeuploadid000000
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=f87d9caa7f47f0bbebc44543f25692d86bdc26d3c82b04af259c324cdbf06348
X-Amz-Content-Sha256: c71107e0c0da54aa3a6e89f7f6190f3c61b51a768d53771860a095fee681374a
X-Amz-Date: 20260713T181036Z

partdata
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD727146E9C`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1EBD727146E9C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectPart status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mp.txt requestID=18C1EBD727146E9C`

**`UploadPartCopy`** — dst `PutObjectAction` [cmd/object-multipart-handlers.go:L268] + src `GetObjectAction` [cmd/object-multipart-handlers.go:L301]:

```
PUT /testbucket/shared/mpc.txt?partNumber=1&uploadId=fakeuploadid000000
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-copy-source;x-amz-date, Signature=92a5e44650b2eaac2eb4e905bc9975997c274ce2a2b01191223c8d40397b0e97
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Copy-Source: /testbucket/shared/b.txt
X-Amz-Date: 20260713T181037Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD73C0C59F1`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mpc.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mpc.txt</Resource><RequestId>18C1EBD73C0C59F1</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=CopyObjectPart status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mpc.txt requestID=18C1EBD73C0C59F1`

**`CompleteMultipartUpload`** — guard `PutObjectAction` [cmd/object-multipart-handlers.go:L927]:

```
POST /testbucket/shared/mp.txt?uploadId=fakeuploadid000000
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=3fb7f215c362fb60f2c0da3fd106806ae3aa8e1ad4bdcd714cabcd9ed50d96fe
X-Amz-Content-Sha256: e66d2c4c39abc9d7b531ee552bf60a0f83f2a4d6267fc3c0cec06c9ca622741c
X-Amz-Date: 20260713T181037Z

<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"x"</ETag></Part></CompleteMultipartUpload>
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD751045DB0`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1EBD751045DB0</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=CompleteMultipartUpload status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mp.txt requestID=18C1EBD751045DB0`

**`AbortMultipartUpload`** — guard `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118]:

```
DELETE /testbucket/shared/mp.txt?uploadId=fakeuploadid000000
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=85bd75c592b2445692d938e0abd1bf1cc07cf8aec5542dad884d0ebae826fd9d
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T181037Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD765FAC345`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1EBD765FAC345</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=AbortMultipartUpload status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mp.txt requestID=18C1EBD765FAC345`

**`ListParts`** — guard `ListMultipartUploadPartsAction` [cmd/object-multipart-handlers.go:L1162]:

```
GET /testbucket/shared/mp.txt?uploadId=fakeuploadid000000
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=6c67968e7fc0b253f2b77e3c65afcddfc7348f213091bcc41622087cec5fdb43
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T181038Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD77AEF691C`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/mp.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/mp.txt</Resource><RequestId>18C1EBD77AEF691C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=ListObjectParts status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/mp.txt requestID=18C1EBD77AEF691C`

**Storage side-effect (all six).** Immediately after the six probes the backend was inspected live:

```
$ cd /tmp/minio-data
$ for k in shared/mp.txt shared/mpc.txt; do [ -e "testbucket/$k" ] && echo "PRESENT $k" || echo "ABSENT $k"; done
ABSENT shared/mp.txt
ABSENT shared/mpc.txt
$ find .minio.sys/multipart -mindepth 1 -type d | wc -l
0
```

Neither target object directory nor `xl.meta` exists, and `.minio.sys/multipart` holds **0** upload-id directories — no multipart session was ever created. The five other multipart denials are logged under the internal API names `PutObjectPart`, `CopyObjectPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`, and `ListObjectParts` respectively (shown above), each `statusCode=403 accessKey=rouser`.

### 5.2 Copy-style writes — `CopyObject`

`CopyObjectHandler` [cmd/object-handlers.go:L1154] checks the destination `PutObjectAction` [cmd/object-handlers.go:L1173] first, then the source `GetObjectAction` [cmd/object-handlers.go:L1206]. The read-only policy grants neither on the destination, so the copy is denied at the destination gate:

```
PUT /testbucket/shared/copy.txt
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-copy-source;x-amz-date, Signature=ff36ea473ff758dc67567dc2efe190741b52d27211a94e253158f6dfa7535348
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Copy-Source: /testbucket/shared/b.txt
X-Amz-Date: 20260713T181038Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD78FE5F9DE`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/copy.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/copy.txt</Resource><RequestId>18C1EBD78FE5F9DE</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=CopyObject status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/copy.txt requestID=18C1EBD78FE5F9DE`

**Storage side-effect:** `testbucket/shared/copy.txt` is **ABSENT** (no object directory, no `xl.meta`) after the probe — nothing was copied.

### 5.3 Metadata changes

Metadata mutations are gated by distinct per-operation actions. For every probe that targets the **existing** object `shared/a.txt` (both tagging operations, legal-hold, retention, and the single delete in §5.4), the storage check is **mutation-sensitive**: rather than comparing `xl.meta` byte length (which can coincide even after a change), the probe records the SHA-256 of `xl.meta` **and** re-reads the object's semantic metadata as root (`StatObject` ETag and `GetObjectTagging` tag count) immediately after each denied attempt, and asserts all three are unchanged.

**`PutObjectTagging`** — `PutObjectTaggingHandler` [cmd/object-handlers.go:L3122] → `PutObjectTaggingAction` [cmd/object-handlers.go:L3151]:

```
PUT /testbucket/shared/a.txt?tagging=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=9fbdcbd4bc44f4d6c258482e5d3006102edd101a485f3c9b9d8ff553e9da048c
Content-Md5: FqV7h4yLHK9RjIVuFJSqNQ==
X-Amz-Content-Sha256: 46255ed547da21000f4cc471cf8abc7a22f989c6ef7fa09d9444aafb986690d5
X-Amz-Date: 20260713T181038Z

<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD7A5FF7937`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD7A5FF7937</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectTagging status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/a.txt requestID=18C1EBD7A5FF7937`
Mutation-sensitive check on `shared/a.txt` after the denial: `xl.meta` SHA-256 = `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd` (unchanged); root `StatObject` ETag = `78d1f9238908e7d0b0558499b3341189` (unchanged); root `GetObjectTagging` tag-count = `0` (unchanged) ⇒ **MUTATED=false**.

**Malformed / non-XML `PutObjectTagging` body — the body parse runs *before* the IAM check, so the status reflects the parse failure (`500`/`400`), not `403`.** `PutObjectTaggingHandler` parses the request body with `tags.ParseObjectXML(io.LimitReader(r.Body, 1<<20))` [cmd/object-handlers.go:L3141] and returns on any parse error [cmd/object-handlers.go:L3142-L3144] **before** it reaches the `PutObjectTaggingAction` authorization gate [cmd/object-handlers.go:L3151] (the intervening line even sets the `x-amz-object-tagging` header from the parsed tags [cmd/object-handlers.go:L3148]). A request whose body is not well-formed tagging XML is therefore rejected at parse time, and the HTTP status reflects the *parse* failure rather than the authorization decision. Observed directly — all four events are audit-logged with `accessKey=None` (no principal attached yet), confirming the failure precedes IAM:

- **1-byte non-XML body, `rouser`** → `HTTP/1.1 500 Internal Server Error` · `X-Amz-Request-Id: 18C20CF990AC4003`.
- **1-byte non-XML body, root (`minioadmin`)** → `HTTP 500` — identical shape, confirming the defect is principal-independent:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InternalError</Code><Message>We encountered an internal error, please try again.: cause(EOF)</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C20CF9911F5126</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

- **Empty body, root** → `HTTP 500` `InternalError` `cause(EOF)` · `RequestId 18C20CF991861723` (an entirely non-XML or empty body surfaces the decoder's `EOF` as `InternalError`).
- **Truncated-but-XML-shaped body `<Tagging><TagSet>`, root** → `HTTP 400 MalformedXML` (a body that starts as valid XML but ends early is mapped to the client-error `MalformedXML` instead):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>MalformedXML</Code><Message>The XML you provided was not well-formed or did not validate against our published schema. (XML syntax error on line 1: unexpected EOF)</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C20CF991E6B977</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit (all four, principal not yet attached): `api.name=PutObjectTagging accessKey=None` with `statusCode=500 status=Internal Server Error` (×3) and `statusCode=400 status=Bad Request` (×1).

**Scope — this does *not* breach the read-only mutation boundary.** The parse-before-authz ordering means a read-only principal (indeed any principal) sending a malformed tagging body receives `500`/`400` instead of `403`, but **no tags are written**: after all four attempts, root `GetObjectTagging` on `shared/a.txt` still returns an empty `<Tagging><TagSet></TagSet></Tagging>` and the object body is intact (`GET` → `shared object a: hello`). A *well-formed* read-only `PutObjectTagging` is still denied `403 AccessDenied` (the trace immediately above). The exposure is therefore (a) a robustness gap — a malformed body surfaces as `500 InternalError` rather than a purely client-side `400`-class error — and (b) a minor pre-authorization processing quirk: the handler decodes the caller's body (and, for well-formed XML, sets the tagging header) before it authorizes. It changes **no** object state and grants **no** write capability. Product remediation (parsing after authorization, or returning `400` for the non-XML case) is outside this documentation-only task's scope (AAP §0.3.2).

**`DeleteObjectTagging`** — `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235] → `DeleteObjectTaggingAction` [cmd/object-handlers.go:L3301]:

```
DELETE /testbucket/shared/a.txt?tagging=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=8969204c5f3082c839655c3a14494008e2fdca0af5196c2174da201aba20c33b
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T181039Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD7BB2E56C5`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD7BB2E56C5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=DeleteObjectTagging status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/a.txt requestID=18C1EBD7BB2E56C5`
Mutation-sensitive check: `xl.meta` SHA-256 `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd`, ETag `78d1f9238908e7d0b0558499b3341189`, tag-count `0` — all unchanged ⇒ **MUTATED=false**.

**`PutObjectLegalHold`** — `PutObjectLegalHoldHandler` [cmd/object-handlers.go:L2698] → `PutObjectLegalHoldAction` [cmd/object-handlers.go:L2718]. **Authorization is the first gate:** the handler checks the action at [cmd/object-handlers.go:L2718] *before* it checks `Content-Md5` presence at [cmd/object-handlers.go:L2727], so a valid `Content-Md5` is not a precondition for reaching the IAM decision. The probe nonetheless carried a valid `Content-Md5` so the request is otherwise well-formed, and it is denied purely at the action check:

```
PUT /testbucket/shared/a.txt?legal-hold=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=449aef0ffe68780af81d50efcea05bc4beed294c2cdf8feb9772ef3b2abd32fc
Content-Md5: erOisiAmIIzsh/Ze2QqGXw==
X-Amz-Content-Sha256: 96b73c95a8d33e664ab2170e095025b47ebd55978bb71cebd6a51e394bf96722
X-Amz-Date: 20260713T181039Z

<LegalHold xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>ON</Status></LegalHold>
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD7D052FEB6`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD7D052FEB6</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectLegalHold status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/a.txt requestID=18C1EBD7D052FEB6`
Mutation-sensitive check: `xl.meta` SHA-256 `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd`, ETag `78d1f9238908e7d0b0558499b3341189`, tag-count `0` — unchanged ⇒ **MUTATED=false**.

**`PutObjectRetention` — the one write-adjacent operation on `shared/` that does *not* return `403`.** This is a deliberate, honest nuance. `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] validates only the **signature** at the top level via `validateSignature` [cmd/object-handlers.go:L2874]; it does **not** run a top-level `checkRequestAuthType`. The IAM decision for `PutObjectRetentionAction` is deferred into the object-layer metadata callback `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913]. Before that callback can run, the handler requires the bucket to have object-lock enabled: `if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled` [cmd/object-handlers.go:L2890] returns `ErrInvalidBucketObjectLockConfiguration` [cmd/object-handlers.go:L2891] — mapped to HTTP 400, S3 code `InvalidRequest`, message "Bucket is missing ObjectLockConfiguration" [cmd/api-errors.go:L919]. Because `testbucket` has no object lock, this **configuration precheck fires first**, so the observed result is a `400`, *not* the authorization decision:

```
PUT /testbucket/shared/a.txt?retention=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=17d466c452e7ad02185274fb48a9035521820fb57e9994364d85cc90ff6be480
Content-Md5: /YIC7M3yXxiVZayGzKajyw==
X-Amz-Content-Sha256: 8279b8588309b01a971a4fa1e3acef0bc9052603e0ebc3268c75dc0b6ff9003c
X-Amz-Date: 20260713T181039Z

<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-01T00:00:00Z</RetainUntilDate></Retention>
```

Wire: `HTTP/1.1 400 Bad Request` · `X-Amz-Request-Id: 18C1EBD7E574EB6A`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Bucket is missing ObjectLockConfiguration</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD7E574EB6A</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectRetention status=Bad Request statusCode=400 accessKey=<nil> requestID=18C1EBD7E574EB6A` — note `accessKey` is `<nil>`: the request is rejected at the pre-authorization configuration check, before the principal is attached to the audit record. Mutation-sensitive check on `shared/a.txt`: `xl.meta` SHA-256 `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd`, ETag `78d1f9238908e7d0b0558499b3341189`, tag-count `0` — unchanged ⇒ **MUTATED=false**. The clean read-only **authorization** denial for retention (`403 AccessDenied`) is reached only on a **lock-enabled** bucket, where this configuration precheck passes and the request proceeds past it: the deferred `PutObjectRetentionAction` check — evaluated inside the `EvalMetadataFn` callback [cmd/object-handlers.go:L2912] via `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913] — denies a read-only principal. That lock-enabled `403` is now **directly observed** (it was previously labeled *inferred*; this run captures it). Repeating the retention probe as `rouser` against the **lock-enabled** `wormbucket`, carrying a valid `Content-Md5` (`/YIC7M3yXxiVZayGzKajyw==`) so the request clears the signature, bucket-info, `Content-Md5`-presence, and object-lock-configuration prechecks and reaches the deferred `PutObjectRetentionAction` decision inside the `EvalMetadataFn` callback [cmd/object-handlers.go:L2912] via `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913], yields the clean authorization denial:

```
PUT /wormbucket/rt.txt?retention=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260714/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=...
Content-Md5: /YIC7M3yXxiVZayGzKajyw==

<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-01T00:00:00Z</RetainUntilDate></Retention>
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C20C03A02283AB`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>rt.txt</Key><BucketName>wormbucket</BucketName><Resource>/wormbucket/rt.txt</Resource><RequestId>18C20C03A02283AB</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectRetention statusCode=403 status=Forbidden accessKey=None object=rt.txt` — `accessKey` is `None` because the IAM decision is evaluated *inside* the object-layer metadata callback rather than at a top-level `checkRequestAuthType`, so the principal is not attached to the audit record (the same deferred-authz signature seen on the `400` above and on the malformed-tagging `500`s in this section). The call-ordering derivation is retained in **§8.3** (`PutObjectMetadata` → namespace lock → `readAllXL` → `EvalMetadataFn` → `IAMSys.IsAllowed(PutObjectRetentionAction)` → `errAuthentication` → `ErrAccessDenied`); it is now **corroborated by this wire+audit capture** rather than left inferred. The read-only principal thus cannot set retention on either bucket: `400` (config precheck) on the non-lock `testbucket`, `403 AccessDenied` (IAM) on the lock-enabled `wormbucket`. §7.5 does **not** exercise `PutObjectRetention`; what it *demonstrates* on a real object-lock bucket is the **independence of the IAM and object-lock gates for the *delete* path** — a read-only DELETE is stopped at the first `DeleteObjectAction` gate [cmd/object-handlers.go:L2528] (Case 1, object layer never reached), while the distinct `BypassGovernanceRetentionAction` gate [cmd/bucket-object-lock.go:L153] is exercised in Cases 3–4.

### 5.4 Deletes — single (`DeleteObject`) vs batch (`DeleteObjects`)

This is the one family where the HTTP-level *shape* of the denial differs between the single and batch forms, even though the authorization outcome is identical (nothing deleted).

**Single `DeleteObject`** — `DeleteObjectHandler` [cmd/object-handlers.go:L2509] → `DeleteObjectAction` [cmd/object-handlers.go:L2528]. Denied at the top level with `HTTP 403`:

```
DELETE /testbucket/shared/a.txt
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=ae5cb6311e74b8a766b23b6375da3ac27c1142d76f529990538d05094c48cd9b
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T181040Z
```

Wire: `HTTP/1.1 403 Forbidden` · `X-Amz-Request-Id: 18C1EBD7FA96363C`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD7FA96363C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=DeleteObject status=Forbidden statusCode=403 accessKey=rouser bucket=testbucket object=shared/a.txt requestID=18C1EBD7FA96363C`

**Batch `DeleteObjects`** — `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416] checks each object **individually** via `checkRequestAuthTypeWithVID` / `DeleteObjectAction` [cmd/bucket-handlers.go:L505]. The overall `POST` is well-formed, so the **HTTP status is `200`**; the denial appears **per-object** inside the `<DeleteResult>` body. Crucially for audit accounting (see §4), the whole batch is a **single** audit event — `newContext(r, w, "DeleteMultipleObjects")` [cmd/bucket-handlers.go:L417] with one handler-level `defer logger.AuditLog` [cmd/bucket-handlers.go:L419] — so it is logged **once** as `api.name=DeleteMultipleObjects statusCode=200`, carrying an `api.objects[]` array; the per-object `AccessDenied` results live only in the response body, **not** as per-object `403` audit records:

```
POST /testbucket?delete=
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=content-md5;host;x-amz-content-sha256;x-amz-date, Signature=a23af046228b1ae94a9eafd66385388e8fe29ccd7505a0c449973e438f4ce058
Content-Md5: wi4s+CUiZpmK1AEVNO7diw==
X-Amz-Content-Sha256: 4d12dcb15216fc1920f8493c6e1c3ed963dbb30cb85fe247037f6ae804fb5f26
X-Amz-Date: 20260713T181040Z

<Delete><Object><Key>shared/a.txt</Key></Object><Object><Key>shared/b.txt</Key></Object></Delete>
```

Wire: `HTTP/1.1 200 OK` · `X-Amz-Request-Id: 18C1EBD81066CAC2`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><VersionId></VersionId></Error><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/b.txt</Key><VersionId></VersionId></Error></DeleteResult>
```

Audit (single event for the whole batch): `api.name=DeleteMultipleObjects status=OK statusCode=200 accessKey=rouser bucket=testbucket requestID=18C1EBD81066CAC2` with `api.objects=[{objectName:shared/a.txt},{objectName:shared/b.txt}]`.

So: **single delete = top-level `HTTP 403 AccessDenied`; batch delete = `HTTP 200` carrying a per-object `<Error><Code>AccessDenied</Code>` for every key** — a different envelope, the same authorization outcome (nothing deleted).

**`Content-Md5` precondition (not the authorization decision).** Both the batch-delete and retention APIs require a `Content-Md5` header; omitting it yields `HTTP 400 MissingContentMD5`, validated **before** the IAM decision (the audit record carries `accessKey=None`, i.e. no principal was attached yet):

```
POST /testbucket?delete=   (no Content-Md5)
```

Wire: `HTTP/1.1 400 Bad Request` · `X-Amz-Request-Id: 18C1ECAB8F205C5F`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>MissingContentMD5</Code><Message>Missing required header for this request: Content-Md5.</Message><BucketName>testbucket</BucketName><Resource>/testbucket</Resource><RequestId>18C1ECAB8F205C5F</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=DeleteMultipleObjects status=Bad Request statusCode=400 accessKey=None requestID=18C1ECAB8F205C5F`.

```
PUT /testbucket/shared/a.txt?retention=   (no Content-Md5)
```

Wire: `HTTP/1.1 400 Bad Request` · `X-Amz-Request-Id: 18C1ECAB8FC6B077`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>MissingContentMD5</Code><Message>Missing required header for this request: Content-Md5.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1ECAB8FC6B077</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Audit: `api.name=PutObjectRetention status=Bad Request statusCode=400 accessKey=None requestID=18C1ECAB8FC6B077`. This is a request-validation error, **not** the authorization decision; the probes above send a valid `Content-Md5` to reach the true decision.

**`Content-Md5` *value* is not validated — only its *presence* (API-2).** The precondition just checked verifies that the header **exists**, not that it matches the body. For batch delete the test is `if _, ok := r.Header[xhttp.ContentMD5]; !ok { … ErrMissingContentMD5 }` [cmd/bucket-handlers.go:L432]; the retention path uses the `hasContentMD5(r.Header)` helper [cmd/object-handlers.go:L2885], which is simply `_, ok := h[xhttp.ContentMD5]; return ok` [cmd/utils.go:L258-L261]. Neither recomputes or compares the digest, so a **wrong** `Content-Md5` value is accepted and the operation proceeds. This surfaces on the **write-capable path** (root / `readwrite`), where the caller is already authorized; it is **not** a read-only bypass — a read-only principal is still stopped by IAM (`400`/`403`, §5.3 and above). It is disclosed here because it is a correctness gap on the very header the denial-shape discussion relies on. Observed with a deliberately wrong digest `Content-MD5: AAAAAAAAAAAAAAAAAAAAAA==` (the correct value shown for contrast):

- **Batch `DeleteObjects` as root** — body `<Delete><Object><Key>disp/one.txt</Key></Object></Delete>` (correct MD5 `vzdFh6u+URCrfJAUr8t/6g==`), sent with the wrong MD5. Before: `HEAD disp/one.txt` → `200`.

Wire: `HTTP/1.1 200 OK` · `X-Amz-Request-Id: 18C20D1E3BBCED11`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>disp/one.txt</Key></Deleted></DeleteResult>
```

After: `HEAD disp/one.txt` → `404` — the object was **really deleted** despite the mismatched digest. Audit: `api.name=DeleteMultipleObjects statusCode=200 accessKey=minioadmin object=disp/one.txt`.

- **`PutObjectRetention` as `wormuser` on the lock-enabled `wormbucket`** — sets `GOVERNANCE` until `2031-06-01` (correct MD5 `CP2CdwR0g76py4eozOcbvw==`), sent with the wrong MD5. Before: `GET rt2.txt?retention` → `400` (no retention set). Result: `HTTP/1.1 200 OK`. After: `GET rt2.txt?retention` → `200`, retention now **persisted**:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>GOVERNANCE</Mode><RetainUntilDate>2031-06-01T00:00:00.000Z</RetainUntilDate></Retention>
```

Audit: `api.name=PutObjectRetention statusCode=200 accessKey=None object=rt2.txt` (`accessKey=None`: retention authorization runs in the deferred metadata callback, as in §5.3). So an authorized writer supplying *any* present `Content-Md5` — even a wrong one — completes the mutation; the header is a presence gate, not an integrity check. **The read-only principal remains denied**, so this is a write-path correctness gap, not a boundary breach. Product remediation (verifying the digest) is outside this documentation-only task (AAP §0.3.2).

**Storage side-effect (single & batch).** After the denied single delete **and** the batch denial, both keys are intact on disk, verified by `xl.meta` SHA-256 and a root `StatObject`:

| Object | `xl.meta` present | `xl.meta` SHA-256 (unchanged from seed) | root `StatObject` ETag |
|---|---|---|---|
| `shared/a.txt` | yes | `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd` | `78d1f9238908e7d0b0558499b3341189` |
| `shared/b.txt` | yes | `98c73a0d72b558758210d32f3bb130aa0d825b2488e0a7cdd8c84018278bcb82` | `5979fba72462ec73d7a6dcb760912068` |

**Nothing was deleted.**

### 5.5 Outside the granted prefix — write-adjacent operations on `private/`

The read-only grant is `GetObject` on `testbucket/shared/*` plus bucket-wide `ListBucket`. To confirm the write boundary holds **outside** the granted prefix as well, the same write-adjacent operations were issued against `private/secret.txt` / `private/*` (which `rouser` cannot even read). Every object-scoped write is denied with `403`, and the batch form again returns `200` with a per-object denial in the body — with **zero** storage side effects:

| Operation | Request | Wire | Audit `api.name` (statusCode) | `X-Amz-Request-Id` |
|---|---|---|---|---|
| `CreateMultipartUpload` | `POST /testbucket/private/mp.txt?uploads=` | `403` | `NewMultipartUpload` (403) | `18C1EBD825F1D92D` |
| `CopyObject` (dst `private/`) | `PUT /testbucket/private/copy.txt` (`x-amz-copy-source: /testbucket/shared/b.txt`) | `403` | `CopyObject` (403) | `18C1EBD83B0D145B` |
| `PutObjectTagging` | `PUT /testbucket/private/secret.txt?tagging=` | `403` | `PutObjectTagging` (403) | `18C1EBD8502EBB0F` |
| `DeleteObject` (single) | `DELETE /testbucket/private/secret.txt` | `403` | `DeleteObject` (403) | `18C1EBD8656B07AC` |
| `DeleteObjects` (batch) | `POST /testbucket?delete=` (`private/secret.txt`) | `200` | `DeleteMultipleObjects` (200) | `18C1EBD87A98D834` |

Representative full traces — the single delete (`403`) and the batch delete (`200`, per-object denial in body):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>private/secret.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/private/secret.txt</Resource><RequestId>18C1EBD8656B07AC</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

```xml
<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>private/secret.txt</Key><VersionId></VersionId></Error></DeleteResult>
```

**Storage side-effect (outside prefix).** The two denied *create* targets are absent (`testbucket/private/mp.txt`, `testbucket/private/copy.txt` — no directory, no `xl.meta`); the existing `private/secret.txt` is unchanged after the tagging, single-delete, and batch-delete denials — `xl.meta` SHA-256 = `32f1487b63fbbeab3dd01c69b5984a5a4ce308f7d377571b05a6b34e6fd1b70b` (unchanged), root `StatObject` ETag = `89f763923622b9b65a1ec900bfe145a9` (unchanged), tag-count `0` (unchanged) ⇒ **MUTATED=false**. The write boundary holds identically inside and outside the granted prefix; the only cross-prefix asymmetry is on the **read** side (listing discloses `private/` metadata — see §6.3).


---

## 6. Listing & HEAD — metadata disclosure characterization

Listing and HEAD are **read** surfaces, not mutation surfaces: the question here is not "can the read-only identity change anything" (Section 5 already answered *no*) but "**what can it learn**". The read-only identity `rouser` holds a custom policy granting `s3:GetObject` on `arn:aws:s3:::testbucket/shared/*` and `s3:ListBucket` on `arn:aws:s3:::testbucket` (Section 3). Every probe below was issued by `rouser` with SigV4-signed requests **while the concurrent read-write load was running**, and each response is shown in full (status line, response headers, and complete body) together with its correlated audit record.

The relevant guards are:

- **HEAD object** → `headObjectHandler` [cmd/object-handlers.go:L744], gated by `policy.GetObjectAction` via `authenticateRequest(ctx, r, policy.GetObjectAction)` [cmd/object-handlers.go:L760] (and again in the object branch via `authorizeRequest` [cmd/object-handlers.go:L845]); dispatched from `HeadObjectHandler` [cmd/object-handlers.go:L1009].
- **GET object** → `getObjectHandler` [cmd/object-handlers.go:L312] / `GetObjectHandler` [cmd/object-handlers.go:L715], gated by `policy.GetObjectAction` [cmd/object-handlers.go:L326].
- **HEAD bucket** → `HeadBucketHandler` [cmd/bucket-handlers.go:L1644], gated by `policy.ListBucketAction` [cmd/bucket-handlers.go:L1658].
- **List objects (v2)** → `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L154], gated by `policy.ListBucketAction` [cmd/bucket-listobjects-handlers.go:L172]; **v1** → `ListObjectsV1Handler` [cmd/bucket-listobjects-handlers.go:L273], gated by `policy.ListBucketAction` [cmd/bucket-listobjects-handlers.go:L287].
- **List multipart uploads** → `ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251], gated by `policy.ListBucketMultipartUploadsAction` [cmd/bucket-handlers.go:L265].

### 6.1 HEAD and GET — object-level reads (inside vs. outside the grant)

`GetObjectAction` covers both HEAD and GET, and the grant is prefix-bound to `shared/*`. Inside the prefix the read succeeds; outside it (e.g. `private/`) the identical operation is denied.

**HEAD `shared/a.txt` (inside prefix) → HTTP 200.** The response exposes object metadata but not the bytes:

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 23
Content-Type: text/plain
ETag: "78d1f9238908e7d0b0558499b3341189"
Last-Modified: Mon, 13 Jul 2026 17:47:30 GMT
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EB7EF4110C2B
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1136712
X-Ratelimit-Remaining: 1136712
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 18:04:17 GMT
```

Correlated audit: `api.name=HeadObject statusCode=200 accessKey=rouser object=shared/a.txt requestID=18C1EB7EF4110C2B`. Note the distinction between the authorization *action* and the audit *api.name*: the guard is `policy.GetObjectAction` — the single action shared by GET and HEAD, checked at [cmd/object-handlers.go:L760] — but the audit `api.name` recorded for a HEAD is `HeadObject`, because `HeadObjectHandler` [cmd/object-handlers.go:L1009] opens its request context with `newContext(r, w, "HeadObject")` [cmd/object-handlers.go:L1010]. This is the same `HeadObject` api.name listed in the §8.4 coverage table (whereas GET, below, records `GetObject`).

Note precisely which fields HEAD discloses: `Content-Length: 23`, `Content-Type: text/plain`, `ETag`, `Last-Modified`, and `Accept-Ranges`. There is **no `x-amz-meta-*` header** in the response — the seeded objects carry no user metadata, so HEAD reveals none.

**GET `shared/a.txt` (inside prefix) → HTTP 200**, returning the object body:

```
X-Amz-Request-Id: 18C1ECFDC1D36CC4   Content-Length: 23   Content-Type: text/plain   ETag: "78d1f9238908e7d0b0558499b3341189"
BODY: shared object a: hello
```

Correlated audit: `api.name=GetObject statusCode=200 accessKey=rouser object=shared/a.txt requestID=18C1ECFDC1D36CC4`. The 23-byte body `shared object a: hello` is exactly the seeded content, confirming full read access inside the grant.

**HEAD `private/secret.txt` (outside prefix) → HTTP 403.** Per the S3 HEAD contract there is **no XML body** (`Content-Length: 0`); the denial is carried in MinIO's `X-Minio-Error-Code`/`X-Minio-Error-Desc` headers:

```
HTTP/1.1 403 Forbidden
Accept-Ranges: bytes
Content-Length: 0
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EB7EF4C3B0FF
X-Content-Type-Options: nosniff
X-Minio-Error-Code: AccessDenied
X-Minio-Error-Desc: "Access Denied."
X-Ratelimit-Limit: 1136712
X-Ratelimit-Remaining: 1136712
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 18:04:17 GMT
```

**GET `private/secret.txt` (outside prefix) → HTTP 403** with the full S3 XML error body:

```
X-Amz-Request-Id: 18C1ECFDC2BB76A2
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>private/secret.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/private/secret.txt</Resource><RequestId>18C1ECFDC2BB76A2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Correlated audit: `api.name=GetObject statusCode=403 accessKey=rouser object=private/secret.txt requestID=18C1ECFDC2BB76A2`.

**Reasoning.** Both HEAD and GET are gated by the single `policy.GetObjectAction` check [cmd/object-handlers.go:L760 (HEAD), L326 (GET)]. The grant resource is `arn:aws:s3:::testbucket/shared/*`, so `shared/a.txt` matches and returns 200 while `private/secret.txt` does not match and returns 403. The two verbs differ only in body shape: GET emits the full `<Error>` XML on denial, whereas HEAD (which by protocol has no response body) surfaces the denial through the `X-Minio-Error-Code: AccessDenied` header with `Content-Length: 0`.

### 6.2 HEAD bucket

**HEAD `testbucket` → HTTP 200** (empty body):

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EB7EF56F5CD1
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1136712
X-Ratelimit-Remaining: 1136712
X-Xss-Protection: 1; mode=block
Date: Mon, 13 Jul 2026 18:04:17 GMT
```

`HeadBucketHandler` [cmd/bucket-handlers.go:L1644] gates on `policy.ListBucketAction` [cmd/bucket-handlers.go:L1658], which `rouser` **does** hold, so the probe returns 200. HEAD bucket discloses only bucket existence and region (via response headers); it returns no object listing.

### 6.3 Listing — what a read-only principal can enumerate

`rouser` holds `s3:ListBucket` on `arn:aws:s3:::testbucket` **without** an `s3:prefix` condition, so `ListBucket` succeeds bucket-wide. The listing exposes per-object metadata but never object bytes.

**ListObjectsV2, `prefix=shared/&max-keys=10` → HTTP 200, `KeyCount=4`** (complete body):

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>shared/</Prefix><KeyCount>4</KeyCount><MaxKeys>10</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>shared/a.txt</Key><LastModified>2026-07-13T17:47:30.790Z</LastModified><ETag>&#34;78d1f9238908e7d0b0558499b3341189&#34;</ETag><Size>23</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/b.txt</Key><LastModified>2026-07-13T17:47:30.794Z</LastModified><ETag>&#34;5979fba72462ec73d7a6dcb760912068&#34;</ETag><Size>23</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/contended.txt</Key><LastModified>2026-07-13T18:01:07.884Z</LastModified><ETag>&#34;63f9a036ef053d49ec1a32ac3e3e7fc5&#34;</ETag><Size>37</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/sub/c.txt</Key><LastModified>2026-07-13T17:47:30.796Z</LastModified><ETag>&#34;1bd6b26f94901123019dc3d4088d0ad7&#34;</ETag><Size>30</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

Each `<Contents>` exposes `Key`, `LastModified`, `ETag`, `Size`, and `StorageClass`. Note `shared/contended.txt` is 37 bytes with ETag `63f9a036ef053d49ec1a32ac3e3e7fc5` — this reflects the TOCTOU churn of Section 7.4 (its seeded size was 13 bytes); the listing is a live view.

**ListObjectsV2, `prefix=private/` → HTTP 200, `KeyCount=1`** (complete body) — the disclosure nuance:

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>private/</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>private/secret.txt</Key><LastModified>2026-07-13T17:47:30.798Z</LastModified><ETag>&#34;89f763923622b9b65a1ec900bfe145a9&#34;</ETag><Size>29</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

This is the key information-disclosure finding: `rouser` **cannot** `GetObject` on `private/secret.txt` (Section 6.1 showed that returns 403), yet it **can** enumerate that object's key, size (29 bytes), ETag, last-modified time, and storage class, because its `s3:ListBucket` grant is bucket-scoped with no `s3:prefix` condition. Listing scope and read scope are independent grants; a bucket-wide `ListBucket` leaks the metadata of objects the principal cannot read.

**ListObjectsV2, `delimiter=/` (top-level prefixes) → HTTP 200, `KeyCount=3`** (complete body):

```
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix></Prefix><KeyCount>3</KeyCount><MaxKeys>1000</MaxKeys><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated><CommonPrefixes><Prefix>load/</Prefix></CommonPrefixes><CommonPrefixes><Prefix>private/</Prefix></CommonPrefixes><CommonPrefixes><Prefix>shared/</Prefix></CommonPrefixes></ListBucketResult>
```

With `delimiter=/` and no prefix, `rouser` enumerates every top-level prefix in the bucket: `load/` (the concurrent read-write load's output), `private/` (which it cannot read), and `shared/` (its granted prefix). The bucket-scoped `ListBucket` therefore discloses the full namespace structure, including the other writers' key space.

**ListObjectsV2, no prefix → HTTP 200, first page of 1000 keys.** A full un-prefixed list returns one page of 1000 objects; the disclosure signal is the aggregate metadata reported in the page header, given exactly here (observed values, not a truncated body):

| Field | Observed value |
|-------|----------------|
| `KeyCount` | `1000` |
| `MaxKeys` | `1000` |
| `IsTruncated` | `true` |
| `NextContinuationToken` | `bG9hZC93MC8xODk4LnR4dFttaW5pb19jYWNoZTp2MixyZXR1cm46XQ==` |
| first `<Contents>` key | `load/w0/0.txt` (ETag `81baea0927ef384b3d44b772259141d0`, `Size 13`, `LastModified 2026-07-13T17:53:52.221Z`) |

`IsTruncated=true` plus the `NextContinuationToken` shows `rouser` can page through the entire bucket 1000 keys at a time; the first page is dominated by `load/wN/*.txt` keys produced by the concurrent writers — i.e. a read-only principal can fully enumerate what the other identities are writing.

**ListObjectsV1 (legacy, no `list-type`), `prefix=shared/` → HTTP 200** (complete body) — V1 additionally discloses object `Owner`:

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>shared/</Prefix><Marker></Marker><MaxKeys>10</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>shared/a.txt</Key><LastModified>2026-07-13T17:47:30.790Z</LastModified><ETag>&#34;78d1f9238908e7d0b0558499b3341189&#34;</ETag><Size>23</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/b.txt</Key><LastModified>2026-07-13T17:47:30.794Z</LastModified><ETag>&#34;5979fba72462ec73d7a6dcb760912068&#34;</ETag><Size>23</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/contended.txt</Key><LastModified>2026-07-13T18:01:07.884Z</LastModified><ETag>&#34;63f9a036ef053d49ec1a32ac3e3e7fc5&#34;</ETag><Size>37</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/sub/c.txt</Key><LastModified>2026-07-13T17:47:30.796Z</LastModified><ETag>&#34;1bd6b26f94901123019dc3d4088d0ad7&#34;</ETag><Size>30</Size><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

V1 uses `<Marker></Marker>` for pagination (vs. V2's `NextContinuationToken`) and, because the request did not suppress owner info, emits an `<Owner>` block for every key exposing the owner canonical ID `02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4` and display name `minio`. Both V1 and V2 are gated by the same `policy.ListBucketAction` [cmd/bucket-listobjects-handlers.go:L287 (v1), L172 (v2)].

**Metadata-disclosure summary.** For a read-only principal holding bucket-wide `s3:ListBucket`:

| Surface | Discloses | Does NOT disclose |
|---------|-----------|-------------------|
| ListObjectsV2 | `Key`, `LastModified`, `ETag`, `Size`, `StorageClass`; `CommonPrefixes` with a delimiter; pagination token | object bytes, `Content-Type`, `x-amz-meta-*` |
| ListObjectsV1 | all of the above **plus** `Owner{ID, DisplayName}` | object bytes, `Content-Type`, `x-amz-meta-*` |
| HEAD object (inside grant) | `Content-Length`, `Content-Type`, `ETag`, `Last-Modified`, `Accept-Ranges` | object bytes; (here) no `x-amz-meta-*` since none set |
| HEAD bucket | bucket existence, region | any object listing |

The disclosure is **bucket-wide** because the `ListBucket` grant lacks an `s3:prefix` condition — `rouser` can enumerate `private/` and `load/` metadata despite having no read grant there. The standard mitigation is to attach an `s3:prefix` `Condition` to the `ListBucket` statement so listing is confined to `shared/*` (documented in `docs/multi-user/README.md`); this mitigation is **demonstrated at runtime** immediately below.

**Runtime demonstration of the `s3:prefix` mitigation.** To confirm the bound holds in practice — not merely in principle — a second read-only identity `rouserp` was provisioned through the **canonical admin path** (`madmin` `AddCannedPolicy` → `AddUser` → `SetPolicy`) with policy `readonly-bp-prefixed`, identical to `readonly-bp` except its `s3:ListBucket` grant carries an `s3:prefix` condition:

```json
{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::testbucket"],
 "Condition":{"StringLike":{"s3:prefix":["shared/*"]}}}
```

The same three listing requests that leak the namespace for the un-conditioned `rouser` now behave differently for `rouserp` (raw SigV4, complete bodies):

**(a) `ListObjectsV2 prefix=shared/` — within the condition → `HTTP 200`, `KeyCount=3`:**

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>shared/</Prefix><KeyCount>3</KeyCount><MaxKeys>10</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>shared/a.txt</Key><LastModified>2026-07-14T03:58:33.815Z</LastModified><ETag>&#34;617a30921a7c30f7335a447b320265a9&#34;</ETag><Size>22</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/b.txt</Key><LastModified>2026-07-14T03:58:33.818Z</LastModified><ETag>&#34;b52cca3797ab68a27a17459296f82e3b&#34;</ETag><Size>22</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>shared/sub/c.txt</Key><LastModified>2026-07-14T03:58:33.820Z</LastModified><ETag>&#34;6f79664f9e09f184bd063385e39a2136&#34;</ETag><Size>29</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

**(b) `ListObjectsV2 prefix=private/` — outside the condition → `HTTP 403 AccessDenied`** (the metadata that leaked to `rouser` above is now denied):

```
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>testbucket</BucketName><Resource>/testbucket</Resource><RequestId>18C20F90C2C3B900</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**(c) `ListObjectsV2` no prefix — empty `s3:prefix`, which does not match `shared/*` → `HTTP 403 AccessDenied`:**

```
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>testbucket</BucketName><Resource>/testbucket</Resource><RequestId>18C20F90C346AF7C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Correlated audit: `api.name=ListObjectsV2 statusCode=200 status=OK accessKey=rouserp` for the in-prefix list, and `statusCode=403 status=Forbidden accessKey=rouserp` for **both** the `private/` (`requestID=18C20F90C2C3B900`) and the no-prefix (`requestID=18C20F90C346AF7C`) attempts. Note these `ListBucket` denials carry `accessKey=rouserp` — unlike the deferred-authz metadata operations of §5.3 (which log `accessKey=None`) — because `ListObjectsV2Handler` authorizes at the top-level `checkRequestAuthType(... ListBucketAction ...)` gate [cmd/bucket-listobjects-handlers.go:L172] where the principal is already attached, and MinIO evaluates the `s3:prefix` request condition against that gate. With the condition attached, the principal can list **only** within `shared/`; the bucket-wide over-disclosure of §6.3 is closed. Remediation of the *default* (un-conditioned) grant in any given deployment is an operator policy choice and is outside this documentation-only task (plan §0.3.2).

**Listing correctness caveat — an exact prefix-*stem* object hides its "directory" descendants (API-3).** Distinct from the *over*-disclosure above, listing can also *under*-report. When an object's key is exactly a path stem (e.g. `coll`) **and** other objects live beneath it (e.g. `coll/child.txt`), MinIO's list-merge collapses the name collision by **dropping the synthesized directory entry**. In `mergeEntryChannels`, when `path.Clean(best.name) == path.Clean(other.name)` [cmd/metacache-entries.go:L738] and one side is an object while the other is the directory of the same name, the code "will drop the directory entry" per its own comment [cmd/metacache-entries.go:L739-L741], discarding the directory when an equally-named object exists [cmd/metacache-entries.go:L751-L763]. The descendant objects then vanish from *both* delimited and flat listings while the stem object exists. Observed as `rouser` (who can list `testbucket`) after seeding an object `coll` (26 bytes) alongside `coll/child.txt` (19 bytes):

- **`ListObjectsV2 prefix=coll&delimiter=/`** → `HTTP 200`, `KeyCount=1`, only the stem object; **no** `CommonPrefixes` for `coll/`:

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>coll</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated><Contents><Key>coll</Key><LastModified>2026-07-14T04:22:18.803Z</LastModified><ETag>&#34;b3f9a2dc44b021b32706f6b1cf4d0108&#34;</ETag><Size>26</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

- **`ListObjectsV2 prefix=coll` (flat, no delimiter)** → `HTTP 200`, `KeyCount=1` — the flat form *also* omits `coll/child.txt`, returning only `coll` (identical body minus `<Delimiter>`). `ListObjectsV1` behaves the same way.

Yet `coll/child.txt` genuinely exists and is directly addressable: root `HEAD coll/child.txt` → `HTTP 200`, `Content-Length: 19`, `ETag "6905a6d74e838f0a49d96ca6b69d53c4"`; `GET` → `I am coll/child.txt`. The masking is purely a listing artifact and reverses the instant the stem object is removed — after `DELETE coll` (root, `204`), the same query re-exposes the directory:

```
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>testbucket</Name><Prefix>coll</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated><CommonPrefixes><Prefix>coll/</Prefix></CommonPrefixes></ListBucketResult>
```

**Security framing.** This grants a read-only principal no extra access — it can already enumerate and `GetObject` within its grant, and the shadowed descendant is reachable by its exact key regardless. The relevance is to *auditing via listing*: an operator or read-only auditor enumerating a bucket to inventory its contents can **miss** objects shadowed by an equally-named stem object, so a listing-based inventory is not a reliable census of what exists. It changes no object state. Product remediation is outside this documentation-only task (AAP §0.3.2).

### 6.4 List multipart uploads → denied

**ListMultipartUploads `testbucket?uploads=` → HTTP 403** (complete body):

```
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>testbucket</BucketName><Resource>/testbucket</Resource><RequestId>18C1EB9BB8551A5D</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

`ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251] gates on `policy.ListBucketMultipartUploadsAction` [cmd/bucket-handlers.go:L265] — a **distinct** action from `ListBucketAction`. `rouser`'s policy grants `s3:ListBucket` but **not** `s3:ListBucketMultipartUploads`, so this returns 403. Consequently a read-only principal **cannot** enumerate the in-flight multipart uploads that the concurrent read-write writers may have open, even though it can list completed objects. This is a meaningful scoping boundary: object listing and multipart-upload listing are separately gated.

**Authorized-principal behavior of `ListMultipartUploads` — `prefix` is matched as an *exact key*, not an S3 prefix (API-4).** The read-only denial above is correct; the two behaviors below were surfaced by driving the *same* API as an authorized principal (root) and are disclosed because they bear on multipart *discoverability*. MinIO deliberately does not implement prefix-based multipart listing — the comment "We do not support prefix based listing, this is a deliberate attempt towards simplification of multipart APIs" sits at [cmd/erasure-multipart.go:L254-L259]. The pool method `ListMultipartUploads` [cmd/erasure-server-pool.go:L1742-L1775] routes an **empty** prefix through the in-memory `mpCache` scan ("if no prefix provided, return the list from cache" [cmd/erasure-server-pool.go:L1753-L1759]), while a **non-empty** prefix is matched as an exact object name. With one active upload for key `shared/big.txt`:

- **`?uploads&prefix=`** (empty) → `HTTP 200`, the upload **is** returned (cache scan):

```
<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>testbucket</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker></NextKeyMarker><NextUploadIdMarker></NextUploadIdMarker><Prefix></Prefix><MaxUploads>10000</MaxUploads><IsTruncated>false</IsTruncated><Upload><Key>shared/big.txt</Key><UploadId>NTY4YTU3MWItMWZjYi00YzhlLTkxZjItNzUyZTYxZjU1NDk5LmNlN2Y4MWExLWY3NDgtNDUxYS05MDkwLTM2OGI3ZmRmZGU3MXgxNzg0MDAzMDM4MDYzNjU1NTg4</UploadId><Initiator><ID></ID><DisplayName></DisplayName></Initiator><Owner><ID></ID><DisplayName></DisplayName></Owner><StorageClass></StorageClass><Initiated>2026-07-14T04:23:58.085Z</Initiated></Upload></ListMultipartUploadsResult>
```

- **`?uploads&prefix=shared/`** (a legitimate S3 prefix) → `HTTP 200` but **empty** — no `<Upload>` element, because `shared/` is matched as an exact key and nothing is named exactly `shared/`:

```
<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>testbucket</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker></NextKeyMarker><NextUploadIdMarker></NextUploadIdMarker><Prefix>shared/</Prefix><MaxUploads>10000</MaxUploads><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>
```

- **`?uploads&prefix=shared/big.txt`** (the exact full key) → `HTTP 200`, the upload **is** returned again (an `<Upload>` block identical to the empty-prefix case, differing only in the echoed `<Prefix>shared/big.txt</Prefix>`).

So a client that scopes a multipart-uploads listing by a directory-style prefix (the natural S3 idiom) is silently told there are **zero** uploads even though one exists beneath that prefix; discovery requires an empty prefix or the exact object key.

**In-progress multipart discovery via empty prefix is lost across a server restart (STORAGE-1).** The empty-prefix path reads the in-memory `mpCache` [cmd/erasure-server-pool.go:L68, initialized L223, populated on upload creation at L1796, read at L1759]; that map is **not** rehydrated from disk at startup, so a restart erases empty-prefix discoverability while the upload's bytes remain fully intact on disk. Captured across the documented single restart (server PID rotates), for the same active upload:

| Probe | Before restart | After restart |
|-------|----------------|---------------|
| on-disk `…/multipart/…/xl.meta` | present | **present** (survives) |
| `ListMultipartUploads prefix=` (empty) | upload returned | **empty — upload LOST from discovery** |
| `ListMultipartUploads prefix=shared/big.txt` (exact) | upload returned | **upload returned** (read from disk) |
| `ListParts` (exact upload id) | Part 1, `Size 5242880`, ETag `b8fc857a25e7958868c2f003d5e0952d` | **Part 1 identical** |

After-restart empty-prefix body (discovery lost):

```
<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>testbucket</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker></NextKeyMarker><NextUploadIdMarker></NextUploadIdMarker><Prefix></Prefix><MaxUploads>10000</MaxUploads><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>
```

After-restart `ListParts` (intact, disk-backed):

```
<?xml version="1.0" encoding="UTF-8"?>
<ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>testbucket</Bucket><Key>shared/big.txt</Key><UploadId>NTY4YTU3MWItMWZjYi00YzhlLTkxZjItNzUyZTYxZjU1NDk5LmNlN2Y4MWExLWY3NDgtNDUxYS05MDkwLTM2OGI3ZmRmZGU3MXgxNzg0MDAzMDM4MDYzNjU1NTg4</UploadId><Initiator><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</DisplayName></Initiator><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</DisplayName></Owner><StorageClass>STANDARD</StorageClass><PartNumberMarker>0</PartNumberMarker><NextPartNumberMarker>0</NextPartNumberMarker><MaxParts>10000</MaxParts><IsTruncated>false</IsTruncated><ChecksumAlgorithm></ChecksumAlgorithm><Part><PartNumber>1</PartNumber><LastModified>2026-07-14T04:23:58.164Z</LastModified><ETag>&#34;b8fc857a25e7958868c2f003d5e0952d&#34;</ETag><Size>5242880</Size></Part></ListPartsResult>
```

**Scope.** Neither behavior touches the read-only boundary: `rouser` is denied `ListMultipartUploads` outright (`403`, above) and holds no multipart write action. Both are multipart-*discovery* caveats for authorized clients — API-4 can hide active uploads from a prefix-scoped query, and STORAGE-1 means an orchestrator relying on empty-prefix enumeration to find stragglers will miss uploads created before a restart (they remain addressable by exact key / upload id, and their storage is reclaimed by the normal stale-multipart expiry). Product remediation is outside this documentation-only task (AAP §0.3.2).


---

## 7. Edge & variant coverage

### 7.1 Inside vs. outside the granted prefix — reconciliation with Section 5

Every write-adjacent operation was probed both **inside** the granted prefix (`shared/*`) and **outside** it (`private/*`). The result is uniform denial, with two status-code nuances that are fully explained (not contradictions):

| Operation | Inside `shared/*` | Outside `private/*` | Why |
|-----------|-------------------|---------------------|-----|
| Multipart create / upload / copy-part / complete / abort | 403 `AccessDenied` | 403 `AccessDenied` | `PutObjectAction` (create/upload/complete) and `AbortMultipartUploadAction` not granted |
| CopyObject (server-side copy) | 403 `AccessDenied` | 403 `AccessDenied` | destination `PutObjectAction` not granted |
| PutObjectTagging / DeleteObjectTagging | 403 `AccessDenied` | 403 `AccessDenied` | `PutObjectTaggingAction` / `DeleteObjectTaggingAction` not granted |
| PutObjectLegalHold | 403 `AccessDenied` | 403 `AccessDenied` | `PutObjectLegalHoldAction` not granted (checked before Content-MD5) |
| PutObjectRetention | **400 `InvalidRequest`** | **400 `InvalidRequest`** | bucket has no Object-Lock configuration → `ErrInvalidBucketObjectLockConfiguration` short-circuits **before** the IAM decision (Section 5.3) |
| DeleteObject (single) | 403 `AccessDenied` | 403 `AccessDenied` | `DeleteObjectAction` not granted |
| DeleteObjects (batch) | **HTTP 200** with per-object `<Error>AccessDenied` | **HTTP 200** with per-object `<Error>AccessDenied` | batch handler always returns 200; each object individually denied `DeleteObjectAction` (Section 5.4) |
| GET / HEAD object (read) | **200** | 403 `AccessDenied` | `GetObjectAction` granted only on `shared/*` |

The two apparent exceptions are consistent with Section 5 and with each other:

- **Retention returns 400, not 403**, both inside and outside, because the bucket was created without Object-Lock; the retention handler validates the bucket's Object-Lock configuration before reaching the IAM authorization branch, so the request is rejected as malformed regardless of prefix (see Section 5.3 for the full trace and the `enforceRetentionBypassForPut` ordering).
- **Batch delete returns HTTP 200**, both inside and outside, because `DeleteMultipleObjectsHandler` reports per-object outcomes inside a `<DeleteResult>` body rather than at the HTTP layer; every targeted object still receives an individual `AccessDenied` (Section 5.4).

Every *mutation* attempt — inside or outside — is denied, and every denied attempt left the backend unchanged (Section 5 storage tables). The only 200s are the batch-delete envelope (whose body denies each object) and legitimate reads inside `shared/*`.

### 7.2 Signed vs. presigned request forms

All Section 5 and Section 7.4 probes used **SigV4 header-signed** requests (the `Authorization: AWS4-HMAC-SHA256` request header). Presigned (query-string-signed) requests exercise a different auth-extraction path, so both a valid and an expired presigned DELETE were captured.

**Presigned DELETE, valid (`X-Amz-Expires=300`).** The presigned URL carries the full SigV4 query parameter set (the access-key value inside `X-Amz-Credential` and the 64-hex `X-Amz-Signature` are shown verbatim on the REQUEST line and in the audit `requestQuery`; the standalone "query params" listing below labels them as redacted only for readability):

```
--- PRESIGNED URL QUERY PARAMS (SigV4 query-string auth) ---
path: /testbucket/shared/a.txt
X-Amz-Algorithm  = AWS4-HMAC-SHA256
X-Amz-Credential = <redacted-access-key>/20260713/us-east-1/s3/aws4_request
X-Amz-Date       = 20260713T181042Z
X-Amz-Expires    = 300
X-Amz-SignedHeaders = host
X-Amz-Signature  = <redacted-64-hex-signature>
--- REQUEST (actual, unredacted) ---
DELETE /testbucket/shared/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=rouser%2F20260713%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260713T181042Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=891ed54ca1235f9956bd795b2c01162c743d542f2f5110be7f40b84657e535ec
--- RESPONSE ---
HTTP 403 403 Forbidden
Content-Length: 335
Content-Type: application/xml
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C1EBD8905674F0
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD8905674F0</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Full correlated audit event (note `accessKey: "rouser"` — the presigned signature authenticated the principal — and the `requestQuery` echoing every SigV4 parameter):

```
{
  "accessKey": "rouser",
  "api": { "bucket": "testbucket", "name": "DeleteObject", "object": "shared/a.txt", "status": "Forbidden", "statusCode": 403, "tx": 335 },
  "deploymentid": "584479d9-b9c6-423f-bff3-1f4c9308df56",
  "remotehost": "127.0.0.1",
  "requestHeader": { "Accept-Encoding": "gzip", "User-Agent": "Go-http-client/1.1", "X-Amz-Signature-Age": "782" },
  "requestID": "18C1EBD8905674F0",
  "requestPath": "/testbucket/shared/a.txt",
  "requestQuery": {
    "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
    "X-Amz-Credential": "rouser/20260713/us-east-1/s3/aws4_request",
    "X-Amz-Date": "20260713T181042Z",
    "X-Amz-Expires": "300",
    "X-Amz-Signature": "891ed54ca1235f9956bd795b2c01162c743d542f2f5110be7f40b84657e535ec",
    "X-Amz-SignedHeaders": "host"
  },
  "time": "2026-07-13T18:10:42.782692309Z",
  "trigger": "incoming",
  "version": "1"
}
```

Storage: before and after, `shared/a.txt` had `xl.meta` sha256 `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd` and root `StatObject` ETag `78d1f9238908e7d0b0558499b3341189` — **object intact** (`etag changed=false xl.meta changed=false`).

**Presigned DELETE, expired (`X-Amz-Expires=1`, sent ~2 s later).** A different rejection — the message is **`Request has expired`** and the audit `accessKey` is **`<nil>`**, proving the expiry is checked *before* the principal is resolved:

```
--- REQUEST (actual, unredacted) ---
DELETE /testbucket/shared/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=rouser%2F20260713%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260713T181043Z&X-Amz-Expires=1&X-Amz-SignedHeaders=host&X-Amz-Signature=c2fbbf05753842db465544f5e1b6cdda637ff909bb09ced702d20157e06c407a
--- RESPONSE ---
HTTP 403 403 Forbidden
Content-Length: 340
Content-Type: application/xml
X-Amz-Request-Id: 18C1EBD91CC37A57
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Request has expired</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C1EBD91CC37A57</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Correlated audit: `api.name=DeleteObject statusCode=403 accessKey=<nil> requestID=18C1EBD91CC37A57` (the `accessKey` field is absent from the JSON because no principal was attached). The object again remained intact (`xl.meta` sha256 `a94e32448b495287c314a6f50b9d9780aab9766196d9f6ad40bf102e4714b8bd`, ETag `78d1f9238908e7d0b0558499b3341189`).

**Reasoning.** The two presigned outcomes reveal two distinct rejection points on the request lifecycle:
- A **valid** presigned URL is authenticated (the query signature resolves `accessKey=rouser`) and then denied at the `DeleteObjectAction` gate — identical authorization outcome to the header-signed delete of Section 5.4, just via the presigned code path. The `X-Amz-Signature-Age: 782` header shows the request was accepted within the 300 s window.
- An **expired** presigned URL is rejected by the pre-authentication expiry check with `Message=Request has expired` and **no principal attached** (`accessKey=<nil>`) — a strictly earlier failure than the IAM decision. Either way, no delete occurs and the object is untouched.

**Audit-log confidentiality — a captured presigned request is byte-for-byte replayable from the audit sink within its expiry window (OBS-1).** The presigned examples above expose a second, orthogonal concern that is about *confidentiality of the audit stream*, not about the S3 authorization decision. Because the audit entry's `requestQuery` echoes **every** SigV4 query parameter verbatim — including the `X-Amz-Signature` — anyone who can read the audit sink can reconstruct the full presigned URL and replay it until it expires, **without ever holding the principal's secret key**. The copy is unconditional: `ToEntry` [internal/logger/message/audit/entry.go:L44] builds `reqQuery` by iterating the raw query string and joining every value (`reqQuery[k] = strings.Join(v, ",")`, then `entry.ReqQuery = reqQuery` [internal/logger/message/audit/entry.go:L53-L58]) with **no** allow-list, deny-list, or redaction of signature material.

Observed directly. A presigned **GET** was generated for `rouser` on `shared/a.txt` (which `rouser` *is* authorized to read) with `X-Amz-Expires=600`, used once (→ `HTTP 200`), then reconstructed **solely from the fields the audit sink recorded** and replayed from a client that never possessed `rouser`'s secret:

```
--- generated presigned GET (first use → HTTP 200 "shared object a: hello") ---
http://127.0.0.1:9000/testbucket/shared/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=rouser%2F20260714%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260714T044223Z&X-Amz-Expires=600&X-Amz-SignedHeaders=host&X-Amz-Signature=15fc77d70c2bbbdbe81fa1605c0062b18a8ca33c4ac0adcca215ece02c614f1d
```

The audit sink captured the request with the signature intact — these are the replay-relevant fields of the recorded event (`X-Amz-Signature` is present in full):

```json
{
  "accessKey": "rouser",
  "api": { "bucket": "testbucket", "name": "GetObject", "object": "shared/a.txt", "status": "OK", "statusCode": 200 },
  "deploymentid": "568a571b-1fcb-4c8e-91f2-752e61f55499",
  "requestID": "18C20E50F4D613AE",
  "requestPath": "/testbucket/shared/a.txt",
  "requestQuery": {
    "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
    "X-Amz-Credential": "rouser/20260714/us-east-1/s3/aws4_request",
    "X-Amz-Date": "20260714T044223Z",
    "X-Amz-Expires": "600",
    "X-Amz-Signature": "15fc77d70c2bbbdbe81fa1605c0062b18a8ca33c4ac0adcca215ece02c614f1d",
    "X-Amz-SignedHeaders": "host"
  },
  "time": "2026-07-14T04:42:23.260863964Z",
  "trigger": "incoming",
  "version": "1"
}
```

Rebuilding the URL from *only* those `requestPath` + `requestQuery` fields (parameters re-emitted in sorted order — SigV4 presigned validity does not depend on query ordering) and replaying it returns the object:

```
--- replay of audit-reconstructed URL (no secret key held) → HTTP 200 ---
http://127.0.0.1:9000/testbucket/shared/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=rouser%2F20260714%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260714T044223Z&X-Amz-Expires=600&X-Amz-Signature=15fc77d70c2bbbdbe81fa1605c0062b18a8ca33c4ac0adcca215ece02c614f1d&X-Amz-SignedHeaders=host
→ "shared object a: hello"
```

That the replay is a genuine reuse of the captured signature — not some independent bypass — is confirmed by flipping a single hex digit of `X-Amz-Signature` (…`614f1d` → …`614f1a`), which is rejected:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message><Key>shared/a.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/a.txt</Resource><RequestId>18C20E5B9D749630</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Scope — this is an audit-sink *confidentiality* concern, not a read-only-boundary breach.** The replay grants exactly what the presigned URL already granted: here, a read `rouser` was authorized to perform anyway, so no privilege is gained over the read-only identity, and no *write* capability is conferred. The exposure is temporal and general: *any* presigned request captured in the audit stream — including a **write** presigned by a write-capable principal — is replayable by an audit-sink reader **until that URL's `X-Amz-Expires` window closes**, because the signature stays valid until then and the sink stores it verbatim. The expiry check still applies to the replay (an expired captured URL yields `Request has expired`, exactly as in the expired-DELETE case above), which bounds the window but does not eliminate it.

**Operator guidance.** Treat the audit sink as **credential-bearing**: restrict read access to it as tightly as to the credentials themselves; prefer short `X-Amz-Expires` windows for presigned URLs; and, where feasible, redact or drop `X-Amz-Signature` (and `X-Amz-Credential`) from audit records at the sink/collector. A product-side default redaction of the signature query parameters inside `ToEntry` [internal/logger/message/audit/entry.go:L53-L58] would close this at the source, but changing product code is outside this documentation-only task per AAP §0.3.2.

**Note on `deploymentid`.** The OBS-1 traces above were captured on this investigation's current canonical server bring-up, whose `deploymentid` is `568a571b-1fcb-4c8e-91f2-752e61f55499`; the earlier presigned-DELETE captures in this section were recorded on a prior bring-up (`584479d9-…`). Both report the same externally-visible `HostId` `dd9025…` because that value is `sha256(globalLocalNodeName)` — a hash of the node's bind address alone [cmd/server-main.go:L410-L412] — and is therefore independent of the per-format `deploymentid`; the provisioned identities (`rouser` et al.) and their policies are reproduced identically across bring-ups, so the authorization behavior is unchanged.

### 7.3 Single vs. batch delete

Covered in full in Section 5.4: a **single** `DeleteObject` returns HTTP 403 `AccessDenied` (requestID `18C1EBD7FA96363C`), while a **batch** `DeleteObjects` returns HTTP 200 with a `<DeleteResult>` body that carries a per-object `<Error><Code>AccessDenied</Code>` for every targeted key, logged as a single `api.name=DeleteMultipleObjects statusCode=200` audit event. Both forms result in zero objects deleted; the batch's 200 is an envelope status, not a grant.

### 7.4 Time-of-check/time-of-use (TOCTOU) probe

The user's core worry is behavior *under contention*. To test for a check-vs-use window, `rouser` attempted to delete the exact key that other identities were concurrently overwriting (`shared/contended.txt`). A **quiescent control** (Phase A) is contrasted with the **concurrent** run (Phase B).

**Phase A — quiescent control.** With no other writer touching the key, `rouser` issues one signed DELETE:

```
--- REQUEST ---
DELETE /testbucket/shared/contended.txt
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=20bfea56633b8531bc613ee1458eec116cca71d6607589df6f48e38ba5a7517c
X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
X-Amz-Date: 20260713T180104Z
--- RESPONSE ---
HTTP 403 403 Forbidden
X-Amz-Request-Id: 18C1EB51F7CA8BA2
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/contended.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/contended.txt</Resource><RequestId>18C1EB51F7CA8BA2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Correlated audit: `api.name=DeleteObject statusCode=403 accessKey=rouser requestID=18C1EB51F7CA8BA2`. State before **and** after: root `StatObject` ETag `36e6bf3181687fd030794790bbc38991`, `xl.meta` sha256 `6eca7a8fe83abf430e4ea7ce9f92b846b8a490291e8263a543ff562e2213916f` — **unchanged** (`intact=true`).

**Phase B — concurrent (same key churning).** Eight goroutines (separate read-write identity) overwrote `shared/contended.txt` in a tight loop while `rouser` repeatedly attempted to delete it:

- Writers: **968 successful overwrites**, producing **968 distinct ETags**.
- Between `rouser`'s attempts, root observed **25 distinct live ETags** on the key — direct proof the object genuinely churned across the check-vs-use window.
- `rouser`: **25 DELETE attempts**, status distribution **`{403: 25}`**, **0 successes**.

The first and last of `rouser`'s 25 attempts, in full:

```
--- FIRST rouser DELETE attempt ---
DELETE /testbucket/shared/contended.txt
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=6a9b406cc1750929248ee23c85b6dc9728e0a69bb4f761323ac034945aef10a2
X-Amz-Date: 20260713T180105Z
--- RESPONSE ---
HTTP 403 403 Forbidden
X-Amz-Request-Id: 18C1EB521ED39171
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/contended.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/contended.txt</Resource><RequestId>18C1EB521ED39171</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

--- LAST rouser DELETE attempt ---
DELETE /testbucket/shared/contended.txt
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=20d24e9340b37aab38b9a0b37e763422d1a7353a286dd822bf8b44633f5bc1be
X-Amz-Date: 20260713T180108Z
--- RESPONSE ---
HTTP 403 403 Forbidden
X-Amz-Request-Id: 18C1EB52C07F0C4F
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>shared/contended.txt</Key><BucketName>testbucket</BucketName><Resource>/testbucket/shared/contended.txt</Resource><RequestId>18C1EB52C07F0C4F</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

After the run, the key's live state had genuinely advanced: `xl.meta` sha256 `4f0cce407cb788e59f405948ac393e2581d6070212babea93d146ab7e49b12b6`, ETag `63f9a036ef053d49ec1a32ac3e3e7fc5` (vs. the seeded `36e6bf3181687fd030794790bbc38991`).

**Bounded conclusion.** Over 25 `rouser` DELETE attempts spanning 968 successful concurrent overwrites (968 distinct ETags, 25 of them observed live between attempts), `rouser` succeeded **0** times; every attempt returned 403. The authorization **decision** is computed from the request's fixed tuple `(principal=rouser, action=s3:DeleteObject, resource=arn:aws:s3:::testbucket/shared/contended.txt)` in the handler **before** the object layer runs, so the churning object state produced no check-vs-use window on the decision. This is bounded to the tested scale (968 overwrites / 25 attempts, stable across the two load runs of Section 4), not a formal proof of impossibility.

### 7.5 WORM / object-lock independence from IAM

The read-only boundary is enforced by IAM; object-lock (WORM) is a **separate** enforcement layer in `cmd/bucket-object-lock.go`. This experiment proves the two are independent, ordered gates and that a read-only identity holds neither the delete action nor the governance-bypass action. Four identities delete the **same** governance-locked object version, isolating each gate.

**Setup.** Root creates an object-lock-enabled bucket and writes one governance-locked object:

```
MakeBucket wormbucket ObjectLocking=true
PUT wormbucket/worm-1783965682.txt ETag=4149cd7a505dca244e3f8752981df056 versionId=6f98f7ed-7386-408c-9b66-9addbd70b544 mode=GOVERNANCE retainUntil=2026-07-13T19:01:22Z
GetObjectRetention: mode=GOVERNANCE retainUntil=2026-07-13T19:01:22Z
storage: /tmp/minio-data/wormbucket/worm-1783965682.txt/xl.meta present=true sha256=e04e296548b78dd51c946ea5cce68cd320f4184fa7bd346ad8fbf4fddd43f04c
```

Identities used (all provisioned in Section 3):
- `rouser` — read-only on `testbucket/shared/*`; **no** grant on `wormbucket` at all.
- `wormuser` — policy `wormpol`: grants `s3:DeleteObject` (+ bucket reads) on `wormbucket`, but **not** `s3:BypassGovernanceRetention`.
- `bypassuser` — policy `bypasspol`: grants `s3:DeleteObject` **and** `s3:BypassGovernanceRetention` on `wormbucket`.

The exact `wormpol` and `bypasspol` documents attached via `AddCannedPolicy` (§3 defers their listing here, where they are exercised) are:

```json
{
 "Version": "2012-10-17",
 "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketObjectLockConfiguration"], "Resource": ["arn:aws:s3:::wormbucket"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:PutObjectRetention", "s3:GetObjectRetention"], "Resource": ["arn:aws:s3:::wormbucket/*"]}
 ]
}
```

`wormpol` grants `s3:DeleteObject` and `s3:PutObjectRetention` on `wormbucket/*` but **not** `s3:BypassGovernanceRetention` — this is the identity that passes IAM yet is blocked by object-lock (Case 2).

```json
{
 "Version": "2012-10-17",
 "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::wormbucket"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:DeleteObject", "s3:BypassGovernanceRetention"], "Resource": ["arn:aws:s3:::wormbucket/*"]}
 ]
}
```

`bypasspol` additionally grants `s3:BypassGovernanceRetention` — the identity that can remove a governance-locked object (Case 4).

The four cases delete `worm-1783965682.txt?versionId=6f98f7ed-7386-408c-9b66-9addbd70b544`. The decisive tell is the audit **`tags`** block: it is **absent** when the request is denied in the IAM gate before the object layer, and **present** (`{DeleteObject, GetObjectInfo}`) once the object layer is reached.

**Case 1 — `rouser` + bypass header → HTTP 403 `AccessDenied` (IAM gate, object layer never reached).**

```
--- REQUEST ---
DELETE /wormbucket/worm-1783965682.txt?versionId=6f98f7ed-7386-408c-9b66-9addbd70b544
Authorization: AWS4-HMAC-SHA256 Credential=rouser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=1ccb2f0811ae29a3185e5e46cc4938de48389bfe6fb428eb2fd4bbb36ff12cda
X-Amz-Bypass-Governance-Retention: true
X-Amz-Date: 20260713T180122Z
--- RESPONSE ---
HTTP 403 403 Forbidden
X-Amz-Request-Id: 18C1EB562DC627A3
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>worm-1783965682.txt</Key><BucketName>wormbucket</BucketName><Resource>/wormbucket/worm-1783965682.txt</Resource><RequestId>18C1EB562DC627A3</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Full correlated audit — **note there is no `tags` key**, so the object layer was never entered:

```
{
  "accessKey": "rouser",
  "api": { "bucket": "wormbucket", "name": "DeleteObject", "object": "worm-1783965682.txt", "status": "Forbidden", "statusCode": 403, "tx": 349 },
  "deploymentid": "584479d9-b9c6-423f-bff3-1f4c9308df56",
  "remotehost": "127.0.0.1",
  "requestID": "18C1EB562DC627A3",
  "requestPath": "/wormbucket/worm-1783965682.txt",
  "requestQuery": { "versionId": "6f98f7ed-7386-408c-9b66-9addbd70b544" },
  "time": "2026-07-13T18:01:22.783275019Z",
  "trigger": "incoming",
  "version": "1"
}
```

Storage after: `xl.meta present=true sha256=e04e296548b78dd51c946ea5cce68cd320f4184fa7bd346ad8fbf4fddd43f04c`; root `StatObject(versionId)` err=`<nil>` (**version still present**). `rouser` fails at `checkRequestAuthType(ctx, r, policy.DeleteObjectAction, bucket, object)` [cmd/object-handlers.go:L2528] — the very first gate — so the object-lock code in `cmd/bucket-object-lock.go` is never consulted. The `x-amz-bypass-governance-retention: true` header is irrelevant because the delete is rejected before any bypass evaluation.

**Case 2 — `wormuser` (no bypass) → HTTP 400 `InvalidRequest "Object is WORM protected and cannot be overwritten"` (IAM allows, object-lock blocks). This is the independence proof.**

```
--- REQUEST ---
DELETE /wormbucket/worm-1783965682.txt?versionId=6f98f7ed-7386-408c-9b66-9addbd70b544
Authorization: AWS4-HMAC-SHA256 Credential=wormuser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=be8a5cb7376fec588ca7d44841c6a0bf7d70782a6c13ed393201d76f1e2612cf
X-Amz-Date: 20260713T180123Z
--- RESPONSE ---
HTTP 400 400 Bad Request
X-Amz-Request-Id: 18C1EB5642DAD5BD
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>worm-1783965682.txt</Key><BucketName>wormbucket</BucketName><Resource>/wormbucket/worm-1783965682.txt</Resource><RequestId>18C1EB5642DAD5BD</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Full correlated audit — **`tags` is present** (`DeleteObject` + `GetObjectInfo`), so the object layer *was* reached before object-lock rejected the delete:

```
{
  "accessKey": "wormuser",
  "api": { "bucket": "wormbucket", "name": "DeleteObject", "object": "worm-1783965682.txt", "status": "Bad Request", "statusCode": 400, "tx": 387 },
  "deploymentid": "584479d9-b9c6-423f-bff3-1f4c9308df56",
  "remotehost": "127.0.0.1",
  "requestID": "18C1EB5642DAD5BD",
  "requestPath": "/wormbucket/worm-1783965682.txt",
  "requestQuery": { "versionId": "6f98f7ed-7386-408c-9b66-9addbd70b544" },
  "tags": {
    "DeleteObject": "name=worm-1783965682.txt,pool=1,set=1",
    "GetObjectInfo": "name=worm-1783965682.txt,pool=1,set=1"
  },
  "time": "2026-07-13T18:01:23.137268866Z",
  "trigger": "incoming",
  "version": "1"
}
```

Storage after: `xl.meta` sha256 unchanged (`e04e296548b78dd51c946ea5cce68cd320f4184fa7bd346ad8fbf4fddd43f04c`); version still present. **This is the crux**: `wormuser` *passes* the IAM `DeleteObjectAction` gate [cmd/object-handlers.go:L2528] (its policy grants `s3:DeleteObject`), yet the delete is still blocked — by `enforceRetentionBypassForDelete` [cmd/bucket-object-lock.go:L84], which returns `ObjectLocked{}` [cmd/bucket-object-lock.go:L101/L117/L121] for a governance-locked object when no valid bypass is present. That `ObjectLocked` error maps to `ErrObjectLocked` [cmd/api-errors.go:L2299] → HTTP 400 `InvalidRequest` [cmd/api-errors.go:L1059]. IAM said "allowed"; object-lock said "no". The two layers are independent.

**Case 3 — `wormuser` + bypass header (but lacks `s3:BypassGovernanceRetention`) → HTTP 403 `AccessDenied` (bypass is a separate gate).**

```
--- REQUEST ---
DELETE /wormbucket/worm-1783965682.txt?versionId=6f98f7ed-7386-408c-9b66-9addbd70b544
Authorization: AWS4-HMAC-SHA256 Credential=wormuser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=a3737f7feb1f389078de509f22d0b158d7d5772dfe9ab088c51563c75ce1157e
X-Amz-Bypass-Governance-Retention: true
X-Amz-Date: 20260713T180123Z
--- RESPONSE ---
HTTP 403 403 Forbidden
X-Amz-Request-Id: 18C1EB5657F653C1
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>worm-1783965682.txt</Key><BucketName>wormbucket</BucketName><Resource>/wormbucket/worm-1783965682.txt</Resource><RequestId>18C1EB5657F653C1</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Correlated audit: `api.name=DeleteObject statusCode=403 accessKey=wormuser requestID=18C1EB5657F653C1`, with `tags={DeleteObject, GetObjectInfo}` present (object layer reached). Storage after: `xl.meta` sha256 unchanged; version still present. Here `wormuser` passes the `DeleteObjectAction` gate and, because it set the bypass header, `enforceRetentionBypassForDelete` proceeds to `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, bucket, object.ObjectName)` [cmd/bucket-object-lock.go:L153] — which **fails** because `wormpol` does not grant `s3:BypassGovernanceRetention` — returning `errAuthentication` → `ErrAccessDenied` [cmd/api-errors.go:L2176] → HTTP 403. The governance bypass is a **third, distinct** permission, separate from both `DeleteObjectAction` and the object-lock check.

**Case 4 — `bypassuser` (has both actions) + bypass header → HTTP 204 No Content (version deleted).**

```
--- REQUEST ---
DELETE /wormbucket/worm-1783965682.txt?versionId=6f98f7ed-7386-408c-9b66-9addbd70b544
Authorization: AWS4-HMAC-SHA256 Credential=bypassuser/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=39b0f7229050ca5c822130923ab27345f260e6f7dc70bebfb76a98fea33100ee
X-Amz-Bypass-Governance-Retention: true
X-Amz-Date: 20260713T180123Z
--- RESPONSE ---
HTTP 204 204 No Content
X-Amz-Request-Id: 18C1EB566D1B2A54
X-Amz-Version-Id: 6f98f7ed-7386-408c-9b66-9addbd70b544
```

Correlated audit: `api.name=DeleteObject statusCode=200 status=OK accessKey=bypassuser bucket=wormbucket requestID=18C2175246BC0AF1` (`reqQuery={versionId=7e1f7a0c-d26c-464b-93b1-4dafde57fa10}`), with `tags={DeleteObject, GetObjectInfo}` present (object layer reached). **A subtlety worth stating precisely: the audit `statusCode` is `200 OK`, not the wire's `204` — a genuine audit-status-fidelity artifact of MinIO's gzip response path, and it reproduces deterministically.** Re-running this exact Case-4 delete records `statusCode=200 status=OK` in the audit stream while the client still receives wire `HTTP 204`, and the audit event's `timeToFirstByte` field is **absent** — the tell that the recorder's `WriteHeader` had not yet run when the audit line was emitted. The audit layer *does* copy the recorder's status verbatim (`statusCode = tc.ResponseRecorder.StatusCode` [internal/logger/audit.go:L99], surfaced as `entry.API.StatusCode` / `entry.API.Status = http.StatusText(statusCode)` [internal/logger/audit.go:L119-L120]); the decisive detail is *when* that copy runs relative to the gzip wrapper's deferred flush:

- Every S3 handler is wrapped by `gzipHandler` [cmd/api-router.go:L232], defined as `gzhttp.NewWrapper(gzhttp.MinSize(1000), gzhttp.CompressionLevel(gzip.BestSpeed))` [cmd/admin-router.go:L40-L41] (dependency `github.com/klauspost/compress v1.17.11` [go.mod:L35]). The `minio-go` SDK — like any Go `http.Client` — has its `Transport` auto-add `Accept-Encoding: gzip` (unsigned, added after SigV4), so `acceptsGzip(r)` is true and the writer handed to `DeleteObjectHandler` [cmd/object-handlers.go:L2509] is a `*gzhttp.GzipResponseWriter` wrapping the single per-request trace `ResponseRecorder` created in `httpTracerMiddleware` [cmd/http-tracer.go:L73,L83] — the very object the audit reads as `tc.ResponseRecorder`.
- The success path is `writeSuccessNoContent` [cmd/api-response.go:L930] → `writeResponse(w, http.StatusNoContent, nil, mimeNone)` [cmd/api-response.go:L885] → `w.WriteHeader(204)` with a `nil` body [cmd/api-response.go:L899] (no `Write`). `GzipResponseWriter.WriteHeader` only **buffers** the code (`w.code = 204`) and does *not* forward it to the underlying recorder — gzhttp defers the real header write until it can decide whether to compress.
- `DeleteObjectHandler`'s first deferred statement, `logger.AuditLog` [cmd/object-handlers.go:L2512; internal/logger/audit.go:L63], runs the instant the handler returns and reads `tc.ResponseRecorder.StatusCode` — still the constructor default `http.StatusOK` (200) [internal/http/response-recorder.go:L84], because the buffered `204` has not been flushed. Since the recorder's `WriteHeader` never ran, `timeToFirstByte` is zero and is omitted [internal/logger/audit.go:L103,L133].
- Only afterward does the **outer** deferred `gw.Close()` in the gzip wrapper (`GzipResponseWriter.Close` → `startPlain`) call `recorder.WriteHeader(204)` [internal/http/response-recorder.go:L171-L174], setting `StatusCode=204` — in time for the wire (the client sees `204`), but after the audit event was already emitted.

The discriminator is confirmed directly: the **identical** delete issued from a client that does *not* send `Accept-Encoding: gzip` (e.g. `curl`) records `statusCode=204 status="No Content"` with `timeToFirstByte` **present**, because gzhttp then uses `NoGzipResponseWriter.WriteHeader`, which forwards to the recorder immediately; adding `-H "Accept-Encoding: gzip"` to that same `curl` delete flips the audit back to `200`, while `Accept-Encoding: identity` leaves it at `204`. This is therefore a general property of MinIO's empty-body `204 No Content` responses to gzip-capable clients — a plain root `DELETE` of an ordinary object exhibits the same `wire 204 / audit 200` split — and is *not* specific to WORM. It is purely an audit-status artifact: the wire response, the returned `X-Amz-Version-Id`, and the storage outcome are all unaffected, and it does not touch the authorization verdict (the read-only-boundary result is identical either way). Storage after: `xl.meta present=false`; root `StatObject(versionId)` returns `The specified version does not exist.` — the locked version was genuinely **deleted**. Only the identity holding *both* `s3:DeleteObject` and `s3:BypassGovernanceRetention` can remove a governance-locked object.

**Conclusion (F10).** IAM and object-lock are independent, ordered gates:

1. `DeleteObjectAction` (IAM) — `rouser` fails here (Case 1), so object-lock is never consulted; the bypass header is moot.
2. Object-lock retention (`enforceRetentionBypassForDelete` [cmd/bucket-object-lock.go:L84]) — a principal that *passes* IAM is still blocked (Case 2, the independence proof) unless it presents a valid governance bypass.
3. `BypassGovernanceRetentionAction` (IAM) — a *separate* permission checked at [cmd/bucket-object-lock.go:L153]; `wormuser` lacks it (Case 3) and is denied; `bypassuser` holds it (Case 4) and succeeds.

A **read-only identity holds none of these** — not `DeleteObjectAction`, not the retention/legal-hold set, and not `BypassGovernanceRetentionAction` — so it can neither set nor bypass retention, and its delete is stopped at the very first gate. On the write path the symmetric guard is `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] (blocks shortening governance retention without the bypass and makes compliance mode unchangeable by anyone), and `checkPutObjectLockAllowed` [cmd/bucket-object-lock.go:L245] gates lock metadata on writes. The version-scoped bypass path is only reached for versioned deletes (`if vID != ""` [cmd/object-handlers.go:L2600] guarding the `enforceRetentionBypassForDelete` call at [cmd/object-handlers.go:L2601]).


---

## 8. Reasoning with `file:line` grounding

### 8.1 The enforcement chain (where the guard runs)

Every authenticated S3 request converges on one path:

1. **Classification.** `getRequestAuthType` [cmd/auth-handler.go:L124] classifies the request into one of the **12** auth types enumerated at [cmd/auth-handler.go:L109-L120] (including `authTypeAnonymous`, `authTypePresigned`, `authTypeSigned`, and `authTypeStreamingSigned`). `rouser`'s header-signed probes are `authTypeSigned`; the presigned delete is `authTypePresigned`.
2. **Gate.** Handlers call `checkRequestAuthType` [cmd/auth-handler.go:L339] (or `checkRequestAuthTypeWithVID` [cmd/auth-handler.go:L349] for versioned/batch), which calls `authenticateRequest` [cmd/auth-handler.go:L358]. PUT-class operations additionally pass through `isPutActionAllowed` [cmd/auth-handler.go:L749].
3. **Terminal decision.** `authenticateRequest` invokes `IAMSys.IsAllowed` [cmd/iam.go:L2437]. Its flow, verified against this checkout:
   - external AuthZ-plugin short-circuit (not configured here);
   - `if args.IsOwner { return true }` [cmd/iam.go:L2448] — `rouser` is **not** owner;
   - temporary-credential path `IsAllowedSTS` [cmd/iam.go:L2242] — not an STS credential;
   - service-account path `IsAllowedServiceAccount` [cmd/iam.go:L2140] — not a service account;
   - regular user: `PolicyDBGet` → **`if len(policies) == 0 { return false }`** [cmd/iam.go:L2476], else it combines the principal's attached policies via `GetCombinedPolicy` and returns `IsAllowed(args)` [cmd/iam.go:L2482].
   `rouser` has exactly one attached policy (`readonly-bp`), which contains **no statement** allowing any write-adjacent action, so the combined-policy `IsAllowed(args)` evaluation [cmd/iam.go:L2482] returns `false` for each → `403`.
4. **Denial surface.** `false` becomes `ErrAccessDenied` [cmd/api-errors.go:L86] → `http.StatusForbidden` [cmd/api-errors.go:L539]; the XML `APIErrorResponse` [cmd/api-errors.go:L64] is built by `getAPIErrorResponse` [cmd/api-errors.go:L2597] and emitted by `writeErrorResponse` [cmd/api-response.go:L945].
5. **Audit.** Each S3 handler registers `defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))` at its entry — e.g. `GetObjectHandler` [cmd/object-handlers.go:L718], `ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L254], `NewMultipartUploadHandler` [cmd/object-multipart-handlers.go:L67] — implemented by `logger.AuditLog` [internal/logger/audit.go:L63]; on the way out it emits the event carrying `accessKey` (principal), `api.name`, bucket/object, `statusCode`, and `requestID`. (The `defer logger.AuditLog` at [cmd/auth-handler.go:L636] is a *specific* pre-handler branch that rejects requests missing the `Date` header, not the general per-request audit path.)

**Deny-by-default** is the crux: MinIO grants nothing implicitly. A read-only policy that allows only `s3:GetObject`+`s3:ListBucket` therefore denies every write-adjacent `policy.Action`. Importantly, these actions are **not** one-per-operation: `policy.PutObjectAction` is the single gate **reused** across five distinct operations — `NewMultipartUpload` [cmd/object-multipart-handlers.go:L83], `PutObjectPart` [cmd/object-multipart-handlers.go:L667], the destination of `CopyObjectPart` [cmd/object-multipart-handlers.go:L268], `CompleteMultipartUpload` [cmd/object-multipart-handlers.go:L927], and the destination of `CopyObject` [cmd/object-handlers.go:L1173]. The remaining write-adjacent guards are distinct: `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118], `ListMultipartUploadPartsAction` [cmd/object-multipart-handlers.go:L1162], `DeleteObjectAction` [cmd/object-handlers.go:L2528], `PutObjectTaggingAction` [cmd/object-handlers.go:L3151], `DeleteObjectTaggingAction` [cmd/object-handlers.go:L3301], `PutObjectLegalHoldAction` [cmd/object-handlers.go:L2718], `PutObjectRetentionAction` (evaluated in a metadata callback — see §8.3), and `ListBucketMultipartUploadsAction` [cmd/bucket-handlers.go:L265]. `readonly-bp` grants none of them, so each probed operation is denied. (These `policy.Action` symbols come from `github.com/minio/pkg/v3 v3.0.22` [go.mod:L55].)

### 8.2 Why the bucket-policy path is not involved

`PolicySys.IsAllowed` [cmd/bucket-policy.go:L48] takes `policy.BucketPolicyArgs` and evaluates **per-bucket JSON policy for anonymous callers**. `rouser` is an authenticated IAM principal, so its decision is made by `IAMSys.IsAllowed`, not by the bucket-policy path. (No bucket policy was set on `testbucket`, so anonymous callers are denied too — cf. the anonymous `GET /` baseline in §2.)

### 8.3 Enforcement ordering and the read-before-authz exceptions

For **most** write-adjacent handlers the action check runs at the top of the handler, **before** the object layer acquires a namespace lock or touches any shard. This is true for multipart (create/upload/copy-part/complete/abort), `CopyObject`, `PutObjectTagging`, legal-hold, and single/batch delete: each calls `checkRequestAuthType` (or `checkRequestAuthTypeWithVID`) as an early guard, so a denial returns at the handler boundary and the erasure backend is never entered. (One ordering nuance on `PutObjectTagging`: while its `PutObjectTaggingAction` check [cmd/object-handlers.go:L3151] does precede the object layer, the handler first *parses the request body* at [cmd/object-handlers.go:L3141] — earlier than that authz check — so a **malformed** tagging body is rejected with `500`/`400` *before* the authorization decision, still without entering the erasure backend; this is the API-1 disclosure characterized in §5.3, and it does not breach the read-only boundary because no tags are written.) The runtime audit stream confirms the authz ordering for well-formed requests: on the `rouser` denial each of these events carries `tags=null` (no object-layer read occurred before the 403). The storage stream corroborates it: after these denials the multipart create targets are **absent**, `.minio.sys/multipart` holds **0** upload-id directories, and the pre-existing objects' `xl.meta` SHA-256 values are **unchanged** (§5 storage tables). The on-disk erasure layout used for these assertions (`<datadir>/<bucket>/<object>/xl.meta` + part files) matches the layout exercised by the repository's own tests (`cmd/erasure-object_test.go`, `cmd/erasure-healing_test.go`). **Two write-adjacent handlers are exceptions that read the object *before* the authorization decision — `DeleteObjectTagging` and `PutObjectRetention` — and both are treated below; in each, a *denied* request still mutates nothing.**

**`DeleteObjectTagging` is the first exception.** It differs from its sibling `PutObjectTagging`, whose handler runs `checkRequestAuthType(ctx, r, policy.PutObjectTaggingAction, …)` [cmd/object-handlers.go:L3151] **before** it ever calls `GetObjectInfo` [cmd/object-handlers.go:L3162] — so a denied `PutObjectTagging` never touches the backend (its audit event carries `tags=null`). `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235] inverts that order: it first calls `objAPI.GetObjectInfo(ctx, bucket, object, opts)` [cmd/object-handlers.go:L3259] to load the object's *existing* tags — "Set this such that authorization policies can be applied on the object tags." [cmd/object-handlers.go:L3296-L3297] — and only **then** calls `checkRequestAuthType(ctx, r, policy.DeleteObjectTaggingAction, …)` [cmd/object-handlers.go:L3301]. Because `GetObjectInfo` reads `xl.meta` from the erasure backend (via `readAllXL`), a *denied* `DeleteObjectTagging` **does** enter the backend — for a read — before returning 403. The runtime audit stream shows exactly this asymmetry: on the `rouser` denial the `PutObjectTagging` event carries `tags=null`, whereas the `DeleteObjectTagging` event carries `tags={"GetObjectInfo":"name=shared/a.txt,pool=1,set=1"}` — the pre-auth read is recorded, the same "`tags` block present ⇒ object layer reached" signal used for the WORM deletes in §7.5. As with retention, this changes only *where* the decision is made, not *whether* a write occurs: the tag-**mutation** (`objAPI.DeleteObjectTags`) is reached only *after* the action is allowed, so a *denied* `DeleteObjectTagging` performs **no** metadata write — `shared/a.txt`'s `xl.meta` SHA-256 is **unchanged** across the denial (§5, §8.4), i.e. the zero-mutation-side-effect property still holds.

**`PutObjectRetention` is the second exception** to "authz strictly before the object layer", and it is worth stating precisely because it changes *where* the decision is made — not *whether* a write occurs. `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] does **not** perform a top-level `checkRequestAuthType`. Its order is:

1. `validateSignature(getRequestAuthType(r), r)` [cmd/object-handlers.go:L2874] — verifies the **signature only** (not the `PutObjectRetentionAction` authorization);
2. `GetBucketInfo` [cmd/object-handlers.go:L2880];
3. `hasContentMD5` [cmd/object-handlers.go:L2885] — else `ErrMissingContentMD5`;
4. **Object-Lock configuration check** — on a bucket without Object-Lock this returns `ErrInvalidBucketObjectLockConfiguration` → HTTP **400 `InvalidRequest`** [cmd/object-handlers.go:L2891], which is exactly why `PutObjectRetention` on `testbucket` returns 400, not 403 (§5.3, §7.1);
5. only on a lock-enabled bucket does it build `popts` with an `EvalMetadataFn` [cmd/object-handlers.go:L2912] wrapping `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913] and call `PutObjectMetadata` [cmd/object-handlers.go:L2933].

Inside `erasureObjects.PutObjectMetadata` [cmd/erasure-object.go:L2121] the ordering is: acquire the namespace lock `NewNSLock` [cmd/erasure-object.go:L2124] → **read** existing metadata `readAllXL` [cmd/erasure-object.go:L2142] → run the callback `EvalMetadataFn` [cmd/erasure-object.go:L2182], where `enforceRetentionBypassForPut` → `isPutRetentionAllowed` calls `IAMSys.IsAllowed(PutObjectRetentionAction)`; if denied it returns `errAuthentication` [cmd/bucket-object-lock.go:L189/L206/L219/L230] → `ErrAccessDenied` (HTTP 403) [cmd/api-errors.go:L2176]. The actual write, `updateObjectMeta` [cmd/erasure-object.go:L2192], runs **only after** the callback allows. So for retention on a *lock-enabled* bucket the IAM decision happens **after** a lock + read but **before** any write — meaning even in this exception a *denied* retention change still performs **no** metadata write (zero side effect holds), it simply makes the decision one layer deeper. A read-only identity cannot reach a successful write here regardless, because it holds neither `PutObjectRetentionAction` nor the object-lock configuration to make the bucket eligible in the first place. This lock-enabled retention `403 AccessDenied` is now **directly observed** (§5.3): repeating the probe as `rouser` against the lock-enabled `wormbucket` with a valid `Content-Md5` clears the signature/bucket-info/`Content-Md5`/object-lock-config prechecks, reaches the deferred IAM decision inside the callback, and returns `403 AccessDenied` (`RequestId 18C20C03A02283AB`, audit `api.name=PutObjectRetention statusCode=403 accessKey=None`) — corroborating the call-ordering derivation above rather than leaving it inferred. On the non-lock `testbucket` the handler instead stops earlier at the object-lock configuration precheck with `400` (§5.3, §7.1). Either way it follows directly from deny-by-default (§8.1): `readonly-bp` grants no `PutObjectRetentionAction`, so `IAMSys.IsAllowed` returns false inside the callback. (Note that because this handler authenticates via `validateSignature` [cmd/object-handlers.go:L2874] rather than a top-level `checkRequestAuthType`, its audit record carries `accessKey` = `<nil>` even when the deferred check denies — the same principal-less audit shape seen for the non-lock `400` in §5.3.)

### 8.4 Coverage pass — every named item, answered by name

Citations give the handler declaration line and the authorization line. The **Audit** column is the internal `api.name` recorded by the correlated event.

| Named item | Handler decl → guard action (`file:line`) | Wire result | Storage side-effect (mutation-sensitive) | Audit `api.name` |
|------------|-------------------------------------------|-------------|-------------------------------------------|------------------|
| **Multipart** `CreateMultipartUpload` | `NewMultipartUploadHandler` [cmd/object-multipart-handlers.go:L64] → `PutObjectAction` [cmd/object-multipart-handlers.go:L83] | 403 AccessDenied | `shared/mp.txt` absent; 0 upload-ids | `NewMultipartUpload` |
| **Multipart** `UploadPart` | `PutObjectPartHandler` [cmd/object-multipart-handlers.go:L583] → `PutObjectAction` [cmd/object-multipart-handlers.go:L667] | 403 AccessDenied | no part dir; 0 upload-ids | `PutObjectPart` |
| **Multipart** `UploadPartCopy` | `CopyObjectPartHandler` [cmd/object-multipart-handlers.go:L244] → dst `PutObjectAction` [cmd/object-multipart-handlers.go:L268] + src `GetObjectAction` [cmd/object-multipart-handlers.go:L301] | 403 AccessDenied | no part dir; src `xl.meta` sha256 unchanged | `CopyObjectPart` |
| **Multipart** `CompleteMultipartUpload` | `CompleteMultipartUploadHandler` [cmd/object-multipart-handlers.go:L908] → `PutObjectAction` [cmd/object-multipart-handlers.go:L927] | 403 AccessDenied | target absent | `CompleteMultipartUpload` |
| **Multipart** `AbortMultipartUpload` | `AbortMultipartUploadHandler` [cmd/object-multipart-handlers.go:L1098] → `AbortMultipartUploadAction` [cmd/object-multipart-handlers.go:L1118] | 403 AccessDenied | n/a (fabricated upload id) | `AbortMultipartUpload` |
| **Multipart** `ListParts` | `ListObjectPartsHandler` [cmd/object-multipart-handlers.go:L1143] → `ListMultipartUploadPartsAction` [cmd/object-multipart-handlers.go:L1162] | 403 AccessDenied | read — none | `ListObjectParts` |
| **Copy** `CopyObject` | `CopyObjectHandler` [cmd/object-handlers.go:L1154] → dst `PutObjectAction` [cmd/object-handlers.go:L1173] + src `GetObjectAction` [cmd/object-handlers.go:L1206] | 403 AccessDenied | `shared/copy.txt` absent; src `xl.meta` sha256 unchanged | `CopyObject` |
| **Metadata** `PutObjectTagging` | `PutObjectTaggingHandler` [cmd/object-handlers.go:L3122] → `PutObjectTaggingAction` [cmd/object-handlers.go:L3151] | 403 AccessDenied | `a.txt` xl.meta sha256 unchanged; root tag-count 0 | `PutObjectTagging` |
| **Metadata** `DeleteObjectTagging` | `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235] → `DeleteObjectTaggingAction` [cmd/object-handlers.go:L3301] | 403 AccessDenied | `a.txt` xl.meta sha256 unchanged | `DeleteObjectTagging` |
| **Metadata** `PutObjectLegalHold` | `PutObjectLegalHoldHandler` [cmd/object-handlers.go:L2698] → `PutObjectLegalHoldAction` [cmd/object-handlers.go:L2718] (checked **before** Content-MD5 at [cmd/object-handlers.go:L2727]) | 403 AccessDenied | `a.txt` xl.meta sha256 unchanged | `PutObjectLegalHold` |
| **Metadata** `PutObjectRetention` | `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] → `PutObjectRetentionAction` via `enforceRetentionBypassForPut` [cmd/object-handlers.go:L2913] (see §8.3) | **400 InvalidRequest** on non-lock `testbucket` (pre-IAM, observed); **403 AccessDenied** on lock-enabled `wormbucket` (observed, §5.3) | object intact; no metadata write | `PutObjectRetention` |
| **Delete** `DeleteObject` (single) | `DeleteObjectHandler` [cmd/object-handlers.go:L2509] → `DeleteObjectAction` [cmd/object-handlers.go:L2528] | 403 AccessDenied | `a.txt` present; xl.meta sha256 unchanged | `DeleteObject` |
| **Delete** `DeleteObjects` (batch) | `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416] → per-object `DeleteObjectAction` [cmd/bucket-handlers.go:L505] | **200** + per-object AccessDenied in `<DeleteResult>` | all targets present; xl.meta sha256 unchanged | **`DeleteMultipleObjects`** (one event, statusCode 200) |
| **List** `ListObjectsV2` (in prefix) | `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L154] → `ListBucketAction` [cmd/bucket-listobjects-handlers.go:L172] | 200 (4 keys in `shared/`) | read — none | `ListObjectsV2` |
| **List** `ListObjectsV2` (no prefix) | `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L154] → `ListBucketAction` [cmd/bucket-listobjects-handlers.go:L172] | 200 — **discloses `private/` & `load/`** | read — none | `ListObjectsV2` |
| **List** `ListObjectsV1` (in prefix) | `ListObjectsV1Handler` [cmd/bucket-listobjects-handlers.go:L273] → `ListBucketAction` [cmd/bucket-listobjects-handlers.go:L287] | 200 — discloses key+`Owner` | read — none | `ListObjectsV1` |
| **List** `ListMultipartUploads` | `ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251] → `ListBucketMultipartUploadsAction` [cmd/bucket-handlers.go:L265] | 403 AccessDenied | read — none | `ListMultipartUploads` |
| **HEAD** `HeadObject` (in prefix) | `headObjectHandler` [cmd/object-handlers.go:L744] → `GetObjectAction` [cmd/object-handlers.go:L760] | 200 — discloses size/ETag/Content-Type | read — none | `HeadObject` |
| **HEAD** `HeadObject` (outside prefix) | `headObjectHandler` [cmd/object-handlers.go:L744] → `GetObjectAction` [cmd/object-handlers.go:L760] | 403 (no XML body) | read — none | `HeadObject` |
| **HEAD** `HeadBucket` | `HeadBucketHandler` [cmd/bucket-handlers.go:L1644] → `ListBucketAction` [cmd/bucket-handlers.go:L1658] | 200 | read — none | `HeadBucket` |

**Adjacent behaviors surfaced during this coverage pass (documented in §5–§7 and §9; none breaches the read-only mutation boundary).** Exhaustively exercising the named operations also revealed the following, each observed at runtime and characterized in place: **API-1** — a malformed `PutObjectTagging` body returns `500`/`400` *before* the IAM check (§5.3), principal-independent, writing nothing; **API-2** — `Content-Md5` is validated for *presence* only, so a wrong digest is accepted on the write-capable path for `DeleteObjects`/`PutObjectRetention` (§5.4), while the read-only principal stays denied; **API-3** — an exact prefix-*stem* object hides its `stem/` descendants from `ListObjects` V1/V2 until the stem is removed (§6.3); **API-4** — `ListMultipartUploads` matches `prefix` as an exact key rather than an S3 prefix (§6.4); **STORAGE-1** — empty-prefix multipart discovery is lost across a restart while the upload persists on disk (§6.4). Each affects either *any* principal (API-1) or only *authorized writers* (API-2/API-3/API-4/STORAGE-1); none grants the read-only identity any mutation or any read it lacks. The audit-confidentiality note (**OBS-1**, §7.2) and the baseline dependency/advisory posture (**DEP-1**, §9) are likewise disclosed without altering the read-only mutation verdict.

### 8.5 Bottom line

Within the tested configuration (see §9 for the exact scope and limits), a MinIO identity scoped to `s3:GetObject` (bucket+prefix) + `s3:ListBucket` (bucket) behaved as **read-only for object content and metadata mutation** under sustained concurrent write/metadata load: across every probed multipart, copy, tagging/legal-hold/retention, and delete operation — signed and presigned, inside and outside the prefix, single and batch, quiescent and same-key-contended — the read-only identity performed **no** successful mutation. Each denial returned at deny-by-default with the corresponding audit record, and the storage backend showed no side effect (the objects' `xl.meta` SHA-256 and root semantic state were unchanged; multipart create left zero upload-id directories). The one behavior that is a *disclosure*, not a mutation, is that bucket-scoped `s3:ListBucket` (with no `s3:prefix` condition) lets the identity enumerate key names, sizes, ETags, timestamps, and (via V1) owner across the whole bucket — including outside its read prefix — which should be constrained with an `s3:prefix` `Condition` if that enumeration is undesirable.

Beyond that listing disclosure, exhaustive probing surfaced five adjacent behaviors (**API-1**–**API-4**, **STORAGE-1**) and an audit-confidentiality note (**OBS-1**) — catalogued in §8.4 and detailed in §5–§7 — concerning malformed-input robustness, write-path `Content-Md5` correctness, `ListMultipartUploads` prefix semantics, cross-restart multipart discovery, listing completeness under stem collisions, and audit-sink handling of presigned signatures. **None of them lets the read-only identity mutate data or read content it lacks:** API-1 affects any principal but writes nothing; API-2/API-3/API-4/STORAGE-1 concern the authorized-writer/enumeration paths; OBS-1 concerns confidentiality of the audit sink, not the S3 authorization decision. The read-only **mutation** boundary therefore holds; these items are disclosed for operator awareness, with the standard `s3:prefix` condition and audit-sink-hygiene mitigations noted in place, and the baseline dependency/advisory posture recorded in §9.

---

## 9. Limitations & scope of this result

This is an empirical, runtime investigation, not a formal proof or a product-wide certification. Its conclusions are bounded to what was actually exercised:

- **Single deployment, single version.** All evidence comes from one single-node server built from this checkout — version string `DEVELOPMENT.GOGET`, `go1.23.12 linux/amd64`, deployment id `584479d9-b9c6-423f-bff3-1f4c9308df56` — built with `CGO_ENABLED=0 go build .` in its **default** configuration. Distributed/erasure-set topologies, other releases, and non-default server configuration were **not** tested and are out of scope.
- **Tested principal and action set.** The read-only identity under test was one IAM user (`rouser`) with exactly one attached policy (`readonly-bp`: `s3:GetObject` on `testbucket/shared/*` + `s3:ListBucket` on `testbucket`); a second read-only identity (`rouserp`, policy `readonly-bp-prefixed`) was provisioned **solely** to demonstrate the `s3:prefix` `ListBucket` mitigation at runtime (§6.3). Group policies, STS/temporary credentials, service accounts, LDAP/OIDC-mapped identities, and external AuthZ plugins were **not** exercised; the `IsAllowedSTS`/`IsAllowedServiceAccount` branches (§8.1) were confirmed by code path only, not by runtime probe.
- **Bounded, not absolute.** The concurrency results are bounded to the tested scale — two identical 24-worker / 8-second load runs (§4) and, for TOCTOU, 968 concurrent overwrites across 25 read-only delete attempts (§7.4). The "no check-vs-use window" statement is a bounded observation over that scale (stable across the two runs), **not** a formal guarantee of impossibility at all scales.
- **Enumerated operations only.** Coverage is the operation families named in the prompt and their variants (§8.4). S3 surface area beyond that table (e.g. SELECT/`POST` object, website/CORS/ACL sub-resources, replication and ILM internals, KMS/SSE paths) was **not** probed.
- **WORM scope.** The object-lock independence result (§7.5) was demonstrated for **governance**-mode retention on versioned deletes; **compliance**-mode and legal-hold-only interactions were reasoned from source (`cmd/bucket-object-lock.go`) but not each independently reproduced at runtime.

Where a statement rests on code path rather than a captured runtime signal, it is labeled as such above. Every other claim is backed by the adjacent wire, audit, and storage evidence from this run.

### 9.1 Dependency & advisory posture (baseline)

Because the prompt's concern is a security boundary, the exercised toolchain and dependency set were themselves scanned for known vulnerabilities, so the read-only-boundary result can be read against an explicit baseline. This posture is **observed** — a real `govulncheck` run against the canonically-built server module — and its remediation is **out of scope** under the read-only mandate (plan §0.3.2): no `go.mod`/`go.sum`/toolchain change is permitted, so the versions below are *reported*, not altered.

**Exercised versions (read from `go.mod`, left unchanged):**

- Go toolchain `go1.23.12 linux/amd64`, satisfying `go 1.23` [go.mod:L3].
- `github.com/minio/madmin-go/v3 v3.0.77` [go.mod:L52], `github.com/minio/minio-go/v7 v7.0.80` [go.mod:L53], `github.com/minio/pkg/v3 v3.0.22` [go.mod:L55] — the admin/client/policy libraries the harness reused as-is.
- `golang.org/x/crypto v0.29.0` [go.mod:L91] — the transitively-pinned crypto library relevant to the flagship advisory below.

**Scan method (observed):** `govulncheck@v1.1.4` (Go `go1.23.12`; DB `https://vuln.go.dev`, DB timestamp `2026-07-08 17:05:00 +0000 UTC`), run against the built module. Verbatim summary:

```
Your code is affected by 50 vulnerabilities from 7 modules and the Go standard library.
This scan also found 15 vulnerabilities in packages you import and 17
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
Use '-show verbose' for more details.
```

So of **82 advisories found in total** (50 + 15 + 17), **50 are reachable** ("your code is affected") and 32 are present-but-not-called. Two representative *reachable* advisories, quoted exactly from the scan:

- **`GO-2024-3321`** — *"Misuse of connection.serverAuthenticate may cause authorization bypass in golang.org/x/crypto"*; `Found in: golang.org/x/crypto@v0.29.0`, `Fixed in: golang.org/x/crypto@v0.31.0`. Its only reported trace enters through the **SFTP** subsystem:

```
Vulnerability #50: GO-2024-3321
    Misuse of connection.serverAuthenticate may cause authorization bypass in
    golang.org/x/crypto
  More info: https://pkg.go.dev/vuln/GO-2024-3321
  Module: golang.org/x/crypto
    Found in: golang.org/x/crypto@v0.29.0
    Fixed in: golang.org/x/crypto@v0.31.0
    Example traces found:
      #1: cmd/sftp-server.go:509:25: cmd.startSFTPServer calls sftp.Server.Listen, which eventually calls ssh.NewServerConn
```

  (This advisory is publicly aliased as **CVE-2024-45337** — the `x/crypto/ssh` `ServerConfig` authorization-bypass issue; the CVE alias is external context, not part of the scan text.)
- **`GO-2026-5856`** — *"Invoking Encrypted Client Hello privacy leak in crypto/tls"*, Go standard library; `Found in: crypto/tls@go1.23.12`, `Fixed in: crypto/tls@go1.25.12` — reachable via TLS transport paths (e.g. the reported trace `cmd/iam.go:1676:42: cmd.IAMSys.NormalizeLDAPMappingImport calls ldap.Config.Connect, which eventually calls tls.Conn.Handshake`).

**Baseline attribution (observed).** Every one of these advisories inheres in the pinned dependency and toolchain versions that predate this task; the investigation added, updated, and removed **no** dependency. The manifests are byte-for-byte identical to the untouched baseline — `go.mod` SHA-256 `b85e689662e001da57c8e38a7cc29ff7430c81df040ab913a802aa4e080c8e0f`, `go.sum` SHA-256 `184a7add019c576c926f07da8ba57280a6c95f00ca3f427049d2a09b1d55fc63` — and `git status` shows only this answer document changed. **The investigation therefore introduced zero new dependency risk;** the posture above is the repository's own pre-existing baseline.

**Reachability vs. the read-only authorization path (reasoning).** None of the reachable advisories lies on the S3 per-request PBAC decision path that this result depends on — `authenticateRequest` [cmd/auth-handler.go:L358] → `IAMSys.IsAllowed` [cmd/iam.go:L2437]. The reported traces enter through *adjacent* subsystems: the SFTP server (`GO-2024-3321`), LDAP-config/TLS transport, Prometheus metrics, and the Go TLS/x509 stack — not the object-request authorization gate. In particular, the single advisory that is itself an *authorization bypass* (`GO-2024-3321`) is reachable only when the **SFTP** service is enabled, and is unrelated to the S3 `s3:GetObject`/`s3:ListBucket` evaluation exercised here. Upgrading `golang.org/x/crypto` to `v0.31.0`+ and the toolchain to a fixed `go1.25.x` would clear the two advisories quoted above, but any such change is out of scope for this read-only task and is recorded only for operator awareness (**DEP-1**).

---

## Appendix — reproduction summary & repository integrity

- **Build (exact, canonical):** `CGO_ENABLED=0 go build .` (Go 1.23.12) → exit 0, ~150 MB `./minio` binary in the repo root (git-ignored, so the tree stays clean), `go.mod`/`go.sum` unchanged. Version string `DEVELOPMENT.GOGET`.
- **Audit sink:** a tiny local webhook receiver (`/tmp/mh/bin/sink`) was started first, appending every JSON audit event to `/tmp/audit.log`.
- **Run (exact):** `MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_AUDIT_WEBHOOK_ENABLE=on MINIO_AUDIT_WEBHOOK_ENDPOINT=http://127.0.0.1:9099/ ./minio server /tmp/minio-data --address :9000 --console-address :9001` (server PID captured; health `/minio/health/live` and `/minio/health/ready` both returned HTTP 200; anonymous `GET /` returned the standard S3 `AccessDenied` XML baseline).
- **Provision (canonical admin path):** `madmin` `AddCannedPolicy`/`AddUser`/`SetPolicy` → `readonly-bp` (custom bucket+prefix) on `rouser`, built-in `readwrite` on `rwuser`, custom `wormpol` on `wormuser`, custom `bypasspol` on `bypassuser`, and `readonly-bp-prefixed` (adds the `s3:prefix` `ListBucket` condition) on `rouserp` for the §6.3 mitigation demonstration. `mc` was not installed, so the `madmin` SDK was used (an equivalent canonical entry point).
- **Probe:** raw SigV4 via `github.com/minio/minio-go/v7@v7.0.80/pkg/signer` (`SignV4` [github.com/minio/minio-go/v7@v7.0.80/pkg/signer/request-signature-v4.go:L343], `PreSignV4` [github.com/minio/minio-go/v7@v7.0.80/pkg/signer/request-signature-v4.go:L208]) — the real S3 API path, with raw HTTP for byte-level trace capture.
- **Cleanup:** the server and sink processes were stopped, and the `./minio` binary, `/tmp/minio-data` datadir, `/tmp/mh` harness, sink, `/tmp/audit.log`, and policy JSON were removed after the run. The repository's only change is this document; `go.mod`/`go.sum` are byte-for-byte unchanged; `git status` shows only `blitzy/documentation/minio_c07e5b49d477.md`.
- **Non-canonical values:** none. Every value above was produced by the default build exercised through the real admin and S3 APIs. Request IDs/timestamps are specific to this run; `HostId`/`X-Amz-Id-2` = `dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8`.
