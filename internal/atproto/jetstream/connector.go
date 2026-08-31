package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// EventHandler processes a single Jetstream event. All event consumers
// (users, communities, posts, votes, comments, aggregators) implement this.
// Handlers MUST be idempotent: cursor-based reconnects intentionally replay
// a few seconds of already-processed events.
type EventHandler interface {
	HandleEvent(ctx context.Context, event *JetstreamEvent) error
}

// CursorStore persists the per-consumer Jetstream cursor (time_us of the
// last fully processed event) so consumers resume where they left off
// instead of at the live tail.
type CursorStore interface {
	// GetCursor returns the persisted cursor for the consumer, or 0 if the
	// consumer has never persisted one (first run → live tail).
	GetCursor(ctx context.Context, consumerName string) (int64, error)
	// SaveCursor upserts the cursor for the consumer.
	SaveCursor(ctx context.Context, consumerName string, cursorTimeUS int64) error
}

// DeadLetterWriter captures events that failed all in-line retries so they
// can be replayed later instead of being silently dropped.
type DeadLetterWriter interface {
	// AddDeadLetter stores the raw event bytes. redriveAttempts seeds the
	// redrive budget: 0 for infrastructure failures, MaxRedriveAttempts minus
	// UnresolvedRedriveAttempts for unresolved references, and
	// MaxRedriveAttempts for permanent failures. Re-adding an already-captured event (same consumer,
	// time_us, and payload) must succeed as a no-op so the cursor can
	// advance past it.
	AddDeadLetter(ctx context.Context, consumerName string, eventTimeUS int64, eventData []byte, handleErr string, redriveAttempts int) error
}

// ConnectorStatus is a point-in-time snapshot of a connector's health,
// exposed via the /health/consumers endpoint.
type ConnectorStatus struct {
	Name                  string     `json:"name"`
	Connected             bool       `json:"connected"`
	ConnectedSince        *time.Time `json:"connectedSince,omitempty"`
	DisconnectedSince     *time.Time `json:"disconnectedSince,omitempty"`
	LastEventAt           *time.Time `json:"lastEventAt,omitempty"`
	CursorTimeUS          int64      `json:"cursorTimeUs"`
	PersistedCursorTimeUS int64      `json:"persistedCursorTimeUs"`
	EventsProcessed       uint64     `json:"eventsProcessed"`
	EventsDeadLettered    uint64     `json:"eventsDeadLettered"`
	// LastError and LastErrorAt are excluded from JSON on purpose: the
	// /health/consumers endpoint is unauthenticated and raw error strings
	// leak internal details (hosts, SQL fragments, file paths). In-process
	// callers still read them from the struct.
	LastError   string     `json:"-"`
	LastErrorAt *time.Time `json:"-"`
}

// Connector maintains a WebSocket connection to Jetstream and feeds events
// to an EventHandler. One Connector replaces the previously duplicated
// per-consumer connectors and adds the reliability guarantees they lacked:
//
//   - Cursor persistence: reconnects (and restarts) resume from the last
//     processed time_us minus a small rewind, so no events are lost during
//     gaps. Requires idempotent handlers, which all consumers already are.
//   - Retry then dead-letter: a handler error is retried in-line with
//     backoff; if it still fails the raw event is written to the dead
//     letter queue and the cursor advances. If even the dead letter write
//     fails (e.g. Postgres is down) the connection is dropped WITHOUT
//     advancing the cursor, so the event is replayed on reconnect.
//   - Graceful shutdown: cancelling the Start context unblocks the read
//     loop, waits for the in-flight handler, and flushes the cursor.
type Connector struct {
	name        string
	wsURL       string
	handler     EventHandler
	cursorStore CursorStore
	deadLetters DeadLetterWriter

	reconnectDelay      time.Duration
	cursorFlushInterval time.Duration
	cursorRewind        time.Duration
	retryDelays         []time.Duration

	started atomic.Bool // guards against double Start invocation

	mu                 sync.Mutex
	cursorLoaded       bool
	cursorTimeUS       int64 // last fully processed event (in-memory)
	persistedTimeUS    int64 // last value written to the CursorStore
	connected          bool
	connectedSince     time.Time
	disconnectedSince  time.Time
	lastEventAt        time.Time
	eventsProcessed    uint64
	eventsDeadLettered uint64
	lastError          string
	lastErrorAt        time.Time
}

