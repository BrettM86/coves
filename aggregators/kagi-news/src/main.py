"""
Main Orchestration Script for Kagi News Aggregator.

Coordinates all components to:
1. Fetch RSS (XML) and JSON sibling feeds; join clusters on cluster_number
2. Build KagiStory objects from JSON's pre-structured content
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
from datetime import datetime, timezone
from typing import Dict, Optional
from urllib.parse import urlparse

from src.citations import build_index, strip as strip_citations
from src.config import ConfigLoader
from src.rss_fetcher import RSSFetcher, JSONFetcher
from src.json_parser import KagiJSONParser
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
        self.json_fetcher = JSONFetcher()
        self.json_parser = KagiJSONParser()
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

        # Fetch RSS feed (XML) — provides link, guid, pubDate, categories, title
        try:
            feed = self.rss_fetcher.fetch_feed(feed_config.url)
        except Exception as e:
            logger.error(f"Failed to fetch feed '{feed_config.name}': {e}")
            raise

        # Check for feed errors
        if feed.bozo:
            logger.warning(f"Feed '{feed_config.name}' has parsing issues (bozo flag set)")

        # Fetch the matching JSON feed for rich, pre-structured content.
        # The .json sibling URL is derived from the configured .xml URL.
        if not feed_config.url.endswith('.xml'):
            raise ValueError(
                f"Feed '{feed_config.name}' URL must end with '.xml' to derive the "
                f"JSON sibling URL; got: {feed_config.url!r}"
            )
        json_url = feed_config.url[:-len('.xml')] + '.json'
        try:
            clusters = self.json_fetcher.fetch_clusters(json_url)
        except Exception as e:
            logger.error(f"Failed to fetch JSON feed '{json_url}': {e}")
            raise

        # Phase 1: Parse all entries, filter by exact GUID
        # Store as (entry_guid, story) tuples to preserve the authoritative GUID
        candidates = []
        skipped_guid = 0
        resolved_count = 0

        for entry in feed.entries:
            try:
                guid = entry.guid if hasattr(entry, 'guid') else entry.link
                if self.state_manager.is_posted(feed_config.url, guid):
                    skipped_guid += 1
                    logger.debug(f"Skipping already-posted story: {guid}")
                    continue

                cluster = self._resolve_json_cluster(entry, clusters)
                if cluster is None:
                    continue

                resolved_count += 1

                # Normalize feedparser's struct_time to a real timezone-aware
                # datetime so json_parser.parse_to_story's `datetime` annotation
                # is honored. Skip the entry if published_parsed is missing.
                published_parsed = getattr(entry, 'published_parsed', None)
                if published_parsed is None:
                    logger.warning(
                        f"Entry missing published_parsed; skipping: "
                        f"guid={guid!r} title={getattr(entry, 'title', '<no-title>')!r}"
                    )
                    continue
                pub_date = datetime(*published_parsed[:6], tzinfo=timezone.utc)

                story = self.json_parser.parse_to_story(
                    title=entry.title,
                    link=entry.link,
                    guid=guid,
                    pub_date=pub_date,
                    categories=[tag.term for tag in entry.tags] if hasattr(entry, 'tags') else [],
                    json_cluster=cluster,
                )
                candidates.append((guid, story))

            except Exception as e:
                logger.error(
                    f"Error processing entry "
                    f"guid={getattr(entry, 'guid', '<no-guid>')!r} "
                    f"title={getattr(entry, 'title', '<no-title>')!r}: {e}",
                    exc_info=True,
                )
                continue

        # Tripwire: 0-resolved means TITLE-format drift (or wholesale removal of
        # overlap with JSON titles). Renumberings alone are absorbed by the
        # title-scan fallback in `_resolve_json_cluster`, which only warns.
        # Continue the run across other feeds.
        #
        # Only entries that actually reached resolution can be evidence of drift.
        # An already-posted entry `continue`s above without ever calling
        # `_resolve_json_cluster`, so counting it here made the tripwire fire
        # whenever a feed was fully published -- the steady state between new
        # stories, i.e. most runs. That reduced the one alarm that detects a Kagi
        # format change to routine noise, indistinguishable from a healthy run.
        attempted = len(feed.entries) - skipped_guid
        if attempted > 0 and resolved_count == 0:
            logger.error(
                f"Feed '{feed_config.name}' resolved 0 of {attempted} unposted "
                f"entries to JSON clusters; Kagi JSON title-format may have changed "
                f"(or no XML/JSON overlap)"
            )

        # Precompute citation-stripped summaries once per candidate so the dedup
        # input, embed.description, and state snippet all use the cleaned text.
        stripped_summaries: Dict[str, str] = {
            guid: strip_citations(
                story.summary, build_index(story.sources), context=story.link
            )
            for guid, story in candidates
        }

        # Phase 2: Semantic dedup (within same feed only)
        skipped_semantic = 0
        if self.semantic_dedup and candidates:
            recent_stories = self.state_manager.get_recent_stories(
                feed_config.url, self.config.dedup.lookback_days
            )
            if recent_stories:
                new_for_comparison = [
                    {"id": guid, "title": story.title, "summary": stripped_summaries[guid][:200]}
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

                # `domain` is optional in social.coves.embed.external#source, so
                # omit it when Kagi left it blank rather than publishing an empty
                # string -- absent reads as "unknown", "" asserts a domain that
                # renders as a blank attribution chip.
                sources = [
                    {"uri": s.url, "title": s.title, **({"domain": s.domain} if s.domain else {})}
                    for s in story.sources
                ] if story.sources else None

                stripped_summary = stripped_summaries[guid]
                description = stripped_summary[:200]
                embed = self.coves_client.create_external_embed(
                    uri=story.link,
                    title=story.title,
                    description=description,
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
                    summary_snippet=stripped_summary[:200]
                )
                new_posts += 1
                logger.info(f"Posted: {story.title[:50]}... -> {post_uri}")

            except Exception as e:
                failed_posts += 1
                logger.error(
                    f"Failed to post story guid={guid!r} title={story.title!r} "
                    f"in feed '{feed_config.name}': {e}",
                    exc_info=True,
                )
                continue

        # Update last run timestamp
        self.state_manager.update_last_run(feed_config.url, datetime.now(timezone.utc))

        logger.info(
            f"Feed '{feed_config.name}': {new_posts} new, "
            f"{failed_posts} failed, "
            f"{skipped_guid} exact dupes, {skipped_semantic} semantic dupes"
        )

    @staticmethod
    def _normalize_title(s: str) -> str:
        """Normalize a title for tolerant equality: strip + casefold."""
        return (s or "").strip().casefold()

    def _resolve_json_cluster(self, entry, clusters: Dict[int, dict]) -> Optional[dict]:
        """
        Find the JSON cluster that corresponds to an XML feed entry.

        The normalized title is the join key: it is the only value Kagi carries
        identically in both feeds. A cluster number parsed out of entry.link is
        only ever an optimization, never a requirement -- Kagi has changed that
        URL shape before (on 2026-07-30 a slug was appended and the number
        gained a date prefix), and a hard dependency on it takes down every
        feed at once.

        Empirically, when the link does end in a bare number,
        JSON.cluster_number = XML.cluster_number + 1. We take that offset as the
        fast path and verify it by title equality, so a stale or misread offset
        can never resolve to the wrong cluster.

        Behavior:
        - Fast-path hit (offset cluster exists AND title matches): returns silently.
        - Title-scan fallback (exactly one cluster with the same title): returns it.
          Logs WARN "JSON join offset mismatch ..." only when a cluster number was
          parsed and its offset missed, since that signals a Kagi indexing change;
          when no number was parsable this is the ordinary path and logs DEBUG.
        - Ambiguous title (multiple clusters share the title): WARN
          "Ambiguous title match ..."; returns None.
        - No match (zero title hits): WARN
          "No matching JSON cluster for XML entry ..."; returns None.
        - Bad entry.link (missing or non-string): WARN
          "entry.link missing or not a string ..."; falls through to the title scan.
        - Unparseable trailing segment in entry.link: DEBUG; falls through to the
          title scan.
        """
        # Kagi URL patterns seen in the wild:
        #   .../<uuid>/<category>/<cluster_number>          (pre 2026-07-30)
        #   .../<category>/<YYYYMMDD><n><cluster_number>/<slug>   (current)
        # Only the first yields a directly usable number, so the parse is
        # best-effort and `xml_cn` stays None whenever it does not apply.
        xml_cn: Optional[int] = None
        link = getattr(entry, 'link', None)
        if not isinstance(link, str) or not link:
            logger.warning(
                f"entry.link missing or not a string (got {link!r}); "
                f"falling back to title match"
            )
        else:
            try:
                xml_cn = int(urlparse(link).path.rstrip('/').rsplit('/', 1)[-1])
            except ValueError:
                logger.debug(
                    f"No trailing cluster_number in {link!r}; using title match"
                )

        entry_title_norm = Aggregator._normalize_title(getattr(entry, 'title', ''))

        if xml_cn is not None:
            cluster = clusters.get(xml_cn + 1)
            if cluster is not None and Aggregator._normalize_title(cluster.get("title", "")) == entry_title_norm:
                return cluster

        matches = [
            c for c in clusters.values()
            if Aggregator._normalize_title(c.get("title", "")) == entry_title_norm
        ]
        if len(matches) == 1:
            if xml_cn is not None:
                logger.warning(
                    f"JSON join offset mismatch for cluster_number={xml_cn} "
                    f"(found by title scan instead); Kagi indexing may have changed"
                )
            else:
                logger.debug(
                    f"Resolved {getattr(entry, 'title', '')!r} by title scan "
                    f"(no cluster_number in link)"
                )
            return matches[0]

        if len(matches) > 1:
            logger.warning(
                f"Ambiguous title match for XML entry cn={xml_cn} "
                f"title={getattr(entry, 'title', '')!r}: {len(matches)} JSON clusters "
                f"share this title; skipping"
            )
            return None

        logger.warning(
            f"No matching JSON cluster for XML entry cn={xml_cn} "
            f"title={getattr(entry, 'title', '')!r}; skipping"
        )
        return None


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
