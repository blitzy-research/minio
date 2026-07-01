# Blitzy Project Guide — MinIO Security & Storage-Integrity Investigation

> **Deliverable:** `blitzy/documentation/minio_c07e5b49d477.md` (843 lines)
> **Repository:** `github.com/minio/minio` @ commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`
> **Branch:** `blitzy-8b0296c6-3a4b-4353-af22-48d1c64047e2` · **HEAD:** `68dab37f5`
> **Task type:** Read-only, runtime-grounded investigation → single Markdown answer document

---

## 1. Executive Summary

### 1.1 Project Overview

This project is a **read-only, runtime-grounded** security and storage-integrity investigation of the MinIO object storage server. The objective was to author a single, evidence-backed answer document that resolves five distinct questions — SSE-versus-authorization precedence on `PutObject`, WORM/object-lock enforcement on `DELETE`, bitrot detection and self-heal on `GET`, STS session-policy enforcement, and IAM privilege-escalation prevention. The target audience is MinIO operators and security reviewers who require *proof* (not prose) of these behaviors. Business impact: authoritative, reproducible security documentation grounded directly in the shipped codebase. The technical scope spans encryption/KMS, object lock, erasure coding, STS, and admin-API authorization — all **observed**, none modified.

### 1.2 Completion Status

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#111111','pieLegendTextColor':'#111111'}}}%%
pie showData title Completion — 87.8% Complete
    "Completed Work (AI)" : 43
    "Remaining Work" : 6
```

| Metric | Hours |
|--------|-------|
| **Total Hours** | **49** |
| Completed Hours — AI (autonomous) | 43 |
| Completed Hours — Manual | 0 |
| **Completed Hours (AI + Manual)** | **43** |
| **Remaining Hours** | **6** |
| **Percent Complete** | **87.8%**  (43 ÷ 49) |

> Completion is computed with the AAP-scoped, hours-based methodology: `Completed ÷ (Completed + Remaining) = 43 ÷ 49 = 87.8%`. All AAP deliverables are complete; the remaining 6 hours are **path-to-production human activities only** (review, reproduction, merge).

### 1.3 Key Accomplishments

- ✅ **Single deliverable authored and committed** — `blitzy/documentation/minio_c07e5b49d477.md`, 843 lines, answering all five questions with verbatim evidence.
- ✅ **MinIO built and run** at the pinned commit — server binary (`DEVELOPMENT.GOGET`, `go1.23.2`) plus both single-node filesystem and erasure 4-drive backends, each returning HTTP 200 health.
- ✅ **Q1 proven** — unencrypted `PutObject` by a broad-`Put` user is transparently auto-encrypted; authorization runs before encryption; object is encrypted at rest (on-disk cleartext grep + `mc stat`).
- ✅ **Q2 proven** — WORM `DELETE` rejected with `InvalidRequest` / HTTP 400 (single) and per-object error inside HTTP 200 `<DeleteResult>` (batch).
- ✅ **Q3 proven** — corrupted shard detected, object reconstructed from parity on `GET`, and repaired via deep heal `[Yellow → Green]` with sha256-verified shard restore.
- ✅ **Q4 proven** — `TestIAMInternalIDPSTSServerSuite` PASS + live `AssumeRole`; effective permissions = parent ∩ session.
- ✅ **Q5 proven** — `TestIAMInternalIDPServerSuite` PASS + live 403; basic user cannot self-promote to `consoleAdmin`; root cause identified across two layers.
- ✅ **Citation accuracy** — ~132 `file:line` references across 27 files; unique citations resolve exactly (two minor Q3 range corrections applied).
- ✅ **Read-only guarantee intact** — zero source files modified; `git status` clean; all temporary artifacts removed.

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| None | — | — | — |

> No unresolved issues block release or validation. The MinIO build is clean, both cited evidence tests pass, all runtime evidence reproduces, and every unique citation resolves. The two off-by-one Q3 citation ranges discovered during validation were corrected and committed (`68dab37f5`).

