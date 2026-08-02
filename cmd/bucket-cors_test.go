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
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/minio/madmin-go/v3/logger/log"
	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio/internal/auth"
	"github.com/minio/minio/internal/hash"
	xhttp "github.com/minio/minio/internal/http"
	"github.com/minio/minio/internal/logger"
	types "github.com/minio/minio/internal/logger/target/loggertypes"
	"github.com/minio/mux"
	"github.com/minio/pkg/v3/policy"
	"github.com/tinylib/msgp/msgp"
)

// s3CORSNamespace is the namespace minio-go defaults and emits for S3 CORS
// configurations.
const s3CORSNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

const corsMinimalRuleBody = `<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`

// corsCanonicalDocument is the canonical, AWS shaped CORS configuration. It is a
// verbatim copy of the round-trip fixture shipped by the vendored SDK, whose own
// test asserts the document is byte identical after a parse-and-remarshal cycle,
// so it doubles as the reference wire format for this server. It deliberately
// exercises a suffix wildcard origin, a wildcard AllowedHeader, a bare wildcard
// origin, an ExposeHeader and a MaxAgeSeconds across four rules.
const corsCanonicalDocument = `<?xml version="1.0" encoding="UTF-8"?>
<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule><AllowedHeader>*</AllowedHeader><AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod><AllowedMethod>DELETE</AllowedMethod><AllowedOrigin>http://www.example1.com</AllowedOrigin></CORSRule><CORSRule><AllowedHeader>*</AllowedHeader><AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod><AllowedMethod>DELETE</AllowedMethod><AllowedOrigin>http://www.example2.*</AllowedOrigin></CORSRule><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><ExposeHeader>x-amz-id-2</ExposeHeader><MaxAgeSeconds>6000</MaxAgeSeconds></CORSRule><CORSRule><AllowedMethod>POST</AllowedMethod><AllowedOrigin>https://www.example3.com</AllowedOrigin></CORSRule></CORSConfiguration>`

// allSupportedCORSMethods lists, in a deterministic order, every method that
// supportedCORSMethods accepts. Ranging over that map directly would make the
// generated subtest order unstable, so the slice is the source of iteration and
// TestCorsSupportedMethods guards the two against drifting apart.
var allSupportedCORSMethods = []string{
	http.MethodGet,
	http.MethodPut,
	http.MethodPost,
	http.MethodDelete,
	http.MethodHead,
}

func corsTestDoc(rules ...string) string {
	return `<CORSConfiguration xmlns="` + s3CORSNamespace + `">` + strings.Join(rules, "") + `</CORSConfiguration>`
}

func corsTestRule(children ...string) string {
	return `<CORSRule>` + strings.Join(children, "") + `</CORSRule>`
}

func corsTestConfig(rules ...miniogocors.Rule) *miniogocors.Config {
	return miniogocors.NewConfig(rules)
}

// TestValidateBucketCorsConfig covers accepted and rejected XML shapes, the
// strict schema walk, the size boundary and duplicate scalar elements. Inputs
// are handed over unbounded, exactly as the PUT handler hands over the request
// body, because the validator owns the size ceiling.
//
// Failures are asserted with a distinctive substring: the message reaches the
// client as the description of a MalformedXML error.
func TestValidateBucketCorsConfig(t *testing.T) {
	// corsPaddedDoc builds an otherwise valid single-rule document whose total
	// length is driven purely by the length of its ID element. The two size
	// cases below therefore differ in nothing but the document length, which is
	// what makes the size the provable cause of the rejection.
	corsPaddedDoc := func(padding int) string {
		return corsTestDoc(corsTestRule(`<ID>` + strings.Repeat("p", padding) + `</ID>` + corsMinimalRuleBody))
	}
	sizeOverhead := len(corsPaddedDoc(0))

	type validateCase struct {
		name string
		xml  string
		// wantErrSubstring is a distinctive fragment of the expected failure.
		// An empty value means the document must be accepted.
		wantErrSubstring string
		wantRules        int
	}

	testCases := []validateCase{
		{
			name:             "V1a/notWellFormedXML",
			xml:              `<CORSConfiguration><CORSRule>`,
			wantErrSubstring: "XML syntax error",
		},
		{
			name:             "V1a/emptyBody",
			xml:              ``,
			wantErrSubstring: "does not contain a CORSConfiguration element",
		},
		{
			name:             "V1a/whitespaceOnlyBody",
			xml:              "  \n\t ",
			wantErrSubstring: "does not contain a CORSConfiguration element",
		},
		{
			name:             "V1a/unclosedRuleElement",
			xml:              `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod></CORSConfiguration>`,
			wantErrSubstring: "XML syntax error",
		},
		{
			name:             "V1b/rootElementNotCORSConfiguration",
			xml:              `<NotCORSConfiguration/>`,
			wantErrSubstring: `Unexpected root element "NotCORSConfiguration"`,
		},
		{
			name:             "V1b/rootElementNotCORSConfigurationButCarriesRules",
			xml:              `<AccessControlPolicy>` + corsTestRule(corsMinimalRuleBody) + `</AccessControlPolicy>`,
			wantErrSubstring: `Unexpected root element "AccessControlPolicy"`,
		},

		// Document framing: the body must be exactly one CORSConfiguration
		// document. A single decode stops as soon as it has filled the root
		// element and never asks for EOF, so anything trailing the closing tag
		// would otherwise be accepted in silence.
		{
			name:             "V1c/trailingTextAfterTheDocument",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + `trailing`,
			wantErrSubstring: "character data after the CORSConfiguration element",
		},
		{
			name:             "V1c/trailingElementAfterTheDocument",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + `<Other/>`,
			wantErrSubstring: `contains the element "Other" after the CORSConfiguration element`,
		},
		{
			name:             "V1c/secondRootElement",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantErrSubstring: `contains the element "CORSConfiguration" after the CORSConfiguration element`,
		},
		{
			name:             "V1c/trailingMalformedBytesAfterTheDocument",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + `<<<`,
			wantErrSubstring: "XML syntax error",
		},
		{
			name:             "V1c/textBetweenRules",
			xml:              `<CORSConfiguration>trailing` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: "CORSConfiguration contains character data outside of a CORSRule element",
		},
		{
			name:             "V1c/documentTypeDeclaration",
			xml:              `<!DOCTYPE CORSConfiguration>` + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantErrSubstring: "Unexpected document type declaration",
		},
		{
			// A declaration is refused wherever it appears, not only in the
			// prolog, because the reason to refuse it - keeping entity handling
			// out of the picture - does not depend on its position.
			name:             "V1c/documentTypeDeclarationBetweenRules",
			xml:              `<CORSConfiguration>` + corsTestRule(corsMinimalRuleBody) + `<!DOCTYPE x>` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: "Unexpected document type declaration",
		},
		{
			name:             "V1c/documentTypeDeclarationInsideARule",
			xml:              corsTestDoc(`<CORSRule><!DOCTYPE x/>` + corsMinimalRuleBody + `</CORSRule>`),
			wantErrSubstring: "Unexpected document type declaration",
		},
		{
			name:             "V1c/documentTypeDeclarationInsideAValueElement",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET<!DOCTYPE x></AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: "Unexpected document type declaration",
		},
		{
			name:      "V1c/leadingXMLDeclarationIsAccepted",
			xml:       xml.Header + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			name:      "V1c/commentsAreAccepted",
			xml:       `<!-- before -->` + corsTestDoc(`<!-- inside -->`+corsTestRule(corsMinimalRuleBody)) + `<!-- after -->`,
			wantRules: 1,
		},
		{
			name: "V1c/prettyPrintedDocumentIsAccepted",
			xml: "<CORSConfiguration xmlns=\"" + s3CORSNamespace + "\">\n" +
				"  <CORSRule>\n" +
				"    <AllowedMethod>GET</AllowedMethod>\n" +
				"    <AllowedOrigin>*</AllowedOrigin>\n" +
				"  </CORSRule>\n" +
				"</CORSConfiguration>\n",
			wantRules: 1,
		},
		{
			// A leading byte order mark is an encoding signature XML 1.0
			// permits, so the document is well-formed and must be accepted.
			name:      "V1c/leadingUTF8BOMIsAccepted",
			xml:       utf8BOM + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			name:      "V1c/leadingUTF8BOMBeforeTheXMLDeclarationIsAccepted",
			xml:       utf8BOM + xml.Header + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			// Only the first mark is a signature. A second one is an ordinary
			// character standing before the root element, which is content the
			// document may not carry.
			name:             "V1c/repeatedLeadingUTF8BOMIsRejected",
			xml:              utf8BOM + utf8BOM + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantErrSubstring: "character data before the CORSConfiguration element",
		},
		{
			name:             "V1c/utf8BOMFollowedByNoDocumentIsRejected",
			xml:              utf8BOM,
			wantErrSubstring: "does not contain a CORSConfiguration element",
		},
		{
			// Consuming the signature must not turn into tolerating U+FEFF
			// anywhere: after the root element it is a trailing character like
			// any other.
			name:             "V1c/utf8BOMAfterTheDocumentIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + utf8BOM,
			wantErrSubstring: "character data after the CORSConfiguration element",
		},

		// Element placement and cardinality. The document model would otherwise
		// normalize a violation away: a repeated scalar keeps only its last
		// value and an unknown element is skipped without comment.
		{
			name:             "V1d/unexpectedElementUnderRoot",
			xml:              `<CORSConfiguration><NotARule/>` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `CORSConfiguration contains unsupported element "NotARule"`,
		},
		{
			name:             "V1d/unexpectedElementInCORSRule",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<AllowedHeaders>x-a</AllowedHeaders>`)),
			wantErrSubstring: `CORSRule 0 contains unsupported element "AllowedHeaders"`,
		},
		{
			name:             "V1d/unexpectedElementInSecondCORSRuleReportsItsOwnIndex",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody), corsTestRule(corsMinimalRuleBody+`<Bogus/>`)),
			wantErrSubstring: `CORSRule 1 contains unsupported element "Bogus"`,
		},
		{
			name:             "V1d/elementNestedInsideAValueElement",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod><GET/></AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `CORSRule 0 has AllowedMethod containing the nested element "GET"`,
		},
		{
			name:             "V1d/duplicateID",
			xml:              corsTestDoc(corsTestRule(`<ID>first</ID><ID>second</ID>` + corsMinimalRuleBody)),
			wantErrSubstring: "CORSRule 0 contains 2 ID elements, at most one is allowed",
		},
		{
			name:             "V1d/duplicateMaxAgeSeconds",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>10</MaxAgeSeconds><MaxAgeSeconds>20</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements, at most one is allowed",
		},
		{
			// Without the cardinality check the decoder would keep only the
			// trailing, valid value and the negative one would never be seen.
			name:             "V1d/duplicateMaxAgeSecondsCannotMaskANegativeValue",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>-1</MaxAgeSeconds><MaxAgeSeconds>3000</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements, at most one is allowed",
		},
		{
			name:      "V1d/repeatedListElementsAreAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedHeader>x-a</AllowedHeader><AllowedHeader>x-b</AllowedHeader><AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod><AllowedOrigin>https://a.example.com</AllowedOrigin><AllowedOrigin>https://b.example.com</AllowedOrigin><ExposeHeader>ETag</ExposeHeader><ExposeHeader>x-amz-request-id</ExposeHeader>`)),
			wantRules: 1,
		},

		{
			name:             "V2/zeroCORSRule",
			xml:              `<CORSConfiguration></CORSConfiguration>`,
			wantErrSubstring: "must contain at least one CORSRule",
		},
		{
			name:             "V2/zeroCORSRuleWithNamespace",
			xml:              corsTestDoc(),
			wantErrSubstring: "must contain at least one CORSRule",
		},

		{
			name:      "V3/exactlyMaxRulesIsAccepted",
			xml:       corsTestDoc(strings.Repeat(corsTestRule(corsMinimalRuleBody), maxBucketCORSRules)),
			wantRules: maxBucketCORSRules,
		},
		{
			name:             "V3/oneRuleOverMaxRulesIsRejected",
			xml:              corsTestDoc(strings.Repeat(corsTestRule(corsMinimalRuleBody), maxBucketCORSRules+1)),
			wantErrSubstring: fmt.Sprintf("at most %d are allowed", maxBucketCORSRules),
		},

		{
			name:             "V4/ruleWithoutAllowedMethod",
			xml:              corsTestDoc(corsTestRule(`<AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: "CORSRule 0 must contain at least one AllowedMethod",
		},
		{
			name:             "V4/secondRuleWithoutAllowedMethodReportsItsOwnIndex",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody), corsTestRule(`<AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: "CORSRule 1 must contain at least one AllowedMethod",
		},

		{
			name:             "V5/unsupportedAllowedMethodPATCH",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>PATCH</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `unsupported AllowedMethod "PATCH"`,
		},
		{
			name:             "V5/unsupportedAllowedMethodOPTIONS",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>OPTIONS</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `unsupported AllowedMethod "OPTIONS"`,
		},
		{
			name:             "V5/unsupportedAllowedMethodIsReportedNormalized",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>patch</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `unsupported AllowedMethod "PATCH"`,
		},
		{
			name:             "V5/unsupportedAllowedMethodBesideASupportedOne",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedMethod>TRACE</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `unsupported AllowedMethod "TRACE"`,
		},

		{
			name:             "V6/ruleWithoutAllowedOrigin",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod>`)),
			wantErrSubstring: "CORSRule 0 must contain at least one AllowedOrigin",
		},

		{
			name:             "V7/allowedOriginWithTwoWildcards",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>http://*.example.*</AllowedOrigin>`)),
			wantErrSubstring: `AllowedOrigin "http://*.example.*" with more than one wildcard`,
		},
		{
			name:             "V7/secondAllowedOriginWithTwoWildcards",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://ok.example.com</AllowedOrigin><AllowedOrigin>http://*.bad.*</AllowedOrigin>`)),
			wantErrSubstring: `AllowedOrigin "http://*.bad.*" with more than one wildcard`,
		},
		{
			name:      "V7/bareWildcardOriginIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantRules: 1,
		},
		{
			name:      "V7/singleEmbeddedWildcardOriginIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>http://www.example2.*</AllowedOrigin>`)),
			wantRules: 1,
		},

		{
			name:             "V8/allowedHeaderWithTwoWildcards",
			xml:              corsTestDoc(corsTestRule(`<AllowedHeader>x-*-*</AllowedHeader>` + corsMinimalRuleBody)),
			wantErrSubstring: `AllowedHeader "x-*-*" with more than one wildcard`,
		},
		{
			name:      "V8/bareWildcardHeaderIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedHeader>*</AllowedHeader>` + corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			name:      "V8/singleEmbeddedWildcardHeaderIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedHeader>x-amz-*</AllowedHeader>` + corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			name:      "V8/zeroAllowedHeaderIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantRules: 1,
		},

		// Body-size boundary: the body is bounded by maxBucketCORSConfigSize.
		// The boundary itself is accepted and one byte more is rejected as too
		// large, which is only provable because the validator reads one byte
		// past the ceiling instead of validating a truncated prefix of the body.
		{
			name:      "V9/oneByteUnderMaxConfigSizeIsAccepted",
			xml:       corsPaddedDoc(maxBucketCORSConfigSize - sizeOverhead - 1),
			wantRules: 1,
		},
		{
			name:      "V9/exactlyMaxConfigSizeIsAccepted",
			xml:       corsPaddedDoc(maxBucketCORSConfigSize - sizeOverhead),
			wantRules: 1,
		},
		{
			name:             "V9/oneByteOverMaxConfigSizeIsRejected",
			xml:              corsPaddedDoc(maxBucketCORSConfigSize - sizeOverhead + 1),
			wantErrSubstring: fmt.Sprintf("larger than the maximum of %d bytes", maxBucketCORSConfigSize),
		},
		{
			// A complete, valid document followed by enough padding to push the
			// body over the ceiling: the decoder would stop at the root element
			// and never see the padding, so only a size check on the whole body
			// can reject it.
			name: "V9/validDocumentFollowedByOversizePaddingIsRejected",
			xml: corsTestDoc(corsTestRule(corsMinimalRuleBody)) +
				strings.Repeat(" ", maxBucketCORSConfigSize),
			wantErrSubstring: fmt.Sprintf("larger than the maximum of %d bytes", maxBucketCORSConfigSize),
		},
		{
			// A document far over the ceiling must be rejected on its length
			// too, rather than on whatever the truncated prefix happens to be.
			name:             "V9/farOverMaxConfigSizeIsRejected",
			xml:              corsPaddedDoc(2 * maxBucketCORSConfigSize),
			wantErrSubstring: fmt.Sprintf("larger than the maximum of %d bytes", maxBucketCORSConfigSize),
		},
		{
			// The worst case for a ceiling that only truncates: a small, valid
			// document whose root element closes long before the limit,
			// followed by an oversized tail. The request is over the ceiling
			// and must be rejected on its length.
			name:             "V9/validRootFollowedByAnOversizedTailIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + strings.Repeat("t", maxBucketCORSConfigSize),
			wantErrSubstring: fmt.Sprintf("larger than the maximum of %d bytes", maxBucketCORSConfigSize),
		},

		// Namespace compatibility: the wire format is the AWS one, so the
		// document either omits the namespace or declares exactly the S3
		// namespace. Any other value would be preserved on the persisted document
		// and handed back to clients that cannot read it.
		{
			name: "R8/wrongNamespaceIsRejected",
			xml: `<CORSConfiguration xmlns="http://example.com/wrong">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `Unexpected XML namespace "http://example.com/wrong"`,
		},
		{
			// The element namespace is the S3 one through its prefix, yet the
			// default xmlns attribute is not, and it is the attribute that the
			// marshaller emits. Neither check implies the other.
			name: "R8/wrongDefaultNamespaceAttributeIsRejected",
			xml: `<s3:CORSConfiguration xmlns:s3="` + s3CORSNamespace + `" xmlns="http://example.com/wrong">` +
				`<s3:CORSRule><s3:AllowedMethod>GET</s3:AllowedMethod><s3:AllowedOrigin>*</s3:AllowedOrigin></s3:CORSRule>` +
				`</s3:CORSConfiguration>`,
			wantErrSubstring: `Unexpected xmlns attribute "http://example.com/wrong"`,
		},
		{
			// A CORSRule in a foreign namespace is not a CORSRule. The document
			// model would adopt it regardless, because it matches child elements
			// by local name alone.
			name:             "R8/foreignNamespaceOnCORSRuleIsRejected",
			xml:              corsTestDoc(`<CORSRule xmlns="http://example.com/wrong">` + corsMinimalRuleBody + `</CORSRule>`),
			wantErrSubstring: `unsupported element "{http://example.com/wrong}CORSRule"`,
		},
		{
			name:      "R8/absentNamespaceIsAccepted",
			xml:       `<CORSConfiguration>` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantRules: 1,
		},
		{
			name:      "R8/emptyNamespaceAttributeIsAccepted",
			xml:       `<CORSConfiguration xmlns="">` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantRules: 1,
		},
		{
			// The S3 namespace bound to a prefix is the same namespace, so a
			// document written that way is equally valid.
			name: "R8/prefixedS3NamespaceIsAccepted",
			xml: `<s3:CORSConfiguration xmlns:s3="` + s3CORSNamespace + `">` +
				`<s3:CORSRule><s3:AllowedMethod>GET</s3:AllowedMethod><s3:AllowedOrigin>*</s3:AllowedOrigin></s3:CORSRule>` +
				`</s3:CORSConfiguration>`,
			wantRules: 1,
		},

		// Single-valued elements: ID and MaxAgeSeconds map to scalar fields of
		// the document model, which silently keeps the last occurrence, so a
		// repeated element can only be caught by validating the document itself.
		{
			name:             "R1/duplicateIDIsRejected",
			xml:              corsTestDoc(corsTestRule(`<ID>first</ID><ID>second</ID>` + corsMinimalRuleBody)),
			wantErrSubstring: "CORSRule 0 contains 2 ID elements, at most one is allowed",
		},
		{
			name: "R1/duplicateMaxAgeSecondsIsRejected",
			xml: corsTestDoc(corsTestRule(corsMinimalRuleBody +
				`<MaxAgeSeconds>3000</MaxAgeSeconds><MaxAgeSeconds>6000</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements, at most one is allowed",
		},
		{
			name: "R1/duplicateIDInSecondRuleReportsItsOwnIndex",
			xml: corsTestDoc(
				corsTestRule(`<ID>only-one</ID>`+corsMinimalRuleBody),
				corsTestRule(`<ID>first</ID><ID>second</ID>`+corsMinimalRuleBody),
			),
			wantErrSubstring: "CORSRule 1 contains 2 ID elements, at most one is allowed",
		},
		{
			name:      "R1/singleIDIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<ID>only-one</ID>` + corsMinimalRuleBody)),
			wantRules: 1,
		},

		// Unknown elements: an element the document model does not know is
		// discarded while decoding, so a client would be told a configuration was
		// stored that the server never understood. Every unknown element is
		// rejected, and so is a known element in an invalid position.
		{
			name:             "schema/unknownElementInCORSConfigurationIsRejected",
			xml:              corsTestDoc(`<CORSPolicy/>`, corsTestRule(corsMinimalRuleBody)),
			wantErrSubstring: `CORSConfiguration contains unsupported element "CORSPolicy"`,
		},
		{
			name:             "schema/unknownElementInCORSRuleIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<AllowedCredentials>true</AllowedCredentials>`)),
			wantErrSubstring: `CORSRule 0 contains unsupported element "AllowedCredentials"`,
		},
		{
			// The realistic failure this guards against: a plural spelling of a
			// singular element name, silently dropped, leaving a rule that
			// allows an origin the client never listed.
			name:             "schema/misspelledAllowedOriginIsRejected",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigins>*</AllowedOrigins>`)),
			wantErrSubstring: `CORSRule 0 contains unsupported element "AllowedOrigins"`,
		},
		{
			name:             "schema/lowerCaseElementNameIsRejected",
			xml:              corsTestDoc(corsTestRule(`<allowedmethod>GET</allowedmethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `CORSRule 0 contains unsupported element "allowedmethod"`,
		},
		{
			name:             "schema/nestedElementInsideAllowedMethodIsRejected",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod><GET/></AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `CORSRule 0 has AllowedMethod containing the nested element "GET"`,
		},
		{
			name:             "schema/nestedCORSRuleIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + corsTestRule(corsMinimalRuleBody))),
			wantErrSubstring: `CORSRule 0 contains unsupported element "CORSRule"`,
		},
		{
			name:             "schema/characterDataInsideCORSRuleIsRejected",
			xml:              corsTestDoc(`<CORSRule>stray text` + corsMinimalRuleBody + `</CORSRule>`),
			wantErrSubstring: "CORSRule 0 contains character data outside of a child element",
		},
		{
			name:             "schema/characterDataInsideCORSConfigurationIsRejected",
			xml:              `<CORSConfiguration>stray text` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: "CORSConfiguration contains character data outside of a CORSRule element",
		},
		{
			name:             "schema/characterDataBeforeRootIsRejected",
			xml:              `stray text` + corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantErrSubstring: "character data before the CORSConfiguration element",
		},
		{
			// A child element of a rule is only that child element when it
			// belongs to the namespace of the document.
			name: "schema/foreignNamespaceOnRuleChildIsRejected",
			xml: corsTestDoc(corsTestRule(
				`<AllowedMethod xmlns="http://example.com/wrong">GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `CORSRule 0 contains unsupported element "{http://example.com/wrong}AllowedMethod"`,
		},
		{
			// Whitespace between elements is insignificant, so the indented
			// document every hand written and pretty-printed client sends must
			// still be accepted.
			name: "schema/indentedDocumentIsAccepted",
			xml: "<CORSConfiguration xmlns=\"" + s3CORSNamespace + "\">\n" +
				"\t<CORSRule>\n" +
				"\t\t<AllowedMethod>GET</AllowedMethod>\n" +
				"\t\t<AllowedOrigin>https://www.example1.com</AllowedOrigin>\n" +
				"\t</CORSRule>\n" +
				"</CORSConfiguration>\n",
			wantRules: 1,
		},
		{
			name: "schema/declarationAndCommentsAreAccepted",
			xml: xml.Header + "<!-- written by hand -->" +
				`<CORSConfiguration xmlns="` + s3CORSNamespace + `"><!-- first rule -->` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantRules: 1,
		},

		// Trailing content: the decoder stops at the end of the first root
		// element and the document model never learns what followed it, so a
		// trailing tail has to be rejected explicitly.
		{
			name:             "trailer/characterDataAfterRootIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody)) + "trailing garbage",
			wantErrSubstring: "character data after the CORSConfiguration element",
		},
		{
			name: "trailer/secondRootElementIsRejected",
			xml: corsTestDoc(corsTestRule(corsMinimalRuleBody)) +
				corsTestDoc(corsTestRule(`<AllowedMethod>DELETE</AllowedMethod><AllowedOrigin>https://www.evil.example</AllowedOrigin>`)),
			wantErrSubstring: `contains the element "CORSConfiguration" after the CORSConfiguration element`,
		},
		{
			name:      "trailer/whitespaceAfterRootIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody)) + "\n\t \n",
			wantRules: 1,
		},
		{
			name:      "trailer/commentAfterRootIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody)) + "\n<!-- end of configuration -->\n",
			wantRules: 1,
		},

		// No element may carry the same attribute twice. XML 1.0 forbids it
		// outright and the Namespaces specification extends the prohibition to
		// two attributes whose expanded names are equal however they were
		// spelled, so every document below is one that a conforming parser
		// refuses to read at all. Go's decoder enforces neither rule and reports
		// the repetitions as ordinary attributes, so without an explicit check
		// each of these was accepted, persisted, and handed back to clients as a
		// configuration - replacing one that was actually valid.
		{
			name: "attributes/duplicateIdenticalDefaultNamespaceDeclarationIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns="` + s3CORSNamespace + `">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `The element "CORSConfiguration" declares the attribute "xmlns" more than once`,
		},
		{
			// Two default declarations with different values are rejected for
			// repeating the attribute rather than for the namespace the decoder
			// happened to resolve last, so the client is told what is actually
			// wrong with the document it sent.
			name: "attributes/duplicateDifferingDefaultNamespaceDeclarationIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns="urn:evil">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `The element "CORSConfiguration" declares the attribute "xmlns" more than once`,
		},
		{
			name: "attributes/duplicateOrdinaryAttributeOnTheRootIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" foo="1" foo="2">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `The element "CORSConfiguration" declares the attribute "foo" more than once`,
		},
		{
			name:             "attributes/duplicateOrdinaryAttributeOnACORSRuleIsRejected",
			xml:              corsTestDoc(`<CORSRule bar="1" bar="2">` + corsMinimalRuleBody + `</CORSRule>`),
			wantErrSubstring: `The element "CORSRule" declares the attribute "bar" more than once`,
		},
		{
			// A repetition on a value element is refused even though a single
			// attribute there is deliberately ignored: one attribute cannot be
			// mistaken for a configured value, while a document no parser will
			// read cannot be stored as though it had been valid.
			name: "attributes/duplicateOrdinaryAttributeOnAValueElementIsRejected",
			xml: corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod>` +
				`<AllowedOrigin baz="1" baz="2">https://example.com</AllowedOrigin>`)),
			wantErrSubstring: `The element "AllowedOrigin" declares the attribute "baz" more than once`,
		},
		{
			name: "attributes/duplicateNamespacePrefixDeclarationIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns:a="urn:one" xmlns:a="urn:two">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `The element "CORSConfiguration" declares the attribute "xmlns:a" more than once`,
		},
		{
			// The two attributes are spelled differently yet name the same
			// expanded attribute, because both prefixes are bound to the same
			// namespace. Comparing the resolved name is what catches it.
			name: "attributes/duplicateExpandedAttributeReachedThroughTwoPrefixesIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns:p="urn:x" xmlns:q="urn:x" p:k="1" q:k="2">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `The element "CORSConfiguration" declares the attribute "{urn:x}k" more than once`,
		},
		{
			// The xml prefix is bound to its namespace without a declaration, so
			// a repeated xml:lang is a repeated expanded name as well.
			name: "attributes/duplicateReservedXMLAttributeIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xml:lang="en" xml:lang="fr">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `declares the attribute "{http://www.w3.org/XML/1998/namespace}lang" more than once`,
		},
		{
			// An undeclared prefix leaves the attribute name unresolved, and two
			// attributes carrying the same unresolved name are still the same
			// name twice.
			name: "attributes/duplicateAttributeWithAnUndeclaredPrefixIsRejected",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" q:k="1" q:k="2">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantErrSubstring: `declares the attribute "{q}k" more than once`,
		},
		{
			// The repetition is on the third rule, which is reached only because
			// every rule is walked rather than only the first.
			name: "attributes/duplicateAttributeInALaterRuleIsRejected",
			xml: corsTestDoc(
				corsTestRule(corsMinimalRuleBody),
				corsTestRule(corsMinimalRuleBody),
				`<CORSRule dup="1" dup="2">`+corsMinimalRuleBody+`</CORSRule>`,
			),
			wantErrSubstring: `The element "CORSRule" declares the attribute "dup" more than once`,
		},
		{
			// Distinct attributes are not a repetition, whatever their number,
			// and a single attribute on any element remains ignored rather than
			// refused, so the check adds no strictness AWS does not have.
			name: "attributes/distinctAttributesAreAccepted",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns:a="urn:one" xmlns:b="urn:two" a:k="1" b:k="2" foo="3" bar="4">` +
				`<CORSRule note="one"><AllowedMethod lang="en">GET</AllowedMethod><AllowedOrigin lang="en">*</AllowedOrigin></CORSRule>` +
				`</CORSConfiguration>`,
			wantRules: 1,
		},
		{
			// A prefixed attribute and an ordinary one that share a local name
			// have different expanded names, so neither is a repetition of the
			// other.
			name: "attributes/aPrefixedAndAnUnprefixedAttributeSharingALocalNameAreAccepted",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" xmlns:a="urn:one" a:k="1" k="2">` +
				corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`,
			wantRules: 1,
		},
		{
			// The same attribute name on two different elements is not a
			// repetition either: the constraint is per element.
			name: "attributes/theSameAttributeNameOnDifferentElementsIsAccepted",
			xml: `<CORSConfiguration xmlns="` + s3CORSNamespace + `" tag="root">` +
				`<CORSRule tag="rule"><AllowedMethod tag="method">GET</AllowedMethod><AllowedOrigin tag="origin">*</AllowedOrigin></CORSRule>` +
				`<CORSRule tag="rule"><AllowedMethod tag="method">PUT</AllowedMethod><AllowedOrigin tag="origin">*</AllowedOrigin></CORSRule>` +
				`</CORSConfiguration>`,
			wantRules: 2,
		},
		{
			// The canonical AWS document declares the S3 namespace exactly once
			// on its root element and nothing else anywhere, which is the shape
			// every real client sends.
			name:      "attributes/theCanonicalDocumentIsAccepted",
			xml:       corsCanonicalDocument,
			wantRules: 4,
		},

		// ID and MaxAgeSeconds may each appear at most once per rule. The parsed
		// model stores both in a scalar field, so a repeated element decodes
		// without complaint and silently keeps only the value that appears last;
		// these cases prove the document is rejected on the wire instead of being
		// persisted as something the client did not send.
		{
			name:             "cardinality/duplicateIDIsRejected",
			xml:              corsTestDoc(corsTestRule(`<ID>first</ID><ID>second</ID>` + corsMinimalRuleBody)),
			wantErrSubstring: "CORSRule 0 contains 2 ID elements, at most one is allowed",
		},
		{
			name: "cardinality/duplicateIDInALaterRuleReportsItsOwnIndex",
			xml: corsTestDoc(
				corsTestRule(`<ID>only-once</ID>`+corsMinimalRuleBody),
				corsTestRule(`<ID>a</ID><ID>b</ID><ID>c</ID>`+corsMinimalRuleBody),
			),
			wantErrSubstring: "CORSRule 1 contains 3 ID elements",
		},
		{
			name:             "cardinality/duplicateMaxAgeSecondsIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>3000</MaxAgeSeconds><MaxAgeSeconds>6000</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements, at most one is allowed",
		},
		{
			name: "cardinality/duplicateMaxAgeSecondsInALaterRuleReportsItsOwnIndex",
			xml: corsTestDoc(
				corsTestRule(corsMinimalRuleBody+`<MaxAgeSeconds>3000</MaxAgeSeconds>`),
				corsTestRule(corsMinimalRuleBody+`<MaxAgeSeconds>10</MaxAgeSeconds><MaxAgeSeconds>20</MaxAgeSeconds>`),
			),
			wantErrSubstring: "CORSRule 1 contains 2 MaxAgeSeconds elements",
		},
		{
			name:      "cardinality/singleIDAndMaxAgeSecondsAreAccepted",
			xml:       corsTestDoc(corsTestRule(`<ID>only-once</ID>` + corsMinimalRuleBody + `<MaxAgeSeconds>3000</MaxAgeSeconds>`)),
			wantRules: 1,
		},
		{
			name: "cardinality/oneIDAndMaxAgeSecondsPerRuleAcrossRulesIsAccepted",
			xml: corsTestDoc(
				corsTestRule(`<ID>rule-zero</ID>`+corsMinimalRuleBody+`<MaxAgeSeconds>100</MaxAgeSeconds>`),
				corsTestRule(`<ID>rule-one</ID>`+corsMinimalRuleBody+`<MaxAgeSeconds>200</MaxAgeSeconds>`),
			),
			wantRules: 2,
		},
		{
			name: "cardinality/repeatedMultiValuedElementsAreAccepted",
			xml: corsTestDoc(corsTestRule(
				`<ID>repeats-what-it-may</ID>`,
				`<AllowedHeader>x-a</AllowedHeader><AllowedHeader>x-b</AllowedHeader>`,
				`<AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod>`,
				`<AllowedOrigin>https://www.example1.com</AllowedOrigin><AllowedOrigin>https://www.example2.com</AllowedOrigin>`,
				`<ExposeHeader>ETag</ExposeHeader><ExposeHeader>x-amz-request-id</ExposeHeader>`,
			)),
			wantRules: 1,
		},
		{
			// The element count is scoped to the rules of the single document the
			// parser decodes, so a second root element is never counted into
			// them. It is refused by the trailing content check instead, which is
			// the only reading under which the count and the parser agree.
			name: "cardinality/elementsAfterTheRootElementAreRejectedRatherThanCounted",
			xml: corsTestDoc(corsTestRule(`<ID>only-once</ID>`+corsMinimalRuleBody)) +
				corsTestDoc(corsTestRule(`<ID>a</ID><ID>b</ID>`+corsMinimalRuleBody)),
			wantErrSubstring: `contains the element "CORSConfiguration" after the CORSConfiguration element`,
		},

		// ID is optional and appears at most once per rule. A repeated element
		// must be rejected rather than silently collapsed onto the single scalar
		// field of the configuration model.
		{
			name:             "ID/duplicateIsRejected",
			xml:              corsTestDoc(corsTestRule(`<ID>first</ID><ID>second</ID>` + corsMinimalRuleBody)),
			wantErrSubstring: "CORSRule 0 contains 2 ID elements",
		},
		{
			name:             "ID/threeOccurrencesAreRejected",
			xml:              corsTestDoc(corsTestRule(`<ID>first</ID><ID>second</ID><ID>third</ID>` + corsMinimalRuleBody)),
			wantErrSubstring: "CORSRule 0 contains 3 ID elements",
		},
		{
			name: "ID/duplicateInTheSecondRuleReportsItsOwnIndex",
			xml: corsTestDoc(
				corsTestRule(`<ID>only</ID>`+corsMinimalRuleBody),
				corsTestRule(`<ID>first</ID>`+corsMinimalRuleBody+`<ID>second</ID>`),
			),
			wantErrSubstring: "CORSRule 1 contains 2 ID elements",
		},
		{
			name:      "ID/singleOccurrenceIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<ID>only-one</ID>` + corsMinimalRuleBody)),
			wantRules: 1,
		},
		{
			name:      "ID/omittedIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody)),
			wantRules: 1,
		},

		// MaxAgeSeconds appears at most once per rule and must be a
		// non-negative integer when present.
		{
			name:             "maxAgeSeconds/duplicateIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>10</MaxAgeSeconds><MaxAgeSeconds>20</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements",
		},
		{
			// The check counts elements instead of comparing their values, so
			// two identical occurrences are a duplicate just the same.
			name:             "maxAgeSeconds/duplicateWithIdenticalValuesIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>0</MaxAgeSeconds><MaxAgeSeconds>0</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements",
		},
		{
			// Cardinality is validated before the value is converted to an
			// integer, so the duplicate is reported rather than the fact that
			// the second occurrence is not a number.
			name:             "maxAgeSeconds/duplicateIsRejectedBeforeScalarConversion",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>10</MaxAgeSeconds><MaxAgeSeconds>not-a-number</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 contains 2 MaxAgeSeconds elements",
		},
		{
			name: "maxAgeSeconds/duplicateInTheSecondRuleReportsItsOwnIndex",
			xml: corsTestDoc(
				corsTestRule(corsMinimalRuleBody+`<MaxAgeSeconds>10</MaxAgeSeconds>`),
				corsTestRule(corsMinimalRuleBody+`<MaxAgeSeconds>20</MaxAgeSeconds><MaxAgeSeconds>30</MaxAgeSeconds>`),
			),
			wantErrSubstring: "CORSRule 1 contains 2 MaxAgeSeconds elements",
		},
		{
			name:             "maxAgeSeconds/negativeIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>-1</MaxAgeSeconds>`)),
			wantErrSubstring: "CORSRule 0 has negative MaxAgeSeconds -1",
		},
		{
			name:             "maxAgeSeconds/nonNumericIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>not-a-number</MaxAgeSeconds>`)),
			wantErrSubstring: "invalid syntax",
		},
		{
			name:      "maxAgeSeconds/zeroIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>0</MaxAgeSeconds>`)),
			wantRules: 1,
		},
		{
			name:      "maxAgeSeconds/positiveIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody + `<MaxAgeSeconds>3000</MaxAgeSeconds>`)),
			wantRules: 1,
		},

		{
			name: "accepted/fullyPopulatedRule",
			xml: corsTestDoc(corsTestRule(
				`<ID>fully-populated</ID>`,
				`<AllowedHeader>x-amz-meta-foo</AllowedHeader><AllowedHeader>content-type</AllowedHeader>`,
				`<AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod><AllowedMethod>DELETE</AllowedMethod><AllowedMethod>HEAD</AllowedMethod>`,
				`<AllowedOrigin>https://www.example1.com</AllowedOrigin><AllowedOrigin>http://www.example2.*</AllowedOrigin>`,
				`<ExposeHeader>ETag</ExposeHeader><ExposeHeader>x-amz-request-id</ExposeHeader>`,
				`<MaxAgeSeconds>3000</MaxAgeSeconds>`,
			)),
			wantRules: 1,
		},
		{
			name:      "accepted/canonicalMultiRuleDocument",
			xml:       corsCanonicalDocument,
			wantRules: 4,
		},

		// Normalization: XML carries a document's indentation into the text of
		// its elements, so values are trimmed before they are validated.
		{
			name: "normalization/valuesOnTheirOwnLinesAreAccepted",
			xml: "<CORSConfiguration>\n" +
				"  <CORSRule>\n" +
				"    <AllowedOrigin>\n      https://app.example.com\n    </AllowedOrigin>\n" +
				"    <AllowedMethod>\n      GET\n    </AllowedMethod>\n" +
				"    <AllowedHeader>\n      x-amz-acl\n    </AllowedHeader>\n" +
				"    <ExposeHeader>\n      ETag\n    </ExposeHeader>\n" +
				"    <ID>\n      hand written\n    </ID>\n" +
				"  </CORSRule>\n" +
				"</CORSConfiguration>\n",
			wantRules: 1,
		},
		{
			// Without normalization this document is rejected outright, for
			// naming a method that it does in fact name correctly.
			name:      "normalization/paddedAllowedMethodIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>  GET  </AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantRules: 1,
		},
		{
			name:      "normalization/paddedAllowedOriginIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody + `<AllowedOrigin>  https://app.example.com  </AllowedOrigin>`)),
			wantRules: 1,
		},
		{
			name:      "normalization/cdataPaddedAllowedOriginIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin><![CDATA[  https://app.example.com  ]]></AllowedOrigin>`)),
			wantRules: 1,
		},
		{
			// Trimming a value to nothing leaves the rule matching nothing,
			// which is the same outcome an explicitly empty AllowedOrigin has
			// and is therefore accepted rather than refused: a rule that
			// matches nothing can only ever deny.
			name:      "normalization/whitespaceOnlyAllowedOriginIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>   </AllowedOrigin>`)),
			wantRules: 1,
		},
		{
			// Normalization must not reach inside a value: an interior space is
			// part of the value and still makes the method unsupported.
			name:             "normalization/allowedMethodWithAnInteriorSpaceIsStillRejected",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GE T</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantErrSubstring: `unsupported AllowedMethod "GE T"`,
		},
		{
			// Trimming may not widen a rule either: only the whitespace around
			// the value is removed, never a character of the value, so a
			// two-wildcard origin stays refused.
			name:             "normalization/paddedAllowedOriginWithTwoWildcardsIsStillRejected",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>  *a*  </AllowedOrigin>`)),
			wantErrSubstring: `AllowedOrigin "*a*" with more than one wildcard`,
		},

		// Control characters: the XML decoder already refuses the ones XML
		// forbids, so what must be refused here is a tab, carriage return or
		// line feed sitting inside a value, where it survives trimming and
		// would otherwise reach a response header.
		{
			name:             "control/allowedOriginWithALineFeedIsRejected",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://a&#10;.example.com</AllowedOrigin>`)),
			wantErrSubstring: "AllowedOrigin \"https://a\\n.example.com\" containing a control character",
		},
		{
			name:             "control/allowedHeaderWithATabIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + "<AllowedHeader>x-amz-a\tb</AllowedHeader>")),
			wantErrSubstring: "AllowedHeader \"x-amz-a\\tb\" containing a control character",
		},
		{
			// The response splitting attempt: a stored ExposeHeader carrying
			// CRLF and a header of its own. It is refused at the door rather
			// than relied upon to be defused when it is written out.
			name:             "control/exposeHeaderWithCarriageReturnLineFeedIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + "<ExposeHeader>ETag\r\nX-Injected: yes</ExposeHeader>")),
			wantErrSubstring: "ExposeHeader \"ETag\\nX-Injected: yes\" containing a control character",
		},
		{
			name:             "control/exposeHeaderWithACharacterReferenceLineFeedIsRejected",
			xml:              corsTestDoc(corsTestRule(corsMinimalRuleBody + `<ExposeHeader>ETag&#10;X-Injected: yes</ExposeHeader>`)),
			wantErrSubstring: "containing a control character",
		},
		{
			name:             "control/allowedOriginWithADeleteCharacterIsRejected",
			xml:              corsTestDoc(corsTestRule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://a\u007f.example.com</AllowedOrigin>")),
			wantErrSubstring: "containing a control character",
		},
		{
			// A value whose only control characters surround it is trimmed, so
			// it is accepted: the check applies to what is left after trimming.
			name:      "control/exposeHeaderSurroundedByControlCharactersIsAccepted",
			xml:       corsTestDoc(corsTestRule(corsMinimalRuleBody + "<ExposeHeader>\r\n\tETag\t\r\n</ExposeHeader>")),
			wantRules: 1,
		},
		{
			// ID is never matched and never emitted as a header, so it is
			// deliberately exempt: it is handed back exactly as it was stored.
			name:      "control/idWithALineFeedIsAccepted",
			xml:       corsTestDoc(corsTestRule(`<ID>first&#10;second</ID>` + corsMinimalRuleBody)),
			wantRules: 1,
		},
	}

	for _, method := range allSupportedCORSMethods {
		testCases = append(testCases, validateCase{
			name:      "V5/supportedAllowedMethod" + method,
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>` + method + `</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantRules: 1,
		})
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// PutBucketCorsHandler hands the request body straight to the
			// validator, which owns the size ceiling; feeding an unbounded
			// reader here keeps the size cases honest.
			cfg, err := validateBucketCorsConfig(strings.NewReader(tc.xml))

			if tc.wantErrSubstring == "" {
				if err != nil {
					t.Fatalf("expected the document to be accepted, got error: %v", err)
				}
				if cfg == nil {
					t.Fatal("expected a configuration on success, got nil")
				}
				if len(cfg.CORSRules) != tc.wantRules {
					t.Fatalf("expected %d rules, got %d", tc.wantRules, len(cfg.CORSRules))
				}
				if cfg.XMLNS != s3CORSNamespace {
					t.Fatalf("expected namespace %q, got %q", s3CORSNamespace, cfg.XMLNS)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected the document to be rejected with an error mentioning %q, got a configuration with %d rules",
					tc.wantErrSubstring, len(cfg.CORSRules))
			}
			if cfg != nil {
				t.Fatal("expected a nil configuration on failure")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Fatalf("expected the error to mention %q, got %q", tc.wantErrSubstring, err)
			}
		})
	}

	// A caller that bounds the body itself must not be able to turn an
	// oversized document into an accepted one: truncating at the ceiling can
	// close the root element early, so the shortened document has to fail the
	// strict structural check instead of being persisted with its trailing
	// rules silently dropped.
	t.Run("callerSideLimitReaderStillRejectsAnOversizedDocument", func(t *testing.T) {
		oversized := corsTestDoc(strings.Repeat(corsTestRule(corsMinimalRuleBody), maxBucketCORSRules))
		for len(oversized) <= maxBucketCORSConfigSize {
			oversized += corsTestRule(corsMinimalRuleBody)
		}
		bounded := io.LimitReader(strings.NewReader(oversized), maxBucketCORSConfigSize)
		if cfg, err := validateBucketCorsConfig(bounded); err == nil {
			t.Fatalf("expected a truncated oversized document to be rejected, got a configuration with %d rules",
				len(cfg.CORSRules))
		}
	})

	// An io.Reader failure must surface as an error rather than as an empty,
	// and therefore invalid, document being blamed on the client's XML.
	t.Run("readErrorIsReported", func(t *testing.T) {
		wantErr := errors.New("connection reset by peer")
		cfg, err := validateBucketCorsConfig(iotest.ErrReader(wantErr))
		if err == nil {
			t.Fatalf("expected the read error to be reported, got a configuration with %d rules", len(cfg.CORSRules))
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected the read error to be wrapped, got %q", err)
		}
	})
}

