package postgres

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/notifications"
)

// UpdateSeen advances the recipient's seen time without changing preferences.
// seenAt is sent in UTC: datetime syntax allows zone offsets to ±23:59, but
// Postgres rejects offsets beyond ±15:59.
func (r *postgresNotificationRepo) UpdateSeen(ctx context.Context, did string, seenAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO notification_state (did, seen_at)
		VALUES ($1, LEAST($2::timestamptz, NOW()))
		ON CONFLICT (did) DO UPDATE SET seen_at = GREATEST(notification_state.seen_at, LEAST(EXCLUDED.seen_at, NOW()))`, did, seenAt.UTC())
	if isStateAccountForeignKeyViolation(err) {
		return notifications.ErrAccountNotIndexed
	}
	if err != nil {
		return fmt.Errorf("update notification seen time: %w", err)
	}
	return nil
}
