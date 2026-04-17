# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Feature Objective

Based on the prompt, the Blitzy platform understands that the new feature requirement is to **create a comprehensive investigative document** (`minio_c07e5b49d477.md`) that answers deep, specific questions about MinIO's erasure-coding healing process and its decision-making behavior under ambiguous cluster states. This is a **read-only, documentation-only task** — no existing source files may be modified.

The specific requirements are:

- **Healing Decision Logic Under Ambiguity**: Explain how MinIO's healing subsystem decides whether to reconstruct, leave degraded, or delete an object when the cluster state is inconsistent — meaning some disks have valid data, some have corrupted data, and some have nothing.
- **Reconstruct vs. Stay-Deleted/Degraded**: Determine whether MinIO always reconstructs from valid shards, or whether there are situations where the healer deliberately decides the object should remain deleted or degraded — and identify those exact code paths.
- **Runtime Evidence of Decision-Making**: Show actual healing output (JSON `HealResultItem` structs) that reveal per-drive `before`/`after` state transitions, with drive states such as `ok`, `missing`, `corrupt`, and `offline`.
- **Boundary Conditions**: Document the minimum number of valid shards needed for healing to succeed in a 4-disk erasure coded setup, what error appears when healing fails, and how healing handles the edge between recoverable and irrecoverable.
- **Partially Failed Write vs. Partially Failed Delete**: Explain whether and how healing behavior differs between these two failure modes, referencing the MRF (Most Recently Failed) subsystem that queues partial operations for healing.
- **Build, Run, and Demonstrate**: Build the MinIO binary from source, run a local 4-disk erasure-coded instance, create test scenarios to demonstrate each healing outcome, capture the actual JSON output, and clean up after.

Implicit requirements detected:

- The document must be backed by **actual code evidence** (file paths, function names, line numbers) and **runtime observations** (actual JSON output from the healing API).
- The user expects a synthesis of both the static code analysis and the dynamic (runtime) behavior.
- All test infrastructure must be **ephemeral** — created during analysis and removed afterward.

### 0.1.2 Special Instructions and Constraints

- **CRITICAL**: Do not modify any existing source files in the repository. The rule `SWE-AtlasQnA-Repo` mandates only creating a new markdown document.
- **Output Location**: The markdown document must be placed in `blitzy/documentation/minio_c07e5b49d477.md` in the destination repository.
- **Evidence-Based**: Answers must be grounded in the code, not assumptions. The user explicitly states "base your answers on the code as the truth."
- **Provide Rationale**: Include thinking and rationale behind every answer.
- **Test Scenarios**: Create whatever test scenarios needed to demonstrate behavior, but clean up afterward.
- **No Other Code Changes**: Do not add any other code in the source repository besides the requested document.

### 0.1.3 Technical Interpretation

These feature requirements translate to the following technical implementation strategy:

- To **understand healing decision logic**, we will analyze `cmd/erasure-healing.go` (the `healObject` function at line 258), `cmd/erasure-healing-common.go` (the `disksWithAllParts` function at line 291 and `listOnlineDisks` at line 219), and `cmd/erasure-healing.go` `isObjectDangling` function at line 968.
- To **demonstrate runtime behavior**, we will build the MinIO binary from source (`go build`), launch a 4-disk erasure coded local instance, upload test objects, introduce targeted corruptions (missing data, corrupted parts, dangling metadata), trigger the admin heal API, and capture the `HealResultItem` JSON responses.
- To **identify boundary conditions**, we will examine the `objectQuorumFromMeta` function in `cmd/erasure-metadata.go` (line 531), the `cannotHeal` logic at line 428 of `cmd/erasure-healing.go`, and the `deleteIfDangling` function in `cmd/erasure-object.go` (line 482).
- To **compare write vs. delete failures**, we will trace the `addPartialOp` MRF calls in `cmd/erasure-object.go` (lines 400 and 805) and `cmd/mrf.go` `healRoutine` function.
- To **create the document**, we will write a single markdown file at `blitzy/documentation/minio_c07e5b49d477.md` containing all findings, organized with code references and actual runtime evidence.

## 0.2 Repository Scope Discovery

### 0.2.1 Comprehensive File Analysis

The MinIO repository is a large Go monolith (`go 1.23`) with the entry point at `main.go` and all server logic in the `cmd/` package. The following files are directly relevant to understanding the healing process:

**Core Healing Files (Primary Analysis Targets)**

