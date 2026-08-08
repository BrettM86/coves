//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/comments"
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

// TestVisibility_CollectionAwareFailClosed is the corrected core of task 7, and
// the single most important assertion in this suite. Cycle 1's predicate went
// FAIL-OPEN: a post with no admission row was visible to everyone. That is
// correct for a LEGACY community.post — it was signed into the community's own
// repo under the old model, is accepted by construction, and must stay visible
// until task 8 drains it — but it is a security HOLE for a postv2, because the
// task-5 consumer ALWAYS seeds a pending admission the moment it indexes a
// postv2. A postv2 with NO admission row therefore does not mean "pre-admission
// content to grandfather in"; it means the seed has not happened (or failed),
// and the right answer is to FAIL CLOSED.
//
// So the no-admission rule is COLLECTION-AWARE, discriminated by the collection
// segment of the post URI (CollectionOfPostURI): legacy → visible, postv2 →
// hidden from non-authors, visible to its author. Both posts below carry no
// admission row at all; only their collection differs.
func TestVisibility_CollectionAwareFailClosed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "fc2")
	author := "did:plc:visfc2author"
	createTestUser(t, db, "visfc2author.test", author)
	stranger := "did:plc:visfc2stranger"
	createTestUser(t, db, "visfc2stranger.test", stranger)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A legacy community-repo post, no admission row: accepted-by-construction,
	// must stay visible to the public until task 8's drain.
	legacy := seedFilterablePost(t, db, community, author, "fc2leg", base.Add(2*time.Hour))
	// A postv2, no admission row: the consumer would have seeded pending, so a
	// missing row is a failed seed — fail closed for non-authors.
	postv2 := seedVisibilityPost(t, db, community, author, "fc2pv2", "postv2 with no admission", base.Add(1*time.Hour))

	feedRepo := NewCommunityFeedRepository(db, "test-secret")
	postRepo := NewPostRepository(db)
	discoverRepo := NewDiscoverRepository(db, "test-secret")

	communityFeed := func(t *testing.T, viewer string) []string {
		t.Helper()
		feed, _, err := feedRepo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
			Community: community, ViewerDID: viewer, Sort: visibilitySort, Limit: 50,
		})
		require.NoError(t, err)
		return feedURIs(feed)
	}

	t.Run("the legacy post with no admission row stays visible to the public", func(t *testing.T) {
		assert.Containsf(t, communityFeed(t, publicViewer), legacy,
			"a legacy community.post with no admission row must stay visible — it was accepted by construction under the "+
				"old model and task 7 must not retro-hide content that predates admissions (visible until task 8's drain)")

		views, err := postRepo.GetViewsByURIs(ctx, []string{legacy})
		require.NoError(t, err)
		assert.Contains(t, views, legacy, "post.get must still serve a legacy post with no admission row to the public")
	})

	t.Run("the postv2 with no admission row is HIDDEN from non-authors everywhere", func(t *testing.T) {
		// The corrected security core. A missing admission row on a postv2 is a
		// failed pending seed, not grandfathered content — so it must fail closed
		// on every display surface for anyone who is not its author.
		assert.NotContainsf(t, communityFeed(t, publicViewer), postv2,
			"a postv2 with no admission row leaked to the public in the community feed. The consumer always seeds a "+
				"pending row on index, so no-row means the seed failed — the read path must FAIL CLOSED for postv2, "+
				"not fail open as it does for legacy posts (this is the hole this task closes)")
		assert.NotContainsf(t, communityFeed(t, stranger), postv2,
			"a postv2 with no admission row leaked to a non-author (an authenticated stranger) in the community feed")

		disc, _, err := discoverRepo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50,
		})
		require.NoError(t, err)
		assert.NotContains(t, discoverFeedURIs(disc), postv2,
			"a postv2 with no admission row leaked into discover")

		views, err := postRepo.GetViewsByURIs(ctx, []string{postv2})
		require.NoError(t, err)
		assert.NotContainsf(t, views, postv2,
			"post.get served a postv2 with no admission row to the public — permalink is the alternate path the feed "+
				"gate is worthless without")
	})

	t.Run("the postv2 with no admission row is visible to its own author", func(t *testing.T) {
		// A failed/absent seed must not cost the AUTHOR their own post. On the
		// surfaces that thread a viewer DID (the feed does), the author sees their
		// own postv2 even with no admission row, exactly as they see their own
		// pending one.
		assert.Containsf(t, communityFeed(t, author), postv2,
			"the author of a postv2 with no admission row cannot see their own post in the community feed; a missing "+
				"seed must fail closed for OTHERS, never for the author")

		// NOTE — post.get's author path is a known follow-up, not covered here.
		// GetViewsByURIs takes no viewer DID (posts.Repository's 2-arg signature),
		// so post.get currently hides a non-accepted postv2 from its author too.
		// Threading a viewer would break the three in-suite Repository fakes, so it
		// is deferred: the author reaches their own pending/no-admission posts
		// through actor.getPosts (TestActorPostsVisibility_AuthorVsNonAuthor) and
		// getStatus, which is sufficient. Flagged in the cycle-2 report.
	})
}

