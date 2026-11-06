//go:build integration

package jetstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community bans an account by writing social.coves.moderation.ban in its
// own repo, at the record key derived from the banned DID (one record per
// community and subject). The post consumer indexes those records; the ban
// index is then read through the repository's GetCommunityBan, which is what
// the write paths consult.

const (
	banCommunity2 = "did:plc:bancommunity2"
	banUserRepo   = "did:plc:banuserrepo"
)

// banStep is one commit delivered through HandleEvent.
type banStep struct {
	repo      string
	op        string // create, update, delete
	subject   string
	rkeyOf    string // the DID whose ban record key is used; the subject when empty
	rev       string
	cid       string
	reason    string
	expiresAt string
	// wantErr, when set, is the error class HandleEvent must report for
	// this step.
	wantErr error
}

// banExpectation is one GetCommunityBan read after every step ran; want nil
// means the index must report ErrCommunityBanNotFound.
type banExpectation struct {
	community string
	subject   string
	want      *communities.CommunityBan
}

func banCommitEvent(s banStep, timeUS int64) *JetstreamEvent {
	keyDID := s.rkeyOf
	if keyDID == "" {
		keyDID = s.subject
	}
	rkey := communities.BanRecordKey(keyDID)
	if s.op == "delete" {
		return revCommitEvent(s.repo, communities.BanCollection, "delete", rkey, s.rev, "", timeUS, nil)
	}
	record := map[string]interface{}{
		"$type":     communities.BanCollection,
		"subject":   s.subject,
		"createdAt": "2026-10-01T00:00:00Z",
	}
	if s.reason != "" {
		record["reason"] = s.reason
	}
	if s.expiresAt != "" {
		record["expiresAt"] = s.expiresAt
	}
	return revCommitEvent(s.repo, communities.BanCollection, s.op, rkey, s.rev, s.cid, timeUS, record)
}

