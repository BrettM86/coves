package postgres

import (
	"database/sql"
	"time"

	"Coves/internal/core/discover"
)

type postgresDiscoverRepo struct {
	*feedRepoBase
	discoverHotCandidateLimit               int
	discoverHotStoredCandidateLimit         int
	discoverHotStoredCheckpointLimit        int
	discoverHotCheckpointStateByteLimit     int
	discoverHotSnapshotBuildsPerMinuteLimit int
	discoverHotWorkDeadline                 time.Duration
}

// NewDiscoverRepository creates the PostgreSQL Discover Hot repository.
func NewDiscoverRepository(db *sql.DB, cursorSecret string, options ...AccountRestrictionOption) discover.Repository {
	return &postgresDiscoverRepo{
		feedRepoBase: newFeedRepoBase(db, cursorSecret, options),
	}
}