| File | Lines | Purpose |
|---|---|---|
| `cmd/erasure-healing.go` | 1116 | Main healing orchestration: `healObject`, `healObjectDir`, `shouldHealObjectOnDisk`, `isObjectDangling`, `deleteIfDangling` invocation, `HealObject` public API, drive state classification |
| `cmd/erasure-healing-common.go` | 459 | Disk classification: `listOnlineDisks`, `disksWithAllParts`, `convPartErrToInt`, `partNeedsHealing`, quorum time/etag functions |
| `cmd/erasure-healing_test.go` | ~600+ | Tests for `isObjectDangling`, `TestHealing`, `TestHealingVersioned` — exact test case definitions for dangling detection |
| `cmd/erasure-healing-common_test.go` | 795 | Tests for `listOnlineDisks`, `disksWithAllParts`, `getLatestFileInfo` |
| `cmd/erasure-heal_test.go` | 157 | Low-level erasure shard heal test cases with varying offDisk/badDisk/badStaleDisk combinations |
| `cmd/global-heal.go` | 598 | Background healing orchestration: `healErasureSet`, heal worker parallelism, lifecycle filtering during heal |
| `cmd/background-heal-ops.go` | 189 | Heal task queue, `healRoutine` worker, `waitForLowIO` throttling |
| `cmd/admin-heal-ops.go` | 932 | Admin API heal session management: `healSequence`, `healSequenceStatus`, `LaunchNewHealSequence`, `PopHealStatusJSON` |
| `cmd/mrf.go` | ~230 | Most Recently Failed queue: `PartialOperation`, `addPartialOp`, `healRoutine` for async heal retry, MRF persistence/shutdown |
| `cmd/background-newdisks-heal-ops.go` | ~700 | New disk healing: `healingTracker`, disk formatting, `monitorLocalDisksAndHeal` |

**Erasure Engine Files (Supporting Analysis)**

| File | Lines | Purpose |
|---|---|---|
| `cmd/erasure-decode.go` | 364 | `Erasure.Heal()` function (line 317) — the low-level Reed-Solomon reconstruction that writes healed shards |
| `cmd/erasure-encode.go` | ~200 | Encoding logic for writing erasure-coded shards |
| `cmd/erasure-object.go` | ~2200 | Object CRUD operations: `deleteIfDangling` (line 482), `addPartialOp` MRF calls (lines 400, 805, 1578, 2113) |
| `cmd/erasure-metadata.go` | ~600 | `objectQuorumFromMeta` (line 531), `pickValidFileInfo`, `findFileInfoInQuorum`, `commonParity` |
| `cmd/erasure-metadata-utils.go` | ~160 | Quorum reduction functions: `reduceReadQuorumErrs`, `reduceWriteQuorumErrs` |
| `cmd/erasure-errors.go` | 28 | Error sentinels: `errErasureReadQuorum`, `errErasureWriteQuorum`, `errNoHealRequired` |
| `cmd/erasure-sets.go` | ~1800 | Erasure set management, `HealObject` dispatch |
| `cmd/erasure-server-pool.go` | ~2500 | Server pool orchestration |

**Storage Layer Files**

| File | Lines | Purpose |
|---|---|---|
| `cmd/storage-datatypes.go` | ~560 | `checkPart*` constants (line 535): `checkPartUnknown`, `checkPartSuccess`, `checkPartDiskNotFound`, `checkPartVolumeNotFound`, `checkPartFileNotFound`, `checkPartFileCorrupt` |
| `cmd/xl-storage.go` | ~3200 | Local disk storage: `CheckParts`, `VerifyFile` implementations |
| `cmd/storage-interface.go` | ~100 | `StorageAPI` interface definition |

**Configuration Files**

| File | Purpose |
|---|---|
| `internal/config/heal/heal.go` | Heal configuration: `Bitrot`, `Sleep`, `IOCount`, `DriveWorkers` settings |
| `internal/config/heal/help.go` | Heal configuration help text |
| `internal/config/scanner/scanner.go` | Scanner configuration that triggers healing |

**External SDK (madmin-go)**

| File | Purpose |
|---|---|
| `github.com/minio/madmin-go/v3/heal-commands.go` | `HealResultItem`, `HealDriveInfo`, `HealOpts`, `HealScanMode`, `DriveState*` constants, `GetMissingCounts`, `GetCorruptedCounts`, `GetOnlineCounts`, `GetOfflineCounts` |

