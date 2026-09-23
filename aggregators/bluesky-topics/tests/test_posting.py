"""
Tests for posting module: selection by verdict, permalink, title, and embed building.
"""
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

import pytest

from src import posting
from src.coves_client import CovesClient
from src.models import BlueskyPost
from src.posting import build_embed, build_permalink, build_title, select_posts

NOW = datetime(2026, 9, 18, 12, 0, 0, tzinfo=timezone.utc)
TITLE_LIMIT = 280
VALID_API_KEY = "ckapi_" + "a" * 64


def make_post(rkey="p1", age=timedelta(0), likes=0, reposts=0, quotes=0, **overrides) -> BlueskyPost:
    fields = {
        "uri": f"at://did:plc:author/app.bsky.feed.post/{rkey}",
        "cid": f"bafy{rkey}",
        "author_did": "did:plc:author",
        "author_handle": "author.bsky.social",
        "author_display_name": "Author Name",
        "text": f"post {rkey}",
        "created_at": NOW - age,
        "like_count": likes,
        "repost_count": reposts,
        "quote_count": quotes,
    }
    fields.update(overrides)
    return BlueskyPost(**fields)


def verdict(index, on_topic=True, confidence=0.9, story=None):
    """
    Verdict stand-in with the attributes select_posts reads. Each verdict
    defaults to a story key unique to its id, i.e. every post is its own story
    unless a test says otherwise.
    """
    return SimpleNamespace(
        id=index, on_topic=on_topic, confidence=confidence, story=story if story is not None else f"story-{index}"
    )


def select(posts, verdicts, *, min_confidence=0.7, limit=10, already_posted=()):
    return select_posts(
        posts, verdicts, min_confidence=min_confidence, limit=limit, already_posted=set(already_posted)
    )


class TestSelectPosts:
    def test_keeps_on_topic_posts_at_or_above_threshold(self):
        # Scores descend with input order so the expected order is unambiguous.
        posts = [make_post("a", likes=30), make_post("b", likes=20), make_post("c", likes=10)]
        verdicts = [verdict(0, True, 0.7), verdict(1, True, 0.69), verdict(2, True, 0.95)]

        assert select(posts, verdicts) == [posts[0], posts[2]]

    def test_off_topic_dropped_even_at_full_confidence(self):
        posts = [make_post("a", likes=30), make_post("b", likes=20)]
        verdicts = [verdict(0, False, 1.0), verdict(1, True, 0.8)]

        assert select(posts, verdicts) == [posts[1]]

    def test_truncates_to_limit_preserving_order(self):
        posts = [make_post(rkey, likes=likes) for rkey, likes in (("a", 40), ("b", 30), ("c", 20), ("d", 10))]
        verdicts = [verdict(index) for index in range(4)]

        assert select(posts, verdicts, limit=2) == [posts[0], posts[1]]

    def test_limit_larger_than_survivors_returns_all_survivors(self):
        posts = [make_post("a", likes=30), make_post("b", likes=20), make_post("c", likes=10)]
        verdicts = [verdict(0), verdict(1, False), verdict(2)]

        assert select(posts, verdicts, limit=50) == [posts[0], posts[2]]

    def test_verdicts_are_matched_by_id_not_position(self):
        posts = [make_post("a", likes=30), make_post("b", likes=20)]
        verdicts = [verdict(1, True, 0.9), verdict(0, False, 0.9)]

        assert select(posts, verdicts) == [posts[1]]

    def test_empty_input_returns_empty_list(self):
        assert select([], []) == []

    def test_empty_input_with_already_posted_returns_empty_list(self):
        assert select([], [], already_posted={"at://did:plc:author/app.bsky.feed.post/x"}) == []


