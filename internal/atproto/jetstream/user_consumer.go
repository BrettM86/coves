package jetstream

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// CovesProfileCollection is the atProto collection for Coves user profiles.
// NOTE: This constant is intentionally duplicated in internal/api/handlers/user/update_profile.go
// to avoid circular dependencies between packages. Keep both definitions in sync.
const CovesProfileCollection = "social.coves.actor.profile"

// CovesActorBlockCollection is the atProto collection for user-to-user blocks.
// Records live in the blocker's repository at at://blocker_did/social.coves.actor.block/{tid}
const CovesActorBlockCollection = "social.coves.actor.block"

// SessionHandleUpdater is an interface for updating OAuth session handles
// when identity changes occur. This keeps active sessions in sync with
// the user's current handle.
type SessionHandleUpdater interface {
	UpdateHandleByDID(ctx context.Context, did, newHandle string) (int64, error)
}

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
	userService          users.UserService
	identityResolver     identity.Resolver
	sessionHandleUpdater SessionHandleUpdater    // Optional: updates OAuth sessions on handle change
	userBlockRepo        userblocks.Repository   // Optional: indexes user-to-user blocks
	wsURL                string
	pdsFilter            string // Optional: only index users from specific PDS
}

// ConsumerOption is a functional option for configuring UserEventConsumer
type ConsumerOption func(*UserEventConsumer)

// WithSessionHandleUpdater sets the session handle updater for syncing OAuth sessions
// when identity changes occur. If not set, OAuth sessions won't be updated on handle changes.
func WithSessionHandleUpdater(updater SessionHandleUpdater) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.sessionHandleUpdater = updater
	}
}

// WithUserBlockRepo sets the user block repository for indexing user-to-user blocks
// from the Jetstream firehose. If not set, block events will be ignored.
func WithUserBlockRepo(repo userblocks.Repository) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.userBlockRepo = repo
	}
}

// NewUserEventConsumer creates a new Jetstream consumer for user events
func NewUserEventConsumer(userService users.UserService, identityResolver identity.Resolver, wsURL, pdsFilter string, opts ...ConsumerOption) *UserEventConsumer {
	c := &UserEventConsumer{
		userService:      userService,
		identityResolver: identityResolver,
		wsURL:            wsURL,
		pdsFilter:        pdsFilter,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
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

	// We're interested in identity events (handle updates), account events (new users),
	// and commit events (profile updates from social.coves.actor.profile)
	switch event.Kind {
	case "identity":
		return c.handleIdentityEvent(ctx, &event)
	case "account":
		return c.handleAccountEvent(ctx, &event)
	case "commit":
		return c.handleCommitEvent(ctx, &event)
	default:
		// Ignore other event types
		return nil
	}
}

// HandleEvent processes a Jetstream event for user-related records.
// This is the public entry point used by tests and external callers.
func (c *UserEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	switch event.Kind {
	case "identity":
		return c.handleIdentityEvent(ctx, event)
	case "account":
		return c.handleAccountEvent(ctx, event)
	case "commit":
		return c.handleCommitEvent(ctx, event)
	default:
		return nil
	}
}

// Deprecated: HandleIdentityEventPublic is superseded by HandleEvent which routes
// all event kinds. Use HandleEvent for new code; this remains for existing tests.
func (c *UserEventConsumer) HandleIdentityEventPublic(ctx context.Context, event *JetstreamEvent) error {
	return c.handleIdentityEvent(ctx, event)
}

// handleIdentityEvent processes identity events (handle changes)
// NOTE: This only UPDATES existing users - it does NOT create new users.
// Users are created during OAuth login or signup, not from Jetstream events.
// This prevents indexing millions of Bluesky users who never interact with Coves.
func (c *UserEventConsumer) handleIdentityEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Identity == nil {
		return fmt.Errorf("identity event missing identity data")
	}

	did := event.Identity.Did
	handle := event.Identity.Handle

	if did == "" || handle == "" {
		return fmt.Errorf("identity event missing did or handle")
	}

	// Only process users who exist in our database (i.e., have used Coves before)
	existingUser, err := c.userService.GetUserByDID(ctx, did)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			// User doesn't exist in our database - skip this event
			// They'll be indexed when they actually interact with Coves (OAuth login, signup, etc.)
			// This prevents us from indexing millions of Bluesky users we don't care about
			return nil
		}
		// Database error - propagate so it can be retried
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	log.Printf("Identity event for known user: %s (%s)", handle, did)

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
			slog.Error("CRITICAL: failed to purge old handle cache",
				slog.String("handle", existingUser.Handle),
				slog.String("error", purgeErr.Error()))
		}

		// DID: did:plc:abc123 → alice.bsky.social (must be removed)
		if purgeErr := c.identityResolver.Purge(ctx, did); purgeErr != nil {
			slog.Error("CRITICAL: failed to purge DID cache",
				slog.String("did", did),
				slog.String("error", purgeErr.Error()))
		}

		// Update OAuth session handles to keep mobile/web sessions in sync
		// Failure here causes users to see stale handles in their active sessions
		if c.sessionHandleUpdater != nil {
			if sessionsUpdated, updateErr := c.sessionHandleUpdater.UpdateHandleByDID(ctx, did, handle); updateErr != nil {
				slog.Error("failed to update OAuth session handles (users may see stale handle)",
					slog.String("did", did),
					slog.String("new_handle", handle),
					slog.String("error", updateErr.Error()))
			} else if sessionsUpdated > 0 {
				log.Printf("Updated %d OAuth session(s) with new handle: %s", sessionsUpdated, handle)
			}
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

	// Account events don't include handle, so we skip them.
	// Users are indexed via OAuth login or signup, not from account events.
	return nil
}

