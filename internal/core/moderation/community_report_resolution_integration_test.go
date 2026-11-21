//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chunk 14 B5: a community removal resolves the reports community moderators
// see on that subject, in the transaction that records the removal action.
// Reports are read back from admin_reports directly; csam and illegal reports
// stay open for the instance admins, and reports on any other subject are
// untouched.

const (
	reportResolutionSpamReporter       = "did:plc:rqresolvespam"
	reportResolutionHarassmentReporter = "did:plc:rqresolveharass"
	reportResolutionCSAMReporter       = "did:plc:rqresolvecsam"
	reportResolutionOtherReporter      = "did:plc:rqresolveother"
)

// reportResolutionRow is one admin_reports row's resolution columns.
type reportResolutionRow struct {
	Status     string
	ResolvedBy sql.NullString
	ResolvedAt sql.NullTime
}

// fileReport stores one open report through the production admin report
// repository and returns its id.
func (f *communityPostDBFixture) fileReport(t *testing.T, reporterDID, targetURI string, targetType adminreports.TargetType, reason adminreports.Reason) int64 {
	t.Helper()
	report := &adminreports.Report{
		ReporterDID: reporterDID, TargetURI: targetURI, TargetType: targetType,
		Reason: reason, Status: adminreports.StatusOpen,
	}
	require.NoError(t, postgres.NewAdminReportRepository(f.db, "", nil).Create(context.Background(), report))
	return report.ID
}

// reportRow is the report's resolution columns as admin_reports holds them.
func (f *communityPostDBFixture) reportRow(t *testing.T, id int64) reportResolutionRow {
	t.Helper()
	var row reportResolutionRow
	require.NoError(t, f.db.QueryRow(`SELECT status, resolved_by, resolved_at FROM admin_reports WHERE id = $1`, id).
		Scan(&row.Status, &row.ResolvedBy, &row.ResolvedAt))
	return row
}

// subjectReports is the reports filed on one subject: two the community sees
// and one it never sees.
type subjectReports struct {
	Spam, Harassment, CSAM int64
}

func (f *communityPostDBFixture) fileSubjectReports(t *testing.T, subjectURI string, targetType adminreports.TargetType) subjectReports {
	t.Helper()
	return subjectReports{
		Spam:       f.fileReport(t, reportResolutionSpamReporter, subjectURI, targetType, adminreports.ReasonSpam),
		Harassment: f.fileReport(t, reportResolutionHarassmentReporter, subjectURI, targetType, adminreports.ReasonHarassment),
		CSAM:       f.fileReport(t, reportResolutionCSAMReporter, subjectURI, targetType, adminreports.ReasonCSAM),
	}
}

func TestCommunityReportResolution_RemovalResolvesCommunityReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		targetType adminreports.TargetType
		// seed indexes the subject and returns its URI and the action that
		// removes it.
		seed           func(t *testing.T, f *communityCommentDBFixture) (subjectURI string, remove func(t *testing.T))
		wantResolvedBy sql.NullString
	}{
		{
			name:       "local post removal by a moderator",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				return post.URI, func(t *testing.T) {
					_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "resolve-remove-post")
					require.NoError(t, err, "the moderator's removal succeeds")
				}
			},
			wantResolvedBy: sql.NullString{String: "did:plc:troublemaker456", Valid: true},
		},
		{
			name:       "local comment removal by a moderator",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				thread := f.seedThread(t)
				return thread.Comment.URI, func(t *testing.T) {
					_, err := f.removeComment(rosterModerator, thread.Comment.URI, "spam", commentRemovalReason, 0, "resolve-remove-comment")
					require.NoError(t, err, "the moderator's removal succeeds")
				}
			},
			wantResolvedBy: sql.NullString{String: "did:plc:troublemaker456", Valid: true},
		},
		{
			name:       "firehose post removal record with no intent",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				return post.URI, func(t *testing.T) {
					require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, post.SubjectRkey,
						rosterHeadOneRev, postFirehoseExternalRemovalCID,
						removalRecordBody(post, "spam", postDBRemovalReason, postFirehoseExternalCreatedAt))))
					require.NoError(t, f.deliverPost(postStandEvent("delete", posts.AcceptanceCollection, post.SubjectRkey,
						rosterHeadOneRev, "", nil)))
					require.Equal(t, []storedPostAction{externalPostAction("remove", post.URI, postDBReasonSpam)}, f.postActions(t),
						"fixture: the external removal is recorded")
				}
			},
		},
		{
			name:       "firehose comment removal record with no intent",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				comment := f.seedThread(t).Comment
				return comment.URI, func(t *testing.T) {
					require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey,
						"3krosterrev21", commentDBExternalSpamCID,
						commentRemovalBody(comment, "spam", commentRemovalReason, commentDBExternalCreatedAt))))
					require.Equal(t, []storedPostAction{
						externalCommentAction(rosterCommunity, "apply-removal", comment.URI, commentDBReasonSpam),
					}, f.commentActions(t, comment.URI), "fixture: the external removal is recorded")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			subjectURI, remove := tt.seed(t, f)
			other := f.seedAcceptedPost(t, postDBSeedFor("3kpostother1"))
			reports := f.fileSubjectReports(t, subjectURI, tt.targetType)
			otherReport := f.fileReport(t, reportResolutionOtherReporter, other.URI, adminreports.TargetTypePost, adminreports.ReasonSpam)
			otherBefore := f.reportRow(t, otherReport)
			require.Equal(t, "open", otherBefore.Status, "fixture: the other subject's report is open")

			remove(t)

			for name, id := range map[string]int64{"spam": reports.Spam, "harassment": reports.Harassment} {
				row := f.reportRow(t, id)
				assert.Equalf(t, "resolved", row.Status, "the %s report on the removed subject is resolved", name)
				assert.Truef(t, row.ResolvedAt.Valid, "the %s report has a resolution time", name)
				assert.Equalf(t, tt.wantResolvedBy, row.ResolvedBy, "the %s report names the removal's actor", name)
			}
			csam := f.reportRow(t, reports.CSAM)
			assert.Equal(t, "open", csam.Status, "the csam report stays open for the instance admins")
			assert.False(t, csam.ResolvedBy.Valid, "the csam report names no resolver")
			assert.False(t, csam.ResolvedAt.Valid, "the csam report has no resolution time")
			assert.Equal(t, otherBefore, f.reportRow(t, otherReport), "the other subject's report is untouched")
		})
	}
}

