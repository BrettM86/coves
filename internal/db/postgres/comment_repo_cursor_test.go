//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/comments"
)

// The sort clause, the timeframe filter and the cursor codec behind
// ListByParentWithHotRank — the query that renders a comment thread.
//
// These four helpers are unexported and pure-looking, which is exactly why they
// are tested from both sides here. Read on their own they are string builders
// and a base64 codec, and every one of them "works": the ORDER BY parses, the
// cursor round-trips, the filter is valid SQL. What they actually decide is
// which comments a reader ever sees, and that only becomes visible when the
// strings meet the rows. So every arm is asserted twice — once directly, so the
// arm is unambiguous, and once through the query, so the arm is proven to
// change the answer.
//
// The sort tests deliberately use fixtures whose orderings DISAGREE. A ranking
// test built on rows that come out in the same order under every sort mode is
// asserting nothing at all, and would pass against a repository that ignored
// the sort parameter entirely.
//
// The pagination tests page through a whole set and check every row was visited
// exactly once. This is the only assertion shape that catches both of the ways
// a cursor fails, and it is how two cursor defects were found: under the "hot"
// sort a months-old thread lost every comment after page one, because the rank
// was written into the cursor at six decimal places and rounded to zero
// (guarded by TestCommentRepo_HotPaginationReachesEveryReplyOnAMonthsOldThread),
// and under an unrecognised sort the cursor is discarded and page one repeats
// forever (pinned by TestCommentRepo_UnknownSortCursorRepeatsPageOne).
//
// See comment_repo_write_test.go for commentEnv and the seeding helpers.

// commentHotRankOf recomputes the ranking formula the way the SQL does, so a
// test can say what the cursor SHOULD have carried.
func commentHotRankOf(t *testing.T, env *commentEnv, uri string) float64 {
	t.Helper()
	var rank float64
	query := fmt.Sprintf(`
		SELECT %s
		FROM comments c WHERE c.uri = $1
	`, commentHotRankSQL("c", "NOW()"))
	require.NoError(t, env.db.QueryRowContext(env.ctx, query, uri).Scan(&rank))
	return rank
}

func TestCommentRepo_BuildCommentSortClause(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)

	// Each ordering must differ from the others: three sort modes that produced
	// the same ORDER BY would be one sort mode with three names.
	t.Run("each mode orders by its own key", func(t *testing.T) {
		t.Parallel()
		hot, _ := env.impl.buildCommentSortClause("hot", "")
		top, _ := env.impl.buildCommentSortClause("top", "")
		fresh, _ := env.impl.buildCommentSortClause("new", "")

		assert.Equal(t, "hot_rank DESC, c.score DESC, c.created_at DESC, c.uri DESC", hot)
		assert.Equal(t, "c.score DESC, c.created_at DESC, c.uri DESC", top)
		assert.Equal(t, "c.created_at DESC, c.uri DESC", fresh)
		assert.NotEqual(t, hot, top)
		assert.NotEqual(t, top, fresh)

		for name, clause := range map[string]string{"hot": hot, "top": top, "new": fresh} {
			assert.Truef(t, strings.HasSuffix(clause, "c.uri DESC"),
				"%s must end in a unique tiebreak: without one, two comments with equal keys swap "+
					"places between requests, which is a pagination bug and not only a cosmetic one", name)
		}
	})

	t.Run("an unrecognised mode falls back to hot", func(t *testing.T) {
		t.Parallel()
		hot, _ := env.impl.buildCommentSortClause("hot", "")
		fallback, _ := env.impl.buildCommentSortClause("nonsense", "")
		assert.Equal(t, hot, fallback,
			"the sort string reaches here from a query parameter; without a default arm the ORDER BY "+
				"would be empty and Postgres would return the thread in whatever order the plan produced")

		empty, _ := env.impl.buildCommentSortClause("", "")
		assert.Equal(t, hot, empty)
	})

	// A timeframe is meaningless for a chronological or a decayed ranking — "the
	// best comments of this week" is a question only "top" can answer.
	t.Run("only top takes a timeframe", func(t *testing.T) {
		t.Parallel()
		_, topFilter := env.impl.buildCommentSortClause("top", "week")
		assert.Contains(t, topFilter, "INTERVAL '7 days'")

		for _, sort := range []string{"hot", "new", "nonsense", ""} {
			_, filter := env.impl.buildCommentSortClause(sort, "week")
			assert.Emptyf(t, filter, "%q must ignore the timeframe: applying it would silently hide "+
				"every older comment in a thread the reader asked to see chronologically", sort)
		}
	})
}

func TestCommentRepo_BuildCommentTimeFilter(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)

	t.Run("each named window becomes its own interval", func(t *testing.T) {
		t.Parallel()
		expected := map[string]string{
			"hour":  "1 hour",
			"day":   "1 day",
			"week":  "7 days",
			"month": "30 days",
			"year":  "1 year",
		}
		seen := make(map[string]bool, len(expected))
		for timeframe, interval := range expected {
			filter := env.impl.buildCommentTimeFilter(timeframe)
			assert.Equalf(t, fmt.Sprintf("AND c.created_at >= NOW() - INTERVAL '%s'", interval), filter,
				"%q must select its own window; two timeframes that produced the same SQL would be "+
					"one control with two labels", timeframe)
			assert.Falsef(t, seen[filter], "%q duplicated another timeframe's filter", timeframe)
			seen[filter] = true
		}
	})

	// No filter at all, rather than a very large interval: "all time" must reach
	// comments older than any interval anybody thought to write down.
	t.Run("all time and the empty string add no filter", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, env.impl.buildCommentTimeFilter(""))
		assert.Empty(t, env.impl.buildCommentTimeFilter("all"))
	})

	// An unrecognised timeframe widens rather than narrows. Failing open is the
	// right direction here: showing too much is a worse ranking, showing nothing
	// is a broken thread.
	t.Run("an unrecognised window adds no filter", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, env.impl.buildCommentTimeFilter("fortnight"))
		assert.Empty(t, env.impl.buildCommentTimeFilter("'; DROP TABLE comments; --"),
			"the timeframe is interpolated into SQL rather than bound, so the switch is the only "+
				"thing standing between a query parameter and the query text")
	})
}

