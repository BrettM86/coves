package notifications

import (
	"encoding/base64"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNotificationCursorCodec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		at   time.Time
		id   int64
	}{
		{"microseconds", time.Date(2026, 9, 30, 12, 0, 0, 123456000, time.UTC), 42},
		{"maximum ID", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), math.MaxInt64},
		{"non-UTC location", time.Date(2026, 9, 30, 8, 0, 0, 1000, time.FixedZone("EDT", -4*60*60)), 7},
		{"longest canonical encoding", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), math.MinInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := DecodeCursor(EncodeCursor(Cursor{SortAt: tc.at, ID: tc.id}))
			require.NoError(t, err)
			require.True(t, decoded.SortAt.Equal(tc.at), "decoded sort time: %s", decoded.SortAt)
			require.Equal(t, tc.id, decoded.ID)
		})
	}

	t.Run("canonical UTC wire encoding", func(t *testing.T) {
		got := EncodeCursor(Cursor{
			SortAt: time.Date(2026, 9, 30, 8, 0, 0, 1000, time.FixedZone("EDT", -4*60*60)),
			ID:     7,
		})
		require.Equal(t, base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00.000001Z|7")), got)
	})
	t.Run("independently encoded wire decoding", func(t *testing.T) {
		decoded, err := DecodeCursor(base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00.123456Z|42")))
		require.NoError(t, err)
		require.True(t, decoded.SortAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 123456000, time.UTC)))
		require.Equal(t, int64(42), decoded.ID)
	})

	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"invalid base64", "!!!not-base64"},
		{"one field", base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00Z"))},
		{"three fields", base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00Z|1|2"))},
		{"invalid time", base64.RawURLEncoding.EncodeToString([]byte("yesterday|1"))},
		{"non-numeric ID", base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00Z|abc"))},
		{"fractional ID", base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00Z|1.5"))},
		{"over-length encoding of a valid position", base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T12:00:00Z|" + strings.Repeat("0", 70) + "42"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(tc.input)
			require.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}
