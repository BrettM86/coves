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

type upvoteGroupFixture struct {
	db        *sql.DB
	author    string
	community string
	post      string
	createdAt string
}

func newUpvoteGroupFixture(t *testing.T) upvoteGroupFixture {
	t.Helper()
	db := testkit.DB(t)
	id := testkit.UniqueID(t)
	author := "did:plc:" + id + "author"
	community := "did:plc:" + id + "community"
	key := testkit.TID()
	post := pv2URI(author, key)
	seedIndexedPost(t, db, post, community, author, key)
	return upvoteGroupFixture{db: db, author: author, community: community, post: post,
		createdAt: activatedCommentNotificationTime(t, db, context.Background())}
}

func (fixture upvoteGroupFixture) addPost(t *testing.T) string {
	t.Helper()
	key := testkit.TID()
	uri := pv2URI(fixture.author, key)
	_, err := fixture.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'another post', NOW())`,
		uri, key, fixture.author, fixture.community)
	require.NoError(t, err)
	return uri
}

func (fixture upvoteGroupFixture) voter(t *testing.T) string {
	t.Helper()
	id := testkit.UniqueID(t)
	did := "did:plc:" + id + "voter"
	insertBridgedUserOnPDS(t, fixture.db, did, id+"voter.test", bridgedTestNativePDS)
	return did
}

func (fixture upvoteGroupFixture) consumer(options ...VoteEventConsumerOption) *VoteEventConsumer {
	return NewVoteEventConsumer(postgres.NewVoteRepository(fixture.db), newMockUserService(), fixture.db,
		append([]VoteEventConsumerOption{WithVoteNotifications(postgres.NewNotificationRepository(fixture.db))}, options...)...)
}

func upvoteGroupEvent(voter, subject, direction, createdAt, rev string) *JetstreamEvent {
	return revCommitEvent(voter, "social.coves.feed.vote", "create", testkit.TID(), rev,
		"bafupvotegroupvote", time.Now().UnixMicro(), map[string]interface{}{
			"$type": "social.coves.feed.vote", "subject": map[string]interface{}{"uri": subject, "cid": "bafupvotesubject"},
			"direction": direction, "createdAt": createdAt,
		})
}

func deliverGroupVote(t *testing.T, consumer *VoteEventConsumer, voter, subject, direction, createdAt string) string {
	t.Helper()
	event := upvoteGroupEvent(voter, subject, direction, createdAt, testkit.TID())
	require.NoError(t, consumer.HandleEvent(context.Background(), event))
	uri := "at://" + voter + "/social.coves.feed.vote/" + event.Commit.RKey
	exists, active := voteRowState(t, consumer.db, uri)
	require.True(t, exists && active, "fixture: the vote must be indexed and active")
	return uri
}

func groupCount(t *testing.T, db *sql.DB, recipient, subject string) int {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, recipient, subject)
}

func TestVoteConsumer_UpvoteGroupsPostCommentAndLegacyAuthor(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"postv2", "comment", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			recipient, subject, root := fixture.author, fixture.post, fixture.post
			switch kind {
			case "comment":
				// A second indexed user writes the comment under fixture.author's post.
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
			case "legacy":
				key := testkit.TID()
				subject = "at://" + fixture.community + "/social.coves.community.post/" + key
				root = subject
				_, err := fixture.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
					VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'legacy post', NOW())`,
					subject, key, fixture.author, fixture.community)
				require.NoError(t, err)
			}
			deliverGroupVote(t, fixture.consumer(), fixture.voter(t), subject, "up", fixture.createdAt)
			require.Equal(t, 1, groupCount(t, fixture.db, recipient, subject),
				"a qualifying upvote must create exactly one group for the actual author")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
				WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND root_post_uri = $3
				AND record_uri IS NULL AND record_cid IS NULL AND actor_did IS NULL`, recipient, subject, root),
				"group navigation and nullable record/actor fields must match the subject")
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications`))
			if kind == "comment" {
				require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE recipient_did = $1`,
					fixture.author), "the root post's author did not write the comment")
			}
			if kind == "legacy" {
				require.Zero(t, groupCount(t, fixture.db, fixture.community, subject), "the community is not the legacy post author")
			}
		})
	}
}

type recordingVoteErasureRepository struct {
	notifications.Repository
	erasureCalls int
}

func (repository *recordingVoteErasureRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, did string) (bool, error) {
	repository.erasureCalls++
	return repository.Repository.ErasureGateTx(ctx, tx, did)
}

func TestVoteConsumer_UpvoteGroupBumpsOnlyForNewVotes(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	first := upvoteGroupEvent(fixture.voter(t), fixture.post, "up", fixture.createdAt, revA)
	consumer := fixture.consumer()
	require.NoError(t, consumer.HandleEvent(context.Background(), first))
	var groupID int64
	var initialSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications WHERE reason = 'upvote'
		AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&groupID, &initialSort),
		"the first vote must create the group before a bump can be measured")
	deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
	var bumpedID int64
	var bumpedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications WHERE reason = 'upvote'
		AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&bumpedID, &bumpedSort))
	assert.Equal(t, groupID, bumpedID, "another voter bumps the same group row")
	assert.True(t, bumpedSort.After(initialSort), "another qualifying vote must raise sort_at")
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	firstURI := "at://" + first.Did + "/social.coves.feed.vote/" + first.Commit.RKey
	recorder := &recordingVoteErasureRepository{Repository: postgres.NewNotificationRepository(fixture.db)}
	replayConsumer := fixture.consumer(WithVoteNotifications(recorder))
	for index, rev := range []string{revB, revB, revA} {
		past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
		_, err := fixture.db.Exec(`UPDATE notifications SET sort_at = $1 WHERE id = $2`, past, groupID)
		require.NoError(t, err)
		replay := *first
		commit := *first.Commit
		commit.Rev = rev
		replay.Commit = &commit
		beforeCalls := recorder.erasureCalls
		require.NoError(t, replayConsumer.HandleEvent(context.Background(), &replay))
		var storedID int64
		var storedSort time.Time
		require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications WHERE id = $1`, groupID).Scan(&storedID, &storedSort))
		assert.Equal(t, groupID, storedID)
		assert.True(t, storedSort.Equal(past), "rev %s replay must not bump the group", rev)
		assert.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
		assert.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, firstURI))
		assert.Equal(t, 2, readSubjectCounts(t, fixture.db,
			`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes)
		var storedRev string
		require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, firstURI).Scan(&storedRev))
		assert.Equal(t, revB, storedRev, "the higher rev wins even when the vote insert conflicts")
		if index == 0 {
			assert.Equal(t, beforeCalls+1, recorder.erasureCalls,
				"the higher rev must pass the gate before its vote insert conflicts")
		} else {
			assert.Equal(t, beforeCalls, recorder.erasureCalls, "rev-gate losses must not call ErasureGateTx")
		}
	}
}

func TestVoteConsumer_UpvoteGroupSelfAndDownvoteGuards(t *testing.T) {
	t.Parallel()
	t.Run("self_upvote_guard", func(t *testing.T) {
		fixture := newUpvoteGroupFixture(t)
		deliverGroupVote(t, fixture.consumer(), fixture.author, fixture.post, "up", fixture.createdAt)
		require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post), "self-upvotes must not notify")
	})
	t.Run("downvote_guard", func(t *testing.T) {
		fixture := newUpvoteGroupFixture(t)
		deliverGroupVote(t, fixture.consumer(), fixture.voter(t), fixture.post, "down", fixture.createdAt)
		require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post), "downvotes must not create groups")
	})
}

func TestVoteConsumer_UpvoteGroupGuardsHaveEligibleSiblingControl(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	other := fixture.addPost(t)
	deliverGroupVote(t, fixture.consumer(), fixture.voter(t), other, "up", fixture.createdAt)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, other),
		"positive control: the indexed author can receive a sibling-post upvote")
}

func ineligibleUpvoteTime(t *testing.T, fixture upvoteGroupFixture, gate, voter string) string {
	t.Helper()
	createdAt := fixture.createdAt
	switch gate {
	case "recipient_blocks_voter", "voter_blocks_recipient":
		blocker, blocked := fixture.author, voter
		if gate == "voter_blocks_recipient" {
			blocker, blocked = voter, fixture.author
		}
		_, err := fixture.db.Exec(`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
			VALUES ($1, $2, $3, 'bafupvoteblock')`, blocker, blocked,
			"at://"+blocker+"/"+CovesActorBlockCollection+"/"+testkit.TID())
		require.NoError(t, err)
	case "before_activation", "older_than_seven_days":
		var now time.Time
		require.NoError(t, fixture.db.QueryRow(`SELECT NOW()`).Scan(&now))
		activation := now.Add(-2 * time.Minute)
		created := now.Add(-time.Hour)
		if gate == "older_than_seven_days" {
			activation, created = now.Add(-30*24*time.Hour), now.Add(-8*24*time.Hour)
		}
		_, err := fixture.db.Exec(`UPDATE notification_activation SET activated_at = $1`, activation)
		require.NoError(t, err)
		createdAt = created.UTC().Format(time.RFC3339Nano)
	}
	return createdAt
}

func TestVoteConsumer_UpvoteGroupIneligibleVotesDoNotCreate(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"recipient_blocks_voter", "voter_blocks_recipient", "before_activation", "older_than_seven_days"} {
		t.Run(gate, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			voter := fixture.voter(t)
			createdAt := ineligibleUpvoteTime(t, fixture, gate, voter)
			deliverGroupVote(t, fixture.consumer(), voter, fixture.post, "up", createdAt)
			require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
				"%s must not create a group for the indexed author", gate)
		})
	}
}

func TestVoteConsumer_UpvoteGroupIneligibleVotesDoNotBumpOrCreate(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"recipient_blocks_voter", "voter_blocks_recipient", "before_activation", "older_than_seven_days"} {
		t.Run(gate, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
			var groupID int64
			require.NoError(t, fixture.db.QueryRow(`SELECT id FROM notifications WHERE reason = 'upvote'
				AND recipient_did = $1 AND subject_uri = $2`, fixture.author, fixture.post).Scan(&groupID),
				"a qualifying vote must keep this group alive before testing suppression")
			secondPost := fixture.addPost(t)
			voter := fixture.voter(t)
			createdAt := ineligibleUpvoteTime(t, fixture, gate, voter)
			past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
			_, err := fixture.db.Exec(`UPDATE notifications SET sort_at = $1 WHERE id = $2`, past, groupID)
			require.NoError(t, err)
			deliverGroupVote(t, consumer, voter, fixture.post, "up", createdAt)
			var sortAt time.Time
			require.NoError(t, fixture.db.QueryRow(`SELECT sort_at FROM notifications WHERE id = $1`, groupID).Scan(&sortAt))
			assert.True(t, sortAt.Equal(past), "%s must not bump the existing group", gate)
			deliverGroupVote(t, consumer, voter, secondPost, "up", createdAt)
			assert.Zero(t, groupCount(t, fixture.db, fixture.author, secondPost), "%s must not create a group", gate)
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
		})
	}
}

func TestVoteConsumer_UpvoteGroupRecipientEligibility(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"aggregator", "erased", "unindexed", "trusted_bridge"} {
		t.Run(gate, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			control := "did:plc:" + testkit.UniqueID(t) + "control"
			insertBridgedUserOnPDS(t, fixture.db, control, testkit.UniqueID(t)+"control.test", bridgedTestNativePDS)
			controlKey := testkit.TID()
			controlPost := pv2URI(control, controlKey)
			_, err := fixture.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
				VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'control', NOW())`,
				controlPost, controlKey, control, fixture.community)
			require.NoError(t, err)
			options := []VoteEventConsumerOption{}
			switch gate {
			case "aggregator":
				_, err = fixture.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
					VALUES ($1, 'Aggregator recipient', $2, 'bafupvoteservice')`, fixture.author,
					"at://"+fixture.author+"/social.coves.aggregator.service/self")
			case "erased":
				_, err = fixture.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.author)
			case "unindexed":
				_, err = fixture.db.Exec(`DELETE FROM users WHERE did = $1`, fixture.author)
			case "trusted_bridge":
				insertBridgedUserOnPDS(t, fixture.db, fixture.author, testkit.UniqueID(t)+"author.test", bridgedTestPDS)
				options = append(options, WithVoteBridgeTrust(NewBridgeTrust([]string{bridgedTestPDS})))
			}
			require.NoError(t, err)
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM posts WHERE uri = $1`, fixture.post),
				"fixture: recipient's post must still be countable")
			t.Run("suppressed", func(t *testing.T) {
				deliverGroupVote(t, fixture.consumer(options...), fixture.voter(t), fixture.post, "up", fixture.createdAt)
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
					"%s recipient must not receive a group", gate)
			})
			t.Run("eligible_control", func(t *testing.T) {
				deliverGroupVote(t, fixture.consumer(options...), fixture.voter(t), controlPost, "up", fixture.createdAt)
				require.Equal(t, 1, groupCount(t, fixture.db, control, controlPost),
					"eligible control recipient must receive a group under the same gate")
			})
			if gate == "trusted_bridge" {
				t.Run("without_trust", func(t *testing.T) {
					deliverGroupVote(t, fixture.consumer(), fixture.voter(t), fixture.post, "up", fixture.createdAt)
					require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
						"without bridge trust the same bridge-hosted recipient receives a group")
				})
			}
		})
	}
}

