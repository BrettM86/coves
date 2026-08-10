//go:build integration

package posts_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"
	"Coves/internal/core/unfurl"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Where a post's MEDIA ends up, now that the post itself lives in the author's
// repository (docs/PRD_AUTHOR_OWNED_POSTS.md §4.2 step 2).
//
// # WHY THE BLOB CANNOT BE LEFT BEHIND
//
// The record moved and the blob has to move with it. A postv2 record in the
// author's repo whose thumb blob was uploaded to the COMMUNITY's PDS is a record
// pointing at media in a repository it has no relationship to, and every failure
// that produces is quiet:
//
//   - The blob ref in the record names a CID, not a repo. A reader resolves it
//     against the repo it believes owns the record — the author's — and gets
//     nothing back. A broken image, no error, nothing in a log.
//   - The community can garbage-collect a blob no record in ITS repo references.
//     atProto blobs are reference-counted per repository, so an orphan upload is
//     media on borrowed time in someone else's storage.
//   - Deleting the post deletes the record and dereferences nothing, because the
//     blob was never the author's to release. An author who deletes a post keeps
//     paying for its images in a repo they cannot reach.
//
// # WHAT MUST SURVIVE THE MOVE
//
// UploadBlobFromURL is a CHOKE POINT, not a convenience: it bounds the fetch
// with a timeout, refuses a Content-Type outside the image allowlist, and caps
// the body at 6MB. Every one of those exists because the URL being fetched is
// attacker-influenced — a client picks the page that gets unfurled, and the page
// picks the thumbnail. A new upload path that reached the PDS without them would
// turn a link preview into an unbounded fetch performed by the AppView, with the
// author's own credentials, into the author's own storage quota.
//
// So this file pins two things: that the blob lands in the AUTHOR's repo, and
// that it still has to get past the guard to land anywhere.

// blobFixture is the write path wired with a real blob service and an unfurl
// service the test scripts.
//
// UNFURL IS THE ROUTE IN because it is the one a regular user takes. The other
// producer of a thumbnail — CreatePostRequest.ThumbnailURL — is only honoured for
// a TRUSTED aggregator, and trust is read from the process environment, which
// t.Setenv cannot be combined with t.Parallel. The unfurl path exercises the
// same UploadBlobFromURL call with no such constraint.
type blobFixture struct {
	base    *postFixture
	service posts.Service

	// origin serves the images the "remote page" offers as its thumbnail.
	origin *imageOrigin
}

// imageOrigin is the remote host a thumbnail is fetched from — the untrusted
// end of the choke point.
type imageOrigin struct {
	server *httptest.Server

	// body and contentType are what the next fetch receives.
	body        []byte
	contentType string
}

func newImageOrigin(t *testing.T) *imageOrigin {
	t.Helper()

	origin := &imageOrigin{
		body:        testkit.TestPNG(64, 64),
		contentType: "image/png",
	}
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", origin.contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(origin.body)
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

func (o *imageOrigin) thumbnailURL() string { return o.server.URL + "/thumb.png" }

// scriptedUnfurl reports every URL as supported and answers with a thumbnail
// pointing at the origin. The unfurl domain has its own tests; what matters here
// is only that a thumbnail URL reaches the blob service.
type scriptedUnfurl struct {
	thumbnailURL string
}

func (s *scriptedUnfurl) IsSupported(string) bool { return true }

func (s *scriptedUnfurl) UnfurlURL(context.Context, string) (*unfurl.UnfurlResult, error) {
	return &unfurl.UnfurlResult{
		Type:         "article",
		Title:        "An article with a picture",
		Description:  "unfurled",
		ThumbnailURL: s.thumbnailURL,
		Provider:     "example",
		Domain:       "example.com",
	}, nil
}

func newBlobFixture(t *testing.T) *blobFixture {
	t.Helper()

	base := newPostFixture(t)
	origin := newImageOrigin(t)

	return &blobFixture{
		base:   base,
		origin: origin,
		service: posts.NewPostService(
			postgres.NewPostRepository(base.db), base.communityService,
			nil, // aggregators
			// The origin is an httptest server on loopback, which is exactly what
			// the remote-fetch SSRF guard refuses. Production never passes this.
			blobs.NewBlobService(base.pds.URL(), blobs.WithPrivateHostsAllowed()),
			&scriptedUnfurl{thumbnailURL: origin.thumbnailURL()},
			nil, // bluesky
			base.pds.URL(),
			append(base.writePathOptions(),
				posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))...),
	}
}

// postWithExternalEmbed submits a post carrying an external embed, which is what
// makes the unfurl-and-upload path run at all.
func (f *blobFixture) postWithExternalEmbed(t *testing.T, title string) (*posts.CreatePostResponse, error) {
	t.Helper()

	content := "a post whose link preview has a picture"
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), f.base.author.DID),
		sessionFor(t, f.base.author, f.base.pds.URL()),
		posts.CreatePostRequest{
			Community: f.base.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.base.author.DID,
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com/an-article",
				},
			},
		})
}

