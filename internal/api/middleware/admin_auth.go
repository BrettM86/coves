package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// InstanceAdminAuthority answers whether an authenticated DID is on the
// operator-managed instance admin allowlist.
type InstanceAdminAuthority interface {
	IsInstanceAdmin(did string) bool
}

// InstanceAdminMiddleware authenticates a caller by PDS service JWT or OAuth
// sealed session. RequireInstanceAdmin additionally requires an instance
// admin; RequireAuthenticated does not.
type InstanceAdminMiddleware struct {
	unsealer  SessionUnsealer
	store     oauthlib.ClientAuthStore
	validator ServiceAuthValidator
	authority InstanceAdminAuthority
}

// NewInstanceAdminMiddleware builds both gates over the existing
// authentication primitives: RequireInstanceAdmin authenticates the caller and
// requires an instance admin; RequireAuthenticated only authenticates.
func NewInstanceAdminMiddleware(unsealer SessionUnsealer, store oauthlib.ClientAuthStore, validator ServiceAuthValidator, authority InstanceAdminAuthority) *InstanceAdminMiddleware {
	return &InstanceAdminMiddleware{unsealer: unsealer, store: store, validator: validator, authority: authority}
}

// RequireInstanceAdmin refuses any request whose caller is not a verified
// instance admin.
func (m *InstanceAdminMiddleware) RequireInstanceAdmin(next http.Handler) http.Handler {
	return m.gate(next, true)
}

// RequireAuthenticated authenticates the caller with the same credentials and
// refusals as RequireInstanceAdmin, without requiring instance admin
// authority. It serves endpoints that answer about the caller's own authority.
func (m *InstanceAdminMiddleware) RequireAuthenticated(next http.Handler) http.Handler {
	return m.gate(next, false)
}

// gate authenticates the caller by service JWT bound to the request's XRPC
// method or by sealed session, optionally requires instance admin authority,
// and passes the authenticated DID to next.
func (m *InstanceAdminMiddleware) gate(next http.Handler, requireAdmin bool) http.Handler {
	refuse := refuseInstanceAdmin
	if !requireAdmin {
		refuse = refuseUnauthenticated
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if header := r.Header.Get("Authorization"); header != "" {
			var ok bool
			token, ok = extractBearerToken(header)
			if !ok {
				refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid Authorization header", "invalid_authorization_header", "", nil)
				return
			}
		} else if cookie, err := r.Cookie("coves_session"); err == nil {
			token = cookie.Value
		}
		if token == "" {
			refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Missing authentication", "missing_authentication", "", nil)
			return
		}

		var did, authMethod string
		ctx := r.Context()
		if isJWTFormat(token) {
			if m.validator == nil {
				refuse(w, r, http.StatusInternalServerError, "InternalServerError", "Service authentication is not configured", "missing_service_validator", "", nil)
				return
			}
			methodName, ok := strings.CutPrefix(r.URL.Path, "/xrpc/")
			if !ok {
				refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid service authentication", "missing_service_method", "", nil)
				return
			}
			method, err := syntax.ParseNSID(methodName)
			if err != nil {
				refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid service method", "invalid_service_method", "", err)
				return
			}
			issuer, err := m.validator.Validate(ctx, token, &method)
			if err != nil {
				refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid or expired service JWT", "invalid_service_jwt", "", err)
				return
			}
			did, authMethod = issuer.String(), AuthMethodServiceJWT
		} else {
			auth := authenticateSealedSession(ctx, token, m.unsealer, m.store)
			if auth.failure == sealedSessionStoreFailure {
				refuse(w, r, http.StatusServiceUnavailable, "ModerationUnavailable", "Session lookup temporarily unavailable", auth.failure.reason(), auth.did, auth.err)
				return
			}
			if auth.failure != sealedSessionNoFailure {
				refuse(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid or expired session", auth.failure.reason(), auth.did, auth.err)
				return
			}
			did, authMethod = auth.did, AuthMethodOAuth
			ctx = context.WithValue(ctx, OAuthSessionKey, auth.session)
			ctx = context.WithValue(ctx, UserAccessToken, auth.session.AccessToken)
		}

		if requireAdmin && (m.authority == nil || !m.authority.IsInstanceAdmin(did)) {
			refuse(w, r, http.StatusForbidden, "Forbidden", "Instance admin authority required", "not_instance_admin", did, nil)
			return
		}
		ctx = context.WithValue(ctx, UserDIDKey, did)
		ctx = context.WithValue(ctx, AuthMethodKey, authMethod)
		ctx = context.WithValue(ctx, IsAggregatorAuthKey, false)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refuseInstanceAdmin logs and writes a refusal from the instance admin gate.
// A server-side failure (5xx) logs at Error; a refused caller logs at Warn.
func refuseInstanceAdmin(w http.ResponseWriter, r *http.Request, status int, code, message, reason, did string, cause error) {
	writeGateRefusal(w, r, slog.LevelWarn, "instance admin request refused", status, code, message, reason, did, cause)
}

// refuseUnauthenticated logs and writes a refusal from the authenticate-only
// gate. This gate serves the viewer's own-authority query, so a missing
// credential, an unknown session, or an expired or undecryptable sealed token
// is expected and logs at Debug. Any other refused credential (for example a
// session DID mismatch or a rejected service JWT) logs at Warn, and a
// server-side failure (5xx) logs at Error.
func refuseUnauthenticated(w http.ResponseWriter, r *http.Request, status int, code, message, reason, did string, cause error) {
	level := slog.LevelWarn
	switch reason {
	case "missing_authentication", sealedSessionNotFound.reason(), sealedSessionUnsealFailed.reason():
		level = slog.LevelDebug
	}
	writeGateRefusal(w, r, level, "authenticated request refused", status, code, message, reason, did, cause)
}

// writeGateRefusal logs a refusal at callerLevel, or at Error for a
// server-side failure (5xx), and writes the XRPC error body. The client IP is
// not logged.
func writeGateRefusal(w http.ResponseWriter, r *http.Request, callerLevel slog.Level, logMessage string, status int, code, message, reason, did string, cause error) {
	attributes := []any{"method", r.Method, "path", r.URL.Path, "reason", reason}
	if did != "" {
		attributes = append(attributes, "did", did)
	}
	if cause != nil {
		attributes = append(attributes, "error", cause)
	}
	level := callerLevel
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	slog.Log(r.Context(), level, logMessage, attributes...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{Error: code, Message: message}); err != nil {
		slog.Warn("failed to write instance admin error response", "error", err)
	}
}
