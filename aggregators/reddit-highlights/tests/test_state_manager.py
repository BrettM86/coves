"""
Tests for state_manager module.
"""
import json
import pytest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch, MagicMock

from src.state_manager import StateManager


class TestStateManagerInit:
    """Tests for StateManager initialization."""

    def test_creates_new_state_file(self, tmp_path):
        """Test that new state file is created when it doesn't exist."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        assert state_file.exists()
        assert manager.state == {"feeds": {}}

    def test_loads_existing_state_file(self, tmp_path):
        """Test that existing state file is loaded."""
        state_file = tmp_path / "state.json"
        existing_state = {
            "feeds": {
                "nba": {
                    "posted_guids": [{"guid": "test123", "post_uri": "at://...", "posted_at": "2024-01-01T00:00:00"}],
                    "last_successful_run": "2024-01-01T00:00:00",
                }
            }
        }
        state_file.write_text(json.dumps(existing_state))

        manager = StateManager(state_file)
        assert manager.state == existing_state

    def test_handles_corrupted_state_file(self, tmp_path):
        """Test that corrupted state file is handled gracefully."""
        state_file = tmp_path / "state.json"
        state_file.write_text("not valid json {{{")

        manager = StateManager(state_file)

        # Should create new state and backup corrupted file
        assert manager.state == {"feeds": {}}
        backup_file = tmp_path / "state.json.corrupted"
        assert backup_file.exists()
        assert backup_file.read_text() == "not valid json {{{"

    def test_creates_parent_directories(self, tmp_path):
        """Test that parent directories are created if needed."""
        state_file = tmp_path / "nested" / "deep" / "state.json"
        manager = StateManager(state_file)

        assert state_file.exists()
        assert manager.state == {"feeds": {}}


class TestStateManagerIsPosted:
    """Tests for is_posted method."""

    def test_returns_false_for_new_feed(self, tmp_path):
        """Test that is_posted returns False for new feed."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        assert not manager.is_posted("nba", "newguid123")

    def test_returns_false_for_unposted_guid(self, tmp_path):
        """Test that is_posted returns False for unposted GUID."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)
        manager.mark_posted("nba", "existingguid", "at://test")

        assert not manager.is_posted("nba", "differentguid")

    def test_returns_true_for_posted_guid(self, tmp_path):
        """Test that is_posted returns True for posted GUID."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)
        manager.mark_posted("nba", "postedguid", "at://test")

        assert manager.is_posted("nba", "postedguid")


class TestStateManagerMarkPosted:
    """Tests for mark_posted method."""

    def test_marks_guid_as_posted(self, tmp_path):
        """Test that GUID is marked as posted."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "testguid", "at://did:plc:test/post/abc")

        assert manager.is_posted("nba", "testguid")

    def test_saves_state_to_file(self, tmp_path):
        """Test that state is persisted to file."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "testguid", "at://test")

        # Create new manager from same file
        manager2 = StateManager(state_file)
        assert manager2.is_posted("nba", "testguid")

    def test_stores_post_uri(self, tmp_path):
        """Test that post URI is stored."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "testguid", "at://did:plc:test/post/xyz")

        # Check the stored data
        posted_guids = manager.state["feeds"]["nba"]["posted_guids"]
        entry = next(e for e in posted_guids if e["guid"] == "testguid")
        assert entry["post_uri"] == "at://did:plc:test/post/xyz"

    def test_stores_timestamp(self, tmp_path):
        """Test that posted_at timestamp is stored."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "testguid", "at://test")

        posted_guids = manager.state["feeds"]["nba"]["posted_guids"]
        entry = next(e for e in posted_guids if e["guid"] == "testguid")
        # Should be a valid ISO timestamp
        datetime.fromisoformat(entry["posted_at"])


class TestStateManagerAtomicSave:
    """Tests for atomic save functionality."""

    def test_atomic_write_uses_temp_file(self, tmp_path):
        """Test that atomic write uses temp file."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        # The temp file should not exist after successful save
        temp_file = tmp_path / "state.json.tmp"
        manager.mark_posted("nba", "test", "at://test")
        assert not temp_file.exists()

    def test_write_error_cleans_up_temp_file(self, tmp_path):
        """Test that temp file is cleaned up on write error."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        temp_file = tmp_path / "state.json.tmp"

        # Mock rename to fail
        with patch("pathlib.Path.rename", side_effect=OSError("Mock error")):
            with pytest.raises(OSError):
                manager._save_state({"feeds": {}})

        # Temp file should be cleaned up
        assert not temp_file.exists()


