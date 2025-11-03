package postgres

import (
	"Coves/internal/core/votes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestDB creates a test database connection and runs migrations
func setupTestDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err, "Failed to connect to test database")

	// Run migrations
	require.NoError(t, goose.Up(db, "../../db/migrations"), "Failed to run migrations")

	return db
}

// cleanupVotes removes all test votes and users from the database
func cleanupVotes(t *testing.T, db *sql.DB) {
	_, err := db.Exec("DELETE FROM votes WHERE voter_did LIKE 'did:plc:test%' OR voter_did LIKE 'did:plc:nonexistent%'")
	require.NoError(t, err, "Failed to cleanup votes")

	_, err = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:test%'")
	require.NoError(t, err, "Failed to cleanup test users")
}

// createTestUser creates a minimal test user for foreign key constraints
func createTestUser(t *testing.T, db *sql.DB, handle, did string) {
	query := `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO NOTHING
	`
	_, err := db.Exec(query, did, handle, "https://bsky.social")
	require.NoError(t, err, "Failed to create test user")
}

func TestVoteRepo_Create(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	// Create test voter
	voterDID := "did:plc:testvoter123"
	createTestUser(t, db, "testvoter123.test", voterDID)

	vote := &votes.Vote{
		URI:        "at://did:plc:testvoter123/social.coves.interaction.vote/3k1234567890",
		CID:        "bafyreigtest123",
		RKey:       "3k1234567890",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/abc123",
		SubjectCID: "bafyreigpost123",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}

	err := repo.Create(ctx, vote)
	assert.NoError(t, err)
	assert.NotZero(t, vote.ID, "Vote ID should be set after creation")
	assert.NotZero(t, vote.IndexedAt, "IndexedAt should be set after creation")
}

func TestVoteRepo_Create_Idempotent(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoter456"
	createTestUser(t, db, "testvoter456.test", voterDID)

	vote := &votes.Vote{
		URI:        "at://did:plc:testvoter456/social.coves.interaction.vote/3k9876543210",
		CID:        "bafyreigtest456",
		RKey:       "3k9876543210",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/xyz789",
		SubjectCID: "bafyreigpost456",
		Direction:  "down",
		CreatedAt:  time.Now(),
	}

	// Create first time
	err := repo.Create(ctx, vote)
	require.NoError(t, err)

	// Create again with same URI - should be idempotent (no error)
	vote2 := &votes.Vote{
		URI:        vote.URI, // Same URI
		CID:        "bafyreigdifferent",
		RKey:       vote.RKey,
		VoterDID:   voterDID,
		SubjectURI: vote.SubjectURI,
		SubjectCID: vote.SubjectCID,
		Direction:  "up", // Different direction
		CreatedAt:  time.Now(),
	}

	err = repo.Create(ctx, vote2)
	assert.NoError(t, err, "Creating duplicate URI should be idempotent (ON CONFLICT DO NOTHING)")
}

func TestVoteRepo_Create_VoterNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	// Don't create test user - vote should still be created (FK removed)
	// This allows votes to be indexed before users in Jetstream
	vote := &votes.Vote{
		URI:        "at://did:plc:nonexistentvoter/social.coves.interaction.vote/3k1111111111",
		CID:        "bafyreignovoter",
		RKey:       "3k1111111111",
		VoterDID:   "did:plc:nonexistentvoter",
		SubjectURI: "at://did:plc:community/social.coves.post.record/test123",
		SubjectCID: "bafyreigpost789",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}

	err := repo.Create(ctx, vote)
	if err != nil {
		t.Logf("Create error: %v", err)
	}
	assert.NoError(t, err, "Vote should be created even if voter doesn't exist (FK removed)")
	assert.NotZero(t, vote.ID, "Vote should have an ID")
	t.Logf("Vote created with ID: %d", vote.ID)
}

func TestVoteRepo_GetByURI(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoter789"
	createTestUser(t, db, "testvoter789.test", voterDID)

	// Create vote
	vote := &votes.Vote{
		URI:        "at://did:plc:testvoter789/social.coves.interaction.vote/3k5555555555",
		CID:        "bafyreigtest789",
		RKey:       "3k5555555555",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/post123",
		SubjectCID: "bafyreigpost999",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}
	err := repo.Create(ctx, vote)
	require.NoError(t, err)

	// Retrieve by URI
	retrieved, err := repo.GetByURI(ctx, vote.URI)
	assert.NoError(t, err)
	assert.Equal(t, vote.URI, retrieved.URI)
	assert.Equal(t, vote.VoterDID, retrieved.VoterDID)
	assert.Equal(t, vote.Direction, retrieved.Direction)
	assert.Nil(t, retrieved.DeletedAt, "DeletedAt should be nil for active vote")
}

func TestVoteRepo_GetByURI_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := NewVoteRepository(db)
	ctx := context.Background()

	_, err := repo.GetByURI(ctx, "at://did:plc:nonexistent/social.coves.interaction.vote/nope")
	assert.ErrorIs(t, err, votes.ErrVoteNotFound)
}

func TestVoteRepo_GetByVoterAndSubject(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoter999"
	createTestUser(t, db, "testvoter999.test", voterDID)

	subjectURI := "at://did:plc:community/social.coves.post.record/subject123"

	// Create vote
	vote := &votes.Vote{
		URI:        "at://did:plc:testvoter999/social.coves.interaction.vote/3k6666666666",
		CID:        "bafyreigtest999",
		RKey:       "3k6666666666",
		VoterDID:   voterDID,
		SubjectURI: subjectURI,
		SubjectCID: "bafyreigsubject123",
		Direction:  "down",
		CreatedAt:  time.Now(),
	}
	err := repo.Create(ctx, vote)
	require.NoError(t, err)

	// Retrieve by voter + subject
	retrieved, err := repo.GetByVoterAndSubject(ctx, voterDID, subjectURI)
	assert.NoError(t, err)
	assert.Equal(t, vote.URI, retrieved.URI)
	assert.Equal(t, voterDID, retrieved.VoterDID)
	assert.Equal(t, subjectURI, retrieved.SubjectURI)
}

