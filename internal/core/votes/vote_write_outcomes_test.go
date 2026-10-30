package votes_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/stretchr/testify/require"
)

// What each write outcome does to the next write for the same viewer and
// subject (PRD §5.3, §5.4): the per-key write lock (D4), ambiguous plain
// writes (D5), definitive refusals, and the stale-key replacement.
//
// The lock tests run in a synctest bubble. The service must wait for the lock
// in a select on the request context and a channel (a canceled wait returns
// the context error), which synctest counts as durably blocked, so
// synctest.Wait returns once a second write is parked on the lock. A write
// that is not serialized reaches the fake PDS before synctest.Wait returns.

// committedState renders the fake repo's committed writes and its final
// records. Each record the service minted is named new#1, new#2, ... in the
// order its create was committed; a record the test seeded keeps its rkey,
// and so does a create at a record key an earlier write already touched.
// Records render as "<direction> <subject URI>".
func committedState(fake *fakePDS) (operations []string, records map[string]string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	labels := map[string]string{}
	touched := map[string]bool{}
	label := func(rkey string) string {
		if named, exists := labels[rkey]; exists {
			return named
		}
		return rkey
	}
	for _, operation := range fake.ops {
		if operation.kind == "create" && !touched[operation.rkey] {
			labels[operation.rkey] = fmt.Sprintf("new#%d", len(labels)+1)
		}
		touched[operation.rkey] = true
		operations = append(operations, operation.kind+"("+label(operation.rkey)+")")
	}
	records = map[string]string{}
	for rkey, record := range fake.records {
		records[label(rkey)] = record.Direction + " " + record.Subject.URI
	}
	return operations, records
}

// requireCommittedState compares the fake repo's write log and final records.
func requireCommittedState(t *testing.T, fake *fakePDS, wantOperations []string, wantRecords map[string]string) {
	t.Helper()
	operations, records := committedState(fake)
	require.Equal(t, wantOperations, operations, "the writes committed to the viewer's repo")
	require.Equal(t, wantRecords, records, "the viewer's vote records afterwards")
}

// heldWrite parks each of the first few CreateRecord or DeleteRecord calls
// that reach the fake PDS until that write is released, and lets every later
// one through. It records the operation of every write that reaches the PDS,
// held or not.
type heldWrite struct {
	held        []chan struct{}
	release     []chan struct{}
	releaseOnce []sync.Once
	mutex       sync.Mutex
	arrivals    []string
}

// holdFirstWrite holds only the first write. Call it inside the synctest bubble.
func holdFirstWrite(fake *fakePDS) *heldWrite {
	return holdWrites(fake, 1)
}

// holdWrites holds each of the first count writes until releaseWrite is
// called with its arrival index. Call it inside the synctest bubble.
func holdWrites(fake *fakePDS, count int) *heldWrite {
	hold := &heldWrite{
		held:        make([]chan struct{}, count),
		release:     make([]chan struct{}, count),
		releaseOnce: make([]sync.Once, count),
	}
	for index := range count {
		hold.held[index] = make(chan struct{})
		hold.release[index] = make(chan struct{})
	}
	fake.writeHook = func(_ context.Context, operation, _ string) error {
		hold.mutex.Lock()
		hold.arrivals = append(hold.arrivals, operation)
		index := len(hold.arrivals) - 1
		hold.mutex.Unlock()
		if index < count {
			close(hold.held[index])
			<-hold.release[index]
		}
		return nil
	}
	return hold
}

// releaseWrite lets the held write with that arrival index commit. It is
// safe to call more than once.
func (hold *heldWrite) releaseWrite(index int) {
	hold.releaseOnce[index].Do(func() { close(hold.release[index]) })
}

// releaseFirst lets the first held write commit.
func (hold *heldWrite) releaseFirst() {
	hold.releaseWrite(0)
}

// releaseAll lets every held write commit.
func (hold *heldWrite) releaseAll() {
	for index := range hold.release {
		hold.releaseWrite(index)
	}
}

// arrivalsSoFar returns the operation of every write that has reached the PDS.
func (hold *heldWrite) arrivalsSoFar() []string {
	hold.mutex.Lock()
	defer hold.mutex.Unlock()
	return append([]string(nil), hold.arrivals...)
}

// voteResult is a CreateVote outcome delivered from another goroutine.
type voteResult struct {
	response *votes.CreateVoteResponse
	err      error
}

// startCreateVote runs CreateVote for testVoterDID in a new goroutine.
func startCreateVote(t *testing.T, ctx context.Context, service votes.Service, subject votes.StrongRef, direction string) <-chan voteResult {
	t.Helper()
	session := voter(t, testVoterDID)
	results := make(chan voteResult, 1)
	go func() {
		response, err := service.CreateVote(ctx, session, votes.CreateVoteRequest{Subject: subject, Direction: direction})
		results <- voteResult{response: response, err: err}
	}()
	return results
}

