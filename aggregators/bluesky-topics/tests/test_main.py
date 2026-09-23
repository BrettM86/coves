"""
Acceptance test for the Bluesky Topics aggregator.

Observes the whole feature from the outside: a config file on disk, a feed
served by `responses` from a fake Bluesky AppView, a fake topic classifier,
a Coves client that records calls instead of hitting the network, and a
real state file. Three runs share one state file:

  run 1 (fresh Aggregator) posts the 40- and 30-like posts, in that order
  run 2 (fresh Aggregator, same state file) posts nothing: the top stories are already posted and are not backfilled
  run 3 (fresh Aggregator, same state file) posts nothing
"""
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace

import responses
import yaml

from src.coves_client import CovesClient
from src.main import Aggregator

BLUESKY_API_URL = "https://bsky.test"
COVES_API_URL = "https://coves.test"
FEED_URI = "at://did:plc:feedowner/app.bsky.feed.generator/nba-essential"
TOPIC_NAME = "nba"
COMMUNITY_HANDLE = "nba.coves.social"
TOPIC_DESCRIPTION = "Posts about the NBA: teams, players, games, trades, injuries, league news."
EXCLUDED_HANDLE = "surfrecommends.bsky.social"

NOW = datetime(2026, 9, 18, 12, 0, 0, tzinfo=timezone.utc)

# Dummy key in the shape CovesClient's constructor validates; never sent anywhere.
FAKE_COVES_API_KEY = "ckapi_" + "a" * 64


def iso(delta: timedelta) -> str:
    return (NOW - delta).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def make_post(
    rkey: str,
    author_did: str,
    handle: str,
    display_name,
    text: str,
    age: timedelta,
    like_count: int,
    repost_count: int = 0,
    post_labels=None,
    author_labels=None,
    reply=None,
):
    """Hand-written app.bsky.feed.defs#postView per the spec's verified facts."""
    author = {"did": author_did, "handle": handle, "labels": list(author_labels or [])}
    if display_name is not None:
        author["displayName"] = display_name
    record = {
        "$type": "app.bsky.feed.post",
        "text": text,
        "createdAt": iso(age),
    }
    if reply is not None:
        record["reply"] = reply
    return {
        "uri": f"at://{author_did}/app.bsky.feed.post/{rkey}",
        "cid": f"bafy{rkey}",
        "author": author,
        "record": record,
        "likeCount": like_count,
        "repostCount": repost_count,
        "replyCount": 0,
        "quoteCount": 0,
        "labels": [{"val": val} for val in (post_labels or [])],
        "indexedAt": iso(age),
    }


# Eligible posts. Likes 50/40/30/20; the 50 is marked off-topic by the fake classifier.
POST_50 = make_post(
    rkey="likes50",
    author_did="did:plc:collegefan",
    handle="collegefan.bsky.social",
    display_name="College Fan",
    text="What a finish in the Georgia vs Alabama game tonight",
    age=timedelta(hours=1),
    like_count=50,
)
POST_40 = make_post(
    rkey="likes40",
    author_did="did:plc:shams",
    handle="shams.bsky.social",
    display_name="Shams Charania",
    text="The Lakers are   trading   for a new center, sources say.",
    age=timedelta(hours=2),
    like_count=40,
)
POST_30 = make_post(
    rkey="likes30",
    author_did="did:plc:woj",
    handle="woj.bsky.social",
    display_name="Adrian Wojnarowski",
    text="Celtics guard out six weeks with a sprained ankle.",
    age=timedelta(hours=3),
    like_count=30,
)
POST_20 = make_post(
    rkey="likes20",
    author_did="did:plc:hoopsfan",
    handle="hoopsfan.bsky.social",
    display_name="Hoops Fan",
    text="Knicks pull off a comeback win in overtime.",
    age=timedelta(minutes=30),
    like_count=20,
)

