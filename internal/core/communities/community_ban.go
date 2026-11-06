package communities

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"
)

// BanCollection is the NSID of a community's ban record.
const BanCollection = "social.coves.moderation.ban"

// ErrCommunityBanNotFound is returned when the ban index has no ban for a
// community and subject.
var ErrCommunityBanNotFound = errors.New("community ban not found")

// CommunityBan is one indexed social.coves.moderation.ban record.
type CommunityBan struct {
	CommunityDID string
	SubjectDID   string
	RecordKey    string
	RecordCID    string
	Reason       string
	ExpiresAt    *time.Time
	IndexedAt    time.Time
}

// URI is the AT-URI of the ban record.
func (b CommunityBan) URI() string {
	return "at://" + b.CommunityDID + "/" + BanCollection + "/" + b.RecordKey
}

// BanReader reads the community ban index.
type BanReader interface {
	GetCommunityBan(ctx context.Context, communityDID, subjectDID string) (*CommunityBan, error)
}

// BanSubjectLockKeySQL is the transaction-scoped advisory lock key for one
// DID, with the DID as $1, held until commit. Its holders:
//   - ban indexing, on the banned DID, before drawing a Coves index order;
//   - comment, vote and post indexing, on the author's DID, before drawing a
//     Coves index order, so a ban and a record by its subject commit in the
//     order of their index orders;
//   - moderation LockSubject, on the DID of every at://<did> subject (account
//     bans and unbans, community removals and restores);
//   - userRepo.Create and communityRepo.Create, on the DID they index, so an
//     account row and a community row for one DID cannot both slip past the
//     other's account-activity check.
const BanSubjectLockKeySQL = "hashtext('community-ban-subject:' || $1)"

// BanRepository maintains the community ban index.
type BanRepository interface {
	BanReader
	UpsertCommunityBan(ctx context.Context, ban CommunityBan) error
	DeleteCommunityBan(ctx context.Context, communityDID, recordKey string) error
}

// CommunityBanIndex reads bans and resolves the community of indexed content.
type CommunityBanIndex interface {
	BanReader
	ContentCommunity(ctx context.Context, contentURI string) (communityDID string, found bool, err error)
}

// ActiveCommunityBan returns the ban in force for subjectDID in communityDID at
// now, or nil when there is none. A ban with an expiresAt lapses at that
// instant. A lookup error is returned, so callers fail closed.
func ActiveCommunityBan(ctx context.Context, reader BanReader, communityDID, subjectDID string, now time.Time) (*CommunityBan, error) {
	ban, err := reader.GetCommunityBan(ctx, communityDID, subjectDID)
	if errors.Is(err, ErrCommunityBanNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ban == nil {
		return nil, nil
	}
	if ban.ExpiresAt != nil && !now.Before(*ban.ExpiresAt) {
		return nil, nil
	}
	return ban, nil
}

// BanRecordKey is the deterministic record key of a ban on subjectDID.
// It is the unpadded lowercase base32 encoding of SHA-256(subjectDID), so a
// community holds at most one ban record per subject.
func BanRecordKey(subjectDID string) string {
	digest := sha256.Sum256([]byte(subjectDID))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
}
