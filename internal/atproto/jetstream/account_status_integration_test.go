//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireStoredAccountStatus reads did's restriction through the Postgres
// store and compares it with want (nil: no row).
func requireStoredAccountStatus(t *testing.T, store accountstatus.Reader, did string, want *accountstatus.Status) {
	t.Helper()
	status, found, err := store.Restriction(t.Context(), did)
	require.NoError(t, err)
	if want == nil {
		assert.False(t, found, "expected no stored status restriction, found %q", status)
		return
	}
	require.True(t, found, "expected stored status restriction %q, found none", *want)
	assert.Equal(t, *want, status)
}

// giveTargetPresence gives accountStatusTargetDID Coves presence (a users
// row), so its #account events are confirmed with the PDS.
func giveTargetPresence(t *testing.T, db *sql.DB) {
	t.Helper()
	fixtures.User(t, db, testkit.UniqueIDWithPrefix(t, "astarget")+".test", accountStatusTargetDID)
}

// TestUserConsumer_AccountStatusStoredInPostgres drives #account events through
// the users consumer over the Postgres account status store: a confirmed
// takedown is stored, a confirmed reactivation clears it, and an activation
// the PDS does not confirm keeps it. The rule table itself is
// TestUserConsumer_AccountStatusEvents'.
func TestUserConsumer_AccountStatusStoredInPostgres(t *testing.T) {
	takendown := accountstatus.Takendown
	tests := []struct {
		name        string
		existingRow *accountstatus.Status
		eventActive bool
		eventStatus string
		pdsAnswer   accountstatus.RepoStatus
		wantRow     *accountstatus.Status
	}{
		{
			name:        "takendown event confirmed takendown stores takendown",
			eventStatus: "takendown", pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
		{
			name:        "active event over a stored takedown confirmed active clears it",
			existingRow: &takendown, eventActive: true, pdsAnswer: pdsAnswersActive, wantRow: nil,
		},
		{
			name:        "active event over a stored takedown while PDS still reports takendown keeps it",
			existingRow: &takendown, eventActive: true, pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			giveTargetPresence(t, db)
			if tt.existingRow != nil {
				accountstatusfixture.Set(t, db, accountStatusTargetDID, *tt.existingRow)
			}
			consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
				WithUserAccountStatus(store, &fakeRepoStatusConfirmer{answer: tt.pdsAnswer}))

			require.NoError(t, consumer.HandleEvent(t.Context(), accountStatusEvent(tt.eventActive, tt.eventStatus)))
			requireStoredAccountStatus(t, store, accountStatusTargetDID, tt.wantRow)
		})
	}
}

// switchableRepoStatusPDS is a loopback PDS answering getRepoStatus for
// accountStatusTargetDID with a scripted response the test changes between
// passes, standing in for a PDS that recovers from an outage. Any other path
// or DID gets 404. requests counts the getRepoStatus requests it answered.
type switchableRepoStatusPDS struct {
	server     *httptest.Server
	mutex      sync.Mutex
	statusCode int
	body       string
	requests   int
}

func newSwitchableRepoStatusPDS(t *testing.T, statusCode int, body string) *switchableRepoStatusPDS {
	t.Helper()
	pds := &switchableRepoStatusPDS{statusCode: statusCode, body: body}
	pds.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/xrpc/com.atproto.sync.getRepoStatus" || request.URL.Query().Get("did") != accountStatusTargetDID {
			http.NotFound(writer, request)
			return
		}
		pds.mutex.Lock()
		pds.requests++
		statusCode, body := pds.statusCode, pds.body
		pds.mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(statusCode)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

func (pds *switchableRepoStatusPDS) requestCount() int {
	pds.mutex.Lock()
	defer pds.mutex.Unlock()
	return pds.requests
}

func (pds *switchableRepoStatusPDS) set(statusCode int, body string) {
	pds.mutex.Lock()
	defer pds.mutex.Unlock()
	pds.statusCode, pds.body = statusCode, body
}

