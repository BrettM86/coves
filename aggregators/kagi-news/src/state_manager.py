"""
State Manager for tracking posted stories.

Handles deduplication by tracking which stories have already been posted.
Uses JSON file for persistence.
"""
import json
import logging
import os
import tempfile
from pathlib import Path
from datetime import datetime, timedelta
from typing import Optional, Dict, List

logger = logging.getLogger(__name__)


class StateManager:
    """
    Manages aggregator state for deduplication.

    Tracks:
    - Posted GUIDs per feed (with timestamps)
    - GUIDs suppressed as semantic duplicates per feed (with timestamps)
    - Last successful run timestamp per feed
    - Automatic cleanup of old entries
    """

    def __init__(self, state_file: Path, max_guids_per_feed: int = 100, max_age_days: int = 30):
        """
        Initialize state manager.

        Args:
            state_file: Path to JSON state file
            max_guids_per_feed: Maximum GUIDs to keep per feed (default: 100)
            max_age_days: Maximum age in days for GUIDs (default: 30)
        """
        self.state_file = Path(state_file)
        self.max_guids_per_feed = max_guids_per_feed
        self.max_age_days = max_age_days
        self.state = self._load_state()

    def _load_state(self) -> Dict:
        """Load state from file, or create new state if file doesn't exist."""
        if not self.state_file.exists():
            logger.info(f"Creating new state file at {self.state_file}")
            state = {'feeds': {}}
            self._save_state(state)
            return state

        try:
            with open(self.state_file, 'r') as f:
                state = json.load(f)
                logger.info(f"Loaded state from {self.state_file}")
                return state
        except json.JSONDecodeError as e:
            logger.error(f"Failed to load state file: {e}. Creating new state.")
            state = {'feeds': {}}
            self._save_state(state)
            return state

    def _save_state(self, state: Optional[Dict] = None):
        """Save state to file."""
        if state is None:
            state = self.state

        # Ensure parent directory exists
        self.state_file.parent.mkdir(parents=True, exist_ok=True)

        # Atomic write: write to temp file then rename to avoid corruption
        fd, tmp_path = tempfile.mkstemp(
            dir=str(self.state_file.parent), suffix='.tmp'
        )
        try:
            with os.fdopen(fd, 'w') as f:
                json.dump(state, f, indent=2)
            os.replace(tmp_path, str(self.state_file))
        except BaseException:
            # Clean up temp file on any failure
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
            raise

    def _ensure_feed_exists(self, feed_url: str):
        """Ensure feed entry exists in state, including lists added after first release."""
        if feed_url not in self.state['feeds']:
            self.state['feeds'][feed_url] = {
                'posted_guids': [],
                'last_successful_run': None,
                'suppressed_guids': []
            }
            return

        # State files written before suppression tracking existed lack the key.
        self.state['feeds'][feed_url].setdefault('suppressed_guids', [])

    def is_posted(self, feed_url: str, guid: str) -> bool:
        """
        Check if a story has already been posted.

        Args:
            feed_url: RSS feed URL
            guid: Story GUID

        Returns:
            True if already posted, False otherwise
        """
        self._ensure_feed_exists(feed_url)

        posted_guids = self.state['feeds'][feed_url]['posted_guids']
        return any(entry['guid'] == guid for entry in posted_guids)

    def mark_posted(self, feed_url: str, guid: str, post_uri: str,
                    title: str = "", summary_snippet: str = ""):
        """
        Mark a story as posted.

        Args:
            feed_url: RSS feed URL
            guid: Story GUID
            post_uri: AT Proto URI of created post
            title: Story title (for semantic dedup)
            summary_snippet: First 200 chars of summary (for semantic dedup)
        """
        self._ensure_feed_exists(feed_url)

        # Add to posted list
        entry = {
            'guid': guid,
            'post_uri': post_uri,
            'posted_at': datetime.now().isoformat(),
            'title': title,
            'summary_snippet': summary_snippet[:200] if summary_snippet else ""
        }
        self.state['feeds'][feed_url]['posted_guids'].append(entry)

        # Auto-cleanup to keep state file manageable
        self.cleanup_old_entries(feed_url)

        # Save state
        self._save_state()

        logger.info(f"Marked as posted: {guid} -> {post_uri}")

    def is_suppressed(self, feed_url: str, guid: str) -> bool:
        """
        Check if a story was suppressed as a semantic duplicate.

        Args:
            feed_url: RSS feed URL
            guid: Story GUID

        Returns:
            True if already suppressed for this feed, False otherwise
        """
        self._ensure_feed_exists(feed_url)

        suppressed_guids = self.state['feeds'][feed_url]['suppressed_guids']
        return any(entry['guid'] == guid for entry in suppressed_guids)

    def mark_suppressed(self, feed_url: str, guid: str, duplicate_of: str):
        """
        Record a story that was withheld as a semantic duplicate.

        Suppressed stories were never published, so they are tracked separately
        from posted_guids. Remembering them keeps the next run from offering the
        same candidate to the model again.

        Args:
            feed_url: RSS feed URL
            guid: Story GUID that was suppressed
            duplicate_of: GUID of the already posted story it duplicates
        """
        self._ensure_feed_exists(feed_url)

        entry = {
            'guid': guid,
            'duplicate_of': duplicate_of,
            'suppressed_at': datetime.now().isoformat()
        }
        self.state['feeds'][feed_url]['suppressed_guids'].append(entry)

        # Auto-cleanup to keep state file manageable
        self.cleanup_old_entries(feed_url)

        # Save state
        self._save_state()

        logger.info(f"Marked as suppressed: {guid} (duplicate of {duplicate_of})")

    def get_last_run(self, feed_url: str) -> Optional[datetime]:
        """
        Get last successful run timestamp for a feed.

        Args:
            feed_url: RSS feed URL

        Returns:
            Datetime of last run, or None if never run
        """
        self._ensure_feed_exists(feed_url)

        timestamp_str = self.state['feeds'][feed_url]['last_successful_run']
        if timestamp_str is None:
            return None

        return datetime.fromisoformat(timestamp_str)

    def update_last_run(self, feed_url: str, timestamp: datetime):
        """
        Update last successful run timestamp.

        Args:
            feed_url: RSS feed URL
            timestamp: Timestamp of successful run
        """
        self._ensure_feed_exists(feed_url)

        self.state['feeds'][feed_url]['last_successful_run'] = timestamp.isoformat()
        self._save_state()

        logger.info(f"Updated last run for {feed_url}: {timestamp}")

    def cleanup_old_entries(self, feed_url: str):
        """
        Remove old entries from state.

        Removes entries that are:
        - Older than max_age_days
        - Beyond max_guids_per_feed limit (keeps most recent)

        Posted and suppressed entries get the same age limit and the same size
        limit, but each list is trimmed on its own budget, so suppressions never
        evict posted history.

        Args:
            feed_url: RSS feed URL
        """
        self._ensure_feed_exists(feed_url)

        self._cleanup_entry_list(feed_url, 'posted_guids', 'posted_at')
        self._cleanup_entry_list(feed_url, 'suppressed_guids', 'suppressed_at')

    def _cleanup_entry_list(self, feed_url: str, list_key: str, timestamp_key: str):
        """Age out and cap one per-feed entry list, most recent kept.

        An entry whose timestamp is missing or not ISO-parsable is dropped with a
        WARNING naming it, rather than raising: cleanup runs on every write, so
        one corrupt entry would otherwise stop the feed recording anything again
        and lose the write that triggered the cleanup with it.
        """
        entries = self.state['feeds'][feed_url][list_key]

        # Filter out entries older than max_age_days
        cutoff_date = datetime.now() - timedelta(days=self.max_age_days)
        filtered = []
        for entry in entries:
            timestamp = entry.get(timestamp_key)
            try:
                recorded_at = datetime.fromisoformat(timestamp)
            except (TypeError, ValueError):
                logger.warning(
                    f"Dropping {list_key} entry {entry.get('guid')!r} for {feed_url}: "
                    f"{timestamp_key} {timestamp!r} is missing or not ISO-parsable"
                )
                continue
            if recorded_at > cutoff_date:
                filtered.append(entry)

        # Keep only most recent max_guids_per_feed entries
        filtered.sort(key=lambda entry: entry[timestamp_key], reverse=True)
        filtered = filtered[:self.max_guids_per_feed]

        # Update state
        old_count = len(entries)
        new_count = len(filtered)
        self.state['feeds'][feed_url][list_key] = filtered

        if old_count != new_count:
            logger.info(
                f"Cleaned up {old_count - new_count} old {list_key} entries for {feed_url}"
            )

    def get_recent_stories(self, feed_url: str, days: int = 4) -> List[Dict]:
        """
        Get recently posted stories with title and summary for semantic comparison.

        Only posted stories are returned. Suppressed stories were never shown to
        the community, so a later candidate is compared against the post that
        suppressed them, not against the suppressed story itself.

        Args:
            feed_url: RSS feed URL
            days: Number of days to look back (default: 4)

        Returns:
            List of dicts with keys: id, title, summary
            Only includes entries that have title data (backward compatible).
        """
        self._ensure_feed_exists(feed_url)

        cutoff = datetime.now() - timedelta(days=days)
        recent = []

        for entry in self.state['feeds'][feed_url]['posted_guids']:
            try:
                # Skip entries without title (old format)
                title = entry.get('title', '')
                if not title:
                    continue

                posted_at = datetime.fromisoformat(entry['posted_at'])
                if posted_at > cutoff:
                    recent.append({
                        'id': entry['guid'],
                        'title': title,
                        'summary': entry.get('summary_snippet', '')
                    })
            except (ValueError, KeyError) as e:
                logger.warning(
                    f"Skipping malformed state entry for feed '{feed_url}': {e}"
                )
                continue

        return recent

    def get_posted_count(self, feed_url: str) -> int:
        """
        Get count of posted items for a feed.

        Args:
            feed_url: RSS feed URL

        Returns:
            Number of posted items
        """
        self._ensure_feed_exists(feed_url)
        return len(self.state['feeds'][feed_url]['posted_guids'])

    def get_all_posted_guids(self, feed_url: str) -> List[str]:
        """
        Get all posted GUIDs for a feed.

        Args:
            feed_url: RSS feed URL

        Returns:
            List of GUIDs
        """
        self._ensure_feed_exists(feed_url)
        return [entry['guid'] for entry in self.state['feeds'][feed_url]['posted_guids']]
