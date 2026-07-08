# Blitzy Project Guide

> **Project:** MinIO Erasure-Coding Healing Decision — Onboarding Investigation & Answer Document
> **Branch:** `blitzy-2da61bc3-469c-49fb-996a-35247342f6b3`  •  **Base commit:** `c07e5b49d477`  •  **HEAD:** `0eddf5c15`
> **Task type:** Read-only documentation/onboarding investigation (single net-new deliverable)

---

## 1. Executive Summary

### 1.1 Project Overview

This project delivers a single, runtime-evidence-backed onboarding document that explains how MinIO's erasure-coding healing subsystem decides — on a default 4-disk **EC:2** erasure set — whether to **reconstruct** an inconsistent object, leave it **deleted** (purge as dangling), or leave it **degraded** (refuse). The audience is engineers onboarding to the MinIO codebase. Scope is strictly read-only: the entire MinIO Go source tree is consulted as reference, and the sole repository artifact is the answer document `blitzy/documentation/minio_c07e5b49d477.md`. Every behavioral claim is grounded in captured, unedited runtime output and a `file:line` citation, produced by building and running MinIO through its real healing entry points (`mc admin heal`, in-process `HealObject`).

### 1.2 Completion Status

The project is **88.7% complete** on an AAP-scoped basis. All autonomous investigation and authoring work is delivered and validated; the remaining work is human path-to-production verification (SME review, reproduction, merge).

```mermaid
%%{init: {"theme": "base", "themeVariables": {"pie1": "#5B39F3", "pie2": "#FFFFFF", "pieStrokeColor": "#B23AF2", "pieStrokeWidth": "2px", "pieOuterStrokeColor": "#B23AF2", "pieOuterStrokeWidth": "2px", "pieSectionTextColor": "#B23AF2", "pieTitleTextSize": "16px"}}}%%
pie showData title AAP-Scoped Completion — 88.7% Complete
    "Completed Work (AI)" : 47
    "Remaining Work" : 6
```

| Metric | Hours |
|---|---|
| **Total Hours** | **53** |
| Completed Hours (AI) | 47 |
| Completed Hours (Manual) | 0 |
| **Completed Hours (AI + Manual)** | **47** |
| **Remaining Hours** | **6** |
| **Percent Complete** | **88.7%** |

> Completion formula (PA1, hours-based): `47 / (47 + 6) = 47 / 53 = 88.7%`. All completed hours were performed autonomously by Blitzy agents (0 manual hours to date).

### 1.3 Key Accomplishments

- ✅ Built MinIO in its canonical default configuration (`make build`, `CGO_ENABLED=0 go build -tags kqueue`) and stood up a real 4-drive single-node **EC:2** server (`mc admin info` → "4 drives online, EC:2").
- ✅ Answered all six question items (Q1, Q2, Q3, Q4a, Q4b, Q5) with **actual, unedited runtime output** through the real heal entry points — no debug hooks or synthetic stand-ins.
- ✅ Reproduced all **three terminal healing outcomes** — RECONSTRUCT, PURGE (dangling→deleted), REFUSE (`errErasureReadQuorum`) — plus the "already-gone" fast-path.
- ✅ Established the 4-disk EC:2 boundary numbers from the code and confirmed them at runtime: `dataBlocks=2`, `parityBlocks=2`, `readQuorum=2`, `writeQuorum=3`.
- ✅ Proved the partial-**write** vs partial-**delete** divergence structurally via an in-process `isObjectDangling` call (`parityBlocks` threshold for a normal object vs `dataBlocks=(N+1)/2` for a delete marker).
- ✅ Authored the 1,863-line deliverable with **66** byte-exact `file:line` citations, a Mermaid decision diagram, and a full coverage-pass checklist.
- ✅ Maintained the read-only rule end-to-end: branch delta = **exactly one added file**; zero source/build/config changes; all ephemeral scaffolding removed; working tree clean.
- ✅ Passed 5/5 Blitzy production-readiness gates (build, tests-in-isolation, zero unresolved errors, in-scope file validated, committed + read-only intact).

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|---|---|---|---|
| *(none)* — No blocking issues. The deliverable is complete, builds clean, tests pass in isolation, and all six questions reproduced at runtime. | N/A | N/A | N/A |

