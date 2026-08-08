package posts

import (
	"context"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/atproto/pds"
)

// NewAuthorRepoFactory builds the production AuthorRepoFactory: the seam the
// AUTHOR's own credentials arrive through, now that a post is signed by its
// author rather than by the community it was submitted to (§4.2 step 3).
//
// # TWO KINDS OF AUTHOR, ONE SEAM
//
// A person posting from a browser or the mobile app arrives with an OAuth
// session the API boundary already holds, and it is passed straight through:
// the session IS the credential, and resolving it a second time from the store
// would only introduce a way for the two to disagree.
//
// An aggregator has no session on the request — it authenticates by API key —
// so its credentials come from the tokens it granted when it registered
// (migration 025), resumed under storedSessionID. Before the write path
// flipped, an aggregator needed no repository at all: its post went into the
// community's repo under the community's token, with the aggregator named in a
// field. Now it writes into its own repo like any other author.
//
// # WHY A MISSING STORED SESSION IS ITS OWN ERROR
//
// A human's dead session surfaces as pds.ErrSessionExpired, which the boundary
// answers with a 401 the client can act on: sign in again. An aggregator cannot
// sign in again — nobody is at the keyboard — so the same condition is
// ErrNoAuthorCredentials instead, which is an operator problem: the service is
// running, correctly configured, and completely unable to post. Collapsing them
// would have a revoked aggregator grant diagnosed as a PDS outage, or a
// signed-out user told to file a ticket.
func NewAuthorRepoFactory(oauthClient *oauth.ClientApp, storedSessionID string) AuthorRepoFactory {
	return func(ctx context.Context, authorDID string, session *oauth.ClientSessionData) (AuthorRepo, error) {
		if oauthClient == nil {
			return nil, fmt.Errorf("opening the repository of %s: no OAuth client is wired: %w",
				authorDID, ErrNoAuthorCredentials)
		}

		did, err := syntax.ParseDID(authorDID)
		if err != nil {
			return nil, NewValidationError("authorDid", fmt.Sprintf("not a DID: %s", err))
		}

		if session == nil {
			// The non-interactive author. Resumed here rather than inside
			// NewFromOAuthSession so that the stored session's own HostURL —
			// which PDS the aggregator's repo is actually on — comes from the
			// store rather than being guessed at, and so that "there is nothing
			// to resume" is answered in the vocabulary the boundary needs.
			resumed, resumeErr := oauthClient.ResumeSession(ctx, did, storedSessionID)
			if resumeErr != nil {
				return nil, fmt.Errorf("resuming the stored session of %s: %w: %w",
					authorDID, ErrNoAuthorCredentials, resumeErr)
			}
			if resumed == nil || resumed.Data == nil {
				return nil, fmt.Errorf("resuming the stored session of %s: the store returned nothing: %w",
					authorDID, ErrNoAuthorCredentials)
			}
			session = resumed.Data
		} else if session.AccountDID != did {
			// The session is what the record will actually be signed by, so a
			// request naming a different author is refused here even though
			// CreatePost has already compared the two — this is the last point
			// at which an author-supplied DID could still reach a repo.
			return nil, fmt.Errorf("opening the repository of %s under the session of %s: %w",
				authorDID, session.AccountDID, ErrNotAuthorized)
		}

		client, err := pds.NewFromOAuthSession(ctx, oauthClient, session)
		if err != nil {
			return nil, fmt.Errorf("opening the repository of %s: %w", authorDID, err)
		}

		// The write path needs the guarded put and the commit rev, and neither
		// is on the base Client. Asserted rather than assumed: a transport that
		// lost either would otherwise fail at the first post, after admission
		// has already spent the author's quota slot.
		repo, ok := client.(AuthorRepo)
		if !ok {
			return nil, fmt.Errorf("opening the repository of %s: the PDS client does not implement the "+
				"author-repo write surface (guarded put + commit rev)", authorDID)
		}
		return repo, nil
	}
}
