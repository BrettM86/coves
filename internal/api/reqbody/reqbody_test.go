package reqbody

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type testPayload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// decode runs DecodeJSON against a synthetic POST carrying body.
func decode(t *testing.T, body string, limit Limit, dst any, opts ...DecodeOption) error {
	t.Helper()
	r := httptest.NewRequest("POST", "/xrpc/test", strings.NewReader(body))
	w := httptest.NewRecorder()
	return DecodeJSON(w, r, limit, dst, opts...)
}

func TestDecodeJSON_ValidBody(t *testing.T) {
	var got testPayload
	if err := decode(t, `{"name":"coves","count":3}`, LimitTiny, &got); err != nil {
		t.Fatalf("DecodeJSON returned error for valid body: %v", err)
	}
	if got.Name != "coves" || got.Count != 3 {
		t.Fatalf("decoded payload = %+v, want {coves 3}", got)
	}
}

func TestDecodeJSON_TrailingWhitespaceAccepted(t *testing.T) {
	var got testPayload
	if err := decode(t, "{\"name\":\"x\"} \n\t", LimitTiny, &got); err != nil {
		t.Fatalf("trailing whitespace must be accepted, got error: %v", err)
	}
}

func TestDecodeJSON_OverLimitReturnsTooLarge(t *testing.T) {
	// A single long JSON string is the exact memory-amplification shape the
	// limit exists to stop.
	body := `{"name":"` + strings.Repeat("a", int(LimitTiny)) + `"}`
	var got testPayload
	err := decode(t, body, LimitTiny, &got)

	var tooLarge *TooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("want *TooLargeError, got %T: %v", err, err)
	}
	if tooLarge.Limit != int64(LimitTiny) {
		t.Fatalf("TooLargeError.Limit = %d, want %d", tooLarge.Limit, int64(LimitTiny))
	}
}

// TestDecodeJSON_OverLimitPaddingReturnsTooLarge pins the failure-open bug the
// original dec.More() implementation had: a small, valid JSON value padded
// past the cap with whitespace was ACCEPTED — More() swallowed the
// MaxBytesReader error, so no 413 was written and nothing was logged. The
// clean-EOF check must classify that read error as TooLargeError.
func TestDecodeJSON_OverLimitPaddingReturnsTooLarge(t *testing.T) {
	body := `{"name":"a"}` + strings.Repeat(" ", int(LimitTiny)*2)
	var got testPayload
	err := decode(t, body, LimitTiny, &got)

	var tooLarge *TooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("padded over-limit body must be rejected as too large, got %T: %v", err, err)
	}
}

// TestDecodeJSON_ValueAtLimitTrailingObjectRejected is the smuggling variant
// of the same bug: value ends inside the cap, a second object hides beyond
// it. The rejection may classify as either size or trailing data, but it must
// never be a success.
func TestDecodeJSON_ValueAtLimitTrailingObjectRejected(t *testing.T) {
	body := `{"name":"a"}` + strings.Repeat(" ", int(LimitTiny)) + `{"name":"EVIL"}`
	var got testPayload
	if err := decode(t, body, LimitTiny, &got); err == nil {
		t.Fatal("a trailing object hidden beyond the size limit was silently accepted")
	}
}

func TestDecodeJSON_ExactlyAtLimitAccepted(t *testing.T) {
	// http.MaxBytesReader permits exactly n bytes; the failure must come
	// only from byte n+1.
	prefix, suffix := `{"name":"`, `"}`
	fill := int(LimitTiny) - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("a", fill) + suffix
	if len(body) != int(LimitTiny) {
		t.Fatalf("test bug: body is %d bytes, want %d", len(body), int64(LimitTiny))
	}

	var got testPayload
	if err := decode(t, body, LimitTiny, &got); err != nil {
		t.Fatalf("body exactly at limit must decode, got error: %v", err)
	}
}

// TestDecodeJSON_OneByteOverLimitRejected pins the boundary tightly: the
// exact-limit test above plus a far-over test would both pass an off-by-one
// that admits limit+1.
func TestDecodeJSON_OneByteOverLimitRejected(t *testing.T) {
	prefix, suffix := `{"name":"`, `"}`
	fill := int(LimitTiny) - len(prefix) - len(suffix) + 1
	body := prefix + strings.Repeat("a", fill) + suffix
	if len(body) != int(LimitTiny)+1 {
		t.Fatalf("test bug: body is %d bytes, want %d", len(body), int64(LimitTiny)+1)
	}

	var got testPayload
	var tooLarge *TooLargeError
	if err := decode(t, body, LimitTiny, &got); !errors.As(err, &tooLarge) {
		t.Fatalf("body one byte over the limit must be too large, got %v", err)
	}
}

