# Blitzy Project Guide — MinIO Healing: Reconstruct vs. Purge Investigation (4-Disk EC:2)

> **Brand color legend** — Completed / AI Work: **Dark Blue `#5B39F3`** · Remaining / Not Completed: **White `#FFFFFF`** · Headings / Accents: **Violet-Black `#B23AF2`** · Highlight: **Mint `#A8FDD9`**

---

## 1. Executive Summary

### 1.1 Project Overview

This project is a **read-only, code-grounded onboarding investigation** into MinIO's object-healing subsystem. Its objective is a single runtime-evidence-backed document explaining how MinIO decides — on a single-node **4-disk erasure-coded (EC:2 = 2 data + 2 parity)** instance — whether to **reconstruct** an object from surviving shards, **purge** it as "dangling," or **leave it degraded** when shards conflict across drives. The audience is engineers onboarding onto MinIO storage internals. The technical scope is MinIO's `cmd/` healing path (`healObject`, `isObjectDangling`, `deleteIfDangling`) and the `madmin-go/v3` heal types. The sole persistent output is one Markdown file; no production source is changed.

### 1.2 Completion Status

```mermaid
%%{init: {'theme':'base','themeVariables':{'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieSectionTextColor':'#B23AF2','pieTitleTextColor':'#5B39F3','pieLegendTextColor':'#B23AF2'}}}%%
pie showData title MinIO Healing Investigation — 91.3% Complete
    "Completed Work (AI)" : 42
    "Remaining Work" : 4
```

| Metric | Hours |
|--------|------:|
| **Total Project Hours** | **46** |
| Completed Hours (AI + Manual) | 42 (AI 42 + Manual 0) |
| Remaining Hours | 4 |
| **Percent Complete** | **91.3%** |

> Completion is computed with the AAP-scoped hours methodology: `42 completed / (42 completed + 4 remaining) = 42/46 = 91.3%`. 100% of AAP-*specified* deliverables are complete and validated; the remaining 4 hours are inherently-human path-to-production gates.

### 1.3 Key Accomplishments

- ✅ **Deliverable authored & committed** — `blitzy/documentation/minio_c07e5b49d477.md` (535 lines), answering all eight questions (Q1–Q8) with an explicit **Answer** plus verbatim runtime evidence.
- ✅ **Run-first methodology executed** — MinIO built from source (`make build`, Go 1.23.2), a 4-drive EC:2 server run, and **5 fault-injection scenarios** exercised (truncate, corrupt-meta, 2-damaged, 3-damaged-purge, delete-marker).
- ✅ **Both outcomes proven** — reconstruction (`objects_healed:1`) *and* purge-as-dangling (`objects_healed:0`, HTTP 503 `SlowDownRead`), substantiating the thesis that MinIO does **not** always reconstruct.
- ✅ **~42 exact `file:line` citations** verified 100% accurate across 14+ source files; `madmin-go/v3@v3.0.77` drive-state literals re-confirmed against the container module cache.
- ✅ **19/19 healing unit tests pass**; `make build` and full-tree compile (868 files) succeed; `go mod verify` clean.
- ✅ **Repository integrity preserved** — `git diff` against base shows exactly one new file; working tree clean; all ephemeral `/tmp` harness removed.
- ✅ **Honest caveats included** — the out-of-scope `joinErrs` source bug, per-instance value drift, and source-verified-vs-log-observed distinctions are all disclosed rather than glossed over.

### 1.4 Critical Unresolved Issues

**No release-blocking issues identified.** The items below are documented, non-blocking, and correctly out of scope for this read-only task.

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| Upstream `joinErrs` bug (`cmd/erasure-object.go:469`) — audit `merrs` tag always `""` | Non-blocking; **out of scope** to fix (read-only mandate). Observability gap in `DeleteDanglingObject` audit only. Already documented in the deliverable (Q5 caveat). | MinIO upstream (optional follow-up) | N/A — not part of this deliverable |
| Binary/commit-id drift (`c07e5b49d477` in doc vs `8c494261a` on fresh build) | Non-blocking; behavior identical (HEAD advanced by 2 doc-only commits). | Human reviewer (awareness only) | N/A |

