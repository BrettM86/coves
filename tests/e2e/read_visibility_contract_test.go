//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read-path visibility contract (task 7, PRD §6.2, §9 re-scoped): the
// binding security arc for author-owned posts. It is the compensating control
// for task 6's write-path flip — that flip means any author can index a postv2
// naming any community, so the ONLY thing standing between an unadmitted post and
// every reader is the admission predicate this task builds. An alternate-endpoint
// hole is a real leak, so the invisibility is asserted on EVERY public display
// endpoint, not just the feed.
//
// # WHAT THIS TIER CAN AND CANNOT OBSERVE
//
// The viewer here is the ANONYMOUS public. §3.4b's standing limitation — nothing
// but the browser OAuth callback mints a sealed session RequireAuth accepts —
// means this tier cannot authenticate a viewer AT ALL, so it proves the one case
// that matters most: the unauthenticated internet must reach accepted content
// only. The author's own privileged view of their pending/removed posts (PRD
// §6.2) needs an authenticated viewer DID and is proven at T1, where the read
// requests carry a ViewerDID directly (internal/db/postgres/post_visibility_test.go,
// TestActorPostsVisibility_AuthorVsNonAuthor).
//
// getTimeline is also absent below for a tier reason, not an oversight: it is the
// one feed behind RequireAuth (routes: authRequired), so an anonymous timeline
// read is a 401 rather than a filtered feed. Its accepted-only predicate is
// proven at T1 (TestTimelineVisibility_AcceptedOnly).
//
// # HOW ADMISSION STATE IS DRIVEN
//
// The same way author_post_contract_test.go drives it, and for the same reason:
// no community in this tier holds PDS credentials, so the production acceptance
// engine cannot run out here. The test writes the acceptance / removal records
// into the community's own repo directly — which is exactly the events a
// credentialed engine would emit — and confirms the resulting admission state
// through getStatus BEFORE probing the display endpoints, so a display leak can
// never be mistaken for the state not having converged yet.

// feedItemView is the slice of a feed response the visibility probes read.
type feedItemView struct {
	Post struct {
		URI string `json:"uri"`
	} `json:"post"`
}

// removablePostView reads post.get's union positionally, including the
// #removedPost member (a removed post is served as a tombstone, not omitted).
type removablePostView struct {
	URI      string `json:"uri"`
	NotFound bool   `json:"notFound"`
	Removed  bool   `json:"removed"`
	Code     string `json:"code"`
}

// communityFeedURIs reads a community feed as the public and returns the URIs it
// served.
func communityFeedURIs(t *testing.T, p *pipeline, communityDID string) []string {
	t.Helper()
	var out struct {
		Feed []feedItemView `json:"feed"`
	}
	require.NoError(t, p.AppView.Query(context.Background(), "social.coves.communityFeed.getCommunity",
		url.Values{"community": {communityDID}, "sort": {"new"}, "limit": {"50"}}, &out))
	uris := make([]string, 0, len(out.Feed))
	for _, item := range out.Feed {
		uris = append(uris, item.Post.URI)
	}
	return uris
}

// discoverFeedURIsPublic reads the discover feed as the public and returns the
// URIs it served.
func discoverFeedURIsPublic(t *testing.T, p *pipeline) []string {
	t.Helper()
	var out struct {
		Feed []feedItemView `json:"feed"`
	}
	require.NoError(t, p.AppView.Query(context.Background(), "social.coves.feed.getDiscover",
		url.Values{"sort": {"new"}, "limit": {"50"}}, &out))
	uris := make([]string, 0, len(out.Feed))
	for _, item := range out.Feed {
		uris = append(uris, item.Post.URI)
	}
	return uris
}

// getRemovablePost reads one post.get union member, exposing the #removedPost
// discriminator and code the plain postView helper does not.
func getRemovablePost(t *testing.T, p *pipeline, uri string) removablePostView {
	t.Helper()
	var out struct {
		Posts []removablePostView `json:"posts"`
	}
	require.NoError(t, p.AppView.Query(context.Background(), "social.coves.community.post.get",
		url.Values{"uris": {uri}}, &out))
	require.Lenf(t, out.Posts, 1, "post.get must answer positionally, one member per requested URI")
	return out.Posts[0]
}