// corsCountingReader is an io.Reader that records how many bytes were consumed
// from it, so a test can prove the validator stops reading rather than merely
// prove that it rejects an oversized document.
type corsCountingReader struct {
	remaining int
	read      int
}

func (r *corsCountingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = ' '
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

// TestValidateBucketCorsConfigBoundedRead proves the request body is bounded
// rather than merely validated: the validator must consume at most one byte more
// than maxBucketCORSConfigSize before rejecting an oversized document, because
// reading an unbounded PUT body into memory is a denial-of-service vector.
//
// The extra byte is what makes the ceiling exact - it is the byte whose presence
// proves the body is too long instead of merely as long as the limit allows.
func TestValidateBucketCorsConfigBoundedRead(t *testing.T) {
	body := &corsCountingReader{remaining: 64 * maxBucketCORSConfigSize}

	cfg, err := validateBucketCorsConfig(body)
	if err == nil {
		t.Fatalf("expected an oversized body to be rejected, got a configuration with %d rules", len(cfg.CORSRules))
	}
	if cfg != nil {
		t.Fatal("expected a nil configuration on failure")
	}
	want := fmt.Sprintf("larger than the maximum of %d bytes", maxBucketCORSConfigSize)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected the error to mention %q, got %q", want, err)
	}
	if body.read != maxBucketCORSConfigSize+1 {
		t.Fatalf("expected the validator to read exactly %d bytes, got %d",
			maxBucketCORSConfigSize+1, body.read)
	}
}

// TestValidateBucketCorsConfigReadFailure covers the body that cannot be read at
// all, which is what a client disconnecting mid-upload looks like from here. The
// failure must be reported as a rejection naming the document rather than
// swallowed into an empty configuration.
func TestValidateBucketCorsConfigReadFailure(t *testing.T) {
	readErr := errors.New("connection reset by peer")

	cfg, err := validateBucketCorsConfig(iotest.ErrReader(readErr))
	if err == nil {
		t.Fatalf("expected an unreadable body to be rejected, got a configuration with %d rules", len(cfg.CORSRules))
	}
	if cfg != nil {
		t.Fatal("expected a nil configuration on failure")
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("expected the read failure to be wrapped, got %q", err)
	}
	if !strings.Contains(err.Error(), "Unable to read the CORSConfiguration document") {
		t.Fatalf("expected the error to name the document it could not read, got %q", err)
	}
}

// TestValidateBucketCorsConfigReadError proves that a request body which fails
// part way through is reported as a read failure, instead of being validated as
// though the client had sent a complete document.
//
// The validator reads the body into memory so that the schema walk can examine
// exactly the bytes the parser decoded, which makes the read itself a failure
// path the client has to be told about.
func TestValidateBucketCorsConfigReadError(t *testing.T) {
	wantErr := errors.New("connection reset by peer")
	document := corsTestDoc(corsTestRule(corsMinimalRuleBody))
	body := io.MultiReader(strings.NewReader(document[:len(document)/2]), iotest.ErrReader(wantErr))

	cfg, err := validateBucketCorsConfig(body)
	if err == nil {
		t.Fatalf("expected the read failure to be reported, got a configuration with %d rules", len(cfg.CORSRules))
	}
	if cfg != nil {
		t.Fatal("expected a nil configuration on failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the error to wrap %v, got %v", wantErr, err)
	}
}

// TestValidateCorsElementAttributes covers the well-formedness constraint that
// no element may carry the same attribute twice, directly on the check rather
// than through a document, so that the comparison and the client-visible
// rendering of each attribute name form are pinned independently of the shape of
// any particular document.
//
// The elements are built the way the decoder reports them - namespace
// declarations resolved to the reserved xmlns space, prefixed attributes with
// their prefix already replaced by the namespace it is bound to - because that
// resolved form is what the check actually compares.
func TestValidateCorsElementAttributes(t *testing.T) {
	const xmlNamespace = "http://www.w3.org/XML/1998/namespace"

	corsAttr := func(space, local, value string) xml.Attr {
		return xml.Attr{Name: xml.Name{Space: space, Local: local}, Value: value}
	}

	testCases := []struct {
		name string
		elem xml.StartElement
		// space is the namespace of the document, against which the element's
		// own name is rendered.
		space string
		// wantErr is the exact failure, asserted verbatim because it reaches the
		// client as the description of a MalformedXML error. An empty value means
		// the element must be accepted.
		wantErr string
	}{
		{
			name:  "noAttributesIsAccepted",
			elem:  xml.StartElement{Name: xml.Name{Space: s3CORSNamespace, Local: corsRuleElement}},
			space: s3CORSNamespace,
		},
		{
			name: "oneAttributeIsAccepted",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsConfigurationElement},
				Attr: []xml.Attr{corsAttr("", "xmlns", s3CORSNamespace)},
			},
			space: s3CORSNamespace,
		},
		{
			name: "aRepeatedDefaultNamespaceDeclarationIsRejected",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsConfigurationElement},
				Attr: []xml.Attr{
					corsAttr("", "xmlns", s3CORSNamespace),
					corsAttr("", "xmlns", s3CORSNamespace),
				},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "CORSConfiguration" declares the attribute "xmlns" more than once`,
		},
		{
			name: "aRepeatedPrefixDeclarationIsRejectedAndRenderedWithItsPrefix",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsConfigurationElement},
				Attr: []xml.Attr{
					corsAttr("xmlns", "a", "urn:one"),
					corsAttr("xmlns", "a", "urn:two"),
				},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "CORSConfiguration" declares the attribute "xmlns:a" more than once`,
		},
		{
			name: "aRepeatedOrdinaryAttributeIsRejectedAndRenderedBare",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsRuleElement},
				Attr: []xml.Attr{corsAttr("", "foo", "1"), corsAttr("", "foo", "2")},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "CORSRule" declares the attribute "foo" more than once`,
		},
		{
			name: "aRepeatedNamespacedAttributeIsRejectedAndRenderedExpanded",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: "AllowedOrigin"},
				Attr: []xml.Attr{corsAttr("urn:x", "k", "1"), corsAttr("urn:x", "k", "2")},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "AllowedOrigin" declares the attribute "{urn:x}k" more than once`,
		},
		{
			name: "aRepeatedReservedXMLAttributeIsRejected",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsConfigurationElement},
				Attr: []xml.Attr{corsAttr(xmlNamespace, "lang", "en"), corsAttr(xmlNamespace, "lang", "fr")},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "CORSConfiguration" declares the attribute "{` + xmlNamespace + `}lang" more than once`,
		},
		{
			// The element itself is in a foreign namespace, so it is named the
			// way every other failure in this file names one.
			name: "anElementOutsideTheDocumentNamespaceIsNamedExpanded",
			elem: xml.StartElement{
				Name: xml.Name{Space: "urn:other", Local: corsRuleElement},
				Attr: []xml.Attr{corsAttr("", "foo", "1"), corsAttr("", "foo", "2")},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "{urn:other}CORSRule" declares the attribute "foo" more than once`,
		},
		{
			// Same local name, different namespaces: two attributes, not one
			// repeated.
			name: "twoAttributesSharingALocalNameInDifferentNamespacesAreAccepted",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsConfigurationElement},
				Attr: []xml.Attr{corsAttr("urn:one", "k", "1"), corsAttr("urn:two", "k", "2"), corsAttr("", "k", "3")},
			},
			space: s3CORSNamespace,
		},
		{
			// Values play no part in the comparison, so identical values on
			// distinct names are accepted just as differing values on a repeated
			// name are rejected.
			name: "manyDistinctAttributesWithIdenticalValuesAreAccepted",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsRuleElement},
				Attr: []xml.Attr{
					corsAttr("", "a", "same"),
					corsAttr("", "b", "same"),
					corsAttr("xmlns", "c", "same"),
					corsAttr("urn:x", "d", "same"),
				},
			},
			space: s3CORSNamespace,
		},
		{
			// The repetition is the last pair of a long list, which is reached
			// only because every attribute is compared rather than only the
			// first few.
			name: "aRepetitionAtTheEndOfALongAttributeListIsRejected",
			elem: xml.StartElement{
				Name: xml.Name{Space: s3CORSNamespace, Local: corsRuleElement},
				Attr: []xml.Attr{
					corsAttr("", "a", "1"),
					corsAttr("", "b", "2"),
					corsAttr("", "c", "3"),
					corsAttr("", "d", "4"),
					corsAttr("", "e", "5"),
					corsAttr("", "e", "6"),
				},
			},
			space:   s3CORSNamespace,
			wantErr: `The element "CORSRule" declares the attribute "e" more than once`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCorsElementAttributes(tc.elem, tc.space)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the element to be accepted, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected the element to be rejected with %q, got no error", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("expected the error %q, got %q", tc.wantErr, err)
			}
		})
	}
}

// TestValidateBucketCorsConfigCanonicalDocument verifies the pinned minio-go
// fixture preserves rule values, method normalization, the S3 namespace, and
// byte-identical re-marshaling.
func TestValidateBucketCorsConfigCanonicalDocument(t *testing.T) {
	cfg, err := validateBucketCorsConfig(strings.NewReader(corsCanonicalDocument))
	if err != nil {
		t.Fatalf("the canonical document must be accepted, got error: %v", err)
	}

	wantRules := []miniogocors.Rule{
		{
			AllowedHeader: []string{"*"},
			AllowedMethod: []string{http.MethodPut, http.MethodPost, http.MethodDelete},
			AllowedOrigin: []string{"http://www.example1.com"},
		},
		{
			AllowedHeader: []string{"*"},
			AllowedMethod: []string{http.MethodPut, http.MethodPost, http.MethodDelete},
			AllowedOrigin: []string{"http://www.example2.*"},
		},
		{
			AllowedMethod: []string{http.MethodGet},
			AllowedOrigin: []string{"*"},
			ExposeHeader:  []string{"x-amz-id-2"},
			MaxAgeSeconds: 6000,
		},
		{
			AllowedMethod: []string{http.MethodPost},
			AllowedOrigin: []string{"https://www.example3.com"},
		},
	}

	t.Run("ruleCountAndPerRuleFieldValues", func(t *testing.T) {
		if len(cfg.CORSRules) != len(wantRules) {
			t.Fatalf("expected %d rules, got %d", len(wantRules), len(cfg.CORSRules))
		}
		for i := range wantRules {
			if !reflect.DeepEqual(cfg.CORSRules[i], wantRules[i]) {
				t.Errorf("rule %d: expected %+v, got %+v", i, wantRules[i], cfg.CORSRules[i])
			}
		}
	})

	t.Run("allowedMethodsAreUpperCased", func(t *testing.T) {
		for i, rule := range cfg.CORSRules {
			for j, method := range rule.AllowedMethod {
				if method != strings.ToUpper(method) {
					t.Errorf("rule %d AllowedMethod %d: expected an upper case value, got %q", i, j, method)
				}
			}
		}

		mixedCase := corsTestDoc(corsTestRule(
			`<AllowedMethod>get</AllowedMethod><AllowedMethod>Put</AllowedMethod><AllowedMethod>hEaD</AllowedMethod>`,
			`<AllowedOrigin>*</AllowedOrigin>`,
		))
		mixedCfg, err := validateBucketCorsConfig(strings.NewReader(mixedCase))
		if err != nil {
			t.Fatalf("a document with mixed case methods must be accepted, got error: %v", err)
		}
		want := []string{http.MethodGet, http.MethodPut, http.MethodHead}
		if !reflect.DeepEqual(mixedCfg.CORSRules[0].AllowedMethod, want) {
			t.Fatalf("expected methods %v, got %v", want, mixedCfg.CORSRules[0].AllowedMethod)
		}
	})

	t.Run("namespaceIsPreservedWhenPresent", func(t *testing.T) {
		if cfg.XMLNS != s3CORSNamespace {
			t.Errorf("expected XMLNS %q, got %q", s3CORSNamespace, cfg.XMLNS)
		}
		if cfg.XMLName.Local != "CORSConfiguration" {
			t.Errorf("expected root element %q, got %q", "CORSConfiguration", cfg.XMLName.Local)
		}
		if cfg.XMLName.Space != s3CORSNamespace {
			t.Errorf("expected root element namespace %q, got %q", s3CORSNamespace, cfg.XMLName.Space)
		}
	})

	t.Run("namespaceIsDefaultedWhenAbsent", func(t *testing.T) {
		bare := `<CORSConfiguration>` + corsTestRule(corsMinimalRuleBody) + `</CORSConfiguration>`
		bareCfg, err := validateBucketCorsConfig(strings.NewReader(bare))
		if err != nil {
			t.Fatalf("a document without an xmlns attribute must be accepted, got error: %v", err)
		}
		if bareCfg.XMLNS != s3CORSNamespace {
			t.Fatalf("expected the namespace to default to %q, got %q", s3CORSNamespace, bareCfg.XMLNS)
		}
	})

	t.Run("remarshalIsByteIdentical", func(t *testing.T) {
		got, err := cfg.ToXML()
		if err != nil {
			t.Fatalf("marshaling the parsed configuration must succeed, got error: %v", err)
		}
		if !strings.HasPrefix(string(got), xml.Header) {
			t.Errorf("expected the marshaled document to start with the XML header, got %q", truncateForError(string(got)))
		}
		if !strings.Contains(string(got), `<CORSConfiguration xmlns="`+s3CORSNamespace+`">`) {
			t.Errorf("expected a CORSConfiguration root carrying the S3 namespace, got %q", truncateForError(string(got)))
		}
		if string(got) != corsCanonicalDocument {
			t.Errorf("expected the round trip to be byte identical\n got: %s\nwant: %s", got, corsCanonicalDocument)
		}
	})
}

// TestValidateBucketCorsConfigCanonicalSizeCeiling covers the document that
// fits the ceiling as it arrives but not as it is stored.
//
// What is persisted is the canonical re-marshaling of what was received, and it
// is longer whenever the client omits the xmlns attribute, because the canonical
// form declares the S3 namespace. The parser that reads the stored document back
// stops at exactly maxBucketCORSConfigSize bytes, so a canonical document past
// the ceiling would be written and then fail to decode - which surfaces to the
// client as an internal error rather than as the oversized input it is.
//
// The boundary is computed rather than hard coded, so the test stays exact if the
// canonical form ever gains or loses a byte.
func TestValidateBucketCorsConfigCanonicalSizeCeiling(t *testing.T) {
	// A document without an xmlns attribute whose total length is driven purely
	// by the length of its ID element.
	namespacelessDoc := func(total int) string {
		document := `<CORSConfiguration><CORSRule><ID></ID>` + corsMinimalRuleBody + `</CORSRule></CORSConfiguration>`
		return strings.Replace(document, `<ID></ID>`,
			`<ID>`+strings.Repeat("p", total-len(document))+`</ID>`, 1)
	}

	// How many bytes the canonical form adds, measured on a document short
	// enough that neither length is anywhere near the ceiling.
	probe := namespacelessDoc(256)
	probeCfg, err := validateBucketCorsConfig(strings.NewReader(probe))
	if err != nil {
		t.Fatalf("the probe document must be accepted, got error: %v", err)
	}
	probeCanonical, err := xml.Marshal(probeCfg)
	if err != nil {
		t.Fatalf("marshaling the probe configuration must succeed, got error: %v", err)
	}
	growth := len(probeCanonical) - len(probe)
	if growth <= 0 {
		t.Fatalf("expected the canonical form of a namespace-less document to be longer, grew by %d bytes", growth)
	}

	t.Run("aBodyWhoseCanonicalFormExactlyFitsIsAccepted", func(t *testing.T) {
		document := namespacelessDoc(maxBucketCORSConfigSize - growth)
		cfg, err := validateBucketCorsConfig(strings.NewReader(document))
		if err != nil {
			t.Fatalf("expected the document to be accepted, got error: %v", err)
		}
		canonical, err := xml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshaling the validated configuration must succeed, got error: %v", err)
		}
		if len(canonical) != maxBucketCORSConfigSize {
			t.Fatalf("expected a canonical document of exactly %d bytes, got %d",
				maxBucketCORSConfigSize, len(canonical))
		}
		// The stored document has to decode again, which is the invariant this
		// whole ceiling exists to protect.
		if _, err := miniogocors.ParseBucketCorsConfig(bytes.NewReader(canonical)); err != nil {
			t.Fatalf("expected the canonical document to decode, got error: %v", err)
		}
	})

	t.Run("aBodyThatFitsButWhoseCanonicalFormDoesNotIsRejected", func(t *testing.T) {
		// One byte longer than the case above, so the body is still within the
		// ceiling while the document that would be stored is one byte over it.
		document := namespacelessDoc(maxBucketCORSConfigSize - growth + 1)
		if len(document) > maxBucketCORSConfigSize {
			t.Fatalf("the body itself must remain within the ceiling, got %d bytes", len(document))
		}
		cfg, err := validateBucketCorsConfig(strings.NewReader(document))
		if err == nil {
			t.Fatalf("expected the document to be rejected, got a configuration with %d rules", len(cfg.CORSRules))
		}
		if cfg != nil {
			t.Fatal("expected a nil configuration on failure")
		}
		if !strings.Contains(err.Error(), "once stored in its canonical form") {
			t.Fatalf("expected the error to name the canonical form, got %q", err)
		}
	})

	t.Run("aBodyAtTheCeilingWithTheNamespaceDeclaredIsStillAccepted", func(t *testing.T) {
		// Declaring the namespace is what keeps the canonical form from growing,
		// so the plain ceiling still holds for a document that declares it.
		document := corsTestDoc(corsTestRule(`<ID></ID>` + corsMinimalRuleBody))
		document = strings.Replace(document, `<ID></ID>`,
			`<ID>`+strings.Repeat("p", maxBucketCORSConfigSize-len(document))+`</ID>`, 1)
		if len(document) != maxBucketCORSConfigSize {
			t.Fatalf("expected a body of exactly %d bytes, got %d", maxBucketCORSConfigSize, len(document))
		}
		if _, err := validateBucketCorsConfig(strings.NewReader(document)); err != nil {
			t.Fatalf("expected the document to be accepted, got error: %v", err)
		}
	})
}