class TestStateManagerCleanup:
    """Tests for cleanup_old_entries method."""

    def test_removes_entries_older_than_max_age(self, tmp_path):
        """Test that entries older than max_age_days are removed."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file, max_age_days=7)

        # Add old entry manually
        old_date = (datetime.now() - timedelta(days=10)).isoformat()
        manager.state["feeds"]["nba"] = {
            "posted_guids": [{"guid": "old", "post_uri": "at://old", "posted_at": old_date}],
            "last_successful_run": None,
        }

        # Add new entry
        manager.mark_posted("nba", "new", "at://new")

        # Old entry should be removed after cleanup (triggered by mark_posted)
        assert not manager.is_posted("nba", "old")
        assert manager.is_posted("nba", "new")

    def test_keeps_entries_within_max_guids(self, tmp_path):
        """Test that only max_guids_per_feed entries are kept."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file, max_guids_per_feed=3)

        # Add 5 entries
        for i in range(5):
            manager.mark_posted("nba", f"guid{i}", f"at://uri{i}")

        # Only most recent 3 should remain
        assert manager.get_posted_count("nba") == 3

    def test_handles_malformed_timestamps(self, tmp_path):
        """Test that malformed timestamps are handled gracefully."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        # Add entry with malformed timestamp
        manager.state["feeds"]["nba"] = {
            "posted_guids": [
                {"guid": "malformed", "post_uri": "at://test", "posted_at": "not-a-date"},
                {"guid": "valid", "post_uri": "at://test2", "posted_at": datetime.now().isoformat()},
            ],
            "last_successful_run": None,
        }

        # Cleanup should handle malformed entry without crashing
        manager.cleanup_old_entries("nba")

        # Malformed entry should be removed, valid should remain
        assert not manager.is_posted("nba", "malformed")
        assert manager.is_posted("nba", "valid")


class TestStateManagerLastRun:
    """Tests for get_last_run and update_last_run methods."""

    def test_get_last_run_returns_none_for_new_feed(self, tmp_path):
        """Test that get_last_run returns None for new feed."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        assert manager.get_last_run("nba") is None

    def test_update_and_get_last_run(self, tmp_path):
        """Test updating and retrieving last run timestamp."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        now = datetime.now()
        manager.update_last_run("nba", now)

        result = manager.get_last_run("nba")
        # Compare without microseconds (ISO format may lose precision)
        assert result.replace(microsecond=0) == now.replace(microsecond=0)

    def test_last_run_persisted_to_file(self, tmp_path):
        """Test that last run is persisted to file."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        now = datetime.now()
        manager.update_last_run("nba", now)

        # Create new manager from same file
        manager2 = StateManager(state_file)
        result = manager2.get_last_run("nba")
        assert result.replace(microsecond=0) == now.replace(microsecond=0)


class TestStateManagerGetPostedGuids:
    """Tests for get_all_posted_guids method."""

    def test_returns_empty_list_for_new_feed(self, tmp_path):
        """Test that empty list is returned for new feed."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        assert manager.get_all_posted_guids("nba") == []

    def test_returns_all_guids(self, tmp_path):
        """Test that all posted GUIDs are returned."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "guid1", "at://1")
        manager.mark_posted("nba", "guid2", "at://2")
        manager.mark_posted("nba", "guid3", "at://3")

        guids = manager.get_all_posted_guids("nba")
        assert set(guids) == {"guid1", "guid2", "guid3"}


class TestStateManagerGetPostedCount:
    """Tests for get_posted_count method."""

    def test_returns_zero_for_new_feed(self, tmp_path):
        """Test that zero is returned for new feed."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        assert manager.get_posted_count("nba") == 0

    def test_returns_correct_count(self, tmp_path):
        """Test that correct count is returned."""
        state_file = tmp_path / "state.json"
        manager = StateManager(state_file)

        manager.mark_posted("nba", "guid1", "at://1")
        manager.mark_posted("nba", "guid2", "at://2")

        assert manager.get_posted_count("nba") == 2
