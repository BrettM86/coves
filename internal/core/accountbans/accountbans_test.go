package accountbans_test

import (
	"testing"
	"time"

	"Coves/internal/core/accountbans"

	"github.com/stretchr/testify/assert"
)

func TestBanInForce(t *testing.T) {
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	at := func(instant time.Time) *time.Time { return &instant }
	for _, test := range []struct {
		name string
		ban  *accountbans.Ban
		want bool
	}{
		{name: "no ban", ban: nil, want: false},
		{name: "permanent ban", ban: &accountbans.Ban{ActionID: "action-1"}, want: true},
		{name: "expires one millisecond after now", ban: &accountbans.Ban{
			ActionID:  "action-1",
			ExpiresAt: at(time.Date(2026, time.October, 22, 12, 0, 0, 1_000_000, time.UTC)),
		}, want: true},
		{name: "expires exactly now", ban: &accountbans.Ban{
			ActionID:  "action-1",
			ExpiresAt: at(time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)),
		}, want: false},
		{name: "expired one millisecond before now", ban: &accountbans.Ban{
			ActionID:  "action-1",
			ExpiresAt: at(time.Date(2026, time.October, 22, 11, 59, 59, 999_000_000, time.UTC)),
		}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, test.ban.InForce(now))
		})
	}
}
