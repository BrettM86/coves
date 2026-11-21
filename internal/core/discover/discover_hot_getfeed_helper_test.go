//go:build integration

package discover_test

import (
	"database/sql"

	"Coves/internal/core/feedgen"
	"Coves/internal/db/postgres"

	feedhandler "Coves/internal/api/handlers/feed"
)

// discoverPage is one decoded getFeed / getDiscover response page; Cursor is
// nil when the response carried none.
type discoverPage struct {
	Cursor *string                 `json:"cursor,omitempty"`
	Feed   []*feedgen.FeedViewPost `json:"feed"`
}

// discoverHotInstanceDID is the instance DID the getFeed registry serves the
// discover-hot generator under in these tests.
const discoverHotInstanceDID = "did:plc:discoverhotinstance"

// newDiscoverHotFeedHandler serves Discover Hot through getFeed scope=all
// (feed omitted resolves to discover-hot): the real repository as the
// pre-hydrated generator, the real post hydrator and the real cursor signer,
// all over db and signing with cursorSecret.
func newDiscoverHotFeedHandler(db *sql.DB, cursorSecret string) *feedhandler.GetFeedHandler {
	repository := postgres.NewDiscoverRepository(db, cursorSecret)
	service := feedgen.NewService(
		feedgen.NewRegistry(discoverHotInstanceDID, feedgen.DiscoverHotBuiltin(repository.(feedgen.HydratedPostGenerator))),
		postgres.NewPostHydrator(db),
		postgres.NewCursorSigner(cursorSecret),
	)
	return feedhandler.NewGetFeedHandler(service, nil, nil, nil)
}
