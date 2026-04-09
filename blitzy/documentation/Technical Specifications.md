# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create a new, comprehensive investigative documentation artifact** (`blitzy/documentation/minio_c07e5b49d477.md`) that answers a series of deep technical questions about the MinIO erasure coding layer's actual runtime behavior during drive failure scenarios. The document must be grounded in source code analysis, not marketing documentation.

- **Category**: Create new documentation
- **Documentation Type**: Technical investigation / Q&A reference document based on source code analysis

The specific documentation requirements are:

- **Write-Path Failure Behavior**: Document the exact error code and message MinIO returns to an S3 client when a drive becomes unavailable during an active write (PutObject) operation. Clarify whether the write succeeds or fails under various failure thresholds.
- **Read-Path Degraded Behavior**: Document whether objects stored before a drive failure can still be read successfully. If reads fail, specify the exact error returned. If reads succeed, explain the erasure decoding mechanism that makes this possible.
- **Healing Trigger Conditions**: Identify the specific event or condition that causes MinIO to initiate a healing operation when a drive returns online. Document the monitoring loop, disk detection mechanism, and state transitions.
- **Healing Decision Criteria**: Explain the criteria the system uses to determine that a particular object on a particular drive needs to be healed (i.e., the `shouldHealObjectOnDisk` decision function and its inputs).
- **Healing Log Output**: Catalog the specific log messages that appear during an active healing operation, including start, progress, completion, and failure messages.
- **Health Metrics Under Failure**: Identify the exact Prometheus metric names that track online versus offline drive counts, and describe their expected values before and after a drive failure event.

### 0.1.2 Special Instructions and Constraints

- **CRITICAL**: The user explicitly requires answers based on actual system behavior as evidenced by source code — "what the system actually does in these failure scenarios, not just what the documentation promises it should do."
- **CRITICAL**: "Include the actual error messages, log output, and metric values you observe when running these scenarios."
- **CRITICAL**: "The repository itself should remain unchanged and anything temporary should be cleaned up afterward."
- **Implementation Rule**: A new markdown document named `minio_c07e5b49d477.md` must be created in the `blitzy/documentation` directory that comprehensively answers the questions posed, with rationale based on the code as the source of truth. No existing files in the source repository may be modified.
- **Style**: Provide thinking/rationale behind the answers; do not make assumptions; base all answers on the code.

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To document write-path failure behavior, we will analyze `cmd/erasure-object.go` (`putObject` function), `cmd/erasure-encode.go` (`Encode` function), `cmd/erasure-errors.go` (quorum error definitions), `cmd/api-errors.go` (S3 error code mappings), and `cmd/object-api-errors.go` (error type definitions) to trace the complete error flow from a disk failure during a write to the S3 error response received by the client.
- To document read-path degraded behavior, we will analyze `cmd/erasure-object.go` (`GetObjectNInfo`, `getObjectFileInfo`, `getObjectWithFileInfo`), `cmd/erasure-decode.go` (parallel reader and reconstruction), and `cmd/erasure-metadata-utils.go` (`reduceReadQuorumErrs`, `objectQuorumFromMeta`) to determine read quorum requirements and the error returned when quorum is not met.
- To document healing triggers, we will analyze `cmd/background-newdisks-heal-ops.go` (`monitorLocalDisksAndHeal`, `healFreshDisk`, `getLocalDisksToHeal`), `cmd/erasure-sets.go` (`connectDisks`, `monitorAndConnectEndpoints`), and `cmd/global-heal.go` (`healErasureSet`) to trace the disk-reconnection-to-healing pipeline.
- To document healing criteria, we will analyze `cmd/erasure-healing.go` (`shouldHealObjectOnDisk`) to map the per-object decision tree.
- To document healing logs, we will catalog all `healingLogEvent` and `healingLogIf` calls across `cmd/background-newdisks-heal-ops.go`, `cmd/global-heal.go`, and `cmd/erasure-healing.go`.
- To document health metrics, we will analyze `cmd/metrics-v3-cluster-health.go`, `cmd/metrics-v3-system-drive.go`, and `cmd/metrics-v3-cluster-erasure-set.go` to list the exact metric names, descriptions, and their values under normal and degraded states.

### 0.1.4 Inferred Documentation Needs

Based on code analysis, the following implicit documentation needs have been identified:

- **Quorum calculation explanation**: The `objectQuorumFromMeta` function (`cmd/erasure-metadata.go:531`) reveals that readQuorum = dataBlocks and writeQuorum = dataBlocks (or dataBlocks+1 when data == parity). This fundamental concept must be documented to contextualize all error behavior.
- **Availability-optimized parity upgrade**: `cmd/erasure-object.go:1291` shows that MinIO dynamically increases parity when drives are offline, which directly affects write behavior under degraded conditions. This must be documented.
- **MRF (Most Recently Failed) subsystem**: `cmd/mrf.go` shows that partial operations (objects written with quorum but not all drives) are tracked and queued for background repair. This bridging mechanism between write success and healing must be covered.
- **Healthcheck endpoint behavior**: `cmd/healthcheck-handler.go` shows the `/minio/health/cluster` endpoint returns write quorum status and healing drive counts in response headers, which complements the Prometheus metrics view.
- **Drive reconnection loop**: `cmd/erasure-sets.go:283` (`monitorAndConnectEndpoints`) runs periodically and triggers `connectDisks`, which detects unformatted or healing drives and feeds them into the healing pipeline. This must be documented as the bridge between drive recovery and healing initiation.


