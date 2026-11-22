//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // registers the JPEG decoder for sent previews
	"image/png"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/atproto/identity"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"
	"Coves/internal/core/imageproxy"

	cidlib "github.com/ipfs/go-cid"
	multihash "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The image half of the classification worker suite
// (docs/PRD_CONTENT_CLASSIFICATION.md R2-R5): subject images read through the
// real image proxy, with Postgres block and reference checks, from an
// in-memory PDS, sent to the recording Workers AI transport in batches.

// classificationImagePDSURL is the PDS every owner DID resolves to.
const classificationImagePDSURL = "https://pds.classification-images.test"

// The request URLs of the two models.
const (
	classificationTextModelURL  = "https://api.cloudflare.com/client/v4/accounts/acct-classify/ai/run/@cf/cloudflare/clef-flash"
	classificationImageModelURL = "https://api.cloudflare.com/client/v4/accounts/acct-classify/ai/run/@cf/cloudflare/clef"
)

// Literal per-batch answers and the per-question maxima across them.
var (
	classificationBatchOne = map[string]float64{
		"sexual_explicit": 0.61, "sexual_suggestive": 0.12, "sexual_minors": 0.03,
		"graphic_violence": 0.04, "hate_harassment": 0.25, "spam_scam": 0.06,
	}
	classificationBatchTwo = map[string]float64{
		"sexual_explicit": 0.11, "sexual_suggestive": 0.72, "sexual_minors": 0.02,
		"graphic_violence": 0.44, "hate_harassment": 0.05, "spam_scam": 0.16,
	}
	classificationBatchThree = map[string]float64{
		"sexual_explicit": 0.21, "sexual_suggestive": 0.32, "sexual_minors": 0.09,
		"graphic_violence": 0.14, "hate_harassment": 0.55, "spam_scam": 0.86,
	}
)

// classificationImagePDS is an in-memory PDS serving blobs by raw CID. With
// failing set, every fetch fails.
type classificationImagePDS struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	failing bool
}

func (p *classificationImagePDS) Fetch(_ context.Context, pdsURL, _, cid string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failing {
		return nil, fmt.Errorf("%w: in-memory PDS is failing", imageproxy.ErrPDSFetchFailed)
	}
	data, ok := p.blobs[cid]
	if !ok || pdsURL != classificationImagePDSURL {
		return nil, imageproxy.ErrPDSNotFound
	}
	return data, nil
}

// addImage stores a PNG width pixels wide and returns its raw CID. Widths
// identify which image a request carried; distinct widths also give every
// image distinct bytes.
func (p *classificationImagePDS) addImage(t *testing.T, width int) string {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, 8))
	shade := color.RGBA{R: uint8(width), G: 0x80, B: 0x40, A: 0xff}
	for x := range width {
		for y := range 8 {
			picture.SetRGBA(x, y, shade)
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, picture))
	hash, err := multihash.Sum(encoded.Bytes(), multihash.SHA2_256, -1)
	require.NoError(t, err)
	cid := cidlib.NewCidV1(cidlib.Raw, hash).String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blobs == nil {
		p.blobs = map[string][]byte{}
	}
	p.blobs[cid] = encoded.Bytes()
	return cid
}

// classificationImageResolver puts every DID on classificationImagePDSURL.
type classificationImageResolver struct{}

func (classificationImageResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	return &identity.Identity{DID: identifier, PDSURL: classificationImagePDSURL}, nil
}

func (classificationImageResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{DID: did, Service: []identity.Service{{
		ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: classificationImagePDSURL,
	}}}, nil
}

func (classificationImageResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", errors.New("classificationImageResolver: handles are not part of the image path")
}

func (classificationImageResolver) Purge(context.Context, string) error { return nil }

// imageWorker builds a worker like worker that reads subject images through
// a real image proxy over pds, with the Postgres moderation repository as
// its block checker and the blob reference repository as its reference
// checker.
func (f *classificationFixture) imageWorker(t *testing.T, pds *classificationImagePDS, adjust func(*classification.WorkerConfig), opts ...classification.WorkerOption) *classification.Worker {
	t.Helper()
	cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	service, err := imageproxy.NewService(cache, processor, pds,
		NewModerationRepository(f.db), NewBlobReferenceRepository(f.db), imageproxy.DefaultConfig())
	require.NoError(t, err)
	fetcher := classification.NewProxyImageFetcher(service, classificationImageResolver{})
	return f.workerOn(t, f.transport, adjust, append([]classification.WorkerOption{classification.WithImageFetcher(fetcher)}, opts...)...)
}

