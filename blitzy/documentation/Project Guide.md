# Blitzy Project Guide — MinIO Read-Only IAM Boundary Investigation

> **Deliverable type:** Run-first security investigation (documentation). **Sole repository write:** `blitzy/documentation/minio_c07e5b49d477.md`.
> **Branch:** `blitzy-b8fad361-f2c2-4f2e-90a6-30ee12dd6565` · **HEAD:** `a92582bfd` · **Base:** `c07e5b49d477`

---

## 1. Executive Summary

### 1.1 Project Overview

This project answers a single, security-critical question about **MinIO** — an S3-compatible object-storage server written in Go — using a *run-first* methodology: **does the IAM policy boundary reliably confine a read-only principal to read-only behavior, even while other clients concurrently hammer the same bucket with writes and metadata traffic, or can that principal mutate data through less-obvious S3 surface area?** The deliverable is one evidence-backed markdown document that builds and runs the canonical server, drives a genuinely read-only identity against the full write-adjacent API surface under concurrent load, captures request/response traces plus on-disk storage side effects, and states a grounded verdict. The MinIO codebase is treated strictly as read-only reference material — built, run, and exercised, never modified.

### 1.2 Completion Status

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextSize':'15px','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#B23AF2','pieSectionTextSize':'13px','pieLegendTextColor':'#B23AF2'}}}%%
pie showData title Project Completion — 86.7% Complete (52h of 60h)
    "Completed Work (AI)" : 52
    "Remaining Work" : 8