### 1.5 Access Issues

**No access issues identified.** All build, run, fault-injection, and evidence-capture steps executed successfully inside the designated container. No external repository permissions, service credentials, or third-party API access were required.

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-----------------|----------------|-------------------|-------------------|-------|
| — | — | No access issues identified | N/A | — |

### 1.6 Recommended Next Steps

1. **[High]** SME technical review & sign-off of the answer's accuracy for onboarding use (~2h).
2. **[Medium]** Reproduction spot-check on a fresh 4-drive EC:2 instance per the deliverable's "Reproducing these results" runbook (~1.5h).
3. **[Medium]** PR review & merge of the single-file change to the target branch (~0.5h).
4. **[Low · optional · out-of-scope]** File an upstream MinIO issue/PR for the documented `joinErrs` bug (`cmd/erasure-object.go:469`). Tracked separately; **not** counted in this project's hours.

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|------:|-------------|
| Build & environment setup | 4 | `make build` (Go 1.23.2) → `./minio`; obtain `mc` client; start single-node 4-drive EC:2 topology; configure `MINIO_AUDIT_WEBHOOK` sink; create bucket/alias/objects. Maps AAP H1+H2. |
| Fault-injection harness (5 scenarios) | 6 | Scripts manipulating backend `xl.meta`/`part.N`: truncate, corrupt-meta, 2-damaged (boundary), 3-damaged (purge), delete-marker. Maps AAP H3. |
| Runtime evidence capture & code correlation | 5 | Capture `HealResultItem` JSON, heal summaries, `DeleteDanglingObject` audit, S3 503 XML, before/after drive-state arrays; correlate each to the governing code path. Maps AAP H4. |
| Code archaeology & citation grounding | 7 | ~42 exact `file:line` citations across 14+ source files (>15,000 LOC) + `madmin-go/v3@v3.0.77` literal re-confirmation vs module cache. Maps AAP C1+C2. |
| Authoring the answer document (Q1–Q8) | 14 | 535-line document: explicit answers, verbatim evidence, decision-logic flowchart, honest caveats, coverage checklist, reproduction runbook. Maps AAP A1–A3, G1–G3. |
| Cleanup & repository-integrity verification | 2 | Remove all `/tmp` ephemeral harness; confirm `git status` clean and byte-for-byte unchanged. Maps AAP R1–R3. |
| Remediation of 8 code-review findings | 4 | Commit `8c494261a`: raw-struct-vs-`mc`-output labeling, tightened citations, added caveats. |
| **Total Completed** | **42** | **Matches Completed Hours in §1.2.** |

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|----------|------:|----------|
| SME technical review & sign-off of answer accuracy | 2.0 | High |
| Reproduction spot-check on a fresh 4-drive EC:2 instance (AAP §0.9.4) | 1.5 | Medium |
| PR review & merge to target branch | 0.5 | Medium |
| **Total Remaining** | **4.0** | — |

> **Integrity:** §2.1 (42) + §2.2 (4) = **46** = Total Project Hours (§1.2). §2.2 total (4) = §1.2 Remaining (4) = §7 pie "Remaining Work" (4).

### 2.3 Basis of Estimate

Hours reflect the effort a senior engineer unfamiliar with MinIO internals would invest to reproduce this deliverable to the same quality: building/running a distributed storage engine, designing backend fault-injection, tracing decision logic across a large codebase, and authoring a verbatim-grounded technical document. Confidence: **High** for the completed items (backed by commits, validator logs, and independent spot-checks) and **High** for the remaining items (standard, well-understood human review/merge gates).

---

## 3. Test Results

