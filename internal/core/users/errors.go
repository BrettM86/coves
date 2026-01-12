package users

import (
	"errors"
	"fmt"
)

// Sentinel errors for common user operations
var (
	// ErrUserNotFound is returned when a user lookup finds no matching record
	ErrUserNotFound = errors.New("user not found")

	// ErrHandleAlreadyTaken is returned when attempting to use a handle that belongs to another user
	ErrHandleAlreadyTaken = errors.New("handle already taken")
)

// Domain errors for user service operations
// These map to lexicon error types defined in social.coves.actor.signup

type InvalidHandleError struct {
	Handle string
	Reason string
}

func (e *InvalidHandleError) Error() string {
	return fmt.Sprintf("invalid handle %q: %s", e.Handle, e.Reason)
}

type HandleNotAvailableError struct {
	Handle string
}

func (e *HandleNotAvailableError) Error() string {
	return fmt.Sprintf("handle %q is not available", e.Handle)
}

type InvalidInviteCodeError struct {
	Code string
}

func (e *InvalidInviteCodeError) Error() string {
	return "invalid or expired invite code"
}

type InvalidEmailError struct {
	Email string
}

func (e *InvalidEmailError) Error() string {
	return fmt.Sprintf("invalid email address: %q", e.Email)
}

type WeakPasswordError struct {
	Reason string
}

func (e *WeakPasswordError) Error() string {
	return fmt.Sprintf("password does not meet strength requirements: %s", e.Reason)
}

// PDSError wraps errors from the PDS that we couldn't map to domain errors
type PDSError struct {
	Message    string
	StatusCode int
}

func (e *PDSError) Error() string {
	return fmt.Sprintf("PDS error (%d): %s", e.StatusCode, e.Message)
}

// InvalidDIDError is returned when a DID does not meet format requirements
type InvalidDIDError struct {
	DID    string
	Reason string
}

func (e *InvalidDIDError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("invalid DID %q: %s", e.DID, e.Reason)
	}
	return fmt.Sprintf("invalid DID %q: must start with 'did:'", e.DID)
}
