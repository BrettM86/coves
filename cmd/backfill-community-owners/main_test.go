package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderatorCollection = "social.coves.community.moderator"
	backfillInstanceDID = "did:web:coves.example"
)

// ownerRecordWrite is one PutRecord the fake repo received.
type ownerRecordWrite struct {
	collection string
	rkey       string
	record     string // JSON
	swap       string
}

type storedRecord struct {
	rkey  string
	value map[string]any
}

// fakeOwnerRepo is one community's repo over memory. ListRecords pages through
// the moderator records pageSize at a time; PutRecord stores, so a second run
// sees the first run's writes.
type fakeOwnerRepo struct {
	did      string
	pageSize int
	records  []storedRecord
	writes   []ownerRecordWrite
	// stuckCursor, when set, is the cursor of every page, so a reader that
	// follows it sees the same cursor again.
	stuckCursor string
}

func (r *fakeOwnerRepo) ListRecords(_ context.Context, collection string, _ int, cursor string) (*pds.ListRecordsResponse, error) {
	if collection != moderatorCollection {
		return &pds.ListRecordsResponse{}, nil
	}
	start := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, fmt.Errorf("bad cursor %q", cursor)
		}
		start = parsed
	}
	end := min(start+r.pageSize, len(r.records))
	out := &pds.ListRecordsResponse{}
	for _, record := range r.records[start:end] {
		out.Records = append(out.Records, pds.RecordEntry{
			URI: "at://" + r.did + "/" + collection + "/" + record.rkey, CID: "bafy" + record.rkey, Value: record.value,
		})
	}
	if end < len(r.records) {
		out.Cursor = strconv.Itoa(end)
	}
	if r.stuckCursor != "" {
		out.Cursor = r.stuckCursor
	}
	return out, nil
}

func (r *fakeOwnerRepo) PutRecord(_ context.Context, collection, rkey string, record any, swapRecord string) (string, string, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", "", err
	}
	r.writes = append(r.writes, ownerRecordWrite{collection: collection, rkey: rkey, record: string(encoded), swap: swapRecord})
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", "", err
	}
	if collection == moderatorCollection {
		r.records = append(r.records, storedRecord{rkey: rkey, value: value})
	}
	return "at://" + r.did + "/" + collection + "/" + rkey, "bafyput" + rkey, nil
}

func moderatorRecord(subject, role string) map[string]any {
	return map[string]any{
		"$type": moderatorCollection, "subject": subject, "role": role, "createdAt": "2026-10-01T00:00:00Z",
	}
}

func ownerRecordWithCreatedAt(subject, createdAt string) map[string]any {
	record := moderatorRecord(subject, "owner")
	record["createdAt"] = createdAt
	return record
}

func ownerRecordWithoutCreatedAt(subject string) map[string]any {
	record := moderatorRecord(subject, "owner")
	delete(record, "createdAt")
	return record
}

type backfillFixture struct {
	hosted      []string
	communities map[string]*communities.Community
	repos       map[string]*fakeOwnerRepo
	openErrors  map[string]error
	users       map[string]*users.User
	erased      map[string]bool
}

func newBackfillFixture() *backfillFixture {
	return &backfillFixture{
		communities: map[string]*communities.Community{},
		repos:       map[string]*fakeOwnerRepo{},
		openErrors:  map[string]error{},
		users:       map[string]*users.User{},
		erased:      map[string]bool{},
	}
}

// addCommunity adds a hosted community created by createdBy whose repo holds
// records.
func (f *backfillFixture) addCommunity(did, createdBy string, pageSize int, records ...storedRecord) *fakeOwnerRepo {
	f.hosted = append(f.hosted, did)
	f.communities[did] = &communities.Community{DID: did, CreatedByDID: createdBy, HostedByDID: backfillInstanceDID}
	repo := &fakeOwnerRepo{did: did, pageSize: pageSize, records: records}
	f.repos[did] = repo
	return repo
}

func (f *backfillFixture) addUser(did string) { f.users[did] = &users.User{DID: did} }

func (f *backfillFixture) HostedCommunityDIDs(context.Context) ([]string, error) {
	return append([]string(nil), f.hosted...), nil
}

func (f *backfillFixture) GetByDID(_ context.Context, did string) (*communities.Community, error) {
	if community, ok := f.communities[did]; ok {
		return community, nil
	}
	return nil, communities.ErrCommunityNotFound
}

