# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification


### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create a new comprehensive security-analysis document** that answers a focused, investigative question about MinIO's IAM policy enforcement under concurrent load. The user wants a code-grounded, evidence-based audit of whether a read-only principal can bypass its policy boundary via less-obvious S3 API surface areas (multipart operations, copy-style writes, metadata mutations, delete attempts) and what metadata exposure exists through listing and HEAD operations — all validated against the actual MinIO source code in this repository.

- **Category:** Create new documentation
- **Documentation type:** Security analysis / Q&A document (runtime-evidence-backed investigation)
- **Target file:** `blitzy/documentation/minio_c07e5b49d477.md` (per the implementation rule: `<source_branch_name>.md` in `blitzy/documentation/`)

The documentation requirements, restated with enhanced clarity:

- **R1 — Read-only policy boundary analysis:** Map every S3 API handler in the MinIO codebase to the IAM policy action it checks, and determine which operations a read-only principal (`s3:GetObject`, `s3:GetBucketLocation`, `s3:ListBucket`) can and cannot perform, citing the exact source file and line where each check occurs.
- **R2 — Mutation-adjacent attack surface inventory:** Enumerate the "write-adjacent" operations the user specifically called out — multipart initiation/upload/complete/abort, CopyObject, CopyObjectPart, PutObjectTagging, DeleteObjectTagging, PutObjectRetention, PutObjectLegalHold, DeleteObject, PostRestoreObject — and document the policy action each requires, proving that a read-only policy would be denied.
- **R3 — Metadata leakage assessment:** Document what information a read-only principal can learn from `HeadObject`, `HeadBucket`, `GetBucketLocation`, `ListObjectsV1/V2`, `ListObjectVersions`, `GetObjectAttributes`, `SelectObjectContent`, and `GetObjectTagging`, including the metadata fields returned in headers and the policy actions gating each.
- **R4 — Concurrency stress reproduction design:** Describe a minimal reproduction scenario where one identity is restricted to read-only access on a specific bucket and prefix, while other clients generate heavy write and metadata traffic on the same bucket, to test whether the boundary holds under load.
- **R5 — Request/response trace evidence:** Provide the expected structure of request and response traces (HTTP status codes, S3 error codes, relevant headers) for each test case, showing concrete evidence of allow or deny.
- **R6 — Storage side-effect verification:** Define how to verify that no storage-side mutations occurred (e.g., object counts, version lists, tag states before and after the test).
- **R7 — Temporary script cleanup:** All observation and test scripts are temporary and must be cleaned up after execution; the repository itself must remain unchanged.

### 0.1.2 Special Instructions and Constraints

- **Repository immutability:** The implementation rule states: *"Do not modify any existing files in the source repository."* All output is a single new Markdown document placed in `blitzy/documentation/`.
- **Code-as-truth principle:** The implementation rule states: *"Do not make assumptions, base your answers on the code as the truth."* Every claim about policy enforcement must cite the specific Go source file, function name, and the `policy.Action` constant used in the `checkRequestAuthType` or `isPutActionAllowed` call.
- **Thinking and rationale:** The implementation rule states: *"Provide thinking / rationale behind the answers."* The document must explain not just what happens, but why — tracing the authorization flow from the middleware chain through `checkRequestAuthType` → `authenticateRequest` → `authorizeRequest` → `IAMSys.IsAllowed()`.
- **Temporary scripts:** The user explicitly requested that temporary scripts may be used for observation but must be cleaned up afterward. The document should describe the scripts conceptually and provide inline code blocks, but no persistent script files should be created in the repository.
- **Stress-testing context:** The user specifically asked for concurrent write and metadata traffic while the read-only identity attempts mutations. The document must address whether concurrency or server stress could affect policy enforcement timing or consistency.
- **Style preference:** Analytical, evidence-first, security-audit tone. Concrete runtime evidence via request/response traces. Tables for API-to-action mapping.

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To **document the policy enforcement boundary** (R1, R2), we will **create** a comprehensive API-to-policy-action mapping table by extracting every `checkRequestAuthType(ctx, r, policy.<Action>, ...)` call from `cmd/object-handlers.go`, `cmd/object-multipart-handlers.go`, `cmd/bucket-handlers.go`, `cmd/bucket-listobjects-handlers.go`, and related handler files.
- To **assess metadata leakage** (R3), we will **document** the response structures returned by HeadObject, ListObjects, and GetObjectAttributes, cross-referenced against the policy actions that gate access, drawing from `cmd/object-handlers.go` (HeadObjectHandler, GetObjectAttributesHandler) and `cmd/bucket-listobjects-handlers.go`.
- To **design the stress reproduction** (R4), we will **describe** a MinIO deployment with two identities — one root/admin and one read-only — using `mc admin user add` and `mc admin policy attach` with a custom read-only policy scoped to a specific bucket and prefix, with concurrent write traffic generated by a background script.
- To **produce trace evidence** (R5, R6), we will **document** the expected HTTP status codes (403 AccessDenied for denied operations, 200/204 for allowed operations) and the specific S3 error codes returned by the error classification in `cmd/api-errors.go`.
- To **address concurrency safety** (R4 stress context), we will **analyze** the authorization middleware pipeline defined in `cmd/routers.go` (the nine-handler chain) and explain that policy evaluation occurs synchronously within the request goroutine before any storage operation begins, making it immune to concurrent load bypass.

### 0.1.4 Inferred Documentation Needs

Based on code analysis of the MinIO repository:

- **Inferred need — Authorization pipeline deep dive:** The document must explain the three-gate authorization model (Gate 1: `setAuthMiddleware`, Gate 2: `checkRequestAuthType` → `IAMSys.IsAllowed()`, Gate 3: bucket policy for anonymous callers), as this is the foundational architecture that makes the read-only boundary robust. Source: `cmd/auth-handler.go` lines 339–500, `cmd/iam.go` lines 2437–2483.
- **Inferred need — Deny-by-default posture:** The document should highlight that MinIO uses a deny-by-default authorization posture — if no policy grants access, the request is denied. This is the `IAMSys.IsAllowed()` fallback behavior in `cmd/iam.go`.
- **Inferred need — Canned policy mapping:** MinIO ships with built-in `readonly`, `writeonly`, and `readwrite` canned policies (documented in `docs/multi-user/README.md`). The `readonly` policy grants exactly: `s3:GetBucketLocation`, `s3:ListBucket`, `s3:GetObject`. The document should compare this against the full set of policy actions used across all handlers.
- **Inferred need — Object Lock interaction:** Operations like `PutObjectRetention` and `PutObjectLegalHold` have their own dedicated policy actions (`policy.PutObjectRetentionAction`, `policy.PutObjectLegalHoldAction`) checked in `cmd/object-handlers.go`. These are separate from the read-only grant and will be denied.
- **Inferred need — SelectObjectContent gateway:** `SelectObjectContent` is gated by `policy.GetObjectAction` (in `cmd/object-handlers.go` line 139), meaning a read-only principal CAN use S3 Select — this is a metadata/content access vector worth documenting.