// TestValidateBucketCorsConfigNormalizesValues covers the hand-authored
// document: one saved with a byte order mark and indented so that every value
// sits on a line of its own.
//
// Accepting it is not enough - it has to mean what it says. An untrimmed origin
// is one no browser can ever send, so the values reaching the configuration, the
// document that would be persisted, and the preflight the rule was written for
// are all asserted.
func TestValidateBucketCorsConfigNormalizesValues(t *testing.T) {
	// Written the way an editor that emits a byte order mark would save it.
	document := utf8BOM + xml.Header +
		"<CORSConfiguration>\n" +
		"  <CORSRule>\n" +
		"    <ID>\n      hand written\n    </ID>\n" +
		"    <AllowedOrigin>\n      https://app.example.com\n    </AllowedOrigin>\n" +
		"    <AllowedOrigin>\thttps://admin.example.com\t</AllowedOrigin>\n" +
		"    <AllowedMethod>\n      GET\n    </AllowedMethod>\n" +
		"    <AllowedMethod>  put  </AllowedMethod>\n" +
		"    <AllowedHeader>\n      x-amz-acl\n    </AllowedHeader>\n" +
		"    <ExposeHeader>\n      ETag\n    </ExposeHeader>\n" +
		"    <MaxAgeSeconds> 3000 </MaxAgeSeconds>\n" +
		"  </CORSRule>\n" +
		"</CORSConfiguration>\n"

	cfg, err := validateBucketCorsConfig(strings.NewReader(document))
	if err != nil {
		t.Fatalf("a hand-authored document must be accepted, got error: %v", err)
	}
	if len(cfg.CORSRules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.CORSRules))
	}

	want := miniogocors.Rule{
		AllowedHeader: []string{"x-amz-acl"},
		AllowedMethod: []string{http.MethodGet, http.MethodPut},
		AllowedOrigin: []string{"https://app.example.com", "https://admin.example.com"},
		ExposeHeader:  []string{"ETag"},
		ID:            "hand written",
		MaxAgeSeconds: 3000,
	}

	t.Run("everyValueIsTrimmed", func(t *testing.T) {
		if !reflect.DeepEqual(cfg.CORSRules[0], want) {
			t.Fatalf("expected rule %+v, got %+v", want, cfg.CORSRules[0])
		}
	})

	t.Run("theRuleMatchesThePreflightItWasWrittenFor", func(t *testing.T) {
		rule := mustCORSRuleFor(t, cfg, "https://app.example.com", http.MethodGet, []string{"x-amz-acl"})
		if rule == nil {
			t.Fatal("expected the trimmed rule to match the preflight request it allows")
		}
		if rule.MaxAgeSeconds != want.MaxAgeSeconds {
			t.Errorf("expected MaxAgeSeconds %d, got %d", want.MaxAgeSeconds, rule.MaxAgeSeconds)
		}
		if second := mustCORSRuleFor(t, cfg, "https://admin.example.com", http.MethodPut, nil); second == nil {
			t.Error("expected the second trimmed origin and method to match as well")
		}
		if denied := mustCORSRuleFor(t, cfg, "https://evil.example.com", http.MethodGet, nil); denied != nil {
			t.Error("expected an origin the document does not name to match no rule")
		}
	})

	t.Run("thePersistedDocumentCarriesTheTrimmedValues", func(t *testing.T) {
		// PutBucketCorsHandler marshals exactly this configuration, so what is
		// asserted here is what a subsequent GetBucketCors returns.
		configData, err := xml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshaling the validated configuration must succeed, got error: %v", err)
		}
		got := string(configData)
		for _, fragment := range []string{
			`<ID>hand written</ID>`,
			`<AllowedOrigin>https://app.example.com</AllowedOrigin>`,
			`<AllowedOrigin>https://admin.example.com</AllowedOrigin>`,
			`<AllowedMethod>GET</AllowedMethod>`,
			`<AllowedMethod>PUT</AllowedMethod>`,
			`<AllowedHeader>x-amz-acl</AllowedHeader>`,
			`<ExposeHeader>ETag</ExposeHeader>`,
			`<MaxAgeSeconds>3000</MaxAgeSeconds>`,
			`<CORSConfiguration xmlns="` + s3CORSNamespace + `">`,
		} {
			if !strings.Contains(got, fragment) {
				t.Errorf("expected the persisted document to contain %s, got %s", fragment, truncateForError(got))
			}
		}
		// Re-reading what would be stored has to produce the same
		// configuration, so persistence cannot reintroduce what was trimmed.
		reparsed, err := validateBucketCorsConfig(bytes.NewReader(configData))
		if err != nil {
			t.Fatalf("the persisted document must validate, got error: %v", err)
		}
		if !reflect.DeepEqual(reparsed.CORSRules, cfg.CORSRules) {
			t.Fatalf("expected the persisted document to round trip, got %+v", reparsed.CORSRules)
		}
	})

	t.Run("theByteOrderMarkIsNotStored", func(t *testing.T) {
		configData, err := xml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshaling the validated configuration must succeed, got error: %v", err)
		}
		if bytes.Contains(configData, []byte(utf8BOM)) {
			t.Fatal("expected the byte order mark not to survive into the persisted document")
		}
		if cfg.XMLName.Local != corsConfigurationElement {
			t.Fatalf("expected root element %q, got %q", corsConfigurationElement, cfg.XMLName.Local)
		}
	})
}

// TestCorsSupportedMethods pins the exact set of methods that may appear in an
// AllowedMethod element. OPTIONS is the preflight method itself and must never
// be configurable, and the deterministic slice the tables iterate must stay in
// step with the map the validator consults.
func TestCorsSupportedMethods(t *testing.T) {
	if len(supportedCORSMethods) != len(allSupportedCORSMethods) {
		t.Fatalf("expected %d supported methods, got %d: %v",
			len(allSupportedCORSMethods), len(supportedCORSMethods), supportedCORSMethods)
	}
	for _, method := range allSupportedCORSMethods {
		if _, ok := supportedCORSMethods[method]; !ok {
			t.Errorf("expected %s to be an accepted AllowedMethod", method)
		}
	}
	for _, method := range []string{
		http.MethodOptions, http.MethodPatch, http.MethodConnect, http.MethodTrace,
	} {
		if _, ok := supportedCORSMethods[method]; ok {
			t.Errorf("expected %s not to be an accepted AllowedMethod", method)
		}
	}
}

// TestCorsConfigLimits pins the persisted document name and the two limits the
// S3 CORS contract fixes. The size ceiling is asserted as a lower bound because
// AWS caps a CORS document at 64 KiB: a smaller ceiling here would truncate, and
// therefore reject, a document a real client is entitled to send.
func TestCorsConfigLimits(t *testing.T) {
	if bucketCORSConfig != "cors.xml" {
		t.Errorf("expected the CORS configuration file to be %q, got %q", "cors.xml", bucketCORSConfig)
	}
	if maxBucketCORSRules != 100 {
		t.Errorf("expected at most %d rules, got %d", 100, maxBucketCORSRules)
	}
	const awsMaxCORSDocumentSize = 64 * 1024
	if maxBucketCORSConfigSize < awsMaxCORSDocumentSize {
		t.Errorf("expected the body ceiling to be at least the AWS document limit of %d bytes, got %d",
			awsMaxCORSDocumentSize, maxBucketCORSConfigSize)
	}
	// The namespace the validator enforces is the one every S3 client sends and
	// expects back, so it is pinned against a literal rather than against itself.
	if corsConfigXMLNS != s3CORSNamespace {
		t.Errorf("expected the enforced namespace to be %q, got %q", s3CORSNamespace, corsConfigXMLNS)
	}
	if corsConfigurationElement != "CORSConfiguration" {
		t.Errorf("expected the root element to be %q, got %q", "CORSConfiguration", corsConfigurationElement)
	}
	if corsRuleElement != "CORSRule" {
		t.Errorf("expected the rule element to be %q, got %q", "CORSRule", corsRuleElement)
	}
	// The child elements of a CORSRule are exactly those AWS defines, and only
	// ID and MaxAgeSeconds are single valued.
	wantRuleElements := map[string]bool{
		"AllowedHeader": false,
		"AllowedMethod": false,
		"AllowedOrigin": false,
		"ExposeHeader":  false,
		"ID":            true,
		"MaxAgeSeconds": true,
	}
	if !reflect.DeepEqual(corsRuleElements, wantRuleElements) {
		t.Errorf("expected the CORSRule child elements %v, got %v", wantRuleElements, corsRuleElements)
	}
}

// TestCorsWildcardMatch pins the pattern language S3 defines for AllowedOrigin
// and AllowedHeader: literal text plus at most one "*". Two properties matter as
// much as the matches themselves. A "?" must be literal, because honoring it
// would grant an origin the bucket owner never configured, and the match must be
// non-recursive, because a preflight reaches this code before any authentication.
func TestCorsWildcardMatch(t *testing.T) {
	testCases := []struct {
		pattern string
		name    string
		want    bool
	}{
		// No wildcard: the pattern matches only itself.
		{pattern: "https://www.example1.com", name: "https://www.example1.com", want: true},
		{pattern: "https://www.example1.com", name: "https://www.example2.com", want: false},
		{pattern: "https://www.example1.com", name: "https://www.example1.com.evil.net", want: false},
		{pattern: "https://www.example1.com", name: "", want: false},
		{pattern: "", name: "", want: true},
		{pattern: "", name: "x", want: false},

		// The bare wildcard covers everything.
		{pattern: "*", name: "https://www.example1.com", want: true},
		{pattern: "*", name: "", want: true},

		// A trailing wildcard is a prefix match.
		{pattern: "http://www.example2.*", name: "http://www.example2.com", want: true},
		{pattern: "http://www.example2.*", name: "http://www.example2.", want: true},
		{pattern: "http://www.example2.*", name: "http://www.example3.com", want: false},
		{pattern: "http://www.example2.*", name: "http://www.example2", want: false},

		// A leading wildcard is a suffix match.
		{pattern: "*.example.com", name: "https://a.example.com", want: true},
		{pattern: "*.example.com", name: "https://a.example.com.evil.net", want: false},

		// An embedded wildcard must match both sides, and the prefix and the
		// suffix may not overlap on the same characters.
		{pattern: "https://*.example.com", name: "https://a.example.com", want: true},
		{pattern: "https://*.example.com", name: "http://a.example.com", want: false},
		{pattern: "ab*cd", name: "abcd", want: true},
		{pattern: "ab*cd", name: "abXcd", want: true},
		{pattern: "ab*cd", name: "abc", want: false},
		{pattern: "a*a", name: "a", want: false},
		{pattern: "a*a", name: "aa", want: true},

		// "?" carries no special meaning: it matches only itself.
		{pattern: "https://www.example?.com", name: "https://www.example1.com", want: false},
		{pattern: "https://www.example?.com", name: "https://www.example?.com", want: true},
		{pattern: "https://www.example1.com?", name: "https://www.example1.com", want: false},
		{pattern: "http://ex?mple.*", name: "http://example.com", want: false},
		{pattern: "http://ex?mple.*", name: "http://ex?mple.com", want: true},

		// A pattern with more than one wildcard cannot be persisted, and if one
		// arrives anyway every wildcard after the first is literal - which can
		// only ever narrow the match, never widen it.
		{pattern: "http://*.example.*", name: "http://a.example.com", want: false},
		{pattern: "http://*.example.*", name: "http://a.example.*", want: true},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%q~%q", tc.pattern, tc.name), func(t *testing.T) {
			if got := corsWildcardMatch(tc.pattern, tc.name); got != tc.want {
				t.Fatalf("corsWildcardMatch(%q, %q) = %v, expected %v", tc.pattern, tc.name, got, tc.want)
			}
		})
	}

	// A "*" followed by a suffix is the shape a recursive matcher backtracks
	// on. A long, adversarial name must therefore still be answered by two
	// bounded comparisons rather than by nested recursion.
	t.Run("longAdversarialNameIsAnsweredWithoutRecursion", func(t *testing.T) {
		const pattern = "http://*.example.com"
		name := "http://" + strings.Repeat("a.", 100000) + "example.com"
		if !corsWildcardMatch(pattern, name) {
			t.Fatalf("expected %q to match the suffix wildcard pattern %q", truncateForError(name), pattern)
		}
		if corsWildcardMatch(pattern, name+"x") {
			t.Fatalf("expected %q not to match the suffix wildcard pattern %q", truncateForError(name+"x"), pattern)
		}
	})
}

// TestGetBucketCORSURL pins the request target the test harness builds for the
// three CORS operations. The sub-resource has to be the exact lowercase "cors"
// query key with an empty value: that is what every S3 client sends, what the
// router's Queries("cors", "") matcher selects on, and the sub-resource name the
// V2 signature calculation already authorizes. The bucket the preflight
// evaluator resolves from that target must be the same one.
func TestGetBucketCORSURL(t *testing.T) {
	const (
		endPoint   = "http://127.0.0.1:9000"
		bucketName = "blitzy-cors-bucket"
	)

	got := getBucketCORSURL(endPoint, bucketName)
	want := endPoint + SlashSeparator + bucketName + SlashSeparator + "?cors="
	if got != want {
		t.Fatalf("expected the CORS sub-resource URL %q, got %q", want, got)
	}

	req := httptest.NewRequest(http.MethodOptions, got, nil)
	if bucket, object := request2BucketObjectName(req); bucket != bucketName || object != "" {
		t.Fatalf("expected the request to resolve to bucket %q with no object, got bucket %q and object %q",
			bucketName, bucket, object)
	}
}

// truncateForError shortens a document for inclusion in a failure message, so a
// large configuration cannot flood the test output.
func truncateForError(s string) string {
	const limit = 256
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}

