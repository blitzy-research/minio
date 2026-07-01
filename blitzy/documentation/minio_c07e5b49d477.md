# MinIO first‑bucket walkthrough — a runtime Q&A for a fresh single‑node deployment

This document answers, end‑to‑end and **from direct observation of a running server**, what a freshly provisioned single‑node MinIO deployment actually does when you use it for real object operations. It is not a "the server starts" confirmation: every behavioral statement below is immediately followed by the exact command that produced it and the verbatim output line that demonstrates it (HTTP status, response headers, response body, server log line, on‑disk bytes, measured size/timing/count), and every system claim is grounded in a `file:line` reference into the source tree at commit `c07e5b49d477b0774f23db3b290745aef8c01bd2`.

The user's example flow is preserved verbatim as the spine of the investigation:

> *"What does a full flow look like where you create a new bucket, upload at least two different objects to it, list them, and then download one of them again?"*

**How the server was built, run, and observed (methodology).** A single‑node single‑drive (SNSD) server was built from source with the repository‑pinned Go toolchain and run against a scratch data directory under `/tmp`, using the documented default credentials and ports. The live endpoint was then driven with three independent S3 clients so that each claim can be cross‑checked:

- `curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin"` — for raw HTTP status lines, full response headers, and exact XML/byte bodies. Empty‑body requests add `-H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"` (the SHA‑256 of the empty string).
- `boto3` (SigV4, path‑style addressing) — for structured status code / header / parsed‑body confirmation.
- `mc admin trace` (single‑line) and `mc admin trace -v` (verbose `REQUEST`/`RESPONSE`) — for **timestamped per‑request** server logs, because (as Section 4 shows with evidence) the default console log is quiet on the success path.

**The two objects are deliberately different** — distinct keys, sizes, content types, and ETags — to satisfy the "at least two different objects" requirement:

| Object key | Exact bytes | Size | `Content-Type` | ETag (MD5 of bytes) |
|------------|-------------|------|----------------|---------------------|
| `hello.txt` | `Hello MinIO first bucket!\n` | 26 B | `text/plain` | `3bb51064cf13d3be5e710394962bbeda` |
| `data/report.json` | `{"report": "weekly"}\n` | 21 B | `application/json` | `001b1ab535a1cbd53bef9e4933aca2be` |

An S3 `ETag` for a single‑part upload is the deterministic MD5 of the uploaded bytes; both were verified independently before upload:

```console
$ printf 'Hello MinIO first bucket!\n' | md5sum
3bb51064cf13d3be5e710394962bbeda  -
$ printf '{"report": "weekly"}\n' | md5sum
001b1ab535a1cbd53bef9e4933aca2be  -
```

> Note on per‑run values: identifiers such as `X-Amz-Request-Id`, `X-Amz-Id-2`, `Date`, `Last-Modified`, trace timestamps, and `Duration`/`TTFB` differ on every run. The values pasted below are the ones this run actually produced; the deterministic values (ETags, XML structure, error `Code`/`Message`, `format.json` marker, `xl.meta` magic bytes, byte sizes, object bodies) are stable and reproducible.

---

## Section 1 — Local setup & startup

**What was done.** The server binary was compiled from source with the pinned Go toolchain (`go 1.23` [go.mod:3], resolved to `go1.23.12`) and launched in single‑node single‑drive mode against a scratch directory, using the documented defaults. The process entry point is `func main()` [main.go:29], which calls `minio.Main(os.Args)`; that dispatches into `func Main(args []string)` [cmd/main.go:201]. The startup banner and the default‑credentials warning are emitted from `serverMain()` — specifically the warning is appended when the active credentials equal the built‑in defaults [cmd/server-main.go:974-975].

**Build.** The `Makefile` `build` target compiles with `CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio` [Makefile:177-179]; a self‑contained build is sufficient for the investigation:

```console
$ CGO_ENABLED=0 GOFLAGS=-mod=mod go build -o /tmp/minio .
$ /tmp/minio --version
minio version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
```

The build produced a 156,743,666‑byte static binary in ~4.9 s. The `go1.23.12 linux/amd64` runtime confirms the pinned `go 1.23` [go.mod:3] line is satisfied.

**Run (SNSD).** The documented run form is `minio server /data` [README.md:51]; the S3 API listens on port 9000 by default [README.md:143]; the Web Console is exposed via `--console-address ":9001"` [README.md:25-26]; the default root credentials are `minioadmin:minioadmin` [README.md:29]:

```console
$ MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    /tmp/minio server /tmp/minio-data --address :9000 --console-address :9001 \
    > /tmp/minio-server.log 2>&1 &
```

**Startup banner (verbatim, from `/tmp/minio-server.log`).** The console log carries **no per‑line timestamps** — it is a formatted banner, not a per‑request log stream (this becomes important in Section 4):