func TestVoteRepo_GetByVoterAndSubject_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := NewVoteRepository(db)
	ctx := context.Background()

	_, err := repo.GetByVoterAndSubject(ctx, "did:plc:nobody", "at://did:plc:community/social.coves.post.record/nopost")
	assert.ErrorIs(t, err, votes.ErrVoteNotFound)
}

func TestVoteRepo_Delete(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoterdelete"
	createTestUser(t, db, "testvoterdelete.test", voterDID)

	// Create vote
	vote := &votes.Vote{
		URI:        "at://did:plc:testvoterdelete/social.coves.interaction.vote/3k7777777777",
		CID:        "bafyreigdelete",
		RKey:       "3k7777777777",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/deletetest",
		SubjectCID: "bafyreigdeletepost",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}
	err := repo.Create(ctx, vote)
	require.NoError(t, err)

	// Delete vote
	err = repo.Delete(ctx, vote.URI)
	assert.NoError(t, err)

	// Verify vote is soft-deleted (still exists but has deleted_at)
	retrieved, err := repo.GetByURI(ctx, vote.URI)
	assert.NoError(t, err)
	assert.NotNil(t, retrieved.DeletedAt, "DeletedAt should be set after deletion")

	// GetByVoterAndSubject should not find deleted votes
	_, err = repo.GetByVoterAndSubject(ctx, voterDID, vote.SubjectURI)
	assert.ErrorIs(t, err, votes.ErrVoteNotFound, "GetByVoterAndSubject should not return deleted votes")
}

func TestVoteRepo_Delete_Idempotent(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoterdelete2"
	createTestUser(t, db, "testvoterdelete2.test", voterDID)

	vote := &votes.Vote{
		URI:        "at://did:plc:testvoterdelete2/social.coves.interaction.vote/3k8888888888",
		CID:        "bafyreigdelete2",
		RKey:       "3k8888888888",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/deletetest2",
		SubjectCID: "bafyreigdeletepost2",
		Direction:  "down",
		CreatedAt:  time.Now(),
	}
	err := repo.Create(ctx, vote)
	require.NoError(t, err)

	// Delete first time
	err = repo.Delete(ctx, vote.URI)
	assert.NoError(t, err)

	// Delete again - should be idempotent (no error)
	err = repo.Delete(ctx, vote.URI)
	assert.NoError(t, err, "Deleting already deleted vote should be idempotent")
}

func TestVoteRepo_ListBySubject(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID1 := "did:plc:testvoterlist1"
	voterDID2 := "did:plc:testvoterlist2"
	createTestUser(t, db, "testvoterlist1.test", voterDID1)
	createTestUser(t, db, "testvoterlist2.test", voterDID2)

	subjectURI := "at://did:plc:community/social.coves.post.record/listtest"

	// Create multiple votes on same subject
	vote1 := &votes.Vote{
		URI:        "at://did:plc:testvoterlist1/social.coves.interaction.vote/3k9999999991",
		CID:        "bafyreiglist1",
		RKey:       "3k9999999991",
		VoterDID:   voterDID1,
		SubjectURI: subjectURI,
		SubjectCID: "bafyreiglistpost",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}
	vote2 := &votes.Vote{
		URI:        "at://did:plc:testvoterlist2/social.coves.interaction.vote/3k9999999992",
		CID:        "bafyreiglist2",
		RKey:       "3k9999999992",
		VoterDID:   voterDID2,
		SubjectURI: subjectURI,
		SubjectCID: "bafyreiglistpost",
		Direction:  "down",
		CreatedAt:  time.Now(),
	}

	require.NoError(t, repo.Create(ctx, vote1))
	require.NoError(t, repo.Create(ctx, vote2))

	// List votes
	result, err := repo.ListBySubject(ctx, subjectURI, 10, 0)
	assert.NoError(t, err)
	assert.Len(t, result, 2, "Should find 2 votes on subject")
}

func TestVoteRepo_ListByVoter(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupVotes(t, db)

	repo := NewVoteRepository(db)
	ctx := context.Background()

	voterDID := "did:plc:testvoterlistvoter"
	createTestUser(t, db, "testvoterlistvoter.test", voterDID)

	// Create multiple votes by same voter
	vote1 := &votes.Vote{
		URI:        "at://did:plc:testvoterlistvoter/social.coves.interaction.vote/3k0000000001",
		CID:        "bafyreigvoter1",
		RKey:       "3k0000000001",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/post1",
		SubjectCID: "bafyreigp1",
		Direction:  "up",
		CreatedAt:  time.Now(),
	}
	vote2 := &votes.Vote{
		URI:        "at://did:plc:testvoterlistvoter/social.coves.interaction.vote/3k0000000002",
		CID:        "bafyreigvoter2",
		RKey:       "3k0000000002",
		VoterDID:   voterDID,
		SubjectURI: "at://did:plc:community/social.coves.post.record/post2",
		SubjectCID: "bafyreigp2",
		Direction:  "down",
		CreatedAt:  time.Now(),
	}

	require.NoError(t, repo.Create(ctx, vote1))
	require.NoError(t, repo.Create(ctx, vote2))

	// List votes by voter
	result, err := repo.ListByVoter(ctx, voterDID, 10, 0)
	assert.NoError(t, err)
	assert.Len(t, result, 2, "Should find 2 votes by voter")
}
