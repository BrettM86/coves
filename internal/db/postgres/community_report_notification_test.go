//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/adminreports"
	"Coves/internal/core/communities"
	"Coves/internal/core/notifications"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The write path of community report notifications (docs/NOTIFICATIONS.md,
// "Community report groups"): a report that lands in a hosted community's
// moderator queue bumps one communityReport group per roster member, inside
// the report's own Create transaction. The group's subject is the community,
// and its count is the community's open community-visible report count.

// reportNotificationFixture is the community queue fixture with its report
// repository built the way cmd/server builds it, plus the notification reader
// the recipients list through.
type reportNotificationFixture struct {
	communityQueueFixture
	notifications notifications.ReadRepository
}

func newReportNotificationFixture(t *testing.T) reportNotificationFixture {
	t.Helper()
	f := newCommunityQueueFixture(t)
	f.reports = NewAdminReportRepository(f.db, fixtures.InstanceDID(), nil)
	return reportNotificationFixture{
		communityQueueFixture: f,
		notifications: NewNotificationRepository(f.db,
			WithNotificationAccountRestrictionAuthority(fixtures.InstanceDID())).(notifications.ReadRepository),
	}
}

// reportNotificationCommunity is a community with an indexed owner and
// moderators on its roster.
type reportNotificationCommunity struct {
	DID        string
	Owner      string
	Moderators []string
}

// rosteredCommunity seeds a community through seed (hostedCommunity or
// uncredentialedCommunity) and puts an owner and moderatorCount moderators on
// its roster as role rows, each with a users row so no recipient exclusion
// applies.
func (f reportNotificationFixture) rosteredCommunity(t *testing.T, seed func(*testing.T, *sql.DB, string) string, name string, moderatorCount int) reportNotificationCommunity {
	t.Helper()
	ctx := context.Background()
	community := reportNotificationCommunity{DID: seed(t, f.db, name)}
	roles := NewCommunityRoleRepository(f.db)
	addRole := func(role, suffix string) string {
		id := testkit.UniqueIDWithPrefix(t, name+suffix)
		did := "did:plc:" + id
		createTestUser(t, f.db, id+".test", did)
		require.NoError(t, roles.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community.DID, SubjectDID: did, Role: role,
			RecordKey: "rk" + id, RecordCID: "bafyrole" + id,
			RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		}))
		return did
	}
	community.Owner = addRole(communities.RoleOwner, "o")
	for i := 0; i < moderatorCount; i++ {
		community.Moderators = append(community.Moderators, addRole(communities.RoleModerator, "m"+string(rune('a'+i))))
	}
	return community
}

// recipients is the owner followed by the moderators.
func (c reportNotificationCommunity) recipients() []string {
	return append([]string{c.Owner}, c.Moderators...)
}

// reportNotificationView is the part of a listed notification these tests own.
type reportNotificationView struct {
	Reason       notifications.Reason
	CommunityDID string
	ReportCount  int
	IsRead       bool
}

// listed returns recipientDID's listed notifications, reduced to the fields a
// community report group carries.
func (f reportNotificationFixture) listed(t *testing.T, recipientDID string) []reportNotificationView {
	t.Helper()
	page, err := f.notifications.List(context.Background(), recipientDID, "", 50)
	require.NoError(t, err)
	views := make([]reportNotificationView, 0, len(page.Notifications))
	for _, listed := range page.Notifications {
		view := reportNotificationView{Reason: listed.Reason, ReportCount: listed.ReportCount, IsRead: listed.IsRead}
		if listed.Community != nil {
			view.CommunityDID = listed.Community.DID
		}
		views = append(views, view)
	}
	return views
}

func (f reportNotificationFixture) unread(t *testing.T, recipientDID string) int {
	t.Helper()
	count, err := f.notifications.CountUnread(context.Background(), recipientDID)
	require.NoError(t, err)
	return count
}

// communityReportRows counts every stored communityReport notification.
func communityReportRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE reason = 'communityReport'`).Scan(&count))
	return count
}

// storedCommunityReportRows counts the stored communityReport rows of
// recipientDID for communityDID, whether or not listing would serve them.
func storedCommunityReportRows(t *testing.T, db *sql.DB, recipientDID, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM notifications
		 WHERE reason = 'communityReport' AND recipient_did = $1 AND subject_uri = $2`,
		recipientDID, "at://"+communityDID).Scan(&count))
	return count
}

// unreadGroup is one unread communityReport group for communityDID.
func unreadGroup(communityDID string, reportCount int) []reportNotificationView {
	return []reportNotificationView{{
		Reason: notifications.ReasonCommunityReport, CommunityDID: communityDID, ReportCount: reportCount,
	}}
}

