package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/oauth"

	indigoauth "github.com/bluesky-social/indigo/atproto/auth"
	indigoidentity "github.com/bluesky-social/indigo/atproto/identity"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/internal/core/unfurl"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	commentsAPI "Coves/internal/api/handlers/comments"

	postgresRepo "Coves/internal/db/postgres"
)

// Compile-time interface satisfaction checks
var _ oauth.UserIndexer = (users.UserService)(nil)

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

	// Get PLC URL for OAuth and other services
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "https://plc.directory"
	}
	log.Printf("🔐 OAuth will use PLC directory: %s", plcURL)

	// Initialize OAuth client for sealed session tokens
	// Mobile apps authenticate via OAuth flow and receive sealed session tokens
	// These tokens are encrypted references to OAuth sessions stored in the database
	oauthSealSecret := os.Getenv("OAUTH_SEAL_SECRET")
	if oauthSealSecret == "" {
		if os.Getenv("IS_DEV_ENV") != "true" {
			log.Fatal("OAUTH_SEAL_SECRET is required in production mode")
		}
		// Generate RANDOM secret for dev mode
		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			log.Fatal("Failed to generate random seal secret: ", err)
		}
		oauthSealSecret = base64.StdEncoding.EncodeToString(randomBytes)
		log.Println("⚠️  DEV MODE: Generated random OAuth seal secret (won't persist across restarts)")
	}

	isDevMode := os.Getenv("IS_DEV_ENV") == "true"
	pdsURL := os.Getenv("PDS_URL") // For dev mode: resolve handles via local PDS
	oauthConfig := &oauth.OAuthConfig{
		PublicURL:       os.Getenv("APPVIEW_PUBLIC_URL"),
		SealSecret:      oauthSealSecret,
		Scopes:          []string{"atproto", "transition:generic"},
		DevMode:         isDevMode,
		AllowPrivateIPs: isDevMode, // Allow private IPs only in dev mode
		PLCURL:          plcURL,
		PDSURL:          pdsURL, // For dev mode handle resolution
		// SessionTTL and SealedTokenTTL will use defaults if not set (7 days and 14 days)
	}

	// Create PostgreSQL-backed OAuth session store (using default 7-day TTL)
	baseOAuthStore := oauth.NewPostgresOAuthStore(db, 0)
	// Wrap with MobileAwareStoreWrapper to capture OAuth state for mobile CSRF validation.
	// This intercepts SaveAuthRequestInfo to save mobile CSRF data when present in context.
	oauthStore := oauth.NewMobileAwareStoreWrapper(baseOAuthStore)

	if oauthConfig.PublicURL == "" {
		oauthConfig.PublicURL = "http://localhost:8080"
		oauthConfig.DevMode = true // Force dev mode for localhost
	}

	oauthClient, err := oauth.NewOAuthClient(oauthConfig, oauthStore)
	if err != nil {
		log.Fatalf("Failed to initialize OAuth client: %v", err)
	}

	// Initialize user repository and service early (needed for OAuth user indexing)
	userRepo := postgresRepo.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, defaultPDS)

	// Create OAuth handler for HTTP endpoints
	// WithUserIndexer ensures users are indexed into local database after OAuth login
	oauthHandler := oauth.NewOAuthHandler(oauthClient, oauthStore, oauth.WithUserIndexer(userService))

	// Create OAuth auth middleware
	// Validates sealed session tokens and loads OAuth sessions from database
	authMiddleware := middleware.NewOAuthAuthMiddleware(oauthClient, oauthStore)
	log.Println("✅ OAuth auth middleware initialized (sealed session tokens)")

	// Create identity directory for service auth validator
	// This is used to verify DIDs in service JWTs for aggregator authentication
	// Note: The 10-second timeout here is for HTTP requests made by the identity resolver itself,
	// not for the auth middleware's request context. The middleware passes r.Context() to the validator,
	// which properly respects request cancellation. This timeout is a safety net for slow DID resolution.
	identityDir := &indigoidentity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: http.Client{Timeout: 10 * time.Second},
	}

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

	// Community creation restriction - if set, only these DIDs can create communities
	var allowedCommunityCreators []string
	if communityCreators := os.Getenv("COMMUNITY_CREATORS"); communityCreators != "" {
		for _, did := range strings.Split(communityCreators, ",") {
			did = strings.TrimSpace(did)
			if did != "" {
				allowedCommunityCreators = append(allowedCommunityCreators, did)
			}
		}
		log.Printf("Community creation restricted to %d DIDs", len(allowedCommunityCreators))
	} else {
		log.Println("Community creation open to all authenticated users")
	}

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

	// Create user consumer with session handle updater to sync OAuth sessions on handle changes
	var consumerOpts []jetstream.ConsumerOption
	if sessionUpdater, ok := baseOAuthStore.(jetstream.SessionHandleUpdater); ok {
		consumerOpts = append(consumerOpts, jetstream.WithSessionHandleUpdater(sessionUpdater))
		log.Println("✅ OAuth session handle sync enabled for identity changes")
	}
	userConsumer := jetstream.NewUserEventConsumer(userService, identityResolver, jetstreamURL, pdsFilter, consumerOpts...)
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

	// Start OAuth session cleanup background job with cancellable context
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				log.Println("OAuth cleanup job stopped")
				return
			case <-ticker.C:
				// Check if store implements cleanup methods
				// Use UnwrapPostgresStore to get the underlying store from the wrapper
				if cleanupStore := oauthStore.UnwrapPostgresStore(); cleanupStore != nil {
					sessions, sessErr := cleanupStore.CleanupExpiredSessions(cleanupCtx)
					if sessErr != nil {
						log.Printf("Error cleaning up expired OAuth sessions: %v", sessErr)
					}
					requests, reqErr := cleanupStore.CleanupExpiredAuthRequests(cleanupCtx)
					if reqErr != nil {
						log.Printf("Error cleaning up expired OAuth auth requests: %v", reqErr)
					}
					if sessions > 0 || requests > 0 {
						log.Printf("OAuth cleanup: removed %d expired sessions, %d expired auth requests", sessions, requests)
					}
				}
			}
		}
	}()

	log.Println("Started OAuth session cleanup background job (runs hourly)")

	// Initialize aggregator service
	aggregatorRepo := postgresRepo.NewAggregatorRepository(db)
	aggregatorService := aggregators.NewAggregatorService(aggregatorRepo, communityService)
	log.Println("✅ Aggregator service initialized")

	// Initialize API key service for aggregator authentication
	apiKeyService := aggregators.NewAPIKeyService(aggregatorRepo, oauthClient.ClientApp)
	log.Println("✅ API key service initialized")

	// Get instance DID for service auth validator audience
	serviceDID := instanceDID // Use instance DID as the service audience

	// Create ServiceAuthValidator for aggregator JWT authentication
	// This validates service JWTs signed by aggregator PDSs
	serviceValidator := &indigoauth.ServiceAuthValidator{
		Audience:        serviceDID,
		Dir:             identityDir,
		TimestampLeeway: 30 * time.Second,
	}
	log.Printf("✅ Service auth validator initialized (audience: %s)", serviceDID)

	// Create DualAuthMiddleware that supports OAuth, service JWT, and API keys
	// OAuth tokens are for user authentication (sealed session tokens)
	// Service JWTs are for aggregator authentication (PDS-signed tokens)
	// API keys are for aggregator bot authentication (stateless, cryptographic)
	apiKeyValidator := middleware.NewAPIKeyValidatorAdapter(apiKeyService)
	dualAuth := middleware.NewDualAuthMiddleware(
		oauthClient,      // SessionUnsealer for OAuth
		oauthStore,       // ClientAuthStore for OAuth sessions
		serviceValidator, // ServiceAuthValidator for JWT validation
		aggregatorRepo,   // AggregatorChecker - uses repo directly since it implements the interface
	).WithAPIKeyValidator(apiKeyValidator)
	log.Println("✅ Dual auth middleware initialized (OAuth + service JWT + API keys)")

	// Initialize unfurl cache repository
	unfurlRepo := unfurl.NewRepository(db)

	// Initialize blob upload service
	blobService := blobs.NewBlobService(defaultPDS)

	// Initialize unfurl service with configuration
	unfurlService := unfurl.NewService(
		unfurlRepo,
		unfurl.WithTimeout(10*time.Second),
		unfurl.WithUserAgent("CovesBot/1.0 (+https://coves.social)"),
		unfurl.WithCacheTTL(24*time.Hour),
	)
	log.Println("✅ Unfurl and blob services initialized")

	// Initialize Bluesky post cache repository and service
	//
	// Production PLC Read-Only Resolver
	// ==================================
	// This resolver is used ONLY for resolving real Bluesky handles (e.g., "bretton.dev")
	// that exist on the production AT Protocol network.
	//
	// READ-ONLY GUARANTEE: The identity.Resolver interface only supports read operations:
	//   - Resolve(), ResolveHandle(), ResolveDID() - HTTP GET lookups only
	//   - Purge() - clears local cache, does NOT write to PLC
	//
	// DO NOT use this resolver for:
	//   - Integration tests (use local PLC at localhost:3002 via identityResolver)
	//   - Creating/registering new DIDs (handled by separate PLC client)
	//
	// Safe in dev/test: only performs HTTP GET to resolve existing Bluesky identities.
	productionPLCConfig := identity.DefaultConfig()
	productionPLCConfig.PLCURL = "https://plc.directory" // Production PLC - READ ONLY
	productionPLCResolver := identity.NewResolver(db, productionPLCConfig)
	log.Println("✅ Production PLC resolver initialized (READ-ONLY for Bluesky handle resolution)")

	blueskyRepo := blueskypost.NewRepository(db)
	blueskyService := blueskypost.NewService(
		blueskyRepo,
		productionPLCResolver, // READ-ONLY: resolves real Bluesky handles like "bretton.dev"
		blueskypost.WithTimeout(10*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour), // 1 hour cache (shorter than unfurl)
	)
	log.Println("✅ Bluesky post service initialized")

	// Initialize post service (with aggregator support)
	postRepo := postgresRepo.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, aggregatorService, blobService, unfurlService, blueskyService, defaultPDS)

	// Initialize vote repository (used by Jetstream consumer for indexing)
	voteRepo := postgresRepo.NewVoteRepository(db)
	log.Println("✅ Vote repository initialized (Jetstream indexing only)")

	// Initialize comment repository (used by Jetstream consumer for indexing)
	commentRepo := postgresRepo.NewCommentRepository(db)
	log.Println("✅ Comment repository initialized (Jetstream indexing only)")

	// Initialize vote cache (stores user votes from PDS to avoid eventual consistency issues)
	// TTL of 10 minutes - cache is also updated on vote create/delete
	voteCache := votes.NewVoteCache(10*time.Minute, nil)
	log.Println("✅ Vote cache initialized (10 minute TTL)")

	// Initialize vote service (for XRPC API endpoints)
	// Note: We don't validate subject existence - the vote goes to the user's PDS regardless.
	// The Jetstream consumer handles orphaned votes correctly by only updating counts for
	// non-deleted subjects. This avoids race conditions and eventual consistency issues.
	voteService := votes.NewService(voteRepo, oauthClient, oauthStore, voteCache, nil)
	log.Println("✅ Vote service initialized (with OAuth authentication and vote cache)")

	// Initialize comment service (for query and write APIs)
	// Requires user and community repos for proper author/community hydration per lexicon
	// OAuth client and store are needed for write operations (create, update, delete)
	commentService := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo, oauthClient, oauthStore, nil)
	log.Println("✅ Comment service initialized (with author/community hydration and write support)")

	// Initialize feed service
	feedRepo := postgresRepo.NewCommunityFeedRepository(db, cursorSecret)
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

	postEventConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)
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
		commentJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.community.comment"
	}

	commentEventConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
	commentJetstreamConnector := jetstream.NewCommentJetstreamConnector(commentEventConsumer, commentJetstreamURL)

	go func() {
		if startErr := commentJetstreamConnector.Start(ctx); startErr != nil {
			log.Printf("Comment Jetstream consumer stopped: %v", startErr)
		}
	}()

	log.Printf("Started Jetstream comment consumer: %s", commentJetstreamURL)
	log.Println("  - Indexing: social.coves.community.comment CREATE/UPDATE/DELETE operations")
	log.Println("  - Updating: Post comment counts and comment reply counts atomically")

	// Register XRPC routes
	routes.RegisterUserRoutes(r, userService)
	routes.RegisterCommunityRoutes(r, communityService, authMiddleware, allowedCommunityCreators)
	log.Println("Community XRPC endpoints registered with OAuth authentication")

	routes.RegisterPostRoutes(r, postService, dualAuth)
	log.Println("Post XRPC endpoints registered with dual auth (OAuth + service JWT for aggregators)")

	routes.RegisterVoteRoutes(r, voteService, authMiddleware)
	log.Println("Vote XRPC endpoints registered with OAuth authentication")

	// Register comment write routes (create, update, delete)
	routes.RegisterCommentRoutes(r, commentService, authMiddleware)
	log.Println("Comment write XRPC endpoints registered")
	log.Println("  - POST /xrpc/social.coves.community.comment.create")
	log.Println("  - POST /xrpc/social.coves.community.comment.update")
	log.Println("  - POST /xrpc/social.coves.community.comment.delete")

	routes.RegisterCommunityFeedRoutes(r, feedService, voteService, blueskyService, authMiddleware)
	log.Println("Feed XRPC endpoints registered (public with optional auth for viewer vote state)")

	routes.RegisterTimelineRoutes(r, timelineService, voteService, blueskyService, authMiddleware)
	log.Println("Timeline XRPC endpoints registered (requires authentication, includes viewer vote state)")

	routes.RegisterDiscoverRoutes(r, discoverService, voteService, blueskyService, authMiddleware)
	log.Println("Discover XRPC endpoints registered (public with optional auth for viewer vote state)")

	routes.RegisterActorRoutes(r, postService, userService, voteService, blueskyService, commentService, authMiddleware)
	log.Println("Actor XRPC endpoints registered (public with optional auth for viewer vote state)")
	log.Println("  - GET /xrpc/social.coves.actor.getPosts")
	log.Println("  - GET /xrpc/social.coves.actor.getComments")

	routes.RegisterAggregatorRoutes(r, aggregatorService, communityService, userService, identityResolver)
	log.Println("Aggregator XRPC endpoints registered (query endpoints public, registration endpoint public)")

	routes.RegisterAggregatorAPIKeyRoutes(r, authMiddleware, apiKeyService, aggregatorService)
	log.Println("✅ Aggregator API key endpoints registered")
	log.Println("  - POST /xrpc/social.coves.aggregator.createApiKey (requires OAuth)")
	log.Println("  - GET /xrpc/social.coves.aggregator.getApiKey (requires OAuth)")
	log.Println("  - POST /xrpc/social.coves.aggregator.revokeApiKey (requires OAuth)")
	log.Println("  - GET /xrpc/social.coves.aggregator.getMetrics (public)")

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

	// Configure allowed CORS origins for OAuth callback
	// SECURITY: Never use wildcard "*" with credentials - only allow specific origins
	var oauthAllowedOrigins []string
	appviewPublicURL := os.Getenv("APPVIEW_PUBLIC_URL")
	if appviewPublicURL == "" {
		appviewPublicURL = "http://localhost:8080"
	}
	oauthAllowedOrigins = append(oauthAllowedOrigins, appviewPublicURL)

	// In dev mode, also allow common localhost origins for testing
	if oauthConfig.DevMode {
		oauthAllowedOrigins = append(oauthAllowedOrigins,
			"http://localhost:3000",
			"http://localhost:3001",
			"http://localhost:5173",
			"http://127.0.0.1:8080",
			"http://127.0.0.1:3000",
			"http://127.0.0.1:3001",
			"http://127.0.0.1:5173",
		)
		log.Printf("🧪 DEV MODE: OAuth CORS allows localhost origins for testing")
	}
	log.Printf("OAuth CORS allowed origins: %v", oauthAllowedOrigins)

	// Register OAuth routes for authentication flow
	routes.RegisterOAuthRoutes(r, oauthHandler, oauthAllowedOrigins)
	log.Println("✅ OAuth endpoints registered")
	log.Println("  - GET /oauth/client-metadata.json")
	log.Println("  - GET /oauth/jwks.json")
	log.Println("  - GET /oauth/login")
	log.Println("  - GET /oauth/mobile/login")
	log.Println("  - GET /oauth/callback")
	log.Println("  - POST /oauth/logout")
	log.Println("  - POST /oauth/refresh")

	// Register well-known routes for mobile app deep linking
	routes.RegisterWellKnownRoutes(r)
	log.Println("✅ Well-known endpoints registered (mobile Universal Links & App Links)")
	log.Println("  - GET /.well-known/apple-app-site-association (iOS Universal Links)")
	log.Println("  - GET /.well-known/assetlinks.json (Android App Links)")

	// Health check endpoints
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			log.Printf("Failed to write health check response: %v", err)
		}
	}
	r.Get("/health", healthHandler)
	r.Get("/xrpc/_health", healthHandler)

	// Check PORT first (docker-compose), then APPVIEW_PORT (legacy)
	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("APPVIEW_PORT")
	}
	if port == "" {
		port = "8080"
	}

	// Create HTTP server for graceful shutdown
	server := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Channel to listen for shutdown signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in goroutine
	go func() {
		fmt.Printf("Coves AppView starting on port %s\n", port)
		fmt.Printf("Default PDS: %s\n", defaultPDS)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-stop
	log.Println("Shutting down server...")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop OAuth cleanup background job
	cleanupCancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}
	log.Println("Server stopped gracefully")
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