// referenceBlobs writes the subject's active blob references as the post
// indexer does.
func (f *classificationFixture) referenceBlobs(t *testing.T, ownerDID, subjectURI string, references []embedblobs.Reference) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	if err := NewBlobReferenceRepository(f.db).ReplaceSubjectTx(ctx, tx, ownerDID, subjectURI, references); err != nil {
		_ = tx.Rollback()
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// blockMedia blocks the owner's blob as an instance removal of another post
// carrying it does.
func (f *classificationFixture) blockMedia(t *testing.T, ownerDID, cid string) {
	t.Helper()
	actionID := seedPostInstanceRemoval(t, f.db, classificationInstanceDID,
		postV2URI(ownerDID, "3lclsremovedimg"), "social.coves.moderation.defs#reasonSexualContent")
	_, err := f.db.ExecContext(t.Context(),
		`INSERT INTO moderation_media_blocks (owner_did, blob_cid, action_id, active) VALUES ($1, $2, $3, TRUE)`,
		ownerDID, cid, actionID)
	require.NoError(t, err)
}

// classificationBlobJSON is an atproto blob reference to cid.
func classificationBlobJSON(cid string) string {
	return fmt.Sprintf(`{"$type":"blob","ref":{"$link":%q},"mimeType":"image/png","size":1024}`, cid)
}

// classificationGalleryJSON is a social.coves.embed.images embed of cids.
func classificationGalleryJSON(cids ...string) string {
	entries := make([]string, 0, len(cids))
	for _, cid := range cids {
		entries = append(entries, fmt.Sprintf(`{"image":%s,"alt":"a photo"}`, classificationBlobJSON(cid)))
	}
	return `{"$type":"social.coves.embed.images","images":[` + strings.Join(entries, ",") + `]}`
}

// classificationGalleryWithLinkJSON is an imagesWithAttachment embed: a
// gallery of galleryCIDs and an external link with a blob thumbnail.
func classificationGalleryWithLinkJSON(galleryCIDs []string, linkURI, linkTitle, thumbCID string) string {
	return fmt.Sprintf(`{"$type":"social.coves.embed.imagesWithAttachment","images":%s,`+
		`"attachment":{"$type":"social.coves.embed.external","external":{"uri":%q,"title":%q,"thumb":%s}}}`,
		classificationGalleryJSON(galleryCIDs...), linkURI, linkTitle, classificationBlobJSON(thumbCID))
}

// postImageReferences is a post_image reference per cid.
func postImageReferences(cids ...string) []embedblobs.Reference {
	references := make([]embedblobs.Reference, 0, len(cids))
	for _, cid := range cids {
		references = append(references, embedblobs.Reference{CID: cid, Kind: embedblobs.KindPostImage})
	}
	return references
}

// classificationSentRequest is what one recorded request sent.
type classificationSentRequest struct {
	URL   string
	State string
	// HasImages reports that the body carries an images key at all.
	HasImages bool
	// Widths are the decoded widths of the sent images, in order.
	Widths []int
}

// sent decodes every recorded request, in order.
func (f *classificationFixture) sent(t *testing.T) []classificationSentRequest {
	t.Helper()
	requests := f.transport.Requests()
	sent := make([]classificationSentRequest, 0, len(requests))
	for _, request := range requests {
		var body struct {
			State  string          `json:"state"`
			Images json.RawMessage `json:"images"`
		}
		require.NoError(t, json.Unmarshal(request.Body, &body))
		decoded := classificationSentRequest{URL: request.URL, State: body.State, HasImages: body.Images != nil}
		if body.Images != nil {
			var dataURLs []string
			require.NoError(t, json.Unmarshal(body.Images, &dataURLs), "images are data URL strings")
			for _, dataURL := range dataURLs {
				header, payload, found := strings.Cut(dataURL, ",")
				require.True(t, found, "an image is not a data URL")
				require.True(t, strings.HasPrefix(header, "data:image/") && strings.HasSuffix(header, ";base64"),
					"an image data URL header is %q", header)
				data, err := base64.StdEncoding.DecodeString(payload)
				require.NoError(t, err)
				config, _, err := image.DecodeConfig(bytes.NewReader(data))
				require.NoError(t, err, "a sent image does not decode")
				decoded.Widths = append(decoded.Widths, config.Width)
			}
		}
		sent = append(sent, decoded)
	}
	return sent
}

// classificationResultRow is a stored result's model and image fields.
type classificationResultRow struct {
	ModelID       string
	Probabilities map[string]float64
	ImagesSent    int
	ImagesSkipped int
}

// result reads the single result of subjectURI.
func (f *classificationFixture) result(t *testing.T, subjectURI string) classificationResultRow {
	t.Helper()
	var (
		row               classificationResultRow
		probabilitiesJSON []byte
	)
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT model_id, probabilities, images_sent, images_skipped
		FROM classification_results WHERE subject_uri = $1
	`, subjectURI).Scan(&row.ModelID, &probabilitiesJSON, &row.ImagesSent, &row.ImagesSkipped))
	require.NoError(t, json.Unmarshal(probabilitiesJSON, &row.Probabilities))
	return row
}

// usage reads a model's usage row for one UTC date; a missing row reads as
// zero.
func (f *classificationFixture) usage(t *testing.T, modelID, usageDate string) (requests, inputTokens int) {
	t.Helper()
	err := f.db.QueryRowContext(t.Context(), `
		SELECT requests, input_tokens FROM classification_usage WHERE model_id = $1 AND usage_date = $2
	`, modelID, usageDate).Scan(&requests, &inputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0
	}
	require.NoError(t, err)
	return requests, inputTokens
}

// classificationBatchRow is one persisted batch of a job.
type classificationBatchRow struct {
	Index         int
	ModelID       string
	Probabilities map[string]float64
	ImagesSent    int
	ImagesSkipped int
}

// batches reads the persisted batches of subjectURI's job, by index.
func (f *classificationFixture) batches(t *testing.T, subjectURI string) []classificationBatchRow {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), `
		SELECT b.batch_index, b.model_id, b.probabilities, b.images_sent, b.images_skipped
		FROM classification_job_batches b JOIN classification_jobs j ON j.id = b.job_id
		WHERE j.subject_uri = $1 ORDER BY b.batch_index
	`, subjectURI)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var batches []classificationBatchRow
	for rows.Next() {
		var (
			row               classificationBatchRow
			probabilitiesJSON []byte
		)
		require.NoError(t, rows.Scan(&row.Index, &row.ModelID, &probabilitiesJSON, &row.ImagesSent, &row.ImagesSkipped))
		require.NoError(t, json.Unmarshal(probabilitiesJSON, &row.Probabilities))
		batches = append(batches, row)
	}
	require.NoError(t, rows.Err())
	return batches
}

// answersInTurn answers the nth request with answers[n] and inputTokens[n];
// a nil entry answers with a provider 500.
func answersInTurn(answers []map[string]float64, inputTokens []int) cloudflaretest.Responder {
	var (
		mu       sync.Mutex
		answered int
	)
	return func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		turn := answered
		answered++
		mu.Unlock()
		if turn >= len(answers) {
			return nil, fmt.Errorf("answersInTurn: unexpected request %d", turn+1)
		}
		if answers[turn] == nil {
			return cloudflaretest.Response(req, http.StatusInternalServerError, classificationProviderFailure), nil
		}
		return cloudflaretest.Response(req, http.StatusOK, cloudflaretest.Envelope(answers[turn], inputTokens[turn])), nil
	}
}

// TestClassificationWorkerSkipsBlockedAndUnreferencedImages: images the
// image proxy will not serve to a viewer, or cannot fetch, are counted
// skipped and never sent; a request with no image left goes to the text
// model (R4, R5).
func TestClassificationWorkerSkipsBlockedAndUnreferencedImages(t *testing.T) {
	const title = "Kelp forest at dawn"
	for _, row := range []struct {
		name string
		// seed stores the post's images and returns its embed.
		seed            func(t *testing.T, f *classificationFixture, pds *classificationImagePDS, authorDID, postURI string) string
		wantURL         string
		wantHasImages   bool
		wantWidths      []int
		wantModelID     string
		wantSent        int
		wantSkipped     int
		wantTextUsage   int
		wantImagesUsage int
	}{
		{
			name: "blocked and unreferenced images are skipped",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS, authorDID, postURI string) string {
				servableOne, servableTwo := pds.addImage(t, 101), pds.addImage(t, 102)
				blocked, unreferenced := pds.addImage(t, 103), pds.addImage(t, 104)
				f.referenceBlobs(t, authorDID, postURI, postImageReferences(servableOne, servableTwo, blocked))
				f.blockMedia(t, authorDID, blocked)
				return classificationGalleryJSON(servableOne, servableTwo, blocked, unreferenced)
			},
			wantURL:         classificationImageModelURL,
			wantHasImages:   true,
			wantWidths:      []int{101, 102},
			wantModelID:     "@cf/cloudflare/clef",
			wantSent:        2,
			wantSkipped:     2,
			wantImagesUsage: 1,
		},
		{
			name: "no fetchable image sends text only to the text model",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS, authorDID, postURI string) string {
				first, second := pds.addImage(t, 111), pds.addImage(t, 112)
				f.referenceBlobs(t, authorDID, postURI, postImageReferences(first, second))
				pds.failing = true
				return classificationGalleryJSON(first, second)
			},
			wantURL:       classificationTextModelURL,
			wantModelID:   "@cf/cloudflare/clef-flash",
			wantSkipped:   2,
			wantTextUsage: 1,
		},
		{
			name: "a video embed is skipped and nothing of it is sent",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS, authorDID, postURI string) string {
				thumbnail := pds.addImage(t, 121)
				f.referenceBlobs(t, authorDID, postURI, []embedblobs.Reference{{CID: thumbnail, Kind: embedblobs.KindVideoThumb}})
				return fmt.Sprintf(`{"$type":"social.coves.embed.video","video":{"$type":"blob","ref":{"$link":"bafkreiclsimgskipvideo"},"mimeType":"video/mp4","size":1},"thumbnail":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1}}`, thumbnail)
			},
			wantURL:       classificationTextModelURL,
			wantModelID:   "@cf/cloudflare/clef-flash",
			wantSkipped:   1,
			wantTextUsage: 1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			pds := &classificationImagePDS{}
			worker := f.imageWorker(t, pds, nil)
			authorDID := f.author(t, "clsimgskip")
			postURI := postV2URI(authorDID, "3lclsimgskip01")
			embed := row.seed(t, f, pds, authorDID, postURI)
			require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, "3lclsimgskip01", title, "", embed))
			f.enqueue(t, postURI, classificationT0)

			f.run(t, worker, classificationT0)

			assert.Equal(t, []classificationSentRequest{{
				URL: row.wantURL, State: "Kelp forest at dawn", HasImages: row.wantHasImages, Widths: row.wantWidths,
			}}, f.sent(t))
			assert.Equal(t, "classified", f.job(t, postURI).Status)
			assert.Equal(t, classificationResultRow{
				ModelID: row.wantModelID,
				Probabilities: map[string]float64{
					"sexual_explicit": 0.01, "sexual_suggestive": 0.02, "sexual_minors": 0.03,
					"graphic_violence": 0.04, "hate_harassment": 0.05, "spam_scam": 0.06,
				},
				ImagesSent:    row.wantSent,
				ImagesSkipped: row.wantSkipped,
			}, f.result(t, postURI))
			textRequests, _ := f.usage(t, "@cf/cloudflare/clef-flash", "2026-11-12")
			imageRequests, _ := f.usage(t, "@cf/cloudflare/clef", "2026-11-12")
			assert.Equal(t, row.wantTextUsage, textRequests, "text-model requests")
			assert.Equal(t, row.wantImagesUsage, imageRequests, "image-model requests")
		})
	}
}

// seedNineImagePost seeds a visible post with an eight-image gallery and an
// external link whose blob thumbnail is the ninth image, widths firstWidth
// onward in order, every image referenced except the gallery images at the
// unreferenced positions. It returns the post URI.
func (f *classificationFixture) seedNineImagePost(t *testing.T, pds *classificationImagePDS, label string, firstWidth int, unreferenced ...int) string {
	t.Helper()
	authorDID := f.author(t, label)
	rkey := "3l" + label
	postURI := postV2URI(authorDID, rkey)
	cids := make([]string, 0, 9)
	for offset := range 9 {
		cids = append(cids, pds.addImage(t, firstWidth+offset))
	}
	var references []embedblobs.Reference
	for position, cid := range cids[:8] {
		skip := false
		for _, unreferencedPosition := range unreferenced {
			skip = skip || unreferencedPosition == position
		}
		if !skip {
			references = append(references, embedblobs.Reference{CID: cid, Kind: embedblobs.KindPostImage})
		}
	}
	references = append(references, embedblobs.Reference{CID: cids[8], Kind: embedblobs.KindExternalThumb})
	f.referenceBlobs(t, authorDID, postURI, references)
	embed := classificationGalleryWithLinkJSON(cids[:8], "https://example.com/anemone-census", "Anemone census results", cids[8])
	require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, rkey,
		"Tidepool survey of nine coves", "Counting anemones at low tide", embed))
	return postURI
}

// nineImagePostText is the classified text of seedNineImagePost's post:
// title, body, link title and link URI.
const nineImagePostText = "Tidepool survey of nine coves\n\nCounting anemones at low tide\n\nAnemone census results\n\nhttps://example.com/anemone-census"

// TestClassificationWorkerTakesTheMaximumAcrossImageBatches: nine images go
// out as batches of four, four and one, each with the whole text including
// the attachment's link, and the result is the highest answer per question
// (R4).
func TestClassificationWorkerTakesTheMaximumAcrossImageBatches(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{classificationBatchOne, classificationBatchTwo, classificationBatchThree},
		[]int{101, 202, 303}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, nil)
	postURI := f.seedNineImagePost(t, pds, "clsimgmax", 201)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, []classificationSentRequest{
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{201, 202, 203, 204}},
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{205, 206, 207, 208}},
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{209}},
	}, f.sent(t))
	assert.Equal(t, "classified", f.job(t, postURI).Status)
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.72, "sexual_minors": 0.09,
			"graphic_violence": 0.44, "hate_harassment": 0.55, "spam_scam": 0.86,
		},
		ImagesSent:    9,
		ImagesSkipped: 0,
	}, f.result(t, postURI))
	requests, inputTokens := f.usage(t, "@cf/cloudflare/clef", "2026-11-12")
	assert.Equal(t, 3, requests)
	assert.Equal(t, 606, inputTokens)
}

// TestClassificationWorkerContinuesImageBatchesAfterTheDailyCap: batches
// answered before the daily cap are kept, and the next UTC day's run sends
// only the remaining batch, each request counted on the day it was sent
// (R4, R5).
func TestClassificationWorkerContinuesImageBatchesAfterTheDailyCap(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{classificationBatchOne, classificationBatchTwo, classificationBatchThree},
		[]int{101, 202, 303}))
	pds := &classificationImagePDS{}
	capTwo := func(cfg *classification.WorkerConfig) { cfg.DailyRequestCap = 2 }
	// The second gallery image has no blob reference, so the first batch
	// sends three images and skips one.
	postURI := f.seedNineImagePost(t, pds, "clsimgcap", 301, 1)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, f.imageWorker(t, pds, capTwo), classificationT0)

	assert.Equal(t, []classificationSentRequest{
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{301, 303, 304}},
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{305, 306, 307, 308}},
	}, f.sent(t), "day 1 sends the first two batches")
	held := f.job(t, postURI)
	assert.Equal(t, "queued", held.Status, "the job waits for the next day's budget")
	assert.Equal(t, 0, held.AttemptCount)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, postURI))
	assert.Equal(t, []classificationBatchRow{
		{Index: 0, ModelID: "@cf/cloudflare/clef", Probabilities: classificationBatchOne, ImagesSent: 3, ImagesSkipped: 1},
		{Index: 1, ModelID: "@cf/cloudflare/clef", Probabilities: classificationBatchTwo, ImagesSent: 4, ImagesSkipped: 0},
	}, f.batches(t, postURI))
	dayOneRequests, dayOneTokens := f.usage(t, "@cf/cloudflare/clef", "2026-11-12")
	assert.Equal(t, 2, dayOneRequests)
	assert.Equal(t, 303, dayOneTokens)

	f.run(t, f.imageWorker(t, pds, capTwo), time.Date(2026, 11, 13, 0, 5, 0, 0, time.UTC))

	sent := f.sent(t)
	require.Len(t, sent, 3, "day 2 sends exactly the remaining batch")
	assert.Equal(t, classificationSentRequest{
		URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{309},
	}, sent[2], "the day-2 request carries only the final image")
	assert.Equal(t, "classified", f.job(t, postURI).Status)
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.72, "sexual_minors": 0.09,
			"graphic_violence": 0.44, "hate_harassment": 0.55, "spam_scam": 0.86,
		},
		ImagesSent:    8,
		ImagesSkipped: 1,
	}, f.result(t, postURI))
	dayOneRequests, dayOneTokens = f.usage(t, "@cf/cloudflare/clef", "2026-11-12")
	dayTwoRequests, dayTwoTokens := f.usage(t, "@cf/cloudflare/clef", "2026-11-13")
	assert.Equal(t, 2, dayOneRequests)
	assert.Equal(t, 303, dayOneTokens)
	assert.Equal(t, 1, dayTwoRequests)
	assert.Equal(t, 303, dayTwoTokens)
}

// TestClassificationWorkerRetriesOnlyTheUnfinishedImageBatch: when a later
// batch's request fails, the retry sends only that batch and the answered
// batch is not sent or counted again (R4, R5).
func TestClassificationWorkerRetriesOnlyTheUnfinishedImageBatch(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{classificationBatchOne, nil, classificationBatchTwo},
		[]int{101, 0, 202}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, nil)
	authorDID := f.author(t, "clsimgretry")
	postURI := postV2URI(authorDID, "3lclsimgretry1")
	cids := []string{pds.addImage(t, 401), pds.addImage(t, 402), pds.addImage(t, 403), pds.addImage(t, 404), pds.addImage(t, 405)}
	f.referenceBlobs(t, authorDID, postURI, postImageReferences(cids...))
	require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, "3lclsimgretry1",
		"Sea star recovery", "", classificationGalleryJSON(cids...)))
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, []classificationSentRequest{
		{URL: classificationImageModelURL, State: "Sea star recovery", HasImages: true, Widths: []int{401, 402, 403, 404}},
		{URL: classificationImageModelURL, State: "Sea star recovery", HasImages: true, Widths: []int{405}},
	}, f.sent(t))
	failed := f.job(t, postURI)
	assert.Equal(t, "retrying", failed.Status)
	assert.Equal(t, 1, failed.AttemptCount)
	assert.Equal(t, []classificationBatchRow{
		{Index: 0, ModelID: "@cf/cloudflare/clef", Probabilities: classificationBatchOne, ImagesSent: 4, ImagesSkipped: 0},
	}, f.batches(t, postURI), "the answered batch is kept")

	f.run(t, worker, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC))

	sent := f.sent(t)
	require.Len(t, sent, 3, "the retry sends one request")
	assert.Equal(t, classificationSentRequest{
		URL: classificationImageModelURL, State: "Sea star recovery", HasImages: true, Widths: []int{405},
	}, sent[2], "the retry carries only the unfinished batch")
	assert.Equal(t, "classified", f.job(t, postURI).Status)
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.72, "sexual_minors": 0.03,
			"graphic_violence": 0.44, "hate_harassment": 0.25, "spam_scam": 0.16,
		},
		ImagesSent:    5,
		ImagesSkipped: 0,
	}, f.result(t, postURI))
	requests, inputTokens := f.usage(t, "@cf/cloudflare/clef", "2026-11-12")
	assert.Equal(t, 3, requests, "the failed request counts")
	assert.Equal(t, 303, inputTokens)
}

// partialResultRow is a stored result's partial flag and identity.
type partialResultRow struct {
	ID      int64
	Partial bool
}

// partialResult reads the single result of subjectURI with its partial flag.
func (f *classificationFixture) partialResult(t *testing.T, subjectURI string) partialResultRow {
	t.Helper()
	var row partialResultRow
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT id, partial FROM classification_results WHERE subject_uri = $1`, subjectURI).Scan(&row.ID, &row.Partial))
	return row
}

