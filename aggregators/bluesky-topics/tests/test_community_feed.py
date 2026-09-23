"""
Tests for community_feed module: collecting external link uris posted to a
Coves community since a given time, with pagination and failure handling.
"""
import json
import logging
from datetime import datetime, timedelta, timezone

import pytest
import requests
import responses
from responses import matchers

from src.community_feed import CommunityFeedError, CommunityFeedReader

BASE_URL = "https://coves.test"
FEED_URL = f"{BASE_URL}/xrpc/social.coves.communityFeed.getCommunity"
COMMUNITY = "nba.coves.test"
SINCE = datetime(2026, 9, 19, 0, 0, 0, tzinfo=timezone.utc)


def external_item(number: int, age: timedelta = timedelta(0), link=None, created_at=None) -> dict:
    created = created_at if created_at is not None else (SINCE + timedelta(hours=1) - age).strftime("%Y-%m-%dT%H:%M:%SZ")
    return {
        "post": {
            "uri": f"at://did:plc:coves/social.coves.post.record/{number}",
            "createdAt": created,
            "author": {"handle": "someone.coves.test"},
            "embed": {
                "$type": "social.coves.embed.external#view",
                "external": {"uri": link or f"https://bsky.app/profile/a.test/post/{number}"},
            },
        }
    }


def post_embed_item(number: int, at_uri: str, created_at=None) -> dict:
    """Feed item whose link post the AppView turned into a Bluesky post embed."""
    item = external_item(number, created_at=created_at)
    item["post"]["embed"] = {
        "$type": "social.coves.embed.post#view",
        "post": {"cid": f"bafy{number}", "uri": at_uri},
        "resolved": {"text": f"quoted {number}"},
    }
    return item


def add_page(items, cursor=None, request_cursor=None, status=200, body=None, page=None):
    params = {"community": COMMUNITY, "sort": "new", "limit": "50"}
    if request_cursor is not None:
        params["cursor"] = request_cursor
    if page is None:
        page = {"feed": items}
        if cursor is not None:
            page["cursor"] = cursor
    kwargs = {"json": page} if body is None else {"body": body}
    responses.add(
        responses.GET,
        FEED_URL,
        status=status,
        match=[matchers.query_param_matcher(params)],
        **kwargs,
    )


def read(**kwargs):
    return CommunityFeedReader(BASE_URL, **kwargs).external_uris_since(COMMUNITY, SINCE)


class TestCollectsExternalUris:
    @responses.activate
    def test_collects_uris_from_external_embeds_with_user_agent(self):
        add_page([external_item(1, link="https://bsky.app/profile/a.test/post/one"),
                  external_item(2, link="https://example.test/article")])

        result = read()

        assert result == {"https://bsky.app/profile/a.test/post/one", "https://example.test/article"}
        assert len(responses.calls) == 1
        user_agent = responses.calls[0].request.headers["User-Agent"]
        assert user_agent.startswith("coves-bluesky-topics/")

    @responses.activate
    def test_skips_posts_without_external_embed(self):
        no_embed = external_item(1, link="https://skip.test/no-embed")
        del no_embed["post"]["embed"]
        image = external_item(2, link="https://skip.test/image")
        image["post"]["embed"] = {"$type": "social.coves.embed.images#view", "images": []}
        malformed = external_item(3, link="https://skip.test/malformed")
        malformed["post"]["embed"] = {"$type": "social.coves.embed.external#view", "external": {}}
        non_dict_external = external_item(4, link="https://skip.test/non-dict")
        non_dict_external["post"]["embed"] = {"$type": "social.coves.embed.external#view", "external": "x"}
        add_page([no_embed, image, malformed, non_dict_external, external_item(5, link="https://keep.test/ok")])

        assert read() == {"https://keep.test/ok"}

    @responses.activate
    def test_skips_items_missing_uri_or_created_at_or_unparseable_created_at(self):
        missing_uri = external_item(1, link="https://skip.test/no-uri")
        del missing_uri["post"]["uri"]
        missing_created = external_item(2, link="https://skip.test/no-created")
        del missing_created["post"]["createdAt"]
        bad_created = external_item(3, link="https://skip.test/bad-created", created_at="not-a-date")
        no_post = {"nope": {}}
        add_page([missing_uri, missing_created, bad_created, no_post, external_item(4, link="https://keep.test/ok")])

        assert read() == {"https://keep.test/ok"}

    @responses.activate
    def test_includes_posts_at_or_after_since_and_excludes_older_on_mixed_page(self):
        at_since = external_item(1, link="https://keep.test/at-since", created_at="2026-09-19T00:00:00Z")
        after = external_item(2, link="https://keep.test/after", created_at="2026-09-19T03:30:00Z")
        before = external_item(3, link="https://skip.test/before", created_at="2026-09-18T23:59:59Z")
        add_page([after, at_since, before], cursor="c1")

        result = read()

        assert result == {"https://keep.test/at-since", "https://keep.test/after"}
        assert len(responses.calls) == 1


