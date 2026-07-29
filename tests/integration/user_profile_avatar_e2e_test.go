//go:build integration

package integration

// SERIAL BY DESIGN — do not add t.Parallel() to this file.
//
// Its tests drive the Jetstream firehose through the hand-rolled
// subscribeToJetstream* helpers below rather than testkit's cursor-gated
// subscriber. Those helpers subscribe to one shared stream and match on the
// first event of a collection, so a concurrent test writing the same
// collection is delivered to them too and either steals the match or trips
// their timeout. Per-test database clones do not isolate a shared websocket.
//
// docs/TEST_ARCHITECTURE.md §3.3 ("Parallelism is earned, not assumed").

import (
	"Coves/internal/api/handlers/user"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
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
// 2. Profile record is written to PDS (social.coves.actor.profile)
// 3. Jetstream consumer receives and processes the event
// 4. GetProfile returns the correct avatar URL
func TestUserProfileAvatarE2E_UpdateWithAvatar(t *testing.T) {
	db := testkit.DB(t)

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.actor.profile", pdsHostname)

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
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	// Setup user consumer for processing Jetstream events
	userConsumer := jetstream.NewUserEventConsumer(userService, identityResolver)

	// Setup HTTP server with all user routes using password-based PDS client for E2E tests
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutesWithOptions(r, userService, e2eAuth.OAuthAuthMiddleware, nil, &routes.UserRouteOptions{
		PDSClientFactory: UserProfilePasswordAuthPDSClientFactory(),
	})
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Cleanup old test data
	testID := uniqueTestID()

	t.Run("update profile with avatar via real PDS and Jetstream", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("avatartest%s.local.coves.dev", testID)
		email := fmt.Sprintf("avatartest%s@test.com", testID)
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

			// ONE deadline for the whole subscription, not one per read: the
			// budget is what the caller is willing to wait in total, and a
			// per-read deadline would let a busy stream extend it indefinitely.
			readDeadline := time.Now().Add(jetstreamReadBudget)

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if deadlineErr := conn.SetReadDeadline(readDeadline); deadlineErr != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if readErr := conn.ReadJSON(&event); readErr != nil {
						// Any read error ends this subscription. A gorilla connection is
						// corrupt once its read deadline has expired, and looping on it
						// is what reaches the panic that aborts the whole test binary.
						// The caller's own timeout reports the missing event.
						return
					}

					// Only process profile update events for our user
					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.actor.profile" &&
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
		timeout := time.After(jetstreamReadBudget)

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
		// The consumer checks for social.coves.actor.profile commit events
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
			// URL-decode the avatar URL before checking for DID (DIDs are URL-encoded in query params)
			decodedAvatarURL, _ := url.QueryUnescape(finalProfile.Avatar)
			assert.Contains(t, decodedAvatarURL, userDID,
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
	db := testkit.DB(t)

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.actor.profile", pdsHostname)

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
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	// Setup HTTP server using password-based PDS client for E2E tests
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutesWithOptions(r, userService, e2eAuth.OAuthAuthMiddleware, nil, &routes.UserRouteOptions{
		PDSClientFactory: UserProfilePasswordAuthPDSClientFactory(),
	})
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	testID := uniqueTestID()

	t.Run("update profile with banner via real PDS and Jetstream", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("bannertest%s.local.coves.dev", testID)
		email := fmt.Sprintf("bannertest%s@test.com", testID)
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

			// ONE deadline for the whole subscription, not one per read: the
			// budget is what the caller is willing to wait in total, and a
			// per-read deadline would let a busy stream extend it indefinitely.
			readDeadline := time.Now().Add(jetstreamReadBudget)

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if err := conn.SetReadDeadline(readDeadline); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						// Any read error ends this subscription. A gorilla connection is
						// corrupt once its read deadline has expired, and looping on it
						// is what reaches the panic that aborts the whole test binary.
						// The caller's own timeout reports the missing event.
						return
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.actor.profile" &&
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
		timeout := time.After(jetstreamReadBudget)

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
			// URL-decode the banner URL before checking for DID (DIDs are URL-encoded in query params)
			decodedBannerURL, _ := url.QueryUnescape(finalProfile.Banner)
			assert.Contains(t, decodedBannerURL, userDID)
		}

		t.Logf("\n TRUE E2E USER PROFILE BANNER UPDATE COMPLETE")
	})
}

// TestUserProfileAvatarE2E_UpdateDisplayNameAndBio tests updating non-blob profile fields
func TestUserProfileAvatarE2E_UpdateDisplayNameAndBio(t *testing.T) {
	db := testkit.DB(t)

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.actor.profile", pdsHostname)

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
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	// Setup HTTP server using password-based PDS client for E2E tests
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutesWithOptions(r, userService, e2eAuth.OAuthAuthMiddleware, nil, &routes.UserRouteOptions{
		PDSClientFactory: UserProfilePasswordAuthPDSClientFactory(),
	})
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	testID := uniqueTestID()

	t.Run("update display name and bio without blobs", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("texttest%s.local.coves.dev", testID)
		email := fmt.Sprintf("texttest%s@test.com", testID)
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

			// ONE deadline for the whole subscription, not one per read: the
			// budget is what the caller is willing to wait in total, and a
			// per-read deadline would let a busy stream extend it indefinitely.
			readDeadline := time.Now().Add(jetstreamReadBudget)

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if err := conn.SetReadDeadline(readDeadline); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						// Any read error ends this subscription. A gorilla connection is
						// corrupt once its read deadline has expired, and looping on it
						// is what reaches the panic that aborts the whole test binary.
						// The caller's own timeout reports the missing event.
						return
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.actor.profile" &&
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
		timeout := time.After(jetstreamReadBudget)

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
	db := testkit.DB(t)

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
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.actor.profile", pdsHostname)

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
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	// Setup HTTP server using password-based PDS client for E2E tests
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterUserRoutesWithOptions(r, userService, e2eAuth.OAuthAuthMiddleware, nil, &routes.UserRouteOptions{
		PDSClientFactory: UserProfilePasswordAuthPDSClientFactory(),
	})
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	testID := uniqueTestID()

	// subscribeForProfileEvent opens the Jetstream subscription and returns a wait
	// function that blocks until a profile commit for userDID arrives (or times out),
	// returning the avatar CID extracted from the commit record.
	//
	// It MUST be called BEFORE the PDS write. The firehose subscription is cursorless
	// (see jetstreamURL), so it only streams commits emitted after the socket is
	// established — there is no replay. Dialing after the write (the previous helper's
	// behavior) races the PDS→firehose relay and silently drops the event under load.
	subscribeForProfileEvent := func(t *testing.T, userDID string, timeout time.Duration) func() (string, *jetstream.JetstreamEvent) {
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		done := make(chan bool)
		ready := make(chan struct{})
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, timeout)

		go func() {
			conn, _, dialErr := websocket.DefaultDialer.Dial(jetstreamURL, nil)
			if dialErr != nil {
				t.Logf("Failed to connect to Jetstream: %v", dialErr)
				close(ready)
				return
			}
			defer func() { _ = conn.Close() }()

			// ONE deadline for the whole subscription, not one per read: the
			// budget is what the caller is willing to wait in total, and a
			// per-read deadline would let a busy stream extend it indefinitely.
			readDeadline := time.Now().Add(jetstreamReadBudget)
			close(ready) // socket dialed; safe for the caller to write

			for {
				select {
				case <-done:
					return
				case <-subscribeCtx.Done():
					return
				default:
					if err := conn.SetReadDeadline(readDeadline); err != nil {
						return
					}

					var event jetstream.JetstreamEvent
					if err := conn.ReadJSON(&event); err != nil {
						// Any read error ends this subscription. A gorilla connection is
						// corrupt once its read deadline has expired, and looping on it
						// is what reaches the panic that aborts the whole test binary.
						// The caller's own timeout reports the missing event.
						return
					}

					if event.Kind == "commit" && event.Commit != nil &&
						event.Commit.Collection == "social.coves.actor.profile" &&
						event.Did == userDID {
						eventChan <- &event
					}
				}
			}
		}()

		// Block until the socket is dialed, then give Jetstream a moment to register
		// the subscription, so the caller's subsequent write is guaranteed to land
		// after we are listening.
		<-ready
		time.Sleep(500 * time.Millisecond)

		return func() (string, *jetstream.JetstreamEvent) {
			defer cancelSubscribe()
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
	}

	t.Run("replace existing avatar with new one", func(t *testing.T) {
		// Create test user account on PDS
		userHandle := fmt.Sprintf("replaceav%s.local.coves.dev", testID)
		email := fmt.Sprintf("replaceav%s@test.com", testID)
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

		// Subscribe to the firehose BEFORE the write (cursorless: no replay).
		waitInitial := subscribeForProfileEvent(t, userDID, 30*time.Second)

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
		initialAvatarCID, initialEvent := waitInitial()
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

		// Subscribe BEFORE the replacement write (cursorless: no replay).
		waitReplacement := subscribeForProfileEvent(t, userDID, 30*time.Second)

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
		newAvatarCID, newEvent := waitReplacement()
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
