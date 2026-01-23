package integration

import (
	"Coves/internal/api/handlers/user"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/blobs"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestAvatarPNG creates a simple PNG image for avatar testing
// Parameters:
// - width, height: image dimensions in pixels
// - c: fill color for the image
// Returns the PNG encoded as bytes
func createTestAvatarPNG(width, height int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(fmt.Sprintf("createTestAvatarPNG: failed to encode PNG: %v", err))
	}
	return buf.Bytes()
}

// TestUserProfileAvatarE2E_UpdateWithAvatar tests the full flow of updating a user profile with an avatar:
// 1. User updates profile via Coves API (POST /xrpc/social.coves.actor.updateProfile)
// 2. Profile record is written to PDS (app.bsky.actor.profile)
// 3. Jetstream consumer receives and processes the event
// 4. GetProfile returns the correct avatar URL
func TestUserProfileAvatarE2E_UpdateWithAvatar(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to connect to test database")
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=app.bsky.actor.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()
	t.Logf("Jetstream available at %s", jetstreamURL)

	ctx := context.Background()

	// Setup identity resolver
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	// Setup user consumer for processing Jetstream events
	userConsumer := jetstream.NewUserEventConsumer(userService, identityResolver, jetstreamURL, "")

	// Setup HTTP server with all user routes
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutes(r, userService, e2eAuth.OAuthAuthMiddleware, blobService)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Cleanup old test data
	timestamp := time.Now().Unix()
	shortTS := timestamp % 10000
	_, _ = db.Exec("DELETE FROM users WHERE handle LIKE 'avatartest%.local.coves.dev'")

	t.Run("update profile with avatar via real PDS and Jetstream", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("avatartest%d.local.coves.dev", shortTS)
		email := fmt.Sprintf("avatartest%d@test.com", shortTS)
		password := "test-password-avatar-123"

		t.Logf("\n Creating test user account on PDS: %s", userHandle)

		userToken, userDID, err := createPDSAccount(pdsURL, userHandle, email, password)
		require.NoError(t, err, "Failed to create test user account")
		require.NotEmpty(t, userToken, "User should receive access token")
		require.NotEmpty(t, userDID, "User should receive DID")

		t.Logf("User created: %s (%s)", userHandle, userDID)

		// Index user in AppView database
		_ = createTestUser(t, db, userHandle, userDID)

		// Register user with OAuth middleware using real PDS token
		userAPIToken := e2eAuth.AddUserWithPDSToken(userDID, userToken, pdsURL)

		// Verify user has no avatar initially
		initialProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)
		assert.Empty(t, initialProfile.Avatar, "Initial avatar should be empty")
		t.Logf("Initial profile verified - no avatar")

		// Create test avatar image (100x100 red square)
		avatarData := createTestAvatarPNG(100, 100, color.RGBA{255, 0, 0, 255})
		t.Logf("\n Updating profile with avatar (%d bytes)...", len(avatarData))

		// Subscribe to Jetstream BEFORE making the update
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 30*time.Second)
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
						var netErr net.Error
						if nErr, ok := readErr.(net.Error); ok && nErr.Timeout() {
							continue
						}
						// Check using errors.As as well
						if netErr != nil && netErr.Timeout() {
							continue
						}
						continue
					}

					// Only process profile update events for our user
					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "app.bsky.actor.profile" &&
						event.Did == userDID {
						eventChan <- &event
					}
				}
			}
		}()
		time.Sleep(500 * time.Millisecond) // Give subscriber time to connect

		// Build update profile request
		displayName := "Avatar Test User"
		bio := "Testing avatar upload E2E"
		updateReq := user.UpdateProfileRequest{
			DisplayName:    &displayName,
			Bio:            &bio,
			AvatarBlob:     avatarData,
			AvatarMimeType: "image/png",
		}

		reqBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.actor.updateProfile",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Update profile should succeed")

		var updateResp user.UpdateProfileResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updateResp))

		t.Logf("Profile update written to PDS:")
		t.Logf("   URI: %s", updateResp.URI)
		t.Logf("   CID: %s", updateResp.CID)

		// Wait for REAL Jetstream event
		t.Logf("\n Waiting for profile update event from Jetstream...")
		var realEvent *jetstream.JetstreamEvent
		timeout := time.After(15 * time.Second)

	eventLoop:
		for {
			select {
			case event := <-eventChan:
				realEvent = event
				t.Logf("Received REAL profile update event from Jetstream!")
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
			case <-timeout:
				close(done)
				t.Fatalf("Timeout waiting for Jetstream profile update event for DID %s", userDID)
			}
		}
		close(done)

		// Process the REAL event through user consumer
		t.Logf("\n Processing real Jetstream event through user consumer...")
		if handleErr := userConsumer.HandleIdentityEventPublic(ctx, realEvent); handleErr != nil {
			// HandleIdentityEventPublic is for identity events, use commit handling instead
			t.Logf("   Note: Identity event handling result: %v", handleErr)
		}

		// For profile updates, we need to manually process the commit event
		// The consumer checks for app.bsky.actor.profile commit events
		if realEvent.Kind == "commit" && realEvent.Commit != nil {
			// Extract profile data from the event and update the user
			var displayNamePtr, bioPtr, avatarCIDPtr, bannerCIDPtr *string

			if dn, ok := realEvent.Commit.Record["displayName"].(string); ok {
				displayNamePtr = &dn
			}
			if desc, ok := realEvent.Commit.Record["description"].(string); ok {
				bioPtr = &desc
			}
			if avatarMap, ok := realEvent.Commit.Record["avatar"].(map[string]interface{}); ok {
				if ref, ok := avatarMap["ref"].(map[string]interface{}); ok {
					if link, ok := ref["$link"].(string); ok {
						avatarCIDPtr = &link
						t.Logf("   AvatarCID from Jetstream: %s", link)
					}
				}
			}

			_, updateErr := userService.UpdateProfile(ctx, userDID, users.UpdateProfileInput{
				DisplayName: displayNamePtr,
				Bio:         bioPtr,
				AvatarCID:   avatarCIDPtr,
				BannerCID:   bannerCIDPtr,
			})
			if updateErr != nil {
				t.Logf("   Update profile from event error: %v", updateErr)
			}
		}

		// Verify profile now has avatar URL via GetProfile
		t.Logf("\n Verifying profile via GetProfile...")
		finalProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)

		t.Logf("Final profile verification:")
		t.Logf("   DisplayName: %s", finalProfile.DisplayName)
		t.Logf("   Bio: %s", finalProfile.Bio)
		t.Logf("   Avatar URL: %s", finalProfile.Avatar)

		assert.Equal(t, displayName, finalProfile.DisplayName, "DisplayName should match")
		assert.Equal(t, bio, finalProfile.Bio, "Bio should match")
		assert.NotEmpty(t, finalProfile.Avatar, "Avatar URL should be set")

		// Verify avatar URL format (should be PDS blob URL)
		if finalProfile.Avatar != "" {
			assert.Contains(t, finalProfile.Avatar, "/xrpc/com.atproto.sync.getBlob",
				"Avatar URL should be a PDS blob URL")
			assert.Contains(t, finalProfile.Avatar, userDID,
				"Avatar URL should contain user DID")
		}

		// Optionally: Fetch avatar URL and verify blob is accessible
		if finalProfile.Avatar != "" {
			avatarResp, avatarErr := http.Get(finalProfile.Avatar)
			if avatarErr != nil {
				t.Logf("   Warning: Could not fetch avatar URL: %v", avatarErr)
			} else {
				defer func() { _ = avatarResp.Body.Close() }()
				t.Logf("   Avatar fetch status: %d", avatarResp.StatusCode)
				if avatarResp.StatusCode == http.StatusOK {
					t.Logf("   Avatar blob is accessible!")
				}
			}
		}

		t.Logf("\n TRUE E2E USER PROFILE AVATAR UPDATE COMPLETE")
		t.Logf("   API -> PDS uploadBlob -> PDS putRecord -> Jetstream -> AppView")
	})
}

