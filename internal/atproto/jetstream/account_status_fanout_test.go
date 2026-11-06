//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// While A's PDS-confirmed takendown or suspended status is stored
// (account_status_restrictions) and no instance account ban exists, a comment
// create, edit or active re-create by A that Coves indexes late sends no reply
// or mention notification. No hide_reason is recorded for this source, and A's
// votes still count, so clearing the status restores A's content with no undo
// step.

// A late comment by status-restricted A that mentions D notifies D of nothing.
func TestAccountStatusFanout_RestrictedCommentNotifiesNoMention(t *testing.T) {
	t.Parallel()
	fixture, _ := abhMentionFixture(t)
	mentioned := fixture.recipient(t)
	accountstatusfixture.Set(t, fixture.gate.db, fixture.gate.commenterDID, accountstatus.Takendown)

	fixture.create(t, mentioned)
	assert.Equal(t, bhNotHidden, readStoredHideReason(t, fixture.gate.db, "comments", fixture.uri),
		"the status source records no hide_reason")
	assert.Equal(t, 0, mentionEditRows(t, fixture, mentioned, "mention"), "a restricted account's comment mentions no one")
}

// A late reply by status-restricted A to a post notifies the post author of
// nothing.
func TestAccountStatusFanout_RestrictedReplyNotifiesNoOne(t *testing.T) {
	t.Parallel()
	fixture, _ := abhMentionFixture(t)
	accountstatusfixture.Set(t, fixture.gate.db, fixture.gate.commenterDID, accountstatus.Suspended)

	fixture.create(t)
	assert.Equal(t, 0, mentionEditRows(t, fixture, fixture.postAuthor(t), "postReply"),
		"a restricted account's reply notifies the post author of nothing")
	assert.Equal(t, 0, abhRecordNotifications(t, fixture), "a restricted account's reply notifies no one")
}

// A's comment indexed before the restriction, edited while restricted to add
// a mention of E, notifies E of nothing.
func TestAccountStatusFanout_EditWhileRestrictedNotifiesNoAddedMention(t *testing.T) {
	t.Parallel()
	fixture, _ := abhMentionFixture(t)
	fixture.create(t)
	added := fixture.recipient(t)
	accountstatusfixture.Set(t, fixture.gate.db, fixture.gate.commenterDID, accountstatus.Takendown)

	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().UnixMicro(), added))
	assert.Equal(t, 0, mentionEditRows(t, fixture, added, "mention"), "an edit while restricted adds no mention notification")
}

// The same for an active same-rkey re-create, which is an edit.
func TestAccountStatusFanout_ActiveRecreateWhileRestrictedNotifiesNoAddedMention(t *testing.T) {
	t.Parallel()
	fixture, _ := abhMentionFixture(t)
	fixture.create(t)
	added := fixture.recipient(t)
	accountstatusfixture.Set(t, fixture.gate.db, fixture.gate.commenterDID, accountstatus.Suspended)

	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	assert.Equal(t, 0, mentionEditRows(t, fixture, added, "mention"), "an active re-create while restricted adds no mention notification")
}

// A's comment and upvote indexed while A is status-restricted record no
// hide_reason; the upvote counts throughout; once the status is cleared the
// comment is served to another viewer with no undo step.
func TestAccountStatusFanout_ClearingRestoresWithNoHideReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAccountBanHideFixture(t)
	now := time.Now()
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(accAuthor, "create", "abhroot0",
		testkit.TID(), abhRootCID(0), now.UnixMicro(), pv2Record(accCommunity, "thread root", "root body"))))
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, abhRootURI(0), abhRootCID(0),
		testkit.TID(), now.UnixMicro())))
	require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, "abhsubject0", abhSubjectCID(0), testkit.TID(),
		now.UnixMicro(), "", abhRootURI(0), abhRootCID(0), abhRootURI(0), abhRootCID(0))), "B's comment")

	accountstatusfixture.Set(t, f.db, bhAuthor, accountstatus.Takendown)
	require.NoError(t, f.comments.HandleEvent(ctx, abhCommentEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano))))
	require.NoError(t, f.votes.HandleEvent(ctx, abhUpvoteEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano))))

	checkAccountBanHides(t, f, abhComment, []abhRecord{{stage: abhWhileBanned, wantPlaceholder: true, wantHideReason: bhNotHidden}},
		"while A is status-restricted")
	checkAccountBanHides(t, f, abhVote, []abhRecord{{stage: abhWhileBanned, wantUpvotes: 1, wantHideReason: bhNotHidden}},
		"while A is status-restricted")

	accountstatusfixture.Clear(t, f.db, bhAuthor)
	checkAccountBanHides(t, f, abhComment, []abhRecord{{stage: abhWhileBanned, wantPlaceholder: false, wantHideReason: bhNotHidden}},
		"after A's status restriction is cleared")
	checkAccountBanHides(t, f, abhVote, []abhRecord{{stage: abhWhileBanned, wantUpvotes: 1, wantHideReason: bhNotHidden}},
		"after A's status restriction is cleared")
}
