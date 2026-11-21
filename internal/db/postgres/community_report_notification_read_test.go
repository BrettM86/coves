//go:build integration

package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/notifications"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read path of community report notifications (docs/NOTIFICATIONS.md,
// "Community report groups"): a recipient's group is listed and counted unread
// only while the community's queue holds a community-visible open report the
// recipient did not file, the recipient still holds a role on the community's
// hosted roster and is not suspended, and the community is not removed by the
// instance. Its reportCount is the queue's community-visible open report count.

// withBridgeHosts rebuilds the fixture's report repository with bridgeHosts
// as its trusted-bridge checker, the way cmd/server builds it.
func (f reportNotificationFixture) withBridgeHosts(bridgeHosts notifications.BridgeHostChecker) reportNotificationFixture {
	f.reports = NewAdminReportRepository(f.db, fixtures.InstanceDID(), bridgeHosts)
	return f
}

// dismiss dismisses subjectURI in community's queue as moderatorDID.
func (f reportNotificationFixture) dismiss(t *testing.T, communityDID, subjectURI, moderatorDID, key string) {
	t.Helper()
	_, err := f.queue.DismissCommunitySubject(context.Background(), adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityDID, SubjectURI: subjectURI, ModeratorDID: moderatorDID, IdempotencyKey: key,
	})
	require.NoError(t, err)
}

// resolveByCommunityRemoval resolves subjectURI's community reports in the
// transaction that would record community's removal of it.
func (f reportNotificationFixture) resolveByCommunityRemoval(t *testing.T, communityDID, subjectURI, actorDID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, resolveCommunityReportsTx(ctx, tx, communityDID, subjectURI, actorDID))
	require.NoError(t, tx.Commit())
}

// requireRosterNotified fails unless every recipient lists exactly one unread
// group for community with reportCount reports: the state the read-time
// conditions below are applied to.
func (f reportNotificationFixture) requireRosterNotified(t *testing.T, community reportNotificationCommunity, recipients []string, reportCount int) {
	t.Helper()
	for _, recipient := range recipients {
		require.Equal(t, unreadGroup(community.DID, reportCount), f.listed(t, recipient),
			"%s lists one unread communityReport group with %d reports", recipient, reportCount)
	}
}

// assertNothingListed asserts each recipient lists nothing and has nothing unread.
func (f reportNotificationFixture) assertNothingListed(t *testing.T, recipients []string, why string) {
	t.Helper()
	for _, recipient := range recipients {
		assert.Empty(t, f.listed(t, recipient), "%s lists nothing: %s", recipient, why)
		assert.Equal(t, 0, f.unread(t, recipient), "%s has nothing unread: %s", recipient, why)
	}
}

func TestCommunityReportNotification_EmptyQueueClearsTheGroupAndANewReportRevivesIt(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rnclear", 2)
	firstPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnclearp1", "first", communityQueuePostCreated)
	secondPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnclearp2", "second", communityQueuePostCreated)
	thirdPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnclearp3", "third", communityQueuePostCreated)

	f.report(t, "did:plc:rnclear1", firstPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	f.report(t, "did:plc:rnclear2", firstPost, adminreports.TargetTypePost, adminreports.ReasonHarassment, reportNotificationAt)
	f.report(t, "did:plc:rnclear3", secondPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 3), f.listed(t, recipient),
			"%s lists one group for three reports on two posts", recipient)
	}

	f.dismiss(t, community.DID, firstPost, community.Moderators[0], "rnclear-dismiss")
	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s still lists the group, now counting the second post's one report", recipient)
	}

	f.resolveByCommunityRemoval(t, community.DID, secondPost, community.Moderators[1])
	f.assertNothingListed(t, community.recipients(), "the community's queue is empty")

	f.report(t, "did:plc:rnclear4", thirdPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s lists the group again, unread, with the new report", recipient)
		assert.Equal(t, 1, f.unread(t, recipient), "%s has the revived group unread", recipient)
	}
}

func TestCommunityReportNotification_AuthorDeletedSubjectClearsTheGroup(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rndeleted", 2)
	postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rndeletedpost", "deleted", communityQueuePostCreated)

	f.report(t, "did:plc:rndeletedreporter", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	f.requireRosterNotified(t, community, community.recipients(), 1)

	_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, postURI)
	require.NoError(t, err)

	f.assertNothingListed(t, community.recipients(), "the only reported post was deleted by its author")
}

