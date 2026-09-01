package bridgedvotes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const unrelatedSweepPDSURL = "https://unrelated.test"

type sweepSelectCall struct {
	storedHosts []string
	lookback    time.Duration
	limit       int
}

type sweepStore struct {
	mu sync.Mutex

	distinctURLs []string
	candidates   []bridgedvotes.Candidate
	applyErr     error
	markErr      error
	distinctErr  error
	// selectErrByStoredHost fails SelectCandidates when the filter names that
	// stored host, so one host's selection can fail while another's succeeds.
	selectErrByStoredHost map[string]error
	// Exercises the poller's defense when a buggy or racing Store returns rows
	// outside the exact stored-host filter it was given.
	ignoreHostFilter  bool
	rotateByWatermark bool
	polledAt          map[string]time.Time
	markSequence      int64

	distinctCalls int
	selectCalls   []sweepSelectCall
	applied       []bridgedvotes.Aggregate
	marked        [][]string
}

func (s *sweepStore) SelectCandidates(_ context.Context, storedHosts []string, lookback time.Duration, limit int) ([]bridgedvotes.Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.selectCalls = append(s.selectCalls, sweepSelectCall{
		storedHosts: append([]string(nil), storedHosts...),
		lookback:    lookback,
		limit:       limit,
	})
	for _, host := range storedHosts {
		if err := s.selectErrByStoredHost[host]; err != nil {
			return nil, err
		}
	}
	allowed := make(map[string]struct{}, len(storedHosts))
	for _, host := range storedHosts {
		allowed[host] = struct{}{}
	}
	selected := make([]bridgedvotes.Candidate, 0, len(s.candidates))
	for _, candidate := range s.candidates {
		if _, ok := allowed[candidate.StoredPDSURL]; s.ignoreHostFilter || ok {
			selected = append(selected, candidate)
		}
	}
	if s.rotateByWatermark {
		sort.SliceStable(selected, func(i, j int) bool {
			left, leftPolled := s.polledAt[selected[i].URI]
			right, rightPolled := s.polledAt[selected[j].URI]
			switch {
			case leftPolled != rightPolled:
				return !leftPolled
			case !leftPolled:
				return selected[i].URI < selected[j].URI
			case left.Equal(right):
				return selected[i].URI < selected[j].URI
			default:
				return left.Before(right)
			}
		})
	}
	if limit >= 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected, nil
}

func (s *sweepStore) DistinctCommunityPDSURLs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.distinctCalls++
	if s.distinctErr != nil {
		return nil, s.distinctErr
	}
	return append([]string(nil), s.distinctURLs...), nil
}

func (s *sweepStore) ApplyAggregate(_ context.Context, aggregate bridgedvotes.Aggregate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, aggregate)
	return s.applyErr
}

func (s *sweepStore) MarkPolled(_ context.Context, uris []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = append(s.marked, append([]string(nil), uris...))
	if s.rotateByWatermark {
		if s.polledAt == nil {
			s.polledAt = make(map[string]time.Time)
		}
		s.markSequence++
		stamp := time.Unix(s.markSequence, 0).UTC()
		for _, uri := range uris {
			s.polledAt[uri] = stamp
		}
	}
	return s.markErr
}

func (s *sweepStore) selectCallsSnapshot() []sweepSelectCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := make([]sweepSelectCall, len(s.selectCalls))
	for i, call := range s.selectCalls {
		calls[i] = sweepSelectCall{
			storedHosts: append([]string(nil), call.storedHosts...),
			lookback:    call.lookback,
			limit:       call.limit,
		}
	}
	return calls
}

func (s *sweepStore) appliedSnapshot() []bridgedvotes.Aggregate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridgedvotes.Aggregate(nil), s.applied...)
}

func (s *sweepStore) markedSnapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	marked := make([][]string, len(s.marked))
	for i, uris := range s.marked {
		marked[i] = append([]string(nil), uris...)
	}
	return marked
}

func (s *sweepStore) callCounts() (distinct, selectCalls, applied, marked int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.distinctCalls, len(s.selectCalls), len(s.applied), len(s.marked)
}

type sweepServedAggregate struct {
	URI       string `json:"uri"`
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	UpdatedAt string `json:"updatedAt"`
}

type sweepBridge struct {
	mu sync.Mutex

	status     int
	aggregates map[string]sweepServedAggregate
	batches    [][]string
}

