# MinIO Object-Healing Decisions on a 4-Drive Erasure-Coded Instance — Reconstruct vs. Leave DELETED vs. DEGRADED

This document answers, with captured runtime evidence, how MinIO's erasure object-healing
subsystem decides whether to reconstruct an object, purge it (leave it DELETED), or leave it
unrecoverable (DEGRADED) when the object is in an inconsistent state across the drives of a
standalone **4-drive erasure set** (which auto-selects **EC:2** — 2 data + 2 parity).

Every behavioral claim below is backed by the exact command that produced it and its raw,
unedited output, and is grounded to a `file:line` anchor in the checkout under test
(`minio` commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`, server banner
`DEVELOPMENT.2024-11-25T17-10-22Z`, runtime `go1.23.2 linux/amd64`). Statements that could
not be produced through a canonical trigger are explicitly labelled **(inferred)** and cite
the governing code.

---

## 1. The question (verbatim)

> On a 4-disk erasure coded instance, what happens when an object is in an inconsistent state
> across disks (some have data, some corrupted data, some nothing) and healing runs? Does MinIO
> ALWAYS reconstruct from valid shards, or are there situations where it decides the object
> should stay DELETED or DEGRADED? I want actual runtime evidence in each case — not theory.
> What appears in the healing output that reveals decision-making: are there status indicators
> showing BEFORE/AFTER state? Do the logs explain WHY MinIO chose to restore vs leave alone?
> And the boundary conditions: (a) how many valid shards must exist for healing to succeed;
> (b) what error appears when healing cannot recover an object; (c) whether healing behavior
> differs between a partially failed WRITE vs a partially failed DELETE. Don't modify any source
> files, but create whatever test scenarios you need to demonstrate this behavior and clean them
> once done.

---

## 2. Executive answer — **No, MinIO does not always reconstruct**

Healing on a 4-drive EC:2 set resolves to exactly **one of three** observed outcomes:

| Outcome | When it happens (observed) | On-disk result |
|---------|----------------------------|----------------|
| **RECONSTRUCT** | ≤ `ParityBlocks` (≤ 2) drives need repair and the object is a normal, non-dangling object | Missing/corrupt shards are rebuilt from the survivors and written back; all 4 drives become `ok` |
| **PURGE (stay DELETED)** | The object is judged *dangling* by `isObjectDangling` — its `xl.meta` **or** its data-dir is missing beyond the dangling threshold, **or** a delete-marker version lacks quorum | The whole object/version is removed from **all** drives; `stat` returns "does not exist" |
| **DEGRADED (unrecoverable)** | More than `ParityBlocks` (> 2) drives are damaged by **corruption** (a non-actionable error), so it is neither reconstructable nor dangling | Nothing changes on disk; the object cannot be read and the heal reports `Storage resources are insufficient…` |

The single most important nuance the older draft got wrong: **corruption is *non-actionable*
for the dangling decision** (`danglingPartErrsCount`/`danglingMetaErrsCount` count only
*not-found* errors as actionable; every other error, including bit-rot corruption, increments
`nonActionableCount`, `cmd/erasure-healing.go:934,950`). Consequently a heavily-corrupted object
is **never purged** — but it may still be **unrecoverable** if too many shards are corrupt. Both
of those facts are demonstrated at runtime in §4.7 (D4).

The governing gate is `cannotHeal` (`cmd/erasure-healing.go:428`):

```go
cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
if cannotHeal && quorumETag != "" {
	// This is an object that is supposed to be removed by the dangling code
	// but we noticed that ETag is the same for all objects, let's give it a shot
	cannotHeal = false
}
```

- When `cannotHeal` is **false** → reconstruction proceeds (§4.1–§4.6).
- When `cannotHeal` is **true** → the object is routed to `deleteIfDangling`, which either
  purges it (dangling → stay DELETED, §4.8) or returns a read-quorum error and leaves it
  DEGRADED when the damage is non-actionable corruption (§4.7).

The `quorumETag != ""` override (§3.3) does **not** require unanimity; it only fires on the
modtime-fallback path and uses an ETag *quorum* (finding-corrected below).

---

## 3. The decision flow, with code anchors

### 3.1 The exported entry point and dispatch chain

`mc admin heal` (and the madmin-go `Heal()` API it wraps), the inline MRF auto-heal, and the
background scanner all converge on the same per-object worker `healObject`
(`cmd/erasure-healing.go:258`). The dispatch chain for a manual/API heal is:

```
HealHandler (cmd/admin-handlers.go:1308)
  → LaunchNewHealSequence (cmd/admin-heal-ops.go:296)
    → healSequence.healObject (cmd/admin-heal-ops.go:916) → objAPI.HealObject
      → erasureServerPools.HealObject (cmd/erasure-server-pool.go:2561)
        → erasureSets.HealObject     (cmd/erasure-sets.go:1151)
          → erasureObjects.HealObject (cmd/erasure-healing.go:1039)   [exported wrapper]
            → healObject               (cmd/erasure-healing.go:258)   [decision + repair]
```

The exported `HealObject` wrapper at `cmd/erasure-healing.go:1039` translates the internal
error through `toObjectErr` (`cmd/object-api-errors.go:152`), which is what turns the raw
sentinel `errErasureReadQuorum` into the public `InsufficientReadQuorum` type (§5.2).

### 3.2 `healObject` has **two** `deleteIfDangling` call sites, not one

This is the central correction. `healObject` can decide to purge an object at **two** distinct
points:

**(A) EARLY — metadata-quorum failure (`cmd/erasure-healing.go:307-323`).** Before any
reconstruction, it derives read quorum. If quorum cannot be established (e.g. `xl.meta` is
missing/unreadable on more than half the drives), it *immediately* tries a dangling delete:

```go
	readQuorum, _, err := objectQuorumFromMeta(ctx, partsMetadata, errs, er.defaultParityCount)
	if err != nil {
		m, derr := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs, nil, ObjectOptions{
			VersionID: versionID,
		})
		errs = make([]error, len(errs))
		if derr == nil {
			derr = errFileNotFound
			if versionID != "" {
				derr = errFileVersionNotFound
			}
			// We did find a new danging object
			return er.defaultHealResult(m, storageDisks, storageEndpoints,
				errs, bucket, object, versionID), derr
		}
		return er.defaultHealResult(m, storageDisks, storageEndpoints,
			errs, bucket, object, versionID), err
	}
```

This early site is the path that **D5** (§4.8) and the **delete-marker** cross-product
(§4.9) actually take. Note two terminal returns here: if `deleteIfDangling` returns `nil`
(the object *was* dangling and was purged) the function returns `errFileNotFound`; otherwise
it returns whatever `deleteIfDangling` returned (a read-quorum error) and the object is left
DEGRADED.

**(B) LATE — `cannotHeal` after per-drive validation (`cmd/erasure-healing.go:428-456`).**
When quorum *did* succeed but more than `ParityBlocks` drives still need repair, the same
dangling check runs again:

```go
	cannotHeal := !latestMeta.XLV1 && !latestMeta.Deleted && disksToHealCount > latestMeta.Erasure.ParityBlocks
	if cannotHeal && quorumETag != "" {
		// This is an object that is supposed to be removed by the dangling code
		// but we noticed that ETag is the same for all objects, let's give it a shot
		cannotHeal = false
	}

	if cannotHeal {
		// Allow for dangling deletes, on versions that have DataDir missing etc.
		// this would end up restoring the correct readable versions.
		m, err := er.deleteIfDangling(ctx, bucket, object, partsMetadata, errs, dataErrsByPart, ObjectOptions{
			VersionID: versionID,
		})
		errs = make([]error, len(errs))
		if err == nil {
			err = errFileNotFound
			if versionID != "" {
				err = errFileVersionNotFound
			}
			// We did find a new danging object
			return er.defaultHealResult(m, storageDisks, storageEndpoints,
				errs, bucket, object, versionID), err
		}
		for i := range errs {
			errs[i] = err
		}
		return er.defaultHealResult(m, storageDisks, storageEndpoints,
			errs, bucket, object, versionID), err
	}