## 0.2 Documentation Discovery and Analysis


### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a mature Markdown-driven documentation tree under `docs/` with over 30 subfolders, no documentation generator framework (no `mkdocs.yml`, `docusaurus.config.js`, or `sphinx/conf.py` present), and Jekyll site configuration (`_config.yml` at root with `jekyll-theme-minimal`). The documentation is organized as standalone Markdown files with inline diagrams and code examples.

- **Current documentation framework:** None (standalone Markdown files served via GitHub/Jekyll)
- **Documentation generator configuration:** `_config.yml` at repository root (Jekyll theme configuration)
- **API documentation tools in use:** None (no JSDoc, Godoc generation, or Sphinx). API documentation is manual Markdown.
- **Diagram tools detected:** None configured; Mermaid is available for inline Markdown diagrams
- **Documentation hosting/deployment:** GitHub repository with Jekyll theming

**Key existing documentation relevant to this task:**

| Documentation File | Relevance | Coverage Status |
|---|---|---|
| `docs/iam/access-management-plugin.md` | Describes external authorization plugin webhook protocol | Complete for plugin flow; no read-only policy analysis |
| `docs/iam/opa.md` | OPA integration tutorial with deny-PutObject example | Complete for OPA; no native IAM policy boundary analysis |
| `docs/iam/policies/` | JSON policy fixtures for SSE-KMS enforcement testing | Narrow scope — SSE-KMS deny only |
| `docs/security/README.md` | SSE encryption model, key hierarchy, DARE protocol | Complete for encryption; no IAM policy content |
| `docs/multi-user/README.md` | Multi-user quickstart with `readonly`, `writeonly`, `readwrite` canned policies | Documents policy attachment but not policy boundary behavior |
| `docs/minio-limits.md` | Server limits, unsupported S3 APIs, multipart constraints | Lists API surface area and limits but no policy enforcement detail |
| `SECURITY.md` | Vulnerability disclosure policy | Process only — not technical security analysis |

**Critical finding:** No existing documentation in the repository systematically maps S3 API handlers to IAM policy actions, and no document analyzes the boundary behavior of a read-only policy under stress. This is a novel documentation artifact.

### 0.2.2 Repository Code Analysis for Documentation

The following source areas were examined to extract the code-level evidence needed for this documentation:

**Search patterns used:**
- Policy action checks: `grep -n "checkRequestAuthType\|isPutActionAllowed\|authenticateRequest\|authorizeRequest" cmd/*-handlers.go`
- IAM authorization dispatch: `cmd/iam.go` lines 2437–2483 (`IsAllowed` method)
- Auth handler pipeline: `cmd/auth-handler.go` lines 339–615 (`checkRequestAuthType`, `authenticateRequest`, `authorizeRequest`)
- Read-only policy definition: `cmd/policy_test.go` lines 146–165 (`getReadOnlyStatement` function)
- Canned policy documentation: `docs/multi-user/README.md`
- Multipart handlers: `cmd/object-multipart-handlers.go` — `NewMultipartUploadHandler` (line 63), `CopyObjectPartHandler` (line 247), `PutObjectPartHandler` (line 583), `CompleteMultipartUploadHandler` (line 908), `AbortMultipartUploadHandler` (line 1118), `ListObjectPartsHandler` (line 1162)
- Object handlers: `cmd/object-handlers.go` — `GetObjectHandler`, `HeadObjectHandler` (line 1009), `CopyObjectHandler` (line 1157), `PutObjectHandler`, `DeleteObjectHandler` (line 2528), `PutObjectTaggingHandler` (line 3151), `DeleteObjectTaggingHandler` (line 3301), `GetObjectTaggingHandler` (line 3048), `PutObjectLegalHoldHandler` (line 2718), `GetObjectLegalHoldHandler` (line 2811), `PutObjectRetentionHandler`, `GetObjectRetentionHandler` (line 2976), `PostRestoreObjectHandler` (line 3341), `SelectObjectContentHandler` (line 104), `GetObjectAttributesHandler` (line 581)
- Bucket handlers: `cmd/bucket-handlers.go` — `HeadBucketHandler` (line 1644), `GetBucketLocationHandler` (line 204), `ListMultipartUploadsHandler` (line 251), `DeleteMultipleObjectsHandler` (line 416), `DeleteBucketHandler` (line 1689)
- Listing handlers: `cmd/bucket-listobjects-handlers.go` — `ListObjectsV1Handler`, `ListObjectsV2Handler`, `ListObjectVersionsHandler`
- Bucket feature handlers: `cmd/bucket-encryption-handlers.go`, `cmd/bucket-lifecycle-handlers.go`, `cmd/bucket-notification-handlers.go`, `cmd/bucket-replication-handlers.go`, `cmd/bucket-versioning-handler.go`, `cmd/bucket-policy-handlers.go`

**Key directories examined:**
- `cmd/` — All handler files, auth-handler, IAM, policy evaluation, API router
- `internal/config/policy/` — OPA and plugin policy engine integration
- `internal/auth/` — Credential model and constant-time comparison
- `docs/iam/` — IAM documentation, plugin samples, policy fixtures
- `docs/security/` — Encryption security model
- `docs/multi-user/` — Multi-user quickstart with canned policies
- `buildscripts/` — IAM-related test scripts (e.g., `disable-root.sh`, `multipart-quorum-test.sh`)

### 0.2.3 Web Search Research Conducted

No web search was required for this task. All conclusions are derived directly from the MinIO source code, which is the authoritative truth per the implementation rules. The codebase contains complete handler-to-policy-action mappings, the authorization pipeline implementation, and the canned policy definitions needed to answer the user's question comprehensively.


