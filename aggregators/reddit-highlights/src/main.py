"""
Main Orchestration Script for Reddit Highlights Aggregator.

Coordinates all components to:
1. Apply anti-detection jitter delay
2. Fetch Reddit RSS feeds
3. Extract streamable video links
4. Deduplicate via state tracking
5. Post to Coves communities

Designed to run via CRON (single execution, then exit).
"""
import os
import re
import sys
import time
import random
import logging
from pathlib import Path
from datetime import datetime
from typing import Optional

from src.config import ConfigLoader
from src.rss_fetcher import RSSFetcher
from src.link_extractor import LinkExtractor
from src.state_manager import StateManager
from src.coves_client import CovesClient
from src.models import RedditPost

# Setup logging
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s"
)
logger = logging.getLogger(__name__)

# Reddit RSS URL template
REDDIT_RSS_URL = "https://www.reddit.com/r/{subreddit}/.rss"

# Anti-detection jitter range (0-10 minutes in seconds)
JITTER_MIN_SECONDS = 0
JITTER_MAX_SECONDS = 600


class Aggregator:
    """
    Main aggregator orchestration.

    Coordinates all components to fetch, filter, and post video highlights.
    """

    def __init__(
        self,
        config_path: Path,
        state_file: Path,
        coves_client: Optional[CovesClient] = None,
        skip_jitter: bool = False,
    ):
        """
        Initialize aggregator.

        Args:
            config_path: Path to config.yaml
            state_file: Path to state.json
            coves_client: Optional CovesClient (for testing)
            skip_jitter: Skip anti-detection delay (for testing)
        """
        self.skip_jitter = skip_jitter

        # Load configuration
        logger.info("Loading configuration...")
        config_loader = ConfigLoader(config_path)
        self.config = config_loader.load()

        # Initialize components
        logger.info("Initializing components...")
        self.rss_fetcher = RSSFetcher()
        self.link_extractor = LinkExtractor(
            allowed_domains=self.config.allowed_domains
        )
        self.state_manager = StateManager(state_file)
        self.state_file = state_file

        # Initialize Coves client (or use provided one for testing)
        if coves_client:
            self.coves_client = coves_client
        else:
            api_key = os.getenv("COVES_API_KEY")
            if not api_key:
                raise ValueError("COVES_API_KEY environment variable required")

            self.coves_client = CovesClient(
                api_url=self.config.coves_api_url, api_key=api_key
            )

    def run(self):
        """
        Run aggregator: apply jitter, fetch, filter, post, and update state.

        This is the main entry point for CRON execution.
        """
        logger.info("=" * 60)
        logger.info("Starting Reddit Highlights Aggregator")
        logger.info("=" * 60)

        # Anti-detection jitter: random delay before starting
        if not self.skip_jitter:
            jitter_seconds = random.uniform(JITTER_MIN_SECONDS, JITTER_MAX_SECONDS)
            logger.info(
                f"Applying anti-detection jitter: sleeping for {jitter_seconds:.1f} seconds "
                f"({jitter_seconds/60:.1f} minutes)"
            )
            time.sleep(jitter_seconds)

        # Get enabled subreddits only
        enabled_subreddits = [s for s in self.config.subreddits if s.enabled]
        logger.info(f"Processing {len(enabled_subreddits)} enabled subreddits")

        # Authenticate once at the start
        try:
            self.coves_client.authenticate()
        except Exception as e:
            logger.error(f"Failed to authenticate: {e}")
            logger.error("Cannot continue without authentication")
            raise RuntimeError("Authentication failed") from e

        # Process each subreddit
        for subreddit_config in enabled_subreddits:
            try:
                self._process_subreddit(subreddit_config)
            except KeyboardInterrupt:
                # Re-raise interrupt signals - don't suppress user abort
                logger.info("Received interrupt signal, stopping...")
                raise
            except Exception as e:
                # Log error but continue with other subreddits
                logger.error(
                    f"Error processing subreddit '{subreddit_config.name}': {e}",
                    exc_info=True,
                )
                continue

        logger.info("=" * 60)
        logger.info("Aggregator run completed")
        logger.info("=" * 60)

    def _process_subreddit(self, subreddit_config):
        """
        Process a single subreddit.

        Args:
            subreddit_config: SubredditConfig object
        """
        subreddit_name = subreddit_config.name
        community_handle = subreddit_config.community_handle

        # Sanitize subreddit name to prevent URL injection
        # Only allow alphanumeric, underscores, and hyphens
        if not re.match(r'^[a-zA-Z0-9_-]+$', subreddit_name):
            raise ValueError(f"Invalid subreddit name: {subreddit_name}")

        logger.info(f"Processing subreddit: r/{subreddit_name} -> {community_handle}")

        # Build RSS URL
        rss_url = REDDIT_RSS_URL.format(subreddit=subreddit_name)

        # Fetch RSS feed
        try:
            feed = self.rss_fetcher.fetch_feed(rss_url)
        except Exception as e:
            logger.error(f"Failed to fetch feed for r/{subreddit_name}: {e}")
            raise

        # Check for feed errors
        if feed.bozo:
            bozo_exception = getattr(feed, 'bozo_exception', None)
            logger.warning(
                f"Feed for r/{subreddit_name} has parsing issues (bozo flag set): {bozo_exception}"
            )

        # Find top N entries with streamable links (filter first, then limit)
        max_posts = self.config.max_posts_per_run
        new_posts = 0
        skipped_posts = 0
        no_video_count = 0
        entries_checked = 0

        logger.info(f"Scanning feed for top {max_posts} streamable entries")

        for entry in feed.entries:
            # Stop once we've found enough posts to process
            if new_posts + skipped_posts >= max_posts:
                break

            entries_checked += 1

            try:
                # Extract video URL - skip if not a streamable link
                video_url = self.link_extractor.extract_video_url(entry)
                if not video_url:
                    no_video_count += 1
                    continue  # Skip posts without video links

                # Get entry ID for deduplication
                entry_id = self._get_entry_id(entry)
                if self.state_manager.is_posted(subreddit_name, entry_id):
                    skipped_posts += 1
                    logger.debug(f"Skipping already-posted entry: {entry_id}")
                    continue

                # Parse entry into RedditPost
                reddit_post = self._parse_entry(entry, subreddit_name, video_url)

                # Create embed with sources and video metadata
                # Note: Thumbnail is fetched by backend via unfurl service
                embed = self.coves_client.create_external_embed(
                    uri=reddit_post.streamable_url,
                    title=reddit_post.title,
                    description=f"From r/{subreddit_name}",
                    sources=[
                        {
                            "uri": reddit_post.reddit_url,
                            "title": f"r/{subreddit_name}",
                            "domain": "reddit.com",
                        }
                    ],
                    embed_type="video",
                    provider="streamable",
                    domain="streamable.com",
                )

                # Post to community
                try:
                    post_uri = self.coves_client.create_post(
                        community_handle=community_handle,
                        title=reddit_post.title,
                        content="",  # No additional content needed
                        facets=[],
                        embed=embed,
                    )

                    # Mark as posted (only if successful)
                    self.state_manager.mark_posted(subreddit_name, entry_id, post_uri)
                    new_posts += 1
                    logger.info(f"Posted: {reddit_post.title[:50]}... -> {post_uri}")

                except Exception as e:
                    # Don't update state if posting failed
                    logger.error(f"Failed to post '{reddit_post.title}': {e}")
                    continue

            except Exception as e:
                # Log error but continue with other entries
                logger.error(f"Error processing entry: {e}", exc_info=True)
                continue

        # Update last run timestamp
        self.state_manager.update_last_run(subreddit_name, datetime.now())

        logger.info(
            f"r/{subreddit_name}: {new_posts} new posts, {skipped_posts} duplicates "
            f"(checked {entries_checked} entries, skipped {no_video_count} without streamable link)"
        )

    def _get_entry_id(self, entry) -> str:
        """
        Get unique identifier for RSS entry.

        Args:
            entry: feedparser entry

        Returns:
            Unique ID string
        """
        # Reddit RSS uses 'id' field with format like 't3_abc123'
        if hasattr(entry, "id") and entry.id:
            return entry.id

        # Fallback to link
        if hasattr(entry, "link") and entry.link:
            return entry.link

        # Last resort: title hash (using SHA-256 for security)
        if hasattr(entry, "title") and entry.title:
            import hashlib

            logger.warning(f"Using fallback hash for entry ID (no id or link found)")
            return hashlib.sha256(entry.title.encode()).hexdigest()

        raise ValueError("Cannot determine entry ID")

    def _parse_entry(self, entry, subreddit: str, video_url: str) -> RedditPost:
        """
        Parse RSS entry into RedditPost object.

        Args:
            entry: feedparser entry
            subreddit: Subreddit name
            video_url: Extracted video URL

        Returns:
            RedditPost object
        """
        # Get entry ID
        entry_id = self._get_entry_id(entry)

        # Get title
        title = entry.title if hasattr(entry, "title") else "Untitled"

        # Get Reddit permalink
        reddit_url = entry.link if hasattr(entry, "link") else ""

        # Get author (Reddit RSS uses 'author' field)
        author = ""
        if hasattr(entry, "author"):
            author = entry.author
        elif hasattr(entry, "author_detail") and hasattr(entry.author_detail, "name"):
            author = entry.author_detail.name

        # Get published date
        published = None
        if hasattr(entry, "published_parsed") and entry.published_parsed:
            try:
                published = datetime(*entry.published_parsed[:6])
            except (TypeError, ValueError) as e:
                logger.warning(f"Failed to parse published date for entry: {e}")

        return RedditPost(
            id=entry_id,
            title=title,
            link=entry.link if hasattr(entry, "link") else "",
            reddit_url=reddit_url,
            subreddit=subreddit,
            author=author,
            published=published,
            streamable_url=video_url,
        )


def main():
    """
    Main entry point for command-line execution.

    Usage:
        python -m src.main
    """
    # Get paths from environment or use defaults
    config_path = Path(os.getenv("CONFIG_PATH", "config.yaml"))
    state_file = Path(os.getenv("STATE_FILE", "data/state.json"))

    # Check for skip jitter flag (for testing)
    skip_jitter = os.getenv("SKIP_JITTER", "").lower() in ("true", "1", "yes")

    # Validate config file exists
    if not config_path.exists():
        logger.error(f"Configuration file not found: {config_path}")
        logger.error("Please create config.yaml (see config.example.yaml)")
        sys.exit(1)

    # Create aggregator and run
    try:
        aggregator = Aggregator(
            config_path=config_path,
            state_file=state_file,
            skip_jitter=skip_jitter,
        )
        aggregator.run()
        sys.exit(0)
    except Exception as e:
        logger.error(f"Aggregator failed: {e}", exc_info=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
