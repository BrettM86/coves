package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- Test doubles ---

// fakeEventHandler records handled events and can simulate transient,
// always-failing (retry-exhausting), or permanent (ErrPermanentEvent)
// failures per event time_us.
type fakeEventHandler struct {
	mu                   sync.Mutex
	events               []*JetstreamEvent
	callsByTimeUS        map[int64]int // total HandleEvent invocations per time_us
	failuresByTimeUS     map[int64]int // remaining transient failures per time_us
	alwaysFailTimeUS     map[int64]bool
	permanentErrorTimeUS map[int64]bool // fail with ErrPermanentEvent
}

func newFakeEventHandler() *fakeEventHandler {
	return &fakeEventHandler{
		callsByTimeUS:        make(map[int64]int),
		failuresByTimeUS:     make(map[int64]int),
		alwaysFailTimeUS:     make(map[int64]bool),
		permanentErrorTimeUS: make(map[int64]bool),
	}
}

func (h *fakeEventHandler) HandleEvent(_ context.Context, event *JetstreamEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.callsByTimeUS[event.TimeUS]++
	if h.permanentErrorTimeUS[event.TimeUS] {
		return fmt.Errorf("%w: bad record", ErrPermanentEvent)
	}
	if h.alwaysFailTimeUS[event.TimeUS] {
		return errors.New("handler failure that never clears")
	}
	if remaining := h.failuresByTimeUS[event.TimeUS]; remaining > 0 {
		h.failuresByTimeUS[event.TimeUS] = remaining - 1
		return errors.New("transient handler failure")
	}
	h.events = append(h.events, event)
	return nil
}

func (h *fakeEventHandler) handledCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

func (h *fakeEventHandler) calls(timeUS int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callsByTimeUS[timeUS]
}

// fakeCursorStore is an in-memory CursorStore.
type fakeCursorStore struct {
	mu      sync.Mutex
	cursors map[string]int64
}

func newFakeCursorStore() *fakeCursorStore {
	return &fakeCursorStore{cursors: make(map[string]int64)}
}

func (s *fakeCursorStore) GetCursor(_ context.Context, consumerName string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[consumerName], nil
}

func (s *fakeCursorStore) SaveCursor(_ context.Context, consumerName string, cursorTimeUS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursorTimeUS > s.cursors[consumerName] {
		s.cursors[consumerName] = cursorTimeUS
	}
	return nil
}

func (s *fakeCursorStore) get(consumerName string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[consumerName]
}

// fakeDeadLetterQueue is an in-memory DeadLetterQueue.
type fakeDeadLetterQueue struct {
	mu     sync.Mutex
	nextID int64
	rows   []DeadLetterEvent
	addErr error // injected failure for AddDeadLetter
}

func newFakeDeadLetterQueue() *fakeDeadLetterQueue {
	return &fakeDeadLetterQueue{nextID: 1}
}

func (q *fakeDeadLetterQueue) AddDeadLetter(_ context.Context, consumerName string, eventTimeUS int64, eventData []byte, handleErr string, redriveAttempts int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.addErr != nil {
		return q.addErr
	}
	// Mirror the store's dedup index: re-adding an already-captured event
	// is a no-op success.
	for _, row := range q.rows {
		if row.ConsumerName == consumerName && row.EventTimeUS == eventTimeUS && bytes.Equal(row.EventData, eventData) {
			return nil
		}
	}
	q.rows = append(q.rows, DeadLetterEvent{
		ID:           q.nextID,
		ConsumerName: consumerName,
		EventTimeUS:  eventTimeUS,
		EventData:    append([]byte(nil), eventData...),
		LastError:    handleErr,
		Attempts:     redriveAttempts,
		CreatedAt:    time.Now(),
	})
	q.nextID++
	return nil
}

func (q *fakeDeadLetterQueue) ListRetryable(_ context.Context, consumerName string, maxAttempts, limit int) ([]DeadLetterEvent, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var result []DeadLetterEvent
	for _, row := range q.rows {
		if row.ConsumerName == consumerName && row.Attempts < maxAttempts && len(result) < limit {
			result = append(result, row)
		}
	}
	return result, nil
}

