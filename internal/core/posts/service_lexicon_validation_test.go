package posts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/unfurl"
	"Coves/internal/validation"
)

// The post service validates the FINAL postv2 record against
// social.coves.community.postv2 immediately before the author-repo write, on
// both create and update. An embed that breaks the lexicon is refused as
// ErrInvalidEmbed (a validation error on field "embed"); any other lexicon
// failure is a plain validation error; nothing reaches the author's repo and a
// create's submission reservation is given back. Valid records are written
// unchanged, and unfurl enhancement is fitted to the lexicon rather than
// allowed to fail a post.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	lexiconAuthorDID    = "did:plc:lexvalauthor2345abcdefgh"
	lexiconCommunityDID = "did:plc:lexvalcommunity2345abcde"
	lexiconEditRkey     = "3m2lexiconeditab"

	// lexiconBlobJPEG is a blob ref carrying a real CIDv1 (raw, sha2-256).
	lexiconBlobJPEG = `{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213}`
	lexiconBlobMP4  = `{"$type":"blob","ref":{"$link":"bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"},"mimeType":"video/mp4","size":2400000}`
)

// lexiconEditURI is the postv2 the update tests edit.
const lexiconEditURI = "at://" + lexiconAuthorDID + "/" + PostV2Collection + "/" + lexiconEditRkey

// pathLexiconValidator loads the repository's lexicon tree from disk.
func pathLexiconValidator(t *testing.T) *validation.LexiconValidator {
	t.Helper()
	v, err := validation.NewLexiconValidator("../../atproto/lexicon", false)
	require.NoError(t, err, "loading the lexicon schemas from internal/atproto/lexicon")
	return v
}

// jsonValue decodes a JSON literal into the generic shape a record takes after
// a PDS round trip (maps, []any, float64).
func jsonValue(t *testing.T, literal string) any {
	t.Helper()
	var v any
	require.NoErrorf(t, json.Unmarshal([]byte(literal), &v), "test literal is not JSON: %s", literal)
	return v
}

// jsonObject is jsonValue for an object literal.
func jsonObject(t *testing.T, literal string) map[string]interface{} {
	t.Helper()
	obj, ok := jsonValue(t, literal).(map[string]interface{})
	require.Truef(t, ok, "test literal is not a JSON object: %s", literal)
	return obj
}

// jsonArray is jsonValue for an array literal.
func jsonArray(t *testing.T, literal string) []interface{} {
	t.Helper()
	arr, ok := jsonValue(t, literal).([]interface{})
	require.Truef(t, ok, "test literal is not a JSON array: %s", literal)
	return arr
}

func lexiconStringPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// lexiconCommunities resolves exactly one public community. Every other
// communities.Service method is the embedded nil interface and panics, which is
// itself the assertion that the write paths reach for nothing else.
type lexiconCommunities struct{ communities.Service }

func (lexiconCommunities) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	if identifier != lexiconCommunityDID {
		return "", communities.ErrCommunityNotFound
	}
	return lexiconCommunityDID, nil
}

func (lexiconCommunities) GetByDID(_ context.Context, did string) (*communities.Community, error) {
	if did != lexiconCommunityDID {
		return nil, communities.ErrCommunityNotFound
	}
	return &communities.Community{DID: lexiconCommunityDID, Visibility: "public"}, nil
}

// recordStoringAuthorRepo is an author PDS that keeps what was written: each
// put is snapshotted through a JSON round trip (what a PDS would store), and
// the swap guard behaves like the PDS's — create-only when swapRecord is "",
// compare-and-swap on the standing CID otherwise.
type recordStoringAuthorRepo struct {
	*memAuthorRepo
}

func newRecordStoringAuthorRepo() *recordStoringAuthorRepo {
	return &recordStoringAuthorRepo{memAuthorRepo: newMemAuthorRepo(lexiconAuthorDID)}
}

func copyRecordResponse(rec *pds.RecordResponse) (*pds.RecordResponse, error) {
	encoded, err := json.Marshal(rec.Value)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return &pds.RecordResponse{URI: rec.URI, CID: rec.CID, Value: value}, nil
}

