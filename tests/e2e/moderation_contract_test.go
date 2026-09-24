//go:build e2e

package e2e

import (
	"context"
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
