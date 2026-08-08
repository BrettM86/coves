package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The admitPost decision matrix (docs/PRD_AUTHOR_OWNED_POSTS.md §4.1, §5.6, §8).
//
// admitPost is a pure function over injected lookups and an injected clock, so
// every branch is reachable here without Postgres, without a PDS, and without
// waiting for a rate-limit window to roll. That is the point of the extraction:
// the same checks used to be interleaved with blob uploads and PDS writes
// inside CreatePost, where the only way to reach the private-community branch
// was to provision a private community on a real PDS.
//
// The outer contract — that CreatePost actually consults this, against real
// rows, in the right place in its flow — is service_admission_test.go. What is
// proven HERE is the policy itself, at the width the policy has.
//
// THE LEDGER FAKE IS A MODEL, NOT A RECORDER. stubLedger enforces the same
// unique key the real table does and answers CountSince from the same rows it
// accepted, so "ten admitted then the eleventh refused" is a genuine boundary
// crossing rather than a canned answer. A recorder-shaped fake would let an
// implementation that never consulted the count pass every case below.

const (
	admitAuthorDID       = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	admitAggregatorDID   = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
	admitCommunityDID    = "did:plc:cccccccccccccccccccccccc"
	admitCommunityHandle = "!gardening.communities.coves.social"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// stubCommunities answers with one community, or with whichever failure it was
// handed. It counts its calls so a test can prove a later check never ran.
type stubCommunities struct {
	community  *communities.Community
	resolveErr error
	getErr     error

	resolveCalls int
	getCalls     int
}

func (s *stubCommunities) ResolveCommunityIdentifier(_ context.Context, _ string) (string, error) {
	s.resolveCalls++
	if s.resolveErr != nil {
		return "", s.resolveErr
	}
	return s.community.DID, nil
}

func (s *stubCommunities) GetByDID(_ context.Context, _ string) (*communities.Community, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.community, nil
}

// stubBans is the community_memberships lookup. Its default — the zero value —
// is the ordinary case: no membership row, because posting in a public
// community has never required joining it.
type stubBans struct {
	membership *communities.Membership
	err        error

	calls          int
	lastIdentifier string
	lastAuthorDID  string
}

func (s *stubBans) GetMembership(_ context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	s.calls++
	s.lastAuthorDID = userDID
	s.lastIdentifier = communityIdentifier
	if s.err != nil {
		return nil, s.err
	}
	if s.membership == nil {
		return nil, communities.ErrMembershipNotFound
	}
	return s.membership, nil
}

// stubAggregatorAuthorizer stands in for aggregators.Service, whose own
// authorization and hourly-quota rules have their own tests.
type stubAggregatorAuthorizer struct {
	err   error
	calls int
}

func (s *stubAggregatorAuthorizer) ValidateAggregatorPost(_ context.Context, _, _ string) error {
	s.calls++
	return s.err
}

// ledgerRow is one live reservation.
type ledgerRow struct {
	id  int64
	cmd ReserveSubmissionCommand
	at  time.Time
}

// stubLedger models post_submissions in memory: the same unique key, the same
// rolling-window count, and Release genuinely removing the row.
type stubLedger struct {
	now Clock

	rows   []ledgerRow
	nextID int64

	// reserveErr and countErr force the infrastructure-failure paths. A
	// duplicate is NOT set this way — it emerges from the unique key, like it
	// does in Postgres.
	reserveErr error
	countErr   error

	reserveCalls []ReserveSubmissionCommand
	releaseCalls []SubmissionReservation
}

func (l *stubLedger) Reserve(_ context.Context, cmd ReserveSubmissionCommand) (SubmissionReservation, error) {
	l.reserveCalls = append(l.reserveCalls, cmd)
	if l.reserveErr != nil {
		return SubmissionReservation{}, l.reserveErr
	}
	for _, row := range l.rows {
		if row.cmd == cmd {
			return SubmissionReservation{}, ErrDuplicateSubmission
		}
	}
	l.nextID++
	l.rows = append(l.rows, ledgerRow{id: l.nextID, cmd: cmd, at: l.now()})
	return SubmissionReservation{ID: l.nextID}, nil
}

func (l *stubLedger) Release(_ context.Context, reservation SubmissionReservation) error {
	l.releaseCalls = append(l.releaseCalls, reservation)
	kept := l.rows[:0]
	for _, row := range l.rows {
		if row.id != reservation.ID {
			kept = append(kept, row)
		}
	}
	l.rows = kept
	return nil
}

func (l *stubLedger) CountSince(_ context.Context, authorDID, communityDID string, since time.Time) (int, error) {
	if l.countErr != nil {
		return 0, l.countErr
	}
	count := 0
	for _, row := range l.rows {
		if row.cmd.AuthorDID == authorDID && row.cmd.CommunityDID == communityDID && !row.at.Before(since) {
			count++
		}
	}
	return count, nil
}

// liveRows is what the ledger holds after the decision — the assertion behind
// "a refusal consumes no quota".
func (l *stubLedger) liveRows() int { return len(l.rows) }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// admitHarness is the default world: a public community, an author with no
// membership row, an authorized aggregator, an empty ledger, and a clock that
// only moves when a test moves it.
type admitHarness struct {
	communities *stubCommunities
	bans        *stubBans
	aggregators *stubAggregatorAuthorizer
	ledger      *stubLedger
	limits      SubmissionLimits
	now         time.Time
}

func newAdmitHarness() *admitHarness {
	h := &admitHarness{
		communities: &stubCommunities{community: &communities.Community{
			DID:        admitCommunityDID,
			Handle:     admitCommunityHandle,
			Visibility: "public",
		}},
		bans:        &stubBans{},
		aggregators: &stubAggregatorAuthorizer{},
		limits: SubmissionLimits{
			MaxPerAuthorPerCommunity: 3,
			Window:                   time.Hour,
			DedupeWindow:             time.Hour,
		},
		now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	}
	h.ledger = &stubLedger{now: h.clock()}
	return h
}

// clock hands out a Clock that reads the harness's mutable instant, so
// advancing time after the ledger was built still moves the ledger's clock.
func (h *admitHarness) clock() Clock {
	return func() time.Time { return h.now }
}

func (h *admitHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *admitHarness) deps() admissionDeps {
	return admissionDeps{
		communities: h.communities,
		bans:        h.bans,
		aggregators: h.aggregators,
		ledger:      h.ledger,
		limits:      h.limits,
		now:         h.clock(),
	}
}

// admit runs the decision for a user submitting `fingerprint`.
func (h *admitHarness) admit(t *testing.T, actor ActorClass, fingerprint string) (AdmissionDecision, error) {
	t.Helper()
	authorDID := admitAuthorDID
	if actor != ActorUser {
		authorDID = admitAggregatorDID
	}
	return admitPost(context.Background(), h.deps(), AdmissionRequest{
		Actor:       actor,
		AuthorDID:   authorDID,
		Community:   admitCommunityHandle,
		Fingerprint: fingerprint,
	})
}

// banned is a membership row with the ban flag set.
func banned() *communities.Membership {
	return &communities.Membership{
		UserDID:      admitAuthorDID,
		CommunityDID: admitCommunityDID,
		IsBanned:     true,
	}
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

func TestAdmitPost_DecisionMatrix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		actor    ActorClass
		setup    func(*admitHarness)
		wantCode DecisionCode
		why      string
	}{
		{
			name:  "a community nobody has indexed",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.communities.resolveErr = communities.ErrCommunityNotFound
			},
			wantCode: DecisionCommunityNotFound,
			why:      "nothing else can be evaluated against a community that does not exist",
		},
		{
			name:  "an identifier that resolves to a community the index has since lost",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.communities.getErr = communities.ErrCommunityNotFound
			},
			wantCode: DecisionCommunityNotFound,
			why:      "resolution and fetch are two lookups, and either failing to find it is the same answer to the client",
		},
		{
			name:  "a regular user submitting to a private community",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.communities.community.Visibility = "private"
			},
			wantCode: DecisionCommunityPrivate,
			why:      "Alpha admits public and unlisted only; membership for private communities is Beta",
		},
		{
			name:  "an unlisted community is not a private one",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.communities.community.Visibility = "unlisted"
			},
			wantCode: "",
			why:      "unlisted means undiscoverable, not closed — only 'private' blocks a submission",
		},
		{
			name:  "a BANNED user of a PRIVATE community",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.communities.community.Visibility = "private"
				h.bans.membership = banned()
			},
			wantCode: DecisionCommunityPrivate,
			why: "a ban must not be disclosed through a privacy wall: answering author-banned would " +
				"confirm to an outsider both that the community exists and that a moderator has acted on them",
		},
		{
			name:  "a banned member of a public community",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.bans.membership = banned()
			},
			wantCode: DecisionAuthorBanned,
			why:      "the check §4.1 admits does not exist yet, and this is it",
		},
		{
			name:  "a member in good standing",
			actor: ActorUser,
			setup: func(h *admitHarness) {
				h.bans.membership = &communities.Membership{
					UserDID: admitAuthorDID, CommunityDID: admitCommunityDID, IsBanned: false,
				}
			},
			wantCode: "",
			why:      "a membership row is not itself a refusal",
		},
		{
			name:     "a non-member of a public community",
			actor:    ActorUser,
			setup:    func(h *admitHarness) { h.bans.membership = nil },
			wantCode: "",
			why: "ErrMembershipNotFound is a VALUE meaning 'not banned' — posting in a public " +
				"community has never required joining it, so the absent row is the common case",
		},
		{
			name:  "a registered aggregator the community never authorized",
			actor: ActorRegisteredAggregator,
			setup: func(h *admitHarness) {
				h.aggregators.err = aggregators.ErrNotAuthorized
			},
			wantCode: DecisionAggregatorNotAuthorized,
			why:      "existing semantics: being registered with the instance is not permission to write anywhere",
		},
		{
			name:  "a registered aggregator over its OWN hourly quota",
			actor: ActorRegisteredAggregator,
			setup: func(h *admitHarness) {
				h.aggregators.err = aggregators.ErrRateLimitExceeded
			},
			wantCode: DecisionAggregatorNotAuthorized,
			why:      "the aggregator limiter answers through the same call; the sentinel it carries is what tells 403 from 429",
		},
		{
			name:  "a registered aggregator submitting into a PRIVATE community",
			actor: ActorRegisteredAggregator,
			setup: func(h *admitHarness) {
				h.communities.community.Visibility = "private"
			},
			wantCode: "",
			why: "aggregators are authorized services rather than members, so visibility says nothing " +
				"about them — this is today's behaviour and the extraction must not change it",
		},
		{
			name:  "a trusted aggregator submitting into a PRIVATE community it is banned from",
			actor: ActorTrustedAggregator,
			setup: func(h *admitHarness) {
				h.communities.community.Visibility = "private"
				h.bans.membership = banned()
				h.aggregators.err = aggregators.ErrNotAuthorized
			},
			wantCode: "",
			why:      "a trusted aggregator skips visibility, ban and authorization — all three, deliberately",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			tc.setup(h)

			decision, err := h.admit(t, tc.actor, "fingerprint-1")
			require.NoErrorf(t, err, "a policy refusal is a decision, not an error: %s", tc.why)

			assert.Equalf(t, tc.wantCode, decision.Code, "%s", tc.why)
			assert.Equalf(t, tc.wantCode == "", decision.Admitted(),
				"Admitted() must agree with the code it is derived from")

			if decision.Admitted() {
				require.NotNil(t, decision.Community,
					"an admission carries the resolved community so CreatePost does not fetch it a second time")
				assert.Equal(t, admitCommunityDID, decision.Community.DID)
				require.NotNil(t, decision.Reservation,
					"an admission carries the ledger row it reserved, so a failed PDS write can release it")
				assert.Equal(t, 1, h.ledger.liveRows())
				return
			}

			// The other half of every refusal: it cost nothing.
			assert.Zerof(t, h.ledger.liveRows(),
				"a refused submission left a ledger row behind, so it burned quota it was never granted")
		})
	}
}

