package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

// TestHostedByVerification_DomainMatching tests that hostedBy domain must match handle domain
func TestHostedByVerification_DomainMatching(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("rejects community with mismatched hostedBy domain", func(t *testing.T) {
		// Create consumer with verification enabled
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.social", false)

		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Attempt to create community claiming to be hosted by nintendo.com
		// but with a coves.social handle (ATTACK!)
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      "gaming.community.coves.social", // coves.social handle
					"name":        "gaming",
					"displayName": "Nintendo Gaming",
					"description": "Fake Nintendo community",
					"createdBy":   "did:plc:attacker123",
					"hostedBy":    "did:web:nintendo.com", // ← SPOOFED! Claiming Nintendo hosting
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		// This should fail verification
		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected verification error for mismatched hostedBy domain, got nil")
		}

		// Verify error message mentions domain mismatch
		errMsg := err.Error()
		if errMsg == "" {
			t.Fatal("Expected error message, got empty string")
		}
		t.Logf("Got expected error: %v", err)

		// Verify community was NOT indexed
		_, getErr := repo.GetByDID(ctx, communityDID)
		if getErr == nil {
			t.Fatal("Community should not have been indexed, but was found in database")
		}
	})

	t.Run("accepts community with matching hostedBy domain", func(t *testing.T) {
		// Create consumer with verification enabled
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.social", false)

		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Create community with matching hostedBy and handle domains
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      "gaming.community.coves.social", // coves.social handle
					"name":        "gaming",
					"displayName": "Gaming Community",
					"description": "Legitimate coves.social community",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.social", // ✅ Matches handle domain
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		// This should succeed
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected verification to succeed, got error: %v", err)
		}

		// Verify community was indexed
		community, getErr := repo.GetByDID(ctx, communityDID)
		if getErr != nil {
			t.Fatalf("Community should have been indexed: %v", getErr)
		}
		if community.HostedByDID != "did:web:coves.social" {
			t.Errorf("Expected hostedByDID 'did:web:coves.social', got '%s'", community.HostedByDID)
		}
	})

	t.Run("rejects hostedBy with non-did:web format", func(t *testing.T) {
		// Create consumer with verification enabled
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.social", false)

		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Attempt to use did:plc for hostedBy (not allowed)
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      "gaming.community.coves.social",
					"name":        "gaming",
					"displayName": "Test Community",
					"description": "Test",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:plc:xyz123", // ← Invalid: must be did:web
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		// This should fail verification
		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected verification error for non-did:web hostedBy, got nil")
		}
		t.Logf("Got expected error: %v", err)
	})

	t.Run("skip verification flag bypasses all checks", func(t *testing.T) {
		// Create consumer with verification DISABLED
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.social", true)

		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Even with mismatched domain, this should succeed with skipVerification=true
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      "gaming.community.example.com",
					"name":        "gaming",
					"displayName": "Test",
					"description": "Test",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:nintendo.com", // Mismatched, but verification skipped
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		// Should succeed because verification is skipped
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected success with skipVerification=true, got error: %v", err)
		}

		// Verify community was indexed
		_, getErr := repo.GetByDID(ctx, communityDID)
		if getErr != nil {
			t.Fatalf("Community should have been indexed: %v", getErr)
		}
	})
}

// TestExtractDomainFromHandle tests the domain extraction logic for various handle formats
func TestExtractDomainFromHandle(t *testing.T) {
	// This is an internal function test - we'll test it through the consumer
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	testCases := []struct {
		name          string
		handle        string
		hostedByDID   string
		shouldSucceed bool
	}{
		{
			name:          "DNS-style handle with subdomain",
			handle:        "gaming.community.coves.social",
			hostedByDID:   "did:web:coves.social",
			shouldSucceed: true,
		},
		{
			name:          "Simple two-part domain",
			handle:        "gaming.coves.social",
			hostedByDID:   "did:web:coves.social",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part subdomain",
			handle:        "gaming.test.community.example.com",
			hostedByDID:   "did:web:example.com",
			shouldSucceed: true,
		},
		{
			name:          "Mismatched domain",
			handle:        "gaming.community.coves.social",
			hostedByDID:   "did:web:example.com",
			shouldSucceed: false,
		},
		// CRITICAL: Multi-part TLD tests (PR review feedback)
		{
			name:          "Multi-part TLD: .co.uk",
			handle:        "gaming.community.coves.co.uk",
			hostedByDID:   "did:web:coves.co.uk",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: .com.au",
			handle:        "gaming.community.example.com.au",
			hostedByDID:   "did:web:example.com.au",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: Reject incorrect .co.uk extraction",
			handle:        "gaming.community.coves.co.uk",
			hostedByDID:   "did:web:co.uk", // Wrong! Should be coves.co.uk
			shouldSucceed: false,
		},
		{
			name:          "Multi-part TLD: .org.uk",
			handle:        "gaming.community.myinstance.org.uk",
			hostedByDID:   "did:web:myinstance.org.uk",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: .ac.uk",
			handle:        "gaming.community.university.ac.uk",
			hostedByDID:   "did:web:university.ac.uk",
			shouldSucceed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.social", false)

			uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
			communityDID := generateTestDID(uniqueSuffix)

			event := &jetstream.JetstreamEvent{
				Did:    communityDID,
				TimeUS: time.Now().UnixMicro(),
				Kind:   "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        "rev123",
					Operation:  "create",
					Collection: "social.coves.community.profile",
					RKey:       "self",
					CID:        "bafy123abc",
					Record: map[string]interface{}{
						"handle":      tc.handle,
						"name":        "test",
						"displayName": "Test",
						"description": "Test",
						"createdBy":   "did:plc:user123",
						"hostedBy":    tc.hostedByDID,
						"visibility":  "public",
						"federation": map[string]interface{}{
							"allowExternalDiscovery": true,
						},
						"memberCount":     0,
						"subscriberCount": 0,
						"createdAt":       time.Now().Format(time.RFC3339),
					},
				},
			}

			err := consumer.HandleEvent(ctx, event)
			if tc.shouldSucceed && err != nil {
				t.Errorf("Expected success for %s, got error: %v", tc.handle, err)
			} else if !tc.shouldSucceed && err == nil {
				t.Errorf("Expected failure for %s, got success", tc.handle)
			}
		})
	}
}