// awaitSignal waits for a channel, bounded by a timer so a write stuck on the
// lock fails the test instead of hanging it. In a synctest bubble the timer
// is virtual and fires only once every goroutine is blocked.
func awaitSignal[Value any](t *testing.T, signal <-chan Value, what string) Value {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(time.Minute):
		require.FailNow(t, "timed out waiting: "+what)
		var zero Value
		return zero
	}
}

func TestCreateVote_SerializesWritesForOneViewerAndSubject(t *testing.T) {
	t.Parallel()

	t.Run("a second tap on the same key waits for the first, then toggles it off", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			hold := holdFirstWrite(fake)
			defer hold.releaseFirst()

			first := startCreateVote(t, context.Background(), service, subject(), "up")
			awaitSignal(t, hold.held[0], "the first tap reaches the PDS")
			second := startCreateVote(t, context.Background(), service, subject(), "up")
			synctest.Wait()

			require.Equal(t, []string{"create"}, hold.arrivalsSoFar(),
				"the second tap reached the PDS while the first tap's write was still in flight")
			select {
			case <-second:
				require.FailNow(t, "the second tap finished while the first tap's write was still in flight")
			default:
			}

			hold.releaseFirst()
			firstResult := awaitSignal(t, first, "the first tap finishes")
			secondResult := awaitSignal(t, second, "the second tap finishes")
			require.NoError(t, firstResult.err)
			require.NoError(t, secondResult.err)
			require.Equal(t, "at://"+testVoterDID+"/social.coves.feed.vote/"+fake.creates()[0], firstResult.response.URI)
			require.Empty(t, secondResult.response.URI,
				"the second tap's lookup sees the first tap's vote and toggles it off")
			requireCommittedState(t, fake, []string{"create(new#1)", "delete(new#1)"}, map[string]string{})
		})
	})

	t.Run("three taps on the same key reach the PDS one at a time", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			hold := holdWrites(fake, 2)
			defer hold.releaseAll()

			first := startCreateVote(t, context.Background(), service, subject(), "up")
			awaitSignal(t, hold.held[0], "the first tap reaches the PDS")
			second := startCreateVote(t, context.Background(), service, subject(), "up")
			synctest.Wait()
			require.Equal(t, []string{"create"}, hold.arrivalsSoFar(),
				"the second tap reached the PDS while the first tap's write was still in flight")

			hold.releaseWrite(0)
			firstResult := awaitSignal(t, first, "the first tap finishes")
			require.NoError(t, firstResult.err)
			awaitSignal(t, hold.held[1], "the second tap reaches the PDS")
			<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.

			// The second tap holds the lock the first one released; the third
			// arrives while the second's write is in flight.
			third := startCreateVote(t, context.Background(), service, subject(), "up")
			synctest.Wait()
			require.Equal(t, []string{"create", "delete"}, hold.arrivalsSoFar(),
				"the third tap reached the PDS while the second tap's write was still in flight")
			select {
			case <-third:
				require.FailNow(t, "the third tap finished while the second tap's write was still in flight")
			default:
			}

			hold.releaseWrite(1)
			secondResult := awaitSignal(t, second, "the second tap finishes")
			thirdResult := awaitSignal(t, third, "the third tap finishes")
			require.NoError(t, secondResult.err)
			require.NoError(t, thirdResult.err)
			require.Empty(t, secondResult.response.URI, "the second tap toggles the first tap's vote off")
			require.NotEmpty(t, thirdResult.response.URI, "the third tap sees the toggle-off and casts again")
			requireCommittedState(t, fake,
				[]string{"create(new#1)", "delete(new#1)", "create(new#2)"},
				map[string]string{"new#2": "up " + testSubject})
		})
	})

	t.Run("a tap on another subject does not wait", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			hold := holdFirstWrite(fake)
			defer hold.releaseFirst()

			first := startCreateVote(t, context.Background(), service, subject(), "up")
			awaitSignal(t, hold.held[0], "the first tap reaches the PDS")
			<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.

			other := startCreateVote(t, context.Background(), service, controlSubject(), "up")
			otherResult := awaitSignal(t, other, "the tap on another subject finishes while the first is held")
			require.NoError(t, otherResult.err)
			require.NotEmpty(t, otherResult.response.URI)

			hold.releaseFirst()
			firstResult := awaitSignal(t, first, "the first tap finishes")
			require.NoError(t, firstResult.err)
			require.NotEmpty(t, firstResult.response.URI)
			requireCommittedState(t, fake,
				[]string{"create(new#1)", "create(new#2)"},
				map[string]string{
					"new#1": "up " + controlSubjectURI,
					"new#2": "up " + testSubject,
				})
		})
	})

	t.Run("a canceled wait returns the context error and makes no PDS call", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			hold := holdFirstWrite(fake)
			defer hold.releaseFirst()

			first := startCreateVote(t, context.Background(), service, subject(), "up")
			awaitSignal(t, hold.held[0], "the first tap reaches the PDS")
			listsBefore := fake.listCalls
			waitingContext, cancel := context.WithCancel(context.Background())
			defer cancel()
			second := startCreateVote(t, waitingContext, service, subject(), "up")
			synctest.Wait()
			cancel()

			secondResult := awaitSignal(t, second, "the canceled tap returns")
			require.ErrorIs(t, secondResult.err, context.Canceled)
			require.Equal(t, []string{"create"}, hold.arrivalsSoFar(), "the canceled tap made no PDS write")
			require.Equal(t, listsBefore, fake.listCalls, "the canceled tap did not read the PDS")
			require.Zero(t, fake.batchCalls)

			hold.releaseFirst()
			firstResult := awaitSignal(t, first, "the first tap finishes")
			require.NoError(t, firstResult.err)
			requireCommittedState(t, fake, []string{"create(new#1)"}, map[string]string{"new#1": "up " + testSubject})
		})
	})
}