## 0.3 Documentation Scope Analysis


### 0.3.1 Code-to-Documentation Mapping

The following modules require documentation analysis to construct the security analysis document:

**Module: `cmd/auth-handler.go` — Authorization Pipeline**
- Public APIs: `checkRequestAuthType()` (line 339), `authenticateRequest()` (line 358), `authorizeRequest()` (line 419), `checkRequestAuthTypeCredential()` (line 523), `isPutActionAllowed()` (line 749), `checkAdminRequestAuth()` (line 189)
- Current documentation: No existing document maps these functions to S3 API enforcement
- Documentation needed: Complete walkthrough of the three-gate authorization model with code citations

**Module: `cmd/iam.go` — IAM Policy Evaluation Engine**
- Public APIs: `IAMSys.IsAllowed()` (lines 2437–2483), `IsAllowedSTS()`, `IsAllowedServiceAccount()`
- Current documentation: Architecture is described in tech spec Section 6.4.2 but no runtime boundary analysis exists
- Documentation needed: Explanation of the deny-by-default posture and dispatch chain for read-only credentials

**Module: `cmd/object-handlers.go` — Object API Handlers**
- Endpoints: `GET /bucket/object`, `HEAD /bucket/object`, `PUT /bucket/object` (copy), `PUT /bucket/object` (direct), `DELETE /bucket/object`, `PUT /bucket/object?tagging`, `DELETE /bucket/object?tagging`, `GET /bucket/object?tagging`, `PUT /bucket/object?legal-hold`, `GET /bucket/object?legal-hold`, `PUT /bucket/object?retention`, `GET /bucket/object?retention`, `POST /bucket/object?restore`, `POST /bucket/object?select`, `GET /bucket/object?attributes`
- Current documentation: No per-handler policy action mapping exists
- Documentation needed: Table mapping each handler to its required policy action, with source line citations

**Module: `cmd/object-multipart-handlers.go` — Multipart Upload Handlers**
- Endpoints: `POST /bucket/object?uploads` (initiate), `PUT /bucket/object?partNumber&uploadId` (upload part), `PUT /bucket/object?partNumber&uploadId` (copy part), `POST /bucket/object?uploadId` (complete), `DELETE /bucket/object?uploadId` (abort), `GET /bucket/object?uploadId` (list parts)
- Current documentation: No policy action mapping exists for multipart handlers
- Documentation needed: Explicit mapping showing all multipart operations require `PutObjectAction` or `AbortMultipartUploadAction`, none of which are in the read-only grant

**Module: `cmd/bucket-handlers.go` — Bucket-Level Handlers**
- Endpoints: `HEAD /bucket`, `GET /bucket?location`, `GET /bucket?uploads` (list multipart uploads), `POST /bucket?delete` (multi-delete), `DELETE /bucket`
- Current documentation: No policy enforcement mapping
- Documentation needed: Show which bucket-level operations are accessible with `ListBucketAction` and `GetBucketLocationAction`

**Module: `cmd/bucket-listobjects-handlers.go` — Object Listing Handlers**
- Endpoints: `GET /bucket` (ListObjectsV1), `GET /bucket?list-type=2` (ListObjectsV2), `GET /bucket?versions` (ListObjectVersions)
- Current documentation: No policy enforcement mapping
- Documentation needed: Show that listing requires `ListBucketAction` (granted by read-only policy) and what metadata fields are exposed

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, documentation gaps include:

- **No existing handler-to-policy-action matrix:** No document in the repository systematically lists which `policy.Action` constant each handler checks. The tech spec Section 6.4 documents the architectural pattern but does not enumerate per-handler mappings.
- **No read-only boundary analysis:** The canned `readonly` policy is mentioned in `docs/multi-user/README.md` but its effective boundary is never tested or documented against the full S3 API surface.
- **No concurrency safety analysis for authorization:** No document explains that the authorization check is synchronous and per-request-goroutine, making it immune to race-condition bypass under load.
- **No metadata exposure analysis for read-only principals:** HeadObject returns rich metadata (ETag, Content-Type, Content-Length, Last-Modified, user-defined metadata, SSE info, replication status, version ID, tagging count) but no document catalogs what is visible to a read-only principal.
- **No multipart operation policy analysis:** Multipart operations are spread across `cmd/object-multipart-handlers.go` and their individual policy checks are not documented in any user-facing document.
- **No document linking authorization to S3 error codes:** The mapping from `ErrAccessDenied` in `cmd/api-errors.go` to the HTTP 403 response with `<Code>AccessDenied</Code>` is not documented from a consumer perspective.

All of these gaps will be addressed by the new `blitzy/documentation/minio_c07e5b49d477.md` document.


## 0.4 Documentation Implementation Design


### 0.4.1 Documentation Structure Planning

The new document `blitzy/documentation/minio_c07e5b49d477.md` will follow this structure:

```
blitzy/documentation/
└── minio_c07e5b49d477.md
    ├── Title & Executive Summary
    ├── 1. Authorization Architecture Deep Dive
    │   ├── 1.1 The Three-Gate Model
    │   ├── 1.2 IAMSys.IsAllowed() Dispatch Chain
    │   └── 1.3 Deny-by-Default Posture
    ├── 2. Read-Only Policy Definition
    │   ├── 2.1 Canned readonly Policy Actions
    │   └── 2.2 Custom Prefix-Scoped Read-Only Policy
    ├── 3. Complete S3 API Handler-to-Policy-Action Map
    │   ├── 3.1 Object Operations
    │   ├── 3.2 Multipart Upload Operations
    │   ├── 3.3 Bucket-Level Operations
    │   ├── 3.4 Metadata and Tagging Operations
    │   └── 3.5 Retention and Legal Hold Operations
    ├── 4. Mutation-Adjacent Attack Surface Analysis
    │   ├── 4.1 Multipart Operations (All Denied)
    │   ├── 4.2 Copy-Style Writes (Denied)
    │   ├── 4.3 Metadata Mutation via Tagging (Denied)
    │   ├── 4.4 Delete Operations (Denied)
    │   ├── 4.5 Retention/Legal Hold Writes (Denied)
    │   └── 4.6 PostRestoreObject (Denied)
    ├── 5. Metadata Exposure Assessment
    │   ├── 5.1 HeadObject / HeadBucket Information Leakage
    │   ├── 5.2 ListObjects Metadata Fields
    │   ├── 5.3 GetObjectAttributes Response
    │   ├── 5.4 GetObjectTagging (Read Access)
    │   └── 5.5 SelectObjectContent (Read Access)
    ├── 6. Concurrency and Stress Analysis
    │   ├── 6.1 Synchronous Per-Request Authorization
    │   ├── 6.2 IAM Cache Refresh and Consistency
    │   └── 6.3 Why Load Cannot Bypass the Boundary
    ├── 7. Minimal Reproduction Design
    │   ├── 7.1 Environment Setup
    │   ├── 7.2 Identity and Policy Configuration
    │   ├── 7.3 Background Load Generation
    │   ├── 7.4 Read-Only Boundary Test Cases
    │   └── 7.5 Side-Effect Verification
    ├── 8. Expected Request/Response Traces
    │   ├── 8.1 Allowed Operations (200/204/206)
    │   ├── 8.2 Denied Operations (403 AccessDenied)
    │   └── 8.3 Error Response XML Structure
    ├── 9. Conclusions
    └── 10. Source Citations
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach:**
- Extract policy action constants from every handler via `grep -n "checkRequestAuthType\|isPutActionAllowed" cmd/*-handlers.go` — already completed during context gathering
- Extract the read-only policy definition from `cmd/policy_test.go` lines 146–165 (`getReadOnlyStatement`) and corroborate with `docs/multi-user/README.md`
- Extract the authorization dispatch chain from `cmd/auth-handler.go` lines 339–500 and `cmd/iam.go` lines 2437–2483
- Extract metadata response fields from `cmd/object-handlers.go` HeadObjectHandler (lines 744–900) and GetObjectAttributesHandler (lines 575–700)
- Extract error response structure from `cmd/api-errors.go` (`ErrAccessDenied` mapping)

**Documentation Standards:**
- Markdown formatting with hierarchical headers (`#`, `##`, `###`)
- Mermaid diagrams for the authorization flow and test topology
- Code examples using Go source citations in the format `Source: cmd/file.go:LineNumber`
- Tables for API-to-policy-action mappings and expected test results
- Consistent terminology: "read-only principal", "policy action", "handler", "authorization gate"

### 0.4.3 Diagram and Visual Strategy

Mermaid diagrams to create within the document:

- **Authorization flow diagram:** Flowchart showing `HTTP Request → Middleware → authenticateRequest → authorizeRequest → IAMSys.IsAllowed() → Allow/Deny`, with the specific gate at which a read-only principal's mutation attempt is rejected
- **Test topology diagram:** Showing MinIO server, root identity (writer), read-only identity (test subject), background load generator, and the shared bucket with prefix
- **Policy action coverage diagram:** A visual categorization of S3 actions into "Granted by readonly" vs. "Denied by readonly" groups
- **Sequence diagram for a denied multipart initiation:** Showing the exact request/response exchange when the read-only principal attempts `NewMultipartUpload`

### 0.4.4 Template Application

No user-provided template. The document structure follows the analytical security-audit pattern derived from the user's question structure:
- Start with architectural context (how authorization works)
- Present the evidence (handler-to-action mapping)
- Analyze specific attack vectors (mutation-adjacent operations)
- Provide reproduction methodology (test design)
- Show expected results (traces and side effects)
- Conclude with findings


## 0.5 Documentation File Transformation Mapping


### 0.5.1 File-by-File Documentation Plan

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---|---|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | CREATE | `cmd/auth-handler.go`, `cmd/iam.go`, `cmd/object-handlers.go`, `cmd/object-multipart-handlers.go`, `cmd/bucket-handlers.go`, `cmd/bucket-listobjects-handlers.go`, `cmd/bucket-encryption-handlers.go`, `cmd/bucket-lifecycle-handlers.go`, `cmd/bucket-notification-handlers.go`, `cmd/bucket-replication-handlers.go`, `cmd/bucket-versioning-handler.go`, `cmd/bucket-policy-handlers.go`, `cmd/api-errors.go`, `cmd/routers.go`, `cmd/globals.go`, `cmd/policy_test.go`, `docs/multi-user/README.md`, `docs/iam/opa.md`, `docs/minio-limits.md`, `docs/security/README.md` | Complete security analysis document: authorization architecture deep dive, exhaustive S3 handler-to-policy-action mapping table, mutation-adjacent attack surface analysis for multipart/copy/tagging/delete/retention operations, metadata exposure assessment for HEAD/list/attributes, concurrency stress analysis explaining synchronous per-request authorization, minimal reproduction design with identity setup and background load generation, expected request/response trace evidence showing 403 AccessDenied for all mutation attempts, storage side-effect verification methodology, and source code citations |

### 0.5.2 New Documentation Files Detail

```
File: blitzy/documentation/minio_c07e5b49d477.md
Type: Security Analysis / Q&A Document
Source Code:
  - cmd/auth-handler.go (authorization pipeline: checkRequestAuthType, authenticateRequest, authorizeRequest, isPutActionAllowed)
  - cmd/iam.go (IAMSys.IsAllowed dispatch chain, deny-by-default posture)
  - cmd/object-handlers.go (GetObject, HeadObject, CopyObject, PutObject, DeleteObject, tagging, retention, legal hold, restore, select, attributes handlers)
  - cmd/object-multipart-handlers.go (NewMultipartUpload, CopyObjectPart, PutObjectPart, CompleteMultipartUpload, AbortMultipartUpload, ListObjectParts)
  - cmd/bucket-handlers.go (HeadBucket, GetBucketLocation, ListMultipartUploads, DeleteMultipleObjects, DeleteBucket, PutBucketTagging, GetBucketTagging)
  - cmd/bucket-listobjects-handlers.go (ListObjectsV1, ListObjectsV2, ListObjectVersions)
  - cmd/bucket-encryption-handlers.go (Put/Get/Delete BucketEncryption)
  - cmd/bucket-lifecycle-handlers.go (Put/Get/Delete BucketLifecycle)
  - cmd/bucket-notification-handlers.go (Put/Get BucketNotification)
  - cmd/bucket-replication-handlers.go (Put/Get/Delete ReplicationConfiguration)
  - cmd/bucket-versioning-handler.go (Put/Get BucketVersioning)
  - cmd/bucket-policy-handlers.go (Put/Delete/Get BucketPolicy)
  - cmd/routers.go (nine-handler middleware chain definition)
  - cmd/globals.go (globalMaxSkewTime, globalRefreshIAMInterval constants)
  - cmd/api-errors.go (ErrAccessDenied error code mapping)
  - cmd/policy_test.go (getReadOnlyStatement function defining readonly policy)
  - docs/multi-user/README.md (canned readonly/writeonly/readwrite policy documentation)
  - docs/iam/opa.md (OPA policy enforcement example)
  - docs/minio-limits.md (unsupported S3 APIs, multipart limits)
  - docs/security/README.md (encryption architecture for SSE context)
Sections:
  - Title and Executive Summary (purpose, key finding: read-only boundary holds)
  - Authorization Architecture Deep Dive (three-gate model, IsAllowed chain, deny-by-default)
  - Read-Only Policy Definition (canned policy actions, custom prefix-scoped policy)
  - Complete S3 API Handler-to-Policy-Action Map (exhaustive table with source citations)
  - Mutation-Adjacent Attack Surface Analysis (multipart, copy, tagging, delete, retention, restore)
  - Metadata Exposure Assessment (HeadObject fields, ListObjects metadata, GetObjectAttributes, tags, select)
  - Concurrency and Stress Analysis (synchronous auth, IAM cache, load immunity)
  - Minimal Reproduction Design (environment, identity setup, background load, test cases, verification)
  - Expected Request/Response Traces (allowed 200/204, denied 403 with XML error body)
  - Conclusions (summary of findings, risk assessment)
  - Source Citations (all files referenced with line numbers)
Diagrams:
  - Authorization flow mermaid flowchart
  - Test topology mermaid diagram
  - Denied-operation sequence diagram for multipart initiation
Key Citations:
  - cmd/auth-handler.go:339 (checkRequestAuthType)
  - cmd/auth-handler.go:358 (authenticateRequest)
  - cmd/auth-handler.go:419 (authorizeRequest)
  - cmd/auth-handler.go:523 (checkRequestAuthTypeCredential)
  - cmd/iam.go:2437-2483 (IsAllowed dispatch chain)
  - cmd/object-multipart-handlers.go:83 (NewMultipartUpload → PutObjectAction)
  - cmd/object-multipart-handlers.go:268 (CopyObjectPart → PutObjectAction on dst)
  - cmd/object-multipart-handlers.go:667 (PutObjectPart → PutObjectAction)
  - cmd/object-multipart-handlers.go:927 (CompleteMultipartUpload → PutObjectAction)
  - cmd/object-multipart-handlers.go:1118 (AbortMultipartUpload → AbortMultipartUploadAction)
  - cmd/object-multipart-handlers.go:1162 (ListObjectParts → ListMultipartUploadPartsAction)
  - cmd/object-handlers.go:139 (SelectObjectContent → GetObjectAction)
  - cmd/object-handlers.go:760 (HeadObject → GetObjectAction)
  - cmd/object-handlers.go:1173 (CopyObject → PutObjectAction on dst)
  - cmd/object-handlers.go:1206 (CopyObject → GetObjectAction on src)
  - cmd/object-handlers.go:2528 (DeleteObject → DeleteObjectAction)
  - cmd/object-handlers.go:3151 (PutObjectTagging → PutObjectTaggingAction)
  - cmd/object-handlers.go:3301 (DeleteObjectTagging → DeleteObjectTaggingAction)
  - cmd/object-handlers.go:3048 (GetObjectTagging → GetObjectTaggingAction)
  - cmd/object-handlers.go:2718 (PutObjectLegalHold → PutObjectLegalHoldAction)
  - cmd/object-handlers.go:2976 (GetObjectRetention → GetObjectRetentionAction)
  - cmd/object-handlers.go:3362 (PostRestoreObject → RestoreObjectAction)
  - cmd/bucket-handlers.go:1658 (HeadBucket → ListBucketAction)
  - cmd/bucket-handlers.go:218 (GetBucketLocation → GetBucketLocationAction)
  - cmd/policy_test.go:146-165 (getReadOnlyStatement definition)
```

### 0.5.3 Documentation Configuration Updates

No documentation generator configuration files need updating. The repository does not use mkdocs, docusaurus, sphinx, or any documentation build system. The new file is a standalone Markdown document placed in the `blitzy/documentation/` directory, which is a new directory that will be created as part of this task.

### 0.5.4 Cross-Documentation Dependencies

- **No shared content or includes:** The document is self-contained.
- **No navigation links:** No documentation navigation system exists to update.
- **No table of contents updates:** No root-level documentation index references `blitzy/documentation/`.
- **No glossary updates:** The document will define its own terminology inline.
- **Internal cross-references:** The document will reference `docs/multi-user/README.md` and `docs/iam/opa.md` for context but does not modify them.


## 0.6 Dependency Inventory


### 0.6.1 Documentation Dependencies

This documentation task produces a standalone Markdown file. No documentation generation tools are required for the document itself. However, the reproduction design described within the document references the following MinIO ecosystem tools that the reader would use to execute the test scenario:

| Registry | Package Name | Version | Purpose |
|---|---|---|---|
| Go module | `github.com/minio/minio` | commit `c07e5b49d477` | MinIO server binary under analysis (Go 1.23 toolchain) |
| Go module | `github.com/minio/minio-go/v7` | v7.0.80 (from go.mod) | MinIO Go SDK used in reproduction script examples for S3 API calls |
| Binary | `mc` (MinIO Client) | latest stable | CLI tool for user/policy management (`mc admin user add`, `mc admin policy attach`) |
| Go module | `github.com/minio/pkg/v3/policy` | v3.0.23 (from go.mod) | IAM policy action constants referenced throughout the analysis |

No additional documentation-specific packages (mkdocs, docusaurus, sphinx, typedoc, etc.) are needed because the output is a standalone Markdown file with no build step.

### 0.6.2 Documentation Reference Updates

Not applicable. No existing documentation files require link updates. The new document is placed in the `blitzy/documentation/` directory, which is an isolated output location that is not referenced by any existing document in the repository.


## 0.7 Coverage and Quality Targets


### 0.7.1 Documentation Coverage Metrics

**Current coverage analysis (before this task):**

| Coverage Area | Documented | Total | Percentage |
|---|---|---|---|
| S3 API handlers with policy action mapping | 0 | 40+ | 0% |
| Read-only policy boundary behavior | 0 | 1 (needed) | 0% |
| Mutation-adjacent operation analysis | 0 | 12 operations | 0% |
| Metadata exposure via HEAD/List | 0 | 5 endpoints | 0% |
| Concurrency safety of authorization | 0 | 1 (needed) | 0% |

**Target coverage (after this task):**

| Coverage Area | Target | Method |
|---|---|---|
| S3 API handlers with policy action mapping | 100% of handlers relevant to read-only boundary | Exhaustive `grep` of `checkRequestAuthType` and `isPutActionAllowed` across all `*-handlers.go` files |
| Read-only policy boundary behavior | Complete analysis with code citations | Trace through `checkRequestAuthType` → `authenticateRequest` → `authorizeRequest` → `IsAllowed()` for each operation |
| Mutation-adjacent operations | 12/12 operations analyzed (multipart: 6, copy: 2, tagging: 2, delete: 1, restore: 1) | Per-handler source code examination with line citations |
| Metadata exposure via HEAD/List | 5/5 endpoint response structures documented | HeadObject, HeadBucket, ListObjectsV1/V2, ListObjectVersions, GetObjectAttributes |
| Concurrency safety | Complete analysis of synchronous authorization model | Analysis of middleware chain, per-request goroutine model, and IAM cache refresh mechanism |

### 0.7.2 Documentation Quality Criteria

**Completeness requirements:**
- Every S3 API handler relevant to the user's question must be mapped to its required policy action with the exact source file and line number
- Every mutation-adjacent operation must have its denial proven by showing the `policy.Action` constant it checks, which is not in the read-only grant set
- The read-only grant set (`GetObjectAction`, `GetBucketLocationAction`, `ListBucketAction`) must be precisely defined with source citations
- The metadata exposure assessment must enumerate specific response fields (HTTP headers and XML body elements) returned by each allowed operation
- The reproduction design must be complete enough for a reader to execute without additional research

**Accuracy validation:**
- All policy action mappings must match the actual `checkRequestAuthType` calls in the handler source code
- The read-only policy definition must match `cmd/policy_test.go:146–165` (`getReadOnlyStatement`)
- Error response structure must match `cmd/api-errors.go` definitions
- IAM refresh interval must cite `cmd/globals.go` line 108 (`globalRefreshIAMInterval = 10 * time.Minute`)

**Clarity standards:**
- Security-audit analytical tone, evidence-first
- Progressive disclosure: architecture → evidence → analysis → reproduction → conclusions
- Consistent terminology: "read-only principal", "policy action", "handler function", "authorization gate", "mutation-adjacent operation"
- Every claim backed by `Source: path/to/file.go:LineNumber` citations

**Maintainability:**
- Source citations enable future verification against code changes
- Table-based mappings are easy to update when handlers change
- Mermaid diagrams use standard syntax for portability

### 0.7.3 Example and Diagram Requirements

- **Minimum code examples:** At least 3 inline code blocks showing (1) the read-only policy JSON, (2) a sample denied multipart initiation request/response, (3) an allowed HeadObject request/response
- **Diagram types required:** Authorization flow (flowchart), test topology (flowchart), denied-operation trace (sequence diagram)
- **Code example verification:** All examples are derived from actual MinIO source code patterns and S3 API specifications; no synthetic examples
- **Table requirements:** At least 3 tables: (1) complete handler-to-action mapping, (2) read-only grant set, (3) expected test results with HTTP status codes


## 0.8 Scope Boundaries


### 0.8.1 Exhaustively In Scope

**New documentation files:**
- `blitzy/documentation/minio_c07e5b49d477.md` — The sole deliverable: a comprehensive security analysis document

**Source code files analyzed (read-only, for evidence extraction):**
- `cmd/auth-handler.go` — Authorization pipeline functions
- `cmd/iam.go` — IAM policy evaluation engine
- `cmd/object-handlers.go` — Object API handlers (GET, HEAD, PUT, COPY, DELETE, tagging, retention, legal hold, restore, select, attributes)
- `cmd/object-multipart-handlers.go` — Multipart upload handlers (initiate, upload part, copy part, complete, abort, list parts)
- `cmd/bucket-handlers.go` — Bucket API handlers (HEAD, location, list uploads, multi-delete, create, delete, tagging, object lock config, policy status)
- `cmd/bucket-listobjects-handlers.go` — List objects handlers (V1, V2, versions)
- `cmd/bucket-encryption-handlers.go` — Bucket encryption config handlers
- `cmd/bucket-lifecycle-handlers.go` — Bucket lifecycle config handlers
- `cmd/bucket-notification-handlers.go` — Bucket notification config handlers
- `cmd/bucket-replication-handlers.go` — Bucket replication config handlers
- `cmd/bucket-versioning-handler.go` — Bucket versioning config handlers
- `cmd/bucket-policy-handlers.go` — Bucket policy handlers
- `cmd/api-router.go` — S3 API route definitions
- `cmd/api-errors.go` — Error code definitions and HTTP status mappings
- `cmd/routers.go` — Middleware chain definition
- `cmd/globals.go` — Security constants (time skew, IAM refresh interval)
- `cmd/policy_test.go` — Read-only policy statement definition
- `docs/multi-user/README.md` — Canned policy documentation
- `docs/iam/opa.md` — OPA integration tutorial
- `docs/iam/policies/*.json` — Policy fixture examples
- `docs/minio-limits.md` — S3 API limits and unsupported APIs
- `docs/security/README.md` — Encryption security model

**Documentation content areas:**
- Authorization architecture explanation with code citations
- S3 API handler-to-policy-action exhaustive mapping table
- Mutation-adjacent attack surface analysis (multipart, copy, tagging, delete, retention, restore)
- Metadata exposure assessment (HeadObject, HeadBucket, ListObjects, GetObjectAttributes, GetObjectTagging, SelectObjectContent)
- Concurrency/stress analysis of the authorization pipeline
- Minimal reproduction design with identity/policy setup
- Expected request/response trace structures
- Storage side-effect verification methodology
- Conclusions and risk assessment

### 0.8.2 Explicitly Out of Scope

- **Source code modifications:** No existing files in the repository will be modified (per implementation rule)
- **New Go source files:** No Go files, test files, or build scripts will be created
- **Persistent test scripts:** Any scripts described in the document are conceptual; no persistent script files will be created in the repository
- **Admin API analysis:** The user's question is focused on S3 API surface area, not MinIO Admin API (`/minio/admin/v3/`)
- **FTP/SFTP protocol analysis:** The user's question is about S3 API policy enforcement, not FTP/SFTP protocol adapters
- **Console UI authorization:** The user's question targets S3 clients, not the embedded Console UI
- **Encryption key management:** SSE configuration and KMS integration are out of scope unless they interact with policy enforcement (SSE-C TLS enforcement is noted but not the focus)
- **Bucket policy evaluation for anonymous callers:** The user's question is about an authenticated read-only identity, not anonymous/public access
- **Site replication and cross-cluster authorization:** The analysis is scoped to a single MinIO instance
- **Documentation build system changes:** No mkdocs, docusaurus, or sphinx configuration
- **Performance benchmarking:** The reproduction design tests authorization correctness, not throughput
- **External identity provider integration (LDAP, OIDC):** The reproduction uses native MinIO user accounts for simplicity


## 0.9 Execution Parameters


### 0.9.1 Documentation-Specific Instructions

- **Documentation build command:** Not applicable — output is a standalone Markdown file with no build step
- **Documentation preview command:** Any Markdown renderer (e.g., `grip minio_c07e5b49d477.md`, GitHub web UI, VS Code Markdown preview)
- **Diagram generation command:** Mermaid diagrams are embedded inline in the Markdown and rendered natively by GitHub, GitLab, and most modern Markdown viewers
- **Documentation deployment command:** Not applicable — file is committed directly to `blitzy/documentation/`
- **Default format:** Markdown with embedded Mermaid diagrams and inline code blocks
- **Citation requirement:** Every section must reference source files with `Source: path/to/file.go:LineNumber` format
- **Style guide:** Analytical security-audit tone; evidence-first; tables for structured data; progressive disclosure from architecture to evidence to conclusions
- **Documentation validation:** Manual review of all source citations against the actual codebase to verify line numbers and function names match

### 0.9.2 Content Extraction Commands

The following commands were used (and can be re-executed for verification) to extract the evidence that populates the document:

- **Handler-to-action mapping extraction:**
  `grep -n "checkRequestAuthType\|isPutActionAllowed" cmd/*-handlers.go`
- **Authorization pipeline inspection:**
  `sed -n '339,500p' cmd/auth-handler.go`
- **IAM dispatch chain:**
  `sed -n '2437,2483p' cmd/iam.go`
- **Read-only policy definition:**
  `sed -n '146,165p' cmd/policy_test.go`
- **Middleware chain definition:**
  `grep -n "globalMiddlewares" cmd/routers.go`
- **Error code definitions:**
  `grep -n "ErrAccessDenied" cmd/api-errors.go`
- **IAM refresh constant:**
  `grep -n "globalRefreshIAMInterval" cmd/globals.go`


## 0.10 Rules for Documentation


The following rules are derived from the user's explicit instructions and the project's implementation rules:

- **Do not modify any existing files in the source repository.** The only output is a new file at `blitzy/documentation/minio_c07e5b49d477.md`. No existing `.go`, `.md`, `.json`, `.yaml`, or any other file may be changed.
- **Do not make assumptions; base answers on the code as the truth.** Every claim about which policy action a handler checks must be verified against the actual `checkRequestAuthType` or `isPutActionAllowed` call in the source code, with file path and line number citation.
- **Provide thinking and rationale behind the answers.** The document must explain the "why" behind each finding — not just that an operation is denied, but the exact mechanism (which policy action is checked, why it is not in the read-only grant set, and where in the code the denial originates).
- **Temporary scripts for observation only; clean up afterward.** The reproduction design in the document describes temporary bash/Go scripts conceptually and provides inline code blocks. No persistent script files are committed to the repository. The document must include a cleanup section for any temporary artifacts.
- **The repository itself should remain unchanged.** This reinforces the first rule — the MinIO codebase is treated as a read-only reference. The document is an external artifact in the `blitzy/documentation/` directory.
- **Document file must be named `minio_c07e5b49d477.md`.** Per the implementation rule: "Create a new markdown document named `<source_branch_name>.md`" — the source branch is `minio_c07e5b49d477`.
- **Place the document in `blitzy/documentation/`.** Per the implementation rule: "Place the generated document in the `blitzy/documentation` directory in the destination repo."
- **Concrete runtime evidence in request/response traces.** The user explicitly requested "concrete runtime evidence in the form of request and response traces and observed storage side effects." The document must include example HTTP request/response pairs showing headers, status codes, and error bodies.
- **Cover mutation-adjacent S3 surface areas.** The user specifically enumerated: multipart operations, copy-style writes, metadata changes, deletes, and metadata learned from listing and HEAD. Every one of these categories must be addressed individually.
- **Address behavior under stress.** The user asked about "when the system is under stress" — the document must explain why concurrent load does not affect policy enforcement correctness.


## 0.11 References


### 0.11.1 Codebase Files and Folders Searched

The following files and folders were systematically searched and analyzed to derive the conclusions in this Agent Action Plan:

**Authorization and IAM Pipeline:**
- `cmd/auth-handler.go` — `getRequestAuthType()` (lines 102–157), `checkRequestAuthType()` (line 339), `authenticateRequest()` (line 358), `authorizeRequest()` (line 419), `checkRequestAuthTypeCredential()` (line 523), `isPutActionAllowed()` (line 749), `checkAdminRequestAuth()` (line 189)
- `cmd/iam.go` — `IAMSys` struct (lines 86–120), `IsAllowed()` dispatch chain (lines 2437–2483)
- `cmd/routers.go` — `globalMiddlewares` nine-handler chain (lines 54–81)
- `cmd/globals.go` — `globalMaxSkewTime` (line 98), `globalRefreshIAMInterval` (line 108)

**S3 Object Handlers:**
- `cmd/object-handlers.go` — `SelectObjectContentHandler` (line 104), `GetObjectHandler`, `HeadObjectHandler` (line 1009), `headObjectHandler` (line 744), `getObjectAttributesHandler` (line 581), `CopyObjectHandler` (line 1157), `PutObjectHandler`, `DeleteObjectHandler` (line 2528), `PutObjectLegalHoldHandler` (line 2718), `GetObjectLegalHoldHandler` (line 2811), `GetObjectRetentionHandler` (line 2976), `GetObjectTaggingHandler` (line 3048), `PutObjectTaggingHandler` (line 3151), `DeleteObjectTaggingHandler` (line 3301), `PostRestoreObjectHandler` (line 3341)

**S3 Multipart Handlers:**
- `cmd/object-multipart-handlers.go` — `NewMultipartUploadHandler` (line 63, checks `policy.PutObjectAction`), `CopyObjectPartHandler` (line 247, checks `policy.PutObjectAction` on dest and `policy.GetObjectAction` on source), `PutObjectPartHandler` (line 583, checks `policy.PutObjectAction`), `CompleteMultipartUploadHandler` (line 908, checks `policy.PutObjectAction`), `AbortMultipartUploadHandler` (line 1118, checks `policy.AbortMultipartUploadAction`), `ListObjectPartsHandler` (line 1162, checks `policy.ListMultipartUploadPartsAction`)

**S3 Bucket Handlers:**
- `cmd/bucket-handlers.go` — `GetBucketLocationHandler` (line 204, checks `policy.GetBucketLocationAction`), `ListMultipartUploadsHandler` (line 251, checks `policy.ListBucketMultipartUploadsAction`), `ListBucketsHandler` (line 306, checks `policy.ListAllMyBucketsAction`), `DeleteMultipleObjectsHandler` (line 416, checks `policy.DeleteObjectAction` per object), `HeadBucketHandler` (line 1644, checks `policy.ListBucketAction`), `DeleteBucketHandler` (line 1689, checks `policy.DeleteBucketAction`), `PutBucketTaggingHandler` (line 1913, checks `policy.PutBucketTaggingAction`), `GetBucketTaggingHandler` (line 1971, checks `policy.GetBucketTaggingAction`)
- `cmd/bucket-listobjects-handlers.go` — `ListObjectsV2Handler` (checks `policy.ListBucketAction`), `ListObjectsV1Handler` (checks `policy.ListBucketAction`), `ListObjectVersionsHandler` (checks `policy.ListBucketVersionsAction`)

**S3 Bucket Feature Handlers:**
- `cmd/bucket-encryption-handlers.go` — PutBucketEncryption (`policy.PutBucketEncryptionAction`), GetBucketEncryption (`policy.GetBucketEncryptionAction`), DeleteBucketEncryption (`policy.PutBucketEncryptionAction`)
- `cmd/bucket-lifecycle-handlers.go` — PutBucketLifecycle (`policy.PutBucketLifecycleAction`), GetBucketLifecycle (`policy.GetBucketLifecycleAction`), DeleteBucketLifecycle (`policy.PutBucketLifecycleAction`)
- `cmd/bucket-notification-handlers.go` — GetBucketNotification (`policy.GetBucketNotificationAction`), PutBucketNotification (`policy.PutBucketNotificationAction`)
- `cmd/bucket-replication-handlers.go` — PutReplicationConfiguration (`policy.PutReplicationConfigurationAction`), GetReplicationConfiguration (`policy.GetReplicationConfigurationAction`)
- `cmd/bucket-versioning-handler.go` — PutBucketVersioning (`policy.PutBucketVersioningAction`), GetBucketVersioning (`policy.GetBucketVersioningAction`)
- `cmd/bucket-policy-handlers.go` — PutBucketPolicy (`policy.PutBucketPolicyAction`), DeleteBucketPolicy (`policy.DeleteBucketPolicyAction`), GetBucketPolicy (`policy.GetBucketPolicyAction`)

**API Infrastructure:**
- `cmd/api-router.go` — S3 API route definitions with handler registrations
- `cmd/api-errors.go` — Error code definitions (`ErrAccessDenied` → HTTP 403)

**Policy Test Fixtures:**
- `cmd/policy_test.go` — `getReadOnlyStatement()` function (lines 146–165) defining the canonical read-only policy

**Existing Documentation:**
- `docs/multi-user/README.md` — Canned policy (`readonly`, `writeonly`, `readwrite`) documentation and user management quickstart
- `docs/iam/access-management-plugin.md` — Access management plugin webhook protocol
- `docs/iam/opa.md` — OPA integration tutorial with deny-PutObject example
- `docs/iam/policies/deny-non-sse-kms-objects.json` — SSE-KMS deny policy fixture
- `docs/iam/policies/deny-objects-with-invalid-sse-kms-key-id.json` — SSE-KMS key ID deny policy fixture
- `docs/security/README.md` — SSE encryption model (SSE-C, SSE-S3 key hierarchy, DARE protocol)
- `docs/minio-limits.md` — Server limits, unsupported S3 APIs, multipart constraints
- `SECURITY.md` — Vulnerability disclosure policy

**Build and Test Infrastructure:**
- `go.mod` — Go 1.23 toolchain, dependency versions (minio-go v7.0.80, pkg/v3 v3.0.23)
- `buildscripts/multipart-quorum-test.sh` — Multipart quorum regression test
- `buildscripts/disable-root.sh` — Root access disable/enable verification

**Repository Metadata:**
- Repository root (folder path `""`) — Full repository structure
- `cmd/` folder — Complete listing of all handler and infrastructure files
- `internal/` folder — All internal packages (auth, crypto, config, policy, etc.)
- `docs/` folder — Complete documentation tree structure
- `buildscripts/` folder — All build and test scripts

### 0.11.2 Tech Spec Sections Retrieved

- **Section 6.4 Security Architecture** — Comprehensive documentation of the authentication framework (12 auth types, middleware chain), authorization system (three-gate model, IAM dispatch chain, external policy engines), data protection (SSE modes, key hierarchy, DARE), security zone architecture, and audit/compliance controls
- **Section 4.7 S3 API Request Lifecycle** — PutObject end-to-end sequence diagram showing middleware → auth → handler → erasure → storage flow
- **Section 1.2 System Overview** — Project context, primary system capabilities, major components, deployment modes, and success criteria

### 0.11.3 Attachments and External Resources

- **No attachments provided:** The user did not provide any attachments to this project.
- **No Figma URLs:** No design files are referenced.
- **No external URLs:** All analysis is based on the repository source code.


