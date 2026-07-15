# Blitzy Project Guide — MinIO Runtime Security Investigation

> Branch `minio_c07e5b49d477` · HEAD `c07e5b49d` · Task type: **Documentation / Runtime Investigation (read-only source tree)**
> Brand legend — <span style="color:#5B39F3">**Completed / AI Work = Dark Blue `#5B39F3`**</span> · **Remaining / Not Completed = White `#FFFFFF`** · Headings/Accents = Violet-Black `#B23AF2` · Highlight = Mint `#A8FDD9`

---

## 1. Executive Summary

### 1.1 Project Overview

This project is a read-only, runtime-grounded investigation of five MinIO security and data-integrity behaviors — policy-vs-encryption precedence, object-lock delete enforcement, bit-rot detection/heal-on-read, STS session-policy scoping, and IAM privilege-escalation prevention. The MinIO source tree (`github.com/minio/minio` @ HEAD `c07e5b49d`) is treated strictly as read-only evidence. The sole persistent deliverable is one Markdown report that answers each question with complete, unedited runtime output next to every claim and anchors each behavior to a specific function and `file:line`. Target consumers are MinIO operators and security engineers who need proof-backed answers rather than code-reading inferences.

### 1.2 Completion Status

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieSectionTextColor':'#B23AF2','pieTitleTextSize':'17px','pieLegendTextSize':'14px'}}}%%
pie showData title Completion Status — 86.2% Complete
    "Completed Work (AI) — 56h" : 56
    "Remaining Work — 9h" : 9
```

<sub>Slice colors — Completed = Dark Blue `#5B39F3` · Remaining = White `#FFFFFF` (outlined in `#B23AF2` for contrast). Center/label metric: **86.2% Complete**.</sub>

| Metric | Hours | Notes |
|--------|-------|-------|
| **Total Hours** | **65** | AAP-scoped investigation + path-to-production (PA1) |
| **Completed Hours (AI + Manual)** | **56** | AI (autonomous) = 56 · Manual = 0 |
| **Remaining Hours** | **9** | Human review / acceptance / triage / sign-off |
| **Percent Complete** | **86.2%** | 56 ÷ 65 × 100 = 86.15% → **86.2%** |

### 1.3 Key Accomplishments

- ✅ All **five** investigation requirements (R1–R5) answered with runtime-captured evidence — no answer written from code-reading alone.
- ✅ Sole deliverable created and committed: `blitzy/documentation/minio_c07e5b49d477.md` (3,867 lines).
- ✅ **Read-only MainRule provably honored** — `git diff c07e5b49d..HEAD` = exactly one added file; **0** source files changed.
- ✅ MinIO built canonically from HEAD and driven through real S3/STS/admin entry points (SigV4 via `boto3`/`mc`) — no mocks, hooks, or synthetic bypasses.
- ✅ **76/76** source-anchor citations verified against HEAD (2 off-by-one imprecisions corrected).
- ✅ **42** distinct runtime conditions exercised (39 PASS, 2 honest PARTIAL/[INFERRED], 1 disclosed weakness); **5/5** requirement reproductions with zero divergence.
- ✅ Bonus security posture documented: **CVE-2025-62506** session-policy bypass confirmed present at HEAD, plus permissive CORS, heal-API `%#v` type disclosure, 426 mode disclosure, STS caching-header gap.
- ✅ All ephemeral fixtures (data dirs, scripts, aliases, keys) removed; working tree clean.

### 1.4 Critical Unresolved Issues

There are **no unresolved defects in the deliverable**. The items below are open **actions on the underlying MinIO build** that the investigation surfaced; per the AAP, remediation is out of scope (observe-and-document only), so these require human routing.

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| CVE-2025-62506 session-policy bypass confirmed present at HEAD | High — a scoped STS/service-account identity can restore parent-level permissions on this build | MinIO Security / Platform owner | Within 1 business day of review (triage/routing only; ~1.5h) |
| Report + citations not yet human-accepted | Medium — findings should not be relied upon for action until SME-reviewed | Reviewing SME | With peer review (~4h) |
| Hardening observations (permissive CORS, heal-API `%#v` disclosure, 426 mode disclosure, STS caching headers) | Low–Medium — information disclosure / defensive-hardening gaps | Platform / Config owner | With triage (bundled in ~1.5h) |

