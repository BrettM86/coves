package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"

	"github.com/stretchr/testify/assert"
)

// TestSweepRows_RetriesHostedByPendingRowsAlone: community refreshes answered
// "hostedBy verification pending" in the parallel pass (another refresh holds
// the shared verification in flight) are retried after it, one row at a time.
// A retry that succeeds counts as updated; a row still pending after its retry
// is reported as hostedby-pending, not error.
func TestSweepRows_RetriesHostedByPendingRowsAlone(t *testing.T) {
	t.Parallel()
	pending := fmt.Errorf("unresolved reference: bidirectional DID verification required: %w",
		jetstream.ErrHostedByVerificationPending)
	rows := []bridgedRow{
		{kind: kindCommunity, did: "did:plc:firstpond", handle: "firstpond.bridge.test"},
		{kind: kindCommunity, did: "did:plc:secondpond", handle: "secondpond.bridge.test"},
		{kind: kindCommunity, did: "did:plc:thirdpond", handle: "thirdpond.bridge.test"},
	}
	cases := []struct {
		name          string
		retrySucceeds bool
		wantStatus    rowStatus
		wantReason    string
	}{
		{name: "retry alone succeeds", retrySucceeds: true, wantStatus: rowUpdated, wantReason: ""},
		{name: "still pending after retry", retrySucceeds: false, wantStatus: rowFailed, wantReason: "hostedby-pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var mu sync.Mutex
			calls := map[string]int{}
			active, mostActiveDuringRetry := 0, 0
			var firstCallsStarted sync.WaitGroup
			firstCallsStarted.Add(len(rows))
			everyFirstCallStarted := make(chan struct{})
			go func() {
				firstCallsStarted.Wait()
				close(everyFirstCallStarted)
			}()
			refresh := func(ctx context.Context, did string) (jetstream.IdentityHandleRefresh, error) {
				mu.Lock()
				calls[did]++
				call := calls[did]
				active++
				if call > 1 {
					mostActiveDuringRetry = max(mostActiveDuringRetry, active)
				}
				mu.Unlock()
				defer func() {
					mu.Lock()
					active--
					mu.Unlock()
				}()
				if call == 1 {
					// Every row's first refresh overlaps the others, as in the parallel pass.
					firstCallsStarted.Done()
					select {
					case <-everyFirstCallStarted:
					case <-ctx.Done():
					}
					return jetstream.IdentityHandleRefresh{}, pending
				}
				if tc.retrySucceeds {
					return jetstream.IdentityHandleRefresh{VerifiedHandle: "new" + did, Changed: true}, nil
				}
				return jetstream.IdentityHandleRefresh{}, pending
			}

			outcomes := sweepRows(ctx, rows, len(rows), func(ctx context.Context, row bridgedRow) rowOutcome {
				return refreshRow(ctx, refresh, row)
			})

			for index, row := range rows {
				assert.Equal(t, tc.wantStatus, outcomes[index].status, "%s status", row.did)
				assert.Equal(t, tc.wantReason, outcomes[index].differing.Reason, "%s reason", row.did)
				assert.Equal(t, 2, calls[row.did], "%s refreshes", row.did)
			}
			assert.Equal(t, 1, mostActiveDuringRetry, "refreshes running at once during the retries")
		})
	}
}