func (r *recordStoringAuthorRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	rec, ok := r.records[collection+"/"+rkey]
	if !ok {
		return nil, pds.ErrNotFound
	}
	return copyRecordResponse(rec)
}

func (r *recordStoringAuthorRepo) PutRecordWithCommit(_ context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	key := collection + "/" + rkey
	standing, exists := r.records[key]
	switch {
	case swapRecord == "" && exists:
		return nil, pds.ErrSwapConflict
	case swapRecord != "" && (!exists || standing.CID != swapRecord):
		return nil, pds.ErrSwapConflict
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("fake PDS: encoding record: %w", err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, fmt.Errorf("fake PDS: decoding record: %w", err)
	}

	r.puts++
	uri := "at://" + r.did + "/" + collection + "/" + rkey
	cid := fmt.Sprintf("bafyreistoredrecord%d", r.puts)
	r.records[key] = &pds.RecordResponse{URI: uri, CID: cid, Value: value}
	return &pds.RecordCommit{URI: uri, CID: cid}, nil
}

// seed places a standing postv2 at lexiconEditRkey, as an earlier create would
// have left it.
func (r *recordStoringAuthorRepo) seed(t *testing.T, value map[string]interface{}) {
	t.Helper()
	r.records[PostV2Collection+"/"+lexiconEditRkey] = &pds.RecordResponse{
		URI: lexiconEditURI, CID: "bafyreiseededrecord", Value: value,
	}
}

// snapshot is a deep copy of the standing record at rkey, or nil.
func (r *recordStoringAuthorRepo) snapshot(t *testing.T, rkey string) *pds.RecordResponse {
	t.Helper()
	rec, ok := r.records[PostV2Collection+"/"+rkey]
	if !ok {
		return nil
	}
	copied, err := copyRecordResponse(rec)
	require.NoError(t, err)
	return copied
}

// onlyRecord returns the single postv2 in the repo.
func (r *recordStoringAuthorRepo) onlyRecord(t *testing.T) *pds.RecordResponse {
	t.Helper()
	require.Len(t, r.records, 1, "exactly one record should have been written")
	for _, rec := range r.records {
		return rec
	}
	return nil
}

// fixedUnfurl answers every URL with one result and counts the fetches.
type fixedUnfurl struct {
	result unfurl.UnfurlResult
	calls  int
}

func (u *fixedUnfurl) IsSupported(string) bool { return true }

func (u *fixedUnfurl) UnfurlURL(context.Context, string) (*unfurl.UnfurlResult, error) {
	u.calls++
	result := u.result
	return &result, nil
}

// countingThumbnailFetcher is a blob service that counts thumbnail fetches and
// refuses each one, so a fetch is observable without any network.
type countingThumbnailFetcher struct{ fetches int }

func (f *countingThumbnailFetcher) UploadBlob(context.Context, blobs.BlobOwner, []byte, string) (*blobs.BlobRef, error) {
	return nil, errors.New("countingThumbnailFetcher: community-owned uploads are not on the post path")
}

func (f *countingThumbnailFetcher) FetchImageForURL(context.Context, string) ([]byte, error) {
	f.fetches++
	return nil, errors.New("countingThumbnailFetcher: thumbnail fetch refused")
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type lexiconHarness struct {
	service   Service
	ledger    *stubLedger
	repo      *recordStoringAuthorRepo
	session   *oauth.ClientSessionData
	ctx       context.Context
	validator *validation.LexiconValidator
}

func newLexiconHarness(t *testing.T, unfurlService unfurl.Service) *lexiconHarness {
	t.Helper()
	return newLexiconHarnessWithBlobs(t, unfurlService, nil)
}

// newLexiconHarnessWithBlobs wires blobService and a media pipeline, so an
// unfurled thumbnail is fetched through blobService.
func newLexiconHarnessWithBlobs(t *testing.T, unfurlService unfurl.Service, blobService blobs.Service) *lexiconHarness {
	t.Helper()
	return buildLexiconHarness(t, unfurlService, blobService, pathLexiconValidator(t))
}

// newUnvalidatedLexiconHarness is the harness with NO lexicon validator wired,
// for checks that must hold without the lexicon backstop.
func newUnvalidatedLexiconHarness(t *testing.T) *lexiconHarness {
	t.Helper()
	return buildLexiconHarness(t, nil, nil, nil)
}

func buildLexiconHarness(t *testing.T, unfurlService unfurl.Service, blobService blobs.Service, validator *validation.LexiconValidator) *lexiconHarness {
	t.Helper()

	now := func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	h := &lexiconHarness{
		ledger:    &stubLedger{now: now},
		repo:      newRecordStoringAuthorRepo(),
		session:   sessionForDIDString(t, lexiconAuthorDID),
		ctx:       middleware.SetTestUserDID(context.Background(), lexiconAuthorDID),
		validator: validator,
	}
	options := []PostServiceOption{
		WithAuthorRepoFactory(func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) {
			return h.repo, nil
		}),
		WithAdmissionPolicy(AdmissionPolicy{
			Ledger: h.ledger,
			Bans:   unenforcedBans{},
			Limits: SubmissionLimits{
				MaxPerAuthorPerCommunity: 100,
				Window:                   time.Hour,
				DedupeWindow:             time.Hour,
			},
			Now: now,
		}),
	}
	if validator != nil {
		options = append(options, WithLexiconValidator(validator))
	}
	if blobService != nil {
		options = append(options, WithMediaPipeline(newThumbnailPipeline(t)))
	}
	h.service = NewPostService(
		nil, lexiconCommunities{}, nil, blobService, unfurlService, nil, "https://pds.invalid", options...,
	)
	return h
}

func (h *lexiconHarness) create(req CreatePostRequest) (*CreatePostResponse, error) {
	req.Community = lexiconCommunityDID
	req.AuthorDID = lexiconAuthorDID
	return h.service.CreatePost(h.ctx, h.session, req)
}

func (h *lexiconHarness) update(req UpdatePostRequest) (*UpdatePostResponse, error) {
	req.URI = lexiconEditURI
	return h.service.UpdatePost(h.ctx, h.session, req)
}

// ---------------------------------------------------------------------------
// B2 — create refuses a record the lexicon rejects
// ---------------------------------------------------------------------------

func TestCreatePost_LexiconValidationRejectsInvalidRecords(t *testing.T) {
	tests := []struct {
		name string
		req  func(t *testing.T) CreatePostRequest
		// wantInvalidEmbed: the failure is in the embed, so it is
		// ErrInvalidEmbed on field "embed"; otherwise a plain validation error.
		wantInvalidEmbed bool
		// wantField, when set, is the field a plain validation error names.
		wantField string
	}{
		{
			name: "images entry without its image blob",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Photos"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"alt":"a harbour at dusk"}]}`),
				}
			},
			wantInvalidEmbed: true,
		},
		{
			name: "images alt of 1001 graphemes",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Photos"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"image":`+lexiconBlobJPEG+
						`,"alt":"`+strings.Repeat("a", 1001)+`"}]}`),
				}
			},
			wantInvalidEmbed: true,
		},
		{
			name: "external sources entry with a wrongly typed title",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Roundup"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.external","external":{`+
						`"uri":"https://news.example.com/roundup","title":"Roundup",`+
						`"sources":[{"uri":"https://wire.example.org/a","title":42,"domain":"wire.example.org"}]}}`),
				}
			},
			wantInvalidEmbed: true,
		},
		{
			name: "title of 301 graphemes outside the embed",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title:   lexiconStringPtr(strings.Repeat("t", 301)),
					Content: lexiconStringPtr("Body text."),
				}
			},
			wantInvalidEmbed: false,
			wantField:        "title",
		},
		{
			// 50001 graphemes is within the 500000-byte content cap, so only
			// the lexicon's content maxGraphemes refuses it — beside an embed
			// that is valid, which must not be blamed.
			name: "content of 50001 graphemes beside a valid images embed",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title:   lexiconStringPtr("Photos"),
					Content: lexiconStringPtr(strings.Repeat("c", 50001)),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"image":`+lexiconBlobJPEG+
						`,"alt":"a harbour at dusk"}]}`),
				}
			},
			wantInvalidEmbed: false,
			wantField:        "record",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)

			resp, err := h.create(tc.req(t))

			require.Errorf(t, err, "CreatePost accepted a record the lexicon rejects (response %+v)", resp)
			assert.True(t, IsValidationError(err), "expected a validation error, got %v", err)
			if tc.wantInvalidEmbed {
				assert.ErrorIs(t, err, ErrInvalidEmbed)
				var ve *ValidationError
				if assert.ErrorAs(t, err, &ve) {
					assert.Equal(t, "embed", ve.Field)
				}
			} else {
				assert.NotErrorIs(t, err, ErrInvalidEmbed)
				var ve *ValidationError
				if assert.ErrorAs(t, err, &ve) {
					assert.Equal(t, tc.wantField, ve.Field)
				}
			}
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Equal(t, 0, h.ledger.liveRows(), "the submission reservation must be released")
		})
	}
}

