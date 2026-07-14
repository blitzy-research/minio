# Blitzy Project Guide

> **Deliverable:** `blitzy/documentation/minio_c07e5b49d477.md` — a single, runtime-evidenced technical answer document explaining how MinIO's object-healing subsystem makes its reconstruct-versus-leave decisions on a 4-drive erasure-coded (EC:2) instance.
> **Task type:** Documentation-only Q&A investigation (rule set "SWE-AtlasQnA-Repo"). The MinIO Go source tree was treated as strictly read-only.
> **Branch:** `blitzy-562c7fff-8663-4c77-aefb-b38e8fc07dbe` · **HEAD:** `affa31b3e` · **Base:** `c07e5b49d477`

---

## 1. Executive Summary

### 1.1 Project Overview

This project answers an onboarding/knowledge-transfer question about MinIO's object-healing subsystem: on a 4-drive erasure-coded instance, when an object is in an ambiguous state across drives (some intact, some corrupt, some missing), does healing **always** reconstruct from surviving shards, or are there conditions under which it purges the object (stays deleted) or leaves it degraded? The audience is engineers onboarding to MinIO's erasure/healing internals. The deliverable is a single Markdown document grounded entirely in **observed runtime output** captured from a canonical MinIO server built from source, with every behavioral claim anchored to a specific `file:line` in the read-only source tree. No product code was changed; the sole tracked artifact is the answer document.

### 1.2 Completion Status

The project is **87.6% complete** on an AAP-scoped basis. The autonomous deliverable — the runtime-evidenced answer document — is fully authored, validated, and committed with zero outstanding corrections. The remaining 8.5 hours are human-gated path-to-production activities: subject-matter-expert (SME) technical sign-off, independent reproduction, and knowledge-base publication.

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStroke':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieTitleTextSize':'16px','pieSectionTextSize':'14px'}}}%%
pie showData title Project Completion — 87.6% Complete
    "Completed Work (AI)" : 60
    "Remaining Work" : 8.5
```

| Metric | Hours |
|--------|-------|
| **Total Project Hours** | **68.5** |
| Completed Hours (AI + Manual) | 60.0 |
| Remaining Hours | 8.5 |
| **Percent Complete** | **87.6%** |

**Calculation (PA1, AAP-scoped, hours-based):** Completion % = Completed ÷ (Completed + Remaining) = 60 ÷ (60 + 8.5) = 60 ÷ 68.5 = **87.59% ≈ 87.6%**.

> Color key (Blitzy brand): Completed / AI Work = Dark Blue `#5B39F3`; Remaining / Not Completed = White `#FFFFFF`; Headings / Accents = Violet-Black `#B23AF2`; Highlight = Mint `#A8FDD9`.

### 1.3 Key Accomplishments

- ✅ **Canonical server built & run:** MinIO binary built from a detached `git worktree` at commit `c07e5b49d477` (CGO_ENABLED=0), reproducing the exact banner `DEVELOPMENT.2024-11-25T17-10-22Z` on `go1.23.2 linux/amd64`; stood up as a standalone 4-drive erasure set that auto-selected **EC:2** ("Formatting 1st pool, 1 set(s), 4 drives per set").
- ✅ **Core question answered with evidence:** Demonstrated that healing does **NOT** always reconstruct — three distinct outcomes were reproduced live: **RECONSTRUCT**, **PURGE (stays DELETED)**, and **DEGRADED (unrecoverable)**.
- ✅ **All three canonical trigger paths exercised:** manual `mc admin heal`, inline GET/PUT MRF auto-heal (trace-proven ~1.5s rebuilds), and the background scanner (source-verified `skipHeal` gate on erasure sets).
- ✅ **Boundary conditions captured:** minimum valid shards (≥ `DataBlocks`=2), the unrecoverable-error text (unwrapping `errErasureReadQuorum`), and the WRITE-vs-DELETE dangling divergence (full cross-product).
- ✅ **Every claim grounded:** all `file:line` anchors verified exact against the read-only source; inferred-vs-observed labels applied throughout; each scenario run ≥2×.
- ✅ **Read-only constraint honored:** `git diff c07e5b49d477..HEAD --name-status` = a single added file; `cmd/` and `internal/` byte-identical to base.
- ✅ **Validation:** all 5 autonomous production-readiness gates passed (dependencies, compilation, tests, runtime, cleanliness) with **zero corrections** required; the 5 heal unit tests the document cites all pass on this checkout.

### 1.4 Critical Unresolved Issues