**Build Scripts**

| File | Purpose |
|---|---|
| `buildscripts/verify-healing.sh` | Integration test: 3-node cluster healing after node wipe |
| `buildscripts/heal-manual.go` | Manual heal debugging tool |
| `buildscripts/verify-healing-empty-erasure-set.sh` | Edge-case healing of empty erasure sets |
| `buildscripts/heal-inconsistent-versions.sh` | Healing objects with inconsistent version metadata |
| `buildscripts/verify-healing-with-root-disks.sh` | Healing with root-mounted disks |

### 0.2.2 Web Search Research Conducted

No external web search was necessary for this task. All analysis is grounded exclusively in the source code and runtime behavior of the MinIO binary at commit `c07e5b49d477`. The `madmin-go/v3` SDK (v3.0.77) type definitions were read directly from the Go module cache.

### 0.2.3 New File Requirements

- **CREATE**: `blitzy/documentation/minio_c07e5b49d477.md` — The comprehensive markdown document answering all user questions about MinIO healing behavior, containing code analysis, runtime experiment results, and decision-making logic explanation.

## 0.3 Dependency Inventory

### 0.3.1 Private and Public Packages

The following packages are directly relevant to the healing analysis and documentation task:

| Registry | Package | Version | Purpose |
|---|---|---|---|
| go module | `github.com/minio/minio` | commit `c07e5b49d477` | The MinIO server itself — built from source for runtime experiments |
| go module | `github.com/minio/madmin-go/v3` | v3.0.77 | Admin SDK — defines `HealResultItem`, `HealOpts`, `HealDriveInfo`, `DriveState*` constants, `HealScanMode` |
| go module | `github.com/klauspost/reedsolomon` | v1.12.4 | Reed-Solomon erasure coding — the underlying FEC algorithm used by `Erasure.Heal()` |
| go module | `github.com/minio/pkg/v3` | (indirect) | MinIO utility libraries — `sync/errgroup`, `workers`, `console` used in healing parallelism |
| go module | `github.com/tinylib/msgp` | v1.2.4 | MessagePack serialization — used by MRF persistence (`PartialOperation.EncodeMsg`/`DecodeMsg`) |
| go module | `github.com/dustin/go-humanize` | (indirect) | Human-readable byte sizes in healing log messages |
| go | Go toolchain | 1.23 | Required Go version per `go.mod` |
| pip | `awscli` | 1.44.79 | AWS CLI used for S3 operations in test scenarios (installed at runtime, not part of repo) |

### 0.3.2 Dependency Updates

No dependency updates are required. This task is purely documentation — it creates a single markdown file. All dependencies listed above are already present in the repository's `go.mod` / `go.sum` and are used only during the build-and-run phase of the analysis.

The only runtime installation needed is:
- **Go 1.23**: Required to build the MinIO binary from source (`go build -o /tmp/minio .`)
- **awscli**: Required for S3 operations during test scenarios (creating buckets, uploading objects)

## 0.4 Integration Analysis

### 0.4.1 Existing Code Touchpoints

Since this task is a read-only analysis, no code modifications are required. However, the following integration points within the healing subsystem must be deeply understood and documented in the output markdown:

**Healing Decision Chain (Code Path Analysis)**

The healing process touches the following code in sequence during an `admin heal` API call:

- `cmd/admin-router.go` — Registers the `POST /minio/health/heal/{bucket}/{prefix}` admin endpoint
- `cmd/admin-heal-ops.go` — `LaunchNewHealSequence` creates and manages the `healSequence` state machine
- `cmd/erasure-sets.go` — `HealObject` dispatches to the correct erasure set based on object hash
- `cmd/erasure-healing.go:1039` — `HealObject` public method: reads all disk metadata, checks `isAllNotFound`, delegates to `healObject`
- `cmd/erasure-healing.go:258` — `healObject` internal: acquires lock, reads all FileInfo, computes quorum, classifies drives, executes reconstruction
- `cmd/erasure-healing-common.go:219` — `listOnlineDisks`: determines which disks have the latest valid metadata by quorum modtime/etag
- `cmd/erasure-healing-common.go:291` — `disksWithAllParts`: verifies each disk has intact part files, builds `dataErrsByDisk` and `dataErrsByPart` maps
- `cmd/erasure-healing.go:156` — `shouldHealObjectOnDisk`: classifies each disk as needing healing and identifies the reason (`errFileNotFound`, `errFileCorrupt`, `errOutdatedXLMeta`, `errPartMissingOrCorrupt`, `errLegacyXLMeta`)
- `cmd/erasure-healing.go:428` — `cannotHeal` check: if `disksToHealCount > latestMeta.Erasure.ParityBlocks`, the object cannot be healed — triggers `deleteIfDangling`
- `cmd/erasure-object.go:482` — `deleteIfDangling`: calls `isObjectDangling` to determine if the object is irrecoverable, then purges it from all disks if dangling
- `cmd/erasure-healing.go:968` — `isObjectDangling`: the core dangling detection logic that determines whether an object with mixed errors should be purged

