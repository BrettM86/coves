package integration

import (
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommunityIdentifierResolution tests all formats accepted by ResolveCommunityIdentifier
func TestCommunityIdentifierResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	// Get configuration from environment
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001" // Default to dev PDS port (see .env.dev)
	}

	instanceDomain := os.Getenv("INSTANCE_DOMAIN")
	if instanceDomain == "" {
		instanceDomain = "coves.social"
	}

	// Create provisioner (signature: instanceDomain, pdsURL)
	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)

	// Create service
	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:" + instanceDomain
	}

	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	// Create a test community to resolve
	uniqueName := fmt.Sprintf("test%d", time.Now().UnixNano()%1000000)
	req := communities.CreateCommunityRequest{
		Name:                   uniqueName,
		DisplayName:            "Test Community",
		Description:            "A test community for identifier resolution",
		Visibility:             "public",
		CreatedByDID:           "did:plc:testowner123",
		HostedByDID:            instanceDID,
		AllowExternalDiscovery: true,
	}

	community, err := service.CreateCommunity(ctx, req)
	require.NoError(t, err, "Failed to create test community")
	require.NotNil(t, community)

	t.Run("DID format", func(t *testing.T) {
		t.Run("resolves valid DID", func(t *testing.T) {
			did, err := service.ResolveCommunityIdentifier(ctx, community.DID)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("rejects non-existent DID", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "did:plc:nonexistent123")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "community not found")
		})

		t.Run("rejects malformed DID", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "did:invalid")
			require.Error(t, err)
		})
	})

	t.Run("Canonical handle format", func(t *testing.T) {
		t.Run("resolves lowercase canonical handle", func(t *testing.T) {
			did, err := service.ResolveCommunityIdentifier(ctx, community.Handle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("resolves uppercase canonical handle (case-insensitive)", func(t *testing.T) {
			// Use actual community handle in uppercase
			upperHandle := fmt.Sprintf("c-%s.%s", uniqueName, strings.ToUpper(instanceDomain))
			did, err := service.ResolveCommunityIdentifier(ctx, upperHandle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("rejects non-existent canonical handle", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, fmt.Sprintf("c-nonexistent.%s", instanceDomain))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "community not found")
		})
	})

	t.Run("At-identifier format", func(t *testing.T) {
		t.Run("resolves @-prefixed handle", func(t *testing.T) {
			atHandle := "@" + community.Handle
			did, err := service.ResolveCommunityIdentifier(ctx, atHandle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("resolves @-prefixed handle with uppercase (case-insensitive)", func(t *testing.T) {
			atHandle := "@" + fmt.Sprintf("c-%s.%s", uniqueName, strings.ToUpper(instanceDomain))
			did, err := service.ResolveCommunityIdentifier(ctx, atHandle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})
	})

	t.Run("Scoped format (!name@instance)", func(t *testing.T) {
		t.Run("resolves valid scoped identifier", func(t *testing.T) {
			scopedID := fmt.Sprintf("!%s@%s", uniqueName, instanceDomain)
			did, err := service.ResolveCommunityIdentifier(ctx, scopedID)
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("resolves uppercase scoped identifier (case-insensitive domain)", func(t *testing.T) {
			scopedID := fmt.Sprintf("!%s@%s", uniqueName, strings.ToUpper(instanceDomain))
			did, err := service.ResolveCommunityIdentifier(ctx, scopedID)
			require.NoError(t, err, "Should normalize uppercase domain to lowercase")
			assert.Equal(t, community.DID, did)
		})

		t.Run("resolves mixed-case scoped identifier", func(t *testing.T) {
			// Mix case of domain
			mixedDomain := ""
			for i, c := range instanceDomain {
				if i%2 == 0 {
					mixedDomain += strings.ToUpper(string(c))
				} else {
					mixedDomain += strings.ToLower(string(c))
				}
			}
			scopedID := fmt.Sprintf("!%s@%s", uniqueName, mixedDomain)
			did, err := service.ResolveCommunityIdentifier(ctx, scopedID)
			require.NoError(t, err, "Should normalize all parts to lowercase")
			assert.Equal(t, community.DID, did)
		})

		t.Run("rejects scoped identifier without @ symbol", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "!testcommunity")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must include @ symbol")
		})

		t.Run("rejects scoped identifier with empty name", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, fmt.Sprintf("!@%s", instanceDomain))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "community name cannot be empty")
		})

		t.Run("rejects scoped identifier with wrong instance", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "!testcommunity@wrong.social")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not hosted on this instance")
		})

		t.Run("rejects non-existent community in scoped format", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, fmt.Sprintf("!nonexistent@%s", instanceDomain))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "community not found")
		})
	})

	t.Run("Edge cases", func(t *testing.T) {
		t.Run("rejects empty identifier", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "")
			require.Error(t, err)
		})

		t.Run("rejects whitespace-only identifier", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "   ")
			require.Error(t, err)
		})

		t.Run("handles leading/trailing whitespace in valid identifier", func(t *testing.T) {
			did, err := service.ResolveCommunityIdentifier(ctx, "  "+community.Handle+"  ")
			require.NoError(t, err)
			assert.Equal(t, community.DID, did)
		})

		t.Run("rejects identifier without dots (not a valid handle)", func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, "nodots")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be a DID, handle, or scoped identifier")
		})
	})
}

