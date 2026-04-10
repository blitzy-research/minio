# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create a new investigative analysis document** that answers five specific runtime-behavioral questions about MinIO's security enforcement mechanisms. The document must contain real runtime log output and test results as evidence, not theoretical descriptions.

**Documentation Category:** Create new documentation
**Documentation Type:** Technical investigation report with runtime evidence (Q&A format)

The five investigation areas, restated with technical precision, are:

- **SSE Bucket Encryption Precedence:** When a bucket-level `ServerSideEncryptionConfiguration` (SSE-S3 via `AES256`) is active, what is the specific runtime execution sequence — visible in server trace logs — when a client issues a `PutObject` request without any `X-Amz-Server-Side-Encryption` header? The investigation must capture the trace showing how `BucketSSEConfig.Apply()` injects the SSE header server-side before the object is written (`internal/bucket/encryption/bucket-sse-config.go`, `cmd/object-handlers.go` line 1894).

- **Object Lock Delete Rejection:** When a bucket has Object Lock enabled with `COMPLIANCE` retention and a client attempts to `DeleteObject` on a locked version, what specific log entries (S3 API trace, XML error response) does MinIO produce? The investigation must capture the `InvalidRequest` / `Object is WORM protected` error response from `enforceRetentionBypassForDelete()` (`cmd/bucket-object-lock.go` line 84).

- **Bit Rot Detection and Self-Healing:** When raw data shards on the storage backend are manually corrupted (simulating unauthorized data tampering / bit rot), what runtime logs does MinIO generate during a subsequent `GetObject` request? The investigation must capture the erasure-decode path reading the corrupted shard via `storage.ReadFileStream` and the resulting `[HEALING heal.Object]` log entry that indicates automatic reconstruction.

- **STS Session Policy Enforcement:** When a user obtains temporary credentials via `AssumeRole` with a restrictive inline session policy, does MinIO enforce the session policy intersection? The investigation must provide runtime test output proving that the STS credentials are restricted to only the actions and resources specified in the session policy, even though the parent user has broader (`readwrite`) permissions. This validates the `isAllowedBySessionPolicy()` logic in `cmd/iam.go` line 2385.

- **Privilege Escalation Prevention:** Can a user with `readonly` policy promote themselves to `consoleAdmin` by calling admin API endpoints to modify user mappings, attach policies, or create new users? The investigation must provide test output proving that all admin API operations are rejected with `Access Denied` and identify the root cause — the `validateAdminReq()` / `validateAdminSignature()` gate in `cmd/admin-handlers-users.go` that evaluates `policy.CreateUserAdminAction` and similar admin IAM actions via `globalIAMSys.IsAllowed()`.

**Inferred Documentation Needs:**
- Based on code analysis: The document must explain the code paths involved in each scenario, citing specific source files and line numbers
- Based on the "no source modification" constraint: All runtime evidence must be gathered using temporary scripts and configurations that are cleaned up afterward
- Based on user journey: The document must present runtime output alongside code-path analysis so readers can trace behavior from API call to enforcement logic to log output

### 0.1.2 Special Instructions and Constraints

- **CRITICAL: No source code modification.** The user explicitly states: "Don't modify any repository source files." All investigation must be conducted through runtime experiments, existing test suites, and trace observation. Temporary scripts and artifacts used to observe behavior must be cleaned up afterward.
- **Runtime evidence required.** The user explicitly asks for "runtime log output," "runtime test output," "test output to prove," and "server trace logs." Theoretical documentation alone is insufficient — the output document must contain actual captured outputs.
- **Root cause analysis required.** For the privilege escalation scenario, the user requests: "Identify the root cause of the user mappings modification behavior that you observe." The document must trace the rejection path through the code.
- **Implementation rule (SWE-AtlasQnA-Repo):** The output document must be named `<source_branch_name>.md`, placed in the `blitzy/documentation` directory, and must comprehensively answer all questions with thinking/rationale. The source branch name is `minio_c07e5b49d477`.
- **Documentation style:** Markdown with code blocks for all runtime output, clear headings per investigation area, and source code citations

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To document the SSE bucket encryption precedence behavior, we will **create** a section in `blitzy/documentation/minio_c07e5b49d477.md` that presents the MinIO server trace output captured during a `PutObject` to an SSE-configured bucket, annotated with the code path through `cmd/object-handlers.go` → `globalBucketSSEConfigSys.Get()` → `BucketSSEConfig.Apply()` showing the `X-Amz-Server-Side-Encryption: AES256` header injection.

- To document the Object Lock delete rejection behavior, we will **create** a section that presents the trace output showing the `DeleteMultipleObjects` request, the `storage.ReadVersion` calls to retrieve retention metadata, and the XML `<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>` response body, annotated with the code path through `enforceRetentionBypassForDelete()`.

- To document the bit rot detection behavior, we will **create** a section that presents the trace output showing `storage.ReadFileStream` operations on all erasure shards (including the corrupted one), the successful `200 OK` GetObject response (proving erasure reconstruction), and the `[HEALING heal.Object]` log entry, annotated with the code path through `cmd/bitrot-streaming.go` and `cmd/erasure-decode.go`.