### 1.5 Access Issues

| System / Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-------------------|----------------|-------------------|-------------------|-------|
| None identified | — | — | — | — |

> **No access issues identified.** The repository is fully accessible, the Go 1.23.2 toolchain and `mc` client are present, MinIO builds and runs, both cited test suites pass, and no external credentials or third-party API access are required (the Q1 KMS secret is a local, throwaway value).

### 1.6 Recommended Next Steps

1. **[High]** Assign a MinIO-knowledgeable engineer to perform an SME technical review of the answer document — confirm the five answers are sound and complete, and spot-check a sample of citations against commit `c07e5b49d477`.
2. **[Medium]** Independently reproduce the runtime evidence in a clean environment — rebuild MinIO, re-run the Q4/Q5 Go suites, and reproduce the Q1/Q2/Q3 live scenarios to confirm the verbatim outputs match.
3. **[Low]** Approve and merge the single documentation file to the target branch.

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|-------|-------------|
| Runtime foundation | 4 | Build MinIO (Makefile `build` target, `-tags kqueue -trimpath`); run in single-node FS and erasure 4-drive backends; install `mc`; configure KMS secret; set up alias + `mc admin trace`; smoke test (HTTP 200 health). |
| Q1 — SSE precedence evidence | 5 | KMS + bucket default-SSE config; broad-`s3:PutObject` user; unencrypted upload; verbose trace proving `isPutActionAllowed` → `sseConfig.Apply` → `EncryptRequest`; on-disk cleartext grep + `mc stat` SSE-S3. |
| Q2 — WORM DELETE evidence | 4 | Lock-enabled bucket + retention/legal hold; single + batch DELETE; capture HTTP 400 `InvalidRequest` / HTTP 200 per-object error; governance/compliance/legal-hold + fail-closed NTP analysis. |
| Q3 — Bitrot detect + self-heal evidence | 7 | Erasure EC 2+2 backend; locate + corrupt a data shard; `GET` reconstruction from parity; inline + deep heal `[Yellow → Green]`; sha256 shard-restore verification; honest runtime-vs-source note. |
| Q4 — STS session-policy (tests + demo) | 5 | Run `TestIAMInternalIDPSTSServerSuite`; live `AssumeRole` with restrictive inline session policy; JWT decode; prove effective = parent ∩ session. |
| Q5 — Escalation prevention (tests + demo) | 4 | Run `TestIAMInternalIDPServerSuite`; live basic-user attach-`consoleAdmin` attempt → 403; identify two-layer root cause. |
| Answer document authoring | 6 | 843-line structured Markdown: per-question command + verbatim output + citations + reasoning; 16-part coverage checklist; cleanup/read-only section. |
| Citation cataloging + verification | 4 | ~132 `file:line` references across 27 files; verification against source; two code-review rounds + two citation-range corrections. |
| Final validation + read-only cleanup + commit | 4 | `go build`/`vet`/`mod verify`; test re-runs; Q1–Q3 reproduction; temporary-artifact removal; commit on branch. |
| **Total Completed** | **43** | **Matches Completed Hours in Section 1.2.** |

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|----------|-------|----------|
| Documentation SME review & citation spot-check | 3 | High |
| Independent evidence reproduction (rebuild, re-run Q4/Q5 suites, reproduce Q1/Q2/Q3 live) | 2 | Medium |
| Merge & publish | 1 | Low |
| **Total Remaining** | **6** | **Matches Remaining Hours in Section 1.2 and Section 7 pie chart.** |

### 2.3 Total Project Hours Reconciliation

| Line | Hours |
|------|-------|
| Section 2.1 — Completed | 43 |
| Section 2.2 — Remaining | 6 |
| **Total Project Hours** | **49** |
| Completion (43 ÷ 49) | **87.8%** |

> **Integrity check:** Section 2.1 (43) + Section 2.2 (6) = 49 = Total Project Hours in Section 1.2. Remaining hours (6) are identical in Sections 1.2, 2.2, and 7.

