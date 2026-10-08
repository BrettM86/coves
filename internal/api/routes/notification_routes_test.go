package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

type unreadRouteService struct {
	reached          bool
	updateSeenCalled bool
}

var _ notifications.Service = (*unreadRouteService)(nil)

func (s *unreadRouteService) CountUnread(context.Context, string) (int, error) {
	s.reached = true
	return 3, nil
}

func (s *unreadRouteService) UpdateSeen(context.Context, string, time.Time) error {
	s.updateSeenCalled = true
	return nil
}

func TestNotificationRoutes_AuthenticatedGetOnly(t *testing.T) {
	service := &unreadRouteService{}
	unsealer := &probeUnsealer{seen: map[string]bool{}}
	mux := chi.NewRouter()
	RegisterNotificationRoutes(mux, service, middleware.NewOAuthAuthMiddleware(unsealer, nil))

	routes := walkRoutes(t, mux)
	key := routeKey{method: http.MethodGet, path: "/xrpc/social.coves.notification.getUnreadCount"}
	chain, registered := routes[key]
	require.True(t, registered, "notification routes must serve GET getUnreadCount")
	facts := chainFacts(chain, unsealer)
	require.Equal(t, 1, countKind(facts, mwRequireAuth), "getUnreadCount needs exactly one RequireAuth")
	require.Equal(t, 0, countKind(facts, mwOptionalAuth))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, key.path, nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, service.reached, "anonymous requests must not reach CountUnread")

	require.Len(t, routes, 2, "notification routes must expose GET getUnreadCount and POST updateSeen")
	postKey := routeKey{method: http.MethodPost, path: "/xrpc/social.coves.notification.updateSeen"}
	postChain, registered := routes[postKey]
	require.True(t, registered, "notification routes must serve POST updateSeen")
	postFacts := chainFacts(postChain, unsealer)
	require.Equal(t, 1, countKind(postFacts, mwRequireAuth), "updateSeen needs exactly one RequireAuth")
	require.Equal(t, 0, countKind(postFacts, mwOptionalAuth))

	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, postKey.path, nil))
	require.Equal(t, http.StatusUnauthorized, postRec.Code)
	require.False(t, service.updateSeenCalled, "anonymous requests must not reach UpdateSeen")
}