```

| Metric | Value |
|---|---|
| **Total Hours** | **60** |
| **Completed Hours (AI + Manual)** | **52** (52 AI-autonomous + 0 manual) |
| **Remaining Hours** | **8** |
| **Percent Complete** | **86.7%** |

> **How this is calculated (AAP-scoped, PA1).** All **20** discrete AAP requirement groups are **Completed** (the investigation document exists, every named probe was exercised at runtime, evidence was captured, the verdict is grounded, and the repository is unchanged except the one document). The remaining **8 hours** are **path-to-production** activities that intrinsically require humans — chiefly a security engineer's independent sign-off before the verdict is treated as authoritative. `Completion % = 52 / (52 + 8) = 86.7%`.

### 1.3 Key Accomplishments

- ✅ **Verdict delivered and validated:** *No — the read-only boundary holds; there was no bypass.* Every write-adjacent operation by the read-only principal was denied at handler entry with **no storage side effect**.
- ✅ **Canonical build & run reproduced** with the exact documented commands — build exit 0, a **156,592,469-byte** statically-linked binary (byte-identical on independent rebuild).
- ✅ **Full write-adjacent probe matrix exercised** through the real signed S3 API: multipart (7 ops), copy-style (2), metadata (5), deletes (single + multi) — **all `403 AccessDenied`**.
- ✅ **Information-leakage surface quantified** (existence, size, ETag, content-type, lock-header presence) within the granted prefix; denied outside it and for `GetObjectAttributes`.
- ✅ **Independent bypass hunt** across less-obvious surfaces (presigned PUT, POST-policy, version-targeted delete, S3 SELECT, restore, replication/batch) — no mutation possible.
- ✅ **Stability confirmed across ≥2 runs** under differing concurrent load; read-only outcomes identical, seed objects byte-stable, backend snapshot `SNAPSHOT_IDENTICAL`.
- ✅ **Cross-SDK corroboration** (minio-go + boto3/botocore), surfacing the pre-IAM `Content-MD5` gate nuance.
- ✅ **Scope compliance:** exactly one file added; content-level diff shows **0** changes to any source/config/test file; repository left clean.
- ✅ **All five Blitzy validation gates PASSED** (dependencies, compilation, unit tests, runtime, in-scope file) with **zero discrepancies**.

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|---|---|---|---|
| *(None blocking.)* No compilation, test, runtime, or scope defects were found; the deliverable passed all validation gates with zero discrepancies. | None — release path is a review gate, not a defect fix | — | — |
| Security verdict awaits human sign-off (governance gate, not a defect) | Verdict should not be treated as authoritative until independently reviewed | Security Engineer | ~4h (see §1.6 / Task HT-1) |

### 1.5 Access Issues

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|---|---|---|---|---|
| Git repository | Write / merge | None — branch present, HEAD `a92582bfd`, base `c07e5b49d477` resolvable | ✅ No issue | — |
| Go module cache | Build dependency | Warm module cache present; `go mod verify` = "all modules verified" offline | ✅ No issue | — |
| Client tooling (boto3, curl) | Runtime probes | Present (boto3 1.43.42, curl 8.14.1) | ✅ No issue | — |

**No access issues identified.** The environment self-hosts every dependency needed to rebuild and re-verify offline (no external credentials, network egress, or third-party API access is required).

### 1.6 Recommended Next Steps

1. **[High]** Have a security engineer independently review the verdict and methodology, and formally sign off before the finding is treated as authoritative (Task HT-1, ~4h).
2. **[Medium]** Spot-check reproduce the highest-signal probes — multi-delete per-key `AccessDenied`, the retention `Content-MD5` pre-gate, and the leakage boundary — plus the five coverage items not independently re-run during validation (Task HT-2, ~2h).
3. **[Medium]** Editorial/peer review: confirm all 43 `file:line` anchors resolve at HEAD and every raw-evidence block is complete/untruncated (Task HT-3, ~1h).
4. **[Low]** Merge and publish the single-file documentation PR after a final scope re-verification (Task HT-4, ~1h).

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

Every component below traces to one or more AAP requirement groups (R1–R20) and was delivered autonomously.

| Component | Hours | Description |
|---|---:|---|
| Authorization-boundary discovery & handler→policy-action mapping | 6 | Traced the 21-operation enforcement map and the `IAMSys.IsAllowed` dispatch (`cmd/iam.go:2437`), `checkRequestAuthType`/`isPutActionAllowed` (`cmd/auth-handler.go:339,749`). (R2, R15) |
| Canonical build & server runtime setup | 2 | Exact build (`go build -tags kqueue -trimpath`) & run commands, health checks, `DEVELOPMENT.GOGET` banner, on-disk backend layout. (R11) |
| Identity & policy provisioning harness (real admin API) | 4 | Provisioned `rouser` (custom prefix-scoped RO), `rocanned` (built-in `readonly`), `rwuser` (readwrite), `rwnoret` via `madmin-go`. (R2, R12) |
| Concurrent "under stress" load design & implementation | 3 | 12-thread writer churn (PutObject + tagging + multipart + delete) sustained during probes. (R3) |
| Write-adjacent probe matrix (multipart, copy, metadata, deletes) | 6 | 16+ signed operations exercised through the real S3 API; all denied `403`. (R4, R5, R6, R7) |
| Information-leakage probes (list / HEAD / attributes) | 3 | `ListObjectsV2/V1`, `ListObjectVersions`, `HeadObject`, `HeadBucket`, `GetObjectAttributes` characterized. (R8) |
| Evidence-capture infrastructure | 4 | Server-side `ServiceTrace` `[REQUEST]`/`[RESPONSE]`, client status + error XML, before/after backend snapshots. (R9, R13) |
| Cross-SDK verification (boto3 / botocore) | 3 | Independent SDK cross-check; discovered the pre-IAM `Content-MD5` gate. (R17) |
| Independent bypass hunt (6+ less-obvious surfaces) | 4 | Presigned PUT, POST-policy, version-targeted delete, S3 SELECT, restore, replication/batch. (R18) |
| ≥2-run stability replay & confirmation | 2 | Full matrix replayed twice under differing load; normalized transcript diff IDENTICAL, seeds byte-stable. (R14) |
| Authoring the deliverable document | 9 | 1,364 lines / 8 major sections with embedded raw evidence, tables, and the 32-item coverage matrix. (R1, R10, R16) |
| Source-anchor verification + coverage pass | 2 | 43 `file:line` references validated; §8.4 coverage matrix confirms every named item. (R15, R16) |
| QA review iteration + final validation reproduction | 3 | 3 commits (initial → QA findings → bypass-hunt/git-state); all 5 gates re-verified independently. |
| Cleanup & scope-compliance verification | 1 | Server stopped (exact pid), out-of-repo artifacts removed, content-only `git status` clean. (R19, R20) |
| **Total Completed** | **52** | |

### 2.2 Remaining Work Detail

All remaining work is **path-to-production** (human acceptance of a security finding). None represents an incomplete AAP deliverable or rework.

| Category | Hours | Priority |
|---|---:|---|
| Human security-engineer review & sign-off of verdict + methodology | 4 | High |
| Spot-check reproduction of key probes (multi-delete, `Content-MD5` gate, leakage boundary) + 5 not-yet-re-run coverage items | 2 | Medium |
| Editorial/peer review of document (anchor resolution, evidence completeness, readability) | 1 | Medium |
| Merge & publish PR (incl. final scope re-verification) | 1 | Low |
| **Total Remaining** | **8** | |

### 2.3 Hours Reconciliation

| Bucket | Hours |
|---|---:|
| Section 2.1 — Completed | 52 |
| Section 2.2 — Remaining | 8 |
| **Total (matches Section 1.2)** | **60** |

`52 + 8 = 60` ✓  ·  `Completion = 52 / 60 = 86.7%` ✓

---

## 3. Test Results

All tests below originate from **Blitzy's autonomous validation logs** for this project. Because the change is documentation-only, these are **regression/authorization tests executed against the unchanged MinIO reference codebase** to confirm the environment and the exact authorization code paths the investigation relies on. A doc-only change cannot alter source behavior; the suite is green.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---|---|---:|---:|---:|---|---|
| Focused authorization functions | Go `testing` | 7 | 7 | 0 | Auth path | `TestPolicySysIsAllowed`, `TestIsReqAuthenticated`, `TestGetRequestAuthType`, `TestCheckAdminRequestAuthType`, `TestValidateAdminSignature`, `TestS3SupportedAuthType`, `TestIsRequestPresignedSignatureV4` — all `ok` |
| Object-lock / retention package | Go `testing` | 1 pkg | 1 pkg | 0 | `internal/bucket/object/lock` | Package `ok` (WORM/retention enforcement, independent of IAM) |
| Handler & policy tests (`cmd`) | Go `testing` | 5+ | 5+ | 0 | Handlers | Multipart, post-policy, bucket-policy, signature, `TestObjectNewMultipartUpload` — all `ok` |
| End-to-end IAM policy enforcement | Go `testing` (suite) | 1 suite × 3 backends | 3 | 0 | ErasureSD / Erasure / ErasureSet | `TestIAMInternalIDPServerSuite` **PASS** across all three backends; etcd variants **SKIP** gracefully (no etcd) |
| Compile-all (build gate) | `go build ./...` | 1 | 1 | 0 | Whole module | `CGO_ENABLED=0 go build -tags kqueue,dev ./...` → exit 0 (no repo artifact emitted) |

**Summary:** All executed test groups **passed**; **zero failures**. Per-assertion counts beyond the named functions/suites were not individually enumerated in the validation logs; statuses shown (`ok`/`PASS`/`SKIP`) are exactly as reported. The green result confirms the authorization primitives (`IsAllowed`, `checkRequestAuthType`, `isPutActionAllowed`) and object-lock enforcement behave as the investigation documents.

---

## 4. Runtime Validation & UI Verification

**UI Verification:** ⚠ **Not applicable** — this project delivers a server-side security investigation and one markdown document. There is **no UI deliverable** in scope. Runtime validation below focuses on server health and the S3/admin **API** surface, which *is* the boundary under test.

**Runtime health & provisioning**
- ✅ **Operational** — Canonical server launched: `/tmp/minio server /tmp/minio-data --address :9000`; banner `DEVELOPMENT.GOGET`, `Runtime: go1.23.2`. Health `live`/`ready`/`cluster` → `200`.
- ✅ **Operational** — Identities provisioned via the **real admin API** (`madmin-go`): `rouser` (custom prefix-scoped RO policy, read back verbatim), `rocanned` (built-in `readonly`), `rwuser` (readwrite).
- ✅ **Operational** — Concurrent "under stress" load: 12 writer goroutines drove the shared bucket across two runs (differing volumes) while probes executed.

**Read-only probe matrix (real signed S3 API) — authorization outcomes**
- ✅ **Boundary holds** — Multipart (Create / UploadPart / UploadPartCopy / Complete / Abort / ListParts / ListMultipartUploads) → **all `403 AccessDenied`**; no object, no orphan upload.
- ✅ **Boundary holds** — Copy-style (`CopyObject`, `UploadPartCopy`) → **`403`**; no object created.
- ✅ **Boundary holds** — Metadata (`PutObjectTagging`, `DeleteObjectTagging`, `PutObjectRetention`, `PutObjectLegalHold`, `PutObjectAcl`) → **`403`** (retention reaches IAM only with `Content-MD5`, then `403`).
- ✅ **Boundary holds** — Deletes: single `DeleteObject` → **`403`** (object intact); multi `DeleteObjects` → **`200` with per-key `AccessDenied`**, all seeds intact (documented nuance).
- ✅ **Leakage bounded** — In-prefix `GetObject`/`HeadObject`/`ListObjectsV2` → `200` (existence, size, ETag, content-type); out-of-prefix → `403`; `HeadBucket` → `403`; `GetObjectAttributes` → `403`.
- ✅ **Documented gap** — `rocanned` (built-in `readonly`) `ListBucket` → `403` (no `ListBucket` action in the canned policy).

**Storage side-effect proof**
- ✅ **No mutation** — All distinctive read-only write targets **absent** in the backend across both runs; seed objects byte-stable (identical size/ETag/last-modified); `ro-prefix/` snapshot `SNAPSHOT_IDENTICAL` before vs. after.
- ✅ **Timing evidence** — Deny latencies sub-millisecond (80–330 µs), i.e. denial lands **before** the object layer, independent of concurrent load.

**Cross-SDK & bypass hunt**
- ✅ **Operational** — boto3/botocore 1.43.42 reproduced core denials; `Content-MD5` pre-gate reproduced (retention `400 MissingContentMD5` without MD5, `403` with MD5; multi-delete `400` without MD5).
- ✅ **Boundary holds** — presigned-URL PUT → `403` (no write); version-targeted `DeleteObject` → `403` (version intact); `SelectObjectContent` → in-grant `200` (a *read*) / out-of-grant `403`; `RestoreObject` → `403`.

---

## 5. Compliance & Quality Review

Cross-mapping the AAP's governing rules (SWE-AtlasQnA-Repo) and quality benchmarks to observed evidence.

| Benchmark / Rule | Requirement | Status | Evidence / Fixes Applied |
|---|---|---|---|
| Deliverable rule (§0.7.1) | Single answer doc `blitzy/documentation/<branch>.md` | ✅ Pass | `minio_c07e5b49d477.md` present; only repo write |
| Run-first (§0.7.2) | Build & run before writing; canonical config | ✅ Pass | Exact build/run cmds in §2; reproduced (build exit 0, banner live) |
| Real entry points (§0.7.2) | Signed S3 + real admin API only, no synthetic stand-ins | ✅ Pass | `madmin-go` provisioning; signed `minio-go`/boto3/curl probes |
| Exercise every condition (§0.7.2) | Every named op + edge/transitional paths | ✅ Pass | 32-item coverage matrix (§8.4); multi-delete & `Content-MD5` edges captured |
| Before/during/after state (§0.7.2) | Capture state around each write | ✅ Pass | Backend snapshots + authorized re-list; `SNAPSHOT_IDENTICAL` |
| ≥2-run stability (§0.7.2) | Repeat, confirm stable distribution | ✅ Pass | Two runs, differing load, normalized diff IDENTICAL (§7) |
| Evidence discipline (§0.7.3) | Actual complete unedited output per claim | ✅ Pass | Raw `[REQUEST]`/`[RESPONSE]`, full error XML, status codes embedded |
| Grounding (§0.7.3) | `file:line` and/or observed output for every claim | ✅ Pass | 43 anchors; 7/7 spot-verified accurate at HEAD |
| Coverage pass (§0.7.3) | Confirm each named item answered | ✅ Pass | §8.4 matrix — 32 items |
| Direct-answer-first (§0.7.3) | Lead with verdict, layer nuance | ✅ Pass | §1 direct answer; nuances in §6 |
| Read-only scope (§0.7.4) | No source modified; temp scripts removed | ✅ Pass | Content-only diff = 1 file; no untracked; artifacts deleted |
| Markdown quality | Well-formed, no placeholders | ✅ Pass | 98 balanced fences, clean EOF, no TODO/FIXME |
| Dependency integrity (§0.6) | No dependency changes | ✅ Pass | `go.mod`/`go.sum` untouched; `go mod verify` clean |

**Fixes applied during autonomous validation:** none required — the investigation's evidence, verdict, and anchors were corroborated on first reproduction (zero discrepancies). Earlier QA iteration (commit `469e775b4`) addressed review findings and commit `a92582bfd` added the independent bypass-hunt evidence, prior to final validation.

**Outstanding compliance items:** none autonomous. Governance sign-off (human) is tracked in §1.6 / §2.2.

---

## 6. Risk Assessment

Overall posture: **LOW** (documentation-only, read-only investigation, all gates passed, zero rework). No High-severity risks.

| Risk | Category | Severity | Probability | Mitigation | Status |
|---|---|---|---|---|---|
| Over-reliance on the verdict before human review | Security | Medium | Low | Verdict is evidence-backed + `file:line` grounded; High-priority security sign-off is the top remaining task (HT-1) | Open (by-design human gate) |
| Over-generalization of a narrowly-scoped finding | Security | Low | Medium | Doc §6.7 states the thesis narrowly; documents built-in `readonly` `ListBucket` gap and object-lock independence | Documented |
| Sensitive fields (SigV4 `Authorization`) in captured traces | Security | Low | Low | Artifacts kept out-of-repo and deleted; default throwaway creds (`minioadmin`) | Mitigated |
| Offline reproducibility friction (cold module cache) | Technical | Low | Medium | Exact commands + offline flags documented; module cache warmed; `go mod verify` clean | Mitigated |
| `DEVELOPMENT.GOGET` banner misread as a defect | Technical | Low | Low | Doc §2.3 explicitly labels it a plain-build artifact (Makefile ldflags absent) | Resolved (documented) |
| Five coverage items not independently re-run by validator (17,18,28,29,31) | Technical | Low | Low | Documented with raw evidence in-doc; covered by spot-check task HT-2 | Open (minor) |
| SDK-version-dependent pre-IAM behavior (`Content-MD5` gate) | Integration | Low | Medium | Doc pins exact versions and explains botocore-1.43 checksum behavior | Documented |
| Repo mode-bit noise (1305 files `100644→100755`) in raw `git status` | Operational | Low | Medium | Content-only diff clean (1 file); use `git -c core.fileMode=false`; documented as environmental | Documented / benign |
| No CI gate validates the prose document | Operational | Low | Low | Doc-only change cannot break build/tests; markdown well-formed | Accepted |

---

## 7. Visual Project Status

### 7.1 Project Hours (Completed vs Remaining)

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextSize':'15px','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#B23AF2','pieSectionTextSize':'13px','pieLegendTextColor':'#B23AF2'}}}%%
pie showData title Project Hours — 60h total (86.7% complete)
    "Completed Work" : 52
    "Remaining Work" : 8
```

