package communities_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What subscribe does when the viewer's PDS already holds a subscription
// record for the community.
//
// The decision reads the viewer's own PDS, never only the AppView index: the
// index lags the firehose, so a subscribe repeated before the first one is
// indexed would otherwise write a second record. A supplied contentVisibility
// always rewrites the matched record in place with putRecord; an omitted one
// (0, which only the handler produces) writes nothing and reports the record
// that is there.

const otherCommunityDID = "did:plc:othercommunity0000000"

// subscriptionEntry is a subscription record as the fake PDS lists it.
func subscriptionEntry(rkey, cid string, value map[string]any) pds.RecordEntry {
	return pds.RecordEntry{
		URI:   "at://" + subscriberDID + "/social.coves.community.subscription/" + rkey,
		CID:   cid,
		Value: value,
	}
}

func subscriptionValue(subject string, createdAt, contentVisibility any) map[string]any {
	value := map[string]any{
		"$type":   "social.coves.community.subscription",
		"subject": subject,
	}
	if createdAt != nil {
		value["createdAt"] = createdAt
	}
	if contentVisibility != nil {
		value["contentVisibility"] = contentVisibility
	}
	return value
}

func TestSubscribeToCommunity_NoMatchingRecordCreates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name                  string
		contentVisibility     int
		wantContentVisibility int
	}{
		{name: "supplied value", contentVisibility: 4, wantContentVisibility: 4},
		{name: "omitted value defaults to 3", contentVisibility: 0, wantContentVisibility: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			userPDS.records = []pds.RecordEntry{
				subscriptionEntry("3kaaaaaaaaaa2", "bafyother",
					subscriptionValue(otherCommunityDID, "2026-01-02T03:04:05Z", float64(2))),
			}

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, tt.contentVisibility)
			require.NoError(t, err)

			assert.Empty(t, userPDS.putAttempts,
				"the only listed record names another community, so nothing may be rewritten")
			require.Len(t, userPDS.createAttempts, 1, "no record names this community, so exactly one is created")
			write := userPDS.created[0]
			assert.Equal(t, "social.coves.community.subscription", write.collection)
			assert.Equal(t, seededCommunityDID, write.record["subject"])
			assert.Equal(t, tt.wantContentVisibility, write.record["contentVisibility"])

			require.NotNil(t, subscription)
			assert.False(t, subscription.Existing, "a freshly created record is not an existing subscription")
			assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.subscription/"+write.rkey, subscription.RecordURI)
			assert.Equal(t, "bafycid"+write.rkey, subscription.RecordCID)
		})
	}
}

func TestSubscribeToCommunity_SuppliedValueRewritesMatchingRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name              string
		listedCreatedAt   any
		listedValue       any
		indexedValue      int // 0 = no index row
		contentVisibility int
		// wantCreatedAt is the literal createdAt the rewrite must carry; empty
		// means a fresh RFC3339 timestamp taken during the call.
		wantCreatedAt string
	}{
		{
			name:            "not yet indexed, listed createdAt preserved",
			listedCreatedAt: "2026-01-02T03:04:05Z", listedValue: float64(3),
			contentVisibility: 5, wantCreatedAt: "2026-01-02T03:04:05Z",
		},
		{
			name:            "listed record has no createdAt",
			listedCreatedAt: nil, listedValue: float64(3),
			contentVisibility: 5,
		},
		{
			name:            "listed createdAt is not a string",
			listedCreatedAt: float64(1767323045), listedValue: float64(3),
			contentVisibility: 5,
		},
		{
			name:            "index lags a previous rewrite; supplied value equals the index",
			listedCreatedAt: "2026-01-02T03:04:05Z", listedValue: float64(5), indexedValue: 3,
			contentVisibility: 3, wantCreatedAt: "2026-01-02T03:04:05Z",
		},
		{
			name:            "PDS already holds the supplied value",
			listedCreatedAt: "2026-01-02T03:04:05Z", listedValue: float64(5), indexedValue: 5,
			contentVisibility: 5, wantCreatedAt: "2026-01-02T03:04:05Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, repo, userPDS := writeForwardService(t)
			userPDS.records = []pds.RecordEntry{
				subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
					subscriptionValue(seededCommunityDID, tt.listedCreatedAt, tt.listedValue)),
			}
			if tt.indexedValue != 0 {
				repo.subscription = &communities.Subscription{
					UserDID:           subscriberDID,
					CommunityDID:      seededCommunityDID,
					RecordURI:         "at://" + subscriberDID + "/social.coves.community.subscription/3kaaaaaaaaaa2",
					RecordCID:         "bafyindexed",
					ContentVisibility: tt.indexedValue,
				}
			}

			before := time.Now().Truncate(time.Second)
			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, tt.contentVisibility)
			after := time.Now()
			require.NoError(t, err)

			require.Empty(t, userPDS.createAttempts,
				"the PDS already holds a subscription record for this community; creating another "+
					"leaves duplicate records the index cannot reconcile")
			require.Len(t, userPDS.putAttempts, 1,
				"a supplied value always rewrites the matched record, whatever the PDS or index holds")
			write := userPDS.putAttempts[0]
			assert.Equal(t, "social.coves.community.subscription", write.collection)
			assert.Equal(t, "3kaaaaaaaaaa2", write.rkey)
			assert.Equal(t, "", write.swapRecord)
			assert.Len(t, write.record, 4, "the rewrite carries exactly $type, subject, createdAt, contentVisibility")
			assert.Equal(t, "social.coves.community.subscription", write.record["$type"])
			assert.Equal(t, seededCommunityDID, write.record["subject"])
			assert.Equal(t, tt.contentVisibility, write.record["contentVisibility"])
			createdAt, isString := write.record["createdAt"].(string)
			require.Truef(t, isString, "createdAt is %T, want an RFC3339 string", write.record["createdAt"])
			if tt.wantCreatedAt != "" {
				assert.Equal(t, tt.wantCreatedAt, createdAt)
			} else {
				parsed, parseErr := time.Parse(time.RFC3339, createdAt)
				require.NoErrorf(t, parseErr, "createdAt %q is not RFC3339", createdAt)
				assert.Falsef(t, parsed.Before(before) || parsed.After(after),
					"createdAt %s is outside the call window [%s, %s]", parsed, before, after)
			}

			require.NotNil(t, subscription)
			assert.True(t, subscription.Existing)
			assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.subscription/3kaaaaaaaaaa2", subscription.RecordURI)
			assert.Equal(t, "bafyput3kaaaaaaaaaa2", subscription.RecordCID, "the result carries the rewrite's new CID")
			assert.Equal(t, seededCommunityDID, subscription.CommunityDID)
			assert.Equal(t, tt.contentVisibility, subscription.ContentVisibility)
		})
	}
}