There are **no release-blocking defects** in the deliverable. The items below are quality-gate confirmations appropriate to a knowledge-transfer artifact, not code defects.

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| SME technical sign-off not yet performed | Doc is validated autonomously but not yet human-approved as an authoritative onboarding reference | MinIO / erasure-coding SME | 4.0h |
| Two inferred mechanisms await expert confirmation (quorumETag override §3.3; DEL-B(ii) lone-delete-marker stale-orphan §4.9.2) | Low — both are explicitly labeled "inferred"; confirmation upgrades them to authoritative | SME (part of sign-off) | Included in 4.0h |
| Independent reproduction not yet run by a second engineer | Low — evidence is ephemeral by design; raw output is quoted verbatim in-doc | Reviewing engineer | 3.0h |

### 1.5 Access Issues

**No access issues identified.** All resources required for the autonomous work were available: the MinIO source checkout, the Go 1.23.2 toolchain, the warmed offline module cache (`go mod verify` = "all modules verified"), and the `mc` admin client. The build and 4-drive EC:2 runtime were executed end-to-end on the host with no permission, credential, or network gaps.

| System / Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-------------------|----------------|-------------------|-------------------|-------|
| MinIO source repo (`c07e5b49d477`) | Read | None | ✅ Available (read-only honored) | — |
| Go module cache | Build | None | ✅ Verified offline | — |
| 4-drive EC:2 runtime + `mc` client | Runtime | None | ✅ Ran end-to-end | — |

### 1.6 Recommended Next Steps

1. **[High]** Conduct the SME technical review and sign-off (HT-1, 4.0h) — validate `file:line` anchors against commit `c07e5b49d477` and confirm the two inferred mechanisms.
2. **[Medium]** Independently reproduce the build and 2–3 representative scenarios (HT-2, 3.0h) — D1 RECONSTRUCT, D4 DEGRADED, D5 dangling PURGE — following §9 / doc §7.
3. **[Medium]** Publish to the onboarding knowledge base and verify Mermaid diagrams and wide tables render on the target platform (HT-3, 1.5h).
4. **[Low]** Record a maintenance reminder to re-anchor `file:line` references if the document is later cited against a newer MinIO release (version-drift risk R-T2).

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

Each row traces to AAP-scoped investigation and authoring activity for the documentation deliverable.

| Component | Hours | Description |
|-----------|-------|-------------|
| Environment build & harness setup | 6.0 | Build canonical binary from detached worktree at `c07e5b49d477`; stand up standalone 4-drive EC:2 set; `mc` alias, bucket, baseline object & on-disk layout |
| Decision-path investigation & flowchart | 8.0 | Trace `healObject` → `disksWithAllParts` → `shouldHealObjectOnDisk` → `cannotHeal` gate → `isObjectDangling`; author the decision flowchart with code anchors (doc §3) |
| Per-scenario evidence capture (~11 scenarios × 2 runs) | 12.0 | Corrupt `part.N`, corrupt `xl.meta`, removed dir, user's exact mixed state, zero-byte/truncated, degraded, dangling purge, WRITE-vs-DELETE cross-product — before/during/after (doc §4) |
| Boundary walk (a): 1→2→3 shard loss | 3.0 | Quorum-boundary walk establishing ≥ `DataBlocks`(2) valid shards required; recovery at 2-of-4 loss, failure at 3-of-4 (doc §5.1, §4.5) |
| Trigger coverage + ServiceTrace proofs | 5.0 | Manual `mc admin heal`; inline GET/PUT MRF (trace-proven ~1.5s rebuilds ×2); background-scanner `skipHeal` gate; write-back trace (2 survivors + RenameData) (doc §4.10–§4.11) |
| Decision-visibility analysis (§6) | 4.0 | `HealResultItem` Before/After grid, drive-state vocabulary, client-side color rendering, `healingLogOnceIf` WHY tags, server-computes/client-renders contract (doc §6) |
| Document synthesis & authoring (1866 lines) | 10.0 | Compose the full answer document: verbatim question, executive answer, per-scenario evidence, boundary conditions, build/invocation commands, coverage pass |
| QA remediation (4 rounds, ~42 findings) | 10.0 | Iterative code-review remediation across commits `e619b4d7d` (23), `9002dfc3c` (5), `0bb39cace` (12), `affa31b3e` (2) |
| Cleanup discipline | 2.0 | Guarded, PID-scoped teardown of all ephemeral `/tmp` artifacts and the build worktree; verify `git status` clean, source tree byte-unchanged |
| **Total Completed** | **60.0** | |

### 2.2 Remaining Work Detail