## 0.2 Documentation Discovery and Analysis

### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a mature Markdown-driven documentation structure with extensive operational guides but no focused document covering the specific failure-mode behavior investigated here.

- **Documentation framework**: Static Markdown files served via Jekyll (`_config.yml` configures `jekyll-theme-minimal`) and GitHub-hosted README rendering. No dedicated documentation generator (MkDocs, Docusaurus, Sphinx) is detected.
- **Documentation generator configuration**: `_config.yml` at repository root (Jekyll configuration)
- **API documentation tools**: No automated API doc generation tools (JSDoc, Sphinx, Godoc) are configured. All documentation is manually authored Markdown.
- **Diagram tools detected**: None explicitly configured; existing docs use static images (SVG/JPG in `docs/screenshots/`). Mermaid is not currently used in the docs.
- **Documentation hosting**: GitHub-hosted repository documentation via standard Markdown rendering.

### 0.2.2 Repository Code Analysis for Documentation

The following search patterns were used to locate all code modules relevant to the user's questions:

**Erasure coding and write/read paths:**
- `cmd/erasure-object.go` — PutObject, GetObjectNInfo, getObjectFileInfo (core read/write operations)
- `cmd/erasure-encode.go` — Erasure.Encode with multiWriter quorum logic
- `cmd/erasure-decode.go` — parallelReader for erasure-coded reads
- `cmd/erasure-coding.go` — Reed-Solomon encoder/decoder initialization
- `cmd/erasure-metadata.go` — `objectQuorumFromMeta` (quorum calculation from metadata)
- `cmd/erasure-metadata-utils.go` — `reduceReadQuorumErrs`, `reduceWriteQuorumErrs`

**Error definitions and S3 error mapping:**
- `cmd/erasure-errors.go` — `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired`
- `cmd/storage-errors.go` — `errDiskNotFound`, `errFaultyDisk`, `errUnformattedDisk`, and all StorageErr types
- `cmd/object-api-errors.go` — `InsufficientReadQuorum`, `InsufficientWriteQuorum`, `RQErrType`
- `cmd/api-errors.go` — `ErrSlowDownRead` (S3 Code: `SlowDownRead`, HTTP 503), `ErrSlowDownWrite` (S3 Code: `SlowDownWrite`, HTTP 503)

**Healing mechanism:**
- `cmd/background-newdisks-heal-ops.go` — `monitorLocalDisksAndHeal`, `healFreshDisk`, `getLocalDisksToHeal`, `healingTracker`
- `cmd/erasure-healing.go` — `shouldHealObjectOnDisk`, `healObject`, `HealObject`
- `cmd/erasure-healing-common.go` — `commonTime`, `commonETags` (quorum helpers for healing decisions)
- `cmd/global-heal.go` — `healErasureSet`, background heal sequence management
- `cmd/mrf.go` — MRF (Most Recently Failed) partial operation tracking and repair

**Health and metrics:**
- `cmd/metrics-v3-cluster-health.go` — `drives_offline_count`, `drives_online_count`, `drives_count`
- `cmd/metrics-v3-system-drive.go` — Per-drive `health`, `timeout_errors_total`, `io_errors_total`, `availability_errors_total`, `offline_count`, `online_count`
- `cmd/metrics-v3-cluster-erasure-set.go` — Per-set `online_drives_count`, `healing_drives_count`, `health`, `read_tolerance`, `write_tolerance`
- `cmd/healthcheck-handler.go` — `/minio/health/cluster`, `/minio/health/cluster/read` endpoints

**Drive lifecycle and reconnection:**
- `cmd/erasure-sets.go` — `connectDisks`, `monitorAndConnectEndpoints`
- `cmd/xl-storage-disk-id-check.go` — `IsOnline`, `checkDiskStale`, `DiskInfo`
- `cmd/prepare-storage.go` — `waitForFormatErasure`, `connectLoadInitFormats`
- `cmd/logging.go` — `healingLogIf`, `healingLogEvent`, `storageLogIf`

