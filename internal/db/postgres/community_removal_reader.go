package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/core/communities"
)

// communityRemovedSQL is THE community-removal read: a boolean SQL expression
// that is true while the community whose DID didExpr evaluates to has an
// active instance removal decision on at://<did> whose action records the
// community subject kind (the action RemoveCommunity writes), made by the
// instance whose DID authorityExpr evaluates to. An empty or NULL authority
// matches no decision, so a repository without
// WithAccountRestrictionAuthority reads no community as removed. An account
// ban records the account kind, so it never reads as a community removal, even
// on a DID indexed as a community afterwards. didExpr and
// authorityExpr must be column references, bind parameters or fixed SQL
// fragments (commentChainCommunitySQL), never caller-supplied values.
func communityRemovedSQL(didExpr, authorityExpr string) string {
	return `EXISTS (
				SELECT 1 FROM moderation_decisions community_removal_decision
				JOIN moderation_actions community_removal_action ON community_removal_action.id = community_removal_decision.active_action_id
				WHERE community_removal_decision.subject_uri = 'at://' || ` + didExpr + `
					AND community_removal_decision.authority_did = ` + authorityExpr + `
					AND community_removal_decision.scope_kind = 'instance'
					AND community_removal_decision.kind = 'removal'
					AND community_removal_decision.active
					AND community_removal_action.subject_kind = 'community'
			)`
}

// communityStatus is the Community.Status of a community read with its
// communityRemovedSQL result.
func communityStatus(removed bool) string {
	if removed {
		return communities.CommunityStatusRemoved
	}
	return ""
}

type communityRemovalReader struct {
	db          *sql.DB
	instanceDID string
}

// NewCommunityRemovalReader returns a reader of active community removals
// made by instanceDID.
func NewCommunityRemovalReader(db *sql.DB, instanceDID string) communities.RemovalReader {
	return communityRemovalReader{db: db, instanceDID: instanceDID}
}

// CommunityRemoved reports whether the community is under an active instance
// removal.
func (r communityRemovalReader) CommunityRemoved(ctx context.Context, communityDID string) (bool, error) {
	var removed bool
	if err := r.db.QueryRowContext(ctx, `SELECT `+communityRemovedSQL("$1::text", "$2::text"),
		communityDID, r.instanceDID).Scan(&removed); err != nil {
		return false, fmt.Errorf("reading community removal: %w", err)
	}
	return removed, nil
}
