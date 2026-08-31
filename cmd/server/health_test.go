package main

import (
	"Coves/internal/atproto/jetstream"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// noopEventHandler satisfies jetstream.EventHandler for connectors that are
// constructed but never started in tests.
type noopEventHandler struct{}

func (noopEventHandler) HandleEvent(ctx context.Context, event *jetstream.JetstreamEvent) error {
	return nil
}

// fakeDeadLetterQueue supplies canned backlog counts so the health handler can
// be exercised without Postgres.
type fakeDeadLetterQueue struct {
	backlogs map[string]int64
	countErr error
}

func (f *fakeDeadLetterQueue) CountDeadLetters(ctx context.Context) (map[string]int64, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	return f.backlogs, nil
}

// connectedStatus builds a healthy, connected consumer status.
func connectedStatus(name string, now time.Time) jetstream.ConnectorStatus {
	connectedSince := now.Add(-10 * time.Minute)
	return jetstream.ConnectorStatus{
		Name:           name,
		Connected:      true,
		ConnectedSince: &connectedSince,
	}
}

func TestBuildConsumerHealthResponse_AllConnected(t *testing.T) {
	now := time.Now()
	statuses := []jetstream.ConnectorStatus{
		connectedStatus(jetstream.ConsumerUsers, now),
		connectedStatus(jetstream.ConsumerPosts, now),
	}

	response, httpCode := buildConsumerHealthResponse(statuses, nil, false, now)

	if httpCode != http.StatusOK {
		t.Errorf("expected HTTP %d, got %d", http.StatusOK, httpCode)
	}
	if response.Status != "ok" {
		t.Errorf("expected status %q, got %q", "ok", response.Status)
	}
	if response.DeadLetterBacklogUnknown {
		t.Error("expected deadLetterBacklogUnknown to be false")
	}
	if len(response.Consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d", len(response.Consumers))
	}
}

func TestBuildConsumerHealthResponse_DisconnectedPastThresholdIsStalled(t *testing.T) {
	now := time.Now()
	disconnectedSince := now.Add(-(consumerStalledThreshold + time.Second))
	statuses := []jetstream.ConnectorStatus{
		connectedStatus(jetstream.ConsumerUsers, now),
		{
			Name:              jetstream.ConsumerVotes,
			Connected:         false,
			DisconnectedSince: &disconnectedSince,
		},
	}

	response, httpCode := buildConsumerHealthResponse(statuses, nil, false, now)

	if httpCode != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP %d, got %d", http.StatusServiceUnavailable, httpCode)
	}
	if response.Status != "stalled" {
		t.Errorf("expected status %q, got %q", "stalled", response.Status)
	}
}

func TestBuildConsumerHealthResponse_RecentDisconnectIsNotStalled(t *testing.T) {
	now := time.Now()
	disconnectedSince := now.Add(-(consumerStalledThreshold - time.Second))
	statuses := []jetstream.ConnectorStatus{
		{
			Name:              jetstream.ConsumerComments,
			Connected:         false,
			DisconnectedSince: &disconnectedSince,
		},
	}

	response, httpCode := buildConsumerHealthResponse(statuses, nil, false, now)

	if httpCode != http.StatusOK {
		t.Errorf("expected HTTP %d, got %d", http.StatusOK, httpCode)
	}
	if response.Status != "ok" {
		t.Errorf("expected status %q, got %q", "ok", response.Status)
	}
}

func TestBuildConsumerHealthResponse_StalledWinsOverDegraded(t *testing.T) {
	now := time.Now()
	disconnectedSince := now.Add(-(consumerStalledThreshold + time.Minute))
	statuses := []jetstream.ConnectorStatus{
		{
			Name:              jetstream.ConsumerPosts,
			Connected:         false,
			DisconnectedSince: &disconnectedSince,
		},
	}

	response, httpCode := buildConsumerHealthResponse(statuses, nil, true, now)

	if httpCode != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP %d, got %d", http.StatusServiceUnavailable, httpCode)
	}
	if response.Status != "stalled" {
		t.Errorf("expected status %q, got %q", "stalled", response.Status)
	}
	if !response.DeadLetterBacklogUnknown {
		t.Error("expected deadLetterBacklogUnknown to remain true alongside stalled")
	}
}

