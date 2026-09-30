//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const (
	removeContentMethod  = "social.coves.moderation.removeContent"
	restoreContentMethod = "social.coves.moderation.restoreContent"
)

// TestModerationCommentRemovalContract crosses the real PDS → consumer → AppView
// boundary twice: once for the comment and again for the author's edit while the
// moderation overlay is active. The edited CID is the delivery barrier for the
// negative assertion: a placeholder seen before that edit was indexed proves
// nothing about whether the edit can resurrect removed content.
func TestModerationCommentRemovalContract(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "mca")
	community := indexedCommunity(t, p, "mca", author.DID)
	post := indexedPost(t, p, community, author, "moderation image comment")
	outsider := p.IndexedAccount(t, "mco")
	admin := testkit.ModerationAdmin(t, 1)

	image := author.UploadBlob(t, testkit.TestPNG(64, 64), "image/png")
	// The edit introduces a second image. Only the consumer's media
	// reconciliation can block it, since the removal never saw it.
	editImage := author.UploadBlob(t, testkit.TestPNG(96, 96), "image/png")
	require.NotEqual(t, image.CID(), editImage.CID())
	rkey := testkit.TID()
	uri := commentURI(author.DID, rkey)
	initialText := "original comment " + testkit.UniqueID(t)
	editedText := "edited comment " + testkit.UniqueID(t)
	writeComment := func(content string, blobs ...testkit.BlobRef) string {
		t.Helper()
		record := commentRecord(post, post, content)
		images := make([]any, 0, len(blobs))
		for _, blob := range blobs {
			images = append(images, map[string]any{"image": blobRefValue(blob), "alt": "moderation contract image"})
		}
		record["embed"] = map[string]any{"$type": "social.coves.embed.images", "images": images}
		return author.PutRecord(t, commentCollection, rkey, record).CID
	}
	createdCID := writeComment(initialText, image)

	var imageURL string
	p.Await(t, "the directly written image comment to appear in the thread", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(uri)
		if !found || node.Comment.CID != createdCID || node.Comment.Record["content"] != initialText {
			return false, nil
		}
		images, ok := node.Comment.Embed["images"].([]any)
		if !ok || len(images) != 1 {
			return false, nil
		}
		servedImage, ok := images[0].(map[string]any)
		if !ok {
			return false, nil
		}
		imageURL, ok = servedImage["fullsize"].(string)
		return ok && imageURL != "", nil
	}, withReadCadence())
	require.Contains(t, imageURL, image.CID())
	requireServesImage(t, p, "comment image before removal", imageURL)
	parsedImage, err := url.Parse(imageURL)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(parsedImage.Path, "/img/"), "the served URL must use the AppView image proxy")

	stateToken := admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	readState := func() (moderationSubjectStateResponse, error) {
		var response moderationSubjectStateResponse
		err := p.AppView.As(stateToken).Query(
			context.Background(), subjectStateMethod, url.Values{"subject": {uri}}, &response)
		return response, err
	}
	initialState, err := readState()
	require.NoError(t, err)
	require.Equal(t, "v0", initialState.State.Version)
	require.Equal(t, "clear", initialState.State.Moderation.State)
	require.Equal(t, createdCID, initialState.State.CurrentSubject.CID)

	removeInput := map[string]any{
		"subject":         map[string]any{"uri": uri, "cid": initialState.State.CurrentSubject.CID},
		"expectedVersion": initialState.State.Version,
		"idempotencyKey":  testkit.UniqueID(t),
		"reason":          "social.coves.moderation.defs#reasonSpam",
	}
	err = p.AppView.As(outsider.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
		t.Context(), removeContentMethod, removeInput, nil)
	requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "a non-admin removal")
	unchangedState, err := readState()
	require.NoError(t, err)
	require.Equal(t, "v0", unchangedState.State.Version)
	require.Equal(t, "clear", unchangedState.State.Moderation.State)

	// Re-read immediately before the mutation: the request must carry the
	// inspected indexed CID and version, not just the PDS's write response.
	inspected, err := readState()
	require.NoError(t, err)
	require.Equal(t, createdCID, inspected.State.CurrentSubject.CID)
	removeInput["subject"] = map[string]any{"uri": uri, "cid": inspected.State.CurrentSubject.CID}
	removeInput["expectedVersion"] = inspected.State.Version
	var removal struct {
		Outcome string `json:"outcome"`
		Action  struct {
			Action struct {
				Ref struct {
					ActionID string `json:"actionId"`
				} `json:"ref"`
			} `json:"action"`
		} `json:"action"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
		t.Context(), removeContentMethod, removeInput, &removal)
	require.NoError(t, err)
	require.Equal(t, "applied", removal.Outcome)
	require.NotEmpty(t, removal.Action.Action.Ref.ActionID)

	placeholder := func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(uri)
		if !found {
			return false, fmt.Errorf("removed comment disappeared from thread: %s", thread.uris())
		}
		comment := node.Comment
		if !comment.IsDeleted || comment.Record != nil || comment.Embed != nil ||
			comment.DeletionReason == nil || *comment.DeletionReason != "moderator" ||
			comment.Moderation == nil || comment.Moderation.State != "removed" ||
			comment.Author.Handle != "handle.invalid" {
			return false, nil
		}
		return true, nil
	}
	// getComments has a separate 20/minute per-IP limit. A phase boundary
	// leaves room for both the placeholder wait and the post-edit Holds window.
	p.FreshReadQuota(t, "moderator-placeholder")
	p.Await(t, "moderator removal to render as a content-free placeholder", placeholder, withReadCadence())
	pathBlocked := func(path string) (bool, error) {
		_, err := p.AppView.GetBinary(context.Background(), path)
		if testkit.IsStatus(err, http.StatusNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}
	imageBlocked := func() (bool, error) { return pathBlocked(parsedImage.Path) }
	p.Await(t, "the previously served comment image to return 404", imageBlocked)

	// The edit's image is served under the same owner and preset as the
	// original, so its proxy path differs only in the CID.
	editImagePath := strings.Replace(parsedImage.Path, image.CID(), editImage.CID(), 1)
	require.NotEqual(t, parsedImage.Path, editImagePath)
	editedCID := writeComment(editedText, image, editImage)
	require.NotEqual(t, createdCID, editedCID)
	// R8: observe the edited record in the AppView index before asserting that
	// the author's putRecord did not undo the active removal.
	stateToken = admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	p.Await(t, "getSubjectState to report the author's edited CID", func() (bool, error) {
		state, err := readState()
		if err != nil {
			return false, err
		}
		return state.State.CurrentSubject.CID == editedCID, nil
	})
	p.Holds(t, "the edited comment to remain hidden and both its images blocked", func() (bool, error) {
		stillRemoved, err := placeholder()
		if err != nil || !stillRemoved {
			return stillRemoved, err
		}
		blocked, err := imageBlocked()
		if err != nil || !blocked {
			return blocked, err
		}
		return pathBlocked(editImagePath)
	})

	currentState, err := readState()
	require.NoError(t, err)
	require.Equal(t, editedCID, currentState.State.CurrentSubject.CID)
	require.Equal(t, "removed", currentState.State.Moderation.State)
	var restoration struct {
		Outcome string `json:"outcome"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, restoreContentMethod)).Procedure(
		t.Context(), restoreContentMethod, map[string]any{
			"actionId":        removal.Action.Action.Ref.ActionID,
			"reviewedSubject": map[string]any{"uri": uri, "cid": editedCID},
			"expectedVersion": currentState.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
		}, &restoration)
	require.NoError(t, err)
	require.Equal(t, "applied", restoration.Outcome)

	p.FreshReadQuota(t, "restored-comment")
	p.Await(t, "restored comment to serve the edited text", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(uri)
		return found && !node.Comment.IsDeleted && node.Comment.Record["content"] == editedText &&
			node.Comment.Moderation == nil, nil
	}, withReadCadence())
	for _, path := range []string{parsedImage.Path, editImagePath} {
		p.Await(t, "the restored comment's images to serve again", func() (bool, error) {
			response, err := p.AppView.GetBinary(context.Background(), path)
			if testkit.IsStatus(err, http.StatusNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return response.Status == http.StatusOK && len(response.Body) > 0 &&
				strings.HasPrefix(response.ContentType, "image/"), nil
		})
	}
}

