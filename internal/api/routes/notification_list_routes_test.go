package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

type listRouteService struct{ reached bool }

func (s *listRouteService) ListNotifications(context.Context, string, string, int) (notifications.ListNotificationsOutput, error) {
	s.reached = true
	return notifications.ListNotificationsOutput{}, nil
}

func TestNotificationListRoute_RequiresAuth(t *testing.T) {
	service := &listRouteService{}
	unsealer := &probeUnsealer{seen: map[string]bool{}}
	mux := chi.NewRouter()
	RegisterNotificationListRoutes(mux, service, middleware.NewOAuthAuthMiddleware(unsealer, nil))
	routes := walkRoutes(t, mux)
	key := routeKey{method: http.MethodGet, path: "/xrpc/social.coves.notification.listNotifications"}
	chain, registered := routes[key]
	require.True(t, registered, "GET listNotifications must be registered")
	require.Len(t, routes, 1, "listNotifications registers exactly one GET route")
	facts := chainFacts(chain, unsealer)
	require.Equal(t, 1, countKind(facts, mwRequireAuth))
	require.Equal(t, 0, countKind(facts, mwOptionalAuth))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, key.path, nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, service.reached, "anonymous GET must not reach ListNotifications")
}
