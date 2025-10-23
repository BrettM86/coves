package integration

import (
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestAggregatorRepository_Create tests basic aggregator creation
func TestAggregatorRepository_Create(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewAggregatorRepository(db)
	ctx := context.Background()

	t.Run("creates aggregator successfully", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID := generateTestDID(uniqueSuffix)

		// Create config schema (JSON Schema)
		configSchema := map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"maxItems": map[string]interface{}{
					"type":    "number",
					"minimum": 1,
					"maximum": 50,
				},
				"category": map[string]interface{}{
					"type": "string",
					"enum": []string{"news", "sports", "tech"},
				},
			},
		}
		schemaBytes, _ := json.Marshal(configSchema)

		agg := &aggregators.Aggregator{
			DID:          aggregatorDID,
			DisplayName:  "Test RSS Aggregator",
			Description:  "A test aggregator for integration testing",
			AvatarURL:    "bafytest123",
			ConfigSchema: schemaBytes,
			MaintainerDID: "did:plc:maintainer123",
			SourceURL:  "https://example.com/aggregator",
		CreatedAt:    time.Now(),
			IndexedAt:    time.Now(),
			RecordURI:    fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
			RecordCID:    "bagtest456",
		}

		err := repo.CreateAggregator(ctx, agg)
		if err != nil {
			t.Fatalf("Failed to create aggregator: %v", err)
		}

		// Verify it was created
		retrieved, err := repo.GetAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("Failed to retrieve aggregator: %v", err)
		}

		if retrieved.DID != aggregatorDID {
			t.Errorf("Expected DID %s, got %s", aggregatorDID, retrieved.DID)
		}
		if retrieved.DisplayName != "Test RSS Aggregator" {
			t.Errorf("Expected display name 'Test RSS Aggregator', got %s", retrieved.DisplayName)
		}
		if len(retrieved.ConfigSchema) == 0 {
			t.Error("Expected config schema to be stored")
		}
	})

	t.Run("upserts on duplicate DID", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID := generateTestDID(uniqueSuffix)

		agg := &aggregators.Aggregator{
			DID:         aggregatorDID,
			DisplayName: "Original Name",
		CreatedAt:    time.Now(),
			IndexedAt:   time.Now(),
			RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
			RecordCID:   "bagtest789",
		}

		// Create first time
		if err := repo.CreateAggregator(ctx, agg); err != nil {
			t.Fatalf("First create failed: %v", err)
		}

		// Create again with different name (should update)
		agg.DisplayName = "Updated Name"
		agg.RecordCID = "bagtest999"
		if err := repo.CreateAggregator(ctx, agg); err != nil {
			t.Fatalf("Upsert failed: %v", err)
		}

		// Verify it was updated
		retrieved, err := repo.GetAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("Failed to retrieve aggregator: %v", err)
		}

		if retrieved.DisplayName != "Updated Name" {
			t.Errorf("Expected display name 'Updated Name', got %s", retrieved.DisplayName)
		}
	})
}

// TestAggregatorRepository_IsAggregator tests the fast existence check
func TestAggregatorRepository_IsAggregator(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewAggregatorRepository(db)
	ctx := context.Background()

	t.Run("returns true for existing aggregator", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID := generateTestDID(uniqueSuffix)

		agg := &aggregators.Aggregator{
			DID:         aggregatorDID,
			DisplayName: "Test Aggregator",
		CreatedAt:    time.Now(),
			IndexedAt:   time.Now(),
			RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
			RecordCID:   "bagtest123",
		}

		if err := repo.CreateAggregator(ctx, agg); err != nil {
			t.Fatalf("Failed to create aggregator: %v", err)
		}

		exists, err := repo.IsAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("IsAggregator failed: %v", err)
		}

		if !exists {
			t.Error("Expected aggregator to exist")
		}
	})

	t.Run("returns false for non-existent aggregator", func(t *testing.T) {
		exists, err := repo.IsAggregator(ctx, "did:plc:nonexistent123")
		if err != nil {
			t.Fatalf("IsAggregator failed: %v", err)
		}

		if exists {
			t.Error("Expected aggregator to not exist")
		}
	})
}

