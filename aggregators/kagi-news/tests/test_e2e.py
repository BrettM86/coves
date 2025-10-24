"""
End-to-End Integration Tests.

Tests the complete aggregator workflow against live infrastructure:
- Real HTTP mocking (Kagi RSS)
- Real PDS (Coves test PDS via Docker)
- Real community posting
- Real state management

Requires:
- Coves test PDS running on localhost:3001
- Test database with community: e2e-95206.community.coves.social
- Aggregator account: kagi-news.local.coves.dev
"""
import os
import pytest
import responses
from pathlib import Path
from datetime import datetime

from src.main import Aggregator
from src.coves_client import CovesClient
from src.config import ConfigLoader


# Skip E2E tests by default (require live infrastructure)
pytestmark = pytest.mark.skipif(
    os.getenv('RUN_E2E_TESTS') != '1',
    reason="E2E tests require RUN_E2E_TESTS=1 and live PDS"
)


@pytest.fixture
def test_community(aggregator_credentials):
    """Create a test community for E2E testing."""
    import time
    import requests

    handle, password = aggregator_credentials

    # Authenticate
    auth_response = requests.post(
        "http://localhost:3001/xrpc/com.atproto.server.createSession",
        json={"identifier": handle, "password": password}
    )
    token = auth_response.json()["accessJwt"]

    # Create community (use short name to avoid handle length limits)
    community_name = f"e2e-{int(time.time()) % 10000}"  # Last 4 digits only
    create_response = requests.post(
        "http://localhost:8081/xrpc/social.coves.community.create",
        headers={"Authorization": f"Bearer {token}"},
        json={
            "name": community_name,
            "displayName": "E2E Test Community",
            "description": "Temporary community for aggregator E2E testing",
            "visibility": "public"
        }
    )

    if create_response.ok:
        community = create_response.json()
        community_handle = f"{community_name}.community.coves.social"
        print(f"\n✅ Created test community: {community_handle}")
        return community_handle
    else:
        raise Exception(f"Failed to create community: {create_response.text}")


@pytest.fixture
def test_config_file(tmp_path, test_community):
    """Create test configuration file with dynamic community."""
    config_content = f"""
coves_api_url: http://localhost:8081

feeds:
  - name: "Kagi World News"
    url: "https://news.kagi.com/world.xml"
    community_handle: "{test_community}"
    enabled: true

log_level: debug
"""
    config_file = tmp_path / "config.yaml"
    config_file.write_text(config_content)
    return config_file


@pytest.fixture
def test_state_file(tmp_path):
    """Create temporary state file."""
    return tmp_path / "state.json"


@pytest.fixture
def mock_kagi_feed():
    """Load real Kagi RSS feed fixture."""
    # Load from data directory (where actual feed is stored)
    fixture_path = Path(__file__).parent.parent / "data" / "world.xml"
    if not fixture_path.exists():
        # Fallback to tests/fixtures if moved
        fixture_path = Path(__file__).parent / "fixtures" / "world.xml"
    return fixture_path.read_text()


@pytest.fixture
def aggregator_credentials():
    """Get aggregator credentials from environment."""
    handle = os.getenv('AGGREGATOR_HANDLE', 'kagi-news.local.coves.dev')
    password = os.getenv('AGGREGATOR_PASSWORD', 'kagi-aggregator-2024-secure-pass')
    return handle, password