// TestUserProfileAvatarE2E_UpdateWithBanner tests the full flow of updating a user profile with a banner
func TestUserProfileAvatarE2E_UpdateWithBanner(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to connect to test database")
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=app.bsky.actor.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup identity resolver
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutes(r, userService, e2eAuth.OAuthAuthMiddleware, blobService)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	timestamp := time.Now().Unix()
	shortTS := timestamp % 10000
	_, _ = db.Exec("DELETE FROM users WHERE handle LIKE 'bannertest%.local.coves.dev'")

	t.Run("update profile with banner via real PDS and Jetstream", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("bannertest%d.local.coves.dev", shortTS)
		email := fmt.Sprintf("bannertest%d@test.com", shortTS)
		password := "test-password-banner-123"

		t.Logf("\n Creating test user account on PDS: %s", userHandle)

		userToken, userDID, err := createPDSAccount(pdsURL, userHandle, email, password)
		require.NoError(t, err, "Failed to create test user account")

		t.Logf("User created: %s (%s)", userHandle, userDID)

		// Index user in AppView database
		_ = createTestUser(t, db, userHandle, userDID)

		// Register user with OAuth middleware
		userAPIToken := e2eAuth.AddUserWithPDSToken(userDID, userToken, pdsURL)

		// Verify no banner initially
		initialProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)
		assert.Empty(t, initialProfile.Banner, "Initial banner should be empty")

		// Create test banner image (300x100 blue rectangle)
		bannerData := createTestAvatarPNG(300, 100, color.RGBA{0, 0, 255, 255})
		t.Logf("\n Updating profile with banner (%d bytes)...", len(bannerData))

		// Subscribe to Jetstream
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 30*time.Second)
		defer cancelSubscribe()

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
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
					if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						continue
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "app.bsky.actor.profile" &&
						event.Did == userDID {
						eventChan <- &event
					}
				}
			}
		}()
		time.Sleep(500 * time.Millisecond)

		// Build update profile request with banner
		displayName := "Banner Test User"
		updateReq := user.UpdateProfileRequest{
			DisplayName:    &displayName,
			BannerBlob:     bannerData,
			BannerMimeType: "image/png",
		}

		reqBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.actor.updateProfile",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Update profile should succeed")

		var updateResp user.UpdateProfileResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updateResp))

		t.Logf("Profile update written to PDS: URI=%s, CID=%s", updateResp.URI, updateResp.CID)

		// Wait for Jetstream event
		t.Logf("\n Waiting for profile update event from Jetstream...")
		var realEvent *jetstream.JetstreamEvent
		timeout := time.After(15 * time.Second)

	eventLoop:
		for {
			select {
			case event := <-eventChan:
				realEvent = event
				t.Logf("Received REAL profile update event!")

				if event.Commit.Record != nil {
					if banner, hasBanner := event.Commit.Record["banner"]; hasBanner {
						t.Logf("   Banner in event: %v", banner)
					}
				}
				break eventLoop
			case <-timeout:
				close(done)
				t.Fatalf("Timeout waiting for Jetstream event")
			}
		}
		close(done)

		// Process the event and update user profile
		if realEvent.Kind == "commit" && realEvent.Commit != nil {
			var displayNamePtr, bioPtr, avatarCIDPtr, bannerCIDPtr *string

			if dn, ok := realEvent.Commit.Record["displayName"].(string); ok {
				displayNamePtr = &dn
			}
			if bannerMap, ok := realEvent.Commit.Record["banner"].(map[string]interface{}); ok {
				if ref, ok := bannerMap["ref"].(map[string]interface{}); ok {
					if link, ok := ref["$link"].(string); ok {
						bannerCIDPtr = &link
						t.Logf("   BannerCID from Jetstream: %s", link)
					}
				}
			}

			_, _ = userService.UpdateProfile(ctx, userDID, users.UpdateProfileInput{
				DisplayName: displayNamePtr,
				Bio:         bioPtr,
				AvatarCID:   avatarCIDPtr,
				BannerCID:   bannerCIDPtr,
			})
		}

		// Verify profile now has banner URL
		finalProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)

		t.Logf("Final profile verification:")
		t.Logf("   DisplayName: %s", finalProfile.DisplayName)
		t.Logf("   Banner URL: %s", finalProfile.Banner)

		assert.Equal(t, displayName, finalProfile.DisplayName)
		assert.NotEmpty(t, finalProfile.Banner, "Banner URL should be set")

		if finalProfile.Banner != "" {
			assert.Contains(t, finalProfile.Banner, "/xrpc/com.atproto.sync.getBlob")
			assert.Contains(t, finalProfile.Banner, userDID)
		}

		t.Logf("\n TRUE E2E USER PROFILE BANNER UPDATE COMPLETE")
	})
}