// seedEightImagePost seeds a visible post with an eight-image gallery, every
// image referenced, widths firstWidth onward. It returns the post URI.
func (f *classificationFixture) seedEightImagePost(t *testing.T, pds *classificationImagePDS, label string, firstWidth int) string {
	t.Helper()
	authorDID := f.author(t, label)
	rkey := "3l" + label
	postURI := postV2URI(authorDID, rkey)
	cids := make([]string, 0, 8)
	for offset := range 8 {
		cids = append(cids, pds.addImage(t, firstWidth+offset))
	}
	f.referenceBlobs(t, authorDID, postURI, postImageReferences(cids...))
	require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, rkey,
		"Rockpool census", "", classificationGalleryJSON(cids...)))
	return postURI
}

// partialMinorsAnswer is a first-batch answer whose minors probability meets
// the minors score alert.
var partialMinorsAnswer = scoreAlertProbabilities(0.9, 0.05)

// TestClassificationWorkerFinalFailureStoresAPartialResult: an eight-image
// post whose first batch is answered and whose second batch fails to the
// attempt limit ends failed with its attempt history, and its answered batch
// becomes a partial result in the same statement: the per-question maximum of
// the answered batch, the four images never sent counted skipped, and the
// score alert its minors answer meets (R2, R4, R5).
func TestClassificationWorkerFinalFailureStoresAPartialResult(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{partialMinorsAnswer, nil, nil},
		[]int{101, 0, 0}))
	pds := &classificationImagePDS{}
	service := classification.NewScoreAlertService(NewClassificationScoreAlertRepository(f.db), nil,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute}, f.clock.Now, signalLogger())
	worker := f.imageWorker(t, pds, func(cfg *classification.WorkerConfig) { cfg.AttemptLimit = 2 },
		classification.WithScoreAlerts(scoreAlertMinorsAndSpam, service))
	postURI := f.seedEightImagePost(t, pds, "clsimgpartial", 421)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)
	assert.Equal(t, "retrying", f.job(t, postURI).Status)
	f.run(t, worker, classificationT0.Add(time.Minute))

	require.Len(t, f.sent(t), 3, "batch 0 answered once, batch 1 failed twice")
	job := f.job(t, postURI)
	assert.Equal(t, "failed", job.Status, "the job still ends failed")
	assert.Equal(t, 2, job.AttemptCount, "the attempt history is kept")
	assert.Equal(t, "provider_error", job.LastErrorCode.String)
	assert.Empty(t, f.batches(t, postURI), "the batches are consumed by the partial result")
	assert.Equal(t, classificationResultRow{
		ModelID:       "@cf/cloudflare/clef",
		Probabilities: partialMinorsAnswer,
		ImagesSent:    4,
		ImagesSkipped: 4,
	}, f.result(t, postURI))
	result := f.partialResult(t, postURI)
	assert.True(t, result.Partial, "the result is marked partial")
	alerts := f.scoreAlerts(t)
	require.Len(t, alerts, 1, "the minors alert is raised")
	assert.Equal(t, result.ID, alerts[0].ResultID)
	assert.Equal(t, "sexual_minors", alerts[0].QuestionKey)
	assert.Equal(t, 1, f.logs.countCode("partial_result_stored"), "the partial result is logged once")
	assert.Zero(t, f.logs.countCode("batches_discarded"), "nothing is discarded")
}

