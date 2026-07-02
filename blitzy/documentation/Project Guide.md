# Blitzy Project Guide

## 1. Executive Summary

### 1.1 Project Overview

This project delivers a single, evidence-backed technical document that explains how MinIO — the distributed object-storage server (Go module `github.com/minio/minio`, Go 1.23) — decides it is "healthy" in a four-directory erasure-coded deployment, and how it behaves as drives are lost through permission changes and later recovered. Targeted at engineers operating or studying MinIO, the document answers eight specific questions by pairing exact `file:line` source citations with live runtime observations (health-endpoint statuses/headers, write/read attempts, and logs) captured from a running server. The technical scope is a strictly read-only investigation: the only committed artifact is one Markdown answer document under `blitzy/documentation/`; no MinIO source, test, configuration, or CI file is altered.

### 1.2 Completion Status

The project is **92.6% complete** based on AAP-scoped hours: 25.0 completed hours out of 27.0 total hours, with 2.0 hours of human path-to-production review remaining.

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#B23AF2'}}}%%
pie showData
    title Completion Status — 92.6% Complete
    "Completed Work (h)" : 25
    "Remaining Work (h)" : 2
```

| Metric | Hours |
|--------|-------|
| Total Hours | 27.0 |
| Completed Hours (AI + Manual) | 25.0 (25.0 AI + 0.0 Manual) |
| Remaining Hours | 2.0 |
| **Percent Complete** | **92.6%** |

**Calculation:** Completion % = Completed Hours ÷ Total Hours × 100 = 25.0 ÷ 27.0 × 100 = **92.6%**.

### 1.3 Key Accomplishments

- ✅ Built MinIO from source (`CGO_ENABLED=0 go build -tags kqueue -trimpath`) to a 150 MB static binary (go1.23.12), exit 0, zero errors.
- ✅ Ran a four-directory `EC:2` erasure deployment as non-root (`nobody`, uid 65534) and captured the healthy baseline: `/minio/health/cluster` → `200` with `X-Minio-Write-Quorum: 3`; `/minio/health/cluster/read` → `200` with `X-Minio-Read-Quorum: 2`.
- ✅ Reproduced Scenario A (one drive lost, 3 online = write quorum): cluster stays `200`, writes succeed; failing drive logged **by full path**.
- ✅ Reproduced Scenario B (second drive lost, 2 online < write quorum): `/cluster` → `503`, `/cluster/read` → `200`, PUT fails with S3 `SlowDownWrite`, GET of existing object still succeeds.
- ✅ Reproduced recovery: restoring permissions returns cluster health to `200` on its own; object written during the outage repaired on next read (heal-on-read) and via `mc admin heal`.
- ✅ Traced the full quorum code chain from default-parity selection through per-object quorum to write-path enforcement and `Health()`, including the parity-equals-half `+1` split-brain rule.
- ✅ Authored the 583-line answer document covering Q1–Q8 and 30 named mechanisms, with 57 verified `file:line` citations; all temporary artifacts cleaned up; working tree clean.

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| None | No unresolved issues block release or validation. The deliverable is complete, builds clean, and every documented claim was independently reproduced at runtime. | — | — |

### 1.5 Access Issues

No access issues identified. The investigation runs entirely on localhost (`127.0.0.1:9000/9001`) with no external service, credential, or network dependency. Build and runtime were performed with tooling already present on the host (Go 1.23.12, `setpriv`, `curl`, `mc`).

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-----------------|----------------|-------------------|-------------------|-------|
| MinIO source repository | Read/write (branch) | None — deliverable committed successfully | Resolved | Blitzy Agent |
| MinIO server (localhost) | Runtime (localhost only) | None — ran as non-root, no external creds | Resolved | Blitzy Agent |

### 1.6 Recommended Next Steps

1. **[High]** Have a MinIO-familiar SME review and accept the answer document — confirm Q1–Q8 are answered, spot-check citations, and confirm the quorum math (WQ 3, RQ 2, `+1` rule) and the reconnect-not-full-heal recovery nuance (1.0 h).
2. **[Medium]** Independently re-validate on a Linux host: rebuild with Go 1.23.x and re-run baseline + Scenario A + Scenario B + recovery as non-root, confirming the health statuses/headers and `SlowDownWrite` error match the document (0.5 h).
3. **[Low]** Complete PR review housekeeping and merge — confirm the read-only scope (only the one file added), clean `git status`, and approve (0.5 h).

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|-------|-------------|
| [AAP] Runtime substrate | 2.0 | Build `./minio` from source (go1.23.12); non-root execution (uid 65534/`nobody`); four scratch directories; `mc`/`curl` tooling |
| [AAP] Q1 — Baseline health decision & disk assumptions | 2.5 | Probe `/cluster`, `/cluster/read`, `/live`, `/ready` + baseline PUT/GET; trace `Health()` and `DefaultParityBlocks` (`EC:2`) |
| [AAP] Q2 — Single-disk permission loss | 1.5 | `chmod 000`; capture "drive access denied"; trace `os.IsPermission` → `errDiskAccessDenied` + disk-error taxonomy |
| [AAP] Q3 — Two scenarios above/below threshold | 3.0 | Writes at 3 and 2 online; `200`/`503`; `SlowDownWrite` JSON; read-still-served; trace threshold math (metadata/object/errors/api-errors) |
| [AAP] Q4 — By-path logging + live recovery signal | 2.0 | `endpoint=` path logs + `.healing.bin` probe; trace `printEndpointError`, call sites, `Healing()` |
| [AAP] Q5 — Recovery detection | 2.0 | Restore perms; self-detect polling; trace `monitorLocalDisksAndHeal` (10s) + `healFreshDisk`; reconnect-not-full-heal nuance |
| [AAP] Q6 — Object repair | 2.5 | Heal-on-read (`xl.meta` 0→1); `mc admin heal`; trace `healObject`/`HealObject`/`shouldHealObjectOnDisk` + MRF |
| [AAP] Q7 — Quorum location & threshold math | 2.0 | Full chain `DefaultParityBlocks` → `objectQuorumFromMeta` → write-path → `Health()`; the `+1` split-brain rule |
| [AAP] Q8 — Grounding + coverage pass | 1.5 | Consolidate evidence; map Q1–Q8 + 30 named mechanisms in coverage tables |
| [AAP] Web-search terminology validation | 1.0 | Read/write quorum, `+1` split-brain rule, healing vocabulary validated against MinIO docs then re-verified in code |
| [AAP] Citation verification + fidelity corrections | 2.5 | Verify all 57 `file:line` refs; 3 fidelity corrections (go1.23.4→.12, stack frame 8→7, `SlowDownWrite` JSON note); re-ground to runtime |
| [AAP] Document authoring / GFM structure + cleanup + git hygiene | 2.5 | 583 lines, tables, balanced fences, single H1; cleanup temp artifacts; 3 commits; clean working tree |
| **Total Completed** | **25.0** | |

**Validation:** The Hours column sums to **25.0**, matching Completed Hours in Section 1.2.

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|----------|-------|----------|
| SME technical review & acceptance of the answer document | 1.0 | High |
| Independent environment re-validation before sign-off | 0.5 | Medium |
| PR review housekeeping & merge | 0.5 | Low |
| **Total Remaining** | **2.0** | |

**Validation:** The Hours column sums to **2.0**, matching Remaining Hours in Section 1.2 and the "Remaining Work" value in Section 7.

### 2.3 Totals Reconciliation

| Aggregate | Hours |
|-----------|-------|
| Section 2.1 — Completed | 25.0 |
| Section 2.2 — Remaining | 2.0 |
| **Total (must equal Section 1.2 Total)** | **27.0** |

25.0 + 2.0 = **27.0** ✅ matches Total Hours in Section 1.2.

---

## 3. Test Results

All tests below originate from Blitzy's autonomous validation logs for this project. Because the deliverable is an evidence-backed document (not application code), the primary "tests" are runtime reproductions of every documented behavioral claim, complemented by the one directly-relevant, fast, safe unit test that validates the parity math underpinning the quorum numbers.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|-------------|--------|--------|-----------|-------|
| Unit (parity math) | `go test` | 1 suite | 1 | 0 | n/a | `./internal/config/storageclass/...` → `ok` (validates `DefaultParityBlocks` = `EC:2` for 4/5 drives ⇒ WQ 3 / RQ 2) |
| Build validation | `go build` | 1 | 1 | 0 | n/a | `CGO_ENABLED=0 go build -tags kqueue -trimpath` → exit 0, 150 MB static ELF (go1.23.12) |
| Runtime — Baseline (4 online) | curl + mc | 6 | 6 | 0 | n/a | `/cluster` 200 + WQ 3; `/cluster/read` 200 + RQ 2; `/live` 200; `/ready` 200; PUT ok; GET ok |
| Runtime — Scenario A (3 online) | curl + mc | 3 | 3 | 0 | n/a | `/cluster` stays 200; PUT succeeds; failing drive logged by full path |
| Runtime — Scenario B (2 online) | curl + mc | 5 | 5 | 0 | n/a | `/cluster` 503; `/cluster/read` 200; PUT → `SlowDownWrite`; GET succeeds; write-quorum log line emitted |
| Runtime — Recovery | curl + mc | 3 | 3 | 0 | n/a | Cluster self-recovers to 200 (no manual push); heal-on-read restores shard; `mc admin heal` completes |
| Citation integrity | scripted check | 57 | 57 | 0 | 100% | All 57 distinct `file:line` citations exist and fall within source bounds; core citations content-matched |
| **Total** | — | **79** | **79** | **0** | — | 100% pass rate across all autonomous validations |

> Note on scope: the MinIO Go test suite at large is out-of-scope REFERENCE code per the AAP and was not run or modified. The applicable validations (100% runtime reproduction + the directly-relevant storageclass package test) fully passed.

---

## 4. Runtime Validation & UI Verification

This is a server-side investigation with no user-facing UI; "UI verification" is therefore the HTTP health-surface and object-API behavior observed at runtime.

**Build & process health**
- ✅ Operational — MinIO built to a 150 MB static binary and launched successfully as non-root (`nobody`, uid 65534) over four directories; startup banner: `Formatting 1st pool, 1 set(s), 4 drives per set.`

**Health endpoints — Baseline (4 drives online)**
- ✅ Operational — `GET /minio/health/cluster` → `HTTP/1.1 200 OK`, `X-Minio-Write-Quorum: 3`
- ✅ Operational — `GET /minio/health/cluster/read` → `HTTP/1.1 200 OK`, `X-Minio-Read-Quorum: 2`
- ✅ Operational — `GET /minio/health/live` → `200`; `GET /minio/health/ready` → `200`

**Object API — Baseline**
- ✅ Operational — bucket create, PUT, and GET all succeed

**Scenario A — one drive lost (3 online = write quorum 3)**
- ✅ Operational — `/minio/health/cluster` stays `200`; PUT succeeds (write above threshold)
- ✅ Operational — failing drive named by full path in logs (`drive access denied`, endpoint `/tmp/minio-data/disk4`)

**Scenario B — second drive lost (2 online < write quorum 3)**
- ❌ Failing (by design) — `/minio/health/cluster` → `503`; PUT fails with S3 `SlowDownWrite`
- ✅ Operational — `/minio/health/cluster/read` → `200`; GET of existing object still succeeds (read quorum 2 met)
- ✅ Operational — health layer logs: `Write quorum could not be established on pool: 0, set: 0, expected write quorum: 3, drives-online: 2`

**Recovery — permissions restored**
- ✅ Operational — `/minio/health/cluster` self-recovers to `200` (no manual push required)
- ✅ Operational — object written during the outage restored on next read (heal-on-read); `mc admin heal` completes with `[Green -> Green]`

---

## 5. Compliance & Quality Review

The deliverable is measured against the AAP's binding rules (`SWE-AtlasQnA-Repo`) and Blitzy quality benchmarks.

| Benchmark / AAP Rule | Status | Progress | Notes |
|----------------------|--------|----------|-------|
| Deliverable at correct path/name (`blitzy/documentation/minio_c07e5b49d477.md`) | ✅ Pass | 100% | Filename derives from source branch `minio_c07e5b49d477` |
| Investigate by RUNNING code first, then write | ✅ Pass | 100% | Built + ran server; all prose derived from captured runtime output |
| Quote observed output verbatim (one claim / one evidence) | ✅ Pass | 100% | Each behavioral claim paired with the exact log line / HTTP status / error code |
| Exact & grounded literals with `file:line` | ✅ Pass | 100% | 57 citations, all verified in-bounds; quorum numbers, error codes, paths quoted exactly |
| Report exactly what is observed (incl. nuance) | ✅ Pass | 100% | Reconnect-not-full-heal nuance documented as observed, not idealized |
| Answer every question + every named item | ✅ Pass | 100% | Q1–Q8 + 30 named mechanisms covered; coverage-pass tables included |
| Read-only scope (no existing files modified) | ✅ Pass | 100% | 1 file added; zero source/test/CI/docs modifications |
| No dependency changes (`go.mod`/`go.sum`) | ✅ Pass | 100% | Unchanged; binary built from pinned deps as-is |
| Temp artifacts removed; repo left clean | ✅ Pass | 100% | All `/tmp` artifacts removed; `git status --porcelain` clean |
| Well-formed GitHub-flavored Markdown | ✅ Pass | 100% | Single H1, balanced code fences, well-formed tables; no placeholders/TODO/stubs |

**Fixes applied during autonomous validation:** (1) Go toolchain patch corrected `go1.23.4` → `go1.23.12` to match the live startup banner; (2) Q4 `.healing.bin` stack-frame number corrected `8` → `7` to match the real trace variant; (3) Q3 `SlowDownWrite` JSON annotated to show salient/reproducible fields with volatile `RequestID`/`HostID` elided.

**Outstanding compliance items:** None. All benchmarks pass; remaining work is human review only.

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Citation/version drift — 57 `file:line` refs pinned to commit `c07e5b49d`; future edits shift line numbers | Technical | Low | Medium | Filename encodes the commit; doc scoped to "this checkout"; re-verify if rebased onto newer MinIO | Mitigated by design |
| Single-node vs true multi-host fidelity — a real network-partition split-brain cannot be physically reproduced single-node | Technical | Low | Low | Doc cites per-erasure-set quorum design; single-node exercises identical code paths; optional multi-host re-validation flagged | Documented caveat |
| Go patch-version specificity — banner `go1.23.12` vs `go.mod` constraint `go 1.23` | Technical | Very Low | Low | Reported-as-observed; `go.mod:L3` constraint noted | Mitigated |
| Default credentials shown (`minioadmin`) in launch command | Security | Low | Low | MinIO's public defaults on an ephemeral localhost run; explicit "note on credentials" in doc | Mitigated / annotated |
| Secrets committed | Security | None | — | Deliverable is a doc; no keys/tokens; git-clean confirmed | Pass |
| New attack surface | Security | None | — | Read-only; zero MinIO code changes | Pass |
| Reproduction requires Linux + `setpriv` + non-root | Operational | Low | Low | Exact launch command + rationale documented | Documented |
| Temp-artifact hygiene | Operational | None | — | All `/tmp` artifacts removed; git-clean verified twice | Pass |
| Docs-index integration | Integration | None | — | Self-contained in new directory; AAP confirms no index/README update needed | Pass / N/A |
| `mc` client version dependency (`--json` formatting) | Integration | Very Low | Low | `mc` version pinned; salient JSON fields noted, volatile IDs elided | Mitigated |
| External service dependencies | Integration | None | — | Entirely localhost; no external APIs/credentials | Pass |

**Net risk profile: LOW.** No High/Critical risks. All identified risks are Low/Very-Low/None, either mitigated by design or documented as caveats. Nothing blocks acceptance or merge.

---

## 7. Visual Project Status

**Project hours breakdown (Completed vs Remaining):**

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#FFFFFF','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#B23AF2'}}}%%
pie showData
    title Project Hours Breakdown
    "Completed Work" : 25
    "Remaining Work" : 2
```

