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
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	humanize "github.com/dustin/go-humanize"
	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

const (
	bucketCORSConfig = "cors.xml"

	// As per AWS S3 specification, a CORS configuration document is limited to
	// 64 KiB; MinIO's own SDK bounds its decoder at 128 KiB as a safety margin,
	// so the same ceiling is applied to the request body here. The ceiling is
	// enforced by validateBucketCorsConfig itself, which refuses a body that
	// exceeds it instead of validating a truncated prefix of it.
	maxBucketCORSConfigSize = 128 * humanize.KiByte

	maxBucketCORSRules = 100

	// Maximum number of distinct headers an OPTIONS preflight may ask about.
	// A browser lists only the headers the request it is about to make
	// actually carries, so this ceiling is far above what any real client
	// sends, while it bounds the work an unauthenticated preflight can ask the
	// rule matcher to perform.
	maxCORSPreflightRequestHeaders = 64

	// corsConfigXMLNS is the XML namespace of every S3 CORS document. A client
	// either omits the namespace, in which case the parser defaults it to this
	// value, or declares exactly this one. Any other namespace is rejected: the
	// parser preserves it, so it would be persisted and handed back to clients
	// that expect the AWS wire format.
	corsConfigXMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

	// Element names of a CORS document, spelled exactly as AWS spells them.
	corsConfigurationElement = "CORSConfiguration"
	corsRuleElement          = "CORSRule"
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

// corsRuleElements is the exact set of child elements a CORSRule may contain,
// mapped to whether S3 allows the element at most once. ID and MaxAgeSeconds
// are single valued, the four remaining elements may repeat.
var corsRuleElements = map[string]bool{
	"AllowedHeader": false,
	"AllowedMethod": false,
	"AllowedOrigin": false,
	"ExposeHeader":  false,
	"ID":            true,
	"MaxAgeSeconds": true,
}

// corsRuleElementOrder lists the child elements of a CORSRule in the order a
// cardinality violation is reported, so a rule that repeats more than one
// single valued element always fails on the same one.
var corsRuleElementOrder = []string{
	"AllowedHeader",
	"AllowedMethod",
	"AllowedOrigin",
	"ExposeHeader",
	"ID",
	"MaxAgeSeconds",
}

// validateBucketCorsConfig reads a bucket CORS configuration document and
// validates that it satisfies the schema and the constraints MinIO enforces on
// top of it, mirroring the S3 PutBucketCors contract. It returns the parsed
// configuration, which is the canonical form that is persisted.
//
// Callers hand the request body over directly: the size ceiling is enforced
// here, by reading one byte more than maxBucketCORSConfigSize and refusing a
// body that long, so an oversized document is rejected outright rather than
// accepted as a truncated prefix of itself. The body is buffered because
// duplicate ID and MaxAgeSeconds elements cannot be detected after decoding
// into the SDK model.
func validateBucketCorsConfig(r io.Reader) (*miniogocors.Config, error) {
	// Reading one byte past the ceiling is what makes the limit provable: a
	// document of exactly maxBucketCORSConfigSize bytes is still accepted,
	// while a longer one is detected here instead of being silently truncated
	// into an XML decoding failure - or, worse, into a valid document followed
	// by discarded padding.
	data, err := io.ReadAll(io.LimitReader(r, maxBucketCORSConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("Unable to read the %s document: %w", corsConfigurationElement, err)
	}
	if len(data) > maxBucketCORSConfigSize {
		return nil, fmt.Errorf("%s document is larger than the maximum of %d bytes",
			corsConfigurationElement, maxBucketCORSConfigSize)
	}

	// The document model cannot express element cardinality, silently discards
	// elements it does not know and ignores everything that follows the root
	// element, so those parts of the schema are validated against the bytes the
	// client sent rather than against the parsed model. The walk runs before the
	// model is decoded, so a schema violation is reported as such instead of as
	// whatever the model made of it - a repeated MaxAgeSeconds whose second
	// occurrence is not a number being the clearest example.
	if err := validateCorsDocumentSchema(data); err != nil {
		return nil, err
	}

	cfg, err := miniogocors.ParseBucketCorsConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	// validateCorsDocumentSchema has already pinned the root element name in
	// the token stream. Asserting it again on the decoded model keeps the
	// invariant local to the value that is about to be persisted, so the two
	// layers cannot silently drift apart.
	if cfg.XMLName.Local != corsConfigurationElement {
		return nil, fmt.Errorf("Unexpected root element %q, expected %s", cfg.XMLName.Local, corsConfigurationElement)
	}

	// Requirement R8, the AWS wire format. The parser defaults an absent
	// namespace to the S3 namespace but preserves any other value, and the
	// marshaller emits whatever it holds, so a document declaring a different
	// namespace has to be rejected here. Both forms matter and neither implies
	// the other: the resolved element namespace can be correct while a default
	// xmlns attribute alongside a prefixed element name is not.
	if cfg.XMLName.Space != "" && cfg.XMLName.Space != corsConfigXMLNS {
		return nil, fmt.Errorf("Unexpected XML namespace %q on the %s element, expected %s",
			cfg.XMLName.Space, corsConfigurationElement, corsConfigXMLNS)
	}
	if cfg.XMLNS != corsConfigXMLNS {
		return nil, fmt.Errorf("Unexpected xmlns attribute %q, expected %s", cfg.XMLNS, corsConfigXMLNS)
	}

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

// validateCorsDocumentSchema walks the received document token by token and
// enforces the parts of the S3 CORS schema that the document model cannot
// express: which element may appear where, how often a single valued element
// may appear, and that nothing but insignificant markup follows the root
// element.
//
// The strictness is deliberate. An element the model does not recognize is
// discarded while decoding, so a client that misspells AllowedOrigin, repeats
// MaxAgeSeconds or appends a second document would otherwise be told its
// configuration was stored while the bucket ended up with rules it never
// intended. A document type declaration is refused wherever it appears, which
// keeps entity handling out of the picture entirely.
func validateCorsDocumentSchema(data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))

	root, err := corsDocumentRootElement(dec)
	if err != nil {
		return err
	}

	// Every element of a CORS document belongs to the namespace of its root
	// element, whether that namespace is empty or the S3 one, so the root
	// namespace is what every element name below is required to carry. index is
	// the position of the rule being validated in document order, which is the
	// index every failure below reports.
	index := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decoding xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != root.Name.Space || t.Name.Local != corsRuleElement {
				return fmt.Errorf("%s contains unsupported element %q",
					corsConfigurationElement, corsElementName(t.Name, root.Name.Space))
			}
			if err := validateCorsRuleElement(dec, root.Name.Space, index); err != nil {
				return err
			}
			index++
		case xml.EndElement:
			// The root element is the only element that can end at this depth,
			// because every CORSRule is consumed whole above.
			return validateCorsDocumentTrailer(dec)
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return fmt.Errorf("%s contains character data outside of a %s element",
					corsConfigurationElement, corsRuleElement)
			}
		case xml.Directive:
			return fmt.Errorf("Unexpected document type declaration in the %s document",
				corsConfigurationElement)
		}
		// Comments and processing instructions carry no configuration, so they
		// are insignificant and simply skipped.
	}
}

