# Bucket CORS Configuration Guide [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io) [![Docker Pulls](https://img.shields.io/docker/pulls/minio/minio.svg?maxAge=604800)](https://hub.docker.com/r/minio/minio/)

Cross-Origin Resource Sharing (CORS) is the browser mechanism that decides whether a page served from one origin may read a response from another. A browser-based application hosted on `https://app.example.com` can upload to and download from a MinIO bucket directly, instead of proxying every byte through its own backend, but only if MinIO tells the browser that the cross-origin request is permitted.

MinIO stores a CORS configuration per bucket, as part of that bucket's metadata alongside its other configurations such as tagging and lifecycle, and evaluates the stored rules when it answers a browser preflight. The configuration is written, read, and removed through the S3 `PutBucketCors`, `GetBucketCors`, and `DeleteBucketCors` operations, so AWS SDKs, `aws s3api`, and `mc` all work against it unchanged. A bucket that carries no configuration of its own is unaffected: it continues to be governed by the server-wide setting described in [Relationship to the server-wide CORS setting](#relationship-to-the-server-wide-cors-setting).

Read the next section before you rely on a bucket's rules to keep an origin out. **A bucket's CORS rules are consulted only for requests a browser asks permission for**, which is not every cross-origin request.

## Scope: preflighted requests only

**A bucket's CORS rules are consulted only when a browser sends a preflight. They do not restrict a cross-origin request a browser makes without one.**

A browser sends no preflight for a *simple* request: a `GET`, `HEAD`, or `POST` carrying no header beyond the CORS-safelisted ones - and, for `POST`, a `Content-Type` of `application/x-www-form-urlencoded`, `multipart/form-data`, or `text/plain`. Nothing announces such a request in advance, so there is nothing for a bucket's rules to answer. MinIO serves it, and the `Access-Control-*` headers on its response come from the server-wide `MINIO_API_CORS_ALLOW_ORIGIN` list, which defaults to `'*'`. On a deployment left at that default, a page on **any** origin can read the response to a simple cross-origin `GET` of a readable object, whatever the bucket's rules say - including from a bucket whose only rule names one other origin.

Which requests this leaves out is worth being concrete about. Anything carrying `Authorization`, `x-amz-content-sha256`, or any other `x-amz-*` header is preflighted, and that is every request an AWS SDK signs with SigV4 headers, so those do fall under the bucket's rules. What escapes them is the request that needs no header of its own: an anonymous read of an object a bucket policy makes public, or a presigned URL, fetched with `GET` or `HEAD`.

Two consequences follow:

- **Amazon S3 differs here.** S3 applies a bucket's configuration to the actual response as well, withholding `Access-Control-Allow-Origin` when no rule allows the origin. MinIO applies it to the preflight only, so a configuration S3 would enforce on both is enforced on one.
- **Restricting simple requests is a server-wide setting, not a per-bucket one.** Narrow `MINIO_API_CORS_ALLOW_ORIGIN` from its `'*'` default to the origins you actually mean to admit - see [Relationship to the server-wide CORS setting](#relationship-to-the-server-wide-cors-setting). A bucket's own rules then further restrict, for that bucket, the requests a browser does ask permission for.

Underlying both: **CORS is not access control.** It is enforced by the browser on a page's behalf, so it decides nothing for a client that speaks to MinIO directly, and a response a browser withholds from one page was still served. What may be read at all is settled by credentials and bucket policies - see [Access control](#access-control).

## Prerequisites

- Install MinIO - [MinIO Quickstart Guide](https://docs.min.io/community/minio-object-store/operations/deployments/baremetal-deploy-minio-on-redhat-linux.html#procedure).
- [Use `mc` with MinIO Server](https://docs.min.io/community/minio-object-store/reference/minio-mc.html#quickstart)
- Install `awscli` - [Installing AWS Command Line Interface](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-install.html)

## Quickstart

Write the configuration document shown in [The CORS configuration document](#the-cors-configuration-document) to a local file, then set it on a bucket, read it back, and remove it again. `mc cors` takes the document in its XML form:

```sh
mc cors set myminio/mybucket /path/to/cors.xml
mc cors get myminio/mybucket
mc cors remove myminio/mybucket
```

The same three operations are available through `aws s3api` and through the AWS SDKs, and each of them is a request against the `?cors` sub-resource of the bucket. Each is authorized against its own policy action. A signing client such as `mc` is the usual way to issue them, but a signature is not itself what the server insists on: an unsigned request is evaluated against the bucket policy, so a policy that grants the action to an anonymous principal admits one - see [Bucket CORS operations](#bucket-cors-operations) for the wire contract and [Access control](#access-control) for the actions involved.

## The CORS configuration document

The document's root element is `CORSConfiguration` in the S3 namespace `http://s3.amazonaws.com/doc/2006-03-01/`, and it holds one or more `CORSRule` elements. Element names and their casing are exactly the ones AWS defines, so a document written for Amazon S3 is accepted as it is.

```xml
<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <CORSRule>
    <AllowedHeader>Content-Type</AllowedHeader>
    <AllowedHeader>x-amz-*</AllowedHeader>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedMethod>POST</AllowedMethod>
    <AllowedOrigin>https://app.example.com</AllowedOrigin>
    <AllowedOrigin>http://www.example2.*</AllowedOrigin>
    <ExposeHeader>ETag</ExposeHeader>
    <ExposeHeader>x-amz-request-id</ExposeHeader>
    <ID>web-app-uploads</ID>
    <MaxAgeSeconds>3000</MaxAgeSeconds>
  </CORSRule>
  <CORSRule>
    <AllowedHeader>*</AllowedHeader>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>HEAD</AllowedMethod>
    <AllowedOrigin>*</AllowedOrigin>
    <ExposeHeader>ETag</ExposeHeader>
    <ID>public-read</ID>
    <MaxAgeSeconds>600</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>
```

| Element | Cardinality per rule | Meaning |
| --- | --- | --- |
| `ID` | at most one | A label for the rule. Its value is preserved apart from the surrounding-whitespace normalization described below, and it is never matched against a request. |
| `AllowedMethod` | one or more | An HTTP method the rule permits. Only `GET`, `PUT`, `POST`, `DELETE`, and `HEAD` are accepted. |
| `AllowedOrigin` | one or more | An origin the rule permits, either literally or through a single `*` wildcard. |
| `AllowedHeader` | zero or more | A request header the browser may announce in the preflight. Matched case-insensitively, and a single `*` wildcard is permitted. |
| `ExposeHeader` | zero or more | A response header the browser is allowed to read. |
| `MaxAgeSeconds` | at most one | How long, in seconds, the browser may cache this preflight answer. |

What is stored is the canonical form of the document that was submitted, rather than the bytes that arrived, so a subsequent `GetBucketCors` returns an equivalent configuration that may differ from the submission in these harmless ways:

- Every `AllowedMethod` is normalized to upper case, so a rule submitted with `<AllowedMethod>get</AllowedMethod>` is returned as `<AllowedMethod>GET</AllowedMethod>`.
- The whitespace and line breaks that surround an element's value are insignificant markup and are dropped, so a value indented onto a line of its own is stored as the value the document means.
- The namespace is filled in when the submitted document omitted it.
- The child elements of every rule are returned in the order `AllowedHeader`, `AllowedMethod`, `AllowedOrigin`, `ExposeHeader`, `ID`, `MaxAgeSeconds`, whatever order they were submitted in. The example above is already written in that order.
- An element that carries no information is left out, which is why a `MaxAgeSeconds` of `0` does not come back: it means the same as omitting the element, namely that the browser is not to cache the answer.

The rules themselves are never merged and never reordered: they are stored and returned in the order the document lists them, which is the order [Preflight evaluation](#preflight-evaluation) matches them in. The canonicalization above is the only rewriting a value undergoes, and none of it widens a rule: no origin, method, or header the document does not name is ever added to one.

## Bucket CORS operations

| Operation | Request | Success | Notes |
| --- | --- | --- | --- |
| `PutBucketCors` | `PUT /{bucket}?cors` | `200 OK` with an empty body | The document is validated in full before it is stored, and it replaces any configuration the bucket already had. |
| `GetBucketCors` | `GET /{bucket}?cors` | `200 OK` with the stored `CORSConfiguration` document | Returns `404 NoSuchCORSConfiguration` when the bucket has no configuration. |
| `DeleteBucketCors` | `DELETE /{bucket}?cors` | `204 No Content` with an empty body | Idempotent: removing a configuration a bucket never had also succeeds with `204`. |

Failures are reported in the standard S3 XML error body. The two codes specific to this API are:

| Code | HTTP status | Message | Raised when |
| --- | --- | --- | --- |
| `NoSuchCORSConfiguration` | `404` | The CORS configuration does not exist | `GetBucketCors` was called on a bucket that has no CORS configuration, either because none was ever set or because it was removed. |
| `MalformedXML` | `400` | The XML you provided was not well-formed or did not validate against our published schema. | The document submitted to `PutBucketCors` did not pass validation. The specific cause is carried in the `Message` of the error body. |

A request whose credentials do not permit the operation fails with `403 AccessDenied`, as everywhere else in the S3 API. `PutBucketCors` checks that the bucket exists and fails with `404 NoSuchBucket` when it does not. `GetBucketCors` resolves the configuration through the bucket metadata layer without a separate existence check, exactly as the sibling per-bucket configuration APIs do, so a bucket that does not exist is reported as `404 NoSuchCORSConfiguration` rather than as `404 NoSuchBucket`.

## Validation rules

`PutBucketCors` accepts a document only if all of the following hold. Validation is all or nothing: a document that violates any one of these rules is rejected in full, no part of it is stored, and the bucket keeps whatever configuration it had. Nothing that carries meaning is repaired or dropped to make a document acceptable - the only changes it undergoes are the canonicalization described in [The CORS configuration document](#the-cors-configuration-document) and the skipping of the insignificant markup listed below.

- The body is well-formed XML and its root element is `CORSConfiguration`. Both the XML that arrives and the canonical form of it that gets stored must be no larger than **128 KiB**, so a document that only just fits can still be refused once the namespace has been filled in or a value escaped. A leading byte order mark is tolerated. A document type declaration is not.
- The root element either declares the namespace `http://s3.amazonaws.com/doc/2006-03-01/` or declares no namespace at all, in which case the S3 namespace is assumed. Any other namespace is rejected, because the stored document is the one handed back to clients that expect the AWS wire format.
- The document contains **between 1 and 100 `CORSRule` elements** and nothing besides them that could be read as configuration: text outside a rule, an unrecognized element, a nested element where a value belongs, and any element or text following the root element are each refused. A misspelled element such as `AllowedOrigins` is refused rather than ignored, so a rule can never be stored with fewer values than the document appears to grant. Markup that carries no configuration is insignificant and is simply skipped wherever it appears: the XML declaration, the indentation between elements, comments, processing instructions, and any attribute other than the root element's `xmlns`.
- **No element carries the same attribute twice.** This is a well-formedness rule rather than a MinIO restriction - XML forbids an element from repeating an attribute name, and extends the prohibition to two attributes whose names resolve to the same namespace and local name however they were spelled, so `xmlns="..." xmlns="..."`, `foo="1" foo="2"`, `xmlns:a="urn:one" xmlns:a="urn:two"` and `p:k="1" q:k="2"` with both prefixes bound to one namespace are each refused, on the root element, on a `CORSRule` and on a value element alike. A document that repeats an attribute is one no conforming XML parser will read, so it is rejected rather than skipped as insignificant markup, and the bucket keeps the configuration it had.
- Every rule carries **at least one `AllowedMethod`**, and each one is `GET`, `PUT`, `POST`, `DELETE`, or `HEAD`. `OPTIONS` is not a valid value - it is the preflight method itself, which the server answers rather than something a rule grants - and neither is `PATCH`.
- Every rule carries **at least one `AllowedOrigin`**. An origin is either the bare wildcard `*` or a value containing **at most one `*`**, such as `http://www.example2.*`.
- Every `AllowedHeader` contains **at most one `*`**.
- `ID` and `MaxAgeSeconds` appear **at most once per rule**; `AllowedHeader`, `AllowedMethod`, `AllowedOrigin`, and `ExposeHeader` may repeat. A repeated `ID` or `MaxAgeSeconds` is rejected rather than resolved to one of the values given.
- `MaxAgeSeconds` is a **non-negative** integer.
- No origin or header name contains a control character.

A rejection is a `MalformedXML` error whose `Message` names the specific validation cause rather than the generic schema text. What else the message points at follows from the cause: one that belongs to a single rule identifies that rule, numbering rules from zero in document order, and one that turns on a particular value quotes that value. A cause that belongs to the document as a whole - an oversized body, a rule count outside the permitted range, a root element that is not `CORSConfiguration` - belongs to no rule, so it identifies none. Submitting a rule whose only `AllowedMethod` is `PATCH` is a per-value cause within a rule, and produces the following body, shown here indented for readability and with the `RequestId` and `HostId` that identify the request and the server it reached:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>MalformedXML</Code>
  <Message>CORSRule 0 has unsupported AllowedMethod &#34;PATCH&#34;</Message>
  <BucketName>mybucket</BucketName>
  <Resource>/mybucket</Resource>
  <RequestId>18C78572B8497F72</RequestId>
  <HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId>
</Error>
```

Clients surface that `Message` verbatim, so `mc` and `aws s3api` both report `CORSRule 0 has unsupported AllowedMethod "PATCH"` rather than the generic schema text.

A request whose body fails its own integrity check is not a document that failed to validate, and it is not reported as one. Whatever a request declares about the bytes it is sending - `Content-Md5`, `x-amz-content-sha256`, or a supported `x-amz-checksum-*` value, including one promised through `x-amz-trailer` - is verified against those bytes before any of them becomes stored configuration. That holds however the request is authorized: signature version 4, signature version 2, and an unsigned request a bucket policy authorizes are all verified the same way. A declared value that does not match the body is answered with the error S3 defines for it - `BadDigest`, `XAmzContentSHA256Mismatch` or `XAmzContentChecksumMismatch` - rather than with `MalformedXML`, and one that cannot be decoded at all is refused before the body is read, with `InvalidDigest` for `Content-Md5`, `XAmzContentSHA256Mismatch` for `x-amz-content-sha256`, and `InvalidArgument` for a checksum. A transfer that stops before the length it announced is refused for what it is as well - `ClientDisconnected`, `499`, which is what this server answers whenever a client goes away before its request is answered - rather than with a complaint about XML the client never finished sending. A client cutting a transfer short has to close its end of the connection to say so, and the server sees that close first, so the truncation is reported as the disconnect that carried it rather than as a document that failed to validate. What matters either way is that the document is read in full, and checked against whatever the request declared about it, before any of it becomes configuration: a truncated transfer leaves the bucket with the configuration it had.

Only what a client declares is verified, so a client that declares nothing is unaffected. `UNSIGNED-PAYLOAD` declares no digest either and is the one value for `x-amz-content-sha256` the server treats that way here; every other value is taken as a digest and verified. One exception is inherited from the server rather than chosen here, and it applies to a CORS configuration exactly as it does to every other body this server accepts: a deployment started with the hidden `--no-compat` flag, which trades strict S3 compatibility for certain performance optimizations, deliberately tolerates clients that send a non-empty body while declaring the SHA-256 of an empty one, so that one declaration is skipped rather than verified. In the default strict-compatibility mode - the mode everything above describes - it is verified like any other, and a body that does not match it is refused.

What this operation does not accept is a streamed body: `PutBucketCors` is not a chunked-upload route, so a request declaring `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` or `STREAMING-UNSIGNED-PAYLOAD-TRAILER` - the values an `aws-chunked` payload announces itself with, whether or not it promises a trailer - is refused with `InvalidRequest` for the authorization mechanism it presented, before its body is read at all. A body of `THIS IS NOT XML AT ALL` behind such a declaration is answered the same way rather than with `MalformedXML`, which is how one can see the document was never consumed. Send the configuration as an ordinary body instead, and declare its checksum in a header if you want it verified; that is what every client documented here does.

## Preflight evaluation

Before a browser issues a cross-origin request that is not simple - anything using `PUT`, `DELETE`, or a custom header, for instance - it sends an `OPTIONS` request to the same URL announcing what it is about to do, in an `Origin` header, an `Access-Control-Request-Method` header, and optionally an `Access-Control-Request-Headers` header. That request is the preflight, and its answer decides whether the browser proceeds.

Everything in this section is about that answer, and only about it. A bucket's rules are evaluated when a preflight arrives and at no other point: the request that follows a preflight is served without consulting them again, and a cross-origin request the browser sends without a preflight at all never reaches them - see [Scope: preflighted requests only](#scope-preflighted-requests-only). A rule that names one origin therefore keeps another origin from making a `PUT`, or from reading a response through a custom header, but it does not by itself keep that origin from reading a plain `GET`.

MinIO resolves the target bucket from the preflight - both path-style and virtual-host-style addressing are understood, and the `Host` header is admitted by the same rule every other request is admitted by, so a preflight is attributed to exactly the bucket the request it precedes would reach. A `Host` value this server does not accept is refused rather than answered from a more permissive default, as [When no rule matches](#when-no-rule-matches) describes. Having resolved the bucket, MinIO loads that bucket's rules and walks them **in document order**, taking the **first rule that matches on all three axes at once**:

- **Origin.** Some `AllowedOrigin` of the rule equals the request's `Origin` exactly, or matches it through its single `*`, which stands for any sequence of characters. `http://www.example2.*` therefore matches `http://www.example2.com`, and the bare `*` matches every origin.
- **Method.** Some `AllowedMethod` of the rule is the method named in `Access-Control-Request-Method`.
- **Headers.** **Every** header named in `Access-Control-Request-Headers` is covered by an `AllowedHeader` of the rule. Header names are compared case-insensitively, so `Content-Type` and `content-type` are the same header, `*` covers every header, and a pattern such as `x-amz-*` covers every header with that prefix. A preflight that announces no header is trivially satisfied on this axis, while a rule that lists no `AllowedHeader` at all cannot match a preflight that announces one.

**First matching rule wins.** Rules are not combined, so the single rule that matched determines the whole answer. A permissive rule placed ahead of a restrictive one therefore shadows it - in the document above, moving the `public-read` rule first would have it answer every `GET` and `HEAD` preflight, including those from `https://app.example.com`.

### When a rule matches

The answer is `200 OK` with no body and the CORS response headers the matched rule configures:

| Response header | Value |
| --- | --- |
| `Access-Control-Allow-Origin` | The request's own `Origin`, echoed back. |
| `Access-Control-Allow-Methods` | The method the preflight asked about. |
| `Access-Control-Allow-Headers` | The headers the preflight asked about, when it named any. |
| `Access-Control-Max-Age` | The rule's `MaxAgeSeconds`, when it is greater than zero. |
| `Access-Control-Expose-Headers` | The rule's `ExposeHeader` list, when it has one. |

The answer also declares `Vary: Origin`, `Vary: Access-Control-Request-Method`, and `Vary: Access-Control-Request-Headers`, because it depends on all three: an intermediary cache has to key on each of them rather than hand one origin's answer to another.

The origin is echoed rather than answered with `*` so that the answer names the one caller it was written for, which is what makes the `Vary: Origin` declaration above meaningful: an intermediary that caches the answer can then only reuse it for that origin. The method and the headers are echoed for the same reason a browser announces them one request at a time: the answer only has to cover the request being asked about.

The table above is the complete set of `Access-Control-*` headers a matched rule produces. In particular, a preflight answered from a bucket's own rules does **not** carry `Access-Control-Allow-Credentials`; only the server-wide handler emits that header, which is one of the ways the two answers are told apart in [Relationship to the server-wide CORS setting](#relationship-to-the-server-wide-cors-setting). A browser will not send a credentialed cross-origin request - one made with `credentials: 'include'`, carrying cookies or HTTP authentication - unless the preflight answer says `Access-Control-Allow-Credentials: true`, so a bucket's own rules govern uncredentialed cross-origin requests.

The following preflight matches the `web-app-uploads` rule of the document above:

```sh
curl -i -X OPTIONS http://localhost:9000/mybucket/ \
  -H "Origin: https://app.example.com" \
  -H "Access-Control-Request-Method: PUT" \
  -H "Access-Control-Request-Headers: content-type"
```

Its answer carries the following CORS headers. The block is an excerpt: the response also carries the generic headers the HTTP server adds to any reply, such as `Date` and a zero `Content-Length`.

```http
HTTP/1.1 200 OK
Access-Control-Allow-Headers: content-type
Access-Control-Allow-Methods: PUT
Access-Control-Allow-Origin: https://app.example.com
Access-Control-Expose-Headers: ETag, x-amz-request-id
Access-Control-Max-Age: 3000
Vary: Origin
Vary: Access-Control-Request-Method
Vary: Access-Control-Request-Headers
```

A preflight is not an authenticated S3 request and carries no credentials, so it is answered from the bucket's rules alone. Whether the request the browser then goes on to make succeeds is a separate question, settled by that request's own signature and policy as usual.

### When no rule matches

**The answer carries no `Access-Control-Allow-*` header at all, and that absence is the denial.** The status is still `200 OK`, because the preflight itself was a perfectly valid HTTP request; what tells the browser it may not proceed is that the response never names its origin. Among the CORS decision headers, only `Vary` is set. There is no error code to look for, so a client debugging a denied request should look for a missing `Access-Control-Allow-Origin` rather than for a failed status.

Changing the origin of the request above to one no rule names is denied in exactly that way:

```sh
curl -i -X OPTIONS http://localhost:9000/mybucket/ \
  -H "Origin: http://evil.example.com" \
  -H "Access-Control-Request-Method: PUT" \
  -H "Access-Control-Request-Headers: content-type"
```

Again as an excerpt, alongside the generic headers the HTTP server adds:

```http
HTTP/1.1 200 OK
Vary: Origin
Vary: Access-Control-Request-Method
Vary: Access-Control-Request-Headers
```

The same answer is given whenever the request deviates on any single axis - a method no rule allows, or a single requested header no rule covers, is enough. However many headers a preflight announces, every one of them is evaluated against the rule: the list is never shortened, and no count of its own refuses a request.

Two bounds do apply to the work a single preflight may ask for. A preflight is admitted only if it carries no more header bytes than any other MinIO request may - 8 KiB of headers in total, of which at most 2 KiB may be user metadata, counting every value of every header field a request repeats - and its evaluation is allowed a fixed number of comparisons. Neither bound is a limit on how many headers may be *announced*: the allowance is generous enough that a browser-issued preflight spends a small fraction of it even against a bucket holding the largest document this server accepts. A preflight that exceeds either bound is refused instead of answered, for the same reason a partial evaluation settles nothing: rules that were not examined in full cannot be known to allow the request. Because a preflight is unauthenticated, these are the only bounds standing between a client and the work its request asks for, which is why the byte bound is applied before the announced header list is read at all.

One further case is answered the same way. A `Host` header this server does not accept is refused rather than answered from the server-wide setting. A browser will send such a value - a hostname containing an underscore, for instance - and this server answers it with `400 Bad Request` for every other request, so a preflight carrying one is refused rather than handed to a more permissive default it was never meant to reach.

Each of these refusals looks to a client exactly like one a bucket's rules produced, so the server reports the reason in its log, at a bounded rate rather than once per request. It is reported as an **event** under the `cors` subsystem rather than as an error, because a refusal is a condition this layer expects and handles - and so the entry carries no stack trace. Frames would name the same three call sites on every refusal, all of them inside the logger, this middleware and `net/http`, which is nothing an operator can act on and nothing the reason has not already said. The report is composed of server-authored text, plus - for a refusal that is attributable to a particular bucket - the name of that bucket, which has already been validated as a bucket name and is therefore bounded in both length and alphabet. Nothing else the request carried is repeated in it: not the `Origin`, not the headers the preflight announced, not the method it asked about, not the `Host` it was addressed to. A preflight is unauthenticated, so every one of those values is chosen by whoever sent it. An operator can therefore tell a refused preflight apart from a denied one without the report itself becoming the load it describes.

That event is the only server-side record a preflight leaves, because **no audit entry is written for a preflight, whichever way it was answered**. A preflight is answered ahead of the router, so it reaches none of the per-request audit, trace or request-metric plumbing: an allowed preflight leaves no entry, and neither does a denied one - not the denial a bucket's rules produced, which is normal operation and is deliberately not reported at all, and not the refusals above, whose only trace is that rate-limited event. What *is* audited is the request that follows an allowed preflight, recorded under its own API name like any other, and so are the `PutBucketCors`, `GetBucketCors` and `DeleteBucketCors` calls that maintain the configuration. Those audit entries record each request as it arrived, minus the values that authenticate it: the `Authorization` header, the presigned `X-Amz-Signature` and `Signature` query parameters, and any `X-Amz-Security-Token` are dropped, so an entry cannot be replayed as a request. Everything an audit entry is read for is kept, including the API name, the principal, the bucket, the status, the request ID and the remaining headers and query parameters - among them `X-Amz-Date`, `X-Amz-Credential`, `X-Amz-Expires` and `X-Amz-SignedHeaders`, which record how a request was signed without recording what it was signed with. A flow a browser reports as blocked is therefore a flow whose audit log is silent about the decision that blocked it. Diagnose it from the two ends instead: read the stored document back with `GetBucketCors`, and reproduce the preflight with the `curl -X OPTIONS` form shown above, which returns the very answer the browser acted on.

A bucket whose own rules are not in hand is a different matter, and how it is answered depends on *why* they are not. A bucket that demonstrably has no configuration - none was ever set, it was removed, or the bucket does not exist - is handed to the server-wide setting, which is the behavior every such bucket has always had. A bucket whose configuration could not be established at all is refused instead, because rules that were never read cannot be known to allow the request, and answering it from `MINIO_API_CORS_ALLOW_ORIGIN` is precisely how a configured bucket's own rules would be bypassed.

The two are told apart rather than treated alike, which matters most in the seconds after a restart. A freshly started server serves requests while it is still loading bucket metadata, so for a short while its cache holds neither the buckets that have a configuration nor the buckets that do not. During that window a bucket the cache does not hold is resolved from its stored metadata directly - one document, for the one bucket the request names, read on that request's own context and nothing else - so a bucket's stored rules govern its preflights from the first request the server answers, and a bucket without a configuration keeps receiving the server-wide answer throughout. Once loading has finished no such read happens again: the cache then holds every bucket that exists, so a bucket missing from it demonstrably has no configuration of its own.

Because the requests that trigger those reads are unauthenticated, they are bounded: a handful run at a time, each with a short deadline, and a preflight that arrives when they are all busy is refused rather than queued behind them. Nothing that is read this way is cached, so preflights naming buckets that do not exist cannot accumulate anything. The remaining case is a server too early in its startup to have an object layer at all: there is nothing to read a configuration from, so such a preflight is refused - and the request it asks about could not have been served either, since every S3 operation answers `503 ServerNotInitialized` until that point. A refusal carries no `Access-Control-Max-Age`, so browsers do not cache it and a retry once the server is up succeeds.

Be clear about what a denial is and is not. It is enforced by the browser, which withholds the response from the page that asked for it; it is not server-side access control. A client that speaks to MinIO directly is unaffected by CORS rules entirely, because nothing in the mechanism depends on it cooperating. Deciding who may read an object remains the job of credentials and bucket policies; a bucket's CORS rules decide only what a browser will do on a page's behalf.

## Relationship to the server-wide CORS setting

MinIO has always had a server-wide allow-origin list, `MINIO_API_CORS_ALLOW_ORIGIN`, which defaults to `'*'` and is documented with the rest of the `api` subsystem in the [MinIO Server Configuration Guide](https://github.com/minio/minio/blob/master/docs/config/README.md):

```text
MINIO_API_CORS_ALLOW_ORIGIN               (csv)       set comma separated list of origins allowed for CORS requests (default: '*')
```

That setting is unchanged by per-bucket CORS and remains the server-wide control. The two divide cleanly, per bucket:

- A bucket whose CORS configuration is in hand is answered from it, and only from it. Its rules decide, and the server-wide list is not consulted for that bucket's preflights.
- A bucket with **no** CORS configuration is answered exactly as it was before, by the server-wide handler governed by `MINIO_API_CORS_ALLOW_ORIGIN`. Setting a configuration on one bucket changes nothing for any other bucket. A bucket whose configuration the server cannot produce - it has not finished loading its bucket metadata, for instance - is answered the same way for as long as that lasts, as [When no rule matches](#when-no-rule-matches) describes.

The two answers are easy to tell apart when you are checking which one you got: the server-wide handler replies to a preflight with `204 No Content` and includes `Access-Control-Allow-Credentials: true`, and it never emits `Access-Control-Max-Age` or `Access-Control-Expose-Headers`. A preflight answered from a bucket's own rules replies `200 OK`, carries no `Access-Control-Allow-Credentials`, and does emit `Access-Control-Max-Age` and `Access-Control-Expose-Headers` when the matched rule configures them. Removing a bucket's configuration with `DeleteBucketCors` hands that bucket back to the server-wide setting.

Which of the two answers a bucket gives therefore changes the moment a configuration is stored on it or removed from it, and every difference above travels with that: the status a browser sees, whether the answer carries a `Max-Age` it can cache and an `Expose-Headers` list, and - most consequentially - whether a credentialed cross-origin request is permitted at all, since only the server-wide answer says `Access-Control-Allow-Credentials: true`, as [When a rule matches](#when-a-rule-matches) explains. Storing a first document on a bucket a browser already reaches is worth checking against the page that reaches it, because that is the point at which those properties change. The vocabulary of methods differs as well: the server-wide handler compares the requested method exactly and entertains `PATCH` in addition to the five S3 methods, while a bucket's rules compare it case-insensitively and can name only those five. The same preflight the server-wide handler turns down for asking about a lower-case `get` is therefore admitted by a bucket rule naming `GET`, and a `PATCH` preflight the server-wide handler admits is denied by any bucket that has rules at all. Neither difference is one a browser meets: it asks about an upper-case method, and `PATCH` is not a method this server routes at all, so the request such a preflight asks about answers `400 Bad Request` whichever way the preflight itself was answered.

One operating detail of that server-wide list is worth knowing precisely because every bucket without a configuration of its own is answered from it: a value carrying an empty entry - a trailing comma, or a lone `,` - is not accepted, and the server logs `Invalid api configuration: invalid cors value` and then runs with the `'*'` default rather than refusing to start. Narrowing the list is therefore worth confirming rather than assuming, and one request confirms it: send the `curl -X OPTIONS` form shown above from an origin the narrowed list excludes, against a bucket that has no configuration of its own, and check that no `Access-Control-Allow-Origin` comes back.

**That division is over preflights specifically, and it is the one thing to get right about these two controls.** A bucket's rules are consulted when a preflight arrives and nowhere else, so the server-wide list - not the bucket's rules - is what answers every cross-origin request a browser sends without one, as [Scope: preflighted requests only](#scope-preflighted-requests-only) sets out. Leaving `MINIO_API_CORS_ALLOW_ORIGIN` at its `'*'` default therefore leaves a simple cross-origin `GET` or `HEAD` readable from any origin on every bucket, however narrow that bucket's own rules are. The two controls are complementary rather than alternatives: narrow the server-wide list to the origins the deployment admits at all, and use per-bucket rules to say which of them may do what, per bucket, among the requests a browser asks permission for.

## Client compatibility

The wire format is the AWS one, so the operations work with the AWS SDKs and with any tool built on them. Two clients are worth showing explicitly, because they take the configuration in different shapes.

`mc` takes the XML document as it goes over the wire. Leaving the access key and secret key off the `alias set` command makes `mc` prompt for them, which keeps them out of the shell history; the plain `http://` endpoint below is only appropriate because it is local, and any other endpoint should be `https://`:

```sh
mc alias set myminio http://localhost:9000
mc cors set myminio/mybucket cors.xml
mc cors get myminio/mybucket
mc cors remove myminio/mybucket
```

The AWS CLI takes the same configuration as **JSON**, which it converts to the XML above for you. Its keys are the **plural** forms of the XML element names - `CORSRules`, `AllowedHeaders`, `AllowedMethods`, `AllowedOrigins`, `ExposeHeaders`, each an array - while `ID` and `MaxAgeSeconds` stay singular. This JSON is a CLI input format only; it is never sent to the server, so do not submit it to the `?cors` sub-resource directly.

```json
{
  "CORSRules": [
    {
      "ID": "web-app-uploads",
      "AllowedHeaders": ["Content-Type", "x-amz-*"],
      "AllowedMethods": ["GET", "PUT", "POST"],
      "AllowedOrigins": ["https://app.example.com", "http://www.example2.*"],
      "ExposeHeaders": ["ETag", "x-amz-request-id"],
      "MaxAgeSeconds": 3000
    },
    {
      "ID": "public-read",
      "AllowedHeaders": ["*"],
      "AllowedMethods": ["GET", "HEAD"],
      "AllowedOrigins": ["*"],
      "ExposeHeaders": ["ETag"],
      "MaxAgeSeconds": 600
    }
  ]
}
```

With that document saved as `cors.json`:

```sh
aws --endpoint-url http://localhost:9000 s3api put-bucket-cors \
  --bucket mybucket --cors-configuration file://cors.json
aws --endpoint-url http://localhost:9000 s3api get-bucket-cors --bucket mybucket
aws --endpoint-url http://localhost:9000 s3api delete-bucket-cors --bucket mybucket
```

`get-bucket-cors` prints the stored configuration back in the same JSON shape. On a bucket with no configuration it reports the `NoSuchCORSConfiguration` error, which is the `404` described in [Bucket CORS operations](#bucket-cors-operations).

## Access control

The three operations are governed by the policy actions S3 defines for them, and each one is authorized separately:

| Operation | Policy action |
| --- | --- |
| `PutBucketCors` | `s3:PutBucketCors` |
| `GetBucketCors` | `s3:GetBucketCors` |
| `DeleteBucketCors` | `s3:DeleteBucketCors` |

`DeleteBucketCors` requires its own `s3:DeleteBucketCors` action; permission to write a configuration does not imply permission to remove one. All three actions already existed in MinIO's policy vocabulary, so the capability ships without any change to the identity and access model. There are two ways to grant them, and they differ in one element.

An **identity policy** names actions and resources but no principal, because the principal is whichever user or group the policy is attached to:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutBucketCors",
        "s3:GetBucketCors",
        "s3:DeleteBucketCors"
      ],
      "Resource": ["arn:aws:s3:::mybucket"]
    }
  ]
}
```

Save that as `cors-admin.json`, then create it on the server and attach it to an identity:

```sh
mc admin policy create myminio cors-admin cors-admin.json
mc admin policy attach myminio cors-admin --user webops
```

A **bucket policy** is attached to the bucket instead, so it has to say who it applies to: every statement must carry a `Principal`, and MinIO rejects one that does not. Granting a CORS action to the anonymous principal is what makes an unsigned request to the `?cors` sub-resource succeed:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"AWS": ["*"]},
      "Action": [
        "s3:GetBucketCors"
      ],
      "Resource": ["arn:aws:s3:::mybucket"]
    }
  ]
}
```

Save that as `cors-read.json` and put it on the bucket:

```sh
mc anonymous set-json cors-read.json myminio/mybucket
```

Grant only the actions you mean to: the example above lets anyone read the bucket's CORS configuration, and adding `s3:PutBucketCors` or `s3:DeleteBucketCors` to it would let anyone rewrite or erase that configuration. The root credentials need neither policy, so an administrator can configure a bucket's CORS rules straight away and grant the actions to other identities afterwards.

## Explore Further

- [Use `mc` with MinIO Server](https://docs.min.io/community/minio-object-store/reference/minio-mc.html#quickstart)
- [`aws s3api put-bucket-cors` reference](https://docs.aws.amazon.com/cli/latest/reference/s3api/put-bucket-cors.html) - the command and the JSON input shape shown in [Client compatibility](#client-compatibility); the sibling `get-bucket-cors` and `delete-bucket-cors` pages document the other two operations
- [Use the `minio-go` SDK with MinIO Server](https://docs.min.io/aistor/developers/sdk/go/)
- [The MinIO documentation website](https://docs.min.io/community/minio-object-store/index.html)
