package postgres

// The centralized read-path visibility predicate (task 7, PRD §6.2).
//
// STUB — signature only. This is the single admission-aware join every posts
// display query must go through so that no read path can forget the gate (the
// piecemeal-predicate failure PRD §6.2 calls out). GREEN fills in the body and
// wires it into GetViewsByURIs, the three feed queries, GetByAuthor and the
// profile count; the visibility suites in post_visibility_test.go are red until
// it does.
//
// THE JOIN KEY IS (a.community_did = p.community_did AND a.post_uri = p.uri).
// Both halves are load-bearing. The post_uri half selects the subject; the
// community_did half is what makes a post visible iff ITS OWN community accepted
// it, which is the fork-case security property TestDiscoverVisibility_ForkJoinKey
// pins — a join on post_uri alone would let one community's acceptance publish
// another community's pending post.
//
// THE STATUS RULE depends on the viewer:
//   - a non-author (viewerDID == "" or != the post's author): status = 'accepted'
//     only.
//   - the author of the post (viewerDID == posts.author_did): accepted OR the
//     author's own non-accepted rows, so a client can render 'pending' / 'removed'
//     on the author's own profile.
//
// visiblePostsJoin returns the SQL fragment to splice after the posts `p`
// reference (a JOIN plus its WHERE contribution) and the arguments it binds,
// starting at paramOffset. Returning the empty fragment — the stub's behavior —
// applies NO gate, which is the pre-task-7 state every visibility suite fails
// against.
func visiblePostsJoin(viewerDID string, paramOffset int) (sqlFragment string, args []interface{}) {
	return "", nil
}

// Referenced so the stub is not flagged as dead before GREEN wires it into the
// read queries. Delete this line once visiblePostsJoin has a real caller.
var _ = visiblePostsJoin