func (q *fakeDeadLetterQueue) DeleteDeadLetter(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, row := range q.rows {
		if row.ID == id {
			q.rows = append(q.rows[:i], q.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func (q *fakeDeadLetterQueue) MarkRedriveAttempt(_ context.Context, id int64, handleErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].ID == id {
			q.rows[i].Attempts++
			q.rows[i].LastError = handleErr
			return nil
		}
	}
	return nil
}

func (q *fakeDeadLetterQueue) RetireDeadLetter(_ context.Context, id int64, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].ID == id {
			if q.rows[i].Attempts < MaxRedriveAttempts {
				q.rows[i].Attempts = MaxRedriveAttempts
			}
			q.rows[i].LastError = reason
			return nil
		}
	}
	return nil
}

func (q *fakeDeadLetterQueue) CountDeadLetters(_ context.Context) (map[string]int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	counts := make(map[string]int64)
	for _, row := range q.rows {
		counts[row.ConsumerName]++
	}
	return counts, nil
}

func (q *fakeDeadLetterQueue) rowCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.rows)
}

func (q *fakeDeadLetterQueue) row(i int) DeadLetterEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.rows[i]
}

// --- WebSocket test server ---

// jetstreamTestServer is a fake Jetstream endpoint. Each connection records
// the cursor query parameter and sends the configured messages. When
// holdOpen is true (the default) it then keeps the connection open until
// the client disconnects; when false it closes immediately after sending,
// forcing the client to reconnect.
type jetstreamTestServer struct {
	server   *httptest.Server
	holdOpen bool
	mu       sync.Mutex
	cursors  []string // cursor param per ACCEPTED connection ("" if absent)
	messages [][]byte
	// refuseFirst dials are answered with an HTTP error instead of a
	// WebSocket upgrade, standing in for a Jetstream that is not up yet.
	refuseFirst int
	refused     int
}

func newJetstreamTestServer(t *testing.T, messages [][]byte) *jetstreamTestServer {
	t.Helper()
	return newJetstreamTestServerWithHold(t, messages, true)
}

// newClosingJetstreamTestServer sends its messages and then closes each
// connection, so every batch of messages is followed by a client reconnect.
func newClosingJetstreamTestServer(t *testing.T, messages [][]byte) *jetstreamTestServer {
	t.Helper()
	return newJetstreamTestServerWithHold(t, messages, false)
}

// newRefusingJetstreamTestServer answers the first refuseFirst dials with an
// HTTP error — no WebSocket upgrade, so the client's dial itself fails — and
// serves normally from then on. It stands in for a Jetstream that has not
// finished booting, which is what a connector meets on a cold stack.
func newRefusingJetstreamTestServer(t *testing.T, messages [][]byte, refuseFirst int) *jetstreamTestServer {
	t.Helper()
	ts := newJetstreamTestServerWithHold(t, messages, true)
	ts.mu.Lock()
	ts.refuseFirst = refuseFirst
	ts.mu.Unlock()
	return ts
}

func newJetstreamTestServerWithHold(t *testing.T, messages [][]byte, holdOpen bool) *jetstreamTestServer {
	t.Helper()
	ts := &jetstreamTestServer{messages: messages, holdOpen: holdOpen}
	upgrader := websocket.Upgrader{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		if ts.refused < ts.refuseFirst {
			ts.refused++
			ts.mu.Unlock()
			http.Error(w, "jetstream is not accepting connections", http.StatusServiceUnavailable)
			return
		}
		ts.cursors = append(ts.cursors, r.URL.Query().Get("cursor"))
		ts.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for _, message := range ts.messages {
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
		if !ts.holdOpen {
			return // deferred Close drops the connection after sending
		}
		// Hold the connection open until the client goes away.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *jetstreamTestServer) wsURL() string {
	return "ws" + strings.TrimPrefix(ts.server.URL, "http") + "/subscribe?wantedCollections=social.coves.test"
}

func (ts *jetstreamTestServer) connectionCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.cursors)
}

func (ts *jetstreamTestServer) cursorForConnection(i int) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.cursors[i]
}

func (ts *jetstreamTestServer) refusedCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.refused
}

// --- Helpers ---