func (b *sweepBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uris := decodeClientURIs(r.URL.Query()["uris"])
	b.mu.Lock()
	b.batches = append(b.batches, append([]string(nil), uris...))
	status := b.status
	aggregates := make([]sweepServedAggregate, 0, len(uris))
	for _, uri := range uris {
		if aggregate, ok := b.aggregates[uri]; ok {
			aggregates = append(aggregates, aggregate)
		}
	}
	b.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Aggregates []sweepServedAggregate `json:"aggregates"`
	}{Aggregates: aggregates}); err != nil {
		panic(fmt.Sprintf("encode sweep bridge response: %v", err))
	}
}

func (b *sweepBridge) batchesSnapshot() [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	batches := make([][]string, len(b.batches))
	for i, batch := range b.batches {
		batches[i] = append([]string(nil), batch...)
	}
	return batches
}

func TestSweepNormalizesStoredHostsAndDialsConfiguredHost(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	const candidateURI = "at://did:plc:sweephost/social.coves.community.postv2/post"
	store := &sweepStore{
		distinctURLs: []string{storedURL, unrelatedSweepPDSURL},
		candidates: []bridgedvotes.Candidate{
			{URI: candidateURI, StoredPDSURL: storedURL},
		},
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	sweepOK(t, poller)
	calls := store.selectCallsSnapshot()
	require.Len(t, calls, 1)
	require.Equal(t, []string{storedURL}, calls[0].storedHosts,
		"SelectCandidates must receive the exact stored URL that normalized-matched trusted config")
	require.Equal(t, [][]string{{candidateURI}}, bridge.batchesSnapshot(),
		"the configured host, not the cosmetic stored URL, must be dialled")
}

func TestSweepNormalizesHostCaseAndDefaultPort(t *testing.T) {
	t.Parallel()

	const (
		storedURL = "https://BRIDGE.TEST:443/" // coves:allow-host-literal: inert normalization input with default port; no candidate is returned or dialled
		configURL = "https://bridge.test"      // coves:allow-host-literal: inert normalization input paired with storedURL; no candidate is returned or dialled
	)
	store := &sweepStore{distinctURLs: []string{storedURL}}
	poller := newSweepPoller(t, store, http.DefaultClient, []string{configURL}, bridgedvotes.Options{})

	sweepOK(t, poller)
	calls := store.selectCallsSnapshot()
	require.Len(t, calls, 1)
	require.Equal(t, []string{storedURL}, calls[0].storedHosts)
}

func TestSweepBatchesEachHostAtOneHundredURIs(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   sweepCandidates(storedURL, 150),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 150})

	sweepOK(t, poller)
	batches := bridge.batchesSnapshot()
	require.Len(t, batches, 2)
	sizes := make([]int, len(batches))
	for i, batch := range batches {
		sizes[i] = len(batch)
		require.LessOrEqual(t, len(batch), 100)
	}
	require.ElementsMatch(t, []int{100, 50}, sizes)
}

func TestSweepAppliesServedAggregatesAndMarksEveryPolledURI(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	asOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	bridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		"at://did:plc:sweepapply/social.coves.community.postv2/served-post": {
			URI: "at://did:plc:sweepapply/social.coves.community.postv2/served-post", Upvotes: 5, Downvotes: 2, UpdatedAt: updatedAt,
		},
		"at://did:plc:sweepapply/social.coves.community.comment/served-comment": {
			URI: "at://did:plc:sweepapply/social.coves.community.comment/served-comment", Upvotes: 2, Downvotes: 0, UpdatedAt: updatedAt,
		},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	polledURIs := []string{
		"at://did:plc:sweepapply/social.coves.community.postv2/served-post",
		"at://did:plc:sweepapply/social.coves.community.postv2/omitted-post",
		"at://did:plc:sweepapply/social.coves.community.comment/served-comment",
	}
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates: []bridgedvotes.Candidate{
			{URI: polledURIs[0], StoredPDSURL: storedURL},
			{URI: polledURIs[1], StoredPDSURL: storedURL},
			{URI: polledURIs[2], StoredPDSURL: storedURL},
		},
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	sweepOK(t, poller)
	require.ElementsMatch(t, []bridgedvotes.Aggregate{
		{URI: polledURIs[0], Upvotes: 5, Downvotes: 2, AsOf: asOf},
		{URI: polledURIs[2], Upvotes: 2, Downvotes: 0, AsOf: asOf},
	}, store.appliedSnapshot())
	require.ElementsMatch(t, polledURIs, flattenMarkedURIs(store.markedSnapshot()),
		"served and omitted URIs must both advance after a successful fetch")
}

