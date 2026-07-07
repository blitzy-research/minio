# Blitzy Project Guide — MinIO Security Behavior Investigation (Runtime-Evidence Q&A)

> **Document type:** Investigative Question-and-Answer deliverable (read-only)
> **Repository:** `github.com/minio/minio` @ base commit `c07e5b49d477`
> **Source branch:** `minio_c07e5b49d477`
> **Deliverable:** `blitzy/documentation/minio_c07e5b49d477.md` (2,038 lines — the sole persisted artifact)
> **Brand legend:** 🟪 Completed / AI Work = Dark Blue `#5B39F3` · ⬜ Remaining / Not Completed = White `#FFFFFF` · Accents = Violet-Black `#B23AF2` · Highlight = Mint `#A8FDD9`

---

## 1. Executive Summary

### 1.1 Project Overview

This project is a **read-only, run-first security investigation** of the MinIO object-storage server. The objective was to determine and *demonstrate with captured runtime evidence* how MinIO behaves across five discrete security scenarios — bucket-encryption precedence, object-lock (WORM) deletion, erasure-coded bit-rot detection, STS session-policy enforcement, and IAM privilege-escalation prevention — and to root-cause each behavior at a specific `file:line`. The intended audience is security engineers and platform reviewers who need authoritative, reproducible answers rather than code-reading conjecture. The technical scope spans MinIO's encryption/IAM, object-lock, erasure/bit-rot, and STS subsystems. The **only** durable output is one Markdown document; no MinIO source, test, configuration, or build file was modified.

### 1.2 Completion Status

The project is **90.0% complete** on an AAP-scoped basis. All autonomous investigation work (build, run in both deployment modes, reproduce every named sub-mode, capture verbatim evidence, verify citations, leave a clean tree) is delivered and validated. The remaining 10% is the human review/acceptance gate — the natural path-to-production for an authoritative security document (no deployment, CI, or integration applies to a Q&A artifact).

```mermaid
%%{init: {'theme':'base','themeVariables':{'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieSectionTextColor':'#B23AF2','pieTitleTextSize':'16px'}}}%%
pie showData title Completion Status — 90.0% Complete (63 of 70 hrs)
    "Completed Work (AI)" : 63
    "Remaining Work" : 7
```

| Metric | Value |
|---|---|
| **Total Hours** | **70** |
| **Completed Hours (AI + Manual)** | **63** (63 AI + 0 Manual) |
| **Remaining Hours** | **7** |
| **Percent Complete** | **90.0%** |

### 1.3 Key Accomplishments

- ✅ MinIO server **built from source** with the canonical `Makefile:179` recipe; version banner `DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12)` reproduced and explained.
- ✅ **Q1** — both encryption-precedence mechanisms reproduced: bucket default/auto-encryption (transparent SSE-KMS on a 200 PUT) and an encryption-mandating IAM deny (`403 AccessDenied`), each captured with `mc admin trace`.
- ✅ **Q2** — all object-lock modes reproduced: Governance, Compliance, Legal Hold, and the transitional unversioned delete-marker case, with server logs and the client WORM error.
- ✅ **Q3** — on a 4-drive erasure (EC:2) backend, a shard was physically corrupted; recoverable corruption **healed on read**, unrecoverable corruption surfaced as `SlowDownRead`/HTTP 503; two-run payload stability confirmed.
- ✅ **Q4** — STS session-policy **intersection** proven (in-session ALLOWED, out-of-session DENIED); additionally discovered/reproduced **CVE-2025-62506** (session-policy bypass via self-created service accounts).
- ✅ **Q5** — basic user **cannot** self-attach `consoleAdmin` via the admin API (`403`, pre-mutation abort) nor via direct `.minio.sys` writes (`AllAccessDisabled`); root cause `validateAdminReq`.
- ✅ **111 `file:line` citations** verified against source (0 missing); 164 balanced verbatim evidence blocks; **3 AAP corrections** empirically confirmed.
- ✅ Full codebase compiles clean (`go build ./...` exit 0); `go.mod`/`go.sum` unchanged; working tree clean with exactly one added file.

### 1.4 Critical Unresolved Issues

There are **no blocking defects**. The deliverable passed all five validation gates. The single item warranting explicit human attention is informational (already fixed upstream), tracked below.

