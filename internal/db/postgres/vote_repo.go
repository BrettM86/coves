package postgres

import (
	"Coves/internal/core/votes"
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type postgresVoteRepo struct {
	db *sql.DB
}

// NewVoteRepository creates a new PostgreSQL vote repository
func NewVoteRepository(db *sql.DB) votes.Repository {
	return &postgresVoteRepo{db: db}
}

// Create inserts a new vote into the votes table
// Called by Jetstream consumer after vote is created on PDS
// Idempotent: Returns success if vote already exists (for Jetstream replays)
func (r *postgresVoteRepo) Create(ctx context.Context, vote *votes.Vote) error {
	query := `
		INSERT INTO votes (
			uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, NOW()
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id, indexed_at
	`

	err := r.db.QueryRowContext(
		ctx, query,
		vote.URI, vote.CID, vote.RKey, vote.VoterDID,
		vote.SubjectURI, vote.SubjectCID, vote.Direction,
		vote.CreatedAt,
	).Scan(&vote.ID, &vote.IndexedAt)

	// ON CONFLICT DO NOTHING returns no rows if duplicate - this is OK (idempotent)
	if err == sql.ErrNoRows {
		return nil // Vote already exists, no error for idempotency
	}

	if err != nil {
		// Check for unique constraint violation (voter + subject)
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "unique_voter_subject") {
			return votes.ErrVoteAlreadyExists
		}

		// Check for DID format constraint violation
		if strings.Contains(err.Error(), "chk_voter_did_format") {
			return fmt.Errorf("invalid voter DID format: %s", vote.VoterDID)
		}

		return fmt.Errorf("failed to insert vote: %w", err)
	}

	return nil
}

// GetByURI retrieves an active vote by its AT-URI
// Used by Jetstream consumer for DELETE operations
// Returns ErrVoteNotFound for soft-deleted votes
func (r *postgresVoteRepo) GetByURI(ctx context.Context, uri string) (*votes.Vote, error) {
	query := `
		SELECT
			id, uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at, deleted_at
		FROM votes
		WHERE uri = $1 AND deleted_at IS NULL
	`

	var vote votes.Vote

	err := r.db.QueryRowContext(ctx, query, uri).Scan(
		&vote.ID, &vote.URI, &vote.CID, &vote.RKey, &vote.VoterDID,
		&vote.SubjectURI, &vote.SubjectCID, &vote.Direction,
		&vote.CreatedAt, &vote.IndexedAt, &vote.DeletedAt,
	)

	if err == sql.ErrNoRows {
		return nil, votes.ErrVoteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get vote by URI: %w", err)
	}

	return &vote, nil
}

// GetByVoterAndSubject retrieves a user's vote on a specific subject
// Used by service to check existing vote state before creating/toggling
func (r *postgresVoteRepo) GetByVoterAndSubject(ctx context.Context, voterDID, subjectURI string) (*votes.Vote, error) {
	query := `
		SELECT
			id, uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at, deleted_at
		FROM votes
		WHERE voter_did = $1 AND subject_uri = $2 AND deleted_at IS NULL
	`

	var vote votes.Vote

	err := r.db.QueryRowContext(ctx, query, voterDID, subjectURI).Scan(
		&vote.ID, &vote.URI, &vote.CID, &vote.RKey, &vote.VoterDID,
		&vote.SubjectURI, &vote.SubjectCID, &vote.Direction,
		&vote.CreatedAt, &vote.IndexedAt, &vote.DeletedAt,
	)

	if err == sql.ErrNoRows {
		return nil, votes.ErrVoteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get vote by voter and subject: %w", err)
	}

	return &vote, nil
}

// Delete soft-deletes a vote (sets deleted_at)
// Called by Jetstream consumer after vote is deleted from PDS
// Idempotent: Returns success if vote already deleted
func (r *postgresVoteRepo) Delete(ctx context.Context, uri string) error {
	query := `
		UPDATE votes
		SET deleted_at = NOW()
		WHERE uri = $1 AND deleted_at IS NULL
	`

	result, err := r.db.ExecContext(ctx, query, uri)
	if err != nil {
		return fmt.Errorf("failed to delete vote: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}

	// Idempotent: If no rows affected, vote already deleted (OK for Jetstream replays)
	if rowsAffected == 0 {
		return nil
	}

	return nil
}

// ListBySubject retrieves all active votes on a specific post/comment
// Future: Used for vote detail views
func (r *postgresVoteRepo) ListBySubject(ctx context.Context, subjectURI string, limit, offset int) ([]*votes.Vote, error) {
	query := `
		SELECT
			id, uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at, deleted_at
		FROM votes
		WHERE subject_uri = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, subjectURI, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list votes by subject: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*votes.Vote
	for rows.Next() {
		var vote votes.Vote
		err := rows.Scan(
			&vote.ID, &vote.URI, &vote.CID, &vote.RKey, &vote.VoterDID,
			&vote.SubjectURI, &vote.SubjectCID, &vote.Direction,
			&vote.CreatedAt, &vote.IndexedAt, &vote.DeletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan vote: %w", err)
		}
		result = append(result, &vote)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating votes: %w", err)
	}

	return result, nil
}

// ListByVoter retrieves all active votes by a specific user
// Future: Used for user voting history
func (r *postgresVoteRepo) ListByVoter(ctx context.Context, voterDID string, limit, offset int) ([]*votes.Vote, error) {
	query := `
		SELECT
			id, uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at, deleted_at
		FROM votes
		WHERE voter_did = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, voterDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list votes by voter: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*votes.Vote
	for rows.Next() {
		var vote votes.Vote
		err := rows.Scan(
			&vote.ID, &vote.URI, &vote.CID, &vote.RKey, &vote.VoterDID,
			&vote.SubjectURI, &vote.SubjectCID, &vote.Direction,
			&vote.CreatedAt, &vote.IndexedAt, &vote.DeletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan vote: %w", err)
		}
		result = append(result, &vote)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating votes: %w", err)
	}

	return result, nil
}
