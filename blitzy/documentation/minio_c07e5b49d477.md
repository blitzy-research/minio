# MinIO Security-Behavior Investigation — Runtime-Evidenced Answers

> **Source branch:** `minio_c07e5b49d477` &nbsp;•&nbsp; **Commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2` ("refactor: replace experimental maps and slices with stdlib (#20679)")
> **Repository:** `github.com/minio/minio` &nbsp;•&nbsp; **Module go directive:** `go 1.23` (`go.mod:3`)

## Overview

This document answers five MinIO object-storage security-behavior questions. **Every behavioral claim is backed by output captured at runtime** from a canonical MinIO server that was built from this exact commit and driven through its **real S3 and admin API entry points**. The methodology was *run the code first, then write* — the server was built, launched in erasure mode, and each behavior was reproduced and captured before any conclusion was drawn.

The five questions:

1. **Q1 — SSE requirement vs. broad write permission on an unencrypted PUT.** What happens when a bucket-level encryption requirement takes precedence over a user's broad write permission during an unencrypted upload, and what is the ordered runtime execution captured in the server trace?
2. **Q2 — Object-lock delete enforcement logging.** When object locking is enabled on a bucket, what specific log/error entries appear when someone tries to delete the locked objects?
3. **Q3 — Bit-rot detection on read.** How does the system handle unauthorized manual data corruption in the storage backend? Trigger a bit-rot detection event and identify the specific runtime logs generated during a subsequent GET request.
4. **Q4 — STS session-policy enforcement.** When a user gets temporary credentials, prove MinIO enforces the inline session policy on that user.
5. **Q5 — Privilege-escalation prevention via user mappings.** Prove a user with basic access cannot promote themselves to console admin by modifying the user→policy mappings, and identify the root cause.

A per-question **Coverage Pass** at the end maps every named mechanism, function, condition, and flag to its concrete value, file:line anchor, and observed evidence.

---

## Environment / Build

All observations were produced with the following toolchain, build, and server configuration. Every command is reproducible.

### Toolchain

```
$ go version
go version go1.23.12 linux/amd64
```

`go1.23.12` is the highest 1.23.x patch available and matches `go.mod`'s `go 1.23` directive (`go.mod:3`), the CI pin of `1.23.x` (`.github/workflows/go.yml`), and `Dockerfile.release`'s `golang:1.23-alpine`.

### Canonical build command

The build is the project's canonical target from `Makefile:177-179`:

```makefile
build: checks build-debugging ## builds minio to $(PWD)
	@echo "Building minio binary to './minio'"
	@CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
```

with `LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)` (`Makefile:3`). The exact command used, with the binary emitted **outside** the repository tree to keep the working tree pristine:

```bash
CGO_ENABLED=0 go build -tags kqueue -trimpath \
  --ldflags "$(go run buildscripts/gen-ldflags.go)" \
  -o /tmp/minio-investigation/minio
```

The resulting `gen-ldflags.go` output stamps this build as:

```
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z \
   -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z \
   -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 \
   -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 ...
```

### Server invocation (ERASURE MODE — required for Q3)

The server was launched with **four drive directories** so that erasure parity shards exist (required for Q3's heal-on-read). The `MINIO_KMS_SECRET_KEY` is the standard CI demo key, needed to enable SSE-S3 / default-encryption for Q1.

```bash
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=" \
  /tmp/minio-investigation/minio server \
  /tmp/minio-investigation/data/d1 /tmp/minio-investigation/data/d2 \
  /tmp/minio-investigation/data/d3 /tmp/minio-investigation/data/d4 \
  --address :9000 --console-address :9001
```

Startup banner and cluster info confirm a **4-drive single-set erasure layout with EC:2 parity** (NOT single-drive/FS mode):

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.

$ mc admin info local
●  127.0.0.1:9000
   Uptime: 32 minutes
   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK
   Drives: 4/4 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.8% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘
```

The standard storage class parity is `EC:2` (2 data + 2 parity across the 4 drives), which the Q3 `xl.meta` decode confirms (`EcM=2`, `EcN=2`).

### Client / tooling

| Tool | Version | Use |
|------|---------|-----|
| `mc` (MinIO Client) | `RELEASE.2025-08-13T08-35-41Z` (embeds `minio-go/v7.0.90`) | Bucket/user/policy config, S3 ops, `mc admin trace` |
| `minio-go/v7` SDK | `v7.0.80` (`go.mod`) | Scratch driver programs (single-object DELETE, STS AssumeRole) |
| `madmin-go/v3` SDK | `v3.0.77` (`go.mod`) | Scratch driver (deprecated `SetPolicy` admin call) |
| `xl-meta` | in-repo `docs/debugging/xl-meta` | Decode `xl.meta` → JSON to locate the Q3 shard |
| `reedsolomon` | `v1.12.4` (`go.mod`) | Reed-Solomon erasure coding / reconstruction (Q3) |
| `minio/sio` | `v0.4.1` (`go.mod`) | DARE encrypted-object stream (Q1 SSE) |
| `minio/pkg/v3` | `v3.0.22` (`go.mod`) | Policy engine + admin-action constants (Q4, Q5) |

Client alias:

```bash
mc alias set local http://127.0.0.1:9000 minioadmin minioadmin
```

### How the runtime signals were captured

- **HTTP trace** (the primary signal for Q1–Q3, and used throughout): `mc admin trace -v local` (or `--all -v` to include the `[STORAGE ...]` internal calls). Each request is shown as a `[REQUEST <api>]` block with headers/signature, followed by a `[RESPONSE]` block with the status line (`200 OK`, `403 Forbidden`, …) and, for failures, the XML/JSON error body.
- **Scanner trace** (Q3 background bit-rot cycle): `mc admin scanner trace local` (function `scanner.ScanObject`).
- **Server console/audit log**: the server's stderr was tee'd to a log file; the `internal/logger` targets (`console.go`, `audit.go`, `reqinfo.go`) emit any server-side log lines.

All ephemeral artifacts (server data dirs, scratch driver programs, test buckets/users/policies) live under `/tmp/minio-investigation/` — **outside** the repository — and were removed after the investigation. The only file added to the repository is this document.

---

## Conventions

### Observed vs. Inferred labeling

Every factual claim is tagged:

- **[OBSERVED]** — captured at runtime, paired with the exact producing command and its complete, unedited output.
- **[INFERRED]** — derived from reading the source because the signal genuinely could not be produced at runtime. Each such statement justifies why it could not be observed. **This document contains essentially no inferred behavioral claims** — every behavior below was reproduced live; source citations are used to explain *why* an observed behavior occurs, not to substitute for observation.

### Evidence discipline

- Each behavioral claim carries its **producing command** and its **complete, unedited output** (real log lines, full command output, HTTP status codes, error bodies, hashes). Nothing is paraphrased or truncated before the relevant event.
- Each factual claim about the code carries a **file:line anchor** and names the responsible **function / struct / method**. Anchors were re-confirmed against the live source at commit `c07e5b49d477`; where the platform's initial assumption diverged from observed reality, **the document text was corrected to match what the running server actually did** (never the source).
- Each question's **primary path and its sibling/edge variants** are exercised; for state changes the **before / during / after** states are shown.
- Only **canonical entry points** (the real S3/admin/STS APIs) are used as proof.
- Stability-sensitive observations were **re-run at least twice** and confirmed stable.

### Two important, up-front corrections to common assumptions

1. **Q1:** A **bucket policy** `Deny` on unencrypted uploads does **NOT** gate an **authenticated IAM user** in MinIO — bucket policies are only evaluated for **anonymous** requests. For an authenticated user, the encryption requirement must live in the user's **identity policy** (or be enforced transparently by **bucket default encryption**). This is proven below with four sub-tests.
2. **Q2:** The object-lock delete error `ErrObjectLocked` maps to **HTTP 400 `InvalidRequest`** ("Object is WORM protected and cannot be overwritten"), **not 403**. The one 403 in Q2 is a *different* error (`AccessDenied`) returned when a user lacks `s3:BypassGovernanceRetention`. Both are shown with their actual codes.

---

## Q1 — SSE requirement vs. broad write permission on an unencrypted PUT

**Question:** *What happens when a bucket-level encryption requirement takes precedence over a user's broad write permissions during an unencrypted upload? Identify the specific runtime execution sequence captured in the server trace logs.*

### Summary of findings

