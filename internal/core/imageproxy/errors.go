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

	// ErrPDSBlocked is returned when the SSRF guard refuses the PDS URL — its
	// address is private, reserved or loopback, or its shape is one this
	// service will not dial.
	//
	// IT IS DISTINCT FROM ErrPDSFetchFailed IN-PROCESS AND IDENTICAL ON THE
	// WIRE. A caller that cannot tell a security refusal from a network failure
	// logs both the same way and retries both the same way, which is why the
	// sentinel exists at all. But the endpoint being refused comes from a DID
	// document a stranger minted, so a distinguishable RESPONSE would hand that
	// stranger a better oracle than the one this guard removes — it would say
	// "this address is internal" rather than merely "something happened here".
	// handleServiceError therefore serves it as the same 502, with the same
	// body, as an ordinary fetch failure.
	ErrPDSBlocked = errors.New("PDS URL refused by the SSRF guard")

	// ErrPDSNotFound is returned when the requested blob does not exist on the PDS.
	ErrPDSNotFound = errors.New("blob not found on PDS")

	// ErrPDSTimeout is returned when a PDS request exceeds the configured timeout.
	ErrPDSTimeout = errors.New("PDS request timed out")

	// ErrUnsupportedFormat is returned when the source image format cannot be processed.
	ErrUnsupportedFormat = errors.New("unsupported image format")

	// ErrImageTooLarge is returned by the fetcher when the source blob exceeds
	// the byte cap.
	ErrImageTooLarge = errors.New("source image exceeds size limit")

	// ErrImageTooManyPixels is returned when the dimensions an image header
	// declares multiply to more than the processor's pixel budget. It is
	// distinct from ErrImageTooLarge, which is the fetcher's cap on BYTES: a
	// 47-byte file can declare a 12000×12000 frame, so the two limits guard
	// different resources and a log line has to say which one tripped. On
	// the wire both are the same 400; the client did nothing different.
	ErrImageTooManyPixels = errors.New("source image declares more pixels than the budget allows")

	// ErrProcessingFailed is returned when image processing fails for any reason.
	ErrProcessingFailed = errors.New("image processing failed")

	// ErrProcessorBusy is returned when every processing slot is occupied and
	// the request could not obtain one within the configured queue wait, or
	// its context ended while it was waiting. Decoding is the one step whose
	// memory cost is set by a header the caller controls, independent of the
	// byte count the fetcher already capped: a few dozen bytes can demand
	// hundreds of megabytes. That amplification is why decoding alone is
	// bounded by a fixed number of slots; a refusal here is load shedding,
	// not a fault in the image.
	ErrProcessorBusy = errors.New("image processor is at capacity")

	// ErrNilDependency is returned when a required dependency is nil.
	ErrNilDependency = errors.New("required dependency is nil")
)
