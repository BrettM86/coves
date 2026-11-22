//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/communitysignals"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signalClock is a fixed clock the test moves by hand.
type signalClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *signalClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *signalClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// signalSender records every send attempt and fails while err is set.
type signalSender struct {
	mu        sync.Mutex
	err       error
	attempts  []string
	delivered []string
}

func (s *signalSender) SendMessage(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, text)
	if s.err != nil {
		return s.err
	}
	s.delivered = append(s.delivered, text)
	return nil
}

func (s *signalSender) SetErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *signalSender) Attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempts...)
}

func (s *signalSender) Delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}

// signalInstanceDomain is the local instance domain the signal repository is
// built with: a community with this origin is native.
const signalInstanceDomain = "coves.social"

func signalLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func signalConfig() communitysignals.Config {
	return communitysignals.Config{
		AlertsEnabled:       true,
		SweepInterval:       5 * time.Minute,
		AttemptLimit:        3,
		RetryBaseDelay:      time.Minute,
		DigestMinPosts:      10,
		DigestRiseThreshold: 20,
	}
}

// createFlaggedCommunity indexes a community whose record carries the nsfw
// content warning through the production repository, which records one
// false->true flag event.
func createFlaggedCommunity(t *testing.T, db *sql.DB, label, origin string) (did, handle string) {
	t.Helper()
	suffix := testkit.UniqueID(t)
	name := label + suffix
	did = "did:plc:" + name
	handle = name + ".lemmy-world.tdpl.io"
	_, err := NewCommunityRepository(db, credentialciphertest.Fixed()).Create(context.Background(), &communities.Community{
		DID:             did,
		Handle:          handle,
		Name:            name,
		DisplayName:     "Signal " + name,
		Description:     "signal fixture " + name,
		OwnerDID:        did,
		CreatedByDID:    "did:plc:signalcreator",
		HostedByDID:     "did:web:tdpl.io",
		PDSURL:          "https://pds.tdpl.io",
		Visibility:      "public",
		Origin:          origin,
		ContentWarnings: []string{"nsfw"},
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	})
	require.NoError(t, err, "indexing the nsfw community fixture")
	return did, handle
}

type flagEventRow struct {
	ID            int64
	OriginHost    sql.NullString
	OldNSFW       bool
	NewNSFW       bool
	OccurredAt    time.Time
	DeliveredAt   sql.NullTime
	Attempts      int
	LastErrorCode sql.NullString
	NextAttemptAt time.Time
}

