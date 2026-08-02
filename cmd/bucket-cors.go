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
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/minio/minio/internal/logger"
)

const (
	bucketCORSConfig = "cors.xml"

	// Maximum size of a CORS configuration document. AWS S3 limits the document
	// to 64 KiB; MinIO's own SDK bounds its decoder at 128 KiB as a safety
	// margin, and the same ceiling is applied here so that every document the
	// SDK accepts is accepted. validateBucketCorsConfig enforces it, reading one
	// byte past the ceiling so an oversized body is refused rather than
	// validated as a truncated prefix of itself.
	maxBucketCORSConfigSize = 128 * humanize.KiByte

	maxBucketCORSRules = 100

	// Most comparisons one preflight evaluation may perform before it is refused
	// instead of completed.
	//
	// The work is the product of two lists chosen separately, by a bucket owner
	// and by an unauthenticated client: every header a preflight asks about is
	// compared against the AllowedHeader values of the rules whose origin and
	// method match. Each list is bounded on its own - by
	// maxBucketCORSConfigSize and by corsPreflightHeadersTooLarge - but nothing
	// bounds their product, so this budget does. It leaves ordinary preflights,
	// which ask about a handful of headers, orders of magnitude of headroom.
	// Exhausting it fails closed: rules that were not evaluated in full cannot
	// be known to allow the request.
	maxCORSPreflightMatchCost = 1 << 20

	// Shortest interval between two reports that preflight requests are being
	// refused for a reason other than the bucket's own rules - a request this
	// layer will not evaluate at all, or one whose rules it could not finish
	// evaluating. The reason is reported so that an operator can tell such a
	// refusal apart from a rule that simply did not match, and it is reported no
	// more often than this because a preflight is unauthenticated: reporting
	// every one of them would let whoever sends them decide how much a
	// deployment logs.
	corsPreflightRefusalReportInterval = time.Minute

	// Most preflight requests that may read a bucket's stored metadata from the
	// backend at the same time.
	//
	// Such a read happens only while the bucket metadata cache is still loading
	// and only for a bucket that cache does not yet hold, so it is bounded in
	// time by startup; this bounds it in width as well. A preflight carries no
	// credentials, so without a bound the number of concurrent backend reads
	// would be chosen by whoever sends the requests. The gate is not waited on:
	// a request that finds it full is refused rather than queued, which keeps
	// the number of goroutines parked in this layer at the gate's width.
	maxCORSPreflightConfigReads = 8

	// Longest one such read may take before the preflight is refused instead.
	//
	// The read is a single metadata document from the local erasure set, so this
	// is generous for the work involved while still bounding how long an
	// unauthenticated request may occupy one of the slots above.
	corsPreflightConfigReadTimeout = 2 * time.Second

	// corsConfigXMLNS is the XML namespace of every S3 CORS document. A client
	// either omits the namespace, in which case the parser defaults it to this
	// value, or declares exactly this one. Any other namespace is rejected: the
	// parser preserves it, so it would be persisted and handed back to clients
	// that expect the AWS wire format.
	corsConfigXMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

	// Element names of a CORS document, spelled exactly as AWS spells them.
	corsConfigurationElement = "CORSConfiguration"
	corsRuleElement          = "CORSRule"

	// utf8BOM is the UTF-8 encoding of U+FEFF, the byte order mark. XML 1.0
	// permits a single one at the very start of a UTF-8 entity, where it is an
	// encoding signature rather than content, and editors on some platforms
	// write it into every file they save.
	utf8BOM = "\ufeff"
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

// corsConfigReadError reports that the body carrying a CORS configuration could
// not be read to its end, as opposed to a document that was read and did not
// validate. The distinction decides which S3 error the client is answered with.
//
// It matters because the body PutBucketCorsHandler hands over is not a plain
// reader: corsConfigBody wraps it in one that verifies the Content-MD5,
// x-amz-content-sha256 and x-amz-checksum-* values the client declared as the
// document is consumed, so a digest that does not match what was declared
// surfaces from the read rather than from the document. Such a failure has its
// own S3 error code, which tells the client exactly what to correct, and
// reporting it as a schema violation would hide that behind a complaint about XML
// that was in fact well-formed.
type corsConfigReadError struct {
	cause error
}

func (e corsConfigReadError) Error() string {
	return fmt.Sprintf("Unable to read the %s document: %v", corsConfigurationElement, e.cause)
}

// Unwrap exposes the underlying failure so that a caller can classify it, which
// is what routes a digest or checksum mismatch to its own S3 error code.
func (e corsConfigReadError) Unwrap() error { return e.cause }

// errCORSPreflightHostNotAccepted is the reason reported when a preflight is
// refused because its Host header is not one this server accepts, and so cannot
// be attributed to a bucket the way the router would attribute it.
//
// It carries no part of the request. The Host is client chosen, unauthenticated
// and only bounded by the maximum header size, and the parse failure it produces
// quotes it back, so reporting that failure verbatim would put an arbitrary
// client string into the server's log.
var errCORSPreflightHostNotAccepted = errors.New(
	"its Host header is not one this server accepts, so it could not be attributed to a bucket")

// errCORSPreflightHeadersTooLarge is the reason reported when a preflight is
// refused because it carries more header bytes than this server admits, which is
// the ceiling setRequestLimitMiddleware applies to every other request.
//
// It names no header and no size, because both come from the request.
var errCORSPreflightHeadersTooLarge = errors.New(
	"it carries more header bytes than this server admits")

// errCORSPreflightEvaluationTooCostly is the reason reported when a preflight is
// refused because evaluating it against the bucket's rules would cost more
// comparisons than maxCORSPreflightMatchCost allows.
//
// It is the one refusal that says nothing about whether the rules allow the
// request: they were never fully consulted, which is precisely why the request
// cannot be allowed.
var errCORSPreflightEvaluationTooCostly = errors.New(
	"evaluating it against the bucket's CORS rules would cost more comparisons than this server performs for one preflight")

// errCORSPreflightConfigReadsBusy is the reason reported when a preflight is
// refused because as many of them as this server allows were already reading
// bucket metadata from the backend, so this one would have had to wait for a slot
// to establish whether its bucket has rules.
//
// Like every other refusal here it says nothing about what those rules are: they
// were never read, which is why the request cannot be allowed.
var errCORSPreflightConfigReadsBusy = errors.New(
	"as many preflight requests as this server allows were already establishing a bucket's CORS configuration from its stored metadata")

// corsPreflightConfigReads bounds how many preflight requests read bucket
// metadata from the backend at the same time. It is a buffered channel used as a
// counting gate, acquired without waiting: a request that cannot take a slot is
// refused rather than parked, so this layer never holds more than
// maxCORSPreflightConfigReads goroutines however many preflights arrive.
var corsPreflightConfigReads = make(chan struct{}, maxCORSPreflightConfigReads)

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
// Two forms of insignificant markup a hand written document carries are
// normalized away before the configuration is validated, so that what is
// validated is also what is stored and later matched: a leading byte order mark,
// and the whitespace surrounding every element value.
//
// Callers hand the request body over directly: the size ceiling is enforced
// here, by reading one byte more than maxBucketCORSConfigSize and refusing a
// body that long, so an oversized document is rejected outright rather than
// accepted as a truncated prefix of itself. The body is buffered because
// duplicate ID and MaxAgeSeconds elements cannot be detected after decoding
// into the SDK model.
//
// A body that cannot be read to its end is reported as a corsConfigReadError
// wrapping the reason, so that a caller can tell a failure of the transfer -
// including the digest and checksum verification corsConfigBody installs on the
// body it hands over - from a document that failed to validate.
func validateBucketCorsConfig(r io.Reader) (*miniogocors.Config, error) {
	// Reading one byte past the ceiling is what makes the limit provable: a
	// document of exactly maxBucketCORSConfigSize bytes is still accepted,
	// while a longer one is detected here instead of being silently truncated
	// into an XML decoding failure - or, worse, into a valid document followed
	// by discarded padding.
	data, err := io.ReadAll(io.LimitReader(r, maxBucketCORSConfigSize+1))
	if err != nil {
		return nil, corsConfigReadError{cause: err}
	}
	if len(data) > maxBucketCORSConfigSize {
		return nil, fmt.Errorf("%s document is larger than the maximum of %d bytes",
			corsConfigurationElement, maxBucketCORSConfigSize)
	}

	// A leading byte order mark is an encoding signature, not content, so it is
	// consumed here rather than validated below: the XML decoder reports it as
	// character data ahead of the root element, and character data ahead of the
	// root element is exactly what the strict walk refuses. Only the first mark
	// is consumed, because only the first one is a signature - anywhere else
	// U+FEFF is an ordinary character, and a second one really is content
	// before the root element.
	//
	// The mark is removed after the ceiling has been applied, since the bytes
	// were part of the body the client sent, and it is removed from the bytes
	// both the walk and the document model read, so the two cannot disagree
	// about where the document starts.
	data = bytes.TrimPrefix(data, []byte(utf8BOM))

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

	// The parser defaults an absent namespace to the S3 namespace but preserves
	// any other value, and the marshaller emits whatever it holds, so a document
	// declaring a different namespace has to be rejected here: it would be
	// persisted and handed back to clients that expect the AWS wire format. Both
	// forms matter and neither implies the other - the resolved element namespace
	// can be correct while a default xmlns attribute alongside a prefixed element
	// name is not.
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

	for i := range cfg.CORSRules {
		// The rule is addressed rather than copied so that normalizing it
		// reaches the configuration that is validated below, persisted by the
		// handler and later matched against preflight requests.
		rule := &cfg.CORSRules[i]
		normalizeCORSRuleValues(rule)

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
		// A rule also has to name an origin that survives being stored. The
		// vendored model tags AllowedOrigin omitempty, and encoding/xml applies
		// omitempty to every element of a slice, so an AllowedOrigin carrying no
		// value - written empty, or holding nothing but the whitespace that
		// normalization has just removed - is dropped from the canonical
		// document the handler persists and hands back. A rule whose every
		// AllowedOrigin is empty would therefore be stored, and returned by GET,
		// as a rule with no AllowedOrigin at all: a document the check above
		// refuses. That breaks reapplying what was read - "mc cors get" into "mc
		// cors set", or "aws s3api get-bucket-cors" into "put-bucket-cors" - and
		// it leaves bucket metadata holding a configuration no request could
		// have created. Such a rule can never match anything either, because no
		// browser can send the empty origin, so refusing it withholds nothing
		// and tells the bucket owner what is wrong instead of answering 200 for
		// a rule that does nothing. An empty value sitting alongside one that
		// carries a value stays accepted, as AWS accepts it: dropping it narrows
		// nothing, and what is stored is still a document this function accepts.
		namesAnOrigin := false
		for _, origin := range rule.AllowedOrigin {
			// A bare "*" and a single embedded wildcard such as
			// "http://www.example.*" are both legal, more than one is not.
			if strings.Count(origin, "*") > 1 {
				return nil, fmt.Errorf("CORSRule %d has AllowedOrigin %q with more than one wildcard", i, origin)
			}
			if corsValueHasControlCharacter(origin) {
				return nil, fmt.Errorf("CORSRule %d has AllowedOrigin %q containing a control character", i, origin)
			}
			if origin != "" {
				namesAnOrigin = true
			}
		}
		if !namesAnOrigin {
			return nil, fmt.Errorf("CORSRule %d must contain an AllowedOrigin that is not empty", i)
		}

		for _, header := range rule.AllowedHeader {
			if strings.Count(header, "*") > 1 {
				return nil, fmt.Errorf("CORSRule %d has AllowedHeader %q with more than one wildcard", i, header)
			}
			if corsValueHasControlCharacter(header) {
				return nil, fmt.Errorf("CORSRule %d has AllowedHeader %q containing a control character", i, header)
			}
		}

		for _, header := range rule.ExposeHeader {
			if corsValueHasControlCharacter(header) {
				return nil, fmt.Errorf("CORSRule %d has ExposeHeader %q containing a control character", i, header)
			}
		}

		// MaxAgeSeconds is decoded as an integer, so an element that carries no
		// value means zero - the same as omitting it, which is how a browser is
		// told not to cache the answer - while a value that is not a number, or
		// one too large for the type, has already been refused by the decoder.
		// Only a negative age remains to be refused here.
		if rule.MaxAgeSeconds < 0 {
			return nil, fmt.Errorf("CORSRule %d has negative MaxAgeSeconds %d", i, rule.MaxAgeSeconds)
		}
	}

	// The document that is stored is the canonical re-marshaling of the one that
	// was received, and it can be longer than what arrived: an omitted xmlns
	// attribute is filled in with the S3 namespace, and a value carrying a
	// character the encoder escapes grows with it. The ceiling has to hold for
	// that document as well, because it is the document that is read back, and
	// the parser reading it stops after exactly this many bytes. Without this
	// check a body that only just fits is accepted, and the write then fails
	// while decoding what it just built - reporting an internal error for what
	// is a client's oversized input.
	canonical, err := xml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("Unable to encode the %s document: %w", corsConfigurationElement, err)
	}
	if len(canonical) > maxBucketCORSConfigSize {
		return nil, fmt.Errorf("%s document is larger than the maximum of %d bytes once stored in its canonical form",
			corsConfigurationElement, maxBucketCORSConfigSize)
	}

	return cfg, nil
}

