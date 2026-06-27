# Blitzy Project Guide — MinIO Runtime Security Investigation (`minio_c07e5b49d477`)

> Branch under investigation: `minio_c07e5b49d477` · MinIO HEAD `c07e5b49d477b0774f23db3b290745aef8c01bd2`
> Deliverable: `blitzy/documentation/minio_c07e5b49d477.md` · Branch head: `ee9cf0825`
> Brand colors — **Completed/AI = Dark Blue `#5B39F3`** · **Remaining = White `#FFFFFF`** · Accents `#B23AF2` · Highlight `#A8FDD9`

---

## 1. Executive Summary

### 1.1 Project Overview

This project is a **read-only, code-as-truth investigation** of the MinIO object-storage server (Go). The objective was to author a single evidence-backed Markdown report that answers five distinct runtime-security questions — server-side encryption precedence over IAM, Object-Lock (WORM) delete enforcement, bitrot detection & self-healing, STS session-policy intersection, and IAM privilege-escalation prevention — with every behavioral claim anchored to a verified source `file:line` and substantiated by captured runtime trace/test evidence. The audience is security and platform engineers who need authoritative, reproducible answers. The MinIO source tree was treated as a strictly read-only reference corpus; the sole artifact produced is the report in the destination repository's `blitzy/documentation/` directory.

### 1.2 Completion Status

The completion percentage is computed strictly on AAP-scoped work plus path-to-production, using the hours-based PA1 methodology: **Completed Hours ÷ Total Hours**.

```mermaid
%%{init: {"theme":"base","themeVariables":{"pie1":"#5B39F3","pie2":"#FFFFFF","pieStrokeColor":"#B23AF2","pieOuterStrokeColor":"#B23AF2","pieStrokeWidth":"2px","pieOuterStrokeWidth":"2px","pieSectionTextColor":"#111111","pieTitleTextColor":"#5B39F3","pieLegendTextColor":"#111111"}}}%%
pie showData title MinIO Investigation — 88.0% Complete
    "Completed Work (AI)" : 44
    "Remaining Work" : 6
```

| Metric | Hours |
|--------|-------|
| **Total Hours** | **50** |
| **Completed Hours (AI + Manual)** | **44** (AI = 44, Manual = 0) |
| **Remaining Hours** | **6** |
| **Percent Complete** | **88.0%** (44 ÷ 50) |

> Calculation: `Completion % = Completed (44) ÷ Total (50) = 88.0%`. All five AAP investigation requirements are fully delivered and independently validated; the 6 remaining hours are path-to-production activities that require a human (review/acceptance, re-verification, housekeeping).

### 1.3 Key Accomplishments

- ✅ **Single deliverable produced** — `blitzy/documentation/minio_c07e5b49d477.md` (784 lines, ~45.7 KB, 36 evidence code blocks, ~50 unique `file:line` citations).
- ✅ **All five security questions answered** with the mandated structure: verbatim question → reproduction → captured evidence → code-truth root cause → rationale.
- ✅ **Req 1 — Encryption precedence:** proved SSE is enforced *after* and *independently of* IAM authorization; an `s3:*` principal's unencrypted `PutObject` is transparently stored as SSE-KMS (the `SignedHeaders` set proves the SSE header was server-injected, not client-signed).
- ✅ **Req 2 — Object-Lock DELETE:** captured the exact surface — single delete → **HTTP 400 `InvalidRequest`** "Object is WORM protected and cannot be overwritten"; multi-delete → HTTP 200 batch with per-object `<Error>`.
- ✅ **Req 3 — Bitrot detection:** corrupted a backend shard, proved the GET returns byte-identical data via erasure reconstruction and a deep heal restores the shard; documented the honest "quiet success path" finding.
- ✅ **Req 4 — STS intersection:** proved temporary credentials are constrained to the **intersection** of parent and inline session policy via four corroborating evidence layers (Go test PASS, two `mc admin trace` blocks, JWT `sessionPolicy` claim decode).
- ✅ **Req 5 — Escalation prevention + root cause:** `TestIAMInternalIDPServerSuite` PASS; root cause established — `AddUser` builds identity from `auth.Credentials` only and never reads `PolicyName`, with policy attachment decoupled behind admin-only actions.
- ✅ **Code-as-truth discipline upheld** — four AAP assumptions were corrected by the actual source ("CODE WINS"); an independent 6/6 citation spot-check this session was byte-accurate.
- ✅ **Source-tree integrity preserved** — `git status --porcelain` is empty; `git diff c07e5b49d..HEAD` adds **only** the one report file.