### 1.5 Access Issues

No access issues identified. The investigation ran fully within the provided environment: the repository was accessible at HEAD `c07e5b49d`, the Go toolchain built the server, and the `mc`/`boto3` clients exercised the local server over loopback. No third-party API access, external credentials, or elevated repository permissions were required. Root credentials and the KMS key were operator-supplied ephemeral fixtures (redacted in the report) and are not needed to read the deliverable.

| System / Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-------------------|----------------|-------------------|-------------------|-------|
| MinIO source repo @ HEAD `c07e5b49d` | Read | None | ✅ Resolved | — |
| Local MinIO server (loopback :9000/:9100) | Runtime | None | ✅ Resolved | — |
| Built-in KMS (`MINIO_KMS_SECRET_KEY`) | Runtime fixture | Operator-supplied ephemeral key; reviewer supplies own for reproduction | ✅ Resolved (documented) | Reviewer |

### 1.6 Recommended Next Steps

1. **[High]** SME peer review of the report — verify accuracy, completeness, verbatim-question coverage, and all 76 `file:line` citations against HEAD `c07e5b49d`.
2. **[High]** Triage & route **CVE-2025-62506** (and the CORS / heal-`%#v` / 426 hardening observations) to the MinIO security owner for a remediation/upgrade decision.
3. **[Medium]** Independently reproduce at least one requirement (e.g., R4 STS intersection or R3 heal-on-read) by rebuilding from HEAD and re-running the documented commands.
4. **[Medium]** Obtain stakeholder sign-off and merge the report to mainline documentation.

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

Every completed component traces to a specific AAP requirement or its enabling foundation. All items are classified **Completed** (evidence: report sections, `file:line` citations, validator reproductions, commits, cleanup proof).

| Component | Hours | Description |
|-----------|-------|-------------|
| Foundation: canonical build + dual servers + tooling | 5 | Go 1.23.12 build of MinIO from HEAD; `mc` + `boto3` clients; built-in KMS; single-drive (:9000) and 4-drive erasure (:9100) servers; audit-webhook sink; `/minio/health/ready` gates. |
| R1 — Policy evaluation vs. SSE precedence | 6 | Auto-encryption `AES256` proof (SSE header injected server-side, absent from `SignedHeaders`); IAM `Deny`-if-no-SSE alternate; KMS-mode variants 1D–1H; bucket-enc config-size limits. `cmd/object-handlers.go:1893-1896`. |
| R2 — Object-lock delete log entries | 6 | Legal hold / compliance / governance deletes → `400` WORM; bypass matrix (`403` without grant, `204` with grant); delete-marker; duplicate-header order dependence; audit JSON. `cmd/bucket-object-lock.go:84`. |
| R3 — Bit-rot detection on GET | 6 | 4-drive erasure set; on-disk `part.1` shard corruption; page-cache drop; GET → `200` byte-correct heal-on-read over 2 stable cycles; single-drive `503` variant. `cmd/storage-errors.go:104`, `cmd/bitrot-streaming.go:184`. |
| R4 — STS session-policy enforcement | 6 | `AssumeRole` with narrow inline policy → GET `200` / PUT `403` (parent ∩ session); 2,048-byte cap enforced; **CVE-2025-62506** disclosure. `cmd/sts-handlers.go:256/:89`, `cmd/iam.go:2242`. |
| R5 — Privilege-escalation prevention | 5 | Basic user self-attach of `consoleAdmin` → `403` on both handlers; 20-attempt adversarial matrix all denied; mapping unchanged (28 constant snapshots); root cause identified. `cmd/auth-handler.go` / `cmd/admin-handler-utils.go:37`. |
| Report authoring & consolidation | 10 | 3,867-line Markdown report; per-requirement Direct Answer / Reproduction / Observed Output / Responsible Code / Rationale; 472 code fences; complete unedited output discipline; verbatim question preservation. |
| Web-search research | 2 | `mc admin trace`/`logs`/audit formats; HighwayHash bit-rot & heal-on-read semantics; STS intersection + 2,048-byte cap; advisory context (AAP §0.2.2). |
| Source-anchor citation verification | 3 | 76 `file:line` anchors verified against HEAD; 2 off-by-one imprecisions corrected (R3 `mrf.go:276`, R5 `admin-handler-utils.go:59`). |
| QA remediation iterations | 6 | ≥36 QA findings resolved across 7 commits (11 → 3 → 2 → CVE → 20 → 2), each addressing evidence-quality, labeling, and citation accuracy. |
| Cleanup + read-only proof | 1 | Removed data dirs, scripts, aliases, keys; `git diff --name-status` = single added file; no listeners on 9000/9001/9100/9101. |
| **Total Completed** | **56** | **Matches Completed Hours in Section 1.2** |