func TestSweepFailedBatchDoesNotApplyOrMark(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{status: http.StatusServiceUnavailable}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   sweepCandidates(storedURL, 3),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.True(t, bridgedvotes.IsTransient(err), "HTTP 503 must surface as a transient sweep failure")
	require.Empty(t, store.appliedSnapshot(), "a failed fetch cannot apply aggregates")
	require.Empty(t, store.markedSnapshot(), "a failed fetch cannot advance watermarks")
}

func TestSweepNeverRoutesCandidatesFromUnmatchedStoredHost(t *testing.T) {
	t.Parallel()

	const (
		matchedURI   = "at://did:plc:sweeproute/social.coves.community.postv2/matched"
		unmatchedURI = "at://did:plc:sweeproute/social.coves.community.postv2/unmatched"
	)
	bridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		unmatchedURI: {
			URI: unmatchedURI, Upvotes: 9, Downvotes: 1, UpdatedAt: "2026-08-31T02:04:01.080Z",
		},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{
		distinctURLs:     []string{storedURL, unrelatedSweepPDSURL},
		ignoreHostFilter: true,
		candidates: []bridgedvotes.Candidate{
			{URI: matchedURI, StoredPDSURL: storedURL},
			{URI: unmatchedURI, StoredPDSURL: unrelatedSweepPDSURL},
		},
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	sweepOK(t, poller)
	require.NotContains(t, flattenBatches(bridge.batchesSnapshot()), unmatchedURI)
	require.NotContains(t, flattenMarkedURIs(store.markedSnapshot()), unmatchedURI)
	appliedURIs := make([]string, 0, len(store.appliedSnapshot()))
	for _, aggregate := range store.appliedSnapshot() {
		appliedURIs = append(appliedURIs, aggregate.URI)
	}
	require.NotContains(t, appliedURIs, unmatchedURI)
}

func TestSweepWithNoTrustedHostsIsNoOp(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	store := &sweepStore{
		distinctURLs: []string{server.URL + "/"},
		candidates:   sweepCandidates(server.URL+"/", 1),
	}
	poller := newSweepPoller(t, store, server.Client(), nil, bridgedvotes.Options{})

	sweepOK(t, poller)
	distinctCalls, selectCalls, applied, marked := store.callCounts()
	require.Zero(t, distinctCalls)
	require.Zero(t, selectCalls)
	require.Zero(t, applied)
	require.Zero(t, marked)
	require.Empty(t, bridge.batchesSnapshot())
}

func TestSweepZeroOptionsUseSaneSelectionDefaults(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	store := &sweepStore{distinctURLs: []string{server.URL + "/"}}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{})

	sweepOK(t, poller)
	calls := store.selectCallsSnapshot()
	require.Len(t, calls, 1)
	require.Equal(t, 90*24*time.Hour, calls[0].lookback, "the documented default lookback")
	require.Equal(t, 2000, calls[0].limit, "the documented default sweep cap")
}

func TestNewPollerRejectsNilDependencies(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	client := bridgedvotes.NewClient(server.Client())

	t.Run("nil Store", func(t *testing.T) {
		_, err := bridgedvotes.NewPoller(nil, client, []string{server.URL}, bridgedvotes.Options{})
		require.Error(t, err, "a poller without persistence cannot run safely")
	})
	t.Run("nil Client", func(t *testing.T) {
		_, err := bridgedvotes.NewPoller(&sweepStore{}, nil, []string{server.URL}, bridgedvotes.Options{})
		require.Error(t, err, "a poller without an HTTP client cannot fetch safely")
	})
}

func TestNewPollerRejectsSchemelessTrustedHosts(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	tests := []struct {
		name  string
		hosts []string
	}{
		{name: "all invalid", hosts: []string{"tdpl.io"}},
		{name: "mixed valid and invalid", hosts: []string{server.URL, "tdpl.io"}},
		{name: "path would be prefixed onto the XRPC path", hosts: []string{"https://tdpl.io/pds"}},
		{name: "credentials would be sent and logged", hosts: []string{"https://user:secret@tdpl.io"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := bridgedvotes.NewPoller(
				&sweepStore{}, bridgedvotes.NewClient(server.Client()), test.hosts, bridgedvotes.Options{},
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), "tdpl.io", "the configuration error must name the offending dial target")
		})
	}
}

func TestSweepNegativeOptionsUseSaneSelectionDefaults(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	store := &sweepStore{distinctURLs: []string{server.URL + "/"}}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{
		Lookback: -1,
		SweepCap: -1,
	})

	sweepOK(t, poller)
	calls := store.selectCallsSnapshot()
	require.Len(t, calls, 1)
	require.Equal(t, 90*24*time.Hour, calls[0].lookback, "the documented default lookback")
	require.Equal(t, 2000, calls[0].limit, "the documented default sweep cap")
}

