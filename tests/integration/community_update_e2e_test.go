//go:build integration

package integration

// SERIAL BY DESIGN — do not add t.Parallel() to this file.
//
// Its tests drive the Jetstream firehose through the hand-rolled
// subscribeToJetstream* helpers below rather than testkit's cursor-gated
// subscriber. Those helpers subscribe to one shared stream and match on the
// first event of a collection, so a concurrent test writing the same
// collection is delivered to them too and either steals the match or trips
// their timeout. Per-test database clones do not isolate a shared websocket.
//
// docs/TEST_ARCHITECTURE.md §3.3 ("Parallelism is earned, not assumed").

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestCommunityUpdateE2E_WithJetstream tests the FULL community update flow with REAL Jetstream
// Flow: Service.UpdateCommunity() → PDS putRecord → REAL Jetstream Firehose → Consumer → AppView DB
//
// This is a TRUE E2E test - no simulated Jetstream events!
func TestCommunityUpdateE2E_WithJetstream(t *testing.T) {
	db := testkit.DB(t)

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is running
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.profile", pdsHostname)

	testConn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		t.Skipf("Jetstream not running at %s: %v. Run 'docker-compose --profile jetstream up' to start.", jetstreamURL, err)
	}
	_ = testConn.Close()

	ctx := context.Background()
	instanceDID := "did:web:coves.social"

	// Setup identity resolver with local PLC
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002" // Local PLC directory
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	communityRepo := postgres.NewCommunityRepository(db)
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil,
		blobService,
	)

	consumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, true, identityResolver)

	t.Run("update community with real Jetstream indexing", func(t *testing.T) {
		// First, create a community
		uniqueName := fmt.Sprintf("upd%s", uniqueTestID())
		creatorDID := "did:plc:jetstream-update-test"

		t.Logf("\n📝 Creating community on PDS...")
		community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Original Display Name",
			Description:            "Original description before update",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
		})
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		t.Logf("✅ Community created on PDS:")
		t.Logf("   DID: %s", community.DID)
		t.Logf("   RecordCID: %s", community.RecordCID)

		// Verify community is indexed (the service indexes it synchronously on create)
		t.Logf("\n🔄 Checking community is indexed...")
		indexed, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed: %v", err)
		}
		t.Logf("✅ Community indexed in AppView: %s", indexed.DisplayName)

		// Now update the community
		t.Logf("\n📝 Updating community via service...")

		// Start Jetstream subscription for update event BEFORE calling update
		updateEventChan := make(chan *jetstream.JetstreamEvent, 10)
		updateErrorChan := make(chan error, 1)
		updateDone := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForCommunityEvent(ctx, jetstreamURL, community.DID, "update", consumer, updateEventChan, updateDone)
			if subscribeErr != nil {
				updateErrorChan <- subscribeErr
			}
		}()

		// Give Jetstream a moment to connect
		time.Sleep(500 * time.Millisecond)

		// Perform the update
		newDisplayName := "Updated via TRUE E2E Test!"
		newDescription := "This description was updated and indexed via real Jetstream firehose"
		newVisibility := "unlisted"

		updated, err := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
			CommunityDID:           community.DID,
			UpdatedByDID:           creatorDID,
			DisplayName:            &newDisplayName,
			Description:            &newDescription,
			Visibility:             &newVisibility,
			AllowExternalDiscovery: nil,
		})
		if err != nil {
			t.Fatalf("Failed to update community: %v", err)
		}

		t.Logf("✅ Community update written to PDS:")
		t.Logf("   New RecordCID: %s (was: %s)", updated.RecordCID, community.RecordCID)

		// Wait for update event from real Jetstream
		t.Logf("\n⏳ Waiting for update event from Jetstream (max 30 seconds)...")

		select {
		case event := <-updateEventChan:
			t.Logf("✅ Received REAL update event from Jetstream!")
			t.Logf("   Event DID:    %s", event.Did)
			t.Logf("   Collection:   %s", event.Commit.Collection)
			t.Logf("   Operation:    %s", event.Commit.Operation)
			t.Logf("   RKey:         %s", event.Commit.RKey)

			// Verify operation type
			if event.Commit.Operation != "update" {
				t.Errorf("Expected operation 'update', got '%s'", event.Commit.Operation)
			}

			// Verify the update was indexed in AppView
			t.Logf("\n🔍 Verifying update indexed in AppView...")
			indexedUpdated, err := communityService.GetCommunity(ctx, community.DID)
			if err != nil {
				t.Fatalf("Failed to get updated community: %v", err)
			}

			t.Logf("✅ Update indexed in AppView:")
			t.Logf("   DisplayName: %s", indexedUpdated.DisplayName)
			t.Logf("   Description: %s", indexedUpdated.Description)
			t.Logf("   Visibility:  %s", indexedUpdated.Visibility)

			// Verify the changes
			if indexedUpdated.DisplayName != newDisplayName {
				t.Errorf("Expected display name '%s', got '%s'", newDisplayName, indexedUpdated.DisplayName)
			}
			if indexedUpdated.Description != newDescription {
				t.Errorf("Expected description '%s', got '%s'", newDescription, indexedUpdated.Description)
			}
			if indexedUpdated.Visibility != newVisibility {
				t.Errorf("Expected visibility '%s', got '%s'", newVisibility, indexedUpdated.Visibility)
			}

			close(updateDone)

		case err := <-updateErrorChan:
			t.Fatalf("Jetstream error: %v", err)

		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout: No update event received from Jetstream within 30 seconds")
		}

		t.Logf("\n✅ TRUE E2E COMMUNITY UPDATE FLOW COMPLETE:")
		t.Logf("   Service → PDS putRecord → Jetstream Firehose → Consumer → AppView ✓")
	})

	t.Run("multiple updates with real Jetstream", func(t *testing.T) {
		// This tests that consecutive updates all flow through Jetstream correctly
		uniqueName := fmt.Sprintf("mlt%s", uniqueTestID())
		creatorDID := "did:plc:multi-update-test"

		t.Logf("\n📝 Creating community for multi-update test...")
		community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   uniqueName,
			DisplayName:            "Multi-Update Test",
			Description:            "Testing multiple updates",
			Visibility:             "public",
			CreatedByDID:           creatorDID,
			HostedByDID:            instanceDID,
			AllowExternalDiscovery: true,
		})
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Verify create is indexed (service indexes synchronously on create)
		indexed, err := communityService.GetCommunity(ctx, community.DID)
		if err != nil {
			t.Fatalf("Community not indexed after create: %v", err)
		}
		t.Logf("✅ Create indexed: %s", indexed.DisplayName)

		// Perform 3 consecutive updates
		for i := 1; i <= 3; i++ {
			t.Logf("\n📝 Update %d of 3...", i)

			updateEventChan := make(chan *jetstream.JetstreamEvent, 10)
			updateErrorChan := make(chan error, 1)
			updateDone := make(chan bool)

			go func() {
				subscribeErr := subscribeToJetstreamForCommunityEvent(ctx, jetstreamURL, community.DID, "update", consumer, updateEventChan, updateDone)
				if subscribeErr != nil {
					updateErrorChan <- subscribeErr
				}
			}()

			time.Sleep(300 * time.Millisecond)

			newDesc := fmt.Sprintf("Update #%d at %s", i, time.Now().Format(time.RFC3339))
			_, err := communityService.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
				CommunityDID: community.DID,
				UpdatedByDID: creatorDID,
				Description:  &newDesc,
			})
			if err != nil {
				t.Fatalf("Update %d failed: %v", i, err)
			}

			select {
			case event := <-updateEventChan:
				if event.Commit.Operation != "update" {
					t.Errorf("Expected update operation, got %s", event.Commit.Operation)
				}
				t.Logf("✅ Update %d received via Jetstream", i)
			case err := <-updateErrorChan:
				t.Fatalf("Jetstream error on update %d: %v", i, err)
			case <-time.After(30 * time.Second):
				t.Fatalf("Timeout on update %d", i)
			}
			close(updateDone)

			// Verify in AppView
			indexed, getErr := communityService.GetCommunity(ctx, community.DID)
			if getErr != nil {
				t.Fatalf("Update %d: failed to get community: %v", i, getErr)
			}
			if indexed.Description != newDesc {
				t.Errorf("Update %d: expected description '%s', got '%s'", i, newDesc, indexed.Description)
			}
		}

		t.Logf("\n✅ MULTIPLE UPDATES TEST COMPLETE:")
		t.Logf("   3 consecutive updates all indexed via real Jetstream ✓")
	})
}