**MRF Integration (Partially Failed Operations)**

- `cmd/erasure-object.go:400` — Read path: When a successful read detects missing/corrupt shards, `addPartialOp` queues the object for async MRF healing
- `cmd/erasure-object.go:805` — GetObjectInfo path: When metadata read shows missing blocks below `DataBlocks`, queues for MRF
- `cmd/erasure-object.go:1578` — PutObject path: When write succeeds but some disks were offline, queues for MRF
- `cmd/erasure-object.go:2113` — DeleteObject path: When delete succeeds with quorum but some disks were offline, queues for MRF
- `cmd/mrf.go:78` — `addPartialOp`: Non-blocking enqueue to the MRF channel (capacity 100,000)
- `cmd/mrf.go:210` — `healRoutine`: Consumes MRF queue, calls `healObject` or `healBucket` for each entry

**Background Healing Integration**

- `cmd/global-heal.go:152` — `healErasureSet`: The background healer that iterates all buckets and objects in an erasure set, calling `HealObject` for each
- `cmd/global-heal.go:55` — Sets `Remove: healDeleteDangling` (which is `true` by default, `cmd/data-scanner.go:60`), meaning background healing will purge irrecoverable objects
- `cmd/data-scanner.go:967` — The data scanner triggers healing for objects it finds during scanning

### 0.4.2 Healing Result Output Structure

The `HealResultItem` (from `madmin-go/v3`) is the key data structure that reveals healing decisions:

```go
type HealResultItem struct {
  Type         HealItemType
  Bucket       string
  Object       string
  VersionID    string
  ParityBlocks int
  DataBlocks   int
  DiskCount    int
  Before       struct { Drives []HealDriveInfo }
  After        struct { Drives []HealDriveInfo }
  ObjectSize   int64
}
```

Each `HealDriveInfo` carries a `State` field with one of: `ok`, `offline`, `corrupt`, `missing`, `permission-denied`, `faulty`, `root-mount`, `unknown`, `unformatted`.

The drive state classification in `cmd/erasure-healing.go` (lines 382-393) maps internal errors to these user-visible states:
- `nil` (no error) → `DriveStateOk`
- `errDiskNotFound` → `DriveStateOffline`
- `errFileNotFound`, `errFileVersionNotFound`, `errVolumeNotFound`, `errPartMissingOrCorrupt`, `errOutdatedXLMeta`, `errLegacyXLMeta` → `DriveStateMissing`
- All other errors → `DriveStateCorrupt`

## 0.5 Technical Implementation

### 0.5.1 File-by-File Execution Plan

This task requires creating exactly one new file. No existing files are modified.

- **Group 1 - Documentation Output**:
  - **CREATE**: `blitzy/documentation/minio_c07e5b49d477.md` — The comprehensive markdown document answering all user questions about MinIO healing decision-making behavior

### 0.5.2 Implementation Approach

The document creation follows a structured methodology:

**Phase 1: Static Code Analysis**

Establish the theoretical framework by tracing the healing code paths:

- Trace `HealObject` → `healObject` → `listOnlineDisks` → `disksWithAllParts` → `shouldHealObjectOnDisk` to document the complete decision tree
- Map the `cannotHeal` logic at `cmd/erasure-healing.go:428` which determines the threshold between reconstruction and dangling object cleanup
- Analyze `isObjectDangling` at `cmd/erasure-healing.go:968` for the five distinct dangling detection criteria
- Document the quorum math: for a 4-disk EC(2,2) setup, `readQuorum=2`, `writeQuorum=3`, `parityBlocks=2`
- Trace the MRF code paths for partially failed writes and deletes

**Phase 2: Runtime Experimentation**

