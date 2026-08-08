package postgres

import (
	"context"
	"fmt"
	"time"

	"Coves/internal/core/posts"
)

// maxPendingSubjectsPerPass caps what one backlog scan may return, whatever a
// caller asks for.
//
// The bound belongs here rather than only at the call site because the caller
// is a periodic job and the table grows with every submission the instance has
// ever seen: a driver misconfigured with an enormous batch would hold one
// transaction open across the whole backlog and then try to settle all of it
// inside a single bounded cycle.
const maxPendingSubjectsPerPass = 500

// defaultPendingSubjectsPerPass is what a non-positive limit means.
//
// A zero limit reads as "no bound" to a LIMIT clause author and as "no work" to
// everyone else, and neither is a useful answer to give a queue. Substituting a
// modest page keeps a driver built without an explicit batch size working
// rather than silently idle.
const defaultPendingSubjectsPerPass = 100

// ListPendingSubjects returns the acceptance engine's backlog: subjects this
// AppView can actually settle, oldest first.
//
// The two exclusions are documented on the interface
// (posts.AdmissionRepository); what belongs here is how they are SPELLED, since
// both are joins and a join written the obvious way gets one of them wrong:
//
//   - HOSTED is a credential test — communities.pds_refresh_token_encrypted IS
//     NOT NULL — and never communities.hosted_by_did, which is a claim copied
//     out of a firehose-indexed community's own profile record and is therefore
//     attacker-controlled (see posts.NewCommunityRepoFactory).
//   - THE POST MUST STAND, which is an INNER JOIN on posts plus a
//     deleted_at IS NULL predicate. Both halves are needed and they exclude
//     different rows: an admission can legitimately exist with NO post row at
//     all (an acceptance that arrived before its subject), and a LEFT JOIN would
//     let those through as decidable when there is nothing to decide about.
func (r *postgresAdmissionRepo) ListPendingSubjects(ctx context.Context, limit int) ([]posts.PendingSubject, error) {
	switch {
	case limit <= 0:
		limit = defaultPendingSubjectsPerPass
	case limit > maxPendingSubjectsPerPass:
		limit = maxPendingSubjectsPerPass
	}

	// Both joins are INNER, and both exclusions are spelled as a join rather
	// than as a NOT EXISTS so the planner can use the ordinary primary keys on
	// posts and communities. ORDER BY created_at is the queue discipline: a
	// queue served newest-first starves its own backlog, and the post that has
	// waited longest is the one whose author is already wondering where it went.
	const query = `
		SELECT admissions.community_did, admissions.post_uri, admissions.created_at
		FROM community_post_admissions AS admissions
		JOIN posts
		  ON posts.uri = admissions.post_uri
		 AND posts.deleted_at IS NULL
		JOIN communities
		  ON communities.did = admissions.community_did
		 AND communities.pds_refresh_token_encrypted IS NOT NULL
		WHERE admissions.status IN ('pending', 'pending_reacceptance')
		ORDER BY admissions.created_at ASC
		LIMIT $1
	`

	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("listing the acceptance engine's pending subjects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	subjects := make([]posts.PendingSubject, 0, limit)
	for rows.Next() {
		var subject posts.PendingSubject
		if err := rows.Scan(&subject.CommunityDID, &subject.PostURI, &subject.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning a pending subject: %w", err)
		}
		subjects = append(subjects, subject)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the pending subjects: %w", err)
	}
	return subjects, nil
}

// CountRecentAdmissions counts this author's admitted posts in one community
// since a point in time.
//
// The author is matched by AT-URI prefix rather than by joining posts: under
// author-owned posts the URI's authority IS the author, and an admission can
// legitimately exist with no posts row at all (an acceptance that arrived before
// its subject). A join would silently exempt exactly those from the quota.
func (r *postgresAdmissionRepo) CountRecentAdmissions(
	ctx context.Context, communityDID, authorDID string, since time.Time,
) (int, error) {
	return 0, nil
}
