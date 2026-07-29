//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// voteCollection is the vote record type. Spelled here because the journey is
// the first thing in this package to write one; task 14's vote contract owns
// the collection and should reuse this constant rather than add a second.
const voteCollection = "social.coves.feed.vote"

// voteRecord builds a social.coves.feed.vote record in the shape
// internal/core/votes writes it. The vote lives in the VOTER's repo and names
// its target by strong reference, so the same shape votes on a post and on a
// comment.
func voteRecord(subject strongRef, direction string) map[string]any {
	return map[string]any{
		"$type":     voteCollection,
		"subject":   map[string]any{"uri": subject.URI, "cid": subject.CID},
		"direction": direction,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// TestUserJourney is the one cross-domain saga: does the product hold together.
//
// docs/TEST_ARCHITECTURE.md §3.4 rule 5 keeps exactly one of these, and rule 4
// is why it is allowed to be a sequence — eventual consistency IS the thing
// under test, so each step legitimately builds on the last. It is deliberately
// ONE test function rather than a suite: split into independent tests, each
// would have to re-create the whole world, and the composition — the only thing
// this file proves that the contracts do not — would be gone.
//
// # WHAT IT IS FOR, AND WHAT IT IS NOT FOR
//
// Every individual behaviour below is proven somewhere better: the ingestion
// contracts prove each collection reaches the index, the API contracts prove
// each serving endpoint, and T1 proves the consumers' edge cases. This test
// exists for the seam none of them can see — that a post, a comment on that
// post, and a vote on that post, written by DIFFERENT actors into THREE
// DIFFERENT repos, compose into one coherent thing when read back through
// endpoints a client actually calls.
//
// So it asserts joins and counts, not field transport. When it fails alongside
// a contract, the contract is the interesting failure. When it fails ALONE,
// something about the composition is wrong: a count owned by one domain's
// consumer and read through another domain's endpoint, a join that drops a row,
// an actor reference that resolves in one view and not another.
//
// # IT REPLACES A TEST THAT PROMISED MORE THAN IT DID
//
// The predecessor (tests/integration/user_journey_e2e_test.go, deleted with
// this commit) advertised "writes → PDS → Jetstream → AppView" across 11 steps.
// It did that for two of them. The comment, both votes and the subscription
// were hand-built jetstream.JetstreamEvent literals passed straight to
// in-process consumers; the users were direct SQL INSERTs; the block was a raw
// INSERT INTO user_blocks; and every HTTP call went to an httptest router
// wired up inside the test binary rather than to the shipped AppView. Even the
// two real steps could be switched off with ALLOW_SIMULATION_FALLBACK=true, at
// which point one of them became a direct SQL insert with a "fakecid".
//
// It was also the last big consumer of the hand-rolled websocket subscribers,
// and it was starving on the shared stream: the events it waited for competed
// with unfiltered account/identity traffic from every test running beside it
// (the note-[C] hazard). Every write below is a real record in a real repo,
// observed only through the running AppView's endpoints, so there is no
// subscriber left to starve.
//
// # THE ONE STEP THAT MOVED, AND WHY
//
// The old step 9 read social.coves.feed.getTimeline — User B's PERSONALISED
// feed, which is why it needed the subscription in step 5. That endpoint is
// behind RequireAuth, and §3.4b's standing limitation is that nothing outside
// the browser OAuth callback mints a credential RequireAuth accepts, so T2
// cannot call it at all.
//
// The substitution is social.coves.communityFeed.getCommunity: the same
// indexed post, the same hydrated stats, read through the public feed the
// community page is built from. What is lost is specifically the subscription
// fan-out (does a post reach a subscriber's timeline), which is
// community.subscription's own concern — task 14 owns that collection's
// ingestion contract, and the personalised feed becomes reachable here when the
// Phase-5 test-only session mint lands. Stated plainly rather than quietly
// dropped, because "the journey covers the timeline" would otherwise stay true
// in everyone's memory and false in the code.
func TestUserJourney(t *testing.T) {
	p := newPipeline(t)
	ctx := context.Background()

	// ---- the cast -------------------------------------------------------------
	// Two humans and a community. Both accounts go through signup, which is a
	// SYNCHRONOUS path and proves nothing about the pipeline (§3.4) — its job
	// here is to put the identities in the index so the joins below have
	// something to resolve, and to hand back PDS sessions for the direct writes
	// that DO prove it.
	author := p.IndexedAccount(t, "ja")
	reader := p.IndexedAccount(t, "jb")
	community := indexedCommunity(t, p, "j", author.DID)

	t.Logf("journey cast: author=%s reader=%s community=%s", author.DID, reader.DID, community.DID)

	// ---- 1. the author posts --------------------------------------------------
	// Into the COMMUNITY's repo, with the community's session, because that is
	// where post records live (post_contract_test.go opens with why).
	post := indexedPost(t, p, community, author.DID, "journey "+testkit.UniqueID(t))

	view, err := p.Post(ctx, post.URI)
	require.NoError(t, err)
	require.Equal(t, author.DID, view.Author.DID)
	require.Equal(t, community.DID, view.Community.DID)
	require.Equal(t, postStats{}, view.Stats, "a fresh post starts with no votes and no comments")

	// ---- 2. the reader comments on it -----------------------------------------
	// Into the READER's own repo — a different repo from the post's, which is
	// the first of the three this saga spans.
	commentRKey := testkit.TID()
	commentURIStr := commentURI(reader.DID, commentRKey)
	commentRec := reader.PutRecord(t, commentCollection, commentRKey,
		commentRecord(post, post, "a reader's comment on somebody else's post"))

	var commented threadNode
	p.Await(t, "the reader's comment to appear in the author's post thread", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		node, ok := thread.find(commentURIStr)
		if !ok {
			return false, nil
		}
		commented = node
		return true, nil
	}, withReadCadence())

	require.Equal(t, reader.DID, commented.Comment.Author.DID,
		"the comment's author is the reader, resolved through a join to a users row that only "+
			"exists because the reader signed up")
	require.Equal(t, reader.Handle, commented.Comment.Author.Handle)
	require.Equal(t, post.URI, commented.Comment.Post.URI)

	// The count crossing a domain boundary: the COMMENT consumer wrote it, the
	// POST endpoint serves it. Nothing but composition tests this.
	p.Await(t, "the post's commentCount to reflect the reader's comment", func() (bool, error) {
		v, err := p.Post(context.Background(), post.URI)
		if err != nil {
			return false, err
		}
		return v.Stats.CommentCount == 1, nil
	})

	// ---- 3. the reader upvotes the post ---------------------------------------
	// A third repo: the vote lives in the VOTER's. Direction "up" on a strong
	// reference to the post.
	postVoteRKey := testkit.TID()
	reader.PutRecord(t, voteCollection, postVoteRKey, voteRecord(post, "up"))

	p.Await(t, "the reader's upvote to reach the post's stats", func() (bool, error) {
		v, err := p.Post(context.Background(), post.URI)
		if err != nil {
			return false, err
		}
		return v.Stats.Upvotes == 1, nil
	})

	view, err = p.Post(ctx, post.URI)
	require.NoError(t, err)
	require.Equal(t, 1, view.Stats.Upvotes)
	require.Equal(t, 0, view.Stats.Downvotes)
	require.Equal(t, 1, view.Stats.Score, "score is upvotes minus downvotes, computed by the vote consumer")
	// A FORWARD GUARD, not a live proof, and worth saying which: two consumers
	// write this posts row — the comment consumer owns comment_count, the vote
	// consumer owns upvote_count/downvote_count/score — but both do so with
	// column-scoped UPDATEs today, so neither can currently clobber the other's
	// column and this assertion cannot fail for that reason. What it is here to
	// catch is the change that makes it possible: a consumer that starts writing
	// the stats wholesale, or a shared recompute path that derives one counter
	// while resetting another. This is the only place two domains' counters are
	// read back from one row, so it is the only place that regression surfaces.
	require.Equal(t, 1, view.Stats.CommentCount,
		"the comment count did not survive the vote: something now writes this post's stats "+
			"across column boundaries")

	// ---- 4. the author upvotes the reader's comment ---------------------------
	// The reciprocal direction, and a vote whose subject is a COMMENT rather
	// than a post: the same collection, a different target table, read back
	// through a third domain's endpoint.
	commentVoteRKey := testkit.TID()
	author.PutRecord(t, voteCollection, commentVoteRKey,
		voteRecord(strongRef{URI: commentURIStr, CID: commentRec.CID}, "up"))

	p.Await(t, "the author's upvote to reach the comment's stats in the thread", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		node, ok := thread.find(commentURIStr)
		if !ok {
			return false, nil
		}
		return node.Comment.Stats.Upvotes == 1, nil
	}, withReadCadence())

	thread, err := p.Thread(ctx, post.URI, nil)
	require.NoError(t, err)
	voted, ok := thread.find(commentURIStr)
	require.Truef(t, ok, "the comment left the thread after being voted on: %s", thread.uris())
	require.Equal(t, 1, voted.Comment.Stats.Upvotes)
	require.Equal(t, 1, voted.Comment.Stats.Score)
	require.Equal(t, "a reader's comment on somebody else's post", voted.Comment.Record["content"],
		"a vote must not disturb the comment's content")

	// The thread hydrates its post, so this is the same composed state read
	// through a second endpoint — a count that agreed on post.get and
	// disagreed here would be a hydration bug invisible to either contract.
	require.Equal(t, 1, thread.Post.Stats.CommentCount)
	require.Equal(t, 1, thread.Post.Stats.Upvotes)

	// ---- 5. it all shows up on the public read surfaces -----------------------
	// The payoff: everything above, assembled, through the two feeds a client
	// renders. Each is a different query over different joins, so a post can be
	// correct on post.get and missing or unhydrated here.
	t.Run("the post carries its comment and its vote into the author's feed", func(t *testing.T) {
		var feed struct {
			Feed []struct {
				Post postView `json:"post"`
			} `json:"feed"`
		}
		require.NoError(t, p.AppView.Query(ctx, "social.coves.actor.getPosts",
			url.Values{"actor": {author.DID}, "limit": {"25"}}, &feed))

		found, seen := false, make([]string, 0, len(feed.Feed))
		for _, item := range feed.Feed {
			seen = append(seen, item.Post.URI)
			if item.Post.URI == post.URI {
				found = true
				require.Equal(t, 1, item.Post.Stats.CommentCount)
				require.Equal(t, 1, item.Post.Stats.Upvotes)
				require.Equal(t, community.DID, item.Post.Community.DID)
			}
		}
		require.Truef(t, found, "the journey's post was not in the author's %d posts: %s",
			len(feed.Feed), strings.Join(seen, ", "))
	})

	t.Run("the post carries its comment and its vote into the community feed", func(t *testing.T) {
		// Public, OptionalAuth — the community page's own read, and the honest
		// stand-in for the personalised timeline the old journey used (see the
		// doc comment). sort=new so the assertion does not depend on how many
		// posts a kept stack has accumulated.
		var feed struct {
			Feed []struct {
				Post postView `json:"post"`
			} `json:"feed"`
		}
		require.NoError(t, p.AppView.Query(ctx, "social.coves.communityFeed.getCommunity",
			url.Values{"community": {community.DID}, "sort": {"new"}, "limit": {"25"}}, &feed))

		found, seen := false, make([]string, 0, len(feed.Feed))
		for _, item := range feed.Feed {
			seen = append(seen, item.Post.URI)
			if item.Post.URI == post.URI {
				found = true
				require.Equal(t, author.DID, item.Post.Author.DID)
				require.Equal(t, author.Handle, item.Post.Author.Handle)
				require.Equal(t, 1, item.Post.Stats.CommentCount)
				require.Equal(t, 1, item.Post.Stats.Upvotes)
				require.Equal(t, 1, item.Post.Stats.Score)
			}
		}
		require.Truef(t, found, "the journey's post was not in community %s's %d posts: %s",
			community.DID, len(feed.Feed), strings.Join(seen, ", "))
	})
}
