package adminreports_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/communities"
)

const (
	queueAlphaDID     = "did:plc:queuealpha"
	queueBetaDID      = "did:plc:queuebeta"
	queueRemovedDID   = "did:plc:queueremoved"
	queueAlphaOwner   = "did:plc:alphaowner"
	queueAlphaMod     = "did:plc:alphamod"
	queueBetaMod      = "did:plc:betamod"
	queueRemovedMod   = "did:plc:removedmod"
	queueAdmin        = "did:plc:instanceadmin"
	queueNoRole       = "did:plc:norole"
	queueSubjectURI   = "at://did:plc:author/social.coves.community.post/3kqueue"
	queueIdempotency  = "dismiss-key-1"
	queueRepoCursor   = "repo-next-cursor"
	queueRepoSubject  = "at://did:plc:author/social.coves.community.post/3kentry"
	queueRepoDismissN = 4
)

// queueRepository is an in-memory queue: it serves one stored entry for every
// community and stores each dismissal it applies.
type queueRepository struct {
	entries    []adminreports.CommunityQueueEntry
	listLimits []int
	dismissals []adminreports.DismissCommunitySubjectRequest
}

func newQueueRepository() *queueRepository {
	return &queueRepository{entries: []adminreports.CommunityQueueEntry{{
		SubjectURI:      queueRepoSubject,
		SubjectType:     adminreports.TargetTypePost,
		Reasons:         []adminreports.Reason{adminreports.ReasonHarassment, adminreports.ReasonSpam},
		ReportCount:     4,
		FirstReportedAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC),
		LastReportedAt:  time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC),
	}}}
}

func (r *queueRepository) ListCommunityQueue(_ context.Context, _ string, limit int, _ string) (adminreports.CommunityQueuePage, error) {
	r.listLimits = append(r.listLimits, limit)
	return adminreports.CommunityQueuePage{Entries: r.entries, Cursor: queueRepoCursor}, nil
}

func (r *queueRepository) DismissCommunitySubject(_ context.Context, req adminreports.DismissCommunitySubjectRequest) (adminreports.DismissResult, error) {
	r.dismissals = append(r.dismissals, req)
	return adminreports.DismissResult{DismissedCount: queueRepoDismissN}, nil
}

// Authorizer ports for the real communities.NewRoleAuthorizer.
type queueHosted map[string]bool

func (h queueHosted) IsHostedCommunity(_ context.Context, communityDID string) (bool, error) {
	return h[communityDID], nil
}

type queueRemovals map[string]bool

func (r queueRemovals) CommunityRemoved(_ context.Context, communityDID string) (bool, error) {
	return r[communityDID], nil
}

type queueRoles []communities.CommunityRole

func (r queueRoles) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	var out []communities.CommunityRole
	for _, row := range r {
		if row.CommunityDID == communityDID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r queueRoles) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	var out []communities.CommunityRole
	for _, row := range r {
		if row.SubjectDID == subjectDID {
			out = append(out, row)
		}
	}
	return out, nil
}

type queueAccountBans struct{}

func (queueAccountBans) ActiveBan(context.Context, string) (*accountbans.Ban, error) { return nil, nil }

type queueAccountStatus struct{}

func (queueAccountStatus) Restriction(context.Context, string) (accountstatus.Status, bool, error) {
	return "", false, nil
}

type queueAdmins map[string]bool

func (a queueAdmins) IsInstanceAdmin(did string) bool { return a[did] }

