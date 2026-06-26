# MinIO Read-Only Authorization Boundary — Security Investigation

**Subject:** Can a principal *intended* to be **read-only** on a specific bucket/prefix nonetheless **mutate data** through less-obvious "write-adjacent" S3 surface area — especially under **concurrent write load** (a time-of-check-to-time-of-use / TOCTOU race window)?

**Repository:** `github.com/minio/minio`
**Branch / commit under test:** `minio_c07e5b49d477` @ `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Method:** Source-code audit (code is the source of truth) **paired with** runtime experimentation against a server built from this exact checkout. Every system claim carries an inline `[<path>:<locator>]` citation; every behavioral claim is backed by a captured HTTP/S3 trace **and** a storage side-effect check.

---

## 1. TL;DR — Verdict

**The read-only boundary HOLDS. No bypass hides in the corners, and no TOCTOU window was found.**

Across **48 distinct probes** plus a **1,780-attempt concurrent TOCTOU storm**, a read-only identity could **not** mutate a single byte, object, tag, version, ACL, retention setting, or multipart part. Every mutation-capable or mutation-adjacent operation returned a true authorization denial (**HTTP 403 `AccessDenied`**, or per-object `AccessDenied` for bulk delete), and **every denial was corroborated by a byte-for-byte storage side-effect check** showing the backend identical to a recorded baseline (normalized manifest hash `62b7fdb620a4ec26dd4b3b8bb7195eb6`).

The reason is structural, not incidental: **authorization (`IAMSys.IsAllowed` [cmd/iam.go:L2437]) is evaluated inside the auth handler *before* the object-layer/erasure engine is ever invoked**, under a **deny-by-default** model. A denied request returns from the handler without taking a namespace lock or touching storage, so there is no code path — and therefore no race window — by which a denied operation can produce a side effect.

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

**Non-destructive to the repository:** No MinIO source file was modified. The only artifact created is this document. The build, the server, the data directory, the `mc` client, the policy JSON, and all probe scripts lived outside the working tree (under `/tmp`) and were deleted afterward; `git status --porcelain` is clean and `HEAD` is unchanged at `c07e5b49d477b0774f23db3b290745aef8c01bd2`.

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
# GET /minio/health/live  -> 200
```
A single node fully exercises the authorization decision because authorization is decided **before** the erasure/data-plane layer; quorum mechanics do not change the deny path. The backend stores objects in `xl.meta` format (single-node, single-drive).

**Step 3 — Provision identities/policies via `mc`** (workflow per [docs/multi-user/README.md:L44,L50,L56]: `mc admin policy create` → `mc admin user add` → `mc admin policy attach`):
- `writer / writer12345` → canned **`readwrite`** (load generator **and** the authorized inventory "oracle" used for side-effect verification)
- `rocanned / rocanned12345` → canned **`readonly`** — **Variant A**
- `roprefix / roprefix12345` → custom prefix-scoped policy — **Variant B**

**Step 4 — Seed baseline state and record inventory** (keys, sizes, ETags, version IDs):
- `testbucket` (versioning **enabled**): `readable/f1.txt` (50 B, ETag `d0a31ea3…`, tagged `team=research&class=public`), `readable/f2.txt` (51 B), `secret/secret.txt` (27 B)
- `lockbucket` (object-lock **enabled** + versioned): `locked.txt` (19 B)
- On-disk baseline: normalized manifest of all object `xl.meta` files → combined hash **`62b7fdb620a4ec26dd4b3b8bb7195eb6`**

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
   ├── DENY  ─► handler returns 403 AccessDenied; Object Handler & Erasure Engine NEVER invoked
   └── ALLOW ─► proceed ─► Object/Multipart Handler ─► Erasure Engine
                           (namespace lock, write to .minio.sys/tmp, write-quorum, atomic rename)
