package postgres

import (
	"Coves/internal/core/communitysuggestions"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"
)

type postgresCommunitySuggestionRepo struct {
	db *sql.DB
}

// NewCommunitySuggestionRepository creates a new PostgreSQL community suggestion repository
func NewCommunitySuggestionRepository(db *sql.DB) communitysuggestions.Repository {
	return &postgresCommunitySuggestionRepo{db: db}
}

// Create inserts a new community suggestion into the database
// Returns the suggestion with ID, CreatedAt, and UpdatedAt populated
// KNOWN DEFECT (issue 2026-07-31-suggestion-check-constraints-weaker-than-go.md): the title_not_empty and
// description_not_empty CHECK constraints this maps are WEAKER than the Go validation
// they back. Postgres' one-argument TRIM strips the SPACE character only, while the
// service uses strings.TrimSpace — so a title of "\t\n\r" passes the constraint and
// lands on the board as a blank, votable row.
// (see TestSuggestionRepo_BlankTitleOfTabsSlipsPastTheConstraint)
func (r *postgresCommunitySuggestionRepo) Create(ctx context.Context, suggestion *communitysuggestions.CommunitySuggestion) error {
	query := `
		INSERT INTO community_suggestions (
			title, description, submitter_did, status
		) VALUES (
			$1, $2, $3, $4
		)
		RETURNING id, created_at, updated_at
	`

	status := suggestion.Status
	if status == "" {
		status = communitysuggestions.StatusOpen
	}

	err := r.db.QueryRowContext(
		ctx, query,
		suggestion.Title, suggestion.Description,
		suggestion.SubmitterDID, string(status),
	).Scan(&suggestion.ID, &suggestion.CreatedAt, &suggestion.UpdatedAt)

	if err != nil {
		if pqErr := extractPQError(err); pqErr != nil {
			if strings.Contains(pqErr.Constraint, "valid_suggestion_status") {
				return communitysuggestions.ErrInvalidStatus
			}
			if strings.Contains(pqErr.Constraint, "title_not_empty") {
				return communitysuggestions.ErrTitleRequired
			}
			if strings.Contains(pqErr.Constraint, "title_max_length") {
				return communitysuggestions.ErrTitleTooLong
			}
			if strings.Contains(pqErr.Constraint, "description_not_empty") {
				return communitysuggestions.ErrDescriptionRequired
			}
			if strings.Contains(pqErr.Constraint, "description_max_length") {
				return communitysuggestions.ErrDescriptionTooLong
			}
		}
		return fmt.Errorf("failed to create community suggestion: %w", err)
	}

	suggestion.Status = status
	return nil
}

// GetByID retrieves a single community suggestion by its ID
// Returns ErrSuggestionNotFound if the suggestion does not exist
func (r *postgresCommunitySuggestionRepo) GetByID(ctx context.Context, id int64) (*communitysuggestions.CommunitySuggestion, error) {
	query := `
		SELECT id, title, description, submitter_did, status,
			   vote_count, created_at, updated_at
		FROM community_suggestions
		WHERE id = $1
	`

	var suggestion communitysuggestions.CommunitySuggestion
	var status string

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&suggestion.ID, &suggestion.Title, &suggestion.Description,
		&suggestion.SubmitterDID, &status,
		&suggestion.VoteCount, &suggestion.CreatedAt, &suggestion.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, communitysuggestions.ErrSuggestionNotFound
		}
		return nil, fmt.Errorf("failed to get community suggestion by ID: %w", err)
	}

	suggestion.Status = communitysuggestions.Status(status)
	return &suggestion, nil
}

// List retrieves community suggestions with optional filtering and sorting
// Supports sorting by "popular" (vote_count DESC, created_at DESC) or "new" (created_at DESC)
// Supports optional filtering by status
func (r *postgresCommunitySuggestionRepo) List(ctx context.Context, req communitysuggestions.ListSuggestionsRequest) ([]*communitysuggestions.CommunitySuggestion, error) {
	var queryBuilder strings.Builder
	var args []interface{}
	argIndex := 1

	queryBuilder.WriteString(`
		SELECT id, title, description, submitter_did, status,
			   vote_count, created_at, updated_at
		FROM community_suggestions
	`)

	// Optional status filter
	if req.Status != "" {
		queryBuilder.WriteString(fmt.Sprintf(" WHERE status = $%d", argIndex))
		args = append(args, req.Status)
		argIndex++
	}

	// Sorting
	switch req.Sort {
	case "popular":
		queryBuilder.WriteString(" ORDER BY vote_count DESC, created_at DESC")
	case "new", "":
		queryBuilder.WriteString(" ORDER BY created_at DESC")
	default:
		return nil, fmt.Errorf("unknown sort value: %s", req.Sort)
	}

	// Pagination
	queryBuilder.WriteString(fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIndex, argIndex+1))
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, req.Offset)

	rows, err := r.db.QueryContext(ctx, queryBuilder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list community suggestions: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close rows in List community suggestions",
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	var suggestions []*communitysuggestions.CommunitySuggestion
	for rows.Next() {
		suggestion, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, suggestion)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating community suggestions: %w", err)
	}

	return suggestions, nil
}

