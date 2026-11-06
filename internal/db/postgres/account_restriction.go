package postgres

import "Coves/internal/core/accountbans"

// accountRestrictedSQL is THE account-restriction read: a boolean SQL
// expression that is true while the account whose DID didExpr evaluates to is
// restricted on this instance, so that its posts, comments and profile are
// hidden from every other viewer. Every content read path that hides a
// restricted account's content (posts, comments, notifications) goes
// through this one function; a further account-level restriction
// source is OR'd in here and nowhere else.
//
// The sources are an in-force instance account ban
// (accountbans.InForceSQL) made by the instance whose DID authorityExpr
// evaluates to, on the database clock, so a temporary ban stops restricting
// at its expiresAt with no action and another instance's removal of the
// account restricts nothing. Index-time hides of comments and votes use the
// same condition on the consumer clock. The second source is the account's
// PDS-confirmed takendown or suspended status (account_status_restrictions,
// kept by the users consumer from #account events and by the account status
// recheck worker); it is not an instance decision, but it is applied only under the same configured authority so a
// zero-value accountRestriction still restricts nothing.
//
// The restriction is a read-time hide: it changes no stored row, so unbanning
// or expiry restores exactly what it hid, and per-item removals stay
// independent of it.
//
// didExpr and authorityExpr must be column references or bind parameters,
// never caller-supplied values; a repository binds authorityExpr to its
// accountRestriction.authorityDID.
func accountRestrictedSQL(didExpr, authorityExpr string) string {
	return `(` + accountbans.InForceSQL(didExpr, "NOW()", authorityExpr) + `
			OR (COALESCE(` + authorityExpr + `, '') <> ''
				AND EXISTS (SELECT 1 FROM account_status_restrictions WHERE did = ` + didExpr + `)))`
}

// visibleToViewerUnlessRestrictedSQL is true when the content authored by
// authorExpr is not hidden from viewerExpr by an account restriction: the
// author always sees its own content, every other viewer (including the
// anonymous "") only while the author is unrestricted.
func visibleToViewerUnlessRestrictedSQL(authorExpr, viewerExpr, authorityExpr string) string {
	return `(` + authorExpr + ` = ` + viewerExpr + ` OR NOT ` + accountRestrictedSQL(authorExpr, authorityExpr) + `)`
}

// accountRestriction is the instance whose in-force account bans a content
// read repository applies (accountRestrictedSQL). Its zero value names no
// instance and restricts no account, so production wiring must set it with
// WithAccountRestrictionAuthority.
type accountRestriction struct {
	authorityDID string
}

// AccountRestrictionOption configures the account-ban authority of a content
// read repository (posts, comments, community, timeline and discover feeds,
// communities).
type AccountRestrictionOption func(*accountRestriction)

// WithAccountRestrictionAuthority makes the repository hide the content of
// accounts under an in-force account ban made by instanceDID.
func WithAccountRestrictionAuthority(instanceDID string) AccountRestrictionOption {
	return func(r *accountRestriction) { r.authorityDID = instanceDID }
}

func newAccountRestriction(options []AccountRestrictionOption) accountRestriction {
	var restriction accountRestriction
	for _, option := range options {
		option(&restriction)
	}
	return restriction
}
