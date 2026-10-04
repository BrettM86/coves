package moderation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// maxActionIDLength bounds an action ID in the actionId filter and in a cursor.
const maxActionIDLength = 128

var errActionTargetNotFound = errors.New("action list target not found")

// permanentHandleFailures mean a handle filter names no usable account rather
// than that a resolver is down, following the OAuth login classification in
// internal/atproto/oauth/identity_failure.go.
var permanentHandleFailures = []error{
	indigoIdentity.ErrInvalidHandle, indigoIdentity.ErrHandleNotFound,
	indigoIdentity.ErrHandleMismatch, indigoIdentity.ErrHandleNotDeclared,
	indigoIdentity.ErrHandleReservedTLD, indigoIdentity.ErrDIDNotFound,
}

// HiddenActionReasons returns the reasons that suppress subject details in the public log.
func HiddenActionReasons() []string {
	return []string{illegalContentReason, doxingReason}
}

// PublicExcludedActions returns the action kinds the public log never serves.
// NSFW label applies and retractions are left out of it (user decision,
// 2026-09-30); the admin log keeps them.
func PublicExcludedActions() []string {
	return []string{ActionLabel, ActionRetractLabel}
}

func publicExcludedAction(kind string) bool {
	return slices.Contains(PublicExcludedActions(), kind)
}

func hiddenAction(action Action) bool {
	for _, reason := range HiddenActionReasons() {
		if action.Reason == reason || action.ReversedActionReason == reason {
			return true
		}
	}
	return false
}

// restrictedSubject reports a subject the public log must no longer name (PRD
// §6), because its indexed row is gone: its URI, CID and community stay out of
// the public view.
func restrictedSubject(action Action) bool {
	return action.SubjectAccess == SubjectAccessRestricted
}

// privateSubject reports an action whose subject reference is admin-only.
func privateSubject(action Action) bool {
	return hiddenAction(action) || restrictedSubject(action)
}