### 1.4 Critical Unresolved Issues

There are **no critical unresolved issues**. The deliverable is complete, citation-accurate, and independently validated (all five validation gates pass). The items below are standard path-to-production gates, not defects.

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| Human SME review & acceptance of the report has not yet occurred | None to correctness; standard acceptance gate before merge | Security/Platform reviewer | 3h |
| Reproduction not yet re-verified in a fresh (non-Blitzy) environment | Low; confidence/assurance only — evidence already reproduced twice autonomously | Reviewer/QA | 2h |
| 4 gitignored debugging binaries left on disk (`hash-set`, `healing-bin`, `s3-check-md5`, `xl-meta`) | Cosmetic; not committed, do not affect git integrity | Developer | 1h |

### 1.5 Access Issues

**No access issues identified.** The toolchain (Go 1.23.12), the `mc` client (`RELEASE.2025-08-13T08-35-41Z`), `boto3`, `git`/`git-lfs`, and the warm Go module cache were all present and usable. The build resolved all modules offline with zero network access, and no repository permissions, service credentials, or third-party API access were missing.

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-----------------|----------------|-------------------|-------------------|-------|
| MinIO source repo | Read | None — read-only reference corpus accessed successfully | ✅ Resolved | — |
| Go module cache | Build | None — warm cache, offline build succeeded | ✅ Resolved | — |
| `mc` admin / KMS (local) | Runtime | None — local KMS key + admin trace used successfully | ✅ Resolved | — |

### 1.6 Recommended Next Steps

1. **[High]** Have a security/platform SME review and sign off on the report — read the five findings, spot-check a sample of the `file:line` citations against HEAD `c07e5b49d`, and confirm the four CODE-WINS corrections. (≈3h)
2. **[Medium]** Independently re-verify reproduction in a clean environment: `make build`, run `TestIAMInternalIDPServerSuite`, and spot-check one or two trace scenarios (Req 1 and/or Req 4). (≈2h)
3. **[Low]** Remove the four leftover gitignored debugging binaries to keep the working tree clean. (≈1h)
4. **[Low]** Merge the deliverable once review sign-off is recorded.

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

Every completed component traces to a specific AAP requirement (the five questions, the implicit build/run, the code-as-truth and cleanup constraints, and the web-search corroboration directive).

| Component | Hours | Description |
|-----------|-------|-------------|
| Environment setup & build | 3 | Go 1.23.12 toolchain; `make build` → `./minio` (+ debugging tools); `mc` alias + `boto3`; throwaway `/tmp` 4-drive erasure data dirs |
| Req 1 — Encryption precedence | 5 | KMS auto-encryption configuration; unencrypted `PutObject` under broad `s3:*`; `mc admin trace -v` capture (server-injected SSE proven via `SignedHeaders`); root cause across `object-handlers.go`/`iam.go`/`bucket-sse-config.go` |
| Req 2 — Object-Lock DELETE | 5 | `--with-lock` bucket + COMPLIANCE retention; single + multi delete paths; HTTP 400 / 200-batch evidence; CODE-WINS #1 (400 not 403); error-mapping trace |
| Req 3 — Bitrot detection & heal | 6 | On-disk shard corruption (`dd`); GET reconstruction proof; `mc admin heal --scan deep` repair; honest quiet-success-path analysis; root cause `bitrot.go`/`erasure-object.go` |
| Req 4 — STS session-policy intersection | 6 | Ephemeral `/tmp` Go reproduction; four corroborating evidence layers (test PASS, AssumeRole `Policy=` trace, STS PutObject 403, JWT `sessionPolicy` decode); CODE-WINS #3/#4 |
| Req 5 — Privilege-escalation + root cause | 5 | `TestIAMInternalIDPServerSuite` run (PASS); `AddUser`/`PolicyName` root-cause analysis at the store layer; CODE-WINS #2 (LDAP DN-normalization mis-citation) |
| Code-as-truth citations & anchor map | 3 | ~50 `file:line` anchors verified against source; master anchor map + evidence⇄citation appendix |
| Web-search corroboration | 2 | `mc admin trace` tooling + STS intersection/2048 semantics validated against MinIO and AWS documentation |
| Report authoring, structure & review revisions | 4 | 784-line Markdown; preamble/methodology/CODE-WINS table; review-finding revision pass (commit `1f9023896`, +164/−55) |
| Autonomous validation & QA | 4 | Independent live re-reproduction of all five requirements; full citation audit; one line-range fix (commit `ee9cf0825`) |
| Cleanup & source-tree integrity | 1 | Temporary-artifact removal; `git status` verification (empty) |
| **TOTAL COMPLETED** | **44** | **Matches Section 1.2 Completed Hours** |

