package communitysignals

import "time"

// DigestSlot returns the latest Monday 09:00 UTC at or before now, in UTC.
func DigestSlot(now time.Time) time.Time {
	t := now.UTC()
	daysSinceMonday := (int(t.Weekday()) + 6) % 7
	slot := time.Date(t.Year(), t.Month(), t.Day()-daysSinceMonday, 9, 0, 0, 0, time.UTC)
	if slot.After(t) {
		slot = slot.AddDate(0, 0, -7)
	}
	return slot
}

// maxRetryDelay caps the retry backoff.
const maxRetryDelay = 6 * time.Hour

// retryDelay is the wait after the attempts-th failed send:
// base*2^(attempts-1), capped at maxRetryDelay.
func retryDelay(base time.Duration, attempts int) time.Duration {
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= maxRetryDelay/2 {
			return maxRetryDelay
		}
		delay *= 2
	}
	return min(delay, maxRetryDelay)
}