// TestAggregatorAuthorization_Create tests authorization creation
func TestAggregatorAuthorization_Create(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("creates authorization successfully", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID := generateTestDID(uniqueSuffix + "agg")
		communityDID := generateTestDID(uniqueSuffix + "comm")

		// Create aggregator first
		agg := &aggregators.Aggregator{
			DID:         aggregatorDID,
			DisplayName: "Test Aggregator",
		CreatedAt:    time.Now(),
			IndexedAt:   time.Now(),
			RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
			RecordCID:   "bagtest123",
		}
		if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
			t.Fatalf("Failed to create aggregator: %v", err)
		}

		// Create community
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!test-comm-%s@coves.local", uniqueSuffix),
			Name:         "test-comm",
			OwnerDID:     "did:web:coves.local",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := commRepo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Create authorization
		config := map[string]interface{}{
			"maxItems": 10,
			"category": "tech",
		}
		configBytes, _ := json.Marshal(config)

		auth := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID,
			Enabled:       true,
			Config:        configBytes,
			CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/abc123", communityDID),
			RecordCID:     "bagauth456",
		}

		err := aggRepo.CreateAuthorization(ctx, auth)
		if err != nil {
			t.Fatalf("Failed to create authorization: %v", err)
		}

		// Verify it was created
		retrieved, err := aggRepo.GetAuthorization(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve authorization: %v", err)
		}

		if !retrieved.Enabled {
			t.Error("Expected authorization to be enabled")
		}
		if len(retrieved.Config) == 0 {
			t.Error("Expected config to be stored")
		}
	})

	t.Run("enforces unique constraint on (aggregator_did, community_did)", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID := generateTestDID(uniqueSuffix + "agg")
		communityDID := generateTestDID(uniqueSuffix + "comm")

		// Create aggregator
		agg := &aggregators.Aggregator{
			DID:         aggregatorDID,
			DisplayName: "Test Aggregator",
		CreatedAt:    time.Now(),
			IndexedAt:   time.Now(),
			RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
			RecordCID:   "bagtest123",
		}
		if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
			t.Fatalf("Failed to create aggregator: %v", err)
		}

		// Create community
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!test-unique-%s@coves.local", uniqueSuffix),
			Name:         "test-unique",
			OwnerDID:     "did:web:coves.local",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := commRepo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Create first authorization
		auth1 := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID,
			Enabled:       true,
			CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/first", communityDID),
			RecordCID:     "bagauth1",
		}
		if err := aggRepo.CreateAuthorization(ctx, auth1); err != nil {
			t.Fatalf("First authorization failed: %v", err)
		}

		// Try to create duplicate (should update via ON CONFLICT)
		auth2 := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID,
			Enabled:       false, // Different value
			CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/second", communityDID),
			RecordCID:     "bagauth2",
		}
		if err := aggRepo.CreateAuthorization(ctx, auth2); err != nil {
			t.Fatalf("Second authorization (update) failed: %v", err)
		}

		// Verify it was updated
		retrieved, err := aggRepo.GetAuthorization(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve authorization: %v", err)
		}

		if retrieved.Enabled {
			t.Error("Expected authorization to be disabled after update")
		}
	})
}

// TestAggregatorAuthorization_IsAuthorized tests fast authorization check
func TestAggregatorAuthorization_IsAuthorized(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")
	communityDID := generateTestDID(uniqueSuffix + "comm")

	// Setup aggregator and community
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Test Aggregator",
		CreatedAt:    time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	community := &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("!test-auth-%s@coves.local", uniqueSuffix),
		Name:         "test-auth",
		OwnerDID:     "did:web:coves.local",
		HostedByDID:  "did:web:coves.local",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if _, err := commRepo.Create(ctx, community); err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	t.Run("returns true for enabled authorization", func(t *testing.T) {
		auth := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID,
			Enabled:       true,
			CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/enabled", communityDID),
			RecordCID:     "bagauth123",
		}
		if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
			t.Fatalf("Failed to create authorization: %v", err)
		}

		authorized, err := aggRepo.IsAuthorized(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Fatalf("IsAuthorized failed: %v", err)
		}

		if !authorized {
			t.Error("Expected aggregator to be authorized")
		}
	})

	t.Run("returns false for disabled authorization", func(t *testing.T) {
		uniqueSuffix2 := fmt.Sprintf("%d", time.Now().UnixNano())
		aggregatorDID2 := generateTestDID(uniqueSuffix2 + "agg")
		communityDID2 := generateTestDID(uniqueSuffix2 + "comm")

		// Setup
		agg2 := &aggregators.Aggregator{
			DID:         aggregatorDID2,
			DisplayName: "Test Aggregator 2",
		CreatedAt:    time.Now(),
			IndexedAt:   time.Now(),
			RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID2),
			RecordCID:   "bagtest456",
		}
		if err := aggRepo.CreateAggregator(ctx, agg2); err != nil {
			t.Fatalf("Failed to create aggregator: %v", err)
		}

		community2 := &communities.Community{
			DID:          communityDID2,
			Handle:       fmt.Sprintf("!test-disabled-%s@coves.local", uniqueSuffix2),
			Name:         "test-disabled",
			OwnerDID:     "did:web:coves.local",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := commRepo.Create(ctx, community2); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Create disabled authorization
		auth := &aggregators.Authorization{
			AggregatorDID: aggregatorDID2,
			CommunityDID:  communityDID2,
			Enabled:       false,
			CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/disabled", communityDID2),
			RecordCID:     "bagauth789",
		}
		if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
			t.Fatalf("Failed to create authorization: %v", err)
		}

		authorized, err := aggRepo.IsAuthorized(ctx, aggregatorDID2, communityDID2)
		if err != nil {
			t.Fatalf("IsAuthorized failed: %v", err)
		}

		if authorized {
			t.Error("Expected aggregator to NOT be authorized (disabled)")
		}
	})

	t.Run("returns false for non-existent authorization", func(t *testing.T) {
		authorized, err := aggRepo.IsAuthorized(ctx, "did:plc:fake123", "did:plc:fake456")
		if err != nil {
			t.Fatalf("IsAuthorized failed: %v", err)
		}

		if authorized {
			t.Error("Expected non-existent authorization to return false")
		}
	})
}

