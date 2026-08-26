package validation

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// uriVectorFile is the cross-language conformance corpus. The Python bridges'
// test suites read the same file, so a one-sided change to either normalizer
// turns the other language's suite red — which is what makes "the two
// implementations are kept in step" an enforced property rather than a comment.
const uriVectorFile = "testdata/uri_vectors.json"

// errorClasses maps the vector file's error names to the sentinels this package
// returns. Keep in sync with the header comment in the vector file.
var errorClasses = map[string]error{
	"empty":            ErrURIEmpty,
	"no_scheme":        ErrURINoScheme,
	"bad_scheme":       ErrURIBadScheme,
	"scheme_forbidden": ErrURISchemeNotAllowed,
	"no_authority":     ErrURINoAuthority,
	"too_long":         ErrURITooLong,
	"unnormalizable":   ErrURIUnnormalizable,
}

func TestNormalizeURIConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile(uriVectorFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", uriVectorFile, err)
	}
	var corpus struct {
		Cases []struct {
			Name   string `json:"name"`
			Input  string `json:"input"`
			Output string `json:"output"`
			Error  string `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("failed to parse %s: %v", uriVectorFile, err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatalf("%s contains no cases", uriVectorFile)
	}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := NormalizeURI(tc.Input)

			if tc.Error != "" {
				sentinel, known := errorClasses[tc.Error]
				if !known {
					t.Fatalf("vector names unknown error class %q", tc.Error)
				}
				if err == nil {
					t.Fatalf("NormalizeURI(%q) = %q, want error class %s", tc.Input, got, tc.Error)
				}
				if !errors.Is(err, sentinel) {
					t.Fatalf("NormalizeURI(%q) error = %v, want class %s", tc.Input, err, tc.Error)
				}
				return
			}

			if err != nil {
				t.Fatalf("NormalizeURI(%q) unexpected error: %v", tc.Input, err)
			}
			if got != tc.Output {
				t.Errorf("NormalizeURI(%q)\n got %q\nwant %q", tc.Input, got, tc.Output)
			}
			if !ValidURI(got) {
				t.Errorf("NormalizeURI(%q) = %q, which still fails the atproto uri format", tc.Input, got)
			}
			// Every successful vector must also be a fixed point.
			again, err := NormalizeURI(got)
			if err != nil || again != got {
				t.Errorf("not idempotent: %q -> %q -> %q (err=%v)", tc.Input, got, again, err)
			}
		})
	}
}

// TestValidURIMatchesLexiconFormat pins the behaviour the whole fix is built on:
// the indigo parser that third-party validators use rejects raw non-ASCII.
func TestValidURIMatchesLexiconFormat(t *testing.T) {
	if ValidURI("https://example.com/pokémon") {
		t.Error("expected a raw accented character to fail the atproto uri format")
	}
	if !ValidURI("https://example.com/pok%C3%A9mon") {
		t.Error("expected the percent-encoded form to satisfy the atproto uri format")
	}
	if ValidURI("https://example.com/with space") {
		t.Error("expected a space to fail the atproto uri format")
	}
	if ValidURI("HTTPS://example.com/a") {
		t.Error("expected an uppercase scheme to fail the atproto uri format")
	}
}

// TestEscapeNonGraphBytes exercises the encoder directly, including the exact
// boundaries of the printable-ASCII range. An off-by-one in graphLow/graphHigh
// would silently either mangle legal characters or emit illegal ones, and the
// end-to-end vectors would not localise it.
func TestEscapeNonGraphBytes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "plain", "plain"},
		{"query syntax is not touched", "a=1&b=2", "a=1&b=2"},
		{"existing escape is not re-encoded", "already%C3%A9", "already%C3%A9"},
		{"reserved chars are preserved", "a%2Fb/c:d@e+f;g", "a%2Fb/c:d@e+f;g"},
		{"accented char", "é", "%C3%A9"},
		{"space is below the range", "a b", "a%20b"},
		{"tab is below the range", "tab\there", "tab%09here"},
		{"newline is below the range", "a\nb", "a%0Ab"},
		{"nul is below the range", "a\x00b", "a%00b"},
		{"0x20 is escaped (just below graphLow)", "\x20", "%20"},
		{"0x21 is preserved (graphLow)", "\x21", "!"},
		{"0x7E is preserved (graphHigh)", "\x7e", "~"},
		{"0x7F is escaped (just above graphHigh)", "\x7f", "%7F"},
		{"full printable range passes through", "!\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}~", "!\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}~"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeNonGraphBytes(tt.in)
			if got != tt.want {
				t.Errorf("escapeNonGraphBytes(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Applying the encoder twice must not change the result. Unlike an
			// end-to-end idempotence check this cannot be satisfied by the
			// already-conforming short circuit, so it genuinely pins that '%'
			// is never re-encoded.
			if again := escapeNonGraphBytes(got); again != got {
				t.Errorf("escapeNonGraphBytes not idempotent: %q -> %q -> %q", tt.in, got, again)
			}
		})
	}
}

// TestNormalizeURIErrorsAreDescriptive keeps the failure paths actionable: a
// client that gets a 400 must be told what to fix, and each distinct cause must
// be distinguishable rather than collapsing into one opaque sentinel.
func TestNormalizeURIErrorsAreDescriptive(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErr     error
		wantMention string
	}{
		{"missing scheme", "example.com/path", ErrURINoScheme, "scheme"},
		{"scheme with digits", "s3://bucket/key", ErrURIBadScheme, "s3"},
		{"forbidden scheme", "javascript:alert(1)", ErrURISchemeNotAllowed, "javascript"},
		{"non-web scheme", "ftp://example.com/x", ErrURISchemeNotAllowed, "ftp"},
		{"no authority", "https:isbn:123", ErrURINoAuthority, "authority"},
		{"empty host", "https:///path", ErrURINoAuthority, "host"},
		{"too long", "https://example.com/" + strings.Repeat("a", 9000), ErrURITooLong, "max"},
		{"unresolvable host", "https://ä..com/x", ErrURIUnnormalizable, "punycode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeURI(tt.input)
			if err == nil {
				t.Fatalf("NormalizeURI(%.40q) = nil, want %v", tt.input, tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NormalizeURI(%.40q) error = %v, want %v", tt.input, err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMention) {
				t.Errorf("error %q should mention %q", err, tt.wantMention)
			}
		})
	}
}

// TestNormalizeURIPreservesReservedEscapes guards the regression that made the
// first version of this normalizer silently repoint links: round-tripping
// through net/url decoded %2F into a path separator, so a URI naming one
// resource came back naming another.
func TestNormalizeURIPreservesReservedEscapes(t *testing.T) {
	for _, escape := range []string{"%2F", "%3A", "%40", "%2B", "%3B", "%3F", "%23", "%26"} {
		input := "https://example.com/a" + escape + "b/café"
		want := "https://example.com/a" + escape + "b/caf%C3%A9"
		got, err := NormalizeURI(input)
		if err != nil {
			t.Fatalf("NormalizeURI(%q) unexpected error: %v", input, err)
		}
		if got != want {
			t.Errorf("NormalizeURI(%q)\n got %q\nwant %q (escape must survive)", input, got, want)
		}
	}
}
