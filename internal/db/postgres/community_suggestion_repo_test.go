//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communitysuggestions"
	"Coves/tests/testkit"
)

// community_suggestion_repo.go against real SQL: the "which community should we
// create next" board and the votes that rank it.
//
// The board is small but it is a ranked, user-writable list, which makes it a
// vote-counting problem rather than a CRUD one. community_suggestions carries a
// DENORMALISED vote_count column that no trigger maintains — three repository
// methods (UpsertVote, DeleteVote, AtomicVote) each adjust it by hand inside a
// transaction alongside the suggestion_votes row they wrote. Nothing in the
// schema forces the two to agree. If they drift, the ordering the board is
// served in is wrong, and it is wrong permanently: nothing ever recomputes the
// count from the rows.
//
// So every vote test here asserts TWICE — once on vote_count, and once on a
// COUNT/SUM over suggestion_votes read straight out of the same database. A test
// that checked only the counter would pass against an implementation that
// double-counted, and a test that checked only the rows would pass against one
// whose counter never moved.
//
// The three things only a real server decides, and the reason these are
// integration tests:
//
//   - Five CHECK constraints (valid_suggestion_status, title_not_empty,
//     title_max_length, description_not_empty, description_max_length) plus
//     valid_vote_value are the entire write-side vocabulary and length
//     enforcement. Create maps each constraint NAME onto a distinct domain
//     error, which is string matching against text only Postgres emits.
//   - suggestion_votes.suggestion_id is a real foreign key with ON DELETE
//     CASCADE, and unique_suggestion_voter is what makes "one vote per person"
//     true rather than merely intended. Both are load-bearing and neither exists
//     in Go.
//   - CountBySubmitterSince is the rate limiter's only input (three suggestions
//     per user per day). Whether its boundary is inclusive is a property of the
//     SQL comparison operator, and getting it wrong by one is either a limit of
//     two or a limit of four.
//
// AtomicVote is the sharpest surface in the file and gets the most attention
// below: it is toggle semantics — up, up again means none; up then down means
// down — implemented as read-then-branch-then-write under row locks, with the
// denormalised counter moved by a delta computed in Go from the value it read.

// suggestionRepoAndDB returns the repository under test alongside the raw handle
// for the same private database. The raw handle is not a shortcut: the central
// claim of this file is that the denormalised counter agrees with the vote rows,
// and the repository deliberately exposes no way to read the rows in bulk.
func suggestionRepoAndDB(t *testing.T) (communitysuggestions.Repository, *sql.DB) {
	t.Helper()
	db := testkit.DB(t)
	return NewCommunitySuggestionRepository(db), db
}

// suggestionFixture is a suggestion that satisfies all five CHECK constraints,
// so a test about one column does not have to restate the other four.
func suggestionFixture(t *testing.T) *communitysuggestions.CommunitySuggestion {
	t.Helper()
	id := testkit.UniqueID(t)
	return &communitysuggestions.CommunitySuggestion{
		Title:        "photography " + id,
		Description:  "a place for film and digital photography, posted by " + id,
		SubmitterDID: "did:plc:submitter" + id,
	}
}

// suggestionSeeded creates one suggestion and fails the test if it did not land,
// because every vote assertion downstream is meaningless without it.
func suggestionSeeded(t *testing.T, repo communitysuggestions.Repository) *communitysuggestions.CommunitySuggestion {
	t.Helper()
	suggestion := suggestionFixture(t)
	require.NoError(t, repo.Create(context.Background(), suggestion),
		"seeding the suggestion the votes hang off")
	return suggestion
}

// suggestionTally reads the two numbers that must always agree: the
// denormalised counter on the suggestion, and the sum of the actual vote rows.
// It also returns how many vote rows exist, which is what separates "counted
// twice" from "one vote with the wrong value".
func suggestionTally(t *testing.T, db *sql.DB, suggestionID int64) (counter, rowSum, rowCount int) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT vote_count FROM community_suggestions WHERE id = $1`, suggestionID).Scan(&counter))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(value), 0), COUNT(*) FROM suggestion_votes WHERE suggestion_id = $1`,
		suggestionID).Scan(&rowSum, &rowCount))
	return counter, rowSum, rowCount
}

// suggestionAssertReconciled is the assertion this whole file exists for: the
// denormalised counter equals the sum of the rows behind it.
func suggestionAssertReconciled(t *testing.T, db *sql.DB, suggestionID int64, wantSum, wantRows int) {
	t.Helper()
	counter, rowSum, rowCount := suggestionTally(t, db, suggestionID)
	assert.Equal(t, wantRows, rowCount, "the number of vote rows is not what this sequence should leave")
	assert.Equal(t, wantSum, rowSum, "the vote rows themselves do not sum to the expected score")
	assert.Equal(t, wantSum, counter,
		"vote_count (%d) has drifted from the sum of suggestion_votes (%d). Nothing recomputes this "+
			"column, so the board's ranking is now permanently wrong for this suggestion", counter, rowSum)
}

// suggestionBackdate moves created_at so ordering and the rate-limit boundary
// are decided by fixed values rather than by how fast the inserts ran.
func suggestionBackdate(t *testing.T, db *sql.DB, suggestionID int64, at time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE community_suggestions SET created_at = $1 WHERE id = $2`, at, suggestionID)
	require.NoError(t, err)
}

func suggestionTimestamps(t *testing.T, db *sql.DB, suggestionID int64) (created, updated time.Time) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT created_at, updated_at FROM community_suggestions WHERE id = $1`,
		suggestionID).Scan(&created, &updated))
	return created, updated
}

