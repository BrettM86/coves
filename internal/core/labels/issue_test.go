package labels_test

import (
	"testing"
	"time"

	"Coves/internal/core/labels"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextCreatedAt(t *testing.T) {
	previous := "2026-10-22T12:00:00.500Z"
	previousTime := time.Date(2026, 10, 22, 12, 0, 0, 500*int(time.Millisecond), time.UTC)
	for _, test := range []struct {
		name     string
		now      time.Time
		previous string
		want     time.Time
		wantErr  bool
	}{
		{
			name: "the first label takes now", now: previousTime, previous: "",
			want: previousTime,
		},
		{
			name: "now after the previous label is used as is", now: previousTime.Add(2 * time.Second), previous: previous,
			want: previousTime.Add(2 * time.Second),
		},
		{
			name: "now in the same millisecond as the previous label moves one millisecond later",
			now:  previousTime.Add(300 * time.Microsecond), previous: previous,
			want: previousTime.Add(time.Millisecond),
		},
		{
			name: "now before the previous label moves one millisecond after it",
			now:  previousTime.Add(-time.Hour), previous: previous,
			want: previousTime.Add(time.Millisecond),
		},
		{
			name: "now is truncated to millisecond precision and converted to UTC",
			now:  time.Date(2026, 10, 22, 5, 0, 1, 123456789, time.FixedZone("PDT", -7*60*60)), previous: previous,
			want: time.Date(2026, 10, 22, 12, 0, 1, 123*int(time.Millisecond), time.UTC),
		},
		{
			name: "an unparsable previous label cts is an error", now: previousTime, previous: "not a timestamp",
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := labels.NextCreatedAt(test.now, test.previous)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, test.want.Equal(got), "want %s, got %s", test.want.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}
