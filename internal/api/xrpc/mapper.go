package xrpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/atproto/pds"
	coreerrors "Coves/internal/core/errors"
)

// Mapping is the XRPC error response a Go error resolves to.
//
// Code is the machine-readable contract clients switch on, so it must stay
// stable; Message is human-readable and may be reworded freely. Message must
// never carry internal detail — see the *Detail rule constructors for the
// narrow cases where an error's own text is safe to surface.
type Mapping struct {
	Status  int
	Code    string
	Message string
}

// Rule reports the Mapping for err, or false when it does not apply.
type Rule func(err error) (Mapping, bool)

// Sentinel matches err against target with errors.Is.
//
// errors.Is rather than ==: these sentinels travel up through service layers
// that add context with %w, and an == comparison silently stops matching the
// moment anyone wraps them — degrading a considered 4xx into a 500.
func Sentinel(target error, status int, code, message string) Rule {
	return func(err error) (Mapping, bool) {
		if !errors.Is(err, target) {
			return Mapping{}, false
		}
		return Mapping{Status: status, Code: code, Message: message}, true
	}
}

// SentinelDetail is Sentinel with the error's own text as the message. Same
// caution as MatchDetail: only for errors written for the client to read.
func SentinelDetail(target error, status int, code string) Rule {
	return func(err error) (Mapping, bool) {
		if !errors.Is(err, target) {
			return Mapping{}, false
		}
		return Mapping{Status: status, Code: code, Message: err.Error()}, true
	}
}

// Match answers with a fixed message when pred accepts err. Use it for the
// predicate helpers domain packages already expose (IsValidationError and
// friends).
func Match(pred func(error) bool, status int, code, message string) Rule {
	return func(err error) (Mapping, bool) {
		if !pred(err) {
			return Mapping{}, false
		}
		return Mapping{Status: status, Code: code, Message: message}, true
	}
}

// MatchDetail is Match with the error's own text as the message.
//
// Only for errors whose text is written for the client: validation failures
// naming a bad field, say. Anything that could contain a query, a DID we did
// not receive from the caller, or a driver message belongs in Match with a
// fixed string.
func MatchDetail(pred func(error) bool, status int, code string) Rule {
	return func(err error) (Mapping, bool) {
		if !pred(err) {
			return Mapping{}, false
		}
		return Mapping{Status: status, Code: code, Message: err.Error()}, true
	}
}

// As matches the first error of type T in err's chain and derives the message
// from that error alone.
//
// Preferred over MatchDetail whenever a typed error is available: it reads the
// message off the typed error itself, so wrapper context added upstream
// ("failed to create comment: ...") cannot leak into the client's message.
//
// T must be the concrete type the domain returns, pointer included
// (*posts.ValidationError). Two ways to get it wrong: instantiating with a
// value type whose Error method has a pointer receiver won't compile, and
// As[error] compiles but matches every error in existence, silently becoming a
// catch-all.
func As[T error](status int, code string, message func(T) string) Rule {
	return func(err error) (Mapping, bool) {
		var target T
		if !errors.As(err, &target) {
			return Mapping{}, false
		}
		return Mapping{Status: status, Code: code, Message: message(target)}, true
	}
}

// defaultReauth is the answer when the user's session is dead. Overridable per
// mapper via Mapper.WithReauth, because a few endpoints ship different codes to
// clients already.
var defaultReauth = Mapping{
	Status:  http.StatusUnauthorized,
	Code:    "AuthRequired",
	Message: "Authentication required or session expired",
}

// internalError is the last resort. Its message is deliberately uniform and
// content-free: the cause is logged, never sent.
var internalError = Mapping{
	Status:  http.StatusInternalServerError,
	Code:    "InternalServerError",
	Message: "An internal error occurred",
}

// sharedRules are consulted after a mapper's own rules, in this order. They
// cover the errors that reach every handler identically.
var sharedRules = []Rule{
	// Typed domain errors, read off the typed value so wrapper context cannot
	// leak. One entry each, rather than one per domain package, because the
	// domains alias these types — see internal/core/errors.
	As[*coreerrors.ValidationError](http.StatusBadRequest, "InvalidRequest",
		func(e *coreerrors.ValidationError) string { return e.Error() }),
	As[*coreerrors.NotFoundError](http.StatusNotFound, "NotFound",
		func(e *coreerrors.NotFoundError) string { return e.Error() }),
	As[*coreerrors.ConflictError](http.StatusConflict, "AlreadyExists",
		func(e *coreerrors.ConflictError) string { return e.Error() }),

	// PDS failures other than a dead session, which is handled ahead of the
	// domain rules. 403 is a permissions problem — a missing OAuth scope, say —
	// not an expired session, so it must not trigger a client sign-out; signing
	// in again is nonetheless what re-grants the scope.
	Sentinel(pds.ErrForbidden, http.StatusForbidden, "PermissionDenied",
		"Your session does not have permission for this action. Sign out and back in to grant it."),
	Sentinel(pds.ErrBadRequest, http.StatusBadRequest, "InvalidRequest",
		"Invalid request to PDS"),
	Sentinel(pds.ErrNotFound, http.StatusNotFound, "NotFound",
		"Record not found on PDS"),
	// ErrSwapConflict before ErrConflict: a 409 InvalidSwap wraps both
	// sentinels, and the lost-swap message is the more actionable one. A 400
	// InvalidSwap wraps only ErrSwapConflict, so without this rule it would
	// fall through to the generic 500 — a lost race reported as our failure.
	Sentinel(pds.ErrSwapConflict, http.StatusConflict, "Conflict",
		"Record was modified by another operation, please retry"),
	Sentinel(pds.ErrConflict, http.StatusConflict, "Conflict",
		"Record was modified by another operation"),
	Sentinel(pds.ErrPayloadTooLarge, http.StatusRequestEntityTooLarge, "PayloadTooLarge",
		"Request payload exceeds size limit"),
	Sentinel(pds.ErrRateLimited, http.StatusTooManyRequests, "RateLimitExceeded",
		"Too many requests, please try again later"),
	// A PDS 5xx is a classified upstream failure, not our internal error, so it
	// answers 502 rather than falling through to internalError — the same call
	// the image proxy makes for a PDS it cannot reach. The message is fixed:
	// the PDS's own text may carry internal detail we must not forward.
	Sentinel(pds.ErrServerError, http.StatusBadGateway, "UpstreamFailure",
		"PDS failed to process the request"),

	// Request lifecycle. A cancellation is the client's own doing, so it is a
	// 4xx; a deadline we blew is ours to report as a gateway timeout.
	Sentinel(context.DeadlineExceeded, http.StatusGatewayTimeout, "Timeout",
		"Request timed out"),
	Sentinel(context.Canceled, http.StatusBadRequest, "RequestCanceled",
		"Request was canceled"),
}

