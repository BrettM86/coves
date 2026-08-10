package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// The hosted-community scope query, kept deliberately apart from the community
// repository (docs/PRD_AUTHOR_OWNED_POSTS.md §11).
//
// # WHY THIS IS ITS OWN QUERY AND NOT A FIELD ON `List`
//
// "Which communities does this AppView host?" is answered by ONE fact: whether
// it stores that community's PDS refresh token. Nothing else — not
// `hosted_by_did`, which is a claim in a record anyone can write — decides
// whether we can sign a write, and therefore whether the re-materialization tool
// may write into that repo and delete out of it.
//
// The obvious implementation, walking `Repository.List` and testing
// `PDSRefreshToken != ""`, is silently wrong in the most dangerous direction:
// `List`'s SELECT does not include the credential columns, so the field is empty
// on every listed row and the filter matches NOTHING. A tool built on it reports
// a clean, complete, exit-0 run having migrated nothing at all.
//
// The repair is NOT to teach `List` to decrypt credentials. `List` backs public
// discovery endpoints; putting a decrypted refresh token on that path so that
// one batch tool can test it for emptiness trades a leak for a convenience. This
// query instead answers the question directly, in the vocabulary the caller
// actually needs — DIDs — and never decrypts anything: presence of the
// ciphertext column IS the fact being asked about.
type hostedCommunityQuery struct {
	db *sql.DB
}

// HostedCommunitySource answers which communities this AppView can sign for.
//
// It is an interface so the tool's scope can be faked in tests without a
// database, and so the production wiring reads as the narrow capability it is
// rather than as "the community repository".
type HostedCommunitySource interface {
	// HostedCommunityDIDs returns the DIDs of every community whose PDS refresh
	// token this AppView stores, in a stable order. It returns identifiers only:
	// no credential material crosses the boundary.
	HostedCommunityDIDs(ctx context.Context) ([]string, error)
}

// NewHostedCommunityQuery returns the hosted-community scope query.
func NewHostedCommunityQuery(db *sql.DB) HostedCommunitySource {
	return &hostedCommunityQuery{db: db}
}

// HostedCommunityDIDs returns the DIDs of the communities whose PDS refresh
// token is stored — the exact set whose repos this AppView can write to and
// delete from.
//
// The predicate is `pds_refresh_token_encrypted IS NOT NULL` because that column
// is written only by the provisioning path that also holds the account: a row
// carrying it is a repo we have credentials for, and a row without it is one we
// merely index. The ciphertext is never decrypted here — its presence is the
// whole answer.
func (q *hostedCommunityQuery) HostedCommunityDIDs(ctx context.Context) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT did
		FROM communities
		WHERE pds_refresh_token_encrypted IS NOT NULL
		ORDER BY did
	`)
	if err != nil {
		return nil, fmt.Errorf("listing hosted community DIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var dids []string
	for rows.Next() {
		var did string
		if err := rows.Scan(&did); err != nil {
			return nil, fmt.Errorf("scanning a hosted community DID: %w", err)
		}
		dids = append(dids, did)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating hosted community DIDs: %w", err)
	}
	return dids, nil
}
