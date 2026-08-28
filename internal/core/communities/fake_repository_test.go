package communities_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"Coves/internal/core/communities"
)

// fakeCommunityRepo is a communities.Repository that answers from memory and
// records the arguments it was handed.
//
// # WHY IT RECORDS RATHER THAN JUST ANSWERS
//
// A stub that returns nil from every method — which is what the deleted
// tests/unit/community_service_test.go used — cannot fail. The service's read
// methods are mostly a resolve step, a clamp, and a forward, and all three of
// those are only visible in what reached the repository: a limit of 5,000
// arriving as 5,000 rather than 50, a scoped identifier arriving as a raw "!"
// string rather than a DID, an offset silently dropped. So every method here
// appends its arguments to a call log and the tests assert against that.
//
// # WHY IT IS UNTAGGED
//
// This file has no build tag, so it compiles into both this package's unit
// build and its integration build; the tagged files may use it, but not the
// reverse (the phase-2 rule). It deliberately does not declare a TestMain —
// harness_test.go owns that for the integration build, and a second one would
// make the tagged binary refuse to link.
type fakeCommunityRepo struct {
	mu sync.Mutex

	byDID    map[string]*communities.Community
	byHandle map[string]*communities.Community

	// Canned answers for the read methods that have no in-memory model. A test
	// that cares about the value sets it; a test that only cares that the call
	// was forwarded leaves it nil.
	subscriptions []*communities.Subscription
	subscribers   []*communities.Subscription
	blocked       []*communities.CommunityBlock
	members       []*communities.Membership
	listResult    []*communities.Community
	searchResult  []*communities.Community
	searchTotal   int
	membership    *communities.Membership
	block         *communities.CommunityBlock
	isBlocked     bool
	subscription  *communities.Subscription

	// err, when set, is returned by every method that can fail. It is how the
	// tests prove a repository failure is propagated rather than flattened into
	// an empty result.
	err error

	calls []repoCall
}

// repoCall is one method invocation, with the arguments that decide behaviour.
type repoCall struct {
	method string
	args   []any
}

func newFakeCommunityRepo() *fakeCommunityRepo {
	return &fakeCommunityRepo{
		byDID:    map[string]*communities.Community{},
		byHandle: map[string]*communities.Community{},
	}
}

// seed adds a community addressable by both its DID and its handle.
func (f *fakeCommunityRepo) seed(community *communities.Community) *communities.Community {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byDID[community.DID] = community
	f.byHandle[community.Handle] = community
	return community
}

func (f *fakeCommunityRepo) record(method string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repoCall{method: method, args: args})
}

// callsTo returns every recorded invocation of method, in order.
func (f *fakeCommunityRepo) callsTo(method string) []repoCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repoCall
	for _, call := range f.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

// onlyCallTo returns the single recorded invocation of method, or an error
// naming what was actually recorded — which is far more useful in a failure
// than an index-out-of-range panic.
func (f *fakeCommunityRepo) onlyCallTo(method string) (repoCall, error) {
	matching := f.callsTo(method)
	if len(matching) != 1 {
		f.mu.Lock()
		defer f.mu.Unlock()
		return repoCall{}, fmt.Errorf("expected exactly one call to %s, got %d; the full log is %v",
			method, len(matching), f.calls)
	}
	return matching[0], nil
}

func (f *fakeCommunityRepo) Create(_ context.Context, community *communities.Community) (*communities.Community, error) {
	f.record("Create", community.DID)
	if f.err != nil {
		return nil, f.err
	}
	return f.seed(community), nil
}

func (f *fakeCommunityRepo) GetByDID(_ context.Context, did string) (*communities.Community, error) {
	f.record("GetByDID", did)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if community, ok := f.byDID[did]; ok {
		return community, nil
	}
	return nil, communities.ErrCommunityNotFound
}

func (f *fakeCommunityRepo) GetByHandle(_ context.Context, handle string) (*communities.Community, error) {
	f.record("GetByHandle", handle)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if community, ok := f.byHandle[handle]; ok {
		return community, nil
	}
	return nil, communities.ErrCommunityNotFound
}

