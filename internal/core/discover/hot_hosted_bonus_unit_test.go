package discover

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The secrets these vectors were computed under. They stand for the
// repository's cursor secret, which is the key the bonus is derived with.
const (
	firstHostedBonusSecret  = "discover-cursor-secret-one"
	secondHostedBonusSecret = "discover-cursor-secret-two"
)

// A post URI is at://<author did>/social.coves.community.postv2/<rkey>, and the
// rkey is chosen by the author. That is the whole reason the bonus is keyed: an
// unkeyed digest of a string the author controls end to end can be ground
// offline until it hashes near the 3.0 ceiling, and every post that author
// writes then enters Discover at the top.
//
// The expected values were computed outside Go, from the formula rather than
// from this package's output:
//
//	python3 -c "import hmac,hashlib,struct; mac=hmac.new(secret.encode(), b'discover-hot-hosted-bonus\x00'+uri.encode(), hashlib.sha256).digest(); n=struct.unpack('>Q',mac[:8])[0]; print(3.0*(n>>11)/float(2**53))"
//
// Pinning them as literals is what makes the bonus stable for a post's
// lifetime. A value derived from a seed, a clock or a process-local map would
// reproduce inside one run and silently reshuffle every ranked feed on the next
// deploy, so a test that only compared the function to itself would not notice.
var hostedCommunityBonusVectors = []struct {
	secret string
	uri    string
	bonus  float64
}{
	{firstHostedBonusSecret, "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/3lbf2qk7xy22a", 1.8743999374955136},
	{secondHostedBonusSecret, "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/3lbf2qk7xy22b", 1.908845968980569},
	{firstHostedBonusSecret, "at://did:plc:z72i7hdynmk6r22z27h6tvur/social.coves.community.postv2/3juvq6mrxyz2k", 0.8659818296073254},
	{secondHostedBonusSecret, "at://did:plc:vpkhqolt662uhesyj6nxm7ys/social.coves.community.postv2/3m5tj4pumyc77", 0.965717612953847},
	{firstHostedBonusSecret, "at://did:plc:44ybard66vv44zksje25o7dz/social.coves.community.postv2/3kzzzzzzzzzz2", 1.7494194956022269},
	{secondHostedBonusSecret, "at://did:plc:hdhoaan3xa3jiuq4fg4mefid/social.coves.community.postv2/3mvtj4pumycdd", 2.7601419001651606},
}

func TestHostedCommunityBonusReferenceVectors(t *testing.T) {
	t.Parallel()

	for _, vector := range hostedCommunityBonusVectors {
		assert.InDeltaf(t, vector.bonus, HostedCommunityBonus(vector.secret, vector.uri), 1e-15,
			"bonus for %s under its server secret must be the value the formula fixes for that post forever", vector.uri)
	}
}

// unkeyedHostedCommunityBonus is the retired formula: a bare SHA-256 of the
// URI, with no server secret anywhere in it.
//
// It is restated here to be excluded. The keyed and the unkeyed value are both
// deterministic and both spread over [0, 3.0), so an implementation that
// accepted the secret and then ignored it would satisfy every other property in
// this file. Naming the old value and requiring the new one to differ from it
// is what rules that out.
func unkeyedHostedCommunityBonus(uri string) float64 {
	digest := sha256.Sum256([]byte(uri))
	return 3.0 * float64(binary.BigEndian.Uint64(digest[:8])>>11) / (1 << 53)
}

func TestHostedCommunityBonusIsNotTheUnkeyedDigest(t *testing.T) {
	t.Parallel()

	for _, vector := range hostedCommunityBonusVectors {
		assert.Greaterf(t,
			math.Abs(HostedCommunityBonus(vector.secret, vector.uri)-unkeyedHostedCommunityBonus(vector.uri)),
			0.1, "bonus for %s must not be the unkeyed SHA-256 of the URI, which an author can grind offline", vector.uri)
	}
}

func TestHostedCommunityBonusVariesWithTheServerSecret(t *testing.T) {
	t.Parallel()

	const uri = "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/3lbf2qk7xy22a"
	assert.Greater(t,
		math.Abs(HostedCommunityBonus(firstHostedBonusSecret, uri)-HostedCommunityBonus(secondHostedBonusSecret, uri)),
		0.1, "one URI must hash to different bonuses under different server secrets: the secret is what the author cannot grind against")
}