// corsDocumentRootElement advances the decoder to the root element of the
// document and enforces that it is a CORSConfiguration element. The XML
// declaration, comments and surrounding whitespace are tolerated, all of which a
// real client may send, while a document type declaration is refused: a CORS
// configuration has no legitimate use for one, and refusing it keeps entity
// handling out of the picture entirely.
func corsDocumentRootElement(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return xml.StartElement{}, fmt.Errorf("decoding xml: the document does not contain a %s element",
					corsConfigurationElement)
			}
			return xml.StartElement{}, fmt.Errorf("decoding xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != corsConfigurationElement {
				return xml.StartElement{}, fmt.Errorf("Unexpected root element %q, expected %s",
					t.Name.Local, corsConfigurationElement)
			}
			return t, nil
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return xml.StartElement{}, fmt.Errorf("The document contains character data before the %s element",
					corsConfigurationElement)
			}
		case xml.Directive:
			return xml.StartElement{}, fmt.Errorf("Unexpected document type declaration in the %s document",
				corsConfigurationElement)
		}
	}
}

// validateCorsRuleElement consumes one CORSRule element and enforces that it
// contains only the child elements AWS defines, in the namespace of the
// document, that ID and MaxAgeSeconds appear at most once, and that no child
// carries nested elements. index is the position of the rule in document order
// and is reported with every failure.
func validateCorsRuleElement(dec *xml.Decoder, space string, index int) error {
	seen := make(map[string]int, len(corsRuleElements))
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decoding xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if _, known := corsRuleElements[t.Name.Local]; !known || t.Name.Space != space {
				return fmt.Errorf("%s %d contains unsupported element %q",
					corsRuleElement, index, corsElementName(t.Name, space))
			}
			seen[t.Name.Local]++
			if err := validateCorsRuleValueElement(dec, space, index, t.Name.Local); err != nil {
				return err
			}
		case xml.EndElement:
			// Closes the CORSRule element itself. The single valued elements are
			// reported once the whole rule has been read, so the failure names
			// the total number of occurrences rather than the first repetition.
			for _, name := range corsRuleElementOrder {
				if single := corsRuleElements[name]; single && seen[name] > 1 {
					return fmt.Errorf("%s %d contains %d %s elements, at most one is allowed",
						corsRuleElement, index, seen[name], name)
				}
			}
			return nil
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return fmt.Errorf("%s %d contains character data outside of a child element",
					corsRuleElement, index)
			}
		case xml.Directive:
			return fmt.Errorf("Unexpected document type declaration in the %s document",
				corsConfigurationElement)
		}
	}
}

