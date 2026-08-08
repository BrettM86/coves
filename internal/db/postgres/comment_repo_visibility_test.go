//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actor.getComments' COMMUNITY FILTER is a posts read path wearing a comments
// costume (PRD §6.2 names "comment community filters" in the must-convert
// inventory), and it is the one surface where the removed write barrier can be
// rebuilt one table over.
//
// Under community-owned posts, only a key holder could put content under a
// community's name. Under author-owned posts ANY user writes a postv2 naming ANY
// community, and only the admission row makes it that community's content. The
// comment consumer indexes a comment whatever its root is — it does not validate
// that the root exists, let alone that it was admitted — so an attacker can:
//
//  1. write a postv2 naming community C, which C never accepts (banned author,
//     rejected content, quota) and which is therefore correctly hidden in every
//     feed, in post.get, and on the profile;
//  2. write a comment rooted at that hidden post;
//  3. call getComments?actor=<attacker>&community=C.
//
// If the community filter is a bare `root_uri IN (SELECT uri FROM posts WHERE
// community_did = $n)`, step 3 returns the attacker's comment — content, facets,
// embeds, labels, score — inside a listing every client renders as "this user's
// comments in community C". C admitted nothing and is publishing the attacker
// anyway.
//
// So the filter must ask the same question every other posts read path asks:
// visiblePostsJoin plus deleted_at IS NULL, with the VIEWER bound, so that the
// author's own carve-out survives and nobody else's does.
//
// What this suite must NOT change is the UNFILTERED listing: a person's comments
// are their own public speech and stay listed even when the root is hidden — that
// is TestActorCommentsVisibility_RootIsReferenceOnly in post_visibility_test.go,
// and the reference-only response shape is what makes it safe. The community
// filter is different precisely because the filter itself is a claim about the
// community: naming C in the query is asking for C's scope, and C's scope is what
// C admitted.

// seedActorComment inserts one comment by commenter rooted at rootURI.
func seedActorComment(t *testing.T, db *sql.DB, commenter, rootURI, rkey string, createdAt time.Time) string {
	t.Helper()

	uri := "at://" + commenter + "/social.coves.community.comment/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8)
	`, uri, "bafycmt"+rkey, rkey, commenter, rootURI, "bafyroot"+rkey, "comment "+rkey, createdAt)
	require.NoErrorf(t, err, "seeding comment %s", rkey)
	return uri
}

// seedLegacyPost inserts a DEPRECATED community-repo post
// (social.coves.community.post), whose authority is the COMMUNITY's DID. It
// carries no admission row by construction — it was never routed through the
// admission engine — and visiblePostsJoin grandfathers exactly that shape until
// task 8's drain. The community filter must keep serving comments on these or the
// drain window goes dark.
func seedLegacyPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey, title string, createdAt time.Time) string {
	t.Helper()

	uri := "at://" + communityDID + "/" + posts.LegacyPostCollection + "/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0)
	`, uri, "bafylegacy"+rkey, rkey, authorDID, communityDID, title, createdAt, 1, 1)
	require.NoErrorf(t, err, "seeding legacy post %s", rkey)
	return uri
}

// listActorComments runs the community-filtered profile listing as a given
// viewer and returns the comment URIs it served.
func listActorComments(t *testing.T, repo comments.Repository, commenter, communityDID, viewerDID string) []string {
	t.Helper()

	req := comments.ListByCommenterRequest{
		CommenterDID: commenter,
		Limit:        50,
		ViewerDID:    viewerDID,
	}
	if communityDID != "" {
		req.CommunityDID = &communityDID
	}
	page, _, err := repo.ListByCommenterWithCursor(context.Background(), req)
	require.NoError(t, err, "the community-filtered profile listing must not fail")
	return commentURIs(page)
}

