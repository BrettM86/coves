//go:build integration

package jetstream

import (
	"database/sql"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community classification through the community consumer and the worker
// (docs/PRD_CONTENT_CLASSIFICATION.md, chunk 03): a community profile write
// enqueues a job in the repository's write transaction when a classified
// field (display name, description, avatar, banner) changed, or when the
// community still has a non-terminal job. The name is classified too, but
// from the stored value: it is immutable, so a record cannot change it. The
// subject is the community DID, which is also the job's author and community.

const (
	clsProfileCommunity       = accPrefix + "profilecommunity"
	clsProfileCommunityHandle = "c-otterwatch.test.local"
)

// clsCommunityFields are the profile record fields the community tests vary.
type clsCommunityFields struct {
	Name        string
	DisplayName string
	Description string
	Avatar      string
	Banner      string
	MemberCount int
	Warnings    []string
}

// clsOtterWatch is the profile every community test starts from.
func clsOtterWatch() clsCommunityFields {
	return clsCommunityFields{
		Name:        "otterwatch",
		DisplayName: "Otter Watch",
		Description: "Sightings of otters along the coast",
	}
}

// clsCommunityRecord is communityProfileEvent's record for clsProfileCommunity
// with fields applied.
func clsCommunityRecord(fields clsCommunityFields) map[string]interface{} {
	record := communityProfileEvent(clsProfileCommunity, clsProfileCommunityHandle, fields.Name, "").Commit.Record
	record["displayName"] = fields.DisplayName
	if fields.Description != "" {
		record["description"] = fields.Description
	}
	if fields.Avatar != "" {
		record["avatar"] = profileBlob(fields.Avatar)
	}
	if fields.Banner != "" {
		record["banner"] = profileBlob(fields.Banner)
	}
	if fields.MemberCount != 0 {
		record["memberCount"] = fields.MemberCount
	}
	if fields.Warnings != nil {
		record["contentWarnings"] = fields.Warnings
	}
	return record
}

// profile consumes a create or update of clsProfileCommunity's profile at cid
// through the gated community consumer.
func (f *clsFixture) profile(t *testing.T, op, cid string, fields clsCommunityFields) {
	t.Helper()
	f.timeUS += 1_000_000
	require.NoError(t, f.communities.HandleEvent(t.Context(), revCommitEvent(
		clsProfileCommunity, conflictProfileCollection, op, "self", testkit.TID(), cid, f.timeUS, clsCommunityRecord(fields))))
	var storedCID, avatarCID, bannerCID sql.NullString
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT record_cid, avatar_cid, banner_cid FROM communities WHERE did = $1`, clsProfileCommunity).
		Scan(&storedCID, &avatarCID, &bannerCID))
	require.Equal(t, cid, storedCID.String, "fixture: the %s at %s must apply", op, cid)
	require.Equal(t, fields.Avatar, avatarCID.String, "fixture: the avatar must survive canonicalization")
	require.Equal(t, fields.Banner, bannerCID.String, "fixture: the banner must survive canonicalization")
}

// jobCIDs lists the record CIDs of every job of uri, in insertion order.
func (f *clsFixture) jobCIDs(t *testing.T, uri string) []string {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(),
		`SELECT record_cid FROM classification_jobs WHERE subject_uri = $1 ORDER BY id`, uri)
	require.NoError(t, err)
	defer rows.Close()
	var cids []string
	for rows.Next() {
		var cid string
		require.NoError(t, rows.Scan(&cid))
		cids = append(cids, cid)
	}
	require.NoError(t, rows.Err())
	return cids
}

// TestClassificationEnqueue_CommunityProfileChanges (M1): with every
// predecessor job terminal, a profile update queues the new CID exactly when
// a classified field changed.
func TestClassificationEnqueue_CommunityProfileChanges(t *testing.T) {
	t.Parallel()
	f := newClsFixture(t)
	const createCID = "bafyclscommunitycreate"

	fields := clsOtterWatch()
	f.profile(t, "create", createCID, fields)
	assert.Equal(t, "queued", f.jobStatus(t, clsProfileCommunity, createCID), "the community create enqueues a job")
	f.runWorker(t, f.worker)
	assert.Equal(t, "classified", f.jobStatus(t, clsProfileCommunity, createCID))
	assert.Equal(t, []clsResult{{
		SubjectKind:  "community",
		SubjectCID:   "bafyclscommunitycreate",
		AuthorDID:    "did:plc:accprofilecommunity",
		CommunityDID: "did:plc:accprofilecommunity",
	}}, f.results(t, clsProfileCommunity))
	assert.Equal(t, []string{"otterwatch\n\nOtter Watch\n\nSightings of otters along the coast"}, f.states(t),
		"the create's request carries name, display name and description")

	for _, step := range []struct {
		name    string
		cid     string
		change  func(*clsCommunityFields)
		wantJob bool
	}{
		{"member count only", "bafyclscommunitymembers", func(c *clsCommunityFields) { c.MemberCount = 42 }, false},
		{"description", "bafyclscommunitydescription", func(c *clsCommunityFields) { c.Description = "Otter and seal sightings" }, true},
		{"avatar", "bafyclscommunityavatar", func(c *clsCommunityFields) { c.Avatar = profileAvatarA }, true},
		{"display name only", "bafyclscommunitydisplayname", func(c *clsCommunityFields) { c.DisplayName = "Coastal Otter Watch" }, true},
		{"banner only", "bafyclscommunitybanner", func(c *clsCommunityFields) { c.Banner = profileBannerB }, true},
	} {
		step.change(&fields)
		f.profile(t, "update", step.cid, fields)
		if step.wantJob {
			assert.Equal(t, "queued", f.jobStatus(t, clsProfileCommunity, step.cid), "%s: the update queues its CID", step.name)
		} else {
			assert.Equal(t, "", f.jobStatus(t, clsProfileCommunity, step.cid), "%s: the update queues nothing", step.name)
		}
		f.runWorker(t, f.worker)
		if step.wantJob {
			assert.Equal(t, "classified", f.jobStatus(t, clsProfileCommunity, step.cid), "%s: the job is terminal before the next step", step.name)
		}
	}

	assert.Equal(t, []string{
		"bafyclscommunitycreate",
		"bafyclscommunitydescription",
		"bafyclscommunityavatar",
		"bafyclscommunitydisplayname",
		"bafyclscommunitybanner",
	}, f.jobCIDs(t, clsProfileCommunity), "jobs exist for exactly the classified-field changes")
}

// TestClassificationEnqueue_CommunityNameOnlyChange (M2): the community name
// is immutable, so an update that changes only the record's name keeps the
// stored name and queues no job, and later classified text uses the stored
// name.
func TestClassificationEnqueue_CommunityNameOnlyChange(t *testing.T) {
	t.Parallel()
	f := newClsFixture(t)
	const createCID, renameCID, describeCID = "bafyclscommunitynamea", "bafyclscommunitynameb", "bafyclscommunitynamec"

	fields := clsOtterWatch()
	f.profile(t, "create", createCID, fields)
	f.runWorker(t, f.worker)
	require.Equal(t, "classified", f.jobStatus(t, clsProfileCommunity, createCID), "fixture: the create job is classified")

	fields.Name = "kelpwatch"
	f.profile(t, "update", renameCID, fields)
	f.runWorker(t, f.worker)

	var storedName string
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT name FROM communities WHERE did = $1`, clsProfileCommunity).Scan(&storedName))
	assert.Equal(t, "otterwatch", storedName, "the update must not write the record's name")
	assert.Equal(t, "", f.jobStatus(t, clsProfileCommunity, renameCID), "a name-only change queues nothing")
	assert.Equal(t, []string{createCID}, f.jobCIDs(t, clsProfileCommunity), "only the create has a job")
	assert.Equal(t, []string{
		"otterwatch\n\nOtter Watch\n\nSightings of otters along the coast",
	}, f.states(t), "only the create was classified")

	fields.Description = "Sightings of otters in the kelp"
	f.profile(t, "update", describeCID, fields)
	assert.Equal(t, "queued", f.jobStatus(t, clsProfileCommunity, describeCID), "a description change queues its CID")
	f.runWorker(t, f.worker)
	states := f.states(t)
	if assert.Len(t, states, 2) {
		assert.Equal(t, "otterwatch\n\nOtter Watch\n\nSightings of otters in the kelp", states[1],
			"the classified text uses the stored name, not the record's")
	}
}