// newTargetRepoStatusConfirmer is the real PDS confirmer resolving
// accountStatusTargetDID to pdsURL, with the private-host hatch open for the
// loopback PDS.
func newTargetRepoStatusConfirmer(pdsURL string) *PDSRepoStatusConfirmer {
	resolver := &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		accountStatusTargetDID: {DID: accountStatusTargetDID, Handle: "target.coves.test", PDSURL: pdsURL},
	}}
	return NewPDSRepoStatusConfirmer(resolver, PrivateRepoStatusConfirmerOptions(true)...)
}

const (
	targetRepoTakendownBody   = `{"did":"did:plc:accountstatustargetaaaaa","active":false,"status":"takendown"}`
	targetRepoActiveBody      = `{"did":"did:plc:accountstatustargetaaaaa","active":true,"rev":"3l4abcdefghij"}`
	targetRepoUnavailableBody = `{"error":"ServiceUnavailable"}`
)

// TestUserConsumer_AccountStatusConfirmationFailureRechecked delivers an
// #account event through the users consumer over the Postgres store while the
// account's PDS answers 503: the event is handled (no error), the stored
// restriction stays as it was, and a recheck is scheduled. Once the PDS
// recovers, one due recheck pass, with no further account event, applies the
// PDS's answer and removes the recheck: a takedown is applied, a restore is
// lifted.
func TestUserConsumer_AccountStatusConfirmationFailureRechecked(t *testing.T) {
	takendown := accountstatus.Takendown
	tests := []struct {
		name          string
		existingRow   *accountstatus.Status
		eventActive   bool
		eventStatus   string
		recoveredBody string
		wantRow       *accountstatus.Status
	}{
		{
			name:          "takedown applied once the PDS recovers",
			eventStatus:   "takendown",
			recoveredBody: targetRepoTakendownBody,
			wantRow:       &takendown,
		},
		{
			name:          "restore lifted once the PDS recovers",
			existingRow:   &takendown,
			eventActive:   true,
			recoveredBody: targetRepoActiveBody,
			wantRow:       nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			giveTargetPresence(t, db)
			if tt.existingRow != nil {
				accountstatusfixture.Set(t, db, accountStatusTargetDID, *tt.existingRow)
			}
			pds := newSwitchableRepoStatusPDS(t, http.StatusServiceUnavailable, targetRepoUnavailableBody)
			consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
				WithUserAccountStatus(store, newTargetRepoStatusConfirmer(pds.server.URL)))

			before := time.Now()
			require.NoError(t, consumer.HandleEvent(t.Context(), accountStatusEvent(tt.eventActive, tt.eventStatus)),
				"a failed PDS confirmation is recorded for recheck, not returned as an error")
			after := time.Now()

			requireStoredAccountStatus(t, store, accountStatusTargetDID, tt.existingRow)
			recheck := accountstatusfixture.Recheck(t, db, accountStatusTargetDID)
			require.NotNil(t, recheck, "a failed PDS confirmation must schedule a recheck")
			assert.Equal(t, 1, recheck.Attempts)
			assert.Equal(t, accountstatus.FailurePDSErrorStatus, recheck.LastErrorCode)
			assert.False(t, recheck.NextAttemptAt.Before(before.Add(time.Minute-time.Millisecond)),
				"next attempt %s is before one minute after the event (%s)", recheck.NextAttemptAt, before)
			assert.False(t, recheck.NextAttemptAt.After(after.Add(time.Minute+time.Millisecond)),
				"next attempt %s is after one minute after the event (%s)", recheck.NextAttemptAt, after)

			pds.set(http.StatusOK, tt.recoveredBody)
			recheckTime := after.Add(2 * time.Minute)
			worker := accountstatus.NewReconciler(store, newTargetRepoStatusConfirmer(pds.server.URL),
				func() time.Time { return recheckTime })
			report, err := worker.RecheckDue(t.Context(), 50)
			require.NoError(t, err)
			assert.Equal(t, accountstatus.RecheckReport{Due: 1, Confirmed: 1, Failed: 0}, report)

			requireStoredAccountStatus(t, store, accountStatusTargetDID, tt.wantRow)
			assert.Nil(t, accountstatusfixture.Recheck(t, db, accountStatusTargetDID),
				"a confirmed recheck must remove the recheck row")
		})
	}
}