---

## 3. Test Results

All entries below originate from Blitzy's autonomous validation logs for this project. The two Go suites are the AAP-designated cited-evidence tests; the four live scenarios are the runtime reproductions that back Q1–Q3.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|------------|-------|
| Q4 — STS session-policy suite | Go `testing` (`-tags kqueue,dev`) | 1 suite (ErasureSD) | 1 | 0 | N/A (evidence suite) | `TestIAMInternalIDPSTSServerSuite`; wires `TestSTS` + `TestSTSWithGroupPolicy`; `--- PASS (3.47s)`. |
| Q5 — IAM escalation suite | Go `testing` (`-tags kqueue,dev`) | 1 suite (ErasureSD) | 1 | 0 | N/A (evidence suite) | `TestIAMInternalIDPServerSuite`; wires `TestUserPolicyEscalationBug` + `TestServiceAccountPrivilegeEscalationBug`; `--- PASS (3.59s)`. |
| Q1 — SSE precedence (live) | `minio server` + `mc` + trace | 1 scenario | 1 | 0 | N/A (runtime) | Unencrypted `PutObject` → server-injected SSE-S3; encrypted at rest confirmed. |
| Q2 — WORM DELETE (live) | `minio server` + `minio-go`/`mc` + trace | 2 scenarios | 2 | 0 | N/A (runtime) | Single → HTTP 400 `InvalidRequest`; batch → HTTP 200 + per-object error. |
| Q3 — Bitrot detect + heal (live) | erasure `minio server` + `mc` + trace | 1 scenario | 1 | 0 | N/A (runtime) | Parity reconstruction on `GET`; deep heal `[Yellow → Green]`; sha256 shard restore. |
| **Totals** | — | **6** | **6** | **0** | — | **100% pass rate.** |

> **Notes on coverage:** these are behavioral/integration evidence tests and live reproductions, not unit-coverage runs; MinIO does not emit a coverage percentage for them in this workflow, hence `N/A`. Timings (3.47s / 3.59s) are independent re-run captures and fall within the expected variance of the document's first-run values (3.51s / 3.47s).

---

## 4. Runtime Validation & UI Verification

**Build & server runtime**

- ✅ **Operational** — `go build ./...` clean (exit 0); server binary builds: `minio version DEVELOPMENT.GOGET … Runtime: go1.23.2 linux/amd64`.
- ✅ **Operational** — Single-node filesystem backend starts; banner `MinIO Object Storage Server / Version: DEVELOPMENT.GOGET (go1.23.2 linux/amd64)`; `/minio/health/live` and `/minio/health/ready` → HTTP 200.
- ✅ **Operational** — Erasure 4-drive backend starts; banner `Formatting 1st pool, 1 set(s), 4 drives per set.`; health → HTTP 200.

**Per-question runtime evidence**

- ✅ **Operational** — Q1: broad-`Put` user's unencrypted upload to a default-SSE bucket returns SSE-S3 metadata; on-disk cleartext token NOT found in the secure bucket (encrypted), found in the control (plaintext) bucket.
- ✅ **Operational** — Q2: single `DeleteObject` → `Code=InvalidRequest` / `StatusCode=400`; batch `DeleteMultipleObjects` → HTTP 200 with `<Error><Code>InvalidRequest</Code>…` per object.
- ✅ **Operational** — Q3: `GET` returns correct bytes (md5 match) despite on-disk corruption; four `storage.CheckParts` + inline `heal.Object mode=0`; deep heal `[Yellow → Green]`, `Healed: 1/1 objects`.
- ✅ **Operational** — Q4: live `AssumeRole` → `PutObject` denied 403; `GetObject`/`ListObjects` allowed.
- ✅ **Operational** — Q5: live basic-user attach-`consoleAdmin` → 403 `AccessDenied`.

**UI verification**