// TestModerationPostRemovalContract follows an author-owned image post across
// the PDS, the AppView's consumers, instance removal, an author edit, community
// re-acceptance and restoration. The edited CID is the indexing barrier before
// checking that the overlay still hides the post and blocks the new image.
func TestModerationPostRemovalContract(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "mpa")
	authorToken := p.AppView.SignIn(t, author)
	authorView := p.AppView.As(authorToken)
	community := indexedCommunity(t, p, "mpa", author.DID)
	admin := testkit.ModerationAdmin(t, 1)
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	needle := "modpost" + testkit.UniqueID(t)
	title := needle + " acceptance title"
	content := "original post content " + testkit.UniqueID(t)
	image := author.UploadBlob(t, testkit.TestPNG(64, 64), "image/png")
	writePost := func(postTitle, postContent string, blobs ...testkit.BlobRef) string {
		t.Helper()
		record := postV2Record(community.DID, postTitle, postContent)
		images := make([]any, 0, len(blobs))
		for _, blob := range blobs {
			images = append(images, map[string]any{"image": blobRefValue(blob), "alt": "post moderation image"})
		}
		record["embed"] = map[string]any{"$type": "social.coves.embed.images", "images": images}
		return author.PutRecord(t, postV2Collection, rkey, record).CID
	}
	createdCID := writePost(title, content, image)
	awaitStatus(t, p, uri, community.DID, "pending", "the author's image post to reach the admission queue")
	acceptRkey := subjectRkey(uri)
	community.PutRecord(t, acceptanceCollection, acceptRkey, acceptanceRecord(uri, createdCID))
	accepted := awaitStatus(t, p, uri, community.DID, "accepted", "the community to accept the image post")

	readPost := func() (map[string]any, error) {
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		err := p.AppView.Query(context.Background(), "social.coves.community.post.get",
			url.Values{"uris": {uri}}, &response)
		if err != nil {
			return nil, err
		}
		if len(response.Posts) != 1 {
			return nil, fmt.Errorf("post.get returned %d union members for one URI", len(response.Posts))
		}
		return response.Posts[0], nil
	}
	// GetBinary preserves the actual response bytes so content leakage is checked
	// across the whole response, not just in the decoded post union member.
	readAuthorPost := func() (map[string]any, []byte, error) {
		path := "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
		response, err := authorView.GetBinary(t.Context(), path)
		if err != nil {
			return nil, nil, err
		}
		var out struct {
			Posts []map[string]any `json:"posts"`
		}
		if err := json.Unmarshal(response.Body, &out); err != nil {
			return nil, nil, fmt.Errorf("decoding author post.get: %w", err)
		}
		if len(out.Posts) != 1 {
			return nil, nil, fmt.Errorf("author post.get returned %d union members for one URI", len(out.Posts))
		}
		return out.Posts[0], response.Body, nil
	}
	requireModeratedAuthorPost := func(forbiddenContent ...string) {
		t.Helper()
		post, body, err := readAuthorPost()
		require.NoError(t, err)
		require.Equal(t, "social.coves.community.post.defs#moderatedPost", post["$type"])
		require.Equal(t, uri, post["uri"])
		moderation, ok := post["moderation"].(map[string]any)
		require.True(t, ok, "author's moderated post must carry moderation details")
		require.Equal(t, "removed", moderation["state"])
		sources, ok := moderation["sources"].([]any)
		require.True(t, ok, "moderation sources must be an array")
		require.Len(t, sources, 1)
		source, ok := sources[0].(map[string]any)
		require.True(t, ok, "moderation source must be an object")
		require.Equal(t, communityInstanceDID, source["authorityDid"])
		scope, ok := source["scope"].(map[string]any)
		require.True(t, ok, "moderation scope must be an object")
		require.Equal(t, "instance", scope["kind"])
		for _, key := range []string{"record", "title", "embed"} {
			require.NotContains(t, post, key, "moderated post must not expose content")
		}
		for _, text := range forbiddenContent {
			require.NotContains(t, string(body), text, "moderated response leaked post content")
		}
	}
	readAuthorFeedURIs := func(method string, params url.Values) []string {
		t.Helper()
		var feed struct {
			Feed []feedItemView `json:"feed"`
		}
		require.NoError(t, authorView.Query(t.Context(), method, params, &feed))
		uris := make([]string, 0, len(feed.Feed))
		for _, item := range feed.Feed {
			uris = append(uris, item.Post.URI)
		}
		return uris
	}
	authorFeedParams := url.Values{"actor": {author.DID}, "limit": {"25"}}
	communityFeedParams := url.Values{"community": {community.DID}, "sort": {"new"}, "limit": {"50"}}
	var imageURL string
	p.Await(t, "the accepted image post to serve through post.get", func() (bool, error) {
		post, err := readPost()
		if err != nil {
			return false, err
		}
		record, ok := post["record"].(map[string]any)
		if !ok || post["uri"] != uri || record["title"] != title || record["content"] != content || post["$type"] != nil {
			return false, nil
		}
		embed, ok := post["embed"].(map[string]any)
		if !ok {
			return false, nil
		}
		images, ok := embed["images"].([]any)
		if !ok || len(images) != 1 {
			return false, nil
		}
		served, ok := images[0].(map[string]any)
		if !ok {
			return false, nil
		}
		imageURL, ok = served["fullsize"].(string)
		return ok && imageURL != "", nil
	})
	require.Contains(t, imageURL, image.CID())
	servedImage := requireServesImage(t, p, "accepted post image", imageURL)
	require.Equal(t, "public, max-age=86400", servedImage.Header.Get("Cache-Control"))
	require.NotContains(t, servedImage.Header.Get("Cache-Control"), "s-maxage")
	parsedImage, err := url.Parse(imageURL)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(parsedImage.Path, "/img/"), "the image must be served through the AppView proxy")
	require.Contains(t, communityFeedURIs(t, p, community.DID), uri,
		"the accepted post must appear in the feed before its removal can prove exclusion")
	require.Contains(t, readAuthorFeedURIs("social.coves.actor.getPosts", authorFeedParams), uri)
	require.Contains(t, readAuthorFeedURIs("social.coves.communityFeed.getCommunity", communityFeedParams), uri)
	p.Await(t, "search to find the accepted post before removal", func() (bool, error) {
		search, err := queryPostSearch(p, needle, community.DID)
		if err != nil {
			return false, err
		}
		return len(search.Feed) == 1 && search.Feed[0].Post.URI == uri, nil
	}, withReadCadence())

	commentRkey := testkit.TID()
	commentURI := commentURI(author.DID, commentRkey)
	commentText := "comment under removed post " + testkit.UniqueID(t)
	ref := strongRef{URI: uri, CID: createdCID}
	author.PutRecord(t, commentCollection, commentRkey, commentRecord(ref, ref, commentText))
	p.Await(t, "the comment to appear in the accepted post's thread", func() (bool, error) {
		thread, err := p.Thread(context.Background(), uri, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(commentURI)
		return found && node.Comment.Record["content"] == commentText, nil
	}, withReadCadence())
	// The wait above can spend 19 of getComments' 20 reads per minute, and the
	// removal phase reads the thread three more times.
	p.FreshReadQuota(t, "removed-post-thread")
	// As copies the client IP, and FreshReadQuota just replaced it.
	authorView = p.AppView.As(authorToken)

	stateToken := admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	readState := func() (moderationSubjectStateResponse, error) {
		var state moderationSubjectStateResponse
		err := p.AppView.As(stateToken).Query(context.Background(), subjectStateMethod,
			url.Values{"subject": {uri}}, &state)
		return state, err
	}
	state, err := readState()
	require.NoError(t, err)
	require.Equal(t, createdCID, state.State.CurrentSubject.CID)
	require.Equal(t, "clear", state.State.Moderation.State)
	var removal struct {
		Outcome string `json:"outcome"`
		Action  struct {
			Action struct {
				Ref struct {
					ActionID string `json:"actionId"`
				} `json:"ref"`
			} `json:"action"`
		} `json:"action"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
		t.Context(), removeContentMethod, map[string]any{
			"subject":         map[string]any{"uri": uri, "cid": state.State.CurrentSubject.CID},
			"expectedVersion": state.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonSpam",
		}, &removal)
	require.NoError(t, err)
	require.Equal(t, "applied", removal.Outcome)
	require.NotEmpty(t, removal.Action.Action.Ref.ActionID)
	admissionAfterRemoval, err := p.PostStatus(context.Background(), uri, community.DID)
	require.NoError(t, err)
	require.Equal(t, accepted, admissionAfterRemoval, "instance removal must not change the community's acceptance")

	moderated := func() (bool, error) {
		post, err := readPost()
		if err != nil {
			return false, err
		}
		if post["$type"] != "social.coves.community.post.defs#moderatedPost" || post["uri"] != uri {
			return false, nil
		}
		for _, key := range []string{"record", "title", "embed"} {
			if _, leaked := post[key]; leaked {
				return false, fmt.Errorf("moderated post leaked %s: %#v", key, post)
			}
		}
		return true, nil
	}
	notFoundAnonymously := func() (bool, error) {
		post, err := readPost()
		if err != nil {
			return false, err
		}
		for _, key := range []string{"record", "title", "embed", "cid"} {
			if _, leaked := post[key]; leaked {
				return false, fmt.Errorf("notFound post leaked %s: %#v", key, post)
			}
		}
		return post["notFound"] == true && post["uri"] == uri, nil
	}
	removed, err := moderated()
	require.NoError(t, err)
	require.True(t, removed, "post.get must serve a content-free moderatedPost immediately after removal")
	requireModeratedAuthorPost(title, content)
	missingRootURI := authorPostURI(author.DID, testkit.TID())
	_, missingRootErr := p.Thread(context.Background(), missingRootURI, nil)
	missingRoot := requireXRPCRefusal(t, missingRootErr, http.StatusNotFound, "RootNotFound", "a never-indexed post thread")
	_, removedRootErr := p.Thread(context.Background(), uri, nil)
	removedRoot := requireXRPCRefusal(t, removedRootErr, http.StatusNotFound, "RootNotFound", "an instance-removed post thread")
	require.Equal(t, missingRoot.XRPCError, removedRoot.XRPCError)
	authorRootErr := authorView.Query(t.Context(), "social.coves.community.comment.getComments",
		url.Values{"post": {uri}}, nil)
	authorRoot := requireXRPCRefusal(t, authorRootErr, http.StatusNotFound, "RootNotFound", "the author's instance-removed post thread")
	require.Equal(t, removedRoot.XRPCError, authorRoot.XRPCError)
	require.NotContains(t, readAuthorFeedURIs("social.coves.actor.getPosts", authorFeedParams), uri,
		"the author must not find an instance-removed post in their own feed")
	require.NotContains(t, readAuthorFeedURIs("social.coves.communityFeed.getCommunity", communityFeedParams), uri,
		"the author must not find an instance-removed post in the community feed")
	require.NotContains(t, communityFeedURIs(t, p, community.DID), uri)
	search, err := queryPostSearch(p, needle, community.DID)
	require.NoError(t, err)
	require.Empty(t, search.Feed, "the removed post must not appear in search for its unique title")
	var blockedImageHeaders http.Header
	pathBlocked := func(path string) (bool, error) {
		_, err := p.AppView.GetBinary(context.Background(), path)
		if testkit.IsStatus(err, http.StatusNotFound) {
			if path == parsedImage.Path {
				var statusError *testkit.StatusError
				if errors.As(err, &statusError) {
					blockedImageHeaders = statusError.Header
				}
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}
	p.Await(t, "the removed post's cached image to return 404", func() (bool, error) {
		return pathBlocked(parsedImage.Path)
	})
	require.NotNil(t, blockedImageHeaders, "the blocked image's 404 response headers were not captured")
	require.Equal(t, "no-store", blockedImageHeaders.Get("Cache-Control"))

	editImage := author.UploadBlob(t, testkit.TestPNG(96, 96), "image/png")
	require.NotEqual(t, image.CID(), editImage.CID())
	editImagePath := strings.Replace(parsedImage.Path, image.CID(), editImage.CID(), 1)
	require.NotEqual(t, parsedImage.Path, editImagePath)
	editedTitle := title + " edited"
	editedContent := "edited while removed"
	editedCID := writePost(editedTitle, editedContent, image, editImage)
	require.NotEqual(t, createdCID, editedCID)
	// Observe the edit in the indexed post before testing the overlay or media
	// reconciliation. A still-hidden pre-edit view proves neither behavior.
	stateToken = admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	p.Await(t, "getSubjectState to report the author's edited post CID", func() (bool, error) {
		indexed, err := readState()
		if err != nil {
			return false, err
		}
		return indexed.State.CurrentSubject.CID == editedCID, nil
	})
	awaitStatus(t, p, uri, community.DID, "pending_reacceptance",
		"the author's edit to invalidate the old community acceptance")
	// The edited CID is not admitted yet, so an anonymous viewer gets notFound:
	// removal never widens access to an unadmitted CID. The author could read
	// their own pending post before the removal, so they keep a view, but only a
	// content-free #moderatedPost, even after the edit reaches the index.
	p.Holds(t, "the unadmitted edited post to read as notFound anonymously with its newly added image blocked", func() (bool, error) {
		hidden, err := notFoundAnonymously()
		if err != nil || !hidden {
			return hidden, err
		}
		return pathBlocked(editImagePath)
	})
	requireModeratedAuthorPost(title, content, editedTitle, editedContent)

	// The community account can update its acceptance at the same subject rkey,
	// so restore can be checked through the public thread rather than an
	// author-only read of a still-pending post (R3).
	community.PutRecord(t, acceptanceCollection, acceptRkey, acceptanceRecord(uri, editedCID))
	reaccepted := awaitStatus(t, p, uri, community.DID, "accepted",
		"the community to re-accept the edited CID while instance removal stands")
	removed, err = moderated()
	require.NoError(t, err)
	require.True(t, removed, "community re-acceptance must not undo instance removal")

	stateToken = admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	state, err = readState()
	require.NoError(t, err)
	require.Equal(t, editedCID, state.State.CurrentSubject.CID)
	require.Equal(t, "removed", state.State.Moderation.State)
	var restoration struct {
		Outcome string `json:"outcome"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, restoreContentMethod)).Procedure(
		t.Context(), restoreContentMethod, map[string]any{
			"actionId":        removal.Action.Action.Ref.ActionID,
			"reviewedSubject": map[string]any{"uri": uri, "cid": editedCID},
			"expectedVersion": state.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
		}, &restoration)
	require.NoError(t, err)
	require.Equal(t, "applied", restoration.Outcome)
	admissionAfterRestore, err := p.PostStatus(context.Background(), uri, community.DID)
	require.NoError(t, err)
	require.Equal(t, reaccepted, admissionAfterRestore, "instance restoration must not change the community's re-acceptance")

	p.Await(t, "the restored edited post to serve to anonymous readers", func() (bool, error) {
		post, err := readPost()
		if err != nil {
			return false, err
		}
		record, ok := post["record"].(map[string]any)
		return ok && post["uri"] == uri && post["$type"] == nil && record["title"] == editedTitle, nil
	})
	p.FreshReadQuota(t, "restored-post-thread")
	// As copies the client IP, and FreshReadQuota just replaced it.
	authorView = p.AppView.As(authorToken)
	p.Await(t, "the restored post's comment thread to serve publicly", func() (bool, error) {
		thread, err := p.Thread(context.Background(), uri, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(commentURI)
		return thread.Post.URI == uri && found && node.Comment.Record["content"] == commentText, nil
	}, withReadCadence())
	restoredAuthorPost, _, err := readAuthorPost()
	require.NoError(t, err)
	require.Equal(t, uri, restoredAuthorPost["uri"])
	require.NotContains(t, restoredAuthorPost, "$type", "restoration must serve a post view, not a moderated placeholder")
	restoredRecord, ok := restoredAuthorPost["record"].(map[string]any)
	require.True(t, ok, "restored author's post must carry a record")
	require.Equal(t, editedTitle, restoredRecord["title"])
	require.Equal(t, editedContent, restoredRecord["content"])
	require.NotContains(t, restoredAuthorPost, "moderation")
	require.Contains(t, communityFeedURIs(t, p, community.DID), uri,
		"restoration must return the re-accepted post to the community feed")
	// Positive control for the blocked-path checks above: both paths are real,
	// servable blobs, so their 404s came from the media blocks restore lifts.
	for _, path := range []string{parsedImage.Path, editImagePath} {
		p.Await(t, "the restored post's images to serve again", func() (bool, error) {
			response, err := p.AppView.GetBinary(context.Background(), path)
			if testkit.IsStatus(err, http.StatusNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return response.Status == http.StatusOK && len(response.Body) > 0 &&
				strings.HasPrefix(response.ContentType, "image/"), nil
		})
	}
}
