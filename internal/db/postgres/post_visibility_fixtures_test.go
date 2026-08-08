//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/posts"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Shared seeding for the read-path visibility suites (task 7, PRD §6.2).
//
// Every posts read path — the three feeds, post.get, actor.getPosts, the
// comment header, the profile counts — has to answer the SAME question before
// it renders a row: has THIS community admitted THIS post? Under author-owned
// posts (PRD §2, §6.1) that answer is not a column on `posts`; it is a row in
// community_post_admissions keyed by (community_did, post_uri), and a post can
// hold independent decisions from several communities at once. So a read path
// that does not join that table shows speech no community agreed to carry.
//
// These helpers seed the two halves directly — a postv2 content row in `posts`,
// and an admission decision in community_post_admissions — the way the firehose
// consumers would once the acceptance/removal engine has run. Direct SQL rather
// than the AdmissionRepository writers or the post consumer, for the reason the
// block-filter fixtures give one file over: what is under test is the READING
// query, and driving the writers would make a visibility suite fail whenever the
// admission state machine breaks, which is a different suite's job.

// postV2URI renders the AT-URI a postv2 record has once committed: the AUTHOR's
// DID is the authority (PRD §3.1), never the community's. The read paths must
// carry both kinds of post through one table, so the collection in the URI is
// the only thing that says which repo a row came from (see blobOwnerOf).
func postV2URI(authorDID, rkey string) string {
	return "at://" + authorDID + "/" + posts.PostV2Collection + "/" + rkey
}

// seedVisibilityPost inserts one author-owned postv2 content row and returns its
// URI. community_did is the post's INITIAL submission target (PRD §6.1) — the
// admission decision lives in a separate row, seeded with seedAdmission, so a
// post can be pending in one community and accepted in another (the fork case).
//
// The author row is NOT created here: some suites deliberately seed a post whose
// author has no `users` row (a federated author the AppView has not hydrated,
// PRD §5.3), and an INNER JOIN to users would make that post invisible even once
// its community accepts it. Callers that want a known author call createTestUser
// first.
func seedVisibilityPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey, title string, createdAt time.Time) string {
	t.Helper()

	uri := postV2URI(authorDID, rkey)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0)
	`, uri, "bafypostv2"+rkey, rkey, authorDID, communityDID, title, createdAt, 1, 1)
	require.NoErrorf(t, err, "seeding postv2 %s", rkey)
	return uri
}

// seedVisibilityPostWithEmbed is seedVisibilityPost carrying an external embed
// with a blob thumbnail, so a suite can prove the visible row still hydrates its
// media out of the AUTHOR's repository (blobOwnerOf, PRD §3.1) after the
// admission predicate lands. A predicate that dropped the author's pds_url from
// the SELECT would address the blob to an empty host — a broken image that looks
// fine server-side.
func seedVisibilityPostWithEmbed(t *testing.T, db *sql.DB, communityDID, authorDID, rkey, title, thumbCID string, createdAt time.Time) string {
	t.Helper()

	uri := postV2URI(authorDID, rkey)
	embedJSON := fmt.Sprintf(`{
		"$type": "social.coves.embed.external",
		"external": {
			"uri": "https://example.com/article",
			"title": "Example Article",
			"description": "A test article",
			"thumb": {"$type": "blob", "ref": {"$link": "%s"}, "mimeType": "image/jpeg", "size": 52813}
		}
	}`, thumbCID)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0)
	`, uri, "bafypostv2"+rkey, rkey, authorDID, communityDID, title, embedJSON, createdAt, 1, 1)
	require.NoErrorf(t, err, "seeding postv2-with-embed %s", rkey)
	return uri
}

// driftedAcceptedCID is the pinned CID of an acceptance the post's content has
// moved PAST (PRD §5.5): the community attested to this CID, an edit landed, and
// posts.cid advanced before the accepted → pending_reacceptance transition
// committed. It is deliberately a value seedVisibilityPost can never mint, so a
// row carrying it is un-attested by construction and the read predicate must
// hide it from everyone.
const driftedAcceptedCID = "bafyDRIFTEDacceptedcid"

