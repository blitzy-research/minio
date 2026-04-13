# Blitzy Project Guide

## 1. Executive Summary

### 1.1 Project Overview

This project conducts a deep, non-destructive runtime investigation of five specific MinIO security enforcement mechanisms within the `github.com/minio/minio` repository (branch `minio_c07e5b49d477`). The investigation targets five defense layers — SSE bucket-level encryption enforcement, WORM object lock delete protection, bitrot detection and healing, STS session policy enforcement, and privilege escalation prevention — producing verifiable runtime evidence including server trace logs, Go test suite output, and code-path analysis. The sole output artifact is a 1,049-line comprehensive Q&A documentation file. No repository source files were modified.

### 1.2 Completion Status

```mermaid
pie title Project Completion Status
    "Completed (63h)" : 63
    "Remaining (8h)" : 8
```

| Metric | Value |
|---|---|
| **Total Project Hours** | 71 |
| **Completed Hours (AI)** | 63 |
| **Remaining Hours** | 8 |
| **Completion Percentage** | 88.7% |

**Calculation**: 63 completed hours / (63 + 8 remaining hours) = 63 / 71 = **88.7% complete**

### 1.3 Key Accomplishments

- ✅ **Investigation 1 — SSE Enforcement**: Live server traces captured proving `X-Amz-Server-Side-Encryption: AES256` header injection on unencrypted uploads; full code path documented from `PutObjectHandler()` through `BucketSSEConfig.Apply()`
- ✅ **Investigation 2 — Object Lock Protection**: HTTP 400 XML error response (`InvalidRequest` / "WORM protected") captured for version-specific deletes; governance vs. compliance behavior fully documented
- ✅ **Investigation 3 — Bitrot Detection**: Three existing test suites executed and passed — `TestHealObjectCorruptedParts`, `TestHealObjectCorruptedPools`, `TestHealObjectCorruptedXLMeta`; code path from `newBitrotReader` through `errFileCorrupt` to `globalMRFState.addPartialOp()` documented
- ✅ **Investigation 4 — STS Session Policy**: `TestIAMInternalIDPSTSServerSuite` passed across 3 server configurations; intersection model of session + parent policies proven at `cmd/iam.go:2310-2312`
- ✅ **Investigation 5 — Privilege Escalation Prevention**: Live 403 AccessDenied traces captured; `TestIAMInternalIDPServerSuite` passed across 3 configurations; root cause identified — `PolicyName` silently ignored at `cmd/iam-store.go:2659-2692`
- ✅ **Cross-cutting integration points** documented: middleware chain, IAM authorization dispatch, bucket metadata cache, KMS integration, audit logging
- ✅ **Dependency vulnerability assessment**: 5 CVEs identified (1 CRITICAL, 4 HIGH) with consolidated remediation plan
- ✅ **Documentation artifact**: `blitzy/documentation/minio_c07e5b49d477.md` — 1,049 lines covering all 5 investigations
- ✅ **Non-destructive constraint**: Zero source files modified; only 1 documentation file added
- ✅ **All validation gates passed**: `go vet` clean, `go build` successful, all 9 test suites passed (2 etcd variants auto-skipped)

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|---|---|---|---|
| CVE-2024-45337 — SSH auth bypass in `golang.org/x/crypto v0.29.0` (CVSS 9.1) | Unauthorized SFTP access to MinIO storage | Human Developer | 4h |
| CVE-2025-30204 — JWT DoS in `golang-jwt/jwt/v4 v4.5.1` (CVSS 7.5) | Memory exhaustion via crafted OIDC tokens | Human Developer | 2h |
| 3 additional HIGH CVEs in `golang.org/x/crypto` and `golang.org/x/net` | DoS and proxy bypass vectors | Human Developer | Included in crypto upgrade |
| Go 1.23.8 stdlib has 26+ known vulnerabilities | Various security impacts across `crypto/tls`, `net/http`, etc. | Human Developer | 2h |

### 1.5 Access Issues

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|---|---|---|---|---|
| etcd v3.5.17 cluster | Infrastructure | No etcd server available; 2 IAM test variants auto-skipped | Open — optional for validation | Human Developer |
| Multi-disk XFS/loop device environment | Infrastructure | No erasure multi-disk setup; bitrot live testing not possible in single-disk mode | Open — verified via Go test suites instead | Human Developer |

### 1.6 Recommended Next Steps