# Drop-rule items, one per rule.
POST_REPLY = make_post(
    rkey="reply1",
    author_did="did:plc:replier",
    handle="replier.bsky.social",
    display_name="Replier",
    text="Agreed, that trade makes no sense for the Lakers.",
    age=timedelta(hours=1),
    like_count=90,
    reply={
        "root": {"uri": "at://did:plc:shams/app.bsky.feed.post/likes40", "cid": "bafyroot"},
        "parent": {"uri": "at://did:plc:shams/app.bsky.feed.post/likes40", "cid": "bafyparent"},
    },
)
POST_OLD = make_post(
    rkey="old8h",
    author_did="did:plc:oldnews",
    handle="oldnews.bsky.social",
    display_name="Old News",
    text="NBA draft lottery results are in.",
    age=timedelta(hours=8),
    like_count=95,
)
POST_FEW_LIKES = make_post(
    rkey="twolikes",
    author_did="did:plc:quiet",
    handle="quiet.bsky.social",
    display_name="Quiet Poster",
    text="Warriors practice report.",
    age=timedelta(hours=1),
    like_count=2,
)
POST_PORN_LABEL = make_post(
    rkey="labelled",
    author_did="did:plc:labelled",
    handle="labelled.bsky.social",
    display_name="Labelled",
    text="NBA cheerleader photo dump.",
    age=timedelta(hours=1),
    like_count=85,
    post_labels=["porn"],
)
POST_NO_UNAUTHENTICATED = make_post(
    rkey="private",
    author_did="did:plc:private",
    handle="private.bsky.social",
    display_name="Private Author",
    text="Bulls win again.",
    age=timedelta(hours=1),
    like_count=80,
    author_labels=[{"val": "!no-unauthenticated", "src": "did:plc:private"}],
)
POST_EXCLUDED_HANDLE = make_post(
    rkey="promo",
    author_did="did:plc:surf",
    handle=EXCLUDED_HANDLE,
    display_name="Surf",
    text="Try the NBA Essentials feed on Surf!",
    age=timedelta(hours=1),
    like_count=75,
)
POST_REPOSTED = make_post(
    rkey="reposted",
    author_did="did:plc:original",
    handle="original.bsky.social",
    display_name="Original",
    text="Nuggets clinch the top seed.",
    age=timedelta(hours=1),
    like_count=70,
)

FEED_PAGE = {
    "feed": [
        # Deliberately not in score order so ordering is asserted, not inherited.
        {"post": POST_30},
        {"post": POST_REPLY},
        {"post": POST_20},
        {"post": POST_OLD},
        {"post": POST_FEW_LIKES},
        {"post": POST_50},
        {"post": POST_PORN_LABEL},
        {"post": POST_NO_UNAUTHENTICATED},
        {"post": POST_40},
        {"post": POST_EXCLUDED_HANDLE},
        {
            "post": POST_REPOSTED,
            "reason": {
                "$type": "app.bsky.feed.defs#reasonRepost",
                "by": {"did": "did:plc:reposter", "handle": "reposter.bsky.social"},
                "indexedAt": iso(timedelta(minutes=10)),
            },
        },
    ]
    # No cursor: single page.
}

ON_TOPIC_URIS = {POST_40["uri"], POST_30["uri"], POST_20["uri"]}


class FakeClassifier:
    """
    Stands in for TopicClassifier. Verdicts are keyed by post URI: for each
    post handed to classify(), a verdict with the batch-local id (index into
    `posts`) is produced from the URI's entry in `verdicts_by_uri`.
    """

    def __init__(self, verdicts_by_uri):
        self.verdicts_by_uri = verdicts_by_uri
        self.calls = []

    def classify(self, topic_description, posts, bios=None, **kwargs):
        self.calls.append((topic_description, [post.uri for post in posts]))
        verdicts = []
        for index, post in enumerate(posts):
            on_topic, confidence = self.verdicts_by_uri.get(post.uri, (False, 0.0))
            verdicts.append(
                SimpleNamespace(
                    id=index, on_topic=on_topic, confidence=confidence, story=post.uri
                )
            )
        return verdicts


class RecordingCovesClient(CovesClient):
    """
    Real embed building, no network. create_post records its kwargs and
    returns a fake at-uri.
    """

    def __init__(self):
        super().__init__(api_url=COVES_API_URL, api_key=FAKE_COVES_API_KEY)
        self.authenticate_calls = 0
        self.create_post_calls = []

    def authenticate(self):
        self.authenticate_calls += 1

    def create_post(self, community_handle, content, facets, title=None, embed=None, thumbnail_url=None):
        self.create_post_calls.append(
            {
                "community_handle": community_handle,
                "content": content,
                "facets": facets,
                "title": title,
                "embed": embed,
                "thumbnail_url": thumbnail_url,
            }
        )
        return f"at://did:plc:coves/social.coves.post/{len(self.create_post_calls)}"


