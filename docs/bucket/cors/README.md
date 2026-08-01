# Bucket CORS Configuration Guide [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io) [![Docker Pulls](https://img.shields.io/docker/pulls/minio/minio.svg?maxAge=604800)](https://hub.docker.com/r/minio/minio/)

Cross-Origin Resource Sharing (CORS) is the browser mechanism that decides whether a page served from one origin may read a response from another. A browser-based application hosted on `https://app.example.com` can upload to and download from a MinIO bucket directly, instead of proxying every byte through its own backend, but only if MinIO tells the browser that the cross-origin request is permitted.

MinIO stores a CORS configuration per bucket, as part of that bucket's metadata alongside its other configurations such as tagging and lifecycle, and evaluates the stored rules when it answers a browser preflight. The configuration is written, read, and removed through the S3 `PutBucketCors`, `GetBucketCors`, and `DeleteBucketCors` operations, so AWS SDKs, `aws s3api`, and `mc` all work against it unchanged. A bucket that carries no configuration of its own is unaffected: it continues to be governed by the server-wide setting described in [Relationship to the server-wide CORS setting](#relationship-to-the-server-wide-cors-setting).

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

The same three operations are available through `aws s3api` and through the AWS SDKs, and each of them is a request against the `?cors` sub-resource of the bucket. Every one is authenticated and authorized against its own policy action, so the requests are issued through a client that signs them - see [Bucket CORS operations](#bucket-cors-operations) for the wire contract and [Access control](#access-control) for the actions involved.

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
| `ID` | at most one | A label for the rule. It is stored and returned verbatim and is never matched against a request. |
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

The rules themselves are never merged, reordered, or rewritten. They are stored and returned in the order the document lists them, which is the order [Preflight evaluation](#preflight-evaluation) matches them in.

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

`PutBucketCors` accepts a document only if all of the following hold. Nothing is silently dropped or corrected: a document that violates any of these rules is rejected in full, and the bucket keeps whatever configuration it had.

- The body is well-formed XML no larger than **128 KiB**, and its root element is `CORSConfiguration`. A leading byte order mark is tolerated. A document type declaration is not.
- The root element either declares the namespace `http://s3.amazonaws.com/doc/2006-03-01/` or declares no namespace at all, in which case the S3 namespace is assumed. Any other namespace is rejected, because the stored document is the one handed back to clients that expect the AWS wire format.
- The document contains **between 1 and 100 `CORSRule` elements**, and nothing else: no text, no unrecognized element, and nothing following the root element. A misspelled element such as `AllowedOrigins` is refused rather than ignored, so a rule can never be stored with fewer values than the document appears to grant.
- Every rule carries **at least one `AllowedMethod`**, and each one is `GET`, `PUT`, `POST`, `DELETE`, or `HEAD`. `OPTIONS` is not a valid value - it is the preflight method itself, which the server answers rather than something a rule grants - and neither is `PATCH`.
- Every rule carries **at least one `AllowedOrigin`**. An origin is either the bare wildcard `*` or a value containing **at most one `*`**, such as `http://www.example2.*`.
- Every `AllowedHeader` contains **at most one `*`**.
- `ID` and `MaxAgeSeconds` appear **at most once per rule**; `AllowedHeader`, `AllowedMethod`, `AllowedOrigin`, and `ExposeHeader` may repeat. A repeated `ID` or `MaxAgeSeconds` is rejected rather than resolved to one of the values given.
- `MaxAgeSeconds` is a **non-negative** integer.
- No origin or header name contains a control character.

A rejection is a `MalformedXML` error whose `Message` names the cause, the rule that carries it, and the offending value, with rules numbered from zero in document order. Submitting a rule whose only `AllowedMethod` is `PATCH` produces the following body, shown here indented for readability:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>MalformedXML</Code>
  <Message>CORSRule 0 has unsupported AllowedMethod &#34;PATCH&#34;</Message>
  <BucketName>mybucket</BucketName>
  <Resource>/mybucket</Resource>
  <RequestId>18C78572B8497F72</RequestId>
  <HostId>3l137</HostId>
</Error>
```

Clients surface that `Message` verbatim, so `mc` and `aws s3api` both report `CORSRule 0 has unsupported AllowedMethod "PATCH"` rather than the generic schema text.

## Preflight evaluation

Before a browser issues a cross-origin request that is not simple - anything using `PUT`, `DELETE`, or a custom header, for instance - it sends an `OPTIONS` request to the same URL announcing what it is about to do, in an `Origin` header, an `Access-Control-Request-Method` header, and optionally an `Access-Control-Request-Headers` header. That request is the preflight, and its answer decides whether the browser proceeds.

MinIO resolves the target bucket from the preflight - both path-style and virtual-host-style addressing are understood - loads that bucket's rules, and walks them **in document order**, taking the **first rule that matches on all three axes at once**:

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

The origin is echoed rather than answered with `*` because MinIO allows credentialed cross-origin requests, and a browser rejects `*` for those. The method and the headers are echoed for the same reason a browser announces them one request at a time: the answer only has to cover the request being asked about.

The following preflight matches the `web-app-uploads` rule of the document above:

```sh
curl -i -X OPTIONS http://localhost:9000/mybucket/ \
  -H "Origin: https://app.example.com" \
  -H "Access-Control-Request-Method: PUT" \
  -H "Access-Control-Request-Headers: content-type"
```

```sh
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

**The answer carries no `Access-Control-Allow-*` header at all, and that absence is the denial.** The status is still `200 OK`, because the preflight itself was a perfectly valid HTTP request; what tells the browser it may not proceed is that the response never names its origin. Only `Vary` is set. There is no error code to look for, so a client debugging a denied request should look for a missing `Access-Control-Allow-Origin` rather than for a failed status.

Changing the origin of the request above to one no rule names produces exactly that:

```sh
curl -i -X OPTIONS http://localhost:9000/mybucket/ \
  -H "Origin: http://evil.example.com" \
  -H "Access-Control-Request-Method: PUT" \
  -H "Access-Control-Request-Headers: content-type"
```

```sh
HTTP/1.1 200 OK
Vary: Origin
Vary: Access-Control-Request-Method
Vary: Access-Control-Request-Headers
```

The same answer is given whenever the request deviates on any single axis - a method no rule allows, or a single requested header no rule covers, is enough - and in two further cases:

- A preflight that announces an implausible number of distinct headers, far more than any browser sends, is refused outright instead of being matched against a shortened list, so no header is ever left unchecked against the rules.
- A bucket whose own rules cannot be established is refused rather than answered from the server-wide setting - while a freshly started server is still loading bucket metadata, for instance. This fails closed on purpose: a restrictively configured bucket is never answered out of a more permissive default merely because its rules were momentarily unavailable. The refusal costs the client nothing that outlives the condition, because it carries no `Access-Control-Max-Age` and a browser does not cache a refused preflight, so a retry once the rules are in hand is answered from them.

Be clear about what a denial is and is not. It is enforced by the browser, which withholds the response from the page that asked for it; it is not server-side access control. A client that speaks to MinIO directly is unaffected by CORS rules entirely, because nothing in the mechanism depends on it cooperating. Deciding who may read an object remains the job of credentials and bucket policies; a bucket's CORS rules decide only what a browser will do on a page's behalf.

## Relationship to the server-wide CORS setting

MinIO has always had a server-wide allow-origin list, `MINIO_API_CORS_ALLOW_ORIGIN`, which defaults to `'*'` and is documented with the rest of the `api` subsystem in the [MinIO Server Configuration Guide](https://github.com/minio/minio/blob/master/docs/config/README.md):

```sh
MINIO_API_CORS_ALLOW_ORIGIN               (csv)       set comma separated list of origins allowed for CORS requests (default: '*')
```

That setting is unchanged by per-bucket CORS and remains the server-wide control. The two divide cleanly, per bucket:

- A bucket that **has** a CORS configuration is answered from it, and only from it. Its rules decide, and the server-wide list is not consulted for that bucket's preflights.
- A bucket that has **no** CORS configuration is answered exactly as it was before, by the server-wide handler governed by `MINIO_API_CORS_ALLOW_ORIGIN`. Setting a configuration on one bucket changes nothing for any other bucket.

The two answers are easy to tell apart when you are checking which one you got: the server-wide handler replies to a preflight with `204 No Content` and includes `Access-Control-Allow-Credentials: true`, and it never emits `Access-Control-Max-Age` or `Access-Control-Expose-Headers`. A preflight answered from a bucket's own rules replies `200 OK` instead. Removing a bucket's configuration with `DeleteBucketCors` hands that bucket back to the server-wide setting.

The division is over preflights specifically, because that is where a bucket's rules are consulted. A cross-origin request simple enough that the browser sends no preflight at all - a plain `GET` carrying no custom header, say - is not preceded by one, so the CORS headers on its response continue to come from the server-wide handler exactly as they did before. Setting per-bucket rules is therefore how you constrain the requests a browser has to ask permission for, not a way to change how MinIO answers a request it is asked directly.

## Client compatibility

The wire format is the AWS one, so the operations work with the AWS SDKs and with any tool built on them. Two clients are worth showing explicitly, because they take the configuration in different shapes.

`mc` takes the XML document as it goes over the wire:

```sh
mc alias set myminio http://localhost:9000 minioadmin minioadmin
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

`DeleteBucketCors` requires its own `s3:DeleteBucketCors` action; permission to write a configuration does not imply permission to remove one. All three actions already existed in MinIO's policy vocabulary, so the capability ships without any change to the identity and access model, and a policy granting them is written like any other bucket policy statement:

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

The root credentials need no such policy, so an administrator can configure a bucket's CORS rules straight away and grant the actions to other identities afterwards.

## Explore Further

- [Use `mc` with MinIO Server](https://docs.min.io/community/minio-object-store/reference/minio-mc.html#quickstart)
- [Use `aws-cli` with MinIO Server](https://docs.min.io/community/minio-object-store/integrations/aws-cli-with-minio.html)
- [Use `minio-go` SDK with MinIO Server](https://docs.min.io/community/minio-object-store/developers/go/minio-go.html)
- [The MinIO documentation website](https://docs.min.io/community/minio-object-store/index.html)
