package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"Coves/internal/api/handlers/oauth"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	oauthCore "Coves/internal/core/oauth"
	"Coves/internal/core/users"
	postgresRepo "Coves/internal/db/postgres"
)

func main() {
	// Database configuration (AppView database)
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		// Use dev database from .env.dev
		dbURL = "postgres://dev_user:dev_password@localhost:5433/coves_dev?sslmode=disable"
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

	log.Printf("Started Jetstream consumer: %s", jetstreamURL)

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
