//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func (f *unreadVisibilityFixture) notifyFromActor(t *testing.T, actor, reason, record, subject string) {
	t.Helper()
	var subjectURI any = subject
	if reason == "mention" {
		subjectURI = nil
	}
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, $2, $3, 'bafyunreadrecord', $4, $5, $6, $7, $7)`,
		f.recipient, reason, record, actor, subjectURI, f.root, f.sortAt)
	require.NoError(t, err)
}

func (f *unreadVisibilityFixture) insertVote(t *testing.T, voter, subject string, retracted bool) {
	t.Helper()
	key := testkit.TID()
	var deletedAt any
	if retracted {
		deletedAt = f.sortAt
	}
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, deleted_at)
		VALUES ($1, 'bafyunreadvote', $2, $3, $4, 'bafyunreadsubject', 'up', $5, $6)`,
		"at://"+voter+"/social.coves.feed.vote/"+key, key, voter, subject, f.sortAt, deletedAt)
	require.NoError(t, err)
}

func (f *unreadVisibilityFixture) insertBlock(t *testing.T, blocker, blocked string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO user_blocks
		(blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, 'bafyunreadblock')`, blocker, blocked,
		"at://"+blocker+"/social.coves.actor.block/"+testkit.TID())
	require.NoError(t, err)
}

func (f *unreadVisibilityFixture) eraseDID(t *testing.T, did string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO deleted_accounts (did) VALUES ($1)`, did)
	require.NoError(t, err)
}

func (f *unreadVisibilityFixture) setDisabledReasons(t *testing.T, did string, reasons []string) {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(), `UPDATE notification_state
		SET disabled_reasons = $2 WHERE did = $1`, did, pq.Array(reasons))
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated)
}

func TestNotificationUnreadRules_BlocksHideRecordRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason, blocker string
		deletedRecord         bool
		want                  int
	}{
		{"recipient blocks actor", "commentReply", "recipient", false, 1},
		{"actor blocks recipient", "mention", "actor", false, 1},
		{"recipient blocks actor of deleted comment", "commentReply", "recipient", true, 1},
		{"actor blocks recipient of deleted comment", "commentReply", "actor", true, 1},
		{"actor blocks unrelated DID", "commentReply", "third", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnreadVisibilityFixture(t)
			actor := "did:plc:unreadcase" + testkit.UniqueID(t)
			record := seedActorComment(t, f.db, actor, f.root, testkit.TID(), f.sortAt)
			if tc.deletedRecord {
				f.deleteComment(t, record)
			}
			subject := seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.sortAt)
			f.notifyFromActor(t, actor, tc.reason, record, subject)
			switch tc.blocker {
			case "recipient":
				f.insertBlock(t, f.recipient, actor)
			case "actor":
				f.insertBlock(t, actor, f.recipient)
			case "third":
				f.insertBlock(t, actor, "did:plc:unreadthird"+testkit.UniqueID(t))
			}
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadRules_UpvoteGroupNeedsQualifyingVote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subjectKind, votes string
		want                     int
	}{
		{"blocked V1 plus live V2", "post", "liveSecond", 2},
		{"both voters blocked by recipient", "post", "blockedSecond", 1},
		{"V2 blocks recipient", "post", "secondBlocks", 1},
		{"V2 retracts upvote", "post", "retractedSecond", 1},
		{"V2 erased", "post", "erasedSecond", 1},
		{"only recipient self upvote", "post", "self", 1},
		{"vote only on another accepted post", "post", "otherSubject", 1},
		{"deleted recipient comment with live vote", "deletedComment", "live", 2},
		{"community withdrawn post with live vote", "removedPost", "live", 2},
		{"community withdrawn post with retracted vote", "removedPost", "retracted", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnreadVisibilityFixture(t)
			subject := f.post(t, posts.AdmissionStatusAccepted, false)
			switch tc.subjectKind {
			case "deletedComment":
				subject = seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.sortAt)
				f.deleteComment(t, subject)
			case "removedPost":
				subject = f.post(t, posts.AdmissionStatusRemoved, false)
				seedWithdrawalMarker(t, f.db, subject, "communityWithdrawal", "3lqqqqqqqqqq1")
			}
			f.notify(t, "upvote", "", subject, f.root)
			voterOne := "did:plc:unreadvoter" + testkit.UniqueID(t)
			voterTwo := "did:plc:unreadvoter" + testkit.UniqueID(t)
			switch tc.votes {
			case "liveSecond", "blockedSecond", "secondBlocks", "retractedSecond", "erasedSecond":
				f.insertVote(t, voterOne, subject, false)
				f.insertBlock(t, f.recipient, voterOne)
				f.insertVote(t, voterTwo, subject, tc.votes == "retractedSecond")
				switch tc.votes {
				case "blockedSecond":
					f.insertBlock(t, f.recipient, voterTwo)
				case "secondBlocks":
					f.insertBlock(t, voterTwo, f.recipient)
				case "erasedSecond":
					f.eraseDID(t, voterTwo)
				}
			case "self":
				f.insertVote(t, f.recipient, subject, false)
			case "otherSubject":
				f.insertVote(t, voterTwo, f.post(t, posts.AdmissionStatusAccepted, false), false)
			case "live", "retracted":
				f.insertVote(t, voterTwo, subject, tc.votes == "retracted")
			}
			f.requireCount(t, tc.want)
		})
	}
}

func TestNotificationUnreadRules_DisabledReasonsHideRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, scenario string
		want           int
	}{
		{"recipient disables mention then enables it", "mentionToggle", 2},
		{"recipient disables commentReply including placeholder", "commentReply", 1},
		{"null element does not stop mention filter", "nullMention", 1},
		{"another user's preference does not apply", "otherUser", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnreadVisibilityFixture(t)
			actor := "did:plc:unreadcase" + testkit.UniqueID(t)
			record := seedActorComment(t, f.db, actor, f.root, testkit.TID(), f.sortAt)
			f.notifyFromActor(t, actor, "mention", record, "")
			switch tc.scenario {
			case "mentionToggle":
				f.notifyFromActor(t, actor, "postReply",
					seedActorComment(t, f.db, actor, f.root, testkit.TID(), f.sortAt), f.root)
				f.setDisabledReasons(t, f.recipient, []string{"mention"})
			case "commentReply":
				placeholder := seedActorComment(t, f.db, actor, f.root, testkit.TID(), f.sortAt)
				f.deleteComment(t, placeholder)
				f.notifyFromActor(t, actor, "commentReply", placeholder,
					seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.sortAt))
				f.setDisabledReasons(t, f.recipient, []string{"commentReply"})
			case "nullMention":
				_, err := f.db.ExecContext(context.Background(), `UPDATE notification_state
					SET disabled_reasons = ARRAY[NULL,'mention']::text[] WHERE did = $1`, f.recipient)
				require.NoError(t, err)
			case "otherUser":
				otherUser := retentionUser(t, f.db)
				retentionSeenAt(t, f.db, otherUser, f.sortAt)
				f.setDisabledReasons(t, otherUser, []string{"mention"})
				f.setDisabledReasons(t, f.recipient, []string{})
			}
			f.requireCount(t, tc.want)
			if tc.scenario == "mentionToggle" {
				f.setDisabledReasons(t, f.recipient, []string{})
				f.requireCount(t, 3)
			}
		})
	}
}
