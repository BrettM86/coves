package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
)

// Fakes for the community action service: an in-memory action store whose
// role index is also the authorizer's roster, and an in-memory community repo.

// communityActionRecorded is one moderation_actions row the fake store wrote.
type communityActionRecorded struct {
	ActorDID     string
	AdminActing  bool
	CommunityDID string
	SubjectDID   string
}

// communityActionLogged is one moderation_actions row with its action name
// and the invitation-aware actor (the audit actor when the intent names one).
type communityActionLogged struct {
	Action       string
	ActorDID     string
	AdminActing  bool
	CommunityDID string
	SubjectDID   string
}

type communityActionIdempotencyKey struct {
	actorDID, communityDID, key string
}

type communityActionStoredRecord struct {
	record    moderation.CommunityIdempotencyRecord
	expiresAt time.Time
}

// fakeCommunityActionStore is the store, and the role index the authorizer and
// the service read: a completed removal drops the row, so what the authorizer
// sees changes with it. Transaction effects apply only on Commit.
type fakeCommunityActionStore struct {
	mu          sync.Mutex
	roles       map[string]map[string]communities.CommunityRole // community -> rkey -> row
	versions    map[string]int64
	intents     []moderation.CommunityIntent
	idempotency map[communityActionIdempotencyKey]communityActionStoredRecord
	actions     []communityActionRecorded
	actionLog   []communityActionLogged
	invitations []communities.Invitation
	bans        map[string]map[string]communities.CommunityBan // community -> subject -> ban
	locks       []moderation.CommunityActionLock
	now         func() time.Time

	// Post actions (community_post_actions_test.go): the indexed post stand
	// state by post URI, every CommunityPost read, and the recent removal
	// counts RecentCommunityRemovals serves.
	posts                   map[string]moderation.CommunityPostState
	postReads               []string
	recentRemovalsByActor   map[string]int // actor DID -> count
	recentRemovalsCommunity map[string]int // community DID -> count

	// Comment actions (community_comment_actions_test.go): the indexed comment
	// removal state by comment URI, every CommunityComment read, and the
	// comment each seeded removal record URI is about.
	comments        map[string]moderation.CommunityCommentState
	commentReads    []string
	commentRemovals map[string]string // removal record URI -> comment URI

	// Ban actions (community_ban_actions_test.go): the (community, subject)
	// version CommunityAccountBan serves, and the recent ban counts
	// RecentCommunityBans serves.
	banVersions         map[string]int64 // community + " " + subject -> version
	recentBansByActor   map[string]int   // actor DID -> count
	recentBansCommunity map[string]int   // community DID -> count
}

func newFakeCommunityActionStore(now func() time.Time) *fakeCommunityActionStore {
	return &fakeCommunityActionStore{
		roles:       map[string]map[string]communities.CommunityRole{},
		versions:    map[string]int64{},
		idempotency: map[communityActionIdempotencyKey]communityActionStoredRecord{},
		bans:        map[string]map[string]communities.CommunityBan{},
		now:         now,

		posts:                   map[string]moderation.CommunityPostState{},
		recentRemovalsByActor:   map[string]int{},
		recentRemovalsCommunity: map[string]int{},

		comments:        map[string]moderation.CommunityCommentState{},
		commentRemovals: map[string]string{},

		banVersions:         map[string]int64{},
		recentBansByActor:   map[string]int{},
		recentBansCommunity: map[string]int{},
	}
}

func (s *fakeCommunityActionStore) seedRole(row communities.CommunityRole) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roles[row.CommunityDID] == nil {
		s.roles[row.CommunityDID] = map[string]communities.CommunityRole{}
	}
	s.roles[row.CommunityDID][row.RecordKey] = row
}

// dropRole removes the index row at (community, rkey), as an external delete
// indexed from the firehose would.
func (s *fakeCommunityActionStore) dropRole(communityDID, rkey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles[communityDID], rkey)
}

// role returns the index row at (community, rkey).
func (s *fakeCommunityActionStore) role(communityDID, rkey string) (communities.CommunityRole, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.roles[communityDID][rkey]
	return row, ok
}

func (s *fakeCommunityActionStore) setVersion(communityDID string, version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[communityDID] = version
}

func (s *fakeCommunityActionStore) seedInvitation(inv communities.Invitation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invitations = append(s.invitations, inv)
}

func (s *fakeCommunityActionStore) invitationsSnapshot() []communities.Invitation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]communities.Invitation(nil), s.invitations...)
}

