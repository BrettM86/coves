package postgres

import (
	"context"
	"fmt"
	"strings"

	"Coves/internal/core/posts"
)

type notificationVisibilitySQL struct {
	joins               string
	subjectPostState    string
	recordPostState     string
	rootPostState       string
	recordCommentState  string
	subjectCommentState string
	// referencesVisible holds iff no required reference position reads hidden,
	// including an unsupported collection; it ignores blocks, preferences and
	// the upvote alive rule. visible is this plus those rules.
	referencesVisible string
	visible           string
}

// notificationVisibility builds the ONE read-time notification predicate and
// exposes the reference states for later list and response queries. An active
// admin moderation removal (moderation_decisions kind 'removal'; label decisions
// never count) reads as removedByServerAdmin when any active removal is
// instance-scope and removedByModerator when all are community-scope.
//
// A post whose own admission is removed with a public withdrawal marker (even if
// deleted) is removedByServerAdmin under an active instance removal, else
// removedByModerator. Otherwise an undeleted, publicly admitted post is live, or
// carries its removal status under an active removal; a deleted post with an
// author-delete marker is deleted (the author's delete wins); anything else is
// hidden, so a post that was never public stays hidden even when removed.
// Comments are hidden when unindexed, deleted when deleted, else live or the
// removal status of the active removals on the comment or its root post (a
// comment under an admin-removed post reads as removed). An absent reference position has NULL state; a row is visible
// iff no required reference is hidden. Later chunks extend this same predicate
// rather than copying it.
func notificationVisibility(bridgedTotals bool) notificationVisibilitySQL {
	postCollection := fmt.Sprintf("split_part(%%s, '/', 4) IN ('%s', '%s')", posts.PostV2Collection, posts.LegacyPostCollection)
	postLookup := func(alias, uri string) string {
		admissionJoin, admitted := admittedPostsPredicate(anonymousViewerSQL)
		return fmt.Sprintf(`
		LEFT JOIN LATERAL (
			SELECT CASE
				WHEN a.status = 'removed' AND EXISTS (
					SELECT 1 FROM notification_public_post_withdrawals withdrawal
					WHERE withdrawal.post_uri = p.uri AND withdrawal.kind = 'communityWithdrawal')
					THEN CASE WHEN EXISTS (
						SELECT 1 FROM moderation_decisions d
						WHERE d.subject_uri = p.uri AND d.kind = 'removal' AND d.active AND d.scope_kind = 'instance')
						THEN 'removedByServerAdmin' ELSE 'removedByModerator' END
				WHEN p.deleted_at IS NULL AND %s THEN %s
				WHEN p.deleted_at IS NOT NULL AND EXISTS (
					SELECT 1 FROM notification_public_post_withdrawals withdrawal
					WHERE withdrawal.post_uri = p.uri AND withdrawal.kind = 'authorDelete')
					THEN 'deleted'
				ELSE 'hidden'
			END AS state, p.cid
			FROM posts p %s
			WHERE p.uri = %s AND %s
		) %s ON true`, admitted, moderatedState("p.uri"), admissionJoin, uri, fmt.Sprintf(postCollection, uri), alias)
	}
	postState := func(alias string) string { return "COALESCE(" + alias + ".state, 'hidden')" }
	commentLookup := func(alias, uri string) string {
		return fmt.Sprintf(`
		LEFT JOIN LATERAL (
			SELECT CASE WHEN c.deleted_at IS NOT NULL THEN 'deleted' ELSE %s END AS state, c.cid
			FROM comments c WHERE c.uri = %s
		) %s ON true`, moderatedState("c.uri, c.root_uri"), uri, alias)
	}
	commentState := func(alias string) string { return "COALESCE(" + alias + ".state, 'hidden')" }
	subjectCollection := "split_part(n.subject_uri, '/', 4)"
	recordCollection := "split_part(n.record_uri, '/', 4)"
	visibility := notificationVisibilitySQL{
		joins: postLookup("subject_post", "n.subject_uri") +
			postLookup("record_post", "n.record_uri") +
			postLookup("root_post", "n.root_post_uri") +
			commentLookup("record_comment", "n.record_uri") +
			commentLookup("subject_comment", "n.subject_uri"),
		subjectPostState:    fmt.Sprintf("CASE WHEN n.reason = 'postReply' OR (n.reason = 'upvote' AND "+postCollection+") THEN %s END", "n.subject_uri", postState("subject_post")),
		recordPostState:     fmt.Sprintf("CASE WHEN n.reason = 'mention' AND "+postCollection+" THEN %s END", "n.record_uri", postState("record_post")),
		rootPostState:       postState("root_post"),
		recordCommentState:  fmt.Sprintf("CASE WHEN n.reason IN ('postReply', 'commentReply') OR (n.reason = 'mention' AND %s = 'social.coves.community.comment') THEN %s END", recordCollection, commentState("record_comment")),
		subjectCommentState: fmt.Sprintf("CASE WHEN n.reason = 'commentReply' OR (n.reason = 'upvote' AND %s = 'social.coves.community.comment') THEN %s END", subjectCollection, commentState("subject_comment")),
	}
	// A non-null position with an unsupported collection is also hidden.
	states := []string{visibility.subjectPostState, visibility.recordPostState, visibility.rootPostState,
		visibility.recordCommentState, visibility.subjectCommentState}
	references := make([]string, 0, len(states)+2)
	for _, state := range states {
		references = append(references, "COALESCE(("+state+") <> 'hidden', true)")
	}
	references = append(references,
		fmt.Sprintf("(n.reason <> 'mention' OR %s = 'social.coves.community.comment' OR "+postCollection+")", recordCollection, "n.record_uri"),
		fmt.Sprintf("(n.reason <> 'upvote' OR %s = 'social.coves.community.comment' OR "+postCollection+")", subjectCollection, "n.subject_uri"))
	visibility.referencesVisible = strings.Join(references, " AND ")
	conditions := []string{visibility.referencesVisible,
		`(n.actor_did IS NULL OR NOT EXISTS (
			SELECT 1 FROM user_blocks b WHERE
				(b.blocker_did = n.recipient_did AND b.blocked_did = n.actor_did)
				OR (b.blocker_did = n.actor_did AND b.blocked_did = n.recipient_did)))`,
		"(n.reason <> 'upvote' OR " + upvoteGroupAliveSQL("n.subject_uri", "n.recipient_did", bridgedTotals) + ")",
		`NOT EXISTS (SELECT 1 FROM notification_state ps
			WHERE ps.did = n.recipient_did AND n.reason = ANY(ps.disabled_reasons))`}
	visibility.visible = strings.Join(conditions, " AND ")
	return visibility
}