> There are **no critical unresolved issues**. The only outstanding items are non-blocking human path-to-production verification tasks (see §1.6 and §2.2). The lone pre-existing test-ordering sensitivity (batch-run flakiness of `TestHealObjectCorruptedParts`) is out-of-scope, documented honestly, and not a product defect (see §6, R5).

### 1.5 Access Issues

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|---|---|---|---|---|
| MinIO source repository | Read/Write (branch) | Full access; single deliverable committed | ✅ Resolved | Blitzy Agent |
| Go module cache / toolchain | Build | Go 1.23.12 present; modules resolve | ✅ Resolved | Blitzy Agent |
| `dl.min.io` (mc client fetch) | Network egress | `mc` fetched via `curl -sL` (302→GitHub); works in this environment; would be blocked in air-gapped envs (in-process `HealObject` path is the credential-free alternative) | ✅ Resolved (mitigated) | Human reviewer (if reproducing air-gapped) |

> **No blocking access issues identified.** All resources required to build, run, and validate the deliverable were accessible.

### 1.6 Recommended Next Steps

1. **[High]** Have a MinIO erasure-coding/healing SME review the technical analysis — the reconstruct/purge/refuse trichotomy, the boundary numbers (`dataBlocks=2, parityBlocks=2, readQuorum=2, writeQuorum=3`), and the `isObjectDangling` threshold claims (**2h**).
2. **[High]** Spot-verify a representative sample of the 66 `file:line` citations against current source to catch any residual line-drift (**1h**).
3. **[Medium]** Independently reproduce ≥2 representative scenarios (one RECONSTRUCT + one PURGE/REFUSE) on a fresh 4-drive EC:2 server following the commands in §9 / deliverable §10 (**2h**).
4. **[Low]** Final editorial pass (proofread, confirm the Mermaid diagram renders, check links) and merge/index the document into the team's onboarding materials (**1h**).

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

All completed work was performed autonomously by Blitzy agents and traces to an AAP requirement (§0.1.1 / §0.5).

| Component | Hours | Description |
|---|---|---|
| Runtime foundation | 3 | `make build` (canonical), 4-drive single-node EC:2 server, `mc` alias, EC:2 confirmation (`mc admin info`, `xl.meta` EcM=2/EcN=2). |
| Baseline & backend shard-layout mapping | 2 | Create a known-good object; map `xl.meta` + `<dataDir>/part.N` across `/tmp/d1..d4` for fault injection. |
| Q1 — reconstruct vs. stay-deleted vs. stay-degraded | 4 | `healObject`/`cannotHeal` (`cmd/erasure-healing.go:428`) + `deleteIfDangling`; reproduce all 3 outcomes + "already-gone". |
| Q2 — before/after status indicators | 3 | `HealResultItem.Before/After.Drives[]` `DriveState` transitions + `mc admin heal --verbose` color key. |
| Q3 — why-logs | 4 | `mc admin trace --call heal`, `HealObject` audit event, decisive `DeleteDanglingObject` audit; honest "no rationale in console" finding. |
| Q4a/Q4b — boundary walk + error strings | 5 | 4→3→2→1 valid-shard walk; `errErasureReadQuorum` / `errFileNotFound` / `errFileVersionNotFound`; actionability (corrupt-part vs offline-disk); 2-run repeats; `:309` in-process probe. |
| Q5 — partial-write vs partial-delete divergence | 5 | `isObjectDangling` (`:968`) `parityBlocks` vs `dataBlocks=(N+1)/2`; in-process function-level proof; 2-run rungs. |
| Scan-mode analysis (normal vs deep) | 2 | `HealNormalScan`/`HealDeepScan`; why silent bitrot needs a deep scan; label each observation's scan mode. |
| Code reading + citation verification | 4 | Read ~25 reference files; verify 66 `file:line` citations byte-exact. |
| Deliverable authoring (1,863 lines) | 8 | Executive answer, Mermaid decision diagram, per-question evidence blocks, tables, coverage pass. |
| Autonomous validation + 5 QA remediation rounds | 6 | Initial draft + major +1317/−472 remediation + 3 refinement rounds (citations, provenance, Q4b attribution, sub-case count). |
| Cleanup + read-only compliance verification | 1 | Remove `/tmp/d1..d4`, `mc`, scripts, binary; verify repo byte-for-byte unchanged. |
| **Total Completed** | **47** | |