- ⚠ **Not applicable** — This deliverable is a Markdown document; there is no user-facing UI, component library, or design system in scope. MinIO's embedded console was not part of the investigation and was not verified.

---

## 5. Compliance & Quality Review

Cross-mapping the AAP directives and quality benchmarks to observed status.

| Benchmark / AAP Directive | Requirement | Status | Progress |
|---------------------------|-------------|--------|----------|
| Deliverable location & name | `blitzy/documentation/minio_c07e5b49d477.md` | ✅ Pass | 100% — exists, 843 lines, committed |
| Run-first methodology | Build & run MinIO before writing | ✅ Pass | 100% — binary + both backends run |
| Quote-verbatim evidence | Verbatim trace/logs/HTTP/test markers | ✅ Pass | 100% — all five questions carry verbatim blocks |
| Full coverage | Every sub-part answered | ✅ Pass | 100% — 16/16 coverage-checklist items |
| Exactness + `file:line` citations | Literals cited and resolve | ✅ Pass | 100% — unique citations resolve after 2 fixes |
| Read-only scope | No source file modified | ✅ Pass | 100% — `git status` empty; 1 file added |
| Temporary-artifact cleanup | Remove temp scripts/dirs | ✅ Pass | 100% — all removed |
| Build (Go 1.23.2, `kqueue`) | Compiles cleanly | ✅ Pass | 100% — `go build`/`vet` exit 0 |
| Dependencies unchanged | `go.mod`/`go.sum` intact | ✅ Pass | 100% — `go mod verify` OK; 7 AAP deps present |
| Never claim 100% | Cap at 99% pre-review | ✅ Pass | Reported 87.8% |

**Fixes applied during autonomous validation**

- Code-review findings addressed (commit `87c358f5a`).
- Two coverage-checklist citations corrected to resolve from repo root (commit `dde92f695`).
- Two off-by-one Q3 `cmd/xl-storage.go` citation ranges corrected — `3126 → 3126-3127` and `3126-3128 → 3126-3129` (commit `68dab37f5`).

**Outstanding items:** none. All quality benchmarks pass.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Citation line-number drift if the repo advances past the pinned commit | Technical | Low | Low | Document pins commit `c07e5b49d477`; all unique citations verified at that commit | Mitigated |
| Runtime trace non-determinism (Go map-iteration order in the heal-trace tag) | Technical | Low | Low | Document explicitly notes this as expected non-determinism (map-backed trace tags), not a defect | Resolved |
| Reviewer disagreement with a technical interpretation | Technical | Low | Low | Every claim grounded in `file:line` + verbatim output; explicit honesty notes for limitations (e.g., Q3(b) emits no literal `"file is corrupted"` log) | Mitigated |
| Q3 reproduction requires an erasure backend (4 drives) + timing variance | Operational | Low | Medium | Document specifies exact backend layout, commands, and env; timings labeled as first-run captures | Documented |
| Reproduction tooling (`mc` client, `MINIO_KMS_SECRET_KEY`) must be installed/set separately | Operational | Low | Low | Document includes install commands and KMS secret format | Documented |
| Security regression from code changes | Security | None | N/A | Zero source modifications; the document explains existing security behavior, it does not alter it | N/A |
| External integration failure | Integration | None | N/A | No external service integration; STS/IAM/KMS/erasure exercised locally with throwaway data | N/A |

> **Overall risk posture: LOW.** Because the task is strictly read-only with zero source changes and full runtime validation, there is no security or integration risk surface. The residual technical/operational risks are informational and already mitigated or documented within the deliverable.

---

## 7. Visual Project Status

**Project hours breakdown**

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#111111','pieLegendTextColor':'#111111'}}}%%
pie showData title Project Hours (Total 49h)
    "Completed Work" : 43
    "Remaining Work" : 6
```

**Remaining work by priority (hours)**

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#A8FDD9','pie3':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#111111','pieLegendTextColor':'#111111'}}}%%
pie showData title Remaining 6h by Priority
    "High — SME review" : 3
    "Medium — Reproduction" : 2
    "Low — Merge & publish" : 1
```