// voteWrite is a write request. It answers with the CreateVote response; a
// toggle-off or a deletion answers with an empty URI.
type voteWrite func(t *testing.T, service votes.Service) (*votes.CreateVoteResponse, error)

func createUpVote(t *testing.T, service votes.Service) (*votes.CreateVoteResponse, error) {
	return service.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
}

func createDownVote(t *testing.T, service votes.Service) (*votes.CreateVoteResponse, error) {
	return service.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
}

func deleteTheVote(t *testing.T, service votes.Service) (*votes.CreateVoteResponse, error) {
	if err := service.DeleteVote(context.Background(), voter(t, testVoterDID),
		votes.DeleteVoteRequest{Subject: subject()}); err != nil {
		return nil, err
	}
	return &votes.CreateVoteResponse{}, nil
}

// voteRecordURI is the AT-URI of testVoterDID's vote record with this key.
func voteRecordURI(rkey string) string {
	return "at://" + testVoterDID + "/social.coves.feed.vote/" + rkey
}

// onlyRecordKey returns the key of the one vote record the fake repo holds.
func onlyRecordKey(t *testing.T, fake *fakePDS) string {
	t.Helper()
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	require.Len(t, fake.records, 1, "the viewer's repo holds one vote")
	for rkey := range fake.records {
		return rkey
	}
	return ""
}