// handleCommitEvent processes commit events for user-related collections.
// Routes to appropriate handler based on collection:
// - social.coves.actor.profile: Profile updates for users in our database
// - social.coves.actor.block: User-to-user block create/delete events
func (c *UserEventConsumer) handleCommitEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Commit == nil {
		slog.Warn("received nil commit in handleCommitEvent (malformed event)", slog.String("did", event.Did))
		return nil
	}

	switch event.Commit.Collection {
	case CovesProfileCollection:
		return c.handleProfileCommit(ctx, event)
	case CovesActorBlockCollection:
		return c.handleUserBlock(ctx, event.Did, event.Commit)
	default:
		return nil
	}
}

// handleProfileCommit processes profile commit events for users already in our database.
// This syncs profile data (displayName, bio, avatar, banner) from Coves profiles.
func (c *UserEventConsumer) handleProfileCommit(ctx context.Context, event *JetstreamEvent) error {
	// Profile handling requires userService
	if c.userService == nil {
		return nil
	}

	// Only process users who exist in our database
	_, err := c.userService.GetUserByDID(ctx, event.Did)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			// User doesn't exist in our database - skip this event
			// They'll be indexed when they actually interact with Coves
			return nil
		}
		// Database error - propagate so it can be retried
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	switch event.Commit.Operation {
	case "create", "update":
		return c.handleProfileUpdate(ctx, event.Did, event.Commit)
	case "delete":
		return c.handleProfileDelete(ctx, event.Did)
	default:
		return nil
	}
}

// handleProfileUpdate processes profile create/update operations
// Extracts displayName, description (bio), avatar, and banner from the record
func (c *UserEventConsumer) handleProfileUpdate(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		slog.Warn("received nil record in profile commit (profile update silently dropped)",
			slog.String("did", did),
			slog.String("operation", commit.Operation))
		return nil
	}

	input := users.UpdateProfileInput{}

	// Extract displayName
	if dn, ok := commit.Record["displayName"].(string); ok {
		input.DisplayName = &dn
	}

	// Extract description (bio)
	if desc, ok := commit.Record["description"].(string); ok {
		input.Bio = &desc
	}

	// Extract avatar CID from blob ref structure
	if avatarMap, ok := commit.Record["avatar"].(map[string]interface{}); ok {
		if cid, ok := extractBlobCID(avatarMap); ok {
			input.AvatarCID = &cid
		}
	}

	// Extract banner CID from blob ref structure
	if bannerMap, ok := commit.Record["banner"].(map[string]interface{}); ok {
		if cid, ok := extractBlobCID(bannerMap); ok {
			input.BannerCID = &cid
		}
	}

	_, err := c.userService.UpdateProfile(ctx, did, input)
	if err != nil {
		return fmt.Errorf("failed to update user profile: %w", err)
	}

	log.Printf("Updated profile for user %s", did)
	return nil
}

