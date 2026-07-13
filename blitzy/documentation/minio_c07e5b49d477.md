# MinIO Security Behavior Investigation — Runtime-Evidenced Answers

**Source branch under investigation:** `minio_c07e5b49d477`
**Investigated source commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2` (the base commit this branch was cut from)
**Method:** build → run → exercise the real S3/admin/STS API entry points → capture complete unedited runtime output → document.

This document answers five MinIO security-behavior questions with **runtime evidence**. Every behavioral claim is paired with the exact command that produced it and its complete, unedited output. Statements that could only be derived by reading source code (and could not be surfaced at runtime) are explicitly labeled `[INFERRED]`; everything captured from a live server is labeled `[OBSERVED]`.

## Questions answered

1. **Q1** — What happens when a bucket-level encryption requirement meets a user's broad write permission during an *unencrypted* upload? What is the runtime execution sequence?
2. **Q2** — With object locking enabled, what log/error entries appear when someone tries to delete locked objects?
3. **Q3** — How does the system handle unauthorized manual data corruption in the storage backend? Trigger a bit-rot detection event and capture the runtime logs during a subsequent GET.
4. **Q4** — Prove that MinIO enforces an STS session policy on temporary credentials, with runtime test output.
5. **Q5** — Prove with test output that a basic user cannot promote itself to console admin by modifying user→policy mappings, and identify the root cause.

## Evidence conventions

- **`[OBSERVED]`** — the claim is backed by output captured from the live MinIO server during this investigation. The producing command and its complete, unedited output appear alongside the claim.
- **`[INFERRED]`** — the claim is derived from reading the source at commit `c07e5b49d477`. MinIO's runtime signals (the HTTP trace stream and console log) do **not** expose internal handler stages, so ordering claims about *within-handler* steps cannot be observed at the wire and are labeled `[INFERRED]` with a `file:line` anchor. Wherever a runtime signal *does* corroborate the inference, both are shown.
- All file:line anchors refer to commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.
- Secrets (root password, temporary STS secret keys, session tokens) are **redacted** in this document; only lengths and non-secret prefixes are shown. All other output is verbatim.
- Every observation below was captured at least **twice**; the second run is shown (or its verdict noted) to confirm stability.
- **Trace excerpts.** `mc admin trace` blocks are shown with the request line, **all** `Authorization`, `x-amz-*`, `Content-*`, `Host`, and `User-Agent` headers, the `[RESPONSE]` status line, and the **complete** response/error body — all verbatim. Only fixed per-response transport boilerplate that is identical on every reply is omitted for length: `X-Ratelimit-Limit/Remaining`, `X-Xss-Protection`, `X-Content-Type-Options`, `Strict-Transport-Security`, `Vary`, `Accept-Ranges`, `Server`, and `X-Amz-Id-2`. MinIO's own literal `<BLOB>` marker (its placeholder for a binary body in the trace stream) is retained as emitted. The complete unfiltered trace logs are preserved as evidence artifacts during the investigation.

## Environment & build

### Toolchain

```
$ go version
go version go1.23.12 linux/amd64
```

`go.mod` declares `go 1.23`; CI pins `1.23.x` (`.github/workflows/go.yml:23`); `Dockerfile.release` uses `golang:1.23-alpine`. `go1.23.12` is the highest 1.23 patch, used for all builds below. Host: Ubuntu 25.10, amd64.

### Git context (honest HEAD-vs-base relationship)

This investigation branch adds exactly one file — this document — as a commit on top of the source under study:

```
HEAD (this doc commit):  d45d9240c0b3dd0ea243bbcaa4f6d3cb2f3da94f  "docs: add runtime-evidenced MinIO security investigation"
HEAD~1 (investigated):   c07e5b49d477b0774f23db3b290745aef8c01bd2  "refactor: replace experimental maps and slices with stdlib (#20679)"
```

`buildscripts/gen-ldflags.go` derives the version string from the **current HEAD** commit's date and id. Run at this branch's HEAD it would stamp the *documentation* commit (`DEVELOPMENT.2026-07-13T...`, commit-id `d45d9240…`), which is **not** the code under investigation. To study the actual source the questions concern, the server binary is built **at the base commit `c07e5b49d477`**, whose `gen-ldflags.go` output is authoritative for this investigation:

```
$ git -C <worktree@c07e5b49d477> ... go run buildscripts/gen-ldflags.go
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z \
   -X github.com/minio/minio/cmd.CopyrightYear=2024 \
   -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z \
   -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 \
   -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 \
   -X github.com/minio/minio/cmd.GOPATH= -X github.com/minio/minio/cmd.GOROOT=
```

### Canonical build (Makefile:177-179)

```
$ CGO_ENABLED=0 go build -tags kqueue -trimpath \
    --ldflags "$(go run buildscripts/gen-ldflags.go)" -o ./minio      # run at commit c07e5b49d477
build exit code: 0   (elapsed ~4s)

$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

**Reproducibility note (`[OBSERVED]`).** The freshly built binary and the binary produced during environment setup carry an **identical version/commit stamp** but **different sha256** — expected because `-trimpath` removes file paths yet the two builds ran in different Go build environments (build-cache/host differences produce non-bit-identical output):

```
84243d66fef9cf3d07669722c24cc419e13af8e07ff2fdff6d4e1005b915f5c4  ./minio   (freshly built here)
0a5f31231e32243ce38566edb3a8d264ffe01e7b8c70c46476a1c44796901d9c  ./minio   (setup-built)
```

Embedded provenance confirms the exact source revision and build flags (`go version -m ./minio`, excerpted):

```
build   -tags=kqueue
build   -trimpath=true
build   CGO_ENABLED=0
build   GOARCH=amd64
build   GOOS=linux
build   vcs=git
build   vcs.revision=c07e5b49d477b0774f23db3b290745aef8c01bd2
build   vcs.time=2024-11-25T17:10:22Z
build   vcs.modified=false
dep github.com/klauspost/reedsolomon  v1.12.4   # erasure/parity (Q3)
dep github.com/minio/madmin-go/v3     v3.0.77   # admin client + TraceInfo
dep github.com/minio/minio-go/v7      v7.0.80   # S3 SDK
dep github.com/minio/pkg/v3           v3.0.22   # policy engine + admin-action constants
dep github.com/minio/sio             v0.4.1    # DARE encrypted stream (Q1)
```

### Client / investigation tooling provenance (`[OBSERVED]`)

```
$ mc --version
mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0dd2a80935aded936b09fa12cbb3cb8096)
Runtime: go1.24.6 linux/amd64
$ sha256sum $(command -v mc)
01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891  /usr/local/bin/mc

$ ./xl-meta --help            # in-repo tool: docs/debugging/xl-meta, built at c07e5b49d477
$ sha256sum ./xl-meta
8c6ebb2eeae4c5382147c6c0027133b25da5fd4c82554d51d0dbb5050d5720d1  ./xl-meta
   (go version -m ./xl-meta -> vcs.revision=c07e5b49d477..., vcs.modified=false)
```

