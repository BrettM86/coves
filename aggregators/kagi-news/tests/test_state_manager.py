"""
Tests for State Manager.

Tests deduplication state tracking and persistence.
"""
import pytest
import json
import tempfile
from pathlib import Path
from datetime import datetime, timedelta

from src.state_manager import StateManager


@pytest.fixture
def temp_state_file():
    """Create a temporary state file for testing."""
    with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.json') as f:
        temp_path = Path(f.name)
    yield temp_path
    # Cleanup
    if temp_path.exists():
        temp_path.unlink()


class TestStateManager:
    """Test suite for StateManager."""

    def test_initialize_new_state_file(self, temp_state_file):
        """Test initializing a new state file."""
        manager = StateManager(temp_state_file)

        # Should create an empty state
        assert temp_state_file.exists()
        state = json.loads(temp_state_file.read_text())
        assert 'feeds' in state
        assert state['feeds'] == {}

    def test_is_posted_returns_false_for_new_guid(self, temp_state_file):
        """Test that is_posted returns False for new GUIDs."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"
        guid = "https://kite.kagi.com/test/world/1"

        assert not manager.is_posted(feed_url, guid)

    def test_mark_posted_stores_guid(self, temp_state_file):
        """Test that mark_posted stores GUIDs."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"
        guid = "https://kite.kagi.com/test/world/1"
        post_uri = "at://did:plc:test/social.coves.post/abc123"

        manager.mark_posted(feed_url, guid, post_uri)

        # Should now return True
        assert manager.is_posted(feed_url, guid)

    def test_state_persists_across_instances(self, temp_state_file):
        """Test that state persists when creating new instances."""
        feed_url = "https://news.kagi.com/world.xml"
        guid = "https://kite.kagi.com/test/world/1"
        post_uri = "at://did:plc:test/social.coves.post/abc123"

        # First instance marks as posted
        manager1 = StateManager(temp_state_file)
        manager1.mark_posted(feed_url, guid, post_uri)

        # Second instance should see the same state
        manager2 = StateManager(temp_state_file)
        assert manager2.is_posted(feed_url, guid)

    def test_track_last_run_timestamp(self, temp_state_file):
        """Test tracking last successful run timestamp."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"
        timestamp = datetime.now()

        manager.update_last_run(feed_url, timestamp)

        retrieved = manager.get_last_run(feed_url)
        assert retrieved is not None
        # Compare timestamps (allow small difference due to serialization)
        assert abs((retrieved - timestamp).total_seconds()) < 1

    def test_get_last_run_returns_none_for_new_feed(self, temp_state_file):
        """Test that get_last_run returns None for new feeds."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        assert manager.get_last_run(feed_url) is None

    def test_cleanup_old_guids(self, temp_state_file):
        """Test cleanup of old GUIDs (> 30 days)."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Add recent GUID
        recent_guid = "https://kite.kagi.com/test/world/1"
        manager.mark_posted(feed_url, recent_guid, "at://test/1")

        # Manually add old GUID (> 30 days)
        old_timestamp = (datetime.now() - timedelta(days=31)).isoformat()
        state_data = json.loads(temp_state_file.read_text())
        state_data['feeds'][feed_url]['posted_guids'].append({
            'guid': 'https://kite.kagi.com/test/world/old',
            'post_uri': 'at://test/old',
            'posted_at': old_timestamp
        })
        temp_state_file.write_text(json.dumps(state_data, indent=2))

        # Reload and cleanup
        manager = StateManager(temp_state_file)
        manager.cleanup_old_entries(feed_url)

        # Recent GUID should still be there
        assert manager.is_posted(feed_url, recent_guid)

        # Old GUID should be removed
        assert not manager.is_posted(feed_url, 'https://kite.kagi.com/test/world/old')

    def test_limit_guids_to_100_per_feed(self, temp_state_file):
        """Test that only last 100 GUIDs are kept per feed."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Add 150 GUIDs
        for i in range(150):
            guid = f"https://kite.kagi.com/test/world/{i}"
            manager.mark_posted(feed_url, guid, f"at://test/{i}")

        # Cleanup (should limit to 100)
        manager.cleanup_old_entries(feed_url)

        # Reload state
        manager = StateManager(temp_state_file)

        # Should have exactly 100 entries (most recent)
        state_data = json.loads(temp_state_file.read_text())
        assert len(state_data['feeds'][feed_url]['posted_guids']) == 100

        # Oldest entries should be removed
        assert not manager.is_posted(feed_url, "https://kite.kagi.com/test/world/0")
        assert not manager.is_posted(feed_url, "https://kite.kagi.com/test/world/49")

        # Recent entries should still be there
        assert manager.is_posted(feed_url, "https://kite.kagi.com/test/world/149")
        assert manager.is_posted(feed_url, "https://kite.kagi.com/test/world/100")

    def test_multiple_feeds_tracked_separately(self, temp_state_file):
        """Test that multiple feeds are tracked independently."""
        manager = StateManager(temp_state_file)

        feed1 = "https://news.kagi.com/world.xml"
        feed2 = "https://news.kagi.com/tech.xml"
        guid1 = "https://kite.kagi.com/test/world/1"
        guid2 = "https://kite.kagi.com/test/tech/1"

        manager.mark_posted(feed1, guid1, "at://test/1")
        manager.mark_posted(feed2, guid2, "at://test/2")

        # Each feed should only know about its own GUIDs
        assert manager.is_posted(feed1, guid1)
        assert not manager.is_posted(feed1, guid2)

        assert manager.is_posted(feed2, guid2)
        assert not manager.is_posted(feed2, guid1)

    def test_get_posted_count(self, temp_state_file):
        """Test getting count of posted items per feed."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Initially 0
        assert manager.get_posted_count(feed_url) == 0

        # Add 5 items
        for i in range(5):
            manager.mark_posted(feed_url, f"guid-{i}", f"post-{i}")

        assert manager.get_posted_count(feed_url) == 5

    def test_state_file_format_is_valid_json(self, temp_state_file):
        """Test that state file is always valid JSON."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        manager.mark_posted(feed_url, "test-guid", "test-post-uri")
        manager.update_last_run(feed_url, datetime.now())

        # Should be valid JSON
        with open(temp_state_file) as f:
            state = json.load(f)

        assert 'feeds' in state
        assert feed_url in state['feeds']
        assert 'posted_guids' in state['feeds'][feed_url]
        assert 'last_successful_run' in state['feeds'][feed_url]

    def test_automatic_cleanup_on_mark_posted(self, temp_state_file):
        """Test that cleanup happens automatically when marking posted."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Add old entry manually
        old_timestamp = (datetime.now() - timedelta(days=31)).isoformat()
        state_data = {
            'feeds': {
                feed_url: {
                    'posted_guids': [{
                        'guid': 'old-guid',
                        'post_uri': 'old-uri',
                        'posted_at': old_timestamp
                    }],
                    'last_successful_run': None
                }
            }
        }
        temp_state_file.write_text(json.dumps(state_data, indent=2))

        # Reload and add new entry (should trigger cleanup)
        manager = StateManager(temp_state_file)
        manager.mark_posted(feed_url, "new-guid", "new-uri")

        # Old entry should be gone
        assert not manager.is_posted(feed_url, "old-guid")
        assert manager.is_posted(feed_url, "new-guid")

    def test_mark_posted_stores_title_and_summary(self, temp_state_file):
        """Test that mark_posted stores title and summary_snippet."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        manager.mark_posted(
            feed_url, "guid-1", "at://test/1",
            title="US announces tariffs",
            summary_snippet="The United States has announced new tariffs on goods."
        )

        state_data = json.loads(temp_state_file.read_text())
        entry = state_data['feeds'][feed_url]['posted_guids'][0]
        assert entry['title'] == "US announces tariffs"
        assert entry['summary_snippet'] == "The United States has announced new tariffs on goods."

    def test_mark_posted_truncates_summary_to_200(self, temp_state_file):
        """Test that summary_snippet is truncated to 200 characters."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        long_summary = "x" * 300
        manager.mark_posted(
            feed_url, "guid-1", "at://test/1",
            title="Title", summary_snippet=long_summary
        )

        state_data = json.loads(temp_state_file.read_text())
        entry = state_data['feeds'][feed_url]['posted_guids'][0]
        assert len(entry['summary_snippet']) == 200

    def test_get_recent_stories_within_window(self, temp_state_file):
        """Test get_recent_stories returns entries within lookback window."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Add a recent story with title
        manager.mark_posted(
            feed_url, "guid-1", "at://test/1",
            title="Recent Story", summary_snippet="Recent summary"
        )

        recent = manager.get_recent_stories(feed_url, days=4)
        assert len(recent) == 1
        assert recent[0]['id'] == "guid-1"
        assert recent[0]['title'] == "Recent Story"
        assert recent[0]['summary'] == "Recent summary"

    def test_get_recent_stories_excludes_old_entries(self, temp_state_file):
        """Test that get_recent_stories excludes entries outside lookback."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Manually add an old entry with title
        old_timestamp = (datetime.now() - timedelta(days=5)).isoformat()
        state_data = {
            'feeds': {
                feed_url: {
                    'posted_guids': [{
                        'guid': 'old-guid',
                        'post_uri': 'at://test/old',
                        'posted_at': old_timestamp,
                        'title': 'Old Story',
                        'summary_snippet': 'Old summary'
                    }],
                    'last_successful_run': None
                }
            }
        }
        temp_state_file.write_text(json.dumps(state_data, indent=2))

        manager = StateManager(temp_state_file)
        recent = manager.get_recent_stories(feed_url, days=4)
        assert len(recent) == 0

    def test_get_recent_stories_skips_entries_without_title(self, temp_state_file):
        """Test backward compat: entries without title are skipped."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        # Old-format entry (no title)
        manager.mark_posted(feed_url, "old-format-guid", "at://test/1")

        # New-format entry (with title)
        manager.mark_posted(
            feed_url, "new-format-guid", "at://test/2",
            title="Story Title", summary_snippet="Summary"
        )

        recent = manager.get_recent_stories(feed_url, days=4)
        assert len(recent) == 1
        assert recent[0]['id'] == "new-format-guid"

    def test_get_recent_stories_empty_feed(self, temp_state_file):
        """Test get_recent_stories with no entries."""
        manager = StateManager(temp_state_file)
        feed_url = "https://news.kagi.com/world.xml"

        recent = manager.get_recent_stories(feed_url, days=4)
        assert recent == []