> **Integrity:** the pie chart "Remaining Work" value (6) equals the Remaining Hours in Section 1.2 and the sum of the Section 2.2 "Hours" column (3 + 2 + 1 = 6). "Completed Work" (43) equals Section 2.1's total. Colors: Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`.

---

## 8. Summary & Recommendations

**Achievements.** The project delivers exactly what the Agent Action Plan scoped: one comprehensive, runtime-grounded answer document that resolves all five MinIO questions with verbatim evidence and exact `file:line` citations. MinIO was built and run at the pinned commit in both filesystem and erasure backends; the two cited Go evidence suites pass; the Q1/Q2/Q3 live scenarios reproduce byte-for-byte; and the repository working tree is left unchanged.

**Remaining gaps.** There are **no engineering gaps** — nothing is broken, no test fails, and no functionality is missing. The remaining **6 hours (12.2%)** are entirely path-to-production human activities: an SME technical review of the document, an independent reproduction of the evidence, and the final merge.

**Critical path to production.** SME review → independent reproduction → merge. None of these steps depend on further autonomous work.

**Success metrics.** 100% of AAP deliverables complete; 6/6 evidence executions passing (2 Go suites + 4 live scenarios); unique citations resolving; read-only guarantee intact (`git status` clean).

**Production-readiness assessment.** The deliverable is **production-ready pending human sign-off**. Consistent with the honest-assessment principle (never report 100% before human review), the project is assessed at **87.8% complete** — all autonomous work is done; the balance is human acceptance.

| Metric | Value |
|--------|-------|
| AAP deliverables complete | 9 / 9 |
| Evidence executions passing | 6 / 6 (100%) |
| Source files modified | 0 |
| Completion | 87.8% |

---

## 9. Development Guide

Every command below was executed and verified in the validation environment (Linux/amd64, Go 1.23.2).

### 9.1 System Prerequisites

- **OS/Arch:** Linux `amd64`.
- **Go:** `1.23.x` (validated with `go1.23.2`). The module directive is `go 1.23` (`go.mod:3`).
- **Git + Git LFS.**
- **`mc` (MinIO Client):** installed separately (external tool, not a `go.mod` dependency).
- **`curl`** for health probes.
- **Q3 only:** four writable disk directories (erasure backend).
- **Q1 only:** a KMS secret in `MINIO_KMS_SECRET_KEY`.

### 9.2 Environment Setup

```bash
# Pin the toolchain so Go does not attempt to auto-download another version
export GOTOOLCHAIN=local

# Root credentials for a local server
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin

# Q1 KMS secret (local, throwaway — format "<key-name>:<base64-32-bytes>")
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="
```

### 9.3 Build

```bash
# Fast full-tree compile check (expect no output, exit 0)
go build ./...

# Production build (Makefile 'build' target)
make build            # -> ./minio

# Equivalent explicit form
CGO_ENABLED=0 go build -tags kqueue -trimpath -o ./minio ./
./minio --version     # -> DEVELOPMENT.GOGET / Runtime: go1.23.2 linux/amd64

# Install the mc client
go install github.com/minio/mc@latest
```

### 9.4 Application Startup

```bash
# Single-node filesystem backend (Q1, Q2, Q5 live)
./minio server /tmp/data --address 127.0.0.1:9000

# Erasure backend — four drives, EC 2+2 (required for Q3)
./minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address 127.0.0.1:9000
# boot banner: "Formatting 1st pool, 1 set(s), 4 drives per set."
```

### 9.5 Verification

```bash
# Liveness / readiness — both should return HTTP 200
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/live
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready

# Register an alias for the running server
mc alias set local http://127.0.0.1:9000 minioadmin minioadmin

# Capture all trace call types verbosely (run in a second terminal)
mc admin trace -a -v local
```

### 9.6 Running the Cited Evidence Tests (Q4, Q5)

