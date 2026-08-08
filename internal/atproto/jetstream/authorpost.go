package jetstream

import (
	"context"
	"net/http"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/posts"
)

// RED STUB (task 5, cycle 1). Declarations only — every function body here
// returns zero values, so the tests describing author-owned post ingestion
// compile and fail on their assertions rather than on missing symbols. The
// implementations, the HandleEvent dispatch for the three new collections, and
// the consumerWantedCollections entries are GREEN's.

// PostV2Collection is the author-repo post record of
// docs/PRD_AUTHOR_OWNED_POSTS.md §3.1 — the §3.0 successor to the deprecated
// social.coves.community.post.
//
// The NSID is new rather than reused, and that is a safety property, not
// bookkeeping: a consumer built against the published community.post derives
// community = repo DID for that collection, so feeding it author-repo records
// under the same name would have it index authors as communities. A new NSID
// makes a stale consumer ignore the records entirely, which is the correct
// failure mode.
const PostV2Collection = "social.coves.community.postv2"

// DeletedAccountLookup reports whether a DID names an account this AppView was
// asked to erase (migration 036, PRD rev 2.7).
//
// It exists because "no users row" stopped being an answer. Under author-owned
// posts an unknown author is a NORMAL state that must still index (§5.3), so
// the absence of a profile can no longer stand in for "this account is gone" —
// and without a marker, a redriven post event or a replayed acceptance quietly
// recreates the very rows the deletion swept.
//
// A lookup FAILURE must never be read as "not deleted". Failing open here means
// a database blip re-indexes an erased account's content, which is the one
// outcome a deletion is supposed to make impossible.
type DeletedAccountLookup interface {
	IsAccountDeleted(ctx context.Context, did string) (bool, error)
}

// WithAdmissions installs the per-(community, post) admission store. Without
// it, postv2 and acceptance/removal events have nowhere to record a decision.
func WithAdmissions(admissions posts.AdmissionRepository) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.admissions = admissions }
}

// WithDeletedAccounts installs the erased-account gate. Without it, no gate
// runs and every event indexes — the pre-036 behaviour.
func WithDeletedAccounts(lookup DeletedAccountLookup) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.deletedAccounts = lookup }
}

// WithPostRecordFetcher installs the §5.4 direct fetch used when an acceptance
// names a post this AppView has never indexed.
func WithPostRecordFetcher(fetcher PostRecordFetcher) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.postFetcher = fetcher }
}

// FetchedPost is one author-repo record read directly from its PDS.
//
// It carries the CID separately from the record because the CID is what the
// caller VERIFIES: an acceptance pins a strongRef, and a fetch that returned
// only the record body would leave the consumer indexing whatever the author's
// PDS felt like serving under that rkey.
type FetchedPost struct {
	URI    string
	CID    string
	Record map[string]interface{}
}

// PostRecordFetcher reads an author's post record straight from their PDS.
//
// This is what makes firehose-only ingestion actually CONVERGE (§5.4).
// Acceptance-before-post does not converge by dead-letter redrive alone:
// bounded retries cannot manufacture a post event that a relay-coverage gap
// will never deliver. Redrive stays the backstop for transient failures; this
// is the mechanism.
type PostRecordFetcher interface {
	// FetchPost resolves the repo DID in postURI to a PDS and reads the record.
	// The returned CID is the PDS's, unverified — checking it against the CID an
	// acceptance pinned is the caller's job, because only the caller knows what
	// was pinned.
	FetchPost(ctx context.Context, postURI string) (*FetchedPost, error)
}

// DirectPostFetcher is the production PostRecordFetcher: DID resolution, then
// com.atproto.repo.getRecord over an SSRF-guarded client with a size cap.
type DirectPostFetcher struct {
	resolver identity.Resolver

	// allowPrivateHosts disables the SSRF protection that blocks private and
	// loopback addresses. NEVER set outside tests. It exists for the same reason
	// blueskypost's allowPrivateHost does: this package's own tests point the
	// fetcher at an httptest server, which necessarily listens on loopback and
	// would otherwise be refused by the guard that must stay on in production.
	//
	// The guard is not decorative here. The DID document that names the PDS is
	// attacker-controlled — anyone can publish one — so an unguarded fetcher is
	// a request forger pointed at whatever is reachable from the AppView's
	// network, driven by any stranger who writes an acceptance record.
	allowPrivateHosts bool
}

// NewDirectPostFetcher wires the §5.4 fetch. SSRF protection is ON and there is
// no parameter to turn it off: a constructor that accepted a boolean is a
// constructor someone eventually passes true to from production wiring.
func NewDirectPostFetcher(resolver identity.Resolver) *DirectPostFetcher {
	return &DirectPostFetcher{resolver: resolver}
}

// httpClient builds the guarded client for one fetch. Declared here so the
// guard is derived from allowPrivateHosts at call time rather than baked into a
// client at construction, where a test seam could not reach it.
func (f *DirectPostFetcher) httpClient() *http.Client { return nil }

// FetchPost implements PostRecordFetcher.
func (f *DirectPostFetcher) FetchPost(ctx context.Context, postURI string) (*FetchedPost, error) {
	return nil, nil
}