// partialSpamAnswer is a second-batch answer whose spam probability meets
// the spam score alert and whose other maxima differ from
// classificationBatchOne's.
var partialSpamAnswer = map[string]float64{
	"sexual_explicit": 0.22, "sexual_suggestive": 0.47, "sexual_minors": 0.08,
	"graphic_violence": 0.39, "hate_harassment": 0.14, "spam_scam": 0.81,
}

// TestClassificationWorkerFinalFailureAggregatesEveryAnsweredBatch: a
// nine-image post whose first two batches are answered and whose final
// one-image batch fails at the attempt limit stores a partial result built
// from both answered batches: the per-question maximum across them, the
// eight images they sent, the one image never sent counted skipped, and the
// spam alert only the second batch's answer meets (R2, R4, R5).
func TestClassificationWorkerFinalFailureAggregatesEveryAnsweredBatch(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{classificationBatchOne, partialSpamAnswer, nil},
		[]int{101, 202, 0}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, func(cfg *classification.WorkerConfig) { cfg.AttemptLimit = 1 },
		classification.WithScoreAlerts(scoreAlertMinorsAndSpam, nil))
	postURI := f.seedNineImagePost(t, pds, "clsimgpartialtwo", 451)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	require.Len(t, f.sent(t), 3, "two batches answered, the third failed once")
	job := f.job(t, postURI)
	assert.Equal(t, "failed", job.Status)
	assert.Equal(t, 1, job.AttemptCount)
	assert.Empty(t, f.batches(t, postURI), "the batches are consumed by the partial result")
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.47, "sexual_minors": 0.08,
			"graphic_violence": 0.39, "hate_harassment": 0.25, "spam_scam": 0.81,
		},
		ImagesSent:    8,
		ImagesSkipped: 1,
	}, f.result(t, postURI))
	result := f.partialResult(t, postURI)
	assert.True(t, result.Partial, "the result is marked partial")
	alerts := f.scoreAlerts(t)
	require.Len(t, alerts, 1, "the second batch's spam alert is raised")
	assert.Equal(t, result.ID, alerts[0].ResultID)
	assert.Equal(t, "spam_scam", alerts[0].QuestionKey)
	assert.Equal(t, 1, f.logs.countCode("partial_result_stored"), "the partial result is logged once")
}