// A retry after an ambiguous write reads the PDS (PRD §5.4). A retry of the
// same request repeats it (D6): when the ambiguous write's intended result
// already holds, it answers with that result and writes nothing. Any other
// request follows the normal toggle logic on what the PDS holds.
func TestVoteWrites_RetryAfterAnAmbiguousWrite(t *testing.T) {
	t.Parallel()
	indexedDisplay := map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: indexedUpVoteURI}}

	for _, testCase := range []struct {
		name string
		// indexed: the index and the PDS hold the viewer's up vote 3kindexedup.
		indexed bool
		// write is made once failing.
		write voteWrite
		// failingOperation is "create", "delete", or "batch" for a direction
		// switch whose reconciliation read is refused, so its outcome stays
		// unknown.
		failingOperation string
		failure          error
		committed        bool
		// wantDisplay is what display shows between the write and the retry.
		wantDisplay map[string]votes.ViewerVote
		retry       voteWrite
		// wantRetryWritesNothing: the retry makes no PDS write at all.
		wantRetryWritesNothing bool
		// wantRetryCast: the retry answers with the one vote the repo holds
		// afterwards; otherwise it answers toggled off or deleted.
		wantRetryCast  bool
		wantOperations []string
		wantRecords    map[string]string
		// wantDirectionAfterRetry is the direction display shows after the
		// retry, for the one vote the repo holds; empty for no vote.
		wantDirectionAfterRetry string
	}{
		{
			name:                    "create committed, then 5xx: the retry answers with the committed vote",
			write:                   createUpVote,
			failingOperation:        "create",
			failure:                 serverError("createRecord"),
			committed:               true,
			wantDisplay:             nil,
			retry:                   createUpVote,
			wantRetryWritesNothing:  true,
			wantRetryCast:           true,
			wantOperations:          []string{"create(new#1)"},
			wantRecords:             map[string]string{"new#1": "up " + testSubject},
			wantDirectionAfterRetry: "up",
		},
		{
			name:                    "create not committed, then timeout: the retry creates one record",
			write:                   createUpVote,
			failingOperation:        "create",
			failure:                 context.DeadlineExceeded,
			committed:               false,
			wantDisplay:             nil,
			retry:                   createUpVote,
			wantRetryCast:           true,
			wantOperations:          []string{"create(new#1)"},
			wantRecords:             map[string]string{"new#1": "up " + testSubject},
			wantDirectionAfterRetry: "up",
		},
		{
			name:                    "create committed, then 5xx, retry in the other direction: the retry switches",
			write:                   createUpVote,
			failingOperation:        "create",
			failure:                 serverError("createRecord"),
			committed:               true,
			wantDisplay:             nil,
			retry:                   createDownVote,
			wantRetryCast:           true,
			wantOperations:          []string{"create(new#1)", "delete(new#1)", "create(new#2)"},
			wantRecords:             map[string]string{"new#2": "down " + testSubject},
			wantDirectionAfterRetry: "down",
		},
		{
			name:                    "toggle-off delete committed, then 5xx: the retry answers toggled off",
			indexed:                 true,
			write:                   createUpVote,
			failingOperation:        "delete",
			failure:                 serverError("deleteRecord"),
			committed:               true,
			wantDisplay:             indexedDisplay,
			retry:                   createUpVote,
			wantRetryWritesNothing:  true,
			wantRetryCast:           false,
			wantOperations:          []string{"delete(3kindexedup)"},
			wantRecords:             map[string]string{},
			wantDirectionAfterRetry: "",
		},
		{
			name:                    "toggle-off delete committed, then 5xx, retry in the other direction: the retry creates at a new key",
			indexed:                 true,
			write:                   createUpVote,
			failingOperation:        "delete",
			failure:                 serverError("deleteRecord"),
			committed:               true,
			wantDisplay:             indexedDisplay,
			retry:                   createDownVote,
			wantRetryCast:           true,
			wantOperations:          []string{"delete(3kindexedup)", "create(new#1)"},
			wantRecords:             map[string]string{"new#1": "down " + testSubject},
			wantDirectionAfterRetry: "down",
		},
		{
			name:                    "DeleteVote delete committed, then timeout: the retry succeeds",
			indexed:                 true,
			write:                   deleteTheVote,
			failingOperation:        "delete",
			failure:                 context.DeadlineExceeded,
			committed:               true,
			wantDisplay:             indexedDisplay,
			retry:                   deleteTheVote,
			wantRetryWritesNothing:  true,
			wantRetryCast:           false,
			wantOperations:          []string{"delete(3kindexedup)"},
			wantRecords:             map[string]string{},
			wantDirectionAfterRetry: "",
		},
		{
			name:                    "DeleteVote delete committed, then timeout, retry is CreateVote: the retry creates",
			indexed:                 true,
			write:                   deleteTheVote,
			failingOperation:        "delete",
			failure:                 context.DeadlineExceeded,
			committed:               true,
			wantDisplay:             indexedDisplay,
			retry:                   createUpVote,
			wantRetryCast:           true,
			wantOperations:          []string{"delete(3kindexedup)", "create(new#1)"},
			wantRecords:             map[string]string{"new#1": "up " + testSubject},
			wantDirectionAfterRetry: "up",
		},
		{
			name:                    "direction switch committed, reconciliation unresolved: the retry answers with the switched vote",
			indexed:                 true,
			write:                   createDownVote,
			failingOperation:        "batch",
			failure:                 context.DeadlineExceeded,
			committed:               true,
			wantDisplay:             indexedDisplay,
			retry:                   createDownVote,
			wantRetryWritesNothing:  true,
			wantRetryCast:           true,
			wantOperations:          []string{"delete(3kindexedup)", "create(new#1)"},
			wantRecords:             map[string]string{"new#1": "down " + testSubject},
			wantDirectionAfterRetry: "down",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			if testCase.indexed {
				indexVoteWithRecord(t, fake, repository, indexedUpVoteURI, "3kindexedup", "up")
			}
			service := newService(t, fake, repository)

			switch testCase.failingOperation {
			case "create":
				fake.createErr = testCase.failure
			case "delete":
				fake.deleteErr = testCase.failure
			case "batch":
				fake.batchErr = testCase.failure
				fake.getErr = pds.ErrForbidden
			}
			fake.commitBeforeError = testCase.committed
			_, err := testCase.write(t, service)
			require.ErrorIs(t, err, testCase.failure, "test setup: the write's outcome is unknown")
			if testCase.failingOperation == "batch" {
				require.Equal(t, 1, fake.getCalls, "test setup: the switch's reconciliation read was refused")
			}
			fake.createErr = nil
			fake.deleteErr = nil
			fake.batchErr = nil
			fake.getErr = nil
			fake.commitBeforeError = false

			listsBefore := fake.listCalls
			got, err := service.ViewerVotes(context.Background(), testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, testCase.wantDisplay, got, "display shows the index value for an Unverified key")
			require.Equal(t, listsBefore, fake.listCalls, "display never scans the PDS")

			listsBefore = fake.listCalls
			attemptsBefore := len(fake.attemptedCreates())
			operationsBefore := len(fake.ops)
			batchesBefore := fake.batchCalls
			response, err := testCase.retry(t, service)
			require.Greater(t, fake.listCalls, listsBefore, "the retry looks up the existing vote on the PDS")
			require.NoError(t, err)
			if testCase.wantRetryWritesNothing {
				require.Len(t, fake.ops, operationsBefore, "the retry commits no PDS write")
				require.Len(t, fake.attemptedCreates(), attemptsBefore, "the retry attempts no create")
				require.Equal(t, batchesBefore, fake.batchCalls, "the retry attempts no replacement")
			}
			if testCase.wantRetryCast {
				require.Equal(t, voteRecordURI(onlyRecordKey(t, fake)), response.URI,
					"the retry answers with the vote the viewer's repo holds")
				require.NotEmpty(t, response.CID)
			} else {
				require.Empty(t, response.URI, "the retry answers toggled off or deleted")
			}
			requireCommittedState(t, fake, testCase.wantOperations, testCase.wantRecords)

			var wantAfterRetry map[string]votes.ViewerVote
			if testCase.wantDirectionAfterRetry != "" {
				wantAfterRetry = map[string]votes.ViewerVote{testSubject: {
					Direction: testCase.wantDirectionAfterRetry,
					URI:       voteRecordURI(onlyRecordKey(t, fake)),
				}}
			}
			got, err = service.ViewerVotes(context.Background(), testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, wantAfterRetry, got,
				"after the retry, display shows its outcome, whatever the index still holds")
		})
	}
}

