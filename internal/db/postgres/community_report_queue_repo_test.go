//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/adminreports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// community_report_queue_repo.go against real SQL: the community moderators'
// view of admin_reports.
//
// A report carries no community column. Its community is resolved from indexed
// content only (posts.community_did, or the parent chain of a comment up to its
// indexed post), never from the authority of the reported URI: a v1 post URI
// names the community repo it was written to, which need not be the community
// the post is indexed in. csam and illegal reports stay with the instance
// admins and never reach a community queue, and only open reports count.

// communityQueueModerator is the moderator DID the dismissal tests act as.
const communityQueueModerator = "did:plc:rqmoderator"

// communityQueueFixture is the report repository, the queue repository and the
// private database behind both.
type communityQueueFixture struct {
	db      *sql.DB
	reports adminreports.Repository
	queue   adminreports.CommunityQueueRepository
}

func newCommunityQueueFixture(t *testing.T) communityQueueFixture {
	t.Helper()
	reports, db := adminReportRepoAndDB(t)
	return communityQueueFixture{db: db, reports: reports, queue: NewCommunityReportQueueRepository(db)}
}

// report files one open report through the admin report repository and
// backdates it to at.
func (f communityQueueFixture) report(t *testing.T, reporterDID, targetURI string, targetType adminreports.TargetType, reason adminreports.Reason, at time.Time) *adminreports.Report {
	t.Helper()
	report := &adminreports.Report{
		ReporterDID: reporterDID,
		TargetURI:   targetURI,
		TargetType:  targetType,
		Reason:      reason,
		Status:      adminreports.StatusOpen,
	}
	require.NoError(t, f.reports.Create(context.Background(), report))
	adminReportBackdate(t, f.db, report.ID, at)
	return report
}

// list returns the whole queue of communityDID in one page.
func (f communityQueueFixture) list(t *testing.T, communityDID string) []adminreports.CommunityQueueEntry {
	t.Helper()
	page, err := f.queue.ListCommunityQueue(context.Background(), communityDID, 50, "")
	require.NoError(t, err)
	assert.Empty(t, page.Cursor, "a queue shorter than the limit has no next page")
	return communityQueueEntriesUTC(page.Entries)
}

// communityQueueEntriesUTC renders entry timestamps in UTC so they compare
// equal to the literal times the reports were backdated to.
func communityQueueEntriesUTC(entries []adminreports.CommunityQueueEntry) []adminreports.CommunityQueueEntry {
	normalized := make([]adminreports.CommunityQueueEntry, 0, len(entries))
	for _, entry := range entries {
		entry.FirstReportedAt = entry.FirstReportedAt.UTC()
		entry.LastReportedAt = entry.LastReportedAt.UTC()
		normalized = append(normalized, entry)
	}
	return normalized
}

// communityQueueReportState is a report row's resolution columns.
type communityQueueReportState struct {
	Status     string
	ResolvedBy sql.NullString
	ResolvedAt sql.NullTime
}

func readCommunityQueueReportState(t *testing.T, db *sql.DB, id int64) communityQueueReportState {
	t.Helper()
	var state communityQueueReportState
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, resolved_by, resolved_at FROM admin_reports WHERE id = $1`, id,
	).Scan(&state.Status, &state.ResolvedBy, &state.ResolvedAt))
	return state
}

// communityQueueDismissalRows counts the dismissal records for one
// (moderator, community, key) tuple.
func communityQueueDismissalRows(t *testing.T, db *sql.DB, moderatorDID, communityDID, key string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM community_report_dismissals
		WHERE dismissed_by_did = $1 AND community_did = $2 AND idempotency_key = $3
	`, moderatorDID, communityDID, key).Scan(&count))
	return count
}

// seedMisfiledLegacyPost seeds a v1 post whose URI authority is
// authorityCommunityDID but which is indexed in indexedCommunityDID.
func seedMisfiledLegacyPost(t *testing.T, db *sql.DB, authorityCommunityDID, indexedCommunityDID, rkey string, createdAt time.Time) string {
	t.Helper()
	uri := seedLegacyPost(t, db, authorityCommunityDID, "did:plc:rqauthor", rkey, "legacy "+rkey, createdAt)
	_, err := db.ExecContext(context.Background(),
		`UPDATE posts SET community_did = $2 WHERE uri = $1`, uri, indexedCommunityDID)
	require.NoError(t, err)
	return uri
}

