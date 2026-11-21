package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"Coves/internal/core/notifications"

	"github.com/lib/pq"
)

const maximumNotificationListLimit = 100

// listNotificationsSQL has five parameters: recipient DID ($1), optional cursor
// sort time ($2) and ID ($3), probe-inclusive row limit ($4), and the
// account-ban authority DID ($5).
// The combined CASE prevents the planner from multiplying selectivity guesses
// for each dependent reference state and choosing an unordered index plus Sort.
// With NULL seen_at, only the recipient's newest visible row of any reason is unread.
// Gate-off statement for existing plan tests; runtime uses r.listSQL.
var listNotificationsSQL = buildListNotificationsSQL(false)

// buildListNotificationUpvotesSQL builds one statement per page: recipient ($1),
// listed upvote subject URIs ($2).
func buildListNotificationUpvotesSQL(bridgedTotals bool) string {
	return `SELECT s.subject_uri, aggregates.vote_count, aggregates.recent_voters
	FROM unnest($2::text[]) AS s(subject_uri)
	CROSS JOIN LATERAL (
		SELECT COUNT(*) + ` + bridgedUpvoteTotalSQL("s.subject_uri", bridgedTotals) + ` AS vote_count,
			COALESCE(array_agg(ranked.voter_did ORDER BY ranked.indexed_at DESC, ranked.id DESC)
				FILTER (WHERE ranked.position <= 3), ARRAY[]::text[]) AS recent_voters
		FROM (
			SELECT v.voter_did, v.indexed_at, v.id,
				ROW_NUMBER() OVER (ORDER BY v.indexed_at DESC, v.id DESC) AS position
			FROM votes v WHERE ` + qualifyingUpvoteSQL("v", "s.subject_uri", "$1") + `
		) ranked
	) aggregates`
}

func buildListNotificationsSQL(bridgedTotals bool) string {
	visibility := notificationVisibility(bridgedTotals, "$5::text")
	return `SELECT n.id, n.reason, n.record_uri, n.actor_did, n.subject_uri,
		n.root_post_uri, n.record_created_at, n.sort_at,
		CASE WHEN state.seen_at IS NOT NULL THEN n.sort_at <= state.seen_at
		ELSE n.id <> (
			SELECT n.id FROM notifications n` + visibility.joins + `
			WHERE n.recipient_did = $1::text AND ` + visibility.visible + `
			ORDER BY n.sort_at DESC, n.id DESC LIMIT 1
		) END AS is_read,
		` + visibility.rootPostState + ` AS root_state, root_post.cid,
		COALESCE(` + visibility.subjectPostState + `, ` + visibility.subjectCommentState + `) AS subject_state,
		COALESCE(subject_post.cid, subject_comment.cid) AS subject_cid,
		COALESCE(` + visibility.recordPostState + `, ` + visibility.recordCommentState + `) AS record_state,
		COALESCE(record_post.cid, record_comment.cid) AS record_cid
	FROM notifications n` + visibility.joins + `
	LEFT JOIN notification_state state ON state.did = n.recipient_did
	WHERE n.recipient_did = $1::text
		AND ($2::timestamptz IS NULL OR (n.sort_at, n.id) < ($2::timestamptz, $3::bigint))
		AND CASE WHEN (` + visibility.visible + `)
			THEN true ELSE false END
	ORDER BY n.sort_at DESC, n.id DESC
	LIMIT $4::int`
}

var _ notifications.ReadRepository = (*postgresNotificationRepo)(nil)