// TestCorsRuleFor exercises every branch of the preflight rule matcher: the
// three predicates it evaluates - origin, requested method and requested headers
// - and the first-matching-rule-wins ordering that S3 mandates.
//
// Every rule carries a distinct ID, so an assertion can name exactly which rule
// the matcher selected. That is what makes the ordering guarantee provable
// rather than merely plausible.
func TestCorsRuleFor(t *testing.T) {
	type matchCase struct {
		name       string
		cfg        *miniogocors.Config
		origin     string
		method     string
		reqHeaders []string
		// wantRuleID is the ID of the rule that must match. An empty value means
		// no rule may match, which is how a preflight is denied.
		wantRuleID string
	}

	firstRule := miniogocors.Rule{
		ID:            "first",
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"https://www.example1.com"},
		ExposeHeader:  []string{"ETag"},
		MaxAgeSeconds: 100,
	}
	secondRule := miniogocors.Rule{
		ID:            "second",
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"https://www.example1.com"},
		ExposeHeader:  []string{"x-amz-request-id"},
		MaxAgeSeconds: 200,
	}

	allMethodsRule := miniogocors.Rule{
		ID:            "all-methods",
		AllowedMethod: slices.Clone(allSupportedCORSMethods),
		AllowedOrigin: []string{"https://www.example1.com"},
	}

	testCases := []matchCase{
		{
			name:       "firstMatchWins/firstRuleInDocumentOrderIsSelected",
			cfg:        corsTestConfig(firstRule, secondRule),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			wantRuleID: "first",
		},
		{
			name:       "firstMatchWins/reversingTheOrderFlipsTheAnswer",
			cfg:        corsTestConfig(secondRule, firstRule),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			wantRuleID: "second",
		},
		{
			name: "firstMatchWins/earlierRuleRejectedOnOriginIsSkipped",
			cfg: corsTestConfig(
				miniogocors.Rule{
					ID:            "other-origin",
					AllowedMethod: []string{http.MethodPut},
					AllowedOrigin: []string{"https://www.example9.com"},
				},
				firstRule,
			),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			wantRuleID: "first",
		},
		{
			name: "firstMatchWins/earlierRuleRejectedOnMethodIsSkipped",
			cfg: corsTestConfig(
				miniogocors.Rule{
					ID:            "other-method",
					AllowedMethod: []string{http.MethodGet},
					AllowedOrigin: []string{"https://www.example1.com"},
				},
				firstRule,
			),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			wantRuleID: "first",
		},
		{
			name: "firstMatchWins/earlierRuleRejectedOnHeadersIsSkipped",
			cfg: corsTestConfig(
				miniogocors.Rule{
					ID:            "no-headers",
					AllowedMethod: []string{http.MethodPut},
					AllowedOrigin: []string{"https://www.example1.com"},
				},
				miniogocors.Rule{
					ID:            "wildcard-headers",
					AllowedHeader: []string{"*"},
					AllowedMethod: []string{http.MethodPut},
					AllowedOrigin: []string{"https://www.example1.com"},
				},
			),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo"},
			wantRuleID: "wildcard-headers",
		},

		{
			name: "origin/exactMatch",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "exact-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodGet,
			wantRuleID: "exact-origin",
		},
		{
			name: "origin/mismatchDeniesTheRequest",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "exact-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin: "https://evil.example.net",
			method: http.MethodGet,
		},
		{
			name: "origin/exactMatchIsCaseSensitive",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "exact-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin: "https://WWW.EXAMPLE1.COM",
			method: http.MethodGet,
		},
		{
			name: "origin/secondAllowedOriginOfTheSameRuleMatches",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "two-origins",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com", "https://www.example4.com"},
			}),
			origin:     "https://www.example4.com",
			method:     http.MethodGet,
			wantRuleID: "two-origins",
		},
		{
			name: "origin/bareWildcardMatchesAnyOrigin",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "any-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"*"},
			}),
			origin:     "https://anything.example.test",
			method:     http.MethodGet,
			wantRuleID: "any-origin",
		},
		{
			name: "origin/suffixWildcardMatchesTheSameHostPrefix",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "suffix-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"http://www.example2.*"},
			}),
			origin:     "http://www.example2.com",
			method:     http.MethodGet,
			wantRuleID: "suffix-origin",
		},
		{
			name: "origin/suffixWildcardDoesNotMatchADifferentHost",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "suffix-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"http://www.example2.*"},
			}),
			origin: "http://www.example3.com",
			method: http.MethodGet,
		},
		{
			// "?" is not a wildcard in the S3 pattern language. Treating it as
			// one would allow an origin the bucket owner never configured.
			name: "origin/questionMarkInAllowedOriginIsLiteral",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "question-mark-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example?.com"},
			}),
			origin: "https://www.example1.com",
			method: http.MethodGet,
		},
		{
			name: "origin/questionMarkInAllowedOriginMatchesItself",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "question-mark-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example?.com"},
			}),
			origin:     "https://www.example?.com",
			method:     http.MethodGet,
			wantRuleID: "question-mark-origin",
		},
		{
			name: "origin/wildcardDoesNotSpanAnOverlappingPrefixAndSuffix",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "embedded-origin",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://*.example.com"},
			}),
			origin: "https://example.com",
			method: http.MethodGet,
		},

		{
			name: "method/mismatchDeniesTheRequest",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "read-only",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin: "https://www.example1.com",
			method: http.MethodPut,
		},
		{
			name: "method/secondAllowedMethodOfTheSameRuleMatches",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "read-write",
				AllowedMethod: []string{http.MethodGet, http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			wantRuleID: "read-write",
		},
		{
			name: "method/comparisonIsCaseInsensitive",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "read-only",
				AllowedMethod: []string{http.MethodGet},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     "get",
			wantRuleID: "read-only",
		},

		{
			name: "headers/everyRequestedHeaderCovered",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "explicit-headers",
				AllowedHeader: []string{"x-amz-meta-foo", "content-type"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo", "content-type"},
			wantRuleID: "explicit-headers",
		},
		{
			name: "headers/caseInsensitiveRuleUpperRequestLower",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "upper-headers",
				AllowedHeader: []string{"X-Amz-Meta-Foo"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo"},
			wantRuleID: "upper-headers",
		},
		{
			name: "headers/caseInsensitiveRuleLowerRequestUpper",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "lower-headers",
				AllowedHeader: []string{"x-amz-meta-foo"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"X-Amz-Meta-Foo"},
			wantRuleID: "lower-headers",
		},
		{
			name: "headers/bareWildcardCoversAnyRequestedHeader",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "wildcard-headers",
				AllowedHeader: []string{"*"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo", "authorization", "content-md5"},
			wantRuleID: "wildcard-headers",
		},
		{
			name: "headers/prefixWildcardCoversMatchingHeaders",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "prefix-headers",
				AllowedHeader: []string{"x-amz-*"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"X-Amz-Meta-Foo", "x-amz-acl"},
			wantRuleID: "prefix-headers",
		},
		{
			name: "headers/prefixWildcardDoesNotCoverOtherHeaders",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "prefix-headers",
				AllowedHeader: []string{"x-amz-*"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"content-type"},
		},
		{
			// "?" is literal for AllowedHeader too, so a rule naming it does
			// not cover a header that merely differs in that one position.
			name: "headers/questionMarkInAllowedHeaderIsLiteral",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "question-mark-headers",
				AllowedHeader: []string{"x-am?-acl"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-acl"},
		},
		{
			name: "headers/questionMarkInAllowedHeaderMatchesItselfCaseInsensitively",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "question-mark-headers",
				AllowedHeader: []string{"x-am?-acl"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"X-Am?-Acl"},
			wantRuleID: "question-mark-headers",
		},
		{
			name: "headers/partialCoverageDeniesTheRequest",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "one-header",
				AllowedHeader: []string{"x-amz-meta-foo"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo", "x-amz-meta-bar"},
		},
		{
			name: "headers/noneRequestedIsTriviallyAllowed",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "explicit-headers",
				AllowedHeader: []string{"x-amz-meta-foo"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: nil,
			wantRuleID: "explicit-headers",
		},
		{
			name: "headers/emptySliceRequestedIsTriviallyAllowed",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "no-headers",
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{},
			wantRuleID: "no-headers",
		},
		{
			name: "headers/ruleWithoutAllowedHeaderCannotCoverARequestedHeader",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "no-headers",
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-amz-meta-foo"},
		},
		{
			name: "headers/multiValueUnionFullyCovered",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "union-headers",
				AllowedHeader: []string{"x-a", "X-B"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-a", "x-b"},
			wantRuleID: "union-headers",
		},
		{
			name: "headers/multiValueUnionPartiallyCovered",
			cfg: corsTestConfig(miniogocors.Rule{
				ID:            "union-headers",
				AllowedHeader: []string{"x-a"},
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"https://www.example1.com"},
			}),
			origin:     "https://www.example1.com",
			method:     http.MethodPut,
			reqHeaders: []string{"x-a", "x-b"},
		},

		{
			name:   "config/nilConfigurationMatchesNothing",
			cfg:    nil,
			origin: "https://www.example1.com",
			method: http.MethodGet,
		},
		{
			name:   "config/zeroRulesMatchesNothing",
			cfg:    corsTestConfig(),
			origin: "https://www.example1.com",
			method: http.MethodGet,
		},
		{
			name:   "config/emptyOriginMatchesNothingUnderAWildcardRule",
			cfg:    corsTestConfig(allMethodsRule),
			origin: "",
			method: http.MethodGet,
		},
	}

	for _, method := range allSupportedCORSMethods {
		testCases = append(testCases, matchCase{
			name:       "method/allows" + method,
			cfg:        corsTestConfig(allMethodsRule),
			origin:     "https://www.example1.com",
			method:     method,
			wantRuleID: "all-methods",
		})
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rule := mustCORSRuleFor(t, tc.cfg, tc.origin, tc.method, tc.reqHeaders)

			if tc.wantRuleID == "" {
				if rule != nil {
					t.Fatalf("expected no rule to match, got rule %q", rule.ID)
				}
				return
			}

			if rule == nil {
				t.Fatalf("expected rule %q to match, got no match", tc.wantRuleID)
			}
			if rule.ID != tc.wantRuleID {
				t.Fatalf("expected rule %q to match, got rule %q", tc.wantRuleID, rule.ID)
			}

			// The middleware builds the preflight response from the returned
			// rule, so the matcher must hand back a pointer into the supplied
			// configuration rather than a copy of it.
			matched := false
			for i := range tc.cfg.CORSRules {
				if &tc.cfg.CORSRules[i] == rule {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatal("expected the matched rule to point into the supplied configuration")
			}
		})
	}
}

// TestCorsRequestHeadersUnion covers the flattening of Access-Control-Request-
// Headers. The Fetch standard guarantees at most one such field, but a gateway
// may split it into repeated fields, so the matcher must be fed the trimmed union
// of every value rather than just the first one.
func TestCorsRequestHeadersUnion(t *testing.T) {
	testCases := []struct {
		name   string
		values []string
		want   []string
	}{
		{
			name:   "noHeaderFieldAtAll",
			values: nil,
			want:   nil,
		},
		{
			name:   "emptyHeaderFieldYieldsNothing",
			values: []string{""},
			want:   nil,
		},
		{
			name:   "singleHeader",
			values: []string{"x-amz-meta-foo"},
			want:   []string{"x-amz-meta-foo"},
		},
		{
			name:   "commaSeparatedValueIsSplitAndTrimmed",
			values: []string{"x-a, x-b ,  x-c"},
			want:   []string{"x-a", "x-b", "x-c"},
		},
		{
			name:   "repeatedFieldsAreUnioned",
			values: []string{"x-a", "x-b"},
			want:   []string{"x-a", "x-b"},
		},
		{
			name:   "repeatedFieldsEachCarryingAListAreUnioned",
			values: []string{"x-a, x-b", "x-c ,x-d"},
			want:   []string{"x-a", "x-b", "x-c", "x-d"},
		},
		{
			name:   "blankEntriesAreDropped",
			values: []string{"", " , ", "x-a", ",,"},
			want:   []string{"x-a"},
		},
		{
			name:   "surroundingWhitespaceIsTrimmed",
			values: []string{"\tx-a \r\n"},
			want:   []string{"x-a"},
		},
		{
			name:   "casingIsPreservedForTheEchoedResponseHeader",
			values: []string{"X-Amz-Meta-Foo, Content-Type"},
			want:   []string{"X-Amz-Meta-Foo", "Content-Type"},
		},
		{
			name:   "duplicateNamesAreCollapsed",
			values: []string{"x-a, x-a, x-b, x-a"},
			want:   []string{"x-a", "x-b"},
		},
		{
			// Header names are case-insensitive, so these are one header. The
			// first casing seen is the one the response echoes back.
			name:   "duplicateNamesDifferingOnlyInCaseAreCollapsed",
			values: []string{"X-Amz-Meta-Foo, x-amz-meta-foo, X-AMZ-META-FOO"},
			want:   []string{"X-Amz-Meta-Foo"},
		},
		{
			name:   "duplicatesAcrossRepeatedFieldsAreCollapsed",
			values: []string{"x-a, x-b", "X-A", "x-c ,x-b"},
			want:   []string{"x-a", "x-b", "x-c"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			for _, value := range tc.values {
				header.Add("Access-Control-Request-Headers", value)
			}

			got := parseCORSRequestHeaders(header)
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d requested headers %v, got %d %v", len(tc.want), tc.want, len(got), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("requested header %d: expected %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}

	// A long list is a list like any other: S3 puts no ceiling on how many
	// headers a preflight may ask about, and a rule only allows a request when
	// it covers every one of them, so every name has to survive parsing to be
	// compared against the rules. Duplicates still collapse, however many there
	// are, because a name repeated in any casing is one requested header.
	t.Run("aLongRequestedHeaderListIsReturnedInFull", func(t *testing.T) {
		const count = 512

		names := make([]string, 0, count)
		for i := range count {
			names = append(names, fmt.Sprintf("x-blitzy-%d", i))
		}
		header := http.Header{}
		header.Set("Access-Control-Request-Headers", strings.Join(names, ","))

		got := parseCORSRequestHeaders(header)
		if len(got) != count {
			t.Fatalf("expected all %d requested headers, got %d", count, len(got))
		}
		for i := range names {
			if got[i] != names[i] {
				t.Fatalf("requested header %d: expected %q, got %q", i, names[i], got[i])
			}
		}

		repeated := http.Header{}
		repeated.Set("Access-Control-Request-Headers",
			strings.TrimSuffix(strings.Repeat("X-A,x-a,", count), ","))
		if got := parseCORSRequestHeaders(repeated); len(got) != 1 || got[0] != "X-A" {
			t.Fatalf("expected the repeated name to collapse to [X-A], got %v", got)
		}
	})

	t.Run("unionIsEvaluatedAsAWholeByTheMatcher", func(t *testing.T) {
		header := http.Header{}
		header.Add("Access-Control-Request-Headers", "x-a, x-b")
		header.Add("Access-Control-Request-Headers", "x-c")
		reqHeaders := parseCORSRequestHeaders(header)

		const origin = "https://www.example1.com"

		covering := corsTestConfig(miniogocors.Rule{
			ID:            "covers-every-header",
			AllowedHeader: []string{"x-a", "X-B", "x-c"},
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})
		if rule := mustCORSRuleFor(t, covering, origin, http.MethodPut, reqHeaders); rule == nil {
			t.Errorf("expected the rule covering every header of %v to match", reqHeaders)
		}

		partial := corsTestConfig(miniogocors.Rule{
			ID:            "covers-some-headers",
			AllowedHeader: []string{"x-a", "x-b"},
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})
		if rule := mustCORSRuleFor(t, partial, origin, http.MethodPut, reqHeaders); rule != nil {
			t.Errorf("expected no match when %v is only partially covered, got rule %q", reqHeaders, rule.ID)
		}
	})

	// The number of requested headers changes nothing about how they are
	// evaluated: every one of a long list is compared against the rule, whether
	// the rule covers them through a wildcard or by naming each of them, and a
	// single header left uncovered still refuses the request. A matcher that
	// stopped short of the whole list could not tell these three cases apart.
	t.Run("aLongRequestedHeaderListIsEvaluatedInFull", func(t *testing.T) {
		const (
			count  = 512
			origin = "https://www.example1.com"
		)

		reqHeaders := make([]string, 0, count)
		for i := range count {
			reqHeaders = append(reqHeaders, fmt.Sprintf("X-Blitzy-%d", i))
		}

		rule := func(id string, allowedHeaders []string) *miniogocors.Config {
			return corsTestConfig(miniogocors.Rule{
				ID:            id,
				AllowedHeader: allowedHeaders,
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{origin},
			})
		}

		wildcard := rule("wildcard", []string{"x-blitzy-*"})
		if matched := mustCORSRuleFor(t, wildcard, origin, http.MethodPut, reqHeaders); matched == nil {
			t.Errorf("expected a wildcard AllowedHeader to cover all %d requested headers", count)
		}

		enumerated := rule("enumerated", slices.Clone(reqHeaders))
		if matched := mustCORSRuleFor(t, enumerated, origin, http.MethodPut, reqHeaders); matched == nil {
			t.Errorf("expected %d enumerated AllowedHeader values to cover all %d requested headers", count, count)
		}

		short := rule("one-short", slices.Clone(reqHeaders[:count-1]))
		if matched := mustCORSRuleFor(t, short, origin, http.MethodPut, reqHeaders); matched != nil {
			t.Errorf("expected no match when the last of %d requested headers is uncovered, got rule %q",
				count, matched.ID)
		}
	})
}

// mustCORSRuleFor evaluates a preflight against a configuration and fails the
// test if the evaluation could not be completed.
//
// Every ordinary matcher assertion goes through it, so each one also asserts that
// the evaluation budget was not a factor: a configuration and a header list of
// the shapes real clients and real bucket owners produce must always be evaluated
// in full, and a bound that refused any of them would be a bound set too low. The
// cases that deliberately exceed it call corsRuleFor directly.
func mustCORSRuleFor(t *testing.T, cfg *miniogocors.Config, origin, method string, reqHeaders []string) *miniogocors.Rule {
	t.Helper()

	rule, err := corsRuleFor(cfg, origin, method, reqHeaders)
	if err != nil {
		t.Fatalf("evaluating %d requested header(s) against the configuration must complete, got error: %v",
			len(reqHeaders), err)
	}
	return rule
}

// TestCorsMatchBudget pins the accounting the bounded evaluation rests on.
//
// The allowance is spent across a whole evaluation rather than per rule, and
// exhaustion is sticky, because an evaluation that stopped comparing cannot be
// resumed into a verdict: whatever the remaining predicates would have answered,
// the rules were not fully consulted.
func TestCorsMatchBudget(t *testing.T) {
	t.Run("spendsDownToZeroAndThenRefuses", func(t *testing.T) {
		budget := &corsMatchBudget{remaining: 3}

		for i := range 3 {
			if !budget.spend(1) {
				t.Fatalf("expected comparison %d of 3 to be covered by the allowance", i)
			}
			if budget.exhausted {
				t.Fatalf("expected the allowance not to be exhausted after %d of 3 comparisons", i+1)
			}
		}
		if budget.spend(1) {
			t.Fatal("expected the comparison past the allowance to be refused")
		}
		if !budget.exhausted {
			t.Fatal("expected the refused comparison to mark the allowance exhausted")
		}
		if budget.remaining != 0 {
			t.Fatalf("expected no allowance to remain, got %d", budget.remaining)
		}
	})

	t.Run("exhaustionIsSticky", func(t *testing.T) {
		budget := &corsMatchBudget{remaining: 1}

		if budget.spend(2) {
			t.Fatal("expected a claim larger than the allowance to be refused")
		}
		if budget.spend(0) {
			t.Fatal("expected an exhausted allowance to refuse even a free claim")
		}
		if !budget.exhausted {
			t.Fatal("expected exhaustion to persist")
		}
	})

	// The production allowance has to sit far above what a real preflight costs
	// and far below the product it exists to bound, or it would either refuse
	// legitimate requests or fail to bound anything.
	t.Run("theProductionAllowanceLeavesRoomForRealRequests", func(t *testing.T) {
		// A generous real request: a browser asks about a handful of headers,
		// so thirty-two of them against a document packed with patterns is
		// already far beyond anything observed.
		const generousRealCost = 32 * 4096
		if maxCORSPreflightMatchCost <= generousRealCost {
			t.Fatalf("expected the allowance to exceed a generous real evaluation of %d comparisons, it is %d",
				generousRealCost, maxCORSPreflightMatchCost)
		}
		// The product the bound exists to cut down: the widest header list an
		// admitted request can name against the most patterns a stored document
		// can hold.
		const adversarialCost = 4096 * 4096
		if maxCORSPreflightMatchCost >= adversarialCost {
			t.Fatalf("expected the allowance to be well below the adversarial product of %d comparisons, it is %d",
				adversarialCost, maxCORSPreflightMatchCost)
		}
	})
}

// TestCorsRuleForBoundedWork pins that evaluating a preflight costs at most the
// allowance, whatever a bucket owner stored and whatever a client asks about.
//
// The two lists whose product decides the cost are chosen separately: the bucket
// owner fills a stored document with wildcard AllowedHeader patterns, which no
// index can flatten, and an unauthenticated client names as many headers as the
// server's header limit admits. Neither is illegitimate on its own, and nothing
// authenticates the request that pairs them, so the pairing has to be bounded or a
// single request buys millions of comparisons - about 16.8 million for the widest
// pairing exercised here.
func TestCorsRuleForBoundedWork(t *testing.T) {
	const origin = "https://www.example1.com"

	requested := func(count int) []string {
		names := make([]string, 0, count)
		for i := range count {
			names = append(names, fmt.Sprintf("x-amz-blitzy-%d", i))
		}
		return names
	}

	// The costliest AllowedHeader list a stored document can express: decoy
	// wildcard patterns that match nothing the request asks about, followed by one
	// that covers all of it.
	//
	// The trailing pattern is what makes the list expensive rather than merely
	// long. A rule stops being evaluated at the first requested header it does not
	// cover, so a list of decoys alone is abandoned after one header; a list whose
	// last entry covers every header instead forces each of them through every
	// decoy before matching. That is the product the allowance exists to bound,
	// and it is a document a bucket owner can legitimately store.
	patterns := func(decoys int) []string {
		values := make([]string, 0, decoys+1)
		for i := range decoys {
			values = append(values, fmt.Sprintf("x-never-%d-*", i))
		}
		return append(values, "x-amz-blitzy-*")
	}

	t.Run("anEvaluationWithinTheAllowanceCompletes", func(t *testing.T) {
		// 64 requested headers, each compared against 4096 decoys before the
		// covering pattern: roughly 262144 comparisons, a quarter of the
		// allowance. It is not a request a browser makes, yet it is answered
		// rather than refused, which is what keeps the bound from deciding
		// ordinary outcomes.
		cfg := corsTestConfig(miniogocors.Rule{
			ID:            "costly-but-affordable",
			AllowedHeader: patterns(4096),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})

		rule, err := corsRuleFor(cfg, origin, http.MethodPut, requested(64))
		if err != nil {
			t.Fatalf("expected an evaluation within the allowance to complete, got error: %v", err)
		}
		if rule == nil {
			t.Fatal("expected the covering pattern to allow the request")
		}
	})

	t.Run("anEvaluationBeyondTheAllowanceIsRefused", func(t *testing.T) {
		// 4096 decoys against 4096 requested headers is roughly sixteen million
		// comparisons, sixteen times the allowance - and the covering pattern
		// means every one of them is performed before a verdict is reached.
		cfg := corsTestConfig(miniogocors.Rule{
			ID:            "unaffordable",
			AllowedHeader: patterns(4096),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})

		rule, err := corsRuleFor(cfg, origin, http.MethodPut, requested(4096))
		if !errors.Is(err, errCORSPreflightEvaluationTooCostly) {
			t.Fatalf("expected %v, got %v", errCORSPreflightEvaluationTooCostly, err)
		}
		// Refusing is fail-closed: no rule is returned, so no allow header can
		// be produced from one - even though this rule would have matched had the
		// evaluation been allowed to finish.
		if rule != nil {
			t.Fatalf("expected a refused evaluation to return no rule, got %q", rule.ID)
		}
	})

	// The allowance is spent across the whole document rather than per rule, so
	// spreading the same patterns over the hundred rules a document may hold buys
	// no more work than packing them into one.
	t.Run("theAllowanceIsNotResetPerRule", func(t *testing.T) {
		const decoysPerRule = 4096 / maxBucketCORSRules

		rules := make([]miniogocors.Rule, 0, maxBucketCORSRules)
		for i := range maxBucketCORSRules {
			rules = append(rules, miniogocors.Rule{
				ID:            fmt.Sprintf("rule-%d", i),
				AllowedHeader: patterns(decoysPerRule),
				AllowedMethod: []string{http.MethodPut},
				// Only the last rule's origin matches, so every rule before it
				// is walked and the header comparisons of the matching one are
				// reached with the allowance already drawn down.
				AllowedOrigin: []string{"https://www.example9.com"},
			})
		}
		rules[len(rules)-1].AllowedOrigin = []string{origin}
		cfg := corsTestConfig(rules...)

		if _, err := corsRuleFor(cfg, origin, http.MethodPut, requested(8)); err != nil {
			t.Fatalf("expected a modest request against the spread document to complete, got error: %v", err)
		}

		// Twelve rules' worth of patterns is already enough to exceed the
		// allowance when paired with the widest admissible header list, and it
		// is refused whether they sit in one rule or in many.
		wide := make([]miniogocors.Rule, 0, 12)
		for i := range 12 {
			wide = append(wide, miniogocors.Rule{
				ID:            fmt.Sprintf("wide-%d", i),
				AllowedHeader: patterns(4096 / 12),
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{origin},
			})
		}
		if _, err := corsRuleFor(corsTestConfig(wide...), origin, http.MethodPut, requested(4096)); !errors.Is(err, errCORSPreflightEvaluationTooCostly) {
			t.Fatalf("expected patterns spread over rules to draw on the same allowance, got %v", err)
		}
	})

	// The refusal must not depend on reaching the last rule: an evaluation whose
	// very first rule is unaffordable is refused on that rule, rather than
	// walking the rest of the document reporting each as a non-match.
	t.Run("theFirstUnaffordableRuleEndsTheEvaluation", func(t *testing.T) {
		matching := miniogocors.Rule{
			ID:            "would-have-matched",
			AllowedHeader: []string{"*"},
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		}
		costly := miniogocors.Rule{
			ID:            "unaffordable",
			AllowedHeader: patterns(4096),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		}

		// The costly rule comes first, so first-matching-rule-wins reaches it
		// before the rule that would have allowed the request. The evaluation is
		// refused rather than falling through to that rule, because a document
		// this expensive cannot be evaluated in the order S3 requires.
		cfg := corsTestConfig(costly, matching)
		if _, err := corsRuleFor(cfg, origin, http.MethodPut, requested(4096)); !errors.Is(err, errCORSPreflightEvaluationTooCostly) {
			t.Fatalf("expected the unaffordable first rule to refuse the evaluation, got %v", err)
		}

		// With the matching rule first, the same request is allowed by it before
		// any of the expensive comparisons is reached, so document order decides
		// the cost as well as the outcome.
		cfg = corsTestConfig(matching, costly)
		rule, err := corsRuleFor(cfg, origin, http.MethodPut, requested(4096))
		if err != nil {
			t.Fatalf("expected a rule matching before the expensive one to complete, got error: %v", err)
		}
		if rule == nil || rule.ID != matching.ID {
			t.Fatalf("expected rule %q to match first, got %+v", matching.ID, rule)
		}
	})

	// A rule listing its allowed headers literally is indexed, so naming a great
	// many of them is cheap however many headers the request asks about. This is
	// the case the bound must not refuse, since it is the shape a large but
	// ordinary configuration takes.
	t.Run("literalAllowedHeadersStayAffordableInBulk", func(t *testing.T) {
		names := requested(4096)
		cfg := corsTestConfig(miniogocors.Rule{
			ID:            "enumerated",
			AllowedHeader: slices.Clone(names),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})

		rule, err := corsRuleFor(cfg, origin, http.MethodPut, names)
		if err != nil {
			t.Fatalf("expected an indexed rule to be affordable in bulk, got error: %v", err)
		}
		if rule == nil {
			t.Fatalf("expected the rule naming all %d requested headers to match", len(names))
		}
	})
}

// TestBucketCorsPreflightMiddlewareDelegation verifies non-OPTIONS,
// incomplete-preflight and root-path requests reach the wrapped global CORS
// handler without added CORS headers.
//
// That gate is what keeps the server-wide MINIO_API_CORS_ALLOW_ORIGIN handler in
// force for every OPTIONS request that is not a complete preflight, including the
// Origin-only shape TestCors asserts against.
func TestBucketCorsPreflightMiddlewareDelegation(t *testing.T) {
	const delegatedBody = "delegated"

	corsAllowHeaders := []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
		"Access-Control-Expose-Headers",
	}

	testCases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
	}{
		{
			name:   "plainGetIsDelegated",
			method: http.MethodGet,
			target: "/blitzy-cors-bucket/object",
		},
		{
			name:   "getCarryingPreflightHeadersIsDelegated",
			method: http.MethodGet,
			target: "/blitzy-cors-bucket",
			headers: map[string]string{
				"Origin":                        "https://www.example1.com",
				"Access-Control-Request-Method": http.MethodPut,
			},
		},
		{
			name:   "putCarryingPreflightHeadersIsDelegated",
			method: http.MethodPut,
			target: "/blitzy-cors-bucket/object",
			headers: map[string]string{
				"Origin":                        "https://www.example1.com",
				"Access-Control-Request-Method": http.MethodPut,
			},
		},
		{
			name:   "optionsWithoutRequestMethodIsDelegated",
			method: http.MethodOptions,
			target: "/blitzy-cors-bucket",
			headers: map[string]string{
				"Origin": "https://www.example1.com",
			},
		},
		{
			name:   "optionsWithoutOriginIsDelegated",
			method: http.MethodOptions,
			target: "/blitzy-cors-bucket",
			headers: map[string]string{
				"Access-Control-Request-Method": http.MethodPut,
			},
		},
		{
			name:   "optionsWithNeitherHeaderIsDelegated",
			method: http.MethodOptions,
			target: "/blitzy-cors-bucket",
		},
		{
			name:   "rootPathPreflightIsDelegated",
			method: http.MethodOptions,
			target: "/",
			headers: map[string]string{
				"Origin":                        "https://www.example1.com",
				"Access-Control-Request-Method": http.MethodPut,
			},
		},
		{
			name:   "rootPathPreflightWithRequestedHeadersIsDelegated",
			method: http.MethodOptions,
			target: "/",
			headers: map[string]string{
				"Origin":                         "https://www.example1.com",
				"Access-Control-Request-Method":  http.MethodPut,
				"Access-Control-Request-Headers": "x-amz-meta-foo, content-type",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
				if _, err := w.Write([]byte(delegatedBody)); err != nil {
					t.Errorf("writing the delegated response failed: %v", err)
				}
			})

			req := httptest.NewRequest(tc.method, tc.target, nil)
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)

			if !delegated {
				t.Fatal("expected the request to reach the wrapped handler")
			}
			if rec.Code != http.StatusTeapot {
				t.Errorf("expected the wrapped handler's status %d, got %d", http.StatusTeapot, rec.Code)
			}
			if rec.Body.String() != delegatedBody {
				t.Errorf("expected the wrapped handler's body %q, got %q", delegatedBody, rec.Body.String())
			}
			for _, name := range corsAllowHeaders {
				if value := rec.Header().Get(name); value != "" {
					t.Errorf("expected the middleware not to set %s on a delegated request, got %q", name, value)
				}
			}
			if value := rec.Header().Get("Vary"); value != "" {
				t.Errorf("expected the middleware not to set Vary on a delegated request, got %q", value)
			}
		})
	}
}

// TestBucketCorsPreflightMiddlewareUnresolvableHost pins the disposition of a
// preflight whose Host header the server does not accept, which with
// virtual-host-style addressing configured is also a Host the target bucket
// cannot be resolved from.
//
// The Host on this path is client controlled and unauthenticated, and the server
// has already decided what such a Host is worth: setRequestValidityMiddleware
// answers HTTP 400 for it, so the request the preflight asks about is refused
// before it reaches any bucket handler. Handing the preflight to the server-wide
// handler instead would answer the one request the server does respond to out of
// the most permissive setting it has - and the router does still route such a
// request path-style, so a bucket with restrictive rules of its own can genuinely
// be its target. It is therefore refused, in parity with the request it precedes.
// It must also not fail while doing so: resolving the Host through
// request2BucketObjectName reaches logger.CriticalIf, which panics.
//
// A Host that parses resolves its bucket as before, and an empty Host - which is
// the one unparsable Host the server tolerates, under CI/CD - resolves path-style
// off the request path, again exactly as the router would. Each case installs a
// configuration that would have matched the request, so the disposition is
// provably decided by the Host alone.
func TestBucketCorsPreflightMiddlewareUnresolvableHost(t *testing.T) {
	const (
		domain        = "s3.example.com"
		bucket        = "blitzy-cors-bucket"
		origin        = "https://www.example1.com"
		delegatedBody = "delegated"
	)

	matchingRule := miniogocors.Rule{
		ID:            "would-have-matched",
		AllowedHeader: []string{"*"},
		AllowedMethod: []string{http.MethodGet},
		AllowedOrigin: []string{"*"},
		ExposeHeader:  []string{"ETag"},
		MaxAgeSeconds: 3000,
	}

	testCases := []struct {
		name string
		host string
		// path is the request path, which is what the bucket is resolved from
		// whenever the Host does not name a virtual host.
		path string
		want corsPreflightOutcome
	}{
		// A Host the server refuses outright. Delegating any of these is what
		// would answer a request bound for a restrictive bucket out of the
		// server-wide default.
		{name: "nonNumericPortIsRefused", host: bucket + "." + domain + ":notaport", path: "/object", want: corsOutcomeDenied},
		{name: "portOutOfRangeIsRefused", host: bucket + "." + domain + ":99999", path: "/object", want: corsOutcomeDenied},
		{name: "invalidHostLabelIsRefused", host: "]bad[." + domain, path: "/object", want: corsOutcomeDenied},
		{name: "tooManyColonsIsRefused", host: ":::", path: "/object", want: corsOutcomeDenied},
		// The refusal does not depend on the path naming a bucket that exists:
		// the Host decided it before any lookup, which is what keeps this layer
		// in step with the middleware that answers 400 for the same Host.
		{
			name: "aRefusedHostIsRefusedEvenForAnUnknownBucket",
			host: "]bad[." + domain,
			path: "/blitzy-cors-absent/object",
			want: corsOutcomeDenied,
		},
		// The one unparsable Host the server tolerates. It names no virtual
		// host, so the bucket comes from the path, and the stored rules of the
		// bucket named there are what answer - parity with the path-style route
		// such a request is dispatched to.
		{
			name: "anEmptyHostResolvesTheBucketPathStyle",
			host: "",
			path: "/" + bucket + "/object",
			want: corsOutcomeAllowed,
		},
		// The same Host over a path naming a bucket without a configuration
		// falls back to the server-wide setting, which proves the case above
		// resolved the bucket rather than merely reaching the evaluator.
		{
			name: "anEmptyHostOverAnUnconfiguredBucketDelegates",
			host: "",
			path: "/blitzy-cors-absent/object",
			want: corsOutcomeDelegated,
		},
		// Port zero is a valid port, so this Host parses and the bucket it names
		// is evaluated. It is the boundary that proves only a Host the server
		// rejects is refused.
		{
			name: "portZeroResolves",
			host: bucket + "." + domain + ":0",
			path: "/object",
			want: corsOutcomeAllowed,
		},
		{
			name: "wellFormedHostResolves",
			host: bucket + "." + domain,
			path: "/object",
			want: corsOutcomeAllowed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			reporter := installTestCORSPreflightReporter(t)

			saved := globalDomainNames
			t.Cleanup(func() { globalDomainNames = saved })
			globalDomainNames = []string{domain}

			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
				if _, err := w.Write([]byte(delegatedBody)); err != nil {
					t.Errorf("writing the delegated response failed: %v", err)
				}
			})

			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.Host = tc.host
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			req.Header.Set("Access-Control-Request-Headers", "content-type")
			rec := httptest.NewRecorder()

			// Recover so a panic is reported as this subtest's failure instead
			// of aborting the suite.
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("the middleware panicked on the host %q: %v", tc.host, recovered)
					}
				}()
				bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)
			}()

			got := corsOutcomeDenied
			switch {
			case delegated:
				got = corsOutcomeDelegated
			case rec.Header().Get("Access-Control-Allow-Origin") != "":
				got = corsOutcomeAllowed
			}
			if got != tc.want {
				t.Fatalf("expected the preflight carrying host %q for path %q to be %s, it was %s (status %d, headers %v)",
					tc.host, tc.path, tc.want, got, rec.Code, rec.Header())
			}

			reportedAt, _ := corsPreflightReporterState(t, reporter)
			switch tc.want {
			case corsOutcomeAllowed:
				if rec.Code != http.StatusOK {
					t.Errorf("expected status %d for the matched rule, got %d", http.StatusOK, rec.Code)
				}
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
					t.Errorf("expected the matched rule to echo the origin %q, got %q", origin, got)
				}
				if !reportedAt.IsZero() {
					t.Errorf("expected an allowed preflight not to be reported, it was reported at %s", reportedAt)
				}
			case corsOutcomeDelegated:
				if rec.Code != http.StatusTeapot {
					t.Errorf("expected the wrapped handler's status %d, got %d", http.StatusTeapot, rec.Code)
				}
				if rec.Body.String() != delegatedBody {
					t.Errorf("expected the wrapped handler's body %q, got %q", delegatedBody, rec.Body.String())
				}
				for _, name := range corsPreflightAllowHeaders {
					if value := rec.Header().Get(name); value != "" {
						t.Errorf("expected the middleware not to set %s on a delegated request, got %q", name, value)
					}
				}
				if value := rec.Header().Get("Vary"); value != "" {
					t.Errorf("expected the middleware not to set Vary on a delegated request, got %q", value)
				}
				if !reportedAt.IsZero() {
					t.Errorf("expected a delegated preflight not to be reported, it was reported at %s", reportedAt)
				}
			case corsOutcomeDenied:
				if rec.Code != http.StatusOK {
					t.Errorf("expected a refused preflight to answer %d, got %d", http.StatusOK, rec.Code)
				}
				for _, name := range corsPreflightAllowHeaders {
					if value := rec.Header().Get(name); value != "" {
						t.Errorf("expected a refused preflight to carry no %s, got %q", name, value)
					}
				}
				// The refusal is indistinguishable on the wire from one the
				// bucket's own rules produced, so it is reported - and the
				// report carries nothing of the Host that caused it.
				if reportedAt.IsZero() {
					t.Error("expected a Host the server does not accept to be reported")
				}
			}
		})
	}
}

// corsPreflightAllowHeaders are the response headers a matched rule produces.
// Not one of them may appear on a delegated or denied response: their absence is
// exactly how a browser learns that a preflight was refused.
var corsPreflightAllowHeaders = []string{
	"Access-Control-Allow-Origin",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers",
	"Access-Control-Max-Age",
	"Access-Control-Expose-Headers",
}

// installTestBucketMetadataSys replaces the global bucket metadata system with a
// fresh, empty and fully loaded one for the duration of the test, restoring the
// previous value afterwards. A bucket is made known to it, with or without a CORS
// configuration, through setTestBucketCORSConfig; every other bucket name is one
// the cache does not hold.
//
// The system is marked loaded because that is the state of a running server, and
// it is the state in which a bucket the cache does not hold demonstrably has no
// configuration of its own. installLoadingTestBucketMetadataSys installs one that
// is still loading, where the same miss says nothing at all.
func installTestBucketMetadataSys(t *testing.T) *BucketMetadataSys {
	t.Helper()

	sys := installLoadingTestBucketMetadataSys(t)
	sys.Lock()
	sys.initialized = true
	sys.Unlock()
	return sys
}

// installLoadingTestBucketMetadataSys replaces the global bucket metadata system
// with a fresh, empty one that has not finished loading, restoring the previous
// value afterwards. It is the startup state: a bucket the cache does not hold may
// still have a configuration that has not been read yet.
func installLoadingTestBucketMetadataSys(t *testing.T) *BucketMetadataSys {
	t.Helper()

	saved := globalBucketMetadataSys
	t.Cleanup(func() { globalBucketMetadataSys = saved })

	sys := NewBucketMetadataSys()
	globalBucketMetadataSys = sys
	return sys
}

// installTestNoObjectLayer removes the object layer for the duration of the test,
// restoring the previous value afterwards.
//
// It is stated explicitly wherever a case turns on the object layer being absent,
// because the layer is a process-wide global that other tests in this package
// install and leave behind: without this, whether a load-window lookup can reach
// the backend at all would depend on which tests ran first.
func installTestNoObjectLayer(t *testing.T) {
	t.Helper()

	saved := globalObjectAPI
	t.Cleanup(func() { setObjectLayer(saved) })
	setObjectLayer(nil)
}

// setTestBucketCORSConfig makes bucket known to sys carrying cfg as its parsed
// CORS configuration. A nil cfg leaves the bucket present but without one.
func setTestBucketCORSConfig(sys *BucketMetadataSys, bucket string, cfg *miniogocors.Config) {
	meta := newBucketMetadata(bucket)
	meta.corsConfig = cfg
	sys.Set(bucket, meta)
}

// isCORSConfigNotFound reports whether err is the typed sentinel that says a
// bucket has no CORS configuration, however it is wrapped. It is the error the
// GET handler turns into NoSuchCORSConfiguration, so asserting on it rather than
// on any error is what proves an absence is reported as an absence.
func isCORSConfigNotFound(err error) bool {
	var notFound BucketCORSConfigNotFound
	return errors.As(err, &notFound)
}

// TestPreflightCORSConfigIsCacheOnly pins the property the preflight evaluator
// depends on. A preflight request carries no credentials and names its bucket in
// a path the client chooses freely, so the lookup it performs must never add an
// entry to the bucket metadata map - otherwise a stream of made-up names turns
// into permanent, unbounded memory growth - and once the cache has finished
// loading it must not read from the backend at all, because a miss then is
// already a confirmed absence.
//
// What the lookup reports matters as much as whether it succeeds: an absence and
// an unestablished configuration are disposed of differently by the caller, and
// corsPreflightConfigAbsent is what separates them. The cases below therefore
// assert the exact error alongside the configuration.
func TestPreflightCORSConfigIsCacheOnly(t *testing.T) {
	const (
		knownBucket   = "blitzy-cors-known"
		unknownBucket = "blitzy-cors-unknown"
	)
	ctx := context.Background()
	cfg := corsTestConfig(miniogocors.Rule{
		ID:            "only",
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"https://www.example1.com"},
	})

	// An absent metadata subsystem establishes nothing about the bucket, so it
	// is reported as an unestablished configuration rather than an absent one.
	t.Run("anAbsentMetadataSubsystem", func(t *testing.T) {
		saved := globalBucketMetadataSys
		t.Cleanup(func() { globalBucketMetadataSys = saved })
		globalBucketMetadataSys = nil

		got, err := preflightCORSConfig(ctx, knownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if !errors.Is(err, errServerNotInitialized) {
			t.Fatalf("expected %v, got %v", errServerNotInitialized, err)
		}
		if corsPreflightConfigAbsent(err) {
			t.Fatalf("expected %v not to be read as an absent configuration", err)
		}
	})

	t.Run("aBucketALoadedCacheDoesNotHold", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t)

		got, err := preflightCORSConfig(ctx, unknownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if err == nil {
			t.Fatal("expected an error for a bucket the cache does not hold")
		}
		// A loaded cache holds every bucket that exists, so the miss is the
		// bucket demonstrably having no configuration of its own.
		if !corsPreflightConfigAbsent(err) {
			t.Fatalf("expected a miss on a loaded cache to read as an absent configuration, got %v", err)
		}
		if sys.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", sys.Count())
		}
	})

	// While the cache is still loading the very same miss says nothing about the
	// bucket, so it must not be read as an absence: without an object layer to
	// establish the configuration from, the outcome is unestablished.
	t.Run("aBucketACacheThatIsStillLoadingDoesNotHold", func(t *testing.T) {
		sys := installLoadingTestBucketMetadataSys(t)
		installTestNoObjectLayer(t)

		got, err := preflightCORSConfig(ctx, unknownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if !errors.Is(err, errServerNotInitialized) {
			t.Fatalf("expected %v, got %v", errServerNotInitialized, err)
		}
		if corsPreflightConfigAbsent(err) {
			t.Fatalf("expected a miss on a loading cache not to read as an absent configuration, got %v", err)
		}
		if sys.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", sys.Count())
		}
	})

	// A bucket the loading cache does hold is answered from it, so loading
	// changes nothing for the buckets that have already been read.
	t.Run("aBucketACacheThatIsStillLoadingAlreadyHolds", func(t *testing.T) {
		sys := installLoadingTestBucketMetadataSys(t)
		setTestBucketCORSConfig(sys, knownBucket, cfg)

		got, err := preflightCORSConfig(ctx, knownBucket)
		if err != nil {
			t.Fatalf("expected the stored configuration, got error: %v", err)
		}
		if got != cfg {
			t.Fatalf("expected the stored configuration to be returned as-is, got %+v", got)
		}
	})

	// The reserved bucket is never a CORS target, and it is answered for without
	// consulting anything - loaded or not.
	t.Run("theReservedBucket", func(t *testing.T) {
		for name, install := range map[string]func(*testing.T) *BucketMetadataSys{
			"loaded":  installTestBucketMetadataSys,
			"loading": installLoadingTestBucketMetadataSys,
		} {
			t.Run(name, func(t *testing.T) {
				sys := install(t)

				got, err := preflightCORSConfig(ctx, minioMetaBucket)
				if got != nil {
					t.Fatalf("expected no configuration, got %+v", got)
				}
				if !corsPreflightConfigAbsent(err) {
					t.Fatalf("expected the reserved bucket to have no CORS configuration, got %v", err)
				}
				if sys.Count() != 0 {
					t.Fatalf("expected the lookup not to add a metadata entry, got %d", sys.Count())
				}
			})
		}
	})

	t.Run("aKnownBucketWithoutAConfiguration", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t)
		setTestBucketCORSConfig(sys, knownBucket, nil)

		got, err := preflightCORSConfig(ctx, knownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		var notFound BucketCORSConfigNotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("expected a BucketCORSConfigNotFound sentinel, got %v", err)
		}
		if notFound.Bucket != knownBucket {
			t.Fatalf("expected the sentinel to name the bucket %q, got %q", knownBucket, notFound.Bucket)
		}
		if !corsPreflightConfigAbsent(err) {
			t.Fatalf("expected the sentinel to read as an absent configuration, got %v", err)
		}
		if sys.Count() != 1 {
			t.Fatalf("expected exactly the one bucket that was stored, got %d", sys.Count())
		}
	})

	t.Run("aKnownBucketWithAConfiguration", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t)
		setTestBucketCORSConfig(sys, knownBucket, cfg)

		got, err := preflightCORSConfig(ctx, knownBucket)
		if err != nil {
			t.Fatalf("expected the stored configuration, got error: %v", err)
		}
		// The parsed document itself is handed back, not a copy of it, which is
		// what makes the lookup free of per-request parsing.
		if got != cfg {
			t.Fatalf("expected the stored configuration to be returned as-is, got %+v", got)
		}
	})

	// A configuration is removed by clearing the parsed pointer, so the lookup
	// must stop answering from it the moment that happens rather than keep the
	// rules a DELETE has already taken away.
	t.Run("aConfigurationThatWasRemoved", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t)
		setTestBucketCORSConfig(sys, knownBucket, cfg)

		if got, err := preflightCORSConfig(ctx, knownBucket); err != nil || got != cfg {
			t.Fatalf("expected the stored configuration, got %+v and error %v", got, err)
		}

		setTestBucketCORSConfig(sys, knownBucket, nil)

		got, err := preflightCORSConfig(ctx, knownBucket)
		if err == nil {
			t.Fatalf("expected no configuration once it was removed, got %+v", got)
		}
		if !corsPreflightConfigAbsent(err) {
			t.Fatalf("expected a removed configuration to read as an absent one, got %v", err)
		}
	})

	t.Run("manyUnknownBucketsNeverGrowTheMetadataMap", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t)

		for i := range 512 {
			bucket := fmt.Sprintf("blitzy-cors-probe-%d", i)
			if _, err := preflightCORSConfig(ctx, bucket); err == nil {
				t.Fatalf("expected an unknown bucket to have no configuration, got one for probe %d", i)
			}
		}
		if sys.Count() != 0 {
			t.Fatalf("expected 512 unknown lookups to add no metadata entries, got %d", sys.Count())
		}
	})

	// The same probing while the cache is still loading must be equally free of
	// consequences for the cache, however it is answered.
	t.Run("manyUnknownBucketsNeverGrowALoadingMetadataMap", func(t *testing.T) {
		sys := installLoadingTestBucketMetadataSys(t)
		installTestNoObjectLayer(t)

		for i := range 512 {
			bucket := fmt.Sprintf("blitzy-cors-loading-probe-%d", i)
			if _, err := preflightCORSConfig(ctx, bucket); err == nil {
				t.Fatalf("expected an unknown bucket to have no configuration, got one for probe %d", i)
			}
		}
		if sys.Count() != 0 {
			t.Fatalf("expected 512 unknown lookups to add no metadata entries, got %d", sys.Count())
		}
	})

	// A context that is already done must not turn into a lookup that reports an
	// absent configuration, because a canceled read establishes nothing.
	t.Run("aCanceledContextDuringLoading", func(t *testing.T) {
		installLoadingTestBucketMetadataSys(t)
		installTestNoObjectLayer(t)

		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		got, err := preflightCORSConfig(canceled, unknownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if corsPreflightConfigAbsent(err) {
			t.Fatalf("expected a canceled lookup not to read as an absent configuration, got %v", err)
		}
	})
}

// TestCorsPreflightConfigAbsent covers the classification the middleware's
// disposition turns on: which errors say a bucket has no CORS configuration of its
// own, and which say its configuration could not be established.
//
// Getting this wrong in either direction is a defect with teeth. Reading an
// unestablished configuration as an absent one hands the request to the
// server-wide handler and lets a configured bucket's own rules be bypassed;
// reading an absent one as unestablished refuses preflights for every bucket that
// simply has no configuration.
func TestCorsPreflightConfigAbsent(t *testing.T) {
	const bucket = "blitzy-cors-classify"

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "noError", err: nil, want: false},
		{
			name: "theTypedSentinel",
			err:  BucketCORSConfigNotFound{Bucket: bucket},
			want: true,
		},
		{
			name: "theTypedSentinelWrapped",
			err:  fmt.Errorf("for bucket %s: %w", bucket, BucketCORSConfigNotFound{Bucket: bucket}),
			want: true,
		},
		{
			name: "aCacheOrDocumentMiss",
			err:  errConfigNotFound,
			want: true,
		},
		{
			name: "aCacheOrDocumentMissWrapped",
			err:  fmt.Errorf("reading metadata: %w", errConfigNotFound),
			want: true,
		},
		{
			name: "anAbsentObjectLayerOrMetadataSubsystem",
			err:  errServerNotInitialized,
			want: false,
		},
		{
			name: "aMetadataSubsystemThatHasNotLoaded",
			err:  errBucketMetadataNotInitialized,
			want: false,
		},
		{
			name: "theReadGateBeingFull",
			err:  errCORSPreflightConfigReadsBusy,
			want: false,
		},
		{
			name: "aReadThatTimedOut",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "aClientThatWentAway",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "aDocumentThatWouldNotParse",
			err:  errors.New("xml: syntax error"),
			want: false,
		},
		{
			// A sentinel for a different configuration is not this one.
			name: "aSiblingConfigurationSentinel",
			err:  BucketSSEConfigNotFound{Bucket: bucket},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := corsPreflightConfigAbsent(tc.err); got != tc.want {
				t.Fatalf("expected corsPreflightConfigAbsent(%v) to be %t, got %t", tc.err, tc.want, got)
			}
		})
	}
}

