//go:build integration

package posts_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Everything CreatePost decides before it writes anything.
//
// # WHAT THIS COVERS THAT service_writeforward_test.go DOES NOT
//
// Its neighbour proves where an accepted post lands. This one is about the
// posts that never get that far: the three spellings of a community identifier
// the service must all resolve to the same DID, and the seven request shapes it
// must refuse. Both halves need a real database — resolution is a lookup in the
// communities table, and "community not found" is only meaningful against a
// table that could have contained it.
//
// # WHY THE AUTHOR HAS NO REPOSITORY HERE
//
// The service is deliberately wired with NO author-repo factory, so every
// request that survives validation stops at the same place: opening the author's
// repository, the step immediately before the record is written, which answers
// ErrNoAuthorCredentials because there is nothing to authenticate as the author
// with. That is the assertion — reaching the write is what proves nothing
// earlier rejected the post — and it costs neither a provisioned PDS account nor
// a real repo write. A post that must actually arrive somewhere is
// service_writeforward_test.go's job, where both repos are provisioned for real.
//
// Before task 6 the same trick was played on the COMMUNITY's credentials, which
// the write path no longer touches: a post is written to its author's repo now
// (§4.2 step 3), so an unusable community token stops nothing.
func TestService_CreateResolvesTheCommunityAndValidatesTheRequest(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	pdsURL := testkit.Endpoints().PDS.BaseURL

	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, pdsURL, nil, "")
	// The instance domain is load-bearing here and nowhere else in this file:
	// the scoped identifier !name@domain is only resolved for communities this
	// instance hosts, so the service's notion of "here" has to match the domain
	// in the community's handle.
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, pdsURL, instanceDID, instanceDomain, nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))

	authorDID := fixtures.DID("postauthor")
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    authorDID,
		Handle: "postauthor.test",
		PDSURL: pdsURL,
	})
	require.NoError(t, err, "creating the post's author")

	community := &communities.Community{
		DID:         fixtures.DID("testcommunity"),
		Handle:      fmt.Sprintf("c-testcommunity.%s", instanceDomain),
		Name:        "testcommunity",
		DisplayName: "Test Community",
		Description: "A community for testing posts",
		Visibility:  "public",
		// CreatedByDID is the author so the post is not also testing membership.
		CreatedByDID: authorDID,
		HostedByDID:  instanceDID,
		PDSURL:       pdsURL,
		// Unparseable on purpose: see the file comment.
		PDSAccessToken: "fake_token_for_test",
	}
	_, err = communityRepo.Create(ctx, community)
	require.NoError(t, err, "seeding the target community")

	// createPost sends a request as the author. The DID goes into the context as
	// well as the request body because the service cross-checks the two.
	createPost := func(req posts.CreatePostRequest) error {
		// No session: this service has no author-repo factory either, so the two
		// agree about what the author cannot be authenticated as.
		_, err := postService.CreatePost(middleware.SetTestUserDID(ctx, authorDID), nil, req)
		return err
	}

	// reachedTheWrite asserts that the request was accepted by everything up to
	// the PDS write and failed only on the community's deliberately broken
	// credentials.
	reachedTheWrite := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err, "there are no credentials for the author's repo, so the write must fail")
		assert.ErrorIsf(t, err, posts.ErrNoAuthorCredentials,
			"the post should have been rejected by nothing before the author-repo write; got: %v", err)
	}

	title := "Test Post Title"

	t.Run("Community addressed by DID", func(t *testing.T) {
		content := "This is a test post"
		reachedTheWrite(t, createPost(posts.CreatePostRequest{
			Community: community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: authorDID,
		}))
	})

	t.Run("Community addressed by its canonical handle", func(t *testing.T) {
		// The canonical atProto form, c-{name}.{instance}, with no ! prefix.
		content := "Post using handle instead of DID"
		reachedTheWrite(t, createPost(posts.CreatePostRequest{
			Community: community.Handle,
			Title:     &title,
			Content:   &content,
			AuthorDID: authorDID,
		}))
	})

	t.Run("Community addressed by the scoped shorthand", func(t *testing.T) {
		// !name@instance is Coves' own UX shorthand for the canonical handle,
		// and the service has to fold it back into c-{name}.{instance} before it
		// can look anything up. Derived from the row rather than written out, so
		// the two cannot drift apart.
		label, domain, found := strings.Cut(community.Handle, ".")
		require.True(t, found, "the community handle should carry a domain")
		scoped := fmt.Sprintf("!%s@%s", strings.TrimPrefix(label, "c-"), domain)

		content := "Post using !-prefixed handle"
		reachedTheWrite(t, createPost(posts.CreatePostRequest{
			Community: scoped,
			Title:     &title,
			Content:   &content,
			AuthorDID: authorDID,
		}))
	})

	t.Run("Rejects a missing community", func(t *testing.T) {
		content := "Post without community"
		err := createPost(posts.CreatePostRequest{
			Community: "",
			Content:   &content,
			AuthorDID: authorDID,
		})
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err), "got %v", err)
	})

	t.Run("Rejects a well-formed handle for a community that does not exist", func(t *testing.T) {
		content := "Post with non-existent handle"
		err := createPost(posts.CreatePostRequest{
			Community: fmt.Sprintf("c-nonexistent.%s", instanceDomain),
			Content:   &content,
			AuthorDID: authorDID,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "community not found")
	})

	t.Run("Rejects a missing author", func(t *testing.T) {
		content := "Post without author"
		err := createPost(posts.CreatePostRequest{
			Community: community.DID,
			Content:   &content,
			AuthorDID: "",
		})
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err), "got %v", err)
		assert.Contains(t, err.Error(), "authorDid", "the error should name the field at fault")
	})

	t.Run("Rejects a community DID that is not indexed", func(t *testing.T) {
		content := "Post in fake community"
		err := createPost(posts.CreatePostRequest{
			Community: "did:plc:nonexistent",
			Content:   &content,
			AuthorDID: authorDID,
		})
		require.Error(t, err)
		// The sentinel itself, not a wrapper: the handler maps this exact value
		// onto a 404.
		assert.Equal(t, posts.ErrCommunityNotFound, err)
	})

	t.Run("Rejects content over the length limit", func(t *testing.T) {
		// One byte past maxContentLength (100,000). The limit is what keeps a
		// single record from being unbounded on the way to the PDS.
		longContent := string(make([]byte, 100001))
		err := createPost(posts.CreatePostRequest{
			Community: community.DID,
			Content:   &longContent,
			AuthorDID: authorDID,
		})
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err), "got %v", err)
		assert.Contains(t, err.Error(), "too long")
	})

	t.Run("Rejects an unknown self-label", func(t *testing.T) {
		// Self-labels drive client-side content warnings, so an unrecognised one
		// has to be refused rather than stored: a label nothing renders is a
		// warning that silently does not appear.
		content := "Post with invalid label"
		err := createPost(posts.CreatePostRequest{
			Community: community.DID,
			Content:   &content,
			Labels:    &posts.SelfLabels{Values: []posts.SelfLabel{{Val: "invalid_label"}}},
			AuthorDID: authorDID,
		})
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err), "got %v", err)
		assert.Contains(t, err.Error(), "unknown content label")
	})

	t.Run("Accepts the known self-labels", func(t *testing.T) {
		content := "Post with valid labels"
		reachedTheWrite(t, createPost(posts.CreatePostRequest{
			Community: community.DID,
			Content:   &content,
			Labels: &posts.SelfLabels{Values: []posts.SelfLabel{
				{Val: "nsfw"},
				{Val: "spoiler"},
			}},
			AuthorDID: authorDID,
		}))
	})
}