// TestGetCommentsVisibility_HeaderIsAdmissionAndDeleteAware closes the read-path
// hole getComments has carried since 2026-07-29. GetComments hydrates its thread
// header through postRepo.GetByURI, which has NO admission gate and NO
// `deleted_at IS NULL` filter — so the comment thread endpoint serves the full
// header (title, content, author) of a post the feeds correctly hide: a pending
// postv2, and a soft-deleted post. The header must go through the same
// admission+deleted-aware fetch the feeds use.
//
// Driven through the comment SERVICE rather than a bare repo call, because the
// defect is in which fetch GetComments chooses — a repo-only test could not see
// it pick the leaky one.
func TestGetCommentsVisibility_HeaderIsAdmissionAndDeleteAware(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "gc")
	author := "did:plc:visgcauthor"
	createTestUser(t, db, "visgcauthor.test", author)
	stranger := "did:plc:visgcstranger"
	createTestUser(t, db, "visgcstranger.test", stranger)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	acceptedV2 := seedVisibilityPost(t, db, community, author, "gcacc", "accepted header", base.Add(6*time.Hour))
	pendingV2 := seedVisibilityPost(t, db, community, author, "gcpen", "pending header", base.Add(5*time.Hour))
	removedV2 := seedVisibilityPost(t, db, community, author, "gcrv2", "removed postv2 header", base.Add(4*time.Hour))
	seedVisibilityAdmission(t, db, community, acceptedV2, posts.AdmissionStatusAccepted, "bafypostv2gcacc", "")
	seedVisibilityAdmission(t, db, community, pendingV2, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, removedV2, posts.AdmissionStatusRemoved, "", "rule-violation")

	// A LEGACY community.post carrying a REMOVED admission row. A legacy post can
	// hold one — applyRemoval has no collection guard (unlike applyAcceptance) — so
	// a moderator removes a legacy post exactly as they remove a postv2. This is
	// the P1 leak: GetViewsByURIs correctly omits it, but a header gate that infers
	// "omitted + non-postv2 = show it" serves its content to the anonymous public.
	removedLegacy := seedFilterablePost(t, db, community, author, "gcrleg", base.Add(3*time.Hour))
	seedVisibilityAdmission(t, db, community, removedLegacy, posts.AdmissionStatusRemoved, "", "rule-violation")

	// A LEGACY community.post that has been SOFT-DELETED, with no admission row.
	// This is the REAL 2026-07-29 case: nothing but deleted_at hides it (a legacy
	// post with no admission is otherwise visible), so it isolates the deleted_at
	// gate that GetByURI omits.
	deletedLegacy := seedFilterablePost(t, db, community, author, "gcdleg", base.Add(2*time.Hour))
	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deletedLegacy)
	require.NoError(t, err)

	service := comments.NewCommentServiceWithPDSFactory(
		NewCommentRepository(db),
		NewUserRepository(db),
		NewPostRepository(db),
		NewCommunityRepository(db),
		nil, nil,
	)

	header := func(t *testing.T, postURI, viewerDID string) (*comments.GetCommentsResponse, error) {
		t.Helper()
		var viewer *string
		if viewerDID != "" {
			viewer = &viewerDID
		}
		return service.GetComments(ctx, &comments.GetCommentsRequest{PostURI: postURI, ViewerDID: viewer})
	}

	requireHidden := func(t *testing.T, postURI, viewer, what string) {
		t.Helper()
		_, err := header(t, postURI, viewer)
		require.Errorf(t, err, "getComments served %s", what)
		assert.ErrorIsf(t, err, comments.ErrRootNotFound,
			"%s must be root-not-found through getComments, the same answer post.get gives", what)
	}

	t.Run("an accepted post serves its header, carrying the hydrated admission context", func(t *testing.T) {
		resp, err := header(t, acceptedV2, stranger)
		require.NoError(t, err, "getComments must serve the header of an accepted post")
		require.NotNil(t, resp.Post)
		postView, ok := resp.Post.(*posts.PostView)
		require.Truef(t, ok, "getComments post header is %T, not *posts.PostView", resp.Post)
		assert.Equal(t, acceptedV2, postView.URI)

		// P5: the header the visibility check hydrated (via GetViewsByURIs) must be
		// the one served — not a second view rebuilt from the raw Post, which drops
		// the admission context. status and acceptanceUri are the fields postView
		// gained for exactly this (PRD §6.2); a thread header that omits them makes
		// the thread endpoint disagree with post.get about a post they both serve.
		assert.Equalf(t, string(posts.AdmissionStatusAccepted), postView.Status,
			"the served thread header dropped the admission status. buildPostView rebuilds the view from the raw Post "+
				"and discards the admission-aware view GetViewsByURIs already hydrated — reuse that view instead")
		assert.NotEmptyf(t, postView.AcceptanceURI,
			"the served thread header dropped acceptanceUri — a client cannot follow the accepted post to its attestation")
	})

	t.Run("the author sees their own pending post's header", func(t *testing.T) {
		resp, err := header(t, pendingV2, author)
		require.NoError(t, err, "an author must reach their own pending post's thread header, matching the feed's author branch")
		require.NotNil(t, resp.Post)
	})

	t.Run("a pending postv2 header is hidden from a non-author", func(t *testing.T) {
		requireHidden(t, pendingV2, stranger, "a non-author the header of a PENDING postv2")
	})

	t.Run("a removed postv2 header is hidden from a non-author", func(t *testing.T) {
		requireHidden(t, removedV2, stranger, "a non-author the header of a REMOVED postv2")
	})

	t.Run("a removed LEGACY post's header is hidden from a non-author (P1 leak)", func(t *testing.T) {
		// The header gate must not infer visibility from the collection. A legacy
		// post with a removed admission row is omitted by GetViewsByURIs for a REAL
		// reason, and treating "omitted + non-postv2" as "show it" serves a
		// moderator-removed post's content to anyone.
		requireHidden(t, removedLegacy, stranger,
			"a non-author the content of a moderator-REMOVED legacy community.post (the collection-inference leak)")
	})

	t.Run("a soft-deleted LEGACY post no longer leaks (the real 2026-07-29 case)", func(t *testing.T) {
		requireHidden(t, deletedLegacy, stranger,
			"a non-author the header of a SOFT-DELETED legacy post — nothing but deleted_at hides it, and GetByURI has no such filter")
	})
}

