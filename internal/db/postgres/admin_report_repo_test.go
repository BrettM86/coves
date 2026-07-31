//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/adminreports"
	"Coves/tests/testkit"
)

// admin_report_repo.go against real SQL: the table behind the "report this to
// the admins" button.
//
// This is the escalation path of last resort. A user who has found CSAM, a
// doxing post or an illegal listing has no other channel — there is no email
// address and no moderation queue outside this table — so a report that does
// not land, or that lands under the wrong status and is therefore never listed,
// is content nobody is coming to look at. Every function in the file was at zero
// coverage, which meant nothing had ever proved that a report survives the round
// trip to Postgres at all.
//
// Three things here are only decidable against a real server, and are the
// reason these tests are integration tests rather than unit tests with a fake:
//
//   - The two CHECK constraints (valid_reason, valid_status) are the ONLY
//     enforcement of the report vocabulary on the write path. Create and
//     UpdateStatus both translate the constraint name into a domain error, and
//     that translation is string matching against a value only Postgres
//     produces.
//   - explanation, resolved_by, resolution_notes and resolved_at are nullable,
//     and the repository writes NULL for an absent explanation while reading
//     every one of them back through a sql.Null* wrapper. A round trip is the
//     only way to see that the NULL survives as "" and not as a scan error.
//   - created_at is not in the INSERT at all. The database clock stamps it, and
//     ListByStatus orders by it, so what a caller passes in is discarded — a
//     fact no test that only reads back its own inputs would ever notice.
//
// Unlike most tables in this schema, admin_reports has no foreign keys:
// reporter_did and target_uri are bare TEXT. That is deliberate — a report must
// be storable about content the AppView has not indexed, and about a reporter
// on a PDS this instance has never seen — so these tests seed nothing, and the
// absence of seeding is the point rather than an omission.

// adminReportRepoAndDB returns the repository under test alongside the raw
// handle for the same private database, because several claims here ("the
// explanation is NULL, not empty string", "created_at is what the server wrote")
// are not observable through the repository's own read path.
func adminReportRepoAndDB(t *testing.T) (adminreports.Repository, *sql.DB) {
	t.Helper()
	db := testkit.DB(t)
	return NewAdminReportRepository(db), db
}

// adminReportFixture is a report that will pass both CHECK constraints, so a
// test that is about one field does not have to restate the other five.
func adminReportFixture(t *testing.T) *adminreports.Report {
	t.Helper()
	id := testkit.UniqueID(t)
	return &adminreports.Report{
		ReporterDID: "did:plc:reporter" + id,
		TargetURI:   "at://did:plc:author" + id + "/social.coves.community.post/rkey" + id,
		TargetType:  adminreports.TargetTypePost,
		Reason:      adminreports.ReasonSpam,
		Explanation: "unsolicited bulk posting",
		Status:      adminreports.StatusOpen,
	}
}

