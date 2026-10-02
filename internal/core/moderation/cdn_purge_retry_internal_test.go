package moderation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCDNPurgeRetryDelay(t *testing.T) {
	for _, test := range []struct {
		failedAttempts int
		want           time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour},
		{8, time.Hour},
		{1000, time.Hour},
	} {
		assert.Equal(t, test.want, cdnPurgeRetryDelay(test.failedAttempts), "failed attempts %d", test.failedAttempts)
	}
}
