package postgres

import (
	"context"
	"database/sql"

	"Coves/internal/core/posts"
)

// COMPILE STUB — no behaviour yet.
//
// This file exists so that the T1 contract in admission_repo_lifecycle_test.go
// compiles and fails on its ASSERTIONS rather than on a missing symbol: a red
// that is a build error proves nothing about the specification. Every method
// returns zero values. The implementation — single-statement CAS upserts over
// community_post_admissions, gated by the §5.2 (rev, op-rank) watermark — lands
// with migration 034.

type postgresAdmissionRepo struct {
	db *sql.DB
}

// NewAdmissionRepository creates a PostgreSQL admission repository.
func NewAdmissionRepository(db *sql.DB) posts.AdmissionRepository {
	return &postgresAdmissionRepo{db: db}
}

func (r *postgresAdmissionRepo) UpsertPending(ctx context.Context, cmd posts.UpsertPendingCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) ApplyAcceptance(ctx context.Context, cmd posts.ApplyAcceptanceCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) ApplyAcceptanceDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) ApplyRemoval(ctx context.Context, cmd posts.ApplyRemovalCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) ApplyRemovalDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) Get(ctx context.Context, communityDID, postURI string) (*posts.Admission, error) {
	return nil, nil
}
