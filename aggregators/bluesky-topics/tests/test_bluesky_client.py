"""
Tests for bluesky_client module: feed/list paging, profile lookup, and
PostView parsing.
"""
import copy
import json
from datetime import datetime, timezone

import pytest
import requests
import responses
from responses import matchers

from src.bluesky_client import (
    BlueskyAPIError,
    BlueskyClient,
    FeedFetchResult,
    parse_post_view,
)

BASE_URL = "https://bsky.test"
GET_FEED_URL = f"{BASE_URL}/xrpc/app.bsky.feed.getFeed"
GET_LIST_FEED_URL = f"{BASE_URL}/xrpc/app.bsky.feed.getListFeed"
GET_PROFILE_URL = f"{BASE_URL}/xrpc/app.bsky.actor.getProfile"
FEED_URI = "at://did:plc:feedowner/app.bsky.feed.generator/nba-essential"
LIST_URI = "at://did:plc:listowner/app.bsky.graph.list/3kabc"


def item(number: int) -> dict:
    return {"post": {"uri": f"at://did:plc:a/app.bsky.feed.post/{number}"}}


def add_feed_page(url, source_param, source_uri, items, cursor=None, request_cursor=None, status=200, body=None):
    params = {source_param: source_uri, "limit": "100"}
    if request_cursor is not None:
        params["cursor"] = request_cursor
    page = {"feed": items}
    if cursor is not None:
        page["cursor"] = cursor
    kwargs = {"json": page} if body is None else {"body": body}
    responses.add(
        responses.GET,
        url,
        status=status,
        match=[matchers.query_param_matcher(params)],
        **kwargs,
    )


class TestGetFeed:
    @responses.activate
    def test_single_page_returns_items_with_user_agent(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(1), item(2)])

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert isinstance(result, FeedFetchResult)
        assert result.items == [item(1), item(2)]
        assert result.partial is False
        assert len(responses.calls) == 1
        user_agent = responses.calls[0].request.headers["User-Agent"]
        assert user_agent.startswith("coves-bluesky-topics/")

    @responses.activate
    def test_follows_cursor_until_page_without_cursor(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(1)], cursor="c1")
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(2)], cursor="c2", request_cursor="c1")
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(3)], request_cursor="c2")

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert result.items == [item(1), item(2), item(3)]
        assert result.partial is False
        assert len(responses.calls) == 3

    @responses.activate
    def test_stops_when_cursor_repeats(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(1)], cursor="c1")
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(2)], cursor="c1", request_cursor="c1")

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert result.items == [item(1), item(2)]
        assert result.partial is False
        assert len(responses.calls) == 2

    @responses.activate
    def test_stops_after_five_pages(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(1)], cursor="c1")
        for page_number in range(2, 8):
            add_feed_page(
                GET_FEED_URL,
                "feed",
                FEED_URI,
                [item(page_number)],
                cursor=f"c{page_number}",
                request_cursor=f"c{page_number - 1}",
            )

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert result.items == [item(number) for number in range(1, 6)]
        assert len(responses.calls) == 5

    @responses.activate
    def test_empty_feed_is_not_an_error(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [])

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert result.items == []
        assert result.partial is False

    @responses.activate
    def test_later_page_failure_keeps_earlier_pages_and_marks_partial(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [item(1)], cursor="c1")
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [], request_cursor="c1", status=500, body="boom")

        result = BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert result.items == [item(1)]
        assert result.partial is True

    @responses.activate
    def test_malformed_json_on_first_page_raises(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [], body="<html>not json</html>")

        with pytest.raises(BlueskyAPIError):
            BlueskyClient(BASE_URL).get_feed(FEED_URI)

    @responses.activate
    def test_non_2xx_on_first_page_raises_with_status_code(self):
        add_feed_page(GET_FEED_URL, "feed", FEED_URI, [], status=502, body="bad gateway")

        with pytest.raises(BlueskyAPIError) as excinfo:
            BlueskyClient(BASE_URL).get_feed(FEED_URI)

        assert excinfo.value.status_code == 502

    @responses.activate
    def test_transport_timeout_raises(self):
        responses.add(responses.GET, GET_FEED_URL, body=requests.Timeout("slow"))

        with pytest.raises(BlueskyAPIError):
            BlueskyClient(BASE_URL).get_feed(FEED_URI)

    def test_requests_use_configured_timeout(self, monkeypatch):
        recorded = []

        def fake_request(self, method, url, **kwargs):
            recorded.append(kwargs)
            response = requests.Response()
            response.status_code = 200
            response._content = json.dumps({"feed": []}).encode()
            response.url = url
            return response

        monkeypatch.setattr(requests.Session, "request", fake_request)

        BlueskyClient(BASE_URL, timeout=30).get_feed(FEED_URI)

        assert len(recorded) == 1
        assert recorded[0]["timeout"] == 30


