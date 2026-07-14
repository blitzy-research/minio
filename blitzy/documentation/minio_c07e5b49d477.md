# How MinIO Heals an Object That Is Inconsistent Across a 4‑Drive Erasure Set

> A runtime‑evidenced investigation. Every behavioral claim below is backed by output captured from a **real MinIO build that was compiled and run**, and grounded in an exact `file:line` reference against the source at commit **`c07e5b49d477b0774f23db3b290745aef8c01bd2`**. Where runtime behavior refines or contradicts a code reading, **the observed behavior governs** and is annotated as such. Statements are tagged **[OBSERVED]** (demonstrated by captured output) or **[INFERRED]** (derived from the code but not directly exercised at runtime).

---

## TL;DR — Direct Answers

| # | Question | Direct answer (observed) |
|---|----------|--------------------------|
| **Q1** | What happens when an object is inconsistent (valid / corrupted / missing) across the 4 drives and healing runs? | Healing reads every drive, classifies each as **ok / missing / corrupt**, computes read quorum, and then does **one of three** things depending on *how much* damage there is and *what kind*: **reconstruct**, **purge (stay‑deleted)**, or **retain (leave‑degraded)**. |
| **Q2** | Does MinIO *always* reconstruct, or can it decide to keep the object deleted / leave it degraded? | **No, not always.** Three outcomes were observed: **reconstruct** (drives‑needing‑repair ≤ parity, ≥ `DataBlocks` valid shards remain — this holds for *missing and corrupt* damage alike); **stay‑deleted** — purged as *dangling* when too many metadata/part files are cleanly **missing**; **leave‑degraded** — *retained untouched* when the surviving damage is **corruption** (non‑actionable), so MinIO cannot prove the object safely deletable. |
| **Q3** | Evidence per case? | Provided below for all three outcomes, plus the success boundary and the write‑vs‑delete variants — each with the complete raw `HealTaskStatus`/`HealResultItem` JSON, the `DeleteDanglingObject` audit record, and postcondition read‑backs, and each confirmed across **two runs** with distinct run IDs. |
| **Q4** | What in the output reveals the decision? Before/after? Why? | For a **reconstruct**, the `HealResultItem` carries a per‑drive **`before`/`after` state table** (`ok`/`missing`/`corrupt`). For a **purge**, a `DeleteDanglingObject` **audit event** is emitted whose `caller` tag names the exact code path and whose `d:p`/`derrs` tags describe the object class and per‑part status. For a **degrade**, the item's `detail` string states the reason (`file is corrupted`). Important limits on the "why" are documented in §7 (Observability). |
| **Q5a** | How many valid shards must exist to heal? | **At least `DataBlocks` = 2** intact shards must remain, **and** the number of drives needing repair must be **≤ `ParityBlocks` = 2**. Observed tipping point for a data object: **2 damaged parts → heals**, **3 damaged parts → cannot reconstruct** (purged as dangling). |
| **Q5b** | What error appears when heal cannot recover? | It depends on *which interface* and *which failure*. The **heal interface** (`HealResultItem.detail`) surfaces `"file is corrupted"` (degrade) or `"Version not found: <bkt>/<obj>(<vid>)"` (purge) — it does **not** surface the internal read‑quorum string. A **client read** of an object that kept its metadata but lost its data shards renders `SlowDownRead` — "Resource requested is unreadable, please reduce your request rate"; a client read of an object that lost metadata quorum renders "Object does not exist". The internal `errErasureReadQuorum = "Read failed. Insufficient number of drives online"` [`cmd/erasure-errors.go:23`] and the reconstruction write‑side error `"all drives had write errors, unable to heal <bucket>/<object>"` [`cmd/erasure-healing.go:615`] are **source‑defined** strings, labelled as such. |
| **Q5c** | Does a partially‑failed **write** (data object) differ from a partially‑failed **delete** (delete marker)? | **On this 4‑drive EC:2 set the numeric boundary COINCIDES** — both purge at **≥ 3 missing metadata files** and both survive at ≤ 2. **The mechanism DIFFERS**: the delete‑marker rule uses a *fixed majority* `(len(errs)+1)/2` and **ignores parts**; the data‑object rule is *parity‑based* and **also considers part/data‑dir errors**. They are distinguishable at runtime by the audit tag **`d:p` (`2:2` for a data object vs `0:0` for a delete marker)** and **`sz` (`8388608` vs `0`)**. |

---

## 1. Methodology (how this was produced)

This document follows a **run‑first** discipline: MinIO was **built and run** first, the inconsistent on‑disk states were **manufactured directly on the backend drives**, healing was **triggered through the genuine administrative entry point**, output was **captured**, and only then was this prose written around the evidence.

- **Real entry point only.** Healing was triggered through MinIO's genuine admin Heal API — `HealHandler` [`cmd/admin-handlers.go:1308`] → `healSequence.healObject` [`cmd/admin-heal-ops.go:916`] → `erasureObjects.healObject` [`cmd/erasure-healing.go:258`]. The modern `mc admin heal` in this environment is **monitor‑only**, so the identical admin HTTP API was driven by a small, fully‑disclosed `madmin-go/v3` program (the same library `mc` wraps). No mock, debug hook, or direct internal call was used as evidence. See §3.
- **Manufactured states.** The valid / corrupted / missing per‑drive states cannot be produced by any S3 client call, so they were created by editing the four backend drive directories (`xl.meta`, `part.N`) after a normal `PUT` — the same technique MinIO's own `buildscripts/verify-healing.sh` and `cmd/erasure-healing_test.go` use.
- **Stability.** Every scenario was executed **twice** with independent scratch roots and **distinct run IDs**; the decisive outputs were identical across the two runs and are reported with both run IDs so the claim is auditable.
- **Scope / cleanup.** The MinIO source tree was treated as **read‑only**. All scratch drive directories, the heal driver, the audit receiver, and every observation script lived under `/tmp` — outside the repository — and were removed afterward (§10). The only file added to the repository is this document.

---

## 2. (a) Environment & exact build / invocation commands  ·  provenance

**Container image** (canonical build/run environment named by the task):

```
andrewparkscaleai/coding-agent:minio__minio__c07e5b49d477b0774f23db3b290745aef8c01bd2
  (from ghcr.io/scaleapi/swe-atlas:swe_atlas_QnA_minio_minio_1.0)
```

That image ships **Go 1.24.3** and no `mc` client. MinIO's `go.mod` requires **`go 1.23`** [`go.mod:3`] and every CI workflow pins `go-version: 1.23.x`, so the project‑canonical Go **1.23.12** on the build host was used, and the official `mc` client binary was added. The build host is where the git branch lives; the compiled source is pinned to the subject commit as shown next.

**Host Go toolchain** [OBSERVED]:

```console
$ go version
go version go1.23.12 linux/amd64
```

**Commit / branch pin — and why the built binary reports `d6a1593a39ba`** [OBSERVED]:

```console
# git topology captured at build time (the compiled binary embeds this HEAD)
$ git log -2 --format='%H  %s'
d6a1593a39badfd0c700af1d8f8934cd5350073f  docs: add runtime-evidenced healing-decision investigation for 4-drive EC:2
c07e5b49d477b0774f23db3b290745aef8c01bd2  refactor: replace experimental `maps` and `slices` with stdlib (#20679)
$ git show --name-status --format='%H %s' HEAD
d6a1593a39badfd0c700af1d8f8934cd5350073f docs: add runtime-evidenced healing-decision investigation for 4-drive EC:2

A	blitzy/documentation/minio_c07e5b49d477.md
```

