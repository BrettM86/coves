package discover

import (
	"math"
	"sort"
	"time"
)

const (
	minimumReferencePosts = 5
	baselinePriorPosts    = 20
	minimumAdjustment     = 0.5
	recentCommunityWindow = 20
	repetitionPenaltyStep = 0.5
)

func communityReference(scoresByCommunity map[string][]int) (float64, bool) {
	means := make([]float64, 0, len(scoresByCommunity))
	for _, scores := range scoresByCommunity {
		if len(scores) < minimumReferencePosts {
			continue
		}

		means = append(means, engagementSum(scores)/float64(len(scores)))
	}
	if len(means) == 0 {
		return 0, false
	}

	sort.Float64s(means)
	middle := len(means) / 2
	if len(means)%2 == 1 {
		return means[middle], true
	}
	return (means[middle-1] + means[middle]) / 2, true
}

// CommunityReference returns the community-balanced historical reference.
func CommunityReference(scoresByCommunity map[string][]int) (float64, bool) {
	return communityReference(scoresByCommunity)
}

func communityAdjustment(scores []int, reference float64, referenceAvailable bool) float64 {
	if !referenceAvailable || len(scores) == 0 || math.IsNaN(reference) || math.IsInf(reference, 0) {
		return 1
	}

	sum := engagementSum(scores)
	if sum == 0 {
		return 1
	}

	baseline := (sum + baselinePriorPosts*reference) / (float64(len(scores)) + baselinePriorPosts)
	adjustment := math.Sqrt(math.Max(reference, 1) / math.Max(baseline, 1))
	return min(max(adjustment, minimumAdjustment), 1)
}

// CommunityAdjustment returns the smoothed one-sided normalization multiplier.
func CommunityAdjustment(scores []int, reference float64, referenceAvailable bool) float64 {
	return communityAdjustment(scores, reference, referenceAvailable)
}

func discoverHotRank(score int, createdAt, rankingTime time.Time, communityAdjustment float64) float64 {
	ageHours := rankingTime.Sub(createdAt).Hours()
	if ageHours < 0 {
		ageHours = 0
	}

	numerator := 1.0
	switch {
	case score > 0:
		if math.IsNaN(communityAdjustment) || math.IsInf(communityAdjustment, 0) {
			communityAdjustment = 1
		}
		communityAdjustment = min(max(communityAdjustment, minimumAdjustment), 1)
		numerator += communityAdjustment * math.Log1p(float64(score))
	case score < 0:
		numerator -= math.Log1p(math.Abs(float64(score)))
	}

	return numerator / math.Pow(ageHours+2, 1.5)
}

// DiscoverHotRank returns the normalized, freshness-decayed base rank.
func DiscoverHotRank(score int, createdAt, rankingTime time.Time, communityAdjustment float64) float64 {
	return discoverHotRank(score, createdAt, rankingTime, communityAdjustment)
}

func engagementSum(scores []int) float64 {
	var sum float64
	for _, score := range scores {
		if score > 0 {
			sum += math.Log1p(float64(score))
		}
	}
	return sum
}

type DiscoverHotCandidate struct {
	URI          string
	CommunityDID string
	BaseRank     float64
	CreatedAt    time.Time
	Excluded     bool
}

type DiscoverHotSelectionState struct {
	CommunityPositions map[string]int
	RecentCommunities  []string
}

func SelectDiscoverHotCandidates(candidates []DiscoverHotCandidate, limit int, state DiscoverHotSelectionState) ([]DiscoverHotCandidate, DiscoverHotSelectionState) {
	streams := make(map[string][]DiscoverHotCandidate)
	for _, candidate := range candidates {
		if math.IsNaN(candidate.BaseRank) || math.IsInf(candidate.BaseRank, 0) {
			continue
		}
		streams[candidate.CommunityDID] = append(streams[candidate.CommunityDID], candidate)
	}
	for community, stream := range streams {
		sort.Slice(stream, func(i, j int) bool {
			return candidateRanksBefore(stream[i], stream[j])
		})
		streams[community] = stream
	}

	positions := make(map[string]int, len(state.CommunityPositions))
	for community, position := range state.CommunityPositions {
		positions[community] = max(position, 0)
	}
	recent := append([]string(nil), state.RecentCommunities...)
	if len(recent) > recentCommunityWindow {
		recent = recent[len(recent)-recentCommunityWindow:]
	}
	nextState := DiscoverHotSelectionState{
		CommunityPositions: positions,
		RecentCommunities:  recent,
	}
	advanceExcluded := func() {
		for community, stream := range streams {
			for positions[community] < len(stream) && stream[positions[community]].Excluded {
				positions[community]++
			}
		}
	}
	advanceExcluded()
	if limit <= 0 {
		return nil, nextState
	}

	recentCounts := make(map[string]int, len(recent))
	for _, community := range recent {
		recentCounts[community]++
	}

	selected := make([]DiscoverHotCandidate, 0, min(limit, len(candidates)))
	for len(selected) < limit {
		var best DiscoverHotCandidate
		var bestCommunity string
		var bestAdjustedRank float64
		found := false

		for community, stream := range streams {
			position := positions[community]
			if position >= len(stream) {
				continue
			}

			candidate := stream[position]
			adjustedRank := penalizedRank(candidate.BaseRank, recentCounts[community])
			if !found || adjustedRank > bestAdjustedRank ||
				(adjustedRank == bestAdjustedRank && candidateTiesBefore(candidate, best)) {
				best = candidate
				bestCommunity = community
				bestAdjustedRank = adjustedRank
				found = true
			}
		}
		if !found {
			break
		}

		selected = append(selected, best)
		positions[bestCommunity]++
		if len(recent) == recentCommunityWindow {
			expired := recent[0]
			recent = recent[1:]
			recentCounts[expired]--
		}
		recent = append(recent, bestCommunity)
		recentCounts[bestCommunity]++
		advanceExcluded()
	}

	nextState.RecentCommunities = recent
	return selected, nextState
}

func candidateRanksBefore(left, right DiscoverHotCandidate) bool {
	if left.BaseRank != right.BaseRank {
		return left.BaseRank > right.BaseRank
	}
	return candidateTiesBefore(left, right)
}

func candidateTiesBefore(left, right DiscoverHotCandidate) bool {
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.After(right.CreatedAt)
	}
	if left.URI != right.URI {
		return left.URI > right.URI
	}
	return left.CommunityDID > right.CommunityDID
}

func penalizedRank(baseRank float64, priorEmissions int) float64 {
	penalty := 1 + repetitionPenaltyStep*float64(priorEmissions)
	if baseRank >= 0 {
		return baseRank / penalty
	}

	adjusted := baseRank * penalty
	if math.IsInf(adjusted, -1) {
		return -math.MaxFloat64
	}
	return adjusted
}