**Existing documentation found (provides context but does not answer the user's questions):**
- `docs/erasure/README.md` — Erasure code quickstart guide; explains Reed-Solomon sharding and set sizes but does not cover failure-mode specifics
- `docs/erasure/storage-class/` — Storage class configuration for STANDARD/REDUCED_REDUNDANCY parity ratios
- `docs/metrics/README.md` — Metrics endpoint overview
- `docs/metrics/v3.md` — Metrics V3 hierarchy reference
- `docs/metrics/healthcheck/` — Health probe semantics for liveness and cluster checks
- `docs/config/` — Configuration guide including heal/scanner tuning

### 0.2.3 Web Search Research Conducted

No web search was required as the user's questions are entirely answerable from source code analysis. The user explicitly requested code-based truth over external documentation: "I want to understand what the system actually does in these failure scenarios, not just what the documentation promises it should do."


## 0.3 Documentation Scope Analysis

### 0.3.1 Code-to-Documentation Mapping

The following modules require documentation coverage to answer the user's questions:

**Module: `cmd/erasure-object.go`**
- Public APIs: `PutObject`, `GetObjectNInfo`, `GetObjectInfo`, `getObjectFileInfo`, `putObject`
- Current documentation: Partial — `docs/erasure/README.md` describes erasure coding concepts but not failure-mode behavior
- Documentation needed: Write-path quorum calculation, write failure error flow, read-path quorum verification, degraded read behavior

**Module: `cmd/erasure-encode.go`**
- Public APIs: `Erasure.Encode`, `multiWriter.Write`
- Current documentation: Missing — no documentation covers the quorum enforcement during encoding
- Documentation needed: How write quorum is enforced during data encoding, how individual disk write failures are aggregated into quorum decisions

**Module: `cmd/erasure-decode.go`**
- Public APIs: `parallelReader.Read`
- Current documentation: Missing
- Documentation needed: How reads are distributed across available disks and how missing shards are reconstructed

**Module: `cmd/erasure-errors.go`**
- Definitions: `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired`
- Current documentation: Missing — error messages are not documented
- Documentation needed: Exact error message strings and their mapping to S3 responses

**Module: `cmd/api-errors.go` + `cmd/object-api-errors.go`**
- Error mapping chain: `errErasureWriteQuorum` → `InsufficientWriteQuorum` → `ErrSlowDownWrite` → S3 `SlowDownWrite` (HTTP 503)
- Error mapping chain: `errErasureReadQuorum` → `InsufficientReadQuorum` → `ErrSlowDownRead` → S3 `SlowDownRead` (HTTP 503)
- Documentation needed: Complete error mapping from internal error to S3 client response

**Module: `cmd/background-newdisks-heal-ops.go`**
- Functions: `monitorLocalDisksAndHeal`, `healFreshDisk`, `getLocalDisksToHeal`, `healingTracker`
- Current documentation: Missing — no documentation explains the healing trigger mechanism
- Documentation needed: Monitoring interval, detection criteria, healing workflow, tracker persistence

**Module: `cmd/erasure-healing.go`**
- Functions: `shouldHealObjectOnDisk`, `healObject`, `HealObject`
- Current documentation: Missing
- Documentation needed: Per-object healing decision tree, healing process steps, audit logging

**Module: `cmd/global-heal.go`**
- Functions: `healErasureSet`, `newBgHealSequence`
- Current documentation: Missing
- Documentation needed: Erasure set scanning and per-bucket/per-object healing orchestration

**Module: `cmd/metrics-v3-cluster-health.go` + `cmd/metrics-v3-system-drive.go` + `cmd/metrics-v3-cluster-erasure-set.go`**
- Metric names: `drives_offline_count`, `drives_online_count`, `drives_count`, per-drive `health` (0/1/2), per-set `online_drives_count`, `healing_drives_count`, `read_tolerance`, `write_tolerance`
- Current documentation: `docs/metrics/v3.md` lists metrics categories but does not provide failure-scenario behavior analysis
- Documentation needed: Metric names, their expected values under normal and degraded states, and how they change when drives go offline

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, documentation gaps include:

- **Undocumented failure-mode behavior**: No existing document comprehensively covers what happens at the API level when drives fail during active I/O. `docs/erasure/README.md` says "you can lose any of the six drives and still reconstruct the data" but provides no specifics about error codes, quorum thresholds, or partial failure behavior.
- **Undocumented error flow**: The translation chain from internal Go errors (`errErasureWriteQuorum`) through `toObjectErr()` to S3 XML error responses (`SlowDownWrite`) is not documented anywhere.
- **Undocumented healing trigger pipeline**: The connection between `monitorAndConnectEndpoints` (drive reconnection), `connectDisks` (disk state detection), `monitorLocalDisksAndHeal` (healing initiation), and `healFreshDisk` (actual healing execution) is not described in any existing document.
- **Undocumented healing decision criteria**: The `shouldHealObjectOnDisk` function's decision tree — including the specific error types that trigger healing — has no documentation.
- **Undocumented healing log messages**: The exact log messages emitted by `healingLogEvent` and `healingLogIf` during healing operations are not cataloged.
- **Undocumented metric behavior under failure**: While `docs/metrics/v3.md` lists metric endpoint paths, no document describes the actual metric values expected before and after a drive failure.


## 0.4 Documentation Implementation Design

### 0.4.1 Documentation Structure Planning

The output document `blitzy/documentation/minio_c07e5b49d477.md` will be structured as a single comprehensive Q&A reference organized by the user's question themes:

```
blitzy/
└── documentation/
    └── minio_c07e5b49d477.md
        ├── Introduction and Methodology
        ├── 1. Erasure Coding Fundamentals (context)
        │   ├── Quorum Calculation
        │   └── Availability-Optimized Parity Upgrade
        ├── 2. Write-Path Behavior During Drive Failure
        │   ├── Error Code and Message Returned
        │   ├── Write Success vs. Failure Thresholds
        │   └── Error Flow Trace (code references)
        ├── 3. Read-Path Behavior for Existing Objects
        │   ├── Successful Degraded Reads
        │   ├── Read Failure Error Code
        │   └── MRF (Partial Operation Repair)
        ├── 4. Healing Mechanism
        │   ├── Trigger Event/Condition
        │   ├── Drive Reconnection Pipeline
        │   ├── Per-Object Healing Criteria
        │   └── Healing Log Messages
        ├── 5. Cluster Health Reporting
        │   ├── Prometheus Metric Names
        │   ├── Expected Values (Normal vs. Degraded)
        │   ├── Per-Drive Health Metric
        │   └── Erasure Set Health Metrics
        └── 6. Source References
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach:**
- Extract quorum formulas from `cmd/erasure-metadata.go:objectQuorumFromMeta` (line 531)
- Extract error message strings directly from `cmd/erasure-errors.go` (lines 23, 26, 29)
- Extract S3 error codes from `cmd/api-errors.go` (lines 869-878)
- Extract healing trigger logic from `cmd/background-newdisks-heal-ops.go:monitorLocalDisksAndHeal` (line 563)
- Extract healing criteria from `cmd/erasure-healing.go:shouldHealObjectOnDisk` (line 156)
- Extract log messages from all `healingLogEvent` calls across `cmd/background-newdisks-heal-ops.go` and `cmd/global-heal.go`
- Extract metric definitions from `cmd/metrics-v3-cluster-health.go`, `cmd/metrics-v3-system-drive.go`, `cmd/metrics-v3-cluster-erasure-set.go`
- Generate example metric values for a hypothetical 16-drive deployment with default parity

**Documentation Standards:**
- Markdown formatting with proper headers (# ## ### ####)
- Mermaid diagram integration using triple-backtick mermaid blocks for the healing pipeline flow
- Code references using `Source: /path/to/file.go:LineNumber` format
- Tables for metric inventories and error code mappings
- Consistent terminology following the MinIO codebase conventions (e.g., "drive" not "disk" in user-facing contexts, "erasure set" not "stripe")

### 0.4.3 Diagram and Visual Strategy

The following Mermaid diagrams will be created:

- **Write-path quorum decision flowchart**: Showing the decision from `putObject` through quorum check to either success or `SlowDownWrite` error
- **Healing trigger pipeline sequence diagram**: Showing the flow from `monitorAndConnectEndpoints` → `connectDisks` → `pushHealLocalDisks` → `monitorLocalDisksAndHeal` → `healFreshDisk` → `healErasureSet`
- **Healing per-object decision tree**: Showing the `shouldHealObjectOnDisk` logic branches
- **Metric hierarchy diagram**: Showing the relationship between cluster health, system drive, and erasure set metric groups


## 0.5 Documentation File Transformation Mapping

### 0.5.1 File-by-File Documentation Plan

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---------------------------|----------------|------------------|-----------------|
| `blitzy/documentation/minio_c07e5b49d477.md` | CREATE | `cmd/erasure-object.go`, `cmd/erasure-encode.go`, `cmd/erasure-decode.go`, `cmd/erasure-errors.go`, `cmd/api-errors.go`, `cmd/object-api-errors.go`, `cmd/erasure-metadata.go`, `cmd/erasure-metadata-utils.go`, `cmd/erasure-healing.go`, `cmd/erasure-healing-common.go`, `cmd/background-newdisks-heal-ops.go`, `cmd/global-heal.go`, `cmd/mrf.go`, `cmd/erasure-sets.go`, `cmd/xl-storage-disk-id-check.go`, `cmd/healthcheck-handler.go`, `cmd/metrics-v3-cluster-health.go`, `cmd/metrics-v3-system-drive.go`, `cmd/metrics-v3-cluster-erasure-set.go`, `cmd/prepare-storage.go`, `cmd/logging.go` | Complete investigative Q&A document covering: (1) write-path failure behavior with exact S3 error codes, (2) read-path degraded behavior with quorum analysis, (3) healing trigger pipeline with event/condition documentation, (4) per-object healing criteria from `shouldHealObjectOnDisk`, (5) catalog of healing log messages, (6) Prometheus metric names/values under normal and degraded states. Includes Mermaid diagrams for write-path flowchart, healing pipeline sequence, and healing decision tree. All claims cite source file and line number. |

### 0.5.2 New Documentation File Detail

```
File: blitzy/documentation/minio_c07e5b49d477.md
Type: Technical Investigation / Q&A Reference Document
Source Code: cmd/erasure-*.go, cmd/api-errors.go, cmd/object-api-errors.go, cmd/background-newdisks-heal-ops.go, cmd/global-heal.go, cmd/metrics-v3-*.go, cmd/healthcheck-handler.go
Sections:
    - Introduction and Methodology (scope, approach, caveats)
    - Erasure Coding Fundamentals (quorum formulas, availability-optimized parity)
    - Write-Path Behavior During Drive Failure
      - Quorum calculation: writeQuorum = dataDrives (or dataDrives+1 when data==parity)
      - Availability-optimized parity upgrade behavior
      - 50%+ offline threshold → immediate errErasureWriteQuorum
      - S3 error response: SlowDownWrite / HTTP 503
    - Read-Path Behavior for Existing Objects
      - Quorum calculation: readQuorum = dataDrives
      - Successful degraded reads via erasure reconstruction
      - Read failure: SlowDownRead / HTTP 503
      - MRF partial operation tracking
    - Healing Mechanism
      - Trigger: monitorLocalDisksAndHeal (10-second interval timer)
      - Drive detection: connectDisks → errUnformattedDisk or .healing.bin
      - Healing initiation: pushHealLocalDisks → healFreshDisk
      - Per-object criteria: shouldHealObjectOnDisk decision tree
      - Log messages: Complete catalog from code
    - Cluster Health Reporting
      - Cluster-level metrics: drives_offline_count, drives_online_count, drives_count
      - Per-drive metric: health (0=offline, 1=healthy, 2=healing)
      - Erasure set metrics: online_drives_count, healing_drives_count, health, read_tolerance, write_tolerance
      - Healthcheck endpoint behavior: /minio/health/cluster, X-Minio-Write-Quorum header
    - Source References (all files cited)
Diagrams:
    - Write-path quorum decision flowchart (Mermaid)
    - Healing trigger pipeline sequence diagram (Mermaid)
    - Per-object healing decision tree (Mermaid)
Key Citations: cmd/erasure-object.go, cmd/erasure-errors.go, cmd/api-errors.go, cmd/erasure-healing.go, cmd/background-newdisks-heal-ops.go, cmd/global-heal.go, cmd/metrics-v3-cluster-health.go, cmd/metrics-v3-system-drive.go, cmd/metrics-v3-cluster-erasure-set.go
```

### 0.5.3 Documentation Configuration Updates

No documentation configuration updates are required. The project does not use a documentation site generator that requires navigation configuration. The new file is placed in a standalone `blitzy/documentation/` directory per the implementation rules.

### 0.5.4 Cross-Documentation Dependencies

- The new document references `docs/erasure/README.md` for context on erasure coding fundamentals
- The new document references `docs/metrics/v3.md` for the metrics endpoint hierarchy
- The new document references `docs/metrics/healthcheck/README.md` for healthcheck endpoint semantics
- No table of contents, navigation, or index updates are required since the document lives in `blitzy/documentation/`, not in the repository's `docs/` tree


## 0.6 Dependency Inventory

### 0.6.1 Documentation Dependencies

No external documentation tools or packages are required for this task. The output is a single Markdown file that uses only standard Markdown syntax and Mermaid diagram notation (rendered natively by GitHub).

| Registry | Package Name | Version | Purpose |
|----------|--------------|---------|---------|
| Go module | `github.com/minio/minio` | `go 1.23` (from `go.mod`) | Source repository under analysis |
| N/A | Markdown | N/A | Output format for documentation |
| N/A | Mermaid (GitHub-native) | N/A | Diagram rendering within Markdown (no external dependency needed) |

### 0.6.2 Key Source Dependencies Analyzed

The following internal MinIO packages were analyzed to derive the documentation content:

| Package | Key Files | Relevance |
|---------|-----------|-----------|
| `cmd` (main server) | `erasure-object.go`, `erasure-encode.go`, `erasure-decode.go` | Write and read path implementations |
| `cmd` (errors) | `erasure-errors.go`, `storage-errors.go`, `api-errors.go`, `object-api-errors.go` | Error definitions and S3 error mapping |
| `cmd` (healing) | `erasure-healing.go`, `erasure-healing-common.go`, `background-newdisks-heal-ops.go`, `global-heal.go` | Healing trigger, criteria, and execution |
| `cmd` (metrics) | `metrics-v3-cluster-health.go`, `metrics-v3-system-drive.go`, `metrics-v3-cluster-erasure-set.go`, `metrics-v3.go` | Prometheus metric definitions and loader functions |
| `cmd` (health) | `healthcheck-handler.go`, `healthcheck-router.go` | HTTP healthcheck endpoints |
| `cmd` (storage) | `xl-storage-disk-id-check.go`, `erasure-sets.go`, `prepare-storage.go` | Disk health monitoring, reconnection, and format validation |
| `cmd` (infrastructure) | `logging.go`, `mrf.go`, `erasure-metadata.go`, `erasure-metadata-utils.go` | Logging subsystem, MRF partial repair, quorum utilities |
| `github.com/minio/madmin-go/v3` | (external) | Admin API types used for `HealResultItem`, `HealOpts`, `DiskIOStats` |

### 0.6.3 Documentation Reference Updates

No documentation reference or link updates are required. The new file is self-contained and does not modify or link-update any existing documentation.


## 0.7 Coverage and Quality Targets

### 0.7.1 Documentation Coverage Metrics

**Current coverage analysis of the user's questions against existing documentation:**

| Question Area | Files Relevant | Currently Documented | Coverage |
|---------------|----------------|---------------------|----------|
| Write-path error codes during drive failure | `cmd/erasure-object.go`, `cmd/erasure-encode.go`, `cmd/erasure-errors.go`, `cmd/api-errors.go` | Not documented | 0% |
| Read-path behavior for pre-existing objects | `cmd/erasure-object.go`, `cmd/erasure-decode.go`, `cmd/erasure-metadata-utils.go` | Conceptual only in `docs/erasure/README.md` | 10% |
| Healing trigger event/condition | `cmd/background-newdisks-heal-ops.go`, `cmd/erasure-sets.go` | Not documented | 0% |
| Per-object healing criteria | `cmd/erasure-healing.go` | Not documented | 0% |
| Healing log messages | `cmd/background-newdisks-heal-ops.go`, `cmd/global-heal.go`, `cmd/erasure-healing.go` | Not documented | 0% |
| Health metrics for drive online/offline | `cmd/metrics-v3-cluster-health.go`, `cmd/metrics-v3-system-drive.go`, `cmd/metrics-v3-cluster-erasure-set.go` | Metric names listed in `docs/metrics/v3.md` but no failure-scenario analysis | 20% |

**Target coverage**: 100% — Every question posed by the user must be comprehensively answered with source code citations.

### 0.7.2 Documentation Quality Criteria

**Completeness requirements:**
- Every user question receives a direct, unambiguous answer
- Every error message, log message, and metric name is quoted exactly from source code
- Quorum formulas are derived from actual code, not inferred from documentation
- The healing pipeline is traced end-to-end from disk reconnection to healing completion
- Hypothetical metric values are computed for a concrete example deployment (e.g., 16 drives, default parity)

**Accuracy validation:**
- All code references include file path and line number
- Error message strings are quoted verbatim from source (e.g., `"Write failed. Insufficient number of drives online"`)
- S3 error code mappings are traced through the complete chain: internal error → object API error → S3 API error
- Quorum calculations are verified against the `objectQuorumFromMeta` function
- Metric names use the exact constant values from the metrics source files

**Clarity standards:**
- Technical accuracy with progressive disclosure: start with the direct answer, then provide code-level rationale
- Each section opens with the user's original question restated for traceability
- Code snippets are kept minimal (2-3 lines max) to illustrate specific behaviors
- Mermaid diagrams visualize complex flows that would be difficult to follow in prose alone

**Maintainability:**
- Source citations reference specific file paths and line numbers for future verification
- The document structure mirrors the user's original question order for easy navigation
- Each section is self-contained to allow independent updates

### 0.7.3 Example and Diagram Requirements

- **Minimum examples per answer**: 1 (specific error message or metric value)
- **Diagram types required**: Flowchart (write-path decision), sequence diagram (healing pipeline), decision tree (healing criteria)
- **Code example testing**: Not applicable — examples are verbatim code extractions, not executable snippets
- **Hypothetical metric table**: One table showing metric values for a 16-drive deployment before and after 1 drive failure


## 0.8 Scope Boundaries

### 0.8.1 Exhaustively In Scope

**New documentation files:**
- `blitzy/documentation/minio_c07e5b49d477.md` — The sole deliverable, a comprehensive markdown document answering all user questions about MinIO erasure coding behavior during drive failures

**Source code files analyzed for documentation content (read-only, no modifications):**

| Category | Files |
|----------|-------|
| Erasure error definitions | `cmd/erasure-errors.go` |
| Storage error definitions | `cmd/storage-errors.go` |
| Object write path | `cmd/erasure-object.go` (lines 1240-1380, `putObject` / `PutObject`) |
| Object read path | `cmd/erasure-object.go` (lines 200-305, `GetObjectNInfo`; lines 690-860, `getObjectWithFileInfo`) |
| Erasure encoding | `cmd/erasure-encode.go` |
| Erasure decoding | `cmd/erasure-decode.go` |
| S3 error code mapping | `cmd/api-errors.go` (lines 869-878, `SlowDownRead` / `SlowDownWrite`) |
| Object API error types | `cmd/object-api-errors.go` (lines 228-255, `InsufficientReadQuorum` / `InsufficientWriteQuorum`) |
| Quorum calculation | `cmd/erasure-metadata.go` (lines 531-565, `objectQuorumFromMeta`) |
| Quorum reduction logic | `cmd/erasure-metadata-utils.go` (`reduceReadQuorumErrs`, `reduceWriteQuorumErrs`) |
| Healing trigger and execution | `cmd/background-newdisks-heal-ops.go` (lines 377-600, `monitorLocalDisksAndHeal`, `healFreshDisk`) |
| Healing criteria | `cmd/erasure-healing.go` (lines 156-183, `shouldHealObjectOnDisk`) |
| Healing object execution | `cmd/erasure-healing.go` (lines 258-420, `healObject`) |
| Drive monitoring and reconnection | `cmd/erasure-sets.go` (lines 194-309, `monitorAndConnectEndpoints`, `connectDisks`) |
| Cluster health metrics | `cmd/metrics-v3-cluster-health.go` |
| Per-drive metrics | `cmd/metrics-v3-system-drive.go` |
| Erasure set metrics | `cmd/metrics-v3-cluster-erasure-set.go` |
| Healthcheck handler | `cmd/healthcheck-handler.go` |
| Server pool health | `cmd/erasure-server-pool.go` (lines 2679-2800) |
| MRF (missing repair) state | `cmd/mrf.go` |
| Existing erasure docs | `docs/erasure/README.md` |
| Existing metrics catalog | `docs/metrics/v3.md` |
| Storage class configuration | `cmd/erasure-object.go` (storage class parity logic) |

**Documentation topics covered:**
- Write-path behavior during drive failure (error codes, quorum formula, parity upgrade)
- Read-path behavior for pre-existing objects during drive failure (reconstruction, read quorum)
- Healing trigger mechanism (timer-based detection, disk reconnection, conditions)
- Per-object healing criteria (`shouldHealObjectOnDisk` decision tree)
- Healing log messages (verbatim strings from source)
- Health metrics (cluster-level and per-drive metric names, expected values before/after failure)
- Error-to-S3-mapping chain (internal errors through to HTTP response)

### 0.8.2 Explicitly Out of Scope

- **Source code modifications** — The user explicitly states "the repository itself should remain unchanged"; no Go source files, configuration files, or test files will be modified
- **Temporary scripts** — The user mentions "temporary scripts may be used for observation" and "anything temporary should be cleaned up afterward"; no persistent scripts are created
- **Operational runbook creation** — The deliverable answers specific technical questions; it does not create an operational runbook or playbook
- **Performance benchmarking** — No load testing, throughput measurement, or performance analysis
- **Multi-pool or multi-site replication** — Analysis focuses on single-pool erasure set behavior, not cross-site replication
- **Encryption-at-rest or KMS interactions** — Drive failure behavior is analyzed at the erasure coding layer, not the encryption layer
- **Non-drive failure modes** — Network partitions, OOM events, and process crashes are not in scope
- **Documentation infrastructure changes** — No `mkdocs.yml`, `docusaurus.config.js`, or documentation generator modifications
- **Existing file updates** — `docs/erasure/README.md` and `docs/metrics/v3.md` are analyzed as references but not modified
- **Test suite execution or modification** — Build scripts such as `verify-healing.sh` are referenced for context only


## 0.9 Execution Parameters

### 0.9.1 Documentation-Specific Instructions

- **Documentation build command**: Not applicable — the deliverable is a standalone markdown file at `blitzy/documentation/minio_c07e5b49d477.md`, no static-site generation or build step required
- **Documentation preview command**: Any markdown renderer (e.g., `grip`, VS Code preview, or GitHub rendering) can preview the output file
- **Diagram generation command**: Diagrams are embedded as Mermaid code blocks within the markdown and render natively on GitHub and most markdown viewers
- **Documentation deployment command**: Not applicable — file is committed directly into the repository
- **Default format**: GitHub-Flavored Markdown (GFM) with embedded Mermaid diagrams for flowcharts and sequence diagrams
- **Citation requirement**: Every technical claim must reference the specific source file path and line number from the MinIO repository
- **Style guide**: Follows the structure of the user's questions — each section addresses one specific question area with a direct answer, code evidence, and rationale
- **Documentation validation**: Manual review for completeness against the user's original question list

### 0.9.2 Output File Specification

| Property | Value |
|----------|-------|
| File path | `blitzy/documentation/minio_c07e5b49d477.md` |
| Format | GitHub-Flavored Markdown |
| Naming convention | Branch name derived: `minio_c07e5b49d477` |
| Directory | `blitzy/documentation/` (to be created if absent) |
| Diagrams | Inline Mermaid blocks (`\`\`\`mermaid ... \`\`\``) |
| Code references | Inline code format: `Source: path/to/file.go:LineNumber` |