func TestSweepPermanentFetchFailureMarksPoisonBatch(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{status: http.StatusBadRequest}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 3)
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   candidates,
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.Empty(t, store.appliedSnapshot(), "a rejected response cannot apply aggregates")
	require.ElementsMatch(t, candidateURIs(candidates), flattenMarkedURIs(store.markedSnapshot()),
		"a permanent poison batch must advance so it cannot wedge the rotation head")
}

func TestSweepPriorHostFailureIsNotMaskedByLaterCancellation(t *testing.T) {
	t.Parallel()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "first bridge failed", http.StatusInternalServerError)
	}))
	t.Cleanup(first.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(second.Close)
	firstStored := first.URL + "/"
	secondStored := second.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{firstStored, secondStored},
		candidates: []bridgedvotes.Candidate{
			{URI: "at://did:plc:sweepcancel/social.coves.community.postv2/first", StoredPDSURL: firstStored},
			{URI: "at://did:plc:sweepcancel/social.coves.community.postv2/second", StoredPDSURL: secondStored},
		},
	}
	poller := newSweepPoller(t, store, first.Client(), []string{first.URL, second.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500", "the first host failure must survive later cancellation")
	require.False(t, errors.Is(err, context.Canceled),
		"shutdown classification must not hide a real bridge failure from the job logger")
}

func TestSweepApplyFailurePreservesEarlierFetchFailures(t *testing.T) {
	t.Parallel()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "first bridge unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(first.Close)
	const (
		healthyURI = "at://did:plc:sweepmultierror/social.coves.community.postv2/healthy"
		updatedAt  = "2026-08-31T02:04:01.080Z"
	)
	secondBridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		healthyURI: {URI: healthyURI, Upvotes: 5, Downvotes: 2, UpdatedAt: updatedAt},
	}}
	second := httptest.NewServer(secondBridge)
	t.Cleanup(second.Close)
	firstStored := first.URL + "/"
	secondStored := second.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{firstStored, secondStored},
		candidates: []bridgedvotes.Candidate{
			{URI: "at://did:plc:sweepmultierror/social.coves.community.postv2/failing", StoredPDSURL: firstStored},
			{URI: healthyURI, StoredPDSURL: secondStored},
		},
		applyErr: errors.New("apply database unavailable"),
	}
	poller := newSweepPoller(t, store, first.Client(), []string{first.URL, second.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "503", "the accumulated bridge failure must be retained")
	require.Contains(t, err.Error(), "apply database unavailable", "the later Store failure must also be retained")
}

func TestSweepIsolatesTransientFailureAndCompletesHealthyHost(t *testing.T) {
	t.Parallel()

	failingBridge := &sweepBridge{status: http.StatusServiceUnavailable}
	failingServer := httptest.NewServer(failingBridge)
	t.Cleanup(failingServer.Close)
	const (
		failingURI = "at://did:plc:sweepisolation/social.coves.community.postv2/failing"
		healthyURI = "at://did:plc:sweepisolation/social.coves.community.postv2/healthy"
		updatedAt  = "2026-08-31T02:04:01.080Z"
	)
	asOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	healthyBridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		healthyURI: {URI: healthyURI, Upvotes: 6, Downvotes: 1, UpdatedAt: updatedAt},
	}}
	healthyServer := httptest.NewServer(healthyBridge)
	t.Cleanup(healthyServer.Close)
	failingStored := failingServer.URL + "/"
	healthyStored := healthyServer.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{failingStored, healthyStored},
		candidates: []bridgedvotes.Candidate{
			{URI: failingURI, StoredPDSURL: failingStored},
			{URI: healthyURI, StoredPDSURL: healthyStored},
		},
	}
	poller := newSweepPoller(t, store, failingServer.Client(),
		[]string{failingServer.URL, healthyServer.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err = poller.Sweep(context.Background())
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.Equal(t, []bridgedvotes.Aggregate{{
		URI: healthyURI, Upvotes: 6, Downvotes: 1, AsOf: asOf,
	}}, store.appliedSnapshot())
	marked := flattenMarkedURIs(store.markedSnapshot())
	require.Contains(t, marked, healthyURI)
	require.NotContains(t, marked, failingURI,
		"a transient failure must remain at its watermark for retry")
}

func TestSweepApplyErrorDoesNotMarkBatch(t *testing.T) {
	t.Parallel()

	const (
		uri       = "at://did:plc:sweepapplyerror/social.coves.community.postv2/post"
		updatedAt = "2026-08-31T02:04:01.080Z"
	)
	bridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		uri: {URI: uri, Upvotes: 5, Downvotes: 2, UpdatedAt: updatedAt},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	applyErr := errors.New("apply aggregate failed")
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   []bridgedvotes.Candidate{{URI: uri, StoredPDSURL: storedURL}},
		applyErr:     applyErr,
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.ErrorIs(t, err, applyErr)
	require.Empty(t, store.markedSnapshot(), "a batch with an unapplied aggregate cannot advance")
}

func TestSweepCapRotatesAcrossNeverPolledCandidatesOverSuccessiveSweeps(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 4)
	store := &sweepStore{
		distinctURLs:      []string{storedURL},
		candidates:        candidates,
		rotateByWatermark: true,
		polledAt:          make(map[string]time.Time),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 2})

	sweepOK(t, poller)
	sweepOK(t, poller)
	requested := flattenBatches(bridge.batchesSnapshot())
	require.Len(t, requested, 4)
	require.ElementsMatch(t, candidateURIs(candidates), requested,
		"two bounded sweeps must reach every never-polled candidate exactly once")
}

