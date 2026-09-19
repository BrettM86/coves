package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"Coves/internal/core/discover"

	"github.com/lib/pq"
)

const (
	discoverHotAlgorithmVersion                    = 2
	discoverHotCursorVersion                       = "discover-hot-v1"
	discoverHotCursorMaxLength                     = 500
	discoverHotSnapshotReuse                       = 30 * time.Second
	discoverHotSnapshotLifetime                    = time.Hour
	discoverHotDefaultCandidateLimit               = 100_000
	discoverHotDefaultStoredCandidateLimit         = 10_000_000
	discoverHotDefaultStoredCheckpointLimit        = 20_000
	discoverHotDefaultCheckpointStateByteLimit     = 64 * 1024
	discoverHotDefaultSnapshotBuildsPerMinuteLimit = 20
	discoverHotDefaultWorkDeadline                 = 5 * time.Second
	discoverHotCheckpointStateSizeConstraint       = "ck_discover_hot_checkpoint_state_size"
)

func (r *postgresDiscoverRepo) getDiscoverHot(ctx context.Context, req discover.GetDiscoverRequest) (posts []*discover.FeedViewPost, nextCursor *string, err error) {
	callerContext := ctx
	ctx, cancel := context.WithTimeout(ctx, r.effectiveDiscoverHotWorkDeadline())
	defer func() {
		workErr := ctx.Err()
		cancel()
		if err == nil {
			return
		}
		posts = nil
		nextCursor = nil
		if callerErr := callerContext.Err(); callerErr != nil {
			err = callerErr
			if errors.Is(callerErr, context.DeadlineExceeded) {
				err = errors.Join(callerErr, discover.ErrDiscoverUnavailable)
			}
			return
		}
		if errors.Is(workErr, context.DeadlineExceeded) {
			err = fmt.Errorf("Discover Hot work deadline exceeded: %w", discover.ErrDiscoverUnavailable)
		}
	}()

	var (
		snapshotID   int64
		candidates   []discover.DiscoverHotCandidate
		state        discover.DiscoverHotSelectionState
		checkpointID int64
	)

	if req.Cursor != nil && *req.Cursor != "" {
		parsedSnapshotID, parsedCheckpointID, err := r.parseDiscoverHotCursor(*req.Cursor, req.ViewerDID)
		if err != nil {
			return nil, nil, discover.ErrInvalidCursor
		}
		snapshotID = parsedSnapshotID
		checkpointID = parsedCheckpointID
	} else {
		var err error
		snapshotID, candidates, err = r.loadOrCreateDiscoverHotSnapshot(ctx, req.ViewerDID)
		if err != nil {
			return nil, nil, err
		}
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, fmt.Errorf("begin Discover Hot page: %w", err)
	}
	defer tx.Rollback()

	if checkpointID != 0 {
		state, err = loadDiscoverHotCheckpoint(ctx, tx, snapshotID, checkpointID, req.ViewerDID)
		if err != nil {
			if errors.Is(err, discover.ErrInvalidCursor) {
				return nil, nil, discover.ErrInvalidCursor
			}
			return nil, nil, fmt.Errorf("load Discover Hot checkpoint: %w", err)
		}
		candidates, err = loadDiscoverHotCandidates(ctx, tx, snapshotID)
		if err != nil {
			return nil, nil, fmt.Errorf("load Discover Hot candidates: %w", err)
		}
	} else if err := pinDiscoverHotSnapshot(ctx, tx, snapshotID); err != nil {
		return nil, nil, fmt.Errorf("pin Discover Hot snapshot: %w", err)
	}

	if err := markExcludedDiscoverHotCandidates(ctx, tx, candidates, req.ViewerDID); err != nil {
		return nil, nil, fmt.Errorf("revalidate Discover Hot candidates: %w", err)
	}
	var (
		selected  []discover.DiscoverHotCandidate
		nextState discover.DiscoverHotSelectionState
		feedPosts []*discover.FeedViewPost
	)
	for {
		selected, nextState = discover.SelectDiscoverHotCandidates(candidates, req.Limit, state)
		feedPosts, err = hydrateDiscoverHotCandidates(ctx, tx, selected, req.ViewerDID)
		if err != nil {
			return nil, nil, fmt.Errorf("hydrate Discover Hot candidates: %w", err)
		}
		if len(feedPosts) == len(selected) {
			break
		}

		hydrated := make(map[string]struct{}, len(feedPosts))
		for _, feedPost := range feedPosts {
			hydrated[feedPost.Post.URI] = struct{}{}
		}
		missing := make(map[string]struct{}, len(selected)-len(feedPosts))
		for _, candidate := range selected {
			if _, ok := hydrated[candidate.URI]; !ok {
				missing[candidate.URI] = struct{}{}
			}
		}
		for i := range candidates {
			if _, ok := missing[candidates[i].URI]; ok {
				candidates[i].Excluded = true
			}
		}
	}

	lookahead, _ := discover.SelectDiscoverHotCandidates(candidates, 1, nextState)
	if len(lookahead) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit Discover Hot page: %w", err)
		}
		return feedPosts, nil, nil
	}

	checkpointID, err = r.storeDiscoverHotCheckpoint(ctx, tx, snapshotID, nextState)
	if err != nil {
		return nil, nil, fmt.Errorf("store Discover Hot checkpoint: %w", err)
	}
	cursor, err := r.buildDiscoverHotCursor(snapshotID, checkpointID, req.ViewerDID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit Discover Hot page: %w", err)
	}
	return feedPosts, &cursor, nil
}

