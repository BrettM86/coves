//go:build integration

package postgres

import (
	"fmt"
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
// a cursor fails, and it is how the two pins in this file were found: under the
// "hot" sort a thread of any age loses every comment after page one
// (TestCommentRepo_HotCursorLosesEveryRowAfterPageOne), and under an
// unrecognised sort the cursor is discarded and page one repeats forever
// (TestCommentRepo_UnknownSortCursorRepeatsPageOne).
//
// See comment_repo_write_test.go for commentEnv and the seeding helpers.

// commentHotRankOf recomputes the ranking formula the way the SQL does, so a
// test can say what the cursor SHOULD have carried.
func commentHotRankOf(t *testing.T, env *commentEnv, uri string) float64 {
	t.Helper()
	var rank float64
	require.NoError(t, env.db.QueryRowContext(env.ctx, `
		SELECT log(greatest(2, c.score + 2)) / power(((EXTRACT(EPOCH FROM (NOW() - c.created_at)) / 3600) + 2), 1.8)
		FROM comments c WHERE c.uri = $1
	`, uri).Scan(&rank))
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
			"hot": {"0.5|42|" + validTime + "|" + validURI, 5},
		}
		for sort, shape := range shapes {
			cursor := commentEncodeCursor(shape.valid)
			filter, values, err := env.impl.parseCommentCursor(&cursor, sort)
			require.NoErrorf(t, err, "the %s cursor shape must be accepted", sort)
			assert.NotEmpty(t, filter)
			assert.Lenf(t, values, shape.params, "the %s filter binds one parameter per key it "+
				"compares (hot repeats the URI to exclude the boundary row)", sort)

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
			"hot": "0.5|42|" + validTime + "|" + validURI + "|extra",
		} {
			cursor := commentEncodeCursor(plaintext)
			_, _, err := env.impl.parseCommentCursor(&cursor, sort)
			require.Errorf(t, err, "the %s parser accepted a cursor with a trailing field", sort)
			assert.ErrorIs(t, err, comments.ErrInvalidCursor)
		}
	})

	for name, tc := range map[string]struct {
		sort   string
		cursor string
	}{
		"new sort, uri is not an AT-URI":  {"new", validTime + "|https://example.com/evil"},
		"top sort, uri is not an AT-URI":  {"top", "42|" + validTime + "|https://example.com"},
		"hot sort, uri is not an AT-URI":  {"hot", "0.5|42|" + validTime + "|nope"},
		"top sort, score is not a number": {"top", "banana|" + validTime + "|" + validURI},
		"hot sort, score is not a number": {"hot", "0.5|banana|" + validTime + "|" + validURI},
		"hot sort, rank is not a number":  {"hot", "banana|42|" + validTime + "|" + validURI},
		"top sort, score overflows int":   {"top", "99999999999999999999|" + validTime + "|" + validURI},
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

	// Round trip, per sort: what buildCommentCursor writes must be what
	// parseCommentCursor reads back. These are the only two functions that know
	// the wire format, and nothing else validates that they agree.
	t.Run("reads back what the builder wrote", func(t *testing.T) {
		t.Parallel()
		comment := &comments.Comment{
			URI:       validURI,
			Score:     37,
			CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC),
		}

		newCursor := env.impl.buildCommentCursor(comment, "new", 0)
		_, values, err := env.impl.parseCommentCursor(&newCursor, "new")
		require.NoError(t, err)
		require.Len(t, values, 2)
		assert.Equal(t, "2026-03-04T05:06:07.891011Z", values[0],
			"the timestamp must survive with its fractional second: truncating to whole seconds "+
				"would skip or repeat every comment posted in the same second")
		assert.Equal(t, validURI, values[1])

		topCursor := env.impl.buildCommentCursor(comment, "top", 0)
		_, values, err = env.impl.parseCommentCursor(&topCursor, "top")
		require.NoError(t, err)
		require.Len(t, values, 3)
		assert.Equal(t, 37, values[0], "the score is the primary sort key of the top ranking")

		hotCursor := env.impl.buildCommentCursor(comment, "hot", 0.123456789)
		_, values, err = env.impl.parseCommentCursor(&hotCursor, "hot")
		require.NoError(t, err)
		require.Len(t, values, 5)
		assert.Equal(t, 37, values[1])
		assert.Equal(t, validURI, values[3])
		assert.Equal(t, validURI, values[4], "the hot filter binds the URI twice: once for the "+
			"tiebreak and once to exclude the boundary row, whose recomputed rank has drifted")
	})

	t.Run("the default arm of the builder emits the bare URI", func(t *testing.T) {
		t.Parallel()
		comment := &comments.Comment{URI: validURI, Score: 5, CreatedAt: time.Now()}
		cursor := env.impl.buildCommentCursor(comment, "wildly-unknown", 0)
		assert.Equal(t, validURI, commentDecodeCursor(t, cursor),
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

	// The freshest possible thread is the only case where the hot cursor works,
	// because the rank is still large enough to survive being written to six
	// decimal places. Asserted so the pin below is understood as a precision
	// failure rather than as "hot pagination is unimplemented".
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

// TestCommentRepo_HotCursorLosesEveryRowAfterPageOne pins the sharpest defect in
// this file.
//
// Issue: 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md
//
// buildCommentCursor writes the hot rank with %f — six decimal places. The rank
// is log10(greatest(2, score+2)) / (age_in_hours+2)^1.8, and six decimals lose
// rows in two different ways.
//
// The one this test drives is TOTAL LOSS on old threads. At score 0 the rank
// crosses 1e-6 at about 46 days and rounds to "0.000000" at about 68 days; at
// higher scores the second crossing runs from roughly 120 days (score 5) to 194
// days (score 100). parseCommentCursor then binds that zero and the filter asks
// for rows whose recomputed rank is STRICTLY LESS, which no row can satisfy —
// the second page is empty and the cursor is nil. The thread ends after one
// page, with no error, no log line and no way for the client to tell.
//
// The timescale matters for how this gets prioritised, so it is stated
// precisely rather than roundly: at seven days the rank is about 2.9e-5, some
// 29x above the floor, so week-old threads are FINE. What is affected is
// months-old threads — which, on a forum, is most of the archive.
//
// The second way is PARTIAL LOSS at any age, and this test does not drive it
// because it needs a rank whose 7th decimal digit is non-zero. %f rounds to
// nearest, so the recorded boundary is usually not the boundary row's true
// rank. Rounded DOWN, every row whose true rank falls in [rounded, true) is
// excluded by the strict `<` although it belongs on the next page — a hole in
// the middle of a fresh thread, not just an old one. Rounded UP, rows in
// [true, rounded) are served twice, since the `c.uri != $7` guard excludes only
// the boundary row itself.
//
// Threads under the default sort are the most-read surface the AppView has.
//
// The fix is to write the rank at full precision (%v, or strconv.FormatFloat
// with -1) — or to page hot on a stable key rather than on a value recomputed
// from NOW() at every request, which also removes the drift the `c.uri != $7`
// guard exists to paper over.
func TestCommentRepo_HotCursorLosesEveryRowAfterPageOne(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	lastYear := time.Now().UTC().AddDate(-1, 0, 0)
	var all []string
	for i, rkey := range []string{"aged1", "aged2", "aged3"} {
		all = append(all, env.seed(commentSpec{
			rkey: rkey, score: 100 - i*10, createdAt: lastYear.Add(time.Duration(i) * time.Hour),
		}))
	}

	first, cursor, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 1, nil, "")
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NotNil(t, cursor, "there are three replies and the page holds one")

	trueRank := commentHotRankOf(t, env, first[0].URI)
	require.Less(t, trueRank, 1e-6, "a year-old comment ranks below what six decimals can express")
	assert.True(t, strings.HasPrefix(commentDecodeCursor(t, *cursor), "0.000000|"),
		"the cursor rounded the rank to zero: %%f gives six decimal places and the rank needs seven. "+
			"IF THIS FAILED (issue 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md) the defect is FIXED — delete this pin")

	second, next, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 1, cursor, "")
	require.NoError(t, err)
	assert.Empty(t, second,
		"page two of a year-old thread should return the second reply. Instead the filter asks for "+
			"a rank below 0.000000 and nothing qualifies, so two of three replies are unreachable. "+
			"IF THIS FAILED (issue 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md) the defect is FIXED — delete this pin and assert the full walk instead")
	assert.Nil(t, next)

	require.Len(t, all, 3)
	byNew, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "new", "", 10, nil, "")
	require.NoError(t, err)
	assert.Len(t, byNew, 3,
		"all three replies are present and reachable under every other sort, which is what makes "+
			"this a cursor defect rather than a missing-data one")
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

// TestCommentRepo_HotCursorAcceptsANonNumericRank pins a validation gap in the
// hot cursor parser.
//
// The rank is read with fmt.Sscanf("%f"), which accepts "NaN" and "Inf" as
// perfectly good floats, and accepts a numeric prefix followed by any trailing
// garbage. lib/pq then sends NaN to Postgres, where NaN sorts ABOVE every other
// numeric value — so "rank < NaN" is true for every row and the filter degrades
// to "everything except the one URI the cursor names".
//
// Nothing is corrupted and nothing crashes; a forged cursor just gets the
// reader a page they have already seen. It is recorded because the parser
// validates the URI half strictly and the numeric half not at all, and because
// a future comparison that treats NaN as "no rows" rather than "all rows" would
// turn the same input into a silent truncation.
func TestCommentRepo_HotCursorAcceptsANonNumericRank(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	now := time.Now().UTC()
	boundary := env.seed(commentSpec{rkey: "n1", score: 30, createdAt: now})
	other := env.seed(commentSpec{rkey: "n2", score: 20, createdAt: now})

	forged := commentEncodeCursor("NaN|30|" + now.Format("2006-01-02T15:04:05.999999999Z07:00") + "|" + boundary)
	_, values, err := env.impl.parseCommentCursor(&forged, "hot")
	require.NoError(t, err,
		"a rank of \"NaN\" should be rejected with comments.ErrInvalidCursor the way a rank of "+
			"\"banana\" is. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")
	require.Len(t, values, 5)

	page, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "hot", "", 10, &forged, "")
	require.NoError(t, err)
	assert.Equal(t, []string{other}, commentURIs(page),
		"NaN compares greater than every rank in Postgres, so the filter admits the whole thread "+
			"and only the cursor's own URI is excluded — the reader is served a page they have "+
			"already read. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")

	trailing := commentEncodeCursor("0.5xyz|30zzz|" + now.Format("2006-01-02T15:04:05.999999999Z07:00") + "|" + boundary)
	_, values, err = env.impl.parseCommentCursor(&trailing, "hot")
	require.NoError(t, err,
		"Sscanf stops at the first character it cannot use and reports no error, so a rank of "+
			"\"0.5xyz\" is read as 0.5. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")
	assert.InDelta(t, 0.5, values[0], 1e-9)
	assert.Equal(t, 30, values[1])
}
