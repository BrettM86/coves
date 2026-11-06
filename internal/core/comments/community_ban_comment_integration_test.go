//go:build integration

package comments_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// recordingPDS is a pds.Client that records the creates the comment service
// asks for. Only CreateRecord does anything; every other method fails, because
// the create path has no business calling them.
type recordingPDS struct {
	did     string
	created []string // collection of each CreateRecord call, in order
}

func (r *recordingPDS) CreateRecord(_ context.Context, collection, rkey string, _ any) (string, string, error) {
	r.created = append(r.created, collection)
	return "at://" + r.did + "/" + collection + "/" + rkey, "bafyrecorded" + rkey, nil
}

func (r *recordingPDS) DeleteRecord(context.Context, string, string) error {
	return fmt.Errorf("recordingPDS: DeleteRecord is not part of the create path")
}

func (r *recordingPDS) ListRecords(context.Context, string, int, string) (*pds.ListRecordsResponse, error) {
	return nil, fmt.Errorf("recordingPDS: ListRecords is not part of the create path")
}

func (r *recordingPDS) GetRecord(context.Context, string, string) (*pds.RecordResponse, error) {
	return nil, fmt.Errorf("recordingPDS: GetRecord is not part of the create path")
}

func (r *recordingPDS) PutRecord(context.Context, string, string, any, string) (string, string, error) {
	return "", "", fmt.Errorf("recordingPDS: PutRecord is not part of the create path")
}

func (r *recordingPDS) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	return nil, fmt.Errorf("recordingPDS: UploadBlob is not part of the create path")
}

func (r *recordingPDS) DID() string     { return r.did }
func (r *recordingPDS) HostURL() string { return "https://pds.invalid" }

// TestCreateComment_CommunityBan is the comment write path's ban gate wired to
// the real ban index: a commenter banned in the community of the thread's root
// post is refused with comments.ErrBanned before anything reaches their PDS. A
// ban in another community, or one that has lapsed, creates the comment. The
// community comes from the parent chain however deep it is, and a misleading
// client-supplied root cannot override it; the root is consulted only to
// refuse, when the parent is not indexed yet.
func TestCreateComment_CommunityBan(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityDID, err := fixtures.Community(ctx, db, "bancommentc", "bancommentowner")
	require.NoError(t, err)
	otherCommunityDID, err := fixtures.Community(ctx, db, "bancommentc2", "bancommentowner")
	require.NoError(t, err)
	authorDID := fixtures.DID("bancommentauthor")
	rootURI := fixtures.Post(t, db, communityDID, authorDID, "a post to reply to", 0, time.Now())
	otherRootURI := fixtures.Post(t, db, otherCommunityDID, authorDID, "a post in another community", 0, time.Now())
	unindexedURI := "at://" + authorDID + "/social.coves.community.comment/" + testkit.TID()

	// A two-level thread under rootURI: depthTwoURI replies to depthOneURI,
	// which replies to the post. A reply to depthTwoURI resolves its community
	// only by walking the whole parent chain.
	commentRepo := postgres.NewCommentRepository(db)
	indexComment := func(parentURI string) string {
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/social.coves.community.comment/" + rkey
		require.NoError(t, commentRepo.Create(ctx, &comments.Comment{
			URI:          uri,
			CID:          "bafybancomment" + rkey,
			RKey:         rkey,
			CommenterDID: authorDID,
			RootURI:      rootURI,
			RootCID:      "bafytest",
			ParentURI:    parentURI,
			ParentCID:    "bafytest",
			Content:      "a comment in the thread",
			CreatedAt:    time.Now(),
		}))
		return uri
	}
	depthOneURI := indexComment(rootURI)
	depthTwoURI := indexComment(depthOneURI)

	bans := postgres.NewCommunityBanRepository(db)
	expiredAt := time.Now().Add(-time.Hour).UTC()

	rows := []struct {
		name         string
		commenterDID string
		parentURI    string
		replyRootURI string
		banCommunity string
		banExpiresAt *time.Time
		wantBanned   bool
	}{
		{"active ban in the root post's community refuses the comment", fixtures.DID("bancommentactive"), rootURI, rootURI, communityDID, nil, true},
		{"a ban only in another community creates the comment", fixtures.DID("bancommentother"), rootURI, rootURI, otherCommunityDID, nil, false},
		{"a ban that expired an hour ago creates the comment", fixtures.DID("bancommentexpired"), rootURI, rootURI, communityDID, &expiredAt, false},
		{"active ban refuses a reply to a reply", fixtures.DID("bancommentdeep"), depthTwoURI, rootURI, communityDID, nil, true},
		{"active ban refuses a reply to a reply whose root names another community's post", fixtures.DID("bancommentdeepoth"), depthTwoURI, otherRootURI, communityDID, nil, true},
		{"active ban refuses a reply to a reply whose root is not indexed", fixtures.DID("bancommentdeepunx"), depthTwoURI, unindexedURI, communityDID, nil, true},
		{"active ban refuses a reply whose parent is not indexed and whose root is in the banned community", fixtures.DID("bancommentunxpar"), unindexedURI, rootURI, communityDID, nil, true},
		{"a reply whose parent is not indexed and whose root is in another community is created", fixtures.DID("bancommentunxoth"), unindexedURI, otherRootURI, communityDID, nil, false},
		{"a reply whose parent and root are both not indexed is created", fixtures.DID("bancommentunxall"), unindexedURI, unindexedURI, communityDID, nil, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.NoError(t, bans.UpsertCommunityBan(ctx, communities.CommunityBan{
				CommunityDID: row.banCommunity,
				SubjectDID:   row.commenterDID,
				RecordKey:    communities.BanRecordKey(row.commenterDID),
				RecordCID:    "bafybancommentrecord",
				Reason:       "spam",
				ExpiresAt:    row.banExpiresAt,
			}))

			recorder := &recordingPDS{did: row.commenterDID}
			service := comments.NewCommentServiceWithPDSFactory(
				postgres.NewCommentRepository(db), nil, postgres.NewPostRepository(db), nil, nil,
				func(context.Context, *oauthlib.ClientSessionData) (pds.Client, error) { return recorder, nil },
				comments.WithCommunityBans(bans),
			)
			commenter, err := syntax.ParseDID(row.commenterDID)
			require.NoError(t, err)

			_, err = service.CreateComment(ctx, &oauthlib.ClientSessionData{AccountDID: commenter, AccessToken: "test-access-token"},
				comments.CreateCommentRequest{
					Reply: comments.ReplyRef{
						Root:   comments.StrongRef{URI: row.replyRootURI, CID: "bafytest"},
						Parent: comments.StrongRef{URI: row.parentURI, CID: "bafytest"},
					},
					Content: "a reply in the banned community",
				})

			if row.wantBanned {
				require.ErrorIs(t, err, comments.ErrBanned)
				require.Empty(t, recorder.created, "a refused comment must not write to the commenter's PDS")
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"social.coves.community.comment"}, recorder.created,
				"an admitted comment must be written to the commenter's PDS")
		})
	}
}