def write_config(tmp_path: Path) -> Path:
    config = {
        "coves_api_url": COVES_API_URL,
        "bluesky_api_url": BLUESKY_API_URL,
        "window_hours": 6,
        "max_posts_per_run": 2,
        "classify_candidates": 50,
        "min_confidence": 0.7,
        "classifier_model": "claude-opus-5",
        "log_level": "info",
        "topics": [
            {
                "name": TOPIC_NAME,
                "community_handle": COMMUNITY_HANDLE,
                "description": TOPIC_DESCRIPTION,
                "feeds": [FEED_URI],
                "lists": [],
                "excluded_handles": [EXCLUDED_HANDLE],
                "min_likes": 5,
                "enabled": True,
            }
        ],
    }
    config_path = tmp_path / "config.yaml"
    config_path.write_text(yaml.safe_dump(config))
    return config_path


def register_bluesky_endpoints():
    responses.add(
        responses.GET,
        f"{BLUESKY_API_URL}/xrpc/app.bsky.feed.getFeed",
        json=FEED_PAGE,
        status=200,
    )
    responses.add(
        responses.GET,
        f"{BLUESKY_API_URL}/xrpc/app.bsky.actor.getProfile",
        json={"did": "did:plc:whoever", "handle": "whoever.bsky.social", "description": "Covers the NBA."},
        status=200,
    )


def make_classifier() -> FakeClassifier:
    return FakeClassifier(
        {
            POST_50["uri"]: (False, 0.95),
            POST_40["uri"]: (True, 0.9),
            POST_30["uri"]: (True, 0.9),
            POST_20["uri"]: (True, 0.9),
        }
    )


def run_once(config_path: Path, state_file: Path) -> RecordingCovesClient:
    coves_client = RecordingCovesClient()
    aggregator = Aggregator(
        config_path=config_path,
        state_file=state_file,
        coves_client=coves_client,
        classifier=make_classifier(),
        community_feed_reader=FakeCommunityFeedReader(set()),
        now=lambda: NOW,
    )
    aggregator.run()
    return coves_client


def posted_uris_in_state(state_file: Path):
    state = json.loads(state_file.read_text())
    return [entry["guid"] for entry in state["feeds"][TOPIC_NAME]["posted_guids"]]


def permalink(post) -> str:
    rkey = post["uri"].rsplit("/", 1)[1]
    return f"https://bsky.app/profile/{post['author']['handle']}/post/{rkey}"


def assert_post_call(call, post):
    assert call["community_handle"] == COMMUNITY_HANDLE
    assert call["content"] == ""
    assert call["facets"] == []
    display_name = post["author"]["displayName"]
    assert call["title"].startswith(f"{display_name}: ")
    embed = call["embed"]
    assert embed["$type"] == "social.coves.embed.external"
    external = embed["external"]
    assert external["uri"] == permalink(post)
    assert external["provider"] == "bluesky"
    assert external["domain"] == "bsky.app"
    assert external["embedType"] == "website"
    assert external["title"].startswith(display_name)
    assert external["sources"] == [
        {"uri": permalink(post), "title": "Bluesky", "domain": "bsky.app"}
    ]


@responses.activate
def test_three_runs_cross_post_top_on_topic_posts_once(tmp_path):
    register_bluesky_endpoints()
    config_path = write_config(tmp_path)
    state_file = tmp_path / "data" / "state.json"

    # Run 1: 40 and 30, in score order. 50 is off-topic; 20 is cut by max_posts_per_run.
    first = run_once(config_path, state_file)
    assert first.authenticate_calls == 1
    assert len(first.create_post_calls) == 2
    assert_post_call(first.create_post_calls[0], POST_40)
    assert_post_call(first.create_post_calls[1], POST_30)
    # Whitespace in the source text is collapsed in the title.
    assert first.create_post_calls[0]["title"] == (
        "Shams Charania: The Lakers are trading for a new center, sources say."
    )
    assert set(posted_uris_in_state(state_file)) == {POST_40["uri"], POST_30["uri"]}

    # Run 2: fresh Aggregator over the same state file. The top two stories are
    # already posted, and skipped stories are not backfilled by the 20-like post.
    second = run_once(config_path, state_file)
    assert second.create_post_calls == []
    assert set(posted_uris_in_state(state_file)) == {POST_40["uri"], POST_30["uri"]}

    # Run 3: still nothing.
    third = run_once(config_path, state_file)
    assert third.create_post_calls == []
    assert set(posted_uris_in_state(state_file)) == {POST_40["uri"], POST_30["uri"]}

    # Nothing that should have been dropped ever reached Coves.
    all_posted = {
        call["embed"]["external"]["uri"]
        for client in (first, second, third)
        for call in client.create_post_calls
    }
    assert all_posted == {permalink(POST_40), permalink(POST_30)}


