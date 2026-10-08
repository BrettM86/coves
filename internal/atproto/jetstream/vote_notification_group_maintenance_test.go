//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func maintenanceGroup(t *testing.T, fixture upvoteGroupFixture) (int64, time.Time) {
	t.Helper()
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
		"the group must exist before the vote set changes")
	var groupID int64
	var sortAt time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		fixture.author, fixture.post).Scan(&groupID, &sortAt))
	return groupID, sortAt
}

func ageMaintenanceGroup(t *testing.T, db *sql.DB, groupID int64) time.Time {
	t.Helper()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	result, err := db.Exec(`UPDATE notifications SET sort_at = $1 WHERE id = $2`, past, groupID)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated)
	return past
}

func replaceMaintenanceVote(t *testing.T, fixture upvoteGroupFixture, consumer *VoteEventConsumer, voter, direction, oldURI string) {
	t.Helper()
	event := upvoteGroupEvent(voter, fixture.post, direction, fixture.createdAt, testkit.TID())
	newURI := "at://" + voter + "/social.coves.feed.vote/" + event.Commit.RKey
	require.NotEqual(t, oldURI, newURI, "replacement must use a new record key")
	require.NoError(t, consumer.HandleEvent(context.Background(), event))
	exists, active := voteRowState(t, fixture.db, oldURI)
	require.True(t, exists, "the old vote must still exist as a soft-deleted row")
	require.False(t, active, "the old vote must be soft-deleted by the replacement")
	exists, active = voteRowState(t, fixture.db, newURI)
	require.True(t, exists && active, "the replacement vote must be live")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes
		WHERE uri = $1 AND direction = $2 AND deleted_at IS NULL`, newURI, direction),
		"the replacement must have the requested direction")
}

func TestVoteConsumer_UpvoteGroupMaintenanceBlockedLastVoterDownvote(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	oldURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
	maintenanceGroup(t, fixture)
	ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes
		WHERE uri = $1 AND direction = 'up' AND deleted_at IS NULL`, oldURI),
		"the blocked voter still has a live upvote; the block makes it ineligible")
	otherVoter := fixture.voter(t)
	deliverGroupVote(t, consumer, otherVoter, fixture.post, "down", fixture.createdAt)
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"group deleted: a downvote cannot keep a group whose only upvoter is blocked")
}

func TestVoteConsumer_UpvoteGroupMaintenanceDownvoteReplacement(t *testing.T) {
	t.Parallel()
	for _, companion := range []bool{false, true} {
		name := "last_qualifying_upvote_deleted"
		if companion {
			name = "other_qualifying_upvote_preserves_group_and_sort"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			oldURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
			var otherURI string
			if companion {
				otherURI = deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
			}
			groupID, _ := maintenanceGroup(t, fixture)
			var past time.Time
			if companion {
				past = ageMaintenanceGroup(t, fixture.db, groupID)
			}
			replaceMaintenanceVote(t, fixture, consumer, voter, "down", oldURI)
			if !companion {
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
					"group deleted: replacing the last qualifying upvote with a downvote leaves no voters")
				return
			}
			exists, active := voteRowState(t, fixture.db, otherURI)
			require.True(t, exists && active, "the other voter's qualifying upvote must still be live")
			var storedID int64
			var storedSort time.Time
			require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
				WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
				fixture.author, fixture.post).Scan(&storedID, &storedSort),
				"the other voter's live upvote must keep the group")
			assert.Equal(t, groupID, storedID, "the surviving group must be the original row")
			assert.True(t, storedSort.Equal(past), "a downvote replacement must not bump the surviving group")
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
		})
	}
}

func TestVoteConsumer_UpvoteGroupMaintenanceUpvoteReplacement(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name      string
		blocked   bool
		companion bool
	}{
		{"blocked_last_qualifying_upvote_deleted", true, false},
		{"blocked_with_other_upvote_preserves_group_and_sort", true, true},
		{"eligible_replacement_keeps_group_without_bump", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			oldURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
			var otherURI string
			if scenario.companion {
				otherURI = deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
			}
			groupID, _ := maintenanceGroup(t, fixture)
			past := ageMaintenanceGroup(t, fixture.db, groupID)
			if scenario.blocked {
				ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
			}
			replaceMaintenanceVote(t, fixture, consumer, voter, "up", oldURI)
			if scenario.blocked && !scenario.companion {
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
					"group deleted: blocked up-to-up replacement leaves no qualifying upvotes")
				return
			}
			if scenario.companion {
				exists, active := voteRowState(t, fixture.db, otherURI)
				require.True(t, exists && active, "the other voter's qualifying upvote must still be live")
			}
			var storedID int64
			var storedSort time.Time
			require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
				WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
				fixture.author, fixture.post).Scan(&storedID, &storedSort),
				"the group must survive while a qualifying upvote exists")
			assert.Equal(t, groupID, storedID, "replacement must retain the same group id")
			if scenario.blocked {
				assert.True(t, storedSort.Equal(past), "blocked replacement must not bump a surviving group")
			} else {
				assert.True(t, storedSort.Equal(past), "eligible up-to-up replacement must keep the group's sort_at unchanged")
			}
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
		})
	}
}