// TestActorCommentsCommunityFilter_AdmissionGated is the leak test: the
// attacker's comment on their own never-admitted post must not come back under
// the community's scope.
func TestActorCommentsCommunityFilter_AdmissionGated(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	community := visibilityCommunity(t, db, "acf")
	attacker := "did:plc:visacfattacker"
	createTestUser(t, db, "visacfattacker.test", attacker)

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	// The four admission states a postv2 can hold in its own community, plus the
	// comment each one carries.
	accepted := seedVisibilityPost(t, db, community, attacker, "acfacc", "accepted", base.Add(5*time.Hour))
	pending := seedVisibilityPost(t, db, community, attacker, "acfpen", "pending", base.Add(4*time.Hour))
	rejected := seedVisibilityPost(t, db, community, attacker, "acfrej", "rejected", base.Add(3*time.Hour))
	removed := seedVisibilityPost(t, db, community, attacker, "acfrem", "removed", base.Add(2*time.Hour))
	// And a postv2 whose admission row never got seeded at all — the failed-seed
	// case visiblePostsJoin fails CLOSED on.
	unseeded := seedVisibilityPost(t, db, community, attacker, "acfnos", "no admission row", base.Add(time.Hour))

	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2acfacc", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, rejected, posts.AdmissionStatusRejected, "", "spam")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")

	onAccepted := seedActorComment(t, db, attacker, accepted, "acfc1", base.Add(5*time.Hour))
	onPending := seedActorComment(t, db, attacker, pending, "acfc2", base.Add(4*time.Hour))
	onRejected := seedActorComment(t, db, attacker, rejected, "acfc3", base.Add(3*time.Hour))
	onRemoved := seedActorComment(t, db, attacker, removed, "acfc4", base.Add(2*time.Hour))
	onUnseeded := seedActorComment(t, db, attacker, unseeded, "acfc5", base.Add(time.Hour))

	repo := NewCommentRepository(db)

	t.Run("a public caller sees only comments on admitted posts", func(t *testing.T) {
		got := listActorComments(t, repo, attacker, community, publicViewer)

		assert.ElementsMatchf(t, []string{onAccepted}, got,
			"the community-filtered profile listing leaked comments rooted at posts community %s never admitted. "+
				"An attacker writes a postv2 naming a community, the community rejects or never accepts it, and then "+
				"comments on their own hidden post: this endpoint is where that content re-enters the community's "+
				"scope in full — content, facets, embeds, labels, score — under a heading the client renders as "+
				"'this user's comments in %s'. The filter must run visiblePostsJoin, not a bare community_did match. "+
				"(pending=%s rejected=%s removed=%s no-admission-row=%s)",
			community, community, onPending, onRejected, onRemoved, onUnseeded)
	})

	t.Run("a third-party viewer sees the same thing", func(t *testing.T) {
		// The author carve-out is keyed to the POST's author. A logged-in stranger
		// must not inherit it just by being authenticated.
		stranger := "did:plc:visacfstranger"
		createTestUser(t, db, "visacfstranger.test", stranger)

		got := listActorComments(t, repo, attacker, community, stranger)

		assert.ElementsMatchf(t, []string{onAccepted}, got,
			"an authenticated stranger saw comments on unadmitted posts. The visibility predicate's author branch is "+
				"`p.author_did = $viewer`, so only the POST's own author may see it — being logged in is not the gate")
	})

	t.Run("the post's own author still sees their own unadmitted roots", func(t *testing.T) {
		// PRD §6.2: an author sees their own posts in every admission state so a
		// client can render "pending review" / "removed" on their own profile. The
		// same must hold for their comment history, or the author's own pending
		// thread disappears from their own view.
		got := listActorComments(t, repo, attacker, community, attacker)

		assert.ElementsMatchf(t,
			[]string{onAccepted, onPending, onRejected, onRemoved, onUnseeded}, got,
			"the author's own community-filtered comment history dropped comments on their own posts. The viewer DID "+
				"must be bound into visiblePostsJoin so the author carve-out survives the filter")
	})

	t.Run("the unfiltered listing is unchanged", func(t *testing.T) {
		// The reference-only guarantee (TestActorCommentsVisibility_RootIsReferenceOnly)
		// still holds: without a community in the query there is no claim about a
		// community's scope, and a person's comments are their own public speech.
		got := listActorComments(t, repo, attacker, "", publicViewer)

		assert.ElementsMatchf(t,
			[]string{onAccepted, onPending, onRejected, onRemoved, onUnseeded}, got,
			"the UNFILTERED profile listing must keep serving every comment the actor wrote — the root is carried as a "+
				"reference (uri/cid) only, so a hidden root leaks nothing, and suppressing the comment would delete a "+
				"person's own speech from their own profile")
	})
}

// TestActorCommentsCommunityFilter_DeletedRoot pins the second half of the fix:
// the old subquery had no deleted_at filter either, so an author who WITHDREW
// their post still had its comment thread listed under the community.
func TestActorCommentsCommunityFilter_DeletedRoot(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "acd")
	actor := "did:plc:visacdactor"
	createTestUser(t, db, "visacdactor.test", actor)

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	live := seedVisibilityPost(t, db, community, actor, "acdliv", "live", base.Add(2*time.Hour))
	withdrawn := seedVisibilityPost(t, db, community, actor, "acddel", "withdrawn", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, community, live, posts.AdmissionStatusAccepted, "bafypostv2acdliv", "")
	seedVisibilityAdmission(t, db, community, withdrawn, posts.AdmissionStatusAccepted, "bafypostv2acddel", "")

	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, withdrawn)
	require.NoError(t, err)

	onLive := seedActorComment(t, db, actor, live, "acdc1", base.Add(2*time.Hour))
	seedActorComment(t, db, actor, withdrawn, "acdc2", base.Add(time.Hour))

	repo := NewCommentRepository(db)

	t.Run("a public caller does not reach the withdrawn root's thread", func(t *testing.T) {
		got := listActorComments(t, repo, actor, community, publicViewer)
		assert.ElementsMatchf(t, []string{onLive}, got,
			"a comment rooted at a SOFT-DELETED post was still listed under the community. The author withdrew that "+
				"post; the community filter must exclude deleted roots (deleted_at IS NULL) exactly as every other "+
				"posts read path does")
	})

	t.Run("the author does not reach it either", func(t *testing.T) {
		// A soft delete is terminal for display on every path, including the
		// author's own — unlike a non-accepted admission, which the author may see.
		got := listActorComments(t, repo, actor, community, actor)
		assert.ElementsMatchf(t, []string{onLive}, got,
			"the author self-view must not resurrect a deleted root: deleted_at is a separate gate from admission "+
				"status and the author carve-out does not cross it")
	})
}