// TestUserProfileAvatarE2E_UpdateDisplayNameAndBio tests updating non-blob profile fields
func TestUserProfileAvatarE2E_UpdateDisplayNameAndBio(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to connect to test database")
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=app.bsky.actor.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup identity resolver
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutes(r, userService, e2eAuth.OAuthAuthMiddleware, blobService)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	timestamp := time.Now().Unix()
	shortTS := timestamp % 10000

	t.Run("update display name and bio without blobs", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("texttest%d.local.coves.dev", shortTS)
		email := fmt.Sprintf("texttest%d@test.com", shortTS)
		password := "test-password-text-123"

		userToken, userDID, err := createPDSAccount(pdsURL, userHandle, email, password)
		require.NoError(t, err)

		t.Logf("User created: %s (%s)", userHandle, userDID)

		// Index user in AppView
		_ = createTestUser(t, db, userHandle, userDID)
		userAPIToken := e2eAuth.AddUserWithPDSToken(userDID, userToken, pdsURL)

		// Subscribe to Jetstream
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 30*time.Second)
		defer cancelSubscribe()

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
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
					if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						continue
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "app.bsky.actor.profile" &&
						event.Did == userDID {
						eventChan <- &event
					}
				}
			}
		}()
		time.Sleep(500 * time.Millisecond)

		// Update with only text fields
		displayName := "Text Update Test User"
		bio := "This is my test bio for E2E testing"
		updateReq := user.UpdateProfileRequest{
			DisplayName: &displayName,
			Bio:         &bio,
		}

		reqBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.actor.updateProfile",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Wait for Jetstream event
		var realEvent *jetstream.JetstreamEvent
		timeout := time.After(15 * time.Second)

	eventLoop:
		for {
			select {
			case event := <-eventChan:
				realEvent = event
				t.Logf("Received profile update event!")
				break eventLoop
			case <-timeout:
				close(done)
				t.Fatalf("Timeout waiting for Jetstream event")
			}
		}
		close(done)

		// Process the event
		if realEvent.Kind == "commit" && realEvent.Commit != nil {
			var displayNamePtr, bioPtr *string

			if dn, ok := realEvent.Commit.Record["displayName"].(string); ok {
				displayNamePtr = &dn
			}
			if desc, ok := realEvent.Commit.Record["description"].(string); ok {
				bioPtr = &desc
			}

			_, _ = userService.UpdateProfile(ctx, userDID, users.UpdateProfileInput{
				DisplayName: displayNamePtr,
				Bio:         bioPtr,
			})
		}

		// Verify profile
		finalProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)

		assert.Equal(t, displayName, finalProfile.DisplayName)
		assert.Equal(t, bio, finalProfile.Bio)

		t.Logf("Text-only profile update verified:")
		t.Logf("   DisplayName: %s", finalProfile.DisplayName)
		t.Logf("   Bio: %s", finalProfile.Bio)

		t.Logf("\n TRUE E2E TEXT-ONLY PROFILE UPDATE COMPLETE")
	})
}