### 0.9.3 Content Structure of the Deliverable

The output document `minio_c07e5b49d477.md` will contain the following sections, each mapping to a user question:

- **Section 1 — Write-Path Behavior During Drive Failure**: Covers the specific S3 error code (`SlowDownWrite`, HTTP 503), the internal error chain, the quorum formula, the availability-optimized parity upgrade, and the exact threshold (≥ 50% drives offline) at which writes fail immediately
- **Section 2 — Read-Path Behavior for Pre-Existing Objects**: Covers whether reads succeed (yes, as long as `readQuorum` healthy drives remain), the erasure-coded reconstruction process, the MRF partial-operation queue, and the specific S3 error (`SlowDownRead`, HTTP 503) when read quorum cannot be met
- **Section 3 — Healing Trigger Mechanism**: Covers the `monitorLocalDisksAndHeal` timer (10-second interval), the `connectDisks` endpoint monitor, the `errUnformattedDisk` detection, the `.healing.bin` tracker resume, and the `pushHealLocalDisks` channel push
- **Section 4 — Per-Object Healing Criteria**: Covers all five conditions in `shouldHealObjectOnDisk` that determine whether a specific object needs healing on a specific drive
- **Section 5 — Healing Log Messages**: Provides the verbatim log strings emitted during healing start, worker allocation, retry attempts, per-object failures, and completion
- **Section 6 — Health Metrics During Drive Failure**: Lists the exact metric names (`drives_offline_count`, `drives_online_count`, per-drive `health` with values 0/1/2, erasure-set `online_drives_count`, `healing_drives_count`, `write_tolerance`, `read_tolerance`), and provides a hypothetical before/after table for a 16-drive deployment
- **Section 7 — Summary and Rationale**: Ties together the findings with an end-to-end narrative of what happens from the moment a drive disappears to the moment it is healed


