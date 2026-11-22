package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"Coves/internal/core/communitysignals"
)

// nsfwContentWarning is the community content warning whose presence changes
// are recorded as community_nsfw_flag_events.
const nsfwContentWarning = "nsfw"

type postgresCommunitySignalRepo struct {
	db                  *sql.DB
	localInstanceDomain string
}

// NewCommunitySignalRepository builds the repository for community NSFW flag
// events and the weekly digest. localInstanceDomain is this instance's own
// domain: a digest community whose origin is empty or equals it
// (case-insensitively) is reported as native.
func NewCommunitySignalRepository(db *sql.DB, localInstanceDomain string) communitysignals.Repository {
	return &postgresCommunitySignalRepo{db: db, localInstanceDomain: strings.TrimSpace(localInstanceDomain)}
}

// WithLocalInstanceDomain names this instance's own domain: a community whose
// origin is empty or equals it (case-insensitively) records its flag events as
// native. Only the community repository reads it.
func WithLocalInstanceDomain(domain string) AccountRestrictionOption {
	return func(r *accountRestriction) { r.localInstanceDomain = strings.TrimSpace(domain) }
}

// communityOriginHost is the origin host signals report for a community with
// this origin: "" for a native community, whose origin is empty or equals
// localInstanceDomain (case-insensitively).
func communityOriginHost(origin, localInstanceDomain string) string {
	origin = strings.TrimSpace(origin)
	if localInstanceDomain != "" && strings.EqualFold(origin, localInstanceDomain) {
		return ""
	}
	return origin
}

// flagEventOriginHost is the origin_host a flag event records for a community
// with this origin: NULL for a native community.
func (r *postgresCommunityRepo) flagEventOriginHost(origin string) sql.NullString {
	host := communityOriginHost(origin, r.localInstanceDomain)
	if host == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: host, Valid: true}
}

// recordFlagChangeTx inserts a flag event on tx when the nsfw presence of
// oldWarnings and newWarnings differs.
func (r *postgresCommunityRepo) recordFlagChangeTx(ctx context.Context, tx *sql.Tx, communityDID, origin string, oldWarnings, newWarnings []string) error {
	oldNSFW := slices.Contains(oldWarnings, nsfwContentWarning)
	newNSFW := slices.Contains(newWarnings, nsfwContentWarning)
	if oldNSFW == newNSFW {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO community_nsfw_flag_events (community_did, origin_host, old_nsfw, new_nsfw)
		VALUES ($1, $2, $3, $4)`,
		communityDID, r.flagEventOriginHost(origin), oldNSFW, newNSFW); err != nil {
		return fmt.Errorf("failed to record community nsfw flag event: %w", err)
	}
	return nil
}

// claimDueEventQuery claims the oldest due event in one statement: the due
// CTE locks the row it selects (skipping rows another claim holds), and the
// claimed CTE moves its next_attempt_at to the lease expiry so no other claim
// sees it as due. Parameters: $1 now, $2 lease expiry, $3 attempt limit, $4
// community DID (NULL for every community).
const claimDueEventQuery = `
	WITH due AS (
		SELECT e.id
		FROM community_nsfw_flag_events e
		JOIN communities c ON c.did = e.community_did
		WHERE e.delivered_at IS NULL AND e.attempts < $3 AND e.next_attempt_at <= $1
		  AND ($4::text IS NULL OR e.community_did = $4)
		ORDER BY e.next_attempt_at, e.id
		LIMIT 1
		FOR UPDATE OF e SKIP LOCKED
	), claimed AS (
		UPDATE community_nsfw_flag_events e
		SET next_attempt_at = $2
		FROM due
		WHERE e.id = due.id
		RETURNING e.id, e.community_did, e.origin_host, e.old_nsfw, e.new_nsfw,
		          e.occurred_at, e.delivered_at, e.attempts, e.last_error_code, e.next_attempt_at
	)
	SELECT e.id, e.community_did, c.handle, COALESCE(e.origin_host, ''), e.old_nsfw, e.new_nsfw,
	       e.occurred_at, e.delivered_at, e.attempts, COALESCE(e.last_error_code, ''), e.next_attempt_at
	FROM claimed e
	JOIN communities c ON c.did = e.community_did`

func (r *postgresCommunitySignalRepo) ClaimDueEvent(ctx context.Context, communityDID string, now, leaseUntil time.Time, attemptLimit int) (communitysignals.Event, bool, error) {
	var community any
	if communityDID != "" {
		community = communityDID
	}
	var e communitysignals.Event
	var deliveredAt sql.NullTime
	err := r.db.QueryRowContext(ctx, claimDueEventQuery, now, leaseUntil, attemptLimit, community).Scan(
		&e.ID, &e.CommunityDID, &e.CommunityHandle, &e.OriginHost, &e.OldNSFW, &e.NewNSFW,
		&e.OccurredAt, &deliveredAt, &e.Attempts, &e.LastErrorCode, &e.NextAttemptAt)
	if errors.Is(err, sql.ErrNoRows) {
		return communitysignals.Event{}, false, nil
	}
	if err != nil {
		return communitysignals.Event{}, false, fmt.Errorf("failed to claim community nsfw flag event: %w", err)
	}
	e.OccurredAt = e.OccurredAt.UTC()
	e.NextAttemptAt = e.NextAttemptAt.UTC()
	if deliveredAt.Valid {
		at := deliveredAt.Time.UTC()
		e.DeliveredAt = &at
	}
	return e, true, nil
}

// MarkEventDelivered and MarkEventFailed match the claim's lease, so a caller
// whose lease expired and was reclaimed cannot overwrite the newer claim.
func (r *postgresCommunitySignalRepo) MarkEventDelivered(ctx context.Context, id int64, lease, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE community_nsfw_flag_events SET delivered_at = $3
		WHERE id = $1 AND next_attempt_at = $2 AND delivered_at IS NULL`, id, lease, at)
	if err != nil {
		return fmt.Errorf("failed to mark community nsfw flag event delivered: %w", err)
	}
	return leaseHeld(result)
}