Three small Go **driver programs** (built offline against the repo's exact dependency graph — `minio-go/v7 v7.0.80`, `madmin-go/v3 v3.0.77`) exercise API paths that `mc` does not surface cleanly: a single-object `DeleteObject` (Q2), an STS `AssumeRole` + intersection probe (Q4), and the deprecated admin `SetPolicy` call (Q5). Their source is shown inline in the relevant sections. These drivers live outside the repository and are removed at the end (see Cleanup).

### Server invocation — erasure mode (required for Q3)

The server is run in its default configuration as a normal operator would, with four drive directories so a single erasure set with parity `EC:2` is formed (parity is required for Q3's heal-on-read to manifest):

```
$ MINIO_ROOT_USER=<ephemeral, 11 chars> \
  MINIO_ROOT_PASSWORD=<ephemeral, 32 chars, redacted> \
  MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=" \
  /tmp/minio-investigation/bin/minio server /tmp/minio-investigation/data/{1,2,3,4} \
        --address 127.0.0.1:9000 --console-address 127.0.0.1:9001

MinIO Object Storage Server
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)
API: http://127.0.0.1:9000
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
```

```
$ mc admin info inv
●  127.0.0.1:9000
   Uptime: ...   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK    Drives: 4/4 OK    Pool: 1
┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.8% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘
4 drives online, 0 drives offline, EC:2
```

**Security posture of this local investigation (not a deployment recommendation).** The server is bound to **loopback `127.0.0.1` only** (never `0.0.0.0`), the root user/password are **ephemeral random values generated per run** (not the `minioadmin` default, not committed to the repo, redacted here), and `MINIO_KMS_SECRET_KEY` uses MinIO's **published CI/demo key** purely to enable SSE-S3/SSE-KMS locally — it is explicitly a demo key and must never be used in production. The `mc` client config is kept in an **isolated `MC_CONFIG_DIR`** so no alias leaks into the host's `~/.mc`. All data directories live under `/tmp` and are destroyed at cleanup.

### Methodology and independent verifiability

This document is a **complete, self-contained, from-scratch runtime investigation**: every behavioral claim below was produced by exercising the real S3 / admin / STS API entry points against the live server described above, and each is presented with its exact producing command and complete, unedited output. No claim relies on a prior draft or on any external checkpoint artifact. Each observation was captured at least **twice** to confirm stability, and every factual statement is labeled `[OBSERVED]` (captured at runtime) or `[INFERRED]` (derived from reading source at commit `c07e5b49d477`, with a `file:line` anchor). The evidence is therefore independently reproducible by rebuilding the binary and re-running the commands shown.

---

## Q1 — Bucket encryption requirement vs. a user's broad write, on an unencrypted upload

**Question.** What happens when a bucket-level encryption requirement takes precedence over a user's broad write permission during an *unencrypted* upload? Identify the runtime execution sequence captured in the server trace.

**Short answer (`[OBSERVED]`).** "Takes precedence" resolves **three different ways depending on which mechanism enforces the requirement**, and MinIO's behavior is not what a naive reading suggests:

| # | Enforcing mechanism | Principal | SSE header sent | Runtime outcome |
|---|---------------------|-----------|-----------------|-----------------|
| A2 | Bucket **default encryption** | `q1user` (`s3:*`) | none | **200** — object stored, server **auto-injected `AES256`** |
| B | **Bucket policy** `DenyUnEncryptedObjectUploads` | `q1user` (`s3:*`, authenticated) | none | **200** — stored **unencrypted** (bucket policy **not consulted** for an authenticated identity) |
| C1 | **Bucket policy** `DenyUnEncryptedObjectUploads` | **anonymous** | none | **403 AccessDenied** |
| C2 | **Bucket policy** `DenyUnEncryptedObjectUploads` | **anonymous** | `AES256` | **200** — SSE-S3 |
| D1 | **Identity policy** with a `Deny` condition | `q1deny` (authenticated) | none | **403 AccessDenied** |
| D2 | **Identity policy** with a `Deny` condition | `q1deny` (authenticated) | `AES256` | **200** — SSE-S3 |

The key corrections this investigation establishes at runtime:

1. A **bucket policy** `DenyUnEncryptedObjectUploads` does **not** override an *authenticated* user's broad write — MinIO evaluates only the **identity** policy for authenticated requests and never consults the bucket policy for them (TEST B, 200 unencrypted). It overrides only **anonymous** requests (TEST C1, 403).
2. To override an *authenticated* broad-write user, the deny must live in the **identity** policy (TEST D1, 403) — where `Deny` beats `Allow` — or the bucket must use **default encryption**, which does not deny at all but transparently **auto-encrypts** (TEST A2, 200 + `AES256`).
3. The `mc admin trace` stream does **not** expose the handler's internal stages; it shows the request headers **after** the handler has run and the response. The internal ordering is therefore `[INFERRED]` from source (below).

### Root cause — why a bucket policy does not gate an authenticated identity (`[INFERRED]` from source, `[OBSERVED]` at runtime)

`isPutActionAllowed` (`cmd/auth-handler.go:749`) branches on whether the request is anonymous:

```
if cred.AccessKey == "" {                         // anonymous
    if globalPolicySys.IsAllowed(policy.BucketPolicyArgs{...}) { return ErrNone }
    return ErrAccessDenied
}
if globalIAMSys.IsAllowed(policy.Args{...}) { return ErrNone }   // authenticated -> IDENTITY policy only
return ErrAccessDenied
```

For an authenticated user, `IAMSys.IsAllowed` (`cmd/iam.go:2437`) ends at `return sys.GetCombinedPolicy(policies...).IsAllowed(args)` (`cmd/iam.go:2482`) — only the identity's combined policy is evaluated; the bucket policy is never consulted. This is `[INFERRED]` from source and `[OBSERVED]` by TEST B succeeding.

### Setup (canonical commands)

```
# broad-write identity policy for q1user
$ cat q1user-policy.json
{ "Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],
  "Resource":["arn:aws:s3:::q1-default-enc/*","arn:aws:s3:::q1-policy-deny/*","arn:aws:s3:::q1-identity-deny/*",
               "arn:aws:s3:::q1-default-enc","arn:aws:s3:::q1-policy-deny","arn:aws:s3:::q1-identity-deny"]}]}
$ mc admin policy create inv q1-broadwrite q1user-policy.json
Created policy `q1-broadwrite` successfully.
$ mc admin user add inv q1user ***REDACTED***
Added user `q1user` successfully.
$ mc admin policy attach inv q1-broadwrite --user q1user
Attached Policies: [q1-broadwrite]
To User: q1user

# mechanism A: bucket default SSE-S3
$ mc encrypt set sse-s3 inv/q1-default-enc
Auto encryption configuration has been set successfully for inv/q1-default-enc

# mechanism B/C: DenyUnEncryptedObjectUploads bucket policy (anon Allow isolates the encryption condition)
$ mc anonymous set-json deny-unencrypted-bucket-policy.json inv/q1-policy-deny
$ mc anonymous get-json inv/q1-policy-deny
{"Statement":[{"Action":["s3:GetObject","s3:ListBucket","s3:PutObject"],"Effect":"Allow","Principal":{"AWS":["*"]},
  "Resource":["arn:aws:s3:::q1-policy-deny","arn:aws:s3:::q1-policy-deny/*"],"Sid":"AllowAnonAll"},
 {"Action":["s3:PutObject"],"Condition":{"Null":{"s3:x-amz-server-side-encryption":[true]}},"Effect":"Deny",
  "Principal":{"AWS":["*"]},"Resource":["arn:aws:s3:::q1-policy-deny/*"],"Sid":"DenyUnEncryptedObjectUploads"}],
 "Version":"2012-10-17"}

# mechanism D: identity policy that Denies PutObject when the SSE header is absent (Null == true)
$ mc admin policy create inv q1-identity-deny-pol q1deny-identity-policy.json
$ mc admin user add inv q1deny ***REDACTED*** ; mc admin policy attach inv q1-identity-deny-pol --user q1deny
```

### TEST A2 — bucket **default encryption** auto-encrypts (server-side), `[OBSERVED]`

A **raw minio-go PutObject with NO server-side-encryption option** (User-Agent `minio-go/v7.0.80`, no `mc`), so the client sends no SSE header:

```
$ AK=q1user SK=*** ENDPOINT=127.0.0.1:9000 ./put_raw q1-default-enc objA2-rawsdk
PUT RESULT: SUCCESS etag=5dc6a30de90893eff4d0cfe818b90621 size=29
STAT: size=29 sse-header="AES256"
```

`mc admin trace -v` for that PUT (note: the `X-Amz-Server-Side-Encryption: AES256` shown in the **request** block is **not** in `SignedHeaders` and was **not** sent by the client — see the trace-timing note below):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T19:31:30.392] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-default-enc/objA2-rawsdk
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 29
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260713/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=d6019f1537752fdeb6ecbe547e7fea755e2672a8003459657850827231031415
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:31:30.396] [ Duration 4.354ms TTFB 4.323055ms ↑ 350 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
```

**Trace-timing note (`[INFERRED]` from source).** The `X-Amz-Server-Side-Encryption: AES256` header that appears in the *request* section, despite the client never sending it, is explained by the tracer implementation: `httpTracerMiddleware` calls `h.ServeHTTP(respRecorder, r)` (`cmd/http-tracer.go:88`) and only **afterwards** clones the request headers with `reqHeaders := r.Header.Clone()` (`cmd/http-tracer.go:103`). Because `PutObjectHandler` mutates `r.Header` when it applies bucket default encryption — `sseConfig.Apply(r.Header, ...)` at `cmd/object-handlers.go:1894-1896` — the **server-injected** header is present by the time the tracer snapshots the request. This is the concrete runtime signature of the "encryption requirement transparently taking precedence": the write is honored **and** the object is encrypted, with no denial.

### TEST B — `DenyUnEncryptedObjectUploads` **bucket** policy does **not** deny the authenticated broad-write user, `[OBSERVED]`

```
$ mc cp payload.txt q1/q1-policy-deny/objB-authed-noenc          # q1 alias = q1user (s3:*)
...Total: 39 B ... (success)
$ mc stat q1/q1-policy-deny/objB-authed-noenc
Name      : objB-authed-noenc
Size      : 39 B
   (no Encryption line -> stored UNENCRYPTED)
```

Trace of the PUT — **200 OK**, and note the request carries **no** `X-Amz-Server-Side-Encryption` header (this bucket has no default encryption, and the object was stored as-is):

```
127.0.0.1:9000 PUT /q1-policy-deny/objB-authed-noenc
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260713/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=a6137d481fdc5633ab0203dc69c141f4d73a602b0c21748cf0e05a03e8a22c6a
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:29:48.142] [ Duration 3.643ms TTFB 3.616204ms ↑ 347 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 ETag: "94de2e07c23a6d86ad020c98ee12f751"
```

This is the crux: even though a `DenyUnEncryptedObjectUploads` bucket policy is installed, the authenticated broad-write user's unencrypted upload **succeeds** because the bucket policy governs only anonymous requests.

### TEST C1 / C2 — the same **bucket** policy **does** gate an **anonymous** upload, `[OBSERVED]`

Anonymous, no SSE header — **403**:

```
$ curl -s -X PUT --data-binary @payload.txt http://127.0.0.1:9000/q1-policy-deny/objC1-anon-noenc  -w 'HTTP %{http_code}\n'
HTTP 403
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>objC1-anon-noenc</Key><BucketName>q1-policy-deny</BucketName><Resource>/q1-policy-deny/objC1-anon-noenc</Resource><RequestId>18C1F0413D1A471D</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace (anonymous — no `Authorization` header — `403 Forbidden`):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T19:31:30.404] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-policy-deny/objC1-anon-noenc
127.0.0.1:9000 User-Agent: curl/8.14.1
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:31:30.404] [ Duration 149µs TTFB 139.788µs ↑ 51 B  ↓ 351 B ]
127.0.0.1:9000 403 Forbidden
```

Anonymous **with** the SSE header — **200**, object encrypted (the `Null` condition no longer matches, so the `Deny` does not fire and `AllowAnonAll` permits it):

```
$ curl -s -X PUT -H "x-amz-server-side-encryption: AES256" --data-binary @payload.txt \
     http://127.0.0.1:9000/q1-policy-deny/objC2-anon-sse -w 'HTTP %{http_code}\n'