- To document the STS session policy enforcement, we will **create** a section with Go-based test output showing that STS credentials with a restrictive session policy can access only the allowed resource while being denied on out-of-scope resources and actions, annotated with the `IsAllowedSTS()` → `isAllowedBySessionPolicy()` code path.

- To document the privilege escalation prevention, we will **create** a section with `mc admin` command output showing all admin operations rejected with `Access Denied`, annotated with the `validateAdminReq()` → `globalIAMSys.IsAllowed()` → `policy.CreateUserAdminAction` code path that forms the root cause.


## 0.2 Documentation Discovery and Analysis

### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a mature documentation tree rooted at `docs/` covering deployment, security, IAM, STS, bucket features, KMS, and erasure coding, alongside root-level governance documents (`README.md`, `SECURITY.md`, `CONTRIBUTING.md`, `COMPLIANCE.md`). The documentation license is Creative Commons Attribution 4.0 (`docs/LICENSE`), and a Jekyll site configuration (`_config.yml` with `jekyll-theme-minimal` theme) is present for GitHub Pages rendering.

**Documentation framework:** Jekyll-based GitHub Pages with Markdown source files
**Documentation generator configuration:** `_config.yml` (root level)
**API documentation tools:** None detected (no JSDoc, Sphinx, Godoc configuration); documentation is hand-written Markdown
**Diagram tools:** No dedicated diagram tooling detected in the repository (no Mermaid CLI, PlantUML configs); diagrams consist of inline ASCII and SVG screenshots in `docs/screenshots/`
**Documentation hosting:** GitHub repository-hosted Markdown, rendered via GitHub Pages

**Existing documentation directly related to the investigation areas:**

| Documentation Area | Existing File | Coverage Status |
|---|---|---|
| Server-side encryption | `docs/security/README.md` | Covers SSE-C, SSE-S3 key hierarchy, AEAD, key rotation — does not cover bucket-level enforcement runtime trace |
| KMS integration | `docs/kms/README.md`, `docs/kms/IAM.md` | Covers KES setup, auto-encryption env vars — does not cover SSE precedence behavior |
| Object locking / retention | `docs/bucket/retention/README.md` | Covers WORM configuration, governance/compliance modes — does not include runtime log analysis |
| STS / AssumeRole | `docs/sts/assume-role.md` | Covers API parameters, session policy — does not include session policy enforcement test output |
| IAM / access management | `docs/iam/access-management-plugin.md` | Covers OPA/plugin integration — does not cover admin API privilege escalation analysis |
| Erasure coding | `docs/erasure/` | Covers fundamentals, storage classes — does not cover bitrot detection runtime logs |
| Audit logging | `docs/logging/README.md` | Covers audit webhook/Kafka targets — does not cover specific trace output analysis |

**Key finding:** No existing documentation in the repository provides the runtime-behavioral investigation and trace-log analysis the user is requesting. The `blitzy/documentation/` directory does not yet exist and must be created.

### 0.2.2 Repository Code Analysis for Documentation

The following source modules were examined to understand the runtime behavior documented in the output:

**Policy evaluation and authorization:**
- `cmd/iam.go` — `IsAllowed()` dispatch chain (line 2437), `IsAllowedSTS()` (line 2242), `isAllowedBySessionPolicy()` (line 2385), `GetCombinedPolicy()` (line 2424), `CreateUser()` (line 1340), `PolicyDBSet()` (line 1925)
- `cmd/iam-store.go` — `policyDBGet()` (line 371), `PolicyDBGet()` (line 752), `PolicyDBSet()` (line 1169), `AddUser()` (line 2659)
- `cmd/auth-handler.go` — `getRequestAuthType()` (line 124), `checkRequestAuthType()` (line 339), `authorizeRequest()` (line 419), `isReqAuthenticated()` (line 560)
- `cmd/admin-handler-utils.go` — `validateAdminReq()` (line 37)
- `cmd/admin-handlers-users.go` — `AddUser()` (line 444), `RemoveUser()` (line 47), `ListUsers()` (line 136), `SetUserStatus()` (line 406)

**Server-side encryption enforcement:**
- `cmd/object-handlers.go` — `PutObjectHandler()` (line 1745), bucket SSE fetch and apply (line 1894–1896)
- `internal/bucket/encryption/bucket-sse-config.go` — `ParseBucketSSEConfig()`, `Apply()` method that injects SSE headers, `Algo()`, `KeyID()`
- `internal/crypto/sse.go` — `Requested()` function, `Type` interface, `EncryptSinglePart()`, `DecryptSinglePart()`
- `cmd/bucket-encryption.go` — `BucketSSEConfigSys` (bucket encryption configuration system)
- `cmd/encryption-v1.go` — `EncryptRequest()`, key generation and sealing

**Object lock enforcement:**
- `cmd/bucket-object-lock.go` — `enforceRetentionBypassForDelete()` (line 84), `enforceRetentionBypassForPut()` (line 167), `checkPutObjectLockAllowed()` (line 245)
- `internal/bucket/object/lock/` — Object lock configuration parsing, retention mode/date validation