func TestCommentRepo_ParseCommentCursor(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	const validURI = "at://did:plc:cmtcur/social.coves.community.comment/abc"
	const validTime = "2026-03-04T05:06:07Z"
	// The instant a hot walk was ranked at. It differs from validTime so an
	// assertion on the parsed values can tell the two timestamps apart.
	const validRankedAt = "2026-03-05T06:07:08.123456789Z"

	t.Run("no cursor means no filter", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"hot", "top", "new"} {
			filter, values, err := env.impl.parseCommentCursor(nil, sort)
			require.NoError(t, err)
			assert.Empty(t, filter)
			assert.Empty(t, values)

			blank := ""
			filter, values, err = env.impl.parseCommentCursor(&blank, sort)
			require.NoError(t, err)
			assert.Empty(t, filter)
			assert.Empty(t, values)
		}
	})

	// Each sort's cursor carries exactly the keys its ORDER BY compares on, and
	// the filter has to bind one parameter per key. A cursor with the wrong
	// arity is either a client that changed sort mid-scroll or a forged one;
	// either way, guessing at the missing key would skip rows.
	t.Run("each sort accepts only its own cursor shape", func(t *testing.T) {
		t.Parallel()
		shapes := map[string]struct {
			valid  string
			params int
		}{
			"new": {validTime + "|" + validURI, 2},
			"top": {"42|" + validTime + "|" + validURI, 3},
			"hot": {validRankedAt + "|42|" + validTime + "|" + validURI, 4},
		}
		for sort, shape := range shapes {
			cursor := commentEncodeCursor(shape.valid)
			filter, values, err := env.impl.parseCommentCursor(&cursor, sort)
			require.NoErrorf(t, err, "the %s cursor shape must be accepted", sort)
			assert.NotEmpty(t, filter)
			assert.Lenf(t, values, shape.params, "the %s filter binds one parameter per field its "+
				"cursor carries (hot carries the instant page one was ranked at, not a rank)", sort)

			for otherSort, other := range shapes {
				if otherSort == sort {
					continue
				}
				wrong := commentEncodeCursor(other.valid)
				_, _, err := env.impl.parseCommentCursor(&wrong, sort)
				require.Errorf(t, err, "a %s cursor must be rejected by the %s parser", otherSort, sort)
				assert.ErrorIsf(t, err, comments.ErrInvalidCursor,
					"a client that switched sort mid-scroll sent bad input, not a broken server; the "+
						"wrap is what makes this a 400 (%s cursor under %s sort)", otherSort, sort)
			}
		}
	})

	// A cursor with a valid prefix and junk appended. The URI check alone would
	// accept these — the URI is still in a plausible position — so the arity
	// check is the only thing that catches them, and a forged cursor that
	// parsed would page the thread from a key the ORDER BY does not use.
	t.Run("rejects a cursor with extra fields appended", func(t *testing.T) {
		t.Parallel()
		for sort, plaintext := range map[string]string{
			"new": validTime + "|" + validURI + "|extra",
			"top": "42|" + validTime + "|" + validURI + "|extra",
			"hot": validRankedAt + "|42|" + validTime + "|" + validURI + "|extra",
		} {
			cursor := commentEncodeCursor(plaintext)
			_, _, err := env.impl.parseCommentCursor(&cursor, sort)
			require.Errorf(t, err, "the %s parser accepted a cursor with a trailing field", sort)
			assert.ErrorIs(t, err, comments.ErrInvalidCursor)
		}
	})

	// Each hot row breaks exactly one field and keeps the other three valid, so
	// the refusal can only come from the check on that field.
	for name, tc := range map[string]struct {
		sort   string
		cursor string
	}{
		"new sort, uri is not an AT-URI":                    {"new", validTime + "|https://example.com/evil"},
		"top sort, uri is not an AT-URI":                    {"top", "42|" + validTime + "|https://example.com"},
		"hot sort, uri is not an AT-URI":                    {"hot", validRankedAt + "|42|" + validTime + "|nope"},
		"top sort, score is not a number":                   {"top", "banana|" + validTime + "|" + validURI},
		"hot sort, score is not a number":                   {"hot", validRankedAt + "|banana|" + validTime + "|" + validURI},
		"hot sort, score has trailing garbage":              {"hot", validRankedAt + "|30zzz|" + validTime + "|" + validURI},
		"hot sort, score is above the SQL integer range":    {"hot", validRankedAt + "|2147483648|" + validTime + "|" + validURI},
		"hot sort, score is below the SQL integer range":    {"hot", validRankedAt + "|-2147483649|" + validTime + "|" + validURI},
		"hot sort, score has no room for the rank's +2":     {"hot", validRankedAt + "|2147483646|" + validTime + "|" + validURI},
		"hot sort, score is the SQL integer maximum":        {"hot", validRankedAt + "|2147483647|" + validTime + "|" + validURI},
		"hot sort, rankedAt is not a timestamp":             {"hot", "banana|42|" + validTime + "|" + validURI},
		"hot sort, createdAt is not a timestamp":            {"hot", validRankedAt + "|42|banana|" + validURI},
		"hot sort, rankedAt is in year 0000":                {"hot", "0000-01-01T00:00:00Z|42|" + validTime + "|" + validURI},
		"hot sort, createdAt is in year 0000":               {"hot", validRankedAt + "|42|0000-01-01T00:00:00Z|" + validURI},
		"hot sort, rankedAt is year 0000 in UTC":            {"hot", "0001-01-01T00:30:00+01:00|42|" + validTime + "|" + validURI},
		"hot sort, createdAt is year 0000 in UTC":           {"hot", validRankedAt + "|42|0001-01-01T00:30:00+01:00|" + validURI},
		"hot sort, rank slot of a pre-fix cursor":           {"hot", "0.000000|0|" + validTime + "|" + validURI},
		"hot sort, full-precision rank of a pre-fix cursor": {"hot", "1.2e-07|0|" + validTime + "|" + validURI},
		"top sort, score overflows int":                     {"top", "99999999999999999999|" + validTime + "|" + validURI},
	} {
		tc := tc
		t.Run("rejects a cursor where the "+name, func(t *testing.T) {
			t.Parallel()
			cursor := commentEncodeCursor(tc.cursor)
			_, _, err := env.impl.parseCommentCursor(&cursor, tc.sort)
			require.Error(t, err, "a cursor whose keys do not parse must be refused; coercing them to "+
				"zero would restart the page at the top of the thread")
			assert.ErrorIs(t, err, comments.ErrInvalidCursor)
		})
	}

	t.Run("rejects a cursor that is not base64", func(t *testing.T) {
		t.Parallel()
		cursor := "!!! definitely not base64 !!!"
		for _, sort := range []string{"hot", "top", "new"} {
			_, _, err := env.impl.parseCommentCursor(&cursor, sort)
			require.Error(t, err)
			assert.ErrorIs(t, err, comments.ErrInvalidCursor)
		}
	})

	t.Run("rejects an oversized cursor before decoding it", func(t *testing.T) {
		t.Parallel()
		huge := commentEncodeCursor(validTime + "|at://" + strings.Repeat("a", 4096))
		require.Greater(t, len(huge), 1024)

		_, _, err := env.impl.parseCommentCursor(&huge, "new")
		require.Error(t, err)
		assert.ErrorIs(t, err, comments.ErrInvalidCursor)
		assert.ErrorContains(t, err, "cursor too large",
			"the length check must precede the base64 decode, or a hostile client still makes the "+
				"server allocate whatever it sent")
	})

	// Round trip, per sort: the cursor the listing hands out must be what
	// parseCommentCursor reads back. The builder and the parser are the only two
	// functions that know the wire format, and nothing else validates that they
	// agree. The cursors come from real listings rather than from the builder, so
	// the test does not depend on the builder's signature.
	//
	// The reply the cursor names has a fractional-second created_at and the
	// highest score and the latest created_at in its thread, so it leads the
	// thread under every sort and page one (limit 1) ends on it under each. The
	// other reply exists only so that there is a page two and so a cursor.
	t.Run("reads back what the builder wrote", func(t *testing.T) {
		t.Parallel()
		threadEnv := commentEnvFor(t)
		createdAt := time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC)
		named := threadEnv.seed(commentSpec{rkey: "named", score: 37, createdAt: createdAt})
		threadEnv.seed(commentSpec{rkey: "other", score: 0, createdAt: createdAt.Add(-time.Hour)})

		cursorFor := func(t *testing.T, sort string) string {
			t.Helper()
			page, next, err := threadEnv.repo.ListByParentWithHotRank(threadEnv.ctx, threadEnv.root, sort, "", 1, nil, "")
			require.NoError(t, err)
			require.Equalf(t, []string{named}, commentURIs(page),
				"THE FIXTURE IS WRONG, not the code: page one under %s must be the reply the cursor is "+
					"meant to name", sort)
			require.NotNilf(t, next, "THE FIXTURE IS WRONG, not the code: the thread has two replies and "+
				"the page holds one, so %s must hand out a cursor", sort)
			return *next
		}

		newCursor := cursorFor(t, "new")
		_, values, err := threadEnv.impl.parseCommentCursor(&newCursor, "new")
		require.NoError(t, err)
		require.Len(t, values, 2)
		assert.Equal(t, "2026-03-04T05:06:07.891011Z", values[0],
			"the timestamp must survive with its fractional second: truncating to whole seconds "+
				"would skip or repeat every comment posted in the same second")
		assert.Equal(t, named, values[1])

		topCursor := cursorFor(t, "top")
		_, values, err = threadEnv.impl.parseCommentCursor(&topCursor, "top")
		require.NoError(t, err)
		require.Len(t, values, 3)
		assert.Equal(t, 37, values[0], "the score is the primary sort key of the top ranking")

		hotCursor := cursorFor(t, "hot")
		_, values, err = threadEnv.impl.parseCommentCursor(&hotCursor, "hot")
		require.NoError(t, err)
		require.Len(t, values, 4, "a hot cursor carries rankedAt, score, createdAt and uri, and "+
			"nothing else: the rank is recomputed as of rankedAt, not carried")
		rankedAt, isString := values[0].(string)
		require.True(t, isString, "rankedAt must be handed to the query as the string the cursor carries")
		assert.Equal(t, strings.Split(commentDecodeCursor(t, hotCursor), "|")[0], rankedAt,
			"rankedAt must be read back exactly as the cursor wrote it")
		_, err = time.Parse(time.RFC3339Nano, rankedAt)
		assert.NoError(t, err, "rankedAt is the instant page one was ranked at, written as RFC3339 "+
			"with fractional seconds")
		assert.Equal(t, 37, values[1])
		assert.Equal(t, "2026-03-04T05:06:07.891011Z", values[2],
			"the created_at tiebreak must survive with its fractional second, as it does under new")
		assert.Equal(t, named, values[3])
	})

	// Its own thread, not the shared env: a parallel sibling that seeded into
	// the same thread would change which reply page one ends on.
	t.Run("the default arm of the builder emits the bare URI", func(t *testing.T) {
		t.Parallel()
		threadEnv := commentEnvFor(t)
		now := time.Now().UTC()
		threadEnv.seed(commentSpec{rkey: "first", score: 5, createdAt: now})
		threadEnv.seed(commentSpec{rkey: "second", score: 1, createdAt: now.Add(-time.Minute)})

		page, cursor, err := threadEnv.repo.ListByParentWithHotRank(threadEnv.ctx, threadEnv.root, "wildly-unknown", "", 1, nil, "")
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.NotNil(t, cursor, "THE FIXTURE IS WRONG, not the code: the thread has two replies and "+
			"the page holds one, so the listing must hand out a cursor")
		assert.Equal(t, page[0].URI, commentDecodeCursor(t, *cursor),
			"an unrecognised sort produces a cursor carrying no sort key at all, which is what makes "+
				"it unusable — see TestCommentRepo_UnknownSortCursorRepeatsPageOne")
	})
}

// TestCommentRepo_UnknownSortCursorIsSilentlyDiscarded pins the parser half of
// the unrecognised-sort defect.
//
// buildCommentSortClause defaults an unknown sort to the hot ORDER BY, so the
// query runs and returns a sensible first page. parseCommentCursor's default
// arm returns no filter and NO ERROR, so the cursor built from that page is
// thrown away on the next request. The two halves disagree about what an
// unknown sort means, and the disagreement is silent in both directions.
//
// Either would be defensible on its own: reject the sort, or default both arms
// to hot. What is not defensible is accepting the cursor and ignoring it.
//
// REACHABILITY: no production caller can reach this today.
// comment_service.go:1327-1333 validates Sort against {hot, top, new} and
// returns an error before the repository is called, so an unrecognised sort
// dies one layer up. This is pinned anyway because the guard is in a DIFFERENT
// package from the two functions that disagree, nothing enforces the
// relationship, and the defect goes live the moment a fourth sort is added to
// the service's allow-list without both repository arms learning it — which is
// exactly the change someone will make when they add "controversial".
//
// Issue: 2026-07-31-comment-repo-unrecognised-sort-trio.md
func TestCommentRepo_UnknownSortCursorIsSilentlyDiscarded(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	cursor := commentEncodeCursor("2026-03-04T05:06:07Z|at://did:plc:x/social.coves.community.comment/y")

	filter, values, err := env.impl.parseCommentCursor(&cursor, "wildly-unknown")
	assert.NoError(t, err,
		"an unknown sort should reject the cursor with comments.ErrInvalidCursor, or default to the "+
			"hot parser the way buildCommentSortClause defaults to the hot ORDER BY. "+
			"IF THIS FAILED (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md) the defect is FIXED — delete this pin")
	assert.Empty(t, filter, "the cursor was discarded, so the next page starts at the top again")
	assert.Empty(t, values)

	orderBy, _ := env.impl.buildCommentSortClause("wildly-unknown", "")
	assert.Contains(t, orderBy, "hot_rank",
		"and the ORDER BY half of the same unknown sort did NOT default to nothing — it defaulted "+
			"to hot, which is the inconsistency")
}

