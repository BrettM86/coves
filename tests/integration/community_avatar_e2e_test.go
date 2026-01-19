package integration

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// createTestPNGImage creates a simple PNG image for testing
// Panics on encoding error since this is a test helper and encoding should never fail
// for simple in-memory images
func createTestPNGImage(width, height int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(fmt.Sprintf("createTestPNGImage: failed to encode PNG: %v", err))
	}
	return buf.Bytes()
}

// TestCommunityAvatarE2E_CreateWithAvatar tests creating a community with an avatar
// Flow: CreateCommunity(avatar) → PDS uploadBlob + putRecord → Jetstream → Consumer → AppView
func TestCommunityAvatarE2E_CreateWithAvatar(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	// Run migrations
	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is running
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()
	t.Logf("✅ Jetstream available at %s", jetstreamURL)

	ctx := context.Background()
	instanceDID := "did:web:coves.social"

	// Setup identity resolver with local PLC
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	communityRepo := postgres.NewCommunityRepository(db)
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil, // No custom PDS factory, uses built-in
		blobService,
	)

	consumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, true, identityResolver)

	t.Run("create community with avatar via real Jetstream", func(t *testing.T) {
		uniqueName := fmt.Sprintf("avt%d", time.Now().UnixNano()%1000000)
		creatorDID := "did:plc:avatar-create-test"

		// Create a test PNG image (100x100 red square)
		avatarData := createTestPNGImage(100, 100, color.RGBA{255, 0, 0, 255})
		t.Logf("Created test avatar image: %d bytes", len(avatarData))

		// Subscribe to Jetstream BEFORE creating the community
		// This ensures we catch the create event
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 30*time.Second)
		defer cancelSubscribe()

		// We don't know the DID yet, so we'll filter by collection and match after
		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
				t.Logf("Failed to connect to Jetstream: %v", dialErr)
				return
			}
			defer func() { _ = conn.Close() }()

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if deadlineErr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if readErr := conn.ReadJSON(&event); readErr != nil {
						continue // Timeout or error, keep trying
					}

					// Only process community profile create events
					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.community.profile" &&
						event.Commit.Operation == "create" {
						eventChan <- &event
					}
				}
			}
		}()
		time.Sleep(500 * time.Millisecond) // Give subscriber time to connect

		t.Logf("\n📝 Creating community with avatar on PDS...")
		community, createErr := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Community With Avatar",
			Description:            "Testing avatar upload on create",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
			AvatarBlob:             avatarData,
			AvatarMimeType:         "image/png",
		})
		if createErr != nil {
			close(done)
			t.Fatalf("Failed to create community with avatar: %v", createErr)
		}

		t.Logf("✅ Community created on PDS:")
		t.Logf("   DID: %s", community.DID)
		t.Logf("   RecordCID: %s", community.RecordCID)
		t.Logf("   AvatarCID (from service): %s", community.AvatarCID)

		// Wait for REAL Jetstream event
		t.Logf("\n⏳ Waiting for create event from Jetstream...")
		var realEvent *jetstream.JetstreamEvent
		timeout := time.After(15 * time.Second)

	eventLoop:
		for {
			select {
			case event := <-eventChan:
				// Match by DID (we now know it)
				if event.Did == community.DID {
					realEvent = event
					t.Logf("✅ Received REAL create event from Jetstream!")
					t.Logf("   DID: %s", event.Did)
					t.Logf("   Operation: %s", event.Commit.Operation)
					t.Logf("   CID: %s", event.Commit.CID)

					// Log avatar info from real event
					if event.Commit.Record != nil {
						if avatar, hasAvatar := event.Commit.Record["avatar"]; hasAvatar {
							t.Logf("   Avatar in event: %v", avatar)
						}
					}
					break eventLoop
				}
			case <-timeout:
				close(done)
				t.Fatalf("Timeout waiting for Jetstream create event for DID %s", community.DID)
			}
		}
		close(done)

		// Process the REAL event through consumer
		// Note: The community already exists (service indexed it), so consumer will hit conflict
		// But this tests that the real event has correct avatar data
		t.Logf("\n🔄 Processing real Jetstream event through consumer...")
		if handleErr := consumer.HandleEvent(ctx, realEvent); handleErr != nil {
			t.Logf("   Note: Consumer conflict expected (already indexed): %v", handleErr)
		}

		// Verify avatar CID matches what's in the database
		final, err := communityRepo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		t.Logf("\n✅ Community avatar verification:")
		t.Logf("   AvatarCID in DB: %s", final.AvatarCID)

		if final.AvatarCID == "" {
			t.Errorf("Expected AvatarCID to be set after create with avatar")
		}

		// Verify the avatar CID from the real Jetstream event matches what we stored
		if realEvent.Commit.Record != nil {
			if avatar, hasAvatar := realEvent.Commit.Record["avatar"].(map[string]interface{}); hasAvatar {
				if ref, hasRef := avatar["ref"].(map[string]interface{}); hasRef {
					if link, hasLink := ref["$link"].(string); hasLink {
						t.Logf("   AvatarCID from Jetstream: %s", link)
						if final.AvatarCID != link {
							t.Errorf("AvatarCID mismatch: DB has %s, Jetstream has %s", final.AvatarCID, link)
						} else {
							t.Logf("   ✅ AvatarCID matches between DB and Jetstream event!")
						}
					}
				}
			}
		}

		// Verify we can fetch the avatar from PDS
		pdsResp, pdsErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.profile&rkey=self",
			pdsURL, community.DID))
		if pdsErr != nil {
			t.Fatalf("Failed to fetch profile record from PDS: %v", pdsErr)
		}
		defer func() { _ = pdsResp.Body.Close() }()

		if pdsResp.StatusCode != http.StatusOK {
			t.Fatalf("Profile record not found on PDS: status %d", pdsResp.StatusCode)
		}
		t.Logf("   ✅ Profile record with avatar exists on PDS")

		t.Logf("\n✅ TRUE E2E AVATAR CREATE FLOW COMPLETE:")
		t.Logf("   Service → PDS uploadBlob → PDS putRecord → Jetstream → Verified ✓")
	})
}

