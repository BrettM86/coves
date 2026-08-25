package user

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/core/users"
)

// RequestSignupTokenHandler serves the social.coves.actor.requestSignupToken
// XRPC endpoint: it verifies a Cloudflare Turnstile token and, on success,
// mints a single-use PDS invite code that the client then redeems via signup.
type RequestSignupTokenHandler struct {
	userService users.UserService
}

// NewRequestSignupTokenHandler constructs a RequestSignupTokenHandler bound to
// the given UserService, which carries the Turnstile verifier and PDS admin
// client used to mint invites.
func NewRequestSignupTokenHandler(userService users.UserService) *RequestSignupTokenHandler {
	return &RequestSignupTokenHandler{userService: userService}
}

// HandleRequestSignupToken serves POST requests to the signup-token endpoint.
// It enforces a body size limit, decodes the request, verifies the captcha via
// the user service, and returns the minted invite code as JSON. All domain
// errors are mapped to safe HTTP responses by mapSignupTokenError — captcha
// internals are never surfaced to the client.
func (h *RequestSignupTokenHandler) HandleRequestSignupToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	clientIP := middleware.GetClientIP(r)

	// This endpoint is pure bot bait, so rejections log the client IP with
	// endpoint-specific message prefixes — bespoke handling beyond the shared
	// xrpc.DecodeJSON wrapper. Unknown fields are rejected too: a real client
	// sends exactly one field (a Turnstile token, ~1-2 KB, so the tiny tier).
	var req users.RequestSignupTokenRequest
	if err := reqbody.DecodeJSON(w, r, reqbody.LimitTiny, &req, reqbody.WithDisallowUnknownFields()); err != nil {
		var tooLarge *reqbody.TooLargeError
		if errors.As(err, &tooLarge) {
			slog.Info("request signup token: body over limit",
				slog.String("client_ip", clientIP),
				slog.Int64("limit", tooLarge.Limit),
			)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Request body too large")
			return
		}
		// Security event — invalid input from a presumed-bot client.
		slog.Info("request signup token: decode failed",
			slog.String("client_ip", clientIP),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Always source RemoteIP from the request headers — never trust client-supplied value.
	req.RemoteIP = clientIP

	resp, err := h.userService.RequestSignupToken(r.Context(), req)
	if err != nil {
		mapSignupTokenError(w, err, clientIP)
		return
	}

	// Audit log success — never log the invite code itself.
	slog.Info("request signup token: invite minted",
		slog.String("client_ip", clientIP),
	)

	responseBytes, err := json.Marshal(resp)
	if err != nil {
		// Invite was already minted but we cannot encode the response — the
		// client will retry, burning another invite. Log loudly for ops.
		slog.Error("failed to marshal signup token response (orphaned invite)",
			slog.String("client_ip", clientIP),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		// Same orphan-invite scenario: caller didn't get the code we issued.
		slog.Error("failed to write signup token response (orphaned invite)",
			slog.String("client_ip", clientIP),
			slog.String("error", writeErr.Error()),
		)
	}
}

// mapSignupTokenError converts domain errors into JSON error responses.
// SECURITY: never surface captcha verification internals — clients learn only
// that the captcha was rejected, not why.
func mapSignupTokenError(w http.ResponseWriter, err error, clientIP string) {
	var (
		invalidCaptcha     *users.InvalidCaptchaError
		captchaUnavailable *users.CaptchaUnavailableError
		inviteMint         *users.InviteMintError
	)

	switch {
	case errors.Is(err, users.ErrSignupTokenDisabled):
		// Misconfiguration — distinct from a captcha rejection so ops can alert.
		slog.Error("request signup token: endpoint not configured",
			slog.String("client_ip", clientIP),
		)
		writeJSONError(w, http.StatusServiceUnavailable, "SignupTokenDisabled", "Signup is temporarily unavailable")

	case errors.Is(err, users.ErrPDSAdminUnavailable):
		// PDS unreachable / transport-layer failure — distinct from PDS-responded-non-2xx
		// (which is InviteMintError). 503 so clients back off; ops can alert on
		// this sentinel separately from generic 500s.
		slog.Warn("request signup token: PDS admin unavailable",
			slog.String("client_ip", clientIP),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusServiceUnavailable, "PDSUnavailable", "Signup invite service unavailable, please try again")

	case errors.As(err, &captchaUnavailable):
		// Cloudflare outage / transport failure — surface 503 so clients back off
		// instead of treating it as user error.
		slog.Warn("request signup token: captcha verifier unavailable",
			slog.String("client_ip", clientIP),
			slog.String("reason", captchaUnavailable.Reason),
		)
		writeJSONError(w, http.StatusServiceUnavailable, "CaptchaUnavailable", "Captcha service unavailable, please try again")

	case errors.As(err, &invalidCaptcha):
		writeJSONError(w, http.StatusForbidden, "InvalidCaptcha", "Captcha verification failed")

	case errors.As(err, &inviteMint):
		// Body is already truncated by NewInviteMintError.
		slog.Error("request signup token: PDS admin mint failed",
			slog.Int("pds_status", inviteMint.StatusCode()),
			slog.String("pds_body", inviteMint.Body()),
			slog.String("client_ip", clientIP),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "Failed to issue invite")

	default:
		slog.Error("request signup token: unexpected error",
			slog.String("client_ip", clientIP),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
