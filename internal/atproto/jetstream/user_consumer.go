package jetstream

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"
)

// CovesProfileCollection is the atProto collection for Coves user profiles.
// NOTE: Alias of users.ProfileCollection, the canonical definition — kept as an
// exported constant of this package because existing callers reference it here.
const CovesProfileCollection = users.ProfileCollection

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

// UserEventConsumer consumes user-related events from Jetstream.
// Connection management (WebSocket, cursor, retries, dead letters) lives in
// Connector; this type only implements EventHandler.
type UserEventConsumer struct {
	userService          users.UserService
	identityResolver     identity.Resolver
	sessionHandleUpdater SessionHandleUpdater  // Optional: updates OAuth sessions on handle change
	userBlockRepo        userblocks.Repository // Optional: indexes user-to-user blocks
	bridgeTrust          *BridgeTrust          // Optional: admits new identities hosted by trusted bridge PDSes
	revGate              *RevGate              // Optional: cross-feed ordering guard for commit events (nil = ungated)
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

// WithUserBridgeTrust enables discovery of bridged profiles. Unknown native
// users remain ignored, preserving the consumer's avoid-indexing-the-world
// policy.
func WithUserBridgeTrust(bt *BridgeTrust) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.bridgeTrust = bt
	}
}

// WithUserRevGate installs the per-record rev gate (see rev_gate.go) so profile
// and block commits are applied in repo commit order even when the same repo is
// carried by multiple Jetstream feeds. Without it, commit events are ungated.
func WithUserRevGate(gate *RevGate) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.revGate = gate
	}
}