Build MinIO, create a 4-disk instance, and execute the following test scenarios:

- **Scenario A** (1 disk missing): Remove all data from 1 of 4 disks for an object → heal → expect `before: {missing, ok, ok, ok}` → `after: {ok, ok, ok, ok}` — successful reconstruction
- **Scenario B** (2 disks missing at parity boundary): Remove data from 2 disks → heal → expect successful reconstruction since `disksToHealCount (2) == parityBlocks (2)`
- **Scenario C** (3 disks missing beyond parity): Remove data from 3 disks → heal → expect the object to be treated as irrecoverable/dangling and deleted, with the heal result not even returning an object-level result item
- **Scenario D** (corrupted part file): Overwrite a part file with garbage on 1 disk → heal with deep scan → expect `before: {missing, ok, ok, ok}` → `after: {ok, ok, ok, ok}` — detected via bitrot and healed
- **Scenario E** (dangling metadata): Remove xl.meta from 3 of 4 disks leaving only data directories → heal → expect dangling detection and cleanup
- **Scenario F** (partially failed delete simulation): Remove data from 2 disks while MinIO is stopped → restart → heal → expect reconstruction

**Phase 3: Document Synthesis**

Combine static analysis and runtime evidence into a single document with:

- Code-referenced explanations of each decision point
- Actual JSON output from each experiment
- Summary table of boundary conditions
- Comparison of partially failed write vs. delete behavior

### 0.5.3 Key Healing Decision Logic to Document

The document must explain these specific code decisions:

**Decision 1: Can we establish quorum?** (`cmd/erasure-metadata.go:531`)
- `objectQuorumFromMeta` checks that at least `N/2` (half the drives) have valid, consistent metadata
- If not, the object cannot even begin healing → triggers `deleteIfDangling`

**Decision 2: Can we identify the canonical version?** (`cmd/erasure-healing-common.go:219`)
- `listOnlineDisks` finds the most common `modTime` among disks — this determines which metadata version is "correct"
- Falls back to ETag-based matching if modtime quorum fails

**Decision 3: Are there enough healthy shards to reconstruct?** (`cmd/erasure-healing.go:428`)
- `cannotHeal = disksToHealCount > latestMeta.Erasure.ParityBlocks`
- For EC(2,2): if more than 2 disks need healing, reconstruction is impossible

**Decision 4: Is this a dangling object?** (`cmd/erasure-healing.go:968`)
- Five criteria for dangling detection based on combinations of `notFoundMetaErrs`, `notFoundPartsErrs`, `nonActionableMetaErrs`, and `nonActionablePartsErrs`
- A dangling object is purged across all disks via `deleteIfDangling`

**Decision 5: Should a specific disk be healed?** (`cmd/erasure-healing.go:156`)
- `shouldHealObjectOnDisk` checks: file not found, file corrupt, legacy XL meta, outdated XL meta, missing/corrupt parts

## 0.6 Scope Boundaries

### 0.6.1 Exhaustively In Scope

- **Output file**: `blitzy/documentation/minio_c07e5b49d477.md`
- **Source analysis files** (read-only, not modified):
  - `cmd/erasure-healing.go` — Main healing orchestration and decision logic
  - `cmd/erasure-healing-common.go` — Disk classification and part verification
  - `cmd/erasure-healing_test.go` — Test cases documenting expected dangling behavior
  - `cmd/erasure-healing-common_test.go` — Test cases for online disk detection
  - `cmd/erasure-heal_test.go` — Low-level erasure reconstruction test matrix
  - `cmd/global-heal.go` — Background heal orchestration
  - `cmd/admin-heal-ops.go` — Heal session management
  - `cmd/background-heal-ops.go` — Heal task queue
  - `cmd/mrf.go` — MRF queue for partial operation healing
  - `cmd/erasure-object.go` — `deleteIfDangling`, `addPartialOp` calls
  - `cmd/erasure-decode.go` — `Erasure.Heal()` Reed-Solomon reconstruction
  - `cmd/erasure-metadata.go` — `objectQuorumFromMeta`, `findFileInfoInQuorum`
  - `cmd/erasure-metadata-utils.go` — Quorum reduction functions
  - `cmd/erasure-errors.go` — Error sentinels
  - `cmd/storage-datatypes.go` — `checkPart*` constants
  - `cmd/xl-storage.go` — `CheckParts`, `VerifyFile`
  - `cmd/data-scanner.go` — `healDeleteDangling` constant
  - `internal/config/heal/heal.go` — Heal configuration
  - `buildscripts/verify-healing.sh` — Integration healing test script
  - `buildscripts/heal-manual.go` — Manual heal tool
  - External dependency: `github.com/minio/madmin-go/v3/heal-commands.go` — `HealResultItem`, `DriveState*` types