### 2.2 Remaining Work Detail

All remaining work is human path-to-production verification. No autonomous "creation" work remains.

| Category | Hours | Priority |
|---|---|---|
| Human SME technical-accuracy review (healing analysis, boundary numbers, 66 citations) | 3 | High |
| Independent reproduction of documented commands on a fresh environment | 2 | Medium |
| Final editorial pass + merge/publish to onboarding docs | 1 | Low |
| **Total Remaining** | **6** | |

> **Cross-section integrity:** Section 2.1 (47) + Section 2.2 (6) = **53** = Total Hours in §1.2. Section 2.2 total (6) = Remaining Hours in §1.2 = "Remaining Work" in §7 pie chart.

---

## 3. Test Results

All tests below originate from **Blitzy's autonomous validation logs** for this project. Because this is a strictly read-only investigation, no new test files were authored; the repository's own healing suite was exercised as the in-process reproduction template, and the six question items were reproduced end-to-end on a live server. Command form (isolated): `MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 go test -tags kqueue,dev -run '^<Test>$' ./cmd/ -count=1 -timeout 10m`.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---|---|---|---|---|---|---|
| Unit — dangling classifier | Go `testing` | 13 | 13 | 0 | N/A | `TestIsObjectDangling` (13 sub-cases), `ok 0.252s` — normal-object vs delete-marker thresholds (Q5). |
| Integration — object-layer heal | Go `testing` | 3 | 3 | 0 | N/A | `TestHealingDanglingObject` (`ok 0.470s`), `TestHealCorrectQuorum` (`ok 0.555s`), `TestHealObjectCorruptedParts` (`ok 0.501s`) — run in isolation. |
| Runtime / End-to-End — live heal | `minio server` + `mc admin heal`/`trace` | 6 | 6 | 0 | N/A | Q1–Q5 (Q4 split a/b) reproduced on a 4-drive EC:2 server; boundary & Q5 rungs run ≥2× to report distribution. |
| **Totals** | — | **22** | **22** | **0** | **N/A** | 100% pass on all executed checks. |

> **Coverage %** is marked **N/A** honestly: the task authored no new tests and did not run a coverage-driven suite — it exercised existing tests and real runtime paths as evidence. **Test-ordering note:** run as a single batch, `TestHealObjectCorruptedParts` intermittently fails (`erasure-healing_test.go:1386`) due to MinIO's process-global heal state — a **pre-existing, out-of-scope** sensitivity; each test **passes deterministically in isolation** (the results above). No source file was modified.

---

## 4. Runtime Validation & UI Verification

**Runtime health (live 4-drive EC:2 single-node server):**

- ✅ **Operational** — Server builds and starts; banner logs `Formatting 1st pool, 1 set(s), 4 drives per set`.
- ✅ **Operational** — Storage scheme confirmed **EC:2** (`mc admin info local` → "4 drives online, EC:2"; `xl.meta` decode `EcM=2`, `EcN=2`).
- ✅ **Operational** — Real heal entry point `mc admin heal -r --verbose --json local/<bucket>` returns `HealResultItem` with per-drive `Before`/`After` states.
- ✅ **Operational** — `mc admin trace --call heal` surfaces the heal trace (`mode=1`, HealNormalScan).