func TestVoteConsumer_UpvoteGroupMaintenanceErasedVoter(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"up", "down"} {
		t.Run("later_"+direction+"vote", func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			original := upvoteGroupEvent(voter, fixture.post, "up", fixture.createdAt, revA)
			originalURI := "at://" + voter + "/social.coves.feed.vote/" + original.Commit.RKey
			require.NoError(t, consumer.HandleEvent(context.Background(), original))
			groupID, _ := maintenanceGroup(t, fixture)
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes
				WHERE uri = $1 AND deleted_at IS NULL`, originalURI), "A must be the group's only voter")

			require.NoError(t, postgres.NewUserRepository(fixture.db).Delete(context.Background(), voter),
				"erase A through the user repository's real deletion path")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, voter),
				"the erasure gate must see A's marker")
			require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE voter_did = $1`, voter),
				"erasure must hard-delete A's original vote")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE id = $1`, groupID),
				"the group must survive erasure itself for the later vote to maintain it")
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))

			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
				WHERE record_uri = $1 AND rev = $2`, originalURI, revA),
				"erasure must leave the original rev claim to reject an equal-rev replay")
			require.NoError(t, consumer.HandleEvent(context.Background(), original), "replay A's original event at the same rev")
			require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, originalURI),
				"the rev-gated replay must not restore A's hard-deleted vote")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE id = $1`, groupID),
				"the rev-gated replay must not delete the group")
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))

			later := upvoteGroupEvent(voter, fixture.post, direction, fixture.createdAt, revB)
			laterURI := "at://" + voter + "/social.coves.feed.vote/" + later.Commit.RKey
			require.NotEqual(t, originalURI, laterURI, "the later vote must use a new record key")
			require.NoError(t, consumer.HandleEvent(context.Background(), later))
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes
				WHERE uri = $1 AND voter_did = $2 AND subject_uri = $3 AND direction = $4 AND deleted_at IS NULL`,
				laterURI, voter, fixture.post, direction), "the erased voter's later vote must be accepted")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
				WHERE record_uri = $1 AND rev = $2`, laterURI, revB), "the new record's newer rev must be claimed")
			require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
				"group deleted: A's later vote is not qualifying after erasure, regardless of direction")
		})
	}
}

func maintenanceVoteAtKey(voter, subject, createdAt, rev, recordKey string) *JetstreamEvent {
	event := upvoteGroupEvent(voter, subject, "up", createdAt, rev)
	event.Commit.RKey = recordKey
	return event
}

func seedMaintenanceReplacementConflict(t *testing.T, fixture upvoteGroupFixture, consumer *VoteEventConsumer, voter, recipient, subject string) (*JetstreamEvent, string, string) {
	t.Helper()
	secondKey := testkit.TID()
	firstKey := testkit.TID()
	require.NotEqual(t, secondKey, firstKey)
	secondURI := "at://" + voter + "/social.coves.feed.vote/" + secondKey
	firstURI := "at://" + voter + "/social.coves.feed.vote/" + firstKey

	require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(voter, subject, fixture.createdAt, revA, secondKey)))
	require.Equal(t, 1, groupCount(t, fixture.db, recipient, subject), "the first upvote must create the group")
	require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(voter, subject, fixture.createdAt, revB, firstKey)))
	exists, active := voteRowState(t, fixture.db, secondURI)
	require.True(t, exists && !active, "rkey2 must already be soft-deleted before the conflicting create")
	exists, active = voteRowState(t, fixture.db, firstURI)
	require.True(t, exists && active, "rkey1 must be live before the conflicting create")
	require.Equal(t, 1, groupCount(t, fixture.db, recipient, subject), "the group must exist before the conflicting create")
	return maintenanceVoteAtKey(voter, subject, fixture.createdAt, revC, secondKey), secondURI, firstURI
}

func TestVoteConsumer_UpvoteGroupMaintenanceReplacementConflictDeletesLastUpvote(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"postv2", "comment", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			recipient, subject, countQuery := fixture.author, fixture.post,
				`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`
			switch kind {
			case "comment":
				id := testkit.UniqueID(t)
				recipient = "did:plc:" + id + "commenter"
				insertBridgedUserOnPDS(t, fixture.db, recipient, id+"commenter.test", bridgedTestNativePDS)
				key := testkit.TID()
				subject = "at://" + recipient + "/" + CommentCollection + "/" + key
				_, err := fixture.db.Exec(`INSERT INTO comments
					(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
					VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'bafupvoteroot', $4, 'bafupvoteroot', 'comment', NOW())`,
					subject, key, recipient, fixture.post)
				require.NoError(t, err)
				countQuery = `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`
			case "legacy":
				key := testkit.TID()
				subject = "at://" + fixture.community + "/social.coves.community.post/" + key
				_, err := fixture.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
					VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'legacy post', NOW())`,
					subject, key, recipient, fixture.community)
				require.NoError(t, err)
			}
			consumer := fixture.consumer()
			conflict, secondURI, firstURI := seedMaintenanceReplacementConflict(t, fixture, consumer,
				fixture.voter(t), recipient, subject)
			before := readSubjectCounts(t, fixture.db, countQuery, subject)
			require.Equal(t, 1, before.Upvotes, "rkey1 must be the last live upvote before the conflict")
			require.NoError(t, consumer.HandleEvent(context.Background(), conflict))
			for _, uri := range []string{secondURI, firstURI} {
				exists, active := voteRowState(t, fixture.db, uri)
				require.True(t, exists && !active, "both vote rows must exist and be soft-deleted after the conflict: %s", uri)
			}
			after := readSubjectCounts(t, fixture.db, countQuery, subject)
			require.Equal(t, before.Upvotes-1, after.Upvotes, "the conflict must subtract rkey1's upvote exactly once")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
				WHERE record_uri = $1 AND rev = $2`, secondURI, revC), "rkey2's rev must advance to r3")
			require.Zero(t, groupCount(t, fixture.db, recipient, subject),
				"group deleted: the conflicting create removed the last qualifying upvote")
		})
	}
}

func TestVoteConsumer_UpvoteGroupMaintenanceReplacementConflictPreservesCompanion(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	conflict, secondURI, firstURI := seedMaintenanceReplacementConflict(t, fixture, consumer,
		voter, fixture.author, fixture.post)
	companion := fixture.voter(t)
	companionURI := deliverGroupVote(t, consumer, companion, fixture.post, "up", fixture.createdAt)
	groupID, _ := maintenanceGroup(t, fixture)
	past := ageMaintenanceGroup(t, fixture.db, groupID)
	before := readSubjectCounts(t, fixture.db, `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post)
	require.Equal(t, 2, before.Upvotes)
	require.NoError(t, consumer.HandleEvent(context.Background(), conflict))
	for _, uri := range []string{secondURI, firstURI} {
		exists, active := voteRowState(t, fixture.db, uri)
		require.True(t, exists && !active, "the conflict must leave both of A's vote rows soft-deleted")
	}
	exists, active := voteRowState(t, fixture.db, companionURI)
	require.True(t, exists && active, "D's independent qualifying upvote must remain live")
	after := readSubjectCounts(t, fixture.db, `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post)
	require.Equal(t, before.Upvotes-1, after.Upvotes)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, secondURI, revC))
	var storedID int64
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&storedID, &storedSort),
		"D's qualifying upvote must keep the original group")
	require.Equal(t, groupID, storedID)
	require.Truef(t, storedSort.Equal(past), "the conflicting create changed sort_at from %s to %s", past, storedSort)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	// A later real withdrawal is the positive control for the group's survival while D remains.
	replaceMaintenanceVote(t, fixture, consumer, companion, "down", companionURI)
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"positive control: removing D's last qualifying upvote deletes the group")
}

func TestVoteConsumer_UpvoteGroupMaintenanceReplacementConflictWithoutStaleVoteLeavesGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)))
	groupID, _ := maintenanceGroup(t, fixture)
	ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
	past := ageMaintenanceGroup(t, fixture.db, groupID)
	require.NoError(t, consumer.HandleEvent(context.Background(), maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revB, key)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, uri, revB), "the same-rkey create must win the rev gate")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes
		WHERE uri = $1 AND deleted_at IS NULL`, uri), "no stale vote was soft-deleted")
	var storedID int64
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&storedID, &storedSort))
	require.Equal(t, groupID, storedID, "a conflict without a vote-set change must keep the group")
	require.Truef(t, storedSort.Equal(past), "a conflict without a vote-set change changed sort_at from %s to %s", past, storedSort)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	// A new-rkey vote changes the set and proves the blocked-voter group can be deleted in this fixture.
	fresh := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revC, testkit.TID())
	require.NotEqual(t, key, fresh.Commit.RKey)
	require.NoError(t, consumer.HandleEvent(context.Background(), fresh))
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"positive control: a real replacement of the blocked voter's upvote deletes the group")
}

