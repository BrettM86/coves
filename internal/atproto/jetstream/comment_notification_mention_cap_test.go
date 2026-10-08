//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func mentionCapRecipients(t *testing.T, fixture *mentionEditFixture, count int) []mentionEditRecipient {
	t.Helper()
	recipients := make([]mentionEditRecipient, count)
	for index := range recipients {
		recipients[index] = fixture.recipient(t)
	}
	return recipients
}

func mentionCapRows(t *testing.T, fixture mentionEditFixture) map[string]int64 {
	t.Helper()
	rows, err := fixture.gate.db.QueryContext(context.Background(),
		`SELECT recipient_did, id FROM notifications WHERE record_uri = $1 AND reason = 'mention'`, fixture.uri)
	require.NoError(t, err)
	defer rows.Close()
	got := make(map[string]int64)
	for rows.Next() {
		var recipientDID string
		var notificationID int64
		require.NoError(t, rows.Scan(&recipientDID, &notificationID))
		got[recipientDID] = notificationID
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return got
}

func requireMentionCapRecipients(t *testing.T, rows map[string]int64, expected []mentionEditRecipient) {
	t.Helper()
	require.Equal(t, len(expected), len(rows), "record mention count must respect the total cap")
	for index, recipient := range expected {
		_, exists := rows[recipient.did]
		require.True(t, exists, "eligible facet recipient %d must have a mention row", index)
	}
}

func requireMentionCapUnchanged(t *testing.T, before, after map[string]int64) {
	t.Helper()
	require.Equal(t, len(before), len(after), "historical mention count must remain unchanged")
	for recipientDID, notificationID := range before {
		actualID, exists := after[recipientDID]
		require.True(t, exists, "historical mention recipient must retain a row")
		require.Equal(t, notificationID, actualID, "historical mention row id must remain unchanged")
	}
}

func TestCommentConsumer_MentionCap_CreateKeepsFirstTenFacetRecipients(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	recipients := mentionCapRecipients(t, &fixture, 12)
	fixture.create(t, recipients...)
	requireMentionCapRecipients(t, mentionCapRows(t, fixture), recipients[:10])
}

func TestCommentConsumer_MentionCap_EditCannotExceedFullBudget(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	recipients := mentionCapRecipients(t, &fixture, 12)
	added := mentionCapRecipients(t, &fixture, 3)
	fixture.create(t, recipients...)
	before := mentionCapRows(t, fixture)
	requireMentionCapRecipients(t, before, recipients[:10])
	updated := append(append([]mentionEditRecipient{}, recipients...), added...)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), updated...))
	requireMentionCapUnchanged(t, before, mentionCapRows(t, fixture))
}

func TestCommentConsumer_MentionCap_RemovingFacetsDoesNotFreeHistoricalRows(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	original := mentionCapRecipients(t, &fixture, 10)
	replacement := mentionCapRecipients(t, &fixture, 10)
	fixture.create(t, original...)
	before := mentionCapRows(t, fixture)
	requireMentionCapRecipients(t, before, original)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), replacement...))
	requireMentionCapUnchanged(t, before, mentionCapRows(t, fixture))
}

func TestCommentConsumer_MentionCap_DeletedNotificationFreesOneSlotInFacetOrder(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	original := mentionCapRecipients(t, &fixture, 10)
	added := mentionCapRecipients(t, &fixture, 2)
	fixture.create(t, original...)
	before := mentionCapRows(t, fixture)
	requireMentionCapRecipients(t, before, original)
	result, err := fixture.gate.db.ExecContext(context.Background(),
		`DELETE FROM notifications WHERE record_uri = $1 AND reason = 'mention' AND id = $2`, fixture.uri, before[original[0].did])
	require.NoError(t, err)
	deleted, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "fixture: remove exactly one stored mention")
	withoutDeleted := mentionCapRows(t, fixture)
	requireMentionCapRecipients(t, withoutDeleted, original[1:])
	updated := append(append([]mentionEditRecipient{}, original...), added...)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), updated...))
	after := mentionCapRows(t, fixture)
	expected := append(append([]mentionEditRecipient{}, original[1:]...), added[0])
	requireMentionCapRecipients(t, after, expected)
	for did, id := range withoutDeleted {
		require.Equal(t, id, after[did], "the other historical mention rows must keep their ids")
	}
	require.NotZero(t, after[added[0].did], "the first new eligible facet takes the freed slot")
	_, secondAdded := after[added[1].did]
	require.False(t, secondAdded, "the second new facet cannot take an eleventh slot")
}