func testEventJSON(t *testing.T, timeUS int64) []byte {
	t.Helper()
	event := JetstreamEvent{
		Did:    "did:plc:connectortest",
		Kind:   "commit",
		TimeUS: timeUS,
		Commit: &CommitEvent{
			Operation:  "create",
			Collection: "social.coves.test",
			RKey:       fmt.Sprintf("rkey%d", timeUS),
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal test event: %v", err)
	}
	return data
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", description)
}

// fastConnectorOptions keeps test runtimes low.
func fastConnectorOptions(extra ...ConnectorOption) []ConnectorOption {
	opts := []ConnectorOption{
		WithReconnectDelay(20 * time.Millisecond),
		WithCursorFlushInterval(20 * time.Millisecond),
		WithHandlerRetryDelays([]time.Duration{time.Millisecond, time.Millisecond}),
	}
	return append(opts, extra...)
}

func startConnector(t *testing.T, connector *Connector) (cancel context.CancelFunc, done chan struct{}) {
	t.Helper()
	ctx, cancelFunc := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_ = connector.Start(ctx)
	}()
	t.Cleanup(func() {
		cancelFunc()
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			t.Error("connector did not stop within 5s")
		}
	})
	return cancelFunc, doneCh
}

// --- Tests ---

func TestConnector_ResumesFromPersistedCursor(t *testing.T) {
	server := newJetstreamTestServer(t, nil)
	handler := newFakeEventHandler()
	cursorStore := newFakeCursorStore()
	// Persisted cursor: 100 seconds in microseconds.
	persistedCursor := int64(100_000_000)
	cursorStore.cursors["test-consumer"] = persistedCursor

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(cursorStore))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "first connection", func() bool {
		return server.connectionCount() >= 1
	})

	// The dial must include the cursor rewound by 5s (5,000,000 µs) for
	// gapless playback.
	expected := fmt.Sprintf("%d", persistedCursor-5_000_000)
	if got := server.cursorForConnection(0); got != expected {
		t.Errorf("expected cursor query param %s, got %q", expected, got)
	}
}

func TestConnector_FirstRunLiveTailsWithoutCursor(t *testing.T) {
	server := newJetstreamTestServer(t, nil)
	connector := NewConnector("test-consumer", server.wsURL(), newFakeEventHandler(),
		fastConnectorOptions(WithCursorStore(newFakeCursorStore()))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "first connection", func() bool {
		return server.connectionCount() >= 1
	})

	if got := server.cursorForConnection(0); got != "" {
		t.Errorf("expected no cursor param on first run, got %q", got)
	}
}

func TestConnector_ProcessesEventsAndPersistsCursor(t *testing.T) {
	events := [][]byte{
		testEventJSON(t, 1_000),
		testEventJSON(t, 2_000),
		testEventJSON(t, 3_000),
	}
	server := newJetstreamTestServer(t, events)
	handler := newFakeEventHandler()
	cursorStore := newFakeCursorStore()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(cursorStore), WithDeadLetterWriter(newFakeDeadLetterQueue()))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "all events handled", func() bool {
		return handler.handledCount() == 3
	})
	waitFor(t, 2*time.Second, "cursor persisted", func() bool {
		return cursorStore.get("test-consumer") == 3_000
	})

	status := connector.Status()
	if status.EventsProcessed != 3 {
		t.Errorf("expected 3 events processed in status, got %d", status.EventsProcessed)
	}
	if status.CursorTimeUS != 3_000 {
		t.Errorf("expected in-memory cursor 3000, got %d", status.CursorTimeUS)
	}
}

func TestConnector_RetriesTransientHandlerFailure(t *testing.T) {
	server := newJetstreamTestServer(t, [][]byte{testEventJSON(t, 5_000)})
	handler := newFakeEventHandler()
	handler.failuresByTimeUS[5_000] = 2 // fail twice, succeed on third attempt
	deadLetters := newFakeDeadLetterQueue()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(newFakeCursorStore()), WithDeadLetterWriter(deadLetters))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "event handled after retries", func() bool {
		return handler.handledCount() == 1
	})
	if deadLetters.rowCount() != 0 {
		t.Errorf("expected no dead letters for a transient failure, got %d", deadLetters.rowCount())
	}
}

