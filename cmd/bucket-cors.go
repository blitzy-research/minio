// Copyright (c) 2015-2025 MinIO, Inc.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	humanize "github.com/dustin/go-humanize"
	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/pkg/v3/wildcard"
)

const (
	// Bucket CORS configuration file.
	bucketCORSConfig = "cors.xml"

	// As per AWS S3 specification, a CORS configuration document is limited to
	// 64 KiB; MinIO's own SDK bounds its decoder at 128 KiB as a safety margin,
	// so the same ceiling is applied to the request body here.
	maxBucketCORSConfigSize = 128 * humanize.KiByte

	// Maximum number of CORSRule elements permitted in one configuration.
	maxBucketCORSRules = 100
)

// supportedCORSMethods is the exact set of HTTP methods that S3 accepts in an
// AllowedMethod element of a CORS rule. OPTIONS is deliberately absent: it is
// the preflight method itself and is never configured as an allowed method.
var supportedCORSMethods = map[string]struct{}{
	http.MethodGet:    {},
	http.MethodPut:    {},
	http.MethodPost:   {},
	http.MethodDelete: {},
	http.MethodHead:   {},
}

// validateBucketCorsConfig parses a bucket CORS configuration document and
// validates that it satisfies the constraints MinIO enforces on top of the
// schema, mirroring the S3 PutBucketCors contract.
//
// The size ceiling is enforced by the caller, which wraps the request body in
// an io.LimitReader bounded by maxBucketCORSConfigSize; a document larger than
// that is truncated and therefore fails to decode here.
//
// Every error returned by this function is surfaced verbatim to the client as
// the description of a MalformedXML S3 error, so each one names the offending
// rule index and the offending value.
func validateBucketCorsConfig(r io.Reader) (*miniogocors.Config, error) {
	cfg, err := miniogocors.ParseBucketCorsConfig(r)
	if err != nil {
		return nil, err
	}

	// A document whose root element is not CORSConfiguration is already
	// rejected while decoding, because the root element name is part of the
	// document model. This guard keeps that part of the contract explicit and
	// local, and protects against a document that decodes into an unexpected
	// root element name.
	if cfg.XMLName.Local != "CORSConfiguration" {
		return nil, fmt.Errorf("Unexpected root element %q, expected CORSConfiguration", cfg.XMLName.Local)
	}

	// S3 accepts between one and one hundred rules per configuration.
	if len(cfg.CORSRules) == 0 {
		return nil, errors.New("CORSConfiguration must contain at least one CORSRule")
	}
	if len(cfg.CORSRules) > maxBucketCORSRules {
		return nil, fmt.Errorf("CORSConfiguration contains %d CORSRule elements, at most %d are allowed",
			len(cfg.CORSRules), maxBucketCORSRules)
	}

	for i, rule := range cfg.CORSRules {
		if len(rule.AllowedMethod) == 0 {
			return nil, fmt.Errorf("CORSRule %d must contain at least one AllowedMethod", i)
		}
		for _, method := range rule.AllowedMethod {
			// Stored methods are normalized to upper case while parsing; the
			// explicit conversion keeps this check correct regardless.
			if _, ok := supportedCORSMethods[strings.ToUpper(method)]; !ok {
				return nil, fmt.Errorf("CORSRule %d has unsupported AllowedMethod %q", i, method)
			}
		}

		if len(rule.AllowedOrigin) == 0 {
			return nil, fmt.Errorf("CORSRule %d must contain at least one AllowedOrigin", i)
		}
		for _, origin := range rule.AllowedOrigin {
			// A bare "*" and a single embedded wildcard such as
			// "http://www.example.*" are both legal, more than one is not.
			if strings.Count(origin, "*") > 1 {
				return nil, fmt.Errorf("CORSRule %d has AllowedOrigin %q with more than one wildcard", i, origin)
			}
		}

		for _, header := range rule.AllowedHeader {
			if strings.Count(header, "*") > 1 {
				return nil, fmt.Errorf("CORSRule %d has AllowedHeader %q with more than one wildcard", i, header)
			}
		}

		if rule.MaxAgeSeconds < 0 {
			return nil, fmt.Errorf("CORSRule %d has negative MaxAgeSeconds %d", i, rule.MaxAgeSeconds)
		}
	}

	return cfg, nil
}

// corsRuleFor returns the first rule of cfg, in document order, that allows the
// supplied origin, the requested method and every requested header. It returns
// nil when no rule allows the request.
//
// S3 semantics are first matching rule wins: rules are never merged and never
// reordered, so the returned rule alone determines the preflight response.
func corsRuleFor(cfg *miniogocors.Config, origin, method string, reqHeaders []string) *miniogocors.Rule {
	if cfg == nil {
		return nil
	}
	for i := range cfg.CORSRules {
		rule := &cfg.CORSRules[i]
		if corsRuleAllowsOrigin(rule, origin) &&
			corsRuleAllowsMethod(rule, method) &&
			corsRuleAllowsHeaders(rule, reqHeaders) {
			return rule
		}
	}
	return nil
}

// corsRuleAllowsOrigin reports whether any AllowedOrigin of the rule covers the
// request origin. An origin matches when it is equal to the configured value or
// when it satisfies the single wildcard S3 permits, evaluated with the same
// matcher the server wide allow-origin list uses.
func corsRuleAllowsOrigin(rule *miniogocors.Rule, origin string) bool {
	for _, allowedOrigin := range rule.AllowedOrigin {
		if allowedOrigin == origin || wildcard.MatchSimple(allowedOrigin, origin) {
			return true
		}
	}
	return false
}