// GetByNameAndOrigin mirrors the repository contract: zero matches is a miss,
// more than one is ambiguity, and it answers from the same seeded rows so a
// test can build a collision by seeding two rows with the same pair.
func (f *fakeCommunityRepo) GetByNameAndOrigin(_ context.Context, name, origin string) (*communities.Community, error) {
	f.record("GetByNameAndOrigin", name, origin)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []*communities.Community
	for _, community := range f.byDID {
		if community.Name == name && community.Origin == origin {
			matches = append(matches, community)
		}
	}
	switch len(matches) {
	case 0:
		return nil, communities.ErrCommunityNotFound
	case 1:
		return matches[0], nil
	default:
		return nil, communities.ErrAmbiguousCommunity
	}
}

func (f *fakeCommunityRepo) Update(_ context.Context, community *communities.Community) (*communities.Community, error) {
	f.record("Update", community.DID)
	if f.err != nil {
		return nil, f.err
	}
	return f.seed(community), nil
}

func (f *fakeCommunityRepo) Delete(_ context.Context, did string) error {
	f.record("Delete", did)
	return f.err
}

func (f *fakeCommunityRepo) UpdateCredentials(_ context.Context, did, accessToken, refreshToken string) error {
	f.record("UpdateCredentials", did, accessToken, refreshToken)
	return f.err
}

func (f *fakeCommunityRepo) List(_ context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	f.record("List", req)
	if f.err != nil {
		return nil, f.err
	}
	return f.listResult, nil
}

func (f *fakeCommunityRepo) Search(_ context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	f.record("Search", req)
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.searchResult, f.searchTotal, nil
}

func (f *fakeCommunityRepo) Subscribe(_ context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	f.record("Subscribe", subscription.UserDID, subscription.CommunityDID)
	if f.err != nil {
		return nil, f.err
	}
	return subscription, nil
}

func (f *fakeCommunityRepo) SubscribeWithCount(_ context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	f.record("SubscribeWithCount", subscription.UserDID, subscription.CommunityDID)
	if f.err != nil {
		return nil, f.err
	}
	return subscription, nil
}

func (f *fakeCommunityRepo) Unsubscribe(_ context.Context, userDID, communityDID string) error {
	f.record("Unsubscribe", userDID, communityDID)
	return f.err
}

func (f *fakeCommunityRepo) UnsubscribeWithCount(_ context.Context, userDID, communityDID string) error {
	f.record("UnsubscribeWithCount", userDID, communityDID)
	return f.err
}

func (f *fakeCommunityRepo) GetSubscription(_ context.Context, userDID, communityDID string) (*communities.Subscription, error) {
	f.record("GetSubscription", userDID, communityDID)
	if f.err != nil {
		return nil, f.err
	}
	if f.subscription == nil {
		return nil, communities.ErrSubscriptionNotFound
	}
	return f.subscription, nil
}

func (f *fakeCommunityRepo) GetSubscriptionByURI(_ context.Context, recordURI string) (*communities.Subscription, error) {
	f.record("GetSubscriptionByURI", recordURI)
	if f.err != nil {
		return nil, f.err
	}
	if f.subscription == nil {
		return nil, communities.ErrSubscriptionNotFound
	}
	return f.subscription, nil
}

func (f *fakeCommunityRepo) ListSubscriptions(_ context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	f.record("ListSubscriptions", userDID, limit, offset)
	if f.err != nil {
		return nil, f.err
	}
	return f.subscriptions, nil
}

func (f *fakeCommunityRepo) ListSubscribers(_ context.Context, communityDID string, limit, offset int) ([]*communities.Subscription, error) {
	f.record("ListSubscribers", communityDID, limit, offset)
	if f.err != nil {
		return nil, f.err
	}
	return f.subscribers, nil
}

func (f *fakeCommunityRepo) GetSubscribedCommunityDIDs(_ context.Context, userDID string, communityDIDs []string) (map[string]bool, error) {
	f.record("GetSubscribedCommunityDIDs", userDID, communityDIDs)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]bool{}, nil
}

func (f *fakeCommunityRepo) BlockCommunity(_ context.Context, block *communities.CommunityBlock) (*communities.CommunityBlock, error) {
	f.record("BlockCommunity", block.UserDID, block.CommunityDID)
	if f.err != nil {
		return nil, f.err
	}
	return block, nil
}

func (f *fakeCommunityRepo) UnblockCommunity(_ context.Context, userDID, communityDID string) error {
	f.record("UnblockCommunity", userDID, communityDID)
	return f.err
}

