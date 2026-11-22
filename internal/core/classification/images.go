package classification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
)

// MaxImagesPerRequest is the most images one classification request carries.
const MaxImagesPerRequest = 4

// Image is one prepared image sent with a classification request.
type Image struct {
	ContentType string
	Data        []byte
}

// ImageRef is a blob a subject shows: the DID whose repository holds it and
// its CID.
type ImageRef struct {
	OwnerDID string
	CID      string
}

// previewPreset is the image proxy preset images are classified at: the size
// a viewer sees in a post preview.
const previewPreset = "content_preview"

// PostImages returns the image refs of a post embed owned by ownerDID and the
// number of image slots skipped: a gallery entry or link thumbnail that is
// not a valid blob counts as skipped, and a video counts as one skipped slot
// with nothing of it, not even its thumbnail, sent. An embed that does not
// decode yields nothing.
func PostImages(ownerDID, embedJSON string) ([]ImageRef, int) {
	embed, ok := decodeEmbed(embedJSON)
	if !ok {
		return nil, 0
	}
	if embed["$type"] == embedblobs.TypeVideo {
		return nil, 1
	}
	references, skipped := embedblobs.PostReferencesAndSkipped(embed)
	return ownedRefs(ownerDID, references), skipped
}

// CommentImages returns the image refs of a comment embed owned by ownerDID
// and the number of image slots skipped.
func CommentImages(ownerDID, embedJSON string) ([]ImageRef, int) {
	embed, ok := decodeEmbed(embedJSON)
	if !ok {
		return nil, 0
	}
	references, skipped := embedblobs.CommentReferencesAndSkipped(embed)
	return ownedRefs(ownerDID, references), skipped
}

// CommunityImages returns a community's avatar and banner refs, in that
// order; an empty or invalid CID is omitted.
func CommunityImages(communityDID, avatarCID, bannerCID string) []ImageRef {
	return ownedRefs(communityDID, embedblobs.ProfileReferences(avatarCID, bannerCID))
}

// decodeEmbed decodes a stored embed, reporting false for an empty or
// malformed one.
func decodeEmbed(embedJSON string) (map[string]interface{}, bool) {
	if embedJSON == "" {
		return nil, false
	}
	var embed map[string]interface{}
	if err := json.Unmarshal([]byte(embedJSON), &embed); err != nil {
		return nil, false
	}
	return embed, true
}

// ownedRefs gives each blob reference its owner.
func ownedRefs(ownerDID string, references []embedblobs.Reference) []ImageRef {
	if len(references) == 0 {
		return nil
	}
	refs := make([]ImageRef, 0, len(references))
	for _, reference := range references {
		refs = append(refs, ImageRef{OwnerDID: ownerDID, CID: reference.CID})
	}
	return refs
}

// ImageBatches splits refs into request batches of at most
// MaxImagesPerRequest, in order. No refs make one empty batch: a subject
// without images is still classified, by its text alone.
func ImageBatches(refs []ImageRef) [][]ImageRef {
	if len(refs) == 0 {
		return [][]ImageRef{{}}
	}
	batches := make([][]ImageRef, 0, (len(refs)+MaxImagesPerRequest-1)/MaxImagesPerRequest)
	for start := 0; start < len(refs); start += MaxImagesPerRequest {
		batches = append(batches, refs[start:min(start+MaxImagesPerRequest, len(refs))])
	}
	return batches
}

// MaxProbabilities combines batch answers into the per-question maximum.
func MaxProbabilities(batches []map[string]float64) map[string]float64 {
	combined := make(map[string]float64)
	for _, batch := range batches {
		for question, probability := range batch {
			if current, ok := combined[question]; !ok || probability > current {
				combined[question] = probability
			}
		}
	}
	return combined
}

// ImageFetcher reads a subject's image as a viewer would see it.
type ImageFetcher interface {
	FetchImage(ctx context.Context, ref ImageRef) (Image, error)
}

// NewProxyImageFetcher returns an ImageFetcher that reads images through the
// image proxy at the content preview size, so blocked and unreferenced blobs
// are refused and an animated GIF yields its first frame. The owner's PDS is
// found with resolver only when the image is not cached.
func NewProxyImageFetcher(service imageproxy.Service, resolver identity.Resolver) ImageFetcher {
	return proxyImageFetcher{service: service, resolver: resolver}
}

type proxyImageFetcher struct {
	service  imageproxy.Service
	resolver identity.Resolver
}

func (f proxyImageFetcher) FetchImage(ctx context.Context, ref ImageRef) (Image, error) {
	data, err := f.service.GetImageResolvingPDS(ctx, previewPreset, ref.OwnerDID, ref.CID, func(ctx context.Context) (string, error) {
		resolveContext, cancel := context.WithTimeout(ctx, identity.ResolveTimeout)
		defer cancel()
		owner, err := f.resolver.Resolve(resolveContext, ref.OwnerDID)
		if err != nil {
			return "", fmt.Errorf("resolving image owner: %w", err)
		}
		if owner.PDSURL == "" {
			return "", errors.New("image owner has no PDS endpoint")
		}
		return owner.PDSURL, nil
	})
	if err != nil {
		return Image{}, fmt.Errorf("fetching image through the image proxy: %w", err)
	}
	return Image{ContentType: http.DetectContentType(data), Data: data}, nil
}

// BatchProgress is one completed image batch of a job.
type BatchProgress struct {
	Index         int
	ModelID       string
	Probabilities map[string]float64
	ImagesSent    int
	ImagesSkipped int
	InputTokens   int
}

// WorkerOption configures a Worker.
type WorkerOption func(*Worker)

// WithImageFetcher gives the worker the fetcher it reads subject images with.
// A worker without one sends no images and counts every image skipped.
func WithImageFetcher(fetcher ImageFetcher) WorkerOption {
	return func(w *Worker) { w.imageFetcher = fetcher }
}