class TestGetListFeed:
    @responses.activate
    def test_follows_cursor_against_get_list_feed(self):
        add_feed_page(GET_LIST_FEED_URL, "list", LIST_URI, [item(1)], cursor="c1")
        add_feed_page(GET_LIST_FEED_URL, "list", LIST_URI, [item(2)], request_cursor="c1")

        result = BlueskyClient(BASE_URL).get_list_feed(LIST_URI)

        assert isinstance(result, FeedFetchResult)
        assert result.items == [item(1), item(2)]
        assert result.partial is False
        assert len(responses.calls) == 2
        assert responses.calls[0].request.headers["User-Agent"].startswith("coves-bluesky-topics/")


class TestGetProfile:
    @responses.activate
    def test_returns_description(self):
        responses.add(
            responses.GET,
            GET_PROFILE_URL,
            json={"did": "did:plc:woj", "handle": "woj.bsky.social", "description": "NBA insider."},
            match=[matchers.query_param_matcher({"actor": "did:plc:woj"})],
        )

        description = BlueskyClient(BASE_URL).get_profile("did:plc:woj")

        assert description == "NBA insider."

    @responses.activate
    def test_missing_description_is_empty_string(self):
        responses.add(
            responses.GET,
            GET_PROFILE_URL,
            json={"did": "did:plc:woj", "handle": "woj.bsky.social"},
            match=[matchers.query_param_matcher({"actor": "did:plc:woj"})],
        )

        assert BlueskyClient(BASE_URL).get_profile("did:plc:woj") == ""

    @responses.activate
    def test_non_2xx_raises(self):
        responses.add(responses.GET, GET_PROFILE_URL, status=400, json={"error": "InvalidRequest"})

        with pytest.raises(BlueskyAPIError) as excinfo:
            BlueskyClient(BASE_URL).get_profile("did:plc:missing")

        assert excinfo.value.status_code == 400


FULL_POST_VIEW = {
    "uri": "at://did:plc:woj/app.bsky.feed.post/3kpost",
    "cid": "bafycid",
    "author": {
        "did": "did:plc:woj",
        "handle": "woj.bsky.social",
        "displayName": "Adrian Wojnarowski",
        "labels": [{"val": "!no-unauthenticated", "src": "did:plc:woj"}],
    },
    "record": {
        "$type": "app.bsky.feed.post",
        "text": "Celtics guard out six weeks.",
        "createdAt": "2026-09-18T11:00:00.123Z",
    },
    "likeCount": 30,
    "repostCount": 4,
    "replyCount": 2,
    "quoteCount": 1,
    "labels": [{"val": "porn", "src": "did:plc:labeler"}],
    "embed": {"$type": "app.bsky.embed.images#view", "images": []},
    "indexedAt": "2026-09-18T11:00:01.000Z",
}


def full_item(**post_overrides) -> dict:
    post = copy.deepcopy(FULL_POST_VIEW)
    post.update(post_overrides)
    return {"post": post}


def without(post: dict, *path):
    """Return a copy of post with the nested key at `path` removed."""
    post = copy.deepcopy(post)
    parent = post
    for key in path[:-1]:
        parent = parent[key]
    del parent[path[-1]]
    return post