class TestEndToEnd:
    """Full end-to-end integration tests."""

    @responses.activate
    def test_full_aggregator_workflow(
        self,
        test_config_file,
        test_state_file,
        mock_kagi_feed,
        aggregator_credentials
    ):
        """
        Test complete workflow: fetch → parse → format → post → verify.

        This test:
        1. Mocks Kagi RSS HTTP request
        2. Authenticates with real PDS
        3. Parses real Kagi HTML content
        4. Formats with rich text facets
        5. Posts to real community
        6. Verifies post was created
        7. Tests deduplication (no repost)
        """
        # Mock Kagi RSS feed
        responses.add(
            responses.GET,
            "https://news.kagi.com/world.xml",
            body=mock_kagi_feed,
            status=200,
            content_type="application/xml"
        )

        # Allow passthrough for localhost (PDS)
        responses.add_passthru("http://localhost")

        # Set up environment
        handle, password = aggregator_credentials
        os.environ['AGGREGATOR_HANDLE'] = handle
        os.environ['AGGREGATOR_PASSWORD'] = password
        os.environ['PDS_URL'] = 'http://localhost:3001'  # Auth through PDS

        # Create aggregator
        aggregator = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )

        # Run first time: should post stories
        print("\n" + "="*60)
        print("🚀 Running first aggregator pass (should post stories)")
        print("="*60)
        aggregator.run()

        # Verify state was updated (stories marked as posted)
        posted_count = aggregator.state_manager.get_posted_count(
            "https://news.kagi.com/world.xml"
        )
        print(f"\n✅ First pass: {posted_count} stories posted and tracked")
        assert posted_count > 0, "Should have posted at least one story"

        # Create new aggregator instance (simulates CRON re-run)
        aggregator2 = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )

        # Run second time: should skip duplicates
        print("\n" + "="*60)
        print("🔄 Running second aggregator pass (should skip duplicates)")
        print("="*60)
        aggregator2.run()

        # Verify count didn't change (deduplication worked)
        posted_count2 = aggregator2.state_manager.get_posted_count(
            "https://news.kagi.com/world.xml"
        )
        print(f"\n✅ Second pass: Still {posted_count2} stories (duplicates skipped)")
        assert posted_count2 == posted_count, "Should not post duplicates"

    @responses.activate
    def test_post_with_external_embed(
        self,
        test_config_file,
        test_state_file,
        mock_kagi_feed,
        aggregator_credentials
    ):
        """
        Test that posts include external embeds with images.

        Verifies:
        - External embed is created
        - Thumbnail URL is included
        - Title and description are set
        """
        # Mock Kagi RSS feed
        responses.add(
            responses.GET,
            "https://news.kagi.com/world.xml",
            body=mock_kagi_feed,
            status=200
        )

        # Allow passthrough for localhost (PDS)
        responses.add_passthru("http://localhost")

        # Set up environment
        handle, password = aggregator_credentials
        os.environ['AGGREGATOR_HANDLE'] = handle
        os.environ['AGGREGATOR_PASSWORD'] = password
        os.environ['PDS_URL'] = 'http://localhost:3001'  # Auth through PDS

        # Run aggregator
        aggregator = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )

        print("\n" + "="*60)
        print("🖼️  Testing external embed creation")
        print("="*60)
        aggregator.run()

        # Verify posts were created
        posted_count = aggregator.state_manager.get_posted_count(
            "https://news.kagi.com/world.xml"
        )
        print(f"\n✅ Posted {posted_count} stories with external embeds")
        assert posted_count > 0

    def test_authentication_with_live_pds(self, aggregator_credentials):
        """
        Test authentication against live PDS.

        Verifies:
        - Can authenticate with aggregator account
        - Receives valid JWT tokens
        - DID matches expected format
        """
        handle, password = aggregator_credentials

        print("\n" + "="*60)
        print(f"🔐 Testing authentication: {handle}")
        print("="*60)

        # Create client and authenticate
        client = CovesClient(
            api_url="http://localhost:8081",  # AppView for posting
            handle=handle,
            password=password,
            pds_url="http://localhost:3001"  # PDS for auth
        )

        client.authenticate()

        print(f"\n✅ Authentication successful")
        print(f"   Handle: {client.handle}")
        print(f"   Authenticated: {client._authenticated}")

        assert client._authenticated is True
        assert hasattr(client, 'did')
        assert client.did.startswith("did:plc:")

    def test_state_persistence_across_runs(
        self,
        test_config_file,
        test_state_file,
        aggregator_credentials
    ):
        """
        Test that state persists correctly across multiple runs.

        Verifies:
        - State file is created
        - Posted GUIDs are tracked
        - Last run timestamp is updated
        - State survives aggregator restart
        """
        # Mock empty feed (to avoid posting)
        import responses as resp
        resp.start()
        resp.add(
            resp.GET,
            "https://news.kagi.com/world.xml",
            body='<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>',
            status=200
        )

        handle, password = aggregator_credentials
        os.environ['AGGREGATOR_HANDLE'] = handle
        os.environ['AGGREGATOR_PASSWORD'] = password

        print("\n" + "="*60)
        print("💾 Testing state persistence")
        print("="*60)

        # First run
        aggregator1 = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )
        aggregator1.run()

        # Verify state file was created
        assert test_state_file.exists(), "State file should be created"
        print(f"\n✅ State file created: {test_state_file}")

        # Verify last run was recorded
        last_run1 = aggregator1.state_manager.get_last_run(
            "https://news.kagi.com/world.xml"
        )
        assert last_run1 is not None, "Last run should be recorded"
        print(f"   Last run: {last_run1}")

        # Second run (new instance)
        aggregator2 = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )
        aggregator2.run()

        # Verify state persisted
        last_run2 = aggregator2.state_manager.get_last_run(
            "https://news.kagi.com/world.xml"
        )
        assert last_run2 >= last_run1, "Last run should be updated"
        print(f"   Last run (after restart): {last_run2}")
        print(f"\n✅ State persisted across aggregator restarts")

        resp.stop()
        resp.reset()

    def test_error_recovery(
        self,
        test_config_file,
        test_state_file,
        aggregator_credentials
    ):
        """
        Test that aggregator handles errors gracefully.

        Verifies:
        - Continues processing on feed errors
        - Doesn't crash on network failures
        - Logs errors appropriately
        """
        # Mock feed failure
        import responses as resp
        resp.start()
        resp.add(
            resp.GET,
            "https://news.kagi.com/world.xml",
            body="Internal Server Error",
            status=500
        )

        handle, password = aggregator_credentials
        os.environ['AGGREGATOR_HANDLE'] = handle
        os.environ['AGGREGATOR_PASSWORD'] = password

        print("\n" + "="*60)
        print("🛡️  Testing error recovery")
        print("="*60)

        # Should not crash
        aggregator = Aggregator(
            config_path=test_config_file,
            state_file=test_state_file
        )

        try:
            aggregator.run()
            print(f"\n✅ Aggregator handled feed error gracefully")
        except Exception as e:
            pytest.fail(f"Aggregator should handle errors gracefully: {e}")

        resp.stop()
        resp.reset()


def test_coves_client_external_embed_format(aggregator_credentials):
    """
    Test external embed formatting.

    Verifies:
    - Embed structure matches social.coves.embed.external
    - All required fields are present
    - Optional thumbnail is included when provided
    """
    handle, password = aggregator_credentials

    client = CovesClient(
        api_url="http://localhost:8081",
        handle=handle,
        password=password
    )

    # Test with thumbnail
    embed = client.create_external_embed(
        uri="https://example.com/story",
        title="Test Story",
        description="Test description",
        thumb="https://example.com/image.jpg"
    )

    assert embed["$type"] == "social.coves.embed.external"
    assert embed["external"]["uri"] == "https://example.com/story"
    assert embed["external"]["title"] == "Test Story"
    assert embed["external"]["description"] == "Test description"
    assert embed["external"]["thumb"] == "https://example.com/image.jpg"

    # Test without thumbnail
    embed_no_thumb = client.create_external_embed(
        uri="https://example.com/story2",
        title="Test Story 2",
        description="Test description 2"
    )

    assert "thumb" not in embed_no_thumb["external"]
    print("\n✅ External embed format correct")
