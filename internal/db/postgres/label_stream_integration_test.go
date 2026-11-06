//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/routes"
	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labelStreamPollInterval keeps live delivery quick in these tests; the
// production default is the Stream's own.
const labelStreamPollInterval = 20 * time.Millisecond

// gatedLabelSource is the real label repository with a gate in front of
// LabelsAfter: a call whose afterSeq matches is held, before it reads, until
// release. reached receives the afterSeq of each held call.
type gatedLabelSource struct {
	*postgres.LabelRepository
	match       func(afterSeq int64) bool
	repeat      bool
	reached     chan int64
	released    chan struct{}
	releaseOnce sync.Once

	mutex sync.Mutex
	held  bool
}

// newGatedLabelSource holds the first LabelsAfter call whose afterSeq
// satisfies match or, with repeat, every such call.
func newGatedLabelSource(db *sql.DB, match func(afterSeq int64) bool, repeat bool) *gatedLabelSource {
	return &gatedLabelSource{
		LabelRepository: postgres.NewLabelRepository(db),
		match:           match,
		repeat:          repeat,
		reached:         make(chan int64, 64),
		released:        make(chan struct{}),
	}
}

func (s *gatedLabelSource) LabelsAfter(ctx context.Context, afterSeq int64, limit int) ([]labels.Label, error) {
	if s.shouldHold(afterSeq) {
		select {
		case s.reached <- afterSeq:
		default:
		}
		select {
		case <-s.released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.LabelRepository.LabelsAfter(ctx, afterSeq, limit)
}

func (s *gatedLabelSource) shouldHold(afterSeq int64) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if !s.match(afterSeq) || (s.held && !s.repeat) {
		return false
	}
	s.held = true
	return true
}

func (s *gatedLabelSource) release() {
	s.releaseOnce.Do(func() { close(s.released) })
}

// waitForGate waits until a held LabelsAfter call reaches the gate.
func waitForGate(t *testing.T, source *gatedLabelSource, description string) int64 {
	t.Helper()
	var afterSeq int64
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		select {
		case afterSeq = <-source.reached:
			return true, nil
		default:
			return false, nil
		}
	}, testkit.WithDescription("%s", description))
	return afterSeq
}

// newLabelStreamServer serves subscribeLabels from a Stream over source through
// the production route registration. At cleanup a gated source is released
// first, so a held read cannot block shutdown, then the Stream and the server
// are closed.
func newLabelStreamServer(t *testing.T, source labels.StreamSource, options labels.StreamOptions) (string, *labels.Stream) {
	t.Helper()
	stream := labels.NewStream(source, options)
	router := chi.NewRouter()
	routes.RegisterLabelStreamRoutes(router, stream)
	server := httptest.NewServer(router)
	t.Cleanup(func() {
		if gated, ok := source.(*gatedLabelSource); ok {
			gated.release()
		}
		stream.Close()
		server.Close()
	})
	return server.URL, stream
}

// streamLabelDecider commits real moderation decisions, each publishing one
// signed label, and reports the stored row's seq and subject.
type streamLabelDecider struct {
	db           *sql.DB
	service      moderation.Service
	communityDID string
	authorDID    string
	decisions    int
}

type committedLabel struct {
	seq int64
	uri string
}

func newStreamLabelDecider(t *testing.T, db *sql.DB) *streamLabelDecider {
	t.Helper()
	communityDID, authorDID := moderationPostActors(t, db)
	return &streamLabelDecider{
		db: db, service: newLabelPublishingModerationService(t, db),
		communityDID: communityDID, authorDID: authorDID,
	}
}

// remove removes a fresh indexed post and returns the label row it published,
// read back from the labels table.
func (d *streamLabelDecider) remove(t *testing.T) committedLabel {
	t.Helper()
	return d.removeWithin(t, t.Context())
}

