package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The one cursor-gated Jetstream subscriber.
//
// # What this is for, and what it is NOT for
//
// This is a T1 and debugging aid: consumer-plumbing tests that need to see a
// commit arrive, and a human asking "did the PDS actually emit that?". It is
// NOT the end-to-end mechanism. Pipeline contracts (docs/TEST_ARCHITECTURE.md
// §3.4, rule 1) never dial a websocket — the AppView container's own consumers
// do the consuming, exactly as deployed, and the test observes through serving
// endpoints. A T2 test that subscribes here is testing a socket it opened
// itself, which is the one thing production does not do.
//
// scripts/test-audit.sh counts websocket.DefaultDialer everywhere except this
// file, which is the mechanical form of that rule.
//
// # The race this API makes unrepresentable
//
// The ten hand-rolled copies this replaces all had the same shape: write to the
// PDS, then dial Jetstream, then wait for the event. A cursorless subscription
// only streams commits emitted after the socket is dialled, so that ordering
// races the PDS→Jetstream relay and silently drops the event under load. The
// copies papered over it with sleeps.
//
// NewFirehose captures a replay cursor at CONSTRUCTION and Await replays from
// it, so the correct ordering is the only one the API can express:
//
//	fh := testkit.NewFirehose(t, testkit.WithCollections(collection)) // cursor captured here
//	rec := account.CreateRecord(t, collection, payload)               // write
//	ev := fh.Await(t, 30*time.Second, testkit.MatchRecord(rec))       // dials, replays
//
// Jetstream stamps each event's time_us when it ingests the commit, which is
// necessarily after a write that had not happened when the cursor was taken. So
// the cursor is always below the event's time_us (we receive it) and above every
// earlier event's (no stale matches). Test and Jetstream share a host clock —
// the dev stack publishes on loopback, the CI stack shares a network namespace —
// so there is no skew to compensate for. Where that assumption fails it is
// reported rather than silently starving the wait: see the discarded-event
// accounting in render.
//
// The cursor is also a BOUND, not merely a starting hint: events older than it
// are discarded on arrival. A Jetstream subscribed "from now" on a quiet stream
// has been observed replaying its entire retained store, and a negative
// assertion ("no delete event arrived") written against an unbounded stream is
// answered by whatever a previous run left behind.
//
// # Feeding a real consumer
//
// The decoded Event carries only what a matcher needs. A test that wants to run
// a production consumer over the same event keeps the bytes:
//
//	var je jetstream.JetstreamEvent
//	require.NoError(t, ev.Into(&je))
//	require.NoError(t, consumer.HandleEvent(ctx, &je))
//
// which is what the ten copies did inline, and is why Event does not need to
// grow every field the consumers read.
//
// # Event types are declared here, not imported
//
// internal/atproto/jetstream owns JetstreamEvent, and testkit cannot import it:
// its consumers pull in communities, posts, comments, users, votes, userblocks
// and aggregators, and testkit importing anything under internal/core makes
// those packages' own tests an import cycle (see the package doc in testkit.go).
//
// The four structs below are therefore a second declaration of the same wire
// format. firehose_pin_test.go pins them against the real ones from an EXTERNAL
// test package, which may import jetstream without putting the cycle into
// testkit's own import graph.

