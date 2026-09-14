"""
Tests for State Manager.

Tests deduplication state tracking and persistence.
"""
import json
import logging
import tempfile

import pytest
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


FEED_URL = "https://news.kagi.com/world.xml"
OTHER_FEED_URL = "https://news.kagi.com/science.xml"
RECENT_GUID = "https://kite.kagi.com/world/2026091012/us-announces-new-tariffs-on-china"
SUPPRESSED_GUID = "https://kite.kagi.com/world/2026091212/trade-tensions-escalate-tariffs-take-effect"
UNKNOWN_GUID = "https://kite.kagi.com/world/2026091299/never-seen-before"


class TestSuppressedGuids:
    """Suppressed candidates are remembered separately from posted ones.

    A semantic duplicate was never posted, so it cannot live in posted_guids, but
    it must not be re-offered to the model on the next run either. It gets its own
    per-feed list.
    """

    def test_mark_suppressed_records_without_posting(self, temp_state_file):
        """A suppressed guid reads back as suppressed and stays invisible to posted queries."""
        manager = StateManager(temp_state_file)

        manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        assert manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        assert not manager.is_suppressed(FEED_URL, UNKNOWN_GUID)

        # Suppressed is not posted: nothing was published to the network.
        assert not manager.is_posted(FEED_URL, SUPPRESSED_GUID)
        assert manager.get_posted_count(FEED_URL) == 0
        assert manager.get_all_posted_guids(FEED_URL) == []
        assert manager.get_recent_stories(FEED_URL, days=4) == []

    def test_suppressed_entry_persists_with_timestamp(self, temp_state_file):
        """The suppression survives a reload and records what it duplicated, and when."""
        manager = StateManager(temp_state_file)
        manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        reloaded = StateManager(temp_state_file)
        assert reloaded.is_suppressed(FEED_URL, SUPPRESSED_GUID)

        state_data = json.loads(temp_state_file.read_text())
        suppressed = state_data['feeds'][FEED_URL]['suppressed_guids']
        assert len(suppressed) == 1

        entry = suppressed[0]
        assert set(entry) == {"guid", "duplicate_of", "suppressed_at"}
        assert entry["guid"] == SUPPRESSED_GUID
        assert entry["duplicate_of"] == RECENT_GUID
        # Stored as an ISO string, like posted_at, so cleanup can age it.
        assert isinstance(entry["suppressed_at"], str)
        datetime.fromisoformat(entry["suppressed_at"])

    def test_suppression_is_scoped_to_its_feed(self, temp_state_file):
        """Suppressing a guid in one feed says nothing about another feed."""
        manager = StateManager(temp_state_file)

        manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        assert manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        assert not manager.is_suppressed(OTHER_FEED_URL, SUPPRESSED_GUID)

    def test_cleanup_drops_aged_suppressed_entries(self, temp_state_file):
        """Suppressed entries age out at max_age_days, like posted ones."""
        manager = StateManager(temp_state_file)
        manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        aged_guid = "https://kite.kagi.com/world/2026080112/long-forgotten-story"
        old_timestamp = (datetime.now() - timedelta(days=31)).isoformat()
        state_data = json.loads(temp_state_file.read_text())
        state_data['feeds'][FEED_URL]['suppressed_guids'].append({
            'guid': aged_guid,
            'duplicate_of': RECENT_GUID,
            'suppressed_at': old_timestamp,
        })
        temp_state_file.write_text(json.dumps(state_data, indent=2))

        manager = StateManager(temp_state_file)
        manager.cleanup_old_entries(FEED_URL)

        assert manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        assert not manager.is_suppressed(FEED_URL, aged_guid)

    def test_cleanup_caps_suppressed_entries_per_feed(self, temp_state_file):
        """Beyond max_guids_per_feed, the oldest suppressions are dropped first."""
        oldest_guid = "https://kite.kagi.com/world/2026091210/oldest-suppressed"
        middle_guid = "https://kite.kagi.com/world/2026091211/middle-suppressed"
        newest_guid = "https://kite.kagi.com/world/2026091212/newest-suppressed"

        now = datetime.now()
        temp_state_file.write_text(json.dumps({
            "feeds": {
                FEED_URL: {
                    "posted_guids": [],
                    "last_successful_run": None,
                    "suppressed_guids": [
                        {"guid": oldest_guid, "duplicate_of": RECENT_GUID,
                         "suppressed_at": (now - timedelta(hours=3)).isoformat()},
                        {"guid": middle_guid, "duplicate_of": RECENT_GUID,
                         "suppressed_at": (now - timedelta(hours=2)).isoformat()},
                        {"guid": newest_guid, "duplicate_of": RECENT_GUID,
                         "suppressed_at": (now - timedelta(hours=1)).isoformat()},
                    ],
                }
            }
        }, indent=2))

        manager = StateManager(temp_state_file, max_guids_per_feed=2)
        manager.cleanup_old_entries(FEED_URL)

        assert not manager.is_suppressed(FEED_URL, oldest_guid)
        assert manager.is_suppressed(FEED_URL, middle_guid)
        assert manager.is_suppressed(FEED_URL, newest_guid)

    def test_suppressed_and_posted_limits_are_independent(self, temp_state_file):
        """Suppressions do not consume the posted budget, or the reverse.

        A busy feed would otherwise evict posted history to make room for
        suppressions, reopening exact-duplicate posting.
        """
        posted_guid_one = "https://kite.kagi.com/world/2026091210/posted-one"
        posted_guid_two = "https://kite.kagi.com/world/2026091211/posted-two"
        suppressed_guid_one = "https://kite.kagi.com/world/2026091212/suppressed-one"
        suppressed_guid_two = "https://kite.kagi.com/world/2026091213/suppressed-two"

        manager = StateManager(temp_state_file, max_guids_per_feed=2)
        manager.mark_posted(FEED_URL, posted_guid_one, "at://test/1", title="One")
        manager.mark_posted(FEED_URL, posted_guid_two, "at://test/2", title="Two")
        manager.mark_suppressed(FEED_URL, suppressed_guid_one, RECENT_GUID)
        manager.mark_suppressed(FEED_URL, suppressed_guid_two, RECENT_GUID)

        manager.cleanup_old_entries(FEED_URL)

        # All four survive: the cap applies to each list on its own.
        assert manager.get_posted_count(FEED_URL) == 2
        assert manager.is_posted(FEED_URL, posted_guid_one)
        assert manager.is_posted(FEED_URL, posted_guid_two)
        assert manager.is_suppressed(FEED_URL, suppressed_guid_one)
        assert manager.is_suppressed(FEED_URL, suppressed_guid_two)

    def test_legacy_state_file_without_suppressed_key(self, temp_state_file):
        """A state file written before suppressions existed still loads and accepts them."""
        posted_guid = "https://kite.kagi.com/world/2026091210/already-posted"
        temp_state_file.write_text(json.dumps({
            "feeds": {
                FEED_URL: {
                    "posted_guids": [{
                        "guid": posted_guid,
                        "post_uri": "at://test/1",
                        "posted_at": datetime.now().isoformat(),
                    }],
                    "last_successful_run": None,
                }
            }
        }, indent=2))

        manager = StateManager(temp_state_file)

        assert not manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)

        manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        assert manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        # The pre-existing posted history is untouched.
        assert manager.is_posted(FEED_URL, posted_guid)


