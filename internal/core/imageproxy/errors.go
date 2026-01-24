package imageproxy

import "errors"

var (
	// ErrInvalidPreset is returned when a preset name is not found in the preset registry.
	ErrInvalidPreset = errors.New("invalid image preset")

	// ErrInvalidDID is returned when a DID string does not match expected atproto DID format.
	ErrInvalidDID = errors.New("invalid DID format")

	// ErrInvalidCID is returned when a CID string is not a valid content identifier.
	ErrInvalidCID = errors.New("invalid CID format")

	// ErrPDSFetchFailed is returned when fetching a blob from a PDS fails for any reason.
	ErrPDSFetchFailed = errors.New("failed to fetch blob from PDS")

	// ErrPDSNotFound is returned when the requested blob does not exist on the PDS.
	ErrPDSNotFound = errors.New("blob not found on PDS")

	// ErrPDSTimeout is returned when a PDS request exceeds the configured timeout.
	ErrPDSTimeout = errors.New("PDS request timed out")

	// ErrUnsupportedFormat is returned when the source image format cannot be processed.
	ErrUnsupportedFormat = errors.New("unsupported image format")

	// ErrImageTooLarge is returned when the source image exceeds the maximum allowed size.
	ErrImageTooLarge = errors.New("source image exceeds size limit")

	// ErrProcessingFailed is returned when image processing fails for any reason.
	ErrProcessingFailed = errors.New("image processing failed")

	// ErrNilDependency is returned when a required dependency is nil.
	ErrNilDependency = errors.New("required dependency is nil")
)