// subscribeToJetstreamForCommunityEvent subscribes to real Jetstream for specific community events
func subscribeToJetstreamForCommunityEvent(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	operation string, // "create", "update", or "delete"
	consumer *jetstream.CommunityEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// ONE deadline for the whole subscription, not one per read: the
	// budget is what the caller is willing to wait in total, and a
	// per-read deadline would let a busy stream extend it indefinitely.
	readDeadline := time.Now().Add(jetstreamReadBudget)

	// The gorilla/websocket library panics after 1000 repeated reads on a failed connection

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(readDeadline); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				// Check done channel first to handle clean shutdown
				select {
				case <-done:
					return nil
				default:
				}
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					return fmt.Errorf("Jetstream closed the subscription before the event arrived: %w", err)
				}
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return fmt.Errorf("Jetstream hung up before the event arrived: %w", err)
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// The deadline is the whole budget, so its expiry is the answer:
					// no matching event arrived. Reading on would be reading a
					// connection gorilla has already marked failed.
					return fmt.Errorf("no matching event within %s", jetstreamReadBudget)
				}
				// Still nil: this one only happens when the socket was closed
				// underneath us during shutdown, which the done check above
				// has already established is not a missing event.
				if strings.Contains(err.Error(), "use of closed network connection") {
					return nil
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			// Check if this is the event we're looking for
			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.profile" &&
				event.Commit.Operation == operation {

				// Process through consumer to index in AppView
				if err := consumer.HandleEvent(ctx, &event); err != nil {
					return fmt.Errorf("failed to process event: %w", err)
				}

				select {
				case eventChan <- &event:
					return nil
				case <-time.After(1 * time.Second):
					return fmt.Errorf("timeout sending event to channel")
				}
			}
		}
	}
}