### 7.2 Remaining Work by Priority (8h)

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#A8FDD9','pie3':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextSize':'15px','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#111111','pieSectionTextSize':'13px','pieLegendTextColor':'#B23AF2'}}}%%
pie showData title Remaining Hours by Priority
    "High" : 4
    "Medium" : 3
    "Low" : 1
```

### 7.3 Remaining Work by Category (bar view)

| Category | Hours | Bar |
|---|---:|---|
| Security-engineer review & sign-off | 4 | ████████ |
| Spot-check reproduction of key probes | 2 | ████ |
| Editorial/peer review | 1 | ██ |
| Merge & publish PR | 1 | ██ |
| **Total Remaining** | **8** | |

> **Integrity:** the "Remaining Work" value (**8h**) is identical in Section 1.2, Section 2.2, and the §7.1 pie chart.

---

## 8. Summary & Recommendations

**Achievements.** The investigation delivers a clear, evidence-backed answer: **the MinIO read-only IAM boundary holds under concurrent write stress — there is no bypass.** Across two runs under genuine 12-thread writer load and two independent SDKs, a faithfully read-only principal was denied on every write-adjacent surface (multipart, copy-style, metadata, deletes) and on every less-obvious surface probed (presigned, POST-policy, version-targeted delete, SELECT, restore, replication/batch), with **no storage side effect** in any case. The only `200`s were genuine prefix-scoped reads. The causal mechanism is grounded in source: the IAM decision (`IAMSys.IsAllowed`, `cmd/iam.go:2437`) is evaluated at handler entry (`checkRequestAuthType`/`isPutActionAllowed`, `cmd/auth-handler.go:339,749`) **before** the object/erasure layer, and is per-request and independent of other clients' load.

**Remaining gaps.** None are defects. The project is **86.7% complete**; the remaining **8 hours** are path-to-production human activities — a security engineer's independent review/sign-off (the dominant item), a spot-check reproduction, an editorial pass, and the merge.

**Critical path to production.** (1) Security sign-off → (2) spot-check reproduction of the highest-signal probes and the five not-yet-re-run coverage items → (3) editorial/anchor-resolution pass → (4) merge the single-file PR.

**Production-readiness assessment.** The autonomous deliverable is **complete and internally validated** (all five gates passed, zero discrepancies, repository clean except the one document, build/run reproduced byte-for-byte). It is ready for human security review; treating the verdict as authoritative is appropriately gated behind that sign-off.

| Success Metric | Target | Observed |
|---|---|---|
| Named items covered | 100% | 32/32 (§8.4) |
| Write-adjacent probes denied | All | All `403`, no side effect |
| Run-to-run stability | Stable across ≥2 runs | Identical; seeds byte-stable |
| Source anchors accurate | 100% | 43 refs; 7/7 spot-verified |
| Repository scope | 1 file only | 1 file (content-only diff) |
| Validation gates | All pass | 5/5 pass, 0 discrepancies |

---

## 9. Development Guide

> Every command below was executed and verified in this environment. All build/runtime artifacts live **outside** the repository (`/tmp`), per the read-only scope rule.

### 9.1 System Prerequisites

- **Go 1.23.x** — verified `go version` → `go1.23.2 linux/amd64` (matches `go.mod: go 1.23`). Located at `/usr/local/go/bin`.
- **Disk:** ~2 GB free (≈157 MB binary + server data directory).
- **OS:** Linux (validated on Ubuntu 25.10 container) or macOS.
- **Optional cross-check clients:** `python3` 3.13.7 + `boto3` 1.43.42; `curl` 8.14.1.

### 9.2 Environment Setup

```bash
# Ensure Go is on PATH
export PATH=$PATH:/usr/local/go/bin

