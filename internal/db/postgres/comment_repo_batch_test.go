//go:build integration

package postgres

import (
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/comments"
)

// The three batch reads: GetByURIsBatch, ListByParentsBatch and
// GetVoteStateForComments.
//
// These exist for one reason — rendering a comment tree of N nodes must not
// cost N queries — and they are the only methods in the repository that return
// a MAP. That changes what can go wrong. A list either has the right rows or
// it does not; a map keyed by something the caller supplied can be subtly out
// of step with the request: a key that is absent when the caller expected a
// zero value, a key present with the wrong contents, an id the caller asked for
// twice, or an id nobody indexed silently answered with a blank record. Every
// one of those is invisible until a thread renders with the wrong author's text
// under the wrong node.
//
// GetVoteStateForComments is different again: it is the only per-viewer state
// in this file. Everything else here answers the same for everybody, so a
// scoping mistake in the other two shows up as missing content. A scoping
// mistake in this one shows one person's votes to another, which is a privacy
// leak that renders as a perfectly plausible UI.
//
// See comment_repo_write_test.go for commentEnv and the seeding helpers.

// commentSeedVote indexes a vote the way the vote consumer would.
//
// Written as raw SQL rather than through NewVoteRepository because what is
// under test is the reading query; going through the writing repository would
// make these assertions fail whenever vote_repo.go broke.
func commentSeedVote(env *commentEnv, voterDID, subjectURI, direction, rkey string, deleted bool) {
	env.t.Helper()

	var deletedAt interface{}
	if deleted {
		deletedAt = time.Now().UTC()
	}
	_, err := env.db.ExecContext(env.ctx, `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, 'bafysubject', $6, $7, $8)
	`,
		"at://"+voterDID+"/social.coves.interaction.vote/"+rkey,
		"bafyvote"+rkey, rkey, voterDID, subjectURI, direction, commentBaseTime, deletedAt)
	require.NoErrorf(env.t, err, "indexing %s's %s vote on %s", voterDID, direction, subjectURI)
}

