package labels_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	labelhandlers "Coves/internal/api/handlers/labels"
	"Coves/internal/core/labels"
	"Coves/tests/testkit"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// idleLabelSource has no labels, so a subscriber on it is sent nothing.
type idleLabelSource struct{}

func (idleLabelSource) LatestSeq(context.Context) (int64, error) { return 0, nil }

func (idleLabelSource) LabelsAfter(context.Context, int64, int) ([]labels.Label, error) {
	return nil, nil
}

// countingLabelServer serves the subscribeLabels handler and counts the
// requests whose handler has returned.
type countingLabelServer struct {
	URL      string
	returned atomic.Int64
}

func newCountingLabelServer(t *testing.T, handler http.Handler) *countingLabelServer {
	t.Helper()
	server := &countingLabelServer{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer server.returned.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	server.URL = httpServer.URL
	return server
}

// dialAdmitted reports whether a dial is upgraded (true) or refused because
// the stream is full (false); any other outcome is an error.
func dialAdmitted(t *testing.T, baseURL string) (bool, error) {
	t.Helper()
	conn, err := testkit.DialLabelStreamConn(t, context.Background(), baseURL, nil)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	var refused *testkit.LabelStreamHandshakeError
	if errors.As(err, &refused) && refused.StatusCode == http.StatusServiceUnavailable {
		return false, nil
	}
	return false, err
}

// TestSubscribeLabelsRefusesDialWhenStreamIsFull covers the cap at the
// handler: a dial to a full stream is answered before any upgrade with HTTP
// 503 and the XRPC error Unavailable.
func TestSubscribeLabelsRefusesDialWhenStreamIsFull(t *testing.T) {
	stream := labels.NewStream(idleLabelSource{}, labels.StreamOptions{PollInterval: 10 * time.Millisecond, MaxSubscriptions: 1})
	t.Cleanup(stream.Close)
	occupant, err := stream.Subscribe(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(occupant.Close)
	server := newCountingLabelServer(t, labelhandlers.NewSubscribeLabelsHandler(stream))

	_, err = testkit.DialLabelStreamConn(t, context.Background(), server.URL, nil)
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "a dial to a full stream must be refused before the upgrade")
	var refused *testkit.LabelStreamHandshakeError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusServiceUnavailable, refused.StatusCode)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(refused.Body, &body), "body: %s", refused.Body)
	require.Equal(t, "Unavailable", body.Error)
}

// TestSubscribeLabelsKeepsPeerThatAnswersPings covers the heartbeat for an
// idle subscriber that keeps reading: it is pinged, its pongs extend the read
// deadline, and it stays connected across several pong windows.
func TestSubscribeLabelsKeepsPeerThatAnswersPings(t *testing.T) {
	const pongWait = 200 * time.Millisecond
	stream := labels.NewStream(idleLabelSource{}, labels.StreamOptions{PollInterval: 10 * time.Millisecond, MaxSubscriptions: 1})
	t.Cleanup(stream.Close)
	server := newCountingLabelServer(t, labelhandlers.NewSubscribeLabelsHandler(stream,
		labelhandlers.WithStreamTimeouts(time.Second, 50*time.Millisecond, pongWait)))

	conn, err := testkit.DialLabelStreamConn(t, context.Background(), server.URL, nil)
	require.NoError(t, err)
	var pings atomic.Int64
	conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	testkit.Holds(t, 5*pongWait, func() (bool, error) {
		return server.returned.Load() == 0, nil
	}, testkit.WithPollInterval(20*time.Millisecond),
		testkit.WithDescription("the subscription of a peer that answers pings stays open"))
	require.GreaterOrEqual(t, pings.Load(), int64(3), "an idle peer must be pinged every ping interval")
}

// TestSubscribeLabelsDropsPeerThatNeverAnswersPings covers the heartbeat for
// a subscriber that never reads, so never answers a ping: it is disconnected
// once the pong wait passes and its subscription slot is freed.
func TestSubscribeLabelsDropsPeerThatNeverAnswersPings(t *testing.T) {
	stream := labels.NewStream(idleLabelSource{}, labels.StreamOptions{PollInterval: 10 * time.Millisecond, MaxSubscriptions: 1})
	t.Cleanup(stream.Close)
	server := newCountingLabelServer(t, labelhandlers.NewSubscribeLabelsHandler(stream,
		labelhandlers.WithStreamTimeouts(time.Second, 50*time.Millisecond, 200*time.Millisecond)))

	_, err := testkit.DialLabelStreamConn(t, context.Background(), server.URL, nil)
	require.NoError(t, err)

	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return server.returned.Load() == 1, nil
	}, testkit.WithPollInterval(20*time.Millisecond),
		testkit.WithDescription("the handler drops a peer that never answers pings"))
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return dialAdmitted(t, server.URL)
	}, testkit.WithPollInterval(20*time.Millisecond),
		testkit.WithDescription("a new dial is admitted to the slot the dropped peer held"))
}

// TestSubscribeLabelsWriteTimeoutDropsPeerThatStopsReading covers the
// per-frame write deadline: a subscriber that stops reading while large
// labels are pending fills the socket buffers, the blocked frame write times
// out long before the heartbeat could notice, and the slot is freed.
func TestSubscribeLabelsWriteTimeoutDropsPeerThatStopsReading(t *testing.T) {
	// 32 KiB labels without end: enough to fill the loopback socket buffers.
	source := &endlessLabelSource{value: strings.Repeat("v", 32*1024), latestSeq: 1 << 40}
	stream := labels.NewStream(source, labels.StreamOptions{PageSize: 10, PollInterval: 10 * time.Millisecond, MaxSubscriptions: 1})
	t.Cleanup(stream.Close)
	server := newCountingLabelServer(t, labelhandlers.NewSubscribeLabelsHandler(stream,
		labelhandlers.WithStreamTimeouts(200*time.Millisecond, 50*time.Second, time.Minute)))

	cursor := int64(0)
	_, err := testkit.DialLabelStreamConn(t, context.Background(), server.URL, &cursor)
	require.NoError(t, err)

	testkit.WaitFor(t, 15*time.Second, func() (bool, error) {
		return server.returned.Load() == 1, nil
	}, testkit.WithPollInterval(20*time.Millisecond),
		testkit.WithDescription("the handler's blocked frame write times out and drops a peer that stopped reading"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("highest seq served: %d", source.highestServed.Load())
		}))
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return dialAdmitted(t, server.URL)
	}, testkit.WithPollInterval(20*time.Millisecond),
		testkit.WithDescription("a new dial is admitted to the slot the dropped peer held"))
}