```text
INFO: Formatting 1st pool, 1 set(s), 1 drives per set.
INFO: WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)

API: http://10.236.7.33:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
WebUI: http://10.236.7.33:9001 http://172.17.0.1:9001 http://127.0.0.1:9001

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

The `WARN: Detected default credentials 'minioadmin:minioadmin'...` line is exactly the message constructed at [cmd/server-main.go:974-975]. That there are no per‑line timestamps was confirmed directly:

```console
$ grep -cE '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}' /tmp/minio-server.log
0
```

**Readiness probe.** The server reports live/ready on its health endpoint:

```console
$ curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:9000/minio/health/live
200
```

**Why / rationale.** A local dev deployment is just the compiled server pointed at a directory with the default identity; the health endpoint returning `200` is the minimal proof the API is accepting connections, and the banner both identifies the exact build (`go1.23.12`) and warns that the default credentials are in use — which is precisely the identity that governs the authorization behavior in Section 5.

---

## Section 2 — The four‑step first‑bucket flow

This section walks the user's verbatim example — *"create a new bucket, upload at least two different objects to it, list them, and then download one of them again"* — giving the **status code, response headers, and body** for each step. The handlers are: CreateBucket → `PutBucketHandler()` [cmd/bucket-handlers.go:723]; PutObject → `PutObjectHandler()` [cmd/object-handlers.go:1745]; ListObjectsV2 → `ListObjectsV2Handler()` [cmd/bucket-listobjects-handlers.go:154]; GetObject → `GetObjectHandler()` [cmd/object-handlers.go:715].

### Step 1 — CreateBucket (`PUT /first-bucket`) → `200 OK`

```console
$ curl -i -X PUT "http://127.0.0.1:9000/first-bucket" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
Location: /first-bucket
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE49AE12E672E2
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1148343
X-Ratelimit-Remaining: 1148343
X-Xss-Protection: 1; mode=block
Date: Wed, 01 Jul 2026 22:05:14 GMT
```

- **Status:** `200 OK`. **Body:** empty (`Content-Length: 0`). **Key header:** `Location: /first-bucket` identifies the created bucket. `X-Amz-Request-Id`/`X-Amz-Id-2` are per‑request identifiers attached by the middleware (Section 6).

### Step 2 — PutObject `hello.txt` (`PUT /first-bucket/hello.txt`) → `200 OK`

```console
$ curl -i -X PUT "http://127.0.0.1:9000/first-bucket/hello.txt" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "Content-Type: text/plain" \
    -H "x-amz-content-sha256: 692afb37ddd07c5acb376b6db1b6c71b687ec2479d277a231a105d94f0090c4a" \
    --data-binary @hello.txt
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
ETag: "3bb51064cf13d3be5e710394962bbeda"
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE49AE150707C1
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1148343
X-Ratelimit-Remaining: 1148343
X-Xss-Protection: 1; mode=block
Date: Wed, 01 Jul 2026 22:05:14 GMT
```

- **Status:** `200 OK`. **Body:** empty. **Key header:** `ETag: "3bb51064cf13d3be5e710394962bbeda"` — the MD5 of the 26 uploaded bytes, exactly the value verified in the intro.

### Step 3 — PutObject `data/report.json` (a *different* object) → `200 OK`

```console
$ curl -i -X PUT "http://127.0.0.1:9000/first-bucket/data/report.json" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "Content-Type: application/json" \
    -H "x-amz-content-sha256: cce5d85e615aa8ba608568a366e402b4a7b8024176dd00a147fdbe3d7387e421" \
    --data-binary @report.json
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
ETag: "001b1ab535a1cbd53bef9e4933aca2be"
Server: MinIO
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE49AFFB914FFB
Date: Wed, 01 Jul 2026 22:05:22 GMT
```

- **Status:** `200 OK`. **Body:** empty. **Key header:** `ETag: "001b1ab535a1cbd53bef9e4933aca2be"`.
- **The two objects are demonstrably different:** keys `hello.txt` vs `data/report.json`; sizes 26 B vs 21 B; content types `text/plain` vs `application/json`; ETags `3bb51064cf13d3be5e710394962bbeda` vs `001b1ab535a1cbd53bef9e4933aca2be`.

### Step 4a — ListObjectsV2 (`GET /first-bucket?list-type=2`) → `200 OK`

```console
$ curl -i "http://127.0.0.1:9000/first-bucket?list-type=2" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 652
Content-Type: application/xml
Server: MinIO
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE49AFFC369EE4
Date: Wed, 01 Jul 2026 22:05:22 GMT

<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>first-bucket</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>data/report.json</Key><LastModified>2026-07-01T22:05:22.674Z</LastModified><ETag>&#34;001b1ab535a1cbd53bef9e4933aca2be&#34;</ETag><Size>21</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-07-01T22:05:14.511Z</LastModified><ETag>&#34;3bb51064cf13d3be5e710394962bbeda&#34;</ETag><Size>26</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

- **Status:** `200 OK`. **`Content-Type: application/xml`**, `Content-Length: 652`. The body reports `<KeyCount>2</KeyCount>`, `<MaxKeys>1000</MaxKeys>`, `<IsTruncated>false</IsTruncated>`, and both keys with `<StorageClass>STANDARD</StorageClass>`. Note the ETags are XML‑escaped as `&#34;...&#34;` (the escaped double‑quote).

### Step 4b — ListObjectsV2 confirmed by `boto3`

```console
$ python3 - <<'PY'
import boto3; from botocore.config import Config
s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
    aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin",
    region_name="us-east-1", config=Config(s3={"addressing_style":"path"}))
lo = s3.list_objects_v2(Bucket="first-bucket")
print("HTTPStatusCode:", lo["ResponseMetadata"]["HTTPStatusCode"])
print("content-type:", lo["ResponseMetadata"]["HTTPHeaders"]["content-type"])
print("KeyCount:", lo["KeyCount"], "IsTruncated:", lo["IsTruncated"])
for o in lo["Contents"]:
    print(" ", o["Key"], o["Size"], o["ETag"], o["StorageClass"])
PY
HTTPStatusCode: 200
content-type: application/xml
KeyCount: 2 IsTruncated: False
  data/report.json 21 "001b1ab535a1cbd53bef9e4933aca2be" STANDARD
  hello.txt 26 "3bb51064cf13d3be5e710394962bbeda" STANDARD
```