func TestSubscribeToCommunity_RepeatBeforeIndexingRewritesFirstRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, userPDS := writeForwardService(t)

	first, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 2)
	require.NoError(t, err)
	second, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 5)
	require.NoError(t, err)

	require.Len(t, userPDS.createAttempts, 1,
		"the second subscribe must find the first record on the PDS, not create another")
	require.Len(t, userPDS.created, 1)
	createdRkey := userPDS.created[0].rkey
	require.Len(t, userPDS.putAttempts, 1)
	assert.Equal(t, createdRkey, userPDS.putAttempts[0].rkey)
	assert.Equal(t, 5, userPDS.putAttempts[0].record["contentVisibility"])

	matching := 0
	for _, record := range userPDS.records {
		if record.Value["subject"] == seededCommunityDID {
			matching++
		}
	}
	assert.Equal(t, 1, matching, "the PDS ends with exactly one subscription record for the community")

	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.False(t, first.Existing)
	assert.True(t, second.Existing)
	assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.subscription/"+createdRkey, second.RecordURI)
	assert.Equal(t, first.RecordURI, second.RecordURI)
}

func TestSubscribeToCommunity_FindsMatchAcrossPagesAndPicksGreatestRkey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The greatest matching rkey carries the OLDEST createdAt, so a choice by
	// createdAt picks the wrong record.
	smaller := subscriptionEntry("3kaaaaaaaaab2", "bafysmaller",
		subscriptionValue(seededCommunityDID, "2026-03-01T00:00:00Z", float64(3)))
	greater := subscriptionEntry("3kaaaaaaaaac2", "bafygreater",
		subscriptionValue(seededCommunityDID, "2025-01-01T00:00:00Z", float64(3)))
	otherA := subscriptionEntry("3kaaaaaaaaad2", "bafyothera",
		subscriptionValue(otherCommunityDID, "2026-01-01T00:00:00Z", float64(3)))
	otherB := subscriptionEntry("3kaaaaaaaaae2", "bafyotherb",
		subscriptionValue(otherCommunityDID, "2026-01-01T00:00:00Z", float64(3)))

	// The fake serves two records per page.
	tests := []struct {
		name    string
		records []pds.RecordEntry
	}{
		{name: "only match on the second page", records: []pds.RecordEntry{otherA, otherB, greater}},
		{name: "greater listed before smaller", records: []pds.RecordEntry{greater, smaller}},
		{name: "greater on a later page than smaller", records: []pds.RecordEntry{smaller, otherA, otherB, greater}},
		{name: "smaller on a later page than greater", records: []pds.RecordEntry{greater, otherA, otherB, smaller}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			userPDS.listPageSize = 2
			userPDS.records = append([]pds.RecordEntry(nil), tt.records...)

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 4)
			require.NoError(t, err)

			require.Empty(t, userPDS.createAttempts,
				"a matching record exists on some page; the service must follow the cursor to it")
			require.Len(t, userPDS.putAttempts, 1)
			assert.Equal(t, "3kaaaaaaaaac2", userPDS.putAttempts[0].rkey,
				"of several matching records, the one with the greatest rkey (newest TID) is rewritten")
			require.NotNil(t, subscription)
			assert.True(t, subscription.Existing)
			assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.subscription/3kaaaaaaaaac2", subscription.RecordURI)
		})
	}
}