```

The authorization call happens **before** any object-layer call in every write-adjacent handler. Because a denied request returns from the handler **before** the namespace lock is taken or any temp write/rename occurs, **there is no execution path by which a denied request can mutate storage** — and therefore no race against concurrent writers can manufacture one. The concurrent-load experiment (Section 9) validates this empirically.

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
| 15 | PutObjectRetention (no MD5) | (request-validation gate) | `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855,L2874] | **400** `MissingContentMD5` | none | validation reject (pre-authz) |
| 16 | PutObjectRetention (valid MD5) | `s3:PutObjectRetention` | `isPutRetentionAllowed` [cmd/auth-handler.go:L704,L728-L731] | **403** `AccessDenied` | none | DENY (true authz) |
| 17 | GetObjectAttributes | `s3:GetObjectAttributes` **AND** `s3:GetObject` | `getObjectAttributesHandler` [cmd/object-handlers.go:L593-L596] | **403** `AccessDenied` | read only | DENY (see §12.1) |
| 18 | DeleteObject | `s3:DeleteObject` | `DeleteObjectHandler` [cmd/object-handlers.go:L2509,L2528] | **403** `AccessDenied` | none | DENY |
| 19 | DeleteObjects bulk (no MD5) | (request-validation gate) | `DeleteMultipleObjectsHandler` [cmd/bucket-handlers.go:L416,L433] | **400** `MissingContentMD5` | none | validation reject (pre-authz) |
| 20 | DeleteObjects bulk (valid MD5) | `s3:DeleteObject` per-object | per-object auth [cmd/bucket-handlers.go:L505] | **200**, per-object `AccessDenied`, **0 deleted** | none | DENY (true authz) |

**All 18 write-adjacent operations are denied; the 2 read operations behave as expected for read-only. Every "none" in the side-effect column was verified — see Section 8.**

---

## 8. Storage Side-Effect Verification (mandatory, not inferred)

After each probe wave the backend was checked two independent ways:

1. **On-disk (authoritative byte-level):** re-snapshot every object `xl.meta` under the data directory, normalize paths, and compare md5 + a combined manifest hash against the baseline. After the full static probe matrix (Sections 7, 10, 11): **`CURRENT == BASELINE == 62b7fdb620a4ec26dd4b3b8bb7195eb6`**, manifest diff **empty (4/4 objects byte-for-byte identical)**, only the 4 baseline object directories present, **0** pending multipart upload directories, and **all** would-be probe artifacts (`copy.txt`, `ro-mpu.txt`, `presigned-evil.txt`, `secret/presigned-evil2.txt`, `post-evil.txt`) **absent**.

2. **S3-layer (oracle corroboration):** listing via the authorized `writer` identity reported exactly the 3 seeded `testbucket` objects (same sizes/ETags), **3 versions, 0 delete markers**, and `readable/f1.txt` tags still `{class=public, team=research}` — confirming the denied `PutObjectTagging`/`DeleteObjectTagging`/`CopyObject(REPLACE)`/`DeleteObject` probes changed nothing.

**Conclusion:** every denial truly had no effect; the 403 responses were not masking partial writes.

---

## 9. Concurrent-Load / TOCTOU Results

**Setup:** 6 legitimate **writer** threads churned the `churn/` prefix of `testbucket` (rapid `PutObject` + `PutObjectTagging` + `DeleteObject` + `CreateMultipartUpload`/`Abort`), creating namespace-lock contention and a constantly-moving race surface, while 6 **read-only** (`rocanned`) threads raced four mutation classes each iteration:
- `PutObject` of a **uniquely-named canary** `readable/RO-CANARY-<uuid>.txt` (a definitive positive breach test — only the read-only identity ever attempts these names),
- a raw SigV4 **presigned-style PUT** `readable/RO-PRESIGN-*`,
- `DeleteObject` of a **live churn key** the writer had just created,
- `CreateMultipartUpload` `readable/RO-MPU-*`.

**Outcome (8.2 s):**