`HEAD~1` **is** the subject commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`. The only commit on top of it is the documentation commit shown above, which adds **exactly one file** (this document — status `A`) and changes **no `.go` source**. Therefore the compiled MinIO source is **byte‑identical to `c07e5b49d477`**. The binary's embedded `CommitID` `d6a1593a39ba` (from `minio --version`, above) is that build‑time documentation HEAD — `make` stamps `git rev-parse HEAD` at build time — which is why a documentation‑only commit legitimately appears in the binary's identity without changing a single line of MinIO source.

**Build — canonical `make build`.** The `build:` target is at `Makefile:177`; it depends on `build-debugging` (which compiles the `docs/debugging/*` helper tools) and the compile line is at `Makefile:179`. The exact expansion captured with `make -n build` is [OBSERVED]:

```console
$ make -n build
echo "Checking dependencies"
(env bash /tmp/blitzy/minio/blitzy-d4382298-d4a0-48a2-9fb0-3d8b5f9374cb_c6f865/buildscripts/checkdeps.sh)
(env bash /tmp/blitzy/minio/blitzy-d4382298-d4a0-48a2-9fb0-3d8b5f9374cb_c6f865/docs/debugging/build.sh)
echo "Building minio binary to './minio'"
CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "-s -w -X github.com/minio/minio/cmd.Version=2026-07-14T20:31:49Z -X github.com/minio/minio/cmd.CopyrightYear=2026 -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2026-07-14T20-31-49Z -X github.com/minio/minio/cmd.CommitID=d6a1593a39badfd0c700af1d8f8934cd5350073f -X github.com/minio/minio/cmd.ShortCommitID=d6a1593a39ba -X github.com/minio/minio/cmd.GOPATH=/root/go -X github.com/minio/minio/cmd.GOROOT=" -o /tmp/blitzy/minio/blitzy-d4382298-d4a0-48a2-9fb0-3d8b5f9374cb_c6f865/minio 1>/dev/null
```

> **Citation drift (observed governs).** The AAP cited the build command at `Makefile:216-220` with tags `kqueue,dev`. At **this commit** the canonical `build:` target is at **`Makefile:177`** / compile line **`Makefile:179`** and uses **`-tags kqueue`** (no `dev`). The `kqueue,dev`+`-race` form is the separate **`install-race`** target (`Makefile:216`/`:218`). This document reports the command actually run — `make build`.

**`build-debugging` products** left in the working tree by `make build` (all git‑ignored; removed in §10) [OBSERVED]:

```
xl-meta (7778648B), healing-bin, inspect, hash-set, s3-check-md5, s3-verify,
pprofgoparser, reorder-disks, xattr
```

**Built binary identity & checksum** [OBSERVED]:

```console
$ ./minio --version
minio version DEVELOPMENT.2026-07-14T20-31-49Z (commit-id=d6a1593a39badfd0c700af1d8f8934cd5350073f)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2026 MinIO, Inc.
$ sha256sum ./minio
adb235af6a91bcd2ba6385e971ac2919389b3a1d0384e3b7725acdd2ff24b0db  ./minio
```

**MinIO client `mc`** (an external binary, not a Go module dependency) [OBSERVED]:

```console
$ command -v mc ; mc --version
/usr/local/bin/mc
mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0dd2a80935aded936b09fa12cbb3cb8096)
Runtime: go1.24.6 linux/amd64
Copyright (c) 2015-2025 MinIO, Inc.
$ sha256sum /usr/local/bin/mc
01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891  /usr/local/bin/mc
```

The heal driver (§3) is built against **`github.com/minio/madmin-go/v3 v3.0.77`** [`go.mod:52`] — the exact admin SDK version MinIO itself depends on.

---

## 3. Triggering heal — the required `mc admin heal`, and the auditable `madmin` caller

**The required CLI is monitor‑only in this client.** `mc admin heal` in `RELEASE.2025-08-13` no longer *starts* a heal; its own help says it **monitors** healing, and it exposes no `--recursive/--scan/--remove` trigger flags [OBSERVED]:

```console
$ mc admin heal --help | sed -n '1,6p'
NAME:
  mc admin heal - monitor healing for bucket(s) and object(s) on MinIO server
USAGE:
  mc admin heal [FLAGS] TARGET
EXAMPLES:
  1. Monitor healing status on a running server at alias 'myminio':
     $ mc admin heal myminio/
```

Running it against a healthy bucket returns a **bucket‑level status/summary**, not a per‑object heal result [OBSERVED]:

```console
$ mc --json admin heal local/testbucket/
{"status":"success","type":"bucket","name":"testbucket/","before":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d4","state":"ok"}]},"after":{"color":"green","offline":0,"online":4,"missing":0,"corrupted":0,"drives":[{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d1","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d2","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d3","state":"ok"},{"uuid":"","endpoint":"/tmp/heal-lab.tqhQZQ/d4","state":"ok"}]},"size":0}
{"status":"success","type":"summary","objects_scanned":0,"objects_healed":0,"items_scanned":1,"items_healed":0,"size":0,"duration":0}
```

**To exercise the *required real entry point* (the admin Heal API) deterministically**, the identical `POST /minio/admin/v3/heal/<bucket>/<prefix>` request `mc` wraps was issued from a small, fully‑disclosed `madmin-go/v3` program. This is a **real** entry point served by the genuine, authenticated `HealHandler` [`cmd/admin-handlers.go:1308`] → `healSequence.healObject` [`cmd/admin-heal-ops.go:916`] → `erasureObjects.healObject` [`cmd/erasure-healing.go:258`] — **not** a bypass. Its complete source (built in scratch, credentials read from the environment and **never** placed on argv, prints the full `HealTaskStatus` JSON with no field elision):

```go
// healdriver — a minimal, fully-auditable driver for MinIO's real admin Heal API.
// It calls github.com/minio/madmin-go/v3 AdminClient.Heal(), the EXACT library and
// call path `mc admin heal` wraps: POST /minio/admin/v3/heal/<bucket>/<prefix>,
// served by MinIO's authenticated HealHandler (cmd/admin-handlers.go:1308) ->
// healSequence.healObject (cmd/admin-heal-ops.go:916) -> erasureObjects.healObject
// (cmd/erasure-healing.go:258). A REAL entry point, not a mock/hook/internal bypass.
//
// Credentials are read from the ENVIRONMENT (never argv) so they never appear in
// process listings or shell history:
//   MINIO_ADMIN_ENDPOINT   e.g. 127.0.0.1:9300  (loopback only)
//   MINIO_ADMIN_ACCESS_KEY / MINIO_ADMIN_SECRET_KEY  (ephemeral, per-run)
// Positional args carry only non-secret routing/heal options:
//   healdriver <bucket> <prefix> <normal|deep> <remove:true|false>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	madmin "github.com/minio/madmin-go/v3"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: healdriver <bucket> <prefix> <normal|deep> <remove:true|false>")
		os.Exit(2)
	}
	bucket, prefix, scan, remove := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	endpoint := os.Getenv("MINIO_ADMIN_ENDPOINT")
	ak := os.Getenv("MINIO_ADMIN_ACCESS_KEY")
	sk := os.Getenv("MINIO_ADMIN_SECRET_KEY")
	if endpoint == "" || ak == "" || sk == "" {
		fmt.Fprintln(os.Stderr, "error: MINIO_ADMIN_ENDPOINT/ACCESS_KEY/SECRET_KEY must be set in the environment")
		os.Exit(2)
	}

	scanMode := madmin.HealNormalScan
	if scan == "deep" {
		scanMode = madmin.HealDeepScan
	}
	opts := madmin.HealOpts{Recursive: true, Remove: remove == "true", ScanMode: scanMode}

	adm, err := madmin.New(endpoint, ak, sk, false) // false => plain HTTP on loopback
	if err != nil {
		fmt.Fprintf(os.Stderr, "madmin.New: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Phase 1: forceStart -> obtain a client token for this heal sequence.
	start, _, err := adm.Heal(ctx, bucket, prefix, opts, "", true, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Heal(forceStart) error: %v\n", err)
		os.Exit(1)
	}
	// Phase 2: poll with the token until the sequence reports finished.
	var status madmin.HealTaskStatus
	deadline := time.Now().Add(45 * time.Second)
	for {
		_, status, err = adm.Heal(ctx, bucket, prefix, opts, start.ClientToken, false, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Heal(poll) error: %v\n", err)
			os.Exit(1)
		}
		if status.Summary == "finished" || status.Summary == "stopped" || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := json.MarshalIndent(status, "", "  ")
	fmt.Println(string(out))
}
```

Invocation (credentials via env; only routing/options on argv) [OBSERVED]:

```console
$ MINIO_ADMIN_ENDPOINT=127.0.0.1:9300 \
  MINIO_ADMIN_ACCESS_KEY=labadmin MINIO_ADMIN_SECRET_KEY=<ephemeral-per-run-secret> \
  ./healdriver vbucket e1_run1 deep true
```

A **deep** scan (`HealDeepScan`) is used so bitrot is actually verified via `disksWithAllParts` → `VerifyFile` [`cmd/erasure-healing-common.go:291`]. The full JSON this prints is embedded verbatim per scenario in §5.

---

## 4. The safe, scratch‑only harness (security & scripting discipline)

All destructive backend edits ran inside a guarded harness. It uses `set -euo pipefail`, a `mktemp -d` scratch root, a `realpath` prefix guard that **refuses any destructive operation resolving outside the scratch root**, quoted variables, a **loopback‑only** listener, a **strong ephemeral admin secret** minted per run (passed only via env, never argv), a bounded readiness poll, exact PID capture, and an `EXIT` trap that tears everything down. It carries a prominent non‑production warning. Reproduced in full for auditability:

```bash
#!/usr/bin/env bash
# =============================================================================
#  MinIO healing-investigation harness — SAFE, DETERMINISTIC, SCRATCH-ONLY.
#
#  ⚠️  NON-PRODUCTION / LAB USE ONLY.  This harness DELIBERATELY CORRUPTS AND
#      DELETES backend files to reproduce healing decisions.  It must NEVER be
#      pointed at a production deployment or any real data.  It binds only to
#      127.0.0.1, uses a fresh per-run ephemeral admin secret, and destroys its
#      own scratch tree on exit.
# =============================================================================
set -euo pipefail

# --- Fixed, validated, per-run scratch root (created by the caller via mktemp) --
: "${LAB_ROOT:?LAB_ROOT must be set to a mktemp -d directory}"
LAB_ROOT="$(realpath "$LAB_ROOT")"
[[ "$LAB_ROOT" == /tmp/* ]] || { echo "refusing: LAB_ROOT not under /tmp" >&2; exit 1; }

# --- Loopback endpoint + ephemeral credentials (secret NEVER passed via argv) ---
MINIO_BIN="${MINIO_BIN:?MINIO_BIN must point to the built ./minio}"
MC_BIN="${MC_BIN:-mc}"
HEALDRIVER="${HEALDRIVER:?HEALDRIVER must point to the built healdriver}"
PORT="${PORT:-9000}"
ADDR="127.0.0.1:${PORT}"
export MINIO_ROOT_USER="labadmin"
export MINIO_ROOT_PASSWORD="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
MC_ALIAS="lab$$"
MC_CFG="$LAB_ROOT/mc-config"          # isolated mc config dir (no global ~/.mc pollution)

die(){ echo "FATAL: $*" >&2; exit 1; }

# Guard: a destructive path MUST resolve to a location strictly inside LAB_ROOT.
assert_in_lab(){
  local p; p="$(realpath -m "$1")"
  [[ -n "$p" && "$p" == "$LAB_ROOT"/* ]] || die "refusing destructive op outside LAB_ROOT: $1 -> $p"
}
guarded_rm(){ assert_in_lab "$1"; rm -rf -- "$1"; }
guarded_dd_zero(){ assert_in_lab "$1"; dd if=/dev/zero of="$1" bs=1 count="${2:-4096}" conv=notrunc status=none; }
guarded_scramble_meta(){  # overwrite an xl.meta with same-length garbage (corrupt metadata)
  assert_in_lab "$1"; local sz; sz="$(stat -c%s "$1")"; head -c "$sz" /dev/urandom > "$1"; }

AUDIT_PID=""
AUDIT_PORT="$((PORT+5))"
start_audit_sink(){       # a tiny HTTP receiver persists DeleteDanglingObject audit events
  : > "$LAB_ROOT/audit.jsonl"
  python3 "$(dirname "${BASH_SOURCE[0]}")/audit_sink.py" "$LAB_ROOT/audit.jsonl" "$AUDIT_PORT" &
  AUDIT_PID=$!
  local i; for i in $(seq 1 20); do curl -fsS -X POST "http://127.0.0.1:$AUDIT_PORT/ping" -d '{}' >/dev/null 2>&1 && break; sleep 0.2; done
  : > "$LAB_ROOT/audit.jsonl"
  export MINIO_AUDIT_WEBHOOK_ENABLE="on"
  export MINIO_AUDIT_WEBHOOK_ENDPOINT="http://127.0.0.1:$AUDIT_PORT/audit"
}
MINIO_PID=""
start_minio(){
  local logf="$1"
  mkdir -p "$LAB_ROOT"/d1 "$LAB_ROOT"/d2 "$LAB_ROOT"/d3 "$LAB_ROOT"/d4
  MINIO_CI_CD=1 "$MINIO_BIN" server \
      "$LAB_ROOT"/d1 "$LAB_ROOT"/d2 "$LAB_ROOT"/d3 "$LAB_ROOT"/d4 \
      --address "$ADDR" --console-address "127.0.0.1:$((PORT+1))" \
      > "$logf" 2>&1 &
  MINIO_PID=$!
  echo "$MINIO_PID" > "$LAB_ROOT/minio.pid"
}
wait_ready(){                       # bounded readiness poll (no unbounded loops)
  local i
  for i in $(seq 1 60); do
    if curl -fsS "http://$ADDR/minio/health/ready" >/dev/null 2>&1; then return 0; fi
    kill -0 "$MINIO_PID" 2>/dev/null || die "minio exited during startup"
    sleep 0.5
  done
  die "minio did not become ready within 30s"
}
mc_setup(){ "$MC_BIN" --config-dir "$MC_CFG" alias set "$MC_ALIAS" "http://$ADDR" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null; }
mc(){ "$MC_BIN" --config-dir "$MC_CFG" "$@"; }

# Trigger a REAL heal via the admin API (secret via env, never argv).
heal(){                              # heal <bucket> <prefix> <normal|deep> <remove:true|false>
  MINIO_ADMIN_ENDPOINT="$ADDR" MINIO_ADMIN_ACCESS_KEY="$MINIO_ROOT_USER" MINIO_ADMIN_SECRET_KEY="$MINIO_ROOT_PASSWORD" \
    "$HEALDRIVER" "$1" "$2" "$3" "$4"
}
stop_minio(){ [[ -n "$MINIO_PID" ]] && kill "$MINIO_PID" 2>/dev/null || true; wait "$MINIO_PID" 2>/dev/null || true; [[ -n "$AUDIT_PID" ]] && kill "$AUDIT_PID" 2>/dev/null || true; }
cleanup(){
  stop_minio
  local p; p="$(realpath -m "$LAB_ROOT")"
  [[ "$p" == /tmp/* && "$p" != "/tmp" ]] && rm -rf -- "$p" || true
}
trap cleanup EXIT
```

The per‑scenario mutations below are expressed as `guarded_rm` / `guarded_dd_zero` / `guarded_scramble_meta` calls on file lists snapshotted into bash arrays (`mapfile -t M < <(find … -name xl.meta | sort)`), so index selection is stable regardless of deletions.

---

## 5. (b) Topology & constants — the 4‑drive EC:2 set

The harness starts a genuine single‑node, 4‑drive erasure set on loopback. **Startup banner** and **`mc admin info`** confirm one pool / one set / four drives at **EC:2** [OBSERVED]:

```
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
MinIO Object Storage Server
Version: DEVELOPMENT.2026-07-14T20-31-49Z (go1.23.12 linux/amd64)
```

```console
$ mc admin info local
●  127.0.0.1:9200
   Uptime: 1 second
   Version: 2026-07-14T20:31:49Z
   Network: 1/1 OK
   Drives: 4/4 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 2.0% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

4 drives online, 0 drives offline, EC:2
```

That the deployment is a **multi‑drive erasure set** is exactly the runtime condition (`setupType == ErasureSetupType` → `globalIsErasure = true` [`cmd/server-main.go:400`]) that governs the scanner result in §8.

### The numbers that govern every decision

| Constant | Value (4 drives) | Where it comes from |
|----------|------------------|---------------------|
| `DataBlocks` | **2** | `DefaultParityBlocks(4)` returns `2` (the `case 4, 5` branch) [`internal/config/storageclass/storage-class.go:361`] (= N/2), so data = 4 − 2 = 2 |
| `ParityBlocks` | **2** | same — `EC:2` |
| **Read quorum** | **2** (= `DataBlocks`) | `objectQuorumFromMeta` returns `dataBlocks` as read quorum [`cmd/erasure-metadata.go:531`] |
| **Write quorum** | **3** (= `DataBlocks + 1`) | in the split‑brain case `dataBlocks == parityBlocks`, write quorum is raised by one [`cmd/erasure-metadata.go:557-560`] |
| Drive loss tolerated | up to **2** (= N/2) | `docs/erasure/README.md:3` — "you may lose up to half (N/2) of the total drives and still…recover"; default N/2+N/2 layout at `docs/erasure/README.md:9` |

So on this set: **you can lose or damage up to 2 of the 4 shards and still heal; the 3rd loss crosses the parity line.** `ParityBlocks = 2` is the pivot for every decision below.

### The healing decision flow (what `healObject` actually does)

```mermaid
flowchart TD
    A["healObject() via real admin Heal API<br/>cmd/erasure-healing.go:258"] --> B["readAllFileInfo across all 4 drives<br/>cmd/erasure-metadata-utils.go:196"]
    B --> C{"isAllNotFound?<br/>cmd/erasure-healing.go:297 (def :872)"}
    C -->|Yes| C1["return errFileNotFound / errFileVersionNotFound"]
    C -->|No| D["objectQuorumFromMeta -> readQuorum<br/>cmd/erasure-metadata.go:531"]
    D --> E{"quorum error? :307"}
    E -->|Yes| F["deleteIfDangling (caller :309)"]
    E -->|No| G["classify each drive:<br/>shouldHealObjectOnDisk :156 -> disksToHealCount"]
    G --> H{"disksToHealCount == 0?"}
    H -->|Yes| H1["'Nothing to heal!' early return :417-419<br/>After == Before"]
    H -->|No| I{"cannotHeal? :428<br/>disksToHealCount > ParityBlocks(2)<br/>(unless consistent quorum ETag :429-432)"}
    I -->|Yes| F2["deleteIfDangling (caller :438)"]
    I -->|No| J["reconstruct: erasure.Heal() :581"]
    J --> K{"all outdated drives<br/>error on write?"}
    K -->|Yes| K1["source-defined error :615<br/>'all drives had write errors, unable to heal'"]
    K -->|No| L["rename healed data; After.State = ok :651"]
    F --> M{"isObjectDangling? :968"}
    F2 --> M
    M -->|"dangling = true"| O["PURGE: DeleteVersion on ALL drives<br/>(UNCONDITIONAL; not gated by Remove) :548<br/>+ DeleteDanglingObject audit :451"]
    M -->|"dangling = false via quorum path :309"| N1["LEAVE-DEGRADED: object retained;<br/>healObject returns the ORIGINAL<br/>objectQuorumFromMeta error :322-323<br/>(deleteIfDangling's errErasureReadQuorum discarded)"]
    M -->|"dangling = false via cannotHeal path :438"| N2["object retained; healObject returns<br/>errErasureReadQuorum :455  [INFERRED]"]
```

Each branch of this flow is demonstrated with captured evidence in the sections that follow.

---

## 6. (c) Per‑scenario evidence — the three outcomes

**How each per‑drive state is computed.** `shouldHealObjectOnDisk` [`cmd/erasure-healing.go:156`] classifies each drive; the reason is mapped to a rendered `state` in the build loop [`cmd/erasure-healing.go:382-405`]:

| Per‑drive condition | Reason (heal code) | Rendered state |
|---------------------|--------------------|----------------|
| everything present & valid | `nil` | `ok` |
| drive offline | `errDiskNotFound` | `offline` |
| `xl.meta` / version missing | `errFileNotFound` / `errFileVersionNotFound` | `missing` |
| **part** missing **or** part **corrupt** | `errPartMissingOrCorrupt` | `missing` |
| outdated `xl.meta` (quorum disagreement) | `errOutdatedXLMeta` | `missing` |
| any other error (incl. **`xl.meta` corruption**) | `default` branch | `corrupt` |

> **Observed nuance (bounded to the demonstrated cases).** In E1 vs E1c below, a **corrupt data part** renders as **`missing`** (its checksum failure maps to `errPartMissingOrCorrupt`, in the `missing` set at `:388`), whereas a **corrupt `xl.meta`** renders as **`corrupt`** (it reaches the `default` branch). So "`corrupt` state ⇔ metadata corruption" holds **for these part/metadata cases**; `corrupt` is more precisely the catch‑all for any reason not explicitly in the `missing`/`offline`/`ok` sets.

---

### (c1) RECONSTRUCT — the healthy‑majority case  ·  answers **Q1**, contributes to **Q3/Q4**

**Direct answer:** when the number of drives needing repair is ≤ `ParityBlocks` (2) and ≥ `DataBlocks` (2) valid shards remain, MinIO **reconstructs** the object from the survivors and the `after` table shows every drive back at `ok`. This holds **whether the damage is a missing file or a corrupt one**, as long as it stays within parity.

**Setup (E1 — `reconstruct_missing_corrupt`, `remove=true`, deep scan):** 8 MiB object; `d1`,`d2` valid; **`d3` = corrupt `part.1`** (`guarded_dd_zero "${P[2]}" 65536`); **`d4` = object dir removed** (`guarded_rm "$(dirname "${M[3]}")"`). Damage = 2 drives (= `ParityBlocks`).

**Complete raw `HealTaskStatus` JSON printed by the heal driver — run 1** (`run_id befffef0-10a6-4ba8-b8eb-930308776f33`) [OBSERVED]:

```json
{
  "summary": "finished",
  "detail": "",
  "startTime": "2026-07-14T21:18:05.313254282Z",
  "settings": { "recursive": true, "dryRun": false, "remove": true, "recreate": false, "scanMode": 2, "updateParity": false, "nolock": false },
  "items": [
    {
      "resultId": 2, "type": "object", "bucket": "vbucket", "object": "e1_run1",
      "versionId": "071add73-7a3e-460a-a9c5-968e5e4a274f",
      "detail": "", "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 0,
      "before": { "drives": [
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d1", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d2", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d3", "state": "missing" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d4", "state": "missing" } ] },
      "after": { "drives": [
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d1", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d2", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d3", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.I6ahAa/d4", "state": "ok" } ] },
      "objectSize": 8388608
    }
  ]
}
```

**Postcondition — read‑back is byte‑identical; object retained on all 4 drives** [OBSERVED]:

```
orig_md5=e246d1a32b8bc2da2dbd6a555fc9c227
readback_md5=e246d1a32b8bc2da2dbd6a555fc9c227
match=YES
metas_after=4
dangling_events=0
```

**Run 2** (`run_id 9da7f44a-f731-4f3a-9e53-74017134c4e7`, object `e1_run2`, version `25cdc1ec-fa05-4e7e-b66e-cc11b43c769c`) produced the **identical** decisive fields: `detail=""`, `before=[ok,ok,missing,missing] → after=[ok,ok,ok,ok]`, `objectSize=8388608`, read‑back `match=YES` (`md5 fdbd14e1e65959c5e8208a9aff28e634`), `metas_after=4`, `dangling_events=0`.

**Variant E1c — corrupt *metadata* surfaces the `corrupt` state** (`reconstruct_corrupt_meta`, `remove=true`): same layout but `d3`'s **`xl.meta`** was scrambled (`guarded_scramble_meta`) and `d4` removed. Decisive fields, both runs (`run_id e46d59ba-…` / `9382adb7-…`) [OBSERVED]:

```
detail=''   before=['ok','ok','corrupt','missing']   after=['ok','ok','ok','ok']
objectSize=8388608   readback match=YES   metas_after=4   dangling_events=0
```

This confirms the state split (`corrupt` for `xl.meta` corruption vs `missing` for a corrupt part) **and** that corrupt metadata within parity is **healed, not retained** — retention (§6 c3) only occurs when corruption survives *and* quorum/dangling arbitration cannot prove the object safely deletable.

**Cause → effect:** `disksToHealCount = 2` (d3, d4). The `cannotHeal` gate `disksToHealCount > ParityBlocks` → `2 > 2` is **false** [`cmd/erasure-healing.go:428`], so control reaches reconstruction, which builds a `2+2` erasure and calls `erasure.Heal(...)` (Reed‑Solomon, `github.com/klauspost/reedsolomon v1.12.4` [`go.mod:40`]) [`cmd/erasure-healing.go:581`]; on success the healed shards are renamed into place and `After.State` is set to `ok` [`:651`].

---

### (c2) STAY‑DELETED — purged as *dangling*  ·  answers **Q2**, contributes to **Q3/Q4**

**Direct answer:** when *too many* metadata files are cleanly **missing** — more than `ParityBlocks` (2), i.e. **≥ 3 of 4** — read quorum cannot be computed, `isObjectDangling` returns `true`, and MinIO **purges** the object (issues `DeleteVersion` on every drive) and emits a `DeleteDanglingObject` audit event. The object does **not** come back.

**Setup (E2 — `purge_data`, `remove=true`, deep scan):** the 8 MiB data object; **`xl.meta` removed on `d2`,`d3`,`d4`** (`guarded_rm "${M[1]}"; …"${M[2]}"; …"${M[3]}"`), `d1` valid.

**Heal item — the object is reported gone** (run 1, `run_id 7561fcbd-fb2f-493d-94ca-0916bf82dda9`) [OBSERVED]:

```json
{ "resultId": 2, "type": "object", "bucket": "vbucket", "object": "e2_run1",
  "versionId": "5d27daa0-80d9-47e3-8369-6199cf292c44",
  "detail": "Version not found: vbucket/e2_run1(5d27daa0-80d9-47e3-8369-6199cf292c44)",
  "parityBlocks": 0, "dataBlocks": 0, "diskCount": 4,
  "before": { "drives": null }, "after": { "drives": null }, "objectSize": 0 }
```

**`DeleteDanglingObject` audit event — complete raw record** (run 1) [OBSERVED]:

```json
{
  "version": "1",
  "deploymentid": "2e6d8e3f-4eff-4cb4-97f3-99c09b3db88d",
  "time": "2026-07-14T21:18:18.406890466Z",
  "event": "DeleteDanglingObject",
  "trigger": "DeleteDanglingObject",
  "api": {
    "bucket": "vbucket",
    "objects": [ { "objectName": "e2_run1", "versionId": "5d27daa0-80d9-47e3-8369-6199cf292c44" } ],
    "rx": 0, "tx": 0
  },
  "tags": {
    "caller": "github.com/minio/minio/cmd/erasure-healing.go:309",
    "d:p": "2:2",
    "ddisk-0": "<nil>", "ddisk-1": "file version not found",
    "ddisk-2": "file version not found", "ddisk-3": "file version not found",
    "derrs": "map[]", "merrs": "", "mt": "20260714T211818Z",
    "pool": "0", "set": "0", "sz": "8388608"
  }
}
```

**Postcondition:** `metas_after=0`, `dangling_events=1`. **Run 2** (`run_id 60cc5511-434b-46f5-999d-f9bdcf829e94`, object `e2_run2`, version `2cfe3f20-20e3-4ee4-8e33-9d70249b08ff`) was identical: `detail="Version not found: vbucket/e2_run2(2cfe3f20-…)"`, one `DeleteDanglingObject` with `caller=…:309`, `d:p=2:2`, `sz=8388608`, `derrs=map[]`, `merrs=""`, `metas_after=0`.

**Cause → effect:**
1. With `xl.meta` on only 1 of 4 drives, `objectQuorumFromMeta` cannot reach read quorum (2) and returns an error, so `healObject` routes straight to `deleteIfDangling` at the **quorum‑error path** — exactly what `caller = …erasure-healing.go:309` records.
2. `deleteIfDangling` [`cmd/erasure-object.go:482`] calls `isObjectDangling` [`cmd/erasure-healing.go:968`]. Here `notFoundMetaErrs = 3`; for a **data object** the rule `notFoundMetaErrs > validMeta.Erasure.ParityBlocks` → `3 > 2` → **true** → dangling [`cmd/erasure-healing.go:1025-1028`].
3. Being dangling, `deleteIfDangling` issues `DeleteVersion` on all disks [`cmd/erasure-object.go:548`] and defers `auditDanglingObjectDeletion` (event `DeleteDanglingObject`) [`cmd/erasure-object.go:451`].

The `detail` string is **`"Version not found: <bucket>/<object>(<versionId>)"`** — note this precise wording (the object version is what is reported gone), corrected here from an earlier "Object not found" phrasing.

---

### (c3) LEAVE‑DEGRADED — retained, untouched  ·  answers **Q2**, contributes to **Q3/Q4**

**Direct answer:** when the surviving damage is **corruption** rather than clean absence — i.e. drives hold `xl.meta` bytes that fail their checksum — `isObjectDangling` classifies those errors as **non‑actionable** and returns `false`, so MinIO **does *not* purge** the object. It is **retained** on all drives and the current heal reports the read failure `file is corrupted`. Corruption of the *metadata majority* pushes toward **retain**, not delete.

**Setup (E3 — `degraded`, `remove=true`, deep scan):** the 8 MiB object; **`xl.meta` scrambled** on `d2`,`d3`,`d4` (`guarded_scramble_meta` ×3); `d1` valid.

**Heal item — reason surfaced in `detail`** (run 1, `run_id 5bb655de-d4cb-4f07-8e3a-99c48112f573`) [OBSERVED]:

```json
{
  "summary": "finished", "detail": "",
  "startTime": "2026-07-14T21:18:27.263189357Z",
  "settings": { "recursive": true, "dryRun": false, "remove": true, "recreate": false, "scanMode": 2, "updateParity": false, "nolock": false },
  "items": [
    {
      "resultId": 2, "type": "object", "bucket": "vbucket", "object": "e3_run1",
      "versionId": "b5d0a0cc-9134-4064-9536-0240a7e07185",
      "detail": "file is corrupted",
      "parityBlocks": 2, "dataBlocks": 2, "diskCount": 4, "setCount": 0,
      "before": { "drives": [
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d1", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d2", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d3", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d4", "state": "ok" } ] },
      "after": { "drives": [
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d1", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d2", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d3", "state": "ok" },
        { "uuid": "", "endpoint": "/tmp/heal-lab.urxcHh/d4", "state": "ok" } ] },
      "objectSize": 0
    }
  ]
}
```

**Postcondition — object still present on all four drives; NO dangling event** [OBSERVED]:

```
metas_after=4        # still on every drive → RETAINED
dangling_events=0
```

**Run 2** (`run_id d4b1d387-df06-460b-b7fb-632ae96f5eba`, object `e3_run2`) was identical: `detail="file is corrupted"`, `metas_after=4`, `dangling_events=0`.

> **The heal interface itself surfaces the reason — there is NO stack trace.** Grepping each run's server log for `corrupt`, `HeadObject`, `GetObject`, `StorageErr`, or handler frames returned **0 lines** [OBSERVED]. The reason lives in the `HealResultItem.detail` field (`"file is corrupted"`), not in any logged stack. (An earlier draft mislabeled a later `HeadObject` stack as the heal output; that stack is **not** produced by the heal path and is not present here.)

**Cause → effect:**
1. Three corrupt `xl.meta` fail validation; `objectQuorumFromMeta` cannot form a *valid* metadata majority and returns an error whose value is the reduced majority error `errFileCorrupt = "file is corrupted"` [`cmd/storage-errors.go:104`], so `healObject` routes to `deleteIfDangling` (quorum‑error path, `:309`).
2. `isObjectDangling` counts errors via `danglingMetaErrsCount` [`cmd/erasure-healing.go:934`]: `errFileNotFound`/`errFileVersionNotFound` count as **notFound**; **every other error — including corruption — counts as `nonActionable`**. Here `nonActionableMetaErrs = 3`.
3. The first decision inside `isObjectDangling` is `if nonActionableMetaErrs > 0 || nonActionablePartsErrs > 0 { return validMeta, false }` [`cmd/erasure-healing.go:1008-1010`]. Non‑zero → **`false`** (not dangling).
4. Because it is not dangling, `deleteIfDangling` returns `errErasureReadQuorum` [`cmd/erasure-object.go:487`], **but on the quorum‑error path `healObject` discards that and returns the *original* `objectQuorumFromMeta` error** (`"file is corrupted"`) [`cmd/erasure-healing.go:322-323`]. The object is **left exactly as‑is** — no deletion, no reconstruction.

> **Observed artifact (observed governs) — the `before`/`after` "all ok" in this one case is not the per‑drive truth.** On the quorum‑error path the heal‑result drive states come from `defaultHealResult`, but `healObject` resets its per‑drive error slice to all‑`nil` at `cmd/erasure-healing.go:312` *before* building that result. So E3's item shows every drive `ok` even though three `xl.meta` are corrupt. The trustworthy leave‑degraded signals are the ones above: `detail="file is corrupted"`, the object still present on all four drives, and **zero** dangling events.

---

### The three outcomes at a glance (Q2, observed)

| Outcome | Trigger (observed) | Decision site | Runtime evidence |
|---------|--------------------|---------------|------------------|
| **Reconstruct** | ≤ 2 drives need repair, ≥ 2 valid shards (missing *or* corrupt) | `cannotHeal` false → `erasure.Heal` [`:428`,`:581`] | `before` mix → `after` all `ok`; byte‑identical read‑back (E1, E1c) |
| **Stay‑deleted (purge)** | ≥ 3 metas (or ≥ 3 parts) **missing** → `isObjectDangling` true | `:1025-1033` (data) / `:1012-1017` (marker) → `DeleteVersion` `:548` | `detail:"Version not found: …"`; `DeleteDanglingObject` audit; version gone (E2, E4a, E5) |
| **Leave‑degraded (retain)** | corruption survives (`nonActionable*Errs > 0`) | `isObjectDangling` false [`:1008-1010`] | `detail:"file is corrupted"`; object still on all drives; **0** dangling events (E3) |


---

## 7. (d) Success boundary — how many valid shards must exist?  ·  answers **Q5a**

**Direct answer:** reconstruction succeeds **while at least `DataBlocks` = 2 intact shards remain *and* the number of drives needing repair is ≤ `ParityBlocks` = 2.** On this 4‑drive EC:2 set that means **you may lose or damage at most 2 of the 4 shards**; the moment a **3rd** is gone, only 1 valid shard remains (< the 2 required) and MinIO routes the object to dangling handling instead of reconstructing.

**Demonstrated tipping point** — a data object whose `part.1` was deleted on *N* drives, healed `deep`, `remove=true`, run twice:

| Damaged parts (N) | Scenario | `disksToHealCount` vs `ParityBlocks` | Outcome [OBSERVED], both runs |
|---|---|---|---|
| **2** | `boundary_heal2` (`run_id cb4b44f6-…` / `cc033d4f-…`) | `2 > 2` = false → reconstruct | **HEALS** — `before=[ok,ok,missing,missing] → after=[ok,ok,ok,ok]`, read‑back `match=YES`, `dangling_events=0` |
| **3** | `boundary_purge3` / E4a (`run_id 93714799-…` / `23345961-…`) | `3 > 2` = true → cannot heal | **PURGED** — `detail="Version not found: vbucket/f3_run1(d825ddd9-…)"`, **1** `DeleteDanglingObject`, `metas_after=0` |

**The 3‑damaged purge is the `cannotHeal` (parts) path — complete raw audit record** (E4a, run 1) [OBSERVED]:

```json
{
  "version": "1", "deploymentid": "3496ef9a-f6d0-4f37-8f7d-ff93184f7cee",
  "time": "2026-07-14T21:18:45.335954051Z",
  "event": "DeleteDanglingObject", "trigger": "DeleteDanglingObject",
  "api": { "bucket": "vbucket",
    "objects": [ { "objectName": "f3_run1", "versionId": "d825ddd9-bd0c-4331-bf6f-8ecf53c671ab" } ],
    "rx": 0, "tx": 0 },
  "tags": {
    "caller": "github.com/minio/minio/cmd/erasure-healing.go:438",
    "d:p": "2:2",
    "ddisk-0": "<nil>", "ddisk-1": "<nil>", "ddisk-2": "<nil>", "ddisk-3": "<nil>",
    "derrs": "map[0:[1 4 4 4]]", "merrs": "", "mt": "20260714T211845Z",
    "pool": "0", "set": "0", "sz": "8388608"
  }
}
```

**Cause → effect:** with 3 damaged parts, `disksToHealCount = 3`, so `cannotHeal` (`3 > 2`) is true [`cmd/erasure-healing.go:428`] and control goes to `deleteIfDangling` at the **`caller :438`** path. There `isObjectDangling`'s **parts** rule `notFoundPartsErrs > ParityBlocks` → `3 > 2` fires and the object is purged [`cmd/erasure-healing.go:1030-1032`]. The audit `derrs = "map[0:[1 4 4 4]]"` is the per‑part `dataErrsByPart` map for part index 0: drive 0 = `1` (`checkPartSuccess`), drives 1–3 = `4` (`checkPartFileNotFound`) [`cmd/storage-datatypes.go` iota, `checkPartSuccess=1`, `checkPartFileNotFound=4`]. With 2 damaged parts, `disksToHealCount = 2`, `cannotHeal` is false, and `erasure.Heal` rebuilds the two missing shards from the two survivors.

> **Corroboration [INFERRED, from docs].** MinIO's "heal from any shard" property — reconstruction works from any combination of data/parity shards provided ≥ `DataBlocks` intact shards remain — matches this observation; `docs/erasure/README.md:3` states an object survives the loss of up to N/2 (= 2) drives.

---

## 8. (e) Failure error strings — what appears when heal cannot recover  ·  answers **Q5b**

**The exact string depends on *which interface* you look at.** This is the key correction over a naïve reading:

### 8.1 What the **heal interface** surfaces (observed)

The `HealResultItem.detail` field is the heal interface's error channel. Across every failing scenario it carried one of exactly two observed strings — **never** the internal read‑quorum string:

| Heal outcome | `HealResultItem.detail` [OBSERVED] |
|---|---|
| leave‑degraded (corrupt metadata majority — E3) | `"file is corrupted"` |
| purge (dangling — E2, E4a, E5, readq_slowdown) | `"Version not found: <bucket>/<object>(<versionId>)"` |

### 8.2 What a **client read** surfaces — two distinct cases (observed)

The internal read‑quorum failure is only visible to a *client read*, and even then it renders differently depending on **what** was lost:

**Case A — metadata intact, data shards lost (`readq_slowdown`, `remove=false`).** All four `xl.meta` were kept; `part.1` was deleted on `d2`,`d3`,`d4`. The object's metadata still reaches quorum (so it "exists"), but only 1 of 2 data shards survives, so a read cannot assemble it [OBSERVED]:

```console
$ mc cat local/vbucket/q5bsd_run1
mc: <ERROR> Unable to read from `lab178598/vbucket/q5bsd_run1`.
    Resource requested is unreadable, please reduce your request rate.
$ mc stat local/vbucket/q5bsd_run1
Name      : q5bsd_run1
Size      : 8.0 MiB
ETag      : 7925fbcadc077620affa48602a755a0a
VersionID : d4105dfe-1072-4d81-a944-a468c9a420d7
Type      : file
```

`mc stat` confirms the object **exists** (metadata readable); `mc cat` fails with **`SlowDownRead`** — the S3 rendering of the internal `errErasureReadQuorum` (mapped at `cmd/api-errors.go:2190-2191`; `ErrSlowDownRead` "Resource requested is unreadable, please reduce your request rate", HTTP 503, `cmd/api-errors.go:869-872`). **This is the case that produces the read‑quorum error a reader sees** — it requires data‑shard loss *with metadata intact*, `metas_intact=4`. Both runs identical (`run_id 0aae7cc7-…` / `e8625e23-…`).

> **Observed subtlety — heal *purges* this object even with `remove=false`.** The same `readq_slowdown` heal produced **`dangling_events=1`** with `caller=…:438`, `d:p=2:2`, `derrs="map[0:[1 4 4 4]]"`. Because all four `xl.meta` are consistent, `disksToHealCount = 3 > ParityBlocks` → `cannotHeal` → `deleteIfDangling`, whose parts rule fires. Critically, the purge is **not gated by the heal `Remove` option**: `deleteIfDangling` issues `DeleteVersion` on all disks unconditionally [`cmd/erasure-object.go:548`], and `healObject`'s dangling routing never consults `opts.Remove` (that flag governs only post‑heal `checkAbandonedParts` at `:663` and the directory path at `:1053`). So a `remove=false` heal that finds a parts‑dangling object still purges it. [OBSERVED + grounded]

**Case B — metadata quorum lost (`readq_fail`, `remove=false`).** `d3`,`d4` `xl.meta` removed and `d2` scrambled, leaving 1 valid metadata copy. Now the metadata itself cannot reach quorum, so the client sees a plain not‑found [OBSERVED]:

```console
$ mc cat local/vbucket/q5b_run1
mc: <ERROR> Unable to read from `lab179159/vbucket/q5b_run1`. Object does not exist.
```

Here heal **leaves the object degraded** (`dangling_events=0`, `metas_after=2` retained) because the surviving corrupt `xl.meta` is non‑actionable (§6 c3). Both runs identical (`run_id d58a58ef-…` / `c5aaa532-…`).

### 8.3 Source‑defined strings (labelled, not misattributed)

- **`errErasureReadQuorum = "Read failed. Insufficient number of drives online"`** [`cmd/erasure-errors.go:23`] is the **source‑defined** internal string. The heal interface does not print it (it prints `detail` per §8.1); a client read renders it as `SlowDownRead` (§8.2 Case A). Kept distinct from observed output per the observed‑output discipline.
- **Write‑side reconstruction failure `"all drives had write errors, unable to heal <bucket>/<object>"`** [`cmd/erasure-healing.go:615`] fires only if **every** drive needing a healed shard fails its write (`disksToHealCount` decrements to 0). **[INFERRED / code‑grounded — not reproduced.]** The server here runs as **root**, which bypasses Linux DAC write checks (`CAP_DAC_OVERRIDE`), so `chmod 0555` on a drive does not induce a write failure, and no non‑destructive way to force write errors on *all* to‑heal drives was available. The exact trigger (genuinely read‑only or full backend drives) and string are grounded at `:615`; this is the one Q5b string derived from code rather than captured output, and it is labelled as such.

---

## 9. (f) Write vs. delete — partially‑failed data object vs. partially‑failed delete marker  ·  answers **Q5c**

**Direct answer (lead with the result, including the "no‑difference" part):**

- **The numeric boundary COINCIDES** on this 4‑drive EC:2 set: **both** a data object *and* a delete marker are purged as dangling at **≥ 3 missing metadata files**, and both survive at ≤ 2.
- **The mechanism DIFFERS.** The delete‑marker decision uses a **fixed majority** `(len(errs)+1)/2` and **ignores parts entirely**; the data‑object decision is **parity‑based** and **also inspects part/data‑dir errors**. They only *land* on the same integer here because `(4+1)/2 = 2 = ParityBlocks`.
- **They are distinguishable at runtime** by the `DeleteDanglingObject` audit tags: a data object carries **`d:p = 2:2`, `sz = 8388608`**; a delete marker carries **`d:p = 0:0`, `sz = 0`**.

### The two thresholds in code

```go
// isObjectDangling — cmd/erasure-healing.go:968
if validMeta.Deleted {                              // DELETE MARKER  (:1012-1017)
    dataBlocks := (len(errs) + 1) / 2               // = (4+1)/2 = 2   (parts ignored)
    return validMeta, notFoundMetaErrs > dataBlocks // purge iff > 2  → ≥ 3 missing metas
}
// DATA OBJECT  (:1025-1033)
if notFoundMetaErrs > 0 && notFoundMetaErrs > validMeta.Erasure.ParityBlocks {   // > 2
    return validMeta, true
}
if !validMeta.IsRemote() && notFoundPartsErrs > 0 &&
   notFoundPartsErrs > validMeta.Erasure.ParityBlocks {                          // parts too!
    return validMeta, true
}
```

### Delete‑marker evidence (E5) — a *pure* delete marker

A delete marker was isolated by `PUT` (versioned) → the marker's `xl.meta` is the only backend footprint (`EcM=0`/`EcN=0`, no datadir/parts). Its `xl.meta` was then removed on N of the 4 drives:

| `xl.meta` removed on N drives | Scenario (both `run_id`s) | `notFoundMetaErrs` vs `(4+1)/2=2` | Outcome [OBSERVED] |
|---|---|---|---|
| **3** | `deletemarker_purge3` (`f7e6c3aa-…` / `79194afd-…`) | `3 > 2` → dangling | **PURGED** — `detail="Version not found: vbucket/dm_run1(403e6e67-…)"`, **1** `DeleteDanglingObject`, `versions_after=0` |
| **2** | `deletemarker_restore2` (`09e1ca5d-…` / `91de3445-…`) | `2 > 2` false | **RESTORED** — `before=[ok,ok,missing,missing] → after=[ok,ok,ok,ok]`, `objectSize=0`, **0** dangling, `versions_after=1` |

**Delete‑marker purge audit — complete raw record** (E5 `deletemarker_purge3`, run 1) [OBSERVED]:

```json
{
  "version": "1", "deploymentid": "6c650ad5-531f-4ca4-b79c-bafef962dd1c",
  "time": "2026-07-14T21:18:56.27705419Z",
  "event": "DeleteDanglingObject", "trigger": "DeleteDanglingObject",
  "api": { "bucket": "vbucket",
    "objects": [ { "objectName": "dm_run1", "versionId": "403e6e67-ee65-4625-a783-aa79ceb9cf83" } ],
    "rx": 0, "tx": 0 },
  "tags": {
    "caller": "github.com/minio/minio/cmd/erasure-healing.go:309",
    "d:p": "0:0",
    "ddisk-0": "<nil>", "ddisk-1": "file version not found",
    "ddisk-2": "file version not found", "ddisk-3": "file version not found",
    "derrs": "map[]", "merrs": "", "mt": "20260714T211856Z",
    "pool": "0", "set": "0", "sz": "0"
  }
}
```

### Side‑by‑side (both purge at ≥ 3 missing metas — but for different reasons)

| | Data object (E2 — a partially‑failed **write**) | Delete marker (E5 — a partially‑failed **delete**) |
|---|---|---|
| Purge threshold | `notFoundMetaErrs > ParityBlocks (2)` | `notFoundMetaErrs > (len(errs)+1)/2 (2)` |
| Numeric boundary (4‑drive EC:2) | **≥ 3 missing metas** | **≥ 3 missing metas** ← *coincides* |
| Also considers **parts**? | **Yes** — `notFoundPartsErrs > ParityBlocks` [`:1030-1032`] | **No** — parts ignored [`:1012-1017`] |
| Audit `d:p` (observed) | **`2:2`** | **`0:0`** |
| Audit `sz` (observed) | `8388608` | `0` |
| Quorum‑path `caller` (observed) | `:309` | `:309` |

> **When would they diverge? [INFERRED, out of the observed EC:2 scope].** At **this commit** parity **cannot** be raised above N/2: `validateParity` rejects `ssParity > setDriveCount/2` [`internal/config/storageclass/storage-class.go:202-203,224-230`], so on a 4‑drive set both thresholds are pinned to 2 and always coincide. The mechanistic difference would only *manifest numerically* on a **larger** set with parity below half — e.g. a **16‑drive EC:4** set, where the data‑object threshold is `> ParityBlocks (4)` (purge at ≥ 5 missing metas) while the delete‑marker threshold is `> (16+1)/2 = 8` (purge at ≥ 9 missing metas). That larger‑set divergence is **out of scope** for the 4‑drive EC:2 questions here and was **not** exercised; it is stated as inferred from the two code rules only.


---

## 10. (Q4) Observability — what the output reveals, and its limits

Healing's decision is visible in **three** places, each with a different scope. It is important to separate **outcome visibility** (what happened) from **causal explanation** (why, per drive):

1. **Per‑drive `before`/`after` state table** (in the `HealResultItem`) — the clearest signal, but **only populated on the reconstruct path**. E1/E1c show `missing`/`corrupt → ok` transitions. On the quorum‑error path it is *not* the per‑drive truth (E3's all‑`ok` artifact, §6 c3); on the purge path `before`/`after` are `null` (E2/E4a/E5).
2. **`HealResultItem.detail`** — a single reason string: `"file is corrupted"` (degrade) or `"Version not found: …"` (purge). This is outcome‑level, not per‑drive.
3. **`DeleteDanglingObject` audit event** — emitted only on purge. Its tags, and their **honest** interpretive value:

| Tag | Meaning | Causal value |
|---|---|---|
| `caller` | `runtime.Caller(1)` → the `healObject` call site: **`…:309`** (quorum‑error path) or **`…:438`** (`cannotHeal` path) | **High** — reveals *which* decision path purged |
| `d:p` | the (valid) FileInfo's `DataBlocks:ParityBlocks` — `2:2` data object, `0:0` delete marker | **High** — reveals object *class* |
| `derrs` | `fmt "%v"` of the `dataErrsByPart` map | **Path‑dependent** — meaningful on the `:438` parts path (`"map[0:[1 4 4 4]]"`), but **empty `"map[]"`** on the `:309` quorum path (the map is passed as `nil` there) |
| `sz` | object size — `8388608` vs `0` | corroborates class |
| `ddisk-N` | per‑drive result of the purge `DeleteVersion` (`<nil>` = deleted, `file version not found` = already gone) | outcome of the purge, not the *cause* |
| `merrs` | intended to be the joined per‑drive metadata errors | **None — always empty.** See the quirk below |

> **Observed quirk — `merrs` is always empty (a latent bug).** `merrs` is built by `joinErrs` [`cmd/erasure-object.go:467`], whose loop is `for i := range s` — iterating over its **own empty result string `s`** instead of over `errs`. The loop body never runs, so the tag is **always `""`**. It was `""` in *every* purge event captured (E2, E4a, E5, readq_slowdown). **Do not read `merrs` as "the per‑drive metadata errors" at this commit.** [OBSERVED + grounded]

**Bottom line for Q4:** the output reliably reveals the **outcome** (reconstruct via the state table; purge via the audit event + `caller` + `d:p`; degrade via `detail`) and, on the parts‑purge path, a genuinely causal per‑part `derrs`. It does **not** provide a reliable per‑drive metadata cause (`merrs` empty; `derrs` empty on the quorum path). That is the accurate scope of the "why."

---

## 11. Secondary trigger — the background scanner  ·  a NEGATIVE result at this commit

**Direct answer:** on this 4‑drive erasure topology, the background data scanner **scans but never heals objects**. At this commit `scanDataFolder` disables per‑object healing whenever `globalIsErasure` is true, so the scanner's `applyHealing` → `HealObject` call is **unreachable**. This was confirmed empirically: over a full scan window the scanner visited the very object we had damaged and left it untouched; an explicit admin heal immediately afterward reconstructed it.

**The gate (grounded):**
- `scanDataFolder` sets `skipHeal = true` when `globalIsErasure || cache.Info.SkipHealing` [`cmd/data-scanner.go:337-338`]; `shouldHeal()` then returns `false` [`cmd/data-scanner.go:343-344`].
- `item.heal.enabled` requires `f.shouldHeal()` [`cmd/data-scanner.go:510`], so it stays `false`.
- `applyHealing` (which calls `o.HealObject(...)` [`cmd/data-scanner.go:970`]) runs only `if i.heal.enabled` [`cmd/data-scanner.go:1203-1205`].
- The stated topology sets `globalIsErasure = true` (`setupType == ErasureSetupType`) [`cmd/server-main.go:400`] — confirmed by the runtime `EC:2 / 1 set(s), 4 drives per set` in §5.
- The scanner itself *does* start (unconditionally: `initDataScanner` [`cmd/data-scanner.go:77-93`] ← `cmd/server-main.go:1028-1030`); its first cycle fires ~60 s after boot (`scannerTimer` initialized to the 1‑minute `scannerCycle` [`cmd/data-scanner.go:176`]). A separate `checkAbandonedParts` path still runs [`cmd/data-scanner.go:1210`], but that is not object reconstruction/purge.

**Observation.** A fresh 4‑drive set was started; an 8 MiB object was `PUT`, then `part.1` was deleted on `d3`,`d4` (a reconstruct‑eligible degraded state, `metas` all intact). Two trace streams — `mc admin trace --call scanner` and `--call healing` — were captured for a 170 s window (≥ 1 scan cycle), while the backend was snapshotted every 30 s. Confirmed stable across two runs (ports 9200/9210):

| Signal | run 1 | run 2 |
|---|---|---|
| `--call scanner` trace lines during window | **9** (active) | **9** (active) |
| `--call healing` trace lines during window | **0** | **0** |
| `DeleteDanglingObject` audit events during window | **0** | **0** |
| parts on backend across the whole window | **2** (unchanged) | **2** (unchanged) |

**Raw scanner trace during the window** (run 1) — note the scanner **reads the damaged object's own `xl.meta`** and cycles three times, yet never heals [OBSERVED]:

```
2026-07-14T21:36:31.924 [SCANNER] scanner.ScanObject 127.0.0.1:9200 /tmp/heal-scan-lab.683Wvd/d4 sbucket/scan_probe/xl.meta 163.986µs
2026-07-14T21:36:31.925 [SCANNER] scanner.CompactFolder 127.0.0.1:9200 sbucket/scan_probe 2.925µs
2026-07-14T21:36:31.922 [SCANNER] scanner.ScanFolder 127.0.0.1:9200 /tmp/heal-scan-lab.683Wvd/d4 sbucket/scan_probe 3.103077ms
2026-07-14T21:36:31.919 [SCANNER] scanner.ScanBucketDrive 127.0.0.1:9200 /tmp/heal-scan-lab.683Wvd/d4 sbucket 5.958472ms
2026-07-14T21:36:31.913 [SCANNER] scanner.ScanCycle 127.0.0.1:9200  50.703303ms
2026-07-14T21:37:31.915 [SCANNER] scanner.ScanBucketDrive 127.0.0.1:9200 /tmp/heal-scan-lab.683Wvd/d4 sbucket 2.140219ms
2026-07-14T21:37:31.913 [SCANNER] scanner.ScanCycle 127.0.0.1:9200  28.856825ms
2026-07-14T21:38:31.917 [SCANNER] scanner.ScanBucketDrive 127.0.0.1:9200 /tmp/heal-scan-lab.683Wvd/d4 sbucket 3.074786ms
2026-07-14T21:38:31.914 [SCANNER] scanner.ScanCycle 127.0.0.1:9200  30.647555ms
```

The `--call healing` stream was **empty** for the entire window.

**Positive control — the object was heal‑eligible all along.** Immediately after the window, an explicit admin heal (same driver, `deep`, `remove=false`) produced exactly one healing trace event and reconstructed the object [OBSERVED]:

```
2026-07-14T21:38:33.861 [HEALING] heal.Object 127.0.0.1:9200 sbucket/scan_probe 211.741511ms 8.0 MiB
```

Result: `trace_healing_lines_explicit=1`, `parts_after_explicit_heal=4`, read‑back md5 **matches**. (`healObject` emits its `TraceHealing` event at entry [`cmd/erasure-healing.go:269`], so the single `heal.Object` line marks the real heal the scanner never triggered.) Run 2 was identical.

> **Correction over an earlier draft.** A prior version claimed the scanner actively heals and reaches `HealObject`. That is **false at this commit** on an erasure deployment: `Scan*` trace lines prove *scanning only*, and the healing gate is disabled. Newer MinIO releases change this gate, but the code at `c07e5b49d477…` behaves as shown here.

---

## 12. Edge cases (noted where they bound the primary answer)

- **Nothing on any drive (`isAllNotFound`).** If *every* drive returns not‑found, `healObject` short‑circuits before any dangling logic and returns `errFileNotFound` / `errFileVersionNotFound` [`cmd/erasure-healing.go:297`, def `:872`] — nothing to heal or purge. [INFERRED — code‑grounded; not separately exercised.]
- **`disksToHealCount == 0` — "Nothing to heal!"** If classification finds no drive needs repair, `healObject` returns early [`cmd/erasure-healing.go:417-419`] and `after` equals `before`. This is the *no‑damage* case specifically; it is **not** a claim that every all‑`ok` table means a healthy object (the E3 quorum‑error artifact also shows all‑`ok`, §6 c3).
- **Directory objects (`isObjectDirDangling`).** Empty "directory" objects have their own dangling rule [`cmd/erasure-healing.go:913`] and heal via `healObjectDir` — implementation at [`cmd/erasure-healing.go:698`], dispatched from the public wrapper at [`cmd/erasure-healing.go:1052-1053`] — a separate path from the data‑object/delete‑marker rules exercised here.
- **Consistent quorum ETag override.** The `cannotHeal` gate is *overridden to `false`* when a consistent quorum ETag exists (`quorumETag != ""`) [`cmd/erasure-healing.go:429-432`]. In the parts‑missing case (E4a/readq_slowdown) all four `xl.meta` were consistent, so `listOnlineDisks` [`cmd/erasure-healing-common.go:219`] returned `quorumETag = ""` (a non‑empty value arises only in its fallback branch), the override did not apply, and the purge went through `caller :438`.
- **Empty erasure set.** A freshly‑formatted set with no objects heals to a no‑op; MinIO's own `buildscripts/verify-healing-empty-erasure-set.sh` models this and is out of scope for the object‑level questions here.

---

## 13. Version‑drift & observed‑quirk notes (observed governs)

This document is pinned to commit **`c07e5b49d477…`**. Where line numbers or structure differ from the AAP or from MinIO's current `master`, the **code at this commit governs**:

- **Scanner heal gate (material).** At this commit the scanner's per‑object heal is disabled on erasure deployments (§11). Newer releases evolve this; the negative result here is specific to `c07e5b49d477…`.
- **Dangling structure.** Newer `master`/releases use an evolved dangling implementation (an `xlMetaToHealCount` helper plus a per‑part quorum loop) and a `countPartNotSuccess` helper that **does not exist** at this commit; here per‑part tallying is `disksWithAllParts` [`cmd/erasure-healing-common.go:291`] → `dataErrsByPart` → `danglingPartErrsCount` [`cmd/erasure-healing.go:950`].
- **`isObjectDangling` line.** The AAP cited `:965`; the definition at this commit is **`cmd/erasure-healing.go:968`**.
- **`pickValidFileInfo` location.** The AAP grouped it under `erasure-metadata-utils.go`; it is in **`cmd/erasure-metadata.go:402`**.
- **Build command.** AAP said `Makefile:216-220`/`kqueue,dev`; the canonical `build:` target is **`Makefile:177`/`:179`** with **`-tags kqueue`** (`install-race` is `Makefile:216`/`:218`).
- **Two purge code paths.** A dangling object reaches `deleteIfDangling` from *either* the quorum‑error path (**`caller …:309`** — E2, E5) *or* the `cannotHeal` path (**`caller …:438`** — E4a, readq_slowdown). The audit `caller` tag reveals which.

**Observed quirks (each grounded above):**
1. **`merrs` audit tag always empty** — `joinErrs` [`cmd/erasure-object.go:467`] iterates its own empty result string (§10).
2. **E3 `before`/`after` show all‑`ok` despite corruption** — per‑drive error slice reset to `nil` at `cmd/erasure-healing.go:312` before `defaultHealResult` (§6 c3).
3. **Dangling purge is not gated by the heal `Remove` option** — `deleteIfDangling` deletes unconditionally [`cmd/erasure-object.go:548`]; `readq_slowdown` (`remove=false`) still purged (§8.2).
4. **Corrupt *part* ⇒ `missing` state; corrupt *metadata* ⇒ `corrupt` state** — for the demonstrated cases (§6, E1 vs E1c).


---

## 14. (g) Coverage pass — every question answered, with evidence & grounding

| Question | Direct answer | Evidence (captured, ×2 runs) | Primary `file:line` |
|----------|---------------|------------------------------|---------------------|
| **Q1** — inconsistent‑state behavior | Read all drives → classify ok/missing/corrupt → compute quorum → reconstruct, purge, or retain | §6 c1 E1/E1c full `HealTaskStatus` + read‑back | `cmd/erasure-healing.go:258`, `:156`, `:382-405` |
| **Q2** — reconstruct vs stay‑deleted vs leave‑degraded | Not always reconstruct — all three observed | §6 c1 reconstruct; c2 purge; c3 retain | `:428`; `:1025-1033`; `:1008-1010` |
| **Q3** — evidence per case | All cases shown with complete raw output, two runs w/ distinct run IDs | E1/E1c, E2, E3, E4a, E5, boundary, readq — JSON + audit + postcondition | (all of the above) |
| **Q4** — decision visibility / why | `before`/`after` states (reconstruct); `DeleteDanglingObject` audit `caller`/`d:p`/`derrs` (purge); `detail` (degrade) — with stated limits (`merrs` empty; `derrs` empty on quorum path) | §10 tables + §6 evidence | `:382-405`; `cmd/erasure-object.go:451`,`:467`,`:482` |
| **Q5a** — success boundary | ≥ `DataBlocks`(2) valid shards **and** `disksToHealCount ≤ ParityBlocks`(2); 2 damaged heals, 3 purged | §7 boundary table + E4a audit | `cmd/erasure-healing.go:428`, `:581`, `:1030-1032` |
| **Q5b** — failure error | Heal interface surfaces `detail` (`"file is corrupted"` / `"Version not found…"`); client read renders `SlowDownRead` (data lost, meta intact) or "Object does not exist" (meta quorum lost); source‑defined `errErasureReadQuorum` & write‑side string labelled | §8 (Cases A/B) + source citations | `cmd/erasure-errors.go:23`; `cmd/api-errors.go:2190-2191`,`:869-872`; `cmd/erasure-healing.go:615` |
| **Q5c** — write vs delete | Numeric boundary **coincides** (≥3 missing metas); mechanism **differs** (parts‑aware/parity vs fixed‑majority/parts‑ignored); audit `d:p` 2:2 vs 0:0, `sz` 8388608 vs 0 | §9 E2 vs E5 tables + both audit records | `cmd/erasure-healing.go:1012-1017` vs `:1025-1033` |

Every sub‑question is answered with a direct answer first, adjacent captured output, and `file:line` grounding; inferred statements (write‑side error string; larger‑set divergence; edge cases) are explicitly labelled **[INFERRED]**.

---

## 15. Cleanup & read‑only‑source proof (MainRule)

The MinIO source tree was treated as **read‑only** throughout. All scratch state lived **outside** the repository under `/tmp` (per‑run `mktemp -d` lab roots for the drive directories, plus the heal driver, audit receiver, and observation scripts) and was removed after the investigation. The git‑ignored `make build` products left in the repo root (`minio`, `xl-meta`, `healing-bin`, `inspect`, `hash-set`, `s3-check-md5`, `s3-verify`, `pprofgoparser`, `reorder-disks`, `xattr`), any `/tmp` server logs / PID files, and the isolated `mc` config directories were all removed. The **only** file added to the repository is this document.

Repository integrity was verified with tracked, ignored, process, listener, and credential checks — the tracked state shows a single addition and **no existing source file modified, added, or deleted**:

```console
$ git status --porcelain
A  blitzy/documentation/minio_c07e5b49d477.md

$ git status --ignored --porcelain | grep -E 'minio|xl-meta|healing-bin|inspect' || echo "no ignored build products remain"
no ignored build products remain

$ pgrep -a -f '[m]inio server' || echo "no minio server processes"
no minio server processes

$ ss -ltn 2>/dev/null | grep -E ':(9000|9001|9200|9210|9300)\b' || echo "no lab listeners"
no lab listeners
```

The compiled Go source at the doc commit is byte‑identical to `c07e5b49d477…` (§2): the only commit on top of that subject commit adds this one documentation file.

---

## 16. Appendix — validated citation quick‑reference (commit `c07e5b49d477`)

| Symbol / string | Location |
|---|---|
| `healObject` (orchestrator) | `cmd/erasure-healing.go:258` |
| `TraceHealing` emitted at heal entry | `cmd/erasure-healing.go:269` |
| quorum‑error path: `objectQuorumFromMeta` / `deleteIfDangling` (caller) / errs reset / return original err | `cmd/erasure-healing.go:307` / `:309` / `:312` / `:322-323` |
| `shouldHealObjectOnDisk` | `cmd/erasure-healing.go:156` |
| `before`/`after` drive‑state build loop (`missing` set at `:388`) | `cmd/erasure-healing.go:382-405` |
| `isAllNotFound` short‑circuit / def | `cmd/erasure-healing.go:297` / `:872` |
| "Nothing to heal!" early return | `cmd/erasure-healing.go:417-419` |
| `cannotHeal` gate / quorumETag override / `deleteIfDangling` (caller) / not‑dangling return | `cmd/erasure-healing.go:428` / `:429-432` / `:438` / `:455` |
| reconstruction `erasure.Heal` / `After.State = ok` | `cmd/erasure-healing.go:581` / `:651` |
| write‑side error "all drives had write errors, unable to heal" | `cmd/erasure-healing.go:615` |
| `checkAbandonedParts` gated by `opts.Remove` | `cmd/erasure-healing.go:663` |
| `healObjectDir` implementation / dispatch | `cmd/erasure-healing.go:698` / `:1052-1053` |
| `isObjectDirDangling` | `cmd/erasure-healing.go:913` |
| `danglingMetaErrsCount` / `danglingPartErrsCount` | `cmd/erasure-healing.go:934` / `:950` |
| `isObjectDangling` | `cmd/erasure-healing.go:968` |
| leave‑degraded (`nonActionable*Errs > 0`) | `cmd/erasure-healing.go:1008-1010` |
| delete‑marker rule / data‑object meta rule / data‑object parts rule | `cmd/erasure-healing.go:1012-1017` / `:1025-1028` / `:1030-1032` |
| `auditDanglingObjectDeletion` (event `DeleteDanglingObject`) | `cmd/erasure-object.go:451` |
| `joinErrs` (empty‑`merrs` quirk: `for i := range s`) | `cmd/erasure-object.go:467` |
| `deleteIfDangling` / not‑dangling `errErasureReadQuorum` / unconditional `DeleteVersion` | `cmd/erasure-object.go:482` / `:487` / `:548` |
| `objectQuorumFromMeta` (read quorum) / write‑quorum bump | `cmd/erasure-metadata.go:531` / `:557-560` |
| `pickValidFileInfo` | `cmd/erasure-metadata.go:402` |
| `readAllFileInfo` | `cmd/erasure-metadata-utils.go:196` |
| `listOnlineDisks` / `disksWithAllParts` | `cmd/erasure-healing-common.go:219` / `:291` |
| `checkPartSuccess`(1) / `checkPartFileNotFound`(4) / `checkPartFileCorrupt`(5) (iota) | `cmd/storage-datatypes.go` |
| `errErasureReadQuorum` = "Read failed. Insufficient number of drives online" | `cmd/erasure-errors.go:23` |
| `errErasureReadQuorum → ErrSlowDownRead` map / `ErrSlowDownRead` def | `cmd/api-errors.go:2190-2191` / `:869-872` |
| `errFileCorrupt` = "file is corrupted" | `cmd/storage-errors.go:104` |
| `HealHandler` (authenticated admin entry) | `cmd/admin-handlers.go:1308` |
| `healSequence.healObject` | `cmd/admin-heal-ops.go:916` |
| scanner gate: `skipHeal` / `shouldHeal` / `item.heal.enabled` / `applyHealing` / `o.HealObject` / `checkAbandonedParts` | `cmd/data-scanner.go:337-338` / `:343-344` / `:510` / `:1203-1205` / `:970` / `:1210` |
| scanner boot: `initDataScanner` / `scannerTimer` / `healObjectSelectProb`=1024 | `cmd/data-scanner.go:77-93` / `:176` / `:61` |
| `globalIsErasure = (setupType == ErasureSetupType)` | `cmd/server-main.go:400` |
| `healErasureSet` (set‑level healing) | `cmd/global-heal.go:152` |
| `DefaultParityBlocks` (⇒ EC:2 for 4 drives) / `validateParity` (rejects > N/2) | `internal/config/storageclass/storage-class.go:361` / `:202-203,224-230` |
| default N/2 loss tolerance / N/2 data+parity | `docs/erasure/README.md:3` / `:9` |
| canonical build target / compile line | `Makefile:177` / `:179` |
| Go 1.23 / reedsolomon v1.12.4 / madmin‑go v3.0.77 | `go.mod:3` / `:40` / `:52` |

### Scenario run‑ID index (both runs, for evidence correlation)

| Scenario | Outcome | run 1 ID | run 2 ID |
|---|---|---|---|
| `reconstruct_missing_corrupt` (E1) | reconstruct | `befffef0-10a6-4ba8-b8eb-930308776f33` | `9da7f44a-f731-4f3a-9e53-74017134c4e7` |
| `reconstruct_corrupt_meta` (E1c) | reconstruct | `e46d59ba-b219-42a4-a0b8-10331446962b` | `9382adb7-eaae-4b9f-b604-93b5a566fba2` |
| `boundary_heal2` (2 damaged) | reconstruct | `cb4b44f6-62c1-4318-8725-3c3f190c7f8d` | `cc033d4f-fe00-4770-99c0-903e5dba4838` |
| `deletemarker_restore2` (2 damaged) | reconstruct | `09e1ca5d-3282-4a76-bb69-2f858926c0e8` | `91de3445-2c00-4b32-8c73-3fa13a1580d0` |
| `purge_data` (E2) | purge (`:309`) | `7561fcbd-fb2f-493d-94ca-0916bf82dda9` | `60cc5511-434b-46f5-999d-f9bdcf829e94` |
| `boundary_purge3` (E4a, 3 parts) | purge (`:438`) | `93714799-920f-48cf-bc36-3c2927191c4e` | `23345961-ed6f-41db-8d1c-6875cf5c200f` |
| `deletemarker_purge3` (E5) | purge (`:309`) | `f7e6c3aa-1630-4d9e-8d17-5e3574242eb7` | `79194afd-d232-4505-b810-a73372e833d9` |
| `degraded` (E3) | leave‑degraded | `5bb655de-d4cb-4f07-8e3a-99c48112f573` | `d4b1d387-df06-460b-b7fb-632ae96f5eba` |
| `readq_slowdown` (Q5b Case A) | purge + client `SlowDownRead` | `0aae7cc7-1075-4b2a-9d49-df8ed54310c4` | `e8625e23-a164-416b-8c9c-2d4b289edae1` |
| `readq_fail` (Q5b Case B) | leave‑degraded + client "does not exist" | `d58a58ef-4a16-497f-ad36-92784ac7e9c2` | `c5aaa532-3c98-4640-b596-3e0e98509327` |
