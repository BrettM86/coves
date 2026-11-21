//go:build integration

// Package commentfeedfixture builds the feed registry getComments resolves its
// comment generators from: the three built-in comment generators over a real
// database, as production wires them. Integration tests that read threads
// through a real comment service install it with comments.WithFeedRegistry.
package commentfeedfixture

import (
	"database/sql"

	"Coves/internal/core/feedgen"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
)

// Registry returns a registry of comments-hot, comments-top and comments-new
// over db, under the fixture instance DID.
func Registry(db *sql.DB) *feedgen.Registry {
	return feedgen.NewRegistry(fixtures.InstanceDID(),
		feedgen.CommentsHotBuiltin(postgres.NewCommentsHotGenerator(db)),
		feedgen.CommentsTopBuiltin(postgres.NewCommentsTopGenerator(db)),
		feedgen.CommentsNewBuiltin(postgres.NewCommentsNewGenerator(db)),
	)
}
