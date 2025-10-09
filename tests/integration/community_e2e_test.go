package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"Coves/internal/api/routes"
	"Coves/internal/atproto/did"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
)

// TestCommunity_E2E is a comprehensive end-to-end test covering:
// 1. Write-forward to PDS (service layer)
// 2. Firehose consumer indexing
// 3. XRPC HTTP endpoints (create, get, list)
func TestCommunity_E2E(t *testing.T) {
	// Skip in short mode since this requires real PDS
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer db.Close()

	// Run migrations
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("Failed to set goose dialect: %v", err)
	}
	if err := goose.Up(db, "../../internal/db/migrations"); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	healthResp.Body.Close()

	// Setup dependencies
	communityRepo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")

	// Get instance credentials
	instanceHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	instancePassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if instanceHandle == "" {
		instanceHandle = "testuser123.local.coves.dev"
	}
	if instancePassword == "" {
		instancePassword = "test-password-123"
	}

	t.Logf("🔐 Authenticating with PDS as: %s", instanceHandle)

	// Authenticate to get instance DID
	accessToken, instanceDID, err := authenticateWithPDS(pdsURL, instanceHandle, instancePassword)
	if err != nil {
		t.Fatalf("Failed to authenticate with PDS: %v", err)
	}

	t.Logf("✅ Authenticated - Instance DID: %s", instanceDID)

	// Create service and consumer
	communityService := communities.NewCommunityService(communityRepo, didGen, pdsURL, instanceDID)
	if svc, ok := communityService.(interface{ SetPDSAccessToken(string) }); ok {
		svc.SetPDSAccessToken(accessToken)
	}

	consumer := jetstream.NewCommunityEventConsumer(communityRepo)

	// Setup HTTP server with XRPC routes
	r := chi.NewRouter()
	routes.RegisterCommunityRoutes(r, communityService)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	ctx := context.Background()

	// ====================================================================================
	// Part 1: Write-Forward to PDS (Service Layer)
	// ====================================================================================
	t.Run("1. Write-Forward to PDS", func(t *testing.T) {
		communityName := fmt.Sprintf("e2e-test-%d", time.Now().UnixNano())

		createReq := communities.CreateCommunityRequest{
			Name:                   communityName,
			DisplayName:            "E2E Test Community",
			Description:            "Testing full E2E flow",
			Visibility:             "public",
			CreatedByDID:           instanceDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
		}

		t.Logf("\n📝 Creating community via service: %s", communityName)
		community, err := communityService.CreateCommunity(ctx, createReq)
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		t.Logf("✅ Service returned:")
		t.Logf("   DID:        %s", community.DID)
		t.Logf("   Handle:     %s", community.Handle)
		t.Logf("   RecordURI:  %s", community.RecordURI)
		t.Logf("   RecordCID:  %s", community.RecordCID)

		// Verify DID format
		if community.DID[:8] != "did:plc:" {
			t.Errorf("Expected did:plc DID, got: %s", community.DID)
		}

		// Verify record exists in PDS
		t.Logf("\n📡 Querying PDS for the record...")

		collection := "social.coves.community.profile"
		rkey := extractRKeyFromURI(community.RecordURI)

		getRecordURL := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
			pdsURL, instanceDID, collection, rkey)

		pdsResp, err := http.Get(getRecordURL)
		if err != nil {
			t.Fatalf("Failed to query PDS: %v", err)
		}
		defer pdsResp.Body.Close()

		if pdsResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(pdsResp.Body)
			t.Fatalf("PDS returned status %d: %s", pdsResp.StatusCode, string(body))
		}

		var pdsRecord struct {
			URI   string                 `json:"uri"`
			CID   string                 `json:"cid"`
			Value map[string]interface{} `json:"value"`
		}

		if err := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); err != nil {
			t.Fatalf("Failed to decode PDS response: %v", err)
		}

		t.Logf("✅ Record found in PDS!")
		t.Logf("   URI: %s", pdsRecord.URI)
		t.Logf("   CID: %s", pdsRecord.CID)

		// Verify record has correct DIDs
		if pdsRecord.Value["did"] != community.DID {
			t.Errorf("Community DID mismatch in PDS record: expected %s, got %v",
				community.DID, pdsRecord.Value["did"])
		}

		// ====================================================================================
		// Part 2: Firehose Consumer Indexing
		// ====================================================================================
		t.Run("2. Firehose Consumer Indexing", func(t *testing.T) {
			t.Logf("\n🔄 Simulating Jetstream firehose event...")

			// Simulate firehose event (in production, this comes from Jetstream)
			firehoseEvent := jetstream.JetstreamEvent{
				Did:    instanceDID, // Repository owner (instance DID, not community DID!)
				TimeUS: time.Now().UnixMicro(),
				Kind:   "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        "test-rev",
					Operation:  "create",
					Collection: collection,
					RKey:       rkey,
					CID:        pdsRecord.CID,
					Record:     pdsRecord.Value,
				},
			}

			err := consumer.HandleEvent(ctx, &firehoseEvent)
			if err != nil {
				t.Fatalf("Failed to process firehose event: %v", err)
			}

			t.Logf("✅ Consumer processed event")

			// Verify indexed in AppView database
			t.Logf("\n🔍 Querying AppView database...")

			indexed, err := communityRepo.GetByDID(ctx, community.DID)
			if err != nil {
				t.Fatalf("Community not indexed in AppView: %v", err)
			}

			t.Logf("✅ Community indexed in AppView:")
			t.Logf("   DID:         %s", indexed.DID)
			t.Logf("   Handle:      %s", indexed.Handle)
			t.Logf("   DisplayName: %s", indexed.DisplayName)
			t.Logf("   RecordURI:   %s", indexed.RecordURI)

			// Verify record_uri points to instance repo (not community repo)
			if indexed.RecordURI[:len("at://"+instanceDID)] != "at://"+instanceDID {
				t.Errorf("record_uri should point to instance repo, got: %s", indexed.RecordURI)
			}

			t.Logf("\n✅ Part 1 & 2 Complete: Write-Forward → PDS → Firehose → AppView ✓")
		})
	})

	// ====================================================================================
	// Part 3: XRPC HTTP Endpoints
	// ====================================================================================
	t.Run("3. XRPC HTTP Endpoints", func(t *testing.T) {

		t.Run("Create via XRPC endpoint", func(t *testing.T) {
			createReq := map[string]interface{}{
				"name":                   fmt.Sprintf("xrpc-%d", time.Now().UnixNano()),
				"displayName":            "XRPC E2E Test",
				"description":            "Testing true end-to-end flow",
				"visibility":             "public",
				"createdByDid":           instanceDID,
				"hostedByDid":            instanceDID,
				"allowExternalDiscovery": true,
			}

			reqBody, _ := json.Marshal(createReq)

			// Step 1: Client POSTs to XRPC endpoint
			t.Logf("📡 Client → POST /xrpc/social.coves.community.create")
			resp, err := http.Post(
				httpServer.URL+"/xrpc/social.coves.community.create",
				"application/json",
				bytes.NewBuffer(reqBody),
			)
			if err != nil {
				t.Fatalf("Failed to POST: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
			}

			var createResp struct {
				URI    string `json:"uri"`
				CID    string `json:"cid"`
				DID    string `json:"did"`
				Handle string `json:"handle"`
			}

			json.NewDecoder(resp.Body).Decode(&createResp)

			t.Logf("✅ XRPC response received:")
			t.Logf("   DID:    %s", createResp.DID)
			t.Logf("   Handle: %s", createResp.Handle)
			t.Logf("   URI:    %s", createResp.URI)

			// Step 2: Simulate firehose consumer picking up the event
			t.Logf("🔄 Simulating Jetstream consumer indexing...")
			rkey := extractRKeyFromURI(createResp.URI)
			event := jetstream.JetstreamEvent{
				Did:    instanceDID,
				TimeUS: time.Now().UnixMicro(),
				Kind:   "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        "test-rev",
					Operation:  "create",
					Collection: "social.coves.community.profile",
					RKey:       rkey,
					Record: map[string]interface{}{
						"did":         createResp.DID,    // Community's DID from response
						"handle":      createResp.Handle, // Community's handle from response
						"name":        createReq["name"],
						"displayName": createReq["displayName"],
						"description": createReq["description"],
						"visibility":  createReq["visibility"],
						"createdBy":   createReq["createdByDid"],
						"hostedBy":    createReq["hostedByDid"],
						"federation": map[string]interface{}{
							"allowExternalDiscovery": createReq["allowExternalDiscovery"],
						},
						"createdAt": time.Now().Format(time.RFC3339),
					},
					CID: createResp.CID,
				},
			}
			consumer.HandleEvent(context.Background(), &event)

			// Step 3: Verify it's indexed in AppView
			t.Logf("🔍 Querying AppView to verify indexing...")
			var indexedCommunity communities.Community
			err = db.QueryRow(`
				SELECT did, handle, display_name, description
				FROM communities
				WHERE did = $1
			`, createResp.DID).Scan(
				&indexedCommunity.DID,
				&indexedCommunity.Handle,
				&indexedCommunity.DisplayName,
				&indexedCommunity.Description,
			)
			if err != nil {
				t.Fatalf("Community not indexed in AppView: %v", err)
			}

			t.Logf("✅ TRUE E2E FLOW COMPLETE:")
			t.Logf("   Client → XRPC → PDS → Firehose → AppView ✓")
			t.Logf("   Indexed community: %s (%s)", indexedCommunity.Handle, indexedCommunity.DisplayName)
		})

		t.Run("Get via XRPC endpoint", func(t *testing.T) {
			// Create a community first (via service, so it's indexed)
			community := createAndIndexCommunity(t, communityService, consumer, instanceDID)

			// GET via HTTP endpoint
			resp, err := http.Get(fmt.Sprintf("%s/xrpc/social.coves.community.get?community=%s",
				httpServer.URL, community.DID))
			if err != nil {
				t.Fatalf("Failed to GET: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
			}

			var getCommunity communities.Community
			json.NewDecoder(resp.Body).Decode(&getCommunity)

			t.Logf("✅ Retrieved via XRPC HTTP endpoint:")
			t.Logf("   DID:         %s", getCommunity.DID)
			t.Logf("   DisplayName: %s", getCommunity.DisplayName)

			if getCommunity.DID != community.DID {
				t.Errorf("DID mismatch: expected %s, got %s", community.DID, getCommunity.DID)
			}
		})

		t.Run("List via XRPC endpoint", func(t *testing.T) {
			// Create and index multiple communities
			for i := 0; i < 3; i++ {
				createAndIndexCommunity(t, communityService, consumer, instanceDID)
			}

			resp, err := http.Get(fmt.Sprintf("%s/xrpc/social.coves.community.list?limit=10",
				httpServer.URL))
			if err != nil {
				t.Fatalf("Failed to GET list: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
			}

			var listResp struct {
				Communities []communities.Community `json:"communities"`
				Total       int                     `json:"total"`
			}

			json.NewDecoder(resp.Body).Decode(&listResp)

			t.Logf("✅ Listed %d communities via XRPC", len(listResp.Communities))

			if len(listResp.Communities) < 3 {
				t.Errorf("Expected at least 3 communities, got %d", len(listResp.Communities))
			}
		})

		t.Logf("\n✅ Part 3 Complete: All XRPC HTTP endpoints working ✓")
	})

	divider := strings.Repeat("=", 70)
	t.Logf("\n%s", divider)
	t.Logf("✅ COMPREHENSIVE E2E TEST COMPLETE!")
	t.Logf("%s", divider)
	t.Logf("✓ Write-forward to PDS")
	t.Logf("✓ Record stored with correct DIDs (community vs instance)")
	t.Logf("✓ Firehose consumer indexes to AppView")
	t.Logf("✓ XRPC create endpoint (HTTP)")
	t.Logf("✓ XRPC get endpoint (HTTP)")
	t.Logf("✓ XRPC list endpoint (HTTP)")
	t.Logf("%s", divider)
}

// Helper: create and index a community (simulates full flow)
func createAndIndexCommunity(t *testing.T, service communities.Service, consumer *jetstream.CommunityEventConsumer, instanceDID string) *communities.Community {
	req := communities.CreateCommunityRequest{
		Name:                   fmt.Sprintf("test-%d", time.Now().UnixNano()),
		DisplayName:            "Test Community",
		Description:            "Test",
		Visibility:             "public",
		CreatedByDID:           instanceDID,
		HostedByDID:            instanceDID,
		AllowExternalDiscovery: true,
	}

	community, err := service.CreateCommunity(context.Background(), req)
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	// Fetch from PDS to get full record
	pdsURL := "http://localhost:3001"
	collection := "social.coves.community.profile"
	rkey := extractRKeyFromURI(community.RecordURI)

	pdsResp, _ := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		pdsURL, instanceDID, collection, rkey))
	defer pdsResp.Body.Close()

	var pdsRecord struct {
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}
	json.NewDecoder(pdsResp.Body).Decode(&pdsRecord)

	// Simulate firehose event
	event := jetstream.JetstreamEvent{
		Did:    instanceDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test",
			Operation:  "create",
			Collection: collection,
			RKey:       rkey,
			CID:        pdsRecord.CID,
			Record:     pdsRecord.Value,
		},
	}

	consumer.HandleEvent(context.Background(), &event)

	return community
}

func extractRKeyFromURI(uri string) string {
	// at://did/collection/rkey -> rkey
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[len(parts)-1]
	}
	return ""
}

// authenticateWithPDS authenticates with the PDS and returns access token and DID
func authenticateWithPDS(pdsURL, handle, password string) (string, string, error) {
	// Call com.atproto.server.createSession
	sessionReq := map[string]string{
		"identifier": handle,
		"password":   password,
	}

	reqBody, _ := json.Marshal(sessionReq)
	resp, err := http.Post(
		pdsURL+"/xrpc/com.atproto.server.createSession",
		"application/json",
		bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("PDS auth failed (status %d): %s", resp.StatusCode, string(body))
	}

	var sessionResp struct {
		AccessJwt string `json:"accessJwt"`
		DID       string `json:"did"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return "", "", fmt.Errorf("failed to decode session response: %w", err)
	}

	return sessionResp.AccessJwt, sessionResp.DID, nil
}