- **Runtime experiments** (ephemeral, cleaned up after):
  - Build MinIO binary from source
  - Launch 4-disk erasure coded local instance
  - Create test buckets and objects
  - Introduce targeted corruptions
  - Trigger admin heal API and capture JSON output
  - Verify object recovery/deletion outcomes
  - Clean up all test data and stop MinIO

### 0.6.2 Explicitly Out of Scope

- Modification of any existing source file in the repository
- Adding new Go source files, test files, or configuration files to the repository
- Performance optimization or refactoring of healing code
- Multi-node distributed healing (analysis is limited to single-node 4-disk erasure coding)
- Site replication healing (`cmd/site-replication.go`)
- ILM lifecycle healing interactions beyond what the background healer does
- Decommission/rebalance healing (`cmd/erasure-server-pool-decom.go`, `cmd/erasure-server-pool-rebalance.go`)
- FIPS or encryption-specific healing behavior
- FTP/SFTP protocol healing
- Console UI healing interface

## 0.7 Rules for Feature Addition

### 0.7.1 User-Specified Implementation Rules

- **SWE-AtlasQnA-Repo rule:** Create a new markdown document named `minio_c07e5b49d477.md` that comprehensively answers the questions posed in the prompt. Build and run the source code to analyse repository behavior as needed. Do not make assumptions — base answers on the code as the truth. Provide thinking and rationale behind the answers. Do not modify any existing files in the source repository. Do not add any other code in the source repository besides the requested document. Place the generated document in the `blitzy/documentation` directory in the destination repo.
- **Read-only constraint:** No existing source files in `cmd/`, `internal/`, `buildscripts/`, or any other repository directory may be modified. The only file created is the output document.
- **Evidence-based answers:** Every claim about healing behavior must be supported by either source code references (file, line, function) or runtime experiment output captured during the session.
- **Runtime verification required:** The user explicitly requests "actual runtime evidence" and "show me what happens in each case" — the document must include captured heal output showing before/after drive states and decision outcomes.
- **Cleanup after experiments:** All test buckets, objects, temporary MinIO data directories, and MinIO processes must be removed after experiments are complete.

### 0.7.2 Documentation Standards

- The output document must be a self-contained markdown file that a new engineer can read end-to-end to understand MinIO's healing decision-making
- Code snippets from the source must include file paths and line numbers for traceability
- Runtime output must be presented as verbatim captured JSON or formatted tables
- The document must address all six question areas the user raised:
  - Whether MinIO always reconstructs from valid shards or sometimes leaves objects deleted/degraded
  - What appears in healing output that reveals decision-making (status indicators, before/after states)
  - Whether logs explain why MinIO chose to restore versus leave something alone
  - How many valid shards are needed for healing to succeed
  - What error appears when healing cannot recover an object
  - Whether healing behavior differs between partial write failure and partial delete failure

## 0.8 References

### 0.8.1 Repository Files Analyzed

