//go:build integration

package testkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The claim the whole file exists for
// ---------------------------------------------------------------------------

// TestFirehose_CursorGatingBeatsSubscribeAfterWrite is the load-bearing test.
//
// The record is written, AND read back from the PDS, before Await opens the
// websocket. Every hand-rolled subscriber this replaces would miss that event
// by construction: a cursorless subscription only carries commits emitted after
// the dial. It is caught here because NewFirehose took a replay cursor before
// the write, and Await replays from it.
func TestFirehose_CursorGatingBeatsSubscribeAfterWrite(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	// Cursor captured here. Nothing is dialled yet.
	firehose := NewFirehose(t, WithCollections(testCollection))

	record := account.CreateRecord(t, testCollection, map[string]any{
		"$type": testCollection,
		"text":  "cursor gating",
	})
	// A full round trip to the PDS after the write, so the socket is opened
	// well after the commit that produced the event.
	require.Equal(t, record.URI, account.GetRecord(t, testCollection, record.RKey).URI)

	event := firehose.Await(t, 30*time.Second, MatchRecord(record),
		WithDescription("the commit for %s", record.URI))

	require.NotNil(t, event)
	assert.Equal(t, "commit", event.Kind)
	assert.Equal(t, account.DID, event.DID)
	require.NotNil(t, event.Commit)
	assert.Equal(t, "create", event.Commit.Operation)
	assert.Equal(t, testCollection, event.Commit.Collection)
	assert.Equal(t, record.RKey, event.Commit.RKey)
	assert.Equal(t, "cursor gating", event.Commit.Record["text"])
	assert.Greater(t, event.TimeUS, firehose.Cursor(),
		"an event matched by a cursor-gated subscription is always newer than the cursor")
}

// TestFirehose_DeliversUpdateAndDeleteOnOneSubscription covers the shape every
// ingestion contract has — create, update, delete — and with it the buffering
// that makes successive Awaits on one connection safe.
func TestFirehose_DeliversUpdateAndDeleteOnOneSubscription(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)
	firehose := NewFirehose(t, WithCollections(testCollection))

	record := account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection, "text": "v1"})
	account.PutRecord(t, testCollection, record.RKey, map[string]any{"$type": testCollection, "text": "v2"})
	account.DeleteRecord(t, testCollection, record.RKey)

	deleted := firehose.Await(t, 30*time.Second, MatchAll(MatchRecord(record), MatchOperation("delete")))
	require.NotNil(t, deleted)

	// Awaited last but emitted first: the create and update events arrived while
	// the delete was being waited for, and were held rather than dropped.
	created := firehose.Await(t, 10*time.Second, MatchAll(MatchRecord(record), MatchOperation("create")))
	require.NotNil(t, created)
	assert.Equal(t, "v1", created.Commit.Record["text"])

	updated := firehose.Await(t, 10*time.Second, MatchAll(MatchRecord(record), MatchOperation("update")))
	require.NotNil(t, updated)
	assert.Equal(t, "v2", updated.Commit.Record["text"])
	assert.Less(t, created.TimeUS, updated.TimeUS)
}

// ---------------------------------------------------------------------------
// A Jetstream under the test's control
// ---------------------------------------------------------------------------

// fakeJetstream serves websocket subscriptions from a handler the test writes.
//
// The guards below — the stale-connection counter, the deadline, the cursor
// bound — are about what happens when the stream misbehaves, and a real
// Jetstream cannot be asked to misbehave on cue.
func fakeJetstream(t *testing.T, handle func(*websocket.Conn, *url.URL)) (baseURL string) {
	t.Helper()
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn, r.URL)
	}))
	t.Cleanup(server.Close)

	return "ws://" + strings.TrimPrefix(server.URL, "http://")
}