// postContentCID reads back the content CID a seeded post actually carries, so
// an acceptance can PIN it. The read path gates on a.accepted_cid = p.cid, which
// makes "what CID does this post have" load-bearing rather than incidental: an
// acceptance pinning anything else is an acceptance of content that no longer
// stands.
func postContentCID(t *testing.T, db *sql.DB, postURI string) string {
	t.Helper()

	var cid string
	err := db.QueryRowContext(context.Background(), `SELECT cid FROM posts WHERE uri = $1`, postURI).Scan(&cid)
	require.NoErrorf(t, err, "reading the content CID of %s (seed the post before its admission)", postURI)
	return cid
}

// seedAdmission writes one community's decision about one post directly into
// community_post_admissions.
//
// The columns set per status mirror what the admission repository leaves behind:
// an accepted row pins the acceptance record and the accepted CID; a removed or
// rejected row carries the decision code the schema's CHECK constraint requires;
// a community event (accept/remove) advances the (rev, op_rank) watermark, while
// a pending observation and a local rejection do not. Callers that only care
// about the STATUS a reader keys off can ignore the detail and pass "".
//
// acceptedCID == "" MEANS "PIN THE POST'S REAL CONTENT CID", not "pin some
// placeholder". This is the intuitive-call trap the fixture used to set: the
// default was a fixed literal ("bafyaccepted") that can never equal the CID
// seedVisibilityPost derives from the rkey, so the obvious `…, Accepted, "", ""`
// call seeded an acceptance pinning content the post does not have and produced
// an INVISIBLE post. Every accepted seed in the suite had to hand-repeat the
// "bafypostv2"+rkey literal to work, and the next surface's test would have
// silently asserted against a post nobody can see. The default now looks the CID
// up, so the intuitive call means what it reads as. To seed the §5.5 drifted
// case deliberately, call seedVisibilityAdmissionDriftedCID; for an accepted row
// with NO pin at all, seedVisibilityAdmissionUnpinned.
func seedVisibilityAdmission(t *testing.T, db *sql.DB, communityDID, postURI string, status posts.AdmissionStatus, acceptedCID, decisionCode string) {
	t.Helper()

	var (
		acceptanceURI, acceptanceRkey, accCID, evalCID, code, rev sql.NullString
		decisionAt                                                sql.NullTime
		opRank                                                    sql.NullInt16
	)
	switch status {
	case posts.AdmissionStatusAccepted, posts.AdmissionStatusPendingReacceptance:
		acceptanceURI = sql.NullString{String: "at://" + communityDID + "/" + posts.AcceptanceCollection + "/acc" + postURI[len(postURI)-6:], Valid: true}
		acceptanceRkey = sql.NullString{String: "acc" + postURI[len(postURI)-6:], Valid: true}
		if acceptedCID == "" {
			if status == posts.AdmissionStatusPendingReacceptance {
				// pending_reacceptance IS the drifted state — the standing
				// acceptance pins content the post has moved past — so pinning the
				// post's current CID here would seed a row that contradicts its own
				// status.
				acceptedCID = driftedAcceptedCID
			} else {
				acceptedCID = postContentCID(t, db, postURI)
			}
		}
		accCID = sql.NullString{String: acceptedCID, Valid: true}
		// evaluated_cid is "the exact content CID the AppView has indexed", which
		// is the POST's CID whatever the acceptance pins — that is precisely how a
		// drifted acceptance is recognisable as drifted (accepted_cid <>
		// evaluated_cid). Mirroring accepted_cid into it, as this fixture used to,
		// seeds a row that claims the acceptance still matches the content.
		evalCID = sql.NullString{String: postContentCID(t, db, postURI), Valid: true}
		rev = sql.NullString{String: "3lqqqqqqqqqq2", Valid: true}
		opRank = sql.NullInt16{Int16: int16(posts.CommunityOpPut), Valid: true}
	case posts.AdmissionStatusRemoved:
		if decisionCode == "" {
			decisionCode = "rule-violation"
		}
		code = sql.NullString{String: decisionCode, Valid: true}
		decisionAt = sql.NullTime{Time: time.Now(), Valid: true}
		rev = sql.NullString{String: "3lqqqqqqqqqq3", Valid: true}
		opRank = sql.NullInt16{Int16: int16(posts.CommunityOpPut), Valid: true}
	case posts.AdmissionStatusRejected:
		if decisionCode == "" {
			decisionCode = "rate-limit-exceeded"
		}
		code = sql.NullString{String: decisionCode, Valid: true}
		decisionAt = sql.NullTime{Time: time.Now(), Valid: true}
	case posts.AdmissionStatusPending:
		// nothing set beyond status
	default:
		t.Fatalf("seedVisibilityAdmission: unknown status %q", status)
	}

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status,
			acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
			decision_code, decision_at,
			last_community_rev, last_community_op_rank,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = excluded.status,
			acceptance_uri = excluded.acceptance_uri,
			acceptance_rkey = excluded.acceptance_rkey,
			accepted_cid = excluded.accepted_cid,
			evaluated_cid = excluded.evaluated_cid,
			decision_code = excluded.decision_code,
			decision_at = excluded.decision_at,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()
	`, communityDID, postURI, string(status),
		acceptanceURI, acceptanceRkey, accCID, evalCID,
		code, decisionAt, rev, opRank)
	require.NoErrorf(t, err, "seeding admission %s for %s in %s", status, postURI, communityDID)
}

// seedVisibilityAdmissionDriftedCID seeds the §5.5 read-side hazard explicitly:
// a row that still says status='accepted' while the acceptance pins content the
// post has moved past. The admission consumer commits an edit's new content
// (posts.cid advances) in a SEPARATE transaction from the accepted →
// pending_reacceptance transition, so this state is reachable in production for
// as long as that window is open, and in it the attested content is gone with
// un-attested content standing in its place. The predicate must hide it from
// EVERYONE, author included — there is nothing here anyone agreed to carry.
func seedVisibilityAdmissionDriftedCID(t *testing.T, db *sql.DB, communityDID, postURI string) {
	t.Helper()
	require.NotEqualf(t, driftedAcceptedCID, postContentCID(t, db, postURI),
		"seedVisibilityAdmissionDriftedCID needs a post whose CID differs from the drifted pin, or it seeds the matched case")
	seedVisibilityAdmission(t, db, communityDID, postURI, posts.AdmissionStatusAccepted, driftedAcceptedCID, "")
}

// seedVisibilityAdmissionUnpinned seeds an accepted row whose accepted_cid is
// NULL. The column is nullable and migration 034 attaches no CHECK tying it to
// the status, so an acceptance with nothing pinned is REPRESENTABLE — a partial
// write, an acceptance record indexed before its subject's content, or a future
// writer that forgets the column. `a.accepted_cid = p.cid` is NULL-propagating,
// so such a row can never satisfy the accepted branch and the post is hidden
// from everyone but its author. That is the fail-closed answer (an acceptance
// that attests to no CID attests to nothing) and this helper exists so the case
// is stated rather than inferred.
func seedVisibilityAdmissionUnpinned(t *testing.T, db *sql.DB, communityDID, postURI string) {
	t.Helper()

	seedVisibilityAdmission(t, db, communityDID, postURI, posts.AdmissionStatusAccepted, "", "")
	_, err := db.ExecContext(context.Background(), `
		UPDATE community_post_admissions SET accepted_cid = NULL
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, postURI)
	require.NoErrorf(t, err, "unpinning the acceptance of %s in %s", postURI, communityDID)
}

// visibilityCommunity creates a community row plus its owner, returning the DID.
// A thin wrapper over createTestCommunity that mints the owner too, so a suite
// can stand up two communities (the fork case) without hand-rolling owners.
func visibilityCommunity(t *testing.T, db *sql.DB, label string) string {
	t.Helper()

	ownerDID := "did:plc:vis" + label + "owner"
	createTestUser(t, db, "vis"+label+"owner.test", ownerDID)
	communityDID := "did:plc:vis" + label + "community"
	createTestCommunity(t, db, communityDID, "vis"+label+".coves.social", ownerDID)
	return communityDID
}