// TestClassificationWorkerSingleBatchFinalFailureStoresNoResult: a job with
// no answered batch that fails at the attempt limit ends failed with no
// result and no score alert (R2).
func TestClassificationWorkerSingleBatchFinalFailureStoresNoResult(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn([]map[string]float64{nil}, []int{0}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, func(cfg *classification.WorkerConfig) { cfg.AttemptLimit = 1 },
		classification.WithScoreAlerts(scoreAlertMinorsAndSpam, nil))
	authorDID := f.author(t, "clsimgsingle")
	postURI := postV2URI(authorDID, "3lclsimgsingle1")
	cids := []string{pds.addImage(t, 431), pds.addImage(t, 432), pds.addImage(t, 433)}
	f.referenceBlobs(t, authorDID, postURI, postImageReferences(cids...))
	require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, "3lclsimgsingle1",
		"Single batch survey", "", classificationGalleryJSON(cids...)))
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	require.Len(t, f.sent(t), 1)
	job := f.job(t, postURI)
	assert.Equal(t, "failed", job.Status)
	assert.Equal(t, 1, job.AttemptCount)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM classification_results WHERE subject_uri = $1`, postURI),
		"no result is stored")
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM classification_score_alerts`), "no score alert is stored")
	assert.Zero(t, f.logs.countCode("partial_result_stored"))
}

