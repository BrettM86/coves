package labels_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/labels"
	"Coves/tests/testkit"
)

// TestLabelStreamCapsOpenSubscriptions covers Subscribe at the subscription
// cap: the subscription past the cap is refused with ErrStreamFull, and a
// closed subscription frees its slot.
func TestLabelStreamCapsOpenSubscriptions(t *testing.T) {
	ctx := context.Background()
	stream := labels.NewStream(&appendableLabelSource{}, labels.StreamOptions{
		PageSize: 10, PollInterval: 10 * time.Millisecond, MaxSubscriptions: 2,
	})
	t.Cleanup(stream.Close)

	first, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	t.Cleanup(first.Close)
	second, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	t.Cleanup(second.Close)

	third, err := stream.Subscribe(ctx, nil)
	if third != nil {
		t.Cleanup(third.Close)
	}
	if !errors.Is(err, labels.ErrStreamFull) {
		t.Fatalf("third Subscribe with two of two subscriptions open returned %v, want ErrStreamFull", err)
	}

	first.Close()
	replacement, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("Subscribe after a subscription closed returned %v, want a subscription in the freed slot", err)
	}
	t.Cleanup(replacement.Close)
}

// TestLabelStreamAdmitsConcurrentSubscribersAtomically covers many callers
// racing for the last slot: exactly one is admitted and every other one is
// refused with ErrStreamFull.
func TestLabelStreamAdmitsConcurrentSubscribersAtomically(t *testing.T) {
	const subscribers = 32
	ctx := context.Background()
	stream := labels.NewStream(&appendableLabelSource{}, labels.StreamOptions{
		PageSize: 10, PollInterval: 10 * time.Millisecond, MaxSubscriptions: 1,
	})
	t.Cleanup(stream.Close)

	start := make(chan struct{})
	var group sync.WaitGroup
	var mutex sync.Mutex
	admitted := 0
	var unexpected []error
	for range subscribers {
		group.Go(func() {
			<-start
			subscription, err := stream.Subscribe(ctx, nil)
			mutex.Lock()
			defer mutex.Unlock()
			switch {
			case err == nil:
				admitted++
				t.Cleanup(subscription.Close)
			case !errors.Is(err, labels.ErrStreamFull):
				unexpected = append(unexpected, err)
			}
		})
	}
	close(start)
	group.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("refused subscribers returned %v, want only ErrStreamFull", unexpected)
	}
	if admitted != 1 {
		t.Fatalf("%d of 32 concurrent subscribers were admitted under a cap of 1, want exactly 1", admitted)
	}
}

// heldRead is one LabelsAfter call held until the test answers it.
type heldRead struct {
	afterSeq int64
	answer   chan []labels.Label
}

// pollingLabelSource serves an in-memory label log the test appends to. It
// counts LabelsAfter calls made with a live context (a read on a cancelled
// context is not polling), and it can hold the next LabelsAfter call until
// the test answers it. A held call ignores its context, standing in for a
// read whose rows had already come back when its reader was cancelled.
type pollingLabelSource struct {
	mutex     sync.Mutex
	log       []labels.Label
	liveReads int
	holdNext  bool
	held      chan heldRead
}

func newPollingLabelSource() *pollingLabelSource {
	return &pollingLabelSource{held: make(chan heldRead, 1)}
}

func (source *pollingLabelSource) commitThrough(lastSeq int64) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	for seq := int64(len(source.log)) + 1; seq <= lastSeq; seq++ {
		source.log = append(source.log, labels.Label{Seq: seq, Val: fmt.Sprintf("label-%d", seq)})
	}
}

func (source *pollingLabelSource) committed(firstSeq, lastSeq int64) []labels.Label {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return slices.Clone(source.log[firstSeq-1 : lastSeq])
}

func (source *pollingLabelSource) liveReadCount() int {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.liveReads
}

func (source *pollingLabelSource) holdNextRead() {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.holdNext = true
}

func (source *pollingLabelSource) LatestSeq(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return int64(len(source.log)), nil
}

func (source *pollingLabelSource) LabelsAfter(ctx context.Context, afterSeq int64, limit int) ([]labels.Label, error) {
	source.mutex.Lock()
	if ctx.Err() != nil {
		source.mutex.Unlock()
		return nil, ctx.Err()
	}
	source.liveReads++
	if source.holdNext {
		source.holdNext = false
		source.mutex.Unlock()
		read := heldRead{afterSeq: afterSeq, answer: make(chan []labels.Label)}
		source.held <- read
		return <-read.answer, nil
	}
	defer source.mutex.Unlock()
	if afterSeq >= int64(len(source.log)) {
		return nil, nil
	}
	end := min(int(afterSeq)+limit, len(source.log))
	return slices.Clone(source.log[afterSeq:end]), nil
}

// runCollecting runs subscription on its own goroutine, handing every label
// it sends to the returned channel. done is closed when Run returns.
func runCollecting(t *testing.T, subscription *labels.Subscription) (received <-chan labels.Label, done <-chan struct{}) {
	t.Helper()
	labelsSent := make(chan labels.Label, 64)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = subscription.Run(func(label labels.Label) error {
			labelsSent <- label
			return nil
		}, noopCloser{})
	}()
	t.Cleanup(func() {
		subscription.Close()
		<-runDone
	})
	return labelsSent, runDone
}