func TestCommentRepo_GetByURIsBatch(t *testing.T) {
	t.Parallel()

	// An empty request must not reach the database at all: the tree renderer
	// calls this once per depth level and the deepest level is routinely empty.
	// A nil map would panic the caller on the first index; an unfiltered query
	// would return the whole table.
	t.Run("an empty request is an empty map, not nil and not everything", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		env.seed(commentSpec{rkey: "bystander"})

		got, err := env.repo.GetByURIsBatch(env.ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, got, "callers index this map directly; nil would work for reads but not for "+
			"the code paths that write into it")
		assert.Empty(t, got)

		got, err = env.repo.GetByURIsBatch(env.ctx, []string{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("keys the map by the URI that was asked for", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		first := env.seed(commentSpec{rkey: "one", content: "first body"})
		second := env.seed(commentSpec{rkey: "two", content: "second body"})
		env.seed(commentSpec{rkey: "unasked", content: "must not appear"})

		got, err := env.repo.GetByURIsBatch(env.ctx, []string{first, second})
		require.NoError(t, err)
		require.Len(t, got, 2, "the batch returned rows nobody asked for")
		require.Contains(t, got, first)
		require.Contains(t, got, second)
		assert.Equal(t, "first body", got[first].Content,
			"the map is keyed by URI and the caller renders each node from its key; keys and bodies "+
				"that drift apart put one person's words under another person's name")
		assert.Equal(t, "second body", got[second].Content)
	})

	// A URI the AppView has not indexed must be ABSENT. A zero-valued entry
	// would render as a real comment with no author and no text, which is
	// indistinguishable from a deleted one and impossible for the caller to
	// detect.
	t.Run("a URI nobody indexed is absent rather than blank", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		real := env.seed(commentSpec{rkey: "real"})
		phantom := "at://" + env.author + "/social.coves.community.comment/phantom"

		got, err := env.repo.GetByURIsBatch(env.ctx, []string{real, phantom})
		require.NoError(t, err)
		assert.Len(t, got, 1)
		_, present := got[phantom]
		assert.False(t, present, "an unindexed comment must not materialise as an empty one")
	})

	t.Run("a URI asked for twice collapses to one entry", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "repeated"})

		got, err := env.repo.GetByURIsBatch(env.ctx, []string{uri, uri, uri})
		require.NoError(t, err)
		assert.Len(t, got, 1, "the caller collects parent URIs from a tree walk and does not "+
			"deduplicate; ANY($1) plus a map keyed by URI is what makes that safe")
		assert.Equal(t, uri, got[uri].URI)
	})

	// Bridged counts are an origin platform's votes, stored in their own columns
	// so native votes can never clobber them, and folded in at read time. A
	// batch fetch that returned the raw columns would show a bridged comment as
	// having no votes while its score said otherwise.
	t.Run("folds bridged votes into the displayed counts", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "bridged"})
		env.setNativeVotes(uri, 4, 1)
		sampledAt := commentBaseTime.Add(time.Hour)
		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "c", Content: "bridged body",
			BridgedUpvoteCount: 20, BridgedDownvoteCount: 3, BridgedStatsAsOf: &sampledAt,
		}))

		got, err := env.repo.GetByURIsBatch(env.ctx, []string{uri})
		require.NoError(t, err)
		require.Contains(t, got, uri)
		assert.Equal(t, 24, got[uri].UpvoteCount, "4 native + 20 bridged")
		assert.Equal(t, 4, got[uri].DownvoteCount, "1 native + 3 bridged")
		assert.Equal(t, 20, got[uri].Score, "and the stored score already folds both")
	})

	// Deleted comments are returned on purpose: the tree renderer needs the node
	// so its children have somewhere to hang. What makes that safe is that
	// SoftDeleteWithReason has already blanked the text.
	t.Run("returns a deleted comment as a blanked placeholder", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "tombstone", content: "removed words"})
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonModerator, "did:plc:cmtbatchmod"))

		got, err := env.repo.GetByURIsBatch(env.ctx, []string{uri})
		require.NoError(t, err)
		require.Contains(t, got, uri, "dropping the row would orphan every reply below it")
		assert.Empty(t, got[uri].Content, "and the placeholder must carry no text")
		require.NotNil(t, got[uri].DeletionReason)
		assert.Equal(t, comments.DeletionReasonModerator, *got[uri].DeletionReason)
	})
}

// TestCommentRepo_GetByURIsBatchDropsTheAuthorHandle pins a discarded column.
//
// The query joins users and selects COALESCE(u.handle, c.commenter_did) AS
// author_handle, scans it into a local variable, and then never assigns it to
// the comment — unlike GetByRootAndRkey and ListByParentsBatch, which both do
// (comment_repo.go:1015 and :1234). So the batch pays for the join on every
// call and throws the answer away, and any caller that trusts
// Comment.CommenterHandle from this method gets the empty string.
//
// It is not visible in the API today because comment_service.go re-hydrates
// authors from the user repository afterwards, which is itself the extra round
// trip this method exists to avoid.
func TestCommentRepo_GetByURIsBatchDropsTheAuthorHandle(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	uri := env.seed(commentSpec{rkey: "hydrated"})

	viaPermalink, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "hydrated", "")
	require.NoError(t, err)
	require.Equal(t, "cmt-"+env.id+".test", viaPermalink.CommenterHandle,
		"the sibling read hydrates the handle from the same join")

	batch, err := env.repo.GetByURIsBatch(env.ctx, []string{uri})
	require.NoError(t, err)
	require.Contains(t, batch, uri)
	assert.Empty(t, batch[uri].CommenterHandle,
		"GetByURIsBatch should set CommenterHandle to the joined handle, as GetByRootAndRkey and "+
			"ListByParentsBatch do. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin and assert "+
			"the handle instead")
}