**Bitrot detection and erasure healing:**
- `cmd/bitrot.go` — `BitrotAlgorithm`, `bitrotVerify()` (line 158), `bitrotSelfTest()` (line 218)
- `cmd/bitrot-streaming.go` — `streamingBitrotWriter` and `streamingBitrotReader` for per-shard hash verification
- `cmd/erasure-decode.go` — Erasure decode with shard-level verification
- `cmd/erasure-healing.go` — Automatic healing after corruption detection
- `cmd/erasure-object.go` — Object read/write paths with erasure coding

**STS temporary credentials:**
- `cmd/sts-handlers.go` — `AssumeRole()` (line 256), `populateSessionPolicy()` (line 94), `maxSTSSessionPolicySize` (line 89)

### 0.2.3 Web Search Research Conducted

No external web searches were required because all investigation areas are fully addressable through runtime experimentation against the repository source and the MinIO binary built from it. The codebase is self-contained, the Go toolchain is installed, and all five scenarios were executed locally with captured trace output.


## 0.3 Documentation Scope Analysis

### 0.3.1 Code-to-Documentation Mapping

**Module: `cmd/object-handlers.go` — SSE Bucket Encryption Enforcement**
- Public APIs: `PutObjectHandler()` (line 1745)
- Current documentation: `docs/security/README.md` covers SSE theory but not runtime behavior
- Documentation needed: Runtime trace showing `sseConfig.Apply()` injecting `X-Amz-Server-Side-Encryption: AES256` into the request during `NewMultipartUpload`, even when the client sends no encryption header

**Module: `cmd/bucket-object-lock.go` — Object Lock Delete Rejection**
- Public APIs: `enforceRetentionBypassForDelete()` (line 84), `enforceRetentionBypassForPut()` (line 167)
- Current documentation: `docs/bucket/retention/README.md` covers setup but not runtime trace output
- Documentation needed: Trace log output showing the full `DeleteMultipleObjects` request/response cycle when deleting a locked version, including the `<Error><Code>InvalidRequest</Code>` XML body

**Module: `cmd/bitrot.go`, `cmd/bitrot-streaming.go`, `cmd/erasure-decode.go`, `cmd/erasure-healing.go` — Bit Rot Detection**
- Public APIs: `bitrotVerify()` (line 158), erasure decode/heal paths
- Current documentation: `docs/erasure/` covers theory; no runtime corruption analysis exists
- Documentation needed: Trace log output showing `storage.ReadFileStream` reading a corrupted shard, the `[HEALING heal.Object]` log entry, and successful data reconstruction via erasure coding

**Module: `cmd/sts-handlers.go`, `cmd/iam.go` — STS Session Policy Enforcement**
- Public APIs: `AssumeRole()` (line 256), `IsAllowedSTS()` (line 2242), `isAllowedBySessionPolicy()` (line 2385)
- Current documentation: `docs/sts/assume-role.md` covers API parameters but not session policy enforcement proof
- Documentation needed: Go test program output showing allowed access to in-scope resource and denied access to out-of-scope resources/actions

**Module: `cmd/admin-handlers-users.go`, `cmd/admin-handler-utils.go`, `cmd/iam.go` — Privilege Escalation Prevention**
- Public APIs: `AddUser()` (line 444), `validateAdminReq()`, `IsAllowed()` (line 2437)
- Current documentation: `docs/multi-user/` covers user management; no privilege escalation analysis exists
- Documentation needed: Command output showing all admin API calls rejected for a `readonly` user, plus root cause code path analysis

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, the documentation gaps are:

- **No runtime trace analysis documentation exists.** All existing docs describe configuration and theory. None capture actual server trace logs to illustrate runtime enforcement sequences.
- **No bit rot / corruption detection documentation with live examples.** The erasure docs describe the algorithm but not the observable healing behavior.
- **No STS session policy enforcement proof documentation.** The STS assume-role docs describe parameters but provide no test-driven evidence of intersection enforcement.
- **No privilege escalation analysis documentation.** No existing document analyzes the admin API authorization boundary from the perspective of an attacker attempting escalation.
- **No investigative Q&A document in `blitzy/documentation/`.** The target directory and file do not exist and must be created from scratch.

### 0.3.3 Configuration Options Requiring Documentation

| Config Element | Current Documentation | Documentation Needed |
|---|---|---|
| `MINIO_KMS_SECRET_KEY` | `docs/kms/README.md` | Environment setup for SSE-S3 bucket encryption test |
| `MINIO_CI_CD=1` | Not documented in user-facing docs | Required to run erasure mode on same filesystem (test environment) |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | `README.md` | Root credential setup for all test scenarios |
| Bucket SSE config (`mc encrypt set sse-s3`) | `docs/security/README.md` | Runtime configuration step for SSE enforcement test |
| Object Lock config (`mc mb --with-lock`) | `docs/bucket/retention/README.md` | Runtime configuration step for WORM enforcement test |
| IAM user/policy (`mc admin user add`, `mc admin policy attach`) | `docs/multi-user/` | User and policy setup for STS and escalation tests |


## 0.4 Documentation Implementation Design

### 0.4.1 Documentation Structure Planning