Each category traces to a path-to-production need for the documentation deliverable. All items are human-gated.

| Category | Hours | Priority |
|----------|-------|----------|
| SME technical review & sign-off (HT-1) | 4.0 | High |
| Independent reproduction of build + representative scenarios (HT-2) | 3.0 | Medium |
| Publication / knowledge-base integration (HT-3) | 1.5 | Medium |
| **Total Remaining** | **8.5** | |

### 2.3 Hours Reconciliation

- **Total Project Hours** = Completed (60.0) + Remaining (8.5) = **68.5** ✔ (matches Section 1.2)
- **Section 2.1 total** (60.0) = Completed Hours in Section 1.2 ✔
- **Section 2.2 total** (8.5) = Remaining Hours in Section 1.2 = Section 7 pie "Remaining Work" ✔
- **Completion %** = 60 ÷ 68.5 = **87.6%** ✔ (consistent across Sections 1.2, 7, 8)

---

## 3. Test Results

All results below originate from Blitzy's autonomous validation logs for this project (Final Validator gates + the project's own unit tests run on checkout `c07e5b49d477`). Because this is a documentation-only task with **no product code authored**, code-coverage percentages are not applicable; "tests" here are (a) the project's own heal unit tests that the document cites, and (b) the autonomous validation gates that confirm the deliverable's accuracy.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|-----------|-------|
| Unit — heal decision | Go `testing` | 5 | 5 | 0 | n/a | The exact tests the doc cites, run in isolation on `c07e5b49d477`: `TestIsObjectDangling` (0.252s), `TestHealCorrectQuorum` (0.553s), `TestHealObjectCorruptedParts` (0.467s), `TestHealObjectCorruptedXLMeta` (0.440s), `TestHealingDanglingObject` (0.480s) |
| Compilation / Build | `go build` | 4 | 4 | 0 | n/a | Canonical server binary (exit 0, banner verbatim) + in-tree `xl-meta` & `healing-bin` decoders + 2 harness clients (`healcli`, `s3cli`) |
| Dependency verification | `go mod verify` | 1 | 1 | 0 | n/a | "all modules verified" from the warmed offline cache; doc-cited versions match `go.mod` exactly |
| Doc anchor verification | Custom (autonomous) | All anchors | All | 0 | n/a | Every `file:line` anchor verified exact against read-only source + madmin-go module cache — zero corrections |
| Runtime reproduction | Live 4-drive EC:2 harness | All claims | All | 0 | n/a | Every documented behavior reproduced live through canonical entry points — zero discrepancies |

**Autonomous production-readiness gates (all passed):**

| Gate | Result | Evidence |
|------|--------|----------|
| 1 — Dependencies | ✅ 100% | `go mod verify` OK; versions match `go.mod` (reedsolomon v1.12.4, highwayhash v1.0.3, madmin-go/v3 v3.0.77, minio-go/v7 v7.0.80) |
| 2 — Compilation | ✅ 100% | Server binary built exit 0; `--version` banner reproduced verbatim |
| 3 — Tests | ✅ 100% | 5 cited heal tests pass; all anchors + all runtime claims verified |
| 4 — Runtime | ✅ 100% | 4-drive EC:2 ran; all 3 outcomes + boundaries + WRITE/DELETE divergence + 3 triggers reproduced |
| 5 — Cleanliness | ✅ 100% | `git status` clean; single added file; source tree byte-unchanged |

---

## 4. Runtime Validation & UI Verification

**UI verification: Not applicable.** This project has no web UI in scope — the deliverable is a Markdown document and the only "interface" exercised is the `mc admin heal` CLI/JSON output and server logs. Runtime validation focuses on the healing subsystem behaviors reproduced on the live 4-drive EC:2 harness.

**Server / harness health**
- ✅ **Operational** — Canonical binary `--version` = `DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477…)`, `go1.23.2 linux/amd64`.
- ✅ **Operational** — Standalone 4-drive server formatted a single erasure set → auto **EC:2** (parityBlocks:2, dataBlocks:2).
- ✅ **Operational** — Health probe `GET /minio/health/live` → **HTTP 200**.

**Healing decision outcomes (all reproduced live)**
- ✅ **RECONSTRUCT** — corrupt `part.N` → drive state "missing"; corrupt `xl.meta` → "corrupt"; removed object dir → "missing"; in every case shards were rebuilt from survivors with byte-exact recovery.
- ✅ **PURGE (stays DELETED)** — object judged dangling by `isObjectDangling` [cmd/erasure-healing.go:968] → removed from all drives via the object-heal path; subsequent GET → "Object not found".
- ✅ **DEGRADED (unrecoverable)** — 3 corrupt parts (> ParityBlocks) → grey/read-quorum error, object **not** purged (corruption is non-actionable for the dangling decision).