### 2.2 Remaining Work Detail

Each remaining category is a path-to-production activity requiring a human. There are **no blocking code fixes** (no compilation errors, no failing tests, no missing functionality).

| Category | Hours | Priority |
|----------|-------|----------|
| Review & Acceptance — SME sign-off of the five findings + `file:line` citation spot-check | 3 | High |
| Reproduction Re-verification — `make build` + `TestIAMInternalIDPServerSuite` + 1–2 trace spot-checks in a fresh environment | 2 | Medium |
| Repository Housekeeping — remove the 4 leftover gitignored debugging binaries | 1 | Low |
| **TOTAL REMAINING** | **6** | **Matches Section 1.2 Remaining Hours & Section 7 pie** |

### 2.3 Hours Reconciliation

| Check | Result |
|-------|--------|
| Section 2.1 total (Completed) | 44 |
| Section 2.2 total (Remaining) | 6 |
| 2.1 + 2.2 | **50 = Total Project Hours (Section 1.2)** ✅ |
| Completion % | 44 ÷ 50 = **88.0%** ✅ |

---

## 3. Test Results

All entries below originate exclusively from Blitzy's autonomous validation logs for this project (compilation gate, test gate, and runtime gate). Because this is a targeted investigation rather than a feature build, the goal was specific evidence reproduction, not aggregate coverage; coverage % is therefore reported as **N/A** (not a metric of this task).

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|------------|-------|
| IAM/STS Suite (Req 5) | Go test (`-tags kqueue,dev`) | 8 sub-configs | 4 | 0 | N/A | `TestIAMInternalIDPServerSuite` PASS (~14s); 4 backend configs PASS + 4 etcd configs SKIP; invokes `TestUserPolicyEscalationBug` → self-promotion **prevented** |
| STS Session-Policy Intersection (Req 4) | Go test (ephemeral `/tmp`, `minio-go` creds) | 1 | 1 | 0 | N/A | `TestInlineSTSSessionPolicyIntersection` PASS (0.27s); parent `s3:*` PutObject succeeds, STS PutObject **denied 403** → intersection proven |
| Compilation — server (Req 1–3 binary) | `go build` (`make build`) | 1 | 1 | 0 | N/A | `make build` → exit 0, produced `./minio` (runtime go1.23.12, commit-id matched HEAD) |
| Compilation — test package (Req 4–5) | `go test -c ./cmd` | 1 | 1 | 0 | N/A | exit 0 — the package that yields Req 4/5 evidence compiles cleanly |
| **TOTAL** | — | **11** | **7** | **0** | **N/A** | **0 failures across all autonomous validation gates** |

> Integrity note: No tests were authored into the source tree (read-only constraint). The Req 4 intersection test was an ephemeral `/tmp` reproduction (deleted after capture); its PASS output is preserved in the report. The Req 5 evidence is an existing in-tree suite, run unmodified.

---

## 4. Runtime Validation & UI Verification

**UI Verification: Not applicable.** All five questions concern server/back-end behavior (encryption, object lock, data integrity, STS, IAM). No user interface was built, modified, or required.

**Runtime Validation** — every requirement was reproduced against a live, source-built MinIO and matched the report:

- ✅ **Operational — Req 1 (Encryption precedence):** auto-encryption server; unencrypted `PutObject` by an `s3:*` user → `200 OK` + server-injected `x-amz-server-side-encryption: aws:kms`; trace `SignedHeaders` excludes the SSE header (server-injected, broad write did **not** bypass).
- ✅ **Operational — Req 2 (Object-Lock DELETE):** COMPLIANCE-retained single DELETE → **HTTP 400 `InvalidRequest`** "Object is WORM protected and cannot be overwritten"; multi-delete → **HTTP 200** batch with per-object `<Error>`.
- ✅ **Operational — Req 3 (Bitrot):** zeroed a 128 KiB region of a 2+2 shard → GET returned byte-identical data (erasure reconstruction); `mc admin heal --scan deep` → shard restored byte-for-byte; success path is quiet (failure-only log strings), as documented.
- ✅ **Operational — Req 4 (STS intersection):** STS-credentialed `PutObject` → **403 `AccessDenied`** while the parent policy grants `s3:*`; List/Get succeed → intersection proven.
- ✅ **Operational — Req 5 (Escalation prevention):** `TestIAMInternalIDPServerSuite` PASS; `RemoveBucket` remains `"Access Denied."` after the add-user attempt → no privilege gained.
- ✅ **Operational — Build & toolchain:** `make build` exit 0; `./minio` runs and reports the matching commit-id.
- ✅ **Operational — Source-tree integrity:** `git status --porcelain` empty; only the report file added.

---

## 5. Compliance & Quality Review

This section cross-maps each AAP deliverable and governing rule ("SWE-AtlasQnA-Repo") to its compliance status, including fixes applied during autonomous validation.

| AAP Deliverable / Rule | Benchmark | Status | Progress | Evidence / Notes |
|------------------------|-----------|--------|----------|------------------|
| Single deliverable in `blitzy/documentation/` | Exactly one new file | ✅ Pass | 100% | `git diff c07e5b49d..HEAD` = `A blitzy/documentation/minio_c07e5b49d477.md` only |
| Source tree strictly read-only | No edits to `cmd/**`, `internal/**`, `docs/**`, `Makefile`, `go.mod`/`go.sum`, CI, tests | ✅ Pass | 100% | `git status --porcelain` empty; diff = 1 file added |
| Code-as-truth, no assumptions | Every claim cites a verified `file:line` | ✅ Pass | 100% | ~50 unique citations; 6/6 independent spot-check byte-accurate |
| Req 1 — Encryption precedence | Reproduction + runtime trace + root cause + rationale | ✅ Pass | 100% | §1 of report; reproduced live |
| Req 2 — Object-Lock DELETE | Reproduction + runtime log + root cause + rationale | ✅ Pass | 100% | §2 of report; reproduced live |
| Req 3 — Bitrot detection | Reproduction + runtime log + root cause + rationale | ✅ Pass | 100% | §3 of report; reproduced live |
| Req 4 — STS session-policy | Runtime **test** output proving intersection | ✅ Pass | 100% | §4; ephemeral test PASS + trace + JWT decode |
| Req 5 — Escalation prevention + root cause | Runtime **test** output + named root cause | ✅ Pass | 100% | §5; suite PASS + `PolicyName`-ignored root cause |
| Web-search corroboration | Validate `mc admin trace` + STS semantics vs official docs | ✅ Pass | 100% | Per-section "documentation corroboration" |
| Cleanup & integrity | Temp artifacts removed; `git status` clean | ✅ Pass | 100% | §6 of report; verified empty (cosmetic: 4 gitignored binaries remain on disk) |

**Fixes applied during autonomous validation:**
- **CODE-WINS #1** — corrected `ErrObjectLocked` HTTP status: **400 `InvalidRequest`** (not 403). Verified at `cmd/api-errors.go:L1059-L1063`.
- **CODE-WINS #2** — corrected attribution: `cmd/iam.go:L1770` is LDAP DN-normalization; the admin-gated `SetPolicyForUserOrGroup` is in `cmd/admin-handlers-users.go`. Root cause unaffected.
- **CODE-WINS #3** — corrected location: `maxSTSSessionPolicySize = 2048` is at `cmd/sts-handlers.go:L89` (not `globals.go`).
- **CODE-WINS #4** — corrected test invocation: `TestSTS`/`TestUserPolicyEscalationBug` are `*TestSuiteIAM` methods; the inline-session-policy intersection required a dedicated reproduction.
- **Line-range fix** — `AddUser` function body corrected to `L2659-L2692` (commit `ee9cf0825`).