func receiveSeqs(t *testing.T, received <-chan labels.Label, count int, description string) []int64 {
	t.Helper()
	var seqs []int64
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for len(seqs) < count {
		select {
		case label := <-received:
			seqs = append(seqs, label.Seq)
		case <-timeout.C:
			t.Fatalf("%s: received seqs %v, want %d labels within 5s", description, seqs, count)
		}
	}
	return seqs
}

// TestLabelStreamStopsPollingWithoutSubscribers covers the shared poller's
// lifetime: once the last subscription closes the source sees no further
// polling reads, and a later Subscribe restarts polling and delivers newly
// committed labels in seq order.
func TestLabelStreamStopsPollingWithoutSubscribers(t *testing.T) {
	const pollInterval = 10 * time.Millisecond
	ctx := context.Background()
	source := newPollingLabelSource()
	stream := labels.NewStream(source, labels.StreamOptions{PageSize: 10, PollInterval: pollInterval})
	t.Cleanup(stream.Close)

	first, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	firstReceived, firstDone := runCollecting(t, first)
	source.commitThrough(1)
	if seqs := receiveSeqs(t, firstReceived, 1, "first subscriber"); !slices.Equal(seqs, []int64{1}) {
		t.Fatalf("first subscriber received seqs %v, want [1]", seqs)
	}

	first.Close()
	<-firstDone
	readsAtClose := source.liveReadCount()
	testkit.Holds(t, 20*pollInterval, func() (bool, error) {
		return source.liveReadCount() == readsAtClose, nil
	}, testkit.WithPollInterval(pollInterval),
		testkit.WithDescription("no polling reads of the label source after the last subscription closed"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("%d live reads at close, %d now", readsAtClose, source.liveReadCount())
		}))

	second, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	secondReceived, _ := runCollecting(t, second)
	source.commitThrough(4)
	if seqs := receiveSeqs(t, secondReceived, 3, "subscriber after polling restarted"); !slices.Equal(seqs, []int64{2, 3, 4}) {
		t.Fatalf("subscriber after polling restarted received seqs %v, want [2 3 4]", seqs)
	}
}

// TestLabelStreamDropsStalePollAfterRestart covers a poll read still in
// flight when the last subscription closes. Polling restarts for the next
// subscription, and when the stopped poller's read finally returns its rows
// they are not published: every subscriber receives each label exactly once,
// in seq order, with no gap.
func TestLabelStreamDropsStalePollAfterRestart(t *testing.T) {
	// The poller reads once when it starts and then not again within the
	// test, so every window change is one the test released.
	const pollInterval = time.Hour
	ctx := context.Background()
	source := newPollingLabelSource()
	source.commitThrough(2)
	stream := labels.NewStream(source, labels.StreamOptions{PageSize: 10, PollInterval: pollInterval})
	t.Cleanup(stream.Close)
	awaitHeldRead := func(description string) heldRead {
		t.Helper()
		select {
		case read := <-source.held:
			return read
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting: %s", description)
			return heldRead{}
		}
	}

	// The first subscription never runs, so the only LabelsAfter caller is
	// the poller it starts.
	source.holdNextRead()
	first, err := stream.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	stale := awaitHeldRead("the first poller reads the label source")
	if stale.afterSeq != 2 {
		t.Fatalf("the first poller read after seq %d, want 2", stale.afterSeq)
	}
	t.Cleanup(func() {
		select {
		case stale.answer <- nil:
		default:
		}
	})
	source.commitThrough(4)
	first.Close()

	// The second subscription is not running yet either, so the held read is
	// the restarted poller's.
	source.holdNextRead()
	second, err := stream.Subscribe(ctx, func() *int64 { cursor := int64(4); return &cursor }())
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	restarted := awaitHeldRead("polling restarts for the subscription opened after the last one closed")
	source.commitThrough(6)
	restarted.answer <- source.committed(restarted.afterSeq+1, 6)
	// From cursor 4 the second subscriber reads the restarted poller's window,
	// so receiving seqs 5 and 6 shows that poller has published them.
	secondReceived, _ := runCollecting(t, second)
	if seqs := receiveSeqs(t, secondReceived, 2, "second subscriber"); !slices.Equal(seqs, []int64{5, 6}) {
		t.Fatalf("second subscriber received seqs %v, want [5 6]", seqs)
	}

	// The stopped poller's read returns the rows committed while it was held.
	select {
	case stale.answer <- source.committed(3, 4):
	case <-time.After(5 * time.Second):
		t.Fatal("the stale poll read was no longer waiting for its rows")
	}

	// Holding here also gives a published stale page time to land before the
	// third subscriber reads the window.
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		return len(secondReceived) == 0, nil
	}, testkit.WithPollInterval(10*time.Millisecond),
		testkit.WithDescription("the second subscriber receives nothing after seq 6"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("%d labels pending for the second subscriber", len(secondReceived))
		}))

	third, err := stream.Subscribe(ctx, func() *int64 { cursor := int64(4); return &cursor }())
	if err != nil {
		t.Fatalf("third Subscribe: %v", err)
	}
	thirdReceived, _ := runCollecting(t, third)
	if seqs := receiveSeqs(t, thirdReceived, 2, "subscriber from cursor 4 after the stale read returned"); !slices.Equal(seqs, []int64{5, 6}) {
		t.Fatalf("subscriber from cursor 4 after the stale read returned received seqs %v, want [5 6]", seqs)
	}
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		return len(thirdReceived) == 0, nil
	}, testkit.WithPollInterval(10*time.Millisecond),
		testkit.WithDescription("the third subscriber receives nothing after seq 6"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("%d labels pending for the third subscriber", len(thirdReceived))
		}))
}