**Healing decision verification (all reproduced at runtime):**

- ✅ **Operational** — RECONSTRUCT: `Before` `missing`/`corrupt` → `After` `DriveStateOk` when `disksToHealCount ≤ parityBlocks (2)`.
- ✅ **Operational** — PURGE: object classified dangling and `DeleteVersion`-d (`DeleteDanglingObject` audit, `caller=:438`, `d:p=2:2`) with actionable missing errors beyond parity.
- ✅ **Operational** — REFUSE: `errErasureReadQuorum` ("Read failed. Insufficient number of drives online") when errors are non-actionable (corrupt part / offline disk); object preserved (2/2 runs).
- ✅ **Operational** — Boundary: minimum **2** valid shards (=`dataBlocks`) required for success; transition at `disksToHealCount > parityBlocks(2)`.
- ✅ **Operational** — Scan mode: silent bitrot missed by normal scan (`mode=1`), caught by deep scan (`mode=2`).

**UI verification:** ⚠ **Not applicable** — this is a backend documentation deliverable; there is no web-UI artifact in scope (AAP §0.5.3). The closest "UI" is the `mc admin heal --verbose` terminal output (Green/Yellow/Red/Grey color key), whose before→after color transition was verified in §5 of the deliverable. No MinIO Console UI change was made or required.

---

## 5. Compliance & Quality Review

This matrix cross-maps the AAP's governing rules ("SWE-AtlasQnA-Repo") and quality benchmarks to their delivered status. Fixes applied during autonomous validation are noted.

| Benchmark / AAP Rule | Requirement | Status | Progress | Notes / Fixes Applied |
|---|---|---|---|---|
| Deliverable location & name (§0.7.1) | Single file `blitzy/documentation/minio_c07e5b49d477.md` | ✅ Pass | 100% | Exactly one added file; correct name (= branch name). |
| Run-first methodology (§0.7.2) | Build & run real paths; write from observation | ✅ Pass | 100% | 77 recorded commands; unedited output for every claim. |
| Real entry point (§0.7.2) | `mc admin heal` / in-process `HealObject`; no debug hooks | ✅ Pass | 100% | On-demand `mc` path + in-process API; non-canonical values labeled. |
| Default canonical config (§0.7.2) | Default 4-drive EC:2, not 16-drive test default | ✅ Pass | 100% | EC:2 confirmed at runtime; boundary numbers from 4-disk config. |
| Every condition + before/during/after (§0.7.2) | Primary + error/edge + state transitions | ✅ Pass | 100% | 3 outcomes + already-gone; before→after drive states captured. |
| Run-to-run distribution (§0.7.2) | Repeat ambiguous inputs; report distribution | ✅ Pass | 100% | Boundary & Q5 rungs run 2× (16 repeated-run captures). |
| Evidence completeness (§0.7.3) | Unedited output + command + `file:line` per claim | ✅ Pass | 100% | 66 citations; byte-exact error strings; inferred items labeled (8×). |
| Answer every named item (§0.7.3) | Q1–Q5 + every mechanism/flag/function by name | ✅ Pass | 100% | Coverage-pass table (§11) maps each item→section→evidence. |
| Read-only scope (§0.7.4) | No source edits; cleanup all scaffolding | ✅ Pass | 100% | Zero `.go`/`.mod`/`.sum`/`Makefile` changes; `/tmp` dirs removed. |
| Citation accuracy | `file:line` resolves byte-exact | ✅ Pass | 100% | Spot-checked (`cannotHeal:428`, `errErasureReadQuorum:23`, `DefaultParityBlocks` EC:2, etc.); one off-by-one (14→13) fixed. |
| Build integrity | Compiles clean in default config | ✅ Pass | 100% | `CGO_ENABLED=0 go build -tags kqueue` → clean 150M binary, EXIT 0. |
| Markdown structure | Valid fences/tables/diagram | ✅ Pass | 100% | 146 balanced code fences; well-formed tables; 1 Mermaid diagram. |
| Inferred vs. observed | Label anything not reproduced | ✅ Pass | 100% | `quorumETag` override & unparseable-`xl.meta` branch labeled inferred. |