func TestCommunityReportNotification_OwnReportDoesNotReviveTheReportersGroup(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rnrevive", 2)
	firstPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnrevivep1", "first", communityQueuePostCreated)
	secondPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnrevivep2", "second", communityQueuePostCreated)
	reportingModerator := community.Moderators[0]

	f.report(t, "did:plc:rnreviveoutsider", firstPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	f.requireRosterNotified(t, community, community.recipients(), 1)
	f.dismiss(t, community.DID, firstPost, community.Moderators[1], "rnrevive-dismiss")

	f.report(t, reportingModerator, secondPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)

	f.assertNothingListed(t, []string{reportingModerator}, "the only open report is the moderator's own")
	for _, recipient := range []string{community.Owner, community.Moderators[1]} {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s lists the group for the moderator's report", recipient)
		assert.Equal(t, 1, f.unread(t, recipient), "%s has the group unread", recipient)
	}

	// reportCount is the queue's whole community-visible open report count,
	// the recipient's own reports included.
	f.report(t, "did:plc:rnreviveoutsider2", secondPost, adminreports.TargetTypePost, adminreports.ReasonHarassment, reportNotificationAt)
	for _, recipient := range []string{reportingModerator, community.Owner} {
		assert.Equal(t, unreadGroup(community.DID, 2), f.listed(t, recipient),
			"%s's group counts the moderator's report and the outsider's", recipient)
	}
}

func TestCommunityReportNotification_SensitiveReportsNeitherCountNorKeepTheGroup(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rnmixed", 2)
	postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnmixedpost", "mixed", communityQueuePostCreated)

	f.report(t, "did:plc:rnmixed1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	csam := f.report(t, "did:plc:rnmixed2", postURI, adminreports.TargetTypePost, adminreports.ReasonCSAM, reportNotificationAt)
	illegal := f.report(t, "did:plc:rnmixed3", postURI, adminreports.TargetTypePost, adminreports.ReasonIllegal, reportNotificationAt)
	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s's group counts only the spam report", recipient)
	}

	f.dismiss(t, community.DID, postURI, community.Moderators[0], "rnmixed-dismiss")

	f.assertNothingListed(t, community.recipients(), "only csam and illegal reports, which stay with the admins, remain open")
	assert.Equal(t, "open", readCommunityQueueReportState(t, f.db, csam.ID).Status, "the csam report stays open")
	assert.Equal(t, "open", readCommunityQueueReportState(t, f.db, illegal.ID).Status, "the illegal report stays open")
}

// reportNotificationRecipientCase is one N5 row: condition makes some of C's
// roster ineligible (returned as excluded), either before the report is filed
// (write time) or after the group exists (read time). Every other roster
// member is still notified.
type reportNotificationRecipientCase struct {
	name      string
	condition func(t *testing.T, f reportNotificationFixture, community reportNotificationCommunity) (excluded []string)
}

// setTakendown stores a PDS-confirmed takedown of accountDID.
func setTakendown(t *testing.T, f reportNotificationFixture, accountDID string) {
	accountstatusfixture.Set(t, f.db, accountDID, accountstatus.Takendown)
}

func TestCommunityReportNotification_RecipientEligibilityWhenTheReportIsFiled(t *testing.T) {
	t.Parallel()
	for _, row := range []reportNotificationRecipientCase{
		{"moderator under an in-force instance ban", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			seedNotificationAccountBan(t, f.db, c.Moderators[0])
			return []string{c.Moderators[0]}
		}},
		{"moderator whose instance ban has expired", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			seedNotificationAccountBan(t, f.db, c.Moderators[0])
			_, err := f.db.ExecContext(context.Background(),
				`UPDATE moderation_actions SET expires_at = NOW() - INTERVAL '1 hour' WHERE subject_uri = $1`,
				"at://"+c.Moderators[0])
			require.NoError(t, err)
			return nil
		}},
		{"moderator with a takendown account", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			setTakendown(t, f, c.Moderators[0])
			return []string{c.Moderators[0]}
		}},
		{"moderator who is an aggregator", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			_, err := f.db.ExecContext(context.Background(),
				`INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
				c.Moderators[0], "Report notification aggregator",
				"at://"+c.Moderators[0]+"/social.coves.aggregator.service/self", "bafyreireportnotificationaggregator")
			require.NoError(t, err)
			return []string{c.Moderators[0]}
		}},
		{"moderator on a trusted-bridge PDS", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			_, err := f.db.ExecContext(context.Background(), `UPDATE users SET pds_url = $2 WHERE did = $1`,
				c.Moderators[0], bridgeAPDSURL)
			require.NoError(t, err)
			return []string{c.Moderators[0]}
		}},
		{"community removed by the instance", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			removeCommunityAtInstance(t, f.db, c.DID)
			return c.recipients()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newReportNotificationFixture(t).withBridgeHosts(bridgedNotificationHosts{})
			community := f.rosteredCommunity(t, hostedCommunity, "rnwrite", 2)
			control := f.rosteredCommunity(t, hostedCommunity, "rnwritectl", 1)
			postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnwritepost", "reported", communityQueuePostCreated)
			controlURI := seedVisibilityPost(t, f.db, control.DID, "did:plc:rnauthor", "rnwritectlpost", "control", communityQueuePostCreated)
			excluded := row.condition(t, f, community)

			f.report(t, "did:plc:rnwritereporter", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
			f.report(t, "did:plc:rnwritereporter", controlURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)

			for _, recipient := range community.recipients() {
				if slices.Contains(excluded, recipient) {
					assert.Empty(t, f.listed(t, recipient), "%s is not notified", recipient)
					assert.Equal(t, 0, storedCommunityReportRows(t, f.db, recipient, community.DID),
						"no group is stored for %s, so lifting the condition later shows nothing", recipient)
					continue
				}
				assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
					"%s, still eligible, is notified", recipient)
			}
			if len(excluded) == len(community.recipients()) {
				assert.Equal(t, 2, communityReportRows(t, f.db),
					"no group is stored for the removed community; the control community's owner and moderator have theirs")
			}
			for _, recipient := range control.recipients() {
				assert.Equal(t, unreadGroup(control.DID, 1), f.listed(t, recipient),
					"positive control: %s in an unaffected community is notified", recipient)
			}
		})
	}
}

func TestCommunityReportNotification_RecipientEligibilityAfterTheGroupExists(t *testing.T) {
	t.Parallel()
	for _, row := range []reportNotificationRecipientCase{
		{"moderator banned by the instance", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			seedNotificationAccountBan(t, f.db, c.Moderators[0])
			return []string{c.Moderators[0]}
		}},
		{"moderator's account taken down", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			setTakendown(t, f, c.Moderators[1])
			return []string{c.Moderators[1]}
		}},
		{"community removed by the instance", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			removeCommunityAtInstance(t, f.db, c.DID)
			return c.recipients()
		}},
		{"moderator removed from the roster", func(t *testing.T, f reportNotificationFixture, c reportNotificationCommunity) []string {
			recordKey := "rk" + strings.TrimPrefix(c.Moderators[0], "did:plc:")
			require.NoError(t, NewCommunityRoleRepository(f.db).DeleteCommunityRole(context.Background(), c.DID, recordKey))
			return []string{c.Moderators[0]}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newReportNotificationFixture(t)
			community := f.rosteredCommunity(t, hostedCommunity, "rnread", 2)
			postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnreadpost", "reported", communityQueuePostCreated)
			f.report(t, "did:plc:rnreadreporter", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
			f.requireRosterNotified(t, community, community.recipients(), 1)

			excluded := row.condition(t, f, community)

			for _, recipient := range community.recipients() {
				if slices.Contains(excluded, recipient) {
					assert.Empty(t, f.listed(t, recipient), "%s no longer lists the group", recipient)
					assert.Equal(t, 0, f.unread(t, recipient), "%s no longer counts the group unread", recipient)
					continue
				}
				assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
					"%s, still eligible, still lists the group", recipient)
				assert.Equal(t, 1, f.unread(t, recipient), "%s still has the group unread", recipient)
			}
		})
	}
}
