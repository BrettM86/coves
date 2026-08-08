package main

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/posts"
)

// The acceptance driver's entry in /health/consumers.
//
// The driver is the one moving part in this system with no natural symptom of
// its own. A consumer that dies disconnects, and the connector says so; a driver
// that dies simply stops running, produces no error and no log line, and every
// consequence shows up somewhere else entirely — as posts that never become
// visible, days later, in a community whose moderators assume nobody is posting.
// These three fields are the only place that failure is visible before a user
// reports it.

func TestBuildAcceptanceQueueHealth_ReportsBacklogAndAges(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-30 * time.Minute)
	lastPass := now.Add(-45 * time.Second)

	queue := buildAcceptanceQueueHealth(posts.QueueSnapshot{
		PendingBacklog:   12,
		OldestPendingAt:  &oldest,
		LastPassAt:       &lastPass,
		LastPassDeferred: 4,
		LastPassFailed:   1,
	}, now)

	if queue.PendingBacklog != 12 {
		t.Errorf("expected pendingBacklog 12, got %d", queue.PendingBacklog)
	}

	// The age, not the timestamp. A backlog that is merely BIG is a busy
	// instance; a backlog whose oldest entry keeps getting older is an engine
	// that has stopped settling anything, and only the age says which of those
	// is happening without the reader doing arithmetic against their own clock.
	if queue.OldestPendingAgeSeconds == nil {
		t.Fatal("expected oldestPendingAgeSeconds to be set for a non-empty backlog")
	}
	if *queue.OldestPendingAgeSeconds != 1800 {
		t.Errorf("expected oldestPendingAgeSeconds 1800, got %d", *queue.OldestPendingAgeSeconds)
	}

	if queue.LastPassAt == nil || !queue.LastPassAt.Equal(lastPass) {
		t.Errorf("expected lastPassAt %v, got %v", lastPass, queue.LastPassAt)
	}
	if queue.LastPassDeferred != 4 {
		t.Errorf("expected lastPassDeferred 4, got %d", queue.LastPassDeferred)
	}
	if queue.LastPassFailed != 1 {
		t.Errorf("expected lastPassFailed 1, got %d", queue.LastPassFailed)
	}
}

func TestBuildAcceptanceQueueHealth_OmitsAgesItCannotHonestlyReport(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// A driver that has never run, with an empty backlog. Both omissions matter
	// and for the same reason: a zero would be READ, and read wrongly.
	// oldestPendingAgeSeconds of 0 says "something arrived just now" when in
	// fact nothing is waiting, and a zero-valued lastPassAt renders as the epoch
	// — which looks like a driver that has been dead since 1970 rather than one
	// that started a minute ago.
	queue := buildAcceptanceQueueHealth(posts.QueueSnapshot{}, now)

	if queue.PendingBacklog != 0 {
		t.Errorf("expected an empty backlog, got %d", queue.PendingBacklog)
	}
	if queue.OldestPendingAgeSeconds != nil {
		t.Errorf("an empty backlog has no oldest entry; got age %d", *queue.OldestPendingAgeSeconds)
	}
	if queue.LastPassAt != nil {
		t.Errorf("a driver that has never run must not claim a last pass; got %v", *queue.LastPassAt)
	}

	encoded, err := json.Marshal(queue)
	if err != nil {
		t.Fatalf("marshalling the queue health: %v", err)
	}
	body := string(encoded)
	for _, omitted := range []string{"oldestPendingAgeSeconds", "lastPassAt"} {
		if contains(body, omitted) {
			t.Errorf("%s must be omitted from the JSON when unset, not serialised as null: %s", omitted, body)
		}
	}
}

func TestConsumerHealthResponse_OmitsTheQueueEntirelyWhenNoDriverRuns(t *testing.T) {
	// An AppView that hosts no communities runs no acceptance driver at all, and
	// its health response must not carry an all-zero queue — that reads as a
	// driver that is running and settling nothing, which is the exact shape of
	// the failure an operator is watching for.
	encoded, err := json.Marshal(consumerHealthResponse{Status: "ok"})
	if err != nil {
		t.Fatalf("marshalling the response: %v", err)
	}
	if contains(string(encoded), "acceptanceQueue") {
		t.Errorf("acceptanceQueue must be absent when no driver is wired: %s", encoded)
	}
}

// contains is strings.Contains, spelled locally so this file's imports stay the
// two it genuinely needs.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
