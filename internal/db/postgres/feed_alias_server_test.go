//go:build integration

package postgres

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/votes"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// newFeedRouter is the one router every getFeed and alias suite serves
// through: getFeed and its three aliases (getTimeline, getCommunity,
// getDiscover) registered by RegisterFeedRoutes over feeds, with a real
// community service for community resolution, voteService for viewer vote
// state, auth for bearer tokens and the allowlist moderation authority.
func newFeedRouter(db *sql.DB, feeds *builtinFeeds, voteService votes.Service, auth *fixtures.OAuthMiddleware, restriction AccountRestrictionOption) http.Handler {
	instanceDID := fixtures.InstanceDID()
	communityService := communities.NewCommunityServiceWithPDSFactory(
		NewCommunityRepository(db, credentialciphertest.Fixed(), restriction),
		testkit.Endpoints().PDS.BaseURL, instanceDID, "",
		nil, nil, nil, nil,
		communities.PrivateHostOptions(true)...,
	)

	router := chi.NewRouter()
	routes.RegisterFeedRoutes(router, feeds.service, feeds.registry, communityService, voteService, nil,
		auth.OAuthAuthMiddleware, moderation.NewAllowlistAuthority(nil), instanceDID)
	return router
}

// newFeedVoteService is the real vote service over db, without a PDS.
func newFeedVoteService(db *sql.DB) votes.Service {
	return votes.NewServiceWithPDSFactory(NewVoteRepository(db), nil, nil)
}

// feedAliasServer serves the alias endpoints over the production post
// built-ins on one database, for the suites that pinned the old endpoints'
// HTTP behaviour (getCommunity, getTimeline, getDiscover).
type feedAliasServer struct {
	router http.Handler
	auth   *fixtures.OAuthMiddleware
}

// newFeedAliasServer wires the router over db. voteService nil serves the
// real vote service over db.
func newFeedAliasServer(db *sql.DB, voteService votes.Service) *feedAliasServer {
	restriction := WithAccountRestrictionAuthority(fixtures.InstanceDID())
	if voteService == nil {
		voteService = newFeedVoteService(db)
	}
	auth := fixtures.NewOAuthMiddleware()
	feeds := newBuiltinFeeds(db, feedAliasCursorSecret, restriction)
	return &feedAliasServer{router: newFeedRouter(db, feeds, voteService, auth, restriction), auth: auth}
}

// Serve issues GET path?rawQuery as viewerDID ("" = anonymous, no
// Authorization header) and returns the recorded response.
func (s *feedAliasServer) Serve(path, rawQuery, viewerDID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path+"?"+rawQuery, nil)
	if viewerDID != "" {
		request.Header.Set("Authorization", "Bearer "+s.auth.AddUser(viewerDID))
	}
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, request)
	return recorder
}

// FeedAliasPage is one decoded feed page; Cursor is nil when the response
// carried none.
type FeedAliasPage struct {
	Feed   []*feedgen.FeedViewPost `json:"feed"`
	Cursor *string                 `json:"cursor,omitempty"`
}

// Page requires a 200 for GET path?rawQuery as viewerDID and decodes it.
func (s *feedAliasServer) Page(t *testing.T, path, rawQuery, viewerDID string) FeedAliasPage {
	t.Helper()

	recorder := s.Serve(path, rawQuery, viewerDID)
	require.Equalf(t, http.StatusOK, recorder.Code, "GET %s?%s: %s", path, rawQuery, recorder.Body.String())

	var page FeedAliasPage
	require.NoErrorf(t, json.Unmarshal(recorder.Body.Bytes(), &page), "GET %s?%s: decoding %q", path, rawQuery, recorder.Body.String())
	return page
}

// Exported names for the package postgres_test suites.
type FeedAliasServer = feedAliasServer

// The alias endpoints' paths.
const (
	GetTimelinePath  = getTimelinePath
	GetCommunityPath = getCommunityPath
	GetDiscoverPath  = getDiscoverPath
)

// NewFeedAliasServer is newFeedAliasServer for package postgres_test.
func NewFeedAliasServer(db *sql.DB, voteService votes.Service) *FeedAliasServer {
	return newFeedAliasServer(db, voteService)
}