All entries originate from Blitzy's autonomous validation logs for this project (Final Validator Gates 1–3).

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|------------:|-------:|-------:|-----------:|-------|
| Unit — Healing subsystem | Go `testing` (tags `kqueue,dev`) | 19 | 19 | 0 | Not measured | `ok github.com/minio/minio/cmd 7.646s`. Includes `TestIsObjectDangling` (13 subtests), `TestHealing`, `TestHealingVersioned`, `TestHealingDanglingObject`, `TestHealCorrectQuorum`, `TestHealObjectCorrupted{Pools,XLMeta,Parts}`, `TestHealObjectErasure`, `TestHealEmptyDirectoryErasure`, `TestHealLastDataShard` (8 size subtests), `TestErasureHeal`, `TestDisksWithAllParts`. |
| Runtime Scenario Validation | `mc admin heal` fault-injection harness (Gate 2) | 5 | 5 | 0 | n/a | 5 scenarios (truncate / corrupt-meta / 2-damaged / 3-damaged-purge / delete-marker); each output matched the deliverable **byte-for-byte** except per-instance values (deployment/request/host IDs, timestamps, UUIDs, modtime). |
| **Totals** | — | **24** | **24** | **0** | — | 0 failures across all autonomous test execution. |

**Additional autonomous checks (Gate 3, not unit tests):** `make build` → exit 0 (`./minio`, 117 MB); `CGO_ENABLED=0 go build -tags kqueue ./...` across all 868 files → exit 0, zero warnings; `go mod verify` → "all modules verified".

> **Coverage note (honest):** A coverage percentage was **not measured** — this is a read-only documentation task, not a coverage-driven feature build. The subsystem's existing unit tests were executed as a **regression guard** to confirm the code the document describes still behaves as cited. The source is byte-for-byte unchanged (zero regression risk). A first isolated run hit a documented `globalBytePoolCap` **test-ordering** panic ("integer divide by zero"), resolved by sequencing a `prepareErasure16` test first — **not** a source regression.

---

## 4. Runtime Validation & UI Verification

**UI verification: Not applicable.** MinIO is a backend Go storage engine; the AAP (§0.10) confirms no design frames, URLs, or UI component library are in scope. No browser/UI surface exists for this deliverable.

**Runtime health & scenario outcomes (all autonomously executed):**