// TestReadVisibilityContract is the binding arc: an accepted post is reachable
// through every public display endpoint, a pending one through NONE of them, and
// a removed one renders as #removedPost carrying its code.
//
// It carries NO ingestion-contract marker: markers are for pipeline proofs
// (§3.4a), and postv2's is already owned by TestAuthorPostIngestion. This asserts
// the READ path — what a public client reaches — which is a different contract.
func TestReadVisibilityContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "rvc")
	community := indexedCommunity(t, p, "rvc", author.DID)

	// ---- an accepted post: the positive control on every surface ------------
	acceptedRkey := testkit.TID()
	acceptedURI := authorPostURI(author.DID, acceptedRkey)
	acceptedTitle := "accepted " + testkit.UniqueID(t)
	acceptedRecord := author.PutRecord(t, postV2Collection, acceptedRkey,
		postV2Record(community.DID, acceptedTitle, "content the community will attest to"))
	awaitStatus(t, p, acceptedURI, community.DID, "pending", "the acceptable post to be indexed")
	community.PutRecord(t, acceptanceCollection, subjectRkey(acceptedURI),
		acceptanceRecord(acceptedURI, acceptedRecord.CID))
	awaitStatus(t, p, acceptedURI, community.DID, "accepted", "the community's acceptance to admit the post")

	// ---- a pending post: never admitted, and it bounds the accepted one -----
	// Written and confirmed pending. Because both posts are in the SAME author
	// repo and the pending one is committed after the acceptable one's postv2,
	// its indexing has necessarily been through the consumer by the time the
	// acceptance above is observed — so its ABSENCE below is meaningful, not just
	// "hasn't arrived yet".
	pendingRkey := testkit.TID()
	pendingURI := authorPostURI(author.DID, pendingRkey)
	pendingTitle := "pending " + testkit.UniqueID(t)
	author.PutRecord(t, postV2Collection, pendingRkey,
		postV2Record(community.DID, pendingTitle, "content no community has agreed to carry"))
	awaitStatus(t, p, pendingURI, community.DID, "pending", "the pending post to be indexed and awaiting a decision")

	t.Run("the accepted post is reachable through every public endpoint", func(t *testing.T) {
		assert.Containsf(t, communityFeedURIs(t, p, community.DID), acceptedURI,
			"an accepted post must appear in its community feed")
		assert.Contains(t, discoverFeedURIsPublic(t, p), acceptedURI,
			"an accepted post must appear in discover")

		got := getRemovablePost(t, p, acceptedURI)
		assert.False(t, got.NotFound, "an accepted post must be served by post.get")
		assert.False(t, got.Removed)

		thread, err := p.Thread(context.Background(), acceptedURI, nil)
		require.NoError(t, err, "getComments must serve the header of an accepted post")
		assert.Equal(t, acceptedURI, thread.Post.URI)
	})

	t.Run("the pending post is invisible on every public endpoint", func(t *testing.T) {
		// The security core. Each of these is an alternate path to the same
		// content, and a hole in any one of them is a real leak — a client that
		// cannot see a pending post in the feed can still permalink it, read it
		// through the author feed, or open its comment thread.
		assert.NotContainsf(t, communityFeedURIs(t, p, community.DID), pendingURI,
			"a PENDING post appeared in the community feed. It has no acceptance; rendering it publishes speech the "+
				"community never agreed to carry (PRD §2)")
		assert.NotContainsf(t, discoverFeedURIsPublic(t, p), pendingURI,
			"a PENDING post appeared in discover")

		got := getRemovablePost(t, p, pendingURI)
		assert.Truef(t, got.NotFound,
			"post.get served a PENDING post to an anonymous caller. A pending post must be a notFoundPost to the public, "+
				"or every feed gate is worthless against a direct permalink")

		// getComments hydrates its post header through the post read path, and on
		// this branch that path is admission-blind and serves soft-deleted posts
		// too (the 2026-07-29 defect). The header of a pending post must not reach
		// the public — whether GREEN answers with a not-found error or a hidden
		// header, the pending title must never appear.
		thread, err := p.Thread(context.Background(), pendingURI, nil)
		if err == nil {
			assert.NotEqualf(t, pendingTitle, thread.Post.Record["title"],
				"getComments exposed a PENDING post's header (title, content, author) to the public through its thread "+
					"endpoint — the alternate-endpoint leak PRD §6.2 names explicitly")
			assert.Truef(t, thread.Post.URI == "" || thread.Post.NotFound,
				"getComments served a real post header for a pending post: %+v", thread.Post)
		}
	})

	// ---- removal: the accepted post is taken down and renders as a tombstone -
	t.Run("a removed post renders as #removedPost with its code, everywhere else gone", func(t *testing.T) {
		subject := subjectRkey(acceptedURI)
		applyWrites(t, community, []map[string]any{
			{"$type": "com.atproto.repo.applyWrites#delete", "collection": acceptanceCollection, "rkey": subject},
			{
				"$type":      "com.atproto.repo.applyWrites#create",
				"collection": removalCollection,
				"rkey":       subject,
				"value":      removalRecord(acceptedURI, acceptedRecord.CID, "rule-violation"),
			},
		})
		awaitStatus(t, p, acceptedURI, community.DID, "removed", "the atomic removal commit to take the post down")

		// post.get answers the removed member — NOT notFound — carrying the code,
		// so a client renders "removed: rule-violation" rather than a blank
		// permalink.
		p.Await(t, "post.get to serve the removed post as a #removedPost tombstone", func() (bool, error) {
			got := getRemovablePost(t, p, acceptedURI)
			return got.Removed, nil
		})
		got := getRemovablePost(t, p, acceptedURI)
		assert.Falsef(t, got.NotFound,
			"a removed post must be a #removedPost, not a notFoundPost — the author is owed the reason, not silence")
		assert.Equalf(t, "rule-violation", got.Code,
			"the removedPost member must carry the removal code; a tombstone without one is an unexplained disappearance")

		// And it is gone from every browsing surface.
		assert.NotContainsf(t, communityFeedURIs(t, p, community.DID), acceptedURI,
			"a removed post must drop out of the community feed")
		assert.NotContains(t, discoverFeedURIsPublic(t, p), acceptedURI,
			"a removed post must drop out of discover")
	})
}
