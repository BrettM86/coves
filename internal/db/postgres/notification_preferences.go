package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/core/notifications"

	"github.com/lib/pq"
)

var _ notifications.PreferencesRepository = (*postgresNotificationRepo)(nil)

const preferenceColumnsSQL = `NOT COALESCE('postReply' = ANY(disabled_reasons), false),
	NOT COALESCE('commentReply' = ANY(disabled_reasons), false),
	NOT COALESCE('mention' = ANY(disabled_reasons), false),
	NOT COALESCE('upvote' = ANY(disabled_reasons), false)`

// GetPreferences defaults to every reason enabled when the recipient has no state.
func (r *postgresNotificationRepo) GetPreferences(ctx context.Context, did string) (notifications.Preferences, error) {
	var preferences notifications.Preferences
	err := r.db.QueryRowContext(ctx, `SELECT `+preferenceColumnsSQL+` FROM notification_state WHERE did = $1`, did).
		Scan(&preferences.PostReply, &preferences.CommentReply, &preferences.Mention, &preferences.Upvote)
	if errors.Is(err, sql.ErrNoRows) {
		return notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}, nil
	}
	if err != nil {
		return notifications.Preferences{}, fmt.Errorf("get notification preferences: %w", err)
	}
	return preferences, nil
}

// PutPreferences changes only the named reasons without advancing seen_at.
func (r *postgresNotificationRepo) PutPreferences(ctx context.Context, did string, update notifications.PreferencesUpdate) (notifications.Preferences, error) {
	additions := []string{}
	removals := []string{}
	for _, change := range []struct {
		reason  string
		enabled *bool
	}{
		{"postReply", update.PostReply},
		{"commentReply", update.CommentReply},
		{"mention", update.Mention},
		{"upvote", update.Upvote},
	} {
		if change.enabled == nil {
			continue
		}
		if *change.enabled {
			removals = append(removals, change.reason)
		} else {
			additions = append(additions, change.reason)
		}
	}

	var preferences notifications.Preferences
	err := r.db.QueryRowContext(ctx, `INSERT INTO notification_state (did, disabled_reasons)
		VALUES ($1, $2::text[])
		ON CONFLICT (did) DO UPDATE SET disabled_reasons = ARRAY(
			SELECT DISTINCT reason FROM unnest(notification_state.disabled_reasons || EXCLUDED.disabled_reasons) AS reason
			WHERE reason IS NOT NULL AND reason <> ALL($3::text[]))
		RETURNING `+preferenceColumnsSQL, did, pq.Array(additions), pq.Array(removals)).
		Scan(&preferences.PostReply, &preferences.CommentReply, &preferences.Mention, &preferences.Upvote)
	if isStateAccountForeignKeyViolation(err) {
		return notifications.Preferences{}, notifications.ErrAccountNotIndexed
	}
	if err != nil {
		return notifications.Preferences{}, fmt.Errorf("put notification preferences: %w", err)
	}
	return preferences, nil
}

// isStateAccountForeignKeyViolation reports whether err is the foreign-key
// violation raised when notification_state's did has no users row.
func isStateAccountForeignKeyViolation(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && databaseError.Code == "23503" &&
		databaseError.Constraint == "notification_state_did_fkey"
}
