package users

import (
	"errors"
	"fmt"
)

// Sentinel errors for common user operations
var (
	// ErrUserNotFound is returned when a user lookup finds no matching record
	ErrUserNotFound = errors.New("user not found")

	// ErrHandleAlreadyTaken is returned when attempting to use a handle that belongs to another user
	ErrHandleAlreadyTaken = errors.New("handle already taken")

	// ErrUserAlreadyExists is returned when a user with the same DID already exists.
	// Callers (e.g. CreateUser) use this with errors.Is to detect the duplicate-DID
	// case and treat the operation as idempotent — fetching and returning the
	// existing user instead of failing.
	ErrUserAlreadyExists = errors.New("user with DID already exists")

	// ErrSignupTokenDisabled signals that the captcha-gated signup-token endpoint
	// is not configured (missing TURNSTILE_SECRET_KEY or PDS_ADMIN_PASSWORD).
	// Distinct from a captcha rejection — operators should see this as a 503,
	// not a 403, so misconfiguration is observable rather than silently hidden
	// behind "captcha failed".
	ErrSignupTokenDisabled = errors.New("signup token endpoint not configured")

	// ErrPDSAdminUnavailable signals a transport/decode-side failure when calling
	// the PDS admin API (network error, body read failure, malformed JSON). This
	// is distinct from InviteMintError, which represents the PDS *responding* with
	// a non-2xx status. Surface to clients as 503 so ops can alert on "PDS is
	// down" separately from "PDS rejected our request".
	ErrPDSAdminUnavailable = errors.New("PDS admin API unavailable")
)

// Domain errors for user service operations
// These map to lexicon error types defined in social.coves.actor.signup

type InvalidHandleError struct {
	Handle string
	Reason string
}

func (e *InvalidHandleError) Error() string {
	return fmt.Sprintf("invalid handle %q: %s", e.Handle, e.Reason)
}

type HandleNotAvailableError struct {
	Handle string
}

func (e *HandleNotAvailableError) Error() string {
	return fmt.Sprintf("handle %q is not available", e.Handle)
}

type InvalidInviteCodeError struct {
	Code string
}

func (e *InvalidInviteCodeError) Error() string {
	return "invalid or expired invite code"
}

type InvalidEmailError struct {
	Email string
}

func (e *InvalidEmailError) Error() string {
	return fmt.Sprintf("invalid email address: %q", e.Email)
}

type WeakPasswordError struct {
	Reason string
}

func (e *WeakPasswordError) Error() string {
	return fmt.Sprintf("password does not meet strength requirements: %s", e.Reason)
}

// PDSError wraps errors from the PDS that we couldn't map to domain errors
type PDSError struct {
	Message    string
	StatusCode int
}

func (e *PDSError) Error() string {
	return fmt.Sprintf("PDS error (%d): %s", e.StatusCode, e.Message)
}

// InvalidDIDError is returned when a DID does not meet format requirements
type InvalidDIDError struct {
	DID    string
	Reason string
}

func (e *InvalidDIDError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("invalid DID %q: %s", e.DID, e.Reason)
	}
	return fmt.Sprintf("invalid DID %q: must start with 'did:'", e.DID)
}

// InvalidCaptchaError is returned when a Turnstile token is *rejected* by Cloudflare
// (success:false). Surface to the client as 403 — the user must retry with a fresh
// token. Compare with CaptchaUnavailableError, which indicates a transport/decode
// failure on our side, not a user-side rejection.
type InvalidCaptchaError struct {
	Reason string
}

func (e *InvalidCaptchaError) Error() string {
	if e.Reason == "" {
		return "captcha verification failed"
	}
	return fmt.Sprintf("captcha verification failed: %s", e.Reason)
}

// CaptchaUnavailableError signals that we could not reach Cloudflare or could not
// parse its response: network error, non-2xx, body-read failure, JSON decode
// failure. Surface to the client as 503 — distinct from a legitimate 403 rejection
// so ops can alert on Cloudflare outages instead of drowning in user-rejection
// noise.
type CaptchaUnavailableError struct {
	Reason string
}

func (e *CaptchaUnavailableError) Error() string {
	if e.Reason == "" {
		return "captcha verification unavailable"
	}
	return fmt.Sprintf("captcha verification unavailable: %s", e.Reason)
}

// maxInviteMintErrorBody caps how much of the PDS response body we retain in
// the error. PDS error bodies can contain server internals; we keep just enough
// to debug while bounding log volume.
const maxInviteMintErrorBody = 512

// InviteMintError wraps a non-success response from the PDS admin createInviteCode
// endpoint. Fields are unexported so the body is always constructed via
// NewInviteMintError — that's the only place that enforces the size cap, and
// letting callers literal-init the struct would silently bypass it.
type InviteMintError struct {
	statusCode int
	body       string
}

// NewInviteMintError truncates the body at maxInviteMintErrorBody bytes.
// Callers should always go through this constructor, not literal-init the struct.
func NewInviteMintError(statusCode int, body string) *InviteMintError {
	if len(body) > maxInviteMintErrorBody {
		body = body[:maxInviteMintErrorBody] + "...[truncated]"
	}
	return &InviteMintError{statusCode: statusCode, body: body}
}

// StatusCode returns the HTTP status the PDS admin endpoint returned.
func (e *InviteMintError) StatusCode() int { return e.statusCode }

// Body returns the (already-truncated) PDS response body.
func (e *InviteMintError) Body() string { return e.body }

func (e *InviteMintError) Error() string {
	return fmt.Sprintf("failed to mint invite code: status %d: %s", e.statusCode, e.body)
}
