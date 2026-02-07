package userblocks

import "errors"

// Sentinel errors for user block operations
var (
	// ErrBlockNotFound is returned when a block lookup finds no matching record
	ErrBlockNotFound = errors.New("user block not found")

	// ErrBlockAlreadyExists is returned when attempting to create a duplicate block
	ErrBlockAlreadyExists = errors.New("user already blocked")

	// ErrCannotBlockSelf is returned when a user attempts to block themselves
	ErrCannotBlockSelf = errors.New("cannot block yourself")
)

// IsNotFound returns true if the error is ErrBlockNotFound
func IsNotFound(err error) bool {
	return errors.Is(err, ErrBlockNotFound)
}

// IsConflict returns true if the error is ErrBlockAlreadyExists
func IsConflict(err error) bool {
	return errors.Is(err, ErrBlockAlreadyExists)
}