func TestSweepPerHostSelectionPreventsFailingBacklogFromStarvingHealthyHost(t *testing.T) {
	t.Parallel()

	failingBridge := &sweepBridge{status: http.StatusServiceUnavailable}
	failingServer := httptest.NewServer(failingBridge)
	t.Cleanup(failingServer.Close)
	healthyBridge := &sweepBridge{aggregates: make(map[string]sweepServedAggregate)}
	healthyServer := httptest.NewServer(healthyBridge)
	t.Cleanup(healthyServer.Close)
	failingStored := failingServer.URL + "/"
	healthyStored := healthyServer.URL + "/"
	failingCandidates := sweepCandidatesForHost(failingStored, "fairness-a", 20)
	healthyCandidates := sweepCandidatesForHost(healthyStored, "fairness-b", 3)
	const updatedAt = "2026-08-31T02:04:01.080Z"
	for _, candidate := range healthyCandidates {
		healthyBridge.aggregates[candidate.URI] = sweepServedAggregate{
			URI: candidate.URI, Upvotes: 3, Downvotes: 1, UpdatedAt: updatedAt,
		}
	}
	store := &sweepStore{
		distinctURLs: []string{failingStored, healthyStored},
		candidates:   append(failingCandidates, healthyCandidates...),
	}
	poller := newSweepPoller(t, store, failingServer.Client(),
		[]string{failingServer.URL, healthyServer.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	calls := store.selectCallsSnapshot()
	assert.Len(t, calls, 2, "each matched config host needs its own candidate budget")
	for _, call := range calls {
		assert.Len(t, call.storedHosts, 1)
		assert.Equal(t, 100, call.limit, "minimum per-host budget prevents starvation by small global caps")
	}
	healthyURIs := candidateURIs(healthyCandidates)
	assert.ElementsMatch(t, healthyURIs, flattenBatches(healthyBridge.batchesSnapshot()))
	assert.ElementsMatch(t, healthyURIs, aggregateURIs(store.appliedSnapshot()))
	marked := flattenMarkedURIs(store.markedSnapshot())
	for _, uri := range healthyURIs {
		assert.Contains(t, marked, uri)
	}
	for _, uri := range candidateURIs(failingCandidates) {
		assert.NotContains(t, marked, uri, "transiently failed host candidates must remain eligible for retry")
	}
}

func TestSweepCanceledMarkErrorDoesNotMaskPriorHostFailure(t *testing.T) {
	t.Parallel()

	transientServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "first host failed", http.StatusInternalServerError)
	}))
	t.Cleanup(transientServer.Close)
	permanentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "poison batch", http.StatusBadRequest)
	}))
	t.Cleanup(permanentServer.Close)
	transientStored := transientServer.URL + "/"
	permanentStored := permanentServer.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{transientStored, permanentStored},
		candidates: []bridgedvotes.Candidate{
			{URI: "at://did:plc:sweepdbcancel/social.coves.community.postv2/first", StoredPDSURL: transientStored},
			{URI: "at://did:plc:sweepdbcancel/social.coves.community.postv2/poison", StoredPDSURL: permanentStored},
		},
		markErr: fmt.Errorf("mark canceled: %w", context.Canceled),
	}
	poller := newSweepPoller(t, store, transientServer.Client(),
		[]string{transientServer.URL, permanentServer.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "500", "the first host failure must remain visible")
	require.NotEmpty(t, store.markedSnapshot(), "the permanent response must exercise the poison-batch DB path")
	require.False(t, errors.Is(err, context.Canceled),
		"a canceled DB leaf must not suppress an earlier actionable host failure")
}