var communityQueuePostCreated = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

func TestCommunityReportQueue_GroupsOpenReportsBySubject(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	communityDID := hostedCommunity(t, f.db, "rqgroup")
	postURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqgrouped", "grouped", communityQueuePostCreated)

	f.report(t, "did:plc:rqreporter1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter2", postURI, adminreports.TargetTypePost, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter3", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter4", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC))

	assert.Equal(t, []adminreports.CommunityQueueEntry{{
		SubjectURI:      postURI,
		SubjectType:     adminreports.TargetTypePost,
		Reasons:         []adminreports.Reason{adminreports.ReasonHarassment, adminreports.ReasonSpam},
		ReportCount:     4,
		FirstReportedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
		LastReportedAt:  time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC),
	}}, f.list(t, communityDID),
		"four open reports on one post are one queue entry with distinct ascending reasons")
}

func TestCommunityReportQueue_PagesByLastReportedThenSubject(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := context.Background()
	communityDID := hostedCommunity(t, f.db, "rqpage")

	newest := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqpage1newest", "newest", communityQueuePostCreated)
	tiedSecond := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqpage2tied", "tied second", communityQueuePostCreated)
	tiedThird := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqpage3tied", "tied third", communityQueuePostCreated)
	oldest := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqpage4oldest", "oldest", communityQueuePostCreated)

	// Filed out of display order, so neither insertion order nor id decides.
	f.report(t, "did:plc:rqreporter1", oldest, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter2", tiedThird, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter3", newest, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter4", tiedSecond, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC))

	var traversed []string
	var pageSizes []int
	cursor := ""
	for page := 0; page < 5; page++ {
		result, err := f.queue.ListCommunityQueue(ctx, communityDID, 2, cursor)
		require.NoError(t, err)
		pageSizes = append(pageSizes, len(result.Entries))
		for _, entry := range result.Entries {
			traversed = append(traversed, entry.SubjectURI)
		}
		cursor = result.Cursor
		if cursor == "" {
			break
		}
	}

	assert.Equal(t, []string{newest, tiedSecond, tiedThird, oldest}, traversed,
		"pages run newest last report first, ties broken by subject URI ascending, with no "+
			"subject repeated or skipped across the cursor")
	require.NotEmpty(t, pageSizes)
	assert.Equal(t, 2, pageSizes[0], "the first page holds exactly the limit")
	assert.Empty(t, cursor, "the traversal ends on an empty cursor")
}

func TestCommunityReportQueue_AdminOnlyReasonsAndClosedReportsDoNotCount(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := context.Background()
	communityDID := hostedCommunity(t, f.db, "rqadmin")
	postURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqmixed", "mixed", communityQueuePostCreated)
	csamOnlyURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqcsamonly", "csam only", communityQueuePostCreated)

	f.report(t, "did:plc:rqreporter1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter2", postURI, adminreports.TargetTypePost, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter3", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter4", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC))
	// Later than every counted report, so counting any of them moves LastReportedAt.
	csam := f.report(t, "did:plc:rqreporter5", postURI, adminreports.TargetTypePost, adminreports.ReasonCSAM, time.Date(2026, 11, 3, 9, 40, 0, 0, time.UTC))
	illegal := f.report(t, "did:plc:rqreporter6", postURI, adminreports.TargetTypePost, adminreports.ReasonIllegal, time.Date(2026, 11, 3, 9, 50, 0, 0, time.UTC))
	dismissed := f.report(t, "did:plc:rqreporter7", postURI, adminreports.TargetTypePost, adminreports.ReasonDoxing, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	require.NoError(t, f.reports.UpdateStatus(ctx, dismissed.ID, string(adminreports.StatusDismissed), "did:plc:rqadmin", ""))
	resolved := f.report(t, "did:plc:rqreporter8", postURI, adminreports.TargetTypePost, adminreports.ReasonOther, time.Date(2026, 11, 3, 10, 10, 0, 0, time.UTC))
	require.NoError(t, f.reports.UpdateStatus(ctx, resolved.ID, string(adminreports.StatusResolved), "did:plc:rqadmin", ""))
	csamOnly := f.report(t, "did:plc:rqreporter9", csamOnlyURI, adminreports.TargetTypePost, adminreports.ReasonCSAM, time.Date(2026, 11, 3, 10, 20, 0, 0, time.UTC))

	assert.Equal(t, []adminreports.CommunityQueueEntry{{
		SubjectURI:      postURI,
		SubjectType:     adminreports.TargetTypePost,
		Reasons:         []adminreports.Reason{adminreports.ReasonHarassment, adminreports.ReasonSpam},
		ReportCount:     4,
		FirstReportedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
		LastReportedAt:  time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC),
	}}, f.list(t, communityDID),
		"csam and illegal reports stay with the admins and closed reports never count; a post "+
			"reported only for csam has no community entry")

	open, err := f.reports.ListByStatus(ctx, string(adminreports.StatusOpen), 50, 0)
	require.NoError(t, err)
	assert.Subset(t, adminReportIDs(open), []int64{csam.ID, illegal.ID, csamOnly.ID},
		"the admin listing still carries the csam and illegal reports")
}

