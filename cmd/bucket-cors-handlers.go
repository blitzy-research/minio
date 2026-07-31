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
	"encoding/xml"
	"net/http"

	"github.com/minio/minio/internal/logger"
	"github.com/minio/mux"
	"github.com/minio/pkg/v3/policy"
)

// PutBucketCorsHandler - PUT Bucket cors.
// ----------
// Stores the supplied CORSConfiguration document as bucket metadata, replacing
// any configuration previously stored on the bucket. Responds with HTTP 200 on
// success, which the AWS and MinIO SDKs both accept.
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

	// Check if bucket exists.
	if _, err := objectAPI.GetBucketInfo(ctx, bucket, BucketOptions{}); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutBucketCorsAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// The body is handed over as it is: validateBucketCorsConfig owns the size
	// ceiling, reading one byte past maxBucketCORSConfigSize so that an
	// oversized document is refused for its length instead of being accepted as
	// a truncated prefix of itself. r.ContentLength is deliberately not
	// consulted, it may be absent or negative on a chunked upload.
	cfg, err := validateBucketCorsConfig(r.Body)
	if err != nil {
		// Surface the specific validation cause to the client, the S3 error code
		// itself only says the document did not validate.
		apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
		apiErr.Description = err.Error()
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}

	// Persist the re-marshaled configuration rather than the raw body so the
	// stored document is canonical: the namespace is populated and every
	// AllowedMethod is upper cased by the parser.
	configData, err := xml.Marshal(cfg)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if _, err := globalBucketMetadataSys.Update(ctx, bucket, bucketCORSConfig, configData); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Write success response.
	writeSuccessResponseHeadersOnly(w)
}

// GetBucketCorsHandler - GET Bucket cors.
// ----------
// Returns the CORSConfiguration document stored on the bucket. A bucket with no
// stored configuration yields NoSuchCORSConfiguration with HTTP 404, which the
// BucketCORSConfigNotFound sentinel is mapped to by toAPIErrorCode.
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

	// check if user has permissions to perform this operation
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
	// namespace through its struct tags, so marshaling it reproduces the exact
	// wire format the AWS SDK, "aws s3api ... cors" and "mc" expect.
	configData, err := xml.Marshal(config)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Write success response.
	writeSuccessResponseXML(w, configData)
}

// DeleteBucketCorsHandler - DELETE Bucket cors.
// ----------
// Clears the CORS configuration stored on the bucket and responds with HTTP 204.
// The operation is idempotent: BucketMetadataSys.Delete clears the field
// unconditionally, so deleting a configuration that was never set succeeds too.
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

	// Write success response.
	writeSuccessNoContent(w)
}
