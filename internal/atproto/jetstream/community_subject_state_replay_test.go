//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A replay the rev gate or the admission watermark drops changes no index row,
// so it must not advance the subject's community version. Each row first
// applies events that do change the subject, so its version is at least one
// and a test that never advanced anything cannot pass.

// replayEnvironment is one subject's consumers over one database.
type replayEnvironment struct {
	db      *sql.DB
	deliver func(ctx context.Context, event *JetstreamEvent) error
}

const (
	replayPostRkey       = "csreplaypost"
	replayPostCID        = "bafyreicsreplaypost"
	replayRootRkey       = "csreplayroot"
	replayRootCID        = "bafyreicsreplayroot"
	replayOtherRootRkey  = "csreplayotherroot"
	replayOtherRootCID   = "bafyreicsreplayotherroot"
	replayCommentRkey    = "csreplaycomment"
	replayCommentCID     = "bafyreicsreplaycomment"
	replayBanSubject     = "did:plc:csreplaybansubject"
	replayCommentSubject = "at://did:plc:ccrcommenter/social.coves.community.comment/csreplaycomment"
	replayPostSubject    = "at://did:plc:accauthor/social.coves.community.postv2/csreplaypost"
)

func newReplayPostEnvironment(t *testing.T) replayEnvironment {
	t.Helper()
	db := testkit.DB(t)
	f := newAccFixture(t, db)
	markHosted(t, db, accCommunity)
	return replayEnvironment{db: db, deliver: f.consumer.HandleEvent}
}

func newReplayBanEnvironment(t *testing.T) replayEnvironment {
	t.Helper()
	db := testkit.DB(t)
	f := newAccFixture(t, db, WithPostCommunityBans(postgres.NewCommunityBanRepository(db)))
	markHosted(t, db, accCommunity)
	return replayEnvironment{db: db, deliver: f.consumer.HandleEvent}
}

func newReplayCommentEnvironment(t *testing.T) replayEnvironment {
	t.Helper()
	f := newCCRFixture(t)
	markHosted(t, f.db, accCommunity)
	return replayEnvironment{db: f.db, deliver: func(ctx context.Context, event *JetstreamEvent) error {
		if event.Commit != nil && event.Commit.Collection == CommentCollection {
			return f.comments.HandleEvent(ctx, event)
		}
		return f.posts.consumer.HandleEvent(ctx, event)
	}}
}

func newReplayRosterEnvironment(t *testing.T) replayEnvironment {
	t.Helper()
	db := testkit.DB(t)
	consumer, _ := newRosterConsumer(t, db)
	return replayEnvironment{db: db, deliver: consumer.HandleEvent}
}

func replayRootEvent(rev string, timeUS int64) *JetstreamEvent {
	return pv2Event(accAuthor, "create", replayRootRkey, rev, replayRootCID, timeUS,
		pv2Record(accCommunity, "thread root", "root body"))
}

func replayOtherRootEvent(rev string, timeUS int64) *JetstreamEvent {
	return pv2Event(accAuthor, "create", replayOtherRootRkey, rev, replayOtherRootCID, timeUS,
		pv2Record(ccrOtherCommunity, "other root", "other body"))
}

func replayCommentEvent(rev string, timeUS int64) *JetstreamEvent {
	rootURI := accPostURI(replayRootRkey)
	return revCommitEvent(ccrCommenter, CommentCollection, "create", replayCommentRkey, rev, replayCommentCID, timeUS,
		revCommentRecord("the comment", rootURI, replayRootCID, rootURI, replayRootCID))
}

func replayCommentRemovalEvent(rev string, timeUS int64) *JetstreamEvent {
	return removalEvent(accCommunity, replayCommentSubject, replayCommentCID, string(posts.DecisionRuleViolation), rev, timeUS)
}

