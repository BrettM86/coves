package notifications

import (
	"context"
	"fmt"
	"time"
)

// Service serves the notification read endpoints.
type Service interface {
	CountUnread(ctx context.Context, recipientDID string) (int, error)
	UpdateSeen(ctx context.Context, did string, seenAt time.Time) error
}

type service struct{ repo ReadRepository }

// NewService builds the notification service.
func NewService(repo ReadRepository) Service { return service{repo: repo} }

func (s service) CountUnread(ctx context.Context, recipientDID string) (int, error) {
	count, err := s.repo.CountUnread(ctx, recipientDID)
	if err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}

func (s service) UpdateSeen(ctx context.Context, did string, seenAt time.Time) error {
	return s.repo.UpdateSeen(ctx, did, seenAt)
}

// PreferencesService serves getPreferences and putPreferences.
type PreferencesService interface {
	GetPreferences(ctx context.Context, did string) (Preferences, error)
	PutPreferences(ctx context.Context, did string, update PreferencesUpdate) (Preferences, error)
}

type preferencesService struct{ repo PreferencesRepository }

// NewPreferencesService builds the notification preferences service.
func NewPreferencesService(repo PreferencesRepository) PreferencesService {
	return preferencesService{repo: repo}
}

func (s preferencesService) GetPreferences(ctx context.Context, did string) (Preferences, error) {
	preferences, err := s.repo.GetPreferences(ctx, did)
	if err != nil {
		return Preferences{}, fmt.Errorf("get notification preferences: %w", err)
	}
	return preferences, nil
}

func (s preferencesService) PutPreferences(ctx context.Context, did string, update PreferencesUpdate) (Preferences, error) {
	preferences, err := s.repo.PutPreferences(ctx, did, update)
	if err != nil {
		return Preferences{}, fmt.Errorf("put notification preferences: %w", err)
	}
	return preferences, nil
}