func TestCommunityBanIngestion(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		steps   []banStep
		expects []banExpectation
	}{
		{
			name: "a ban in an indexed community's repo is indexed",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjecta", rev: revA,
					cid: "bafyreibanrecorda", reason: "spam", expiresAt: "2026-12-01T00:00:00Z",
				},
			},
			expects: []banExpectation{{
				community: accCommunity, subject: "did:plc:bansubjecta",
				want: &communities.CommunityBan{
					CommunityDID: "did:plc:acccommunity",
					SubjectDID:   "did:plc:bansubjecta",
					RecordKey:    "hj6tn6qelbcygtn3sejgd2m7xzhxkv3scdlubzfqv3i5xp7553fa",
					RecordCID:    "bafyreibanrecorda",
					Reason:       "spam",
					ExpiresAt:    &expiry,
				},
			}},
		},
		{
			name: "b a ban in a repo that is not an indexed community is not indexed",
			steps: []banStep{
				{
					repo: banUserRepo, op: "create", subject: "did:plc:bansubjectb", rev: revA,
					cid: "bafyreibanrecordb", reason: "spam",
					// Redriven, not dropped: a community's first ban can arrive
					// before its own profile event.
					wantErr: ErrUnresolvedReference,
				},
			},
			expects: []banExpectation{
				{community: banUserRepo, subject: "did:plc:bansubjectb"},
				{community: accCommunity, subject: "did:plc:bansubjectb"},
			},
		},
		{
			name: "c an update at a higher rev replaces reason and CID and clears expiresAt",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectc", rev: revA,
					cid: "bafyreibanrecordc1", reason: "spam", expiresAt: "2026-12-01T00:00:00Z",
				},
				{
					repo: accCommunity, op: "update", subject: "did:plc:bansubjectc", rev: revB,
					cid: "bafyreibanrecordc2", reason: "harassment",
				},
			},
			expects: []banExpectation{{
				community: accCommunity, subject: "did:plc:bansubjectc",
				want: &communities.CommunityBan{
					CommunityDID: "did:plc:acccommunity",
					SubjectDID:   "did:plc:bansubjectc",
					RecordKey:    "jx5pqvrzm4j7vg4krwbalxyrb4p7qwllvcw7xtgil7clvro7n6wa",
					RecordCID:    "bafyreibanrecordc2",
					Reason:       "harassment",
				},
			}},
		},
		{
			name: "d deleting the ban record removes the ban",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectd", rev: revA,
					cid: "bafyreibanrecordd", reason: "spam",
				},
				{repo: accCommunity, op: "delete", subject: "did:plc:bansubjectd", rev: revB},
			},
			expects: []banExpectation{{community: accCommunity, subject: "did:plc:bansubjectd"}},
		},
		{
			name: "e a ban whose rkey is not derived from its subject is not indexed",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjecte", rkeyOf: "did:plc:bansubjectother",
					rev: revA, cid: "bafyreibanrecorde", reason: "spam",
				},
			},
			expects: []banExpectation{{community: accCommunity, subject: "did:plc:bansubjecte"}},
		},
		{
			name: "f a stale create replayed after its delete stays deleted",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectf", rev: revA,
					cid: "bafyreibanrecordf", reason: "spam",
				},
				{repo: accCommunity, op: "delete", subject: "did:plc:bansubjectf", rev: revB},
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectf", rev: revA,
					cid: "bafyreibanrecordf", reason: "spam",
				},
			},
			expects: []banExpectation{{community: accCommunity, subject: "did:plc:bansubjectf"}},
		},
		{
			name: "g deleting one community's ban leaves the other community's ban",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectg", rev: revA,
					cid: "bafyreibanrecordg1", reason: "spam",
				},
				{
					repo: banCommunity2, op: "create", subject: "did:plc:bansubjectg", rev: revA,
					cid: "bafyreibanrecordg2", reason: "trolling",
				},
				{repo: accCommunity, op: "delete", subject: "did:plc:bansubjectg", rev: revB},
			},
			expects: []banExpectation{
				{community: accCommunity, subject: "did:plc:bansubjectg"},
				{
					community: banCommunity2, subject: "did:plc:bansubjectg",
					want: &communities.CommunityBan{
						CommunityDID: "did:plc:bancommunity2",
						SubjectDID:   "did:plc:bansubjectg",
						RecordKey:    "jxxo6iynjby4z6vut7wt76pajz6ltwnxj6zv54hvvkbbochqrrfa",
						RecordCID:    "bafyreibanrecordg2",
						Reason:       "trolling",
					},
				},
			},
		},
		{
			name: "h a ban with an unparseable expiresAt is not indexed",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjecth", rev: revA,
					cid: "bafyreibanrecordh", reason: "spam", expiresAt: "not-a-date",
					wantErr: ErrPermanentEvent,
				},
			},
			expects: []banExpectation{{community: accCommunity, subject: "did:plc:bansubjecth"}},
		},
		{
			name: "i a ban whose subject is not a DID is not indexed",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "not-a-did", rev: revA,
					cid: "bafyreibanrecordi", reason: "spam",
					wantErr: ErrPermanentEvent,
				},
			},
			expects: []banExpectation{{community: accCommunity, subject: "not-a-did"}},
		},
		{
			name: "j a ban whose reason exceeds the lexicon limit is not indexed",
			steps: []banStep{
				{
					repo: accCommunity, op: "create", subject: "did:plc:bansubjectj", rev: revA,
					cid: "bafyreibanrecordj", reason: strings.Repeat("a", maxBanReasonGraphemes+1),
					wantErr: ErrPermanentEvent,
				},
			},
			expects: []banExpectation{{community: accCommunity, subject: "did:plc:bansubjectj"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			bans := postgres.NewCommunityBanRepository(db)
			f := newAccFixture(t, db, WithPostCommunityBans(bans))
			insertBridgedCommunity(t, db, banCommunity2, "bancommunity2.test", accAuthor)
			insertBridgedUser(t, db, banUserRepo, "banuserrepo.test")
			ctx := context.Background()
			base := time.Now().UnixMicro()

			for i, step := range tt.steps {
				// A step without wantErr pins only the indexed outcome.
				err := f.consumer.HandleEvent(ctx, banCommitEvent(step, base+int64(i)*1_000_000))
				if step.wantErr != nil {
					assert.Truef(t, errors.Is(err, step.wantErr),
						"step %d must report %v, got %v", i, step.wantErr, err)
				}
			}

			for _, exp := range tt.expects {
				got, err := bans.GetCommunityBan(ctx, exp.community, exp.subject)
				if exp.want == nil {
					assert.ErrorIs(t, err, communities.ErrCommunityBanNotFound,
						"no ban may be indexed for %s in %s", exp.subject, exp.community)
					continue
				}
				require.NoError(t, err)
				require.NotNil(t, got, "a ban must be indexed for %s in %s", exp.subject, exp.community)
				assert.False(t, got.IndexedAt.IsZero(), "an indexed ban carries its index time")
				normalized := *got
				normalized.IndexedAt = time.Time{}
				if normalized.ExpiresAt != nil {
					utc := normalized.ExpiresAt.UTC()
					normalized.ExpiresAt = &utc
				}
				assert.Equal(t, *exp.want, normalized)
			}
		})
	}
}