```bash
# Q4 — STS session-policy suite
env -u KUBERNETES_SERVICE_HOST GOTOOLCHAIN=local \
  MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=" \
  go test -run '^TestIAMInternalIDPSTSServerSuite$/^Test:_1,_ServerType:_ErasureSD$' \
  -v -tags kqueue,dev ./cmd
# expect: --- PASS: TestIAMInternalIDPSTSServerSuite (~3.5s) / PASS / ok

# Q5 — IAM escalation suite
env -u KUBERNETES_SERVICE_HOST GOTOOLCHAIN=local \
  MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=" \
  go test -run '^TestIAMInternalIDPServerSuite$/^Test:_1,_ServerType:_ErasureSD$' \
  -v -tags kqueue,dev ./cmd
# expect: --- PASS: TestIAMInternalIDPServerSuite (~3.5s) / PASS / ok
```

### 9.7 Example Usage (per-question reproduction sketch)

```bash
# Q1 — auto-encryption of an unencrypted upload under a default-SSE bucket
mc mb local/securebucket
mc encrypt set sse-s3 local/securebucket
mc cp ./plain.txt local/securebucket/         # uploaded without a client SSE header
mc stat local/securebucket/plain.txt          # -> Encryption: SSE-S3 (server-injected)

# Q2 — WORM delete rejection
mc mb --with-lock local/lockbucket
mc cp ./worm.txt local/lockbucket/
mc retention set --default COMPLIANCE 1d local/lockbucket
mc rm local/lockbucket/worm.txt               # -> WORM protected; cannot be overwritten

# Q3 — deep heal after shard corruption (erasure backend)
mc admin heal -r --verbose --scan deep local/ecbucket
# -> [Yellow -> Green] ecbucket/ec_object.bin ; Healed: 1/1 objects
```

### 9.8 Troubleshooting

- **`error: externally-managed-environment` on `pip install`** — Ubuntu 25 uses PEP 668. Use `pip install --break-system-packages <pkg>` or a virtualenv.
- **Go tries to download a different toolchain** — set `export GOTOOLCHAIN=local`.
- **Q4/Q5 suite hangs or misbehaves** — run with `KUBERNETES_SERVICE_HOST` unset (`env -u KUBERNETES_SERVICE_HOST`), `MINIO_KMS_SECRET_KEY` set, and `-tags kqueue,dev`.
- **No bitrot/heal on a single disk** — Q3 requires an erasure backend (four drives); the filesystem backend has no parity and cannot detect or heal bitrot.
- **`mc --version` shows a different Go runtime than the server** — expected; `mc` is an independent external tool.

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---------|---------|
| Compile check | `go build ./...` |
| Production build | `make build` / `CGO_ENABLED=0 go build -tags kqueue -trimpath -o ./minio ./` |
| Static analysis | `go vet -tags kqueue ./cmd/` |
| Verify modules | `go mod verify` |
| Single test | `go test -run '^TestName$' -v -tags kqueue,dev ./cmd` |
| Start FS server | `./minio server /tmp/data --address 127.0.0.1:9000` |
| Start erasure server | `./minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address 127.0.0.1:9000` |
| Trace (all types) | `mc admin trace -a -v local` |
| Deep heal | `mc admin heal -r --verbose --scan deep local/<bucket>` |

### B. Port Reference

| Port | Purpose |
|------|---------|
| 9000 | Default S3 API address (`--address`) used in the guide examples |
| 9111 / 9112 | FS / erasure server ports used during validation testing |
| `/minio/health/live` | Liveness probe (HTTP 200) |
| `/minio/health/ready` | Readiness probe (HTTP 200) |
| Console (WebUI) | Auto-assigned unless pinned via `--console-address` |