// normalizeCORSRuleValues trims the whitespace surrounding every text value of
// a rule, in place.
//
// XML carries the indentation of a hand written document into the text of its
// elements, so an AllowedOrigin written on a line of its own arrives as a value
// no browser can ever send. Without this, such a rule is either stored and
// silently never matched - the client is told its configuration was accepted -
// or, for AllowedMethod, refused for being unsupported when the method it names
// is in fact supported. Neither outcome tells the bucket owner what is wrong,
// and both are avoided by validating and storing the value the document means.
//
// Only the surrounding whitespace is removed. The value itself is never
// otherwise rewritten, so a rule can never be widened into permitting an origin
// or a header the document does not name.
func normalizeCORSRuleValues(rule *miniogocors.Rule) {
	rule.ID = strings.TrimSpace(rule.ID)
	// The slices are trimmed through their backing arrays, which is what makes
	// the normalization visible to the caller's configuration.
	for _, values := range [][]string{
		rule.AllowedHeader,
		rule.AllowedMethod,
		rule.AllowedOrigin,
		rule.ExposeHeader,
	} {
		for i, value := range values {
			values[i] = strings.TrimSpace(value)
		}
	}
}

// corsValueHasControlCharacter reports whether a value contains a control
// character, which no origin and no header name may.
//
// The XML decoder already refuses every control character the XML specification
// forbids, so what reaches here is a tab, a carriage return or a line feed that
// survived normalization by sitting inside a value rather than around it. Such a
// value is meaningless as an origin, and as a header name it would be written
// into an Access-Control-Expose-Headers or Access-Control-Allow-Headers
// response header, where the HTTP writer replaces the character with a space -
// so the bucket owner would receive neither the header they configured nor any
// indication that it had been altered. Refusing the document keeps what is
// stored and what is emitted identical, and keeps line breaks out of a response
// header.
//
// ID is deliberately not subjected to this check: it is never matched and never
// emitted as a header, only carried through the stored document and handed back
// verbatim, so refusing it would add strictness that AWS does not have without
// protecting anything.
func corsValueHasControlCharacter(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
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
			if err := validateCorsElementAttributes(t, root.Name.Space); err != nil {
				return err
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
			// The root element's namespace is what every element below is
			// required to carry, so its own attributes are checked against that
			// namespace rather than against an enclosing one.
			if err := validateCorsElementAttributes(t, t.Name.Space); err != nil {
				return xml.StartElement{}, err
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
			if err := validateCorsElementAttributes(t, space); err != nil {
				return err
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
//
// An attribute that appears once on the element is deliberately ignored rather
// than rejected: unlike a nested element, it cannot be mistaken for a configured
// value, so ignoring it cannot mislead the client about what was stored. One
// that appears twice is a different matter and is refused before this point, by
// validateCorsElementAttributes.
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

// validateCorsElementAttributes enforces that no element of the document carries
// the same attribute twice. space is the namespace of the document, and is used
// only to render the element's name in a failure.
//
// This is a well-formedness constraint rather than a MinIO restriction: XML 1.0
// forbids an element from repeating an attribute name, and the Namespaces
// specification extends that to two attributes whose expanded names are equal,
// however they were spelled. Go's decoder enforces neither, so a document that
// every conforming XML parser refuses would otherwise be accepted here, stored,
// and handed back to clients as though it had been valid all along - replacing a
// configuration that was.
//
// Comparing the resolved xml.Name is what makes one check cover every spelling.
// The decoder reports a default xmlns declaration as the unprefixed name xmlns, a
// prefix declaration as that prefix within the reserved xmlns space, and a
// prefixed attribute with its prefix already replaced by the namespace it is
// bound to. Two attributes therefore compare equal exactly when their expanded
// names are equal: repeated identical declarations, a repeated ordinary
// attribute, and one name reached through two prefixes bound to the same
// namespace are all one comparison, while a document that spells each of its
// attributes once compares unequal throughout.
func validateCorsElementAttributes(elem xml.StartElement, space string) error {
	// An element carrying at most one attribute cannot repeat one, and elements
	// with no attributes at all are the overwhelming majority, so nothing is
	// allocated for them.
	if len(elem.Attr) < 2 {
		return nil
	}

	seen := make(map[xml.Name]struct{}, len(elem.Attr))
	for _, attr := range elem.Attr {
		if _, dup := seen[attr.Name]; dup {
			return fmt.Errorf("The element %q declares the attribute %q more than once",
				corsElementName(elem.Name, space), corsAttributeName(attr.Name))
		}
		seen[attr.Name] = struct{}{}
	}
	return nil
}

// corsAttributeName renders an attribute name for a client visible error, in the
// form the document spelled it rather than in the resolved form the decoder
// reports: a namespace declaration reads as xmlns or xmlns:prefix, an ordinary
// attribute by its bare name, and one belonging to a namespace in the
// conventional {namespace}local form, since the prefix it was written with is not
// preserved.
func corsAttributeName(name xml.Name) string {
	switch name.Space {
	case "":
		return name.Local
	case "xmlns":
		return "xmlns:" + name.Local
	}
	return "{" + name.Space + "}" + name.Local
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

// corsMatchBudget bounds the comparisons one preflight evaluation performs, so
// that the cost of answering a request a client chose freely is a cost this
// server chose.
//
// It is spent across the whole evaluation rather than reset per rule, because
// what has to be bounded is the work one request causes, and it is carried by
// pointer so every rule the evaluation walks draws on the same allowance.
type corsMatchBudget struct {
	// remaining is how many comparisons are still allowed.
	remaining int
	// exhausted records that the allowance ran out. It is sticky: once the
	// evaluation is incomplete, no later comparison can complete it, so the
	// outcome is a refusal however the remaining predicates would have answered.
	exhausted bool
}

// spend claims n comparisons, reporting whether the allowance covered them. A
// caller that is refused must stop comparing and let the evaluation end: its
// answer is no longer the one the rules give.
func (b *corsMatchBudget) spend(n int) bool {
	if b.exhausted {
		return false
	}
	if b.remaining < n {
		b.exhausted = true
		return false
	}
	b.remaining -= n
	return true
}

// corsRuleFor returns the first rule of cfg, in document order, that allows the
// supplied origin, the requested method and every requested header. It returns a
// nil rule and a nil error when the rules were fully evaluated and none of them
// allows the request.
//
// S3 semantics are first matching rule wins: rules are never merged and never
// reordered, so the returned rule alone determines the preflight response. Every
// rule is examined until one matches, and every requested header is compared
// against a rule before that rule is accepted, because a rule that covers all but
// one requested header does not allow the request.
//
// The evaluation is bounded by maxCORSPreflightMatchCost, since its cost is the
// product of a list the bucket owner chose and a list the client chose. Exceeding
// the bound returns errCORSPreflightEvaluationTooCostly rather than "no rule
// matched": the two lead to the same answer on the wire, but only the second is
// something the bucket's rules actually say, and the first is worth reporting.
func corsRuleFor(cfg *miniogocors.Config, origin, method string, reqHeaders []string) (*miniogocors.Rule, error) {
	if cfg == nil {
		return nil, nil
	}
	budget := &corsMatchBudget{remaining: maxCORSPreflightMatchCost}
	for i := range cfg.CORSRules {
		rule := &cfg.CORSRules[i]
		matched := corsRuleAllowsOrigin(rule, origin, budget) &&
			corsRuleAllowsMethod(rule, method, budget) &&
			corsRuleAllowsHeaders(rule, reqHeaders, budget)
		if budget.exhausted {
			return nil, errCORSPreflightEvaluationTooCostly
		}
		if matched {
			return rule, nil
		}
	}
	return nil, nil
}

// corsWildcardMatch reports whether name satisfies pattern under the pattern
// language S3 defines for AllowedOrigin and AllowedHeader: every character is
// literal except a single "*", which stands for any sequence of characters. A
// pattern without a wildcard therefore matches only itself.
//
// It deliberately does not reuse the general wildcard matcher the server-wide
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
//
// Each configured value examined costs one comparison from budget, so a document
// that lists a great many of them cannot be walked for free.
func corsRuleAllowsOrigin(rule *miniogocors.Rule, origin string, budget *corsMatchBudget) bool {
	for _, allowedOrigin := range rule.AllowedOrigin {
		if !budget.spend(1) {
			return false
		}
		if corsWildcardMatch(allowedOrigin, origin) {
			return true
		}
	}
	return false
}

// corsRuleAllowsMethod reports whether the rule contains the requested method.
// Case-insensitive comparison also supports configurations constructed without
// parser normalization. Each value examined costs one comparison from budget.
func corsRuleAllowsMethod(rule *miniogocors.Rule, method string, budget *corsMatchBudget) bool {
	for _, allowedMethod := range rule.AllowedMethod {
		if !budget.spend(1) {
			return false
		}
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
//
// Every requested header is evaluated, however many there are, because a rule
// that covers all but one of them does not allow the request. The rule's
// AllowedHeader values are indexed once here rather than rescanned for each
// requested header, so a header the rule names literally costs a single map
// lookup no matter how many values the rule lists.
//
// What that index cannot flatten is a rule listing wildcard patterns: those have
// to be compared one by one, so the cost of the pairing is the product of the two
// lists. That product is what budget bounds, and running out of it refuses the
// request rather than reporting the rule as not matching, because a header list
// that was not compared in full says nothing about whether the rule covers it.
func corsRuleAllowsHeaders(rule *miniogocors.Rule, reqHeaders []string, budget *corsMatchBudget) bool {
	if len(reqHeaders) == 0 {
		return true
	}
	allowed, ok := newCORSHeaderMatcher(rule.AllowedHeader, budget)
	if !ok {
		return false
	}
	for _, reqHeader := range reqHeaders {
		if !allowed.allows(reqHeader, budget) {
			return false
		}
	}
	return true
}

// corsHeaderMatcher answers whether a requested header name is covered by the
// AllowedHeader values of one rule. Header names are compared
// case-insensitively, a bare "*" covers every header and a single embedded
// wildcard such as "x-amz-*" covers every header with that prefix.
type corsHeaderMatcher struct {
	// all is set when an AllowedHeader is the bare wildcard, which covers every
	// header and makes the two lists below irrelevant.
	all bool
	// exact holds the lower-cased AllowedHeader values that carry no wildcard,
	// so the common case is answered by a single map lookup.
	exact map[string]struct{}
	// patterns holds the lower-cased AllowedHeader values that carry a
	// wildcard. They cannot be looked up and are compared one by one, which is
	// what maxCORSPreflightMatchCost bounds: a rule may list as many of them as
	// a stored document holds.
	patterns []string
}

// newCORSHeaderMatcher indexes the AllowedHeader values of a rule for repeated
// lookups. The values are lower-cased once, here, so that the case-insensitive
// comparison does not have to re-fold the same configured value for every
// requested header.
//
// Indexing one value costs one comparison from budget, since a rule may list as
// many of them as a stored document holds. It reports false when the allowance
// ran out, in which case the matcher it returns must not be used: it indexes only
// a prefix of the rule's values and would answer for the rest as though the rule
// did not list them.
func newCORSHeaderMatcher(allowedHeaders []string, budget *corsMatchBudget) (corsHeaderMatcher, bool) {
	matcher := corsHeaderMatcher{}
	for _, allowedHeader := range allowedHeaders {
		if !budget.spend(1) {
			return corsHeaderMatcher{}, false
		}
		if allowedHeader == "*" {
			return corsHeaderMatcher{all: true}, true
		}
		lowerAllowedHeader := strings.ToLower(allowedHeader)
		if strings.IndexByte(lowerAllowedHeader, '*') >= 0 {
			matcher.patterns = append(matcher.patterns, lowerAllowedHeader)
			continue
		}
		if matcher.exact == nil {
			matcher.exact = make(map[string]struct{}, len(allowedHeaders))
		}
		matcher.exact[lowerAllowedHeader] = struct{}{}
	}
	return matcher, true
}

// allows reports whether a single requested header is covered.
//
// The map lookup costs one comparison from budget and each wildcard pattern
// compared costs one more, so the allowance is drawn down in proportion to the
// work actually performed. Running out reports the header as not covered, which
// the caller turns into a refusal rather than a verdict of the rules.
func (m corsHeaderMatcher) allows(reqHeader string, budget *corsMatchBudget) bool {
	if m.all {
		return true
	}
	if !budget.spend(1) {
		return false
	}
	// Lower-casing the requested name makes the comparison case-insensitive for
	// the literal parts of a pattern as well as for a plain header name, so no
	// separate equality check is needed.
	lowerReqHeader := strings.ToLower(reqHeader)
	if _, ok := m.exact[lowerReqHeader]; ok {
		return true
	}
	for _, pattern := range m.patterns {
		if !budget.spend(1) {
			return false
		}
		if corsWildcardMatch(pattern, lowerReqHeader) {
			return true
		}
	}
	return false
}

// corsPreflightHeadersTooLarge reports whether a request carries more header
// bytes than this server admits, applying the two ceilings
// setRequestLimitMiddleware applies to every other request: maxHeaderSize over
// all of the headers, and maxUserDataSize over the user metadata among them.
//
// It counts them here rather than calling isHTTPHeaderSizeTooLarge, the helper
// that middleware uses, because that helper measures each field name once
// against Header.Get - the first value of the field, and only that one - while
// this layer reads every value of Access-Control-Request-Headers, splits them
// into one list of requested headers, and echoes that whole list back on a
// matched rule. A request that repeats a field would therefore be measured by a
// fraction of what it carries and of what answering it costs, and a preflight is
// unauthenticated, so that fraction would be a client's to choose. Every value
// of every field is summed instead, with the field name counted once per value,
// the way the wire carries it.
//
// Counting at least as much as the inner limit does keeps the two layers
// consistent in the only direction that is safe: a preflight this refuses may or
// may not have been admitted further in, while a preflight this admits is one
// that layer admits as well.
func corsPreflightHeadersTooLarge(header http.Header) bool {
	var size, userSize int
	for key, values := range header {
		for _, value := range values {
			length := len(key) + len(value)
			size += length
			for _, prefix := range userMetadataKeyPrefixes {
				if stringsHasPrefixFold(key, prefix) {
					userSize += length
					break
				}
			}
			if userSize > maxUserDataSize || size > maxHeaderSize {
				return true
			}
		}
	}
	return false
}

// bucketCORSPreflightMiddleware answers browser CORS preflight requests from
// the CORS configuration stored on the target bucket.
//
// It wraps the server-wide rs/cors handler built by corsHandler and is therefore
// the outermost HTTP layer. That placement is required: no mux route registers
// OPTIONS, so a preflight never reaches an S3 handler and a mux middleware would
// be bypassed for it. It also means this runs before authentication and before
// the mux middlewares, on a request whose path and headers are entirely client
// controlled, so it repeats their admission checks itself and reads only the
// in-memory bucket metadata.
//
// A request is disposed of in one of three ways:
//
//   - Delegated, which keeps the server-wide MINIO_API_CORS_ALLOW_ORIGIN setting
//     in force for it: the request is not a preflight, addresses the server root
//     or no syntactically valid bucket, or names a bucket that demonstrably has
//     no CORS configuration of its own - none is stored, or the bucket does not
//     exist - or one whose stored configuration carries no rule. Delegating on
//     every one of those is what leaves a bucket without a configuration of its
//     own behaving exactly as it did before this layer existed.
//   - Allowed, on the first rule that matches: HTTP 200 with the CORS response
//     headers that rule configures.
//   - Denied, HTTP 200 carrying no Access-Control-Allow-* header, which is how a
//     browser learns the request is refused: the bucket has rules and none of
//     them allows this request, the request is one the server itself would refuse
//     outright, so the request this preflight asks about could never be served
//     whatever any rule says, or the bucket's configuration could not be
//     established at all - in which case rules that were never read cannot be
//     known to allow the request, and answering it from the server-wide setting
//     would let a configured bucket's own rules be bypassed.
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

		// setRequestLimitMiddleware refuses a request carrying more header bytes
		// than this ceiling before any bucket handler runs, so the request this
		// preflight asks about could never be served and the preflight is refused
		// here rather than answered from the server-wide default. Applying it
		// before anything else is read from the headers is also what bounds every
		// step below, since the lists this layer parses, compares and echoes back
		// are the ones those bytes carry.
		if corsPreflightHeadersTooLarge(r.Header) {
			corsPreflightRefused.report(errCORSPreflightHeadersTooLarge)
			writeCORSPreflightDenied(w)
			return
		}

		// setRequestValidityMiddleware answers HTTP 400 for a Host this server
		// does not accept, so the request this preflight asks about could never be
		// served either - while the router does still route it to a real bucket
		// whose own rules may be restrictive, which is why the preflight is
		// refused here rather than answered from the server-wide default.
		// hasBadHost is the very check that middleware applies, including its
		// allowance for an empty Host under CI/CD, so the two layers agree by
		// construction.
		if hasBadHost(r.Host) != nil {
			corsPreflightRefused.report(errCORSPreflightHostNotAccepted)
			writeCORSPreflightDenied(w)
			return
		}

		// Resolve the target bucket the same way the rest of the server does,
		// so both path-style and virtual-host-style addressing are handled:
		// getResource turns a virtual-host-style Host into a path-style
		// resource, and path2BucketObject splits the bucket off it.
		//
		// These are the two halves of request2BucketObjectName, spelled out
		// rather than called through it, because that wrapper reports a Host it
		// cannot parse through logger.CriticalIf, which panics - and the Host
		// here is client controlled and unauthenticated. The wrapper does
		// nothing else, so for every Host it does not panic on the two produce
		// the same bucket it would have.
		resource, err := getResource(r.URL.Path, r.Host, globalDomainNames)
		if err != nil {
			// The only Host getResource rejects is one hasBadHost rejects too,
			// so reaching this is the empty Host that check allows under CI/CD
			// while virtual-host-style addressing is configured. An empty Host
			// names no virtual host, so the router resolves such a request
			// path-style off the request path, and so does this: the bucket
			// evaluated here stays the bucket the request would reach, which is
			// the whole point of resolving it the same way.
			resource = r.URL.Path
		}
		bucket, _ := path2BucketObject(resource)
		if bucket == "" {
			// A preflight for the server root names no bucket, so there are no
			// per-bucket rules to apply and the server-wide setting answers it.
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

		cfg, err := preflightCORSConfig(r.Context(), bucket)
		switch {
		case err != nil && !corsPreflightConfigAbsent(err):
			// Whether this bucket has rules of its own could not be
			// established, so the request is refused rather than answered from
			// the server-wide setting: a bucket whose rules were never read
			// cannot be known to allow it, and answering it globally is exactly
			// how a configured bucket's own rules would be bypassed. Like the
			// refusals above this says something about how this request was
			// handled rather than what the bucket's rules say, so it is
			// reported at the same bounded rate, naming only the validated
			// bucket.
			corsPreflightRefused.report(fmt.Errorf(
				"for bucket %s, its CORS configuration could not be established: %w", bucket, err))
			writeCORSPreflightDenied(w)
			return
		case err != nil, cfg == nil, len(cfg.CORSRules) == 0:
			// The bucket demonstrably has no rules of its own to answer this
			// preflight from: it carries no configuration, it does not exist, or
			// what is stored carries no rule. Every one of those hands the
			// request to the server-wide handler, which is what keeps the
			// MINIO_API_CORS_ALLOW_ORIGIN setting in force for exactly the
			// requests it governed before this layer existed.
			next.ServeHTTP(w, r)
			return
		}

		reqHeaders := parseCORSRequestHeaders(r.Header)

		rule, err := corsRuleFor(cfg, origin, reqMethod, reqHeaders)
		switch {
		case err != nil:
			// The rules could not be evaluated in full, so whether they allow
			// the request is unknown and the request is refused. Like the
			// refusals above this says something about how this request was
			// handled rather than what the bucket's rules say, so it is
			// reported at the same bounded rate, naming only the validated
			// bucket.
			corsPreflightRefused.report(fmt.Errorf("for bucket %s, %w", bucket, err))
			writeCORSPreflightDenied(w)
			return
		case rule == nil:
			// The bucket has rules, they were evaluated in full, and none of
			// them allows this request. That is the configuration working as
			// written, so it is not reported.
			writeCORSPreflightDenied(w)
			return
		}

		header := w.Header()
		setCORSPreflightVary(header)

		// The matched rule fully determines the response. The origin is echoed
		// back rather than answered with "*" because the server-wide handler
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
			// The server-wide handler never exposes headers on a preflight
			// response, so a matched rule's ExposeHeader list can only be
			// honored here.
			header.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeader, ", "))
		}
		w.WriteHeader(http.StatusOK)
	})
}

// preflightCORSConfig returns the CORS configuration a bucket's own rules are to
// be read from when answering a preflight request, or an error saying either that
// the bucket demonstrably has none or that its configuration could not be
// established. corsPreflightConfigAbsent tells those two apart, and the caller
// disposes of the request accordingly: a bucket with no configuration is answered
// by the server-wide handler, while one whose configuration is unknown is
// refused.
//
// The lookup is in-memory whenever the answer is authoritative there, through
// BucketMetadataSys.Get, which reads the bucket metadata cache under its own lock
// and reports errConfigNotFound for a bucket it does not hold. Once that cache
// has finished loading it holds every bucket that exists, so a miss is a
// confirmed absence and no further work is done - which is the whole of the
// steady state.
//
// While the cache is still loading a miss says nothing at all: the bucket may
// have rules that simply have not been read yet. Answering such a request from
// the server-wide setting would let a bucket's own rules be bypassed for as long
// as loading takes, so the configuration is instead established from the backend,
// for that one bucket, by corsBackendCORSConfig.
//
// Note the loading accessor GetCORSConfig is deliberately not used for this. It
// is not request scoped, it migrates legacy configuration, and it caches whatever
// it loads - including the default metadata it invents for a bucket that does not
// exist - so an unauthenticated preflight naming made-up buckets could grow the
// metadata cache without bound and leave work running after the request that
// started it was gone.
func preflightCORSConfig(ctx context.Context, bucket string) (*miniogocors.Config, error) {
	sys := globalBucketMetadataSys
	if sys == nil {
		return nil, errServerNotInitialized
	}

	meta, err := sys.Get(bucket)
	if err == nil {
		if meta.corsConfig == nil {
			return nil, BucketCORSConfigNotFound{Bucket: bucket}
		}
		return meta.corsConfig, nil
	}
	if sys.Initialized() {
		// The cache holds every bucket that exists, so this bucket has no
		// configuration of its own - it has none stored, or it does not exist.
		return nil, err
	}

	return corsBackendCORSConfig(ctx, bucket)
}

// corsBackendCORSConfig establishes one bucket's CORS configuration from its
// stored metadata document, for a preflight request that arrived while the bucket
// metadata cache was still loading.
//
// The read is deliberately the narrowest one that answers the question:
// readBucketMetadata reads the single metadata document of this one bucket and
// decodes it. Nothing is written to the cache, no legacy configuration is
// migrated, and no other bucket is touched, so a preflight naming buckets that do
// not exist leaves no trace and creates no work beyond its own bounded read.
//
// The work is bounded three ways, because a preflight is unauthenticated and its
// rate is therefore chosen by whoever sends it: the read runs on the request's
// own context, so a client that goes away takes its read with it; it is given
// corsPreflightConfigReadTimeout to complete; and at most
// maxCORSPreflightConfigReads of them run at once, with a request that finds the
// gate full refused rather than queued behind it.
//
// A configuration is parsed here rather than taken from the metadata's own parsed
// field because readBucketMetadata does not parse - parseAllConfigs is part of the
// loading path this deliberately avoids. The same parser it would have used is
// used here, so both paths answer a bucket's rules from the same document.
func corsBackendCORSConfig(ctx context.Context, bucket string) (*miniogocors.Config, error) {
	if isMinioMetaBucketName(bucket) {
		// Not a bucket a client configures, and not one to read metadata for.
		return nil, BucketCORSConfigNotFound{Bucket: bucket}
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		// There is nothing to read the configuration from yet, so whether this
		// bucket has rules is unknown and the preflight is refused rather than
		// answered from the server-wide setting. The request it asks about could
		// not be served either: every S3 handler answers ErrServerNotInitialized
		// while the object layer is absent.
		return nil, errServerNotInitialized
	}

	select {
	case corsPreflightConfigReads <- struct{}{}:
		defer func() { <-corsPreflightConfigReads }()
	default:
		return nil, errCORSPreflightConfigReadsBusy
	}

	ctx, cancel := context.WithTimeout(ctx, corsPreflightConfigReadTimeout)
	defer cancel()

	meta, err := readBucketMetadata(ctx, objAPI, bucket)
	if err != nil {
		// errConfigNotFound is the bucket having no metadata document at all,
		// which is a confirmed absence: it has no configuration, or it does not
		// exist. Anything else leaves the configuration unknown.
		return nil, err
	}
	if len(meta.CORSConfigXML) == 0 {
		return nil, BucketCORSConfigNotFound{Bucket: bucket}
	}

	cfg, err := miniogocors.ParseBucketCorsConfig(bytes.NewReader(meta.CORSConfigXML))
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// corsPreflightConfigAbsent reports whether err says the bucket has no CORS
// configuration of its own, as opposed to saying its configuration could not be
// established.
//
// Only these two errors mean absence. BucketCORSConfigNotFound is the sentinel
// the metadata layer raises for a bucket whose metadata carries no CORS document,
// and errConfigNotFound is a bucket the loaded cache does not hold or one with no
// metadata document at all - a bucket that has no configuration, or that does not
// exist. Every other error - the object layer or the metadata subsystem being
// absent, a read that failed or timed out, a document that would not parse, the
// read gate being full - says the answer is unknown, and an unknown answer is
// never treated as an absent configuration.
func corsPreflightConfigAbsent(err error) bool {
	if err == nil {
		return false
	}
	var notFound BucketCORSConfigNotFound
	return errors.As(err, &notFound) || errors.Is(err, errConfigNotFound)
}

// corsPreflightRefusalReporter reports that preflight requests are being refused
// for a reason other than the bucket's own rules, at a rate that does not depend
// on how many such requests arrive.
//
// The refusal is deliberately indistinguishable on the wire from the one a rule
// that did not match produces - both are a preflight response carrying no
// Access-Control-Allow-* header - and a preflight is answered ahead of the
// router, so none of the per-request audit, trace or metric plumbing covers it.
// Without a report the two are indistinguishable to an operator as well, and the
// ones that say a request was turned away before its rules were consulted, or
// that its rules could not be evaluated to the end, are the ones worth acting
// on.
//
// Every such reason shares this one reporter, and therefore one rate limit,
// because they are all reachable by an unauthenticated request and a client that
// could pick between them could otherwise multiply the volume a deployment logs
// by choosing several.
//
// The report is rate limited rather than emitted per request because a preflight
// carries no credentials, so its rate is chosen by whoever sends it. Both the
// state kept here and the volume emitted are therefore fixed, whatever arrives:
// one counter, and at most one report per corsPreflightRefusalReportInterval. The
// number of refusals that went unreported travels with the next report, so the
// condition never reads as rarer than it is.
type corsPreflightRefusalReporter struct {
	mu sync.Mutex
	// reportedAt is when the last report was emitted. The zero value means none
	// has been, which is what makes the first refusal report immediately.
	reportedAt time.Time
	// unreported counts the refusals observed since that report.
	unreported uint64
}

// corsPreflightRefused reports the condition for the running server. It is held
// as a pointer so that a test can install a reporter of its own without copying
// a mutex.
var corsPreflightRefused = &corsPreflightRefusalReporter{}

// report records that a preflight request was refused because of reason, and
// emits a diagnostic when one is due.
//
// Only server authored text may reach the log, so a caller composes reason from
// errors this package and the bucket metadata layer produce, and interpolates
// nothing the request carried except a bucket name it has already validated as
// one - which is therefore bounded in both length and alphabet. Nothing else is
// ever logged - not the origin, not the headers the request asks about, not the
// Host it was addressed to - because the request is unauthenticated and every one
// of those values is chosen by the client.
func (r *corsPreflightRefusalReporter) report(reason error) {
	unreported, due := r.due(time.Now())
	if !due {
		return
	}

	// An event, and not an audit entry: the condition describes the state of this
	// server rather than the outcome of an authenticated operation, and the
	// preflight it refused reaches no audit path to begin with.
	//
	// An event and not an error either, because a refusal is a condition this
	// layer expects and handles rather than a fault to be traced back to a line of
	// code. logger.Event is the path that says so: it builds the entry with no
	// stack, so the report carries the reason and nothing about the internals that
	// produced it - no frames from this middleware, from the logger, or from
	// net/http, all of which describe the same three call sites on every refusal
	// and so tell an operator nothing a refusal has not already said. The reason is
	// passed as an argument rather than as the format string so that nothing in it
	// is read as a formatting verb.
	logger.Event(GlobalContext, "cors", "%s", corsPreflightRefusalReport(reason, unreported))
}

// corsPreflightRefusalReport composes the diagnostic a report emits, carrying the
// reason and however many refusals went unreported before it, so that the
// condition never reads as rarer than it is.
func corsPreflightRefusalReport(reason error, unreported uint64) error {
	err := fmt.Errorf("Refused a CORS preflight request: %w", reason)
	if unreported > 0 {
		err = fmt.Errorf("%w (%d further refusal(s) went unreported since the previous report)", err, unreported)
	}
	return err
}

// due reports whether a refusal observed at now is to be reported, together with
// how many refusals went unreported since the previous report. A refusal that is
// not reported is counted instead, so nothing about it is lost but its timing.
func (r *corsPreflightRefusalReporter) due(now time.Time) (unreported uint64, report bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.reportedAt.IsZero() && now.Sub(r.reportedAt) < corsPreflightRefusalReportInterval {
		r.unreported++
		return 0, false
	}
	unreported, r.unreported = r.unreported, 0
	r.reportedAt = now
	return unreported, true
}

// setCORSPreflightVary declares which request headers the preflight response
// depends on, so an intermediary cache keys on all three of them rather than
// serving one origin's answer to another. It matches the variance the
// server-wide rs/cors handler declares on its own preflight responses.
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
// every Access-Control-Request-Headers value carried by the request. The Fetch
// standard guarantees at most one such header, but some gateways split it into
// repeated fields, so all of them are considered.
//
// Header names are case-insensitive, so a name repeated in any casing is one
// requested header: it is kept once, in the casing and at the position it first
// appeared, which is both what the matcher has to satisfy and what the response
// echoes back. Collapsing duplicates also stops a repetitive request from
// multiplying the work the matcher performs.
//
// The complete list is always returned, however long it is, because a rule only
// matches when it covers every header the request asks about and a header that
// was never compared against the rules cannot be known to be covered. Nothing
// about the length is refused here, and nothing needs to be: the caller admits
// only requests that carry no more header bytes than
// corsPreflightHeadersTooLarge allows, which counts every value of every
// repeated field and therefore caps this list at a few thousand names however
// they are spread across those fields, and the cost of comparing them against
// the rules is bounded separately by maxCORSPreflightMatchCost. Both of those
// bounds are the caller's to apply, so passing an unbounded header here is a
// programming error rather than something to be silently truncated - truncating
// would drop names that a rule then would not have to cover.
func parseCORSRequestHeaders(h http.Header) []string {
	values := h.Values("Access-Control-Request-Headers")
	if len(values) == 0 {
		return nil
	}
	reqHeaders := make([]string, 0, len(values))
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
			seen[key] = struct{}{}
			reqHeaders = append(reqHeaders, reqHeader)
		}
	}
	return reqHeaders
}