// blockingRepoStatusConfirmer answers like a PDS whose response is held in
// flight: it reports entering, then returns answer only once released (or its
// context ends).
type blockingRepoStatusConfirmer struct {
	answer   accountstatus.RepoStatus
	entered  chan struct{}
	released chan struct{}
}

func (c *blockingRepoStatusConfirmer) RepoStatus(ctx context.Context, _ string) (accountstatus.RepoStatus, error) {
	close(c.entered)
	select {
	case <-c.released:
		return c.answer, nil
	case <-ctx.Done():
		return accountstatus.RepoStatus{}, ctx.Err()
	}
}

// TestUserConsumer_AccountStatusHandlingSerializedPerAccount holds a takedown
// event's handling inside its PDS confirmation (the PDS has answered
// takendown) while an active event for the same account starts. The active
// handling must wait for the takedown to commit, then see the stored takedown,
// confirm the account active, and clear it: no row remains. Were the two
// interleaved, the active handling would find no row and stop, leaving the
// takedown stored for an active account.
func TestUserConsumer_AccountStatusHandlingSerializedPerAccount(t *testing.T) {
	db := testkit.DB(t)
	store := postgres.NewAccountStatusRepository(db)
	giveTargetPresence(t, db)

	takedownConfirmer := &blockingRepoStatusConfirmer{
		answer:   pdsAnswersTakendown,
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseTakedown := func() { releaseOnce.Do(func() { close(takedownConfirmer.released) }) }
	t.Cleanup(releaseTakedown)
	takedownConsumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
		WithUserAccountStatus(store, takedownConfirmer))
	activeConsumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
		WithUserAccountStatus(store, &fakeRepoStatusConfirmer{answer: pdsAnswersActive}))

	handlingContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- takedownConsumer.HandleEvent(handlingContext, accountStatusEvent(false, "takendown"))
	}()
	select {
	case <-takedownConfirmer.entered:
	case err := <-takedownDone:
		t.Fatalf("the takedown handling finished (%v) without asking the PDS", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the takedown handling never asked the PDS for the repo status")
	}

	activeEvent := accountStatusEvent(true, "")
	activeEvent.TimeUS += 1_000
	activeDone := make(chan error, 1)
	go func() {
		activeDone <- activeConsumer.HandleEvent(handlingContext, activeEvent)
	}()

	waitForLockWaitOrDone(t, db, activeDone, true,
		"the active handling to wait for the in-flight takedown handling of the same account")
	if len(activeDone) > 0 {
		err := <-activeDone
		t.Fatalf("the active handling finished (%v) while the takedown handling of the same account was still "+
			"in flight; it must wait for that handling to commit", err)
	}

	releaseTakedown()
	select {
	case err := <-takedownDone:
		require.NoError(t, err, "takedown handling")
	case <-handlingContext.Done():
		t.Fatal("the takedown handling did not finish after its PDS answer was released")
	}
	select {
	case err := <-activeDone:
		require.NoError(t, err, "active handling")
	case <-handlingContext.Done():
		t.Fatal("the active handling did not finish after the takedown handling committed")
	}

	requireStoredAccountStatus(t, store, accountStatusTargetDID, nil)
}

