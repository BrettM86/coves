package moderation_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityStateAlphaDID   = "did:plc:communityalpha"
	communityStateBetaDID    = "did:plc:communitybeta"
	communityStateRemoteDID  = "did:plc:communityremote"
	communityStateRemovedDID = "did:plc:communityremoved"

	communityStateAlphaOwner     = "did:plc:alphaowner"
	communityStateAlphaModerator = "did:plc:alphamoderator"
	communityStateBetaModerator  = "did:plc:betamoderator"
	communityStateRemoteMod      = "did:plc:remotemoderator"
	communityStateRemovedMod     = "did:plc:removedmoderator"
	communityStateAdmin          = "did:plc:instanceadmin"
	communityStateNoRole         = "did:plc:norolemember"
	communityStateBannedAccount  = "did:plc:troublemaker456"

	communityStateAlphaPost     = "at://did:plc:authoralice/social.coves.community.postv2/3kabcalphapost"
	communityStateLegacyPost    = "at://did:plc:communityalpha/social.coves.community.post/3kabclegacypost"
	communityStateRemotePost    = "at://did:plc:authoralice/social.coves.community.postv2/3kabcremotepost"
	communityStateRemovedPost   = "at://did:plc:authoralice/social.coves.community.postv2/3kabcremovedpost"
	communityStateAlphaComment  = "at://did:plc:authorbob/social.coves.community.comment/3kabcalphacomment"
	communityStateUnindexedPost = "at://did:plc:authoralice/social.coves.community.postv2/3kabcunindexed"
	communityStateForeignRecord = "at://did:plc:authoralice/app.bsky.feed.post/3kabcforeign"
)

// communityStateResolver is the content-to-community index; it records the
// URIs it was asked about.
type communityStateResolver struct {
	mu          sync.Mutex
	communities map[string]string
	lookups     []string
	err         error
}

func (r *communityStateResolver) ContentCommunity(_ context.Context, contentURI string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, contentURI)
	if r.err != nil {
		return "", false, r.err
	}
	community, ok := r.communities[contentURI]
	return community, ok, nil
}

type communityStateRead struct {
	CommunityDID string
	Subject      string
	Kind         moderation.CommunitySubjectKind
}

// communityStateReader echoes the subject it was asked for at version 7 and
// records every read, so a test can prove a refusal happened before any read.
type communityStateReader struct {
	mu    sync.Mutex
	reads []communityStateRead
	err   error
}

func (r *communityStateReader) ReadCommunitySubjectState(_ context.Context, communityDID, subject string, kind moderation.CommunitySubjectKind) (*moderation.CommunityState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, communityStateRead{CommunityDID: communityDID, Subject: subject, Kind: kind})
	if r.err != nil {
		return nil, r.err
	}
	return &moderation.CommunityState{CommunityDID: communityDID, Subject: subject, Kind: kind, Version: 7}, nil
}

// Authorizer ports. The hosted checker records the communities it was asked
// about: a lookup there is the authorizer having been consulted.
type communityStateHosted struct {
	mu      sync.Mutex
	hosted  map[string]bool
	lookups []string
	err     error
}

func (h *communityStateHosted) IsHostedCommunity(_ context.Context, communityDID string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lookups = append(h.lookups, communityDID)
	if h.err != nil {
		return false, h.err
	}
	return h.hosted[communityDID], nil
}

type communityStateRemovals map[string]bool

func (r communityStateRemovals) CommunityRemoved(_ context.Context, communityDID string) (bool, error) {
	return r[communityDID], nil
}

// communityStateRoles serves rows of every community, hosted or not, so the
// authorizer itself must refuse a non-hosted community.
type communityStateRoles []communities.CommunityRole

func (r communityStateRoles) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	var out []communities.CommunityRole
	for _, row := range r {
		if row.CommunityDID == communityDID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r communityStateRoles) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	var out []communities.CommunityRole
	for _, row := range r {
		if row.SubjectDID == subjectDID {
			out = append(out, row)
		}
	}
	return out, nil
}

type communityStateAccountBans struct{}

func (communityStateAccountBans) ActiveBan(context.Context, string) (*accountbans.Ban, error) {
	return nil, nil
}

type communityStateAccountStatus struct{}

func (communityStateAccountStatus) Restriction(context.Context, string) (accountstatus.Status, bool, error) {
	return "", false, nil
}

type communityStateAdmins map[string]bool

func (a communityStateAdmins) IsInstanceAdmin(did string) bool { return a[did] }