| Issue | Impact | Owner | ETA |
|---|---|---|---|
| CVE-2025-62506 documented as present at investigated commit `c07e5b49d477` | Informational — confirms the *investigated* build is vulnerable; already fixed upstream in `RELEASE.2025-10-15T17-29-55Z`. Requires human triage that production is not on a vulnerable build. | Security reviewer | 1h |
| Deliverable not yet human-reviewed | Authoritative security claims should receive SME sign-off before acceptance | Security SME | 3h |

### 1.5 Access Issues

**No access issues identified.** The investigation ran entirely on a locally built server with default credentials (`minioadmin:minioadmin`) on loopback ports; the Go toolchain, module cache, and `mc` client were all available. No external repository permissions, service credentials, or third-party API access were required or blocked.

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|---|---|---|---|---|
| — | — | No access issues identified | N/A | — |

### 1.6 Recommended Next Steps

1. **[High]** Perform SME technical review of all five answers (Q1–Q5) against the cited source and captured evidence (~3h).
2. **[Medium]** Independently spot-check reproduce the strongest claims — rebuild at `c07e5b49d477` and rerun Q3 (erasure corrupt+heal) and Q4 (STS intersection) (~2h).
3. **[Medium]** Triage CVE-2025-62506 — confirm no production build predates the upstream fix and route to security tracking (~1h).
4. **[Low]** Editorial review, stakeholder sign-off, and merge the single new file (~1h).

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

Every component traces to an AAP requirement (foundation, Q1–Q5, or cross-cutting evidence rules). Total = **63 hours**.

| Component | Hours | Description |
|---|---|---|
| Environment & canonical build | 6 | Built `minio` + `mc` from source; verified `Makefile:179` recipe and `gen-ldflags` version derivation; stood up 4 server configurations (single-node; KMS+auto-encrypt; KMS-only; 4-drive erasure). |
| Q1 — Encryption vs. broad write permission | 7 | Reproduced Path A (auto-encryption, 200 + SSE-KMS injected) and Path B (IAM deny, `403 AccessDenied` + encrypted 200); captured `mc admin trace`; confirmed AAP correction #1 (IAM ≠ bucket policy). |
| Q2 — Object-lock (WORM) delete | 7 | Reproduced Governance, Compliance, Legal Hold, and transitional delete-marker; captured server logs + client WORM error; root-caused `enforceRetentionBypassForDelete`. |
| Q3 — Bit-rot detection & heal | 9 | Stood up 4-drive erasure (EC:2); corrupted a shard with `dd`; captured recoverable heal-on-read + deep-scan heal and unrecoverable `SlowDownRead`/503; two-run stability. |
| Q4 — STS session policy (+ CVE) | 10 | Built a `minio-go`/`madmin-go` harness; proved intersection (in/out-of-session); discovered and reproduced CVE-2025-62506 across multiple runs with full root cause. |
| Q5 — Privilege escalation | 6 | Exercised admin-API attack path (`403`, pre-mutation abort) and direct-storage path (`AllAccessDisabled`); root-caused `validateAdminReq` + reserved `.minio.sys`. |
| Document authoring & evidence integration | 9 | Authored the 2,038-line deliverable: per-question setup/commands, 164 verbatim evidence blocks, Coverage Pass table, Notes. |
| Citation verification | 3 | Grep-verified 111 unique `file:line` citations against source (0 missing files). |
| QA review iteration & fresh re-capture | 4 | Addressed QA review (commit `fc04ae4c4`): full producing-commands + re-captured runtime evidence; Q4 stdout-fidelity fix. |
| Cleanup, git hygiene, repo-integrity | 2 | Removed all ephemeral artifacts under `/tmp/blitzy/...`; verified clean tree, unchanged manifests, git-ignored binary. |
| **Total** | **63** | **Sum of Completed Hours (matches Section 1.2)** |

### 2.2 Remaining Work Detail

Each category is a path-to-production (human review/acceptance) activity. Total = **7 hours**.

| Category | Hours | Priority |
|---|---|---|
| SME technical review of all five answers for correctness | 3 | High |
| Independent spot-check reproduction (rebuild + rerun Q3/Q4) | 2 | Medium |
| Security-finding triage (CVE-2025-62506 disclosure/tracking) | 1 | Medium |
| Editorial review + stakeholder sign-off + merge | 1 | Low |
| **Total** | **7** | **Sum of Remaining Hours (matches Section 1.2 and Section 7)** |