func TestHostedCommunityBonusSpread(t *testing.T) {
	t.Parallel()

	const uriCount = 1000
	values := make([]float64, uriCount)
	minimum, maximum, sum := 3.0, 0.0, 0.0
	for i := range values {
		// Only the rkey varies: two posts from one author must not share a bonus.
		uri := fmt.Sprintf("at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/3lbf2qk7xy%03d", i)
		value := HostedCommunityBonus(firstHostedBonusSecret, uri)
		require.GreaterOrEqualf(t, value, 0.0, "bonus for %s is negative", uri)
		require.Lessf(t, value, 3.0, "bonus for %s is at or over the 3.0 ceiling", uri)

		values[i] = value
		minimum = min(minimum, value)
		maximum = max(maximum, value)
		sum += value
	}

	// A bonus that clustered would be indistinguishable from a constant: every
	// hosted post would move by the same amount and the tie-breakers, not the
	// bonus, would still decide the order.
	assert.Lessf(t, minimum, 0.3, "the smallest of %d bonuses should reach the bottom of the range", uriCount)
	assert.Greaterf(t, maximum, 2.7, "the largest of %d bonuses should reach the top of the range", uriCount)
	assert.InDelta(t, 1.5, sum/float64(uriCount), 0.2, "the bonus should be spread evenly across [0, 3.0)")
}

func TestHostedCommunityBonusIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	const uri = "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/3lbf2qk7xy22a"
	assert.Equal(t,
		HostedCommunityBonus(firstHostedBonusSecret, uri),
		HostedCommunityBonus(firstHostedBonusSecret, uri),
		"one secret and one URI must produce the identical value within a process, not merely a close one")
}

// frozenRankingTime and the frozen table below are the pre-bonus ranking
// function's own output, captured from this package before the parameter
// existed and printed with %.17g. They are compared with == rather than a
// delta: a bonus of zero must leave every existing rank bit-for-bit identical,
// because a rank that drifted even in the last digit would reshuffle the ties
// that decide feed order.
var frozenRankingTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

var frozenDiscoverHotRanks = []struct {
	name       string
	score      int
	age        time.Duration
	adjustment float64
	rank       float64
}{
	{"fresh zero score", 0, 0, 1, 0.35355339059327379},
	{"half-day-old zero score", 0, 15 * time.Hour, 1, 0.014266801472725471},
	{"small positive score", 5, 3 * time.Hour, 1, 0.24970255800090652},
	{"popular two-day-old post", 400, 48 * time.Hour, 0.8, 0.016391213593287227},
	{"very popular three-day-old post", 1000, 72 * time.Hour, 0.5, 0.0069974439678588476},
	{"single upvote, minutes old", 1, 30 * time.Minute, 0.8, 0.39326533884824877},
	{"lightly downvoted", -3, 6 * time.Hour, 1, -0.017071960142624978},
	{"heavily downvoted", -250, 36 * time.Hour, 0.5, -0.01931908965911432},
	{"future-dated positive score", 50, -5 * time.Hour, 1, 1.7436636742645033},
	{"future-dated zero score", 0, -90 * time.Minute, 0.5, 0.35355339059327379},
	{"fractional age", 7, 9*time.Hour + 45*time.Minute, 0.5, 0.050642357939854221},
}

func TestDiscoverHotRankWithoutBonusIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, frozen := range frozenDiscoverHotRanks {
		createdAt := frozenRankingTime.Add(-frozen.age)
		assert.Equalf(t, frozen.rank,
			discoverHotRank(frozen.score, createdAt, frozenRankingTime, frozen.adjustment, 0),
			"%s: a zero bonus must reproduce the pre-bonus rank exactly", frozen.name)
	}
}

// expectedRankWithBonus restates the contract rather than calling the code
// under test: numerator, then bonus for non-negative scores only, over the
// (age + 2)^1.5 decay.
func expectedRankWithBonus(score int, ageHours, adjustment, bonus float64) float64 {
	numerator := 1.0
	switch {
	case score > 0:
		numerator += adjustment * math.Log1p(float64(score))
	case score < 0:
		numerator -= math.Log1p(math.Abs(float64(score)))
	}
	if score >= 0 {
		numerator += bonus
	}
	return numerator / math.Pow(ageHours+2, 1.5)
}

func TestDiscoverHotRankAddsBonusToNumerator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		score      int
		age        time.Duration
		ageHours   float64
		adjustment float64
		bonus      float64
	}{
		{"zero score, small bonus", 0, 15 * time.Hour, 15, 1, 0.125},
		{"zero score, near-maximum bonus", 0, 3 * time.Hour, 3, 0.5, 2.9999},
		{"positive score, mid bonus", 400, 48 * time.Hour, 48, 0.8, 1.5},
		{"positive score, fractional age", 7, 9*time.Hour + 45*time.Minute, 9.75, 0.5, 2.25},
		// Age is clamped at zero, so a future post ranks as if brand new and
		// the bonus still applies.
		{"future-dated positive score", 50, -5 * time.Hour, 0, 1, 1.0},
	}

	for _, testCase := range cases {
		createdAt := frozenRankingTime.Add(-testCase.age)
		assert.InDeltaf(t,
			expectedRankWithBonus(testCase.score, testCase.ageHours, testCase.adjustment, testCase.bonus),
			discoverHotRank(testCase.score, createdAt, frozenRankingTime, testCase.adjustment, testCase.bonus),
			1e-12, "%s: the bonus belongs in the numerator, under the same decay", testCase.name)
	}
}