// ---------------------------------------------------------------------------
// B3 — update refuses a final record whose embed the lexicon rejects
// ---------------------------------------------------------------------------

func TestUpdatePost_LexiconValidationRejectsInvalidEmbed(t *testing.T) {
	validStored := `{"$type":"social.coves.community.postv2","community":"` + lexiconCommunityDID + `",` +
		`"createdAt":"2026-10-01T09:00:00Z","title":"Harbour photos",` +
		`"embed":{"$type":"social.coves.embed.images","images":[{"image":` + lexiconBlobJPEG + `,"alt":"before"}]}}`
	invalidStored := `{"$type":"social.coves.community.postv2","community":"` + lexiconCommunityDID + `",` +
		`"createdAt":"2026-10-01T09:00:00Z","title":"Harbour photos",` +
		`"embed":{"$type":"social.coves.embed.images","images":[{"image":` + lexiconBlobJPEG +
		`,"alt":"` + strings.Repeat("a", 1001) + `"}]}}`

	tests := []struct {
		name   string
		stored string
		req    func(t *testing.T) UpdatePostRequest
	}{
		{
			name:   "replacing the embed with one whose alt is 1001 graphemes",
			stored: validStored,
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"image":`+lexiconBlobJPEG+
						`,"alt":"`+strings.Repeat("a", 1001)+`"}]}`),
				}
			},
		},
		{
			name:   "title-only edit of a record whose standing embed is invalid",
			stored: invalidStored,
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Title: lexiconStringPtr("Harbour photos, corrected")}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			h.repo.seed(t, jsonObject(t, tc.stored))
			before := h.repo.snapshot(t, lexiconEditRkey)

			resp, err := h.update(tc.req(t))

			require.Errorf(t, err, "UpdatePost wrote a record the lexicon rejects (response %+v)", resp)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
		})
	}
}

// ---------------------------------------------------------------------------
// B4 — valid records pass validation and are written unchanged (GUARD)
// ---------------------------------------------------------------------------

func TestPostWrites_ValidRecordsPassLexiconValidationUnchanged(t *testing.T) {
	createTests := []struct {
		name      string
		req       func(t *testing.T) CreatePostRequest
		wantEmbed string // JSON literal; "" means no embed is stored

		// Checked only when set.
		wantTitle   string
		wantContent string
		wantFacets  string
	}{
		{
			name: "images, one image without alt or aspectRatio",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("One photo"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"image":`+lexiconBlobJPEG+`}]}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213}}]}`,
		},
		{
			name: "images, several with alt and aspectRatio",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Three photos"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[`+
						`{"image":`+lexiconBlobJPEG+`,"alt":"the harbour","aspectRatio":{"width":4,"height":3}},`+
						`{"image":`+lexiconBlobJPEG+`,"alt":"the lighthouse","aspectRatio":{"width":3,"height":4}},`+
						`{"image":`+lexiconBlobJPEG+`,"alt":"`+strings.Repeat("a", 1000)+`","aspectRatio":{"width":1,"height":1}}]}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.images","images":[` +
				`{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"the harbour","aspectRatio":{"width":4,"height":3}},` +
				`{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"the lighthouse","aspectRatio":{"width":3,"height":4}},` +
				`{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"` + strings.Repeat("a", 1000) + `","aspectRatio":{"width":1,"height":1}}]}`,
		},
		{
			name: "video with alt and aspectRatio",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("A clip"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.video","video":`+lexiconBlobMP4+
						`,"alt":"waves on the breakwater","aspectRatio":{"width":16,"height":9}}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.video","video":{"$type":"blob","ref":{"$link":"bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"},"mimeType":"video/mp4","size":2400000},"alt":"waves on the breakwater","aspectRatio":{"width":16,"height":9}}`,
		},
		{
			name: "external with uri, title and description",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Worth reading"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.external","external":{`+
						`"uri":"https://news.example.com/story","title":"The story","description":"What happened today."}}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story","title":"The story","description":"What happened today."}}`,
		},
		{
			name: "aggregator-shaped external with sources, embedType, thumb and link facets",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title:   lexiconStringPtr("Harbour reopens after storm repairs"),
					Content: lexiconStringPtr("The harbour reopened on Monday. Source: wire.example.org"),
					Facets: jsonArray(t, `[{"index":{"byteStart":40,"byteEnd":56},`+
						`"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://wire.example.org/harbour"}]}]`),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.external","external":{`+
						`"uri":"https://news.example.com/harbour-reopens","title":"Harbour reopens",`+
						`"description":"Repairs finished ahead of schedule.","embedType":"article","thumb":`+lexiconBlobJPEG+`,`+
						`"sources":[{"uri":"https://wire.example.org/harbour","title":"Wire report","domain":"wire.example.org"},`+
						`{"uri":"https://local.example.net/harbour","title":"Local report","domain":"local.example.net"}]}}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.external","external":{` +
				`"uri":"https://news.example.com/harbour-reopens","title":"Harbour reopens",` +
				`"description":"Repairs finished ahead of schedule.","embedType":"article",` +
				`"thumb":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},` +
				`"sources":[{"uri":"https://wire.example.org/harbour","title":"Wire report","domain":"wire.example.org"},` +
				`{"uri":"https://local.example.net/harbour","title":"Local report","domain":"local.example.net"}]}}`,
			wantTitle:   "Harbour reopens after storm repairs",
			wantContent: "The harbour reopened on Monday. Source: wire.example.org",
			wantFacets: `[{"index":{"byteStart":40,"byteEnd":56},` +
				`"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://wire.example.org/harbour"}]}]`,
		},
		{
			name: "post strongRef",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title: lexiconStringPtr("Quoting this"),
					Embed: jsonObject(t, `{"$type":"social.coves.embed.post","post":{`+
						`"uri":"at://did:plc:lexvalquoted2345abcdefgh/social.coves.community.postv2/3l7bsovn5rz2n",`+
						`"cid":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}}`),
				}
			},
			wantEmbed: `{"$type":"social.coves.embed.post","post":{` +
				`"uri":"at://did:plc:lexvalquoted2345abcdefgh/social.coves.community.postv2/3l7bsovn5rz2n",` +
				`"cid":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}}`,
		},
		{
			name: "no embed",
			req: func(t *testing.T) CreatePostRequest {
				return CreatePostRequest{
					Title:   lexiconStringPtr("Just text"),
					Content: lexiconStringPtr("Nothing attached."),
				}
			},
			wantEmbed:   "",
			wantTitle:   "Just text",
			wantContent: "Nothing attached.",
		},
	}

	for _, tc := range createTests {
		t.Run("create/"+tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)

			_, err := h.create(tc.req(t))
			require.NoError(t, err)

			stored := h.repo.onlyRecord(t).Value
			if tc.wantEmbed == "" {
				assert.NotContains(t, stored, "embed")
			} else {
				assert.Equal(t, jsonValue(t, tc.wantEmbed), stored["embed"])
			}
			if tc.wantTitle != "" {
				assert.Equal(t, tc.wantTitle, stored["title"])
			}
			if tc.wantContent != "" {
				assert.Equal(t, tc.wantContent, stored["content"])
			}
			if tc.wantFacets != "" {
				assert.Equal(t, jsonValue(t, tc.wantFacets), stored["facets"])
			}
		})
	}

	plainStored := `{"$type":"social.coves.community.postv2","community":"` + lexiconCommunityDID + `",` +
		`"createdAt":"2026-10-01T09:00:00Z","title":"Harbour","content":"Photos to follow."}`

	updateTests := []struct {
		name      string
		req       func(t *testing.T) UpdatePostRequest
		wantTitle string
		wantEmbed string // JSON literal; "" means no embed is stored
	}{
		{
			name: "adding images with alt and aspectRatio",
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{
					Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[`+
						`{"image":`+lexiconBlobJPEG+`,"alt":"the harbour","aspectRatio":{"width":4,"height":3}},`+
						`{"image":`+lexiconBlobJPEG+`,"alt":"the lighthouse","aspectRatio":{"width":3,"height":4}}]}`),
				}
			},
			wantTitle: "Harbour",
			wantEmbed: `{"$type":"social.coves.embed.images","images":[` +
				`{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"the harbour","aspectRatio":{"width":4,"height":3}},` +
				`{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"the lighthouse","aspectRatio":{"width":3,"height":4}}]}`,
		},
		{
			name: "title-only edit of a record with no embed",
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Title: lexiconStringPtr("Harbour, corrected")}
			},
			wantTitle: "Harbour, corrected",
			wantEmbed: "",
		},
	}

	for _, tc := range updateTests {
		t.Run("update/"+tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			h.repo.seed(t, jsonObject(t, plainStored))

			_, err := h.update(tc.req(t))
			require.NoError(t, err)

			stored := h.repo.snapshot(t, lexiconEditRkey)
			require.NotNil(t, stored)
			assert.NotEqual(t, "bafyreiseededrecord", stored.CID, "the edit must have been written")
			assert.Equal(t, tc.wantTitle, stored.Value["title"])
			assert.Equal(t, "Photos to follow.", stored.Value["content"])
			if tc.wantEmbed == "" {
				assert.NotContains(t, stored.Value, "embed")
			} else {
				assert.Equal(t, jsonValue(t, tc.wantEmbed), stored.Value["embed"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// B4b — unfurl enhancement never fails a post, and never stores an invalid one
// ---------------------------------------------------------------------------

// createWithUnfurl posts a bare link through an unfurl answering result and
// returns the stored external object, after checking the stored record
// satisfies the postv2 lexicon.
func createWithUnfurl(t *testing.T, result unfurl.UnfurlResult) map[string]interface{} {
	t.Helper()
	h := newLexiconHarness(t, &fixedUnfurl{result: result})

	_, err := h.create(CreatePostRequest{
		Title: lexiconStringPtr("A link worth reading"),
		Embed: jsonObject(t, `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/long-story"}}`),
	})
	require.NoError(t, err, "an unfurl result must not fail the post")

	stored := h.repo.onlyRecord(t)
	require.NoError(t, h.validator.ValidateRecord(stored.Value, "social.coves.community.postv2"),
		"the stored record must satisfy social.coves.community.postv2")
	embed, ok := stored.Value["embed"].(map[string]interface{})
	require.True(t, ok, "the stored record has no embed object")
	external, ok := embed["external"].(map[string]interface{})
	require.True(t, ok, "the stored embed has no external object")
	return external
}