// validateCorsRuleValueElement consumes one child element of a CORSRule and
// enforces that it carries a text value only. A nested element would be
// discarded by the document model, leaving the client no way to learn that the
// value it configured was ignored.
func validateCorsRuleValueElement(dec *xml.Decoder, space string, index int, name string) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decoding xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return fmt.Errorf("%s %d has %s containing the nested element %q, a text value is expected",
				corsRuleElement, index, name, corsElementName(t.Name, space))
		case xml.EndElement:
			return nil
		case xml.Directive:
			return fmt.Errorf("Unexpected document type declaration in the %s document",
				corsConfigurationElement)
		}
	}
}

// validateCorsDocumentTrailer enforces that nothing of substance follows the
// root element. The decoder happily tokenizes trailing character data and even
// a second root element, and the document model stops at the first root, so a
// document with a tail would otherwise be accepted with the tail discarded.
//
// A trailing element is rejected whatever namespace it belongs to, so it is
// reported by its local name alone.
func validateCorsDocumentTrailer(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decoding xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return fmt.Errorf("The document contains the element %q after the %s element",
				t.Name.Local, corsConfigurationElement)
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return fmt.Errorf("The document contains character data after the %s element",
					corsConfigurationElement)
			}
		case xml.Directive:
			return fmt.Errorf("Unexpected document type declaration in the %s document",
				corsConfigurationElement)
		}
	}
}

