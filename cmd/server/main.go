package main

import (
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

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
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

	// PDS URL configuration
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001" // Local dev PDS
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

	// Initialize repositories and services
	userRepo := postgresRepo.NewUserRepository(db)
	userService := users.NewUserService(userRepo, pdsURL)

	// Mount XRPC routes
	r.Mount("/xrpc/social.coves.actor", routes.UserRoutes(userService))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	port := os.Getenv("APPVIEW_PORT")
	if port == "" {
		port = "8081" // Match .env.dev default
	}

	fmt.Printf("Coves AppView starting on port %s\n", port)
	fmt.Printf("PDS URL: %s\n", pdsURL)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