// TestClassificationWorkerRevivedPartialJobReplacesItsResult: re-enqueue
// revives a failed job that holds a partial result; when the revived job
// completes, its result replaces the partial one in place (same id, partial
// false, verdicts kept) and an alert already raised is not raised twice (R5,
// R6).
func TestClassificationWorkerRevivedPartialJobReplacesItsResult(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{partialMinorsAnswer, nil, classificationBatchOne, scoreAlertProbabilities(0.4, 0.7)},
		[]int{101, 0, 101, 202}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, func(cfg *classification.WorkerConfig) { cfg.AttemptLimit = 1 },
		classification.WithScoreAlerts(scoreAlertMinorsAndSpam, nil))
	postURI := f.seedEightImagePost(t, pds, "clsimgrevive", 441)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)
	partial := f.partialResult(t, postURI)
	require.True(t, partial.Partial)
	_, err := f.db.ExecContext(t.Context(), `
		INSERT INTO classification_verdicts (result_id, admin_did, present, recorded_at)
		VALUES ($1, 'did:plc:clsreviveadmin', '{"sexual_minors": false}', NOW())`, partial.ID)
	require.NoError(t, err)

	report, err := NewClassificationReenqueueRepository(f.db, 0).EnqueuePostsAndComments(t.Context(),
		classification.Evaluation{PolicyVersion: "v1", TextModelID: "@cf/cloudflare/clef-flash", ImageModelID: "@cf/cloudflare/clef"},
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 1, report.Revived, "the failed job with a partial result is revived")

	// Revival makes the job due at the database's NOW().
	f.run(t, worker, time.Now().UTC().Add(time.Minute))

	require.Len(t, f.sent(t), 4, "the revived job sends both batches")
	assert.Equal(t, "classified", f.job(t, postURI).Status)
	complete := f.partialResult(t, postURI)
	assert.Equal(t, partialResultRow{ID: partial.ID, Partial: false}, complete, "the result is replaced in place")
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.12, "sexual_minors": 0.4,
			"graphic_violence": 0.04, "hate_harassment": 0.25, "spam_scam": 0.7,
		},
		ImagesSent:    8,
		ImagesSkipped: 0,
	}, f.result(t, postURI))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM classification_verdicts WHERE result_id = $1`, partial.ID),
		"the verdict on the content is kept")
	var questions []string
	for _, alert := range f.scoreAlerts(t) {
		questions = append(questions, alert.QuestionKey)
	}
	assert.Equal(t, []string{"sexual_minors", "spam_scam"}, questions,
		"the minors alert is not raised twice and the new spam alert is raised")
}

// TestClassificationWorkerStoresASkippedOnlyImageBatch: a middle batch none
// of whose images can be fetched sends no request and is stored with its
// skipped count, no model and no answer, so a retry after a later batch fails
// continues at the failed batch; the skipped-only batch adds no usage (R4, R5).
func TestClassificationWorkerStoresASkippedOnlyImageBatch(t *testing.T) {
	f := newClassificationFixture(t, answersInTurn(
		[]map[string]float64{classificationBatchOne, nil, classificationBatchThree},
		[]int{101, 0, 303}))
	pds := &classificationImagePDS{}
	worker := f.imageWorker(t, pds, nil)
	// Gallery images five to eight have no blob reference, so the second
	// batch has no image the proxy will serve.
	postURI := f.seedNineImagePost(t, pds, "clsimgskiponly", 601, 4, 5, 6, 7)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, []classificationSentRequest{
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{601, 602, 603, 604}},
		{URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{609}},
	}, f.sent(t), "the skipped-only batch sends no request")
	failed := f.job(t, postURI)
	assert.Equal(t, "retrying", failed.Status)
	assert.Equal(t, 1, failed.AttemptCount)
	assert.Equal(t, []classificationBatchRow{
		{Index: 0, ModelID: "@cf/cloudflare/clef", Probabilities: classificationBatchOne, ImagesSent: 4, ImagesSkipped: 0},
		{Index: 1, ModelID: "", Probabilities: map[string]float64{}, ImagesSent: 0, ImagesSkipped: 4},
	}, f.batches(t, postURI), "the skipped-only batch is stored with the answered one")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_usage WHERE model_id = ''`),
		"the skipped-only batch adds no usage row")

	f.run(t, worker, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC))

	sent := f.sent(t)
	require.Len(t, sent, 3, "the retry sends one request")
	assert.Equal(t, classificationSentRequest{
		URL: classificationImageModelURL, State: nineImagePostText, HasImages: true, Widths: []int{609},
	}, sent[2], "the retry carries only the failed batch")
	assert.Equal(t, "classified", f.job(t, postURI).Status)
	assert.Equal(t, classificationResultRow{
		ModelID: "@cf/cloudflare/clef",
		Probabilities: map[string]float64{
			"sexual_explicit": 0.61, "sexual_suggestive": 0.32, "sexual_minors": 0.09,
			"graphic_violence": 0.14, "hate_harassment": 0.55, "spam_scam": 0.86,
		},
		ImagesSent:    5,
		ImagesSkipped: 4,
	}, f.result(t, postURI))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_usage WHERE model_id = ''`))
}

// TestClassificationWorkerRechecksBeforeEachImageBatch: a subject removed or
// superseded while an earlier batch is in flight is closed before the next
// batch is sent, with no result and no batch progress left (R4, V1).
func TestClassificationWorkerRechecksBeforeEachImageBatch(t *testing.T) {
	for _, row := range []struct {
		name string
		// change runs during the first batch's request.
		change          func(t *testing.T, f *classificationFixture, postURI string)
		wantCloseReason string
	}{
		{
			name: "post instance-removed",
			change: func(t *testing.T, f *classificationFixture, postURI string) {
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, postURI, "social.coves.moderation.defs#reasonSpam")
			},
			wantCloseReason: "not_visible",
		},
		{
			name: "community instance-removed",
			change: func(t *testing.T, f *classificationFixture, _ string) {
				seedCommunityRemoval(t, f.db, classificationInstanceDID, f.community)
			},
			wantCloseReason: "not_visible",
		},
		{
			name: "superseded by a new indexed CID",
			change: func(t *testing.T, f *classificationFixture, postURI string) {
				_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET cid = 'bafyclsimgeditedcid' WHERE uri = $1`, postURI)
				require.NoError(t, err)
			},
			wantCloseReason: "superseded",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			pds := &classificationImagePDS{}
			worker := f.imageWorker(t, pds, nil)
			authorDID := f.author(t, "clsimgrecheck")
			postURI := postV2URI(authorDID, "3lclsimgrchk1")
			cids := []string{pds.addImage(t, 501), pds.addImage(t, 502), pds.addImage(t, 503), pds.addImage(t, 504), pds.addImage(t, 505)}
			f.referenceBlobs(t, authorDID, postURI, postImageReferences(cids...))
			require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, "3lclsimgrchk1",
				"Limpet migration", "", classificationGalleryJSON(cids...)))
			f.enqueue(t, postURI, classificationT0)
			var once sync.Once
			f.transport.SetBeforeAnswer(func(*http.Request) {
				once.Do(func() { row.change(t, f, postURI) })
			})

			f.run(t, worker, classificationT0)

			assert.Equal(t, []classificationSentRequest{
				{URL: classificationImageModelURL, State: "Limpet migration", HasImages: true, Widths: []int{501, 502, 503, 504}},
			}, f.sent(t), "only the first batch is sent")
			closed := f.job(t, postURI)
			assert.Equal(t, "closed", closed.Status)
			assert.Equal(t, row.wantCloseReason, closed.CloseReason.String)
			assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, postURI),
				"a closed job stores no result")
			assert.Empty(t, f.batches(t, postURI), "closing the job discards its batch progress")
		})
	}
}