// TestActorCommentsCommunityFilter_LegacyRootStaysVisible is the drain-window
// pin. A deprecated community-repo post carries no admission row, and
// visiblePostsJoin treats a missing row as VISIBLE for non-postv2 collections
// (§11's gated follow-up retires that branch only once the drain is confirmed).
// A filter that required an accepted row outright would blank every legacy
// thread's comment history on the profile.
func TestActorCommentsCommunityFilter_LegacyRootStaysVisible(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	community := visibilityCommunity(t, db, "acl")
	actor := "did:plc:visaclactor"
	createTestUser(t, db, "visaclactor.test", actor)

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	legacy := seedLegacyPost(t, db, community, actor, "aclleg", "legacy community-repo post", base.Add(time.Hour))
	onLegacy := seedActorComment(t, db, actor, legacy, "aclc1", base.Add(time.Hour))

	repo := NewCommentRepository(db)
	got := listActorComments(t, repo, actor, community, publicViewer)

	assert.ElementsMatchf(t, []string{onLegacy}, got,
		"the community filter dropped a comment on a DEPRECATED community-repo post (%s). Those rows never had an "+
			"admission row — they live in the community's own repo and are accepted by construction — so the filter "+
			"must reuse visiblePostsJoin's collection-aware rule rather than demanding an accepted row", legacy)

	t.Run("a legacy root that was MODERATOR-REMOVED is still hidden", func(t *testing.T) {
		// applyRemoval has no collection guard, so a legacy row CAN carry a removed
		// admission — the leak the getComments header gate was rebuilt to close.
		// The community filter inherits the same rule for free by reusing the join.
		removedLegacy := seedLegacyPost(t, db, community, actor, "aclrem", "removed legacy post", base)
		seedVisibilityAdmission(t, db, community, removedLegacy, posts.AdmissionStatusRemoved, "", "rule-violation")
		seedActorComment(t, db, actor, removedLegacy, "aclc2", base)

		stranger := "did:plc:visaclstranger"
		createTestUser(t, db, "visaclstranger.test", stranger)

		got := listActorComments(t, repo, actor, community, stranger)
		assert.ElementsMatchf(t, []string{onLegacy}, got,
			"a comment on a moderator-REMOVED legacy post was listed under the community. Removal is a real admission "+
				"row on a legacy URI, and the collection-aware rule only grandfathers the ABSENCE of a row")
	})
}

// TestActorCommentsCommunityFilter_PagesWithCursor is the parameter-arithmetic
// pin. ListByCommenterWithCursor builds its bind numbers by hand: $1/$2 fixed,
// two cursor values, then the community DID — and now the viewer DID as well.
// Page two of a community-filtered profile is the only request that binds all six
// at once, and an off-by-one there is not a wrong answer, it is a "there is no
// parameter $6" from Postgres in production only.
func TestActorCommentsCommunityFilter_PagesWithCursor(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	community := visibilityCommunity(t, db, "acp")
	actor := "did:plc:visacpactor"
	createTestUser(t, db, "visacpactor.test", actor)

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	root := seedVisibilityPost(t, db, community, actor, "acproo", "accepted root", base.Add(10*time.Hour))
	seedVisibilityAdmission(t, db, community, root, posts.AdmissionStatusAccepted, "bafypostv2acproo", "")

	hidden := seedVisibilityPost(t, db, community, actor, "acphid", "pending root", base.Add(9*time.Hour))
	seedVisibilityAdmission(t, db, community, hidden, posts.AdmissionStatusPending, "", "")
	seedActorComment(t, db, actor, hidden, "acphc", base.Add(9*time.Hour))

	var want []string
	for i, rkey := range []string{"acpp1", "acpp2", "acpp3", "acpp4", "acpp5"} {
		want = append(want, seedActorComment(t, db, actor, root, rkey, base.Add(time.Duration(5-i)*time.Hour)))
	}

	repo := NewCommentRepository(db)

	var visited []string
	req := comments.ListByCommenterRequest{
		CommenterDID: actor,
		CommunityDID: &community,
		ViewerDID:    publicViewer,
		Limit:        2,
	}
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, 10, "the cursor stopped advancing: %d pages and counting", pages)
		page, next, err := repo.ListByCommenterWithCursor(context.Background(), req)
		require.NoError(t, err,
			"paging a community-filtered profile failed. The community filter, the cursor filter and the viewer DID "+
				"each compute their own bind numbers, and page two is the only request that binds all of them")
		visited = append(visited, commentURIs(page)...)
		if next == nil {
			break
		}
		req.Cursor = next
	}

	assert.Equalf(t, want, visited,
		"paging a community-filtered profile must visit every comment on an admitted root exactly once, newest first, "+
			"and never surface the one rooted at a pending post")
}