// TestCorsBackendCORSConfigIsBounded covers the bounds the backend read is held
// to, which exist because the request that triggers it is unauthenticated.
//
// The read gate is the one bound a test can exercise without an object layer:
// with every slot taken, a request must be refused immediately rather than parked
// waiting for one, and the refusal must not read as an absent configuration.
func TestCorsBackendCORSConfigIsBounded(t *testing.T) {
	const bucket = "blitzy-cors-bounded"
	ctx := context.Background()

	if got := cap(corsPreflightConfigReads); got != maxCORSPreflightConfigReads {
		t.Fatalf("expected the read gate to admit %d readers, got %d", maxCORSPreflightConfigReads, got)
	}

	// Fill the gate, then restore it, so the case runs against exactly the
	// condition it describes and leaves the gate as it found it.
	for range maxCORSPreflightConfigReads {
		corsPreflightConfigReads <- struct{}{}
	}
	t.Cleanup(func() {
		for range maxCORSPreflightConfigReads {
			<-corsPreflightConfigReads
		}
	})

	// An object layer has to be present, or the absent one would be reported
	// before the gate is ever consulted.
	savedLayer := globalObjectAPI
	t.Cleanup(func() { setObjectLayer(savedLayer) })
	setObjectLayer(&erasureServerPools{})

	done := make(chan error, 1)
	go func() {
		_, err := corsBackendCORSConfig(ctx, bucket)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errCORSPreflightConfigReadsBusy) {
			t.Fatalf("expected %v, got %v", errCORSPreflightConfigReadsBusy, err)
		}
		if corsPreflightConfigAbsent(err) {
			t.Fatalf("expected a full read gate not to read as an absent configuration, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("expected a full read gate to refuse immediately rather than wait for a slot")
	}
}

// corsPreflightOutcome is how the middleware disposed of a request.
type corsPreflightOutcome int

const (
	// corsOutcomeDelegated means the wrapped, server-wide handler answered.
	corsOutcomeDelegated corsPreflightOutcome = iota
	// corsOutcomeAllowed means a stored rule matched and the middleware answered
	// with the CORS response headers that rule configures.
	corsOutcomeAllowed
	// corsOutcomeDenied means the middleware answered without a single
	// Access-Control-Allow-* header.
	corsOutcomeDenied
)

func (o corsPreflightOutcome) String() string {
	switch o {
	case corsOutcomeDelegated:
		return "delegated"
	case corsOutcomeAllowed:
		return "allowed"
	case corsOutcomeDenied:
		return "denied"
	}
	return "unknown"
}

// TestBucketCorsPreflightMiddlewareDisposition covers how the middleware answers
// a genuine preflight for every state the target bucket's CORS configuration can
// be in.
//
// The three dispositions carry very different weight. Delegating hands the
// request to the server-wide handler, and it is what every state in which this
// layer has no rules of its own to apply comes to: the bucket carries no
// configuration, the metadata subsystem is absent or has not finished loading,
// the lookup failed, or what is stored carries no rule. That is what leaves such
// a bucket answered exactly as it was before this layer existed, by the
// MINIO_API_CORS_ALLOW_ORIGIN setting. Once the rules are in hand the layer
// answers on its own, allowing on the first matching rule and denying when none
// allows the request.
func TestBucketCorsPreflightMiddlewareDisposition(t *testing.T) {
	const (
		bucket = "blitzy-cors-bucket"
		origin = "https://www.example1.com"
	)

	matchingRule := miniogocors.Rule{
		ID:            "matching",
		AllowedHeader: []string{"x-amz-*"},
		AllowedMethod: []string{http.MethodPut, http.MethodPost},
		AllowedOrigin: []string{origin},
		ExposeHeader:  []string{"ETag", "x-amz-request-id"},
		MaxAgeSeconds: 3000,
	}
	otherOriginRule := miniogocors.Rule{
		ID:            "other-origin",
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"https://www.example9.com"},
	}

	// A requested-header list far longer than any browser sends, which must
	// still be evaluated in full rather than answered on its length.
	manyHeaders := make([]string, 0, 256)
	for i := range 256 {
		manyHeaders = append(manyHeaders, fmt.Sprintf("x-amz-blitzy-%d", i))
	}

	testCases := []struct {
		name  string
		setup func(t *testing.T)
		// target defaults to a path-style request for bucket.
		target string
		// host and domainNames drive virtual-host-style addressing; both are
		// empty for the path-style default.
		host        string
		domainNames []string
		origin      string
		reqMethod   string
		reqHeaders  string
		want        corsPreflightOutcome
		// wantHeaders is asserted on an allowed response. A header named with an
		// empty value must be absent.
		wantHeaders map[string]string
	}{
		{
			// The metadata subsystem is not there to be asked, so whether this
			// bucket has rules of its own cannot be established at all. The
			// request is refused rather than answered from the server-wide
			// setting, because rules that were never read cannot be known to
			// allow it and answering globally is how a configured bucket's rules
			// would be bypassed.
			name: "nilMetadataSystemDenies",
			setup: func(t *testing.T) {
				t.Helper()
				saved := globalBucketMetadataSys
				t.Cleanup(func() { globalBucketMetadataSys = saved })
				globalBucketMetadataSys = nil
			},
			want: corsOutcomeDenied,
		},
		{
			// A loaded cache holds every bucket that exists, so one it does not
			// hold demonstrably has no configuration of its own and the request
			// is delegated exactly as it was before this layer existed.
			name: "bucketALoadedCacheDoesNotHoldDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t)
			},
			want: corsOutcomeDelegated,
		},
		{
			// The same miss on a cache that is still loading says nothing about
			// the bucket, and with no object layer to establish its configuration
			// from, the request is refused instead of delegated.
			name: "bucketALoadingCacheDoesNotHoldDenies",
			setup: func(t *testing.T) {
				t.Helper()
				installLoadingTestBucketMetadataSys(t)
				installTestNoObjectLayer(t)
			},
			want: corsOutcomeDenied,
		},
		{
			// The same outcome reached the other way: the bucket's metadata is in
			// hand and carries no CORS configuration at all.
			name: "bucketWithoutAConfigurationDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, nil)
			},
			want: corsOutcomeDelegated,
		},
		{
			// A rule-less document cannot have come from PutBucketCors, which
			// requires at least one rule. There is nothing in it to match
			// against, so it grants nothing and the request is delegated like
			// any other bucket this layer has no rules for.
			name: "storedConfigurationWithZeroRulesDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig())
			},
			want: corsOutcomeDelegated,
		},
		{
			name: "syntacticallyInvalidBucketNameDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t)
			},
			target: "/Not_A_Bucket/object",
			want:   corsOutcomeDelegated,
		},
		{
			name: "rootPathPreflightDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t)
			},
			target: "/",
			want:   corsOutcomeDelegated,
		},
		{
			name: "noRuleMatchesTheOriginDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(otherOriginRule))
			},
			want: corsOutcomeDenied,
		},
		{
			name: "noRuleMatchesTheMethodDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			},
			reqMethod: http.MethodDelete,
			want:      corsOutcomeDenied,
		},
		{
			name: "noRuleCoversTheRequestedHeadersDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			},
			reqHeaders: "content-type",
			want:       corsOutcomeDenied,
		},
		{
			// S3 puts no ceiling on how many headers a preflight may ask about,
			// so a rule that covers them all allows the request however long the
			// list is, and every name it asked about is echoed back.
			name: "manyRequestedHeadersAreAllowedWhenTheRuleCoversThem",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(
					miniogocors.Rule{
						ID:            "everything",
						AllowedHeader: []string{"*"},
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{"*"},
					},
				))
			},
			reqHeaders: strings.Join(manyHeaders, ","),
			want:       corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":  origin,
				"Access-Control-Allow-Methods": http.MethodPut,
				"Access-Control-Allow-Headers": strings.Join(manyHeaders, ", "),
			},
		},
		{
			// The same long list against a rule that covers all of it but one
			// header: the request is refused because the whole list is
			// evaluated, which is what a ceiling stopping short would hide.
			name: "manyRequestedHeadersDenyWhenOneOfThemIsNotCovered",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(
					miniogocors.Rule{
						ID:            "almost-everything",
						AllowedHeader: slices.Clone(manyHeaders[:len(manyHeaders)-1]),
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{"*"},
					},
				))
			},
			reqHeaders: strings.Join(manyHeaders, ","),
			want:       corsOutcomeDenied,
		},
		{
			name: "matchingRuleAllowsAndEmitsEveryHeaderFamily",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			},
			reqHeaders: "X-Amz-Meta-Foo, x-amz-acl",
			want:       corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":   origin,
				"Access-Control-Allow-Methods":  http.MethodPut,
				"Access-Control-Allow-Headers":  "X-Amz-Meta-Foo, x-amz-acl",
				"Access-Control-Max-Age":        "3000",
				"Access-Control-Expose-Headers": "ETag, x-amz-request-id",
			},
		},
		{
			name: "matchingRuleWithoutRequestedHeadersOmitsAllowHeaders",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			},
			want: corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":  origin,
				"Access-Control-Allow-Methods": http.MethodPut,
				"Access-Control-Allow-Headers": "",
				"Access-Control-Max-Age":       "3000",
			},
		},
		{
			name: "matchingRuleWithoutMaxAgeOrExposeHeaderOmitsThem",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(
					miniogocors.Rule{
						ID:            "bare",
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{origin},
					},
				))
			},
			want: corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":   origin,
				"Access-Control-Allow-Methods":  http.MethodPut,
				"Access-Control-Max-Age":        "",
				"Access-Control-Expose-Headers": "",
			},
		},
		{
			// First matching rule wins: the earlier rule's max age and exposed
			// headers are the ones that reach the client, even though a later
			// rule would also have matched.
			name: "firstMatchingRuleInDocumentOrderDeterminesTheResponse",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(
					otherOriginRule,
					miniogocors.Rule{
						ID:            "first-match",
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{origin},
						ExposeHeader:  []string{"ETag"},
						MaxAgeSeconds: 100,
					},
					miniogocors.Rule{
						ID:            "second-match",
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{"*"},
						ExposeHeader:  []string{"x-amz-request-id"},
						MaxAgeSeconds: 200,
					},
				))
			},
			want: corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin":   origin,
				"Access-Control-Max-Age":        "100",
				"Access-Control-Expose-Headers": "ETag",
			},
		},
		{
			// Virtual-host-style addressing must resolve to the same bucket,
			// because getResource turns the Host into a path-style resource
			// before path2BucketObject splits the bucket off it.
			name: "virtualHostStyleAddressingResolvesTheSameBucket",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(matchingRule))
			},
			target:      "/object",
			host:        bucket + ".s3.example.com",
			domainNames: []string{"s3.example.com"},
			want:        corsOutcomeAllowed,
			wantHeaders: map[string]string{
				"Access-Control-Allow-Origin": origin,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			target := tc.target
			if target == "" {
				target = "/" + bucket + "/object"
			}
			reqOrigin := tc.origin
			if reqOrigin == "" {
				reqOrigin = origin
			}
			reqMethod := tc.reqMethod
			if reqMethod == "" {
				reqMethod = http.MethodPut
			}

			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
			})

			req := httptest.NewRequest(http.MethodOptions, target, nil)
			req.Header.Set("Origin", reqOrigin)
			req.Header.Set("Access-Control-Request-Method", reqMethod)
			if tc.reqHeaders != "" {
				req.Header.Set("Access-Control-Request-Headers", tc.reqHeaders)
			}
			if tc.host != "" {
				req.Host = tc.host
				saved := globalDomainNames
				t.Cleanup(func() { globalDomainNames = saved })
				globalDomainNames = tc.domainNames
			}
			rec := httptest.NewRecorder()

			bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)

			got := corsOutcomeDenied
			switch {
			case delegated:
				got = corsOutcomeDelegated
			case rec.Header().Get("Access-Control-Allow-Origin") != "":
				got = corsOutcomeAllowed
			}
			if got != tc.want {
				t.Fatalf("expected the request to be %s, it was %s (status %d, headers %v)",
					tc.want, got, rec.Code, rec.Header())
			}

			if tc.want == corsOutcomeDelegated {
				if rec.Code != http.StatusTeapot {
					t.Errorf("expected the wrapped handler's status %d, got %d", http.StatusTeapot, rec.Code)
				}
				return
			}

			// Both answered dispositions are HTTP 200 and declare their
			// variance on all three preflight request axes.
			if rec.Code != http.StatusOK {
				t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
			}
			wantVary := []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"}
			if gotVary := rec.Header().Values("Vary"); !slices.Equal(gotVary, wantVary) {
				t.Errorf("expected Vary %v, got %v", wantVary, gotVary)
			}

			if tc.want == corsOutcomeDenied {
				for _, name := range corsPreflightAllowHeaders {
					if value := rec.Header().Get(name); value != "" {
						t.Errorf("expected a denied preflight to carry no %s, got %q", name, value)
					}
				}
				return
			}

			for name, want := range tc.wantHeaders {
				if got := rec.Header().Get(name); got != want {
					if want == "" {
						t.Errorf("expected %s to be absent, got %q", name, got)
						continue
					}
					t.Errorf("expected %s to be %q, got %q", name, want, got)
				}
			}
		})
	}
}

// TestBucketCorsPreflightMiddlewareMalformedHost pins the disposition of a
// preflight whose Host header a browser will send but this server will not accept,
// which is the case that decides whether this layer can be talked out of a
// bucket's rules by the Host alone.
//
// Every host below is one a browser forms an origin from and issues a request to,
// yet none of them parses as a MinIO host, so the server answers HTTP 400 for the
// request the preflight asks about. The preflight is therefore refused rather than
// delegated: delegating it would answer, out of the server-wide default that
// allows every origin with credentials, a request whose path-style route the
// generic router does resolve to a real bucket. The refusal also has to be
// panic-free, since resolving such a Host through request2BucketObjectName reaches
// logger.CriticalIf.
//
// Each case installs a bucket whose single rule would have matched, and the
// configured domain is varied so the refusal is shown not to depend on the Host
// belonging to it. The two axes are asserted together: what the middleware answers,
// and that it never reaches the wrapped handler.
func TestBucketCorsPreflightMiddlewareMalformedHost(t *testing.T) {
	const (
		bucket = "blitzy-cors-bucket"
		domain = "qa.local"
		origin = "https://www.example1.com"
	)

	overLongLabel := strings.Repeat("a", 70)

	hosts := []struct {
		name string
		host string
		// domains is the virtual-host-style configuration in force. An empty
		// slice is path-style-only addressing, where resolving the bucket never
		// parses the Host at all - and where the Host is still refused, because
		// the middleware that answers 400 for it does not consult the
		// configuration either.
		domains []string
	}{
		{name: "underscoreInALabel", host: "a_b." + domain, domains: []string{domain}},
		{name: "leadingHyphenInALabel", host: "-bad." + domain, domains: []string{domain}},
		{name: "overLongLabel", host: overLongLabel + "." + domain, domains: []string{domain}},
		{name: "emptyLabel", host: "a..b." + domain, domains: []string{domain}},
		{name: "malformedHostOutsideTheDomain", host: "a_b.other.tld", domains: []string{domain}},
		{name: "malformedHostCarryingAPort", host: "a_b." + domain + ":9000", domains: []string{domain}},
		// A browser accepts http://a_b.example/<bucket>/<object>, forms the origin
		// http://a_b.example from it, and the request is routed path-style to the
		// bucket in the path. No domain is configured, so nothing parses the Host
		// while resolving the bucket, and only refusing on the Host keeps the
		// preflight from being answered out of the server-wide default.
		{name: "pathStyleOnlyDeploymentIsRefusedToo", host: "a_b.example"},
	}

	for _, tc := range hosts {
		t.Run(tc.name, func(t *testing.T) {
			// The bucket is present and its single rule would match the probe,
			// so the disposition cannot be mistaken for the ordinary
			// no-configuration fallback: the only thing standing in the way is
			// the Host the server does not accept.
			sys := installTestBucketMetadataSys(t)
			setTestBucketCORSConfig(sys, bucket, corsTestConfig(miniogocors.Rule{
				ID:            "matching",
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"*"},
			}))
			reporter := installTestCORSPreflightReporter(t)

			saved := globalDomainNames
			t.Cleanup(func() { globalDomainNames = saved })
			globalDomainNames = tc.domains

			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
			})

			req := httptest.NewRequest(http.MethodOptions, "/"+bucket+"/object", nil)
			req.Host = tc.host
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", http.MethodPut)
			req.Header.Set("Access-Control-Request-Headers", "x-amz-meta-foo")
			rec := httptest.NewRecorder()

			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("the middleware panicked on the host %q: %v", tc.host, recovered)
					}
				}()
				bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)
			}()

			// Reaching the wrapped handler is what would hand the request to the
			// server-wide default, so it is the failure this test exists to
			// catch. Not reaching it is also what proves nothing panicked, since
			// the middleware answered on its own.
			if delegated {
				t.Fatalf("expected a preflight carrying the unaccepted Host %q not to reach the server-wide handler, it did (status %d)",
					tc.host, rec.Code)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("expected a refused preflight to answer %d, got %d", http.StatusOK, rec.Code)
			}
			for _, name := range corsPreflightAllowHeaders {
				if value := rec.Header().Get(name); value != "" {
					t.Errorf("expected a refused preflight to carry no %s, got %q", name, value)
				}
			}
			// The refusal declares the same variance as every other preflight
			// answer, so an intermediary cache cannot serve it to another origin.
			if got := rec.Header().Values("Vary"); len(got) == 0 {
				t.Error("expected a refused preflight to declare its variance")
			}
			if reportedAt, _ := corsPreflightReporterState(t, reporter); reportedAt.IsZero() {
				t.Error("expected a Host the server does not accept to be reported")
			}
		})
	}
}

// corsRequestedHeaderNamePrefix is shared by every name corsRequestedHeaderNames
// produces, so that a single wildcard AllowedHeader pattern can cover all of them.
// A rule is abandoned at the first requested header it does not cover, so only a
// covering pattern makes an evaluation walk the whole list - which is the shape
// the evaluation allowance exists to bound.
const corsRequestedHeaderNamePrefix = "z"

// corsRequestedHeaderNames returns count distinct, syntactically valid header
// names short enough that a great many of them fit inside one header value. They
// are the raw material of the widest requested-header list a client can get past
// the server's header size limit.
func corsRequestedHeaderNames(count int) []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	names := make([]string, 0, count)
	for i := range count {
		n := i
		name := []byte(corsRequestedHeaderNamePrefix + "\x00\x00\x00")
		for j := len(corsRequestedHeaderNamePrefix); j < len(name); j++ {
			name[j] = alphabet[n%len(alphabet)]
			n /= len(alphabet)
		}
		names = append(names, string(name))
	}
	return names
}

// corsWildcardHeaderPatterns returns decoys wildcard AllowedHeader patterns that
// cover nothing corsRequestedHeaderNames produces, followed by one that covers all
// of them and one that covers the ordinary x-amz-* headers a browser asks about.
//
// It is an entirely ordinary document by size - a few hundred short patterns - and
// it is the second factor of the product the evaluation allowance bounds. The
// index the matcher builds cannot flatten a wildcard, so each of them has to be
// compared against each requested header.
func corsWildcardHeaderPatterns(decoys int) []string {
	patterns := make([]string, 0, decoys+2)
	for i := range decoys {
		patterns = append(patterns, fmt.Sprintf("x-never-%d-*", i))
	}
	return append(patterns, corsRequestedHeaderNamePrefix+"*", "x-amz-*")
}

// TestBucketCorsPreflightMiddlewareBoundedWork pins the cost of answering one
// preflight, which is the cost of answering an unauthenticated request whose
// headers are entirely the client's to choose.
//
// Two limits do that, and they compose. The first is the server's own header size
// limit, which the evaluator applies itself because it runs ahead of the
// middleware that normally would: without it a request may carry a megabyte of
// headers, since that is all the HTTP server itself refuses. The second is the
// evaluation allowance, which bounds the product of the requested header list and
// the AllowedHeader patterns of the rules - a product that stays out of reach of
// any size limit, because both lists are individually modest.
//
// Neither may be reached by narrowing what is evaluated: every header a request
// asks about is still compared against the rules, so a rule that covers all but
// one of them still refuses the request.
func TestBucketCorsPreflightMiddlewareBoundedWork(t *testing.T) {
	const (
		bucket = "blitzy-cors-bucket"
		origin = "https://www.example1.com"
	)

	allowEverything := corsTestConfig(miniogocors.Rule{
		ID:            "allow-everything",
		AllowedHeader: []string{"*"},
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"*"},
		MaxAgeSeconds: 3000,
	})

	preflight := func(reqHeaders string) *http.Request {
		req := httptest.NewRequest(http.MethodOptions, "/"+bucket+"/object", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		if reqHeaders != "" {
			req.Header.Set("Access-Control-Request-Headers", reqHeaders)
		}
		return req
	}

	serve := func(t *testing.T, req *http.Request) (corsPreflightOutcome, *httptest.ResponseRecorder) {
		t.Helper()

		delegated := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			delegated = true
			w.WriteHeader(http.StatusTeapot)
		})
		rec := httptest.NewRecorder()
		bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)

		switch {
		case delegated:
			return corsOutcomeDelegated, rec
		case rec.Header().Get("Access-Control-Allow-Origin") != "":
			return corsOutcomeAllowed, rec
		}
		return corsOutcomeDenied, rec
	}

	// A request carrying more header bytes than the server admits is refused
	// before anything is read from those headers, so the refusal cannot depend on
	// the bucket at all. Delegating it would answer, out of the permissive
	// server-wide default, a request the server itself refuses outright.
	//
	// The two fixtures carry the same excess differently, and both have to be
	// refused. The first states it in a single Access-Control-Request-Headers
	// value. The second opens with a value so ordinary that an accounting reading
	// only the first value of each field would find nothing to object to, and
	// carries the rest of the excess in further fields of the same name - which is
	// the shape that matters, because this layer reads every value of that field,
	// joins them into one requested-header list, and echoes that whole list back
	// on a match.
	t.Run("headersLargerThanTheServerAdmitsAreRefused", func(t *testing.T) {
		names := corsRequestedHeaderNames(4096)

		for _, fixture := range []struct {
			name    string
			request func() *http.Request
		}{
			{
				name:    "statedInOneValue",
				request: func() *http.Request { return preflight(strings.Join(names, ",")) },
			},
			{
				name: "spreadAcrossRepeatedFields",
				request: func() *http.Request {
					req := preflight("x-amz-acl")
					for chunk := range slices.Chunk(names, 256) {
						req.Header.Add("Access-Control-Request-Headers", strings.Join(chunk, ","))
					}
					return req
				},
			},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				for _, tc := range []struct {
					name  string
					setup func(t *testing.T)
				}{
					{
						name: "forABucketWithPermissiveRules",
						setup: func(t *testing.T) {
							t.Helper()
							setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, allowEverything)
						},
					},
					{
						// No configuration at all, which is the one state that
						// would otherwise delegate. The size limit outranks it.
						name: "forABucketWithoutAConfiguration",
						setup: func(t *testing.T) {
							t.Helper()
							setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, nil)
						},
					},
				} {
					t.Run(tc.name, func(t *testing.T) {
						tc.setup(t)
						reporter := installTestCORSPreflightReporter(t)

						req := fixture.request()
						if !corsPreflightHeadersTooLarge(req.Header) {
							t.Fatalf("the fixture must exceed the header size limit, %d name(s) across %d field(s) did not",
								len(names), len(req.Header.Values("Access-Control-Request-Headers")))
						}

						got, rec := serve(t, req)
						if got != corsOutcomeDenied {
							t.Fatalf("expected an oversized preflight to be refused, it was %s (status %d, headers %v)",
								got, rec.Code, rec.Header())
						}
						for _, name := range corsPreflightAllowHeaders {
							if value := rec.Header().Get(name); value != "" {
								t.Errorf("expected a refused preflight to carry no %s, got %q", name, value)
							}
						}
						// Nothing but the variance the answer depends on, so the
						// refusal reports none of the names it declined to read.
						if got := rec.Header().Values("Vary"); len(got) == 0 {
							t.Error("expected a refused preflight to declare its variance")
						}
						// One report, whatever the request carried: the reason is
						// the server's own text, and the volume is bounded by the
						// reporter rather than by how many fields arrived.
						reportedAt, unreported := corsPreflightReporterState(t, reporter)
						if reportedAt.IsZero() {
							t.Error("expected an oversized preflight to be reported")
						}
						if unreported != 0 {
							t.Errorf("expected a single refusal to be reported once, %d went unreported", unreported)
						}
					})
				}
			})
		}
	})

	// The widest list the size limit does admit is still evaluated in full: every
	// name is compared, the response echoes every one of them back, and a rule one
	// name short refuses the request. A layer that quietly stopped after the first
	// so many headers would allow requests the rules do not.
	t.Run("theWidestAdmissibleHeaderListIsEvaluatedInFull", func(t *testing.T) {
		names := corsRequestedHeaderNames(1600)
		reqHeaders := strings.Join(names, ",")

		req := preflight(reqHeaders)
		if corsPreflightHeadersTooLarge(req.Header) {
			t.Fatalf("the fixture must be admissible, %d name(s) were not", len(names))
		}

		t.Run("aWildcardCoversThemAll", func(t *testing.T) {
			setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, allowEverything)

			got, rec := serve(t, preflight(reqHeaders))
			if got != corsOutcomeAllowed {
				t.Fatalf("expected the wildcard rule to allow all %d requested headers, the request was %s",
					len(names), got)
			}
			if allowed := rec.Header().Get("Access-Control-Allow-Headers"); allowed != strings.Join(names, ", ") {
				t.Errorf("expected every requested header to be echoed back, got %d byte(s)", len(allowed))
			}
		})

		t.Run("aRuleOneNameShortRefusesTheRequest", func(t *testing.T) {
			setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(miniogocors.Rule{
				ID:            "one-short",
				AllowedHeader: slices.Clone(names[:len(names)-1]),
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{origin},
			}))
			reporter := installTestCORSPreflightReporter(t)

			got, _ := serve(t, preflight(reqHeaders))
			if got != corsOutcomeDenied {
				t.Fatalf("expected a rule missing the last of %d requested headers to refuse, the request was %s",
					len(names), got)
			}
			// The rules answered, so this is the configuration working as
			// written rather than a condition to report.
			if reportedAt, _ := corsPreflightReporterState(t, reporter); !reportedAt.IsZero() {
				t.Errorf("expected a refusal by the rules not to be reported, it was reported at %s", reportedAt)
			}
		})
	})

	// The pairing the size limit cannot bound: a request the server admits, and a
	// stored document of an entirely ordinary size whose AllowedHeader values are
	// wildcard patterns, which no index can flatten. Their product is what the
	// allowance refuses.
	t.Run("anUnaffordableEvaluationIsRefusedAndReported", func(t *testing.T) {
		names := corsRequestedHeaderNames(1600)
		reqHeaders := strings.Join(names, ",")

		setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(miniogocors.Rule{
			ID:            "many-patterns",
			AllowedHeader: corsWildcardHeaderPatterns(800),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		}))
		reporter := installTestCORSPreflightReporter(t)

		req := preflight(reqHeaders)
		if corsPreflightHeadersTooLarge(req.Header) {
			t.Fatal("the fixture must be a request the server admits, or the size limit rather than the allowance would refuse it")
		}

		got, rec := serve(t, req)
		if got != corsOutcomeDenied {
			t.Fatalf("expected an unaffordable evaluation to be refused, it was %s (status %d, headers %v)",
				got, rec.Code, rec.Header())
		}
		for _, name := range corsPreflightAllowHeaders {
			if value := rec.Header().Get(name); value != "" {
				t.Errorf("expected a refused preflight to carry no %s, got %q", name, value)
			}
		}
		if reportedAt, _ := corsPreflightReporterState(t, reporter); reportedAt.IsZero() {
			t.Error("expected an unaffordable evaluation to be reported")
		}

		// The same document answers an ordinary preflight, so the allowance
		// refuses the pairing rather than the configuration.
		if got, _ := serve(t, preflight("x-amz-acl")); got != corsOutcomeAllowed {
			t.Errorf("expected the same document to allow an ordinary preflight, the request was %s", got)
		}
	})

	// Every refusal is decided per request, so a flood of them costs the flood's
	// sender the requests and costs this server one report.
	t.Run("concurrentUnaffordablePreflightsAreEachRefused", func(t *testing.T) {
		const requests = 64

		reqHeaders := strings.Join(corsRequestedHeaderNames(1600), ",")

		setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(miniogocors.Rule{
			ID:            "many-patterns",
			AllowedHeader: corsWildcardHeaderPatterns(800),
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		}))
		reporter := installTestCORSPreflightReporter(t)

		middleware := bucketCORSPreflightMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))

		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			delegates int
			allows    int
		)
		for range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				middleware.ServeHTTP(rec, preflight(reqHeaders))

				mu.Lock()
				defer mu.Unlock()
				if rec.Code == http.StatusTeapot {
					delegates++
				}
				if rec.Header().Get("Access-Control-Allow-Origin") != "" {
					allows++
				}
			}()
		}
		wg.Wait()

		if delegates != 0 || allows != 0 {
			t.Fatalf("expected all %d concurrent preflights to be refused, %d were delegated and %d allowed",
				requests, delegates, allows)
		}
		reportedAt, unreported := corsPreflightReporterState(t, reporter)
		if reportedAt.IsZero() {
			t.Fatal("expected the first of the concurrent refusals to be reported")
		}
		// One report, and the rest counted: the volume is this server's choice
		// rather than the sender's.
		if unreported != requests-1 {
			t.Fatalf("expected %d of %d refusals to be counted rather than reported, got %d",
				requests-1, requests, unreported)
		}
	})
}

// TestCorsPreflightHeadersTooLarge pins the accounting a preflight is admitted
// by, which bounds the input an unauthenticated client may present to this layer.
// The cost of evaluating what it does present is bounded separately, by
// maxCORSPreflightMatchCost.
//
// What it has to measure is a request whose header fields may repeat: the
// requested-header list is read from every value of Access-Control-Request-Headers
// and echoed back in full on a match, so every value of every field counts, with
// the field name counted once per value the way the wire carries it. Measuring the
// first value of each field alone - which is what the server-wide helper does, and
// precisely what this function exists in order not to do - would let a client
// state a modest first value and carry the rest of the list in further fields of
// the same name.
func TestCorsPreflightHeadersTooLarge(t *testing.T) {
	const requestedHeaders = "Access-Control-Request-Headers"

	// bytes returns a header value of exactly the requested length, so a fixture
	// states how many header bytes it carries rather than implying it.
	bytesOfLength := func(length int) string { return strings.Repeat("x", length) }

	// preflightHeader returns the fields an ordinary browser preflight carries,
	// which every fixture below starts from.
	preflightHeader := func() http.Header {
		header := http.Header{}
		header.Set("Origin", "https://www.example1.com")
		header.Set("Access-Control-Request-Method", http.MethodPut)
		return header
	}

	testCases := []struct {
		name   string
		header func() http.Header
		want   bool
	}{
		{
			name: "anOrdinaryPreflightIsAdmitted",
			header: func() http.Header {
				header := preflightHeader()
				header.Set(requestedHeaders, "content-type, x-amz-acl")
				return header
			},
			want: false,
		},
		{
			name: "aValueFillingTheCeilingIsStillAdmitted",
			header: func() http.Header {
				header := http.Header{}
				header.Set(requestedHeaders, bytesOfLength(maxHeaderSize-len(requestedHeaders)))
				return header
			},
			want: false,
		},
		{
			name: "oneValueOverTheCeilingIsRefused",
			header: func() http.Header {
				header := http.Header{}
				header.Set(requestedHeaders, bytesOfLength(maxHeaderSize-len(requestedHeaders)+1))
				return header
			},
			want: true,
		},
		{
			// Excess bytes split across repeated fields of the same name have to
			// be counted even when the first value is small: an accounting that
			// read Header.Get alone would admit this request.
			name: "excessSpreadAcrossRepeatedFieldsIsRefused",
			header: func() http.Header {
				header := preflightHeader()
				header.Set(requestedHeaders, "x-amz-acl")
				for range 8 {
					header.Add(requestedHeaders, bytesOfLength(maxHeaderSize/8))
				}
				return header
			},
			want: true,
		},
		{
			// Repetition is not itself the fault: fields that repeat while their
			// values still fit inside the ceiling are admitted, so a gateway that
			// splits one list across several fields is not turned away for it.
			name: "repeatedFieldsInsideTheCeilingAreAdmitted",
			header: func() http.Header {
				header := preflightHeader()
				for range 8 {
					header.Add(requestedHeaders, bytesOfLength(512))
				}
				return header
			},
			want: false,
		},
		{
			// Two values whose bytes alone would just fit, and do not once each
			// one carries its field name - which is what a second field costs on
			// the wire.
			name: "theFieldNameCountsOncePerValue",
			header: func() http.Header {
				header := http.Header{}
				half := (maxHeaderSize - len(requestedHeaders)) / 2
				header.Add(requestedHeaders, bytesOfLength(half))
				header.Add(requestedHeaders, bytesOfLength(maxHeaderSize-len(requestedHeaders)-half))
				return header
			},
			want: true,
		},
		{
			// The second ceiling this server applies. User metadata is bounded
			// well below the total, and repetition must not evade that bound
			// either, even though the request stays far inside maxHeaderSize.
			name: "repeatedUserMetadataIsRefusedByItsOwnCeiling",
			header: func() http.Header {
				header := preflightHeader()
				for range 4 {
					header.Add("x-amz-meta-tag", bytesOfLength(maxUserDataSize/4))
				}
				return header
			},
			want: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			header := testCase.header()

			if got := corsPreflightHeadersTooLarge(header); got != testCase.want {
				t.Fatalf("expected %t, got %t for %d field(s) carrying %d value(s)",
					testCase.want, got, len(header), len(header.Values(requestedHeaders)))
			}

			// Whatever this accounting admits, the request-limit middleware
			// inside the router admits as well: every value of every field is
			// counted here, so this can only ever refuse more. A preflight
			// answered from a bucket's rules is therefore never one the request
			// it precedes would have been refused for carrying.
			if isHTTPHeaderSizeTooLarge(header) && !corsPreflightHeadersTooLarge(header) {
				t.Fatal("expected every request the server-wide limit refuses to be refused here too")
			}
		})
	}
}