func TestCreatePost_UnfurlEnhancementFitsTheLexicon(t *testing.T) {
	external := createWithUnfurl(t, unfurl.UnfurlResult{
		Type:        "article",
		URI:         "https://news.example.com/long-story",
		Title:       strings.Repeat("T", 501),
		Description: strings.Repeat("d", 1001),
		Provider:    "Example News",
		Domain:      "news.example.com",
	})

	assert.Equal(t, strings.Repeat("T", 500), external["title"], "title cut to 500 graphemes")
	assert.Equal(t, strings.Repeat("d", 1000), external["description"], "description cut to 1000 graphemes")
	assert.Equal(t, "article", external["embedType"])
	assert.Equal(t, "Example News", external["provider"])
	assert.Equal(t, "news.example.com", external["domain"])
}

// A title of multi-code-point graphemes hits the 5000-byte cap long before the
// 500-grapheme one: the cut lands on the last grapheme boundary within the cap.
func TestCreatePost_UnfurlTitleIsCutOnAGraphemeBoundaryAtTheByteCap(t *testing.T) {
	const family = "\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466" // one grapheme, 25 bytes
	require.Len(t, family, 25)
	require.Equal(t, 1, uniseg.GraphemeClusterCount(family))

	external := createWithUnfurl(t, unfurl.UnfurlResult{
		Type:   "article",
		URI:    "https://news.example.com/long-story",
		Title:  "x" + strings.Repeat(family, 300), // 301 graphemes, 7501 bytes
		Domain: "news.example.com",
	})

	title, ok := external["title"].(string)
	require.True(t, ok, "stored title is not a string: %#v", external["title"])
	assert.True(t, utf8.ValidString(title), "stored title is not valid UTF-8")
	// 1 + 199*25 = 4976 bytes; one more grapheme would be 5001.
	assert.Equal(t, "x"+strings.Repeat(family, 199), title)
}