// removeWithin is remove with the decision call bounded by ctx.
func (d *streamLabelDecider) removeWithin(t *testing.T, ctx context.Context) committedLabel {
	t.Helper()
	d.decisions++
	subject := indexedModerationPost(t, d.db, moderation.PostV2Collection, d.communityDID, d.authorDID, "label stream", "")
	_, err := d.service.RemoveContent(ctx, fixtures.DID("labelstreamadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: fmt.Sprintf("label-stream-remove-%d", d.decisions),
		Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	var seq int64
	require.NoError(t, d.db.QueryRowContext(t.Context(), `SELECT seq FROM labels WHERE uri = $1`, subject.URI).Scan(&seq))
	return committedLabel{seq: seq, uri: subject.URI}
}

func (d *streamLabelDecider) removeMany(t *testing.T, count int) []committedLabel {
	t.Helper()
	committed := make([]committedLabel, count)
	for index := range committed {
		committed[index] = d.remove(t)
	}
	return committed
}

// nextLabelFrame reads the next frame, failing if none arrives in time or the
// server ends the connection.
func nextLabelFrame(t *testing.T, stream *testkit.LabelStream, description string) testkit.LabelStreamFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	frame, err := stream.Next(ctx)
	require.NoError(t, err, description)
	return frame
}

// requireLabelFrame checks one #labels frame against the committed row it must
// carry, and verifies the label's signature with the issuance public key.
func requireLabelFrame(t *testing.T, frame testkit.LabelStreamFrame, want committedLabel) {
	t.Helper()
	requireLabelFrameSignedBy(t, frame, want, issuanceLabelPublicKey(t), "a streamed label must verify against the issuance key")
}

// requireLabelFrameSignedBy is requireLabelFrame verifying the label's
// signature with signer.
func requireLabelFrameSignedBy(t *testing.T, frame testkit.LabelStreamFrame, want committedLabel, signer atcrypto.PublicKey, description string) {
	t.Helper()
	assert.Equal(t, websocket.BinaryMessage, frame.MessageType, "event-stream frames are binary")
	require.Equal(t, testkit.LabelStreamHeader{Op: 1, T: "#labels", HasT: true}, frame.Header)
	require.Equal(t, want.seq, frame.Seq, "frames must carry the stored seq, in order")
	require.Len(t, frame.Labels, 1)
	label := frame.Labels[0]
	assert.Equal(t, want.uri, label.Uri)
	assert.Equal(t, fixtures.InstanceDID(), label.Src)
	assert.Equal(t, "!takedown", label.Val)
	decoded := labeling.FromLexicon(&label)
	require.NoError(t, decoded.VerifySignature(signer), description)
}

// requireNoLabelFrame asserts that nothing more arrives on stream for window.
func requireNoLabelFrame(t *testing.T, stream *testkit.LabelStream, window time.Duration, description string) {
	t.Helper()
	testkit.Holds(t, window, func() (bool, error) {
		expired, cancel := context.WithCancel(t.Context())
		cancel()
		frame, err := stream.Next(expired)
		if errors.Is(err, context.Canceled) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("unexpected frame: header %+v seq %d", frame.Header, frame.Seq)
	}, testkit.WithDescription("%s", description))
}

func TestLabelStreamReplaysThenTailsWithoutGapOrDuplicate(t *testing.T) {
	db := testkit.DB(t)
	decider := newStreamLabelDecider(t, db)
	seeded := decider.removeMany(t, 5)

	// The replay's second page (the read after rows 1 and 2) is held.
	source := newGatedLabelSource(db, func(afterSeq int64) bool { return afterSeq == seeded[1].seq }, false)
	baseURL, _ := newLabelStreamServer(t, source, labels.StreamOptions{PageSize: 2, PollInterval: labelStreamPollInterval})

	cursor := int64(0)
	stream, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")

	waitForGate(t, source, "the replay's second page read is held")
	duringReplay := decider.remove(t)
	source.release()

	want := append(append([]committedLabel{}, seeded...), duringReplay)
	for index, row := range want {
		requireLabelFrame(t, nextLabelFrame(t, stream, fmt.Sprintf("replayed row %d", index+1)), row)
	}

	afterReplay := decider.remove(t)
	requireLabelFrame(t, nextLabelFrame(t, stream, "the row committed after replay drained"), afterReplay)
	requireNoLabelFrame(t, stream, 500*time.Millisecond, "no frame beyond the seven committed rows")
}

func TestLabelStreamFutureCursorSendsErrorFrameThenCloses(t *testing.T) {
	db := testkit.DB(t)
	decider := newStreamLabelDecider(t, db)
	latest := decider.remove(t)
	baseURL, _ := newLabelStreamServer(t, postgres.NewLabelRepository(db), labels.StreamOptions{PollInterval: labelStreamPollInterval})

	cursor := latest.seq + 1000
	stream, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "a future cursor is refused on the stream, after the upgrade")

	frame := nextLabelFrame(t, stream, "the error frame")
	assert.Equal(t, websocket.BinaryMessage, frame.MessageType, "event-stream frames are binary")
	assert.Equal(t, testkit.LabelStreamHeader{Op: -1}, frame.Header, "an error frame has op -1 and no t")
	assert.Equal(t, "FutureCursor", frame.Error)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = stream.Next(ctx)
	require.ErrorIs(t, err, testkit.ErrLabelStreamClosed, "the server must close the connection after the error frame")

	// A cursor equal to the latest seq is a caught-up consumer, not a future
	// cursor: no frame until the next committed row, which then arrives.
	caughtUpCursor := latest.seq
	caughtUp, err := testkit.DialLabelStream(t, t.Context(), baseURL, &caughtUpCursor)
	require.NoError(t, err, "subscribeLabels must accept a cursor equal to the latest seq")
	requireNoLabelFrame(t, caughtUp, 500*time.Millisecond, "no frame, and no FutureCursor error, for a caught-up cursor")
	next := decider.remove(t)
	requireLabelFrame(t, nextLabelFrame(t, caughtUp, "the row committed after the caught-up subscription"), next)
}

