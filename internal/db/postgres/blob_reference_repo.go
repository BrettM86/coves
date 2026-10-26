package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/core/blobreferences"
)

type postgresBlobReferenceRepo struct{ db *sql.DB }

// NewBlobReferenceRepository returns the Postgres blob reference repository.
func NewBlobReferenceRepository(db *sql.DB) blobreferences.Repository {
	return &postgresBlobReferenceRepo{db: db}
}

func (r *postgresBlobReferenceRepo) ReplaceSubjectTx(ctx context.Context, tx *sql.Tx, ownerDID, subjectURI string, references []embedblobs.Reference) error {
	if _, err := tx.ExecContext(ctx, `UPDATE blob_references SET active = false, updated_at = NOW()
		WHERE subject_uri = $1 AND active`, subjectURI); err != nil {
		return fmt.Errorf("deactivate previous blob references for %s: %w", subjectURI, err)
	}
	for _, reference := range references {
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (owner_did, blob_cid, subject_uri, kind)
			DO UPDATE SET active = true, updated_at = NOW()`, ownerDID, reference.CID, subjectURI, reference.Kind); err != nil {
			return fmt.Errorf("write blob reference for %s: %w", subjectURI, err)
		}
	}
	return nil
}

func (r *postgresBlobReferenceRepo) DeactivateSubjectTx(ctx context.Context, tx *sql.Tx, subjectURI string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE blob_references SET active = false, updated_at = NOW()
		WHERE subject_uri = $1 AND active`, subjectURI); err != nil {
		return fmt.Errorf("deactivate blob references for %s: %w", subjectURI, err)
	}
	return nil
}

// ListBlobOwners returns every distinct owner of a CID, including inactive references.
func (r *postgresBlobReferenceRepo) ListBlobOwners(ctx context.Context, cid string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT owner_did FROM blob_references WHERE blob_cid = $1`, cid)
	if err != nil {
		return nil, fmt.Errorf("query blob owners: %w", err)
	}
	defer rows.Close()

	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("scan blob owner: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate blob owners: %w", err)
	}
	return owners, nil
}

// HasActiveReference reports whether the owner has any active reference to the blob.
func (r *postgresBlobReferenceRepo) HasActiveReference(ctx context.Context, ownerDID, cid string) (bool, error) {
	var active bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM blob_references WHERE owner_did = $1 AND blob_cid = $2 AND active
	)`, ownerDID, cid).Scan(&active); err != nil {
		return false, fmt.Errorf("check active blob reference: %w", err)
	}
	return active, nil
}
