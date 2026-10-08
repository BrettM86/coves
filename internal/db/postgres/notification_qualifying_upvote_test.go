//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type qualifyingUpvoteFixture struct {
	db        *sql.DB
	uniqueID  string
	recipient string
	voter     string
	control   string
	thirdUser string
	subject   string
	otherPost string
}

func newQualifyingUpvoteFixture(t *testing.T) qualifyingUpvoteFixture {
	t.Helper()
	db := testkit.DB(t)
	id := testkit.UniqueID(t)
	fixture := qualifyingUpvoteFixture{
		db: db, uniqueID: id,
		recipient: "did:plc:" + id + "recipient",
		voter:     "did:plc:" + id + "voter",
		control:   "did:plc:" + id + "control",
		thirdUser: "did:plc:" + id + "third",
	}
	fixture.subject = "at://" + fixture.recipient + "/social.coves.community.postv2/subject"
	fixture.otherPost = "at://" + fixture.recipient + "/social.coves.community.postv2/control"
	createTestUser(t, db, id+"recipient.test", fixture.recipient)
	return fixture
}

func (fixture qualifyingUpvoteFixture) insertVote(t *testing.T, voter, subject, direction string, createdAt time.Time, deleted bool) {
	t.Helper()
	key := testkit.TID()
	var deletedAt *time.Time
	if deleted {
		deletedAt = &createdAt
	}
	_, err := fixture.db.Exec(`INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, deleted_at)
		VALUES ($1, 'bafyqualifyingvote', $2, $3, $4, 'bafyqualifyingsubject', $5, $6, $7)`,
		"at://"+voter+"/social.coves.feed.vote/"+key, key, voter, subject, direction, createdAt, deletedAt)
	require.NoError(t, err)
}

func (fixture qualifyingUpvoteFixture) insertBlock(t *testing.T, blocker, blocked string) {
	t.Helper()
	_, err := fixture.db.Exec(`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, 'bafyqualifyingblock')`, blocker, blocked,
		"at://"+blocker+"/social.coves.actor.block/"+testkit.TID())
	require.NoError(t, err)
}

func (fixture qualifyingUpvoteFixture) qualifies(t *testing.T, subject, recipient string) bool {
	t.Helper()
	var qualified bool
	err := fixture.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM votes v WHERE `+
		qualifyingUpvoteSQL("v", "$1", "$2")+`)`, subject, recipient).Scan(&qualified)
	require.NoError(t, err)
	return qualified
}

func TestQualifyingUpvoteSQL_LiveUpvotesIgnoreNonqualificationGates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*testing.T, qualifyingUpvoteFixture, time.Time)
	}{
		{"indexed_voter", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			createTestUser(t, fixture.db, fixture.uniqueID+"voter.test", fixture.voter)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"voter_without_users_row", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
			var users int
			require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM users WHERE did = $1`, fixture.voter).Scan(&users))
			require.Zero(t, users)
		}},
		{"before_notification_activation", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`UPDATE notification_activation SET activated_at = $1`, now.Add(time.Hour))
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"eight_days_old", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`UPDATE notification_activation SET activated_at = $1`, now.Add(-30*24*time.Hour))
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now.Add(-8*24*time.Hour), false)
		}},
		{"bridge_pds_voter", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`, fixture.voter,
				fixture.uniqueID+"bridge.test", "https://"+fixture.uniqueID+".bridge.test")
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"blocks_third_user", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.voter, fixture.thirdUser)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"blocked_by_third_user", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.thirdUser, fixture.voter)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"recipient_blocks_third_user", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.recipient, fixture.thirdUser)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"third_user_blocks_recipient", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.thirdUser, fixture.recipient)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"unrelated_erased_account", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.thirdUser)
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"unrelated_aggregator", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
				VALUES ($1, 'Unrelated aggregator', $2, 'bafyqualifyingaggregator')`, fixture.thirdUser,
				"at://"+fixture.thirdUser+"/social.coves.aggregator.service/self")
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newQualifyingUpvoteFixture(t)
			test.setup(t, fixture, time.Now().UTC())
			require.True(t, fixture.qualifies(t, fixture.subject, fixture.recipient),
				"this live upvote must qualify regardless of users row, activation, age, PDS, unrelated blocks, erasures or aggregators")
		})
	}
}