func queueRole(communityDID, subjectDID, role string) communities.CommunityRole {
	return communities.CommunityRole{
		CommunityDID:    communityDID,
		SubjectDID:      subjectDID,
		Role:            role,
		RecordKey:       "rk-" + subjectDID[len("did:plc:"):],
		RecordCID:       "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy",
		RecordCreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

// newQueueService wires the service over the real role authorizer: alpha and
// beta are hosted; removed is hosted but removed by the instance.
func newQueueService() (*adminreports.CommunityQueueService, *queueRepository) {
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:   queueHosted{queueAlphaDID: true, queueBetaDID: true, queueRemovedDID: true},
		Removals: queueRemovals{queueRemovedDID: true},
		Roles: queueRoles{
			queueRole(queueAlphaDID, queueAlphaOwner, communities.RoleOwner),
			queueRole(queueAlphaDID, queueAlphaMod, communities.RoleModerator),
			queueRole(queueBetaDID, queueBetaMod, communities.RoleModerator),
			queueRole(queueRemovedDID, queueRemovedMod, communities.RoleModerator),
		},
		AccountBans:   queueAccountBans{},
		AccountStatus: queueAccountStatus{},
		Admins:        queueAdmins{queueAdmin: true},
		Now:           func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
	})
	repo := newQueueRepository()
	return adminreports.NewCommunityQueueService(repo, authorizer), repo
}