func (r *postgresDiscoverRepo) loadOrCreateDiscoverHotSnapshot(ctx context.Context, viewerDID string) (int64, []discover.DiscoverHotCandidate, error) {
	scope := discoverHotViewerScope(viewerDID)
	snapshotID, err := findReusableDiscoverHotSnapshot(ctx, r.db, scope)
	if err == nil {
		candidates, err := loadDiscoverHotCandidates(ctx, r.db, snapshotID)
		return snapshotID, candidates, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("find reusable Discover Hot snapshot: %w", err)
	}

	return r.createDiscoverHotSnapshot(ctx, viewerDID, scope)
}

type discoverHotQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func findReusableDiscoverHotSnapshot(ctx context.Context, querier discoverHotQuerier, scope string) (int64, error) {
	var snapshotID int64
	err := querier.QueryRowContext(ctx, `
		SELECT id
		FROM discover_hot_snapshots
		WHERE algorithm_version = $1
			AND viewer_scope = $2
			AND created_at >= NOW() - $3::interval
			AND expires_at > NOW()
		ORDER BY created_at DESC
		LIMIT 1
	`, discoverHotAlgorithmVersion, scope, discoverHotSnapshotReuse.String()).Scan(&snapshotID)
	return snapshotID, err
}

func (r *postgresDiscoverRepo) createDiscoverHotSnapshot(ctx context.Context, viewerDID, scope string) (snapshotID int64, candidates []discover.DiscoverHotCandidate, err error) {
	connection, err := r.db.Conn(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("open Discover Hot snapshot connection: %w", err)
	}
	defer connection.Close()

	lockKey := discoverHotSnapshotLockKey(scope)
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		discardDiscoverHotConnection(connection)
		return 0, nil, fmt.Errorf("wait for Discover Hot snapshot lock: %w", err)
	}
	defer func() {
		if unlockErr := releaseDiscoverHotAdvisoryLock(connection, lockKey); unlockErr != nil {
			snapshotID = 0
			candidates = nil
			err = errors.Join(err, fmt.Errorf("release Discover Hot snapshot lock: %w", unlockErr))
		}
	}()

	snapshotID, err = findReusableDiscoverHotSnapshot(ctx, connection, scope)
	if err == nil {
		candidates, err = loadDiscoverHotCandidates(ctx, connection, snapshotID)
		if err != nil {
			return 0, nil, fmt.Errorf("load reusable Discover Hot candidates: %w", err)
		}
		return snapshotID, candidates, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("recheck reusable Discover Hot snapshot: %w", err)
	}

	globalLockKey := discoverHotSnapshotAdmissionLockKey()
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, globalLockKey); err != nil {
		discardDiscoverHotConnection(connection)
		return 0, nil, fmt.Errorf("wait for Discover Hot snapshot admission lock: %w", err)
	}
	defer func() {
		if unlockErr := releaseDiscoverHotAdvisoryLock(connection, globalLockKey); unlockErr != nil {
			snapshotID = 0
			candidates = nil
			err = errors.Join(err, fmt.Errorf("release Discover Hot snapshot admission lock: %w", unlockErr))
		}
	}()

	if err := r.recordDiscoverHotBuildAttempt(ctx, connection); err != nil {
		return 0, nil, err
	}

	tx, err := connection.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, nil, fmt.Errorf("begin Discover Hot snapshot: %w", err)
	}
	defer tx.Rollback()

	var rankingTime time.Time
	if err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&rankingTime); err != nil {
		return 0, nil, fmt.Errorf("capture Discover Hot ranking time: %w", err)
	}

	history, err := queryDiscoverHotHistory(ctx, tx, rankingTime)
	if err != nil {
		return 0, nil, err
	}
	reference, referenceAvailable := discover.CommunityReference(history)

	rawCandidates, err := queryDiscoverHotCandidates(ctx, tx, viewerDID, r.effectiveDiscoverHotCandidateLimit())
	if err != nil {
		return 0, nil, err
	}
	candidates = make([]discover.DiscoverHotCandidate, 0, len(rawCandidates))
	adjustments := make(map[string]float64)
	for _, candidate := range rawCandidates {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		adjustment, ok := adjustments[candidate.CommunityDID]
		if !ok {
			adjustment = discover.CommunityAdjustment(history[candidate.CommunityDID], reference, referenceAvailable)
			adjustments[candidate.CommunityDID] = adjustment
		}
		var hostedBonus float64
		if candidate.Hosted {
			hostedBonus = discover.HostedCommunityBonus(r.cursorSecret, candidate.URI)
		}
		candidate.BaseRank = discover.DiscoverHotRank(candidate.Score, candidate.CreatedAt, rankingTime, adjustment, hostedBonus)
		if math.IsNaN(candidate.BaseRank) || math.IsInf(candidate.BaseRank, 0) {
			return 0, nil, fmt.Errorf("nonfinite Discover Hot rank for %s", candidate.URI)
		}
		candidates = append(candidates, candidate.DiscoverHotCandidate)
	}

	var storedCandidates int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM discover_hot_candidates`).Scan(&storedCandidates); err != nil {
		return 0, nil, fmt.Errorf("count stored Discover Hot candidates: %w", err)
	}
	storedCandidateLimit := int64(r.effectiveDiscoverHotStoredCandidateLimit())
	additionalCandidates := int64(len(candidates))
	if storedCandidates > storedCandidateLimit || additionalCandidates > storedCandidateLimit-storedCandidates {
		return 0, nil, fmt.Errorf("Discover Hot stored candidate limit exceeded: %w", discover.ErrDiscoverUnavailable)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES ($1, $2, $3, $4)
		RETURNING id
	`, discoverHotAlgorithmVersion, scope, rankingTime, rankingTime.Add(discoverHotSnapshotLifetime)).Scan(&snapshotID)
	if err != nil {
		return 0, nil, fmt.Errorf("store Discover Hot snapshot: %w", err)
	}

	if err = copyDiscoverHotCandidates(ctx, tx, snapshotID, candidates); err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit Discover Hot snapshot: %w", err)
	}
	return snapshotID, candidates, nil
}