// TestAggregatorService_PostCreationIntegration tests the full post creation flow with aggregators
func TestAggregatorService_PostCreationIntegration(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)

	aggService := aggregators.NewAggregatorService(aggRepo, nil) // nil community service for this test
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")
	communityDID := generateTestDID(uniqueSuffix + "comm")

	// Setup aggregator
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Test RSS Feed",
		CreatedAt:    time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	// Setup community
	community := &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("!test-post-%s@coves.local", uniqueSuffix),
		Name:         "test-post",
		OwnerDID:     "did:web:coves.local",
		HostedByDID:  "did:web:coves.local",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if _, err := commRepo.Create(ctx, community); err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	// Create authorization
	auth := &aggregators.Authorization{
		AggregatorDID: aggregatorDID,
		CommunityDID:  communityDID,
		Enabled:       true,
		CreatedBy:     "did:plc:moderator123",
		CreatedAt:     time.Now(),
		IndexedAt:     time.Now(),
		RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/test", communityDID),
		RecordCID:     "bagauth123",
	}
	if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
		t.Fatalf("Failed to create authorization: %v", err)
	}

	t.Run("validates aggregator post successfully", func(t *testing.T) {
		// This should pass (authorization exists and enabled)
		err := aggService.ValidateAggregatorPost(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Errorf("Expected validation to pass, got error: %v", err)
		}
	})

	t.Run("rejects post without authorization", func(t *testing.T) {
		fakeAggDID := generateTestDID(uniqueSuffix + "fake")
		err := aggService.ValidateAggregatorPost(ctx, fakeAggDID, communityDID)
		if !aggregators.IsUnauthorized(err) {
			t.Errorf("Expected unauthorized error, got: %v", err)
		}
	})

	t.Run("records aggregator post for rate limiting", func(t *testing.T) {
		postURI := fmt.Sprintf("at://%s/social.coves.post.record/post1", communityDID)

		err := aggRepo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, postURI, "bafy123")
		if err != nil {
			t.Fatalf("Failed to record post: %v", err)
		}

		// Count recent posts
		since := time.Now().Add(-1 * time.Hour)
		count, err := aggRepo.CountRecentPosts(ctx, aggregatorDID, communityDID, since)
		if err != nil {
			t.Fatalf("Failed to count posts: %v", err)
		}

		if count != 1 {
			t.Errorf("Expected 1 post, got %d", count)
		}
	})
}