func communityStateRole(communityDID, subjectDID, role string) communities.CommunityRole {
	return communities.CommunityRole{
		CommunityDID:    communityDID,
		SubjectDID:      subjectDID,
		Role:            role,
		RecordKey:       "rk-" + subjectDID[len("did:plc:"):],
		RecordCID:       "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy",
		RecordCreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

type communityStateFixture struct {
	service  *moderation.CommunityStateService
	resolver *communityStateResolver
	reader   *communityStateReader
	hosted   *communityStateHosted
}

// newCommunityStateFixture wires the service over the real role authorizer.
// alpha and beta are hosted; remote is indexed but not hosted; removed is
// hosted but removed by the instance.
func newCommunityStateFixture() *communityStateFixture {
	resolver := &communityStateResolver{communities: map[string]string{
		communityStateAlphaPost:    communityStateAlphaDID,
		communityStateLegacyPost:   communityStateAlphaDID,
		communityStateRemotePost:   communityStateRemoteDID,
		communityStateRemovedPost:  communityStateRemovedDID,
		communityStateAlphaComment: communityStateAlphaDID,
	}}
	reader := &communityStateReader{}
	hosted := &communityStateHosted{hosted: map[string]bool{
		communityStateAlphaDID:   true,
		communityStateBetaDID:    true,
		communityStateRemovedDID: true,
	}}
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:   hosted,
		Removals: communityStateRemovals{communityStateRemovedDID: true},
		Roles: communityStateRoles{
			communityStateRole(communityStateAlphaDID, communityStateAlphaOwner, communities.RoleOwner),
			communityStateRole(communityStateAlphaDID, communityStateAlphaModerator, communities.RoleModerator),
			communityStateRole(communityStateBetaDID, communityStateBetaModerator, communities.RoleModerator),
			communityStateRole(communityStateRemoteDID, communityStateRemoteMod, communities.RoleModerator),
			communityStateRole(communityStateRemovedDID, communityStateRemovedMod, communities.RoleModerator),
		},
		AccountBans:   communityStateAccountBans{},
		AccountStatus: communityStateAccountStatus{},
		Admins:        communityStateAdmins{communityStateAdmin: true},
		Now:           func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
	return &communityStateFixture{
		service:  moderation.NewCommunityStateService(resolver, authorizer, reader),
		resolver: resolver,
		reader:   reader,
		hosted:   hosted,
	}
}

// TestCommunityStateAuthorization covers who may read a subject's community
// state, and which community decides that: an at-URI subject's indexed
// community always replaces the request community.
func TestCommunityStateAuthorization(t *testing.T) {
	for _, test := range []struct {
		name    string
		request moderation.CommunityStateRequest
		// wantRead is the one read an allowed request makes; nil means the
		// request is refused with ErrForbidden before any read.
		wantRead *communityStateRead
	}{
		{
			name:    "caller with no role is forbidden",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateNoRole, Subject: communityStateAlphaPost},
		},
		{
			name:    "moderator of another community is forbidden",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateBetaModerator, Subject: communityStateAlphaPost},
		},
		{
			name:     "instance admin is allowed",
			request:  moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: communityStateAlphaPost},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaPost, Kind: "post"},
		},
		{
			name:     "moderator of the post's community is allowed",
			request:  moderation.CommunityStateRequest{ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaPost},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaPost, Kind: "post"},
		},
		{
			name:     "owner reads a legacy post",
			request:  moderation.CommunityStateRequest{ViewerDID: communityStateAlphaOwner, Subject: communityStateLegacyPost},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateLegacyPost, Kind: "post"},
		},
		// B8: a community this instance does not host.
		{
			name:    "instance admin is forbidden for a post in a non-hosted community",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: communityStateRemotePost},
		},
		{
			name:    "indexed moderator is forbidden for a post in a non-hosted community",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateRemoteMod, Subject: communityStateRemotePost},
		},
		{
			name:    "moderator is forbidden for a post in a removed community",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateRemovedMod, Subject: communityStateRemovedPost},
		},
		// B9: the request community disagrees with the post's indexed community.
		{
			name: "moderator of the indexed community is allowed despite another request community",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaPost, CommunityDID: communityStateBetaDID,
			},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaPost, Kind: "post"},
		},
		{
			name: "moderator of only the request community is forbidden for a post",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateBetaModerator, Subject: communityStateAlphaPost, CommunityDID: communityStateBetaDID,
			},
		},
		// B10: a comment's community comes from the resolver.
		{
			name:     "moderator of the comment's community is allowed",
			request:  moderation.CommunityStateRequest{ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaComment},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaComment, Kind: "comment"},
		},
		{
			name: "comment request community is ignored for a moderator of the resolved community",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaComment, CommunityDID: communityStateBetaDID,
			},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaComment, Kind: "comment"},
		},
		{
			name: "moderator of only the request community is forbidden for a comment",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateBetaModerator, Subject: communityStateAlphaComment, CommunityDID: communityStateBetaDID,
			},
		},
		// DID subjects are authorized in the request community.
		{
			name: "subject equal to the community reads the roster",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaDID, CommunityDID: communityStateAlphaDID,
			},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaDID, Kind: "roster"},
		},
		{
			name: "another DID reads the account",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateAlphaModerator, Subject: communityStateBannedAccount, CommunityDID: communityStateAlphaDID,
			},
			wantRead: &communityStateRead{CommunityDID: communityStateAlphaDID, Subject: communityStateBannedAccount, Kind: "account"},
		},
		{
			name: "moderator of another community is forbidden for an account",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateBetaModerator, Subject: communityStateBannedAccount, CommunityDID: communityStateAlphaDID,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCommunityStateFixture()

			state, err := fixture.service.GetCommunityState(t.Context(), test.request)

			// An at-URI subject makes exactly one community lookup; a DID
			// subject makes none.
			var wantLookups []string
			if strings.HasPrefix(test.request.Subject, "at://") {
				wantLookups = []string{test.request.Subject}
			}
			if test.wantRead == nil {
				require.ErrorIs(t, err, moderation.ErrForbidden)
				assert.Nil(t, state)
				assert.Empty(t, fixture.reader.reads, "a refused request must not read the subject's state")
				assert.Equal(t, wantLookups, fixture.resolver.lookups)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []communityStateRead{*test.wantRead}, fixture.reader.reads)
			assert.Equal(t, wantLookups, fixture.resolver.lookups)
			assert.Equal(t, &moderation.CommunityState{
				CommunityDID: test.wantRead.CommunityDID,
				Subject:      test.wantRead.Subject,
				Kind:         test.wantRead.Kind,
				Version:      7,
			}, state)
		})
	}
}