// Q4: an aggregator or erased voter gives no group, while the vote itself is
// still indexed and counted. An eligible voter on the same post is the control.
func TestVoteConsumer_UpvoteGroupVoterEligibility(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"aggregator", "erased"} {
		t.Run(gate, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			voter := fixture.voter(t)
			var err error
			switch gate {
			case "aggregator":
				_, err = fixture.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
					VALUES ($1, 'Aggregator voter', $2, 'bafupvoteservice')`, voter,
					"at://"+voter+"/social.coves.aggregator.service/self")
			case "erased":
				_, err = fixture.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, voter)
			}
			require.NoError(t, err)
			consumer := fixture.consumer()
			t.Run("suppressed", func(t *testing.T) {
				deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
				require.Equal(t, 1, readSubjectCounts(t, fixture.db,
					`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes,
					"the %s voter's vote must still be counted", gate)
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
					"an %s voter must not create a group", gate)
			})
			t.Run("eligible_control", func(t *testing.T) {
				deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
				require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
					"an eligible voter on the same post must create the group")
			})
		})
	}
}

// Q4: the bridge rule applies to the recipient only, so a voter hosted on a
// trusted bridge PDS still bumps a native recipient's group.
func TestVoteConsumer_UpvoteGroupBridgeHostedVoterBumps(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	// seedIndexedPost puts the author on the bridge host; this recipient is native.
	insertBridgedUserOnPDS(t, fixture.db, fixture.author, testkit.UniqueID(t)+"author.test", bridgedTestNativePDS)
	id := testkit.UniqueID(t)
	voter := "did:plc:" + id + "bridgevoter"
	insertBridgedUserOnPDS(t, fixture.db, voter, id+"bridgevoter.test", bridgedTestPDS)
	trust := NewBridgeTrust([]string{bridgedTestPDS})
	require.True(t, trust.TrustsPDS(bridgedTestPDS), "fixture: the consumer must trust the voter's PDS host")
	deliverGroupVote(t, fixture.consumer(WithVoteBridgeTrust(trust)), voter, fixture.post, "up", fixture.createdAt)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
		"a voter on a trusted bridge PDS must still bump the native recipient's group")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications`))
}

type failingUpvoteGroupRepository struct {
	notifications.Repository
	failure error
	intents []notifications.UpvoteGroupIntent
}

func (repository *failingUpvoteGroupRepository) ApplyUpvoteGroupTx(_ context.Context, _ *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	repository.intents = append(repository.intents, intent)
	return repository.failure
}

func TestVoteConsumer_UpvoteGroupFailureRollsBackVoteCountAndRev(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	event := upvoteGroupEvent(voter, fixture.post, "up", fixture.createdAt, testkit.TID())
	voteURI := "at://" + voter + "/social.coves.feed.vote/" + event.Commit.RKey
	injected := errors.New("injected upvote group write failure")
	repository := &failingUpvoteGroupRepository{Repository: postgres.NewNotificationRepository(fixture.db), failure: injected}
	consumer := fixture.consumer(WithVoteNotifications(repository))
	err := consumer.HandleEvent(context.Background(), event)
	require.Equal(t, []notifications.UpvoteGroupIntent{{Action: notifications.UpvoteGroupBump,
		RecipientDID: fixture.author, SubjectURI: fixture.post, RootPostURI: fixture.post}}, repository.intents,
		"fan-out must request precisely the author's post group")
	require.ErrorIs(t, err, injected, "notification write failure must propagate")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, voteURI), "failed vote insert must roll back")
	assert.Zero(t, readSubjectCounts(t, fixture.db,
		`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes,
		"failed vote count must roll back")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, voteURI),
		"failed rev claim must roll back")
	assert.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post))
	require.NoError(t, fixture.consumer().HandleEvent(context.Background(), event), "identical retry must succeed")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, voteURI))
	require.Equal(t, 1, readSubjectCounts(t, fixture.db,
		`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, voteURI, event.Commit.Rev))
}

func TestVoteConsumer_UpvoteGroupMissingActivationRollsBackVoteCountAndRev(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	var activatedAt time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT activated_at FROM notification_activation`).Scan(&activatedAt),
		"fixture: the activation row must exist before it is removed")
	result, err := fixture.db.Exec(`DELETE FROM notification_activation`)
	require.NoError(t, err)
	removed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, removed, "fixture: the singleton activation row must be removed")
	event := upvoteGroupEvent(voter, fixture.post, "up", fixture.createdAt, testkit.TID())
	voteURI := "at://" + voter + "/social.coves.feed.vote/" + event.Commit.RKey
	consumer := fixture.consumer()
	err = consumer.HandleEvent(context.Background(), event)
	require.ErrorIs(t, err, postgres.ErrNotificationActivationMissing, "a missing activation row must fail the vote's transaction")
	assert.ErrorContains(t, err, "compute vote notifications")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, voteURI), "failed vote insert must roll back")
	assert.Zero(t, readSubjectCounts(t, fixture.db,
		`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes,
		"failed vote count must roll back")
	assert.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, voteURI),
		"failed rev claim must roll back")
	assert.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post))
	_, err = fixture.db.Exec(`INSERT INTO notification_activation (activated_at) VALUES ($1)`, activatedAt)
	require.NoError(t, err, "fixture: restore the activation row")
	require.NoError(t, consumer.HandleEvent(context.Background(), event), "identical retry must succeed once the row is restored")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, voteURI))
	require.Equal(t, 1, readSubjectCounts(t, fixture.db,
		`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, voteURI, event.Commit.Rev))
}