// silentStream accepts the connection and sends nothing, until the client goes
// away.
func silentStream(conn *websocket.Conn, _ *url.URL) {
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

// sendEvents replays a fixed list from the subscription's cursor and then stays
// silent.
//
// Honouring the cursor is what makes this a stand-in for Jetstream rather than
// a tape recorder: a firehose that has to re-dial resumes from the last event it
// saw, and a fake that replayed everything regardless would test the fake's
// amnesia instead of the resume.
func sendEvents(events ...Event) func(*websocket.Conn, *url.URL) {
	return func(conn *websocket.Conn, requested *url.URL) {
		cursor, _ := strconv.ParseInt(requested.Query().Get("cursor"), 10, 64)
		for _, event := range events {
			if event.TimeUS < cursor {
				continue
			}
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		}
		silentStream(conn, requested)
	}
}

func commitEvent(did, collection, rkey, operation string, timeUS int64) Event {
	return Event{
		DID:    did,
		Kind:   "commit",
		TimeUS: timeUS,
		Commit: &Commit{Operation: operation, Collection: collection, RKey: rkey},
	}
}

// ---------------------------------------------------------------------------
// Guards
// ---------------------------------------------------------------------------

// TestFirehose_QuietStreamIsBoundedByTheDeadlineNotTheGuard is the regression
// test for the bug that made every subscriber in the old suite lie about its
// timeout.
//
// A read-deadline expiry corrupts a gorilla connection, so recovering from a
// quiet window means re-dialling. The ten copies this replaces instead counted
// those expiries toward a stale-connection guard and re-read the corrupt
// connection, which made the counter hit its limit microseconds after the first
// expiry: a "wait up to 30 seconds" subscription really waited five. Here the
// stream is silent for far more windows than the guard's limit, and the wait
// must still run to the caller's deadline and blame nothing but the clock.
func TestFirehose_QuietStreamIsBoundedByTheDeadlineNotTheGuard(t *testing.T) {
	baseURL := fakeJetstream(t, silentStream)

	firehose := NewFirehose(t, WithFirehoseURL(baseURL))
	firehose.readDeadline = 20 * time.Millisecond
	firehose.maxRecoveryFailures = 3

	ft := &fakeT{}
	runIsolated(func() {
		firehose.Await(ft, 400*time.Millisecond, MatchAll(), WithDescription("anything at all"))
	})

	require.True(t, ft.failed(), "a silent stream still has to fail eventually")
	message := ft.message()
	assert.Contains(t, message, "timed out")
	assert.Contains(t, message, "limit 400ms", "the caller's deadline is what ended the wait")
	assert.NotContains(t, message, "consecutive failures",
		"a quiet window is not a failed recovery, however many of them there are")
	assert.Contains(t, message, "anything at all")

	firehose.mu.Lock()
	reconnects := firehose.reconnects
	firehose.mu.Unlock()
	assert.Greater(t, reconnects, firehose.maxRecoveryFailures,
		"the wait should have re-dialled past the guard's limit without tripping it")
}

// TestFirehose_UnreachableJetstreamTripsTheRecoveryGuard covers the other half:
// a subscription that cannot be established at all is a finding, reported with
// the dial error rather than as a bare timeout.
func TestFirehose_UnreachableJetstreamTripsTheRecoveryGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := "ws://" + strings.TrimPrefix(server.URL, "http://")
	server.Close() // nothing is listening now

	firehose := NewFirehose(t, WithFirehoseURL(address))
	firehose.maxRecoveryFailures = 3

	ft := &fakeT{}
	runIsolated(func() {
		firehose.Await(ft, time.Minute, MatchAll(), WithDescription("anything at all"))
	})

	require.True(t, ft.failed())
	message := ft.message()
	assert.Contains(t, message, "3 consecutive failures to establish the subscription")
	assert.Contains(t, message, "dialling Jetstream")
	assert.Contains(t, message, address)
}

