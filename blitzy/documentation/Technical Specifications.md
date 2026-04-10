# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create new documentation** that comprehensively explains MinIO's erasure-coded healing decision-making process, grounded in source-code evidence from the repository rather than theoretical abstractions.

- **Category**: Create new documentation
- **Documentation type**: Technical investigation guide / Q&A deep-dive document
- **Output file**: `blitzy/documentation/minio_c07e5b49d477.md`

The user is onboarding to the MinIO repository and specifically wants to understand:

- **Healing decision logic under ambiguity**: How MinIO's healing process resolves conflicting states in a 4-disk erasure-coded instance when disks hold a mixture of valid data, corrupted data, and missing data
- **Restore vs. leave-alone decisions**: Whether MinIO always reconstructs objects from remaining valid shards, or whether it decides certain objects should remain deleted or degraded — and the specific criteria for each outcome
- **Runtime evidence**: The actual healing output, status indicators, and log entries that reveal the before/after state of each disk and explain why MinIO chose to restore versus leave an object alone
- **Boundary conditions**: The minimum number of valid shards required for healing success, the exact error that appears when healing cannot recover an object, and whether healing behavior differs between a partially failed write and a partially failed delete
- **Observable proof over theory**: The user explicitly requests actual runtime evidence of how MinIO resolves conflicting states, not just theoretical explanations

### 0.1.2 Special Instructions and Constraints

- **CRITICAL**: "Don't modify any source files" — no changes to any existing `.go`, `.sh`, `.md`, or any other file in the repository
- **CRITICAL**: "Create whatever test scenarios you need to demonstrate this behavior and clean them once done" — ephemeral test scenarios are expected but all evidence must be captured and documented before cleanup
- **Implementation Rule**: Create a new markdown document named `minio_c07e5b49d477.md` placed in the `blitzy/documentation` directory
- **Implementation Rule**: Provide thinking/rationale behind the answers, base all answers on the code as the truth, do not make assumptions
- **Style**: Evidence-driven, code-referenced investigation guide showing actual source-code logic, function paths, and test-case outcomes

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To **explain healing decision logic**, we will create documentation that traces the call path through `HealObject()` → `healObject()` → `readAllFileInfo()` → `objectQuorumFromMeta()` → `listOnlineDisks()` → `disksWithAllParts()` → `shouldHealObjectOnDisk()` → `erasure.Heal()`, citing specific lines from `cmd/erasure-healing.go`, `cmd/erasure-healing-common.go`, and `cmd/erasure-decode.go`
- To **document restore-vs-leave-alone decisions**, we will trace the `isObjectDangling()` function logic at `cmd/erasure-healing.go:968`, the `cannotHeal` threshold check at line 428, and the `deleteIfDangling()` path at `cmd/erasure-object.go:482`
- To **surface runtime evidence**, we will document the `madmin.HealResultItem` structure's `Before.Drives` / `After.Drives` fields with their `DriveState` values (`ok`, `missing`, `corrupt`, `offline`), and the `healTrace()` function at `cmd/erasure-healing.go:1090` that publishes structured trace records
- To **define boundary conditions**, we will cite the quorum formulas from `cmd/erasure.go:85-96` (`defaultRQuorum` = `setDriveCount - defaultParityCount`), the `errErasureReadQuorum` error at `cmd/erasure-errors.go:23`, and the test data in `cmd/erasure-heal_test.go` that enumerates pass/fail thresholds
- To **distinguish partial write vs. partial delete behavior**, we will reference `TestHealingDanglingObject` at `cmd/erasure-healing_test.go:647` and the dangling detection cases in `TestIsObjectDangling` at line 40

### 0.1.4 Inferred Documentation Needs

Based on code analysis, the following additional documentation needs have been identified:

- **Quorum arithmetic for a 4-disk setup**: With 4 disks and default EC:2 parity, `readQuorum = 4 - 2 = 2` data blocks — this specific numeric threshold needs explicit documentation since it is the smallest erasure set MinIO supports
- **Drive state classification**: The 5 possible disk states documented in `cmd/erasure-healing-common.go:196-213` (online, offline, availableWithParts, outdated, missingParts) must be explicitly described since they form the decision taxonomy
- **Dangling object heuristics**: The `isObjectDangling()` function uses different thresholds for delete-markers versus data objects versus objects with missing metadata — all three must be documented
- **Bitrot detection escalation**: `HealObject()` at `cmd/erasure-healing.go:1080` automatically escalates from `HealNormalScan` to `HealDeepScan` when `errFileCorrupt` is detected — this implicit behavior should be documented
- **Inline vs. on-disk data healing**: The healing path diverges for inline data (small objects) vs. on-disk parts — `cmd/erasure-healing.go:532-535` handles `InlineData()` separately

