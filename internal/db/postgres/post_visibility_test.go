//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read-path visibility predicate (task 7, PRD §6.2). Under author-owned
// posts a post record is indexed the moment its author writes it, but it is only
// VISIBLE in a community once that community has admitted it (PRD §2). Admission
// state is per-(community, post) in community_post_admissions, and the security
// property this whole task exists to establish is:
//
//	a non-author must not reach a pending / rejected / removed post through ANY
//	read path — the feeds, post.get, actor.getPosts, the comment header, or the
//	counts.
//
// On this branch the read queries reference community_post_admissions NOWHERE
// (loop_state task-6 note: "post_repo.go + all feed queries currently reference
// it NOWHERE outside getStatus"), so every one of these suites fails until the
// centralized predicate lands. They are the compensating control for task 6's
// deploy gate, not a feature nicety: task 6 shipped the write-path flip, which
// means any authenticated user can already index a postv2 naming any community,
// and only this predicate stops it rendering as that community's content.

const (
	// visibilitySort keeps every feed read chronological so the seeded set is
	// the whole answer — a hot rank computed against NOW() would make an
	// exact-set assertion flaky for reasons that have nothing to do with
	// admission.
	visibilitySort = "new"
	// A public (unauthenticated) read: no viewer DID. This is the security case
	// — the anonymous internet must see accepted content only.
	publicViewer = ""
)

// feedURIs renders a community feed as the URIs it returned.
func feedURIs(feed []*communityFeeds.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

func discoverFeedURIs(feed []*discover.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

func timelineFeedURIs(feed []*timeline.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

// TestCommunityFeedVisibility_AcceptedOnly is the community feed's predicate:
// getCommunity(X) shows a post iff community X has ADMITTED it. The four seeded
// posts span every admission state so the accepted one is the only survivor —
// and the pending, rejected and removed ones each prove a distinct leak the
// predicate closes.
func TestCommunityFeedVisibility_AcceptedOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "cf")
	author := "did:plc:viscfauthor"
	createTestUser(t, db, "viscfauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "cfacc", "accepted", base.Add(4*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "cfpen", "pending", base.Add(3*time.Hour))
	rejected := seedVisibilityPost(t, db, community, author, "cfrej", "rejected", base.Add(2*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "cfrem", "removed", base.Add(1*time.Hour))

	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2cfacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, rejected, posts.AdmissionStatusRejected, "", "spam")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	repo := NewCommunityFeedRepository(db, "test-secret")
	feed, _, err := repo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
		Community: community, ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50,
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{accepted}, feedURIs(feed),
		"a community feed must serve accepted posts only. A pending post is speech the community has not agreed to carry, "+
			"a rejected one it refused, and a removed one it took down — every non-accepted URI here is a leak the "+
			"admission predicate exists to close")
}

// TestDiscoverVisibility_ForkJoinKey is the join-key security assertion, pinned
// hardest per the task brief. Discover spans every community, so it cannot lean
// on a `p.community_did = $1` filter the way getCommunity does — it must join the
// admission row on BOTH halves of the subject key
// (a.community_did = p.community_did AND a.post_uri = p.uri) and show a post iff
// its OWN community accepted it.
//
// The fork case is what makes the community half of that key load-bearing. One
// post carries TWO admission rows: pending in its own community B, and accepted
// in a DIFFERENT community A that forked it (PRD §2, §6.1). A predicate that
// joined on post_uri + status alone — dropping the community_did equality — would
// see the accepted (A, post) row and leak the post into discover under community
// B, which has not accepted it. That is the single most dangerous read-path bug
// this task can ship: a moderator's decision in one community silently
// publishing a post in another.
func TestDiscoverVisibility_ForkJoinKey(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	forker := visibilityCommunity(t, db, "dfA")   // community A: forks/accepts the post
	homeComm := visibilityCommunity(t, db, "dfB") // community B: the post's own community, still pending
	author := "did:plc:visdfauthor"
	createTestUser(t, db, "visdfauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// The forked post: its OWN community is B (pending), but community A holds an
	// accepted admission for the very same URI.
	forked := seedVisibilityPost(t, db, homeComm, author, "dffork", "forked-post", base.Add(3*time.Hour))
	seedVisibilityAdmission(t, db, homeComm, forked, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, forker, forked, posts.AdmissionStatusAccepted, "bafypostv2dffork", "")

	// A positive control: a post whose own community A accepted it must appear.
	homegrown := seedVisibilityPost(t, db, forker, author, "dfhome", "homegrown", base.Add(2*time.Hour))
	seedVisibilityAdmission(t, db, forker, homegrown, posts.AdmissionStatusAccepted, "bafypostv2dfhome", "")

	repo := NewDiscoverRepository(db, "test-secret")
	feed, _, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
		ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50,
	})
	require.NoError(t, err)

	got := discoverFeedURIs(feed)
	assert.Contains(t, got, homegrown, "a post its own community accepted must appear in discover")
	assert.NotContainsf(t, got, forked,
		"the forked post leaked into discover. It is PENDING in its own community (%s) and only ACCEPTED in a "+
			"community that forked it (%s); the discover join must key on (a.community_did = p.community_did AND "+
			"a.post_uri = p.uri), so that a post is visible iff ITS community accepted it. A join on post_uri + status "+
			"alone lets one community's acceptance publish another community's pending post.",
		homeComm, forker)
}