func TestFirehose_TimeoutReportsWhatTheStreamDid(t *testing.T) {
	did := "did:plc:firehosetest"
	baseURL := fakeJetstream(t, sendEvents(
		commitEvent(did, testCollection, "aaa", "create", time.Now().UnixMicro()+1_000_000),
		commitEvent(did, testCollection, "bbb", "create", time.Now().UnixMicro()+2_000_000),
	))

	firehose := NewFirehose(t, WithFirehoseURL(baseURL), WithCollections(testCollection))
	firehose.readDeadline = 50 * time.Millisecond

	ft := &fakeT{}
	runIsolated(func() {
		// Well inside the stale guard's own window (10 × 50ms here), so the
		// deadline is unambiguously the binding constraint and the verdict is
		// "timed out" rather than "stale connection".
		firehose.Await(ft, 300*time.Millisecond, MatchRKey("never-sent"),
			WithDescription("a record that was never written"),
			WithDiagnostics(func() string { return "caller diagnostics" }))
	})

	require.True(t, ft.failed())
	message := ft.message()
	// The distinction a timeout must make: a live stream whose events did not
	// match reads very differently from a stream that delivered nothing.
	assert.Contains(t, message, "timed out")
	assert.Contains(t, message, "a record that was never written")
	assert.Contains(t, message, "2 received, 2 held unmatched")
	assert.Contains(t, message, "wantedCollections="+url.QueryEscape(testCollection))
	assert.Contains(t, message, "aaa", "the unmatched events should be listed")
	assert.Contains(t, message, "caller diagnostics")
}

// TestFirehose_CursorIsABoundNotAHint is the defence against the Jetstream
// behaviour where a subscription on a quiet stream replays the entire retained
// store: an event older than the cursor must not be able to satisfy an Await,
// however well it matches.
//
// The fake IGNORES the cursor here, deliberately. A server that filtered by
// cursor would make this test pass without the client bound existing at all —
// which is exactly what it looked like before, and exactly the misbehaving
// server the bound is for.
func TestFirehose_CursorIsABoundNotAHint(t *testing.T) {
	did := "did:plc:firehosetest"
	now := time.Now().UnixMicro()
	ancient := commitEvent(did, testCollection, "ancient", "create", now-3_600_000_000)
	baseURL := fakeJetstream(t, func(conn *websocket.Conn, requested *url.URL) {
		require.NotEmpty(t, requested.Query().Get("cursor"), "the subscription must still ask for a cursor")
		_ = conn.WriteJSON(ancient)
		silentStream(conn, requested)
	})

	firehose := NewFirehose(t, WithFirehoseURL(baseURL), WithFirehoseCursor(now))
	firehose.readDeadline = 50 * time.Millisecond

	ft := &fakeT{}
	runIsolated(func() {
		firehose.Await(ft, 300*time.Millisecond, MatchRKey("ancient"))
	})

	require.True(t, ft.failed(), "an event older than the cursor must not match")
	message := ft.message()
	assert.Contains(t, message, "0 received")
	// The count is the difference between "the write never happened" and "your
	// clock disagrees with Jetstream's", which no amount of waiting fixes.
	assert.Contains(t, message, "PREDATED the cursor")
	assert.Contains(t, message, "clock skew")
}