func releaseDiscoverHotAdvisoryLock(connection *sql.Conn, lockKey int64) error {
	unlockContext, cancel := context.WithTimeout(context.Background(), discoverHotDefaultWorkDeadline)
	defer cancel()

	var released bool
	err := connection.QueryRowContext(unlockContext, `SELECT pg_advisory_unlock($1)`, lockKey).Scan(&released)
	if err == nil && released {
		return nil
	}
	discardDiscoverHotConnection(connection)
	if err != nil {
		return err
	}
	return errors.New("Discover Hot advisory lock was not held")
}

func discardDiscoverHotConnection(connection *sql.Conn) {
	_ = connection.Raw(func(any) error { return driver.ErrBadConn })
}

func (r *postgresDiscoverRepo) recordDiscoverHotBuildAttempt(ctx context.Context, connection *sql.Conn) error {
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Discover Hot snapshot admission: %w", err)
	}
	defer tx.Rollback()

	var recentBuilds int64
	if err = tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM discover_hot_build_attempts
		WHERE created_at > NOW() - INTERVAL '1 minute'
	`).Scan(&recentBuilds); err != nil {
		return fmt.Errorf("count recent Discover Hot snapshot builds: %w", err)
	}
	if recentBuilds >= int64(r.effectiveDiscoverHotSnapshotBuildsPerMinuteLimit()) {
		return fmt.Errorf("Discover Hot snapshot build rate limit exceeded: %w", discover.ErrDiscoverUnavailable)
	}

	if _, err = tx.ExecContext(ctx, `INSERT INTO discover_hot_build_attempts DEFAULT VALUES`); err != nil {
		return fmt.Errorf("record Discover Hot snapshot build attempt: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit Discover Hot snapshot admission: %w", err)
	}
	return nil
}

type discoverHotRawCandidate struct {
	discover.DiscoverHotCandidate
	Score int
	// Hosted is the presence of a stored PDS refresh token for the post's
	// community, never its contents: the credential is not decrypted here.
	Hosted bool
}

func queryDiscoverHotHistory(ctx context.Context, tx *sql.Tx, rankingTime time.Time) (map[string][]int, error) {
	visJoin, visWhere := visiblePostsPredicate(anonymousViewerSQL)
	rows, err := tx.QueryContext(ctx, `
		SELECT p.community_did, p.score
		FROM posts p`+visJoin+`
		WHERE p.deleted_at IS NULL
			AND `+visWhere+`
			AND p.created_at >= $1::timestamptz - INTERVAL '14 days'
			AND p.created_at < $1::timestamptz - INTERVAL '24 hours'
	`, rankingTime)
	if err != nil {
		return nil, fmt.Errorf("query Discover Hot history: %w", err)
	}
	defer rows.Close()

	history := make(map[string][]int)
	for rows.Next() {
		var communityDID string
		var score int
		if err := rows.Scan(&communityDID, &score); err != nil {
			return nil, fmt.Errorf("scan Discover Hot history: %w", err)
		}
		history[communityDID] = append(history[communityDID], score)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Discover Hot history: %w", err)
	}
	return history, nil
}

func queryDiscoverHotCandidates(ctx context.Context, tx *sql.Tx, viewerDID string, candidateLimit int) ([]discoverHotRawCandidate, error) {
	visJoin, visWhere := visiblePostsJoin(1)
	viewerFilter := ""
	if viewerDID != "" {
		viewerFilter = viewerBlockFilters(1, aggregateSurface)
	}
	queryLimit := int64(candidateLimit)
	if queryLimit < math.MaxInt64 {
		queryLimit++
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT p.uri, p.community_did, p.score, p.created_at,
			c.pds_refresh_token_encrypted IS NOT NULL AS hosted
		FROM posts p`+visJoin+`
			-- communities.did is UNIQUE, so this join cannot multiply candidate
			-- rows: the candidate limit and the atomic overflow refusal both
			-- depend on one row per post. A missing community reads as not hosted.
			LEFT JOIN communities c ON c.did = p.community_did
		WHERE p.deleted_at IS NULL
			AND `+visWhere+`
			`+viewerFilter+`
		LIMIT $2
	`, viewerDID, queryLimit)
	if err != nil {
		return nil, fmt.Errorf("query Discover Hot candidates: %w", err)
	}
	defer rows.Close()

	var candidates []discoverHotRawCandidate
	for rows.Next() {
		var candidate discoverHotRawCandidate
		if err := rows.Scan(&candidate.URI, &candidate.CommunityDID, &candidate.Score, &candidate.CreatedAt, &candidate.Hosted); err != nil {
			return nil, fmt.Errorf("scan Discover Hot candidate: %w", err)
		}
		if len(candidates) == candidateLimit {
			return nil, fmt.Errorf("Discover Hot candidate limit %d exceeded: %w", candidateLimit, discover.ErrDiscoverUnavailable)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Discover Hot candidates: %w", err)
	}
	return candidates, nil
}