const (
	// firehoseReadDeadline bounds a single websocket read. It has to be finite:
	// a blocking read on a connection whose peer died never returns, and the
	// test's own deadline would then be enforced by the go test timeout killing
	// the whole binary.
	//
	// It is a WINDOW, not a budget. Reaching it means "nothing arrived in five
	// seconds", which for a quiet stream is not an error at all — the wait
	// continues on a fresh connection until the caller's deadline.
	firehoseReadDeadline = 5 * time.Second

	// maxRecoveryFailures bounds consecutive FAILED recoveries — a dial that
	// errors, or a connection that breaks again as soon as it is established.
	// Once it trips, the subscription is reported as gone.
	//
	// It does not count quiet windows, and that distinction is the fix for a bug
	// this guard had in all ten copies it replaces. They answered a
	// read-deadline expiry with `continue`, and gorilla documents a connection
	// whose read deadline has been exceeded as CORRUPT — every subsequent read
	// fails instantly with the same timeout error. The counter therefore reached
	// ten within microseconds of the FIRST expiry, so every "wait up to 30
	// seconds for the event" subscription in the old suite actually gave up
	// after five, reporting "connection appears stale". Recovering means
	// dialling again, which is what the production Connector does too.
	maxRecoveryFailures = 10

	// defaultMaxPendingEvents bounds the buffer of received-but-unmatched
	// events. Successive Awaits on one Firehose share a subscription, so an
	// event read while waiting for a different one has to be kept.
	//
	// Overflow FAILS THE TEST rather than discarding the oldest entry: the
	// buffer holds evidence a later assertion may be about to ask for, and a
	// harness that silently drops it produces a green run that proves nothing.
	// A test that legitimately streams more than this is subscribing too
	// broadly and should filter with WithCollections.
	defaultMaxPendingEvents = 512

	// firehoseDialTimeout caps a single handshake. The Await deadline caps it
	// further whenever it is nearer.
	firehoseDialTimeout = 15 * time.Second

	// recoveryBackoff paces reconnection after a FAILED recovery, so a server
	// that refuses or immediately drops connections is retried a few times a
	// second rather than as fast as the scheduler allows.
	recoveryBackoff = 100 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

// Event is one Jetstream event, decoded far enough to match on.
type Event struct {
	DID    string `json:"did"`
	Kind   string `json:"kind"` // "commit", "identity", "account"
	TimeUS int64  `json:"time_us"`

	Commit   *Commit        `json:"commit,omitempty"`
	Identity *IdentityEvent `json:"identity,omitempty"`
	Account  *AccountEvent  `json:"account,omitempty"`

	// raw is the frame as it arrived. Unexported so it cannot be compared,
	// copied or marshalled by accident; reached through Raw and Into.
	raw []byte
}

// Commit is a record write carried by a commit event.
type Commit struct {
	Rev        string         `json:"rev"`
	Operation  string         `json:"operation"` // "create", "update", "delete"
	Collection string         `json:"collection"`
	RKey       string         `json:"rkey"`
	CID        string         `json:"cid,omitempty"`
	Record     map[string]any `json:"record,omitempty"`
}

// IdentityEvent is a handle or DID-document change.
type IdentityEvent struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
}

// AccountEvent is an account activation or deactivation.
type AccountEvent struct {
	DID    string `json:"did"`
	Active bool   `json:"active"`
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
}

// Raw returns a copy of the frame exactly as Jetstream sent it.
//
// A copy, because a test that kept the slice and a later decode of the same
// buffer would otherwise be able to interfere with each other — a subtle bug to
// design into a harness in exchange for saving one allocation per event.
func (e *Event) Raw() []byte {
	if e == nil {
		return nil
	}
	return slices.Clone(e.raw)
}

// Into decodes the original frame into v, which is how a test hands a real event
// to a real consumer:
//
//	var je jetstream.JetstreamEvent
//	require.NoError(t, ev.Into(&je))
//	require.NoError(t, consumer.HandleEvent(ctx, &je))
//
// Decoding the bytes rather than translating this package's structs is
// deliberate: a field testkit does not model still reaches the consumer, so the
// two declarations of the wire format cannot drift into a silently truncated
// event.
func (e *Event) Into(v any) error {
	if e == nil {
		return errors.New("testkit: decoding a nil event")
	}
	if len(e.raw) == 0 {
		return errors.New("testkit: this event carries no raw frame (it was not read from a subscription)")
	}
	if err := json.Unmarshal(e.raw, v); err != nil {
		return fmt.Errorf("testkit: decoding a %s event into %T: %w", e.Kind, v, err)
	}
	return nil
}

// URI renders the AT-URI of the record a commit event carries, or "" for an
// event that is not a commit.
func (e *Event) URI() string {
	if e == nil || e.Commit == nil {
		return ""
	}
	return "at://" + e.DID + "/" + e.Commit.Collection + "/" + e.Commit.RKey
}

// String renders an event compactly, for failure messages.
func (e *Event) String() string {
	if e == nil {
		return "<nil>"
	}
	if e.Commit != nil {
		return fmt.Sprintf("%s %s %s (time_us %d)", e.Kind, e.Commit.Operation, e.URI(), e.TimeUS)
	}
	return fmt.Sprintf("%s %s (time_us %d)", e.Kind, e.DID, e.TimeUS)
}

