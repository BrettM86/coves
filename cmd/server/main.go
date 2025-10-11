package main

import (
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

	"Coves/internal/api/handlers/oauth"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/did"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	oauthCore "Coves/internal/core/oauth"
	"Coves/internal/core/users"
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

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatal("Failed to ping database:", err)
	}

	log.Println("Connected to AppView database")

	// Run migrations
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal("Failed to set goose dialect:", err)
	}

	if err := goose.Up(db, "internal/db/migrations"); err != nil {
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
	identityConfig := identity.DefaultConfig()
	// Override from environment if set
	if plcURL := os.Getenv("IDENTITY_PLC_URL"); plcURL != "" {
		identityConfig.PLCURL = plcURL
	}
	if cacheTTL := os.Getenv("IDENTITY_CACHE_TTL"); cacheTTL != "" {
		if duration, err := time.ParseDuration(cacheTTL); err == nil {
			identityConfig.CacheTTL = duration
		}
	}

	identityResolver := identity.NewResolver(db, identityConfig)
	log.Println("Identity resolver initialized with PLC:", identityConfig.PLCURL)

	// Initialize OAuth session store
	sessionStore := oauthCore.NewPostgresSessionStore(db)
	log.Println("OAuth session store initialized")

	// Initialize repositories and services
	userRepo := postgresRepo.NewUserRepository(db)
	userService := users.NewUserService(userRepo, identityResolver, defaultPDS)

	communityRepo := postgresRepo.NewCommunityRepository(db)

	// Initialize DID generator for communities
	// IS_DEV_ENV=true:  Generate did:plc:xxx without registering to PLC directory
	// IS_DEV_ENV=false: Generate did:plc:xxx and register with PLC_DIRECTORY_URL
	isDevEnv := os.Getenv("IS_DEV_ENV") == "true"
	plcDirectoryURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcDirectoryURL == "" {
		plcDirectoryURL = "https://plc.directory" // Default to Bluesky's PLC
	}
	didGenerator := did.NewGenerator(isDevEnv, plcDirectoryURL)
	log.Printf("DID generator initialized (dev_mode=%v, plc_url=%s)", isDevEnv, plcDirectoryURL)

	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:coves.local" // Default for development
	}

	// V2: Extract instance domain for community handles
	// IMPORTANT: This MUST match the domain in INSTANCE_DID for security
	// We cannot allow arbitrary domains to prevent impersonation attacks
	// Example attack: !leagueoflegends@riotgames.com on a non-Riot instance
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

	// V2: Initialize PDS account provisioner for communities
	provisioner := communities.NewPDSAccountProvisioner(userService, instanceDomain, defaultPDS)

	communityService := communities.NewCommunityService(communityRepo, didGenerator, defaultPDS, instanceDID, instanceDomain, provisioner)

	// Authenticate Coves instance with PDS to enable community record writes
	// The instance needs a PDS account to write community records it owns
	pdsHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	pdsPassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if pdsHandle != "" && pdsPassword != "" {
		log.Printf("Authenticating Coves instance (%s) with PDS...", instanceDID)
		accessToken, err := authenticateWithPDS(defaultPDS, pdsHandle, pdsPassword)
		if err != nil {
			log.Printf("Warning: Failed to authenticate with PDS: %v", err)
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
		if err := userConsumer.Start(ctx); err != nil {
			log.Printf("Jetstream consumer stopped: %v", err)
		}
	}()

	log.Printf("Started Jetstream user consumer: %s", jetstreamURL)

	// Note: Community indexing happens through the same Jetstream firehose
	// The CommunityEventConsumer is used by handlers when processing community-related events
	// For now, community records are created via write-forward to PDS, then indexed when
	// they appear in the firehose. A dedicated consumer can be added later if needed.
	log.Println("Community event consumer initialized (processes events from firehose)")

	// Start OAuth cleanup background job
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if pgStore, ok := sessionStore.(*oauthCore.PostgresSessionStore); ok {
				_ = pgStore.CleanupExpiredRequests(ctx)
				_ = pgStore.CleanupExpiredSessions(ctx)
				log.Println("OAuth cleanup completed")
			}
		}
	}()

	log.Println("Started OAuth cleanup background job (runs hourly)")

	// Initialize OAuth cookie store (singleton)
	cookieSecret, err := oauth.GetEnvBase64OrPlain("OAUTH_COOKIE_SECRET")
	if err != nil {
		log.Fatalf("Failed to load OAUTH_COOKIE_SECRET: %v", err)
	}
	if cookieSecret == "" {
		log.Fatal("OAUTH_COOKIE_SECRET not configured")
	}

	if err := oauth.InitCookieStore(cookieSecret); err != nil {
		log.Fatalf("Failed to initialize cookie store: %v", err)
	}

	// Initialize OAuth handlers
	loginHandler := oauth.NewLoginHandler(identityResolver, sessionStore)
	callbackHandler := oauth.NewCallbackHandler(sessionStore)
	logoutHandler := oauth.NewLogoutHandler(sessionStore)

	// OAuth routes (public endpoints)
	r.Post("/oauth/login", loginHandler.HandleLogin)
	r.Get("/oauth/callback", callbackHandler.HandleCallback)
	r.Post("/oauth/logout", logoutHandler.HandleLogout)
	r.Get("/oauth/client-metadata.json", oauth.HandleClientMetadata)
	r.Get("/oauth/jwks.json", oauth.HandleJWKS)

	log.Println("OAuth endpoints registered")

	// Register XRPC routes
	routes.RegisterUserRoutes(r, userService)
	routes.RegisterCommunityRoutes(r, communityService)
	log.Println("Community XRPC endpoints registered")

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	var session CreateSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return session.AccessJwt, nil
}
