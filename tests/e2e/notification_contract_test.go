//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Direct PDS writes prove that replies, mentions and votes reach the recipient's
// notification endpoints through the running consumers. Each positive is awaited
// through listNotifications; the self-upvote negative is bounded by the same
// repo's served vote stats, then held to catch a late notification.

type notificationContractRow struct {
	Reason   string `json:"reason"`
	SortAt   string `json:"sortAt"`
	IsRead   bool   `json:"isRead"`
	RootPost struct {
		URI string `json:"uri"`
	} `json:"rootPost"`
	Subject struct {
		URI string `json:"uri"`
	} `json:"subject"`
	Record struct {
		URI string `json:"uri"`
	} `json:"record"`
	Author struct {
		DID string `json:"did"`
	} `json:"author"`
	UpvoteCount    int `json:"upvoteCount"`
	RecentUpvoters []struct {
		DID string `json:"did"`
	} `json:"recentUpvoters"`
}

type notificationContractPage struct {
	Notifications []notificationContractRow `json:"notifications"`
	SeenAt        string                    `json:"seenAt"`
}

func notificationContractList(viewer *testkit.AppView) (notificationContractPage, error) {
	var page notificationContractPage
	err := viewer.Query(context.Background(), "social.coves.notification.listNotifications",
		url.Values{"limit": {"100"}}, &page)
	return page, err
}

func notificationContractUnread(viewer *testkit.AppView) (int, error) {
	var unread struct {
		Count int `json:"count"`
	}
	err := viewer.Query(context.Background(), "social.coves.notification.getUnreadCount", nil, &unread)
	return unread.Count, err
}

// The probe deliberately reads only the list; each phase checks unread once the
// matching row has arrived, rather than spending the read quota inside Await.
func awaitNotificationContractRow(t *testing.T, p *pipeline, viewer *testkit.AppView,
	description string, matches func(notificationContractRow) bool) notificationContractRow {
	t.Helper()
	var found notificationContractRow
	p.Await(t, description, func() (bool, error) {
		page, err := notificationContractList(viewer)
		if err != nil {
			return false, err
		}
		for _, row := range page.Notifications {
			if matches(row) {
				found = row
				return true, nil
			}
		}
		return false, nil
	})
	return found
}

type notificationContractKey struct {
	reason, recordURI, subjectURI string
}

func notificationContractRows(page notificationContractPage) map[notificationContractKey]int {
	rows := make(map[notificationContractKey]int, len(page.Notifications))
	for _, row := range page.Notifications {
		rows[notificationContractKey{row.Reason, row.Record.URI, row.Subject.URI}]++
	}
	return rows
}

// Use the original sortAt spelling for updateSeen, even though time parsing is
// necessary to select the newest row (string ordering is not time ordering).
func notificationContractSeen(t *testing.T, viewer *testkit.AppView) {
	t.Helper()
	before, err := notificationContractList(viewer)
	require.NoError(t, err, "the recipient must be able to list the rows being marked seen")
	require.NotEmpty(t, before.Notifications, "there must be a notification to mark seen")

	var latest time.Time
	var watermark string
	for _, row := range before.Notifications {
		instant, parseErr := time.Parse(time.RFC3339Nano, row.SortAt)
		require.NoErrorf(t, parseErr, "notification sortAt %q must be a timestamp", row.SortAt)
		if watermark == "" || instant.After(latest) {
			latest, watermark = instant, row.SortAt
		}
	}
	require.NoError(t, viewer.Procedure(context.Background(), "social.coves.notification.updateSeen",
		map[string]string{"seenAt": watermark}, nil),
		"updateSeen must accept the newest listed sortAt verbatim")
	unread, err := notificationContractUnread(viewer)
	require.NoError(t, err, "getUnreadCount must answer after updateSeen")
	require.Equal(t, 0, unread, "marking the newest listed row seen must clear unread notifications")

	after, err := notificationContractList(viewer)
	require.NoError(t, err, "the recipient must be able to re-list after updateSeen")
	require.Equal(t, notificationContractRows(before), notificationContractRows(after),
		"updateSeen must keep the same rows, identified by reason and record/subject URI")
	require.Equal(t, watermark, after.SeenAt, "the list must echo the exact sortAt sent to updateSeen")
	for _, row := range after.Notifications {
		require.Truef(t, row.IsRead, "notification %s (%s, %s) must be read after updateSeen",
			row.Reason, row.Record.URI, row.Subject.URI)
	}
}

func notificationContractMentionFacet(content, handle, did string) []map[string]any {
	mention := "@" + handle
	start := strings.Index(content, mention)
	return []map[string]any{{
		"index":    map[string]any{"byteStart": start, "byteEnd": start + len(mention)},
		"features": []map[string]any{{"$type": "social.coves.richtext.facet#mention", "did": did}},
	}}
}