func TestConnector_DeadLettersAfterRetryExhaustion(t *testing.T) {
	events := [][]byte{
		testEventJSON(t, 7_000), // permanently failing
		testEventJSON(t, 8_000), // healthy
	}
	server := newJetstreamTestServer(t, events)
	handler := newFakeEventHandler()
	handler.alwaysFailTimeUS[7_000] = true
	cursorStore := newFakeCursorStore()
	deadLetters := newFakeDeadLetterQueue()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(cursorStore), WithDeadLetterWriter(deadLetters))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "failing event dead-lettered", func() bool {
		return deadLetters.rowCount() == 1
	})
	waitFor(t, 2*time.Second, "healthy event still handled", func() bool {
		return handler.handledCount() == 1
	})
	// The cursor must advance past the dead-lettered event: it is safe in
	// the DLQ, so it must not be replayed forever.
	waitFor(t, 2*time.Second, "cursor advanced past dead-lettered event", func() bool {
		return cursorStore.get("test-consumer") == 8_000
	})

	deadLetter := deadLetters.row(0)
	if deadLetter.EventTimeUS != 7_000 {
		t.Errorf("expected dead letter time_us 7000, got %d", deadLetter.EventTimeUS)
	}
	var event JetstreamEvent
	if err := json.Unmarshal(deadLetter.EventData, &event); err != nil {
		t.Fatalf("dead letter payload is not replayable JSON: %v", err)
	}
	if event.TimeUS != 7_000 {
		t.Errorf("dead letter payload has wrong time_us: %d", event.TimeUS)
	}

	// Dead-lettered events must not count as processed throughput: only the
	// healthy event does. A 100%-failing consumer must not graph as healthy.
	status := connector.Status()
	if status.EventsProcessed != 1 {
		t.Errorf("expected 1 event processed (dead-lettered event excluded), got %d", status.EventsProcessed)
	}
	if status.EventsDeadLettered != 1 {
		t.Errorf("expected 1 event dead-lettered, got %d", status.EventsDeadLettered)
	}
}

func TestConnector_DeadLetterWriteFailureDoesNotAdvanceCursor(t *testing.T) {
	server := newJetstreamTestServer(t, [][]byte{testEventJSON(t, 9_000)})
	handler := newFakeEventHandler()
	handler.alwaysFailTimeUS[9_000] = true
	cursorStore := newFakeCursorStore()
	deadLetters := newFakeDeadLetterQueue()
	deadLetters.addErr = errors.New("postgres is down")

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(cursorStore), WithDeadLetterWriter(deadLetters))...)
	startConnector(t, connector)

	// The connector must tear down the connection and reconnect (replaying
	// from the persisted cursor) rather than advance past a lost event.
	waitFor(t, 2*time.Second, "connection torn down and retried", func() bool {
		return server.connectionCount() >= 2
	})
	if got := cursorStore.get("test-consumer"); got != 0 {
		t.Errorf("cursor must not advance past an event that could not be dead-lettered, got %d", got)
	}
}

func TestConnector_MalformedEventIsDeadLettered(t *testing.T) {
	events := [][]byte{
		[]byte("this is not json"),
		testEventJSON(t, 11_000),
	}
	server := newJetstreamTestServer(t, events)
	handler := newFakeEventHandler()
	deadLetters := newFakeDeadLetterQueue()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(newFakeCursorStore()), WithDeadLetterWriter(deadLetters))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "malformed event dead-lettered", func() bool {
		return deadLetters.rowCount() == 1
	})
	waitFor(t, 2*time.Second, "valid event still handled", func() bool {
		return handler.handledCount() == 1
	})
	if got := string(deadLetters.row(0).EventData); got != "this is not json" {
		t.Errorf("expected raw payload preserved in dead letter, got %q", got)
	}
}

func TestConnector_ShutdownFlushesCursor(t *testing.T) {
	server := newJetstreamTestServer(t, [][]byte{testEventJSON(t, 42_000)})
	handler := newFakeEventHandler()
	cursorStore := newFakeCursorStore()

	// Flush interval far larger than the test: only the shutdown flush can
	// persist the cursor.
	connector := NewConnector("test-consumer", server.wsURL(), handler,
		WithReconnectDelay(20*time.Millisecond),
		WithCursorFlushInterval(time.Hour),
		WithHandlerRetryDelays([]time.Duration{time.Millisecond}),
		WithCursorStore(cursorStore),
		WithDeadLetterWriter(newFakeDeadLetterQueue()),
	)
	cancel, done := startConnector(t, connector)

	waitFor(t, 2*time.Second, "event handled", func() bool {
		return handler.handledCount() == 1
	})
	if got := cursorStore.get("test-consumer"); got != 0 {
		t.Fatalf("cursor unexpectedly persisted before shutdown: %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connector did not stop")
	}

	if got := cursorStore.get("test-consumer"); got != 42_000 {
		t.Errorf("expected shutdown to flush cursor 42000, got %d", got)
	}
}

