package integration

import (
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"Coves/internal/api/routes"
)

func setupTestDB(t *testing.T) *sql.DB {
	// Build connection string from environment variables (set by .env.dev)
	// These are loaded by the Makefile when running tests
	testUser := os.Getenv("POSTGRES_TEST_USER")
	testPassword := os.Getenv("POSTGRES_TEST_PASSWORD")
	testPort := os.Getenv("POSTGRES_TEST_PORT")
	testDB := os.Getenv("POSTGRES_TEST_DB")

	// Fallback to defaults if not set
	if testUser == "" {
		testUser = "test_user"
	}
	if testPassword == "" {
		testPassword = "test_password"
	}
	if testPort == "" {
		testPort = "5434"
	}
	if testDB == "" {
		testDB = "coves_test"
	}

	dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		testUser, testPassword, testPort, testDB)

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("Failed to ping test database: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("Failed to set goose dialect: %v", err)
	}

	if err := goose.Up(db, "../../internal/db/migrations"); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	// Clean up any existing test data
	_, err = db.Exec("DELETE FROM users WHERE email LIKE '%@example.com'")
	if err != nil {
		t.Logf("Warning: Failed to clean up test data: %v", err)
	}

	return db
}

func TestCreateUser(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Wire up dependencies according to architecture
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo)

	r := chi.NewRouter()
	r.Mount("/api/users", routes.UserRoutes(userService))

	user := users.CreateUserRequest{
		Email:    "test@example.com",
		Username: "testuser",
	}

	body, _ := json.Marshal(user)
	req := httptest.NewRequest("POST", "/api/users", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d. Response: %s", http.StatusCreated, w.Code, w.Body.String())
		return
	}

	var createdUser users.User
	if err := json.NewDecoder(w.Body).Decode(&createdUser); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if createdUser.Email != user.Email {
		t.Errorf("Expected email %s, got %s", user.Email, createdUser.Email)
	}

	if createdUser.Username != user.Username {
		t.Errorf("Expected username %s, got %s", user.Username, createdUser.Username)
	}
}