class TestParsePostView:
    def test_parses_every_field(self):
        post = parse_post_view(full_item())

        assert post is not None
        assert post.uri == "at://did:plc:woj/app.bsky.feed.post/3kpost"
        assert post.cid == "bafycid"
        assert post.author_did == "did:plc:woj"
        assert post.author_handle == "woj.bsky.social"
        assert post.author_display_name == "Adrian Wojnarowski"
        assert post.text == "Celtics guard out six weeks."
        assert post.created_at == datetime(2026, 9, 18, 11, 0, 0, 123000, tzinfo=timezone.utc)
        assert post.created_at.tzinfo is not None
        assert post.like_count == 30
        assert post.repost_count == 4
        assert post.reply_count == 2
        assert post.quote_count == 1
        assert post.is_reply is False
        assert list(post.labels) == ["porn"]
        assert list(post.author_labels) == ["!no-unauthenticated"]
        assert post.embed_type == "app.bsky.embed.images#view"

    def test_defaults_for_optional_fields(self):
        post_view = FULL_POST_VIEW
        for path in (
            ("author", "displayName"),
            ("author", "labels"),
            ("labels",),
            ("likeCount",),
            ("repostCount",),
            ("replyCount",),
            ("quoteCount",),
            ("embed",),
        ):
            post_view = without(post_view, *path)

        post = parse_post_view({"post": post_view})

        assert post is not None
        assert post.author_display_name == ""
        assert list(post.author_labels) == []
        assert list(post.labels) == []
        assert post.like_count == 0
        assert post.repost_count == 0
        assert post.reply_count == 0
        assert post.quote_count == 0
        assert post.embed_type is None

    def test_record_with_media_resolves_to_media_type(self):
        embed = {
            "$type": "app.bsky.embed.recordWithMedia#view",
            "record": {"$type": "app.bsky.embed.record#view"},
            "media": {"$type": "app.bsky.embed.video#view", "playlist": "https://video.test/p.m3u8"},
        }

        post = parse_post_view(full_item(embed=embed))

        assert post is not None
        assert post.embed_type == "app.bsky.embed.video#view"

    def test_reply_is_flagged(self):
        record = dict(
            FULL_POST_VIEW["record"],
            reply={
                "root": {"uri": "at://did:plc:x/app.bsky.feed.post/root", "cid": "bafyroot"},
                "parent": {"uri": "at://did:plc:x/app.bsky.feed.post/root", "cid": "bafyroot"},
            },
        )

        post = parse_post_view(full_item(record=record))

        assert post is not None
        assert post.is_reply is True

    @pytest.mark.parametrize(
        "reason",
        [
            pytest.param(
                {
                    "$type": "app.bsky.feed.defs#reasonRepost",
                    "by": {"did": "did:plc:reposter", "handle": "reposter.bsky.social"},
                    "indexedAt": "2026-09-18T11:30:00.000Z",
                },
                id="repost",
            ),
            pytest.param({"$type": "app.bsky.feed.defs#reasonPin"}, id="pin"),
        ],
    )
    def test_item_with_reason_is_skipped(self, reason):
        feed_item = full_item()
        feed_item["reason"] = reason

        assert parse_post_view(feed_item) is None

    @pytest.mark.parametrize(
        "path",
        [
            pytest.param(("uri",), id="missing-uri"),
            pytest.param(("author", "did"), id="missing-author-did"),
            pytest.param(("author", "handle"), id="missing-author-handle"),
            pytest.param(("record", "createdAt"), id="missing-created-at"),
        ],
    )
    def test_missing_required_field_is_skipped(self, path):
        assert parse_post_view({"post": without(FULL_POST_VIEW, *path)}) is None

    def test_unparseable_created_at_is_skipped(self):
        record = dict(FULL_POST_VIEW["record"], createdAt="yesterday-ish")

        assert parse_post_view(full_item(record=record)) is None
