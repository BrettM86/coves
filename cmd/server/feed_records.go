package main

import (
	"context"
	"log/slog"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/feedgen"
)

// feedRecordSyncTimeout bounds the instance PDS login plus the built-in
// generator record writes at startup, so a PDS that accepts the connection
// but never answers cannot hang the boot.
const feedRecordSyncTimeout = 30 * time.Second

// instanceRecordLogin logs into the instance's own PDS account and returns a
// client that reads and writes records in that account's repo.
type instanceRecordLogin func(ctx context.Context, host, handle, password string) (feedgen.RecordClient, error)

// passwordRecordLogin is the production instanceRecordLogin: one
// createSession with the instance credentials. pdsOptions carries the
// private-host hatch a dev or CI PDS on a private address needs.
func passwordRecordLogin(pdsOptions ...pds.ClientOption) instanceRecordLogin {
	return func(ctx context.Context, host, handle, password string) (feedgen.RecordClient, error) {
		return pds.NewFromPasswordAuth(ctx, host, handle, password, pdsOptions...)
	}
}

// syncBuiltinFeedRecords writes the built-in feed generator records into the
// instance PDS account's repo.
//
// It never stops the boot. The served feed at-uris come from INSTANCE_DID,
// not from this login, so getFeed works whether or not the records are
// published. A missing credential, a failed login or a failed record read or
// write logs an ERROR and returns; the records are synced on the next
// restart. The password and session tokens are never logged.
func syncBuiltinFeedRecords(ctx context.Context, pdsCfg config.PDSConfig, login instanceRecordLogin, registry *feedgen.Registry, now time.Time) {
	if !pdsCfg.HasInstanceCredentials() {
		slog.Error("PDS_INSTANCE_HANDLE / PDS_INSTANCE_PASSWORD not set; "+
			"built-in feed generator records are not published to the instance PDS account",
			"pds_url", pdsCfg.URL)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, feedRecordSyncTimeout)
	defer cancel()

	client, err := login(ctx, pdsCfg.URL, pdsCfg.InstanceHandle, pdsCfg.InstancePassword)
	if err != nil {
		slog.Error("instance PDS login failed; built-in feed generator records not synced until next restart",
			"pds_url", pdsCfg.URL,
			"instance_handle", pdsCfg.InstanceHandle,
			"error", err,
		)
		return
	}

	if err := feedgen.EnsureBuiltinRecords(ctx, client, registry, now); err != nil {
		slog.Error("built-in feed generator record sync failed; retried on next restart",
			"pds_url", pdsCfg.URL,
			"instance_handle", pdsCfg.InstanceHandle,
			"error", err,
		)
		return
	}

	slog.Info("built-in feed generator records synced to the instance PDS account",
		"instance_handle", pdsCfg.InstanceHandle,
		"count", len(registry.Builtins()),
	)
}