// corsElementName renders an element name for a client visible error. space is
// the namespace of the document: an element that belongs to it is reported by
// its bare name, while an element from any other namespace is reported in the
// conventional {namespace}local form, so an element rejected for its namespace
// is not reported by a name that looks correct.
func corsElementName(name xml.Name, space string) string {
	if name.Space == space {
		return name.Local
	}
	return "{" + name.Space + "}" + name.Local
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

// corsWildcardMatch reports whether name satisfies pattern under the pattern
// language S3 defines for AllowedOrigin and AllowedHeader: every character is
// literal except a single "*", which stands for any sequence of characters. A
// pattern without a wildcard therefore matches only itself.
//
// It deliberately does not reuse the general wildcard matcher the server wide
// allow-origin list applies. That matcher also honors "?" as a single-character
// wildcard, which would silently widen a stored rule beyond the pattern language
// PutBucketCors accepts and grant an origin the bucket owner never configured,
// and it backtracks recursively for a "*" followed by a suffix, which an
// unauthenticated preflight could use to drive deep recursion. Two bounded
// prefix and suffix comparisons implement the S3 language exactly, in linear
// time and without recursion.
//
// A pattern carrying more than one "*" is rejected before it can be persisted,
// so it cannot reach this function through the configured path. Should one
// arrive anyway, every "*" after the first is compared literally, which can only
// ever narrow the match.
func corsWildcardMatch(pattern, name string) bool {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return pattern == name
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	// The prefix and the suffix must not overlap, otherwise a single character
	// of name could be claimed by both of them.
	if len(name) < len(prefix)+len(suffix) {
		return false
	}
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// corsRuleAllowsOrigin reports whether any AllowedOrigin of the rule covers the
// request origin. An origin matches when it is equal to the configured value or
// when it satisfies the single wildcard S3 permits, so a bare "*" covers every
// origin and "http://www.example2.*" covers every origin with that prefix.
func corsRuleAllowsOrigin(rule *miniogocors.Rule, origin string) bool {
	for _, allowedOrigin := range rule.AllowedOrigin {
		if corsWildcardMatch(allowedOrigin, origin) {
			return true
		}
	}
	return false
}

// corsRuleAllowsMethod reports whether the rule contains the requested method.
// Case-insensitive comparison also supports configurations constructed without
// parser normalization.
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
	lowerReqHeader := strings.ToLower(reqHeader)
	for _, allowedHeader := range rule.AllowedHeader {
		// Lower-casing both sides makes the comparison case-insensitive for
		// the literal parts of the pattern as well as for a plain header name,
		// so no separate equality check is needed.
		if corsWildcardMatch(strings.ToLower(allowedHeader), lowerReqHeader) {
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
// mux middleware would be bypassed altogether for it. It also means this code
// runs before any authentication, on a request whose path and headers are
// entirely client controlled, so it does exactly two things with them: read the
// in-memory bucket metadata, and answer.
//
// Requests are disposed of in one of three ways.
//
// Delegated untouched, so the server wide MINIO_API_CORS_ALLOW_ORIGIN setting
// stays in force exactly as before this feature existed: anything that is not a
// preflight, a preflight without an origin to echo back, a preflight that does
// not resolve to a syntactically valid bucket, and a preflight for a bucket
// that definitively has no CORS configuration.
//
// Answered from the matched rule, with the five Access-Control-Allow families
// and HTTP 200.
//
// Denied - HTTP 200 carrying no Access-Control-Allow-* header at all, which is
// how a browser learns the request is not permitted: the bucket has rules but
// none of them allows this request, the request asks about an implausible
// number of headers, or the server cannot currently establish what the bucket's
// rules are. That last case is deliberate: treating an unavailable or
// unreadable configuration as no configuration would silently hand the request
// to the permissive server wide default, so a restrictive bucket policy would
// relax during exactly the moments it matters most.
func bucketCORSPreflightMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match the rs/cors preflight gate, plus the Origin that a matched rule
		// echoes back, so incomplete OPTIONS requests continue to the global
		// handler.
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

		// A name that cannot be a bucket cannot carry a CORS configuration, so
		// it is turned away here rather than looked up. Checking the syntax
		// first is what keeps unauthenticated, arbitrary path segments away
		// from the metadata layer altogether.
		if s3utils.CheckValidBucketNameStrict(bucket) != nil {
			next.ServeHTTP(w, r)
			return
		}

		cfg, err := preflightCORSConfig(bucket)
		switch {
		case err == nil:
			// A configuration is available; fall through and evaluate it.
		case isBucketCORSConfigNotFound(err):
			// The bucket definitively has no CORS configuration of its own,
			// which is the one and only case that falls back to the server
			// wide handler.
			next.ServeHTTP(w, r)
			return
		default:
			// The configuration could not be established - the metadata
			// subsystem is absent or still loading, the stored document is
			// unusable, or the lookup failed. Deny rather than fall back.
			writeCORSPreflightDenied(w)
			return
		}

		reqHeaders, ok := parseCORSRequestHeaders(r.Header)
		if !ok {
			// The request asks about more headers than any browser would, so it
			// is denied outright rather than matched against a shortened list.
			writeCORSPreflightDenied(w)
			return
		}

		rule := corsRuleFor(cfg, origin, reqMethod, reqHeaders)
		if rule == nil {
			// The bucket has rules but none of them allows this request.
			writeCORSPreflightDenied(w)
			return
		}

		header := w.Header()
		setCORSPreflightVary(header)

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

// preflightCORSConfig returns the usable CORS configuration of a bucket for the
// purpose of answering a preflight request.
//
// The lookup is strictly in-memory. An unauthenticated preflight names its
// bucket in the request path, so resolving it through the loading accessor would
// let anyone turn a stream of made-up names into backend metadata reads and into
// permanent entries in the bucket metadata map, with work that outlives the
// request because that accessor is not request scoped.
//
// A configuration that parsed but carries no rule cannot have come from
// PutBucketCors, which requires at least one, so it is reported as an error
// rather than as an absence: the caller denies on it instead of falling back to
// the server wide default.
func preflightCORSConfig(bucket string) (*miniogocors.Config, error) {
	if globalBucketMetadataSys == nil {
		return nil, errServerNotInitialized
	}
	cfg, _, err := globalBucketMetadataSys.GetCORSConfigCached(bucket)
	if err != nil {
		return nil, err
	}
	if cfg == nil || len(cfg.CORSRules) == 0 {
		return nil, fmt.Errorf("Stored CORS configuration for bucket %s contains no CORSRule", bucket)
	}
	return cfg, nil
}

// isBucketCORSConfigNotFound reports whether err is the concrete sentinel that
// says a bucket has no CORS configuration, as opposed to any other reason the
// configuration could not be produced. Only that one condition may be treated as
// an absence, so the test is deliberately narrow.
func isBucketCORSConfigNotFound(err error) bool {
	var notFound BucketCORSConfigNotFound
	return errors.As(err, &notFound)
}

// setCORSPreflightVary declares which request headers the preflight response
// depends on, so an intermediary cache keys on all three of them rather than
// serving one origin's answer to another. It matches the variance the server
// wide rs/cors handler declares on its own preflight responses.
func setCORSPreflightVary(header http.Header) {
	header.Add("Vary", "Origin")
	header.Add("Vary", "Access-Control-Request-Method")
	header.Add("Vary", "Access-Control-Request-Headers")
}

// writeCORSPreflightDenied answers a preflight request without a single
// Access-Control-Allow-* header, which is how a browser learns that the request
// is not permitted.
func writeCORSPreflightDenied(w http.ResponseWriter) {
	setCORSPreflightVary(w.Header())
	w.WriteHeader(http.StatusOK)
}

// parseCORSRequestHeaders returns the flattened, whitespace-trimmed union of
// every Access-Control-Request-Headers value carried by the request, and reports
// whether the request asks about a plausible number of headers. The Fetch
// standard guarantees at most one such header, but some gateways split it into
// repeated fields, so all of them are considered.
//
// Header names are case-insensitive, so a name repeated in any casing is one
// requested header: it is kept once, in the casing and at the position it first
// appeared, which is both what the matcher has to satisfy and what the response
// echoes back. Collapsing duplicates also stops a repetitive request from
// multiplying the work the matcher performs.
//
// A request naming more than maxCORSPreflightRequestHeaders distinct headers is
// reported as not evaluable, and the caller must deny it. Truncating the list
// instead would risk allowing headers that no rule was ever checked against, and
// evaluating it in full would let an unauthenticated request choose how much
// matching work the server does.
func parseCORSRequestHeaders(h http.Header) (reqHeaders []string, ok bool) {
	values := h.Values("Access-Control-Request-Headers")
	if len(values) == 0 {
		return nil, true
	}
	reqHeaders = make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		for _, reqHeader := range strings.Split(value, ",") {
			reqHeader = strings.TrimSpace(reqHeader)
			if reqHeader == "" {
				continue
			}
			key := strings.ToLower(reqHeader)
			if _, dup := seen[key]; dup {
				continue
			}
			if len(reqHeaders) == maxCORSPreflightRequestHeaders {
				return nil, false
			}
			seen[key] = struct{}{}
			reqHeaders = append(reqHeaders, reqHeader)
		}
	}
	return reqHeaders, true
}