// TestBucketCorsPreflightMiddlewareLeavesNoMetadataTrace pins the resource half
// of the preflight contract: a stream of preflights naming buckets that do not
// exist must leave the bucket metadata map exactly as it found it. Without that
// guarantee an unauthenticated client can grow the map without bound simply by
// inventing a new name per request.
func TestBucketCorsPreflightMiddlewareLeavesNoMetadataTrace(t *testing.T) {
	sys := installTestBucketMetadataSys(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	middleware := bucketCORSPreflightMiddleware(next)

	for i := range 1024 {
		req := httptest.NewRequest(http.MethodOptions, fmt.Sprintf("/blitzy-cors-probe-%d/object", i), nil)
		req.Header.Set("Origin", "https://www.example1.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		middleware.ServeHTTP(httptest.NewRecorder(), req)
	}

	if sys.Count() != 0 {
		t.Fatalf("expected 1024 preflights for non-existent buckets to add no metadata entries, got %d",
			sys.Count())
	}
}

// TestBucketCorsPreflightServerWideFallback holds the disposition of a preflight
// for a bucket that has no rules of its own, through the handler a running server
// actually composes rather than through the middleware alone.
//
// The bound it pins is the one a browser feels: a bucket that demonstrably has no
// configuration of its own must keep being answered from
// MINIO_API_CORS_ALLOW_ORIGIN, and every reason there are demonstrably no rules to
// answer from has to be treated alike - the bucket carries no configuration, the
// loaded cache does not hold it, or what is stored carries no rule.
//
// A bucket whose configuration could not be established at all is a different
// matter, and is deliberately not treated alike. While the metadata cache is
// loading, a miss says nothing: the bucket may have rules that have not been read
// yet. Answering such a request from the server-wide setting would let a
// configured bucket's own rules be bypassed for as long as loading takes, so the
// configuration is established from the backend for that one bucket instead, and
// only what that read establishes decides the answer. When even that cannot be
// done - there is no object layer or no metadata subsystem to read through - the
// request is refused rather than answered globally, which is the one disposition
// that cannot serve a bucket's data to an origin its rules exclude.
//
// The three answers are distinguished by their own fingerprints rather than by
// inspection: the server-wide handler replies 204 and sets
// Access-Control-Allow-Credentials, and never emits Access-Control-Expose-Headers
// on a preflight; the per-bucket evaluator replies 200 and sets neither; and a
// per-bucket refusal is that same 200 carrying no Access-Control-Allow-* header at
// all. So a rule that matches and a rule set that refuses must both still answer
// for themselves, which is what keeps this from passing by delegating everything.
func TestBucketCorsPreflightServerWideFallback(t *testing.T) {
	const (
		bucket = "blitzy-cors-bucket"
		origin = "https://www.example1.com"
	)

	rule := miniogocors.Rule{
		ID:            "matching",
		AllowedHeader: []string{"x-amz-*"},
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{origin},
		ExposeHeader:  []string{"ETag"},
		MaxAgeSeconds: 3000,
	}

	// corsFallbackDisposition is which layer is expected to answer the request.
	type corsFallbackDisposition int
	const (
		// corsAnsweredServerWide is the untouched global handler answering.
		corsAnsweredServerWide corsFallbackDisposition = iota
		// corsAnsweredPerBucket is the per-bucket evaluator answering from rules
		// it has in hand, whether it allows the request or refuses it.
		corsAnsweredPerBucket
		// corsRefusedPerBucket is the per-bucket evaluator refusing because the
		// bucket's configuration could not be established.
		corsRefusedPerBucket
	)

	testCases := []struct {
		name  string
		setup func(t *testing.T)
		// origin the preflight announces; the stored rule allows only origin.
		origin string
		// want is the layer expected to answer.
		want corsFallbackDisposition
	}{
		{
			// The condition a freshly started server is in for as long as it takes
			// to load bucket metadata. The cache cannot say whether the bucket has
			// a configuration, and with no object layer to establish it from the
			// request is refused rather than answered from the global setting -
			// which is also the state in which every S3 handler answers
			// ErrServerNotInitialized, so the request this preflight asks about
			// could not have been served either.
			name: "aBucketAbsentFromACacheThatIsStillLoading",
			setup: func(t *testing.T) {
				t.Helper()
				installLoadingTestBucketMetadataSys(t)
				installTestNoObjectLayer(t)
			},
			want: corsRefusedPerBucket,
		},
		{
			name:  "aBucketAbsentFromALoadedCache",
			setup: func(t *testing.T) { t.Helper(); installTestBucketMetadataSys(t) },
			want:  corsAnsweredServerWide,
		},
		{
			name: "aBucketWhoseMetadataCarriesNoConfiguration",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, nil)
			},
			want: corsAnsweredServerWide,
		},
		{
			// The same bucket, in a cache that has not finished loading: it has
			// already been read, so its absent configuration is established and
			// the global setting answers exactly as it does once loading is done.
			name: "aBucketACacheThatIsStillLoadingHasAlreadyRead",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installLoadingTestBucketMetadataSys(t), bucket, nil)
			},
			want: corsAnsweredServerWide,
		},
		{
			name: "aStoredDocumentCarryingNoRule",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig())
			},
			want: corsAnsweredServerWide,
		},
		{
			// Nothing to establish the configuration through, so nothing is known
			// about the bucket's rules and the request is refused.
			name: "anAbsentMetadataSubsystem",
			setup: func(t *testing.T) {
				t.Helper()
				saved := globalBucketMetadataSys
				t.Cleanup(func() { globalBucketMetadataSys = saved })
				globalBucketMetadataSys = nil
			},
			want: corsRefusedPerBucket,
		},
		{
			// Rules that are in hand answer for themselves, so the fallback must
			// not reach this request: the matched rule allows it.
			name: "aMatchingRuleAnswersInstead",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(rule))
			},
			want: corsAnsweredPerBucket,
		},
		{
			// The same rules govern while the cache is still loading, because the
			// bucket they belong to has already been read into it. This is the
			// case a global fallback must never reach: the origin below is one the
			// unset global list would allow and the stored rule allows too, so
			// only the absent Access-Control-Allow-Credentials tells the two
			// answers apart.
			name: "aMatchingRuleGovernsWhileTheCacheIsStillLoading",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installLoadingTestBucketMetadataSys(t), bucket, corsTestConfig(rule))
			},
			want: corsAnsweredPerBucket,
		},
		{
			// And the fallback must not reach a request the rules refuse either,
			// even though the server-wide setting would have allowed this origin.
			name: "rulesThatRefuseTheRequestAnswerInstead",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(rule))
			},
			origin: "https://www.example9.com",
			want:   corsAnsweredPerBucket,
		},
		{
			// The stored rules refuse this origin and the cache is still loading,
			// which is the exact shape of the leak this disposition exists to
			// prevent: the global setting would have allowed it.
			name: "rulesThatRefuseTheRequestGovernWhileTheCacheIsStillLoading",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installLoadingTestBucketMetadataSys(t), bucket, corsTestConfig(rule))
			},
			origin: "https://www.example9.com",
			want:   corsAnsweredPerBucket,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			// A refusal is reported, and this test causes several of them, so the
			// reporter is replaced to keep the server's own out of it.
			installTestCORSPreflightReporter(t)

			// The S3 handler behind the CORS layers must never be reached by a
			// preflight: no mux route is registered for OPTIONS, which is the
			// whole reason the evaluator is HTTP middleware.
			handler := corsHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("expected a preflight not to reach the wrapped S3 handler")
			}))

			reqOrigin := tc.origin
			if reqOrigin == "" {
				reqOrigin = origin
			}
			req := httptest.NewRequest(http.MethodOptions, "/"+bucket+"/object", nil)
			req.Header.Set("Origin", reqOrigin)
			req.Header.Set("Access-Control-Request-Method", http.MethodPut)
			req.Header.Set("Access-Control-Request-Headers", "x-amz-meta-foo")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if tc.want != corsAnsweredServerWide {
				// Answered by the per-bucket evaluator: 200, and the server-wide
				// handler's Access-Control-Allow-Credentials is absent because
				// that handler never saw the request.
				if rec.Code != http.StatusOK {
					t.Errorf("expected the per-bucket evaluator's status %d, got %d", http.StatusOK, rec.Code)
				}
				if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
					t.Errorf("expected the server-wide handler not to have answered, it set Access-Control-Allow-Credentials to %q", got)
				}
				if tc.want == corsRefusedPerBucket {
					// A refusal carries no allow header of any kind, which is how
					// a browser learns the request is not permitted.
					for _, name := range corsPreflightAllowHeaders {
						if got := rec.Header().Values(name); len(got) != 0 {
							t.Errorf("expected a refused preflight to carry no %s, got %v", name, got)
						}
					}
				}
				return
			}

			// Answered by the server-wide handler, with the status and headers it
			// has always answered a preflight with. The global allow-origin list
			// is unset in this process, which getCorsAllowOrigins reports as "*",
			// so the origin is allowed.
			if rec.Code != http.StatusNoContent {
				t.Errorf("expected the server-wide handler's status %d, got %d", http.StatusNoContent, rec.Code)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != reqOrigin {
				t.Errorf("expected the server-wide handler to allow the origin %q, got %q", reqOrigin, got)
			}
			if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Errorf("expected the server-wide handler's Access-Control-Allow-Credentials to be %q, got %q", "true", got)
			}
			if got := rec.Header().Get("Access-Control-Allow-Methods"); got != http.MethodPut {
				t.Errorf("expected the server-wide handler to allow the method %q, got %q", http.MethodPut, got)
			}
			// Never emitted on a preflight by the server-wide handler, so its
			// absence is what proves the per-bucket evaluator did not answer.
			if got := rec.Header().Values("Access-Control-Expose-Headers"); len(got) != 0 {
				t.Errorf("expected no Access-Control-Expose-Headers on a server-wide preflight response, got %v", got)
			}
		})
	}
}

// TestBucketCorsPreflightAcrossTheMetadataLoadWindow proves the property a
// restart turns on, against a real backend: from the moment a server answers
// requests, a bucket's stored CORS rules govern its preflights, and a bucket that
// has none is still answered from MINIO_API_CORS_ALLOW_ORIGIN.
//
// The two halves pull in opposite directions, which is why they are asserted
// together. While the bucket metadata cache is loading it holds neither bucket, so
// a miss cannot be read as an absence - doing that answers a configured bucket's
// preflight from the global setting and lets an origin its rules exclude through
// for as long as loading takes. Nor can a miss be refused outright, because that
// denies every unconfigured bucket the answer the server has always given it. Only
// establishing the configuration for the one bucket the request names satisfies
// both, and only a real object layer can prove it does.
//
// The global allow-origin list is unset in this process, which
// getCorsAllowOrigins reports as "*", so the server-wide handler would allow every
// origin below. A stored rule set that refuses one is therefore refusing an origin
// the fallback would have allowed, which is exactly the leak this exists to
// prevent, and the answers are told apart by their fingerprints: the server-wide
// handler replies 204 with Access-Control-Allow-Credentials, the per-bucket
// evaluator replies 200 without it.
func TestBucketCorsPreflightAcrossTheMetadataLoadWindow(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{
		t:          t,
		objAPITest: testBucketCorsPreflightAcrossTheMetadataLoadWindow,
	})
}

func testBucketCorsPreflightAcrossTheMetadataLoadWindow(obj ObjectLayer, instanceType, bucketName string,
	apiRouter http.Handler, creds auth.Credentials, t *testing.T,
) {
	const (
		configuredOrigin = "https://configured.example"
		excludedOrigin   = "https://excluded.example"
	)
	ctx := t.Context()

	// The rule allows one origin and one method, so every other origin is one the
	// stored configuration refuses while the global setting would allow it.
	document := corsTestDoc(corsTestRule(
		`<ID>load-window</ID>` +
			`<AllowedMethod>PUT</AllowedMethod>` +
			`<AllowedOrigin>` + configuredOrigin + `</AllowedOrigin>` +
			`<AllowedHeader>x-amz-*</AllowedHeader>` +
			`<ExposeHeader>ETag</ExposeHeader>` +
			`<MaxAgeSeconds>3000</MaxAgeSeconds>`))

	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodPut, bucketName, []byte(document), creds))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the configuration to be stored with %d, got %d with body %q",
			instanceType, http.StatusOK, rec.Code, truncateForError(rec.Body.String()))
	}

	// A second bucket that exists and carries no CORS configuration, which is the
	// bucket whose preflights the global setting has always answered.
	unconfiguredBucket := getRandomBucketName()
	if err := obj.MakeBucket(ctx, unconfiguredBucket, MakeBucketOptions{}); err != nil {
		t.Fatalf("%s: failed to create the unconfigured bucket %s: %v", instanceType, unconfiguredBucket, err)
	}

	// An object layer has to be reachable for a configuration to be established
	// from the backend at all, which is the state a server is in once it is
	// serving requests while its metadata cache still loads.
	savedLayer := globalObjectAPI
	t.Cleanup(func() { setObjectLayer(savedLayer) })
	setObjectLayer(obj)

	// The load window itself: a cache that holds nothing and has not finished
	// loading, in front of the backend that holds both buckets.
	sys := installLoadingTestBucketMetadataSys(t)
	installTestCORSPreflightReporter(t)

	handler := corsHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("%s: expected a preflight not to reach the wrapped S3 handler", instanceType)
	}))

	preflight := func(bucket, origin string) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequest(http.MethodOptions, "/"+bucket+"/object", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		req.Header.Set("Access-Control-Request-Headers", "x-amz-meta-foo")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// The stored rule allows this request, and it is answered from that rule
	// rather than by the global handler: 200, no Access-Control-Allow-Credentials,
	// and the rule's own ExposeHeader and MaxAgeSeconds, neither of which the
	// global handler ever emits on a preflight.
	rec = preflight(bucketName, configuredOrigin)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the stored rule to answer with %d during the load window, got %d",
			instanceType, http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("%s: expected the stored rule rather than the server-wide handler to answer, it set Access-Control-Allow-Credentials to %q",
			instanceType, got)
	}
	for header, want := range map[string]string{
		"Access-Control-Allow-Origin":   configuredOrigin,
		"Access-Control-Allow-Methods":  http.MethodPut,
		"Access-Control-Allow-Headers":  "x-amz-meta-foo",
		"Access-Control-Max-Age":        "3000",
		"Access-Control-Expose-Headers": "ETag",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s: expected %s to be %q during the load window, got %q", instanceType, header, want, got)
		}
	}

	// The same bucket, an origin its rules exclude and the global setting would
	// allow. This is the leak: it must be refused, with no allow header at all.
	rec = preflight(bucketName, excludedOrigin)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the stored rules to refuse the excluded origin with %d, got %d",
			instanceType, http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("%s: expected the excluded origin to be refused by the stored rules, the server-wide handler answered it with Access-Control-Allow-Credentials %q",
			instanceType, got)
	}
	for _, header := range corsPreflightAllowHeaders {
		if got := rec.Header().Values(header); len(got) != 0 {
			t.Fatalf("%s: expected the excluded origin to receive no %s, got %v", instanceType, header, got)
		}
	}

	// A bucket that exists and has no configuration keeps the answer it has always
	// been given, during the very same window: the server-wide 204.
	for _, bucket := range []string{unconfiguredBucket, getRandomBucketName()} {
		rec = preflight(bucket, excludedOrigin)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: expected the server-wide handler to answer for %s with %d during the load window, got %d",
				instanceType, bucket, http.StatusNoContent, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != excludedOrigin {
			t.Fatalf("%s: expected the server-wide handler to allow the origin %q for %s, got %q",
				instanceType, excludedOrigin, bucket, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("%s: expected the server-wide handler to answer for %s, its Access-Control-Allow-Credentials was %q",
				instanceType, bucket, got)
		}
	}

	// None of this may have grown the metadata cache: the reads are made for one
	// request and left behind with it, so a preflight naming buckets that do not
	// exist cannot accumulate anything.
	if sys.Count() != 0 {
		t.Fatalf("%s: expected the load-window reads to add no metadata entries, got %d",
			instanceType, sys.Count())
	}
}

// installTestCORSPreflightReporter replaces the reporter the preflight evaluator
// reports its refusals through with a fresh one, restoring the previous value
// afterwards, so a test observes only the refusals it causes.
func installTestCORSPreflightReporter(t *testing.T) *corsPreflightRefusalReporter {
	t.Helper()

	saved := corsPreflightRefused
	t.Cleanup(func() { corsPreflightRefused = saved })

	corsPreflightRefused = &corsPreflightRefusalReporter{}
	return corsPreflightRefused
}

// corsPreflightReporterState reads a reporter's state under its own lock, which
// is what makes it safe to inspect after concurrent reports.
func corsPreflightReporterState(t *testing.T, r *corsPreflightRefusalReporter) (reportedAt time.Time, unreported uint64) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reportedAt, r.unreported
}

// TestCorsPreflightRefusalReporterRate pins the rate at which the evaluator
// reports a refusal that the bucket's own rules did not produce.
//
// The refusal that follows such a report is answered to an unauthenticated
// request, so the rate has to be the server's choice rather than the client's:
// the first refusal is reported at once, every refusal inside the interval that
// follows is counted rather than reported, and the next report carries that count
// so the condition cannot read as rarer than it was. One reporter serves every
// such reason, so a client cannot raise the volume by alternating between them
// either.
func TestCorsPreflightRefusalReporterRate(t *testing.T) {
	start := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	reporter := &corsPreflightRefusalReporter{}

	if unreported, report := reporter.due(start); !report || unreported != 0 {
		t.Fatalf("expected the first refusal to be reported with no backlog, got (%d, %t)", unreported, report)
	}

	// Everything inside the interval is counted instead, including a refusal one
	// nanosecond short of it.
	for i, at := range []time.Time{
		start,
		start.Add(time.Nanosecond),
		start.Add(corsPreflightRefusalReportInterval / 2),
		start.Add(corsPreflightRefusalReportInterval - time.Nanosecond),
	} {
		if unreported, report := reporter.due(at); report || unreported != 0 {
			t.Fatalf("expected refusal %d inside the interval to be counted, it was reported with (%d, %t)",
				i, unreported, report)
		}
	}
	if _, unreported := corsPreflightReporterState(t, reporter); unreported != 4 {
		t.Fatalf("expected 4 counted refusals, got %d", unreported)
	}

	elapsed := start.Add(corsPreflightRefusalReportInterval)
	if unreported, report := reporter.due(elapsed); !report || unreported != 4 {
		t.Fatalf("expected the refusal at the interval to be reported with a backlog of 4, got (%d, %t)",
			unreported, report)
	}
	if reportedAt, unreported := corsPreflightReporterState(t, reporter); unreported != 0 || !reportedAt.Equal(elapsed) {
		t.Fatalf("expected the report to reset the backlog and stamp %s, got %d and %s",
			elapsed, unreported, reportedAt)
	}
	if unreported, report := reporter.due(elapsed.Add(time.Nanosecond)); report || unreported != 0 {
		t.Fatalf("expected the interval to restart from the report, got (%d, %t)", unreported, report)
	}
}

// TestCorsPreflightRefusalReportCarriesNoClientData pins what a refusal report is
// allowed to say.
//
// A preflight is unauthenticated, so every value it carries is the client's: the
// Host it was addressed to, the origin it declares, the headers it asks about. The
// Host in particular is only bounded by the maximum header size, and the parse
// failure it produces quotes it back, so composing the report from that failure
// would put an arbitrary client string - newlines and all - into the server's log.
// The reason reported for such a refusal is therefore a fixed sentence, and the
// only value ever interpolated into a reason is a bucket name the evaluator has
// already validated as one.
func TestCorsPreflightRefusalReportCarriesNoClientData(t *testing.T) {
	const bucket = "blitzy-cors-bucket"

	// Fragments of the hosts, origins and header names a client can choose. None
	// may appear in a report, and the parse failures xnet.ParseHost produces for
	// the first two do quote them.
	clientChosen := []string{
		"a_b.example", "]bad[", ":::", ":notaport", "99999",
		"https://www.example1.com", "x-amz-blitzy",
	}

	reasons := []struct {
		name   string
		reason error
	}{
		{name: "aHostTheServerDoesNotAccept", reason: errCORSPreflightHostNotAccepted},
		{name: "aRequestCarryingTooManyHeaderBytes", reason: errCORSPreflightHeadersTooLarge},
		{
			name:   "rulesThatCouldNotBeEvaluatedInFull",
			reason: fmt.Errorf("for bucket %s, %w", bucket, errCORSPreflightEvaluationTooCostly),
		},
	}

	for _, tc := range reasons {
		t.Run(tc.name, func(t *testing.T) {
			report := corsPreflightRefusalReport(tc.reason, 0).Error()

			for _, fragment := range clientChosen {
				if strings.Contains(report, fragment) {
					t.Errorf("expected the report not to carry the client-chosen %q, got %q", fragment, report)
				}
			}
			if !strings.Contains(report, "Refused a CORS preflight request") {
				t.Errorf("expected the report to name the condition, got %q", report)
			}
			if strings.ContainsAny(report, "\n\r") {
				t.Errorf("expected the report to occupy a single log line, got %q", report)
			}

			// A backlog is the one number the report adds, and it is the server's
			// own count rather than anything the request carried.
			withBacklog := corsPreflightRefusalReport(tc.reason, 7).Error()
			if !strings.Contains(withBacklog, "7 further refusal(s) went unreported") {
				t.Errorf("expected a report to carry its backlog, got %q", withBacklog)
			}
			if strings.Contains(report, "went unreported") {
				t.Errorf("expected a report with no backlog not to mention one, got %q", report)
			}
		})
	}

	// The reason a refused Host is reported through must not be built from the
	// Host, so it carries no formatting placeholder that one could be spliced
	// into either.
	if strings.Contains(errCORSPreflightHostNotAccepted.Error(), "%") {
		t.Errorf("expected the Host refusal reason to be a fixed sentence, got %q", errCORSPreflightHostNotAccepted)
	}
}

// TestCorsPreflightRefusalReporterConcurrent pins the reporter's accounting under
// the concurrency it actually meets, since every refusal it reports is observed
// on a request goroutine.
//
// The reasons are deliberately mixed, because they share one reporter precisely so
// that a client cannot multiply the volume a deployment logs by alternating
// between them: whichever reasons arrive, exactly one report leaves the interval.
func TestCorsPreflightRefusalReporterConcurrent(t *testing.T) {
	const refusals = 256

	reporter := &corsPreflightRefusalReporter{}
	reasons := []error{
		fmt.Errorf("for bucket %s, %w", "blitzy-cors-bucket", errCORSPreflightEvaluationTooCostly),
		errCORSPreflightHostNotAccepted,
	}

	var wg sync.WaitGroup
	for i := range refusals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// report is the production entry point, so this also exercises the
			// message it builds - no log target is configured under test, so
			// nothing is emitted.
			reporter.report(reasons[i%len(reasons)])
		}()
	}
	wg.Wait()

	reportedAt, unreported := corsPreflightReporterState(t, reporter)
	if reportedAt.IsZero() {
		t.Fatal("expected one of the concurrent refusals to have been reported")
	}
	if unreported != refusals-1 {
		t.Fatalf("expected %d of %d concurrent refusals to be counted, got %d",
			refusals-1, refusals, unreported)
	}
}

// corsCapturingLogTarget is a logger target that records the entries sent to it,
// so a test can assert on the shape of what a refusal report emits rather than
// only on the text it composes.
//
// It satisfies the target contract without doing any of the work a real target
// does: Send never fails, so nothing is ever re-routed to the console, and once a
// test releases it the target stops recording, because targets can be added to the
// logger but not removed and this one outlives the test that installed it.
type corsCapturingLogTarget struct {
	mu        sync.Mutex
	recording bool
	entries   []log.Entry
}

func (t *corsCapturingLogTarget) String() string           { return "blitzy-cors-capture" }
func (t *corsCapturingLogTarget) Endpoint() string         { return "" }
func (t *corsCapturingLogTarget) Stats() types.TargetStats { return types.TargetStats{} }
func (t *corsCapturingLogTarget) Init(context.Context) error {
	return nil
}

func (t *corsCapturingLogTarget) IsOnline(context.Context) bool { return true }
func (t *corsCapturingLogTarget) Cancel()                       {}
func (t *corsCapturingLogTarget) Type() types.TargetType        { return types.TargetHTTP }

func (t *corsCapturingLogTarget) Send(_ context.Context, entry any) error {
	e, ok := entry.(log.Entry)
	if !ok {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recording {
		t.entries = append(t.entries, e)
	}
	return nil
}

func (t *corsCapturingLogTarget) captured() []log.Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.entries)
}

// installTestCORSLogCapture makes the entries a refusal report emits observable.
//
// The test binary disables logging wholesale, so it is re-enabled for the duration
// of one test and restored afterwards, and the capturing target is released at the
// same time so that no other test's logging is recorded.
func installTestCORSLogCapture(t *testing.T) *corsCapturingLogTarget {
	t.Helper()

	target := &corsCapturingLogTarget{recording: true}
	if err := logger.AddSystemTarget(context.Background(), target); err != nil {
		t.Fatalf("failed to install the capturing log target: %v", err)
	}

	disabled := logger.DisableLog
	logger.DisableLog = false
	t.Cleanup(func() {
		logger.DisableLog = disabled
		target.mu.Lock()
		target.recording = false
		target.mu.Unlock()
	})

	return target
}

// TestCorsPreflightRefusalReportCarriesNoStackTrace pins the logging path a
// refusal report is emitted through.
//
// A refusal is a condition this layer expects and handles, not a fault to be
// traced back to a line of code, and every refusal is produced from the same three
// call sites - so frames naming them describe the logger and net/http rather than
// anything an operator can act on. The entry therefore has to carry the reason and
// no trace at all. That is a property of the emission path and not of the message,
// so it is asserted on the entry itself: an error-shaped emission would attach a
// trace and would be caught here even though the text was unchanged.
func TestCorsPreflightRefusalReportCarriesNoStackTrace(t *testing.T) {
	target := installTestCORSLogCapture(t)
	reporter := installTestCORSPreflightReporter(t)

	reporter.report(fmt.Errorf("for bucket %s, %w", "blitzy-cors-bucket",
		errCORSPreflightHeadersTooLarge))

	entries := target.captured()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one entry to be emitted, got %d: %+v", len(entries), entries)
	}
	entry := entries[0]

	if entry.Trace != nil {
		t.Fatalf("expected no trace to be attached, got message %q with %d source frame(s): %v",
			entry.Trace.Message, len(entry.Trace.Source), entry.Trace.Source)
	}
	if entry.Level != logger.EventKind {
		t.Fatalf("expected the entry to be emitted as %q, got %q", logger.EventKind, entry.Level)
	}

	// The reason has to survive, including the bucket it concerns and the count of
	// refusals folded into this report, or the entry says less than the refusal did.
	for _, want := range []string{
		"Refused a CORS preflight request",
		"blitzy-cors-bucket",
		errCORSPreflightHeadersTooLarge.Error(),
	} {
		if !strings.Contains(entry.Message, want) {
			t.Fatalf("expected the message to contain %q, got %q", want, entry.Message)
		}
	}

	// Nothing about the internals that produced the refusal belongs in the entry,
	// whether as a trace or folded into the message itself.
	for _, unwanted := range []string{
		".go:", "cmd.", "logger.", "net/http", "bucket-cors",
	} {
		if strings.Contains(entry.Message, unwanted) {
			t.Fatalf("expected the message to name no source location, %q contains %q",
				entry.Message, unwanted)
		}
	}
}

// TestCorsAuditLogFilterKeys pins the request keys a CORS audit entry drops.
//
// An audit entry records a request verbatim, so a signed request's signature is
// recorded with it, and a signature replayed inside its expiry window
// re-authenticates the operation it covers. The keys below are the only ones a
// client can authenticate a request with, so the set has to hold all of them - and
// each has to be spelled the one way that authenticates, since the header names are
// canonicalized by net/http and the presigned forms are read case-sensitively out
// of the query string.
func TestCorsAuditLogFilterKeys(t *testing.T) {
	want := []string{
		"Authorization",
		"X-Amz-Signature",
		"Signature",
		"X-Amz-Security-Token",
	}
	if !slices.Equal(corsAuditLogFilterKeys, want) {
		t.Fatalf("expected the filtered keys to be %v, got %v", want, corsAuditLogFilterKeys)
	}

	// Spelled through the constants the signature code itself reads them by, so a
	// rename on either side cannot silently stop a key from being filtered.
	for i, canonical := range []string{
		xhttp.Authorization,
		xhttp.AmzSignature,
		xhttp.AmzSignatureV2,
		xhttp.AmzSecurityToken,
	} {
		if corsAuditLogFilterKeys[i] != canonical {
			t.Fatalf("expected filtered key %d to be %q, got %q",
				i, canonical, corsAuditLogFilterKeys[i])
		}
	}

	// A header name is canonicalized before it reaches an entry, so the filtered
	// spelling has to be the canonical one or the key is never matched.
	for _, name := range []string{xhttp.Authorization, xhttp.AmzSecurityToken} {
		if got := http.CanonicalHeaderKey(name); got != name {
			t.Fatalf("expected %q to already be the canonical header key, got %q", name, got)
		}
	}

	// Every CORS handler filters, not just the one that writes a configuration:
	// reading one and removing one are signed the same way and are recorded the
	// same way.
	handlers := map[string]http.HandlerFunc{
		"PutBucketCors":    objectAPIHandlers{}.PutBucketCorsHandler,
		"GetBucketCors":    objectAPIHandlers{}.GetBucketCorsHandler,
		"DeleteBucketCors": objectAPIHandlers{}.DeleteBucketCorsHandler,
	}
	for name, handler := range handlers {
		if handler == nil {
			t.Fatalf("expected %s to be registered", name)
		}
	}
}