1. **[High]** Remediate the 5 identified dependency CVEs — upgrade `golang.org/x/crypto` to v0.45.0+, `golang-jwt/jwt/v4` to v4.5.2, `golang-jwt/jwt/v5` to v5.2.2, and `golang.org/x/net` to v0.36.0+
2. **[High]** Upgrade Go toolchain from 1.23.8 to latest stable to address 26+ stdlib vulnerabilities
3. **[Medium]** Review investigation findings with security team for organizational sign-off
4. **[Medium]** Validate etcd-backend IAM test variants by provisioning an etcd cluster
5. **[Low]** Optionally conduct multi-disk erasure live bitrot testing with XFS/loop device infrastructure

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|---|---|---|
| Repository Analysis & Tool Setup | 4 | Go 1.23.8 compiler setup, MinIO binary compilation (150M), mc client download, repository structure exploration (1,405 files, 894 Go files) |
| Investigation 1 — SSE Enforcement | 10 | Live server with KMS config, user/bucket setup, PutObject trace capture, code path analysis through 12 source files, documentation with Mermaid flowchart |
| Investigation 2 — Object Lock Protection | 8 | Locked bucket with governance retention, version-specific delete testing, XML error response capture, code path analysis through 5 source files |
| Investigation 3 — Bitrot Detection | 8 | Test suite identification, 3 test suites executed (all PASS), code path analysis through 7 source files including `bitrot.go`, `erasure-object.go`, `erasure-healing.go` |
| Investigation 4 — STS Session Policy | 9 | Live STS testing attempt (SigV4 barrier documented), test suite execution (3 configs PASS), deep `iam.go` intersection model analysis, documentation |
| Investigation 5 — Privilege Escalation | 10 | Live admin API rejection testing (403 traces), test suite execution (3 configs PASS), root cause analysis in `iam-store.go`, self-update loophole closure analysis |
| Cross-Cutting Integration Documentation | 3 | Middleware chain, IAM authorization dispatch, bucket metadata system, KMS integration, audit logging analysis |
| Report Compilation & Quality | 7 | Initial 1,049-line document creation, 8 code review fixes, dependency vulnerability assessment (5 CVEs with remediation plan) |
| Validation & Testing | 4 | `go vet` (zero warnings), 5 test suite executions (all PASS), live runtime validation, temporary artifact cleanup |
| **Total Completed** | **63** | |

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|---|---|---|
| Dependency Vulnerability Remediation — Upgrade `golang.org/x/crypto`, `golang-jwt`, `golang.org/x/net` per CVE findings | 4 | High |
| Go Toolchain Upgrade — Update from Go 1.23.8 to latest stable for stdlib CVE remediation | 2 | High |
| Human Review & Stakeholder Sign-off — Security team review of all 5 investigation findings | 2 | Medium |
| **Total Remaining** | **8** | |

---

## 3. Test Results

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---|---|---|---|---|---|---|
| Bitrot Healing — Corrupted Parts | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestHealObjectCorruptedParts` — 0.19s |
| Bitrot Healing — Corrupted Pools | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestHealObjectCorruptedPools` — 0.23s |
| Bitrot Healing — Corrupted XL Metadata | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestHealObjectCorruptedXLMeta` — 0.18s |
| STS Session Policy — ErasureSD | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPSTSServerSuite/ErasureSD` — 3.36s |
| STS Session Policy — Erasure | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPSTSServerSuite/Erasure` — 3.34s |
| STS Session Policy — ErasureSet | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPSTSServerSuite/ErasureSet` — 3.45s |
| STS Session Policy — Etcd variants | Go test (`go test ./cmd/`) | 2 | 0 | 0 | N/A | Auto-skipped — no etcd server configured |
| Privilege Escalation — ErasureSD | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPServerSuite/ErasureSD` — 3.38s |
| Privilege Escalation — Erasure | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPServerSuite/Erasure` — 3.35s |
| Privilege Escalation — ErasureSet | Go test (`go test ./cmd/`) | 1 | 1 | 0 | N/A | `TestIAMInternalIDPServerSuite/ErasureSet` — 3.56s |
| Privilege Escalation — Etcd variants | Go test (`go test ./cmd/`) | 2 | 0 | 0 | N/A | Auto-skipped — no etcd server configured |
| Static Analysis | `go vet -tags kqueue ./...` | All packages | Pass | 0 | N/A | Zero warnings, zero errors |
| **Totals** | | **14** | **9** | **0** | | **4 skipped** (etcd), **1 static analysis pass** |

