"""
Main Orchestration Script for Kagi News Aggregator.

Coordinates all components to:
1. Fetch RSS feeds
2. Parse HTML content
3. Format as rich text
4. Deduplicate stories
5. Post to Coves communities
6. Track state

Designed to run via CRON (single execution, then exit).
"""
import os
import sys
import logging
from pathlib import Path
from datetime import datetime
from typing import Optional

from src.config import ConfigLoader
from src.rss_fetcher import RSSFetcher
from src.html_parser import KagiHTMLParser
from src.richtext_formatter import RichTextFormatter
from src.state_manager import StateManager
from src.coves_client import CovesClient
from src.semantic_dedup import SemanticDeduplicator

# Setup logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)


class Aggregator:
    """
    Main aggregator orchestration.

    Coordinates all components to fetch, parse, format, and post stories.
    """

    def __init__(
        self,
        config_path: Path,
        state_file: Path,
        coves_client: Optional[CovesClient] = None,
        semantic_dedup: Optional[SemanticDeduplicator] = None
    ):
        """
        Initialize aggregator.

        Args:
            config_path: Path to config.yaml
            state_file: Path to state.json
            coves_client: Optional CovesClient (for testing)
            semantic_dedup: Optional SemanticDeduplicator (for testing)
        """
        # Load configuration
        logger.info("Loading configuration...")
        config_loader = ConfigLoader(config_path)
        self.config = config_loader.load()

        # Initialize components
        logger.info("Initializing components...")
        self.rss_fetcher = RSSFetcher()
        self.html_parser = KagiHTMLParser()
        self.richtext_formatter = RichTextFormatter()
        self.state_manager = StateManager(state_file)
        self.state_file = state_file

        # Initialize Coves client (or use provided one for testing)
        if coves_client:
            self.coves_client = coves_client
        else:
            # Get API key from environment
            api_key = os.getenv('COVES_API_KEY')

            if not api_key:
                raise ValueError(
                    "COVES_API_KEY environment variable required"
                )

            self.coves_client = CovesClient(
                api_url=self.config.coves_api_url,
                api_key=api_key
            )

        # Initialize semantic deduplicator (or use provided one for testing)
        if semantic_dedup:
            self.semantic_dedup = semantic_dedup
        elif self.config.dedup.semantic_enabled:
            anthropic_key = os.getenv('ANTHROPIC_API_KEY')
            if not anthropic_key:
                raise ValueError(
                    "ANTHROPIC_API_KEY environment variable required when "
                    "dedup.semantic_enabled is true. Set the env var or "
                    "set dedup.semantic_enabled: false in config."
                )
            self.semantic_dedup = SemanticDeduplicator(
                api_key=anthropic_key,
                threshold=self.config.dedup.similarity_threshold
            )
            logger.info("Semantic deduplication enabled")
        else:
            self.semantic_dedup = None

    def run(self):
        """
        Run aggregator: fetch, parse, post, and update state.

        This is the main entry point for CRON execution.
        """
        logger.info("=" * 60)
        logger.info("Starting Kagi News Aggregator")
        logger.info("=" * 60)

        # Get enabled feeds only
        enabled_feeds = [f for f in self.config.feeds if f.enabled]
        logger.info(f"Processing {len(enabled_feeds)} enabled feeds")

        # Authenticate once at the start
        try:
            self.coves_client.authenticate()
        except Exception as e:
            logger.error(f"Failed to authenticate: {e}")
            logger.error("Cannot continue without authentication")
            return

        # Process each feed
        for feed_config in enabled_feeds:
            try:
                self._process_feed(feed_config)
            except Exception as e:
                # Log error but continue with other feeds
                logger.error(f"Error processing feed '{feed_config.name}': {e}", exc_info=True)
                continue

        logger.info("=" * 60)
        logger.info("Aggregator run completed")
        logger.info("=" * 60)

    def _process_feed(self, feed_config):
        """
        Process a single RSS feed.

        Three phases:
        1. Parse all entries, filter by exact GUID match
        2. Filter by semantic similarity (if enabled)
        3. Post remaining candidates

        Args:
            feed_config: FeedConfig object
        """
        logger.info(f"Processing feed: {feed_config.name} -> {feed_config.community_handle}")

        # Fetch RSS feed
        try:
            feed = self.rss_fetcher.fetch_feed(feed_config.url)
        except Exception as e:
            logger.error(f"Failed to fetch feed '{feed_config.name}': {e}")
            raise

        # Check for feed errors
        if feed.bozo:
            logger.warning(f"Feed '{feed_config.name}' has parsing issues (bozo flag set)")

        # Phase 1: Parse all entries, filter by exact GUID
        # Store as (entry_guid, story) tuples to preserve the authoritative GUID
        candidates = []
        skipped_guid = 0

        for entry in feed.entries:
            try:
                guid = entry.guid if hasattr(entry, 'guid') else entry.link
                if self.state_manager.is_posted(feed_config.url, guid):
                    skipped_guid += 1
                    logger.debug(f"Skipping already-posted story: {guid}")
                    continue

                story = self.html_parser.parse_to_story(
                    title=entry.title,
                    link=entry.link,
                    guid=guid,
                    pub_date=entry.published_parsed,
                    categories=[tag.term for tag in entry.tags] if hasattr(entry, 'tags') else [],
                    html_description=entry.description
                )
                candidates.append((guid, story))

            except Exception as e:
                logger.error(f"Error processing entry: {e}", exc_info=True)
                continue

        # Phase 2: Semantic dedup (within same feed only)
        skipped_semantic = 0
        if self.semantic_dedup and candidates:
            recent_stories = self.state_manager.get_recent_stories(
                feed_config.url, self.config.dedup.lookback_days
            )
            if recent_stories:
                new_for_comparison = [
                    {"id": guid, "title": story.title, "summary": (story.summary or "")[:200]}
                    for guid, story in candidates
                ]
                duplicate_ids = self.semantic_dedup.find_duplicates(
                    new_for_comparison, recent_stories
                )
                before_count = len(candidates)
                candidates = [(g, s) for g, s in candidates if g not in duplicate_ids]
                skipped_semantic = before_count - len(candidates)

        # Phase 3: Post remaining candidates
        new_posts = 0
        failed_posts = 0
        for guid, story in candidates:
            try:
                rich_text = self.richtext_formatter.format_full(story)

                sources = [
                    {"uri": s.url, "title": s.title, "domain": s.domain}
                    for s in story.sources
                ] if story.sources else None

                embed = self.coves_client.create_external_embed(
                    uri=story.link,
                    title=story.title,
                    description=story.summary[:200] if len(story.summary) > 200 else story.summary,
                    sources=sources
                )

                post_uri = self.coves_client.create_post(
                    community_handle=feed_config.community_handle,
                    title=story.title,
                    content=rich_text["content"],
                    facets=rich_text["facets"],
                    embed=embed,
                    thumbnail_url=story.image_url
                )

                self.state_manager.mark_posted(
                    feed_config.url, guid, post_uri,
                    title=story.title,
                    summary_snippet=story.summary[:200]
                )
                new_posts += 1
                logger.info(f"Posted: {story.title[:50]}... -> {post_uri}")

            except Exception as e:
                failed_posts += 1
                logger.error(f"Failed to post story '{story.title}' in feed '{feed_config.name}': {e}")
                continue

        # Update last run timestamp
        self.state_manager.update_last_run(feed_config.url, datetime.now())

        logger.info(
            f"Feed '{feed_config.name}': {new_posts} new, "
            f"{failed_posts} failed, "
            f"{skipped_guid} exact dupes, {skipped_semantic} semantic dupes"
        )


def main():
    """
    Main entry point for command-line execution.

    Usage:
        python -m src.main
    """
    # Get paths from environment or use defaults
    config_path = Path(os.getenv('CONFIG_PATH', 'config.yaml'))
    state_file = Path(os.getenv('STATE_FILE', 'data/state.json'))

    # Validate config file exists
    if not config_path.exists():
        logger.error(f"Configuration file not found: {config_path}")
        logger.error("Please create config.yaml (see config.example.yaml)")
        sys.exit(1)

    # Create aggregator and run
    try:
        aggregator = Aggregator(
            config_path=config_path,
            state_file=state_file
        )
        aggregator.run()
        sys.exit(0)
    except Exception as e:
        logger.error(f"Aggregator failed: {e}", exc_info=True)
        sys.exit(1)


if __name__ == '__main__':
    main()