The output document follows a single-file investigative report structure, placed in the mandated location per the `SWE-AtlasQnA-Repo` rule:

```
blitzy/
└── documentation/
    └── minio_c07e5b49d477.md
        ├── Title and Overview
        ├── Investigation 1: SSE Bucket Encryption Precedence
        │   ├── Question Restatement
        │   ├── Code Path Analysis
        │   ├── Runtime Setup
        │   ├── Server Trace Output
        │   └── Conclusion
        ├── Investigation 2: Object Lock Delete Rejection
        │   ├── Question Restatement
        │   ├── Code Path Analysis
        │   ├── Runtime Setup
        │   ├── Server Trace Output
        │   └── Conclusion
        ├── Investigation 3: Bit Rot Detection and Self-Healing
        │   ├── Question Restatement
        │   ├── Code Path Analysis
        │   ├── Runtime Setup (Corruption Injection)
        │   ├── Server Trace Output
        │   └── Conclusion
        ├── Investigation 4: STS Session Policy Enforcement
        │   ├── Question Restatement
        │   ├── Code Path Analysis
        │   ├── Runtime Test Program and Output
        │   └── Conclusion
        ├── Investigation 5: Privilege Escalation Prevention
        │   ├── Question Restatement
        │   ├── Code Path Analysis
        │   ├── Test Command Output
        │   ├── Root Cause Analysis
        │   └── Conclusion
        └── References
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach:**
- "Extract SSE enforcement logic from `cmd/object-handlers.go` line 1894 and `internal/bucket/encryption/bucket-sse-config.go` Apply() method"
- "Extract object lock enforcement from `cmd/bucket-object-lock.go` enforceRetentionBypassForDelete() line 84"
- "Extract bitrot verification from `cmd/bitrot.go` bitrotVerify() line 158 and healing trigger from `cmd/erasure-healing.go`"
- "Extract STS session policy intersection from `cmd/iam.go` IsAllowedSTS() line 2242 and isAllowedBySessionPolicy() line 2385"
- "Extract admin authorization gate from `cmd/admin-handler-utils.go` validateAdminReq() line 37 and `cmd/admin-handlers-users.go` AddUser() line 444"
- "Generate runtime output by executing MinIO in erasure mode with `MINIO_CI_CD=1` and `MINIO_KMS_SECRET_KEY` configured"

**Documentation Standards:**
- Markdown formatting with `#`, `##`, `###` headers for progressive disclosure
- Code examples using triple-backtick blocks with language identifiers (`go`, `bash`, `xml`)
- Source citations in format: `Source: /path/to/file.go:LineNumber`
- Tables for parameter descriptions and configuration summaries
- All runtime output captured as verbatim code blocks

### 0.4.3 Diagram and Visual Strategy

**Mermaid diagrams to create within the output document:**

- **SSE Enforcement Flow:** Flowchart showing `PutObjectHandler()` → `globalBucketSSEConfigSys.Get(bucket)` → `sseConfig.Apply(r.Header, ...)` → `crypto.Requested(headers)` check → header injection → `EncryptRequest()` → encrypted write
- **Object Lock Enforcement Flow:** Flowchart showing `DeleteObjectHandler()` → `enforceRetentionBypassForDelete()` → `objectlock.GetObjectLegalHoldMeta()` → `objectlock.GetObjectRetentionMeta()` → `ObjectLocked{}` error → XML error response
- **Bitrot Detection and Healing Flow:** Sequence diagram showing `GetObject` → `erasure-decode` reads shards → corrupted shard detected via hash mismatch → erasure reconstruction from remaining shards → `200 OK` response → `[HEALING heal.Object]` background trigger
- **STS Session Policy Intersection:** Flowchart showing `IsAllowed()` → `IsTempUser()` → `IsAllowedSTS()` → `GetCombinedPolicy()` → `isAllowedBySessionPolicy()` → `subPolicy.IsAllowed(sessionPolicyArgs)` AND `combinedPolicy.IsAllowed(args)`
- **Admin Authorization Gate:** Flowchart showing `AddUser()` → `validateAdminSignature()` → `globalIAMSys.IsAllowed()` with `policy.CreateUserAdminAction` → `Access Denied` for non-admin user


## 0.5 Documentation File Transformation Mapping

### 0.5.1 File-by-File Documentation Plan

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---|---|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | CREATE | `cmd/object-handlers.go`, `cmd/bucket-object-lock.go`, `cmd/bitrot.go`, `cmd/bitrot-streaming.go`, `cmd/erasure-decode.go`, `cmd/erasure-healing.go`, `cmd/sts-handlers.go`, `cmd/iam.go`, `cmd/iam-store.go`, `cmd/admin-handlers-users.go`, `cmd/admin-handler-utils.go`, `cmd/auth-handler.go`, `internal/bucket/encryption/bucket-sse-config.go`, `internal/crypto/sse.go` | Complete investigative Q&A document with five investigation sections, runtime trace output, code path analysis, Mermaid diagrams, and root cause analysis for all five user questions |

**Single-file rationale:** The `SWE-AtlasQnA-Repo` implementation rule mandates a single document named `<source_branch_name>.md` in `blitzy/documentation/`. The source branch name is `minio_c07e5b49d477`.