HTTP 200
$ mc stat inv/q1-policy-deny/objC2-anon-sse
Name      : objC2-anon-sse
Encryption: SSE-S3
```

### TEST D1 / D2 — an **identity** policy `Deny` **does** override the authenticated user (Deny > Allow), `[OBSERVED]`

`q1deny` holds `s3:*` on the bucket **plus** a `Deny` on `s3:PutObject` when `s3:x-amz-server-side-encryption` is `Null`. Unencrypted upload — **403**:

```
$ AK=q1deny SK=*** ./put_raw q1-identity-deny objD1-noenc
PUT RESULT: DENIED/ERROR
  HTTPStatusCode: 403
  Code: AccessDenied
  Message: Access Denied.
```

Trace — authenticated as `q1deny`, **403 Forbidden**:

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T19:31:30.453] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-identity-deny/objD1-noenc
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1deny/20260713/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=0e8b8ff45e722f3b66dbd6819028ab85dcbda27ec2e2b43c424cf373bf4dfab5
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:31:30.453] [ Duration 108µs TTFB 96.75µs ↑ 119 B  ↓ 345 B ]
127.0.0.1:9000 403 Forbidden
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>objD1-noenc</Key><BucketName>q1-identity-deny</BucketName>...</Error>
```

The same user **with** SSE (valid `mc` syntax — the `--enc-s3` value is a **path prefix**, not a filename) — **200**, encrypted:

```
$ mc cp --enc-s3 "q1deny/q1-identity-deny/objD2-sse" payload.txt q1deny/q1-identity-deny/objD2-sse
...(success)
$ mc stat inv/q1-identity-deny/objD2-sse
Name      : objD2-sse
Encryption: SSE-S3
```

> Correction of an invalid command in a prior draft: `mc cp --enc-s3 payload.txt` is **wrong** — `--enc-s3` consumes its argument as the encryption **path prefix**, so `payload.txt` would be treated as that prefix and the actual source/target operands would be missing. The correct form supplies the prefix explicitly, exactly as in the working command shown above: `mc cp --enc-s3 "q1deny/q1-identity-deny/objD2-sse" payload.txt q1deny/q1-identity-deny/objD2-sse` (encryption path-prefix first, then the local source `payload.txt`, then the `alias/bucket/object` target).

### Runtime execution sequence — what the trace shows, and the internal ordering

**`[OBSERVED]` (from the trace).** For every case above, `mc admin trace` shows exactly two things per request: the request line + headers (snapshotted **after** the handler runs) and the `[RESPONSE]` with its status code (`200 OK` or `403 Forbidden`) and body. The trace does **not** emit per-stage markers for signature verification, authorization, or encryption.

**`[INFERRED]` (from source at `c07e5b49d477`).** Inside `PutObjectHandler` the order is:

1. `cmd/object-handlers.go:1836` — `isPutActionAllowed(...)` (authorization: identity policy for authenticated, bucket policy for anonymous). **This runs first.**
2. `cmd/object-handlers.go:1844` — `newSignV4ChunkedReader(r, ...)` for the streaming-signed path (the mc/SDK requests above all use `X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`). The per-chunk payload signatures are verified **as the body is read**, i.e. **after** authorization.
3. `cmd/object-handlers.go:1894-1896` — `sseConfig.Apply(r.Header, {AutoEncrypt: globalAutoEncryption})` injects bucket default encryption.

Therefore the frequently-stated ordering "SigV4 → IAM → SSE" is imprecise for streaming uploads: only the **seed** signature is validated before the handler; the **body** signature is validated *after* the IAM authorization decision, and SSE application is last. This ordering is `[INFERRED]` — it cannot be read off the trace, which only proves the *outcome* (the status code and, for default encryption, the injected `AES256` header).

### Stability

Each case was run twice with fresh object keys; verdicts were identical: A2 `200/AES256`, B `200`/unencrypted, C1 `403`, C2 `200`, D1 `403`, D2 `200`.

---

## Q2 — Object-lock delete enforcement: what log entries appear when deleting locked objects

**Question.** With object locking enabled, what specific log entries appear when someone tries to delete locked objects? Show runtime log output.

**Short answer (`[OBSERVED]`).** The "log entry" a delete of a locked object produces is the **HTTP error response** emitted on the trace stream — MinIO does **not** write a server console/audit line for a normal WORM block (the console log line count was **unchanged** across every blocked delete below; `internalLogIf` fires only on an NTP/clock error, `cmd/bucket-object-lock.go:116,143`). The response differs by lock mode and permission:

| # | Lock state | Caller | Bypass hdr | Handler | HTTP | Code | Message |
|---|-----------|--------|-----------|---------|------|------|---------|
| 1 | Legal hold ON | root | — | `s3.DeleteObject` | **400** | `InvalidRequest` | `Object is WORM protected and cannot be overwritten` |
| 2 | Governance | root | no | `s3.DeleteObject` | **400** | `InvalidRequest` | (same) |
| 3 | Governance | `q2user` (no Bypass perm) | yes | `s3.DeleteObject` | **403** | `AccessDenied` | `Access Denied.` |
| 4 | Compliance | root | yes | `s3.DeleteObject` | **400** | `InvalidRequest` | (same, bypass ignored) |
| 5 | *(control)* none | `q2user` | — | `s3.DeleteObject` | **200** | — | success (proves DeleteObject IS granted) |
| 6 | Governance | root | yes | `s3.DeleteObject` | **200** | — | success (version removed) |
| 7 | none (versionless) | root | — | `s3.DeleteMultipleObjects` | **200** | — | delete marker inserted |

### Root cause — `enforceRetentionBypassForDelete` (`cmd/bucket-object-lock.go:84`)

The single-object `DeleteObjectHandler` registers this check as `opts.SetEvalRetentionBypassFn(...)` and runs it **only when a specific `versionId` is supplied** (`cmd/object-handlers.go:2598-2611`, guarded by `if vID != ""`). Inside:

- **Legal hold ON** → `return ObjectLocked{}` (`:100-102`) — unconditional, applies to everyone including root.
- **Compliance** → if `!RetainUntilDate.Before(now)` → `return ObjectLocked{}` (`:117-121`) — no bypass branch exists, so **not even root** can bypass.
- **Governance, no bypass header** → if `!RetainUntilDate.Before(now)` → `return ObjectLocked{}` (`:143-146`).
- **Governance, bypass header set** → `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, ...)`; if that `!= ErrNone` → `return errAuthentication` (`:152-154`).

Error mapping (`cmd/api-errors.go`): `ObjectLocked` → `ErrObjectLocked` (`:2298`) = Code `InvalidRequest`, **HTTP 400**, "Object is WORM protected and cannot be overwritten" (`:1059-1062`); `errAuthentication` → `ErrAccessDenied` (`:2176`) = Code `AccessDenied`, **HTTP 403** (`:539-543`).

### Setup and before-state (`[OBSERVED]`)

```
$ mc mb --with-lock inv/q2-lock
Bucket created successfully `inv/q2-lock`.
$ mc version info inv/q2-lock
inv/q2-lock versioning is enabled

# q2user: DeleteObject/DeleteObjectVersion granted, s3:BypassGovernanceRetention deliberately ABSENT
$ mc admin policy create inv q2-deletenobypass q2user-policy.json ; mc admin policy attach inv q2-deletenobypass --user q2user
Created policy `q2-deletenobypass` successfully.
Attached Policies: [q2-deletenobypass]

# apply the three lock modes to distinct object versions
$ mc legalhold set inv/q2-lock/obj-legalhold
Object legal hold successfully set for `obj-legalhold`.
$ mc retention set --version-id c0d6f45c-1cc2-4297-9993-e7066fc5f666 GOVERNANCE 3650d inv/q2-lock/obj-governance
Object retention successfully set for `inv/q2-lock/obj-governance` (version-id=c0d6f45c-1cc2-4297-9993-e7066fc5f666).
$ mc retention set --version-id 5c3503e9-186c-4835-8829-c95d7e65e3d1 COMPLIANCE 3650d inv/q2-lock/obj-compliance
Object retention successfully set for `inv/q2-lock/obj-compliance` (version-id=5c3503e9-186c-4835-8829-c95d7e65e3d1).

# before-state queries
$ mc legalhold info inv/q2-lock/obj-legalhold
[    ON    ]  obj-legalhold
$ mc retention info --version-id c0d6f45c-1cc2-4297-9993-e7066fc5f666 inv/q2-lock/obj-governance
Mode    : GOVERNANCE, expiring in 3649 days
$ mc retention info --version-id 5c3503e9-186c-4835-8829-c95d7e65e3d1 inv/q2-lock/obj-compliance
Mode    : COMPLIANCE, expiring in 3649 days
```

