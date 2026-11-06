package labels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"
)

// Production defaults for a Stream.
const (
	defaultStreamPageSize     = 100
	defaultStreamPollInterval = 500 * time.Millisecond
	defaultMaxSubscriptions   = 256

	// liveWindowCapacity bounds the recent labels the shared poller keeps in
	// memory. A subscriber that falls further behind than this reads from the
	// source instead, so it never holds the poller or another subscriber back.
	liveWindowCapacity = 1024
)

var (
	// ErrFutureCursor is returned by Subscribe for a cursor ahead of the
	// latest committed label seq.
	ErrFutureCursor = errors.New("cursor is ahead of the latest label seq")

	// ErrStreamClosed is returned once the Stream has been closed.
	ErrStreamClosed = errors.New("label stream is closed")

	// ErrStreamFull is returned by Subscribe when the Stream already holds
	// its maximum number of open subscriptions.
	ErrStreamFull = errors.New("label stream is at its subscription limit")
)

// StreamSource reads the committed label log in seq order.
//
// Every label writer commits in seq order (the label allocation lock), so a
// read of the rows after a seq sees a gap-free committed prefix: the Stream
// never needs to wait for a seq it has not been given.
type StreamSource interface {
	LabelsAfter(ctx context.Context, afterSeq int64, limit int) ([]Label, error)
	LatestSeq(ctx context.Context) (int64, error)
}

// StreamOptions tunes a Stream. Zero values select production defaults.
type StreamOptions struct {
	PageSize     int
	PollInterval time.Duration
	// MaxSubscriptions caps the open subscriptions; zero selects a modest
	// production default.
	MaxSubscriptions int
}

// Stream serves com.atproto.label.subscribeLabels subscriptions.
//
// One shared poller reads new rows from the source into a bounded window of
// recent labels. Each subscription runs on its caller's goroutine: it replays
// from the source page by page until its last delivered seq is inside the
// window, then delivers from the window. A subscription that cannot keep up
// falls out of the window and reads from the source again, so a slow
// subscriber stalls only itself. The poller runs only while a subscription is
// open: the last one to close stops it and empties the window, and the next
// Subscribe starts a fresh poller.
type Stream struct {
	source           StreamSource
	pageSize         int
	pollInterval     time.Duration
	maxSubscriptions int

	mutex         sync.Mutex
	closed        bool
	subscriptions map[*Subscription]struct{}
	// stopPoller cancels the running poller; nil while no poller runs. A
	// poller checks its context under mutex before every window write, so a
	// stopped poller never publishes.
	stopPoller context.CancelFunc

	// The window holds every committed label with seq in (windowBase,
	// windowHead], in seq order. windowReady is false until the poller has
	// read its starting seq.
	windowReady bool
	windowBase  int64
	windowHead  int64
	window      []Label
	// advanced is closed and replaced whenever the window changes.
	advanced chan struct{}
}

// NewStream returns a Stream reading from source.
func NewStream(source StreamSource, options StreamOptions) *Stream {
	if options.PageSize <= 0 {
		options.PageSize = defaultStreamPageSize
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaultStreamPollInterval
	}
	if options.MaxSubscriptions <= 0 {
		options.MaxSubscriptions = defaultMaxSubscriptions
	}
	return &Stream{
		source:           source,
		pageSize:         options.PageSize,
		pollInterval:     options.PollInterval,
		maxSubscriptions: options.MaxSubscriptions,
		subscriptions:    make(map[*Subscription]struct{}),
		advanced:         make(chan struct{}),
	}
}

// Subscribe starts a subscription. With a nil cursor it delivers only labels
// committed after the latest seq read here; with a cursor it first replays
// every label after it. A cursor ahead of the latest seq is ErrFutureCursor,
// and a Stream already holding its maximum open subscriptions refuses with
// ErrStreamFull. The caller must Run or Close the returned Subscription.
func (s *Stream) Subscribe(ctx context.Context, cursor *int64) (*Subscription, error) {
	if s.isClosed() {
		return nil, ErrStreamClosed
	}
	latest, err := s.source.LatestSeq(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the latest label seq: %w", err)
	}
	delivered := latest
	if cursor != nil {
		if *cursor > latest {
			return nil, ErrFutureCursor
		}
		delivered = *cursor
	}
	subscriptionContext, cancel := context.WithCancel(context.Background())
	subscription := &Subscription{stream: s, ctx: subscriptionContext, cancel: cancel, delivered: delivered}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		cancel()
		return nil, ErrStreamClosed
	}
	if len(s.subscriptions) >= s.maxSubscriptions {
		cancel()
		return nil, ErrStreamFull
	}
	s.subscriptions[subscription] = struct{}{}
	if s.stopPoller == nil {
		pollerContext, stopPoller := context.WithCancel(context.Background())
		s.stopPoller = stopPoller
		go s.poll(pollerContext)
	}
	return subscription, nil
}

// Close ends every open subscription and refuses new ones. It cancels each
// subscription's reads and closes the connection each one registered, so a
// subscription blocked writing to its peer is released too. It is safe to call
// more than once.
func (s *Stream) Close() {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	s.closed = true
	if s.stopPoller != nil {
		s.stopPoller()
	}
	var closers []io.Closer
	for subscription := range s.subscriptions {
		subscription.cancel()
		if subscription.closer != nil {
			closers = append(closers, subscription.closer)
		}
	}
	s.mutex.Unlock()

	for _, closer := range closers {
		_ = closer.Close()
	}
}