// TestCommunityPostCountVisibility_AcceptedOnly pins that a community's
// post_count reflects accepted posts only (PRD §6.2: counts must not include
// non-accepted rows).
//
// UNLIKE the user post_count, which is a live COUNT this task can gate directly,
// community.post_count is a STORED column with a write-time incrementer
// (community_repo_memberships.go) left over from the old community-repo write
// path. Under author-owned posts nothing increments it on acceptance, so it is
// already stale — and the honest fix is consumer-side (increment on the accept
// transition, decrement on remove/unaccept), NOT a read predicate.
//
// This pins the accepted-only SEMANTICS against countAcceptedPostsForCommunity —
// the source of truth GREEN would drive the counter from, whether it recomputes
// live or reconciles the stored column from the admission consumer. See the
// cycle-2 report for the sequencing recommendation (this is a consumer-side
// follow-up, not part of the read-path predicate).
func TestCommunityPostCountVisibility_AcceptedOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "cc")
	author := "did:plc:visccauthor"
	createTestUser(t, db, "visccauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := seedVisibilityPost(t, db, community, author, "ccacc", "accepted", base.Add(3*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "ccpen", "pending", base.Add(2*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "ccrem", "removed", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2ccacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	count, err := countAcceptedPostsForCommunity(ctx, db, community)
	require.NoError(t, err)
	assert.Equalf(t, 1, count,
		"a community's accepted-post count must be 1 (3 seeded: accepted, pending, removed); a count that includes "+
			"non-accepted rows advertises content no reader can reach")
}