### Step 5 — GetObject `hello.txt` (download it again) → `200 OK`

```console
$ curl -i "http://127.0.0.1:9000/first-bucket/hello.txt" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 26
Content-Type: text/plain
ETag: "3bb51064cf13d3be5e710394962bbeda"
Last-Modified: Wed, 01 Jul 2026 22:05:14 GMT
Server: MinIO
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18BE49B1F2DE34E6
X-Content-Type-Options: nosniff
Date: Wed, 01 Jul 2026 22:05:31 GMT

Hello MinIO first bucket!
```

- **Status:** `200 OK`. **`Content-Type: text/plain`**, **`Content-Length: 26`**, `ETag: "3bb51064cf13d3be5e710394962bbeda"` (matches the upload), and a `Last-Modified` timestamp. The body is exactly `Hello MinIO first bucket!` followed by a newline — 26 bytes total. `boto3` confirms the byte‑exact body:

```console
GetObject HTTPStatusCode: 200
GetObject ContentType: text/plain ContentLength: 26 ETag: "3bb51064cf13d3be5e710394962bbeda"
GetObject body repr: b'Hello MinIO first bucket!\n'
```

**Why / rationale.** Each of the four user‑named operations maps to a distinct handler and a distinct response shape: create returns `200` + a `Location` header and no body; each write returns `200` + an `ETag` (the content MD5) and no body; the listing returns `200` + an XML `ListBucketResult`; and the download returns `200` + the raw object bytes with the object's own `Content-Type` and `Content-Length`. The `ETag` on GET matching the `ETag` from PUT is the end‑to‑end proof the stored bytes are the uploaded bytes.

---


## Section 3 — Response‑body examples (what each body actually looks like)

The flow exercises five distinct body shapes. Each is shown verbatim.

**(a) CreateBucket — empty body + `Location` header.** There is no XML payload; the result is signaled entirely by the `200` status and the `Location: /first-bucket` header (see Step 1). `Content-Length: 0`.

**(b) PutObject — empty body + `ETag` header.** Writes return no payload; the result is the `ETag` header carrying the content MD5 (see Steps 2 and 3), e.g. `ETag: "3bb51064cf13d3be5e710394962bbeda"`. `Content-Length: 0`.

**(c) ListObjectsV2 — `application/xml` `<ListBucketResult>`.** The full body (from Step 4a):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>first-bucket</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>data/report.json</Key><LastModified>2026-07-01T22:05:22.674Z</LastModified><ETag>&#34;001b1ab535a1cbd53bef9e4933aca2be&#34;</ETag><Size>21</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-07-01T22:05:14.511Z</LastModified><ETag>&#34;3bb51064cf13d3be5e710394962bbeda&#34;</ETag><Size>26</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

**(d) GetObject — raw object bytes + object headers.** The body is the stored bytes themselves, not XML (from Step 5):

```text
Hello MinIO first bucket!
```

delivered with `Content-Type: text/plain` and `Content-Length: 26`.

**(e) ListBuckets (`GET /`) — `application/xml` `<ListAllMyBucketsResult>`.** The service‑level listing is handled by `ListBucketsHandler()` [cmd/bucket-handlers.go:306]:

```console
$ curl -i "http://127.0.0.1:9000/" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 200 OK
Content-Length: 370
Content-Type: application/xml
Server: MinIO
X-Amz-Request-Id: 18BE49B1F3554F62
Date: Wed, 01 Jul 2026 22:05:31 GMT

<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>02d6176db174dc93cb1b899f7c6078f08654445fe8cf1b6ce98d8855f66bdbf4</ID><DisplayName>minio</DisplayName></Owner><Buckets><Bucket><Name>first-bucket</Name><CreationDate>2026-07-01T22:05:14.476Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>
```

- **Status:** `200 OK`, `Content-Type: application/xml`, `Content-Length: 370`. The body carries the `<Owner>` (`DisplayName>minio`) and one `<Bucket>` with `<Name>first-bucket</Name>` and its `<CreationDate>2026-07-01T22:05:14.476Z</CreationDate>` — the same timestamp that will be checked for persistence in Section 8.

**Why / rationale.** S3 splits results across three channels: HTTP status, response headers, and body. Bucket‑level and list operations return descriptive XML documents; object writes return their result in a header (`ETag`) with an empty body to avoid echoing payloads; and object reads stream the raw bytes with the object's own metadata headers. Recognizing which channel carries the answer for each verb is the core of reading S3 responses correctly.

---


## Section 4 — Timestamped server logs & the console‑vs‑trace distinction

This is the one place where a naïve expectation is wrong, so it is stated plainly with evidence: **the default console log is quiet on the success path.** After a full flow, the console log for the (restarted) server contained only its ~10‑line startup banner — no per‑request lines at all:

```console
$ wc -l < /tmp/minio-server2.log
10
$ grep -cE 's3\.(PutBucket|PutObject|GetObject|ListObjectsV2|ListBuckets)|^(GET|PUT) /' /tmp/minio-server2.log
0
$ grep -cE '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}' /tmp/minio-server2.log
0
```

So the timestamped, per‑request "received → completed" lines the question asks for come **only** from `mc admin trace` — they are *not* fabricated console lines.

**Single‑line trace (`mc admin trace --all local`).** Each line carries a timestamp, the result code, the operation, the target, the client IP, the total duration, the call time (`⇣`), and the uploaded/downloaded byte counts (`↑`/`↓`). The six success‑path operations of the flow:

```text
2026-07-01T22:05:14.475 [200 OK] s3.PutBucket 127.0.0.1:9000/first-bucket 127.0.0.1        27.739ms     ⇣  27.687253ms  ↑ 84 B ↓ 0 B
2026-07-01T22:05:14.511 [200 OK] s3.PutObject 127.0.0.1:9000/first-bucket/hello.txt 127.0.0.1        22.208ms     ⇣  22.166248ms  ↑ 123 B ↓ 0 B
2026-07-01T22:05:22.674 [200 OK] s3.PutObject 127.0.0.1:9000/first-bucket/data/report.json 127.0.0.1        3.71ms       ⇣  3.683119ms  ↑ 118 B ↓ 0 B
2026-07-01T22:05:22.684 [200 OK] s3.ListObjectsV2 127.0.0.1:9000/first-bucket?list-type=2  127.0.0.1        862µs       ⇣  845.507µs  ↑ 84 B ↓ 652 B
2026-07-01T22:05:31.117 [200 OK] s3.GetObject 127.0.0.1:9000/first-bucket/hello.txt 127.0.0.1        782µs       ⇣  741.933µs  ↑ 84 B ↓ 26 B
2026-07-01T22:05:31.125 [200 OK] s3.ListBuckets 127.0.0.1:9000/ 127.0.0.1        294µs       ⇣  283.066µs  ↑ 84 B ↓ 370 B
```

Note the downloaded byte counts line up exactly with the HTTP `Content-Length` values in Section 2: ListObjectsV2 `↓ 652 B`, GetObject `↓ 26 B`, ListBuckets `↓ 370 B`.

**Verbose trace (`mc admin trace -v --all local`).** The verbose mode emits a paired `[REQUEST]`/`[RESPONSE]` block that includes the signed `Authorization` header. Captured verbatim (each line is prefixed by `mc` with the node address `127.0.0.1:9000`):

```text
127.0.0.1:9000 [REQUEST s3.ListObjectsV2] [2026-07-01T22:06:43.770] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /first-bucket?list-type=2
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: curl/8.14.1
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260701T220643Z
127.0.0.1:9000 Accept: */*
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260701/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=64f26c690ce25cac53f1f212536ff6fb6ea7f4d8069b3f73834c9905f35cd3af
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-01T22:06:43.770] [ Duration 803µs TTFB 788.708µs ↑ 84 B  ↓ 652 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18BE49C2DD43EB20
127.0.0.1:9000 X-Ratelimit-Remaining: 1148343
127.0.0.1:9000 Content-Length: 652
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
```

**Why / rationale.** MinIO's console logger prints a startup banner and (by default) only surfaces errors/warnings, not a success‑path access log; that is why the count of per‑request console lines is `0`. Operator‑facing, timestamped request visibility is provided by the admin trace subsystem and consumed with `mc admin trace`. Reporting this honestly — rather than inventing per‑request console lines — is required by the "report exactly what is observed" discipline.

---


## Section 5 — Authorization checks in the default local setup

In the default local setup there is a single root identity (`minioadmin`) and **no bucket policies**, so authorization is effectively owner‑only: correctly signed requests from the root identity succeed, and everything else is denied.

The S3 API requires **AWS Signature Version 4**. The signing algorithm literal is `signV4Algorithm = "AWS4-HMAC-SHA256"` [cmd/signature-v4.go:48]. Each request's auth type is classified by `checkRequestAuthType()` [cmd/auth-handler.go:339] and validated by `isReqAuthenticated()` [cmd/auth-handler.go:560]; a signature mismatch resolves to `ErrSignatureDoesNotMatch` [cmd/signature-v4.go:200,295].

### Success indicator (owner, correctly signed) → `200 OK`

The proof of a successful authorization is the signed `Authorization` header (from the verbose trace in Section 4) leading to a `200`:

```text
Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260701/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=64f26c690ce25cac53f1f212536ff6fb6ea7f4d8069b3f73834c9905f35cd3af
...
[RESPONSE] ... 200 OK
```

The `Credential=minioadmin/20260701/us-east-1/s3/aws4_request` scope shows the request is signed by the root identity for the `s3` service in `us-east-1`.

### Failure 1 — bad secret key → `403 Forbidden`, `SignatureDoesNotMatch`

```console
$ curl -i "http://127.0.0.1:9000/first-bucket?list-type=2" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:WRONGSECRET" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 403 Forbidden
Content-Length: 411
Content-Type: application/xml
X-Amz-Request-Id: 18BE49B70E5292C2
...

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message><BucketName>first-bucket</BucketName><Resource>/first-bucket</Resource><RequestId>18BE49B70E5292C2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The XML `<Code>SignatureDoesNotMatch</Code>` and `<Message>The request signature we calculated does not match the signature you provided. Check your key and signing method.</Message>` at HTTP `403` match the error‑table definition exactly (`Code: "SignatureDoesNotMatch"`, `HTTPStatusCode: http.StatusForbidden`) [cmd/api-errors.go:704].

### Failure 2 — anonymous / unsigned request → `403 Forbidden`, `AccessDenied`

```console
$ curl -i "http://127.0.0.1:9000/first-bucket/hello.txt"
HTTP/1.1 403 Forbidden
Content-Length: 333
Content-Type: application/xml
X-Amz-Request-Id: 18BE49B70EB44BF0
...

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>hello.txt</Key><BucketName>first-bucket</BucketName><Resource>/first-bucket/hello.txt</Resource><RequestId>18BE49B70EB44BF0</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The XML `<Code>AccessDenied</Code>` and `<Message>Access Denied.</Message>` at HTTP `403` match the error‑table definition exactly (`Code: "AccessDenied"`, `Description: "Access Denied."`, `HTTPStatusCode: http.StatusForbidden`) [cmd/api-errors.go:539]. The single‑line trace shows both denials server‑side:

```text
2026-07-01T22:05:53.053 [403 Forbidden] s3.ListObjectsV2 127.0.0.1:9000/first-bucket?list-type=2  127.0.0.1        204µs       ⇣  190.233µs  ↑ 84 B ↓ 411 B
2026-07-01T22:05:53.059 [403 Forbidden] s3.GetObject 127.0.0.1:9000/first-bucket/hello.txt 127.0.0.1        420µs       ⇣  389.112µs  ↑ 38 B ↓ 333 B
```

**Why / rationale.** With one root identity and no anonymous bucket policy, the only path to a `200` is a correct SigV4 signature from that identity. A wrong secret fails the HMAC comparison (`ErrSignatureDoesNotMatch`), and a request with no credentials is treated as anonymous, for which no policy grants access (`ErrAccessDenied`) — both surface as `403` with the exact `Code`/`Message` defined in `cmd/api-errors.go`.

---


## Section 6 — How the system processes requests and persists data

Requests are routed by `registerAPIRouter()` [cmd/api-router.go:253], which maps each S3 verb/path to its handler and wraps each one in `s3APIMiddleware`. The middleware attaches security headers and the per‑request identifiers: `X-Amz-Request-Id` is set in `setRequestHeaderFunc` [cmd/generic-handlers.go:548], and the header name constants are `AmzRequestID = "x-amz-request-id"` and `AmzRequestHostID = "x-amz-id-2"` [internal/http/headers.go:160-161] — visible on every response in Section 2.

Using the verbose and single‑line traces, the specific lines that answer "which log line shows X" are:

**Request received.** The verbose `[REQUEST]` header line marks the moment a request is accepted and classified:

```text
[REQUEST s3.ListObjectsV2] [2026-07-01T22:06:43.770] [Client IP: 127.0.0.1]
GET /first-bucket?list-type=2
```

**Operation completed.** The paired `[RESPONSE]` line (verbose) — or the single‑line `[200 OK] s3.<Op>` — marks completion, with duration and time‑to‑first‑byte:

```text
[RESPONSE] [2026-07-01T22:06:43.770] [ Duration 803µs TTFB 788.708µs ↑ 84 B  ↓ 652 B ]
200 OK
```

**Data written (PutObject).** MinIO writes to a temporary staging path and then atomically renames it into place. For `data/report.json` the write path is, in order:

```text
2026-07-01T22:05:22.674 [OS] os.OpenFileW 127.0.0.1:9000 /tmp/minio-data/.minio.sys/tmp/60654976-f166-46e2-b1f5-abdcd9b02765/xl.meta 28.244µs
2026-07-01T22:05:22.677 [OS] os.Rename 127.0.0.1:9000 /tmp/minio-data/.minio.sys/tmp/60654976-f166-46e2-b1f5-abdcd9b02765/xl.meta -> /tmp/minio-data/first-bucket/data/report.json/xl.meta 32.386µs
2026-07-01T22:05:22.674 [STORAGE] storage.RenameData 127.0.0.1:9000 /tmp/minio-data 60654976-f166-46e2-b1f5-abdcd9b02765 80a6f775-ba57-4e7e-b085-b08136e1a7ea first-bucket data/report.json 3.265344ms
```

The `os.OpenFileW` opens the staging `xl.meta`, the `os.Rename` moves it to `first-bucket/data/report.json/xl.meta`, and `storage.RenameData` is the storage‑layer operation that names the object `first-bucket data/report.json`. Bucket creation itself is a `storage.MakeVol`:

```text
2026-07-01T22:05:14.475 [STORAGE] storage.MakeVol 127.0.0.1:9000 /tmp/minio-data first-bucket 107.943µs
```

**Data read (GetObject).** The read path opens the object's `xl.meta` and reads the XL metadata:

```text
2026-07-01T22:05:31.118 [OS] os.OpenFileR 127.0.0.1:9000 /tmp/minio-data/first-bucket/hello.txt/xl.meta 35.203µs
2026-07-01T22:05:31.118 [STORAGE] storage.ReadXL 127.0.0.1:9000 /tmp/minio-data first-bucket hello.txt 71.679µs 437 B
```

`storage.ReadXL ... first-bucket hello.txt ... 437 B` shows the server read the 437‑byte `xl.meta` for `hello.txt` (the on‑disk size confirmed in Section 7), from which the inlined body is served.

**Why / rationale.** Every request flows router → `s3APIMiddleware` (which stamps `X-Amz-Request-Id`/`X-Amz-Id-2`) → auth → handler → `xl-storage` backend. Writes use write‑to‑temp‑then‑atomic‑rename (`os.OpenFileW` → `os.Rename` / `storage.RenameData`) so a partially written object is never visible; reads go straight to the object's `xl.meta` (`os.OpenFileR` / `storage.ReadXL`). The trace lines are the direct, timestamped evidence of each of these stages.

---


## Section 7 — On‑disk artifacts that prove buckets and objects are stored

**The backend is erasure, even for one drive.** The data directory's format marker is `xl-single`:

```console
$ cat /tmp/minio-data/.minio.sys/format.json
{"version":"1","format":"xl-single","id":"4330edec-d436-4abd-bc11-e26aa314b0a5","xl":{"version":"3","this":"73bd93f7-8d3e-4068-8038-dc5e3960f551","sets":[["73bd93f7-8d3e-4068-8038-dc5e3960f551"]],"distributionAlgo":"SIPMOD+PARITY"}}
```

`"format":"xl-single"` is exactly the constant `formatBackendErasureSingle = "xl-single"` [cmd/format-erasure.go:43] — so a single‑drive dev setup uses the erasure (xl) backend, not a legacy filesystem backend.

**Each object is a directory containing an `xl.meta` file.** The metadata filename literal is `xlStorageFormatFile = "xl.meta"` [cmd/xl-storage.go:68]:

```console
$ find /tmp/minio-data/first-bucket -type f | sort
/tmp/minio-data/first-bucket/data/report.json/xl.meta
/tmp/minio-data/first-bucket/hello.txt/xl.meta
```

So the object key `hello.txt` is stored at `first-bucket/hello.txt/xl.meta`, and `data/report.json` at `first-bucket/data/report.json/xl.meta` (the `/` in the key becomes a real subdirectory). Server‑internal state lives under `.minio.sys`:

```console
$ find /tmp/minio-data/.minio.sys -maxdepth 2 | sort
/tmp/minio-data/.minio.sys
/tmp/minio-data/.minio.sys/buckets
/tmp/minio-data/.minio.sys/buckets/.bloomcycle.bin
/tmp/minio-data/.minio.sys/buckets/.usage-cache.bin
/tmp/minio-data/.minio.sys/buckets/.usage-cache.bin.bkp
/tmp/minio-data/.minio.sys/buckets/.usage.json
/tmp/minio-data/.minio.sys/buckets/first-bucket
/tmp/minio-data/.minio.sys/config
/tmp/minio-data/.minio.sys/config/config.json
/tmp/minio-data/.minio.sys/config/iam
/tmp/minio-data/.minio.sys/format.json
/tmp/minio-data/.minio.sys/multipart
/tmp/minio-data/.minio.sys/pool.bin
/tmp/minio-data/.minio.sys/pool.bin/xl.meta
/tmp/minio-data/.minio.sys/tmp
/tmp/minio-data/.minio.sys/tmp/.trash
```

**`xl.meta` magic bytes.** The first four bytes are the XL v2 header:

```console
$ head -c 8 /tmp/minio-data/first-bucket/hello.txt/xl.meta | od -An -tx1
 58 4c 32 20 01 00 03 00
$ head -c 4 /tmp/minio-data/first-bucket/hello.txt/xl.meta | od -An -c
   X   L   2
```

`58 4c 32 20` is ASCII `XL2 ` (the fourth byte `0x20` is a space) — the header `xlHeader = [4]byte{'X', 'L', '2', ' '}` [cmd/xl-storage-format-v2.go:44].

**Small object bodies are inlined into `xl.meta` (no separate part file).** The object bytes are embedded inside the metadata file:

```console
$ strings /tmp/minio-data/first-bucket/hello.txt/xl.meta | grep -o 'Hello MinIO first bucket!'
Hello MinIO first bucket!
$ strings /tmp/minio-data/first-bucket/data/report.json/xl.meta | grep -o '{"report": "weekly"}'
{"report": "weekly"}
$ wc -c < /tmp/minio-data/first-bucket/hello.txt/xl.meta;       ls -1 /tmp/minio-data/first-bucket/hello.txt/
437
xl.meta
$ wc -c < /tmp/minio-data/first-bucket/data/report.json/xl.meta; ls -1 /tmp/minio-data/first-bucket/data/report.json/
438
xl.meta
$ find /tmp/minio-data/first-bucket -name 'part.*' | wc -l
0
```

For this run the `hello.txt` `xl.meta` is **437 bytes** (matching the `storage.ReadXL ... 437 B` trace line in Section 6) and the `report.json` `xl.meta` is **438 bytes**; each object directory contains **only** `xl.meta` and there are **zero** `part.*` files — confirming the 26‑ and 21‑byte bodies are stored inline rather than as separate part files.

**Why / rationale.** These artifacts are the durable proof that the operations actually persisted data: the bucket became a top‑level directory (`storage.MakeVol` in Section 6), each object became a `<key>/xl.meta` directory/file, and the `format.json` marker (`xl-single`) shows the erasure backend is in use even on one drive. Because the bodies here are small, MinIO inlines them into `xl.meta` (which is why `strings` finds the literal bytes and there is no `part.1`).

---


## Section 8 — Persistence across a server restart

**Clean shutdown.** Sending `SIGTERM` causes the process to exit cleanly, logging the signal:

```console
$ kill -TERM <server-pid>
$ tail -1 /tmp/minio-server.log
INFO: Exiting on signal: TERMINATED
```

**Restart against the same data directory.** The server is relaunched with the same `/tmp/minio-data`. Its console log this time has **no** `INFO: Formatting ...` line (the drive is already formatted, so it loads the existing `format.json`), and readiness returns `200`:

```console
$ curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:9000/minio/health/live
200
```

**Re‑verify — everything is identical to before the restart.**

