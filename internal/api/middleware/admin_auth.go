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
// sealed session and then requires the DID to be an instance admin.
type InstanceAdminMiddleware struct {
	unsealer  SessionUnsealer
	store     oauthlib.ClientAuthStore
	validator ServiceAuthValidator
	authority InstanceAdminAuthority
}

// NewInstanceAdminMiddleware builds the admin gate over the existing
// authentication primitives.
func NewInstanceAdminMiddleware(unsealer SessionUnsealer, store oauthlib.ClientAuthStore, validator ServiceAuthValidator, authority InstanceAdminAuthority) *InstanceAdminMiddleware {
	return &InstanceAdminMiddleware{unsealer: unsealer, store: store, validator: validator, authority: authority}
}

// RequireInstanceAdmin refuses any request whose caller is not a verified
// instance admin.
func (m *InstanceAdminMiddleware) RequireInstanceAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if header := r.Header.Get("Authorization"); header != "" {
			var ok bool
			token, ok = extractBearerToken(header)
			if !ok {
				refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid Authorization header", "invalid_authorization_header", "", nil)
				return
			}
		} else if cookie, err := r.Cookie("coves_session"); err == nil {
			token = cookie.Value
		}
		if token == "" {
			refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Missing authentication", "missing_authentication", "", nil)
			return
		}

		var did, authMethod string
		ctx := r.Context()
		if isJWTFormat(token) {
			methodName, ok := strings.CutPrefix(r.URL.Path, "/xrpc/")
			if !ok || m.validator == nil {
				refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid service authentication", "missing_service_validator_or_method", "", nil)
				return
			}
			method, err := syntax.ParseNSID(methodName)
			if err != nil {
				refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid service method", "invalid_service_method", "", err)
				return
			}
			issuer, err := m.validator.Validate(ctx, token, &method)
			if err != nil {
				refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid or expired service JWT", "invalid_service_jwt", "", err)
				return
			}
			did, authMethod = issuer.String(), AuthMethodServiceJWT
		} else {
			auth := authenticateSealedSession(ctx, token, m.unsealer, m.store)
			if auth.failure == sealedSessionStoreFailure {
				refuseInstanceAdmin(w, r, http.StatusServiceUnavailable, "ModerationUnavailable", "Session lookup temporarily unavailable", auth.failure.reason(), auth.did, auth.err)
				return
			}
			if auth.failure != sealedSessionNoFailure {
				refuseInstanceAdmin(w, r, http.StatusUnauthorized, "AuthRequired", "Invalid or expired session", auth.failure.reason(), auth.did, nil)
				return
			}
			did, authMethod = auth.did, AuthMethodOAuth
			ctx = context.WithValue(ctx, OAuthSessionKey, auth.session)
			ctx = context.WithValue(ctx, UserAccessToken, auth.session.AccessToken)
		}

		if m.authority == nil || !m.authority.IsInstanceAdmin(did) {
			refuseInstanceAdmin(w, r, http.StatusForbidden, "Forbidden", "Instance admin authority required", "not_instance_admin", did, nil)
			return
		}
		ctx = context.WithValue(ctx, UserDIDKey, did)
		ctx = context.WithValue(ctx, AuthMethodKey, authMethod)
		ctx = context.WithValue(ctx, IsAggregatorAuthKey, false)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refuseInstanceAdmin logs and writes a refusal. A server-side failure (5xx)
// logs at Error; a refused caller logs at Warn. The client IP is not logged.
func refuseInstanceAdmin(w http.ResponseWriter, r *http.Request, status int, code, message, reason, did string, cause error) {
	attributes := []any{"method", r.Method, "path", r.URL.Path, "reason", reason}
	if did != "" {
		attributes = append(attributes, "did", did)
	}
	if cause != nil {
		attributes = append(attributes, "error", cause)
	}
	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	slog.Log(r.Context(), level, "instance admin request refused", attributes...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{Error: code, Message: message}); err != nil {
		slog.Warn("failed to write instance admin error response", "error", err)
	}
}