## 0.10 Rules for Documentation

### 0.10.1 User-Specified Directives

The following rules are derived directly from the user's instructions and the project-level implementation rules:

- **"Create a new markdown document named `<source_branch_name>.md`"** — The output file must be `blitzy/documentation/minio_c07e5b49d477.md` in the destination repository. No other files are created.
- **"Comprehensively answers the question(s) posed in the prompt"** — Every question in the user's input must receive a direct, complete answer. No question may be deferred or marked as out of scope.
- **"Provide thinking / rationale behind the answers"** — Each answer must include not just the fact (e.g., the error code) but the reasoning derived from code analysis explaining why that behavior occurs.
- **"Do not make assumptions, base your answers on the code as the truth"** — All claims must be traceable to specific source code locations. Behavior described in external documentation or general knowledge about erasure coding is insufficient on its own — it must be verified against the actual MinIO codebase.
- **"Do not modify any existing files in the source repository"** — Zero modifications to any file in the MinIO repository. The only write operation is the creation of the new markdown document in `blitzy/documentation/`.
- **"Place the generated document in the `blitzy/documentation` directory"** — The directory must be created if it does not exist.

### 0.10.2 Inferred Documentation Rules

Based on the user's emphasis on observability and concrete evidence, the following rules are inferred:

- **Verbatim error messages** — Error messages and log strings must be quoted exactly as they appear in source code, enclosed in code blocks or inline code formatting
- **Code-sourced metric names** — Metric names must match the constant strings defined in the metrics source files, not summarized or paraphrased
- **Complete error chains** — For each error scenario, document the full chain from internal error → object API error → S3 API error → HTTP response, so the reader can trace the exact path
- **Hypothetical but grounded examples** — Where the user asks "what values do they show", provide computed values based on a concrete example deployment (e.g., 16-drive erasure set with default parity), clearly labeled as hypothetical but derived from code-verified formulas
- **No operational guarantees** — The user explicitly states they want to understand "what the system actually does" not "what the documentation promises"; the document must describe observed code behavior, not aspirational guarantees
- **Repository cleanliness** — "Temporary scripts may be used for observation, but the repository itself should remain unchanged and anything temporary should be cleaned up afterward" — no temporary files, scripts, or artifacts may persist after the documentation task completes