// TestCommunityStateInvalidInput covers refusals made before authorization:
// none of them consults the authorizer or reads any state.
func TestCommunityStateInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		request moderation.CommunityStateRequest
		wantErr error
		// wantLookups are the resolver lookups the refusal makes.
		wantLookups []string
	}{
		{
			name:    "no viewer",
			request: moderation.CommunityStateRequest{Subject: communityStateAlphaPost},
			wantErr: moderation.ErrAuthRequired,
		},
		{
			name:    "DID subject without a community",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: communityStateBannedAccount},
			wantErr: moderation.ErrInvalidRequest,
		},
		{
			name: "DID subject with a malformed community",
			request: moderation.CommunityStateRequest{
				ViewerDID: communityStateAdmin, Subject: communityStateBannedAccount, CommunityDID: "not-a-did",
			},
			wantErr: moderation.ErrInvalidRequest,
		},
		{
			name:    "at-URI in an unknown collection",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: communityStateForeignRecord},
			wantErr: moderation.ErrInvalidSubject,
		},
		{
			name:    "garbage subject",
			request: moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: "not-a-subject"},
			wantErr: moderation.ErrInvalidSubject,
		},
		{
			name:        "post that was never indexed",
			request:     moderation.CommunityStateRequest{ViewerDID: communityStateAdmin, Subject: communityStateUnindexedPost},
			wantErr:     moderation.ErrInvalidSubject,
			wantLookups: []string{communityStateUnindexedPost},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCommunityStateFixture()

			state, err := fixture.service.GetCommunityState(t.Context(), test.request)

			require.ErrorIs(t, err, test.wantErr)
			assert.Nil(t, state)
			assert.Equal(t, test.wantLookups, fixture.resolver.lookups)
			assert.Empty(t, fixture.hosted.lookups, "invalid input must be refused before authorization")
			assert.Empty(t, fixture.reader.reads, "invalid input must not read any state")
		})
	}
}

// TestCommunityStateDependencyFailure covers an unexpected error from the
// content resolver, the authorizer or the state reader: the read fails as
// ErrModerationUnavailable, and a failure before authorization completes
// reads no state.
func TestCommunityStateDependencyFailure(t *testing.T) {
	failure := errors.New("connection reset")
	for _, test := range []struct {
		name     string
		fail     func(*communityStateFixture)
		wantRead bool
	}{
		{
			name: "content resolver fails",
			fail: func(f *communityStateFixture) { f.resolver.err = failure },
		},
		{
			name: "authorizer fails",
			fail: func(f *communityStateFixture) { f.hosted.err = failure },
		},
		{
			name:     "state reader fails",
			fail:     func(f *communityStateFixture) { f.reader.err = failure },
			wantRead: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCommunityStateFixture()
			test.fail(fixture)

			state, err := fixture.service.GetCommunityState(t.Context(), moderation.CommunityStateRequest{
				ViewerDID: communityStateAlphaModerator, Subject: communityStateAlphaPost,
			})

			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.NotErrorIs(t, err, moderation.ErrForbidden)
			assert.Nil(t, state)
			if test.wantRead {
				assert.Equal(t, []communityStateRead{{CommunityDID: communityStateAlphaDID, Subject: communityStateAlphaPost, Kind: "post"}}, fixture.reader.reads)
			} else {
				assert.Empty(t, fixture.reader.reads, "a failure before authorization completes must not read any state")
			}
		})
	}
}