func TestCommentRepo_ListByParentsBatch(t *testing.T) {
	t.Parallel()

	t.Run("an empty request is an empty map", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		env.seed(commentSpec{rkey: "bystander"})

		got, err := env.repo.ListByParentsBatch(env.ctx, nil, "new", 10, "")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Empty(t, got, "an empty level of the tree must not fetch every reply in the database")
	})

	// The whole point of the window function: each parent gets its own top-N,
	// not a shared N across the batch.
	t.Run("limits per parent rather than across the batch", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		busy := env.seed(commentSpec{rkey: "busy"})
		quiet := env.seed(commentSpec{rkey: "quiet"})
		for _, reply := range []struct {
			rkey  string
			score int
		}{{"busy1", 30}, {"busy2", 20}, {"busy3", 10}} {
			env.seed(commentSpec{rkey: reply.rkey, parent: busy, score: reply.score})
		}
		quietReply := env.seed(commentSpec{rkey: "quiet1", parent: quiet, score: 5})

		got, err := env.repo.ListByParentsBatch(env.ctx, []string{busy, quiet}, "top", 2, "")
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Len(t, got[busy], 2, "ROW_NUMBER() must partition by parent_uri; a plain LIMIT would "+
			"give the whole batch two replies and starve every parent after the first")
		assert.Equal(t, []string{quietReply}, commentURIs(got[quiet]),
			"and a parent with fewer replies than the limit must still get all of them")
	})

	t.Run("a parent with no replies is absent from the map", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		leaf := env.seed(commentSpec{rkey: "leaf"})

		got, err := env.repo.ListByParentsBatch(env.ctx, []string{leaf, "at://did:plc:nobody/x/y"}, "new", 5, "")
		require.NoError(t, err)
		_, present := got[leaf]
		assert.False(t, present, "a childless parent must be absent rather than mapped to an empty "+
			"slice: the renderer tests presence to decide whether to draw a 'show replies' affordance")
		_, present = got["at://did:plc:nobody/x/y"]
		assert.False(t, present)
	})

	t.Run("keys each reply under its own parent", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		left := env.seed(commentSpec{rkey: "left"})
		right := env.seed(commentSpec{rkey: "right"})
		leftReply := env.seed(commentSpec{rkey: "leftchild", parent: left})
		rightReply := env.seed(commentSpec{rkey: "rightchild", parent: right})

		got, err := env.repo.ListByParentsBatch(env.ctx, []string{left, right}, "new", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{leftReply}, commentURIs(got[left]),
			"grouping is done in Go from the parent_uri column; a mix-up here nests one comment's "+
				"replies under a different comment")
		assert.Equal(t, []string{rightReply}, commentURIs(got[right]))
	})

	// Every sort arm builds its own window ORDER BY, and the hot arm has to
	// inline the rank formula because PostgreSQL will not accept a SELECT alias
	// in a window clause. Three different orderings from the same rows is the
	// only evidence the three arms are actually different code.
	t.Run("each sort arm orders the replies differently", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		parent := env.seed(commentSpec{rkey: "sorted"})
		// Newest has the worst score and oldest the best, so "new" and "top"
		// must disagree, and "hot" — which weights score against age — must
		// agree with "top" here because all three are the same age apart.
		oldBest := env.seed(commentSpec{rkey: "oldbest", parent: parent, score: 100, createdAt: commentBaseTime})
		middle := env.seed(commentSpec{rkey: "middle", parent: parent, score: 50, createdAt: commentBaseTime.Add(time.Hour)})
		newWorst := env.seed(commentSpec{rkey: "newworst", parent: parent, score: 1, createdAt: commentBaseTime.Add(2 * time.Hour)})

		byNew, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "new", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{newWorst, middle, oldBest}, commentURIs(byNew[parent]),
			"'new' must be strictly reverse chronological")

		byTop, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "top", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{oldBest, middle, newWorst}, commentURIs(byTop[parent]),
			"'top' must be strictly by score; an ordering identical to 'new' would mean the sort "+
				"parameter is being ignored")

		byHot, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "hot", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{oldBest, middle, newWorst}, commentURIs(byHot[parent]),
			"the hot rank is log(score)/age^1.8; over a two-hour spread a hundredfold score "+
				"difference dominates, so hot agrees with top here")

		byDefault, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "wildly-unknown", 10, "")
		require.NoError(t, err)
		assert.Equal(t, commentURIs(byHot[parent]), commentURIs(byDefault[parent]),
			"an unrecognised sort falls back to hot rather than to an unordered scan; without the "+
				"default arm the window function would have no ORDER BY and the per-parent top-N "+
				"would be an arbitrary N")
	})

	// Age is what separates hot from top. With equal scores the newer comment
	// must win, which is the arm the score-dominated case above cannot see.
	t.Run("hot prefers the newer of two equally scored replies", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		parent := env.seed(commentSpec{rkey: "tied"})
		older := env.seed(commentSpec{rkey: "older", parent: parent, score: 10, createdAt: commentBaseTime.AddDate(0, 0, -30)})
		newer := env.seed(commentSpec{rkey: "newer", parent: parent, score: 10, createdAt: commentBaseTime})

		got, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "hot", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{newer, older}, commentURIs(got[parent]),
			"the time decay is the entire difference between hot and top; a hot ranking that put a "+
				"month-old comment above an equally scored fresh one is not ranking by hot at all")
	})

	t.Run("hides a blocked author's replies from that viewer only", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		parent := env.seed(commentSpec{rkey: "blockparent"})
		viewer := "did:plc:cmtbbviewer" + env.id
		blocked := "did:plc:cmtbbblocked" + env.id
		createTestUser(t, env.db, "cmtbbv-"+env.id+".test", viewer)
		createTestUser(t, env.db, "cmtbbb-"+env.id+".test", blocked)
		blockedReply := env.seed(commentSpec{rkey: "byblocked", parent: parent, author: blocked})
		otherReply := env.seed(commentSpec{rkey: "byother", parent: parent, createdAt: commentBaseTime.Add(time.Minute)})

		before, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "new", 10, viewer)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{blockedReply, otherReply}, commentURIs(before[parent]),
			"both replies are visible before any block exists")

		insertUserBlock(t, env.db, viewer, blocked)

		after, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "new", 10, viewer)
		require.NoError(t, err)
		assert.Equal(t, []string{otherReply}, commentURIs(after[parent]),
			"the blocked author's reply must leave and the third party's must stay — a filter that "+
				"dropped everything would satisfy half of this")

		anonymous, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "new", 10, "")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{blockedReply, otherReply}, commentURIs(anonymous[parent]),
			"one viewer's block must not remove the reply for everybody")
	})

	t.Run("hydrates the author handle and falls back to the DID", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		parent := env.seed(commentSpec{rkey: "handles"})
		stranger := "did:plc:cmtbbstranger" + env.id
		known := env.seed(commentSpec{rkey: "known", parent: parent})
		unknown := env.seed(commentSpec{rkey: "unknown", parent: parent, author: stranger,
			createdAt: commentBaseTime.Add(time.Minute)})

		got, err := env.repo.ListByParentsBatch(env.ctx, []string{parent}, "new", 10, "")
		require.NoError(t, err)
		byURI := map[string]*comments.Comment{}
		for _, reply := range got[parent] {
			byURI[reply.URI] = reply
		}
		require.Contains(t, byURI, known)
		require.Contains(t, byURI, unknown)
		assert.Equal(t, "cmt-"+env.id+".test", byURI[known].CommenterHandle)
		assert.Equal(t, stranger, byURI[unknown].CommenterHandle,
			"a comment whose author the firehose has not delivered yet must still render; the LEFT "+
				"JOIN with COALESCE is what stops an INNER JOIN from silently dropping it")
	})
}