// adminReportBackdate moves a report's created_at, so ordering and pagination
// are decided by fixed values rather than by how fast the inserts ran.
func adminReportBackdate(t *testing.T, db *sql.DB, id int64, at time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE admin_reports SET created_at = $1 WHERE id = $2`, at, id)
	require.NoError(t, err, "backdating the report the ordering assertion depends on")
}

// adminReportIDs renders a listing as the identifiers alone, so an ordering
// failure prints the sequence rather than four struct dumps.
func adminReportIDs(reports []*adminreports.Report) []int64 {
	ids := make([]int64, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.ID)
	}
	return ids
}

func TestAdminReportRepo_Create(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips every column through Postgres", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Reason = adminreports.ReasonCSAM
		report.TargetType = adminreports.TargetTypeComment
		report.Explanation = "child sexual abuse material in the third image"
		require.NoError(t, repo.Create(ctx, report))

		require.NotZero(t, report.ID, "the BIGSERIAL primary key is the only handle an admin has on a "+
			"report; without it UpdateStatus can never resolve this row")

		listed, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		got := listed[0]

		assert.Equal(t, report.ID, got.ID)
		assert.Equal(t, report.ReporterDID, got.ReporterDID)
		assert.Equal(t, report.TargetURI, got.TargetURI,
			"the target URI is the only pointer to the content being reported; an admin with a report "+
				"and no target has nothing to act on")
		assert.Equal(t, adminreports.TargetTypeComment, got.TargetType)
		assert.Equal(t, adminreports.ReasonCSAM, got.Reason,
			"the reason decides the queue and the legal obligation attached to it; csam read back as "+
				"anything else is a report that misses its escalation")
		assert.Equal(t, "child sexual abuse material in the third image", got.Explanation)
		assert.Equal(t, adminreports.StatusOpen, got.Status)
		assert.Nil(t, got.ResolvedBy, "a report nobody has touched must not name a resolver")
		assert.Nil(t, got.ResolutionNotes)
		assert.Nil(t, got.ResolvedAt)
	})

	// created_at is absent from the INSERT column list, so the server's NOW()
	// wins and whatever the caller had in the struct is discarded. That matters
	// because ListByStatus orders by created_at: if a client's clock could set
	// it, a client could pin its own report to the top of the admin queue —
	// or bury someone else's at the bottom.
	t.Run("stamps created_at from the database clock, not from the caller", func(t *testing.T) {
		t.Parallel()
		repo, db := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.CreatedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
		before := time.Now().Add(-time.Minute)
		require.NoError(t, repo.Create(ctx, report))

		assert.True(t, report.CreatedAt.After(before),
			"Create returned %s: the caller's 1999 timestamp reached the row, which means a client "+
				"controls its own position in the admin queue", report.CreatedAt)

		var stored time.Time
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT created_at FROM admin_reports WHERE id = $1`, report.ID).Scan(&stored))
		assert.WithinDuration(t, stored, report.CreatedAt, time.Millisecond,
			"the timestamp handed back by RETURNING must be the one in the row")
	})

	// The repository maps "" to a SQL NULL rather than storing an empty string.
	// Both read back as "" through scanReport, so the distinction is invisible
	// from Go — but it is not invisible to an admin querying the table by hand,
	// and it is what keeps "no explanation given" distinguishable from "the
	// reporter submitted a blank one".
	t.Run("stores an absent explanation as NULL rather than an empty string", func(t *testing.T) {
		t.Parallel()
		repo, db := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Explanation = ""
		require.NoError(t, repo.Create(ctx, report))

		var isNull bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT explanation IS NULL FROM admin_reports WHERE id = $1`, report.ID).Scan(&isNull))
		assert.True(t, isNull, "an empty explanation was written as '' instead of NULL")

		listed, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "", listed[0].Explanation,
			"and a NULL explanation must scan back as the empty string, not fail the whole listing")
	})

	t.Run("defaults an unset status to open", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Status = ""
		require.NoError(t, repo.Create(ctx, report))

		assert.Equal(t, adminreports.StatusOpen, report.Status,
			"Create writes the defaulted status back onto the struct, so the caller's copy agrees "+
				"with the row")

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		assert.Len(t, open, 1, "a report created with no status must land in the open queue; the one "+
			"queue an admin actually reads is 'open', so anywhere else is a report nobody sees")
	})

	t.Run("honours an explicit status instead of forcing open", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Status = adminreports.StatusReviewing
		require.NoError(t, repo.Create(ctx, report))

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		assert.Empty(t, open)

		reviewing, err := repo.ListByStatus(ctx, string(adminreports.StatusReviewing), 10, 0)
		require.NoError(t, err)
		require.Len(t, reviewing, 1)
		assert.Equal(t, adminreports.StatusReviewing, reviewing[0].Status)
	})

	// valid_reason is the whole vocabulary check. There is no Go-side guard on
	// the repository path — adminreports.SubmitReportRequest.Validate has one,
	// but the repository is also reachable from anywhere else — so the CHECK
	// constraint plus this translation is what stops an unroutable reason
	// reaching the table.
	t.Run("translates a reason the CHECK constraint rejects into a domain error", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Reason = adminreports.Reason("vibes")

		err := repo.Create(ctx, report)
		require.ErrorIs(t, err, adminreports.ErrInvalidReason,
			"a raw SQLSTATE 23514 here would make the handler answer 500 to what is a malformed "+
				"request, and would put the constraint name in front of a user")
	})

	t.Run("translates a status the CHECK constraint rejects into a domain error", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		report := adminReportFixture(t)
		report.Status = adminreports.Status("escalated")

		err := repo.Create(ctx, report)
		require.ErrorIs(t, err, adminreports.ErrInvalidStatus)
	})

	t.Run("accepts every reason in the lexicon", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		for _, reason := range adminreports.ValidReasons() {
			report := adminReportFixture(t)
			report.Reason = reason
			require.NoErrorf(t, repo.Create(ctx, report),
				"reason %q is in ValidReasons() but the CHECK constraint refused it: the Go vocabulary "+
					"and the SQL vocabulary have drifted apart, and the half that loses is whichever "+
					"one the reporter's client believes", reason)
		}

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 100, 0)
		require.NoError(t, err)
		assert.Len(t, open, len(adminreports.ValidReasons()))
	})

	t.Run("accepts every status in the lexicon", func(t *testing.T) {
		t.Parallel()
		repo, _ := adminReportRepoAndDB(t)

		for _, status := range adminreports.ValidStatuses() {
			report := adminReportFixture(t)
			report.Status = status
			require.NoErrorf(t, repo.Create(ctx, report),
				"status %q is in ValidStatuses() but the CHECK constraint refused it", status)

			listed, err := repo.ListByStatus(ctx, string(status), 10, 0)
			require.NoError(t, err)
			require.Len(t, listed, 1, "a report created as %q must be findable under %q", status, status)
		}
	})
}

// TestAdminReportRepo_TargetTypeIsUnconstrained pins a gap between the
// repository and the schema.
//
// KNOWN DEFECT: Create has a branch mapping a "valid_target_type" constraint
// violation onto adminreports.ErrInvalidTargetType, but migration
// 028_create_admin_reports_table.sql declares no such constraint — target_type
// is a bare TEXT column with only a comment saying it should be 'post' or
// 'comment'. The branch is therefore unreachable and the column accepts
// anything, so an admin listing renders a target type no client knows how to
// link to.
//
// It is currently masked: the only production writer is adminreports.NewReport,
// which derives the type from the URI and can only produce "post" or "comment".
// The exposure is any future caller that builds a Report directly.
func TestAdminReportRepo_TargetTypeIsUnconstrained(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := adminReportRepoAndDB(t)

	report := adminReportFixture(t)
	report.TargetType = adminreports.TargetType("spreadsheet")

	err := repo.Create(ctx, report)
	require.NoError(t, err,
		"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) a valid_target_type CHECK constraint was added to admin_reports and the "+
			"defect is FIXED — assert ErrInvalidTargetType here and delete this pin")

	listed, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, adminreports.TargetType("spreadsheet"), listed[0].TargetType,
		"the correct behaviour would be to reject the write; today the garbage value is stored and "+
			"read straight back out")
	assert.False(t, adminreports.IsValidTargetType(string(listed[0].TargetType)),
		"and the domain layer agrees the stored value is not a target type it recognises")
}

func TestAdminReportRepo_ListByStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Four reports across two statuses, with created_at forced to fixed,
	// distinct values so both the ORDER BY and the offsets are decided by the
	// data rather than by insert timing.
	seed := func(t *testing.T) (adminreports.Repository, *sql.DB, map[string]int64) {
		t.Helper()
		repo, db := adminReportRepoAndDB(t)
		base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
		ids := map[string]int64{}
		for _, spec := range []struct {
			label  string
			status adminreports.Status
			days   int
		}{
			{"oldest-open", adminreports.StatusOpen, 0},
			{"newest-open", adminreports.StatusOpen, 3},
			{"middle-open", adminreports.StatusOpen, 1},
			{"resolved-one", adminreports.StatusResolved, 2},
		} {
			report := adminReportFixture(t)
			report.Status = spec.status
			report.Explanation = spec.label
			require.NoError(t, repo.Create(ctx, report))
			adminReportBackdate(t, db, report.ID, base.AddDate(0, 0, spec.days))
			ids[spec.label] = report.ID
		}
		return repo, db, ids
	}

	t.Run("returns only the status asked for", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 100, 0)
		require.NoError(t, err)
		assert.ElementsMatch(t,
			[]int64{ids["oldest-open"], ids["newest-open"], ids["middle-open"]},
			adminReportIDs(open),
			"the WHERE clause is what makes 'open' an admin queue rather than a full table dump; a "+
				"resolved report reappearing in it is work already done, done twice")

		resolved, err := repo.ListByStatus(ctx, string(adminreports.StatusResolved), 100, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["resolved-one"]}, adminReportIDs(resolved))
	})

	t.Run("lists newest first", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 100, 0)
		require.NoError(t, err)
		assert.Equal(t,
			[]int64{ids["newest-open"], ids["middle-open"], ids["oldest-open"]},
			adminReportIDs(open),
			"reports are triaged newest first, and the ordering is also what makes LIMIT/OFFSET "+
				"paging stable; oldest-first would hide today's CSAM report behind every report "+
				"ever filed")
	})

	t.Run("pages without skipping or repeating a report", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		first, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 2, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["newest-open"], ids["middle-open"]}, adminReportIDs(first))

		second, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 2, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{ids["oldest-open"]}, adminReportIDs(second),
			"the second page must start exactly where the first stopped: an off-by-one in the offset "+
				"drops a report out of the queue entirely")

		past, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 2, 99)
		require.NoError(t, err)
		assert.Empty(t, past, "reading past the end is an empty page, not an error")
	})

	t.Run("a limit of one returns exactly the newest report", func(t *testing.T) {
		t.Parallel()
		repo, _, ids := seed(t)

		page, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 1, 0)
		require.NoError(t, err)
		require.Len(t, page, 1, "LIMIT 1 must bound the result: an unbounded admin queue is the "+
			"pagination bug that takes the page down once the table is large")
		assert.Equal(t, ids["newest-open"], page[0].ID)
	})

	// ListByStatus accumulates into a nil slice, so an empty result is nil
	// rather than []. There is no production caller today (the admin listing
	// endpoint is not built yet), so this is recorded rather than filed: the
	// first HTTP handler to marshal this straight to JSON will emit `null`,
	// and clients written against `[]` will read that as an error.
	t.Run("an unused status yields an empty listing", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		dismissed, err := repo.ListByStatus(ctx, string(adminreports.StatusDismissed), 10, 0)
		require.NoError(t, err, "a status with no reports is an empty queue, not a failure")
		assert.Empty(t, dismissed)
	})

	t.Run("a status outside the vocabulary matches nothing rather than erroring", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := seed(t)

		listed, err := repo.ListByStatus(ctx, "escalated", 10, 0)
		require.NoError(t, err, "the read path has no CHECK constraint to trip: an unknown status is "+
			"simply a filter nothing matches")
		assert.Empty(t, listed)
	})
}

func TestAdminReportRepo_UpdateStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	openReport := func(t *testing.T) (adminreports.Repository, *sql.DB, *adminreports.Report) {
		t.Helper()
		repo, db := adminReportRepoAndDB(t)
		report := adminReportFixture(t)
		require.NoError(t, repo.Create(ctx, report))
		return repo, db, report
	}

	// Resolving is the terminal transition: it is what takes a report out of the
	// admin queue, and the three resolution columns are the audit trail for why.
	// A resolution with no resolver recorded is an action nobody is accountable
	// for.
	t.Run("resolving records the resolver, the notes and the time", func(t *testing.T) {
		t.Parallel()
		repo, _, report := openReport(t)
		before := time.Now().Add(-time.Minute)

		require.NoError(t, repo.UpdateStatus(ctx, report.ID,
			string(adminreports.StatusResolved), "did:plc:admin00000001", "content removed, account suspended"))

		resolved, err := repo.ListByStatus(ctx, string(adminreports.StatusResolved), 10, 0)
		require.NoError(t, err)
		require.Len(t, resolved, 1)
		got := resolved[0]

		assert.Equal(t, adminreports.StatusResolved, got.Status)
		require.NotNil(t, got.ResolvedBy, "a resolved report with a NULL resolver is a moderation "+
			"action with nobody's name on it")
		assert.Equal(t, "did:plc:admin00000001", *got.ResolvedBy)
		require.NotNil(t, got.ResolutionNotes)
		assert.Equal(t, "content removed, account suspended", *got.ResolutionNotes)
		require.NotNil(t, got.ResolvedAt, "resolved_at is how long a report sat in the queue; without "+
			"it there is no way to measure whether reports are being handled at all")
		assert.True(t, got.ResolvedAt.After(before),
			"resolved_at %s predates this test", got.ResolvedAt)

		stillOpen, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		assert.Empty(t, stillOpen, "a resolved report must leave the open queue")
	})

	t.Run("dismissing takes the same resolution path as resolving", func(t *testing.T) {
		t.Parallel()
		repo, _, report := openReport(t)

		require.NoError(t, repo.UpdateStatus(ctx, report.ID,
			string(adminreports.StatusDismissed), "did:plc:admin00000002", "not a violation"))

		dismissed, err := repo.ListByStatus(ctx, string(adminreports.StatusDismissed), 10, 0)
		require.NoError(t, err)
		require.Len(t, dismissed, 1)
		require.NotNil(t, dismissed[0].ResolvedBy)
		assert.Equal(t, "did:plc:admin00000002", *dismissed[0].ResolvedBy)
		require.NotNil(t, dismissed[0].ResolutionNotes)
		assert.Equal(t, "not a violation", *dismissed[0].ResolutionNotes)
		assert.NotNil(t, dismissed[0].ResolvedAt,
			"a dismissal is a decision too, and is recorded the same way a resolution is")
	})

	// Moving to "reviewing" runs the other branch, which touches only the status
	// column. That is the correct shape — a report being looked at has not been
	// resolved by anyone yet — and writing a resolver at this point would claim
	// the report was closed.
	t.Run("taking a report under review leaves the resolution columns NULL", func(t *testing.T) {
		t.Parallel()
		repo, _, report := openReport(t)

		require.NoError(t, repo.UpdateStatus(ctx, report.ID,
			string(adminreports.StatusReviewing), "did:plc:admin00000003", "picking this up"))

		reviewing, err := repo.ListByStatus(ctx, string(adminreports.StatusReviewing), 10, 0)
		require.NoError(t, err)
		require.Len(t, reviewing, 1)
		assert.Nil(t, reviewing[0].ResolvedBy,
			"a report under review has not been resolved; naming a resolver here would close it in "+
				"the audit trail while it is still open in the queue")
		assert.Nil(t, reviewing[0].ResolutionNotes)
		assert.Nil(t, reviewing[0].ResolvedAt)
	})

	t.Run("reports an id that does not exist as a domain not-found", func(t *testing.T) {
		t.Parallel()
		repo, _, _ := openReport(t)

		err := repo.UpdateStatus(ctx, 987654321,
			string(adminreports.StatusResolved), "did:plc:admin00000004", "nothing here")
		require.ErrorIs(t, err, adminreports.ErrReportNotFound,
			"an UPDATE that matched no rows returns nil from Postgres; without the RowsAffected check "+
				"an admin would be told they had resolved a report that does not exist")
	})

	t.Run("rejects a status the CHECK constraint does not allow", func(t *testing.T) {
		t.Parallel()
		repo, _, report := openReport(t)

		err := repo.UpdateStatus(ctx, report.ID, "escalated", "did:plc:admin00000005", "")
		require.ErrorIs(t, err, adminreports.ErrInvalidStatus,
			"the CHECK constraint is the only thing keeping the status column inside the vocabulary "+
				"the admin queue filters on; a value outside it is a report in no queue at all")

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		require.Len(t, open, 1)
		assert.Equal(t, adminreports.StatusOpen, open[0].Status,
			"and the rejected update left the row where it was")
	})

	t.Run("changes only the report named", func(t *testing.T) {
		t.Parallel()
		repo, _, first := openReport(t)
		bystander := adminReportFixture(t)
		require.NoError(t, repo.Create(ctx, bystander))

		require.NoError(t, repo.UpdateStatus(ctx, first.ID,
			string(adminreports.StatusResolved), "did:plc:admin00000006", "handled"))

		open, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
		require.NoError(t, err)
		require.Len(t, open, 1, "resolving one report closed another; a missing WHERE id = $n here "+
			"would empty the entire admin queue in one click")
		assert.Equal(t, bystander.ID, open[0].ID)
		assert.Nil(t, open[0].ResolvedBy)
	})

	// resolved_by is written verbatim, so an empty resolver string is stored as
	// '' rather than NULL and reads back as a non-nil pointer to "". Callers
	// checking `ResolvedBy != nil` therefore see a resolver that is not there.
	// Recorded rather than filed: every caller today passes an authenticated
	// admin DID, so the empty string is not reachable from production code.
	t.Run("an empty resolver is stored as an empty string, not NULL", func(t *testing.T) {
		t.Parallel()
		repo, db, report := openReport(t)

		require.NoError(t, repo.UpdateStatus(ctx, report.ID,
			string(adminreports.StatusResolved), "", ""))

		var resolvedByIsNull, notesIsNull bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT resolved_by IS NULL, resolution_notes IS NULL FROM admin_reports WHERE id = $1`,
			report.ID).Scan(&resolvedByIsNull, &notesIsNull))
		assert.False(t, resolvedByIsNull)
		assert.False(t, notesIsNull)

		resolved, err := repo.ListByStatus(ctx, string(adminreports.StatusResolved), 10, 0)
		require.NoError(t, err)
		require.Len(t, resolved, 1)
		require.NotNil(t, resolved[0].ResolvedBy,
			"a nil check on ResolvedBy cannot tell 'no resolver' from 'the empty resolver'")
		assert.Equal(t, "", *resolved[0].ResolvedBy)
	})
}