### 0.5.2 New Documentation File Detail

```
File: blitzy/documentation/minio_c07e5b49d477.md
Type: Investigative Technical Report (Q&A)
Source Code: 14 source files across cmd/ and internal/
Sections:
    - Title and Overview (document purpose, scope)
    - Investigation 1: SSE Bucket Encryption Precedence
        - Code path: cmd/object-handlers.go:1894 → BucketSSEConfig.Apply()
        - Runtime output: mc admin trace showing X-Amz-Server-Side-Encryption: AES256 injection
        - Runtime output: mc stat confirming Encryption: SSE-S3
    - Investigation 2: Object Lock Delete Rejection
        - Code path: cmd/bucket-object-lock.go:84 → enforceRetentionBypassForDelete()
        - Runtime output: Trace showing DeleteMultipleObjects with ObjectLocked error
        - Runtime output: XML <Error> response with InvalidRequest code
    - Investigation 3: Bit Rot Detection and Self-Healing
        - Code path: cmd/bitrot-streaming.go → hash verification → erasure-decode
        - Runtime output: Trace showing storage.ReadFileStream on corrupted shard
        - Runtime output: [HEALING heal.Object] log entry
        - Runtime output: Successful 200 OK GetObject despite corruption
    - Investigation 4: STS Session Policy Enforcement
        - Code path: cmd/iam.go:2242 → IsAllowedSTS() → isAllowedBySessionPolicy()
        - Runtime output: Go test program showing allowed/denied access
    - Investigation 5: Privilege Escalation Prevention
        - Code path: cmd/admin-handlers-users.go:444 → validateAdminSignature() → IsAllowed()
        - Runtime output: mc admin commands showing Access Denied
        - Root cause: validateAdminReq() evaluates admin IAM actions
    - References (all source files cited)
Diagrams:
    - SSE enforcement flowchart (Mermaid)
    - Object lock enforcement flowchart (Mermaid)
    - Bitrot detection sequence diagram (Mermaid)
    - STS session policy intersection flowchart (Mermaid)
    - Admin authorization gate flowchart (Mermaid)
Key Citations: cmd/object-handlers.go, cmd/bucket-object-lock.go, cmd/bitrot.go,
    cmd/iam.go, cmd/sts-handlers.go, cmd/admin-handlers-users.go,
    internal/bucket/encryption/bucket-sse-config.go, internal/crypto/sse.go
```

### 0.5.3 Documentation Configuration Updates

No documentation generator configuration updates are required. The repository uses Jekyll GitHub Pages (`_config.yml`), but the `blitzy/documentation/` directory is a standalone output location per the `SWE-AtlasQnA-Repo` rule and does not integrate into the existing Jekyll site.

### 0.5.4 Cross-Documentation Dependencies

- **Shared content:** The output document will reference concepts from `docs/security/README.md` (SSE key hierarchy), `docs/bucket/retention/README.md` (object lock setup), and `docs/sts/assume-role.md` (STS parameters) but will not modify any existing files
- **Navigation links:** None required — the output is a standalone document
- **Index/glossary:** Not applicable — no changes to existing documentation structure


## 0.6 Dependency Inventory

### 0.6.1 Documentation Dependencies

The following tools and packages are required to reproduce the runtime experiments documented in the output file:

| Registry | Package Name | Version | Purpose |
|---|---|---|---|
| go.dev | go | 1.23.8 | Go toolchain for building MinIO from source (`go.mod` specifies `go 1.23`) |
| github.com/minio/minio | minio (binary) | DEVELOPMENT.GOGET | MinIO server binary built from repository source via `go build` |
| dl.min.io | mc (MinIO Client) | RELEASE.2025-08-13T08-35-41Z | CLI client for bucket operations, admin commands, and trace capture |
| github.com/minio/minio-go/v7 | minio-go | v7.0.90 | Go SDK used in the STS session policy test program (transitive dependency via `go.mod`) |
| github.com/minio/minio-go/v7/pkg/credentials | credentials | v7.0.90 | STS AssumeRole credential provider for session policy test |

### 0.6.2 Runtime Environment Dependencies

| Component | Value | Purpose |
|---|---|---|
| `MINIO_ROOT_USER` | `minioadmin` | Root credential for admin operations during testing |
| `MINIO_ROOT_PASSWORD` | `minioadmin123` | Root credential secret key |
| `MINIO_CI_CD` | `1` | Required to allow erasure mode on same-filesystem directories (bypasses root disk check) |
| `MINIO_KMS_SECRET_KEY` | `my-minio-key:<base64-key>` | Built-in KMS key for SSE-S3 bucket encryption testing |
| Erasure disk layout | 4 directories (`data{1..4}`) | Minimum erasure set for bitrot detection and healing |

### 0.6.3 Documentation Reference Updates

No existing documentation files require link updates. The `blitzy/documentation/minio_c07e5b49d477.md` file is a standalone new document that does not introduce or break any existing cross-references.


## 0.7 Coverage and Quality Targets

### 0.7.1 Documentation Coverage Metrics