func TestCommentRepo_GetVoteStateForComments(t *testing.T) {
	t.Parallel()

	t.Run("returns only the asking viewer's own votes", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		first := env.seed(commentSpec{rkey: "v1"})
		second := env.seed(commentSpec{rkey: "v2"})
		third := env.seed(commentSpec{rkey: "v3"})
		viewer := "did:plc:cmtvoter" + env.id
		other := "did:plc:cmtother" + env.id

		commentSeedVote(env, viewer, first, "up", "va", false)
		commentSeedVote(env, other, second, "down", "vb", false)

		state, err := env.repo.GetVoteStateForComments(env.ctx, viewer, []string{first, second, third})
		require.NoError(t, err)
		require.Len(t, state, 1, "another user's vote appeared in this viewer's state: the arrows in "+
			"the UI would show a stranger's opinion as the reader's own, and a second tap would try "+
			"to undo a vote the reader never cast")
		require.Contains(t, state, first)
		assert.NotContains(t, state, second)
		assert.NotContains(t, state, third, "a comment the viewer has not voted on must be absent, "+
			"not present with an empty direction")

		vote, ok := state[first].(map[string]interface{})
		require.True(t, ok, "the map value is what the view model reads the direction out of")
		assert.Equal(t, "up", vote["direction"],
			"the direction decides which arrow lights up; up rendered as down is a vote the reader "+
				"cannot see they cast")
		assert.Equal(t, "at://"+viewer+"/social.coves.interaction.vote/va", vote["uri"],
			"the vote's own URI is what the client deletes to retract; without it the arrow is "+
				"lit and unclickable")
	})

	// An anonymous reader has no votes, and the query is skipped entirely rather
	// than run with an empty voter_did — which would match nothing today but
	// would match every vote from a caller that passed a wildcard.
	t.Run("an anonymous viewer gets no state at all", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "anon"})
		commentSeedVote(env, "did:plc:cmtsomeone"+env.id, uri, "up", "vc", false)

		state, err := env.repo.GetVoteStateForComments(env.ctx, "", []string{uri})
		require.NoError(t, err)
		require.NotNil(t, state, "the caller indexes this map without checking it")
		assert.Empty(t, state, "an empty viewer DID must mean 'nobody', never 'everybody'")
	})

	t.Run("an empty subject list is an empty map", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		viewer := "did:plc:cmtempty" + env.id
		uri := env.seed(commentSpec{rkey: "unasked"})
		commentSeedVote(env, viewer, uri, "up", "vd", false)

		state, err := env.repo.GetVoteStateForComments(env.ctx, viewer, nil)
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.Empty(t, state, "asking about no comments must not return every vote the viewer "+
			"has ever cast")
	})

	// A retracted vote is a soft delete, and the partial unique index lets a
	// user vote again on the same subject afterwards. Serving the tombstone
	// would light the arrow for a vote that no longer exists, and re-tapping it
	// would try to delete an already-deleted record.
	t.Run("ignores a retracted vote", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "retracted"})
		viewer := "did:plc:cmtretract" + env.id
		commentSeedVote(env, viewer, uri, "up", "ve", true)

		state, err := env.repo.GetVoteStateForComments(env.ctx, viewer, []string{uri})
		require.NoError(t, err)
		assert.Empty(t, state, "a retracted vote must not keep the arrow lit")

		commentSeedVote(env, viewer, uri, "down", "vf", false)
		state, err = env.repo.GetVoteStateForComments(env.ctx, viewer, []string{uri})
		require.NoError(t, err)
		require.Contains(t, state, uri, "and the replacement vote must be the one that shows")
		assert.Equal(t, "down", state[uri].(map[string]interface{})["direction"])
	})

	t.Run("scopes to the comments asked about", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		asked := env.seed(commentSpec{rkey: "asked"})
		unasked := env.seed(commentSpec{rkey: "notasked"})
		viewer := "did:plc:cmtscope" + env.id
		commentSeedVote(env, viewer, asked, "up", "vg", false)
		commentSeedVote(env, viewer, unasked, "down", "vh", false)

		state, err := env.repo.GetVoteStateForComments(env.ctx, viewer, []string{asked})
		require.NoError(t, err)
		assert.Len(t, state, 1, "the ANY($2) filter is what keeps a one-comment page from paying "+
			"for the viewer's entire voting history")
		assert.Contains(t, state, asked)
	})

	// Votes carry no foreign key to comments (migration 014 removed the one to
	// users for the same reason), so a vote can be indexed before its subject.
	// The lookup is by URI string and must survive that.
	t.Run("survives a vote whose subject is not a comment this AppView holds", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		viewer := "did:plc:cmtorphan" + env.id
		orphan := "at://" + env.author + "/social.coves.community.comment/notindexedyet"
		commentSeedVote(env, viewer, orphan, "up", "vi", false)

		state, err := env.repo.GetVoteStateForComments(env.ctx, viewer, []string{orphan})
		require.NoError(t, err, "the query joins nothing, so an out-of-order vote must not fail it")
		assert.Contains(t, state, orphan,
			"and the state must still be reported: the vote is real even if the comment row is late")
	})
}