func TestVoteWrites_UnverifiedSurvivesAScanThatChangesNothing(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		// leaveUnverified makes the key's last write ambiguous.
		leaveUnverified func(t *testing.T, service votes.Service, fake *fakePDS)
		// scanThenNoChange is a write that scans the PDS and changes nothing.
		scanThenNoChange func(t *testing.T, service votes.Service, fake *fakePDS)
		wantOperations   []string
		wantRecords      map[string]string
	}{
		{
			// Not a retry of the ambiguous write (D6): the DeleteVote follows
			// an ambiguous CreateVote.
			name: "a DeleteVote scan finds no vote",
			leaveUnverified: func(t *testing.T, service votes.Service, fake *fakePDS) {
				leaveUnverified(t, service, fake, subject())
			},
			scanThenNoChange: func(t *testing.T, service votes.Service, fake *fakePDS) {
				listsBefore := fake.listCalls
				_, err := deleteTheVote(t, service)
				require.ErrorIs(t, err, votes.ErrVoteNotFound)
				require.Greater(t, fake.listCalls, listsBefore, "test setup: the DeleteVote scans the PDS")
			},
			wantOperations: []string{"create(new#1)"},
			wantRecords:    map[string]string{"new#1": "up " + testSubject},
		},
		{
			name: "a create after the scan is refused",
			leaveUnverified: func(t *testing.T, service votes.Service, fake *fakePDS) {
				leaveUnverified(t, service, fake, subject())
			},
			scanThenNoChange: func(t *testing.T, service votes.Service, fake *fakePDS) {
				fake.createErr = fmt.Errorf("createRecord: %w: InvalidRecord", pds.ErrBadRequest)
				listsBefore := fake.listCalls
				_, err := createUpVote(t, service)
				require.ErrorIs(t, err, pds.ErrBadRequest)
				require.Greater(t, fake.listCalls, listsBefore, "test setup: the CreateVote scans the PDS")
				fake.createErr = nil
			},
			wantOperations: []string{"create(new#1)"},
			wantRecords:    map[string]string{"new#1": "up " + testSubject},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			testCase.leaveUnverified(t, service, fake)
			testCase.scanThenNoChange(t, service, fake)

			listsBefore := fake.listCalls
			response, err := createUpVote(t, service)
			require.NoError(t, err)
			require.Greater(t, fake.listCalls, listsBefore,
				"the key is still Unverified, so the next write scans the PDS again")
			require.NotEmpty(t, response.URI, "the PDS holds no vote, so the tap casts one")
			requireCommittedState(t, fake, testCase.wantOperations, testCase.wantRecords)
		})
	}
}

// sessionBusyError is how the PDS client reports that another operation held
// the viewer's OAuth session: the request was never sent.
func sessionBusyError() error {
	return fmt.Errorf("OAuth session held by another operation: %w", pds.ErrSessionBusy)
}

