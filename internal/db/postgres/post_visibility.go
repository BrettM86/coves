package postgres

import (
	"fmt"

	"Coves/internal/core/posts"
)

// The centralized read-path visibility predicate (task 7, PRD §6.2).
//
// This is the single admission-aware gate every posts display query goes
// through so that no read path can forget it (the piecemeal-predicate failure
// PRD §6.2 calls out). ONE predicate string serves all of them —
// visiblePostsPredicate below — and everything else in this file is a different
// way of BINDING ITS VIEWER, never a second copy of the rule:
//
//   - visiblePostsJoin(n)              — viewer bound to $n. GetViewsByURIs,
//     VisibleHeaderView, GetByAuthor, the three feed queries, and the profile
//     post_count.
//   - visiblePostCountSubquery(expr)   — viewer bound to the anonymous literal,
//     wrapped in a COUNT over one community. The served community postCount.
//
// The counts used to hand-copy the predicate instead. A reviewer demonstrated
// three separate mutations to that copy — dropping the collection check,
// dropping the pinned-CID equality, dropping the community half of the join key
// — that no test caught, because a copy has no way to disagree with itself.
// There is nothing to copy now.
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
	return visiblePostsPredicate(fmt.Sprintf("$%d", viewerParam))
}

// anonymousViewerSQL is the viewer expression for a read that HAS no viewer: a
// literal empty string, which is the same value visiblePostsJoin's callers bind
// for an anonymous read.
//
// It is a SQL literal rather than a bound parameter because there is no input
// here to bind — the public count of a community's posts is definitionally
// viewer-independent, so there is no caller-supplied value that could reach it.
// The constant is spliced into a query built entirely from other constants; no
// user data is concatenated into SQL anywhere in this file.
const anonymousViewerSQL = `''`

// visiblePostsPredicate is THE read-path predicate, and the only place it is
// spelled. viewerExpr is whatever SQL evaluates to the viewer's DID — a bind
// parameter reference for a query that has a viewer to bind, or
// anonymousViewerSQL for one that structurally does not.
//
// Everything above about the join key, the collection-aware status rule and the
// pinned CID describes THIS function; visiblePostsJoin is the parameterized
// spelling of it. Two viewer bindings, one predicate: a count and a display
// query cannot disagree about what "visible" means, because there is only one
// string.
func visiblePostsPredicate(viewerExpr string) (joinSQL, whereSQL string) {
	joinSQL = `
			LEFT JOIN community_post_admissions a
				ON a.community_did = p.community_did AND a.post_uri = p.uri`

	whereSQL = fmt.Sprintf(`(
				(a.status = 'accepted' AND a.accepted_cid = p.cid)
				OR (a.status IS NULL AND (split_part(p.uri, '/', 4) <> '%s' OR p.author_did = %s))
				OR (a.status IN ('pending', 'pending_reacceptance', 'removed', 'rejected') AND p.author_did = %s)
			)`, posts.PostV2Collection, viewerExpr, viewerExpr)

	return joinSQL, whereSQL
}

// visiblePostCountSubquery renders a scalar subquery counting the posts that are
// PUBLICLY visible in the community named by communityExpr — the same predicate
// every display query runs, with the anonymous viewer.
//
// It exists because `communities.post_count` was a STORED column, and the only
// thing that ever incremented it (community_repo_memberships.go's
// IncrementPostCount) belonged to the retired community-repo write path, so
// nothing has advanced it since posts became author-owned: every community's
// postCount served 0 on community.get/.list/.search, and `ORDER BY post_count`
// was a sort on a uniformly-zero key. A stored counter also has to be advanced
// on the accept transition AND decremented on remove/unaccept/tombstone, and
// every one of those is a chance to drift out of agreement with what the read
// path will actually render. A live subquery over the same predicate cannot
// drift by construction: it IS the read path's answer.
//
// communityExpr must be a column reference or a bind parameter — it is spliced,
// never a caller-supplied value.
//
// It is COLLECTION-AWARE, which an accepted-only count is not. A legacy
// social.coves.community.post lives in the community's own repo, is accepted by
// construction, and carries no admission row at all (§3.0) — an accepted-only
// count silently undercounts every community that still holds legacy posts,
// which today is all of them. Counting exactly what the predicate renders is
// also the only definition that cannot advertise unreachable content: the count
// and the feed answer the same question.
func visiblePostCountSubquery(communityExpr string) string {
	joinSQL, whereSQL := visiblePostsPredicate(anonymousViewerSQL)
	return `(SELECT COUNT(*)
			FROM posts p` + joinSQL + `
			WHERE p.community_did = ` + communityExpr + `
				AND p.deleted_at IS NULL
				AND ` + whereSQL + `)`
}
