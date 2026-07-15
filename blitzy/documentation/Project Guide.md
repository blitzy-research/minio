# Blitzy Project Guide — MinIO Erasure-Coded Fault-Behavior Investigation

> **Brand color legend** — Completed / AI Work: **Dark Blue `#5B39F3`** · Remaining / Not Completed: **White `#FFFFFF`** · Headings / Accents: **Violet-Black `#B23AF2`** · Highlight: **Mint `#A8FDD9`**. Pie-chart slices below are ordered so the first slice (`pie1`) renders Dark Blue (Completed) and the second (`pie2`) renders White (Remaining).

---

## 1. Executive Summary

### 1.1 Project Overview

This project delivers a single, evidence-grounded technical answer document that explains — and demonstrates at runtime — how the MinIO object-storage server (`github.com/minio/minio`) behaves in a four-directory, erasure-coded deployment when drives become inaccessible and later return. The target audience is operators evaluating MinIO for fault tolerance. It is a **read-only investigation and documentation task**: the server was built and run, faults were injected via POSIX permission changes, and behavior was captured through the HTTP health endpoints and real S3 writes, then traced to exact `file:line` code locations. The sole artifact added is `blitzy/documentation/minio_c07e5b49d477.md`; no existing source file is modified.

### 1.2 Completion Status

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#5B39F3','pieStrokeWidth':'2px','pieOpacity':'1'}}}%%
pie showData title Completion Status — 91.7% Complete
    "Completed (AI)" : 33
    "Remaining" : 3