// TestBucketCorsPreflightMiddlewareReportsItsOwnRefusals pins which refusals the
// evaluator reports.
//
// A refusal carries no Access-Control-Allow-* header whether the bucket's rules
// were read and allow nothing or the request was turned away before they were
// consulted, and a preflight is answered ahead of the router, so no audit entry,
// trace or metric distinguishes the two. The report is the only thing that does,
// and it therefore has to be emitted for exactly the refusals this layer produces
// on its own account: a request the server would refuse outright, and rules it
// could not finish evaluating. A refusal by the rules themselves is the
// configuration working as written and is not reported, and neither is a request
// handed to the server-wide handler.
func TestBucketCorsPreflightMiddlewareReportsItsOwnRefusals(t *testing.T) {
	const (
		bucket = "blitzy-cors-bucket"
		origin = "https://www.example1.com"
	)

	// Rules that would allow the probe below, so that a case which is refused all
	// the same is refused for the reason it is testing rather than for want of a
	// matching rule.
	allowEverything := corsTestConfig(miniogocors.Rule{
		ID:            "allow-everything",
		AllowedHeader: []string{"*"},
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"*"},
	})

	testCases := []struct {
		name  string
		setup func(t *testing.T)
		// request mutates the probe, which is otherwise a preflight the rules
		// above allow.
		request      func(req *http.Request)
		want         corsPreflightOutcome
		wantReported bool
	}{
		{
			// The server answers 400 for this Host, so the request the preflight
			// asks about could never be served. The refusal says something about
			// how this request was handled rather than about the bucket, which is
			// what makes it worth an operator's attention.
			name: "aHostTheServerDoesNotAcceptIsReported",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, allowEverything)
			},
			request:      func(req *http.Request) { req.Host = "a_b.example" },
			want:         corsOutcomeDenied,
			wantReported: true,
		},
		{
			// Likewise for a request carrying more header bytes than any MinIO
			// request may: it is refused before those headers are read.
			name: "aRequestCarryingTooManyHeaderBytesIsReported",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, allowEverything)
			},
			request: func(req *http.Request) {
				req.Header.Set("Access-Control-Request-Headers",
					strings.Join(corsRequestedHeaderNames(4096), ","))
			},
			want:         corsOutcomeDenied,
			wantReported: true,
		},
		{
			// The bucket's rules were read and none of them allows the request.
			// That is the configuration doing its job, so reporting it would
			// report normal operation as a fault.
			name: "refusalByTheRulesIsNotReported",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, corsTestConfig(
					miniogocors.Rule{
						AllowedMethod: []string{http.MethodGet},
						AllowedOrigin: []string{"https://www.example9.com"},
					},
				))
			},
			want:         corsOutcomeDenied,
			wantReported: false,
		},
		{
			// The bucket has no rules of its own, so the request is the
			// server-wide handler's to answer and there is nothing to report.
			name: "delegationIsNotReported",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, nil)
			},
			want:         corsOutcomeDelegated,
			wantReported: false,
		},
		{
			// Neither is the delegation of a bucket a loaded cache does not hold:
			// that bucket demonstrably has no configuration of its own, which is
			// the ordinary fallback rather than a condition.
			name: "delegationOfABucketTheLoadedCacheDoesNotHoldIsNotReported",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t)
			},
			want:         corsOutcomeDelegated,
			wantReported: false,
		},
		{
			// A configuration that could not be established at all is a different
			// matter, and is exactly what an operator needs told: the request was
			// refused without the bucket's rules ever being read. Here there is no
			// metadata subsystem to read them through.
			name: "aConfigurationThatCouldNotBeEstablishedIsReported",
			setup: func(t *testing.T) {
				t.Helper()
				saved := globalBucketMetadataSys
				t.Cleanup(func() { globalBucketMetadataSys = saved })
				globalBucketMetadataSys = nil
			},
			want:         corsOutcomeDenied,
			wantReported: true,
		},
		{
			// The same report for the same reason reached the other way: the cache
			// is still loading and there is no object layer to establish this
			// bucket's configuration from.
			name: "aBucketMissedInALoadingCacheIsReported",
			setup: func(t *testing.T) {
				t.Helper()
				installLoadingTestBucketMetadataSys(t)
				installTestNoObjectLayer(t)
			},
			want:         corsOutcomeDenied,
			wantReported: true,
		},
		{
			// A rule matched, so there is nothing to report either.
			name: "anAllowedRequestIsNotReported",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t), bucket, allowEverything)
			},
			want:         corsOutcomeAllowed,
			wantReported: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			reporter := installTestCORSPreflightReporter(t)

			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
			})
			middleware := bucketCORSPreflightMiddleware(next)

			// Three identical requests, so that a report per request would be
			// visible as a backlog that never accumulates.
			const requests = 3
			for range requests {
				req := httptest.NewRequest(http.MethodOptions, "/"+bucket+"/object", nil)
				req.Header.Set("Origin", origin)
				req.Header.Set("Access-Control-Request-Method", http.MethodPut)
				if tc.request != nil {
					tc.request(req)
				}
				rec := httptest.NewRecorder()

				middleware.ServeHTTP(rec, req)

				got := corsOutcomeDenied
				switch {
				case delegated:
					got = corsOutcomeDelegated
				case rec.Header().Get("Access-Control-Allow-Origin") != "":
					got = corsOutcomeAllowed
				}
				if got != tc.want {
					t.Fatalf("expected the request to be %s, it was %s (status %d, headers %v)",
						tc.want, got, rec.Code, rec.Header())
				}
				if tc.want == corsOutcomeDenied {
					for _, name := range corsPreflightAllowHeaders {
						if value := rec.Header().Get(name); value != "" {
							t.Fatalf("expected a refused preflight to carry no %s, got %q", name, value)
						}
					}
				}
				delegated = false
			}

			reportedAt, unreported := corsPreflightReporterState(t, reporter)
			if !tc.wantReported {
				if !reportedAt.IsZero() || unreported != 0 {
					t.Fatalf("expected a %s request not to be reported, it was reported at %s with %d counted",
						tc.want, reportedAt, unreported)
				}
				return
			}
			if reportedAt.IsZero() {
				t.Fatal("expected the first refusal to be reported")
			}
			// The first refusal was reported and the two that followed it were
			// counted, which is the rate limit holding rather than the report
			// being emitted per request.
			if unreported != requests-1 {
				t.Fatalf("expected %d of %d refusals to be counted rather than reported, got %d",
					requests-1, requests, unreported)
			}
		})
	}
}

// corsMetadataUpdatedAt is a fixed instant carrying nanoseconds. MessagePack
// stores a time with nanosecond precision and normalizes it to UTC, so using a
// value with a sub-second component proves the codec keeps the whole timestamp
// instead of truncating it.
var corsMetadataUpdatedAt = time.Date(2025, time.July, 4, 12, 30, 45, 123456789, time.UTC)

// TestBucketMetadataCORSConfigRoundTrip verifies the CORS payload and nanosecond
// timestamp survive both MessagePack codec paths, including a cleared payload.
func TestBucketMetadataCORSConfigRoundTrip(t *testing.T) {
	created := time.Date(2025, time.June, 1, 8, 0, 0, 0, time.UTC)

	meta := newBucketMetadata("blitzy-cors-round-trip")
	meta.Created = created
	meta.CORSConfigXML = []byte(corsCanonicalDocument)
	meta.CORSConfigUpdatedAt = corsMetadataUpdatedAt

	assertRoundTrip := func(t *testing.T, decoded BucketMetadata) {
		t.Helper()
		if decoded.Name != meta.Name {
			t.Errorf("expected Name %q, got %q", meta.Name, decoded.Name)
		}
		if !decoded.Created.Equal(created) {
			t.Errorf("expected Created %s, got %s", created, decoded.Created)
		}
		if !bytes.Equal(decoded.CORSConfigXML, meta.CORSConfigXML) {
			t.Errorf("expected CORSConfigXML %q, got %q",
				truncateForError(string(meta.CORSConfigXML)), truncateForError(string(decoded.CORSConfigXML)))
		}
		if !decoded.CORSConfigUpdatedAt.Equal(corsMetadataUpdatedAt) {
			t.Errorf("expected CORSConfigUpdatedAt %s, got %s", corsMetadataUpdatedAt, decoded.CORSConfigUpdatedAt)
		}
	}

	t.Run("marshalUnmarshal", func(t *testing.T) {
		data, err := meta.MarshalMsg(nil)
		if err != nil {
			t.Fatalf("marshaling the metadata must succeed, got error: %v", err)
		}

		var decoded BucketMetadata
		left, err := decoded.UnmarshalMsg(data)
		if err != nil {
			t.Fatalf("unmarshaling the metadata must succeed, got error: %v", err)
		}
		if len(left) > 0 {
			t.Errorf("expected the whole payload to be consumed, %d bytes left over", len(left))
		}
		assertRoundTrip(t, decoded)
	})

	t.Run("encodeDecode", func(t *testing.T) {
		var buf bytes.Buffer
		writer := msgp.NewWriter(&buf)
		if err := meta.EncodeMsg(writer); err != nil {
			t.Fatalf("encoding the metadata must succeed, got error: %v", err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatalf("flushing the encoded metadata must succeed, got error: %v", err)
		}

		var decoded BucketMetadata
		if err := decoded.DecodeMsg(msgp.NewReader(&buf)); err != nil {
			t.Fatalf("decoding the metadata must succeed, got error: %v", err)
		}
		assertRoundTrip(t, decoded)
	})

	t.Run("clearedPayloadRoundTripsAsCleared", func(t *testing.T) {
		// A bucket whose CORS configuration was deleted carries no payload, and
		// that absence has to survive the round trip as an absence too, while the
		// timestamp recording the deletion is still carried.
		cleared := meta
		cleared.CORSConfigXML = nil

		data, err := cleared.MarshalMsg(nil)
		if err != nil {
			t.Fatalf("marshaling the metadata must succeed, got error: %v", err)
		}

		var decoded BucketMetadata
		if _, err := decoded.UnmarshalMsg(data); err != nil {
			t.Fatalf("unmarshaling the metadata must succeed, got error: %v", err)
		}
		if len(decoded.CORSConfigXML) != 0 {
			t.Errorf("expected no CORS payload, got %q", truncateForError(string(decoded.CORSConfigXML)))
		}
		if !decoded.CORSConfigUpdatedAt.Equal(corsMetadataUpdatedAt) {
			t.Errorf("expected CORSConfigUpdatedAt %s, got %s", corsMetadataUpdatedAt, decoded.CORSConfigUpdatedAt)
		}
	})
}

// TestBucketMetadataParseCORSConfig proves parseAllConfigs turns the persisted
// payload into the parsed configuration that readers are served, and clears that
// configuration again once the payload is removed.
//
// Both directions matter: the populated branch is what makes a stored
// configuration observable, and the nil branch is what makes a DELETE observable,
// since without it a deleted configuration would keep answering preflight
// requests from a stale pointer.
func TestBucketMetadataParseCORSConfig(t *testing.T) {
	meta := newBucketMetadata("blitzy-cors-parse")

	t.Run("payloadIsParsedIntoTheConfiguration", func(t *testing.T) {
		meta.CORSConfigXML = []byte(corsCanonicalDocument)
		if err := meta.parseAllConfigs(t.Context(), nil); err != nil {
			t.Fatalf("parsing the bucket configurations must succeed, got error: %v", err)
		}

		if meta.corsConfig == nil {
			t.Fatal("expected the parsed CORS configuration to be populated")
		}
		if len(meta.corsConfig.CORSRules) != 4 {
			t.Fatalf("expected %d rules, got %d", 4, len(meta.corsConfig.CORSRules))
		}
		if meta.corsConfig.XMLNS != s3CORSNamespace {
			t.Errorf("expected namespace %q, got %q", s3CORSNamespace, meta.corsConfig.XMLNS)
		}
		wantFirstRule := miniogocors.Rule{
			AllowedHeader: []string{"*"},
			AllowedMethod: []string{http.MethodPut, http.MethodPost, http.MethodDelete},
			AllowedOrigin: []string{"http://www.example1.com"},
		}
		if !reflect.DeepEqual(meta.corsConfig.CORSRules[0], wantFirstRule) {
			t.Errorf("expected the first rule to be %+v, got %+v", wantFirstRule, meta.corsConfig.CORSRules[0])
		}
	})

	t.Run("removingThePayloadClearsTheConfiguration", func(t *testing.T) {
		meta.CORSConfigXML = nil
		if err := meta.parseAllConfigs(t.Context(), nil); err != nil {
			t.Fatalf("parsing the bucket configurations must succeed, got error: %v", err)
		}
		if meta.corsConfig != nil {
			t.Fatalf("expected the parsed CORS configuration to be cleared, got %+v", meta.corsConfig)
		}
	})

	t.Run("malformedPayloadIsReported", func(t *testing.T) {
		broken := newBucketMetadata("blitzy-cors-parse-malformed")
		broken.CORSConfigXML = []byte(`<CORSConfiguration><CORSRule>`)
		if err := broken.parseAllConfigs(t.Context(), nil); err == nil {
			t.Fatal("expected a malformed stored payload to be reported as an error")
		}
	})
}

// TestBucketMetadataLastUpdateCORS proves lastUpdate accounts for the CORS
// timestamp. That value is how the rest of the cluster learns its cached bucket
// metadata is stale, so a timestamp left out of the comparison would let a CORS
// change go unnoticed.
func TestBucketMetadataLastUpdateCORS(t *testing.T) {
	base := time.Date(2025, time.July, 4, 12, 0, 0, 0, time.UTC)
	earlier, later := base.Add(-time.Hour), base.Add(time.Hour)

	testCases := []struct {
		name string
		meta BucketMetadata
		want time.Time
	}{
		{
			name: "corsIsTheOnlyTimestamp",
			meta: BucketMetadata{CORSConfigUpdatedAt: base},
			want: base,
		},
		{
			name: "corsIsTheLatestTimestamp",
			meta: BucketMetadata{
				PolicyConfigUpdatedAt:  earlier,
				TaggingConfigUpdatedAt: base,
				CORSConfigUpdatedAt:    later,
			},
			want: later,
		},
		{
			name: "aSiblingTimestampIsLaterThanCors",
			meta: BucketMetadata{
				CORSConfigUpdatedAt:        base,
				ReplicationConfigUpdatedAt: later,
			},
			want: later,
		},
		{
			name: "noTimestampIsSet",
			meta: BucketMetadata{},
			want: time.Time{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.meta.lastUpdate(); !got.Equal(tc.want) {
				t.Fatalf("expected the last update to be %s, got %s", tc.want, got)
			}
		})
	}
}

// TestBucketMetadataDefaultTimestampsCORS proves defaultTimestamps back-fills an
// unset CORS timestamp from the bucket creation time and leaves a set one alone.
//
// Metadata written before this field existed decodes with a zero timestamp, and
// reporting that zero value as the configuration's age is exactly what the
// back-fill exists to prevent.
func TestBucketMetadataDefaultTimestampsCORS(t *testing.T) {
	created := time.Date(2025, time.June, 1, 8, 0, 0, 0, time.UTC)

	t.Run("unsetTimestampIsBackFilledFromCreated", func(t *testing.T) {
		meta := newBucketMetadata("blitzy-cors-timestamps")
		meta.Created = created

		meta.defaultTimestamps()

		if !meta.CORSConfigUpdatedAt.Equal(created) {
			t.Fatalf("expected the CORS timestamp to default to %s, got %s", created, meta.CORSConfigUpdatedAt)
		}
	})

	t.Run("setTimestampIsPreserved", func(t *testing.T) {
		meta := newBucketMetadata("blitzy-cors-timestamps")
		meta.Created = created
		meta.CORSConfigUpdatedAt = corsMetadataUpdatedAt

		meta.defaultTimestamps()

		if !meta.CORSConfigUpdatedAt.Equal(corsMetadataUpdatedAt) {
			t.Fatalf("expected the CORS timestamp to stay %s, got %s", corsMetadataUpdatedAt, meta.CORSConfigUpdatedAt)
		}
	})
}

// TestCorsConfigAPIError pins the S3 error the PUT handler answers with for each
// way a CORS document can fail to be accepted.
//
// The distinction the table draws is the point of the classifier. A document that
// really did not validate answers MalformedXML and carries the specific cause, so
// the client is told which rule and which value to correct. A body that failed its
// own integrity check never got as far as validating: the body the PUT handler
// reads verifies the Content-MD5, x-amz-content-sha256 and x-amz-checksum-* values
// the client declared while it is consumed - the last of them installed by
// corsConfigBody rather than by any authentication path - so those mismatches
// surface from the read of a document that may be perfectly well-formed. Answering
// MalformedXML for them would send the client looking for an XML fault that does
// not exist while the digest it declared stays wrong, and it would make an
// integrity failure indistinguishable from a schema violation on the wire.
//
// A body that simply stopped early is the third case, and it is a client fault
// with an S3 code of its own: IncompleteBody, HTTP 400. Only a failure that says
// nothing about the client - an unknown transport error - is reported as a server
// error, so a truncated upload is never answered with a 500 that invites the
// client to retry against a server that was working correctly.
func TestCorsConfigAPIError(t *testing.T) {
	ctx := t.Context()

	schemaErr := errors.New(`CORSRule 0 has unsupported AllowedMethod "PATCH"`)
	transportErr := errors.New("connection reset by peer")

	testCases := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
		// wantDescription is asserted only when set, because the descriptions of
		// the registered codes are fixed strings this test has no business
		// restating - only the MalformedXML path substitutes a description.
		wantDescription string
	}{
		{
			name:            "aDocumentThatDidNotValidateCarriesItsCause",
			err:             schemaErr,
			wantCode:        "MalformedXML",
			wantStatus:      http.StatusBadRequest,
			wantDescription: schemaErr.Error(),
		},
		{
			name: "aContentMD5MismatchIsBadDigest",
			err: corsConfigReadError{cause: hash.BadDigest{
				ExpectedMD5:   "5eb63bbbe01eeed093cb22bb8f5acdc3",
				CalculatedMD5: "6f5902ac237024bdd0c176cb93063dc4",
			}},
			wantCode:   "BadDigest",
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "aContentSHA256MismatchKeepsItsOwnCode",
			err: corsConfigReadError{cause: hash.SHA256Mismatch{
				ExpectedSHA256:   "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
				CalculatedSHA256: "486ea46224d1bb4fb680f34f7c9ad96a8f24ec88be73ea8e5a6c65260e9cb8a7",
			}},
			wantCode:   "XAmzContentSHA256Mismatch",
			wantStatus: http.StatusBadRequest,
		},
		{
			// The classifier defers to toAPIError instead of enumerating the
			// integrity codes itself, and this is one the route provokes for
			// itself: a checksum is verified because corsConfigBody adds it to
			// the reader, not because any authentication path does.
			// TestPutBucketCorsHandler drives it end to end - both a value that
			// does not match the body and one promised in a trailer that never
			// arrives - so what this case pins is the code such a failure keeps on
			// its way back out through the classifier.
			name: "aTrailingChecksumMismatchKeepsItsOwnCode",
			err: corsConfigReadError{cause: hash.ChecksumMismatch{
				Want: "kAFQmDzST7DWlj99KOF/cg==",
				Got:  "n0jUY0F+Wg1nxRTiVzZUvw==",
			}},
			wantCode:   "XAmzContentChecksumMismatch",
			wantStatus: http.StatusBadRequest,
		},
		{
			// The classifier looks through the chain rather than at the outermost
			// error, so a read failure stays attributable however it is wrapped
			// on its way back to the handler.
			name: "aWrappedIntegrityFailureIsStillClassified",
			err: fmt.Errorf("putting the CORS configuration: %w",
				corsConfigReadError{cause: hash.BadDigest{ExpectedMD5: "expected", CalculatedMD5: "calculated"}}),
			wantCode:   "BadDigest",
			wantStatus: http.StatusBadRequest,
		},
		{
			// A client that sends fewer bytes than it said it would is not a
			// client that sent bad XML either, and S3 has a code that says
			// exactly what went wrong: the body was incomplete. Reporting it as a
			// server error would blame the server for a request the client cut
			// short, and reporting it as MalformedXML would send the client
			// looking for a schema fault in a document it never finished sending.
			name:       "aBodyThatEndedEarlyIsAnIncompleteBody",
			err:        corsConfigReadError{cause: io.ErrUnexpectedEOF},
			wantCode:   "IncompleteBody",
			wantStatus: http.StatusBadRequest,
		},
		{
			// The transport wraps that condition on its way up in some cases, so
			// the classification has to look through the chain here as well.
			name: "aWrappedEarlyEndIsStillAnIncompleteBody",
			err: corsConfigReadError{cause: fmt.Errorf("reading the request body: %w",
				io.ErrUnexpectedEOF)},
			wantCode:   "IncompleteBody",
			wantStatus: http.StatusBadRequest,
		},
		{
			// A failure neither the document nor the client's own declarations
			// account for stays a server error, which is what the sibling
			// configuration handlers report for one too. It is deliberately not
			// IncompleteBody: nothing here says the client sent too little.
			name:       "anUnknownTransportFailureIsAServerError",
			err:        corsConfigReadError{cause: transportErr},
			wantCode:   "InternalError",
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			apiErr := corsConfigAPIError(ctx, testCase.err)

			if apiErr.Code != testCase.wantCode {
				t.Fatalf("expected the code %q, got %q with description %q",
					testCase.wantCode, apiErr.Code, apiErr.Description)
			}
			if apiErr.HTTPStatusCode != testCase.wantStatus {
				t.Fatalf("expected status %d, got %d", testCase.wantStatus, apiErr.HTTPStatusCode)
			}
			if testCase.wantDescription != "" && apiErr.Description != testCase.wantDescription {
				t.Fatalf("expected the description %q, got %q", testCase.wantDescription, apiErr.Description)
			}
		})
	}
}

// TestCorsConfigReadError covers the type that carries a body read failure back
// to the handler. Both halves of it are load bearing: the message keeps naming
// the document so an operator reading a log still sees what was being read, and
// the wrapped cause is what lets the handler answer with the code the failure
// already has.
func TestCorsConfigReadError(t *testing.T) {
	cause := hash.BadDigest{ExpectedMD5: "expected", CalculatedMD5: "calculated"}
	err := corsConfigReadError{cause: cause}

	if want := "Unable to read the " + corsConfigurationElement + " document: " + cause.Error(); err.Error() != want {
		t.Fatalf("expected the message %q, got %q", want, err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatalf("expected the cause %v to be reachable through the chain, got %q", cause, err)
	}

	var unwrapped hash.BadDigest
	if !errors.As(err, &unwrapped) {
		t.Fatalf("expected the cause to unwrap to hash.BadDigest, got %q", err)
	}
	if unwrapped != cause {
		t.Fatalf("expected the unwrapped cause to be %+v, got %+v", cause, unwrapped)
	}
}

// TestCORSConfigBody covers the properties of corsConfigBody the routed tests
// cannot reach: a body that already verifies is handed over with its verification
// intact rather than replaced, and a declaration that is not a digest at all is
// not treated as one.
//
// The first is the presigned V4 case. Such a request declares its payload digest in
// the query string rather than in a header, and authentication has already
// installed a reader that verifies it. Wrapping that body a second time around a
// header derived digest would not add a check, it would take one away: merging an
// empty expectation into a hash.Reader overwrites the one it holds (see newReader
// in internal/hash), so a request whose digest lives in the query string would end
// up with no digest verification at all.
func TestCORSConfigBody(t *testing.T) {
	document := []byte(corsCanonicalDocument)
	corrupted := []byte("not the document that was declared")

	t.Run("aVerifyingBodyIsHandedOverUntouched", func(t *testing.T) {
		// The digest of the document the client promised, over bytes that are not
		// that document, so a verification that survived reports the mismatch and
		// one that was replaced reports nothing at all.
		installed, err := hash.NewReader(GlobalContext, bytes.NewReader(corrupted), -1,
			"", getSHA256Hash(document), -1)
		if err != nil {
			t.Fatalf("failed to install a verifying reader: %v", err)
		}

		// No digest header whatsoever, exactly as a presigned request arrives.
		request := httptest.NewRequest(http.MethodPut, "/blitzy-cors-config-body?cors", nil)
		request.Body = installed

		body, apiErr := corsConfigBody(GlobalContext, request)
		if apiErr != ErrNone {
			t.Fatalf("expected the body to be accepted, got the API error code %v", apiErr)
		}
		if reader, ok := body.(*hash.Reader); !ok || reader != installed {
			t.Fatalf("expected the reader authentication installed to be handed over, got %T", body)
		}

		var mismatch hash.SHA256Mismatch
		if _, err := io.ReadAll(body); !errors.As(err, &mismatch) {
			t.Fatalf("expected reading the body to report a SHA256 mismatch, got %v", err)
		}
	})

	t.Run("aPlainBodyIsGivenTheVerificationItsHeadersDeclare", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPut, "/blitzy-cors-config-body?cors",
			bytes.NewReader(corrupted))
		request.Header.Set(xhttp.ContentMD5, getMD5HashBase64(document))

		body, apiErr := corsConfigBody(GlobalContext, request)
		if apiErr != ErrNone {
			t.Fatalf("expected the body to be accepted, got the API error code %v", apiErr)
		}
		if body == io.Reader(request.Body) {
			t.Fatal("expected an unverified body to be wrapped, got the request body itself")
		}

		var badDigest hash.BadDigest
		if _, err := io.ReadAll(body); !errors.As(err, &badDigest) {
			t.Fatalf("expected reading the body to report a bad digest, got %v", err)
		}
	})

	t.Run("aChecksumIsVerifiedOnTopOfABodyThatAlreadyVerifies", func(t *testing.T) {
		// The checksum is the verification no authentication path installs, so it
		// has to be added to a body that already verifies as well as to one that
		// does not - otherwise every signed request would keep the gap.
		installed, err := hash.NewReader(GlobalContext, bytes.NewReader(document), -1,
			"", getSHA256Hash(document), -1)
		if err != nil {
			t.Fatalf("failed to install a verifying reader: %v", err)
		}

		request := httptest.NewRequest(http.MethodPut, "/blitzy-cors-config-body?cors", nil)
		request.Body = installed
		request.Header.Set(xhttp.AmzChecksumCRC32, hash.NewChecksumFromData(hash.ChecksumCRC32, corrupted).Encoded)

		body, apiErr := corsConfigBody(GlobalContext, request)
		if apiErr != ErrNone {
			t.Fatalf("expected the body to be accepted, got the API error code %v", apiErr)
		}

		var mismatch hash.ChecksumMismatch
		if _, err := io.ReadAll(body); !errors.As(err, &mismatch) {
			t.Fatalf("expected reading the body to report a checksum mismatch, got %v", err)
		}
	})

	t.Run("anUndecodableDeclarationIsRefusedBeforeTheBodyIsRead", func(t *testing.T) {
		testCases := []struct {
			name    string
			header  string
			value   string
			wantErr APIErrorCode
		}{
			{
				name:    "contentMD5",
				header:  xhttp.ContentMD5,
				value:   "this is not a base64 encoded md5",
				wantErr: ErrInvalidDigest,
			},
			{
				name:    "contentSHA256",
				header:  xhttp.AmzContentSha256,
				value:   "this is not a hex encoded sha256",
				wantErr: ErrContentSHA256Mismatch,
			},
			{
				name:    "checksum",
				header:  xhttp.AmzChecksumCRC32,
				value:   "this is not a base64 encoded crc32",
				wantErr: ErrInvalidChecksum,
			},
		}

		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPut, "/blitzy-cors-config-body?cors",
					bytes.NewReader(document))
				request.Header.Set(testCase.header, testCase.value)

				body, apiErr := corsConfigBody(GlobalContext, request)
				if apiErr != testCase.wantErr {
					t.Fatalf("expected the API error code %v, got %v", testCase.wantErr, apiErr)
				}
				if body != nil {
					t.Fatalf("expected no reader alongside a refusal, got %T", body)
				}
			})
		}
	})

	t.Run("whatS3DefinesInPlaceOfADigestIsNotADigest", func(t *testing.T) {
		// UNSIGNED-PAYLOAD and its trailing form are the values S3 defines for a
		// client that is declaring no payload digest at all, so neither may be
		// mistaken for a malformed one. skipContentSha256Cksum is what draws that
		// distinction for the whole server, and consulting it rather than reading
		// the header directly is what keeps this handler's answer to such a
		// declaration the same as every other handler's.
		//
		// The empty-payload digest is the one value whose treatment depends on how
		// the deployment was started, and this pins that this handler inherits the
		// decision rather than making one of its own. A client that sends a
		// non-empty body while declaring the SHA-256 of an empty one is broken; in
		// the default strict-compatibility mode the mismatch is reported, while a
		// deployment started with --no-compat has asked to tolerate exactly those
		// clients.
		testCases := []struct {
			name         string
			declared     string
			strict       bool
			wantVerified bool
		}{
			{name: "unsignedPayloadDeclaresNoDigest", declared: unsignedPayload, strict: true},
			{name: "aTrailingUnsignedPayloadDeclaresNoneEither", declared: unsignedPayloadTrailer, strict: true},
			{name: "anEmptyPayloadDigestIsVerifiedWhenStrict", declared: emptySHA256, strict: true, wantVerified: true},
			{name: "anEmptyPayloadDigestIsToleratedWithoutStrictCompatibility", declared: emptySHA256, strict: false},
		}

		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				strict := globalServerCtxt.StrictS3Compat
				t.Cleanup(func() { globalServerCtxt.StrictS3Compat = strict })
				globalServerCtxt.StrictS3Compat = testCase.strict

				// A non-empty body, so a declaration that is verified reports the
				// mismatch while one that is skipped reads through to its end. The
				// length is what the empty-payload case turns on, and
				// httptest.NewRequest declares it from the reader.
				request := httptest.NewRequest(http.MethodPut, "/blitzy-cors-config-body?cors",
					bytes.NewReader(document))
				request.Header.Set(xhttp.AmzContentSha256, testCase.declared)

				body, apiErr := corsConfigBody(GlobalContext, request)
				if apiErr != ErrNone {
					t.Fatalf("expected the body to be accepted, got the API error code %v", apiErr)
				}

				read, err := io.ReadAll(body)
				if testCase.wantVerified {
					var mismatch hash.SHA256Mismatch
					if !errors.As(err, &mismatch) {
						t.Fatalf("expected reading the body to report a SHA256 mismatch, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("expected the body to read through unverified, got %v", err)
				}
				if !bytes.Equal(read, document) {
					t.Fatalf("expected the document to be read back unchanged, got %q",
						truncateForError(string(read)))
				}
			})
		}
	})
}

// assertCORSHandlerError decodes the S3 XML error a CORS handler wrote and
// asserts its status line and code together. Either one alone is ambiguous: a
// 400 could be any rejection, and a MalformedXML body carrying a 200 would still
// be a broken answer.
func assertCORSHandlerError(t *testing.T, instanceType string, rec *httptest.ResponseRecorder,
	wantCode string, wantStatus int,
) APIErrorResponse {
	t.Helper()

	var errorResponse APIErrorResponse
	unmarshalErr := xml.Unmarshal(rec.Body.Bytes(), &errorResponse)

	// The status is reported before the body is insisted upon, because a handler
	// that succeeded where it should have refused answers with no body at all:
	// reporting that as an unparsable body would hide which of the two went
	// wrong.
	if rec.Code != wantStatus {
		t.Fatalf("%s: expected status %d, got %d with body %q",
			instanceType, wantStatus, rec.Code, truncateForError(rec.Body.String()))
	}
	if unmarshalErr != nil {
		t.Fatalf("%s: expected an S3 XML error body, got %q: %v",
			instanceType, truncateForError(rec.Body.String()), unmarshalErr)
	}
	if errorResponse.Code != wantCode {
		t.Fatalf("%s: expected error code %q, got %q with message %q",
			instanceType, wantCode, errorResponse.Code, errorResponse.Message)
	}
	return errorResponse
}

// newSignedCORSRequest builds a request the registered ?cors route matches,
// signed with the root credentials the harness handed over.
func newSignedCORSRequest(t *testing.T, method, bucketName string, body []byte, creds auth.Credentials) *http.Request {
	t.Helper()

	return newSignedCORSRequestWithHeaders(t, method, bucketName, body, creds, nil)
}

// newSignedCORSRequestWithHeaders builds the same request while overriding the
// given headers, which are applied before the request is signed so that the
// signature stays valid and the override is what the server acts upon.
//
// Overriding a digest header is how a body integrity failure is driven end to
// end: the harness derives a correct Content-Md5 and x-amz-content-sha256 from
// the body, so replacing one of them leaves a perfectly signed request whose
// declared digest does not match the bytes that follow.
func newSignedCORSRequestWithHeaders(t *testing.T, method, bucketName string, body []byte,
	creds auth.Credentials, headers map[string]string,
) *http.Request {
	t.Helper()

	var reader io.ReadSeeker
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := newTestSignedRequestV4(method, getBucketCORSURL("", bucketName),
		int64(len(body)), reader, creds.AccessKey, creds.SecretKey, headers)
	if err != nil {
		t.Fatalf("failed to build a signed %s ?cors request: %v", method, err)
	}
	return request
}