func TestCommentRepo_ThreadSortModes(t *testing.T) {
	t.Parallel()

	// Scores and ages run in opposite directions so no two sort modes can agree
	// by accident.
	seed := func(t *testing.T) (*commentEnv, string, string, string, string) {
		t.Helper()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		best := env.seed(commentSpec{rkey: "best", score: 500, createdAt: now.Add(-72 * time.Hour)})
		middling := env.seed(commentSpec{rkey: "middling", score: 20, createdAt: now.Add(-36 * time.Hour)})
		newest := env.seed(commentSpec{rkey: "newest", score: 1, createdAt: now.Add(-time.Minute)})
		return env, env.root, best, middling, newest
	}

	t.Run("new is chronological and top is by score", func(t *testing.T) {
		t.Parallel()
		env, parent, best, middling, newest := seed(t)

		byNew, _, err := env.repo.ListByParentWithHotRank(env.ctx, parent, "new", "", 10, nil, "")
		require.NoError(t, err)
		assert.Equal(t, []string{newest, middling, best}, commentURIs(byNew))

		byTop, _, err := env.repo.ListByParentWithHotRank(env.ctx, parent, "top", "all", 10, nil, "")
		require.NoError(t, err)
		assert.Equal(t, []string{best, middling, newest}, commentURIs(byTop),
			"top must rank by score; an order identical to new would mean the sort parameter never "+
				"reached the ORDER BY")
	})

	// Hot is neither of the other two, and this fixture is built so that it
	// cannot coincide with either: the freshest comment has the worst score, and
	// the best-scored one is three days old, so only a formula that weighs both
	// puts the middling comment where hot puts it.
	t.Run("hot is neither chronological nor by score alone", func(t *testing.T) {
		t.Parallel()
		env, parent, best, middling, newest := seed(t)

		byHot, _, err := env.repo.ListByParentWithHotRank(env.ctx, parent, "hot", "", 10, nil, "")
		require.NoError(t, err)
		assert.Equal(t, []string{newest, middling, best}, commentURIs(byHot),
			"a minute-old comment outranks a three-day-old one even at 1 point against 500, because "+
				"the decay is age^1.8; if this ever matches the top ordering the decay term has stopped "+
				"being applied")

		byTop, _, err := env.repo.ListByParentWithHotRank(env.ctx, parent, "top", "", 10, nil, "")
		require.NoError(t, err)
		assert.NotEqual(t, commentURIs(byTop), commentURIs(byHot),
			"hot and top must disagree on this fixture, or neither assertion above is testing "+
				"anything the other does not")
	})

	// The timeframe is a filter, not a weighting: "top this week" must not
	// merely demote older comments, it must exclude them.
	//
	// The fixture is a LADDER, one comment in each gap between adjacent windows,
	// because the obvious fixture does not test what it appears to. An earlier
	// version of this used ages of 10 minutes, 30 hours and 2 years, which are
	// an order of magnitude apart: every window from "hour" to "year" then had
	// only two possible answers, so swapping hour for day, or year for month, in
	// buildCommentTimeFilter changed no result and the test stayed green. Only
	// deleting the filter entirely was caught.
	//
	// With one row per gap, each window admits exactly one more comment than the
	// window below it, so every adjacent-boundary mutation moves at least one
	// row across at least one assertion. The pair at 50 and 70 minutes also sits
	// tight against the hour boundary, so a window that drifts even slightly is
	// caught rather than merely a window swapped for a different named one.
	t.Run("the timeframe excludes comments outside the window", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		now := time.Now().UTC()

		// Scores ASCEND with age, so "top" orders oldest-first and every
		// expected page below is a prefix-free, fully ordered list. It also
		// keeps the original point sharp: the highest-scoring comment in the
		// thread is the one furthest outside every window.
		insideHour := env.seed(commentSpec{rkey: "in-hr", score: 1, createdAt: now.Add(-50 * time.Minute)})
		insideDay := env.seed(commentSpec{rkey: "in-day", score: 2, createdAt: now.Add(-70 * time.Minute)})
		insideWeek := env.seed(commentSpec{rkey: "in-wk", score: 3, createdAt: now.AddDate(0, 0, -3)})
		insideMonth := env.seed(commentSpec{rkey: "in-mo", score: 4, createdAt: now.AddDate(0, 0, -14)})
		insideYear := env.seed(commentSpec{rkey: "in-yr", score: 5, createdAt: now.AddDate(0, 0, -100)})
		ancient := env.seed(commentSpec{rkey: "ancient", score: 1000, createdAt: now.AddDate(-2, 0, 0)})

		for _, tc := range []struct {
			timeframe string
			want      []string
		}{
			{"hour", []string{insideHour}},
			{"day", []string{insideDay, insideHour}},
			{"week", []string{insideWeek, insideDay, insideHour}},
			{"month", []string{insideMonth, insideWeek, insideDay, insideHour}},
			{"year", []string{insideYear, insideMonth, insideWeek, insideDay, insideHour}},
		} {
			page, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "top", tc.timeframe, 10, nil, "")
			require.NoError(t, err)
			assert.Equalf(t, tc.want, commentURIs(page),
				"'top of the last %s' admitted the wrong set. Each window must admit exactly one "+
					"more comment than the one below it; a window that matches its neighbour's "+
					"answer is a window that is not doing its own job", tc.timeframe)
		}

		byHour, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "top", "hour", 10, nil, "")
		require.NoError(t, err)
		assert.NotContains(t, commentURIs(byHour), ancient,
			"'top of the last hour' must not be led by a two-year-old comment with a thousand points")

		everythingWanted := []string{ancient, insideYear, insideMonth, insideWeek, insideDay, insideHour}
		for _, timeframe := range []string{"", "all", "fortnight"} {
			everything, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "top", timeframe, 10, nil, "")
			require.NoError(t, err)
			assert.ElementsMatchf(t, everythingWanted, commentURIs(everything),
				"timeframe %q must widen to everything rather than narrow to nothing", timeframe)
		}
	})

	t.Run("the timeframe is ignored by the sorts that do not take one", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		recent := env.seed(commentSpec{rkey: "recent", score: 1, createdAt: now.Add(-time.Minute)})
		ancient := env.seed(commentSpec{rkey: "ancient", score: 5, createdAt: now.AddDate(-2, 0, 0)})

		for _, sort := range []string{"new", "hot"} {
			page, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, sort, "hour", 10, nil, "")
			require.NoError(t, err)
			assert.ElementsMatchf(t, []string{recent, ancient}, commentURIs(page),
				"%q must show the whole thread even when a timeframe is supplied; hiding older "+
					"replies would silently truncate the conversation", sort)
		}
	})
}

// TestCommentRepo_UnknownSortRanksLikeTopNotLikeHot pins the third way an
// unrecognised sort goes wrong, and the one with an observable effect on
// ordering.
//
// buildCommentSortClause's default arm returns the hot ORDER BY — "hot_rank
// DESC, c.score DESC, …". But ListByParentWithHotRank only COMPUTES hot_rank
// when sort == "hot" exactly; every other value selects NULL::numeric for that
// column. So under an unrecognised sort the leading key is NULL for every row,
// the comparison collapses to the score tiebreak, and the thread comes back
// ranked by score — that is, as "top", with no timeframe.
//
// ListByParentsBatch, which renders the replies below those same comments, has
// an explicit default arm that DOES compute the rank (comment_repo.go:1150), so
// the two halves of one thread view disagree about what an unknown sort means:
// the top level is ordered by score and the nested replies by hot.
//
// The fix is for the selectClause branch to follow the same default as the sort
// clause, or for the repository to reject a sort it does not recognise.
//
// REACHABILITY: no production caller can reach this today.
// comment_service.go:1327-1333 validates Sort against {hot, top, new} and
// returns an error before the repository is called, so an unrecognised sort
// dies one layer up. This is pinned anyway because the guard is in a DIFFERENT
// package from the two functions that disagree, nothing enforces the
// relationship, and the defect goes live the moment a fourth sort is added to
// the service's allow-list without both repository arms learning it — which is
// exactly the change someone will make when they add "controversial".
//
// Issue: 2026-07-31-comment-repo-unrecognised-sort-trio.md
func TestCommentRepo_UnknownSortRanksLikeTopNotLikeHot(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	// Old and high-scoring versus fresh and low-scoring: hot puts the fresh one
	// first, score puts the old one first.
	oldBest := env.seed(commentSpec{rkey: "oldbest", score: 500, createdAt: now.Add(-72 * time.Hour)})
	freshWorst := env.seed(commentSpec{rkey: "freshworst", score: 1, createdAt: now.Add(-time.Minute)})

	byHot, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 10, nil, "")
	require.NoError(t, err)
	require.Equal(t, []string{freshWorst, oldBest}, commentURIs(byHot))

	byUnknown, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "confidence", "", 10, nil, "")
	require.NoError(t, err)
	assert.Equal(t, []string{oldBest, freshWorst}, commentURIs(byUnknown),
		"an unrecognised sort should rank the way buildCommentSortClause's default arm says it "+
			"will — as hot. It ranks by score instead, because hot_rank is NULL unless the sort "+
			"string is exactly \"hot\". IF THIS FAILED (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md) the defect is FIXED — delete this pin")

	nested, err := env.repo.ListByParentsBatch(env.ctx, []string{env.root}, "confidence", 10, "")
	require.NoError(t, err)
	assert.Equal(t, []string{freshWorst, oldBest}, commentURIs(nested[env.root]),
		"and the batch used for the nested replies of the same thread DOES default to hot, so one "+
			"view is ordered two ways. IF THIS FAILED (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md) the two paths now agree — delete this pin")
}

