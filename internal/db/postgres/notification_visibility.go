package postgres

import (
	"context"
	"fmt"
	"strings"

	"Coves/internal/core/posts"
)

// commentHideReasonPendingBanCheck is the comments.hide_reason code (migration
// 069) of a comment whose community, and so its ban check, is not known yet.
const commentHideReasonPendingBanCheck = "pending_ban_check"

// commentHideReasonAccountBan is the comments.hide_reason code (migration 073)
// of a comment written while its author's account was banned.
const commentHideReasonAccountBan = "account_ban"

// commentHideReasonCommunityBan is the comments.hide_reason code of a comment
// written while its author was banned from the comment's community.
const commentHideReasonCommunityBan = "community_ban"

// notificationRecipientSQL is the recipient DID of the notification row (alias
// n) every visibility predicate is evaluated against.
const notificationRecipientSQL = "n.recipient_did"

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
// deleted) is removedByServerAdmin under an active instance removal of the post
// or (unless the recipient wrote the post) of its community, else
// removedByModerator. Otherwise an undeleted, publicly admitted post is live, or
// carries its removal status under an active removal; a deleted post with an
// author-delete marker is deleted (the author's delete wins); anything else is
// hidden, so a post that was never public stays hidden even when removed.
// Comments are hidden when unindexed, deleted when deleted, else live or the
// removal status of the active removals on the comment or its root post (a
// comment under an admin-removed post reads as removed). A comment recorded
// hidden at index time by a community ban reads as removedByModerator (or an
// admin removal's status); one recorded hidden by an account ban reads as
// removedByServerAdmin, also after the ban ends, except to its author as the
// notification's recipient, who sees it like an unhidden comment; one whose
// ban check is still pending is hidden.
// Undeleted, otherwise-live content whose author's account is restricted (an
// in-force instance account ban) reads as removedByServerAdmin, and live again
// once the restriction ends; authorityExpr is the statement's bind parameter for
// the account-ban authority DID (accountRestrictedSQL). Undeleted,
// otherwise-live content in a community under an active instance removal by
// that authority (a comment by the community its parent chain reaches,
// commentChainCommunitySQL) reads as
// removedByServerAdmin until the community is restored. Both of those reads
// exempt a reference the notification's recipient authored, so an account sees
// its own content as before; direct removal decisions on that content (or on a
// comment's root post) still apply. An absent reference
// position has NULL state; a row is visible iff no required reference is
// hidden. Later chunks extend this same predicate rather than copying it.
func notificationVisibility(bridgedTotals bool, authorityExpr string) notificationVisibilitySQL {
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
						OR (p.author_did IS DISTINCT FROM %[8]s AND %[7]s)
						THEN 'removedByServerAdmin' ELSE 'removedByModerator' END
				WHEN p.deleted_at IS NULL AND %[1]s THEN %[2]s
				WHEN p.deleted_at IS NOT NULL AND EXISTS (
					SELECT 1 FROM notification_public_post_withdrawals withdrawal
					WHERE withdrawal.post_uri = p.uri AND withdrawal.kind = 'authorDelete')
					THEN 'deleted'
				ELSE 'hidden'
			END AS state, p.cid
			FROM posts p %[3]s
			WHERE p.uri = %[4]s AND %[5]s
		) %[6]s ON true`, admitted, moderatedState("p.uri", "p.community_did", "p.author_did", authorityExpr), admissionJoin, uri,
			fmt.Sprintf(postCollection, uri), alias, communityRemovedSQL("p.community_did", authorityExpr), notificationRecipientSQL)
	}
	postState := func(alias string) string { return "COALESCE(" + alias + ".state, 'hidden')" }
	commentLookup := func(alias, uri string) string {
		return fmt.Sprintf(`
		LEFT JOIN LATERAL (
			SELECT CASE
				WHEN c.deleted_at IS NOT NULL THEN 'deleted'
				WHEN c.hide_reason = '%[4]s' THEN 'hidden'
				WHEN c.hide_reason = '%[5]s' AND c.commenter_did IS DISTINCT FROM %[6]s THEN 'removedByServerAdmin'
				WHEN c.hide_reason IS NOT NULL AND c.hide_reason <> '%[5]s' THEN COALESCE(NULLIF(%[1]s, 'live'), 'removedByModerator')
				ELSE %[1]s
			END AS state, c.cid
			FROM comments c WHERE c.uri = %[2]s
		) %[3]s ON true`, moderatedState("c.uri, c.root_uri", commentChainCommunitySQL("c"), "c.commenter_did", authorityExpr), uri, alias, commentHideReasonPendingBanCheck, commentHideReasonAccountBan, notificationRecipientSQL)
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
	states := []string{
		visibility.subjectPostState, visibility.recordPostState, visibility.rootPostState,
		visibility.recordCommentState, visibility.subjectCommentState,
	}
	references := make([]string, 0, len(states)+2)
	for _, state := range states {
		references = append(references, "COALESCE(("+state+") <> 'hidden', true)")
	}
	references = append(references,
		fmt.Sprintf("(n.reason <> 'mention' OR %s = 'social.coves.community.comment' OR "+postCollection+")", recordCollection, "n.record_uri"),
		fmt.Sprintf("(n.reason <> 'upvote' OR %s = 'social.coves.community.comment' OR "+postCollection+")", subjectCollection, "n.subject_uri"))
	visibility.referencesVisible = strings.Join(references, " AND ")
	conditions := []string{
		visibility.referencesVisible,
		`(n.actor_did IS NULL OR NOT EXISTS (
			SELECT 1 FROM user_blocks b WHERE
				(b.blocker_did = n.recipient_did AND b.blocked_did = n.actor_did)
				OR (b.blocker_did = n.actor_did AND b.blocked_did = n.recipient_did)))`,
		"(n.reason <> 'upvote' OR " + upvoteGroupAliveSQL("n.subject_uri", "n.recipient_did", bridgedTotals) + ")",
		`NOT EXISTS (SELECT 1 FROM notification_state ps
			WHERE ps.did = n.recipient_did AND n.reason = ANY(ps.disabled_reasons))`,
	}
	visibility.visible = strings.Join(conditions, " AND ")
	return visibility
}

// moderatedState renders the state of otherwise-live content authored by
// authorExpr in the community communityExpr names, whose removal is decided by
// any of the comma-separated uriExprs. Content of a restricted author
// (accountRestrictedSQL), or in a community under an active instance removal
// (communityRemovedSQL), reads as removedByServerAdmin while the restriction
// or removal lasts, except to the notification's recipient when authorExpr is
// that recipient: an account sees its own content. Otherwise it is one scalar
// aggregate over the active removal decisions, so several decisions never
// multiply rows; bool_or over no rows is NULL, which reads as live. The
// recipient's own content in a removed community deliberately reads as live
// here, while the author's own post views (authorPostViewSelectColumns) mark it
// admission status "removed" without the community avatar.
func moderatedState(uriExprs, communityExpr, authorExpr, authorityExpr string) string {
	return `(CASE WHEN ` + authorExpr + ` IS DISTINCT FROM ` + notificationRecipientSQL + ` AND (` +
		accountRestrictedSQL(authorExpr, authorityExpr) + ` OR ` +
		communityRemovedSQL(communityExpr, authorityExpr) + `) THEN 'removedByServerAdmin' ELSE (SELECT CASE bool_or(d.scope_kind = 'instance')
				WHEN true THEN 'removedByServerAdmin' WHEN false THEN 'removedByModerator' ELSE 'live' END
			FROM moderation_decisions d
			WHERE d.subject_uri IN (` + uriExprs + `) AND d.kind = 'removal' AND d.active) END)`
}

// Gate-off statement for existing plan tests; runtime uses r.countUnreadSQL.
// Its parameters are the recipient DID ($1) and the account-ban authority DID ($2).
var countUnreadNotificationsSQL = buildCountUnreadNotificationsSQL(false)

// buildCountUnreadNotificationsSQL bounds sort_at by seen_at as an index
// condition so the recipient index scan stops at the read boundary instead of
// walking the read history. sort_at is NOT NULL, so a NULL or absent seen_at
// ('-infinity') treats every row as unread; the LIMIT then counts only the
// newest visible row.
func buildCountUnreadNotificationsSQL(bridgedTotals bool) string {
	visibility := notificationVisibility(bridgedTotals, "$2::text")
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
	if err := r.db.QueryRowContext(ctx, r.countUnreadSQL, recipientDID, r.accountBanAuthorityDID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}