## 0.2 Documentation Discovery and Analysis

### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a **Markdown-driven documentation structure** under `docs/` with topical subdirectories, but **no existing documentation specifically covering healing decision logic or healing output interpretation**. The documentation infrastructure lacks a documentation generator configuration (no `mkdocs.yml`, `docusaurus.config.js`, or `sphinx/conf.py` was found); documentation is served via raw Markdown files and a Jekyll site configuration (`_config.yml` with `jekyll-theme-minimal`).

**Existing erasure documentation found:**
- `docs/erasure/README.md` — A quickstart guide covering erasure-code concepts (Reed-Solomon, N/2 default parity, bitrot protection), but contains **no healing decision logic, no output format documentation, and no boundary condition analysis**
- `docs/erasure/storage-class/README.md` — Storage class configuration for custom data/parity ratios
- `docs/distributed/DESIGN.md` — Distributed architecture design overview

**Documentation framework**: None (raw Markdown served by GitHub/Jekyll)
- API documentation tools in use: None (no JSDoc, Sphinx, or Godoc generator configured)
- Diagram tools detected: None pre-configured; Mermaid will be used inline
- Documentation hosting: GitHub repository + Jekyll (`_config.yml`)

### 0.2.2 Repository Code Analysis for Documentation

The following search patterns were used to locate the code that must be documented:

**Primary healing implementation files:**

| File Path | Purpose | Relevance |
|-----------|---------|-----------|
| `cmd/erasure-healing.go` | Core healing decision logic, object/dir healing, dangling detection | **Critical** — Contains `healObject()`, `isObjectDangling()`, `shouldHealObjectOnDisk()`, `HealObject()` |
| `cmd/erasure-healing-common.go` | Common healing helpers: quorum selection, disk classification, part verification | **Critical** — Contains `listOnlineDisks()`, `disksWithAllParts()`, `commonTime()`, `convPartErrToInt()` |
| `cmd/erasure-decode.go` | Parallel read, shard reconstruction, `Erasure.Heal()` | **Critical** — Contains `parallelReader.Read()`, `Erasure.Heal()`, `canDecode()` |
| `cmd/erasure-coding.go` | Reed-Solomon erasure coding primitives, `NewErasure()` | **High** — Contains `Erasure` struct, `DecodeDataAndParityBlocks()`, self-test |
| `cmd/erasure-object.go` | Object CRUD operations, `deleteIfDangling()` | **High** — Contains dangling deletion with audit tags |
| `cmd/erasure.go` | Erasure set coordinator, quorum functions | **High** — Contains `defaultRQuorum()`, `defaultWQuorum()`, `diskErrToDriveState()` |
| `cmd/erasure-metadata.go` | Metadata reconciliation, `objectQuorumFromMeta()` | **High** — Contains quorum computation from metadata |
| `cmd/global-heal.go` | Background healing orchestration | **Medium** — Orchestrates `healErasureSet()` |
| `cmd/background-newdisks-heal-ops.go` | New disk healing tracker | **Medium** — Contains `healingTracker`, `.healing.bin` persistence |
| `cmd/admin-heal-ops.go` | Admin heal API, sequence management | **Medium** — Contains `healSequence`, result reporting |
| `cmd/storage-datatypes.go` | Part check status constants | **High** — Defines `checkPartSuccess`, `checkPartFileNotFound`, etc. |
| `cmd/erasure-errors.go` | Erasure error definitions | **High** — Defines `errErasureReadQuorum` |

**Test files providing scenario coverage:**

| File Path | Purpose | Key Scenarios |
|-----------|---------|---------------|
| `cmd/erasure-healing_test.go` | End-to-end healing tests | `TestIsObjectDangling`, `TestHealing`, `TestHealingVersioned`, `TestHealingDanglingObject`, `TestHealCorrectQuorum`, `TestHealObjectCorruptedXLMeta`, `TestHealObjectCorruptedParts`, `TestHealObjectErasure`, `TestHealEmptyDirectoryErasure`, `TestHealLastDataShard` |
| `cmd/erasure-heal_test.go` | Low-level erasure shard heal tests | `TestErasureHeal` — 20 test cases across varying data/parity/offline/bad disk combinations with explicit `shouldFail` flags |
| `cmd/erasure-healing-common_test.go` | Healing helper tests | `TestCommonTime`, `TestListOnlineDisks`, `TestDisksWithAllParts`, `TestCommonParities` |

**Build/verification scripts:**

| File Path | Purpose |
|-----------|---------|
| `buildscripts/verify-healing.sh` | End-to-end 3-node healing regression harness — wipes node data, restarts, verifies heal |
| `buildscripts/verify-healing-empty-erasure-set.sh` | Tests healing of completely wiped erasure sets |

