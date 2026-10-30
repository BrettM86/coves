package votes

import "time"

// SetNowForTest replaces the service's clock, which decides when overlay
// entries expire.
func SetNowForTest(s Service, now func() time.Time) {
	s.(*voteService).now = now
}

// OverlaySizeForTest reports how many entries the recent-write overlay holds.
func OverlaySizeForTest(s Service) int {
	return s.(*voteService).overlay.size()
}