**Boundary conditions (all reproduced live)**
- ✅ **(a) Minimum valid shards** — recovery succeeds with ≥ `DataBlocks`(2) valid shards; walk 1→2→3 loss confirmed failure at 3-of-4.
- ✅ **(b) Unrecoverable error** — DEGRADED surfaces `Storage resources are insufficient for the read operation …` (unwrapping `errErasureReadQuorum` = "Read failed. Insufficient number of drives online" [cmd/erasure-errors.go:23]); dangling purge surfaces `Object not found`.
- ✅ **(c) WRITE vs DELETE divergence** — normal-object parts branch [cmd/erasure-healing.go:1025-1030] purges on `notFoundPartsErrs > ParityBlocks` (≥3 of 4), independent of metadata quorum; delete-marker branch [:1012-1016] uses strict `notFoundMetaErrs > (len+1)/2` so a 2-of-4 tie **survives**.

**Trigger-path coverage (all reproduced live)**
- ✅ **Operational** — Manual `mc admin heal` (deterministic capture of Before/After grid).
- ✅ **Operational** — Inline MRF GET auto-heal (remove data-dir → GET → rebuild ~1.5s, trace-proven, ×2).
- ✅ **Operational** — Inline MRF partial-WRITE auto-heal (chattr +i → PUT quorum-3 → chattr -i → rebuild ~1.5s, trace-proven, ×2).
- ⚠ **Partial (by design)** — Background scanner is **gated off** for object heals on erasure sets (source-verified `skipHeal` gate); a 1024-object / 200s run showed the scanner active but 0 object heals. Documented as observed + source-grounded.

---

## 5. Compliance & Quality Review

Cross-maps the AAP acceptance criteria (R1–R6 + constraints) to Blitzy's quality benchmarks. All in-scope deliverable requirements are met; path-to-production items are human-gated.

| AAP Requirement | Benchmark | Status | Evidence / Progress |
|-----------------|-----------|--------|---------------------|
| R1 — Decision semantics (3 outcomes) | Behavior demonstrated with runtime evidence | ✅ Pass | Doc §2, §3.1–§3.6, §4.1–§4.9; `healObject`:258, `cannotHeal`:428, `isObjectDangling`:968 exercised live |
| R2 — Output/decision visibility | Before/After grid + WHY logs captured | ✅ Pass | Doc §6.1–§6.5; `HealResultItem` Before/After; `healingLogOnceIf` tags :477/:487/:497 (honestly labeled not-tripped for these inputs) |
| R3 — Boundary (a) min valid shards | Quorum threshold quantified | ✅ Pass | Doc §5.1, §4.5 (1→2→3 walk); write-back reads exactly 2 survivors |
| R4 — Boundary (b) unrecoverable error | Exact error strings captured | ✅ Pass | Doc §5.2, §4.7 (`errErasureReadQuorum` [cmd/erasure-errors.go:23]), §4.8 ("Object not found") |
| R5 — Boundary (c) WRITE vs DELETE | Divergence contrasted via cross-product | ✅ Pass | Doc §5.3, §4.9.2; parts-branch :1030 vs marker-branch :1015-1016 |
| R6 — Methodology constraints | Evidence-first, canonical triggers, grounding, ≥2 runs, cleanup | ✅ Pass | Doc §4.0 (2-run method), §4.10 trace, §4.11 (3 triggers), §7 build/invoke/cleanup |
| Deliverable naming & location | `blitzy/documentation/minio_c07e5b49d477.md` | ✅ Pass | Git-tracked at correct path |
| Read-only source constraint | No source file modified | ✅ Pass | `git diff base..HEAD` = single added file; `cmd/`+`internal/` byte-identical |
| Zero-placeholder / no elision | Full commands & raw output; no `// ...` | ✅ Pass | Full heal JSON, logs, ServiceTrace quoted verbatim; coverage pass §8 |
| Path-to-production: SME sign-off | Human expert approval | ⏳ Pending | HT-1 (4.0h) |
| Path-to-production: reproduction | Independent re-run | ⏳ Pending | HT-2 (3.0h) |
| Path-to-production: publication | KB integration + render check | ⏳ Pending | HT-3 (1.5h) |

