//go:build integration

package postgres

import (
	"context"
	"math"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverHotRank_NeutralMatchesLegacyPostgresExpression(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	rankingTime := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	cases := []struct {
		name      string
		score     int
		createdAt time.Time
	}{
		{name: "positive brand new", score: 37, createdAt: rankingTime},
		{name: "positive sub-hour", score: 9, createdAt: rankingTime.Add(-37 * time.Minute)},
		{name: "zero at one day", score: 0, createdAt: rankingTime.Add(-24 * time.Hour)},
		{name: "negative at six hours", score: -4, createdAt: rankingTime.Add(-6 * time.Hour)},
		{name: "positive at thirteen days", score: 1_000, createdAt: rankingTime.Add(-13 * 24 * time.Hour)},
		{name: "future timestamp is clamped", score: 18, createdAt: rankingTime.Add(4 * time.Hour)},
	}

	query := `SELECT ` + hotRankSQL("p", "$3::timestamptz") + `
		FROM (VALUES ($1::integer, $2::timestamptz)) AS p(score, created_at)`
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var postgresRank float64
			err := db.QueryRowContext(context.Background(), query,
				testCase.score, testCase.createdAt, rankingTime).Scan(&postgresRank)
			require.NoError(t, err)

			goRank := discover.DiscoverHotRank(testCase.score, testCase.createdAt, rankingTime, 1, 0)
			absoluteError := math.Abs(goRank - postgresRank)
			scale := math.Max(math.Abs(goRank), math.Abs(postgresRank))
			relativeError := absoluteError
			if scale > 0 {
				relativeError /= scale
			}
			assert.Truef(t, absoluteError <= 1e-12 || relativeError <= 1e-12,
				"neutral Go rank %0.17g differs from PostgreSQL hotRankSQL %0.17g (absolute error %g, relative error %g)",
				goRank, postgresRank, absoluteError, relativeError)
		})
	}
}
