package communitysignals_test

import (
	"testing"
	"time"

	"Coves/internal/core/communitysignals"
)

func TestDigestSlot(t *testing.T) {
	pacific := time.FixedZone("x", -8*3600)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "monday 08:59 utc gives previous monday",
			now:  time.Date(2026, 11, 16, 8, 59, 0, 0, time.UTC),
			want: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "monday 09:00 utc gives same instant",
			now:  time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "wednesday gives that week's monday",
			now:  time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "sunday 23:00 utc gives previous monday",
			now:  time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "monday 02:00 -08:00 is monday 10:00 utc",
			now:  time.Date(2026, 11, 16, 2, 0, 0, 0, pacific),
			want: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := communitysignals.DigestSlot(tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("DigestSlot(%s) = %s, want %s", tc.now.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if got.Location() != time.UTC {
				t.Errorf("DigestSlot location = %s, want UTC", got.Location())
			}
		})
	}
}