// thumbCIDOf digs the blob CID out of a written record's external embed, or
// reports that the record carries no thumb at all.
func thumbCIDOf(t *testing.T, record testkit.RecordValue) (string, bool) {
	t.Helper()

	embed, ok := record.Value["embed"].(map[string]any)
	if !ok {
		return "", false
	}
	external, ok := embed["external"].(map[string]any)
	if !ok {
		return "", false
	}
	thumb, ok := external["thumb"].(map[string]any)
	if !ok {
		return "", false
	}
	ref, ok := thumb["ref"].(map[string]any)
	require.Truef(t, ok, "the thumb is not a blob reference: %#v", thumb)
	link, ok := ref["$link"].(string)
	require.Truef(t, ok, "the blob reference has no $link: %#v", ref)
	return link, true
}

// listBlobs returns the CIDs of every blob held in an account's repository —
// com.atproto.sync.listBlobs, which is the PDS's own answer to "whose storage is
// this in" and cannot be satisfied by a record that merely mentions a CID.
func listBlobs(t *testing.T, account *testkit.Account) []string {
	t.Helper()

	var resp struct {
		CIDs []string `json:"cids"`
	}
	require.NoError(t, account.XRPC().Query(context.Background(), "com.atproto.sync.listBlobs",
		map[string][]string{
			"did":   {account.DID},
			"limit": {"100"},
		}, &resp),
		"listing the blobs of %s", account.DID)
	return resp.CIDs
}

func TestService_AThumbnailBlobLandsInTheAuthorsRepo(t *testing.T) {
	t.Parallel()

	f := newBlobFixture(t)
	resp, err := f.postWithExternalEmbed(t, "a link with a preview image")
	require.NoError(t, err)

	record := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	thumbCID, hasThumb := thumbCIDOf(t, record)
	require.Truef(t, hasThumb,
		"the unfurled thumbnail never made it onto the record; embed was %#v", record.Value["embed"])

	// THE BLOB IS IN THE AUTHOR'S STORAGE. Asserted through listBlobs rather
	// than by re-reading the record, because the record only names a CID: a blob
	// uploaded to the community's PDS produces a record that looks exactly like
	// this one and resolves for nobody.
	assert.Containsf(t, listBlobs(t, f.base.author), thumbCID,
		"the post's thumbnail blob is not in the author's repository; the record that references it "+
			"lives there, so a reader resolving this CID against the author's PDS gets nothing back")

	// AND NOT IN THE COMMUNITY'S. The mirror assertion, for the same reason the
	// write-forward test lists both repos: a path that uploaded to both would
	// satisfy the check above while still orphaning a blob in storage the author
	// cannot release when they delete the post.
	assert.NotContainsf(t, listBlobs(t, f.base.communityAccount(t)), thumbCID,
		"the thumbnail was uploaded into the community's repository, where no record references it — "+
			"the community can garbage-collect it, and deleting the post will not release it")
}

func TestService_TheBlobGuardStillRefusesAnOversizeThumbnail(t *testing.T) {
	t.Parallel()

	// THE CHOKE POINT HAS TO SURVIVE THE MOVE. The URL being fetched here is
	// attacker-influenced twice over — a client chooses the page, the page
	// chooses the thumbnail — and the 6MB cap is what stops a link preview
	// becoming an unbounded fetch the AppView performs with the author's own
	// credentials into the author's own storage quota.
	//
	// The body is not a real image: the guard rejects on the Content-Type header
	// and the byte count, neither of which requires decoding, so 7MB of zeros
	// exercises the same branch as 7MB of PNG without the cost of producing one.
	f := newBlobFixture(t)
	f.origin.body = make([]byte, 7*1024*1024)

	resp, err := f.postWithExternalEmbed(t, "a link whose preview image is enormous")

	// The POST still succeeds. A thumbnail is an enhancement, and it has never
	// been able to fail a post — the write path logs the upload failure and
	// carries on. That stays true on the author path: an author must not lose a
	// post because a remote page served a 7MB image.
	require.NoErrorf(t, err,
		"a refused thumbnail must not fail the post; the record is the author's and it is complete without it")

	record := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	_, hasThumb := thumbCIDOf(t, record)
	assert.Falsef(t, hasThumb,
		"the oversize image was uploaded and attached anyway — the size cap did not survive the move to "+
			"the author's PDS")

	// And nothing reached storage. Checked separately from the record because
	// the two failures are different: a record with no thumb but a blob in the
	// repo means the upload SUCCEEDED and only the attachment was dropped, which
	// is the cap being enforced in the wrong place and paying the full cost of
	// not having it.
	assert.Emptyf(t, listBlobs(t, f.base.author),
		"the oversize image reached the author's storage; the cap must refuse it before the upload, "+
			"not discard the result afterwards")
}