func (s *Stream) isClosed() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.closed
}

// poll moves the window forward every poll interval until ctx ends.
func (s *Stream) poll(ctx context.Context) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		s.advanceWindow(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// advanceWindow appends every label committed after the window's head. It is
// called only from the running poller, so it is the only writer of windowHead.
// It re-checks ctx under the mutex before each write: a poller stopped while a
// read was in flight drops that read's rows rather than publish them into the
// window of the poller that replaced it.
func (s *Stream) advanceWindow(ctx context.Context) {
	s.mutex.Lock()
	ready, head := s.windowReady, s.windowHead
	s.mutex.Unlock()

	if !ready {
		latest, err := s.source.LatestSeq(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("label stream: reading the latest label seq failed", slog.String("error", err.Error()))
			}
			return
		}
		s.mutex.Lock()
		if ctx.Err() != nil {
			s.mutex.Unlock()
			return
		}
		s.windowReady, s.windowBase, s.windowHead = true, latest, latest
		s.notifyLocked()
		s.mutex.Unlock()
		head = latest
	}

	for {
		page, err := s.source.LabelsAfter(ctx, head, s.pageSize)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("label stream: polling for new labels failed", slog.String("error", err.Error()))
			}
			return
		}
		if len(page) == 0 {
			return
		}
		head = page[len(page)-1].Seq

		s.mutex.Lock()
		if ctx.Err() != nil {
			s.mutex.Unlock()
			return
		}
		s.window = append(s.window, page...)
		if excess := len(s.window) - liveWindowCapacity; excess > 0 {
			s.windowBase = s.window[excess-1].Seq
			s.window = slices.Clone(s.window[excess:])
		}
		s.windowHead = head
		s.notifyLocked()
		s.mutex.Unlock()

		if len(page) < s.pageSize {
			return
		}
	}
}

func (s *Stream) notifyLocked() {
	close(s.advanced)
	s.advanced = make(chan struct{})
}

// Subscription is one subscriber's position in the label log.
type Subscription struct {
	stream    *Stream
	ctx       context.Context
	cancel    context.CancelFunc
	delivered int64     // seq of the last label handed to send; owned by Run
	closer    io.Closer // guarded by stream.mutex
}

// Run delivers labels in seq order, one send call per label, until send
// fails, the source fails, or the subscription or its Stream is closed; the
// last two return nil. closer is closed when the Stream closes, so a send
// blocked on its peer returns. Run closes the subscription when it returns.
func (sub *Subscription) Run(send func(Label) error, closer io.Closer) error {
	defer sub.Close()

	sub.stream.mutex.Lock()
	if sub.stream.closed {
		sub.stream.mutex.Unlock()
		_ = closer.Close()
		return nil
	}
	sub.closer = closer
	sub.stream.mutex.Unlock()

	for {
		page, advanced, err := sub.nextPage()
		if sub.ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		for _, label := range page {
			if err := send(label); err != nil {
				return err
			}
			sub.delivered = label.Seq
		}
		if len(page) > 0 {
			continue
		}
		// Caught up: wait for the window to move, retrying the source after a
		// poll interval in case the poller has not started its window yet.
		timer := time.NewTimer(sub.stream.pollInterval)
		select {
		case <-advanced:
		case <-timer.C:
		case <-sub.ctx.Done():
		}
		timer.Stop()
	}
}

// nextPage returns the next labels after the last delivered seq: from the
// window when it covers that seq, otherwise from the source. advanced is
// closed when the window next changes.
func (sub *Subscription) nextPage() (page []Label, advanced <-chan struct{}, err error) {
	stream := sub.stream
	stream.mutex.Lock()
	advanced = stream.advanced
	if stream.windowReady && sub.delivered >= stream.windowBase {
		start := sort.Search(len(stream.window), func(index int) bool {
			return stream.window[index].Seq > sub.delivered
		})
		end := min(start+stream.pageSize, len(stream.window))
		page = slices.Clone(stream.window[start:end])
		stream.mutex.Unlock()
		return page, advanced, nil
	}
	stream.mutex.Unlock()

	page, err = stream.source.LabelsAfter(sub.ctx, sub.delivered, stream.pageSize)
	if err != nil {
		return nil, advanced, fmt.Errorf("reading labels after %d: %w", sub.delivered, err)
	}
	return page, advanced, nil
}

// Close ends the subscription and frees its slot. Closing the last open
// subscription stops the shared poller and empties the window. It is safe to
// call more than once and from any goroutine.
func (sub *Subscription) Close() {
	sub.cancel()
	stream := sub.stream
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	if _, open := stream.subscriptions[sub]; !open {
		return
	}
	delete(stream.subscriptions, sub)
	if len(stream.subscriptions) == 0 && stream.stopPoller != nil {
		stream.stopPoller()
		stream.stopPoller = nil
		stream.windowReady, stream.windowBase, stream.windowHead = false, 0, 0
		stream.window = nil
		stream.notifyLocked()
	}
}
