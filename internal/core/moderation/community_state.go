package moderation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"Coves/internal/core/communities"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// CommunitySubjectKind is the kind of a community moderation subject:
// "post", "comment", "account" or "roster".
type CommunitySubjectKind string

// The community moderation subject kinds.
const (
	CommunitySubjectPost    CommunitySubjectKind = "post"
	CommunitySubjectComment CommunitySubjectKind = "comment"
	CommunitySubjectAccount CommunitySubjectKind = "account"
	CommunitySubjectRoster  CommunitySubjectKind = "roster"
)

// CommunityStateRequest asks for a subject's community moderation state.
type CommunityStateRequest struct {
	ViewerDID    string
	Subject      string
	CommunityDID string
}

// CommunityState is a subject's community moderation state and version.
type CommunityState struct {
	CommunityDID    string
	Subject         string
	Kind            CommunitySubjectKind
	Version         int64
	AdmissionStatus string                    // post kind; "none" when no admission row
	CommentRemoved  bool                      // comment kind
	Ban             *communities.CommunityBan // account kind; nil when no in-force ban
	Roster          *communities.Roster       // roster kind
}

// CommunityContentResolver resolves a post or comment URI to its community.
type CommunityContentResolver interface {
	ContentCommunity(ctx context.Context, contentURI string) (string, bool, error)
}

// CommunitySubjectStateReader reads a subject's community state and version.
type CommunitySubjectStateReader interface {
	ReadCommunitySubjectState(ctx context.Context, communityDID, subject string, kind CommunitySubjectKind) (*CommunityState, error)
}

// CommunityStateService serves social.coves.moderation.getCommunityState.
type CommunityStateService struct {
	resolver   CommunityContentResolver
	authorizer communities.RoleAuthorizer
	reader     CommunitySubjectStateReader
}

// NewCommunityStateService builds the service. Every argument is required.
func NewCommunityStateService(resolver CommunityContentResolver, authorizer communities.RoleAuthorizer, reader CommunitySubjectStateReader) *CommunityStateService {
	if resolver == nil || authorizer == nil || reader == nil {
		panic("moderation.NewCommunityStateService: resolver, authorizer and reader are required")
	}
	return &CommunityStateService{resolver: resolver, authorizer: authorizer, reader: reader}
}

// GetCommunityState returns the subject's community state to a moderator or
// owner of the subject's community, or to an instance admin. A post or comment
// at-URI is authorized in the community its index row names, never the request
// community; a DID subject is authorized in the request community, and is the
// roster when it is that community's own DID.
func (s *CommunityStateService) GetCommunityState(ctx context.Context, req CommunityStateRequest) (*CommunityState, error) {
	if req.ViewerDID == "" {
		return nil, ErrAuthRequired
	}
	communityDID, kind, err := s.resolveSubject(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.authorizer.Authorize(ctx, communityDID, req.ViewerDID, communities.RoleModerator); err != nil {
		if errors.Is(err, communities.ErrUnauthorized) || errors.Is(err, communities.ErrCommunityRemoved) {
			return nil, fmt.Errorf("%w: %w", ErrForbidden, err)
		}
		return nil, fmt.Errorf("%w: authorizing the community state read: %w", ErrModerationUnavailable, err)
	}
	state, err := s.reader.ReadCommunitySubjectState(ctx, communityDID, req.Subject, kind)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the community state of %s: %w", ErrModerationUnavailable, req.Subject, err)
	}
	return state, nil
}

// resolveSubject returns the community that governs the subject and its kind.
func (s *CommunityStateService) resolveSubject(ctx context.Context, req CommunityStateRequest) (string, CommunitySubjectKind, error) {
	if strings.HasPrefix(req.Subject, "at://") {
		kind, err := contentSubjectKind(req.Subject)
		if err != nil {
			return "", "", err
		}
		communityDID, found, err := s.resolver.ContentCommunity(ctx, req.Subject)
		if err != nil {
			return "", "", fmt.Errorf("%w: resolving the community of %s: %w", ErrModerationUnavailable, req.Subject, err)
		}
		if !found {
			return "", "", fmt.Errorf("%w: %s is not indexed in a community", ErrInvalidSubject, req.Subject)
		}
		return communityDID, kind, nil
	}
	if _, err := syntax.ParseDID(req.Subject); err != nil {
		return "", "", fmt.Errorf("%w: subject must be a post or comment at-URI or a DID", ErrInvalidSubject)
	}
	if req.CommunityDID == "" {
		return "", "", fmt.Errorf("%w: community is required for a DID subject", ErrInvalidRequest)
	}
	if _, err := syntax.ParseDID(req.CommunityDID); err != nil {
		return "", "", fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	if req.Subject == req.CommunityDID {
		return req.CommunityDID, CommunitySubjectRoster, nil
	}
	return req.CommunityDID, CommunitySubjectAccount, nil
}

// contentSubjectKind is the kind of a post or comment record at-URI.
func contentSubjectKind(subject string) (CommunitySubjectKind, error) {
	uri, err := syntax.ParseATURI(subject)
	if err != nil {
		return "", fmt.Errorf("%w: invalid subject URI: %w", ErrInvalidSubject, err)
	}
	if uri.RecordKey().String() == "" {
		return "", fmt.Errorf("%w: subject URI names no record", ErrInvalidSubject)
	}
	switch uri.Collection().String() {
	case CommentCollection:
		return CommunitySubjectComment, nil
	case PostV2Collection, LegacyPostCollection:
		return CommunitySubjectPost, nil
	default:
		return "", fmt.Errorf("%w: unsupported subject collection", ErrInvalidSubject)
	}
}