### 0.2.3 Web Search Research Conducted

No external web search is necessary. The documentation is derived entirely from codebase analysis per the user's instruction to "base your answers on the code as the truth." The MinIO repository contains sufficient test cases, source code comments, and verification scripts to fully document the healing decision-making process with code-sourced evidence.

## 0.3 Documentation Scope Analysis

### 0.3.1 Code-to-Documentation Mapping

The following modules contain the source of truth for healing behavior that must be documented:

**Module: `cmd/erasure-healing.go` — Core Healing Decision Engine**
- Public APIs: `HealObject()` (line 1039), `healObject()` (line 258), `healObjectDir()` (line 698), `checkAbandonedParts()` (line 662)
- Key decision functions: `shouldHealObjectOnDisk()` (line 156), `isObjectDangling()` (line 968), `isObjectDirDangling()` (line 913), `danglingMetaErrsCount()` (line 934), `danglingPartErrsCount()` (line 950)
- Output structures: `defaultHealResult()` (line 787), `auditHealObject()` (line 221), `healTrace()` (line 1090)
- Current documentation: **Missing** — No existing documentation covers this logic
- Documentation needed: Full decision-tree documentation with code citations, drive-state classification, before/after trace output format

**Module: `cmd/erasure-healing-common.go` — Quorum and Disk Classification**
- Public APIs: `listOnlineDisks()` (line 219), `disksWithAllParts()` (line 291), `commonTime()` (line 114), `commonETag()` (line 122)
- Key classification helpers: `convPartErrToInt()` (line 258), `partNeedsHealing()` (line 276), `hasPartErr()` (line 280), `filterOnlineDisksInplace()` (line 186)
- 5-state disk model documented in source comments at lines 196-213
- Current documentation: **Missing**
- Documentation needed: Disk state taxonomy, quorum selection algorithm, part verification logic

**Module: `cmd/erasure-decode.go` — Shard Reconstruction**
- Public APIs: `Erasure.Heal()` (line 317), `Erasure.Decode()` (line 239)
- Key logic: `parallelReader.Read()` (line 127), `canDecode()` (line 116) — quorum threshold `bufCount >= p.dataBlocks`
- Error reporting: `errErasureReadQuorum` with `offline-disks=X/Y` formatting (line 234)
- Current documentation: **Missing**
- Documentation needed: Minimum-shards-for-reconstruction formula, error format documentation

**Module: `cmd/erasure.go` — Quorum Formulas**
- `defaultRQuorum()` (line 94): `setDriveCount - defaultParityCount`
- `defaultWQuorum()` (line 85): `dataCount` or `dataCount + 1` when data equals parity
- Current documentation: **Missing for 4-disk specifics**
- Documentation needed: Explicit quorum calculation for EC:2 on 4 disks

**Module: `cmd/erasure-object.go` — Dangling Deletion**
- `deleteIfDangling()` (line 482): Deletes dangling objects with full audit trail including set/pool indexes, metadata errors, data errors, and offline disk count
- Current documentation: **Missing**
- Documentation needed: Dangling deletion criteria and audit output format

**Module: `cmd/storage-datatypes.go` — Part Check Constants**
- Constants at lines 536-545: `checkPartUnknown` (0), `checkPartSuccess` (1), `checkPartDiskNotFound` (2), `checkPartVolumeNotFound` (3), `checkPartFileNotFound` (4), `checkPartFileCorrupt` (5)
- Current documentation: **Missing**
- Documentation needed: Status code reference table

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, documentation gaps include:

- **Undocumented healing decision tree**: The entire path from `HealObject()` through quorum evaluation to reconstruct/delete/skip is only documented in source comments — no user-facing documentation exists
- **Undocumented drive state taxonomy**: The 5-state model (online, offline, availableWithParts, outdated, missingParts) appears only in code comments at `cmd/erasure-healing-common.go:196-213`
- **Undocumented healing output format**: The `madmin.HealResultItem` structure with `Before.Drives`/`After.Drives` and `DriveState` values has no user-facing documentation showing what actual heal output looks like
- **Undocumented boundary conditions**: The exact shard threshold for successful healing in a 4-disk setup (need ≥ 2 valid data blocks) and the error format `"Read failed. Insufficient number of drives online"` are only discoverable by reading source code
- **Undocumented partial-write vs. partial-delete asymmetry**: `TestHealingDanglingObject` demonstrates that partial deletes create dangling delete-markers that are cleaned during healing, while partial writes create objects that are healed to full quorum — this behavioral difference is undocumented
- **Missing test-evidence reference**: The extensive table-driven test data in `erasureHealTests` (20 cases) quantifies exactly which disk-failure combinations succeed vs. fail, but this data is not available in any documentation