func TestBuildConsumerHealthResponse_BacklogMappedByConsumerName(t *testing.T) {
	now := time.Now()
	statuses := []jetstream.ConnectorStatus{
		connectedStatus(jetstream.ConsumerUsers, now),
		connectedStatus(jetstream.ConsumerVotes, now),
		connectedStatus(jetstream.ConsumerComments, now),
	}
	backlogs := map[string]int64{
		jetstream.ConsumerUsers: 3,
		jetstream.ConsumerVotes: 7,
		// comments intentionally absent: no dead letters → 0
	}

	response, _ := buildConsumerHealthResponse(statuses, backlogs, false, now)

	want := map[string]int64{
		jetstream.ConsumerUsers:    3,
		jetstream.ConsumerVotes:    7,
		jetstream.ConsumerComments: 0,
	}
	if len(response.Consumers) != len(want) {
		t.Fatalf("expected %d consumers, got %d", len(want), len(response.Consumers))
	}
	for _, consumer := range response.Consumers {
		if consumer.DeadLetterBacklog != want[consumer.Name] {
			t.Errorf("consumer %s: expected backlog %d, got %d",
				consumer.Name, want[consumer.Name], consumer.DeadLetterBacklog)
		}
	}
}

func TestBuildConsumerHealthResponse_AgeFields(t *testing.T) {
	now := time.Now()

	lastEventAt := now.Add(-90 * time.Second)
	withActivity := connectedStatus(jetstream.ConsumerPosts, now)
	withActivity.LastEventAt = &lastEventAt
	withActivity.CursorTimeUS = now.Add(-2 * time.Minute).UnixMicro()

	// Never received an event and cursor still 0: both ages must be omitted.
	withoutActivity := connectedStatus(jetstream.ConsumerUsers, now)

	response, _ := buildConsumerHealthResponse(
		[]jetstream.ConnectorStatus{withActivity, withoutActivity}, nil, false, now)

	active := response.Consumers[0]
	if active.LastEventAgeSeconds == nil {
		t.Fatal("expected lastEventAgeSeconds to be set")
	}
	if *active.LastEventAgeSeconds != 90 {
		t.Errorf("expected lastEventAgeSeconds 90, got %d", *active.LastEventAgeSeconds)
	}
	if active.CursorAgeSeconds == nil {
		t.Fatal("expected cursorAgeSeconds to be set")
	}
	if *active.CursorAgeSeconds != 120 {
		t.Errorf("expected cursorAgeSeconds 120, got %d", *active.CursorAgeSeconds)
	}

	idle := response.Consumers[1]
	if idle.LastEventAgeSeconds != nil {
		t.Errorf("expected lastEventAgeSeconds omitted, got %d", *idle.LastEventAgeSeconds)
	}
	if idle.CursorAgeSeconds != nil {
		t.Errorf("expected cursorAgeSeconds omitted, got %d", *idle.CursorAgeSeconds)
	}
}

// newTestConnectors builds real (never started) connectors. A connector that
// was never started reports Connected=false with no DisconnectedSince, which
// the handler treats as boot grace: consumers start alongside the HTTP server
// and need a moment to connect, so a fresh boot must not report stalled.
func newTestConnectors(names ...string) []*jetstream.Connector {
	connectors := make([]*jetstream.Connector, 0, len(names))
	for _, name := range names {
		connectors = append(connectors,
			jetstream.NewConnector(name, "ws://localhost:0/subscribe", noopEventHandler{}))
	}
	return connectors
}