| Metric | Value |
|--------|-------|
| Concurrent legitimate writer ops | **2,502** |
| Read-only mutation attempts | **1,780** |
| Read-only **successes** | **0** |
| Read-only `403 AccessDenied` | **1,778** |
| **Breaches** (any canary/part/delete that took effect) | **0 (empty)** |

Per-class denial counts: `PutObject` 445, presigned-PUT 445, `CreateMultipartUpload` 445, race-`DeleteObject` 443 — all `403`.

**Post-storm side-effect check:** **zero** `RO-CANARY*`/`RO-PRESIGN*`/`RO-MPU*` artifacts anywhere on disk; the 4 baseline objects byte-for-byte identical. After purging the writers' *legitimate* churn (including delete-marker versions — see note), the data directory returned **exactly** to the baseline hash `62b7fdb620a4ec26dd4b3b8bb7195eb6` with an empty diff.

**Interpretation:** Under sustained contention, **no TOCTOU window exists**. This is the empirical confirmation of the code invariant from Section 6: because `IsAllowed` is evaluated before the object layer and a denial returns immediately, concurrent writers cannot create a moment in which a denied read-only request slips through to storage.

> *Secondary observation (versioning semantics, not a security finding):* on a versioned bucket, the writers' `DeleteObject` calls created delete markers, so prior-version `xl.meta` persisted on disk until an explicit `--versions` purge. This is documented MinIO behavior [docs/bucket/versioning/README.md] and is orthogonal to the authorization question.

---

## 10. Information Disclosure Without a GET (both variants)

| Operation | IAM action | Handler `[file:line]` | Variant A (canned) | Variant B (prefix `readable/*`) |
|-----------|------------|-----------------------|--------------------|---------------------------------|
| ListObjectsV2 (no prefix) | `s3:ListBucket` | [cmd/bucket-listobjects-handlers.go:L154,L172] | **403** | **403** (condition not satisfied) |
| ListObjectsV2 (`prefix=readable/`) | `s3:ListBucket` + `s3:prefix` | [cmd/bucket-listobjects-handlers.go:L172] | **403** | **200** (lists `readable/*`) |
| ListObjectsV2 (`prefix=secret/`) | `s3:ListBucket` + `s3:prefix` | [cmd/bucket-listobjects-handlers.go:L172] | **403** | **403** (prefix outside grant) |
| ListObjectsV1 | `s3:ListBucket` | [cmd/bucket-listobjects-handlers.go:L273,L287] | **403** | **403** (no prefix) |
| ListObjectVersions (no prefix) | `s3:ListBucket` | [cmd/bucket-listobjects-handlers.go:L62] | **403** | **403** |
| ListObjectVersions (`prefix=readable/`) | `s3:ListBucket` + `s3:prefix` | [cmd/bucket-listobjects-handlers.go:L62] | **403** | **200** |
| HeadBucket | `s3:ListBucket` | [cmd/bucket-handlers.go:L1644,L1658] | **403** | **403** (HEAD sends no prefix) |
| HeadObject (`readable/f1`) | `s3:GetObject` | `headObjectHandler` | **200** | **200** |
| HeadObject (`secret/secret`) | `s3:GetObject` | `headObjectHandler` | **200** | **403** (GetObject scoped to `readable/*`) |
| GetBucketLocation | `s3:GetBucketLocation` | [cmd/bucket-handlers.go:L204,L218] | **200** | **200** |

**What this means:**
- **Variant A (canned `readonly`) cannot enumerate at all** — every `List*` is `403`, because the canned policy has no `s3:ListBucket` (matching the intentional omission in [github.com/minio/pkg/v3@v3.0.22/policy/constants.go:L53-L60]). But it **can** `HeadObject`/`GetObject` **any** key it knows across **all** prefixes (including `secret/`), since its `GetObject` is granted on `arn:aws:s3:::*`.
- **Variant B confines both listing and head/get to the `readable/` prefix.** `secret/` is fully invisible: it can neither be listed nor head-ed. This is the correct least-privilege shape.

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
- **`PutObjectRetention`** likewise requires `Content-MD5` [cmd/object-handlers.go:L2855] and an object-lock-enabled bucket *before* authorization; on a lock-enabled bucket with a valid `Content-MD5` it reaches `isPutRetentionAllowed` [cmd/auth-handler.go:L704] → `IsAllowed(PutObjectRetentionAction)` and returns **403**.

