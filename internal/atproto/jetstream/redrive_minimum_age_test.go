package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeadLetterRedriver_MinimumAgeDefersFreshRows(t *testing.T) {
	t.Parallel()

	const (
		consumer  = "minimum-age-consumer"
		oldTimeUS = int64(401_000)
		newTimeUS = int64(402_000)
		age       = time.Minute
	)

	for _, tc := range []struct {
		name           string
		options        []RedriveOption
		wantFreshCalls int
	}{
		{name: "configured minimum age", options: []RedriveOption{WithRedriveMinimumAge(age)}, wantFreshCalls: 0},
		{name: "zero minimum age replays everything", wantFreshCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			queue := newFakeDeadLetterQueue()
			require.NoError(t, queue.AddDeadLetter(ctx, consumer, oldTimeUS, testEventJSON(t, oldTimeUS), "old failure", 0))
			require.NoError(t, queue.AddDeadLetter(ctx, consumer, newTimeUS, testEventJSON(t, newTimeUS), "fresh failure", 0))
			now := time.Now()
			queue.mu.Lock()
			queue.rows[0].UpdatedAt = now.Add(-2 * age)
			queue.rows[1].UpdatedAt = now
			queue.mu.Unlock()

			handler := newFakeEventHandler()
			redriver := NewDeadLetterRedriver(fakeRedriveStore(queue), map[string]EventHandler{consumer: handler}, tc.options...)
			redriver.redriveAll(ctx)

			assert.Equal(t, 1, handler.calls(oldTimeUS),
				"a row older than the minimum age must be replayed so genuine failures still converge")
			assert.Equal(t, tc.wantFreshCalls, handler.calls(newTimeUS),
				"a fresh row must wait out the identity negative cache; replaying it now burns a bounded redrive without touching the network")
		})
	}
}