// identity is what makes two frames the same event, for the deduplication a
// resumed subscription needs.
//
// Rev and CID are part of it because one atProto commit can apply several
// writes, and those events share a time_us and a repo while differing in
// nothing else a matcher looks at. Collapsing them would drop an event a test
// is waiting for.
func (e *Event) identity() string {
	switch {
	case e.Commit != nil:
		return fmt.Sprintf("c|%s|%s|%s|%s", e.URI(), e.Commit.Operation, e.Commit.Rev, e.Commit.CID)
	case e.Identity != nil:
		return fmt.Sprintf("i|%s|%d|%s", e.Identity.DID, e.Identity.Seq, e.Identity.Handle)
	case e.Account != nil:
		return fmt.Sprintf("a|%s|%d|%t", e.Account.DID, e.Account.Seq, e.Account.Active)
	default:
		// An event kind this package does not model. Falling back to the frame
		// keeps two genuinely different events distinguishable.
		return "r|" + string(e.raw)
	}
}

// ---------------------------------------------------------------------------
// Matchers
// ---------------------------------------------------------------------------

// Matcher selects the event an Await is waiting for.
type Matcher func(*Event) bool

// MatchDID matches every event for one repo.
func MatchDID(did string) Matcher {
	return func(e *Event) bool { return e.DID == did }
}

// MatchCollection matches commit events in one collection.
func MatchCollection(collection string) Matcher {
	return func(e *Event) bool { return e.Commit != nil && e.Commit.Collection == collection }
}

// MatchRKey matches commit events with one record key.
func MatchRKey(rkey string) Matcher {
	return func(e *Event) bool { return e.Commit != nil && e.Commit.RKey == rkey }
}

// MatchOperation matches commit events by operation: "create", "update" or
// "delete".
func MatchOperation(operation string) Matcher {
	return func(e *Event) bool { return e.Commit != nil && e.Commit.Operation == operation }
}

// MatchURI matches the commit event for exactly one record.
func MatchURI(uri string) Matcher {
	return func(e *Event) bool { return e.URI() == uri }
}

// MatchRecord matches the commit events for a record just written, which is the
// common case: the write returns a Record, and that Record is the matcher.
func MatchRecord(r Record) Matcher { return MatchURI(r.URI) }

// MatchAll matches events that satisfy every matcher. With no arguments it
// matches everything.
func MatchAll(matchers ...Matcher) Matcher {
	return func(e *Event) bool {
		for _, m := range matchers {
			if !m(e) {
				return false
			}
		}
		return true
	}
}

// ---------------------------------------------------------------------------
// The subscriber
// ---------------------------------------------------------------------------

// Firehose is a cursor-gated Jetstream subscription.
//
// One test goroutine drives it. The lock protects its bookkeeping and is
// deliberately NOT held across the handshake or a blocking read, so Close — from
// a cleanup, or from another goroutine deciding the test is over — takes effect
// promptly instead of waiting out the current read window.
type Firehose struct {
	cursor      int64
	baseURL     string
	path        string
	collections []string

	// Tunables, fields rather than constants so this package's own tests can
	// trip the guards in under a second instead of fifty. Not exported: a test
	// that needs a shorter read deadline is testing this file.
	readDeadline        time.Duration
	maxRecoveryFailures int
	maxPending          int

	// ctx is cancelled by Close, which is what aborts a handshake in flight.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	conn    *websocket.Conn
	pending []*Event

	// lastTimeUS and seenAtLast are the resume bookkeeping. A re-dialled
	// subscription resumes from lastTimeUS INCLUSIVE — rewinding by a
	// microsecond would skip anything sharing that timestamp, and one atProto
	// commit applying several writes emits several events with a single time_us
	// — so the events already taken at that timestamp are remembered
	// individually and skipped on the way back through.
	lastTimeUS int64
	seenAtLast map[string]bool

	received  int
	discarded int
	// newestDiscarded is the largest time_us thrown away for predating the
	// cursor. Under a clock skew between this process and Jetstream it is the
	// only evidence distinguishing "nothing arrived" from "everything arrived
	// and was rejected", which otherwise reads as an unfixable timeout.
	newestDiscarded int64
	duplicates      int
	reconnects      int
	closed          bool
}

type firehoseConfig struct {
	baseURL     string
	path        string
	collections []string
	cursor      int64
	cursorSet   bool
}

// FirehoseOption customises NewFirehose.
type FirehoseOption func(*firehoseConfig)

// WithCollections restricts the subscription to the named collections, via
// Jetstream's wantedCollections filter. Without it every commit on the stream is
// delivered, which is legal but wasteful.
func WithCollections(collections ...string) FirehoseOption {
	return func(c *firehoseConfig) { c.collections = append(c.collections, collections...) }
}