// TestResolveScopedIdentifier_InputValidation tests input sanitization
func TestResolveScopedIdentifier_InputValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3000"
	}

	instanceDomain := os.Getenv("INSTANCE_DOMAIN")
	if instanceDomain == "" {
		instanceDomain = "coves.social"
	}

	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:" + instanceDomain
	}

	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	tests := []struct {
		name        string
		identifier  string
		expectError string
	}{
		{
			name:        "rejects special characters in name",
			identifier:  fmt.Sprintf("!<script>@%s", instanceDomain),
			expectError: "valid DNS label",
		},
		{
			name:        "rejects name with spaces",
			identifier:  fmt.Sprintf("!test community@%s", instanceDomain),
			expectError: "valid DNS label",
		},
		{
			name:        "rejects name starting with hyphen",
			identifier:  fmt.Sprintf("!-test@%s", instanceDomain),
			expectError: "valid DNS label",
		},
		{
			name:        "rejects name ending with hyphen",
			identifier:  fmt.Sprintf("!test-@%s", instanceDomain),
			expectError: "valid DNS label",
		},
		{
			name:        "rejects name exceeding 63 characters",
			identifier:  "!" + string(make([]byte, 64)) + "@" + instanceDomain,
			expectError: "valid DNS label",
		},
		{
			name:        "accepts valid name with hyphens",
			identifier:  fmt.Sprintf("!test-community@%s", instanceDomain),
			expectError: "", // Should create successfully or fail on not found
		},
		{
			name:        "accepts valid name with numbers",
			identifier:  fmt.Sprintf("!test123@%s", instanceDomain),
			expectError: "", // Should create successfully or fail on not found
		},
		{
			name:        "rejects invalid domain format",
			identifier:  "!test@not a domain",
			expectError: "invalid",
		},
		{
			name:        "rejects domain with special characters",
			identifier:  "!test@coves$.social",
			expectError: "invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, tt.identifier)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				// Either succeeds or fails with "not found" (not a validation error)
				if err != nil {
					assert.Contains(t, err.Error(), "not found")
				}
			}
		})
	}
}

// TestGetDisplayHandle tests the GetDisplayHandle method
func TestGetDisplayHandle(t *testing.T) {
	tests := []struct {
		name            string
		handle          string
		expectedDisplay string
	}{
		{
			name:            "standard two-part domain",
			handle:          "c-gardening.coves.social",
			expectedDisplay: "!gardening@coves.social",
		},
		{
			name:            "multi-part TLD",
			handle:          "c-gaming.coves.co.uk",
			expectedDisplay: "!gaming@coves.co.uk",
		},
		{
			name:            "subdomain instance",
			handle:          "c-test.dev.coves.social",
			expectedDisplay: "!test@dev.coves.social",
		},
		{
			name:            "single part name",
			handle:          "c-a.coves.social",
			expectedDisplay: "!a@coves.social",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a community struct and set the handle
			community := &communities.Community{
				Handle: tt.handle,
			}

			// Test GetDisplayHandle
			displayHandle := community.GetDisplayHandle()
			assert.Equal(t, tt.expectedDisplay, displayHandle)
		})
	}

	t.Run("handles malformed input gracefully", func(t *testing.T) {
		// Test edge cases
		testCases := []struct {
			handle   string
			fallback string
			desc     string
		}{
			{"nodots", "nodots", "No dots - should return as-is"},
			{"single.dot", "single.dot", "Single dot without c- prefix - should return as-is"},
			{"", "", "Empty - should return as-is"},
			{"c-", "c-", "Prefix only, no name - should return as-is"},
			{"c-.", "c-.", "Prefix with empty name - should return as-is"},
			{"c-.coves.social", "c-.coves.social", "Prefix with dot but no name - should return as-is"},
			{"c-nodot", "c-nodot", "Prefix but no dot after name - should return as-is"},
		}

		for _, tc := range testCases {
			community := &communities.Community{
				Handle: tc.handle,
			}
			result := community.GetDisplayHandle()
			assert.Equal(t, tc.fallback, result, "Should fallback to original handle for: %s (%s)", tc.handle, tc.desc)
		}
	})
}

