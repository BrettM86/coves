package discover

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSelectDiscoverHotCandidates(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	candidate := func(uri, community string, rank float64) DiscoverHotCandidate {
		return DiscoverHotCandidate{URI: uri, CommunityDID: community, BaseRank: rank, CreatedAt: createdAt}
	}
	uris := func(candidates []DiscoverHotCandidate) []string {
		result := make([]string, len(candidates))
		for i, candidate := range candidates {
			result[i] = candidate.URI
		}
		return result
	}

	t.Run("selects the highest adjusted community head", func(t *testing.T) {
		selected, _ := SelectDiscoverHotCandidates([]DiscoverHotCandidate{
			candidate("a2", "a", 9),
			candidate("b1", "b", 7),
			candidate("a1", "a", 10),
		}, 3, DiscoverHotSelectionState{})

		assert.Equal(t, []string{"a1", "b1", "a2"}, uris(selected))
	})

	t.Run("penalty counts prior emissions", func(t *testing.T) {
		candidates := []DiscoverHotCandidate{
			candidate("a1", "a", 9),
			candidate("b1", "b", 5),
		}

		selectedAfterOne, _ := SelectDiscoverHotCandidates(candidates, 1, DiscoverHotSelectionState{
			RecentCommunities: []string{"a"},
		})
		selectedAfterTwo, _ := SelectDiscoverHotCandidates(candidates, 1, DiscoverHotSelectionState{
			RecentCommunities: []string{"a", "a"},
		})

		assert.Equal(t, []string{"a1"}, uris(selectedAfterOne), "9 / (1 + 0.5) should beat 5")
		assert.Equal(t, []string{"b1"}, uris(selectedAfterTwo), "9 / (1 + 2*0.5) should lose to 5")
	})

	t.Run("negative ranks are penalized away from zero", func(t *testing.T) {
		selected, _ := SelectDiscoverHotCandidates([]DiscoverHotCandidate{
			candidate("repeated", "a", -0.10),
			candidate("alternative", "b", -0.15),
		}, 1, DiscoverHotSelectionState{RecentCommunities: []string{"a", "a"}})

		assert.Equal(t, []string{"alternative"}, uris(selected),
			"a repeated negative rank must be multiplied by its penalty, not divided")
	})

	t.Run("ties use creation time then URI descending", func(t *testing.T) {
		older := candidate("older", "a", 1)
		older.CreatedAt = createdAt.Add(-time.Minute)
		newer := candidate("newer", "b", 1)
		selectedByTime, _ := SelectDiscoverHotCandidates([]DiscoverHotCandidate{older, newer}, 1, DiscoverHotSelectionState{})

		lowURI := candidate("at://example/a", "a", 1)
		highURI := candidate("at://example/z", "b", 1)
		selectedByURI, _ := SelectDiscoverHotCandidates([]DiscoverHotCandidate{lowURI, highURI}, 1, DiscoverHotSelectionState{})

		assert.Equal(t, []string{"newer"}, uris(selectedByTime))
		assert.Equal(t, []string{"at://example/z"}, uris(selectedByURI))
	})

	t.Run("one community exhausts normally", func(t *testing.T) {
		candidates := []DiscoverHotCandidate{
			candidate("a-low", "a", 1),
			candidate("a-high", "a", 3),
			candidate("a-middle", "a", 2),
		}
		selected, state := SelectDiscoverHotCandidates(candidates, 10, DiscoverHotSelectionState{})
		afterExhaustion, _ := SelectDiscoverHotCandidates(candidates, 10, state)

		assert.Equal(t, []string{"a-high", "a-middle", "a-low"}, uris(selected))
		assert.Empty(t, afterExhaustion)
	})

	t.Run("the oldest community expires from the twenty-post window", func(t *testing.T) {
		recent := make([]string, 20)
		recent[0] = "a"
		for i := 1; i < len(recent); i++ {
			recent[i] = "other"
		}
		initial := DiscoverHotSelectionState{RecentCommunities: recent}
		competitors := []DiscoverHotCandidate{
			candidate("a1", "a", 9),
			candidate("b1", "b", 6.5),
		}

		beforeExpiry, _ := SelectDiscoverHotCandidates(competitors, 1, initial)
		_, advanced := SelectDiscoverHotCandidates([]DiscoverHotCandidate{candidate("c1", "c", 100)}, 1, initial)
		afterExpiry, _ := SelectDiscoverHotCandidates(competitors, 1, advanced)

		assert.Equal(t, []string{"b1"}, uris(beforeExpiry))
		assert.Equal(t, []string{"a1"}, uris(afterExpiry))
	})

	t.Run("continuation matches one request without losing deferred candidates", func(t *testing.T) {
		candidates := []DiscoverHotCandidate{
			candidate("a3", "a", 8),
			candidate("b2", "b", 6.3),
			candidate("a1", "a", 10),
			candidate("c1", "c", 5),
			candidate("b1", "b", 7),
			candidate("a2", "a", 9),
		}
		expected := []string{"a1", "b1", "a2", "c1", "b2", "a3"}

		allAtOnce, _ := SelectDiscoverHotCandidates(candidates, len(candidates), DiscoverHotSelectionState{})
		state := DiscoverHotSelectionState{}
		var paged []DiscoverHotCandidate
		for _, limit := range []int{2, 2, len(candidates)} {
			page, next := SelectDiscoverHotCandidates(candidates, limit, state)
			paged = append(paged, page...)
			state = next
		}

		assert.Equal(t, expected, uris(allAtOnce))
		assert.Equal(t, uris(allAtOnce), uris(paged), "2+2+N selection should resume the same sequence")
		unique := make(map[string]struct{}, len(paged))
		for _, selected := range paged {
			unique[selected.URI] = struct{}{}
		}
		assert.Len(t, unique, len(candidates), "every deferred candidate should be returned exactly once")
	})
}