// invitationState is the stored state of the invitation, "" when unknown.
func (s *fakeCommunityActionStore) invitationState(id string) communities.InvitationState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inv := range s.invitations {
		if inv.ID == id {
			return inv.State
		}
	}
	return ""
}

func (s *fakeCommunityActionStore) actionLogSnapshot() []communityActionLogged {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]communityActionLogged(nil), s.actionLog...)
}

// InvitationByID reads the invitation outside any transaction, or nil.
func (s *fakeCommunityActionStore) InvitationByID(_ context.Context, invitationID string) (*communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invitationLocked(invitationID), nil
}

func (s *fakeCommunityActionStore) invitationLocked(invitationID string) *communities.Invitation {
	for _, inv := range s.invitations {
		if inv.ID == invitationID {
			copied := inv
			return &copied
		}
	}
	return nil
}

// pendingLocked lists the open, unlapsed invitations matching keep, newest
// first.
func (s *fakeCommunityActionStore) pendingLocked(now time.Time, keep func(communities.Invitation) bool) []communities.Invitation {
	var out []communities.Invitation
	for i := len(s.invitations) - 1; i >= 0; i-- {
		inv := s.invitations[i]
		if inv.State == communities.InvitationOpen && now.Before(inv.ExpiresAt) && keep(inv) {
			out = append(out, inv)
		}
	}
	return out
}

// ListPendingInvitationsForCommunity serves communities.InvitationReader.
func (s *fakeCommunityActionStore) ListPendingInvitationsForCommunity(_ context.Context, communityDID string, now time.Time) ([]communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked(now, func(inv communities.Invitation) bool { return inv.CommunityDID == communityDID }), nil
}

// ListPendingInvitationsForInvitee serves communities.InvitationReader.
func (s *fakeCommunityActionStore) ListPendingInvitationsForInvitee(_ context.Context, inviteeDID string, now time.Time) ([]communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked(now, func(inv communities.Invitation) bool { return inv.InviteeDID == inviteeDID }), nil
}

// PendingInvitation serves communities.InvitationReader.
func (s *fakeCommunityActionStore) PendingInvitation(_ context.Context, communityDID, inviteeDID string, now time.Time) (*communities.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pendingLocked(now, func(inv communities.Invitation) bool {
		return inv.CommunityDID == communityDID && inv.InviteeDID == inviteeDID
	})
	if len(pending) == 0 {
		return nil, nil
	}
	return &pending[0], nil
}

// ListCommunityRoles serves the community's indexed role rows.
func (s *fakeCommunityActionStore) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []communities.CommunityRole
	for _, row := range s.roles[communityDID] {
		out = append(out, row)
	}
	return out, nil
}

// ListRolesBySubject serves the subject's indexed role rows.
func (s *fakeCommunityActionStore) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []communities.CommunityRole
	for _, rows := range s.roles {
		for _, row := range rows {
			if row.SubjectDID == subjectDID {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// hasRole reports whether the index holds a row at (community, rkey).
func (s *fakeCommunityActionStore) hasRole(communityDID, rkey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.roles[communityDID][rkey]
	return ok
}

func (s *fakeCommunityActionStore) version(communityDID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versions[communityDID]
}

func (s *fakeCommunityActionStore) intentsSnapshot() []moderation.CommunityIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]moderation.CommunityIntent(nil), s.intents...)
}

// expireIntents moves every open intent's expiry to at.
func (s *fakeCommunityActionStore) expireIntents(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.intents {
		if s.intents[i].State == moderation.CommunityIntentOpen {
			s.intents[i].ExpiresAt = at
		}
	}
}

func (s *fakeCommunityActionStore) actionsSnapshot() []communityActionRecorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]communityActionRecorded(nil), s.actions...)
}

