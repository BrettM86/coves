//go:build !dev

package oauth

import (
	"context"

	"github.com/bluesky-social/indigo/atproto/identity"
)

// DevHandleResolver is a stub for production builds.
// The actual implementation is in dev_resolver.go (only compiled with -tags dev).
type DevHandleResolver struct{}

// NewDevHandleResolver returns nil in production builds.
// Dev mode features are only available when built with -tags dev.
func NewDevHandleResolver(pdsURL string, allowPrivateIPs bool) *DevHandleResolver {
	return nil
}

// ResolveHandle is a stub that should never be called in production.
// The nil check in handlers.go prevents this from being reached.
func (r *DevHandleResolver) ResolveHandle(ctx context.Context, handle string) (string, error) {
	panic("dev mode: ResolveHandle called in production build - this should never happen")
}

// DevAuthResolver is a stub for production builds.
// The actual implementation is in dev_auth_resolver.go (only compiled with -tags dev).
type DevAuthResolver struct{}

// NewDevAuthResolver returns nil in production builds.
// Dev mode features are only available when built with -tags dev.
func NewDevAuthResolver(pdsURL string, allowPrivateIPs bool) *DevAuthResolver {
	return nil
}

// StartDevAuthFlow is a stub that should never be called in production.
// The nil check in handlers.go prevents this from being reached.
func (r *DevAuthResolver) StartDevAuthFlow(ctx context.Context, client *OAuthClient, identifier string, dir identity.Directory) (string, error) {
	panic("dev mode: StartDevAuthFlow called in production build - this should never happen")
}
