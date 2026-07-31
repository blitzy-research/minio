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
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio/internal/auth"
	"github.com/minio/mux"
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

		// V1c - the body must be exactly one CORSConfiguration document. A
		// single decode stops as soon as it has filled the root element and
		// never asks for EOF, so anything trailing the closing tag would
		// otherwise be accepted in silence.
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

		// V1d - element placement and cardinality. The document model would
		// otherwise normalize a violation away: a repeated scalar keeps only
		// its last value and an unknown element is skipped without comment.
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

		// V9 - the body is bounded by maxBucketCORSConfigSize. The boundary
		// itself is accepted and one byte more is rejected as too large, which
		// is only provable because the validator reads one byte past the ceiling
		// instead of validating a truncated prefix of the body.
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

		// R8 - the wire format is the AWS one, so the document either omits the
		// namespace or declares exactly the S3 namespace. Any other value would
		// be preserved on the persisted document and handed back to clients that
		// cannot read it.
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

		// R1 - ID and MaxAgeSeconds are single valued. They map to scalar fields
		// of the document model, which silently keeps the last occurrence, so a
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

		// R1/R2 - an element the document model does not know is discarded while
		// decoding, so a client would be told a configuration was stored that
		// the server never understood. Every unknown element is rejected, and so
		// is a known element in an invalid position.
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

		// R2 - the decoder stops at the end of the first root element and the
		// document model never learns what followed it, so a trailing tail has
		// to be rejected explicitly.
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
		rule := corsRuleFor(cfg, "https://app.example.com", http.MethodGet, []string{"x-amz-acl"})
		if rule == nil {
			t.Fatal("expected the trimmed rule to match the preflight request it allows")
		}
		if rule.MaxAgeSeconds != want.MaxAgeSeconds {
			t.Errorf("expected MaxAgeSeconds %d, got %d", want.MaxAgeSeconds, rule.MaxAgeSeconds)
		}
		if second := corsRuleFor(cfg, "https://admin.example.com", http.MethodPut, nil); second == nil {
			t.Error("expected the second trimmed origin and method to match as well")
		}
		if denied := corsRuleFor(cfg, "https://evil.example.com", http.MethodGet, nil); denied != nil {
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
	// Requirement R8: the namespace the validator enforces is the one every S3
	// client sends and expects back, so it is pinned against a literal rather
	// than against itself.
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
	// A browser lists only the headers the request it is about to make
	// actually carries. The ceiling has to stay comfortably above that while
	// remaining a bound.
	if maxCORSPreflightRequestHeaders < 16 {
		t.Errorf("expected the requested-header ceiling to leave room for a real client, got %d",
			maxCORSPreflightRequestHeaders)
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
			rule := corsRuleFor(tc.cfg, tc.origin, tc.method, tc.reqHeaders)

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

			got, ok := parseCORSRequestHeaders(header)
			if !ok {
				t.Fatalf("expected %v to be evaluable, got a refusal", tc.values)
			}
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

	// The ceiling is what stops an unauthenticated preflight from choosing how
	// much matching work the server performs. Exactly the ceiling must still be
	// evaluable, one header over it must not, and duplicates must not count
	// towards it.
	t.Run("requestedHeaderCeiling", func(t *testing.T) {
		requestHeaders := func(count int) http.Header {
			names := make([]string, 0, count)
			for i := range count {
				names = append(names, fmt.Sprintf("x-blitzy-%d", i))
			}
			header := http.Header{}
			header.Set("Access-Control-Request-Headers", strings.Join(names, ","))
			return header
		}

		got, ok := parseCORSRequestHeaders(requestHeaders(maxCORSPreflightRequestHeaders))
		if !ok {
			t.Fatalf("expected exactly %d requested headers to be evaluable", maxCORSPreflightRequestHeaders)
		}
		if len(got) != maxCORSPreflightRequestHeaders {
			t.Fatalf("expected %d requested headers, got %d", maxCORSPreflightRequestHeaders, len(got))
		}

		if got, ok := parseCORSRequestHeaders(requestHeaders(maxCORSPreflightRequestHeaders + 1)); ok {
			t.Fatalf("expected %d requested headers to be refused, got %d headers",
				maxCORSPreflightRequestHeaders+1, len(got))
		} else if got != nil {
			t.Fatalf("expected no requested headers alongside a refusal, got %d", len(got))
		}

		// A single name repeated well past the ceiling collapses to one header
		// and stays evaluable.
		header := http.Header{}
		header.Set("Access-Control-Request-Headers",
			strings.TrimSuffix(strings.Repeat("X-A,x-a,", maxCORSPreflightRequestHeaders), ","))
		got, ok = parseCORSRequestHeaders(header)
		if !ok {
			t.Fatal("expected a repeated header name to stay evaluable")
		}
		if len(got) != 1 || got[0] != "X-A" {
			t.Fatalf("expected the repeated name to collapse to [X-A], got %v", got)
		}
	})

	t.Run("unionIsEvaluatedAsAWholeByTheMatcher", func(t *testing.T) {
		header := http.Header{}
		header.Add("Access-Control-Request-Headers", "x-a, x-b")
		header.Add("Access-Control-Request-Headers", "x-c")
		reqHeaders, ok := parseCORSRequestHeaders(header)
		if !ok {
			t.Fatal("expected three requested headers to be evaluable")
		}

		const origin = "https://www.example1.com"

		covering := corsTestConfig(miniogocors.Rule{
			ID:            "covers-every-header",
			AllowedHeader: []string{"x-a", "X-B", "x-c"},
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})
		if rule := corsRuleFor(covering, origin, http.MethodPut, reqHeaders); rule == nil {
			t.Errorf("expected the rule covering every header of %v to match", reqHeaders)
		}

		partial := corsTestConfig(miniogocors.Rule{
			ID:            "covers-some-headers",
			AllowedHeader: []string{"x-a", "x-b"},
			AllowedMethod: []string{http.MethodPut},
			AllowedOrigin: []string{origin},
		})
		if rule := corsRuleFor(partial, origin, http.MethodPut, reqHeaders); rule != nil {
			t.Errorf("expected no match when %v is only partially covered, got rule %q", reqHeaders, rule.ID)
		}
	})
}

// TestBucketCorsPreflightMiddlewareDelegation verifies non-OPTIONS,
// incomplete-preflight and root-path requests reach the wrapped global CORS
// handler without added CORS headers.
//
// That gate is what keeps the server-wide MINIO_API_CORS_ALLOW_ORIGIN handler in
// force, and it is the structural reason the existing TestCors regression guard -
// which sends OPTIONS with only an Origin header - is unaffected by this feature.
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
// preflight whose target bucket cannot be resolved because the Host header
// cannot be parsed.
//
// With virtual-host-style addressing the bucket name comes from a Host header
// that is client controlled and unauthenticated on this path. A Host the server
// cannot parse - a non-numeric or out of range port, an invalid host label, too
// many colons, or no Host at all - resolves to no bucket, so the request is
// delegated to the server-wide handler and must not fail: resolving it through
// request2BucketObjectName instead reaches logger.CriticalIf, which panics.
//
// Each case installs a configuration that would have matched the request, so the
// disposition is provably decided by the unresolved bucket alone.
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
		// resolves reports whether the host is parseable, and therefore whether
		// the bucket it names is evaluated against the stored rules instead of
		// being handed on.
		resolves bool
	}{
		{name: "nonNumericPort", host: bucket + "." + domain + ":notaport"},
		{name: "portOutOfRange", host: bucket + "." + domain + ":99999"},
		{name: "invalidHostLabel", host: "]bad[." + domain},
		{name: "tooManyColons", host: ":::"},
		{name: "noHostAtAll", host: ""},
		// Port zero is a valid port, so this host parses and the bucket it names
		// is evaluated. It is the boundary that proves only an unparsable host
		// is handed on.
		{name: "portZeroResolves", host: bucket + "." + domain + ":0", resolves: true},
		{name: "wellFormedHostResolves", host: bucket + "." + domain, resolves: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))

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

			req := httptest.NewRequest(http.MethodOptions, "/object", nil)
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

			if tc.resolves {
				if delegated {
					t.Fatalf("expected the parseable host %q to resolve to bucket %q and be evaluated, but the request was handed on",
						tc.host, bucket)
				}
				if rec.Code != http.StatusOK {
					t.Errorf("expected status %d for the matched rule, got %d", http.StatusOK, rec.Code)
				}
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
					t.Errorf("expected the matched rule to echo the origin %q, got %q", origin, got)
				}
				return
			}

			if !delegated {
				t.Fatalf("expected a preflight carrying the unparsable host %q to reach the wrapped handler, got status %d with headers %v",
					tc.host, rec.Code, rec.Header())
			}
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
// fresh, empty one for the duration of the test, restoring the previous value
// afterwards. loaded reports whether the replacement presents itself as having
// finished loading, which is what tells the preflight evaluator that a bucket
// absent from the cache is a bucket without a configuration rather than an
// answer that is not known yet.
func installTestBucketMetadataSys(t *testing.T, loaded bool) *BucketMetadataSys {
	t.Helper()

	saved := globalBucketMetadataSys
	t.Cleanup(func() { globalBucketMetadataSys = saved })

	sys := NewBucketMetadataSys()
	if loaded {
		sys.Lock()
		sys.initialized = true
		sys.Unlock()
	}
	globalBucketMetadataSys = sys
	return sys
}

