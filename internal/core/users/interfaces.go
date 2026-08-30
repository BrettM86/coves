package users

import "context"

// UpdateProfileInput contains the fields that can be updated on a user's profile.
// Nil values mean "don't change this field" - only non-nil values are updated.
// Empty string values (*string pointing to "") will clear the field in the database.
type UpdateProfileInput struct {
	DisplayName *string
	Bio         *string
	AvatarCID   *string
	BannerCID   *string
}

// ErasureMarkerStore reads and clears the migration-036 erasure markers.
//
// It is a SEPARATE, OPTIONAL interface rather than a method on UserRepository,
// and detected with a type assertion at the call sites that need it. Adding it
// to UserRepository would oblige every implementation to answer a question only
// the PostgreSQL one can — and a double that answered "not erased" by default
// would be a gate that fails open, which is the single outcome this marker
// exists to prevent. Nothing in production is a repository without it.
//
// # A REPOSITORY THAT LACKS IT MEANS OPPOSITE THINGS AT THE CALL SITES
//
// This is the canonical statement of that split; the methods and interfaces
// that resolve it point here rather than restating it.
//
// IndexUser treats an absent lookup as "gate disabled" and indexes anyway. Its
// callers are the firehose consumers and signup — nobody chooses when a
// firehose event arrives, a freshly minted DID cannot carry a marker, and a
// disabled gate degrades to the behaviour the AppView had before the marker
// existed.
//
// userService.IsAccountDeleted and IndexAuthenticatedUser treat an absent
// store as an ERROR and fail closed. IsAccountDeleted answers the
// unauthenticated registration endpoint, where somebody IS on the other side
// choosing the moment, and "I cannot tell whether this account was erased" must
// not read as "this account was not erased" — the second one registers it.
// IndexAuthenticatedUser fails closed for the mirror-image reason: indexing
// without being able to clear the marker writes a row for an account the
// ingestion gate still refuses, which looks present and publishes nothing.
type ErasureMarkerStore interface {
	// IsAccountDeleted reports whether a DID names an account this AppView was
	// asked to erase.
	IsAccountDeleted(ctx context.Context, did string) (bool, error)

	// ReinstateAccount removes the marker, and it is the marker's ONLY exit.
	// Nothing clears one as a side effect — re-opening ingestion for content the
	// AppView was asked to forget is a decision a caller states, not one reached
	// by writing a row.
	//
	// It is idempotent: a DID with no marker is not an error, because the
	// callers cannot know whether one is there and should not have to ask. The
	// bool reports whether there was one, which is how a caller tells an account
	// returning from erasure from the ordinary login that removes nothing.
	ReinstateAccount(ctx context.Context, did string) (reinstated bool, err error)
}

// UserRepository defines the interface for user data persistence
type UserRepository interface {
	Create(ctx context.Context, user *User) (*User, error)
	GetByDID(ctx context.Context, did string) (*User, error)
	GetByHandle(ctx context.Context, handle string) (*User, error)
	UpdateHandle(ctx context.Context, did, newHandle string) (*User, error)

	// GetByDIDs retrieves multiple users by their DIDs in a single batch query.
	// Returns a map of DID → User for efficient lookups.
	// Missing users are not included in the result map (no error for missing users).
	// Returns error only on database failures or validation errors (invalid DIDs, batch too large).
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//   - dids: Array of DIDs to retrieve (must start with "did:", max 1000 items)
	//
	// Returns:
	//   - map[string]*User: Map of DID → User for found users
	//   - error: Validation or database errors (not errors for missing users)
	//
	// Example:
	//   userMap, err := repo.GetByDIDs(ctx, []string{"did:plc:abc", "did:plc:xyz"})
	//   if err != nil { return err }
	//   if user, found := userMap["did:plc:abc"]; found {
	//       // Use user
	//   }
	GetByDIDs(ctx context.Context, dids []string) (map[string]*User, error)

	// GetProfileStats retrieves aggregated statistics for a user profile.
	// Returns counts of posts, comments, subscriptions, memberships, and total reputation.
	GetProfileStats(ctx context.Context, did string) (*ProfileStats, error)

	// UpdateProfile updates a user's profile fields (display name, bio, avatar, banner).
	// Nil values in the input mean "don't change this field" - only non-nil values are updated.
	// Empty string values will clear the field in the database.
	// Returns the updated user with all fields populated.
	// Returns ErrUserNotFound if the user does not exist.
	UpdateProfile(ctx context.Context, did string, input UpdateProfileInput) (*User, error)

	// Delete removes a user and all associated data from the AppView database.
	// This performs a cascading delete across all tables that reference the user's DID.
	// The operation is atomic - either all data is deleted or none.
	//
	// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
	// The user's identity remains intact for use with other atProto apps.
	//
	// Tables cleaned up (in order):
	//   1. oauth_sessions (explicit DELETE)
	//   2. oauth_requests (explicit DELETE)
	//   3. community_subscriptions (explicit DELETE)
	//   4. community_memberships (explicit DELETE)
	//   5. community_blocks (explicit DELETE)
	//   6. user_blocks (explicit DELETE - both directions)
	//   7. comments (explicit DELETE)
	//   8. votes (explicit DELETE - FK removed in migration 014)
	//   9. community_post_admissions for this author's posts (explicit DELETE
	//      by DID-prefix match on post_uri - no FK to posts by design,
	//      migration 034, and an admission's subject post may never have been
	//      indexed at all, so the sweep cannot go through the posts table)
	//   10. posts (explicit DELETE - fk_author CASCADE removed by migration 034)
	//   11. users
	//
	// Returns ErrUserNotFound if the user does not exist.
	// Returns InvalidDIDError if the DID format is invalid.
	Delete(ctx context.Context, did string) error
}