func TestLabelStreamWithoutCursorDeliversOnlyNewRows(t *testing.T) {
	db := testkit.DB(t)
	decider := newStreamLabelDecider(t, db)
	decider.removeMany(t, 2)
	baseURL, _ := newLabelStreamServer(t, postgres.NewLabelRepository(db), labels.StreamOptions{PollInterval: labelStreamPollInterval})

	stream, err := testkit.DialLabelStream(t, t.Context(), baseURL, nil)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")

	committed := decider.remove(t)
	requireLabelFrame(t, nextLabelFrame(t, stream, "the row committed after connecting"), committed)
	requireNoLabelFrame(t, stream, 500*time.Millisecond, "no other frame after the new row")
}

// labelSeq reads the stored seq of the one label published for uri.
func labelSeq(t *testing.T, db *sql.DB, uri string) committedLabel {
	t.Helper()
	var seq int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT seq FROM labels WHERE uri = $1`, uri).Scan(&seq))
	return committedLabel{seq: seq, uri: uri}
}

// TestLabelStreamConcurrentDecisionsReachSubscribersOnceAcrossReconnect holds
// one decision transaction open with its label row written while a second
// decision waits behind it. The task's literal "B commits while A is held" is
// impossible under the label allocation lock every label writer takes (Task
// 04): B is started concurrently and waits on that lock, which is the
// invariant the stream's committed-prefix reads rely on.
func TestLabelStreamConcurrentDecisionsReachSubscribersOnceAcrossReconnect(t *testing.T) {
	db := testkit.DB(t)
	var background sync.WaitGroup
	t.Cleanup(background.Wait)
	decider := newStreamLabelDecider(t, db)
	first := decider.remove(t)
	baseURL, _ := newLabelStreamServer(t, postgres.NewLabelRepository(db), labels.StreamOptions{PollInterval: labelStreamPollInterval})

	cursor := int64(0)
	firstConnection, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")
	var observed []int64
	frame := nextLabelFrame(t, firstConnection, "the pre-existing row")
	requireLabelFrame(t, frame, first)
	observed = append(observed, frame.Seq)

	heldSubject := indexedModerationPost(t, db, moderation.PostV2Collection, decider.communityDID, decider.authorDID, "label stream held", "")
	waitingSubject := indexedModerationPost(t, db, moderation.PostV2Collection, decider.communityDID, decider.authorDID, "label stream waiting", "")
	heldStore := newHoldingLabelStore(t, db, holdAfterLabelInsert, heldSubject.URI)
	heldService := newPostgresModerationServiceWith(db, heldStore, issuanceLabelSigningKey(t))
	ctx := context.Background()

	held := runInBackground(&background, func() error {
		_, err := heldService.RemoveContent(ctx, fixtures.DID("labelstreamadmina"), moderation.RemoveContentRequest{
			Subject: heldSubject, ExpectedVersion: "v0", IdempotencyKey: "label-stream-held-remove", Reason: moderationConcurrencyReason,
		})
		return err
	})
	waitForHold(t, heldStore, held, "decision A holding open with its label row written")

	waiting := runInBackground(&background, func() error {
		_, err := decider.service.RemoveContent(ctx, fixtures.DID("labelstreamadminb"), moderation.RemoveContentRequest{
			Subject: waitingSubject, ExpectedVersion: "v0", IdempotencyKey: "label-stream-waiting-remove", Reason: moderationConcurrencyReason,
		})
		return err
	})
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case err := <-waiting:
			return false, fmt.Errorf("decision B finished while A held the label allocation lock: %v", err)
		default:
		}
		waiters, err := ungrantedAdvisoryLockWaiters(t.Context(), db)
		return waiters > 0, err
	}, testkit.WithDescription("decision B waiting on the label allocation lock"))

	requireNoLabelFrame(t, firstConnection, 500*time.Millisecond, "no frame past the pre-existing row while decision A is open")

	heldStore.release()
	require.NoError(t, awaitCall(t, held, "decision A to commit"))
	require.NoError(t, awaitCall(t, waiting, "decision B to commit"))
	heldRow, waitingRow := labelSeq(t, db, heldSubject.URI), labelSeq(t, db, waitingSubject.URI)

	frame = nextLabelFrame(t, firstConnection, "decision A's row")
	requireLabelFrame(t, frame, heldRow)
	observed = append(observed, frame.Seq)
	firstConnection.Close()

	cursor = heldRow.seq
	secondConnection, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a reconnect from the last received seq")
	frame = nextLabelFrame(t, secondConnection, "decision B's row after reconnecting")
	requireLabelFrame(t, frame, waitingRow)
	observed = append(observed, frame.Seq)

	live := decider.remove(t)
	frame = nextLabelFrame(t, secondConnection, "the row committed after reconnecting")
	requireLabelFrame(t, frame, live)
	observed = append(observed, frame.Seq)
	requireNoLabelFrame(t, secondConnection, 500*time.Millisecond, "no frame beyond the live row")

	assert.Equal(t, []int64{first.seq, heldRow.seq, waitingRow.seq, live.seq}, observed,
		"across both connections every committed row is observed exactly once, in seq order")
}

// TestLabelStreamResumesAcrossShutdownAndKeyRotation shuts the stream down
// mid-replay, rotates the signing key at the next boot, and resumes from the
// last received seq on a new stream.
func TestLabelStreamResumesAcrossShutdownAndKeyRotation(t *testing.T) {
	db := testkit.DB(t)
	_, err := startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err, "a publisher must start on an empty store under the issuance key")
	decider := newStreamLabelDecider(t, db)
	seeded := decider.removeMany(t, 6)

	// The replay's second page (the read after rows 1 and 2) is held.
	source := newGatedLabelSource(db, func(afterSeq int64) bool { return afterSeq == seeded[1].seq }, false)
	baseURL, firstStream := newLabelStreamServer(t, source, labels.StreamOptions{PageSize: 2, PollInterval: labelStreamPollInterval})

	cursor := int64(0)
	connection, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")
	for index, row := range seeded[:2] {
		requireLabelFrame(t, nextLabelFrame(t, connection, fmt.Sprintf("replayed row %d before shutdown", index+1)), row)
	}
	waitForGate(t, source, "the replay's second page read is held")

	firstStream.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, err = connection.Next(ctx)
		if err != nil {
			break
		}
	}
	require.ErrorIs(t, err, testkit.ErrLabelStreamClosed, "shutting the stream down must end its open connections, not leave them idle")
	source.release()

	_, err = startLabelPublisher(t, db, labelPrivateKey(t, rotationLabelKeyPrivateMultibase), issuanceLabelKeyPublicMultibase)
	require.NoError(t, err, "the next boot rotates to the new key, retiring the issuance key")
	resumedURL, _ := newLabelStreamServer(t, postgres.NewLabelRepository(db), labels.StreamOptions{PageSize: 2, PollInterval: labelStreamPollInterval})

	cursor = seeded[1].seq
	resumed, err := testkit.DialLabelStream(t, t.Context(), resumedURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a reconnect after the restart")

	rotated := *decider
	rotated.service = newPostgresModerationServiceWith(db, postgres.NewModerationRepository(db), labelPrivateKey(t, rotationLabelKeyPrivateMultibase))
	requireRotated := func(frame testkit.LabelStreamFrame, want committedLabel) {
		t.Helper()
		requireLabelFrameSignedBy(t, frame, want, labelPublicKey(t, rotationLabelKeyPublicMultibase), "a label streamed after rotation must verify against the rotation key")
		label := frame.Labels[0]
		decoded := labeling.FromLexicon(&label)
		assert.Error(t, decoded.VerifySignature(issuanceLabelPublicKey(t)), "a label streamed after rotation must no longer verify against the retired key")
	}
	for index, row := range seeded[2:] {
		requireRotated(nextLabelFrame(t, resumed, fmt.Sprintf("replayed row %d after the restart", index+3)), row)
	}
	live := rotated.remove(t)
	requireRotated(nextLabelFrame(t, resumed, "the row committed under the rotation key"), live)
	requireNoLabelFrame(t, resumed, 500*time.Millisecond, "no frame beyond the live row")
}

// TestLabelStreamSlowSubscriberStallsOnlyItself holds one subscriber's replay
// read and checks that decisions still commit within a bound and reach a
// healthy subscriber.
func TestLabelStreamSlowSubscriberStallsOnlyItself(t *testing.T) {
	db := testkit.DB(t)
	decider := newStreamLabelDecider(t, db)
	decider.removeMany(t, 2)

	// Every read from the start of the log (only the slow subscriber's cursor-0
	// replay) is held until cleanup.
	source := newGatedLabelSource(db, func(afterSeq int64) bool { return afterSeq == 0 }, true)
	baseURL, _ := newLabelStreamServer(t, source, labels.StreamOptions{PollInterval: labelStreamPollInterval})

	cursor := int64(0)
	slow, err := testkit.DialLabelStream(t, t.Context(), baseURL, &cursor)
	require.NoError(t, err, "subscribeLabels must accept a WebSocket subscription")
	waitForGate(t, source, "the slow subscriber's replay read is held")

	healthy, err := testkit.DialLabelStream(t, t.Context(), baseURL, nil)
	require.NoError(t, err, "subscribeLabels must accept a second subscription")

	committed := make([]committedLabel, 3)
	for index := range committed {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		committed[index] = decider.removeWithin(t, ctx)
		cancel()
	}
	for index, row := range committed {
		requireLabelFrame(t, nextLabelFrame(t, healthy, fmt.Sprintf("live row %d on the healthy subscriber", index+1)), row)
	}
	requireNoLabelFrame(t, slow, 200*time.Millisecond, "the slow subscriber is still held in its replay")
}
