package errors

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound         = errors.New("resource not found")
	ErrAlreadyExists    = errors.New("resource already exists")
	ErrInvalidInput     = errors.New("invalid input")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrForbidden        = errors.New("forbidden")
	ErrInternal         = errors.New("internal server error")
	ErrDatabaseError    = errors.New("database error")
	ErrValidationFailed = errors.New("validation failed")
)

type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("validation error on field '%s': %s", e.Field, e.Message)
}

type ConflictError struct {
	Resource string
	Field    string
	Value    string
}

func (e ConflictError) Error() string {
	return fmt.Sprintf("%s with %s '%s' already exists", e.Resource, e.Field, e.Value)
}

type NotFoundError struct {
	ID       interface{}
	Resource string
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s with ID '%v' not found", e.Resource, e.ID)
}

func NewValidationError(field, message string) error {
	return ValidationError{
		Field:   field,
		Message: message,
	}
}

func NewConflictError(resource, field, value string) error {
	return ConflictError{
		Resource: resource,
		Field:    field,
		Value:    value,
	}
}

func NewNotFoundError(resource string, id interface{}) error {
	return NotFoundError{
		Resource: resource,
		ID:       id,
	}
}