### 0.10.3 Formatting and Style Rules

- Use GitHub-Flavored Markdown throughout
- Section headings follow the user's question structure (one section per question area)
- Code references use the format: `Source: path/to/file.go:LineNumber`
- Error messages and log strings are wrapped in backtick code blocks
- Mermaid diagrams are used for visualizing flows (write path, healing pipeline, healing criteria decision tree)
- Tables are used for structured data (metric names and values, error mapping chains)
- Bullet points (dashes only, no numbered lists) for enumerations
- Each section begins with the user's original question restated for traceability


## 0.11 References

### 0.11.1 Source Code Files Analyzed

The following files were retrieved and analyzed during context gathering to derive all conclusions in this Agent Action Plan:

| File Path | Purpose | Key Content Extracted |
|-----------|---------|----------------------|
| `cmd/erasure-errors.go` | Erasure-layer error sentinels | `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired` |
| `cmd/storage-errors.go` | Storage-layer error types | `errDiskNotFound`, `errFaultyDisk`, `errUnformattedDisk`, `errDiskFull`, `errFileNotFound`, `errFileCorrupt`, `baseIgnoredErrs`, `objectOpIgnoredErrs` |
| `cmd/erasure-object.go` | Object write and read paths | `PutObject`/`putObject` (write quorum logic, parity upgrade), `GetObjectNInfo`/`getObjectWithFileInfo` (read quorum, erasure reconstruction, MRF queueing) |
| `cmd/erasure-encode.go` | Erasure encoding writer | `multiWriter.Write` per-disk error tracking, quorum check on write |
| `cmd/erasure-decode.go` | Erasure decoding reader | Parallel read and Reed-Solomon reconstruction |
| `cmd/api-errors.go` | S3 API error mapping | `ErrSlowDownRead` (S3 `SlowDownRead`, HTTP 503), `ErrSlowDownWrite` (S3 `SlowDownWrite`, HTTP 503) |
| `cmd/object-api-errors.go` | Object API error structs | `InsufficientReadQuorum` (with `RQErrType`), `InsufficientWriteQuorum` |
| `cmd/erasure-metadata.go` | Quorum from metadata | `objectQuorumFromMeta` — derives `readQuorum` and `writeQuorum` from erasure distribution, algorithm name `rs-vandermonde` |
| `cmd/erasure-metadata-utils.go` | Quorum reduction helpers | `reduceReadQuorumErrs`, `reduceWriteQuorumErrs` with `(offline-disks=X/Y)` annotation |
| `cmd/background-newdisks-heal-ops.go` | Healing background goroutine | `monitorLocalDisksAndHeal` (10s timer), `healFreshDisk`, `getLocalDisksToHeal`, healing log messages, retry logic (4 retries) |
| `cmd/erasure-healing.go` | Object-level healing logic | `shouldHealObjectOnDisk` (5 healing conditions), `healObject` (read quorum check, metadata comparison, drive state categorization) |
| `cmd/erasure-sets.go` | Erasure set management | `monitorAndConnectEndpoints`, `connectDisks` (offline disk reconnection, `pushHealLocalDisks` trigger) |
| `cmd/metrics-v3-cluster-health.go` | Cluster health metrics | `drives_offline_count`, `drives_online_count`, `drives_count`, capacity metrics |
| `cmd/metrics-v3-system-drive.go` | Per-drive metrics | `health` (0=offline, 1=healthy, 2=healing), `timeout_errors_total`, `io_errors_total`, `availability_errors_total`, `offline_count`, `online_count` |
| `cmd/metrics-v3-cluster-erasure-set.go` | Erasure set metrics | `online_drives_count`, `healing_drives_count`, `read_quorum`, `write_quorum`, `read_tolerance`, `write_tolerance`, `read_health`, `write_health`, `overall_health` |
| `cmd/healthcheck-handler.go` | Healthcheck HTTP endpoint | `ClusterCheckHandler` returning 200/503, `X-Minio-Write-Quorum` and `X-Minio-Healing-Drives` headers |
| `cmd/erasure-server-pool.go` | Server pool health aggregation | Health method iterating all disks, counting online/healing per set, write quorum and read quorum health determination |
| `cmd/mrf.go` | Missing-repair-fragment state | `PartialOperation` struct, 100K queue, `.heal/mrf` persistence directory |
| `docs/erasure/README.md` | Existing erasure coding docs | Reed-Solomon overview, set sizing, quickstart — lacks failure-mode specifics |
| `docs/metrics/v3.md` | Existing metrics catalog | Metric names listed but no failure-scenario value analysis |

### 0.11.2 Folders Explored

| Folder Path | Purpose |
|-------------|---------|
| `""` (root) | Initial repository structure discovery |
| `cmd/` | Server implementation — erasure coding, healing, metrics, health |
| `internal/` | Reusable infrastructure libraries |
| `docs/` | Existing documentation |
| `docs/erasure/` | Existing erasure coding documentation |
| `docs/metrics/` | Existing metrics documentation |
| `buildscripts/` | Build and verification scripts including `verify-healing.sh` |

### 0.11.3 Tech Spec Sections Retrieved

| Section Heading | Purpose |
|-----------------|---------|
| 1.1 Executive Summary | System overview and project context |
| 1.3 Scope | Boundaries and deliverables definition |

### 0.11.4 User Attachments

No attachments were provided by the user. There are no Figma URLs, uploaded files, or external design assets.

### 0.11.5 External References

No external web searches were required for this documentation task — all answers are derived exclusively from the MinIO source code repository, per the user's directive to base answers on the code as the truth.