class TestCollectsPostEmbedUris:
    """
    A bsky.app permalink posted as a link post is transformed by the AppView
    into a Bluesky post embed, so the community feed serves the at:// uri of
    the quoted Bluesky post instead of the external link.
    """

    @responses.activate
    def test_collects_at_uri_from_post_embeds(self):
        add_page([post_embed_item(1, "at://did:plc:eshcqzoowlpbj65gp7yualue/app.bsky.feed.post/3mvshaqd7n22p")])

        assert read() == {"at://did:plc:eshcqzoowlpbj65gp7yualue/app.bsky.feed.post/3mvshaqd7n22p"}

    @responses.activate
    def test_collects_post_embed_and_external_embed_uris_from_one_page(self):
        add_page([
            post_embed_item(1, "at://did:plc:quoted/app.bsky.feed.post/3aaa"),
            external_item(2, link="https://keep.test/article"),
        ])

        assert read() == {"at://did:plc:quoted/app.bsky.feed.post/3aaa", "https://keep.test/article"}

    @responses.activate
    def test_skips_post_embeds_without_an_at_uri(self):
        missing_post = post_embed_item(1, "at://did:plc:quoted/app.bsky.feed.post/3aaa")
        del missing_post["post"]["embed"]["post"]
        non_dict_post = post_embed_item(2, "at://did:plc:quoted/app.bsky.feed.post/3bbb")
        non_dict_post["post"]["embed"]["post"] = "x"
        not_an_at_uri = post_embed_item(3, "https://bsky.app/profile/a.test/post/3ccc")
        add_page([missing_post, non_dict_post, not_an_at_uri,
                  post_embed_item(4, "at://did:plc:quoted/app.bsky.feed.post/3ddd")])

        assert read() == {"at://did:plc:quoted/app.bsky.feed.post/3ddd"}


