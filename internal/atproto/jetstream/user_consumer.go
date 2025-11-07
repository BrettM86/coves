package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"

	"github.com/gorilla/websocket"
)

// JetstreamEvent represents an event from the Jetstream firehose
// Jetstream documentation: https://docs.bsky.app/docs/advanced-guides/jetstream
type JetstreamEvent struct {
	Account  *AccountEvent  `json:"account,omitempty"`
	Identity *IdentityEvent `json:"identity,omitempty"`
	Commit   *CommitEvent   `json:"commit,omitempty"`
	Did      string         `json:"did"`
	Kind     string         `json:"kind"`
	TimeUS   int64          `json:"time_us"`
}

type AccountEvent struct {
	Did    string `json:"did"`
	Time   string `json:"time"`
	Seq    int64  `json:"seq"`
	Active bool   `json:"active"`
}

type IdentityEvent struct {
	Did    string `json:"did"`
	Handle string `json:"handle"`
	Time   string `json:"time"`
	Seq    int64  `json:"seq"`
}

// CommitEvent represents a record commit from Jetstream
type CommitEvent struct {
	Rev        string                 `json:"rev"`
	Operation  string                 `json:"operation"` // "create", "update", "delete"
	Collection string                 `json:"collection"`
	RKey       string                 `json:"rkey"`
	Record     map[string]interface{} `json:"record,omitempty"`
	CID        string                 `json:"cid,omitempty"`
}

// UserEventConsumer consumes user-related events from Jetstream
type UserEventConsumer struct {
	userService      users.UserService
	identityResolver identity.Resolver
	wsURL            string
	pdsFilter        string // Optional: only index users from specific PDS
}

// NewUserEventConsumer creates a new Jetstream consumer for user events
func NewUserEventConsumer(userService users.UserService, identityResolver identity.Resolver, wsURL, pdsFilter string) *UserEventConsumer {
	return &UserEventConsumer{
		userService:      userService,
		identityResolver: identityResolver,
		wsURL:            wsURL,
		pdsFilter:        pdsFilter,
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
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Failed to close WebSocket connection: %v", err)
		}
	}()

	log.Println("Connected to Jetstream")

	// Set read deadline to detect connection issues
	if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		log.Printf("Failed to set read deadline: %v", err)
	}

	// Set pong handler to keep connection alive
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			log.Printf("Failed to set read deadline in pong handler: %v", err)
		}
		return nil
	})

	// Start ping ticker
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	var closeOnce sync.Once // Ensure done channel is only closed once

	// Goroutine to send pings
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Ping error: %v", err)
					closeOnce.Do(func() { close(done) })
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
				closeOnce.Do(func() { close(done) })
				return fmt.Errorf("read error: %w", err)
			}

			// Reset read deadline on successful read
			if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				log.Printf("Failed to set read deadline: %v", err)
			}

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

	// Get existing user to check if handle changed
	existingUser, err := c.userService.GetUserByDID(ctx, did)
	if err != nil {
		// User doesn't exist - create new user
		pdsURL := "https://bsky.social" // Default Bluesky PDS
		// TODO: Resolve PDS URL from DID document via PLC directory

		_, createErr := c.userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    did,
			Handle: handle,
			PDSURL: pdsURL,
		})

		if createErr != nil && !isDuplicateError(createErr) {
			return fmt.Errorf("failed to create user: %w", createErr)
		}

		log.Printf("Indexed new user: %s (%s)", handle, did)
		return nil
	}

	// User exists - check if handle changed
	if existingUser.Handle != handle {
		log.Printf("Handle changed: %s → %s (DID: %s)", existingUser.Handle, handle, did)

		// CRITICAL: Update database FIRST, then purge cache
		// This prevents race condition where cache gets refilled with stale data
		_, updateErr := c.userService.UpdateHandle(ctx, did, handle)
		if updateErr != nil {
			return fmt.Errorf("failed to update handle: %w", updateErr)
		}

		// CRITICAL: Purge BOTH old handle and DID from cache
		// Old handle: alice.bsky.social → did:plc:abc123 (must be removed)
		if purgeErr := c.identityResolver.Purge(ctx, existingUser.Handle); purgeErr != nil {
			log.Printf("Warning: failed to purge old handle cache for %s: %v", existingUser.Handle, purgeErr)
		}

		// DID: did:plc:abc123 → alice.bsky.social (must be removed)
		if purgeErr := c.identityResolver.Purge(ctx, did); purgeErr != nil {
			log.Printf("Warning: failed to purge DID cache for %s: %v", did, purgeErr)
		}

		log.Printf("Updated handle and purged cache: %s → %s", existingUser.Handle, handle)
	} else {
		log.Printf("Handle unchanged for %s (%s)", handle, did)
	}

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