// A metadata field over its lexicon maxLength is dropped, not cut.
func TestCreatePost_UnfurlDomainOverItsByteCapIsDropped(t *testing.T) {
	external := createWithUnfurl(t, unfurl.UnfurlResult{
		Type:     "article",
		URI:      "https://news.example.com/long-story",
		Title:    "A story",
		Provider: "Example News",
		Domain:   strings.Repeat("a", 254),
	})

	assert.NotContains(t, external, "domain")
	assert.Equal(t, "Example News", external["provider"])
}

// A hostile page title carrying invalid UTF-8 must still fit the lexicon once
// encoded: each invalid byte becomes a 3-byte U+FFFD on the wire.
func TestCreatePost_UnfurlTextWithInvalidUTF8FitsTheLexicon(t *testing.T) {
	// One grapheme of 9 bytes as sent, 11 once the invalid byte is replaced.
	const hostile = "\xff\u0301\u0301\u0301\u0301"

	external := createWithUnfurl(t, unfurl.UnfurlResult{
		Type:        "article",
		URI:         "https://news.example.com/long-story",
		Title:       strings.Repeat(hostile, 600),
		Description: strings.Repeat(hostile, 1200),
		Domain:      "news.example.com",
	})

	for field, limits := range map[string]struct{ graphemes, bytes int }{
		"title":       {externalTitleMaxGraphemes, externalTitleMaxBytes},
		"description": {externalDescriptionMaxGraphemes, externalDescriptionMaxBytes},
	} {
		text, ok := external[field].(string)
		require.Truef(t, ok, "stored %s is not a string: %#v", field, external[field])
		assert.NotEmptyf(t, text, "stored %s", field)
		assert.Truef(t, utf8.ValidString(text), "stored %s is not valid UTF-8", field)
		assert.LessOrEqualf(t, len(text), limits.bytes, "stored %s bytes", field)
		assert.LessOrEqualf(t, uniseg.GraphemeClusterCount(text), limits.graphemes, "stored %s graphemes", field)
	}
}

