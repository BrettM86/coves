package users

import "context"

// UserRepository defines the interface for user data persistence
type UserRepository interface {
	Create(ctx context.Context, user *User) (*User, error)
	GetByDID(ctx context.Context, did string) (*User, error)
	GetByHandle(ctx context.Context, handle string) (*User, error)
	UpdateHandle(ctx context.Context, did, newHandle string) (*User, error)
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