func TestCommentRepo_ThreadCursorPagination(t *testing.T) {
	t.Parallel()

	// Five replies, of which two share BOTH a score and a created_at, so the
	// pair is tied under "top" and under "new" alike.
	//
	// Two properties of this fixture are load-bearing and easy to lose. The
	// tied pair sits at positions two and three of both orderings, so at a page
	// size of two it STRADDLES the boundary — a cursor whose comparison chain
	// stops before the URI drops one of the pair rather than merely swapping
	// them, and only a straddling tie makes that visible. And the pair is
	// inserted in the opposite order to the one the tiebreak asks for ("tiea"
	// before "tieb", against uri DESC), so a query that lost its tiebreak
	// cannot come out right by accident on physical row order.
	seedFive := func(t *testing.T) (*commentEnv, []string) {
		t.Helper()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		var all []string
		for _, spec := range []commentSpec{
			{rkey: "r1", score: 50, createdAt: now.Add(-time.Minute)},
			{rkey: "tiea", score: 30, createdAt: now.Add(-2 * time.Minute)},
			{rkey: "tieb", score: 30, createdAt: now.Add(-2 * time.Minute)},
			{rkey: "r4", score: 20, createdAt: now.Add(-3 * time.Minute)},
			{rkey: "r5", score: 10, createdAt: now.Add(-4 * time.Minute)},
		} {
			all = append(all, env.seed(spec))
		}
		return env, all
	}

	pageAll := func(t *testing.T, env *commentEnv, sort string, limit int) []string {
		t.Helper()
		var visited []string
		var cursor *string
		for requests := 0; ; requests++ {
			require.Less(t, requests, 20, "the cursor stopped advancing: this thread never ends")
			page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, sort, "", limit, cursor, "")
			require.NoError(t, err)
			visited = append(visited, commentURIs(page)...)
			if next == nil {
				return visited
			}
			cursor = next
		}
	}

	for _, sort := range []string{"new", "top"} {
		sort := sort
		t.Run(sort+" visits every reply exactly once across ties", func(t *testing.T) {
			t.Parallel()
			env, all := seedFive(t)

			visited := pageAll(t, env, sort, 2)
			assert.ElementsMatch(t, all, visited,
				"a reply on no page has been silently removed from the thread; one on two pages "+
					"makes the conversation look duplicated")
			assert.Len(t, visited, len(all))

			single, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, sort, "", 50, nil, "")
			require.NoError(t, err)
			assert.Equal(t, commentURIs(single), visited,
				"and the paged order must match the unpaged one, or the reader sees the thread "+
					"reshuffle as they scroll")
		})
	}

	t.Run("stops at an exact page boundary", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		for i, rkey := range []string{"b1", "b2", "b3", "b4"} {
			env.seed(commentSpec{rkey: rkey, score: 10 - i, createdAt: now.Add(-time.Duration(i) * time.Minute)})
		}

		first, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "top", "", 2, nil, "")
		require.NoError(t, err)
		require.Len(t, first, 2)
		require.NotNil(t, next)

		second, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "top", "", 2, next, "")
		require.NoError(t, err)
		require.Len(t, second, 2)
		assert.Nil(t, next, "the limit+1 probe exists so the last full page knows it is the last; "+
			"a cursor here sends the reader to an empty page")
	})

	// The freshest possible thread, where the ranks are large and far apart.
	// The months-old case, where the ranks are small enough to lose precision,
	// is TestCommentRepo_HotPaginationReachesEveryReplyOnAMonthsOldThread.
	t.Run("hot paginates a brand new thread whose ranks are far apart", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		now := time.Now().UTC()
		var all []string
		for _, spec := range []commentSpec{
			{rkey: "h1", score: 1000, createdAt: now},
			{rkey: "h2", score: 20, createdAt: now},
			{rkey: "h3", score: 0, createdAt: now},
		} {
			all = append(all, env.seed(spec))
		}

		visited := pageAll(t, env, "hot", 1)
		assert.ElementsMatch(t, all, visited)
	})
}

// TestCommentRepo_HotPaginationReachesEveryReplyOnAMonthsOldThread guards hot
// pagination, the default sort for a thread, on an old thread.
//
// Issue: 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md
//
// The hot rank is log10(greatest(2, score+2)) / (age_in_hours+2)^1.8, so it
// shrinks as a thread ages. At score 0 it drops below 5e-7 at about 67.6 days,
// which is where six decimal places print it as 0.000000. A cursor that carries
// the rank at that precision sends page two looking for replies ranked below
// zero, finds none, and ends the thread after one page with no error. Every
// reply after page one becomes unreachable.
//
// Why this fixture: score 0 is the score that crosses earliest, and 70 days is
// past the 67.6-day crossing with margin for clock skew between the test and
// Postgres. The five replies are an hour apart, so their ranks are distinct and
// the hot order is decided by age rather than by a tiebreak. A page size of two
// over five replies takes three pages, so the walk crosses two boundaries.
func TestCommentRepo_HotPaginationReachesEveryReplyOnAMonthsOldThread(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	seventyDaysAgo := time.Now().UTC().Add(-70 * 24 * time.Hour)
	var all []string
	for i, rkey := range []string{"aged1", "aged2", "aged3", "aged4", "aged5"} {
		all = append(all, env.seed(commentSpec{
			rkey: rkey, score: 0, createdAt: seventyDaysAgo.Add(-time.Duration(i) * time.Hour),
		}))
	}

	// Every reply is checked, so the one page one ends on is covered whichever
	// it is.
	for _, uri := range all {
		require.Less(t, commentHotRankOf(t, env, uri), 5e-7,
			"THE FIXTURE IS WRONG, not the code: %s must rank below 5e-7, the value six decimal "+
				"places print as 0.000000, or this test no longer covers a months-old thread", uri)
	}

	var visited []string
	var cursor *string
	for requests := 0; ; requests++ {
		require.Less(t, requests, 20, "the cursor stopped advancing: this thread never ends")
		page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, cursor, "")
		require.NoError(t, err)
		visited = append(visited, commentURIs(page)...)
		if next == nil {
			break
		}
		cursor = next
	}

	assert.ElementsMatch(t, all, visited,
		"every reply on a 70-day-old thread must be reachable under the default sort. A reply on no "+
			"page has been silently removed from the thread; one on two pages makes the conversation "+
			"look duplicated")
	assert.Len(t, visited, len(all))

	single, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 50, nil, "")
	require.NoError(t, err)
	assert.Equal(t, commentURIs(single), visited,
		"and the paged order must match the unpaged one, or the reader sees the thread reshuffle "+
			"as they scroll")
}

