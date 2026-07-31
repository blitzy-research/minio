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
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	miniogocors "github.com/minio/minio-go/v7/pkg/cors"
)

// s3CORSNamespace is the XML namespace every S3 CORS document carries. It
// mirrors the unexported default of github.com/minio/minio-go/v7/pkg/cors, which
// is not addressable from here, and pins the wire format that the AWS SDK, "aws
// s3api ... cors" and "mc" all expect.
const s3CORSNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// corsMinimalRuleBody is the smallest set of child elements a valid CORSRule
// needs: exactly one AllowedMethod and exactly one AllowedOrigin.
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

// corsTestDoc wraps the supplied CORSRule elements in a CORSConfiguration root
// element carrying the S3 namespace, which is the shape every S3 client sends.
func corsTestDoc(rules ...string) string {
	return `<CORSConfiguration xmlns="` + s3CORSNamespace + `">` + strings.Join(rules, "") + `</CORSConfiguration>`
}

// corsTestRule wraps the supplied child elements in a single CORSRule element.
func corsTestRule(children ...string) string {
	return `<CORSRule>` + strings.Join(children, "") + `</CORSRule>`
}

// corsTestConfig builds a configuration from the supplied rules, in document
// order, through the same constructor the SDK uses so that the namespace is
// populated exactly as it would be on a parsed document.
func corsTestConfig(rules ...miniogocors.Rule) *miniogocors.Config {
	return miniogocors.NewConfig(rules)
}

// TestValidateBucketCorsConfig exercises every rejection reason the validator
// enforces on top of the CORS schema, together with the documents that must be
// accepted. Every input is read through the same io.LimitReader ceiling the PUT
// handler applies, so the size case reproduces the production truncation rather
// than approximating it.
//
// Failures are asserted with a distinctive substring rather than exact string
// equality: the message is surfaced verbatim to the client as the description of
// a MalformedXML error, so what matters is that it names the offending rule and
// value, not its exact phrasing.
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
		// xml is the request body handed to the validator.
		xml string
		// wantErrSubstring is a distinctive fragment of the expected failure.
		// An empty value means the document must be accepted.
		wantErrSubstring string
		// wantRules is the rule count expected on an accepted document.
		wantRules int
	}

	testCases := []validateCase{
		// V1 - the document must be well-formed XML rooted at CORSConfiguration.
		{
			name:             "V1a/notWellFormedXML",
			xml:              `<CORSConfiguration><CORSRule>`,
			wantErrSubstring: "XML syntax error",
		},
		{
			name:             "V1a/emptyBody",
			xml:              ``,
			wantErrSubstring: "decoding xml",
		},
		{
			name:             "V1a/unclosedRuleElement",
			xml:              `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod></CORSConfiguration>`,
			wantErrSubstring: "XML syntax error",
		},
		{
			name:             "V1b/rootElementNotCORSConfiguration",
			xml:              `<NotCORSConfiguration/>`,
			wantErrSubstring: "CORSConfiguration",
		},
		{
			name:             "V1b/rootElementNotCORSConfigurationButCarriesRules",
			xml:              `<AccessControlPolicy>` + corsTestRule(corsMinimalRuleBody) + `</AccessControlPolicy>`,
			wantErrSubstring: "CORSConfiguration",
		},

		// V2 - at least one rule is required.
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

		// V3 - at most maxBucketCORSRules rules are allowed, and the boundary
		// itself must still be accepted.
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

		// V4 - every rule needs at least one AllowedMethod.
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

		// V5 - AllowedMethod is restricted to the five methods S3 accepts.
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

		// V6 - every rule needs at least one AllowedOrigin.
		{
			name:             "V6/ruleWithoutAllowedOrigin",
			xml:              corsTestDoc(corsTestRule(`<AllowedMethod>GET</AllowedMethod>`)),
			wantErrSubstring: "CORSRule 0 must contain at least one AllowedOrigin",
		},

		// V7 - an origin may carry at most one wildcard.
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

		// V8 - an AllowedHeader may carry at most one wildcard, and none at all
		// is equally valid.
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

		// V9 - the body is bounded by maxBucketCORSConfigSize, so a document one
		// byte over the ceiling is truncated and fails to decode while the same
		// document one byte under it is accepted.
		{
			name:      "V9/oneByteUnderMaxConfigSizeIsAccepted",
			xml:       corsPaddedDoc(maxBucketCORSConfigSize - sizeOverhead - 1),
			wantRules: 1,
		},
		{
			name:             "V9/oneByteOverMaxConfigSizeIsTruncatedAndRejected",
			xml:              corsPaddedDoc(maxBucketCORSConfigSize - sizeOverhead + 1),
			wantErrSubstring: "unexpected EOF",
		},

		// MaxAgeSeconds must be a non-negative integer when present.
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

		// A fully populated rule, and the canonical multi-rule document, must
		// both be accepted.
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
	}

	// A rule naming any single method S3 accepts must validate. The matrix is
	// generated from allSupportedCORSMethods so that all five cases share one
	// document shape and differ only in the configured method.
	for _, method := range allSupportedCORSMethods {
		testCases = append(testCases, validateCase{
			name:      "V5/supportedAllowedMethod" + method,
			xml:       corsTestDoc(corsTestRule(`<AllowedMethod>` + method + `</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`)),
			wantRules: 1,
		})
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// PutBucketCorsHandler bounds the request body by
			// maxBucketCORSConfigSize before handing it to the validator;
			// mirroring that here keeps the size cases honest.
			cfg, err := validateBucketCorsConfig(io.LimitReader(strings.NewReader(tc.xml), maxBucketCORSConfigSize))

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
				// Requirement R8: the accepted configuration always carries the
				// S3 namespace, whether or not the client sent one.
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
}

