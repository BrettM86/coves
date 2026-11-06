package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/routes"
	"Coves/internal/core/labels"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// idleLabelSource is a label log with a fixed latest seq that never grows.
type idleLabelSource struct{}

func (idleLabelSource) LabelsAfter(context.Context, int64, int) ([]labels.Label, error) {
	return nil, nil
}

func (idleLabelSource) LatestSeq(context.Context) (int64, error) { return 7, nil }

func TestApplicationCloseEndsOpenLabelSubscriptions(t *testing.T) {
	stream := labels.NewStream(idleLabelSource{}, labels.StreamOptions{PollInterval: 10 * time.Millisecond})
	app := &application{labelStream: stream, stopImageProxyCleanup: func() {}}
	router := chi.NewRouter()
	routes.RegisterLabelStreamRoutes(router, stream)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	subscription, err := testkit.DialLabelStream(t, t.Context(), server.URL, nil)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")

	app.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = subscription.Next(ctx)
	require.ErrorIs(t, err, testkit.ErrLabelStreamClosed, "closing the application must end open label subscriptions")
}