// newSignedV2CORSRequestWithHeaders builds the same request signed with
// signature version 2, overriding the given headers before it is signed.
//
// The signature version matters to body integrity rather than to routing. A V2
// signature covers the Content-Md5 header but never the bytes that header
// describes, and the V2 authentication path installs no verifying reader in place
// of the body, so the handler is the only place a declared digest can be checked.
func newSignedV2CORSRequestWithHeaders(t *testing.T, method, bucketName string, body []byte,
	creds auth.Credentials, headers map[string]string,
) *http.Request {
	t.Helper()

	var reader io.ReadSeeker
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := newTestSignedRequestV2(method, getBucketCORSURL("", bucketName),
		int64(len(body)), reader, creds.AccessKey, creds.SecretKey, headers)
	if err != nil {
		t.Fatalf("failed to build a V2 signed %s ?cors request: %v", method, err)
	}
	return request
}

// newAnonymousCORSRequest builds an unsigned request against the ?cors route.
// Carrying no Authorization header is what makes it anonymous, and an anonymous
// caller is authorized against the bucket policy rather than against the owner
// short circuit.
func newAnonymousCORSRequest(t *testing.T, method, bucketName string, body []byte) *http.Request {
	t.Helper()

	return newAnonymousCORSRequestWithHeaders(t, method, bucketName, body, nil)
}

// newAnonymousCORSRequestWithHeaders builds the same unsigned request while
// overriding the given headers.
//
// Nothing is signed, so an override is simply the value the server acts upon -
// which is exactly the point being asserted: an unsigned request declares things
// about its body that no signature and no authentication path verifies.
func newAnonymousCORSRequestWithHeaders(t *testing.T, method, bucketName string, body []byte,
	headers map[string]string,
) *http.Request {
	t.Helper()

	var reader io.ReadSeeker
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := newTestRequest(method, getBucketCORSURL("", bucketName), int64(len(body)), reader)
	if err != nil {
		t.Fatalf("failed to build an anonymous %s ?cors request: %v", method, err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

// grantAnonymousCORSAction stores a bucket policy granting the anonymous
// principal exactly one CORS action on the bucket, and returns the function that
// removes it again.
//
// This is what makes the unsigned write path reachable: without a policy an
// anonymous request is refused before the handler reads anything, so the only way
// to drive a body no signature covers all the way into the handler is to
// authorize it by policy.
func grantAnonymousCORSAction(t *testing.T, bucketName, action string) func() {
	t.Helper()

	if _, err := globalBucketMetadataSys.Update(GlobalContext, bucketName, bucketPolicyConfig,
		[]byte(corsAnonymousPolicy(bucketName, action))); err != nil {
		t.Fatalf("failed to grant the anonymous principal %s on bucket %s: %v", action, bucketName, err)
	}

	return func() {
		if _, err := globalBucketMetadataSys.Delete(GlobalContext, bucketName, bucketPolicyConfig); err != nil {
			t.Fatalf("failed to remove the bucket policy of %s: %v", bucketName, err)
		}
	}
}

// execCORSNilObjectLayerTest covers the branch every CORS handler evaluates
// first: an object layer that is not initialized yet.
//
// ExecObjectLayerAPINilTest nils the global object layer to reach that branch,
// so the caller has to invoke this last, and the layer is restored afterwards so
// that whatever runs next in this package still finds a usable one.
func execCORSNilObjectLayerTest(t *testing.T, instanceType, method string, apiRouter http.Handler) {
	t.Helper()

	// The bucket is never created: the nil check precedes every other check in
	// the handler, so no valid input is required to reach it.
	nilBucket := "blitzy-cors-nil-object-layer"
	nilRequest := newAnonymousCORSRequest(t, method, nilBucket, nil)

	globalObjLayerMutex.Lock()
	savedObjectAPI := globalObjectAPI
	globalObjLayerMutex.Unlock()

	ExecObjectLayerAPINilTest(t, nilBucket, "", instanceType, apiRouter, nilRequest)

	globalObjLayerMutex.Lock()
	globalObjectAPI = savedObjectAPI
	globalObjLayerMutex.Unlock()
}

// TestPutBucketCorsHandler drives the PUT handler through the route the server
// really registers, so routing, signature verification, authorization,
// validation and persistence are all exercised together.
//
// The endpoints list is deliberately left empty. initTestAPIEndPoints falls
// through to registerAPIRouter for an empty list, which is the registration the
// server itself performs, so the query constrained ?cors routes are matched
// exactly as they are in production. The named registerAPIFunctions list has no
// CORS entry, so naming endpoints here would leave the route unregistered and
// every request would be answered by the not-found path instead of by a handler.
func TestPutBucketCorsHandler(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{t: t, objAPITest: testPutBucketCorsHandler})
}

func testPutBucketCorsHandler(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	creds auth.Credentials, t *testing.T,
) {
	// Captured before the first request, so a timestamp dating from the bucket's
	// creation rather than from the write below is distinguishable from one this
	// test really produced.
	start := UTCNow()

	// A body that stops short of the length the client declared is a request that
	// was never delivered, not a document that failed to validate, and S3 names
	// that exactly: IncompleteBody, HTTP 400. The transport reports the shortfall
	// to the handler while the document is being read, which is what the body
	// below reproduces - half of the canonical document, then an unexpected end of
	// input where the rest should have been. Everything else about the request is
	// beyond reproach: it is signed, its digests were computed over the whole
	// document, and the half that does arrive is well-formed as far as it goes, so
	// the answer is attributable to nothing but the body ending early. Answering
	// MalformedXML would send the client looking for a schema fault in a document
	// it never finished sending, and answering InternalError would blame this
	// server for the client's own truncated request.
	//
	// This case runs before anything has been stored, so the bucket still having
	// no configuration afterwards is evidence that a request cut short is refused
	// whole rather than half-applied.
	t.Run("aBodyThatEndedEarlyIsAnIncompleteBody", func(t *testing.T) {
		request := newSignedCORSRequest(t, http.MethodPut, bucketName,
			[]byte(corsCanonicalDocument), creds)
		request.Body = io.NopCloser(io.MultiReader(
			strings.NewReader(corsCanonicalDocument[:len(corsCanonicalDocument)/2]),
			iotest.ErrReader(io.ErrUnexpectedEOF)))

		rec := httptest.NewRecorder()
		apiRouter.ServeHTTP(rec, request)

		errorResponse := assertCORSHandlerError(t, instanceType, rec, "IncompleteBody", http.StatusBadRequest)
		if want := "You did not provide the number of bytes specified by the Content-Length HTTP header."; errorResponse.Message != want {
			t.Fatalf("%s: expected the message %q, got %q", instanceType, want, errorResponse.Message)
		}

		if _, _, err := globalBucketMetadataSys.GetCORSConfig(bucketName); !isCORSConfigNotFound(err) {
			t.Fatalf("%s: expected the bucket to still have no CORS configuration, got %v",
				instanceType, err)
		}
	})

	// A document whose shape is valid and whose length is not, so the refusal is
	// attributable to the ceiling rather than to the schema.
	oversizedDocument := corsTestDoc(corsTestRule(corsMinimalRuleBody,
		"<ExposeHeader>"+strings.Repeat("x", maxBucketCORSConfigSize)+"</ExposeHeader>"))

	tooManyRules := corsTestDoc(strings.Repeat(corsTestRule(corsMinimalRuleBody), maxBucketCORSRules+1))

	testCases := []struct {
		name        string
		bucketName  string
		body        string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			// The accepted case runs first, so the rejections that follow are
			// also proof that a refused document leaves the stored one alone.
			name:       "canonicalDocumentIsAccepted",
			bucketName: bucketName,
			body:       corsCanonicalDocument,
			wantStatus: http.StatusOK,
		},
		{
			name:        "malformedXMLIsRejected",
			bucketName:  bucketName,
			body:        "<CORSConfiguration>",
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: "decoding xml: XML syntax error on line 1: unexpected EOF",
		},
		{
			name:        "wrongRootElementIsRejected",
			bucketName:  bucketName,
			body:        `<NotACORSConfiguration>` + corsTestRule(corsMinimalRuleBody) + `</NotACORSConfiguration>`,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: `Unexpected root element "NotACORSConfiguration", expected CORSConfiguration`,
		},
		{
			name:        "emptyRuleSetIsRejected",
			bucketName:  bucketName,
			body:        corsTestDoc(),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: "CORSConfiguration must contain at least one CORSRule",
		},
		{
			name:       "unsupportedMethodIsRejected",
			bucketName: bucketName,
			body: corsTestDoc(corsTestRule("<AllowedMethod>PATCH</AllowedMethod>",
				"<AllowedOrigin>*</AllowedOrigin>")),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: `CORSRule 0 has unsupported AllowedMethod "PATCH"`,
		},
		{
			name:        "tooManyRulesAreRejected",
			bucketName:  bucketName,
			body:        tooManyRules,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: fmt.Sprintf("CORSConfiguration contains %d CORSRule elements, at most %d are allowed", maxBucketCORSRules+1, maxBucketCORSRules),
		},
		{
			name:        "oversizedDocumentIsRejected",
			bucketName:  bucketName,
			body:        oversizedDocument,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "MalformedXML",
			wantMessage: fmt.Sprintf("CORSConfiguration document is larger than the maximum of %d bytes", maxBucketCORSConfigSize),
		},
		{
			// The body is malformed as well, so answering NoSuchBucket proves the
			// existence check runs before the document is read.
			name:        "missingBucketIsRejectedBeforeTheBodyIsValidated",
			bucketName:  "blitzy-cors-put-no-such-bucket",
			body:        "<CORSConfiguration>",
			wantStatus:  http.StatusNotFound,
			wantCode:    "NoSuchBucket",
			wantMessage: "The specified bucket does not exist",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodPut, testCase.bucketName, []byte(testCase.body), creds))

			if testCase.wantCode == "" {
				if rec.Code != testCase.wantStatus {
					t.Fatalf("%s: expected status %d, got %d with body %q",
						instanceType, testCase.wantStatus, rec.Code, truncateForError(rec.Body.String()))
				}
				if rec.Body.Len() != 0 {
					t.Fatalf("%s: expected an empty body on success, got %q",
						instanceType, truncateForError(rec.Body.String()))
				}
				return
			}

			errorResponse := assertCORSHandlerError(t, instanceType, rec, testCase.wantCode, testCase.wantStatus)
			if errorResponse.Message != testCase.wantMessage {
				t.Fatalf("%s: expected the message %q, got %q",
					instanceType, testCase.wantMessage, errorResponse.Message)
			}
		})
	}

	// A body that fails its own integrity check is not a body that failed to
	// validate, and it keeps the S3 code that failure already has. Each request
	// below carries the canonical document, so the schema is beyond reproach and
	// the only thing wrong is something the client declared about the bytes.
	//
	// newTestRequest computes a correct Content-Md5 and x-amz-content-sha256 for
	// the body and every builder applies these overrides before signing, so each
	// request is perfectly signed and exactly one declared value is wrong. The
	// mismatch is then discovered where it is in production: while the handler
	// reads the document through the verifying reader corsConfigBody handed it.
	corruptedBytes := []byte("not the document that follows")
	integrityCases := []struct {
		name       string
		headers    map[string]string
		wantCode   string
		wantStatus int
	}{
		{
			// Runs first in every mode, so each refusal that follows is
			// attributable to the value the case corrupts rather than to the
			// authentication mode carrying it.
			name:       "aTruthfullyDeclaredDocumentIsAccepted",
			wantStatus: http.StatusOK,
		},
		{
			name:       "aContentMD5MismatchIsBadDigest",
			headers:    map[string]string{xhttp.ContentMD5: getMD5HashBase64(corruptedBytes)},
			wantCode:   "BadDigest",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "anUndecodableContentMD5IsAnInvalidDigest",
			headers:    map[string]string{xhttp.ContentMD5: "this is not a base64 encoded md5"},
			wantCode:   "InvalidDigest",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "aContentSHA256MismatchKeepsItsOwnCode",
			headers:    map[string]string{xhttp.AmzContentSha256: getSHA256Hash(corruptedBytes)},
			wantCode:   "XAmzContentSHA256Mismatch",
			wantStatus: http.StatusBadRequest,
		},
		{
			// A declaration that cannot be decoded is refused rather than
			// discarded, so a client cannot opt out of the check by garbling the
			// value it declares.
			name:       "anUndecodableContentSHA256IsRefusedToo",
			headers:    map[string]string{xhttp.AmzContentSha256: "this is not a hex encoded sha256"},
			wantCode:   "XAmzContentSHA256Mismatch",
			wantStatus: http.StatusBadRequest,
		},
		{
			// A checksum is verified by no authentication path at all, in any
			// signature version, so accepting a truthful one proves the handler
			// itself both installed the verification and honored it.
			name: "aTruthfulChecksumIsAccepted",
			headers: map[string]string{
				xhttp.AmzChecksumCRC32: hash.NewChecksumFromData(hash.ChecksumCRC32, []byte(corsCanonicalDocument)).Encoded,
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "aChecksumMismatchIsAChecksumMismatch",
			headers: map[string]string{
				xhttp.AmzChecksumCRC32: hash.NewChecksumFromData(hash.ChecksumCRC32, corruptedBytes).Encoded,
			},
			wantCode:   "XAmzContentChecksumMismatch",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "anUndecodableChecksumIsRefusedAsAnArgument",
			headers:    map[string]string{xhttp.AmzChecksumCRC32: "this is not a base64 encoded crc32"},
			wantCode:   "InvalidArgument",
			wantStatus: http.StatusBadRequest,
		},
		{
			// A checksum promised in a trailer that never arrives is refused
			// rather than ignored, which is also what keeps the trailing branch
			// from being reached with nothing to compare against.
			name:       "aPromisedTrailingChecksumThatNeverArrivesIsRefused",
			headers:    map[string]string{xhttp.AmzTrailer: xhttp.AmzChecksumCRC32},
			wantCode:   "XAmzContentChecksumMismatch",
			wantStatus: http.StatusBadRequest,
		},
	}

	// Every authentication mode the ?cors route admits, because the guarantee is
	// weakest where the request is least protected. Only the SigV4 path installs a
	// verifying reader while it authenticates, in isReqAuthenticated; a V2
	// signature covers the Content-Md5 header but never the bytes; and a request
	// authorized by a bucket policy is not signed at all. The same contract is
	// therefore asserted three times over rather than only on the one path that
	// would have honored it anyway.
	authModes := []struct {
		name       string
		authorize  func(t *testing.T) func()
		newRequest func(t *testing.T, headers map[string]string) *http.Request
	}{
		{
			name: "signedWithV4",
			newRequest: func(t *testing.T, headers map[string]string) *http.Request {
				return newSignedCORSRequestWithHeaders(t, http.MethodPut, bucketName,
					[]byte(corsCanonicalDocument), creds, headers)
			},
		},
		{
			name: "signedWithV2",
			newRequest: func(t *testing.T, headers map[string]string) *http.Request {
				return newSignedV2CORSRequestWithHeaders(t, http.MethodPut, bucketName,
					[]byte(corsCanonicalDocument), creds, headers)
			},
		},
		{
			name: "unsignedAndAuthorizedByBucketPolicy",
			authorize: func(t *testing.T) func() {
				return grantAnonymousCORSAction(t, bucketName, policy.PutBucketCorsAction)
			},
			newRequest: func(t *testing.T, headers map[string]string) *http.Request {
				return newAnonymousCORSRequestWithHeaders(t, http.MethodPut, bucketName,
					[]byte(corsCanonicalDocument), headers)
			},
		},
	}

	for _, authMode := range authModes {
		t.Run(authMode.name, func(t *testing.T) {
			if authMode.authorize != nil {
				// Removed again when this mode is done, so the modes stay
				// independent and the anonymous refusal asserted at the end of
				// this test is still asserted against a bucket with no policy.
				t.Cleanup(authMode.authorize(t))
			}

			for _, integrityCase := range integrityCases {
				t.Run(integrityCase.name, func(t *testing.T) {
					rec := httptest.NewRecorder()
					apiRouter.ServeHTTP(rec, authMode.newRequest(t, integrityCase.headers))

					if integrityCase.wantCode == "" {
						if rec.Code != integrityCase.wantStatus {
							t.Fatalf("%s: expected status %d, got %d with body %q",
								instanceType, integrityCase.wantStatus, rec.Code, truncateForError(rec.Body.String()))
						}
						if rec.Body.Len() != 0 {
							t.Fatalf("%s: expected an empty body on success, got %q",
								instanceType, truncateForError(rec.Body.String()))
						}
						return
					}

					assertCORSHandlerError(t, instanceType, rec, integrityCase.wantCode, integrityCase.wantStatus)
				})
			}
		})
	}

	// What the handler accepted is what it stored. The document is persisted
	// re-marshaled rather than verbatim, so reading it back through the metadata
	// accessor has to reproduce the canonical bytes without the XML header the
	// client sent - and none of the refusals above may have disturbed it.
	stored, storedAt, err := globalBucketMetadataSys.GetCORSConfig(bucketName)
	if err != nil {
		t.Fatalf("%s: expected the stored CORS configuration to be readable, got error: %v", instanceType, err)
	}

	// The accepted PUT stamped the paired timestamp too, and the refusals that
	// followed left it alone: a timestamp older than this test could only have
	// been back-filled from the bucket creation time, which is what a write path
	// that never stamped it would leave behind.
	if storedAt.Before(start) {
		t.Fatalf("%s: expected the stored CORS configuration to be timestamped at or after %s, got %s",
			instanceType, start, storedAt)
	}

	storedXML, err := xml.Marshal(stored)
	if err != nil {
		t.Fatalf("%s: failed to marshal the stored configuration: %v", instanceType, err)
	}
	if want := strings.TrimPrefix(corsCanonicalDocument, xml.Header); string(storedXML) != want {
		t.Fatalf("%s: expected the stored document to be %q, got %q",
			instanceType, truncateForError(want), truncateForError(string(storedXML)))
	}

	// The bucket carries no policy, so an anonymous write is refused.
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newAnonymousCORSRequest(t, http.MethodPut, bucketName, []byte(corsCanonicalDocument)))
	assertCORSHandlerError(t, instanceType, rec, "AccessDenied", http.StatusForbidden)

	execCORSNilObjectLayerTest(t, instanceType, http.MethodPut, apiRouter)
}

// TestGetBucketCorsHandler covers the read side: the precise 404 for a bucket
// with no stored document, and the exact wire format of a stored one.
func TestGetBucketCorsHandler(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{t: t, objAPITest: testGetBucketCorsHandler})
}

func testGetBucketCorsHandler(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	creds auth.Credentials, t *testing.T,
) {
	// Nothing has been stored yet, so the read reports the configuration missing
	// rather than returning an empty document.
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodGet, bucketName, nil, creds))
	errorResponse := assertCORSHandlerError(t, instanceType, rec, "NoSuchCORSConfiguration", http.StatusNotFound)
	if errorResponse.Message != "The CORS configuration does not exist" {
		t.Fatalf("%s: expected the message %q, got %q",
			instanceType, "The CORS configuration does not exist", errorResponse.Message)
	}

	// A bucket that does not exist reports the same missing configuration. The
	// read path resolves the document through the bucket metadata layer without
	// a separate existence check, exactly as the sibling per-bucket
	// configuration handlers do.
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodGet, "blitzy-cors-get-no-such-bucket", nil, creds))
	assertCORSHandlerError(t, instanceType, rec, "NoSuchCORSConfiguration", http.StatusNotFound)

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodPut, bucketName, []byte(corsCanonicalDocument), creds))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the configuration to be stored with %d, got %d with body %q",
			instanceType, http.StatusOK, rec.Code, truncateForError(rec.Body.String()))
	}

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodGet, bucketName, nil, creds))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected status %d, got %d with body %q",
			instanceType, http.StatusOK, rec.Code, truncateForError(rec.Body.String()))
	}

	// Asserted against the canonical fixture rather than against a re-parse of
	// the response, so the comparison is with the wire format AWS clients expect
	// and not with whatever this server happens to emit.
	if want := strings.TrimPrefix(corsCanonicalDocument, xml.Header); rec.Body.String() != want {
		t.Fatalf("%s: expected the body %q, got %q",
			instanceType, truncateForError(want), truncateForError(rec.Body.String()))
	}
	if got := rec.Header().Get("Content-Type"); got != string(mimeXML) {
		t.Fatalf("%s: expected the Content-Type %q, got %q", instanceType, string(mimeXML), got)
	}

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newAnonymousCORSRequest(t, http.MethodGet, bucketName, nil))
	assertCORSHandlerError(t, instanceType, rec, "AccessDenied", http.StatusForbidden)

	execCORSNilObjectLayerTest(t, instanceType, http.MethodGet, apiRouter)
}

// TestDeleteBucketCorsHandler covers the delete side, whose contract is
// idempotent: clearing a configuration that was never stored succeeds too, and
// clearing one that was stored really removes it.
func TestDeleteBucketCorsHandler(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{t: t, objAPITest: testDeleteBucketCorsHandler})
}

func testDeleteBucketCorsHandler(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	creds auth.Credentials, t *testing.T,
) {
	assertDeleted := func(stage string) {
		t.Helper()

		rec := httptest.NewRecorder()
		apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodDelete, bucketName, nil, creds))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %s expected status %d, got %d with body %q",
				instanceType, stage, http.StatusNoContent, rec.Code, truncateForError(rec.Body.String()))
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%s: %s expected an empty body, got %q",
				instanceType, stage, truncateForError(rec.Body.String()))
		}
	}

	// Nothing was ever stored, and the delete still succeeds.
	assertDeleted("deleting a configuration that was never stored")

	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodPut, bucketName, []byte(corsCanonicalDocument), creds))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the configuration to be stored with %d, got %d with body %q",
			instanceType, http.StatusOK, rec.Code, truncateForError(rec.Body.String()))
	}

	assertDeleted("deleting a stored configuration")

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodGet, bucketName, nil, creds))
	assertCORSHandlerError(t, instanceType, rec, "NoSuchCORSConfiguration", http.StatusNotFound)

	// And repeating the delete is still a success, which is what makes the
	// operation idempotent rather than merely tolerant of an unset bucket.
	assertDeleted("repeating the delete")

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newAnonymousCORSRequest(t, http.MethodDelete, bucketName, nil))
	assertCORSHandlerError(t, instanceType, rec, "AccessDenied", http.StatusForbidden)

	execCORSNilObjectLayerTest(t, instanceType, http.MethodDelete, apiRouter)
}

// TestBucketCorsHandlersMetadataFailure covers the branch each handler takes
// when the bucket metadata layer refuses the operation: the failure has to
// surface as an S3 error, not as a success with nothing behind it.
//
// The failure is produced without a stub. Each handler resolves its own object
// layer through the injectable ObjectAPI field, so handing it the real layer
// while the global one is unset lets authorization succeed and then makes the
// metadata call - which resolves the global layer itself - fail deterministically
// for every verb, PUT included, where the write is the very last step.
func TestBucketCorsHandlersMetadataFailure(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{t: t, objAPITest: testBucketCorsHandlersMetadataFailure})
}

func testBucketCorsHandlersMetadataFailure(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	creds auth.Credentials, t *testing.T,
) {
	api := objectAPIHandlers{ObjectAPI: func() ObjectLayer { return obj }}

	globalObjLayerMutex.Lock()
	savedObjectAPI := globalObjectAPI
	globalObjectAPI = nil
	globalObjLayerMutex.Unlock()

	defer func() {
		globalObjLayerMutex.Lock()
		globalObjectAPI = savedObjectAPI
		globalObjLayerMutex.Unlock()
	}()

	testCases := []struct {
		name    string
		method  string
		body    string
		handler http.HandlerFunc
	}{
		{
			name:    "theWriteFails",
			method:  http.MethodPut,
			body:    corsCanonicalDocument,
			handler: api.PutBucketCorsHandler,
		},
		{
			name:    "theReadFails",
			method:  http.MethodGet,
			handler: api.GetBucketCorsHandler,
		},
		{
			name:    "theDeleteFails",
			method:  http.MethodDelete,
			handler: api.DeleteBucketCorsHandler,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var body []byte
			if testCase.body != "" {
				body = []byte(testCase.body)
			}

			request := newSignedCORSRequest(t, testCase.method, bucketName, body, creds)

			// The handlers are called directly, so the bucket the router would
			// have extracted from the path is supplied here.
			request = mux.SetURLVars(request, map[string]string{"bucket": bucketName})

			rec := httptest.NewRecorder()
			testCase.handler(rec, request)

			errorResponse := assertCORSHandlerError(t, instanceType, rec,
				"XMinioServerNotInitialized", http.StatusServiceUnavailable)
			if errorResponse.BucketName != bucketName {
				t.Fatalf("%s: expected the error to name the bucket %q, got %q",
					instanceType, bucketName, errorResponse.BucketName)
			}
		})
	}
}

// TestBucketCORSMetadataTimestamp covers the timestamp half of the paired CORS
// metadata fields end to end: a real write stamps it, both accessors report the
// very same value, the backend keeps it, and a delete moves it on while clearing
// the document.
//
// Every other CORS timestamp assertion in this package seeds CORSConfigUpdatedAt
// by hand, which pins the codec and the two metadata helpers but says nothing
// about the value the server itself produces. That leaves the field's whole
// reason for existing unasserted: lastUpdate reports it as the bucket's freshness
// and peers reload their cached metadata from it, so a write path that never
// stamped it - or an accessor that answered with the wrong field - would keep the
// rest of the suite green while the cluster went on serving withdrawn rules.
//
// The route is driven rather than the handler called directly, because the
// timestamp under assertion has to be the one a client request really produced.
func TestBucketCORSMetadataTimestamp(t *testing.T) {
	ExecObjectLayerAPITest(ExecObjectLayerAPITestArgs{t: t, objAPITest: testBucketCORSMetadataTimestamp})
}

func testBucketCORSMetadataTimestamp(obj ObjectLayer, instanceType, bucketName string, apiRouter http.Handler,
	creds auth.Credentials, t *testing.T,
) {
	ctx := t.Context()

	// storedTimestamp returns the timestamp GetCORSConfig reports for the stored
	// configuration, having asserted that it was actually stamped and that the
	// preflight evaluator's own lookup serves the same configuration. Both reads
	// are required: the GET handler answers through GetCORSConfig while the
	// preflight evaluator answers through preflightCORSConfig, so a configuration
	// only one of them gets right is one half the server never sees.
	storedTimestamp := func(stage string) time.Time {
		t.Helper()

		config, updatedAt, err := globalBucketMetadataSys.GetCORSConfig(bucketName)
		if err != nil {
			t.Fatalf("%s: %s: expected the stored CORS configuration to be readable, got error: %v",
				instanceType, stage, err)
		}
		if config == nil {
			t.Fatalf("%s: %s: expected the stored CORS configuration, got none", instanceType, stage)
		}
		if updatedAt.IsZero() {
			t.Fatalf("%s: %s: expected GetCORSConfig to report when the configuration was written, got the zero time",
				instanceType, stage)
		}

		preflightConfig, err := preflightCORSConfig(ctx, bucketName)
		if err != nil {
			t.Fatalf("%s: %s: expected the preflight lookup to find the configuration, got error: %v",
				instanceType, stage, err)
		}
		// Equivalence rather than pointer identity, because serving the same
		// configuration is the contract; whether the two answers happen to share
		// one cached value is an implementation detail.
		if !reflect.DeepEqual(preflightConfig, config) {
			t.Fatalf("%s: %s: expected both lookups to serve the same parsed configuration, got %+v and %+v",
				instanceType, stage, config, preflightConfig)
		}
		return updatedAt
	}

	// persistedMetadata reads the record back from the backend instead of from the
	// in-memory map, so what it reports is what a restarted server - or a peer
	// reloading after the write - would find.
	persistedMetadata := func(stage string) BucketMetadata {
		t.Helper()

		meta, err := globalBucketMetadataSys.GetConfigFromDisk(ctx, bucketName)
		if err != nil {
			t.Fatalf("%s: %s: expected the persisted bucket metadata to be readable, got error: %v",
				instanceType, stage, err)
		}
		return meta
	}

	// assertMissing proves a cleared configuration is reported as an absence by
	// the accessor the GET handler answers 404 from, and that the preflight
	// lookup no longer produces rules for the bucket either - which is what hands
	// it back to the server-wide setting.
	assertMissing := func(stage string) {
		t.Helper()

		if _, _, err := globalBucketMetadataSys.GetCORSConfig(bucketName); !isCORSConfigNotFound(err) {
			t.Fatalf("%s: %s: expected GetCORSConfig to report the configuration missing, got error: %v",
				instanceType, stage, err)
		}
		if config, err := preflightCORSConfig(ctx, bucketName); err == nil {
			t.Fatalf("%s: %s: expected the preflight lookup to find no configuration, got %+v",
				instanceType, stage, config)
		}
	}

	// The window brackets the write, which is what tells a freshly stamped
	// timestamp apart from the bucket creation time defaultTimestamps back-fills
	// when the field was never assigned at all.
	beforePut := UTCNow()

	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodPut, bucketName, []byte(corsCanonicalDocument), creds))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: expected the configuration to be stored with %d, got %d with body %q",
			instanceType, http.StatusOK, rec.Code, truncateForError(rec.Body.String()))
	}

	afterPut := UTCNow()

	putAt := storedTimestamp("after the routed PUT")
	if putAt.Before(beforePut) || putAt.After(afterPut) {
		t.Fatalf("%s: expected the CORS timestamp to fall inside the window the PUT was served in, [%s, %s], got %s",
			instanceType, beforePut, afterPut, putAt)
	}

	putMeta := persistedMetadata("after the routed PUT")
	if len(putMeta.CORSConfigXML) == 0 {
		t.Fatalf("%s: expected the persisted metadata to carry the stored CORS document", instanceType)
	}
	if !putMeta.CORSConfigUpdatedAt.Equal(putAt) {
		t.Fatalf("%s: expected the persisted CORS timestamp to be %s, got %s",
			instanceType, putAt, putMeta.CORSConfigUpdatedAt)
	}

	// The update path has to hand back the timestamp it stamped, and re-storing a
	// byte-identical document must still move that timestamp on, because the field
	// records when the configuration was written and not what it contains.
	canonicalXML := []byte(strings.TrimPrefix(corsCanonicalDocument, xml.Header))

	updatedAt, err := globalBucketMetadataSys.Update(ctx, bucketName, bucketCORSConfig, canonicalXML)
	if err != nil {
		t.Fatalf("%s: expected the CORS configuration update to succeed, got error: %v", instanceType, err)
	}
	if !updatedAt.After(putAt) {
		t.Fatalf("%s: expected the update to report a timestamp later than %s, got %s",
			instanceType, putAt, updatedAt)
	}
	if got := storedTimestamp("after the update"); !got.Equal(updatedAt) {
		t.Fatalf("%s: expected the accessors to report the timestamp the update returned, %s, got %s",
			instanceType, updatedAt, got)
	}
	if got := persistedMetadata("after the update").CORSConfigUpdatedAt; !got.Equal(updatedAt) {
		t.Fatalf("%s: expected the persisted CORS timestamp to be the one the update returned, %s, got %s",
			instanceType, updatedAt, got)
	}

	// A delete is a write of its own: it clears the document and records when that
	// happened, so peers holding the withdrawn rules learn their copy is stale.
	beforeDelete := UTCNow()

	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, newSignedCORSRequest(t, http.MethodDelete, bucketName, nil, creds))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%s: expected the configuration to be cleared with %d, got %d with body %q",
			instanceType, http.StatusNoContent, rec.Code, truncateForError(rec.Body.String()))
	}

	afterDelete := UTCNow()

	assertMissing("after the routed DELETE")

	deleteMeta := persistedMetadata("after the routed DELETE")
	if len(deleteMeta.CORSConfigXML) != 0 {
		t.Fatalf("%s: expected the persisted CORS document to be cleared, got %q",
			instanceType, truncateForError(string(deleteMeta.CORSConfigXML)))
	}
	if !deleteMeta.CORSConfigUpdatedAt.After(updatedAt) {
		t.Fatalf("%s: expected the delete to advance the persisted CORS timestamp past %s, got %s",
			instanceType, updatedAt, deleteMeta.CORSConfigUpdatedAt)
	}
	if deleteMeta.CORSConfigUpdatedAt.Before(beforeDelete) || deleteMeta.CORSConfigUpdatedAt.After(afterDelete) {
		t.Fatalf("%s: expected the CORS timestamp to fall inside the window the DELETE was served in, [%s, %s], got %s",
			instanceType, beforeDelete, afterDelete, deleteMeta.CORSConfigUpdatedAt)
	}
}