// moderatedState renders the state of otherwise-live content whose removal is
// decided by any of the comma-separated uriExprs: one scalar aggregate over
// their active removal decisions, so several decisions never multiply rows.
// bool_or over no rows is NULL, which reads as live.
func moderatedState(uriExprs string) string {
	return `(SELECT CASE bool_or(d.scope_kind = 'instance')
				WHEN true THEN 'removedByServerAdmin' WHEN false THEN 'removedByModerator' ELSE 'live' END
			FROM moderation_decisions d
			WHERE d.subject_uri IN (` + uriExprs + `) AND d.kind = 'removal' AND d.active)`
}

// Gate-off statement for existing plan tests; runtime uses r.countUnreadSQL.
var countUnreadNotificationsSQL = buildCountUnreadNotificationsSQL(false)

// buildCountUnreadNotificationsSQL bounds sort_at by seen_at as an index
// condition so the recipient index scan stops at the read boundary instead of
// walking the read history. sort_at is NOT NULL, so a NULL or absent seen_at
// ('-infinity') treats every row as unread; the LIMIT then counts only the
// newest visible row.
func buildCountUnreadNotificationsSQL(bridgedTotals bool) string {
	visibility := notificationVisibility(bridgedTotals)
	return `SELECT COUNT(*) FROM (
		SELECT n.id FROM notifications n` + visibility.joins + `
		WHERE n.recipient_did = $1
			AND n.sort_at > COALESCE((SELECT seen_at FROM notification_state WHERE did = $1), '-infinity'::timestamptz)
			AND ` + visibility.visible + `
		ORDER BY n.sort_at DESC, n.id DESC
		LIMIT CASE WHEN (SELECT seen_at FROM notification_state WHERE did = $1) IS NULL THEN 1 ELSE 101 END
	) visible_unread`
}

// CountUnread counts at most 101 visible unread notifications for the recipient.
func (r *postgresNotificationRepo) CountUnread(ctx context.Context, recipientDID string) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, r.countUnreadSQL, recipientDID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}