**Outstanding compliance items:** none autonomous. Human SME sign-off on technical accuracy is the remaining verification gate (§1.6, §2.2).

---

## 6. Risk Assessment

Overall risk posture: **LOW**. The deliverable is a self-contained, validated markdown document with zero source changes.

| Risk | Category | Severity | Probability | Mitigation | Status |
|---|---|---|---|---|---|
| R1 — `file:line` citation line-drift as MinIO source evolves | Technical | Low | Medium (over time) | Deliverable pins the exact HEAD; re-verify citations on source version bumps | Mitigated |
| R2 — Residual line-number precision (one off-by-one already found/fixed) | Technical | Low | Low | SME re-grep sample during review (HT-2) | Open (low) |
| R3 — Inferred-not-observed claims (`quorumETag`, unparseable `xl.meta`) | Technical | Low | Low | Explicitly labeled "inferred from code"; reproducible if desired | Mitigated |
| R4 — Security exposure introduced | Security | Informational | N/A | Read-only markdown; no code/deps/credentials/attack surface; default creds only in ephemeral local repro (removed) | N/A |
| R5 — Pre-existing batch test-ordering flakiness (`TestHealObjectCorruptedParts`) | Operational | Low | N/A (out of scope) | Run tests in isolation (`-run '^<Test>$' -count=1`); documented honestly; no source modified | Documented |
| R6 — Reproduction environment drift (`mc` version / dl.min.io redirect) | Operational | Low | Low | Doc pins `mc RELEASE.2025-08-13` and exact commands | Mitigated |
| R7 — `mc` fetch requires network egress (air-gapped envs) | Integration | Low | Low | In-process `HealObject` path needs no `mc`; fetch cmd + version recorded | Mitigated |

> The only genuinely outstanding item is human SME independent verification of the technical claims — which is the *remaining work* (§2.2), not a defect.

---

## 7. Visual Project Status