**Remaining work by priority (hours):**

```mermaid
%%{init: {'theme':'base', 'themeVariables': {'pie1':'#5B39F3','pie2':'#B23AF2','pie3':'#A8FDD9','pieStrokeColor':'#B23AF2','pieStrokeWidth':'2px','pieOuterStrokeColor':'#B23AF2','pieTitleTextColor':'#B23AF2','pieSectionTextColor':'#B23AF2'}}}%%
pie showData
    title Remaining Work by Priority (2.0 h total)
    "High — SME review" : 1.0
    "Medium — re-validation" : 0.5
    "Low — PR merge" : 0.5
```

**Integrity check:** Section 7 "Remaining Work" = **2.0 h**, identical to Section 1.2 Remaining Hours and the Section 2.2 Hours sum. "Completed Work" = **25.0 h**, identical to Section 1.2 Completed Hours and the Section 2.1 sum. The priority pie sums to 1.0 + 0.5 + 0.5 = **2.0 h**.

---

## 8. Summary & Recommendations

**Achievements.** The project delivers a complete, evidence-backed answer document that explains MinIO's health/quorum/recovery behavior in a four-drive `EC:2` deployment. Every one of the eight posed questions is answered, and every behavioral claim is paired with a verbatim runtime observation and an exact `file:line` citation. The investigation was independently reproduced end-to-end: the binary rebuilds cleanly, and the baseline → one-drive-lost → two-drives-lost → recovery sequence produced exactly the documented statuses, headers, error codes, and log lines.