func TestCreateVote_DefinitiveRefusalLeavesTheOverlayUnchanged(t *testing.T) {
	t.Parallel()
	// priorWrite makes this AppView's earlier write and returns the fake PDS
	// knob the refused write fails on.
	type priorWrite func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) *error
	var toggledOffIndexedVote priorWrite = func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) *error {
		indexVoteWithRecord(t, fake, repository, indexedUpVoteURI, "3kindexedup", "up")
		withdrawVote(t, service, subject(), "up")
		return &fake.createErr
	}
	var castHere priorWrite = func(t *testing.T, service votes.Service, fake *fakePDS, _ *fakeVoteRepository) *error {
		castVote(t, service, subject(), "up")
		return &fake.deleteErr
	}

	for _, testCase := range []struct {
		name             string
		priorWrite       priorWrite
		refusal          error
		wantCast         bool
		wantOperations   []string
		wantFinalRecords map[string]string
	}{
		{
			name:             "refused create after toggling off an indexed vote: the next tap still creates",
			priorWrite:       toggledOffIndexedVote,
			refusal:          fmt.Errorf("createRecord: %w: InvalidRecord", pds.ErrBadRequest),
			wantCast:         true,
			wantOperations:   []string{"delete(3kindexedup)", "create(new#1)"},
			wantFinalRecords: map[string]string{"new#1": "up " + testSubject},
		},
		{
			name:             "create refused as session busy after toggling off an indexed vote: the next tap still creates",
			priorWrite:       toggledOffIndexedVote,
			refusal:          sessionBusyError(),
			wantCast:         true,
			wantOperations:   []string{"delete(3kindexedup)", "create(new#1)"},
			wantFinalRecords: map[string]string{"new#1": "up " + testSubject},
		},
		{
			name:             "refused toggle-off of a vote cast here: the next tap still deletes it",
			priorWrite:       castHere,
			refusal:          &atclient.APIError{StatusCode: 422, Name: "UnprocessableContent"},
			wantCast:         false,
			wantOperations:   []string{"create(new#1)", "delete(new#1)"},
			wantFinalRecords: map[string]string{},
		},
		{
			name:             "toggle-off refused as session busy: the next tap still deletes it",
			priorWrite:       castHere,
			refusal:          sessionBusyError(),
			wantCast:         false,
			wantOperations:   []string{"create(new#1)", "delete(new#1)"},
			wantFinalRecords: map[string]string{},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			service := newService(t, fake, repository)
			failingKnob := testCase.priorWrite(t, service, fake, repository)

			*failingKnob = testCase.refusal
			_, err := createUpVote(t, service)
			require.ErrorIs(t, err, testCase.refusal, "test setup: the PDS refuses the write")
			*failingKnob = nil

			listsBefore := fake.listCalls
			response, err := createUpVote(t, service)
			require.NoError(t, err)
			require.Equal(t, listsBefore, fake.listCalls,
				"a refused write cannot have committed, so the next write does not scan the PDS")
			require.Equal(t, testCase.wantCast, response.URI != "", "cast, or toggled off")
			requireCommittedState(t, fake, testCase.wantOperations, testCase.wantFinalRecords)
		})
	}
}

func TestCreateVote_StaleKeyOnDirectionSwitchFailsThenTheRetryScans(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakePDS(t, testVoterDID)
		repository := newFakeVoteRepository(t)
		// The index holds an up vote whose record is no longer on the PDS.
		repository.indexActive(t, indexedUpVoteURI, testSubject, "up")
		service := newService(t, fake, repository)

		// applyWrites answers a delete of a missing record with a 500 and
		// commits nothing.
		fake.batchErr = serverError("applyWrites")
		_, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.ErrorIs(t, err, pds.ErrServerError)
		require.Equal(t, 1, fake.batchCalls, "the switch replaces the indexed record in one batch")
		require.Positive(t, fake.getCalls, "an unresolved replacement runs the reconciliation poll")
		fake.batchErr = nil

		listsBefore := fake.listCalls
		response, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.NoError(t, err)
		require.Greater(t, fake.listCalls, listsBefore, "the retry looks up the existing vote on the PDS")
		require.NotEmpty(t, response.URI)
		require.Equal(t, 1, fake.batchCalls, "the PDS holds no vote, so the retry is a plain create")
		requireCommittedState(t, fake, []string{"create(new#1)"}, map[string]string{"new#1": "down " + testSubject})
	})
}