// ---------------------------------------------------------------------------
// Check order
// ---------------------------------------------------------------------------

// The nondisclosure rule is not just about the CODE returned — the ban lookup
// must not run at all. A private community that queried moderation state before
// refusing would still leak through timing, and would make a banned outsider's
// probe indistinguishable from a member's in the logs.
func TestAdmitPost_PrivateCommunityNeverConsultsModerationState(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.communities.community.Visibility = "private"
	h.bans.membership = banned()

	decision, err := h.admit(t, ActorUser, "probe")
	require.NoError(t, err)

	require.Equal(t, DecisionCommunityPrivate, decision.Code)
	assert.Zero(t, h.bans.calls,
		"the privacy wall must refuse before moderation state is read, not after")
}

// A community that does not resolve short-circuits everything downstream. An
// implementation that gathered every input before deciding would issue a ban
// lookup, an authorization check and a ledger insert against a community DID it
// had just failed to find.
func TestAdmitPost_AnAbsentCommunityStopsEveryLaterCheck(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.communities.resolveErr = communities.ErrCommunityNotFound

	decision, err := h.admit(t, ActorUser, "probe")
	require.NoError(t, err)
	require.Equal(t, DecisionCommunityNotFound, decision.Code)

	assert.Zero(t, h.bans.calls, "a ban lookup ran against a community that does not exist")
	assert.Zero(t, h.aggregators.calls, "an authorization check ran against a community that does not exist")
	assert.Empty(t, h.ledger.reserveCalls, "a ledger row was reserved against a community that does not exist")
}