// TestAggregatorService_RateLimiting tests rate limit enforcement
func TestAggregatorService_RateLimiting(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)

	aggService := aggregators.NewAggregatorService(aggRepo, nil)
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")
	communityDID := generateTestDID(uniqueSuffix + "comm")

	// Setup
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Rate Limited Aggregator",
		CreatedAt:    time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	community := &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("!test-ratelimit-%s@coves.local", uniqueSuffix),
		Name:         "test-ratelimit",
		OwnerDID:     "did:web:coves.local",
		HostedByDID:  "did:web:coves.local",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if _, err := commRepo.Create(ctx, community); err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	auth := &aggregators.Authorization{
		AggregatorDID: aggregatorDID,
		CommunityDID:  communityDID,
		Enabled:       true,
		CreatedBy:     "did:plc:moderator123",
		CreatedAt:     time.Now(),
		IndexedAt:     time.Now(),
		RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/test", communityDID),
		RecordCID:     "bagauth123",
	}
	if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
		t.Fatalf("Failed to create authorization: %v", err)
	}

	t.Run("allows posts within rate limit", func(t *testing.T) {
		// Create 9 posts (under the 10/hour limit)
		for i := 0; i < 9; i++ {
			postURI := fmt.Sprintf("at://%s/social.coves.post.record/post%d", communityDID, i)
			if err := aggRepo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, postURI, "bafy123"); err != nil {
				t.Fatalf("Failed to record post %d: %v", i, err)
			}
		}

		// Should still pass validation (9 < 10)
		err := aggService.ValidateAggregatorPost(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Errorf("Expected validation to pass with 9 posts, got error: %v", err)
		}
	})

	t.Run("enforces rate limit at 10 posts/hour", func(t *testing.T) {
		// Add one more post to hit the limit (total = 10)
		postURI := fmt.Sprintf("at://%s/social.coves.post.record/post10", communityDID)
		if err := aggRepo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, postURI, "bafy123"); err != nil {
			t.Fatalf("Failed to record 10th post: %v", err)
		}

		// Now should fail (10 >= 10)
		err := aggService.ValidateAggregatorPost(ctx, aggregatorDID, communityDID)
		if !aggregators.IsRateLimited(err) {
			t.Errorf("Expected rate limit error after 10 posts, got: %v", err)
		}
	})
}

// TestAggregatorPostService_Integration tests the posts service integration
func TestAggregatorPostService_Integration(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	aggService := aggregators.NewAggregatorService(aggRepo, nil)
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")
	userDID := generateTestDID(uniqueSuffix + "user")

	// Create aggregator
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Test Aggregator",
		CreatedAt:    time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	t.Run("identifies aggregator DID correctly", func(t *testing.T) {
		isAgg, err := aggService.IsAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("IsAggregator failed: %v", err)
		}
		if !isAgg {
			t.Error("Expected DID to be identified as aggregator")
		}
	})

	t.Run("identifies regular user DID correctly", func(t *testing.T) {
		isAgg, err := aggService.IsAggregator(ctx, userDID)
		if err != nil {
			t.Fatalf("IsAggregator failed: %v", err)
		}
		if isAgg {
			t.Error("Expected user DID to NOT be identified as aggregator")
		}
	})
}

// TestAggregatorTriggers tests database triggers for auto-updating stats
func TestAggregatorTriggers(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")

	// Create aggregator
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Trigger Test Aggregator",
		CreatedAt:    time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	t.Run("communities_using count updates via trigger", func(t *testing.T) {
		// Create 3 communities and authorize aggregator for each
		for i := 0; i < 3; i++ {
			commSuffix := fmt.Sprintf("%s%d", uniqueSuffix, i)
			communityDID := generateTestDID(commSuffix + "comm")

			community := &communities.Community{
				DID:          communityDID,
				Handle:       fmt.Sprintf("!trigger-test-%s@coves.local", commSuffix),
				Name:         fmt.Sprintf("trigger-test-%d", i),
				OwnerDID:     "did:web:coves.local",
				HostedByDID:  "did:web:coves.local",
				Visibility:   "public",
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			}
			if _, err := commRepo.Create(ctx, community); err != nil {
				t.Fatalf("Failed to create community %d: %v", i, err)
			}

			auth := &aggregators.Authorization{
				AggregatorDID: aggregatorDID,
				CommunityDID:  communityDID,
				Enabled:       true,
				CreatedBy:     "did:plc:moderator123",
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
				RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/auth%d", communityDID, i),
				RecordCID:     fmt.Sprintf("bagauth%d", i),
			}
			if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
				t.Fatalf("Failed to create authorization %d: %v", i, err)
			}
		}

		// Retrieve aggregator and check communities_using count
		retrieved, err := aggRepo.GetAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("Failed to retrieve aggregator: %v", err)
		}

		if retrieved.CommunitiesUsing != 3 {
			t.Errorf("Expected communities_using = 3, got %d", retrieved.CommunitiesUsing)
		}
	})

	t.Run("posts_created count updates via trigger", func(t *testing.T) {
		communityDID := generateTestDID(uniqueSuffix + "postcomm")

		// Create community
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!post-trigger-%s@coves.local", uniqueSuffix),
			Name:         "post-trigger",
			OwnerDID:     "did:web:coves.local",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := commRepo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Record 5 posts
		for i := 0; i < 5; i++ {
			postURI := fmt.Sprintf("at://%s/social.coves.post.record/triggerpost%d", communityDID, i)
			if err := aggRepo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, postURI, "bafy123"); err != nil {
				t.Fatalf("Failed to record post %d: %v", i, err)
			}
		}

		// Retrieve aggregator and check posts_created count
		retrieved, err := aggRepo.GetAggregator(ctx, aggregatorDID)
		if err != nil {
			t.Fatalf("Failed to retrieve aggregator: %v", err)
		}

		// Note: posts_created accumulates across all tests, so check >= 5
		if retrieved.PostsCreated < 5 {
			t.Errorf("Expected posts_created >= 5, got %d", retrieved.PostsCreated)
		}
	})
}