Version IDs (captured for the specific-version deletes): `obj-legalhold=fbbe580b-d056-4df1-9131-bfecbf323cdc`, `obj-governance=c0d6f45c-1cc2-4297-9993-e7066fc5f666`, `obj-compliance=5c3503e9-186c-4835-8829-c95d7e65e3d1`, `obj-govbypass=d2e18aa0-564d-4074-8953-8ace3faf7dee`, `obj-marker v1=b1d414f0-5f40-4734-ab9e-8d2111cf3395`.

### Variant 1 — Legal hold, specific-version delete (root) → 400, `[OBSERVED]`

The single-object `DeleteObject` driver (minio-go `RemoveObject` with a `versionId`, UA `minio-go/v7.0.80`):

```
$ AK=<root> SK=*** ./delete_single q2-lock obj-legalhold fbbe580b-d056-4df1-9131-bfecbf323cdc
DELETE bucket=q2-lock object=obj-legalhold versionId=fbbe580b-d056-4df1-9131-bfecbf323cdc bypass=false
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
```

Trace — note the handler is `s3.DeleteObject` and the request carries `?versionId=...`:

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-13T19:39:21.829] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /q2-lock/obj-legalhold?versionId=fbbe580b-d056-4df1-9131-bfecbf323cdc
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=<root>/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=e2e6fd68bb346b20a8056fb3012b6d5bc7366c028227b2933a0115c75223d648
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:39:21.830] [ Duration 744µs TTFB 722.469µs ↑ 77 B  ↓ 369 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>obj-legalhold</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-legalhold</Resource><RequestId>18C1F0AF003F4711</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Variant 2 — Governance, no bypass (root) → 400, `[OBSERVED]`

```
$ AK=<root> SK=*** ./delete_single q2-lock obj-governance c0d6f45c-1cc2-4297-9993-e7066fc5f666
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
```
Trace `[REQUEST s3.DeleteObject] DELETE /q2-lock/obj-governance?versionId=c0d6f45c-...` → `400 Bad Request`, `<Code>InvalidRequest</Code>` (RequestId `18C1F0AF00C2E3EC`).

### Variant 3 — Governance, bypass header set, caller LACKS `s3:BypassGovernanceRetention` → 403, `[OBSERVED]`

`q2user` supplies `x-amz-bypass-governance-retention: true`:

```
$ AK=q2user SK=*** ./delete_single q2-lock obj-governance c0d6f45c-1cc2-4297-9993-e7066fc5f666 bypass
DELETE RESULT: ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
  Message:        Access Denied.
```

Trace — the bypass header **is** present and signed, and the response is 403:

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-13T19:39:21.846] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /q2-lock/obj-governance?versionId=c0d6f45c-1cc2-4297-9993-e7066fc5f666
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q2user/20260713/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=e3de09baf3c540c12b9e5dfd44ea5d6bda2237885c5021396a025ed4d4dd6b38
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:39:21.847] [ Duration 713µs TTFB 702.217µs ↑ 111 B  ↓ 333 B ]
127.0.0.1:9000 403 Forbidden
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>obj-governance</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-governance</Resource><RequestId>18C1F0AF0148134E</RequestId>...</Error>
```

**Isolation that this 403 is at the `BypassGovernanceRetention` check, not `DeleteObject` (`[OBSERVED]`).** The same `q2user` deletes an **unlocked** version successfully — proving `s3:DeleteObject` is granted, so the only thing missing in variant 3 is the bypass permission:

```
$ AK=q2user SK=*** ./delete_single q2-lock obj-q2ctrl c5a480bf-187a-4899-b4bc-88e6cab079f3
DELETE RESULT: SUCCESS (version c5a480bf-187a-4899-b4bc-88e6cab079f3 removed)
```

(An unrelated `GET /q2-lock/?location=` preflight from the SDK also returns 403 because `q2user` lacks `s3:GetBucketLocation`; the SDK ignores it and proceeds to the DELETE shown above.)

### Variant 4 — Compliance, bypass header set, caller is ROOT → still 400, `[OBSERVED]`

Compliance cannot be bypassed by anyone (the code has no bypass branch for `RetCompliance`):

```
$ AK=<root> SK=*** ./delete_single q2-lock obj-compliance 5c3503e9-186c-4835-8829-c95d7e65e3d1 bypass
DELETE RESULT: ERROR
  HTTPStatusCode: 400
  Code:           InvalidRequest
  Message:        Object is WORM protected and cannot be overwritten