class TestSelectPostsStories:
    def test_one_representative_per_story_is_the_highest_scored_member(self):
        # A1 outscores A2 only when quotes count (20 + 10 + 20 = 50 vs 45).
        a1 = make_post("a1", likes=20, reposts=10, quotes=20)
        a2 = make_post("a2", likes=45)
        verdicts = [verdict(0, story="ausar-extension"), verdict(1, story="ausar-extension")]

        assert select([a2, a1], verdicts) == [a1]
        assert select([a1, a2], verdicts) == [a1]

    def test_equal_scores_within_a_story_prefer_the_newer_post(self):
        older = make_post("older", age=timedelta(hours=3), likes=30)
        newer = make_post("newer", age=timedelta(hours=1), likes=30)
        verdicts = [verdict(0, story="same"), verdict(1, story="same")]

        assert select([older, newer], verdicts) == [newer]
        assert select([newer, older], verdicts) == [newer]

    def test_representative_ignores_off_topic_and_low_confidence_members(self):
        # The top-scored member of the story is off-topic; the next is below
        # threshold; the third on-topic member represents the story.
        top = make_post("top", likes=90)
        low_confidence = make_post("lowconf", likes=80)
        representative = make_post("rep", likes=10)
        verdicts = [
            verdict(0, on_topic=False, confidence=0.9, story="one"),
            verdict(1, on_topic=True, confidence=0.5, story="one"),
            verdict(2, on_topic=True, confidence=0.9, story="one"),
        ]

        assert select([top, low_confidence, representative], verdicts) == [representative]

    def test_stories_are_ordered_by_representative_score_descending(self):
        posts = [make_post("c", likes=10), make_post("a", likes=30), make_post("b", likes=20)]
        verdicts = [verdict(0, story="c"), verdict(1, story="a"), verdict(2, story="b")]

        assert select(posts, verdicts) == [posts[1], posts[2], posts[0]]

    def test_stories_tied_on_score_order_newer_representative_first(self):
        older = make_post("older", age=timedelta(hours=3), likes=30)
        newer = make_post("newer", age=timedelta(hours=1), likes=30)
        verdicts = [verdict(0, story="older-story"), verdict(1, story="newer-story")]

        assert select([older, newer], verdicts) == [newer, older]

    def test_story_ordering_uses_the_representative_not_the_top_member(self):
        # Story X has a 90-score off-topic member and a 10-score representative;
        # story Y's representative scores 20, so Y is ordered before X.
        x_top = make_post("xtop", likes=90)
        x_rep = make_post("xrep", likes=10)
        y_rep = make_post("yrep", likes=20)
        verdicts = [
            verdict(0, on_topic=False, story="x"),
            verdict(1, story="x"),
            verdict(2, story="y"),
        ]

        assert select([x_top, x_rep, y_rep], verdicts) == [y_rep, x_rep]

    def test_limit_counts_stories_not_posts(self):
        posts = [make_post(f"s{index}", likes=50 - index) for index in range(5)]
        second_members = [make_post(f"s{index}-b", likes=1) for index in range(5)]
        verdicts = [verdict(index, story=f"story-{index}") for index in range(5)]
        verdicts += [verdict(5 + index, story=f"story-{index}") for index in range(5)]

        assert select(posts + second_members, verdicts, limit=3) == posts[:3]

    def test_story_with_any_already_posted_member_is_dropped_even_if_that_member_is_off_topic(self):
        a1 = make_post("a1", likes=50)
        a2 = make_post("a2", likes=45)
        c = make_post("c", likes=30)
        verdicts = [
            verdict(0, on_topic=True, confidence=0.9, story="ausar-extension"),
            verdict(1, on_topic=False, confidence=0.3, story="ausar-extension"),
            verdict(2, story="larsson-extension"),
        ]

        assert select([a1, a2, c], verdicts, already_posted={a2.uri}) == [c]

    def test_story_with_a_below_threshold_already_posted_member_is_dropped(self):
        a1 = make_post("a1", likes=50)
        a2 = make_post("a2", likes=45)
        verdicts = [
            verdict(0, on_topic=True, confidence=0.9, story="ausar-extension"),
            verdict(1, on_topic=True, confidence=0.2, story="ausar-extension"),
        ]

        assert select([a1, a2], verdicts, already_posted={a2.uri}) == []

    def test_already_posted_representative_drops_the_whole_story(self):
        a1 = make_post("a1", likes=50)
        a2 = make_post("a2", likes=45)
        verdicts = [verdict(0, story="ausar-extension"), verdict(1, story="ausar-extension")]

        assert select([a1, a2], verdicts, already_posted={a1.uri}) == []

    def test_blocked_story_is_not_backfilled_by_the_next_story(self):
        s1, s2, s3, s4 = (make_post(f"s{index}", likes=50 - 10 * index) for index in range(1, 5))
        verdicts = [verdict(index, story=f"s{index + 1}") for index in range(4)]

        assert select([s1, s2, s3, s4], verdicts, limit=3, already_posted={s2.uri}) == [s1, s3]

    def test_every_top_story_blocked_returns_nothing(self):
        s1, s2, s3, s4 = (make_post(f"s{index}", likes=50 - 10 * index) for index in range(1, 5))
        verdicts = [verdict(index, story=f"s{index + 1}") for index in range(4)]

        assert select([s1, s2, s3, s4], verdicts, limit=3, already_posted={s1.uri, s2.uri, s3.uri}) == []

    def test_already_posted_uri_not_in_candidates_has_no_effect(self):
        s1 = make_post("s1", likes=50)

        assert select([s1], [verdict(0, story="s1")], already_posted={"at://did:plc:other/app.bsky.feed.post/z"}) == [s1]

    def test_blocked_story_below_threshold_still_occupies_a_slot(self):
        # The story was posted on an earlier run; this run its only member dips
        # below the threshold. It must still hold slot 1, so the third
        # unblocked story does not move into the top three.
        blocked = make_post("blocked", likes=90)
        s1, s2, s3 = make_post("s1", likes=30), make_post("s2", likes=20), make_post("s3", likes=10)
        verdicts = [
            verdict(0, on_topic=True, confidence=0.69, story="blocked"),
            verdict(1, story="s1"),
            verdict(2, story="s2"),
            verdict(3, story="s3"),
        ]

        assert select([blocked, s1, s2, s3], verdicts, limit=3, already_posted={blocked.uri}) == [s1, s2]

    def test_blocked_story_that_is_off_topic_this_run_still_occupies_a_slot(self):
        blocked = make_post("blocked", likes=90)
        s1, s2, s3 = make_post("s1", likes=30), make_post("s2", likes=20), make_post("s3", likes=10)
        verdicts = [
            verdict(0, on_topic=False, story="blocked"),
            verdict(1, story="s1"),
            verdict(2, story="s2"),
            verdict(3, story="s3"),
        ]

        assert select([blocked, s1, s2, s3], verdicts, limit=3, already_posted={blocked.uri}) == [s1, s2]

    def test_blocked_story_is_ranked_by_its_highest_scored_member_whatever_its_verdict(self):
        # The blocked story's top member is off-topic and its eligible member
        # scores 10; ranked by the off-topic 90 it takes slot 1, so with
        # limit=2 only the 50-score story survives.
        blocked_top = make_post("blocked-top", likes=90)
        blocked_eligible = make_post("blocked-eligible", likes=10)
        s1, s2 = make_post("s1", likes=50), make_post("s2", likes=20)
        verdicts = [
            verdict(0, on_topic=False, story="blocked"),
            verdict(1, story="blocked"),
            verdict(2, story="s1"),
            verdict(3, story="s2"),
        ]

        selected = select(
            [blocked_top, blocked_eligible, s1, s2], verdicts, limit=2, already_posted={blocked_top.uri}
        )

        assert selected == [s1]

    def test_story_without_an_eligible_member_does_not_occupy_a_slot(self):
        # The highest-scored story is entirely off-topic; it must not count
        # toward the limit, so the three eligible stories are all returned.
        noise = make_post("noise", likes=90)
        s1, s2, s3 = make_post("s1", likes=30), make_post("s2", likes=20), make_post("s3", likes=10)
        verdicts = [
            verdict(0, on_topic=False, story="noise"),
            verdict(1, story="s1"),
            verdict(2, story="s2"),
            verdict(3, story="s3"),
        ]

        assert select([noise, s1, s2, s3], verdicts, limit=3) == [s1, s2, s3]

    def test_story_below_threshold_does_not_occupy_a_slot(self):
        weak = make_post("weak", likes=90)
        s1, s2, s3 = make_post("s1", likes=30), make_post("s2", likes=20), make_post("s3", likes=10)
        verdicts = [
            verdict(0, on_topic=True, confidence=0.69, story="weak"),
            verdict(1, story="s1"),
            verdict(2, story="s2"),
            verdict(3, story="s3"),
        ]

        assert select([weak, s1, s2, s3], verdicts, limit=3) == [s1, s2, s3]

    def test_min_confidence_is_inclusive(self):
        post = make_post("edge", likes=10)

        assert select([post], [verdict(0, True, 0.7)], min_confidence=0.7) == [post]

    def test_post_without_a_verdict_is_ignored(self):
        posts = [make_post("a", likes=30), make_post("b", likes=20)]

        assert select(posts, [verdict(1, story="b")]) == [posts[1]]

    def test_returned_order_follows_story_order_not_input_order(self):
        d = make_post("d", likes=20)
        a1 = make_post("a1", likes=50)
        c = make_post("c", likes=30)
        a2 = make_post("a2", likes=45)
        verdicts = [
            verdict(0, story="duren-2027"),
            verdict(1, story="ausar-extension"),
            verdict(2, story="larsson-extension"),
            verdict(3, story="ausar-extension"),
        ]

        assert select([d, a1, c, a2], verdicts, limit=3) == [a1, c, d]