func (f *backfillFixture) IsAccountDeleted(_ context.Context, did string) (bool, error) {
	return f.erased[did], nil
}

type backfillUsers map[string]*users.User

func (u backfillUsers) GetByDID(_ context.Context, did string) (*users.User, error) {
	if user, ok := u[did]; ok {
		return user, nil
	}
	return nil, users.ErrUserNotFound
}

func (f *backfillFixture) deps() ownerBackfillDeps {
	return ownerBackfillDeps{
		Hosted:      f,
		Communities: f,
		OpenRepo: func(_ context.Context, community *communities.Community) (ownerRepoClient, error) {
			if err := f.openErrors[community.DID]; err != nil {
				return nil, err
			}
			repo, ok := f.repos[community.DID]
			if !ok {
				return nil, fmt.Errorf("no fake repo for %s", community.DID)
			}
			return repo, nil
		},
		Erased: f,
		Users:  backfillUsers(f.users),
		Now:    func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	}
}

func TestBackfillOwners_WritesMissingOwnersOnceAcrossRuns(t *testing.T) {
	const (
		unownedCommunity = "did:plc:backfillcommunityone"
		ownedCommunity   = "did:plc:backfillcommunitytwo"
		creatorOne       = "did:plc:backfillcreatorone"
		creatorTwo       = "did:plc:backfillcreatortwo"
	)
	f := newBackfillFixture()
	f.addUser(creatorOne)
	f.addUser(creatorTwo)
	unowned := f.addCommunity(unownedCommunity, creatorOne, 1,
		storedRecord{rkey: "moderatorrecordone", value: moderatorRecord("did:plc:backfillmoderator", "moderator")})
	// The owner record, for someone other than createdBy (an earlier transfer),
	// is on the second page at its subject's derived record key.
	owned := f.addCommunity(ownedCommunity, creatorTwo, 1,
		storedRecord{rkey: "moderatorrecordtwo", value: moderatorRecord("did:plc:backfillmoderator", "moderator")},
		storedRecord{
			rkey:  communities.RoleRecordKey("did:plc:backfillsomeoneelse"),
			value: moderatorRecord("did:plc:backfillsomeoneelse", "owner"),
		})

	first, err := backfillOwners(context.Background(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, ownerBackfillReport{Written: 1, AlreadyOwned: 1}, first)
	require.Len(t, unowned.writes, 1)
	assert.Equal(t, moderatorCollection, unowned.writes[0].collection)
	assert.Equal(t, "cfnimk5xgwekzkagkk6a6vmcu7kmmpnoda7kmktdmy27mbium4aq", unowned.writes[0].rkey) // RoleRecordKey(creatorOne)
	assert.JSONEq(t, `{"$type":"social.coves.community.moderator","subject":"did:plc:backfillcreatorone",`+
		`"role":"owner","createdAt":"2026-11-12T12:00:00Z"}`, unowned.writes[0].record)
	assert.Empty(t, owned.writes, "a community whose repo holds an owner record gets no write")

	second, err := backfillOwners(context.Background(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, ownerBackfillReport{AlreadyOwned: 2}, second)
	assert.Len(t, unowned.writes, 1, "a second run writes nothing")
	assert.Empty(t, owned.writes)
}

func TestBackfillOwners_OwnerEligibility(t *testing.T) {
	const community = "did:plc:backfilleligibility"
	for _, test := range []struct {
		name       string
		createdBy  string
		indexed    bool
		erased     bool
		wantRkey   string // "" when no owner record may be written
		wantReport ownerBackfillReport
	}{
		{name: "empty createdBy", createdBy: "", wantReport: ownerBackfillReport{Skipped: 1}},
		{
			name: "erased createdBy", createdBy: "did:plc:backfillerased", indexed: true, erased: true,
			wantReport: ownerBackfillReport{Skipped: 1},
		},
		{
			name: "createdBy that is not an indexed account", createdBy: "did:plc:backfillnotanaccount",
			wantReport: ownerBackfillReport{Skipped: 1},
		},
		{
			name: "console-created community whose createdBy is an indexed admin account", createdBy: "did:plc:backfilladmin",
			indexed: true, wantRkey: "n5agxjonzp7myx6smlfvgmsmwbqdyhf2asrpvsukxnwaikarfu4q", // RoleRecordKey("did:plc:backfilladmin")
			wantReport: ownerBackfillReport{Written: 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBackfillFixture()
			repo := f.addCommunity(community, test.createdBy, 50)
			if test.indexed {
				f.addUser(test.createdBy)
			}
			f.erased[test.createdBy] = test.erased

			report, err := backfillOwners(context.Background(), f.deps())

			require.NoError(t, err)
			assert.Equal(t, test.wantReport, report)
			if test.wantRkey == "" {
				assert.Empty(t, repo.writes)
				return
			}
			require.Len(t, repo.writes, 1)
			assert.Equal(t, moderatorCollection, repo.writes[0].collection)
			assert.Equal(t, test.wantRkey, repo.writes[0].rkey)
			assert.JSONEq(t, `{"$type":"social.coves.community.moderator","subject":"`+test.createdBy+`",`+
				`"role":"owner","createdAt":"2026-11-12T12:00:00Z"}`, repo.writes[0].record)
		})
	}
}

func TestBackfillOwners_OneFailingCommunityDoesNotStopTheOthers(t *testing.T) {
	const (
		failingCommunity = "did:plc:backfillfailing"
		healthyCommunity = "did:plc:backfillhealthy"
		creatorOne       = "did:plc:backfillcreatorone"
	)
	f := newBackfillFixture()
	f.addUser(creatorOne)
	f.addCommunity(failingCommunity, creatorOne, 50)
	healthy := f.addCommunity(healthyCommunity, creatorOne, 50)
	errSessionRefused := errors.New("community session refused")
	f.openErrors[failingCommunity] = errSessionRefused

	report, err := backfillOwners(context.Background(), f.deps())

	require.Error(t, err, "a run with a failed community must not report success")
	assert.Equal(t, ownerBackfillReport{Written: 1, Failed: 1}, report)
	require.Len(t, healthy.writes, 1)
	assert.Equal(t, "cfnimk5xgwekzkagkk6a6vmcu7kmmpnoda7kmktdmy27mbium4aq", healthy.writes[0].rkey)
}

func TestBackfillOwners_OwnerRecordTheIndexerRejectsIsReportedNotSuperseded(t *testing.T) {
	const (
		community = "did:plc:backfillunindexable"
		creator   = "did:plc:backfillcreatorone"
	)
	for _, test := range []struct {
		name   string
		record storedRecord
	}{
		{
			name:   "owner record at a key other than its subject's derived key",
			record: storedRecord{rkey: "ownerrecordelsewhere", value: moderatorRecord("did:plc:backfillsomeoneelse", "owner")},
		},
		{
			name:   "owner record whose subject is not a DID",
			record: storedRecord{rkey: communities.RoleRecordKey("someone.example"), value: moderatorRecord("someone.example", "owner")},
		},
		{
			name:   "owner record at the derived key with an invalid createdAt",
			record: storedRecord{rkey: communities.RoleRecordKey(creator), value: ownerRecordWithCreatedAt(creator, "not-a-time")},
		},
		{
			name:   "owner record at the derived key with no createdAt",
			record: storedRecord{rkey: communities.RoleRecordKey(creator), value: ownerRecordWithoutCreatedAt(creator)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBackfillFixture()
			f.addUser(creator)
			repo := f.addCommunity(community, creator, 1,
				storedRecord{rkey: "moderatorrecordone", value: moderatorRecord("did:plc:backfillmoderator", "moderator")},
				test.record)

			report, err := backfillOwners(context.Background(), f.deps())

			require.NoError(t, err)
			assert.Equal(t, ownerBackfillReport{OwnerUnindexable: 1}, report)
			assert.Empty(t, repo.writes, "an unindexable owner record is left for an admin, not superseded")
		})
	}
}

func TestBackfillOwners_RepeatedListCursorFailsTheCommunity(t *testing.T) {
	const (
		community = "did:plc:backfillstuckcursor"
		creator   = "did:plc:backfillcreatorone"
	)
	f := newBackfillFixture()
	f.addUser(creator)
	repo := f.addCommunity(community, creator, 1,
		storedRecord{rkey: "moderatorrecordone", value: moderatorRecord("did:plc:backfillmoderator", "moderator")},
		storedRecord{rkey: "moderatorrecordtwo", value: moderatorRecord("did:plc:backfillmoderatortwo", "moderator")})
	repo.stuckCursor = "1"

	report, err := backfillOwners(context.Background(), f.deps())

	require.Error(t, err)
	assert.Equal(t, ownerBackfillReport{Failed: 1}, report)
	assert.Empty(t, repo.writes, "a repo whose pages were not all read gets no write")
}
