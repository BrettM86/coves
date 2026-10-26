package media_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

type recordingUploader struct {
	uploads  int
	data     []byte // the most recently stored bytes
	mimeType string // the MIME type they were stored under
}

func (u *recordingUploader) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	u.uploads++
	u.data = append([]byte(nil), data...)
	u.mimeType = mimeType
	return &blobs.BlobRef{Type: "blob", Ref: map[string]string{"$link": "bafkreirecordinguploader"}, MimeType: mimeType, Size: len(data)}, nil
}

func TestUploadRefusesRotatedReEncodeOverTenMegabytesBeforeUploading(t *testing.T) {
	input := testkit.JPEGThatGrowsPastTenMegabytesWhenRotated()
	if len(input) > 10_000_000 {
		t.Fatalf("fixture input is %d bytes, want at most 10,000,000", len(input))
	}
	uploader := &recordingUploader{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pipeline(t, 1).Upload(ctx, uploader, media.Input{Data: input, DeclaredContentType: "image/jpeg"})

	expectCode(t, err, media.ErrorCodeImageTooLarge)
	if uploader.uploads != 0 {
		t.Errorf("uploads: got %d, want 0", uploader.uploads)
	}
}