## 0.4 Documentation Implementation Design

### 0.4.1 Documentation Structure Planning

The output document `blitzy/documentation/minio_c07e5b49d477.md` will be structured as a comprehensive investigation guide:

```
blitzy/
└── documentation/
    └── minio_c07e5b49d477.md
        ├── Introduction and Scope
        ├── Erasure Coding Fundamentals for a 4-Disk Setup
        │   ├── Data/Parity Layout
        │   ├── Quorum Arithmetic (readQuorum, writeQuorum)
        │   └── The 5 Disk States
        ├── Healing Decision Tree
        │   ├── Entry Point: HealObject()
        │   ├── Metadata Collection: readAllFileInfo()
        │   ├── Quorum Resolution: objectQuorumFromMeta()
        │   ├── Online Disk Selection: listOnlineDisks()
        │   ├── Part Verification: disksWithAllParts()
        │   ├── Per-Disk Heal Decision: shouldHealObjectOnDisk()
        │   ├── Cannot-Heal Threshold
        │   └── Dangling Object Detection: isObjectDangling()
        ├── Scenario Analysis: What Happens in Each Case
        │   ├── Case 1: One Disk Missing Data — Heal Succeeds
        │   ├── Case 2: One Disk Corrupted — Heal Succeeds
        │   ├── Case 3: One Missing + One Corrupted — Heal Succeeds
        │   ├── Case 4: Three Disks Missing — Cannot Heal (Dangling)
        │   ├── Case 5: All Disks Missing — Object Gone
        │   ├── Case 6: Partial Write (Under-Quorum Object)
        │   └── Case 7: Partial Delete (Under-Quorum Delete Marker)
        ├── Healing Output: Status Indicators and Trace Records
        │   ├── HealResultItem Structure (Before/After Drive States)
        │   ├── Drive State Values (ok, missing, corrupt, offline)
        │   ├── Trace Output via healTrace()
        │   └── Audit Output via auditHealObject()
        ├── Boundary Conditions
        │   ├── Minimum Valid Shards for Successful Healing
        │   ├── Error When Healing Cannot Recover
        │   ├── Bitrot Scan Escalation
        │   └── Partial Write vs. Partial Delete Behavior
        ├── Evidence from Test Suite
        │   ├── erasureHealTests Pass/Fail Matrix
        │   ├── TestIsObjectDangling Case Outcomes
        │   └── TestHealObjectCorruptedParts Reconstruction Proof
        └── Source References
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach:**
- Extract healing decision logic from `cmd/erasure-healing.go` by tracing the `healObject()` function flow line by line
- Extract quorum formulas from `cmd/erasure.go:85-96` and `cmd/erasure-metadata.go:531-564`
- Extract drive-state classification from `cmd/erasure-healing-common.go:196-213` source comments and the `shouldHealObjectOnDisk()` function
- Extract boundary conditions from `cmd/erasure-heal_test.go:29-63` (`erasureHealTests` table)
- Extract dangling detection thresholds from `cmd/erasure-healing.go:968-1036` (`isObjectDangling()`)
- Extract output format from `cmd/erasure-healing.go:277-405` (HealResultItem construction) and `cmd/erasure-healing.go:1090-1116` (trace publishing)
- Generate scenario examples by analyzing test cases in `cmd/erasure-healing_test.go` (11 test functions)

**Documentation Standards:**
- Markdown formatting with proper headers (`#`, `##`, `###`)
- Mermaid diagrams for the healing decision flowchart and scenario state transitions
- Code-path citations in the format `Source: cmd/erasure-healing.go:258`
- Tables for drive-state taxonomy, error code reference, and test-case pass/fail matrix
- Consistent terminology: "shard" for individual erasure block, "disk" for storage backend, "quorum" for minimum agreement threshold

### 0.4.3 Diagram and Visual Strategy

The following Mermaid diagrams will be created:

- **Healing Decision Flowchart**: A flowchart tracing the path from `HealObject()` entry through each decision gate (all-not-found check → quorum resolution → online disk selection → part verification → should-heal check → cannot-heal threshold → dangling detection → reconstruct or delete)
- **4-Disk State Diagram**: Showing possible states of each disk (valid data / corrupted / missing / offline) and the healing outcome for each combination
- **Before/After Drive State Table Diagram**: Visual representation of how `HealResultItem.Before.Drives` and `HealResultItem.After.Drives` change during healing
- **Dangling Detection Decision Tree**: Separate flowchart for `isObjectDangling()` showing the different paths for delete-markers, data objects, and metadata-only presence

## 0.5 Documentation File Transformation Mapping

