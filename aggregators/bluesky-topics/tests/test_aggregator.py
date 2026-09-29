"""
Unit tests for Aggregator.run() orchestration: source and topic failure
isolation, profile caching, classifier input, state marking, dry run, and
disabled topics.

All Bluesky traffic goes through an injected fake Bluesky client (no
`responses`); Coves is a CovesClient subclass that records create_post; the
classifier is a fake keyed by post URI; the community feed reader is a fake
recording lookups; `now` is injected.

Story log location contract assumed here: the aggregator keeps its story log
at the state file's sibling ``stories.json`` (``state_file.parent / "stories.json"``).
"""
import logging
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace

import pytest
import requests
import yaml

from src import main as main_module
from src.bluesky_client import BlueskyAPIError, FeedFetchResult, parse_post_view
from src.classifier import DEFAULT_BATCH_SIZE, ClassificationError
from src.community_feed import CommunityFeedError
from src.coves_client import CovesAPIError, CovesAuthenticationError, CovesClient
from src.main import Aggregator, is_dry_run
from src.state_manager import StateManager
from src.story_log import StoryLog

BLUESKY_API_URL = "https://bsky.test"
COVES_API_URL = "https://coves.test"
FAKE_COVES_API_KEY = "ckapi_" + "a" * 64
NOW = datetime(2026, 9, 18, 12, 0, 0, tzinfo=timezone.utc)

FEED_A = "at://did:plc:feedowner/app.bsky.feed.generator/feed-a"
FEED_B = "at://did:plc:feedowner/app.bsky.feed.generator/feed-b"
LIST_A = "at://did:plc:listowner/app.bsky.graph.list/list-a"
NBA_DESCRIPTION = "Posts about the NBA."
TENNIS_DESCRIPTION = "Posts about tennis."


def iso(age: timedelta) -> str:
    return (NOW - age).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def feed_item(rkey, like_count=10, author_did="did:plc:author", handle="author.bsky.social", age=timedelta(hours=1)):
    return {
        "post": {
            "uri": f"at://{author_did}/app.bsky.feed.post/{rkey}",
            "cid": f"bafy{rkey}",
            "author": {"did": author_did, "handle": handle, "displayName": "Author", "labels": []},
            "record": {"$type": "app.bsky.feed.post", "text": f"post {rkey}", "createdAt": iso(age)},
            "likeCount": like_count,
            "repostCount": 0,
            "replyCount": 0,
            "quoteCount": 0,
            "labels": [],
            "indexedAt": iso(age),
        }
    }


def uri_of(item) -> str:
    return item["post"]["uri"]


def topic(name, description=NBA_DESCRIPTION, feeds=(FEED_A,), lists=(), enabled=True, **overrides):
    data = {
        "name": name,
        "community_handle": f"{name}.coves.social",
        "description": description,
        "feeds": list(feeds),
        "lists": list(lists),
        "excluded_handles": [],
        "min_likes": 5,
        "enabled": enabled,
    }
    data.update(overrides)
    return data


def write_config(tmp_path: Path, topics, **overrides) -> Path:
    config = {
        "coves_api_url": COVES_API_URL,
        "bluesky_api_url": BLUESKY_API_URL,
        "window_hours": 6,
        "max_posts_per_run": 3,
        "classify_candidates": 50,
        "min_confidence": 0.7,
        "classifier_model": "claude-opus-5",
        "log_level": "info",
        "topics": topics,
    }
    config.update(overrides)
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump(config))
    return path


class FakeBlueskyClient:
    """Feeds/lists/profiles map a URI or DID to a result or an exception to raise."""

    def __init__(self, feeds=None, lists=None, profiles=None):
        self.feeds = dict(feeds or {})
        self.lists = dict(lists or {})
        self.profiles = dict(profiles or {})
        self.feed_calls = []
        self.list_calls = []
        self.profile_calls = []

    @staticmethod
    def _resolve(value):
        if isinstance(value, Exception):
            raise value
        return value

    def get_feed(self, feed_uri):
        self.feed_calls.append(feed_uri)
        return self._resolve(self.feeds.get(feed_uri, FeedFetchResult()))

    def get_list_feed(self, list_uri):
        self.list_calls.append(list_uri)
        return self._resolve(self.lists.get(list_uri, FeedFetchResult()))

    def get_profile(self, did):
        self.profile_calls.append(did)
        return self._resolve(self.profiles.get(did, ""))