// TestPostGetVisibility_AcceptedBranchHonorsPinnedCID pins §5.5 at the READ path:
// an acceptance pins a CID, and edited content must never render under it. The
// admission consumer commits an edit's new content (posts.cid advances) in a
// SEPARATE transaction from the admission transition (accepted →
// pending_reacceptance), so there is a window where the row is still
// status='accepted' but posts.cid no longer equals accepted_cid. In that window
// the accepted content the community attested to is GONE and the new,
// un-attested content stands in its place — and a predicate that gates on
// status='accepted' alone renders it.
//
// The gate must also check a.accepted_cid = p.cid. This seeds the mismatch
// directly (accepted_cid one value, the post's cid another) rather than racing
// the consumer.
func TestPostGetVisibility_AcceptedBranchHonorsPinnedCID(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "pc")
	author := "did:plc:vispcauthor"
	createTestUser(t, db, "vispcauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// The post's real content CID is "bafypostv2pcmis" (seedVisibilityPost derives
	// it from the rkey). The acceptance pins a DIFFERENT, older CID.
	mismatched := seedVisibilityPost(t, db, community, author, "pcmis", "edited past the acceptance", base.Add(2*time.Hour))
	seedVisibilityAdmission(t, db, community, mismatched, posts.AdmissionStatusAccepted, "bafySTALEacceptedcid", "")

	// A control whose acceptance pins the CURRENT content CID.
	matched := seedVisibilityPost(t, db, community, author, "pcmat", "still matches the acceptance", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, matched, posts.AdmissionStatusAccepted, "bafypostv2pcmat", "")

	postRepo := NewPostRepository(db)
	views, err := postRepo.GetViewsByURIs(ctx, []string{mismatched, matched})
	require.NoError(t, err)

	assert.Containsf(t, views, matched, "an accepted post whose pinned CID still matches its content must be visible")
	assert.NotContainsf(t, views, mismatched,
		"post.get served an 'accepted' post whose pinned CID no longer matches its content. The acceptance attests to "+
			"accepted_cid, and posts.cid has moved past it (an edit landed before the pending_reacceptance transition) — "+
			"rendering it shows un-attested content under a stale acceptance (§5.5). The accepted branch must AND "+
			"a.accepted_cid = p.cid.")

	feedRepo := NewCommunityFeedRepository(db, "test-secret")
	feed, _, err := feedRepo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
		Community: community, ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50,
	})
	require.NoError(t, err)
	got := feedURIs(feed)
	assert.Contains(t, got, matched)
	assert.NotContainsf(t, got, mismatched,
		"the community feed rendered an accepted post whose content has drifted past its pinned CID (§5.5 read-side leak)")
}