// TestFirehose_ResumesWithoutLosingOrRepeatingEvents is the guarantee that makes
// a quiet window survivable: the connection is replaced, and the gap between the
// two connections is covered by replay rather than lost.
func TestFirehose_ResumesWithoutLosingOrRepeatingEvents(t *testing.T) {
	did := "did:plc:firehosetest"
	base := time.Now().UnixMicro()
	first := commitEvent(did, testCollection, "first", "create", base+1_000)
	// Stamped INSIDE the silent gap: it exists on the server before the second
	// connection is made, so only a resume from the right cursor delivers it.
	second := commitEvent(did, testCollection, "second", "create", base+2_000)

	var connections atomic.Int32
	baseURL := fakeJetstream(t, func(conn *websocket.Conn, requested *url.URL) {
		switch connections.Add(1) {
		case 1:
			// One event, then silence until the read deadline expires.
			_ = conn.WriteJSON(first)
		default:
			// A real Jetstream replays from the cursor inclusive.
			cursor, _ := strconv.ParseInt(requested.Query().Get("cursor"), 10, 64)
			assert.Equal(t, first.TimeUS, cursor,
				"the resume must start at the last event seen, not at the original cursor")
			for _, event := range []Event{first, second} {
				if event.TimeUS >= cursor {
					_ = conn.WriteJSON(event)
				}
			}
		}
		silentStream(conn, requested)
	})

	firehose := NewFirehose(t, WithFirehoseURL(baseURL), WithFirehoseCursor(base))
	firehose.readDeadline = 50 * time.Millisecond

	// The first event is buffered while waiting for the second, which only the
	// resumed connection carries.
	got := firehose.Await(t, 5*time.Second, MatchRKey("second"))
	require.NotNil(t, got)
	assert.Equal(t, second.TimeUS, got.TimeUS)

	buffered := firehose.Await(t, time.Second, MatchRKey("first"))
	require.NotNil(t, buffered, "the event read before the gap must still be available")

	firehose.mu.Lock()
	received, duplicates, reconnects := firehose.received, firehose.duplicates, firehose.reconnects
	firehose.mu.Unlock()

	assert.Equal(t, 2, received, "each event is delivered exactly once across the reconnect")
	assert.Equal(t, 1, duplicates, "the inclusive resume re-sends exactly one event, which is dropped")
	assert.GreaterOrEqual(t, reconnects, 1)
}

// TestFirehose_KeepsEventsSharingOneMicrosecond covers the boundary the resume
// is built around.
//
// One atProto commit can apply several writes, and Jetstream stamps them all
// with the same time_us. Resuming from that timestamp re-delivers every one of
// them, so remembering only the LAST event seen would drop its siblings — and
// resuming from time_us+1 instead would skip them outright. Both failures are
// silent: the test just waits forever for an event that already went past.
func TestFirehose_KeepsEventsSharingOneMicrosecond(t *testing.T) {
	did := "did:plc:firehosetest"
	base := time.Now().UnixMicro()
	shared := base + 1_000
	siblingA := commitEvent(did, testCollection, "sibling-a", "create", shared)
	siblingB := commitEvent(did, testCollection, "sibling-b", "create", shared)

	var connections atomic.Int32
	baseURL := fakeJetstream(t, func(conn *websocket.Conn, requested *url.URL) {
		if connections.Add(1) == 1 {
			_ = conn.WriteJSON(siblingA)
			_ = conn.WriteJSON(siblingB)
		} else {
			cursor, _ := strconv.ParseInt(requested.Query().Get("cursor"), 10, 64)
			assert.Equal(t, shared, cursor, "an inclusive resume cannot skip the shared microsecond")
			_ = conn.WriteJSON(siblingA)
			_ = conn.WriteJSON(siblingB)
		}
		silentStream(conn, requested)
	})

	firehose := NewFirehose(t, WithFirehoseURL(baseURL), WithFirehoseCursor(base))
	firehose.readDeadline = 50 * time.Millisecond

	require.NotNil(t, firehose.Await(t, 5*time.Second, MatchRKey("sibling-b")))
	require.NotNil(t, firehose.Await(t, time.Second, MatchRKey("sibling-a")))

	// Force a reconnect and prove the replayed pair is recognised as already
	// seen rather than delivered twice.
	ft := &fakeT{}
	runIsolated(func() { firehose.Await(ft, 300*time.Millisecond, MatchRKey("never-sent")) })
	require.True(t, ft.failed())

	firehose.mu.Lock()
	received := firehose.received
	firehose.mu.Unlock()
	assert.Equal(t, 2, received, "both siblings arrive once; the replayed copies are ignored")
}