class FakeClassifier:
    """
    Verdicts keyed by URI (default on-topic 0.9). Raises on descriptions in `fail_on`.

    Story keys come from `stories_by_uri`; a URI without an entry is its own
    story (key == uri), so posts are distinct stories unless a test says otherwise.
    """

    def __init__(
        self,
        verdicts_by_uri=None,
        default=(True, 0.9),
        fail_on=(),
        stories_by_uri=None,
        batch_size=DEFAULT_BATCH_SIZE,
    ):
        self.verdicts_by_uri = dict(verdicts_by_uri or {})
        self.default = default
        self.fail_on = set(fail_on)
        self.stories_by_uri = dict(stories_by_uri or {})
        self.batch_size = batch_size
        self.calls = []

    def classify(self, topic_description, posts, bios=None, *, required_ids=(), **kwargs):
        self.calls.append(
            SimpleNamespace(
                topic_description=topic_description,
                uris=[post.uri for post in posts],
                bios=None if bios is None else dict(bios),
                required_ids=list(required_ids),
            )
        )
        if topic_description in self.fail_on:
            raise ClassificationError("classifier down")
        verdicts = []
        for index, post in enumerate(posts):
            on_topic, confidence = self.verdicts_by_uri.get(post.uri, self.default)
            verdicts.append(
                SimpleNamespace(
                    id=index,
                    on_topic=on_topic,
                    confidence=confidence,
                    story=self.stories_by_uri.get(post.uri, post.uri),
                )
            )
        return verdicts


class FakeCommunityFeedReader:
    """
    Stands in for CommunityFeedReader: records every (community_handle, since)
    lookup and returns a fixed set of external link URIs, or raises `error`.
    """

    def __init__(self, external_uris=(), error=None):
        self.external_uris = set(external_uris)
        self.error = error
        self.calls = []

    def external_uris_since(self, community_handle, since):
        self.calls.append((community_handle, since))
        if self.error is not None:
            raise self.error
        return set(self.external_uris)


class RecordingCovesClient(CovesClient):
    def __init__(self, fail_uris=(), authenticate_error=None, connection_error_uris=()):
        super().__init__(api_url=COVES_API_URL, api_key=FAKE_COVES_API_KEY)
        self.fail_uris = set(fail_uris)
        self.connection_error_uris = set(connection_error_uris)
        self.authenticate_error = authenticate_error
        self.authenticate_calls = 0
        self.create_post_calls = []

    def authenticate(self):
        self.authenticate_calls += 1
        if self.authenticate_error is not None:
            raise self.authenticate_error

    def create_post(self, community_handle, content, facets, title=None, embed=None, thumbnail_url=None):
        permalink = embed["external"]["uri"]
        if permalink in self.connection_error_uris:
            raise requests.ConnectionError("connection reset")
        if permalink in self.fail_uris:
            raise CovesAPIError("create_post failed", status_code=500)
        self.create_post_calls.append(
            {"community_handle": community_handle, "title": title, "permalink": permalink}
        )
        return f"at://did:plc:coves/social.coves.post/{len(self.create_post_calls)}"

    def posted_permalinks(self):
        return [call["permalink"] for call in self.create_post_calls]