class TestBuildPermalink:
    def test_uses_handle_and_rkey(self):
        post = make_post("3kabc", author_handle="lebron.bsky.social")

        assert build_permalink(post) == "https://bsky.app/profile/lebron.bsky.social/post/3kabc"

    def test_falls_back_to_did_when_handle_is_invalid(self):
        post = make_post("3kabc", author_handle="handle.invalid", author_did="did:plc:xyz123")

        assert build_permalink(post) == "https://bsky.app/profile/did:plc:xyz123/post/3kabc"


class TestBuildTitle:
    def test_display_name_prefix_and_collapsed_whitespace(self):
        post = make_post(author_display_name="LeBron James", text="  Lakers\n\nwin   again\ttonight  ")

        assert build_title(post) == "LeBron James: Lakers win again tonight"

    def test_falls_back_to_handle_without_display_name(self):
        post = make_post(author_display_name="", author_handle="lebron.bsky.social", text="Lakers win")

        assert build_title(post) == "lebron.bsky.social: Lakers win"

    def test_title_of_281_code_points_is_cut_to_280_ending_in_ellipsis(self):
        prefix = "Author Name: "
        text = "é" * (TITLE_LIMIT + 1 - len(prefix))
        post = make_post(text=text)

        title = build_title(post)

        assert len(title) == TITLE_LIMIT
        assert title.endswith("…")
        assert title == (prefix + text)[: TITLE_LIMIT - 1] + "…"

    def test_title_of_exactly_280_code_points_is_unchanged(self):
        prefix = "Author Name: "
        text = "x" * (TITLE_LIMIT - len(prefix))
        post = make_post(text=text)

        title = build_title(post)

        assert title == prefix + text
        assert len(title) == TITLE_LIMIT
        assert "…" not in title

    def test_textless_post_uses_on_bluesky_title(self):
        assert build_title(make_post(text="")) == "Author Name on Bluesky"
        assert build_title(make_post(text="  \n\t ")) == "Author Name on Bluesky"

    def test_textless_post_falls_back_to_handle(self):
        post = make_post(text="", author_display_name="", author_handle="lebron.bsky.social")

        assert build_title(post) == "lebron.bsky.social on Bluesky"