// checkIntentForWrite is the repo's view of the store when a write arrives:
// every write must have an open intent on its target guarded by swapCommit.
func (s *fakeCommunityActionStore) checkIntentForWrite(repoDID string, writes []pds.Write, swapCommit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range writes {
		target := "at://" + repoDID + "/" + write.Collection + "/" + write.RKey
		found := false
		for _, intent := range s.intents {
			onTarget := intent.TargetURI == target || (intent.PairedTargetURI != "" && intent.PairedTargetURI == target)
			if onTarget && intent.State == moderation.CommunityIntentOpen && intent.GuardCommitCID == swapCommit {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("no open intent on %s guarded by %q when the write arrived", target, swapCommit)
		}
	}
	return nil
}

func (s *fakeCommunityActionStore) BeginAction(_ context.Context, lock moderation.CommunityActionLock) (moderation.CommunityActionTx, error) {
	if lock.CommunityDID == "" {
		return nil, errors.New("fake store: BeginAction without a community")
	}
	s.mu.Lock()
	s.locks = append(s.locks, lock)
	s.mu.Unlock()
	return &fakeCommunityActionTx{store: s}, nil
}

// SaveIntent stores a new intent. The caller assigns its ID; at most one open
// intent may exist per target URI.
func (s *fakeCommunityActionStore) SaveIntent(_ context.Context, intent moderation.CommunityIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent.ID == "" {
		return errors.New("fake store: intent without an ID")
	}
	for _, existing := range s.intents {
		if existing.ID == intent.ID {
			return fmt.Errorf("fake store: duplicate intent ID %s", intent.ID)
		}
		if existing.TargetURI == intent.TargetURI && existing.State == moderation.CommunityIntentOpen {
			return fmt.Errorf("%w: fake store: a second open intent on %s", moderation.ErrStateConflict, intent.TargetURI)
		}
	}
	s.intents = append(s.intents, intent)
	return nil
}

func (s *fakeCommunityActionStore) ExpiredOpenIntents(_ context.Context, now time.Time, after *moderation.CommunityIntent, limit int) ([]moderation.CommunityIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []moderation.CommunityIntent
	for _, intent := range s.intents {
		if intent.State == moderation.CommunityIntentOpen && !now.Before(intent.ExpiresAt) {
			out = append(out, intent)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(out[j].ExpiresAt)
		}
		return out[i].ID < out[j].ID
	})
	if after != nil {
		start := sort.Search(len(out), func(i int) bool {
			return out[i].ExpiresAt.After(after.ExpiresAt) ||
				(out[i].ExpiresAt.Equal(after.ExpiresAt) && out[i].ID > after.ID)
		})
		out = out[start:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeCommunityActionTx stages its writes and applies them on Commit.
type fakeCommunityActionTx struct {
	store   *fakeCommunityActionStore
	pending []func()
	done    bool
}

func (tx *fakeCommunityActionTx) IdempotencyRecord(_ context.Context, actorDID, communityDID, key string, now time.Time) (*moderation.CommunityIdempotencyRecord, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	stored, ok := tx.store.idempotency[communityActionIdempotencyKey{actorDID, communityDID, key}]
	if !ok || !now.Before(stored.expiresAt) {
		return nil, nil
	}
	record := stored.record
	return &record, nil
}

func (tx *fakeCommunityActionTx) PendingRoleRecordDeleteCID(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (tx *fakeCommunityActionTx) IntentByKey(_ context.Context, actorDID, communityDID, key string) (*moderation.CommunityIntent, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for i := len(tx.store.intents) - 1; i >= 0; i-- {
		intent := tx.store.intents[i]
		if intent.ActorDID == actorDID && intent.CommunityDID == communityDID && intent.IdempotencyKey == key {
			return &intent, nil
		}
	}
	return nil, nil
}

func (tx *fakeCommunityActionTx) OpenIntentByTarget(_ context.Context, targetURI string) (*moderation.CommunityIntent, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for _, intent := range tx.store.intents {
		if intent.TargetURI == targetURI && intent.State == moderation.CommunityIntentOpen {
			return &intent, nil
		}
	}
	return nil, nil
}

func (tx *fakeCommunityActionTx) OpenIntentByTargetUnlocked(ctx context.Context, targetURI string) (*moderation.CommunityIntent, error) {
	return tx.OpenIntentByTarget(ctx, targetURI)
}

func (tx *fakeCommunityActionTx) OpenIntentByAction(_ context.Context, communityDID, action string) (*moderation.CommunityIntent, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for _, intent := range tx.store.intents {
		if intent.CommunityDID == communityDID && intent.Action == action && intent.State == moderation.CommunityIntentOpen {
			return &intent, nil
		}
	}
	return nil, nil
}

func (tx *fakeCommunityActionTx) OpenInvitationIntent(_ context.Context, communityDID, inviteeDID string) (*moderation.CommunityIntent, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for _, intent := range tx.store.intents {
		if intent.CommunityDID == communityDID && intent.SubjectDID == inviteeDID && intent.InvitationID != "" &&
			intent.State == moderation.CommunityIntentOpen {
			return &intent, nil
		}
	}
	return nil, nil
}

// ListCommunityRoles reads the store's role index, as the store's own read.
func (tx *fakeCommunityActionTx) ListCommunityRoles(ctx context.Context, communityDID string) ([]communities.CommunityRole, error) {
	return tx.store.ListCommunityRoles(ctx, communityDID)
}

func (tx *fakeCommunityActionTx) RosterVersion(_ context.Context, communityDID string) (int64, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	return tx.store.versions[communityDID], nil
}

// CompleteIntent drops the index row if it still has the replaced CID,
// advances the roster version, writes one action, completes the intent and
// saves the idempotency record — all on Commit.
func (tx *fakeCommunityActionTx) CompleteIntent(_ context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	if commit.Rev == "" {
		return moderation.CommunityActionResult{}, errors.New("fake store: completing an intent without a commit rev")
	}
	index := -1
	for i, stored := range tx.store.intents {
		if stored.ID == intent.ID {
			index = i
		}
	}
	if index < 0 || tx.store.intents[index].State != moderation.CommunityIntentOpen {
		return moderation.CommunityActionResult{}, fmt.Errorf("fake store: no open intent %q to complete", intent.ID)
	}
	stored := tx.store.intents[index]
	if isFakeBanIntent(stored) {
		return tx.completeBanIntentLocked(index, commit, idempotencyTTL)
	}
	if commentURI, ok := tx.store.commentRemovals[stored.TargetURI]; ok {
		return tx.completeCommentIntentLocked(index, commentURI, commit, idempotencyTTL)
	}
	if isFakePostIntent(stored) {
		return tx.completePostIntentLocked(index, commit, idempotencyTTL)
	}
	actionID := fmt.Sprintf("community-action-%d", len(tx.store.actions)+1)
	result := moderation.CommunityActionResult{
		CommunityDID: stored.CommunityDID,
		SubjectDID:   stored.SubjectDID,
		RecordURI:    stored.TargetURI,
		ActionID:     actionID,
		Version:      tx.store.versions[stored.CommunityDID] + 1,
		InvitationID: stored.InvitationID,
	}
	rolePrefix := "at://" + stored.CommunityDID + "/" + communities.RoleCollection + "/"
	rkey := stored.TargetURI[len(rolePrefix):]
	// A create or update indexes the written body with the commit's CID; a
	// transfer's paired write indexes the previous owner's rewritten record.
	var upserts []communities.CommunityRole
	if stored.Op != string(pds.WriteOpDelete) {
		row, err := fakeIndexedRoleRow(stored.CommunityDID, rkey, stored.Record, commit.RecordCID)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		upserts = append(upserts, row)
	}
	if stored.PairedTargetURI != "" {
		row, err := fakeIndexedRoleRow(stored.CommunityDID, stored.PairedTargetURI[len(rolePrefix):], stored.PairedRecord, commit.PairedRecordCID)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		upserts = append(upserts, row)
	}
	actorDID := stored.ActorDID
	if stored.AuditActorDID != "" {
		actorDID = stored.AuditActorDID
	}
	tx.pending = append(tx.pending, func() {
		if stored.Op == string(pds.WriteOpDelete) {
			if row, ok := tx.store.roles[stored.CommunityDID][rkey]; ok && row.RecordCID == stored.ReplacedCID {
				delete(tx.store.roles[stored.CommunityDID], rkey)
			}
		}
		for _, row := range upserts {
			if tx.store.roles[row.CommunityDID] == nil {
				tx.store.roles[row.CommunityDID] = map[string]communities.CommunityRole{}
			}
			tx.store.roles[row.CommunityDID][row.RecordKey] = row
		}
		if stored.InvitationID != "" {
			for i := range tx.store.invitations {
				if tx.store.invitations[i].ID == stored.InvitationID && tx.store.invitations[i].State == communities.InvitationOpen {
					tx.store.invitations[i].State = communities.InvitationAccepted
				}
			}
		}
		tx.store.versions[stored.CommunityDID] = result.Version
		tx.store.actions = append(tx.store.actions, communityActionRecorded{
			ActorDID: actorDID, AdminActing: stored.AdminActing,
			CommunityDID: stored.CommunityDID, SubjectDID: stored.SubjectDID,
		})
		tx.store.actionLog = append(tx.store.actionLog, communityActionLogged{
			Action: stored.Action, ActorDID: actorDID, AdminActing: stored.AdminActing,
			CommunityDID: stored.CommunityDID, SubjectDID: stored.SubjectDID,
		})
		completed := tx.store.intents[index]
		completed.State = moderation.CommunityIntentCompleted
		completed.CommitRev = commit.Rev
		completed.RecordCID = commit.RecordCID
		completed.ActionID = actionID
		tx.store.intents[index] = completed
		tx.store.idempotency[communityActionIdempotencyKey{stored.ActorDID, stored.CommunityDID, stored.IdempotencyKey}] = communityActionStoredRecord{
			record:    moderation.CommunityIdempotencyRecord{Fingerprint: stored.Fingerprint, Result: result},
			expiresAt: tx.store.now().Add(idempotencyTTL),
		}
	})
	return result, nil
}

// fakeIndexedRoleRow is the index row a written role record body produces.
func fakeIndexedRoleRow(communityDID, rkey string, body json.RawMessage, cid string) (communities.CommunityRole, error) {
	if cid == "" {
		return communities.CommunityRole{}, fmt.Errorf("fake store: completing a write of %s without its record CID", rkey)
	}
	var record struct {
		Subject   string `json:"subject"`
		Role      string `json:"role"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return communities.CommunityRole{}, fmt.Errorf("fake store: decoding the intent's record body: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if err != nil {
		return communities.CommunityRole{}, fmt.Errorf("fake store: the intent's record createdAt: %w", err)
	}
	return communities.CommunityRole{
		CommunityDID: communityDID, SubjectDID: record.Subject, Role: record.Role,
		RecordKey: rkey, RecordCID: cid, RecordCreatedAt: created,
	}, nil
}

func (tx *fakeCommunityActionTx) InvitationByID(_ context.Context, invitationID string) (*communities.Invitation, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	return tx.store.invitationLocked(invitationID), nil
}

// CreateInvitation supersedes the invitee's open invitation in the community
// and inserts inv, on Commit.
func (tx *fakeCommunityActionTx) CreateInvitation(_ context.Context, inv communities.Invitation) error {
	if inv.ID == "" {
		return errors.New("fake store: invitation without an ID")
	}
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	if tx.store.invitationLocked(inv.ID) != nil {
		return fmt.Errorf("fake store: duplicate invitation ID %s", inv.ID)
	}
	tx.pending = append(tx.pending, func() {
		for i, existing := range tx.store.invitations {
			if existing.CommunityDID == inv.CommunityDID && existing.InviteeDID == inv.InviteeDID && existing.State == communities.InvitationOpen {
				tx.store.invitations[i].State = communities.InvitationSuperseded
			}
		}
		tx.store.invitations = append(tx.store.invitations, inv)
	})
	return nil
}

func (tx *fakeCommunityActionTx) SetInvitationState(_ context.Context, invitationID string, state communities.InvitationState) error {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	if inv := tx.store.invitationLocked(invitationID); inv == nil || inv.State != communities.InvitationOpen {
		return fmt.Errorf("fake store: no open invitation %q", invitationID)
	}
	tx.pending = append(tx.pending, func() {
		for i := range tx.store.invitations {
			if tx.store.invitations[i].ID == invitationID {
				tx.store.invitations[i].State = state
			}
		}
	})
	return nil
}

func (tx *fakeCommunityActionTx) SaveIdempotencyRecord(_ context.Context, actorDID, communityDID, key string, record moderation.CommunityIdempotencyRecord, ttl time.Duration) error {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	expiresAt := tx.store.now().Add(ttl)
	tx.pending = append(tx.pending, func() {
		tx.store.idempotency[communityActionIdempotencyKey{actorDID, communityDID, key}] = communityActionStoredRecord{record: record, expiresAt: expiresAt}
	})
	return nil
}

// GetCommunityBan serves the ban index as communities.BanReader does.
func (tx *fakeCommunityActionTx) GetCommunityBan(_ context.Context, communityDID, subjectDID string) (*communities.CommunityBan, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	ban, ok := tx.store.bans[communityDID][subjectDID]
	if !ok {
		return nil, communities.ErrCommunityBanNotFound
	}
	return &ban, nil
}

// CommunityComment serves the store's indexed comment removal state and
// records the read.
func (tx *fakeCommunityActionTx) CommunityComment(_ context.Context, commentURI string) (moderation.CommunityCommentState, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	tx.store.commentReads = append(tx.store.commentReads, commentURI)
	state, ok := tx.store.comments[commentURI]
	if !ok {
		return moderation.CommunityCommentState{}, moderation.ErrCommentNotFound
	}
	return state, nil
}

// InstanceOutboxWroteRemoval: the fake store has no instance removal outbox,
// so no removal record is the outbox's.
func (tx *fakeCommunityActionTx) InstanceOutboxWroteRemoval(context.Context, string, posts.StandingRemoval) (bool, error) {
	return false, nil
}

func (tx *fakeCommunityActionTx) FinishIntent(_ context.Context, intentID string, state moderation.CommunityIntentState) error {
	if state != moderation.CommunityIntentFailed && state != moderation.CommunityIntentExpired {
		return fmt.Errorf("fake store: FinishIntent to %q", state)
	}
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for i, stored := range tx.store.intents {
		if stored.ID == intentID && stored.State == moderation.CommunityIntentOpen {
			tx.pending = append(tx.pending, func() { tx.store.intents[i].State = state })
			return nil
		}
	}
	return fmt.Errorf("fake store: no open intent %q to finish", intentID)
}

func (tx *fakeCommunityActionTx) Commit() error {
	if tx.done {
		return errors.New("fake store: transaction already finished")
	}
	tx.done = true
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	for _, apply := range tx.pending {
		apply()
	}
	return nil
}

func (tx *fakeCommunityActionTx) Rollback() error {
	tx.done = true
	tx.pending = nil
	return nil
}

// rosterBatch is one ApplyWrites call the fake repo received.
type rosterBatch struct {
	writes     []pds.Write
	swapCommit string
}

// fakeRosterRepo is one community's repo. A successful batch moves the head to
// commitHead.
type fakeRosterRepo struct {
	mu      sync.Mutex
	did     string
	records map[string]*pds.RecordResponse // collection/rkey
	head    pds.LatestCommit

	commitHead pds.LatestCommit

	getRecordErr error
	// applyErr fails the next batch before it commits; applyThenErr commits the
	// next batch and then returns the error (a lost response). Each is consumed
	// by one call.
	applyErr     error
	applyThenErr error
	// afterHeadRead runs once, after the next GetLatestCommit returns.
	afterHeadRead func(r *fakeRosterRepo)

	// writeCIDs is the CID a create or update of collection/rkey produces.
	writeCIDs map[string]string

	// checkWrite is the store's view when a batch arrives.
	checkWrite func(repoDID string, writes []pds.Write, swapCommit string) error

	batches    []rosterBatch
	violations []string
	// guardedDeletes is every DeleteRecordWithSwap call received.
	guardedDeletes []rosterGuardedDelete
}

// rosterGuardedDelete is one DeleteRecordWithSwap call the fake repo received.
type rosterGuardedDelete struct {
	Collection, RKey, SwapRecord string
}

func (r *fakeRosterRepo) setRecord(collection, rkey, cid string) {
	r.records[collection+"/"+rkey] = &pds.RecordResponse{
		URI: "at://" + r.did + "/" + collection + "/" + rkey,
		CID: cid,
		Value: map[string]any{
			"$type": communities.RoleCollection,
		},
	}
}

func (r *fakeRosterRepo) hasRecord(collection, rkey string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.records[collection+"/"+rkey]
	return ok
}

// landCommit moves the head without touching any role record.
func (r *fakeRosterRepo) landCommit(head pds.LatestCommit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = head
}

func (r *fakeRosterRepo) batchesSnapshot() []rosterBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rosterBatch(nil), r.batches...)
}

func (r *fakeRosterRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getRecordErr != nil {
		return nil, r.getRecordErr
	}
	record, ok := r.records[collection+"/"+rkey]
	if !ok {
		return nil, fmt.Errorf("getRecord: %w", pds.ErrNotFound)
	}
	copied := *record
	return &copied, nil
}

func (r *fakeRosterRepo) PutRecordWithCommit(context.Context, string, string, any, string) (*pds.RecordCommit, error) {
	return nil, errors.New("fake roster repo: PutRecordWithCommit is not part of a roster removal")
}

func (r *fakeRosterRepo) PutRecordGuardedByCommit(context.Context, string, string, any, string, string) (*pds.RecordCommit, error) {
	return nil, errors.New("fake roster repo: PutRecordGuardedByCommit is not part of a roster removal")
}

func (r *fakeRosterRepo) ApplyWrites(_ context.Context, writes []pds.Write, swapCommit string) (*pds.ApplyWritesResult, error) {
	if r.checkWrite != nil {
		if err := r.checkWrite(r.did, writes, swapCommit); err != nil {
			r.mu.Lock()
			r.violations = append(r.violations, err.Error())
			r.mu.Unlock()
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, rosterBatch{writes: append([]pds.Write(nil), writes...), swapCommit: swapCommit})
	if swapCommit != r.head.CID {
		return nil, fmt.Errorf("applyWrites: %w", pds.ErrSwapConflict)
	}
	if r.applyErr != nil {
		err := r.applyErr
		r.applyErr = nil
		return nil, err
	}
	results := make([]pds.WriteResult, len(writes))
	values := make([]map[string]any, len(writes))
	for i, write := range writes {
		path := write.Collection + "/" + write.RKey
		_, exists := r.records[path]
		results[i] = pds.WriteResult{Op: write.Op}
		switch write.Op {
		case pds.WriteOpDelete:
			if !exists {
				return nil, fmt.Errorf("applyWrites: delete of a missing record: %w", pds.ErrNotFound)
			}
			continue
		case pds.WriteOpCreate:
			if exists {
				return nil, fmt.Errorf("fake roster repo: create of an existing record %s", path)
			}
		case pds.WriteOpUpdate:
			if !exists {
				return nil, fmt.Errorf("applyWrites: update of a missing record: %w", pds.ErrNotFound)
			}
		default:
			return nil, fmt.Errorf("fake roster repo: unexpected %s", write.Op)
		}
		cid, ok := r.writeCIDs[path]
		if !ok {
			return nil, fmt.Errorf("fake roster repo: no CID configured for a write of %s", path)
		}
		encoded, err := json.Marshal(write.Record)
		if err != nil {
			return nil, fmt.Errorf("fake roster repo: encoding the record of %s: %w", path, err)
		}
		var value map[string]any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("fake roster repo: the record of %s is not an object: %w", path, err)
		}
		values[i] = value
		results[i].URI = "at://" + r.did + "/" + path
		results[i].CID = cid
	}
	for i, write := range writes {
		path := write.Collection + "/" + write.RKey
		if write.Op == pds.WriteOpDelete {
			delete(r.records, path)
			continue
		}
		r.records[path] = &pds.RecordResponse{URI: results[i].URI, CID: results[i].CID, Value: values[i]}
	}
	r.head = r.commitHead
	if r.applyThenErr != nil {
		err := r.applyThenErr
		r.applyThenErr = nil
		return nil, err
	}
	return &pds.ApplyWritesResult{CommitRev: r.head.Rev, CommitCID: r.head.CID, Results: results}, nil
}

// DeleteRecordWithSwap deletes collection/rkey only while it carries
// swapRecord: a different CID is ErrSwapConflict and a missing record
// ErrNotFound, as the PDS answers.
func (r *fakeRosterRepo) DeleteRecordWithSwap(_ context.Context, collection, rkey, swapRecord string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guardedDeletes = append(r.guardedDeletes, rosterGuardedDelete{Collection: collection, RKey: rkey, SwapRecord: swapRecord})
	path := collection + "/" + rkey
	record, ok := r.records[path]
	if !ok {
		return fmt.Errorf("deleteRecord: %w", pds.ErrNotFound)
	}
	if swapRecord == "" || record.CID != swapRecord {
		return fmt.Errorf("deleteRecord: %w", pds.ErrSwapConflict)
	}
	delete(r.records, path)
	return nil
}

func (r *fakeRosterRepo) GetLatestCommit(_ context.Context) (*pds.LatestCommit, error) {
	r.mu.Lock()
	head := r.head
	hook := r.afterHeadRead
	r.afterHeadRead = nil
	r.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	return &head, nil
}

func (r *fakeRosterRepo) DID() string { return r.did }

// rosterRepoFactory opens the repo for its community and refuses any other
// DID; it records every DID it was asked to open.
type rosterRepoFactory struct {
	mu     sync.Mutex
	repo   *fakeRosterRepo
	opened []string
}

func (f *rosterRepoFactory) open(_ context.Context, communityDID string) (posts.CommunityRepo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, communityDID)
	if communityDID != f.repo.did {
		return nil, fmt.Errorf("no credentials for %s", communityDID)
	}
	return f.repo, nil
}

func (f *rosterRepoFactory) openedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}
