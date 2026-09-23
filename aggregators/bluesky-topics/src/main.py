"""
Main orchestration for the Bluesky Topics aggregator.

Per enabled topic: collect feed and list items, filter and rank candidates,
fetch author bios, classify with Claude, select the top on-topic posts, and
cross-post them to the topic's Coves community. Designed to run from cron
(single execution, then exit). DRY_RUN=1 runs everything except posting and
touches no state.
"""
import logging
import os
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Set

import anthropic
import requests

from src.bluesky_client import BlueskyAPIError, BlueskyClient, parse_post_view
from src.candidates import filter_candidates, rank_candidates, score
from src.classifier import DEFAULT_BATCH_SIZE, ClassificationError, TopicClassifier
from src.community_feed import CommunityFeedError, CommunityFeedReader
from src.config import ConfigLoader
from src.coves_client import CovesAPIError, CovesClient
from src.models import BlueskyPost, TopicConfig
from src.posting import already_posted_uris, build_embed, build_title, select_posts
from src.state_manager import StateManager
from src.story_log import StoryLog

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)

STATE_MAX_GUIDS_PER_TOPIC = 500
STATE_MAX_AGE_DAYS = 30
DRY_RUN_TABLE_TITLE_CHARACTERS = 80


def is_dry_run() -> bool:
    """Exactly the values docker-entrypoint.sh treats as a dry run."""
    return os.getenv("DRY_RUN", "") in ("1", "true")


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