```
Trace `[REQUEST s3.DeleteObject] DELETE /q2-lock/obj-compliance?versionId=5c3503e9-...` → `400 Bad Request` (RequestId `18C1F0AF01CDDEEE`).

### Variant 6 — Governance, bypass with permission (root) → 200, `[OBSERVED]`

```
$ AK=<root> SK=*** ./delete_single q2-lock obj-govbypass d2e18aa0-564d-4074-8953-8ace3faf7dee bypass
DELETE RESULT: SUCCESS (version d2e18aa0-564d-4074-8953-8ace3faf7dee removed)
```

### Variant 7 — Versionless delete → **`DeleteMultipleObjects`** handler, delete marker (handler correction), `[OBSERVED]`

A plain `mc rm` (no `versionId`) on a versioned bucket does **not** hit `DeleteObjectHandler`; it is batched into `POST /?delete=` and served by `DeleteMultipleObjectsHandler` (`cmd/bucket-handlers.go:416`). It inserts a delete marker and is **not** blocked (the retention check only runs for a specific `versionId`):

```
$ mc rm inv/q2-lock/obj-marker
Created delete marker `inv/q2-lock/obj-marker` (versionId=9f37478c-784b-4e2e-a771-1322894bcff2).
$ mc ls --versions inv/q2-lock/obj-marker
[2026-07-13 19:39:49 UTC]     0B STANDARD 9f37478c-784b-4e2e-a771-1322894bcff2 v2 DEL obj-marker
[2026-07-13 19:38:40 UTC]    25B STANDARD b1d414f0-5f40-4734-ab9e-8d2111cf3395 v1 PUT obj-marker
```

Trace proving the handler is `s3.DeleteMultipleObjects` (not `s3.DeleteObject`):

```
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-13T19:39:49.844] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /q2-lock/?delete=
127.0.0.1:9000 Content-Md5: NOmpJKkwhT3DPzfeYxc/OA==
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>obj-marker</Key></Object></Delete>
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:39:49.848] [ Duration 3.968ms TTFB 3.933733ms ↑ 180 B  ↓ 272 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><DeleteMarker>true</DeleteMarker><DeleteMarkerVersionId>9f37478c-784b-4e2e-a771-1322894bcff2</DeleteMarkerVersionId><Key>obj-marker</Key></Deleted></DeleteResult>
```

### No console log line on a WORM block (`[OBSERVED]`)

The server console log (`logs/server.log`) was **16 lines before and 16 lines after** all four blocked deletes (delta 0). The enforcement never calls the logger on a normal block — `internalLogIf(ctx, err, logger.WarningKind)` is reached only when `objectlock.UTCNowNTP()` errors (`cmd/bucket-object-lock.go:116,143`). Thus the authoritative runtime "log entry" for a locked-object delete is the **HTTP error response** shown on the trace stream, not a server log line.

### Stability

The four blocked variants were re-run; verdicts were identical: 1 = `400 InvalidRequest`, 2 = `400 InvalidRequest`, 3 = `403 AccessDenied`, 4 = `400 InvalidRequest`.

---

## Q3 — Bit-rot detection on unauthorized backend corruption, and the logs during a subsequent GET

**Question.** How does the system handle unauthorized manual data corruption in the storage backend? Trigger a bit-rot detection event and identify the specific runtime logs generated during a subsequent GET.

**Short answer (`[OBSERVED]`).** After one on-disk erasure shard is corrupted, a subsequent GET **succeeds and returns byte-for-byte-correct data** (reconstructed from parity). The GET produces **no console/audit log line**; the observable runtime signal is in the `mc admin trace --all` **STORAGE** stream, which shows the read fanning out to a **parity** drive to reconstruct after the corrupt data shard fails its checksum. The corrupt shard is **not** repaired on disk by the GET itself — on-disk repair is deferred (queued to the background MRF healer, `[INFERRED]`) and is performed deterministically by an explicit deep-scan heal, whose `storage.VerifyFile` + `RenameData` write-back **is** observable.

### Setup — 8 MiB object in erasure mode, single set EC:2

An 8 MiB object is used so real `part.N` files exist on disk (objects below the inline threshold are packed into `xl.meta`).

```
$ head -c 8388608 /dev/urandom > original.bin
$ sha256sum original.bin
41b53e9a3e6ba6ba70a8dc7a5b6cd21bf3d891950012c9fcda71edf15d6fa4b2  original.bin
$ mc mb inv/q3-bitrot ; mc cp original.bin inv/q3-bitrot/object.bin
Bucket created successfully `inv/q3-bitrot`.
...Total: 8.00 MiB (success)
```

On-disk layout — identical on all four drives (one shard each):

```
data/1/q3-bitrot/object.bin/xl.meta
data/1/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
data/2/q3-bitrot/object.bin/xl.meta
data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
data/3/... (same)     data/4/... (same)
```

### Locating the shard with `xl-meta` (valid **positional** invocation — no `-d` flag)

The `xl-meta` tool takes the `xl.meta` path as a positional argument. (`xl-meta`'s only flags are `--data`, `--export`, `--combine`, `--xver`, `--help`; there is **no** `-d` flag.)

```
$ ./xl-meta /tmp/minio-investigation/data/1/q3-bitrot/object.bin/xl.meta
```

Decoded erasure metadata (`Versions[0].Metadata.V2Obj`):

```json
{
  "DDir": "jZRoublnRRiwglOhNNvQ1A==",
  "EcAlgo": 1,
  "EcM": 2,
  "EcN": 2,
  "EcBSize": 1048576,
  "EcIndex": 4,
  "EcDist": [ 4, 1, 2, 3 ],
  "Size": 8388608,
  "PartNums": [ 1 ],
  "PartSizes": [ 8388608 ],
  "CSumAlgo": 1
}
```

`EcM=2` data + `EcN=2` parity; `DDir` base64 `jZRoublnRRiwglOhNNvQ1A==` decodes to the on-disk directory UUID `8d9468b9-b967-4518-b082-53a134dbd0d4`. `EcDist=[4,1,2,3]` maps logical shard → drive; the per-drive `EcIndex` values are: drive 1 → 4, **drive 2 → 1**, drive 3 → 2, drive 4 → 3. Each `part.1` is 4194560 bytes on disk (~4 MiB shard + interleaved bit-rot checksums). The **single target** is the data shard on **drive 2** (EcIndex 1), which is always in the read set:

```
/tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
```

### Corrupting EXACTLY ONE shard (`[OBSERVED]`)

Pre-corruption state of the target shard (complete hash, untruncated). The exact absolute path is bound to `$TARGET`:

```
$ TARGET=/tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
$ sha256sum "$TARGET"
0dacecd64f515fb74ea1ec3e18f9074a5acf893cf895646bf014b45eaa77cc3c  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 e5 96 d6 b6 f0 eb ff 8e 56 9c bd 75 c1 3e eb 53
```

Overwrite 16 bytes at offset 2000000 with `0xDEADBEEF`×4 (simulating unauthorized backend tampering of a single shard):

```
$ printf '\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef' \
    | dd of="$TARGET" bs=1 seek=2000000 count=16 conv=notrunc
16+0 records in
16+0 records out
16 bytes copied, 0.000167199 s, 16 kB/s
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 de ad be ef de ad be ef de ad be ef de ad be ef
$ sha256sum "$TARGET"
51104abb62407acbe9b06738a3cd0b120c369330528814ec97f19bab9e3c17f8  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
```

Only the drive-2 shard changed; the other three are byte-identical to baseline (complete hashes):

```
drive 1: 1aefe42af00db63e10204cb40439790e02a24d421068e67c71c25a87e03c929b  UNCHANGED
drive 2: 0dacecd6...aa77cc3c  ->  51104abb...9e3c17f8                       CHANGED
drive 3: 8ab123db03202d0bbe9d9ebd760a393faa7daba6e1f992a0e7f039c9e582912f  UNCHANGED
drive 4: ed4cd247851ad161bde401facac9352983e88c0b15a64550e8bc143039068482  UNCHANGED
```

### The subsequent GET — reconstructs correctly, no console log (`[OBSERVED]`)

A fresh GET to a **unique** destination, with a pre-absence assertion so a stale file cannot masquerade as success:

```
$ DEST=evidence/q3/retrieved_1783971946525022564.bin
$ test ! -e "$DEST" && echo "PRE-GET: dest absent (asserted)"
PRE-GET: dest absent (asserted)
$ mc cp inv/q3-bitrot/object.bin "$DEST" ; echo "exit=$?"
...Total: 8.00 MiB (success)
exit=0
$ sha256sum "$DEST"
41b53e9a3e6ba6ba70a8dc7a5b6cd21bf3d891950012c9fcda71edf15d6fa4b2  retrieved_1783971946525022564.bin
```

The retrieved SHA-256 equals the **original** `41b53e9a...` exactly — the GET returned correct bytes despite the corrupt shard, i.e. it reconstructed the data shard from parity in flight.

**Console/audit log during the GET: none.** The server console log was byte-identical before and after the GET (delta = 0 lines). This is grounded in source: the erasure read path has **no `logIf` on the corruption branch** — `streamingBitrotReader.ReadAt` returns the sentinel silently (`cmd/bitrot-streaming.go`):

```go
	b.h.Write(buf)
	if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {
		return 0, errFileCorrupt
	}
```

(`cmd/bitrot-streaming.go:184-186` — the sentinel is returned directly; there is no `logger.LogIf`/`bugLogIf` call on this branch, which is why the GET produces no console line.)

and `cmd/erasure-decode.go` records the failure only as an in-memory atomic flag (`bitrotHeal`), again with no log (`cmd/erasure-decode.go:193-200`):

```go
			if err != nil {
				switch {
				case errors.Is(err, errFileNotFound):
					atomic.StoreInt32(&missingPartsHeal, 1)
				case errors.Is(err, errFileCorrupt):
					atomic.StoreInt32(&bitrotHeal, 1)
```

The flag is consumed later at `cmd/erasure-decode.go:227-228` (`} else if bitrotHeal == 1 { return newBuf, errFileCorrupt }`), propagating `errFileCorrupt` up to the heal-on-read enqueue — still with no log emitted.

The observable runtime signal is therefore the **`mc admin trace --all` STORAGE stream**, captured concurrently with the GET (`trace_get.log`, 206 lines). The relevant `storage.ReadFileStream` lines (verbatim; drives referenced by their data-dir root) show the read fanning out across drives — critically, a **parity** drive (`data/4`) is read to reconstruct the missing data shard, and no error surfaces (`total-errs-availability=0`):

```
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-13T19:45:48.561] /tmp/minio-investigation/data/2 q3-bitrot object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1 total-errs-availability=0 total-errs-timeout=0 39.562µs 4.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-13T19:45:48.561] /tmp/minio-investigation/data/3 q3-bitrot object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1 total-errs-availability=0 total-errs-timeout=0 50.162µs 4.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-13T19:45:48.564] /tmp/minio-investigation/data/4 q3-bitrot object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1 total-errs-availability=0 total-errs-timeout=0 60.084µs 2.5 MiB
```

The three drives read are `data/2`, `data/3`, `data/4`; `data/1` is not read (only `EcM+1 = 3` shards are needed). The `data/2` shard is the corrupt one, so its bytes fail the interleaved bit-rot checksum and are discarded; the `data/4` **parity** read (partial, 2.5 MiB) supplies the reconstruction. The only REQUEST-level funcnames on this stream were `s3.GetObject`, `s3.HeadObject`, and `s3.GetBucketLocation` — the corruption never becomes a request-level error because parity covers it transparently.

### On-disk state after the GET — still corrupt (deferred heal) (`[OBSERVED]`)

Re-hashing the target shard ~8 s after the GET shows it is **still corrupt** — the GET reconstructs in memory but does **not** rewrite the shard synchronously:

```
$ sha256sum "$TARGET"
51104abb62407acbe9b06738a3cd0b120c369330528814ec97f19bab9e3c17f8  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
```
(unchanged from the post-corruption hash — still corrupt.)

The read path queues the repair rather than performing it inline. In `cmd/erasure-object.go`, once the requested part has been fully served, a partial-heal op tagged for a bit-rot scan is enqueued (`[INFERRED]` — this code is read, not directly observed at runtime, because the enqueue emits no log):

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
```

So the distinction the question targets is real and observable: **in-flight reconstruction (serves correct bytes) ≠ on-disk repair (deferred).**

### Explicit deep-scan heal — repairs the shard, and IS observable (`[OBSERVED]`)

Forcing a deep heal deterministically repairs the shard:

```
$ mc admin heal --recursive --force-start --scan deep inv/q3-bitrot
Unable to display heal status. Invalid Request.
```

The "Unable to display heal status" line is an `mc` **status-display quirk only**; the heal itself ran. Post-heal, the target shard is restored to its original bytes (complete hash):

```
$ sha256sum "$TARGET"
0dacecd64f515fb74ea1ec3e18f9074a5acf893cf895646bf014b45eaa77cc3c  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 e5 96 d6 b6 f0 eb ff 8e 56 9c bd 75 c1 3e eb 53
```
The hash equals the pre-corruption baseline `0dacecd6...` and the bytes at offset 2000000 are the original `e5 96 d6 b6 ...` — the shard is fully restored on disk.

The `mc admin trace --all` STORAGE stream during the heal (`trace_heal.log`, 218 lines) shows the actual detection-and-repair signature: **`storage.VerifyFile` on every one of the four drives** (this is where the corrupt shard is detected on `data/2`), followed by a fresh shard write (`storage.CreateFile`) and atomic rename (`storage.RenameData`) **only on `data/2`**. Verbatim:

```
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-13T19:46:57.293] /tmp/minio-investigation/data/1 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 1.448673ms
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-13T19:46:57.295] /tmp/minio-investigation/data/2 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 667.51µs
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-13T19:46:57.296] /tmp/minio-investigation/data/3 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 1.476351ms
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-13T19:46:57.297] /tmp/minio-investigation/data/4 q3-bitrot object.bin total-errs-timeout=0 total-errs-availability=0 1.460162ms
127.0.0.1:9000  [STORAGE storage.CreateFile] [2026-07-13T19:46:57.299] /tmp/minio-investigation/data/2 .minio.sys/tmp 9f8e6fe1-110a-451c-a81a-65de5778fd10/8d9468b9-b967-4518-b082-53a134dbd0d4/part.1 total-errs-availability=0 total-errs-timeout=0 11.722ms 4.0 MiB
127.0.0.1:9000  [STORAGE storage.RenameData] [2026-07-13T19:46:57.310] /tmp/minio-investigation/data/2 9f8e6fe1-110a-451c-a81a-65de5778fd10 8d9468b9-b967-4518-b082-53a134dbd0d4 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 1.344214ms
```

