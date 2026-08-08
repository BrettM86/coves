package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/core/posts"
)

// The centralized read-path visibility predicate (task 7, PRD §6.2).
//
// This is the single admission-aware gate every posts display query goes
// through so that no read path can forget it (the piecemeal-predicate failure
// PRD §6.2 calls out). It is wired into GetViewsByURIs, the three feed queries
// and GetByAuthor; the profile/community counts apply the same accepted rule
// inline (a COUNT subquery has no row to hydrate).
//
// # The join key is (a.community_did = p.community_did AND a.post_uri = p.uri)
//
// Both halves are load-bearing. The post_uri half selects the subject; the
// community_did half is what makes a post visible iff ITS OWN community accepted
// it, which is the fork-case security property TestDiscoverVisibility_ForkJoinKey
// pins — a join on post_uri alone would let one community's acceptance publish
// another community's pending post. Because p.community_did and p.uri are fixed
// per posts row and community_post_admissions is unique on (community_did,
// post_uri), at most one admission row can match, so the LEFT JOIN never
// duplicates a post.
//
// # The status rule is COLLECTION-AWARE and fails closed for postv2
//
// The gate turns on the admission row the LEFT JOIN produced for the post's OWN
// community:
//
//   - a.status = 'accepted' AND a.accepted_cid = p.cid  → visible to everyone.
//     The CID equality is the §5.5 read-side guard: an acceptance attests to a
//     SPECIFIC content CID, and the admission consumer commits an edit's new
//     content (posts.cid advances) in a separate transaction from the accepted →
//     pending_reacceptance status transition. In that window the row still reads
//     'accepted' while posts.cid has moved past accepted_cid, so the attested
//     content is gone and un-attested content stands in its place. Gating on
//     status alone would render it; requiring accepted_cid = p.cid hides the
//     drifted post from EVERYONE until the status catches up (whereupon the
//     author-branch below shows the author their own pending_reacceptance).
//   - a.status IS NULL       → the collection decides. No admission row exists
//     for this (community, post), and what that MEANS depends on the collection:
//     a LEGACY community-repo post (social.coves.community.post), a bridged post,
//     or any non-postv2 collection was never routed through the admission engine
//     — it is accepted by construction (it lives in the community's own repo) and
//     must stay visible until task 8 drains it. But the task-5 consumer ALWAYS
//     seeds a pending row the moment it indexes a postv2, so a postv2 with NO row
//     is not grandfathered content — the seed failed, and the secure answer is to
//     FAIL CLOSED. So a missing row is visible for a non-postv2 collection, and
//     hidden for a postv2 (except to its own author).
//   - a.status IN (pending, pending_reacceptance, removed, rejected) AND the
//     viewer is the author → visible. An author sees their own posts in every
//     admission state so a client can render "pending review" / "removed" on the
//     author's own profile (PRD §6.2). Any OTHER viewer — including the anonymous
//     public, whose DID is "" — sees accepted content only. This is the security
//     core: every non-accepted post that HAS a decision, and every postv2 with no
//     decision, is invisible to non-authors on every read path.
//
// The collection is the fourth '/'-segment of the AT-URI
// (at://<authority>/<collection>/<rkey>), read with split_part; authorities and
// rkeys carry no '/', so segment 4 is exactly CollectionOfPostURI's answer.
//
// visiblePostsJoin returns the JOIN clause to splice into the FROM/JOIN section
// after `FROM posts p`, and the boolean WHERE fragment to AND into the query's
// WHERE clause. The caller owns the arguments: it must bind $viewerParam to the
// viewer's DID (or "" for an anonymous read), reusing a parameter it has already
// bound where the viewer DID is already in the argument list (the timeline
// reuses $1).
func visiblePostsJoin(viewerParam int) (joinSQL, whereSQL string) {
	joinSQL = `
			LEFT JOIN community_post_admissions a
				ON a.community_did = p.community_did AND a.post_uri = p.uri`

	whereSQL = fmt.Sprintf(`(
				(a.status = 'accepted' AND a.accepted_cid = p.cid)
				OR (a.status IS NULL AND (split_part(p.uri, '/', 4) <> '%s' OR p.author_did = $%d))
				OR (a.status IN ('pending', 'pending_reacceptance', 'removed', 'rejected') AND p.author_did = $%d)
			)`, posts.PostV2Collection, viewerParam, viewerParam)

	return joinSQL, whereSQL
}

// countAcceptedPostsForCommunity is the accepted-only source of truth for a
// community's post_count (task 7, PRD §6.2).
//
// It counts the posts a community has ACCEPTED: a join of `posts` to
// community_post_admissions on the subject key with status = 'accepted',
// excluding soft-deleted rows.
//
// It exists because community.post_count is a STORED column whose only
// incrementer is the old community-repo write path (community_repo_memberships.go)
// — nothing advances it on an acceptance, so it is stale under author-owned
// posts. THIS is the accepted-only value the counter must converge on.
//
// DEFERRED, deliberately not wired here: reconciling the stored column is a
// consumer-side follow-up (increment on the accept transition, decrement on
// remove/unaccept), sequenced to task 8 — not a read-path concern. There is NO
// content leak in the meantime: every display query already excludes
// non-accepted rows via visiblePostsJoin, so the stale column is a cosmetic
// count, not reachable content. This helper lands the semantics (and its pin,
// TestCommunityPostCountVisibility_AcceptedOnly) now; the incrementer follows.
func countAcceptedPostsForCommunity(ctx context.Context, db *sql.DB, communityDID string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM posts p
		JOIN community_post_admissions a
			ON a.community_did = p.community_did AND a.post_uri = p.uri
		WHERE p.community_did = $1
			AND p.deleted_at IS NULL
			AND a.status = 'accepted'
			AND a.accepted_cid = p.cid
	`, communityDID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting accepted posts for community %s: %w", communityDID, err)
	}
	return count, nil
}

// Referenced so the count stub is not flagged as dead before GREEN wires it into
// the community counter. Delete this line once it has a real caller.
var _ = countAcceptedPostsForCommunity