// TestCommunityAvatarE2E_UpdateWithAvatar tests updating a community's avatar
// Flow: UpdateCommunity(avatar) → PDS uploadBlob + putRecord → Jetstream → Consumer → AppView
func TestCommunityAvatarE2E_UpdateWithAvatar(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	// Run migrations
	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is running - REQUIRED for true E2E
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()
	t.Logf("✅ Jetstream available at %s", jetstreamURL)

	ctx := context.Background()
	instanceDID := "did:web:coves.social"

	// Setup identity resolver with local PLC
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	communityRepo := postgres.NewCommunityRepository(db)
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil,
		blobService,
	)

	consumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, true, identityResolver)

	// Helper to wait for Jetstream update event and process it
	waitForUpdateEvent := func(t *testing.T, communityDID string, timeout time.Duration) *jetstream.JetstreamEvent {
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, timeout)
		defer cancelSubscribe()

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
				t.Logf("Failed to connect to Jetstream: %v", dialErr)
				return
			}
			defer func() { _ = conn.Close() }()

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if deadlineErr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if readErr := conn.ReadJSON(&event); readErr != nil {
						continue
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.community.profile" &&
						event.Commit.Operation == "update" &&
						event.Did == communityDID {
						eventChan <- &event
					}
				}
			}
		}()

		select {
		case event := <-eventChan:
			close(done)
			return event
		case <-time.After(timeout):
			close(done)
			return nil
		}
	}

	t.Run("add avatar to community without one", func(t *testing.T) {
		uniqueName := fmt.Sprintf("upav%d", time.Now().UnixNano()%1000000)
		creatorDID := "did:plc:avatar-update-test"

		// Create a community WITHOUT an avatar
		t.Logf("\n📝 Creating community without avatar...")
		community, createErr := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Community Without Avatar",
			Description:            "Will add avatar via update",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
		})
		if createErr != nil {
			t.Fatalf("Failed to create community: %v", createErr)
		}
		t.Logf("✅ Community created: DID=%s", community.DID)

		// Verify no avatar initially
		initial, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed: %v", err)
		}
		if initial.AvatarCID != "" {
			t.Fatalf("Expected no initial avatar, got: %s", initial.AvatarCID)
		}
		t.Logf("   Initial AvatarCID: '' (confirmed empty)")

		// Create test avatar image (100x100 blue square)
		avatarData := createTestPNGImage(100, 100, color.RGBA{0, 0, 255, 255})
		t.Logf("\n📝 Updating community with avatar (%d bytes)...", len(avatarData))

		// Start listening for Jetstream event
		eventReceived := make(chan *jetstream.JetstreamEvent, 1)
		go func() {
			event := waitForUpdateEvent(t, community.DID, 15*time.Second)
			eventReceived <- event
		}()
		time.Sleep(500 * time.Millisecond) // Give subscriber time to connect

		// Perform the update with avatar
		newDisplayName := "Community With New Avatar"
		updated, updateErr := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
			CommunityDID:   community.DID,
			UpdatedByDID:   creatorDID,
			DisplayName:    &newDisplayName,
			AvatarBlob:     avatarData,
			AvatarMimeType: "image/png",
		})
		if updateErr != nil {
			t.Fatalf("Failed to update community with avatar: %v", updateErr)
		}

		t.Logf("✅ Community update written to PDS:")
		t.Logf("   New RecordCID: %s", updated.RecordCID)

		// Wait for REAL Jetstream event
		t.Logf("\n⏳ Waiting for update event from Jetstream...")
		realEvent := <-eventReceived
		if realEvent == nil {
			t.Fatalf("Timeout waiting for Jetstream update event")
		}

		t.Logf("✅ Received REAL update event from Jetstream!")
		t.Logf("   Operation: %s", realEvent.Commit.Operation)
		t.Logf("   CID: %s", realEvent.Commit.CID)

		// Extract avatar CID from real event
		var avatarCIDFromEvent string
		if realEvent.Commit.Record != nil {
			if avatar, hasAvatar := realEvent.Commit.Record["avatar"].(map[string]interface{}); hasAvatar {
				t.Logf("   Avatar in event: %v", avatar)
				if ref, hasRef := avatar["ref"].(map[string]interface{}); hasRef {
					if link, hasLink := ref["$link"].(string); hasLink {
						avatarCIDFromEvent = link
						t.Logf("   AvatarCID from Jetstream: %s", avatarCIDFromEvent)
					}
				}
			}
		}

		// Process the REAL event through consumer
		t.Logf("\n🔄 Processing real Jetstream event through consumer...")
		if handleErr := consumer.HandleEvent(ctx, realEvent); handleErr != nil {
			t.Logf("   Consumer error: %v", handleErr)
		}

		// Verify avatar CID is now set in DB
		final, err := communityRepo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		t.Logf("\n✅ Community avatar update verified:")
		t.Logf("   DisplayName: %s", final.DisplayName)
		t.Logf("   AvatarCID in DB: %s", final.AvatarCID)

		if final.AvatarCID == "" {
			t.Errorf("Expected AvatarCID to be set after update")
		}

		// Verify DB matches Jetstream event
		if avatarCIDFromEvent != "" && final.AvatarCID != avatarCIDFromEvent {
			t.Errorf("AvatarCID mismatch: DB has %s, Jetstream has %s", final.AvatarCID, avatarCIDFromEvent)
		} else if avatarCIDFromEvent != "" {
			t.Logf("   ✅ AvatarCID matches between DB and Jetstream!")
		}

		t.Logf("\n✅ TRUE E2E ADD AVATAR FLOW COMPLETE")
	})

	t.Run("replace existing avatar with new one", func(t *testing.T) {
		uniqueName := fmt.Sprintf("rpav%d", time.Now().UnixNano()%1000000)
		creatorDID := "did:plc:avatar-replace-test"

		// Create a community WITH an initial avatar (red square)
		initialAvatarData := createTestPNGImage(100, 100, color.RGBA{255, 0, 0, 255})
		t.Logf("\n📝 Creating community with initial avatar (red, %d bytes)...", len(initialAvatarData))

		community, createErr := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Community With Initial Avatar",
			Description:            "Will replace avatar",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
			AvatarBlob:             initialAvatarData,
			AvatarMimeType:         "image/png",
		})
		if createErr != nil {
			t.Fatalf("Failed to create community with avatar: %v", createErr)
		}
		t.Logf("✅ Community created: DID=%s", community.DID)

		// Verify initial avatar is set
		initial, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed: %v", err)
		}
		initialAvatarCID := initial.AvatarCID
		if initialAvatarCID == "" {
			t.Fatalf("Expected initial avatar to be set")
		}
		t.Logf("   Initial AvatarCID: %s", initialAvatarCID)

		// Create NEW avatar image (100x100 green square - different from initial red)
		newAvatarData := createTestPNGImage(100, 100, color.RGBA{0, 255, 0, 255})
		t.Logf("\n📝 Replacing avatar with new one (green, %d bytes)...", len(newAvatarData))

		// Start listening for Jetstream event
		eventReceived := make(chan *jetstream.JetstreamEvent, 1)
		go func() {
			event := waitForUpdateEvent(t, community.DID, 15*time.Second)
			eventReceived <- event
		}()
		time.Sleep(500 * time.Millisecond)

		// Perform the update with NEW avatar
		newDisplayName := "Community With Replaced Avatar"
		updated, updateErr := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
			CommunityDID:   community.DID,
			UpdatedByDID:   creatorDID,
			DisplayName:    &newDisplayName,
			AvatarBlob:     newAvatarData,
			AvatarMimeType: "image/png",
		})
		if updateErr != nil {
			t.Fatalf("Failed to update community with new avatar: %v", updateErr)
		}

		t.Logf("✅ Community update written to PDS:")
		t.Logf("   New RecordCID: %s", updated.RecordCID)

		// Wait for REAL Jetstream event
		t.Logf("\n⏳ Waiting for update event from Jetstream...")
		realEvent := <-eventReceived
		if realEvent == nil {
			t.Fatalf("Timeout waiting for Jetstream update event")
		}

		t.Logf("✅ Received REAL update event from Jetstream!")
		t.Logf("   Operation: %s", realEvent.Commit.Operation)

		// Extract new avatar CID from real event
		var newAvatarCIDFromEvent string
		if realEvent.Commit.Record != nil {
			if avatar, hasAvatar := realEvent.Commit.Record["avatar"].(map[string]interface{}); hasAvatar {
				if ref, hasRef := avatar["ref"].(map[string]interface{}); hasRef {
					if link, hasLink := ref["$link"].(string); hasLink {
						newAvatarCIDFromEvent = link
						t.Logf("   New AvatarCID from Jetstream: %s", newAvatarCIDFromEvent)
					}
				}
			}
		}

		// Process the REAL event through consumer
		t.Logf("\n🔄 Processing real Jetstream event through consumer...")
		if handleErr := consumer.HandleEvent(ctx, realEvent); handleErr != nil {
			t.Logf("   Consumer error: %v", handleErr)
		}

		// Verify avatar CID has CHANGED
		final, err := communityRepo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		t.Logf("\n✅ Community avatar replacement verified:")
		t.Logf("   DisplayName: %s", final.DisplayName)
		t.Logf("   Old AvatarCID: %s", initialAvatarCID)
		t.Logf("   New AvatarCID: %s", final.AvatarCID)

		if final.AvatarCID == "" {
			t.Errorf("Expected AvatarCID to be set after replacement")
		}

		if final.AvatarCID == initialAvatarCID {
			t.Errorf("AvatarCID should have changed after replacement! Old: %s, New: %s", initialAvatarCID, final.AvatarCID)
		} else {
			t.Logf("   ✅ AvatarCID successfully changed!")
		}

		// Verify DB matches Jetstream event
		if newAvatarCIDFromEvent != "" && final.AvatarCID != newAvatarCIDFromEvent {
			t.Errorf("AvatarCID mismatch: DB has %s, Jetstream has %s", final.AvatarCID, newAvatarCIDFromEvent)
		} else if newAvatarCIDFromEvent != "" {
			t.Logf("   ✅ New AvatarCID matches between DB and Jetstream!")
		}

		t.Logf("\n✅ TRUE E2E REPLACE AVATAR FLOW COMPLETE")
	})
}