// TestFeedsVisibility_UnknownAuthorAccepted guards the §5.3 open-posting promise
// on EVERY feed, not just post.get. An accepted post by an author with no `users`
// row (a federated author the AppView has not hydrated) must appear, with its
// handle COALESCE'd to the author DID. The users join on each feed is a LEFT join
// for exactly this — reverting any one of them to INNER would silently vanish
// every federated author's accepted post, and cycle 1 pinned this on post.get
// alone. These are the per-feed regression guards.
func TestFeedsVisibility_UnknownAuthorAccepted(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "ua")
	// Deliberately NO createTestUser: this author is indexed nowhere.
	unknownAuthor := "did:plc:visuaunknownfederated"

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	post := seedVisibilityPost(t, db, community, unknownAuthor, "uapost", "federated accepted", base.Add(1*time.Hour))
	seedVisibilityAdmission(t, db, community, post, posts.AdmissionStatusAccepted, "bafypostv2uapost", "")

	assertUnknownAuthorHandle := func(t *testing.T, pv *posts.PostView) {
		t.Helper()
		require.NotNil(t, pv.Author)
		assert.Equalf(t, unknownAuthor, pv.Author.Handle,
			"an unindexed author's handle must COALESCE to their DID; a NULL handle means the feed is INNER-joining users "+
				"and dropping every federated author's accepted post (§5.3)")
	}

	t.Run("feed.getCommunity", func(t *testing.T) {
		feed, _, err := NewCommunityFeedRepository(db, "test-secret").GetCommunityFeed(ctx,
			communityFeeds.GetCommunityFeedRequest{Community: community, ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50})
		require.NoError(t, err)
		require.Containsf(t, feedURIs(feed), post, "the community feed dropped an accepted post by an unindexed author (users INNER join?)")
		assertUnknownAuthorHandle(t, feed[0].Post)
	})

	t.Run("feed.getDiscover", func(t *testing.T) {
		feed, _, err := NewDiscoverRepository(db, "test-secret").GetDiscover(ctx,
			discover.GetDiscoverRequest{ViewerDID: publicViewer, Sort: visibilitySort, Limit: 50})
		require.NoError(t, err)
		require.Containsf(t, discoverFeedURIs(feed), post, "discover dropped an accepted post by an unindexed author")
		assertUnknownAuthorHandle(t, feed[0].Post)
	})

	t.Run("feed.getTimeline", func(t *testing.T) {
		subscriber := "did:plc:visuasubscriber"
		createTestUser(t, db, "visuasubscriber.test", subscriber)
		_, err := db.ExecContext(ctx, `INSERT INTO community_subscriptions (user_did, community_did, subscribed_at) VALUES ($1, $2, NOW())`, subscriber, community)
		require.NoError(t, err)
		feed, _, err := NewTimelineRepository(db, "test-secret").GetTimeline(ctx,
			timeline.GetTimelineRequest{UserDID: subscriber, Sort: visibilitySort, Limit: 50})
		require.NoError(t, err)
		require.Containsf(t, timelineFeedURIs(feed), post, "the timeline dropped an accepted post by an unindexed author")
		assertUnknownAuthorHandle(t, feed[0].Post)
	})

	t.Run("actor.getPosts", func(t *testing.T) {
		views, _, err := NewPostRepository(db).GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: unknownAuthor, Limit: 50})
		require.NoError(t, err)
		require.Lenf(t, views, 1, "actor.getPosts dropped an accepted post by an unindexed author")
		assert.Equal(t, post, views[0].URI)
		assertUnknownAuthorHandle(t, views[0])
	})
}

// TestActorCommentsVisibility_RootIsReferenceOnly pins P4's finding: GetActorComments
// carries only a REFERENCE to each comment's root post (root_uri / root_cid), never
// the root's hydrated content — so a comment on a pending or removed root leaks
// nothing about that root, and following the reference lands on the gated post.get.
// The comment itself is the actor's own public speech and is still listed; the root
// being hidden does not suppress it. This is the reference-only guarantee that keeps
// actor.getComments off the leak list.
func TestActorCommentsVisibility_RootIsReferenceOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "ac")
	actor := "did:plc:visacactor"
	createTestUser(t, db, "visacactor.test", actor)

	// The root is a PENDING postv2 — hidden from the public everywhere else.
	root := seedVisibilityPost(t, db, community, actor, "acroot", "secret pending root", time.Now().Add(-time.Hour))
	seedVisibilityAdmission(t, db, community, root, posts.AdmissionStatusPending, "", "")

	commentURI := "at://" + actor + "/social.coves.community.comment/accmt1"
	_, err := db.ExecContext(ctx, `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, NOW())
	`, commentURI, "bafycmtac", "accmt1", actor, root, "bafypostv2acroot", "a comment on a hidden root")
	require.NoError(t, err)

	service := comments.NewCommentServiceWithPDSFactory(
		NewCommentRepository(db), NewUserRepository(db), NewPostRepository(db), NewCommunityRepository(db), nil, nil,
	)

	resp, err := service.GetActorComments(ctx, &comments.GetActorCommentsRequest{ActorDID: actor, Limit: 50})
	require.NoError(t, err)
	require.Lenf(t, resp.Comments, 1, "the actor's comment must be listed even though its root is hidden — the comment is the actor's own public record")

	cv := resp.Comments[0]
	require.Equalf(t, commentURI, cv.URI, "the listed comment must be the actor's own comment")
	require.NotNil(t, cv.Post, "the comment must reference its root")
	assert.Equalf(t, root, cv.Post.URI, "actor.getComments must carry the root as a reference (uri/cid), which following lands on the gated post.get")
	// The response shape carries no root-content field at all: the guarantee is
	// structural — CommentView holds the root only as a CommentRef (uri/cid), so
	// the root's title/body cannot appear here even when it is a hidden post.
}