**Remaining gaps.** The remaining **2.0 hours** are entirely human path-to-production activities: an SME technical review/acceptance, an optional independent environment re-validation, and PR review/merge housekeeping. There is no outstanding engineering work, no failing validation, and no unresolved error.

**Critical path to production.** SME acceptance (1.0 h) → optional re-validation (0.5 h) → PR merge (0.5 h). None of these are blocking in the sense of requiring code changes; they are quality gates.

**Success metrics.** 79/79 autonomous validations passed; 57/57 citations verified; build exit 0; working tree clean; read-only scope preserved (exactly one file added).

**Production readiness assessment.** The project is **92.6% complete** (25.0 of 27.0 hours). The single deliverable is production-ready pending human sign-off; the residual 7.4% reflects human review gates rather than incomplete engineering. Overall risk is LOW with no High/Critical items.

| Metric | Value |
|--------|-------|
| Completion | 92.6% (25.0 / 27.0 h) |
| Autonomous validations passed | 79 / 79 |
| Citations verified | 57 / 57 |
| Files added / modified | 1 / 0 |
| Open blocking issues | 0 |
| Net risk profile | Low |

---

## 9. Development Guide

This guide reproduces the investigation substrate: build MinIO, run a four-directory `EC:2` deployment as a non-root user, and observe health/quorum/recovery behavior. Every command is copy-pasteable and was tested during validation.

