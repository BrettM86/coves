//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationQueryAuthContract(t *testing.T) {
	p := newPipeline(t)

	for _, nsid := range []string{"social.coves.notification.getUnreadCount"} {
		t.Run(nsid, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
			defer cancel()
			err := p.AppView.Query(ctx, nsid, nil, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
				"%s must answer 401 to a client with no session; answered: %v", nsid, err)
		})
	}
}

func TestNotificationPreferencesAuthContract(t *testing.T) {
	p := newPipeline(t)

	t.Run("social.coves.notification.getPreferences", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		const nsid = "social.coves.notification.getPreferences"
		err := p.AppView.Query(ctx, nsid, nil, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"%s must answer 401 to a client with no session; answered: %v", nsid, err)
	})

	t.Run("social.coves.notification.putPreferences", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		const nsid = "social.coves.notification.putPreferences"
		err := p.AppView.Procedure(ctx, nsid, map[string]any{"mention": false}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"%s must answer 401 to a client with no session; answered: %v", nsid, err)
	})
}

func TestNotificationUpdateSeenAuthContract(t *testing.T) {
	p := newPipeline(t)
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	const nsid = "social.coves.notification.updateSeen"
	err := p.AppView.Procedure(ctx, nsid, map[string]any{"seenAt": "2026-09-30T12:00:00Z"}, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"%s must answer 401 to a client with no session; answered: %v", nsid, err)
}

func TestNotificationListAuthContract(t *testing.T) {
	p := newPipeline(t)
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	const nsid = "social.coves.notification.listNotifications"
	err := p.AppView.Query(ctx, nsid, nil, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"%s must answer 401 to a client with no session; answered: %v", nsid, err)
}
