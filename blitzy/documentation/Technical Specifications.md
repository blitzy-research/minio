# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create new documentation** that comprehensively answers a set of investigative questions about MinIO's runtime fault-tolerance behavior when operating in distributed/erasure-coded mode — specifically, how quorum decisions are made, how disk failures are detected and handled, how healing is triggered and completed, and what observable signals (logs, health endpoints, write behavior) reveal the system's internal state during degraded operation.

- **Documentation Category**: Create new documentation
- **Documentation Type**: Technical investigation / architecture deep-dive Q&A document
- **Output Artifact**: `blitzy/documentation/minio_c07e5b49d477.md` — a single Markdown file placed in the destination repository's `blitzy/documentation` directory, named after the source branch `minio_c07e5b49d477`

The user's requirements decompose into the following distinct documentation objectives:

- **Quorum threshold determination**: How does MinIO, running with 4 directories under erasure coding, decide it is healthy? What assumptions does it make about the required number of disks for read and write operations?
- **Runtime disk failure behavior**: When a directory becomes inaccessible (e.g., via permission change), what happens in that exact moment — does MinIO silently adapt or hard-fail?
- **Above-threshold vs. below-threshold scenarios**: What is the observable difference when one disk is lost (still above write quorum) versus when two disks are lost (potentially below write quorum)?
- **Log diagnostics**: Do MinIO's logs identify the failing disk by its path? Is there evidence of recovery attempts while the system is live?
- **Automatic recovery and healing**: When a missing directory becomes accessible again, does MinIO detect this on its own through a polling mechanism, or must something external trigger healing? How are objects written during the degraded period repaired?
- **Code-grounded quorum logic**: Where in the codebase does the quorum decision live? How does MinIO calculate the threshold for proceeding versus refusing operations?
- **Observable signals**: What can be seen from the health endpoint and actual write attempts while the system is running in a degraded state?

### 0.1.2 Special Instructions and Constraints

- **No repository modifications**: The user explicitly states the repository itself should remain unchanged. Any temporary scripts used for observation must be cleaned up afterward.
- **Implementation rule — SWE-AtlasQnA-Repo**: The output must be a new markdown document named `<source_branch_name>.md` (i.e., `minio_c07e5b49d477.md`), placed in the `blitzy/documentation` directory. No existing files in the source repository may be modified.
- **Answers must be grounded in code**: The user requires that answers be based on the code as the source of truth, not assumptions.
- **Provide thinking/rationale**: Each answer must include the reasoning and code references behind it.
- **Style**: The output is an investigative Q&A document with code citations, not a traditional API reference or user guide.

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To document quorum threshold determination, we will trace the `defaultWQuorum()` and `defaultRQuorum()` methods in `cmd/erasure.go` and the `objectQuorumFromMeta()` function in `cmd/erasure-metadata.go`, explaining how `parityBlocks`, `dataBlocks`, and `writeQuorum` are computed from the erasure set configuration.
- To document runtime disk failure behavior, we will trace the error propagation path from `cmd/xl-storage.go` (where `os.IsPermission()` is caught and mapped to `errDiskAccessDenied` in `cmd/storage-errors.go`), through the `checkDiskStale()` check in `cmd/xl-storage-disk-id-check.go`, to the quorum enforcement in `cmd/erasure-encode.go` and `cmd/erasure-object.go`.
- To document above-threshold vs. below-threshold scenarios, we will analyze the `putObject()` flow in `cmd/erasure-object.go` where write quorum is enforced, and contrast the behavior when enough disks remain (degraded write succeeds) vs. when `errErasureWriteQuorum` is returned.
- To document log diagnostics, we will examine the `storageLogIf`, `storageLogOnceIf`, `printEndpointError`, and `healingLogIf` functions and how the `Health()` method in `cmd/erasure-server-pool.go` logs quorum failures by pool/set with drive counts.
- To document automatic recovery and healing, we will trace the `monitorAndConnectEndpoints()` polling loop in `cmd/erasure-sets.go`, the `connectDisks()` reconnection logic, the `monitorLocalDisksAndHeal()` loop in `cmd/background-newdisks-heal-ops.go`, the `healFreshDisk()` function, and the MRF (Most Recently Failed) healing in `cmd/mrf.go`.
- To document the health endpoint, we will trace the `ClusterCheckHandler` in `cmd/healthcheck-handler.go` through to `Health()` in `cmd/erasure-server-pool.go`, documenting the `HealthResult` structure and the response headers (`X-Minio-Write-Quorum`, `X-Minio-Healing-Drives`).

### 0.1.4 Inferred Documentation Needs

Based on code analysis, the following additional documentation elements are inferred as necessary for a complete answer:

- The **availability-optimized parity upgrade** mechanism in `putObject()` (`cmd/erasure-object.go`, lines ~1300–1320) where MinIO automatically increases parity when disks are offline, affecting write behavior in degraded mode — this is essential context for understanding above-threshold writes.
- The **MRF (Most Recently Failed)** subsystem (`cmd/mrf.go`) which queues partial operations for retry healing — critical for explaining how objects written during degradation get repaired.
- The **`monitorDiskWritable()`** function in `cmd/xl-storage-disk-id-check.go` which runs a periodic write/read/delete cycle to bring offline drives back online — directly answers the question about live recovery detection.
- The **Prometheus metrics** for erasure set health (`cmd/metrics-v3-cluster-erasure-set.go`) that expose `read_tolerance`, `write_tolerance`, and per-set health states — provides additional observable signals beyond the health endpoint.
- The **`diskErrToDriveState()`** function in `cmd/erasure.go` which maps errors including `errDiskAccessDenied` to the `DriveStatePermission` state — explains how permission errors surface in diagnostics.

## 0.2 Documentation Discovery and Analysis

### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a mature, Markdown-driven documentation tree under `docs/` with no automated documentation generator (no `mkdocs.yml`, `docusaurus.config.js`, or `sphinx.conf.py` detected). Documentation is organized as flat Markdown files and subdirectories, with a Jekyll theme configuration (`_config.yml` at root selecting `jekyll-theme-minimal`) providing minimal site rendering.

- **Current documentation framework**: Raw Markdown with Jekyll-based GitHub Pages rendering
- **Documentation generator configuration**: `_config.yml` (root) — `theme: jekyll-theme-minimal`
- **API documentation tools**: None detected (no JSDoc, Godoc generation, Swagger/OpenAPI)
- **Diagram tools**: Mermaid-compatible Markdown notation (no dedicated PlantUML or Mermaid config files detected)
- **Documentation hosting/deployment**: GitHub Pages via Jekyll theme, supplemented by external MinIO documentation site (`min.io/docs`)

Existing documentation directly relevant to the user's questions:

| Documentation File | Content | Relevance |
|---|---|---|
| `docs/erasure/README.md` | Erasure code quickstart guide — explains Reed-Solomon sharding, N/2 parity default, drive tolerance | High — foundational context for quorum explanation |
| `docs/erasure/storage-class/` | Storage class configuration for custom parity | Medium — affects quorum threshold calculation |
| `docs/distributed/README.md` | Distributed mode quickstart — deployment, consistency guarantees, data protection | High — describes fault tolerance at deployment level |
| `docs/distributed/DESIGN.md` | Erasure set selection algorithm, GCD-based set sizing, distribution strategy | High — explains how 4 drives map to an erasure set |
| `docs/distributed/SIZING.md` | Parity/tolerance lookup tables for various configurations | High — directly answers the "4 drives, 1 server" scenario |
| `docs/metrics/` | Prometheus metrics reference, health check documentation | Medium — supplements health endpoint analysis |
| `docs/config/` | Configuration guide including API/heal/scanner tuning | Low — background on heal configuration |
| `README.md` (root) | Quickstart and operational guide | Low — general deployment context |

### 0.2.2 Repository Code Analysis for Documentation

The following code paths were examined to ground the documentation in source code truth:

**Quorum Logic (core decision path)**:
- `cmd/erasure.go` — `erasureObjects` struct with `setDriveCount`, `defaultParityCount`; `defaultWQuorum()` and `defaultRQuorum()` methods
- `cmd/erasure-metadata.go` — `objectQuorumFromMeta()`, `commonParity()`, `listObjectParities()` — per-object quorum derivation
- `cmd/erasure-metadata-utils.go` — `reduceWriteQuorumErrs()`, `reduceReadQuorumErrs()`, `reduceQuorumErrs()` — quorum error aggregation

**Write Path (failure behavior)**:
- `cmd/erasure-object.go` — `putObject()` flow with parity computation, availability-optimized parity upgrade, `errErasureWriteQuorum` enforcement
- `cmd/erasure-encode.go` — `Encode()` and `multiWriter.Write()` with quorum-checked writes to disk shards

**Disk Failure Detection**:
- `cmd/xl-storage.go` — `checkFormatJSON()`, `GetDiskID()` — maps `os.IsPermission()` to `errDiskAccessDenied`
- `cmd/xl-storage-disk-id-check.go` — `checkDiskStale()`, `IsOnline()`, `monitorDiskStatus()`, `monitorDiskWritable()` — stale detection and recovery polling
- `cmd/storage-errors.go` — Error constants: `errDiskAccessDenied`, `errDiskNotFound`, `errFaultyDisk`
- `cmd/erasure-errors.go` — `errErasureWriteQuorum`, `errErasureReadQuorum`