func serveConsumerHealth(t *testing.T, connectors []*jetstream.Connector, queue jetstream.DeadLetterCounter) (*httptest.ResponseRecorder, consumerHealthResponse) {
	t.Helper()
	handler := consumerHealthHandler(connectors, queue)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/consumers", nil))

	var response consumerHealthResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}
	return recorder, response
}

func TestConsumerHealthHandler_NeverStartedConnectorsGetBootGrace(t *testing.T) {
	connectors := newTestConnectors(jetstream.ConsumerUsers, jetstream.ConsumerCommunities)
	queue := &fakeDeadLetterQueue{backlogs: map[string]int64{}}

	recorder, response := serveConsumerHealth(t, connectors, queue)

	if recorder.Code != http.StatusOK {
		t.Errorf("expected HTTP %d, got %d", http.StatusOK, recorder.Code)
	}
	if response.Status != "ok" {
		t.Errorf("expected status %q, got %q", "ok", response.Status)
	}
	if len(response.Consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d", len(response.Consumers))
	}
	for _, consumer := range response.Consumers {
		if consumer.Connected {
			t.Errorf("consumer %s: expected Connected=false before Start", consumer.Name)
		}
	}
}

func TestConsumerHealthHandler_CountDeadLettersErrorIsDegraded(t *testing.T) {
	connectors := newTestConnectors(jetstream.ConsumerUsers)
	queue := &fakeDeadLetterQueue{countErr: errors.New("postgres is down")}

	recorder, response := serveConsumerHealth(t, connectors, queue)

	// Degraded keeps HTTP 200: indexing itself may be fine, but the backlog
	// is unknowable, which monitoring must be able to distinguish from 0.
	if recorder.Code != http.StatusOK {
		t.Errorf("expected HTTP %d, got %d", http.StatusOK, recorder.Code)
	}
	if response.Status != "degraded" {
		t.Errorf("expected status %q, got %q", "degraded", response.Status)
	}
	if !response.DeadLetterBacklogUnknown {
		t.Error("expected deadLetterBacklogUnknown=true when the count fails")
	}
	if response.Consumers[0].DeadLetterBacklog != 0 {
		t.Errorf("expected backlog 0 when unknown, got %d", response.Consumers[0].DeadLetterBacklog)
	}
}

func TestConsumerHealthHandler_BacklogCountsInResponse(t *testing.T) {
	connectors := newTestConnectors(jetstream.ConsumerUsers, jetstream.ConsumerPosts)
	queue := &fakeDeadLetterQueue{backlogs: map[string]int64{
		jetstream.ConsumerPosts: 5,
	}}

	recorder, response := serveConsumerHealth(t, connectors, queue)

	if recorder.Code != http.StatusOK {
		t.Errorf("expected HTTP %d, got %d", http.StatusOK, recorder.Code)
	}
	if response.DeadLetterBacklogUnknown {
		t.Error("expected deadLetterBacklogUnknown to be false")
	}
	byName := make(map[string]int64, len(response.Consumers))
	for _, consumer := range response.Consumers {
		byName[consumer.Name] = consumer.DeadLetterBacklog
	}
	if byName[jetstream.ConsumerPosts] != 5 {
		t.Errorf("expected posts backlog 5, got %d", byName[jetstream.ConsumerPosts])
	}
	if byName[jetstream.ConsumerUsers] != 0 {
		t.Errorf("expected users backlog 0, got %d", byName[jetstream.ConsumerUsers])
	}
}

func TestConsumerHealthHandler_DoesNotLeakCountError(t *testing.T) {
	connectors := newTestConnectors(jetstream.ConsumerUsers)
	secret := "connection refused to secret-internal-host:5432"
	queue := &fakeDeadLetterQueue{countErr: errors.New(secret)}

	handler := consumerHealthHandler(connectors, queue)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/consumers", nil))

	body := recorder.Body.String()
	if body == "" {
		t.Fatal("expected a response body")
	}
	// The raw error must never reach this public endpoint.
	if strings.Contains(body, secret) {
		t.Errorf("response body leaked the internal error: %s", body)
	}
}