// ---------------------------------------------------------------------------
// Title and tag grapheme limits are refused by name
// ---------------------------------------------------------------------------

func TestPostContent_TitleAndTagGraphemeLimitsNameTheField(t *testing.T) {
	// "é" is one grapheme of two bytes, so these stay far under the byte caps.
	createTests := []struct {
		name      string
		req       CreatePostRequest
		wantField string // "" means the create succeeds
	}{
		{
			name:      "title of 301 graphemes within 3000 bytes",
			req:       CreatePostRequest{Title: lexiconStringPtr(strings.Repeat("é", 301))},
			wantField: "title",
		},
		{
			name: "title of exactly 300 graphemes",
			req:  CreatePostRequest{Title: lexiconStringPtr(strings.Repeat("é", 300))},
		},
		{
			name: "tag of 65 graphemes within 640 bytes",
			req: CreatePostRequest{
				Title: lexiconStringPtr("Tagged"),
				Tags:  []string{"harbour", strings.Repeat("é", 65)},
			},
			wantField: "tags",
		},
		{
			name: "tag of exactly 64 graphemes",
			req: CreatePostRequest{
				Title: lexiconStringPtr("Tagged"),
				Tags:  []string{strings.Repeat("é", 64)},
			},
		},
	}

	for _, tc := range createTests {
		t.Run("create/"+tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)

			_, err := h.create(tc.req)

			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var ve *ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, tc.wantField, ve.Field)
			assert.Contains(t, ve.Message, "graphemes")
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Empty(t, h.ledger.reserveCalls, "no submission reservation may be taken")
		})
	}

	// An older post whose title predates the grapheme cap: the edit is
	// refused naming the title, so its author knows what to shorten.
	t.Run("update/standing title of 301 graphemes", func(t *testing.T) {
		h := newLexiconHarness(t, nil)
		h.repo.seed(t, jsonObject(t, `{"$type":"social.coves.community.postv2","community":"`+lexiconCommunityDID+`",`+
			`"createdAt":"2026-10-01T09:00:00Z","title":"`+strings.Repeat("é", 301)+`","content":"Body."}`))
		before := h.repo.snapshot(t, lexiconEditRkey)

		_, err := h.update(UpdatePostRequest{Content: lexiconStringPtr("Body, edited.")})

		require.Error(t, err)
		var ve *ValidationError
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, "title", ve.Field)
		assert.Contains(t, ve.Message, "graphemes")
		assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
	})
}