**Health and Monitoring**:
- `cmd/healthcheck-handler.go` — `ClusterCheckHandler`, `ClusterReadCheckHandler`, `ReadinessCheckHandler`, `LivenessCheckHandler`
- `cmd/healthcheck-router.go` — Route registration for `/minio/health/cluster`, `/minio/health/cluster/read`, `/minio/health/live`, `/minio/health/ready`
- `cmd/erasure-server-pool.go` — `Health()` method, `HealthOptions`, `HealthResult` struct with per-erasure-set breakdown
- `cmd/metrics-v3-cluster-erasure-set.go` — Prometheus metrics: `erasure_set_write_quorum`, `erasure_set_read_tolerance`, `erasure_set_write_tolerance`, `erasure_set_health`
- `cmd/metrics-v3-cluster-health.go` — `drives_offline_count`, `drives_online_count`
- `cmd/metrics-v3-system-drive.go` — Per-drive health state metric (0=offline, 1=online, 2=healing)

**Healing and Recovery**:
- `cmd/erasure-sets.go` — `connectDisks()`, `monitorAndConnectEndpoints()` — periodic disk reconnection loop
- `cmd/background-newdisks-heal-ops.go` — `initAutoHeal()`, `monitorLocalDisksAndHeal()`, `healFreshDisk()` — automatic disk healing
- `cmd/global-heal.go` — `healErasureSet()` — full erasure set healing traversal
- `cmd/erasure-healing.go` — `healObject()` — per-object healing by re-encoding missing/corrupt shards
- `cmd/mrf.go` — `mrfState`, `PartialOperation`, `healRoutine()` — retry healing for objects written during degradation

### 0.2.3 Web Search Research Conducted

No external web searches were required. The MinIO codebase is self-contained, and all questions posed by the user can be answered definitively from source code analysis. The existing documentation in `docs/erasure/`, `docs/distributed/`, and the source code in `cmd/` provide sufficient grounding.

## 0.3 Documentation Scope Analysis

### 0.3.1 Code-to-Documentation Mapping

The documentation requirement is a comprehensive Q&A document. Each question maps to specific code modules:

**Module: Quorum Calculation (`cmd/erasure.go`, `cmd/erasure-metadata.go`)**
- Public APIs: `defaultWQuorum()`, `defaultRQuorum()`, `objectQuorumFromMeta()`, `commonParity()`
- Current documentation: `docs/erasure/README.md` provides a high-level overview; `docs/distributed/SIZING.md` has a lookup table. Neither explains the code-level mechanics.
- Documentation needed: Detailed walkthrough of how quorum thresholds are calculated for a 4-drive erasure set, grounded in code

**Module: Write Path under Degradation (`cmd/erasure-object.go`, `cmd/erasure-encode.go`)**
- Public APIs: `putObject()`, `Encode()`, `multiWriter.Write()`
- Current documentation: `docs/distributed/README.md` briefly mentions that objects get additional parity when drives are offline at write time
- Documentation needed: Step-by-step trace of what happens when `putObject()` encounters offline disks — parity upgrade logic, quorum check, success vs. failure paths

**Module: Disk Failure Detection (`cmd/xl-storage.go`, `cmd/xl-storage-disk-id-check.go`)**
- Public APIs: `GetDiskID()`, `checkFormatJSON()`, `checkDiskStale()`, `IsOnline()`, `monitorDiskWritable()`
- Current documentation: None exists explaining how permission errors map to disk states
- Documentation needed: Trace from `os.IsPermission()` through `errDiskAccessDenied` to disk state classification and quorum impact

**Module: Health Endpoint (`cmd/healthcheck-handler.go`, `cmd/erasure-server-pool.go`)**
- Public APIs: `ClusterCheckHandler`, `Health()`, `HealthResult`
- Current documentation: `docs/metrics/` contains metrics reference; no documentation for the cluster health endpoint's quorum-based decision logic
- Documentation needed: Explanation of what the `/minio/health/cluster` endpoint evaluates and what headers it returns

**Module: Healing and Recovery (`cmd/erasure-sets.go`, `cmd/background-newdisks-heal-ops.go`, `cmd/mrf.go`)**
- Public APIs: `connectDisks()`, `monitorAndConnectEndpoints()`, `monitorLocalDisksAndHeal()`, `healFreshDisk()`, `healObject()`, `healRoutine()`
- Current documentation: None covers the internal healing lifecycle end-to-end
- Documentation needed: Explanation of the polling reconnection mechanism, automatic healing trigger, and MRF-based repair of degraded-mode objects

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, documentation gaps include:

- **No existing code-grounded explanation** of quorum mechanics for a specific N-drive configuration — `docs/erasure/README.md` is user-facing and conceptual, not code-referenced
- **No documentation of the `errDiskAccessDenied` → `DriveStatePermission`** error mapping chain or what happens at runtime when permissions change
- **No end-to-end healing lifecycle document** that traces from disk loss through reconnection, healing initiation, and object repair
- **No documentation of the MRF subsystem** and how it retries objects written during degraded operation
- **No documentation of the `monitorDiskWritable()` recovery polling** mechanism in `xl-storage-disk-id-check.go`
- **No documentation of the `AvailabilityOptimized()` parity upgrade** behavior during writes
- **No documentation mapping health endpoint behavior** to specific code paths and quorum calculations

## 0.4 Documentation Implementation Design

### 0.4.1 Documentation Structure Planning