| File Path | Lines | Purpose in Analysis |
|-----------|-------|---------------------|
| `cmd/erasure-healing.go` | 1–1116 | Core healing orchestration: `healObject`, `isObjectDangling`, `deleteIfDangling` delegation, `cannotHeal` threshold |
| `cmd/erasure-healing-common.go` | 1–459 | Disk classification: `listOnlineDisks`, `disksWithAllParts`, 5-state disk model, part integrity verification |
| `cmd/erasure-healing_test.go` | 1–695 | `TestIsObjectDangling` (12+ test cases), `TestHealing`, `TestHealingVersioned` |
| `cmd/erasure-healing-common_test.go` | 1–795 | Online disk detection tests, part verification tests |
| `cmd/erasure-heal_test.go` | 1–200 | 20 erasure heal test cases varying dataBlocks/offDisks/badDisks/badStaleDisks |
| `cmd/erasure-object.go` | 1–2200 | `deleteIfDangling` (line 482), MRF `addPartialOp` calls at lines 400, 805, 1578, 2113 |
| `cmd/erasure-decode.go` | 1–400 | `Erasure.Heal()` (line 317): Reed-Solomon block reconstruction |
| `cmd/erasure-metadata.go` | 1–600 | `objectQuorumFromMeta` (line 531), `findFileInfoInQuorum` (line 289), `commonParity` |
| `cmd/erasure-metadata-utils.go` | 1–250 | `readAllFileInfo`, `reduceReadQuorum` |
| `cmd/erasure-errors.go` | 1–50 | Error sentinel definitions |
| `cmd/global-heal.go` | 1–598 | `healErasureSet`, parallel worker dispatch, bucket/object iteration |
| `cmd/mrf.go` | 1–200 | `PartialOperation`, `addPartialOp`, `healRoutine`, msgpack persistence |
| `cmd/admin-heal-ops.go` | 1–800 | `healSequence`, `LaunchNewHealSequence`, status reporting |
| `cmd/background-heal-ops.go` | 1–150 | Background heal task scheduling |
| `cmd/storage-datatypes.go` | 530–540 | `checkPart*` constants (Unknown=0, Success=1, DiskNotFound=2, VolumeNotFound=3, FileNotFound=4, FileCorrupt=5) |
| `cmd/xl-storage.go` | Selected | `CheckParts`, `VerifyFile` — disk-level part verification |
| `cmd/data-scanner.go` | Line 60 | `healDeleteDangling = true` constant |
| `internal/config/heal/heal.go` | 1–120 | Heal configuration: Bitrot, Sleep, IOCount, DriveWorkers |
| `buildscripts/verify-healing.sh` | 1–200 | 3-node cluster integration healing test |
| `buildscripts/heal-manual.go` | 1–150 | Manual heal command tool |
| `go.mod` | 1–80 | Go 1.23 requirement, `madmin-go/v3` dependency |
| External: `madmin-go/v3/heal-commands.go` | Selected | `HealResultItem`, `HealDriveInfo`, `DriveState*` constants, `Heal()` API |

### 0.8.2 Folders Explored

| Folder Path | Purpose |
|-------------|---------|
| `/tmp/blitzy/minio/minio_c07e5b49d477_1c89e5/` (root) | Repository root, build entry point |
| `cmd/` | MinIO server command code — all healing logic resides here |
| `internal/config/heal/` | Heal subsystem configuration |
| `buildscripts/` | Build and integration test scripts |
| `docs/` | Documentation (checked for healing docs) |

### 0.8.3 Tech Spec Sections Retrieved

| Section | Key Information Extracted |
|---------|--------------------------|
| 1.1 Executive Summary | Erasure coding engine overview, 4-disk EC(2,2) architecture, background healer purpose |
| 5.2 Component Details | Erasure coding engine design, background healer state machine, MRF system, bit-rot protection, heal decision chain |

### 0.8.4 Runtime Experiments Conducted

| Scenario | Object | Corruption | Disks Valid | Outcome |
|----------|--------|------------|-------------|---------|
| A: 1 disk missing | `heal-test-obj1` | Removed all data from disk1 | 3/4 | Healed: `missing→ok` |
| B: 2 disks missing (parity boundary) | `heal-test-obj2` | Removed data from disk1, disk2 | 2/4 | Healed: `missing→ok` |
| C: 3 disks missing (beyond parity) | `heal-test-obj3` | Removed data from disk1, disk2, disk3 | 1/4 | Irrecoverable: object lost (404) |
| D: Corrupted part file | `heal-test-corrupt` | Overwrote `part.1` on disk1 with 15-byte string | 3/4 (1 corrupt) | Healed: `missing→ok` (corruption detected) |
| E: Dangling metadata | `heal-test-dangling` | Removed `xl.meta` from disk1, disk2, disk3 | 1/4 metadata | Not reconstructed: dangling, metadata quorum lost |
| F: Partial delete simulation | `heal-test-partdel` | Removed data from disk1, disk2 | 2/4 | Healed: `missing→ok` (same as partial write) |

### 0.8.5 Attachments and External Resources

- **No user-provided attachments** were included with this task
- **No Figma URLs** were referenced
- **Docker image:** `ghcr.io/scaleapi/swe-atlas:swe_atlas_QnA_minio_minio_1.0` (container `andrewparkscaleai/coding-agent:minio__minio__c07e5b49d477b0774f23db3b290745aef8c01bd2`)
- **Repository branch:** `minio_c07e5b49d477`
- **Repository commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`