// Mapper resolves domain errors to XRPC error responses for one handler
// package.
//
// Every handler package used to carry its own near-identical copy of this
// switch. Because each copy mapped a different subset, an error a package
// forgot fell through to a 500 — most damagingly a dead OAuth session on
// post, comment, and vote, which left clients with no signal to re-authenticate
// and no way out but a manual sign-out. A mapper owns only the rules unique to
// its domain and inherits the rest.
type Mapper struct {
	domain   string
	rules    []Rule
	reauth   Mapping
	internal Mapping
}

// NewMapper builds a mapper for a handler package. domain names it for logs.
// rules are the domain-specific mappings, tried in order.
func NewMapper(domain string, rules ...Rule) *Mapper {
	return &Mapper{domain: domain, rules: rules, reauth: defaultReauth, internal: internalError}
}

// WithFallback overrides the answer for an error no rule matched, returning a
// derived mapper.
//
// Two uses. An operation that already ships a more specific 500 code to
// clients — a blob upload that failed for reasons we could not classify. And a
// terminal step where every possible failure has the same meaning: if building
// a PDS client for a user fails at all, there is no usable session, whatever
// the cause, so 401 is the honest answer rather than a guess.
//
// Prefer adding a rule for the cause over widening the fallback. Whatever the
// status, an unmatched error is still logged.
func (m *Mapper) WithFallback(status int, code, message string) *Mapper {
	derived := *m
	derived.internal = Mapping{Status: status, Code: code, Message: message}
	return &derived
}

// WithReauth overrides the code and message used when the session is dead,
// returning a derived mapper. The status stays 401 — that is the part clients
// act on.
func (m *Mapper) WithReauth(code, message string) *Mapper {
	derived := *m
	derived.reauth = Mapping{Status: http.StatusUnauthorized, Code: code, Message: message}
	return &derived
}

// With returns a derived mapper whose extra rules are tried before the domain
// rules, for a single operation that needs a more specific answer than its
// package's default — distinguishing an oversized avatar from an oversized
// banner, say.
//
// The extra rules cannot displace the re-authentication check, which stays
// ahead of everything.
func (m *Mapper) With(rules ...Rule) *Mapper {
	derived := *m
	// Fresh backing array: appending onto m.rules would let two derived
	// mappers overwrite each other's rules.
	derived.rules = make([]Rule, 0, len(rules)+len(m.rules))
	derived.rules = append(derived.rules, rules...)
	derived.rules = append(derived.rules, m.rules...)
	return &derived
}

// Resolve returns the response for err and reports whether any rule matched.
//
// A false result means no rule matched and the Mapping is this mapper's
// fallback — the generic 500 unless WithFallback changed it — so it says
// nothing about the cause and the caller must log err. Rules are tried in this
// order:
//
//  1. re-authentication required
//  2. the mapper's own rules, in the order given
//  3. shared rules: typed domain errors, other PDS failures, request lifecycle
//
// Re-authentication comes first on purpose. Services translate a PDS auth
// failure into their own sentinel — votes.ErrNotAuthorized, say — which maps to
// 403; were that consulted first, an expired session would answer 403 and the
// client would never learn to sign in again. Nothing else needs to jump the
// queue, so a domain rule still beats every shared rule.
func (m *Mapper) Resolve(err error) (Mapping, bool) {
	if err == nil {
		return m.internal, false
	}

	if pds.IsReauthRequired(err) {
		return m.reauth, true
	}

	for _, rule := range m.rules {
		if mapping, ok := rule(err); ok {
			return mapping, true
		}
	}

	for _, rule := range sharedRules {
		if mapping, ok := rule(err); ok {
			return mapping, true
		}
	}

	return m.internal, false
}

// Write resolves err and writes the response, logging anything unmapped.
func (m *Mapper) Write(w http.ResponseWriter, err error) {
	if err == nil {
		// A handler reached its error path without an error. Answering 500 is
		// wrong but at least terminates the request; a silent return would
		// leave the client with an empty 200.
		slog.Error("error mapper invoked with nil error", "domain", m.domain)
		WriteError(w, m.internal.Status, m.internal.Code, m.internal.Message)
		return
	}

	mapping, matched := m.Resolve(err)
	if !matched {
		slog.Error("unmapped handler error", "domain", m.domain, "error", err)
	}
	WriteError(w, mapping.Status, mapping.Code, mapping.Message)
}