// maxWebSocketFrameBytes bounds a single WebSocket frame read from
// Jetstream (16 MiB). Without a limit the endpoint could force unbounded
// allocation; the largest plausible Jetstream event — a commit with a full
// record embedded — is well under this.
const maxWebSocketFrameBytes = 16 << 20

// ConnectorOption configures a Connector.
type ConnectorOption func(*Connector)

// WithCursorStore enables cursor persistence. Without it the connector
// live-tails (previous behavior), which is only appropriate in tests.
func WithCursorStore(store CursorStore) ConnectorOption {
	return func(c *Connector) { c.cursorStore = store }
}

// WithDeadLetterWriter enables the dead letter queue for events that fail
// all in-line retries. Without it, exhausted events are logged and dropped.
func WithDeadLetterWriter(writer DeadLetterWriter) ConnectorOption {
	return func(c *Connector) { c.deadLetters = writer }
}

// WithReconnectDelay overrides the delay between reconnect attempts.
func WithReconnectDelay(d time.Duration) ConnectorOption {
	return func(c *Connector) { c.reconnectDelay = d }
}

// WithCursorFlushInterval overrides how often the in-memory cursor is
// persisted to the CursorStore.
func WithCursorFlushInterval(d time.Duration) ConnectorOption {
	return func(c *Connector) { c.cursorFlushInterval = d }
}

// WithHandlerRetryDelays overrides the in-line retry schedule for handler
// errors. len(delays)+1 total attempts are made.
func WithHandlerRetryDelays(delays []time.Duration) ConnectorOption {
	return func(c *Connector) { c.retryDelays = delays }
}

// NewConnector creates a Jetstream connector for the named consumer.
// The name keys the persisted cursor and dead letter rows, so it must be
// stable across releases.
func NewConnector(name, wsURL string, handler EventHandler, opts ...ConnectorOption) *Connector {
	c := &Connector{
		name:                name,
		wsURL:               wsURL,
		handler:             handler,
		reconnectDelay:      5 * time.Second,
		cursorFlushInterval: 5 * time.Second,
		// Jetstream recommends reconnecting a few seconds before the last
		// received event to guarantee gapless playback; handlers are
		// idempotent so the overlap is harmless.
		cursorRewind: 5 * time.Second,
		retryDelays:  []time.Duration{200 * time.Millisecond, 1 * time.Second, 3 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Start runs the connector until ctx is cancelled, reconnecting on errors.
// The persisted cursor (if any) is loaded before the first connection.
// Start may be called at most once per Connector: a second call would race
// two read loops over the same cursor, so it returns an error immediately.
func (c *Connector) Start(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return fmt.Errorf("connector %s: Start called twice", c.name)
	}

	log.Printf("Starting Jetstream %s consumer: %s", c.name, c.wsURL)

	// Disconnected-since-boot: a consumer that never achieves its first
	// connection must still surface as stalled in /health/consumers.
	c.setConnected(false)

	// Flush the cursor one final time on the way out, even on cancellation.
	defer c.flushCursorOnShutdown()

	if c.cursorStore != nil {
		go c.runCursorFlusher(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Jetstream %s consumer shutting down", c.name)
			return ctx.Err()
		default:
		}

		// Cursor load is inside the loop so a transient DB failure at boot
		// retries instead of permanently degrading to live-tail.
		if err := c.loadCursorOnce(ctx); err != nil {
			c.recordError(fmt.Errorf("failed to load cursor: %w", err))
			slog.Error("jetstream consumer failed to load cursor; retrying",
				slog.String("consumer", c.name), slog.String("error", err.Error()))
			sleepCtx(ctx, c.reconnectDelay)
			continue
		}

		if err := c.connect(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				continue // loop re-checks ctx and exits cleanly
			}
			c.recordError(err)
			log.Printf("Jetstream %s connection error: %v. Retrying in %s...", c.name, err, c.reconnectDelay)
			sleepCtx(ctx, c.reconnectDelay)
		}
	}
}

// Status returns a snapshot of the connector's health.
func (c *Connector) Status() ConnectorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	status := ConnectorStatus{
		Name:                  c.name,
		Connected:             c.connected,
		CursorTimeUS:          c.cursorTimeUS,
		PersistedCursorTimeUS: c.persistedTimeUS,
		EventsProcessed:       c.eventsProcessed,
		EventsDeadLettered:    c.eventsDeadLettered,
		LastError:             c.lastError,
	}
	if !c.connectedSince.IsZero() {
		t := c.connectedSince
		status.ConnectedSince = &t
	}
	if !c.disconnectedSince.IsZero() {
		t := c.disconnectedSince
		status.DisconnectedSince = &t
	}
	if !c.lastEventAt.IsZero() {
		t := c.lastEventAt
		status.LastEventAt = &t
	}
	if !c.lastErrorAt.IsZero() {
		t := c.lastErrorAt
		status.LastErrorAt = &t
	}
	return status
}