**Fixes applied during autonomous validation:** ~42 code-review findings remediated across 4 rounds (commits `e619b4d7d`, `9002dfc3c`, `0bb39cace`, `affa31b3e`). Final validation required **zero** further corrections. **Security review:** no credentials/secrets in captures (heal `clientToken` redacted); zero code or dependencies added; `govulncheck` reported 57 findings across 7 modules historically, **none in the healing path** exercised.

---

## 6. Risk Assessment

Overall risk is **LOW**; there are no high-severity risks. Risks are framed for a documentation deliverable.

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| R-T1 — Inferred (non-observed) mechanisms could be inaccurate | Technical | Low | Low | Explicitly labeled inferred-vs-observed; SME to confirm quorumETag override (§3.3) & DEL-B(ii) stale-orphan (§4.9.2) | Open (resolve at SME review) |
| R-T2 — Version drift: `file:line` anchors pinned to `c07e5b49d477` decay as MinIO evolves | Technical | Medium | Medium | Doc states exact commit + banner `DEVELOPMENT.2024-11-25T17-10-22Z`; re-anchor when cited against newer MinIO | Open (accepted) |
| R-T3 — Documented anomaly (DEL-B(ii) lone-delete-marker stale-orphan) misread as a bug | Technical | Low | Low | Captured as observed behavior with inferred-mechanism label §4.9.2; explicitly not a bug to patch (read-only scope) | Resolved |
| R-S1 — Security exposure in captures | Security | Low (info) | Low | `clientToken` redacted; no credentials/secrets/infra leaked; zero code/deps added | Resolved |
| R-O1 — Reproduction requires Go 1.23.2 + on-disk fault injection | Operational | Low | Low | §9 / doc §7 give exact prerequisites + copy-pasteable build/run/cleanup | Resolved |
| R-O2 — Evidence is ephemeral (harness torn down after capture) | Operational | Low | Low | Raw heal JSON/logs/ServiceTrace quoted verbatim in-doc; §7 enables full re-run | Resolved |
| R-I1 — KB rendering of Mermaid diagrams + wide tables | Integration | Low | Low | Standard GitHub-flavored Markdown + Mermaid; verify render on publish | Open (resolve at publication) |
| R-I2 — `mc` client (RELEASE.2025-08-13) newer than server under test | Integration | Low (info) | Low | Role limited to admin heal API; provenance noted doc §7.3 | Resolved |

**Single highest residual risk:** R-T2 (version drift) — inherent to any code-anchored reference document; mitigated by pinning the exact commit and banner.

---

## 7. Visual Project Status

