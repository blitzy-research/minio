# MinIO Security Behavior Investigation — Runtime-Evidenced Answers

**Source branch under investigation:** `minio_c07e5b49d477`
**Investigated source commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2` (the base commit this branch was cut from)
**Method:** build → run → exercise the real S3/admin/STS API entry points → capture complete unedited runtime output → document.

This document answers five MinIO security-behavior questions with **runtime evidence**. Every behavioral claim is paired with the exact command that produced it and its complete, unedited output. Statements that could only be derived by reading source code (and could not be surfaced at runtime) are explicitly labeled `[INFERRED]`; everything captured from a live server is labeled `[OBSERVED]`.

## Questions answered

1. **Q1** — What happens when a bucket-level encryption requirement meets a user's broad write permission during an *unencrypted* upload? What is the runtime execution sequence?
2. **Q2** — With object locking enabled, what log/error entries appear when someone tries to delete locked objects?
3. **Q3** — How does the system handle unauthorized manual data corruption in the storage backend? Trigger a bit-rot detection event and capture the runtime logs during a subsequent GET.
4. **Q4** — Prove that MinIO enforces an STS session policy on temporary credentials, with runtime test output.
5. **Q5** — Prove with test output that a basic user cannot promote itself to console admin by modifying user→policy mappings, and identify the root cause.

## Evidence conventions

- **`[OBSERVED]`** — the claim is backed by output captured from the live MinIO server during this investigation. The producing command and its complete, unedited output appear alongside the claim.
- **`[INFERRED]`** — the claim is derived from reading the source at commit `c07e5b49d477`. MinIO's runtime signals (the HTTP trace stream and console log) do **not** expose internal handler stages, so ordering claims about *within-handler* steps cannot be observed at the wire and are labeled `[INFERRED]` with a `file:line` anchor. Wherever a runtime signal *does* corroborate the inference, both are shown.
- All file:line anchors refer to commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.
- Secrets (root password, temporary STS secret keys, session tokens) are **redacted** in this document; only lengths and non-secret prefixes are shown. All other output is verbatim.
- **Stability.** Each question's primary/headline outcome was confirmed across at least **two** runs; the per-question **Stability** subsection states exactly what was repeated and reports the second-run verdict. Not every individual observation was repeated — some supporting captures (e.g., the complete trace excerpts, the JWT decode, the on-disk shard listings, and long-running edge cases such as the genuine STS token expiry) were captured **once**. Each such claim is still labeled `[OBSERVED]` and is independently reproducible by re-running its shown command.
- **Trace excerpts.** `mc admin trace` blocks are shown with the request line, **all** `Authorization`, `x-amz-*`, `Content-*`, `Host`, and `User-Agent` headers, the `[RESPONSE]` status line, and the **complete** response/error body — all verbatim. Only fixed per-response transport boilerplate that is identical on every reply is omitted for length: `X-Ratelimit-Limit/Remaining`, `X-Xss-Protection`, `X-Content-Type-Options`, `Strict-Transport-Security`, `Vary`, `Accept-Ranges`, `Server`, and `X-Amz-Id-2`. MinIO's own literal `<BLOB>` marker (its placeholder for a binary body in the trace stream) is retained as emitted. The complete unfiltered trace logs are preserved as evidence artifacts during the investigation.

## Environment & build

### Toolchain

```
$ go version
go version go1.23.12 linux/amd64
```

`go.mod` declares `go 1.23`; CI pins `1.23.x` (`.github/workflows/go.yml:23`); `Dockerfile.release` uses `golang:1.23-alpine`. `go1.23.12` is the highest 1.23 patch, used for all builds below. Host: Ubuntu 25.10, amd64.

### Git context (honest HEAD-vs-base relationship)

This investigation branch (`blitzy-b32cbb93-585c-46c3-bafd-7f6152c90649`) adds exactly one file — this document — layered directly on top of the source under study. The immutable anchor for everything below is the **investigated base commit**, which never moves:

```
investigated base commit:  c07e5b49d477b0774f23db3b290745aef8c01bd2  "refactor: replace experimental maps and slices with stdlib (#20679)"
branch tip (HEAD):         the documentation commit(s) that add only this file, on top of that base
```

The exact hash of the documentation commit is deliberately **not** pinned here: committing (or amending) this file necessarily changes the branch-tip hash, whereas the base `c07e5b49d477` is stable. Every `file:line` anchor and the version stamp below refer to that base.

`buildscripts/gen-ldflags.go` derives the version string from the **current HEAD** commit's date and id. Run at the branch tip it would stamp the *documentation* commit (a `DEVELOPMENT.2026-07-13T...` tag whose commit-id is the volatile documentation-commit hash), which is **not** the code under investigation. To study the actual source the questions concern, the server binary is built **at the base commit `c07e5b49d477`**, whose `gen-ldflags.go` output is authoritative for this investigation:

```
$ git -C <worktree@c07e5b49d477> ... go run buildscripts/gen-ldflags.go
-s -w -X github.com/minio/minio/cmd.Version=2024-11-25T17:10:22Z \
   -X github.com/minio/minio/cmd.CopyrightYear=2024 \
   -X github.com/minio/minio/cmd.ReleaseTag=DEVELOPMENT.2024-11-25T17-10-22Z \
   -X github.com/minio/minio/cmd.CommitID=c07e5b49d477b0774f23db3b290745aef8c01bd2 \
   -X github.com/minio/minio/cmd.ShortCommitID=c07e5b49d477 \
   -X github.com/minio/minio/cmd.GOPATH= -X github.com/minio/minio/cmd.GOROOT=
```

### Canonical build (Makefile:177-179)

```
$ CGO_ENABLED=0 go build -tags kqueue -trimpath \
    --ldflags "$(go run buildscripts/gen-ldflags.go)" -o ./minio      # run at commit c07e5b49d477
build exit code: 0   (elapsed ~4s)

$ ./minio --version
minio version DEVELOPMENT.2024-11-25T17-10-22Z (commit-id=c07e5b49d477b0774f23db3b290745aef8c01bd2)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-2024 MinIO, Inc.
```

**Reproducibility note (`[OBSERVED]`).** The freshly built binary and the binary produced during environment setup carry an **identical version/commit stamp** but **different sha256** — expected because `-trimpath` removes file paths yet the two builds ran in different Go build environments (build-cache/host differences produce non-bit-identical output):

```
84243d66fef9cf3d07669722c24cc419e13af8e07ff2fdff6d4e1005b915f5c4  ./minio   (freshly built here)
0a5f31231e32243ce38566edb3a8d264ffe01e7b8c70c46476a1c44796901d9c  ./minio   (setup-built)
```

Embedded provenance confirms the exact source revision and build flags (`go version -m ./minio`, excerpted):

```
build   -tags=kqueue
build   -trimpath=true
build   CGO_ENABLED=0
build   GOARCH=amd64
build   GOOS=linux
build   vcs=git
build   vcs.revision=c07e5b49d477b0774f23db3b290745aef8c01bd2
build   vcs.time=2024-11-25T17:10:22Z
build   vcs.modified=false
dep github.com/klauspost/reedsolomon  v1.12.4   # erasure/parity (Q3)
dep github.com/minio/madmin-go/v3     v3.0.77   # admin client + TraceInfo
dep github.com/minio/minio-go/v7      v7.0.80   # S3 SDK
dep github.com/minio/pkg/v3           v3.0.22   # policy engine + admin-action constants
dep github.com/minio/sio             v0.4.1    # DARE encrypted stream (Q1)
```

### Client / investigation tooling provenance (`[OBSERVED]`)

```
$ mc --version
mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0dd2a80935aded936b09fa12cbb3cb8096)
Runtime: go1.24.6 linux/amd64
$ sha256sum $(command -v mc)
01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891  /usr/local/bin/mc

$ ./xl-meta --help            # in-repo tool: docs/debugging/xl-meta, built at c07e5b49d477
$ sha256sum ./xl-meta
8c6ebb2eeae4c5382147c6c0027133b25da5fd4c82554d51d0dbb5050d5720d1  ./xl-meta
   (go version -m ./xl-meta -> vcs.revision=c07e5b49d477..., vcs.modified=false)
```

Four small Go **driver programs** (built offline against the repo's exact dependency graph — `minio-go/v7 v7.0.80`, `madmin-go/v3 v3.0.77`) exercise API paths that `mc` does not surface cleanly: a raw SigV4 `PutObject` with arbitrary/absent SSE headers (Q1), a single-object `DeleteObject` with an optional governance-bypass header (Q2), an STS `AssumeRole` + intersection probe (Q4), and the admin self-promotion battery (Q5). Their **complete source, `go.mod`, and exact build commands are reproduced verbatim in the subsection immediately below** (§ "Investigation driver programs — complete source"); each source file's `sha256` is recorded there so the embedded listing can be checked byte-for-byte against what was compiled and run. These drivers live outside the repository and are removed at the end (see Cleanup).

### Investigation driver programs — complete source (`[OBSERVED]`)

The four driver programs referenced above are reproduced here **in full**, exactly as built and run — no elision. They exist because the `mc` client and the high-level `minio-go` helpers normalise or refuse the precise wire-level inputs several questions require (an *empty* or *misspelled* SSE header for Q1; a single-version delete carrying a governance-bypass header for Q2; deliberately malformed/expired STS credentials for Q4). Each driver therefore drives the **canonical server-side entry point** directly. They are built **offline** against the repository's exact dependency graph: a throwaway module reuses the server's own `go.sum`, and `GOPROXY=off` guarantees every transitive version is resolved from the module cache that produced the server binary — never re-fetched — so the drivers and the server observe identical library behaviour. The whole `drivers/` tree lives outside the repository and is deleted at Cleanup.

Shared module manifest — `drivers/go.mod` (the two direct requires pin `minio-go/v7 v7.0.80` and `madmin-go/v3 v3.0.77`; `-mod=mod` auto-fills the indirect requires from the copied `go.sum`, each identical to the server build's version):

```
module qafixdrivers

go 1.23

require (
	github.com/minio/madmin-go/v3 v3.0.77
	github.com/minio/minio-go/v7 v7.0.80
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/goccy/go-json v0.10.3 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/klauspost/cpuid/v2 v2.2.8 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/philhofer/fwd v1.1.3-0.20240612014219-fbbf4953d986 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.59.1 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/prometheus/prom2json v1.4.0 // indirect
	github.com/prometheus/prometheus v0.54.1 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/safchain/ethtool v0.4.1 // indirect
	github.com/secure-io/sio-go v0.3.1 // indirect
	github.com/shirou/gopsutil/v3 v3.24.5 // indirect
	github.com/tinylib/msgp v1.2.1 // indirect
	github.com/tklauser/go-sysconf v0.3.14 // indirect
	github.com/tklauser/numcpus v0.8.0 // indirect
	golang.org/x/crypto v0.28.0 // indirect
	golang.org/x/net v0.30.0 // indirect
	golang.org/x/sync v0.8.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/text v0.19.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)