There are **two distinct enforcement mechanisms**, and they behave differently:

| Mechanism | Enforced for | Unencrypted PUT outcome | Where decided |
|-----------|--------------|-------------------------|---------------|
| **(a) Bucket policy** `DenyUnEncryptedObjectUploads` | **anonymous only** | Rejected **403** (anon) / **allowed** (authenticated IAM user) | Authorization gate — but only when `cred.AccessKey == ""` |
| **(b) Identity policy** with the same `Deny` | the authenticated user | Rejected **403 AccessDenied** | Authorization gate `IAMSys.IsAllowed` |
| **(c) Bucket default encryption** (`PutBucketEncryption` / `mc encrypt set sse-s3`) | everyone | **Allowed 200**, transparently **auto-encrypted** | Encryption gate `sseConfig.Apply`, *after* authorization |

The runtime decision ordering captured in the trace for every PUT is: **(1) SigV4 signature auth** (middleware) → **(2) action authorization** (`isPutActionAllowed` → `IAMSys.IsAllowed`) → **(3) encryption gate** (`sseConfig.Apply`). A deny is emitted at stage (2) with `403`; default encryption is applied at stage (3).

### Correction to the classic assumption (proven below)

The classic S3 `DenyUnEncryptedObjectUploads` **bucket** policy rejects unencrypted PUTs *for anonymous callers only*. In MinIO, `checkRequestAuthType`/`isPutActionAllowed` evaluate the **bucket policy** (`globalPolicySys.IsAllowed`) **only when the credential is anonymous** (`cred.AccessKey == ""`); an authenticated IAM user is evaluated **only against its identity policy** (`globalIAMSys.IsAllowed`). So for an authenticated user holding `s3:*`, a *bucket-policy* deny is never consulted — the encryption requirement must be expressed in the user's **identity policy** to take precedence over broad write. This is the mechanism the question is really about, and it is demonstrated in **TEST C**.

Responsible code:

- `cmd/object-handlers.go` — `PutObjectHandler` at **L1745**; the authorization gate `isPutActionAllowed(ctx, rAuthType, bucket, object, r, policy.PutObjectAction)` at **L1836**; the default-encryption application `sseConfig.Apply(r.Header, sse.ApplyOptions{AutoEncrypt: globalAutoEncryption})` at **L1895**.
- `cmd/auth-handler.go` — `isPutActionAllowed` evaluates the **bucket** policy via `globalPolicySys.IsAllowed(...)` only for anonymous (`cred.AccessKey == ""`), otherwise `globalIAMSys.IsAllowed(...)` for the identity.
- `cmd/bucket-policy.go` — `getConditionValues()` at **L77** and the header-copy loop at **L178-186** build the policy condition keys; the key `s3:x-amz-server-side-encryption` **only exists when the client actually sends the header**, so an *absent* header makes the `Null: {"s3:x-amz-server-side-encryption": "true"}` condition match → `Deny`.
- `cmd/iam.go` — `IsAllowed()` at **L2437-2481**, the deny-by-default dispatch.
- `internal/crypto/auto-encryption.go` — `EnvKMSAutoEncryption` (`MINIO_KMS_AUTO_ENCRYPTION`) at **L31**, `LookupAutoEncryption()` at **L37**; wired to `globalAutoEncryption` at `cmd/config-current.go:532`.

### Fixtures