// WithFirehoseURL overrides the Jetstream address from Endpoints(), for tests of
// this file that point it at a server they control.
func WithFirehoseURL(baseURL string) FirehoseOption {
	return func(c *firehoseConfig) { c.baseURL = trimURL(baseURL) }
}

// WithFirehoseCursor replays from an explicit time_us instead of the moment of
// construction.
//
// The honest use is resuming from an event a previous Await returned — bounding
// a "and nothing further happened" assertion by an OBSERVED time_us rather than
// by a wall clock. Passing time.Now() is the anti-pattern the type exists to
// prevent; NewFirehose already does that, at the only moment when it is safe.
func WithFirehoseCursor(timeUS int64) FirehoseOption {
	return func(c *firehoseConfig) { c.cursor, c.cursorSet = timeUS, true }
}

// NewFirehose captures a replay cursor and returns a subscription that has not
// dialled yet.
//
// Construct it BEFORE the write whose event you intend to await. Nothing about
// the connection matters until Await; the cursor is the load-bearing part, and
// it is taken here.
func NewFirehose(t TestingT, opts ...FirehoseOption) *Firehose {
	t.Helper()
	endpoint := Endpoints().Jetstream
	cfg := firehoseConfig{baseURL: endpoint.BaseURL, path: endpoint.SubscribePath}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !cfg.cursorSet {
		cfg.cursor = time.Now().UnixMicro()
	}

	ctx, cancel := context.WithCancel(context.Background())
	f := &Firehose{
		cursor:              cfg.cursor,
		baseURL:             cfg.baseURL,
		path:                cfg.path,
		collections:         cfg.collections,
		readDeadline:        firehoseReadDeadline,
		maxRecoveryFailures: maxRecoveryFailures,
		maxPending:          defaultMaxPendingEvents,
		ctx:                 ctx,
		cancel:              cancel,
		seenAtLast:          map[string]bool{},
	}
	t.Cleanup(f.Close)
	return f
}

// Cursor is the replay cursor this subscription is bounded by, in unix
// microseconds.
func (f *Firehose) Cursor() int64 { return f.cursor }

// SubscribeURL is the URL the next dial will use: the configured filters, and
// the cursor this subscription would resume from right now — the construction
// cursor until an event has been read, and that event's time_us afterwards.
func (f *Firehose) SubscribeURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribeURLLocked()
}

func (f *Firehose) subscribeURLLocked() string {
	params := url.Values{}
	for _, c := range f.collections {
		params.Add("wantedCollections", c)
	}
	params.Set("cursor", strconv.FormatInt(max(f.cursor, f.lastTimeUS), 10))
	return f.baseURL + f.path + "?" + params.Encode()
}