// handleProfileDelete processes profile delete operations
// Clears all profile fields by passing empty strings
func (c *UserEventConsumer) handleProfileDelete(ctx context.Context, did string) error {
	empty := ""
	input := users.UpdateProfileInput{
		DisplayName: &empty,
		Bio:         &empty,
		AvatarCID:   &empty,
		BannerCID:   &empty,
	}
	_, err := c.userService.UpdateProfile(ctx, did, input)
	if err != nil {
		return fmt.Errorf("failed to clear user profile: %w", err)
	}
	log.Printf("Cleared profile for user %s", did)
	return nil
}

// handleUserBlock processes user-to-user block create/delete events.
// CREATE operation = user blocked another user
// DELETE operation = user unblocked another user
func (c *UserEventConsumer) handleUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if c.userBlockRepo == nil {
		slog.Warn("user block event ignored: userBlockRepo not configured (WithUserBlockRepo not called)",
			slog.String("user_did", userDID),
			slog.String("operation", commit.Operation))
		return nil
	}

	switch commit.Operation {
	case "create":
		return c.createUserBlock(ctx, userDID, commit)
	case "delete":
		return c.deleteUserBlock(ctx, userDID, commit)
	default:
		// Update operations shouldn't happen on blocks, but ignore gracefully
		log.Printf("Ignoring unexpected operation on user block: %s (userDID=%s, rkey=%s)",
			commit.Operation, userDID, commit.RKey)
		return nil
	}
}

// createUserBlock indexes a new user-to-user block from the firehose.
func (c *UserEventConsumer) createUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("user block create event missing record data")
	}

	// Validate userDID format (untrusted firehose data)
	if !strings.HasPrefix(userDID, "did:") {
		return fmt.Errorf("invalid blocker DID format from firehose: %s", userDID)
	}

	// Extract blocked user DID from record's subject field
	blockedDID, ok := commit.Record["subject"].(string)
	if !ok {
		return fmt.Errorf("user block record missing subject field")
	}

	// Validate blockedDID format (untrusted firehose data)
	if !strings.HasPrefix(blockedDID, "did:") {
		return fmt.Errorf("invalid blocked DID format from firehose: %s", blockedDID)
	}

	// Validate rkey is non-empty before building AT-URI
	if commit.RKey == "" {
		return fmt.Errorf("user block create event missing rkey")
	}

	// Build AT-URI for the block record (lives in the blocker's repository)
	uri := fmt.Sprintf("at://%s/social.coves.actor.block/%s", userDID, commit.RKey)

	// Parse createdAt from record to preserve chronological ordering during replays
	block := &userblocks.UserBlock{
		BlockerDID: userDID,
		BlockedDID: blockedDID,
		BlockedAt:  utils.ParseCreatedAt(commit.Record),
		RecordURI:  uri,
		RecordCID:  commit.CID,
	}

	// Index the block (idempotent via ON CONFLICT DO UPDATE)
	_, err := c.userBlockRepo.BlockUser(ctx, block)
	if err != nil {
		if userblocks.IsConflict(err) {
			log.Printf("User block already indexed: %s -> %s", userDID, blockedDID)
			return nil
		}
		return fmt.Errorf("failed to index user block: %w", err)
	}

	log.Printf("Indexed user block: %s -> %s", userDID, blockedDID)
	return nil
}

// deleteUserBlock removes a user-to-user block from the index.
// DELETE operations don't include record data, so we look up the block by its URI.
func (c *UserEventConsumer) deleteUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Validate rkey is non-empty before building AT-URI
	if commit.RKey == "" {
		return fmt.Errorf("user block delete event missing rkey")
	}

	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.actor.block/%s", userDID, commit.RKey)

	// Look up the block to get the blocked DID
	block, err := c.userBlockRepo.GetBlockByURI(ctx, uri)
	if err != nil {
		if userblocks.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("User block already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find user block for deletion: %w", err)
	}

	// Remove the block from the index
	err = c.userBlockRepo.UnblockUser(ctx, userDID, block.BlockedDID)
	if err != nil {
		if userblocks.IsNotFound(err) {
			log.Printf("User block already removed: %s -> %s", userDID, block.BlockedDID)
			return nil
		}
		return fmt.Errorf("failed to remove user block: %w", err)
	}

	log.Printf("Removed user block: %s -> %s", userDID, block.BlockedDID)
	return nil
}