// The ban lookup is scoped to the RESOLVED community, not to whatever
// at-identifier the client happened to send. Handles are mutable; a ban keyed
// by handle stops applying the moment a community renames itself.
func TestAdmitPost_BanIsLookedUpByResolvedDID(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	_, err := h.admit(t, ActorUser, "probe")
	require.NoError(t, err)

	require.Equal(t, 1, h.bans.calls)
	assert.Equal(t, admitCommunityDID, h.bans.lastIdentifier,
		"the ban must be looked up against the resolved DID: a handle is mutable, and a ban keyed to one stops applying at rename")
	assert.Equal(t, admitAuthorDID, h.bans.lastAuthorDID)
}

// Neither aggregator class is subject to the ban lookup. Registered and trusted
// aggregators are services rather than members; there is no membership row to
// find, and asking for one on every syndicated item is a query per post for an
// answer that is structurally always the same.
func TestAdmitPost_AggregatorsAreNotBanChecked(t *testing.T) {
	t.Parallel()

	for _, actor := range []ActorClass{ActorRegisteredAggregator, ActorTrustedAggregator} {
		t.Run(string(actor), func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			h.bans.membership = banned()

			decision, err := h.admit(t, actor, "syndicated")
			require.NoError(t, err)

			assert.True(t, decision.Admitted(), "an aggregator was refused by a membership rule that does not apply to it")
			assert.Zero(t, h.bans.calls, "the ban lookup ran for an actor class that has no membership")
		})
	}
}