// TestValidateBucketCorsConfigCanonicalDocument pins the wire contract of
// requirement R8 against the canonical, AWS shaped document: the parsed model,
// the normalization the parser performs, the namespace, and the fact that
// re-marshaling reproduces the input byte for byte. A client written against
// AWS - the SDK, "aws s3api ... cors" or "mc" - can only interoperate if all
// four hold.
func TestValidateBucketCorsConfigCanonicalDocument(t *testing.T) {
	cfg, err := validateBucketCorsConfig(io.LimitReader(strings.NewReader(corsCanonicalDocument), maxBucketCORSConfigSize))
	if err != nil {
		t.Fatalf("the canonical document must be accepted, got error: %v", err)
	}

	// The expected model, in document order. Element names and casing are those
	// AWS uses; the parser maps them through the vendored struct tags.
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

		// The parser normalizes methods in place, so a document written with
		// mixed casing is stored - and later matched - as upper case.
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

	// Two rules that both allow the same origin and method, differing only in
	// the response they would produce. Only their order decides the answer.
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

	// A rule that allows every method S3 accepts, used by the method matrix.
	allMethodsRule := miniogocors.Rule{
		ID:            "all-methods",
		AllowedMethod: slices.Clone(allSupportedCORSMethods),
		AllowedOrigin: []string{"https://www.example1.com"},
	}

	testCases := []matchCase{
		// First matching rule wins, in document order - requirement R5.
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

		// Origin predicate.
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

		// Method predicate.
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

		// Header predicate.
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

		// Degenerate configurations.
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

	// A rule allowing every supported method must match a preflight for each of
	// them. The matrix is generated so that all five cases share one rule and
	// differ only in the requested method.
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
		name string
		// values are the raw Access-Control-Request-Headers field values, in the
		// order a client or gateway would present them.
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

	t.Run("unionIsEvaluatedAsAWholeByTheMatcher", func(t *testing.T) {
		// The union is what the matcher must satisfy in full: a rule covering
		// every member matches, and one covering only some of them does not.
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

// TestBucketCorsPreflightMiddlewareDelegation covers the gate the middleware
// applies before it consults any global state: a request that is not an OPTIONS
// request, a preflight without Access-Control-Request-Method and a preflight
// without an Origin to echo back are all delegated to the wrapped handler
// untouched.
//
// That gate is what keeps the server wide MINIO_API_CORS_ALLOW_ORIGIN handler in
// force, and it is the structural reason the existing TestCors regression guard -
// which sends OPTIONS with only an Origin header - is unaffected by this feature.
// The per-bucket paths beyond the gate need globalBucketMetadataSys and are
// covered end to end by the server suite instead, so this test stays hermetic.
func TestBucketCorsPreflightMiddlewareDelegation(t *testing.T) {
	const delegatedBody = "delegated"

	// corsAllowHeaders are the headers a matched rule would produce. None of
	// them may appear on a delegated response, otherwise the middleware would be
	// answering requests it is supposed to pass through.
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
