package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// U1: each subscription level maps to its Hot multiplier. The values are
// written out so that a change to a multiplier has to change this test too.
func TestTimelineHotMultiplier_MapsEachLevel(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		level int
		want  float64
	}{
		{level: 1, want: 1.0},
		{level: 2, want: 1.0},
		{level: 3, want: 1.0},
		{level: 4, want: 1.5},
		{level: 5, want: 2.0},
	} {
		assert.Equalf(t, testCase.want, timelineHotMultiplier(testCase.level),
			"timelineHotMultiplier(%d)", testCase.level)
	}
}

// U2 (guard): the Hot SQL that Discover and the community feed share with the
// timeline stays byte-identical while the timeline gains its weighted variant.
// The strings are the pre-change output, pasted verbatim.
func TestFeedHotSQL_DiscoverAndCommunityStringsUnchanged(t *testing.T) {
	t.Parallel()

	const goldenHotRank = "((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM (NOW() - p.created_at))::float8/3600, 0) + 2, 0.8))"

	t.Run("feedHotRankExpression", func(t *testing.T) {
		assert.Equal(t, goldenHotRank, feedHotRankExpression)
	})

	t.Run("feedSortClauses hot", func(t *testing.T) {
		assert.Equal(t,
			"((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM (NOW() - p.created_at))::float8/3600, 0) + 2, 0.8)) DESC, p.created_at DESC, p.uri DESC",
			feedSortClauses["hot"])
	})

	const payload = "2026-01-02T03:04:05.123456789Z::at://did:plc:golden/social.coves.community.post/goldenpost::2026-01-03T00:00:00Z"
	wantValues := []interface{}{
		"2026-01-02T03:04:05.123456789Z",
		"at://did:plc:golden/social.coves.community.post/goldenpost",
		"2026-01-03T00:00:00Z",
	}

	for _, testCase := range []struct {
		name       string
		offset     int
		wantFilter string
	}{
		{
			name:       "Discover cursor offset 2",
			offset:     2,
			wantFilter: "AND (\n\t\t\t((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) < (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $3)\n\t\t\tOR (((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) = (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $3) AND p.created_at < $2)\n\t\t\tOR (((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) = (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($4::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $3) AND p.created_at = $2 AND p.uri < $3)\n\t\t)",
		},
		{
			name:       "community cursor offset 3",
			offset:     3,
			wantFilter: "AND (\n\t\t\t((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) < (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $4)\n\t\t\tOR (((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) = (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $4) AND p.created_at < $3)\n\t\t\tOR (((SIGN(p.score) * LN(ABS(p.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - p.created_at))::float8/3600, 0) + 2, 0.8)) = (SELECT ((SIGN(cursor_post.score) * LN(ABS(cursor_post.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz - cursor_post.created_at))::float8/3600, 0) + 2, 0.8)) FROM posts cursor_post WHERE cursor_post.uri = $4) AND p.created_at = $3 AND p.uri < $4)\n\t\t)",
		},
	} {
		t.Run("parseCursorPayload hot "+testCase.name, func(t *testing.T) {
			filter, values, err := (&feedRepoBase{}).parseCursorPayload(payload, "hot", testCase.offset)
			require.NoError(t, err)
			assert.Equal(t, testCase.wantFilter, filter)
			assert.Equal(t, wantValues, values)
		})
	}
}