### 2.3 Hours Reconciliation

- Completed (Section 2.1) = **63h**
- Remaining (Section 2.2) = **7h**
- Total = 63 + 7 = **70h** (matches Section 1.2)
- Completion = 63 / 70 = **90.0%** (matches Sections 1.2, 7, 8)

---

## 3. Test Results

> **Integrity note.** This is a read-only investigation, so "tests" are **Blitzy's autonomous runtime reproductions and verification checks** captured in the validation logs — not a unit-test suite added to the repo. Traditional code-coverage does not apply; the meaningful coverage metric is **AAP sub-mode coverage = 100%** (every named mode reproduced). All items below originate from Blitzy's autonomous validation logs for this project.

| Test Category | Framework / Method | Total | Passed | Failed | Coverage % | Notes |
|---|---|---|---|---|---|---|
| Q1 — Encryption-precedence reproduction | `mc admin trace` + `minio-go/v7` | 2 | 2 | 0 | N/A | Path A auto-encrypt (200, SSE-KMS injected); Path B IAM deny (403) + encrypted (200) |
| Q2 — Object-lock WORM delete reproduction | `mc retention`/`legalhold` + versioned delete | 4 | 4 | 0 | N/A | Governance, Compliance, Legal Hold, transitional delete-marker |
| Q3 — Bit-rot detection/heal reproduction | 4-drive erasure EC:2 + `dd` + `mc cp` | 2 | 2 | 0 | N/A | Recoverable heal-on-read; Unrecoverable `SlowDownRead`/503; 2-run payload stability |
| Q4 — STS session-policy reproduction | `minio-go/v7` v7.0.80 + `madmin-go/v3` v3.0.77 harness | 6 | 6 | 0 | N/A | In-session ALLOWED ×2; out-of-session DENIED ×2; CVE-2025-62506 bypass; scope-bounding control |
| Q5 — Privilege-escalation reproduction | `madmin-go` admin API + raw SigV4 | 2 | 2 | 0 | N/A | admin-API path (403, pre-mutation abort); direct-storage path (`AllAccessDisabled`) |
| Citation-accuracy verification | `grep` vs source @ `c07e5b49d477` | 111 | 111 | 0 | 100% | 0 missing files; unique `(file,line)` pairs |
| Full-codebase compilation | `CGO_ENABLED=0 go build -tags kqueue ./...` | 1 | 1 | 0 | N/A | Exit 0; `go.mod`/`go.sum` unchanged |
| **Aggregate** | — | **128** | **128** | **0** | — | 100% pass rate |

---

## 4. Runtime Validation & UI Verification

Runtime health across the exercised subsystems (all captured live during the investigation):

