//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentRepo_HotSortSurvivesFutureDatedComment(t *testing.T) {
	t.Parallel()

	seedThread := func(t *testing.T) (*commentEnv, []string) {
		t.Helper()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		all := []string{
			env.seed(commentSpec{rkey: "high", score: 50, createdAt: now.Add(-time.Minute)}),
			env.seed(commentSpec{rkey: "medium", score: 30, createdAt: now.Add(-2 * time.Minute)}),
			env.seed(commentSpec{rkey: "low", score: 10, createdAt: now.Add(-3 * time.Minute)}),
			env.seed(commentSpec{rkey: "future", score: 1, createdAt: now.Add(3 * time.Hour)}),
		}
		return env, all
	}

	t.Run("returns every comment", func(t *testing.T) {
		t.Parallel()
		env, all := seedThread(t)

		listed, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 10, nil, "")
		require.NoError(t, err,
			"the thread listing must not error because of one future-dated row")
		assert.ElementsMatch(t, all, commentURIs(listed))
	})

	t.Run("paginates past the first page", func(t *testing.T) {
		t.Parallel()
		env, all := seedThread(t)

		first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, nil, "")
		require.NoError(t, err,
			"the first thread page must not error because of one future-dated row")
		require.NotNil(t, cursor, "four comments with a limit of two must produce a second-page cursor")

		second, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, cursor, "")
		require.NoError(t, err,
			"the second thread page must not error because of one future-dated row")

		firstURIs := commentURIs(first)
		secondURIs := commentURIs(second)
		for _, uri := range secondURIs {
			assert.NotContains(t, firstURIs, uri, "the same comment must not appear on both hot-sort pages")
		}
		visited := append(append([]string{}, firstURIs...), secondURIs...)
		assert.ElementsMatch(t, all, visited,
			"the first two pages must return every comment exactly once")
	})
}

func TestCommentRepo_BatchRepliesSurviveFutureDatedComment(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	ordinaryHigh := env.seed(commentSpec{rkey: "batchhigh", score: 30, createdAt: now.Add(-time.Minute)})
	ordinaryLow := env.seed(commentSpec{rkey: "batchlow", score: 10, createdAt: now.Add(-2 * time.Minute)})
	future := env.seed(commentSpec{rkey: "batchfuture", score: 1, createdAt: now.Add(3 * time.Hour)})

	byParent, err := env.repo.ListByParentsBatch(env.ctx, []string{env.root}, "hot", 10, "")
	require.NoError(t, err,
		"the nested-reply batch path must not error because of one future-dated row")
	assert.ElementsMatch(t, []string{ordinaryHigh, ordinaryLow, future}, commentURIs(byParent[env.root]))
}

func TestCommentRepo_FutureDatedCommentRanksAsBrandNew(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	fresh := env.seed(commentSpec{rkey: "fresh", score: 5, createdAt: now.Add(-time.Minute)})
	future := env.seed(commentSpec{rkey: "future", score: 1, createdAt: now.Add(90 * time.Minute)})

	listed, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 10, nil, "")
	require.NoError(t, err)
	assert.Equal(t, []string{fresh, future}, commentURIs(listed),
		"a future-dated comment must rank as if created now, so the higher-scored fresh comment must win")
}