**Outstanding compliance items:** none blocking. Cosmetic only: 4 gitignored debugging binaries remain on disk (housekeeping).

---

## 6. Risk Assessment

Risk profile is dominated by reproducibility/accuracy, not runtime defects — the source tree is pristine, no code changed, and the deliverable was independently validated. **All risks are Low severity; none block acceptance.**

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Citation line-drift vs future MinIO versions | Technical | Low | Medium | Report pins exact HEAD `c07e5b49d` in title, preamble, and every claim | Mitigated (by design) |
| Ephemeral STS reproduction not re-runnable from the repo | Technical | Low | Medium | Full reproduction code/steps documented; validator re-reproduced live | Mitigated |
| Runtime-evidence formatting drift across environments | Technical | Low | Low | Component versions pinned; behavioral assertions (status codes, PASS) are version-stable | Mitigated |
| Security-analysis artifact handling | Security | Low | Low | Findings align with public MinIO docs; escalation proven *prevented*; no novel vuln disclosed | Accepted/Mitigated |
| Secrets/credentials in report | Security | Low | Low | Only throwaway creds (`minioadmin`, `writer12345`); JWT is from ephemeral test creds | Mitigated |
| Leftover on-disk debugging binaries | Operational | Low | High | Housekeeping task to remove (gitignored, never committed) | **Open** (Low — Task L1) |
| `go.sum` mutation during re-verification | Operational | Low | Low–Med | Use `GOFLAGS=-mod=readonly`; `git checkout -- go.sum`; cache pre-warmed | Mitigated (documented) |
| External tooling prerequisites for reproduction | Integration | Low | Low | Dev guide documents `mc`/`boto3`/`./minio` prerequisites + versions | Mitigated |
| KMS prerequisite for Req 1 reproduction | Integration | Low | Low | Report documents exact `MINIO_KMS_*` env vars | Mitigated |

---

## 7. Visual Project Status