The output document `blitzy/documentation/minio_c07e5b49d477.md` will be structured as a single comprehensive Q&A-style technical investigation document. The internal structure follows the logical flow of the user's questions:

```
blitzy/
└── documentation/
    └── minio_c07e5b49d477.md
        ├── Introduction (context and scenario definition)
        ├── Quorum Threshold Determination
        │   ├── Erasure set formation for 4 drives
        │   ├── Default parity and data block calculation
        │   ├── Read quorum and write quorum derivation
        │   └── Code references with rationale
        ├── Runtime Disk Failure Behavior
        │   ├── Permission-change error propagation path
        │   ├── Disk state classification
        │   └── Immediate impact on in-flight operations
        ├── Above-Threshold vs. Below-Threshold Scenarios
        │   ├── Single disk loss (degraded but operational)
        │   ├── Parity upgrade mechanism during writes
        │   ├── Second disk loss (below write quorum)
        │   └── Error returned to client
        ├── Log Diagnostics
        │   ├── Disk path identification in logs
        │   ├── Health() quorum failure logging
        │   └── Healing progress logging
        ├── Automatic Recovery and Healing
        │   ├── monitorAndConnectEndpoints polling loop
        │   ├── connectDisks reconnection mechanism
        │   ├── monitorDiskWritable recovery polling
        │   ├── healFreshDisk and monitorLocalDisksAndHeal
        │   └── MRF-based object repair
        ├── Health Endpoint Behavior
        │   ├── Endpoint routes and handlers
        │   ├── HealthResult structure
        │   ├── Response headers and status codes
        │   └── Prometheus erasure set metrics
        └── Summary
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach**:
- Extract quorum formulas from `cmd/erasure.go:86-95` — `defaultWQuorum()` and `defaultRQuorum()` methods
- Extract object-level quorum from `cmd/erasure-metadata.go:531-570` — `objectQuorumFromMeta()` function
- Extract write-path behavior from `cmd/erasure-object.go:1245-1340` — `putObject()` with availability-optimized parity
- Extract health evaluation from `cmd/erasure-server-pool.go:2679-2815` — `Health()` method
- Extract healing lifecycle from `cmd/background-newdisks-heal-ops.go:377-450` and `cmd/erasure-sets.go:194-290`
- Extract disk error mapping from `cmd/xl-storage.go:270-280` and `cmd/erasure.go:100-120` — `diskErrToDriveState()`

**Documentation Standards**:
- Markdown formatting with proper header hierarchy (`#`, `##`, `###`)
- Code references using inline format: `Source: cmd/erasure.go:86`
- Concrete numeric examples for the 4-drive scenario throughout
- Mermaid diagrams for the healing lifecycle flow and the write-path decision tree
- Tables for quorum arithmetic and error-to-state mappings

### 0.4.3 Diagram and Visual Strategy

The following Mermaid diagrams will be created in the output document:

- **Write-path decision flowchart**: Showing the path from `putObject()` entry through parity calculation, availability-optimized upgrade check, quorum validation, erasure encode, and either success or `errErasureWriteQuorum` failure
- **Healing lifecycle sequence diagram**: Showing the flow from disk loss → `monitorAndConnectEndpoints` detection → `connectDisks` reconnection → `monitorLocalDisksAndHeal` trigger → `healFreshDisk` execution → object-level `healObject` repair
- **Health endpoint evaluation flow**: Showing how `ClusterCheckHandler` calls `Health()`, which iterates pools and sets, compares online drives to write/read quorum, and returns the aggregate result

## 0.5 Documentation File Transformation Mapping

### 0.5.1 File-by-File Documentation Plan

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---|---|---|---|
| `blitzy/documentation/minio_c07e5b49d477.md` | CREATE | `cmd/erasure.go`, `cmd/erasure-metadata.go`, `cmd/erasure-metadata-utils.go`, `cmd/erasure-object.go`, `cmd/erasure-encode.go`, `cmd/erasure-server-pool.go`, `cmd/healthcheck-handler.go`, `cmd/healthcheck-router.go`, `cmd/xl-storage.go`, `cmd/xl-storage-disk-id-check.go`, `cmd/storage-errors.go`, `cmd/erasure-errors.go`, `cmd/erasure-sets.go`, `cmd/erasure-common.go`, `cmd/erasure-healing.go`, `cmd/background-newdisks-heal-ops.go`, `cmd/global-heal.go`, `cmd/mrf.go`, `cmd/metrics-v3-cluster-erasure-set.go`, `cmd/metrics-v3-cluster-health.go`, `cmd/metrics-v3-system-drive.go`, `docs/erasure/README.md`, `docs/distributed/SIZING.md`, `docs/distributed/DESIGN.md`, `docs/distributed/README.md` | Complete Q&A investigation document answering all user questions about MinIO erasure-coding fault tolerance, quorum mechanics, disk failure behavior, healing lifecycle, log diagnostics, and health endpoint observability — grounded in code references with rationale |

