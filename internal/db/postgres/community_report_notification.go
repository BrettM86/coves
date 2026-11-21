package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/notifications"

	"github.com/lib/pq"
)

// Community report groups (docs/NOTIFICATIONS.md, "Community report groups"):
// one communityReport row per (recipient, community) with subject
// at://<community DID>, bumped by each new report that lands in the
// community's queue and served only while that queue holds a report the
// recipient can act on.

// communityReportGroupCommunitySQL is the community DID of a communityReport
// row (alias n), whose subject is at://<community DID>.
const communityReportGroupCommunitySQL = invitationCommunitySQL

// communityReportGroupAliveSQL is true while the communityReport group of
// recipientExpr on communityExpr is served: the community is hosted here and
// not removed by the instance authorityExpr names, the recipient holds a role
// on its roster (every owner or moderator row is on the effective roster;
// communities.EffectiveRoster only reclassifies losing owner rows) and is not
// suspended (accountRestrictedSQL: in-force instance ban or takendown/suspended
// restriction, the role authorizer's definition), and the queue holds at least
// one community-visible open report the recipient did not file. Every
// expression must be a column reference, bind parameter or fixed SQL fragment.
func communityReportGroupAliveSQL(communityExpr, recipientExpr, authorityExpr string) string {
	return `(NOT ` + communityRemovedSQL(communityExpr, authorityExpr) + `
		AND EXISTS (SELECT 1 FROM community_roles report_role
			WHERE report_role.community_did = ` + communityExpr + `
			  AND report_role.subject_did = ` + recipientExpr + `
			  AND report_role.role IN ('` + communities.RoleOwner + `', '` + communities.RoleModerator + `'))
		AND NOT ` + accountRestrictedSQL(recipientExpr, authorityExpr) + `
		AND EXISTS (SELECT 1 FROM admin_reports group_report
			WHERE group_report.reporter_did <> ` + recipientExpr + `
			  AND ` + communityVisibleReportSQL("group_report", communityExpr) + `))`
}

// listCommunityReportCountsSQL counts each listed community's
// community-visible open reports ($1, community DIDs).
var listCommunityReportCountsSQL = `SELECT listed.did, (
		SELECT COUNT(*) FROM admin_reports counted_report
		WHERE ` + communityVisibleReportSQL("counted_report", "listed.did") + `)
	FROM unnest($1::text[]) AS listed(did)`

// reportedCommunitySQL resolves the community whose queue the report $1 is in:
// the community of the indexed content it reports, when the report is
// community-visible there and the instance ($2) has not removed the community.
// No row means the report is in no community's queue.
var reportedCommunitySQL = `SELECT reported.community_did
	FROM admin_reports r
	CROSS JOIN LATERAL (
		SELECT reported_post.community_did FROM posts reported_post
		WHERE r.target_type = 'post' AND reported_post.uri = r.target_uri
		UNION ALL
		SELECT ` + commentChainCommunitySQL("reported_comment") + ` FROM comments reported_comment
		WHERE r.target_type = 'comment' AND reported_comment.uri = r.target_uri
	) reported(community_did)
	WHERE r.id = $1 AND reported.community_did IS NOT NULL
	  AND ` + communityVisibleReportSQL("r", "reported.community_did") + `
	  AND NOT ` + communityRemovedSQL("reported.community_did", "$2::text") + `
	LIMIT 1`

// bumpCommunityReportGroupsTx bumps the communityReport group of every usable
// roster member of the community whose queue the report reportID landed in,
// except the reporter, in the report's own transaction. A report in no
// community's queue bumps nothing.
func (r *postgresAdminReportRepo) bumpCommunityReportGroupsTx(ctx context.Context, tx *sql.Tx, reportID int64, reporterDID string) error {
	var communityDID string
	err := tx.QueryRowContext(ctx, reportedCommunitySQL, reportID, r.notificationAuthorityDID).Scan(&communityDID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve reported community: %w", err)
	}

	roles, err := listHostedCommunityRoles(ctx, tx, communityDID)
	if err != nil {
		return fmt.Errorf("read reported community roster: %w", err)
	}
	roster := communities.EffectiveRoster(roles)
	var members []string
	if roster.Owner != nil {
		members = append(members, roster.Owner.SubjectDID)
	}
	for _, moderator := range roster.Moderators {
		members = append(members, moderator.SubjectDID)
	}
	if len(members) == 0 {
		return nil
	}
	unsuspended, err := unsuspendedAccountsTx(ctx, tx, members, r.notificationAuthorityDID)
	if err != nil {
		return err
	}
	// Account erasure deletes a recipient's notifications before its users
	// row. Taking each candidate's shared erasure gate, in ascending DID order,
	// before the recipient facts are read and the groups upserted serializes
	// the bump with a concurrent erasure instead of deadlocking it; an erased
	// candidate is dropped.
	candidates := make([]string, 0, len(unsuspended))
	for _, candidate := range ascendingUnique(unsuspended) {
		erased, err := erasureGateTx(ctx, tx, candidate)
		if err != nil {
			return fmt.Errorf("check community report recipient erasure: %w", err)
		}
		if !erased {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	lookups := notificationLookups{tx: tx, indexTime: new(time.Time), authorityDID: r.notificationAuthorityDID}
	intents, err := notifications.FanoutCommunityReport(ctx, lookups, r.notificationBridgeHosts, reporterDID, communityDID, candidates)
	if err != nil {
		return fmt.Errorf("fan out community report notifications: %w", err)
	}
	if len(intents) == 0 {
		return nil
	}
	recipients := make([]string, 0, len(intents))
	subjectURI := intents[0].SubjectURI
	for _, intent := range intents {
		recipients = append(recipients, intent.RecipientDID)
	}
	// The erasure gates keep erasure out, so the users rows are not locked
	// here; the join only skips a recipient with no users row. Rows are
	// upserted in DID order so concurrent reports take the group-row locks in
	// one order.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, sort_at)
		SELECT did, 'communityReport', $2, clock_timestamp()
		FROM users WHERE did = ANY($1)
		ORDER BY did
		ON CONFLICT (recipient_did, subject_uri) WHERE reason = 'communityReport'
		DO UPDATE SET sort_at = GREATEST(notifications.sort_at, clock_timestamp())`,
		pq.Array(recipients), subjectURI); err != nil {
		return fmt.Errorf("upsert community report groups: %w", err)
	}
	return nil
}

// unsuspendedAccountsTx returns the accounts of dids, in order, whose roles
// are not suspended under the instance authorityDID (accountRestrictedSQL).
func unsuspendedAccountsTx(ctx context.Context, tx *sql.Tx, dids []string, authorityDID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT account.did FROM unnest($1::text[]) WITH ORDINALITY AS account(did, position)
		WHERE NOT `+accountRestrictedSQL("account.did", "$2::text")+`
		ORDER BY account.position`, pq.Array(dids), authorityDID)
	if err != nil {
		return nil, fmt.Errorf("read suspended roster members: %w", err)
	}
	defer rows.Close()
	var usable []string
	for rows.Next() {
		var did string
		if err := rows.Scan(&did); err != nil {
			return nil, fmt.Errorf("scan unsuspended roster member: %w", err)
		}
		usable = append(usable, did)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unsuspended roster members: %w", err)
	}
	return usable, nil
}