// B5
func TestCommunitySubjectState_StaleReplayLeavesTheVersion(t *testing.T) {
	t.Parallel()

	// revs are strictly increasing; base is the event time origin.
	type eventsAt func(revs []string, base int64) []*JetstreamEvent

	tests := []struct {
		name      string
		community string
		subject   string
		kind      moderation.CommunitySubjectKind
		env       func(t *testing.T) replayEnvironment
		advance   eventsAt
		// between runs after advance and before replay, outside the consumers.
		between   func(t *testing.T, db *sql.DB)
		replay    eventsAt
		wantDelta int64
		check     func(t *testing.T, state *moderation.CommunityState)
	}{
		{
			name: "admission: an older removal and an equal-rev acceptance replay", community: accCommunity,
			subject: replayPostSubject, kind: "post",
			env: func(t *testing.T) replayEnvironment {
				env := newReplayPostEnvironment(t)
				require.NoError(t, env.deliver(context.Background(), pv2Event(
					accAuthor, "create", replayPostRkey, testkit.TID(), replayPostCID, time.Now().UnixMicro(),
					pv2Record(accCommunity, "a post awaiting a decision", "body"))), "fixture: indexing the post")
				return env
			},
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{acceptanceEvent(accCommunity, replayPostSubject, replayPostCID, revs[2], base+1_000_000)}
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					removalEvent(accCommunity, replayPostSubject, replayPostCID, string(posts.DecisionRuleViolation), revs[1], base+2_000_000),
					acceptanceEvent(accCommunity, replayPostSubject, replayPostCID, revs[2], base+3_000_000),
				}
			},
			check: func(t *testing.T, state *moderation.CommunityState) {
				assert.Equal(t, "accepted", state.AdmissionStatus, "a stale removal must not remove the post")
			},
		},
		{
			name: "comment removal: equal-rev and older-rev replays of an applied removal", community: accCommunity,
			subject: replayCommentSubject, kind: "comment", env: newReplayCommentEnvironment,
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					replayRootEvent(revs[0], base+1_000_000),
					replayCommentEvent(revs[1], base+2_000_000),
					replayCommentRemovalEvent(revs[3], base+3_000_000),
				}
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					replayCommentRemovalEvent(revs[3], base+4_000_000),
					replayCommentRemovalEvent(revs[2], base+5_000_000),
				}
			},
			check: func(t *testing.T, state *moderation.CommunityState) {
				assert.True(t, state.CommentRemoved, "the replayed removal stays applied")
			},
		},
		{
			// The comment was re-created under another community's post, and
			// the process stopped before the held reconcile ran (see
			// TestCommunityCommentRemovalRepository_RedeliveryDemotesAMovedAppliedRemoval).
			// The same-rev redelivery demotes the removal: one change, one
			// advance, however often it is redelivered.
			name: "comment removal: a same-rev redelivery demotes a moved comment's removal once", community: accCommunity,
			subject: replayCommentSubject, kind: "comment", env: newReplayCommentEnvironment,
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					replayRootEvent(revs[0], base+1_000_000),
					replayOtherRootEvent(revs[1], base+2_000_000),
					replayCommentEvent(revs[2], base+3_000_000),
					replayCommentRemovalEvent(revs[3], base+4_000_000),
				}
			},
			between: func(t *testing.T, db *sql.DB) {
				otherRootURI := accPostURI(replayOtherRootRkey)
				_, err := db.Exec(`
					UPDATE comments SET cid = 'bafyreicsreplaymoved', root_uri = $2, root_cid = $3, parent_uri = $2, parent_cid = $3
					WHERE uri = $1`, replayCommentSubject, otherRootURI, replayOtherRootCID)
				require.NoError(t, err, "fixture: re-indexing the comment under the other community's post")
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					replayCommentRemovalEvent(revs[3], base+5_000_000),
					replayCommentRemovalEvent(revs[3], base+6_000_000),
				}
			},
			wantDelta: 1,
			check: func(t *testing.T, state *moderation.CommunityState) {
				assert.False(t, state.CommentRemoved, "the moved comment's removal is demoted")
			},
		},
		{
			name: "ban: an older update and an equal-rev create replay", community: accCommunity,
			subject: replayBanSubject, kind: "account",
			env: newReplayBanEnvironment,
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{banCommitEvent(banStep{
					repo: accCommunity, op: "create", subject: replayBanSubject, rev: revs[2],
					cid: "bafyreicsreplayban", reason: "spam",
				}, base+1_000_000)}
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					banCommitEvent(banStep{
						repo: accCommunity, op: "update", subject: replayBanSubject, rev: revs[1],
						cid: "bafyreicsreplaybanold", reason: "harassment",
					}, base+2_000_000),
					banCommitEvent(banStep{
						repo: accCommunity, op: "create", subject: replayBanSubject, rev: revs[2],
						cid: "bafyreicsreplaybanequal", reason: "harassment",
					}, base+3_000_000),
				}
			},
			check: func(t *testing.T, state *moderation.CommunityState) {
				require.NotNil(t, state.Ban, "the indexed ban stays in force")
				assert.Equal(t, "spam", state.Ban.Reason)
			},
		},
		{
			name: "roster: an older update and an equal-rev create replay", community: rosterCommunityA,
			subject: rosterCommunityA, kind: "roster", env: newReplayRosterEnvironment,
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{rosterCommitEvent(rosterStep{
					repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
					role: "moderator", createdAt: rosterTime1, rev: revs[2], cid: "bafyreicsreplayrole",
				}, base+1_000_000)}
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					rosterCommitEvent(rosterStep{
						repo: rosterCommunityA, op: "update", subject: rosterSubject1, rkey: rosterSubject1Key,
						role: "owner", createdAt: rosterTime1, rev: revs[1], cid: "bafyreicsreplayroleold",
					}, base+2_000_000),
					rosterCommitEvent(rosterStep{
						repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
						role: "owner", createdAt: rosterTime1, rev: revs[2], cid: "bafyreicsreplayroleequal",
					}, base+3_000_000),
				}
			},
			check: func(t *testing.T, state *moderation.CommunityState) {
				require.NotNil(t, state.Roster)
				assert.Nil(t, state.Roster.Owner, "a stale owner record must not take the roster")
				require.Len(t, state.Roster.Moderators, 1)
				assert.Equal(t, "did:plc:rostersubject1", state.Roster.Moderators[0].SubjectDID)
			},
		},
		{
			name: "roster: a delete at a newer rev of a role record already gone", community: rosterCommunityA,
			subject: rosterCommunityA, kind: "roster", env: newReplayRosterEnvironment,
			advance: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					rosterCommitEvent(rosterStep{
						repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
						role: "moderator", createdAt: rosterTime1, rev: revs[1], cid: "bafyreicsreplaygone",
					}, base+1_000_000),
					rosterCommitEvent(rosterStep{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revs[2]}, base+2_000_000),
				}
			},
			replay: func(revs []string, base int64) []*JetstreamEvent {
				return []*JetstreamEvent{
					rosterCommitEvent(rosterStep{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revs[3]}, base+3_000_000),
				}
			},
			check: func(t *testing.T, state *moderation.CommunityState) {
				require.NotNil(t, state.Roster)
				assert.Nil(t, state.Roster.Owner)
				assert.Empty(t, state.Roster.Moderators)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := tt.env(t)
			ctx := context.Background()
			revs := increasingTIDs(t, 4)
			base := time.Now().UnixMicro()

			for i, event := range tt.advance(revs, base) {
				require.NoErrorf(t, env.deliver(ctx, event), "advancing event %d", i)
			}
			before := readSubjectState(t, env.db, tt.community, tt.subject, tt.kind)
			require.GreaterOrEqual(t, before.Version, int64(1), "fixture: the advancing events must give the subject a version")

			if tt.between != nil {
				tt.between(t, env.db)
			}
			for i, event := range tt.replay(revs, base) {
				require.NoErrorf(t, env.deliver(ctx, event), "replayed event %d", i)
			}

			after := readSubjectState(t, env.db, tt.community, tt.subject, tt.kind)
			assert.Equal(t, before.Version+tt.wantDelta, after.Version, "the version after the replay")
			tt.check(t, after)
		})
	}
}