func (r *postgresCommunitySignalRepo) MarkEventFailed(ctx context.Context, id int64, lease time.Time, errorCode string, nextAttemptAt time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE community_nsfw_flag_events
		SET attempts = attempts + 1, last_error_code = $3, next_attempt_at = $4
		WHERE id = $1 AND next_attempt_at = $2 AND delivered_at IS NULL`, id, lease, errorCode, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("failed to mark community nsfw flag event failed: %w", err)
	}
	return leaseHeld(result)
}

func leaseHeld(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read community nsfw flag event update count: %w", err)
	}
	if rows == 0 {
		return communitysignals.ErrLeaseLost
	}
	return nil
}

// DigestRows compares each community's share of nsfw self-labelled posts in
// [now-7d, now) with [now-35d, now-7d). A post counts as nsfw when a
// content_labels value has val "nsfw" and neg not true; deleted posts are
// ignored. The threshold comparison and the ordering use exact numeric
// arithmetic, so a rise of exactly the threshold is never counted as above it.
func (r *postgresCommunitySignalRepo) DigestRows(ctx context.Context, now time.Time, minPosts int, riseThresholdPoints float64) ([]communitysignals.DigestRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		WITH counts AS (
			SELECT p.community_did,
			       count(*) FILTER (WHERE p.created_at >= $1::timestamptz - interval '168 hours') AS recent_posts,
			       count(*) FILTER (WHERE p.created_at >= $1::timestamptz - interval '168 hours' AND p.nsfw) AS recent_nsfw,
			       count(*) FILTER (WHERE p.created_at < $1::timestamptz - interval '168 hours') AS prior_posts,
			       count(*) FILTER (WHERE p.created_at < $1::timestamptz - interval '168 hours' AND p.nsfw) AS prior_nsfw
			FROM (
				SELECT community_did, created_at,
				       COALESCE(jsonb_path_exists(content_labels,
				           '$.values[*] ? (@.val == "nsfw" && !(@.neg == true))'), FALSE) AS nsfw
				FROM posts
				WHERE deleted_at IS NULL
				  AND created_at >= $1::timestamptz - interval '840 hours'
				  AND created_at < $1::timestamptz
			) p
			GROUP BY p.community_did
		)
		SELECT k.community_did, c.handle, COALESCE(c.origin, ''),
		       k.recent_posts, k.recent_nsfw, k.prior_posts, k.prior_nsfw
		FROM counts k
		JOIN communities c ON c.did = k.community_did
		WHERE k.recent_posts >= $2
		  AND (k.recent_nsfw::numeric * GREATEST(k.prior_posts, 1) - k.prior_nsfw::numeric * k.recent_posts) * 100
		      > $3::numeric * k.recent_posts * GREATEST(k.prior_posts, 1)
		ORDER BY k.recent_nsfw::numeric / k.recent_posts - k.prior_nsfw::numeric / GREATEST(k.prior_posts, 1) DESC,
		         c.handle`, now, minPosts, riseThresholdPoints)
	if err != nil {
		return nil, fmt.Errorf("failed to query community nsfw digest rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []communitysignals.DigestRow
	for rows.Next() {
		var row communitysignals.DigestRow
		var recentNSFW, priorPosts, priorNSFW int
		if err := rows.Scan(&row.CommunityDID, &row.CommunityHandle, &row.OriginHost,
			&row.RecentPosts, &recentNSFW, &priorPosts, &priorNSFW); err != nil {
			return nil, fmt.Errorf("failed to scan community nsfw digest row: %w", err)
		}
		row.OriginHost = communityOriginHost(row.OriginHost, r.localInstanceDomain)
		row.RecentShare = float64(recentNSFW) / float64(row.RecentPosts)
		if priorPosts > 0 {
			row.PriorShare = float64(priorNSFW) / float64(priorPosts)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating community nsfw digest rows: %w", err)
	}
	return out, nil
}

func (r *postgresCommunitySignalRepo) DigestLastSent(ctx context.Context) (time.Time, bool, error) {
	var slot time.Time
	err := r.db.QueryRowContext(ctx, `SELECT last_slot FROM community_signal_digest_marker`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("failed to read community digest marker: %w", err)
	}
	return slot.UTC(), true, nil
}

func (r *postgresCommunitySignalRepo) SetDigestLastSent(ctx context.Context, slot time.Time) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO community_signal_digest_marker (singleton, last_slot) VALUES (TRUE, $1)
		ON CONFLICT (singleton) DO UPDATE
		SET last_slot = GREATEST(community_signal_digest_marker.last_slot, EXCLUDED.last_slot)`, slot); err != nil {
		return fmt.Errorf("failed to persist community digest marker: %w", err)
	}
	return nil
}