func TestCommentConsumer_MentionCap_ActiveRecreateCannotExceedFullBudget(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	original := mentionCapRecipients(t, &fixture, 10)
	added := mentionCapRecipients(t, &fixture, 2)
	fixture.create(t, original...)
	before := mentionCapRows(t, fixture)
	requireMentionCapRecipients(t, before, original)
	updated := append(append([]mentionEditRecipient{}, original...), added...)
	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, updated...)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	requireMentionCapUnchanged(t, before, mentionCapRows(t, fixture))
}

func TestCommentConsumer_MentionCap_ResurrectionBudgetCarriesOverKeptRows(t *testing.T) {
	t.Parallel()
	for _, originalCount := range []int{10, 8} {
		t.Run(fmt.Sprintf("original_%d", originalCount), func(t *testing.T) {
			t.Parallel()
			fixture := newMentionEditFixture(t)
			original := mentionCapRecipients(t, &fixture, originalCount)
			added := mentionCapRecipients(t, &fixture, 12-originalCount)
			fixture.create(t, original...)
			before := mentionCapRows(t, fixture)
			originalRows := notificationRowsForRecordOrSubject(t, fixture.gate.db, fixture.uri)
			requireMentionCapRecipients(t, before, original)
			deleteRevision := testkit.TID()
			require.Less(t, fixture.revision, deleteRevision, "fixture: delete revision must be newer than create")
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
				fixture.gate.commenterDID, CommentCollection, "delete", fixture.key, deleteRevision,
				"", time.Now().Add(time.Second).UnixMicro(), nil,
			)))
			var deletedAt sql.NullTime
			require.NoError(t, fixture.gate.db.QueryRowContext(context.Background(),
				`SELECT deleted_at FROM comments WHERE uri = $1`, fixture.uri).Scan(&deletedAt))
			require.True(t, deletedAt.Valid, "fixture: the comment must be soft-deleted")
			requireMentionCapUnchanged(t, before, mentionCapRows(t, fixture))
			recreateRevision := testkit.TID()
			require.Less(t, deleteRevision, recreateRevision, "fixture: resurrection revision must be newer than delete")
			const recreatedCID = "bafyreimentioncapresurrection"
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
				fixture.gate.commenterDID, CommentCollection, "create", fixture.key, recreateRevision,
				recreatedCID, time.Now().Add(2*time.Second).UnixMicro(), fixture.record(t, fixture.createdAt, append(added, original...)...),
			)))
			var storedCID string
			require.NoError(t, fixture.gate.db.QueryRowContext(context.Background(),
				`SELECT cid FROM comments WHERE uri = $1 AND deleted_at IS NULL`, fixture.uri).Scan(&storedCID))
			require.Equal(t, recreatedCID, storedCID, "fixture: the same-parent resurrection must be indexed")
			after := mentionCapRows(t, fixture)
			expected := append(append([]mentionEditRecipient{}, original...), added[:10-originalCount]...)
			requireMentionCapRecipients(t, after, expected)
			for did, id := range before {
				require.Equal(t, id, after[did], "original mention must retain its id")
			}
			allRows := notificationRowsForRecordOrSubject(t, fixture.gate.db, fixture.uri)
			for _, row := range originalRows {
				require.Contains(t, allRows, row, "kept row must retain its original CID, subject, root and sort time")
			}
		})
	}
}