// ActionView is the public defs#actionView projection of an action.
type ActionView struct {
	Ref          ActionRefView   `json:"ref"`
	Action       string          `json:"action"`
	AuthorityDID string          `json:"authorityDid"`
	Scope        ScopeView       `json:"scope"`
	CreatedAt    string          `json:"createdAt"`
	Origin       string          `json:"origin"`
	Subject      *SubjectRefView `json:"subject,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	LabelValue   string          `json:"labelValue,omitempty"`
	Actor        *ActorRefView   `json:"actor,omitempty"`
	Reverses     *ActionRefView  `json:"reverses,omitempty"`
}

// ActionRefView is defs#actionRef.
type ActionRefView struct {
	ServiceDID string `json:"serviceDid"`
	ActionID   string `json:"actionId"`
}

// ScopeView is defs#scopeView.
type ScopeView struct {
	Kind         string `json:"kind"`
	CommunityDID string `json:"communityDid,omitempty"`
}

// SubjectRefView is defs#subjectRef.
type SubjectRefView struct {
	URI          string `json:"uri"`
	CID          string `json:"cid,omitempty"`
	CommunityDID string `json:"communityDid,omitempty"`
}

func newSubjectRefView(action Action) *SubjectRefView {
	return &SubjectRefView{URI: action.SubjectURI, CID: action.ObservedCID, CommunityDID: action.SubjectCommunityDID}
}

// ActorRefView is defs#actorRef.
type ActorRefView struct {
	DID string `json:"did"`
}

// AdminActionView is defs#adminActionView.
type AdminActionView struct {
	Action         ActionView      `json:"action"`
	ActorDID       string          `json:"actorDid,omitempty"`
	PrivateSubject *SubjectRefView `json:"privateSubject,omitempty"`
	PrivateNote    string          `json:"privateNote,omitempty"`
}

// NewActionView projects an action for the public log.
func NewActionView(action Action) ActionView {
	view := ActionView{
		Ref:          ActionRefView{ServiceDID: action.AuthorityDID, ActionID: action.ID},
		Action:       action.Action,
		AuthorityDID: action.AuthorityDID,
		Scope:        ScopeView{Kind: action.ScopeKind, CommunityDID: action.ScopeCommunityDID},
		CreatedAt:    action.CreatedAt.In(time.UTC).Format(time.RFC3339Nano),
		Origin:       action.Origin,
		Reason:       action.Reason,
		LabelValue:   action.LabelValue,
	}
	if !privateSubject(action) {
		view.Subject = newSubjectRefView(action)
	}
	if restrictedSubject(action) {
		// A community scope would tie the restricted subject to its community.
		view.Scope.CommunityDID = ""
	}
	if action.ActorDID != "" {
		view.Actor = &ActorRefView{DID: action.ActorDID}
	}
	if action.ReversesActionID != "" {
		view.Reverses = &ActionRefView{ServiceDID: action.AuthorityDID, ActionID: action.ReversesActionID}
	}
	return view
}

// NewAdminActionView projects an action for the admin log.
func NewAdminActionView(action Action) AdminActionView {
	view := AdminActionView{
		Action:      NewActionView(action),
		ActorDID:    action.ActorDID,
		PrivateNote: action.PrivateNote,
	}
	if privateSubject(action) {
		view.PrivateSubject = newSubjectRefView(action)
	}
	return view
}

// ListActionsParams are the listActions query parameters, as received.
type ListActionsParams struct {
	Limit      *int
	Cursor     string
	Subject    string
	Collection string
	Action     string
	Origin     string
	Authority  string
	Actor      string
	Community  string
	Since      string
	Until      string
}

// ListAdminActionsParams are the listAdminActions query parameters.
type ListAdminActionsParams struct {
	ListActionsParams
	ActionID string
}

// ActionPage is a page of the public action log.
type ActionPage struct {
	Actions []ActionView `json:"actions"`
	Cursor  string       `json:"cursor,omitempty"`
}

// AdminActionPage is a page of the admin action log.
type AdminActionPage struct {
	Actions []AdminActionView `json:"actions"`
	Cursor  string            `json:"cursor,omitempty"`
}

func (s *service) ListActions(ctx context.Context, params ListActionsParams) (*ActionPage, error) {
	actions, cursor, err := listActionPage(ctx, s, params, "", false, NewActionView)
	if err != nil {
		return nil, err
	}
	return &ActionPage{Actions: actions, Cursor: cursor}, nil
}

func (s *service) ListAdminActions(ctx context.Context, params ListAdminActionsParams) (*AdminActionPage, error) {
	actions, cursor, err := listActionPage(ctx, s, params.ListActionsParams, params.ActionID, true, NewAdminActionView)
	if err != nil {
		return nil, err
	}
	return &AdminActionPage{Actions: actions, Cursor: cursor}, nil
}

func listActionPage[T any](ctx context.Context, s *service, params ListActionsParams, actionID string, admin bool, project func(Action) T) ([]T, string, error) {
	query, digest, err := s.actionListQuery(ctx, params, actionID, admin)
	if errors.Is(err, errActionTargetNotFound) {
		if params.Cursor != "" {
			// An empty page here would end the caller's pagination silently.
			return nil, "", fmt.Errorf("%w: action list target no longer resolves", ErrInvalidCursor)
		}
		return []T{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	visibility := "public"
	if admin {
		visibility = "admin"
	}
	rows, err := s.store.ListActions(ctx, query)
	err = withCallerContext(ctx, err)
	if errors.Is(err, context.Canceled) {
		return nil, "", fmt.Errorf("list %s actions: %w", visibility, err)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: list %s actions: %w", ErrModerationUnavailable, visibility, err)
	}
	for _, row := range rows {
		if row.SubjectAccess != SubjectAccessPublic && row.SubjectAccess != SubjectAccessRestricted {
			return nil, "", fmt.Errorf("%w: list %s actions: store returned action %s with subject access %q", ErrModerationUnavailable, visibility, row.ID, row.SubjectAccess)
		}
		if query.ExcludeRestricted && restrictedSubject(row) {
			return nil, "", fmt.Errorf("%w: list %s actions: store returned restricted action %s despite ExcludeRestricted", ErrModerationUnavailable, visibility, row.ID)
		}
		if query.ExcludeHidden && hiddenAction(row) {
			return nil, "", fmt.Errorf("%w: list %s actions: store returned hidden action %s despite ExcludeHidden", ErrModerationUnavailable, visibility, row.ID)
		}
		if query.ExcludeLabelActions && publicExcludedAction(row.Action) {
			return nil, "", fmt.Errorf("%w: list %s actions: store returned %s action %s despite ExcludeLabelActions", ErrModerationUnavailable, visibility, row.Action, row.ID)
		}
	}
	pageSize := query.Limit - 1
	actions := make([]T, 0, min(len(rows), pageSize))
	for _, action := range rows[:min(len(rows), pageSize)] {
		actions = append(actions, project(action))
	}
	if len(rows) > pageSize {
		last := rows[pageSize-1]
		return actions, s.signActionCursor(admin, digest, ActionKey{CreatedAt: last.CreatedAt, ID: last.ID}), nil
	}
	return actions, "", nil
}

func (s *service) actionListQuery(ctx context.Context, params ListActionsParams, actionID string, admin bool) (ActionListQuery, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	query, err := validateActionListParams(params, actionID)
	if err != nil {
		return ActionListQuery{}, digest, err
	}
	if !admin && publicExcludedAction(params.Action) {
		return ActionListQuery{}, digest, fmt.Errorf("%w: the public log does not serve %s actions", ErrInvalidRequest, params.Action)
	}
	if s.config.CursorSecret == "" {
		return ActionListQuery{}, digest, fmt.Errorf("%w: cursor secret is not configured", ErrModerationUnavailable)
	}
	var cursor *actionCursor
	if params.Cursor != "" {
		cursor, err = decodeActionCursor(params.Cursor, admin)
		if err != nil {
			return ActionListQuery{}, digest, err
		}
	}
	for _, field := range []struct {
		name   string
		value  string
		target *string
	}{
		{"authority", params.Authority, &query.AuthorityDID},
		{"actor", params.Actor, &query.ActorDID},
	} {
		if field.value == "" || *field.target != "" {
			continue
		}
		if s.config.HandleResolver == nil {
			return ActionListQuery{}, digest, fmt.Errorf("%w: %w: %s resolver is not configured", ErrModerationUnavailable, ErrResolverUnavailable, field.name)
		}
		*field.target, _, err = s.config.HandleResolver.ResolveHandle(ctx, field.value)
		if err != nil {
			return ActionListQuery{}, digest, handleResolutionError(field.name, withCallerContext(ctx, err))
		}
		if *field.target == "" {
			return ActionListQuery{}, digest, errActionTargetNotFound
		}
	}
	if params.Community != "" && query.CommunityDID == "" {
		if s.config.CommunityResolver == nil {
			return ActionListQuery{}, digest, fmt.Errorf("%w: %w: community resolver is not configured", ErrModerationUnavailable, ErrResolverUnavailable)
		}
		query.CommunityDID, err = s.config.CommunityResolver.ResolveCommunityIdentifier(ctx, params.Community)
		if err != nil {
			err = withCallerContext(ctx, err)
			switch {
			case errors.Is(err, context.Canceled):
				return ActionListQuery{}, digest, fmt.Errorf("resolve community for action list: %w", err)
			case communities.IsNotFound(err):
				return ActionListQuery{}, digest, errActionTargetNotFound
			case communities.IsValidationError(err), errors.Is(err, communities.ErrInvalidInput), errors.Is(err, communities.ErrAmbiguousCommunity):
				return ActionListQuery{}, digest, fmt.Errorf("%w: resolve community for action list: %w", ErrInvalidRequest, err)
			default:
				return ActionListQuery{}, digest, fmt.Errorf("%w: %w: resolve community for action list: %w", ErrModerationUnavailable, ErrResolverUnavailable, err)
			}
		}
		if query.CommunityDID == "" {
			return ActionListQuery{}, digest, errActionTargetNotFound
		}
	}
	digest = s.actionFilterDigest(params, actionID, query)
	if cursor != nil {
		if err := s.verifyActionCursor(cursor, digest); err != nil {
			return ActionListQuery{}, digest, err
		}
		query.Before = &cursor.key
	}
	// A filter that selects by subject, collection or community would confirm
	// what the projection withholds, so such rows are not listed at all.
	query.ExcludeHidden = !admin && (params.Subject != "" || params.Collection != "" || params.Community != "")
	query.ExcludeRestricted = query.ExcludeHidden
	query.ExcludeLabelActions = !admin
	return query, digest, nil
}

// withCallerContext adds the caller's context error to a failed store or
// resolver call. lib/pq reports a statement cancelled mid-flight as SQLSTATE
// 57014, which wraps no context error, so a client disconnect would otherwise
// read as an outage. A blown caller deadline still classifies as unavailable.
func withCallerContext(ctx context.Context, err error) error {
	callerErr := ctx.Err()
	if err == nil || callerErr == nil || errors.Is(err, callerErr) {
		return err
	}
	return fmt.Errorf("%w: %w", callerErr, err)
}

// handleResolutionError classifies a failed authority or actor handle lookup.
func handleResolutionError(field string, err error) error {
	var notFound *identity.ErrNotFound
	var invalid *identity.ErrInvalidIdentifier
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("resolve %s for action list: %w", field, err)
	case errors.As(err, &notFound):
		return errActionTargetNotFound
	case errors.As(err, &invalid):
		return fmt.Errorf("%w: resolve %s for action list: %w", ErrInvalidRequest, field, err)
	}
	for _, permanent := range permanentHandleFailures {
		if errors.Is(err, permanent) {
			return errActionTargetNotFound
		}
	}
	return fmt.Errorf("%w: %w: resolve %s for action list: %w", ErrModerationUnavailable, ErrResolverUnavailable, field, err)
}

func validateActionListParams(params ListActionsParams, actionID string) (ActionListQuery, error) {
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 1 || limit > 100 {
		return ActionListQuery{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidRequest)
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"cursor", params.Cursor, 2048},
		{"action", params.Action, 64},
		{"origin", params.Origin, 64},
		{"community", params.Community, 320},
		{"actionId", actionID, maxActionIDLength},
	} {
		if !utf8.ValidString(field.value) || len(field.value) > field.max {
			return ActionListQuery{}, fmt.Errorf("%w: invalid %s", ErrInvalidRequest, field.name)
		}
	}
	// A control character never names a real target, and a NUL reaching
	// Postgres is an encoding error that would answer 503.
	for _, field := range []struct {
		name  string
		value string
	}{
		{"action", params.Action}, {"origin", params.Origin}, {"community", params.Community},
		{"actor", params.Actor}, {"authority", params.Authority}, {"actionId", actionID},
	} {
		if strings.ContainsFunc(field.value, unicode.IsControl) {
			return ActionListQuery{}, fmt.Errorf("%w: %s contains a control character", ErrInvalidRequest, field.name)
		}
	}
	query := ActionListQuery{
		Limit:             limit + 1,
		SubjectURI:        params.Subject,
		SubjectCollection: params.Collection,
		Action:            params.Action,
		Origin:            params.Origin,
		ActionID:          actionID,
	}
	if params.Subject != "" {
		subject, err := syntax.ParseATURI(params.Subject)
		if err != nil {
			return ActionListQuery{}, fmt.Errorf("%w: invalid subject: %v", ErrInvalidRequest, err)
		}
		// Stored subjects always carry a DID authority.
		if !subject.Authority().IsDID() {
			return ActionListQuery{}, fmt.Errorf("%w: subject authority must be a DID", ErrInvalidRequest)
		}
	}
	if params.Collection != "" {
		if _, err := syntax.ParseNSID(params.Collection); err != nil {
			return ActionListQuery{}, fmt.Errorf("%w: invalid collection: %v", ErrInvalidRequest, err)
		}
	}
	for _, field := range []struct {
		name   string
		value  string
		target *string
	}{
		{"authority", params.Authority, &query.AuthorityDID},
		{"actor", params.Actor, &query.ActorDID},
	} {
		if field.value == "" {
			continue
		}
		identifier, err := syntax.ParseAtIdentifier(field.value)
		if err != nil {
			return ActionListQuery{}, fmt.Errorf("%w: invalid %s: %v", ErrInvalidRequest, field.name, err)
		}
		if identifier.IsDID() {
			*field.target = field.value
		}
	}
	if params.Community != "" {
		if _, err := syntax.ParseDID(params.Community); err == nil {
			query.CommunityDID = params.Community
		}
	}
	for _, field := range []struct {
		name   string
		value  string
		target **time.Time
	}{
		{"since", params.Since, &query.Since},
		{"until", params.Until, &query.Until},
	} {
		if field.value == "" {
			continue
		}
		parsed, err := syntax.ParseDatetime(field.value)
		if err != nil {
			return ActionListQuery{}, fmt.Errorf("%w: invalid %s: %v", ErrInvalidRequest, field.name, err)
		}
		value := parsed.Time().UTC()
		if remainder := value.Nanosecond() % 1000; remainder != 0 {
			value = value.Add(time.Duration(1000-remainder) * time.Nanosecond)
		}
		*field.target = &value
	}
	// since is inclusive and until exclusive, so this window selects nothing.
	if query.Since != nil && query.Until != nil && !query.Since.Before(*query.Until) {
		return ActionListQuery{}, fmt.Errorf("%w: since must be before until", ErrInvalidRequest)
	}
	return query, nil
}
