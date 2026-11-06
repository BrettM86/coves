// Package accountbans reads instance account bans for the write paths that
// refuse a banned account (posts, comments, votes).
package accountbans

import (
	"context"
	"time"
)

// Ban is an account's active instance ban decision.
type Ban struct {
	ActionID  string
	Reason    string
	ExpiresAt *time.Time
}

// Reader returns the account's active ban decision, or nil when there is none.
type Reader interface {
	ActiveBan(ctx context.Context, accountDID string) (*Ban, error)
}

// InForce reports whether the ban applies at now: there is an active ban and
// it has no expiry or expires after now. now is the server's clock, never a
// client's.
func (b *Ban) InForce(now time.Time) bool {
	return b != nil && (b.ExpiresAt == nil || b.ExpiresAt.After(now))
}

// InForceSQL is Ban.InForce as a SQL condition: true while the account whose
// DID didExpr evaluates to has an active instance removal decision, made by the
// instance whose DID authorityExpr evaluates to, on its account subject
// at://<did> whose active action records the account subject kind (the
// action BanAccount writes) and has no expiry or expires after atExpr. Another
// authority's instance-scope removal of the account is not this instance's
// ban, as in Reader.ActiveBan. A community removal (RemoveCommunity) records
// the community kind, so it is never an account ban, and an account ban stays
// one when its DID is indexed as a community afterwards. didExpr, atExpr and
// authorityExpr must be column references, bind parameters or SQL written by
// the caller, never caller-supplied values.
func InForceSQL(didExpr, atExpr, authorityExpr string) string {
	return `EXISTS (
				SELECT 1 FROM moderation_decisions restriction_decision
				JOIN moderation_actions restriction_action ON restriction_action.id = restriction_decision.active_action_id
				WHERE restriction_decision.subject_uri = 'at://' || ` + didExpr + `
					AND restriction_decision.authority_did = ` + authorityExpr + `
					AND restriction_decision.scope_kind = 'instance'
					AND restriction_decision.kind = 'removal'
					AND restriction_decision.active
					AND restriction_action.subject_kind = 'account'
					AND (restriction_action.expires_at IS NULL OR restriction_action.expires_at > ` + atExpr + `)
			)`
}