### 2.2 Remaining Work Detail

All remaining work is **path-to-production for a documentation/security-investigation deliverable** — i.e., human review and acceptance. There is no outstanding AAP implementation work; the deliverable is complete, accurate, and committed.

| Category | Hours | Priority |
|----------|-------|----------|
| SME peer review — verify report accuracy, completeness, verbatim-question coverage & all 76 `file:line` citations vs HEAD | 4.0 | High |
| Security-finding triage & routing — file/route CVE-2025-62506 + CORS / heal-`%#v` / 426 hardening observations to owners (**routing only; remediation out of AAP scope**) | 1.5 | High |
| Independent spot-check reproduction — rebuild MinIO from HEAD and re-run ≥1 requirement in reviewer environment | 2.5 | Medium |
| Final stakeholder sign-off & merge of report to mainline documentation | 1.0 | Medium |
| **Total Remaining** | **9.0** | **Matches Remaining Hours in Section 1.2 and Section 7 pie** |

> **Integrity:** Section 2.1 (56) + Section 2.2 (9) = **65** Total Project Hours (Section 1.2). ✅

---

## 3. Test Results

For this read-only documentation task, no source code was modified, so the pre-existing MinIO unit suite is unaffected and **no new unit tests were introduced**. The task-relevant "tests" are the **autonomous runtime reproductions and source-anchor verifications** captured in Blitzy's validation logs. All entries below originate exclusively from those logs.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|-----------|-------|
| Runtime Behavioral Conditions (R1–R5) | Canonical S3/STS/admin via `boto3` (SigV4) + `mc` | 42 | 39 | 0 | 100% | 2 non-PASS are honest documented **PARTIAL/[INFERRED]** results (R1 bucket-policy-vs-IAM nuance; R3 `mc admin logs` line); 1 is a deliberately **disclosed weakness** (CVE-2025-62506). None are failures. |
| Source-Anchor Citation Verification | Exact-line extraction vs HEAD `c07e5b49d` | 76 | 76 | 0 | 100% | 2 off-by-one imprecisions found and corrected before final. |
| Requirement-Level Reproductions | Canonical S3/STS/admin end-to-end | 5 | 5 | 0 | 100% | R1–R5 each reproduced with **zero divergence** from documented claims. |
| Canonical Build Verification | `go build` (Go 1.23.12) | 1 | 1 | 0 | 100% | Exit 0, ~12s, byte-identical 156,743,610-byte binary; self-reports `DEVELOPMENT.GOGET`. |
| Runtime Health Gate | `curl /minio/health/{live,ready}` | 2 | 2 | 0 | 100% | Both `200`; server READY ~1s after start. |
| **Totals** | — | **126** | **123** | **0** | **100%** | 3 non-PASS rows are intentional honest results (2 PARTIAL/[INFERRED] + 1 disclosed CVE), **not** failures. |

**Evidence density in the report:** 71 `AccessDenied`, 22 `AES256`, 14 `errFileCorrupt`, 9 `HighwayHash` occurrences; SHA-256 `ef3488fc…340185f8` byte-match confirmed for the R3 healed object.

> **Integrity Rule 3:** every test above is drawn from Blitzy's autonomous validation logs for this project; none are hypothetical.

---

## 4. Runtime Validation & UI Verification

**Runtime health** — the MinIO server built from HEAD and ran through canonical entry points:

- ✅ **Operational** — Canonical build succeeds (exit 0, ~12s), binary byte-identical across rebuilds.
- ✅ **Operational** — Single-drive server (:9000) and 4-drive erasure server (:9100) both start; `/minio/health/live` and `/minio/health/ready` return `200`.
- ✅ **Operational** — Built-in KMS enabled via `MINIO_KMS_SECRET_KEY` (R1); absent KMS fails **closed** (`501 NotImplemented`), confirming safe default.

**API integration outcomes (per requirement):**

- ✅ **Operational** — **R1**: unencrypted PUT by broad-write user → `200` + server-injected `X-Amz-Server-Side-Encryption: AES256`; alternate IAM `Deny` path → `403`/`200`.
- ✅ **Operational** — **R2**: locked-object deletes → `400 InvalidRequest` (WORM); governance bypass `403` without grant / `204` with grant; trace + audit JSON captured.
- ✅ **Operational** — **R3**: shard corruption → GET `200` with byte-correct heal-on-read over 2 stable cycles; single-drive → `503 SlowDownRead` (unrecoverable, as expected).
- ⚠ **Partial** — **R4**: STS intersection proven (GET `200` / PUT `403`) and 2,048-byte cap enforced; **however** CVE-2025-62506 lets an own-account service-account path restore parent scope — a genuine **observed weakness** on this build (disclosed, not remediated).
- ✅ **Operational** — **R5**: self-promotion attempts → `403 AccessDenied` on both handlers; policy mapping unchanged after all 20 adversarial attempts.

**UI verification:** Not applicable — this is a headless server/API investigation with no frontend deliverable. The MinIO Console UI was not part of the AAP scope; all behaviors were verified through the S3/STS/admin APIs and `mc` CLI, with server trace, audit-log JSON, and stderr as observation channels.

---

## 5. Compliance & Quality Review

Cross-mapping of AAP deliverables and binding rules to Blitzy quality/compliance benchmarks. Fixes applied during autonomous validation are noted.

| Benchmark / AAP Rule | Requirement | Status | Progress | Evidence / Fixes Applied |
|----------------------|-------------|--------|----------|--------------------------|
| MainRule — read-only source tree | Add only the report; modify no source | ✅ Pass | 100% | `git diff c07e5b49d..HEAD` = 1 added file; 0 source files changed |
| MainRule — deliverable location & name | `blitzy/documentation/minio_c07e5b49d477.md` | ✅ Pass | 100% | File present, committed, 3,867 lines |
| Rule 1 — run-first, canonical entry point | Build & observe; no mocks/bypasses | ✅ Pass | 100% | Canonical build cmd recorded; SigV4 via `boto3`/`mc`; server banners captured |
| Rule 2 — exhaustive conditions + unedited output | Primary + secondary/error/edge variants, complete output | ✅ Pass | 100% | 42-row coverage matrix; complete command output next to each claim (472 fences) |
| Rule 3 — observed-output discipline & labeling | Show output; label inferred | ✅ Pass | 100% | Labels: 162 "Observed", 26 "Source-grounded", 4 "[INFERRED]" (2 in R3, clearly flagged) |
| Rule 4 — complete, precise, grounded | `file:line` + function names; lead with direct answer | ✅ Pass | 100% | 76 verified citations; named functions; Direct-Answer-first structure |
| Stability requirement | Repeat timing/state-dependent obs ≥2 runs | ✅ Pass | 100% | R3 heal-on-read over 2 independent cycles; R4/R5 re-verified |
| Fidelity constraint | Report negatives honestly | ✅ Pass | 100% | R1 bucket-policy-vs-IAM nuance & R3 `mc admin logs` reported as honest PARTIAL/[INFERRED] |
| Cleanup guarantee | Remove all fixtures | ✅ Pass | 100% | Cleanup section; no listeners on 9000/9001/9100/9101; working tree clean |
| Citation accuracy (QA) | Anchors match HEAD | ✅ Pass | 100% | **Fixed** 2 off-by-one: R3 `mrf.go:276`, R5 `admin-handler-utils.go:59` |
| Security-finding disclosure (QA) | Surface, don't remediate | ✅ Pass | 100% | **Added** CVE-2025-62506 disclosure with root cause; remediation correctly withheld per scope |