func TestVoteConsumer_UpvoteGroupMaintenanceRollbackReplacementConflictEarlyCommit(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	workingConsumer := fixture.consumer()
	conflict, secondURI, firstURI := seedMaintenanceReplacementConflict(t, fixture, workingConsumer,
		fixture.voter(t), fixture.author, fixture.post)
	groupID, sortAt := maintenanceGroup(t, fixture)
	countQuery := `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`
	before := readSubjectCounts(t, fixture.db, countQuery, fixture.post)
	require.Equal(t, 1, before.Upvotes, "rkey1 must be the last live upvote")

	injected := errors.New("injected after real replacement-conflict group deletion")
	repository := &deleteThenFailUpvoteGroupRepository{
		Repository: postgres.NewNotificationRepository(fixture.db), failure: injected,
	}
	err := fixture.consumer(WithVoteNotifications(repository)).HandleEvent(context.Background(), conflict)
	require.ErrorIs(t, err, injected)
	require.ErrorContains(t, err, "write stale vote notifications",
		"the early commit must name its own group write, distinct from the final commit's")
	require.Equal(t, []notifications.UpvoteGroupIntent{{
		Action: notifications.UpvoteGroupDeleteIfEmpty, RecipientDID: fixture.author, SubjectURI: fixture.post,
	}}, repository.intents, "the early-commit path must request deletion for the post author")
	require.True(t, repository.deletedInsideTransaction, "the real repository must delete the group inside the aborted transaction")
	exists, active := voteRowState(t, fixture.db, firstURI)
	assert.True(t, exists && active, "rkey1's stale-vote soft delete must roll back")
	exists, active = voteRowState(t, fixture.db, secondURI)
	assert.True(t, exists && !active, "rkey2 must remain soft-deleted")
	assert.Equal(t, before, readSubjectCounts(t, fixture.db, countQuery, fixture.post),
		"the stale-vote count decrement must roll back")
	var storedRev string
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, secondURI).Scan(&storedRev))
	assert.Equal(t, revA, storedRev, "the r3 rev claim must roll back")
	var storedID int64
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&storedID, &storedSort))
	assert.Equal(t, groupID, storedID)
	assert.True(t, storedSort.Equal(sortAt), "the original group must survive the failed attempt")

	require.NoError(t, workingConsumer.HandleEvent(context.Background(), conflict), "identical r3 redrive must succeed")
	for _, uri := range []string{firstURI, secondURI} {
		exists, active := voteRowState(t, fixture.db, uri)
		require.True(t, exists && !active, "redrive must soft-delete both vote rows: %s", uri)
	}
	assert.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"positive control: the same conflict deletes the last-voter group")
	assert.Equal(t, before.Upvotes-1, readSubjectCounts(t, fixture.db, countQuery, fixture.post).Upvotes)
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, secondURI).Scan(&storedRev))
	assert.Equal(t, revC, storedRev)
}