// ---------------------------------------------------------------------------
// A client-invalid record is refused before admission, unfurl or thumbnail
// ---------------------------------------------------------------------------

func TestCreatePost_ClientInvalidLinkEmbedIsRefusedBeforeAnySideEffect(t *testing.T) {
	tests := []struct {
		name     string
		external string // the external object's members after "uri"
	}{
		{
			name:     "sources entry title of 501 graphemes",
			external: `,"sources":[{"uri":"https://wire.example.org/a","title":"` + strings.Repeat("s", 501) + `"}]`,
		},
		{
			name:     "totalCount of -1",
			external: `,"totalCount":-1`,
		},
		{
			name:     "images entry without its image blob",
			external: `,"images":[{"alt":"a harbour at dusk"}]`,
		},
		{
			name:     "images entry alt of 1001 graphemes",
			external: `,"images":[{"image":` + lexiconBlobJPEG + `,"alt":"` + strings.Repeat("a", 1001) + `"}]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unfurlService := &fixedUnfurl{result: unfurl.UnfurlResult{
				Type:         "article",
				URI:          "https://news.example.com/story",
				Title:        "The story",
				ThumbnailURL: "https://news.example.com/thumbnail.jpg",
			}}
			fetcher := &countingThumbnailFetcher{}
			h := newLexiconHarnessWithBlobs(t, unfurlService, fetcher)

			_, err := h.create(CreatePostRequest{
				Title: lexiconStringPtr("A link"),
				Embed: jsonObject(t, `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story"`+
					tc.external+`}}`),
			})

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			assert.Empty(t, h.ledger.reserveCalls, "no submission reservation may be taken")
			assert.Zero(t, unfurlService.calls, "no unfurl fetch may happen")
			assert.Zero(t, fetcher.fetches, "no thumbnail fetch may happen")
			assert.Zero(t, h.repo.uploads, "no blob may be uploaded")
			assert.Empty(t, h.repo.records, "no record may be written")
		})
	}
}

// ---------------------------------------------------------------------------
// A trusted aggregator's record is held to the lexicon too
// ---------------------------------------------------------------------------

func TestCreatePost_TrustedAggregatorRecordIsLexiconValidated(t *testing.T) {
	t.Setenv("TRUSTED_AGGREGATOR_DIDS", lexiconAuthorDID)
	h := newLexiconHarness(t, nil)

	_, err := h.create(CreatePostRequest{
		Title: lexiconStringPtr("Aggregated photos"),
		Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":[{"image":`+lexiconBlobJPEG+
			`,"alt":"`+strings.Repeat("a", 1001)+`"}]}`),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEmbed)
	assert.Empty(t, h.ledger.reserveCalls, "no submission reservation may be taken")
	assert.Empty(t, h.repo.records, "no record may be written")
}