# Offline-friendly build environment (warm module cache present at /root/go/pkg/mod)
export GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off CGO_ENABLED=0
```

> **Python clients (optional):** this is a PEP-668 "externally-managed" system Python. Use a venv (`python3 -m venv .venv && source .venv/bin/activate`) or `pip install --break-system-packages boto3` if boto3 is not already present.

### 9.3 Dependency Verification

```bash
cd /tmp/blitzy/minio/blitzy-b8fad361-f2c2-4f2e-90a6-30ee12dd6565_6c2b36
go mod verify        # → "all modules verified"
```

### 9.4 Build (canonical, verbatim)

```bash
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio .
# → exit 0; produces a statically-linked ELF of 156,592,469 bytes

/tmp/minio --version
# → minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
# → Runtime: go1.23.2 linux/amd64
```

> **Note:** the `DEVELOPMENT.GOGET` banner is expected for a plain `go build` (the Makefile normally stamps the version via ldflags). It is a build-configuration artifact, **not** a defect.

### 9.5 Run (canonical) & Health Verification

```bash
/tmp/minio server /tmp/minio-data --address :9000 &   # default creds minioadmin:minioadmin
# Verify health (each returns HTTP 200):
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/live
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/ready
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/cluster
```

### 9.6 Reproduce the Investigation (example usage)

1. Provision identities via the admin API (`madmin-go` or `mc admin`): create `rouser` with a **custom prefix-scoped read-only policy** — `Allow s3:GetObject` on `arn:aws:s3:::probe-bucket/ro-prefix/*` and `Allow s3:ListBucket` on `arn:aws:s3:::probe-bucket` with `Condition StringLike s3:prefix ["ro-prefix/*"]`.
2. Seed objects under `ro-prefix/` as root; start a concurrent writer load (root/readwrite principal).
3. With `rouser`, drive each write-adjacent operation through a **signed** S3 client and capture the HTTP status + error XML.
4. Capture server-side traces: `mc admin trace --verbose <alias>` (backed by the `ServiceTrace` API) — note the **8 concurrent subscriber** limit.
5. Snapshot the backend before/after each write probe: objects persist under `/tmp/minio-data/<bucket>/<object>/xl.meta`. An authorized re-list confirms the object/tag/version set is unchanged.

### 9.7 Run the Reference Unit Tests (optional)

```bash
env -u KUBERNETES_SERVICE_HOST CGO_ENABLED=0 MINIO_API_REQUESTS_MAX=10000 \
  go test -tags kqueue,dev ./...
```

### 9.8 Troubleshooting

- **`error: externally-managed-environment` (pip):** use a venv or `--break-system-packages`.
- **Noisy `git status` (1000s of `100644→100755` entries):** environmental mode-bit artifact; view the true content diff with `git -c core.fileMode=false status` / `... diff c07e5b49d477`.
- **Port `:9000` already in use:** change `--address :PORT`.
- **Offline build failure:** ensure the module cache is warm; keep `GOPROXY=off` only when the cache is present, otherwise unset it.
- **`mc admin trace` shows nothing / errors:** you may have hit the 8-subscriber trace limit; reduce concurrent trace clients.

### 9.9 Cleanup (leave the repository unchanged)

```bash
# Stop ONLY the server you started (never pkill broadly — that can kill the orchestrator)
kill <the-exact-minio-pid>
rm -rf /tmp/minio /tmp/minio-data /tmp/harness /tmp/*.log
git -c core.fileMode=false status --porcelain   # → only the answer document
```

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---|---|
| Go version | `go version` |
| Verify dependencies (offline) | `go mod verify` |
| Canonical build | `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio .` |
| Compile-all gate | `CGO_ENABLED=0 go build -tags kqueue,dev ./...` |
| Version banner | `/tmp/minio --version` |
| Run server | `/tmp/minio server /tmp/minio-data --address :9000` |
| Health check | `curl http://127.0.0.1:9000/minio/health/{live,ready,cluster}` |
| Unit tests | `env -u KUBERNETES_SERVICE_HOST CGO_ENABLED=0 MINIO_API_REQUESTS_MAX=10000 go test -tags kqueue,dev ./...` |
| Content-only diff vs base | `git -c core.fileMode=false diff c07e5b49d477 --stat` |
| Server-side trace | `mc admin trace --verbose <alias>` |

### B. Port Reference

| Port | Service | Notes |
|---|---|---|
| `9000` | MinIO S3 API | Set via `--address :9000`; the boundary under test |
| *(ephemeral)* | MinIO Console (WebUI) | Random ephemeral port unless `--console-address` is set; not used by this investigation |

### C. Key File Locations

| Path | Role |
|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (only repository write) |
| `cmd/iam.go` (`:2437`) | `IAMSys.IsAllowed` — policy decision |
| `cmd/auth-handler.go` (`:339`, `:749`) | `checkRequestAuthType`, `isPutActionAllowed` |
| `cmd/object-multipart-handlers.go` | Multipart handlers |
| `cmd/object-handlers.go` | Copy / delete / tagging / retention / legal-hold / HEAD / attributes |
| `cmd/bucket-handlers.go` | Multi-delete, list-multipart-uploads, HeadBucket |
| `cmd/bucket-listobjects-handlers.go` | ListObjects V1/V2/Versions |
| `cmd/acl-handlers.go` (`:172`) | `PutObjectACLHandler` (`PutBucketPolicyAction`) |
| `cmd/bucket-object-lock.go` | Object-lock/WORM (independent of IAM) |
| `/tmp/minio`, `/tmp/minio-data` | Out-of-repo build/runtime artifacts (ephemeral) |

### D. Technology Versions

| Component | Version | Source |
|---|---|---|
| Go toolchain | 1.23.2 | `go version` (go.mod declares `go 1.23`) |
| github.com/minio/minio-go/v7 | v7.0.80 | `go.mod` |
| github.com/minio/madmin-go/v3 | v3.0.77 | `go.mod` |
| github.com/minio/pkg/v3 | v3.0.22 | `go.mod` (built-in `readonly` policy) |
| boto3 | 1.43.42 | environment |
| curl | 8.14.1 | environment |
| MinIO build banner | DEVELOPMENT.GOGET | plain `go build` (ldflags not stamped) |

### E. Environment Variable Reference

| Variable | Value used | Purpose |
|---|---|---|
| `CGO_ENABLED` | `0` | Static, CGO-free build |
| `GOFLAGS` | `-mod=mod` | Use module graph as-is |
| `GOPROXY` | `off` | Offline build from warm cache |
| `GOSUMDB` | `off` | Skip checksum DB (offline) |
| `MINIO_API_REQUESTS_MAX` | `10000` | Avoid request throttling during tests |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | default `minioadmin` | Default root credentials (canonical run) |

### F. Developer Tools Guide

- **`mc admin trace`** — canonical server-side `[REQUEST]`/`[RESPONSE]` capture (SigV4 `Authorization` header, method, path, status, timing); supports `--errors`, `--status-code`, `--method`, `--path`; server caps at **8** concurrent subscribers.
- **`minio-go/v7`** — signed S3 client for read-only probes and concurrent write load.
- **`madmin-go/v3`** — admin API to create users/policies and stream the `ServiceTrace` feed.
- **`boto3`** — independent S3 SDK for cross-SDK corroboration (surfaced the `Content-MD5` pre-gate).
- **`curl`** — raw SigV4 requests for byte-level request/response inspection.

### G. Glossary

| Term | Meaning |
|---|---|
| **Read-only principal** | Identity intended to have only read access to a bucket/prefix (`rouser`) |
| **Write-adjacent** | S3 operations that feel like reads but can mutate state (multipart, copy, tagging, retention, ACL, deletes) |
| **Prefix-scoped policy** | Custom policy pairing `s3:GetObject` on `bucket/prefix/*` with `s3:ListBucket` gated by `s3:prefix` condition |
| **Built-in `readonly`** | Canned MinIO policy granting only `GetBucketLocation` + `GetObject` (no `ListBucket`) |
| **`xl.meta`** | Per-object metadata file in the erasure backend used for storage side-effect proofs |
| **`ServiceTrace`** | Admin API feed backing `mc admin trace` |
| **`DEVELOPMENT.GOGET`** | Version banner emitted by a plain `go build` without Makefile ldflags |
| **Storage side effect** | An observable change to backend state (new object, deleted object, mutated tag/version) |