// TestAggregatorAuthorization_DisabledAtField tests that disabledAt is properly stored and retrieved
func TestAggregatorAuthorization_DisabledAtField(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	aggRepo := postgres.NewAggregatorRepository(db)
	commRepo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aggregatorDID := generateTestDID(uniqueSuffix + "agg")
	communityDID := generateTestDID(uniqueSuffix + "comm")

	// Create aggregator
	agg := &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Disabled Test Aggregator",
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   fmt.Sprintf("at://%s/social.coves.aggregator.service/self", aggregatorDID),
		RecordCID:   "bagtest123",
	}
	if err := aggRepo.CreateAggregator(ctx, agg); err != nil {
		t.Fatalf("Failed to create aggregator: %v", err)
	}

	// Create community
	community := &communities.Community{
		DID:         communityDID,
		Handle:      fmt.Sprintf("!disabled-test-%s@coves.local", uniqueSuffix),
		Name:        "disabled-test",
		OwnerDID:    "did:plc:owner123",
		HostedByDID: "did:web:coves.local",
		Visibility:  "public",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if _, err := commRepo.Create(ctx, community); err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	t.Run("stores and retrieves disabledAt timestamp for audit trail", func(t *testing.T) {
		disabledTime := time.Now().UTC().Truncate(time.Microsecond)

		// Create authorization with disabledAt set
		auth := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID,
			Enabled:       false,
			CreatedBy:     "did:plc:moderator123",
			DisabledBy:    "did:plc:moderator456",
			DisabledAt:    &disabledTime, // Pointer to time.Time for nullable field
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/test", communityDID),
			RecordCID:     "bagauth123",
		}

		if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
			t.Fatalf("Failed to create authorization: %v", err)
		}

		// Retrieve and verify disabledAt is stored
		retrieved, err := aggRepo.GetAuthorization(ctx, aggregatorDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve authorization: %v", err)
		}

		if retrieved.DisabledAt == nil {
			t.Fatal("Expected disabledAt to be set, got nil")
		}

		// Compare timestamps (truncate to microseconds for postgres precision)
		if !retrieved.DisabledAt.Truncate(time.Microsecond).Equal(disabledTime) {
			t.Errorf("Expected disabledAt %v, got %v", disabledTime, *retrieved.DisabledAt)
		}

		if retrieved.DisabledBy != "did:plc:moderator456" {
			t.Errorf("Expected disabledBy 'did:plc:moderator456', got %s", retrieved.DisabledBy)
		}
	})

	t.Run("handles nil disabledAt for enabled authorizations", func(t *testing.T) {
		uniqueSuffix2 := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID2 := generateTestDID(uniqueSuffix2 + "comm2")

		// Create another community
		community2 := &communities.Community{
			DID:         communityDID2,
			Handle:      fmt.Sprintf("!enabled-test-%s@coves.local", uniqueSuffix2),
			Name:        "enabled-test",
			OwnerDID:    "did:plc:owner123",
			HostedByDID: "did:web:coves.local",
			Visibility:  "public",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if _, err := commRepo.Create(ctx, community2); err != nil {
			t.Fatalf("Failed to create community2: %v", err)
		}

		// Create enabled authorization without disabledAt
		auth := &aggregators.Authorization{
			AggregatorDID: aggregatorDID,
			CommunityDID:  communityDID2,
			Enabled:       true,
			CreatedBy:     "did:plc:moderator123",
			DisabledAt:    nil, // Explicitly nil for enabled authorization
			CreatedAt:     time.Now(),
			IndexedAt:     time.Now(),
			RecordURI:     fmt.Sprintf("at://%s/social.coves.aggregator.authorization/test2", communityDID2),
			RecordCID:     "bagauth456",
		}

		if err := aggRepo.CreateAuthorization(ctx, auth); err != nil {
			t.Fatalf("Failed to create authorization: %v", err)
		}

		// Retrieve and verify disabledAt is nil
		retrieved, err := aggRepo.GetAuthorization(ctx, aggregatorDID, communityDID2)
		if err != nil {
			t.Fatalf("Failed to retrieve authorization: %v", err)
		}

		if retrieved.DisabledAt != nil {
			t.Errorf("Expected disabledAt to be nil for enabled authorization, got %v", *retrieved.DisabledAt)
		}
	})
}