# ---------------------------------------------------------------------------
# Run 2 acceptance: story dedup, community-aware dedup, quote-inclusive score,
# and skip-not-backfill. Two runs over one state file with a fixed clock:
#
#   run 1 (fresh Aggregator) posts A1 then C
#   run 2 (fresh Aggregator, same state file, same fakes) posts nothing
# ---------------------------------------------------------------------------

STORY_WINDOW_HOURS = 6
STORY_MAX_POSTS_PER_RUN = 3

# A1 outscores A2 only when quotes count: likes+reposts is 30 (< A2's 45),
# likes+reposts+quotes is 50.
STORY_POST_A1 = make_post(
    rkey="ausar1",
    author_did="did:plc:shamsbot",
    handle="shamsbot.bsky.social",
    display_name="Shams Bot",
    text="Ausar Thompson agrees to a five-year extension with the Pistons, sources say.",
    age=timedelta(hours=1),
    like_count=20,
    repost_count=10,
)
STORY_POST_A1["quoteCount"] = 20

STORY_POST_A2 = make_post(
    rkey="ausar2",
    author_did="did:plc:pistonsfan",
    handle="pistonsfan.bsky.social",
    display_name="Pistons Fan",
    text="Ausar Thompson locked in through 2031. Huge for Detroit.",
    age=timedelta(minutes=30),
    like_count=45,
)
STORY_POST_B = make_post(
    rkey="cavsgm",
    author_did="did:plc:cavsbeat",
    handle="cavsbeat.bsky.social",
    display_name="Cavs Beat",
    text="The Cavaliers have hired a new general manager.",
    age=timedelta(hours=2),
    like_count=40,
)
STORY_POST_C = make_post(
    rkey="larsson",
    author_did="did:plc:kingsbeat",
    handle="kingsbeat.bsky.social",
    display_name="Kings Beat",
    text="Pelle Larsson signs a contract extension with the Kings.",
    age=timedelta(hours=3),
    like_count=30,
)
STORY_POST_D = make_post(
    rkey="duren",
    author_did="did:plc:capguru",
    handle="capguru.bsky.social",
    display_name="Cap Guru",
    text="Jalen Duren will be a restricted free agent in 2027.",
    age=timedelta(hours=4),
    like_count=20,
)
STORY_POST_E = make_post(
    rkey="chatter",
    author_did="did:plc:casual",
    handle="casual.bsky.social",
    display_name="Casual Fan",
    text="Love watching basketball on a Friday night.",
    age=timedelta(hours=5),
    like_count=10,
)

STORY_FEED_PAGE = {
    "feed": [
        # Deliberately not in score order so ordering is asserted, not inherited.
        {"post": STORY_POST_C},
        {"post": STORY_POST_A2},
        {"post": STORY_POST_E},
        {"post": STORY_POST_B},
        {"post": STORY_POST_D},
        {"post": STORY_POST_A1},
    ]
    # No cursor: single page.
}

STORY_KEYS_BY_URI = {
    STORY_POST_A1["uri"]: "ausar-extension",
    STORY_POST_A2["uri"]: "ausar-extension",
    STORY_POST_B["uri"]: "cavs-gm",
    STORY_POST_C["uri"]: "larsson-extension",
    STORY_POST_D["uri"]: "duren-2027",
    STORY_POST_E["uri"]: "e-chatter",
}


class FakeStoryClassifier:
    """
    Stands in for TopicClassifier in Run 2: verdicts are keyed by post URI
    and carry a `story` key. Every known URI is on-topic at 0.9; an unknown
    URI is off-topic with a story key unique to that post (its URI).
    """

    def __init__(self, story_keys_by_uri):
        self.story_keys_by_uri = story_keys_by_uri
        self.calls = []

    def classify(self, topic_description, posts, bios=None, **kwargs):
        self.calls.append((topic_description, [post.uri for post in posts]))
        verdicts = []
        for index, post in enumerate(posts):
            story = self.story_keys_by_uri.get(post.uri)
            if story is None:
                verdicts.append(SimpleNamespace(id=index, on_topic=False, confidence=0.0, story=post.uri))
            else:
                verdicts.append(SimpleNamespace(id=index, on_topic=True, confidence=0.9, story=story))
        return verdicts


