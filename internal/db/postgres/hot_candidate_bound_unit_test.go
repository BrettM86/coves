package postgres

import "testing"

// The defaults are written out rather than read from a constant so that a
// change to the bound has to change this test too.
func TestHotCandidateBound_ZeroValuedConfigurationDefaultsToFiveThousand(t *testing.T) {
	t.Parallel()

	if got := (&postgresDiscoverRepo{}).effectiveDiscoverHotCandidateLimit(); got != 5000 {
		t.Errorf("Discover default candidate limit = %d, want 5000", got)
	}
	if got := (&feedRepoBase{}).effectiveHotCandidateLimit(); got != 5000 {
		t.Errorf("feed default candidate limit = %d, want 5000", got)
	}
}
