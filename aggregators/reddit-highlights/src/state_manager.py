"""
State Manager for tracking posted stories.

Handles deduplication by tracking which stories have already been posted.
Uses JSON file for persistence.
"""
import json
import logging
from pathlib import Path
from datetime import datetime, timedelta
from typing import Optional, Dict, List

logger = logging.getLogger(__name__)


class StateManager:
    """
    Manages aggregator state for deduplication.

    Tracks posted Reddit entries per subreddit to prevent duplicate posting.

    Attributes tracked per subreddit:
    - Posted GUIDs (with timestamps and Coves post URIs)
    - Last successful run timestamp
    - Automatic cleanup of old entries to prevent state file bloat

    Note: The 'feed_url' parameter in methods refers to the subreddit name
    (e.g., 'nba'), not a full RSS URL. This naming is historical but the
    functionality uses subreddit names as keys.
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
            # Backup corrupted file before overwriting
            backup_path = self.state_file.with_suffix('.json.corrupted')
            logger.error(f"State file corrupted: {e}. Backing up to {backup_path}")
            try:
                import shutil
                shutil.copy2(self.state_file, backup_path)
                logger.info(f"Corrupted state file backed up to {backup_path}")
            except OSError as backup_error:
                logger.warning(f"Failed to backup corrupted state file: {backup_error}")
            state = {'feeds': {}}
            self._save_state(state)
            return state

    def _save_state(self, state: Optional[Dict] = None):
        """
        Save state to file atomically.

        Uses write-to-temp-then-rename pattern to prevent corruption
        if the process is interrupted during write.

        Raises:
            OSError: If write fails (after logging the error)
        """
        if state is None:
            state = self.state

        # Ensure parent directory exists
        self.state_file.parent.mkdir(parents=True, exist_ok=True)

        # Write to temp file first for atomic update
        temp_file = self.state_file.with_suffix('.json.tmp')
        try:
            with open(temp_file, 'w') as f:
                json.dump(state, f, indent=2)
            # Atomic rename (on POSIX systems)
            temp_file.rename(self.state_file)
        except OSError as e:
            logger.error(f"Failed to save state file: {e}")
            # Clean up temp file if it exists
            if temp_file.exists():
                try:
                    temp_file.unlink()
                except OSError:
                    pass
            raise

    def _ensure_feed_exists(self, feed_url: str):
        """Ensure feed entry exists in state."""
        if feed_url not in self.state['feeds']:
            self.state['feeds'][feed_url] = {
                'posted_guids': [],
                'last_successful_run': None
            }

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

    def mark_posted(self, feed_url: str, guid: str, post_uri: str):
        """
        Mark a story as posted.

        Args:
            feed_url: RSS feed URL
            guid: Story GUID
            post_uri: AT Proto URI of created post
        """
        self._ensure_feed_exists(feed_url)

        # Add to posted list
        entry = {
            'guid': guid,
            'post_uri': post_uri,
            'posted_at': datetime.now().isoformat()
        }
        self.state['feeds'][feed_url]['posted_guids'].append(entry)

        # Auto-cleanup to keep state file manageable
        self.cleanup_old_entries(feed_url)

        # Save state
        self._save_state()

        logger.info(f"Marked as posted: {guid} -> {post_uri}")

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

        Args:
            feed_url: RSS feed URL
        """
        self._ensure_feed_exists(feed_url)

        posted_guids = self.state['feeds'][feed_url]['posted_guids']

        # Filter out entries older than max_age_days
        cutoff_date = datetime.now() - timedelta(days=self.max_age_days)
        filtered = []
        for entry in posted_guids:
            try:
                posted_at = datetime.fromisoformat(entry['posted_at'])
                if posted_at > cutoff_date:
                    filtered.append(entry)
            except (KeyError, ValueError) as e:
                # Skip entries with malformed or missing timestamps
                logger.warning(f"Skipping entry with invalid timestamp: {e}")
                continue

        # Keep only most recent max_guids_per_feed entries
        # Sort by posted_at (most recent first)
        filtered.sort(key=lambda x: x['posted_at'], reverse=True)
        filtered = filtered[:self.max_guids_per_feed]

        # Update state
        old_count = len(posted_guids)
        new_count = len(filtered)
        self.state['feeds'][feed_url]['posted_guids'] = filtered

        if old_count != new_count:
            logger.info(f"Cleaned up {old_count - new_count} old entries for {feed_url}")

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