// Chunk 14 R4: the report resolution is part of the removal's transaction. A
// test trigger refuses every update that resolves a report, so the removal
// cannot be recorded: afterwards no remove action exists for the post and its
// reports are still open.
func TestCommunityReportResolution_RefusedResolutionRollsBackTheRemoval(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	reports := f.fileSubjectReports(t, post.URI, adminreports.TargetTypePost)
	_, err := f.db.Exec(`
		CREATE FUNCTION refuse_report_resolution() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'resolved' THEN
				RAISE EXCEPTION 'test trigger refuses report resolution';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER refuse_report_resolution BEFORE UPDATE ON admin_reports
			FOR EACH ROW EXECUTE FUNCTION refuse_report_resolution();
	`)
	require.NoError(t, err, "fixture: installing the refusing trigger")

	_, removeErr := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "resolve-remove-refused")

	assert.Empty(t, f.postActions(t), "no remove action is recorded when its report resolution fails")
	assert.True(t, removeErr != nil || f.openIntents(t) == 1,
		"the removal does not complete: it returns an error or its intent stays open (error %v)", removeErr)
	for name, id := range map[string]int64{"spam": reports.Spam, "harassment": reports.Harassment, "csam": reports.CSAM} {
		assert.Equalf(t, "open", f.reportRow(t, id).Status, "the %s report is still open", name)
	}
}

// Chunk 14 B5: only a removal resolves reports. A restore of a removed post
// and a retract-removal of a removed comment leave open the reports filed
// after the removal.
func TestCommunityReportResolution_RestoreLeavesLaterReportsOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		targetType adminreports.TargetType
		// seed indexes the subject and returns its URI with the actions that
		// remove and then restore it.
		seed func(t *testing.T, f *communityCommentDBFixture) (subjectURI string, remove, restore func(t *testing.T))
	}{
		{
			name:       "local post restore by a moderator",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T), func(t *testing.T)) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				return post.URI, func(t *testing.T) {
						_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "restore-keeps-remove-post")
						require.NoError(t, err, "the moderator's removal succeeds")
					}, func(t *testing.T) {
						f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
						_, err := f.restorePost(rosterModerator, post.URI, 2, "restore-keeps-restore-post")
						require.NoError(t, err, "the moderator's restore succeeds")
						require.Equal(t, []storedPostAction{
							localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
							localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
						}, f.postActions(t), "fixture: the restore is recorded")
					}
			},
		},
		{
			name:       "local comment retract-removal by a moderator",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T), func(t *testing.T)) {
				thread := f.seedThread(t)
				return thread.Comment.URI, func(t *testing.T) {
						_, err := f.removeComment(rosterModerator, thread.Comment.URI, "spam", commentRemovalReason, 0, "restore-keeps-remove-comment")
						require.NoError(t, err, "the moderator's removal succeeds")
					}, func(t *testing.T) {
						f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
						_, err := f.restoreComment(rosterModerator, thread.Comment.URI, 1, "restore-keeps-restore-comment")
						require.NoError(t, err, "the moderator's restore succeeds")
						require.Equal(t, []storedPostAction{
							localCommentAction("apply-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonSpam),
							localCommentAction("retract-removal", rosterModerator, false, thread.Comment.URI, commentDBReasonSpam),
						}, f.commentActions(t, thread.Comment.URI), "fixture: the retract-removal is recorded")
					}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			subjectURI, remove, restore := tt.seed(t, f)
			earlier := f.fileReport(t, reportResolutionSpamReporter, subjectURI, tt.targetType, adminreports.ReasonSpam)
			remove(t)
			require.Equal(t, "resolved", f.reportRow(t, earlier).Status, "fixture: the removal resolves the earlier report")
			later := f.fileReport(t, reportResolutionOtherReporter, subjectURI, tt.targetType, adminreports.ReasonHarassment)
			require.Equal(t, "open", f.reportRow(t, later).Status, "fixture: the report filed after the removal is open")

			restore(t)

			row := f.reportRow(t, later)
			assert.Equal(t, "open", row.Status, "the report filed after the removal stays open")
			assert.False(t, row.ResolvedBy.Valid, "the later report names no resolver")
			assert.False(t, row.ResolvedAt.Valid, "the later report has no resolution time")
		})
	}
}
