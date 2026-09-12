package user

import (
	"context"

	"Coves/internal/atproto/pds"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// Profile creation needs create-only writes, so the factory must not erase the
// commit-aware client surface that production provides.
var _ func(context.Context, *oauth.ClientSessionData) (pds.CommitClient, error) = PDSClientFactory(nil)
