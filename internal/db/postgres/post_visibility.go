package postgres

import (
	"context"
	"database/sql"
	"fmt"
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
// # The status rule
//
// The gate turns on the admission row the LEFT JOIN produced for the post's OWN
// community:
//
//   - a.status = 'accepted'  → visible to everyone.
//   - a.status IS NULL       → visible. No admission row exists for this
//     (community, post): a legacy community-repo post, a bridged post, or any
//     other collection that never goes through the admission engine, plus the
//     narrow window before the consumer opens a fresh postv2's pending row
//     (authorpost.go writes the post and its pending admission in separate
//     transactions). These carry no decision to gate on, so they stay visible
//     exactly as they were before task 7 — the read path is not the place to
//     retro-hide content that predates admissions.
//   - a.status IN (pending, pending_reacceptance, removed, rejected) AND the
//     viewer is the author → visible. An author sees their own posts in every
//     admission state so a client can render "pending review" / "removed" on the
//     author's own profile (PRD §6.2). Any OTHER viewer — including the anonymous
//     public, whose DID is "" — sees accepted content only. This is the security
//     core: every non-accepted post that HAS a decision is invisible to
//     non-authors on every read path.
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
				a.status = 'accepted'
				OR a.status IS NULL
				OR (a.status IN ('pending', 'pending_reacceptance', 'removed', 'rejected') AND p.author_did = $%d)
			)`, viewerParam)

	return joinSQL, whereSQL
}

// countAcceptedPostsForCommunity is the accepted-only source of truth for a
// community's post_count (task 7, PRD §6.2).
//
// STUB — returns 0 until GREEN implements it. It counts the posts a community has
// ACCEPTED: a join of `posts` to community_post_admissions on the subject key
// with status = 'accepted'. It exists because community.post_count is a STORED
// column whose only incrementer is the old community-repo write path
// (community_repo_memberships.go) — nothing advances it on an acceptance, so it
// is stale under author-owned posts. Whether GREEN recomputes the count live or
// reconciles the stored column from the admission consumer, THIS is the value it
// must converge on. The read paths already exclude non-accepted rows, so this is
// a counting concern, not a content leak — see the cycle-2 report for the
// recommendation to sequence the counter itself as a consumer-side follow-up.
func countAcceptedPostsForCommunity(ctx context.Context, db *sql.DB, communityDID string) (int, error) {
	return 0, nil
}

// Referenced so the count stub is not flagged as dead before GREEN wires it into
// the community counter. Delete this line once it has a real caller.
var _ = countAcceptedPostsForCommunity