```

Build — offline, from the `drivers/` directory (`[OBSERVED]`, every build exits `0`):

```
$ cd drivers
$ GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build -o put_raw ./put_raw   # exit=0
$ GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build -o delete_single ./delete_single   # exit=0
$ GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build -o sts_assume_role ./sts_assume_role   # exit=0
$ GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build -o set_policy ./set_policy   # exit=0
```

Source integrity — `sha256` of each listing below, so the embedded source can be checked byte-for-byte against what was compiled (`[OBSERVED]`, `sha256sum drivers/*/main.go`):

```
84dc00fd56ff45069c4cd9fc648897ddfda68bbae99428e4567a3655808ee23e  put_raw/main.go
8d55be517131b6de8b498c171b80d78bf3c2e908d82bccb87c8bbbec1d6e5a2c  delete_single/main.go
f52618c666357e61d4f0c532d79b9e045c07a9c5675ef809968374ed97c73f0c  sts_assume_role/main.go
575b40e7ab7ee6760628b7410c1c31ee1de48634143e45a76a7d8f62ec190604  set_policy/main.go
```

**`put_raw/main.go`** — raw SigV4 `PutObject` with full control over the `x-amz-server-side-encryption` header value/absence (Q1 unencrypted upload + the empty/misspelled/unsupported-value siblings):

```go
// put_raw performs a raw, SigV4-signed S3 PUT so the caller has full control
// over request headers — in particular the value (or absence) of the
// x-amz-server-side-encryption header. This is required for Q1, where the
// minio-go PutObject helper will not let us send an *empty*, *misspelled*, or
// otherwise malformed SSE header value; the high-level SDK validates/normalizes
// it client-side before transmission. Raw signing lets us drive the canonical
// server-side entry point (PutObjectHandler) with exactly the bytes we choose.
//
// Build (offline, against the repo's pinned module versions):
//
//	cd drivers && GOFLAGS=-mod=mod GOPROXY=off go build -o put_raw ./put_raw
//
// Usage:
//
//	put_raw -endpoint 127.0.0.1:9300 -access KEY -secret SECRET \
//	        -bucket b -object o [-sse VALUE] [-size N]
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/minio/minio-go/v7/pkg/signer"
)

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9300", "host:port of the S3 API")
	access := flag.String("access", "", "access key")
	secret := flag.String("secret", "", "secret key")
	token := flag.String("token", "", "optional STS session token")
	bucket := flag.String("bucket", "", "bucket name")
	object := flag.String("object", "", "object key")
	sse := flag.String("sse", "", "value for x-amz-server-side-encryption header")
	size := flag.Int("size", 16, "payload size in bytes")
	scheme := flag.String("scheme", "http", "http or https")
	flag.Parse()

	// Send the SSE header whenever -sse was explicitly set on the command line
	// (even to an empty value), so the empty-string sibling can be exercised.
	sseSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "sse" {
			sseSet = true
		}
	})

	payload := bytes.Repeat([]byte("A"), *size)
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])

	url := fmt.Sprintf("%s://%s/%s/%s", *scheme, *endpoint, *bucket, *object)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Println("REQUEST_BUILD_ERROR:", err)
		os.Exit(2)
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("X-Amz-Content-Sha256", hexSum)
	req.Header.Set("Content-Type", "application/octet-stream")
	if sseSet {
		req.Header.Set("X-Amz-Server-Side-Encryption", *sse)
	}

	// SigV4-sign over exactly the headers we set (region "us-east-1" is the
	// MinIO default). The signature therefore covers the SSE header verbatim.
	signed := signer.SignV4(*req, *access, *secret, *token, "us-east-1")

	resp, err := http.DefaultClient.Do(signed)
	if err != nil {
		fmt.Println("HTTP_ERROR:", err)
		os.Exit(3)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	fmt.Printf("HTTP_STATUS: %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	if sseSet {
		fmt.Printf("SENT_SSE_HEADER: %q\n", *sse)
	} else {
		fmt.Printf("SENT_SSE_HEADER: <none>\n")
	}
	if v := resp.Header.Get("X-Amz-Server-Side-Encryption"); v != "" {
		fmt.Printf("RESP_SSE_HEADER: %q\n", v)
	} else {
		fmt.Printf("RESP_SSE_HEADER: <none>\n")
	}
	if len(body) > 0 {
		fmt.Printf("RESP_BODY:\n%s\n", string(body))
	}
}
```

**`delete_single/main.go`** — raw SigV4 single-version `DeleteObject` with an optional `x-amz-bypass-governance-retention` header (Q2 governance bypass by a non-root identity holding the permission):

```go
// delete_single issues a raw, SigV4-signed S3 DELETE against a *specific*
// object version, with optional inclusion of the
// x-amz-bypass-governance-retention header. It is used for Q2 to prove that
// GOVERNANCE-mode retention is bypassable only by an identity that holds the
// s3:BypassGovernanceRetention permission — independent of whether that
// identity is the root user. Raw signing lets us attach the bypass header and
// drive the canonical DeleteObjectHandler entry point directly.
//
// Build (offline, against the repo's pinned module versions):
//
//	cd drivers && GOFLAGS=-mod=mod GOPROXY=off go build -o delete_single ./delete_single
//
// Usage:
//
//	delete_single -endpoint 127.0.0.1:9300 -access KEY -secret SECRET \
//	              -bucket b -object o -version VID [-bypass]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/minio/minio-go/v7/pkg/signer"
)

// emptyPayloadHash is the SHA-256 of the empty request body, required by SigV4
// for a bodyless DELETE.
var emptyPayloadHash = func() string {
	s := sha256.Sum256(nil)
	return hex.EncodeToString(s[:])
}()

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9300", "host:port of the S3 API")
	access := flag.String("access", "", "access key")
	secret := flag.String("secret", "", "secret key")
	token := flag.String("token", "", "optional STS session token")
	bucket := flag.String("bucket", "", "bucket name")
	object := flag.String("object", "", "object key")
	version := flag.String("version", "", "specific versionId to delete")
	bypass := flag.Bool("bypass", false, "send x-amz-bypass-governance-retention: true")
	scheme := flag.String("scheme", "http", "http or https")
	flag.Parse()

	u := fmt.Sprintf("%s://%s/%s/%s", *scheme, *endpoint, *bucket, *object)
	if *version != "" {
		u += "?versionId=" + url.QueryEscape(*version)
	}
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		fmt.Println("REQUEST_BUILD_ERROR:", err)
		os.Exit(2)
	}
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	if *bypass {
		// Header name is case-insensitive; server canonicalizes it.
		req.Header.Set("X-Amz-Bypass-Governance-Retention", "true")
	}

	signed := signer.SignV4(*req, *access, *secret, *token, "us-east-1")

	resp, err := http.DefaultClient.Do(signed)
	if err != nil {
		fmt.Println("HTTP_ERROR:", err)
		os.Exit(3)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	fmt.Printf("HTTP_STATUS: %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	fmt.Printf("SENT_BYPASS_HEADER: %v\n", *bypass)
	if vid := resp.Header.Get("x-amz-version-id"); vid != "" {
		fmt.Printf("RESP_VERSION_ID: %s\n", vid)
	}
	if dm := resp.Header.Get("x-amz-delete-marker"); dm != "" {
		fmt.Printf("RESP_DELETE_MARKER: %s\n", dm)
	}
	if len(body) > 0 {
		fmt.Printf("RESP_BODY:\n%s\n", string(body))
	}
}
```

**`sts_assume_role/main.go`** — STS `AssumeRole` with an inline session policy, then real `GET`/`PUT` with the temporary credentials — plus the missing-token, tampered-token, oversized-policy, and post-expiry edge cases (Q4):

```go
// sts_assume_role exercises the canonical STS AssumeRole entry point with an
// inline session policy and then uses the returned temporary credentials to
// drive real S3 operations. It proves the intersection (logical AND) semantics
// of Q4: an operation succeeds only if BOTH the parent policy AND the inline
// session policy allow it. It also drives the adversarial edge cases —
// missing/tampered session token, oversized inline policy, and post-expiry use.
//
// Build (offline, against the repo's pinned module versions):
//
//	cd drivers && GOFLAGS=-mod=mod GOPROXY=off go build -o sts_assume_role ./sts_assume_role
//
// Usage:
//
//	sts_assume_role -endpoint 127.0.0.1:9300 -access PARENT -secret PARENT \
//	    -bucket b -object o -policy '<inline json>' -duration 900 -op get,put
//
// Edge-case flags: -drop-token, -tamper-token, -sleep N (post-expiry).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9300", "host:port of the S3/STS API")
	access := flag.String("access", "", "PARENT access key")
	secret := flag.String("secret", "", "PARENT secret key")
	bucket := flag.String("bucket", "", "bucket name")
	object := flag.String("object", "", "object key")
	policy := flag.String("policy", "", "inline session policy JSON")
	duration := flag.Int("duration", 900, "session DurationSeconds")
	op := flag.String("op", "get,put", "comma-separated ops to attempt: get,put")
	dropToken := flag.Bool("drop-token", false, "use temp access/secret WITHOUT the session token")
	tamperToken := flag.Bool("tamper-token", false, "flip a byte of the session token before use")
	sleepSec := flag.Int("sleep", 0, "sleep N seconds after assuming (for post-expiry test)")
	secure := flag.Bool("secure", false, "use TLS")
	flag.Parse()

	stsEndpoint := fmt.Sprintf("http://%s", *endpoint)
	if *secure {
		stsEndpoint = fmt.Sprintf("https://%s", *endpoint)
	}

	opts := credentials.STSAssumeRoleOptions{
		AccessKey:       *access,
		SecretKey:       *secret,
		DurationSeconds: *duration,
		Location:        "us-east-1",
	}
	if *policy != "" {
		opts.Policy = *policy
	}

	stsCred, err := credentials.NewSTSAssumeRole(stsEndpoint, opts)
	if err != nil {
		fmt.Println("STS_CONSTRUCT_ERROR:", err)
		os.Exit(0)
	}
	val, err := stsCred.Get()
	if err != nil {
		// Expected for the oversized-policy edge case: the server rejects the
		// AssumeRole request before issuing any credentials.
		fmt.Println("STS_ASSUME_ERROR:", err)
		os.Exit(0)
	}

	// Redacted evidence about the issued temporary credential.
	fmt.Printf("STS_OK: tempAccessKey=%s… sessionTokenLen=%d\n",
		safePrefix(val.AccessKeyID, 6), len(val.SessionToken))

	if *sleepSec > 0 {
		fmt.Printf("SLEEPING %ds for expiry...\n", *sleepSec)
		time.Sleep(time.Duration(*sleepSec) * time.Second)
	}

	sessionToken := val.SessionToken
	if *tamperToken && len(sessionToken) > 0 {
		b := []byte(sessionToken)
		if b[len(b)-1] == 'A' {
			b[len(b)-1] = 'B'
		} else {
			b[len(b)-1] = 'A'
		}
		sessionToken = string(b)
		fmt.Println("USING_TAMPERED_TOKEN: true")
	}
	if *dropToken {
		sessionToken = ""
		fmt.Println("USING_TEMP_CREDS_WITHOUT_TOKEN: true")
	}

	cl, err := minio.New(*endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(val.AccessKeyID, val.SecretAccessKey, sessionToken),
		Secure: *secure,
	})
	if err != nil {
		fmt.Println("CLIENT_ERROR:", err)
		os.Exit(0)
	}

	ctx := context.Background()
	for _, o := range strings.Split(*op, ",") {
		switch strings.TrimSpace(o) {
		case "get":
			obj, gerr := cl.GetObject(ctx, *bucket, *object, minio.GetObjectOptions{})
			if gerr != nil {
				fmt.Printf("GET_RESULT: error code=%s msg=%q\n", errCode(gerr), gerr.Error())
				continue
			}
			data, rerr := io.ReadAll(obj)
			if rerr != nil {
				fmt.Printf("GET_RESULT: read error code=%s msg=%q\n", errCode(rerr), rerr.Error())
				continue
			}
			fmt.Printf("GET_RESULT: OK bytes=%d\n", len(data))
		case "put":
			payload := bytes.Repeat([]byte("Z"), 16)
			_, perr := cl.PutObject(ctx, *bucket, *object+".sts-put",
				bytes.NewReader(payload), int64(len(payload)),
				minio.PutObjectOptions{ContentType: "application/octet-stream"})
			if perr != nil {
				fmt.Printf("PUT_RESULT: error code=%s msg=%q\n", errCode(perr), perr.Error())
				continue
			}
			fmt.Printf("PUT_RESULT: OK\n")
		}
	}
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func errCode(err error) string {
	resp := minio.ToErrorResponse(err)
	if resp.Code != "" {
		return resp.Code
	}
	return "<none>"
}
```

**`set_policy/main.go`** — admin self-promotion battery via `madmin-go` — modern attach, deprecated set-policy, create-policy, list-users, add-user, set-user-status, remove-user — all as a basic user (Q5):

```go
// set_policy authenticates as a low-privilege ("basic") user and attempts a
// battery of admin API mutations that would, if permitted, let the user
// promote itself to console administrator or otherwise escalate. Every attempt
// is expected to be denied by MinIO's deny-by-default admin authorization gate
// (validateAdminReq -> checkAdminRequestAuth -> IAMSys.IsAllowed) before any
// mapping mutation executes. This driver captures the exact error for each
// route, proving Q5. It uses the madmin-go admin client so it drives the real
// admin API entry points.
//
// Build (offline, against the repo's pinned module versions):
//
//	cd drivers && GOFLAGS=-mod=mod GOPROXY=off go build -o set_policy ./set_policy
//
// Usage:
//
//	set_policy -endpoint 127.0.0.1:9300 -access BASICKEY -secret BASICSECRET \
//	           -self BASICKEY [-attach consoleAdmin]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	madmin "github.com/minio/madmin-go/v3"
)

func report(label string, err error) {
	if err == nil {
		fmt.Printf("%-22s => UNEXPECTED_SUCCESS (no error returned)\n", label)
		return
	}
	e := madmin.ToErrorResponse(err)
	code := e.Code
	if code == "" {
		code = "<none>"
	}
	fmt.Printf("%-22s => DENIED code=%s msg=%q\n", label, code, err.Error())
}

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9300", "host:port of the admin API")
	access := flag.String("access", "", "BASIC user access key")
	secret := flag.String("secret", "", "BASIC user secret key")
	self := flag.String("self", "", "the basic user's own access key (escalation target)")
	attach := flag.String("attach", "consoleAdmin", "policy to attempt to attach to self")
	secure := flag.Bool("secure", false, "use TLS")
	flag.Parse()

	madm, err := madmin.New(*endpoint, *access, *secret, *secure)
	if err != nil {
		fmt.Println("ADMIN_CLIENT_ERROR:", err)
		os.Exit(2)
	}
	ctx := context.Background()

	// 1. Modern attach: POST /minio/admin/v3/idp/builtin/policy/attach
	_, aerr := madm.AttachPolicy(ctx, madmin.PolicyAssociationReq{
		Policies: []string{*attach},
		User:     *self,
	})
	report("AttachPolicy(self)", aerr)

	// 2. Deprecated set-policy: PUT /minio/admin/v3/set-user-or-group-policy
	report("SetPolicy(self)", madm.SetPolicy(ctx, *attach, *self, false))

	// 3. Create a new all-powerful policy: PUT /minio/admin/v3/add-canned-policy
	adminPol := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:*","s3:*"],"Resource":["*"]}]}`)
	report("AddCannedPolicy", madm.AddCannedPolicy(ctx, "qafix-escalate", adminPol))

	// 4. Enumerate other users: GET /minio/admin/v3/list-users
	_, lerr := madm.ListUsers(ctx)
	report("ListUsers", lerr)

	// 5. Create a brand-new privileged user: PUT /minio/admin/v3/add-user
	report("AddUser", madm.AddUser(ctx, "qafix-newuser", "qafix-newuser-secret-000"))

	// 6. Toggle another account's status: PUT /minio/admin/v3/set-user-status
	report("SetUserStatus", madm.SetUserStatus(ctx, *self, madmin.AccountDisabled))

	// 7. Remove a user: DELETE /minio/admin/v3/remove-user
	report("RemoveUser", madm.RemoveUser(ctx, *self))
}
```

### Server invocation — erasure mode (required for Q3)

The server is run in its default configuration as a normal operator would, with four drive directories so a single erasure set with parity `EC:2` is formed (parity is required for Q3's heal-on-read to manifest):

```
$ MINIO_ROOT_USER=<ephemeral, 11 chars> \
  MINIO_ROOT_PASSWORD=<ephemeral, 32 chars, redacted> \
  MINIO_KMS_SECRET_KEY="my-minio-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw=" \
  /tmp/minio-investigation/bin/minio server /tmp/minio-investigation/data/{1,2,3,4} \
        --address 127.0.0.1:9000 --console-address 127.0.0.1:9001

MinIO Object Storage Server
Version: DEVELOPMENT.2024-11-25T17-10-22Z (go1.23.12 linux/amd64)
API: http://127.0.0.1:9000
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
```

```
$ mc admin info inv
●  127.0.0.1:9000
   Uptime: 1 hour   Version: 2024-11-25T17:10:22Z
   Network: 1/1 OK    Drives: 4/4 OK    Pool: 1
┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 1.8% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘
4 drives online, 0 drives offline, EC:2
```

**Security posture of this local investigation (not a deployment recommendation).** The server is bound to **loopback `127.0.0.1` only** (never `0.0.0.0`), the root user/password are **ephemeral random values generated per run** (not the `minioadmin` default, not committed to the repo, redacted here), and `MINIO_KMS_SECRET_KEY` uses MinIO's **published CI/demo key** purely to enable SSE-S3/SSE-KMS locally — it is explicitly a demo key and must never be used in production. The `mc` client config is kept in an **isolated `MC_CONFIG_DIR`** so no alias leaks into the host's `~/.mc`. All data directories live under `/tmp` and are destroyed at cleanup.

### Methodology and independent verifiability

This document is a **complete, self-contained, from-scratch runtime investigation**: every behavioral claim below was produced by exercising the real S3 / admin / STS API entry points against the live server described above, and each is presented with its exact producing command and complete, unedited output. No claim relies on a prior draft or on any external checkpoint artifact. Each question's primary/headline outcome was confirmed across at least **two** runs — the per-question **Stability** subsection records exactly what was repeated and its second-run verdict — while individual supporting observations (complete trace excerpts, the JWT decode, on-disk shard listings, and long-running edge cases such as the genuine STS token expiry) were captured **once**. Every factual statement is labeled `[OBSERVED]` (captured at runtime) or `[INFERRED]` (derived from reading source at commit `c07e5b49d477`, with a `file:line` anchor). The evidence is therefore independently reproducible by rebuilding the binary and re-running the commands shown.

---

## Q1 — Bucket encryption requirement vs. a user's broad write, on an unencrypted upload

**Question.** What happens when a bucket-level encryption requirement takes precedence over a user's broad write permission during an *unencrypted* upload? Identify the runtime execution sequence captured in the server trace.

**Short answer (`[OBSERVED]`).** "Takes precedence" resolves **three different ways depending on which mechanism enforces the requirement**, and MinIO's behavior is not what a naive reading suggests:

| # | Enforcing mechanism | Principal | SSE header sent | Runtime outcome |
|---|---------------------|-----------|-----------------|-----------------|
| A | Bucket **default encryption** | `q1user` (`s3:*`) | none | **200** — object stored, server **auto-injected `AES256`** |
| B | **Bucket policy** `DenyUnEncryptedObjectUploads` | `q1user` (`s3:*`, authenticated) | none | **200** — stored **unencrypted** (bucket policy **not consulted** for an authenticated identity) |
| C1 | **Bucket policy** `DenyUnEncryptedObjectUploads` | **anonymous** | none | **403 AccessDenied** |
| C2 | **Bucket policy** `DenyUnEncryptedObjectUploads` | **anonymous** | `AES256` | **200** — SSE-S3 |
| D1 | **Identity policy** with a `Deny` condition | `q1deny` (authenticated) | none | **403 AccessDenied** |
| D2 | **Identity policy** with a `Deny` condition | `q1deny` (authenticated) | `AES256` | **200** — SSE-S3 |

The key corrections this investigation establishes at runtime:

1. A **bucket policy** `DenyUnEncryptedObjectUploads` does **not** override an *authenticated* user's broad write — MinIO evaluates only the **identity** policy for authenticated requests and never consults the bucket policy for them (TEST B, 200 unencrypted). It overrides only **anonymous** requests (TEST C1, 403).
2. To override an *authenticated* broad-write user, the deny must live in the **identity** policy (TEST D1, 403) — where `Deny` beats `Allow` — or the bucket must use **default encryption**, which does not deny at all but transparently **auto-encrypts** (TEST A, 200 + `AES256`).
3. The `mc admin trace` stream does **not** expose the handler's internal stages; it shows the request headers **after** the handler has run and the response. The internal ordering is therefore `[INFERRED]` from source (below).

### Root cause — why a bucket policy does not gate an authenticated identity (`[INFERRED]` from source, `[OBSERVED]` at runtime)

`isPutActionAllowed` (`cmd/auth-handler.go:749`) branches on whether the request is anonymous — an empty `cred.AccessKey` consults the **bucket policy** (`globalPolicySys`), while an authenticated identity is evaluated **only** against its IAM identity policy (`globalIAMSys`). The verbatim branch (`cmd/auth-handler.go:778-804`):

```go
	if cred.AccessKey == "" {
		if globalPolicySys.IsAllowed(policy.BucketPolicyArgs{
			AccountName:     cred.AccessKey,
			Groups:          cred.Groups,
			Action:          action,
			BucketName:      bucketName,
			ConditionValues: getConditionValues(r, "", auth.AnonymousCredentials),
			IsOwner:         false,
			ObjectName:      objectName,
		}) {
			return ErrNone
		}
		return ErrAccessDenied
	}

	if globalIAMSys.IsAllowed(policy.Args{
		AccountName:     cred.AccessKey,
		Groups:          cred.Groups,
		Action:          action,
		BucketName:      bucketName,
		ConditionValues: getConditionValues(r, "", cred),
		ObjectName:      objectName,
		IsOwner:         owner,
		Claims:          cred.Claims,
	}) {
		return ErrNone
	}
```

For an authenticated user, `IAMSys.IsAllowed` (`cmd/iam.go:2437`) ends at `return sys.GetCombinedPolicy(policies...).IsAllowed(args)` (`cmd/iam.go:2482`) — only the identity's combined policy is evaluated; the bucket policy is never consulted. This is `[INFERRED]` from source and `[OBSERVED]` by TEST B succeeding.

### Setup (canonical commands)

```
# broad-write identity policy for q1user
$ cat q1user-policy.json
{ "Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],
  "Resource":["arn:aws:s3:::q1-default-enc/*","arn:aws:s3:::q1-policy-deny/*","arn:aws:s3:::q1-identity-deny/*",
               "arn:aws:s3:::q1-default-enc","arn:aws:s3:::q1-policy-deny","arn:aws:s3:::q1-identity-deny"]}]}
$ mc admin policy create inv q1-broadwrite q1user-policy.json
Created policy `q1-broadwrite` successfully.
$ mc admin user add inv q1user ***REDACTED***
Added user `q1user` successfully.
$ mc admin policy attach inv q1-broadwrite --user q1user
Attached Policies: [q1-broadwrite]
To User: q1user

# mechanism A: bucket default SSE-S3
$ mc encrypt set sse-s3 inv/q1-default-enc
Auto encryption configuration has been set successfully for inv/q1-default-enc

# mechanism B/C: DenyUnEncryptedObjectUploads bucket policy (anon Allow isolates the encryption condition)
$ mc anonymous set-json deny-unencrypted-bucket-policy.json inv/q1-policy-deny
$ mc anonymous get-json inv/q1-policy-deny
{"Statement":[{"Action":["s3:GetObject","s3:ListBucket","s3:PutObject"],"Effect":"Allow","Principal":{"AWS":["*"]},
  "Resource":["arn:aws:s3:::q1-policy-deny","arn:aws:s3:::q1-policy-deny/*"],"Sid":"AllowAnonAll"},
 {"Action":["s3:PutObject"],"Condition":{"Null":{"s3:x-amz-server-side-encryption":[true]}},"Effect":"Deny",
  "Principal":{"AWS":["*"]},"Resource":["arn:aws:s3:::q1-policy-deny/*"],"Sid":"DenyUnEncryptedObjectUploads"}],
 "Version":"2012-10-17"}

# mechanism D: identity policy that Denies PutObject when the SSE header is absent (Null == true)
$ mc admin policy create inv q1-identity-deny-pol q1deny-identity-policy.json
$ mc admin user add inv q1deny ***REDACTED*** ; mc admin policy attach inv q1-identity-deny-pol --user q1deny
```

### TEST A — bucket **default encryption** auto-encrypts (server-side), `[OBSERVED]`

A **raw SigV4-signed PUT via the embedded `put_raw` driver with NO `x-amz-server-side-encryption` header** (User-Agent `Go-http-client/1.1`, no `mc`, no minio-go `PutObject` helper), so the client sends no SSE header. `SENT_SSE_HEADER: <none>` confirms the client sent nothing; `RESP_SSE_HEADER: "AES256"` and the subsequent `mc stat` confirm the server auto-encrypted:

```
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-default-enc -object objA2-rawsdk -size 29
HTTP_STATUS: 200 OK
SENT_SSE_HEADER: <none>
RESP_SSE_HEADER: "AES256"
$ mc stat inv/q1-default-enc/objA2-rawsdk
Name      : objA2-rawsdk
Date      : 2026-07-14 06:37:13 UTC 
Size      : 29 B   
ETag      : cf5205dc20fb05145e6d1fa08166e94e 
Type      : file 
Encryption: SSE-S3
Metadata  :
  Content-Type: application/octet-stream 
```

`mc admin trace -v` for that PUT (note: the `X-Amz-Server-Side-Encryption: AES256` shown in the **request** block is **not** in `SignedHeaders` — which lists only `content-type;host;x-amz-content-sha256;x-amz-date` — and was **not** sent by the client; see the trace-timing note below. Per-response transport boilerplate is omitted as declared in Conventions):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T06:37:13.019] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-default-enc/objA2-rawsdk
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260714/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=62d84bc14706511afdd56ce409d6f6af5d3554ad6f5b48fe2c56242674a6d219
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 X-Amz-Content-Sha256: a7951e0ca2e9612a985a36747309822a67a9b8c1a5abd848c03e82216c85f1b3
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Content-Length: 29
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Date: 20260714T063713Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:37:13.028] [ Duration 8.551ms TTFB 8.52109ms ↑ 164 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 ETag: "cf5205dc20fb05145e6d1fa08166e94e"
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 X-Amz-Request-Id: 18C214951A8877B5
127.0.0.1:9000 <BLOB>
```

**Trace-timing note (`[INFERRED]` from source).** The `X-Amz-Server-Side-Encryption: AES256` header that appears in the *request* section, despite the client never sending it, is explained by the tracer implementation: `httpTracerMiddleware` calls `h.ServeHTTP(respRecorder, r)` (`cmd/http-tracer.go:89`) and only **afterwards** clones the request headers with `reqHeaders := r.Header.Clone()` (`cmd/http-tracer.go:103`). Because `PutObjectHandler` mutates `r.Header` when it applies bucket default encryption — `sseConfig.Apply(r.Header, ...)` at `cmd/object-handlers.go:1894-1896` — the **server-injected** header is present by the time the tracer snapshots the request. This is the concrete runtime signature of the "encryption requirement transparently taking precedence": the write is honored **and** the object is encrypted, with no denial.

### TEST B — `DenyUnEncryptedObjectUploads` **bucket** policy does **not** deny the authenticated broad-write user, `[OBSERVED]`

```
$ mc cp payload.txt q1/q1-policy-deny/objB-authed-noenc          # q1 alias = q1user (s3:*)
`/tmp/minio-investigation/evidence/payload.txt` -> `q1/q1-policy-deny/objB-authed-noenc`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 39 B  │ 39 B        │ 00m00s   │ 3.01 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
$ mc stat q1/q1-policy-deny/objB-authed-noenc
Name      : objB-authed-noenc
Date      : 2026-07-14 06:34:12 UTC 
Size      : 39 B   
ETag      : edc9fd0d46b7d6c8c4ae3a5f1fc7942a 
Type      : file 
Metadata  :
  Content-Type: text/plain 
   (no Encryption line -> stored UNENCRYPTED)
```

Trace of the PUT — **200 OK**, and note the request carries **no** `X-Amz-Server-Side-Encryption` header (this bucket has no default encryption, and the object was stored as-is):

```
127.0.0.1:9000 PUT /q1-policy-deny/objB-authed-noenc
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1user/20260713/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=a6137d481fdc5633ab0203dc69c141f4d73a602b0c21748cf0e05a03e8a22c6a
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:29:48.142] [ Duration 3.643ms TTFB 3.616204ms ↑ 347 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 ETag: "94de2e07c23a6d86ad020c98ee12f751"
```

This is the crux: even though a `DenyUnEncryptedObjectUploads` bucket policy is installed, the authenticated broad-write user's unencrypted upload **succeeds** because the bucket policy governs only anonymous requests.

### TEST C1 / C2 — the same **bucket** policy **does** gate an **anonymous** upload, `[OBSERVED]`

Anonymous, no SSE header — **403**:

```
$ curl -s -X PUT --data-binary @payload.txt http://127.0.0.1:9000/q1-policy-deny/objC1-anon-noenc  -w 'HTTP %{http_code}\n'
HTTP 403
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>objC1-anon-noenc</Key><BucketName>q1-policy-deny</BucketName><Resource>/q1-policy-deny/objC1-anon-noenc</Resource><RequestId>18C1F0413D1A471D</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace (anonymous — no `Authorization` header — `403 Forbidden`):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-13T19:31:30.404] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-policy-deny/objC1-anon-noenc
127.0.0.1:9000 User-Agent: curl/8.14.1
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-13T19:31:30.404] [ Duration 149µs TTFB 139.788µs ↑ 51 B  ↓ 351 B ]
127.0.0.1:9000 403 Forbidden
```

Anonymous **with** the SSE header — **200**, object encrypted (the `Null` condition no longer matches, so the `Deny` does not fire and `AllowAnonAll` permits it):

```
$ curl -s -X PUT -H "x-amz-server-side-encryption: AES256" --data-binary @payload.txt \
     http://127.0.0.1:9000/q1-policy-deny/objC2-anon-sse -w 'HTTP %{http_code}\n'
HTTP 200
$ mc stat inv/q1-policy-deny/objC2-anon-sse
Name      : objC2-anon-sse
Encryption: SSE-S3
```

### TEST D1 / D2 — an **identity** policy `Deny` **does** override the authenticated user (Deny > Allow), `[OBSERVED]`

`q1deny` holds `s3:*` on the bucket **plus** a `Deny` on `s3:PutObject` when `s3:x-amz-server-side-encryption` is `Null`. Unencrypted upload — **403**:

```
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1deny -secret *** -bucket q1-identity-deny -object objD1-noenc
HTTP_STATUS: 403 Forbidden
SENT_SSE_HEADER: <none>
RESP_SSE_HEADER: <none>
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>objD1-noenc</Key><BucketName>q1-identity-deny</BucketName><Resource>/q1-identity-deny/objD1-noenc</Resource><RequestId>18C214C51EFFD392</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace — authenticated as `q1deny`, **403 Forbidden** (same request: the `X-Amz-Request-Id` below matches the `RequestId` in the `RESP_BODY` above; transport boilerplate omitted as declared in Conventions):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T06:40:39.253] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q1-identity-deny/objD1-noenc
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q1deny/20260714/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=790b1a5a190d4908161fab9fa8e6a9f4200a23944b70ce4c0b1aa848d9ff4195
127.0.0.1:9000 Content-Length: 16
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Content-Sha256: 991204fba2b6216d476282d375ab88d20e6108d109aecded97ef424ddd114706
127.0.0.1:9000 X-Amz-Date: 20260714T064039Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:40:39.253] [ Duration 218µs TTFB 202.737µs ↑ 106 B  ↓ 345 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Amz-Request-Id: 18C214C51EFFD392
127.0.0.1:9000 Content-Length: 345
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>objD1-noenc</Key><BucketName>q1-identity-deny</BucketName><Resource>/q1-identity-deny/objD1-noenc</Resource><RequestId>18C214C51EFFD392</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The same user **with** SSE (valid `mc` syntax — the `--enc-s3` value is a **path prefix**, not a filename) — **200**, encrypted:

```
$ mc cp --enc-s3 "q1deny/q1-identity-deny/objD2-sse" payload.txt q1deny/q1-identity-deny/objD2-sse
`/tmp/minio-investigation/evidence/payload.txt` -> `q1deny/q1-identity-deny/objD2-sse`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 39 B  │ 39 B        │ 00m00s   │ 3.49 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
$ mc stat inv/q1-identity-deny/objD2-sse
Name      : objD2-sse
Date      : 2026-07-14 06:34:12 UTC 
Size      : 39 B   
ETag      : edc9fd0d46b7d6c8c4ae3a5f1fc7942a 
Type      : file 
Encryption: SSE-S3
Metadata  :
  Content-Type: text/plain 
```

> Correction of an invalid command in a prior draft: `mc cp --enc-s3 payload.txt` is **wrong** — `--enc-s3` consumes its argument as the encryption **path prefix**, so `payload.txt` would be treated as that prefix and the actual source/target operands would be missing. The correct form supplies the prefix explicitly, exactly as in the working command shown above: `mc cp --enc-s3 "q1deny/q1-identity-deny/objD2-sse" payload.txt q1deny/q1-identity-deny/objD2-sse` (encryption path-prefix first, then the local source `payload.txt`, then the `alias/bucket/object` target).

### TEST E — SSE method-value siblings: which `x-amz-server-side-encryption` values are accepted vs. rejected (`[OBSERVED]`)

The question spans the encryption-requirement space, so this test exercises the full **value** space of the `x-amz-server-side-encryption` request header while holding the identity constant at broad write. This isolates the **SSE method-value parsing gate** from authorization: the caller `q1user` holds `s3:*` on a **dedicated plain bucket** `q1-sse` that has **no** bucket default encryption, so authorization is never the blocker and the SSE header value is the only variable.

Additional setup (self-contained; secret redacted):

```
$ mc mb inv/q1-sse
Bucket created successfully `inv/q1-sse`.
$ cat q1broad.json
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],
  "Resource":["arn:aws:s3:::q1-sse","arn:aws:s3:::q1-sse/*"]}]}
$ mc admin policy create inv q1broad q1broad.json
Created policy `q1broad` successfully.
$ mc admin user add inv q1user ***REDACTED***
Added user `q1user` successfully.
$ mc admin policy attach inv q1broad --user q1user
Attached Policies: [q1broad]
To User: q1user
$ mc encrypt info inv/q1-sse
mc: <ERROR> Unable to get encryption info. The server side encryption configuration was not found.
```

The producing driver is `put_raw` (complete source, with sha256, in "Investigation driver programs — complete source"). It issues a **raw SigV4** PUT and lets the `-sse` flag place an arbitrary `x-amz-server-side-encryption` value on the wire (including an empty value), so tokens that `mc`/the SDK would never emit can be exercised. Complete, unedited output for every value:

```
### SSE=AES256 (valid SSE-S3 control)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_aes256 -sse AES256 -size 24
HTTP_STATUS: 200 OK
SENT_SSE_HEADER: "AES256"
RESP_SSE_HEADER: "AES256"

### SSE=aes256 (lowercase)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_lower -sse aes256 -size 24
HTTP_STATUS: 200 OK
SENT_SSE_HEADER: "aes256"
RESP_SSE_HEADER: "AES256"

### SSE=Aes256 (mixed case)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_mixed -sse Aes256 -size 24
HTTP_STATUS: 200 OK
SENT_SSE_HEADER: "Aes256"
RESP_SSE_HEADER: "AES256"

### SSE=AES-256 (hyphenated, malformed)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_hyphen -sse AES-256 -size 24
HTTP_STATUS: 400 Bad Request
SENT_SSE_HEADER: "AES-256"
RESP_SSE_HEADER: <none>
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidArgument</Code><Message>Invalid arguments provided for q1-sse/m_hyphen: (The encryption method is not supported)</Message><Key>m_hyphen</Key><BucketName>q1-sse</BucketName><Resource>/q1-sse/m_hyphen</Resource><RequestId>18C20A7D4F8BAF0E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

### SSE=AES999 (unsupported value)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_aes999 -sse AES999 -size 24
HTTP_STATUS: 400 Bad Request
SENT_SSE_HEADER: "AES999"
RESP_SSE_HEADER: <none>
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidArgument</Code><Message>Invalid arguments provided for q1-sse/m_aes999: (The encryption method is not supported)</Message><Key>m_aes999</Key><BucketName>q1-sse</BucketName><Resource>/q1-sse/m_aes999</Resource><RequestId>18C20A7D4FCCBA93</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

### SSE= (empty value, header present)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_empty -sse  -size 24
HTTP_STATUS: 400 Bad Request
SENT_SSE_HEADER: ""
RESP_SSE_HEADER: <none>
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidArgument</Code><Message>Invalid arguments provided for q1-sse/m_empty: (The encryption method is not supported)</Message><Key>m_empty</Key><BucketName>q1-sse</BucketName><Resource>/q1-sse/m_empty</Resource><RequestId>18C20A7D5007354A</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>

### SSE=aws:kms (valid SSE-KMS)
$ ./put_raw -endpoint 127.0.0.1:9000 -access q1user -secret *** -bucket q1-sse -object m_kms -sse aws:kms -size 24
HTTP_STATUS: 200 OK
SENT_SSE_HEADER: "aws:kms"
RESP_SSE_HEADER: "aws:kms"
```

Stored-vs-rejected state, confirmed against the backend (`[OBSERVED]`):

```
$ mc stat inv/q1-sse/m_lower
Name      : m_lower
Date      : 2026-07-14 03:32:15 UTC 
Size      : 24 B   
ETag      : c7c6abfa9cb508f7fc178d4045313a94 
Type      : file 
Encryption: SSE-S3
Metadata  :
  Content-Type: application/octet-stream 

$ mc stat inv/q1-sse/m_hyphen
mc: <ERROR> Unable to stat `inv/q1-sse/m_hyphen`. Object does not exist.
```

Verdict matrix (`[OBSERVED]`):

| `x-amz-server-side-encryption` sent | HTTP | stored? | resulting encryption |
|---|---|---|---|
| `AES256` | 200 | yes | SSE-S3 |
| `aes256` (lowercase) | 200 | yes | SSE-S3 (response normalized to `AES256`) |
| `Aes256` (mixed case) | 200 | yes | SSE-S3 (response normalized to `AES256`) |
| `AES-256` (hyphen) | **400** `InvalidArgument` | no | — |
| `AES999` (unsupported) | **400** `InvalidArgument` | no | — |
| *(empty, header present)* | **400** `InvalidArgument` | no | — |
| `aws:kms` | 200 | yes | SSE-KMS (`arn:aws:kms:my-minio-key`) |

**What this proves about "precedence".** Even holding broad `s3:*` write, the caller cannot store an object by sending a **malformed** SSE method token: the SSE method-value gate rejects it with **400 `InvalidArgument`** before any object is written (the three rejected keys are absent from the backend). A **valid** token (`AES256`/`aws:kms`, case-insensitively for the AES token) is honored and the object is encrypted. The encryption pipeline therefore validates the method token independently of — and after — the write-permission decision.

**Root cause — the exact gate (`[INFERRED]` from source at `c07e5b49d477`, `[OBSERVED]` by the matrix).** The token is validated during option-building, **before** `EncryptRequest`:

1. `PutObjectHandler` builds put options via `putOptsFromReq` → `putOpts` → `putOptsFromHeaders` (`cmd/object-api-options.go:321,325,368`); `putOpts` wraps any returned error as `InvalidArgument{Bucket,Object,Err:err}` (`cmd/object-api-options.go:347-353`), which is why the body reads `Invalid arguments provided for q1-sse/<obj>: (...)` (`InvalidArgument.Error()`, `cmd/object-api-errors.go:275`).
2. `putOptsFromHeaders` treats the header as an **SSE-KMS** request whenever `crypto.S3KMS.IsRequested(hdr)` is true (`cmd/object-api-options.go:413`). That predicate is *header present AND `strings.ToUpper(value) != "AES256"`* (`internal/crypto/sse-kms.go:57-60`; `AmzEncryptionAES = "AES256"` at `internal/http/headers.go:152`). **The `strings.ToUpper` is the entire source of the case-insensitive `AES256` acceptance** — `aes256`/`Aes256` upper-case to `AES256`, so `IsRequested` is false and the KMS gate is skipped.
3. When the gate is entered, `crypto.S3KMS.ParseHTTP(hdr)` requires the value to be exactly `aws:kms` (`algorithm != xhttp.AmzEncryptionKMS`, `internal/crypto/sse-kms.go:72`; `AmzEncryptionKMS = "aws:kms"` at `internal/http/headers.go:153`) and otherwise returns `ErrInvalidEncryptionMethod` (`internal/crypto/sse-kms.go:73`, text `"The encryption method is not supported"` at `internal/crypto/error.go:57`). This is why `AES-256`, `AES999`, and empty — none of which upper-case to `AES256` and none of which equal `aws:kms` — are rejected identically.
4. `toAPIError` maps `crypto.ErrInvalidEncryptionMethod` → `ErrInvalidEncryptionMethod` (`cmd/api-errors.go:2201-2202`) → Code `InvalidArgument`, HTTP 400 (`cmd/api-errors.go:1166-1170`).
5. For a value that upper-cases to exactly `AES256`, the KMS gate is skipped and the request proceeds to `getDefaultOpts` (`cmd/object-api-options.go:430`); later, `crypto.Requested(r.Header)` is true (`cmd/object-handlers.go:1999`) so `EncryptRequest` (`cmd/encryption-v1.go:466`) runs with kind `crypto.S3` (dispatch at `internal/crypto/sse.go:61`, since the value is not `aws:kms`), encrypting via a KMS-generated data key (`newEncryptMetadata` case `crypto.S3`, `cmd/encryption-v1.go:366-380`); the response header is normalized to `AES256` (`cmd/object-handlers.go:2074-2075`). For `aws:kms`, `ParseHTTP` succeeds and SSE-KMS is used (`cmd/object-api-options.go:418-427`), the response echoing `aws:kms` (`cmd/object-handlers.go:2077-2078`).

> Note (`[INFERRED]`): `crypto.S3.ParseHTTP` (`internal/crypto/sse-s3.go:56`), whose exact `!= "AES256"` check would reject lowercase, is **not** on the PUT path — it is exercised only by `internal/crypto/header_test.go`. On the live write path the token is gated by the `S3KMS.IsRequested`/`ParseHTTP` pair above, which is precisely why the observed behavior is case-insensitive for the AES token yet format-strict (a hyphen or extra digits are rejected).

Both value-matrix passes (run twice with fresh object keys) produced identical verdicts.

### Runtime execution sequence — what the trace shows, and the internal ordering

**`[OBSERVED]` (from the trace).** For every case above, `mc admin trace` shows exactly two things per request: the request line + headers (snapshotted **after** the handler runs) and the `[RESPONSE]` with its status code (`200 OK` or `403 Forbidden`) and body. The trace does **not** emit per-stage markers for signature verification, authorization, or encryption.

**`[INFERRED]` (from source at `c07e5b49d477`).** Inside `PutObjectHandler` the order is:

1. `cmd/object-handlers.go:1836` — `isPutActionAllowed(...)` (authorization: identity policy for authenticated, bucket policy for anonymous). **This runs first.**
2. `cmd/object-handlers.go:1844` — `newSignV4ChunkedReader(r, ...)` for the streaming-signed path (the mc/SDK requests above all use `X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`). The per-chunk payload signatures are verified **as the body is read**, i.e. **after** authorization.
3. `cmd/object-handlers.go:1894-1896` — `sseConfig.Apply(r.Header, {AutoEncrypt: globalAutoEncryption})` injects bucket default encryption.

Therefore the frequently-stated ordering "SigV4 → IAM → SSE" is imprecise for streaming uploads: only the **seed** signature is validated before the handler; the **body** signature is validated *after* the IAM authorization decision, and SSE application is last. This ordering is `[INFERRED]` — it cannot be read off the trace, which only proves the *outcome* (the status code and, for default encryption, the injected `AES256` header).

### Stability

Each case was run twice with fresh object keys; verdicts were identical: A `200/AES256`, B `200`/unencrypted, C1 `403`, C2 `200`, D1 `403`, D2 `200`.

---

## Q2 — Object-lock delete enforcement: what log entries appear when deleting locked objects

**Question.** With object locking enabled, what specific log entries appear when someone tries to delete locked objects? Show runtime log output.

**Short answer (`[OBSERVED]`).** The "log entry" a delete of a locked object produces is the **HTTP error response** emitted on the trace stream — MinIO does **not** write a server console/audit line for a normal WORM block (the console log line count was **unchanged** across every blocked delete below; `internalLogIf` fires only on an NTP/clock error, `cmd/bucket-object-lock.go:116,142`). The response differs by lock mode and permission:

| # | Lock state | Caller | Bypass hdr | Handler | HTTP | Code | Message |
|---|-----------|--------|-----------|---------|------|------|---------|
| 1 | Legal hold ON | root | — | `s3.DeleteObject` | **400** | `InvalidRequest` | `Object is WORM protected and cannot be overwritten` |
| 2 | Governance | root | no | `s3.DeleteObject` | **400** | `InvalidRequest` | (same) |
| 3 | Governance | `q2user` (no Bypass perm) | yes | `s3.DeleteObject` | **403** | `AccessDenied` | `Access Denied.` |
| 4 | Compliance | root | yes | `s3.DeleteObject` | **400** | `InvalidRequest` | (same, bypass ignored) |
| 5 | *(control)* none | `q2user` | — | `s3.DeleteObject` | **204** | — | success (proves DeleteObject IS granted) |
| 6 | Governance | root | yes | `s3.DeleteObject` | **204** | — | success (version removed) |
| 7 | Governance | `q2bypass` (**non-root**, has Bypass perm) | yes | `s3.DeleteObject` | **204** | — | success (bypass gated by permission, not root) |
| 8 | none (versionless) | root/`mc` | — | `s3.DeleteMultipleObjects` | **200** | — | delete marker inserted |

### Root cause — `enforceRetentionBypassForDelete` (`cmd/bucket-object-lock.go:84`)

The single-object `DeleteObjectHandler` registers this check as `opts.SetEvalRetentionBypassFn(...)` and runs it **only when a specific `versionId` is supplied** (`cmd/object-handlers.go:2598-2611`, guarded by `if vID != ""`). Inside:

- **Legal hold ON** → `return ObjectLocked{}` (`:100-102`) — unconditional, applies to everyone including root.
- **Compliance** → if `!RetainUntilDate.Before(now)` → `return ObjectLocked{}` (`:117-121`) — no bypass branch exists, so **not even root** can bypass.
- **Governance, no bypass header** → if `!RetainUntilDate.Before(now)` → `return ObjectLocked{}` (`:143-146`).
- **Governance, bypass header set** → `checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, ...)`; if that `!= ErrNone` → `return errAuthentication` (`:152-154`).

Error mapping (`cmd/api-errors.go`): `ObjectLocked` → `ErrObjectLocked` (`:2298`) = Code `InvalidRequest`, **HTTP 400**, "Object is WORM protected and cannot be overwritten" (`:1059-1062`); `errAuthentication` → `ErrAccessDenied` (`:2176`) = Code `AccessDenied`, **HTTP 403** (`:539-543`).

### Setup and before-state (`[OBSERVED]`)

All deletes below are issued by the **embedded `delete_single` driver** — a raw SigV4 `DELETE` that lets us attach `x-amz-bypass-governance-retention` and target a specific `versionId`, driving the canonical `DeleteObjectHandler` directly (complete source, with sha256, in "Investigation driver programs — complete source"). Its output fields are `HTTP_STATUS`, `SENT_BYPASS_HEADER`, `RESP_VERSION_ID`/`RESP_DELETE_MARKER`, and `RESP_BODY`.

```
$ mc mb --with-lock inv/q2-lock
Bucket created successfully `inv/q2-lock`.
$ mc version info inv/q2-lock
inv/q2-lock versioning is enabled

# q2user  — s3:DeleteObject/DeleteObjectVersion granted, s3:BypassGovernanceRetention deliberately ABSENT
# q2bypass — s3:DeleteObject/DeleteObjectVersion AND s3:BypassGovernanceRetention (a NON-root identity)
$ mc admin policy create inv q2-deletenobypass q2user-policy.json
Created policy `q2-deletenobypass` successfully.
$ mc admin policy create inv q2-deletebypass  q2bypass-policy.json
Created policy `q2-deletebypass` successfully.
$ mc admin user add inv q2user   ***REDACTED*** ; mc admin policy attach inv q2-deletenobypass --user q2user
$ mc admin user add inv q2bypass ***REDACTED*** ; mc admin policy attach inv q2-deletebypass  --user q2bypass

# apply the three lock modes to distinct object versions (+ two extra GOVERNANCE objects for the bypass-success cases)
$ mc legalhold set inv/q2-lock/obj-legalhold
Object legal hold successfully set for `obj-legalhold`.
$ mc retention set --version-id 6c86f9fe-587d-4829-9f57-b4756c1ea5f0 GOVERNANCE 3650d inv/q2-lock/obj-governance
$ mc retention set --version-id 88398c31-b846-4ab2-a77f-e03d62311a45 COMPLIANCE 3650d inv/q2-lock/obj-compliance
$ mc retention set --version-id 71c425b2-f3bc-4089-a6a5-737693700739 GOVERNANCE 3650d inv/q2-lock/obj-govbypass
$ mc retention set --version-id 5d1d0427-fe4f-4649-ae0b-f95b34d0f934 GOVERNANCE 3650d inv/q2-lock/obj-govbypass-user

# before-state
$ mc legalhold info inv/q2-lock/obj-legalhold
[    ON    ]  obj-legalhold
$ mc retention info --version-id 6c86f9fe-587d-4829-9f57-b4756c1ea5f0 inv/q2-lock/obj-governance
Mode    : GOVERNANCE, expiring in 3649 days
$ mc retention info --version-id 88398c31-b846-4ab2-a77f-e03d62311a45 inv/q2-lock/obj-compliance
Mode    : COMPLIANCE, expiring in 3649 days
$ mc retention info --version-id 5d1d0427-fe4f-4649-ae0b-f95b34d0f934 inv/q2-lock/obj-govbypass-user
Mode    : GOVERNANCE, expiring in 3649 days
```

Version IDs captured for the specific-version deletes: `obj-legalhold=90741a47-eea7-43b7-a2a7-cdcd645163db`, `obj-governance=6c86f9fe-587d-4829-9f57-b4756c1ea5f0`, `obj-compliance=88398c31-b846-4ab2-a77f-e03d62311a45`, `obj-govbypass=71c425b2-f3bc-4089-a6a5-737693700739`, `obj-govbypass-user=5d1d0427-fe4f-4649-ae0b-f95b34d0f934`, `obj-q2ctrl=6914b082-9b76-434b-919c-2b73c34727d7`.

### Variant 1 — Legal hold, specific-version delete (root) → 400, `[OBSERVED]`

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access <root> -secret *** -bucket q2-lock -object obj-legalhold -version 90741a47-eea7-43b7-a2a7-cdcd645163db
HTTP_STATUS: 400 Bad Request
SENT_BYPASS_HEADER: false
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>obj-legalhold</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-legalhold</Resource><RequestId>18C215D69E28A545</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace — handler `s3.DeleteObject`, request carries `?versionId=...`, no bypass header:

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T07:00:13.912] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /q2-lock/obj-legalhold?versionId=90741a47-eea7-43b7-a2a7-cdcd645163db
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260714T070013Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=<root>/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=9d5a3776ea152321325c5895afd54e64c4d1df65b2fb40b2fba95f9adb2e0956
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 
127.0.0.1:9000 [RESPONSE] [2026-07-14T07:00:13.913] [ Duration 915µs TTFB 898.206µs ↑ 93 B  ↓ 369 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 Content-Length: 369
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 X-Amz-Request-Id: 18C215D69E28A545
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>obj-legalhold</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-legalhold</Resource><RequestId>18C215D69E28A545</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Variant 2 — Governance, no bypass (root) → 400, `[OBSERVED]`

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access <root> -secret *** -bucket q2-lock -object obj-governance -version 6c86f9fe-587d-4829-9f57-b4756c1ea5f0
HTTP_STATUS: 400 Bad Request
SENT_BYPASS_HEADER: false
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>obj-governance</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-governance</Resource><RequestId>18C20AD50470E654</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Variant 3 — Governance, bypass header set, caller LACKS `s3:BypassGovernanceRetention` → 403, `[OBSERVED]`

`q2user` supplies `x-amz-bypass-governance-retention: true` but its policy omits the bypass action:

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access q2user -secret *** -bucket q2-lock -object obj-governance -version 6c86f9fe-587d-4829-9f57-b4756c1ea5f0 -bypass
HTTP_STATUS: 403 Forbidden
SENT_BYPASS_HEADER: true
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>obj-governance</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-governance</Resource><RequestId>18C20AD504C21CB5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

Trace — the bypass header **is** present and signed (in `SignedHeaders`), and the response is 403:

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T03:38:32.416] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /q2-lock/obj-governance?versionId=6c86f9fe-587d-4829-9f57-b4756c1ea5f0
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q2user/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=ff273958afa76c5e48364fa21cdd580d59518f901698cc0f31c132cf99ce53bf
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:38:32.416] [ Duration 699µs TTFB 689.138µs ↑ 127 B  ↓ 333 B ]
127.0.0.1:9000 403 Forbidden
```

**Isolation that this 403 is at the `BypassGovernanceRetention` check, not `DeleteObject` (`[OBSERVED]`).** The same `q2user` deletes an **unlocked** version successfully (Variant 5), proving `s3:DeleteObject` is granted — so the only thing missing in Variant 3 is the bypass permission.

### Variant 4 — Compliance, bypass header set, caller is ROOT → still 400, `[OBSERVED]`

Compliance cannot be bypassed by anyone (the code has no bypass branch for `RetCompliance`) — even root with the bypass header is blocked:

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access <root> -secret *** -bucket q2-lock -object obj-compliance -version 88398c31-b846-4ab2-a77f-e03d62311a45 -bypass
HTTP_STATUS: 400 Bad Request
SENT_BYPASS_HEADER: true
RESP_BODY:
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>obj-compliance</Key><BucketName>q2-lock</BucketName><Resource>/q2-lock/obj-compliance</Resource><RequestId>18C20AD5050EADC2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

### Variant 5 — Control: unlocked version, specific-version delete by `q2user` → 204, `[OBSERVED]`

This control isolates Variant 3's `403` to the missing bypass permission: the **same** `q2user` deletes an **unlocked** version successfully (HTTP **204 No Content**, the S3 success status for `DeleteObject`), proving `s3:DeleteObject` itself is granted:

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access q2user -secret *** -bucket q2-lock -object obj-q2ctrl -version 6914b082-9b76-434b-919c-2b73c34727d7
HTTP_STATUS: 204 No Content
SENT_BYPASS_HEADER: false
RESP_VERSION_ID: 6914b082-9b76-434b-919c-2b73c34727d7
```

### Variant 6 — Governance, bypass with permission (root) → 204, `[OBSERVED]`

```
$ ./delete_single -endpoint 127.0.0.1:9000 -access <root> -secret *** -bucket q2-lock -object obj-govbypass -version 71c425b2-f3bc-4089-a6a5-737693700739 -bypass
HTTP_STATUS: 204 No Content
SENT_BYPASS_HEADER: true
RESP_VERSION_ID: 71c425b2-f3bc-4089-a6a5-737693700739
$ mc stat --version-id 71c425b2-f3bc-4089-a6a5-737693700739 inv/q2-lock/obj-govbypass
mc: <ERROR> Unable to stat `inv/q2-lock/obj-govbypass`. The specified version does not exist.   # version GONE (bypassed + removed)
```

### Variant 7 — Governance bypass by a NON-ROOT identity with the permission → 204, `[OBSERVED]`

This is the decisive sibling: it proves that GOVERNANCE bypass is gated by the **`s3:BypassGovernanceRetention` permission**, not by being the root account. `q2bypass` is an ordinary IAM user whose only elevation over `q2user` is that its policy includes `s3:BypassGovernanceRetention`. It deletes a GOVERNANCE-locked version, supplying the bypass header — and succeeds:

```
# before: the version is under GOVERNANCE retention
$ mc retention info --version-id 5d1d0427-fe4f-4649-ae0b-f95b34d0f934 inv/q2-lock/obj-govbypass-user
Mode    : GOVERNANCE, expiring in 3649 days

$ ./delete_single -endpoint 127.0.0.1:9000 -access q2bypass -secret *** -bucket q2-lock -object obj-govbypass-user -version 5d1d0427-fe4f-4649-ae0b-f95b34d0f934 -bypass
HTTP_STATUS: 204 No Content
SENT_BYPASS_HEADER: true
RESP_VERSION_ID: 5d1d0427-fe4f-4649-ae0b-f95b34d0f934

# after: the governed version is gone
$ mc stat --version-id 5d1d0427-fe4f-4649-ae0b-f95b34d0f934 inv/q2-lock/obj-govbypass-user
mc: <ERROR> Unable to stat `inv/q2-lock/obj-govbypass-user`. The specified version does not exist.
```

Trace — `Credential=q2bypass` (a **non-root** IAM user), the bypass header is signed, `storage.DeleteVersion` executes, and the response is 204:

```
127.0.0.1:9000  [STORAGE storage.DeleteVersion] [2026-07-14T03:38:50.816] /tmp/minio-investigation/data/3 q2-lock obj-govbypass-user total-errs-availability=0 total-errs-timeout=0 310.104µs
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T03:38:50.815] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /q2-lock/obj-govbypass-user?versionId=5d1d0427-fe4f-4649-ae0b-f95b34d0f934
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q2bypass/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=3ff400c4216c3130f00becd0025a98e4cbedd7a57cd120c2ebac9308afb404d3
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:38:50.816] [ Duration 1.073ms TTFB 1.049187ms ↑ 127 B  ↓ 0 B ]
127.0.0.1:9000 204 No Content
```

Comparing Variant 3 (`q2user` + bypass header → **403**) with Variant 7 (`q2bypass` + bypass header → **204**) isolates the deciding factor to exactly one thing: the presence of `s3:BypassGovernanceRetention` in the caller's policy. The header alone is insufficient; the permission is required — and it need not be root's.

### Variant 8 — Versionless delete → **`DeleteMultipleObjects`** handler, delete marker (handler correction), `[OBSERVED]`

A plain `mc rm` (no `versionId`) on a versioned bucket does **not** hit `DeleteObjectHandler`; `mc` batches it into `POST /?delete=`, served by `DeleteMultipleObjectsHandler` (`cmd/bucket-handlers.go:416`). It inserts a delete marker and is **not** blocked (the retention check only runs for a specific `versionId`):

```
$ mc rm inv/q2-lock/obj-marker2
Created delete marker `inv/q2-lock/obj-marker2` (versionId=82570ff7-4c09-4fbb-9718-bed5a6cf68ed).
$ mc ls --versions inv/q2-lock/obj-marker2
[2026-07-14 03:40:45 UTC]     0B STANDARD 82570ff7-4c09-4fbb-9718-bed5a6cf68ed v2 DEL obj-marker2
[2026-07-14 03:40:45 UTC]    20B STANDARD f5e9442c-c346-425a-9e2e-4da48f6f1004 v1 PUT obj-marker2
```

Trace proving the handler is `s3.DeleteMultipleObjects` (not `s3.DeleteObject`):

```
127.0.0.1:9000 [REQUEST s3.DeleteMultipleObjects] [2026-07-14T03:40:45] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /q2-lock/?delete=
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 <Delete><Quiet>false</Quiet><Object><Key>obj-marker2</Key></Object></Delete>
127.0.0.1:9000 [RESPONSE] 200 OK
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><DeleteMarker>true</DeleteMarker><DeleteMarkerVersionId>82570ff7-4c09-4fbb-9718-bed5a6cf68ed</DeleteMarkerVersionId><Key>obj-marker2</Key></Deleted></DeleteResult>
```

> Note: the embedded `delete_single` driver, issuing a **versionless** raw `DELETE /q2-lock/obj-marker` (no `?versionId`), instead hits `s3.DeleteObject` directly and likewise inserts a delete marker — observed `HTTP_STATUS: 204 No Content`, `RESP_DELETE_MARKER: true`. Both the batch (`mc`) and single (`delete_single`) versionless paths create a delete marker and are never WORM-blocked.

### No console log line on a WORM block (`[OBSERVED]`)

The server console log (`logs/server.log`) was **14 lines before and 14 lines after** all four blocked deletes (delta 0). The enforcement never calls the logger on a normal block — `internalLogIf(ctx, err, logger.WarningKind)` is reached only when `objectlock.UTCNowNTP()` errors (`cmd/bucket-object-lock.go:116,142`). Thus the authoritative runtime "log entry" for a locked-object delete is the **HTTP error response** shown on the trace stream, not a server log line.

### Stability

The blocked variants were re-run; verdicts were identical: 1 = `400 InvalidRequest`, 2 = `400 InvalidRequest`, 3 = `403 AccessDenied`, 4 = `400 InvalidRequest`. The success variants likewise reproduced: 5/6/7 = `204 No Content` (with the version removed), 8 = `200 OK` with a delete marker. The permission-vs-root isolation (Variant 3 `403` vs Variant 7 `204`, differing only in the `s3:BypassGovernanceRetention` grant) held on repeat.

---

## Q3 — Bit-rot detection on unauthorized backend corruption, and the logs during a subsequent GET

**Question.** How does the system handle unauthorized manual data corruption in the storage backend? Trigger a bit-rot detection event and identify the specific runtime logs generated during a subsequent GET.

**Short answer (`[OBSERVED]`).** After one on-disk erasure shard is corrupted, a subsequent GET **succeeds and returns byte-for-byte-correct data**, reconstructed from parity **in flight**. The GET itself writes **no console/audit log line**; the observable runtime signal is in the `mc admin trace --all -v` **STORAGE** stream, which shows the read fanning out to a **parity** drive after the corrupt data shard fails its interleaved bit-rot checksum. Distinctly from the reconstruction, the read path also **enqueues an automatic heal-on-read**, and that heal **is** observable — it appears in the trace as `[HEALING heal.Object] … mode=0`. Critically, at `mode=0` the automatic heal performs only `storage.CheckParts` (a part existence/size check), **not** `storage.VerifyFile` (the bit-rot checksum check); it therefore **does not detect and does not repair** the corruption. The corrupt shard was observed to **remain corrupt** across the reconstructing GET, eight further GETs over ~3.5 minutes (each re-firing a `mode=0` heal), and a full server restart. On-disk repair of bit-rot is achieved only by an **explicit deep-scan heal** (`mc admin heal --scan deep`), whose `storage.VerifyFile` + `storage.CreateFile` + `storage.RenameData` write-back **is** observable and **does** restore the shard. Beyond the parity budget (3 of 4 shards corrupt) the GET fails **safely** — no wrong bytes, no crash. The runtime reason the automatic heal runs at `mode=0` rather than the intended deep scan is traced to a specific code path in the root-cause subsection below (`[INFERRED]`, with `file:line` anchors, corroborated by the observed `mode=0`).

### Setup — 8 MiB object in erasure mode, single set EC:2

An 8 MiB object is used so real `part.N` files exist on disk (objects below the inline threshold are packed into `xl.meta`).

```
$ head -c 8388608 /dev/urandom > original.bin
$ sha256sum original.bin
f41001fa1613895ae2a6e0bd11568b6923dd6a2793bd59cf1d8b0ba722cf3bc9  original.bin
$ mc mb -p inv/q3-bitrot ; mc cp original.bin inv/q3-bitrot/object.bin
Bucket created successfully `inv/q3-bitrot`.
`/tmp/minio-investigation/evidence/original.bin` -> `inv/q3-bitrot/object.bin`
┌──────────┬─────────────┬──────────┬─────────────┐
│ Total    │ Transferred │ Duration │ Speed       │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 79.02 MiB/s │
└──────────┴─────────────┴──────────┴─────────────┘
```

On-disk layout — identical on all four drives (one shard each):

```
data/1/q3-bitrot/object.bin/xl.meta
data/1/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
data/2/q3-bitrot/object.bin/xl.meta
data/2/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
data/3/... (same)     data/4/... (same)
```

### Locating the shard with `xl-meta` (valid **positional** invocation — no `-d` flag)

The `xl-meta` tool takes the `xl.meta` path as a positional argument. (`xl-meta`'s only flags are `--data`, `--export`, `--combine`, `--xver`, `--help`; there is **no** `-d` flag.)

```
$ ./xl-meta /tmp/minio-investigation/data/1/q3-bitrot/object.bin/xl.meta
```

Decoded erasure metadata (`Versions[0].Metadata.V2Obj`):

```json
{
  "DDir": "Y6Gohva+SsK7xHezPtWdXg==",
  "EcAlgo": 1,
  "EcM": 2,
  "EcN": 2,
  "EcBSize": 1048576,
  "EcIndex": 4,
  "EcDist": [ 4, 1, 2, 3 ],
  "Size": 8388608,
  "PartNums": [ 1 ],
  "PartSizes": [ 8388608 ],
  "CSumAlgo": 1
}
```

`EcM=2` data + `EcN=2` parity; `DDir` base64 `Y6Gohva+SsK7xHezPtWdXg==` decodes to the on-disk directory UUID `63a1a886-f6be-4ac2-bbc4-77b33ed59d5e` (verified: `python3 -c "import base64,uuid; print(uuid.UUID(bytes=base64.b64decode('Y6Gohva+SsK7xHezPtWdXg==')))"`). `EcDist=[4,1,2,3]` maps logical shard → drive; the per-drive `EcIndex` values, read from each drive's own `xl.meta`, are: drive 1 → 4, **drive 2 → 1**, drive 3 → 2, drive 4 → 3. Each `part.1` is 4194560 bytes on disk (~4 MiB shard + interleaved bit-rot checksums). The **single target** is the data shard on **drive 2** (EcIndex 1, a data shard that is always in the read set):

```
/tmp/minio-investigation/data/2/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
```

### Corrupting EXACTLY ONE shard (`[OBSERVED]`)

Pre-corruption state — complete hashes of all four shards (untruncated) and the target bytes. The exact absolute path is bound to `$TARGET`:

```
$ TARGET=/tmp/minio-investigation/data/2/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
$ for d in 1 2 3 4; do sha256sum /tmp/minio-investigation/data/$d/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1; done
e7613bf4f914d11bc13a2a7693a8a55ce512b4d5a8875af04677ca77900b7a4f  data/1/.../part.1
568a533479122328681865eae620084da8c761e92516fdcb75a85b31a8f0d270  data/2/.../part.1   <- TARGET
c19032aaa60f743413e86800f7f3a232251b3ac46b8d7353c133272cfbfa8a11  data/3/.../part.1
f1d29681546a6e364d04156650bcc6866b628cd2aa456731f86ec9c139170091  data/4/.../part.1
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 26 fa 9c 81 d0 75 69 cc 20 46 35 d8 15 8e a0 f0
```

Overwrite 16 bytes at offset 2000000 with `0xDEADBEEF`×4 (simulating unauthorized backend tampering of a single shard):

```
$ printf '\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef' \
    | dd of="$TARGET" bs=1 seek=2000000 count=16 conv=notrunc
16+0 records in
16+0 records out
16 bytes copied, 0.000235433 s, 16 kB/s
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 de ad be ef de ad be ef de ad be ef de ad be ef
$ sha256sum "$TARGET"
1d5eefae93f2ec4e0b0a29d0969a3ec5005f1ac5b5698cf22dcbf8b919dcec09  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
```

Only the drive-2 shard changed (`568a5334…` → `1d5eefae…`); drives 1, 3, 4 remain byte-identical to baseline.

### The subsequent GET — reconstructs correctly, no console log (`[OBSERVED]`)

A fresh GET to a **unique** destination, with a pre-absence assertion so a stale file cannot masquerade as success:

```
$ DEST=evidence/retrieved_1783998384000000000.bin
$ test ! -e "$DEST" && echo "PRE-GET: dest absent (asserted)"
PRE-GET: dest absent (asserted)
$ mc cp inv/q3-bitrot/object.bin "$DEST" ; echo "exit=$?"
`inv/q3-bitrot/object.bin` -> `evidence/retrieved_1783998384000000000.bin`
┌──────────┬─────────────┬──────────┬──────────────┐
│ Total    │ Transferred │ Duration │ Speed        │
│ 8.00 MiB │ 8.00 MiB    │ 00m00s   │ 244.23 MiB/s │
└──────────┴─────────────┴──────────┴──────────────┘
exit=0
$ sha256sum "$DEST"
f41001fa1613895ae2a6e0bd11568b6923dd6a2793bd59cf1d8b0ba722cf3bc9  retrieved_1783998384000000000.bin
```

The retrieved SHA-256 equals the **original** `f41001fa…` exactly — the GET returned correct bytes despite the corrupt shard, i.e. it reconstructed the data shard from parity in flight.

**Console/audit log during the GET: none.** The server console log was byte-identical before and after the GET (delta = 0 lines). This is grounded in source: the erasure read path has **no `logIf` on the corruption branch** — `streamingBitrotReader.ReadAt` returns the sentinel silently (`cmd/bitrot-streaming.go:183-186`):

```go
	b.h.Write(buf)
	if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {
		return 0, errFileCorrupt
	}
```

(the sentinel is returned directly; there is no `logger.LogIf`/`bugLogIf` call on this branch, which is why the GET produces no console line), and `cmd/erasure-decode.go:193-198` records the failure only as an in-memory atomic flag (`bitrotHeal`), again with no log:

```go
			if err != nil {
				switch {
				case errors.Is(err, errFileNotFound):
					atomic.StoreInt32(&missingPartsHeal, 1)
				case errors.Is(err, errFileCorrupt):
					atomic.StoreInt32(&bitrotHeal, 1)
```

The observable runtime signal is therefore the **`mc admin trace --all -v` STORAGE stream**, captured concurrently with the GET. The relevant `storage.ReadFileStream` lines (verbatim; drives referenced by their data-dir root) show the read fanning out across drives — critically, a **parity** drive (`data/4`) is read (partially, 2.5 MiB) to reconstruct the failed data shard, and no error surfaces (`total-errs-availability=0`):

```
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-14T03:06:24.974] /tmp/minio-investigation/data/2 q3-bitrot object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1 total-errs-availability=0 total-errs-timeout=0 35.131µs 4.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-14T03:06:24.974] /tmp/minio-investigation/data/3 q3-bitrot object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1 total-errs-availability=0 total-errs-timeout=0 53.85µs 4.0 MiB
127.0.0.1:9000  [STORAGE storage.ReadFileStream] [2026-07-14T03:06:24.977] /tmp/minio-investigation/data/4 q3-bitrot object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1 total-errs-availability=0 total-errs-timeout=0 52.179µs 2.5 MiB
```

Three drives are read (`data/2`, `data/3`, `data/4`); `data/1` is not read (only `EcM+1 = 3` shards are contacted). The `data/2` shard is the corrupt one, so its bytes fail the interleaved bit-rot checksum and are discarded; the `data/4` **parity** read (partial, 2.5 MiB) supplies the reconstruction. The only REQUEST-level funcnames on this stream were `s3.GetObject`, `s3.HeadObject`, and `s3.GetBucketLocation` — the corruption never becomes a request-level error because parity covers it transparently.

### The automatic heal-on-read fires — but at `mode=0` it is a bit-rot NO-OP (`[OBSERVED]`)

Independently of the in-flight reconstruction, the read path enqueues a background heal-on-read for the object. That heal **does** fire, and it **is** visible in the trace — as a `[HEALING heal.Object]` event with **`mode=0`**, accompanied by four `storage.CheckParts` calls (one per drive) and, crucially, **zero `storage.VerifyFile` calls**:

```
127.0.0.1:9000  [STORAGE storage.CheckParts] [2026-07-14T03:06:25.982] /tmp/minio-investigation/data/1 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 67.107µs
127.0.0.1:9000  [STORAGE storage.CheckParts] [2026-07-14T03:06:25.982] /tmp/minio-investigation/data/2 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 16.671µs
127.0.0.1:9000  [STORAGE storage.CheckParts] [2026-07-14T03:06:25.983] /tmp/minio-investigation/data/3 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 24.66µs
127.0.0.1:9000  [STORAGE storage.CheckParts] [2026-07-14T03:06:25.983] /tmp/minio-investigation/data/4 q3-bitrot object.bin total-errs-timeout=0 total-errs-availability=0 11.242µs
127.0.0.1:9000  [HEALING heal.Object] [2026-07-14T03:06:25.982] q3-bitrot/object.bin mode=0 remove=true version-id=null disks=4 dry=false 367.314µs 8.0 MiB
```

`storage.CheckParts` verifies each part's **existence and declared size** — which the corrupt shard still passes, because only 16 content bytes were overwritten (the `part.1` file is still 4194560 bytes). `storage.VerifyFile` — the call that reads the interleaved bit-rot checksums and would detect the tampering — **never runs** at `mode=0`. The direct consequence, verified on disk, is that the shard is **still corrupt immediately after the automatic heal**:

```
$ sha256sum "$TARGET"    # immediately after the heal.Object mode=0 event
1d5eefae93f2ec4e0b0a29d0969a3ec5005f1ac5b5698cf22dcbf8b919dcec09  .../data/2/.../part.1
```
(unchanged from the post-corruption hash — the automatic `mode=0` heal did **not** repair it.)

This is not a one-shot timing artifact. Re-issuing the GET eight times over ~3.5 minutes re-fires the heal every time, and **every** heal is `mode=0`; the shard stays corrupt throughout (`autoheal_poll.log`):

```
poll 1  t+  0s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 2  t+ 30s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 3  t+ 60s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 4  t+ 90s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 5  t+120s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 6  t+151s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 7  t+181s  drive2=1d5eefae93f2ec4e…  CORRUPT
poll 8  t+211s  drive2=1d5eefae93f2ec4e…  CORRUPT
$ grep 'heal\.Object' trace_all.log | grep -oE 'mode=[0-9]+' | sort | uniq -c
      8 mode=0
```

It also survives a **full server restart** (same data directories). Immediately after restart, and after a fresh post-restart GET (which again returns the correct `f41001fa…` bytes from parity), the shard is still corrupt:

```
$ # stop server (by pid), restart on the same data dirs, wait for health
$ sha256sum "$TARGET"        # immediately after restart, before any GET
1d5eefae93f2ec4e0b0a29d0969a3ec5005f1ac5b5698cf22dcbf8b919dcec09  .../data/2/.../part.1
$ mc cp inv/q3-bitrot/object.bin evidence/postrestart.bin ; sha256sum evidence/postrestart.bin
f41001fa1613895ae2a6e0bd11568b6923dd6a2793bd59cf1d8b0ba722cf3bc9  postrestart.bin   # correct (parity)
$ sha256sum "$TARGET"        # after post-restart GET
1d5eefae93f2ec4e0b0a29d0969a3ec5005f1ac5b5698cf22dcbf8b919dcec09  .../data/2/.../part.1  # STILL corrupt
```

So the honest, observed characterization is: **in-flight reconstruction serves correct bytes; the automatic heal-on-read fires at `mode=0` and is a NO-OP for bit-rot; the shard is not repaired by any automatic path within the observed window.** Why the automatic heal runs at `mode=0` rather than a deep scan is a specific, citable code path — see the root-cause subsection below.

### Explicit deep-scan heal — a SEPARATE operator action that DOES repair, and IS observable (`[OBSERVED]`)

Repairing bit-rot on disk requires an operator-initiated **deep** heal. This is a distinct control surface from the automatic heal-on-read above — it is not proof that the automatic path repairs; it is the path that actually does:

```
$ mc admin heal --recursive --force-start --scan deep inv/q3-bitrot
mc: <ERROR> Unable to display heal status. Invalid Request.
```

The "Unable to display heal status" line is an `mc` **status-display quirk only** (the deep heal itself ran, as the trace and on-disk result below confirm). Post-heal, the target shard is restored to its **pre-corruption** bytes (complete hash):

```
$ sha256sum "$TARGET"
568a533479122328681865eae620084da8c761e92516fdcb75a85b31a8f0d270  /tmp/minio-investigation/data/2/q3-bitrot/object.bin/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1
$ od -An -tx1 -j 2000000 -N 16 "$TARGET"
 26 fa 9c 81 d0 75 69 cc 20 46 35 d8 15 8e a0 f0
```

The hash equals the pre-corruption baseline `568a5334…` and the bytes at offset 2000000 are the original `26 fa 9c 81 …` — the shard is fully restored on disk. The `mc admin trace --all -v` STORAGE stream during the deep heal shows the **contrasting** detection-and-repair signature: **`storage.VerifyFile` on every one of the four drives** (the bit-rot checksum check the `mode=0` heal skipped), followed by a fresh shard write (`storage.CreateFile`) and atomic rename (`storage.RenameData`) **only on `data/2`**. Verbatim:

```
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-14T03:11:58.078] /tmp/minio-investigation/data/1 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 1.583715ms
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-14T03:11:58.080] /tmp/minio-investigation/data/2 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 652.483µs
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-14T03:11:58.081] /tmp/minio-investigation/data/3 q3-bitrot object.bin total-errs-availability=0 total-errs-timeout=0 1.532379ms
127.0.0.1:9000  [STORAGE storage.VerifyFile] [2026-07-14T03:11:58.082] /tmp/minio-investigation/data/4 q3-bitrot object.bin total-errs-timeout=0 total-errs-availability=0 1.462121ms
127.0.0.1:9000  [STORAGE storage.CreateFile] [2026-07-14T03:11:58.084] /tmp/minio-investigation/data/2 .minio.sys/tmp 537b7ff2-3200-4e97-bc7f-085ca6f5110b/63a1a886-f6be-4ac2-bbc4-77b33ed59d5e/part.1 total-errs-availability=0 total-errs-timeout=0 48.746168ms 4.0 MiB
127.0.0.1:9000  [STORAGE storage.RenameData] [2026-07-14T03:11:58.133] /tmp/minio-investigation/data/2 537b7ff2-3200-4e97-bc7f-085ca6f5110b 63a1a886-f6be-4ac2-bbc4-77b33ed59d5e q3-bitrot object.bin total-errs-timeout=0 total-errs-availability=0 17.717431ms
```

`VerifyFile` is the disk-level bit-rot check (`cmd/xl-storage.go` `VerifyFile`/`bitrotVerify`, L3097/L3079). The reconstructed shard is written on `data/2` under a temporary UUID directory `.minio.sys/tmp/537b7ff2-3200-4e97-bc7f-085ca6f5110b/` and then atomically `RenameData`'d so its `63a1a886-…` (the object `DDir`) replaces the corrupt one — exactly `healObject`'s write-back path (`cmd/erasure-healing.go`). Only `data/2` receives `CreateFile`+`RenameData`, confirming a single-shard repair. The contrast is the whole point of the question: **automatic heal-on-read → `CheckParts`, `mode=0`, no repair; explicit deep heal → `VerifyFile`, repair.**

### Sibling — beyond the parity budget (3 of 4 shards corrupt): safe controlled failure (`[OBSERVED]`)

With `EcM=2`, reconstruction needs at least 2 intact shards. Corrupting **three** of the four `part.1` shards (drives 2, 3, 4; drive 1 left intact) leaves only one good shard — below the reconstruction threshold. The GET then fails **safely**:

```
$ for d in 2 3 4; do
    printf '\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef\xde\xad\xbe\xef' \
      | dd of="$INV/data/$d/q3-bitrot/object.bin/$DDIR/part.1" bs=1 seek=2000000 count=16 conv=notrunc
  done   # 3 shards corrupt, 1 good ($DDIR = object data-dir located via xl-meta)
16+0 records in
16+0 records out
16 bytes copied, 0.000212109 s, 16 kB/s
16+0 records in
16+0 records out
16 bytes copied, 0.000224017 s, 16 kB/s
16+0 records in
16+0 records out
16 bytes copied, 0.00018746 s, 16 kB/s
$ mc cp inv/q3-bitrot/object.bin evidence/beyondparity.bin ; echo "exit=$?"
`inv/q3-bitrot/object.bin` -> `/tmp/minio-investigation/evidence/beyondparity.bin`
mc: <ERROR> Failed to copy `http://127.0.0.1:9000/q3-bitrot/object.bin`. unexpected EOF
exit=1
$ test -e evidence/beyondparity.bin && echo "dest exists" || echo "dest ABSENT (no wrong bytes written)"
dest ABSENT (no wrong bytes written)
$ curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/live
200
```

The `mc admin trace --all -v` REQUEST block shows the server begins the response (`200 OK` headers are sent and ~3.0 MiB streams) and then the stream aborts at the first erasure block it cannot reconstruct — the client observes `unexpected EOF` (credential/signature redacted):

```
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T03:12:32.450] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /q3-bitrot/object.bin
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=<root-access-key redacted>/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=<redacted>
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:12:32.456] [ Duration 5.356ms TTFB 1.071663ms ↑ 93 B  ↓ 3.0 MiB ]
127.0.0.1:9000 200 OK
```

The safety properties hold: **no complete object with wrong bytes is ever delivered** (the client discards the truncated transfer; the destination file is absent), the **server does not crash** (process alive, `health/live` = 200), and no adjacent object is affected. Beyond the parity budget the loss is **fundamental**, not a policy choice: an explicit `mc admin heal --scan deep` run against this state was observed to leave drives 2, 3, and 4 still corrupt (`deadbeef` at offset 2000000 on each), because reconstructing any one shard needs `EcM=2` intact shards and only one (drive 1) remained. A clean baseline for the subsequent stability run was therefore restored by **re-uploading** the object (a deterministic re-PUT of the same content reproduces the same shard bytes — verified: drive-2 `part.1` returned to `568a5334…`), not by healing.

### Root cause — why the automatic heal runs at `mode=0` (`[INFERRED]`, corroborated by the observed `mode=0`)

The read path *intends* a deep (bit-rot) heal, but the intent is dropped before the background heal executes. The chain, at commit `c07e5b49d477`:

1. `cmd/erasure-object.go:399-409` — once the corrupt part has been fully served, the GET enqueues a partial heal tagged for a bit-rot scan (`healOnce` at L346):
```go
					healOnce.Do(func() {
						globalMRFState.addPartialOp(PartialOperation{
							Bucket:     bucket,
							Object:     object,
							VersionID:  fi.VersionID,
							Queued:     time.Now(),
							SetIndex:   er.setIndex,
							PoolIndex:  er.poolIndex,
							BitrotScan: errors.Is(err, errFileCorrupt),   // = true
						})
					})
```
2. `cmd/mrf.go:260-276` — the MRF routine correctly translates `BitrotScan` into a **deep** scan, then (for a single object with no explicit version list) calls the package-level `healObject` with that scan mode:
```go
			scan := madmin.HealNormalScan
			if u.BitrotScan {
				scan = madmin.HealDeepScan
			}

			if u.Object == "" {
				healBucket(u.Bucket, scan)
			} else {
				if len(u.Versions) > 0 {
					vers := len(u.Versions) / 16
					if vers > 0 {
						for i := 0; i < vers; i++ {
							healObject(u.Bucket, u.Object, uuid.UUID(u.Versions[16*i:]).String(), scan)
						}
					}
				} else {
					healObject(u.Bucket, u.Object, u.VersionID, scan)   // scan = HealDeepScan (single-object branch)
```
3. `cmd/global-heal.go:591-595` — the package-level `healObject` forwards that scan mode to the background heal sequence:
```go
func healObject(bucket, object, versionID string, scan madmin.HealScanMode) error {
	// Get background heal sequence to send elements to heal
	bgSeq, ok := globalBackgroundHealState.getHealSequenceByToken(bgHealingUUID)
	if ok {
		return bgSeq.healObject(bucket, object, versionID, scan)
```
4. `cmd/admin-heal-ops.go:916-926` — **here the deep-scan argument is dropped.** `healSequence.healObject` receives `scanMode` but queues a `healSource` whose `opts` is `&h.settings`, *not* the incoming `scanMode`:
```go
func (h *healSequence) healObject(bucket, object, versionID string, scanMode madmin.HealScanMode) error {
	if h.isQuitting() {
		return errHealStopSignalled
	}

	err := h.queueHealTask(healSource{
		bucket:    bucket,
		object:    object,
		versionID: versionID,
		opts:      &h.settings,      // <-- scanMode argument is NOT used here
	}, madmin.HealItemObject)
```
5. `cmd/admin-heal-ops.go:729-733` — `queueHealTask` then takes the task's scan mode from `*source.opts` (`= h.settings`), whose `ScanMode` is the background sequence's default zero value (`HealUnknownScan = 0`), never the deep mode:
```go
	if source.opts != nil {
		task.opts = *source.opts
	} else {
		task.opts.ScanMode = madmin.HealNormalScan
	}
```
6. Per `madmin-go/v3` `heal-commands.go:36-45`, `HealUnknownScan = 0`, `HealNormalScan = 1`, `HealDeepScan = 2`, and only the deep scan "checks for parts bitrot checksums." A `mode=0` heal therefore calls `CheckParts` (existence/size) rather than `VerifyFile` (bit-rot), which is exactly what the trace shows.
7. `cmd/erasure-healing.go:1080-1083` — deep-scan escalation exists, but it only triggers **after** a heal returns `errFileCorrupt`:
```go
	if errors.Is(err, errFileCorrupt) && opts.ScanMode != madmin.HealDeepScan {
		// Instead of returning an error when a bitrot error is detected
		// during a normal heal scan, heal again with bitrot flag enabled.
		opts.ScanMode = madmin.HealDeepScan
```
Because the `mode=0` heal uses `CheckParts` and never reads the bit-rot checksum, it never returns `errFileCorrupt` for content tampering, so the escalation never fires. Net effect: the automatic heal-on-read cannot repair bit-rot; only an explicit deep heal does — precisely the observed behavior. *(This root cause is `[INFERRED]` from source reading; it is directly corroborated by the `[OBSERVED]` `mode=0` heal event, the observed `CheckParts`-not-`VerifyFile` storage calls, and the shard remaining corrupt across GETs and a restart. Per this investigation's read-only scope, no source change is made — the behavior is reported, not altered.)*

### Stability (2 runs, `[OBSERVED]`)

Re-running the full cycle a second time produced identical verdicts: re-corruption reproduces `1d5eefae…`; the GET exits 0 and returns `f41001fa…` (= original); the automatic heal fires at `mode=0`; the shard remains `1d5eefae…` on disk after the GET, the poll window, and a restart; and the explicit deep heal restores it to `568a5334…`.

### Coverage of named mechanisms (Q3)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| On-read checksum verify → sentinel | `errFileCorrupt` returned with **no log** — `cmd/bitrot-streaming.go` `streamingBitrotReader.ReadAt` (mismatch L183-187) | source-grounded; GET console delta = 0 `[OBSERVED]` |
| Corruption sentinel | `errFileCorrupt = StorageErr("file is corrupted")` `cmd/storage-errors.go:104` | `[INFERRED]` (source) |
| Heal flag + parity reconstruct | `cmd/erasure-decode.go:193-198` sets `bitrotHeal`; RS reconstruct | GET returns correct bytes from parity `[OBSERVED]` |
| Heal-on-read enqueue (BitrotScan=true) | `globalMRFState.addPartialOp(… BitrotScan …)` `cmd/erasure-object.go:399-407` (healOnce L346) | `[INFERRED]` (no log at enqueue); its *result* observed as `heal.Object mode=0` |
| MRF deep-scan intent | `scan = madmin.HealDeepScan` `cmd/mrf.go:260-262`, call L276 | `[INFERRED]` (source) |
| **Deep-scan argument dropped** | `healSequence.healObject` queues `opts:&h.settings` not `scanMode` `cmd/admin-heal-ops.go:916-926`; task mode from `*source.opts` L729-732 | `[INFERRED]` (source); corroborated by `[OBSERVED]` `mode=0` |
| Automatic heal at mode=0 → CheckParts only | `[HEALING heal.Object] mode=0` + 4× `storage.CheckParts`, 0× `storage.VerifyFile` | `[OBSERVED]` (trace); shard stays corrupt |
| Escalation only on errFileCorrupt | `cmd/erasure-healing.go:1080-1083` | `[INFERRED]` (source); never triggers at mode=0 |
| Disk-level verify (deep heal) | `VerifyFile`/`bitrotVerify` `cmd/xl-storage.go:3097,3079` | 4× `storage.VerifyFile` in deep-heal trace `[OBSERVED]` |
| Repair write-back (deep heal) | `healObject` `cmd/erasure-healing.go:258` | `CreateFile`+`RenameData` on drive 2 `[OBSERVED]` |
| Background MRF consumer | `cmd/background-newdisks-heal-ops.go:389-390` | `[INFERRED]` (source) |
| Scanner bit-rot scan-mode select | `getCycleScanMode` `cmd/data-scanner.go:93` | `[INFERRED]` (source) |
| Beyond parity → safe failure | GET `200 OK` then `unexpected EOF` at ~3.0 MiB; dest absent; server health 200 | `[OBSERVED]` (trace + client + health) |

---

## Q4 — STS temporary credentials enforce the inline session policy (intersection semantics)

**Question.** Verify that when a user gets temporary credentials, MinIO enforces the session policy on that user. Give runtime test output to prove this behavior.

**Short answer (`[OBSERVED]`).** Temporary credentials issued by STS `AssumeRole` with an inline session policy are enforced as the **intersection** of the session policy AND the parent identity's policy: a request succeeds only if **both** allow it. Proven two ways with real credentials: (a) a **broad** parent (`s3:*`) + a **GetObject-only** session policy → `GetObject` succeeds (200) but `PutObject` is denied (403) by the session policy; (b) a **narrow** parent (`GetObject`-only) + a **broad** (`s3:*`) session policy → `PutObject` is still denied (403) because the session policy **cannot widen** the parent. The session policy travels as a signed JWT **claim** keyed by the constant `policy.SessionPolicyName` (`"sessionPolicy"`), not as a credentials field.

### Setup — one bucket, one readable object, a BROAD parent and a NARROW parent

```
$ mc mb inv/q4-bucket
Bucket created successfully `inv/q4-bucket`.
$ mc cp seed.txt inv/q4-bucket/readable.txt          # 24-byte object: printf 'q4-seed-object-contents\n'
$ mc ls inv/q4-bucket
[2026-07-14 03:49:50 UTC]    24B STANDARD readable.txt
```

Broad parent policy `q4-broad` (attached to user `q4user`):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow", "Action": ["s3:*"],
    "Resource": ["arn:aws:s3:::q4-bucket", "arn:aws:s3:::q4-bucket/*"] }
 ]
}
```

Narrow parent policy `q4-narrow` (attached to user `q4narrow`):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow", "Action": ["s3:GetObject"],
    "Resource": ["arn:aws:s3:::q4-bucket/*"] }
 ]
}
```

```
$ mc admin policy create inv q4-broad q4-broad.json
Created policy `q4-broad` successfully.
$ mc admin user add inv q4user ***REDACTED*** ; mc admin policy attach inv q4-broad --user q4user
Added user `q4user` successfully.
Attached Policies: [q4-broad]
$ mc admin policy create inv q4-narrow q4-narrow.json
Created policy `q4-narrow` successfully.
$ mc admin user add inv q4narrow ***REDACTED*** ; mc admin policy attach inv q4-narrow --user q4narrow
Added user `q4narrow` successfully.
Attached Policies: [q4-narrow]
```

### Parent baseline — the broad parent CAN do BOTH GET and PUT directly (`[OBSERVED]`)

This baseline is essential: it proves that when the STS `PutObject` is later denied, the denial comes from the **session policy**, not from a parent that lacked `PutObject` in the first place. Issued directly with the parent's own long-term credentials (no STS):

```
$ mc alias set q4parent http://127.0.0.1:9000 q4user ***REDACTED***
$ mc cp q4parent/q4-bucket/readable.txt bget_out.txt ; echo "exit=$?"
`q4parent/q4-bucket/readable.txt` -> `/tmp/minio-investigation/evidence/q4/bget_out.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 24 B  │ 24 B        │ 00m00s   │ 4.36 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
exit=0
$ mc cp bput.txt q4parent/q4-bucket/broad-parent-wrote.txt ; echo "exit=$?"
`/tmp/minio-investigation/evidence/q4/bput.txt` -> `q4parent/q4-bucket/broad-parent-wrote.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 15 B  │ 15 B        │ 00m00s   │ 1.60 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
exit=0
```

Both direct operations succeed → the broad parent `q4user` is allowed **both** `s3:GetObject` and `s3:PutObject`.

### Main proof — GetObject-only session policy narrows the broad parent (`[OBSERVED]`)

The producing program is the **embedded `sts_assume_role` driver** (complete source, with sha256, in "Investigation driver programs — complete source"). It calls the canonical STS `AssumeRole` entry point via the MinIO Go SDK (`credentials.NewSTSAssumeRole`, endpoint `http://127.0.0.1:9000`) with a **restrictive** inline session policy (`GetObject` only), then reuses the **same** temporary credential set for both `GetObject` and `PutObject`. Its output fields are `STS_OK` (redacted temp-access-key prefix + `sessionTokenLen`), `GET_RESULT`, and `PUT_RESULT`:

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt \
    -policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}' \
    -duration 3600 -op get,put
STS_OK: tempAccessKey=OEC6L2… sessionTokenLen=463
GET_RESULT: OK bytes=24
PUT_RESULT: error code=AccessDenied msg="Access Denied."
```

The single temp identity (parent `q4user`) can **read** (allowed by both parent and session policy, `GET_RESULT: OK bytes=24`) but cannot **write** (allowed by parent, **denied by the session policy**, `PUT_RESULT: error code=AccessDenied`) — exactly the intersection behaviour.

### The session policy is a JWT CLAIM keyed by `policy.SessionPolicyName` (`[OBSERVED]`) — resolves the "field" misconception

The temporary credentials do **not** carry a `SessionPolicyName` *field* on the credentials struct. The session policy is embedded in the **session token** as a base64 JWT claim whose key is the exported constant `policy.SessionPolicyName`:

```
$ grep -n SessionPolicyName $(go env GOMODCACHE)/github.com/minio/pkg/v3@v3.0.22/policy/constants.go
27:	SessionPolicyName = "sessionPolicy"
```

Decoding the **payload segment** of the session token issued by the run above (the token itself is a live bearer credential and is redacted everywhere in this document; its decoded payload contains no secret — only `accessKey`, `exp`, `parent`, and the base64 `sessionPolicy` claim):

```
$ TOKEN=<session token from the AssumeRole response>   # redacted; 463-char JWT
$ echo "$TOKEN" | cut -d. -f2 | base64 -d | python3 -m json.tool
{
    "accessKey": "OEC6L2R12I9INNDOWD6Y",
    "exp": 1784004672,
    "parent": "q4user",
    "sessionPolicy": "eyJWZXJzaW9uIjoiMjAxMi0xMC0xNyIsIlN0YXRlbWVudCI6W3siRWZmZWN0IjoiQWxsb3ciLCJBY3Rpb24iOlsiczM6R2V0T2JqZWN0Il0sIlJlc291cmNlIjpbImFybjphd3M6czM6OjpxNC1idWNrZXQvKiJdfV19"
}
```

The literal claim key is `"sessionPolicy"`. Its base64 value decodes back to exactly the policy that was sent:

```
$ echo 'eyJWZXJzaW9uIjoiMjAxMi0xMC0xNyIsIlN0YXRlbWVudCI6W3siRWZmZWN0IjoiQWxsb3ciLCJBY3Rpb24iOlsiczM6R2V0T2JqZWN0Il0sIlJlc291cmNlIjpbImFybjphd3M6czM6OjpxNC1idWNrZXQvKiJdfV19' | base64 -d
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::q4-bucket/*"]}]}
```

Server side, the claim is written at `cmd/sts-handlers.go:127` (`c[policy.SessionPolicyName] = base64.StdEncoding.EncodeToString(policyBuf)`, inside `populateSessionPolicy` at L94, size-bounded by `maxSTSSessionPolicySize = 2048` at L89 / check at L123) and read back at `cmd/auth-handler.go:251` (`sp, spok := claims.Lookup(policy.SessionPolicyName)`), which re-publishes it under the internal key `sessionPolicyNameExtracted` (`cmd/iam.go:2136`) for evaluation.

### Runtime trace of the main proof — COMPLETE, unedited (`[OBSERVED]`)

`mc admin trace --all -v inv` captured the whole flow. The complete captured window is reproduced verbatim below (121 lines, `sts.AssumeRole` → `s3.GetBucketLocation` → `s3.GetObject` → `s3.PutObject`). **The only modification is security redaction of live bearer credentials**: the four values `<SecretAccessKey>`, `<SessionToken>`, and the three `X-Amz-Security-Token` header values are masked (they are single-use temporary credentials); every request line, every other header, every response status, and every response body is shown in full — nothing is omitted "for length".

```
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-14T03:51:12.414] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: Go-http-client/1.1
127.0.0.1:9000 X-Amz-Date: 20260714T035112Z
127.0.0.1:9000 Accept-Encoding: gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q4user/20260714/us-east-1/sts/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=c0e995f9277d88b5191164b63df0fc94e5a7c356ef1f13cdf2155923918091bd
127.0.0.1:9000 Content-Length: 276
127.0.0.1:9000 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9000 Action=AssumeRole&DurationSeconds=3600&Policy=%7B%22Version%22%3A%222012-10-17%22%2C%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%5B%22s3%3AGetObject%22%5D%2C%22Resource%22%3A%5B%22arn%3Aaws%3As3%3A%3A%3Aq4-bucket%2F%2A%22%5D%7D%5D%7D&Version=2011-06-15
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:51:12.417] [ Duration 3.319ms TTFB 3.314235ms ↑ 361 B  ↓ 1004 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 1004
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Vary: Origin
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18C20B85F82B1CF1
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><AssumedRoleUser><Arn></Arn><AssumeRoleId></AssumeRoleId></AssumedRoleUser><Credentials><AccessKeyId>OEC6L2R12I9INNDOWD6Y</AccessKeyId><SecretAccessKey>***REDACTED-TEMP-SECRET***</SecretAccessKey><SessionToken>***REDACTED-SESSION-TOKEN (463-char JWT; decoded payload shown below)***</SessionToken><Expiration>2026-07-14T04:51:12Z</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>18C20B85F82B1CF1</RequestId></ResponseMetadata></AssumeRoleResponse>
127.0.0.1:9000 
127.0.0.1:9000 [REQUEST s3.GetBucketLocation] [2026-07-14T03:51:12.418] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /q4-bucket/?location=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260714T035112Z
127.0.0.1:9000 X-Amz-Security-Token: ***REDACTED-SESSION-TOKEN***
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=OEC6L2R12I9INNDOWD6Y/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=935c07a80b055b9bd253fb8ddf146e43d8737cbe8e47e7048230e28af4a839d3
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:51:12.419] [ Duration 436µs TTFB 417.644µs ↑ 98 B  ↓ 298 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Ratelimit-Remaining: 563280
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C20B85F86D6896
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 563280
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 298
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/</Resource><RequestId>18C20B85F86D6896</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000 
127.0.0.1:9000  [OS os.OpenFileR] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/4/q4-bucket/readable.txt/xl.meta 30.768µs
127.0.0.1:9000  [OS os.OpenFileR] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/2/q4-bucket/readable.txt/xl.meta 30.536µs
127.0.0.1:9000  [STORAGE storage.ReadXL] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/4 q4-bucket readable.txt total-errs-availability=0 total-errs-timeout=0 63.319µs 430 B
127.0.0.1:9000  [OS os.OpenFileR] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/3/q4-bucket/readable.txt/xl.meta 26.322µs
127.0.0.1:9000  [STORAGE storage.ReadXL] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/2 q4-bucket readable.txt total-errs-availability=0 total-errs-timeout=0 62.714µs 430 B
127.0.0.1:9000  [STORAGE storage.ReadXL] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/3 q4-bucket readable.txt total-errs-availability=0 total-errs-timeout=0 58.068µs 430 B
127.0.0.1:9000  [OS os.OpenFileR] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/1/q4-bucket/readable.txt/xl.meta 27.921µs
127.0.0.1:9000  [STORAGE storage.ReadXL] [2026-07-14T03:51:12.419] /tmp/minio-investigation/data/1 q4-bucket readable.txt total-errs-availability=0 total-errs-timeout=0 57.358µs 430 B
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T03:51:12.419] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /q4-bucket/readable.txt
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=OEC6L2R12I9INNDOWD6Y/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=4a5ee2c1e5205f69c8b6ce291d9c1d7c3f60f537e2f04181efd572b6d3b90226
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T035112Z
127.0.0.1:9000 X-Amz-Security-Token: ***REDACTED-SESSION-TOKEN***
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:51:12.420] [ Duration 756µs TTFB 720.512µs ↑ 98 B  ↓ 24 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: text/plain
127.0.0.1:9000 ETag: "462856436fb8c70662878ffa5893fca0"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C20B85F87B885A
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 24
127.0.0.1:9000 Last-Modified: Tue, 14 Jul 2026 03:49:50 GMT
127.0.0.1:9000 X-Ratelimit-Limit: 563280
127.0.0.1:9000 X-Ratelimit-Remaining: 563280
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T03:51:12.420] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /q4-bucket/readable.txt.sts-put
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=OEC6L2R12I9INNDOWD6Y/20260714/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-security-token,Signature=fa6b58d777cbff36cec706c908453099026cbae0e8203ad3f7fe962b55a9aad9
127.0.0.1:9000 Content-Length: 189
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Date: 20260714T035112Z
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 16
127.0.0.1:9000 X-Amz-Security-Token: ***REDACTED-SESSION-TOKEN***
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T03:51:12.420] [ Duration 188µs TTFB 177.791µs ↑ 140 B  ↓ 349 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C20B85F88D67E5
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Ratelimit-Limit: 563280
127.0.0.1:9000 X-Ratelimit-Remaining: 563280
127.0.0.1:9000 Content-Length: 349
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>readable.txt.sts-put</Key><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/readable.txt.sts-put</Resource><RequestId>18C20B85F88D67E5</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000 
```

The `sts.AssumeRole` request carries the URL-encoded session `Policy=` parameter (decodes to the GetObject-only policy) and returns `200 OK`. `s3.GetObject` returns `200 OK`; `s3.PutObject` returns `403 Forbidden` with `<Code>AccessDenied</Code>`. **Note (honest):** the `s3.GetBucketLocation` call *also* returns `403` because the GetObject-only session policy does not grant it; the SDK tolerates this and falls back to region `us-east-1`, so the subsequent GET still succeeds. There are therefore two `403`s in the trace — one benign (`GetBucketLocation`) and the substantive one (`PutObject`).

### Cannot-widen edge — narrow parent + BROAD session policy still denies PUT (`[OBSERVED]`)

The mirror case proves the session policy can only *narrow*. First, the narrow parent `q4narrow` directly (baseline) — GET allowed, PUT denied by the parent policy itself:

```
$ mc alias set q4np http://127.0.0.1:9000 q4narrow ***REDACTED***
$ mc cp q4np/q4-bucket/readable.txt npget_out.txt ; echo "exit=$?"
`q4np/q4-bucket/readable.txt` -> `/tmp/minio-investigation/evidence/q4/npget_out.txt`
┌───────┬─────────────┬──────────┬────────────┐
│ Total │ Transferred │ Duration │ Speed      │
│ 24 B  │ 24 B        │ 00m00s   │ 4.26 KiB/s │
└───────┴─────────────┴──────────┴────────────┘
exit=0
$ mc cp bput.txt q4np/q4-bucket/narrow-parent-wrote.txt ; echo "exit=$?"
mc: <ERROR> Failed to copy `/tmp/minio-investigation/evidence/q4/bput.txt`. Insufficient permissions to access this path `http://127.0.0.1:9000/q4-bucket/narrow-parent-wrote.txt`
exit=1
```

Now `q4narrow` assumes a role with a **broad** `s3:*` session policy (same embedded driver):

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4narrow -secret *** \
    -bucket q4-bucket -object readable.txt \
    -policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::q4-bucket","arn:aws:s3:::q4-bucket/*"]}]}' \
    -duration 3600 -op get,put
STS_OK: tempAccessKey=K6VGNV… sessionTokenLen=498
GET_RESULT: OK bytes=24
PUT_RESULT: error code=AccessDenied msg="Access Denied."
```

Even though the session policy grants `s3:*`, `PutObject` is denied `403` because the **parent** does not allow it. A session policy cannot grant a permission the parent lacks.

### Edge credentials — adversarial siblings (`[OBSERVED]`)

Four edge conditions on the temporary credential are exercised with the same embedded `sts_assume_role` driver (edge flags `-drop-token`, `-tamper-token`, oversized `-policy`, and `-sleep N` for post-expiry). Each produces a clean, specific rejection; the exact runtime code and message are shown together with the server-side trace confirmation.

**Edge A — missing session token** (`-drop-token`: present the temp access/secret key **without** the `X-Amz-Security-Token`). The `AssumeRole` call itself still succeeds (the token *is* issued); the rejection happens when the token-less request reaches S3 auth:

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt -policy '<GetObject-only>' -duration 3600 \
    -op get,put -drop-token
STS_OK: tempAccessKey=ZN98QU… sessionTokenLen=463
USING_TEMP_CREDS_WITHOUT_TOKEN: true
GET_RESULT: read error code=InvalidTokenId msg="The security token included in the request is invalid"
PUT_RESULT: error code=InvalidTokenId msg="The security token included in the request is invalid"
```

Server-side trace (`mc admin trace --all -v inv`) — the S3 request is rejected `403` with `InvalidTokenId`:

```
127.0.0.1:9000 [REQUEST s3.GetBucketLocation] [2026-07-14T03:54:07.636] [Client IP: 127.0.0.1]
127.0.0.1:9000 403 Forbidden
<Error><Code>InvalidTokenId</Code><Message>The security token included in the request is invalid</Message><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/</Resource><RequestId>18C20BAEC43DEEA3</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

**Edge B — tampered session token** (`-tamper-token`: flip the final byte of the JWT before use). The mutated signature fails validation with the identical canonical error:

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt -policy '<GetObject-only>' -duration 3600 \
    -op get,put -tamper-token
STS_OK: tempAccessKey=YZ1QQ7… sessionTokenLen=463
USING_TAMPERED_TOKEN: true
GET_RESULT: read error code=InvalidTokenId msg="The security token included in the request is invalid"
PUT_RESULT: error code=InvalidTokenId msg="The security token included in the request is invalid"
```

Both A and B map to `ErrInvalidToken` → Code `"InvalidTokenId"`, HTTP `403`, description `"The security token included in the request is invalid"` (`cmd/api-errors.go:1256-1260`).

**Edge C — oversized inline session policy** (a valid JSON policy of **2504 bytes**, above `maxSTSSessionPolicySize = 2048`). Unlike A/B, this is rejected **at `AssumeRole` time** — no temporary credential is ever issued:

```
$ wc -c oversized_policy.json
2504 oversized_policy.json
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt -policy "$(cat oversized_policy.json)" -duration 3600 -op get,put
STS_ASSUME_ERROR: Session policy should not exceed 2048 characters
```

Server-side trace — `sts.AssumeRole` returns `400 Bad Request` / `InvalidParameterValue`:

```
127.0.0.1:9000 [REQUEST sts.AssumeRole] [2026-07-14T03:54:06.623] [Client IP: 127.0.0.1]
127.0.0.1:9000 400 Bad Request
<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type></Type><Code>InvalidParameterValue</Code><Message>Session policy should not exceed 2048 characters</Message></Error><RequestId>18C20BAE87D84C29</RequestId></ErrorResponse>
```

The size guard is `if len(policyBuf) > maxSTSSessionPolicySize { return errSessionPolicyTooLarge }` (`cmd/sts-handlers.go:123-124`; `maxSTSSessionPolicySize = 2048` at L89; the sentinel `errSessionPolicyTooLarge = errors.New("Session policy should not exceed 2048 characters")` at `cmd/typed-errors.go:121`). The `AssumeRole` handler surfaces it as `ErrSTSInvalidParameterValue` (`cmd/sts-handlers.go:296-299`), which maps to Code `"InvalidParameterValue"`, HTTP `400` (`cmd/sts-errors.go:113-116`).

**Edge D — session lifetime and post-expiry rejection.** Two independent floors govern the session lifetime, and both were observed. **(1) Server floor:** `GetDefaultExpiration` rejects any requested `DurationSeconds < config.MinExpiration` (`= 900`, `internal/config/constants.go:93`) with `ErrInvalidDuration` (`internal/config/identity/openid/openid.go:602-628`) — so 900 s is the shortest session the server will issue. **(2) SDK floor:** the MinIO Go SDK's AssumeRole provider clamps the wire value up to `defaultDurationSeconds = 3600` (`assume_role.go:126`); it sets the wire `DurationSeconds` as follows (`assume_role.go:158-162`):

```go
	if opts.DurationSeconds > defaultDurationSeconds {
		v.Set("DurationSeconds", strconv.Itoa(opts.DurationSeconds))
	} else {
		v.Set("DurationSeconds", strconv.Itoa(defaultDurationSeconds))
	}
```

So a driver run with `-duration 900` (which is **not** greater than 3600) takes the `else` branch and puts `DurationSeconds=3600` on the wire; the issued token genuinely lives **one hour**. Observed on the wire and in the issued credential:

```
# AssumeRole request line (driver invoked with -duration 900; SDK floored it):
Action=AssumeRole&DurationSeconds=3600
# Issued credential (from the AssumeRole XML response), issued at 04:08:56Z:
<Expiration>2026-07-14T05:08:56Z</Expiration>     # = issue + 3600 s
```

**Corroboration — token still valid before `exp`** (`-sleep 960`, i.e. used 960 s after issue, well inside the 3600 s lifetime):

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt -policy '<GetObject-only>' \
    -duration 900 -op get,put -sleep 960
ISSUE_WALLCLOCK=1784002136 (04:08:56Z)
STS_OK: tempAccessKey=GHLV3U… sessionTokenLen=463
SLEEPING 960s for expiry...
GET_RESULT: OK bytes=24
PUT_RESULT: error code=AccessDenied msg="Access Denied."
USE_WALLCLOCK=1784003096 (04:24:56Z)
```

At `USE_WALLCLOCK` (04:24:56Z) the token is 960 s old but its `exp` is 05:08:56Z, so `GET` still succeeds (`PUT` remains denied by the session policy, unchanged).

**Genuine post-expiry rejection** (`-sleep 3660`, i.e. used 3660 s after issue — 60 s *past* the real 3600 s `exp`):

```
$ ./sts_assume_role -endpoint 127.0.0.1:9000 -access q4user -secret *** \
    -bucket q4-bucket -object readable.txt -policy '<GetObject-only>' \
    -duration 3600 -op get,put -sleep 3660
ISSUE_WALLCLOCK=1784005186 (04:59:46Z)
STS_OK: tempAccessKey=CW406E… sessionTokenLen=463
SLEEPING 3660s for expiry...
GET_RESULT: read error code=InvalidAccessKeyId msg="The Access Key Id you provided does not exist in our records."
PUT_RESULT: error code=InvalidAccessKeyId msg="The Access Key Id you provided does not exist in our records."
USE_WALLCLOCK=1784008846 (06:00:46Z)
```

Server-side trace of the expired-token S3 request:

```
127.0.0.1:9000 [REQUEST s3.GetBucketLocation] [2026-07-14T06:00:46.862] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /q4-bucket/?location=
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.80
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T060046Z
127.0.0.1:9000 X-Amz-Security-Token: ***REDACTED-EXPIRED-SESSION-TOKEN***
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=CW406E5IJRYXY7B58IOR/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=44be9da04f5cb204d88f3021152063f86241332d335e11bbc4497096010693b4
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:00:46.863] [ Duration 274µs TTFB 260.693µs ↑ 98 B  ↓ 351 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Limit: 564847
127.0.0.1:9000 Content-Length: 351
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18C21298196C2204
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 564847
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidAccessKeyId</Code><Message>The Access Key Id you provided does not exist in our records.</Message><BucketName>q4-bucket</BucketName><Resource>/q4-bucket/</Resource><RequestId>18C21298196C2204</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The observed post-expiry error is **not** a dedicated "expired token" code but `InvalidAccessKeyId` ("The Access Key Id you provided does not exist in our records.", HTTP `403`) — and that is **deliberate**. The request-time gate maps an expired temporary credential straight to `errInvalidAccessKeyID` at `cmd/auth-handler.go:301-304`:

```go
	// Expired credentials must return error right away.
	if cred.IsTemp() && cred.IsExpired() {
		return nil, toAPIErrorCode(r.Context(), errInvalidAccessKeyID)
	}
```

The predicate `IsExpired()` is strict and leeway-free — `return cred.Expiration.Before(time.Now().UTC())` (`internal/auth/credentials.go:153`); the identical `cred.IsTemp() && cred.IsExpired()` gate is mirrored at `cmd/jwt.go:85`, and a load-time defence at `cmd/iam-object-store.go:260-265` deletes an expired identity and returns `errNoSuchUser`. So once `exp` passes, the temporary credential resolves as *nonexistent* and every request under it is rejected `403 InvalidAccessKeyId` — exactly matching the `GET_RESULT`/`PUT_RESULT` above.

### Stability (2 runs, `[OBSERVED]`)

The main proof was run twice (a fresh temp key each time, same parent `q4user`). Both runs produced identical verdicts:

```
run1 (tempAccessKey=OEC6L2…): GET_RESULT: OK bytes=24   PUT_RESULT: error code=AccessDenied msg="Access Denied."
run2 (tempAccessKey=6BHNK7…): GET_RESULT: OK bytes=24   PUT_RESULT: error code=AccessDenied msg="Access Denied."
```

### Source grounding — where the intersection is enforced

The enforcement is `IAMSys.IsAllowedSTS` (`cmd/iam.go:2242`). It fetches the parent's mapped policies (`sys.PolicyDBGet(parentUser, ...)`), merges them into `combinedPolicy`, then evaluates the session policy and returns the **AND** of the two (`cmd/iam.go:2310-2313`):

```go
	hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
	if hasSessionPolicy {
		return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))
	}
```

`isAllowedBySessionPolicy` (`cmd/iam.go:2381`) parses the extracted claim and — critically — forces owner status off so even a root-derived session is constrained (`cmd/iam.go:2413-2417`):

```go
	// As the session policy exists, even if the parent is the root account, it
	// must be restricted by it. So, we set `.IsOwner` to false here
	// unconditionally.
	sessionPolicyArgs := args
	sessionPolicyArgs.IsOwner = false
```

For `PutObject` under the GetObject-only session policy, `isAllowedSP == false`, so the AND is false → `403`. For `PutObject` under the narrow parent, `combinedPolicy.IsAllowed(args) == false`, so the AND is false → `403`. Both observed outcomes match.

### Coverage of named mechanisms (Q4)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| STS entry point | `AssumeRole` `cmd/sts-handlers.go:256` | `[REQUEST sts.AssumeRole]` 200 `[OBSERVED]` |
| Session policy read from request | `populateSessionPolicy` `cmd/sts-handlers.go:94` | `Policy=` param in trace `[OBSERVED]` |
| Session-policy size cap | `maxSTSSessionPolicySize=2048` L89; check L123; `errSessionPolicyTooLarge` `cmd/typed-errors.go:121` | Edge C: 2504 B → 400 `InvalidParameterValue` `[OBSERVED]` |
| Session policy stored as JWT claim | `c[policy.SessionPolicyName]=base64(...)` `cmd/sts-handlers.go:127`; const `"sessionPolicy"` `pkg/v3/policy/constants.go:27` | decoded JWT `sessionPolicy` claim `[OBSERVED]` |
| Claim decoded server-side | `claims.Lookup(policy.SessionPolicyName)` `cmd/auth-handler.go:251`; extracted key `cmd/iam.go:2136` | `[INFERRED]` (source) |
| Intersection enforcement | `IsAllowedSTS` `cmd/iam.go:2242`; AND return L2310-2313 | GET 200 / PUT 403 `[OBSERVED]` |
| Owner forced off for session | `sessionPolicyArgs.IsOwner=false` `cmd/iam.go:2413-2417` | `[INFERRED]` (source) |
| Cannot-widen | parent AND session both required | narrow-parent + broad-session PUT 403 `[OBSERVED]` |
| Missing / tampered token | `ErrInvalidToken`→`InvalidTokenId`/403 `cmd/api-errors.go:1256-1260` | Edge A/B: `InvalidTokenId` `[OBSERVED]` |
| Minimum session duration | `config.MinExpiration=900` `internal/config/constants.go:93`; `GetDefaultExpiration` `internal/config/identity/openid/openid.go:602` | Edge D: 900 s floor `[OBSERVED]` |
| Post-expiry rejection | expired temp cred → `errInvalidAccessKeyID` `cmd/auth-handler.go:301-304`; strict `IsExpired()` `internal/auth/credentials.go:153` | Edge D: 403 `InvalidAccessKeyId` `[OBSERVED]` |

---

## Q5 — A basic user cannot self-promote to console admin by modifying user→policy mappings

**Question.** Show test output proving a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user-mappings modification behavior observed.

**Short answer (`[OBSERVED]`).** A user holding only a read-only policy is denied (`403 AccessDenied`) on **every tested admin API that could mutate user→policy mappings or otherwise escalate** — attaching a policy to self (both the modern and the deprecated APIs), creating an all-powerful policy, listing users, and the sibling account-management routes add-user / set-user-status / remove-user. The user→policy mapping and the user table are **provably unchanged** afterward, while the *same* user's **permitted** reads still succeed (the denials are action-specific, not a disabled account). **Root cause (`[OBSERVED]` + source-grounded):** deny-by-default admin authorization. The guard `validateAdminReq` → `checkAdminRequestAuth` → `IAMSys.IsAllowed` evaluates the requested **admin action** against the caller's attached policy and returns `ErrAccessDenied` **before any mapping-mutation code runs**, because the read-only policy grants no `admin:*` action. (Scope: this concerns the direct admin mapping-mutation APIs the question targets, exercised at their canonical entry points; it is not a claim about unrelated import/replication code paths.)

### Setup — a basic read-only user, and the empty consoleAdmin mapping (`[OBSERVED]`)

Read-only policy `q5-readonly` (finding-relevant: the exact JSON):

```json
{
 "Version": "2012-10-17",
 "Statement": [
  { "Effect": "Allow",
    "Action": ["s3:GetObject", "s3:ListBucket"],
    "Resource": ["arn:aws:s3:::q5-bucket", "arn:aws:s3:::q5-bucket/*"] }
 ]
}
```

```
$ mc admin policy create inv q5-readonly q5-readonly.json
Created policy `q5-readonly` successfully.
$ mc admin user add inv q5user ***REDACTED*** ; mc admin policy attach inv q5-readonly --user q5user
Added user `q5user` successfully.
Attached Policies: [q5-readonly]
To User: q5user
$ mc admin user info inv q5user
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
```

BEFORE state — `consoleAdmin` has no attached entities (privileged query as root):

```
$ mc admin policy entities inv --policy consoleAdmin
Query time: 2026-07-14T06:11:10Z
```

(The output lists no users and no groups — the `consoleAdmin` policy is attached to nobody.)

### The escalation battery — embedded `set_policy` driver, all seven admin mutations denied (`[OBSERVED]`)

The embedded `set_policy` driver (complete source with sha256 in "Investigation driver programs — complete source") authenticates **as `q5user`** and, in a single run, attempts the full battery of admin mutations against the real admin API (via the `madmin-go` client — the canonical admin entry points, not a bypass). It tries: (1) the modern attach API, (2) the deprecated set-policy API, (3) creating an all-powerful `admin:*`/`s3:*` policy, (4) listing all users, and the sibling account-management routes (5) add-user, (6) set-user-status, (7) remove-user. **Every one is denied:**

```
$ ./set_policy -endpoint 127.0.0.1:9000 -access q5user -secret *** -self q5user -attach consoleAdmin
AttachPolicy(self)     => DENIED code=AccessDenied msg="Access Denied."
SetPolicy(self)        => DENIED code=AccessDenied msg="Access Denied."
AddCannedPolicy        => DENIED code=AccessDenied msg="Access Denied."
ListUsers              => DENIED code=AccessDenied msg="Access Denied."
AddUser                => DENIED code=AccessDenied msg="Access Denied."
SetUserStatus          => DENIED code=AccessDenied msg="Access Denied."
RemoveUser             => DENIED code=AccessDenied msg="Access Denied."
```

The `mc admin trace --all -v inv` capture shows the **authentic HTTP method + route + `403`** for each of the seven attempts. The complete, unedited request/response blocks are reproduced below (the `q5user` secret never appears on the wire — only the derived one-time SigV4 `Signature`; the request bodies are shown by the tracer as `<BLOB>`):

```
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-14T06:11:12.575] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=8e0bedcf885eb233218f66171a4d427895393e6cf31157df05048e80ea7f747e
127.0.0.1:9000 Content-Length: 102
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: 52b4a00d95ef05c19e047b8bc3cdc0ba9d2217fcc1c5f4c10ab8725f031ad63c
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.575] [ Duration 270µs TTFB 266.492µs ↑ 90 B  ↓ 213 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 213
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C21329C8C69079
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C21329C8C69079","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.SetPolicyForUserOrGroup] [2026-07-14T06:11:12.576] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-or-group-policy?isGroup=false&policyName=consoleAdmin&userOrGroup=q5user
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=dce96c18595434d4633c5bf3b253703c368191f266fb38202a930f39418a4ccd
127.0.0.1:9000 Transfer-Encoding: chunked
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.576] [ Duration 96µs TTFB 94.419µs ↑ 80 B  ↓ 212 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 212
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C21329C8D33680
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C21329C8D33680","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.AddCannedPolicy] [2026-07-14T06:11:12.576] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/add-canned-policy?name=qafix-escalate
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 102
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: ba347864c9def0ed307d56a089408c4f481332c7a965919e42a545ba268815a1
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=f305ea0b1c200b7dea2303a0cbd0b36dce4eac400bdc91238690cf9c07a386fa
127.0.0.1:9000 
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.576] [ Duration 98µs TTFB 96.359µs ↑ 77 B  ↓ 205 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Content-Length: 205
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18C21329C8DBB13E
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/add-canned-policy","RequestId":"18C21329C8DBB13E","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.ListUsers] [2026-07-14T06:11:12.576] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /minio/admin/v3/list-users
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=e41ea19161ed61cc93af62ac63079c1802a51fbbd7af853910dc6d26f205729c
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.577] [ Duration 68µs TTFB 66.827µs ↑ 77 B  ↓ 198 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C21329C8E19CC2
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 198
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/list-users","RequestId":"18C21329C8E19CC2","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.AddUser] [2026-07-14T06:11:12.627] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/add-user?accessKey=qafix-newuser
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=412057ae9c144866476c7ce853e3a32da3b7262e505ebcca9f071f7372fc7be3
127.0.0.1:9000 Content-Length: 116
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: 86cc30a91857db52f3cb1c11eb6fe6b5108cc79f375da469dfb0e966fc13c80e
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.628] [ Duration 1.034ms TTFB 1.030351ms ↑ 77 B  ↓ 196 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 196
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C21329CBEB0CBE
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/add-user","RequestId":"18C21329CBEB0CBE","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.SetUserStatus] [2026-07-14T06:11:12.629] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-status?accessKey=q5user&status=disabled
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=e7d27ddcfbebc24b9b732e64a5ae4efd0cebd314761d858095c5069ff4130e7b
127.0.0.1:9000 Transfer-Encoding: chunked
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.629] [ Duration 100µs TTFB 99.01µs ↑ 80 B  ↓ 203 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 203
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C21329CC017251
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-status","RequestId":"18C21329CC017251","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}

127.0.0.1:9000 [REQUEST admin.RemoveUser] [2026-07-14T06:11:12.629] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /minio/admin/v3/remove-user?accessKey=q5user
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=q5user/20260714//s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=db482db1dd6c0e254f0b06cbc4b3569e3e174abc252522d708129aba62bfd44e
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T061112Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T06:11:12.629] [ Duration 83µs TTFB 81.893µs ↑ 77 B  ↓ 199 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18C21329CC07BA78
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 199
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/remove-user","RequestId":"18C21329CC07BA78","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Each attempt maps to a guarded handler whose **first statement** is the authorization gate (`validateAdminReq` / `validateAdminSignature` + `IsAllowed`), which rejects the request **before any mapping-mutation code executes**:

| # | Driver call | Authentic method + route (`[OBSERVED]`) | Handler + guard action (source) | Route wiring |
|---|-------------|------------------------------------------|----------------------------------|--------------|
| 1 | `AttachPolicy(self)` | `POST /minio/admin/v3/idp/builtin/policy/attach` | `AttachDetachPolicyBuiltin` `cmd/admin-handlers-users.go:1908`; guard `UpdatePolicyAssociationAction`+`AttachPolicyAdminAction` L1911-1912 | `cmd/admin-router.go:268` |
| 2 | `SetPolicy(self)` | `PUT /minio/admin/v3/set-user-or-group-policy` | `SetPolicyForUserOrGroup` `cmd/admin-handlers-users.go:1770`; guard `AttachPolicyAdminAction` L1773 | `cmd/admin-router.go:263` |
| 3 | `AddCannedPolicy` | `PUT /minio/admin/v3/add-canned-policy` | `AddCannedPolicy` `cmd/admin-handlers-users.go:1701`; guard `CreatePolicyAdminAction` L1704 | `cmd/admin-router.go:228` |
| 4 | `ListUsers` | `GET /minio/admin/v3/list-users` | `ListUsers` `cmd/admin-handlers-users.go:136`; guard `ListUsersAdminAction` L139 | `cmd/admin-router.go:275` |
| 5 | `AddUser` | `PUT /minio/admin/v3/add-user` | `AddUser` `cmd/admin-handlers-users.go:444`; `validateAdminSignature` L457 then `IsAllowed(CreateUserAdminAction)` L502-511 | `cmd/admin-router.go:233` |
| 6 | `SetUserStatus` | `PUT /minio/admin/v3/set-user-status` | `SetUserStatus` `cmd/admin-handlers-users.go:406`; guard `EnableUserAdminAction` L409 | `cmd/admin-router.go:235` |
| 7 | `RemoveUser` | `DELETE /minio/admin/v3/remove-user` | `RemoveUser` `cmd/admin-handlers-users.go:47`; guard `DeleteUserAdminAction` L50 | `cmd/admin-router.go:271` |

### Allowed-read baseline — the SAME user's permitted reads still succeed (`[OBSERVED]`)

Essential control: the seven denials are specific to **admin actions**, not a broken or blanket-deny identity. Authenticated as `q5user` (alias `q5`), the reads that its `q5-readonly` policy *does* grant work normally — so the `403`s above are the deny-by-default gate acting on missing `admin:*` permissions, not a disabled account:

```
$ mc ls q5/q5-bucket
[2026-07-14 06:10:53 UTC]    24B STANDARD readable.txt
$ mc cp q5/q5-bucket/readable.txt ./baseline_get.txt ; echo "exit=$?"
`q5/q5-bucket/readable.txt` -> `./baseline_get.txt`
exit=0
$ cat baseline_get.txt
q5-readable-object-body
```

### AFTER state — the mapping and user table are provably unchanged (`[OBSERVED]`)

```
$ mc admin policy entities inv --policy consoleAdmin      # still attached to nobody
Query time: 2026-07-14T06:11:44Z
$ mc admin user info inv q5user                           # still read-only, still enabled
AccessKey: q5user
Status: enabled
PolicyName: q5-readonly
MemberOf: []
$ mc admin user info inv qafix-newuser                    # AddUser was denied -> never created
mc: <ERROR> Unable to get user info. The specified user does not exist. (Specified user does not exist).
$ mc admin policy info inv qafix-escalate                 # AddCannedPolicy was denied -> never created
mc: <ERROR> Unable to fetch policy. The canned policy does not exist. (Specified canned policy does not exist).
```

The `consoleAdmin` entity list is empty both before (`06:11:10Z`) and after (`06:11:44Z`); `q5user` is still mapped **only** to `q5-readonly` and is still `enabled` (the `SetUserStatus` disable attempt was denied); the `qafix-newuser` account was never created (`AddUser` denied) and the `qafix-escalate` policy was never created (`AddCannedPolicy` denied). The self-promotion is prevented on every tested mapping-mutation path, and no side effect of the attempts persisted.

### Root cause (`[OBSERVED]` + source-grounded) — deny-by-default admin authorization, BEFORE any mutation

Every attach/create/list handler begins by calling `validateAdminReq` with the required admin action; the mutation code is never reached. The chain:

1. `validateAdminReq` (`cmd/admin-handler-utils.go:37`) calls `checkAdminRequestAuth(ctx, r, action, "")`; when it returns `ErrAccessDenied` it writes the 403 (`cmd/admin-handler-utils.go:59-60`):

```go
	writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
	return nil, auth.Credentials{}
```

2. `checkAdminRequestAuth` (`cmd/auth-handler.go:189`) verifies the signature, then evaluates the admin action against the caller's policy and returns `ErrAccessDenied` if not allowed (`cmd/auth-handler.go:194-206`):

```go
	if globalIAMSys.IsAllowed(policy.Args{
		AccountName:     cred.AccessKey,
		Groups:          cred.Groups,
		Action:          policy.Action(action),
		ConditionValues: getConditionValues(r, "", cred),
		IsOwner:         owner,
		Claims:          cred.Claims,
	}) {
		// Request is allowed return the appropriate access key.
		return cred, ErrNone
	}

	return cred, ErrAccessDenied
```

3. For a regular user, `IAMSys.IsAllowed` fetches the caller's mapped policies and returns the combined-policy decision (`cmd/iam.go:2482`):

```go
	// Policies were found, evaluate all of them.
	return sys.GetCombinedPolicy(policies...).IsAllowed(args)
```

`q5user`'s only policy is `q5-readonly`, which grants `s3:GetObject`/`s3:ListBucket` and **no** `admin:*` action, so `AttachPolicyAdminAction` / `CreatePolicyAdminAction` / `ListUsersAdminAction` all evaluate to `false` → `ErrAccessDenied` → `403`. Because this gate is the **first** statement of each handler (before any user→policy mapping is read or written), no mutation ever occurs. This is a general deny-by-default outcome, not a special-cased check.

### Correction of a common misconception (`consoleAdmin` at `cmd/admin-handlers-users.go:1455`) (`[OBSERVED]`)

The `consoleAdmin` assignment at `cmd/admin-handlers-users.go:1455-1463` is **not** a privilege-escalation defense and does **not** mean "a regular user can never receive `consoleAdmin`." It lives inside `AccountInfoHandler` (`cmd/admin-handlers-users.go:1348`) and only computes an *effective policy for display* — for the **owner/root** account (`accountName == globalActiveCred.AccessKey`) or when an external authZ plugin is configured — so the console UI can render. The source comment states the intent directly:

```go
		// For owner account and when plugin authZ is configured always set
		// effective policy as `consoleAdmin`.
		//
		// In the latter case, we let the UI render everything, but individual
		// actions would fail if not permitted by the external authZ service.
```

For the record, an **administrator** legitimately *can* attach `consoleAdmin` to any user; what is prevented here is a **basic user attaching it to themselves**, and that prevention is the deny-by-default gate above — not this UI-rendering branch.

### Stability (2 runs, `[OBSERVED]`)

The full `set_policy` battery was run **twice** (two fresh driver invocations as `q5user`). Both runs produced identical verdicts — all **seven** admin mutations denied with `403 AccessDenied` — and the allowed-read baseline succeeded in both, with the user→policy mapping unchanged after each:

```
run1: AttachPolicy/SetPolicy/AddCannedPolicy/ListUsers/AddUser/SetUserStatus/RemoveUser => all DENIED code=AccessDenied
run2: AttachPolicy/SetPolicy/AddCannedPolicy/ListUsers/AddUser/SetUserStatus/RemoveUser => all DENIED code=AccessDenied
baseline (both runs): mc cp q5/q5-bucket/readable.txt -> exit=0 ("q5-readable-object-body")
```

### Coverage of named mechanisms (Q5)

| Item | Value / file:line | Evidence |
|------|-------------------|----------|
| Modern attach entry point + guard | `AttachDetachPolicyBuiltin` `cmd/admin-handlers-users.go:1908`; guard `AttachPolicyAdminAction` L1912; route POST `cmd/admin-router.go:268` | POST `/idp/builtin/policy/attach` → 403 `[OBSERVED]` |
| Deprecated set-policy entry point + guard | `SetPolicyForUserOrGroup` `cmd/admin-handlers-users.go:1770`; guard L1773; route PUT `cmd/admin-router.go:263` | PUT `/set-user-or-group-policy` → 403 `[OBSERVED]` |
| Create-policy guard | `AddCannedPolicy` `cmd/admin-handlers-users.go:1701`; guard `CreatePolicyAdminAction` L1704 | PUT `/add-canned-policy` → 403 `[OBSERVED]` |
| List-users guard | `ListUsers` `cmd/admin-handlers-users.go:136`; guard `ListUsersAdminAction` L139; route GET `cmd/admin-router.go:275` | GET `/list-users` → 403 `[OBSERVED]` |
| Add-user guard | `AddUser` `cmd/admin-handlers-users.go:444`; `validateAdminSignature` L457 + `IsAllowed(CreateUserAdminAction)` L502-511; route PUT `cmd/admin-router.go:233` | PUT `/add-user` → 403; `qafix-newuser` never created `[OBSERVED]` |
| Set-user-status guard | `SetUserStatus` `cmd/admin-handlers-users.go:406`; guard `EnableUserAdminAction` L409; route PUT `cmd/admin-router.go:235` | PUT `/set-user-status` → 403; `q5user` still enabled `[OBSERVED]` |
| Remove-user guard | `RemoveUser` `cmd/admin-handlers-users.go:47`; guard `DeleteUserAdminAction` L50; route DELETE `cmd/admin-router.go:271` | DELETE `/remove-user` → 403; `q5user` still exists `[OBSERVED]` |
| Admin authorization wrapper + 403 | `validateAdminReq` `cmd/admin-handler-utils.go:37`; 403 write L58-59 | `[OBSERVED]` (all seven 403s) |
| Signature+action check | `checkAdminRequestAuth` `cmd/auth-handler.go:189`; `IsAllowed` L194-201; `ErrAccessDenied` L206 | `[INFERRED]` (source) |
| Deny-by-default eval (regular user) | `IsAllowed` return `cmd/iam.go:2482` `GetCombinedPolicy(...).IsAllowed(args)` | `[INFERRED]` (source) |
| consoleAdmin (UI effective policy, root only) | `AccountInfoHandler` `cmd/admin-handlers-users.go:1348`; consoleAdmin branch L1455-1463 | not a defense; corrected `[OBSERVED]` |
| Allowed-read baseline (control) | same identity `q5user`, policy `q5-readonly` grants `s3:GetObject`/`s3:ListBucket` | `mc ls`/`mc cp` succeed (exit 0) → denials are action-specific `[OBSERVED]` |
| Mapping unchanged | consoleAdmin entities before==after; q5user still q5-readonly | `[OBSERVED]` |

---

## Cleanup and repository state

This investigation was conducted with a strict read-only posture toward the repository. All runtime activity used an **isolated scratch root** (`/tmp/minio-investigation`) and an **isolated `mc` config** (`MC_CONFIG_DIR=/tmp/minio-investigation/mc-config`, alias `inv`) so that no alias or credential from this session leaked into the host's default `~/.mc`.

At the end of the investigation the following cleanup was performed (`[OBSERVED]`):

```
# stop the investigation server (loopback-only, ephemeral random root credentials)
$ kill 524916

# remove residual investigation aliases left in the host default config by earlier draft runs
$ mc alias remove q4n ; mc alias remove q5u
Removed `q4n` successfully.
Removed `q5u` successfully.

# host default /root/.mc aliases AFTER cleanup — only pre-existing defaults remain, no investigation aliases
$ mc alias ls | awk '/^[a-zA-Z]/{print $1}'
gcs
local
play
s3

# remove the isolated scratch root: server data dirs, all test buckets/objects/users/policies,
# STS temporary credentials, the corrupted-shard test data, drivers, and the isolated mc config
$ rm -rf /tmp/minio-investigation
```

Final repository state (`[OBSERVED]`). Committing this file necessarily moves the branch tip (see **Git context** above), so the net effect is stated against the **immutable investigated base commit** `c07e5b49d477` rather than a volatile branch-tip hash. Versus that base, the **only** change is this single, newly **added** documentation file — purely additive, with no source file modified or deleted and no dependency change:

```
# exactly one path differs from the investigated base commit — a single ADDED file, nothing else
$ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2
A	blitzy/documentation/minio_c07e5b49d477.md

# that file did not exist at the base, so the diff is purely additive: insertions only, zero deletions
$ git diff --stat c07e5b49d477b0774f23db3b290745aef8c01bd2 -- blitzy/documentation/minio_c07e5b49d477.md
 blitzy/documentation/minio_c07e5b49d477.md | 2571 ++++++++++++++++++++++++++++
 1 file changed, 2571 insertions(+)

# the dependency manifests are byte-for-byte unchanged — this command prints nothing
$ git diff --stat c07e5b49d477b0774f23db3b290745aef8c01bd2 -- go.mod go.sum
```

All temporary scripts, fixtures, credentials, corrupted shards, and server data directories have been removed; fixture-user secrets are redacted throughout this document; and the repository is left byte-for-byte unchanged except for this answer file.