SENTINEL_EMBED = {"$type": "sentinel"}


class RecordingCovesClient:
    def __init__(self):
        self.calls = []

    def create_external_embed(self, **kwargs):
        self.calls.append(kwargs)
        return SENTINEL_EMBED


class TestBuildEmbed:
    def test_passes_expected_kwargs_and_returns_client_result(self):
        post = make_post(
            "3kabc",
            author_display_name="LeBron James",
            author_handle="lebron.bsky.social",
            text="Lakers\n win   again",
            embed_type=None,
        )
        client = RecordingCovesClient()

        result = build_embed(post, client)

        assert result is SENTINEL_EMBED
        permalink = "https://bsky.app/profile/lebron.bsky.social/post/3kabc"
        assert client.calls == [
            {
                "uri": permalink,
                "title": "LeBron James (@lebron.bsky.social) on Bluesky",
                "description": "Lakers win again",
                "sources": [{"uri": permalink, "title": "Bluesky", "domain": "bsky.app"}],
                "embed_type": "website",
                "provider": "bluesky",
                "domain": "bsky.app",
            }
        ]

    def test_title_falls_back_to_handle_and_permalink_uses_did_for_invalid_handle(self):
        post = make_post("3kabc", author_display_name="", author_handle="handle.invalid", author_did="did:plc:xyz")
        client = RecordingCovesClient()

        build_embed(post, client)

        call = client.calls[0]
        assert call["title"] == "handle.invalid (@handle.invalid) on Bluesky"
        assert call["uri"] == "https://bsky.app/profile/did:plc:xyz/post/3kabc"
        assert call["sources"][0]["uri"] == call["uri"]

    def test_description_is_cut_to_300_characters(self):
        client = RecordingCovesClient()

        build_embed(make_post(text="d" * 400), client)

        assert len(client.calls[0]["description"]) == 300

    @pytest.mark.parametrize(
        "post_embed_type, expected",
        [
            ("app.bsky.embed.video#view", "video"),
            ("app.bsky.embed.images#view", "image"),
            ("app.bsky.embed.external#view", "website"),
            ("app.bsky.embed.record#view", "website"),
            (None, "website"),
        ],
    )
    def test_embed_type_mapping(self, post_embed_type, expected):
        client = RecordingCovesClient()

        build_embed(make_post(embed_type=post_embed_type), client)

        assert client.calls[0]["embed_type"] == expected

    def test_through_real_coves_client(self):
        client = CovesClient(api_url="https://coves.test", api_key=VALID_API_KEY)
        post = make_post(
            "3kabc",
            author_display_name="LeBron James",
            author_handle="lebron.bsky.social",
            text="Lakers win again",
            embed_type="app.bsky.embed.images#view",
        )

        embed = build_embed(post, client)

        permalink = "https://bsky.app/profile/lebron.bsky.social/post/3kabc"
        assert embed["$type"] == "social.coves.embed.external"
        external = embed["external"]
        assert external["uri"] == permalink
        assert external["title"] == "LeBron James (@lebron.bsky.social) on Bluesky"
        assert external["description"] == "Lakers win again"
        assert external["provider"] == "bluesky"
        assert external["domain"] == "bsky.app"
        assert external["embedType"] == "image"
        assert external["sources"] == [{"uri": permalink, "title": "Bluesky", "domain": "bsky.app"}]