**Autonomous fixes applied during validation:** ≥36 QA findings resolved across 7 commits — genuine-evidence rerun of R1–R5 (11 findings), documentation-accuracy corrections (3), two rounds of off-by-one citation fixes, CVE-2025-62506 disclosure, and a final 20-finding QA pass.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| T1 — Citation drift if tree advances past HEAD `c07e5b49d` | Technical | Low | Medium | Report states HEAD explicitly; 76/76 anchors verified | Mitigated |
| T2 — Reproduction tied to specific toolchain (Go 1.23.12 / `mc` 2025-08-13 / `boto3` 1.43.47) | Technical | Low | Low | Exact versions documented in Environment section | Mitigated |
| T3 — Two R3 statements are `[INFERRED]`, not directly observed | Technical | Low | Low | Clearly labeled and placed next to observed evidence | Accepted |
| S1 — **CVE-2025-62506** session-policy bypass confirmed present at HEAD | Security | High | High | Disclosed with root cause (`cmd/iam.go:2320/:2381`, `policy.go:188`); route to security / upgrade | Open (routing in remaining work) |
| S2 — Permissive CORS (origin reflection) | Security | Medium | Medium | Disclosed; recommend config review | Open (observation) |
| S3 — Heal API discloses Go types + heap pointers (`%#v`) | Security | Low | Low | Disclosed | Open (observation) |
| S4 — 426 mode disclosure on unknown admin routes; STS success caching-header gap | Security | Low | Low | Disclosed | Open (observation) |
| O1 — Ephemeral harness removed; reviewer must rebuild fixtures | Operational | Low | Medium | Complete reproduction commands inline in report | Mitigated |
| O2 — Point-in-time snapshot; no automated regression guards findings | Operational | Low | Low | HEAD-pinned and dated | Accepted |
| O3 — Secrets redacted (root pw, KMS key); reviewer supplies own | Operational | Low | Low | Documented; any valid values work | Mitigated |
| I1 — R1 needs KMS key; absent → `501` fail-closed | Integration | Low | Low | Variant 1F documented; set `MINIO_KMS_SECRET_KEY` | Mitigated |
| I2 — R3 heal-on-read needs ≥4-drive erasure set; single-drive → `503` | Integration | Low | Low | Documented; run 4-drive server for R3 | Mitigated |
| I3 — `boto3` `PutObjectRetention/LegalHold` → `400 MissingContentMD5` | Integration | Low-Med | Medium | Documented (R2-MIN-01); use `mc` to set locks | Mitigated |
| I4 — Two servers on distinct ports required to cover all R | Integration | Low | Low | Documented dual-server invocation | Mitigated |

**Overall posture:** Risks to the **deliverable** are Low and well-mitigated. The single High item (S1, CVE-2025-62506) is a disclosed observation about the underlying MinIO build — not a defect in the deliverable — and its remediation is explicitly out of AAP scope; only triage/routing is captured as remaining work.

---

## 7. Visual Project Status

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieSectionTextColor':'#B23AF2','pieTitleTextSize':'16px'}}}%%
pie showData title Project Hours Breakdown (Total 65h)
    "Completed Work" : 56
    "Remaining Work" : 9
```

<sub>Colors — Completed Work = Dark Blue `#5B39F3` · Remaining Work = White `#FFFFFF`. **Integrity:** "Remaining Work" = 9 matches Section 1.2 Remaining Hours and the Section 2.2 total.</sub>

**Remaining hours by category (Section 2.2):**

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'primaryColor':'#5B39F3','primaryTextColor':'#B23AF2','lineColor':'#B23AF2'}}}%%
graph LR
    A["SME peer review — 4.0h [High]"]
    B["Independent reproduction — 2.5h [Medium]"]
    C["Security triage & routing — 1.5h [High]"]
    D["Sign-off & merge — 1.0h [Medium]"]