// setTestBucketCORSConfig makes bucket known to sys carrying cfg as its parsed
// CORS configuration. A nil cfg leaves the bucket present but without one.
func setTestBucketCORSConfig(sys *BucketMetadataSys, bucket string, cfg *miniogocors.Config) {
	meta := newBucketMetadata(bucket)
	meta.corsConfig = cfg
	sys.Set(bucket, meta)
}

// TestIsBucketCORSConfigNotFound pins how narrow the preflight evaluator's
// absence test has to be. Recognizing an error as an absence is what sends a
// preflight to the server-wide handler, whose default allows every origin with
// credentials, so exactly one error may be recognized: the typed
// BucketCORSConfigNotFound sentinel, however it is wrapped. Every other reason a
// configuration could not be produced - above all a cache that has not finished
// loading, whose answer is merely unknown - must read as not an absence, so that
// the evaluator denies instead of falling back.
func TestIsBucketCORSConfigNotFound(t *testing.T) {
	const bucket = "blitzy-cors-bucket"

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "noError", err: nil, want: false},
		{name: "theSentinelItself", err: BucketCORSConfigNotFound{Bucket: bucket}, want: true},
		{
			name: "theSentinelWrapped",
			err:  fmt.Errorf("loading bucket %s: %w", bucket, BucketCORSConfigNotFound{Bucket: bucket}),
			want: true,
		},
		{name: "aCacheThatHasNotFinishedLoading", err: errBucketMetadataNotInitialized, want: false},
		{
			name: "aCacheThatHasNotFinishedLoadingWrapped",
			err:  fmt.Errorf("looking up bucket %s: %w", bucket, errBucketMetadataNotInitialized),
			want: false,
		},
		{name: "anAbsentMetadataSubsystem", err: errServerNotInitialized, want: false},
		{name: "aDifferentConfigurationsAbsence", err: BucketSSEConfigNotFound{Bucket: bucket}, want: false},
		{
			name: "aRuleLessStoredDocument",
			err:  fmt.Errorf("Stored CORS configuration for bucket %s contains no CORSRule", bucket),
			want: false,
		},
		{name: "anyOtherLookupFailure", err: errors.New("disk not found"), want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBucketCORSConfigNotFound(tc.err); got != tc.want {
				t.Fatalf("expected isBucketCORSConfigNotFound(%v) to be %t, got %t", tc.err, tc.want, got)
			}
		})
	}
}