// TestFirehose_BufferOverflowFailsRatherThanDiscarding: the buffer holds events
// a later Await may ask for, so silently dropping the oldest would let a run go
// green having thrown away the evidence it was about to check.
func TestFirehose_BufferOverflowFailsRatherThanDiscarding(t *testing.T) {
	did := "did:plc:firehosetest"
	base := time.Now().UnixMicro()
	var events []Event
	for i := range 5 {
		events = append(events, commitEvent(did, testCollection,
			fmt.Sprintf("event-%d", i), "create", base+int64(i+1)*1_000))
	}
	baseURL := fakeJetstream(t, sendEvents(events...))

	firehose := NewFirehose(t, WithFirehoseURL(baseURL), WithFirehoseCursor(base))
	firehose.readDeadline = 50 * time.Millisecond
	firehose.maxPending = 3

	ft := &fakeT{}
	runIsolated(func() { firehose.Await(ft, 5*time.Second, MatchRKey("never-sent")) })

	require.True(t, ft.failed())
	message := ft.message()
	assert.Contains(t, message, "buffer of unmatched events is full at 3")
	assert.Contains(t, message, "WithCollections", "the failure should say how to fix it")
}

// TestFirehose_CloseInterruptsAnAwaitInFlight is why the lock is not held across
// the handshake or a blocking read.
//
// A Firehose registers Close as a test cleanup, and an Await that held the mutex
// for its whole wait would make Close block until the read window expired — so a
// test abandoning a wait would sit there for the rest of the deadline.
func TestFirehose_CloseInterruptsAnAwaitInFlight(t *testing.T) {
	baseURL := fakeJetstream(t, silentStream)
	firehose := NewFirehose(t, WithFirehoseURL(baseURL))

	failed := make(chan struct{})
	ft := &fakeT{}
	go func() {
		defer close(failed)
		runIsolated(func() { firehose.Await(ft, time.Minute, MatchAll()) })
	}()

	// Let the subscription get as far as a blocking read.
	WaitFor(t, 5*time.Second, func() (bool, error) {
		firehose.mu.Lock()
		defer firehose.mu.Unlock()
		return firehose.conn != nil, nil
	}, WithPollInterval(10*time.Millisecond), WithDescription("the subscription to be dialled"))

	closed := make(chan struct{})
	go func() { defer close(closed); firehose.Close() }()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind an in-flight Await")
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("Await did not notice the subscription had been closed")
	}
	assert.Contains(t, ft.message(), "closed")
}

func TestFirehose_AMalformedFrameIsTerminal(t *testing.T) {
	baseURL := fakeJetstream(t, func(conn *websocket.Conn, requested *url.URL) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("this is not an event"))
		silentStream(conn, requested)
	})
	firehose := NewFirehose(t, WithFirehoseURL(baseURL))
	firehose.readDeadline = 50 * time.Millisecond

	ft := &fakeT{}
	runIsolated(func() { firehose.Await(ft, 10*time.Second, MatchAll()) })

	require.True(t, ft.failed())
	message := ft.message()
	// Not retried: a frame this package cannot parse is a protocol change or a
	// different service on the port, and reading again fixes neither.
	assert.Contains(t, message, "not an event")
	assert.Contains(t, message, "this is not an event", "the frame itself is the evidence")
}

func TestFirehose_AwaitAfterCloseFails(t *testing.T) {
	baseURL := fakeJetstream(t, silentStream)
	firehose := NewFirehose(t, WithFirehoseURL(baseURL))
	firehose.Close()
	firehose.Close() // idempotent

	ft := &fakeT{}
	runIsolated(func() { firehose.Await(ft, time.Second, MatchAll()) })

	require.True(t, ft.failed())
	assert.Contains(t, ft.message(), "closed")
}

// ---------------------------------------------------------------------------
// URL construction and matchers
// ---------------------------------------------------------------------------

