//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Author X's own view of X's post in community C carries C's avatar. Once the
// instance removes C, X still reads the post, with status "removed", but no
// longer receives the removed community's avatar.
func TestPostRepository_AuthorViewDropsRemovedCommunityAvatar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	label := testkit.UniqueID(t)
	community := visibilityCommunity(t, db, label)
	_, err := db.ExecContext(ctx, `UPDATE communities SET avatar_cid = 'bafkreiauthorviewavatar', pds_url = 'https://pds.authorview.test'
		WHERE did = $1`, community)
	require.NoError(t, err)
	author := "did:plc:authorview" + label
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post in C", time.Now().Add(-time.Hour))
	seedVisibilityAdmission(t, db, community, post, posts.AdmissionStatusAccepted, "", "")
	repository := NewPostRepository(db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	authorView := func(t *testing.T) *posts.PostView {
		t.Helper()
		views, _, err := repository.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: author, ViewerDID: author, Limit: 10})
		require.NoError(t, err)
		require.Len(t, views, 1, "X reads X's own post")
		require.Equal(t, post, views[0].URI)
		require.NotNil(t, views[0].Community)
		return views[0]
	}

	t.Run("before removal", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusAccepted), view.Status)
		if assert.NotNil(t, view.Community.Avatar, "the live community's avatar is served") {
			assert.Equal(t, "https://pds.authorview.test/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Avis"+label+"community&cid=bafkreiauthorviewavatar",
				*view.Community.Avatar)
		}
	})
	removeNotificationCommunity(t, &unreadVisibilityFixture{db: db}, community)
	t.Run("while the community is removed", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusRemoved), view.Status, "X still reads the post, as removed")
		assert.Nil(t, view.Community.Avatar, "the removed community's avatar is not served")
	})
}
