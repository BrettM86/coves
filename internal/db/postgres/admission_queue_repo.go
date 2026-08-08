package postgres

import (
	"context"

	"Coves/internal/core/posts"
)

// RED STUB (task 5, cycle 2). Signature only; the query is GREEN's.

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
	return nil, nil
}