```

<sub>Sum of remaining categories = 4.0 + 2.5 + 1.5 + 1.0 = **9.0h** (matches Section 1.2 and the pie above).</sub>

**Priority distribution of remaining work:** High = 5.5h (SME review 4.0 + security triage 1.5) · Medium = 3.5h (reproduction 2.5 + sign-off 1.0) · Low = 0h.

---

## 8. Summary & Recommendations

**Achievements.** The investigation is functionally complete: all five questions (R1–R5) are answered with runtime-captured, `file:line`-anchored evidence in a single 3,867-line report, delivered under a strict read-only constraint that is provably honored (zero source files changed). The work was driven through canonical S3/STS/admin entry points, verified across 42 runtime conditions and 76 source anchors, and re-run for stability. It additionally discloses a real, confirmed weakness (CVE-2025-62506) and four hardening observations without overstepping the observe-and-document mandate.

**Completion.** The project is **86.2% complete** (56 of 65 hours). The remaining **9 hours** is entirely human path-to-production for a documentation/security deliverable — it cannot be self-certified by an autonomous agent.

**Remaining gaps & critical path to production.**
1. SME peer review of the report and its 76 citations (**High**, 4h).
2. Security triage/routing of CVE-2025-62506 and the hardening observations (**High**, 1.5h) — the highest-urgency action given the confirmed bypass on this build.
3. Independent spot-check reproduction (**Medium**, 2.5h).
4. Stakeholder sign-off & merge (**Medium**, 1h).

**Success metrics.** 5/5 requirements answered · 76/76 citations verified · 42 conditions exercised (39 PASS, 2 honest PARTIAL/[INFERRED], 1 disclosed weakness) · 0 source files modified · reproducible build · clean teardown.

**Production readiness assessment.** The deliverable itself is **production-ready** — complete, accurate, runtime-grounded, and committed. The gating consideration is not deliverable quality but **decisions the findings demand**: the confirmed CVE-2025-62506 on this build should be routed for a remediation/upgrade decision promptly. Recommendation: **accept the report after SME review and immediately triage the disclosed CVE.**

---

## 9. Development Guide

Rebuild MinIO and reproduce every behavior in the report. All commands are copy-pasteable and were verified in the build environment. The MinIO source tree is read-only — nothing below modifies it.

### 9.1 System Prerequisites

- Linux x86_64 (validated on Ubuntu-family container).
- **Go 1.23.12** (matches `go.mod` `go 1.23` and CI `1.23.x`). Verify: `go version` → `go version go1.23.12 linux/amd64`.
- **`mc`** client `RELEASE.2025-08-13T08-35-41Z` (its own runtime is `go1.24.6`; independent of the server).
- **Python 3.13** with **`boto3` 1.43.47** / `botocore` 1.43.47 (SigV4 S3/STS/admin client).
- `curl` for health probes; ~1 GB free disk for the Go build cache and erasure data dirs.

### 9.2 Environment Setup

```bash
# 1) Work from the repository root at HEAD c07e5b49d (read-only source tree).
cd /path/to/minio            # repo root

# 2) Install the mc client (client-only; not committed to the repo).
#    Place it outside the repo, e.g. /tmp/bin/mc, and ensure it is executable.
/tmp/bin/mc --version        # expect RELEASE.2025-08-13T08-35-41Z

# 3) Install boto3 into a venv (preferred) or with --break-system-packages.
python3 -m venv /tmp/venv && . /tmp/venv/bin/activate
pip install boto3==1.43.47
python3 -c "import boto3; print(boto3.__version__)"   # 1.43.47

# 4) Fixture secrets (ephemeral; any >=8-char values / valid 32-byte base64 KMS key).
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD='<choose-a-strong-password>'
export MINIO_KMS_SECRET_KEY='my-minio-key:<valid-32-byte-base64-key>'   # enables SSE-S3/auto-encryption (R1)
```

### 9.3 Build (canonical, verified)

```bash
CGO_ENABLED=0 GOFLAGS=-mod=mod go build -tags kqueue -o /tmp/minio-bin .
# Verified: exit 0, ~12s, binary 156,743,610 bytes (byte-identical across rebuilds).
/tmp/minio-bin --version
# minio-bin version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
# Runtime: go1.23.12 linux/amd64
```

> A plain `go build` (no ldflags) self-reports `DEVELOPMENT.GOGET`; the canonical `make build` stamps a release version. This does **not** affect any of the five behaviors (all are request-path behaviors).

### 9.4 Application Startup

```bash
# Single-drive server — used for R1, R2, R4, R5 (KMS enabled for R1):
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD="$MINIO_ROOT_PASSWORD" \
MINIO_KMS_SECRET_KEY="$MINIO_KMS_SECRET_KEY" \
/tmp/minio-bin server /tmp/miniodata --address :9000 --console-address :9001 &