func TestDecodeJSON_MalformedSyntax(t *testing.T) {
	var got testPayload
	err := decode(t, `{"name":`, LimitTiny, &got)

	var malformed *MalformedError
	if !errors.As(err, &malformed) {
		t.Fatalf("want *MalformedError for truncated JSON, got %T: %v", err, err)
	}
}

func TestDecodeJSON_EmptyBody(t *testing.T) {
	var got testPayload
	var malformed *MalformedError
	if err := decode(t, "", LimitTiny, &got); !errors.As(err, &malformed) {
		t.Fatalf("want *MalformedError for empty body, got %v", err)
	}
}

func TestDecodeJSON_TypeMismatch(t *testing.T) {
	var got testPayload
	var malformed *MalformedError
	if err := decode(t, `{"count":"not-a-number"}`, LimitTiny, &got); !errors.As(err, &malformed) {
		t.Fatalf("want *MalformedError for type mismatch, got %v", err)
	}
}

func TestDecodeJSON_TrailingDataRejected(t *testing.T) {
	// The stray-bracket cases pin the second dec.More() hole: More() returns
	// false on a closing delimiter, so `{...}}` used to pass as clean.
	for name, body := range map[string]string{
		"second object":         `{"name":"a"}{"name":"b"}`,
		"trailing bytes":        `{"name":"a"}garbage`,
		"stray closing brace":   `{"name":"a"}}`,
		"stray closing bracket": `{"name":"a"}]`,
		"bracket salad":         `{"name":"a"}}}}]]]`,
	} {
		t.Run(name, func(t *testing.T) {
			var got testPayload
			var malformed *MalformedError
			if err := decode(t, body, LimitTiny, &got); !errors.As(err, &malformed) {
				t.Fatalf("want *MalformedError for trailing data, got %v", err)
			}
		})
	}
}

// TestDecodeJSON_TrailingDataCarriesSentinel lets callers count smuggling
// probes separately from ordinary bad JSON.
func TestDecodeJSON_TrailingDataCarriesSentinel(t *testing.T) {
	var got testPayload
	err := decode(t, `{"name":"a"}{"name":"b"}`, LimitTiny, &got)
	if !errors.Is(err, ErrTrailingData) {
		t.Fatalf("trailing-data rejection must wrap ErrTrailingData, got %v", err)
	}
}

func TestDecodeJSON_UnknownFieldsAllowedByDefault(t *testing.T) {
	// atProto tolerates unknown fields; the default must not reject clients
	// sending a newer schema.
	var got testPayload
	if err := decode(t, `{"name":"a","futureField":true}`, LimitTiny, &got); err != nil {
		t.Fatalf("unknown fields must be allowed by default, got error: %v", err)
	}
}

func TestDecodeJSON_DisallowUnknownFieldsOption(t *testing.T) {
	var got testPayload
	err := decode(t, `{"name":"a","probe":true}`, LimitTiny, &got, WithDisallowUnknownFields())

	var malformed *MalformedError
	if !errors.As(err, &malformed) {
		t.Fatalf("want *MalformedError with WithDisallowUnknownFields, got %v", err)
	}
}

// TestDecodeJSON_NonPointerDstPanics: a non-pointer dst is a wiring bug in
// the handler, not client input. It must panic (chi's Recoverer makes that a
// 500) rather than answer 400 to every caller forever.
func TestDecodeJSON_NonPointerDstPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("DecodeJSON with a non-pointer dst must panic")
		}
	}()
	var got testPayload
	_ = decode(t, `{"name":"a"}`, LimitTiny, got) // no & — the bug under test
}

// TestDecodeJSON_NonPositiveLimitPanics: a zero or negative limit would make
// MaxBytesReader reject every request as too large — a dead endpoint blaming
// its clients. Refuse loudly instead.
func TestDecodeJSON_NonPositiveLimitPanics(t *testing.T) {
	for name, limit := range map[string]Limit{"zero": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("DecodeJSON with %s limit must panic", name)
				}
			}()
			var got testPayload
			_ = decode(t, `{"name":"a"}`, limit, &got)
		})
	}
}

// TestLimitOrdering pins the invariant the router relies on: the global
// backstop must never undercut ANY endpoint tier — derived over the full
// tier set, so adding a bigger tier without bumping LimitGlobal fails here
// (and at the compile-time assert in reqbody.go).
func TestLimitOrdering(t *testing.T) {
	tiers := []Limit{LimitTiny, LimitSmall, LimitMedium, LimitLarge, LimitImage}
	for i := 1; i < len(tiers); i++ {
		if tiers[i-1] >= tiers[i] {
			t.Fatalf("size tiers must be strictly increasing; tier %d (%d) >= tier %d (%d)",
				i-1, tiers[i-1], i, tiers[i])
		}
	}
	for _, tier := range tiers {
		if LimitGlobal < tier {
			t.Fatalf("LimitGlobal (%d) must cover every endpoint tier, but tier %d exceeds it",
				int64(LimitGlobal), int64(tier))
		}
	}
}