func suggestionIDsOf(list []*communitysuggestions.CommunitySuggestion) []int64 {
	ids := make([]int64, 0, len(list))
	for _, item := range list {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestSuggestionRepo_Create(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips every column through Postgres", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		before := time.Now().Add(-time.Minute)

		suggestion := suggestionFixture(t)
		suggestion.Title = "urban gardening"
		suggestion.Description = "balcony crops, community plots, seed swaps"
		require.NoError(t, repo.Create(ctx, suggestion))

		require.NotZero(t, suggestion.ID)
		assert.True(t, suggestion.CreatedAt.After(before),
			"created_at is not in the INSERT column list, so the server's NOW() stamps it; a zero "+
				"value here means RETURNING was not read and the 'new' sort has nothing to order by")
		assert.True(t, suggestion.UpdatedAt.After(before))

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Equal(t, "urban gardening", got.Title)
		assert.Equal(t, "balcony crops, community plots, seed swaps", got.Description)
		assert.Equal(t, suggestion.SubmitterDID, got.SubmitterDID,
			"the submitter is what the daily rate limit is counted against; losing it makes the limit "+
				"uncountable")
		assert.Equal(t, communitysuggestions.StatusOpen, got.Status)
		assert.Zero(t, got.VoteCount, "a brand new suggestion starts unranked, not pre-scored")
		assert.WithinDuration(t, suggestion.CreatedAt, got.CreatedAt, time.Millisecond)
		assert.WithinDuration(t, suggestion.UpdatedAt, got.UpdatedAt, time.Millisecond)
		assert.Nil(t, got.Viewer, "viewer state is assembled by the service, not read from this table")
	})

	t.Run("defaults an unset status to open", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		suggestion := suggestionFixture(t)
		suggestion.Status = ""
		require.NoError(t, repo.Create(ctx, suggestion))

		assert.Equal(t, communitysuggestions.StatusOpen, suggestion.Status,
			"the defaulted status is written back onto the caller's struct so it agrees with the row")

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Equal(t, communitysuggestions.StatusOpen, got.Status,
			"'open' is the status the public board filters on; anything else is a suggestion nobody "+
				"can vote for")
	})

	t.Run("accepts every status in the lexicon", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		for _, status := range communitysuggestions.ValidStatuses() {
			suggestion := suggestionFixture(t)
			suggestion.Status = status
			require.NoErrorf(t, repo.Create(ctx, suggestion),
				"status %q is in ValidStatuses() but valid_suggestion_status refused it: the Go "+
					"vocabulary and the SQL vocabulary have drifted", status)

			got, err := repo.GetByID(ctx, suggestion.ID)
			require.NoError(t, err)
			assert.Equal(t, status, got.Status)
		}
	})

	// One subtest per CHECK constraint, because the mapping is by constraint
	// NAME and a rename in a migration would silently collapse all five onto the
	// generic wrapper — the write would still fail, but the handler would answer
	// 500 instead of telling the user their title is too long.
	t.Run("maps each CHECK constraint onto its own domain error", func(t *testing.T) {
		t.Parallel()

		for name, tc := range map[string]struct {
			mutate func(*communitysuggestions.CommunitySuggestion)
			want   error
			why    string
		}{
			"a status outside the vocabulary": {
				mutate: func(s *communitysuggestions.CommunitySuggestion) {
					s.Status = communitysuggestions.Status("shipped")
				},
				want: communitysuggestions.ErrInvalidStatus,
				why:  "a status the board does not filter on is a suggestion in no list at all",
			},
			"a title that is only spaces": {
				mutate: func(s *communitysuggestions.CommunitySuggestion) { s.Title = "   " },
				want:   communitysuggestions.ErrTitleRequired,
				why: "title_not_empty applies TRIM, so a row of spaces is caught in SQL even when a " +
					"caller skipped the Go-side trim",
			},
			"a title past 200 characters": {
				mutate: func(s *communitysuggestions.CommunitySuggestion) {
					s.Title = strings.Repeat("a", 201)
				},
				want: communitysuggestions.ErrTitleTooLong,
				why:  "an unbounded title is a rendering problem on every client that shows the board",
			},
			"a description that is only spaces": {
				mutate: func(s *communitysuggestions.CommunitySuggestion) { s.Description = "     " },
				want:   communitysuggestions.ErrDescriptionRequired,
				why:    "a suggestion with no rationale cannot be voted on meaningfully",
			},
			"a description past 5000 characters": {
				mutate: func(s *communitysuggestions.CommunitySuggestion) {
					s.Description = strings.Repeat("b", 5001)
				},
				want: communitysuggestions.ErrDescriptionTooLong,
				why:  "the length cap is the only thing bounding what one user can write into this table",
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				repo, db := suggestionRepoAndDB(t)

				suggestion := suggestionFixture(t)
				tc.mutate(suggestion)

				err := repo.Create(ctx, suggestion)
				require.ErrorIs(t, err, tc.want, tc.why)

				var stored int
				require.NoError(t, db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM community_suggestions`).Scan(&stored))
				assert.Zero(t, stored, "the rejected suggestion must not have reached the table")
			})
		}
	})

	t.Run("accepts a title and description exactly at the limit", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		suggestion := suggestionFixture(t)
		suggestion.Title = strings.Repeat("a", 200)
		suggestion.Description = strings.Repeat("b", 5000)
		require.NoError(t, repo.Create(ctx, suggestion),
			"the constraints are <=, so the boundary value itself is legal; an off-by-one here would "+
				"reject a title the client's own counter said was fine")

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Len(t, got.Title, 200)
		assert.Len(t, got.Description, 5000)
	})

	// LENGTH() in Postgres counts characters, not bytes, and so does the Go-side
	// utf8.RuneCountInString check in CreateSuggestionRequest.Validate. If the
	// column ever moved to a byte length the two would disagree, and a title a
	// client accepted would be refused by the database with no useful message.
	t.Run("counts multi-byte characters the way the Go validator does", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		suggestion := suggestionFixture(t)
		suggestion.Title = strings.Repeat("é", 200)
		require.NoError(t, repo.Create(ctx, suggestion),
			"200 accented characters is 400 bytes; a byte-counting constraint would reject this while "+
				"communitysuggestions.MaxTitleLength says it is fine")
	})

	t.Run("lets two people suggest the same community", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		first := suggestionFixture(t)
		first.Title = "bird watching"
		require.NoError(t, repo.Create(ctx, first))

		second := suggestionFixture(t)
		second.Title = "bird watching"
		require.NoError(t, repo.Create(ctx, second),
			"there is no unique index on title, and there should not be: deduplication is a human "+
				"judgement the admin makes with the status column, not a write that fails")
		assert.NotEqual(t, first.ID, second.ID)
	})
}

// TestSuggestionRepo_BlankTitleOfTabsSlipsPastTheConstraint pins a gap between
// the SQL emptiness check and the Go one.
//
// KNOWN DEFECT: title_not_empty and description_not_empty are
// LENGTH(TRIM(col)) > 0, and Postgres' one-argument TRIM strips the SPACE
// character only — not tabs, not newlines, not U+00A0. Go's
// CreateSuggestionRequest.Validate uses strings.TrimSpace, which strips all of
// them. The two therefore disagree, and the database is the weaker of the pair:
// a title made of tabs is stored and renders as a blank row on the board that
// can still be voted on.
//
// It is masked today because the service validates before the repository is
// reached. The exposure is any writer that does not go through
// CreateSuggestionRequest — a backfill, an admin tool, a future importer.
//
// The fix is in the schema: TRIM(BOTH E' \t\n\r' FROM col), or a regexp check.
func TestSuggestionRepo_BlankTitleOfTabsSlipsPastTheConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := suggestionRepoAndDB(t)

	suggestion := suggestionFixture(t)
	suggestion.Title = "\t\n\r"
	suggestion.Description = "\t\t"

	err := repo.Create(ctx, suggestion)
	require.NoError(t, err,
		"IF THIS FAILED (issue 2026-07-31-suggestion-check-constraints-weaker-than-go.md) the emptiness constraints were widened past the SPACE character and the "+
			"defect is FIXED — assert ErrTitleRequired here and delete this pin")

	got, err := repo.GetByID(ctx, suggestion.ID)
	require.NoError(t, err)
	assert.Equal(t, "\t\n\r", got.Title,
		"the correct behaviour would be to reject the write the way strings.TrimSpace would; today "+
			"the board carries a suggestion with no visible title")
	assert.Empty(t, strings.TrimSpace(got.Title),
		"and Go agrees the stored title is blank, which is precisely the disagreement")
}

func TestSuggestionRepo_GetByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("reports an id that does not exist as a domain not-found", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		_, err := repo.GetByID(ctx, 987654321)
		require.ErrorIs(t, err, communitysuggestions.ErrSuggestionNotFound,
			"sql.ErrNoRows leaking out of the repository would make every caller import database/sql "+
				"to tell 'no such suggestion' from 'the query broke', and the handler would answer 500 "+
				"where it owes a 404")
	})

	t.Run("reflects the live vote count rather than a value cached at creation", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter000000000000a", 1))
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter000000000000b", 1))

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, got.VoteCount)
	})

	t.Run("returns the suggestion asked for and not a neighbour", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		first := suggestionSeeded(t, repo)
		second := suggestionSeeded(t, repo)

		got, err := repo.GetByID(ctx, second.ID)
		require.NoError(t, err)
		assert.Equal(t, second.ID, got.ID)
		assert.NotEqual(t, first.Title, got.Title)
	})
}

func TestSuggestionRepo_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Four suggestions with fixed created_at values and hand-set vote counts, so
	// both sort orders and the status filter are decided by the data.
	seed := func(t *testing.T) (communitysuggestions.Repository, *sql.DB, map[string]int64) {
		t.Helper()
		repo, db := suggestionRepoAndDB(t)
		base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
		ids := map[string]int64{}
		for _, spec := range []struct {
			label  string
			days   int
			votes  int
			status communitysuggestions.Status
		}{
			{"oldest", 0, 5, communitysuggestions.StatusOpen},
			// "middle" and "newest" share a vote count, so the popular sort's
			// created_at DESC tiebreak has to put "newest" first of the two —
			// and "middle" is INSERTED first, so physical row order pushes the
			// opposite way. Seeded the other way round, a popular sort that had
			// lost its tiebreak entirely would still produce the expected page
			// and every ordering assertion below would pass against it.
			{"middle", 1, 1, communitysuggestions.StatusOpen},
			{"newest", 3, 1, communitysuggestions.StatusOpen},
			{"approved", 2, 9, communitysuggestions.StatusApproved},
		} {
			suggestion := suggestionFixture(t)
			suggestion.Title = spec.label
			suggestion.Status = spec.status
			require.NoError(t, repo.Create(ctx, suggestion))
			suggestionBackdate(t, db, suggestion.ID, base.AddDate(0, 0, spec.days))
			_, err := db.ExecContext(ctx,
				`UPDATE community_suggestions SET vote_count = $1 WHERE id = $2`, spec.votes, suggestion.ID)
			require.NoError(t, err)
			ids[spec.label] = suggestion.ID
		}
		return repo, db, ids
	}

	t.Run("sorted by new, the most recent suggestion is first", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 10})
		require.NoError(t, err)
		assert.Equal(t,
			[]int64{ids["newest"], ids["approved"], ids["middle"], ids["oldest"]},
			suggestionIDsOf(list),
			"'new' is the tab that lets an unvoted suggestion be seen at all; ordering it any other "+
				"way makes the board a permanent incumbency for whatever was posted first")
	})

	t.Run("sorted by popular, the highest vote count is first and ties break on recency", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "popular", Limit: 10})
		require.NoError(t, err)
		assert.Equal(t,
			[]int64{ids["approved"], ids["oldest"], ids["newest"], ids["middle"]},
			suggestionIDsOf(list),
			"vote_count DESC then created_at DESC. The tiebreak is not cosmetic: without a total "+
				"order, two suggestions on the same score swap places between page loads, which "+
				"duplicates one and hides the other across a LIMIT/OFFSET boundary")
	})

	// The repository treats an empty Sort as "new", while
	// ListSuggestionsRequest.Validate defaults it to "popular". The service
	// always validates first, so the two defaults never meet in production —
	// but any new caller that reaches the repository directly gets a different
	// board than the one the API serves.
	t.Run("an unset sort falls back to newest-first", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Limit: 10})
		require.NoError(t, err)
		assert.Equal(t,
			[]int64{ids["newest"], ids["approved"], ids["middle"], ids["oldest"]},
			suggestionIDsOf(list),
			"the repository's own default is 'new', not the 'popular' that "+
				"ListSuggestionsRequest.Validate applies")
	})

	t.Run("refuses a sort it does not implement instead of guessing one", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		_, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "controversial", Limit: 10})
		require.Error(t, err, "silently falling through to some default would serve a board the "+
			"caller did not ask for, and no client could tell")
		assert.ErrorContains(t, err, "controversial",
			"the rejected value belongs in the message; 'unknown sort' alone is undiagnosable")
	})

	t.Run("filters to one status", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		open, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "new", Status: string(communitysuggestions.StatusOpen), Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["newest"], ids["middle"], ids["oldest"]}, suggestionIDsOf(open),
			"the status filter is what keeps an already-approved community off the voting board")

		approved, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "new", Status: string(communitysuggestions.StatusApproved), Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["approved"]}, suggestionIDsOf(approved))
	})

	t.Run("an unset status returns every status", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, list, 4, "the WHERE clause must be omitted entirely, not compared against ''")
	})

	t.Run("a status nobody used matches nothing rather than erroring", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "new", Status: "shipped", Limit: 10})
		require.NoError(t, err, "the read path has no CHECK constraint to trip; an unknown status is "+
			"a filter nothing matches")
		assert.Empty(t, list)
	})

	t.Run("pages without skipping or repeating a suggestion", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		first, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 2, Offset: 0})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["newest"], ids["approved"]}, suggestionIDsOf(first))

		second, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 2, Offset: 2})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["middle"], ids["oldest"]}, suggestionIDsOf(second),
			"page two must resume exactly where page one stopped")

		third, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 2, Offset: 4})
		require.NoError(t, err)
		assert.Empty(t, third, "reading past the end is an empty page, not an error")
	})

	// The pages above are all distinct on created_at, so they exercise LIMIT and
	// OFFSET but say nothing about stability. This one splits the TIED pair
	// across the boundary: under "popular", "newest" is the last row of page one
	// and "middle" the first of page two, on equal vote counts. Two requests
	// sort independently, so an ORDER BY without a total order is free to place
	// them differently each time — which serves one twice and drops the other.
	// A tie that sits wholly inside one page cannot show that.
	t.Run("a tie split across a page boundary is still stable", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		first, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "popular", Limit: 3, Offset: 0})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["approved"], ids["oldest"], ids["newest"]}, suggestionIDsOf(first))

		second, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "popular", Limit: 3, Offset: 3})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["middle"]}, suggestionIDsOf(second),
			"the row that ends page one must not reappear at the top of page two, and the row it "+
				"is tied with must not be skipped")

		assert.ElementsMatch(t,
			[]int64{ids["approved"], ids["oldest"], ids["newest"], ids["middle"]},
			append(suggestionIDsOf(first), suggestionIDsOf(second)...),
			"paging the board must visit every suggestion exactly once")
	})

	t.Run("an offset of one skips exactly one row", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 10, Offset: 1})
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["approved"], ids["middle"], ids["oldest"]}, suggestionIDsOf(list),
			"an offset that is off by one either shows the previous page's last row again or drops a "+
				"suggestion out of the board entirely")
	})

	// The board's page size when a caller asks for none. Fifty-one rows is the
	// only way to see the number: any smaller fixture passes whatever the
	// default happens to be.
	t.Run("a limit of zero means fifty, not none and not everything", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		for i := 0; i < 51; i++ {
			suggestion := suggestionFixture(t)
			suggestion.Title = fmt.Sprintf("bulk %02d", i)
			require.NoError(t, repo.Create(ctx, suggestion))
		}

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: 0})
		require.NoError(t, err)
		assert.Len(t, list, 50,
			"a zero limit reaching LIMIT 0 would serve an empty board; no limit at all would serve "+
				"the whole table to every unauthenticated visitor")

		negative, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "new", Limit: -5})
		require.NoError(t, err)
		assert.Len(t, negative, 50, "a negative limit takes the same floor rather than reaching SQL, "+
			"where it would be an error")
	})

	t.Run("an empty board is an empty listing", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		list, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{Sort: "popular", Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	// Offset is passed to SQL unclamped. ListSuggestionsRequest.Validate raises a
	// negative offset to zero, so the API never sends one, but the repository
	// itself has no floor and Postgres refuses a negative OFFSET outright.
	t.Run("a negative offset reaches SQL and is refused there", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		_, err := repo.List(ctx, communitysuggestions.ListSuggestionsRequest{
			Sort: "new", Limit: 10, Offset: -1})
		require.Error(t, err, "the repository applies a floor to Limit but not to Offset; the "+
			"clamping lives in ListSuggestionsRequest.Validate, which is a caller's obligation "+
			"rather than this layer's guarantee")
		assert.ErrorContains(t, err, "OFFSET")
	})
}

func TestSuggestionRepo_CountBySubmitterSince(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The boundary matters because this is the rate limiter. Three suggestions
	// per user per day is enforced by comparing this count against
	// MaxSuggestionsPerDay, so an operator that is > instead of >= turns the
	// limit into four on the exact tick, and one that is <= inverts it entirely.
	t.Run("the since boundary is inclusive", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		submitter := "did:plc:ratelimited000000"
		at := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)

		suggestion := suggestionFixture(t)
		suggestion.SubmitterDID = submitter
		require.NoError(t, repo.Create(ctx, suggestion))
		suggestionBackdate(t, db, suggestion.ID, at)

		onTheBoundary, err := repo.CountBySubmitterSince(ctx, submitter, at)
		require.NoError(t, err)
		assert.Equal(t, 1, onTheBoundary,
			"created_at >= since: a suggestion made at exactly the window's start counts against the "+
				"limit. A strict > here would let a user post one extra every window")

		justAfter, err := repo.CountBySubmitterSince(ctx, submitter, at.Add(time.Microsecond))
		require.NoError(t, err)
		assert.Zero(t, justAfter,
			"and a microsecond later the same suggestion is outside the window — Postgres stores "+
				"timestamptz to microsecond resolution, so this is the smallest observable step")

		justBefore, err := repo.CountBySubmitterSince(ctx, submitter, at.Add(-time.Microsecond))
		require.NoError(t, err)
		assert.Equal(t, 1, justBefore)
	})

	t.Run("counts only the submitter asked about", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		mine := "did:plc:mysubmitter000000"
		theirs := "did:plc:theirsubmitter000"
		window := time.Now().Add(-24 * time.Hour)

		for i := 0; i < 2; i++ {
			suggestion := suggestionFixture(t)
			suggestion.SubmitterDID = mine
			require.NoError(t, repo.Create(ctx, suggestion))
		}
		for i := 0; i < 3; i++ {
			suggestion := suggestionFixture(t)
			suggestion.SubmitterDID = theirs
			require.NoError(t, repo.Create(ctx, suggestion))
		}

		count, err := repo.CountBySubmitterSince(ctx, mine, window)
		require.NoError(t, err)
		assert.Equal(t, 2, count,
			"a missing submitter_did filter makes the rate limit global: the third person to post "+
				"today would be told they had exhausted their own daily quota")

		count, err = repo.CountBySubmitterSince(ctx, theirs, window)
		require.NoError(t, err)
		assert.Equal(t, 3, count)
	})

	t.Run("counts every status, not only the open ones", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		submitter := "did:plc:prolific0000000000"
		window := time.Now().Add(-24 * time.Hour)

		for _, status := range []communitysuggestions.Status{
			communitysuggestions.StatusOpen,
			communitysuggestions.StatusDeclined,
			communitysuggestions.StatusApproved,
		} {
			suggestion := suggestionFixture(t)
			suggestion.SubmitterDID = submitter
			suggestion.Status = status
			require.NoError(t, repo.Create(ctx, suggestion))
		}

		count, err := repo.CountBySubmitterSince(ctx, submitter, window)
		require.NoError(t, err)
		assert.Equal(t, 3, count,
			"the limit is on submissions, not on surviving submissions; excluding declined ones "+
				"would let a user spam the queue as fast as an admin could decline it")
	})

	t.Run("a submitter with nothing to their name counts zero without erroring", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		count, err := repo.CountBySubmitterSince(ctx, "did:plc:nobody000000000000", time.Now().Add(-time.Hour))
		require.NoError(t, err, "COUNT(*) over no rows is zero, not sql.ErrNoRows; a first-time "+
			"submitter must not be rate-limited by an error")
		assert.Zero(t, count)
	})

	t.Run("suggestions older than the window are excluded", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		submitter := "did:plc:yesterdayuser00000"

		old := suggestionFixture(t)
		old.SubmitterDID = submitter
		require.NoError(t, repo.Create(ctx, old))
		suggestionBackdate(t, db, old.ID, time.Now().Add(-48*time.Hour))

		recent := suggestionFixture(t)
		recent.SubmitterDID = submitter
		require.NoError(t, repo.Create(ctx, recent))

		count, err := repo.CountBySubmitterSince(ctx, submitter, time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, 1, count,
			"the daily limit is a rolling window; counting all history would permanently lock out "+
				"anyone who ever hit it")
	})
}

func TestSuggestionRepo_UpdateStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("moves the suggestion and touches updated_at without disturbing created_at", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		createdBefore, updatedBefore := suggestionTimestamps(t, db, suggestion.ID)

		require.NoError(t, repo.UpdateStatus(ctx, suggestion.ID, communitysuggestions.StatusApproved))

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Equal(t, communitysuggestions.StatusApproved, got.Status)

		createdAfter, updatedAfter := suggestionTimestamps(t, db, suggestion.ID)
		assert.True(t, updatedAfter.After(updatedBefore),
			"updated_at is the only record that an admin acted; leaving it stale makes an approval "+
				"indistinguishable from a suggestion nobody has looked at")
		assert.WithinDuration(t, createdBefore, createdAfter, time.Millisecond,
			"an admin decision must not rewrite when the suggestion was posted — the 'new' sort and "+
				"the rate-limit window both read created_at")
	})

	t.Run("walks the whole status vocabulary", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		for _, status := range communitysuggestions.ValidStatuses() {
			require.NoErrorf(t, repo.UpdateStatus(ctx, suggestion.ID, status),
				"status %q is in the Go vocabulary but valid_suggestion_status refused it", status)
			got, err := repo.GetByID(ctx, suggestion.ID)
			require.NoError(t, err)
			assert.Equal(t, status, got.Status)
		}
	})

	t.Run("rejects a status the CHECK constraint does not allow", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		err := repo.UpdateStatus(ctx, suggestion.ID, communitysuggestions.Status("shipped"))
		require.ErrorIs(t, err, communitysuggestions.ErrInvalidStatus,
			"a raw 23514 here would surface the constraint name to an admin instead of telling them "+
				"which statuses exist")

		got, err := repo.GetByID(ctx, suggestion.ID)
		require.NoError(t, err)
		assert.Equal(t, communitysuggestions.StatusOpen, got.Status,
			"and the rejected update left the row where it was")
	})

	t.Run("reports an id that does not exist as a domain not-found", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		err := repo.UpdateStatus(ctx, 987654321, communitysuggestions.StatusApproved)
		require.ErrorIs(t, err, communitysuggestions.ErrSuggestionNotFound,
			"an UPDATE that matched nothing returns nil from Postgres; without the RowsAffected check "+
				"an admin would be told they approved a suggestion that is not there")
	})

	t.Run("changes only the suggestion named", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		target := suggestionSeeded(t, repo)
		bystander := suggestionSeeded(t, repo)

		require.NoError(t, repo.UpdateStatus(ctx, target.ID, communitysuggestions.StatusDeclined))

		got, err := repo.GetByID(ctx, bystander.ID)
		require.NoError(t, err)
		assert.Equal(t, communitysuggestions.StatusOpen, got.Status,
			"a missing WHERE id = $n would decline the entire board with one click")
	})

	t.Run("leaves the vote count alone", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter000000000000c", 1))

		require.NoError(t, repo.UpdateStatus(ctx, suggestion.ID, communitysuggestions.StatusUnderReview))

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})
}

func TestSuggestionRepo_UpsertVote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a first upvote writes the row and moves the counter by one", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		delta, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000001", 1)
		require.NoError(t, err)
		assert.Equal(t, 1, delta, "the delta is what a caller would apply to a cached score; a wrong "+
			"delta desynchronises every layer above this one")
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})

	t.Run("a downvote moves the counter the other way", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		delta, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000002", -1)
		require.NoError(t, err)
		assert.Equal(t, -1, delta)
		suggestionAssertReconciled(t, db, suggestion.ID, -1, 1)
		assert.Equal(t, -1, mustSuggestionVoteCount(t, repo, suggestion.ID),
			"vote_count is a signed score, not a tally: a suggestion the community rejects has to be "+
				"able to sit below zero, and a GREATEST(0, ...) floor here would make every downvote "+
				"below the first invisible")
	})

	// This is the double-count guard. Upsert is not a toggle — the same vote sent
	// twice (a retried request, a double tap) must be idempotent, and it is only
	// idempotent because the delta is computed as new minus old rather than
	// assumed to be the new value.
	t.Run("the same vote sent twice does not count twice", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter000000000003"

		first, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)
		assert.Equal(t, 1, first)

		second, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)
		assert.Zero(t, second, "re-sending an identical vote is a no-op, so its delta is zero")

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})

	t.Run("flipping a vote moves the counter by two", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter000000000004"

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)

		delta, err := repo.UpsertVote(ctx, suggestion.ID, voter, -1)
		require.NoError(t, err)
		assert.Equal(t, -2, delta, "up to down is a two-point swing: removing one upvote and adding "+
			"one downvote. A delta of -1 would leave the board a point high forever")

		suggestionAssertReconciled(t, db, suggestion.ID, -1, 1)

		vote, err := repo.GetVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.Equal(t, -1, vote.Value, "and there is still exactly one row, updated in place")
	})

	t.Run("many voters accumulate on one suggestion", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		for _, spec := range []struct {
			voter string
			value int
		}{
			{"did:plc:voter00000000000up1", 1},
			{"did:plc:voter00000000000up2", 1},
			{"did:plc:voter00000000000up3", 1},
			{"did:plc:voter00000000000dn1", -1},
		} {
			_, err := repo.UpsertVote(ctx, suggestion.ID, spec.voter, spec.value)
			require.NoError(t, err)
		}

		suggestionAssertReconciled(t, db, suggestion.ID, 2, 4)
	})

	t.Run("votes do not leak between suggestions", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		voted := suggestionSeeded(t, repo)
		bystander := suggestionSeeded(t, repo)

		_, err := repo.UpsertVote(ctx, voted.ID, "did:plc:voter000000000005", 1)
		require.NoError(t, err)

		suggestionAssertReconciled(t, db, voted.ID, 1, 1)
		suggestionAssertReconciled(t, db, bystander.ID, 0, 0)
	})

	t.Run("bumps updated_at so the popular board notices the change", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		_, updatedBefore := suggestionTimestamps(t, db, suggestion.ID)

		_, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000006", 1)
		require.NoError(t, err)

		_, updatedAfter := suggestionTimestamps(t, db, suggestion.ID)
		assert.True(t, updatedAfter.After(updatedBefore))
	})

	t.Run("rejects a vote value the CHECK constraint does not allow", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		_, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000007", 5)
		require.ErrorIs(t, err, communitysuggestions.ErrInvalidVoteValue,
			"valid_vote_value is the only thing bounding a vote's weight; a value of 5 that reached "+
				"the table would let one voter outrank five")

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
	})

	t.Run("a zero vote is refused rather than stored as an abstention", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		_, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000008", 0)
		require.ErrorIs(t, err, communitysuggestions.ErrInvalidVoteValue,
			"the CHECK is value IN (1, -1): removing a vote is DeleteVote's job, not a zero-valued row")

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
	})

	// The whole method is one transaction, so a rejected write must leave neither
	// the vote row nor the counter behind. This is the rollback, observed.
	t.Run("a rejected vote rolls back the counter it had already touched", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter00000000000ok", 1))

		_, err := repo.UpsertVote(ctx, suggestion.ID, "did:plc:voter000000000009", 3)
		require.Error(t, err)

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})
}

// mustSuggestionVoteCount reads the denormalised count through the repository's
// own read path, so a test can assert on what a client would actually be served.
func mustSuggestionVoteCount(t *testing.T, repo communitysuggestions.Repository, suggestionID int64) int {
	t.Helper()
	got, err := repo.GetByID(context.Background(), suggestionID)
	require.NoError(t, err)
	return got.VoteCount
}

// TestSuggestionRepo_UpsertVoteMisclassifiesTwoFailures pins two error mappings
// UpsertVote does not make, both of which AtomicVote does make one function
// away.
//
// KNOWN DEFECT (1): voting on a suggestion that does not exist fails on the
// foreign key, and UpsertVote has no branch for SQLSTATE 23503. The caller gets
// a wrapped driver error instead of ErrSuggestionNotFound, so a handler using
// communitysuggestions.IsNotFound answers 500 to what is a 404.
//
// KNOWN DEFECT (2): valid_vote_value is mapped only on the INSERT path. An
// out-of-range value sent by someone who has already voted takes the UPDATE
// branch, where the constraint violation is wrapped generically — so the same
// bad request is a 400 for a first-time voter and a 500 for a repeat one.
func TestSuggestionRepo_UpsertVoteMisclassifiesTwoFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a vote on a missing suggestion is not reported as not-found", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		_, err := repo.UpsertVote(ctx, 987654321, "did:plc:voter00000000000ph", 1)
		require.Error(t, err)
		assert.NotErrorIs(t, err, communitysuggestions.ErrSuggestionNotFound,
			"IF THIS FAILED (issue 2026-07-31-suggestion-vote-error-misclassification-and-lock-order.md), UpsertVote learned to map SQLSTATE 23503 the way AtomicVote already does "+
				"and the defect is FIXED — assert ErrSuggestionNotFound here and delete this pin")
		assert.ErrorContains(t, err, "failed to insert vote",
			"today the foreign key violation escapes as a generic wrapper, so IsNotFound is false and "+
				"the handler answers 500")
	})

	t.Run("an out-of-range value from a repeat voter is not reported as an invalid value", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000rp"

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)

		_, err = repo.UpsertVote(ctx, suggestion.ID, voter, 9)
		require.Error(t, err)
		assert.NotErrorIs(t, err, communitysuggestions.ErrInvalidVoteValue,
			"IF THIS FAILED (issue 2026-07-31-suggestion-vote-error-misclassification-and-lock-order.md), the UPDATE branch now maps valid_vote_value the way the INSERT branch "+
				"does and the defect is FIXED — assert ErrInvalidVoteValue here and delete this pin")
		assert.ErrorContains(t, err, "failed to update vote",
			"the same malformed request is classified differently depending on whether the voter has "+
				"voted before")

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})
}

func TestSuggestionRepo_DeleteVote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("removing an upvote takes the point back off the board", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000d1"

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)

		delta, err := repo.DeleteVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.Equal(t, -1, delta, "the delta is the negation of the value that was removed, read "+
			"from the DELETE's RETURNING rather than assumed")

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)

		_, err = repo.GetVote(ctx, suggestion.ID, voter)
		assert.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound)
	})

	t.Run("removing a downvote puts the point back", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000d2"

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, -1)
		require.NoError(t, err)

		delta, err := repo.DeleteVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.Equal(t, 1, delta, "withdrawing a downvote must ADD a point; a hardcoded -1 delta here "+
			"would drive the board's score down every time someone changed their mind")

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
	})

	t.Run("reports a vote that was never cast", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		_, err := repo.DeleteVote(ctx, suggestion.ID, "did:plc:voter00000000000d3")
		require.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"DELETE ... RETURNING with no match is sql.ErrNoRows; without the mapping, an unvote that "+
				"hit nothing would look identical to one that worked, and the counter would be "+
				"adjusted for a vote that never existed")

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
	})

	t.Run("removes only the voter named", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		mine := "did:plc:voter00000000000m1"
		theirs := "did:plc:voter00000000000t1"

		_, err := repo.UpsertVote(ctx, suggestion.ID, mine, 1)
		require.NoError(t, err)
		_, err = repo.UpsertVote(ctx, suggestion.ID, theirs, 1)
		require.NoError(t, err)

		_, err = repo.DeleteVote(ctx, suggestion.ID, mine)
		require.NoError(t, err)

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
		remaining, err := repo.GetVote(ctx, suggestion.ID, theirs)
		require.NoError(t, err, "withdrawing one person's vote deleted another's")
		assert.Equal(t, 1, remaining.Value)
	})

	t.Run("removes the vote only from the suggestion named", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		here := suggestionSeeded(t, repo)
		there := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000d4"

		_, err := repo.UpsertVote(ctx, here.ID, voter, 1)
		require.NoError(t, err)
		_, err = repo.UpsertVote(ctx, there.ID, voter, 1)
		require.NoError(t, err)

		_, err = repo.DeleteVote(ctx, here.ID, voter)
		require.NoError(t, err)

		suggestionAssertReconciled(t, db, here.ID, 0, 0)
		suggestionAssertReconciled(t, db, there.ID, 1, 1)
	})

	t.Run("a vote on a suggestion that does not exist is simply not found", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		_, err := repo.DeleteVote(ctx, 987654321, "did:plc:voter00000000000d5")
		require.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"the DELETE finds nothing first, so a missing suggestion and a missing vote are the same "+
				"answer here — which is the right one for an unvote either way")
	})
}

func TestSuggestionRepo_AtomicVote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a first vote is created", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000a1"

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
		vote, err := repo.GetVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.Equal(t, 1, vote.Value)
	})

	// The toggle is the whole point of this method: the client sends "upvote"
	// twice and expects the second one to mean "never mind". If the second call
	// were treated as an idempotent no-op instead, the button would look stuck.
	t.Run("voting the same way twice toggles the vote off", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000a2"

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
		_, err := repo.GetVote(ctx, suggestion.ID, voter)
		assert.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"toggling off must delete the row, not zero its value: a zero-valued row would violate "+
				"valid_vote_value and would still occupy the voter's one slot")
	})

	t.Run("voting the other way flips the vote in place", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000a3"

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, -1))

		suggestionAssertReconciled(t, db, suggestion.ID, -1, 1)
		vote, err := repo.GetVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.Equal(t, -1, vote.Value, "a flip is one row updated, not a second row inserted; "+
			"unique_suggestion_voter would refuse the insert and the transaction would fail")
	})

	// The full cycle a user can drive from the UI, asserted at every step. This
	// is where a counter that moves by the wrong delta shows up as drift that
	// never comes back to zero.
	t.Run("up then down then off lands the counter back on zero", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000a4"

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, -1))
		suggestionAssertReconciled(t, db, suggestion.ID, -1, 1)

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, -1))
		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})

	t.Run("one voter toggling does not disturb another's vote", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		steady := "did:plc:voter00000000000s1"
		fickle := "did:plc:voter00000000000f1"

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, steady, 1))
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, fickle, 1))
		suggestionAssertReconciled(t, db, suggestion.ID, 2, 2)

		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, fickle, 1))
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)

		vote, err := repo.GetVote(ctx, suggestion.ID, steady)
		require.NoError(t, err)
		assert.Equal(t, 1, vote.Value)
	})

	// Two guards produce this answer and only one of them can ever run: the
	// pre-flight existence check returns first, which makes AtomicVote's
	// SQLSTATE 23503 branch on the INSERT unreachable outside a race with a
	// concurrent delete. Removing either one alone leaves the behaviour intact;
	// removing both is what this asserts against.
	t.Run("reports a suggestion that does not exist", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		err := repo.AtomicVote(ctx, 987654321, "did:plc:voter00000000000a5", 1)
		require.ErrorIs(t, err, communitysuggestions.ErrSuggestionNotFound,
			"a vote for a suggestion the board never had is a 404 rather than a foreign key error "+
				"the handler cannot classify")
	})

	t.Run("rejects a vote value the CHECK constraint does not allow", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		err := repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter00000000000a6", 4)
		require.ErrorIs(t, err, communitysuggestions.ErrInvalidVoteValue)

		suggestionAssertReconciled(t, db, suggestion.ID, 0, 0)
	})

	t.Run("a rejected vote leaves neither a row nor a counter change behind", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter00000000000a7", 1))

		require.Error(t, repo.AtomicVote(ctx, suggestion.ID, "did:plc:voter00000000000a8", 0))

		// The whole method is one transaction; a partial apply here would be
		// counter drift nothing ever repairs.
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})

	t.Run("votes never leak onto another suggestion", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		voted := suggestionSeeded(t, repo)
		bystander := suggestionSeeded(t, repo)

		require.NoError(t, repo.AtomicVote(ctx, voted.ID, "did:plc:voter00000000000a9", 1))
		require.NoError(t, repo.AtomicVote(ctx, voted.ID, "did:plc:voter00000000000b1", -1))

		suggestionAssertReconciled(t, db, voted.ID, 0, 2)
		suggestionAssertReconciled(t, db, bystander.ID, 0, 0)
	})

	// A ten-voter mixed sequence with toggles and flips in it, reconciled once at
	// the end. Each step is individually plausible; the point is that the running
	// deltas still add up after a sequence no single test case describes.
	t.Run("a long mixed sequence leaves the counter equal to the rows", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		for _, step := range []struct {
			voter string
			value int
		}{
			{"did:plc:voter00000000000x1", 1},
			{"did:plc:voter00000000000x2", 1},
			{"did:plc:voter00000000000x3", -1},
			{"did:plc:voter00000000000x1", -1}, // flip to down
			{"did:plc:voter00000000000x4", 1},
			{"did:plc:voter00000000000x2", 1}, // toggle off
			{"did:plc:voter00000000000x3", 1}, // flip to up
			{"did:plc:voter00000000000x5", -1},
			{"did:plc:voter00000000000x4", 1}, // toggle off
			{"did:plc:voter00000000000x1", 1}, // flip back to up
		} {
			require.NoErrorf(t, repo.AtomicVote(ctx, suggestion.ID, step.voter, step.value),
				"voting %+d as %s", step.value, step.voter)
		}

		// x1 up, x3 up, x5 down; x2 and x4 toggled themselves off.
		suggestionAssertReconciled(t, db, suggestion.ID, 1, 3)
	})
}

// TestSuggestionRepo_AtomicVoteFlipToAnInvalidValueIsMisclassified pins the
// third instance of the same missing mapping.
//
// KNOWN DEFECT: AtomicVote maps valid_vote_value on the INSERT path only. A
// voter who has already voted and then sends an out-of-range value takes the
// flip branch (the value differs from the existing one), where the UPDATE's
// constraint violation is wrapped generically. Same malformed request, different
// classification, decided by whether the person had voted before.
func TestSuggestionRepo_AtomicVoteFlipToAnInvalidValueIsMisclassified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, db := suggestionRepoAndDB(t)
	suggestion := suggestionSeeded(t, repo)
	voter := "did:plc:voter00000000000fl"

	require.NoError(t, repo.AtomicVote(ctx, suggestion.ID, voter, 1))

	err := repo.AtomicVote(ctx, suggestion.ID, voter, 7)
	require.Error(t, err)
	assert.NotErrorIs(t, err, communitysuggestions.ErrInvalidVoteValue,
		"IF THIS FAILED (issue 2026-07-31-suggestion-vote-error-misclassification-and-lock-order.md), the flip branch now maps valid_vote_value and the defect is FIXED — assert "+
			"ErrInvalidVoteValue here and delete this pin")
	assert.ErrorContains(t, err, "failed to update vote during flip")

	suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
}

// TestSuggestionRepo_ConcurrentVotesDoNotLoseCount drives distinct voters at one
// suggestion from several goroutines at once.
//
// The read-modify-write in AtomicVote is the classic lost-update shape: it reads
// the existing vote, decides a delta in Go, and applies it. What saves it is that
// the counter update is `vote_count = vote_count + $1` evaluated inside the
// transaction, on a row already locked by the existence check — not a value read
// out, incremented in Go and written back. A single-threaded test cannot tell
// those two implementations apart; this one can.
func TestSuggestionRepo_ConcurrentVotesDoNotLoseCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, db := suggestionRepoAndDB(t)
	suggestion := suggestionSeeded(t, repo)

	voters := []string{
		"did:plc:voter00000000000c1",
		"did:plc:voter00000000000c2",
		"did:plc:voter00000000000c3",
		"did:plc:voter00000000000c4",
		"did:plc:voter00000000000c5",
		"did:plc:voter00000000000c6",
	}

	var waitGroup sync.WaitGroup
	errs := make(chan error, len(voters))
	start := make(chan struct{})
	for _, voter := range voters {
		waitGroup.Add(1)
		go func(voter string) {
			defer waitGroup.Done()
			<-start
			errs <- repo.AtomicVote(ctx, suggestion.ID, voter, 1)
		}(voter)
	}
	close(start)
	waitGroup.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "every vote is from a different DID, so none of them contend for the "+
			"same suggestion_votes row and none should fail")
	}

	suggestionAssertReconciled(t, db, suggestion.ID, len(voters), len(voters))
}

func TestSuggestionRepo_GetVote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips the whole vote row", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000g1"
		before := time.Now().Add(-time.Minute)

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, -1)
		require.NoError(t, err)

		vote, err := repo.GetVote(ctx, suggestion.ID, voter)
		require.NoError(t, err)
		assert.NotZero(t, vote.ID)
		assert.Equal(t, suggestion.ID, vote.SuggestionID)
		assert.Equal(t, voter, vote.VoterDID)
		assert.Equal(t, -1, vote.Value,
			"the value is the viewer state a client renders as a highlighted arrow; reading it back "+
				"wrong shows the user voting the opposite way to how they voted")
		assert.True(t, vote.CreatedAt.After(before),
			"created_at is stamped by the database default, and a zero value means the column was "+
				"never scanned")
	})

	t.Run("reports a vote nobody cast", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		_, err := repo.GetVote(ctx, suggestion.ID, "did:plc:voter00000000000g2")
		require.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"'this user has not voted' is the normal case for almost every viewer; surfacing it as a "+
				"query failure would make the board unrenderable for anyone logged in")
	})

	t.Run("is scoped to the suggestion and the voter together", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		here := suggestionSeeded(t, repo)
		there := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000g3"
		other := "did:plc:voter00000000000g4"

		_, err := repo.UpsertVote(ctx, here.ID, voter, 1)
		require.NoError(t, err)

		_, err = repo.GetVote(ctx, there.ID, voter)
		assert.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"a vote on one suggestion must not be reported on another")

		_, err = repo.GetVote(ctx, here.ID, other)
		assert.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
			"and one person's vote must not be reported as somebody else's")
	})

	// unique_suggestion_voter is what makes the single-row GetVote well defined.
	// Exercised by direct SQL because the repository's own paths are written
	// precisely to avoid tripping it, which means nothing above this line proves
	// the index is actually there.
	t.Run("the database refuses a second vote row for the same pair", func(t *testing.T) {
		t.Parallel()
		repo, db := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)
		voter := "did:plc:voter00000000000g5"

		_, err := repo.UpsertVote(ctx, suggestion.ID, voter, 1)
		require.NoError(t, err)

		_, err = db.ExecContext(ctx,
			`INSERT INTO suggestion_votes (suggestion_id, voter_did, value) VALUES ($1, $2, $3)`,
			suggestion.ID, voter, -1)
		require.Error(t, err, "one person, one vote is enforced by unique_suggestion_voter and by "+
			"nothing else; without the index a retried request would count twice and GetVote would "+
			"return whichever row Postgres happened to reach first")

		pqErr := extractPQError(err)
		require.NotNil(t, pqErr)
		assert.Equal(t, "unique_suggestion_voter", pqErr.Constraint)

		suggestionAssertReconciled(t, db, suggestion.ID, 1, 1)
	})
}

func TestSuggestionRepo_GetVotesForViewer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns the viewer's own votes keyed by suggestion", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		first := suggestionSeeded(t, repo)
		second := suggestionSeeded(t, repo)
		third := suggestionSeeded(t, repo)
		viewer := "did:plc:viewer0000000000v1"

		require.NoError(t, repo.AtomicVote(ctx, first.ID, viewer, 1))
		require.NoError(t, repo.AtomicVote(ctx, second.ID, viewer, -1))

		votes, err := repo.GetVotesForViewer(ctx, viewer, []int64{first.ID, second.ID, third.ID})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{first.ID: 1, second.ID: -1}, votes,
			"this map is the viewer state for a whole page of the board in one query; a suggestion "+
				"the viewer has not voted on must be ABSENT rather than present with a zero, because "+
				"the service renders presence as a highlighted arrow")
	})

	// The viewer scoping is the security-relevant half. This map decides which
	// arrows are lit for the person looking at the page, and a missing voter_did
	// filter would light them from whoever voted last — leaking one user's voting
	// record to every other user.
	t.Run("one viewer's votes never appear for another", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		first := suggestionSeeded(t, repo)
		second := suggestionSeeded(t, repo)
		mine := "did:plc:viewer0000000000m1"
		theirs := "did:plc:viewer0000000000t1"

		require.NoError(t, repo.AtomicVote(ctx, first.ID, mine, 1))
		require.NoError(t, repo.AtomicVote(ctx, first.ID, theirs, -1))
		require.NoError(t, repo.AtomicVote(ctx, second.ID, theirs, 1))

		mineVotes, err := repo.GetVotesForViewer(ctx, mine, []int64{first.ID, second.ID})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{first.ID: 1}, mineVotes,
			"the second suggestion was voted on by somebody else, and it must not appear here")

		theirVotes, err := repo.GetVotesForViewer(ctx, theirs, []int64{first.ID, second.ID})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{first.ID: -1, second.ID: 1}, theirVotes)
	})

	t.Run("returns only the suggestions asked about", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		asked := suggestionSeeded(t, repo)
		unasked := suggestionSeeded(t, repo)
		viewer := "did:plc:viewer0000000000v2"

		require.NoError(t, repo.AtomicVote(ctx, asked.ID, viewer, 1))
		require.NoError(t, repo.AtomicVote(ctx, unasked.ID, viewer, 1))

		votes, err := repo.GetVotesForViewer(ctx, viewer, []int64{asked.ID})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{asked.ID: 1}, votes,
			"the ANY($2) filter bounds the result to one page's worth; without it every request "+
				"would return the viewer's entire voting history")
	})

	t.Run("unknown suggestion ids are simply absent", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		onTheBoard := suggestionSeeded(t, repo)
		viewer := "did:plc:viewer0000000000v3"

		require.NoError(t, repo.AtomicVote(ctx, onTheBoard.ID, viewer, 1))

		votes, err := repo.GetVotesForViewer(ctx, viewer, []int64{onTheBoard.ID, 987654321})
		require.NoError(t, err, "an id that is not on the board is not an error; the caller assembled "+
			"the list from a page it just read, and a race with a deletion must not fail the render")
		assert.Equal(t, map[int64]int{onTheBoard.ID: 1}, votes)
	})

	// The early return exists so a logged-out or empty page does not issue a
	// query with an empty array, and it must hand back a usable map: a nil map
	// reads fine but panics on assignment, and the service writes into it.
	t.Run("an empty id list short-circuits to an empty, usable map", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)

		votes, err := repo.GetVotesForViewer(ctx, "did:plc:viewer0000000000v4", nil)
		require.NoError(t, err)
		require.NotNil(t, votes, "a nil map here would panic the first time the service assigned into it")
		assert.Empty(t, votes)

		votes[1] = 1
		assert.Len(t, votes, 1, "and the map must be writable, not just non-nil")
	})

	t.Run("a viewer who has voted on nothing gets an empty map", func(t *testing.T) {
		t.Parallel()
		repo, _ := suggestionRepoAndDB(t)
		suggestion := suggestionSeeded(t, repo)

		votes, err := repo.GetVotesForViewer(ctx, "did:plc:viewer0000000000v5", []int64{suggestion.ID})
		require.NoError(t, err)
		require.NotNil(t, votes)
		assert.Empty(t, votes)
	})
}

// TestSuggestionRepo_DeletingASuggestionTakesItsVotesWithIt covers the ON DELETE
// CASCADE on suggestion_votes, which is the only thing standing between a
// declined-and-purged suggestion and vote rows pointing at an id nothing
// resolves. Deletion has no repository method — an admin does it by hand or a
// future cleanup job will — so the cascade is asserted against the schema
// directly.
func TestSuggestionRepo_DeletingASuggestionTakesItsVotesWithIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, db := suggestionRepoAndDB(t)
	doomed := suggestionSeeded(t, repo)
	survivor := suggestionSeeded(t, repo)
	voter := "did:plc:voter00000000000cs"

	require.NoError(t, repo.AtomicVote(ctx, doomed.ID, voter, 1))
	require.NoError(t, repo.AtomicVote(ctx, survivor.ID, voter, 1))

	_, err := db.ExecContext(ctx, `DELETE FROM community_suggestions WHERE id = $1`, doomed.ID)
	require.NoError(t, err, "the delete itself must not be blocked by the referencing votes")

	_, err = repo.GetByID(ctx, doomed.ID)
	assert.ErrorIs(t, err, communitysuggestions.ErrSuggestionNotFound)

	_, err = repo.GetVote(ctx, doomed.ID, voter)
	assert.ErrorIs(t, err, communitysuggestions.ErrVoteNotFound,
		"a vote outliving its suggestion is a row no query can join and no user can withdraw")

	suggestionAssertReconciled(t, db, survivor.ID, 1, 1)
}
