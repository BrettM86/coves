package votes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"Coves/internal/atproto/pds"
)

// CachedVote represents a vote stored in the cache
type CachedVote struct {
	Direction string // "up" or "down"
	URI       string // vote record URI (at://did/collection/rkey)
	RKey      string // record key
}

// VoteCache provides an in-memory cache of user votes fetched from their PDS.
// This avoids eventual consistency issues with the AppView database.
type VoteCache struct {
	mu     sync.RWMutex
	votes  map[string]map[string]*CachedVote // userDID -> subjectURI -> vote
	expiry map[string]time.Time              // userDID -> expiry time
	ttl    time.Duration
	logger *slog.Logger
}

// NewVoteCache creates a new vote cache with the specified TTL
func NewVoteCache(ttl time.Duration, logger *slog.Logger) *VoteCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &VoteCache{
		votes:  make(map[string]map[string]*CachedVote),
		expiry: make(map[string]time.Time),
		ttl:    ttl,
		logger: logger,
	}
}

// GetVotesForUser returns all cached votes for a user.
// Returns nil if cache is empty or expired for this user.
func (c *VoteCache) GetVotesForUser(userDID string) map[string]*CachedVote {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isCachedLocked(userDID) {
		return nil
	}

	result := make(map[string]*CachedVote, len(c.votes[userDID]))
	for subject, vote := range c.votes[userDID] {
		snapshot := *vote
		result[subject] = &snapshot
	}
	return result
}

// getVotesForSubjects takes a consistent snapshot under the cache lock.
// Nil means unavailable; a non-nil empty map confirms no votes for these subjects.
func (c *VoteCache) getVotesForSubjects(userDID string, subjectURIs []string) map[string]*CachedVote {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isCachedLocked(userDID) {
		return nil
	}

	result := make(map[string]*CachedVote)
	for _, uri := range subjectURIs {
		if vote := c.votes[userDID][uri]; vote != nil {
			snapshot := *vote
			result[uri] = &snapshot
		}
	}
	return result
}

// GetVote returns the cached vote for a specific subject, or nil if not found/expired
func (c *VoteCache) GetVote(userDID, subjectURI string) *CachedVote {
	return c.getVotesForSubjects(userDID, []string{subjectURI})[subjectURI]
}

// IsCached returns true if the user's votes are cached and not expired
func (c *VoteCache) IsCached(userDID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.isCachedLocked(userDID)
}

// isCachedLocked requires the write lock. Only full population establishes a
// deadline; any access after that deadline discards the stale snapshot.
func (c *VoteCache) isCachedLocked(userDID string) bool {
	expiry, exists := c.expiry[userDID]
	if exists && time.Now().Before(expiry) {
		return true
	}
	delete(c.votes, userDID)
	delete(c.expiry, userDID)
	return false
}

// SetVotesForUser replaces all cached votes for a user
func (c *VoteCache) SetVotesForUser(userDID string, votes map[string]*CachedVote) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.votes[userDID] = votes
	c.expiry[userDID] = time.Now().Add(c.ttl)

	c.logger.Debug("vote cache updated",
		"user", userDID,
		"vote_count", len(votes),
		"expires_at", c.expiry[userDID])
}

// SetVote updates a complete, unexpired cache. A single write cannot populate it.
func (c *VoteCache) SetVote(userDID, subjectURI string, vote *CachedVote) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isCachedLocked(userDID) {
		return
	}
	if c.votes[userDID] == nil {
		c.votes[userDID] = make(map[string]*CachedVote)
	}
	c.votes[userDID][subjectURI] = vote
	c.expiry[userDID] = time.Now().Add(c.ttl)

	c.logger.Debug("vote cached",
		"user", userDID,
		"subject", subjectURI,
		"direction", vote.Direction)
}

// RemoveVote removes a vote from the cache (for toggle-off)
func (c *VoteCache) RemoveVote(userDID, subjectURI string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isCachedLocked(userDID) {
		return
	}
	delete(c.votes[userDID], subjectURI)
	c.expiry[userDID] = time.Now().Add(c.ttl)

	c.logger.Debug("vote removed from cache",
		"user", userDID,
		"subject", subjectURI)
}

// Invalidate removes all cached votes for a user
func (c *VoteCache) Invalidate(userDID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.votes, userDID)
	delete(c.expiry, userDID)

	c.logger.Debug("vote cache invalidated", "user", userDID)
}

// FetchAndCacheFromPDS fetches all votes from the user's PDS and caches them.
// This should be called on first authenticated request or when cache is expired.
func (c *VoteCache) FetchAndCacheFromPDS(ctx context.Context, pdsClient pds.Client) error {
	userDID := pdsClient.DID()

	c.logger.Debug("fetching votes from PDS",
		"user", userDID,
		"pds", pdsClient.HostURL())

	votes, err := c.fetchAllVotesFromPDS(ctx, pdsClient)
	if err != nil {
		return fmt.Errorf("failed to fetch votes from PDS: %w", err)
	}

	c.SetVotesForUser(userDID, votes)

	c.logger.Info("vote cache populated from PDS",
		"user", userDID,
		"vote_count", len(votes))

	return nil
}

// fetchAllVotesFromPDS paginates through all vote records on the user's PDS
func (c *VoteCache) fetchAllVotesFromPDS(ctx context.Context, pdsClient pds.Client) (map[string]*CachedVote, error) {
	votes := make(map[string]*CachedVote)
	cursor := ""
	const pageSize = 100
	const collection = "social.coves.feed.vote"

	for {
		result, err := pdsClient.ListRecords(ctx, collection, pageSize, cursor)
		if err != nil {
			if pds.IsAuthError(err) {
				return nil, fmt.Errorf("%w: %w", ErrNotAuthorized, err)
			}
			return nil, fmt.Errorf("listRecords failed: %w", err)
		}

		for _, rec := range result.Records {
			// Extract subject from record value
			subject, ok := rec.Value["subject"].(map[string]any)
			if !ok {
				continue
			}

			subjectURI, ok := subject["uri"].(string)
			if !ok || subjectURI == "" {
				continue
			}

			direction, _ := rec.Value["direction"].(string)
			if direction == "" {
				continue
			}

			// Extract rkey from URI
			rkey := extractRKeyFromURI(rec.URI)

			votes[subjectURI] = &CachedVote{
				Direction: direction,
				URI:       rec.URI,
				RKey:      rkey,
			}
		}

		if result.Cursor == "" {
			break
		}
		cursor = result.Cursor
	}

	return votes, nil
}

// extractRKeyFromURI extracts the rkey from an AT-URI (at://did/collection/rkey)
func extractRKeyFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) >= 5 {
		return parts[len(parts)-1]
	}
	return ""
}