**Project hours breakdown** (Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`):

```mermaid
%%{init: {"theme": "base", "themeVariables": {"pie1": "#5B39F3", "pie2": "#FFFFFF", "pieStrokeColor": "#B23AF2", "pieStrokeWidth": "2px", "pieOuterStrokeColor": "#B23AF2", "pieOuterStrokeWidth": "2px", "pieSectionTextColor": "#B23AF2", "pieTitleTextSize": "16px"}}}%%
pie showData title Project Hours — Completed vs Remaining
    "Completed Work" : 47
    "Remaining Work" : 6
```

**Remaining work by priority** (sums to the 6 remaining hours in §1.2 and §2.2):

| Priority | Hours | Share of remaining | Tasks |
|---|---|---|---|
| 🔵 High | 3 | 50% | SME technical review (2h) + citation spot-verify (1h) |
| 🔵 Medium | 2 | 33% | Independent reproduction (2h) |
| 🔵 Low | 1 | 17% | Editorial pass + merge/publish (1h) |
| **Total** | **6** | **100%** | |

> **Integrity:** "Remaining Work" = **6** in the pie chart, equal to §1.2 Remaining Hours and the §2.2 Hours total. "Completed Work" = **47**, equal to §1.2 Completed Hours and the §2.1 total.

---

## 8. Summary & Recommendations

**Achievements.** The project delivers a complete, rigorously-evidenced onboarding answer to a non-trivial systems question: *when does MinIO's erasure-coding healing reconstruct an object, and when does it leave it deleted or degraded?* On a default 4-disk **EC:2** set, the document proves — with unedited runtime output and 66 byte-exact `file:line` citations — that `healObject` produces **three terminal outcomes** (RECONSTRUCT / PURGE / REFUSE) plus an "already-gone" fast-path, gated by the `cannotHeal` predicate and the `isObjectDangling` classifier, with boundary numbers `dataBlocks=2, parityBlocks=2, readQuorum=2, writeQuorum=3`. All six question items (Q1, Q2, Q3, Q4a, Q4b, Q5) are answered and reproduced at runtime.

**Remaining gaps.** No autonomous work remains. The 6 remaining hours are human path-to-production verification: SME technical-accuracy review, independent reproduction, and an editorial pass + merge. These gate the document's adoption as *authoritative* onboarding material but do not block its correctness as validated.

**Critical path to production.** SME review (§1.6 #1–2) → independent reproduction (§1.6 #3) → editorial pass + merge (§1.6 #4). Estimated **6 hours** total.

**Production readiness assessment.** The deliverable is **production-ready pending human sign-off**. It passes all five Blitzy validation gates, builds clean, its cited tests pass in isolation, and the read-only rule is intact end-to-end (branch delta = one added file). At **88.7%** AAP-scoped completion, the project has completed all autonomous investigation and authoring; only human verification stands between validated and merged.

| Success Metric | Target | Actual |
|---|---|---|
| AAP question items answered with runtime evidence | 6/6 | ✅ 6/6 |
| Read-only rule (source tree unchanged) | 0 source changes | ✅ 0 |
| Cited healing tests passing (isolation) | 4/4 | ✅ 4/4 |
| Build integrity | Clean | ✅ Clean (EXIT 0) |
| Citation accuracy (spot-check) | Byte-exact | ✅ Byte-exact |
| AAP-scoped completion | — | **88.7%** |

---

## 9. Development Guide

This guide covers (A) reading the deliverable and (B) reproducing the runtime investigation. Every command below was extracted from the deliverable's own §2/§10 and tested non-destructively in the build environment.

### 9.1 System Prerequisites

- **OS:** Linux (developed on Ubuntu 25.10 container).
- **Go toolchain:** `go 1.23` (runtime `go1.23.12`) — `go.mod` declares `go 1.23` with no `toolchain` directive.
- **Build flag:** `CGO_ENABLED=0`.
- **Disk:** a few GB free under `/tmp` for the four data directories (environment had 24T free).
- **Client:** `mc` admin client (`RELEASE.2025-08-13`) for the on-demand heal/trace entry points. *(The in-process `HealObject` path via `go test` needs no `mc`.)*

### 9.2 Environment Setup

```bash
# Put Go 1.23.12 on PATH (container convenience script)
source /etc/profile.d/golang.sh
go version                       # => go version go1.23.12 linux/amd64

# Default (demo) credentials for the ephemeral local server
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin
```

### 9.3 Build (default, canonical)

```bash
cd /path/to/minio-repo
make build                       # runs: CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o ./minio
./minio --version                # confirms the version banner
```

*Verified:* `make -n build` resolves to the dependency checks + the exact `CGO_ENABLED=0 go build -tags kqueue ...` line above; `go list ./cmd/` compiles clean (exit 0).

### 9.4 Run a 4-drive single-node EC:2 server

```bash
mkdir -p /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4
./minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 \
  --address :9000 --console-address :9001 > /tmp/minio.log 2>&1 &
echo $! > /tmp/minio.pid          # capture PID for clean shutdown
```

Expected first-launch log (confirms topology): `INFO: Formatting 1st pool, 1 set(s), 4 drives per set.` → default parity for a 4-drive set is **EC:2**.

### 9.5 Verification

```bash
# Configure the admin client
/tmp/mc alias set local http://127.0.0.1:9000 minioadmin minioadmin
# Confirm the erasure scheme
/tmp/mc admin info local          # => "4 drives online, EC:2"
# Run the real on-demand heal entry point (HealNormalScan / mode=1)
/tmp/mc admin heal -r --verbose --json local/<bucket>
# Live heal trace
/tmp/mc admin trace --call heal --verbose local
```

### 9.6 Reading the Deliverable

```bash
wc -l blitzy/documentation/minio_c07e5b49d477.md   # => 1863
less blitzy/documentation/minio_c07e5b49d477.md      # or any markdown viewer
```

### 9.7 Reproduce a Healing Scenario (example)

```bash
# 1) Create a known-good object
echo "hello erasure" > /tmp/obj1.dat
/tmp/mc mb local/testbucket
/tmp/mc cp /tmp/obj1.dat local/testbucket/obj1

# 2) Locate its shards on the backend (xl.meta + <dataDir>/part.1 per disk)
find /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 -path '*testbucket/obj1*'

# 3) Inject a fault (e.g., delete the part on one disk = "missing" shard)
#    then heal and observe Before 'missing' -> After 'ok'
/tmp/mc admin heal -r --verbose --json local/testbucket
```

### 9.8 In-Process Reproduction (no `mc` needed)

```bash
# Run a cited healing test in ISOLATION (avoids the shared-process ordering sensitivity)
MINIO_API_REQUESTS_MAX=10000 CGO_ENABLED=0 \
  go test -tags kqueue,dev -run '^TestIsObjectDangling$' ./cmd/ -count=1 -timeout 10m
```

### 9.9 Cleanup (leaves the repository byte-for-byte unchanged)

```bash
kill "$(cat /tmp/minio.pid)"                 # stop the server via captured PID
rm -rf /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4       # erasure data directories
rm -f  /tmp/mc /tmp/minio.pid /tmp/obj1.dat  # client + scratch
rm -f  ./minio                               # the built binary (git-ignored)
git status --porcelain                       # must show ONLY the deliverable (or empty when committed)
```

### 9.10 Troubleshooting

- **`error: externally-managed-environment` (pip, PEP 668):** if setting up a Python audit sink, use `pip install --break-system-packages <pkg>` or a virtualenv.
- **`mc` download fails:** `dl.min.io` 302-redirects to GitHub — use `curl -sL` (follow redirects).
- **Test fails only in a batch run:** MinIO `cmd/` tests share process-global heal state; run each with `-run '^<Test>$' -count=1`.
- **Server won't stop / port in use:** always `kill "$(cat /tmp/minio.pid)"`; never leave the server running. Verify with `lsof -i :9000`.
- **Read-only confirmation:** `git diff <base> --name-status` must show exactly one added path — the deliverable.

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---|---|
| Put Go on PATH | `source /etc/profile.d/golang.sh` |
| Build MinIO | `make build` |
| Version | `./minio --version` |
| Run 4-drive EC:2 server | `./minio server /tmp/d1 /tmp/d2 /tmp/d3 /tmp/d4 --address :9000 --console-address :9001 &` |
| Set mc alias | `/tmp/mc alias set local http://127.0.0.1:9000 minioadmin minioadmin` |
| Confirm EC:2 | `/tmp/mc admin info local` |
| On-demand heal | `/tmp/mc admin heal -r --verbose --json local/<bucket>` |
| Heal trace | `/tmp/mc admin trace --call heal --verbose local` |
| Isolated test | `go test -tags kqueue,dev -run '^<Test>$' ./cmd/ -count=1` |
| Read-only check | `git diff <base> --name-status` |

### B. Port Reference

| Port | Purpose |
|---|---|
| `9000` | MinIO S3 API endpoint (`--address :9000`) |
| `9001` | MinIO Console web endpoint (`--console-address :9001`) — not part of this deliverable's scope |

### C. Key File Locations

| Path | Role |
|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (only repository write) |
| `cmd/erasure-healing.go` | `healObject`/`cannotHeal` (`:428`), `isObjectDangling` (`:968`), `auditHealObject` (`:221`), `healTrace` (`:1090`) |
| `cmd/erasure-object.go` | `deleteIfDangling` purge/refuse (`:482`/`:487`) |
| `cmd/erasure-metadata.go` | `objectQuorumFromMeta` (`:531`) |
| `cmd/erasure-errors.go` | `errErasureReadQuorum` (`:23`) |
| `cmd/storage-errors.go` | `errFileNotFound` (`:71`), `errFileVersionNotFound` (`:74`) |
| `internal/config/storageclass/storage-class.go` | `DefaultParityBlocks` → EC:2 for 4 disks (`:355-367`) |
| `cmd/erasure-healing_test.go` | In-process reproduction templates (`TestIsObjectDangling:40`, etc.) |
| `<disk>/<bucket>/<object>/xl.meta` + `/<dataDir>/part.N` | Backend shard layout for fault injection |

### D. Technology Versions

| Component | Version | Source of truth |
|---|---|---|
| Go toolchain | `go 1.23` (runtime `go1.23.12`) | `go.mod` |
| Reed-Solomon | `github.com/klauspost/reedsolomon v1.12.4` | `go.mod` |
| Admin result types | `github.com/minio/madmin-go/v3 v3.0.77` | `go.mod` |
| HighwayHash (bitrot) | `github.com/minio/highwayhash v1.0.3` | `go.mod` |
| `mc` admin client | `RELEASE.2025-08-13T08-35-41Z` | fetched to `/tmp/mc` |

### E. Environment Variable Reference

| Variable | Value (dev) | Purpose |
|---|---|---|
| `MINIO_ROOT_USER` | `minioadmin` | Server root user (demo/ephemeral only) |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Server root password (demo/ephemeral only) |
| `CGO_ENABLED` | `0` | Static build (matches Makefile) |
| `MINIO_API_REQUESTS_MAX` | `10000` | Raises request cap during isolated test runs |

### F. Developer Tools Guide

| Tool | Use |
|---|---|
| `mc admin heal` | Real on-demand heal entry point; `--verbose` shows per-drive Before/After + color key; `--json` for machine-readable `HealResultItem`. |
| `mc admin trace --call heal` | Live heal trace (`mode=1` HealNormalScan / `mode=2` HealDeepScan). |
| `mc admin info` | Confirms drive count + erasure scheme (EC:2). |
| `xl-meta` (`docs/debugging/xl-meta`) | Decode `xl.meta` to inspect EcM/EcN, dataDir, parts. |
| `go test -run '^<Test>$' -count=1` | Drive the in-process `HealObject` API deterministically (isolation avoids batch flakiness). |

### G. Glossary

| Term | Definition |
|---|---|
| **EC:2** | Erasure-coding scheme with 2 parity blocks; the default for a 4–5 drive set (`DefaultParityBlocks(4)=2`). |
| **dataBlocks / parityBlocks** | For 4-disk EC:2: 2 data + 2 parity shards per object. |
| **readQuorum / writeQuorum** | Minimum shards to read/write; EC:2 → readQuorum 2, writeQuorum 3 (incremented because dataBlocks == parityBlocks). |
| **`cannotHeal`** | Predicate at `erasure-healing.go:428`: `!XLV1 && !Deleted && disksToHealCount > parityBlocks` — separates RECONSTRUCT from PURGE/REFUSE. |
| **`isObjectDangling`** | Classifier (`:968`) deciding if an object is provably garbage (purge) vs non-actionably degraded (refuse). |
| **Dangling object** | An object with damage beyond the parity budget whose surviving errors are *actionable* (files actually missing) → purged (`DeleteDanglingObject`). |
| **`xl.meta`** | Self-describing per-object XL metadata file on each disk. |
| **`part.N`** | A data/parity shard file under the object's `<dataDir>`. |
| **Bitrot** | Silent on-disk corruption; detectable only under `HealDeepScan` (deep) or an explicit heal, not a normal existence check. |
| **HealNormalScan / HealDeepScan** | `mode=1` (existence/metadata) vs `mode=2` (bitrot checksum verification). |

---

*Generated by the Blitzy Platform. Completion is measured strictly against Agent Action Plan scope and standard path-to-production activities. Brand colors: Completed `#5B39F3`, Remaining `#FFFFFF`.*