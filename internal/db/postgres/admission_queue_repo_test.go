//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The acceptance engine's backlog query (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6).
//
// ListPendingSubjects is a work queue, and the assertions that matter here are
// about what it LEAVES OUT rather than what it returns. Every excluded class is
// a subject the engine can never settle, and including one does not produce a
// visible failure — it produces a pass that does a little useless work, forever,
// on every instance, growing with the network. The two that would hurt most:
//
//   - A community whose credentials this AppView does not hold. The engine's
//     entire job is to write an acceptance into that community's repo. Handing
//     it a community it cannot sign for means every pass ends in the same
//     credential failure, and the genuine deferrals — the ones an operator needs
//     to see — are buried under them.
//   - A subject whose post has been tombstoned. Accepting a deleted post writes
//     an acceptance for content that no longer exists, and the host-side
//     tombstone sweep then deletes that acceptance; the next pass re-lists the
//     same still-pending subject and does it again. Two components, each correct
//     in isolation, looping against a PDS.

// hostedCommunity seeds a community this AppView genuinely hosts.
//
// The credential column is written directly rather than through
// UpdateCredentials, and the value is not a real token, because the query's
// question is PRESENCE and nothing decrypts it here. Going through
// UpdateCredentials would drag in the app-side credential cipher and generate a
// real encrypted value to prove something about a predicate that only reads IS
// NOT NULL.
func hostedCommunity(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	did := uncredentialedCommunity(t, db, name)
	_, err := db.ExecContext(context.Background(),
		`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
		did, []byte("a stored refresh token"))
	require.NoErrorf(t, err, "granting %s credentials", did)
	return did
}

// uncredentialedCommunity seeds a community this AppView does NOT host.
//
// It is fixtures.Community unchanged, and that is the trap worth naming:
// fixtures.Community sets hosted_by_did to this instance's DID while storing no
// credentials at all. Every community indexed from the firehose has exactly that
// shape, because hosted_by_did is copied out of the community's own profile
// record — a claim by whoever controls that repo. So an implementation that
// asked "does hosted_by_did name us" would return this community, and an
// attacker could put their community in any AppView's work queue by writing one
// field.
func uncredentialedCommunity(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	label := testkit.UniqueIDWithPrefix(t, name)
	did, err := fixtures.Community(context.Background(), db, label, "owner"+label)
	require.NoErrorf(t, err, "seeding community %s", label)
	return did
}

// pendingSubjectIn seeds a live post and an admission row in the given status,
// with created_at set to age ago so ordering is assertable without sleeping.
func pendingSubjectIn(t *testing.T, db *sql.DB, communityDID, status string, age time.Duration) string {
	t.Helper()

	uri := livePost(t, db, communityDID)
	seedAdmission(t, db, communityDID, uri, status, age)
	return uri
}

// livePost inserts an author-repo post row that is indexed and not deleted.
func livePost(t *testing.T, db *sql.DB, communityDID string) string {
	t.Helper()

	authorDID := fixtures.DID(testkit.UniqueID(t))
	rkey := testkit.TID()
	uri := "at://" + authorDID + "/social.coves.community.postv2/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, uri, "bafyreiqueue"+rkey, rkey, authorDID, communityDID, "a post awaiting a verdict")
	require.NoError(t, err)
	return uri
}

// seedAdmission writes one admission row directly, so a test can produce a
// status the repository's own mutations would refuse to reach in one step.
func seedAdmission(t *testing.T, db *sql.DB, communityDID, postURI, status string, age time.Duration) {
	t.Helper()

	var decisionCode any
	if status == "rejected" || status == "removed" {
		decisionCode = string(posts.DecisionRuleViolation)
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_post_admissions
			(community_did, post_uri, status, decision_code, evaluated_cid, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW() - $6::interval, NOW())
	`, communityDID, postURI, status, decisionCode, "bafyreievaluated", age.String())
	require.NoErrorf(t, err, "seeding a %s admission for %s", status, postURI)
}

// subjectURIs reduces a result to the post URIs it named, for set comparison.
func subjectURIs(subjects []posts.PendingSubject) []string {
	uris := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		uris = append(uris, subject.PostURI)
	}
	return uris
}

func TestListPendingSubjects_ReturnsOnlyTheUndecidedStatuses(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	community := hostedCommunity(t, db, "queuestat")

	pending := pendingSubjectIn(t, db, community, "pending", 4*time.Minute)
	reacceptance := pendingSubjectIn(t, db, community, "pending_reacceptance", 3*time.Minute)
	accepted := pendingSubjectIn(t, db, community, "accepted", 2*time.Minute)
	rejected := pendingSubjectIn(t, db, community, "rejected", time.Minute)
	removed := pendingSubjectIn(t, db, community, "removed", 30*time.Second)

	subjects, err := NewAdmissionRepository(db).ListPendingSubjects(context.Background(), 50)
	require.NoError(t, err)

	// Both undecided states, and both for the same reason: §5.6 names
	// pending and pending_reacceptance as the two the engine owes an answer for.
	// Leaving pending_reacceptance out would strand every edited post in a state
	// where the old acceptance no longer applies and no new one is ever written —
	// the post silently disappears from the community for good.
	assert.ElementsMatch(t, []string{pending, reacceptance}, subjectURIs(subjects),
		"the backlog is exactly the undecided states")

	for _, settled := range []string{accepted, rejected, removed} {
		assert.NotContainsf(t, subjectURIs(subjects), settled,
			"a settled subject (%s) must not be re-offered: re-deciding a removed row is how a removed post is laundered back into a feed", settled)
	}
}