```

Because the `cannotHeal` predicate contains `!latestMeta.Deleted`, **a delete marker can never
reach this LATE site**. That is why the delete-marker branch of `isObjectDangling` (§3.4) is
only ever reached through the EARLY site (A) — confirmed at runtime in §4.8.

Critically, note the `for i := range errs { errs[i] = err }` overwrite immediately before the
final return: when the object is judged **not** dangling here (non-actionable corruption), the
per-drive error slice is overwritten with the single read-quorum error, and
`defaultHealResult` then renders **every** drive with the derived state. This is the mechanism
that makes D4 report all four drives `corrupt` even though only three parts were faulted (§4.7).

`deleteIfDangling` itself lives in `cmd/erasure-object.go:482`; when the object is **not**
dangling it returns `errErasureReadQuorum` (`cmd/erasure-object.go:484-487`); when it **is**
dangling it purges and returns the `FileInfo` with a `nil` error.

### 3.3 The `quorumETag` safety override (finding-corrected, partly inferred)

`quorumETag` is produced by `listOnlineDisks` (`cmd/erasure-healing-common.go:219`). It is
**non-empty only on the modtime-fallback path** — i.e. when a common modtime cannot be
established and the function falls back to grouping by ETag — and it is set to the ETag that
holds a *quorum* via `commonETag(etags, quorum)`, **not** to an ETag shared unanimously by every
survivor. Therefore the override at L429-433 flips `cannotHeal` back to `false` only when a
quorum of survivors agree on the ETag under that fallback; in the normal modtime path
`quorumETag == ""` and the override does not apply. The precise runtime conditions that drive
`listOnlineDisks` into the modtime-fallback branch were not reproduced through a canonical
trigger in this investigation, so the override's activation is labelled **(inferred, from
`cmd/erasure-healing-common.go:219` and the L429-433 comment)**. All other decision branches in
this document are **observed**.

### 3.4 The dangling classifier — `isObjectDangling` (`cmd/erasure-healing.go:968`), quoted in full

Both `deleteIfDangling` call sites ultimately consult `isObjectDangling`. The complete function
(no elision) is:

```go
func isObjectDangling(metaArr []FileInfo, errs []error, dataErrsByPart map[int][]int) (validMeta FileInfo, ok bool) {
	// We can consider an object data not reliable
	// when xl.meta is not found in read quorum disks.
	// or when xl.meta is not readable in read quorum disks.
	notFoundMetaErrs, nonActionableMetaErrs := danglingMetaErrsCount(errs)

	notFoundPartsErrs, nonActionablePartsErrs := 0, 0
	for _, dataErrs := range dataErrsByPart {
		if nf, na := danglingPartErrsCount(dataErrs); nf > notFoundPartsErrs {
			notFoundPartsErrs, nonActionablePartsErrs = nf, na
		}
	}

	for _, m := range metaArr {
		if m.IsValid() {
			validMeta = m
			break
		}
	}

	if !validMeta.IsValid() {
		// validMeta is invalid because all xl.meta is missing apparently
		// we should figure out if dataDirs are also missing > dataBlocks.
		dataBlocks := (len(metaArr) + 1) / 2
		if notFoundPartsErrs > dataBlocks {
			// Not using parity to ensure that we do not delete
			// any valid content, if any is recoverable. But if
			// notFoundDataDirs are already greater than the data
			// blocks all bets are off and it is safe to purge.
			//
			// This is purely a defensive code, ideally parityBlocks
			// is sufficient, however we can't know that since we
			// do have the FileInfo{}.
			return validMeta, true
		}

		// We have no idea what this file is, leave it as is.
		return validMeta, false
	}

	if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 {
		return validMeta, false
	}

	if validMeta.Deleted {
		// notFoundPartsErrs is ignored since
		// - delete marker does not have any parts
		dataBlocks := (len(errs) + 1) / 2
		return validMeta, notFoundMetaErrs > dataBlocks
	}

	// TODO: It is possible to replay the object via just single
	// xl.meta file, considering quorum number of data-dirs are still
	// present on other drives.
	//
	// However this requires a bit of a rewrite, leave this up for
	// future work.
	if notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks {
		// All xl.meta is beyond parity blocks missing, this is dangling
		return validMeta, true
	}

	if !validMeta.IsRemote() && notFoundPartsErrs > 0 && notFoundPartsErrs > validMeta.Erasure.ParityBlocks {
		// All data-dir is beyond parity blocks missing, this is dangling
		return validMeta, true
	}

	return validMeta, false
}
```

There are therefore **four** terminal decisions, at distinct anchors:

1. **Invalid-meta branch (`L988-1006`)** — no valid `xl.meta` at all: purge only if
   `notFoundPartsErrs > (len(metaArr)+1)/2` (data-majority), else leave as-is.
2. **Non-actionable guard (`L1008-1010`)** — if any meta/part error is non-actionable
   (e.g. **corruption**), return **not dangling**. *This is why corrupt objects are never
   purged.*
3. **Delete-marker branch (`L1012-1016`)** — for a delete marker (`validMeta.Deleted`), ignore
   parts entirely and purge iff `notFoundMetaErrs > (len(errs)+1)/2` (data-majority).
4. **Normal-object branches (`L1025`, `L1030`)** — purge iff `notFoundMetaErrs > ParityBlocks`
   (missing `xl.meta` beyond parity) **or** `notFoundPartsErrs > ParityBlocks` (missing data-dir
   beyond parity).

The two error classifiers it depends on (also quoted in full) make the actionable-vs-non-actionable
split explicit:

```go
func danglingMetaErrsCount(cerrs []error) (notFoundCount int, nonActionableCount int) {
	for _, readErr := range cerrs {
		if readErr == nil {
			continue
		}
		switch {
		case errors.Is(readErr, errFileNotFound) || errors.Is(readErr, errFileVersionNotFound):
			notFoundCount++
		default:
			// All other errors are non-actionable
			nonActionableCount++
		}
	}
	return
}

func danglingPartErrsCount(results []int) (notFoundCount int, nonActionableCount int) {
	for _, partResult := range results {
		switch partResult {
		case checkPartSuccess:
			continue
		case checkPartFileNotFound:
			notFoundCount++
		default:
			// All other errors are non-actionable
			nonActionableCount++
		}
	}
	return
}
```

For a 4-drive set, `ParityBlocks == 2` and `(len+1)/2 == 2`, so **all** dangling thresholds
resolve to "**more than 2**", i.e. **3 or more** of the 4 drives must be *not-found* for a purge.

### 3.5 The per-disk trigger — `shouldHealObjectOnDisk` (`cmd/erasure-healing.go:156`)

Which drives are counted into `disksToHealCount` is decided per-disk (full function):

```go
func shouldHealObjectOnDisk(erErr error, partsErrs []int, meta FileInfo, latestMeta FileInfo) (bool, error) {
	if errors.Is(erErr, errFileNotFound) || errors.Is(erErr, errFileVersionNotFound) || errors.Is(erErr, errFileCorrupt) {
		return true, erErr
	}
	if erErr == nil {
		if meta.XLV1 {
			// Legacy means heal always
			// always check first.
			return true, errLegacyXLMeta
		}
		if !latestMeta.Equals(meta) {
			return true, errOutdatedXLMeta
		}
		if !meta.Deleted && !meta.IsRemote() {
			// If xl.meta was read fine but there may be problem with the part.N files.
			for _, partErr := range partsErrs {
				if slices.Contains([]int{
					checkPartFileNotFound,
					checkPartFileCorrupt,
				}, partErr) {
					return true, errPartMissingOrCorrupt
				}
			}
		}
		return false, nil
	}
	return false, erErr
}
```

Note the two ways a drive is flagged: a missing/corrupt `xl.meta` (`errFileNotFound`/
`errFileCorrupt`) or, when the meta reads fine, a missing/corrupt **part** file
(`errPartMissingOrCorrupt`). This is why in §4 a corrupt *part* renders as drive-state
`missing` (it fails `CheckParts`) whereas a corrupt *`xl.meta`* renders as drive-state
`corrupt`.

### 3.6 The decision, as a flowchart

```mermaid
flowchart TD
    A["HealObject entry<br/>(mc admin heal / madmin API / MRF / scanner)"] --> B["healObject()<br/>cmd/erasure-healing.go:258"]
    B --> C["objectQuorumFromMeta()<br/>cmd/erasure-metadata.go:531<br/>readQuorum = DataBlocks (2 for EC:2)"]
    C -->|"quorum FAILS<br/>(xl.meta not-found > half)"| E1["EARLY deleteIfDangling<br/>cmd/erasure-healing.go:309<br/>cmd/erasure-object.go:482"]
    E1 -->|"isObjectDangling == true"| PURGE["purge all drives, return errFileNotFound<br/>(stays DELETED)"]
    E1 -->|"isObjectDangling == false"| DEG["return errErasureReadQuorum<br/>(DEGRADED)"]
    C -->|"quorum OK"| D["listOnlineDisks + pickValidFileInfo<br/>+ per-disk shouldHealObjectOnDisk<br/>cmd/erasure-healing.go:352,156"]
    D --> F{"cannotHeal?<br/>!Deleted AND disksToHealCount > ParityBlocks(2)<br/>cmd/erasure-healing.go:428"}
    F -->|"quorumETag != '' (modtime fallback)"| G
    F -->|"No (<= 2 to heal)"| G["Reconstruct via Erasure.Heal<br/>cmd/erasure-decode.go:317<br/>RenameData write-back, After.online increases"]
    F -->|"Yes (> 2 to heal)"| H["LATE deleteIfDangling<br/>cmd/erasure-healing.go:438"]
    H -->|"dangling (missing beyond parity)"| PURGE
    H -->|"not dangling (corruption = non-actionable)<br/>errs overwritten, defaultHealResult"| DEG
```

---

## 4. Per-scenario captured evidence

### 4.0 How to read this section, and the client used

`mc` could not be built offline in this environment (its UI dependencies are not in the module
cache). Every heal below is therefore triggered through a **purpose-built client that embeds
the exact library `mc` wraps — `github.com/minio/madmin-go/v3` v3.0.77** (pinned at
`go.mod:52`). It calls `adm.Heal(...)`, which issues the identical canonical request
`POST /minio/admin/v3/heal/{bucket}/{prefix}` (§6.5). This is **not** a bypass: it is the same
admin API surface the `mc admin heal` CLI uses. For each result the client prints:

- a `RAW …` line — the complete, unedited `HealResultItem` JSON exactly as the server returned it
  (fenced as `jsonl` because the stream is one JSON object per line); and
- an `MC  …` line — the client-side summary (online/missing/corrupt/offline counts and the
  green/yellow/red/grey colour) computed with `mc`'s own algorithm reproduced verbatim (§6.2),
  so the reader sees both the raw server states and what `mc` would display.

Provenance and versions of every tool are in §7.

**Two-run methodology (auditability).** Every scenario below was executed **at least twice**,
with a full `reset_object` (§7.2) between runs so each run starts from the pristine baseline.
Where the two runs produced byte-identical heal output (the common case), a single representative
capture is shown and explicitly annotated "both runs identical"; where a run varies a drive
target (e.g. D1 corrupt on d1 vs d3), both runs are shown in full. §4.1 (D1) is the worked
exemplar showing two complete raw runs with UTC timestamps and per-run drive targets; the
remaining scenarios follow the identical protocol and their raw per-run captures live in the
evidence files named in §7. No claim of stability is made from prose alone — each rests on the
captured RAW/MC lines and the on-disk before/after shown with it.

The **baseline** healthy object (`healtest/obj1`, 1 MiB, sha256
`84fe3ab299f674abff207058e4c001cabd570514840445af7fcde2fde1b296d2`, ETag
`ac2a482b34c7a8c6ea03dbbc2a0b2449`) decodes to EC:2 (`EcM:2 EcN:2`), one `part.1` of 524320
bytes on **each** of the four drives (all four share the same datadir UUID per PUT):

```console
$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.2 linux/amd64

$ grep 'Formatting' "$RUN_ROOT/minio.log"
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.

