//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/feedgen"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommentsHotGenerator_CursorDriftMatchesOracle pins the comments-hot
// generator's paging under vote and reply drift to today's query.
//
// A hot walk pins its rank clock (rankedAt) but reads scores live, so votes
// cast between pages can skip a reply or serve one twice. That behaviour is
// kept on purpose (pagination stable under vote changes was not approved).
// The generator must reproduce it exactly: same page one, and after the same
// mutations, the same page two from its own cursor.
//
// One database serves both paths. Page one is read from both, the mutations
// are applied once, then page two is read from both.
//
// Fixture (hot rank = log10(max(2, score+2)) / (age_hours+2)^1.8):
//
//	leader   score 100, 1h  0.278   page one
//	blocked  score  80, 1h  0.265   hidden: the viewer blocks its author
//	boundary score  50, 2h  0.142   page one, the cursor's reply
//	third    score  40, 3h  0.090
//	lifted   score   5, 4h  0.034
//	fifth    score   2, 5h  0.018
//	sixth    score   0, 8h  0.005
//
// Mutations before page two:
//
//	fresh    new reply, score 0, 30m  0.058  lands after third
//	lifted   upvoted to 10000          0.159  above the boundary: skipped
//	leader   downvoted to -1000        0.042  below the boundary: served again
//
// The downvote targets leader, an emitted reply that is not the boundary: the
// cursor excludes only the boundary's own URI, so leader comes back.
func TestCommentsHotGenerator_CursorDriftMatchesOracle(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	cast := seedBlockFilterCast(t, env.db, "drift")
	insertUserBlock(t, env.db, cast.viewer, cast.blocked)
	now := time.Now().UTC()

	names := map[string]string{}
	seed := func(name string, spec commentSpec) string {
		spec.rkey = name
		uri := env.seed(spec)
		names[uri] = name
		return uri
	}
	nameOf := func(uris []string) []string {
		out := make([]string, 0, len(uris))
		for _, uri := range uris {
			if name, ok := names[uri]; ok {
				out = append(out, name)
			} else {
				out = append(out, uri)
			}
		}
		return out
	}

	leader := seed("leader", commentSpec{score: 100, createdAt: now.Add(-1 * time.Hour)})
	seed("blocked", commentSpec{author: cast.blocked, score: 80, createdAt: now.Add(-1 * time.Hour)})
	boundary := seed("boundary", commentSpec{score: 50, createdAt: now.Add(-2 * time.Hour)})
	seed("third", commentSpec{author: cast.thirdParty, score: 40, createdAt: now.Add(-3 * time.Hour)})
	lifted := seed("lifted", commentSpec{score: 5, createdAt: now.Add(-4 * time.Hour)})
	seed("fifth", commentSpec{author: cast.thirdParty, score: 2, createdAt: now.Add(-5 * time.Hour)})
	seed("sixth", commentSpec{createdAt: now.Add(-8 * time.Hour)})

	generator := NewCommentsHotGenerator(env.db)

	// Page one, both paths.
	oraclePage1, oracleCursor, err := oracleListByParentWithHotRank(env.ctx, env.impl, env.root, "hot", "", 2, nil, cast.viewer)
	require.NoError(t, err)
	require.NotNil(t, oracleCursor, "THE FIXTURE IS WRONG, not the code: six visible replies two to a page have a page two")
	require.Equal(t, []string{"leader", "boundary"}, nameOf(commentURIs(oraclePage1)),
		"THE FIXTURE IS WRONG, not the code: the oracle's page one must be leader then boundary, with the blocked reply hidden")

	genPage1, err := generator.CommentSkeleton(env.ctx, feedgen.CommentSkeletonRequest{
		ViewerDID:      cast.viewer,
		Parents:        []string{env.root},
		LimitPerParent: 2,
	})
	require.NoError(t, err)
	require.Len(t, genPage1.Replies, 1, "the generator must return one CommentReplies for the one requested parent")
	require.Equal(t, env.root, genPage1.Replies[0].Parent)
	require.Equal(t, []string{"leader", "boundary"}, nameOf(genPage1.Replies[0].URIs),
		"comments-hot page one must equal the oracle's: leader then boundary, the blocked author's reply hidden")
	require.NotEmpty(t, genPage1.Replies[0].Cursor, "comments-hot page one of six visible replies must carry a cursor")

	// Mutations, applied once.
	seed("fresh", commentSpec{createdAt: time.Now().UTC().Add(-30 * time.Minute)})
	env.setNativeVotes(lifted, 10000, 0)
	env.setNativeVotes(leader, 0, 1000)

	at := time.Now().UTC()
	boundaryRank := commentHotRankAt(t, env, boundary, at)
	require.Greater(t, commentHotRankAt(t, env, lifted, at), boundaryRank,
		"THE FIXTURE IS WRONG, not the code: the upvoted reply must now rank above the boundary, or nothing is skipped")
	require.Less(t, commentHotRankAt(t, env, leader, at), boundaryRank,
		"THE FIXTURE IS WRONG, not the code: the downvoted leader must now rank below the boundary, or nothing repeats")

	// Page two, each path from its own cursor.
	wantPage2 := []string{"third", "fresh", "leader", "fifth", "sixth"}

	oraclePage2, _, err := oracleListByParentWithHotRank(env.ctx, env.impl, env.root, "hot", "", 10, oracleCursor, cast.viewer)
	require.NoError(t, err)
	require.Equal(t, wantPage2, nameOf(commentURIs(oraclePage2)),
		"THE FIXTURE IS WRONG, not the code: the oracle's page two must skip lifted and serve leader again")

	genPage2, err := generator.CommentSkeleton(env.ctx, feedgen.CommentSkeletonRequest{
		ViewerDID:      cast.viewer,
		Parents:        []string{env.root},
		LimitPerParent: 10,
		Cursor:         genPage1.Replies[0].Cursor,
	})
	require.NoError(t, err)
	require.Len(t, genPage2.Replies, 1)
	assert.Equal(t, wantPage2, nameOf(genPage2.Replies[0].URIs),
		"comments-hot page two must equal the oracle's: lifted skipped (upvoted above the pinned boundary), "+
			"leader served again (downvoted below it), fresh in rank order, blocked still hidden")
	assert.Empty(t, genPage2.Replies[0].Cursor, "page two holds the rest of the thread, so no further cursor")
}