### 0.5.1 File-by-File Documentation Plan

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---------------------------|----------------|------------------|-----------------|
| `blitzy/documentation/minio_c07e5b49d477.md` | CREATE | `cmd/erasure-healing.go`, `cmd/erasure-healing-common.go`, `cmd/erasure-decode.go`, `cmd/erasure-coding.go`, `cmd/erasure.go`, `cmd/erasure-object.go`, `cmd/erasure-metadata.go`, `cmd/erasure-errors.go`, `cmd/storage-datatypes.go`, `cmd/erasure-heal_test.go`, `cmd/erasure-healing_test.go`, `cmd/erasure-healing-common_test.go`, `cmd/global-heal.go`, `cmd/background-newdisks-heal-ops.go`, `cmd/admin-heal-ops.go`, `buildscripts/verify-healing.sh`, `docs/erasure/README.md` | Comprehensive Q&A document covering healing decision logic, scenario analysis, runtime evidence, boundary conditions, and test-suite evidence for MinIO's 4-disk erasure-coded healing |

### 0.5.2 New Documentation File Detail

```
File: blitzy/documentation/minio_c07e5b49d477.md
Type: Technical Investigation Guide / Q&A Deep-Dive
Source Code:
  - cmd/erasure-healing.go (primary: healing decision engine)
  - cmd/erasure-healing-common.go (quorum helpers, disk classification)
  - cmd/erasure-decode.go (shard reconstruction, Erasure.Heal())
  - cmd/erasure-coding.go (Reed-Solomon primitives)
  - cmd/erasure.go (quorum formulas)
  - cmd/erasure-object.go (deleteIfDangling)
  - cmd/erasure-metadata.go (objectQuorumFromMeta)
  - cmd/erasure-errors.go (error definitions)
  - cmd/storage-datatypes.go (part check status constants)
  - cmd/erasure-heal_test.go (low-level heal pass/fail matrix)
  - cmd/erasure-healing_test.go (end-to-end healing scenarios)
  - cmd/erasure-healing-common_test.go (helper function tests)
  - cmd/global-heal.go (background heal orchestration)
  - cmd/background-newdisks-heal-ops.go (disk heal tracker)
  - cmd/admin-heal-ops.go (admin heal sequence)
  - buildscripts/verify-healing.sh (end-to-end regression script)
  - docs/erasure/README.md (existing erasure code guide)
Sections:
  - Introduction and Scope (purpose, 4-disk setup context)
  - Erasure Coding Fundamentals (data/parity layout, quorum math, 5 disk states)
  - Healing Decision Tree (full code-traced flowchart with citations)
  - Scenario Analysis (7 cases with disk-state tables showing outcomes)
  - Healing Output (HealResultItem structure, DriveState values, trace/audit format)
  - Boundary Conditions (minimum shards, error messages, partial write vs. delete)
  - Evidence from Test Suite (erasureHealTests matrix, dangling test cases, reconstruction proof)
  - Source References (all files cited)
Diagrams:
  - Healing decision flowchart (Mermaid)
  - 4-disk state outcome table (Mermaid/table)
  - Dangling detection decision tree (Mermaid)
  - Before/After drive state visualization (table)
Key Citations:
  - cmd/erasure-healing.go:156 (shouldHealObjectOnDisk)
  - cmd/erasure-healing.go:258 (healObject)
  - cmd/erasure-healing.go:428 (cannotHeal threshold)
  - cmd/erasure-healing.go:968 (isObjectDangling)
  - cmd/erasure-healing-common.go:116 (canDecode)
  - cmd/erasure-healing-common.go:196-213 (5-state disk model)
  - cmd/erasure-healing-common.go:291 (disksWithAllParts)
  - cmd/erasure-decode.go:317 (Erasure.Heal)
  - cmd/erasure.go:85-96 (quorum formulas)
  - cmd/erasure-metadata.go:531 (objectQuorumFromMeta)
  - cmd/erasure-errors.go:23 (errErasureReadQuorum)
  - cmd/storage-datatypes.go:536-545 (part check constants)
  - cmd/erasure-heal_test.go:29-63 (20-case heal pass/fail table)
  - cmd/erasure-healing_test.go:40-310 (TestIsObjectDangling cases)
  - cmd/erasure-healing_test.go:1297-1454 (TestHealObjectCorruptedParts)
```

### 0.5.3 Documentation Configuration Updates

No documentation configuration updates are required. The project does not use a documentation generator framework (no `mkdocs.yml`, `docusaurus.config.js`, or `sphinx/conf.py`). The new document is a standalone Markdown file placed in the `blitzy/documentation/` directory as specified by the implementation rules.

### 0.5.4 Cross-Documentation Dependencies