// TestCommentRepo_HotPaginationSurvivesSixDecimalRounding guards hot
// pagination, the default sort for a thread, on a fresh thread whose replies
// rank less than a millionth apart.
//
// Issue: 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md. This is the
// partial-loss half of the issue. The total loss on a months-old thread is
// TestCommentRepo_HotPaginationReachesEveryReplyOnAMonthsOldThread.
//
// A page boundary whose rank is kept to six decimal places is rounded to the
// nearest millionth, and the next page asks for replies ranked strictly below
// the rounded value. Rounded down, a reply ranked between the rounded value and
// the boundary's true rank is on neither page. Rounded up, a reply already
// served, ranked between the true rank and the rounded value, is served again.
// Replies tied on rank are lost or repeated the same way. None of this depends
// on the thread's age.
//
// Why the replies are dated in the future: the rank SQL clamps a negative age to
// zero, so a future-dated reply ranks log10(score+2) / 2^1.8 on every request, a
// function of its score alone. A reply dated now loses rank continuously, so its
// seventh decimal place, and with it the direction six decimals round it, is
// different on every request. Scores near one million put neighbouring ranks
// about 1.25e-7 apart, inside one millionth, and the scores here were picked by
// the direction their rank rounds. Each subtest checks that precondition against
// the rank the database computes before it pages, so a fixture that stops
// meeting it fails as a fixture error instead of passing for the wrong reason.
func TestCommentRepo_HotPaginationSurvivesSixDecimalRounding(t *testing.T) {
	t.Parallel()

	// frozenRank reads a reply's rank from the database, after checking that the
	// database clock puts the reply far enough in the future for the age clamp
	// to hold the rank still for the rest of the test.
	frozenRank := func(t *testing.T, env *commentEnv, uri string) float64 {
		t.Helper()
		var future bool
		require.NoError(t, env.db.QueryRowContext(env.ctx,
			`SELECT created_at > NOW() + INTERVAL '10 minutes' FROM comments WHERE uri = $1`, uri).Scan(&future))
		require.True(t, future, "THE FIXTURE IS WRONG, not the code: %s must be dated in the future by "+
			"the database clock, or its rank decays between requests and its seventh decimal place is "+
			"not reproducible", uri)
		return commentHotRankOf(t, env, uri)
	}

	// sixDecimals is a rank as six decimal places print it and read it back:
	// rounded to the nearest millionth.
	sixDecimals := func(t *testing.T, rank float64) float64 {
		t.Helper()
		rounded, err := strconv.ParseFloat(fmt.Sprintf("%f", rank), 64)
		require.NoError(t, err)
		return rounded
	}

	walkHot := func(t *testing.T, env *commentEnv, limit int, cursor *string) []string {
		t.Helper()
		var visited []string
		for requests := 0; ; requests++ {
			require.Less(t, requests, 20, "the cursor stopped advancing: this thread never ends")
			page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", limit, cursor, "")
			require.NoError(t, err)
			visited = append(visited, commentURIs(page)...)
			if next == nil {
				return visited
			}
			cursor = next
		}
	}

	assertEveryReplyOnceInUnpagedOrder := func(t *testing.T, env *commentEnv, all, visited []string, consequence string) {
		t.Helper()
		assert.ElementsMatch(t, all, visited, consequence)
		assert.Len(t, visited, len(all))

		single, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 50, nil, "")
		require.NoError(t, err)
		assert.Equal(t, commentURIs(single), visited,
			"and the paged order must match the unpaged one, or the reader sees the thread reshuffle "+
				"as they scroll")
	}

	// Page one holds only the boundary, and its rank rounds down to below the
	// second reply's rank.
	t.Run("a reply ranked just below a boundary that rounds down is on the next page", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		future := time.Now().UTC().Add(time.Hour)
		boundary := env.seed(commentSpec{rkey: "boundary", score: 1000003, createdAt: future})
		justBelow := env.seed(commentSpec{rkey: "justbelow", score: 1000002, createdAt: future})

		boundaryRank := frozenRank(t, env, boundary)
		justBelowRank := frozenRank(t, env, justBelow)
		require.Less(t, sixDecimals(t, boundaryRank), justBelowRank,
			"THE FIXTURE IS WRONG, not the code: the boundary's rank must round DOWN at six decimal "+
				"places, to below the second reply's rank")
		require.Less(t, justBelowRank, boundaryRank,
			"THE FIXTURE IS WRONG, not the code: the second reply must rank below the boundary, so "+
				"it belongs on page two")

		visited := walkHot(t, env, 1, nil)
		assertEveryReplyOnceInUnpagedOrder(t, env, []string{boundary, justBelow}, visited,
			"a reply ranked less than a millionth below the reply that ends page one must be on "+
				"page two. A reply on no page has been silently removed from the thread")
	})

	// Page one holds the first reply and the boundary, and the boundary's rank
	// rounds up to above the first reply's rank. The third reply ranks well
	// below both, so there is a page two.
	t.Run("a reply ranked just above a boundary that rounds up is not served again", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		future := time.Now().UTC().Add(time.Hour)
		justAbove := env.seed(commentSpec{rkey: "justabove", score: 1000001, createdAt: future})
		boundary := env.seed(commentSpec{rkey: "boundary", score: 1000000, createdAt: future})
		lower := env.seed(commentSpec{rkey: "lower", score: 0, createdAt: future})

		justAboveRank := frozenRank(t, env, justAbove)
		boundaryRank := frozenRank(t, env, boundary)
		lowerRank := frozenRank(t, env, lower)
		require.Less(t, boundaryRank, justAboveRank,
			"THE FIXTURE IS WRONG, not the code: the first reply must rank above the boundary, so "+
				"it belongs on page one")
		require.Less(t, justAboveRank, sixDecimals(t, boundaryRank),
			"THE FIXTURE IS WRONG, not the code: the boundary's rank must round UP at six decimal "+
				"places, to above the first reply's rank")
		require.Less(t, lowerRank, boundaryRank,
			"THE FIXTURE IS WRONG, not the code: the third reply must rank below the boundary, so "+
				"page one ends on the boundary and there is a page two")

		first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, nil, "")
		require.NoError(t, err)
		require.Equal(t, []string{justAbove, boundary}, commentURIs(first),
			"page one is the two highest-ranked replies, ending on the boundary")
		require.NotNil(t, cursor, "there are three replies and the page holds two")

		visited := append(commentURIs(first), walkHot(t, env, 2, cursor)...)
		assertEveryReplyOnceInUnpagedOrder(t, env, []string{justAbove, boundary, lower}, visited,
			"a reply ranked less than a millionth above the reply that ends page one was served on "+
				"page one. Serving it again on page two makes the conversation look duplicated")
	})

	// Three replies with the same score, dated an hour apart and all in the
	// future, rank exactly alike, so created_at orders them. Any rank six
	// decimals cannot hold would do. This one rounds down, which loses the
	// replies after page one; one that rounded up would serve them again in a
	// loop that never ends. Their rkeys sort in the reverse of their created_at
	// order, so a boundary filter that compared uri before created_at would lose
	// the replies after page one rather than keep them by coincidence.
	t.Run("replies tied on rank and score are split across pages by created_at", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		future := time.Now().UTC().Add(time.Hour)
		var all []string
		for i, rkey := range []string{"tiec", "tieb", "tiea"} {
			all = append(all, env.seed(commentSpec{
				rkey: rkey, score: 1000003, createdAt: future.Add(time.Duration(i) * time.Hour),
			}))
		}
		for i := 1; i < len(all); i++ {
			require.Greater(t, all[i-1], all[i],
				"THE FIXTURE IS WRONG, not the code: %s was created before %s, so its uri must sort after "+
					"it; uri order must be the reverse of created_at order", all[i-1], all[i])
		}

		tiedRank := frozenRank(t, env, all[0])
		for _, uri := range all[1:] {
			require.Equal(t, tiedRank, frozenRank(t, env, uri),
				"THE FIXTURE IS WRONG, not the code: %s must rank exactly like the other replies, so "+
					"created_at decides their order", uri)
		}
		require.NotEqual(t, tiedRank, sixDecimals(t, tiedRank),
			"THE FIXTURE IS WRONG, not the code: the tied rank must be one six decimal places cannot "+
				"hold exactly")

		visited := walkHot(t, env, 1, nil)
		assertEveryReplyOnceInUnpagedOrder(t, env, all, visited,
			"replies tied on rank and score are ordered by created_at, and every one of them must be "+
				"on exactly one page. A reply on no page has been silently removed from the thread; one "+
				"on two pages makes the conversation look duplicated")
	})

	// A reply scored 0 and one scored -2, dated one and two hours in the future,
	// rank exactly alike: the rank's numerator is log10(greatest(2, score + 2)),
	// the same for every score of 0 or below, and the age clamp holds both at
	// age zero. Score orders them, the reply scored 0 first, while created_at and
	// uri both order them the other way, so a boundary filter that compared
	// created_at or uri before score would lose the second reply.
	t.Run("replies tied on rank are split across pages by score before created_at", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		future := time.Now().UTC().Add(time.Hour)
		earlier := env.seed(commentSpec{rkey: "earlier", createdAt: future})
		later := env.seed(commentSpec{rkey: "later", createdAt: future.Add(time.Hour)})
		env.setNativeVotes(later, 0, 2)

		tiedRank := frozenRank(t, env, earlier)
		require.Equal(t, tiedRank, frozenRank(t, env, later),
			"THE FIXTURE IS WRONG, not the code: a reply scored 0 and one scored -2 must rank exactly "+
				"alike, so score decides their order")
		require.NotEqual(t, tiedRank, sixDecimals(t, tiedRank),
			"THE FIXTURE IS WRONG, not the code: the tied rank must be one six decimal places cannot "+
				"hold exactly")
		require.Greater(t, later, earlier,
			"THE FIXTURE IS WRONG, not the code: the reply scored -2 must have the greater uri, so uri "+
				"order disagrees with score order as created_at order does")

		visited := walkHot(t, env, 1, nil)
		assertEveryReplyOnceInUnpagedOrder(t, env, []string{earlier, later}, visited,
			"replies tied on rank are ordered by score before created_at, and each must be on exactly "+
				"one page. A reply on no page has been silently removed from the thread")
		assert.Equal(t, []string{earlier, later}, visited, "of replies tied on rank, the higher score comes first")
	})
}

// TestCommentRepo_UnknownSortCursorRepeatsPageOne is the end-to-end half of the
// unrecognised-sort defect pinned in
// TestCommentRepo_UnknownSortCursorIsSilentlyDiscarded.
//
// The ORDER BY defaults to hot, so page one looks right and hands back a
// cursor. The cursor parser's default arm discards it, so page two is page one.
// A client that scrolls until the cursor is nil never stops, and every page it
// renders is the same three comments.
//
// REACHABILITY: no production caller can reach this today.
// comment_service.go:1327-1333 validates Sort against {hot, top, new} and
// returns an error before the repository is called, so an unrecognised sort
// dies one layer up. This is pinned anyway because the guard is in a DIFFERENT
// package from the two functions that disagree, nothing enforces the
// relationship, and the defect goes live the moment a fourth sort is added to
// the service's allow-list without both repository arms learning it — which is
// exactly the change someone will make when they add "controversial".
//
// Issue: 2026-07-31-comment-repo-unrecognised-sort-trio.md
func TestCommentRepo_UnknownSortCursorRepeatsPageOne(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	for i, rkey := range []string{"u1", "u2", "u3"} {
		env.seed(commentSpec{rkey: rkey, score: 30 - i*10, createdAt: now.Add(-time.Duration(i) * time.Minute)})
	}

	first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "confidence", "", 2, nil, "")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.NotNil(t, cursor)

	second, nextCursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "confidence", "", 2, cursor, "")
	require.NoError(t, err)
	assert.Equal(t, commentURIs(first), commentURIs(second),
		"page two of an unrecognised sort should either advance or be refused as an invalid cursor. "+
			"Instead the cursor is discarded and page one is served again, forever. "+
			"IF THIS FAILED (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md) the defect is FIXED — delete this pin")
	require.NotNil(t, nextCursor, "and it keeps offering a cursor, so a client that scrolls until "+
		"the cursor is nil never stops")
	assert.Equal(t, *cursor, *nextCursor)
}