// readFlagEvents reads the documented event columns for one community.
func readFlagEvents(t *testing.T, db *sql.DB, communityDID string) []flagEventRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT id, origin_host, old_nsfw, new_nsfw, occurred_at, delivered_at,
		       attempts, last_error_code, next_attempt_at
		FROM community_nsfw_flag_events
		WHERE community_did = $1
		ORDER BY id`, communityDID)
	require.NoError(t, err, "reading community_nsfw_flag_events")
	defer rows.Close()
	var out []flagEventRow
	for rows.Next() {
		var r flagEventRow
		require.NoError(t, rows.Scan(&r.ID, &r.OriginHost, &r.OldNSFW, &r.NewNSFW, &r.OccurredAt,
			&r.DeliveredAt, &r.Attempts, &r.LastErrorCode, &r.NextAttemptAt))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func onlyFlagEvent(t *testing.T, db *sql.DB, communityDID string) flagEventRow {
	t.Helper()
	events := readFlagEvents(t, db, communityDID)
	require.Len(t, events, 1, "community %s should have exactly one flag event", communityDID)
	return events[0]
}

// TestCommunitySignalSweep_RetriesWithBackoffThenDeliversOnce is B7: a failed
// send is recorded with a backoff, a not-yet-due event is left alone, a
// delivered event is never resent, and an event at the attempt limit is
// abandoned undelivered.
func TestCommunitySignalSweep_RetriesWithBackoffThenDeliversOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	firstDID, firstHandle := createFlaggedCommunity(t, db, "sweepfirst", "lemmy.world")

	t0 := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	clock := &signalClock{now: t0}
	sender := &signalSender{err: errors.New("telegram unavailable")}
	svc := communitysignals.NewService(NewCommunitySignalRepository(db, signalInstanceDomain), sender, signalConfig(), clock.Now, signalLogger())

	// t0: the send fails; one attempt, retry in base delay.
	require.NoError(t, svc.Sweep(ctx))
	require.Len(t, sender.Attempts(), 1, "Sweep at t0 must attempt the due event once")
	event := onlyFlagEvent(t, db, firstDID)
	assert.Equal(t, 1, event.Attempts)
	assert.Equal(t, "send_failed", event.LastErrorCode.String)
	assert.True(t, event.NextAttemptAt.UTC().Equal(t0.Add(time.Minute)),
		"next_attempt_at = %s, want t0+1m %s", event.NextAttemptAt.UTC(), t0.Add(time.Minute))
	assert.False(t, event.DeliveredAt.Valid)

	// t0+30s: not due yet.
	clock.Set(t0.Add(30 * time.Second))
	require.NoError(t, svc.Sweep(ctx))
	require.Len(t, sender.Attempts(), 1, "an event before its next_attempt_at must not be sent")

	// t0+1m: fails again; backoff doubles.
	clock.Set(t0.Add(time.Minute))
	require.NoError(t, svc.Sweep(ctx))
	require.Len(t, sender.Attempts(), 2, "Sweep at the retry time must attempt again")
	event = onlyFlagEvent(t, db, firstDID)
	assert.Equal(t, 2, event.Attempts)
	assert.Equal(t, "send_failed", event.LastErrorCode.String)
	assert.True(t, event.NextAttemptAt.UTC().Equal(t0.Add(3*time.Minute)),
		"next_attempt_at = %s, want t0+1m+2m %s", event.NextAttemptAt.UTC(), t0.Add(3*time.Minute))

	// Due again with a working sender: delivered once.
	sender.SetErr(nil)
	clock.Set(t0.Add(3 * time.Minute))
	require.NoError(t, svc.Sweep(ctx))
	delivered := sender.Delivered()
	require.Len(t, delivered, 1, "the due event must be delivered once the sender works")
	assert.Contains(t, delivered[0], firstHandle)
	event = onlyFlagEvent(t, db, firstDID)
	assert.True(t, event.DeliveredAt.Valid, "delivered_at must be set after a successful send")

	// Later sweep: no resend.
	clock.Set(t0.Add(time.Hour))
	require.NoError(t, svc.Sweep(ctx))
	assert.Len(t, sender.Attempts(), 3, "a delivered event must not be sent again")

	// Second event reaches the attempt limit and is abandoned.
	secondDID, _ := createFlaggedCommunity(t, db, "sweeplimit", "lemmy.world")
	t1 := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	sender.SetErr(errors.New("telegram unavailable"))
	for i, at := range []time.Time{t1, t1.Add(time.Minute), t1.Add(3 * time.Minute)} {
		clock.Set(at)
		require.NoError(t, svc.Sweep(ctx))
		require.Len(t, sender.Attempts(), 4+i, "attempt %d for the second event", i+1)
	}
	limited := onlyFlagEvent(t, db, secondDID)
	assert.Equal(t, 3, limited.Attempts)

	sender.SetErr(nil)
	clock.Set(t1.Add(24 * time.Hour))
	require.NoError(t, svc.Sweep(ctx))
	assert.Len(t, sender.Attempts(), 6, "an event at the attempt limit must not be sent again")
	limited = onlyFlagEvent(t, db, secondDID)
	assert.Equal(t, 3, limited.Attempts)
	assert.False(t, limited.DeliveredAt.Valid, "an abandoned event stays undelivered")
}

// blockingSignalSender counts every send. The first send signals entered and
// waits for release; later sends return at once.
type blockingSignalSender struct {
	mu       sync.Mutex
	attempts []string
	entered  chan struct{}
	release  chan struct{}
}

func (s *blockingSignalSender) SendMessage(ctx context.Context, text string) error {
	s.mu.Lock()
	s.attempts = append(s.attempts, text)
	first := len(s.attempts) == 1
	s.mu.Unlock()
	if !first {
		return nil
	}
	close(s.entered)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingSignalSender) Attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempts...)
}

// TestCommunitySignalDelivery_SweepDuringAfterCommitSendSendsOnce: a sweep
// that runs while the after-commit send of an event is in flight must not
// send that event again.
func TestCommunitySignalDelivery_SweepDuringAfterCommitSendSendsOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	did, handle := createFlaggedCommunity(t, db, "claimonce", "lemmy.world")

	clock := &signalClock{now: time.Now().UTC().Truncate(time.Second).Add(time.Hour)}
	sender := &blockingSignalSender{entered: make(chan struct{}), release: make(chan struct{})}
	svc := communitysignals.NewService(NewCommunitySignalRepository(db, signalInstanceDomain), sender, signalConfig(), clock.Now, signalLogger())

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		svc.DeliverPending(ctx, did)
	}()
	select {
	case <-sender.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("DeliverPending never sent the due event")
	}

	require.NoError(t, svc.Sweep(ctx), "sweep while the after-commit send is in flight")
	close(sender.release)
	<-delivered

	attempts := sender.Attempts()
	require.Len(t, attempts, 1, "the event must be sent exactly once")
	assert.Contains(t, attempts[0], handle)
	event := onlyFlagEvent(t, db, did)
	assert.True(t, event.DeliveredAt.Valid, "delivered_at must be set after the send")
	assert.Equal(t, 0, event.Attempts, "a successful send records no failed attempt")
}

// gatedFailingSender fails every send. A send whose 1-based number is in gates
// signals entered on its channel and waits for release on its channel.
type gatedFailingSender struct {
	mu       sync.Mutex
	attempts []string
	entered  map[int]chan struct{}
	release  map[int]chan struct{}
}

func newGatedFailingSender(gates ...int) *gatedFailingSender {
	s := &gatedFailingSender{entered: map[int]chan struct{}{}, release: map[int]chan struct{}{}}
	for _, n := range gates {
		s.entered[n] = make(chan struct{})
		s.release[n] = make(chan struct{})
	}
	return s
}

func (s *gatedFailingSender) SendMessage(ctx context.Context, text string) error {
	s.mu.Lock()
	s.attempts = append(s.attempts, text)
	n := len(s.attempts)
	s.mu.Unlock()
	if entered, ok := s.entered[n]; ok {
		close(entered)
		select {
		case <-s.release[n]:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("telegram unavailable")
}

func (s *gatedFailingSender) waitEntered(t *testing.T, n int) {
	t.Helper()
	select {
	case <-s.entered[n]:
	case <-time.After(10 * time.Second):
		t.Fatalf("send %d never started", n)
	}
}

func (s *gatedFailingSender) sendsContaining(handle string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, text := range s.attempts {
		if strings.Contains(text, handle) {
			count++
		}
	}
	return count
}

// TestCommunitySignalSweep_LongRunOutlivingFirstLeaseSendsEachEventOnce: a
// sweep whose run lasts past the lease its first claim took must not leave
// later events open to another caller's claim, so each event is sent once and
// counts one attempt.
func TestCommunitySignalSweep_LongRunOutlivingFirstLeaseSendsEachEventOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	_, firstHandle := createFlaggedCommunity(t, db, "leasefirst", "lemmy.world")
	secondDID, secondHandle := createFlaggedCommunity(t, db, "leasesecond", "lemmy.world")

	t0 := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	clock := &signalClock{now: t0}
	sender := newGatedFailingSender(1, 2)
	repo := NewCommunitySignalRepository(db, signalInstanceDomain)
	// A ten-minute backoff keeps a failed event out of the second sweep.
	cfg := signalConfig()
	cfg.RetryBaseDelay = 10 * time.Minute
	slow := communitysignals.NewService(repo, sender, cfg, clock.Now, signalLogger())
	other := communitysignals.NewService(repo, sender, cfg, clock.Now, signalLogger())

	swept := make(chan error, 1)
	go func() { swept <- slow.Sweep(ctx) }()

	// The first send takes four minutes, inside any lease taken at t0.
	sender.waitEntered(t, 1)
	clock.Set(t0.Add(4 * time.Minute))
	close(sender.release[1])

	// The second send is still running at t0+6m, past a lease taken at t0.
	sender.waitEntered(t, 2)
	clock.Set(t0.Add(6 * time.Minute))
	require.NoError(t, other.Sweep(ctx), "a second caller sweeping while the slow sweep sends")
	close(sender.release[2])
	require.NoError(t, <-swept)

	assert.Equal(t, 1, sender.sendsContaining(firstHandle), "the first event must be sent once")
	assert.Equal(t, 1, sender.sendsContaining(secondHandle), "the second event must be sent once")
	second := onlyFlagEvent(t, db, secondDID)
	assert.Equal(t, 1, second.Attempts, "one failed send counts one attempt")
	assert.True(t, second.NextAttemptAt.UTC().Equal(t0.Add(16*time.Minute)),
		"next_attempt_at = %s, want the slow sweep's backoff t0+16m", second.NextAttemptAt.UTC())
}

// TestCommunitySignalSweep_StaleLeaseOutcomeDoesNotOverwriteNewerClaim: a
// send that outlives its lease while another caller reclaims and fails the
// event must not record a second attempt or overwrite the newer backoff.
func TestCommunitySignalSweep_StaleLeaseOutcomeDoesNotOverwriteNewerClaim(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	did, _ := createFlaggedCommunity(t, db, "leasestale", "lemmy.world")

	t0 := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	clock := &signalClock{now: t0}
	sender := newGatedFailingSender(1)
	repo := NewCommunitySignalRepository(db, signalInstanceDomain)
	stale := communitysignals.NewService(repo, sender, signalConfig(), clock.Now, signalLogger())
	newer := communitysignals.NewService(repo, sender, signalConfig(), clock.Now, signalLogger())

	swept := make(chan error, 1)
	go func() { swept <- stale.Sweep(ctx) }()
	sender.waitEntered(t, 1)

	// Past the stale caller's lease, the newer caller reclaims and fails it.
	clock.Set(t0.Add(6 * time.Minute))
	require.NoError(t, newer.Sweep(ctx))
	event := onlyFlagEvent(t, db, did)
	require.Equal(t, 1, event.Attempts)
	require.True(t, event.NextAttemptAt.UTC().Equal(t0.Add(7*time.Minute)))

	// Before the newer backoff ends, so the stale sweep finds nothing more due.
	clock.Set(t0.Add(6*time.Minute + 30*time.Second))
	close(sender.release[1])
	require.NoError(t, <-swept)

	event = onlyFlagEvent(t, db, did)
	assert.Equal(t, 1, event.Attempts, "the stale caller's failure must not count a second attempt")
	assert.True(t, event.NextAttemptAt.UTC().Equal(t0.Add(7*time.Minute)),
		"next_attempt_at = %s, want the newer claim's backoff t0+7m", event.NextAttemptAt.UTC())
}

const (
	labelNSFW         = `{"values":[{"val":"nsfw"}]}`
	labelNSFWNegFalse = `{"values":[{"val":"nsfw","neg":false}]}`
	labelNSFWAndOther = `{"values":[{"val":"spoiler"},{"val":"nsfw"}]}`
	labelNSFWNegated  = `{"values":[{"val":"nsfw","neg":true}]}`
	labelSpoiler      = `{"values":[{"val":"spoiler"}]}`
)

type signalPost struct {
	at      time.Time
	labels  string // "" = no labels
	deleted bool
}

func repeatPost(n int, p signalPost) []signalPost {
	out := make([]signalPost, n)
	for i := range out {
		out[i] = p
		out[i].at = p.at.Add(-time.Duration(i) * time.Minute)
	}
	return out
}

func insertSignalPosts(t *testing.T, db *sql.DB, communityDID, authorDID string, posts []signalPost) {
	t.Helper()
	for _, p := range posts {
		rkey := "sp" + testkit.UniqueID(t)
		var labels any
		if p.labels != "" {
			labels = p.labels
		}
		var deletedAt any
		if p.deleted {
			deletedAt = p.at
		}
		_, err := db.Exec(`
			INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, content_labels, deleted_at)
			VALUES ($1, $2, $3, $4, $5, 'signal post', $6, $7::jsonb, $8)`,
			"at://"+authorDID+"/social.coves.community.post/"+rkey, "bafy"+rkey, rkey,
			authorDID, communityDID, p.at, labels, deletedAt)
		require.NoError(t, err, "seeding post")
	}
}

// insertBulkSignalPosts inserts total posts, the first nsfw of them labelled
// nsfw, created one minute apart going back from newest.
func insertBulkSignalPosts(t *testing.T, db *sql.DB, communityDID, authorDID string, newest time.Time, total, nsfw int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, content_labels)
		SELECT 'at://' || $2 || '/social.coves.community.post/' || $1 || '-' || g,
		       'bafy' || $1 || '-' || g, $1 || '-' || g, $2, $1, 'digest post',
		       $3::timestamptz - make_interval(mins => g),
		       CASE WHEN g <= $5 THEN '{"values":[{"val":"nsfw"}]}'::jsonb ELSE NULL END
		FROM generate_series(1, $4) AS g`,
		communityDID, authorDID, newest, total, nsfw)
	require.NoError(t, err, "bulk seeding posts")
}