func TestFirehose_SubscribeURLCarriesTheCursorAndFilters(t *testing.T) {
	firehose := NewFirehose(t,
		WithFirehoseURL("ws://jetstream.test:6008/"),
		WithCollections("social.coves.community.post", "social.coves.community.comment"),
		WithFirehoseCursor(1234567890))

	parsed, err := url.Parse(firehose.SubscribeURL())
	require.NoError(t, err)
	assert.Equal(t, "ws", parsed.Scheme)
	assert.Equal(t, "jetstream.test:6008", parsed.Host)
	assert.Equal(t, "/subscribe", parsed.Path)
	assert.Equal(t, "1234567890", parsed.Query().Get("cursor"))
	assert.Equal(t,
		[]string{"social.coves.community.post", "social.coves.community.comment"},
		parsed.Query()["wantedCollections"])
}

func TestFirehose_CursorIsCapturedAtConstruction(t *testing.T) {
	before := time.Now().UnixMicro()
	firehose := NewFirehose(t, WithFirehoseURL("ws://jetstream.test:6008"))
	after := time.Now().UnixMicro()

	assert.GreaterOrEqual(t, firehose.Cursor(), before)
	assert.LessOrEqual(t, firehose.Cursor(), after)
}

func TestMatchers(t *testing.T) {
	event := commitEvent("did:plc:alice", "social.coves.community.post", "3kabc", "create", 42)
	identity := &Event{DID: "did:plc:alice", Kind: "identity", TimeUS: 43,
		Identity: &IdentityEvent{DID: "did:plc:alice", Handle: "alice.local.coves.dev"}}

	assert.True(t, MatchDID("did:plc:alice")(&event))
	assert.False(t, MatchDID("did:plc:bob")(&event))
	assert.True(t, MatchCollection("social.coves.community.post")(&event))
	assert.False(t, MatchCollection("social.coves.community.post")(identity),
		"a non-commit event has no collection to match")
	assert.True(t, MatchRKey("3kabc")(&event))
	assert.True(t, MatchOperation("create")(&event))
	assert.False(t, MatchOperation("delete")(&event))
	assert.True(t, MatchURI("at://did:plc:alice/social.coves.community.post/3kabc")(&event))
	assert.True(t, MatchRecord(Record{URI: "at://did:plc:alice/social.coves.community.post/3kabc"})(&event))
	assert.True(t, MatchAll()(&event), "no matchers matches everything")
	assert.True(t, MatchAll(MatchDID("did:plc:alice"), MatchOperation("create"))(&event))
	assert.False(t, MatchAll(MatchDID("did:plc:alice"), MatchOperation("delete"))(&event))
}

// TestFirehose_EventCarriesTheRawFrame proves the property the ten consumer
// migrations depend on: a test can hand the event it awaited to the production
// consumer, fields testkit does not model included.
func TestFirehose_EventCarriesTheRawFrame(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)
	firehose := NewFirehose(t, WithCollections(testCollection))

	record := account.CreateRecord(t, testCollection, map[string]any{
		"$type": testCollection,
		"text":  "raw frames",
	})
	event := firehose.Await(t, 30*time.Second, MatchRecord(record))
	require.NotNil(t, event)

	// A struct shaped like a consumer's, including a field Event does not model.
	var decoded struct {
		DID    string `json:"did"`
		Kind   string `json:"kind"`
		TimeUS int64  `json:"time_us"`
		Commit struct {
			Rev        string         `json:"rev"`
			Operation  string         `json:"operation"`
			Collection string         `json:"collection"`
			RKey       string         `json:"rkey"`
			CID        string         `json:"cid"`
			Record     map[string]any `json:"record"`
		} `json:"commit"`
	}
	require.NoError(t, event.Into(&decoded))
	assert.Equal(t, account.DID, decoded.DID)
	assert.Equal(t, record.RKey, decoded.Commit.RKey)
	assert.Equal(t, record.CID, decoded.Commit.CID)
	assert.NotEmpty(t, decoded.Commit.Rev)
	assert.Equal(t, "raw frames", decoded.Commit.Record["text"])

	raw := event.Raw()
	assert.Contains(t, string(raw), `"time_us"`)
	// A copy: mutating what Raw handed back must not corrupt a later decode.
	for i := range raw {
		raw[i] = 'x'
	}
	require.NoError(t, event.Into(&decoded))
	assert.Equal(t, record.RKey, decoded.Commit.RKey)
}