// List pages all visible notifications with the five parameters of listNotificationsSQL.
// With NULL seen_at, only the newest visible row of any reason is
// unread. A repeatable-read snapshot keeps SeenAt and row read states aligned.
func (r *postgresNotificationRepo) List(ctx context.Context, recipientDID, cursor string, limit int) (notifications.ListPage, error) {
	if limit <= 0 || limit > maximumNotificationListLimit {
		return notifications.ListPage{}, fmt.Errorf("list notifications: invalid limit %d", limit)
	}
	var sortAt any
	var id any
	if cursor != "" {
		position, err := notifications.DecodeCursor(cursor)
		if err != nil {
			return notifications.ListPage{}, fmt.Errorf("list notifications: %w", err)
		}
		sortAt, id = position.SortAt, position.ID
	}

	page := notifications.ListPage{}
	transaction, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return page, fmt.Errorf("begin notification list snapshot: %w", err)
	}
	defer transaction.Rollback()
	var seenAt sql.NullTime
	if err := transaction.QueryRowContext(ctx, `SELECT seen_at FROM notification_state WHERE did = $1`, recipientDID).Scan(&seenAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return page, fmt.Errorf("read notification seen time: %w", err)
	}
	if seenAt.Valid {
		seenTime := seenAt.Time.UTC()
		page.SeenAt = &seenTime
	}

	rows, err := transaction.QueryContext(ctx, r.listSQL, recipientDID, sortAt, id, limit+1, r.accountBanAuthorityDID)
	if err != nil {
		return notifications.ListPage{}, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var notification notifications.ListedNotification
		var recordURI, actorDID, subjectURI, rootPostURI sql.NullString
		var rootState, rootCID, subjectState, subjectCID, recordState, recordCID sql.NullString
		var recordCreatedAt sql.NullTime
		if err := rows.Scan(&notification.ID, &notification.Reason, &recordURI, &actorDID,
			&subjectURI, &rootPostURI, &recordCreatedAt, &notification.SortAt, &notification.IsRead,
			&rootState, &rootCID, &subjectState, &subjectCID, &recordState, &recordCID); err != nil {
			return notifications.ListPage{}, fmt.Errorf("scan listed notification: %w", err)
		}
		notification.ActorDID = actorDID.String
		notification.SubjectURI = subjectURI.String
		notification.SortAt = notification.SortAt.UTC()
		if notification.Reason == notifications.ReasonCommunityInvitation || notification.Reason == notifications.ReasonCommunityReport {
			// It references no post or comment, only its community, read
			// below.
			page.Notifications = append(page.Notifications, notification)
			continue
		}
		notification.RootPost, err = listedReference(rootState, rootCID)
		if err != nil {
			return notifications.ListPage{}, fmt.Errorf("listed notification root post: %w", err)
		}
		if notification.Reason != notifications.ReasonUpvote {
			notification.Record, err = listedReference(recordState, recordCID)
			if err != nil {
				return notifications.ListPage{}, fmt.Errorf("listed notification record: %w", err)
			}
		}
		if notification.Reason != notifications.ReasonMention {
			notification.Subject, err = listedReference(subjectState, subjectCID)
			if err != nil {
				return notifications.ListPage{}, fmt.Errorf("listed notification subject: %w", err)
			}
		}
		notification.RecordURI = recordURI.String
		notification.RootPostURI = rootPostURI.String
		if recordCreatedAt.Valid {
			notification.RecordCreatedAt = recordCreatedAt.Time.UTC()
		}
		page.Notifications = append(page.Notifications, notification)
	}
	if err := rows.Err(); err != nil {
		return notifications.ListPage{}, fmt.Errorf("iterate listed notifications: %w", err)
	}
	if err := rows.Close(); err != nil {
		return notifications.ListPage{}, fmt.Errorf("close listed notifications: %w", err)
	}
	if len(page.Notifications) > limit {
		page.Notifications = page.Notifications[:limit]
		last := page.Notifications[limit-1]
		page.Cursor = notifications.EncodeCursor(notifications.Cursor{SortAt: last.SortAt, ID: last.ID})
	}
	if err := listNotificationCommunities(ctx, transaction, page.Notifications); err != nil {
		return notifications.ListPage{}, err
	}
	if err := listCommunityReportCounts(ctx, transaction, page.Notifications); err != nil {
		return notifications.ListPage{}, err
	}
	var subjects []string
	indices := make(map[string]int)
	for index, notification := range page.Notifications {
		if notification.Reason == notifications.ReasonUpvote {
			subjects = append(subjects, notification.SubjectURI)
			indices[notification.SubjectURI] = index
		}
	}
	if len(subjects) > 0 {
		upvotes, err := transaction.QueryContext(ctx, r.listUpvotesSQL, recipientDID, pq.Array(subjects))
		if err != nil {
			return notifications.ListPage{}, fmt.Errorf("list notification upvotes: %w", err)
		}
		defer upvotes.Close()
		for upvotes.Next() {
			var subject string
			var count int
			var voters pq.StringArray
			if err := upvotes.Scan(&subject, &count, &voters); err != nil {
				return notifications.ListPage{}, fmt.Errorf("scan notification upvotes: %w", err)
			}
			row := &page.Notifications[indices[subject]]
			row.UpvoteCount = count
			row.RecentUpvoterDIDs = voters
		}
		if err := upvotes.Err(); err != nil {
			return notifications.ListPage{}, fmt.Errorf("iterate notification upvotes: %w", err)
		}
	}
	return page, nil
}