// TestCommunitySignalDigestRows_SharesWindowsAndThreshold is B9.
func TestCommunitySignalDigestRows_SharesWindowsAndThreshold(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	id := testkit.UniqueID(t)

	now := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	recent := now.Add(-24 * time.Hour)
	prior := now.Add(-14 * 24 * time.Hour)
	weekAgo := now.Add(-7 * 24 * time.Hour)
	fiveWeeksAgo := now.Add(-35 * 24 * time.Hour)

	authorDID := "did:plc:digestauthor" + id
	createTestUser(t, db, "digestauthor-"+id+".test", authorDID)

	community := func(label string) (string, string) {
		did := "did:plc:digest" + label + id
		handle := "digest" + label + "-" + id + ".coves.social"
		createTestCommunity(t, db, did, handle, did)
		return did, handle
	}
	aDID, aHandle := community("a")
	bDID, _ := community("b")
	cDID, _ := community("c")
	dDID, dHandle := community("d")
	eDID, _ := community("e")
	fDID, _ := community("f")
	gDID, gHandle := community("g")
	_, err := db.Exec(`UPDATE communities SET origin = 'lemmy.world' WHERE did = $1`, aDID)
	require.NoError(t, err)
	// G is native with its origin stored as the instance domain, as the
	// community service stores it; D has no origin.
	_, err = db.Exec(`UPDATE communities SET origin = $2 WHERE did = $1`, gDID, strings.ToUpper(signalInstanceDomain))
	require.NoError(t, err)

	// A: recent 10 posts, 6 nsfw (one exactly at now-7d); prior 10 posts, 1
	// nsfw (exactly at now-35d). Posts at now, after now, deleted, and older
	// than 35 days are all labelled nsfw and must not count.
	var a []signalPost
	a = append(a, signalPost{at: weekAgo, labels: labelNSFW})
	a = append(a, repeatPost(3, signalPost{at: recent, labels: labelNSFW})...)
	a = append(a, signalPost{at: recent, labels: labelNSFWNegFalse})
	a = append(a, signalPost{at: recent, labels: labelNSFWAndOther})
	a = append(a, repeatPost(2, signalPost{at: recent})...)
	a = append(a, signalPost{at: recent, labels: labelSpoiler})
	a = append(a, signalPost{at: recent, labels: labelNSFWNegated})
	a = append(a, signalPost{at: now, labels: labelNSFW})
	a = append(a, signalPost{at: now.Add(time.Hour), labels: labelNSFW})
	a = append(a, repeatPost(2, signalPost{at: recent, labels: labelNSFW, deleted: true})...)
	a = append(a, signalPost{at: fiveWeeksAgo, labels: labelNSFW})
	a = append(a, repeatPost(9, signalPost{at: prior})...)
	a = append(a, repeatPost(3, signalPost{at: fiveWeeksAgo.Add(-time.Second), labels: labelNSFW})...)
	a = append(a, signalPost{at: prior, labels: labelNSFW, deleted: true})
	insertSignalPosts(t, db, aDID, authorDID, a)

	// B: 9 recent nsfw posts plus a deleted one: below the minimum.
	insertSignalPosts(t, db, bDID, authorDID, append(
		repeatPost(9, signalPost{at: recent, labels: labelNSFW}),
		signalPost{at: recent, labels: labelNSFW, deleted: true}))

	// C: recent 0.3, prior 0.1: exactly +20 points, not above the threshold.
	insertSignalPosts(t, db, cDID, authorDID, append(append(append(
		repeatPost(3, signalPost{at: recent, labels: labelNSFW}),
		repeatPost(7, signalPost{at: recent})...),
		signalPost{at: prior, labels: labelNSFW}),
		repeatPost(9, signalPost{at: prior})...))

	// D: recent 4/10, prior 3/20: +25 points.
	insertSignalPosts(t, db, dDID, authorDID, append(append(append(
		repeatPost(4, signalPost{at: recent, labels: labelNSFW}),
		repeatPost(6, signalPost{at: recent})...),
		repeatPost(3, signalPost{at: prior, labels: labelNSFW})...),
		repeatPost(17, signalPost{at: prior})...))

	// E: 10 recent posts whose nsfw label is negated: share 0.
	insertSignalPosts(t, db, eDID, authorDID, repeatPost(10, signalPost{at: recent, labels: labelNSFWNegated}))

	// F: share doubles 0.1 -> 0.2: +100% relative but only +10 points.
	insertSignalPosts(t, db, fDID, authorDID, append(append(append(
		repeatPost(2, signalPost{at: recent, labels: labelNSFW}),
		repeatPost(8, signalPost{at: recent})...),
		signalPost{at: prior, labels: labelNSFW}),
		repeatPost(9, signalPost{at: prior})...))

	// G: recent 3/10, no prior posts: prior share 0, +30 points.
	insertSignalPosts(t, db, gDID, authorDID, append(
		repeatPost(3, signalPost{at: recent, labels: labelNSFW}),
		repeatPost(7, signalPost{at: recent})...))

	rows, err := NewCommunitySignalRepository(db, signalInstanceDomain).DigestRows(ctx, now, 10, 20)
	require.NoError(t, err)

	handles := make([]string, len(rows))
	for i, r := range rows {
		handles[i] = r.CommunityHandle
	}
	require.Equal(t, []string{aHandle, gHandle, dHandle}, handles,
		"qualifying communities, ordered by rise in points descending")

	type want struct {
		did, origin   string
		recentPosts   int
		recent, prior float64
	}
	for i, w := range []want{
		{aDID, "lemmy.world", 10, 0.6, 0.1},
		{gDID, "", 10, 0.3, 0},
		{dDID, "", 10, 0.4, 0.15},
	} {
		r := rows[i]
		assert.Equal(t, w.did, r.CommunityDID, "row %d DID", i)
		assert.Equal(t, w.origin, r.OriginHost, "row %d origin", i)
		assert.Equal(t, w.recentPosts, r.RecentPosts, "row %d recent posts", i)
		assert.InDelta(t, w.recent, r.RecentShare, 1e-9, "row %d recent share", i)
		assert.InDelta(t, w.prior, r.PriorShare, 1e-9, "row %d prior share", i)
	}
}