- ✅ **Operational** — Canonical `minio` server builds and runs (banner `DEVELOPMENT.2024-11-25T17-10-22Z`, go1.23.12).
- ✅ **Operational** — Single-node deployment (`ErasureSDSetupType`) for Q1/Q2/Q4/Q5.
- ✅ **Operational** — 4-drive erasure deployment (`EC:2`) for Q3.
- ✅ **Operational** — `mc admin trace` stream (`httpTracerMiddleware` → `globalTrace.Publish`) — primary evidence source.
- ✅ **Operational** — S3 data path: `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, versioned/unversioned delete.
- ✅ **Operational** — SSE-KMS + `MINIO_KMS_AUTO_ENCRYPTION` auto-encryption.
- ✅ **Operational** — Object-lock retention (Governance/Compliance/Legal Hold) enforcement.
- ✅ **Operational** — Erasure heal-on-read and deep-scan heal (`heal.Object`).
- ✅ **Operational** — STS `AssumeRole` with inline session policy; intersection enforcement.
- ✅ **Operational** — Admin API authorization (`validateAdminReq`) for policy/user mutations.
- ➖ **N/A** — **UI verification not applicable.** This is a documentation-only task with no UI in scope; the MinIO Console web UI was not part of the investigation and no Figma/design system was provided.

---

## 5. Compliance & Quality Review

Cross-mapping of AAP deliverables and governing rules ("SWE-AtlasQnA-Repo") to quality benchmarks:

| Benchmark / Rule | Status | Progress | Evidence |
|---|---|---|---|
| Deliverable location & naming (`blitzy/documentation/minio_c07e5b49d477.md`) | ✅ Pass | 100% | File present, 2,038 lines |
| Read-only mandate (zero source changes) | ✅ Pass | 100% | `git diff` = exactly 1 added file; `go.mod`/`go.sum` unchanged |
| Run-first methodology (evidence before prose) | ✅ Pass | 100% | 164 verbatim evidence blocks captured live |
| Canonical configuration (build/invocation recorded) | ✅ Pass | 100% | §1 records `Makefile:179` recipe + banner + `--version` |
| Every-condition coverage (all named sub-modes) | ✅ Pass | 100% | Coverage Pass table; grep-confirmed sub-modes |
| Verbatim evidence next to each claim | ✅ Pass | 100% | Fenced blocks labelled with producing command |
| `file:line` grounding for every claim | ✅ Pass | 100% | 111 citations verified; 0 missing files |
| Inferred / non-canonical labeling | ✅ Pass | 100% | Notes section labels inferred + redacted items |
| Secret sanitization | ✅ Pass | 100% | KMS key/STS creds/JWT signatures redacted; creds expired/ephemeral |
| Cleanup + clean git status | ✅ Pass | 100% | Ephemeral artifacts under `/tmp/blitzy`; tree clean |

**Fixes applied during autonomous validation:** three AAP corrections empirically confirmed (Q1 IAM-vs-bucket policy at `cmd/iam.go:2482`; Q3 client-facing `SlowDownRead` vs internal `errFileCorrupt`; WORM message at `cmd/api-errors.go:1061`); a §1 version-derivation causal correction (commit `67982a189`); a Q4 stdout-fidelity fix (commit `d34d11520`); and full documentation of CVE-2025-62506 (commit `d2c4dbfcf`).

**Outstanding compliance items:** none. All rules satisfied pending human sign-off.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|---|---|---|---|---|---|
| CVE-2025-62506 (session-policy bypass, CWE-863, CVSS 8.1) present at investigated commit | Security | Medium | High | Already fixed upstream in `RELEASE.2025-10-15T17-29-55Z`; human to confirm no production build predates the fix and route to security tracking | Documented / needs human triage |
| Residual secret material in captured evidence (KMS key, STS creds, JWT) | Security | Low | Low | Secrets redacted; credentials expired/ephemeral/local-only (127.0.0.1 servers no longer exist) | Mitigated |
| Behavioral claims across 5 complex subsystems could vary by environment | Technical | Low | Low | Two-run stability confirmed; all commands copy-pasteable for re-verification | Mitigated |
| Documented behavior pinned to commit `c07e5b49d477` (~9 months old) may differ in newer releases | Technical | Low | Medium | Doc explicitly scopes to this commit and records exact banner | Accepted (in scope) |
| `file:line` citations could drift if doc reused against a different checkout | Technical | Low | Low | 111 citations grep-verified at this commit; anchors commit-pinned | Mitigated |
| Independent reproduction requires toolchain + KMS/erasure setup effort | Operational | Low | Medium | §9 Development Guide provides exact, tested build/run commands | Mitigated |
| Repository integration (merge/build/CI conflict) | Integration | Low | Low | Zero source changes; single additive file in new directory; tree clean | Mitigated |

**Overall risk posture: LOW.** No blocking risks; the highest-severity item (CVE-2025-62506) is informational and already fixed upstream.

---

## 7. Visual Project Status

**Project Hours Breakdown** (Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`):

```mermaid
%%{init: {'theme':'base','themeVariables':{'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieSectionTextColor':'#B23AF2','pieTitleTextSize':'16px'}}}%%
pie showData title Project Hours Breakdown — 90.0% Complete
    "Completed Work" : 63
    "Remaining Work" : 7
```

**Remaining Hours by Category** (from Section 2.2):

```mermaid
%%{init: {'theme':'base','themeVariables':{'xyChart':{'backgroundColor':'#FFFFFF','plotColorPalette':'#5B39F3'},'xyChart.titleColor':'#B23AF2'}}}%%
xychart-beta
    title "Remaining Hours by Category (7h total)"
    x-axis ["SME Review", "Spot-check Repro", "CVE Triage", "Editorial+Merge"]
    y-axis "Hours" 0 --> 4
    bar [3, 2, 1, 1]
```

> **Integrity check:** Pie "Remaining Work" = 7 = Section 1.2 Remaining Hours = Section 2.2 Hours sum ✓. Pie "Completed Work" = 63 = Section 1.2 Completed Hours = Section 2.1 sum ✓.