// Only registered aggregators meet the authorization check. A trusted one is
// authorized by configuration, and a regular user has no aggregator identity to
// check at all.
func TestAdmitPost_OnlyRegisteredAggregatorsAreAuthorizationChecked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		actor     ActorClass
		wantCalls int
	}{
		{ActorUser, 0},
		{ActorRegisteredAggregator, 1},
		{ActorTrustedAggregator, 0},
	} {
		t.Run(string(tc.actor), func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			_, err := h.admit(t, tc.actor, "item")
			require.NoError(t, err)

			assert.Equal(t, tc.wantCalls, h.aggregators.calls)
		})
	}
}

// An aggregator refusal must carry the aggregators-package sentinel that
// caused it.
//
// The two refusals share one DecisionCode but mean opposite things to a machine
// client: 403 says stop asking, 429 says ask later. The API boundary tells them
// apart with aggregators.IsUnauthorized and aggregators.IsRateLimited over the
// error CreatePost returns (internal/api/handlers/post/errors.go), so the
// sentinel has to survive the decision. Collapsed into a bare code, a
// well-behaved aggregator would retry a permanent refusal forever.
func TestAdmitPost_AnAggregatorRefusalKeepsItsSentinel(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []error{aggregators.ErrNotAuthorized, aggregators.ErrRateLimitExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			h.aggregators.err = fmt.Errorf("aggregators: %w", sentinel)

			decision, err := h.admit(t, ActorRegisteredAggregator, "item")
			require.NoError(t, err)
			require.Equal(t, DecisionAggregatorNotAuthorized, decision.Code)

			require.NotNil(t, decision.Cause,
				"the refusal dropped the aggregator sentinel, so the boundary cannot tell 403 from 429")
			assert.ErrorIs(t, decision.Cause, sentinel)
		})
	}
}