// TestCommunityAvatarE2E_UpdateWithBanner tests updating a community's banner
// Flow: UpdateCommunity(banner) → PDS uploadBlob + putRecord → Jetstream → Consumer → AppView
func TestCommunityAvatarE2E_UpdateWithBanner(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	// Run migrations
	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is running - REQUIRED for true E2E
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()
	t.Logf("✅ Jetstream available at %s", jetstreamURL)

	ctx := context.Background()
	instanceDID := "did:web:coves.social"

	// Setup identity resolver with local PLC
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	communityRepo := postgres.NewCommunityRepository(db)
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil,
		blobService,
	)

	consumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, true, identityResolver)

	// Helper to wait for Jetstream update event and process it
	waitForUpdateEvent := func(t *testing.T, communityDID string, timeout time.Duration) *jetstream.JetstreamEvent {
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, timeout)
		defer cancelSubscribe()

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
				t.Logf("Failed to connect to Jetstream: %v", dialErr)
				return
			}
			defer func() { _ = conn.Close() }()

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if deadlineErr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if readErr := conn.ReadJSON(&event); readErr != nil {
						continue
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.community.profile" &&
						event.Commit.Operation == "update" &&
						event.Did == communityDID {
						eventChan <- &event
					}
				}
			}
		}()

		select {
		case event := <-eventChan:
			close(done)
			return event
		case <-time.After(timeout):
			close(done)
			return nil
		}
	}

	t.Run("add banner to community without one", func(t *testing.T) {
		uniqueName := fmt.Sprintf("ban%d", time.Now().UnixNano()%1000000)
		creatorDID := "did:plc:banner-add-test"

		// Create a community WITHOUT a banner
		t.Logf("\n📝 Creating community without banner...")
		community, createErr := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Community Without Banner",
			Description:            "Will add banner via update",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
		})
		if createErr != nil {
			t.Fatalf("Failed to create community: %v", createErr)
		}
		t.Logf("✅ Community created: DID=%s", community.DID)

		// Verify no banner initially
		initial, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed: %v", err)
		}
		if initial.BannerCID != "" {
			t.Fatalf("Expected no initial banner, got: %s", initial.BannerCID)
		}
		t.Logf("   Initial BannerCID: '' (confirmed empty)")

		// Create test banner image (300x100 green rectangle)
		bannerData := createTestPNGImage(300, 100, color.RGBA{0, 255, 0, 255})
		t.Logf("\n📝 Updating community with banner (%d bytes)...", len(bannerData))

		// Start listening for Jetstream event
		eventReceived := make(chan *jetstream.JetstreamEvent, 1)
		go func() {
			event := waitForUpdateEvent(t, community.DID, 15*time.Second)
			eventReceived <- event
		}()
		time.Sleep(500 * time.Millisecond) // Give subscriber time to connect

		// Perform the update with banner
		newDisplayName := "Community With New Banner"
		updated, updateErr := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
			CommunityDID:   community.DID,
			UpdatedByDID:   creatorDID,
			DisplayName:    &newDisplayName,
			BannerBlob:     bannerData,
			BannerMimeType: "image/png",
		})
		if updateErr != nil {
			t.Fatalf("Failed to update community with banner: %v", updateErr)
		}

		t.Logf("✅ Community update written to PDS:")
		t.Logf("   New RecordCID: %s", updated.RecordCID)

		// Wait for REAL Jetstream event
		t.Logf("\n⏳ Waiting for update event from Jetstream...")
		realEvent := <-eventReceived
		if realEvent == nil {
			t.Fatalf("Timeout waiting for Jetstream update event")
		}

		t.Logf("✅ Received REAL update event from Jetstream!")
		t.Logf("   Operation: %s", realEvent.Commit.Operation)
		t.Logf("   CID: %s", realEvent.Commit.CID)

		// Extract banner CID from real event
		var bannerCIDFromEvent string
		if realEvent.Commit.Record != nil {
			if banner, hasBanner := realEvent.Commit.Record["banner"].(map[string]interface{}); hasBanner {
				t.Logf("   Banner in event: %v", banner)
				if ref, hasRef := banner["ref"].(map[string]interface{}); hasRef {
					if link, hasLink := ref["$link"].(string); hasLink {
						bannerCIDFromEvent = link
						t.Logf("   BannerCID from Jetstream: %s", bannerCIDFromEvent)
					}
				}
			}
		}

		// Process the REAL event through consumer
		t.Logf("\n🔄 Processing real Jetstream event through consumer...")
		if handleErr := consumer.HandleEvent(ctx, realEvent); handleErr != nil {
			t.Logf("   Consumer error: %v", handleErr)
		}

		// Verify banner CID is now set in DB
		final, err := communityRepo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		t.Logf("\n✅ Community banner update verified:")
		t.Logf("   DisplayName: %s", final.DisplayName)
		t.Logf("   BannerCID in DB: %s", final.BannerCID)

		if final.BannerCID == "" {
			t.Errorf("Expected BannerCID to be set after update")
		}

		// Verify DB matches Jetstream event
		if bannerCIDFromEvent != "" && final.BannerCID != bannerCIDFromEvent {
			t.Errorf("BannerCID mismatch: DB has %s, Jetstream has %s", final.BannerCID, bannerCIDFromEvent)
		} else if bannerCIDFromEvent != "" {
			t.Logf("   ✅ BannerCID matches between DB and Jetstream!")
		}

		t.Logf("\n✅ TRUE E2E ADD BANNER FLOW COMPLETE")
	})

	t.Run("replace existing banner with new one", func(t *testing.T) {
		uniqueName := fmt.Sprintf("rpban%d", time.Now().UnixNano()%1000000)
		creatorDID := "did:plc:banner-replace-test"

		// Create a community WITH an initial banner (red rectangle)
		initialBannerData := createTestPNGImage(300, 100, color.RGBA{255, 0, 0, 255})
		t.Logf("\n📝 Creating community with initial banner (red, %d bytes)...", len(initialBannerData))

		community, createErr := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Community With Initial Banner",
			Description:            "Will replace banner",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
			BannerBlob:             initialBannerData,
			BannerMimeType:         "image/png",
		})
		if createErr != nil {
			t.Fatalf("Failed to create community with banner: %v", createErr)
		}
		t.Logf("✅ Community created: DID=%s", community.DID)

		// Verify initial banner is set
		initial, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed: %v", err)
		}
		initialBannerCID := initial.BannerCID
		if initialBannerCID == "" {
			t.Fatalf("Expected initial banner to be set")
		}
		t.Logf("   Initial BannerCID: %s", initialBannerCID)

		// Create NEW banner image (300x100 blue rectangle - different from initial red)
		newBannerData := createTestPNGImage(300, 100, color.RGBA{0, 0, 255, 255})
		t.Logf("\n📝 Replacing banner with new one (blue, %d bytes)...", len(newBannerData))

		// Start listening for Jetstream event
		eventReceived := make(chan *jetstream.JetstreamEvent, 1)
		go func() {
			event := waitForUpdateEvent(t, community.DID, 15*time.Second)
			eventReceived <- event
		}()
		time.Sleep(500 * time.Millisecond)

		// Perform the update with NEW banner
		newDisplayName := "Community With Replaced Banner"
		updated, updateErr := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
			CommunityDID:   community.DID,
			UpdatedByDID:   creatorDID,
			DisplayName:    &newDisplayName,
			BannerBlob:     newBannerData,
			BannerMimeType: "image/png",
		})
		if updateErr != nil {
			t.Fatalf("Failed to update community with new banner: %v", updateErr)
		}

		t.Logf("✅ Community update written to PDS:")
		t.Logf("   New RecordCID: %s", updated.RecordCID)

		// Wait for REAL Jetstream event
		t.Logf("\n⏳ Waiting for update event from Jetstream...")
		realEvent := <-eventReceived
		if realEvent == nil {
			t.Fatalf("Timeout waiting for Jetstream update event")
		}

		t.Logf("✅ Received REAL update event from Jetstream!")
		t.Logf("   Operation: %s", realEvent.Commit.Operation)

		// Extract new banner CID from real event
		var newBannerCIDFromEvent string
		if realEvent.Commit.Record != nil {
			if banner, hasBanner := realEvent.Commit.Record["banner"].(map[string]interface{}); hasBanner {
				if ref, hasRef := banner["ref"].(map[string]interface{}); hasRef {
					if link, hasLink := ref["$link"].(string); hasLink {
						newBannerCIDFromEvent = link
						t.Logf("   New BannerCID from Jetstream: %s", newBannerCIDFromEvent)
					}
				}
			}
		}

		// Process the REAL event through consumer
		t.Logf("\n🔄 Processing real Jetstream event through consumer...")
		if handleErr := consumer.HandleEvent(ctx, realEvent); handleErr != nil {
			t.Logf("   Consumer error: %v", handleErr)
		}

		// Verify banner CID has CHANGED
		final, err := communityRepo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		t.Logf("\n✅ Community banner replacement verified:")
		t.Logf("   DisplayName: %s", final.DisplayName)
		t.Logf("   Old BannerCID: %s", initialBannerCID)
		t.Logf("   New BannerCID: %s", final.BannerCID)

		if final.BannerCID == "" {
			t.Errorf("Expected BannerCID to be set after replacement")
		}

		if final.BannerCID == initialBannerCID {
			t.Errorf("BannerCID should have changed after replacement! Old: %s, New: %s", initialBannerCID, final.BannerCID)
		} else {
			t.Logf("   ✅ BannerCID successfully changed!")
		}

		// Verify DB matches Jetstream event
		if newBannerCIDFromEvent != "" && final.BannerCID != newBannerCIDFromEvent {
			t.Errorf("BannerCID mismatch: DB has %s, Jetstream has %s", final.BannerCID, newBannerCIDFromEvent)
		} else if newBannerCIDFromEvent != "" {
			t.Logf("   ✅ New BannerCID matches between DB and Jetstream!")
		}

		t.Logf("\n✅ TRUE E2E REPLACE BANNER FLOW COMPLETE")
	})
}