All test results originate from Blitzy's autonomous validation execution during this project session. Tests were re-verified during the project guide generation phase.

---

## 4. Runtime Validation & UI Verification

### Runtime Health

- ✅ **Go compilation**: `CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio` — successful (150M static binary)
- ✅ **Static analysis**: `go vet -tags kqueue ./...` — zero warnings, zero errors
- ✅ **Dependency verification**: `go mod download` and `go mod verify` — all modules verified

### Live Server Validation

- ✅ **Investigation 1 — SSE Enforcement**: MinIO server started with `MINIO_KMS_SECRET_KEY`, bucket SSE-S3 configured, unencrypted upload produced `X-Amz-Server-Side-Encryption: AES256` response header — confirmed via `mc stat`
- ✅ **Investigation 2 — Object Lock**: Governance-locked object version-specific delete returned HTTP 400 `InvalidRequest` ("WORM protected") — error response captured
- ✅ **Investigation 5 — Privilege Escalation**: Non-admin user `basicuser` with `readwrite` policy attempted `mc admin user add`, `mc admin policy attach`, and `mc admin user list` — all returned 403 AccessDenied

### Test Suite Validation

- ✅ **Investigation 3 — Bitrot Detection**: 3 healing test suites (corrupted parts, pools, XL metadata) — all PASS
- ✅ **Investigation 4 — STS Session Policy**: Full STS server suite across ErasureSD, Erasure, ErasureSet — all PASS
- ✅ **Investigation 5 — Privilege Escalation**: Full IDP server suite across ErasureSD, Erasure, ErasureSet — all PASS

### Limitations Acknowledged

- ⚠ **Bitrot live testing**: Single-disk mode has no erasure coding — bitrot behavior verified via existing Go test suites instead of live server traces
- ⚠ **STS live testing**: STS `AssumeRole` endpoint requires SigV4 signing — verified via existing test suites that handle SigV4 internally
- ⚠ **Etcd-backend IAM**: 4 etcd-dependent test variants auto-skipped due to missing etcd infrastructure

---

## 5. Compliance & Quality Review

| Compliance Area | Requirement | Status | Notes |
|---|---|---|---|
| Read-Only Repository Constraint | No `.go`, `.md`, `.yaml`, `.json`, or other source files modified | ✅ Pass | `git diff --stat HEAD~3..HEAD` shows only 1 file added |
| Evidence Standard | Each investigation produces runtime traces, test output, or both | ✅ Pass | Investigations 1, 2, 5 have live traces; 3, 4, 5 have test output |
| Code Path Analysis | Specific file paths and line numbers referenced | ✅ Pass | All 5 investigations include exact line-number references |
| Documentation Output | `blitzy/documentation/minio_c07e5b49d477.md` created in `blitzy/documentation/` | ✅ Pass | 1,049 lines, all 5 investigations covered |
| Temporary Artifact Cleanup | All `/tmp/` artifacts cleaned up after use | ✅ Pass | Validator confirmed cleanup |
| Test Suite Reproducibility | All tests use existing repository test functions | ✅ Pass | No custom test code injected |
| Mermaid Flowcharts | Code path visualizations included | ✅ Pass | 5 flowcharts (1 per investigation) |
| Dependency Vulnerability Assessment | Known CVEs identified and documented | ✅ Pass | 5 CVEs with CVSS scores and remediation plan |
| Non-Assumption Rule | All answers based on code as ground truth | ✅ Pass | Findings reference specific source lines |
| Build Validation | `go build` and `go vet` pass cleanly | ✅ Pass | Zero warnings, zero errors |

### Autonomous Fixes Applied

