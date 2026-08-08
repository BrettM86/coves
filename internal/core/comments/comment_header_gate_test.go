package comments

import (
	"context"
	"testing"

	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The getComments THREAD HEADER is a posts read path: it serves the viewed
// post's title, content, author and admission context above the comment tree. It
// must therefore answer the same question every other posts read path answers —
// has this community admitted this post, and if not, is the caller its author?
//
// The gate used to be reached through an OPTIONAL type assertion against a
// private capability interface, with a miss falling back to the pre-admission
// header builder. Today's wiring happened to satisfy it, so nothing leaked — but
// the security property rested on convention: any future decorator around the
// post repository (metrics, caching, tracing) that implemented `posts.Repository`
// and nothing more would silently turn the gate OFF, and the in-suite fakes
// exercised precisely that unsafe path. A capability you can forget to implement
// is a capability that will be forgotten.
//
// The requirement now lives in the dependency's TYPE (PostReader), so the
// compiler refuses the wiring instead of the service degrading at runtime. These
// tests pin both halves of that: the behavior when the gate says "hidden", and
// the fact that a bare posts.Repository does not satisfy the dependency.

// headerlessPostRepo is the shape a future decorator would have: it satisfies
// posts.Repository — the embedded interface gives it every method, whatever that
// interface grows next — and NOTHING ELSE. It is the type this package must be
// unable to build a service on.
type headerlessPostRepo struct {
	posts.Repository
}

// TestCommentService_HeaderGateIsACompileTimeRequirement pins the structural half
// of the fix.
//
// The negative assertion is the load-bearing one: headerlessPostRepo is a
// complete posts.Repository, and it must NOT satisfy PostReader. Because
// PostReader is the type the constructors take, that is what makes
// `NewCommentService(..., headerlessPostRepo{}, ...)` a compile error rather than
// a service that quietly serves ungated headers. If someone widens the
// constructor back to posts.Repository to "fix a build", this pin is the note
// explaining why the build was right to fail.
func TestCommentService_HeaderGateIsACompileTimeRequirement(t *testing.T) {
	t.Parallel()

	var decorator any = headerlessPostRepo{}

	_, isRepository := decorator.(posts.Repository)
	require.True(t, isRepository,
		"the fixture must be a complete posts.Repository, or the negative assertion below proves nothing")

	_, isReader := decorator.(PostReader)
	assert.False(t, isReader,
		"a bare posts.Repository satisfied PostReader, so the visibility-aware header lookup is no longer a "+
			"REQUIREMENT of the comment service's dependency — a decorator that implements only the repository "+
			"interface can be wired again, and the thread header would serve pending, rejected and removed posts to "+
			"anyone with the permalink")

	// And the fake this package's unit tests actually use must carry the gate, so
	// no T0 test can exercise a service whose header lookup is absent.
	var fake any = newMockPostRepo()
	_, fakeIsReader := fake.(PostReader)
	assert.True(t, fakeIsReader,
		"the in-suite post-repository fake stopped implementing the visibility-aware header lookup. Every getComments "+
			"unit test would then be exercising a shape production can no longer wire")
}

// TestCommentService_GetComments_HiddenHeaderIsRootNotFound is the behavioral
// half: when the gate answers "not visible to this viewer", the endpoint answers
// root-not-found — it never falls back to a softer check.
func TestCommentService_GetComments_HiddenHeaderIsRootNotFound(t *testing.T) {
	const postURI = "at://did:plc:someauthor/social.coves.community.postv2/hiddenroot"

	newService := func(t *testing.T, hidden bool) Service {
		t.Helper()

		postRepo := newMockPostRepo()
		post := createTestPost(postURI, "did:plc:someauthor", "did:plc:somecommunity")
		require.NoError(t, postRepo.Create(context.Background(), post))
		if hidden {
			postRepo.hideFromHeader(postURI)
		}

		return NewCommentService(newMockCommentRepo(), newMockUserRepo(), postRepo, newMockCommunityRepo(), nil, nil, nil)
	}

	t.Run("a hidden root is not found, and no header is built", func(t *testing.T) {
		t.Parallel()

		resp, err := newService(t, true).GetComments(context.Background(), &GetCommentsRequest{
			PostURI: postURI,
			Sort:    "hot",
			Depth:   3,
			Limit:   10,
		})

		require.ErrorIsf(t, err, ErrRootNotFound,
			"the thread endpoint served a post the visibility gate hid. A permalink is all an attacker has to guess, "+
				"and the header carries the full title and content of a post no community admitted")
		assert.Nil(t, resp, "no response may be built for a hidden root")
	})

	t.Run("a visible root is served with its header", func(t *testing.T) {
		t.Parallel()

		resp, err := newService(t, false).GetComments(context.Background(), &GetCommentsRequest{
			PostURI: postURI,
			Sort:    "hot",
			Depth:   3,
			Limit:   10,
		})

		require.NoError(t, err)
		require.NotNil(t, resp.Post, "an admitted post must still render its thread header")
		header, ok := resp.Post.(*posts.PostView)
		require.Truef(t, ok, "the thread header must be the admission-hydrated post view, got %T", resp.Post)
		assert.Equal(t, postURI, header.URI)
	})

	t.Run("a soft-deleted root is not found", func(t *testing.T) {
		t.Parallel()

		// The gate owns the soft-delete answer too: the fake's VisibleHeaderView
		// hides a deleted row exactly as the repository's predicate does, so this
		// case cannot regress into a second, separate check in the service.
		postRepo := newMockPostRepo()
		post := createTestPost(postURI, "did:plc:someauthor", "did:plc:somecommunity")
		deletedAt := post.CreatedAt
		post.DeletedAt = &deletedAt
		require.NoError(t, postRepo.Create(context.Background(), post))

		service := NewCommentService(newMockCommentRepo(), newMockUserRepo(), postRepo, newMockCommunityRepo(), nil, nil, nil)
		_, err := service.GetComments(context.Background(), &GetCommentsRequest{
			PostURI: postURI,
			Sort:    "hot",
			Depth:   3,
			Limit:   10,
		})

		require.ErrorIs(t, err, ErrRootNotFound,
			"a soft-deleted post's thread header must not be served: the 2026-07-29 defect was exactly this row "+
				"reaching an anonymous caller in full")
	})
}
