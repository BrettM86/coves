package main

import (
	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"

	"Coves/internal/api/middleware"
	"Coves/internal/config"
	"Coves/internal/core/moderation"
)

// buildInstanceAdminMiddleware gates the moderation endpoints on the
// operator-managed MODERATION_ADMINS allowlist.
func buildInstanceAdminMiddleware(cfg *config.Config, unsealer middleware.SessionUnsealer, store oauthlib.ClientAuthStore, validator middleware.ServiceAuthValidator) *middleware.InstanceAdminMiddleware {
	return middleware.NewInstanceAdminMiddleware(
		unsealer, store, validator, moderation.NewAllowlistAuthority(cfg.Moderation.Admins))
}