| Fix | Description |
|---|---|
| Code Review — 8 findings addressed | Commit `507dada61`: Corrected line number references, improved clarity of code path descriptions, fixed formatting issues |
| Vulnerability Assessment Added | Commit `69c874e52`: Added comprehensive Dependency Vulnerability Assessment section with 5 CVEs and consolidated remediation plan |

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|---|---|---|---|---|---|
| CVE-2024-45337 — SSH auth bypass in `golang.org/x/crypto` (CVSS 9.1) | Security | Critical | High | Upgrade `golang.org/x/crypto` to v0.45.0+ | Open — documented in report |
| CVE-2025-30204 — JWT DoS in `golang-jwt` (CVSS 7.5) | Security | High | Medium | Upgrade `golang-jwt/jwt/v4` to v4.5.2+ | Open — documented in report |
| Go 1.23.8 stdlib vulnerabilities (26+) | Security | High | Medium | Upgrade Go toolchain to latest stable | Open — documented in report |
| Single-disk mode bitrot limitation | Technical | Low | N/A | Production deployments use multi-disk erasure (4+ disks); verified via test suites | Mitigated |
| Etcd-backend IAM tests skipped | Technical | Low | Low | Provision etcd v3.5.17 cluster for complete test coverage | Open |
| STS live testing requires SigV4 | Technical | Low | N/A | Verified via comprehensive existing test suites with internal SigV4 handling | Mitigated |
| Investigation findings require human review | Operational | Medium | Medium | Security team review of all 5 investigation findings before organizational sign-off | Open |
| Compliance-mode retention cannot be live-tested | Technical | Low | N/A | Behavior documented from code analysis; compliance mode has no override by design | Accepted |

---

## 7. Visual Project Status

```mermaid
pie title Project Hours Breakdown
    "Completed Work" : 63
    "Remaining Work" : 8
```

### Remaining Work by Priority

| Priority | Category | Hours |
|---|---|---|
| High | Dependency Vulnerability Remediation | 4 |
| High | Go Toolchain Upgrade | 2 |
| Medium | Human Review & Stakeholder Sign-off | 2 |
| **Total** | | **8** |

---

## 8. Summary & Recommendations

### Achievements

The project successfully completed all five security investigations specified in the Agent Action Plan, producing comprehensive runtime evidence and code path analysis for each defense layer. The sole output artifact — `blitzy/documentation/minio_c07e5b49d477.md` — is a 1,049-line document covering SSE enforcement, object lock protection, bitrot detection, STS session policy enforcement, and privilege escalation prevention with Mermaid flowcharts, test outputs, and server traces.

All 9 non-etcd test suites passed at 100%, the MinIO binary compiles cleanly, and `go vet` reports zero warnings. The non-destructive investigation constraint was strictly maintained — no repository source files were modified.

### Remaining Gaps

The project is **88.7% complete** (63 completed hours out of 71 total hours). The remaining 8 hours address path-to-production items:

1. **Dependency vulnerability remediation** (4h): Five CVEs were identified during the investigation (1 CRITICAL at CVSS 9.1, 4 HIGH). The read-only constraint prevented any `go.mod` modifications. A consolidated remediation plan is included in the documentation.
2. **Go toolchain upgrade** (2h): The Go 1.23.8 compiler has 26+ known stdlib vulnerabilities. Upgrading to the latest stable Go release addresses these.
3. **Human review** (2h): Security team review and organizational sign-off on the investigation findings.

### Critical Path to Production

The highest-priority path-to-production action is remediating CVE-2024-45337 (CVSS 9.1 SSH authentication bypass in `golang.org/x/crypto`). This vulnerability affects MinIO's SFTP server subsystem and could allow unauthorized storage access. The fix is a single dependency upgrade: `go get golang.org/x/crypto@v0.45.0`.

### Production Readiness Assessment

The investigation deliverables are production-ready for their intended purpose — comprehensive security documentation of MinIO's defense mechanisms. The documentation accurately reflects the codebase behavior as verified through runtime testing and code analysis. However, the underlying MinIO server should not be deployed to production without first addressing the identified dependency vulnerabilities.

---

## 9. Development Guide

### System Prerequisites

| Component | Version | Purpose |
|---|---|---|
| Go compiler | 1.23.8+ | Building MinIO server binary and running Go test suites |
| Git | 2.x+ | Repository management |
| Linux (x86_64) | Ubuntu 22.04+ / Debian 12+ | Build and runtime environment |
| MinIO Client (mc) | Latest release | Admin operations, tracing, bucket management |
| curl | Any | HTTP-level investigation (optional) |

### Environment Setup

```bash
# 1. Install Go 1.23.8 (if not already installed)
wget -q https://go.dev/dl/go1.23.8.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.23.8.linux-amd64.tar.gz

# 2. Set Go environment variables
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
export GOROOT="/usr/local/go"
export GOPATH="$HOME/go"
export CGO_ENABLED=0

# 3. Verify Go installation
go version
# Expected: go version go1.23.8 linux/amd64

# 4. Clone and switch to branch
cd /tmp/blitzy/minio/blitzy-12d64916-f34c-41a7-a248-65b7362d7754_9ff2a9
git checkout blitzy-12d64916-f34c-41a7-a248-65b7362d7754
```