class TestPagination:
    @responses.activate
    def test_follows_cursor_while_pages_are_within_window(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        add_page([external_item(2, link="https://keep.test/2")], cursor="c2", request_cursor="c1")
        add_page([external_item(3, link="https://keep.test/3")], request_cursor="c2")

        result = read()

        assert result == {"https://keep.test/1", "https://keep.test/2", "https://keep.test/3"}
        assert len(responses.calls) == 3

    @responses.activate
    def test_stops_when_page_oldest_created_at_is_before_since(self):
        add_page([external_item(1, link="https://keep.test/1"),
                  external_item(2, link="https://skip.test/old", created_at="2026-09-18T20:00:00Z")], cursor="c1")
        add_page([external_item(3, link="https://keep.test/never")], request_cursor="c1")

        result = read()

        assert result == {"https://keep.test/1"}
        assert len(responses.calls) == 1

    @responses.activate
    def test_stops_when_cursor_repeats(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        add_page([external_item(2, link="https://keep.test/2")], cursor="c1", request_cursor="c1")

        result = read()

        assert result == {"https://keep.test/1", "https://keep.test/2"}
        assert len(responses.calls) == 2

    @responses.activate
    def test_stops_after_five_pages(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        previous = "c1"
        for number in range(2, 7):
            add_page([external_item(number, link=f"https://keep.test/{number}")],
                     cursor=f"c{number}", request_cursor=previous)
            previous = f"c{number}"

        result = read()

        assert len(responses.calls) == 5
        assert result == {f"https://keep.test/{number}" for number in range(1, 6)}

    @responses.activate
    def test_empty_page_with_cursor_continues(self):
        add_page([], cursor="c1")
        add_page([external_item(1, link="https://keep.test/1")], request_cursor="c1")

        result = read()

        assert result == {"https://keep.test/1"}
        assert len(responses.calls) == 2

    @responses.activate
    def test_empty_pages_with_cursors_stop_at_the_cap(self):
        add_page([], cursor="c1")
        previous = "c1"
        for number in range(2, 8):
            add_page([], cursor=f"c{number}", request_cursor=previous)
            previous = f"c{number}"

        assert read() == set()
        assert len(responses.calls) == 5


class TestEmptyCommunityResponses:
    """A community with no posts serialises as ``{"feed": null}``, not an error."""

    @responses.activate
    def test_null_feed_is_an_empty_page_without_warnings(self, caplog):
        add_page([], page={"feed": None, "cursor": "c1"})

        with caplog.at_level(logging.INFO):
            result = read()

        assert result == set()
        assert len(responses.calls) == 1
        assert [record for record in caplog.records if record.levelno >= logging.WARNING] == []

    @responses.activate
    def test_missing_feed_is_an_empty_page_without_warnings(self, caplog):
        add_page([], page={})

        with caplog.at_level(logging.INFO):
            result = read()

        assert result == set()
        assert len(responses.calls) == 1
        assert [record for record in caplog.records if record.levelno >= logging.WARNING] == []


class TestFirstPageFailures:
    @responses.activate
    def test_non_2xx_raises_with_status_code(self):
        add_page([], status=503, body="unavailable")

        with pytest.raises(CommunityFeedError) as excinfo:
            read()

        assert excinfo.value.status_code == 503

    @responses.activate
    def test_malformed_json_raises(self):
        add_page([], body="{not json")

        with pytest.raises(CommunityFeedError):
            read()

    @responses.activate
    def test_non_object_json_raises(self):
        add_page([], page=[1, 2, 3])

        with pytest.raises(CommunityFeedError):
            read()

    @responses.activate
    def test_non_list_feed_raises(self):
        add_page([], page={"feed": {"post": {}}})

        with pytest.raises(CommunityFeedError):
            read()

    @responses.activate
    def test_string_feed_raises(self):
        add_page([], page={"feed": "nope"})

        with pytest.raises(CommunityFeedError):
            read()

    @responses.activate
    def test_timeout_raises(self):
        responses.add(responses.GET, FEED_URL, body=requests.Timeout("slow"))

        with pytest.raises(CommunityFeedError):
            read()

    @responses.activate
    def test_connection_error_raises(self):
        responses.add(responses.GET, FEED_URL, body=requests.ConnectionError("refused"))

        with pytest.raises(CommunityFeedError):
            read()


class TestLaterPageFailures:
    @responses.activate
    def test_non_2xx_on_page_two_returns_page_one_uris(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        add_page([], status=500, body="boom", request_cursor="c1")

        assert read() == {"https://keep.test/1"}

    @responses.activate
    def test_malformed_json_on_page_two_returns_page_one_uris(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        add_page([], body="<html>", request_cursor="c1")

        assert read() == {"https://keep.test/1"}

    @responses.activate
    def test_timeout_on_page_two_returns_page_one_uris(self):
        add_page([external_item(1, link="https://keep.test/1")], cursor="c1")
        responses.add(
            responses.GET,
            FEED_URL,
            body=requests.Timeout("slow"),
            match=[matchers.query_param_matcher({"community": COMMUNITY, "sort": "new", "limit": "50", "cursor": "c1"})],
        )

        assert read() == {"https://keep.test/1"}


class TestTransport:
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

        CommunityFeedReader(BASE_URL, timeout=30).external_uris_since(COMMUNITY, SINCE)

        assert len(recorded) == 1
        assert recorded[0]["timeout"] == 30

    def test_default_timeout_is_thirty_seconds(self, monkeypatch):
        recorded = []

        def fake_request(self, method, url, **kwargs):
            recorded.append(kwargs)
            response = requests.Response()
            response.status_code = 200
            response._content = json.dumps({"feed": []}).encode()
            response.url = url
            return response

        monkeypatch.setattr(requests.Session, "request", fake_request)

        CommunityFeedReader(BASE_URL).external_uris_since(COMMUNITY, SINCE)

        assert recorded[0]["timeout"] == 30
