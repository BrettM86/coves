//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationLookups_EarlierUpvoteExists(t *testing.T) {
	t.Parallel()
	const (
		voter        = "did:plc:earlierupvotevoter"
		otherVoter   = "did:plc:earlierupvoteother"
		subject      = "at://did:plc:earlierupvoteauthor/social.coves.community.postv2/subject"
		otherSubject = "at://did:plc:earlierupvoteauthor/social.coves.community.postv2/other"
	)
	for _, test := range []struct {
		name           string
		ownDeleted     bool
		extraVoter     string
		extraSubject   string
		extraDirection string
		extraDeleted   bool
		want           bool
	}{
		{name: "only_own_vote"},
		{name: "live_earlier_upvote", ownDeleted: true, extraVoter: voter, extraSubject: subject, extraDirection: "up", want: true},
		{name: "soft_deleted_earlier_upvote", extraVoter: voter, extraSubject: subject, extraDirection: "up", extraDeleted: true, want: true},
		{name: "soft_deleted_earlier_downvote_only", extraVoter: voter, extraSubject: subject, extraDirection: "down", extraDeleted: true},
		{name: "other_voter_upvote", extraVoter: otherVoter, extraSubject: subject, extraDirection: "up"},
		{name: "other_subject_upvote", extraVoter: voter, extraSubject: otherSubject, extraDirection: "up", extraDeleted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := testkit.DB(t)
			transaction, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer transaction.Rollback()
			now := time.Now().UTC()
			insertVote := func(voterDID, subjectURI, direction, key string, createdAt time.Time, deleted bool) string {
				t.Helper()
				uri := "at://" + voterDID + "/social.coves.feed.vote/" + key
				var deletedAt *time.Time
				if deleted {
					deletedAt = &now
				}
				_, err := transaction.ExecContext(ctx, `INSERT INTO votes
					(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, deleted_at)
					VALUES ($1, 'bafyearlierupvote', $2, $3, $4, 'bafyearliersubject', $5, $6, $7)`,
					uri, key, voterDID, subjectURI, direction, createdAt, deletedAt)
				require.NoError(t, err)
				return uri
			}
			// The unique_voter_subject_active index forbids two live rows for
			// the same voter and subject; only the live-earlier case soft-deletes V.
			voteURI := insertVote(voter, subject, "up", testkit.TID(), now, test.ownDeleted)
			if test.extraVoter != "" {
				insertVote(test.extraVoter, test.extraSubject, test.extraDirection,
					testkit.TID(), now.Add(-time.Hour), test.extraDeleted)
			}
			found, err := NewNotificationRepository(db).LookupsTx(transaction).EarlierUpvoteExists(ctx, voter, subject, voteURI)
			require.NoError(t, err)
			require.Equal(t, test.want, found, "only another upvote by this voter on this subject counts, even if soft-deleted")
		})
	}
}