func copyDiscoverHotCandidates(ctx context.Context, tx *sql.Tx, snapshotID int64, candidates []discover.DiscoverHotCandidate) error {
	statement, err := tx.PrepareContext(ctx, pq.CopyIn(
		"discover_hot_candidates", "snapshot_id", "uri", "community_did", "base_rank", "created_at",
	))
	if err != nil {
		return fmt.Errorf("prepare Discover Hot candidates: %w", err)
	}
	defer statement.Close()

	for _, candidate := range candidates {
		if _, err := statement.ExecContext(ctx, snapshotID, candidate.URI, candidate.CommunityDID, candidate.BaseRank, candidate.CreatedAt); err != nil {
			return fmt.Errorf("copy Discover Hot candidate: %w", err)
		}
	}
	if _, err := statement.ExecContext(ctx); err != nil {
		return fmt.Errorf("finish Discover Hot candidates: %w", err)
	}
	return nil
}

func (r *postgresDiscoverRepo) loadDiscoverHotCandidates(ctx context.Context, snapshotID int64) ([]discover.DiscoverHotCandidate, error) {
	return loadDiscoverHotCandidates(ctx, r.db, snapshotID)
}

func loadDiscoverHotCandidates(ctx context.Context, querier discoverHotQuerier, snapshotID int64) ([]discover.DiscoverHotCandidate, error) {
	rows, err := querier.QueryContext(ctx, `
		SELECT uri, community_did, base_rank, created_at
		FROM discover_hot_candidates
		WHERE snapshot_id = $1
	`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []discover.DiscoverHotCandidate
	for rows.Next() {
		var candidate discover.DiscoverHotCandidate
		if err := rows.Scan(&candidate.URI, &candidate.CommunityDID, &candidate.BaseRank, &candidate.CreatedAt); err != nil {
			return nil, err
		}
		if math.IsNaN(candidate.BaseRank) || math.IsInf(candidate.BaseRank, 0) {
			return nil, fmt.Errorf("snapshot %d contains nonfinite rank", snapshotID)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (r *postgresDiscoverRepo) storeDiscoverHotCheckpoint(ctx context.Context, tx *sql.Tx, snapshotID int64, state discover.DiscoverHotSelectionState) (int64, error) {
	positions, err := json.Marshal(state.CommunityPositions)
	if err != nil {
		return 0, err
	}
	recent := state.RecentCommunities
	if recent == nil {
		recent = []string{}
	}
	recentJSON, err := json.Marshal(recent)
	if err != nil {
		return 0, err
	}
	var stateBytes int
	if err := tx.QueryRowContext(ctx, `
		SELECT octet_length($1::jsonb::text)
			+ octet_length(array_to_json($2::text[])::text)
	`, positions, pq.Array(recent)).Scan(&stateBytes); err != nil {
		return 0, fmt.Errorf("measure Discover Hot checkpoint state: %w", err)
	}
	if stateBytes > r.effectiveDiscoverHotCheckpointStateByteLimit() {
		return 0, fmt.Errorf("Discover Hot checkpoint state is %d bytes: %w", stateBytes, discover.ErrDiscoverUnavailable)
	}
	digestInput := make([]byte, 8, 8+len(positions)+len(recentJSON))
	binary.BigEndian.PutUint64(digestInput, uint64(len(positions)))
	digestInput = append(digestInput, positions...)
	digestInput = append(digestInput, recentJSON...)
	stateDigest := sha256.Sum256(digestInput)

	checkpointID, err := findDiscoverHotCheckpoint(ctx, tx, snapshotID, stateDigest[:], positions, recent)
	if err == nil {
		return checkpointID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, discoverHotCheckpointAdmissionLockKey()); err != nil {
		return 0, fmt.Errorf("wait for Discover Hot checkpoint admission lock: %w", err)
	}

	checkpointID, err = findDiscoverHotCheckpoint(ctx, tx, snapshotID, stateDigest[:], positions, recent)
	if err == nil {
		return checkpointID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("recheck reusable Discover Hot checkpoint: %w", err)
	}

	var storedCheckpoints int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM discover_hot_checkpoints`).Scan(&storedCheckpoints); err != nil {
		return 0, fmt.Errorf("count stored Discover Hot checkpoints: %w", err)
	}
	if storedCheckpoints >= int64(r.effectiveDiscoverHotStoredCheckpointLimit()) {
		return 0, fmt.Errorf("Discover Hot stored checkpoint limit exceeded: %w", discover.ErrDiscoverUnavailable)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO discover_hot_checkpoints (snapshot_id, state_digest, community_positions, recent_communities)
		VALUES ($1, $2, $3::jsonb, $4)
		RETURNING id
	`, snapshotID, stateDigest[:], positions, pq.Array(recent)).Scan(&checkpointID)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Constraint == discoverHotCheckpointStateSizeConstraint {
			return 0, fmt.Errorf("Discover Hot checkpoint state exceeds storage limit: %w", discover.ErrDiscoverUnavailable)
		}
		return 0, fmt.Errorf("store Discover Hot checkpoint: %w", err)
	}
	return checkpointID, nil
}

func findDiscoverHotCheckpoint(ctx context.Context, querier discoverHotQuerier, snapshotID int64, stateDigest, positions []byte, recent []string) (int64, error) {
	var checkpointID int64
	err := querier.QueryRowContext(ctx, `
		SELECT id
		FROM discover_hot_checkpoints
		WHERE snapshot_id = $1
			AND state_digest = $2
			AND community_positions = $3::jsonb
			AND recent_communities = $4
	`, snapshotID, stateDigest, positions, pq.Array(recent)).Scan(&checkpointID)
	return checkpointID, err
}

func loadDiscoverHotCheckpoint(ctx context.Context, tx *sql.Tx, snapshotID, checkpointID int64, viewerDID string) (discover.DiscoverHotSelectionState, error) {
	var (
		positionsJSON []byte
		recent        pq.StringArray
	)
	err := tx.QueryRowContext(ctx, `
		SELECT community_positions, recent_communities
		FROM discover_hot_checkpoints checkpoint
		WHERE checkpoint.id = $1
			AND checkpoint.snapshot_id = $2
	`, checkpointID, snapshotID).Scan(&positionsJSON, &recent)
	if errors.Is(err, sql.ErrNoRows) {
		return discover.DiscoverHotSelectionState{}, discover.ErrInvalidCursor
	}
	if err != nil {
		return discover.DiscoverHotSelectionState{}, err
	}

	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT expires_at
		FROM discover_hot_snapshots
		WHERE id = $1
			AND algorithm_version = $2
			AND viewer_scope = $3
		FOR KEY SHARE
	`, snapshotID, discoverHotAlgorithmVersion, discoverHotViewerScope(viewerDID)).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return discover.DiscoverHotSelectionState{}, discover.ErrInvalidCursor
	}
	if err != nil {
		return discover.DiscoverHotSelectionState{}, err
	}

	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return discover.DiscoverHotSelectionState{}, err
	}
	if !expiresAt.After(databaseNow) {
		return discover.DiscoverHotSelectionState{}, discover.ErrInvalidCursor
	}

	positions := make(map[string]int)
	if err := json.Unmarshal(positionsJSON, &positions); err != nil {
		return discover.DiscoverHotSelectionState{}, err
	}
	return discover.DiscoverHotSelectionState{
		CommunityPositions: positions,
		RecentCommunities:  append([]string(nil), recent...),
	}, nil
}

func pinDiscoverHotSnapshot(ctx context.Context, tx *sql.Tx, snapshotID int64) error {
	var pinned int64
	return tx.QueryRowContext(ctx, `
		SELECT id
		FROM discover_hot_snapshots
		WHERE id = $1
		FOR KEY SHARE
	`, snapshotID).Scan(&pinned)
}

func markExcludedDiscoverHotCandidates(ctx context.Context, tx *sql.Tx, candidates []discover.DiscoverHotCandidate, viewerDID string) error {
	if len(candidates) == 0 {
		return nil
	}

	uris := make([]string, len(candidates))
	for i, candidate := range candidates {
		uris[i] = candidate.URI
	}
	visJoin, visWhere := visiblePostsJoin(2)
	viewerFilter := ""
	if viewerDID != "" {
		viewerFilter = viewerBlockFilters(2, aggregateSurface)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT p.uri
		FROM posts p
		INNER JOIN communities c ON p.community_did = c.did`+visJoin+`
		WHERE p.uri = ANY($1)
			AND p.deleted_at IS NULL
			AND `+visWhere+`
			`+viewerFilter+`
	`, pq.Array(uris), viewerDID)
	if err != nil {
		return err
	}
	defer rows.Close()

	eligible := make(map[string]struct{}, len(candidates))
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return err
		}
		eligible[uri] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range candidates {
		_, isEligible := eligible[candidates[i].URI]
		candidates[i].Excluded = !isEligible
	}
	return nil
}

