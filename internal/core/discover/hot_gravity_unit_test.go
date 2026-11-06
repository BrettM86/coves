package discover

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The Hot age decay is (ageHours + 2)^0.8. Every expected value below was
// computed by hand (python3, outside the code under test) and is written as a
// literal. The tolerance is relative, not bitwise: math.Pow differs in the last
// digit between architectures.
const gravityRelativeTolerance = 1e-12

func TestDiscoverHotRankUsesGravityPointEight(t *testing.T) {
	t.Parallel()

	rankingTime := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		score      int
		age        time.Duration
		adjustment float64
		bonus      float64
		want       float64
	}{
		// 1 / 2^0.8
		{"zero score at age zero", 0, 0, 1, 0, 0.5743491774985175},
		// 1 / 12^0.8
		{"zero score at ten hours", 0, 10 * time.Hour, 1, 0, 0.13697931912643546},
		// (1 + 0.5*ln 16 + 1.25) / 12^0.8
		{"positive score with adjustment and hosted bonus at ten hours", 15, 10 * time.Hour, 0.5, 1.25, 0.4980971257294993},
		// (1 - ln 4) / 12^0.8; the bonus is ignored for a negative score
		{"negative score at ten hours", -3, 10 * time.Hour, 1, 2, -0.052914338568584},
	}

	for _, testCase := range cases {
		got := DiscoverHotRank(testCase.score, rankingTime.Add(-testCase.age), rankingTime, testCase.adjustment, testCase.bonus)
		assert.InEpsilonf(t, testCase.want, got, gravityRelativeTolerance,
			"%s: rank must decay as (ageHours + 2)^0.8", testCase.name)
	}
}

// The user-visible effect of the gentler decay: a well-voted post a few hours
// old now beats a brand-new post nobody has voted on. Under the old 1.5
// exponent the order was the reverse (0.3462 against 0.3536).
func TestDiscoverHotRankPopularFewHourOldPostOutranksFreshUnvotedPost(t *testing.T) {
	t.Parallel()

	rankingTime := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	popular := DiscoverHotRank(450, rankingTime.Add(-5*time.Hour-30*time.Minute), rankingTime, 1, 0)
	fresh := DiscoverHotRank(0, rankingTime, rankingTime, 1, 0)

	// (1 + ln 451) / 7.5^0.8 and 1 / 2^0.8
	assert.InEpsilon(t, 1.4187641601957917, popular, gravityRelativeTolerance)
	assert.InEpsilon(t, 0.5743491774985175, fresh, gravityRelativeTolerance)
	assert.Greater(t, popular, fresh,
		"a 450-score post at 5.5h must outrank a 0-score post at 0h")
}