// TestTimelineVisibility_AcceptedOnly is the subscribed-feed predicate. The
// timeline joins community_subscriptions, so a leak here reaches a subscriber's
// home feed directly. The seeded post is pending in the subscribed community and
// must not appear, while the accepted one must.
func TestTimelineVisibility_AcceptedOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "tl")
	author := "did:plc:vistlauthor"
	createTestUser(t, db, "vistlauthor.test", author)
	subscriber := "did:plc:vistlsubscriber"
	createTestUser(t, db, "vistlsubscriber.test", subscriber)

	_, err := db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
		VALUES ($1, $2, NOW())
	`, subscriber, community)
	require.NoError(t, err)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "tlacc", "accepted", base.Add(2*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "tlpen", "pending", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2tlacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")

	repo := NewTimelineRepository(db, "test-secret")
	feed, _, err := repo.GetTimeline(ctx, timeline.GetTimelineRequest{
		UserDID: subscriber, Sort: visibilitySort, Limit: 50,
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{accepted}, timelineFeedURIs(feed),
		"a subscriber's timeline must carry accepted posts only; a pending post reaching the home feed is the "+
			"loudest possible leak of unadmitted content")
}

// TestPostGetVisibility_PublicSeesAcceptedOnly is post.get's predicate for the
// anonymous caller. GetViewsByURIs backs social.coves.community.post.get, and a
// post absent from the returned map becomes a notFoundPost on the wire — which is
// exactly the right answer for a public caller asking about a post no community
// has admitted.
//
// This suite also pins the TWO shapes the predicate must preserve while it adds
// the admission gate: a post by an author with no `users` row (a federated
// author, PRD §5.3) stays visible once accepted, and the visible row still
// hydrates its media out of the author's repository.
func TestPostGetVisibility_PublicSeesAcceptedOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "pg")
	author := "did:plc:vispgauthor"
	createTestUser(t, db, "vispgauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "pgacc", "accepted", base.Add(4*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "pgpen", "pending", base.Add(3*time.Hour))
	rejected := seedVisibilityPost(t, db, community, author, "pgrej", "rejected", base.Add(2*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "pgrem", "removed", base.Add(1*time.Hour))

	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2pgacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, rejected, posts.AdmissionStatusRejected, "", "spam")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	repo := NewPostRepository(db)
	views, err := repo.GetViewsByURIs(ctx, []string{accepted, pending, rejected, removed})
	require.NoError(t, err)

	require.Containsf(t, views, accepted, "an accepted post must be served by post.get")
	assert.NotContainsf(t, views, pending,
		"post.get served a PENDING post to an anonymous caller. Absence from this map is what makes the endpoint "+
			"answer notFoundPost; a pending post that resolves here is reachable by permalink regardless of any feed gate")
	assert.NotContains(t, views, rejected, "post.get must not serve a rejected post to the public")
	assert.NotContains(t, views, removed, "post.get must not serve a removed post to the public")

	t.Run("an accepted post by an unindexed author is still visible", func(t *testing.T) {
		// The write-path flip (PRD §5.3) drops the posts→users FK so a federated
		// author with no `users` row can be indexed. The read join must be a LEFT
		// join, or the whole promise of open federated posting breaks silently at
		// the read path — the post indexes fine and is invisible forever (the
		// loop_state task-2 → task-7 obligation).
		unknownAuthor := "did:plc:visunknownfederated"
		unknownPost := seedVisibilityPost(t, db, community, unknownAuthor, "pgunk", "federated", base.Add(5*time.Hour))
		seedVisibilityAdmission(t, db, community, unknownPost, posts.AdmissionStatusAccepted, "bafypostv2pgunk", "")

		got, err := repo.GetViewsByURIs(ctx, []string{unknownPost})
		require.NoError(t, err)
		require.Containsf(t, got, unknownPost,
			"an accepted post whose author has no users row was dropped by post.get. The author join must be a LEFT "+
				"join (author_did has no FK since migration 034); an INNER join makes every federated author's post "+
				"invisible the moment it is accepted")
		view := got[unknownPost]
		require.NotNil(t, view.Author)
		assert.Equal(t, unknownAuthor, view.Author.DID,
			"the unhydrated author's DID must still be carried — it is the repo the postv2 record lives in")
	})
}

// TestPostGetVisibility_AuthorMediaResolvesOnVisibleRow pins that the admission
// predicate does not cost the visible row its media owner. A postv2 post's blobs
// live in the AUTHOR's repository (blobOwnerOf, PRD §3.1), so the row must carry
// the author's pds_url out of the SELECT. A predicate that narrowed the SELECT
// list, or dropped the author join to a subquery, would blank the pds_url and
// address every accepted post's image to an empty host.
//
// Not parallel: it sets the process-wide image-URL config scanPostView reads,
// the same constraint TestGetCommunityFeed_BlobURLTransformation documents.
func TestPostGetVisibility_AuthorMediaResolvesOnVisibleRow(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "pm")
	author := "did:plc:vispmauthor"
	createTestUser(t, db, "vispmauthor.test", author)

	const thumbCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	accepted := seedVisibilityPostWithEmbed(t, db, community, author, "pmacc", "with media", thumbCID,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2pmacc", "")

	repo := NewPostRepository(db)
	views, err := repo.GetViewsByURIs(ctx, []string{accepted})
	require.NoError(t, err)
	require.Contains(t, views, accepted)

	view := views[accepted]
	require.NotNil(t, view.Author)
	assert.NotEmptyf(t, view.Author.PDSURL,
		"the visible accepted postv2 row lost its author pds_url. A postv2 post's blobs live in the author's repo, "+
			"so the visibility predicate must keep selecting author pds_url; blanking it addresses the post's media to "+
			"an empty host — a broken image that looks fine server-side (blobOwnerOf, PRD §3.1)")
}

// TestActorPostsVisibility_AuthorVsNonAuthor is actor.getPosts, the one display
// surface that already threads a viewer DID (GetAuthorPostsRequest.ViewerDID).
// That makes it the site where BOTH halves of the predicate are testable today:
// a non-author sees the author's accepted posts only, while the author sees
// their own pending and removed posts too (PRD §6.2 — "authors see their own
// posts with per-community status").
func TestActorPostsVisibility_AuthorVsNonAuthor(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "ap")
	author := "did:plc:visapauthor"
	createTestUser(t, db, "visapauthor.test", author)
	stranger := "did:plc:visapstranger"
	createTestUser(t, db, "visapstranger.test", stranger)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "apacc", "accepted", base.Add(3*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "appen", "pending", base.Add(2*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "aprem", "removed", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2apacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	repo := NewPostRepository(db)
	read := func(t *testing.T, viewerDID string) []string {
		t.Helper()
		views, _, err := repo.GetByAuthor(ctx, posts.GetAuthorPostsRequest{
			ActorDID: author, ViewerDID: viewerDID, Limit: 50,
		})
		require.NoError(t, err)
		uris := make([]string, 0, len(views))
		for _, v := range views {
			uris = append(uris, v.URI)
		}
		return uris
	}

	t.Run("a stranger sees the author's accepted posts only", func(t *testing.T) {
		assert.ElementsMatch(t, []string{accepted}, read(t, stranger),
			"actor.getPosts served a non-author the author's pending and removed posts. This is the alternate-endpoint "+
				"leak the task brief names: the feed gate is worthless if the author-feed shows the same content ungated")
	})

	t.Run("a stranger is the same as the anonymous public", func(t *testing.T) {
		assert.ElementsMatch(t, []string{accepted}, read(t, publicViewer))
	})

	t.Run("the author sees their own non-accepted posts", func(t *testing.T) {
		assert.ElementsMatch(t, []string{accepted, pending, removed}, read(t, author),
			"an author must be able to see their own posts in every admission state — it is how a client renders "+
				"'pending review' and 'removed' on the author's own profile (PRD §6.2)")
	})
}

// TestProfileStatsVisibility_PostCountExcludesNonAccepted is the counts predicate
// (PRD §6.2: "counts must not include non-accepted rows"). GetProfileStats'
// post_count is a live COUNT over `posts`, and a count that includes pending or
// removed rows leaks their existence — a profile advertising 4 posts when only 1
// is visible is a side channel onto content the reader cannot reach.
func TestProfileStatsVisibility_PostCountExcludesNonAccepted(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "ps")
	author := "did:plc:vispsauthor"
	createTestUser(t, db, "vispsauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "psacc", "accepted", base.Add(3*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "pspen", "pending", base.Add(2*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "psrem", "removed", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2psacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	repo := NewUserRepository(db)
	stats, err := repo.GetProfileStats(ctx, author)
	require.NoError(t, err)

	assert.Equalf(t, 1, stats.PostCount,
		"a profile's post_count must count accepted posts only; counting the pending and removed rows too (%d seeded, "+
			"1 accepted) advertises the existence of content no reader can reach", 3)
}