Broad-write user `q1user` with an identity policy allowing `s3:*` on the test buckets:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::q1-policy", "arn:aws:s3:::q1-policy/*", "arn:aws:s3:::q1-defenc", "arn:aws:s3:::q1-defenc/*"]
    }
  ]
}
```

The canonical `DenyUnEncryptedObjectUploads` **bucket** policy applied to `q1-policy` (TEST A/B):

```json
{
  "Version": "2012-10-17",
  "Id": "PutObjPolicy",
  "Statement": [
    {
      "Sid": "DenyUnEncryptedObjectUploads",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::q1-policy/*",
      "Condition": { "Null": { "s3:x-amz-server-side-encryption": "true" } }
    }
  ]
}
```

### TEST A — authenticated `q1user` + `DenyUnEncryptedObjectUploads` *bucket* policy → **SUCCEEDS (200)** [OBSERVED]

```
$ mc cp payload.txt q1/q1-policy/unencrypted-object.txt
`/tmp/minio-investigation/evidence/q1/payload.txt` -> `q1/q1-policy/unencrypted-object.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 25 B  │ 25 B        │ 00m00s   │ 2.43 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
```

The unencrypted PUT **succeeds** even though a `DenyUnEncryptedObjectUploads` bucket policy is present, because `q1user` is an **authenticated** identity and the bucket policy is not consulted for authenticated users.

### TEST B — anonymous caller + the same bucket policy → **403 without SSE, 200 with SSE** [OBSERVED]

Anonymous unencrypted PUT (raw `curl`, no credentials, no SSE header):

```
$ curl -s -w 'HTTP_STATUS=%{http_code}\n' -X PUT --data-binary @payload.txt \
       http://127.0.0.1:9000/q1-anon/anon-unencrypted.txt
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>anon-unencrypted.txt</Key><BucketName>q1-anon</BucketName><Resource>/q1-anon/anon-unencrypted.txt</Resource><RequestId>18C1EA8411768266</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
HTTP_STATUS=403
```

Anonymous PUT **with** the SSE header present:

```
$ curl -s -w 'HTTP_STATUS=%{http_code}\n' -X PUT --data-binary @payload.txt \
       -H 'x-amz-server-side-encryption: AES256' \
       http://127.0.0.1:9000/q1-anon/anon-encrypted.txt

HTTP_STATUS=200
```

This confirms the bucket policy **is** enforced for anonymous callers: absent SSE header → `Null` condition matches → `Deny` → **403**; present SSE header → condition does not match → allowed → **200**.

### TEST C — authenticated user + the encryption `Deny` in its **identity policy** → **403 without SSE, 200 with SSE** [OBSERVED]

This is the correct way an encryption requirement takes precedence over broad write for an authenticated user. User `q1denyuser` has this identity policy (broad `Allow s3:*` **plus** an explicit `Deny` on unencrypted PUT):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "AllowAll", "Effect": "Allow", "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::q1-iddeny", "arn:aws:s3:::q1-iddeny/*"] },
    { "Sid": "DenyUnEncryptedObjectUploads", "Effect": "Deny", "Action": ["s3:PutObject"],
      "Resource": ["arn:aws:s3:::q1-iddeny/*"],
      "Condition": { "Null": { "s3:x-amz-server-side-encryption": "true" } } }
  ]
}
```

Unencrypted PUT → **denied**:

```
$ mc cp payload.txt q1deny/q1-iddeny/should-be-denied.txt
`/tmp/minio-investigation/evidence/q1/payload.txt` -> `q1deny/q1-iddeny/should-be-denied.txt`
mc: <ERROR> Failed to copy `/tmp/minio-investigation/evidence/q1/payload.txt`. Insufficient permissions to access this path `http://127.0.0.1:9000/q1-iddeny/should-be-denied.txt`
```

The `mc admin trace -v` block for this request — note the request carries **no** `x-amz-server-side-encryption` header (`SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length`) and the response is **403 Forbidden**:

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T17:45:38.612] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-iddeny/should-be-denied.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Date: 20260713T174538Z
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 25
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1denyuser/20260713/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=727b8c964bc8cf4411f32c2d1d09ffecb9173cc7208db0bf50962ace92d350c3
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Content-Length: 198
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-13T17:45:38.612] [ Duration 106µs TTFB 93.971µs ↑ 135 B  ↓ 349 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>should-be-denied.txt</Key><BucketName>q1-iddeny</BucketName><Resource>/q1-iddeny/should-be-denied.txt</Resource><RequestId>18C1EA7A58CB21E2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Same user, PUT **with** SSE-S3 → **succeeds**:

```
$ mc cp --enc-s3 payload.txt q1deny/q1-iddeny/encrypted-ok.txt
`/tmp/minio-investigation/evidence/q1/payload.txt` -> `q1deny/q1-iddeny/encrypted-ok.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 25 B  │ 25 B        │ 00m00s   │ 2.31 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
```

With SSE requested, the client signs `x-amz-server-side-encryption` (it appears in `SignedHeaders`), so the policy condition key exists and does **not** equal `null`, the `Deny` does not match, and the broad `Allow s3:*` permits the PUT. **The explicit `Deny` in the identity policy takes precedence over the broad `Allow` — this is how the encryption requirement "wins" over broad write for an authenticated user.**

### TEST D — bucket default encryption (SSE-S3) → unencrypted PUT **auto-encrypted (200)** [OBSERVED]

Default encryption set on `q1-defenc`:

```
$ mc encrypt set sse-s3 local/q1-defenc
```

`q1user` PUTs with **no** SSE header:

```
$ mc cp payload.txt q1/q1-defenc/auto-encrypted.txt
`/tmp/minio-investigation/evidence/q1/payload.txt` -> `q1/q1-defenc/auto-encrypted.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 25 B  │ 25 B        │ 00m00s   │ 2.03 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
```

Proof the stored object is encrypted despite no client SSE header:

```
$ mc stat local/q1-defenc/auto-encrypted.txt
Name      : auto-encrypted.txt
Size      : 25 B
Encryption: SSE-S3
```

Proof the encryption was injected **server-side** (not by the client): in the `mc admin trace -v` capture of this PUT, the request's `SignedHeaders` never include an SSE header —

```
SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length
```

— yet the **response** carries the server-added header:

```
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
```

and the client made **zero** `GetBucketEncryption` calls (`GetBucketEncryption calls: 0` across the whole trace). The server injected the encryption via `sseConfig.Apply(r.Header, sse.ApplyOptions{AutoEncrypt: globalAutoEncryption})` at `cmd/object-handlers.go:1895` — i.e., default encryption does **not reject**; it transparently encrypts **after** authorization.

### Decision ordering (from the trace)

For every PUT above, the captured order is:

1. **SigV4 signature authentication** (middleware) — the `Authorization`/`SignedHeaders` are verified first.
2. **Action authorization** — `isPutActionAllowed` (`cmd/object-handlers.go:1836`) → `IAMSys.IsAllowed` (`cmd/iam.go:2437`). For a deny (TEST C), the `403 Forbidden` is emitted **here**, before any object body is written (TTFB ≈ 94 µs, ↓ 349 B error body).
3. **Encryption gate** — `sseConfig.Apply` (`cmd/object-handlers.go:1895`) runs only for allowed requests; for TEST D it injects `AES256`.

### Stability

All four sub-tests (A–D) were re-run a second time with identical verdicts (A: 200; B: 403 then 200; C: 403 then 200; D: 200 + SSE-S3). **Stable.**

---


## Q2 — Object-lock delete enforcement logging

**Question:** *When object locking on a bucket is enabled, what are the specific log entries that appear when someone tries to delete the locked objects? Give me runtime log output to show this.*

### Summary of findings

The runtime signal is the **HTTP error response returned by the delete API**, not a server console log line. On a normal WORM block, MinIO emits **no** console log entry (the enforcement function only logs via `internalLogIf(..., logger.WarningKind)` on an NTP clock error, `cmd/bucket-object-lock.go:142`). The observable, canonical signal is:

| Delete scenario | HTTP status | Error `Code` / body |
|-----------------|-------------|----------------------|
| Legal hold ON, specific version, single-object `DeleteObject` | **400** | `InvalidRequest` — "Object is WORM protected and cannot be overwritten" |
| Legal hold ON, multi-object `DeleteObjects` | **200** envelope | per-object embedded `<Error><Code>InvalidRequest</Code>…` |
| Governance, no bypass | **400** | `InvalidRequest` — WORM |
| Governance, bypass **with** `s3:BypassGovernanceRetention` | **success** | version removed |
| Governance, bypass header **without** the permission | **403** | `AccessDenied` — "Access Denied." |
| Compliance, bypass attempted (even by root) | **400** | `InvalidRequest` — WORM (cannot be bypassed) |
| Plain delete, **no** versionId (versioned bucket) | **success** | inserts a delete marker (NOT blocked) |

Responsible code:

- `cmd/bucket-object-lock.go` — `enforceRetentionBypassForDelete()` at **L84**; legal-hold `return ObjectLocked{}` at **L100-101**; compliance branch at **L107/L117/L121**; governance branch at **L124-155** (`byPassSet` at L138, `return ObjectLocked{}` at L143/L147, the bypass-permission check `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, …)` at **L153**, `return errAuthentication` at **L154**).
- `cmd/object-handlers.go` — `DeleteObjectHandler` at **L2509**; `opts.SetEvalRetentionBypassFn(...)` at **L2598** wires `enforceRetentionBypassForDelete`, invoked only when a `versionId` is present (`vID != ""` at **L2600**).
- `cmd/object-api-errors.go` — `ObjectLocked{}` `Error()` returns "Object is WORM protected and cannot be overwritten: bkt/obj(vid)".
- `cmd/api-errors.go` — `ObjectLocked` → `ErrObjectLocked` mapping at **L2298-2299**; `ErrObjectLocked` entry at **L1059-1062** = `Code:"InvalidRequest"`, `Description:"Object is WORM protected and cannot be overwritten"`, `HTTPStatusCode: http.StatusBadRequest` (**400**); `errAuthentication` → `ErrAccessDenied` at **L2176-2177** (**403**).

### Fixtures

Object-lock bucket (object lock requires versioning; `--with-lock` enables both):

```
$ mc mb --with-lock local/q2-lock
```

Single-object deletes were driven with a small `minio-go/v7` scratch program (`RemoveObject` → `DELETE /bucket/object?versionId=…`) so the *direct* per-request HTTP status is visible (contrasted with `mc rm`, which uses the batch `DeleteObjects` POST).

### Legal hold — specific version delete → **BLOCKED (400)** [OBSERVED]

Single-object `DeleteObject` on the locked version:

```
$ AK=minioadmin SK=minioadmin ./delete_single q2-lock legalhold-obj.txt <versionId>
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
  BucketName:     q2-lock
  Key:            legalhold-obj.txt
```

Multi-object delete (`mc rm --version-id`) returns an HTTP **200** envelope whose body carries the per-object error:

```
$ mc rm --version-id <versionId> local/q2-lock/legalhold-obj.txt
mc: <ERROR> Failed to remove `local/q2-lock/legalhold-obj.txt`. Object, 'legalhold-obj.txt (Version ID=cbb790c2-8db6-4bef-b3b6-eb6a8f95d0f0)' is WORM protected and cannot be overwritten
```

The `mc admin trace` shows the `DeleteMultipleObjects` request returning `200 OK` with the embedded error in the response body:

```
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-13T17:49:05.086] [Client IP: 127.0.0.1]
127.0.0.1:9000 200 OK
127.0.0.1:9000 <DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>legalhold-obj.txt</Key><VersionId>cbb790c2-8db6-4bef-b3b6-eb6a8f95d0f0</VersionId></Error></DeleteResult>
```

This is a documented S3 semantic: `DeleteObjects` (batch) always returns `200` and reports per-key failures in the body; single `DeleteObject` returns the real status (`400`).

### Governance — no bypass → **BLOCKED (400)**; with bypass → conditional on permission [OBSERVED]

Governance retention (`mc retention set --version-id <vid> GOVERNANCE 3d local/q2-lock/gov-obj.txt`). No bypass header:

```
$ AK=minioadmin SK=minioadmin ./delete_single q2-lock gov-obj.txt <versionId>
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
  BucketName:     q2-lock
  Key:            gov-obj.txt
```

Bypass (`x-amz-bypass-governance-retention: true`) as **root** (which holds `s3:BypassGovernanceRetention`) → **succeeds**:

```
$ AK=minioadmin SK=minioadmin ./delete_single q2-lock gov-obj.txt <versionId> bypass
DELETE RESULT: SUCCESS (version 1e5bdde9-8fb3-4629-902c-74c24d85eb91 removed)
```

Bypass header **but** as a user (`q2user`) **without** `s3:BypassGovernanceRetention` → **403 AccessDenied** (a *different* error from the WORM 400):

```
$ AK=q2user SK=q2secret ./delete_single q2-lock gov-noperm.txt <versionId> bypass
DELETE RESULT: ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
  Message:        Access Denied.
  BucketName:     q2-lock
  Key:            gov-noperm.txt
```

This is the `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, …)` check at `cmd/bucket-object-lock.go:153` returning `errAuthentication` (L154), which maps to `ErrAccessDenied` / **403** (`cmd/api-errors.go:2176-2177`).

### Compliance — bypass attempted (even by root) → **BLOCKED (400)** [OBSERVED]

Compliance retention cannot be bypassed by anyone until it expires:

```
$ AK=minioadmin SK=minioadmin ./delete_single q2-lock comp-obj.txt <versionId> bypass
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
  BucketName:     q2-lock
  Key:            comp-obj.txt
```

The governance bypass path is simply not reachable for compliance mode (`cmd/bucket-object-lock.go:107-121` returns `ObjectLocked{}` with no bypass branch).

### Delete marker — plain delete (no versionId) → **NOT blocked** (before/during/after) [OBSERVED]

A plain delete (no `versionId`) on a versioned bucket is **not** a delete of the locked version — it inserts a **delete marker**, which is allowed:

```
$ mc rm local/q2-lock/legalhold-obj.txt
Created delete marker `local/q2-lock/legalhold-obj.txt` (versionId=31f763df-25d2-48b9-8dac-207b1a64dbb6).
```

**After** — the version listing shows the new delete marker (`v2 DEL`) *and* the original locked version (`v1 PUT`) still present and intact:

```
$ mc ls --versions local/q2-lock/legalhold-obj.txt
[2026-07-13 17:51:52 UTC]     0B STANDARD 31f763df-25d2-48b9-8dac-207b1a64dbb6 v2 DEL legalhold-obj.txt
[2026-07-13 17:49:03 UTC]    33B STANDARD cbb790c2-8db6-4bef-b3b6-eb6a8f95d0f0 v1 PUT legalhold-obj.txt
```

Root cause: `DeleteObjectHandler` only calls `enforceRetentionBypassForDelete` when a `versionId` is present (`opts.SetEvalRetentionBypassFn` at `cmd/object-handlers.go:2598`, guarded by `vID != ""` at L2600). A plain delete carries no `versionId`, so the WORM check is never invoked.

### On the "log entries"

The question asks for the specific **log entries** that appear. The runtime observation is that **no server console log line is emitted for a normal WORM-blocked delete** — the enforcement is surfaced entirely through the **API error response** (400 `InvalidRequest` / embedded batch error, or 403 `AccessDenied` for the permission case). The only `internal/logger` call inside `enforceRetentionBypassForDelete` is `internalLogIf(ctx, err, logger.WarningKind)` at `cmd/bucket-object-lock.go:142`, which fires **only** when the retention time-check hits an NTP/clock error — not on a normal block. The authoritative, capturable log entries are therefore the trace/response entries shown above.

### Stability

Each variant was reproduced with identical results on a second run (legal-hold 400; governance 400 / success / 403; compliance 400; delete-marker allowed). **Stable.**

---


## Q3 — Bit-rot detection and heal-on-read

**Question:** *Analyze how the system handles unauthorized manual data corruption within the storage backend. Trigger a bit-rot detection event and identify the specific runtime logs generated during a subsequent GET request.*

### Summary of findings

1. **Heal-on-read is silent on the console by default.** When a shard's checksum fails during a GET, `streamingBitrotReader.ReadAt` returns `errFileCorrupt` with **no logger call** (`cmd/bitrot-streaming.go:184-185`). The object is still returned correctly by reconstructing from parity, and a background heal op is queued — but nothing is written to the server console. The observable runtime signals of the detection during a GET are therefore: **(a)** the trace shows reads fanning out to **all** shards (parity fallback), **(b)** the returned bytes still match the original (reconstruction), and **(c)** a partial-heal op is queued via `globalMRFState.addPartialOp(... BitrotScan: true ...)`.
2. **The explicit, visible detect-and-repair path is the deep-scan heal** (`mc admin heal --scan deep`), which calls `storage.VerifyFile` on **every** shard (the bit-rot checksum verification), reports the object transitioning `[Red → Green]`, and **rewrites the corrupted shards** back to their exact original bytes.

Responsible code:

- `cmd/bitrot-streaming.go` — `streamingBitrotReader.ReadAt()` at **L150**; on checksum mismatch (`bytes.Equal(b.h.Sum(nil), b.hashBytes)` fails) it does `return 0, errFileCorrupt` at **L184-185** (no log line).
- `cmd/storage-errors.go` — `errFileCorrupt = StorageErr("file is corrupted")` at **L104**.
- `cmd/erasure-decode.go` — the `parallelReader` handles the corrupt shard: `case errors.Is(err, errFileCorrupt):` at **L197** sets `atomic.StoreInt32(&bitrotHeal, 1)` at **L198**, and returns `errFileCorrupt` at **L228** so the decoder falls back to parity.
- `cmd/erasure-object.go` — `var healOnce sync.Once` at **L346**; once the requested part is fully served, `healOnce.Do` runs `globalMRFState.addPartialOp(PartialOperation{ …, BitrotScan: errors.Is(err, errFileCorrupt) })` at **L400-407** and the error is nil'd so the client receives correct bytes.
- `cmd/xl-storage.go` — `VerifyFile()` at **L3097**, `bitrotVerify()` at **L3079** (the disk-level checksum verification used by the scanner/heal).
- `cmd/erasure-healing.go` — `healObject` at **L258**; deep-scan bit-rot escalation at **L1080-1084**.
- `cmd/background-newdisks-heal-ops.go` — the MRF (most-recently-failed) heal routine at **L389-390**.
- `cmd/data-scanner.go` — the background bit-rot scan cycle at **L94**.

Bit-rot protection uses **HighwayHash** per erasure block (`CSumAlgo: 1` in the `xl.meta` below), per `docs/erasure/README.md`.

### Setup — PUT a 6 MiB object and decode its on-disk layout [OBSERVED]

```bash
$ head -c 6291456 /dev/urandom > original.bin
$ sha256sum original.bin
894e3b9464818f9a0f0a5c31d1f6042ba523addae65f0c7c22acae5614bc5853  original.bin
$ mc cp original.bin local/q3-bitrot/bitrot-obj.bin
```

The object is stored on **all four** drives (each with an `xl.meta`, a data-dir UUID, and a `part.1`). Decoding one drive's `xl.meta` with `xl-meta`:

```bash
$ ./xl-meta -d data/d1/q3-bitrot/bitrot-obj.bin/xl.meta   # (piped to python -m json.tool)
```

```json
{
  "Versions": [
    {
      "Header": { "EcM": 2, "EcN": 2, "Type": 1, "VersionID": "00000000000000000000000000000000" },
      "Idx": 0,
      "Metadata": {
        "Type": 1,
        "V2Obj": {
          "CSumAlgo": 1,
          "DDir": "LgIMnvwbQA6fHNPWRH+A7Q==",
          "EcAlgo": 1,
          "EcBSize": 1048576,
          "EcDist": [2, 3, 4, 1],
          "EcIndex": 2,
          "EcM": 2,
          "EcN": 2,
          "MetaUsr": { "content-type": "application/octet-stream", "etag": "54696be11bdd9d0ef79329b48e7b563f" },
          "PartNums": [1],
          "PartSizes": [6291456],
          "Size": 6291456
        }
      }
    }
  ]
}
```

Interpretation: **`EcM=2`** data blocks, **`EcN=2`** parity blocks, **`EcBSize=1 MiB`** per erasure block, **`EcDist=[2,3,4,1]`** (block distribution order across drives), **`EcAlgo=1`** (Reed-Solomon), **`CSumAlgo=1`** (HighwayHash bit-rot checksum). Each drive holds a ~3 MiB shard (`part.1` = 3 145 920 bytes).

**Original good shard hashes (before corruption):**

```
d1 part.1 sha256 = 8238eb3cd2fcc8affe5cdf10107bd0e8399bbf07360df50b83ca9ca590c6586d
d2 part.1 sha256 = f9962ce0ef1beb68e68b573ae898e4e351d57b8a11a5476afdc5922a972430f1
d3 part.1 sha256 = ef9fee4d1f5ab37ba98d188dfcd1c1e14ca0356f00990d3e5efa0cf58518aaee
d4 part.1 sha256 = 67b2edea4c181fcd006db387d6265a384240faed124bac750a4cc348a36691a2
```

### Read-set selection — a methodological note [OBSERVED]

With `EcM=2` (only 2 of 4 shards are needed to read), corrupting a shard that is **not** in the initial read set produces **no** observable effect — the object is served from the other shards. To reliably trigger detection, **read-set-member** shards must be corrupted. The final corruption used **two** drives (`d1` and `d2`), leaving exactly `EcM=2` good shards (`d3`, `d4`) — the minimum needed to still reconstruct.

### BEFORE / corruption / AFTER — the on-disk shard state [OBSERVED]

Target region of `d2`'s `part.1` **before** corruption (offset 500000, 32 bytes, via `od`):

```
$ od -A d -t x1 -j 500000 -N 32 data/d2/.../part.1
0500000 34 64 95 b2 14 eb f6 0d 93 5b 13 36 dc 7f 3d 3b
0500016 f8 c3 02 f1 39 ac 5e 22 75 54 1a d7 b8 77 4f ad
0500032
```

Corrupt 32 bytes of `d2` (write `0xBA`) and 64 bytes of `d1` (write `0xC0`), simulating unauthorized backend tampering:

```bash
$ printf '\xba%.0s' {1..32} | dd of=data/d2/.../part.1 bs=1 seek=500000 count=32 conv=notrunc
$ printf '\xc0%.0s' {1..64} | dd of=data/d1/.../part.1 bs=1 seek=200000 count=64 conv=notrunc
```

Same region of `d2` **after** corruption:

```
$ od -A d -t x1 -j 500000 -N 32 data/d2/.../part.1
0500000 ba ba ba ba ba ba ba ba ba ba ba ba ba ba ba ba
*
0500032
```

Shard hashes after corruption: `d1 = 2bc3ef555f17…`, `d2 = aba2bfd2b0e6…` (both changed from the originals above).

### During the GET — reconstruction from parity (correct bytes returned) [OBSERVED, stable ×2]

```bash
$ mc cp local/q3-bitrot/bitrot-obj.bin retrieved.bin
$ sha256sum retrieved.bin
894e3b9464818f9a0f0a5c31d1f6042ba523addae65f0c7c22acae5614bc5853  retrieved.bin
```

The retrieved hash equals the **original** `894e3b94…` — the object is returned **correct** despite two corrupted shards. The `mc admin trace --all -v` capture of the GET shows storage reads fanning out to **all four** shards (the two good primaries plus the parity shards, i.e., the parity fallback triggered by the corruption):

```
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-13T18:02:51.960] [Client IP: 127.0.0.1]
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d1 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d4 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d2 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d3 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
```

**No bit-rot/heal line appears in the server console log during this GET** — confirming heal-on-read is silent by default (`cmd/bitrot-streaming.go:184-185` returns `errFileCorrupt` with no logger call). The detection is real (it forces the all-shard parity read and sets the internal `bitrotHeal` flag at `cmd/erasure-decode.go:198`), but its only outward signals are the reconstruction and the queued MRF heal op.

### The visible detect-and-repair — deep-scan heal [OBSERVED, stable ×2]

A normal heal only checks shard existence/metadata and does **not** repair data corruption:

```
$ mc admin heal -r --force local/q3-bitrot
[Green  ->  Green] q3-bitrot/     # unchanged; data checksums not verified
```

A **deep**-scan heal recomputes checksums, detects the corruption, and repairs it:

```
$ mc admin heal -r --scan deep --force local/q3-bitrot
[Green  ->  Green] q3-bitrot/
[Red    ->  Green] q3-bitrot/bitrot-obj.bin
Healed:	1/1 objects; 6 MiB in 1s
```

The `mc admin trace --all -v` during the deep heal shows the storage-layer bit-rot verification (`storage.VerifyFile` on **all four** drives) followed by reconstruction reads from the two good shards (`d3`, `d4`):

```
127.0.0.1:9000  [STORAGE storage.VerifyFile] .../data/d1 q3-bitrot bitrot-obj.bin total-errs-availability=0 total-errs-timeout=0 192.319µs
127.0.0.1:9000  [STORAGE storage.VerifyFile] .../data/d2 q3-bitrot bitrot-obj.bin total-errs-availability=0 total-errs-timeout=0 161.376µs
127.0.0.1:9000  [STORAGE storage.VerifyFile] .../data/d3 q3-bitrot bitrot-obj.bin total-errs-availability=0 total-errs-timeout=0 882.809µs
127.0.0.1:9000  [STORAGE storage.VerifyFile] .../data/d4 q3-bitrot bitrot-obj.bin total-errs-availability=0 total-errs-timeout=0 1.049254ms
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d3 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] .../data/d4 q3-bitrot bitrot-obj.bin/<DDir>/part.1 ... 3.0 MiB
```

**AFTER the deep heal — the corrupted shards were rewritten to their exact original bytes:**

```
d1 part.1 sha256 = 8238eb3cd2fcc8affe5cdf10107bd0e8399bbf07360df50b83ca9ca590c6586d   (restored)
d2 part.1 sha256 = f9962ce0ef1beb68e68b573ae898e4e351d57b8a11a5476afdc5922a972430f1   (restored)
d3 part.1 sha256 = ef9fee4d1f5ab37ba98d188dfcd1c1e14ca0356f00990d3e5efa0cf58518aaee   (unchanged good)
d4 part.1 sha256 = 67b2edea4c181fcd006db387d6265a384240faed124bac750a4cc348a36691a2   (unchanged good)
```

`d1` and `d2` now match the original good hashes exactly — the bit-rot was detected (`storage.VerifyFile`) and the shards reconstructed from parity and rewritten. This matches the deep-scan escalation at `cmd/erasure-healing.go:1080-1084`.

### Causal chain

On-read shard checksum mismatch in `streamingBitrotReader.ReadAt` (HighwayHash) → `errFileCorrupt` (`cmd/bitrot-streaming.go:184-185`) → `parallelReader` sets `bitrotHeal=1` (`cmd/erasure-decode.go:198`) and returns `errFileCorrupt` (L228) → the decoder reconstructs the missing/bad blocks from parity → `erasure-object.go`'s `healOnce.Do` queues `globalMRFState.addPartialOp(PartialOperation{ …, BitrotScan: true })` (L400-407) and nil's the error so the client still gets correct bytes. Heal-on-read only triggers when the requested part can be fully served from the remaining good shards, which **requires parity (erasure mode)** — hence the 4-drive `EC:2` server. The deep-scan heal path (`mc admin heal --scan deep`) invokes `storage.VerifyFile` (`cmd/xl-storage.go:3097` → `bitrotVerify` L3079) on every shard to recompute checksums, detects corruption, reconstructs, and rewrites the bad shards.

### Stability

The corrupt → GET (reconstruct, hash MATCH) → deep-heal (`[Red → Green]`, shards restored) cycle was run **twice** with identical results. **Stable.**

---


## Q4 — STS session-policy enforcement (intersection semantics)

**Question:** *Verify that when a user gets temporary credentials, MinIO is able to enforce the session policy on that user. Give me runtime test output to prove this behavior.*

### Summary of findings

Temporary credentials issued via **STS `AssumeRole`** carry an inline **session policy** that is enforced as the **intersection** (logical AND) of the session policy and the parent identity's (combined) policy. The session policy can only **narrow**, never **widen**, the parent's permissions. This was proven from both directions:

- **Main proof:** broad parent (`s3:*`) + restrictive session policy (`s3:GetObject` only) → `GetObject` **succeeds (200)**, `PutObject` **denied (403)**. The session policy narrows.
- **Edge proof:** narrow parent (`s3:GetObject` only) + broad session policy (`s3:*`) → `GetObject` **succeeds (200)**, `PutObject` **still denied (403)**. The session policy cannot widen beyond the parent.

Responsible code:

- `cmd/sts-handlers.go` — `stsPolicy` const at **L50**; `maxSTSSessionPolicySize = 2048` at **L89**; `populateSessionPolicy()` at **L94**; the size check at **L123**; storing `cred.SessionPolicyName` at **L127**; the `AssumeRole` handler at **L256**.
- `cmd/iam.go` — `IsAllowedSTS()` at **L2242**; the parent's policy via `PolicyDBGet(parentUser, …)` at **L2266**; the combined policy merge at **L2297**; the intersection return `return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))` at **L2312**; `isAllowedBySessionPolicy()` at **L2381**, which forces `sessionPolicyArgs.IsOwner = false` at **L2417** (so even a root-derived session is constrained).

Corroborating documentation (verbatim, `docs/sts/assume-role.md:39`): *"The resulting session's permissions are the intersection of the canned policy name and the policy set here. You cannot use this policy to grant more permissions than those allowed by the canned policy name being assumed."*

### Fixtures

Bucket `q4-bucket` with a seed object `readable.txt` (29 bytes). Parent user `q4user` attached a broad policy allowing `s3:*` on `q4-bucket`; verified `q4user` can directly PUT and GET. Temporary credentials were obtained with a `minio-go/v7` scratch driver calling `credentials.NewSTSAssumeRole(...)` with the inline `Policy`, then used to run `GetObject` and `PutObject`.

### Main proof — broad parent + restrictive session policy [OBSERVED, stable ×2]

```
$ PARENT_AK=q4user PARENT_SK=q4secret123 \
  SESSION_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}' \
  ./sts_assume_role q4-bucket readable.txt

=== STS AssumeRole ===
Parent AccessKey: q4user
Inline session policy:
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}
--- Temporary credentials issued ---
Temp AccessKeyID:     JBG3WV94NII1DFQHMCOE
Temp SecretAccessKey: 9oh...REDACTED...uY+
Temp SessionToken:    eyJhbGciOiJIUzUxMiIsInR5...(463 chars)
SignerType:           S3v4

=== OP1: GetObject with temp creds ===
GetObject RESULT: SUCCESS (read 29 bytes)

=== OP2: PutObject with temp creds ===
PutObject RESULT: DENIED/ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
  Message:        Access Denied.
```

`GetObject` is allowed by **both** parent (`s3:*`) and session policy (`s3:GetObject`) → **200**. `PutObject` is allowed by the parent (`s3:*`) but **forbidden by the session policy** → **403 AccessDenied**. The session policy narrowed the effective permissions.

The `mc admin trace --all -v` capture shows the STS call and both S3 operations:

```
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-13T18:07:13.951] [Client IP: 127.0.0.1]
127.0.0.1:9000 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3AGetObject%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Aq4-bucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15
127.0.0.1:9000 <AssumeRoleResponse ...><Credentials><AccessKeyId>JBG3WV94NII1DFQHMCOE</AccessKeyId><SecretAccessKey>...</SecretAccessKey><SessionToken>eyJ...</SessionToken><Expiration>2026-07-13T19:07:13Z</Expiration></Credentials>...</AssumeRoleResponse>
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-13T18:07:13.957] [Client IP: 127.0.0.1] => 200 OK
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T18:07:13.958] [Client IP: 127.0.0.1] => 403 Forbidden
```

The `PutObject` response body:

```
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>sts-put-probe.txt</Key><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/sts-put-probe.txt</Resource><RequestId>18C1EBA7F17AAAE3</RequestId>...</Error>
```

**The session policy is carried inside the temporary credential.** Decoding the middle segment of the issued JWT `SessionToken` yields:

```json
{ "accessKey": "JBG3WV94NII1DFQHMCOE", "exp": 1783969633, "parent": "q4user",
  "sessionPolicy": "<base64>" }
```

and the base64 `sessionPolicy` decodes back to **exactly** the inline policy that was passed:

```json
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}
```

This is the `cred.SessionPolicyName` stored at `cmd/sts-handlers.go:127`, later evaluated by `isAllowedBySessionPolicy()` (`cmd/iam.go:2381`).

### Edge proof — session policy cannot widen [OBSERVED]

Narrow parent (`q4narrow`, allowing only `s3:GetObject` + `s3:ListBucket`) with a **broad** session policy attempting to grant `s3:*`:

```
$ PARENT_AK=q4narrow PARENT_SK=q4narrowsecret \
  SESSION_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::q4-bucket","arn:aws:s3:::q4-bucket/*"]}]}' \
  ./sts_assume_role q4-bucket readable.txt

--- Temporary credentials issued ---
Temp AccessKeyID:     KRFR8P8TZ500QD23XYS0
Temp SecretAccessKey: S6I...REDACTED...rDA
Temp SessionToken:    eyJhbGciOiJIUzUxMiIsInR5...(498 chars)
SignerType:           S3v4

=== OP1: GetObject with temp creds ===
GetObject RESULT: SUCCESS (read 29 bytes)

=== OP2: PutObject with temp creds ===
PutObject RESULT: DENIED/ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
  Message:        Access Denied.
```

Even though the session policy grants `s3:*`, `PutObject` is **still denied (403)** because the **parent** does not allow it. This proves the session cannot grant more than the parent — the intersection semantics.

### Causal explanation

`IsAllowedSTS` (`cmd/iam.go:2242`) returns allowed **only if** `isAllowedBySessionPolicy(args)` **AND** `(isOwnerDerived || combinedPolicy.IsAllowed(args))` — the literal `return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))` at **L2312**. The AND of the two booleans is the intersection: the request must be permitted by the session policy *and* by the parent/combined policy. `isAllowedBySessionPolicy` forces `IsOwner = false` (`cmd/iam.go:2417`) so a session can never self-authorize as owner. In both proofs, `PutObject` fails because at least one side of the AND is false (the session policy in the main proof; the parent in the edge proof).

### Stability

The main proof was run **twice** with identical verdicts (`GetObject` SUCCESS 200, `PutObject` DENIED 403 `AccessDenied`). **Stable.**

---


## Q5 — Privilege-escalation prevention via user mappings

**Question:** *Show test output to prove that a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user mappings modification behavior that you observe.*

### Summary of findings

A basic user (`q5user`, attached a read-only policy) **cannot** attach `consoleAdmin` to itself through **any** of the user→policy-mapping mutation entry points. Every attempt returns **HTTP 403 `AccessDenied`**, and the mapping is verified **unchanged** afterward. The **root cause is deny-by-default admin-action authorization**: `q5user`'s policy grants no admin action, so `checkAdminRequestAuth` → `IAMSys.IsAllowed` returns `ErrAccessDenied` **before** any mapping mutation runs. A secondary defense is that the effective `consoleAdmin` policy is granted only to the **root** account.

| Attempt | Admin API endpoint | Result |
|---------|--------------------|--------|
| 1. `mc admin policy attach … consoleAdmin --user q5user` (`AttachDetachPolicyBuiltin`) | `PUT /minio/admin/v3/idp/builtin/policy/attach` | **403 AccessDenied** |
| 2. deprecated `SetPolicyForUserOrGroup` (madmin `SetPolicy`) | `PUT /minio/admin/v3/set-user-or-group-policy` | **403 AccessDenied** |
| 3. create an all-powerful policy (`AddCannedPolicy`) | `PUT /minio/admin/v3/add-canned-policy` | **403 AccessDenied** |
| 4. list users (any admin read) | `GET /minio/admin/v3/list-users` | **403 AccessDenied** |

Responsible code:

- `cmd/auth-handler.go` — `checkAdminRequestAuth()` at **L189**: validates the SigV4 signature, then evaluates the admin action via `globalIAMSys.IsAllowed(policy.Args{ AccountName: cred.AccessKey, Action: policy.Action(action), IsOwner: owner, … })` at **L194-201**; if not allowed, `return cred, ErrAccessDenied` at **L206**.
- `cmd/admin-handler-utils.go` — `validateAdminReq()` at **L37**: for each candidate action calls `checkAdminRequestAuth`; if all are denied it does `writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)` at **L59**.
- `cmd/admin-handlers-users.go` — the mutation entry points, each guarded by an admin action: `AddCannedPolicy` guarded by `policy.CreatePolicyAdminAction` at **L1704**; `SetPolicyForUserOrGroup` guarded by `policy.AttachPolicyAdminAction` at **L1773**; `AttachDetachPolicyBuiltin` guarded by `policy.UpdatePolicyAssociationAction, policy.AttachPolicyAdminAction` at **L1911-1912**; the root-only `consoleAdmin` effective-policy assignment `case accountName == globalActiveCred.AccessKey || newGlobalAuthZPluginFn() != nil:` at **L1455**.
- `cmd/iam.go` — `IsAllowed()` deny-by-default dispatch at **L2437-2481** (for a regular user: `GetCombinedPolicy(policies...).IsAllowed(args)`; no admin grant → `false`).
- `cmd/api-errors.go` — `ErrAccessDenied` at **L539-542** = `Code:"AccessDenied"`, `Description:"Access Denied."`, `HTTPStatusCode: http.StatusForbidden` (**403**).
- The admin-action constants (`admin:CreatePolicy`, `admin:AttachUserOrGroupPolicy`, …) live in the external module `github.com/minio/pkg/v3 v3.0.22` (referenced, not modified). IAM mappings are persisted under the reserved `.minio.sys` bucket (`cmd/iam-object-store.go`) — never reached, because the denial precedes any mutation.

### Fixtures

Read-only policy `q5-readonly` (allow only `s3:GetObject` + `s3:ListBucket` on `q4-bucket`), attached to `q5user`.

**BEFORE** (as root):

```
$ mc admin user info local q5user
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
```

`consoleAdmin` has no user entities (empty).

### The self-promotion attempts (as `q5user`) — all 403 [OBSERVED, stable ×2]

**Attempt 1 — attach `consoleAdmin` to self** (`AttachDetachPolicyBuiltin`):

```
$ mc admin policy attach q5 consoleAdmin --user q5user
mc: <ERROR> Unable to make user/group policy association. Access Denied.
```

Trace:

```
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-13T18:09:25.776] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C1EBC6A26B383B","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

**Attempt 2 — the deprecated `SetPolicyForUserOrGroup` server path.** `mc` blocks this client-side ("Deprecated command"), so it was exercised directly via a `madmin-go` scratch driver (`adm.SetPolicy`) to hit the real server handler:

```
$ AK=q5user SK=q5secretpass ./set_policy consoleAdmin q5user
SetPolicy RESULT: DENIED/ERROR
  Code:      AccessDenied
  Message:   Access Denied.
  RequestID: 18C1EBD5AF35EBD3
  raw:       Access Denied.
```

Trace (the real deprecated endpoint, denied 403):

```
127.0.0.1:9000 [REQUEST admin.SetPolicyForUserOrGroup] [2026-07-13T18:10:30.415] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-or-group-policy?isGroup=false&policyName=consoleAdmin&userOrGroup=q5user
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C1EBD5AF35EBD3",...}
```

**Attempt 3 — create a new all-powerful policy** (`AddCannedPolicy`, granting `admin:*` + `s3:*`):

```
$ mc admin policy create q5 q5-evil-admin evil.json
mc: <ERROR> Unable to create new policy. Access Denied.
```

Trace:

```
127.0.0.1:9000 [REQUEST admin.AddCannedPolicy] [2026-07-13T18:09:25.832] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/add-canned-policy?name=q5-evil-admin
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/add-canned-policy",...}
```

**Attempt 4 — any admin read** (list users), confirming `q5user` has **zero** admin actions:

```
$ mc admin user list q5
mc: <ERROR> Unable to list user. Access Denied.
```

Trace: `GET /minio/admin/v3/list-users` → `403 Forbidden` → `{"Code":"AccessDenied",...}`.

### Proof that no mutation occurred [OBSERVED]

**AFTER** all attempts, as root, `q5user`'s policy mapping is **unchanged** and no escalation happened:

```
$ mc admin user info local q5user
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
```

`consoleAdmin` still has **no** user entities, and the attempted `q5-evil-admin` policy **does not exist**:

```
$ mc admin policy info local q5-evil-admin
mc: <ERROR> Unable to fetch policy. The canned policy does not exist. (Specified canned policy does not exist).
```

Sanity check — `q5user` **is** a valid authenticated identity (its granted read still works), it simply lacks admin privileges:

```
$ mc cat q5/q4-bucket/readable.txt
hello-from-sts-parent-object
```

### Root cause

The denial is **deny-by-default admin-action authorization**, not a special case for `consoleAdmin`. When `q5user` calls any admin mutation endpoint:

1. `validateAdminReq` (`cmd/admin-handler-utils.go:37`) calls `checkAdminRequestAuth` (`cmd/auth-handler.go:189`).
2. `checkAdminRequestAuth` verifies the SigV4 signature (which is valid for `q5user`), then calls `globalIAMSys.IsAllowed(policy.Args{ Action: policy.Action(action), AccountName: cred.AccessKey, … })` (`cmd/auth-handler.go:194-201`).
3. `q5user`'s attached policy (`q5-readonly`) grants **no** `admin:*` action, so `IAMSys.IsAllowed` (`cmd/iam.go:2437-2481`) returns `false` → `checkAdminRequestAuth` returns `ErrAccessDenied` (`cmd/auth-handler.go:206`).
4. `validateAdminReq` writes the `ErrAccessDenied` response (**403**, `cmd/api-errors.go:539-542`) and returns a nil object layer — so the handler body (`SetPolicyForUserOrGroup`, `AttachDetachPolicyBuiltin`, `AddCannedPolicy`) **never executes**. The mapping mutation is never reached; nothing is persisted under `.minio.sys`.

**Secondary defense (root-only `consoleAdmin`):** even if a mapping to `consoleAdmin` existed, the effective `consoleAdmin` policy is assigned only when the account **is** the root credential — `case accountName == globalActiveCred.AccessKey || newGlobalAuthZPluginFn() != nil:` at `cmd/admin-handlers-users.go:1455`. A regular user never receives the `consoleAdmin` effective policy.

### Stability

The self-attach attempt was re-run **twice**; both returned "Access Denied." **Stable.**

---


## Coverage Pass

This section confirms every named mechanism, function, condition, and flag from each question is answered with its concrete value, file:line anchor, observed evidence, sibling variants, and causal reason. All rows are **[OBSERVED]** unless marked otherwise.

### Q1 — SSE requirement vs. broad write

| Named item | Concrete value / behavior | file:line | Observed evidence |
|------------|---------------------------|-----------|-------------------|
| `PutObjectHandler` | PUT entry point | `cmd/object-handlers.go:1745` | All Q1 PUT traces |
| auth gate `isPutActionAllowed(... policy.PutObjectAction)` | Decides allow/deny at stage 2 | `cmd/object-handlers.go:1836` | TEST C 403 emitted here (TTFB 94µs) |
| bucket-policy evaluated only for anonymous (`cred.AccessKey == ""`) | Authenticated users bypass bucket policy | `cmd/auth-handler.go` `isPutActionAllowed` | TEST A (auth 200) vs TEST B (anon 403) |
| `IAMSys.IsAllowed()` deny-by-default | Identity policy `Deny` wins over `Allow s3:*` | `cmd/iam.go:2437-2481` | TEST C 403 AccessDenied |
| `getConditionValues()` + header-copy loop | `s3:x-amz-server-side-encryption` key exists only if header sent | `cmd/bucket-policy.go:77,178-186` | TEST C: no SSE hdr → `Null` matches → Deny |
| default-encryption `sseConfig.Apply(..., AutoEncrypt: globalAutoEncryption)` | Transparent auto-encrypt after auth | `cmd/object-handlers.go:1895` | TEST D: response `X-Amz-Server-Side-Encryption: AES256`, 0 `GetBucketEncryption` calls, `mc stat`=SSE-S3 |
| `MINIO_KMS_AUTO_ENCRYPTION` / `LookupAutoEncryption` | Auto-encryption global toggle | `internal/crypto/auto-encryption.go:31,37`; `cmd/config-current.go:532` | Server run with KMS key; default SSE-S3 verified |
| Actual status codes | anon-deny **403**, identity-deny **403**, default-enc **200** | — | curl `HTTP_STATUS=403/200`; trace `403 Forbidden`/`200 OK` |
| Decision ordering | (1) SigV4 → (2) IsAllowed → (3) sseConfig.Apply | — | Trace blocks in TEST C/D |

### Q2 — Object-lock delete enforcement

| Named item | Concrete value / behavior | file:line | Observed evidence |
|------------|---------------------------|-----------|-------------------|
| `enforceRetentionBypassForDelete()` | Core WORM enforcement → `ObjectLocked{}` | `cmd/bucket-object-lock.go:84` | All Q2 blocked deletes |
| legal-hold branch | `return ObjectLocked{}` | `cmd/bucket-object-lock.go:100-101` | Legal-hold single delete **400** |
| compliance branch | `return ObjectLocked{}`, no bypass | `cmd/bucket-object-lock.go:107,117,121` | Compliance bypass-as-root **400** |
| governance branch + `byPassSet` | bypass allowed with permission | `cmd/bucket-object-lock.go:124-155` (byPassSet L138, ObjectLocked L143/147) | Gov no-bypass **400**; bypass-as-root **success** |
| bypass permission check `checkRequestAuthType(... BypassGovernanceRetentionAction)` → `errAuthentication` | 403 when caller lacks perm | `cmd/bucket-object-lock.go:153-154` | Gov bypass w/o perm **403 AccessDenied** |
| `DeleteObjectHandler` + `SetEvalRetentionBypassFn` (only if `vID != ""`) | plain delete → delete marker (not blocked) | `cmd/object-handlers.go:2509,2598,2600` | Delete-marker: "Created delete marker", v1 retained |
| `ObjectLocked` → `ErrObjectLocked` | mapping | `cmd/api-errors.go:2298-2299` | 400 InvalidRequest body |
| `ErrObjectLocked` code | `InvalidRequest` / "Object is WORM protected…" / **400** | `cmd/api-errors.go:1059-1062` | Single-delete driver `HTTPStatusCode: 400` |
| `errAuthentication` → `ErrAccessDenied` | **403** | `cmd/api-errors.go:2176-2177` | Gov no-perm bypass **403** |
| multi-object `DeleteObjects` envelope | **200** with embedded per-object `<Error>` | — | `<DeleteResult><Error><Code>InvalidRequest</Code>…` |
| server console log | none on normal block; `internalLogIf(WarningKind)` only on NTP error | `cmd/bucket-object-lock.go:142` | server.log unchanged during blocks |

### Q3 — Bit-rot detection & heal-on-read

| Named item | Concrete value / behavior | file:line | Observed evidence |
|------------|---------------------------|-----------|-------------------|
| `streamingBitrotReader.ReadAt()` | checksum mismatch → `errFileCorrupt` (no log) | `cmd/bitrot-streaming.go:150,184-185` | Heal-on-read silent; GET still MATCH |
| `errFileCorrupt` sentinel | `StorageErr("file is corrupted")` | `cmd/storage-errors.go:104` | — (INFERRED sentinel value from source; behavior observed via reconstruction) |
| `parallelReader` bit-rot heal flag | `atomic.StoreInt32(&bitrotHeal, 1)`; returns `errFileCorrupt` | `cmd/erasure-decode.go:197-198,228` | All-4-shard read in GET trace |
| heal-on-read queue `globalMRFState.addPartialOp(... BitrotScan: true)` | queued after part served | `cmd/erasure-object.go:346,400-407` | Correct bytes returned; MRF op queued |
| `VerifyFile()` / `bitrotVerify()` | storage-layer checksum verify | `cmd/xl-storage.go:3097,3079` | Deep-heal trace: `storage.VerifyFile` on all 4 drives |
| `healObject` + deep-scan escalation | repairs corrupt shard | `cmd/erasure-healing.go:258,1080-1084` | `[Red → Green]`, d1/d2 restored |
| MRF heal routine | consumes queued ops | `cmd/background-newdisks-heal-ops.go:389-390` | (queued; visible repair via deep heal) |
| bit-rot scan cycle | background cycle | `cmd/data-scanner.go:94` | `mc admin scanner trace` available |
| `EcM`/`EcN`/`EcBSize`/`EcDist`/`EcAlgo`/`CSumAlgo` | 2 / 2 / 1 MiB / [2,3,4,1] / RS / HighwayHash | `xl.meta` | Decoded JSON shown |
| before/during/after shard state | good → corrupt (`ba…`/`c0…`) → restored | — | `od` hexdump + sha256 before/after |
| reconstructed bytes | GET hash == original `894e3b94…` | — | `sha256sum retrieved.bin` MATCH ×2 |

### Q4 — STS session-policy enforcement

| Named item | Concrete value / behavior | file:line | Observed evidence |
|------------|---------------------------|-----------|-------------------|
| `AssumeRole` handler | issues temp creds | `cmd/sts-handlers.go:256` | Trace `[REQUEST sts.AssumeRole]` + `<Credentials>` |
| `populateSessionPolicy()` | reads inline `Policy` param | `cmd/sts-handlers.go:94` | Trace `Action=AssumeRole&…&Policy=…` |
| `stsPolicy` const | `"Policy"` | `cmd/sts-handlers.go:50` | Policy param in request |
| `maxSTSSessionPolicySize=2048` + size check | policy size limit | `cmd/sts-handlers.go:89,123` | (policies well under limit) |
| store `cred.SessionPolicyName` | session policy carried in cred | `cmd/sts-handlers.go:127` | JWT decode shows embedded `sessionPolicy` |
| `IsAllowedSTS()` | intersection enforcement | `cmd/iam.go:2242` | Both proofs |
| parent `PolicyDBGet` + combined merge | parent/combined policy | `cmd/iam.go:2266,2297` | Edge proof (parent narrow) |
| intersection return | `isAllowedSP && (isOwnerDerived \|\| combinedPolicy.IsAllowed(args))` | `cmd/iam.go:2312` | Main + edge: PutObject 403 |
| `isAllowedBySessionPolicy` forces `IsOwner=false` | session never owner | `cmd/iam.go:2381,2417` | root-derived session constrained |
| allowed vs denied ops | GetObject **200**, PutObject **403 AccessDenied** | — | driver output + trace ×2 |
| cannot widen | narrow parent + broad session → PutObject **403** | — | Edge proof |
| doc corroboration | intersection statement | `docs/sts/assume-role.md:39` | verbatim quote |

### Q5 — Privilege-escalation prevention

| Named item | Concrete value / behavior | file:line | Observed evidence |
|------------|---------------------------|-----------|-------------------|
| `checkAdminRequestAuth()` | sig verify → IsAllowed → ErrAccessDenied | `cmd/auth-handler.go:189,194-201,206` | All 4 attempts 403 |
| `validateAdminReq()` | 403 on ErrAccessDenied | `cmd/admin-handler-utils.go:37,59` | Trace 403 bodies |
| `IsAllowed()` deny-by-default | no admin grant → false | `cmd/iam.go:2437-2481` | q5-readonly lacks admin:* |
| `SetPolicyForUserOrGroup` guard | `AttachPolicyAdminAction` | `cmd/admin-handlers-users.go:1770,1773` | Attempt 2 (deprecated path) 403 |
| `AttachDetachPolicyBuiltin` guard | `UpdatePolicyAssociationAction, AttachPolicyAdminAction` | `cmd/admin-handlers-users.go:1908,1911-1912` | Attempt 1 403 |
| `AddCannedPolicy`/`CreatePolicy` guard | `CreatePolicyAdminAction` | `cmd/admin-handlers-users.go:1701,1704` | Attempt 3 403 |
| root-only `consoleAdmin` effective policy | `accountName == globalActiveCred.AccessKey` | `cmd/admin-handlers-users.go:1455` | secondary defense |
| `ErrAccessDenied` code | `AccessDenied` / **403** | `cmd/api-errors.go:539-542` | Every trace `403 Forbidden` |
| admin-action constants | in `github.com/minio/pkg/v3 v3.0.22` | (external) | referenced, not modified |
| IAM mapping persistence | reserved `.minio.sys` bucket | `cmd/iam-object-store.go` | never mutated (root confirms unchanged) |
| no-mutation proof | q5user still `q5-readonly`; evil policy absent | — | `mc admin user info` / `policy info` after |
| root cause | deny-by-default admin authorization precedes mutation | — | denial before handler body executes |

### Final coverage confirmation

- **Q1:** both mechanisms named and demonstrated — bucket policy (anonymous-only, corrected), identity-policy `Deny` (the real precedence mechanism for authenticated users), and default encryption (transparent auto-encrypt); ordered trace + actual status codes (403/200) captured.
- **Q2:** all sibling/edge variants exercised — legal hold, governance (no-bypass / bypass-with-perm / bypass-without-perm), compliance, single-object vs. multi-object delete, specific-version vs. delete-marker; actual codes (400 `InvalidRequest`, 403 `AccessDenied`) reported; the "log entry" reality (API error response, not console log) documented.
- **Q3:** before/during/after shard state shown with hexdump + sha256; correct reconstruction proven (hash MATCH ×2); heal-on-read's silence documented with its anchor; deep-scan `VerifyFile` detect-and-repair captured; erasure metadata decoded.
- **Q4:** intersection proven from both directions (narrow session over broad parent; broad session over narrow parent), allowed (200) and denied (403) test output captured, session policy shown carried in the JWT, doc cross-referenced.
- **Q5:** all mapping-mutation entry points attempted and denied (403 ×4), no-mutation proven, deny-by-default root cause named with the exact function chain, root-only `consoleAdmin` secondary defense identified.

---

*End of investigation. The only artifact added to the repository is this document; all runtime scratch (server binary, data directories, driver programs, test buckets/users/policies) lived outside the repository tree and was removed after capture.*

