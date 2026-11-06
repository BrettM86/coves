package labels_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/labels"
)

// appendableLabelSource serves labels from an in-memory log the test appends to.
type appendableLabelSource struct {
	mutex  sync.Mutex
	labels []labels.Label
}

func (source *appendableLabelSource) appendLabel(value string) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.labels = append(source.labels, labels.Label{Seq: int64(len(source.labels) + 1), Val: value})
}

func (source *appendableLabelSource) LatestSeq(context.Context) (int64, error) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return int64(len(source.labels)), nil
}

func (source *appendableLabelSource) LabelsAfter(_ context.Context, afterSeq int64, limit int) ([]labels.Label, error) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	if afterSeq >= int64(len(source.labels)) {
		return nil, nil
	}
	end := min(int(afterSeq)+limit, len(source.labels))
	return append([]labels.Label(nil), source.labels[afterSeq:end]...), nil
}

var errPeerClosed = errors.New("peer connection closed")

// blockingPeer is a subscriber that has stopped reading: its send blocks until
// the connection is closed.
type blockingPeer struct {
	blocked   chan struct{}
	closed    chan struct{}
	blockOnce sync.Once
	closeOnce sync.Once
}

func newBlockingPeer() *blockingPeer {
	return &blockingPeer{blocked: make(chan struct{}), closed: make(chan struct{})}
}

func (peer *blockingPeer) send(labels.Label) error {
	peer.blockOnce.Do(func() { close(peer.blocked) })
	<-peer.closed
	return errPeerClosed
}

func (peer *blockingPeer) Close() error {
	peer.closeOnce.Do(func() { close(peer.closed) })
	return nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// TestLabelStreamBlockedSendStallsOnlyItself covers a subscriber whose send
// blocks on a peer that stopped reading: another subscriber still receives
// newly polled labels, and Stream.Close releases the blocked send by closing
// its connection.
func TestLabelStreamBlockedSendStallsOnlyItself(t *testing.T) {
	const boundedWait = 5 * time.Second
	ctx := context.Background()
	source := &appendableLabelSource{}
	stream := labels.NewStream(source, labels.StreamOptions{PageSize: 10, PollInterval: 10 * time.Millisecond})
	// With a broken Stream the test fails while a Run may still hold the
	// stream; close in the background so a failure cannot hang the binary.
	t.Cleanup(func() { go stream.Close() })

	blockedSubscription, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("subscribing the blocked subscriber: %v", err)
	}
	readingSubscription, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("subscribing the reading subscriber: %v", err)
	}

	peer := newBlockingPeer()
	t.Cleanup(func() { _ = peer.Close() })
	blockedRunResult := make(chan error, 1)
	go func() { blockedRunResult <- blockedSubscription.Run(peer.send, peer) }()

	received := make(chan labels.Label, 16)
	go func() {
		_ = readingSubscription.Run(func(label labels.Label) error {
			received <- label
			return nil
		}, noopCloser{})
	}()

	receive := func(wantSeq int64) {
		t.Helper()
		select {
		case label := <-received:
			if label.Seq != wantSeq {
				t.Fatalf("reading subscriber received seq %d, want %d", label.Seq, wantSeq)
			}
		case <-time.After(boundedWait):
			t.Fatalf("reading subscriber did not receive seq %d while another subscriber's send was blocked", wantSeq)
		}
	}

	source.appendLabel("first")
	select {
	case <-peer.blocked:
	case <-time.After(boundedWait):
		t.Fatal("the blocked subscriber never received a label to send")
	}
	receive(1)

	source.appendLabel("second")
	source.appendLabel("third")
	receive(2)
	receive(3)

	stream.Close()
	select {
	case err := <-blockedRunResult:
		if !errors.Is(err, errPeerClosed) {
			t.Fatalf("blocked Run returned %v, want %v", err, errPeerClosed)
		}
	case <-time.After(boundedWait):
		t.Fatal("Stream.Close did not release a Run blocked sending to its peer")
	}
}
