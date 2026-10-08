//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestVoteConsumer_RecentUpvotersUseIndexTimeNotRecordTime(t *testing.T) {
	fixture := newUpvoteGroupFixture(t)
	ctx := context.Background()
	var now time.Time
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT now()`).Scan(&now))
	now = now.UTC().Truncate(time.Microsecond)
	// The vote fixture indexes its post directly without an acceptance; the
	// notification list requires a publicly accepted subject.
	acceptanceKey := testkit.TID()
	_, err := fixture.db.ExecContext(ctx, `INSERT INTO community_post_admissions
		(community_did, post_uri, status, acceptance_uri, acceptance_rkey,
		 accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $4, 'bafredrivesubject', 'bafredrivesubject',
		 '3lqqqqqqqqqq2', $5, $6, $6)`, fixture.community, fixture.post,
		"at://"+fixture.community+"/social.coves.community.acceptance/"+acceptanceKey,
		acceptanceKey, int16(posts.CommunityOpPut), now)
	require.NoError(t, err)
	// The shared fixture activates about two minutes ago; V3's hour-old
	// createdAt must be inside the activation and seven-day windows.
	_, err = fixture.db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, now.Add(-2*time.Hour))
	require.NoError(t, err)
	v1, v2, v3 := fixture.voter(t), fixture.voter(t), fixture.voter(t)
	for _, vote := range []struct {
		voter string
		index time.Time
	}{
		{v1, now.Add(-2 * time.Minute)},
		{v2, now.Add(-time.Minute)},
	} {
		key := testkit.TID()
		_, err := fixture.db.ExecContext(ctx, `INSERT INTO votes
			(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
			VALUES ($1, 'bafupvotegroupvote', $2, $3, $4, 'bafupvotesubject', 'up', $5, $6)`,
			"at://"+vote.voter+"/social.coves.feed.vote/"+key, key, vote.voter, fixture.post,
			now.Add(-10*time.Second), vote.index)
		require.NoError(t, err)
	}
	before := now.Add(-5 * time.Minute)
	_, err = fixture.db.ExecContext(ctx, `INSERT INTO notifications
		(recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $2, $3)`, fixture.author, fixture.post, before)
	require.NoError(t, err)
	event := upvoteGroupEvent(v3, fixture.post, "up", now.Add(-time.Hour).Format(time.RFC3339Nano), testkit.TID())
	require.NoError(t, fixture.consumer().HandleEvent(ctx, event))
	var bumped time.Time
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT sort_at FROM notifications
		WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`, fixture.author, fixture.post).Scan(&bumped))
	require.True(t, bumped.After(before), "V3's qualifying vote must bump the existing group")
	page, err := postgres.NewNotificationRepository(fixture.db).(notifications.ReadRepository).List(ctx, fixture.author, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Notifications, 1)
	require.Equal(t, fixture.post, page.Notifications[0].SubjectURI)
	require.Equal(t, 3, page.Notifications[0].UpvoteCount)
	require.Equal(t, []string{v3, v2, v1}, page.Notifications[0].RecentUpvoterDIDs)
}