class TestPermalinkKey:
    def test_https_handle_form(self):
        assert posting.permalink_key("https://bsky.app/profile/author.bsky.social/post/3kabc") == (
            "author.bsky.social",
            "3kabc",
        )

    def test_http_scheme_and_did_form(self):
        assert posting.permalink_key("http://bsky.app/profile/did:plc:author/post/3kabc") == (
            "did:plc:author",
            "3kabc",
        )

    def test_host_is_case_insensitive(self):
        assert posting.permalink_key("https://BSKY.App/profile/author.bsky.social/post/3kabc") == (
            "author.bsky.social",
            "3kabc",
        )

    def test_trailing_slash_query_and_fragment_are_ignored(self):
        expected = ("author.bsky.social", "3kabc")
        assert posting.permalink_key("https://bsky.app/profile/author.bsky.social/post/3kabc/") == expected
        assert posting.permalink_key("https://bsky.app/profile/author.bsky.social/post/3kabc?ref=x") == expected
        assert posting.permalink_key("https://bsky.app/profile/author.bsky.social/post/3kabc#top") == expected
        assert posting.permalink_key("https://bsky.app/profile/author.bsky.social/post/3kabc/?a=1#f") == expected

    @pytest.mark.parametrize(
        "url",
        [
            "https://bsky.app/profile/author.bsky.social/post/3kabc/extra",
            "https://bsky.app/profile/author.bsky.social/post",
            "https://bsky.app/profile/author.bsky.social",
            "https://bsky.app/profile/author.bsky.social/likes/3kabc",
            "https://bsky.app/",
            "https://bsky.app/profile//post/3kabc",
            "https://bsky.app/profile/author.bsky.social/post/",
            "https://example.test/profile/author.bsky.social/post/3kabc",
            "https://staging.bsky.app/profile/author.bsky.social/post/3kabc",
            "https://bsky.social/profile/author.bsky.social/post/3kabc",
            "https://notbsky.app/profile/author.bsky.social/post/3kabc",
            "ftp://bsky.app/profile/author.bsky.social/post/3kabc",
            "bsky.app/profile/author.bsky.social/post/3kabc",
            "",
            "not a url",
        ],
    )
    def test_other_hosts_paths_and_schemes_return_none(self, url):
        assert posting.permalink_key(url) is None

    def test_segment_keeps_case_of_handle(self):
        assert posting.permalink_key("https://bsky.app/profile/Author.bsky.social/post/3kabc") == (
            "Author.bsky.social",
            "3kabc",
        )