// TestIdentifierResolution_ErrorContext verifies error messages include identifier context
func TestIdentifierResolution_ErrorContext(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3000"
	}

	instanceDomain := os.Getenv("INSTANCE_DOMAIN")
	if instanceDomain == "" {
		instanceDomain = "coves.social"
	}

	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:" + instanceDomain
	}

	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	t.Run("DID error includes identifier", func(t *testing.T) {
		testDID := "did:plc:nonexistent999"
		_, err := service.ResolveCommunityIdentifier(ctx, testDID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "community not found")
		assert.Contains(t, err.Error(), testDID) // Should include the DID in error
	})

	t.Run("handle error includes identifier", func(t *testing.T) {
		testHandle := fmt.Sprintf("c-nonexistent.%s", instanceDomain)
		_, err := service.ResolveCommunityIdentifier(ctx, testHandle)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "community not found")
		assert.Contains(t, err.Error(), testHandle) // Should include the handle in error
	})

	t.Run("scoped identifier error includes validation details", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "!test@wrong.instance")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not hosted on this instance")
		assert.Contains(t, err.Error(), instanceDomain) // Should mention expected instance
	})
}

// TestGetCommunity_IdentifierResolution tests all formats accepted by GetCommunity
// This is distinct from ResolveCommunityIdentifier - GetCommunity returns the full Community object
func TestGetCommunity_IdentifierResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	instanceDomain := os.Getenv("INSTANCE_DOMAIN")
	if instanceDomain == "" {
		instanceDomain = "coves.social"
	}

	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:" + instanceDomain
	}

	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	// Create a test community
	uniqueName := fmt.Sprintf("gettest%d", time.Now().UnixNano()%1000000)
	req := communities.CreateCommunityRequest{
		Name:                   uniqueName,
		DisplayName:            "GetCommunity Test",
		Description:            "Testing GetCommunity identifier resolution",
		Visibility:             "public",
		CreatedByDID:           "did:plc:testowner456",
		HostedByDID:            instanceDID,
		AllowExternalDiscovery: true,
	}

	community, err := service.CreateCommunity(ctx, req)
	require.NoError(t, err, "Failed to create test community")
	require.NotNil(t, community)

	t.Run("DID format", func(t *testing.T) {
		t.Run("returns community for valid DID", func(t *testing.T) {
			result, err := service.GetCommunity(ctx, community.DID)
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
			assert.Equal(t, community.Handle, result.Handle)
		})

		t.Run("error includes identifier for non-existent DID", func(t *testing.T) {
			testDID := "did:plc:nonexistent789"
			_, err := service.GetCommunity(ctx, testDID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testDID)
		})
	})

	t.Run("Canonical handle format", func(t *testing.T) {
		t.Run("returns community for canonical handle", func(t *testing.T) {
			result, err := service.GetCommunity(ctx, community.Handle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
		})

		t.Run("case-insensitive handle resolution", func(t *testing.T) {
			upperHandle := strings.ToUpper(community.Handle)
			result, err := service.GetCommunity(ctx, upperHandle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
		})

		t.Run("error includes identifier for non-existent handle", func(t *testing.T) {
			testHandle := fmt.Sprintf("c-nonexistent.%s", instanceDomain)
			_, err := service.GetCommunity(ctx, testHandle)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testHandle)
		})
	})

	t.Run("At-identifier format", func(t *testing.T) {
		t.Run("strips @ prefix and returns community", func(t *testing.T) {
			atHandle := "@" + community.Handle
			result, err := service.GetCommunity(ctx, atHandle)
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
		})
	})

	t.Run("Scoped identifier format", func(t *testing.T) {
		t.Run("resolves scoped identifier and returns community", func(t *testing.T) {
			scopedID := fmt.Sprintf("!%s@%s", uniqueName, instanceDomain)
			result, err := service.GetCommunity(ctx, scopedID)
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
		})

		t.Run("error includes identifier for non-existent scoped ID", func(t *testing.T) {
			scopedID := fmt.Sprintf("!nonexistent@%s", instanceDomain)
			_, err := service.GetCommunity(ctx, scopedID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), scopedID)
		})
	})

	t.Run("Edge cases", func(t *testing.T) {
		t.Run("rejects empty identifier", func(t *testing.T) {
			_, err := service.GetCommunity(ctx, "")
			require.Error(t, err)
		})

		t.Run("trims whitespace from identifier", func(t *testing.T) {
			result, err := service.GetCommunity(ctx, "  "+community.Handle+"  ")
			require.NoError(t, err)
			assert.Equal(t, community.DID, result.DID)
		})

		t.Run("rejects identifier without dots", func(t *testing.T) {
			_, err := service.GetCommunity(ctx, "nodots")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be a DID, handle, or scoped identifier")
		})
	})
}