- **No shared content dependencies**: The new document is self-contained and does not require updating any existing navigation, TOC, or index files
- **Reference link to existing doc**: `docs/erasure/README.md` provides foundational context that the new document builds upon — a reference link will be included
- **No index or glossary updates**: The `blitzy/documentation/` directory is independent of the existing `docs/` hierarchy

## 0.6 Dependency Inventory

### 0.6.1 Documentation Dependencies

The documentation task is purely Markdown-based and does not require external documentation tooling. However, the following dependencies are relevant to understanding the code being documented and to the runtime environment in which the healing behavior operates:

| Registry | Package Name | Version | Purpose |
|----------|--------------|---------|---------|
| Go toolchain | go | 1.23 | Language runtime for MinIO server (from `go.mod`) |
| Go module | `github.com/minio/minio` | HEAD (`minio_c07e5b49d477` branch) | The MinIO server codebase being documented |
| Go module | `github.com/minio/madmin-go/v3` | (pinned in `go.sum`) | Admin API types including `HealResultItem`, `HealDriveInfo`, `HealOpts`, `HealScanMode`, `DriveState*` constants |
| Go module | `github.com/klauspost/reedsolomon` | (pinned in `go.sum`) | Reed-Solomon erasure coding engine used by `Erasure.Heal()` and `Erasure.Decode()` |
| Go module | `github.com/dustin/go-humanize` | (pinned in `go.sum`) | Used in test files for human-readable size constants |
| Go module | `github.com/minio/pkg/v3` | (pinned in `go.sum`) | Provides `sync/errgroup` used in parallel disk operations |
| N/A | Mermaid | Inline (rendered by GitHub/viewers) | Diagram rendering for flowcharts and decision trees in the Markdown output |

### 0.6.2 Documentation Reference Updates

No link updates are required for existing documentation files. The new document is created in an independent directory (`blitzy/documentation/`) and does not alter existing documentation cross-references. Internal references within the new document will use relative paths to source files:

- `cmd/erasure-healing.go` — referenced as `Source: cmd/erasure-healing.go:LINE`
- `cmd/erasure-heal_test.go` — referenced for test evidence
- `docs/erasure/README.md` — referenced for background context

## 0.7 Coverage and Quality Targets

### 0.7.1 Documentation Coverage Metrics

**Current coverage analysis (before this task):**
- Healing decision logic documented: 0/7 decision gates (0%)
- Healing output format documented: 0/4 output structures (0%)
- Boundary conditions documented: 0/4 thresholds (0%)
- Scenario analysis documented: 0/7 scenarios (0%)

**Target coverage (after this task):**
- Healing decision logic: 7/7 decision gates (100%) — `isAllNotFound`, `objectQuorumFromMeta`, `listOnlineDisks`, `disksWithAllParts`, `shouldHealObjectOnDisk`, `cannotHeal` threshold, `isObjectDangling`
- Healing output format: 4/4 output structures (100%) — `HealResultItem`, `HealDriveInfo`/DriveState values, `healTrace()` trace records, `auditHealObject()` audit entries
- Boundary conditions: 4/4 thresholds (100%) — minimum shards for success, error message on failure, bitrot scan escalation, partial-write vs. partial-delete divergence
- Scenario analysis: 7/7 scenarios (100%) — one missing, one corrupt, one missing + one corrupt, three missing (dangling), all missing, partial write, partial delete

### 0.7.2 Documentation Quality Criteria

**Completeness requirements:**
- Every healing decision gate must have a source code citation with file path and line number
- Every scenario must include a "4-disk state table" showing the state of each disk (valid / corrupt / missing / offline) before and after healing
- Every output field must be mapped to the function that populates it, with the specific drive-state string values documented
- Every boundary condition must cite the exact code that enforces it

**Accuracy validation:**
- All code citations must reference actual function names and line numbers verified against the repository
- All quorum formulas must be validated against `cmd/erasure.go:85-96` and `cmd/erasure-metadata.go:531-564`
- All test-case outcomes must match the `shouldFail` flags in `cmd/erasure-heal_test.go:29-63`
- All dangling detection outcomes must match `TestIsObjectDangling` expected results at `cmd/erasure-healing_test.go:40-310`

**Clarity standards:**
- Technical accuracy with accessible language for onboarding engineers
- Progressive disclosure: start with 4-disk setup fundamentals, then decision tree, then scenarios, then boundary conditions
- Consistent terminology: "shard" for erasure block, "disk" for storage endpoint, "quorum" for minimum agreement threshold, "drive state" for the `madmin.DriveState*` classification

**Maintainability:**
- Every technical claim includes a `Source:` citation so future readers can verify against the codebase
- Section structure follows a logical investigation path that can be incrementally updated