// TestAccountStatusRecheck_NewerEventWaitsForInFlightRecheck holds a due
// recheck's PDS confirmation in flight (the PDS's stale answer is takendown)
// over a stored takedown while a newer active event for the same account
// starts. The event must wait on the account's lock until the recheck
// commits, then see the stored takedown, confirm the account active, and clear
// both the restriction and the recheck row. Were the two interleaved, the
// event would clear the takedown first and the recheck would then store its
// stale takedown for an active account.
func TestAccountStatusRecheck_NewerEventWaitsForInFlightRecheck(t *testing.T) {
	db := testkit.DB(t)
	store := postgres.NewAccountStatusRepository(db)
	accountstatusfixture.Set(t, db, accountStatusTargetDID, accountstatus.Takendown)
	accountstatusfixture.SetRecheck(t, db, accountStatusTargetDID, accountstatus.PendingRecheck{
		Attempts:      2,
		NextAttemptAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		LastErrorCode: accountstatus.FailurePDSUnreachable,
	})

	workerConfirmer := &blockingRepoStatusConfirmer{
		answer:   pdsAnswersTakendown,
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(workerConfirmer.released) }) }
	t.Cleanup(releaseWorker)
	recheckTime := time.Date(2026, 11, 5, 12, 5, 0, 0, time.UTC)
	worker := accountstatus.NewReconciler(store, workerConfirmer, func() time.Time { return recheckTime })
	eventConsumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
		WithUserAccountStatus(store, &fakeRepoStatusConfirmer{answer: pdsAnswersActive}))

	handlingContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	workerDone := make(chan error, 1)
	go func() {
		_, err := worker.RecheckDue(handlingContext, 50)
		workerDone <- err
	}()
	select {
	case <-workerConfirmer.entered:
	case err := <-workerDone:
		t.Fatalf("the recheck pass finished (%v) without asking the PDS", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the recheck pass never asked the PDS for the repo status")
	}

	eventDone := make(chan error, 1)
	go func() {
		eventDone <- eventConsumer.HandleEvent(handlingContext, accountStatusEvent(true, ""))
	}()

	waitForLockWaitOrDone(t, db, eventDone, true,
		"the active event to wait for the in-flight recheck of the same account")
	if len(eventDone) > 0 {
		err := <-eventDone
		t.Fatalf("the active event finished (%v) while the recheck of the same account was still in flight; "+
			"it must wait for that recheck to commit", err)
	}

	releaseWorker()
	select {
	case err := <-workerDone:
		require.NoError(t, err, "recheck pass")
	case <-handlingContext.Done():
		t.Fatal("the recheck pass did not finish after its PDS answer was released")
	}
	select {
	case err := <-eventDone:
		require.NoError(t, err, "active event handling")
	case <-handlingContext.Done():
		t.Fatal("the active event handling did not finish after the recheck committed")
	}

	requireStoredAccountStatus(t, store, accountStatusTargetDID, nil)
	assert.Nil(t, accountstatusfixture.Recheck(t, db, accountStatusTargetDID),
		"the newer confirmed event must leave no recheck row")
}

