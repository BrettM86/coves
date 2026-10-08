//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/notifications"

	"github.com/stretchr/testify/require"
)

// insertListedReply inserts an indexed comment and its reply notification at
// the requested sort time. createdAt may be nil to exercise nullable projection.
func (f *unreadVisibilityFixture) insertListedReply(t *testing.T, recipient, reason, subject string, sortAt time.Time, createdAt any) (string, int64) {
	t.Helper()
	record := f.comment(t, f.root)
	var id int64
	err := f.db.QueryRowContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, $2, $3, 'bafyunreadrecord', $4, $5, $6, $7, $8) RETURNING id`,
		recipient, reason, record, f.actor, subject, f.root, createdAt, sortAt.Truncate(time.Microsecond)).Scan(&id)
	require.NoError(t, err)
	return record, id
}

func (f *unreadVisibilityFixture) listPage(t *testing.T, recipient, cursor string, limit int) notifications.ListPage {
	t.Helper()
	page, err := NewNotificationRepository(f.db).(notifications.ReadRepository).List(context.Background(), recipient, cursor, limit)
	require.NoError(t, err)
	return page
}

// recordLabels translates only fixture-generated URIs into the labels assigned
// at insertion; expected label slices in the tests are independent literals.
func (f *unreadVisibilityFixture) recordLabels(t *testing.T, page notifications.ListPage, labels map[string]string) []string {
	t.Helper()
	got := make([]string, 0, len(page.Notifications))
	for _, notification := range page.Notifications {
		label, ok := labels[notification.RecordURI]
		require.True(t, ok, "unexpected record URI %q", notification.RecordURI)
		got = append(got, label)
	}
	return got
}

func TestNotificationList_LimitAndOrder(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Microsecond))
	labels := make(map[string]string)
	add := func(label, reason, subject string, at time.Time, createdAt any) (string, int64) {
		record, id := f.insertListedReply(t, f.recipient, reason, subject, at, createdAt)
		labels[record] = label
		return record, id
	}
	add("A", "postReply", f.root, base, base)
	add("Bb", "postReply", f.root, base.Add(time.Second), base)
	commentSubject := f.comment(t, f.root)
	commentRecord, commentID := add("C", "commentReply", commentSubject, base.Add(time.Second), base.Add(-time.Hour))
	postRecord, postID := add("D", "postReply", f.root, base.Add(2*time.Second), nil)
	add("E", "postReply", f.root, base.Add(3*time.Second), base)

	otherRecipient := retentionUser(t, f.db)
	f.insertListedReply(t, otherRecipient, "postReply", f.root, base.Add(4*time.Second), base)
	f.insertListedReply(t, otherRecipient, "postReply", f.root, base.Add(5*time.Second), base)

	first := f.listPage(t, f.recipient, "", 3)
	require.Equal(t, []string{"E", "D", "C"}, f.recordLabels(t, first, labels))
	require.NotEmpty(t, first.Cursor)
	all := f.listPage(t, f.recipient, "", 10)
	require.Equal(t, []string{"E", "D", "C", "Bb", "A"}, f.recordLabels(t, all, labels))
	require.Empty(t, all.Cursor)

	comment := all.Notifications[2]
	require.Equal(t, commentID, comment.ID)
	require.Equal(t, notifications.ReasonCommentReply, comment.Reason)
	require.Equal(t, commentRecord, comment.RecordURI)
	require.Equal(t, f.actor, comment.ActorDID)
	require.Equal(t, commentSubject, comment.SubjectURI)
	require.NotEqual(t, f.root, comment.SubjectURI)
	require.Equal(t, f.root, comment.RootPostURI)
	require.True(t, comment.RecordCreatedAt.Equal(base.Add(-time.Hour)))
	require.True(t, comment.SortAt.Equal(base.Add(time.Second)))

	post := all.Notifications[1]
	require.Equal(t, postID, post.ID)
	require.Equal(t, postRecord, post.RecordURI)
	require.Equal(t, notifications.ReasonPostReply, post.Reason)
	require.True(t, post.RecordCreatedAt.IsZero())
}

func TestNotificationList_KeysetPagingIsExact(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	retentionSeenAt(t, f.db, f.recipient, base.Add(-time.Microsecond))
	labels := make(map[string]string)
	// Deliberately shuffled insertion times. Both four-row ties cross a page
	// boundary, so a sort_at-only cursor loses rows even if page one is correct.
	for _, row := range []struct {
		label  string
		micros int
	}{
		{"N25", 4}, {"N22", 7}, {"N18", 11}, {"N12", 17}, {"N08", 18},
		{"N21", 7}, {"N17", 12}, {"N11", 17}, {"N07", 19}, {"N20", 7},
		{"N16", 13}, {"N10", 17}, {"N06", 20}, {"N19", 7}, {"N15", 14},
		{"N09", 17}, {"N05", 21}, {"N24", 5}, {"N14", 15}, {"N04", 22},
		{"N23", 6}, {"N13", 16}, {"N03", 23}, {"N02", 24}, {"N01", 25},
	} {
		record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, base.Add(time.Duration(row.micros)*time.Microsecond), base)
		labels[record] = row.label
	}

	t.Run("10 10 5 pages without loss or duplication", func(t *testing.T) {
		cursor := ""
		got := make([]string, 0, 25)
		for _, size := range []int{10, 10, 5} {
			page := f.listPage(t, f.recipient, cursor, 10)
			require.Len(t, page.Notifications, size)
			got = append(got, f.recordLabels(t, page, labels)...)
			if size == 5 {
				require.Empty(t, page.Cursor)
			} else {
				require.NotEmpty(t, page.Cursor)
			}
			cursor = page.Cursor
		}
		require.Equal(t, []string{
			"N01", "N02", "N03", "N04", "N05", "N06", "N07", "N08", "N09", "N10",
			"N11", "N12", "N13", "N14", "N15", "N16", "N17", "N18", "N19", "N20",
			"N21", "N22", "N23", "N24", "N25",
		}, got)
	})
	t.Run("exact multiple ends without cursor", func(t *testing.T) {
		page := f.listPage(t, f.recipient, "", 25)
		require.Equal(t, []string{
			"N01", "N02", "N03", "N04", "N05", "N06", "N07", "N08", "N09", "N10",
			"N11", "N12", "N13", "N14", "N15", "N16", "N17", "N18", "N19", "N20",
			"N21", "N22", "N23", "N24", "N25",
		}, f.recordLabels(t, page, labels))
		require.Empty(t, page.Cursor)
	})
	t.Run("invalid cursor wraps domain error", func(t *testing.T) {
		_, err := NewNotificationRepository(f.db).(notifications.ReadRepository).List(context.Background(), f.recipient, "not-a-cursor", 10)
		require.ErrorIs(t, err, notifications.ErrInvalidCursor)
	})
}

func TestNotificationList_IsReadAgainstSeenAt(t *testing.T) {
	t.Parallel()
	t.Run("before at and after stored seen time", func(t *testing.T) {
		f := newUnreadCountFixture(t)
		base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
		seenAt := base.Add(2 * time.Second)
		retentionSeenAt(t, f.db, f.recipient, seenAt)
		labels := make(map[string]string)
		for _, row := range []struct {
			label string
			at    time.Time
		}{
			{"before", base.Add(time.Second)}, {"equal", seenAt},
			{"after", base.Add(3 * time.Second)}, {"newest", base.Add(4 * time.Second)},
		} {
			record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, row.at, base)
			labels[record] = row.label
		}
		page := f.listPage(t, f.recipient, "", 10)
		require.Equal(t, []string{"newest", "after", "equal", "before"}, f.recordLabels(t, page, labels))
		require.Equal(t, []bool{false, false, true, true}, []bool{
			page.Notifications[0].IsRead, page.Notifications[1].IsRead,
			page.Notifications[2].IsRead, page.Notifications[3].IsRead,
		})
		require.NotNil(t, page.SeenAt)
		require.True(t, page.SeenAt.Equal(seenAt))
		require.Empty(t, page.Cursor)
	})
	t.Run("microsecond equality is read", func(t *testing.T) {
		f := newUnreadCountFixture(t)
		base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
		seenAt := base.Add(2*time.Second + time.Microsecond)
		retentionSeenAt(t, f.db, f.recipient, seenAt)
		record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, seenAt, base)
		page := f.listPage(t, f.recipient, "", 10)
		require.Equal(t, []string{"at microsecond"}, f.recordLabels(t, page, map[string]string{record: "at microsecond"}))
		require.True(t, page.Notifications[0].IsRead)
		require.NotNil(t, page.SeenAt)
		require.True(t, page.SeenAt.Equal(seenAt))
	})
	t.Run("stored seen time on empty page", func(t *testing.T) {
		f := newUnreadCountFixture(t)
		seenAt := time.Date(2026, 9, 30, 12, 0, 2, 0, time.UTC).Truncate(time.Microsecond)
		retentionSeenAt(t, f.db, f.recipient, seenAt)
		page := f.listPage(t, f.recipient, "", 10)
		require.Empty(t, page.Notifications)
		require.Empty(t, page.Cursor)
		require.NotNil(t, page.SeenAt)
		require.True(t, page.SeenAt.Equal(seenAt))
	})
	t.Run("no state row returns reply and nil seen time", func(t *testing.T) {
		f := newUnreadCountFixture(t)
		base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
		record, _ := f.insertListedReply(t, f.recipient, "postReply", f.root, base, base)
		page := f.listPage(t, f.recipient, "", 10)
		require.Equal(t, []string{"only reply"}, f.recordLabels(t, page, map[string]string{record: "only reply"}))
		require.Nil(t, page.SeenAt)
	})
}
