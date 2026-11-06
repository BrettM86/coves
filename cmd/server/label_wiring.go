package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/config"
	"Coves/internal/core/labels"
	postgresRepo "Coves/internal/db/postgres"
)

// buildLabelPublisher builds and starts the label publisher when
// LABEL_SIGNING_KEY is set, and returns nil when label publication is
// disabled. Start runs the signing key check, any acknowledged rotation and the
// one-time export before the HTTP listener starts; its failure fails boot.
func buildLabelPublisher(ctx context.Context, cfg *config.Config, db *sql.DB) (*labels.Publisher, error) {
	if cfg.Labels.SigningKey == nil {
		// Once publication was on (a label is stored or the export has
		// completed, even with no decision to export), its labels must stay
		// signed and served: turning publication off would silently stop
		// serving them.
		err := labels.RequireNoLabelPublication(ctx, postgresRepo.NewLabelRepository(db))
		if errors.Is(err, labels.ErrSigningKeyRequired) {
			return nil, fmt.Errorf("label publication is off but labels were published at an earlier boot; configure LABEL_SIGNING_KEY: %w", err)
		}
		if err != nil {
			return nil, fmt.Errorf("checking for published labels at boot: %w", err)
		}
		return nil, nil
	}
	publisher, err := labels.NewPublisher(labels.Config{
		SourceDID:       cfg.Instance.DID,
		LabelerEndpoint: cfg.OAuth.PublicURL,
		AtprotoKey:      cfg.Instance.AtprotoPublicKey,
		Key:             cfg.Labels.SigningKey,
		RetiringKey:     cfg.Labels.RetiringKey,
	})
	if err != nil {
		return nil, fmt.Errorf("building the label publisher: %w", err)
	}
	if err := publisher.Start(ctx, postgresRepo.NewLabelRepository(db)); err != nil {
		return nil, fmt.Errorf("starting the label publisher: %w", err)
	}
	return publisher, nil
}