// TestGetCORSConfigCachedIsCacheOnly pins the property the preflight evaluator
// depends on. A preflight request carries no credentials and names its bucket in
// a path the client chooses freely, so the lookup it performs must never read
// from the backend and must never add an entry to the bucket metadata map -
// otherwise a stream of made-up names turns into backend reads and into
// permanent, unbounded memory growth.
//
// The two absence answers must also stay distinguishable: a bucket missing from
// a loaded cache definitively has no configuration, while a bucket missing from
// a cache that is still loading is simply not known yet, and only the former may
// be treated as an absence.
func TestGetCORSConfigCachedIsCacheOnly(t *testing.T) {
	const (
		knownBucket   = "blitzy-cors-known"
		unknownBucket = "blitzy-cors-unknown"
	)
	cfg := corsTestConfig(miniogocors.Rule{
		ID:            "only",
		AllowedMethod: []string{http.MethodPut},
		AllowedOrigin: []string{"https://www.example1.com"},
	})

	t.Run("unknownBucketWhileTheCacheIsStillLoading", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, false)

		got, _, err := sys.GetCORSConfigCached(unknownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if !errors.Is(err, errBucketMetadataNotInitialized) {
			t.Fatalf("expected %v, got %v", errBucketMetadataNotInitialized, err)
		}
		// An unloaded cache must remain distinguishable from a definitive
		// absence: only the latter is the error the preflight evaluator falls
		// back on, and only the latter maps to NoSuchCORSConfiguration for GET.
		if isBucketCORSConfigNotFound(err) {
			t.Fatal("expected an unloaded cache not to report a definitive absence")
		}
		if sys.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", sys.Count())
		}
	})

	t.Run("unknownBucketOnALoadedCache", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, true)

		got, _, err := sys.GetCORSConfigCached(unknownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if !isBucketCORSConfigNotFound(err) {
			t.Fatalf("expected a BucketCORSConfigNotFound sentinel, got %v", err)
		}
		if sys.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", sys.Count())
		}
	})

	t.Run("manyUnknownBucketsNeverGrowTheMetadataMap", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, true)

		for i := range 512 {
			if _, _, err := sys.GetCORSConfigCached(fmt.Sprintf("blitzy-cors-probe-%d", i)); err == nil {
				t.Fatalf("expected an unknown bucket to have no configuration, got one for probe %d", i)
			}
		}
		if sys.Count() != 0 {
			t.Fatalf("expected 512 unknown lookups to add no metadata entries, got %d", sys.Count())
		}
	})

	t.Run("theTwoAbsenceSignalsStayDistinguishable", func(t *testing.T) {
		// The preflight decision rests on telling these two apart: a definitive
		// absence must be recognized as one and delegate, while a bucket missing
		// from a cache that is still loading must not be, or a configuration that
		// is merely unavailable would fall back to the permissive server-wide
		// default. The S3 GET handler depends on the same distinction to answer
		// NoSuchCORSConfiguration. Asserting on the function the evaluator calls
		// also proves neither answer is reshaped on the way out.
		//
		// The loading accessor is deliberately not exercised: it reaches for the
		// process-wide object layer, so its answer depends on what an earlier
		// test in this package left installed. That it stays out of the preflight
		// path is asserted deterministically instead - neither lookup below may
		// add a metadata entry, which only a cache-only lookup can satisfy.
		loaded := installTestBucketMetadataSys(t, true)

		_, err := preflightCORSConfig(unknownBucket)
		if !isBucketCORSConfigNotFound(err) {
			t.Fatalf("expected a definitive absence on a loaded cache, got %v", err)
		}
		if errors.Is(err, errBucketMetadataNotInitialized) {
			t.Fatalf("a definitive absence must not also read as an unknown state, got %v", err)
		}
		if loaded.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", loaded.Count())
		}

		loading := installTestBucketMetadataSys(t, false)

		_, err = preflightCORSConfig(unknownBucket)
		if !errors.Is(err, errBucketMetadataNotInitialized) {
			t.Fatalf("expected an unknown state while the cache is still loading, got %v", err)
		}
		if isBucketCORSConfigNotFound(err) {
			t.Fatal("an unknown state must not read as a definitive absence")
		}
		if loading.Count() != 0 {
			t.Fatalf("expected the lookup not to add a metadata entry, got %d", loading.Count())
		}
	})

	t.Run("knownBucketWithoutAConfiguration", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, true)
		setTestBucketCORSConfig(sys, knownBucket, nil)

		got, _, err := sys.GetCORSConfigCached(knownBucket)
		if got != nil {
			t.Fatalf("expected no configuration, got %+v", got)
		}
		if !isBucketCORSConfigNotFound(err) {
			t.Fatalf("expected a BucketCORSConfigNotFound sentinel, got %v", err)
		}
		if sys.Count() != 1 {
			t.Fatalf("expected exactly the one bucket that was stored, got %d", sys.Count())
		}
	})

	t.Run("knownBucketWithAConfiguration", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, true)
		setTestBucketCORSConfig(sys, knownBucket, cfg)

		got, _, err := sys.GetCORSConfigCached(knownBucket)
		if err != nil {
			t.Fatalf("expected the stored configuration, got error: %v", err)
		}
		if got != cfg {
			t.Fatalf("expected the stored configuration to be returned as-is, got %+v", got)
		}
	})

	// A cold cache must not be reported as an absence even for a bucket it does
	// hold, so the loaded flag may only ever be consulted on a cache miss.
	t.Run("knownBucketIsAnsweredEvenWhileTheCacheIsStillLoading", func(t *testing.T) {
		sys := installTestBucketMetadataSys(t, false)
		setTestBucketCORSConfig(sys, knownBucket, cfg)

		if got, _, err := sys.GetCORSConfigCached(knownBucket); err != nil || got != cfg {
			t.Fatalf("expected the stored configuration, got %+v and error %v", got, err)
		}
	})
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
// request to the server-wide handler, whose default allows every origin with
// credentials, so it is reserved for the single case where the bucket definitively
// has no configuration of its own - requirement R7's fallback. Every state in
// which this layer cannot establish the bucket's rules - an absent or still
// loading metadata subsystem, a stored document carrying no rule, a failed lookup
// - denies instead, because falling back there would quietly relax a restrictive
// bucket exactly when it matters. Once the rules are in hand the layer answers on
// its own, allowing on the first matching rule and denying when
// none allows the request.
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

	// A requested-header list one entry past the ceiling, which must be denied
	// rather than evaluated against a shortened list.
	overCeilingHeaders := make([]string, 0, maxCORSPreflightRequestHeaders+1)
	for i := range maxCORSPreflightRequestHeaders + 1 {
		overCeilingHeaders = append(overCeilingHeaders, fmt.Sprintf("x-amz-blitzy-%d", i))
	}

	testCases := []struct {
		name string
		// setup installs the bucket metadata state the case needs.
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
			// The metadata subsystem is not there to be asked, so whether the
			// bucket restricts cross-origin access cannot be established. That is
			// not the same as establishing that it does not, so the request is
			// refused rather than handed to the permissive default.
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
			// The startup window: the cache cannot yet say whether the bucket
			// has a configuration. Delegating on that unknown would answer a
			// restrictive bucket out of the server-wide default for as long as
			// the window lasts, so it is refused instead - a refusal that
			// carries no Access-Control-Max-Age and is not entered into the
			// browser's preflight cache, so a retry once the cache is loaded is
			// answered from the bucket's own rules.
			name: "unknownBucketWhileTheCacheIsStillLoadingDenies",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t, false)
			},
			want: corsOutcomeDenied,
		},
		{
			// A loaded cache that does not know the bucket answers the definitive
			// BucketCORSConfigNotFound, so this is requirement R7's fallback and
			// the server-wide handler serves the request.
			name: "unknownBucketOnALoadedCacheDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t, true)
			},
			want: corsOutcomeDelegated,
		},
		{
			// The same definitive absence, reached the other way: the bucket's
			// metadata is loaded and carries no CORS configuration at all.
			name: "bucketWithoutAConfigurationDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, nil)
			},
			want: corsOutcomeDelegated,
		},
		{
			// A rule-less document cannot have come from PutBucketCors, which
			// requires at least one rule, so the bucket carries a configuration
			// this layer cannot use rather than no configuration at all. There is
			// nothing to match against and nothing that says the fallback
			// applies, so the request is refused.
			name: "storedConfigurationWithZeroRulesDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig())
			},
			want: corsOutcomeDenied,
		},
		{
			name: "syntacticallyInvalidBucketNameDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t, true)
			},
			target: "/Not_A_Bucket/object",
			want:   corsOutcomeDelegated,
		},
		{
			name: "rootPathPreflightDelegates",
			setup: func(t *testing.T) {
				t.Helper()
				installTestBucketMetadataSys(t, true)
			},
			target: "/",
			want:   corsOutcomeDelegated,
		},
		{
			name: "noRuleMatchesTheOriginDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(otherOriginRule))
			},
			want: corsOutcomeDenied,
		},
		{
			name: "noRuleMatchesTheMethodDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))
			},
			reqMethod: http.MethodDelete,
			want:      corsOutcomeDenied,
		},
		{
			name: "noRuleCoversTheRequestedHeadersDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))
			},
			reqHeaders: "content-type",
			want:       corsOutcomeDenied,
		},
		{
			name: "tooManyRequestedHeadersDenies",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(
					miniogocors.Rule{
						ID:            "everything",
						AllowedHeader: []string{"*"},
						AllowedMethod: []string{http.MethodPut},
						AllowedOrigin: []string{"*"},
					},
				))
			},
			reqHeaders: strings.Join(overCeilingHeaders, ","),
			want:       corsOutcomeDenied,
		},
		{
			name: "matchingRuleAllowsAndEmitsEveryHeaderFamily",
			setup: func(t *testing.T) {
				t.Helper()
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))
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
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))
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
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(
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
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(
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
				setTestBucketCORSConfig(installTestBucketMetadataSys(t, true), bucket, corsTestConfig(matchingRule))
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
// preflight whose Host header does not parse. That is reachable only when
// virtual-host-style addressing is configured, because only then does resolving
// the bucket have to parse the Host at all.
//
// A preflight carries no credentials, so this middleware inspects a client-chosen
// Host before anything has authenticated the request. Resolving it through
// request2BucketObjectName would report the parse failure through
// logger.CriticalIf, which panics. A malformed Host identifies no bucket, so it
// is delegated like any other request that does not resolve to one - and the
// wrapped handler being reached at all is what proves nothing panicked on the way
// there.
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
	}{
		{name: "underscoreInALabel", host: "a_b." + domain},
		{name: "leadingHyphenInALabel", host: "-bad." + domain},
		{name: "overLongLabel", host: overLongLabel + "." + domain},
		{name: "emptyLabel", host: "a..b." + domain},
		{name: "malformedHostOutsideTheDomain", host: "a_b.other.tld"},
		{name: "malformedHostCarryingAPort", host: "a_b." + domain + ":9000"},
	}

	for _, tc := range hosts {
		t.Run(tc.name, func(t *testing.T) {
			// The bucket is present and its single rule would match the probe,
			// so delegation cannot be mistaken for the ordinary
			// no-configuration fallback: the only thing standing in the way is
			// the Host that does not parse.
			sys := installTestBucketMetadataSys(t, true)
			setTestBucketCORSConfig(sys, bucket, corsTestConfig(miniogocors.Rule{
				ID:            "matching",
				AllowedMethod: []string{http.MethodPut},
				AllowedOrigin: []string{"*"},
			}))

			saved := globalDomainNames
			t.Cleanup(func() { globalDomainNames = saved })
			globalDomainNames = []string{domain}

			delegated := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delegated = true
				w.WriteHeader(http.StatusTeapot)
			})

			req := httptest.NewRequest(http.MethodOptions, "/object", nil)
			req.Host = tc.host
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", http.MethodPut)
			req.Header.Set("Access-Control-Request-Headers", "x-amz-meta-foo")
			rec := httptest.NewRecorder()

			bucketCORSPreflightMiddleware(next).ServeHTTP(rec, req)

			if !delegated {
				t.Fatalf("expected a preflight with a malformed Host to reach the wrapped handler, got status %d and headers %v",
					rec.Code, rec.Header())
			}
			if rec.Code != http.StatusTeapot {
				t.Errorf("expected the wrapped handler's status %d, got %d", http.StatusTeapot, rec.Code)
			}
			for _, name := range corsPreflightAllowHeaders {
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

// TestBucketCorsPreflightMiddlewareLeavesNoMetadataTrace pins the resource half
// of the preflight contract: a stream of preflights naming buckets that do not
// exist must leave the bucket metadata map exactly as it found it. Without that
// guarantee an unauthenticated client can grow the map without bound simply by
// inventing a new name per request.
func TestBucketCorsPreflightMiddlewareLeavesNoMetadataTrace(t *testing.T) {
	sys := installTestBucketMetadataSys(t, true)

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

	var reader io.ReadSeeker
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := newTestSignedRequestV4(method, getBucketCORSURL("", bucketName),
		int64(len(body)), reader, creds.AccessKey, creds.SecretKey, nil)
	if err != nil {
		t.Fatalf("failed to build a signed %s ?cors request: %v", method, err)
	}
	return request
}

// newAnonymousCORSRequest builds an unsigned request against the ?cors route.
// Carrying no Authorization header is what makes it anonymous, and an anonymous
// caller is authorized against the bucket policy rather than against the owner
// short circuit.
func newAnonymousCORSRequest(t *testing.T, method, bucketName string, body []byte) *http.Request {
	t.Helper()

	var reader io.ReadSeeker
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := newTestRequest(method, getBucketCORSURL("", bucketName), int64(len(body)), reader)
	if err != nil {
		t.Fatalf("failed to build an anonymous %s ?cors request: %v", method, err)
	}
	return request
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

	// What the handler accepted is what it stored. The document is persisted
	// re-marshaled rather than verbatim, so reading it back through the metadata
	// accessor has to reproduce the canonical bytes without the XML header the
	// client sent - and none of the refusals above may have disturbed it.
	stored, _, err := globalBucketMetadataSys.GetCORSConfig(bucketName)
	if err != nil {
		t.Fatalf("%s: expected the stored CORS configuration to be readable, got error: %v", instanceType, err)
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

	// The delete cleared the document rather than merely reporting success.
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
