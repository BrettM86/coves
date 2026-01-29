package adminreports

import "errors"

var (
	// ErrInvalidReason indicates the report reason is not a valid category
	ErrInvalidReason = errors.New("invalid report reason: must be one of csam, doxing, harassment, spam, illegal, other")

	// ErrInvalidStatus indicates the report status is not a valid value
	ErrInvalidStatus = errors.New("invalid report status: must be one of open, reviewing, resolved, dismissed")

	// ErrInvalidTarget indicates the target URI is malformed or invalid
	ErrInvalidTarget = errors.New("invalid target URI: must be a valid AT Protocol URI starting with at://")

	// ErrExplanationTooLong indicates the explanation exceeds the maximum length
	ErrExplanationTooLong = errors.New("explanation exceeds maximum length of 1000 characters")

	// ErrReporterRequired indicates the reporter DID was not provided
	ErrReporterRequired = errors.New("reporter DID is required")

	// ErrReportNotFound indicates the requested report does not exist
	ErrReportNotFound = errors.New("report not found")

	// ErrInvalidTargetType indicates the target type is not a valid value
	ErrInvalidTargetType = errors.New("invalid target type: must be one of post, comment")
)

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	return errors.Is(err, ErrInvalidReason) ||
		errors.Is(err, ErrInvalidStatus) ||
		errors.Is(err, ErrInvalidTarget) ||
		errors.Is(err, ErrExplanationTooLong) ||
		errors.Is(err, ErrReporterRequired) ||
		errors.Is(err, ErrInvalidTargetType)
}

// IsNotFound checks if an error is a "not found" error
func IsNotFound(err error) bool {
	return errors.Is(err, ErrReportNotFound)
}