class TestAlreadyPostedUris:
    def test_did_form_link_matches(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/did:plc:author/post/3kabc"})

        assert result == {post.uri}

    def test_handle_form_link_matches(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/author.bsky.social/post/3kabc"})

        assert result == {post.uri}

    def test_rkey_must_match(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/author.bsky.social/post/3kxyz"})

        assert result == set()

    def test_other_author_with_same_rkey_does_not_match(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/someone.else/post/3kabc"})

        assert result == set()

    def test_handle_invalid_never_matches_by_handle(self):
        post = make_post("3kabc", author_handle="handle.invalid")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/handle.invalid/post/3kabc"})

        assert result == set()

    def test_handle_invalid_still_matches_by_did(self):
        post = make_post("3kabc", author_handle="handle.invalid")

        result = posting.already_posted_uris([post], {"https://bsky.app/profile/did:plc:author/post/3kabc"})

        assert result == {post.uri}

    def test_at_uri_equal_to_a_candidate_uri_matches(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {post.uri})

        assert result == {post.uri}

    def test_at_uri_for_a_different_rkey_does_not_match(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris([post], {"at://did:plc:author/app.bsky.feed.post/3kxyz"})

        assert result == set()

    def test_non_bsky_links_are_ignored(self):
        post = make_post("3kabc")

        result = posting.already_posted_uris(
            [post],
            {
                "https://example.test/profile/author.bsky.social/post/3kabc",
                "https://example.test/article",
                "not a url",
            },
        )

        assert result == set()

    def test_no_matches_returns_empty_set(self):
        result = posting.already_posted_uris(
            [make_post("a"), make_post("b")],
            {"https://bsky.app/profile/author.bsky.social/post/c"},
        )

        assert result == set()

    def test_empty_inputs_return_empty_set(self):
        assert posting.already_posted_uris([], set()) == set()
        assert posting.already_posted_uris([make_post("a")], set()) == set()

    def test_multiple_candidates_matched_by_mixed_forms(self):
        first = make_post("a")
        second = make_post("b", author_did="did:plc:other", author_handle="other.bsky.social",
                           uri="at://did:plc:other/app.bsky.feed.post/b")
        third = make_post("c")

        result = posting.already_posted_uris(
            [first, second, third],
            {
                "https://bsky.app/profile/author.bsky.social/post/a",
                "https://BSKY.app/profile/did:plc:other/post/b/",
            },
        )

        assert result == {first.uri, second.uri}
