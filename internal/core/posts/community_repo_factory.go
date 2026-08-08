package posts

import (
	"context"
	"errors"

	"Coves/internal/core/communities"
)

// RED STUB (task 5, cycle 2). Signatures only; the body is GREEN's.

// ErrCommunityNotHosted reports that this AppView does not hold the community's
// PDS credentials, so it cannot write records into that community's repo.
//
// It is a PERMANENT SKIP, not a deferral, and the distinction decides whether a
// backlog drains or grows forever. A deferral means "look again later", which is
// right for an expired token — the refresh will fix it. Not being the host is
// not a transient condition: no retry, no redrive and no amount of waiting turns
// another instance's community into one this AppView can sign for. A driver that
// treated the two the same would re-offer every remote community's posts on
// every pass, forever, and the genuine deferrals would be invisible underneath.
var ErrCommunityNotHosted = errors.New("community is not hosted by this AppView")

// CommunityCredentialSource supplies a community's repo credentials, refreshing
// them if they are close to expiry. Satisfied by communities.Service.
type CommunityCredentialSource interface {
	GetByDID(ctx context.Context, did string) (*communities.Community, error)
	EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error)
}

// NewCommunityRepoFactory builds the production CommunityRepoFactory: a
// credential lookup, a token refresh, and a PDS client bound to the community's
// own repo.
//
// # HOSTING IS CREDENTIAL PRESENCE, NEVER hosted_by_did
//
// The obvious test — does communities.hosted_by_did name this instance — is
// wrong, and dangerously so. That column is populated from the community's own
// PROFILE RECORD when a community is indexed from the firehose, which means it
// is a claim made by whoever controls that repo. Anyone can write a community
// profile naming this AppView as its host. Trusting it would have the factory
// hand back a repo client for a community this instance has no keys for; every
// write would then fail at the PDS, but only after the engine had already
// decided, and a hostile community could aim that traffic wherever its PDS URL
// pointed.
//
// Credentials cannot be claimed. Either this AppView holds the community's
// refresh token — which happens exactly once, when it provisioned the account
// through social.coves.community.create — or it does not. That is the honest
// question, and it is the only one this factory asks.
func NewCommunityRepoFactory(communities CommunityCredentialSource) CommunityRepoFactory {
	return func(ctx context.Context, communityDID string) (CommunityRepo, error) {
		return nil, nil
	}
}