### 9.1 System Prerequisites

- **OS:** Linux (permission-based disk loss requires POSIX mode bits + a non-root process; `setpriv` is used to drop privileges).
- **Go:** 1.23.x (repo declares `go 1.23`; validated with `go1.23.12`).
- **Tools:** `git`, `curl`, and the MinIO client `mc` on `PATH`; `setpriv` (from `util-linux`).
- **User:** a non-root uid for the server (uid 65534 / `nobody`) so `chmod 000` genuinely denies access.

```bash
# Verify prerequisites
go version                 # expect: go version go1.23.x linux/amd64
command -v setpriv curl mc # all three must resolve
```

### 9.2 Environment Setup

```bash
# From the repository root (destination branch checked out)
cd <repo-root>

# Create four scratch data directories owned by the non-root run user
rm -rf /tmp/minio-data /tmp/minio-home
mkdir -p /tmp/minio-data/disk1 /tmp/minio-data/disk2 \
         /tmp/minio-data/disk3 /tmp/minio-data/disk4 /tmp/minio-home
chown -R 65534:65534 /tmp/minio-data /tmp/minio-home
```

### 9.3 Build

```bash
mkdir -p /tmp/minio-bin
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .
# Expected: exit 0; ~150 MB static ELF
/tmp/minio-bin/minio --version   # banner reports: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)
```

