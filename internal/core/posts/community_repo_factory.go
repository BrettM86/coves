package posts

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
)

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

// NewCommunityCredentialRefresher is the production CredentialRefresher: the
// engine's one forced renewal after a write comes back 401.
//
// KNOWN LIMITATION, recorded rather than hidden. EnsureFreshToken renews only a
// token that is within its expiry BUFFER, so a token the PDS has rejected for
// any other reason — revoked, invalidated by a password change, rotated out of
// band — is re-fetched unchanged and the retry fails identically. The subject
// then defers and the next pass tries again, which is correct but slower than
// it could be. Repairing it properly means a force-renew path on
// communities.Service, which is a wider change than this task, and the current
// behaviour is at worst the behaviour of having no refresher at all.
func NewCommunityCredentialRefresher(source CommunityCredentialSource) CredentialRefresher {
	return credentialRefresher{source: source}
}

type credentialRefresher struct {
	source CommunityCredentialSource
}

func (r credentialRefresher) RefreshCommunityCredentials(ctx context.Context, communityDID string) error {
	community, err := r.source.GetByDID(ctx, communityDID)
	if err != nil {
		return fmt.Errorf("re-reading the credentials of %s: %w", communityDID, err)
	}
	if community == nil {
		return fmt.Errorf("re-reading the credentials of %s: no such community is indexed", communityDID)
	}
	if _, err := r.source.EnsureFreshToken(ctx, community); err != nil {
		return fmt.Errorf("renewing the credentials of %s: %w", communityDID, err)
	}
	return nil
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
//
// # THE OPTIONS ARE THE SSRF DEV GATE, AND OMITTING THEM IS THE SAFE DIRECTION
//
// fresh.PDSURL is a per-community database column, so the address this factory
// dials is data rather than configuration — which is why the client it builds is
// address-guarded (see pds.newBearerHTTPClient). Passing nothing yields the
// guarded client, so a caller that forgets the gate gets the strict behaviour
// and finds out; the reverse default would be an unguarded client nobody
// notices. Production wiring passes pds.PrivateHostOptions(cfg.IsDevEnv)...,
// and tests driving the CI stack's loopback PDS pass
// pds.PrivateHostOptions(true)... because loopback is exactly what the guard
// refuses.
func NewCommunityRepoFactory(source CommunityCredentialSource, opts ...pds.ClientOption) CommunityRepoFactory {
	return func(ctx context.Context, communityDID string) (CommunityRepo, error) {
		community, err := source.GetByDID(ctx, communityDID)
		if err != nil {
			if communities.IsNotFound(err) {
				// Deliberately NOT ErrCommunityNotHosted. A community nobody has
				// indexed may simply not have arrived yet — cross-repo delivery
				// order is not guaranteed — and spelling an ordering artefact as
				// a permanent skip would abandon a subject that resolves on its
				// own once the profile lands.
				return nil, fmt.Errorf("opening the repo of %s: no community with that DID is indexed: %w",
					communityDID, err)
			}
			return nil, fmt.Errorf("opening the repo of %s: %w", communityDID, err)
		}
		if community == nil {
			return nil, fmt.Errorf("opening the repo of %s: the community lookup returned nothing", communityDID)
		}

		// THE HOSTING TEST, and the only one this factory is allowed to make.
		// A stored refresh token exists exactly when this AppView provisioned
		// the account itself; nothing a remote repo can publish creates one.
		// See the note above for why hosted_by_did is not consulted.
		if community.PDSRefreshToken == "" {
			return nil, fmt.Errorf("opening the repo of %s: %w", communityDID, ErrCommunityNotHosted)
		}

		// The token is renewed BEFORE the client is built rather than after a
		// write fails, because a client is bound to the access token it was
		// constructed with: refreshing afterwards would leave this caller
		// holding the stale one.
		fresh, err := source.EnsureFreshToken(ctx, community)
		if err != nil {
			return nil, fmt.Errorf("refreshing the credentials of %s: %w", communityDID, err)
		}
		if fresh == nil || fresh.PDSAccessToken == "" {
			return nil, fmt.Errorf("refreshing the credentials of %s: no access token came back", communityDID)
		}

		client, err := pds.NewFromAccessToken(fresh.PDSURL, fresh.DID, fresh.PDSAccessToken, opts...)
		if err != nil {
			return nil, fmt.Errorf("building a PDS client for %s: %w", communityDID, err)
		}

		// The community-repo writers need the commit rev and applyWrites, and
		// neither is on the base Client. Asserted rather than assumed: a
		// transport that lost either would otherwise fail at the first
		// moderation commit, which is after a verdict has already been reached.
		repo, ok := client.(CommunityRepo)
		if !ok {
			return nil, fmt.Errorf("building a PDS client for %s: the client does not implement the community-repo "+
				"write surface (commit rev + applyWrites)", communityDID)
		}
		return repo, nil
	}
}