---

## 8. Summary & Recommendations

**Achievements.** The project delivered an authoritative, **90.0%-complete** (63 of 70 hrs) security investigation of MinIO at commit `c07e5b49d477`. All five questions were answered with **captured, verbatim runtime evidence** and root-caused at specific `file:line` anchors. Every named sub-mode was reproduced — encryption auto-vs-deny, WORM Governance/Compliance/Legal-Hold/transitional, bit-rot recoverable-vs-unrecoverable, STS in-vs-out-of-session, and privilege-escalation admin-API-vs-direct-storage. The work went beyond the AAP by discovering and documenting **CVE-2025-62506** and by correcting three AAP inaccuracies with empirical evidence.

**Remaining gaps (7h).** All outstanding work is the human review/acceptance gate: SME technical review (3h), independent spot-check reproduction (2h), CVE-2025-62506 triage (1h), and editorial sign-off + merge (1h). No engineering rework is required — the deliverable has zero known defects, the codebase compiles clean, and the repository tree is clean with exactly one added file.

**Critical path to production.** For this documentation deliverable, "production" = acceptance and merge of the authoritative answer document. The critical path is: SME review → spot-check reproduction → CVE triage → sign-off/merge.

**Success metrics.** 5/5 questions reproduced; 128/128 verification checks passed; 111/111 citations accurate; 0 source files modified; 0 blocking risks.

**Production-readiness assessment.** The deliverable is **ready for human review**. It is authentic (run-first, evidence-backed), accurate (citations verified), complete (full coverage pass), and safe to merge (single additive file, clean tree).

| Metric | Value |
|---|---|
| AAP-scoped completion | 90.0% |
| Questions answered with evidence | 5 / 5 |
| Verification checks passed | 128 / 128 |
| Citations verified | 111 / 111 |
| Source files modified | 0 |
| Blocking risks | 0 |

---

## 9. Development Guide

This guide reproduces the environment used for the investigation. **All commands were tested** in the validation environment (Ubuntu, go1.23.12).

### 9.1 System Prerequisites

- **OS:** Linux (x86-64). Investigation ran on Ubuntu.
- **Go toolchain:** 1.23.x (1.23.12 used) — matches `go.mod`'s `go 1.23`.
- **git:** 2.x (2.51.0 used).
- **Disk:** ~2 GB free (source + module cache + ~117 MB binary + backend data dirs).
- **Tools:** `mc` (MinIO Client), and the `minio-go/v7` + `madmin-go/v3` SDKs (resolved from the module cache for scripted harnesses).

### 9.2 Environment Setup

```bash
# Toolchain environment
export GOROOT=/usr/local/go
export GOPATH=/root/go
export PATH=/usr/local/go/bin:/root/go/bin:$PATH

# Verify prerequisites
go version          # expect: go version go1.23.12 linux/amd64
git --version       # expect: git version 2.x

# IMPORTANT: to reproduce the DOCUMENTED banner (DEVELOPMENT.2024-11-25T17-10-22Z),
# build at the BASE commit (the version is derived from the HEAD commit time):
git checkout c07e5b49d477      # or: git checkout minio_c07e5b49d477
```

### 9.3 Dependency Installation

```bash
# From the repository root — download server module dependencies
go mod download

# Build the mc client (used to drive/observe scenarios)
GOBIN=/tmp/blitzy/tools/bin go install github.com/minio/mc@latest
/tmp/blitzy/tools/bin/mc --version
```

### 9.4 Build the MinIO Server (canonical)

```bash
# Canonical recipe (equivalent to Makefile:179 'build:' target)
export GOROOT=/usr/local/go GOPATH=/root/go PATH=/usr/local/go/bin:/root/go/bin:$PATH
LDFLAGS="$(go run buildscripts/gen-ldflags.go)"
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$LDFLAGS" -o /tmp/blitzy/minio-bin/minio .

# Verify (expected at base commit c07e5b49d477):
/tmp/blitzy/minio-bin/minio --version
# minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
# Runtime: go1.23.12 linux/amd64
```

### 9.5 Run the Server

**Single-node (Q1, Q2, Q4, Q5):**

```bash
/tmp/blitzy/minio-bin/minio server /tmp/blitzy/data \
  --address :9000 --console-address :9001
# Defaults: credentials minioadmin:minioadmin; S3 API :9000; console :9001
```