### 0.7.3 Example and Diagram Requirements

- Minimum of 7 scenario examples covering the full spectrum of healing outcomes
- 3 Mermaid diagrams: healing decision flowchart, dangling detection decision tree, and 4-disk state outcome visualization
- All state tables show 4 columns (one per disk) with clear before/after labels
- Test-evidence tables cite specific test case names and expected outcomes from the test suite

## 0.8 Scope Boundaries

### 0.8.1 Exhaustively In Scope

**New documentation files:**
- `blitzy/documentation/minio_c07e5b49d477.md` — The sole deliverable: a comprehensive Markdown document answering all user questions about healing decision logic

**Source code files analyzed for documentation (read-only):**
- `cmd/erasure-healing.go` — Core healing decisions, dangling detection, trace/audit output
- `cmd/erasure-healing-common.go` — Quorum helpers, disk classification, part verification
- `cmd/erasure-decode.go` — Shard reconstruction, `Erasure.Heal()`, read-quorum error format
- `cmd/erasure-coding.go` — Reed-Solomon primitives, `NewErasure()`
- `cmd/erasure.go` — `defaultRQuorum()`, `defaultWQuorum()`, `diskErrToDriveState()`
- `cmd/erasure-object.go` — `deleteIfDangling()` dangling cleanup with audit tags
- `cmd/erasure-metadata.go` — `objectQuorumFromMeta()`, `pickValidFileInfo()`
- `cmd/erasure-errors.go` — `errErasureReadQuorum` definition
- `cmd/storage-datatypes.go` — Part check status constants (`checkPartSuccess` through `checkPartFileCorrupt`)
- `cmd/erasure-heal_test.go` — 20-case low-level heal pass/fail matrix
- `cmd/erasure-healing_test.go` — 11 end-to-end healing test functions
- `cmd/erasure-healing-common_test.go` — Helper function tests
- `cmd/global-heal.go` — Background heal orchestration
- `cmd/background-newdisks-heal-ops.go` — Disk-level healing tracker
- `cmd/admin-heal-ops.go` — Admin heal API sequence management
- `cmd/erasure-sets.go` — Erasure set topology, `HealFormat()`, reconnect logic
- `cmd/erasure-server-pool.go` — Multi-pool object layer, `HealObject()` routing
- `buildscripts/verify-healing.sh` — End-to-end healing verification script
- `buildscripts/verify-healing-empty-erasure-set.sh` — Empty erasure set healing verification
- `docs/erasure/README.md` — Existing erasure code quickstart guide

**Documentation content topics explicitly in scope:**
- How MinIO's healing process makes decisions when cluster state is ambiguous
- What happens when an object is in an inconsistent state across 4 disks
- Whether MinIO always reconstructs or sometimes leaves objects deleted/degraded
- Actual healing output showing before/after drive states
- Boundary conditions: minimum shards, error messages, partial write vs. partial delete
- Code-sourced evidence and test-suite proof

### 0.8.2 Explicitly Out of Scope

- **Source code modifications**: No `.go`, `.sh`, `.md`, or any other existing file in the repository will be modified (per user instruction: "Don't modify any source files")
- **Test file modifications**: No changes to existing test files
- **Feature additions or code refactoring**: No code changes of any kind
- **Deployment configuration changes**: No changes to Dockerfile, Helm charts, or CI/CD configurations
- **Multi-site replication healing**: The document focuses on single-instance 4-disk erasure healing, not cross-site replication repair
- **Decommissioning / rebalancing**: Data movement during decommission (`DataMov()`) is out of scope
- **KMS/encryption interactions with healing**: SSE/KMS key management during healing is not covered
- **Non-healing admin operations**: Bucket-level healing and format healing are mentioned only where they intersect object healing decisions
- **Documentation for other features**: This task is strictly scoped to healing decision logic — no other MinIO features will be documented

## 0.9 Rules for Documentation

The following rules govern the creation of this documentation, derived from user instructions and implementation constraints:

- **Do not modify any existing files in the source repository** — the documentation is created as a new file only
- **Base all answers on the code as the truth** — do not make assumptions; every claim must trace to a specific function, line, or test case in the repository
- **Provide thinking/rationale behind the answers** — explain not just what the code does, but why the healing logic makes each decision at each gate
- **Place the generated document in `blitzy/documentation/`** — the file must be named `minio_c07e5b49d477.md` matching the source branch name
- **Show actual runtime evidence** — the document must include concrete examples of what healing output looks like (drive state indicators, trace fields, error messages), not abstract descriptions
- **Cover all 7 scenario cases** — one missing, one corrupt, mixed missing+corrupt, beyond-parity missing (dangling), all-missing, partial write, partial delete
- **Include source code citations** — every technical detail must reference the specific file and line number
- **Use Mermaid diagrams** — for the healing decision flowchart and dangling detection tree to aid visual understanding
- **Cite test evidence as proof** — reference specific test case names, their input conditions, and their expected outcomes to validate documented behavior
- **No placeholder or speculative content** — every statement must be verifiable against the codebase