*Bucket creation timestamp preserved* (compare with Section 3's `2026-07-01T22:05:14.476Z`):

```console
$ curl -s "http://127.0.0.1:9000/" --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" \
  | grep -o '<CreationDate>[^<]*</CreationDate>'
<CreationDate>2026-07-01T22:05:14.476Z</CreationDate>
```

*Both objects still listed with identical ETags and sizes:*

```console
$ curl -s "http://127.0.0.1:9000/first-bucket?list-type=2" --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>first-bucket</Name><Prefix></Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>data/report.json</Key><LastModified>2026-07-01T22:05:22.674Z</LastModified><ETag>&#34;001b1ab535a1cbd53bef9e4933aca2be&#34;</ETag><Size>21</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>hello.txt</Key><LastModified>2026-07-01T22:05:14.511Z</LastModified><ETag>&#34;3bb51064cf13d3be5e710394962bbeda&#34;</ETag><Size>26</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>
```

`KeyCount=2`; `data/report.json` ETag `001b1ab535a1cbd53bef9e4933aca2be` size `21`; `hello.txt` ETag `3bb51064cf13d3be5e710394962bbeda` size `26` — identical to the pre‑restart listing.

*Both objects still downloadable byte‑for‑byte:*

```console
$ curl -si "http://127.0.0.1:9000/first-bucket/hello.txt" --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" | sed -n '1p;/Content-Length/p;/Content-Type/p;/ETag/p;$p'
HTTP/1.1 200 OK
Content-Length: 26
Content-Type: text/plain
ETag: "3bb51064cf13d3be5e710394962bbeda"
Hello MinIO first bucket!

$ curl -si "http://127.0.0.1:9000/first-bucket/data/report.json" --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" | sed -n '1p;/Content-Length/p;/Content-Type/p;/ETag/p;$p'
HTTP/1.1 200 OK
Content-Length: 21
Content-Type: application/json
ETag: "001b1ab535a1cbd53bef9e4933aca2be"
{"report": "weekly"}
```

`hello.txt` returns `200`, `26` bytes, `text/plain`, body `Hello MinIO first bucket!`; `data/report.json` returns `200`, `21` bytes, `application/json`, body `{"report": "weekly"}` — both identical to before the restart.

**Why / rationale.** Persistence works because the state lives entirely on disk (Section 7): the bucket directory, the per‑object `xl.meta` files, and `.minio.sys/format.json`. On restart the server re‑reads the existing `format.json` (hence no reformatting) and re‑exposes the same objects. The preserved `CreationDate` and identical ETags/sizes/bodies are the confirming evidence that nothing was regenerated or lost.

---


## Section 9 — Edge cases

**Re‑creating an already‑owned bucket → `409 Conflict`, `BucketAlreadyOwnedByYou`.** Issuing `PUT /first-bucket` a second time does not error out as a generic failure; it reports the idempotent‑create conflict:

```console
$ curl -i -X PUT "http://127.0.0.1:9000/first-bucket" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" \
    -H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
HTTP/1.1 409 Conflict
Content-Length: 382
Content-Type: application/xml
X-Amz-Request-Id: 18BE49B70F1A252E
...

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>BucketAlreadyOwnedByYou</Code><Message>Your previous request to create the named bucket succeeded and you already own it.</Message><BucketName>first-bucket</BucketName><Resource>/first-bucket</Resource><RequestId>18BE49B70F1A252E</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

The XML `<Code>BucketAlreadyOwnedByYou</Code>` / `<Message>Your previous request to create the named bucket succeeded and you already own it.</Message>` at HTTP `409` match the error‑table definition exactly (`Code: "BucketAlreadyOwnedByYou"`, `HTTPStatusCode: http.StatusConflict`) [cmd/api-errors.go:909]. The server side shows the storage layer rejecting the duplicate volume:

```text
2026-07-01T22:05:53.066 [STORAGE] storage.MakeVol 127.0.0.1:9000 /tmp/minio-data first-bucket err='volume already exists' 21.538µs
2026-07-01T22:05:53.066 [409 Conflict] s3.PutBucket 127.0.0.1:9000/first-bucket 127.0.0.1        230µs       ⇣  215.508µs  ↑ 84 B ↓ 382 B
```

**Anonymous access is denied by default.** As shown in Section 5, an unsigned request returns `403 AccessDenied` because no anonymous bucket policy exists in the default setup.

**Why / rationale.** `BucketAlreadyOwnedByYou` (409) rather than a hard failure is S3's idempotent‑create semantics: the caller already owns the bucket, so nothing changes and the specific conflict code is returned. The anonymous denial is the default‑secure posture — access must be explicitly granted via policy, and none exists here.

---

## Section 10 — Rationale summary (including the two counter‑intuitive, observed facts)

- **Setup/startup:** a dev deployment is the compiled server pointed at a directory with the default identity; the `200` health probe and the banner (with the default‑credentials warning [cmd/server-main.go:974-975]) are the minimal proofs it is up and which identity governs access.
- **The four‑step flow:** create → write → write → list → read each maps to a dedicated handler and a distinct response channel (status/header/body); the GET `ETag` matching the PUT `ETag` proves the stored bytes equal the uploaded bytes.
- **Response bodies:** bucket/list operations return descriptive XML; writes return an empty body with the `ETag` header; reads stream raw bytes with the object's own headers.
- **Authorization:** with one root identity and no bucket policies, only correct SigV4 (`AWS4-HMAC-SHA256` [cmd/signature-v4.go:48]) requests succeed; a bad secret is `SignatureDoesNotMatch` and an unsigned request is `AccessDenied` — both `403`.
- **Request processing/persistence:** router → `s3APIMiddleware` (stamps `X-Amz-Request-Id`/`X-Amz-Id-2`) → auth → handler → `xl-storage`; writes are write‑to‑temp‑then‑atomic‑rename; reads go to the object's `xl.meta`.
- **Counter‑intuitive fact #1 — the default console log is quiet on the success path.** After a full flow the console log had only its banner (`0` per‑request lines, `0` per‑line timestamps); timestamped per‑request lines require `mc admin trace`. This document therefore does **not** invent per‑request console lines — it reports the trace lines that actually exist.
- **Counter‑intuitive fact #2 — a single drive still uses the `xl-single` erasure backend, not a legacy filesystem backend.** `format.json` says `"format":"xl-single"` [cmd/format-erasure.go:43], which is why every object is a directory holding an `xl.meta` file [cmd/xl-storage.go:68] (with small bodies inlined) rather than a plain file at the key path.
- **Restart durability:** all state is on disk, so a clean `SIGTERM` + restart re‑reads `format.json` (no reformatting) and re‑exposes identical buckets/objects — proven by the preserved `CreationDate` and identical ETags/sizes/bodies.

---

## Coverage pass — every named sub‑question, addressed by name

| Sub‑question / named item | Where answered | Key evidence |
|---------------------------|----------------|--------------|
| Local setup + startup banner + default‑creds warning + entry points + readiness `200` | Section 1 | banner text; `health/live` → `200`; [main.go:29], [cmd/main.go:201], [cmd/server-main.go:974-975] |
| CreateBucket (`200`, `Location`) — status + headers + body | Section 2, Step 1 | `200 OK`, `Location: /first-bucket`, empty body; [cmd/bucket-handlers.go:723] |
| PutObject `hello.txt` (`200`, ETag `3bb51064…`) | Section 2, Step 2 | `ETag: "3bb51064cf13d3be5e710394962bbeda"`; [cmd/object-handlers.go:1745] |
| PutObject `data/report.json` (`200`, ETag `001b1ab…`) — a **different** object | Section 2, Step 3 | `ETag: "001b1ab535a1cbd53bef9e4933aca2be"`; distinct key/size/type |
| ListObjectsV2 (`200`, `application/xml`, `KeyCount=2`, `STANDARD`) | Section 2, Steps 4a/4b | XML body + `boto3` `KeyCount: 2`; [cmd/bucket-listobjects-handlers.go:154] |
| GetObject `hello.txt` (`200`, `text/plain`, 26 B, body) | Section 2, Step 5 | body `Hello MinIO first bucket!`; [cmd/object-handlers.go:715] |
| Response‑body examples: bucket empty+`Location`, PUT empty+`ETag`, list XML, GET bytes, ListBuckets XML | Section 3 | all five shapes verbatim; [cmd/bucket-handlers.go:306] |
| Timestamped logs + console‑vs‑trace distinction (quiet console; single‑line + verbose) | Section 4 | console `0` per‑request lines; trace single‑line + verbose blocks |
| Authorization: signed success + bad‑secret `403 SignatureDoesNotMatch` + anonymous `403 AccessDenied` | Section 5 | signed `Authorization` header; [cmd/signature-v4.go:48,200,295], [cmd/api-errors.go:539,704], [cmd/auth-handler.go:339,560] |
| Request processing/routing: received / completed / data written / data read | Section 6 | `[REQUEST]`/`[RESPONSE]`, `RenameData`, `ReadXL`; [cmd/api-router.go:253], [cmd/generic-handlers.go:548], [internal/http/headers.go:160-161] |
| On‑disk: `format.json` `xl-single`, per‑object `xl.meta`, `XL2 ` magic, inline body, no part file | Section 7 | `format.json`; magic `58 4c 32 20`; sizes 437/438 B; `part.*` count `0`; [cmd/format-erasure.go:43], [cmd/xl-storage.go:68], [cmd/xl-storage-format-v2.go:44] |
| Restart durability: `SIGTERM`, restart, readiness `200`, identical ETags/sizes/body, `CreationDate` preserved | Section 8 | `Exiting on signal: TERMINATED`; preserved `2026-07-01T22:05:14.476Z` |
| Edge cases: `409 BucketAlreadyOwnedByYou`; anonymous denied | Section 9 | `409 Conflict` XML; [cmd/api-errors.go:909] |
| Rationale for each; report exactly‑observed (quiet console; `xl-single`) | Section 10 | two counter‑intuitive facts called out with evidence |

### `file:line` citation index (verified at commit `c07e5b49d477`)

`main.go:29` · `cmd/main.go:201` · `cmd/server-main.go:974-975` · `cmd/api-router.go:253` · `cmd/bucket-handlers.go:306` (ListBucketsHandler), `:723` (PutBucketHandler) · `cmd/bucket-listobjects-handlers.go:154` (ListObjectsV2Handler) · `cmd/object-handlers.go:715` (GetObjectHandler), `:1745` (PutObjectHandler) · `cmd/auth-handler.go:339` (checkRequestAuthType), `:560` (isReqAuthenticated) · `cmd/signature-v4.go:48` (`AWS4-HMAC-SHA256`), `:200` & `:295` (return `ErrSignatureDoesNotMatch`) · `cmd/api-errors.go:539` (`ErrAccessDenied`, 403), `:704` (`ErrSignatureDoesNotMatch`, 403), `:909` (`ErrBucketAlreadyOwnedByYou`, 409) · `cmd/format-erasure.go:43` (`xl-single`) · `cmd/xl-storage.go:68` (`xl.meta`) · `README.md:25-26,29,51,143` · `Makefile:177-179` · `go.mod:3`. Supplementary: `cmd/generic-handlers.go:548` (`X-Amz-Request-Id`), `internal/http/headers.go:160-161` (header constants), `cmd/xl-storage-format-v2.go:44` (`XL2 ` magic).

