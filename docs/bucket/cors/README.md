# Bucket CORS Configuration Guide [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io) [![Docker Pulls](https://img.shields.io/docker/pulls/minio/minio.svg?maxAge=604800)](https://hub.docker.com/r/minio/minio/)

Cross-Origin Resource Sharing (CORS) lets a web page served from one origin read from a bucket served from another. A browser asks for permission first, with an `OPTIONS` preflight request, and only issues the real request if the answer allows it.

MinIO stores a CORS configuration per bucket and answers preflight requests from it, implementing the S3 `PutBucketCors`, `GetBucketCors` and `DeleteBucketCors` operations. A bucket without a configuration of its own keeps the server-wide behavior described in [Relationship with `MINIO_API_CORS_ALLOW_ORIGIN`](#relationship-with-minio_api_cors_allow_origin), so this feature is opt-in per bucket and changes nothing until a configuration is set.

## Prerequisites

- Install MinIO - [MinIO Quickstart Guide](https://docs.min.io/community/minio-object-store/operations/deployments/baremetal-deploy-minio-on-redhat-linux.html#procedure).
- [Use `mc` with MinIO Server](https://docs.min.io/community/minio-object-store/reference/minio-mc.html#quickstart)

## Set a bucket CORS configuration

Write the configuration to a file, then apply it to the bucket.

```sh
cat > cors.xml <<EOF
<CORSConfiguration>
  <CORSRule>
    <ID>allow-web-app</ID>
    <AllowedOrigin>https://app.example.com</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>3000</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>
EOF

mc cors set myminio/mybucket cors.xml
```

The wire format is the S3 one, so AWS SDKs and the AWS CLI configure the same bucket the same way. The AWS CLI expresses the configuration as JSON rather than XML:

```sh
cat > cors.json <<EOF
{"CORSRules": [{
  "ID": "allow-web-app",
  "AllowedOrigins": ["https://app.example.com"],
  "AllowedMethods": ["GET", "PUT"],
  "AllowedHeaders": ["*"],
  "ExposeHeaders": ["ETag"],
  "MaxAgeSeconds": 3000
}]}
EOF

aws s3api put-bucket-cors --bucket mybucket --cors-configuration file://cors.json
```

A successful `PUT` replaces the whole configuration; rules are never merged into what was stored before.

## Get a bucket CORS configuration

```sh
mc cors get myminio/mybucket
aws s3api get-bucket-cors --bucket mybucket
```

The response is the stored `CORSConfiguration` document. When the bucket has no configuration, the request fails with `NoSuchCORSConfiguration` and HTTP 404.

## Remove a bucket CORS configuration

```sh
mc cors remove myminio/mybucket
aws s3api delete-bucket-cors --bucket mybucket
```

Removal is idempotent: deleting a configuration that was never set succeeds as well, and afterwards the bucket falls back to the server-wide behavior.

## The configuration document

The root element is `CORSConfiguration` and it holds between 1 and 100 `CORSRule` elements. Element names are those AWS defines, including their capitalization.

| Element | Occurrences | Meaning |
| :--- | :--- | :--- |
| `ID` | 0 or 1 | A label for the rule. It is stored and returned, and plays no part in matching. |
| `AllowedOrigin` | 1 or more | An origin the rule applies to. Either an exact origin, the bare wildcard `*`, or a value containing one `*`, such as `https://*.example.com`. |
| `AllowedMethod` | 1 or more | One of `GET`, `PUT`, `POST`, `DELETE`, `HEAD`. `OPTIONS` is the preflight method itself and is never configured. |
| `AllowedHeader` | 0 or more | A request header the rule permits, compared case-insensitively. May be `*`, or contain one `*`, such as `x-amz-*`. |
| `ExposeHeader` | 0 or more | A response header the browser is allowed to read. |
| `MaxAgeSeconds` | 0 or 1 | How long, in seconds, the browser may cache the answer. Omitting it, or setting it to 0, tells the browser not to cache. |

An element the schema does not define, a misspelled element name, a repeated `ID` or `MaxAgeSeconds`, an element nested inside a value, or anything at all following the document is rejected rather than ignored, so a configuration is never stored with part of it silently discarded.

## Validation

Every rejection is reported as `MalformedXML` with HTTP 400, and the message names the specific cause, including the index of the offending rule.

| Rejected when | Example message |
| :--- | :--- |
| The body is not well-formed XML, or its root element is not `CORSConfiguration` | `Unexpected root element "CORSConfig", expected CORSConfiguration` |
| The document declares an XML namespace other than `http://s3.amazonaws.com/doc/2006-03-01/` | `Unexpected XML namespace "urn:example" on the CORSConfiguration element` |
| The document carries a document type declaration | `Unexpected document type declaration in the CORSConfiguration document` |
| There is no rule, or there are more than 100 | `CORSConfiguration contains 101 CORSRule elements, at most 100 are allowed` |
| A rule has no `AllowedMethod`, or one outside the supported set | `CORSRule 0 has unsupported AllowedMethod "PATCH"` |
| A rule has no `AllowedOrigin` | `CORSRule 0 must contain at least one AllowedOrigin` |
| An `AllowedOrigin` or `AllowedHeader` contains more than one `*` | `CORSRule 0 has AllowedOrigin "https://*.example.*" with more than one wildcard` |
| An `AllowedOrigin`, `AllowedHeader` or `ExposeHeader` contains a control character | `CORSRule 0 has ExposeHeader "ETag\nX-Injected: yes" containing a control character` |
| `MaxAgeSeconds` is negative, not a number, or too large | `CORSRule 0 has negative MaxAgeSeconds -1` |
| The document is larger than 131072 bytes | `CORSConfiguration document is larger than the maximum of 131072 bytes` |
| The document fits, but the configuration would exceed 131072 bytes once stored in the canonical form that declares the S3 namespace | `CORSConfiguration document is larger than the maximum of 131072 bytes once stored in its canonical form` |

Two forms of insignificant markup that hand-written documents carry are accepted and normalized away, so a document written in an editor means what it reads: a leading UTF-8 byte order mark, and the whitespace surrounding a value. An `AllowedOrigin` written on a line of its own is therefore stored as the origin alone, and matches the origin a browser sends.

## Preflight evaluation

For an `OPTIONS` request carrying `Origin` and `Access-Control-Request-Method`, MinIO reads the target bucket's rules and evaluates them **in document order, and the first rule that matches wins**. Rules are never merged, so only the matched rule shapes the response. A rule matches when all three of the following hold.

1. One of its `AllowedOrigin` values covers the request `Origin`.
2. One of its `AllowedMethod` values is the requested method.
3. **Every** header named in `Access-Control-Request-Headers` is covered by one of its `AllowedHeader` values. A rule that lists no `AllowedHeader` matches only a request that asks for no header.

A matched rule produces HTTP 200 with the headers derived from it:

```sh
curl -i -X OPTIONS http://localhost:9000/mybucket/myobject \
  -H "Origin: https://app.example.com" \
  -H "Access-Control-Request-Method: PUT" \
  -H "Access-Control-Request-Headers: x-amz-acl"

HTTP/1.1 200 OK
Access-Control-Allow-Origin: https://app.example.com
Access-Control-Allow-Methods: PUT
Access-Control-Allow-Headers: x-amz-acl
Access-Control-Max-Age: 3000
Access-Control-Expose-Headers: ETag
Vary: Origin
Vary: Access-Control-Request-Method
Vary: Access-Control-Request-Headers
```

`Access-Control-Allow-Origin` echoes the request origin rather than answering `*`, because MinIO allows credentials on cross-origin requests. `Access-Control-Max-Age` is emitted only when the matched rule sets a positive `MaxAgeSeconds`, and `Access-Control-Expose-Headers` only when it lists at least one `ExposeHeader`.

When the bucket has rules but none of them matches, the response is HTTP 200 carrying **no** `Access-Control-Allow-*` header at all. That absence is how the browser learns the request is not permitted, and it is what a disallowed origin, a disallowed method, or an unlisted request header each produce.

## Relationship with `MINIO_API_CORS_ALLOW_ORIGIN`

`MINIO_API_CORS_ALLOW_ORIGIN` is the server-wide list of origins MinIO accepts cross-origin requests from, and it defaults to `*`. It is unchanged by this feature and remains the control for a deployment that wants one policy everywhere.

The two work together as follows.

- A bucket with **no** CORS configuration is served exactly as before: preflight requests are answered from the server-wide setting.
- A bucket **with** a CORS configuration has its preflight requests answered from its own rules, which are evaluated in full and can deny a request the server-wide setting would allow.

So a bucket configuration narrows browser access for that bucket, and no bucket loses the server-wide default until a configuration is set on it.

## Behavior worth knowing

- **The document ceiling is 131072 bytes (128 KiB)**, where AWS documents 64 KiB. It is deliberately the more permissive of the two, so a document AWS accepts is always within it. An oversized body is refused for its length, never accepted as a truncated prefix of itself. The ceiling applies both to the body as it arrives and to the document as it is stored, which is the same configuration re-serialized with the S3 namespace declared, so a body within a few dozen bytes of the ceiling that omits `xmlns` is refused rather than stored in a form that could not be read back.
- **A preflight naming more than 64 distinct headers is denied**, which is stricter than AWS. It is the one deliberately stricter limit here: an unauthenticated preflight cannot choose how much matching work the server performs, and a request that cannot be evaluated in full is refused rather than partially checked. No browser approaches this number, since a browser lists only the headers the request it is about to make actually carries.
- **Attributes on a rule or its children are ignored**, as they are by AWS. Only element values configure a rule.
- **An `AllowedOrigin` with no value is accepted and matches nothing**, as it is by AWS. It names the empty origin, which no browser sends, so such a rule can only deny. Because the stored document is the canonical form, an element with no value is not part of what is stored: a rule whose only `AllowedOrigin` was empty comes back from `GetBucketCors` naming no origin at all. Give every rule an origin it is meant to allow.
- **CORS is a per-bucket configuration**, stored alongside the bucket's other configurations, and every node of the deployment reloads it as soon as it is written. Object-level CORS does not exist in S3 and is not implemented.
- **The three operations are authorized individually** through the `s3:PutBucketCors`, `s3:GetBucketCors` and `s3:DeleteBucketCors` actions, so read, write and removal can be granted separately in a policy.

## Explore Further

- [MinIO Bucket Versioning Guide](https://github.com/minio/minio/blob/master/docs/bucket/versioning/README.md)
- [MinIO Server Configuration Guide](https://github.com/minio/minio/blob/master/docs/config/README.md)
- [Use `mc` with MinIO Server](https://docs.min.io/community/minio-object-store/reference/minio-mc.html)