**Project hours — completed vs. remaining** (Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`):

```mermaid
%%{init: {"theme":"base","themeVariables":{"pie1":"#5B39F3","pie2":"#FFFFFF","pieStrokeColor":"#B23AF2","pieOuterStrokeColor":"#B23AF2","pieStrokeWidth":"2px","pieOuterStrokeWidth":"2px","pieSectionTextColor":"#111111","pieTitleTextColor":"#5B39F3","pieLegendTextColor":"#111111"}}}%%
pie showData title Project Hours Breakdown (Total 50h)
    "Completed Work" : 44
    "Remaining Work" : 6
```

**Remaining hours by priority** (High `#5B39F3` · Medium `#B23AF2` · Low `#A8FDD9`):

```mermaid
%%{init: {"theme":"base","themeVariables":{"pie1":"#5B39F3","pie2":"#B23AF2","pie3":"#A8FDD9","pieStrokeColor":"#FFFFFF","pieOuterStrokeColor":"#B23AF2","pieStrokeWidth":"1px","pieSectionTextColor":"#111111","pieTitleTextColor":"#5B39F3","pieLegendTextColor":"#111111"}}}%%
pie showData title Remaining 6h by Priority
    "High — Review & Acceptance" : 3
    "Medium — Re-verification" : 2
    "Low — Housekeeping" : 1
```

**Remaining hours by category** (mirrors Section 2.2; sums to 6h):

| Category | Hours | Bar |
|----------|-------|-----|
| Review & Acceptance | 3 | ███████████████ |
| Reproduction Re-verification | 2 | ██████████ |
| Repository Housekeeping | 1 | █████ |
| **Total** | **6** | — |

> Integrity: "Remaining Work" (6) equals Section 1.2 Remaining Hours and the Section 2.2 Hours total. "Completed Work" (44) equals Section 1.2 Completed Hours.

---

## 8. Summary & Recommendations

**Achievements.** The project is **88.0% complete** (44 of 50 hours). The single mandated deliverable — a 784-line, code-as-truth MinIO runtime-security report — is finished and independently validated. All five questions are answered with the required structure (verbatim question, reproduction, captured evidence, code-truth root cause with `file:line` anchors, and rationale). The investigation went beyond restating the plan: it **corrected four AAP assumptions** against the actual source (most notably that Object-Lock denial is HTTP **400**, not 403), demonstrating genuine code-as-truth rigor. The MinIO source tree is byte-for-byte unchanged.

**Remaining gaps.** The outstanding **6 hours** are entirely path-to-production and require a human: a 3-hour SME review/acceptance, a 2-hour independent reproduction re-verification, and 1 hour of cosmetic housekeeping (removing gitignored debugging binaries). There are no functional gaps, no failing tests, and no compilation issues.

**Critical path to production.** SME review & sign-off (3h) → optional fresh-environment re-verification (2h) → merge. Housekeeping (1h) can proceed in parallel.

**Success metrics.**

| Metric | Target | Actual | Status |
|--------|--------|--------|--------|
| Questions answered with full evidence | 5 | 5 | ✅ |
| Source files modified | 0 | 0 | ✅ |
| `file:line` citations verified | 100% | 6/6 spot-check accurate; GATE 5 100% | ✅ |
| Validation gates passed | 5/5 | 5/5 | ✅ |
| `git status` clean | Yes | Yes (empty) | ✅ |

**Production readiness assessment.** The deliverable is **production-ready pending human acceptance**. Confidence is **High**: the content is complete, the evidence was reproduced twice (authoring + autonomous validation), citations are accurate, and the read-only constraint is provably honored. Recommended action: conduct the SME review, perform the optional re-verification, complete housekeeping, and merge.

---

## 9. Development Guide

This guide builds, runs, and reproduces the evidence behind the report. Commands were tested in the validation environment; running `go` with `-mod=readonly` provably keeps the source tree pristine.

### 9.1 System Prerequisites

- **OS:** Linux x86_64 (Ubuntu 25.10 used).
- **Go:** **1.23.12** at `/usr/local/go` (satisfies the `go 1.23` directive in `go.mod`).
- **`mc` client:** `RELEASE.2025-08-13T08-35-41Z` at `/usr/local/bin/mc`.
- **Python + boto3:** for issuing raw S3 requests (unencrypted `PutObject`, Object-Lock retention).
- **git + git-lfs**, ~2 CPU cores, and a warm Go module cache (the build runs fully offline).
- **Repository at HEAD** `c07e5b49d477b0774f23db3b290745aef8c01bd2`.

### 9.2 Environment Setup

```bash
# Toolchain on PATH and pristine-tree safety flags (CRITICAL — keeps go.sum unchanged)
export PATH=/usr/local/go/bin:$PATH
export GOFLAGS=-mod=readonly
export GOTOOLCHAIN=local

go version                 # expect: go version go1.23.12 linux/amd64
git rev-parse HEAD         # expect: c07e5b49d... (or ee9cf0825 on the deliverable branch)
git status --porcelain     # expect: empty (clean tree)
```

### 9.3 Build

```bash
make build                 # CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o ./minio
./minio --version          # confirm runtime go1.23.12 and matching commit-id
```
> `make build` also runs the `build-debugging` target, producing `hash-set`, `healing-bin`, `s3-check-md5`, `xl-meta`. These are **gitignored** and safe to delete afterward.

### 9.4 Reproduce the Evidence

**Req 1 — Encryption precedence (runtime trace):**
```bash
export MINIO_KMS_SECRET_KEY="minio-test-key:$(head -c 32 /dev/urandom | base64)"
export MINIO_KMS_AUTO_ENCRYPTION=on
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
./minio server /tmp/mdata/d{1...4} --address 127.0.0.1:9100 &
mc alias set local http://127.0.0.1:9100 minioadmin minioadmin
mc mb local/databucket
# create a broad s3:* policy + user, attach it, then:
mc admin trace -v --call s3 local      # watch while boto3 PutObject runs WITHOUT ServerSideEncryption
# Expect: 200 OK + server-injected x-amz-server-side-encryption: aws:kms;
#         Authorization SignedHeaders does NOT include the SSE header.
```

**Req 2 — Object-Lock DELETE (runtime log):**
```bash
mc mb --with-lock local/lockbucket
# boto3 put_object(..., ObjectLockMode="COMPLIANCE", ObjectLockRetainUntilDate=<now+1d>)
# boto3 delete_object(...)  -> HTTP 400 InvalidRequest "Object is WORM protected and cannot be overwritten"
mc rm local/lockbucket/worm.txt           # multi-delete -> HTTP 200 batch with per-object <Error>
```

**Req 3 — Bitrot detection & heal (runtime):**
```bash
# write a ~5 MiB object; locate its part.1 shard under /tmp/mdata/d1/<bucket>/<obj>/<uuid>/part.1
dd if=/dev/zero of="<d1>/.../part.1" bs=1 seek=1000000 count=131072 conv=notrunc   # corrupt 128 KiB
mc cp local/<bucket>/<obj> /tmp/out.bin                  # GET returns byte-identical data
mc admin heal -r --scan deep --json local/<bucket>       # shard restored byte-for-byte
# Note: a SUCCESSFUL reconstruction+heal is quiet by default; proof is behavioral (correct bytes + heal result).
```

**Req 4 — STS session-policy intersection (test output):**
```bash
# Recreate the ephemeral /tmp Go module from the report's §4 steps using
# github.com/minio/minio-go/v7/pkg/credentials (cr.STSAssumeRole with an inline Policy
# allowing only s3:GetObject + s3:ListBucket). Then:
#   PARENT PutObject SUCCEEDS (s3:* is real) ; STS List/Get SUCCEED ; STS PutObject DENIED 403.
# Expect: --- PASS: TestInlineSTSSessionPolicyIntersection
```

**Req 5 — Privilege-escalation prevention (test output):**
```bash
go test -tags kqueue,dev -v -run TestIAMInternalIDPServerSuite ./cmd
# Expect: --- PASS: TestIAMInternalIDPServerSuite (~14s); 4 configs PASS + 4 etcd SKIP; 0 failures.
```

### 9.5 Verification

```bash
git status --porcelain     # MUST be empty (source tree pristine)
git diff c07e5b49d HEAD --name-status   # MUST show only: A blitzy/documentation/minio_c07e5b49d477.md
```

### 9.6 Troubleshooting

- **`ok ... [no tests to run]`** when using `-run TestSTS` or `-run TestUserPolicyEscalationBug`: these are `*TestSuiteIAM` **methods**, not top-level tests. Use the suite runner `-run TestIAMInternalIDPServerSuite` (CODE-WINS #4).
- **`go.sum` shows as modified:** you ran `go` with `-mod=mod`. Re-export `GOFLAGS=-mod=readonly` and revert with `git checkout -- go.sum`.
- **Req 1 object stored unencrypted:** ensure `MINIO_KMS_AUTO_ENCRYPTION=on` and `MINIO_KMS_SECRET_KEY` are exported **before** starting the server.
- **Port 9100 in use:** choose another `--address 127.0.0.1:<port>`.
- **Leftover debugging binaries:** `rm -f hash-set healing-bin s3-check-md5 xl-meta` (gitignored; safe).

---

## 10. Appendices

### Appendix A — Command Reference

| Purpose | Command |
|---------|---------|
| Build server | `make build` |
| Run server (4-drive erasure) | `./minio server /tmp/mdata/d{1...4} --address 127.0.0.1:9100` |
| Req 5 test (escalation) | `go test -tags kqueue,dev -v -run TestIAMInternalIDPServerSuite ./cmd` |
| Compile test package only | `go test -tags kqueue,dev -c ./cmd` |
| Live API trace | `mc admin trace -v --call s3 local` |
| Scanner/heal trace | `mc admin trace --call storage,scanner -v local` |
| Deep heal | `mc admin heal -r --scan deep --json local/<bucket>` |
| Integrity check | `git status --porcelain` · `git diff c07e5b49d HEAD --name-status` |

### Appendix B — Port Reference

| Port | Service | Notes |
|------|---------|-------|
| 9100 | MinIO S3 API (reproduction) | Used in report reproductions (`--address 127.0.0.1:9100`) |
| 9000 | MinIO S3 API (default) | Default if `--address` omitted |
| 9201–9203 | Repro servers (validation) | Used transiently during validation; closed afterward |

### Appendix C — Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (sole new file) |
| `cmd/object-handlers.go` | Req 1 PutObject SSE-apply; Req 2 single-delete call site |
| `internal/bucket/encryption/bucket-sse-config.go` | Req 1 SSE header injection |
| `internal/crypto/auto-encryption.go` | Req 1 auto-encryption toggle |
| `cmd/api-errors.go` | Req 2 `ErrObjectLocked` (HTTP 400) |
| `cmd/bucket-object-lock.go` | Req 2 retention/legal-hold enforcement |
| `cmd/bitrot.go`, `cmd/erasure-object.go`, `cmd/erasure-healing.go` | Req 3 bitrot verify + heal |
| `cmd/iam.go` | Req 4 STS intersection; Req 5 `CreateUser`/`PolicyDBSet` |
| `cmd/sts-handlers.go` | Req 4 STS flows + `maxSTSSessionPolicySize` |
| `cmd/iam-store.go` | Req 5 root cause (`AddUser` ignores `PolicyName`) |
| `cmd/admin-handlers-users.go` / `_test.go` | Req 5 add-user handler + escalation test |

### Appendix D — Technology Versions

| Component | Version |
|-----------|---------|
| Go toolchain | go1.23.12 (linux/amd64) |
| MinIO module | `github.com/minio/minio` @ HEAD `c07e5b49d` |
| `mc` client | `RELEASE.2025-08-13T08-35-41Z` |
| Deliverable branch head | `ee9cf0825` |
| Build tags | `kqueue` (build), `kqueue,dev` (tests) |
| git-lfs | 3.7.1 |

### Appendix E — Environment Variable Reference

| Variable | Purpose |
|----------|---------|
| `MINIO_KMS_AUTO_ENCRYPTION=on` | Forces non-SSE-C requests to SSE-S3/KMS (Req 1) |
| `MINIO_KMS_SECRET_KEY` | Local KMS key (`name:base64key`) for SSE (Req 1) |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | Server root credentials |
| `MINIO_CI_CD=1` | CI mode for non-interactive startup |
| `GOFLAGS=-mod=readonly` | **Keeps `go.sum` pristine** during go commands |
| `GOTOOLCHAIN=local` | Pins toolchain to local Go 1.23.12 |
| `PATH=/usr/local/go/bin:$PATH` | Puts Go 1.23.12 on PATH |

### Appendix F — Developer Tools Guide

| Tool | Use in this project |
|------|---------------------|
| `mc admin trace -v` | Capture full request/response for Req 1–2 (server-side via `httpTracerMiddleware`, `cmd/http-tracer.go:L69`) |
| `mc admin trace --call scanner` / `mc admin scanner trace` | Observe healing/scanner activity for Req 3 |
| `mc admin heal --scan deep` | Trigger and confirm bitrot repair (Req 3) |
| `boto3` | Issue raw S3 requests that deliberately omit SSE and set Object-Lock retention (Req 1–2) |
| `go test -tags kqueue,dev` | Produce Req 4–5 test evidence |

### Appendix G — Glossary

| Term | Definition |
|------|------------|
| **SSE-S3 / SSE-KMS** | Server-side encryption at rest with a server-managed (`AES256`) or KMS-managed (`aws:kms`) key |
| **Auto-encryption** | MinIO setting that turns any non-SSE-C request into an encrypted-at-rest object |
| **WORM / Object Lock** | Write-Once-Read-Many immutability; COMPLIANCE mode cannot be bypassed, GOVERNANCE mode only with `s3:BypassGovernanceRetention` |
| **Bitrot** | Silent on-disk data corruption; MinIO detects it via HighwayHash shard checksums |
| **Erasure coding** | Data split into data+parity shards (here 2+2) enabling reconstruction despite a corrupt/missing shard |
| **STS / AssumeRole** | Security Token Service issuing temporary credentials |
| **Session policy** | Inline policy attached to temporary credentials; effective permissions = **intersection** of parent and session policy |
| **`consoleAdmin`** | Built-in MinIO admin policy; the privilege a basic user must not be able to self-attach |
| **Code-as-truth** | Every claim anchored to a verified source `file:line` and confirmed by running the system |

---

*Project Guide complete. Source tree unchanged at HEAD `c07e5b49d`; the sole deliverable is `blitzy/documentation/minio_c07e5b49d477.md`. Completion: 88.0% (44 of 50 hours); remaining 6 hours are human path-to-production activities.*