func TestDeadLetterRedriver(t *testing.T) {
	ctx := context.Background()
	queue := newFakeDeadLetterQueue()
	handler := newFakeEventHandler()
	handler.alwaysFailTimeUS[2_000] = true

	// Row 1: replayable and now healthy → redriven and deleted.
	// Row 2: still failing → attempts incremented, row kept.
	// Row 3: unparseable → retired in one step (can never succeed), row kept.
	mustAdd := func(timeUS int64, data []byte) {
		if err := queue.AddDeadLetter(ctx, "test-consumer", timeUS, data, "original failure", 0); err != nil {
			t.Fatalf("failed to seed dead letter: %v", err)
		}
	}
	mustAdd(1_000, testEventJSON(t, 1_000))
	mustAdd(2_000, testEventJSON(t, 2_000))
	mustAdd(0, []byte("not json"))

	redriver := NewDeadLetterRedriver(queue, map[string]EventHandler{"test-consumer": handler})
	redriver.redriveAll(ctx)

	if handler.handledCount() != 1 {
		t.Errorf("expected exactly 1 event redriven successfully, got %d", handler.handledCount())
	}
	if queue.rowCount() != 2 {
		t.Fatalf("expected 2 rows remaining after redrive, got %d", queue.rowCount())
	}
	for i := 0; i < queue.rowCount(); i++ {
		row := queue.row(i)
		switch {
		case row.EventTimeUS == 1_000:
			t.Errorf("successfully redriven event was not deleted")
		case row.EventTimeUS == 2_000 && row.Attempts != 1:
			t.Errorf("expected 1 redrive attempt on still-failing row, got %d", row.Attempts)
		case row.EventTimeUS == 0 && row.Attempts != MaxRedriveAttempts:
			t.Errorf("expected unparseable row retired at %d attempts, got %d", MaxRedriveAttempts, row.Attempts)
		}
	}

	// Exhausted rows stop being retried: burn the budget and verify the
	// next pass touches nothing.
	for i := 0; i < 20; i++ {
		redriver.redriveAll(ctx)
	}
	if queue.rowCount() != 2 {
		t.Fatalf("expected exhausted rows to remain for manual inspection, got %d", queue.rowCount())
	}
	for i := 0; i < queue.rowCount(); i++ {
		if got := queue.row(i).Attempts; got > redriver.maxAttempts {
			t.Errorf("row exceeded redrive budget: %d attempts (max %d)", got, redriver.maxAttempts)
		}
	}
}

func TestConnector_PermanentFailureSkipsRetriesAndExhaustsRedrive(t *testing.T) {
	events := [][]byte{
		testEventJSON(t, 13_000), // permanently rejected (ErrPermanentEvent)
		testEventJSON(t, 14_000), // healthy
	}
	server := newJetstreamTestServer(t, events)
	handler := newFakeEventHandler()
	handler.permanentErrorTimeUS[13_000] = true
	cursorStore := newFakeCursorStore()
	deadLetters := newFakeDeadLetterQueue()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(cursorStore), WithDeadLetterWriter(deadLetters))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "permanent event dead-lettered", func() bool {
		return deadLetters.rowCount() == 1
	})
	waitFor(t, 2*time.Second, "healthy event still handled", func() bool {
		return handler.handledCount() == 1
	})
	waitFor(t, 2*time.Second, "cursor advanced past permanent event", func() bool {
		return cursorStore.get("test-consumer") == 14_000
	})

	// Permanent = pointless to retry: exactly one handler invocation.
	if got := handler.calls(13_000); got != 1 {
		t.Errorf("permanent failure must not be retried: got %d handler invocations", got)
	}

	// The row is dead-lettered already exhausted so the redriver skips it.
	row := deadLetters.row(0)
	if row.EventTimeUS != 13_000 {
		t.Fatalf("expected dead letter time_us 13000, got %d", row.EventTimeUS)
	}
	if row.Attempts != MaxRedriveAttempts {
		t.Errorf("expected permanent dead letter inserted with attempts %d, got %d", MaxRedriveAttempts, row.Attempts)
	}
	retryable, err := deadLetters.ListRetryable(context.Background(), "test-consumer", MaxRedriveAttempts, 100)
	if err != nil {
		t.Fatalf("ListRetryable failed: %v", err)
	}
	if len(retryable) != 0 {
		t.Errorf("expected redriver to skip exhausted permanent dead letter, got %d retryable rows", len(retryable))
	}
}