### C. Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | The single deliverable (answer document) |
| `cmd/object-handlers.go` | Q1 authorization→SSE ordering; Q2 `DeleteObjectHandler` |
| `cmd/encryption-v1.go` | Q1 `EncryptRequest` (`:466`), `setEncryptionMetadata` (`:440`) |
| `cmd/bucket-object-lock.go` | Q2 `enforceRetentionBypassForDelete` (`:84`) |
| `cmd/object-api-errors.go` | Q2 WORM error string (`:339-341`) |
| `cmd/api-errors.go` | Q2 `ErrObjectLocked` → HTTP 400 (`:1059-1063`) |
| `cmd/storage-errors.go` | Q3 `errFileCorrupt = "file is corrupted"` (`:103-104`) |
| `cmd/bitrot.go` | Q3 algorithm constants (`:40-43`) |
| `cmd/erasure-object.go` | Q3 `BitrotScan` heal enqueue (`:407`) |
| `cmd/iam.go` | Q4 `IsAllowedSTS` intersection (`:2312`) |
| `cmd/sts-handlers.go` | Q4 `maxSTSSessionPolicySize = 2048` (`:89`) |
| `cmd/auth-handler.go` | Q5 `checkAdminRequestAuth` → `ErrAccessDenied` (`:189/206`) |

### D. Technology Versions

| Component | Version |
|-----------|---------|
| Go toolchain | `go1.23.2` (module directive `go 1.23`) |
| MinIO commit | `c07e5b49d477b0774f23db3b290745aef8c01bd2` |
| `github.com/minio/madmin-go/v3` | `v3.0.77` |
| `github.com/minio/pkg/v3` | `v3.0.22` |
| `github.com/minio/kms-go/kms` | `v0.4.0` |
| `github.com/minio/kms-go/kes` | `v0.3.0` |
| `github.com/minio/sio` | `v0.4.1` |
| `github.com/minio/highwayhash` | `v1.0.3` |
| `github.com/golang-jwt/jwt/v4` | `v4.5.1` |
| `mc` client | external tool (installed separately) |

### E. Environment Variable Reference

| Variable | Purpose |
|----------|---------|
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | Server root credentials |
| `MINIO_KMS_SECRET_KEY` | Q1 KMS secret (`<key-name>:<base64-32-bytes>`) |
| `GOTOOLCHAIN=local` | Prevent Go from auto-downloading a different toolchain |
| `KUBERNETES_SERVICE_HOST` | Unset (`env -u …`) when running the Q4/Q5 suites |
| `MINIO_API_REQUESTS_MAX` | Used by the Makefile `test` target |

### F. Developer Tools Guide

| Tool | Usage |
|------|-------|
| `mc admin trace -a -v` | Capture all trace call types (s3, storage, healing, admin) — the primary runtime-evidence source for Q1/Q2/Q3 |
| `mc admin heal --scan deep` | Trigger a deep/bitrot-scan heal (Q3) |
| `go test -tags kqueue,dev` | Run the co-located Go evidence suites (Q4/Q5) |
| `curl /minio/health/{live,ready}` | Verify server readiness |

### G. Glossary

| Term | Meaning |
|------|---------|
| SSE / SSE-S3 / SSE-KMS | Server-Side Encryption (managed / KMS-backed) |
| WORM | Write-Once-Read-Many (object-lock retention / legal hold) |
| Bitrot | Silent on-disk data corruption detected via stored-hash verification |
| Erasure coding (EC 2+2) | Data + parity sharding enabling reconstruction and self-heal |
| MRF | Metadata Refresh/heal subsystem that records partial-op heal data |
| STS | Security Token Service — temporary credentials via `AssumeRole` |
| Session policy | Inline policy on temporary credentials; effective perms = parent ∩ session |
| IAM | Identity and Access Management (users, policies, admin actions) |
| `consoleAdmin` | The built-in administrator policy a basic user must not self-attach |

---

*This Blitzy Project Guide reports **87.8% completion (43 of 49 hours)**. All Agent Action Plan deliverables are complete and validated; the remaining 6 hours are path-to-production human activities (SME review, independent reproduction, merge). Completed = Dark Blue `#5B39F3`; Remaining = White `#FFFFFF`.*