// A downvoted post must not be able to buy its way back up the feed by being
// hosted here: the bonus is a tie-breaker among posts the community has not
// rejected, not a floor under the ones it has.
func TestDiscoverHotRankIgnoresBonusForNegativeScores(t *testing.T) {
	t.Parallel()

	for _, frozen := range frozenDiscoverHotRanks {
		if frozen.score >= 0 {
			continue
		}
		createdAt := frozenRankingTime.Add(-frozen.age)
		for _, bonus := range []float64{0.125, 1.5, 3.0} {
			assert.Equalf(t, frozen.rank,
				discoverHotRank(frozen.score, createdAt, frozenRankingTime, frozen.adjustment, bonus),
				"%s: bonus %g must not lift a negative-score post", frozen.name, bonus)
		}
	}
}

// The cliff at the first downvote, pinned as a ratio rather than described in
// prose: one downvote removes the WHOLE bonus, dropping a ceiling-bonus post's
// numerator from 4.0 to 1 - ln 2 — a thirteenfold fall — and that is an
// approved product decision, not an accident of the formula.
func TestDiscoverHotRankFirstDownvoteRemovesTheEntireBonus(t *testing.T) {
	t.Parallel()

	const ageHours = 15.0
	createdAt := frozenRankingTime.Add(-ageHours * time.Hour)
	decay := math.Pow(ageHours+2, 1.5)

	atZeroScore := discoverHotRank(0, createdAt, frozenRankingTime, 1, 3.0)
	assert.InDelta(t, 4.0/decay, atZeroScore, 1e-15,
		"an unvoted post at the bonus ceiling carries numerator 1 + 3.0")

	atOneDownvote := discoverHotRank(-1, createdAt, frozenRankingTime, 1, 3.0)
	assert.Equal(t, discoverHotRank(-1, createdAt, frozenRankingTime, 1, 0), atOneDownvote,
		"a single downvote must leave the rank exactly where the bonus-free formula puts it")
	assert.InDelta(t, (1-math.Ln2)/decay, atOneDownvote, 1e-15,
		"the score -1 numerator is 1 - ln 2, with no part of the bonus surviving")

	assert.InDelta(t, 4.0/(1-math.Ln2), atZeroScore/atOneDownvote, 1e-9,
		"the drop across the first downvote is the full bonus, not a fraction of it")
}

func TestDiscoverHotRankRejectsUnusableBonus(t *testing.T) {
	t.Parallel()

	createdAt := frozenRankingTime.Add(-15 * time.Hour)
	rankWith := func(bonus float64) float64 {
		return discoverHotRank(0, createdAt, frozenRankingTime, 1, bonus)
	}

	// A NaN or infinite bonus would poison the rank, and the snapshot builder
	// rejects a nonfinite rank outright — one bad value would fail the whole
	// build rather than one post. A negative bonus is not a bonus.
	for _, bonus := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.5, -3.0} {
		assert.Equalf(t, rankWith(0), rankWith(bonus),
			"bonus %g must be treated as no bonus at all", bonus)
	}

	for _, bonus := range []float64{3.0000001, 10, 1e9} {
		assert.Equalf(t, rankWith(3.0), rankWith(bonus),
			"bonus %g must clamp to the 3.0 ceiling", bonus)
	}
}

func TestDiscoverHotRankStaysStrictlyIncreasingInScore(t *testing.T) {
	t.Parallel()

	createdAt := frozenRankingTime.Add(-4 * time.Hour)
	previous := discoverHotRank(0, createdAt, frozenRankingTime, 0.5, 1.7)
	for score := 1; score <= 50; score++ {
		current := discoverHotRank(score, createdAt, frozenRankingTime, 0.5, 1.7)
		assert.Greaterf(t, current, previous,
			"score %d must rank above score %d at the same bonus: a bonus that flattened engagement would make votes stop mattering", score, score-1)
		previous = current
	}
}

// The scenario the bonus exists for, and the one it must not exceed: a fresh
// post with no votes in a community hosted here beats a day-old popular post
// elsewhere, and does not beat it without the bonus.
func TestDiscoverHotRankHostedFreshPostOutranksOlderPopularPost(t *testing.T) {
	t.Parallel()

	freshCreatedAt := frozenRankingTime.Add(-15 * time.Hour)
	popularCreatedAt := frozenRankingTime.Add(-48 * time.Hour)
	popular := discoverHotRank(400, popularCreatedAt, frozenRankingTime, 0.8, 0)

	assert.Greater(t, discoverHotRank(0, freshCreatedAt, frozenRankingTime, 1, 1.0), popular,
		"a 15h-old hosted post with a 1.0 bonus should outrank a 48h-old post with 400 votes")
	assert.Less(t, discoverHotRank(0, freshCreatedAt, frozenRankingTime, 1, 0), popular,
		"without the bonus the same post must lose: the bonus is doing the work, not the decay")
}
