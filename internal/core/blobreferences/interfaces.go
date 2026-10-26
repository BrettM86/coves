// Package blobreferences records which (owner DID, blob CID) pairs each indexed
// subject uses, and as what kind.
package blobreferences

import (
	"context"
	"database/sql"

	"Coves/internal/atproto/embedblobs"
)

// Repository writes blob references inside a caller's indexing transaction.
type Repository interface {
	// ReplaceSubjectTx makes references the subject's exact active set: every
	// other reference of subjectURI becomes inactive, and each given (CID, kind)
	// is stored active and owned by ownerDID.
	ReplaceSubjectTx(ctx context.Context, tx *sql.Tx, ownerDID, subjectURI string, references []embedblobs.Reference) error
	// DeactivateSubjectTx marks every reference of subjectURI inactive. Rows are kept.
	DeactivateSubjectTx(ctx context.Context, tx *sql.Tx, subjectURI string) error
	// ListBlobOwners lists every owner DID referencing cid, active or inactive.
	ListBlobOwners(ctx context.Context, cid string) ([]string, error)
	// HasActiveReference reports whether (ownerDID, cid) has an active reference.
	HasActiveReference(ctx context.Context, ownerDID, cid string) (bool, error)
}