This is the sole documentation file to produce. Per the implementation rule **SWE-AtlasQnA-Repo**, no existing files in the source repository may be modified.

### 0.5.2 New Documentation File Detail

```
File: blitzy/documentation/minio_c07e5b49d477.md
Type: Technical Investigation / Q&A Document
Source Code: cmd/erasure*.go, cmd/xl-storage*.go, cmd/healthcheck*.go,
             cmd/background-newdisks-heal-ops.go, cmd/mrf.go,
             cmd/storage-errors.go, cmd/metrics-v3-cluster-*.go
Sections:
    - Introduction (scenario: 4-drive erasure-coded MinIO instance)
    - Quorum Threshold Determination
        - How the erasure set is formed and how parity/data drives are decided
        - defaultWQuorum() and defaultRQuorum() code trace
        - objectQuorumFromMeta() for per-object quorum
        - Concrete arithmetic for 4-drive case: 2 data + 2 parity
    - Runtime Disk Failure Behavior (Permission Change)
        - xl-storage.go permission error → errDiskAccessDenied mapping
        - xl-storage-disk-id-check.go stale-disk detection
        - diskErrToDriveState() → DriveStatePermission
        - Immediate impact: disk marked nil in erasure set
    - Above-Threshold vs. Below-Threshold Scenarios
        - 1 disk lost: 3 online >= writeQuorum(3) → writes succeed
        - Availability-optimized parity upgrade explanation
        - 2 disks lost: 2 online < writeQuorum(3) → errErasureWriteQuorum
        - Read behavior: 2 online >= readQuorum(2) → reads still succeed
    - Log Diagnostics
        - printEndpointError identifies disk by endpoint path
        - Health() logs quorum failure with pool/set/drive-count detail
        - monitorDiskWritable logs recovery to online status
    - Automatic Recovery and Healing
        - monitorAndConnectEndpoints polling interval
        - connectDisks reconnection mechanics
        - monitorDiskWritable write/read/delete probe
        - monitorLocalDisksAndHeal auto-heal trigger
        - healFreshDisk full-set healing
        - MRF heal for objects written during degradation
    - Health Endpoint and Observability
        - /minio/health/cluster → ClusterCheckHandler → Health()
        - HealthResult fields and response headers
        - Prometheus metrics for erasure set tolerance
    - Summary
Diagrams:
    - Flowchart: Write-path quorum decision in putObject()
    - Sequence diagram: Disk failure → reconnection → healing lifecycle
Key Citations: cmd/erasure.go, cmd/erasure-metadata.go,
    cmd/erasure-object.go, cmd/erasure-server-pool.go,
    cmd/xl-storage.go, cmd/xl-storage-disk-id-check.go,
    cmd/erasure-sets.go, cmd/background-newdisks-heal-ops.go,
    cmd/mrf.go, cmd/healthcheck-handler.go
```

### 0.5.3 Documentation Configuration Updates

No documentation configuration files need to be created or updated. The output is a standalone Markdown file in `blitzy/documentation/`. There is no `mkdocs.yml`, `docusaurus.config.js`, or similar generator to configure. The `blitzy/documentation/` directory will be created as part of the write operation if it does not exist.

### 0.5.4 Cross-Documentation Dependencies

- The new document references concepts from `docs/erasure/README.md` (erasure coding fundamentals, N/2 parity default) and `docs/distributed/SIZING.md` (parity tolerance tables). These are read-only references; no updates are needed.
- No navigation, table of contents, index, or glossary updates are required since the output is a standalone file in a separate directory (`blitzy/documentation/`).

## 0.6 Dependency Inventory

### 0.6.1 Documentation Dependencies

No external documentation generation tools are required for this task. The output is a hand-authored Markdown file. The only runtime dependency is the Go toolchain for understanding and referencing the source code.

| Registry | Package Name | Version | Purpose |
|---|---|---|---|
| golang.org | go | 1.23 | Project runtime (from `go.mod`) — code comprehension context |
| github.com | klauspost/reedsolomon | (pinned in go.sum) | Reed-Solomon erasure coding library — referenced in documentation for algorithm context |
| github.com | minio/madmin-go/v3 | (pinned in go.sum) | Admin API types including `madmin.HealScanMode`, `madmin.DriveStateOk` — referenced for drive state constants |

These are read-only references for documentation accuracy — no packages need to be installed or executed for the documentation task itself.

### 0.6.2 Documentation Reference Updates

Not applicable. No existing documentation files require link updates, as the output document is a new standalone file in `blitzy/documentation/` that does not participate in any existing navigation structure or cross-linking system.

## 0.7 Coverage and Quality Targets

### 0.7.1 Documentation Coverage Metrics

The user's prompt contains seven distinct investigative questions. Coverage is measured against each:

| Question Topic | Source Code Modules Identified | Coverage Target |
|---|---|---|
| How does MinIO decide health with 4 drives under erasure coding? | `cmd/erasure.go`, `cmd/erasure-metadata.go`, `cmd/erasure-server-pool.go` | 100% — complete code-grounded walkthrough |
| What happens when a directory becomes inaccessible (permission change)? | `cmd/xl-storage.go`, `cmd/xl-storage-disk-id-check.go`, `cmd/storage-errors.go`, `cmd/erasure.go` | 100% — full error propagation chain |
| Above-threshold vs. below-threshold behavior? | `cmd/erasure-object.go`, `cmd/erasure-encode.go`, `cmd/erasure-errors.go` | 100% — both scenarios with code evidence |
| Do logs identify failing disk by path? Recovery signals? | `cmd/erasure-sets.go`, `cmd/xl-storage-disk-id-check.go`, `cmd/erasure-server-pool.go` | 100% — log format and content analysis |
| Does MinIO auto-detect returning disks? Polling mechanism? | `cmd/erasure-sets.go`, `cmd/xl-storage-disk-id-check.go`, `cmd/background-newdisks-heal-ops.go` | 100% — polling interval and reconnection logic |
| How are objects written during degradation repaired? | `cmd/mrf.go`, `cmd/erasure-healing.go`, `cmd/global-heal.go` | 100% — MRF queue and healing pipeline |
| Where does quorum decision live in the code? | `cmd/erasure-metadata.go`, `cmd/erasure-metadata-utils.go`, `cmd/erasure-object.go` | 100% — function-level citations |

**Overall target**: 100% question coverage, with every claim supported by a file path and line range citation.

### 0.7.2 Documentation Quality Criteria

**Completeness requirements**:
- Every user question must be answered with a dedicated section
- Every answer must cite at least one source file with function name
- Quorum arithmetic must be shown explicitly for the 4-drive case (not generalized)
- Both success and failure paths must be documented for each scenario

**Accuracy validation**:
- All code references must correspond to actual functions and line ranges verified through `read_file` during context gathering
- Quorum formulas must be derived directly from `defaultWQuorum()` and `defaultRQuorum()` source code, not from external documentation
- Error constants must be quoted exactly as defined in `cmd/storage-errors.go` and `cmd/erasure-errors.go`

**Clarity standards**:
- Technical depth appropriate for a developer investigating fault-tolerance internals
- Progressive structure: start with the healthy baseline, introduce single failure, then double failure
- Mermaid diagrams for complex control flows (write path, healing lifecycle)
- Inline code formatting for function names, error constants, and file paths

**Maintainability**:
- Source citations in `Source: path/to/file.go:LineRange` format enable future verification
- Self-contained document with no external link dependencies beyond the repository itself

### 0.7.3 Example and Diagram Requirements

- **Minimum code reference examples per question**: At least one function signature or code excerpt per answer section
- **Diagram types required**: Flowchart (write-path quorum decision), sequence diagram (healing lifecycle)
- **Code example verification**: All code snippets are extracted from verified source reads; no synthetic examples
- **Quorum arithmetic examples**: Explicit numeric calculations for 4-drive configuration showing data blocks, parity blocks, read quorum, and write quorum

## 0.8 Scope Boundaries

### 0.8.1 Exhaustively In Scope

**New documentation files**:
- `blitzy/documentation/minio_c07e5b49d477.md` — the sole output artifact

**Source code files analyzed (read-only, for documentation content)**:
- `cmd/erasure.go` — erasureObjects struct, defaultWQuorum(), defaultRQuorum(), diskErrToDriveState()
- `cmd/erasure-coding.go` — Erasure struct, NewErasure()
- `cmd/erasure-metadata.go` — objectQuorumFromMeta(), commonParity(), listObjectParities()
- `cmd/erasure-metadata-utils.go` — reduceWriteQuorumErrs(), reduceReadQuorumErrs(), reduceErrs(), readAllFileInfo()
- `cmd/erasure-object.go` — putObject(), countOnlineDisks(), availability-optimized parity upgrade
- `cmd/erasure-encode.go` — Encode(), multiWriter.Write() with quorum enforcement
- `cmd/erasure-common.go` — getOnlineDisks(), getOnlineLocalDisks()
- `cmd/erasure-sets.go` — erasureSets struct, connectDisks(), monitorAndConnectEndpoints(), getDiskMap()
- `cmd/erasure-server-pool.go` — Health(), HealthOptions, HealthResult, per-pool/set evaluation
- `cmd/erasure-errors.go` — errErasureWriteQuorum, errErasureReadQuorum
- `cmd/erasure-healing.go` — healObject(), healingMetric, shouldHealObjectOnDisk()
- `cmd/erasure-healing-common.go` — healing helper functions
- `cmd/xl-storage.go` — permission error mapping, checkFormatJSON(), GetDiskID()
- `cmd/xl-storage-disk-id-check.go` — checkDiskStale(), IsOnline(), monitorDiskStatus(), monitorDiskWritable()
- `cmd/storage-errors.go` — errDiskAccessDenied, errDiskNotFound, errFaultyDisk, errUnformattedDisk
- `cmd/healthcheck-handler.go` — ClusterCheckHandler, ClusterReadCheckHandler, ReadinessCheckHandler, LivenessCheckHandler
- `cmd/healthcheck-router.go` — health check route registration
- `cmd/background-newdisks-heal-ops.go` — initAutoHeal(), monitorLocalDisksAndHeal(), healFreshDisk(), getLocalDisksToHeal()
- `cmd/background-heal-ops.go` — healTask, healResult, healRoutine
- `cmd/global-heal.go` — healErasureSet(), healBucket(), healObject()
- `cmd/mrf.go` — mrfState, PartialOperation, healRoutine(), startMRFPersistence()
- `cmd/metrics-v3-cluster-erasure-set.go` — erasure set health metrics
- `cmd/metrics-v3-cluster-health.go` — cluster-wide drive count metrics
- `cmd/metrics-v3-system-drive.go` — per-drive health state metrics