class Aggregator:
    """Runs the topic pipeline once for every enabled topic."""

    def __init__(
        self,
        config_path: Path,
        state_file: Path,
        coves_client: Optional[CovesClient] = None,
        classifier=None,
        now: Optional[Callable[[], datetime]] = None,
        bluesky_client=None,
        community_feed_reader=None,
    ):
        self.config = ConfigLoader(config_path).load()
        self.now = now or utc_now
        self.dry_run = is_dry_run()
        self.bluesky_client = bluesky_client or BlueskyClient(self.config.bluesky_api_url)
        self.classifier = classifier or TopicClassifier(anthropic.Anthropic(), self.config.classifier_model)
        self.community_feed_reader = community_feed_reader or CommunityFeedReader(self.config.coves_api_url)

        if self.dry_run:
            logger.info("DRY_RUN set: no Coves client, no state file; showing what a fresh run would pick")
            self.coves_client = None
            self.state_manager = None
            self.story_log = None
        else:
            self.coves_client = coves_client or CovesClient(self.config.coves_api_url, os.environ["COVES_API_KEY"])
            self.state_manager = StateManager(
                state_file, max_guids_per_feed=STATE_MAX_GUIDS_PER_TOPIC, max_age_days=STATE_MAX_AGE_DAYS
            )
            self.story_log = StoryLog(state_file.parent / "stories.json")

    def run(self) -> None:
        if not self.dry_run:
            logger.info("Authenticating with Coves...")
            self.coves_client.authenticate()

        for topic in self.config.topics:
            if not topic.enabled:
                logger.info(f"Topic '{topic.name}' is disabled; skipping")
                continue
            try:
                self._process_topic(topic)
            except Exception as error:
                logger.error(f"Topic '{topic.name}' failed: {error}", exc_info=True)

    def _process_topic(self, topic: TopicConfig) -> None:
        logger.info(f"Processing topic '{topic.name}'")
        now = self.now()

        posts = self._collect(topic)
        if posts is None:
            logger.error(f"Topic '{topic.name}': every source failed; skipping")
            return

        window_start = now - timedelta(hours=topic.window_hours)
        eligible = filter_candidates(posts, topic=topic, now=now, window_hours=topic.window_hours)
        candidates = rank_candidates(eligible, self.config.classify_candidates)
        logger.info(
            f"Topic '{topic.name}': {len(posts)} collected, {len(eligible)} eligible, "
            f"{len(candidates)} sent to the classifier"
        )
        if not candidates:
            if not self.dry_run:
                self._finish_topic(topic, now)
            return

        community_uris = self._community_external_uris(topic, window_start)
        if community_uris is None:
            return

        already_posted = set() if self.dry_run else set(self.state_manager.get_all_posted_guids(topic.name))
        already_posted |= already_posted_uris(candidates, community_uris)

        logged_entries: List[BlueskyPost] = []
        if not self.dry_run:
            candidate_uris = {post.uri for post in candidates}
            for logged in self.story_log.recent(topic.name, window_start):
                already_posted.add(logged.uri)
                if logged.uri not in candidate_uris:
                    logged_entries.append(logged)

        anchors = [post for post in candidates if post.uri in already_posted]
        fresh = [post for post in candidates if post.uri not in already_posted]
        fitted = self._fit_one_batch(topic, anchors, fresh, len(logged_entries))
        if fitted is None:
            if not self.dry_run:
                self._finish_topic(topic, now)
            return
        classifier_input = fitted + logged_entries
        required_ids = [
            index for index, post in enumerate(classifier_input) if post.uri in already_posted
        ]

        bios = self._fetch_bios(classifier_input)
        try:
            verdicts = self.classifier.classify(
                topic.description, classifier_input, bios, required_ids=required_ids
            )
        except ClassificationError as error:
            logger.error(f"Topic '{topic.name}': classification failed, posting nothing: {error}")
            return

        selected = select_posts(
            classifier_input,
            verdicts,
            min_confidence=self.config.min_confidence,
            limit=topic.max_posts_per_run,
            already_posted=already_posted,
        )

        if self.dry_run:
            self._log_ranked_table(topic, classifier_input, verdicts, selected, already_posted)
            return

        for post in selected:
            self._post(topic, post, now)
        self._finish_topic(topic, now)

    def _fit_one_batch(
        self,
        topic: TopicConfig,
        anchors: List[BlueskyPost],
        fresh: List[BlueskyPost],
        logged_entry_count: int,
    ) -> Optional[List[BlueskyPost]]:
        """
        Drop the lowest-ranked fresh candidates so the classifier input fits a
        single batch. Story keys are only consistent within one call, so a
        second batch would silently weaken story dedup.

        Anchors are already-posted candidates whose story must stay blocked;
        they are never dropped, because a dropped anchor stops blocking. None
        when anchors and story-log entries leave no room for a fresh candidate.
        """
        batch_size = getattr(self.classifier, "batch_size", DEFAULT_BATCH_SIZE)
        reserved = len(anchors) + logged_entry_count
        if reserved >= batch_size:
            logger.warning(
                f"Topic '{topic.name}': {reserved} already-posted posts fill the classifier batch "
                f"of {batch_size}, leaving no room for new candidates; classifying nothing"
            )
            return None
        kept = fresh[: batch_size - reserved]
        if len(kept) < len(fresh):
            logger.info(
                f"Topic '{topic.name}': classifier batch holds {batch_size}; dropping "
                f"{len(fresh) - len(kept)} lowest-ranked candidates so the story log fits one batch"
            )
        return kept + anchors

    def _community_external_uris(self, topic: TopicConfig, since: datetime) -> Optional[Set[str]]:
        """
        External links already posted to the topic's community.

        Empty when the feed could not be read, which drops dedup against the
        community for this run; None when the community handle itself looks
        wrong (a 4xx), which means the topic must not post at all.
        """
        try:
            return self.community_feed_reader.external_uris_since(topic.community_handle, since)
        except CommunityFeedError as error:
            if 400 <= error.status_code < 500:
                logger.error(
                    f"Topic '{topic.name}': community feed for {topic.community_handle} returned "
                    f"HTTP {error.status_code}; skipping the topic: {error}"
                )
                return None
            logger.error(
                f"Topic '{topic.name}': community feed lookup for {topic.community_handle} failed, "
                f"posting without dedup against the community: {error}"
            )
            return set()

    def _collect(self, topic: TopicConfig) -> Optional[List[BlueskyPost]]:
        """Collect posts from every source; None when every source failed."""
        sources = [(self.bluesky_client.get_feed, uri) for uri in topic.feeds]
        sources += [(self.bluesky_client.get_list_feed, uri) for uri in topic.lists]

        posts: List[BlueskyPost] = []
        usable_sources = 0
        for fetch, uri in sources:
            try:
                result = fetch(uri)
            except BlueskyAPIError as error:
                logger.warning(f"Topic '{topic.name}': source {uri} failed, skipping it: {error}")
                continue
            usable_sources += 1
            if result.partial:
                logger.warning(f"Topic '{topic.name}': source {uri} returned partial results")
            for item in result.items:
                post = parse_post_view(item)
                if post is not None:
                    posts.append(post)

        if sources and usable_sources == 0:
            return None
        return posts

    def _fetch_bios(self, candidates: List[BlueskyPost]) -> Dict[str, str]:
        """One getProfile per distinct author; failures cached as an empty bio."""
        bios: Dict[str, str] = {}
        for post in candidates:
            if post.author_did in bios:
                continue
            try:
                bios[post.author_did] = self.bluesky_client.get_profile(post.author_did)
            except BlueskyAPIError as error:
                logger.warning(f"Profile lookup for {post.author_did} failed, using empty bio: {error}")
                bios[post.author_did] = ""
        return bios

    def _post(self, topic: TopicConfig, post: BlueskyPost, now: datetime) -> None:
        title = build_title(post)
        try:
            embed = build_embed(post, self.coves_client)
            coves_uri = self.coves_client.create_post(
                community_handle=topic.community_handle, title=title, content="", facets=[], embed=embed
            )
        except (CovesAPIError, ValueError, requests.RequestException) as error:
            logger.error(f"Topic '{topic.name}': failed to post {post.uri}: {error}")
            return
        try:
            self.state_manager.mark_posted(topic.name, post.uri, coves_uri)
            self.story_log.record(topic.name, post, now)
        except Exception as error:
            logger.error(
                f"Topic '{topic.name}': posted {post.uri} as {coves_uri} but recording it failed, "
                f"so it may be posted again: {error}"
            )
            return
        logger.info(f"Topic '{topic.name}': posted {post.uri} as {coves_uri}: {title}")

    def _finish_topic(self, topic: TopicConfig, now: datetime) -> None:
        self.state_manager.update_last_run(topic.name, now)
        logger.info(f"Topic '{topic.name}' complete")

    @staticmethod
    def _log_ranked_table(
        topic: TopicConfig, candidates: List[BlueskyPost], verdicts, selected, already_posted: Set[str]
    ) -> None:
        verdict_by_id = {verdict.id: verdict for verdict in verdicts}
        selected_uris = {post.uri for post in selected}
        lines = [f"DRY RUN ranked table for topic '{topic.name}' ({len(selected)} would be posted):"]
        for index, post in enumerate(candidates):
            verdict = verdict_by_id.get(index)
            on_topic = "on-topic" if verdict is not None and verdict.on_topic else "off-topic"
            confidence = verdict.confidence if verdict is not None else 0.0
            story = verdict.story if verdict is not None else post.uri
            marker = "*" if post.uri in selected_uris else " "
            posted = "[posted]" if post.uri in already_posted else ""
            lines.append(
                f"{marker} score={score(post):>5} {post.author_handle:<32} {on_topic:<9} "
                f"{confidence:.2f} {story} {build_title(post)[:DRY_RUN_TABLE_TITLE_CHARACTERS]} {posted}".rstrip()
            )
        logger.info("\n".join(lines))


def main() -> None:
    """Command-line entry point: python -m src.main"""
    config_path = Path(os.getenv("CONFIG_PATH", "config.yaml"))
    state_file = Path(os.getenv("STATE_FILE", "data/state.json"))

    if not config_path.exists():
        logger.error(f"Configuration file not found: {config_path}")
        logger.error("Please create config.yaml (see config.example.yaml)")
        sys.exit(1)

    try:
        aggregator = Aggregator(config_path=config_path, state_file=state_file)
        logging.getLogger().setLevel(aggregator.config.log_level.value.upper())
        aggregator.run()
        sys.exit(0)
    except Exception as error:
        logger.error(f"Aggregator failed: {error}", exc_info=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