**Project hours breakdown** (Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`):

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStroke':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieTitleTextSize':'16px','pieSectionTextSize':'14px'}}}%%
pie showData title Project Hours — Completed vs Remaining
    "Completed Work" : 60
    "Remaining Work" : 8.5
```

**Remaining work by category** (sums to 8.5h — matches Section 2.2 and Section 1.2):

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#B23AF2','pie3':'#A8FDD9','pieStrokeWidth':'2px','pieTitleTextSize':'16px','pieSectionTextSize':'14px'}}}%%
pie showData title Remaining Hours by Category (8.5h total)
    "SME review & sign-off (High)" : 4
    "Independent reproduction (Medium)" : 3
    "Publication / KB (Medium)" : 1.5
```

**Integrity check for this section:** the "Remaining Work" slice = **8.5h**, identical to the Remaining Hours in Section 1.2 and the sum of the Section 2.2 "Hours" column. The "Completed Work" slice = **60h**, identical to Section 1.2 Completed Hours and the Section 2.1 total.

---

## 8. Summary & Recommendations

**Achievements.** The autonomous work is essentially complete: the deliverable `blitzy/documentation/minio_c07e5b49d477.md` (1,866 lines) answers every part of the user's question with captured runtime evidence, and every behavioral claim is anchored to a verified `file:line` in the read-only source. The central finding is decisive and reproduced live: **MinIO does not always reconstruct.** Healing resolves an ambiguous object to one of three outcomes — **RECONSTRUCT** (drives needing repair ≤ parity, object not dangling), **PURGE / stays DELETED** (object judged dangling by `isObjectDangling`), or **DEGRADED / unrecoverable** (loss exceeds parity and the state is non-actionable). The subtle, correctly-captured nuance is that **corruption is non-actionable for the dangling decision** (`danglingPartErrsCount`/`danglingMetaErrsCount` count only not-found errors — [cmd/erasure-healing.go:934,950]), so a heavily-corrupted object is left DEGRADED rather than purged. Boundary conditions are quantified (≥ `DataBlocks`=2 valid shards to recover; failure at 3-of-4 loss), the unrecoverable error text is captured verbatim, and the WRITE-vs-DELETE divergence is demonstrated through the full cross-product.

**Remaining gaps (critical path to production).** The outstanding 8.5 hours are entirely human-gated: (1) **SME technical sign-off** (4.0h) — the acceptance gate that also confirms the two labeled-inferred mechanisms; (2) **independent reproduction** (3.0h) of the build and 2–3 representative scenarios; and (3) **publication** (1.5h) to the onboarding knowledge base with a render check. None require code changes.

**Production readiness.** As an artifact, the document is production-ready: it compiles and runs the system it describes, every claim is reproducible and code-anchored, the project's own heal tests pass on the checkout, and the repository is clean with the source tree untouched. Final readiness for publication is contingent only on human SME approval.

**Success metrics.** All 6 AAP requirements (R1–R6) met; all 5 autonomous validation gates passed; zero corrections at final validation; read-only constraint fully preserved.

| Metric | Value |
|--------|-------|
| AAP-scoped completion | **87.6%** |
| AAP requirements met (R1–R6 + constraints) | 8 / 8 in-scope |
| Autonomous validation gates passed | 5 / 5 |
| Corrections required at final validation | 0 |
| Cited heal unit tests passing | 5 / 5 |
| Tracked files changed | 1 (added) |
| Remaining hours (human-gated) | 8.5 |

**Recommendation:** Proceed to SME review (HT-1) as the immediate next action; on sign-off, run the independent reproduction (HT-2) and publish (HT-3). The project can then be considered 100% delivered for its knowledge-transfer purpose.

---

## 9. Development Guide

This guide covers how to access the deliverable and, optionally, reproduce the runtime evidence. Every command below was executed on the host during validation. The source tree is **read-only** — none of these steps modify a tracked file.

### 9.1 System Prerequisites

- **OS:** Linux `amd64` (validated on Ubuntu-family container).
- **Go toolchain:** `go1.23.2` (matches the server banner and `go.mod` `go 1.23`).
- **Git:** `2.51.0` (for `git worktree`).
- **Disk:** ~2 GB free (≈112 MB binary + temporary 4-drive backend + module cache).
- **Build env:** `CGO_ENABLED=0`; `/usr/local/go/bin` on `PATH`.

```bash
go version          # expect: go version go1.23.2 linux/amd64
git --version       # expect: git version 2.51.0
```

### 9.2 Get the Source & Access the Deliverable

```bash
# Repository root (destination branch)
cd /tmp/blitzy/minio/blitzy-562c7fff-8663-4c77-aefb-b38e8fc07dbe_cc6bf7

# Confirm the single tracked change vs the base commit
git diff c07e5b49d477..HEAD --name-status
# expect exactly: A  blitzy/documentation/minio_c07e5b49d477.md

# Read the answer document
less blitzy/documentation/minio_c07e5b49d477.md          # or:
git show HEAD:blitzy/documentation/minio_c07e5b49d477.md | head -n 60
```

### 9.3 (Optional) Reproduce the Runtime Evidence

The document is self-contained; reproduction is only needed for independent verification (HT-2). Build the **canonical** binary from a detached worktree so the embedded banner matches exactly (building from the branch tip embeds a non-canonical later date).

```bash
# 1) Verify dependencies from the offline module cache
go mod verify                       # expect: all modules verified

# 2) Detached worktree at the exact commit (keeps the tracked tree read-only)
WT=$(mktemp -d)/src
git worktree add --detach "$WT" c07e5b49d477b0774f23db3b290745aef8c01bd2
cd "$WT"

# 3) Canonical build (banner = DEVELOPMENT.2024-11-25T17-10-22Z)
export CGO_ENABLED=0
LDFLAGS=$(go run buildscripts/gen-ldflags.go)
BIN=$(mktemp -d)/minio-bin
go build -ldflags "$LDFLAGS" -o "$BIN" .        # ~17s, ~112M binary, exit 0

# 4) Confirm the banner
"$BIN" --version
# expect: ... version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477...)
#         Runtime: go1.23.2 linux/amd64
```

**Run a standalone 4-drive EC:2 server** (auto-selects 2 data + 2 parity):

```bash
DRV=$(mktemp -d)
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  "$BIN" server "$DRV"/d{1..4} --address 127.0.0.1:19010 > /tmp/minio.run.log 2>&1 &
MINIO_PID=$!
sleep 3
grep -m1 "Formatting 1st pool" /tmp/minio.run.log
# expect: ... Formatting 1st pool, 1 set(s), 4 drives per set.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:19010/minio/health/live
# expect: 200
```

**Drive an example heal** (using the `mc` admin client; see doc §4/§7 for full fault-injection recipes):

```bash
mc alias set healdemo http://127.0.0.1:19010 minioadmin minioadmin
mc mb healdemo/bkt
echo "hello-heal" | mc pipe healdemo/bkt/obj
# ... inject a fault on one drive's part.1 (corrupt) or remove the object dir (missing) ...
mc admin heal --json healdemo/bkt/obj      # inspect Before/After per-drive state grid
```

### 9.4 Verification Steps

```bash
# Cited heal unit tests pass on this checkout (run from the worktree or repo root)
go test ./cmd/ -run '^TestIsObjectDangling$' -count=1        # ok ... ~0.25s
go test ./cmd/ -run '^TestHealCorrectQuorum$' -count=1       # ok
go test ./cmd/ -run '^TestHealObjectCorruptedParts$' -count=1
go test ./cmd/ -run '^TestHealObjectCorruptedXLMeta$' -count=1
go test ./cmd/ -run '^TestHealingDanglingObject$' -count=1
```

- **Banner:** `--version` prints `DEVELOPMENT.2024-11-25T17-10-22Z` + `go1.23.2`.
- **EC:2 formation:** startup log shows "1 set(s), 4 drives per set".
- **Health:** `GET /minio/health/live` → `200`.

### 9.5 Troubleshooting

- **Wrong banner (non-canonical date):** you built from the branch tip, not the detached commit. Rebuild from the `git worktree add --detach … c07e5b49d477…` checkout (§9.3 step 2).
- **`address already in use`:** port 19010 is taken — choose another `--address` port or free it (`lsof -i :19010`).
- **`go mod verify` / build wants network:** ensure the warmed module cache is present; set `GOFLAGS=-mod=mod` only if needed. The validated build ran fully offline.
- **`govulncheck` warnings:** historical advisories exist across 7 modules; **none are in the healing path** exercised — expected, not a blocker.
- **`/usr/bin/time` missing:** use the shell `time` builtin instead.

### 9.6 Cleanup (leave the tree read-only & clean)

```bash
# Stop the server you started (PID-scoped — never a broad pkill)
kill "$MINIO_PID" 2>/dev/null

# Remove the detached worktree and temporary artifacts
cd /tmp/blitzy/minio/blitzy-562c7fff-8663-4c77-aefb-b38e8fc07dbe_cc6bf7
git worktree remove --force "$WT" && git worktree prune
rm -rf "$DRV" "$(dirname "$BIN")" /tmp/minio.run.log

# Confirm the tree is clean and unchanged
git status --porcelain          # expect: empty
git diff c07e5b49d477..HEAD --name-status   # expect: A blitzy/documentation/minio_c07e5b49d477.md
```

---

## 10. Appendices

### Appendix A — Command Reference

| Purpose | Command |
|---------|---------|
| Verify toolchain | `go version` · `git --version` |
| Show single tracked change | `git diff c07e5b49d477..HEAD --name-status` |
| Read deliverable | `less blitzy/documentation/minio_c07e5b49d477.md` |
| Verify deps (offline) | `go mod verify` |
| Detached worktree at commit | `git worktree add --detach "$WT" c07e5b49d477…` |
| Canonical ldflags | `LDFLAGS=$(go run buildscripts/gen-ldflags.go)` |
| Build server | `go build -ldflags "$LDFLAGS" -o "$BIN" .` |
| Check banner | `"$BIN" --version` |
| Run 4-drive EC:2 | `"$BIN" server "$DRV"/d{1..4} --address 127.0.0.1:19010` |
| Health probe | `curl -s http://127.0.0.1:19010/minio/health/live` |
| Trigger heal | `mc admin heal --json <alias>/<bucket>/<object>` |
| Run a cited unit test | `go test ./cmd/ -run '^TestIsObjectDangling$' -count=1` |
| Remove worktree | `git worktree remove --force "$WT" && git worktree prune` |

### Appendix B — Port Reference

| Port | Service | Notes |
|------|---------|-------|
| 19010 | MinIO S3 API + admin (reproduction harness) | Example only; any free port works via `--address` |
| `/minio/health/live` | Liveness endpoint | Returns HTTP 200 when ready |

### Appendix C — Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (1,866 lines) |
| `cmd/erasure-healing.go` | Heal worker & all decision branches (`healObject`:258, `cannotHeal`:428, `isObjectDangling`:968, delete-marker :1012-1016, normal-object :1025-1030, `danglingMetaErrsCount`:934, `danglingPartErrsCount`:950, `healingLogOnceIf` tags :477/:487/:497, `shouldHealObjectOnDisk`:156) |
| `cmd/erasure-healing-common.go` | `listOnlineDisks`, `disksWithAllParts` |
| `cmd/erasure-metadata.go` | `objectQuorumFromMeta`, `pickValidFileInfo` |
| `cmd/erasure-errors.go` | `errErasureReadQuorum` (:23) |
| `cmd/erasure-decode.go` | `Erasure.Decode`, `Erasure.Heal` (reconstruction) |
| `internal/config/storageclass/storage-class.go` | `DefaultParityBlocks` (:361-362 → 4 drives = EC:2) |
| `cmd/global-heal.go`, `cmd/mrf.go`, `cmd/admin-handlers.go` | Three canonical heal triggers |
| `cmd/erasure-healing_test.go` | The 5 cited heal unit tests |
| `buildscripts/gen-ldflags.go` | Emits canonical version ldflags |

### Appendix D — Technology Versions

| Component | Version | Source |
|-----------|---------|--------|
| Go toolchain | go1.23.2 (module `go 1.23`) | `go.mod:3` |
| `github.com/klauspost/reedsolomon` | v1.12.4 | `go.mod` |
| `github.com/minio/highwayhash` | v1.0.3 | `go.mod` |
| `github.com/minio/madmin-go/v3` | v3.0.77 | `go.mod` |
| `github.com/minio/minio-go/v7` | v7.0.80 | `go.mod` |
| `github.com/klauspost/compress` | v1.17.11 | `go.mod` |
| MinIO server (under test) | `DEVELOPMENT.2024-11-25T17-10-22Z` (commit `c07e5b49d477…`) | build banner |
| `mc` client | RELEASE.2025-08-13T08-35-41Z | operator tool (admin heal API only) |
| Git | 2.51.0 | host |

### Appendix E — Environment Variable Reference

| Variable | Value (reproduction) | Purpose |
|----------|----------------------|---------|
| `CGO_ENABLED` | `0` | Pure-Go static build |
| `LDFLAGS` | `$(go run buildscripts/gen-ldflags.go)` | Embeds canonical version banner |
| `MINIO_ROOT_USER` | `minioadmin` | Harness root access key |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Harness root secret key |

### Appendix F — Developer Tools Guide

- **`mc admin heal [--json|--verbose|--scan=deep]`** — triggers a heal and renders the `HealResultItem` Before/After per-drive state grid; `--json` gives deterministic, machine-readable output for capturing evidence.
- **`docs/debugging/healing-bin`** — decodes `xl.meta` / healing binary for on-disk state inspection (before/during/after).
- **`xl-meta`** (in-tree) — decodes an object's `xl.meta` to inspect erasure metadata and parts.
- **ServiceTrace** (`mc admin trace`) — used to prove write-back (exactly 2 `ReadFileStream` survivors + `RenameData` + `heal.Object`) and MRF rebuild timing (~1.5s).

### Appendix G — Glossary

| Term | Meaning |
|------|---------|
| **EC:2** | Erasure coding with 2 parity blocks; for a 4-drive set = 2 data + 2 parity |
| **DataBlocks / ParityBlocks** | Number of data / parity shards; read quorum = DataBlocks (2), write quorum = 3 for EC:2 |
| **Dangling object** | An object whose surviving metadata/parts fall below the dangling threshold; classified by `isObjectDangling` and purged rather than reconstructed |
| **RECONSTRUCT / PURGE / DEGRADED** | The three healing outcomes: rebuild from survivors / delete as dangling / leave unrecoverable |
| **MRF** | Most-Recent-Failures — inline auto-heal queue populated on GET/PUT partial failures |
| **`cannotHeal` gate** | `disksToHealCount > ParityBlocks` guard [cmd/erasure-healing.go:428] that routes an object away from reconstruction |
| **`HealResultItem`** | madmin-go struct carrying Before/After per-drive states surfaced by `mc admin heal` |
| **Non-actionable corruption** | Bit-rot/corrupt errors are not counted by the dangling classifier (only not-found errors are), so corrupted objects degrade rather than purge |

---

*Generated by the Blitzy Platform. Completion figures are AAP-scoped (PA1 methodology): 60.0h completed + 8.5h remaining = 68.5h total = 87.6% complete. Colors: Completed `#5B39F3`, Remaining `#FFFFFF`.*