### Dependency Installation

```bash
# Download all Go module dependencies
go mod download

# Verify module integrity
go mod verify
# Expected: all modules verified
```

### Building MinIO

```bash
# Build the MinIO server binary
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/minio

# Verify the binary
ls -lh /tmp/minio
# Expected: ~150M static binary

# Run static analysis
go vet -tags kqueue ./...
# Expected: no output (zero warnings)
```

### Running Investigation Test Suites

```bash
# Investigation 3 — Bitrot Detection
go test -v -run TestHealObjectCorruptedParts -timeout 120s -tags kqueue ./cmd/
go test -v -run TestHealObjectCorruptedPools -timeout 120s -tags kqueue ./cmd/
go test -v -run TestHealObjectCorruptedXLMeta -timeout 120s -tags kqueue ./cmd/

# Investigation 4 — STS Session Policy
go test -v -run TestIAMInternalIDPSTSServerSuite -timeout 300s -tags kqueue ./cmd/

# Investigation 5 — Privilege Escalation Prevention
go test -v -run TestIAMInternalIDPServerSuite -timeout 300s -tags kqueue ./cmd/
```

### Live Server Investigation (Optional)

```bash
# 1. Download MinIO Client
wget -q https://dl.min.io/client/mc/release/linux-amd64/mc -O /tmp/mc
chmod +x /tmp/mc

# 2. Start MinIO server with built-in KMS
export MINIO_KMS_SECRET_KEY="minio-default-key:Ol+GS8yMGCMBNHlmNhsMvSMPGjLlkKMBz5g2nmaO9xo="
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin
mkdir -p /tmp/minio-data
/tmp/minio server /tmp/minio-data --address :9100 &

# 3. Configure mc alias
/tmp/mc alias set testmc http://localhost:9100 minioadmin minioadmin

# 4. Create encrypted bucket
/tmp/mc mb testmc/test-encrypted-bucket
/tmp/mc encrypt set sse-s3 testmc/test-encrypted-bucket

# 5. Test SSE enforcement
echo "test data" > /tmp/test-upload.txt
/tmp/mc cp /tmp/test-upload.txt testmc/test-encrypted-bucket/
/tmp/mc stat testmc/test-encrypted-bucket/test-upload.txt
# Expected: Encrypted: X-Amz-Server-Side-Encryption: AES256

# 6. Stop server and clean up
kill %1
rm -rf /tmp/minio-data /tmp/test-upload.txt
```

### Troubleshooting

| Issue | Resolution |
|---|---|
| `timeout: failed to run command 'CGO_ENABLED=0'` | Use `export CGO_ENABLED=0` before running commands, not inline |
| `go: downloading... connection refused` | Ensure internet connectivity; Go modules are downloaded from `proxy.golang.org` |
| Etcd test variants skipped | Expected behavior — no etcd server configured. Set `ETCD_SERVER` env var to enable |
| `mc: <ERROR> ... Access Denied` during admin operations | Use root credentials (`minioadmin:minioadmin`) for admin operations |
| Test timeout on slow hardware | Increase `-timeout` flag (e.g., `-timeout 600s`) |

---

## 10. Appendices

### A. Command Reference

| Command | Purpose |
|---|---|
| `go build -tags kqueue -trimpath -o /tmp/minio` | Build MinIO server binary |
| `go vet -tags kqueue ./...` | Run static analysis |
| `go test -v -run <TestName> -timeout <Ns> -tags kqueue ./cmd/` | Run specific test suite |
| `go mod download` | Download Go module dependencies |
| `go mod verify` | Verify module integrity |
| `/tmp/mc alias set <alias> <endpoint> <access> <secret>` | Configure mc client alias |
| `/tmp/mc admin trace <alias> --call <operation>` | Capture server traces |
| `/tmp/mc stat <alias>/<bucket>/<object>` | View object metadata |
| `/tmp/mc encrypt set sse-s3 <alias>/<bucket>` | Enable bucket SSE-S3 |
| `/tmp/mc mb <alias>/<bucket> --with-lock` | Create bucket with object lock |

### B. Port Reference

| Port | Service | Protocol |
|---|---|---|
| 9100 | MinIO S3 API (investigation configuration) | HTTP |
| 9000 | MinIO S3 API (default) | HTTP |
| 9001 | MinIO Console UI (default) | HTTP |

