package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/users"
	"github.com/gorilla/websocket"
)

// JetstreamEvent represents an event from the Jetstream firehose
// Jetstream documentation: https://docs.bsky.app/docs/advanced-guides/jetstream
type JetstreamEvent struct {
	Did    string          `json:"did"`
	TimeUS int64           `json:"time_us"`
	Kind   string          `json:"kind"` // "account", "commit", "identity"
	Account *AccountEvent  `json:"account,omitempty"`
	Identity *IdentityEvent `json:"identity,omitempty"`
}

type AccountEvent struct {
	Active bool   `json:"active"`
	Did    string `json:"did"`
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
}

type IdentityEvent struct {
	Did    string `json:"did"`
	Handle string `json:"handle"`
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
}

// UserEventConsumer consumes user-related events from Jetstream
type UserEventConsumer struct {
	userService users.UserService
	wsURL       string
	pdsFilter   string // Optional: only index users from specific PDS
}

// NewUserEventConsumer creates a new Jetstream consumer for user events
func NewUserEventConsumer(userService users.UserService, wsURL string, pdsFilter string) *UserEventConsumer {
	return &UserEventConsumer{
		userService: userService,
		wsURL:       wsURL,
		pdsFilter:   pdsFilter,
	}
}

// Start begins consuming events from Jetstream
// Runs indefinitely, reconnecting on errors
func (c *UserEventConsumer) Start(ctx context.Context) error {
	log.Printf("Starting Jetstream user consumer: %s", c.wsURL)

	for {
		select {
		case <-ctx.Done():
			log.Println("Jetstream consumer shutting down")
			return ctx.Err()
		default:
			if err := c.connect(ctx); err != nil {
				log.Printf("Jetstream connection error: %v. Retrying in 5s...", err)
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}
}

// connect establishes WebSocket connection and processes events
func (c *UserEventConsumer) connect(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer conn.Close()

	log.Println("Connected to Jetstream")

	// Set read deadline to detect connection issues
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Set pong handler to keep connection alive
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Start ping ticker
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})

	// Goroutine to send pings
	// TODO: Fix race condition - multiple goroutines can call close(done) concurrently
	// Use sync.Once to ensure close(done) is called exactly once
	// See PR review issue #4
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Ping error: %v", err)
					close(done)
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Read messages
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return fmt.Errorf("connection closed")
		default:
			_, message, err := conn.ReadMessage()
			if err != nil {
				close(done)
				return fmt.Errorf("read error: %w", err)
			}

			// Reset read deadline on successful read
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))

			if err := c.handleEvent(ctx, message); err != nil {
				log.Printf("Error handling event: %v", err)
				// Continue processing other events
			}
		}
	}
}

// handleEvent processes a single Jetstream event
func (c *UserEventConsumer) handleEvent(ctx context.Context, data []byte) error {
	var event JetstreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("failed to parse event: %w", err)
	}

	// We're interested in identity events (handle updates) and account events (new users)
	switch event.Kind {
	case "identity":
		return c.handleIdentityEvent(ctx, &event)
	case "account":
		return c.handleAccountEvent(ctx, &event)
	default:
		// Ignore other event types (commits, etc.)
		return nil
	}
}

// HandleIdentityEventPublic is a public wrapper for testing
func (c *UserEventConsumer) HandleIdentityEventPublic(ctx context.Context, event *JetstreamEvent) error {
	return c.handleIdentityEvent(ctx, event)
}

// handleIdentityEvent processes identity events (handle changes)
func (c *UserEventConsumer) handleIdentityEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Identity == nil {
		return fmt.Errorf("identity event missing identity data")
	}

	did := event.Identity.Did
	handle := event.Identity.Handle

	if did == "" || handle == "" {
		return fmt.Errorf("identity event missing did or handle")
	}

	log.Printf("Identity event: %s → %s", did, handle)

	// For now, we'll create/update user on identity events
	// In a full implementation, you'd want to:
	// 1. Check if user exists
	// 2. Update handle if changed
	// 3. Resolve PDS URL from DID document

	// Simplified: just try to create user (will be idempotent)
	// We need PDS URL - for now use a placeholder
	// TODO: Implement DID→PDS resolution via PLC directory (https://plc.directory/{did})
	// For production federation support, resolve PDS endpoint from DID document
	// For local dev, this works fine since we filter to our own PDS
	// See PR review issue #2
	pdsURL := "https://bsky.social" // Default Bluesky PDS

	_, err := c.userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    did,
		Handle: handle,
		PDSURL: pdsURL,
	})

	if err != nil {
		// Check if it's a duplicate error (expected for idempotency)
		if isDuplicateError(err) {
			log.Printf("User already indexed: %s (%s)", handle, did)
			return nil
		}
		return fmt.Errorf("failed to create user: %w", err)
	}

	log.Printf("Indexed new user: %s (%s)", handle, did)
	return nil
}

// handleAccountEvent processes account events (account creation/updates)
func (c *UserEventConsumer) handleAccountEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Account == nil {
		return fmt.Errorf("account event missing account data")
	}

	did := event.Account.Did
	if did == "" {
		return fmt.Errorf("account event missing did")
	}

	// Account events don't include handle, so we can't index yet
	// We'll wait for the corresponding identity event
	log.Printf("Account event for %s (waiting for identity event)", did)
	return nil
}

// isDuplicateError checks if error is due to duplicate DID/handle
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return contains(errStr, "already exists") || contains(errStr, "already taken") || contains(errStr, "duplicate")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && anySubstring(s, substr))
}

func anySubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
