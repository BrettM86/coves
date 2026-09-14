package discover

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDiscoverHotNormalization(t *testing.T) {
	t.Parallel()

	repeatedScores := func(score, count int) []int {
		scores := make([]int, count)
		for i := range scores {
			scores[i] = score
		}
		return scores
	}

	t.Run("reference is community-balanced", func(t *testing.T) {
		fivePerCommunity := map[string][]int{
			"low":        repeatedScores(3, 5),
			"middle":     repeatedScores(15, 5),
			"high":       repeatedScores(255, 5),
			"too-sparse": repeatedScores(1_000_000, 4),
		}
		withProlificHighCommunity := map[string][]int{
			"low":        repeatedScores(3, 5),
			"middle":     repeatedScores(15, 5),
			"high":       repeatedScores(255, 100),
			"too-sparse": repeatedScores(1_000_000, 4),
		}

		reference, available := communityReference(fivePerCommunity)
		prolificReference, prolificAvailable := communityReference(withProlificHighCommunity)

		assert.True(t, available)
		assert.True(t, prolificAvailable)
		assert.InDelta(t, math.Log(16), reference, 1e-12,
			"G should be the median community mean, excluding cohorts smaller than five")
		assert.InDelta(t, reference, prolificReference, 1e-12,
			"duplicating one community's observations must not give it more weight in G")
	})

	t.Run("adjustment is bounded smoothed and neutral when history cannot justify it", func(t *testing.T) {
		reference := math.Log(4)
		highHistory := repeatedScores(1_000_000, 5)
		highBaseline := (5*math.Log(1_000_001) + 20*reference) / 25
		expectedHighAdjustment := math.Sqrt(reference / highBaseline)

		highAdjustment := communityAdjustment(highHistory, reference, true)
		assert.InDelta(t, expectedHighAdjustment, highAdjustment, 1e-12)
		assert.GreaterOrEqual(t, highAdjustment, 0.5)
		assert.LessOrEqual(t, highAdjustment, 1.0)
		assert.Equal(t, 0.5, communityAdjustment(repeatedScores(1_000_000, 100), reference, true),
			"extreme established baselines should stop at the adjustment floor")

		sparseHistory := repeatedScores(1_000_000, 1)
		sparseBaseline := (math.Log(1_000_001) + 20*reference) / 21
		expectedSparseAdjustment := math.Sqrt(reference / sparseBaseline)
		sparseAdjustment := communityAdjustment(sparseHistory, reference, true)
		assert.InDelta(t, expectedSparseAdjustment, sparseAdjustment, 1e-12)
		assert.Greater(t, sparseAdjustment, highAdjustment,
			"the 20-observation prior should shrink a one-post baseline toward G")

		assert.Equal(t, 1.0, communityAdjustment(nil, reference, true), "empty community history should be neutral")
		assert.Equal(t, 1.0, communityAdjustment(repeatedScores(0, 20), reference, true), "all-zero history should be neutral")

		noReference, available := communityReference(map[string][]int{"sparse": repeatedScores(1_000_000, 4)})
		assert.False(t, available)
		assert.Equal(t, 1.0, communityAdjustment(highHistory, noReference, available),
			"without a qualifying community reference all normalization should be neutral")
	})

	t.Run("base rank preserves score and freshness behavior", func(t *testing.T) {
		rankingTime := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
		twoHoursOld := rankingTime.Add(-2 * time.Hour)
		denominator := math.Pow(4, 1.5)

		positive := discoverHotRank(15, twoHoursOld, rankingTime, 0.5)
		zero := discoverHotRank(0, twoHoursOld, rankingTime, 0.5)
		negative := discoverHotRank(-3, twoHoursOld, rankingTime, 0.5)

		assert.InDelta(t, (1+0.5*math.Log(16))/denominator, positive, 1e-12)
		assert.InDelta(t, 1/denominator, zero, 1e-12)
		assert.InDelta(t, (1-math.Log(4))/denominator, negative, 1e-12)
		assert.Greater(t, discoverHotRank(255, twoHoursOld, rankingTime, 0.5), positive)
		assert.Greater(t, positive, zero)
		assert.Negative(t, negative)
		assert.Less(t, discoverHotRank(-15, twoHoursOld, rankingTime, 0.5), negative)
		assert.Equal(t, zero, discoverHotRank(0, twoHoursOld, rankingTime, 1),
			"normalization must not change zero-score rank")
		assert.Equal(t, negative, discoverHotRank(-3, twoHoursOld, rankingTime, 1),
			"normalization must not change negative-score rank")

		fresh := discoverHotRank(0, rankingTime, rankingTime, 1)
		future := discoverHotRank(0, rankingTime.Add(48*time.Hour), rankingTime, 1)
		assert.InDelta(t, 1/math.Pow(2, 1.5), fresh, 1e-12)
		assert.Equal(t, fresh, future, "future dates should clamp to zero age without gaining a boost")
	})
}