func notificationContractMentionPost(communityDID, handle, did string) map[string]any {
	content := "A post mentioning @" + handle
	record := postV2Record(communityDID, "Mention in a post", content)
	record["facets"] = notificationContractMentionFacet(content, handle, did)
	return record
}

// Each full-length wait below gets a rate-limit bucket of its own. A healthy
// posts-lane wait runs about 24s under make ci; at contractPollInterval, a
// bucket holding two or three of those plus a phase's reads can pass 100
// requests in one window and fail as a 429 on a healthy run. One wait plus its
// reads always fits (contractPollInterval). A signed-in viewer copies the
// client IP it was made from, so it is rebuilt after every rotation.
func notificationContractQuota(t *testing.T, p *pipeline, token, reason string) *testkit.AppView {
	t.Helper()
	p.FreshReadQuota(t, reason)
	return p.AppView.As(token)
}

// notificationContractAcceptedPost is indexedPost with a fresh bucket before
// each of its two status waits. reason must be unique within the test, because
// the bucket is derived from it.
func notificationContractAcceptedPost(t *testing.T, p *pipeline, community provisionedCommunity,
	author *testkit.Account, record map[string]any, reason, description string) strongRef {
	t.Helper()
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	written := author.PutRecord(t, postV2Collection, rkey, record)
	p.FreshReadQuota(t, reason+"-pending")
	awaitStatus(t, p, uri, community.DID, "pending", description+" to reach the admission queue")
	community.PutRecord(t, acceptanceCollection, subjectRkey(uri), acceptanceRecord(uri, written.CID))
	p.FreshReadQuota(t, reason+"-accepted")
	awaitStatus(t, p, uri, community.DID, "accepted", description+" to be accepted")
	return strongRef{URI: uri, CID: written.CID}
}