// legacyPostWithEmbed seeds a visible legacy community-repo post
// (social.coves.community.post) carrying embedJSON; its blobs live in the
// community's repository.
func (f *classificationFixture) legacyPostWithEmbed(t *testing.T, authorDID, rkey, title, embedJSON string) string {
	t.Helper()
	uri := seedLegacyPost(t, f.db, f.community, authorDID, rkey, title, classificationT0.Add(-time.Hour))
	_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET embed = $2 WHERE uri = $1`, uri, embedJSON)
	require.NoError(t, err)
	return uri
}

// TestClassificationWorkerSendsCommentAndCommunityImages: a comment's image
// embed, a community's avatar and banner, and a legacy post's gallery are
// read from their owners' repositories and sent with the subject's text
// (R2, R4, D16).
func TestClassificationWorkerSendsCommentAndCommunityImages(t *testing.T) {
	for _, row := range []struct {
		name string
		// seed seeds and enqueues the subject and returns its URI.
		seed       func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string
		wantState  string
		wantWidths []int
		wantSent   int
	}{
		{
			name: "comment image embed owned by the commenter",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string {
				root := f.visiblePost(t, f.community, f.author(t, "clsimgroot"), "3lclsimgroot1", "Visible root heron")
				commenterDID := f.author(t, "clsimgcommenter")
				cid := pds.addImage(t, 601)
				commentURI := seedClassificationCommentWithEmbed(t, f.db, commenterDID, "3lclsimgcmt01", root, root,
					"Barnacle close-up reply", classificationGalleryJSON(cid))
				f.referenceBlobs(t, commenterDID, commentURI,
					[]embedblobs.Reference{{CID: cid, Kind: embedblobs.KindCommentImage}})
				f.enqueueComment(t, commentURI, classificationT0)
				return commentURI
			},
			wantState:  "Barnacle close-up reply",
			wantWidths: []int{601},
			wantSent:   1,
		},
		{
			name: "community avatar and banner owned by the community",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string {
				const did = "did:plc:clsimgcommunity"
				seedClassificationCommunity(t, f.db, did, "clsimgcommunity.coves.social", f.author(t, "clsimgowner"),
					"Urchin reefs", "Urchin Reef Watch", "A community about sea urchins", "bafyclsimgcommunity")
				setClassificationCommunityImages(t, f.db, did, pds.addImage(t, 611), pds.addImage(t, 612))
				f.enqueueCommunity(t, did, "bafyclsimgcommunity", classificationT0)
				return did
			},
			wantState:  "Urchin reefs\n\nUrchin Reef Watch\n\nA community about sea urchins",
			wantWidths: []int{611, 612},
			wantSent:   2,
		},
		{
			name: "legacy post gallery owned by the community",
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string {
				cid := pds.addImage(t, 621)
				postURI := f.legacyPostWithEmbed(t, f.author(t, "clsimglegacy"), "3lclsimglgcy1",
					"Legacy kelp holdfast", classificationGalleryJSON(cid))
				f.referenceBlobs(t, f.community, postURI, postImageReferences(cid))
				f.enqueue(t, postURI, classificationT0)
				return postURI
			},
			wantState:  "Legacy kelp holdfast",
			wantWidths: []int{621},
			wantSent:   1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			pds := &classificationImagePDS{}
			worker := f.imageWorker(t, pds, nil)
			subjectURI := row.seed(t, f, pds)

			f.run(t, worker, classificationT0)

			assert.Equal(t, []classificationSentRequest{{
				URL: classificationImageModelURL, State: row.wantState, HasImages: true, Widths: row.wantWidths,
			}}, f.sent(t))
			assert.Equal(t, "classified", f.job(t, subjectURI).Status)
			assert.Equal(t, classificationResultRow{
				ModelID: "@cf/cloudflare/clef",
				Probabilities: map[string]float64{
					"sexual_explicit": 0.01, "sexual_suggestive": 0.02, "sexual_minors": 0.03,
					"graphic_violence": 0.04, "hate_harassment": 0.05, "spam_scam": 0.06,
				},
				ImagesSent:    row.wantSent,
				ImagesSkipped: 0,
			}, f.result(t, subjectURI))
		})
	}
}