func hydrateDiscoverHotCandidates(ctx context.Context, tx *sql.Tx, selected []discover.DiscoverHotCandidate, viewerDID string) ([]*discover.FeedViewPost, error) {
	if len(selected) == 0 {
		return nil, nil
	}

	uris := make([]string, len(selected))
	for i, candidate := range selected {
		uris[i] = candidate.URI
	}
	visJoin, visWhere := visiblePostsJoin(2)
	viewerFilter := ""
	if viewerDID != "" {
		viewerFilter = viewerBlockFilters(2, aggregateSurface)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT`+postViewSelectColumns+`
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did`+visJoin+`
		WHERE p.uri = ANY($1)
			AND p.deleted_at IS NULL
			AND `+visWhere+`
			`+viewerFilter+`
	`, pq.Array(uris), viewerDID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	views := make(map[string]*discover.FeedViewPost, len(selected))
	for rows.Next() {
		postView, err := scanPostView(rows)
		if err != nil {
			return nil, err
		}
		views[postView.URI] = &discover.FeedViewPost{Post: postView}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	feedPosts := make([]*discover.FeedViewPost, 0, len(selected))
	for _, candidate := range selected {
		if view := views[candidate.URI]; view != nil {
			feedPosts = append(feedPosts, view)
		}
	}
	return feedPosts, nil
}

func (r *postgresDiscoverRepo) buildDiscoverHotCursor(snapshotID, checkpointID int64, viewerDID string) (string, error) {
	payload := strings.Join([]string{
		discoverHotCursorVersion,
		strconv.FormatInt(snapshotID, 10),
		strconv.FormatInt(checkpointID, 10),
		discoverHotViewerScope(viewerDID),
	}, cursorDelimiter)
	cursor := r.signCursorPayload(payload)
	if len(cursor) > discoverHotCursorMaxLength {
		return "", fmt.Errorf("Discover Hot cursor exceeds %d characters", discoverHotCursorMaxLength)
	}
	return cursor, nil
}

func (r *postgresDiscoverRepo) parseDiscoverHotCursor(cursor, viewerDID string) (int64, int64, error) {
	if cursor == "" || len(cursor) > discoverHotCursorMaxLength {
		return 0, 0, discover.ErrInvalidCursor
	}
	payload, err := r.verifyCursorPayload(cursor)
	if err != nil {
		return 0, 0, discover.ErrInvalidCursor
	}
	parts := strings.Split(payload, cursorDelimiter)
	if len(parts) != 4 || parts[0] != discoverHotCursorVersion || parts[3] != discoverHotViewerScope(viewerDID) {
		return 0, 0, discover.ErrInvalidCursor
	}
	snapshotID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || snapshotID <= 0 {
		return 0, 0, discover.ErrInvalidCursor
	}
	checkpointID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || checkpointID <= 0 {
		return 0, 0, discover.ErrInvalidCursor
	}
	return snapshotID, checkpointID, nil
}

func discoverHotViewerScope(viewerDID string) string {
	sum := sha256.Sum256([]byte("discover-hot-viewer:" + viewerDID))
	return hex.EncodeToString(sum[:])
}

func discoverHotSnapshotLockKey(scope string) int64 {
	sum := sha256.Sum256([]byte(strconv.Itoa(discoverHotAlgorithmVersion) + ":discover-hot-snapshot:" + scope))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func discoverHotSnapshotAdmissionLockKey() int64 {
	sum := sha256.Sum256([]byte("discover-hot-snapshot-admission"))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func discoverHotCheckpointAdmissionLockKey() int64 {
	sum := sha256.Sum256([]byte("discover-hot-checkpoint-admission"))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotCandidateLimit() int {
	if r.discoverHotCandidateLimit <= 0 {
		return discoverHotDefaultCandidateLimit
	}
	return r.discoverHotCandidateLimit
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotStoredCandidateLimit() int {
	if r.discoverHotStoredCandidateLimit <= 0 {
		return discoverHotDefaultStoredCandidateLimit
	}
	return r.discoverHotStoredCandidateLimit
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotStoredCheckpointLimit() int {
	if r.discoverHotStoredCheckpointLimit <= 0 {
		return discoverHotDefaultStoredCheckpointLimit
	}
	return r.discoverHotStoredCheckpointLimit
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotCheckpointStateByteLimit() int {
	if r.discoverHotCheckpointStateByteLimit <= 0 {
		return discoverHotDefaultCheckpointStateByteLimit
	}
	return r.discoverHotCheckpointStateByteLimit
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotSnapshotBuildsPerMinuteLimit() int {
	if r.discoverHotSnapshotBuildsPerMinuteLimit <= 0 {
		return discoverHotDefaultSnapshotBuildsPerMinuteLimit
	}
	return r.discoverHotSnapshotBuildsPerMinuteLimit
}

func (r *postgresDiscoverRepo) effectiveDiscoverHotWorkDeadline() time.Duration {
	if r.discoverHotWorkDeadline <= 0 {
		return discoverHotDefaultWorkDeadline
	}
	return r.discoverHotWorkDeadline
}
