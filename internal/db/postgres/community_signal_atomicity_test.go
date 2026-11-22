//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installCommitFailure makes every later transaction that inserts or updates
// the community row for did fail at COMMIT: a deferred constraint trigger runs
// after every statement of the transaction has succeeded.
func installCommitFailure(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE test_doomed_communities (did TEXT PRIMARY KEY)`,
		`CREATE FUNCTION test_fail_doomed_community_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM test_doomed_communities WHERE did = NEW.did) THEN
				RAISE EXCEPTION 'test: forced commit failure for %', NEW.did;
			END IF;
			RETURN NULL;
		END $$`,
		`CREATE CONSTRAINT TRIGGER test_fail_doomed_community_commit
			AFTER INSERT OR UPDATE ON communities
			DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_fail_doomed_community_commit()`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "installing the commit-failure trigger")
	}
	_, err := db.Exec(`INSERT INTO test_doomed_communities (did) VALUES ($1)`, did)
	require.NoError(t, err)
}

func atomicityCommunity(did, name string, warnings []string) *communities.Community {
	return &communities.Community{
		DID:             did,
		Handle:          name + ".lemmy-world.tdpl.io",
		Name:            name,
		DisplayName:     "Atomicity " + name,
		OwnerDID:        did,
		CreatedByDID:    "did:plc:atomicitycreator",
		HostedByDID:     "did:web:tdpl.io",
		PDSURL:          "https://pds.tdpl.io",
		Visibility:      "public",
		Origin:          "lemmy.world",
		ContentWarnings: warnings,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
}

// TestCommunityFlagEvent_CommitFailureLeavesNoEvent is B6: the flag event is
// written in the same transaction as the community row, so a transaction that
// fails at COMMIT leaves neither the event nor the row change behind.
func TestCommunityFlagEvent_CommitFailureLeavesNoEvent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string)
	}{
		{
			name: "create with nsfw",
			run: func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string) {
				installCommitFailure(t, db, did)

				_, err := repo.Create(context.Background(), atomicityCommunity(did, name, []string{"nsfw"}))
				require.Error(t, err, "a create whose transaction fails at COMMIT must return an error")

				var rows int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&rows))
				assert.Equal(t, 0, rows, "the community row must be absent after the failed create")
			},
		},
		{
			name: "update adding nsfw",
			run: func(t *testing.T, db *sql.DB, repo communities.Repository, did, name string) {
				ctx := context.Background()
				_, err := repo.Create(ctx, atomicityCommunity(did, name, []string{"spoiler"}))
				require.NoError(t, err, "fixture: the community must index without nsfw")
				installCommitFailure(t, db, did)

				stored, err := repo.GetByDID(ctx, did)
				require.NoError(t, err)
				stored.ContentWarnings = []string{"spoiler", "nsfw"}
				_, err = repo.Update(ctx, stored)
				require.Error(t, err, "an update whose transaction fails at COMMIT must return an error")

				var warnings []string
				require.NoError(t, db.QueryRow(`SELECT content_warnings FROM communities WHERE did = $1`, did).
					Scan(pq.Array(&warnings)))
				assert.Equal(t, []string{"spoiler"}, warnings, "content_warnings must be unchanged after the failed update")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			name := "atomic" + testkit.UniqueID(t)
			did := "did:plc:" + name
			repo := NewCommunityRepository(db, credentialciphertest.Fixed())

			tc.run(t, db, repo, did, name)

			assert.Empty(t, readFlagEvents(t, db, did), "a transaction that failed at COMMIT must leave no flag event")
		})
	}
}

// waitForLockWaiters polls, on the coordinating transaction's own connection,
// until want other backends in this test's database are waiting on a lock, or
// the deadline passes. pg_stat_clear_snapshot drops the per-transaction
// statistics snapshot so each poll sees live backend state.
func waitForLockWaiters(ctx context.Context, tx *sql.Tx, want int) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := tx.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return err
		}
		var waiting int
		err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("only %d of %d backends were waiting on a lock: %w", waiting, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

// TestCommunityFlagEvent_ConcurrentUpdatesRecordOneEvent is C1: two updates
// of one community that both add nsfw, overlapping on the row, record exactly
// one false-to-true flag event, because each update compares against the
// version it replaces rather than a value read before the other committed.
func TestCommunityFlagEvent_ConcurrentUpdatesRecordOneEvent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	name := "atomic" + testkit.UniqueID(t)
	did := "did:plc:" + name
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())

	_, err := repo.Create(ctx, atomicityCommunity(did, name, []string{"spoiler"}))
	require.NoError(t, err, "fixture: the community must index without nsfw")

	updates := make([]*communities.Community, 2)
	for i := range updates {
		stored, getErr := repo.GetByDID(ctx, did)
		require.NoError(t, getErr)
		stored.ContentWarnings = []string{"spoiler", "nsfw"}
		updates[i] = stored
	}

	coordinator, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer coordinator.Rollback()
	var lockedDID string
	require.NoError(t, coordinator.QueryRowContext(ctx,
		`SELECT did FROM communities WHERE did = $1 FOR UPDATE`, did).Scan(&lockedDID))

	errs := make([]error, len(updates))
	var wg sync.WaitGroup
	for i, update := range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = repo.Update(ctx, update)
		}()
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	waitErr := waitForLockWaiters(waitCtx, coordinator, len(updates))
	cancel()
	if waitErr != nil {
		_ = coordinator.Rollback()
		wg.Wait()
		t.Fatalf("both updates must block behind the coordinating row lock: %v", waitErr)
	}
	require.NoError(t, coordinator.Commit())
	wg.Wait()

	for i, updateErr := range errs {
		require.NoError(t, updateErr, "update %d", i)
	}
	event := onlyFlagEvent(t, db, did)
	assert.False(t, event.OldNSFW, "the event must record the change from not nsfw")
	assert.True(t, event.NewNSFW, "the event must record the change to nsfw")
}
