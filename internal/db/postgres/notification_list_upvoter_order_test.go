//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationList_RecentUpvotersIndexTimeAndHigherIDTieBreak(t *testing.T) {
	type indexedVote struct {
		name       string
		indexedAgo time.Duration
		createdAgo time.Duration
	}
	for _, tc := range []struct {
		name  string
		votes []indexedVote
		want  []string
	}{
		{
			name:  "equal indexed time uses later insert even when createdAt is older",
			votes: []indexedVote{{"V4", time.Minute, time.Second}, {"V5", time.Minute, time.Hour}},
			want:  []string{"V5", "V4"},
		},
		{
			name: "five votes and third-place indexed tie",
			votes: []indexedVote{
				{"V1", time.Minute, 5 * time.Minute},
				{"V2", 2 * time.Minute, 4 * time.Minute},
				{"V3", 3 * time.Minute, 2 * time.Minute},
				{"V4", 3 * time.Minute, 3 * time.Minute},
				{"V5", 4 * time.Minute, time.Minute},
			},
			want: []string{"V1", "V2", "V4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadCountFixture(t)
			ctx := context.Background()
			var now time.Time
			require.NoError(t, f.db.QueryRowContext(ctx, `SELECT now()`).Scan(&now))
			now = now.UTC().Truncate(time.Microsecond)
			f.listGroupAt(t, f.root, f.root, now)
			dids := make(map[string]string)
			for _, vote := range tc.votes {
				voter := "did:plc:listorder" + testkit.UniqueID(t)
				dids[vote.name] = voter
				key := testkit.TID()
				_, err := f.db.ExecContext(ctx, `INSERT INTO votes
					(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
					VALUES ($1, 'bafylistvote', $2, $3, $4, 'bafylistvotesubject', 'up', $5, $6)`,
					"at://"+voter+"/social.coves.feed.vote/"+key, key, voter, f.root,
					now.Add(-vote.createdAgo), now.Add(-vote.indexedAgo))
				require.NoError(t, err)
			}
			want := make([]string, 0, len(tc.want))
			for _, name := range tc.want {
				want = append(want, dids[name])
			}
			for read := 0; read < 3; read++ {
				page := f.listPage(t, f.recipient, "", 10)
				require.Len(t, page.Notifications, 1)
				require.Equal(t, f.root, page.Notifications[0].SubjectURI)
				require.Equal(t, len(tc.votes), page.Notifications[0].UpvoteCount)
				require.Equal(t, want, page.Notifications[0].RecentUpvoterDIDs, "List call %d", read+1)
			}
		})
	}
}