func newSweepPoller(
	t *testing.T,
	store bridgedvotes.Store,
	httpClient *http.Client,
	trustedHosts []string,
	opts bridgedvotes.Options,
) *bridgedvotes.Poller {
	t.Helper()
	poller, err := bridgedvotes.NewPoller(store, bridgedvotes.NewClient(httpClient), trustedHosts, opts)
	require.NoError(t, err)
	return poller
}

func sweepCandidates(pdsURL string, count int) []bridgedvotes.Candidate {
	candidates := make([]bridgedvotes.Candidate, count)
	for i := range candidates {
		candidates[i] = bridgedvotes.Candidate{
			URI:          fmt.Sprintf("at://did:plc:sweepbatch/social.coves.community.postv2/%03d", i),
			StoredPDSURL: pdsURL,
		}
	}
	return candidates
}

func sweepCandidatesForHost(pdsURL, subject string, count int) []bridgedvotes.Candidate {
	candidates := make([]bridgedvotes.Candidate, count)
	for i := range candidates {
		candidates[i] = bridgedvotes.Candidate{
			URI:          fmt.Sprintf("at://did:plc:%s/social.coves.community.postv2/%03d", subject, i),
			StoredPDSURL: pdsURL,
		}
	}
	return candidates
}

func candidateURIs(candidates []bridgedvotes.Candidate) []string {
	uris := make([]string, len(candidates))
	for i, candidate := range candidates {
		uris[i] = candidate.URI
	}
	return uris
}

func aggregateURIs(aggregates []bridgedvotes.Aggregate) []string {
	uris := make([]string, len(aggregates))
	for i, aggregate := range aggregates {
		uris[i] = aggregate.URI
	}
	return uris
}

func flattenMarkedURIs(batches [][]string) []string {
	return flattenBatches(batches)
}

func flattenBatches(batches [][]string) []string {
	var flattened []string
	for _, batch := range batches {
		flattened = append(flattened, batch...)
	}
	return flattened
}

func TestSweepMalformedJSONWithOKStatusMarksPoisonBatch(t *testing.T) {
	t.Parallel()

	// The motivating failure: a proxy answering the XRPC path with an HTML page
	// and HTTP 200. Left unmarked at the oldest watermark it would be selected
	// first every sweep and block the host's rotation forever.
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 3)
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: candidates}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	report, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.False(t, bridgedvotes.IsTransient(err))
	require.EqualValues(t, 1, requests.Load())
	require.Empty(t, store.appliedSnapshot())
	require.ElementsMatch(t, candidateURIs(candidates), flattenMarkedURIs(store.markedSnapshot()))
	require.Equal(t, 3, report.PoisonMarked)
	require.Equal(t, 3, report.Marked)
	require.Equal(t, 1, report.FailedHosts)
}

func TestSweepSelectFailureIsIsolatedPerHost(t *testing.T) {
	t.Parallel()

	const (
		healthyURI = "at://did:plc:sweepselect/social.coves.community.postv2/healthy"
		updatedAt  = "2026-08-31T02:04:01.080Z"
	)
	brokenBridge := &sweepBridge{}
	brokenServer := httptest.NewServer(brokenBridge)
	t.Cleanup(brokenServer.Close)
	healthyBridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		healthyURI: {URI: healthyURI, Upvotes: 1, Downvotes: 0, UpdatedAt: updatedAt},
	}}
	healthyServer := httptest.NewServer(healthyBridge)
	t.Cleanup(healthyServer.Close)
	brokenStored := brokenServer.URL + "/"
	healthyStored := healthyServer.URL + "/"
	selectErr := errors.New("candidate query failed")
	store := &sweepStore{
		distinctURLs:          []string{brokenStored, healthyStored},
		candidates:            []bridgedvotes.Candidate{{URI: healthyURI, StoredPDSURL: healthyStored}},
		selectErrByStoredHost: map[string]error{brokenStored: selectErr},
	}
	poller := newSweepPoller(t, store, brokenServer.Client(),
		[]string{brokenServer.URL, healthyServer.URL}, bridgedvotes.Options{SweepCap: 10})

	report, err := poller.Sweep(context.Background())
	require.ErrorIs(t, err, selectErr, "the selection failure must be reported")
	require.Empty(t, brokenBridge.batchesSnapshot(), "a host whose selection failed is not dialled")
	require.Equal(t, []string{healthyURI}, flattenBatches(healthyBridge.batchesSnapshot()),
		"a selection failure on one host must not empty the sweep for the other")
	require.Equal(t, []string{healthyURI}, aggregateURIs(store.appliedSnapshot()))
	require.Equal(t, 1, report.FailedHosts)
	require.Equal(t, 2, report.MatchedHosts)
}