```

| Metric | Hours |
|--------|-------|
| **Total Hours** | **36** |
| **Completed Hours (AI + Manual)** | **33** (AI: 33 · Manual: 0) |
| **Remaining Hours** | **3** |
| **Percent Complete** | **91.7%** |

Completion is computed on AAP-scoped work only (PA1): `Completed / (Completed + Remaining) = 33 / 36 = 91.7%`. Every AAP requirement is fully delivered and validated; the remaining 3 hours are the inherent human acceptance gate (review + merge).

### 1.3 Key Accomplishments

- ✅ **Built and ran MinIO from this checkout** — `CGO_ENABLED=0 go build` (Go 1.23.2, 156,750,679-byte binary), run as a **non-root** single-node 4-drive erasure set on `:9000`/`:9001`.
- ✅ **Exercised canonical entry points only** — HTTP `/minio/health/*` (curl) and real S3 `PUT`/`GET`/`LIST` (boto3); no mocks, debug hooks, or synthetic bypasses.
- ✅ **Reproduced the full scenario matrix** — S0 (healthy 4/4), S1 (above threshold 3/4, adapts and keeps writing), S2 (below threshold 2/4, refuses writes with `SlowDownWrite` while reads survive), S3 (restore 4/4, auto-recovers).
- ✅ **Answered all eight sub-questions (Q1–Q8) by name** — each with a direct answer, captured output, and exact `file:line` citations.
- ✅ **Traced the quorum decision to code** — `objectQuorumFromMeta()` [`cmd/erasure-metadata.go:531-565`] and `(*erasureServerPools).Health()` [`cmd/erasure-server-pool.go:2679`]; 81 unique citations (94 total) all verified.
- ✅ **Documented the non-root requirement** with a root counter-example (root bypasses POSIX permission bits).
- ✅ **Preserved repository integrity** — exactly one new file; working tree clean; all temporary artifacts removed.

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| _None._ The deliverable is complete and independently validated; no blocking issues remain. | — | — | — |

### 1.5 Access Issues

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-----------------|----------------|-------------------|-------------------|-------|
| — | — | No access issues identified. Build toolchain (Go 1.23.2), S3 client (boto3), and curl were all available; the repository was fully accessible and left byte-for-byte unchanged. | N/A | — |

### 1.6 Recommended Next Steps

1. **[Medium]** Have a MinIO-familiar reviewer read the answer document and confirm the Q1–Q8 answers, the quorum math (2+2 → read quorum 2 / write quorum 3), and the load-bearing citations.
2. **[Low]** _(Optional)_ Spot-reproduce scenario S2 (revoke two drives → `503` + `SlowDownWrite`, reads still `200`) using the Section 9 commands to build first-hand confidence.
3. **[Medium]** Merge the branch (a single new file in a new directory — no conflict risk) and deliver the document to the operator who posed the fault-tolerance questions.

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|-------|-------------|
| Toolchain provisioning & binary build | 2 | Provision Go 1.23.2 (matching `go.mod`), boto3 venv; `CGO_ENABLED=0 go build` → 156,750,679-byte binary; version verified. **[AAP §0.3.1, Rule 1]** |
| 4-drive non-root erasure cluster stand-up | 3 | Stand up single-node 2+2 erasure set over four `/tmp` dirs as non-root `miniouser`; diagnose and correct the root-run gotcha (root bypasses POSIX perms). **[AAP §0.3.5]** |
| Observation harness & fault-injection orchestration | 3 | curl health-endpoint polling + boto3 `PUT`/`GET`/`LIST` client; `chmod 000`/`755` fault sequences. **[AAP §0.3.1, Rule 2]** |
| Runtime scenario execution & unedited capture | 3 | Execute S0→S1→S2→S3 + A5 root counter-example; capture before/during/after output incl. logs and timing. **[AAP §0.9.1, Rule 2/3]** |
| Source-code tracing & citation verification | 8 | Trace quorum/health/healing/storage/error subsystems across ~30 files; pinpoint and verify 81 unique `file:line` citations. **[AAP §0.2.1, Rule 4]** |
| Web-research corroboration | 2 | Corroborate via GitHub issues #10057/#12972, discussion #20835; flag the commercial AIStor "48-hour offline" caveat. **[AAP §0.2.2]** |
| Answer document authoring | 7 | Author the 596-line document: TL;DR table, methodology, S0–S3 matrix, eight detailed Q&A sections, caveats (incl. `MINIO_CI_CD` nuance), appendices A1–A5, self-validation. **[MainRule, Rule 4]** |
| QA finding remediation | 4 | Six Markdown-only commits resolving review findings (DOC-01..09, F1, F-A/F-B, F-QA1, F-01..11) — evidence-fidelity labels, reproducibility, citation accuracy. |
| Repository integrity & temp-artifact cleanup | 1 | SIGTERM teardown; `rm -rf` of binary/data/scripts (all outside checkout); verify `git status` clean before and after. **[AAP §0.9.3]** |
| **Total Completed** | **33** | — |

_Total of Hours column (33) matches Completed Hours in Section 1.2._

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|----------|-------|----------|
| HT-1 — SME technical review of the 596-line answer document (verify Q1–Q8 answers, quorum math, load-bearing citations, observed-vs-inferred discipline) | 1 | Medium |
| HT-2 — _(Optional)_ Spot-reproduction of scenario S2 for reviewer confidence (build + non-root run + revoke two drives → confirm `503`/`SlowDownWrite`/read-survival) | 1 | Low |
| HT-3 — PR merge / delivery to the requester (confirm single-file diff, merge, hand off) | 1 | Medium |
| **Total Remaining** | **3** | — |

_Total of Hours column (3) matches Remaining Hours in Section 1.2 and the "Remaining Work" value in the Section 7 pie chart._

### 2.3 Hours Reconciliation

| Check | Result |
|-------|--------|
| Section 2.1 completed total | 33 h |
| Section 2.2 remaining total | 3 h |
| **2.1 + 2.2 = Total (Section 1.2)** | **33 + 3 = 36 h** ✅ |
| Remaining consistent across 1.2 / 2.2 / 7 | 3 h everywhere ✅ |
| Completion % | 33 / 36 = **91.7%** ✅ |

---

## 3. Test Results

All entries below originate from **Blitzy's autonomous validation logs** for this project (build, run-first runtime reproduction, and citation verification). Per the AAP (§0.5.2), unit-test artifacts are **out of scope** for this documentation task; the applicable validation is run-first runtime reproduction, which passed 100%.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|------------|-------|
| Compilation | `go build` / `go vet` | 2 | 2 | 0 | n/a | `go build` exit 0 (156,750,679-byte binary); `go vet ./cmd/` exit 0 |
| Runtime Health-Endpoint | curl (HTTP) | 8 | 8 | 0 | S0–S3 × {`/cluster`, `/cluster/read`} | Write- and read-quorum gates behaved exactly (200/503 as predicted) |
| S3 Object Operations | boto3 | 6 | 6 | 0 | PUT/GET/LIST across states | Includes S2 PUT **correctly rejected** with `SlowDownWrite` (expected) |
| Fault-Injection Behavioral | curl + boto3 | 2 | 2 | 0 | above vs. below threshold | S1 adapts & keeps writing; S2 refuses writes, reads survive |
| Heal / Recovery | admin heal API + on-disk | 3 | 3 | 0 | Q6 | Heal API `200` "finished"; `obj-S1` d4 shard reconstructed; S3 auto-recovery |
| Root Counter-Example | curl + boto3 | 1 | 1 | 0 | A5 | Root (uid 0) bypasses POSIX perms — confirmed, justifies non-root run |
| Citation Accuracy | source verification | 94 | 94 | 0 | 100% | Every `file:line` in-bounds and semantically matched to its claim |
| **TOTAL** | — | **116** | **116** | **0** | **100% pass** | — |

> **Out-of-scope note:** The full `./cmd/` Go unit suite is not part of this deliverable and hangs on DNS-topology tests in the sandbox environment; it was intentionally not run (AAP §0.5.2). This does not affect the documentation deliverable, whose validation is runtime reproduction.

---

## 4. Runtime Validation & UI Verification

**Runtime validation** (single-node non-root 4-drive erasure set; all observed live):

- ✅ **Startup & format** — Operational. Server formed "1 set(s), 4 drives per set"; startup banner confirmed the 2+2 layout and `Runtime: go1.23.2`.
- ✅ **S0 — Healthy (4/4)** — Operational. `/minio/health/cluster` → `200` (`X-Minio-Write-Quorum: 3`); `/minio/health/cluster/read` → `200` (`X-Minio-Read-Quorum: 2`); S3 `PUT` OK.
- ✅ **S1 — Above threshold (3/4)** — Operational. Cluster health stays `200`; `PUT` succeeds — MinIO quietly adapts and keeps writing.
- ⚠ **S2 — Below threshold (2/4)** — Partial by design. `/cluster` → **`503`** and `PUT` → **`SlowDownWrite`** (writes correctly refused); `/cluster/read` → `200` and GET/LIST succeed (**reads survive**). This is the correct, intended erasure-quorum behavior.
- ✅ **S3 — Restore (4/4)** — Operational. `/cluster` auto-recovered to `200` within ~1 s with **no** manual heal; `PUT` succeeds again.
- ✅ **Logs by path** — Operational. Server log names the failing drive by full filesystem path (`.../d4/.minio.sys/buckets/.healing.bin … permission denied`) and emits the write-quorum `FatalKind` line below threshold.
- ✅ **Heal / recovery (Q6)** — Operational. Admin heal API returned `200` ("finished"); `obj-S1`'s missing shard was reconstructed onto d4.

**UI verification:** **Not applicable.** MinIO is a backend object-storage server; there is no user interface, component library, or design system in scope (AAP §0.3.3). The Console port (`:9001`) exists but is out of scope for this behavioral investigation.

---

## 5. Compliance & Quality Review

Cross-mapping AAP deliverables and rules to their validation status:

| Benchmark / Requirement | Source | Status | Progress | Notes |
|--------------------------|--------|--------|----------|-------|
| Single deliverable at `blitzy/documentation/minio_c07e5b49d477.md` | MainRule | ✅ Pass | 100% | Filename = `<source_branch_name>.md`; git diff shows single `A` entry |
| No existing repository file modified/added/deleted | MainRule / §0.5.2 | ✅ Pass | 100% | `git diff c07e5b49d477..HEAD --name-status` = one added file only |
| Run-first: build + run canonical entry point; report commands | Rule 1 | ✅ Pass | 100% | `go build` + non-root `server` run; exact commands in the doc |
| Exhaustive coverage: above/below threshold, reads & writes, before/during/after | Rule 2 | ✅ Pass | 100% | S0–S3 matrix + A5; unedited output included |
| Observed-output discipline; label inferred statements | Rule 3 | ✅ Pass | 100% | 18 observed-evidence blocks; 15–16 `[inferred]` labels |
| Complete grounded answering; `file:line` per code claim | Rule 4 | ✅ Pass | 100% | All Q1–Q8 by name; 81 unique citations verified |
| Non-root execution for genuine permission fault | §0.8.1 | ✅ Pass | 100% | Run as `miniouser` (uid 1001); root counter-example (A5) documented |
| Observed vs. inferred; open-source vs. AIStor caveat | §0.8.1 | ✅ Pass | 100% | AIStor "48-hour offline" flagged, not asserted for this build |
| Mandatory cleanup of temporary artifacts | §0.8.1 / §0.9.3 | ✅ Pass | 100% | SIGTERM teardown + `rm -rf`; `git status` clean (1350 tracked == 1350 on disk) |
| Compilation integrity | Quality gate | ✅ Pass | 100% | `go build` exit 0; `go vet ./cmd/` exit 0 |

**Fixes applied during autonomous validation:** Six QA remediation rounds corrected evidence-fidelity labels, reproducibility details, and citation accuracy (DOC-01..09, F1, F-A/F-B, F-QA1, F-01..11). **Outstanding compliance items:** none.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Non-deterministic run-specific values (per-drive log-line counts, heal `clientToken`, timestamps) may differ on re-run | Technical | Low | Medium | Document frames each as a captured observation (grounding disclaimer; `clientToken` labeled "abbreviated") | ✅ Mitigated |
| Full `./cmd/` Go unit suite hangs on DNS-topology tests in sandbox | Technical | Low | Low | Out of scope per AAP §0.5.2; applicable validation is runtime reproduction (100% pass) | ✅ Accepted (out of scope) |
| `MINIO_CI_CD=1` harness flag set during the run (non-default) | Technical | Low | Low | Doc verifies at runtime that the 4-dir layout starts healthy **without** CI mode and that quorum math never references `globalIsCICD` | ✅ Mitigated |
| No new attack surface / secrets introduced | Security | None | N/A | Read-only task: zero code, zero dependency changes; ephemeral credentials lived in temp dirs outside the checkout and were removed | ✅ N/A |
| Deliverable value depends on operator reading it before relying on MinIO fault tolerance | Operational | Medium | Low | SME review task (HT-1) | ⚠ Open (human gate) |
| Version drift — findings pinned to checkout `c07e5b49…`; future MinIO versions may differ | Operational | Low | Medium | `file:line` pinned to this checkout; AIStor caveat flags commercial divergence | ✅ Mitigated (scoped) |
| Repository integration / merge | Integration | Low | Very Low | Single new file in a new directory (`blitzy/documentation/`) → no conflict risk | ✅ Low/Trivial |

**Overall risk posture: LOW.** Because this is a read-only documentation deliverable (one Markdown file, no code changes), traditional code/security/integration risks are largely not applicable; residual risks are either mitigated within the document itself or reduce to the human review/merge gate.

---

## 7. Visual Project Status

**Project hours breakdown** (Completed = Dark Blue `#5B39F3`, Remaining = White `#FFFFFF`):

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#5B39F3','pieStrokeWidth':'2px','pieOpacity':'1'}}}%%
pie showData title Project Hours Breakdown (Total 36h)
    "Completed Work" : 33
    "Remaining Work" : 3
```

**Remaining work by priority** (hours from Section 2.2; sums to 3 h):

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#A8FDD9','pie3':'#B23AF2','pieStrokeColor':'#B23AF2','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#000000','pieStrokeWidth':'2px','pieOpacity':'1'}}}%%
pie showData title Remaining Hours by Priority
    "Medium (HT-1, HT-3)" : 2
    "Low (HT-2)" : 1
```

> **Integrity check:** "Remaining Work" = **3 h**, identical to Section 1.2 Remaining Hours and the sum of the Section 2.2 Hours column. "Completed Work" = **33 h**, identical to Section 1.2 Completed Hours.

---

## 8. Summary & Recommendations

**Achievements.** The project delivers a complete, run-first, evidence-grounded answer to a compound question about MinIO's fault behavior. MinIO was built from this checkout and run as a non-root 4-drive erasure set; faults were injected via POSIX permission changes; and behavior was captured through the health endpoints and real S3 operations, then traced to exact code locations. All eight sub-questions are answered by name, each behavioral claim paired with captured output and each code claim carrying a verified `file:line` citation.

**Remaining gaps.** None in the deliverable itself. The remaining **3 hours** are the inherent human acceptance gate: an SME technical review, an optional spot-reproduction, and the PR merge/delivery. There is no code, test, configuration, or integration work outstanding.

**Critical path to production.** Review → (optional) reproduce → merge & deliver. Because the branch adds a single file in a new directory, the merge is low-risk.

**Success metrics (all met).** Compilation clean; every runtime scenario (S0–S3, heal, root counter-example) reproduced and matched the document; all 94 citations verified; repository integrity confirmed (exactly one new file, working tree clean, temporary artifacts removed).

**Production-readiness assessment.** The project is **91.7% complete** on AAP-scoped work. The documentation deliverable is production-ready as authored; the residual percentage reflects the standard human sign-off gate that no autonomous agent should self-approve (per the never-claim-100% principle).

| Success Metric | Target | Actual |
|----------------|--------|--------|
| Sub-questions answered by name | 8 (Q1–Q8) | 8 ✅ |
| Runtime scenarios reproduced | S0–S3 + counter-example | 5/5 ✅ |
| Citations verified | 100% | 94/94 ✅ |
| Existing files modified | 0 | 0 ✅ |
| Working tree after task | clean | clean ✅ |

---

## 9. Development Guide

This guide reproduces the investigation end-to-end. Commands marked **[tested]** were executed and verified during this assessment.

### 9.1 System Prerequisites

- **OS:** Linux with enforced POSIX permissions (verified on Ubuntu 25.10). Any modern Linux works.
- **Go:** 1.23.x — `go.mod` declares `go 1.23`; validated with **Go 1.23.2**. **[tested]**
- **Python 3** + **boto3 1.43.47** (S3 client). **[tested: boto3 present]**
- **curl** (verified 8.14.1) for the health endpoints. **[tested]**
- **git**; a **non-root** user (essential — root bypasses permission-based fault injection).
- ~200 MB free disk (≈156 MB binary + four small data dirs).

### 9.2 Environment Setup

```bash
# Put Go on PATH (adjust if your install differs)
export PATH="$PATH:/usr/local/go/bin"
go version                    # expect: go version go1.23.2 linux/amd64   [tested]

# Create a NON-ROOT user for the server (or reuse any existing non-root user)
sudo useradd -m -u 1001 miniouser        # miniouser is absent by default   [tested: confirmed absent]

# Create four data directories OUTSIDE the repository checkout, owned by the non-root user
mkdir -p /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4
sudo chown -R miniouser:miniouser /tmp/minio_data
```

### 9.3 Dependency Installation

```bash
# Go modules download automatically on first build (no vendor dir; cache at /root/go/pkg/mod)  [tested]

# S3 client (skip if boto3 is already available)
python3 -m venv .venv && source .venv/bin/activate
pip install boto3==1.43.47
```

### 9.4 Build

```bash
# From the repository root
CGO_ENABLED=0 go build -o /tmp/minio_bin .        # [tested: exit 0, ~5s warm, 156,750,679-byte binary]
/tmp/minio_bin --version                          # [tested] -> "Runtime: go1.23.2 linux/amd64"
```

### 9.5 Application Startup (non-root, required)

```bash
runuser -u miniouser -- env \
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1 \
  /tmp/minio_bin server \
    /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 \
    --address :9000 --console-address :9001 &
MINIO_PID=$!      # capture PID for a clean shutdown later
# server subcommand + multi-dir arg form validated via `minio_bin server --help`   [tested]
```

- Port **9000** serves the S3 API **and** the health endpoints; port **9001** serves the Console.

### 9.6 Verification Steps

```bash
# Healthy baseline (S0) — expect HTTP 200 with the quorum headers
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster        # 200, X-Minio-Write-Quorum: 3
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster/read   # 200, X-Minio-Read-Quorum: 2
# (curl invocation form validated; connection-refused is expected only when no server is running)  [tested]
```

### 9.7 Example Usage — Fault Injection

```bash
# S1 — lose one drive: still ABOVE write quorum (3/4). Cluster stays 200; PUT succeeds.
chmod 000 /tmp/minio_data/d4
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster        # expect 200

# S2 — lose a second drive: BELOW write quorum (2/4). /cluster -> 503; PUT -> SlowDownWrite; reads survive.
chmod 000 /tmp/minio_data/d3
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster        # expect 503
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster/read   # expect 200 (reads survive)

# S3 — restore: cluster auto-recovers to 200 within ~1s, no manual heal.
chmod 755 /tmp/minio_data/d3 /tmp/minio_data/d4
curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster        # expect 200
```

Minimal S3 write check (boto3):

```python
import boto3
s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
                  aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin")
s3.create_bucket(Bucket="testbucket")
s3.put_object(Bucket="testbucket", Key="obj-S0", Body=b"hello minio quorum demo\n")   # OK at 3/4 or 4/4
# At 2/4 this raises ClientError with code "SlowDownWrite" (HTTP 503)
```

### 9.8 Cleanup

```bash
kill -TERM "$MINIO_PID"                 # graceful shutdown (SIGTERM -> handleSignals -> stopProcess)
rm -rf /tmp/minio_bin /tmp/minio_data   # all artifacts live OUTSIDE the checkout
git status --porcelain                  # [tested] expect empty (working tree clean)
```

### 9.9 Troubleshooting

- **`chmod 000` does not degrade health / writes keep succeeding at 2/4** → You are running the server **as root**. Root bypasses POSIX permission bits (the A5 counter-example). Re-run as a non-root user.
- **First build is slow** → Cold Go module download; subsequent builds complete in ~5 s (warm cache). **[tested]**
- **Verify repository integrity** → `git status --porcelain` (empty = clean) and `git diff --name-status c07e5b49d477..HEAD` (should show a single `A blitzy/documentation/minio_c07e5b49d477.md`). **[tested]**
- **Health endpoint returns connection-refused** → The server is not running or is still initializing; confirm the process is up and port `:9000` is listening.

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---------|---------|
| Build | `CGO_ENABLED=0 go build -o /tmp/minio_bin .` |
| Version | `/tmp/minio_bin --version` |
| Run (non-root) | `runuser -u miniouser -- env MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_CI_CD=1 /tmp/minio_bin server /tmp/minio_data/d1 /tmp/minio_data/d2 /tmp/minio_data/d3 /tmp/minio_data/d4 --address :9000 --console-address :9001 &` |
| Health (write gate) | `curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster` |
| Health (read gate) | `curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/cluster/read` |
| Liveness | `curl -sD - -o /dev/null http://127.0.0.1:9000/minio/health/live` |
| Fault inject / restore | `chmod 000 <drive>` / `chmod 755 <drive>` |
| Static check | `go vet ./cmd/` |
| Integrity | `git status --porcelain` · `git diff --name-status c07e5b49d477..HEAD` |
| Cleanup | `kill -TERM "$MINIO_PID"; rm -rf /tmp/minio_bin /tmp/minio_data` |

### B. Port Reference

| Port | Purpose |
|------|---------|
| 9000 | S3 API **and** health endpoints (`/minio/health/{live,ready,cluster,cluster/read}`) |
| 9001 | MinIO Console (web UI; out of scope for this investigation) |

### C. Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | **The deliverable** (596 lines) — the sole new file |
| `cmd/erasure-metadata.go` (`objectQuorumFromMeta`, L531–565) | Read/write quorum formula |
| `cmd/erasure-server-pool.go` (`Health`, L2679) | Per-set health decision + write-quorum log line |
| `cmd/healthcheck-handler.go` | Health handlers → HTTP 200/503 + quorum headers |
| `cmd/api-errors.go` (L874–877) | `SlowDownWrite` (HTTP 503) mapping |
| `cmd/xl-storage.go` (`Healing`, L436) | Source of the by-path permission-denied log |
| `cmd/background-newdisks-heal-ops.go` (L40) | 10 s new-disk heal poll interval |
| `cmd/erasure-sets.go` (L348) | 15 s endpoint-reconnect interval |
| `cmd/mrf.go` | Most-Recent-Failures partial-write repair |
| `docs/minio-limits.md` (L15–16) | Canonical read quorum `N/2`, write quorum `N/2+1` |

### D. Technology Versions

| Component | Version | Notes |
|-----------|---------|-------|
| Go | 1.23.2 | Matches `go.mod` (`go 1.23`); build toolchain |
| MinIO module | `github.com/minio/minio` @ `c07e5b49…` | This checkout (base commit) |
| klauspost/reedsolomon | v1.12.4 | Erasure coding (unchanged) |
| boto3 | 1.43.47 | S3 client for `PUT`/`GET`/`LIST` |
| curl | 8.14.1 | Health-endpoint probing |

### E. Environment Variable Reference

| Variable | Value used | Purpose |
|----------|------------|---------|
| `MINIO_ROOT_USER` | `minioadmin` | Admin/S3 access key (ephemeral) |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Admin/S3 secret key (ephemeral) |
| `MINIO_CI_CD` | `1` | Non-default harness flag; verified **not** to affect quorum math or the health decision (behavior identical when unset for this `/tmp` layout) |

### F. Developer Tools Guide

| Tool | Use |
|------|-----|
| `go build` / `go vet` | Compile the server and run static analysis (`go vet ./cmd/` exit 0) |
| `git diff --name-status c07e5b49d477..HEAD` | Confirm exactly one added file (repository integrity) |
| `git status --porcelain` | Confirm a clean working tree before/after the run |
| `curl -sD -` | Inspect health-endpoint status codes and quorum headers |
| `runuser` / `chmod` | Run as non-root and inject/restore permission-based drive faults |

### G. Glossary

| Term | Meaning |
|------|---------|
| **Erasure coding** | Splitting an object into *data* + *parity* shards so it can be reconstructed after losing up to *parity* shards. A 4-drive default set is 2 data + 2 parity. |
| **Write quorum** | Minimum online drives required to accept a write. For 2+2: `dataBlocks (=2)`, incremented to **3** because `dataBlocks == parityBlocks`. |
| **Read quorum** | Minimum online drives required to read. For 2+2: `N/2 = 2`. |
| **`SlowDownWrite`** | The S3 error (HTTP 503) returned when writes cannot meet write quorum. |
| **MRF** | Most-Recent-Failures subsystem that tracks and heals writes that reached quorum but not all shards. |
| **Healing** | Background reconstruction of missing/outdated shards onto a returned drive from surviving shards. |
| **`.healing.bin`** | Per-drive marker file whose read attempt produces the by-path permission-denied log during the fault. |
| **`[inferred]`** | Label in the deliverable marking a conclusion drawn from reading code rather than directly observed at runtime. |

---

*Prepared per the Blitzy Project Guide Template. All numbers are consistent across Sections 1.2, 2.1, 2.2, and 7 (Total 36 h · Completed 33 h · Remaining 3 h · 91.7% complete). All test results originate from Blitzy's autonomous validation logs for this project.*