// TestCreateComment_AccountBan is the comment write path's instance account
// ban gate wired to the Postgres account-ban read, with the ban written by the
// real moderation service: a banned account is refused with comments.ErrAccountBanned
// before anything reaches its PDS — including on a reply whose parent and root
// are not indexed, which the community-ban gate admits. Lifting the ban, or a
// temporary ban running out, creates the comment.
func TestCreateComment_AccountBan(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityDID, err := fixtures.Community(ctx, db, "accountbancommc", "accountbancommowner")
	require.NoError(t, err)
	rootURI := fixtures.Post(t, db, communityDID, fixtures.DID("accountbancommauthor"), "a post to reply to", 0, time.Now())
	unindexedURI := "at://" + fixtures.DID("accountbancommauthor") + "/social.coves.community.comment/" + testkit.TID()

	moderator := accountbanfixture.New(db)
	banUntilTomorrow := func(t *testing.T, accountDID string) {
		expiresAt := time.Now().Add(24 * time.Hour).UTC()
		moderator.Ban(t, accountDID, &expiresAt)
	}
	rows := []struct {
		name         string
		commenterDID string
		parentURI    string
		setup        func(t *testing.T, accountDID string)
		wantBanned   bool
	}{
		{"a permanent account ban refuses the comment", fixtures.DID("acctbancommperm"), rootURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
		}, true},
		{"a temporary account ban in force refuses the comment", fixtures.DID("acctbancommtemp"), rootURI, banUntilTomorrow, true},
		{"an account ban refuses a reply whose parent and root are not indexed", fixtures.DID("acctbancommunx"), unindexedURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
		}, true},
		{"an unbanned account creates the comment", fixtures.DID("acctbancommunban"), rootURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
			moderator.Unban(t, accountDID)
		}, false},
		{"a lapsed temporary ban creates the comment with no unban", fixtures.DID("acctbancommlapse"), rootURI, func(t *testing.T, accountDID string) {
			banUntilTomorrow(t, accountDID)
			moderator.Lapse(t, accountDID)
		}, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.setup(t, row.commenterDID)

			recorder := &recordingPDS{did: row.commenterDID}
			service := comments.NewCommentServiceWithPDSFactory(
				postgres.NewCommentRepository(db), nil, postgres.NewPostRepository(db), nil, nil,
				func(context.Context, *oauthlib.ClientSessionData) (pds.Client, error) { return recorder, nil },
				comments.WithCommunityBans(postgres.NewCommunityBanRepository(db)),
				comments.WithAccountBans(moderator.Reader()),
			)
			commenter, err := syntax.ParseDID(row.commenterDID)
			require.NoError(t, err)

			_, err = service.CreateComment(ctx, &oauthlib.ClientSessionData{AccountDID: commenter, AccessToken: "test-access-token"},
				comments.CreateCommentRequest{
					Reply: comments.ReplyRef{
						Root:   comments.StrongRef{URI: row.parentURI, CID: "bafytest"},
						Parent: comments.StrongRef{URI: row.parentURI, CID: "bafytest"},
					},
					Content: "a reply from an account under an instance ban decision",
				})

			if row.wantBanned {
				require.ErrorIs(t, err, comments.ErrAccountBanned)
				require.Empty(t, recorder.created, "a refused comment must not write to the commenter's PDS")
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"social.coves.community.comment"}, recorder.created,
				"an admitted comment must be written to the commenter's PDS")
		})
	}
}