## 0.10 References

### 0.10.1 Files and Folders Searched

The following files and folders were searched and analyzed across the codebase to derive the conclusions in this Agent Action Plan:

**Root-level exploration:**
- Repository root (`""`) — Examined for project structure, Go module configuration, documentation directories

**Primary healing implementation files (read in full):**
- `cmd/erasure-healing.go` — Lines 1–1117: Core healing decision engine including `healObject()`, `shouldHealObjectOnDisk()`, `isObjectDangling()`, `defaultHealResult()`, `healObjectDir()`, `HealObject()`, `healTrace()`
- `cmd/erasure-healing-common.go` — Lines 1–459: Quorum helpers including `commonTime()`, `listOnlineDisks()`, `disksWithAllParts()`, `convPartErrToInt()`, `partNeedsHealing()`, 5-state disk model documentation
- `cmd/erasure-decode.go` — Lines 1–365: Parallel reader, `Erasure.Heal()`, `Erasure.Decode()`, `canDecode()`, `errErasureReadQuorum` formatting
- `cmd/erasure-coding.go` — Summary reviewed: Reed-Solomon encoder, `NewErasure()`, `EncodeData()`, `DecodeDataAndParityBlocks()`, self-test
- `cmd/erasure.go` — Lines 82–110: `defaultWQuorum()`, `defaultRQuorum()`, `diskErrToDriveState()`
- `cmd/erasure-object.go` — Lines 482–563: `deleteIfDangling()` with full audit tag construction
- `cmd/erasure-metadata.go` — Lines 531–564: `objectQuorumFromMeta()` with parity-based quorum derivation
- `cmd/erasure-errors.go` — Line 23: `errErasureReadQuorum` definition
- `cmd/storage-datatypes.go` — Lines 530–560: `checkPartSuccess` through `checkPartFileCorrupt` constants, `CheckPartsResp`

**Test files (read in full):**
- `cmd/erasure-healing_test.go` — Lines 1–1771: All 11 test functions including `TestIsObjectDangling` (13 cases), `TestHealing`, `TestHealingVersioned`, `TestHealingDanglingObject`, `TestHealCorrectQuorum`, `TestHealObjectCorruptedPools`, `TestHealObjectCorruptedXLMeta`, `TestHealObjectCorruptedParts`, `TestHealObjectErasure`, `TestHealEmptyDirectoryErasure`, `TestHealLastDataShard`
- `cmd/erasure-heal_test.go` — Lines 1–100: `erasureHealTests` table (20 test cases with dataBlocks, disks, offDisks, badDisks, badStaleDisks, shouldFail), `TestErasureHeal`
- `cmd/erasure-healing-common_test.go` — Summary reviewed: `TestCommonTime`, `TestListOnlineDisks`, `TestDisksWithAllParts`, `TestCommonParities`

**Orchestration and admin files (summaries reviewed):**
- `cmd/global-heal.go` — Background healing coordination, `healErasureSet()`, `healBucket()`, `healObject()` helpers
- `cmd/background-newdisks-heal-ops.go` — `healingTracker`, `.healing.bin` persistence, `healFreshDisk()`, `monitorLocalDisksAndHeal()`
- `cmd/admin-heal-ops.go` — Lines 750–834: `healSequence`, result reporting, `queueHealTask()`, `traverseAndHeal()`
- `cmd/erasure-sets.go` — Summary reviewed: Erasure set topology, `HealFormat()`, endpoint reconnection
- `cmd/erasure-server-pool.go` — Summary reviewed: Multi-pool routing, `HealObject()` delegation

**Build/verification scripts:**
- `buildscripts/verify-healing.sh` — Lines 1–168: 3-node distributed healing regression script
- `buildscripts/verify-healing-empty-erasure-set.sh` — Summary reviewed: Empty erasure set healing verification

**Existing documentation:**
- `docs/erasure/README.md` — Lines 1–68: Erasure code quickstart guide
- `docs/` folder structure — Full directory listing examined

**Configuration files:**
- `go.mod` — Lines 1–30: Go 1.23 module, `github.com/minio/minio` module path
- `_config.yml` — Jekyll site configuration

### 0.10.2 Attachments and External References

- **Attachments provided**: None (0 attachments)
- **Figma screens provided**: None
- **External URLs referenced**: None — all documentation is derived exclusively from codebase analysis per the user's instruction to base answers on the code as truth