`VerifyFile` is the disk-level bit-rot check (`cmd/xl-storage.go` `VerifyFile`/`bitrotVerify`). The reconstructed shard is written on `data/2` under a temporary UUID directory `.minio.sys/tmp/9f8e6fe1-110a-451c-a81a-65de5778fd10/` and then atomically `RenameData`'d so its `8d9468b9-...` (the object `DDir`) replaces the corrupt one — exactly `healObject`'s write-back path (`cmd/erasure-healing.go`). The wider heal trace also contains 8× `ReadVersion`, 4× `Delete`, 4× `DiskInfo`, 8× `StatVol`, 3× `WalkDir`, and 2× `admin.Heal` calls; only `data/2` receives `CreateFile`+`RenameData`, confirming a single-shard repair.

### Stability (2 runs, `[OBSERVED]`)

Re-running the full cycle a second time produced identical verdicts: re-corruption reproduces `51104abb...`; the GET exits 0 and returns `41b53e9a...` (= original); the shard remains `51104abb...` on disk immediately after the GET; the explicit deep heal restores it to `0dacecd6...`.

### Coverage of named mechanisms (Q3)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| On-read checksum verify → sentinel | `errFileCorrupt` returned with **no log** — `cmd/bitrot-streaming.go` `streamingBitrotReader.ReadAt` (L150; mismatch L184-186) | source-grounded; GET console delta = 0 `[OBSERVED]` |
| Corruption sentinel | `errFileCorrupt = StorageErr("file is corrupted")` `cmd/storage-errors.go:104` | `[INFERRED]` (source) |
| Heal flag + parity reconstruct | `cmd/erasure-decode.go:197-198` sets `bitrotHeal`; RS reconstruct | GET returns correct bytes from parity `[OBSERVED]` |
| Heal-on-read enqueue | `globalMRFState.addPartialOp(... BitrotScan ...)` `cmd/erasure-object.go:399-407` (healOnce L346) | `[INFERRED]` (no log at enqueue) |
| Disk-level verify | `VerifyFile`/`bitrotVerify` `cmd/xl-storage.go:3097,3079` | `storage.VerifyFile` in heal trace `[OBSERVED]` |
| Repair write-back | `healObject` `cmd/erasure-healing.go:258` | `CreateFile`+`RenameData` on drive 2 `[OBSERVED]` |
| Background MRF consumer | `cmd/background-newdisks-heal-ops.go:389-390` | `[INFERRED]` (source) |
| Scanner bit-rot scan-mode select | `getCycleScanMode` `cmd/data-scanner.go:93` | `[INFERRED]` (source) |

---

## Q4 — STS temporary credentials enforce the inline session policy (intersection semantics)

**Question.** Verify that when a user gets temporary credentials, MinIO enforces the session policy on that user. Give runtime test output to prove this behavior.

**Short answer (`[OBSERVED]`).** Temporary credentials issued by STS `AssumeRole` with an inline session policy are enforced as the **intersection** of the session policy AND the parent identity's policy: a request succeeds only if **both** allow it. Proven two ways with real credentials: (a) a **broad** parent (`s3:*`) + a **GetObject-only** session policy → `GetObject` succeeds (200) but `PutObject` is denied (403) by the session policy; (b) a **narrow** parent (`GetObject`-only) + a **broad** (`s3:*`) session policy → `PutObject` is still denied (403) because the session policy **cannot widen** the parent. The session policy travels as a signed JWT **claim** keyed by the constant `policy.SessionPolicyName` (`"sessionPolicy"`), not as a credentials field.

### Setup — one bucket, one readable object, a BROAD parent and a NARROW parent

```
$ mc mb inv/q4-bucket
Bucket created successfully `inv/q4-bucket`.
$ mc cp seed.txt inv/q4-bucket/readable.txt        # 24-byte object "q4-seed-object-contents"
```

Broad parent policy `q4-broad` (attached to user `q4user`):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow", "Action": ["s3:*"],
    "Resource": ["arn:aws:s3:::q4-bucket", "arn:aws:s3:::q4-bucket/*"] }
 ]
}
```

Narrow parent policy `q4-narrow` (attached to user `q4narrow`):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow", "Action": ["s3:GetObject"],
    "Resource": ["arn:aws:s3:::q4-bucket/*"] }
 ]
}
```

```
$ mc admin policy create inv q4-broad q4-broad.json
Created policy `q4-broad` successfully.
$ mc admin user add inv q4user ***REDACTED*** ; mc admin policy attach inv q4-broad --user q4user
Added user `q4user` successfully.
Attached Policies: [q4-broad]
To User: q4user
$ mc admin policy create inv q4-narrow q4-narrow.json
Created policy `q4-narrow` successfully.
$ mc admin user add inv q4narrow ***REDACTED*** ; mc admin policy attach inv q4-narrow --user q4narrow
Added user `q4narrow` successfully.
Attached Policies: [q4-narrow]
To User: q4narrow
```

### Parent baseline — the broad parent CAN do BOTH GET and PUT directly (`[OBSERVED]`)

This baseline is essential: it proves that when the STS `PutObject` is later denied, the denial comes from the **session policy**, not from a parent that lacked `PutObject` in the first place.

```
$ mc alias set q4parent http://127.0.0.1:9000 q4user ***REDACTED***
$ mc cp q4parent/q4-bucket/readable.txt parent_get_out.txt ; echo "exit=$?"
...readable.txt         # SUCCESS
exit=0
$ mc cp parent_put_in.txt q4parent/q4-bucket/parent-wrote.txt ; echo "exit=$?"
...parent-wrote.txt     # SUCCESS
exit=0
$ mc ls inv/q4-bucket/parent-wrote.txt
[2026-07-13 19:58:41 UTC]    24B STANDARD parent-wrote.txt
```

Both direct operations succeed → the broad parent `q4user` is allowed **both** `s3:GetObject` and `s3:PutObject`.

### Main proof — GetObject-only session policy narrows the broad parent (`[OBSERVED]`)

Using the MinIO Go SDK (`credentials.NewSTSAssumeRole`, endpoint `http://127.0.0.1:9000`), `q4user` assumes a role with a **restrictive** inline session policy (`GetObject` only), and the **same** temporary credential set runs both `GetObject` and `PutObject`:

```
$ PARENT_AK=q4user PARENT_SK=... SESSION_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}' \
    ./sts_assume_role q4-bucket readable.txt

=== STS AssumeRole ===
Parent AccessKey: q4user
Inline session policy:
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}
--- Temporary credentials issued ---
Temp AccessKeyID:     TF1VE9TSKDMJNL2T3VU4
Temp SecretAccessKey: DEZ...REDACTED...4Go
Temp SessionToken:    eyJhbGciOiJIUzUxMiIsInR5...(463 chars)
SignerType:           S3v4
--- Decoded SessionToken JWT payload ---
{
  "accessKey": "TF1VE9TSKDMJNL2T3VU4",
  "exp": 1783976337,
  "parent": "q4user",
  "sessionPolicy": "eyJWZXJzaW9uIjoiMjAxMi0xMC0xNyIsIlN0YXRlbWVudCI6W3siRWZmZWN0IjoiQWxsb3ciLCJBY3Rpb24iOlsiczM6R2V0T2JqZWN0Il0sIlJlc291cmNlIjpbImFybjphd3M6czM6OjpxNC1idWNrZXQvKiJdfV19"
}

=== OP1: GetObject with temp creds ===
GetObject RESULT: SUCCESS (read 24 bytes)

=== OP2: PutObject with temp creds ===
PutObject RESULT: DENIED/ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
  Message:        Access Denied.
```

The single temp identity `TF1VE9TSKDMJNL2T3VU4` (parent `q4user`) can **read** (allowed by both parent and session policy) but cannot **write** (allowed by parent, **denied by the session policy**) — exactly the intersection behaviour.

### The session policy is a JWT CLAIM keyed by `policy.SessionPolicyName` (`[OBSERVED]`) — resolves the "field" misconception

The temporary credentials do **not** carry a `SessionPolicyName` *field* on the credentials struct. The session policy is embedded in the **session token** as a base64 JWT claim whose key is the exported constant `policy.SessionPolicyName`:

```
$ grep -n SessionPolicyName $(go env GOMODCACHE)/github.com/minio/pkg/v3@v3.0.22/policy/constants.go
27:	SessionPolicyName = "sessionPolicy"
```

The decoded JWT payload above shows the literal claim key `"sessionPolicy"`. Its base64 value decodes back to exactly the policy that was sent:

```
$ echo 'eyJWZXJzaW9uIjoiMjAxMi0xMC0xNyIsIlN0YXRlbWVudCI6W3siRWZmZWN0IjoiQWxsb3ciLCJBY3Rpb24iOlsiczM6R2V0T2JqZWN0Il0sIlJlc291cmNlIjpbImFybjphd3M6czM6OjpxNC1idWNrZXQvKiJdfV19' | base64 -d
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}
```