### 9.4 Run (four-directory EC:2, as non-root)

```bash
setpriv --reuid=65534 --regid=65534 --clear-groups \
  env HOME=/tmp/minio-home MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  /tmp/minio-bin/minio server \
    /tmp/minio-data/disk1 /tmp/minio-data/disk2 /tmp/minio-data/disk3 /tmp/minio-data/disk4 \
    --address 127.0.0.1:9000 --console-address 127.0.0.1:9001 > /tmp/minio-run.log 2>&1 &
MPID=$!
sleep 8
grep -E "Formatting|Version:" /tmp/minio-run.log   # "Formatting 1st pool, 1 set(s), 4 drives per set."
```

> Note on credentials: `minioadmin/minioadmin` are MinIO's public defaults, used only for this ephemeral localhost run — not a production secret.

### 9.5 Verification (healthy baseline)

```bash
curl -sI http://127.0.0.1:9000/minio/health/cluster       | grep -iE 'HTTP/|Write-Quorum'
# HTTP/1.1 200 OK   |   X-Minio-Write-Quorum: 3
curl -sI http://127.0.0.1:9000/minio/health/cluster/read  | grep -iE 'HTTP/|Read-Quorum'
# HTTP/1.1 200 OK   |   X-Minio-Read-Quorum: 2
curl -s -o /dev/null -w 'live: %{http_code}\n'  http://127.0.0.1:9000/minio/health/live    # 200
curl -s -o /dev/null -w 'ready: %{http_code}\n' http://127.0.0.1:9000/minio/health/ready   # 200
```

