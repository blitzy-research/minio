// Copyright (c) 2015-2026 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/minio/minio/internal/etag"
	"github.com/minio/minio/internal/hash"
	xhttp "github.com/minio/minio/internal/http"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/mux"
	"github.com/minio/pkg/v3/policy"
)

// PutBucketCorsHandler - PUT Bucket CORS.
// ----------
// Stores the bucket CORS configuration, replacing any previous one, and returns
// HTTP 200.
func (api objectAPIHandlers) PutBucketCorsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutBucketCors")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if _, err := objectAPI.GetBucketInfo(ctx, bucket, BucketOptions{}); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutBucketCorsAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Verify whatever the client declared about the bytes it is sending before
	// any of them is turned into stored configuration.
	body, s3Error := corsConfigBody(ctx, r)
	if s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// validateBucketCorsConfig owns the fixed body limit and reads one byte past
	// it, so chunked requests remain bounded when ContentLength is absent or
	// negative.
	cfg, err := validateBucketCorsConfig(body)
	if err != nil {
		writeErrorResponse(ctx, w, corsConfigAPIError(ctx, err), r.URL)
		return
	}

	// Persist the re-marshaled configuration rather than the raw body so the
	// stored document is canonical: the namespace is populated and every
	// AllowedMethod is uppercased by the parser.
	configData, err := xml.Marshal(cfg)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if _, err := globalBucketMetadataSys.Update(ctx, bucket, bucketCORSConfig, configData); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	writeSuccessResponseHeadersOnly(w)
}

// corsConfigBody returns the reader the configuration document is read from: the
// request body, wrapped so that everything the client declared about the bytes it
// is sending is verified while they are consumed.
//
// The verification cannot be left to authentication, because what each
// authentication path installs differs and none of them covers everything:
//
//   - A SigV4 or presigned-SigV4 request reaches this handler with its body
//     already replaced by a reader that verifies Content-MD5 and
//     x-amz-content-sha256, installed by isReqAuthenticated. Such a body is not
//     wrapped again - re-deriving the expected values from the headers would
//     discard the ones a presigned request carries in its query string, and
//     hashing the same bytes twice would be pure waste.
//   - A SigV2 or presigned-SigV2 request reaches it with its body untouched.
//     isReqAuthenticatedV2 verifies the signature and nothing else, and the V2
//     signature covers the Content-MD5 header rather than the body, so a client's
//     declared digest is authenticated while the bytes it describes are not. An
//     on-path attacker can therefore replace the document and leave the signed
//     digest in place unless the digest is checked here.
//   - An anonymous request authorized by a bucket policy reaches it with its body
//     untouched as well, since no signature was presented to install anything.
//   - No authentication path installs x-amz-checksum-* verification for any
//     signature version; handlers that accept one do it themselves.
//
// Verifying only what the client declared keeps this compatible with clients that
// declare nothing: an absent Content-MD5, x-amz-content-sha256 and
// x-amz-checksum-* leave the body unverified and accepted, which is what the SDKs
// that send a CORS configuration without a digest rely on. A declared value that
// cannot be parsed is refused here, and one that does not match the bytes is
// refused by the read, each with the S3 error code that names it.
func corsConfigBody(ctx context.Context, r *http.Request) (io.Reader, APIErrorCode) {
	reader, verified := r.Body.(*hash.Reader)
	if !verified {
		clientETag, err := etag.FromContentMD5(r.Header)
		if err != nil {
			return nil, ErrInvalidDigest
		}

		// skipContentSha256Cksum decides whether the header carries a digest to
		// verify at all, so the values S3 defines in its place - UNSIGNED-PAYLOAD
		// and the streaming forms - are not mistaken for a malformed one.
		var contentSHA256 []byte
		if !skipContentSha256Cksum(r) {
			contentSHA256, err = hex.DecodeString(r.Header.Get(xhttp.AmzContentSha256))
			if err != nil || len(contentSHA256) == 0 {
				return nil, ErrContentSHA256Mismatch
			}
		}

		reader, err = hash.NewReader(ctx, r.Body, -1, clientETag.String(), hex.EncodeToString(contentSHA256), -1)
		if err != nil {
			return nil, toAPIErrorCode(ctx, err)
		}
	}

	// The one verification no authentication path installs. AddChecksum also
	// wires up the request trailer, so a checksum a client sends after the body
	// is verified against the bytes rather than reported as missing.
	if err := reader.AddChecksum(r, false); err != nil {
		return nil, toAPIErrorCode(ctx, err)
	}
	return reader, ErrNone
}

// corsConfigAPIError maps a failure reported by validateBucketCorsConfig to the
// S3 error the client is answered with.
//
// A failure raised while the body was read keeps the code that names it rather
// than being reported as a schema violation: the integrity failures corsConfigBody
// verifies surface with their own codes, and a transfer that ended early becomes
// IncompleteBody through toObjectErr, the same conversion the object write path
// applies. A cause neither recognizes remains a server error.
//
// A document that did not validate answers MalformedXML carrying the specific
// cause, because the code itself only says the document did not validate and the
// cause is what tells the client which rule and which value to correct.
func corsConfigAPIError(ctx context.Context, err error) APIError {
	var readErr corsConfigReadError
	if errors.As(err, &readErr) {
		return toAPIError(ctx, toObjectErr(readErr.cause))
	}

	apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
	apiErr.Description = err.Error()
	return apiErr
}

// GetBucketCorsHandler - GET Bucket CORS.
// ----------
// Returns the stored bucket CORS configuration, or NoSuchCORSConfiguration with
// HTTP 404 when the bucket has none.
func (api objectAPIHandlers) GetBucketCorsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetBucketCors")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.GetBucketCorsAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	config, _, err := globalBucketMetadataSys.GetCORSConfig(bucket)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// The parsed model carries the CORSConfiguration root element and the S3
	// namespace through its struct tags, so marshaling it yields the
	// AWS-compatible CORS XML expected by SDK and CLI clients.
	configData, err := xml.Marshal(config)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	writeSuccessResponseXML(w, configData)
}

// DeleteBucketCorsHandler - DELETE Bucket CORS.
// ----------
// Removes the bucket CORS configuration and returns HTTP 204. The removal is
// idempotent, so a bucket that never had one succeeds too.
func (api objectAPIHandlers) DeleteBucketCorsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "DeleteBucketCors")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.DeleteBucketCorsAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	if _, err := globalBucketMetadataSys.Delete(ctx, bucket, bucketCORSConfig); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	writeSuccessNoContent(w)
}