func TestCommunityReportQueue_CommunityComesFromIndexedContent(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	communityA := hostedCommunity(t, f.db, "rqindexa")
	communityB := hostedCommunity(t, f.db, "rqindexb")

	misfiledURI := seedMisfiledLegacyPost(t, f.db, communityB, communityA, "rqmisfiled", communityQueuePostCreated)
	rootURI := seedVisibilityPost(t, f.db, communityA, "did:plc:rqauthor", "rqthreadroot", "thread root", communityQueuePostCreated)
	firstLevel := seedActorComment(t, f.db, "did:plc:rqcommenter", rootURI, "rqlevelone", communityQueuePostCreated)
	secondLevel := seedActorComment(t, f.db, "did:plc:rqcommenter", rootURI, "rqleveltwo", communityQueuePostCreated)
	_, err := f.db.ExecContext(context.Background(),
		`UPDATE comments SET parent_uri = $2, parent_cid = 'bafycmtrqlevelone' WHERE uri = $1`, secondLevel, firstLevel)
	require.NoError(t, err)
	unindexedURI := "at://" + communityA + "/social.coves.community.post/rqnotindexed"

	f.report(t, "did:plc:rqreporter1", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter2", secondLevel, adminreports.TargetTypeComment, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter3", unindexedURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))

	assert.Equal(t, []adminreports.CommunityQueueEntry{
		{
			SubjectURI:      secondLevel,
			SubjectType:     adminreports.TargetTypeComment,
			Reasons:         []adminreports.Reason{adminreports.ReasonHarassment},
			ReportCount:     1,
			FirstReportedAt: time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC),
			LastReportedAt:  time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC),
		},
		{
			SubjectURI:      misfiledURI,
			SubjectType:     adminreports.TargetTypePost,
			Reasons:         []adminreports.Reason{adminreports.ReasonSpam},
			ReportCount:     1,
			FirstReportedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
			LastReportedAt:  time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
		},
	}, f.list(t, communityA),
		"A's queue holds the v1 post indexed in A and the nested reply under A's post, and not "+
			"the report on content that was never indexed")
	assert.Empty(t, f.list(t, communityB),
		"the v1 post's URI authority names B, but it is indexed in A, so B never sees it")
}