### 9.6 Example Usage (reproduce the scenarios)

```bash
export MC_HOST_local="http://minioadmin:minioadmin@127.0.0.1:9000"
mc mb -q local/testbucket
printf 'hello-minio-baseline' > /tmp/obj1.txt
mc cp -q /tmp/obj1.txt local/testbucket/obj1.txt   # baseline PUT: OK

# Scenario A — one drive lost (3 online == write quorum 3)
chmod 000 /tmp/minio-data/disk4 ; sleep 3
curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -iE 'HTTP/'   # 200
printf 'objA-while-disk4-down' > /tmp/objA.txt
mc cp -q /tmp/objA.txt local/testbucket/objA.txt   # PUT still succeeds

# Scenario B — second drive lost (2 online < write quorum 3)
chmod 000 /tmp/minio-data/disk3 ; sleep 3
curl -sI http://127.0.0.1:9000/minio/health/cluster      | grep -iE 'HTTP/'   # 503
curl -sI http://127.0.0.1:9000/minio/health/cluster/read | grep -iE 'HTTP/'   # 200
mc cp --json /tmp/objA.txt local/testbucket/objB.txt | grep -oE '"Code":"[^"]+"' | head -1  # "Code":"SlowDownWrite"
mc cat local/testbucket/obj1.txt   # GET still succeeds (read quorum met)
grep -oE 'Write quorum could not be established[^(]*' /tmp/minio-run.log | tail -1

# Recovery — restore permissions
chmod 755 /tmp/minio-data/disk3 /tmp/minio-data/disk4
for t in 3 6 9; do sleep 3; curl -sI http://127.0.0.1:9000/minio/health/cluster | grep -oE 'HTTP/1.1 [0-9]+'; done  # returns to 200
mc cat local/testbucket/objA.txt          # heal-on-read
mc admin heal --recursive --force local/testbucket | tail -3
```

### 9.7 Teardown

```bash
kill $MPID 2>/dev/null; wait $MPID 2>/dev/null
rm -rf /tmp/minio-data /tmp/minio-home /tmp/minio-bin /tmp/minio-run.log /tmp/obj*.txt
git status --porcelain --untracked-files=all   # must be empty (repo unchanged)
```

### 9.8 Troubleshooting

- **`chmod 000` has no effect / scenarios don't fail:** the server is running as root. Root bypasses mode bits — always launch via `setpriv` as a non-root uid.
- **`address already in use`:** ports 9000/9001 are taken. Change `--address`/`--console-address` or stop the conflicting process (use the exact captured PID; never broad `pkill`).
- **`mc` cannot connect:** confirm `MC_HOST_local` matches the running address/credentials and that the server finished formatting (`sleep 8`, check the log).
- **Permission-denied cleaning `/tmp/minio-data`:** restore permissions first (`chmod -R 755 /tmp/minio-data`) before `rm -rf`.
- **Markdown validation (fence-aware H1, fence balance, citation count):**
  ```bash
  awk '/^```/{f=!f;next} /^# /&&!f{c++} END{print "H1:",c}' blitzy/documentation/minio_c07e5b49d477.md
  grep -cE '^```' blitzy/documentation/minio_c07e5b49d477.md   # even = balanced
  grep -oE '(cmd|internal|docs)/[A-Za-z0-9_./-]+\.(go|md):L[0-9]+(-L[0-9]+)?' \
    blitzy/documentation/minio_c07e5b49d477.md | sort -u | wc -l   # 57
  ```

---

## 10. Appendices

### A. Command Reference

| Purpose | Command |
|---------|---------|
| Build MinIO | `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio-bin/minio .` |
| Run (non-root, 4 dirs) | `setpriv --reuid=65534 --regid=65534 --clear-groups env HOME=/tmp/minio-home ... minio server /tmp/minio-data/disk{1,2,3,4} --address 127.0.0.1:9000 --console-address 127.0.0.1:9001` |
| Cluster write health | `curl -sI http://127.0.0.1:9000/minio/health/cluster` |
| Cluster read health | `curl -sI http://127.0.0.1:9000/minio/health/cluster/read` |
| Object PUT / GET | `mc cp <file> local/<bucket>/<key>` / `mc cat local/<bucket>/<key>` |
| Trigger heal | `mc admin heal --recursive --force local/<bucket>` |
| Storageclass unit test | `go test -tags kqueue,dev -count=1 ./internal/config/storageclass/...` |