func TestService_TheBlobGuardStillRefusesANonImageThumbnail(t *testing.T) {
	t.Parallel()

	// The other half of the guard. An allowlist that admitted arbitrary
	// Content-Types would let a remote page put anything it liked into the
	// author's repository — the AppView fetching it, the author's session
	// signing for it, and the image proxy later serving it to readers.
	f := newBlobFixture(t)
	f.origin.contentType = "text/html"
	f.origin.body = []byte("<html>not an image at all</html>")

	resp, err := f.postWithExternalEmbed(t, "a link whose preview is not an image")
	require.NoError(t, err)

	record := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	_, hasThumb := thumbCIDOf(t, record)
	assert.False(t, hasThumb, "a text/html body was accepted as a thumbnail")

	assert.Empty(t, listBlobs(t, f.base.author),
		"a non-image body reached the author's storage")
}

func TestService_ThePostSurvivesAThumbnailOriginThatIsDown(t *testing.T) {
	t.Parallel()

	// The ordinary operational case, pinned because the author path makes it
	// newly dangerous to get wrong. A dead thumbnail host used to cost the
	// COMMUNITY's write nothing; it must equally cost the AUTHOR's write
	// nothing, or a remote page being down becomes a reason a person cannot
	// post.
	f := newBlobFixture(t)
	f.origin.server.Close()

	resp, err := f.postWithExternalEmbed(t, "a link whose preview host is unreachable")
	require.NoErrorf(t, err, "an unreachable thumbnail host must not fail the post")

	record := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	_, hasThumb := thumbCIDOf(t, record)
	assert.False(t, hasThumb)

	// The rest of the unfurl still landed: the post keeps the metadata that did
	// not depend on the fetch.
	embed, ok := record.Value["embed"].(map[string]any)
	require.True(t, ok)
	external, ok := embed["external"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "An article with a picture", external["title"],
		"the thumbnail failed, not the unfurl — the metadata that needed no fetch must survive")
}

// blobOwnerReadPath is the read-side half of the same question, and the half a
// pure test cannot reach.
//
// blob_transform_test.go pins WHICH owner a record's blobs resolve against.
// What it cannot pin is that the author's PDS URL is populated at all: the
// repository query has always SELECTed users.pds_url — it hydrates the author's
// avatar with it — and has always dropped it on the floor before the view is
// returned. An owner-selection that is perfectly correct over a field nobody
// fills in resolves every postv2 post's media against an empty host.
func TestService_AuthorPDSIsHydratedOntoPostViews(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()

	// Indexed the way the firehose consumer would, rather than by posting: the
	// subject is the READ path, and a real write would leave the row waiting on
	// a consumer this fixture does not run.
	rkey := testkit.TID()
	uri := "at://" + f.author.DID + "/" + posts.PostV2Collection + "/" + rkey
	_, err := f.db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO UPDATE SET pds_url = EXCLUDED.pds_url
	`, f.author.DID, f.author.DID+".test", f.pds.URL())
	require.NoError(t, err)

	_, err = f.db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, uri, "bafyblobowner", rkey, f.author.DID, f.community.DID, "a post with media")
	require.NoError(t, err)

	// This subject is a postv2, so the task-7 visibility predicate hides it from
	// the anonymous GetViewsByURIs read below unless a community has accepted it —
	// a postv2 with no admission row fails CLOSED (the consumer always seeds a
	// pending row, so a missing one means a failed seed). This test is about PDS
	// HYDRATION on a VISIBLE row, not visibility, so it needs the row to be
	// visible: seed the accepted admission the acceptance engine would have
	// written.
	_, err = f.db.ExecContext(ctx, `
		INSERT INTO community_post_admissions (community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
	`, f.community.DID, uri, "bafyblobowner")
	require.NoError(t, err)

	views, err := postgres.NewPostRepository(f.db).GetViewsByURIs(ctx, []string{uri}, "")
	require.NoError(t, err)
	require.Contains(t, views, uri)

	author := views[uri].Author
	require.NotNil(t, author)
	assert.Equal(t, f.author.DID, author.DID)
	assert.Equalf(t, f.pds.URL(), author.PDSURL,
		"the author's PDS URL is not hydrated onto the view, so a postv2 post's blob URLs would be "+
			"built against an empty host — the column is already SELECTed for the avatar and only "+
			"needs carrying")

	// The community's stays populated beside it. The two owners coexist in one
	// view because one table holds both kinds of record, and a change that
	// carried the author's host by overwriting the community's would break every
	// pre-flip post's media instead.
	require.NotNil(t, views[uri].Community)
	assert.NotEmpty(t, views[uri].Community.PDSURL,
		"the community's PDS URL must survive: deprecated community.post records still resolve their "+
			"blobs against it")
}
