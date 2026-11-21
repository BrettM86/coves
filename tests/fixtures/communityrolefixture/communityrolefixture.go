//go:build integration

// Package communityrolefixture wires community role authorization the way
// cmd/server does: the Postgres role index and the real role authorizer over
// the Postgres hosted, removal, ban and account-status reads. CreateCommunity
// and UpdateCommunity refuse to run without it. It is a separate package from
// tests/fixtures because it imports internal/db/postgres, whose in-package
// tests import tests/fixtures.
package communityrolefixture

import (
	"database/sql"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
)

// Authorization is communities.WithRoleAuthorization over db's role index and
// Authorizer(db).
func Authorization(db *sql.DB) communities.PDSClientOption {
	return communities.WithRoleAuthorization(postgres.NewCommunityRoleRepository(db), Authorizer(db))
}

// Authorizer is the real role authorizer over db, deciding bans and removals
// made by fixtures.InstanceDID() (the authority accountbanfixture and
// communityremovalfixture act as), with no instance admins.
func Authorizer(db *sql.DB) communities.RoleAuthorizer {
	return communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:        postgres.NewHostedCommunityQuery(db),
		Removals:      postgres.NewCommunityRemovalReader(db, fixtures.InstanceDID()),
		Roles:         postgres.NewCommunityRoleRepository(db),
		AccountBans:   postgres.NewAccountBanReader(db, fixtures.InstanceDID()),
		AccountStatus: postgres.NewAccountStatusRepository(db),
		Admins:        moderation.NewAllowlistAuthority(nil),
		Now:           time.Now,
	})
}
