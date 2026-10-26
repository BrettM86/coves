package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// LockPostRootLifecyclesTx coordinates comment writes (shared) with community
// deletion (exclusive), before either takes content row locks. Callers acquire
// their erasure lock first. The two-key namespace is separate from erasure's
// single-key locks. Sort actual lock keys so hash collisions cannot invert order.
func LockPostRootLifecyclesTx(ctx context.Context, tx *sql.Tx, rootURIs []string, shared bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT hashtext(root_uri)
        FROM unnest($1::text[]) AS roots(root_uri)
        WHERE root_uri <> '' ORDER BY 1`, pq.Array(rootURIs))
	if err != nil {
		return fmt.Errorf("read post root lifecycle lock keys: %w", err)
	}
	var keys []int32
	for rows.Next() {
		var key int32
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return fmt.Errorf("scan post root lifecycle lock key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate post root lifecycle lock keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close post root lifecycle lock keys: %w", err)
	}
	query := "SELECT pg_advisory_xact_lock(61, $1)"
	if shared {
		query = "SELECT pg_advisory_xact_lock_shared(61, $1)"
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, query, key); err != nil {
			return fmt.Errorf("lock post root lifecycle: %w", err)
		}
	}
	return nil
}