// loadCursorOnce loads the persisted cursor on the first (successful) call.
func (c *Connector) loadCursorOnce(ctx context.Context) error {
	c.mu.Lock()
	loaded := c.cursorLoaded
	c.mu.Unlock()
	if loaded || c.cursorStore == nil {
		return nil
	}

	cursor, err := c.cursorStore.GetCursor(ctx, c.name)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.cursorLoaded = true
	c.cursorTimeUS = cursor
	c.persistedTimeUS = cursor
	c.mu.Unlock()

	if cursor > 0 {
		log.Printf("Jetstream %s consumer resuming from cursor %d (%s)",
			c.name, cursor, time.UnixMicro(cursor).UTC().Format(time.RFC3339))
	}
	return nil
}

// dialURL returns the WebSocket URL, with the cursor query parameter set
// when a cursor is known. The cursor is rewound slightly for gapless
// playback per Jetstream's reconnection guidance.
func (c *Connector) dialURL() (string, error) {
	c.mu.Lock()
	cursor := c.cursorTimeUS
	c.mu.Unlock()

	if cursor <= 0 {
		return c.wsURL, nil
	}

	parsed, err := url.Parse(c.wsURL)
	if err != nil {
		return "", fmt.Errorf("invalid Jetstream URL %q: %w", c.wsURL, err)
	}
	rewound := cursor - c.cursorRewind.Microseconds()
	if rewound < 0 {
		rewound = 0
	}
	query := parsed.Query()
	query.Set("cursor", strconv.FormatInt(rewound, 10))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// connect establishes the WebSocket connection and processes events until
// the connection drops or ctx is cancelled.
func (c *Connector) connect(ctx context.Context) error {
	dialURL, err := c.dialURL()
	if err != nil {
		return err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, dialURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}

	c.setConnected(true)
	defer c.setConnected(false)
	defer func() {
		// The ctx-watcher goroutine below usually closes the connection
		// first; an already-closed error here is a clean shutdown, not a
		// failure worth logging.
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			log.Printf("Jetstream %s: failed to close WebSocket connection: %v", c.name, closeErr)
		}
	}()

	log.Printf("Connected to Jetstream (%s consumer)", c.name)

	// Cap frame size so a misbehaving or malicious endpoint cannot force
	// unbounded allocation. The largest plausible Jetstream event (a commit
	// carrying a full record) is well under this.
	conn.SetReadLimit(maxWebSocketFrameBytes)

	// Set read deadline to detect connection issues
	if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		log.Printf("Jetstream %s: failed to set read deadline: %v", c.name, err)
	}
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			log.Printf("Jetstream %s: failed to set read deadline in pong handler: %v", c.name, err)
		}
		return nil
	})

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }
	defer closeDone()

	// Unblock the read loop whenever the connection must die: on shutdown
	// (ctx cancelled) and on ping failure (done closed). ReadMessage only
	// returns when the connection closes, so without this a SIGTERM would
	// hang until the next network event and a ping failure would wait out
	// the full 60s read deadline before reconnecting.
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = conn.Close()
	}()

	// Ping goroutine keeps the connection alive.
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("Jetstream %s: failed to send ping: %v", c.name, err)
					closeDone()
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return fmt.Errorf("connection closed by ping failure")
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read error: %w", err)
		}

		// Reset read deadline on successful read
		if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			log.Printf("Jetstream %s: failed to set read deadline: %v", c.name, err)
		}

		if err := c.processMessage(ctx, message); err != nil {
			// Only fatal pipeline failures (dead letter write failed, ctx
			// cancelled) reach here. Drop the connection without advancing
			// the cursor so the event replays on reconnect.
			return err
		}
	}
}