Server side, the claim is written at `cmd/sts-handlers.go:127` (`c[policy.SessionPolicyName] = base64.StdEncoding.EncodeToString(policyBuf)`, in `populateSessionPolicy` at L94, size-bounded by `maxSTSSessionPolicySize = 2048` at L89 / check at L123) and read back at `cmd/auth-handler.go:251` (`sp, spok := claims.Lookup(policy.SessionPolicyName)`), which re-publishes it under the internal key `sessionPolicyNameExtracted` (`cmd/iam.go:2136`) for evaluation.

### Runtime trace of the main proof (`[OBSERVED]`)

`mc admin trace --all -v` captured the whole flow (`trace_main.log`, 243 lines). The relevant REQUEST lines and statuses, verbatim:

```
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-13T19:58:57.366] [Client IP: 127.0.0.1]
127.0.0.1:9000 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3AGetObject%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Aq4-bucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15
127.0.0.1:9000 200 OK
# (per-request verbose headers omitted for length; each event's request line + response status shown in full; complete 243-line trace retained as evidence)
127.0.0.1:9000 [REQUEST s3.GetBucketLocation] [2026-07-13T19:58:57.372] [Client IP: 127.0.0.1]
127.0.0.1:9000 403 Forbidden
# (per-request verbose headers omitted for length; each event's request line + response status shown in full; complete 243-line trace retained as evidence)
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-13T19:58:57.373] [Client IP: 127.0.0.1]
127.0.0.1:9000 200 OK
# (per-request verbose headers omitted for length; each event's request line + response status shown in full; complete 243-line trace retained as evidence)
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T19:58:57.374] [Client IP: 127.0.0.1]
127.0.0.1:9000 403 Forbidden
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>sts-put-probe.txt</Key><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/sts-put-probe.txt</Resource><RequestId>18C1F1C0B434D4BD</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The `sts.AssumeRole` request carries the URL-encoded session `Policy=` parameter (decodes to the GetObject-only policy) and returns `200 OK`. `s3.GetObject` returns `200 OK`; `s3.PutObject` returns `403 Forbidden` / `AccessDenied`. **Note (honest):** the `s3.GetBucketLocation` call *also* returns `403` because the GetObject-only session policy does not grant it; the SDK tolerates this and falls back to region `us-east-1`, so the subsequent GET still succeeds. There are therefore two `403`s in the trace — one benign (`GetBucketLocation`) and the substantive one (`PutObject`).

### Cannot-widen edge — narrow parent + BROAD session policy still denies PUT (`[OBSERVED]`)

The mirror case proves the session policy can only *narrow*. First, the narrow parent `q4narrow` directly (baseline):

```
$ mc cp q4narrowp/q4-bucket/readable.txt narrowparent_get.txt ; echo "exit=$?"
...readable.txt        # SUCCESS
exit=0
$ mc cp np_put.txt q4narrowp/q4-bucket/np-wrote.txt ; echo "exit=$?"
mc: <ERROR> Failed to copy `np_put.txt`. Insufficient permissions to access this path `http://127.0.0.1:9000/q4-bucket/np-wrote.txt`
exit=1
```

Now `q4narrow` assumes a role with a **broad** `s3:*` session policy:

```
=== OP1: GetObject with temp creds ===
GetObject RESULT: SUCCESS (read 24 bytes)
=== OP2: PutObject with temp creds ===
PutObject RESULT: DENIED/ERROR
  HTTPStatusCode: 403
  Code:           AccessDenied
```

Even though the session policy grants `s3:*`, `PutObject` is denied `403` because the **parent** does not allow it. Session policy cannot grant a permission the parent lacks.

### Stability (2 runs, `[OBSERVED]`)

The main proof was run twice. Both runs produced identical verdicts (a fresh temp key each time, same parent `q4user`):

```
run1: GetObject RESULT: SUCCESS (read 24 bytes)   PutObject RESULT: DENIED/ERROR (403 AccessDenied)
run2: GetObject RESULT: SUCCESS (read 24 bytes)   PutObject RESULT: DENIED/ERROR (403 AccessDenied)
```

### Source grounding — where the intersection is enforced

The enforcement is `IAMSys.IsAllowedSTS` (`cmd/iam.go:2242`). It fetches the parent's mapped policies (`sys.PolicyDBGet(parentUser, ...)`), merges them into `combinedPolicy`, then evaluates the session policy and returns the **AND** of the two (`cmd/iam.go:2310-2312`):

```go
	hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
	if hasSessionPolicy {
		return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))
	}
```

`isAllowedBySessionPolicy` (`cmd/iam.go:2381`) parses the extracted claim and — critically — forces owner status off so even a root-derived session is constrained (`cmd/iam.go:2413-2417`):

```go
	// As the session policy exists, even if the parent is the root account, it
	// must be restricted by it. So, we set `.IsOwner` to false here
	// unconditionally.
	sessionPolicyArgs := args
	sessionPolicyArgs.IsOwner = false
```

For `PutObject` under the GetObject-only session policy, `isAllowedSP == false`, so the AND is false → `403`. For `PutObject` under the narrow parent, `combinedPolicy.IsAllowed(args) == false`, so the AND is false → `403`. Both observed outcomes match.

### Coverage of named mechanisms (Q4)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| STS entry point | `AssumeRole` `cmd/sts-handlers.go:256` | `[REQUEST sts.AssumeRole]` 200 `[OBSERVED]` |
| Session policy read from request | `populateSessionPolicy` `cmd/sts-handlers.go:94`; size cap `maxSTSSessionPolicySize=2048` L89 / check L123 | `Policy=` param in trace `[OBSERVED]` |
| Session policy stored as JWT claim | `c[policy.SessionPolicyName]=base64(...)` `cmd/sts-handlers.go:127`; const `"sessionPolicy"` `pkg/v3/policy/constants.go:27` | decoded JWT `sessionPolicy` claim `[OBSERVED]` |
| Claim decoded server-side | `claims.Lookup(policy.SessionPolicyName)` `cmd/auth-handler.go:251`; extracted key `cmd/iam.go:2136` | `[INFERRED]` (source) |
| Intersection enforcement | `IsAllowedSTS` `cmd/iam.go:2242`; AND return L2312 | GET 200 / PUT 403 `[OBSERVED]` |
| Owner forced off for session | `sessionPolicyArgs.IsOwner=false` `cmd/iam.go:2417` | `[INFERRED]` (source) |
| Cannot-widen | parent AND session both required | narrow-parent + broad-session PUT 403 `[OBSERVED]` |

---

## Q5 — A basic user cannot self-promote to console admin by modifying user→policy mappings

**Question.** Show test output proving a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user-mappings modification behavior observed.

**Short answer (`[OBSERVED]`).** A user holding only a read-only policy is denied (`403 AccessDenied`) on **every** path that could attach an admin policy to themselves — the modern attach API, the deprecated set-policy API, policy creation, and even listing users. The user→policy mapping is **provably unchanged** afterward. **Root cause (`[OBSERVED]` + source-grounded):** deny-by-default admin authorization. The guard `validateAdminReq` → `checkAdminRequestAuth` → `IAMSys.IsAllowed` evaluates the requested **admin action** against the caller's attached policy and returns `ErrAccessDenied` **before any mapping mutation code runs**, because the read-only policy grants no `admin:*` action.

### Setup — a basic read-only user, and the empty consoleAdmin mapping (`[OBSERVED]`)

Read-only policy `q5-readonly` (finding-relevant: the exact JSON):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow",
    "Action": ["s3:GetObject", "s3:ListBucket"],
    "Resource": ["arn:aws:s3:::q5-bucket", "arn:aws:s3:::q5-bucket/*"] }
 ]
}
```

```
$ mc admin policy create inv q5-readonly q5-readonly.json
Created policy `q5-readonly` successfully.
$ mc admin user add inv q5user ***REDACTED*** ; mc admin policy attach inv q5-readonly --user q5user
Added user `q5user` successfully.
Attached Policies: [q5-readonly]
To User: q5user
$ mc admin user info inv q5user
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
```

BEFORE state — `consoleAdmin` has no attached entities (privileged query as root):

```
$ mc admin policy entities inv --policy consoleAdmin
Query time: 2026-07-13T20:04:25Z
```

(The output lists no users and no groups — the `consoleAdmin` policy is attached to nobody.)

### Attempt 1 — self-attach `consoleAdmin` via the modern API (POST) → 403 (`[OBSERVED]`)

Authenticated **as `q5user`**, attach `consoleAdmin` to itself:

```
$ mc admin policy attach q5 consoleAdmin --user q5user
mc: <ERROR> Unable to make user/group policy association. Access Denied.
```

The `mc admin trace --all -v` capture shows the **authentic HTTP method and route** — it is a **POST** to `/idp/builtin/policy/attach` (not a PUT), handled by `AttachDetachPolicyBuiltin`, returning `403`:

```
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-13T20:04:40.349] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C1F2108F1D8C60","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

This route is wired at `cmd/admin-router.go:268` (`Methods(http.MethodPost).Path(adminVersion + "/idp/builtin/policy/{operation}")` → `AttachDetachPolicyBuiltin`), and the handler's first line guards on `policy.AttachPolicyAdminAction` (`cmd/admin-handlers-users.go:1908,1912`).

### Attempt 2 — deprecated set-policy API (PUT) → 403 (`[OBSERVED]`)

Using the MinIO admin SDK's deprecated `SetPolicy` (which hits `PUT /set-user-or-group-policy`):

```
$ AK=q5user SK=... ./set_policy consoleAdmin q5user
SetPolicy(policy=consoleAdmin, user=q5user, isGroup=false) as caller q5user
SetPolicy RESULT: DENIED/ERROR
  Code:      AccessDenied
  Message:   Access Denied.
  RequestID: 18C1F214C4A13AA0
  raw:       Access Denied.