// TestAdminReportRepo_ReopeningKeepsTheStaleResolution pins a lost-update
// hazard in the status machine.
//
// KNOWN DEFECT: UpdateStatus only writes the resolution columns on the way IN
// to resolved/dismissed. Moving a report back to open or reviewing takes the
// short branch, which touches status alone — so resolved_by, resolution_notes
// and resolved_at survive the reopen. The row then reads as an open report that
// was resolved by someone at a particular time, and any admin view rendering
// "resolved by X" from a non-NULL column shows a resolution for a report that
// is back in the queue.
//
// The correct behaviour is to NULL the three columns when leaving a terminal
// status.
func TestAdminReportRepo_ReopeningKeepsTheStaleResolution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := adminReportRepoAndDB(t)

	report := adminReportFixture(t)
	require.NoError(t, repo.Create(ctx, report))
	require.NoError(t, repo.UpdateStatus(ctx, report.ID,
		string(adminreports.StatusResolved), "did:plc:admin00000007", "closed in error"))

	require.NoError(t, repo.UpdateStatus(ctx, report.ID, string(adminreports.StatusOpen), "", ""))

	reopened, err := repo.ListByStatus(ctx, string(adminreports.StatusOpen), 10, 0)
	require.NoError(t, err)
	require.Len(t, reopened, 1)

	require.NotNil(t, reopened[0].ResolvedBy,
		"IF THIS FAILED (issue 2026-07-31-admin-report-reopen-keeps-stale-resolution.md), reopening now clears resolved_by and the defect is FIXED — assert nil for "+
			"all three resolution columns and delete this pin")
	assert.Equal(t, "did:plc:admin00000007", *reopened[0].ResolvedBy,
		"an open report still names the admin who resolved it")
	require.NotNil(t, reopened[0].ResolutionNotes)
	assert.Equal(t, "closed in error", *reopened[0].ResolutionNotes)
	assert.NotNil(t, reopened[0].ResolvedAt,
		"and still carries a resolution time, so 'time to resolution' counts this report as closed "+
			"while it sits in the open queue")
}