func TestListPendingSubjects_IsOldestFirstAndBounded(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	community := hostedCommunity(t, db, "queueorder")

	oldest := pendingSubjectIn(t, db, community, "pending", 10*time.Minute)
	middle := pendingSubjectIn(t, db, community, "pending", 5*time.Minute)
	newest := pendingSubjectIn(t, db, community, "pending", time.Minute)

	repo := NewAdmissionRepository(db)

	all, err := repo.ListPendingSubjects(context.Background(), 50)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, middle, newest}, subjectURIs(all),
		"oldest first: a queue served newest-first starves its own backlog, and the post that has waited longest is the one whose author is already wondering where it went")

	// The bound is not a nicety. This query runs on a timer against a table that
	// grows with every submission the instance has ever seen, and a pass that
	// listed the whole backlog would hold one transaction open across it and then
	// try to settle all of it inside a single bounded cycle.
	page, err := repo.ListPendingSubjects(context.Background(), 2)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, middle}, subjectURIs(page),
		"the limit must bound the result, and must take the oldest — a limit applied after an unordered scan would let a busy instance never reach its oldest work")

	// created_at travels with the subject because the health surface reports the
	// age of the oldest undecided row, which is the queue's only early warning.
	require.NotEmpty(t, all)
	assert.WithinDuration(t, time.Now().Add(-10*time.Minute), all[0].CreatedAt, time.Minute,
		"the subject must carry the created_at the ordering was made on")
}

func TestListPendingSubjects_ExcludesDeletedAndUnindexedPosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	community := hostedCommunity(t, db, "queueghost")

	live := pendingSubjectIn(t, db, community, "pending", 3*time.Minute)

	// A post the author deleted. The admission row survives the tombstone (the
	// community's decision about it is history), but there is nothing left to
	// decide.
	tombstoned := pendingSubjectIn(t, db, community, "pending", 2*time.Minute)
	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, tombstoned)
	require.NoError(t, err)

	// An admission with NO post row at all — an acceptance that arrived before
	// its subject, which §5.4 says is a state the system genuinely reaches and
	// which is why community_post_admissions carries no foreign key to posts.
	// This is the row a LEFT JOIN would let through.
	unindexed := "at://" + fixtures.DID(testkit.UniqueID(t)) + "/social.coves.community.postv2/" + testkit.TID()
	seedAdmission(t, db, community, unindexed, "pending", time.Minute)

	subjects, err := NewAdmissionRepository(db).ListPendingSubjects(ctx, 50)
	require.NoError(t, err)

	assert.Equal(t, []string{live}, subjectURIs(subjects))
	assert.NotContainsf(t, subjectURIs(subjects), tombstoned,
		"a tombstoned post (%s) must not be offered for a decision: accepting it writes an acceptance for content that no longer exists, "+
			"and the host-side tombstone sweep then deletes that acceptance — the two loop against each other forever", tombstoned)
	assert.NotContainsf(t, subjectURIs(subjects), unindexed,
		"an admission whose post was never indexed (%s) has no content to judge; a LEFT JOIN would offer it every pass and the engine would defer it every pass", unindexed)
}

func TestListPendingSubjects_ExcludesCommunitiesThisAppViewCannotSignFor(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	hosted := hostedCommunity(t, db, "queuehosted")
	claimant := uncredentialedCommunity(t, db, "queueclaim")

	ours := pendingSubjectIn(t, db, hosted, "pending", 2*time.Minute)
	theirs := pendingSubjectIn(t, db, claimant, "pending", time.Minute)

	// The claimant's row asserts what an attacker-controlled profile record can
	// assert. hosted_by_did is copied out of a firehose-indexed community's own
	// profile, so any repo on the network can name this instance as its host;
	// only the credential column reflects something this AppView did itself.
	var claimedHost string
	require.NoError(t, db.QueryRow(`SELECT hosted_by_did FROM communities WHERE did = $1`, claimant).Scan(&claimedHost))
	require.Equal(t, fixtures.InstanceDID(), claimedHost,
		"fixture: the uncredentialed community must CLAIM this instance as its host, or this test proves nothing about which signal is trusted")

	subjects, err := NewAdmissionRepository(db).ListPendingSubjects(context.Background(), 50)
	require.NoError(t, err)

	assert.Equal(t, []string{ours}, subjectURIs(subjects))
	assert.NotContainsf(t, subjectURIs(subjects), theirs,
		"a community with NO stored credentials (%s) was offered to the engine even though it only CLAIMS this instance as its host. "+
			"Hosting is credential presence — pds_refresh_token_encrypted, which exists only because this AppView provisioned the account — "+
			"and never hosted_by_did, which anyone can write into their own profile record", claimant)
}

func TestAdmissionsTable_PendingQueueIndex(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	requireTableExists(t, db, admissionsTable)

	// The existing queue index leads with community_did — right for a
	// moderator's view of ONE community, useless for this query, which asks
	// "what is undecided anywhere" and would scan the whole table through it.
	// A partial index over the two undecided statuses is what keeps a periodic
	// pass from getting more expensive with every post the instance ever
	// indexed, and nothing about a missing one FAILS: it just quietly degrades.
	var matched string
	for name, definition := range indexDefinitions(t, db, admissionsTable) {
		predicate := normalizePredicate(indexPredicate(definition))
		if predicate == "" || !strings.Contains(predicate, "pending") || !strings.Contains(predicate, "pending_reacceptance") {
			continue
		}
		if strings.HasPrefix(indexColumns(definition), "created_at") {
			matched = name
		}
	}
	assert.NotEmptyf(t, matched,
		"no partial index on (created_at) restricted to the undecided statuses; ListPendingSubjects runs on a timer over a table that grows "+
			"with every submission, and the existing (community_did, status, created_at) index cannot serve a cross-community scan. Indexes found: %v",
		indexDefinitions(t, db, admissionsTable))
}
