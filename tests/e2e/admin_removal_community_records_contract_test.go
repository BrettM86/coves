//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// communityRecordView is a record read back from a community's repo, or its
// absence.
type communityRecordView struct {
	Present bool
	CID     string
	Value   map[string]any
}

// readCommunityRecord reads one record from a community's repo through the
// PDS's public com.atproto.repo.getRecord. The community here is hosted by the
// AppView, so the test holds no session on it; any account's client can read
// a public repo.
func readCommunityRecord(reader *testkit.Account, communityDID, collection, rkey string) (communityRecordView, error) {
	var response struct {
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	err := reader.XRPC().Query(context.Background(), "com.atproto.repo.getRecord", url.Values{
		"repo": {communityDID}, "collection": {collection}, "rkey": {rkey},
	}, &response)
	if testkit.IsNotFound(err) {
		return communityRecordView{}, nil
	}
	if err != nil {
		return communityRecordView{}, err
	}
	return communityRecordView{Present: true, CID: response.CID, Value: response.Value}, nil
}

// recordSubject is the strongRef a community acceptance or removal names.
func recordSubject(value map[string]any) (uri, cid string) {
	subject, _ := value["subject"].(map[string]any)
	uri, _ = subject["uri"].(string)
	cid, _ = subject["cid"].(string)
	return uri, cid
}

// TestAdminRemovalWritesHostedCommunityRecordsContract follows an instance
// removal and restore of an accepted post in a community this AppView hosts
// (created through social.coves.community.create, so the AppView holds its
// PDS credentials) into the community's own repo:
//
//	remove  → the community's repo holds a social.coves.community.removal for
//	          the post (subject = the reviewed strongRef, code spam, no
//	          reason) and no acceptance; post.get serves #moderatedPost with
//	          the instance as its source; the post leaves the community feed
//	restore → the removal record is gone and an acceptance pinning the post's
//	          current CID is back; post.get serves the post; it is back in the
//	          community feed
//
// An app that follows acceptances and ignores Coves labels sees exactly what
// Coves shows only because the records change; the label alone would leave
// the acceptance standing (docs/PRD_ADMIN_REMOVAL_COMMUNITY_RECORDS.md).
func TestAdminRemovalWritesHostedCommunityRecordsContract(t *testing.T) {
	p := newPipeline(t)
	creator := p.IndexedAccount(t, "arcreator")
	creatorView := p.AppView.As(p.AppView.SignIn(t, creator))
	member := p.IndexedAccount(t, "armember")
	memberView := p.AppView.As(p.AppView.SignIn(t, member))
	admin := testkit.ModerationAdmin(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, creatorView.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "ar"), "description": "a community this AppView hosts",
	}, &created), "the creator creates a community through the AppView, which stores its PDS credentials")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID

	var post struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, memberView.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post an admin will remove " + testkit.UniqueID(t),
		"content":   "a post in a hosted community",
	}, &post), "a second account posts in the hosted community")
	require.NotEmpty(t, post.URI)
	require.NotEmpty(t, post.CID)
	recordKey := subjectRkey(post.URI)

	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the member's post")
	p.Await(t, "the community's repo to hold the acceptance pinning the post", func() (bool, error) {
		acceptance, err := readCommunityRecord(member, communityDID, acceptanceCollection, recordKey)
		if err != nil || !acceptance.Present {
			return false, err
		}
		uri, cid := recordSubject(acceptance.Value)
		return uri == post.URI && cid == post.CID, nil
	})
	p.Await(t, "the accepted post to be in the community feed", func() (bool, error) {
		return slices.Contains(communityFeedURIs(t, p, communityDID), post.URI), nil
	})

	stateToken := admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
	readState := func() moderationSubjectStateResponse {
		t.Helper()
		var state moderationSubjectStateResponse
		require.NoError(t, p.AppView.As(stateToken).Query(context.Background(), subjectStateMethod,
			url.Values{"subject": {post.URI}}, &state))
		return state
	}
	state := readState()
	require.Equal(t, post.CID, state.State.CurrentSubject.CID)

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
	require.NoError(t, p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
		t.Context(), removeContentMethod, map[string]any{
			"subject":         map[string]any{"uri": post.URI, "cid": post.CID},
			"expectedVersion": state.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonSpam",
		}, &removal), "the instance admin removes the post")
	require.Equal(t, "applied", removal.Outcome)
	require.NotEmpty(t, removal.Action.Action.Ref.ActionID)

	p.Await(t, "the community's repo to hold the instance's removal record and no acceptance", func() (bool, error) {
		removalRecord, err := readCommunityRecord(member, communityDID, removalCollection, recordKey)
		if err != nil || !removalRecord.Present {
			return false, err
		}
		acceptance, err := readCommunityRecord(member, communityDID, acceptanceCollection, recordKey)
		if err != nil || acceptance.Present {
			return false, err
		}
		uri, cid := recordSubject(removalRecord.Value)
		if uri != post.URI || cid != post.CID {
			return false, fmt.Errorf("the removal record names %s@%s, want the reviewed %s@%s", uri, cid, post.URI, post.CID)
		}
		if removalRecord.Value["code"] != "spam" {
			return false, fmt.Errorf("the removal record carries code %v, want spam for reasonSpam", removalRecord.Value["code"])
		}
		if _, hasReason := removalRecord.Value["reason"]; hasReason {
			return false, fmt.Errorf("the removal record carries a reason %v; instance removals publish none", removalRecord.Value["reason"])
		}
		return true, nil
	})

	readPost := func(viewer *testkit.AppView) (map[string]any, error) {
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		if err := viewer.Query(context.Background(), "social.coves.community.post.get",
			url.Values{"uris": {post.URI}}, &response); err != nil {
			return nil, err
		}
		if len(response.Posts) != 1 {
			return nil, fmt.Errorf("post.get returned %d union members for one URI", len(response.Posts))
		}
		return response.Posts[0], nil
	}
	// The removal record's echo makes the admission row removed; the instance
	// decision must still be what post.get serves.
	p.Await(t, "the community's removal echo to be indexed", func() (bool, error) {
		status, err := p.PostStatus(context.Background(), post.URI, communityDID)
		if err != nil {
			return false, err
		}
		return status.Status == "removed", nil
	})
	removedView, err := readPost(memberView)
	require.NoError(t, err)
	require.Equal(t, "social.coves.community.post.defs#moderatedPost", removedView["$type"],
		"while the instance decision is active Coves serves #moderatedPost, not the community's #removedPost")
	moderation, ok := removedView["moderation"].(map[string]any)
	require.True(t, ok, "the author's moderated post carries moderation details: %#v", removedView)
	sources, ok := moderation["sources"].([]any)
	require.True(t, ok, "moderation sources must be an array: %#v", moderation)
	require.Len(t, sources, 1, "the instance is the only moderation source")
	source, ok := sources[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, communityInstanceDID, source["authorityDid"])
	scope, ok := source["scope"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "instance", scope["kind"])
	p.Await(t, "the removed post to leave the community feed", func() (bool, error) {
		return !slices.Contains(communityFeedURIs(t, p, communityDID), post.URI), nil
	})

	state = readState()
	require.Equal(t, "removed", state.State.Moderation.State)
	var restoration struct {
		Outcome string `json:"outcome"`
	}
	require.NoError(t, p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, restoreContentMethod)).Procedure(
		t.Context(), restoreContentMethod, map[string]any{
			"actionId":        removal.Action.Action.Ref.ActionID,
			"reviewedSubject": map[string]any{"uri": post.URI, "cid": state.State.CurrentSubject.CID},
			"expectedVersion": state.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
		}, &restoration), "the instance admin restores the post")
	require.Equal(t, "applied", restoration.Outcome)

	p.Await(t, "the community's repo to drop the removal record and hold an acceptance of the current CID", func() (bool, error) {
		removalRecord, err := readCommunityRecord(member, communityDID, removalCollection, recordKey)
		if err != nil || removalRecord.Present {
			return false, err
		}
		acceptance, err := readCommunityRecord(member, communityDID, acceptanceCollection, recordKey)
		if err != nil || !acceptance.Present {
			return false, err
		}
		uri, cid := recordSubject(acceptance.Value)
		if uri != post.URI || cid != post.CID {
			return false, fmt.Errorf("the restored acceptance names %s@%s, want the current %s@%s", uri, cid, post.URI, post.CID)
		}
		return true, nil
	})
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the restored acceptance's echo to make the post accepted again")
	p.Await(t, "post.get to serve the restored post", func() (bool, error) {
		view, err := readPost(p.AppView)
		if err != nil {
			return false, err
		}
		if view["$type"] == "social.coves.community.post.defs#moderatedPost" ||
			view["removed"] == true || view["notFound"] == true {
			return false, nil
		}
		return view["uri"] == post.URI && view["cid"] == post.CID && view["record"] != nil, nil
	})
	p.Await(t, "the restored post to be back in the community feed", func() (bool, error) {
		return slices.Contains(communityFeedURIs(t, p, communityDID), post.URI), nil
	})
}