func TestSweepDistinctURLFailureReturnsBeforeDialing(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	distinctErr := errors.New("communities query failed")
	store := &sweepStore{distinctErr: distinctErr, candidates: sweepCandidates(server.URL+"/", 2)}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.ErrorIs(t, err, distinctErr)
	require.Empty(t, bridge.batchesSnapshot())
	_, selectCalls, _, marked := store.callCounts()
	require.Zero(t, selectCalls)
	require.Zero(t, marked)
}

func TestSweepMarkFailureAfterSuccessfulFetchIsReturned(t *testing.T) {
	t.Parallel()

	const (
		uri       = "at://did:plc:sweepmarkfail/social.coves.community.postv2/post"
		updatedAt = "2026-08-31T02:04:01.080Z"
	)
	bridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		uri: {URI: uri, Upvotes: 5, Downvotes: 2, UpdatedAt: updatedAt},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	markErr := errors.New("watermark update failed")
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   []bridgedvotes.Candidate{{URI: uri, StoredPDSURL: storedURL}},
		markErr:      markErr,
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	report, err := poller.Sweep(context.Background())
	require.ErrorIs(t, err, markErr)
	require.False(t, errors.Is(err, context.Canceled))
	require.Len(t, store.appliedSnapshot(), 1, "the aggregate was applied before the mark failed")
	require.Equal(t, 1, report.Applied)
	require.Zero(t, report.Marked, "a failed mark must not be counted as advanced")
}

func TestSweepTransientFailureStreakEventuallyMarksBatch(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{status: http.StatusInternalServerError}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 3)
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: candidates}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	// Two transient sweeps leave the batch at its watermark for retry.
	for sweep := 1; sweep <= 2; sweep++ {
		report, err := poller.Sweep(context.Background())
		require.Error(t, err)
		require.True(t, bridgedvotes.IsTransient(err))
		require.Empty(t, store.markedSnapshot(), "sweep %d must not advance a transiently failed batch", sweep)
		require.Zero(t, report.PoisonMarked)
	}

	// The third consecutive failure of the same leading batch is the head-of-line
	// wedge the transient classification would otherwise make permanent.
	report, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.ElementsMatch(t, candidateURIs(candidates), flattenMarkedURIs(store.markedSnapshot()),
		"a batch that fails transiently every sweep must eventually advance past the rotation")
	require.Equal(t, 3, report.PoisonMarked)

	// The streak resets: a fresh run of failures on the same batch gets its
	// full allowance again rather than poison-marking on the next sweep.
	_, err = poller.Sweep(context.Background())
	require.Error(t, err)
	require.Len(t, store.markedSnapshot(), 1, "the sweep after a poison mark starts a new streak")
}

func TestSweepHealthyFetchResetsTransientStreak(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{status: http.StatusServiceUnavailable}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 2)
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: candidates}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	for sweep := 1; sweep <= 2; sweep++ {
		_, err := poller.Sweep(context.Background())
		require.Error(t, err)
	}
	bridge.mu.Lock()
	bridge.status = http.StatusOK
	bridge.mu.Unlock()
	report, err := poller.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, report.Marked)
	require.Zero(t, report.PoisonMarked, "a successful fetch is an ordinary mark, not a poison mark")

	bridge.mu.Lock()
	bridge.status = http.StatusServiceUnavailable
	bridge.mu.Unlock()
	report, err = poller.Sweep(context.Background())
	require.Error(t, err)
	require.Zero(t, report.PoisonMarked, "the earlier streak must have been cleared by the healthy sweep")
}

func TestSweepStopsLaterBatchesOnSameHostAfterFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 2 {
			http.Error(w, "second batch fails", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aggregates":[]}`))
	}))
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	candidates := sweepCandidates(storedURL, 250)
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: candidates}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 250})

	report, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.EqualValues(t, 2, requests.Load(), "the third batch must not be sent to a host whose second batch failed")
	require.ElementsMatch(t, candidateURIs(candidates[:100]), flattenMarkedURIs(store.markedSnapshot()),
		"only the batch that completed advances")
	require.Equal(t, 100, report.Marked)
	require.Equal(t, 250, report.Candidates)
}

func TestSweepPerHostBudgetDividesTheCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		hosts     int
		cap       int
		wantLimit int
	}{
		{name: "two hosts share evenly", hosts: 2, cap: 400, wantLimit: 200},
		{name: "three hosts floor at one batch", hosts: 3, cap: 250, wantLimit: 100},
		{name: "single host keeps the whole cap", hosts: 1, cap: 50, wantLimit: 50},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var configured, stored []string
			var client *http.Client
			for i := 0; i < test.hosts; i++ {
				server := httptest.NewServer(&sweepBridge{})
				t.Cleanup(server.Close)
				configured = append(configured, server.URL)
				stored = append(stored, server.URL+"/")
				client = server.Client()
			}
			store := &sweepStore{distinctURLs: stored}
			poller := newSweepPoller(t, store, client, configured, bridgedvotes.Options{SweepCap: test.cap})

			sweepOK(t, poller)
			calls := store.selectCallsSnapshot()
			require.Len(t, calls, test.hosts)
			for _, call := range calls {
				require.Equal(t, test.wantLimit, call.limit)
			}
		})
	}
}

func TestSweepSoleCancellationStaysQuiet(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: sweepCandidates(storedURL, 2)}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := poller.Sweep(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled),
		"shutdown as the only failure must classify as cancellation so the job logger stays quiet")
	require.Empty(t, store.markedSnapshot(), "a canceled fetch is neither transient nor permanent; the batch stays put")
}

func TestSweepConfiguredButUnmatchedHostsSelectsNothing(t *testing.T) {
	t.Parallel()

	bridge := &sweepBridge{}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	store := &sweepStore{
		distinctURLs: []string{unrelatedSweepPDSURL},
		candidates:   sweepCandidates(unrelatedSweepPDSURL, 2),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	report := sweepOK(t, poller)
	_, selectCalls, _, _ := store.callCounts()
	require.Zero(t, selectCalls)
	require.Empty(t, bridge.batchesSnapshot())
	require.Equal(t, 1, report.TrustedHosts)
	require.Equal(t, 1, report.StoredHosts)
	require.Zero(t, report.MatchedHosts, "the report is how the job tells a misconfigured trust list from an idle one")
}

func TestSweepReportCountsCompletedWork(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	storedPrefix := "at://did:plc:sweepreport/social.coves.community.postv2/"
	bridge := &sweepBridge{aggregates: map[string]sweepServedAggregate{
		storedPrefix + "000": {URI: storedPrefix + "000", Upvotes: 1, Downvotes: 0, UpdatedAt: updatedAt},
		storedPrefix + "002": {URI: storedPrefix + "002", Upvotes: 2, Downvotes: 1, UpdatedAt: updatedAt},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{storedURL, unrelatedSweepPDSURL},
		candidates:   sweepCandidatesForHost(storedURL, "sweepreport", 3),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	report := sweepOK(t, poller)
	require.Equal(t, bridgedvotes.Report{
		TrustedHosts: 1,
		StoredHosts:  2,
		MatchedHosts: 1,
		Candidates:   3,
		Fetched:      2,
		Applied:      2,
		Marked:       3,
	}, report)
}

func sweepOK(t *testing.T, poller *bridgedvotes.Poller) bridgedvotes.Report {
	t.Helper()
	report, err := poller.Sweep(context.Background())
	require.NoError(t, err)
	return report
}

func TestSweepExpiredCycleDeadlineDoesNotCountTowardStreak(t *testing.T) {
	t.Parallel()

	// A bridge that answers only after the cycle deadline is the AppView
	// running out of time, not the bridge failing the batch. Three such
	// cycles must not poison-mark the batch.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		http.Error(w, "too late", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{distinctURLs: []string{storedURL}, candidates: sweepCandidates(storedURL, 2)}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	for sweep := 1; sweep <= 4; sweep++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		report, err := poller.Sweep(ctx)
		cancel()
		require.Error(t, err)
		require.True(t, errors.Is(err, context.DeadlineExceeded), "sweep %d must surface the expired deadline", sweep)
		require.Zero(t, report.PoisonMarked, "sweep %d: a cycle deadline is not a bridge failure", sweep)
		require.Empty(t, store.markedSnapshot())
	}
}

func TestSweepCanceledPoisonMarkKeepsFetchFailureVisible(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "poison batch", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	storedURL := server.URL + "/"
	store := &sweepStore{
		distinctURLs: []string{storedURL},
		candidates:   sweepCandidates(storedURL, 1),
		markErr:      fmt.Errorf("mark canceled: %w", context.Canceled),
	}
	poller := newSweepPoller(t, store, server.Client(), []string{server.URL}, bridgedvotes.Options{SweepCap: 10})

	_, err := poller.Sweep(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "400",
		"the permanent fetch failure must not be discarded along with the canceled mark that followed it")
	require.False(t, errors.Is(err, context.Canceled))
}
