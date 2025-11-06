package main

import (
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/auth"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/internal/core/users"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/core/comments"
	postgresRepo "Coves/internal/db/postgres"
)

func main() {
	// Database configuration (AppView database)
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		// Use dev database from .env.dev
		dbURL = "postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"
	}

	// Default PDS URL for this Coves instance (supports self-hosting)
	defaultPDS := os.Getenv("PDS_URL")
	if defaultPDS == "" {
		defaultPDS = "http://localhost:3001" // Local dev PDS
	}

	// Cursor secret for HMAC signing (prevents cursor manipulation)
	cursorSecret := os.Getenv("CURSOR_SECRET")
	if cursorSecret == "" {
		// Generate a random secret if not set (dev mode)
		// IMPORTANT: In production, set CURSOR_SECRET to a strong random value
		cursorSecret = "dev-cursor-secret-change-in-production"
		log.Println("⚠️  WARNING: Using default cursor secret. Set CURSOR_SECRET env var in production!")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("Failed to close database connection: %v", closeErr)
		}
	}()

	if err = db.Ping(); err != nil {
		log.Fatal("Failed to ping database:", err)
	}

	log.Println("Connected to AppView database")

	// Run migrations
	if err = goose.SetDialect("postgres"); err != nil {
		log.Fatal("Failed to set goose dialect:", err)
	}

	if err = goose.Up(db, "internal/db/migrations"); err != nil {
		log.Fatal("Failed to run migrations:", err)
	}

	log.Println("Migrations completed successfully")

	r := chi.NewRouter()

	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	r.Use(chiMiddleware.RequestID)

	// Rate limiting: 100 requests per minute per IP
	rateLimiter := middleware.NewRateLimiter(100, 1*time.Minute)
	r.Use(rateLimiter.Middleware)

	// Initialize identity resolver
	// IMPORTANT: In dev mode, identity resolution MUST use the same local PLC
	// directory as DID registration to ensure E2E tests work without hitting
	// the production plc.directory
	identityConfig := identity.DefaultConfig()

	isDevEnv := os.Getenv("IS_DEV_ENV") == "true"
	plcDirectoryURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcDirectoryURL == "" {
		plcDirectoryURL = "https://plc.directory" // Default to production PLC
	}

	// In dev mode, use PLC_DIRECTORY_URL for identity resolution
	// In prod mode, use IDENTITY_PLC_URL if set, otherwise PLC_DIRECTORY_URL
	if isDevEnv {
		identityConfig.PLCURL = plcDirectoryURL
		log.Printf("🧪 DEV MODE: Identity resolver will use local PLC: %s", plcDirectoryURL)
	} else {
		// Production: Allow separate IDENTITY_PLC_URL for read operations
		if identityPLCURL := os.Getenv("IDENTITY_PLC_URL"); identityPLCURL != "" {
			identityConfig.PLCURL = identityPLCURL
		} else {
			identityConfig.PLCURL = plcDirectoryURL
		}
		log.Printf("✅ PRODUCTION MODE: Identity resolver using PLC: %s", identityConfig.PLCURL)
	}

	if cacheTTL := os.Getenv("IDENTITY_CACHE_TTL"); cacheTTL != "" {
		if duration, parseErr := time.ParseDuration(cacheTTL); parseErr == nil {
			identityConfig.CacheTTL = duration
		}
	}

	identityResolver := identity.NewResolver(db, identityConfig)

	// Initialize atProto auth middleware for JWT validation
	// Phase 1: Set skipVerify=true to test JWT parsing only
	// Phase 2: Set skipVerify=false to enable full signature verification
	skipVerify := os.Getenv("AUTH_SKIP_VERIFY") == "true"
	if skipVerify {
		log.Println("⚠️  WARNING: JWT signature verification is DISABLED (Phase 1 testing)")
		log.Println("   Set AUTH_SKIP_VERIFY=false for production")
	}

	jwksCacheTTL := 1 * time.Hour // Cache public keys for 1 hour
	jwksFetcher := auth.NewCachedJWKSFetcher(jwksCacheTTL)
	authMiddleware := middleware.NewAtProtoAuthMiddleware(jwksFetcher, skipVerify)
	log.Println("✅ atProto auth middleware initialized")

	// Initialize repositories and services
	userRepo := postgresRepo.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, defaultPDS)

	communityRepo := postgresRepo.NewCommunityRepository(db)

	// V2.0: PDS-managed DID generation
	// Community DIDs and keys are generated entirely by the PDS
	// No Coves-side DID generator needed (reserved for future V2.1 hybrid approach)

	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:coves.social" // Default for development
	}

	// V2: Extract instance domain for community handles
	// IMPORTANT: This MUST match the domain in INSTANCE_DID for security
	// We cannot allow arbitrary domains to prevent impersonation attacks
	// Example attack: !leagueoflegends@riotgames.com on a non-Riot instance
	//
	// SECURITY: did:web domain verification is implemented in the Jetstream consumer
	// See: internal/atproto/jetstream/community_consumer.go - verifyHostedByClaim()
	// Communities with mismatched hostedBy domains are rejected during indexing
	var instanceDomain string
	if strings.HasPrefix(instanceDID, "did:web:") {
		// Extract domain from did:web (this is the authoritative source)
		instanceDomain = strings.TrimPrefix(instanceDID, "did:web:")
	} else {
		// For non-web DIDs (e.g., did:plc), require explicit INSTANCE_DOMAIN
		instanceDomain = os.Getenv("INSTANCE_DOMAIN")
		if instanceDomain == "" {
			log.Fatal("INSTANCE_DOMAIN must be set for non-web DIDs")
		}
	}

	log.Printf("Instance domain: %s (extracted from DID: %s)", instanceDomain, instanceDID)

	// V2.0: Initialize PDS account provisioner for communities (simplified)
	// PDS handles all DID and key generation - no Coves-side cryptography needed
	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, defaultPDS)
	log.Printf("✅ Community provisioner initialized (PDS-managed keys)")
	log.Printf("   - Communities will be created at: %s", defaultPDS)
	log.Printf("   - PDS will generate and manage all DIDs and keys")

	// Initialize community service (no longer needs didGenerator directly)
	communityService := communities.NewCommunityService(communityRepo, defaultPDS, instanceDID, instanceDomain, provisioner)

	// Authenticate Coves instance with PDS to enable community record writes
	// The instance needs a PDS account to write community records it owns
	pdsHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	pdsPassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if pdsHandle != "" && pdsPassword != "" {
		log.Printf("Authenticating Coves instance (%s) with PDS...", instanceDID)
		accessToken, authErr := authenticateWithPDS(defaultPDS, pdsHandle, pdsPassword)
		if authErr != nil {
			log.Printf("Warning: Failed to authenticate with PDS: %v", authErr)
			log.Println("Community creation will fail until PDS authentication is configured")
		} else {
			if svc, ok := communityService.(interface{ SetPDSAccessToken(string) }); ok {
				svc.SetPDSAccessToken(accessToken)
				log.Println("✓ Coves instance authenticated with PDS")
			}
		}
	} else {
		log.Println("Note: PDS_INSTANCE_HANDLE and PDS_INSTANCE_PASSWORD not set")
		log.Println("Community creation via write-forward is disabled")
	}

	// Start Jetstream consumer for read-forward user indexing
	jetstreamURL := os.Getenv("JETSTREAM_URL")
	if jetstreamURL == "" {
		jetstreamURL = "wss://jetstream2.us-east.bsky.network/subscribe?wantedCollections=app.bsky.actor.profile"
	}

	pdsFilter := os.Getenv("JETSTREAM_PDS_FILTER") // Optional: filter to specific PDS

	userConsumer := jetstream.NewUserEventConsumer(userService, identityResolver, jetstreamURL, pdsFilter)
	ctx := context.Background()
	go func() {
		if startErr := userConsumer.Start(ctx); startErr != nil {
			log.Printf("Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream user consumer: %s", jetstreamURL)

	// Start Jetstream consumer for community events (profiles and subscriptions)
	// This consumer indexes:
	// 1. Community profiles (social.coves.community.profile) - in community's own repo
	// 2. User subscriptions (social.coves.community.subscription) - in user's repo
	communityJetstreamURL := os.Getenv("COMMUNITY_JETSTREAM_URL")
	if communityJetstreamURL == "" {
		// Local Jetstream for communities - filter to our instance's collections
		// IMPORTANT: We listen to social.coves.community.subscription (not social.coves.community.subscribe)
		// because subscriptions are RECORD TYPES in the communities namespace, not XRPC procedures
		communityJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.community.profile&wantedCollections=social.coves.community.subscription"
	}

	// Initialize community event consumer with did:web verification
	skipDIDWebVerification := os.Getenv("SKIP_DID_WEB_VERIFICATION") == "true"
	if skipDIDWebVerification {
		log.Println("⚠️  WARNING: did:web domain verification is DISABLED (dev mode)")
		log.Println("   Set SKIP_DID_WEB_VERIFICATION=false for production")
	}

	// Pass identity resolver to consumer for PLC handle resolution (source of truth)
	communityEventConsumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, skipDIDWebVerification, identityResolver)
	communityJetstreamConnector := jetstream.NewCommunityJetstreamConnector(communityEventConsumer, communityJetstreamURL)

	go func() {
		if startErr := communityJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Community Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream community consumer: %s", communityJetstreamURL)
	log.Println("  - Indexing: social.coves.community.profile (community profiles)")
	log.Println("  - Indexing: social.coves.community.subscription (user subscriptions)")

	// Start JWKS cache cleanup background job
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			jwksFetcher.CleanupExpiredCache()
			log.Println("JWKS cache cleanup completed")
		}
	}()

	log.Println("Started JWKS cache cleanup background job (runs hourly)")

	// Initialize aggregator service
	aggregatorRepo := postgresRepo.NewAggregatorRepository(db)
	aggregatorService := aggregators.NewAggregatorService(aggregatorRepo, communityService)
	log.Println("✅ Aggregator service initialized")

	// Initialize post service (with aggregator support)
	postRepo := postgresRepo.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, aggregatorService, defaultPDS)

	// Initialize vote repository (used by Jetstream consumer for indexing)
	voteRepo := postgresRepo.NewVoteRepository(db)
	log.Println("✅ Vote repository initialized (Jetstream indexing only)")

	// Initialize comment repository (used by Jetstream consumer for indexing)
	commentRepo := postgresRepo.NewCommentRepository(db)
	log.Println("✅ Comment repository initialized (Jetstream indexing only)")

	// Initialize comment service (for query API)
	// Requires user and community repos for proper author/community hydration per lexicon
	commentService := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo)
	log.Println("✅ Comment service initialized (with author/community hydration)")

	// Initialize feed service
	feedRepo := postgresRepo.NewCommunityFeedRepository(db)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	log.Println("✅ Feed service initialized")

	// Initialize timeline service (home feed from subscribed communities)
	timelineRepo := postgresRepo.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	log.Println("✅ Timeline service initialized")

	// Initialize discover service (public feed from all communities)
	discoverRepo := postgresRepo.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	log.Println("✅ Discover service initialized")

	// Start Jetstream consumer for posts
	// This consumer indexes posts created in community repositories via the firehose
	// Currently handles only CREATE operations - UPDATE/DELETE deferred until those features exist
	postJetstreamURL := os.Getenv("POST_JETSTREAM_URL")
	if postJetstreamURL == "" {
		// Listen to post record creation events
		postJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.community.post"
	}

	postEventConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService)
	postJetstreamConnector := jetstream.NewPostJetstreamConnector(postEventConsumer, postJetstreamURL)

	go func() {
		if startErr := postJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Post Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream post consumer: %s", postJetstreamURL)
	log.Println("  - Indexing: social.coves.community.post CREATE operations")
	log.Println("  - UPDATE/DELETE indexing deferred until those features are implemented")

	// Start Jetstream consumer for aggregators
	// This consumer indexes aggregator service declarations and authorization records
	// Following Bluesky's pattern for feed generators and labelers
	// NOTE: Uses the same Jetstream as communities, just filtering different collections
	aggregatorJetstreamURL := communityJetstreamURL
	// Override if specific URL needed for testing
	if envURL := os.Getenv("AGGREGATOR_JETSTREAM_URL"); envURL != "" {
		aggregatorJetstreamURL = envURL
	} else if aggregatorJetstreamURL == "" {
		// Fallback if community URL also not set
		aggregatorJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.aggregator.service&wantedCollections=social.coves.aggregator.authorization"
	}

	aggregatorEventConsumer := jetstream.NewAggregatorEventConsumer(aggregatorRepo)
	aggregatorJetstreamConnector := jetstream.NewAggregatorJetstreamConnector(aggregatorEventConsumer, aggregatorJetstreamURL)

	go func() {
		if startErr := aggregatorJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Aggregator Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream aggregator consumer: %s", aggregatorJetstreamURL)
	log.Println("  - Indexing: social.coves.aggregator.service (service declarations)")
	log.Println("  - Indexing: social.coves.aggregator.authorization (authorization records)")

	// Start Jetstream consumer for votes
	// This consumer indexes votes from user repositories and updates post vote counts
	voteJetstreamURL := os.Getenv("VOTE_JETSTREAM_URL")
	if voteJetstreamURL == "" {
		// Listen to vote record CREATE/DELETE events from user repositories
		voteJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.feed.vote"
	}

	voteEventConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
	voteJetstreamConnector := jetstream.NewVoteJetstreamConnector(voteEventConsumer, voteJetstreamURL)

	go func() {
		if startErr := voteJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Vote Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream vote consumer: %s", voteJetstreamURL)
	log.Println("  - Indexing: social.coves.feed.vote CREATE/DELETE operations")
	log.Println("  - Updating: Post vote counts atomically")

	// Start Jetstream consumer for comments
	// This consumer indexes comments from user repositories and updates parent counts
	commentJetstreamURL := os.Getenv("COMMENT_JETSTREAM_URL")
	if commentJetstreamURL == "" {
		// Listen to comment record CREATE/UPDATE/DELETE events from user repositories
		commentJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.feed.comment"
	}

	commentEventConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
	commentJetstreamConnector := jetstream.NewCommentJetstreamConnector(commentEventConsumer, commentJetstreamURL)

	go func() {
		if startErr := commentJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Comment Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream comment consumer: %s", commentJetstreamURL)
	log.Println("  - Indexing: social.coves.feed.comment CREATE/UPDATE/DELETE operations")
	log.Println("  - Updating: Post comment counts and comment reply counts atomically")

	// Register XRPC routes
	routes.RegisterUserRoutes(r, userService)
	routes.RegisterCommunityRoutes(r, communityService, authMiddleware)
	log.Println("Community XRPC endpoints registered with OAuth authentication")

	routes.RegisterPostRoutes(r, postService, authMiddleware)
	log.Println("Post XRPC endpoints registered with OAuth authentication")

	// Vote write endpoints removed - clients write directly to their PDS
	// The AppView indexes votes from Jetstream (see vote consumer above)

	routes.RegisterCommunityFeedRoutes(r, feedService)
	log.Println("Feed XRPC endpoints registered (public, no auth required)")

	routes.RegisterTimelineRoutes(r, timelineService, authMiddleware)
	log.Println("Timeline XRPC endpoints registered (requires authentication)")

	routes.RegisterDiscoverRoutes(r, discoverService)
	log.Println("Discover XRPC endpoints registered (public, no auth required)")

	routes.RegisterAggregatorRoutes(r, aggregatorService)
	log.Println("Aggregator XRPC endpoints registered (query endpoints public)")

	// Comment query API - supports optional authentication for viewer state
	// Stricter rate limiting for expensive nested comment queries
	commentRateLimiter := middleware.NewRateLimiter(20, 1*time.Minute)
	commentServiceAdapter := commentsAPI.NewServiceAdapter(commentService)
	commentHandler := commentsAPI.NewGetCommentsHandler(commentServiceAdapter)
	r.Handle(
		"/xrpc/social.coves.community.comment.getComments",
		commentRateLimiter.Middleware(
			commentsAPI.OptionalAuthMiddleware(authMiddleware, commentHandler.HandleGetComments),
		),
	)
	log.Println("✅ Comment query API registered (20 req/min rate limit)")
	log.Println("  - GET /xrpc/social.coves.community.comment.getComments")

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			log.Printf("Failed to write health check response: %v", err)
		}
	})

	port := os.Getenv("APPVIEW_PORT")
	if port == "" {
		port = "8081" // Match .env.dev default
	}

	fmt.Printf("Coves AppView starting on port %s\n", port)
	fmt.Printf("Default PDS: %s\n", defaultPDS)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

// authenticateWithPDS creates a session on the PDS and returns an access token
func authenticateWithPDS(pdsURL, handle, password string) (string, error) {
	type CreateSessionRequest struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}

	type CreateSessionResponse struct {
		DID       string `json:"did"`
		Handle    string `json:"handle"`
		AccessJwt string `json:"accessJwt"`
	}

	reqBody, err := json.Marshal(CreateSessionRequest{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(
		pdsURL+"/xrpc/com.atproto.server.createSession",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("PDS returned status %d and failed to read body: %w", resp.StatusCode, readErr)
		}
		return "", fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	var session CreateSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return session.AccessJwt, nil
}