// TestAdminReportRepo_ExtractPQError covers the classifier both error-mapping
// functions in this file depend on.
//
// It is one errors.As call, but it is the hinge: every domain error the two
// repositories in this package return for a constraint violation is chosen by
// reading pq.Error.Constraint, and if the type assertion stops matching, all of
// those translations fall through to the generic "failed to ..." wrapper at
// once. That failure is silent — the write still fails, but with an error no
// handler can classify — so it is worth pinning directly rather than only
// through its callers.
func TestAdminReportRepo_ExtractPQError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	// A genuine constraint violation from the server, rather than a hand-built
	// pq.Error: the field this function exists to expose is populated by the
	// driver from the wire protocol, and a fabricated value would prove nothing
	// about what Postgres actually sends.
	_, rawErr := db.ExecContext(ctx, `
		INSERT INTO admin_reports (reporter_did, target_uri, target_type, reason, status)
		VALUES ('did:plc:x', 'at://x/y/z', 'post', 'not-a-reason', 'open')`)
	require.Error(t, rawErr)

	t.Run("exposes the constraint name the mappers switch on", func(t *testing.T) {
		t.Parallel()
		pqErr := extractPQError(rawErr)
		require.NotNil(t, pqErr, "a CHECK violation straight from lib/pq must be recognised as one")
		assert.Equal(t, "valid_reason", pqErr.Constraint,
			"Create and UpdateStatus both branch on strings.Contains of this exact field; an empty "+
				"Constraint turns every constraint violation into an unclassifiable 500")
		assert.Equal(t, pq.ErrorCode("23514"), pqErr.Code,
			"23514 is check_violation, which is what distinguishes a bad value from a duplicate key")
	})

	t.Run("sees through a wrapped error", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("failed to create admin report: %w", rawErr)
		pqErr := extractPQError(wrapped)
		require.NotNil(t, pqErr, "errors.As, not a bare type assertion: repositories wrap as they "+
			"return, and a classifier that only matched the outermost error would classify nothing "+
			"once one more layer was added")
		assert.Equal(t, "valid_reason", pqErr.Constraint)
	})

	t.Run("returns nil for errors that did not come from Postgres", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, extractPQError(nil))
		assert.Nil(t, extractPQError(errors.New("context deadline exceeded")))
		assert.Nil(t, extractPQError(sql.ErrNoRows),
			"sql.ErrNoRows is not a constraint violation, and treating it as one would have the "+
				"mappers read a zero-value Constraint and match nothing anyway")
	})

	t.Run("reports a foreign key violation with its SQLSTATE", func(t *testing.T) {
		t.Parallel()
		_, fkErr := db.ExecContext(ctx,
			`INSERT INTO suggestion_votes (suggestion_id, voter_did, value) VALUES (999999, 'did:plc:x', 1)`)
		require.Error(t, fkErr)
		pqErr := extractPQError(fkErr)
		require.NotNil(t, pqErr)
		assert.Equal(t, pq.ErrorCode("23503"), pqErr.Code,
			"AtomicVote branches on this raw code rather than on a constraint name, so the code has "+
				"to survive extraction intact")
	})

	t.Run("carries the message text for the generic wrapper", func(t *testing.T) {
		t.Parallel()
		pqErr := extractPQError(rawErr)
		require.NotNil(t, pqErr)
		assert.True(t, strings.Contains(pqErr.Error(), "valid_reason"),
			"an unmapped constraint still has to be diagnosable from the log line alone")
	})
}