// processMessage parses and handles one raw Jetstream message.
//
// Outcomes:
//   - handled successfully           → cursor advances, counts as processed
//   - failed all retries, dead-lettered → cursor advances (event is safe in
//     the DLQ) but does NOT count toward eventsProcessed
//   - permanent failure (ErrPermanentEvent) → dead-lettered already
//     exhausted (attempts = MaxRedriveAttempts), cursor advances
//   - unresolved reference (ErrUnresolvedReference) → no in-line retries,
//     dead-lettered with a small redrive budget, cursor advances; the
//     redriver converges it off the lane once the referenced record arrives
//   - dead letter write failed       → error returned, cursor does NOT advance
//   - unparseable JSON               → dead-lettered for forensics, cursor
//     unaffected; on reconnect the same frame may be dead-lettered again
//     until a later event advances the cursor — harmless, because the dedup
//     index makes the re-add a no-op instead of a fresh row.
func (c *Connector) processMessage(ctx context.Context, message []byte) error {
	var event JetstreamEvent
	if err := json.Unmarshal(message, &event); err != nil {
		slog.Error("jetstream event failed to parse; dead-lettering",
			slog.String("consumer", c.name), slog.String("error", err.Error()))
		c.recordError(fmt.Errorf("failed to parse event: %w", err))
		return c.deadLetter(ctx, 0, message, err, 0)
	}

	handleErr := c.handleWithRetry(ctx, &event)
	if handleErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Permanent failures are dead-lettered with their redrive budget
		// already exhausted: replaying a validation rejection can never
		// succeed, so the row is kept for forensics only.
		redriveAttempts := 0
		switch {
		case errors.Is(handleErr, ErrPermanentEvent):
			redriveAttempts = MaxRedriveAttempts
		case errors.Is(handleErr, ErrUnresolvedReference):
			redriveAttempts = MaxRedriveAttempts - UnresolvedRedriveAttempts
		}
		slog.Error("jetstream event failed; dead-lettering",
			slog.String("consumer", c.name),
			slog.String("did", event.Did),
			slog.String("kind", event.Kind),
			slog.Int64("time_us", event.TimeUS),
			slog.Bool("permanent", redriveAttempts == MaxRedriveAttempts),
			slog.Bool("unresolved_reference", errors.Is(handleErr, ErrUnresolvedReference)),
			slog.String("error", handleErr.Error()))
		c.recordError(handleErr)
		if err := c.deadLetter(ctx, event.TimeUS, message, handleErr, redriveAttempts); err != nil {
			return err
		}
		// Safe in the DLQ: advance the cursor, but do not count the event
		// as processed throughput.
		c.advanceCursor(event.TimeUS)
		return nil
	}

	c.recordProcessed(event.TimeUS)
	return nil
}

// handleWithRetry invokes the handler, retrying transient failures in-line.
// Errors wrapped with ErrPermanentEvent short-circuit immediately: retrying
// a permanent rejection can never succeed. Errors wrapped with
// ErrUnresolvedReference short-circuit too: the referenced record lives in
// another repo, and waiting 4.2 seconds of this lane for it is exactly the
// stall an adversary can mint for free by naming a record that does not
// exist. Those go straight to the dead letter queue with their redrive
// budget intact and converge on the redriver's timer instead.
//
// Retries block subsequent events on purpose: events within one consumer's
// stream are ordered (a record's create precedes its update/delete), so
// skipping ahead would apply them out of order. Dead-lettering trades this
// ordering away only after all retries fail.
func (c *Connector) handleWithRetry(ctx context.Context, event *JetstreamEvent) error {
	err := c.handler.HandleEvent(ctx, event)
	if err == nil || skipsInlineRetries(err) {
		return err
	}

	for attempt, delay := range c.retryDelays {
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
		if err = c.handler.HandleEvent(ctx, event); err == nil {
			return nil
		}
		// The permanent or unresolved-reference wrapping can also appear on
		// a later attempt (e.g. a different code path fails this time); stop
		// retrying as soon as the failure is known not to be worth the lane.
		if skipsInlineRetries(err) {
			return err
		}
		log.Printf("Jetstream %s: handler retry %d/%d failed: %v",
			c.name, attempt+1, len(c.retryDelays), err)
	}
	return err
}

