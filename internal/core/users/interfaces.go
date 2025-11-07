package users

import "context"

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
}

// UserService defines the interface for user business logic
type UserService interface {
	CreateUser(ctx context.Context, req CreateUserRequest) (*User, error)
	GetUserByDID(ctx context.Context, did string) (*User, error)
	GetUserByHandle(ctx context.Context, handle string) (*User, error)
	UpdateHandle(ctx context.Context, did, newHandle string) (*User, error)
	ResolveHandleToDID(ctx context.Context, handle string) (string, error)
	RegisterAccount(ctx context.Context, req RegisterAccountRequest) (*RegisterAccountResponse, error)
}