func TestDeleteVote_RemovedEntryIsNotFound(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	repository := newFakeVoteRepository(t)
	indexVoteWithRecord(t, fake, repository, indexedUpVoteURI, "3kindexedup", "up")
	service := newService(t, fake, repository)
	withdrawVote(t, service, subject(), "up")

	err := service.DeleteVote(context.Background(), voter(t, testVoterDID), votes.DeleteVoteRequest{Subject: subject()})
	require.ErrorIs(t, err, votes.ErrVoteNotFound,
		"the vote was removed here; the index still holding its row does not bring it back")
	requireCommittedState(t, fake, []string{"delete(3kindexedup)"}, map[string]string{})
}

func TestCreateVote_PDSScanAuthErrorIsNotAuthorized(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{pds.ErrUnauthorized, pds.ErrForbidden, pds.ErrSessionExpired} {
		t.Run(refusal.Error(), func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			leaveUnverified(t, service, fake, subject())

			fake.listErr = fmt.Errorf("listRecords: %w", refusal)
			_, err := createUpVote(t, service)
			require.ErrorIs(t, err, votes.ErrNotAuthorized)
			require.ErrorIs(t, err, refusal, "the auth failure keeps its cause")
			require.Equal(t, 1, fake.listCalls, "the error comes from the PDS scan")
			require.Empty(t, fake.ops, "a failed scan makes no PDS write")
			require.Zero(t, fake.batchCalls)
		})
	}
}

func TestVoteWrites_PDSScanFailureFailsTheWriteWithNoPDSWrite(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		write voteWrite
	}{
		{"CreateVote", createUpVote},
		{"DeleteVote", deleteTheVote},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			leaveUnverified(t, service, fake, subject())

			fake.listErr = serverError("listRecords")
			_, err := testCase.write(t, service)
			require.ErrorIs(t, err, pds.ErrServerError, "a failed PDS scan fails the write; it is never read as \"no vote\"")
			require.NotErrorIs(t, err, votes.ErrNotAuthorized)
			require.Equal(t, 1, fake.listCalls, "the error comes from the PDS scan")
			require.Empty(t, fake.ops, "a failed scan makes no PDS write")
			require.Len(t, fake.attemptedCreates(), 1, "only the ambiguous create reached the PDS")
			require.Zero(t, fake.batchCalls)
		})
	}
}

// recordsByKey renders the fake repo's records by record key as
// "<direction> <subject URI>".
func recordsByKey(fake *fakePDS) map[string]string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	records := map[string]string{}
	for rkey, record := range fake.records {
		records[rkey] = record.Direction + " " + record.Subject.URI
	}
	return records
}

