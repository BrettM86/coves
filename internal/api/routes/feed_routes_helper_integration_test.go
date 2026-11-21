//go:build integration

package routes_test

import (
	"database/sql"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"

	"github.com/go-chi/chi/v5"
)

// registerFeedAliasRoutes registers getFeed and its aliases (getTimeline,
// getDiscover, communityFeed.getCommunity) on mux as production does: the
// production post built-ins (hot, new, top, discover-hot) over db with the
// given account restriction, the real post hydrator and the cursor signer
// keyed by cursorSecret.
func registerFeedAliasRoutes(
	mux chi.Router,
	db *sql.DB,
	cursorSecret string,
	communityService communities.Service,
	blueskyService blueskypost.Service,
	auth *middleware.OAuthAuthMiddleware,
	authority moderation.Authority,
	instanceDID string,
	options ...postgres.AccountRestrictionOption,
) {
	discoverRepository := postgres.NewDiscoverRepository(db, cursorSecret, options...)
	registry := feedgen.NewRegistry(instanceDID,
		feedgen.HotBuiltin(postgres.NewHotGenerator(db, options...)),
		feedgen.NewBuiltin(postgres.NewNewGenerator(db, options...)),
		feedgen.TopBuiltin(postgres.NewTopGenerator(db, options...)),
		feedgen.DiscoverHotBuiltin(discoverRepository.(feedgen.HydratedPostGenerator)),
	)
	service := feedgen.NewService(registry, postgres.NewPostHydrator(db, options...), postgres.NewCursorSigner(cursorSecret))
	routes.RegisterFeedRoutes(mux, service, registry, communityService, nil, blueskyService, auth, authority, instanceDID)
}