func TestEvent_IntoRejectsWhatItCannotDecode(t *testing.T) {
	var missing *Event
	assert.Error(t, missing.Into(&struct{}{}))
	assert.Nil(t, missing.Raw())

	// An event built by a test rather than read from a subscription has no
	// frame, and saying so beats decoding silence into a zero value.
	handmade := commitEvent("did:plc:alice", testCollection, "abc", "create", 1)
	err := handmade.Into(&struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no raw frame")
}

func TestEvent_URIAndString(t *testing.T) {
	event := commitEvent("did:plc:alice", "social.coves.community.post", "3kabc", "create", 42)
	assert.Equal(t, "at://did:plc:alice/social.coves.community.post/3kabc", event.URI())
	assert.Contains(t, event.String(), "create")

	identity := &Event{DID: "did:plc:alice", Kind: "identity", TimeUS: 43}
	assert.Empty(t, identity.URI(), "only commits name a record")
	assert.Contains(t, identity.String(), "identity")

	var missing *Event
	assert.Empty(t, missing.URI())
	assert.Equal(t, "<nil>", missing.String())
}

// TestEvent_DecodesTheJetstreamWireFormat pins the field names against a
// payload in the shape Jetstream actually publishes.
//
// firehose_pin_test.go pins the same structs against internal/atproto/jetstream's,
// and the two catch different things. That one catches this package drifting
// from the consumers. This one catches BOTH of them drifting from Jetstream,
// which a struct-to-struct comparison cannot see, because the wire format is
// owned by neither.
func TestEvent_DecodesTheJetstreamWireFormat(t *testing.T) {
	const payload = `{
		"did": "did:plc:alice",
		"time_us": 1751000000000000,
		"kind": "commit",
		"commit": {
			"rev": "3kabcdefghij",
			"operation": "create",
			"collection": "social.coves.community.post",
			"rkey": "3kzzzzzzzzzzz",
			"record": {"$type": "social.coves.community.post", "title": "hello"},
			"cid": "bafyreiabc"
		}
	}`

	var event Event
	require.NoError(t, json.Unmarshal([]byte(payload), &event))

	assert.Equal(t, "did:plc:alice", event.DID)
	assert.Equal(t, "commit", event.Kind)
	assert.Equal(t, int64(1751000000000000), event.TimeUS)
	require.NotNil(t, event.Commit)
	assert.Equal(t, "3kabcdefghij", event.Commit.Rev)
	assert.Equal(t, "create", event.Commit.Operation)
	assert.Equal(t, "social.coves.community.post", event.Commit.Collection)
	assert.Equal(t, "3kzzzzzzzzzzz", event.Commit.RKey)
	assert.Equal(t, "bafyreiabc", event.Commit.CID)
	assert.Equal(t, "hello", event.Commit.Record["title"])

	var identity Event
	require.NoError(t, json.Unmarshal([]byte(
		`{"did":"did:plc:alice","kind":"identity","time_us":1,"identity":{"did":"did:plc:alice","handle":"alice.test","seq":7,"time":"2026-07-29T00:00:00Z"}}`), &identity))
	require.NotNil(t, identity.Identity)
	assert.Equal(t, "alice.test", identity.Identity.Handle)
	assert.Equal(t, int64(7), identity.Identity.Seq)

	var account Event
	require.NoError(t, json.Unmarshal([]byte(
		`{"did":"did:plc:alice","kind":"account","time_us":1,"account":{"did":"did:plc:alice","active":true,"seq":9,"time":"2026-07-29T00:00:00Z"}}`), &account))
	require.NotNil(t, account.Account)
	assert.True(t, account.Account.Active)
	assert.Equal(t, int64(9), account.Account.Seq)
}