- ✅ **Server boot** — single-node 4-drive erasure set starts cleanly: server log `"Formatting 1st pool, 1 set(s), 4 drives per set."`; `mc admin info` reports `"4 drives online, 0 drives offline, EC:2"`.
- ✅ **Backend layout** — a 5 MiB object stored as `xl.meta` (368 B) + `part.1` (2,621,600 B) on all 4 drives under one shared datadir UUID; confirms EC:2 (`DefaultParityBlocks(4) == 2`).
- ✅ **Scenario A (Q1) — truncate one part** → reconstruct; `objects_healed:1`; truncated part surfaces as drive-state `missing`.
- ✅ **Scenario A2 (Q3) — corrupt one `xl.meta`** → reconstruct; drive-state `corrupt` → `ok`.
- ✅ **Q4 — before/after indicators** → `{"before":["missing","ok","ok","ok"],"after":["ok","ok","ok","ok"]}` (verbatim).
- ✅ **Scenario B (Q6) — 2 damaged drives (= parity)** → still heals (boundary confirmed).
- ⚠ **Scenario Q7 — 3 drives lose parts (> parity)** → **leave-degraded** on read: HTTP `503 SlowDownRead` (verbatim XML), then heal **purges** as dangling (`objects_healed:0`; object gone from all 4 drives). *(⚠ = intended degraded/purge outcome, not a defect.)*
- ✅ **Q5 — purge rationale** → `DeleteDanglingObject` audit record captured at the webhook sink with tags `d:p="2:2"`, `derrs="map[0:[4 4 4 1]]"`, `caller=".../erasure-healing.go:438"`, `sz="3000000"`.
- ✅ **Scenario Q8 — remove delete-marker `xl.meta` on 2 drives** → both versions reconstruct (`objects_healed:2`); partial DELETE "rolled forward" (contrast with Q7's purge).

---

## 5. Compliance & Quality Review

Cross-mapping the governing rule **"SWE-AtlasQnA-Repo"** and Blitzy quality benchmarks to observed evidence.

| Benchmark / Rule Directive | Requirement | Status | Evidence |
|----------------------------|-------------|:------:|----------|
| Deliverable location & name | `blitzy/documentation/minio_c07e5b49d477.md` | ✅ Pass | File committed, 535 lines, correct path & branch-derived name. |
| Run-first methodology | Build + run before writing; capture real output | ✅ Pass | Gate 2: server built + run; 5 scenarios executed. |
| Verbatim output | Quote actual logs/values/HTTP/errors | ✅ Pass | Audit JSON, 503 XML, before/after arrays, heal summaries — all verbatim. |
| Coverage | Answer every sub-question + coverage pass | ✅ Pass | Q1–Q8 each with **Answer**; coverage checklist — all 8 boxes checked. |
| Exactness / grounding | Exact literals with `file:line` | ✅ Pass | ~42 citations 100% accurate (Gate 4 + independent spot-checks). |
| Read-only scope | No source modified; no code added | ✅ Pass | `git diff` = 1 new file only; clean tree; zero source changes. |
| Mandatory cleanup | Remove ephemeral artifacts | ✅ Pass | `git status --porcelain` empty; `/tmp` harness removed. |
| Literal re-confirmation | `madmin-go/v3` vs module cache | ✅ Pass | 9 `DriveState*` literals + `HealDriveInfo` + `HealResultItem` re-confirmed (`heal-commands.go`). |
| Honest flagging | Flag unverifiable claims | ✅ Pass | `joinErrs` bug, source-vs-observed, delete-marker code-reading caveats disclosed. |
| Compilation | Full tree builds | ✅ Pass | `make build` exit 0; 868-file compile exit 0. |
| Regression tests | Healing tests pass | ✅ Pass | 19/19 pass. |
| Module integrity | `go mod verify` | ✅ Pass | "all modules verified". |

**Fixes applied during autonomous validation:** the remediation commit (`8c494261a`) resolved 8 code-review findings (raw-struct-vs-`mc`-output labeling, citation tightening, added caveats). **Outstanding items:** none in-scope. Progress: **12/12 benchmarks Pass (100%)**.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Per-instance runtime value drift (deployment/request/host IDs, timestamps, UUIDs, modtime differ on fresh runs) | Technical | Low | High | Doc explicitly labels these as per-instance/per-request; compare outcomes, not literals. | Mitigated (documented) |
| Binary/commit-id drift (`c07e5b49d477` vs `8c494261a`) | Technical | Low | Medium | HEAD advanced by doc-only commits; behavior verified identical. | Mitigated |
| Healing test-ordering panic ("integer divide by zero") when run in isolation | Technical | Low | Medium | Sequence a `prepareErasure16` test first; documented in Dev Guide troubleshooting. Not a source regression. | Mitigated (documented) |
| Ephemeral local credentials / KMS env in harness | Security | Low | Low | Process-local only; never persisted; no secrets in repo or doc; `git status` clean. | Mitigated |
| Upstream `joinErrs` bug → `merrs` audit tag always `""` | Operational | Low-Medium | High | Documented with exact `file:line` + empirical proof; **out of scope** to fix (read-only). Recommend upstream fix. | Open (out-of-scope) |
| Purge "why" audit only emitted when an audit target is configured | Operational | Low | Medium | Documented; recommend configuring an audit webhook in production. | Documented |
| `madmin-go/v3` version coupling (literals/lines pinned to v3.0.77) | Integration | Low | Low | Re-confirmed vs module cache; version pinned in `go.mod`/`go.sum`. | Mitigated |
| `mc` client rendering artifact ("Invalid parity shard count…") for purged objects | Integration | Low | Medium | Documented as a client-side artifact (not a server error); authoritative signal is `objects_healed:0` + audit. | Documented |

> **Overall risk posture: LOW.** A read-only investigation with zero source changes carries no high, critical, or release-blocking risk. The single genuinely-open item is the out-of-scope upstream `joinErrs` bug.

---

## 7. Visual Project Status

**Project hours breakdown** (Completed = Dark Blue `#5B39F3`; Remaining = White `#FFFFFF`):

```mermaid
%%{init: {'theme':'base','themeVariables':{'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeWidth':'2px','pieSectionTextColor':'#B23AF2','pieTitleTextColor':'#5B39F3','pieLegendTextColor':'#B23AF2'}}}%%
pie showData title Project Hours — Completed vs Remaining
    "Completed Work" : 42
    "Remaining Work" : 4
```

**Remaining hours by task** (from §2.2, sums to 4h):

```mermaid
xychart-beta
    title "Remaining Hours by Task"
    x-axis ["SME Review", "Reproduction Spot-check", "PR Review & Merge"]
    y-axis "Hours" 0 --> 3
    bar [2.0, 1.5, 0.5]
```

**Priority distribution of remaining work:** High = 2.0h (50%) · Medium = 2.0h (50%) · Low = 0h (in-scope).

> **Integrity:** the pie "Remaining Work" value (4) equals §1.2 Remaining Hours (4) and the §2.2 Hours total (4); "Completed Work" (42) equals §1.2 Completed Hours (42).

---

## 8. Summary & Recommendations

**Achievements.** The project delivers a rigorous, runtime-evidence-backed onboarding document that answers all eight posed questions and substantiates its central thesis: **MinIO does not always reconstruct.** It demonstrates all three outcomes — reconstruct, purge-as-dangling, and leave-degraded — each with verbatim runtime output and exact `file:line` grounding. Autonomous validation confirms 19/19 healing tests pass, the tree compiles cleanly, and the repository is byte-for-byte unchanged apart from the single deliverable.

**Remaining gaps.** None are technical defects. The **4 remaining hours** are inherently-human path-to-production gates: an SME accuracy sign-off, an optional reproduction spot-check, and the PR merge.

**Critical path to production.** SME review (2h) → reproduction spot-check (1.5h) → merge (0.5h). No blockers.

**Production readiness.** The deliverable is **production-ready as-is** per autonomous validation: every citation accurate, every runtime value reproduced, complete Q1–Q8 coverage, and honest caveats — independently corroborated by the source's own passing unit tests (e.g., `TestIsObjectDangling`).

**Success metrics.** 8/8 questions answered · ~42/42 citations verified · 19/19 tests pass · 5/5 runtime scenarios reproduced · 1/1 file delivered with clean integrity.

**Overall completion: 91.3%** (`42 / 46` AAP-scoped hours). The 8.7% remainder is human review and merge, not autonomous work left undone.

---

## 9. Development Guide

### 9.1 System Prerequisites

- **OS:** Linux x86_64 (validated on the designated container).
- **Go:** `1.23.2` (`go version` → `go version go1.23.2 linux/amd64`). CI pins `1.23.x`.
- **Tooling:** GNU `make`, `git` + `git-lfs` (3.7.1), `python3` (for the audit webhook sink), `curl`.
- **Client:** `mc` (MinIO client) — present at `/usr/local/bin/mc` (`mc version DEVELOPMENT.GOGET`).
- **Disk:** ~1 GB free (117 MB binary + four drive directories).

### 9.2 Environment Setup

```bash
# From the repository root (module github.com/minio/minio)
go mod verify        # expect: "all modules verified"

# Runtime env (mirrors go-healing.yml; process-local only, nothing persisted)
export MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1
export MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="  # local demo key
export MINIO_AUDIT_WEBHOOK_ENABLE_sink=on
export MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=http://127.0.0.1:9999   # required for the Q5 purge "why" audit
```

### 9.3 Build

```bash
make build
# → CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "..." -o ./minio
# Produces ./minio (~117 MB). -trimpath makes runtime.Caller paths module-relative
# (this is why the Q5 audit caller reads "github.com/minio/minio/cmd/erasure-healing.go:438").

./minio --version   # sanity check
```

### 9.4 Start the 4-Drive EC:2 Server (+ audit sink)

```bash
# Tiny webhook receiver that logs POST bodies (captures the DeleteDanglingObject "why" record):
cat > /tmp/audit_sink.py <<'PY'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get('Content-Length',0)); b=self.rfile.read(n)
        open('/tmp/audit_events.log','ab').write(b+b"\n"); self.send_response(200); self.end_headers()
    def log_message(self,*a): pass
http.server.HTTPServer(('127.0.0.1',9999),H).serve_forever()
PY
python3 /tmp/audit_sink.py &

./minio server /tmp/healdata1 /tmp/healdata2 /tmp/healdata3 /tmp/healdata4 \
  --address 127.0.0.1:9100 --console-address 127.0.0.1:9101 &

export MC_HOST_local="http://minioadmin:minioadmin@127.0.0.1:9100"
```

### 9.5 Verification

```bash
mc admin info local          # expect: "4 drives online, 0 drives offline, EC:2"
mc mb local/healbucket
head -c 5242880 /dev/urandom > /tmp/obj1.bin
mc cp /tmp/obj1.bin local/healbucket/obj1
ls /tmp/healdata1/healbucket/obj1/    # note the datadir UUID directory
```

### 9.6 Example Usage — Observe Reconstruct vs. Purge

```bash
# --- Reconstruct: truncate one shard, then heal ---
DD=$(ls /tmp/healdata1/healbucket/obj1/)          # per-object datadir UUID
truncate -s 1000 /tmp/healdata1/healbucket/obj1/$DD/part.1
mc admin heal -r --json local/healbucket/obj1 \
  | python3 -c 'import sys,json;[print(json.dumps({"before":[d["state"] for d in o["before"]["drives"]],"after":[d["state"] for d in o["after"]["drives"]]})) for o in (json.loads(l) for l in sys.stdin) if o.get("type")=="object"]'
# expect: {"before": ["missing","ok","ok","ok"], "after": ["ok","ok","ok","ok"]}  (summary objects_healed:1)

# --- Purge / leave-degraded: destroy parts on 3 of 4 drives (> parity) ---
head -c 3000000 /dev/urandom > /tmp/obj2.bin; mc cp /tmp/obj2.bin local/healbucket/obj2
D2=$(ls /tmp/healdata1/healbucket/obj2/)
rm -rf /tmp/healdata1/healbucket/obj2/$D2 /tmp/healdata2/healbucket/obj2/$D2 /tmp/healdata3/healbucket/obj2/$D2
URL=$(mc share download --expire 5m local/healbucket/obj2 | grep -oP 'Share: \K.*'); curl -sS -D- "$URL" | head -1
# expect: HTTP/1.1 503 Service Unavailable  (SlowDownRead)
mc admin heal -r --json local/healbucket/obj2 | tail -1   # expect summary objects_healed:0 (purged)
grep '"event":"DeleteDanglingObject"' /tmp/audit_events.log   # the "why" record
```

### 9.7 Run the Healing Unit Tests (regression guard)

```bash
CGO_ENABLED=0 go test -tags kqueue,dev ./cmd/ -timeout 300s -v \
  -run 'TestApplyNewerNoncurrentVersionsLimit|TestIsObjectDangling|TestHealing|TestHealingVersioned|TestHealingDanglingObject|TestHealCorrectQuorum|TestHealObjectCorrupted|TestHealObjectErasure|TestErasureHeal|TestDisksWithAllParts|TestHealLastDataShard|TestHealEmptyDirectoryErasure'
# The leading TestApplyNewerNoncurrentVersionsLimit initializes globalBytePoolCap first (avoids the divide-by-zero panic).
```

### 9.8 Cleanup (leave the repo unchanged)

```bash
kill %1 %2 2>/dev/null                 # stop minio + audit sink you started
rm -rf /tmp/healdata* /tmp/audit_sink.py /tmp/audit_events.log /tmp/obj1.bin /tmp/obj2.bin
git status --porcelain                 # expect: empty (working tree clean)
```

### 9.9 Troubleshooting

- **`integer divide by zero` in healing tests** — `globalBytePoolCap` uninitialized when tests run in isolation (`internal/ringbuffer/ring_buffer.go:220`). Include a `prepareErasure16` test (e.g. `TestApplyNewerNoncurrentVersionsLimit`) first in `-run`. Not a source bug.
- **Q5 audit record missing** — no audit target configured. Export `MINIO_AUDIT_WEBHOOK_ENABLE_sink=on` / `MINIO_AUDIT_WEBHOOK_ENDPOINT_sink=...` (`auditDanglingObjectDeletion` early-returns if `len(AuditTargets)==0`).
- **`mc` shows "Invalid parity shard count/surplus shard count"** for a purged object — client-side `mc` rendering artifact, **not** a server error. Trust `objects_healed:0` + the `DeleteDanglingObject` audit.
- **Values differ from the document** — deployment/request/host IDs, timestamps, datadir UUIDs, and modtime are per-instance; compare **outcomes**, not literals.
- **`./minio --version` shows a different commit-id** — expected; HEAD advanced by doc-only commits; behavior is identical.

---

## 10. Appendices

### Appendix A — Command Reference

| Command | Purpose |
|---------|---------|
| `make build` | Build `./minio` (`CGO_ENABLED=0 go build -tags kqueue -trimpath`). |
| `go mod verify` | Verify module integrity ("all modules verified"). |
| `./minio server /tmp/healdata{1..4} --address 127.0.0.1:9100` | Start single-node 4-drive EC:2 server. |
| `mc admin info local` | Confirm topology ("4 drives online, 0 drives offline, EC:2"). |
| `mc admin heal -r --json local/<bucket>/<obj>` | Heal an object; emit `HealResultItem` JSON + summary. |
| `mc share download --expire 5m local/<bucket>/<obj>` | Presigned GET URL (for the 503 evidence). |
| `go test -tags kqueue,dev ./cmd/ -run '...'` | Run healing unit tests. |
| `git diff c07e5b49d477 --name-status` | Confirm exactly one new file. |

### Appendix B — Port Reference

| Port | Service |
|-----:|---------|
| 9100 | MinIO S3 API (`--address`) |
| 9101 | MinIO Console (`--console-address`) |
| 9999 | Audit webhook sink (local `python3` receiver) |

### Appendix C — Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (535 lines). |
| `cmd/erasure-healing.go` | `healObject` (258), `shouldHealObjectOnDisk` (156), drive-state switch (382-393), `cannotHeal` (428), `isObjectDangling` (968). |
| `cmd/erasure-object.go` | `deleteIfDangling` (482), audit helper (451/457/531), `joinErrs` bug (469). |
| `cmd/erasure-metadata.go` | `objectQuorumFromMeta` (531), `InsufficientReadQuorum` (552). |
| `cmd/erasure-errors.go` | `errErasureReadQuorum` (23). |
| `cmd/api-errors.go` | `errErasureReadQuorum → ErrSlowDownRead` (2190-2191); `SlowDownRead` 503 (869-873). |
| `cmd/admin-heal-ops.go` | `healStatusSummary` (40-43), `Items []madmin.HealResultItem` (87). |
| `internal/config/storageclass/storage-class.go` | `DefaultParityBlocks(4) == 2` (355; case 4,5 → 2 at 361-362). |
| `buildscripts/verify-healing.sh` | Reference fault-injection + heal-verification template. |

### Appendix D — Technology Versions

| Component | Version |
|-----------|---------|
| Go toolchain | 1.23.2 (module declares `go 1.23`; CI pins `1.23.x`) |
| MinIO (doc) | `DEVELOPMENT.2024-11-25T17-10-22Z` (commit `c07e5b49d477`) |
| MinIO (fresh build) | `DEVELOPMENT.2026-07-01…` (commit `8c494261a`; doc-only commits ahead) |
| `mc` client | `DEVELOPMENT.GOGET` |
| `github.com/minio/madmin-go/v3` | v3.0.77 |
| `github.com/klauspost/reedsolomon` | v1.12.4 |
| `github.com/minio/highwayhash` | v1.0.3 |
| `git-lfs` | 3.7.1 |

### Appendix E — Environment Variable Reference

| Variable | Value (example) | Purpose |
|----------|-----------------|---------|
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | `minioadmin` / `minioadmin` | Server root credentials (local, ephemeral). |
| `MINIO_CI_CD` | `1` | CI-mode server startup. |
| `MINIO_KMS_SECRET_KEY` | `my-minio-key:…` | Local demo KMS key mirroring `go-healing.yml`. |
| `MINIO_AUDIT_WEBHOOK_ENABLE_sink` | `on` | Enable the audit webhook target. |
| `MINIO_AUDIT_WEBHOOK_ENDPOINT_sink` | `http://127.0.0.1:9999` | Sink endpoint for the purge "why" audit record. |
| `MC_HOST_local` | `http://minioadmin:minioadmin@127.0.0.1:9100` | `mc` alias for the running server. |

### Appendix F — Developer Tools Guide

- **`mc admin heal -r --json`** — drives healing and emits per-object `HealResultItem` JSON plus a summary line (`objects_scanned`, `objects_healed`, …). Primary evidence source for Q1–Q4, Q6, Q8.
- **`xl-meta`** — dumps backend `xl.meta` (versions, `EcM`/`EcN`, data dirs, parts). Used to confirm delete-marker (`Type: 2`, no parts) vs. PUT (`Type: 1`, EC:2) structure for Q8.
- **Audit webhook sink (`python3`)** — captures the `DeleteDanglingObject` audit record that explains the purge decision (Q5).
- **`go test -tags kqueue,dev`** — runs the healing unit tests that independently corroborate the document's claims.

### Appendix G — Glossary

| Term | Meaning |
|------|---------|
| **EC:2** | Erasure coding with 2 data + 2 parity blocks (the 4-disk default via `DefaultParityBlocks(4)`). |
| **Dangling object** | An object with too few surviving shards to meet data-block quorum; purged rather than reconstructed. |
| **`isObjectDangling`** | Predicate deciding purge-vs-heal: dangling iff `total disks − (corrupted + missing) < dataBlocks`. |
| **`cannotHeal`** | Gate abandoning heal for a dangling check when `disksToHealCount > ParityBlocks` (excludes delete markers). |
| **Reconstruct** | Rebuild missing/corrupt shards from survivors via Reed-Solomon; healed drives → `DriveStateOk`. |
| **Purge (as dangling)** | Delete the object across all drives; emit `DeleteDanglingObject` audit (`objects_healed:0`). |
| **Leave degraded** | Return `errErasureReadQuorum` → S3 `SlowDownRead` (HTTP 503); touch nothing. |
| **Delete marker** | A metadata-only object version (no parts) representing a versioned DELETE. |
| **`HealResultItem`** | `madmin-go/v3` struct carrying `Before.Drives`/`After.Drives` per-drive states surfaced to `mc`. |

---

*Generated by the Blitzy Platform. Completion (91.3%) reflects AAP-scoped and path-to-production work only; the remaining 4 hours are human review and merge gates, not autonomous work left undone.*
