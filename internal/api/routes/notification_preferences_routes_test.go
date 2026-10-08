package routes

import (
	"context"
	"net/http"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

type preferenceRouteService struct{}

var _ notifications.PreferencesService = preferenceRouteService{}

func (preferenceRouteService) GetPreferences(context.Context, string) (notifications.Preferences, error) {
	return notifications.Preferences{}, nil
}

func (preferenceRouteService) PutPreferences(context.Context, string, notifications.PreferencesUpdate) (notifications.Preferences, error) {
	return notifications.Preferences{}, nil
}

func TestNotificationPreferenceRoutes_AuthenticatedOnly(t *testing.T) {
	unsealer := &probeUnsealer{seen: map[string]bool{}}
	mux := chi.NewRouter()
	RegisterNotificationPreferenceRoutes(mux, preferenceRouteService{}, middleware.NewOAuthAuthMiddleware(unsealer, nil))

	routes := walkRoutes(t, mux)
	require.Len(t, routes, 2, "notification preferences must register exactly GET getPreferences and POST putPreferences")
	for _, key := range []routeKey{
		{method: http.MethodGet, path: "/xrpc/social.coves.notification.getPreferences"},
		{method: http.MethodPost, path: "/xrpc/social.coves.notification.putPreferences"},
	} {
		t.Run(key.String(), func(t *testing.T) {
			chain, registered := routes[key]
			require.True(t, registered, "%s must be registered", key)
			facts := chainFacts(chain, unsealer)
			require.Equal(t, 1, countKind(facts, mwRequireAuth), "%s needs exactly one RequireAuth", key)
			require.Equal(t, 0, countKind(facts, mwOptionalAuth), "%s must not use OptionalAuth", key)
		})
	}
}