// corsRuleAllowsMethod reports whether any AllowedMethod of the rule covers the
// requested method. Configured methods are normalized to upper case while
// parsing and browsers send an upper case value, so the comparison is a plain
// case-insensitive equality check against the fixed set of supported methods.
func corsRuleAllowsMethod(rule *miniogocors.Rule, method string) bool {
	for _, allowedMethod := range rule.AllowedMethod {
		if strings.EqualFold(allowedMethod, method) {
			return true
		}
	}
	return false
}

// corsRuleAllowsHeaders reports whether every requested header is covered by an
// AllowedHeader of the rule. An empty request header list is trivially allowed,
// while a rule that lists no AllowedHeader covers no requested header at all
// and therefore cannot match a preflight that asks for one.
func corsRuleAllowsHeaders(rule *miniogocors.Rule, reqHeaders []string) bool {
	for _, reqHeader := range reqHeaders {
		if !corsRuleAllowsHeader(rule, reqHeader) {
			return false
		}
	}
	return true
}

// corsRuleAllowsHeader reports whether a single requested header is covered by
// an AllowedHeader of the rule. Header names are compared case-insensitively,
// a bare "*" covers every header and a single embedded wildcard such as
// "x-amz-*" covers every header with that prefix.
func corsRuleAllowsHeader(rule *miniogocors.Rule, reqHeader string) bool {
	for _, allowedHeader := range rule.AllowedHeader {
		if strings.EqualFold(allowedHeader, reqHeader) {
			return true
		}
		if wildcard.MatchSimple(strings.ToLower(allowedHeader), strings.ToLower(reqHeader)) {
			return true
		}
	}
	return false
}

// bucketCORSPreflightMiddleware answers browser CORS preflight requests from
// the CORS configuration stored on the target bucket.
//
// It wraps the server wide rs/cors handler built by corsHandler and is
// therefore the outermost HTTP layer. That placement is required: no mux route
// registers OPTIONS, so a preflight request never reaches an S3 handler and a
// mux middleware would be bypassed altogether for it.
//
// Anything that is not a per-bucket preflight is delegated untouched - a
// request that is not a preflight, a preflight without an origin to echo back,
// a preflight that does not resolve to a bucket, and a preflight for a bucket
// without a stored CORS configuration. Delegating is what keeps the server wide
// MINIO_API_CORS_ALLOW_ORIGIN setting in force as the global fallback.
func bucketCORSPreflightMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only CORS preflight requests are evaluated here. This is the same
		// predicate rs/cors uses to detect a preflight, so an OPTIONS request
		// without Access-Control-Request-Method - and every non-OPTIONS
		// request - keeps its existing behavior.
		if r.Method != http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		reqMethod := r.Header.Get("Access-Control-Request-Method")
		origin := r.Header.Get("Origin")
		if reqMethod == "" || origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Resolve the target bucket. request2BucketObjectName handles both
		// path-style and virtual-host-style addressing; a preflight for the
		// server root resolves to an empty bucket and has nothing to evaluate.
		bucket, _ := request2BucketObjectName(r)
		if bucket == "" {
			next.ServeHTTP(w, r)
			return
		}

		if globalBucketMetadataSys == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Any lookup failure, including the not-found sentinel, a nil
		// configuration and an empty rule set, falls back to the global CORS
		// handler.
		cfg, _, err := globalBucketMetadataSys.GetCORSConfig(bucket)
		if err != nil || cfg == nil || len(cfg.CORSRules) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		reqHeaders := parseCORSRequestHeaders(r.Header)
		rule := corsRuleFor(cfg, origin, reqMethod, reqHeaders)

		// The response depends on the request origin whether or not a rule
		// matched, so it always varies by Origin.
		header := w.Header()
		header.Add("Vary", "Origin")

		if rule == nil {
			// The bucket has rules but none of them allows this request.
			// Emitting no Access-Control-Allow-* header at all is how the
			// browser learns that the request is denied.
			w.WriteHeader(http.StatusOK)
			return
		}

		// The matched rule fully determines the response. The origin is echoed
		// back rather than answered with "*" because the server wide handler
		// allows credentials, and the requested method and headers are echoed
		// back because the allowed sets are unbounded in principle.
		header.Set("Access-Control-Allow-Origin", origin)
		header.Set("Access-Control-Allow-Methods", reqMethod)
		if len(reqHeaders) > 0 {
			header.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ", "))
		}
		if rule.MaxAgeSeconds > 0 {
			header.Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
		}
		if len(rule.ExposeHeader) > 0 {
			// The server wide handler never exposes headers on a preflight
			// response, so a matched rule's ExposeHeader list can only be
			// honored here.
			header.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeader, ", "))
		}
		w.WriteHeader(http.StatusOK)
	})
}

// parseCORSRequestHeaders returns the flattened, whitespace-trimmed union of
// every Access-Control-Request-Headers value carried by the request. The Fetch
// standard guarantees at most one such header, but some gateways split it into
// repeated fields, so all of them are considered.
func parseCORSRequestHeaders(h http.Header) []string {
	values := h.Values("Access-Control-Request-Headers")
	if len(values) == 0 {
		return nil
	}
	reqHeaders := make([]string, 0, len(values))
	for _, value := range values {
		for _, reqHeader := range strings.Split(value, ",") {
			if reqHeader = strings.TrimSpace(reqHeader); reqHeader != "" {
				reqHeaders = append(reqHeaders, reqHeader)
			}
		}
	}
	return reqHeaders
}