**Existing documentation files referenced (read-only)**:
- `docs/erasure/README.md`
- `docs/distributed/README.md`
- `docs/distributed/DESIGN.md`
- `docs/distributed/SIZING.md`

### 0.8.2 Explicitly Out of Scope

- **Source code modifications**: No Go source files may be modified (per user instruction and SWE-AtlasQnA-Repo rule)
- **Existing documentation modifications**: No files under `docs/` or `README.md` may be modified
- **Test file modifications**: No test files are in scope
- **Feature additions or code refactoring**: This is a documentation-only task
- **Deployment configuration changes**: No Dockerfile, Helm, or CI/CD changes
- **Multi-site replication documentation**: The user's question is scoped to single-instance distributed mode with local directories, not cross-site replication
- **Network-level failures**: The user's scenario is specifically about local directory permission changes, not network partitions between nodes
- **Custom storage class configurations**: The analysis focuses on default parity settings; user-customized `MINIO_STORAGE_CLASS_STANDARD` overrides are mentioned but not the primary focus
- **Kubernetes health probe configuration**: While health endpoints are documented, Kubernetes-specific probe configuration is out of scope
- **Temporary observation scripts**: The user mentions temporary scripts for observation but states the repository should remain unchanged. No scripts need to be produced in the output document; the document explains what can be observed from existing endpoints and logs

## 0.9 Execution Parameters

### 0.9.1 Documentation-Specific Instructions

- **Documentation build command**: Not applicable — output is a standalone Markdown file, no build system is involved
- **Documentation preview command**: Any Markdown renderer (e.g., `grip minio_c07e5b49d477.md`, VS Code Markdown preview, or GitHub rendering)
- **Diagram generation command**: Mermaid diagrams are embedded inline in the Markdown using fenced mermaid code blocks and rendered natively by GitHub and most Markdown viewers
- **Documentation deployment command**: Not applicable — file is placed in `blitzy/documentation/` and committed via standard git workflow
- **Default format**: Markdown with Mermaid diagrams for flowcharts and sequence diagrams
- **Citation requirement**: Every technical claim must reference the source file and function or line range. Format: `Source: cmd/file.go:LineRange` or `(see function_name in cmd/file.go)`
- **Style guide**: Q&A investigative format with progressive complexity — start with healthy baseline, introduce single failure, then double failure. Each section provides the thinking/rationale behind the answer before stating the conclusion.
- **Documentation validation**: Manual review — ensure all code references correspond to actual file paths and function names; verify quorum arithmetic produces correct results for the 4-drive case (2+2 default parity with write quorum of 3)

## 0.10 Rules for Documentation

### 0.10.1 User-Specified Rules

The following rules are explicitly specified by the user and the project's implementation rules:

- **SWE-AtlasQnA-Repo**: Create a new markdown document named `minio_c07e5b49d477.md` that comprehensively answers the questions posed in the prompt. Provide thinking/rationale behind the answers. Do not make assumptions — base answers on the code as the truth. Do not modify any existing files in the source repository. Place the generated document in the `blitzy/documentation` directory.
- **No repository modifications**: The user explicitly states: "the repository itself should remain unchanged and anything temporary should be cleaned up afterward." This means the only write operation is creating the output file in `blitzy/documentation/`.
- **Code as truth**: All answers must be grounded in the actual MinIO source code, not in external documentation, blog posts, or assumptions about how the system "should" work.
- **Include rationale**: Each answer must explain the thinking process — why the code behaves the way it does, not just what it does.

### 0.10.2 Derived Documentation Standards

Based on the nature of the request and best practices for code-investigation documentation:

- **Source citations for all technical details**: Every section must reference at least one source file path with function name or line range
- **Concrete examples over generalizations**: Use the specific 4-drive scenario throughout, with actual numeric calculations, not abstract N-drive formulas
- **Progressive disclosure**: Structure answers from simple (healthy state) to complex (degraded state, healing)
- **Both paths documented**: For every decision point (write succeeds vs. fails, healing triggers vs. doesn't), document both the success and failure paths
- **No speculative statements**: If a behavior cannot be confirmed from source code, state that explicitly rather than guessing
- **Mermaid diagrams for complex flows**: Use diagrams to make the write-path decision tree and healing lifecycle visually clear

## 0.11 References

### 0.11.1 Source Code Files Searched and Analyzed

The following files were retrieved and analyzed during context gathering to derive the conclusions in this Agent Action Plan:

**Core Quorum and Erasure Logic**:
- `cmd/erasure.go` — erasureObjects struct, defaultWQuorum(), defaultRQuorum(), diskErrToDriveState()
- `cmd/erasure-coding.go` — Erasure struct, NewErasure() with Reed-Solomon encoder
- `cmd/erasure-metadata.go` — objectQuorumFromMeta(), commonParity(), listObjectParities()
- `cmd/erasure-metadata-utils.go` — reduceWriteQuorumErrs(), reduceReadQuorumErrs(), reduceErrs(), readAllFileInfo(), hashOrder(), diskCount(), shuffleDisks*()
- `cmd/erasure-errors.go` — errErasureWriteQuorum, errErasureReadQuorum, errNoHealRequired

**Write Path**:
- `cmd/erasure-object.go` — putObject(), PutObject(), CopyObject(), countOnlineDisks(), availability-optimized parity upgrade logic
- `cmd/erasure-encode.go` — Encode(), multiWriter struct with Write() quorum enforcement
- `cmd/erasure-common.go` — getOnlineDisks(), getOnlineLocalDisks(), getLocalDisks()

**Disk Storage and Failure Detection**:
- `cmd/xl-storage.go` — newXLStorage(), checkFormatJSON(), GetDiskID(), permission error to errDiskAccessDenied mapping
- `cmd/xl-storage-disk-id-check.go` — checkDiskStale(), IsOnline(), monitorDiskStatus(), monitorDiskWritable()
- `cmd/storage-errors.go` — errDiskAccessDenied, errDiskNotFound, errFaultyDisk, errUnformattedDisk, errDiskOngoingReq
- `cmd/storage-datatypes.go` (summary only)

**Erasure Set Management and Reconnection**:
- `cmd/erasure-sets.go` — erasureSets struct, connectDisks(), monitorAndConnectEndpoints(), getDiskMap(), defaultMonitorConnectEndpointInterval
- `cmd/erasure-server-pool.go` — Health(), HealthOptions, HealthResult struct, per-pool/set evaluation logic, BackendInfo()
- `cmd/prepare-storage.go` — connectLoadInitFormats(), waitForFormatErasure()

**Health Endpoints**:
- `cmd/healthcheck-handler.go` — ClusterCheckHandler, ClusterReadCheckHandler, ReadinessCheckHandler, LivenessCheckHandler, checkHealth()
- `cmd/healthcheck-router.go` — registerHealthCheckRouter(), route paths

**Healing and Recovery**:
- `cmd/background-newdisks-heal-ops.go` — initAutoHeal(), monitorLocalDisksAndHeal(), healFreshDisk(), getLocalDisksToHeal(), healingTracker
- `cmd/background-heal-ops.go` — healTask, healResult, healRoutine
- `cmd/global-heal.go` — healErasureSet(), healBucket(), healObject()
- `cmd/erasure-healing.go` — healObject(), shouldHealObjectOnDisk(), auditHealObject()
- `cmd/mrf.go` — mrfState, PartialOperation, healRoutine(), startMRFPersistence()

**Metrics and Observability**:
- `cmd/metrics-v3-cluster-erasure-set.go` — erasure set health/quorum/tolerance metrics, loadClusterErasureSetMetrics()
- `cmd/metrics-v3-cluster-health.go` — cluster-wide drive count metrics, loadClusterHealthDriveMetrics()
- `cmd/metrics-v3-system-drive.go` — per-drive health state (offline/online/healing), timeout/io/availability error counters

**Documentation Files Referenced**:
- `docs/erasure/README.md` — Erasure Code Quickstart Guide (N/2 parity, drive tolerance, EC set sizing)
- `docs/distributed/README.md` — Distributed MinIO Quickstart (data protection, high availability, consistency guarantees)
- `docs/distributed/DESIGN.md` — Erasure set selection algorithm (GCD-based, 2–16 drives per set)
- `docs/distributed/SIZING.md` — Parity/tolerance tables for various drive configurations

**Folder Structure Explored**:
- Root (`""`) — full repository structure
- `cmd/` — primary server implementation package
- `internal/` — reusable runtime infrastructure
- `docs/` — documentation hub
- `docs/erasure/` — erasure coding documentation
- `docs/distributed/` — distributed mode documentation

### 0.11.2 Attachments and External Resources

- **Attachments provided**: None (0 attachments)
- **Figma URLs**: None
- **External URLs**: None
- **Environment files**: No environment files provided in `/tmp/environments_files/`
- **Setup instructions**: None provided by user
- **Branch name**: `minio_c07e5b49d477` (source branch for output file naming)