class FakeCommunityFeedReader:
    """
    Stands in for CommunityFeedReader: records every lookup and returns a
    fixed set of external link URIs found in the community feed.
    """

    def __init__(self, external_uris):
        self.external_uris = set(external_uris)
        self.calls = []

    def external_uris_since(self, community_handle, since):
        self.calls.append((community_handle, since))
        return set(self.external_uris)


def write_story_config(tmp_path: Path) -> Path:
    config = {
        "coves_api_url": COVES_API_URL,
        "bluesky_api_url": BLUESKY_API_URL,
        "window_hours": STORY_WINDOW_HOURS,
        "max_posts_per_run": STORY_MAX_POSTS_PER_RUN,
        "classify_candidates": 50,
        "min_confidence": 0.7,
        "classifier_model": "claude-opus-5",
        "log_level": "info",
        "topics": [
            {
                "name": TOPIC_NAME,
                "community_handle": COMMUNITY_HANDLE,
                "description": TOPIC_DESCRIPTION,
                "feeds": [FEED_URI],
                "lists": [],
                "excluded_handles": [EXCLUDED_HANDLE],
                "min_likes": 5,
                "enabled": True,
            }
        ],
    }
    config_path = tmp_path / "config.yaml"
    config_path.write_text(yaml.safe_dump(config))
    return config_path


def register_story_bluesky_endpoints():
    responses.add(
        responses.GET,
        f"{BLUESKY_API_URL}/xrpc/app.bsky.feed.getFeed",
        json=STORY_FEED_PAGE,
        status=200,
    )
    responses.add(
        responses.GET,
        f"{BLUESKY_API_URL}/xrpc/app.bsky.actor.getProfile",
        json={"did": "did:plc:whoever", "handle": "whoever.bsky.social", "description": "Covers the NBA."},
        status=200,
    )


def run_story_once(config_path: Path, state_file: Path):
    """One fresh Aggregator over the shared state file; returns (coves, reader, classifier)."""
    coves_client = RecordingCovesClient()
    community_feed_reader = FakeCommunityFeedReader({permalink(STORY_POST_B)})
    classifier = FakeStoryClassifier(STORY_KEYS_BY_URI)
    aggregator = Aggregator(
        config_path=config_path,
        state_file=state_file,
        coves_client=coves_client,
        classifier=classifier,
        now=lambda: NOW,
        community_feed_reader=community_feed_reader,
    )
    aggregator.run()
    return coves_client, community_feed_reader, classifier


@responses.activate
def test_two_runs_post_one_representative_per_story_and_skip_community_posted_stories(tmp_path):
    register_story_bluesky_endpoints()
    config_path = write_story_config(tmp_path)
    state_file = tmp_path / "data" / "state.json"
    window_start = NOW - timedelta(hours=STORY_WINDOW_HOURS)

    # Run 1: top three stories are ausar-extension (A1 50), cavs-gm (B 40), larsson-extension (C 30).
    # A1 represents its story (A2 loses within the story); B's story is blocked because a Coves
    # user already posted B to the community; D (story #4) is not backfilled into B's slot.
    first, first_reader, first_classifier = run_story_once(config_path, state_file)
    assert first.authenticate_calls == 1
    assert first_reader.calls == [(COMMUNITY_HANDLE, window_start)]
    assert len(first_classifier.calls) == 1
    assert len(first.create_post_calls) == 2, [
        call["embed"]["external"]["uri"] for call in first.create_post_calls
    ]
    assert_post_call(first.create_post_calls[0], STORY_POST_A1)
    assert_post_call(first.create_post_calls[1], STORY_POST_C)
    assert set(posted_uris_in_state(state_file)) == {STORY_POST_A1["uri"], STORY_POST_C["uri"]}

    # Run 2: fresh Aggregator, same state file, same fakes. The top three stories are all
    # already posted (A and C by the aggregator, B by the community); nothing is backfilled.
    second, second_reader, _ = run_story_once(config_path, state_file)
    assert second_reader.calls == [(COMMUNITY_HANDLE, window_start)]
    assert second.create_post_calls == []
    assert set(posted_uris_in_state(state_file)) == {STORY_POST_A1["uri"], STORY_POST_C["uri"]}

    # Nothing else ever reached Coves.
    all_posted = {
        call["embed"]["external"]["uri"]
        for client in (first, second)
        for call in client.create_post_calls
    }
    assert all_posted == {permalink(STORY_POST_A1), permalink(STORY_POST_C)}