// UserService defines the interface for user business logic
type UserService interface {
	CreateUser(ctx context.Context, req CreateUserRequest) (*User, error)
	GetUserByDID(ctx context.Context, did string) (*User, error)
	GetUserByHandle(ctx context.Context, handle string) (*User, error)
	UpdateHandle(ctx context.Context, did, newHandle string) (*User, error)
	ResolveHandleToDID(ctx context.Context, handle string) (string, error)
	RegisterAccount(ctx context.Context, req RegisterAccountRequest) (*RegisterAccountResponse, error)

	// RequestSignupToken verifies a Cloudflare Turnstile token and, on success,
	// mints a single-use invite code via the PDS admin API. This endpoint does
	// NOT validate handle/email — those checks live on the actual signup
	// endpoint (social.coves.actor.signup) where uniqueness and lexicon rules
	// are enforced authoritatively. Returns:
	//   - ErrSignupTokenDisabled  → endpoint not configured (503)
	//   - InvalidCaptchaError     → captcha rejected by Cloudflare (403)
	//   - CaptchaUnavailableError → Cloudflare unreachable (503)
	//   - ErrPDSAdminUnavailable  → PDS admin transport/decode failure (503)
	//   - InviteMintError         → PDS admin responded with non-2xx (500)
	RequestSignupToken(ctx context.Context, req RequestSignupTokenRequest) (*RequestSignupTokenResponse, error)

	// IndexUser creates or updates a user in the local database.
	// This is idempotent - calling it multiple times with the same DID is safe.
	//
	// Its callers are the firehose consumers and signup, none of which can say
	// the account itself is present — so it is GATED and refuses a DID the
	// erasure marker names. The OAuth callback uses IndexAuthenticatedUser
	// instead; signup stays here because a DID the PDS has just minted cannot
	// carry a marker.
	IndexUser(ctx context.Context, did, handle, pdsURL string) error

	// IndexAuthenticatedUser indexes a user who has just proven who they are —
	// today that means an OAuth login, where the PDS attested the DID and the
	// handle was verified bidirectionally.
	//
	// It is a SEPARATE method from IndexUser rather than a flag on it, because
	// the two differ on the one question the erasure marker asks: whether the
	// account itself is asking to come back. A firehose event says only that
	// some repo emitted a record, so IndexUser stays gated and refuses an
	// erased DID. An authenticated login is the account, so this method
	// reinstates it — clearing the marker and indexing normally.
	//
	// This is the marker's only exit. Nothing else may clear it.
	IndexAuthenticatedUser(ctx context.Context, did, handle, pdsURL string) error

	// IsAccountDeleted reports whether a DID names an account this AppView was
	// asked to erase (migration 036).
	//
	// It is on the SERVICE, not only on the repository's optional
	// ErasureMarkerStore, because the callers that must consult it are handlers:
	// they hold a UserService and nothing below it. Registration is the first
	// such caller. See ErasureMarkerStore for why a repository that cannot
	// answer makes this an error rather than a "no".
	IsAccountDeleted(ctx context.Context, did string) (bool, error)

	// GetProfile retrieves a user's full profile with aggregated statistics.
	// Returns a ProfileViewDetailed matching the social.coves.actor.defs#profileViewDetailed lexicon.
	// Avatar and Banner CIDs are transformed to URLs using the user's PDS URL.
	GetProfile(ctx context.Context, did string) (*ProfileViewDetailed, error)

	// UpdateProfile updates a user's profile fields (display name, bio, avatar, banner).
	// Nil values in the input mean "don't change this field" - only non-nil values are updated.
	// Empty string values will clear the field in the database.
	// Returns the updated user with all fields populated.
	// Returns ErrUserNotFound if the user does not exist.
	UpdateProfile(ctx context.Context, did string, input UpdateProfileInput) (*User, error)

	// DeleteAccount removes a user and all associated data from the Coves AppView.
	// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
	// The user's identity remains intact for use with other atProto apps.
	//
	// Authorization: The caller must be the account owner. The XRPC handler extracts
	// the authenticated user's DID from the OAuth session context and passes it here.
	// This ensures users can ONLY delete their own accounts.
	//
	// This operation is required for Google Play compliance (account deletion requirement).
	//
	// The operation is atomic - either all data is deleted or none.
	// Logs the deletion event for audit trail (DID, handle, timestamp).
	//
	// Returns ErrUserNotFound if the user does not exist.
	// Returns InvalidDIDError if the DID format is invalid.
	DeleteAccount(ctx context.Context, did string) error
}