func TestCommunityReportQueue_DismissIsIdempotentPerKey(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := context.Background()
	communityDID := hostedCommunity(t, f.db, "rqdismiss")
	postURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqdismissed", "dismissed", communityQueuePostCreated)
	otherURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqdismissother", "other", communityQueuePostCreated)

	visible := []*adminreports.Report{
		f.report(t, "did:plc:rqreporter1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)),
		f.report(t, "did:plc:rqreporter2", postURI, adminreports.TargetTypePost, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC)),
		f.report(t, "did:plc:rqreporter3", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC)),
		f.report(t, "did:plc:rqreporter4", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)),
	}
	csam := f.report(t, "did:plc:rqreporter5", postURI, adminreports.TargetTypePost, adminreports.ReasonCSAM, time.Date(2026, 11, 3, 9, 40, 0, 0, time.UTC))
	otherReport := f.report(t, "did:plc:rqreporter6", otherURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC))

	request := adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityDID, SubjectURI: postURI, ModeratorDID: communityQueueModerator, IdempotencyKey: "k1",
	}
	result, err := f.queue.DismissCommunitySubject(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, adminreports.DismissResult{DismissedCount: 4}, result,
		"the dismissal covers the four community-visible open reports")
	for _, report := range visible {
		state := readCommunityQueueReportState(t, f.db, report.ID)
		assert.Equal(t, "dismissed", state.Status, "report %d", report.ID)
		assert.Equal(t, sql.NullString{String: communityQueueModerator, Valid: true}, state.ResolvedBy, "report %d", report.ID)
		assert.True(t, state.ResolvedAt.Valid, "report %d carries the dismissal time", report.ID)
	}
	assert.Equal(t, communityQueueReportState{Status: "open"}, readCommunityQueueReportState(t, f.db, csam.ID),
		"the csam report on the same post stays open for the admins")
	assert.Equal(t, []string{otherURI}, communityQueueSubjects(f.list(t, communityDID)),
		"the dismissed post leaves the queue")

	late := f.report(t, "did:plc:rqreporter7", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC))
	replay, err := f.queue.DismissCommunitySubject(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, adminreports.DismissResult{DismissedCount: 4}, replay,
		"a replay with the same key returns the stored count")
	assert.Equal(t, communityQueueReportState{Status: "open"}, readCommunityQueueReportState(t, f.db, late.ID),
		"a replay does not dismiss a report filed after the original dismissal")
	assert.Equal(t, []adminreports.CommunityQueueEntry{
		{
			SubjectURI:      postURI,
			SubjectType:     adminreports.TargetTypePost,
			Reasons:         []adminreports.Reason{adminreports.ReasonSpam},
			ReportCount:     1,
			FirstReportedAt: time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC),
			LastReportedAt:  time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC),
		},
		{
			SubjectURI:      otherURI,
			SubjectType:     adminreports.TargetTypePost,
			Reasons:         []adminreports.Reason{adminreports.ReasonSpam},
			ReportCount:     1,
			FirstReportedAt: time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC),
			LastReportedAt:  time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC),
		},
	}, f.list(t, communityDID), "the new report puts the post back in the queue with a count of one")

	conflicting := request
	conflicting.SubjectURI = otherURI
	_, err = f.queue.DismissCommunitySubject(ctx, conflicting)
	assert.True(t, errors.Is(err, adminreports.ErrIdempotencyConflict),
		"reusing k1 for a different subject is a conflict, got %v", err)
	assert.Equal(t, communityQueueReportState{Status: "open"}, readCommunityQueueReportState(t, f.db, otherReport.ID),
		"a conflicting request dismisses nothing")
}

