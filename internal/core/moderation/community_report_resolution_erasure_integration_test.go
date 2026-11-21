//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"Coves/internal/core/adminreports"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eraseModerator erases rosterModerator's account through the production
// erasure (users.Delete), after giving the account the users row erasure
// requires.
func (f *communityPostDBFixture) eraseModerator(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, 'rostermoderator.test', 'https://native.pds.test', NOW())`,
		rosterModerator)
	require.NoError(t, err, "fixture: the moderator's users row")
	require.NoError(t, postgres.NewUserRepository(f.db).Delete(context.Background(), rosterModerator), "the moderator's account is erased")
}

// Review finding 3 (DoD 7): a moderator's removal commits, its response is
// lost, and the moderator's account is erased before the removal completes.
// When the firehose copy or the sweep later completes the removal, the
// subject's reports are resolved, but resolved_by names no one: the erased
// account is not written back into the reports.
func TestCommunityReportResolution_CompletionAfterTheActorsErasureNamesNoResolver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		targetType adminreports.TargetType
		// seed indexes the subject, loses the response of the moderator's
		// removal and returns the subject and the completion to run after the
		// erasure.
		seed func(t *testing.T, f *communityCommentDBFixture) (subjectURI string, complete func(t *testing.T))
	}{
		{
			name:       "post removal completed by its firehose copy",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
				f.loseRemovalResponse(t, func() error {
					_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "erased-remove-post")
					return err
				})
				return post.URI, func(t *testing.T) {
					f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID,
						removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt))
				}
			},
		},
		{
			name:       "post removal completed by the sweep",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				post := f.seedAcceptedPost(t, postDBAlpha)
				f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
				f.loseRemovalResponse(t, func() error {
					_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "erased-sweep-post")
					return err
				})
				return post.URI, f.sweepAfterExpiry
			},
		},
		{
			name:       "comment removal completed by its firehose copy",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				comment := f.seedThread(t).Comment
				f.setCommentRemovalWriteCID(comment, commentDBRemovalBodyCID)
				f.loseRemovalResponse(t, func() error {
					_, err := f.removeComment(rosterModerator, comment.URI, "spam", commentRemovalReason, 0, "erased-remove-comment")
					return err
				})
				return comment.URI, func(t *testing.T) {
					require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey, rosterHeadOneRev,
						commentDBRemovalBodyCID, commentRemovalBody(comment, "spam", commentRemovalReason, postDBIntentCreatedAt))))
				}
			},
		},
		{
			name:       "comment removal completed by the sweep",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) (string, func(t *testing.T)) {
				comment := f.seedThread(t).Comment
				f.setCommentRemovalWriteCID(comment, commentDBRemovalBodyCID)
				f.loseRemovalResponse(t, func() error {
					_, err := f.removeComment(rosterModerator, comment.URI, "spam", commentRemovalReason, 0, "erased-sweep-comment")
					return err
				})
				return comment.URI, f.sweepAfterExpiry
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			subjectURI, complete := tt.seed(t, f)
			reports := f.fileSubjectReports(t, subjectURI, tt.targetType)

			f.eraseModerator(t)
			complete(t)

			require.Equal(t, 0, f.openIntents(t), "fixture: the completion settled the moderator's intent")
			for name, id := range map[string]int64{"spam": reports.Spam, "harassment": reports.Harassment} {
				row := f.reportRow(t, id)
				assert.Equalf(t, "resolved", row.Status, "the %s report is resolved by the removal", name)
				assert.Truef(t, row.ResolvedAt.Valid, "the %s report has a resolution time", name)
				assert.Equalf(t, sql.NullString{}, row.ResolvedBy, "the %s report does not name the erased moderator", name)
			}
			assert.Equal(t, "open", f.reportRow(t, reports.CSAM).Status, "the csam report stays open for the instance admins")
		})
	}
}

// loseRemovalResponse runs a removal whose PDS write applies but whose
// response is lost, leaving its intent open.
func (f *communityCommentDBFixture) loseRemovalResponse(t *testing.T, remove func() error) {
	t.Helper()
	f.repo.mu.Lock()
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	f.repo.mu.Unlock()
	require.Error(t, remove(), "fixture: the lost response reads as an error")
	require.Equal(t, 1, f.openIntents(t), "fixture: the intent stays open")
}

// sweepAfterExpiry runs the intent sweep past the open intent's expiry.
func (f *communityCommentDBFixture) sweepAfterExpiry(t *testing.T) {
	t.Helper()
	f.pastIntentExpiry()
	swept, err := f.service.SweepExpiredIntents(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, swept, "fixture: the sweep reconciles the one expired intent")
}

// Review finding 1: a report filed on a subject the community has already
// removed opens no queue entry while the removal stands.
func TestCommunityReportResolution_ReportOnARemovedSubjectIsNotListed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		targetType adminreports.TargetType
		seed       func(t *testing.T, f *communityCommentDBFixture) string
	}{
		{
			name:       "removed post",
			targetType: adminreports.TargetTypePost,
			seed: func(t *testing.T, f *communityCommentDBFixture) string {
				post := f.seedAcceptedPost(t, postDBAlpha)
				_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "listed-remove-post")
				require.NoError(t, err, "the moderator's removal succeeds")
				return post.URI
			},
		},
		{
			name:       "removed comment",
			targetType: adminreports.TargetTypeComment,
			seed: func(t *testing.T, f *communityCommentDBFixture) string {
				comment := f.seedThread(t).Comment
				_, err := f.removeComment(rosterModerator, comment.URI, "spam", commentRemovalReason, 0, "listed-remove-comment")
				require.NoError(t, err, "the moderator's removal succeeds")
				return comment.URI
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityCommentDBFixture(t)
			subjectURI := tt.seed(t, f)
			other := f.seedAcceptedPost(t, postDBSeedFor("3kpostother1"))
			f.fileReport(t, reportResolutionSpamReporter, subjectURI, tt.targetType, adminreports.ReasonSpam)
			f.fileReport(t, reportResolutionOtherReporter, other.URI, adminreports.TargetTypePost, adminreports.ReasonSpam)

			page, err := postgres.NewCommunityReportQueueRepository(f.db).ListCommunityQueue(context.Background(), rosterCommunity, 50, "")
			require.NoError(t, err)
			subjects := make([]string, 0, len(page.Entries))
			for _, entry := range page.Entries {
				subjects = append(subjects, entry.SubjectURI)
			}
			assert.Equal(t, []string{other.URI}, subjects, "only the live post's report is listed")
		})
	}
}