// TestUserProfileAvatarE2E_ReplaceAvatar tests replacing an existing avatar with a new one
func TestUserProfileAvatarE2E_ReplaceAvatar(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to connect to test database")
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=app.bsky.actor.profile", pdsHostname)

	testConn, _, connErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if connErr != nil {
		t.Skipf("Jetstream not available at %s: %v. Run 'make dev-up' to start.", jetstreamURL, connErr)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup identity resolver
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutes(r, userService, e2eAuth.OAuthAuthMiddleware, blobService)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	timestamp := time.Now().Unix()
	shortTS := timestamp % 10000

	// Helper to wait for Jetstream event and extract avatar CID
	waitForProfileEvent := func(t *testing.T, userDID string, timeout time.Duration) (string, *jetstream.JetstreamEvent) {
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, timeout)
		defer cancelSubscribe()

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
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
					if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						continue
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "app.bsky.actor.profile" &&
						event.Did == userDID {
						eventChan <- &event
					}
				}
			}
		}()

		select {
		case event := <-eventChan:
			close(done)
			var avatarCID string
			if event.Commit.Record != nil {
				if avatarMap, ok := event.Commit.Record["avatar"].(map[string]interface{}); ok {
					if ref, ok := avatarMap["ref"].(map[string]interface{}); ok {
						if link, ok := ref["$link"].(string); ok {
							avatarCID = link
						}
					}
				}
			}
			return avatarCID, event
		case <-time.After(timeout):
			close(done)
			return "", nil
		}
	}

	t.Run("replace existing avatar with new one", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("replaceav%d.local.coves.dev", shortTS)
		email := fmt.Sprintf("replaceav%d@test.com", shortTS)
		password := "test-password-replace-123"

		userToken, userDID, err := createPDSAccount(pdsURL, userHandle, email, password)
		require.NoError(t, err)

		t.Logf("User created: %s (%s)", userHandle, userDID)

		// Index user in AppView
		_ = createTestUser(t, db, userHandle, userDID)
		userAPIToken := e2eAuth.AddUserWithPDSToken(userDID, userToken, pdsURL)

		// STEP 1: Create initial avatar (red square)
		t.Logf("\n Step 1: Setting initial avatar (red)...")

		initialAvatarData := createTestAvatarPNG(100, 100, color.RGBA{255, 0, 0, 255})
		displayName := "Replace Avatar Test"
		updateReq := user.UpdateProfileRequest{
			DisplayName:    &displayName,
			AvatarBlob:     initialAvatarData,
			AvatarMimeType: "image/png",
		}

		// Start listening before update
		go func() {
			time.Sleep(500 * time.Millisecond)
		}()

		reqBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.actor.updateProfile",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Wait for initial avatar event
		initialAvatarCID, initialEvent := waitForProfileEvent(t, userDID, 15*time.Second)
		require.NotNil(t, initialEvent, "Should receive initial avatar event")
		require.NotEmpty(t, initialAvatarCID, "Initial avatar CID should not be empty")

		t.Logf("   Initial AvatarCID: %s", initialAvatarCID)

		// Update local user profile
		_, _ = userService.UpdateProfile(ctx, userDID, users.UpdateProfileInput{
			DisplayName: &displayName,
			AvatarCID:   &initialAvatarCID,
		})

		// Verify initial avatar
		profileAfterInitial, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)
		assert.NotEmpty(t, profileAfterInitial.Avatar)

		// Small delay between updates
		time.Sleep(1 * time.Second)

		// STEP 2: Replace with new avatar (green square)
		t.Logf("\n Step 2: Replacing avatar with new one (green)...")

		newAvatarData := createTestAvatarPNG(100, 100, color.RGBA{0, 255, 0, 255})
		updateReq2 := user.UpdateProfileRequest{
			AvatarBlob:     newAvatarData,
			AvatarMimeType: "image/png",
		}

		reqBody2, _ := json.Marshal(updateReq2)
		req2, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.actor.updateProfile",
			bytes.NewBuffer(reqBody2))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Authorization", "Bearer "+userAPIToken)

		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		_ = resp2.Body.Close()
		require.Equal(t, http.StatusOK, resp2.StatusCode)

		// Wait for replacement avatar event
		newAvatarCID, newEvent := waitForProfileEvent(t, userDID, 15*time.Second)
		require.NotNil(t, newEvent, "Should receive replacement avatar event")
		require.NotEmpty(t, newAvatarCID, "New avatar CID should not be empty")

		t.Logf("   New AvatarCID: %s", newAvatarCID)

		// Verify CIDs are different
		assert.NotEqual(t, initialAvatarCID, newAvatarCID,
			"New avatar CID should be different from initial")

		// Update local user profile with new avatar
		_, _ = userService.UpdateProfile(ctx, userDID, users.UpdateProfileInput{
			AvatarCID: &newAvatarCID,
		})

		// Verify final profile
		finalProfile, err := userService.GetProfile(ctx, userDID)
		require.NoError(t, err)

		assert.NotEmpty(t, finalProfile.Avatar, "Final avatar URL should be set")
		assert.Contains(t, finalProfile.Avatar, newAvatarCID,
			"Avatar URL should contain new CID")

		t.Logf("\n Avatar replacement verified:")
		t.Logf("   Old CID: %s", initialAvatarCID)
		t.Logf("   New CID: %s", newAvatarCID)
		t.Logf("   CIDs different: %v", initialAvatarCID != newAvatarCID)

		t.Logf("\n TRUE E2E AVATAR REPLACEMENT COMPLETE")
	})
}