func TestCommunityReportQueue_DismissChangesOnlyTheCommunitysVisibleReports(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := context.Background()
	communityA := hostedCommunity(t, f.db, "rqisoa")
	communityB := hostedCommunity(t, f.db, "rqisob")
	misfiledURI := seedMisfiledLegacyPost(t, f.db, communityB, communityA, "rqisolated", communityQueuePostCreated)

	spamOne := f.report(t, "did:plc:rqreporter1", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	spamTwo := f.report(t, "did:plc:rqreporter2", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	csam := f.report(t, "did:plc:rqreporter3", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonCSAM, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))
	illegal := f.report(t, "did:plc:rqreporter4", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonIllegal, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC))
	dismissed := f.report(t, "did:plc:rqreporter5", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 40, 0, 0, time.UTC))
	require.NoError(t, f.reports.UpdateStatus(ctx, dismissed.ID, string(adminreports.StatusDismissed), "did:plc:rqadmin", ""))
	resolved := f.report(t, "did:plc:rqreporter6", misfiledURI, adminreports.TargetTypePost, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 50, 0, 0, time.UTC))
	require.NoError(t, f.reports.UpdateStatus(ctx, resolved.ID, string(adminreports.StatusResolved), "did:plc:rqadmin", ""))

	unchanged := []*adminreports.Report{csam, illegal, dismissed, resolved}
	before := map[int64]communityQueueReportState{}
	for _, report := range append([]*adminreports.Report{spamOne, spamTwo}, unchanged...) {
		before[report.ID] = readCommunityQueueReportState(t, f.db, report.ID)
	}

	underB, err := f.queue.DismissCommunitySubject(ctx, adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityB, SubjectURI: misfiledURI, ModeratorDID: communityQueueModerator, IdempotencyKey: "kb",
	})
	require.NoError(t, err)
	assert.Equal(t, adminreports.DismissResult{DismissedCount: 0}, underB,
		"B's moderator reaches nothing through the URI authority of a post indexed in A")
	for id, state := range before {
		assert.Equal(t, state, readCommunityQueueReportState(t, f.db, id), "report %d after the dismissal under B", id)
	}

	underA, err := f.queue.DismissCommunitySubject(ctx, adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityA, SubjectURI: misfiledURI, ModeratorDID: communityQueueModerator, IdempotencyKey: "ka",
	})
	require.NoError(t, err)
	assert.Equal(t, adminreports.DismissResult{DismissedCount: 2}, underA,
		"under A the two open spam reports are dismissed")
	for _, report := range []*adminreports.Report{spamOne, spamTwo} {
		state := readCommunityQueueReportState(t, f.db, report.ID)
		assert.Equal(t, "dismissed", state.Status, "report %d", report.ID)
		assert.Equal(t, sql.NullString{String: communityQueueModerator, Valid: true}, state.ResolvedBy, "report %d", report.ID)
	}
	for _, report := range unchanged {
		assert.Equal(t, before[report.ID], readCommunityQueueReportState(t, f.db, report.ID),
			"csam, illegal and already closed report %d are untouched", report.ID)
	}
}

func TestCommunityReportQueue_ConcurrentReplaysRecordOneDismissal(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	communityDID := hostedCommunity(t, f.db, "rqreplay")
	postURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqreplayed", "replayed", communityQueuePostCreated)
	f.report(t, "did:plc:rqreporter1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter2", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	f.report(t, "did:plc:rqreporter3", postURI, adminreports.TargetTypePost, adminreports.ReasonHarassment, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))

	request := adminreports.DismissCommunitySubjectRequest{
		CommunityDID: communityDID, SubjectURI: postURI, ModeratorDID: communityQueueModerator, IdempotencyKey: "kreplay",
	}
	requests := make([]adminreports.DismissCommunitySubjectRequest, 8)
	for i := range requests {
		requests[i] = request
	}
	results, errs := concurrentCommunityQueueDismissals(f.queue, requests)

	for i := range requests {
		require.NoError(t, errs[i], "replay %d", i)
		assert.Equal(t, adminreports.DismissResult{DismissedCount: 3}, results[i],
			"replay %d returns the one recorded count", i)
	}
	assert.Equal(t, 1, communityQueueDismissalRows(t, f.db, communityQueueModerator, communityDID, "kreplay"),
		"concurrent replays of one key record exactly one dismissal")
}

func TestCommunityReportQueue_ConcurrentKeyReuseAcrossSubjectsHasOneWinner(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	communityDID := hostedCommunity(t, f.db, "rqrace")

	requests := make([]adminreports.DismissCommunitySubjectRequest, 8)
	reportBySubject := map[string]*adminreports.Report{}
	for i := range requests {
		rkey := "rqrace" + string(rune('a'+i))
		postURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", rkey, rkey, communityQueuePostCreated)
		reportBySubject[postURI] = f.report(t, "did:plc:rqreporter1", postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, i, 0, 0, time.UTC))
		requests[i] = adminreports.DismissCommunitySubjectRequest{
			CommunityDID: communityDID, SubjectURI: postURI, ModeratorDID: communityQueueModerator, IdempotencyKey: "krace",
		}
	}
	results, errs := concurrentCommunityQueueDismissals(f.queue, requests)

	winners := 0
	conflicts := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			assert.Equal(t, adminreports.DismissResult{DismissedCount: 1}, results[i], "the winner dismisses its one report")
			assert.Equal(t, "dismissed", readCommunityQueueReportState(t, f.db, reportBySubject[requests[i].SubjectURI].ID).Status)
		case errors.Is(err, adminreports.ErrIdempotencyConflict):
			conflicts++
			assert.Equal(t, "open", readCommunityQueueReportState(t, f.db, reportBySubject[requests[i].SubjectURI].ID).Status,
				"a conflicting request dismisses nothing")
		default:
			t.Errorf("request %d failed with %v; only a conflict is an acceptable loss", i, err)
		}
	}
	assert.Equal(t, 1, winners, "one key reused across subjects has exactly one winner")
	assert.Equal(t, 7, conflicts, "every other request is an idempotency conflict")
}