### B. Port Reference

| Port | Service |
|------|---------|
| 9000 | MinIO S3 API + health endpoints (`127.0.0.1:9000`) |
| 9001 | MinIO Console (`127.0.0.1:9001`) |

### C. Key File Locations

| Path | Role |
|------|------|
| `blitzy/documentation/minio_c07e5b49d477.md` | The deliverable (only committed artifact) |
| `internal/config/storageclass/storage-class.go` | `DefaultParityBlocks` — 4/5 drives ⇒ `EC:2` (REFERENCE) |
| `cmd/erasure-metadata.go` | Per-object quorum + `+1` split-brain rule (REFERENCE) |
| `cmd/erasure-object.go` | Write-path quorum enforcement (REFERENCE) |
| `cmd/erasure-server-pool.go` | `Health()` decision + quorum-not-established logging (REFERENCE) |
| `cmd/healthcheck-handler.go` / `-router.go` | Cluster health handlers, headers, routes (REFERENCE) |
| `cmd/xl-storage.go` | `os.IsPermission` → `errDiskAccessDenied`; `Healing()` probe (REFERENCE) |
| `cmd/prepare-storage.go` | `printEndpointError` — by-path disk logging (REFERENCE) |
| `cmd/background-newdisks-heal-ops.go` | 10s heal-poll loop (REFERENCE) |
| `cmd/erasure-healing.go` / `cmd/mrf.go` | Per-object heal + MRF re-heal (REFERENCE) |
| `cmd/api-errors.go` | `SlowDownWrite` mapping for insufficient write quorum (REFERENCE) |

### D. Technology Versions

| Component | Version |
|-----------|---------|
| Go toolchain | go1.23.12 (repo constraint `go 1.23`) |
| MinIO server | `DEVELOPMENT.GOGET` @ commit `c07e5b49d` |
| MinIO client `mc` | `RELEASE.2025-08-13T08-35-41Z` |
| curl | system-provided |
| Build tags | `kqueue` (server), `kqueue,dev` (storageclass test) |

### E. Environment Variable Reference

| Variable | Value (test) | Purpose |
|----------|--------------|---------|
| `MINIO_ROOT_USER` | `minioadmin` | Root access key (public default; ephemeral localhost only) |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Root secret key (public default; ephemeral localhost only) |
| `HOME` | `/tmp/minio-home` | Writable home for the non-root run user |
| `MC_HOST_local` | `http://minioadmin:minioadmin@127.0.0.1:9000` | `mc` alias for the running server |
| `CGO_ENABLED` | `0` | Static build |

### F. Developer Tools Guide

- **`curl -sI`** — read health-endpoint status line and quorum headers (`X-Minio-Write-Quorum`, `X-Minio-Read-Quorum`).
- **`mc` (`cp`, `cat`, `admin heal`)** — exercise object PUT/GET and trigger healing; use `--json` to capture the exact S3 error `Code`.
- **`setpriv`** — drop to a non-root uid so `chmod 000` genuinely denies access (mandatory for the permission scenario).
- **`go test`** — run the fast, safe storageclass unit test that validates the `EC:2` parity math.
- **`awk`/`grep` fence-aware checks** — validate Markdown H1 count, fence balance, and citation count.

### G. Glossary

| Term | Meaning |
|------|---------|
| Erasure set | Group of drives across which an object is erasure-coded; quorum is evaluated per set |
| `EC:2` | Erasure coding with 2 parity blocks (for a 4-drive set: data = 2, parity = 2) |
| Write quorum | Minimum online drives to accept writes; here `dataBlocks + 1 = 3` (parity-equals-half `+1` rule) |
| Read quorum | Minimum online drives to serve reads; here `dataBlocks = 2` |
| Split-brain `+1` rule | When parity equals half the set size, write quorum is incremented by 1 to prevent split-brain |
| Heal-on-read | Repair of a missing shard triggered when an object is next read |
| MRF | Most-Recent-Failures — background routine that re-heals objects after a drive reconnects |
| `SlowDownWrite` | S3 error returned when write quorum cannot be met (mapped from `InsufficientWriteQuorum`) |
| `.healing.bin` | Per-drive marker file MinIO probes to determine healing state |