func TestQualifyingUpvoteSQL_OnlyDisqualifiedVoteDoesNotKeepGroup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*testing.T, qualifyingUpvoteFixture, time.Time)
	}{
		{"recipient_self_upvote", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertVote(t, fixture.recipient, fixture.subject, "up", now, false)
		}},
		{"erased_voter", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.voter)
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"aggregator_voter", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			_, err := fixture.db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
				VALUES ($1, 'Aggregator voter', $2, 'bafyqualifyingaggregator')`, fixture.voter,
				"at://"+fixture.voter+"/social.coves.aggregator.service/self")
			require.NoError(t, err)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"recipient_blocks_voter", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.recipient, fixture.voter)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"voter_blocks_recipient", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertBlock(t, fixture.voter, fixture.recipient)
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
		}},
		{"downvote", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertVote(t, fixture.voter, fixture.subject, "down", now, false)
		}},
		{"soft_deleted_upvote", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, true)
		}},
		{"qualifying_upvote_only_on_other_subject", func(t *testing.T, fixture qualifyingUpvoteFixture, now time.Time) {
			// The control vote below is the sole vote: the target subject has none.
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newQualifyingUpvoteFixture(t)
			now := time.Now().UTC()
			test.setup(t, fixture, now)
			// A different subject under the SAME recipient is a positive control.
			fixture.insertVote(t, fixture.control, fixture.otherPost, "up", now, false)
			require.False(t, fixture.qualifies(t, fixture.subject, fixture.recipient),
				"the target subject has no qualifying upvote")
			require.True(t, fixture.qualifies(t, fixture.otherPost, fixture.recipient),
				"positive control: the same query must find a qualifying vote on another subject")
		})
	}
}

func TestQualifyingUpvoteSQL_CorrelatesSubjectAndRecipientWithDifferentAlias(t *testing.T) {
	t.Parallel()
	fixture := newQualifyingUpvoteFixture(t)
	otherRecipient := "did:plc:" + fixture.uniqueID + "otherrecipient"
	createTestUser(t, fixture.db, fixture.uniqueID+"otherrecipient.test", otherRecipient)
	now := time.Now().UTC()
	// Each recipient's upvote on the other recipient's subject qualifies for
	// that other recipient, but the voter's own query must exclude it.
	fixture.insertVote(t, otherRecipient, fixture.subject, "up", now, false)
	fixture.insertVote(t, fixture.recipient, fixture.otherPost, "up", now, false)
	rows, err := fixture.db.Query(`WITH g(label, subject_uri, recipient_did) AS (VALUES
		('first_owner', $1::text, $2::text),
		('first_voter', $1::text, $3::text),
		('second_owner', $4::text, $3::text),
		('second_voter', $4::text, $2::text))
		SELECT g.label, EXISTS (SELECT 1 FROM votes qv WHERE `+
		qualifyingUpvoteSQL("qv", "g.subject_uri", "g.recipient_did")+`)
		FROM g ORDER BY g.label`, fixture.subject, fixture.recipient, otherRecipient, fixture.otherPost)
	require.NoError(t, err)
	defer rows.Close()
	got := make(map[string]bool)
	for rows.Next() {
		var label string
		var qualifies bool
		require.NoError(t, rows.Scan(&label, &qualifies))
		got[label] = qualifies
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]bool{
		"first_owner": true, "first_voter": false,
		"second_owner": true, "second_voter": false,
	}, got, "the alias and both outer columns must correlate separately for each group")
}

// The retention sweep's shape: a DELETE over an aliased notifications row that
// embeds the fragment under NOT EXISTS. The same statement embeds it again, as
// EXISTS, over the pre-delete snapshot of those rows. Each empty group below is
// kept by exactly one wrong correlation.
func TestQualifyingUpvoteSQL_EmbedsTwiceAgainstAliasedNotificationRows(t *testing.T) {
	t.Parallel()
	fixture := newQualifyingUpvoteFixture(t)
	otherRecipient := "did:plc:" + fixture.uniqueID + "otherrecipient"
	createTestUser(t, fixture.db, fixture.uniqueID+"otherrecipient.test", otherRecipient)
	selfUpvotedSubject := "at://" + otherRecipient + "/social.coves.community.postv2/selfupvoted"
	blockedUpvoteSubject := "at://" + otherRecipient + "/social.coves.community.postv2/blockedupvote"
	now := time.Now().UTC()
	// The only qualifying upvote. It is on fixture.subject, so a lost subject
	// correlation lets it keep every other group.
	fixture.insertVote(t, fixture.voter, fixture.subject, "up", now, false)
	// The other recipient's own upvote is the only vote on its subject, so a
	// lost voter-recipient comparison keeps that group.
	fixture.insertVote(t, otherRecipient, selfUpvotedSubject, "up", now, false)
	// The other recipient blocks the only voter on this subject, so a
	// recipient bound to anything else inside user_blocks keeps that group.
	fixture.insertBlock(t, otherRecipient, fixture.thirdUser)
	fixture.insertVote(t, fixture.thirdUser, blockedUpvoteSubject, "up", now, false)

	labels := map[string]string{
		fixture.recipient: "recipient", otherRecipient: "other_recipient",
		fixture.subject: "subject", fixture.otherPost: "other_post",
		selfUpvotedSubject: "self_upvoted", blockedUpvoteSubject: "blocked_upvote",
	}
	for _, group := range [][2]string{
		{fixture.recipient, fixture.subject},
		{fixture.recipient, fixture.otherPost},
		{otherRecipient, selfUpvotedSubject},
		{otherRecipient, blockedUpvoteSubject},
	} {
		_, err := fixture.db.Exec(`INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
			VALUES ($1, 'upvote', $2, $2)`, group[0], group[1])
		require.NoError(t, err)
	}

	rows, err := fixture.db.Query(`WITH swept AS (
			DELETE FROM notifications n
			WHERE n.reason = 'upvote' AND n.recipient_did IN ($1, $2)
				AND NOT EXISTS (SELECT 1 FROM votes swept_vote WHERE `+
		qualifyingUpvoteSQL("swept_vote", "n.subject_uri", "n.recipient_did")+`)
			RETURNING n.id)
		SELECT kept.recipient_did, kept.subject_uri,
			EXISTS (SELECT 1 FROM votes kept_vote WHERE `+
		qualifyingUpvoteSQL("kept_vote", "kept.subject_uri", "kept.recipient_did")+`),
			kept.id IN (SELECT id FROM swept)
		FROM notifications kept
		WHERE kept.reason = 'upvote' AND kept.recipient_did IN ($1, $2)`,
		fixture.recipient, otherRecipient)
	require.NoError(t, err)
	defer rows.Close()
	type groupOutcome struct{ Qualifies, Deleted bool }
	got := make(map[string]groupOutcome)
	for rows.Next() {
		var recipient, subject string
		var outcome groupOutcome
		require.NoError(t, rows.Scan(&recipient, &subject, &outcome.Qualifies, &outcome.Deleted))
		got[labels[recipient]+"/"+labels[subject]] = outcome
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]groupOutcome{
		"recipient/subject":              {Qualifies: true, Deleted: false},
		"recipient/other_post":           {Qualifies: false, Deleted: true},
		"other_recipient/self_upvoted":   {Qualifies: false, Deleted: true},
		"other_recipient/blocked_upvote": {Qualifies: false, Deleted: true},
	}, got, "both embeddings must correlate to their own outer row's subject and recipient")

	var remaining []string
	remainingRows, err := fixture.db.Query(`SELECT subject_uri FROM notifications
		WHERE reason = 'upvote' AND recipient_did IN ($1, $2)`, fixture.recipient, otherRecipient)
	require.NoError(t, err)
	defer remainingRows.Close()
	for remainingRows.Next() {
		var subject string
		require.NoError(t, remainingRows.Scan(&subject))
		remaining = append(remaining, labels[subject])
	}
	require.NoError(t, remainingRows.Err())
	require.Equal(t, []string{"subject"}, remaining, "the sweep must leave only the group with a qualifying upvote")
}