# Four-drive erasure server — required for R3 heal-on-read (parity reconstruction):
/tmp/minio-bin server /tmp/minio4/d1 /tmp/minio4/d2 /tmp/minio4/d3 /tmp/minio4/d4 \
  --address :9100 --console-address :9101 &
```

### 9.5 Verification Steps

```bash
# Health gate (expect 200):
curl -s -o /dev/null -w "ready=%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
curl -s -o /dev/null -w "live=%{http_code}\n"  http://127.0.0.1:9000/minio/health/live
# Verified: both return 200; server becomes READY within ~1s of start.

# Register an mc alias and inspect cluster state:
/tmp/bin/mc alias set inv9000 http://127.0.0.1:9000 minioadmin "$MINIO_ROOT_PASSWORD"
/tmp/bin/mc admin info inv9000
```

### 9.6 Example Usage (reproduction entry points)

```bash
# Capture the server trace while triggering a behavior (R1/R2/R5):
/tmp/bin/mc admin trace -v inv9000        # full per-request dump (use -a for internal storage/heal calls, R3)

# Drive S3/STS/admin actions via boto3 (SigV4) — see the report's per-requirement
# "Reproduction" blocks for the exact scripts: PUT without SSE (R1), locked-object
# DELETE (R2), corrupt-shard GET (R3), AssumeRole + scoped GET/PUT (R4),
# self-attach consoleAdmin (R5). Each block prints the exact command + full output.
```

### 9.7 Troubleshooting

- **`501 NotImplemented` on SSE requests (R1):** `MINIO_KMS_SECRET_KEY` is unset — MinIO fails closed. Export a valid key and restart.
- **`503 SlowDownRead` on the R3 GET:** you are on a single drive (no parity). Use the 4-drive erasure server for heal-on-read.
- **`400 MissingContentMD5` from `boto3` `PutObjectRetention`/`PutObjectLegalHold` (R2):** a client-side quirk — set locks with the canonical `mc` client instead.
- **`mc admin service restart` hangs:** it needs a TTY in a non-interactive shell — stop and relaunch the server with the identical invocation instead.
- **`FATAL Failed to connect to KMS: illegal base64 data`:** the `MINIO_KMS_SECRET_KEY` value is malformed — supply a valid 32-byte base64 key.

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---------|---------|
| Verify Go | `go version` |
| Verify `mc` | `/tmp/bin/mc --version` |
| Canonical build | `CGO_ENABLED=0 GOFLAGS=-mod=mod go build -tags kqueue -o /tmp/minio-bin .` |
| Verify binary | `/tmp/minio-bin --version` |
| Start single-drive server | `/tmp/minio-bin server /tmp/miniodata --address :9000 --console-address :9001` |
| Start 4-drive erasure server | `/tmp/minio-bin server /tmp/minio4/d{1,2,3,4} --address :9100 --console-address :9101` |
| Health probe | `curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:9000/minio/health/ready` |
| Set `mc` alias | `/tmp/bin/mc alias set inv9000 http://127.0.0.1:9000 <user> <pass>` |
| Capture trace | `/tmp/bin/mc admin trace -v inv9000` (`-a` for internal calls) |
| Server error logs | `/tmp/bin/mc admin logs inv9000` |
| Confirm read-only | `git diff c07e5b49d..HEAD --name-status` |

### B. Port Reference

| Port | Role |
|------|------|
| 9000 | Single-drive server S3 API (R1, R2, R4, R5) |
| 9001 | Single-drive server Console |
| 9100 | 4-drive erasure server S3 API (R3 heal-on-read) |
| 9101 | 4-drive erasure server Console |

### C. Key File Locations