// Await waits for an event matching match and returns it, failing the test if
// none arrives within timeout.
//
// The subscription is dialled on the first call and reused afterwards, so events
// read while waiting for one match are held for the next Await rather than being
// lost. Every event is delivered to at most one Await.
//
// WithDescription and WithDiagnostics shape the failure message;
// WithPollInterval is meaningless here and ignored — this reads a stream, it
// does not poll.
//
// timeout is the ONLY limit on how long this waits. A quiet stream costs
// re-dials, not a failure: the read deadline is a window, and reaching it means
// resuming from the last event seen. What ends an Await unsuccessfully is either
// that timeout or maxRecoveryFailures consecutive failures to re-establish the
// subscription at all — reported as two different findings, because "nothing was
// published" and "Jetstream is gone" want different answers.
func (f *Firehose) Await(t TestingT, timeout time.Duration, match Matcher, opts ...WaitOption) *Event {
	t.Helper()
	cfg := newWaitConfig(opts)
	if cfg.description == "" {
		cfg.description = "a matching firehose event"
	}
	start := time.Now()
	deadline := start.Add(timeout)

	if event := f.takePending(match); event != nil {
		return event
	}

	recoveryFailures := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
				"timed out after %s (limit %s) waiting for %s",
				time.Since(start).Round(time.Millisecond), timeout, cfg.subject())))
			return nil
		}

		conn, err := f.connect(deadline)
		if err != nil {
			if f.isClosed() {
				t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
					"waiting for %s: the subscription was closed after %s",
					cfg.subject(), time.Since(start).Round(time.Millisecond))))
				return nil
			}
			recoveryFailures++
			if recoveryFailures >= f.maxRecoveryFailures {
				t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
					"gave up after %s waiting for %s: %d consecutive failures to establish the subscription; "+
						"the last was: %v",
					time.Since(start).Round(time.Millisecond), cfg.subject(), recoveryFailures, err)))
				return nil
			}
			f.pause(deadline)
			continue
		}

		// The read window is clamped to the caller's deadline, so the last read
		// of an Await ends exactly at it rather than past it.
		window, atDeadline := f.readDeadline, false
		if remaining <= window {
			window, atDeadline = remaining, true
		}

		raw, readErr := readFrame(conn, window)
		if readErr != nil {
			if f.isClosed() {
				t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
					"waiting for %s: the subscription was closed after %s",
					cfg.subject(), time.Since(start).Round(time.Millisecond))))
				return nil
			}
			// Every read error is recoverable, and every recovery is a fresh
			// connection: a websocket past its read deadline is corrupt, and a
			// stream that ended (EOF, a reset, a Jetstream restart) is precisely
			// the case cursor-resume exists for. Only a frame that will not
			// decode is terminal, and that is handled below.
			f.dropConnection()
			if atDeadline {
				// The window that expired WAS the caller's deadline. Re-dialling
				// to read for zero more seconds would replace an honest timeout
				// with whatever that dial happened to return.
				continue
			}
			if quietWindow(readErr) {
				// A silent stream is not a failure. Resume and keep waiting.
				recoveryFailures = 0
				f.countReconnect()
				continue
			}
			recoveryFailures++
			if recoveryFailures >= f.maxRecoveryFailures {
				t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
					"gave up after %s waiting for %s: the subscription broke %d times in a row; "+
						"the last error was: %v",
					time.Since(start).Round(time.Millisecond), cfg.subject(), recoveryFailures, readErr)))
				return nil
			}
			f.countReconnect()
			f.pause(deadline)
			continue
		}

		event := &Event{raw: raw}
		if err := json.Unmarshal(raw, event); err != nil {
			// Terminal: a frame this package cannot parse is a protocol change
			// or a different service on the port, and neither is fixed by
			// reading again.
			t.Fatalf("%s", f.render(cfg, fmt.Sprintf(
				"waiting for %s: Jetstream sent a frame that is not an event: %v\n  frame: %s",
				cfg.subject(), err, truncate(string(raw), 512))))
			return nil
		}
		recoveryFailures = 0

		matched, err := f.accept(event, match)
		if err != nil {
			t.Fatalf("%s", f.render(cfg, fmt.Sprintf("waiting for %s: %v", cfg.subject(), err)))
			return nil
		}
		if matched {
			return event
		}
	}
}

// accept applies the cursor bound and the duplicate filter, then either reports
// the event as the match or buffers it.
//
// It returns an error when the pending buffer is full: the buffered events are
// evidence for the assertions still to come, and quietly dropping the oldest
// would let a run go green having thrown away the thing it was about to check.
func (f *Firehose) accept(event *Event, match Matcher) (matched bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case event.TimeUS < f.cursor:
		f.discarded++
		f.newestDiscarded = max(f.newestDiscarded, event.TimeUS)
		return false, nil
	case event.TimeUS < f.lastTimeUS:
		// Older than the resume point: a stream replaying more than was asked
		// for. Not a duplicate of anything held, but not new either.
		f.duplicates++
		return false, nil
	case event.TimeUS == f.lastTimeUS && f.seenAtLast[event.identity()]:
		f.duplicates++
		return false, nil
	}

	if event.TimeUS > f.lastTimeUS {
		f.lastTimeUS = event.TimeUS
		clear(f.seenAtLast)
	}
	f.seenAtLast[event.identity()] = true
	f.received++

	if match(event) {
		return true, nil
	}
	if len(f.pending) >= f.maxPending {
		return false, fmt.Errorf(
			"the buffer of unmatched events is full at %d, and discarding one to make room would "+
				"throw away evidence a later assertion may need; narrow the subscription with "+
				"WithCollections, or await events closer to when they are written",
			f.maxPending)
	}
	f.pending = append(f.pending, event)
	return false, nil
}

// Close ends the subscription. Registered as a test cleanup by NewFirehose;
// calling it early is safe, idempotent, and unblocks an Await's handshake.
func (f *Firehose) Close() {
	f.mu.Lock()
	f.closed = true
	conn := f.conn
	f.conn = nil
	f.mu.Unlock()

	// Cancelling interrupts a handshake in flight; closing the socket interrupts
	// a blocking read. Neither needs the lock, and holding it across them would
	// reintroduce the stall this ordering exists to avoid.
	f.cancel()
	if conn != nil {
		_ = conn.Close()
	}
}

