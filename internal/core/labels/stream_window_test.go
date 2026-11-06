package labels

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// windowTestSource is an in-memory StreamSource over an appendable label log.
// It records the highest afterSeq for which LabelsAfter returned no rows, so a
// test can wait until a reader has read through a given seq.
type windowTestSource struct {
	mutex          sync.Mutex
	log            []Label
	drainedThrough int64
	read           chan struct{} // closed and replaced after every LabelsAfter
}

func newWindowTestSource() *windowTestSource {
	return &windowTestSource{drainedThrough: -1, read: make(chan struct{})}
}

func (s *windowTestSource) appendThrough(lastSeq int64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	next := int64(1)
	if len(s.log) > 0 {
		next = s.log[len(s.log)-1].Seq + 1
	}
	for seq := next; seq <= lastSeq; seq++ {
		s.log = append(s.log, Label{Seq: seq, Val: "!takedown"})
	}
}

func (s *windowTestSource) LabelsAfter(ctx context.Context, afterSeq int64, limit int) ([]Label, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	var page []Label
	for _, label := range s.log {
		if label.Seq > afterSeq && len(page) < limit {
			page = append(page, label)
		}
	}
	if len(page) == 0 && afterSeq > s.drainedThrough {
		s.drainedThrough = afterSeq
	}
	close(s.read)
	s.read = make(chan struct{})
	return page, nil
}

func (s *windowTestSource) LatestSeq(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if len(s.log) == 0 {
		return 0, nil
	}
	return s.log[len(s.log)-1].Seq, nil
}

// waitForDrainedThrough waits until some reader found no rows after seq.
func (s *windowTestSource) waitForDrainedThrough(t *testing.T, seq int64, description string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		s.mutex.Lock()
		drained, read := s.drainedThrough >= seq, s.read
		s.mutex.Unlock()
		if drained {
			return
		}
		select {
		case <-read:
		case <-deadline.C:
			t.Fatalf("timed out waiting: %s", description)
		}
	}
}

type windowTestCloser struct{}

func (windowTestCloser) Close() error { return nil }

// A subscription held in its send while the poller evicts the labels after it
// from the live window must, once released, read the evicted labels from the
// source rather than skip to the first label still in the window.
func TestSubscriptionHeldPastWindowEvictionReceivesEverySeq(t *testing.T) {
	source := newWindowTestSource()
	source.appendThrough(1)
	stream := NewStream(source, StreamOptions{PageSize: 100, PollInterval: time.Millisecond})
	t.Cleanup(stream.Close)

	cursor := int64(0)
	subscription, err := stream.Subscribe(t.Context(), &cursor)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	finalSeq := int64(1 + liveWindowCapacity + 100)
	parked := make(chan struct{})
	release := make(chan struct{})
	receivedFinal := make(chan struct{})
	var received []int64
	send := func(label Label) error {
		received = append(received, label.Seq)
		if len(received) == 1 {
			close(parked)
			<-release
		}
		if label.Seq == finalSeq {
			close(receivedFinal)
		}
		return nil
	}
	runResult := make(chan error, 1)
	go func() { runResult <- subscription.Run(send, windowTestCloser{}) }()

	waitFor := func(signal <-chan struct{}, description string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting: %s", description)
		}
	}
	waitFor(parked, "the subscription is held sending seq 1")
	// The subscription is parked, so only the poller reads after seq 1.
	source.waitForDrainedThrough(t, 1, "the poller starts its window at seq 1")

	source.appendThrough(finalSeq)
	source.waitForDrainedThrough(t, finalSeq, "the poller reads through the final seq, evicting the labels after seq 1")

	close(release)
	waitFor(receivedFinal, "the subscription receives the final seq")
	subscription.Close()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Run to return after Close")
	}

	want := make([]int64, 0, finalSeq)
	for seq := int64(1); seq <= finalSeq; seq++ {
		want = append(want, seq)
	}
	if !slices.Equal(received, want) {
		t.Fatalf("received %d labels, want seqs 1..%d contiguously; first after seq 1: %v", len(received), finalSeq, received[1:min(len(received), 4)])
	}
}