// ---------------------------------------------------------------------------
// Failing closed
// ---------------------------------------------------------------------------

// A ban lookup that fails for any reason OTHER than "no such membership" must
// fail the request.
//
// This is the single most consequential line in the whole decision. Treating an
// unreachable database as "not banned" would turn a Postgres blip into a global
// unban for its duration — every banned author in every community able to post
// again, with nothing in the logs but a warning. The safe direction is to
// refuse a submission we cannot evaluate.
func TestAdmitPost_ABanLookupFailureFailsTheRequest(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.bans.err = errors.New("dial tcp 10.0.0.4:5432: connect: connection refused")

	decision, err := h.admit(t, ActorUser, "probe")

	require.Error(t, err, "an unevaluable ban check must fail the request, never fall through to admitted")
	assert.False(t, decision.Admitted())
	assert.Empty(t, h.ledger.reserveCalls, "a submission we could not evaluate must not reserve quota")
}

// The infrastructure failures that are NOT policy answers. Each is a decision
// that could not be made, which is a different thing from a refusal — and a
// caller that mapped them to a 4xx would tell a client its perfectly good
// request was its own fault.
func TestAdmitPost_InfrastructureFailuresAreErrorsNotRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(*admitHarness)
	}{
		{
			name:  "the community index is unreachable",
			setup: func(h *admitHarness) { h.communities.resolveErr = errors.New("connection reset by peer") },
		},
		{
			name:  "the community fetch fails after resolution succeeded",
			setup: func(h *admitHarness) { h.communities.getErr = errors.New("connection reset by peer") },
		},
		{
			name:  "the ledger insert fails for a reason that is not a duplicate",
			setup: func(h *admitHarness) { h.ledger.reserveErr = errors.New("deadlock detected") },
		},
		{
			name:  "the quota count fails",
			setup: func(h *admitHarness) { h.ledger.countErr = errors.New("statement timeout") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			tc.setup(h)

			decision, err := h.admit(t, ActorUser, "probe")
			require.Error(t, err)
			assert.False(t, decision.Admitted(),
				"a decision that could not be made must not read as an admission")
			assert.Emptyf(t, decision.Code,
				"an infrastructure failure must not be dressed up as a policy code (%q); the client would be told to stop retrying something that will work in a second", decision.Code)
		})
	}
}

// A malformed community identifier is a client error, and it has to stay one:
// the API boundary turns a validation error into a 400 naming the bad field,
// while an unclassified error becomes an opaque 500.
func TestAdmitPost_AMalformedIdentifierStaysAValidationError(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.communities.resolveErr = communities.NewValidationError("community", "handle must start with !")

	_, err := h.admit(t, ActorUser, "probe")
	require.Error(t, err)
	assert.True(t, IsValidationError(err),
		"a malformed identifier must reach the boundary as a validation error, not as an unclassified failure: %v", err)
}