// TestCreateComment_CommunityRemoval is the comment write path's community
// removal gate wired to the Postgres removal reader, with the removal written
// by the real moderation service: a comment on a post in a removed community,
// and a reply whose indexed parent is in the removed community while the
// client-supplied root names a post in a live community, are refused with
// comments.ErrCommunityRemoved before anything reaches the commenter's PDS.
// After restore the plain comment is created.
func TestCreateComment_CommunityRemoval(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	removedCommunityDID, err := fixtures.Community(ctx, db, "removalcommentc", "removalcommentowner")
	require.NoError(t, err)
	liveCommunityDID, err := fixtures.Community(ctx, db, "removalcommentlive", "removalcommentowner")
	require.NoError(t, err)
	authorDID := fixtures.DID("removalcommentauthor")
	removedPostURI := fixtures.Post(t, db, removedCommunityDID, authorDID, "a post in the removed community", 0, time.Now())
	livePostURI := fixtures.Post(t, db, liveCommunityDID, authorDID, "a post in a live community", 0, time.Now())

	parentRKey := testkit.TID()
	parentURI := "at://" + authorDID + "/social.coves.community.comment/" + parentRKey
	require.NoError(t, postgres.NewCommentRepository(db).Create(ctx, &comments.Comment{
		URI:          parentURI,
		CID:          "bafyremovalcomment" + parentRKey,
		RKey:         parentRKey,
		CommenterDID: authorDID,
		RootURI:      removedPostURI,
		RootCID:      "bafytest",
		ParentURI:    removedPostURI,
		ParentCID:    "bafytest",
		Content:      "a comment in the removed community",
		CreatedAt:    time.Now(),
	}))

	commenterDID := fixtures.DID("removalcommenter")
	create := func(t *testing.T, rootURI, parentURI string) (*recordingPDS, error) {
		t.Helper()
		recorder := &recordingPDS{did: commenterDID}
		service := comments.NewCommentServiceWithPDSFactory(
			postgres.NewCommentRepository(db), nil, postgres.NewPostRepository(db), nil, nil,
			func(context.Context, *oauthlib.ClientSessionData) (pds.Client, error) { return recorder, nil },
			comments.WithCommunityBans(postgres.NewCommunityBanRepository(db)),
			comments.WithCommunityRemovals(postgres.NewCommunityRemovalReader(db, fixtures.InstanceDID())),
		)
		commenter, err := syntax.ParseDID(commenterDID)
		require.NoError(t, err)
		_, err = service.CreateComment(ctx, &oauthlib.ClientSessionData{AccountDID: commenter, AccessToken: "test-access-token"},
			comments.CreateCommentRequest{
				Reply: comments.ReplyRef{
					Root:   comments.StrongRef{URI: rootURI, CID: "bafytest"},
					Parent: comments.StrongRef{URI: parentURI, CID: "bafytest"},
				},
				Content: "a comment in a community under an instance removal decision",
			})
		return recorder, err
	}

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, removedCommunityDID)
	for _, row := range []struct {
		name               string
		rootURI, parentURI string
	}{
		{"a comment on a post in the removed community is refused", removedPostURI, removedPostURI},
		{"a reply whose indexed parent is in the removed community is refused although its root names a live community's post", livePostURI, parentURI},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder, err := create(t, row.rootURI, row.parentURI)
			require.ErrorIs(t, err, comments.ErrCommunityRemoved)
			require.Empty(t, recorder.created, "a refused comment must not write to the commenter's PDS")
		})
	}

	moderator.Restore(t, removedCommunityDID)
	t.Run("after restore a comment on the post is created", func(t *testing.T) {
		recorder, err := create(t, removedPostURI, removedPostURI)
		require.NoError(t, err)
		require.Equal(t, []string{"social.coves.community.comment"}, recorder.created,
			"an admitted comment must be written to the commenter's PDS")
	})
}
