package labels_test

import (
	"context"
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

// endlessLabelSource serves labels 1..latestSeq, each carrying a large value,
// so a subscriber replaying from cursor 0 never runs out of labels to send.
type endlessLabelSource struct {
	value         string
	latestSeq     int64
	highestServed atomic.Int64
}

func (source *endlessLabelSource) LatestSeq(context.Context) (int64, error) {
	return source.latestSeq, nil
}

func (source *endlessLabelSource) LabelsAfter(_ context.Context, afterSeq int64, limit int) ([]labels.Label, error) {
	var page []labels.Label
	for seq := afterSeq + 1; seq <= source.latestSeq && len(page) < limit; seq++ {
		page = append(page, labels.Label{
			Seq:       seq,
			Src:       "did:web:coves.social",
			URI:       "at://did:plc:subject/social.coves.community.post/3kabc",
			Val:       source.value,
			CreatedAt: "2026-10-22T00:00:00.000Z",
			Sig:       []byte("signature"),
			Version:   1,
		})
	}
	if len(page) > 0 {
		source.highestServed.Store(page[len(page)-1].Seq)
	}
	return page, nil
}

// TestSubscribeLabelsReleasesBlockedWriterWhenSubscriberCloses covers a
// subscriber that stops reading until the server's write blocks, then sends a
// close frame. The handler must return rather than hold its goroutine and
// socket on the blocked write.
func TestSubscribeLabelsReleasesBlockedWriterWhenSubscriberCloses(t *testing.T) {
	source := &endlessLabelSource{value: strings.Repeat("v", 32*1024), latestSeq: 1 << 40}
	stream := labels.NewStream(source, labels.StreamOptions{PageSize: 10, PollInterval: 10 * time.Millisecond})
	t.Cleanup(stream.Close)

	handlerReturned := make(chan struct{})
	handler := labelhandlers.NewSubscribeLabelsHandler(stream)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerReturned)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	// The connection's cleanup is registered after server.Close so it runs
	// first: dropping the client socket releases a still-blocked server write
	// if the handler is broken.
	cursor := int64(0)
	clientConn, err := testkit.DialLabelStreamConn(t, context.Background(), server.URL, &cursor)
	require.NoError(t, err)

	// The client never reads. Wait until the source stops being read, which
	// means the server's write is blocked on a full socket buffer.
	const stableTicks = 10
	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	lastServed, unchangedTicks := int64(-1), 0
	for unchangedTicks < stableTicks {
		select {
		case <-deadline:
			t.Fatalf("the server writer never blocked; last served seq %d", source.highestServed.Load())
		case <-ticker.C:
		}
		served := source.highestServed.Load()
		if served > 0 && served == lastServed {
			unchangedTicks++
		} else {
			lastServed, unchangedTicks = served, 0
		}
	}

	require.NoError(t, clientConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribeLabels handler still blocked writing to a subscriber that closed its connection")
	}
}