func TestNotificationContract_Replies(t *testing.T) {
	p := newPipeline(t)
	a := p.IndexedAccount(t, "nra")
	b := p.IndexedAccount(t, "nrb")
	community := indexedCommunity(t, p, "nr", b.DID)
	token := p.AppView.SignIn(t, b)

	post := notificationContractAcceptedPost(t, p, community, b,
		postV2Record(community.DID, "reply to B's post", "a post to hang comments on"),
		"b-post", "B's post")
	viewer := notificationContractQuota(t, p, token, "post-reply")
	firstRkey := testkit.TID()
	firstURI := commentURI(a.DID, firstRkey)
	a.PutRecord(t, commentCollection, firstRkey, commentRecord(post, post, "reply to B's post"))
	row := awaitNotificationContractRow(t, p, viewer, "A's comment to notify the post author B", func(row notificationContractRow) bool {
		return row.Reason == "postReply" && row.Record.URI == firstURI
	})
	require.Equal(t, post.URI, row.Subject.URI, "postReply must name the replied-to post")
	require.Equal(t, a.DID, row.Author.DID, "postReply must attribute A's direct PDS write")
	unread, err := notificationContractUnread(viewer)
	require.NoError(t, err)
	require.Equal(t, 1, unread, "B's first post reply must be unread")
	notificationContractSeen(t, viewer)

	// A owns the root post; B owns its indexed parent comment. A's reply
	// therefore distinguishes the parent-comment recipient from the root author.
	aPost := notificationContractAcceptedPost(t, p, community, a,
		postV2Record(community.DID, "A's post for B's parent comment", "a post to hang comments on"),
		"a-post", "A's post")
	parentRkey := testkit.TID()
	parentURI := commentURI(b.DID, parentRkey)
	parentRecord := b.PutRecord(t, commentCollection, parentRkey,
		commentRecord(aPost, aPost, "B's parent comment"))
	p.FreshReadQuota(t, "parent-comment")
	p.Await(t, "B's parent comment to be indexed before A replies", func() (bool, error) {
		thread, err := p.Thread(context.Background(), aPost.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		_, found := thread.find(parentURI)
		return found, nil
	}, withReadCadence())

	viewer = notificationContractQuota(t, p, token, "comment-reply")
	replyRkey := testkit.TID()
	replyURI := commentURI(a.DID, replyRkey)
	a.PutRecord(t, commentCollection, replyRkey,
		commentRecord(aPost, strongRef{URI: parentURI, CID: parentRecord.CID}, "A replies to B"))
	row = awaitNotificationContractRow(t, p, viewer, "A's reply to notify parent commenter B", func(row notificationContractRow) bool {
		return row.Reason == "commentReply" && row.Record.URI == replyURI
	})
	require.Equal(t, parentURI, row.Subject.URI, "commentReply must name B's parent comment rather than A's root post")
	unread, err = notificationContractUnread(viewer)
	require.NoError(t, err)
	require.Equal(t, 1, unread, "only the new comment reply must be unread after the prior watermark")
	notificationContractSeen(t, viewer)
}

func TestNotificationContract_Mentions(t *testing.T) {
	p := newPipeline(t)
	a := p.IndexedAccount(t, "nma")
	b := p.IndexedAccount(t, "nmb")
	community := indexedCommunity(t, p, "nm", a.DID)
	token := p.AppView.SignIn(t, b)

	post := notificationContractAcceptedPost(t, p, community, a,
		postV2Record(community.DID, "A's post for a comment mention", "a post to hang comments on"),
		"a-post", "A's post")
	viewer := notificationContractQuota(t, p, token, "comment-mention")
	content := "Hello @" + b.Handle
	comment := commentRecord(post, post, content)
	comment["facets"] = notificationContractMentionFacet(content, b.Handle, b.DID)
	commentRkey := testkit.TID()
	mentionURI := commentURI(a.DID, commentRkey)
	a.PutRecord(t, commentCollection, commentRkey, comment)
	row := awaitNotificationContractRow(t, p, viewer, "A's comment mention to notify B", func(row notificationContractRow) bool {
		return row.Reason == "mention" && row.Record.URI == mentionURI
	})
	require.Equal(t, mentionURI, row.Record.URI, "the comment mention must refer to A's written comment")
	unread, err := notificationContractUnread(viewer)
	require.NoError(t, err)
	require.Equal(t, 1, unread, "B's comment mention must be unread")
	notificationContractSeen(t, viewer)

	postURI := notificationContractAcceptedPost(t, p, community, a,
		notificationContractMentionPost(community.DID, b.Handle, b.DID),
		"mention-post", "the mention post").URI
	viewer = notificationContractQuota(t, p, token, "post-mention")
	row = awaitNotificationContractRow(t, p, viewer, "A's accepted post mention to notify B", func(row notificationContractRow) bool {
		return row.Reason == "mention" && row.Record.URI == postURI
	})
	require.Equal(t, postURI, row.RootPost.URI, "a post mention must have its post as the root")
	unread, err = notificationContractUnread(viewer)
	require.NoError(t, err)
	require.Equal(t, 1, unread, "only the new post mention must be unread after the prior watermark")
	notificationContractSeen(t, viewer)
}

func TestNotificationContract_Upvotes(t *testing.T) {
	p := newPipeline(t)
	a := p.IndexedAccount(t, "nua")
	b := p.IndexedAccount(t, "nub")
	community := indexedCommunity(t, p, "nu", b.DID)
	token := p.AppView.SignIn(t, b)

	first := notificationContractAcceptedPost(t, p, community, b,
		postV2Record(community.DID, "B's first vote target", "a post to hang comments on"),
		"first-post", "B's first post")
	viewer := notificationContractQuota(t, p, token, "other-upvote")
	a.PutRecord(t, voteCollection, testkit.TID(), voteRecord(first, "up"))
	row := awaitNotificationContractRow(t, p, viewer, "A's upvote to notify B", func(row notificationContractRow) bool {
		return row.Reason == "upvote" && row.Subject.URI == first.URI
	})
	require.Equal(t, 1, row.UpvoteCount, "the group must count A's one upvote")
	require.Len(t, row.RecentUpvoters, 1, "the group must show its one recent voter")
	require.Equal(t, a.DID, row.RecentUpvoters[0].DID, "the upvote group must identify voter A")
	unread, err := notificationContractUnread(viewer)
	require.NoError(t, err)
	require.Equal(t, 1, unread, "A's upvote must be unread by B")
	notificationContractSeen(t, viewer)

	second := notificationContractAcceptedPost(t, p, community, b,
		postV2Record(community.DID, "B's self-vote target", "a post to hang comments on"),
		"second-post", "B's second post")
	viewer = notificationContractQuota(t, p, token, "self-upvote")
	b.PutRecord(t, voteCollection, testkit.TID(), voteRecord(second, "up"))
	awaitStats(t, p, second.URI, "B's self-upvote to reach the served post stats before checking its notification",
		func(stats postStats) bool { return stats.Upvotes == 1 })
	p.Holds(t, "B's self-upvote to remain absent from notifications after its vote was indexed", func() (bool, error) {
		page, err := notificationContractList(viewer)
		if err != nil {
			return false, err
		}
		for _, row := range page.Notifications {
			if row.Reason == "upvote" && row.Subject.URI == second.URI {
				return false, nil
			}
		}
		unread, err := notificationContractUnread(viewer)
		if err != nil {
			return false, err
		}
		return unread == 0, nil
	})
}
