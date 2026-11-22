package main

import (
	"context"
	"log/slog"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/feedgen"
)

// feedRecordSyncTimeout bounds the server PDS account login plus the built-in
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
// server's own PDS account repo.
//
// It never stops the boot. The served feed at-uris come from SERVER_DID,
// not from this login, so getFeed works whether or not the records are
// published. A missing credential, a failed login or a failed record read or
// write logs an ERROR and returns; the records are synced on the next
// restart. The password and session tokens are never logged.
func syncBuiltinFeedRecords(ctx context.Context, pdsCfg config.PDSConfig, login instanceRecordLogin, registry *feedgen.Registry, now time.Time) {
	if !pdsCfg.HasServerCredentials() {
		slog.Error("SERVER_HANDLE / SERVER_REPO_PASSWORD not set; "+
			"built-in feed generator records are not published to the server's PDS account",
			"pds_url", pdsCfg.URL)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, feedRecordSyncTimeout)
	defer cancel()

	client, err := login(ctx, pdsCfg.URL, pdsCfg.ServerHandle, pdsCfg.ServerRepoPassword)
	if err != nil {
		slog.Error("server PDS account login failed; built-in feed generator records not synced until next restart",
			"pds_url", pdsCfg.URL,
			"server_handle", pdsCfg.ServerHandle,
			"error", err,
		)
		return
	}

	if err := feedgen.EnsureBuiltinRecords(ctx, client, registry, now); err != nil {
		slog.Error("built-in feed generator record sync failed; retried on next restart",
			"pds_url", pdsCfg.URL,
			"server_handle", pdsCfg.ServerHandle,
			"error", err,
		)
		return
	}

	slog.Info("built-in feed generator records synced to the server PDS account",
		"server_handle", pdsCfg.ServerHandle,
		"count", len(registry.Builtins()),
	)
}
