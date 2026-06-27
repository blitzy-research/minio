# MinIO Read-Only Authorization Boundary — Security Investigation

**Subject:** Can a principal *intended* to be **read-only** on a specific bucket/prefix nonetheless **mutate data** through less-obvious "write-adjacent" S3 surface area — especially under **concurrent write load** (a time-of-check-to-time-of-use / TOCTOU race window)?

**Repository:** `github.com/minio/minio`
**Branch / commit under test:** `minio_c07e5b49d477` @ `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Method:** Source-code audit (code is the source of truth) **paired with** runtime experimentation against a server built from this exact checkout. Every system claim carries an inline `[<path>:<locator>]` citation; every behavioral claim is backed by a captured HTTP/S3 trace **and** a storage side-effect check.

---

## 1. TL;DR — Verdict

**The read-only boundary HOLDS. No bypass hides in the corners, and no TOCTOU window was found.**

Across **47 distinct probes** plus a **2,804-attempt concurrent TOCTOU storm**, a read-only identity could **not** mutate a single byte, object, tag, version, ACL, retention setting, or multipart part. Every mutation-capable or mutation-adjacent operation returned a true authorization denial (**HTTP 403 `AccessDenied`**, or per-object `AccessDenied` for bulk delete), and **every denial was corroborated by a byte-for-byte storage side-effect check** showing the backend identical to a recorded baseline (normalized manifest hash `6ea533261c5e5fb9bbac77778ecd9f8e`).

The reason is structural, not incidental: under a **deny-by-default** model, **every write-adjacent handler gates its durable mutation behind a successful `IAMSys.IsAllowed` decision [cmd/iam.go:L2437]**. In the common case that check runs in the auth handler before the object layer is entered at all; in the two handlers that perform a non-mutating pre-auth read (`DeleteObjectTagging`) or authorize inside a metadata callback (`PutObjectRetention`), the decision still precedes any namespace-locked write/rename or metadata commit (the precise three-category audit is in Section 6). A denied request therefore never reaches the durable mutation, so there is no code path — and hence no race window — by which it can produce a side effect.

Two precise nuances refine (and strengthen) this verdict and are documented in full below:
1. **`GetObjectAttributes` is *denied* for the canned read-only user** even though that user can fully `GetObject` the same object — because the handler requires `s3:GetObjectAttributes` **AND** `s3:GetObject` (logical AND), not an OR-fallback [cmd/object-handlers.go:L593-L596].
2. **A `400` is not a `403`.** Bulk `DeleteObjects` and `PutObjectRetention` are gated by a `Content-MD5` request-validation check that runs *before* authorization; a `400 MissingContentMD5` is a validation rejection, **not** an authorization result. The harness explicitly sends a valid `Content-MD5` to push past that gate and observe the true authorization decision (which is still denial).

---

## 2. The Question, Restated

A principal is provisioned to be "read-only" on a bucket/prefix. The worry is not the obvious `PutObject`/`DeleteObject` — those are clearly writes. The worry is the **write-adjacent surface area**: multipart sub-operations, copy-style writes (which are PUTs in disguise), metadata mutations (tagging, ACL, retention, legal-hold), bulk delete, and historically-vulnerable "corners" (presigned URLs, POST-policy form uploads, reserved-bucket targeting). And the sharpest worry is whether **contention** — many concurrent legitimate writers hammering the same bucket — could open a **TOCTOU** window where a check passes/fails inconsistently with the use.

This investigation answers that question empirically and from code truth.

---

## 3. Scope & Method

**In scope (probed from the read-only identity):**
- **Multipart:** `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListMultipartUploads`, `ListParts`
- **Copy writes:** `CopyObject` (PUT with `x-amz-copy-source`), copy-with-metadata-`REPLACE`
- **Metadata mutations:** `PutObjectTagging`, `DeleteObjectTagging`, `PutObjectAcl`, `PutObjectRetention`, `PutObjectLegalHold`, `GetObjectAttributes`
- **Deletes:** `DeleteObject`, `DeleteObjects` (bulk), `DeleteObjectTagging`
- **Information disclosure without a GET:** `ListObjectsV1`, `ListObjectsV2`, `ListObjectVersions`, `HeadObject`, `HeadBucket` — under **both** read-only policy variants
- **Corners:** presigned-PUT URL, POST-policy (browser form) upload, reserved/system-bucket targeting
- **Concurrent-load / TOCTOU loop:** read-only probes repeated while legitimate writers churn the same bucket/prefix

**Method:** For every probe we capture **both** (a) the request/response trace — HTTP status code and S3 error code — **and** (b) the storage side effect, by diffing the backend object inventory (via an authorized "oracle" identity) and inspecting the on-disk data directory (`xl.meta`) against a recorded baseline. Each runtime outcome is then explained by the exact handler → IAM action → authorization call that produced it.

**Non-destructive to the repository:** No MinIO source file was modified. The only artifact created is this document. The build, the server, the data directory, the `mc` client, the policy JSON, and all probe scripts lived outside the working tree (under `/tmp`) and were deleted afterward. The MinIO **source tree is unchanged** relative to the commit under investigation, `c07e5b49d477b0774f23db3b290745aef8c01bd2`: the working tree is clean (`git status --porcelain` empty), and the only difference anywhere in the repository is the addition of this single documentation file — `git diff --name-status c07e5b49d…HEAD` reports exactly `A blitzy/documentation/minio_c07e5b49d477.md`. The destination-branch `HEAD` itself is **not** that commit; it is the documentation commit carrying this deliverable, layered on top of `c07e5b49d477b0774f23db3b290745aef8c01bd2` (which remains its ancestor, per `git merge-base`).

---

## 4. Environment & Reproduction Steps

**Toolchain (all outside the repository tree):**

| Tool | Version | Role |
|------|---------|------|
| Go | `go1.23.12 linux/amd64` | Build the pinned binary (satisfies `go.mod` `go 1.23` [go.mod:L3]; CI targets `1.23.x`) |
| MinIO (built) | `DEVELOPMENT.GOGET (go1.23.12 linux/amd64)` | Server under test, from commit `c07e5b49d` |
| `mc` (MinIO Client) | `RELEASE.2025-08-13T08-35-41Z` | Admin-API provisioning (admin ops require SigV2/SigV4 [cmd/auth-handler.go:L189]) |
| boto3 / botocore | `1.43.36` | SigV4 S3 probe client |
| Python | `3.12` | Harness runtime |

**Step 1 — Build (outside the repo), following the Makefile `build:` recipe [Makefile:L179]:**
```
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-investigation/minio_bin .
```
(The Makefile adds a `--ldflags` version stamp; omitting it yields the `DEVELOPMENT.GOGET` version string — the compiled source is commit `c07e5b49d` regardless.)

**Step 2 — Run a single node** on an ephemeral data dir with known root credentials; confirm health:
```
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin123 MINIO_BROWSER=off \
  /tmp/minio-investigation/minio_bin server /tmp/minio-investigation/data --address 127.0.0.1:9000
curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/minio/health/live   # -> 200
```
A single node fully exercises the authorization decision because authorization is decided **before** the erasure/data-plane layer; quorum mechanics do not change the deny path. The backend stores objects in `xl.meta` format (single-node, single-drive).

**Step 3 — Provision identities/policies via `mc`** (workflow per [docs/multi-user/README.md:L44,L50,L56]: `mc admin policy create` → `mc admin user add` → `mc admin policy attach`):
- `writer / writer12345` → canned **`readwrite`** (load generator **and** the authorized inventory "oracle" used for side-effect verification)
- `rocanned / rocanned12345` → canned **`readonly`** — **Variant A**
- `roprefix / roprefix12345` → custom prefix-scoped policy — **Variant B**

**Step 4 — Seed baseline state and record inventory** (keys, sizes, ETags, version IDs):
- `testbucket` (versioning **enabled**): `readable/f1.txt` (50 B, ETag `0219f6ab…`, tagged `team=research&class=public`), `readable/f2.txt` (51 B), `secret/secret.txt` (27 B)
- `lockbucket` (object-lock **enabled** + versioned): `locked.txt` (19 B)
- On-disk baseline: normalized manifest of all object `xl.meta` files → combined hash **`6ea533261c5e5fb9bbac77778ecd9f8e`**

---

## 5. The Two Read-Only Policy Definitions

The two variants are tested because they yield **different answers to the disclosure question**.

### Variant A — Stock canned `readonly`
Confirmed at runtime (server-reported) and in the vendored source [github.com/minio/pkg/v3@v3.0.22/policy/constants.go:L53-L60]:
```json
{ "Statement": [ { "Effect": "Allow",
  "Action": ["s3:GetBucketLocation", "s3:GetObject"],
  "Resource": ["arn:aws:s3:::*"] } ] }