// skipsInlineRetries reports whether a handler failure belongs to a class the
// connector must not spend lane time retrying: see the taxonomy in errors.go.
func skipsInlineRetries(err error) bool {
	return errors.Is(err, ErrPermanentEvent) || errors.Is(err, ErrUnresolvedReference)
}

// deadLetter writes a failed event to the dead letter queue. A nil writer
// preserves the old log-and-drop behavior (tests only). A write failure is
// returned to the caller, which tears down the connection WITHOUT advancing
// the in-memory cursor; the reconnect dials from that in-memory cursor (the
// persisted cursor only matters after a process restart), so the event is
// replayed instead of lost.
func (c *Connector) deadLetter(ctx context.Context, timeUS int64, message []byte, cause error, redriveAttempts int) error {
	if c.deadLetters == nil {
		slog.Error("jetstream event DROPPED: no dead letter writer configured",
			slog.String("consumer", c.name), slog.String("error", cause.Error()))
		return nil
	}
	if err := c.deadLetters.AddDeadLetter(ctx, c.name, timeUS, message, cause.Error(), redriveAttempts); err != nil {
		return fmt.Errorf("failed to dead-letter event (will replay from cursor): %w", err)
	}
	c.mu.Lock()
	c.eventsDeadLettered++
	c.mu.Unlock()
	return nil
}

// runCursorFlusher periodically persists the in-memory cursor.
func (c *Connector) runCursorFlusher(ctx context.Context) {
	ticker := time.NewTicker(c.cursorFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.flushCursor(ctx)
		}
	}
}

// flushCursor persists the cursor if it advanced since the last flush.
func (c *Connector) flushCursor(ctx context.Context) {
	if c.cursorStore == nil {
		return
	}
	c.mu.Lock()
	cursor := c.cursorTimeUS
	persisted := c.persistedTimeUS
	c.mu.Unlock()

	if cursor <= persisted {
		return
	}
	if err := c.cursorStore.SaveCursor(ctx, c.name, cursor); err != nil {
		slog.Error("failed to persist jetstream cursor",
			slog.String("consumer", c.name), slog.String("error", err.Error()))
		return
	}
	c.mu.Lock()
	if cursor > c.persistedTimeUS {
		c.persistedTimeUS = cursor
	}
	c.mu.Unlock()
}

// flushCursorOnShutdown persists the final cursor with a fresh context,
// because the Start context is already cancelled during shutdown.
func (c *Connector) flushCursorOnShutdown() {
	if c.cursorStore == nil {
		return
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.flushCursor(flushCtx)
}

func (c *Connector) setConnected(connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = connected
	if connected {
		c.connectedSince = time.Now()
		c.disconnectedSince = time.Time{}
	} else {
		c.disconnectedSince = time.Now()
		c.connectedSince = time.Time{}
	}
}

// advanceCursor moves the in-memory cursor and lastEventAt past an event
// that is fully accounted for (handled or safely dead-lettered) WITHOUT
// counting it as processed throughput — a 100%-failing consumer must not
// graph as healthy.
func (c *Connector) advanceCursor(timeUS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastEventAt = time.Now()
	// Replayed events from the reconnect rewind carry older time_us; never
	// regress the cursor.
	if timeUS > c.cursorTimeUS {
		c.cursorTimeUS = timeUS
	}
}

// recordProcessed advances the cursor for a successfully handled event and
// counts it toward eventsProcessed.
func (c *Connector) recordProcessed(timeUS int64) {
	c.advanceCursor(timeUS)
	c.mu.Lock()
	c.eventsProcessed++
	c.mu.Unlock()
}

func (c *Connector) recordError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err.Error()
	c.lastErrorAt = time.Now()
}

// sleepCtx sleeps for d unless ctx is cancelled first. Returns false on
// cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