**4-drive erasure `EC:2` (Q3):**

```bash
/tmp/blitzy/minio-bin/minio server \
  /tmp/blitzy/ec/d1 /tmp/blitzy/ec/d2 /tmp/blitzy/ec/d3 /tmp/blitzy/ec/d4 \
  --address :9000 --console-address :9001
```

**KMS + auto-encryption variant (Q1 Path A):**

```bash
export MINIO_KMS_SECRET_KEY="my-minio-key:<base64-32-bytes>"
export MINIO_KMS_AUTO_ENCRYPTION=on
/tmp/blitzy/minio-bin/minio server /tmp/blitzy/data --address :9010 --console-address :9011
```

### 9.6 Verification Steps

```bash
# Configure an mc alias (GLOBAL FLAGS PRECEDE THE SUBCOMMAND)
mc --config-dir /tmp/blitzy/mc-config alias set local http://127.0.0.1:9000 minioadmin minioadmin

# Confirm the server responds and subscribe to the trace stream
mc --config-dir /tmp/blitzy/mc-config admin info local
mc --config-dir /tmp/blitzy/mc-config admin trace -v local     # primary evidence source
```

### 9.7 Example Usage (per-question triggers)

```bash
# Q2 — create a WORM bucket, set retention, attempt a delete
mc --config-dir /tmp/blitzy/mc-config mb --with-lock local/lockbucket
mc --config-dir /tmp/blitzy/mc-config cp ./file.txt local/lockbucket/file.txt
mc --config-dir /tmp/blitzy/mc-config retention set --default GOVERNANCE 30d local/lockbucket
mc --config-dir /tmp/blitzy/mc-config rm --version-id <VID> local/lockbucket/file.txt   # blocked (WORM)

# Q3 — corrupt a shard on disk, then GET (heal-on-read)
dd if=/dev/zero of=/tmp/blitzy/ec/d2/<bucket>/<obj>/<uuid>/part.1 bs=1024 count=64 conv=notrunc
mc --config-dir /tmp/blitzy/mc-config cp local/<bucket>/<obj> /tmp/out.bin    # returns correct bytes
```

### 9.8 Troubleshooting

- **`mc` returns spurious `AccessDenied`:** global flags (e.g. `--config-dir`) **must precede** the subcommand; placing them after the positionals silently stores empty credentials (anonymous requests).
- **Version banner differs from the documented one:** the version is derived from the HEAD commit time (`gen-ldflags.go`), so build at base commit `c07e5b49d477` to reproduce `DEVELOPMENT.2024-11-25T17-10-22Z`.
- **Port already in use:** change `--address`/`--console-address` (the investigation used `:9010`, `:9020`, `:9030`, `:9100` for isolation).
- **Offline SDK-harness build:** `GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build .`
- **Bit-rot not triggered:** ensure a **multi-drive erasure** deployment (single-drive/FS backends do not exercise the bit-rot read path).
- **Leave the repo clean:** run servers/data/harnesses under `/tmp/blitzy/...`; the in-tree `./minio` binary is git-ignored (`.gitignore:4`).

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---|---|
| Build minio (canonical) | `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$LDFLAGS" -o <out>/minio .` |
| Generate LDFLAGS | `go run buildscripts/gen-ldflags.go` |
| Run single-node | `minio server <dir> --address :9000 --console-address :9001` |
| Run 4-drive erasure | `minio server <d1> <d2> <d3> <d4> --address :9000 --console-address :9001` |
| Set mc alias | `mc --config-dir <cfg> alias set <name> <url> <ak> <sk>` |
| Trace stream | `mc --config-dir <cfg> admin trace -v <alias>` |
| WORM bucket | `mc --config-dir <cfg> mb --with-lock <alias>/<bucket>` |
| Set retention | `mc --config-dir <cfg> retention set --default GOVERNANCE 30d <alias>/<bucket>` |
| Legal hold | `mc --config-dir <cfg> legalhold set <alias>/<bucket>/<obj>` |

### B. Port Reference

| Port | Role | Notes |
|---|---|---|
| 9000 | S3 API (default) | Single-node / erasure |
| 9001 | Console (default) | Web UI (not exercised) |
| 9010 / 9011 | S3 / Console | KMS + auto-encryption variant (Q1 Path A) |
| 9020 | S3 API | 4-drive erasure EC:2 (Q3) |
| 9030 | S3 API | KMS without auto-encryption (Q1 Path B) |
| 9100 | S3 API | CVE-2025-62506 reproduction (Q4) |