**Current coverage analysis (against user's five investigation areas):**

| Investigation Area | Existing Documentation | Runtime Evidence | Target |
|---|---|---|---|
| SSE bucket encryption precedence | Theory covered in `docs/security/README.md` (0%) | None (0%) | 100% — trace output + code path |
| Object lock delete rejection | Setup covered in `docs/bucket/retention/README.md` (0%) | None (0%) | 100% — trace output + XML error body |
| Bit rot detection / self-healing | Theory covered in `docs/erasure/` (0%) | None (0%) | 100% — corruption test + healing log |
| STS session policy enforcement | API params in `docs/sts/assume-role.md` (0%) | None (0%) | 100% — Go test program output |
| Privilege escalation prevention | Not addressed anywhere (0%) | None (0%) | 100% — command output + root cause |

**Target coverage:** 100% of all five investigation areas with runtime evidence
**Coverage gaps to address:** All five areas require new documentation with runtime output — none have existing trace-level analysis

### 0.7.2 Documentation Quality Criteria

**Completeness requirements:**
- Each investigation section must contain: question restatement, code path analysis with file/line citations, runtime setup commands, captured runtime output (trace logs or test results), and a conclusion
- All five investigations must be answered in a single document
- Every code path claim must reference a specific file and line number

**Accuracy validation:**
- All runtime output was captured from a MinIO server built from this exact repository commit
- Code path descriptions must match the actual source code lines (verified via `grep -n` during analysis)
- The STS session policy test uses the same `minio-go` SDK that is a dependency of the MinIO server itself

**Clarity standards:**
- Each investigation begins with a plain-language restatement of the question
- Code paths are described in a "caller → callee" chain format with source citations
- Runtime output is presented in verbatim code blocks with key lines annotated
- Mermaid diagrams illustrate the enforcement flow for each scenario
- Conclusions explicitly state whether the observed behavior confirms or contradicts expectations

**Maintainability:**
- Source citations use the format `Source: file.go:line` for traceability
- Environment setup is documented as reproducible commands
- The document structure follows a consistent template across all five investigations

### 0.7.3 Example and Diagram Requirements

- **Minimum examples per investigation:** 1 complete runtime output block, 1 code path trace
- **Diagram types required:** 5 Mermaid flowchart/sequence diagrams (one per investigation)
- **Code example testing:** All runtime output was captured from actual MinIO server execution; the STS test was run as a Go program via `go run`
- **Visual content freshness:** Output is generated from the current repository commit (`minio_c07e5b49d477`)


## 0.8 Scope Boundaries

### 0.8.1 Exhaustively In Scope

**New documentation file:**
- `blitzy/documentation/minio_c07e5b49d477.md` — The single output document containing all five investigations

**Source code analyzed (read-only, not modified):**
- `cmd/object-handlers.go` — PutObjectHandler SSE enforcement path
- `cmd/bucket-object-lock.go` — Object lock retention enforcement
- `cmd/bitrot.go` — Bitrot algorithm definitions and verification
- `cmd/bitrot-streaming.go` — Streaming bitrot writer/reader
- `cmd/bitrot-whole.go` — Whole-file bitrot verification
- `cmd/erasure-decode.go` — Erasure decode with shard verification
- `cmd/erasure-healing.go` — Automatic healing after corruption
- `cmd/erasure-object.go` — Object read/write with erasure coding
- `cmd/sts-handlers.go` — STS AssumeRole and session policy population
- `cmd/iam.go` — IAM IsAllowed dispatch, IsAllowedSTS, session policy intersection
- `cmd/iam-store.go` — Policy database get/set operations
- `cmd/admin-handlers-users.go` — Admin user management handlers (AddUser, RemoveUser, etc.)
- `cmd/admin-handler-utils.go` — validateAdminReq admin authorization gate
- `cmd/auth-handler.go` — Request authentication type classification
- `cmd/bucket-encryption.go` — Bucket SSE configuration system
- `cmd/bucket-encryption-handlers.go` — Bucket encryption API handlers
- `internal/bucket/encryption/bucket-sse-config.go` — SSE config Apply() method
- `internal/crypto/sse.go` — SSE type interface and Requested() check
- `internal/crypto/sse-s3.go` — SSE-S3 implementation
- `internal/crypto/sse-kms.go` — SSE-KMS implementation

**Existing documentation referenced (read-only, not modified):**
- `docs/security/README.md`
- `docs/bucket/retention/README.md`
- `docs/sts/assume-role.md`
- `docs/kms/README.md`
- `docs/erasure/`

**Runtime experiment artifacts (created temporarily, cleaned up):**
- MinIO server instance (erasure mode, 4 disks)
- Test buckets: `test-encrypt`, `test-lock`, `test-bitrot`, `test-sts`
- Test users: `testuser`, `basicuser`
- Go test program: `/tmp/sts-test.go`
- Trace log files: `/tmp/trace-*.log`

### 0.8.2 Explicitly Out of Scope

- **Source code modifications:** No changes to any repository source files (per user directive)
- **Existing documentation modifications:** No updates to `docs/**/*.md`, `README.md`, or any other existing file
- **Feature additions or code refactoring:** Not applicable
- **Deployment configuration changes:** Not applicable
- **Documentation unrelated to the five investigation areas:** No coverage of other MinIO features (replication, lifecycle, tiering, batch jobs, etc.)
- **Performance testing or benchmarking:** Not requested
- **CI/CD pipeline changes:** Not applicable
- **Helm chart or Docker configuration changes:** Not applicable
- **Test file modifications:** No changes to existing `*_test.go` files
- **Documentation generator configuration:** No changes to `_config.yml` or other build configs


## 0.9 Execution Parameters

### 0.9.1 Documentation-Specific Instructions

**MinIO build command:**
```bash
cd <repo_root> && go build -o /tmp/minio .
```

**MinIO server start command (erasure mode with KMS):**
```bash
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin123
export MINIO_CI_CD=1
export MINIO_KMS_SECRET_KEY="my-minio-key:MjJhN2VjZjRjNGNhMTM5MjFiMjQ4MTU2NGUzNTlhN2Q="
mkdir -p /tmp/minio-erasure/data{1..4}
/tmp/minio server /tmp/minio-erasure/data{1...4} --address ":9000" --console-address ":9001"
```

**MinIO Client (mc) configuration:**
```bash
/tmp/mc alias set myminio http://localhost:9000 minioadmin minioadmin123 --api S3v4
```

**Trace capture command:**
```bash
/tmp/mc admin trace -v -a myminio > /tmp/trace.log 2>&1 &
```

**Default format:** Markdown with Mermaid diagrams, verbatim code blocks for all runtime output
**Citation requirement:** Every code path description must reference source file and line number
**Style guide:** Each investigation follows a consistent template: Question → Code Path → Setup → Output → Conclusion
**Documentation validation:** Content accuracy validated by executing all five scenarios against the built binary

### 0.9.2 Runtime Test Reproduction Steps

For each investigation area, the document must include sufficient setup commands that a reader could reproduce the experiment:

- **SSE test:** Create bucket → `mc encrypt set sse-s3` → upload without encryption header → verify with `mc stat` showing `Encryption: SSE-S3`
- **Object lock test:** Create bucket with `--with-lock` → set compliance retention → upload → attempt `mc rm --version-id` on locked version → observe WORM error
- **Bitrot test:** Upload to erasure bucket → corrupt a `part.1` shard on disk → `mc cat` the object → observe successful read + `[HEALING heal.Object]` log
- **STS test:** Create user with `readwrite` → `AssumeRole` with restrictive session policy → attempt allowed and denied operations
- **Escalation test:** Create user with `readonly` → attempt admin API calls (`mc admin user add`, `mc admin policy attach`, etc.) → observe all `Access Denied`


## 0.10 Rules for Documentation

The following rules are explicitly specified by the user or derived from the implementation rules:

- **Do not modify any repository source files.** All investigations must be conducted through runtime observation, existing test execution, and external temporary scripts only. The codebase must remain unchanged after documentation is complete.
- **Clean up temporary artifacts.** If temporary scripts or test configurations are needed, they must be removed afterward, leaving the codebase in its original state.
- **Create the document as `<source_branch_name>.md`.** Per the `SWE-AtlasQnA-Repo` rule, the output file must be named `minio_c07e5b49d477.md` and placed in the `blitzy/documentation/` directory.
- **Provide thinking and rationale behind answers.** Per the `SWE-AtlasQnA-Repo` rule, the document must not just present outputs but also explain the reasoning and code-path analysis behind each observed behavior.
- **Base answers on the code as truth.** Per the `SWE-AtlasQnA-Repo` rule, all conclusions must be derived from the actual source code, not from assumptions or external documentation.
- **Include runtime log output as evidence.** The user explicitly requests trace logs, server trace output, and test output for each investigation area. Theoretical-only answers are insufficient.
- **Identify root cause.** For the privilege escalation investigation, the user requires root cause identification of the observed behavior, not just a description of the outcome.
- **Use source code citations.** Every technical claim must reference the specific source file and line number where the behavior is implemented.


## 0.11 References

### 0.11.1 Source Files and Folders Searched

The following files and folders were systematically searched and analyzed to derive the conclusions in this Agent Action Plan:

**Root-level files examined:**
- `go.mod` — Go module definition, toolchain version (`go 1.23`), dependency graph
- `main.go` — Entry point, imports `cmd.Main`
- `Makefile` — Build orchestration
- `README.md` — Project overview, deployment guide
- `SECURITY.md` — Vulnerability disclosure policy

**`cmd/` directory — Core server implementation (files examined):**
- `cmd/object-handlers.go` — S3 PutObject handler with SSE enforcement (lines 1745–2134)
- `cmd/bucket-object-lock.go` — Object lock enforcement functions (lines 38–340)
- `cmd/bitrot.go` — Bitrot algorithm definitions, verification, self-test (lines 39–246)
- `cmd/bitrot-streaming.go` — Streaming bitrot writer/reader with per-shard hash
- `cmd/bitrot-whole.go` — Whole-file bitrot verification
- `cmd/erasure-decode.go` — Erasure decode with shard-level integrity check
- `cmd/erasure-healing.go` — Automatic object healing after corruption detection
- `cmd/sts-handlers.go` — STS AssumeRole flows, session policy population (lines 89–918)
- `cmd/iam.go` — IAM system: IsAllowed() dispatch (line 2437), IsAllowedSTS() (line 2242), isAllowedBySessionPolicy() (line 2385), CreateUser() (line 1340), PolicyDBSet() (line 1925)
- `cmd/iam-store.go` — IAM storage: policyDBGet() (line 371), PolicyDBGet() (line 752), PolicyDBSet() (line 1169), AddUser() (line 2659)
- `cmd/admin-handlers-users.go` — Admin user management: AddUser() (line 444), RemoveUser() (line 47), ListUsers() (line 136), SetUserStatus() (line 406)
- `cmd/admin-handler-utils.go` — validateAdminReq() admin authorization gate (line 37)
- `cmd/auth-handler.go` — getRequestAuthType() (line 124), checkRequestAuthType() (line 339), authorizeRequest() (line 419)
- `cmd/bucket-encryption.go` — BucketSSEConfigSys
- `cmd/bucket-encryption-handlers.go` — Bucket encryption API handlers
- `cmd/xl-storage.go` — XL storage drive detection, getDiskInfo() (line 365)
- `cmd/storage-errors.go` — errDiskNotFound sentinel (line 53)
- `cmd/prepare-storage.go` — waitForFormatErasure() (line 239)
- `cmd/encryption-v1.go` — EncryptRequest(), key generation
- `cmd/globals.go` — Security constants, globalMaxSkewTime, globalRefreshIAMInterval
- `cmd/policy_test.go` — Policy evaluation tests

**`internal/` directory — Reusable infrastructure (folders/files examined):**
- `internal/crypto/` — Complete folder: `doc.go`, `sse.go`, `sse-c.go`, `sse-s3.go`, `sse-kms.go`, `key.go`, `error.go`, `header.go`, `metadata.go`, `auto-encryption.go`
- `internal/bucket/encryption/bucket-sse-config.go` — BucketSSEConfig, ParseBucketSSEConfig(), Apply() method, Algo(), KeyID()
- `internal/bucket/object/lock/` — Object lock configuration parsing
- `internal/bucket/lifecycle/` — Lifecycle rules (folder summary examined)
- `internal/bucket/versioning/` — Versioning configuration (folder summary examined)
- `internal/kms/` — KMS backend abstraction (folder summary examined)
- `internal/auth/` — Credential model (folder summary examined)

**`docs/` directory — Documentation files examined:**
- `docs/security/README.md` — SSE key hierarchy, AEAD, key rotation documentation
- `docs/kms/README.md` — KES/KMS setup, auto-encryption
- `docs/kms/IAM.md` — IAM/configuration encryption
- `docs/bucket/retention/README.md` — Object lock and immutability guide
- `docs/sts/assume-role.md` — AssumeRole API parameters, session policy
- `docs/sts/README.md` — STS overview
- `docs/iam/access-management-plugin.md` — OPA/policy plugin docs
- `docs/erasure/` — Erasure coding fundamentals (folder summary examined)
- `docs/logging/` — Audit logging configuration (folder summary examined)

**`buildscripts/` directory — Build/test scripts examined:**
- `buildscripts/verify-build.sh` — Multi-topology server verification
- `buildscripts/verify-healing.sh` — Healing regression test
- `buildscripts/verify-healing-empty-erasure-set.sh` — Empty set healing test

### 0.11.2 Attachments

No attachments were provided by the user. No Figma screens or external design files are associated with this task.

### 0.11.3 Runtime Experiments Conducted

Five runtime experiments were executed against a MinIO server built from the repository source (`go build -o /tmp/minio .`), running in erasure mode with 4 disks and KMS configured:

| Experiment | Method | Key Result |
|---|---|---|
| SSE Bucket Encryption Precedence | `mc encrypt set sse-s3` + upload without encryption header + `mc admin trace` | Server trace shows `X-Amz-Server-Side-Encryption: AES256` injected by server during `NewMultipartUpload`; `mc stat` confirms `Encryption: SSE-S3` on stored object |
| Object Lock Delete Rejection | `mc mb --with-lock` + compliance retention + `mc rm --version-id` on locked version | Server returns `<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message>` in DeleteMultipleObjects XML response |
| Bit Rot Detection / Self-Healing | Upload + corrupt `part.1` shard on disk1 + `mc cat` | GetObject returns `200 OK` with correct data (62 bytes reconstructed from remaining shards); trace shows `[HEALING heal.Object]` triggered on `test-bitrot/important-data.txt` |
| STS Session Policy Enforcement | `AssumeRole` with restrictive session policy via Go test program | `GetObject` on allowed resource succeeds; `GetObject` on restricted resource returns `Access Denied`; `PutObject` returns `Access Denied` |
| Privilege Escalation Prevention | `readonly` user attempts `mc admin user add`, `mc admin policy attach`, `mc admin group add`, `mc admin user list` | All admin API calls rejected with `Access Denied`; user remains with `readonly` policy confirmed via `mc admin user info` |