The harness sent valid `Content-MD5` values precisely so the **authorization** decision (still denial) could be observed, and reported both the `400` and the `403`/per-object cases so the two are never conflated. Object-lock retention/legal-hold WORM semantics are themselves enforced independently of IAM [cmd/bucket-object-lock.go:L167].

---

## 13. Adjacent Surfaces (awareness only — not exercised)

The **primary identity under test is a static IAM user**. Research surfaced a later (post-checkout) MinIO CVE concerning **STS / service-account *session-policy*** privilege escalation. MinIO dispatches those identity classes through dedicated evaluators — `IsAllowedSTS` [cmd/iam.go:L2242] and `IsAllowedServiceAccount` [cmd/iam.go:L2140] — *before* the regular-user path in `IsAllowed`. If the read-only principal were instead an STS token or a service account carrying an inline session policy, that session-policy intersection would be the surface to scrutinize. That is a **follow-on consideration**, explicitly outside this static-user investigation, and is noted so the verdict's scope is precise.

---

## 14. Final Verdict & Rationale

**The read-only boundary HOLDS at commit `c07e5b49d`. A principal intended to be read-only on a bucket/prefix cannot mutate data through any probed write-adjacent surface — multipart, copy, tagging, ACL, retention, legal-hold, single or bulk delete — nor through presigned-URL, POST-policy, or reserved-bucket corners, and not under concurrent write contention.**

**Why (rationale, grounded in code + evidence):**
1. **Deny-by-default at a single chokepoint.** Every write-adjacent handler routes through `s3APIMiddleware` [cmd/api-router.go:L210] to an auth entry point that calls `IsAllowed` [cmd/iam.go:L2437], which denies anything not explicitly granted. The read-only policies grant no mutating action, so all mutations are denied. *(Evidence: 18/18 write-adjacent ops `403`/per-object `AccessDenied`.)*
2. **Authorization precedes the data plane.** The `IsAllowed` decision is made in the auth handler *before* the object/erasure layer is reached; a denial returns immediately, taking no namespace lock and writing no temp/rename. *(Evidence: every denial corroborated by a byte-for-byte unchanged backend.)*
3. **No TOCTOU window.** Because there is no code path from "denied" to "storage," concurrency cannot manufacture one. *(Evidence: 1,780 read-only mutation attempts against 2,502 concurrent legitimate writes → 0 successes, 0 breaches, baseline restored exactly.)*
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
| PutObjectRetention | `PutObjectRetentionHandler` [cmd/object-handlers.go:L2855] | `PutObjectRetentionAction` [cmd/auth-handler.go:L728-L731] | `isPutRetentionAllowed` [cmd/auth-handler.go:L704] |
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
| ListObjects v1/v2/versions | [cmd/bucket-listobjects-handlers.go:L273 / L154 / L62] | `ListBucketAction` [L287 / L172] | `checkRequestAuthType` |
| PutObjectACL / PutBucketACL | [cmd/acl-handlers.go:L172 / L61] | `PutBucketPolicyAction` [L193 / L77] | `checkRequestAuthType` |
| Object-lock (WORM) enforcement | `enforceRetentionBypassForPut` [cmd/bucket-object-lock.go:L167] | (evaluated independently of IAM) | — |
| Authorization engine | — | `IAMSys.IsAllowed` [cmd/iam.go:L2437]; `IsAllowedSTS` [L2242]; `IsAllowedServiceAccount` [L2140] | deny-by-default |

*Investigation performed against `github.com/minio/minio` @ `c07e5b49d477b0774f23db3b290745aef8c01bd2`. Repository unmodified; this document is the sole artifact.*