### C. Key File Locations

| Path | Role |
|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | The deliverable (2,038 lines) |
| `cmd/object-handlers.go` | PutObject/DeleteObject handlers (Q1/Q2) |
| `cmd/encryption-v1.go` | `EncryptRequest` (Q1) |
| `internal/crypto/auto-encryption.go` | `MINIO_KMS_AUTO_ENCRYPTION` toggle (Q1) |
| `cmd/bucket-object-lock.go` | WORM retention enforcement (Q2) |
| `cmd/api-errors.go` | `ErrObjectLocked` / `ErrSlowDownRead` (Q2/Q3) |
| `cmd/bitrot.go`, `cmd/xl-storage.go`, `cmd/erasure-healing.go` | Bit-rot detection & heal (Q3) |
| `cmd/sts-handlers.go`, `cmd/iam.go` | STS session policy + intersection (Q4) |
| `cmd/admin-handlers-users.go`, `cmd/admin-handler-utils.go` | Admin authorization; CVE root cause (Q4/Q5) |
| `cmd/object-api-utils.go` | Reserved `.minio.sys` bucket (Q5) |

### D. Technology Versions

| Component | Version |
|---|---|
| Go toolchain | 1.23.12 |
| MinIO server (built) | `DEVELOPMENT.2024-11-25T17-10-22Z` (commit `c07e5b49d477`) |
| `mc` client | built from `github.com/minio/mc` |
| `minio-go/v7` | v7.0.77 / v7.0.80 (harnesses) |
| `madmin-go/v3` | v3.0.77 |
| `pkg/v3` (policy evaluator) | v3.0.22 |
| git | 2.51.0 |

### E. Environment Variable Reference

| Variable | Purpose |
|---|---|
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | Server root credentials (default `minioadmin`/`minioadmin`) |
| `MINIO_KMS_SECRET_KEY` | KMS master key for SSE-KMS (Q1) |
| `MINIO_KMS_AUTO_ENCRYPTION` | `on` to force auto-encryption of unencrypted uploads (Q1 Path A) |
| `GOROOT` / `GOPATH` / `PATH` | Go toolchain paths for building |
| `CGO_ENABLED=0` | Static build per canonical recipe |
| `GOFLAGS=-mod=mod` / `GOPROXY=off` / `GOSUMDB=off` | Offline SDK-harness builds |

### F. Developer Tools Guide

- **Primary evidence tool:** `mc admin trace -v` — subscribes to the server HTTP trace stream (`httpTracerMiddleware` → `globalTrace.Publish`); used to capture request/response sequences and internal calls.
- **SDK harnesses:** `minio-go/v7` (S3/STS operations) and `madmin-go/v3` (admin operations, service-account creation, trace subscription) drive scripted, reproducible observations.
- **On-disk inspection:** standard Unix tools (`dd`, `md5sum`, `ls`) used to corrupt/verify erasure shards for Q3.
- **Chrome DevTools MCP:** **not applicable** — no web UI was in scope for this investigation.

### G. Glossary

| Term | Definition |
|---|---|
| WORM | Write-Once-Read-Many; object-lock retention preventing overwrite/delete |
| SSE-KMS | Server-Side Encryption using a Key Management Service key |
| Auto-encryption | Server transparently encrypts uploads lacking SSE headers |
| Bit-rot | Silent on-disk data corruption; detected via per-shard HighwayHash checksums |
| Erasure coding (EC:N) | Data split into data+parity shards; `EC:2` = 2 parity shards tolerated |
| Heal-on-read | Automatic shard reconstruction from parity triggered by a read |
| STS | Security Token Service; issues temporary credentials via `AssumeRole` |
| Session policy | Inline policy on temporary creds; effective perms = intersection with parent |
| IAM | Identity and Access Management (users, policies, mappings) |
| `consoleAdmin` | Built-in administrative policy |
| `.minio.sys` | Reserved internal bucket storing IAM state; inaccessible to basic S3 users |
| `validateAdminReq` | Guard authorizing admin actions (Q5 root cause) |
| CVE-2025-62506 | Session-policy bypass via self-created service accounts (fixed upstream) |