// TestClassificationEnqueue_CommunityNonContentChangeWhileQueued (M3): an
// update that changes no classified field still queues the new CID while the
// community's job is queued, and the worker supersedes the old CID.
func TestClassificationEnqueue_CommunityNonContentChangeWhileQueued(t *testing.T) {
	t.Parallel()
	f := newClsFixture(t)
	const createCID, warnedCID = "bafyclscommunityqueueda", "bafyclscommunityqueuedb"

	fields := clsOtterWatch()
	f.profile(t, "create", createCID, fields)
	fields.Warnings = []string{"spoiler"}
	f.profile(t, "update", warnedCID, fields)
	f.runWorker(t, f.worker)

	assert.Equal(t, "classified", f.jobStatus(t, clsProfileCommunity, warnedCID), "the successor job is classified")
	assert.Equal(t, "closed", f.jobStatus(t, clsProfileCommunity, createCID), "the queued create job is closed")
	var closeReason sql.NullString
	err := f.db.QueryRowContext(t.Context(),
		`SELECT close_reason FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2`,
		clsProfileCommunity, createCID).Scan(&closeReason)
	if assert.NoError(t, err, "the create job row") {
		assert.Equal(t, sql.NullString{String: "superseded", Valid: true}, closeReason)
	}
	assert.Equal(t, []string{"otterwatch\n\nOtter Watch\n\nSightings of otters along the coast"}, f.states(t),
		"one request, for the successor CID")
}
