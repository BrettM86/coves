package main

import (
	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"

	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"
)

// buildInstanceAdminMiddleware gates the moderation endpoints on authority,
// the operator-managed MODERATION_ADMINS allowlist the server builds once and
// shares with the read paths.
func buildInstanceAdminMiddleware(authority moderation.Authority, unsealer middleware.SessionUnsealer, store oauthlib.ClientAuthStore, validator middleware.ServiceAuthValidator) *middleware.InstanceAdminMiddleware {
	return middleware.NewInstanceAdminMiddleware(unsealer, store, validator, authority)
}