// B6 (c), (e): the local owner-record index call takes the record's rev gate
// first. When the firehose already handled that rev, or a newer one, it writes
// nothing and advances nothing.
func TestCommunitySubjectState_LocalRoleIndexAtRev(t *testing.T) {
	t.Parallel()

	const gateURI = "at://did:plc:rostercommunitya/social.coves.community.moderator/lk4ulvb25z3owhmahunggqgfc4aiv7a3cbmrrcuwlfdva7x5uiwa"

	tests := []struct {
		name string
		// firehose runs through the communities consumer before the local call;
		// the local call is at revs[0].
		firehose    func(revs []string) []rosterStep
		wantApplied bool
		wantCID     string // the indexed row's record CID; empty when no row
		wantGateRev func(revs []string) string
		wantVersion int64
	}{
		{
			name:        "no firehose copy yet: the local write indexes at its rev",
			firehose:    func([]string) []rosterStep { return nil },
			wantApplied: true,
			wantCID:     "bafyreicslocalowner",
			wantGateRev: func(revs []string) string { return revs[0] },
			wantVersion: 1,
		},
		{
			name: "the firehose already handled the same rev",
			firehose: func(revs []string) []rosterStep {
				return []rosterStep{{
					repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
					role: "owner", createdAt: rosterTime1, rev: revs[0], cid: "bafyreicsfirehoseowner",
				}}
			},
			wantCID:     "bafyreicsfirehoseowner",
			wantGateRev: func(revs []string) string { return revs[0] },
			wantVersion: 1,
		},
		{
			name: "the firehose deleted the record at a newer rev first",
			firehose: func(revs []string) []rosterStep {
				return []rosterStep{{repo: rosterCommunityA, op: "delete", rkey: rosterSubject2Key, rev: revs[1]}}
			},
			wantGateRev: func(revs []string) string { return revs[1] },
			wantVersion: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			consumer, roles := newRosterConsumer(t, db)
			ctx := context.Background()
			revs := increasingTIDs(t, 2)
			deliverRosterSteps(t, consumer, tt.firehose(revs))

			applied, err := roles.UpsertCommunityRoleAtRev(ctx, communities.CommunityRole{
				CommunityDID: rosterCommunityA, SubjectDID: rosterSubject2, Role: communities.RoleOwner,
				RecordKey: rosterSubject2Key, RecordCID: "bafyreicslocalowner", RecordCreatedAt: rosterT1,
			}, revs[0])
			require.NoError(t, err)
			assert.Equal(t, tt.wantApplied, applied, "whether the local index call applied")

			indexed, err := roles.ListCommunityRoles(ctx, rosterCommunityA)
			require.NoError(t, err)
			if tt.wantCID == "" {
				assert.Empty(t, indexed, "no role row may be indexed")
			} else {
				require.Len(t, indexed, 1)
				assert.Equal(t, tt.wantCID, indexed[0].RecordCID, "the indexed owner record")
				assert.Equal(t, "did:plc:rostersubject2", indexed[0].SubjectDID)
			}

			var gateRev string
			require.NoError(t, db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, gateURI).Scan(&gateRev))
			assert.Equal(t, tt.wantGateRev(revs), gateRev, "the owner record's rev gate")

			state := readSubjectState(t, db, rosterCommunityA, rosterCommunityA, "roster")
			assert.Equal(t, tt.wantVersion, state.Version, "the roster version")
		})
	}
}