// namesCommunity reports whether a notification of reason has subject
// at://<community DID> and serves that community.
func namesCommunity(reason notifications.Reason) bool {
	return reason == notifications.ReasonCommunityInvitation || reason == notifications.ReasonCommunityReport
}

// listNotificationCommunities sets the community of each communityInvitation
// and communityReport in listed, read in the list's snapshot. The visibility
// predicate admitted only rows whose community is indexed.
func listNotificationCommunities(ctx context.Context, transaction *sql.Tx, listed []notifications.ListedNotification) error {
	var dids []string
	for _, notification := range listed {
		if namesCommunity(notification.Reason) {
			dids = append(dids, strings.TrimPrefix(notification.SubjectURI, "at://"))
		}
	}
	if len(dids) == 0 {
		return nil
	}
	rows, err := transaction.QueryContext(ctx, `SELECT did, handle, name FROM communities WHERE did = ANY($1)`, pq.Array(dids))
	if err != nil {
		return fmt.Errorf("list invitation notification communities: %w", err)
	}
	defer rows.Close()
	found := make(map[string]notifications.ListedCommunity, len(dids))
	for rows.Next() {
		var community notifications.ListedCommunity
		if err := rows.Scan(&community.DID, &community.Handle, &community.Name); err != nil {
			return fmt.Errorf("scan invitation notification community: %w", err)
		}
		found[community.DID] = community
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate invitation notification communities: %w", err)
	}
	for index := range listed {
		notification := &listed[index]
		if !namesCommunity(notification.Reason) {
			continue
		}
		community, ok := found[strings.TrimPrefix(notification.SubjectURI, "at://")]
		if !ok {
			return fmt.Errorf("listed %s notification %d: community %s is not indexed", notification.Reason, notification.ID, notification.SubjectURI)
		}
		notification.Community = &community
	}
	return nil
}

// listCommunityReportCounts sets the ReportCount of each communityReport in
// listed: its community's community-visible open report count, read in the
// list's snapshot. The visibility predicate admitted only groups whose queue
// holds a report.
func listCommunityReportCounts(ctx context.Context, transaction *sql.Tx, listed []notifications.ListedNotification) error {
	var dids []string
	for _, notification := range listed {
		if notification.Reason == notifications.ReasonCommunityReport {
			dids = append(dids, strings.TrimPrefix(notification.SubjectURI, "at://"))
		}
	}
	if len(dids) == 0 {
		return nil
	}
	rows, err := transaction.QueryContext(ctx, listCommunityReportCountsSQL, pq.Array(dids))
	if err != nil {
		return fmt.Errorf("count community report notification reports: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int, len(dids))
	for rows.Next() {
		var did string
		var count int
		if err := rows.Scan(&did, &count); err != nil {
			return fmt.Errorf("scan community report notification count: %w", err)
		}
		counts[did] = count
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate community report notification counts: %w", err)
	}
	for index := range listed {
		notification := &listed[index]
		if notification.Reason == notifications.ReasonCommunityReport {
			notification.ReportCount = counts[strings.TrimPrefix(notification.SubjectURI, "at://")]
		}
	}
	return nil
}

func listedReference(state, cid sql.NullString) (notifications.ListedReference, error) {
	if !state.Valid || !cid.Valid {
		return notifications.ListedReference{}, fmt.Errorf("missing reference state or CID")
	}
	reference := notifications.ListedReference{CID: cid.String}
	switch state.String {
	case "live":
		reference.State = notifications.ReferenceLive
	case "deleted":
		reference.State = notifications.ReferenceDeleted
	case "removedByModerator":
		reference.State = notifications.ReferenceRemovedByModerator
	case "removedByServerAdmin":
		reference.State = notifications.ReferenceRemovedByServerAdmin
	default:
		return notifications.ListedReference{}, fmt.Errorf("unexpected reference state %q", state.String)
	}
	return reference, nil
}