// TestCommentRepo_HotCursorRejectsNonNumericAndRankBearingFields guards the hot
// cursor parser against the forged and stale values it used to accept.
//
// Issue 2026-07-31-repo-minor-pins-batch.md, item 4. The hot cursor's first
// field was a rank read with fmt.Sscanf("%f"), which takes "NaN" and "Inf" as
// floats and reads a numeric prefix while ignoring trailing garbage; the score
// was read with Sscanf("%d") the same way, so "30zzz" was 30. A NaN rank made
// the filter admit the whole thread, serving the reader a page they had already
// read. The first field is now the instant page one was ranked at, so every
// value that was a rank — NaN, Inf, a decimal, a cursor minted before the change
// — is a timestamp that does not parse and must be refused, and the score must
// be a whole integer.
//
// A cursor must also carry nothing the query cannot compute with. A score of
// 2147483646 or 2147483647 is an SQL integer, but the rank's score + 2
// overflows it; a timestamp in year 0000 parses in Go but not in Postgres,
// whether it is written so or falls there once converted to UTC. Accepted, each
// made the query fail, and the reader got a 500 instead of a 400.
//
// Every cursor keeps its other three fields valid and taken from a reply in the
// thread, so a refusal can only come from the field under test. The thread has
// replies so that a cursor the repository wrongly accepted would visibly serve a
// page rather than an empty one.
func TestCommentRepo_HotCursorRejectsNonNumericAndRankBearingFields(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	boundary := env.seed(commentSpec{rkey: "n1", score: 30, createdAt: now})
	env.seed(commentSpec{rkey: "n2", score: 20, createdAt: now.Add(-time.Minute)})
	createdAt := now.Format(time.RFC3339Nano)
	rankedAt := now.Add(time.Second).Format(time.RFC3339Nano)

	for name, plaintext := range map[string]string{
		"NaN in the rankedAt slot":                              "NaN|30|" + createdAt + "|" + boundary,
		"Inf in the rankedAt slot":                              "Inf|30|" + createdAt + "|" + boundary,
		"-Inf in the rankedAt slot":                             "-Inf|30|" + createdAt + "|" + boundary,
		"a decimal in the rankedAt slot":                        "0.5|30|" + createdAt + "|" + boundary,
		"a decimal with trailing garbage in the rankedAt slot":  "0.5xyz|30|" + createdAt + "|" + boundary,
		"a six-decimal rank from a cursor minted before":        "0.000000|30|" + createdAt + "|" + boundary,
		"a full-precision rank from a cursor minted before":     "1.2e-07|30|" + createdAt + "|" + boundary,
		"a score with trailing garbage after a valid rankedAt":  rankedAt + "|30zzz|" + createdAt + "|" + boundary,
		"a decimal score after a valid rankedAt":                rankedAt + "|30.5|" + createdAt + "|" + boundary,
		"a rank-shaped cursor whose score has trailing garbage": "0.5xyz|30zzz|" + createdAt + "|" + boundary,
		"a score with no room for the rank's +2":                rankedAt + "|2147483646|" + createdAt + "|" + boundary,
		"the SQL integer maximum as the score":                  rankedAt + "|2147483647|" + createdAt + "|" + boundary,
		"a rankedAt in year 0000":                               "0000-01-01T00:00:00Z|30|" + createdAt + "|" + boundary,
		"a createdAt in year 0000":                              rankedAt + "|30|0000-01-01T00:00:00Z|" + boundary,
		"a rankedAt in year 0000 once converted to UTC":         "0001-01-01T00:30:00+01:00|30|" + createdAt + "|" + boundary,
		"a createdAt in year 0000 once converted to UTC":        rankedAt + "|30|0001-01-01T00:30:00+01:00|" + boundary,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			cursor := commentEncodeCursor(plaintext)

			_, _, err := env.impl.parseCommentCursor(&cursor, "hot")
			assert.ErrorIsf(t, err, comments.ErrInvalidCursor,
				"the parser must refuse %q as bad client input; accepting it pages the thread from a "+
					"key that is not the instant page one was ranked at", plaintext)

			page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 10, &cursor, "")
			assert.Truef(t, errors.Is(err, comments.ErrInvalidCursor),
				"the listing must refuse %q with comments.ErrInvalidCursor, which is what makes it a 400; "+
					"got error %v", plaintext, err)
			assert.Empty(t, page, "a refused cursor must not serve a page")
			assert.Nil(t, next, "a refused cursor must not hand out another")
		})
	}
}

// TestCommentRepo_HotCursorServesEquivalentTimestampSpellingsAlike guards the
// spelling of the hot cursor's two timestamps on their way to Postgres.
//
// Go's RFC3339 parser reads spellings Postgres refuses: a zone offset from
// ±16:00 to ±24:00, and a comma before the fractional second. A cursor whose
// timestamps were bound as written made the query fail, and the reader got a
// 500 for a cursor that names real instants. Each timestamp is bound as its own
// UTC text instead, so a cursor that spells rankedAt or createdAt another way is
// served exactly like the cursor that spells them canonically.
//
// Why this fixture: scores fall as age rises, so the hot order at rankedAt T is
// plain. Every cursor names "boundary" as of T, one reply a page, so the page is
// "third" and the next cursor names it, which the canonical cursor confirms. T
// and the boundary's created_at both have a fractional second, so the comma has
// a fraction to stand before. Each spelling changes one field, so a failure
// names the field whose spelling reached Postgres.
func TestCommentRepo_HotCursorServesEquivalentTimestampSpellingsAlike(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	rankedAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour + 250*time.Millisecond)
	boundaryCreatedAt := rankedAt.Add(-20*time.Minute + 123456*time.Microsecond)
	leader := env.seed(commentSpec{rkey: "leader", score: 100, createdAt: rankedAt.Add(-10 * time.Minute)})
	boundary := env.seed(commentSpec{rkey: "boundary", score: 50, createdAt: boundaryCreatedAt})
	third := env.seed(commentSpec{rkey: "third", score: 20, createdAt: rankedAt.Add(-30 * time.Minute)})
	fourth := env.seed(commentSpec{rkey: "fourth", score: 10, createdAt: rankedAt.Add(-40 * time.Minute)})
	require.Equal(t, []string{leader, boundary, third, fourth}, commentHotOrderAt(t, env, rankedAt),
		"THE FIXTURE IS WRONG, not the code: ranked at T the order must be leader, boundary, third, fourth")

	cursorOf := func(rankedAtText, createdAtText string) string {
		return commentEncodeCursor(fmt.Sprintf("%s|50|%s|%s", rankedAtText, createdAtText, boundary))
	}
	canonicalRankedAt := rankedAt.Format(time.RFC3339Nano)
	canonicalCreatedAt := boundaryCreatedAt.Format(time.RFC3339Nano)
	canonical := cursorOf(canonicalRankedAt, canonicalCreatedAt)

	wantPage, wantNext, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 1, &canonical, "")
	require.NoError(t, err)
	require.Equal(t, []string{third}, commentURIs(wantPage),
		"THE FIXTURE IS WRONG, not the code: after the boundary, ranked at T, the page must be third")
	require.NotNil(t, wantNext, "THE FIXTURE IS WRONG, not the code: fourth follows third, so there is a next cursor")

	plusTwenty := time.FixedZone("", 20*60*60)
	for name, cursor := range map[string]string{
		"rankedAt with a +20:00 offset":              cursorOf(rankedAt.In(plusTwenty).Format(time.RFC3339Nano), canonicalCreatedAt),
		"createdAt with a +20:00 offset":             cursorOf(canonicalRankedAt, boundaryCreatedAt.In(plusTwenty).Format(time.RFC3339Nano)),
		"rankedAt with a comma before its fraction":  cursorOf(strings.Replace(canonicalRankedAt, ".", ",", 1), canonicalCreatedAt),
		"createdAt with a comma before its fraction": cursorOf(canonicalRankedAt, strings.Replace(canonicalCreatedAt, ".", ",", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NotEqual(t, commentDecodeCursor(t, canonical), commentDecodeCursor(t, cursor),
				"THE FIXTURE IS WRONG, not the code: the spelling must differ from the canonical cursor's")

			page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 1, &cursor, "")
			require.NoError(t, err, "a cursor naming the same instants in another RFC3339 spelling is valid "+
				"input; a query error here is a 500 for a cursor the parser accepted")
			assert.Equal(t, commentURIs(wantPage), commentURIs(page),
				"the same instants must page the thread from the same place")
			require.NotNil(t, next, "the canonical cursor's page hands out a next cursor, so this one must too")
			assert.Equal(t, commentDecodeCursor(t, *wantNext), commentDecodeCursor(t, *next),
				"the next cursor must carry rankedAt in the same UTC spelling the canonical cursor's does")
		})
	}
}

