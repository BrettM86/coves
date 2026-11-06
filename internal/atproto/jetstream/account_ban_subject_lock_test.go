//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// banSubjectLockWaits reports whether one transaction holds the ban-subject
// advisory lock on accountDID (communities.BanSubjectLockKeySQL) and another
// waits for it. A bigint advisory key is split into classid (high 32 bits) and
// objid (low 32 bits) with objsubid 1.
func banSubjectLockWaits(ctx context.Context, observer *sql.Conn, accountDID string, indexed chan error) testkit.Probe {
	return func() (bool, error) {
		if len(indexed) > 0 {
			return false, fmt.Errorf("A's record finished indexing while the ban was in flight: %v", <-indexed)
		}
		var held, waiting int
		err := observer.QueryRowContext(ctx, `
			SELECT count(*) FILTER (WHERE granted), count(*) FILTER (WHERE NOT granted)
			FROM pg_locks
			WHERE locktype = 'advisory' AND objsubid = 1 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND ((classid::bigint << 32) | objid::bigint) = (`+communities.BanSubjectLockKeySQL+`)::bigint`,
			accountDID).Scan(&held, &waiting)
		return held == 1 && waiting == 1, err
	}
}

// A comment or vote by A that Coves indexes while BanAccount(A) is in flight
// waits for the ban: the ban transaction takes the ban-subject lock on A in
// LockSubject (the ban is paused just after it, at InsertAction), the indexing transaction waits on that same lock, and once the
// ban commits the record classifies against it and is hidden as account_ban.
// Each scenario uses three connections (the pool's limit): the paused ban
// transaction, the waiting indexing transaction, and the pg_locks observer.
func TestAccountBanSubjectLock_IndexingWaitsForAnInFlightBan(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		kind  abhKind
		admin string
	}{
		{name: "comment by the account being banned", kind: abhComment, admin: "did:plc:banlockcommentadmin"},
		{name: "vote by the account being banned", kind: abhVote, admin: "did:plc:banlockvoteadmin"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := newAccountBanHideFixture(t)
			now := time.Now()

			require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(accAuthor, "create", "abhroot0",
				testkit.TID(), abhRootCID(0), now.UnixMicro(), pv2Record(accCommunity, "thread root", "root body"))))
			require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, abhRootURI(0), abhRootCID(0),
				testkit.TID(), now.UnixMicro())))
			if row.kind == abhVote {
				require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, "abhsubject0", abhSubjectCID(0),
					testkit.TID(), now.UnixMicro(), "", abhRootURI(0), abhRootCID(0), abhRootURI(0), abhRootCID(0))))
			}

			observer, err := f.db.Conn(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = observer.Close() })
			store := &pausingModerationStore{
				Store: postgres.NewModerationRepository(f.db), paused: make(chan struct{}), release: make(chan struct{}),
			}
			released := false
			releaseBan := func() {
				if !released {
					released = true
					close(store.release)
				}
			}
			t.Cleanup(releaseBan)
			service := moderation.NewService(
				moderation.NewRepositorySubjectReader(postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)),
				store,
				moderation.Config{
					InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
					Communities: postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()),
				},
			)
			banned := make(chan error, 1)
			go func() {
				_, err := service.BanAccount(ctx, row.admin, moderation.BanAccountRequest{
					Subject: bhAuthor, ExpectedVersion: "v0", IdempotencyKey: "banlock-" + testkit.TID(),
					Reason: "social.coves.moderation.defs#reasonSpam",
				})
				banned <- err
			}()
			select {
			case <-store.paused:
			case err := <-banned:
				t.Fatalf("BanAccount returned before writing its action: %v", err)
			case <-ctx.Done():
				t.Fatal("BanAccount never reached InsertAction")
			}

			indexed := make(chan error, 1)
			go func() {
				if row.kind == abhComment {
					indexed <- f.comments.HandleEvent(ctx, abhCommentEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano)))
					return
				}
				indexed <- f.votes.HandleEvent(ctx, abhUpvoteEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano)))
			}()
			testkit.WaitFor(t, 10*time.Second, banSubjectLockWaits(ctx, observer, bhAuthor, indexed),
				testkit.WithDescription("A's indexing to wait on the ban-subject lock the in-flight ban holds"))

			releaseBan()
			require.NoError(t, <-banned, "the ban commits")
			require.NoError(t, <-indexed, "A's record indexes after the ban")

			table, uri := "comments", abhCommentURI(0)
			if row.kind == abhVote {
				table, uri = "votes", abhVoteURI(0)
			}
			assert.Equal(t, abhHidden, readStoredHideReason(t, f.db, table, uri),
				"A's record indexed behind the ban classifies against it")
		})
	}
}