class FailingOnceStateManager(StateManager):
    """Real state manager whose first mark_posted raises, as a disk failure would."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.remaining_failures = 1

    def mark_posted(self, *args, **kwargs):
        if self.remaining_failures:
            self.remaining_failures -= 1
            raise OSError("state file is not writable")
        return super().mark_posted(*args, **kwargs)


def error_messages(caplog):
    return [record.getMessage() for record in caplog.records if record.levelno >= logging.ERROR]


def permalink(item) -> str:
    post = item["post"]
    return f"https://bsky.app/profile/{post['author']['handle']}/post/{post['uri'].rsplit('/', 1)[1]}"


def state_file_in(tmp_path: Path) -> Path:
    return tmp_path / "data" / "state.json"


def story_log_path(state_file: Path) -> Path:
    return state_file.parent / "stories.json"


def run(tmp_path, topics, bluesky, classifier=None, coves=None, community_feed_reader=None, **config_overrides):
    config_path = write_config(tmp_path, topics, **config_overrides)
    state_file = state_file_in(tmp_path)
    classifier = classifier or FakeClassifier()
    coves = coves or RecordingCovesClient()
    community_feed_reader = community_feed_reader or FakeCommunityFeedReader()
    Aggregator(
        config_path=config_path,
        state_file=state_file,
        coves_client=coves,
        classifier=classifier,
        now=lambda: NOW,
        bluesky_client=bluesky,
        community_feed_reader=community_feed_reader,
    ).run()
    return SimpleNamespace(
        coves=coves,
        classifier=classifier,
        bluesky=bluesky,
        state_file=state_file,
        community_feed_reader=community_feed_reader,
        story_log=StoryLog(story_log_path(state_file)),
    )


def run_dry(tmp_path, topics, bluesky, classifier=None, community_feed_reader=None, **config_overrides):
    """DRY_RUN must already be set by the caller; no Coves client is injected."""
    config_path = write_config(tmp_path, topics, **config_overrides)
    state_file = state_file_in(tmp_path)
    classifier = classifier or FakeClassifier()
    community_feed_reader = community_feed_reader or FakeCommunityFeedReader()
    Aggregator(
        config_path=config_path,
        state_file=state_file,
        classifier=classifier,
        now=lambda: NOW,
        bluesky_client=bluesky,
        community_feed_reader=community_feed_reader,
    ).run()
    return SimpleNamespace(
        classifier=classifier, bluesky=bluesky, state_file=state_file, community_feed_reader=community_feed_reader
    )


def last_run(state_file: Path, topic_name: str):
    if not state_file.exists():
        return None
    return StateManager(state_file).get_last_run(topic_name)


def posted_guids(state_file: Path, topic_name: str):
    if not state_file.exists():
        return []
    return StateManager(state_file).get_all_posted_guids(topic_name)


class TestSourceFailureIsolation:
    def test_failing_feed_is_skipped_and_other_feed_still_posts(self, tmp_path):
        item = feed_item("fromb")
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: BlueskyAPIError("boom", status_code=500), FEED_B: FeedFetchResult(items=[item])}
        )

        result = run(tmp_path, [topic("nba", feeds=(FEED_A, FEED_B))], bluesky)

        assert result.coves.posted_permalinks() == [permalink(item)]
        assert bluesky.feed_calls == [FEED_A, FEED_B]

    def test_all_sources_failing_posts_nothing_and_leaves_last_run_unset(self, tmp_path):
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: BlueskyAPIError("boom"), FEED_B: BlueskyAPIError("boom")},
            lists={LIST_A: BlueskyAPIError("boom")},
        )

        result = run(tmp_path, [topic("nba", feeds=(FEED_A, FEED_B), lists=(LIST_A,))], bluesky)

        assert bluesky.feed_calls == [FEED_A, FEED_B]
        assert bluesky.list_calls == [LIST_A]
        assert result.coves.create_post_calls == []
        assert result.classifier.calls == []
        assert last_run(result.state_file, "nba") is None

    def test_partial_source_still_contributes_its_items(self, tmp_path):
        item = feed_item("partial")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item], partial=True)})

        result = run(tmp_path, [topic("nba")], bluesky)

        assert result.coves.posted_permalinks() == [permalink(item)]

    def test_list_sources_are_fetched_too(self, tmp_path):
        item = feed_item("fromlist")
        bluesky = FakeBlueskyClient(lists={LIST_A: FeedFetchResult(items=[item])})

        result = run(tmp_path, [topic("nba", feeds=(), lists=(LIST_A,))], bluesky)

        assert bluesky.list_calls == [LIST_A]
        assert result.coves.posted_permalinks() == [permalink(item)]


class TestTopicFailureIsolation:
    def test_classifier_error_in_first_topic_does_not_stop_second_topic(self, tmp_path):
        nba_item = feed_item("nba1", author_did="did:plc:nba")
        tennis_item = feed_item("tennis1", author_did="did:plc:tennis")
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[nba_item]), FEED_B: FeedFetchResult(items=[tennis_item])}
        )
        classifier = FakeClassifier(fail_on={NBA_DESCRIPTION})

        result = run(
            tmp_path,
            [topic("nba", feeds=(FEED_A,)), topic("tennis", TENNIS_DESCRIPTION, feeds=(FEED_B,))],
            bluesky,
            classifier=classifier,
        )

        assert result.coves.posted_permalinks() == [permalink(tennis_item)]
        assert result.coves.create_post_calls[0]["community_handle"] == "tennis.coves.social"
        assert last_run(result.state_file, "nba") is None
        assert last_run(result.state_file, "tennis") == NOW
        assert posted_guids(result.state_file, "nba") == []

    def test_unexpected_exception_in_first_topic_does_not_stop_second_topic(self, tmp_path):
        tennis_item = feed_item("tennis1", author_did="did:plc:tennis")
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: RuntimeError("unexpected"), FEED_B: FeedFetchResult(items=[tennis_item])}
        )

        result = run(
            tmp_path,
            [topic("nba", feeds=(FEED_A,)), topic("tennis", TENNIS_DESCRIPTION, feeds=(FEED_B,))],
            bluesky,
        )

        assert result.coves.posted_permalinks() == [permalink(tennis_item)]


class TestEmptyCandidates:
    def test_no_profile_fetch_no_classifier_call_and_last_run_updated(self, tmp_path):
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[feed_item("quiet", like_count=1), feed_item("old", age=timedelta(hours=9))])}
        )

        result = run(tmp_path, [topic("nba")], bluesky)

        assert bluesky.profile_calls == []
        assert result.classifier.calls == []
        assert result.coves.create_post_calls == []
        assert last_run(result.state_file, "nba") == NOW


class TestProfileCaching:
    def test_same_author_fetched_once(self, tmp_path):
        items = [feed_item("one", author_did="did:plc:same"), feed_item("two", author_did="did:plc:same", like_count=8)]
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=items)}, profiles={"did:plc:same": "Reporter"})

        result = run(tmp_path, [topic("nba")], bluesky)

        assert bluesky.profile_calls == ["did:plc:same"]
        assert result.classifier.calls[0].bios == {"did:plc:same": "Reporter"}

    def test_failed_profile_lookup_gives_empty_bio_and_is_not_retried(self, tmp_path):
        items = [feed_item("one", author_did="did:plc:flaky"), feed_item("two", author_did="did:plc:flaky", like_count=8)]
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=items)},
            profiles={"did:plc:flaky": BlueskyAPIError("profile down", status_code=502)},
        )

        result = run(tmp_path, [topic("nba")], bluesky)

        assert bluesky.profile_calls == ["did:plc:flaky"]
        assert result.classifier.calls[0].bios == {"did:plc:flaky": ""}
        assert len(result.coves.create_post_calls) == 2

    def test_candidates_truncated_away_get_no_profile_fetch(self, tmp_path):
        top = feed_item("top", author_did="did:plc:top", like_count=50)
        cut = feed_item("cut", author_did="did:plc:cut", like_count=10)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[cut, top])})

        result = run(tmp_path, [topic("nba")], bluesky, classify_candidates=1)

        assert bluesky.profile_calls == ["did:plc:top"]
        assert result.classifier.calls[0].uris == [uri_of(top)]


class TestClassifierInput:
    def test_receives_ranked_truncated_candidates_and_bios_keyed_by_did(self, tmp_path):
        low = feed_item("low", author_did="did:plc:low", like_count=10)
        high = feed_item("high", author_did="did:plc:high", like_count=30)
        middle = feed_item("middle", author_did="did:plc:middle", like_count=20)
        dropped = feed_item("dropped", author_did="did:plc:dropped", like_count=1)
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[low, high, dropped, middle])},
            profiles={"did:plc:low": "Low bio", "did:plc:high": "High bio", "did:plc:middle": "Middle bio"},
        )

        result = run(tmp_path, [topic("nba")], bluesky, classify_candidates=2)

        assert len(result.classifier.calls) == 1
        call = result.classifier.calls[0]
        assert call.topic_description == NBA_DESCRIPTION
        assert call.uris == [uri_of(high), uri_of(middle)]
        assert call.bios == {"did:plc:high": "High bio", "did:plc:middle": "Middle bio"}
        assert "did:plc:dropped" not in bluesky.profile_calls


class TestStateMarking:
    def test_failed_create_post_is_not_marked_and_next_post_is_still_attempted(self, tmp_path):
        first = feed_item("first", author_did="did:plc:first", like_count=50)
        second = feed_item("second", author_did="did:plc:second", like_count=40)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[first, second])})
        coves = RecordingCovesClient(fail_uris={permalink(first)})

        result = run(tmp_path, [topic("nba")], bluesky, coves=coves)

        assert coves.posted_permalinks() == [permalink(second)]
        assert posted_guids(result.state_file, "nba") == [uri_of(second)]
        assert last_run(result.state_file, "nba") == NOW

    def test_network_error_on_first_post_still_attempts_the_second(self, tmp_path):
        first = feed_item("first", author_did="did:plc:first", like_count=50)
        second = feed_item("second", author_did="did:plc:second", like_count=40)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[first, second])})
        coves = RecordingCovesClient(connection_error_uris={permalink(first)})

        result = run(tmp_path, [topic("nba")], bluesky, coves=coves)

        assert coves.posted_permalinks() == [permalink(second)]
        assert posted_guids(result.state_file, "nba") == [uri_of(second)]
        assert last_run(result.state_file, "nba") == NOW

    def test_bookkeeping_failure_logs_both_uris_and_still_posts_the_next(
        self, tmp_path, monkeypatch, caplog
    ):
        monkeypatch.setattr(main_module, "StateManager", FailingOnceStateManager)
        first = feed_item("first", author_did="did:plc:first", like_count=50)
        second = feed_item("second", author_did="did:plc:second", like_count=40)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[first, second])})
        coves = RecordingCovesClient()

        with caplog.at_level("ERROR"):
            result = run(tmp_path, [topic("nba")], bluesky, coves=coves)

        assert coves.posted_permalinks() == [permalink(first), permalink(second)]
        orphan_coves_uri = "at://did:plc:coves/social.coves.post/1"
        assert any(
            uri_of(first) in message and orphan_coves_uri in message
            for message in error_messages(caplog)
        ), error_messages(caplog)
        assert posted_guids(result.state_file, "nba") == [uri_of(second)]

    def test_authenticate_failure_aborts_run_and_posts_nothing(self, tmp_path):
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[feed_item("one")])})
        coves = RecordingCovesClient(authenticate_error=CovesAuthenticationError("bad key", status_code=401))

        with pytest.raises(CovesAuthenticationError):
            run(tmp_path, [topic("nba")], bluesky, coves=coves)

        assert coves.create_post_calls == []
        assert bluesky.feed_calls == []


class TestDryRun:
    def test_dry_run_posts_nothing_touches_no_state_and_still_classifies(
        self, tmp_path, monkeypatch, caplog
    ):
        monkeypatch.setenv("DRY_RUN", "1")
        monkeypatch.delenv("COVES_API_KEY", raising=False)
        item = feed_item("dry")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})
        with caplog.at_level("INFO"):
            result = run_dry(tmp_path, [topic("nba")], bluesky)

        assert result.classifier.calls[0].uris == [uri_of(item)]
        assert not result.state_file.exists()
        assert not result.state_file.parent.exists()
        assert error_messages(caplog) == []
        assert any("DRY RUN ranked table" in message for message in caplog.messages)

    def test_dry_run_with_no_eligible_candidates_logs_no_error_and_writes_no_state(
        self, tmp_path, monkeypatch, caplog
    ):
        monkeypatch.setenv("DRY_RUN", "1")
        monkeypatch.delenv("COVES_API_KEY", raising=False)
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[feed_item("quiet", like_count=1)])}
        )

        with caplog.at_level("INFO"):
            result = run_dry(tmp_path, [topic("nba")], bluesky)

        assert error_messages(caplog) == []
        assert not result.state_file.exists()
        assert not result.state_file.parent.exists()


class TestClassifierBatchLimit:
    def test_lowest_ranked_candidates_are_dropped_so_story_log_entries_share_one_batch(
        self, tmp_path, caplog
    ):
        high = feed_item("high", author_did="did:plc:high", handle="high.bsky.social", like_count=50)
        low = feed_item("low", author_did="did:plc:low", handle="low.bsky.social", like_count=40)
        logged = parse_post_view(
            feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social")
        )
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", logged, NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[high, low])})
        classifier = FakeClassifier(batch_size=2)

        with caplog.at_level("INFO"):
            result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier)

        assert len(result.classifier.calls) == 1
        assert result.classifier.calls[0].uris == [uri_of(high), logged.uri]
        assert any("dropping 1" in message for message in caplog.messages), caplog.messages

    def test_story_log_entries_are_marked_required_for_the_classifier(self, tmp_path):
        candidate = feed_item("candidate", author_did="did:plc:candidate", handle="candidate.bsky.social")
        logged = parse_post_view(
            feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social")
        )
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", logged, NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[candidate])})

        result = run(tmp_path, [topic("nba")], bluesky)

        call = result.classifier.calls[0]
        assert call.uris == [uri_of(candidate), logged.uri]
        assert call.required_ids == [1]

    def test_already_posted_candidate_is_required_alongside_story_log_entries(self, tmp_path):
        posted = feed_item("posted", author_did="did:plc:posted", handle="posted.bsky.social", like_count=50)
        fresh = feed_item("fresh", author_did="did:plc:fresh", handle="fresh.bsky.social", like_count=40)
        logged = parse_post_view(
            feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social")
        )
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StateManager(state_file).mark_posted("nba", uri_of(posted), "at://did:plc:coves/social.coves.post/1")
        StoryLog(story_log_path(state_file)).record("nba", logged, NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[posted, fresh])})
        classifier = FakeClassifier(
            stories_by_uri={uri_of(posted): "same-story", uri_of(fresh): "same-story"}
        )

        result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier)

        call = result.classifier.calls[0]
        assert sorted(call.required_ids) == sorted(
            [call.uris.index(uri_of(posted)), call.uris.index(logged.uri)]
        )
        assert result.coves.create_post_calls == []

    def test_logged_candidate_survives_trimming_and_a_fresh_candidate_is_dropped_instead(
        self, tmp_path
    ):
        high = feed_item("high", author_did="did:plc:high", handle="high.bsky.social", like_count=50)
        middle = feed_item("middle", author_did="did:plc:middle", handle="middle.bsky.social", like_count=40)
        lowest = feed_item("lowest", author_did="did:plc:lowest", handle="lowest.bsky.social", like_count=30)
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", parse_post_view(lowest), NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[high, middle, lowest])})
        classifier = FakeClassifier(batch_size=2)

        result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier)

        uris = result.classifier.calls[0].uris
        assert uris.count(uri_of(lowest)) == 1
        assert uri_of(high) in uris
        assert uri_of(middle) not in uris

    def test_story_log_filling_the_batch_classifies_nothing_and_warns(self, tmp_path, caplog):
        first = parse_post_view(feed_item("first", author_did="did:plc:first", handle="first.bsky.social"))
        second = parse_post_view(feed_item("second", author_did="did:plc:second", handle="second.bsky.social"))
        fresh = feed_item("fresh", author_did="did:plc:fresh", handle="fresh.bsky.social")
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        story_log = StoryLog(story_log_path(state_file))
        story_log.record("nba", first, NOW - timedelta(hours=1))
        story_log.record("nba", second, NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[fresh])})
        classifier = FakeClassifier(batch_size=2)

        with caplog.at_level("INFO"):
            result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier)

        assert result.classifier.calls == []
        assert result.coves.create_post_calls == []
        assert posted_guids(result.state_file, "nba") == []
        assert last_run(result.state_file, "nba") == NOW
        warnings = [record.getMessage() for record in caplog.records if record.levelno == logging.WARNING]
        assert any("no room" in message for message in warnings), warnings


class TestIsDryRun:
    @pytest.mark.parametrize(
        ("value", "expected"),
        [
            ("0", False),
            ("", False),
            ("false", False),
            ("yes", False),
            ("1", True),
            ("true", True),
            (" TRUE ", False),
            ("TRUE", False),
        ],
    )
    def test_dry_run_env_values(self, value, expected, monkeypatch):
        monkeypatch.setenv("DRY_RUN", value)

        assert is_dry_run() is expected

    def test_unset_is_not_a_dry_run(self, monkeypatch):
        monkeypatch.delenv("DRY_RUN", raising=False)

        assert is_dry_run() is False


class TestDisabledTopic:
    def test_disabled_topic_fetches_nothing_while_enabled_topic_runs(self, tmp_path):
        tennis_item = feed_item("tennis1", author_did="did:plc:tennis")
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[feed_item("one")]), FEED_B: FeedFetchResult(items=[tennis_item])}
        )

        result = run(
            tmp_path,
            [
                topic("nba", feeds=(FEED_A,), lists=(LIST_A,), enabled=False),
                topic("tennis", TENNIS_DESCRIPTION, feeds=(FEED_B,)),
            ],
            bluesky,
        )

        assert bluesky.feed_calls == [FEED_B]
        assert bluesky.list_calls == []
        assert [call.topic_description for call in result.classifier.calls] == [TENNIS_DESCRIPTION]
        assert result.coves.posted_permalinks() == [permalink(tennis_item)]
        assert last_run(result.state_file, "nba") is None


class TestCommunityFeedLookup:
    def test_reader_called_once_per_topic_with_community_handle_and_window_start(self, tmp_path):
        bluesky = FakeBlueskyClient(
            feeds={FEED_A: FeedFetchResult(items=[feed_item("one")]), FEED_B: FeedFetchResult(items=[feed_item("two")])}
        )
        reader = FakeCommunityFeedReader()

        run(
            tmp_path,
            [topic("nba", feeds=(FEED_A,), window_hours=4), topic("tennis", TENNIS_DESCRIPTION, feeds=(FEED_B,))],
            bluesky,
            community_feed_reader=reader,
        )

        assert reader.calls == [
            ("nba.coves.social", NOW - timedelta(hours=4)),
            ("tennis.coves.social", NOW - timedelta(hours=6)),
        ]

    def test_reader_5xx_error_logs_error_and_candidates_are_still_posted(self, tmp_path, caplog):
        item = feed_item("still")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})
        reader = FakeCommunityFeedReader(error=CommunityFeedError("community feed down", status_code=503))

        with caplog.at_level("ERROR"):
            result = run(tmp_path, [topic("nba")], bluesky, community_feed_reader=reader)

        assert reader.calls == [("nba.coves.social", NOW - timedelta(hours=6))]
        assert result.coves.posted_permalinks() == [permalink(item)]
        assert posted_guids(result.state_file, "nba") == [uri_of(item)]
        assert any("community feed down" in message for message in error_messages(caplog))

    def test_reader_4xx_error_skips_the_topic_without_posting(self, tmp_path, caplog):
        item = feed_item("blocked")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})
        reader = FakeCommunityFeedReader(error=CommunityFeedError("community not found", status_code=404))

        with caplog.at_level("ERROR"):
            result = run(tmp_path, [topic("nba")], bluesky, community_feed_reader=reader)

        assert result.coves.create_post_calls == []
        assert posted_guids(result.state_file, "nba") == []
        assert any(
            "nba.coves.social" in message and "404" in message for message in error_messages(caplog)
        ), error_messages(caplog)

    def test_candidate_linked_in_community_is_skipped_and_not_marked(self, tmp_path):
        community_posted = feed_item("linked", author_did="did:plc:linked", handle="linked.bsky.social", like_count=50)
        fresh = feed_item("fresh", author_did="did:plc:fresh", handle="fresh.bsky.social", like_count=40)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[community_posted, fresh])})
        reader = FakeCommunityFeedReader({permalink(community_posted)})

        result = run(tmp_path, [topic("nba")], bluesky, community_feed_reader=reader)

        assert result.coves.posted_permalinks() == [permalink(fresh)]
        assert posted_guids(result.state_file, "nba") == [uri_of(fresh)]
        assert last_run(result.state_file, "nba") == NOW

    def test_community_linked_candidate_blocks_its_whole_story(self, tmp_path):
        linked = feed_item("linked", author_did="did:plc:linked", handle="linked.bsky.social", like_count=30)
        sibling = feed_item("sibling", author_did="did:plc:sibling", handle="sibling.bsky.social", like_count=60)
        other = feed_item("other", author_did="did:plc:other", handle="other.bsky.social", like_count=20)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[linked, sibling, other])})
        classifier = FakeClassifier(stories_by_uri={uri_of(linked): "same-story", uri_of(sibling): "same-story"})
        reader = FakeCommunityFeedReader({permalink(linked)})

        result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier, community_feed_reader=reader)

        assert result.coves.posted_permalinks() == [permalink(other)]
        assert posted_guids(result.state_file, "nba") == [uri_of(other)]

    def test_community_post_embed_at_uri_blocks_its_whole_story(self, tmp_path):
        # The AppView turns a bsky.app link post into a Bluesky post embed, so
        # the community feed reports the at:// uri of the quoted post.
        linked = feed_item("linked", author_did="did:plc:linked", handle="linked.bsky.social", like_count=30)
        sibling = feed_item("sibling", author_did="did:plc:sibling", handle="sibling.bsky.social", like_count=60)
        other = feed_item("other", author_did="did:plc:other", handle="other.bsky.social", like_count=20)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[linked, sibling, other])})
        classifier = FakeClassifier(stories_by_uri={uri_of(linked): "same-story", uri_of(sibling): "same-story"})
        reader = FakeCommunityFeedReader({uri_of(linked)})

        result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier, community_feed_reader=reader)

        assert result.coves.posted_permalinks() == [permalink(other)]
        assert posted_guids(result.state_file, "nba") == [uri_of(other)]

    def test_post_in_state_is_skipped_even_when_community_has_nothing(self, tmp_path):
        item = feed_item("again")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})

        first = run(tmp_path, [topic("nba")], bluesky, community_feed_reader=FakeCommunityFeedReader())
        assert first.coves.posted_permalinks() == [permalink(item)]

        second = run(tmp_path, [topic("nba")], bluesky, community_feed_reader=FakeCommunityFeedReader())

        assert second.coves.create_post_calls == []
        assert second.classifier.calls[0].uris == [uri_of(item)]
        assert posted_guids(second.state_file, "nba") == [uri_of(item)]
        assert last_run(second.state_file, "nba") == NOW


class TestStoryLog:
    def test_successful_post_is_recorded_in_the_story_log_next_to_the_state_file(self, tmp_path):
        item = feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})

        result = run(tmp_path, [topic("nba")], bluesky)

        assert result.coves.posted_permalinks() == [permalink(item)]
        assert story_log_path(result.state_file).exists()
        recent = result.story_log.recent("nba", NOW - timedelta(hours=6))
        assert [post.uri for post in recent] == [uri_of(item)]
        assert recent[0].author_did == "did:plc:logged"
        assert recent[0].author_handle == "logged.bsky.social"
        assert result.story_log.recent("tennis", NOW - timedelta(hours=6)) == []

    def test_failed_post_is_not_recorded_in_the_story_log(self, tmp_path):
        failing = feed_item("failing", author_did="did:plc:failing", handle="failing.bsky.social")
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[failing])})
        coves = RecordingCovesClient(fail_uris={permalink(failing)})

        result = run(tmp_path, [topic("nba")], bluesky, coves=coves)

        assert coves.create_post_calls == []
        assert result.story_log.recent("nba", NOW - timedelta(hours=6)) == []

    def test_logged_story_outside_the_candidate_set_is_classified_and_blocks_its_story(self, tmp_path):
        logged = parse_post_view(feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social"))
        newcomer = feed_item("newcomer", author_did="did:plc:newcomer", handle="newcomer.bsky.social", like_count=80)
        unrelated = feed_item("unrelated", author_did="did:plc:unrelated", handle="unrelated.bsky.social", like_count=20)
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", logged, NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[newcomer, unrelated])})
        classifier = FakeClassifier(stories_by_uri={logged.uri: "shared-event", uri_of(newcomer): "shared-event"})

        result = run(tmp_path, [topic("nba")], bluesky, classifier=classifier)

        classified = result.classifier.calls[0].uris
        assert logged.uri in classified
        assert uri_of(newcomer) in classified
        assert result.coves.posted_permalinks() == [permalink(unrelated)]
        assert posted_guids(result.state_file, "nba") == [uri_of(unrelated)]

    def test_logged_post_already_in_the_candidate_set_is_not_classified_twice(self, tmp_path):
        item = feed_item("both", author_did="did:plc:both", handle="both.bsky.social")
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", parse_post_view(item), NOW - timedelta(hours=1))
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[item])})

        result = run(tmp_path, [topic("nba")], bluesky)

        assert result.classifier.calls[0].uris == [uri_of(item)]
        assert result.coves.create_post_calls == []


class TestDryRunCommunityAndStoryLog:
    def test_dry_run_calls_reader_skips_story_log_and_lists_blocked_only_by_log_as_pick(
        self, tmp_path, monkeypatch, caplog
    ):
        monkeypatch.setenv("DRY_RUN", "1")
        monkeypatch.delenv("COVES_API_KEY", raising=False)
        logged = parse_post_view(feed_item("logged", author_did="did:plc:logged", handle="logged.bsky.social"))
        pick = feed_item("pick", author_did="did:plc:pick", handle="pick.bsky.social", like_count=80)
        state_file = state_file_in(tmp_path)
        state_file.parent.mkdir(parents=True)
        StoryLog(story_log_path(state_file)).record("nba", logged, NOW - timedelta(hours=1))
        story_log_bytes = story_log_path(state_file).read_bytes()
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[pick])})
        classifier = FakeClassifier(stories_by_uri={logged.uri: "shared-event", uri_of(pick): "shared-event"})
        reader = FakeCommunityFeedReader()

        with caplog.at_level("INFO"):
            result = run_dry(tmp_path, [topic("nba")], bluesky, classifier=classifier, community_feed_reader=reader)

        assert reader.calls == [("nba.coves.social", NOW - timedelta(hours=6))]
        assert story_log_path(result.state_file).read_bytes() == story_log_bytes
        assert error_messages(caplog) == []
        assert result.classifier.calls[0].uris == [uri_of(pick)]
        assert not result.state_file.exists()
        table = next(message for message in caplog.messages if "DRY RUN ranked table" in message)
        assert "1 would be posted" in table
        assert "shared-event" in table
        pick_line = next(line for line in table.splitlines() if "pick.bsky.social" in line)
        assert pick_line.startswith("*")
        assert "on-topic" in pick_line
        assert "posted" not in pick_line

    def test_dry_run_marks_community_linked_candidate_as_posted(self, tmp_path, monkeypatch, caplog):
        monkeypatch.setenv("DRY_RUN", "1")
        monkeypatch.delenv("COVES_API_KEY", raising=False)
        linked = feed_item("linked", author_did="did:plc:linked", handle="linked.bsky.social", like_count=50)
        fresh = feed_item("fresh", author_did="did:plc:fresh", handle="fresh.bsky.social", like_count=40)
        bluesky = FakeBlueskyClient(feeds={FEED_A: FeedFetchResult(items=[linked, fresh])})
        reader = FakeCommunityFeedReader({permalink(linked)})

        with caplog.at_level("INFO"):
            run_dry(tmp_path, [topic("nba")], bluesky, community_feed_reader=reader)

        table = next(message for message in caplog.messages if "DRY RUN ranked table" in message)
        assert "1 would be posted" in table
        linked_line = next(line for line in table.splitlines() if "linked.bsky.social" in line)
        fresh_line = next(line for line in table.splitlines() if "fresh.bsky.social" in line)
        assert "posted" in linked_line
        assert not linked_line.startswith("*")
        assert fresh_line.startswith("*")
        assert "posted" not in fresh_line