// commentHotRankAt computes a reply's rank as the listing ranks it at a given
// instant, through the same builder, so a fixture can be checked against the
// clock a cursor pins rather than against whenever the check happens to run.
func commentHotRankAt(t *testing.T, env *commentEnv, uri string, at time.Time) float64 {
	t.Helper()
	var rank float64
	query := fmt.Sprintf(`SELECT %s FROM comments c WHERE c.uri = $1`, commentHotRankSQL("c", "$2::timestamptz"))
	require.NoError(t, env.db.QueryRowContext(env.ctx, query, uri, at).Scan(&rank))
	return rank
}

// commentHotOrderAt lists the thread's direct replies in hot order as ranked at
// a given instant, with the listing's tiebreaks.
func commentHotOrderAt(t *testing.T, env *commentEnv, at time.Time) []string {
	t.Helper()
	query := fmt.Sprintf(`
		SELECT c.uri FROM comments c WHERE c.parent_uri = $1
		ORDER BY %s DESC, c.score DESC, c.created_at DESC, c.uri DESC
	`, commentHotRankSQL("c", "$2::timestamptz"))
	rows, err := env.db.QueryContext(env.ctx, query, env.root, at)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var uris []string
	for rows.Next() {
		var uri string
		require.NoError(t, rows.Scan(&uri))
		uris = append(uris, uri)
	}
	require.NoError(t, rows.Err())
	return uris
}

// commentHotWalk pages the thread under hot from the given cursor (nil for page
// one) until the cursor runs out, and returns every reply in visit order and
// every cursor it was handed. A walk still going after twenty requests is
// reported and cut off, so a cursor that stopped advancing fails the test
// instead of hanging it, and the replies served so far still reach the
// exactly-once assertions, which then show what was repeated.
func commentHotWalk(t *testing.T, env *commentEnv, limit int, cursor *string, viewerDID string) (visited, cursors []string) {
	t.Helper()
	const maxRequests = 20
	for requests := 0; requests < maxRequests; requests++ {
		page, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", limit, cursor, viewerDID)
		require.NoError(t, err)
		visited = append(visited, commentURIs(page)...)
		if next == nil {
			return visited, cursors
		}
		cursors = append(cursors, *next)
		cursor = next
	}
	assert.Failf(t, "the cursor stopped advancing: this thread never ends",
		"the walk was cut off after %d requests, having served %d replies", maxRequests, len(visited))
	return visited, cursors
}

// commentAssertHotWalkMatchesOneRequest checks a finished hot walk against the
// thread read in one request by the same viewer: every reply on exactly one
// page, in the order the single request gives.
func commentAssertHotWalkMatchesOneRequest(t *testing.T, env *commentEnv, want, visited []string, viewerDID string) {
	t.Helper()
	assert.ElementsMatch(t, want, visited,
		"a reply on no page has been silently removed from the thread; one on two pages makes the "+
			"conversation look duplicated")
	assert.Len(t, visited, len(want))

	single, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 50, nil, viewerDID)
	require.NoError(t, err)
	assert.Equal(t, commentURIs(single), visited,
		"and the paged order must match the unpaged one, or the reader sees the thread reshuffle as "+
			"they scroll")
}

// commentRankedAtOf reads the rankedAt field, the first, out of a hot cursor.
func commentRankedAtOf(t *testing.T, cursor string) string {
	t.Helper()
	return strings.Split(commentDecodeCursor(t, cursor), "|")[0]
}

// TestCommentRepo_HotPaginationVisitsFreshTiesExactlyOnce guards hot pagination
// over replies tied on rank while their rank decays between requests.
//
// Issue: 2026-08-30-comment-hot-cursor-moving-now.md. When each page of a hot
// walk was ranked at its own request time, every rank had decayed a little by
// the next request, so every reply ranked strictly below the rank the cursor
// carried, the replies already served included; only the reply that ended the
// previous page was held back. Replies tied on rank then came back in a loop:
// the first, the second, the first again, forever. Ranking every page of a walk
// as of the instant page one was ranked at keeps a tie a tie for the whole
// walk, so the tiebreak decides and each reply is served once.
//
// Why these fixtures: the replies were created a minute ago, so their rank is
// large and falls measurably between two requests. A reply dated in the future
// has its age clamped to zero and a rank that never moves (the fixtures of
// TestCommentRepo_HotPaginationSurvivesSixDecimalRounding), which would hide the
// defect. Each subtest checks from the database that the ranks are equal at one
// instant and lower a second later. A page size of one makes every reply a page
// boundary.
func TestCommentRepo_HotPaginationVisitsFreshTiesExactlyOnce(t *testing.T) {
	t.Parallel()

	requireTiedAndDecaying := func(t *testing.T, env *commentEnv, uris []string) {
		t.Helper()
		at := time.Now().UTC()
		tiedRank := commentHotRankAt(t, env, uris[0], at)
		for _, uri := range uris[1:] {
			require.Equalf(t, tiedRank, commentHotRankAt(t, env, uri, at),
				"THE FIXTURE IS WRONG, not the code: %s must rank exactly like the other replies at one "+
					"instant, so the tiebreak decides their order", uri)
		}
		require.Less(t, commentHotRankAt(t, env, uris[0], at.Add(time.Second)), tiedRank,
			"THE FIXTURE IS WRONG, not the code: the tied rank must fall as time passes, or a page ranked "+
				"later than page one sees the same rank and the defect is not exercised")
	}

	// Four replies with one score and one created_at, so the uri tiebreak alone
	// orders them. They are inserted in the opposite order to uri DESC, so a
	// query that lost its tiebreak cannot come out right on physical row order.
	t.Run("same score and created_at, ordered by uri", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		createdAt := time.Now().UTC().Add(-time.Minute)
		var all []string
		for _, rkey := range []string{"tiea", "tieb", "tiec", "tied"} {
			all = append(all, env.seed(commentSpec{rkey: rkey, score: 5, createdAt: createdAt}))
		}
		requireTiedAndDecaying(t, env, all)

		visited, _ := commentHotWalk(t, env, 1, nil, "")
		commentAssertHotWalkMatchesOneRequest(t, env, all, visited, "")
	})

	// The rank's numerator is log10(greatest(2, score + 2)), so every score of 0
	// or below gives the same numerator. Scores 0, -1 and -3 on one created_at
	// therefore rank alike and the score tiebreak orders them. Their uris sort
	// in a different order (zero, minusthree, minusone), so an order that fell
	// through to the uri tiebreak would not match.
	t.Run("clamped scores on one created_at, ordered by score", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		createdAt := time.Now().UTC().Add(-time.Minute)
		zero := env.seed(commentSpec{rkey: "zero", createdAt: createdAt})
		minusOne := env.seed(commentSpec{rkey: "minusone", createdAt: createdAt})
		env.setNativeVotes(minusOne, 0, 1)
		minusThree := env.seed(commentSpec{rkey: "minusthree", createdAt: createdAt})
		env.setNativeVotes(minusThree, 0, 3)
		all := []string{zero, minusOne, minusThree}
		requireTiedAndDecaying(t, env, all)

		visited, _ := commentHotWalk(t, env, 1, nil, "")
		commentAssertHotWalkMatchesOneRequest(t, env, all, visited, "")
		assert.Equal(t, all, visited, "replies tied on rank are ordered by score, highest first")
	})
}

// TestCommentRepo_HotCursorRanksLaterPagesAsOfRankedAt guards the instant a hot
// cursor pins: every page after the first is ranked, in its ORDER BY and in its
// boundary filter alike, as of the cursor's rankedAt, and the cursor it hands
// out carries the same rankedAt forward unchanged.
//
// Issue: 2026-08-30-comment-hot-cursor-moving-now.md.
//
// Why this fixture: two replies swap places between a past instant T and now.
// "older" is five days old with 1000 points, "younger" four days old with none.
// At T, half an hour after "younger" was posted, its youth beats the other's
// score; now, four days on, the decay has flattened the age difference and the
// score wins. "boundary" was posted a minute before T with 1000 points, so at T
// it ranks above both and is a reply page one of a walk ranked at T could have
// ended on. Walking from a cursor that names it with rankedAt = T, one reply a
// page, serves "younger" then "older" and ends. Each way of getting the clock
// wrong gives a different answer:
//   - a page ordered by the request's own time serves "older" first;
//   - a boundary filter at the request's own time finds nothing ranked below
//     "younger" now, and loses "older";
//   - a cursor that re-mints rankedAt after "younger" does the same on the
//     next page.
func TestCommentRepo_HotCursorRanksLaterPagesAsOfRankedAt(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T) (env *commentEnv, rankedAt time.Time, boundary, older, younger string) {
		t.Helper()
		env = commentEnvFor(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		rankedAt = now.Add(-4*24*time.Hour + 30*time.Minute)
		older = env.seed(commentSpec{rkey: "older", score: 1000, createdAt: now.Add(-5 * 24 * time.Hour)})
		younger = env.seed(commentSpec{rkey: "younger", score: 0, createdAt: now.Add(-4 * 24 * time.Hour)})
		boundary = env.seed(commentSpec{rkey: "boundary", score: 1000, createdAt: rankedAt.Add(-time.Minute)})

		require.Equal(t, []string{boundary, younger, older}, commentHotOrderAt(t, env, rankedAt),
			"THE FIXTURE IS WRONG, not the code: ranked at T the order must be boundary, younger, older")
		require.Equal(t, []string{boundary, older, younger}, commentHotOrderAt(t, env, time.Now().UTC()),
			"THE FIXTURE IS WRONG, not the code: ranked now, older must rank above younger, or ranking "+
				"at the wrong instant gives the same answer as ranking at T")
		return env, rankedAt, boundary, older, younger
	}

	t.Run("a walk from a cursor ranked at T is ranked at T to its end", func(t *testing.T) {
		t.Parallel()
		env, rankedAt, boundary, older, younger := seed(t)

		var score int
		var createdAt time.Time
		require.NoError(t, env.db.QueryRowContext(env.ctx,
			`SELECT score, created_at FROM comments WHERE uri = $1`, boundary).Scan(&score, &createdAt))
		rankedAtText := rankedAt.Format(time.RFC3339Nano)
		cursor := commentEncodeCursor(fmt.Sprintf("%s|%d|%s|%s",
			rankedAtText, score, createdAt.UTC().Format(time.RFC3339Nano), boundary))

		visited, cursors := commentHotWalk(t, env, 1, &cursor, "")
		assert.Equal(t, []string{younger, older}, visited,
			"after a boundary ranked at T, the rest of the thread must be served in its order at T "+
				"and in full. Older first means the page was ranked at the request's own time; older "+
				"missing means a filter or a later cursor was")
		require.Len(t, cursors, 1, "one page follows the one that ends on younger, and it is the last")
		assert.Equal(t, rankedAtText, commentRankedAtOf(t, cursors[0]),
			"the cursor after younger must carry T forward exactly as it arrived; a walk whose clock "+
				"moves between pages ranks the same reply differently on each")
	})

	t.Run("every cursor of a walk from page one carries the same rankedAt", func(t *testing.T) {
		t.Parallel()
		env, _, boundary, older, younger := seed(t)

		visited, cursors := commentHotWalk(t, env, 1, nil, "")
		commentAssertHotWalkMatchesOneRequest(t, env, []string{boundary, older, younger}, visited, "")
		require.Len(t, cursors, 2, "three replies one to a page hand out two cursors")

		first := commentRankedAtOf(t, cursors[0])
		_, err := time.Parse(time.RFC3339Nano, first)
		assert.NoErrorf(t, err, "rankedAt %q must be an RFC3339 timestamp with fractional seconds", first)
		for i, cursor := range cursors {
			assert.Equalf(t, first, commentRankedAtOf(t, cursor),
				"cursor %d must carry the instant page one was ranked at, unchanged; a walk whose clock "+
					"moves between pages ranks the same reply differently on each", i+1)
		}
	})
}