```
**Key property:** it grants `GetObject` + `GetBucketLocation` only and **excludes `s3:ListBucket`**. A stock-readonly user can GET/HEAD an object **if it already knows the key**, on **any** bucket/prefix, but **cannot enumerate** a bucket.

### Variant B — Custom prefix-scoped read-only
```json
{ "Version": "2012-10-17", "Statement": [
  { "Sid": "AllowGetBucketLocation", "Effect": "Allow",
    "Action": ["s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::testbucket"] },
  { "Sid": "AllowListReadablePrefix", "Effect": "Allow",
    "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::testbucket"],
    "Condition": { "StringLike": { "s3:prefix": ["readable/*"] } } },
  { "Sid": "AllowGetReadablePrefix", "Effect": "Allow",
    "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::testbucket/readable/*"] }
] }
```
**Critical policy shape:** the `s3:prefix` condition is attached to **`s3:ListBucket` only** (statement 2), `GetBucketLocation` carries **no** condition (statement 1), and `GetObject` is scoped to the object ARN `testbucket/readable/*` (statement 3). MinIO accepted this shape at provisioning time; attaching an `s3:prefix` condition to `GetBucketLocation` is an unsupported condition-key combination. This is the bucket-ARN-vs-object-ARN distinction that S3 least-privilege design requires.

---

## 6. Authorization Model (Code Audit)

Every S3 data-plane handler routes through the `s3APIMiddleware` chain [cmd/api-router.go:L210] and reaches one of a small set of authorization entry points in `cmd/auth-handler.go`, all of which converge on `IAMSys.IsAllowed` [cmd/iam.go:L2437]:

- `checkRequestAuthType` [cmd/auth-handler.go:L339] / `checkRequestAuthTypeWithVID` [cmd/auth-handler.go:L349] → `authorizeRequest` [cmd/auth-handler.go:L419]
- `isPutActionAllowed` [cmd/auth-handler.go:L749] — the PUT-family helper (PutObject, UploadPart)
- `isPutRetentionAllowed` [cmd/auth-handler.go:L704] — calls `IsAllowed(BypassGovernanceRetentionAction)` [cmd/auth-handler.go:L717-L720] and `IsAllowed(PutObjectRetentionAction)` [cmd/auth-handler.go:L728-L731]
- Admin path: `validateAdminSignature` [cmd/auth-handler.go:L159] / `checkAdminRequestAuth` [cmd/auth-handler.go:L189] (SigV2/SigV4 only — why `mc`/madmin is required to provision)

`IsAllowed` [cmd/iam.go:L2437] applies a **deny-by-default** dispatch: external authz plugin (OPA/webhook) → owner/root unconditional allow → STS (`IsAllowedSTS` [cmd/iam.go:L2242]) → service account (`IsAllowedServiceAccount` [cmd/iam.go:L2140]) → regular-user combined-policy evaluation → **fallback deny**. For a static IAM user with a read-only policy, any action not explicitly allowed (and not matching the resource/condition) falls through to the final deny.

### Why a denial has zero side effect (the TOCTOU-relevant invariant)

```
Read-Only Client (SigV4)
   │  write-adjacent request
   ▼
Middleware chain ─ setAuthMiddleware (classify auth type, validate skew)   [cmd/routers.go, cmd/generic-handlers.go]
   ▼
Auth Handler ─ checkRequestAuthType / isPutActionAllowed                    [cmd/auth-handler.go]
   ▼
IAMSys.IsAllowed(action, resource, conditions)                              [cmd/iam.go:L2437]
   ├── DENY  ─► handler returns 403 AccessDenied; the durable storage mutation (namespace lock + temp write + atomic rename) is never reached
   └── ALLOW ─► proceed ─► Object/Multipart Handler ─► Erasure Engine
                           (namespace lock, write to .minio.sys/tmp, write-quorum, atomic rename)
```

### The three handler categories (where authorization sits relative to the object layer)

**The precise invariant (stated exactly, not over-simplified).** The security-relevant guarantee is **not** the blanket claim that "the object layer is never touched on a denial" — that is too strong, because two of the probed handlers legitimately make a non-mutating object-layer call, or run their authorization check inside an object-layer callback, before the decision is final. The exact, source-accurate invariant is narrower: **in every write-adjacent handler the durable storage *mutation* is gated behind a successful `IsAllowed` decision; the only object-layer calls that may execute *before* that decision are non-mutating reads or request validation.** Auditing every probed handler against the source, they fall into three categories — and in all three a denial leaves storage byte-for-byte unchanged (verified in Sections 8–9):

- **Category 1 — authorize before any object-layer call (the common case).** The handler calls `checkRequestAuthType`/`isPutActionAllowed` → `IsAllowed` first; on denial it returns immediately, never constructing an object-layer request. This covers `PutObject` [cmd/object-handlers.go:L1836], `UploadPart`, `CopyObject` [cmd/object-handlers.go:L1173,L1206], the multipart family [cmd/object-multipart-handlers.go:L83,L268,L927,L1118,L1162], `DeleteObject` [cmd/object-handlers.go:L2528], the per-object check in bulk delete [cmd/bucket-handlers.go:L505], `PutObjectTagging` (new tags are parsed from the **request body** [cmd/object-handlers.go:L3141,L3148], so authorization at [cmd/object-handlers.go:L3151] precedes the `GetObjectInfo` at [cmd/object-handlers.go:L3162]), `PutObjectLegalHold` (authz at [cmd/object-handlers.go:L2718] precedes `GetBucketInfo` at [cmd/object-handlers.go:L2723]), ACL, and all listing/HEAD handlers.

- **Category 2 — a safe, non-mutating pre-auth read; the mutation is strictly post-auth.** `DeleteObjectTaggingHandler` deliberately calls `objAPI.GetObjectInfo` [cmd/object-handlers.go:L3259] **before** `checkRequestAuthType(DeleteObjectTaggingAction)` [cmd/object-handlers.go:L3301], specifically to load the object's *existing* tags into the `X-Amz-Tagging` header [cmd/object-handlers.go:L3297] so tag-conditioned policies can be evaluated. That pre-auth call is a **read**; the mutation `objAPI.DeleteObjectTags` [cmd/object-handlers.go:L3313] runs only after authorization passes. (Contrast `PutObjectTagging`, whose new tags arrive in the request body, so it authorizes first — the asymmetry is by design.) A denied request therefore performs a harmless read and returns `403` with the tags intact — confirmed by the side-effect check (Section 8).

- **Category 3 — authorization evaluated *inside* the object-layer metadata callback, ahead of the durable write.** `PutObjectRetentionHandler` validates the signature [cmd/object-handlers.go:L2874], reads bucket info [cmd/object-handlers.go:L2880], enforces the `Content-MD5` and lock-enabled gates, parses the retention body [cmd/object-handlers.go:L2895], then calls `PutObjectMetadata` [cmd/object-handlers.go:L2933] passing an `EvalMetadataFn` callback [cmd/object-handlers.go:L2912] that performs the `PutObjectRetentionAction` authorization via `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] → `isPutRetentionAllowed` [cmd/auth-handler.go:L704] → `IsAllowed`. The erasure layer invokes this callback **before** committing any change: `PutObjectMetadata` evaluates `EvalMetadataFn` and, on its error, returns `ObjectInfo{}, err` at [cmd/erasure-object.go:L2181-L2183] — *before* `updateObjectMeta` performs the durable on-disk write at [cmd/erasure-object.go:L2192]. So even though the authorization is embedded in the object layer, a denial aborts ahead of the write and produces no side effect — confirmed empirically: the valid-`Content-MD5` retention probe returned `403` with the object's `xl.meta` unchanged.

Across all three categories the conclusion is identical and is grounded in the **actual call ordering** rather than a blanket assertion: **a denied write-adjacent request never reaches the durable mutation, so there is no execution path — and hence no race window — by which it can change storage.** The concurrent-load experiment (Section 9) validates this empirically under sustained same-prefix contention.

---

## 7. Per-Operation Evidence Table (Variant A — canned `readonly`)

Legend: **HTTP/S3** is the captured trace; **Side effect** is the storage check; verdict **DENY** = true authorization denial, **ALLOW** = permitted (and, for the read operations, *expected* under read-only).

| # | Operation | IAM action checked | Handler `[file:line]` | HTTP / S3 code | Side effect | Verdict |
|---|-----------|--------------------|-----------------------|----------------|-------------|---------|
| 1 | GetObject (sanity) | `s3:GetObject` | `getObjectHandler` | **200** | read only | ALLOW (expected) |
| 2 | CreateMultipartUpload | `s3:PutObject` | `NewMultipartUploadHandler` [cmd/object-multipart-handlers.go:L64,L83] | **403** `AccessDenied` | none | DENY |
| 3 | UploadPart | `s3:PutObject` (`isPutActionAllowed`) | `PutObjectPartHandler` [cmd/object-multipart-handlers.go:L583] | **403** `AccessDenied` | none | DENY |
| 4 | UploadPartCopy | `s3:PutObject`(dst)+`s3:GetObject`(src) | `CopyObjectPartHandler` [cmd/object-multipart-handlers.go:L244,L268,L301] | **403** `AccessDenied` | none | DENY |
| 5 | CompleteMultipartUpload | `s3:PutObject` | `CompleteMultipartUploadHandler` [cmd/object-multipart-handlers.go:L908,L927] | **403** `AccessDenied` | none | DENY |
| 6 | AbortMultipartUpload | `s3:AbortMultipartUpload` | `AbortMultipartUploadHandler` [cmd/object-multipart-handlers.go:L1098,L1118] | **403** `AccessDenied` | none | DENY |
| 7 | ListMultipartUploads | `s3:ListBucketMultipartUploads` | `ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251,L265] | **403** `AccessDenied` | none | DENY |
| 8 | ListParts | `s3:ListMultipartUploadParts` | `ListObjectPartsHandler` [cmd/object-multipart-handlers.go:L1143,L1162] | **403** `AccessDenied` | none | DENY |
| 9 | CopyObject | `s3:PutObject`(dst)+`s3:GetObject`(src) | `CopyObjectHandler` [cmd/object-handlers.go:L1154,L1173,L1206] | **403** `AccessDenied` | none | DENY |
| 10 | CopyObject (metadata `REPLACE`) | `s3:PutObject`(dst)+`s3:GetObject`(src) | `CopyObjectHandler` [cmd/object-handlers.go:L1154] | **403** `AccessDenied` | none (target `f1.txt` ETag unchanged) | DENY |
| 11 | PutObjectTagging | `s3:PutObjectTagging` | `PutObjectTaggingHandler` [cmd/object-handlers.go:L3122,L3151] | **403** `AccessDenied` | none (tags unchanged) | DENY |
| 12 | DeleteObjectTagging | `s3:DeleteObjectTagging` | `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235,L3301] | **403** `AccessDenied` | none (tags persist) | DENY |
| 13 | PutObjectAcl | `s3:PutBucketPolicy` | `PutObjectACLHandler` [cmd/acl-handlers.go:L172,L193] | **403** `AccessDenied` | none | DENY |
| 14 | PutObjectLegalHold | `s3:PutObjectLegalHold` | `PutObjectLegalHoldHandler` [cmd/object-handlers.go:L2698,L2718] | **403** `AccessDenied` | none | DENY |
| 15 | PutObjectRetention (no MD5) | (request-validation gate) | `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855,L2885-L2887] | **400** `MissingContentMD5` | none | validation reject (pre-authz) |
| 16 | PutObjectRetention (valid MD5) | `s3:PutObjectRetention` | `isPutRetentionAllowed` [cmd/auth-handler.go:L704,L728-L731] | **403** `AccessDenied` | none | DENY (true authz) |
| 17 | GetObjectAttributes | `s3:GetObjectAttributes` **AND** `s3:GetObject` | `getObjectAttributesHandler` [cmd/object-handlers.go:L593-L596] | **403** `AccessDenied` | read only | DENY (see §12.1) |
| 18 | DeleteObject | `s3:DeleteObject` | `DeleteObjectHandler` [cmd/object-handlers.go:L2509,L2528] | **403** `AccessDenied` | none | DENY |
| 19 | DeleteObjects bulk (no MD5) | (request-validation gate) | `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416,L433] | **400** `MissingContentMD5` | none | validation reject (pre-authz) |
| 20 | DeleteObjects bulk (valid MD5) | `s3:DeleteObject` per-object | per-object auth [cmd/bucket-handlers.go:L505] | **200**, per-object `AccessDenied`, **0 deleted** | none | DENY (true authz) |

**Reconciliation with the table (20 data rows).** The rows partition exactly into three outcome classes: **17 authorization denials** — every mutation-capable or mutation-adjacent operation was denied at authorization, returning **403 `AccessDenied`** (or, for bulk `DeleteObjects` with a valid MD5, an **HTTP 200** envelope carrying **per-object `AccessDenied` with 0 deleted**, row 20); **2 pre-authorization validation rejections** — `PutObjectRetention` (row 15) and bulk `DeleteObjects` (row 19), each returning **400 `MissingContentMD5`** because it was sent without a `Content-MD5`, a request-validation result that never reached authorization; and **1 allowed read control** — `GetObject` (row 1), permitted as expected for read-only. The two *reads* in the matrix deliberately do **not** behave identically: `GetObject` is allowed, but **`GetObjectAttributes` (row 17) is denied** — and is counted among the 17 denials — because it requires the distinct `s3:GetObjectAttributes` action (logical AND with `s3:GetObject`; see §12.1), so being "read-only" does not make it succeed. (17 + 2 + 1 = 20.) Every "none" in the side-effect column was verified — see Section 8.

---

## 8. Storage Side-Effect Verification (mandatory, not inferred)

After each probe wave the backend was checked two independent ways:

1. **On-disk (authoritative byte-level):** re-snapshot every object `xl.meta` under the data directory, normalize paths, and compare md5 + a combined manifest hash against the baseline. After the full static probe matrix (Sections 7, 10, 11): **`CURRENT == BASELINE == 6ea533261c5e5fb9bbac77778ecd9f8e`**, manifest diff **empty (4/4 objects byte-for-byte identical)**, only the 4 baseline object directories present, **0** pending multipart upload directories, and **all** would-be probe artifacts (`readable/ro-copy.txt`, `readable/ro-copy2.txt`, `readable/ro-mpu.txt`, `readable/ro-presign.txt`, `secret/ro-presign2.txt`, `readable/ro-postpolicy.txt`) **absent**.

2. **S3-layer (oracle corroboration):** listing via the authorized `writer` identity reported exactly the 3 seeded `testbucket` objects (same sizes/ETags), **3 versions, 0 delete markers**, and `readable/f1.txt` tags still `{class=public, team=research}` — confirming the denied `PutObjectTagging`/`DeleteObjectTagging`/`CopyObject(REPLACE)`/`DeleteObject` probes changed nothing.

**Conclusion:** every denial truly had no effect; the 403 responses were not masking partial writes.

> *Note — the baseline hash is run-specific.* The manifest hash `6ea533261c5e5fb9bbac77778ecd9f8e` is computed over the seeded objects' `xl.meta`, which embed per-run version UUIDs and timestamps, so it is unique to this provisioning run; an earlier run of this investigation produced a different baseline hash (`62b7fdb620a4ec26dd4b3b8bb7195eb6`). What is run-invariant — and what this side-effect check actually establishes — is `CURRENT == BASELINE` after every probe wave. See the provenance note in Section 9.

---

## 9. Concurrent-Load / TOCTOU Results

**Setup:** 6 legitimate **writer** threads (identity `writer`, canned `readwrite`) churned the **`readable/churn/`** prefix of `testbucket` — deliberately the **same `readable/` prefix the read-only identity is scoped to read**, so the contention lands on the exact key-space under test (not a disjoint `churn/` bucket). Each writer iteration exercised the full spread of write **and** metadata traffic the AAP calls for, against that shared prefix:
- `PutObject` (new object `readable/churn/w*`),
- **`CopyObject`** — a copy-style write with source `readable/f1.txt` → `readable/churn/copy-*` (this is the copy traffic the prior version of this report was missing),
- `PutObjectTagging` (metadata mutation on a churn key),
- **`HeadObject`** (stat) and **`ListObjectsV2`** with `prefix=readable/` (the list/stat traffic the prior version was missing),
- `DeleteObject`, and periodic `CreateMultipartUpload`/`AbortMultipartUpload`.

Simultaneously, 6 **read-only** (`rocanned`) threads raced four mutation classes each iteration, **all targeting the same `readable/` prefix**:
- `PutObject` of a **uniquely-named canary** `readable/RO-CANARY-<uuid>.txt` (a definitive positive breach test — only the read-only identity ever attempts these names),
- a raw SigV4 **presigned-style PUT** `readable/RO-PRESIGN-*`,
- `DeleteObject` of a **live churn key** a writer had just created (the sharpest TOCTOU race — the read-only delete and the writer's lifecycle collide on the identical key),
- `CreateMultipartUpload` `readable/RO-MPU-*`.

**Outcome (9.13 s wall-clock):**

| Metric | Value |
|--------|-------|
| Concurrent legitimate writer ops (across 8 classes) | **3,264** |
| Read-only mutation attempts (across 4 classes) | **2,804** |
| Read-only **successes** (HTTP 2xx) | **0** |
| Read-only `403 AccessDenied` | **2,804** |
| **Breaches** (any canary/part/delete that took effect) | **0 (empty list)** |

**Writer traffic by class** — confirms the contention spans writes *and* the copy + list/stat metadata surface required by the AAP, all on the shared `readable/` prefix:

| Writer op | Count |
|-----------|------:|
| `PutObject` | 490 |
| `CopyObject` (copy-style write) | 490 |
| `PutObjectTagging` | 490 |
| `HeadObject` (stat) | 490 |
| `ListObjectsV2` (`prefix=readable/`) | 490 |
| `DeleteObject` | 490 |
| `CreateMultipartUpload` | 162 |
| `AbortMultipartUpload` | 162 |
| **Total** | **3,264** |

**Read-only attempt accounting — every attempt classified, the totals close exactly.** Each read-only attempt is counted into exactly one mutually-exclusive outcome bucket (`403 AccessDenied`, 2xx breach, 4xx validation reject, 5xx, or SDK/transport error), so the per-class row and the per-outcome columns both sum to the grand total with **nothing left unaccounted**:

| Read-only class | Attempts | `403 AccessDenied` | 2xx breach | 4xx validation | 5xx | SDK/transport error |
|-----------------|---------:|-------------------:|-----------:|---------------:|----:|--------------------:|
| `PutObject` canary `readable/RO-CANARY-*` | 701 | 701 | 0 | 0 | 0 | 0 |
| presigned-PUT `readable/RO-PRESIGN-*` | 701 | 701 | 0 | 0 | 0 | 0 |
| `CreateMultipartUpload` `readable/RO-MPU-*` | 701 | 701 | 0 | 0 | 0 | 0 |
| race-`DeleteObject` of a live `readable/churn/*` key | 701 | 701 | 0 | 0 | 0 | 0 |
| **Total** | **2,804** | **2,804** | **0** | **0** | **0** | **0** |

The accounting closes with **zero residual**: 2,804 attempts = 2,804 `403` + 0 + 0 + 0 + 0. There were **no** non-`403` outcomes — no validation rejections, timeouts, cancellations, SDK/transport errors, or non-S3 responses — so there is no unexplained gap between attempts and denials. (Every read-only mutation class is gated at authorization *before* any object-existence or request-shape check, so even a `DeleteObject` racing a just-deleted key returns `403`, not `404`.)

**Post-storm side-effect check:** **zero** `RO-CANARY*`/`RO-PRESIGN*`/`RO-MPU*` artifacts anywhere on disk and **zero** read-only-created keys in the oracle inventory; the 4 baseline objects byte-for-byte identical. After purging the writers' *legitimate* churn (1,470 object versions and delete-markers, via the authorized oracle — including delete-marker versions, see note) and confirming 0 pending multipart uploads, the data directory returned **exactly** to the baseline hash `6ea533261c5e5fb9bbac77778ecd9f8e` with an empty diff.

**Interpretation:** Under sustained **same-prefix** contention, **no TOCTOU window exists**. This is the empirical confirmation of the code invariant from Section 6: because every write-adjacent handler gates its durable mutation behind the `IsAllowed` decision — whether checked before the object layer (Category 1), after a safe non-mutating pre-auth read (Category 2), or inside the pre-write metadata callback (Category 3) — concurrent writers cannot create a moment in which a denied read-only request slips through to storage.

**Provenance of the absolute figures in Sections 8–9 (run-specificity).** The concurrent-load counts and the normalized manifest hash reported above are artifacts of **this specific, authoritative run** and are **not** run-invariant. An **earlier, narrower run** of this same investigation — preserved in this deliverable's initial commit — recorded a **1,780**-attempt read-only storm against **2,502** legitimate writer ops, of which **1,778** were observed as `403`, with baseline manifest hash **`62b7fdb620a4ec26dd4b3b8bb7195eb6`**. That run was deliberately **superseded** by the run documented above, which is more comprehensive: it adds copy-style writes (`CopyObject`) and list/stat metadata traffic (`HeadObject`, `ListObjectsV2`) to the writer churn, targets the **same `readable/` prefix** the read-only identity is scoped to (rather than a disjoint `churn/` prefix), and **closes the attempt accounting exactly** (2,804 attempts = 2,804 `403`, **0 residual** — eliminating the earlier run's small 1,780-vs-1,778 gap). Two properties make these absolute numbers inherently non-reproducible across runs while leaving the security conclusion fully reproducible: (1) the attempt and writer totals are produced by a **time-bounded** contention window (≈9.13 s wall-clock here), so they scale with run duration and host speed; and (2) the normalized manifest hash is taken over each object's `xl.meta`, which embeds **per-run version UUIDs and timestamps**, so every fresh provisioning yields a distinct baseline hash. The **run-invariant result** — identical in both runs and the sole basis of the verdict — is: **0 read-only successes, 0 breaches, and `CURRENT == BASELINE` after cleanup.**

> *Secondary observation (versioning semantics, not a security finding):* on a versioned bucket, the writers' `DeleteObject` calls created delete markers, so prior-version `xl.meta` persisted on disk until an explicit `--versions` purge. This is documented MinIO behavior [docs/bucket/versioning/README.md] and is orthogonal to the authorization question.

---

## 10. Information Disclosure Without a GET (both variants)

| Operation | IAM action | Handler `[file:line]` | Variant A (canned) | Variant B (prefix `readable/*`) |
|-----------|------------|-----------------------|--------------------|---------------------------------|
| ListObjectsV2 (no prefix) | `s3:ListBucket` | [cmd/bucket-listobjects-handlers.go:L154,L172] | **403** | **403** (condition not satisfied) |
| ListObjectsV2 (`prefix=readable/`) | `s3:ListBucket` + `s3:prefix` | [cmd/bucket-listobjects-handlers.go:L172] | **403** | **200** (lists `readable/*`) |
| ListObjectsV2 (`prefix=secret/`) | `s3:ListBucket` + `s3:prefix` | [cmd/bucket-listobjects-handlers.go:L172] | **403** | **403** (prefix outside grant) |
| ListObjectsV1 | `s3:ListBucket` | [cmd/bucket-listobjects-handlers.go:L273,L287] | **403** | **403** (no prefix) |
| ListObjectVersions (no prefix) | `s3:ListBucketVersions` (primary), `s3:ListBucket` (fallback) | authz [cmd/bucket-listobjects-handlers.go:L87]; fallback [cmd/auth-handler.go:L495-L509] | **403** | **403** |
| ListObjectVersions (`prefix=readable/`) | `s3:ListBucketVersions` (primary), `s3:ListBucket` + `s3:prefix` (fallback) | authz [cmd/bucket-listobjects-handlers.go:L87]; fallback [cmd/auth-handler.go:L495-L509] | **403** | **200** (via `ListBucket` fallback) |
| HeadBucket | `s3:ListBucket` | [cmd/bucket-handlers.go:L1644,L1658] | **403** | **403** (HEAD sends no prefix) |
| HeadObject (`readable/f1`) | `s3:GetObject` | `headObjectHandler` | **200** | **200** |
| HeadObject (`secret/secret`) | `s3:GetObject` | `headObjectHandler` | **200** | **403** (GetObject scoped to `readable/*`) |
| GetBucketLocation | `s3:GetBucketLocation` | [cmd/bucket-handlers.go:L204,L218] | **200** | **200** |

**What this means:**
- **Variant A (canned `readonly`) cannot enumerate at all** — every `List*` is `403`, because the canned policy has no `s3:ListBucket` (matching the intentional omission in [github.com/minio/pkg/v3@v3.0.22/policy/constants.go:L53-L60]). But it **can** `HeadObject`/`GetObject` **any** key it knows across **all** prefixes (including `secret/`), since its `GetObject` is granted on `arn:aws:s3:::*`.
- **Variant B confines both listing and head/get to the `readable/` prefix.** `secret/` is fully invisible: it can neither be listed nor head-ed. This is the correct least-privilege shape.
- **`ListObjectVersions` authorizes against `s3:ListBucketVersions` first**, not `s3:ListBucket`. `ListObjectVersionsHandler` checks `policy.ListBucketVersionsAction` [cmd/bucket-listobjects-handlers.go:L87] (constant `"s3:ListBucketVersions"` [github.com/minio/pkg/v3@v3.0.22/policy/action.go:L84]); if that is not granted, `authorizeRequest` **falls back to `s3:ListBucket`** [cmd/auth-handler.go:L495-L509] (with an equivalent anonymous-path fallback at [cmd/auth-handler.go:L447-L460]), because MinIO treats the two as equivalent ("s3:ListBucket permission is same as s3:ListBucketVersions"). Neither read-only policy grants `s3:ListBucketVersions`, so the observed `ListObjectVersions` results are produced **entirely by the `s3:ListBucket` fallback**: Variant A has no `ListBucket` at all → `403`; Variant B grants `ListBucket` only under the `s3:prefix=readable/*` condition, so the no-prefix call fails the condition (`403`) while `prefix=readable/` satisfies it (`200`). This is why the disclosure answer for versions tracks the plain-`ListBucket` answer exactly.

**Metadata observable without reading object bytes:**
- `ListObjectsV2` (Variant B) exposes per object: **Key, Size, ETag, LastModified, StorageClass, Owner** (DisplayName + canonical ID).
- `HeadObject` exposes more: **Content-Length, ETag, LastModified, Content-Type, VersionId** (versioning on), **StorageClass, user metadata**.
- Neither surface returns object **bytes**; reading bytes requires `s3:GetObject`, which is a *granted read*, not a mutation.

Listing is therefore a genuine, separately-gated privilege — not implied by read access.

---

## 11. The Corners (historically-known read-only bypasses) — all CLOSED

History flagged two read-only bypass families: **CVE-2021-21362** (presigned upload-URL bypass, fixed in `RELEASE.2021-03-04`) and **PR #16849** (post-policy reserved-bucket bypass). This checkout post-dates both; we verified closure rather than assuming it.

| Corner | Vector | HTTP / S3 code | Verdict |
|--------|--------|----------------|---------|
| Control: presigned-**GET** signed by read-only id | presigned-GET | **200** | works (proves presigning + signing are correct; isolates that later denials are *authorization*) |
| presigned-**PUT** URL signed by read-only id (`readable/`) | presigned-PUT | **403** `AccessDenied` | CVE-2021-21362 **closed** |
| presigned-**PUT** into `secret/` | presigned-PUT | **403** `AccessDenied` | closed |
| **POST-policy** (browser form) upload signed by read-only id | POST-policy | **403** `AccessDenied` | PR #16849 family **closed** |
| `PutObject` into reserved `.minio.sys` (read-only id) | reserved-bucket | **403** `AllAccessDisabled` | blocked |
| `PutObject` into reserved `.minio.sys` (**readwrite** `writer`) | reserved-bucket | **403** `AllAccessDisabled` | blocked for **all** identities |
| presigned-PUT into `.minio.sys` (read-only id) | reserved-bucket | **403** `AllAccessDisabled` | blocked |

**Rationale:** a presigned URL or a POST-policy form merely encodes the *signer's* identity into the request signature — it does **not** bypass IAM; the same `IsAllowed(PutObjectAction)` gate applies. Reserved-bucket writes are rejected by a **separate, identity-independent** guard (note the distinct `AllAccessDisabled` code, not `AccessDenied`) — even the `readwrite` `writer` is blocked from `.minio.sys`. No corner yields a write for the read-only identity.

---

## 12. Two Code-Truth Nuances (documented precisely)

### 12.1 `GetObjectAttributes` requires `GetObjectAttributes` **AND** `GetObject` (not an OR-fallback)
```go
// cmd/object-handlers.go  (getObjectAttributesHandler)
s3Error = checkRequestAuthType(ctx, r, policy.GetObjectAttributesAction, bucket, object)  // L593
if s3Error == ErrNone {                                                                    // L594
    s3Error = checkRequestAuthType(ctx, r, policy.GetObjectAction, bucket, object)         // L595
}
```
Line 595 (the `GetObject` check) executes **only if** the `GetObjectAttributesAction` check already passed (`s3Error == ErrNone`). This is a logical **AND**, not a fallback. The canned `readonly` policy grants `GetObject` but **not** `GetObjectAttributes` [github.com/minio/pkg/v3@v3.0.22/policy/constants.go:L60], so L593 denies and the call returns **403** — **even though the same user can fully `GetObject` the object's bytes, size, and ETag.** A least-privilege subtlety worth knowing: granting "read" via `s3:GetObject` does **not** implicitly grant the dedicated attributes API. (The versioned branch [cmd/object-handlers.go:L588-L591] similarly requires `GetObjectVersionAttributes` AND `GetObjectVersion`.)

### 12.2 A `400` validation rejection is not a `403` authorization denial
- **Bulk `DeleteObjects`** checks for the **`Content-Md5` request header's presence first** [cmd/bucket-handlers.go:L432-L433]; absent → `400 MissingContentMD5`, **before** authorization runs. (The gate is header-presence only — a checksum trailer does not substitute at this point.) With a valid `Content-MD5`, the request clears the gate and reaches **per-object** authorization [cmd/bucket-handlers.go:L505], which denies each object and **continues** (records a per-object `AccessDenied` rather than failing the whole request) — yielding **HTTP 200 with per-object `AccessDenied` and zero `<Deleted>` entries**. The bucket-level check at [cmd/bucket-handlers.go:L471] intentionally ignores its error (it only populates the access-key for logging); the authoritative decision is the per-object one.
- **`PutObjectRetention`** likewise requires `Content-MD5` [cmd/object-handlers.go:L2885-L2887] and an object-lock-enabled bucket *before* authorization; on a lock-enabled bucket with a valid `Content-MD5` it parses the retention body [cmd/object-handlers.go:L2895] and calls `PutObjectMetadata` with an `EvalMetadataFn` callback [cmd/object-handlers.go:L2912] that performs the authorization via `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] → `isPutRetentionAllowed` [cmd/auth-handler.go:L704] → `IsAllowed(PutObjectRetentionAction)`. That callback is evaluated **before** the durable metadata write [cmd/erasure-object.go:L2181-L2192], so the denial returns **403** with no side effect (verified — the valid-`Content-MD5` retention probe left `locked.txt`'s `xl.meta` unchanged).

The harness sent valid `Content-MD5` values precisely so the **authorization** decision (still denial) could be observed, and reported both the `400` and the `403`/per-object cases so the two are never conflated. Object-lock retention/legal-hold WORM semantics are themselves enforced independently of IAM [cmd/bucket-object-lock.go:L167].

---

## 13. Adjacent Surfaces (awareness only — not exercised)

The **primary identity under test is a static IAM user**. Research surfaced a later (post-checkout) MinIO CVE concerning **STS / service-account *session-policy*** privilege escalation. MinIO dispatches those identity classes through dedicated evaluators — `IsAllowedSTS` [cmd/iam.go:L2242] and `IsAllowedServiceAccount` [cmd/iam.go:L2140] — *before* the regular-user path in `IsAllowed`. If the read-only principal were instead an STS token or a service account carrying an inline session policy, that session-policy intersection would be the surface to scrutinize. That is a **follow-on consideration**, explicitly outside this static-user investigation, and is noted so the verdict's scope is precise.

---

## 14. Final Verdict & Rationale

**The read-only boundary HOLDS at commit `c07e5b49d`. A principal intended to be read-only on a bucket/prefix cannot mutate data through any probed write-adjacent surface — multipart, copy, tagging, ACL, retention, legal-hold, single or bulk delete — nor through presigned-URL, POST-policy, or reserved-bucket corners, and not under concurrent write contention.**

**Why (rationale, grounded in code + evidence):**
1. **Deny-by-default at a single chokepoint.** Every write-adjacent handler routes through `s3APIMiddleware` [cmd/api-router.go:L210] to an auth entry point that calls `IsAllowed` [cmd/iam.go:L2437], which denies anything not explicitly granted. The read-only policies grant no mutating action, so all mutations are denied. *(Evidence: every write-adjacent probe denied — direct `403 AccessDenied` for the mutation APIs; the two `Content-MD5`-gated paths return `400` pre-auth and `403` once well-formed; bulk delete returns a `200` envelope with per-object `AccessDenied` and 0 deleted.)*
2. **The durable mutation is always gated behind authorization.** In the common case the `IsAllowed` decision is made before the object layer is reached at all; in the two nuanced handlers a non-mutating pre-auth read (`DeleteObjectTagging` [cmd/object-handlers.go:L3259]) or an authorization check embedded in the metadata callback (`PutObjectRetention`, whose `EvalMetadataFn` is evaluated before `updateObjectMeta` [cmd/erasure-object.go:L2181-L2192]) still precedes the durable write (see the three-category audit in Section 6). In all cases a denial aborts before any namespace-locked temp-write/rename or metadata commit. *(Evidence: every denial corroborated by a byte-for-byte unchanged backend.)*
3. **No TOCTOU window.** Because there is no code path from "denied" to the durable mutation, concurrency cannot manufacture one. *(Evidence: 2,804 read-only mutation attempts against 3,264 concurrent legitimate writes on the same `readable/` prefix → 0 successes, 0 breaches, every attempt accounted for as a `403`, baseline restored exactly.)*
4. **The corners are closed and the gates are honest.** Presigned/POST-policy requests are still IAM-checked; reserved buckets are blocked for all identities by a separate guard; and request-validation `400`s are correctly distinguished from authorization `403`s.

**Caveats bounding the verdict:** findings pertain to this exact checkout (`c07e5b49d`) and the pinned policy package `github.com/minio/pkg/v3 v3.0.22`. The identity under test is a **static IAM user**; STS/service-account **session policies** are an adjacent surface noted for awareness (Section 13) but not exercised here. The disclosure surface differs by policy: the canned `readonly` role permits `HeadObject`/`GetObject` on *known keys across all prefixes* (no listing), whereas a properly prefix-scoped policy confines both listing and read to the intended prefix — a configuration choice, not a vulnerability.

**Bottom line:** No bypass hides in the corners. The read-only boundary is enforced structurally, verified empirically, and stable under load.

---

## Appendix A — Reproduction Harness (transient, deleted after run)

All under `/tmp/minio-investigation/` (outside the repo): `minio_bin` (built server), `data/` (ephemeral backend), `mc` + `mc-config/`, `roprefix-policy.json`, and Python probe scripts (`probe_lib.py`, `probe_mutations.py`, `probe_raw.py`, `probe_disclosure.py`, `probe_corners.py`, `toctou.py`) plus the side-effect verifier (`verify_sideeffects.sh`) and the inventory oracle (`inventory_oracle.py`). Raw SigV4 probes use botocore's `S3SigV4Auth` + `URLLib3Session` so the exact signed headers (including `x-amz-content-sha256` and `Content-MD5`) reach the server. All artifacts were removed at teardown and `git status --porcelain` confirmed clean.

## Appendix B — Verified Citation Map (handler → IAM action → authorization call)

| Operation | Handler `[file:line]` | IAM action `[locator]` | Auth call |
|-----------|-----------------------|------------------------|-----------|
| PutObject | `PutObjectHandler` [cmd/object-handlers.go:L1745] | `PutObjectAction` [cmd/object-handlers.go:L1836] | `isPutActionAllowed` [cmd/auth-handler.go:L749] |
| CopyObject | `CopyObjectHandler` [cmd/object-handlers.go:L1154] | `PutObjectAction` [L1173] + `GetObjectAction` [L1206] | `checkRequestAuthType` [cmd/auth-handler.go:L339] |
| DeleteObject | `DeleteObjectHandler` [cmd/object-handlers.go:L2509] | `DeleteObjectAction` [L2528] | `checkRequestAuthType` |
| DeleteObjects (bulk) | `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416] | `DeleteObjectAction` per-object [L505] | Content-MD5 gate [L433] → `checkRequestAuthTypeWithVID` [cmd/auth-handler.go:L349] |
| PutObjectTagging | `PutObjectTaggingHandler` [cmd/object-handlers.go:L3122] | `PutObjectTaggingAction` [L3151] | `checkRequestAuthType` |
| DeleteObjectTagging | `DeleteObjectTaggingHandler` [cmd/object-handlers.go:L3235] | `DeleteObjectTaggingAction` [L3301] | `checkRequestAuthType` |
| PutObjectLegalHold | `PutObjectLegalHoldHandler` [cmd/object-handlers.go:L2698] | `PutObjectLegalHoldAction` [L2718] | `checkRequestAuthType` |
| PutObjectRetention | `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] | `PutObjectRetentionAction` [cmd/auth-handler.go:L728-L731] | `EvalMetadataFn` [cmd/object-handlers.go:L2912] → `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] → `isPutRetentionAllowed` [cmd/auth-handler.go:L704] (callback gated before durable write [cmd/erasure-object.go:L2181-L2192]) |
| GetObjectAttributes | `getObjectAttributesHandler` [cmd/object-handlers.go:L580] | `GetObjectAttributesAction` AND `GetObjectAction` [L593-L596] | `checkRequestAuthType` |
| NewMultipartUpload | `NewMultipartUploadHandler` [cmd/object-multipart-handlers.go:L64] | `PutObjectAction` [L83] | `checkRequestAuthType` |
| CopyObjectPart | `CopyObjectPartHandler` [cmd/object-multipart-handlers.go:L244] | `PutObjectAction` [L268] + `GetObjectAction` [L301] | `checkRequestAuthType` |
| PutObjectPart | `PutObjectPartHandler` [cmd/object-multipart-handlers.go:L583] | `PutObjectAction` | `isPutActionAllowed` |
| CompleteMultipartUpload | `CompleteMultipartUploadHandler` [cmd/object-multipart-handlers.go:L908] | `PutObjectAction` [L927] | `checkRequestAuthType` |
| AbortMultipartUpload | `AbortMultipartUploadHandler` [cmd/object-multipart-handlers.go:L1098] | `AbortMultipartUploadAction` [L1118] | `checkRequestAuthType` |
| ListParts | `ListObjectPartsHandler` [cmd/object-multipart-handlers.go:L1143] | `ListMultipartUploadPartsAction` [L1162] | `checkRequestAuthType` |
| ListMultipartUploads | `ListMultipartUploadsHandler` [cmd/bucket-handlers.go:L251] | `ListBucketMultipartUploadsAction` [L265] | `checkRequestAuthType` |
| GetBucketLocation | `GetBucketLocationHandler` [cmd/bucket-handlers.go:L204] | `GetBucketLocationAction` [L218] | `checkRequestAuthType` |
| HeadBucket | `HeadBucketHandler` [cmd/bucket-handlers.go:L1644] | `ListBucketAction` [L1658] | `checkRequestAuthType` |
| ListObjects v1 / v2 | `ListObjectsV1Handler` [cmd/bucket-listobjects-handlers.go:L273] / `ListObjectsV2Handler` [cmd/bucket-listobjects-handlers.go:L154] | `ListBucketAction` [L287 / L172] | `checkRequestAuthType` |
| ListObjectVersions | `ListObjectVersionsHandler` [cmd/bucket-listobjects-handlers.go:L62] | `ListBucketVersionsAction` (authz [cmd/bucket-listobjects-handlers.go:L87]); fallback `ListBucketAction` [cmd/auth-handler.go:L495-L509] | `checkRequestAuthType` |
| PutObjectACL / PutBucketACL | [cmd/acl-handlers.go:L172 / L61] | `PutBucketPolicyAction` [L193 / L77] | `checkRequestAuthType` |
| Object-lock (WORM) enforcement | `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] | (evaluated independently of IAM) | — |
| Authorization engine | — | `IAMSys.IsAllowed` [cmd/iam.go:L2437]; `IsAllowedSTS` [L2242]; `IsAllowedServiceAccount` [L2140] | deny-by-default |

*Investigation performed against `github.com/minio/minio` @ `c07e5b49d477b0774f23db3b290745aef8c01bd2`. Repository unmodified; this document is the sole artifact.*
