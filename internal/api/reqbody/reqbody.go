// Package reqbody is the single place request-body size limits and JSON
// decoding live. Every handler that reads a JSON request body goes through
// DecodeJSON so the cap, the 413 semantics, and the trailing-data rejection
// are uniform across the API surface — scripts/test-audit.sh (run inside
// `make ci`) counts any raw request-body decode outside this package as a
// violation.
//
// The package is a dependency-free leaf on purpose: internal/api/xrpc already
// depends on internal/atproto/oauth (transitively), so a helper living in
// xrpc could never be used by the OAuth handlers without an import cycle.
// Both import this instead.
package reqbody

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Limit is a request-body cap in bytes. It is a named type so call sites read
// as a tier choice rather than an arbitrary integer; DecodeJSON panics on a
// non-positive value instead of quietly rejecting every request (that is what
// http.MaxBytesReader would do with a zero cap).
type Limit int64

// Size tiers for request bodies. Handlers pick the smallest tier that fits
// their legitimate payload — an attacker then can't spend more of our memory
// than the endpoint's real traffic ever would. The tiers are described by
// payload shape, not by endpoint list, so the docs don't rot as endpoints
// come and go.
const (
	// LimitTiny fits token-and-identifier payloads: a captcha token, a vote
	// subject, a record URI. Legitimate bodies are well under 2 KB.
	LimitTiny Limit = 4 << 10 // 4 KiB

	// LimitSmall fits short structured requests: a subject DID or handle
	// plus a few scalar fields (block/subscribe targets, reports, session
	// refresh, registration metadata).
	LimitSmall Limit = 10 << 10 // 10 KiB

	// LimitMedium fits user-authored text without inline media: comments,
	// suggestion bodies.
	LimitMedium Limit = 100 << 10 // 100 KiB

	// LimitLarge fits the biggest text payloads: full post bodies with
	// embeds and facets. Post embeds carry blob references (CIDs), never
	// image bytes.
	LimitLarge Limit = 1 << 20 // 1 MiB

	// LimitImage fits base64-encoded image payloads: the profile AND
	// community endpoints accept inline avatar/banner bytes, up to 1 MB +
	// 2 MB raw per their lexicons, which is ~4 MB as base64 plus the
	// record's text fields. 10 MB leaves comfortable slack.
	LimitImage Limit = 10_000_000

	// LimitGlobal is the router-wide backstop applied to every request
	// before any handler runs. It must stay >= the largest per-endpoint
	// tier (enforced at compile time below); per-endpoint tiers do the
	// real work under it.
	LimitGlobal Limit = LimitImage
)

// Compile-time guard: if LimitGlobal ever undercuts the largest endpoint
// tier, the conversion below goes negative and the package stops compiling.
const _ = uint64(LimitGlobal - LimitImage)

// ErrTrailingData is the cause carried by the MalformedError returned when
// bytes follow the decoded JSON value — concatenated objects, padding, or a
// stray closing bracket. Callers that want to count smuggling probes
// separately from ordinary bad JSON can errors.Is against it.
var ErrTrailingData = errors.New("trailing data after JSON value")

// TooLargeError reports a request body that exceeded the endpoint's limit.
// Handlers map it to 413 Payload Too Large. Limit is the cap that actually
// tripped — under the router backstop plus an endpoint tier, that is
// whichever wrapper the read exhausted first.
type TooLargeError struct {
	Limit int64

	err error
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %d byte limit", e.Limit)
}

// Unwrap exposes the underlying *http.MaxBytesError so errors.As chains
// written against the stdlib type keep working.
func (e *TooLargeError) Unwrap() error {
	return e.err
}

// MalformedError reports a body that was readable but not a single valid
// JSON value: syntax errors, type mismatches, trailing data, or unknown
// fields when WithDisallowUnknownFields is set. Handlers map it to 400.
type MalformedError struct {
	err error
}

func (e *MalformedError) Error() string {
	if e == nil || e.err == nil {
		return "malformed request body"
	}
	return "malformed request body: " + e.err.Error()
}

func (e *MalformedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// DecodeOption adjusts DecodeJSON's strictness.
type DecodeOption func(*decodeConfig)

type decodeConfig struct {
	disallowUnknownFields bool
}

// WithDisallowUnknownFields rejects bodies containing JSON fields the
// destination struct does not declare. Off by default: atProto tolerates
// unknown fields, and older clients must not break when the schema grows.
// Turn it on for bot-facing endpoints where unexpected fields are a probe.
func WithDisallowUnknownFields() DecodeOption {
	return func(c *decodeConfig) { c.disallowUnknownFields = true }
}

// DecodeJSON caps r.Body at limit bytes and decodes exactly one JSON value
// into dst, which must be a non-nil pointer (a non-pointer is a programmer
// error and panics rather than masquerading as a client 400).
//
// The cap uses http.MaxBytesReader rather than io.LimitReader because it
// yields a typed *http.MaxBytesError (surfaced here as *TooLargeError)
// instead of silently truncating into a confusing syntax error. (Its second
// defense — telling the server to close the connection — does not fire in
// this stack: chi's middleware wraps the ResponseWriter without forwarding
// net/http's private requestTooLarge signal. net/http still bounds how much
// of a rejected body it drains, so the miss costs nothing material.)
//
// DecodeJSON replaces r.Body with the capped reader, so any later read of
// the body in the same handler stays bounded too.
//
// After the value, the body must be at clean EOF. Trailing data
// (concatenated objects, padding, stray brackets) is a smuggling smell and
// returns a *MalformedError wrapping ErrTrailingData — and because the check
// reads, not peeks-and-drops, an over-limit body can never sneak through as
// a false success: the size error is classified, never swallowed.
func DecodeJSON(w http.ResponseWriter, r *http.Request, limit Limit, dst any, opts ...DecodeOption) error {
	if limit <= 0 {
		panic(fmt.Sprintf("reqbody.DecodeJSON: limit must be positive, got %d", int64(limit)))
	}

	var cfg decodeConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(limit))

	dec := json.NewDecoder(r.Body)
	if cfg.disallowUnknownFields {
		dec.DisallowUnknownFields()
	}

	if err := dec.Decode(dst); err != nil {
		return classifyDecodeError(err, dst)
	}

	// dec.More() is the wrong tool here: it swallows read errors (an
	// over-limit body would look like a clean end) and returns false on a
	// stray closing bracket. Reading one more token surfaces all three
	// outcomes distinctly: io.EOF is the only clean result.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return &MalformedError{err: ErrTrailingData}
		}
		return classifyDecodeError(err, dst)
	}

	return nil
}

// classifyDecodeError maps a json.Decoder error to the package's typed
// errors, panicking on the one shape that is a server bug rather than
// client input.
func classifyDecodeError(err error, dst any) error {
	var invalidUnmarshal *json.InvalidUnmarshalError
	if errors.As(err, &invalidUnmarshal) {
		// A non-pointer or nil dst would otherwise surface as 400
		// "invalid request body" on every call — a dead endpoint whose
		// logs blame the clients. chi's Recoverer turns this into a 500
		// and a stack trace, which is what a wiring bug deserves.
		panic(fmt.Sprintf("reqbody.DecodeJSON: dst must be a non-nil pointer, got %T", dst))
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return &TooLargeError{Limit: maxBytesErr.Limit, err: maxBytesErr}
	}
	return &MalformedError{err: err}
}