// TestCommunitySignalWeeklyDigest_SendsOncePerSlotAndPersistsMarker is B10.
func TestCommunitySignalWeeklyDigest_SendsOncePerSlotAndPersistsMarker(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	id := testkit.UniqueID(t)

	firstRun := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	authorDID := "did:plc:weeklyauthor" + id
	createTestUser(t, db, "weeklyauthor-"+id+".test", authorDID)

	// Community i has 400 recent posts, 400-i of them nsfw, and no prior
	// posts: a rise of (400-i)/4 points (100 down to 25.25), so the expected
	// order is i ascending.
	const total = 300
	handles := make([]string, total)
	for i := 0; i < total; i++ {
		did := fmt.Sprintf("did:plc:weekly%03d%s", i, id)
		handles[i] = fmt.Sprintf("weekly%03d-%s.coves.social", i, id)
		createTestCommunity(t, db, did, handles[i], did)
		insertBulkSignalPosts(t, db, did, authorDID, firstRun.Add(-time.Hour), 400, 400-i)
	}

	clock := &signalClock{now: firstRun}
	sender := &signalSender{}
	svc := communitysignals.NewService(NewCommunitySignalRepository(db, signalInstanceDomain), sender, signalConfig(), clock.Now, signalLogger())
	require.NoError(t, svc.RunWeeklyDigest(ctx))

	messages := sender.Delivered()
	require.Len(t, messages, 1, "the Monday digest must be sent exactly once")
	text := messages[0]
	assert.Less(t, len(text), 4096, "digest must fit one Telegram message")

	shown := 0
	lastPos := -1
	for i, handle := range handles {
		count := strings.Count(text, handle)
		require.LessOrEqual(t, count, 1, "handle %s appears more than once", handle)
		if count == 0 {
			for _, later := range handles[i+1:] {
				require.NotContains(t, text, later, "shown handles must be a prefix of the rise order")
			}
			break
		}
		pos := strings.Index(text, handle)
		require.Greater(t, pos, lastPos, "handle %s is out of rise order", handle)
		lastPos = pos
		shown++
	}
	require.Positive(t, shown, "the digest must show at least one community")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	assert.Contains(t, lines[len(lines)-1], strconv.Itoa(total-shown), "last line carries the left-out count")

	marker, ok, err := NewCommunitySignalRepository(db, signalInstanceDomain).DigestLastSent(ctx)
	require.NoError(t, err)
	require.True(t, ok, "the digest marker must be persisted")
	assert.True(t, marker.UTC().Equal(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)), "marker = %s", marker.UTC())

	// After a restart later in the same week: nothing is sent.
	midweekSender := &signalSender{}
	midweek := &signalClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	require.NoError(t, communitysignals.NewService(NewCommunitySignalRepository(db, signalInstanceDomain), midweekSender, signalConfig(),
		midweek.Now, signalLogger()).RunWeeklyDigest(ctx))
	assert.Empty(t, midweekSender.Attempts(), "a slot already sent must not be sent again after a restart")

	// Next Monday with a community qualifying that week: sent again.
	nextDID := "did:plc:weeklynext" + id
	nextHandle := "weeklynext-" + id + ".coves.social"
	createTestCommunity(t, db, nextDID, nextHandle, nextDID)
	insertBulkSignalPosts(t, db, nextDID, authorDID, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), 10, 10)

	nextSender := &signalSender{}
	nextWeek := &signalClock{now: time.Date(2026, 11, 23, 9, 5, 0, 0, time.UTC)}
	require.NoError(t, communitysignals.NewService(NewCommunitySignalRepository(db, signalInstanceDomain), nextSender, signalConfig(),
		nextWeek.Now, signalLogger()).RunWeeklyDigest(ctx))
	nextMessages := nextSender.Delivered()
	require.Len(t, nextMessages, 1, "the next Monday's digest must be sent")
	assert.Contains(t, nextMessages[0], nextHandle)

	marker, ok, err = NewCommunitySignalRepository(db, signalInstanceDomain).DigestLastSent(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, marker.UTC().Equal(time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)), "marker = %s", marker.UTC())
}