$ /tmp/s3cli-bin "$ENDPOINT" get healtest obj1
get healtest/obj1 size=1048576 sha256=84fe3ab299f674abff207058e4c001cabd570514840445af7fcde2fde1b296d2

$ for d in 1 2 3 4; do dd=$(datadir $d healtest obj1); \
    echo "d$d: xl.meta=$(stat -c%s "$RUN_ROOT/d$d/healtest/obj1/xl.meta")B \
part.1=$(stat -c%s "$dd/part.1")B datadir=$(basename "$dd")"; done
d1: xl.meta=368B part.1=524320B datadir=3188ba51-6ece-4c28-91c0-a82630bffaad
d2: xl.meta=368B part.1=524320B datadir=3188ba51-6ece-4c28-91c0-a82630bffaad
d3: xl.meta=368B part.1=524320B datadir=3188ba51-6ece-4c28-91c0-a82630bffaad
d4: xl.meta=368B part.1=524320B datadir=3188ba51-6ece-4c28-91c0-a82630bffaad
```

The complete, unedited `xl-meta` decode of the object metadata (`docs/debugging/xl-meta`,
default output — nothing elided):

```console
$ /tmp/xl-meta "$RUN_ROOT/d1/healtest/obj1/xl.meta"
{
  "Versions": [
    {
      "Header": {
        "EcM": 2,
        "EcN": 2,
        "Flags": 2,
        "ModTime": "2026-07-13T18:39:02.008975314Z",
        "Signature": "27a98ea0",
        "Type": 1,
        "VersionID": "00000000000000000000000000000000"
      },
      "Idx": 0,
      "Metadata": {
        "Type": 1,
        "V2Obj": {
          "CSumAlgo": 1,
          "DDir": "MYi6UW7OTCiRwKgmML/6rQ==",
          "EcAlgo": 1,
          "EcBSize": 1048576,
          "EcDist": [
            3,
            4,
            1,
            2
          ],
          "EcIndex": 3,
          "EcM": 2,
          "EcN": 2,
          "ID": "AAAAAAAAAAAAAAAAAAAAAA==",
          "MTime": 1783967942008975314,
          "MetaSys": {},
          "MetaUsr": {
            "content-type": "application/octet-stream",
            "etag": "ac2a482b34c7a8c6ea03dbbc2a0b2449"
          },
          "PartASizes": [
            1048576
          ],
          "PartETags": null,
          "PartNums": [
            1
          ],
          "PartSizes": [
            1048576
          ],
          "Size": 1048576
        },
        "v": 1732554622
      }
    }
  ]
}
```

`EcM:2`/`EcN:2` confirm 2 data + 2 parity (EC:2); `EcDist:[3,4,1,2]` is the per-drive shard
distribution; `PartSizes:[1048576]` is the single logical part. Baseline heal of the healthy
object (2 result items — a bucket item and the object item):

```jsonl
RAW {"resultId":1,"type":"bucket","bucket":"healtest","object":"","versionId":"","detail":"","diskCount":4,"setCount":-1,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":0}
RAW {"resultId":2,"type":"object","bucket":"healtest","object":"obj1","versionId":"null","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":1048576}
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=4 missing=0 corrupt=0 offline=0 color=green] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

Between **every** independent scenario the object is fully reset with the helper
`reset_object` (S3 `rm`, remove the object dir on all four drives under the asserted disposable
root, S3 `put`, then S3 `get` to confirm the sha256) so no scenario inherits another's state.
The concrete reset transcript and the safe path helpers are in §7.2.

### 4.1 D1 — "some corrupted data" (one corrupt part, recoverable) → **RECONSTRUCT**

Injection: overwrite `part.1` with a short garbage string on one drive (a bit-rot corruption —
the file exists but fails its HighwayHash checksum, so `CheckParts` reports it and the drive
renders as `missing`). Run twice, targeting a different drive each run. The DataDir is resolved
from trusted `ls` output and validated under the disposable root before it is touched (§7.2).

```console
# RUN 1 target=d1  2026-07-13T17:50:53.574Z
$ DD=$(datadir 1 healtest obj1); printf 'GARBAGE-CORRUPT-DATA-NOT-VALID-SHARD' > "$DD/part.1"
# DURING on-disk (part.1 sizes; xl.meta intact on all 4):
  d1: xl.meta=368B part.1=36B      ← corrupted
  d2: xl.meta=368B part.1=524320B
  d3: xl.meta=368B part.1=524320B
  d4: xl.meta=368B part.1=524320B
```

```jsonl
RAW {"resultId":2,"type":"object","bucket":"healtest","object":"obj1","versionId":"null","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"missing"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":1048576}
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

```console
# AFTER on-disk: d1 part.1=524320B (rebuilt); GET sha256 == baseline → SHA-MATCH: yes (byte-identical)

# RUN 2 target=d3  2026-07-13T17:50:53.868Z  (identical outcome, different drive)
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
# AFTER: d3 part.1=524320B rebuilt; SHA-MATCH: yes (byte-identical)
```

**Decision: 1 drive to heal ≤ parity(2) → `cannotHeal` false → reconstruct.** Both runs
identical.

### 4.2 D1b — "some corrupted data" (corrupt `xl.meta`) → **RECONSTRUCT**, renders `corrupt`

Injection: overwrite the whole `xl.meta` on one drive with garbage (runs on d2, then d4). Unlike
a corrupt part, a corrupt `xl.meta` fails the metadata read and renders as drive-state
`corrupt` (per §3.5, `errFileCorrupt` on the meta read).

```jsonl
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=0 corrupt=1 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

Run 2 (d4) identical; `SHA-MATCH: yes` both runs. **Decision: 1 to heal → reconstruct.** This
contrasts the two rendering states: corrupt *part* ⇒ `missing`; corrupt *`xl.meta`* ⇒ `corrupt`.

### 4.3 D2 — "some nothing" (one drive's object dir removed, recoverable) → **RECONSTRUCT**

Injection: `rm -rf` the entire object directory on one drive (runs on d2, then d4) → both
`xl.meta` and `part.1` gone on that drive → renders `missing`.

```jsonl
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

Both runs `SHA-MATCH: yes`. **Decision: 1 to heal → reconstruct.**

### 4.4 D3 — the user's exact mixed state: "some data / some corrupted / some nothing" → **RECONSTRUCT (at the boundary)**

Injection (the user's literal example, all three states at once): d1 corrupt `part.1`, d2 object
dir removed, d3+d4 intact ⇒ exactly **2** drives to heal (= `ParityBlocks`).

```jsonl
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=2 missing=2 corrupt=0 offline=0 color=red] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

Both runs `SHA-MATCH: yes`. **Decision: `disksToHealCount(2) > ParityBlocks(2)` is false →
`cannotHeal` false → reconstruct.** Note the colour is **red** (0 surplus shards, §6.2) yet the
object is fully recovered — red means "at the edge", not "lost".

### 4.5 Boundary walk (a): 1 → 2 → 3 corrupt shards

Same corruption, increasing count, to locate the exact reconstruct/fail boundary for a normal
object. Each row is the object-item `MC` line; all recover except the last.

```console
1 corrupt (D1):     before[online=3 missing=1 color=yellow] → after[online=4 color=green]   SHA-MATCH yes
2 corrupt (walk):   before[online=2 missing=2 color=red]    → after[online=4 color=green]   SHA-MATCH yes
3 corrupt (D4):     before[online=0 corrupt=4 color=grey]   → after[online=0 corrupt=4 grey] UNRECOVERABLE
```

The boundary is exactly **`ParityBlocks`**: losing ≤ 2 of 4 recovers; losing 3 does not. This is
the answer to boundary **(a)**: reconstruction needs at least `DataBlocks` (**2**) *valid,
compatible* erasure shards to survive (see §5(a) for terminology). Both the 1- and 2-shard rows
were confirmed across two runs (§4.1, §4.4).

### 4.6 D-empty / D-truncated — zero-byte and truncated `part.1` → **RECONSTRUCT**

Injection: (empty) truncate `part.1` to 0 bytes; (truncated) truncate to 1000 bytes. Triggered
with a **deep** (bit-rot) scan (`HealDeepScan`) so the checksum mismatch is detected. Both fail
`CheckParts` and render `missing`, then rebuild.

```jsonl
# zero-byte part.1, deep scan:
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
# truncated (1000B) part.1, deep scan:
MC  type=object name="obj1" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

Each case DURING shows `part.1=0B` / `part.1=1000B`; AFTER shows `part.1=524320B` and
`SHA-MATCH: yes`, stable across two runs. **Decision: 1 to heal → reconstruct.**

### 4.7 D4 — three corrupt parts (answers boundary (b)) → **DEGRADED (unrecoverable), not purged**

This is the pivotal case that the earlier draft mis-explained. Injection: corrupt `part.1` on
**three** of four drives (d1,d2,d3); d4 intact; **all four `xl.meta` remain valid**.

```console
# DURING on-disk:
  d1: xl.meta=368B part.1=36B
  d2: xl.meta=368B part.1=36B
  d3: xl.meta=368B part.1=36B
  d4: xl.meta=368B part.1=524320B