// TestCommunityQueueAuthorization covers who may read and dismiss a
// community's queue. A refused caller sees none of the stored entries and
// dismisses nothing.
func TestCommunityQueueAuthorization(t *testing.T) {
	for _, test := range []struct {
		name      string
		community string
		caller    string
		allowed   bool
	}{
		{name: "caller with no role is forbidden", community: queueAlphaDID, caller: queueNoRole},
		{name: "moderator of another community is forbidden", community: queueAlphaDID, caller: queueBetaMod},
		{name: "moderator of the community is allowed", community: queueAlphaDID, caller: queueAlphaMod, allowed: true},
		{name: "owner of the community is allowed", community: queueAlphaDID, caller: queueAlphaOwner, allowed: true},
		{name: "instance admin is allowed", community: queueAlphaDID, caller: queueAdmin, allowed: true},
		{name: "moderator of a removed community is forbidden", community: queueRemovedDID, caller: queueRemovedMod},
	} {
		t.Run(test.name+"/list", func(t *testing.T) {
			service, _ := newQueueService()
			page, err := service.ListQueue(context.Background(), adminreports.ListCommunityQueueRequest{
				CommunityDID: test.community,
				CallerDID:    test.caller,
			})
			if !test.allowed {
				require.ErrorIs(t, err, adminreports.ErrCommunityQueueForbidden)
				assert.Empty(t, page.Entries, "a refused caller must see no queue entries")
				return
			}
			require.NoError(t, err)
			require.Len(t, page.Entries, 1, "an allowed caller must see the stored entry")
			assert.Equal(t, queueRepoSubject, page.Entries[0].SubjectURI)
			assert.Equal(t, queueRepoCursor, page.Cursor)
		})
		t.Run(test.name+"/dismiss", func(t *testing.T) {
			service, repo := newQueueService()
			result, err := service.Dismiss(context.Background(), adminreports.DismissCommunityReportsRequest{
				CommunityDID:   test.community,
				CallerDID:      test.caller,
				SubjectURI:     queueSubjectURI,
				IdempotencyKey: queueIdempotency,
			})
			if !test.allowed {
				require.ErrorIs(t, err, adminreports.ErrCommunityQueueForbidden)
				assert.Empty(t, repo.dismissals, "a refused caller must dismiss nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, adminreports.DismissResult{DismissedCount: 4}, result)
			assert.Equal(t, []adminreports.DismissCommunitySubjectRequest{{
				CommunityDID:   test.community,
				SubjectURI:     queueSubjectURI,
				ModeratorDID:   test.caller,
				IdempotencyKey: queueIdempotency,
			}}, repo.dismissals, "an allowed caller's dismissal must be applied as that caller")
		})
	}
}

// TestCommunityQueueInvalidRequest covers malformed requests: each is refused
// with ErrInvalidQueueRequest and reaches no stored state.
func TestCommunityQueueInvalidRequest(t *testing.T) {
	validList := func() adminreports.ListCommunityQueueRequest {
		return adminreports.ListCommunityQueueRequest{CommunityDID: queueAlphaDID, CallerDID: queueAlphaMod}
	}
	validDismiss := func() adminreports.DismissCommunityReportsRequest {
		return adminreports.DismissCommunityReportsRequest{
			CommunityDID: queueAlphaDID, CallerDID: queueAlphaMod,
			SubjectURI: queueSubjectURI, IdempotencyKey: queueIdempotency,
		}
	}
	listCases := []struct {
		name   string
		mutate func(*adminreports.ListCommunityQueueRequest)
	}{
		{name: "community is not a DID", mutate: func(r *adminreports.ListCommunityQueueRequest) { r.CommunityDID = "queuealpha" }},
		{name: "negative limit", mutate: func(r *adminreports.ListCommunityQueueRequest) { r.Limit = -1 }},
		{name: "limit above 100", mutate: func(r *adminreports.ListCommunityQueueRequest) { r.Limit = 101 }},
	}
	for _, test := range listCases {
		t.Run("list/"+test.name, func(t *testing.T) {
			service, repo := newQueueService()
			request := validList()
			test.mutate(&request)
			page, err := service.ListQueue(context.Background(), request)
			require.ErrorIs(t, err, adminreports.ErrInvalidQueueRequest)
			assert.Empty(t, page.Entries)
			assert.Empty(t, repo.listLimits, "an invalid request must not read the queue")
		})
	}
	dismissCases := []struct {
		name   string
		mutate func(*adminreports.DismissCommunityReportsRequest)
	}{
		{name: "community is not a DID", mutate: func(r *adminreports.DismissCommunityReportsRequest) { r.CommunityDID = "queuealpha" }},
		{name: "subject is not an AT-URI", mutate: func(r *adminreports.DismissCommunityReportsRequest) {
			r.SubjectURI = "https://example.com/post/3kqueue"
		}},
		{name: "empty idempotency key", mutate: func(r *adminreports.DismissCommunityReportsRequest) { r.IdempotencyKey = "" }},
		{name: "129-byte idempotency key", mutate: func(r *adminreports.DismissCommunityReportsRequest) {
			r.IdempotencyKey = strings.Repeat("k", 129)
		}},
	}
	for _, test := range dismissCases {
		t.Run("dismiss/"+test.name, func(t *testing.T) {
			service, repo := newQueueService()
			request := validDismiss()
			test.mutate(&request)
			result, err := service.Dismiss(context.Background(), request)
			require.ErrorIs(t, err, adminreports.ErrInvalidQueueRequest)
			assert.Zero(t, result.DismissedCount)
			assert.Empty(t, repo.dismissals, "an invalid request must dismiss nothing")
		})
	}
}

// TestCommunityQueueLimitBounds covers the accepted limit range: zero means
// the default of 50, and 1 and 100 are passed through.
func TestCommunityQueueLimitBounds(t *testing.T) {
	for _, test := range []struct {
		limit     int
		wantLimit int
	}{
		{limit: 0, wantLimit: 50},
		{limit: 1, wantLimit: 1},
		{limit: 100, wantLimit: 100},
	} {
		service, repo := newQueueService()
		_, err := service.ListQueue(context.Background(), adminreports.ListCommunityQueueRequest{
			CommunityDID: queueAlphaDID, CallerDID: queueAlphaMod, Limit: test.limit,
		})
		require.NoError(t, err, "limit %d", test.limit)
		assert.Equal(t, []int{test.wantLimit}, repo.listLimits, "limit %d must read the queue with limit %d", test.limit, test.wantLimit)
	}
}

// TestCommunityQueueIdempotencyKeyAtLimit: a 128-byte key is accepted.
func TestCommunityQueueIdempotencyKeyAtLimit(t *testing.T) {
	service, repo := newQueueService()
	key := strings.Repeat("k", 128)
	_, err := service.Dismiss(context.Background(), adminreports.DismissCommunityReportsRequest{
		CommunityDID: queueAlphaDID, CallerDID: queueAlphaMod, SubjectURI: queueSubjectURI, IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.Len(t, repo.dismissals, 1, "a 128-byte key must be accepted")
	assert.Equal(t, key, repo.dismissals[0].IdempotencyKey)
}