func TestSubscribeToCommunity_OmittedValueKeepsExistingRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name                  string
		listedValue           any
		wantContentVisibility int
	}{
		{name: "listed value", listedValue: float64(2), wantContentVisibility: 2},
		{name: "listed value absent", listedValue: nil, wantContentVisibility: 3},
		{name: "listed value out of range", listedValue: float64(9), wantContentVisibility: 3},
		{name: "listed value not a number", listedValue: "x", wantContentVisibility: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			userPDS.records = []pds.RecordEntry{
				subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
					subscriptionValue(seededCommunityDID, "2026-01-02T03:04:05Z", tt.listedValue)),
			}

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 0)
			require.NoError(t, err)

			require.Empty(t, userPDS.createAttempts,
				"subscribing again without a value to a community already subscribed on the PDS writes nothing")
			require.Empty(t, userPDS.putAttempts,
				"an omitted value never rewrites the existing record")
			require.NotNil(t, subscription)
			assert.True(t, subscription.Existing)
			assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.subscription/3kaaaaaaaaaa2", subscription.RecordURI)
			assert.Equal(t, "bafylisted", subscription.RecordCID)
			assert.Equal(t, tt.wantContentVisibility, subscription.ContentVisibility)
		})
	}
}

func TestSubscribeToCommunity_ListAndRewriteFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	injectedCause := errors.New("pds is down")

	matching := subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
		subscriptionValue(seededCommunityDID, "2026-01-02T03:04:05Z", float64(3)))
	other := subscriptionEntry("3kaaaaaaaaab2", "bafyother",
		subscriptionValue(otherCommunityDID, "2026-01-02T03:04:05Z", float64(3)))

	tests := []struct {
		name             string
		arrange          func(*fakeUserPDS)
		wantUnauthorized bool
		wantPutAttempts  int
	}{
		{
			name: "listRecords auth error",
			arrange: func(f *fakeUserPDS) {
				f.records = []pds.RecordEntry{matching}
				f.listErr = pds.ErrSessionExpired
			},
			wantUnauthorized: true,
		},
		{
			name: "listRecords other error",
			arrange: func(f *fakeUserPDS) {
				f.records = []pds.RecordEntry{matching}
				f.listErr = injectedCause
			},
		},
		{
			name: "listRecords fails on a later page after an earlier page matched",
			arrange: func(f *fakeUserPDS) {
				f.records = []pds.RecordEntry{matching, other}
				f.listPageSize = 1
				f.listErrOnCall = map[int]error{1: injectedCause}
			},
		},
		{
			name: "putRecord auth error",
			arrange: func(f *fakeUserPDS) {
				f.records = []pds.RecordEntry{matching}
				f.putErr = pds.ErrUnauthorized
			},
			wantUnauthorized: true,
			wantPutAttempts:  1,
		},
		{
			name: "putRecord other error",
			arrange: func(f *fakeUserPDS) {
				f.records = []pds.RecordEntry{matching}
				f.putErr = injectedCause
			},
			wantPutAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			tt.arrange(userPDS)

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 5)

			if tt.wantUnauthorized {
				require.ErrorIs(t, err, communities.ErrUnauthorized,
					"a PDS auth failure is a dead session; the handler turns ErrUnauthorized into a 401")
			} else {
				require.ErrorIs(t, err, injectedCause, "the PDS failure must be returned, not swallowed")
				assert.NotErrorIs(t, err, communities.ErrUnauthorized)
			}
			assert.Nil(t, subscription)
			assert.Empty(t, userPDS.createAttempts,
				"a failed read or rewrite must not fall through to creating a record")
			assert.Len(t, userPDS.putAttempts, tt.wantPutAttempts, "no write is attempted after the failure")
		})
	}
}

// A PDS that answers every page with a fresh cursor would otherwise keep the
// request paging until the client gives up. The service stops at its page cap
// and fails, even though the first page matched: the scan is incomplete, and
// the newest matching rkey could be on a page it never read.
func TestSubscribeToCommunity_StopsAtPageCapWhenCursorNeverEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	service, _, userPDS := writeForwardService(t)
	userPDS.listPageSize = 1
	userPDS.endlessCursor = true
	userPDS.records = []pds.RecordEntry{
		subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
			subscriptionValue(seededCommunityDID, "2026-01-02T03:04:05Z", float64(3))),
	}

	subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 4)

	require.Error(t, err, "an unending listing must fail rather than act on a partial scan")
	assert.NotErrorIs(t, err, communities.ErrUnauthorized,
		"a runaway cursor is not a dead session")
	assert.Nil(t, subscription)
	assert.Len(t, userPDS.listCalls, 50, "the service lists at most 50 pages")
	assert.Empty(t, userPDS.createAttempts, "no record is created after an incomplete scan")
	assert.Empty(t, userPDS.putAttempts, "no record is rewritten after an incomplete scan")
}