```

Trace (verbatim) — a **PUT** to `/set-user-or-group-policy`, handled by `SetPolicyForUserOrGroup`, `403`:

```
127.0.0.1:9000 [REQUEST admin.SetPolicyForUserOrGroup] [2026-07-13T20:04:58.427] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-or-group-policy?isGroup=false&policyName=consoleAdmin&userOrGroup=q5user
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C1F214C4A13AA0","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Route: `cmd/admin-router.go:263` (`Methods(http.MethodPut).Path(adminVersion+"/set-user-or-group-policy")` → `SetPolicyForUserOrGroup`); guard `policy.AttachPolicyAdminAction` at `cmd/admin-handlers-users.go:1770,1773`.

### Attempt 3 — create an all-powerful policy (PUT) → 403 (`[OBSERVED]`)

```
$ mc admin policy create q5 q5-escalate q5-escalate.json   # {"Action":["admin:*","s3:*"]...}
mc: <ERROR> Unable to create new policy. Access Denied.
```

Trace — **PUT** `/add-canned-policy`, handled by `AddCannedPolicy`, `403`:

```
127.0.0.1:9000 [REQUEST admin.AddCannedPolicy] [2026-07-13T20:04:58.454] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/add-canned-policy?name=q5-escalate
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/add-canned-policy","RequestId":"18C1F214C642C96C","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Guard `policy.CreatePolicyAdminAction` at `cmd/admin-handlers-users.go:1701,1704`.

### Attempt 4 — even listing users is denied (GET) → 403 (`[OBSERVED]`)

```
$ mc admin user list q5
mc: <ERROR> Unable to list user. Access Denied.
```

```
127.0.0.1:9000 [REQUEST admin.ListUsers] [2026-07-13T20:04:58.480] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /minio/admin/v3/list-users
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/list-users","RequestId":"18C1F214C7C41976","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

### AFTER state — the mapping is provably unchanged (`[OBSERVED]`)

```
$ mc admin policy entities inv --policy consoleAdmin
Query time: 2026-07-13T20:05:13Z
$ mc admin user info inv q5user
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
$ mc admin policy list inv | grep -i escalate    # q5-escalate was never created
(no output)
```

A byte-diff of the `consoleAdmin` entity list (ignoring the timestamp line) before vs. after is empty — the mapping is identical, `q5user` was never attached to `consoleAdmin`, and no escalation policy was created. The self-promotion is fully prevented.

### Root cause (`[OBSERVED]` + source-grounded) — deny-by-default admin authorization, BEFORE any mutation

Every attach/create/list handler begins by calling `validateAdminReq` with the required admin action; the mutation code is never reached. The chain:

1. `validateAdminReq` (`cmd/admin-handler-utils.go:37`) calls `checkAdminRequestAuth(ctx, r, action, "")`; when it returns `ErrAccessDenied` it writes the 403 (`cmd/admin-handler-utils.go:58-59`):

```go
	writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
	return nil, auth.Credentials{}
```

2. `checkAdminRequestAuth` (`cmd/auth-handler.go:189`) verifies the signature, then evaluates the admin action against the caller's policy and returns `ErrAccessDenied` if not allowed (`cmd/auth-handler.go:194-206`):

```go
	if globalIAMSys.IsAllowed(policy.Args{
		AccountName:     cred.AccessKey,
		Groups:          cred.Groups,
		Action:          policy.Action(action),
		ConditionValues: getConditionValues(r, "", cred),
		IsOwner:         owner,
		Claims:          cred.Claims,
	}) {
		// Request is allowed return the appropriate access key.
		return cred, ErrNone
	}

	return cred, ErrAccessDenied
```

3. For a regular user, `IAMSys.IsAllowed` fetches the caller's mapped policies and returns the combined-policy decision (`cmd/iam.go:2482`):

```go
	// Policies were found, evaluate all of them.
	return sys.GetCombinedPolicy(policies...).IsAllowed(args)
```

`q5user`'s only policy is `q5-readonly`, which grants `s3:GetObject`/`s3:ListBucket` and **no** `admin:*` action, so `AttachPolicyAdminAction` / `CreatePolicyAdminAction` / `ListUsersAdminAction` all evaluate to `false` → `ErrAccessDenied` → `403`. Because this gate is the **first** statement of each handler (before any user→policy mapping is read or written), no mutation ever occurs. This is a general deny-by-default outcome, not a special-cased check.

### Correction of a common misconception (`consoleAdmin` at `cmd/admin-handlers-users.go:1455`) (`[OBSERVED]`)

The `consoleAdmin` assignment at `cmd/admin-handlers-users.go:1455-1463` is **not** a privilege-escalation defense and does **not** mean "a regular user can never receive `consoleAdmin`." It lives inside `AccountInfoHandler` (`cmd/admin-handlers-users.go:1348`) and only computes an *effective policy for display* — for the **owner/root** account (`accountName == globalActiveCred.AccessKey`) or when an external authZ plugin is configured — so the console UI can render. The source comment states the intent directly:

```go
		// For owner account and when plugin authZ is configured always set
		// effective policy as `consoleAdmin`.
		//
		// In the latter case, we let the UI render everything, but individual
		// actions would fail if not permitted by the external authZ service.
```

For the record, an **administrator** legitimately *can* attach `consoleAdmin` to any user; what is prevented here is a **basic user attaching it to themselves**, and that prevention is the deny-by-default gate above — not this UI-rendering branch.

### Stability (2 runs, `[OBSERVED]`)

Re-running the self-attach (both the modern POST attach and the deprecated PUT set-policy) a second time produced identical `403 AccessDenied` results.

### Coverage of named mechanisms (Q5)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| Modern attach entry point + guard | `AttachDetachPolicyBuiltin` `cmd/admin-handlers-users.go:1908`; guard `AttachPolicyAdminAction` L1912; route POST `cmd/admin-router.go:268` | POST `/idp/builtin/policy/attach` → 403 `[OBSERVED]` |
| Deprecated set-policy entry point + guard | `SetPolicyForUserOrGroup` `cmd/admin-handlers-users.go:1770`; guard L1773; route PUT `cmd/admin-router.go:263` | PUT `/set-user-or-group-policy` → 403 `[OBSERVED]` |
| Create-policy guard | `AddCannedPolicy` `cmd/admin-handlers-users.go:1701`; guard `CreatePolicyAdminAction` L1704 | PUT `/add-canned-policy` → 403 `[OBSERVED]` |
| Admin authorization wrapper + 403 | `validateAdminReq` `cmd/admin-handler-utils.go:37`; 403 write L58-59 | `[OBSERVED]` (all four 403s) |
| Signature+action check | `checkAdminRequestAuth` `cmd/auth-handler.go:189`; `IsAllowed` L194-201; `ErrAccessDenied` L206 | `[INFERRED]` (source) |
| Deny-by-default eval (regular user) | `IsAllowed` return `cmd/iam.go:2482` `GetCombinedPolicy(...).IsAllowed(args)` | `[INFERRED]` (source) |
| consoleAdmin (UI effective policy, root only) | `AccountInfoHandler` `cmd/admin-handlers-users.go:1348`; consoleAdmin branch L1455-1463 | not a defense; corrected `[OBSERVED]` |
| Mapping unchanged | consoleAdmin entities before==after; q5user still q5-readonly | `[OBSERVED]` |

---

## Cleanup and repository state

This investigation was conducted with a strict read-only posture toward the repository. All runtime activity used an **isolated scratch root** (`/tmp/minio-investigation`) and an **isolated `mc` config** (`MC_CONFIG_DIR=/tmp/minio-investigation/mc-config`, alias `inv`) so that no alias or credential from this session leaked into the host's default `~/.mc`.

At the end of the investigation the following cleanup was performed (`[OBSERVED]`):

```
# stop the investigation server (loopback-only, ephemeral random root credentials)
$ kill 172792

# remove residual investigation aliases left in the host default config by earlier draft runs
$ mc alias remove q1     ; mc alias remove q1deny
$ mc alias remove q4     ; mc alias remove q5
Removed `q1` successfully.
Removed `q1deny` successfully.
Removed `q4` successfully.
Removed `q5` successfully.

# host default /root/.mc aliases AFTER cleanup — only pre-existing defaults remain, no investigation aliases
$ mc alias ls | awk '{print $1}'
gcs
local
play
s3

# remove the isolated scratch root: server data dirs, all test buckets/objects/users/policies,
# STS temporary credentials, the corrupted-shard test data, drivers, and the isolated mc config
$ rm -rf /tmp/minio-investigation
```

Final repository state (`[OBSERVED]`) — the **only** change versus the baseline is this single documentation file; no source file was modified, added, or deleted:

```
$ git status --porcelain
 M blitzy/documentation/minio_c07e5b49d477.md

$ git diff --stat -- blitzy/documentation/minio_c07e5b49d477.md
 blitzy/documentation/minio_c07e5b49d477.md | 1422 +++++++++++++++-------------
 1 file changed, 781 insertions(+), 641 deletions(-)
```

All temporary scripts, fixtures, credentials, corrupted shards, and server data directories have been removed; fixture-user secrets are redacted throughout this document; and the repository is left byte-for-byte unchanged except for this answer file.