// concurrentCommunityQueueDismissals releases every request at once from its
// own goroutine and returns the results by request index.
func concurrentCommunityQueueDismissals(queue adminreports.CommunityQueueRepository, requests []adminreports.DismissCommunitySubjectRequest) ([]adminreports.DismissResult, []error) {
	results := make([]adminreports.DismissResult, len(requests))
	errs := make([]error, len(requests))
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(len(requests))
	done.Add(len(requests))
	for i := range requests {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = queue.DismissCommunitySubject(context.Background(), requests[i])
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

// communityQueueSubjects renders a queue as its subject URIs.
func communityQueueSubjects(entries []adminreports.CommunityQueueEntry) []string {
	subjects := make([]string, 0, len(entries))
	for _, entry := range entries {
		subjects = append(subjects, entry.SubjectURI)
	}
	return subjects
}

// Review finding 1: a subject its author deleted, or that the community has
// removed, leaves the queue, and a dismissal reaches none of its reports. A
// pending post, which no read path shows a moderator, stays listed. The
// reports themselves stay open: only the queue and the dismissal skip them.
func TestCommunityReportQueue_DeletedAndCommunityRemovedSubjectsLeaveTheQueue(t *testing.T) {
	t.Parallel()
	f := newCommunityQueueFixture(t)
	ctx := context.Background()
	communityDID := hostedCommunity(t, f.db, "rqgone")
	pendingURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqgonepending", "pending", communityQueuePostCreated)
	seedAdmission(t, f.db, communityDID, pendingURI, "pending", time.Hour)
	removedURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqgoneremoved", "removed", communityQueuePostCreated)
	seedAdmission(t, f.db, communityDID, removedURI, "removed", time.Hour)
	deletedURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqgonedeleted", "deleted", communityQueuePostCreated)
	rootURI := seedVisibilityPost(t, f.db, communityDID, "did:plc:rqauthor", "rqgoneroot", "root", communityQueuePostCreated)
	deletedCommentURI := seedActorComment(t, f.db, "did:plc:rqcommenter", rootURI, "rqgonecomment", communityQueuePostCreated)

	pending := f.report(t, "did:plc:rqreporter1", pendingURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	removed := f.report(t, "did:plc:rqreporter2", removedURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 10, 0, 0, time.UTC))
	deleted := f.report(t, "did:plc:rqreporter3", deletedURI, adminreports.TargetTypePost, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 20, 0, 0, time.UTC))
	deletedComment := f.report(t, "did:plc:rqreporter4", deletedCommentURI, adminreports.TargetTypeComment, adminreports.ReasonSpam, time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC))
	_, err := f.db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deletedURI)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, `UPDATE comments SET deleted_at = NOW() WHERE uri = $1`, deletedCommentURI)
	require.NoError(t, err)

	assert.Equal(t, []string{pendingURI}, communityQueueSubjects(f.list(t, communityDID)),
		"only the pending post is listed: the deleted post, the deleted comment and the post the community removed are not")

	for i, subject := range []string{removedURI, deletedURI, deletedCommentURI} {
		result, err := f.queue.DismissCommunitySubject(ctx, adminreports.DismissCommunitySubjectRequest{
			CommunityDID: communityDID, SubjectURI: subject, ModeratorDID: communityQueueModerator,
			IdempotencyKey: fmt.Sprintf("gone-%d", i),
		})
		require.NoError(t, err)
		assert.Equal(t, adminreports.DismissResult{DismissedCount: 0}, result, "a dismissal of %s reaches no report", subject)
	}
	for _, report := range []*adminreports.Report{pending, removed, deleted, deletedComment} {
		assert.Equal(t, "open", readCommunityQueueReportState(t, f.db, report.ID).Status, "report %d stays open", report.ID)
	}
}