// A quota refusal must leave the ledger exactly as it found it. The reservation
// is inserted before the count is taken — that is what closes the concurrent
// double-tap — so the refusal path is responsible for taking it back out.
// Without that, an author who kept retrying past their limit would extend their
// own lockout with every attempt.
func TestAdmitPost_ARateLimitRefusalReleasesItsOwnReservation(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	for i := 0; i < h.limits.MaxPerAuthorPerCommunity; i++ {
		decision, err := h.admit(t, ActorUser, fmt.Sprintf("inside-quota-%d", i))
		require.NoError(t, err)
		require.Truef(t, decision.Admitted(), "submission %d of %d was refused inside the quota with %q",
			i+1, h.limits.MaxPerAuthorPerCommunity, decision.Code)
	}

	decision, err := h.admit(t, ActorUser, "one-too-many")
	require.NoError(t, err)
	require.Equal(t, DecisionRateLimitExceeded, decision.Code)

	assert.Equal(t, h.limits.MaxPerAuthorPerCommunity, h.ledger.liveRows(),
		"the refused submission left its reservation on the ledger, so retrying extends the author's own lockout")
	assert.NotEmpty(t, h.ledger.releaseCalls,
		"the rate-limit path must release the reservation it took before counting")
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

// The boundary itself: N admitted, N+1 refused. Asserting only "the fourth
// fails" would pass against an implementation that refused the third too.
func TestAdmitPost_ExactlyTheLimitIsAdmitted(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.MaxPerAuthorPerCommunity = 5

	for i := 0; i < 5; i++ {
		decision, err := h.admit(t, ActorUser, fmt.Sprintf("item-%d", i))
		require.NoError(t, err)
		assert.Truef(t, decision.Admitted(),
			"submission %d of 5 was refused with %q, so the limit is being applied one short", i+1, decision.Code)
	}

	decision, err := h.admit(t, ActorUser, "item-5")
	require.NoError(t, err)
	assert.Equal(t, DecisionRateLimitExceeded, decision.Code,
		"the sixth submission must be refused, or the limit is being applied one too generously")
}

// The window is ROLLING, not a bucket that empties on the hour: rows age out
// individually, so an author who filled their quota gets one slot back exactly
// one window after the submission that took it — not all of them at a
// boundary, which would let them spend a double quota either side of it.
func TestAdmitPost_QuotaRecoversAsTheWindowRolls(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.MaxPerAuthorPerCommunity = 2
	h.limits.Window = time.Hour

	first, err := h.admit(t, ActorUser, "first")
	require.NoError(t, err)
	require.True(t, first.Admitted())

	h.advance(30 * time.Minute)
	second, err := h.admit(t, ActorUser, "second")
	require.NoError(t, err)
	require.True(t, second.Admitted())

	third, err := h.admit(t, ActorUser, "third")
	require.NoError(t, err)
	require.Equal(t, DecisionRateLimitExceeded, third.Code, "the quota is two and both are inside the window")

	// Cross the window relative to the FIRST submission only. One slot frees;
	// the second submission is still 30 minutes inside the window.
	h.advance(31 * time.Minute)
	fourth, err := h.admit(t, ActorUser, "fourth")
	require.NoError(t, err)
	assert.True(t, fourth.Admitted(),
		"the first submission has aged out of the rolling window, so its slot must be available again")

	fifth, err := h.admit(t, ActorUser, "fifth")
	require.NoError(t, err)
	assert.Equal(t, DecisionRateLimitExceeded, fifth.Code,
		"only ONE slot aged out; a window that emptied wholesale would admit this too")
}

// Neither aggregator class is metered by the new per-author limit.
//
// A trusted aggregator has no submission limit today and inventing one here
// would be a production behaviour change smuggled in under a refactor. A
// registered one is already metered by its own hourly quota inside
// ValidateAggregatorPost, and counting it twice would silently halve the
// throughput every authorized aggregator was granted.
func TestAdmitPost_AggregatorsAreNotMeteredByTheNewLimit(t *testing.T) {
	t.Parallel()

	for _, actor := range []ActorClass{ActorRegisteredAggregator, ActorTrustedAggregator} {
		t.Run(string(actor), func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()
			h.limits.MaxPerAuthorPerCommunity = 2

			for i := 0; i < 5; i++ {
				decision, err := h.admit(t, actor, fmt.Sprintf("syndicated-%d", i))
				require.NoError(t, err)
				assert.Truef(t, decision.Admitted(),
					"item %d was refused with %q; %s is not subject to the per-author limit", i+1, decision.Code, actor)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

// Every actor class deduplicates. An RSS aggregator re-polling a feed and
// resubmitting an identical item is the canonical case, so exempting trusted
// actors would exempt precisely the traffic this check exists for.
func TestAdmitPost_IdenticalResubmissionIsRefusedForEveryActorClass(t *testing.T) {
	t.Parallel()

	for _, actor := range []ActorClass{ActorUser, ActorRegisteredAggregator, ActorTrustedAggregator} {
		t.Run(string(actor), func(t *testing.T) {
			t.Parallel()

			h := newAdmitHarness()

			first, err := h.admit(t, actor, "the same item")
			require.NoError(t, err)
			require.True(t, first.Admitted())

			second, err := h.admit(t, actor, "the same item")
			require.NoError(t, err)
			assert.Equal(t, DecisionDuplicateSubmission, second.Code,
				"an identical resubmission must be refused as a repeat")
			assert.Equal(t, 1, h.ledger.liveRows(),
				"the refused duplicate must not add a second row")
		})
	}
}

// Dedupe runs BEFORE the rate limit, and this is the case that tells them
// apart: an author already at their quota who retries something they have
// already sent. If the order were reversed, a client whose response was lost
// would be told to slow down when what actually happened is that its post
// already exists — and its retry would have burned a quota slot for a post it
// did not make.
func TestAdmitPost_DedupeAnswersBeforeTheQuotaDoes(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.MaxPerAuthorPerCommunity = 2

	for i := 0; i < 2; i++ {
		decision, err := h.admit(t, ActorUser, fmt.Sprintf("item-%d", i))
		require.NoError(t, err)
		require.True(t, decision.Admitted())
	}

	// At quota AND a repeat. Both refusals apply; dedupe is the honest one.
	decision, err := h.admit(t, ActorUser, "item-0")
	require.NoError(t, err)
	assert.Equal(t, DecisionDuplicateSubmission, decision.Code,
		"a retry of an already-accepted submission must be reported as a repeat, not as a quota breach")
}

// Dedupe expires. The ledger's unique key is scoped to a window bucket
// precisely so that "do not accept the same thing twice right now" does not
// silently become "this author may never post this content again".
func TestAdmitPost_DedupeExpiresWithItsWindow(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.DedupeWindow = time.Hour
	h.limits.MaxPerAuthorPerCommunity = 10

	first, err := h.admit(t, ActorUser, "a link worth reposting")
	require.NoError(t, err)
	require.True(t, first.Admitted())

	h.advance(2 * time.Hour)

	second, err := h.admit(t, ActorUser, "a link worth reposting")
	require.NoError(t, err)
	assert.True(t, second.Admitted(),
		"identical content a dedupe window later is a repost, not a duplicate submission")

	require.Len(t, h.ledger.reserveCalls, 2)
	assert.NotEqual(t, h.ledger.reserveCalls[0].DedupeBucket, h.ledger.reserveCalls[1].DedupeBucket,
		"the two submissions must fall in different dedupe buckets, or the unique key would still collide")
}

// Two submissions inside one window share a bucket — the other half of the
// property above, and the one that actually makes the unique key bite.
func TestAdmitPost_SubmissionsInsideOneWindowShareABucket(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.DedupeWindow = time.Hour

	_, err := h.admit(t, ActorUser, "first")
	require.NoError(t, err)

	h.advance(5 * time.Minute)
	_, err = h.admit(t, ActorUser, "second")
	require.NoError(t, err)

	require.Len(t, h.ledger.reserveCalls, 2)
	assert.Equal(t, h.ledger.reserveCalls[0].DedupeBucket, h.ledger.reserveCalls[1].DedupeBucket,
		"submissions five minutes apart in an hourly window must share a bucket")
}

// What the ledger is actually asked to store. The fingerprint is the client's
// content hash and must arrive unmodified, and the community must be the
// resolved DID for the same reason the ban lookup is.
func TestAdmitPost_TheReservationDescribesTheSubmission(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	_, err := h.admit(t, ActorUser, "sha256-of-the-canonical-record")
	require.NoError(t, err)

	require.Len(t, h.ledger.reserveCalls, 1)
	reserved := h.ledger.reserveCalls[0]
	assert.Equal(t, admitAuthorDID, reserved.AuthorDID)
	assert.Equal(t, admitCommunityDID, reserved.CommunityDID,
		"the ledger is keyed by resolved DID; a handle would let a rename reset both the quota and the dedupe key")
	assert.Equal(t, "sha256-of-the-canonical-record", reserved.Fingerprint)
}

// Quota and dedupe are per (author, community): a user at their limit in one
// community must still be able to post in another. A limit that leaked across
// communities would make one busy community silence its author everywhere.
func TestAdmitPost_QuotaIsScopedToOneCommunity(t *testing.T) {
	t.Parallel()

	h := newAdmitHarness()
	h.limits.MaxPerAuthorPerCommunity = 1

	first, err := h.admit(t, ActorUser, "item")
	require.NoError(t, err)
	require.True(t, first.Admitted())

	refused, err := h.admit(t, ActorUser, "another item")
	require.NoError(t, err)
	require.Equal(t, DecisionRateLimitExceeded, refused.Code)

	// The same author, the same content, a different community.
	h.communities.community = &communities.Community{
		DID:        "did:plc:dddddddddddddddddddddddd",
		Handle:     "!woodworking.communities.coves.social",
		Visibility: "public",
	}

	elsewhere, err := h.admit(t, ActorUser, "item")
	require.NoError(t, err)
	assert.True(t, elsewhere.Admitted(),
		"the quota is per community; being at the limit in one must not silence the author in another")
}

// ---------------------------------------------------------------------------
// The sentinel's wording
// ---------------------------------------------------------------------------

// IsConflict classifies by substring, so a duplicate SUBMISSION worded like a
// duplicate KEY would be misread as an indexing conflict — a post refused at
// the admission gate reported to its author as one that already exists.
func TestErrDuplicateSubmissionIsNotAStorageConflict(t *testing.T) {
	t.Parallel()

	assert.False(t, IsConflict(ErrDuplicateSubmission),
		"ErrDuplicateSubmission's message collides with IsConflict's substring match (%q); reword the sentinel",
		ErrDuplicateSubmission.Error())
	assert.False(t, IsConflict(fmt.Errorf("createPost: %w", ErrDuplicateSubmission)),
		"the wrapped form must not be misclassified either — that is the shape the boundary actually sees")
}

// ---------------------------------------------------------------------------
// Fingerprint
// ---------------------------------------------------------------------------

// The fingerprint is what makes two submissions "identical". createdAt is
// stamped per attempt, so including it would make every retry look new and
// dedupe would never fire; everything a moderator would judge must be included,
// or two genuinely different posts would collide and the second would be
// refused as a repeat of the first.
func TestSubmissionFingerprint(t *testing.T) {
	t.Parallel()

	base := func() PostRecord {
		title, content := "A title", "Some body text"
		return PostRecord{
			Type:      "social.coves.community.postv2",
			Community: admitCommunityDID,
			Author:    admitAuthorDID,
			Title:     &title,
			Content:   &content,
			CreatedAt: "2026-08-01T12:00:00Z",
		}
	}

	t.Run("createdAt is excluded", func(t *testing.T) {
		t.Parallel()

		later := base()
		later.CreatedAt = "2026-08-01T12:00:09Z"

		assert.Equal(t, submissionFingerprint(base()), submissionFingerprint(later),
			"the server stamps createdAt per attempt, so a fingerprint that included it would never match a retry")
	})

	t.Run("a non-empty fingerprint", func(t *testing.T) {
		t.Parallel()

		assert.NotEmpty(t, submissionFingerprint(base()),
			"an empty fingerprint would make every submission collide with every other")
	})

	for _, tc := range []struct {
		field  string
		mutate func(*PostRecord)
	}{
		{"title", func(r *PostRecord) { title := "A different title"; r.Title = &title }},
		{"content", func(r *PostRecord) { content := "Different body text"; r.Content = &content }},
		{"community", func(r *PostRecord) { r.Community = "did:plc:dddddddddddddddddddddddd" }},
		{"author", func(r *PostRecord) { r.Author = "did:plc:eeeeeeeeeeeeeeeeeeeeeeee" }},
		{"embed", func(r *PostRecord) {
			r.Embed = map[string]interface{}{"$type": "social.coves.embed.external"}
		}},
	} {
		t.Run("a different "+tc.field+" is a different submission", func(t *testing.T) {
			t.Parallel()

			changed := base()
			tc.mutate(&changed)
			assert.NotEqual(t, submissionFingerprint(base()), submissionFingerprint(changed),
				"two posts differing in %s would collide, and the second would be refused as a repeat of the first", tc.field)
		})
	}
}