var reportNotificationAt = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func TestCommunityReportNotification_OutsiderReportNotifiesTheWholeRoster(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rnout", 2)
	postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnoutpost", "reported", communityQueuePostCreated)
	const reporter = "did:plc:rnoutsider"
	createTestUser(t, f.db, "rnoutsider.test", reporter)

	f.report(t, reporter, postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)

	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s lists one unread communityReport group naming the community with one report", recipient)
		assert.Equal(t, 1, f.unread(t, recipient), "%s has one unread notification", recipient)
	}
	assert.Empty(t, f.listed(t, reporter), "the reporter is not on the roster and is notified of nothing")
}

func TestCommunityReportNotification_RosterReporterIsNotNotifiedOfTheirOwnReport(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	community := f.rosteredCommunity(t, hostedCommunity, "rnself", 2)
	postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnselfpost", "reported", communityQueuePostCreated)
	reporter := community.Moderators[1]

	f.report(t, reporter, postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)

	for _, recipient := range []string{community.Owner, community.Moderators[0]} {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s is notified of the second moderator's report", recipient)
		assert.Equal(t, 1, f.unread(t, recipient), "%s has one unread notification", recipient)
	}
	assert.Empty(t, f.listed(t, reporter), "the moderator who filed the report is not notified of it")
	assert.Equal(t, 0, f.unread(t, reporter), "the reporting moderator has nothing unread")
}

func TestCommunityReportNotification_CommentReportRoutesByParentChain(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	ctx := context.Background()
	community := f.rosteredCommunity(t, hostedCommunity, "rncmtc", 2)
	declaredRoot := f.rosteredCommunity(t, hostedCommunity, "rncmtd", 1)
	postInC := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rncmtpostc", "thread in C", communityQueuePostCreated)
	postInD := seedVisibilityPost(t, f.db, declaredRoot.DID, "did:plc:rnauthor", "rncmtpostd", "post in D", communityQueuePostCreated)
	firstLevel := seedActorComment(t, f.db, "did:plc:rncommenter", postInC, "rncmtlevelone", communityQueuePostCreated)
	secondLevel := seedActorComment(t, f.db, "did:plc:rncommenter", postInC, "rncmtleveltwo", communityQueuePostCreated)
	// The reply's parent chain reaches C's post; its declared root names D's.
	_, err := f.db.ExecContext(ctx, `
		UPDATE comments SET parent_uri = $2, parent_cid = 'bafycmtrncmtlevelone', root_uri = $3, root_cid = 'bafypostv2rncmtpostd'
		WHERE uri = $1`, secondLevel, firstLevel, postInD)
	require.NoError(t, err)

	f.report(t, "did:plc:rncmtreporter", secondLevel, adminreports.TargetTypeComment, adminreports.ReasonHarassment, reportNotificationAt)
	require.Equal(t, []string{secondLevel}, communityQueueSubjects(f.list(t, community.DID)),
		"fixture: the reply is in C's queue")
	require.Empty(t, f.list(t, declaredRoot.DID), "fixture: the reply is not in D's queue")

	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 1), f.listed(t, recipient),
			"%s in C, where the reply's parent chain leads, is notified", recipient)
	}
	for _, recipient := range declaredRoot.recipients() {
		assert.Empty(t, f.listed(t, recipient),
			"%s in D, named only by the reply's declared root, is not notified", recipient)
	}
}