// TestUserConsumer_AccountStatusPresenceGating delivers #account events
// through the users consumer over the Postgres store, with the account's PDS
// answering getRepoStatus. A takendown event for a DID with no Coves presence
// is still confirmed with the PDS; when the confirmation fails it stores
// nothing and leaves no recheck row, while a present DID's failed
// confirmation is rechecked (guard). An activation for a DID with no presence
// and nothing stored asks no PDS (guard). Replaces
// TestUserConsumer_AccountStatusIgnoresDIDWithoutPresence, which pinned
// ignoring a no-presence takedown without asking the PDS.
func TestUserConsumer_AccountStatusPresenceGating(t *testing.T) {
	tests := []struct {
		name         string
		present      bool
		eventActive  bool
		eventStatus  string
		pdsCode      int
		pdsBody      string
		wantRequests int
		wantRecheck  bool
	}{
		{
			name:        "no presence, takedown, PDS fails: asked, no recheck row",
			eventStatus: "takendown", pdsCode: http.StatusServiceUnavailable, pdsBody: targetRepoUnavailableBody,
			wantRequests: 1, wantRecheck: false,
		},
		{
			// Guard: unchanged behavior.
			name:    "present, takedown, PDS fails: asked, recheck row",
			present: true, eventStatus: "takendown", pdsCode: http.StatusServiceUnavailable, pdsBody: targetRepoUnavailableBody,
			wantRequests: 1, wantRecheck: true,
		},
		{
			// Guard: unchanged behavior.
			name:        "no presence, activation: not asked",
			eventActive: true, pdsCode: http.StatusOK, pdsBody: targetRepoTakendownBody,
			wantRequests: 0, wantRecheck: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			if tt.present {
				giveTargetPresence(t, db)
			}
			pds := newSwitchableRepoStatusPDS(t, tt.pdsCode, tt.pdsBody)
			consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
				WithUserAccountStatus(store, newTargetRepoStatusConfirmer(pds.server.URL)))

			require.NoError(t, consumer.HandleEvent(t.Context(), accountStatusEvent(tt.eventActive, tt.eventStatus)))
			assert.Equal(t, tt.wantRequests, pds.requestCount(), "getRepoStatus requests to the account's PDS")
			requireStoredAccountStatus(t, store, accountStatusTargetDID, nil)
			recheck := accountstatusfixture.Recheck(t, db, accountStatusTargetDID)
			if tt.wantRecheck {
				require.NotNil(t, recheck, "a present DID's failed confirmation must schedule a recheck")
				assert.Equal(t, 1, recheck.Attempts)
				assert.Equal(t, accountstatus.FailurePDSErrorStatus, recheck.LastErrorCode)
			} else {
				assert.Nil(t, recheck, "expected no recheck row")
			}
		})
	}
}

// TestUserConsumer_AccountStatusTakedownBeforePresenceHidesLaterPost is the
// acceptance case: a takendown #account event for a DID Coves holds nothing
// for is confirmed with its PDS and stored, so a post the account wrote and
// Coves indexes afterwards is hidden from other viewers and the anonymous
// public while its author still sees it.
func TestUserConsumer_AccountStatusTakedownBeforePresenceHidesLaterPost(t *testing.T) {
	s := newSelfIndexScene(t)
	store := postgres.NewAccountStatusRepository(s.db)
	pds := newSwitchableRepoStatusPDS(t, http.StatusOK, targetRepoTakendownBody)
	consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
		WithUserAccountStatus(store, newTargetRepoStatusConfirmer(pds.server.URL)))

	require.NoError(t, consumer.HandleEvent(t.Context(), accountStatusEvent(false, "takendown")))
	assert.Equal(t, 1, pds.requestCount(), "the no-presence takedown is confirmed with the account's PDS")
	takendown := accountstatus.Takendown
	requireStoredAccountStatus(t, store, accountStatusTargetDID, &takendown)
	assert.Nil(t, accountstatusfixture.Recheck(t, s.db, accountStatusTargetDID), "a confirmed takedown leaves no recheck row")

	viewerName := testkit.UniqueIDWithPrefix(t, "asviewer")
	viewer := fixtures.DID(viewerName)
	fixtures.User(t, s.db, viewerName+".test", viewer)
	s.users.users[accountStatusTargetDID] = &users.User{DID: accountStatusTargetDID, Handle: "target.coves.test"}
	const cid = "bafyreiaccountstatuslatepost"
	base := time.Now().UnixMicro()
	uri := s.indexPost(t, accountStatusTargetDID, "statuslatepost", cid, base)
	s.admit(t, uri, cid, base+1_000_000)

	served := postgres.NewPostRepository(s.db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	own, err := served.VisibleHeaderView(t.Context(), uri, accountStatusTargetDID)
	require.NoError(t, err)
	require.NotNil(t, own, "control: the late post is indexed and its author still sees it")
	for viewerLabel, viewerDID := range map[string]string{"other viewer": viewer, "anonymous": ""} {
		view, err := served.VisibleHeaderView(t.Context(), uri, viewerDID)
		require.NoError(t, err)
		assert.Nil(t, view, "%s: the restricted account's late post must be hidden", viewerLabel)
	}
}
