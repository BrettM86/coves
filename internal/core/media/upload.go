package media

import (
	"context"
	"fmt"

	"Coves/internal/core/blobs"
)

// ErrorCodePDSUploadFailed reports that the caller's PDS did not store the
// sanitized image.
const ErrorCodePDSUploadFailed ErrorCode = "PDSUploadFailed"

// BlobUploader stores blob bytes in a repository; posts.AuthorRepo satisfies it.
type BlobUploader interface {
	UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error)
}

// AspectRatio is the stored image's width and height after orientation.
type AspectRatio struct{ Width, Height int }

// UploadedImage describes a sanitized image stored in the caller's repository.
type UploadedImage struct {
	Blob *blobs.BlobRef
	// MimeType is the format of the stored bytes, never the client's
	// declared Content-Type: an oriented or downscaled WebP is stored as
	// JPEG or PNG.
	MimeType    string
	Size        int
	AspectRatio AspectRatio
}

var uploadFormats = []Format{FormatJPEG, FormatPNG, FormatWebP, FormatGIF}

// Upload sanitizes in.Data (accepting JPEG, PNG, WebP and GIF; in.Accept is
// ignored), downscales a still image whose long edge after orientation exceeds
// the configured maximum, and stores the result through uploader. Output over
// 10,000,000 bytes, which re-encoding can produce from a smaller input, is
// refused with ErrorCodeImageTooLarge before anything is uploaded. An uploader
// failure is returned as ErrorCodePDSUploadFailed.
func (p *Pipeline) Upload(ctx context.Context, uploader BlobUploader, in Input) (UploadedImage, error) {
	in.Accept = uploadFormats
	result, err := p.ingest(ctx, in, p.maxLongEdge)
	if err != nil {
		return UploadedImage{}, err
	}

	return storeUpload(ctx, uploader, result)
}

func storeUpload(ctx context.Context, uploader BlobUploader, result Result) (UploadedImage, error) {
	mimeType := string(result.Format)
	blob, err := uploader.UploadBlob(ctx, result.Data, mimeType)
	if err != nil {
		return UploadedImage{}, &Error{Code: ErrorCodePDSUploadFailed, Err: fmt.Errorf("uploading %s blob: %w", mimeType, err)}
	}
	if blob == nil {
		return UploadedImage{}, &Error{Code: ErrorCodePDSUploadFailed, Err: fmt.Errorf("uploading %s blob: the PDS returned no blob reference", mimeType)}
	}
	return UploadedImage{
		Blob:        blob,
		MimeType:    mimeType,
		Size:        len(result.Data),
		AspectRatio: AspectRatio{Width: result.Width, Height: result.Height},
	}, nil
}
