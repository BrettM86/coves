//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotDiscardsAdvisoryLockConnectionOnAnomaly(t *testing.T) {
	scopeLockKey := discoverHotSnapshotLockKey(discoverHotViewerScope(""))
	globalAdmissionLockKey := discoverHotSnapshotAdmissionLockKey()
	for _, testCase := range []struct {
		name       string
		targetLock int64
		acquire    bool
	}{
		{
			name:       "scope acquisition errors after taking the lock",
			targetLock: scopeLockKey,
			acquire:    true,
		},
		{
			name:       "global admission acquisition errors after taking the lock",
			targetLock: globalAdmissionLockKey,
			acquire:    true,
		},
		{
			name:       "scope release reports lock not held",
			targetLock: scopeLockKey,
		},
		{
			name:       "global admission release reports lock not held",
			targetLock: globalAdmissionLockKey,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := testkit.DB(t)
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			ctx := context.Background()
			seedDiscoverHotSnapshotCandidates(t, db, "advisory-lock-anomaly", []int{100})

			poisonedConnection, err := db.Conn(ctx)
			require.NoError(t, err)
			var databaseName string
			require.NoError(t, poisonedConnection.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName))
			_, err = poisonedConnection.ExecContext(ctx, `CREATE SCHEMA discover_hot_lock_test`)
			require.NoError(t, err)
			poisonSQL := fmt.Sprintf(`
				CREATE FUNCTION discover_hot_lock_test.pg_advisory_unlock(lock_key bigint)
				RETURNS boolean LANGUAGE plpgsql AS $$
				BEGIN
					IF lock_key = %d THEN
						RETURN false;
					END IF;
					RETURN pg_catalog.pg_advisory_unlock(lock_key);
				END
				$$`, testCase.targetLock)
			if testCase.acquire {
				poisonSQL = fmt.Sprintf(`
					CREATE FUNCTION discover_hot_lock_test.pg_advisory_lock(lock_key bigint)
					RETURNS void LANGUAGE plpgsql AS $$
					BEGIN
						PERFORM pg_catalog.pg_advisory_lock(lock_key);
						IF lock_key = %d THEN
							RAISE EXCEPTION 'forced acquisition error after taking lock';
						END IF;
					END
					$$`, testCase.targetLock)
			}
			_, err = poisonedConnection.ExecContext(ctx, poisonSQL)
			require.NoError(t, err)
			_, err = poisonedConnection.ExecContext(ctx, `SET search_path = discover_hot_lock_test, pg_catalog, public`)
			require.NoError(t, err)
			require.NoError(t, poisonedConnection.Close())

			repository := NewDiscoverRepository(db, "advisory-lock-safety-secret")
			feed, cursor, getErr := repository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
			require.Error(t, getErr, "an advisory-lock acquisition or release anomaly must fail GetDiscover")
			require.Empty(t, feed)
			require.Nil(t, cursor)

			observer, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(databaseName))
			require.NoError(t, err)
			observer.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, observer.Close()) })

			for _, lockKey := range []int64{scopeLockKey, globalAdmissionLockKey} {
				var acquired bool
				require.NoError(t, observer.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&acquired))
				require.Truef(t, acquired,
					"a separate session must acquire lock %d after an anomalous lock operation", lockKey)
				var released bool
				require.NoError(t, observer.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey).Scan(&released))
				require.True(t, released)
			}
		})
	}
}