type deleteThenFailUpvoteGroupRepository struct {
	notifications.Repository
	failure                  error
	intents                  []notifications.UpvoteGroupIntent
	deletedInsideTransaction bool
}

func (repository *deleteThenFailUpvoteGroupRepository) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	repository.intents = append(repository.intents, intent)
	if err := repository.Repository.ApplyUpvoteGroupTx(ctx, tx, intent); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		intent.RecipientDID, intent.SubjectURI).Scan(&remaining); err != nil {
		return err
	}
	repository.deletedInsideTransaction = remaining == 0
	return repository.failure
}

func TestVoteConsumer_UpvoteGroupMaintenanceRollbackFinalCommitRealDeletion(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	workingConsumer := fixture.consumer()
	voter := fixture.voter(t)
	oldURI := deliverGroupVote(t, workingConsumer, voter, fixture.post, "up", fixture.createdAt)
	groupID, sortAt := maintenanceGroup(t, fixture)
	countQuery := `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`
	before := readSubjectCounts(t, fixture.db, countQuery, fixture.post)
	require.Equal(t, 1, before.Upvotes, "A must be the group's only live upvoter")
	downvote := upvoteGroupEvent(voter, fixture.post, "down", fixture.createdAt, testkit.TID())
	downvoteURI := "at://" + voter + "/social.coves.feed.vote/" + downvote.Commit.RKey
	require.NotEqual(t, oldURI, downvoteURI, "replacement must use a new record key")

	injected := errors.New("injected after real group deletion")
	repository := &deleteThenFailUpvoteGroupRepository{
		Repository: postgres.NewNotificationRepository(fixture.db), failure: injected,
	}
	err := fixture.consumer(WithVoteNotifications(repository)).HandleEvent(context.Background(), downvote)
	require.ErrorIs(t, err, injected)
	require.Equal(t, []notifications.UpvoteGroupIntent{{
		Action: notifications.UpvoteGroupDeleteIfEmpty, RecipientDID: fixture.author, SubjectURI: fixture.post,
	}}, repository.intents, "the final-commit path must request deletion for the post author")
	require.True(t, repository.deletedInsideTransaction, "the real repository must delete the group inside the aborted transaction")
	var storedID int64
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&storedID, &storedSort))
	assert.Equal(t, groupID, storedID, "rollback must restore the original group row")
	assert.True(t, storedSort.Equal(sortAt), "rollback must restore the original group sort_at")
	exists, active := voteRowState(t, fixture.db, oldURI)
	assert.True(t, exists && active, "A's original upvote must remain live")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, downvoteURI),
		"the replacement downvote insert must roll back")
	assert.Equal(t, before, readSubjectCounts(t, fixture.db, countQuery, fixture.post),
		"the replacement's count mutations must roll back")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, downvoteURI),
		"the new record's rev claim must roll back")

	require.NoError(t, workingConsumer.HandleEvent(context.Background(), downvote), "identical downvote redrive must succeed")
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"positive control: the same replacement deletes the last-voter group")
	exists, active = voteRowState(t, fixture.db, oldURI)
	require.True(t, exists && !active, "the original upvote must be soft-deleted after redrive")
	exists, active = voteRowState(t, fixture.db, downvoteURI)
	require.True(t, exists && active, "the downvote must be live after redrive")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, downvoteURI, downvote.Commit.Rev))
}