func (f *Firehose) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// connect returns the live subscription, dialling if there is none.
//
// The handshake happens OUTSIDE the lock: it is a network round trip, and
// holding the lock across it would make Close wait for a connection it is trying
// to abandon.
func (f *Firehose) connect(deadline time.Time) (*websocket.Conn, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errors.New("this firehose has been closed")
	}
	if f.conn != nil {
		conn := f.conn
		f.mu.Unlock()
		return conn, nil
	}
	target := f.subscribeURLLocked()
	f.mu.Unlock()

	// Bounded by the caller's deadline as well as by the handshake cap: a dial
	// allowed to run past the deadline would make a sub-second Await take
	// fifteen seconds and report a dial error in place of an honest timeout.
	dialDeadline := deadline
	if capped := time.Now().Add(firehoseDialTimeout); capped.Before(dialDeadline) {
		dialDeadline = capped
	}
	ctx, cancel := context.WithDeadline(f.ctx, dialDeadline)
	defer cancel()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, target, nil)
	if err != nil {
		// The handshake response carries the reason a Jetstream rejects a
		// subscription (a malformed collection filter, for instance), and
		// discarding it leaves only "bad handshake".
		status := ""
		if resp != nil {
			status = fmt.Sprintf(" (HTTP %d)", resp.StatusCode)
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("dialling Jetstream at %s%s: %w", target, status, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("this firehose has been closed")
	}
	f.conn = conn
	f.mu.Unlock()
	return conn, nil
}

// dropConnection abandons the current socket so the next connect dials afresh.
func (f *Firehose) dropConnection() {
	f.mu.Lock()
	conn := f.conn
	f.conn = nil
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (f *Firehose) countReconnect() {
	f.mu.Lock()
	f.reconnects++
	f.mu.Unlock()
}

// pause waits out the recovery backoff, so a server that refuses or immediately
// drops connections is not retried in a hot loop.
func (f *Firehose) pause(deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(min(recoveryBackoff, remaining))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-f.ctx.Done():
	}
}

// readFrame reads one message with a bounded deadline. It touches no shared
// state, so it runs without the lock.
func readFrame(conn *websocket.Conn, window time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		return nil, fmt.Errorf("setting the read deadline: %w", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return data, nil
}

// quietWindow reports whether a read failed because nothing arrived, as opposed
// to because the connection broke.
func quietWindow(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// takePending removes and returns the first buffered event that matches.
func (f *Firehose) takePending(match Matcher) *Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, event := range f.pending {
		if match(event) {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			return event
		}
	}
	return nil
}

// render builds a failure message with the subscription's state attached, then
// the caller's own diagnostics.
//
// The state is the point. "Timed out waiting for an event" is the failure this
// tier exists to stop producing: what a reader needs is whether ANY event
// arrived (a live stream with a wrong matcher), whether none did (a dead
// consumer, a wrong collection filter, a PDS that never committed), or whether
// they all arrived and were rejected for predating the cursor (a clock skew
// between this process and Jetstream, which no amount of waiting fixes).
func (f *Firehose) render(cfg waitConfig, msg string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var b strings.Builder
	b.WriteString(msg)
	fmt.Fprintf(&b, "\n  subscription: %s", f.subscribeURLLocked())
	fmt.Fprintf(&b, "\n  events past the cursor: %d received, %d held unmatched", f.received, len(f.pending))
	if f.duplicates > 0 {
		fmt.Fprintf(&b, ", %d replayed duplicates ignored", f.duplicates)
	}
	if f.discarded > 0 {
		fmt.Fprintf(&b, "\n  %d event(s) arrived but PREDATED the cursor (newest time_us %d, cursor %d, "+
			"%s earlier) — that reads as a clock skew between this process and Jetstream, not as a missing event",
			f.discarded, f.newestDiscarded, f.cursor,
			time.Duration(f.cursor-f.newestDiscarded)*time.Microsecond)
	}
	if f.reconnects > 0 {
		fmt.Fprintf(&b, "\n  subscription re-dialled %d time(s); resuming from time_us %d",
			f.reconnects, max(f.cursor, f.lastTimeUS))
	}
	if n := len(f.pending); n > 0 {
		shown := f.pending
		if n > 5 {
			shown = shown[n-5:]
		}
		b.WriteString("\n  most recent unmatched:")
		for _, event := range shown {
			fmt.Fprintf(&b, "\n    %s", event)
		}
	}
	return cfg.render(b.String())
}

// truncate bounds a quoted payload in a failure message.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… (truncated)"
}