// TestConnector_RetriesAfterFailedDial covers Start's dial-failure branch: a
// connect() error is recorded, slept on and dialled again, and the ONLY thing
// that ends the loop is context cancellation (connector.go's Start).
//
// Nothing else in this file reaches that branch. Both reconnect tests —
// ReconnectDialsWithAdvancedCursor and DeadLetterWriteFailureDoesNotAdvanceCursor
// — reconnect after a connection that SUCCEEDED and was then torn down, so a
// connector that returned on a failed dial would still pass them. What it would
// break is every AppView that starts before its Jetstream does: the consumer
// exits at boot, /health/consumers reports it disconnected forever, and no
// record is ever indexed again without a restart.
//
// It lives here, in the untagged unit build, because the retry loop is in this
// package and needs no infrastructure. It replaces the "Consumer retries on
// connection failure" subtest of the deleted tests/e2e/error_recovery_test.go,
// which pointed a connector at ws://invalid:9999 for three wall-clock seconds
// and then t.Logf'd whichever error came back — an assertion-free test that
// could not fail, in a tier whose rules forbid instantiating a consumer at all.
func TestConnector_RetriesAfterFailedDial(t *testing.T) {
	const refusals = 2
	server := newRefusingJetstreamTestServer(t, [][]byte{testEventJSON(t, 21_000)}, refusals)
	handler := newFakeEventHandler()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(newFakeCursorStore()), WithDeadLetterWriter(newFakeDeadLetterQueue()))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "every refused dial to be retried", func() bool {
		return server.refusedCount() == refusals
	})
	// The recovery, and the half that a give-up-on-first-error connector fails:
	// the endpoint came back and the event was consumed with no intervention.
	waitFor(t, 2*time.Second, "the event to be delivered once the endpoint accepted a dial", func() bool {
		return handler.handledCount() == 1
	})

	if got := server.connectionCount(); got != 1 {
		t.Errorf("expected exactly 1 accepted connection after %d refusals, got %d", refusals, got)
	}

	status := connector.Status()
	if !status.Connected {
		t.Error("the connector must report itself connected once a dial succeeded")
	}
	// lastError is never cleared, so this is deterministic once the refusals
	// above have been observed. It matters because a dial failure has no other
	// observable: /health/consumers is where an operator sees WHY a consumer
	// that is retrying has not connected yet.
	if status.LastError == "" {
		t.Error("a refused dial must be recorded as the connector's last error, or a connector " +
			"stuck retrying an unreachable endpoint reports no reason at all")
	}
}

func TestConnector_StartCalledTwiceReturnsError(t *testing.T) {
	server := newJetstreamTestServer(t, nil)
	connector := NewConnector("test-consumer", server.wsURL(), newFakeEventHandler(),
		fastConnectorOptions()...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "first connection", func() bool {
		return server.connectionCount() >= 1
	})

	err := connector.Start(context.Background())
	if err == nil {
		t.Fatal("expected second Start call to return an error")
	}
	if !strings.Contains(err.Error(), "Start called twice") {
		t.Errorf("expected 'Start called twice' error, got: %v", err)
	}
}

func TestConnector_ReconnectDialsWithAdvancedCursor(t *testing.T) {
	// Realistic time_us values so the 5s rewind does not clamp to zero.
	lastEventTimeUS := int64(100_100_000)
	events := [][]byte{
		testEventJSON(t, 100_000_000),
		testEventJSON(t, lastEventTimeUS),
	}
	// The server closes each connection after sending, forcing a reconnect.
	server := newClosingJetstreamTestServer(t, events)
	handler := newFakeEventHandler()

	connector := NewConnector("test-consumer", server.wsURL(), handler,
		fastConnectorOptions(WithCursorStore(newFakeCursorStore()), WithDeadLetterWriter(newFakeDeadLetterQueue()))...)
	startConnector(t, connector)

	waitFor(t, 2*time.Second, "reconnect after server close", func() bool {
		return server.connectionCount() >= 2
	})

	if got := server.cursorForConnection(0); got != "" {
		t.Errorf("expected no cursor param on first connection, got %q", got)
	}
	// The reconnect must dial from the advanced IN-MEMORY cursor (last
	// processed event) minus the 5s rewind — not from the persisted cursor,
	// which may lag behind.
	expected := strconv.FormatInt(lastEventTimeUS-5_000_000, 10)
	if got := server.cursorForConnection(1); got != expected {
		t.Errorf("expected reconnect cursor %s, got %q", expected, got)
	}
}