func TestVoteConsumer_UpvoteGroupMaintenanceReplacementDeleteFirstWaitsOnErasureLock(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	consumer := fixture.consumer()
	oldURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
	maintenanceGroup(t, fixture)
	replacement := upvoteGroupEvent(voter, fixture.post, "down", fixture.createdAt, testkit.TID())
	replacementURI := "at://" + voter + "/social.coves.feed.vote/" + replacement.Commit.RKey
	require.NotEqual(t, oldURI, replacementURI, "the replacement must have a new record key")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // registered before the fixture transaction, which may still be open on failure
	results := make(chan error, 1)
	started, finished := false, false
	voteGroupResultsCleanup(t, ctx, results, &started, &finished, "HandleEvent(A's replacement vote)")
	transaction, fixtureProcessID := mentionEditRowTransaction(t, ctx, fixture.db)
	_, err := transaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+postgres.ErasureLockKeySQL+")", voter)
	require.NoError(t, err)
	_, err = transaction.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, voter)
	require.NoError(t, err)
	deleted, err := transaction.ExecContext(ctx, `DELETE FROM votes WHERE voter_did = $1`, voter)
	require.NoError(t, err)
	deletedRows, err := deleted.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deletedRows, "the open erasure transaction must hold A's old vote row")

	started = true
	go func() { results <- consumer.HandleEvent(ctx, replacement) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		var waitingBeforeContent bool
		err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks waiter
			JOIN pg_locks holder ON holder.locktype = waiter.locktype
				AND holder.database = waiter.database AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
			WHERE holder.pid = $1 AND waiter.pid <> $1
				AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
				AND NOT waiter.granted AND waiter.mode = 'ShareLock'
				AND $1 = ANY(pg_blocking_pids(waiter.pid))
				AND NOT EXISTS (SELECT 1 FROM pg_locks other WHERE other.pid = waiter.pid
					AND NOT other.granted AND other.locktype IN ('tuple', 'transactionid'))
				AND NOT EXISTS (SELECT 1 FROM pg_locks content WHERE content.pid = waiter.pid
					AND content.locktype = 'relation' AND content.relation IN
					('votes'::regclass, 'posts'::regclass, 'comments'::regclass))
		)`, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("replacement consumer waiting for A's erasure advisory lock before any vote/post/comment row lock"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(A's replacement vote)")
	finished = true
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, voter))
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, oldURI),
		"A's upvote must be hard-deleted by the fixture erasure")
	exists, active := voteRowState(t, fixture.db, replacementURI)
	require.True(t, exists && active, "the replacement must finish indexing after the erasure lock is released")
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"A is erased and has no qualifying upvote, so the old group must be deleted")
}