# a canonical S3 GET now fails (cannot assemble DataBlocks=2 good shards):
$ /tmp/s3cli-bin "$ENDPOINT" get healtest obj1
get-read-error: Resource requested is unreadable, please reduce your request rate
```

Heal (both runs byte-identical):

```jsonl
RAW {"resultId":2,"type":"object","bucket":"healtest","object":"obj1","versionId":"null","detail":"Storage resources are insufficient for the read operation healtest/obj1","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"corrupt"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"corrupt"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"corrupt"}]},"objectSize":0}
MC  type=object name="obj1" detail="Storage resources are insufficient for the read operation healtest/obj1" parity=2 data=2 before[online=0 missing=0 corrupt=4 offline=0 color=grey] after[online=0 missing=0 corrupt=4 offline=0 color=grey]
```

```console
# AFTER on-disk: UNCHANGED (3×36B corrupt + 1×524320B). Object is NOT purged and NOT rebuilt = DEGRADED.
```

**Why this outcome, traced exactly (corrects the "objectQuorumFromMeta fails" claim):**

1. All four `xl.meta` are valid ⇒ `objectQuorumFromMeta` (`cmd/erasure-metadata.go:531`)
   **SUCCEEDS** (read quorum 2 is met). The proof is in the RAW line: it carries
   `"parityBlocks":2,"dataBlocks":2`, values that only exist *after* quorum derivation
   succeeded. The EARLY dangling site is therefore **not** taken.
2. Per-drive validation flags the three corrupt parts, so `disksToHealCount = 3 >
   ParityBlocks(2)` ⇒ `cannotHeal = true`. `quorumETag == ""` (normal modtime path), so the
   override does not fire.
3. The LATE `deleteIfDangling` runs (`cmd/erasure-healing.go:438`). Inside `isObjectDangling`,
   the three corrupt parts are **non-actionable** (`danglingPartErrsCount` sends
   `checkPartFileCorrupt` to `default: nonActionableCount++`), so `nonActionablePartsErrs = 3`
   and the non-actionable guard (`L1008-1010`) returns **not dangling**. `deleteIfDangling`
   then returns `errErasureReadQuorum` (`cmd/erasure-object.go:484-487`).
4. Back in `healObject`, `for i := range errs { errs[i] = err }` overwrites **all four** drive
   errors with that single read-quorum error, and `defaultHealResult` renders **every** drive
   as `corrupt` with `objectSize:0`. *This is why the grid shows `corrupt=4` after only three
   parts were faulted.*
5. The exported wrapper maps the sentinel through `toObjectErr` → `InsufficientReadQuorum`,
   whose `Error()` (`cmd/object-api-errors.go:236`) is exactly the `detail` string shown:
   `Storage resources are insufficient for the read operation healtest/obj1`.
6. `mc` colour: `online=0` ⇒ `surplus = 0 − 2 = −2 < 0` ⇒ **grey** (§6.2).

So D4 is the concrete **DEGRADED** outcome: too many shards corrupt to reconstruct, but
corruption is non-actionable so it is **not** purged. It sits on disk, unreadable, until enough
good shards return. Both runs identical.

### 4.8 D5 — dangling PURGE (metadata missing beyond quorum) → object left **DELETED**

Injection: `rm -rf` the whole object directory on **three** of four drives (d1,d2,d3); only d4
retains `xl.meta`+`part.1`.

```console
# DURING on-disk:
  d1: dir=ABSENT   d2: dir=ABSENT   d3: dir=ABSENT   d4: dir=present xl.meta=368B part.1=524320B
```

Heal (both runs identical) — note this produces a **bucket** item and then a **zero**
`HealResultItem` for the object:

```jsonl
RAW {"resultId":2,"type":"object","bucket":"","object":"","versionId":"","detail":"Object not found: healtest/obj1","diskCount":0,"setCount":0,"before":{"drives":null},"after":{"drives":null},"objectSize":0}
MC  type=object name="" detail="Object not found: healtest/obj1" parity=0 data=0 before[online=0 missing=0 corrupt=0 offline=0 color=ERR(Invalid parity shard count/surplus shard count given)] after[online=0 missing=0 corrupt=0 offline=0 color=ERR(Invalid parity shard count/surplus shard count given)]
```

```console
$ /tmp/s3cli-bin "$ENDPOINT" stat healtest obj1
stat-error: The specified key does not exist.
# AFTER on-disk: ALL FOUR drives purged — even the surviving d4:
  d1: dir=ABSENT   d2: dir=ABSENT   d3: dir=ABSENT   d4: dir=ABSENT
```

**Why the object result is a *zero* item (answers finding on D5's shape).** With only one valid
`xl.meta` (< read quorum 2), `objectQuorumFromMeta` returns an error, so the **EARLY**
`deleteIfDangling` at `cmd/erasure-healing.go:309` runs. `isObjectDangling`'s normal-object meta
branch sees `notFoundMetaErrs = 3 > ParityBlocks(2)` ⇒ **dangling** ⇒ the object is purged from
all drives. `deleteIfDangling` returns `nil`, so `healObject` returns `errFileNotFound`. That
propagates up to `erasureServerPools.HealObject` (`cmd/erasure-server-pool.go:2561`), where —
after every pool reports not-found — the function returns a bare `madmin.HealResultItem{}` plus
`ObjectNotFound` (`cmd/erasure-server-pool.go:2604-2607`):

```go
	return madmin.HealResultItem{}, ObjectNotFound{
		Bucket: bucket,
		Object: object,
	}
```

That empty item is exactly what the client renders: empty `object`, `before/after.drives:null`,
`parityBlocks:0`. Because `parityBlocks < 1`, `mc`'s colour routine cannot compute a surplus and
raises `Invalid parity shard count/surplus shard count given` (§6.2) — so a **dangling purge has
no normal Before/After object grid**; the "grid" is a not-found sentinel. This is the
**stay-DELETED** outcome. Both runs identical.

### 4.9 WRITE vs DELETE cross-product (answers boundary (c))

This is the direct comparison the question asks for. All cells are triggered through the
canonical admin heal API; each is run twice with resets between.

#### 4.9.1 Partial WRITE (normal object) — threshold is `> ParityBlocks` (≥ 3 of 4)

| Cell | Injection (with all 4 `xl.meta` present unless noted) | Outcome | Governing branch |
|------|-------------------------------------------------------|---------|------------------|
| WRITE-recoverable | remove data-dir on **2** of 4 | **RECONSTRUCT** | `disksToHealCount(2) ≤ parity` |
| WRITE-dangling (meta) | remove object dir on **3** of 4 (= D5) | **PURGE** | EARLY site, meta branch `L1025` |
| WRITE-dangling (data-only) | remove **data-dir on 3** of 4, **all 4 `xl.meta` intact** | **PURGE** | LATE site, parts branch `L1030` |

The data-only cell is the interesting one — it proves the parts branch purges *independently of
metadata quorum*:

```console
# WRITE-dangling(data-only) DURING — all 4 xl.meta present, data-dir gone on d1,d2,d3:
  d1: xl.meta=368B part.1=absent
  d2: xl.meta=368B part.1=absent
  d3: xl.meta=368B part.1=absent
  d4: xl.meta=368B part.1=524320B
```

```jsonl
RAW {"resultId":2,"type":"object","bucket":"","object":"","versionId":"","detail":"Object not found: healtest/obj1","diskCount":0,"setCount":0,"before":{"drives":null},"after":{"drives":null},"objectSize":0}
```

```console
$ /tmp/s3cli-bin "$ENDPOINT" stat healtest obj1
stat-error: The specified key does not exist.
# AFTER: all 4 drives wiped.
```

Here quorum on metadata *succeeds* (4 valid `xl.meta`), so the LATE site runs; `notFoundPartsErrs
= 3 > ParityBlocks(2)` trips the parts branch (`cmd/erasure-healing.go:1030`) ⇒ purge. Both runs
identical. (WRITE-recoverable ⇒ `before[online=2 missing=2 red] → after green`, `SHA-MATCH yes`,
both runs.)

#### 4.9.2 Partial DELETE (delete marker on a versioned bucket) — threshold is data-majority, parts ignored

Setup: versioned bucket `verbkt`, object `dobj`. `PUT` writes version **V1** (`xl.meta` 368B
`[V1]`); a snapshot of that `[V1]`-only meta is taken; `DELETE` then writes a **delete marker
DM** as the latest version (`xl.meta` grows to 477B `[DM,V1]` on all four drives, V1's `part.1`
retained). A *partial delete* is simulated by restoring the snapshot `[V1]`-only meta on a
subset of drives — i.e. those drives "missed" the delete.

A crucial mechanism: the delete-marker dangling branch is reached **only** through a
**recursive** heal. A non-recursive heal targets version `""` (the null version), which does
not exist in a versioned bucket; the recursive heal enumerates real versions via
`fivs.Versions` (`cmd/erasure-server-pool.go:2513-2514`) and heals the DM **by its version-id**,
which is what drives it into the EARLY dangling site (recall the LATE site is blocked for
delete markers by `!latestMeta.Deleted`, §3.2).

**DEL-A — DM present on the majority (3 of 4; d4 reverted to `[V1]`):**

```console
# DURING: d1,d2,d3 xl.meta=477B [DM,V1]; d4 xl.meta=368B [V1]
```

```jsonl
RAW {"resultId":2,"type":"object","bucket":"verbkt","object":"dobj","versionId":"978d042f-cae8-44c5-9c7f-99b320e41ed2","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"missing"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":0}
MC  type=object name="dobj" detail="" parity=2 data=2 before[online=3 missing=1 corrupt=0 offline=0 color=yellow] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

```console
# versions after heal: DM (isDeleteMarker=true, isLatest=true) + V1
$ /tmp/s3cli-bin "$ENDPOINT" stat verbkt dobj
stat-error: The specified key does not exist.
# AFTER on-disk: d4 xl.meta 368B → 477B  (the delete marker was PROPAGATED to d4)
```

**DM has quorum ⇒ the delete marker is HEALED (propagated) to the missing drive; the object
stays DELETED.** `objectSize:0` because a delete marker has no data. Both runs identical.

**DEL-B — DM present on the minority (1 of 4; d2,d3,d4 reverted to `[V1]`):**

```console
# DURING: d1 xl.meta=477B [DM,V1]; d2,d3,d4 xl.meta=368B [V1]
```

```jsonl
RAW {"resultId":2,"type":"object","bucket":"","object":"","versionId":"","detail":"Version not found: verbkt/dobj(535a4b9f-23cb-4889-9de7-c4f524079c1f)","diskCount":0,"setCount":0,"before":{"drives":null},"after":{"drives":null},"objectSize":0}
RAW {"resultId":3,"type":"object","bucket":"verbkt","object":"dobj","versionId":"10208161-70c9-429a-b123-131731096db4","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":1048576}
MC  type=object name="dobj" detail="" parity=2 data=2 before[online=4 missing=0 corrupt=0 offline=0 color=green] after[online=4 missing=0 corrupt=0 offline=0 color=green]
```

```console
# versions after heal: ONLY V1 (isDeleteMarker=false, isLatest=true)
$ /tmp/s3cli-bin "$ENDPOINT" stat verbkt dobj
stat verbkt/dobj etag=ac2a482b34c7a8c6ea03dbbc2a0b2449 size=1048576 versionId=10208161-70c9-429a-b123-131731096db4
# AFTER on-disk: d1 xl.meta 477B → 368B  (the delete marker was PHYSICALLY REMOVED)
```

**DM lacks quorum ⇒ the delete marker is judged dangling and PURGED** (`resultId:2` is the DM
version, reported `Version not found` with `drives:null`), while **V1 heals green** on all four
drives (`resultId:3`). Net effect: **the object is RESTORED** to V1. Both runs identical.

**Boundary (c), answered directly:** a partial **WRITE** and a partial **DELETE** heal by
*different rules*. A normal object is purged only when its `xl.meta` **or** its data-dir is
not-found on **> ParityBlocks** (≥ 3 of 4) drives (parts matter). A delete marker ignores parts
and is judged on **metadata majority** of the version: if the DM holds quorum it is healed
outward and the object stays DELETED; if the DM is in the minority it is purged and the
underlying version is restored. The two branches are literally different code
(`cmd/erasure-healing.go:1016` for delete markers vs `:1025`/`:1030` for normal objects).

### 4.10 Server-side write-back proof (reconstruction is real, and reads exactly `DataBlocks` survivors)

To show that a reconstruct actually rebuilds and commits shards (not just flips a status), a
`madmin` `ServiceTrace` subscriber (the same stream `mc admin trace` consumes) was attached
during a D1-style single-corrupt reconstruct on d3:

The complete, unedited trace (`TRACE-START` … `TRACE-END`, corrupt shard on d3, datadir
`1a2ea578-…`) — no lines elided:

```console
TRACE-START match="obj1" seconds=8 storage=true healing=true os=true
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d4/healtest/obj1/xl.meta" bytes=0 dur=23.26µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d2/healtest/obj1/xl.meta" bytes=0 dur=34.553µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d3/healtest/obj1/xl.meta" bytes=0 dur=26.411µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=73.638µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d1/healtest/obj1/xl.meta" bytes=0 dur=31.259µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=71.759µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=88.939µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=76.936µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d4/healtest/obj1/xl.meta" bytes=0 dur=17.313µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d1/healtest/obj1/xl.meta" bytes=0 dur=18.713µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d2/healtest/obj1/xl.meta" bytes=0 dur=18.898µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=46.493µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=45.712µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=40.079µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d3/healtest/obj1/xl.meta" bytes=0 dur=26.108µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=73.418µs err=""
TRACE type=os func=os.Lstat path="/tmp/blitzy-heal-run.main/d1/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=5.505µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=23.917µs err=""
TRACE type=os func=os.Lstat path="/tmp/blitzy-heal-run.main/d2/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=3.829µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=10.348µs err=""
TRACE type=os func=os.Lstat path="/tmp/blitzy-heal-run.main/d3/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=3.423µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=10.489µs err=""
TRACE type=os func=os.Lstat path="/tmp/blitzy-heal-run.main/d4/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=3.755µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=9.131µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d1/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=19.94µs err=""
TRACE type=storage func=storage.ReadFileStream path="/tmp/blitzy-heal-run.main/d1 healtest obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=524320 dur=39.042µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d4/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=0 dur=21.376µs err=""
TRACE type=storage func=storage.ReadFileStream path="/tmp/blitzy-heal-run.main/d4 healtest obj1/1a2ea578-352b-4348-aa17-61ece28e60b2/part.1" bytes=524320 dur=35.214µs err=""
TRACE type=os func=os.OpenFileR path="/tmp/blitzy-heal-run.main/d3/healtest/obj1/xl.meta" bytes=0 dur=24.134µs err=""
TRACE type=os func=os.Rename path="/tmp/blitzy-heal-run.main/d3/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2 -> /tmp/blitzy-heal-run.main/d3/.minio.sys/tmp/.trash/8947228a-0d51-4cef-910d-47cafb4660d2" bytes=0 dur=46.538µs err=""
TRACE type=os func=os.Rename path="/tmp/blitzy-heal-run.main/d3/.minio.sys/tmp/0c55ecaf-741c-42ce-8917-ac5b736ce120/1a2ea578-352b-4348-aa17-61ece28e60b2/ -> /tmp/blitzy-heal-run.main/d3/healtest/obj1/1a2ea578-352b-4348-aa17-61ece28e60b2" bytes=0 dur=23.21µs err=""
TRACE type=os func=os.Rename path="/tmp/blitzy-heal-run.main/d3/.minio.sys/tmp/0c55ecaf-741c-42ce-8917-ac5b736ce120/xl.meta -> /tmp/blitzy-heal-run.main/d3/healtest/obj1/xl.meta" bytes=0 dur=45.24µs err=""
TRACE type=storage func=storage.RenameData path="/tmp/blitzy-heal-run.main/d3 0c55ecaf-741c-42ce-8917-ac5b736ce120 1a2ea578-352b-4348-aa17-61ece28e60b2 healtest obj1" bytes=0 dur=1.576194ms err=""
TRACE type=healing func=heal.Object path="healtest/obj1" bytes=1048576 dur=12.870283ms err=""
TRACE-END total_matched=34
```

Four facts fall directly out of this trace: (1) all four `xl.meta` are read and every drive's
`CheckParts` runs, so the corrupt d3 part is detected; (2) reconstruction reads **exactly two**
(`DataBlocks`) surviving `part.1` files — d1 and d4, each `bytes=524320` — confirming §5.1; (3)
the rebuilt shard is committed atomically to the healed drive via `storage.RenameData`
(`cmd/erasure-decode.go:317` `Erasure.Heal` → `RenameData`, `dur=1.576194ms`); (4) the old
corrupt datadir is moved to `.minio.sys/tmp/.trash/…` *before* the reconstructed datadir and
`xl.meta` are renamed into place, so the write-back is crash-safe. The whole `heal.Object`
completed in `12.87ms`.

### 4.11 Trigger coverage — manual, MRF (GET), and background scanner

All three canonical triggers reach the same `healObject` worker.

**Manual (`mc admin heal` / madmin API).** The client `POST`s
`/minio/admin/v3/heal/{bucket}` (madmin `Heal`, `heal-commands.go:253`). The complete, unedited
trace during a manual heal of a single-corrupt object (corrupt shard on d2, datadir `ad0d52ab-…`)
shows the same worker and write-back — `CheckParts` on all four drives, exactly two
`ReadFileStream`, one `RenameData`, one `heal.Object`:

```console
TRACE-START match="obj1" seconds=6 storage=true healing=true os=false
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=81.097µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=81.88µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=82.937µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=77.685µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=40.701µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=42.438µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=46.965µs err=""
TRACE type=storage func=storage.ReadVersion path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=78.118µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d1 healtest obj1" bytes=0 dur=27.024µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d2 healtest obj1" bytes=0 dur=11.164µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d3 healtest obj1" bytes=0 dur=9.73µs err=""
TRACE type=storage func=storage.CheckParts path="/tmp/blitzy-heal-run.main/d4 healtest obj1" bytes=0 dur=9.461µs err=""
TRACE type=storage func=storage.ReadFileStream path="/tmp/blitzy-heal-run.main/d4 healtest obj1/ad0d52ab-4951-40a2-946f-34bcc753196e/part.1" bytes=524320 dur=36.996µs err=""
TRACE type=storage func=storage.ReadFileStream path="/tmp/blitzy-heal-run.main/d3 healtest obj1/ad0d52ab-4951-40a2-946f-34bcc753196e/part.1" bytes=524320 dur=82.477µs err=""
TRACE type=storage func=storage.RenameData path="/tmp/blitzy-heal-run.main/d2 58427b50-f727-43e3-b2ed-ea956b7cb37c ad0d52ab-4951-40a2-946f-34bcc753196e healtest obj1" bytes=0 dur=1.931691ms err=""
TRACE type=healing func=heal.Object path="healtest/obj1" bytes=1048576 dur=7.030011ms err=""
TRACE-END total_matched=16
```

This was captured by the harness `tracecli` (a `madmin` `ServiceTrace` subscriber) while the
harness `healcli` issued the `POST /minio/admin/v3/heal/{bucket}` — i.e. the same canonical route
and worker `mc admin heal` uses.

**Inline MRF on GET (no manual heal).** A GET that hits a missing/corrupt shard reconstructs
inline to serve the read *and* enqueues the object for background repair
(`cmd/erasure-object.go:397-414`):

```go
			if written == partLength {
				if errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt) {
					healOnce.Do(func() {
						globalMRFState.addPartialOp(PartialOperation{
							Bucket:     bucket,
							Object:     object,
							VersionID:  fi.VersionID,
							Queued:     time.Now(),
							SetIndex:   er.setIndex,
							PoolIndex:  er.poolIndex,
							BitrotScan: errors.Is(err, errFileCorrupt),
						})
					})
					// Healing is triggered and we have written
					// successfully the content to client for
					// the specific part, we should `nil` this error
					// and proceed forward, instead of throwing errors.
					err = nil
				}
```

The `healRoutine` (`cmd/mrf.go:220`) drains the queue and calls `healObject`
(`cmd/mrf.go:272`/`276`). The same `globalMRFState.addPartialOp` producer is wired into the
**write** paths as well, not only the GET path shown above (source-cited anchors in this
checkout): the read/GET path at `cmd/erasure-object.go:400`, the PUT/put-object partial-write
path at `cmd/erasure-object.go:805`, the copy/rename paths at `cmd/erasure-object.go:1578` and
`:2113`, the multipart complete path at `cmd/erasure-multipart.go:1409`, and the peer S3 client
at `cmd/peer-s3-client.go:261`. So a partially-failed **WRITE** enqueues the same MRF repair the
GET path does; the outcome is the identical `healObject` decision (§3). The GET case is the one
reproduced end-to-end on-disk below; the write-path producers are cited from source.

Observed, twice, with **no** manual heal issued — remove d3's data-dir, then GET:

```console
# DURING: d3 part.1=absent
$ /tmp/s3cli-bin "$ENDPOINT" get healtest obj1     # succeeds via inline reconstruct
   GET sha-match: yes
# poll d3:  t≈1.5s: d3 part.1 RESTORED (524320B)   ← background MRF healRoutine
TRACE type=healing func=heal.Object path="healtest/obj1" bytes=1048576 dur=7.645891ms err=""
# AFTER: all 4 drives part.1=524320B
```

Both runs restored d3 within ~1.5 s. This is the observed **successful on-disk MRF repair**.

**Background scanner.** Wiring (finding-corrected): the periodic object scanner starts via
`initDataScanner` (`cmd/server-main.go:1028-1030`, gated by `_MINIO_SCANNER`, default `on`) →
`runDataScanner` (`cmd/data-scanner.go:159`) → `scannerItem.applyHealing`
(`cmd/data-scanner.go:954`). Its per-object heal-scan selection is **1-in-`healObjectSelectProb`
= 1-in-1024** (`cmd/data-scanner.go:61`) on a **1-minute** cycle
(`dataScannerStartDelay`, `cmd/data-scanner.go:58`). This is a *different* subsystem from
`initAutoHeal` (`cmd/background-newdisks-heal-ops.go:377`, invoked at
`cmd/erasure-server-pool.go:195`), which drives *drive*-healing and MRF, not the periodic object
scan.

Genuine scaled attempt (observed, honest negative): 1024 non-inlined 512 KiB objects were
created, one shard removed from **every** object, and `ServiceTrace` was watched for **240 s**
(~4 cycles). The scanner was demonstrably active on the bucket (830 captured storage traces —
`.usage-cache.bin` `ReadXL`/`RenameData`/`Delete` accounting), but **no object-level
`heal.Object` fired and 0 shards were restored** in the window, exactly as expected from the
1-in-1024 hash sampling (the AAP itself flags this as non-deterministic and prefers the manual
path). The scanner's heal *action* is the same `healObject` proven deterministic above via the
manual and MRF triggers; only its per-object **selection** is probabilistic. (Objects smaller
than ~256 KiB inline into `xl.meta` and have no separate `part.1`; 512 KiB was used to guarantee
separate shard files.)


---

## 5. Boundary conditions

### 5.1 (a) How many valid shards must exist for healing to succeed?

For a 4-drive set the layout is **EC:2** (2 data + 2 parity), fixed by
`DefaultParityBlocks` (`internal/config/storageclass/storage-class.go:361-362`, `case 4, 5:
return 2`). The read/write quorum is derived by `objectQuorumFromMeta`
(`cmd/erasure-metadata.go:531`): read quorum = `DataBlocks` (**2**), and write quorum =
`DataBlocks`+1 (**3**) because `dataBlocks == parityBlocks` (`cmd/erasure-metadata.go:558-560`).

Reconstruction needs at least **`DataBlocks` = 2 valid, compatible erasure shards** to survive.
The word **compatible** is deliberate (finding #7): the two survivors are not required to be the
two *data* shards — Reed–Solomon reconstructs from *any* `DataBlocks` intact shards, data **or**
parity, that share the same erasure geometry and datadir. The `docs/erasure/README.md:3` framing
("lose up to N/2") is the same statement for the healthy-read path; healing adds the extra gate
that the number of drives *needing repair* must not exceed `ParityBlocks`.

Observed boundary (§4.5, each recoverable row confirmed twice):

| Shards lost (of 4) | Survivors | `disksToHealCount` vs `ParityBlocks(2)` | Outcome |
|--------------------|-----------|------------------------------------------|---------|
| 1 | 3 | 1 ≤ 2 | **RECONSTRUCT** (§4.1) |
| 2 | 2 (= `DataBlocks`) | 2 ≤ 2 | **RECONSTRUCT** at the edge (§4.4) |
| 3 | 1 (< `DataBlocks`) | 3 > 2 | **FAIL** — cannot reconstruct (§4.7) |

The write-back trace (§4.10) confirms the mechanism directly: a successful reconstruct opens
`ReadFileStream` on **exactly two** surviving `part.1` files and rebuilds the rest.

### 5.2 (b) What error appears when healing cannot recover an object?

When fewer than `DataBlocks` compatible shards survive, reconstruction cannot proceed. Two
distinct surfacings were observed, depending on whether the failure is *too much corruption* or
*too little metadata*:

**Non-actionable corruption (D4, §4.7)** — all four `xl.meta` valid, three parts corrupt. Quorum
*succeeds*; the object is judged **not dangling** (corruption is non-actionable); it is left in
place (**DEGRADED**). The heal item's `detail` is the `InsufficientReadQuorum.Error()` string
(`cmd/object-api-errors.go:236-238`), which unwraps to `errErasureReadQuorum`
(`cmd/erasure-errors.go:23`, `Unwrap` at `cmd/object-api-errors.go:241-243`):

```console
detail = "Storage resources are insufficient for the read operation healtest/obj1"
```

and a client GET returns:

```console
Resource requested is unreadable, please reduce your request rate
```

**Metadata below quorum (D5, §4.8)** — object dir gone on 3 of 4. Quorum *fails*, the object is
judged **dangling** and purged; the heal item carries:

```console
detail = "Object not found: healtest/obj1"
```

Both are the observed forms of "healing cannot recover this object". The `errErasureReadQuorum`
sentinel text is exactly `Read failed. Insufficient number of drives online`
(`cmd/erasure-errors.go:23`); the operator-facing wrapper adds the bucket/object as shown.

### 5.3 (c) Does healing behavior differ between a partially failed WRITE and a partially failed DELETE?

**Yes — they are governed by two different branches of `isObjectDangling`
(`cmd/erasure-healing.go:968`), with different thresholds** (full quote in §3.4; cross-product
evidence in §4.9):

| | Partial WRITE (normal object) | Partial DELETE (delete marker) |
|---|---|---|
| Branch | normal-object: `cmd/erasure-healing.go:1025` (meta) / `:1030` (parts) | delete-marker: `cmd/erasure-healing.go:1012-1016` |
| Purge threshold | `notFoundMetaErrs > ParityBlocks` **or** `notFoundPartsErrs > ParityBlocks` (i.e. ≥ 3 of 4) | `notFoundMetaErrs > (len(errs)+1)/2` (data-majority, i.e. ≥ 3 of 4) |
| Parts considered? | **Yes** — a normal object with data-dir gone on 3 drives purges even when all `xl.meta` survive (§4.9.1 data-only cell) | **No** — delete markers have no parts; part errors are ignored |
| Reached via | EARLY site (meta below quorum) or LATE site (`cmd/erasure-healing.go:438`) | **EARLY site only** — the LATE `cannotHeal` gate excludes deletes via `!latestMeta.Deleted` (§3.2) |
| Observed outcomes | recoverable → reconstruct; ≥3 not-found → purge (stays deleted) | DM has quorum → propagate DM, object stays deleted (DEL-A, §4.9.2); DM in minority → purge DM, object restored (DEL-B) |

The concrete divergence: a partial WRITE that lost 3 **data-dirs** but kept all metadata still
purges (parts branch), whereas a delete marker never looks at parts at all and is decided purely
on whether the marker itself holds a data-majority across drives.

---

## 6. What the healing output reveals about decision-making

### 6.1 BEFORE / AFTER status indicators

Every object heal returns a `madmin.HealResultItem`
(`github.com/minio/madmin-go/v3` `heal-commands.go:139-160`) with **`Before`** and **`After`**
blocks, each a list of per-drive `HealDriveInfo{UUID, Endpoint, State}`
(`heal-commands.go:132-135`). This is the literal BEFORE/AFTER the question asks for: the
`Before.Drives[i].State` shows each drive's pre-heal condition and `After.Drives[i].State` its
post-heal condition. A successful reconstruct shows the healed drive flip from `missing`/`corrupt`
→ `ok` (e.g. §4.1: `before … d1:missing … → after … d1:ok`). The `After` states are set to
`madmin.DriveStateOk` for repaired drives at `cmd/erasure-healing.go:651`.

The item also carries `ParityBlocks`, `DataBlocks`, `DiskCount`, `ObjectSize`, and a free-text
`Detail` — the last is where the "why it could not heal" strings appear (§5.2).

### 6.2 Drive-state vocabulary and the client-side colour

The server emits **states**, not colours. The state vocabulary is the `DriveState*` set of
constants (`heal-commands.go:120-128`): `ok`, `offline`, `corrupt`, `missing`,
`permission-denied`, `faulty`, `unformatted`. Observed mappings from the scenarios:

- corrupt **part** → `missing` (§4.1) — `shouldHealObjectOnDisk` treats a bad part as
  `errPartMissingOrCorrupt` (§3.5).
- corrupt **`xl.meta`** → `corrupt` (§4.2).
- object dir removed → `missing` (§4.3).
- degraded/unreadable (D4) → all drives `corrupt` (§4.7, via the `errs`-overwrite mechanism).

The green/yellow/red/grey **colour** is computed **client-side** by `minio/mc`
(`getHColCode`, `mc cmd/admin-heal-ui.go`) from `surplus = onlineCount − DataBlocks` and
`ParityBlocks`. The exact table `mc` uses (reproduced verbatim in the harness client and
confirmed against every capture) is:

```go
var hColOrder = []string{"red", "yellow", "green"}
var hColTable = map[int][]int{
	1: {0, -1, 1}, 2: {0, 1, 2}, 3: {1, 2, 3}, 4: {1, 2, 4},
	5: {1, 3, 5}, 6: {2, 4, 6}, 7: {2, 4, 7}, 8: {2, 5, 8},
}
func getHColCode(surplusShards, parityShards int) (string, error) {
	if parityShards < 1 || parityShards > 8 || surplusShards > parityShards {
		return "", fmt.Errorf("Invalid parity shard count/surplus shard count given")
	}
	if surplusShards < 0 {
		return "grey", nil
	}
	for index, val := range hColTable[parityShards] {
		if val != -1 && surplusShards <= val {
			return hColOrder[index], nil
		}
	}
	return "", fmt.Errorf("cannot get a heal color code")
}
```

For **EC:2** (`parityShards = 2`, row `{0, 1, 2}`) this yields the exact mapping observed:

| Online drives | surplus = online − 2 | Colour | Seen in |
|---------------|----------------------|--------|---------|
| 4 | 2 | **green** | §4.1 after |
| 3 | 1 | **yellow** | §4.1 before |
| 2 | 0 | **red** | §4.4 before |
| < 2 | < 0 | **grey** | §4.7 (online 0) |
| n/a (`parity=0`) | — | **ERR** `Invalid parity shard count/surplus shard count given` | §4.8 (D5 zero item) |

So a dangling purge (D5) has **no** normal colour: because the zero `HealResultItem` reports
`parityBlocks:0`, `getHColCode` rejects it — the operator sees the not-found detail, not a grid.

### 6.3 Do the logs explain WHY? — three distinct log predicates

The server logs its healing rationale through `healingLogOnceIf`
(`cmd/erasure-healing.go`), with **three distinct tags**, each guarding a *different*
distribution-mismatch check (finding #12):

```console
$ grep -n 'healingLogOnceIf' cmd/erasure-healing.go
477:		healingLogOnceIf(ctx, err, "heal-object-available-disks")
487:		healingLogOnceIf(ctx, err, "heal-object-outdated-disks")
497:		healingLogOnceIf(ctx, err, "heal-object-metadata-entries")
```

- `heal-object-available-disks` (L477) — the count of available disks disagrees with the
  metadata's expectation.
- `heal-object-outdated-disks` (L487) — the set of outdated disks disagrees.
- `heal-object-metadata-entries` (L497) — the number of surviving `xl.meta` entries disagrees.

**Honest observation:** these three predicates fire only when the *derived distributions
disagree* — they are integrity assertions, not a per-shard "this drive was corrupt" narrative.
Across all §4 scenarios (single-shard faults through D4/D5) none of the three tripped, because
the distributions remained self-consistent. The decision rationale that *was* observable at
runtime is (i) the per-drive `Before`/`After` states in the heal item, (ii) the `Detail` string
on failure (§5.2), and (iii) the `ServiceTrace` stream (§4.10/§4.11), which shows the actual
`CheckParts`/`ReadFileStream`/`RenameData`/`heal.Object` calls. The three log tags are therefore
labeled **observed-in-code / not-triggered-at-runtime** for these inputs; they would require a
genuine distribution skew (e.g. a torn write that leaves inconsistent shard indices) to emit.

### 6.4 The admin heal API contract (how `mc admin heal` actually drives this)

The manual trigger is a POST to one of three routes (`cmd/admin-router.go:175-177`), plus a
status route:

```console
175: POST /minio/admin/v3/heal/
176: POST /minio/admin/v3/heal/{bucket}
177: POST /minio/admin/v3/heal/{bucket}/{prefix:.*}
178: POST /minio/admin/v3/background-heal/status
```

All three land on `HealHandler` (`cmd/admin-handlers.go:1308`). The request body is a
`madmin.HealOpts` (`heal-commands.go`, fields `Recursive, DryRun, Remove, Recreate, ScanMode,
UpdateParity, NoLock, Pool, Set`); `ScanMode` is `HealNormalScan` or `HealDeepScan` (deep = bit-rot
verify, used for §4.6). `extractHealInitParams` (`cmd/admin-handlers.go:1238`) reads the
`clientToken` (L1260), `forceStart` (L1263) and `forceStop` (L1266) query parameters.

Healing is **asynchronous and streamed**: `HealHandler` calls `LaunchNewHealSequence`
(`cmd/admin-heal-ops.go:296`), which starts a background `healSequence`; the handler returns a
`clientToken`, and the client polls the same endpoint with that token to drain results. Inside
the sequence, `traverseAndHeal` (`cmd/admin-heal-ops.go:832`) walks objects, `healSequence.healObject`
(`cmd/admin-heal-ops.go:916`) heals each, and `pushHealResultItem` (`cmd/admin-heal-ops.go:618`)
enqueues each `HealResultItem` onto the stream the client reads. `forceStart`/`forceStop`
begin/cancel a sequence. The harness client (§4.0) drives exactly this contract: it calls
`madmin`'s `Heal` (`heal-commands.go:253`, which POSTs `adminAPIPrefix+"/heal/%s"`) with
`HealOpts{Recursive, ScanMode, Remove:true}` and prints the raw `HealResultItem`s.

The token/streaming contract is directly observable — the client prints the `clientToken` the
server assigned, streams each `HealResultItem`, and prints the terminal summary when the sequence
finishes (captured on the healthy baseline object):

```console
# heal started clientToken=5903d427-4492-4012-88e7-35ec261ab87f startTime=2026-07-13T18:39:24.39721781Z
RAW {"resultId":1,"type":"bucket","bucket":"healtest","object":"","versionId":"","detail":"","diskCount":4,"setCount":-1,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":0}
RAW {"resultId":2,"type":"object","bucket":"healtest","object":"obj1","versionId":"null","detail":"","parityBlocks":2,"dataBlocks":2,"diskCount":4,"setCount":0,"before":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"after":{"drives":[{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/blitzy-heal-run.main/d4","state":"ok"}]},"objectSize":1048576}
# summary=finished failureDetail="" items=1
```

The `clientToken` is exactly what the client re-submits to drain subsequent pages, and
`summary=finished` marks sequence completion.

The full dispatch chain (all anchors in this checkout):

```console
HealHandler (admin-handlers.go:1308)
  → LaunchNewHealSequence (admin-heal-ops.go:296)
    → healSequence.healObject (admin-heal-ops.go:916)
      → erasureServerPools.HealObject (erasure-server-pool.go:2561)
        → erasureSets.HealObject (erasure-sets.go:1151)
          → erasureObjects.HealObject (erasure-healing.go:1039, exported wrapper)
            → healObject (erasure-healing.go:258)   ← the decision worker (§3)
```

### 6.5 Server computes, client renders — the split that matters

The evidence cleanly separates the two layers. The **server** decides and returns *states* +
*counts* + *detail* in the `HealResultItem` (the RAW lines in §4). The **client** (`minio/mc`, or
the harness `MC` line) computes the colour and human formatting from those numbers. This is why
the same underlying decision is visible three ways: the RAW JSON (authoritative), the colour grid
(client convenience), and the `ServiceTrace` stream (mechanism). A dangling purge is the clearest
proof of the split — the server returns a `parityBlocks:0` not-found item and it is the *client's*
`getHColCode` that raises `Invalid parity shard count`, not the server (§4.8).

The reconstruction engine underneath is Reed–Solomon via `klauspost/reedsolomon`: `NewErasure`
(`cmd/erasure-coding.go:42`) validates shard counts (returning `reedsolomon.ErrInvShardNum` /
`ErrMaxShardNum`) and lazily builds the encoder with `reedsolomon.New(dataBlocks, parityBlocks,
…)` (`cmd/erasure-coding.go:63`); `Erasure.Heal` (`cmd/erasure-decode.go:317`) is the heal-path
reconstruct that the write-back trace (§4.10) exercised.


---

## 7. Exact build, invocation, and cleanup

### 7.1 Canonical build (from this checkout)

```console
$ export PATH=$PATH:/usr/local/go/bin
$ export CGO_ENABLED=0
$ cd <repo>                       # the read-only MinIO source tree (commit c07e5b49d477)
$ LDFLAGS=$(go run buildscripts/gen-ldflags.go)
$ go build -ldflags "$LDFLAGS" -o /tmp/minio-bin .
```

The resulting binary self-identifies as the canonical development build of this commit:

```console
$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.2 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

The exact `LDFLAGS` string produced by `gen-ldflags.go` on this host was:

```console
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z -X github.com/minio/minio/cmd.CopyrightYear=2024 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 -X github.com/minio/minio/cmd.GOPATH=/root/go -X github.com/minio/minio/cmd.GOROOT=
```

### 7.2 Isolated 4-drive run harness (unique root, PID-scoped)

The server is run as a standalone 4-drive erasure set. To keep the run reproducible and
self-contained, every scenario used a **single disposable root** and a **non-default,
collision-checked port**, captured the server **PID**, and **polled readiness** before touching
anything:

```console
$ RUN_ROOT=/tmp/blitzy-heal-run.main          # unique disposable root (never the repo, never /app)
$ PORT=19000 ; CONSOLE=19500                  # non-default; verified free before bind
$ mkdir -p "$RUN_ROOT"/d1 "$RUN_ROOT"/d2 "$RUN_ROOT"/d3 "$RUN_ROOT"/d4
$ MINIO_CI_CD=1 /tmp/minio-bin server \
      "$RUN_ROOT"/d1 "$RUN_ROOT"/d2 "$RUN_ROOT"/d3 "$RUN_ROOT"/d4 \
      --address ":$PORT" --console-address ":$CONSOLE" > "$RUN_ROOT/minio.log" 2>&1 &
$ SRV_PID=$!                                   # PID captured for a scoped shutdown later
$ # readiness poll (not a fixed sleep):
$ until curl -sf "http://127.0.0.1:$PORT/minio/health/live" >/dev/null; do sleep 0.2; done
```

Default credentials `minioadmin:minioadmin`. `MINIO_CI_CD=1` bypasses the root-disk guard
(`getDiskInfo`, `cmd/xl-storage.go:367`, root-disk logic at L372-378) only when `/tmp` happens
to share a device with `/` on a given host;
it changes no healing behavior. The set auto-selects **EC:2**, confirmed at runtime:

```console
$ /tmp/s3cli-bin 127.0.0.1:19000 mb healtest        # create bucket
$ # server RAW heal item reports parityBlocks:2 dataBlocks:2 diskCount:4 → EC:2 on 4 drives
```

All `<DATADIR>` values used in fault injection were **resolved from trusted `ls` output,
validated to live under `$RUN_ROOT`, and quoted** before any write (finding #17). The helper
never expands an unquoted path:

```bash
# Resolve+validate a data-dir path, only ever returning a path inside RUN_ROOT.
datadir() {  # <drive> <bucket> <object>
  local d="$1" bucket="$2" obj="$3"
  local base="${RUN_ROOT}/d${d}/${bucket}/${obj}"
  local dd
  dd="$(ls -1 "${base}" 2>/dev/null | grep -E '^[0-9a-f]{8}-' | head -1 || true)"
  [ -n "$dd" ] || { echo "no-datadir d${d} ${bucket}/${obj}" >&2; return 1; }
  local full="${base}/${dd}"
  case "$full" in "${RUN_ROOT}"/*) : ;; *) echo "REFUSE unsafe ${full}" >&2; return 1;; esac
  [ -d "$full" ] || { echo "not-a-dir ${full}" >&2; return 1; }
  printf '%s\n' "$full"
}
```

Between runs, the object was reset (delete + re-PUT from a fixed source) so each capture starts
from the pristine baseline (finding #20):

```bash
# Recreate the standard 1 MiB baseline object on all 4 drives and verify readable.
reset_object() {  # <bucket> <object>
  local bucket="$1" obj="$2"
  s3 rm "$bucket" "$obj" >/dev/null 2>&1 || true
  # wipe any straggler dirs on the backend (safe: inside RUN_ROOT only)
  local d
  for d in 1 2 3 4; do
    case "${RUN_ROOT}/d${d}/${bucket}/${obj}" in
      "${RUN_ROOT}"/*) rm -rf "${RUN_ROOT}/d${d}/${bucket}/${obj}" ;;
    esac
  done
  s3 put "$bucket" "$obj" "$OBJSRC" >/dev/null
  s3 get "$bucket" "$obj"          # confirms the sha256 before the next scenario
}
```

**On-disk inspection tooling.** The per-drive state shown in the DURING/AFTER blocks was verified
by directly reading the backend files and by the two in-tree decoders (built from this checkout,
never modified) — note they decode **different** files:

- `xl-meta` (`docs/debugging/xl-meta/main.go:52`, usage `"xl.meta to JSON"`) decodes an object's
  **`xl.meta`** (the per-object metadata: erasure geometry `EcM`/`EcN`, `DDir`, `EcIndex`, ETag,
  size, versions). This is what produced the baseline `{"EcM":2,"EcN":2,…}` in §4.0 and the
  `[V1]` vs `[DM,V1]` distinction in §4.9.2.
- `healing-bin` (`docs/debugging/healing-bin/main.go:37`, usage `"healing.bin to JSON"`) decodes
  a **`.healing.bin`** file — the *drive*-level healing tracker written under `.minio.sys` during
  a drive heal — which is a distinct artifact from `xl.meta` and was not needed for these
  object-level scenarios.

### 7.3 Client provenance (why not `mc`)

`mc` could not be built offline in this environment, so the admin/S3 surface was driven by three
tiny purpose-built clients compiled **against the exact pinned dependencies** from this checkout's
`go.mod`, with `GOPROXY=off` (cached modules only):

| Harness client | Module used (pinned in `go.mod`) | Role |
|----------------|----------------------------------|------|
| `/tmp/healcli-bin` | `github.com/minio/madmin-go/v3 v3.0.77` | POSTs the canonical heal route (`madmin.Heal`), prints raw `HealResultItem` JSON, and reproduces `mc`'s `getHColCode` colour **verbatim** |
| `/tmp/s3cli-bin` | `github.com/minio/minio-go/v7 v7.0.80` | canonical S3 PUT/GET/STAT/RM/versioning used to build scenarios and observe reads |
| `/tmp/tracecli-bin` | `github.com/minio/madmin-go/v3 v3.0.77` | subscribes to the canonical `ServiceTrace` stream (the same stream `mc admin trace` consumes) to prove write-back |

These are **canonical entry points** — the heal client hits the real `POST /minio/admin/v3/heal/`
route and the real `healObject` worker; nothing is mocked or bypassed. The colour computation is
the only client-side reproduction, and it is byte-identical to `mc`'s table (§6.2). All three were
built as:

```console
$ cd /tmp/healcli && GOPROXY=off GOFLAGS=-mod=mod /usr/local/go/bin/go build -o /tmp/healcli-bin .
```

**Config isolation.** Unlike `mc alias set`, these clients write **no** persistent configuration:
each invocation takes the endpoint and credentials as arguments/flags
(e.g. `/tmp/s3cli-bin 127.0.0.1:19000 …`, `/tmp/healcli-bin -endpoint 127.0.0.1:19000 …`) and
holds them only in-process. There is no `~/.mc`/`MC_CONFIG_DIR` to leak into or clean up, so no
operator configuration can be altered. Had `mc` been used instead, every command would need an
isolated `--config-dir "$RUN_ROOT/mc"` (removed by the `rm -rf "$RUN_ROOT"` in §7.4).

### 7.4 Cleanup (leaves the repository byte-unchanged except the answer document)

On completion, the server is stopped **by its captured PID** (never a broad `pkill`), and every
ephemeral artifact is removed so `git status` shows only this document:

```bash
kill "$SRV_PID" 2>/dev/null; wait "$SRV_PID" 2>/dev/null || true   # PID-scoped shutdown
rm -rf /tmp/blitzy-heal-run.main            # the 4 drives + logs + fixture
rm -f  /tmp/minio-bin                        # built server binary
rm -rf /tmp/minio-src                        # detached build worktree
rm -rf /tmp/healcli /tmp/healcli-bin /tmp/s3cli /tmp/s3cli-bin \
       /tmp/tracecli /tmp/tracecli-bin /tmp/.v1meta.bin   # harness clients + scratch
rm -rf /tmp/heal-evidence /tmp/heal-*.sh /tmp/canon_ldflags.txt   # evidence + scripts
# verify: no listener remains on the run port, and the tree is clean
ss -ltnp 2>/dev/null | grep ':19000' || echo "no 19000 listener"
git -C <repo> status --porcelain           # → only: ?? blitzy/documentation/minio_c07e5b49d477.md
```

The MinIO source tree (`cmd/**`, `internal/**`, `docs/**`, build files) is **never modified**;
the only tracked change is this file.

---

## 8. Coverage pass — every part of the question, answered

**"Does MinIO ALWAYS reconstruct?"** — **No.** Three distinct observed outcomes:
**RECONSTRUCT** (§4.1–§4.6, ≤ 2 shards to heal), **PURGE / stays DELETED** (§4.8 D5; §4.9.1
data-only), and **DEGRADED / left in place** (§4.7 D4). The gate is `cannotHeal`
(`cmd/erasure-healing.go:428`) plus `isObjectDangling` (`:968`).

**"Situations where it decides the object should stay DELETED or DEGRADED?"** — DELETED:
dangling purge when metadata or data-dir is not-found beyond parity (§4.8, §4.9.1), and when a
minority delete marker is purged (§4.9.2 DEL-B leaves the marker gone). DEGRADED: too many
non-actionable corruptions to reconstruct but not dangling (§4.7).

**"Runtime evidence in each case, not theory"** — every claim above carries its raw
`HealResultItem` JSON, `MC` colour line, on-disk before/during/after, S3 GET/STAT output, and —
for reconstruction and triggers — a `ServiceTrace` capture (§4.10, §4.11). Each scenario was run
**twice** with resets between; all were stable.

**"BEFORE/AFTER status indicators?"** — yes: the `Before`/`After` per-drive `State` blocks of
`HealResultItem` (§6.1), rendered by the client as the green/yellow/red/grey grid (§6.2).

**"Do logs explain WHY?"** — partially and honestly: the three `healingLogOnceIf` predicates
(`:477/:487/:497`) are distribution-integrity assertions that did **not** trip for these inputs
(§6.3); the observable rationale at runtime is the `Detail` string, the drive-state grid, and the
`ServiceTrace` mechanism. Labeled observed-in-code vs triggered-at-runtime.

**"(a) how many valid shards for success?"** — at least `DataBlocks` = **2** valid *compatible*
shards; boundary walk 1→2→3 (§4.5, §5.1); write-back reads exactly 2 survivors (§4.10).

**"(b) what error when it cannot recover?"** — `Storage resources are insufficient for the read
operation …` (unwrapping `errErasureReadQuorum` = `Read failed. Insufficient number of drives
online`) for degraded (§4.7, §5.2), and `Object not found: …` for dangling purge (§4.8).

**"(c) partial WRITE vs partial DELETE?"** — different branches of `isObjectDangling` with
different thresholds; full cross-product in §4.9 and the comparison table in §5.3. WRITE
considers parts (`:1030`); DELETE ignores parts and uses data-majority (`:1016`); a delete marker
can only reach the dangling test via the EARLY site because the LATE gate excludes deletes.

**Named mechanisms exercised and cited:** `healObject` `:258`, `cannotHeal` `:428`, both
`deleteIfDangling` sites `:309`/`:438`, `isObjectDangling` `:968` (all four branches),
`danglingMetaErrsCount` `:934`, `danglingPartErrsCount` `:950`, `shouldHealObjectOnDisk` `:156`,
`defaultHealResult`, `objectQuorumFromMeta` `:531`, `Erasure.Heal` `:317`, `NewErasure`
`:42`/`reedsolomon.New` `:63`, `HealHandler` `:1308`, `LaunchNewHealSequence` `:296`,
`pushHealResultItem` `:618`, the dispatch chain to `erasureObjects.HealObject` `:1039`, the MRF
producer `erasure-object.go:397-414` + `mrf.go:220/272`, and the scanner
`data-scanner.go:954/61/58`. All three canonical triggers (manual, MRF-on-GET, background
scanner) were exercised (§4.11).

**"Create test scenarios and clean them once done"** — honored: all scenarios were built on the
disposable `/tmp` root and torn down (§7.4); the source tree remains byte-unchanged and the only
tracked artifact is this document.