// CountBySubmitterSince counts the number of suggestions created by a submitter since a given time
// Used for rate limiting suggestion creation
func (r *postgresCommunitySuggestionRepo) CountBySubmitterSince(ctx context.Context, submitterDID string, since time.Time) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM community_suggestions
		WHERE submitter_did = $1 AND created_at >= $2
	`

	var count int
	err := r.db.QueryRowContext(ctx, query, submitterDID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count suggestions by submitter: %w", err)
	}

	return count, nil
}

// UpdateStatus updates a suggestion's status
// Returns ErrSuggestionNotFound if the suggestion does not exist
func (r *postgresCommunitySuggestionRepo) UpdateStatus(ctx context.Context, id int64, status communitysuggestions.Status) error {
	query := `
		UPDATE community_suggestions
		SET status = $1, updated_at = NOW()
		WHERE id = $2
	`

	result, err := r.db.ExecContext(ctx, query, string(status), id)
	if err != nil {
		if pqErr := extractPQError(err); pqErr != nil {
			if strings.Contains(pqErr.Constraint, "valid_suggestion_status") {
				return communitysuggestions.ErrInvalidStatus
			}
		}
		return fmt.Errorf("failed to update community suggestion status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	if rowsAffected == 0 {
		return communitysuggestions.ErrSuggestionNotFound
	}

	return nil
}

// UpsertVote inserts or updates a vote for a suggestion and atomically updates the vote count
// Returns the delta applied to the suggestion's vote count
// Uses a transaction to ensure consistency between the vote and the denormalized count
// KNOWN DEFECT (issue 2026-07-31-suggestion-vote-error-misclassification-and-lock-order.md), two of them:
//
//  1. No SQLSTATE 23503 mapping. Voting on a suggestion that does not exist escapes as a
//     bare pq error, so IsNotFound is false and the handler answers 500 — where
//     AtomicVote, one function away, answers 404.
//  2. valid_vote_value is mapped on the INSERT branch only, so an out-of-range value from
//     a repeat voter takes the UPDATE branch and is wrapped generically. The same
//     malformed request is a 400 for a first-time voter and a 500 for a repeat one.
//
// Also note this method locks vote-row-then-suggestion-row while AtomicVote locks them
// the other way round; mixing the two on one suggestion can deadlock. Not reachable
// today (Service.Vote uses AtomicVote exclusively) — see the issue.
// (see TestSuggestionRepo_UpsertVoteMisclassifiesTwoFailures)
func (r *postgresCommunitySuggestionRepo) UpsertVote(ctx context.Context, suggestionID int64, voterDID string, value int) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction for upsert vote: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("failed to rollback upsert vote transaction",
				slog.String("error", rbErr.Error()),
			)
		}
	}()

	// Check for existing vote with row lock
	var existingValue int
	var hasExisting bool
	selectQuery := `
		SELECT value FROM suggestion_votes
		WHERE suggestion_id = $1 AND voter_did = $2
		FOR UPDATE
	`
	err = tx.QueryRowContext(ctx, selectQuery, suggestionID, voterDID).Scan(&existingValue)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("failed to check existing vote: %w", err)
	}
	hasExisting = err == nil

	var delta int
	if hasExisting {
		// Update existing vote
		updateQuery := `
			UPDATE suggestion_votes
			SET value = $1
			WHERE suggestion_id = $2 AND voter_did = $3
		`
		_, err = tx.ExecContext(ctx, updateQuery, value, suggestionID, voterDID)
		if err != nil {
			return 0, fmt.Errorf("failed to update vote: %w", err)
		}
		// Delta is the difference between new and old value
		delta = value - existingValue
	} else {
		// Insert new vote
		insertQuery := `
			INSERT INTO suggestion_votes (suggestion_id, voter_did, value)
			VALUES ($1, $2, $3)
		`
		_, err = tx.ExecContext(ctx, insertQuery, suggestionID, voterDID, value)
		if err != nil {
			if pqErr := extractPQError(err); pqErr != nil {
				if strings.Contains(pqErr.Constraint, "valid_vote_value") {
					return 0, communitysuggestions.ErrInvalidVoteValue
				}
			}
			return 0, fmt.Errorf("failed to insert vote: %w", err)
		}
		delta = value
	}

	// Atomically update the denormalized vote count
	updateCountQuery := `
		UPDATE community_suggestions
		SET vote_count = vote_count + $1, updated_at = NOW()
		WHERE id = $2
	`
	_, err = tx.ExecContext(ctx, updateCountQuery, delta, suggestionID)
	if err != nil {
		return 0, fmt.Errorf("failed to update vote count: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit upsert vote transaction: %w", err)
	}

	return delta, nil
}

// DeleteVote removes a vote from a suggestion and atomically updates the vote count
// Returns the delta applied to the suggestion's vote count
// Uses a transaction to ensure consistency between the vote deletion and the denormalized count
func (r *postgresCommunitySuggestionRepo) DeleteVote(ctx context.Context, suggestionID int64, voterDID string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction for delete vote: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("failed to rollback delete vote transaction",
				slog.String("error", rbErr.Error()),
			)
		}
	}()

	// Delete the vote and get the deleted value
	deleteQuery := `
		DELETE FROM suggestion_votes
		WHERE suggestion_id = $1 AND voter_did = $2
		RETURNING value
	`
	var deletedValue int
	err = tx.QueryRowContext(ctx, deleteQuery, suggestionID, voterDID).Scan(&deletedValue)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, communitysuggestions.ErrVoteNotFound
		}
		return 0, fmt.Errorf("failed to delete vote: %w", err)
	}

	// Atomically update the denormalized vote count (subtract the deleted vote value)
	delta := -deletedValue
	updateCountQuery := `
		UPDATE community_suggestions
		SET vote_count = vote_count + $1, updated_at = NOW()
		WHERE id = $2
	`
	result, err := tx.ExecContext(ctx, updateCountQuery, delta, suggestionID)
	if err != nil {
		return 0, fmt.Errorf("failed to update vote count after delete: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to check vote count update result: %w", err)
	}
	if rowsAffected == 0 {
		return 0, communitysuggestions.ErrSuggestionNotFound
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit delete vote transaction: %w", err)
	}

	return delta, nil
}

// AtomicVote atomically handles voting with toggle semantics in a single transaction
// If no existing vote: creates the vote
// If existing vote in same direction: removes the vote (toggle off)
// If existing vote in opposite direction: flips the vote
// KNOWN DEFECT (issue 2026-07-31-suggestion-vote-error-misclassification-and-lock-order.md): valid_vote_value is
// mapped on the INSERT branch only, so an out-of-range value from a voter who has already
// voted takes the flip branch and is wrapped generically — a 500 where a first-time
// voter gets a 400.
// (see TestSuggestionRepo_AtomicVoteFlipToAnInvalidValueIsMisclassified)
func (r *postgresCommunitySuggestionRepo) AtomicVote(ctx context.Context, suggestionID int64, voterDID string, value int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction for atomic vote: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("failed to rollback atomic vote transaction",
				slog.String("error", rbErr.Error()),
			)
		}
	}()

	// Verify suggestion exists and lock the row to prevent concurrent modification
	var exists bool
	existsQuery := `SELECT EXISTS(SELECT 1 FROM community_suggestions WHERE id = $1 FOR UPDATE)`
	err = tx.QueryRowContext(ctx, existsQuery, suggestionID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check suggestion existence: %w", err)
	}
	if !exists {
		return communitysuggestions.ErrSuggestionNotFound
	}

	// Check for existing vote with row lock
	var existingValue int
	var hasExisting bool
	selectQuery := `
		SELECT value FROM suggestion_votes
		WHERE suggestion_id = $1 AND voter_did = $2
		FOR UPDATE
	`
	err = tx.QueryRowContext(ctx, selectQuery, suggestionID, voterDID).Scan(&existingValue)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to check existing vote: %w", err)
	}
	hasExisting = err == nil

	var delta int
	if hasExisting {
		if existingValue == value {
			// Same direction: toggle off (remove the vote)
			deleteQuery := `
				DELETE FROM suggestion_votes
				WHERE suggestion_id = $1 AND voter_did = $2
			`
			_, err = tx.ExecContext(ctx, deleteQuery, suggestionID, voterDID)
			if err != nil {
				return fmt.Errorf("failed to delete vote during toggle: %w", err)
			}
			delta = -existingValue
		} else {
			// Opposite direction: flip the vote
			updateQuery := `
				UPDATE suggestion_votes
				SET value = $1
				WHERE suggestion_id = $2 AND voter_did = $3
			`
			_, err = tx.ExecContext(ctx, updateQuery, value, suggestionID, voterDID)
			if err != nil {
				return fmt.Errorf("failed to update vote during flip: %w", err)
			}
			delta = value - existingValue
		}
	} else {
		// No existing vote: insert new
		insertQuery := `
			INSERT INTO suggestion_votes (suggestion_id, voter_did, value)
			VALUES ($1, $2, $3)
		`
		_, err = tx.ExecContext(ctx, insertQuery, suggestionID, voterDID, value)
		if err != nil {
			if pqErr := extractPQError(err); pqErr != nil {
				if pqErr.Code == "23503" {
					return communitysuggestions.ErrSuggestionNotFound
				}
				if strings.Contains(pqErr.Constraint, "valid_vote_value") {
					return communitysuggestions.ErrInvalidVoteValue
				}
			}
			return fmt.Errorf("failed to insert vote: %w", err)
		}
		delta = value
	}

	// Update the denormalized vote count and verify the suggestion still exists
	updateCountQuery := `
		UPDATE community_suggestions
		SET vote_count = vote_count + $1, updated_at = NOW()
		WHERE id = $2
	`
	result, err := tx.ExecContext(ctx, updateCountQuery, delta, suggestionID)
	if err != nil {
		return fmt.Errorf("failed to update vote count: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check vote count update result: %w", err)
	}
	if rowsAffected == 0 {
		return communitysuggestions.ErrSuggestionNotFound
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit atomic vote transaction: %w", err)
	}

	return nil
}

// GetVote retrieves a single vote by suggestion ID and voter DID
// Returns ErrVoteNotFound if the vote does not exist
func (r *postgresCommunitySuggestionRepo) GetVote(ctx context.Context, suggestionID int64, voterDID string) (*communitysuggestions.SuggestionVote, error) {
	query := `
		SELECT id, suggestion_id, voter_did, value, created_at
		FROM suggestion_votes
		WHERE suggestion_id = $1 AND voter_did = $2
	`

	var vote communitysuggestions.SuggestionVote
	err := r.db.QueryRowContext(ctx, query, suggestionID, voterDID).Scan(
		&vote.ID, &vote.SuggestionID, &vote.VoterDID,
		&vote.Value, &vote.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, communitysuggestions.ErrVoteNotFound
		}
		return nil, fmt.Errorf("failed to get vote: %w", err)
	}

	return &vote, nil
}

// GetVotesForViewer retrieves the votes cast by a viewer on a set of suggestions
// Returns a map of suggestion ID to vote value
func (r *postgresCommunitySuggestionRepo) GetVotesForViewer(ctx context.Context, voterDID string, suggestionIDs []int64) (map[int64]int, error) {
	if len(suggestionIDs) == 0 {
		return make(map[int64]int), nil
	}

	query := `
		SELECT suggestion_id, value
		FROM suggestion_votes
		WHERE voter_did = $1 AND suggestion_id = ANY($2)
	`

	rows, err := r.db.QueryContext(ctx, query, voterDID, pq.Array(suggestionIDs))
	if err != nil {
		return nil, fmt.Errorf("failed to get votes for viewer: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close rows in GetVotesForViewer",
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	votes := make(map[int64]int)
	for rows.Next() {
		var suggestionID int64
		var value int
		if err := rows.Scan(&suggestionID, &value); err != nil {
			return nil, fmt.Errorf("failed to scan vote for viewer: %w", err)
		}
		votes[suggestionID] = value
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating votes for viewer: %w", err)
	}

	return votes, nil
}

// scanSuggestion scans a single suggestion from a database row
func scanSuggestion(rows *sql.Rows) (*communitysuggestions.CommunitySuggestion, error) {
	var suggestion communitysuggestions.CommunitySuggestion
	var status string

	err := rows.Scan(
		&suggestion.ID, &suggestion.Title, &suggestion.Description,
		&suggestion.SubmitterDID, &status,
		&suggestion.VoteCount, &suggestion.CreatedAt, &suggestion.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan community suggestion: %w", err)
	}

	suggestion.Status = communitysuggestions.Status(status)
	return &suggestion, nil
}
