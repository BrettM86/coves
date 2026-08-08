package main

import (
	"testing"

	"Coves/internal/config"
	postgresRepo "Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

// The production decider must be wired with its firehose quota counter, or the §8
// abuse control is silently OFF (whole-branch review, P2).
//
// decider.go's applyQuota short-circuits — allowing UNLIMITED admissions — when
// DeciderDeps.Admissions is nil. The wiring built the decider without setting
// that field, so on the live instance every direct-PDS-write / remote-author post
// (ActorUser) got unlimited admission: the per-author-per-community submission
// quota that tasks 3 and 5 built exists in code and does nothing in production.
//
// This pins the WIRING, not the decider (which enforces the quota correctly once
// given the counter): buildDeciderDeps must carry the admissions repo the
// ingestion consumer and engine already share, so the same rows that count as
// admitted are the rows the quota meters.

func TestBuildDeciderDeps_WiresTheFirehoseQuotaCounter(t *testing.T) {
	// A non-nil AdmissionRepository is all this needs — the test never queries it,
	// it only asserts the wiring PASSES it through. NewAdmissionRepository(nil)
	// wraps a nil DB without touching it, so no infrastructure is required.
	admissionRepo := postgresRepo.NewAdmissionRepository(nil)

	a := &application{
		admissionRepo: admissionRepo,
		cfg:           &config.Config{},
	}

	deps := a.buildDeciderDeps()

	require.NotNilf(t, deps.Admissions,
		"buildDeciderDeps left DeciderDeps.Admissions nil, so applyQuota short-circuits and the §8 firehose per-author-per-community quota is DISABLED in production: "+
			"any authenticated author can write unlimited postv2 records naming any community and each is admitted. Set Admissions: a.admissionRepo (the same counter the ingestion consumer writes and the engine settles).")
}