// TestCommentRepo_HotPaginationHonoursViewerBlocksOnEveryPage guards the viewer
// block filter across a hot walk.
//
// The filter binds the viewer's DID at a parameter index that comes after the
// cursor's values, so the index moves whenever the cursor's shape changes. Page
// one carries no cursor and passes with any shape; only a later page shows
// whether the viewer is bound where the filter reads it. A page size of one
// makes every page after the first a cursored one.
//
// Why this fixture: scores fall as age rises, so hot order is plain, and the
// blocked author's two replies sit second and fourth in it, next to a page
// boundary on both sides. The replies of the other two authors must each be on
// exactly one page: a filter that hid too much is as wrong as one that hid
// nothing.
func TestCommentRepo_HotPaginationHonoursViewerBlocksOnEveryPage(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	cast := seedBlockFilterCast(t, env.db, "hotwalk")
	now := time.Now().UTC()

	first := env.seed(commentSpec{rkey: "author1", score: 50, createdAt: now.Add(-1 * time.Minute)})
	blockedFirst := env.seed(commentSpec{rkey: "blocked1", author: cast.blocked, score: 40, createdAt: now.Add(-2 * time.Minute)})
	thirdPartyFirst := env.seed(commentSpec{rkey: "third1", author: cast.thirdParty, score: 30, createdAt: now.Add(-3 * time.Minute)})
	blockedSecond := env.seed(commentSpec{rkey: "blocked2", author: cast.blocked, score: 20, createdAt: now.Add(-4 * time.Minute)})
	thirdPartySecond := env.seed(commentSpec{rkey: "third2", author: cast.thirdParty, score: 10, createdAt: now.Add(-5 * time.Minute)})
	last := env.seed(commentSpec{rkey: "author2", score: 5, createdAt: now.Add(-6 * time.Minute)})

	anonymous, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 50, nil, "")
	require.NoError(t, err)
	require.Equal(t,
		[]string{first, blockedFirst, thirdPartyFirst, blockedSecond, thirdPartySecond, last}, commentURIs(anonymous),
		"THE FIXTURE IS WRONG, not the code: before the block the thread must hold every reply, with "+
			"the blocked author's second and fourth")

	insertUserBlock(t, env.db, cast.viewer, cast.blocked)

	visited, _ := commentHotWalk(t, env, 1, nil, cast.viewer)
	assert.NotContains(t, visited, blockedFirst, "a blocked author's reply must stay hidden on every page")
	assert.NotContains(t, visited, blockedSecond, "a blocked author's reply must stay hidden on every page")
	commentAssertHotWalkMatchesOneRequest(t, env,
		[]string{first, thirdPartyFirst, thirdPartySecond, last}, visited, cast.viewer)
}

// TestCommentRepo_HotPaginationDoesNotRepeatADownvotedBoundary guards the reply
// a page ended on when it is downvoted before the next request.
//
// A hot walk ranks every row with its current score. Once the reply that ended
// page one is downvoted, it ranks below the replies still to come, so without
// the explicit exclusion of the reply the cursor names, page two would serve it
// a second time.
//
// Why this fixture: scores fall as age rises, so the hot order is plain and page
// one (two replies) ends on "boundary". Its score then drops to -1000, which
// ranks it below every other reply. The rest of the thread fits on page two,
// because the exclusion covers the request that follows the boundary: on a
// later page the cursor names a different reply, and a changed reply ranked
// below it is outside the exactly-once promise.
func TestCommentRepo_HotPaginationDoesNotRepeatADownvotedBoundary(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	leader := env.seed(commentSpec{rkey: "leader", score: 100, createdAt: now.Add(-1 * time.Minute)})
	boundary := env.seed(commentSpec{rkey: "boundary", score: 50, createdAt: now.Add(-2 * time.Minute)})
	third := env.seed(commentSpec{rkey: "third", score: 20, createdAt: now.Add(-3 * time.Minute)})
	fourth := env.seed(commentSpec{rkey: "fourth", score: 10, createdAt: now.Add(-4 * time.Minute)})

	first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, nil, "")
	require.NoError(t, err)
	require.Equal(t, []string{leader, boundary}, commentURIs(first),
		"THE FIXTURE IS WRONG, not the code: page one must end on the boundary")
	require.NotNil(t, cursor, "THE FIXTURE IS WRONG, not the code: four replies two to a page have a page two")

	env.setNativeVotes(boundary, 0, 1000)
	at := time.Now().UTC()
	boundaryRank := commentHotRankAt(t, env, boundary, at)
	for _, uri := range []string{leader, third, fourth} {
		require.Lessf(t, boundaryRank, commentHotRankAt(t, env, uri, at),
			"THE FIXTURE IS WRONG, not the code: the downvoted boundary must rank below %s, or nothing "+
				"would bring it back after page one", uri)
	}

	rest, _ := commentHotWalk(t, env, 2, cursor, "")
	assert.NotContains(t, rest, boundary,
		"the reply page one ended on must not be served again after it is downvoted")
	assert.Equal(t, []string{third, fourth}, rest,
		"the replies after the boundary must each be served once, in order")
	visited := append(commentURIs(first), rest...)
	assert.ElementsMatch(t, []string{leader, boundary, third, fourth}, visited)
	assert.Len(t, visited, 4)
}

// TestCommentRepo_HotPaginationSurvivesADeletedBoundary guards a hot walk whose
// boundary reply is hard-deleted before the next request.
//
// The cursor has to carry everything the next page's boundary filter needs. A
// filter that looked the boundary reply up by its URI, the way the post feeds
// look up their rank, would find nothing once the row is gone, and the walk
// would end early with no error.
//
// Why this fixture: scores fall as age rises, so the hot order is plain and page
// one (two replies) ends on "boundary". Three replies follow it, more than one
// page, so the walk crosses a second boundary after the deletion.
func TestCommentRepo_HotPaginationSurvivesADeletedBoundary(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	leader := env.seed(commentSpec{rkey: "leader", score: 100, createdAt: now.Add(-1 * time.Minute)})
	boundary := env.seed(commentSpec{rkey: "boundary", score: 50, createdAt: now.Add(-2 * time.Minute)})
	third := env.seed(commentSpec{rkey: "third", score: 20, createdAt: now.Add(-3 * time.Minute)})
	fourth := env.seed(commentSpec{rkey: "fourth", score: 10, createdAt: now.Add(-4 * time.Minute)})
	fifth := env.seed(commentSpec{rkey: "fifth", score: 5, createdAt: now.Add(-5 * time.Minute)})

	first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 2, nil, "")
	require.NoError(t, err)
	require.Equal(t, []string{leader, boundary}, commentURIs(first),
		"THE FIXTURE IS WRONG, not the code: page one must end on the boundary")
	require.NotNil(t, cursor, "THE FIXTURE IS WRONG, not the code: five replies two to a page have a page two")

	result, err := env.db.ExecContext(env.ctx, `DELETE FROM comments WHERE uri = $1`, boundary)
	require.NoError(t, err)
	deleted, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "THE FIXTURE IS WRONG, not the code: the boundary row must be gone")

	rest, _ := commentHotWalk(t, env, 2, cursor, "")
	assert.Equal(t, []string{third, fourth, fifth}, rest,
		"the replies after a deleted boundary must each be served once, in order; a walk that ends "+
			"here has silently cut the thread short")

	single, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 50, nil, "")
	require.NoError(t, err)
	assert.Equal(t, commentURIs(single), append([]string{leader}, rest...),
		"and the walk must match the thread read in one request once the boundary is gone")
}
