package postgres

import "fmt"

// viewerBlockFilters renders post-level viewer preferences for a query whose
// viewer DID is bound at $viewerParam. Keeping the SQL here prevents Discover's
// cursor-relative parameter, the community feed's different cursor-relative
// parameter, and Timeline's fixed $1 from drifting apart; the planned
// cross-community search read must reuse it rather than create another copy.
//
// Author blocks apply to every post feed. Community blocks are aggregate-feed
// mutes, so aggregateSurface enables them for Discover and the timeline today
// (and cross-community search when it lands) while explicitSurface leaves a
// community's own requested feed unchanged.
//
// comment_repo.go's three author-block clauses intentionally do not use this
// helper: community blocks do not apply to comment queries, and those clauses
// compare c.commenter_did rather than p.author_did.
// feedSurface says what kind of read a query is, because the two block kinds
// apply to different surfaces. A named kind rather than a bool so the product
// decision is legible at every call site: `viewerBlockFilters(n, aggregateSurface)`
// says what `viewerBlockFilters(n, true)` hid.
type feedSurface int

const (
	// explicitSurface is a read that names one community — its own feed. The
	// viewer asked for this place, so community blocks do not apply; author
	// blocks still do.
	explicitSurface feedSurface = iota
	// aggregateSurface is a read across communities — Discover, the subscribed
	// timeline, and cross-community search when it lands. Community blocks
	// apply here: this is exactly what a community mute is for.
	aggregateSurface
)

func viewerBlockFilters(viewerParam int, surface feedSurface) string {
	filters := fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = p.author_did)", viewerParam)
	if surface == aggregateSurface {
		filters += fmt.Sprintf(" AND NOT EXISTS (SELECT 1 FROM community_blocks WHERE user_did = $%d AND community_did = p.community_did)", viewerParam)
	}
	return filters
}