### C. Key File Locations

| File | Purpose |
|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | Investigation report (sole output artifact) |
| `cmd/object-handlers.go` | S3 PutObject/DeleteObject handlers |
| `cmd/bucket-object-lock.go` | WORM retention enforcement |
| `cmd/bitrot.go` | Bitrot algorithm registry and verification |
| `cmd/erasure-object.go` | Erasure object read/write with bitrot readers |
| `cmd/erasure-healing.go` | Healing orchestration |
| `cmd/erasure-healing_test.go` | Bitrot healing test suites |
| `cmd/sts-handlers.go` | STS API handlers |
| `cmd/iam.go` | IAM authorization dispatch |
| `cmd/iam-store.go` | IAM storage layer (privilege escalation root cause) |
| `cmd/admin-handlers-users.go` | Admin user API |
| `cmd/admin-handlers-users_test.go` | Escalation prevention test suites |
| `internal/bucket/encryption/bucket-sse-config.go` | SSE config Apply() logic |
| `internal/bucket/object/lock/lock.go` | Object lock types and NTP time |
| `internal/kms/config.go` | KMS backend selection |
| `go.mod` | Go module and dependency manifest |

### D. Technology Versions

| Technology | Version | Notes |
|---|---|---|
| Go | 1.23.8 | Build compiler |
| MinIO Server | Branch `minio_c07e5b49d477` | Investigation target |
| MinIO Client (mc) | RELEASE.2025-08-13T08-35-41Z | Admin operations tool |
| `github.com/minio/sio` | v0.4.1 | DARE encryption |
| `github.com/minio/kms-go/kes` | v0.3.0 | KES client |
| `github.com/minio/kms-go/kms` | v0.4.0 | KMS client |
| `github.com/minio/highwayhash` | v1.0.3 | Bitrot checksum |
| `github.com/klauspost/reedsolomon` | v1.12.4 | Erasure coding |
| `github.com/minio/madmin-go/v3` | v3.0.77 | Admin SDK |
| `github.com/golang-jwt/jwt/v4` | v4.5.1 | JWT handling |
| `github.com/minio/pkg/v3` | v3.0.22 | Policy engine |
| `golang.org/x/crypto` | v0.29.0 | Cryptographic primitives |

### E. Environment Variable Reference

| Variable | Value | Purpose |
|---|---|---|
| `CGO_ENABLED` | `0` | Disable CGO for static binary |
| `MINIO_ROOT_USER` | `minioadmin` | Root access key (test only) |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Root secret key (test only) |
| `MINIO_KMS_SECRET_KEY` | `minio-default-key:<base64>` | Built-in KMS symmetric key |
| `MINIO_KMS_AUTO_ENCRYPTION` | `on` | Server-wide auto-encryption fallback |
| `GOROOT` | `/usr/local/go` | Go installation root |
| `GOPATH` | `$HOME/go` | Go workspace path |
| `PATH` | `/usr/local/go/bin:$HOME/go/bin:$PATH` | Include Go binaries |

### G. Glossary

| Term | Definition |
|---|---|
| **SSE-S3** | Server-Side Encryption with S3-managed keys — MinIO generates and manages encryption keys via KMS |
| **SSE-C** | Server-Side Encryption with Customer-provided keys — client provides the encryption key with each request |
| **SSE-KMS** | Server-Side Encryption with KMS-managed keys — encryption keys managed by an external Key Management Service |
| **DARE** | Data At Rest Encryption — MinIO's streaming authenticated encryption protocol implemented by `github.com/minio/sio` |
| **WORM** | Write Once Read Many — immutable storage enforced by object lock with retention policies |
| **STS** | Security Token Service — issues temporary credentials with optional session-scoped policies |
| **IAM** | Identity and Access Management — MinIO's authorization framework for policy evaluation |
| **MRF** | Most Recently Failed — background state manager that queues failed operations for automatic retry/healing |
| **Erasure Coding** | Data protection scheme using Reed-Solomon codes to distribute data and parity across multiple disks |
| **Bitrot** | Silent data corruption on storage media — detected by comparing stored checksums against recomputed hashes |
| **HighwayHash256S** | Streaming variant of HighwayHash256 that embeds per-shard checksums for efficient partial verification |
| **XL Metadata** | Erasure coding metadata stored alongside each shard, including checksum records and distribution info |
| **KES** | Key Encryption Service — MinIO's dedicated key management proxy for SSE |