func TestCreateVote_RetryAfterAmbiguousCreateReusesTheRecordKey(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		// lateCommit commits the failed first create right after the retry's
		// PDS scan returns, before the retry writes.
		lateCommit bool
		// deleteFirst makes a DeleteVote, which finds no vote, before the retry.
		deleteFirst bool
		// retryFailsAmbiguously makes the retry fail with a 5xx without
		// committing; a third CreateVote, in the same direction, follows it.
		retryFailsAmbiguously bool
		// lateCommitAfterRetry commits the failed first create after the
		// retry has succeeded.
		lateCommitAfterRetry bool
		retryDirection       string
		wantRetryFailed      bool
		// wantDirection is the direction of the one record the repo holds.
		wantDirection      string
		wantCreateAttempts int
	}{
		{
			name:               "the first create commits late, retry in the same direction",
			lateCommit:         true,
			retryDirection:     "up",
			wantRetryFailed:    true,
			wantDirection:      "up",
			wantCreateAttempts: 2,
		},
		{
			name:               "the first create commits late, retry in the other direction",
			lateCommit:         true,
			retryDirection:     "down",
			wantRetryFailed:    true,
			wantDirection:      "up",
			wantCreateAttempts: 2,
		},
		{
			name:               "the first create never commits",
			retryDirection:     "up",
			wantDirection:      "up",
			wantCreateAttempts: 2,
		},
		{
			name:               "a DeleteVote finds no vote before the retry",
			deleteFirst:        true,
			retryDirection:     "up",
			wantDirection:      "up",
			wantCreateAttempts: 2,
		},
		{
			name:                  "the retry also fails ambiguously, then a third create succeeds",
			retryFailsAmbiguously: true,
			retryDirection:        "up",
			wantDirection:         "up",
			wantCreateAttempts:    3,
		},
		{
			name:                 "the retry lands first, then the first create commits late",
			lateCommitAfterRetry: true,
			retryDirection:       "down",
			wantDirection:        "down",
			wantCreateAttempts:   2,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))

			fake.createErr = serverError("createRecord")
			_, err := createUpVote(t, service)
			require.ErrorIs(t, err, pds.ErrServerError, "test setup: the first create's outcome is unknown")
			fake.createErr = nil
			require.Empty(t, fake.ops, "test setup: the first create has not committed")
			attempts := fake.attemptedCreates()
			require.Len(t, attempts, 1, "test setup: one create reached the PDS")
			firstAttempt := attempts[0]

			if testCase.deleteFirst {
				listsBefore := fake.listCalls
				_, err := deleteTheVote(t, service)
				require.ErrorIs(t, err, votes.ErrVoteNotFound)
				require.Greater(t, fake.listCalls, listsBefore, "test setup: the DeleteVote scans the PDS")
			}

			committedLate := false
			if testCase.lateCommit {
				fake.afterList = func() {
					if !committedLate {
						committedLate = fake.commitLate(firstAttempt)
					}
				}
			}
			if testCase.retryFailsAmbiguously {
				fake.createErr = serverError("createRecord")
			}

			listsBefore := fake.listCalls
			_, retryErr := service.CreateVote(context.Background(), voter(t, testVoterDID),
				votes.CreateVoteRequest{Subject: subject(), Direction: testCase.retryDirection})
			require.Greater(t, fake.listCalls, listsBefore, "test setup: the retry scans the PDS")
			require.Equal(t, testCase.lateCommit, committedLate, "test setup: the late commit lands after the retry's scan")

			if testCase.retryFailsAmbiguously {
				require.ErrorIs(t, retryErr, pds.ErrServerError, "test setup: the retry's outcome is unknown too")
				fake.createErr = nil
				require.Empty(t, fake.ops, "test setup: the retry has not committed")
				listsBefore := fake.listCalls
				_, retryErr = service.CreateVote(context.Background(), voter(t, testVoterDID),
					votes.CreateVoteRequest{Subject: subject(), Direction: testCase.retryDirection})
				require.Greater(t, fake.listCalls, listsBefore, "test setup: the third create scans the PDS")
			}

			lateCommitAfterRetryLanded := false
			if testCase.lateCommitAfterRetry {
				require.NoError(t, retryErr, "test setup: the retry succeeds before the late commit")
				lateCommitAfterRetryLanded = fake.commitLate(firstAttempt)
			}

			var attemptedRecordKeys []string
			for _, attempt := range fake.attemptedCreates() {
				attemptedRecordKeys = append(attemptedRecordKeys, attempt.rkey)
			}
			wantRecordKeys := make([]string, testCase.wantCreateAttempts)
			for index := range wantRecordKeys {
				wantRecordKeys[index] = firstAttempt.rkey
			}
			require.Equal(t, wantRecordKeys, attemptedRecordKeys,
				"every create attempt uses the first attempt's record key")
			require.Equal(t, map[string]string{firstAttempt.rkey: testCase.wantDirection + " " + testSubject}, recordsByKey(fake),
				"the viewer's repo holds exactly one vote for the subject, at the first attempt's record key")
			require.False(t, lateCommitAfterRetryLanded,
				"the first create's late commit is refused: the retry already holds its record key")
			if testCase.wantRetryFailed {
				require.Error(t, retryErr, "the retry's create collides with the late-committed record")
			} else {
				require.NoError(t, retryErr)
			}
		})
	}
}

func TestCreateVote_SuccessNamingAnotherRecordIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		answer func(rkey string) string
	}{
		{"another repo", func(rkey string) string {
			return "at://did:plc:cccccccccccccccccccccccc/social.coves.feed.vote/" + rkey
		}},
		{"another record key", func(string) string { return voteRecordURI("3kanotherkey") }},
		{"another collection", func(rkey string) string { return "at://" + testVoterDID + "/social.coves.feed.like/" + rkey }},
		{"no URI", func(string) string { return "" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))

			fake.createAnswer = func(rkey string) (string, string) { return testCase.answer(rkey), "bafyanswer" }
			_, err := createUpVote(t, service)
			require.Error(t, err, "a success that names another record does not establish the vote")
			fake.createAnswer = nil
			attempts := fake.attemptedCreates()
			require.Len(t, attempts, 1, "test setup: one create reached the PDS")
			firstAttempt := attempts[0]

			listsBefore := fake.listCalls
			response, err := createUpVote(t, service)
			require.NoError(t, err)
			require.Greater(t, fake.listCalls, listsBefore, "the outcome is unknown, so the next write scans the PDS")
			var attemptedRecordKeys []string
			for _, attempt := range fake.attemptedCreates() {
				attemptedRecordKeys = append(attemptedRecordKeys, attempt.rkey)
			}
			require.Equal(t, []string{firstAttempt.rkey, firstAttempt.rkey}, attemptedRecordKeys,
				"the scan finds no vote, so the next create uses the first attempt's record key")
			require.Equal(t, voteRecordURI(firstAttempt.rkey), response.URI)
			require.Equal(t, map[string]string{firstAttempt.rkey: "up " + testSubject}, recordsByKey(fake))
		})
	}
}