class TestUnparsableTimestamps:
    """A single corrupt timestamp must not wedge the whole feed's state.

    cleanup_old_entries runs on every mark_posted and mark_suppressed, so an
    entry with a missing or non-ISO timestamp raised on every write: the feed
    could never record another post, and the write that carried the new entry was
    lost with it. The bad entry is dropped, named in a WARNING, and the write
    proceeds.
    """

    def test_mark_posted_survives_unparsable_posted_at(self, temp_state_file, caplog):
        """Bad posted_at entries are dropped; good history and the new post survive."""
        missing_timestamp_guid = "https://kite.kagi.com/world/2026091201/no-timestamp"
        bad_timestamp_guid = "https://kite.kagi.com/world/2026091202/bad-timestamp"
        good_guid = "https://kite.kagi.com/world/2026091203/good-timestamp"
        new_guid = "https://kite.kagi.com/world/2026091204/newly-posted"

        temp_state_file.write_text(json.dumps({
            "feeds": {
                FEED_URL: {
                    "posted_guids": [
                        {"guid": missing_timestamp_guid, "post_uri": "at://test/1"},
                        {"guid": bad_timestamp_guid, "post_uri": "at://test/2",
                         "posted_at": "not-a-date"},
                        {"guid": good_guid, "post_uri": "at://test/3",
                         "posted_at": datetime.now().isoformat()},
                    ],
                    "last_successful_run": None,
                    "suppressed_guids": [],
                }
            }
        }, indent=2))

        manager = StateManager(temp_state_file)

        with caplog.at_level(logging.WARNING, logger="src.state_manager"):
            manager.mark_posted(FEED_URL, new_guid, "at://test/4", title="Newly posted")

        assert manager.is_posted(FEED_URL, new_guid)
        assert manager.is_posted(FEED_URL, good_guid)
        assert not manager.is_posted(FEED_URL, missing_timestamp_guid)
        assert not manager.is_posted(FEED_URL, bad_timestamp_guid)

        # The surviving state is what was written to disk, not just in memory.
        reloaded = StateManager(temp_state_file)
        assert reloaded.is_posted(FEED_URL, new_guid)
        assert reloaded.is_posted(FEED_URL, good_guid)
        assert not reloaded.is_posted(FEED_URL, missing_timestamp_guid)
        assert not reloaded.is_posted(FEED_URL, bad_timestamp_guid)

        warnings = [
            record.getMessage() for record in caplog.records
            if record.levelno == logging.WARNING
        ]
        assert any(missing_timestamp_guid in message for message in warnings), warnings
        assert any(bad_timestamp_guid in message for message in warnings), warnings

    def test_mark_suppressed_survives_unparsable_suppressed_at(self, temp_state_file,
                                                               caplog):
        """Bad suppressed_at entries are dropped; good ones and the new one survive."""
        missing_timestamp_guid = "https://kite.kagi.com/world/2026091205/no-timestamp"
        bad_timestamp_guid = "https://kite.kagi.com/world/2026091206/bad-timestamp"
        good_guid = "https://kite.kagi.com/world/2026091207/good-timestamp"

        temp_state_file.write_text(json.dumps({
            "feeds": {
                FEED_URL: {
                    "posted_guids": [],
                    "last_successful_run": None,
                    "suppressed_guids": [
                        {"guid": missing_timestamp_guid, "duplicate_of": RECENT_GUID},
                        {"guid": bad_timestamp_guid, "duplicate_of": RECENT_GUID,
                         "suppressed_at": "not-a-date"},
                        {"guid": good_guid, "duplicate_of": RECENT_GUID,
                         "suppressed_at": datetime.now().isoformat()},
                    ],
                }
            }
        }, indent=2))

        manager = StateManager(temp_state_file)

        with caplog.at_level(logging.WARNING, logger="src.state_manager"):
            manager.mark_suppressed(FEED_URL, SUPPRESSED_GUID, RECENT_GUID)

        assert manager.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        assert manager.is_suppressed(FEED_URL, good_guid)
        assert not manager.is_suppressed(FEED_URL, missing_timestamp_guid)
        assert not manager.is_suppressed(FEED_URL, bad_timestamp_guid)

        reloaded = StateManager(temp_state_file)
        assert reloaded.is_suppressed(FEED_URL, SUPPRESSED_GUID)
        assert reloaded.is_suppressed(FEED_URL, good_guid)
        assert not reloaded.is_suppressed(FEED_URL, missing_timestamp_guid)
        assert not reloaded.is_suppressed(FEED_URL, bad_timestamp_guid)

        warnings = [
            record.getMessage() for record in caplog.records
            if record.levelno == logging.WARNING
        ]
        assert any(missing_timestamp_guid in message for message in warnings), warnings
        assert any(bad_timestamp_guid in message for message in warnings), warnings