| Path | Description |
|------|-------------|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The sole deliverable** — the investigation report (3,867 lines) |
| `cmd/object-handlers.go:1893-1896` | R1 — bucket default-encryption applied on the write path |
| `cmd/bucket-object-lock.go:84` | R2 — `enforceRetentionBypassForDelete` |
| `cmd/storage-errors.go:104` · `cmd/bitrot-streaming.go:184` | R3 — `errFileCorrupt` sentinel + streaming HighwayHash mismatch |
| `cmd/sts-handlers.go:256/:89` · `cmd/iam.go:2242` | R4 — `AssumeRole`, 2,048-byte cap, `IsAllowedSTS` |
| `cmd/auth-handler.go` · `cmd/admin-handler-utils.go:37` | R5 — admin auth gate + `validateAdminReq` |
| `/tmp/minio-bin` · `/tmp/bin/mc` | Built server binary + `mc` client (outside repo; not committed) |

### D. Technology Versions

| Component | Version |
|-----------|---------|
| Go toolchain | 1.23.12 (`linux/amd64`) |
| MinIO server | HEAD `c07e5b49d` (self-reports `DEVELOPMENT.GOGET`, `go1.23.12`) |
| `mc` client | `RELEASE.2025-08-13T08-35-41Z` (runtime `go1.24.6`) |
| `boto3` / `botocore` | 1.43.47 / 1.43.47 |
| Python | 3.13.7 |
| `github.com/minio/pkg/v3/policy` | as pinned in `go.mod` (admin/S3 action constants) |

### E. Environment Variable Reference

| Variable | Purpose |
|----------|---------|
| `MINIO_ROOT_USER` | Root access key (provisioning only) |
| `MINIO_ROOT_PASSWORD` | Root secret key (provisioning only) |
| `MINIO_KMS_SECRET_KEY` | Built-in KMS master key `name:base64key` — enables SSE-S3/auto-encryption (R1) |
| `MINIO_KMS_AUTO_ENCRYPTION` | Toggles global auto-encryption (R1 variant 1E) |
| `CGO_ENABLED=0` | Static, pure-Go build |
| `GOFLAGS=-mod=mod` | Module resolution mode used by the canonical build |

### F. Developer Tools Guide

- **`mc admin trace [-v|-a]`** — streams server-side API operations; `-v` gives the full per-request dump; `-a` includes internal storage/heal calls (used for R3). Compact line format: `<time> [<STATUS>] s3.<Func> <path> <ip> <dur> ↑<rx> ↓<tx>`.
- **`mc admin logs`** — surfaces server error logs to the console.
- **Audit-log JSON** (webhook/Kafka target) — carries `api.name`, `statusCode`, `requestID`, and principal; used to capture R2/R5 signals.
- **`boto3` (SigV4)** — programmatic S3/STS/admin calls: PUT/GET/DELETE, `AssumeRole`, and signed admin requests; the report's reproduction scripts use it directly.

### G. Glossary

| Term | Meaning |
|------|---------|
| SSE-S3 / SSE-KMS | Server-side encryption with MinIO-managed / KMS-managed keys |
| Auto-encryption | Server-side injection of encryption when a bucket default rule (or global toggle) is set |
| WORM | Write-Once-Read-Many — object-lock retention/legal-hold enforcement |
| Governance vs Compliance | Governance retention is bypassable with `BypassGovernanceRetentionAction`; Compliance cannot be bypassed by anyone |
| Bit-rot | Silent on-disk data corruption; MinIO detects it via per-shard HighwayHash on read |
| Heal-on-read | Reconstruction of a corrupt shard from parity during a GET (needs a multi-drive erasure set) |
| STS `AssumeRole` | Issues temporary credentials whose effective permission = parent policy ∩ inline session policy |
| Deny-by-default | Policy evaluation denies unless an action is explicitly allowed — the basis for R4/R5 `AccessDenied` |
| `errFileCorrupt` | Sentinel `StorageErr("file is corrupted")` surfaced on HighwayHash mismatch (`cmd/storage-errors.go:104`) |

---

<sub>All hour figures reconcile across Sections 1.2, 2.1, 2.2, and 7: Completed = 56h · Remaining = 9h · Total = 65h · **86.2% complete** (56 ÷ 65). All Section 3 tests originate from Blitzy's autonomous validation logs. Completed = Dark Blue `#5B39F3` · Remaining = White `#FFFFFF`.</sub>