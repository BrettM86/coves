// Package errors defines the error types shared across every Coves domain
// package.
//
// Each domain used to declare its own ValidationError, NotFoundError, and
// friends. Because those were distinct Go types with identical shapes, a
// handler could not ask "is this a validation failure?" in one place — it had
// to call posts.IsValidationError, then communities.IsValidationError, then
// aggregators.IsValidationError, and any domain a handler forgot silently fell
// through to a 500.
//
// Domain packages now alias these types:
//
//	type ValidationError = coreerrors.ValidationError
//
// A Go type alias is the *same* type, not a similar one, so the ValidationError
// of every domain that aliases it is matched by a single errors.As at the
// boundary, while each package keeps its own familiar constructors and
// predicates.
//
// Six packages participate today: posts, communities, aggregators,
// communityFeeds, discover, and timeline. Domains that signal validation
// failures with sentinel errors instead — comments, for one — still need their
// own predicate at the boundary.
package errors

import (
	"errors"
	"fmt"
)

// Shared sentinel errors, kept to the two the typed errors below bridge to.
// Domain packages define their own more specific sentinels; speculative ones
// here would rebuild the duplicated error surface this package exists to
// remove.
var (
	ErrNotFound      = errors.New("resource not found")
	ErrAlreadyExists = errors.New("resource already exists")
)

// ValidationError reports invalid input, with the offending field named.
//
// Error() is surfaced to clients as the human-readable message of an XRPC
// error response, so it stays short and free of internal detail. The stable
// machine-readable contract is the error *code* the handler chooses, not this
// string.
type ValidationError struct {
	// Field is the input that failed validation, named as the client named
	// it (a lexicon property, not an internal struct field).
	Field string

	// Message explains what was wrong with it.
	Message string

	// Err is the underlying cause, when there is one. Without it, wrapping a
	// typed error to add field context would flatten it to a string and make
	// the original sentinel unreachable from errors.Is.
	Err error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *ValidationError) Unwrap() error {
	return e.Err
}

// NewValidationError reports that field failed validation for the given
// reason.
func NewValidationError(field, message string) *ValidationError {
	return &ValidationError{Field: field, Message: message}
}

// NewValidationErrorFrom adds field context to an existing error while keeping
// cause matchable through errors.Is — which matters when cause is a sentinel
// from a package like internal/validation.
func NewValidationErrorFrom(field string, cause error) *ValidationError {
	return &ValidationError{
		Field:   field,
		Message: cause.Error(),
		Err:     cause,
	}
}

// IsValidationError reports whether err is, or wraps, a ValidationError.
func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr)
}

// NotFoundError reports that a specific resource does not exist.
type NotFoundError struct {
	// Resource is the kind of thing that was missing ("post", "community").
	Resource string

	// ID is the identifier that was looked up.
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s not found: %s", e.Resource, e.ID)
}

// Is makes every NotFoundError match the shared ErrNotFound sentinel, so a
// caller can test for "missing" without caring which resource was missing.
func (e *NotFoundError) Is(target error) bool {
	return target == ErrNotFound
}

// NewNotFoundError reports that the identified resource does not exist.
func NewNotFoundError(resource, id string) *NotFoundError {
	return &NotFoundError{Resource: resource, ID: id}
}

// IsNotFound reports whether err is, or wraps, a NotFoundError.
func IsNotFound(err error) bool {
	var notFoundErr *NotFoundError
	return errors.As(err, &notFoundErr)
}

// ConflictError reports that a resource already exists with the given value,
// typically a uniqueness violation.
type ConflictError struct {
	// Resource is the kind of thing that already existed ("community").
	Resource string

	// Field and Value identify the colliding value ("handle", "!go@coves").
	Field string
	Value string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s with %s %q already exists", e.Resource, e.Field, e.Value)
}

// Is makes every ConflictError match the shared ErrAlreadyExists sentinel.
func (e *ConflictError) Is(target error) bool {
	return target == ErrAlreadyExists
}

// NewConflictError reports a uniqueness violation on the given field.
func NewConflictError(resource, field, value string) *ConflictError {
	return &ConflictError{Resource: resource, Field: field, Value: value}
}

// IsConflict reports whether err is, or wraps, a ConflictError.
func IsConflict(err error) bool {
	var conflictErr *ConflictError
	return errors.As(err, &conflictErr)
}
