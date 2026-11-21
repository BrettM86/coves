//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/adminreports"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chunk 14 B6: erasing an account removes the reports it filed, its name as
// the resolver of reports it dismissed (the dismissed reports stay), and the
// dismissal records naming it. Reports by other accounts stay, and a report the
// erased account tries to file afterwards is refused.

const (
	erasedReporter      = "did:plc:rqerasedx"
	erasureReportOtherY = "did:plc:rqerasurey"
	erasureReportOtherZ = "did:plc:rqerasurez"
)

func TestUserRepo_Delete_RemovesReportsAndDismissalsOfTheErasedAccount(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := t.Context()
	createTestUser(t, f.db, "rqerasedx"+testkit.UniqueID(t)+".test", erasedReporter)
	communityDID := hostedCommunity(t, f.db, "rqerase")
	reportedPost := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqerasep1", "reported", communityQueuePostCreated)
	dismissedPost := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqerasep2", "dismissed", communityQueuePostCreated)

	f.report(t, erasedReporter, reportedPost, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	f.report(t, erasedReporter, reportedPost, adminreports.TargetTypePost, adminreports.ReasonCSAM, time.Date(2026, 11, 3, 9, 5, 0, 0, time.UTC))
	otherReport := f.report(t, erasureReportOtherY, reportedPost, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	dismissedReport := f.report(t, erasureReportOtherZ, dismissedPost, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))

	dismissed, err := f.queue.DismissCommunitySubject(ctx, adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityDID, SubjectURI: dismissedPost, ModeratorDID: erasedReporter, IdempotencyKey: "erase-dismiss-1",
	})
	require.NoError(t, err)
	require.Equal(t, adminreports.DismissResult{DismissedCount: 1}, dismissed, "fixture: the erased account dismissed one report")
	require.Equal(t, erasedReporter, readCommunityQueueReportState(t, f.db, dismissedReport.ID).ResolvedBy.String,
		"fixture: the dismissed report names the erased account as resolver")
	otherBefore := erasureRoleRows(t, f.db, "admin_reports", "id = $1", otherReport.ID)

	require.NoError(t, NewUserRepository(f.db).Delete(ctx, erasedReporter))

	assert.Equal(t, "[]", erasureRoleRows(t, f.db, "admin_reports", "reporter_did = $1", erasedReporter),
		"no report filed by the erased account remains")
	assert.Equal(t, "[]", erasureRoleRows(t, f.db, "admin_reports", "resolved_by = $1", erasedReporter),
		"no report names the erased account as resolver")
	dismissedAfter := readCommunityQueueReportState(t, f.db, dismissedReport.ID)
	assert.Equal(t, "dismissed", dismissedAfter.Status, "the report the erased account dismissed stays dismissed")
	assert.False(t, dismissedAfter.ResolvedBy.Valid, "the dismissed report names no resolver")
	assert.Equal(t, "[]", erasureRoleRows(t, f.db, "community_report_dismissals", "dismissed_by_did = $1", erasedReporter),
		"no dismissal record names the erased account")
	assert.Equal(t, otherBefore, erasureRoleRows(t, f.db, "admin_reports", "id = $1", otherReport.ID),
		"another account's report is untouched")
	assert.Equal(t, []adminreports.CommunityQueueEntry{{
		SubjectURI:      reportedPost,
		SubjectType:     adminreports.TargetTypePost,
		Reasons:         []adminreports.Reason{adminreports.ReasonSpam},
		ReportCount:     1,
		FirstReportedAt: time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC),
		LastReportedAt:  time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC),
	}}, f.list(t, communityDID), "the community's queue no longer counts the erased account's report")
}

// Chunk 14 R2: once an account is erased, a report it files is refused with
// ErrReporterErased and nothing is stored.
func TestUserRepo_Delete_RefusesReportsFromTheErasedAccount(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := t.Context()
	createTestUser(t, f.db, "rqerasedx"+testkit.UniqueID(t)+".test", erasedReporter)
	require.NoError(t, NewUserRepository(f.db).Delete(ctx, erasedReporter))

	err := f.reports.Create(ctx, &adminreports.Report{
		ReporterDID: erasedReporter,
		TargetURI:   "at://did:plc:rqauthor/social.coves.community.postv2/rqerasedlate",
		TargetType:  adminreports.TargetTypePost,
		Reason:      adminreports.ReasonSpam,
		Status:      adminreports.StatusOpen,
	})

	assert.ErrorIs(t, err, adminreports.ErrReporterErased, "a report by an erased account is refused")
	assert.Equal(t, "[]", erasureRoleRows(t, f.db, "admin_reports", "reporter_did = $1", erasedReporter),
		"nothing is stored for the erased account")
}

// Chunk 14 DoD 7: once a moderator's account is erased, a dismissal it sends
// is refused, no dismissal record is stored, and the report stays open.
func TestUserRepo_Delete_RefusesDismissalsFromTheErasedAccount(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := t.Context()
	createTestUser(t, f.db, "rqerasedx"+testkit.UniqueID(t)+".test", erasedReporter)
	communityDID := hostedCommunity(t, f.db, "rqerased")
	reportedPost := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqerasep3", "reported", communityQueuePostCreated)
	report := f.report(t, erasureReportOtherY, reportedPost, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	require.NoError(t, NewUserRepository(f.db).Delete(ctx, erasedReporter))

	result, err := f.queue.DismissCommunitySubject(ctx, adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityDID, SubjectURI: reportedPost, ModeratorDID: erasedReporter, IdempotencyKey: "erased-dismiss-1",
	})

	assert.ErrorIs(t, err, adminreports.ErrCommunityQueueForbidden, "a dismissal by an erased account is refused")
	assert.Equal(t, adminreports.DismissResult{}, result, "the refused dismissal dismisses nothing")
	assert.Equal(t, "[]", erasureRoleRows(t, f.db, "community_report_dismissals", "dismissed_by_did = $1", erasedReporter),
		"no dismissal record names the erased account")
	state := readCommunityQueueReportState(t, f.db, report.ID)
	assert.Equal(t, "open", state.Status, "the report stays open")
	assert.False(t, state.ResolvedBy.Valid, "the report names no resolver")
}