// NewUserEventConsumer creates a new Jetstream consumer for user events
func NewUserEventConsumer(userService users.UserService, identityResolver identity.Resolver, opts ...ConsumerOption) *UserEventConsumer {
	c := &UserEventConsumer{
		userService:      userService,
		identityResolver: identityResolver,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RevGated reports whether this consumer applies the per-record rev gate (true when a
// gate was injected via WithUserRevGate). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *UserEventConsumer) RevGated() bool { return c.revGate != nil }

// HandleEvent implements EventHandler; it is invoked by the Connector for live
// events and by the DeadLetterRedriver for replays, so it must stay idempotent.
// The two callers may invoke the same consumer instance concurrently, so it must
// also hold no unguarded mutable in-memory state (all state lives in the DB).
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
//
// KNOWN LIMITATION (accepted): identity events carry NO rev (they are not repo
// commits), so cross-feed ordering of handle changes is NOT rev-gated. A lagging
// feed's stale identity event can transiently revert a handle until the next
// identity event for that DID arrives. Re-resolving the DID against PLC (the
// source of truth) on every identity event would fix this and is deliberately
// not implemented yet.
func (c *UserEventConsumer) handleIdentityEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Identity == nil {
		// PERMANENT: structurally invalid event — replays fail identically.
		return fmt.Errorf("%w: identity event missing identity data", ErrPermanentEvent)
	}

	did := event.Identity.Did
	handle := event.Identity.Handle

	if did == "" {
		// PERMANENT: structurally invalid event — replays fail identically.
		return fmt.Errorf("%w: identity event missing did", ErrPermanentEvent)
	}

	// A handle-less identity event is VALID, not malformed: Jetstream emits it
	// when an identity's handle is invalidated or tombstoned. We index only
	// known users and have no handle-invalid state to record, so there is
	// nothing to apply — skip. (Erroring here dead-lettered every such event
	// network-wide as a permanent failure: tens of thousands of junk DLQ rows
	// per day on the unfiltered identity stream.)
	if handle == "" {
		return nil
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
		// PERMANENT: structurally invalid event — replays fail identically.
		return fmt.Errorf("%w: account event missing account data", ErrPermanentEvent)
	}

	did := event.Account.Did
	if did == "" {
		return fmt.Errorf("%w: account event missing did", ErrPermanentEvent)
	}

	// Account events don't include handle, so we skip them.
	// Users are indexed via OAuth login or signup, not from account events.
	return nil
}

// handleCommitEvent processes commit events for user-related collections.
// Routes to appropriate handler based on collection:
//   - social.coves.actor.profile: Profile updates for known users, plus discovery
//     of unknown identities hosted by an explicitly trusted bridge PDS
//   - social.coves.actor.block: User-to-user block create/delete events
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

// handleProfileCommit processes profile commit events. Native profiles still update
// only existing users. A previously unknown identity may be indexed when DID
// resolution proves it is hosted by an explicitly trusted bridge PDS; this is how
// virtual bridge users enter the AppView without opening the door to indexing every
// profile on the network.
func (c *UserEventConsumer) handleProfileCommit(ctx context.Context, event *JetstreamEvent) error {
	// Profile handling requires userService
	if c.userService == nil {
		return nil
	}

	// Only process users who exist in our database, except trusted bridge
	// profiles whose first event is the profile itself.
	existingUser, err := c.userService.GetUserByDID(ctx, event.Did)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			existingUser = nil // freshly discovered identity: recency guard below must not apply
			if event.Commit.Operation != "create" && event.Commit.Operation != "update" {
				return nil
			}
			if c.identityResolver == nil {
				return nil
			}
			resolved, resolveErr := c.identityResolver.Resolve(ctx, event.Did)
			if resolveErr != nil {
				return fmt.Errorf("failed to resolve unknown profile identity %s: %w", event.Did, resolveErr)
			}
			if resolved == nil || resolved.DID != event.Did || !c.bridgeTrust.TrustsPDS(resolved.PDSURL) {
				return nil
			}
			if indexErr := c.userService.IndexUser(ctx, resolved.DID, resolved.Handle, resolved.PDSURL); indexErr != nil {
				return fmt.Errorf("failed to index trusted bridge profile identity %s: %w", event.Did, indexErr)
			}
		} else {
			// Database error - propagate so the connector can report it.
			return fmt.Errorf("failed to check if user exists: %w", err)
		}
	}

	// REV GATE: the exact cross-feed ordering guard. rev is the repo's own
	// monotonic commit TID, so unlike the wall-clock guard below it orders the
	// same repo's events correctly across feeds with hours of skew. Checked
	// before the write and advanced after it (check→write→advance; the write is
	// idempotent, so a crash in between replays safely).
	uri := commitRecordURI(event.Did, event.Commit)
	stale, err := c.revGate.IsStale(ctx, uri, event.Commit.Rev)
	if err != nil {
		return err
	}
	if stale {
		logSkippedStaleRev(ConsumerUsers, event.Commit.Operation, uri, event.Commit.Rev)
		return nil
	}

	// RECENCY GUARD: a redriven (DeadLetterRedriver) or rewound profile event can
	// arrive AFTER a newer profile write was already applied; applying it would
	// silently revert the newer profile. users.updated_at is bumped by every
	// UpdateProfile/UpdateHandle, so an event older than the row's last successful
	// write is skipped. Skipping is SUCCESS — the newer state wins.
	//
	// CAVEAT (unlike posts/comments, which store the Jetstream event time as their
	// watermark): updated_at is AppView wall clock, so this compares across clock
	// domains. That is safe for redrives (replayed minutes after the newer write)
	// and for human-paced profile edits; only two writes for the same user landing
	// within the ingest lag could be spuriously skipped, and the next profile edit
	// self-heals. The rev gate above is exact for events that carry a rev; this
	// guard remains for rev-less events (old dead letters, synthetic tests).
	if evTime, ok := eventTime(event.TimeUS); ok &&
		existingUser != nil && !existingUser.UpdatedAt.IsZero() && !existingUser.UpdatedAt.Before(evTime) {
		log.Printf("INFO: skipping stale profile event for %s (event time %s <= last user write %s; newer state already applied)",
			event.Did, evTime.Format(time.RFC3339Nano), existingUser.UpdatedAt.Format(time.RFC3339Nano))
		return nil
	}

	var opErr error
	switch event.Commit.Operation {
	case "create", "update":
		opErr = c.handleProfileUpdate(ctx, event.Did, event.Commit)
	case "delete":
		opErr = c.handleProfileDelete(ctx, event.Did)
	default:
		return nil
	}
	if opErr != nil {
		return opErr
	}
	return c.revGate.Advance(ctx, uri, event.Commit.Rev)
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

	// REV GATE (check→write→advance, see rev_gate.go). The gate row survives
	// the hard delete of the block row, so a stale cross-feed copy of the
	// block's CREATE arriving after the unblock cannot re-index a phantom block.
	return applyGated(ctx, c.revGate, ConsumerUsers, userDID, commit, func() error {
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
	})
}

// createUserBlock indexes a new user-to-user block from the firehose.
func (c *UserEventConsumer) createUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: user block create event missing record data", ErrPermanentEvent)
	}

	// The rejections below are PERMANENT (ErrPermanentEvent): they depend only on
	// the immutable event payload, so retries and redrives would fail identically.

	// Validate userDID format (untrusted firehose data)
	if !strings.HasPrefix(userDID, "did:") {
		return fmt.Errorf("%w: invalid blocker DID format from firehose: %s", ErrPermanentEvent, userDID)
	}

	// Extract blocked user DID from record's subject field
	blockedDID, ok := commit.Record["subject"].(string)
	if !ok {
		return fmt.Errorf("%w: user block record missing subject field", ErrPermanentEvent)
	}

	// Validate blockedDID format (untrusted firehose data)
	if !strings.HasPrefix(blockedDID, "did:") {
		return fmt.Errorf("%w: invalid blocked DID format from firehose: %s", ErrPermanentEvent, blockedDID)
	}

	// Validate rkey is non-empty before building AT-URI
	if commit.RKey == "" {
		return fmt.Errorf("%w: user block create event missing rkey", ErrPermanentEvent)
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
	// Validate rkey is non-empty before building AT-URI.
	// PERMANENT: the rkey is immutable — replays fail identically.
	if commit.RKey == "" {
		return fmt.Errorf("%w: user block delete event missing rkey", ErrPermanentEvent)
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