func (f *fakeCommunityRepo) GetBlock(_ context.Context, userDID, communityDID string) (*communities.CommunityBlock, error) {
	f.record("GetBlock", userDID, communityDID)
	if f.err != nil {
		return nil, f.err
	}
	if f.block == nil {
		return nil, communities.ErrBlockNotFound
	}
	return f.block, nil
}

func (f *fakeCommunityRepo) GetBlockByURI(_ context.Context, recordURI string) (*communities.CommunityBlock, error) {
	f.record("GetBlockByURI", recordURI)
	if f.err != nil {
		return nil, f.err
	}
	if f.block == nil {
		return nil, communities.ErrBlockNotFound
	}
	return f.block, nil
}

func (f *fakeCommunityRepo) ListBlockedCommunities(_ context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	f.record("ListBlockedCommunities", userDID, limit, offset)
	if f.err != nil {
		return nil, f.err
	}
	return f.blocked, nil
}

func (f *fakeCommunityRepo) IsBlocked(_ context.Context, userDID, communityDID string) (bool, error) {
	f.record("IsBlocked", userDID, communityDID)
	if f.err != nil {
		return false, f.err
	}
	return f.isBlocked, nil
}

func (f *fakeCommunityRepo) CreateMembership(_ context.Context, membership *communities.Membership) (*communities.Membership, error) {
	f.record("CreateMembership", membership.UserDID, membership.CommunityDID)
	if f.err != nil {
		return nil, f.err
	}
	return membership, nil
}

func (f *fakeCommunityRepo) GetMembership(_ context.Context, userDID, communityDID string) (*communities.Membership, error) {
	f.record("GetMembership", userDID, communityDID)
	if f.err != nil {
		return nil, f.err
	}
	if f.membership == nil {
		return nil, communities.ErrMembershipNotFound
	}
	return f.membership, nil
}

func (f *fakeCommunityRepo) UpdateMembership(_ context.Context, membership *communities.Membership) (*communities.Membership, error) {
	f.record("UpdateMembership", membership.UserDID, membership.CommunityDID)
	if f.err != nil {
		return nil, f.err
	}
	return membership, nil
}

func (f *fakeCommunityRepo) ListMembers(_ context.Context, communityDID string, limit, offset int) ([]*communities.Membership, error) {
	f.record("ListMembers", communityDID, limit, offset)
	if f.err != nil {
		return nil, f.err
	}
	return f.members, nil
}

func (f *fakeCommunityRepo) CreateModerationAction(_ context.Context, action *communities.ModerationAction) (*communities.ModerationAction, error) {
	f.record("CreateModerationAction", action.CommunityDID, action.Action)
	if f.err != nil {
		return nil, f.err
	}
	return action, nil
}

func (f *fakeCommunityRepo) ListModerationActions(_ context.Context, communityDID string, limit, offset int) ([]*communities.ModerationAction, error) {
	f.record("ListModerationActions", communityDID, limit, offset)
	if f.err != nil {
		return nil, f.err
	}
	return nil, nil
}

func (f *fakeCommunityRepo) IncrementMemberCount(_ context.Context, communityDID string) error {
	f.record("IncrementMemberCount", communityDID)
	return f.err
}

func (f *fakeCommunityRepo) DecrementMemberCount(_ context.Context, communityDID string) error {
	f.record("DecrementMemberCount", communityDID)
	return f.err
}

func (f *fakeCommunityRepo) IncrementSubscriberCount(_ context.Context, communityDID string) error {
	f.record("IncrementSubscriberCount", communityDID)
	return f.err
}

func (f *fakeCommunityRepo) DecrementSubscriberCount(_ context.Context, communityDID string) error {
	f.record("DecrementSubscriberCount", communityDID)
	return f.err
}

func (f *fakeCommunityRepo) IncrementPostCount(_ context.Context, communityDID string) error {
	f.record("IncrementPostCount", communityDID)
	return f.err
}

// errRepositoryUnavailable stands in for anything the datastore can go wrong
// with. The tests only care that it is not one of the domain's typed errors, so
// that "propagated" can be told apart from "mapped to not-found".
var errRepositoryUnavailable = errors.New("connection pool exhausted")

// compile-time proof that the fake really is the interface the service holds.
// Without it, a method added to Repository would make the fake silently unused
// rather than making this file fail to build.
var _ communities.Repository = (*fakeCommunityRepo)(nil)