func TestCommunityReportNotification_BurstBumpsOneGroupPerRecipient(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	ctx := context.Background()
	community := f.rosteredCommunity(t, hostedCommunity, "rnburstc", 2)
	other := f.rosteredCommunity(t, hostedCommunity, "rnburstd", 1)
	firstPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnburstp1", "first", communityQueuePostCreated)
	secondPost := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnburstp2", "second", communityQueuePostCreated)
	otherPost := seedVisibilityPost(t, f.db, other.DID, "did:plc:rnauthor", "rnburstpd", "in D", communityQueuePostCreated)

	f.report(t, "did:plc:rnburst1", firstPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	f.report(t, "did:plc:rnburst2", firstPost, adminreports.TargetTypePost, adminreports.ReasonHarassment, reportNotificationAt.Add(time.Minute))
	f.report(t, "did:plc:rnburst3", firstPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt.Add(2*time.Minute))
	f.report(t, "did:plc:rnburst4", secondPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt.Add(3*time.Minute))
	f.report(t, "did:plc:rnburst5", secondPost, adminreports.TargetTypePost, adminreports.ReasonHarassment, reportNotificationAt.Add(4*time.Minute))

	for _, recipient := range community.recipients() {
		assert.Equal(t, unreadGroup(community.DID, 5), f.listed(t, recipient),
			"%s lists one group for five reports on two posts", recipient)
	}

	seenModerator := community.Moderators[0]
	page, err := f.notifications.List(ctx, seenModerator, "", 50)
	require.NoError(t, err)
	require.Len(t, page.Notifications, 1, "the moderator has the group to mark seen")
	require.NoError(t, f.notifications.UpdateSeen(ctx, seenModerator, page.Notifications[0].SortAt.Add(time.Microsecond)))
	require.Equal(t, 0, f.unread(t, seenModerator), "marking seen past the group reads it")

	f.report(t, "did:plc:rnburst6", secondPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt.Add(5*time.Minute))

	assert.Equal(t, unreadGroup(community.DID, 6), f.listed(t, seenModerator),
		"a sixth report bumps the same group, unread again, with six reports")
	assert.Equal(t, 1, f.unread(t, seenModerator), "the bumped group is the one unread notification")

	f.report(t, "did:plc:rnburst7", otherPost, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt.Add(6*time.Minute))

	assert.Equal(t, unreadGroup(other.DID, 1), f.listed(t, other.Moderators[0]),
		"D's moderator gets D's own group")
	for _, recipient := range community.recipients() {
		assert.Len(t, f.listed(t, recipient), 1, "%s still has only C's group after a report in D", recipient)
	}
	assert.Equal(t, unreadGroup(community.DID, 6), f.listed(t, community.Owner),
		"C's group still counts C's six reports, not D's")
}

func TestCommunityReportNotification_ReportsOutsideTheCommunityQueueWriteNoNotification(t *testing.T) {
	t.Parallel()
	type reportedSubject struct {
		uri        string
		targetType adminreports.TargetType
		reason     adminreports.Reason
	}
	for _, example := range []struct {
		name    string
		subject func(t *testing.T, f reportNotificationFixture, community reportNotificationCommunity) reportedSubject
	}{
		{"csam report", func(t *testing.T, f reportNotificationFixture, community reportNotificationCommunity) reportedSubject {
			uri := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnnocsam", "csam", communityQueuePostCreated)
			return reportedSubject{uri, adminreports.TargetTypePost, adminreports.ReasonCSAM}
		}},
		{"illegal report", func(t *testing.T, f reportNotificationFixture, community reportNotificationCommunity) reportedSubject {
			uri := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnnoillegal", "illegal", communityQueuePostCreated)
			return reportedSubject{uri, adminreports.TargetTypePost, adminreports.ReasonIllegal}
		}},
		{"post in a non-hosted community", func(t *testing.T, f reportNotificationFixture, _ reportNotificationCommunity) reportedSubject {
			nonHosted := f.rosteredCommunity(t, uncredentialedCommunity, "rnnohost", 1)
			uri := seedVisibilityPost(t, f.db, nonHosted.DID, "did:plc:rnauthor", "rnnohostpost", "not hosted", communityQueuePostCreated)
			return reportedSubject{uri, adminreports.TargetTypePost, adminreports.ReasonSpam}
		}},
		{"post the community removed", func(t *testing.T, f reportNotificationFixture, community reportNotificationCommunity) reportedSubject {
			uri := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnnoremoved", "removed", communityQueuePostCreated)
			seedAdmission(t, f.db, community.DID, uri, "removed", time.Hour)
			return reportedSubject{uri, adminreports.TargetTypePost, adminreports.ReasonSpam}
		}},
		{"unindexed content", func(_ *testing.T, _ reportNotificationFixture, community reportNotificationCommunity) reportedSubject {
			return reportedSubject{"at://" + community.DID + "/social.coves.community.post/rnnotindexed", adminreports.TargetTypePost, adminreports.ReasonSpam}
		}},
	} {
		t.Run(example.name, func(t *testing.T) {
			t.Parallel()
			f := newReportNotificationFixture(t)
			community := f.rosteredCommunity(t, hostedCommunity, "rnnone", 2)
			subject := example.subject(t, f, community)

			report := f.report(t, "did:plc:rnnonereporter", subject.uri, subject.targetType, subject.reason, reportNotificationAt)

			assert.Equal(t, communityQueueReportState{Status: "open"}, readCommunityQueueReportState(t, f.db, report.ID),
				"the report itself is stored open")
			assert.Equal(t, 0, communityReportRows(t, f.db), "a report outside the community queue writes no notification")

			// Positive control: the same harness does notify for a report that
			// reaches C's queue, so the zero above is not a harness that never writes.
			controlURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnnonecontrol", "control", communityQueuePostCreated)
			f.report(t, "did:plc:rnnonecontrolreporter", controlURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt.Add(time.Minute))
			assert.Equal(t, 3, communityReportRows(t, f.db),
				"a spam report on a live post in C writes one group for each of C's owner and two moderators")
		})
	}
}
