//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/communitysignals"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// communityProfileEventWithWarnings is communityProfileEvent as a create or
// update commit carrying contentWarnings and, when non-empty, an origin.
func communityProfileEventWithWarnings(did, handle, name, op, rev, origin string, warnings []string) *JetstreamEvent {
	base := communityProfileEvent(did, handle, name, rev)
	base.Commit.Record["contentWarnings"] = warnings
	if origin != "" {
		base.Commit.Record["origin"] = origin
	}
	return revCommitEvent(did, conflictProfileCollection, op, "self", rev, base.Commit.CID, base.TimeUS, base.Commit.Record)
}

type communityFlagEventRow struct {
	OriginHost  sql.NullString
	OldNSFW     bool
	NewNSFW     bool
	DeliveredAt sql.NullTime
}

func readCommunityFlagEvents(t *testing.T, db *sql.DB, communityDID string) []communityFlagEventRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT origin_host, old_nsfw, new_nsfw, delivered_at
		FROM community_nsfw_flag_events
		WHERE community_did = $1
		ORDER BY id`, communityDID)
	require.NoError(t, err, "reading community_nsfw_flag_events")
	defer rows.Close()
	var out []communityFlagEventRow
	for rows.Next() {
		var r communityFlagEventRow
		require.NoError(t, rows.Scan(&r.OriginHost, &r.OldNSFW, &r.NewNSFW, &r.DeliveredAt))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func storedCommunityOrigin(t *testing.T, db *sql.DB, did string) string {
	t.Helper()
	var origin sql.NullString
	require.NoError(t, db.QueryRow(`SELECT origin FROM communities WHERE did = $1`, did).Scan(&origin))
	return origin.String
}

// TestCommunityConsumer_NSFWFlagEvents is B5: indexing community profiles
// records one event per change of nsfw presence, never for a redelivery or a
// change of other warnings, with the origin host of a bridged community and
// NULL for a native one.
func TestCommunityConsumer_NSFWFlagEvents(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	id := testkit.UniqueID(t)
	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithLocalInstanceDomain("test.local"))

	t.Run("bridged community", func(t *testing.T) {
		did := "did:plc:flagbridged" + id
		handle := "flagbridged" + id + ".lemmy-world.tdpl.io"
		name := "flagbridged" + id
		resolver := fixedResolver{id: identity.Identity{Handle: handle, PDSURL: trustedBridgePDS}}
		consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, resolver,
			WithCommunityBridgeTrust(trustingBridge()))
		event := func(op, rev string, warnings ...string) *JetstreamEvent {
			return communityProfileEventWithWarnings(did, handle, name, op, rev, bridgedOrigin, append([]string{}, warnings...))
		}

		require.NoError(t, consumer.HandleEvent(ctx, event("create", "3lflaga", "nsfw")))
		require.Equal(t, bridgedOrigin, storedCommunityOrigin(t, db, did), "fixture: the bridged origin must be admitted")
		require.NoError(t, consumer.HandleEvent(ctx, event("update", "3lflagb")))
		reflagged := event("update", "3lflagc", "nsfw")
		require.NoError(t, consumer.HandleEvent(ctx, reflagged))
		require.NoError(t, consumer.HandleEvent(ctx, reflagged), "redelivery of the same commit")
		require.NoError(t, consumer.HandleEvent(ctx, event("update", "3lflagd", "nsfw", "spoiler")))
		require.Equal(t, bridgedOrigin, storedCommunityOrigin(t, db, did), "fixture: updates must keep the bridged origin")

		events := readCommunityFlagEvents(t, db, did)
		require.Len(t, events, 3, "exactly one event per change of nsfw presence")
		for i, want := range []struct{ old, new bool }{{false, true}, {true, false}, {false, true}} {
			assert.Equal(t, want.old, events[i].OldNSFW, "event %d old_nsfw", i)
			assert.Equal(t, want.new, events[i].NewNSFW, "event %d new_nsfw", i)
			assert.True(t, events[i].OriginHost.Valid, "event %d must carry the origin host", i)
			assert.Equal(t, "lemmy.world", events[i].OriginHost.String, "event %d origin_host", i)
		}
	})

	t.Run("native community", func(t *testing.T) {
		did := "did:plc:flagnative" + id
		handle := "c-flagnative" + id + ".test.local"
		consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil)

		require.NoError(t, consumer.HandleEvent(ctx,
			communityProfileEventWithWarnings(did, handle, "flagnative"+id, "create", "3lflagn", "test.local", []string{"nsfw"})))
		require.Equal(t, "test.local", storedCommunityOrigin(t, db, did), "fixture: the native origin must be the local instance domain")

		events := readCommunityFlagEvents(t, db, did)
		require.Len(t, events, 1, "a native community created with nsfw records one event")
		assert.False(t, events[0].OldNSFW)
		assert.True(t, events[0].NewNSFW)
		assert.False(t, events[0].OriginHost.Valid, "a native community's event has origin_host NULL, got %q", events[0].OriginHost.String)
	})

	t.Run("spoiler only", func(t *testing.T) {
		did := "did:plc:flagspoiler" + id
		handle := "c-flagspoiler" + id + ".test.local"
		consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil)

		require.NoError(t, consumer.HandleEvent(ctx,
			communityProfileEventWithWarnings(did, handle, "flagspoiler"+id, "create", "3lflags", "", []string{"spoiler"})))
		var indexed int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&indexed))
		require.Equal(t, 1, indexed, "fixture: the community must be indexed")

		assert.Empty(t, readCommunityFlagEvents(t, db, did), "a community without nsfw records no event")
	})
}

// flagRecordingSender records every message; probe, when set, runs during the
// send and its result is recorded alongside the text.
type flagRecordingSender struct {
	mu       sync.Mutex
	messages []string
	probes   []flagVisibility
	probe    func(ctx context.Context) flagVisibility
}

type flagVisibility struct {
	communityVisible bool
	addedVisible     bool // a committed false->true event row
	removedVisible   bool // a committed true->false event row
	err              error
}

func (s *flagRecordingSender) SendMessage(ctx context.Context, text string) error {
	var seen flagVisibility
	if s.probe != nil {
		seen = s.probe(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, text)
	s.probes = append(s.probes, seen)
	return nil
}

func (s *flagRecordingSender) Messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *flagRecordingSender) Probes() []flagVisibility {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]flagVisibility(nil), s.probes...)
}

func flagSignalConfig(alertsEnabled bool) communitysignals.Config {
	return communitysignals.Config{
		AlertsEnabled:       alertsEnabled,
		SweepInterval:       5 * time.Minute,
		AttemptLimit:        3,
		RetryBaseDelay:      time.Minute,
		DigestMinPosts:      10,
		DigestRiseThreshold: 20,
	}
}

func flagSignalLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// mondayHalfPastNineAfter is the first Monday 09:30 UTC strictly after t.
func mondayHalfPastNineAfter(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, time.UTC)
	for d.Weekday() != time.Monday || !d.After(t) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// independentPool opens a second pool to the same per-test database, so a
// query through it sees only committed state.
func independentPool(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	var name string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))
	other, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(name))
	require.NoError(t, err)
	other.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = other.Close() })
	require.NoError(t, other.Ping())
	return other
}

// TestCommunityConsumer_FlagNotifierWiring is B8: the consumer hands committed
// flag events to the notifier, which sends nothing while alerts are disabled or
// Telegram is unconfigured, and otherwise sends each alert after the community
// change has committed.
func TestCommunityConsumer_FlagNotifierWiring(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		alertsEnabled bool
		nilSender     bool
	}{
		{name: "alerts disabled", alertsEnabled: false},
		{name: "telegram unconfigured", alertsEnabled: true, nilSender: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()
			id := testkit.UniqueID(t)

			clockAt := mondayHalfPastNineAfter(time.Now().Add(time.Hour))
			recorder := &flagRecordingSender{}
			var sender adminreports.MessageSender
			if !tc.nilSender {
				sender = recorder
			}
			svc := communitysignals.NewService(postgres.NewCommunitySignalRepository(db, "test.local"), sender,
				flagSignalConfig(tc.alertsEnabled), func() time.Time { return clockAt }, flagSignalLogger())
			repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithLocalInstanceDomain("test.local"))
			consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil, WithCommunityFlagNotifier(svc))

			did := "did:plc:flagquiet" + id
			handle := "c-flagquiet" + id + ".test.local"
			name := "flagquiet" + id
			require.NoError(t, consumer.HandleEvent(ctx,
				communityProfileEventWithWarnings(did, handle, name, "create", "3lquieta", "", []string{"nsfw"})))
			require.NoError(t, consumer.HandleEvent(ctx,
				communityProfileEventWithWarnings(did, handle, name, "update", "3lquietb", "", []string{})))

			// Qualifying digest content: 12 recent posts, all nsfw, no prior posts.
			authorDID := "did:plc:flagquietauthor" + id
			insertBridgedUser(t, db, authorDID, "flagquietauthor"+id+".test")
			for i := 0; i < 12; i++ {
				rkey := fmt.Sprintf("flagquiet%s-%d", id, i)
				_, err := db.Exec(`
					INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, content_labels)
					VALUES ($1, $2, $3, $4, $5, 'flag post', $6, '{"values":[{"val":"nsfw"}]}'::jsonb)`,
					"at://"+authorDID+"/social.coves.community.post/"+rkey, "bafy"+rkey, rkey, authorDID, did,
					clockAt.Add(-time.Hour-time.Duration(i)*time.Minute))
				require.NoError(t, err, "seeding post")
			}

			require.NoError(t, svc.Sweep(ctx))
			require.NoError(t, svc.RunWeeklyDigest(ctx))

			assert.Len(t, readCommunityFlagEvents(t, db, did), 2, "both changes are recorded even when nothing is sent")
			assert.Empty(t, recorder.Messages(), "no message may be sent")
		})
	}

	t.Run("alerts enabled sends after commit", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		ctx := context.Background()
		id := testkit.UniqueID(t)
		did := "did:plc:flagwire" + id
		handle := "c-flagwire" + id + ".test.local"
		name := "flagwire" + id

		observer := independentPool(t, db)
		sender := &flagRecordingSender{probe: func(ctx context.Context) flagVisibility {
			var seen flagVisibility
			var communities, added, removed int
			seen.err = observer.QueryRowContext(ctx, `SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&communities)
			if seen.err == nil {
				seen.err = observer.QueryRowContext(ctx, `
					SELECT count(*) FILTER (WHERE NOT old_nsfw AND new_nsfw),
					       count(*) FILTER (WHERE old_nsfw AND NOT new_nsfw)
					FROM community_nsfw_flag_events WHERE community_did = $1`, did).Scan(&added, &removed)
			}
			seen.communityVisible, seen.addedVisible, seen.removedVisible = communities == 1, added == 1, removed == 1
			return seen
		}}
		clockAt := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
		svc := communitysignals.NewService(postgres.NewCommunitySignalRepository(db, "test.local"), sender,
			flagSignalConfig(true), func() time.Time { return clockAt }, flagSignalLogger())
		repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithLocalInstanceDomain("test.local"))
		consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil, WithCommunityFlagNotifier(svc))

		require.NoError(t, consumer.HandleEvent(ctx,
			communityProfileEventWithWarnings(did, handle, name, "create", "3lwirea", "", []string{"nsfw"})))
		messages := sender.Messages()
		require.Len(t, messages, 1, "indexing a community with nsfw must send one alert")
		assert.True(t, strings.Contains(messages[0], handle), "the alert must name the handle %q: %q", handle, messages[0])
		probe := sender.Probes()[0]
		require.NoError(t, probe.err, "probing committed state during the send")
		assert.True(t, probe.communityVisible, "the community row must be committed before the alert is sent")
		assert.True(t, probe.addedVisible, "the event row must be committed before the alert is sent")
		events := readCommunityFlagEvents(t, db, did)
		require.Len(t, events, 1)
		assert.True(t, events[0].DeliveredAt.Valid, "delivered_at must be set after the send")

		require.NoError(t, consumer.HandleEvent(ctx,
			communityProfileEventWithWarnings(did, handle, name, "update", "3lwireb", "", []string{})))
		messages = sender.Messages()
		require.Len(t, messages, 2, "removing nsfw must send a second alert")
		assert.True(t, strings.Contains(messages[1], handle), "the alert must name the handle %q: %q", handle, messages[1])
		probe = sender.Probes()[1]
		require.NoError(t, probe.err, "probing committed state during the send")
		assert.True(t, probe.removedVisible, "the removal event must be committed before the alert is sent")
		events = readCommunityFlagEvents(t, db, did)
		require.Len(t, events, 2)
		assert.True(t, events[1].DeliveredAt.Valid, "delivered_at must be set after the second send")
	})
}
