"""
Tests for candidates module: filtering, dedupe, and ranking of Bluesky posts.
"""
from datetime import datetime, timedelta, timezone

import pytest

from src.candidates import filter_candidates, rank_candidates, score
from src.models import BlueskyPost, TopicConfig

NOW = datetime(2026, 9, 18, 12, 0, 0, tzinfo=timezone.utc)
WINDOW_HOURS = 6
EXCLUDED_HANDLE = "surfrecommends.bsky.social"

TOPIC = TopicConfig(
    name="nba",
    community_handle="nba.coves.social",
    description="Posts about the NBA.",
    feeds=("at://did:plc:feedowner/app.bsky.feed.generator/nba-essential",),
    lists=(),
    window_hours=WINDOW_HOURS,
    max_posts_per_run=3,
    excluded_handles=(EXCLUDED_HANDLE,),
    min_likes=5,
)


def make_post(rkey="p1", age=timedelta(hours=1), like_count=10, **overrides) -> BlueskyPost:
    fields = {
        "uri": f"at://did:plc:author/app.bsky.feed.post/{rkey}",
        "cid": f"bafy{rkey}",
        "author_did": "did:plc:author",
        "author_handle": "author.bsky.social",
        "text": f"post {rkey}",
        "created_at": NOW - age,
        "like_count": like_count,
    }
    fields.update(overrides)
    return BlueskyPost(**fields)


def run_filter(posts):
    return filter_candidates(
        posts,
        topic=TOPIC,
        now=NOW,
        window_hours=WINDOW_HOURS,
    )


def uris(posts):
    return [post.uri for post in posts]


class TestFilterCandidates:
    def test_keeps_eligible_post_unchanged(self):
        post = make_post()

        assert run_filter([post]) == [post]

    def test_drops_replies(self):
        assert run_filter([make_post(is_reply=True)]) == []

    def test_drops_posts_older_than_window(self):
        old = make_post("old", age=timedelta(hours=WINDOW_HOURS, minutes=1))
        recent = make_post("recent", age=timedelta(hours=1))

        assert uris(run_filter([old, recent])) == [recent.uri]

    def test_post_exactly_at_window_edge_is_kept(self):
        # Window is inclusive: created_at == now - window_hours survives.
        edge = make_post("edge", age=timedelta(hours=WINDOW_HOURS))

        assert run_filter([edge]) == [edge]

    def test_drops_posts_more_than_five_minutes_in_future(self):
        four_ahead = make_post("four", age=-timedelta(minutes=4))
        six_ahead = make_post("six", age=-timedelta(minutes=6))

        assert uris(run_filter([six_ahead, four_ahead])) == [four_ahead.uri]

    @pytest.mark.parametrize(
        "label", ["porn", "sexual", "nudity", "graphic-media", "!hide", "!warn"]
    )
    def test_drops_posts_with_adult_or_hidden_labels(self, label):
        assert run_filter([make_post(labels=("sports", label))]) == []

    def test_keeps_posts_with_harmless_labels(self):
        post = make_post(labels=("sports",))

        assert run_filter([post]) == [post]

    def test_drops_authors_opted_out_of_unauthenticated_view(self):
        assert run_filter([make_post(author_labels=("!no-unauthenticated",))]) == []

    def test_drops_excluded_handles(self):
        assert run_filter([make_post(author_handle=EXCLUDED_HANDLE)]) == []

    def test_drops_below_min_likes_and_keeps_equal(self):
        below = make_post("below", like_count=TOPIC.min_likes - 1)
        equal = make_post("equal", like_count=TOPIC.min_likes)

        assert uris(run_filter([below, equal])) == [equal.uri]

    def test_filter_never_drops_by_uri_so_already_posted_posts_stay_candidates(self):
        # Already-posted posts must survive filtering so their story can block
        # other posts at selection time; the filter has no already_posted input.
        posted = make_post("posted")
        fresh = make_post("fresh")

        assert uris(run_filter([posted, fresh])) == [posted.uri, fresh.uri]

    def test_filter_rejects_the_removed_already_posted_keyword(self):
        with pytest.raises(TypeError):
            filter_candidates(
                [make_post()], topic=TOPIC, now=NOW, window_hours=WINDOW_HOURS, already_posted=set()
            )

    def test_dedupes_same_uri_keeping_higher_like_count(self):
        lower = make_post("dup", like_count=10)
        higher = make_post("dup", like_count=25)

        result = run_filter([lower, higher])

        assert result == [higher]

    def test_empty_input_returns_empty_list(self):
        assert run_filter([]) == []


class TestScore:
    def test_score_is_likes_plus_reposts_plus_quotes(self):
        post = make_post(like_count=7, repost_count=3, quote_count=5)

        assert score(post) == 15

    def test_score_counts_zero_when_only_quotes_are_absent(self):
        # quote_count defaults to 0, so existing fixtures score as likes + reposts.
        assert score(make_post(like_count=7, repost_count=3)) == 10

    def test_score_of_quotes_only_post_is_its_quote_count(self):
        assert score(make_post(like_count=0, repost_count=0, quote_count=4)) == 4


class TestRankCandidates:
    def test_post_with_quotes_outranks_more_likes_without_quotes(self):
        # likes 10 + quotes 15 = 25 beats likes 20 + 0 quotes; a likes-plus-reposts
        # score would rank them the other way round.
        quoted = make_post("quoted", like_count=10, quote_count=15)
        liked = make_post("liked", like_count=20)

        assert uris(rank_candidates([liked, quoted], limit=10)) == [quoted.uri, liked.uri]

    def test_sorts_by_likes_plus_reposts_plus_quotes_descending(self):
        low = make_post("low", like_count=10, repost_count=0, quote_count=0)
        middle = make_post("middle", like_count=10, repost_count=2, quote_count=3)
        high = make_post("high", like_count=12, repost_count=0, quote_count=4)

        assert uris(rank_candidates([middle, low, high], limit=10)) == [
            high.uri,
            middle.uri,
            low.uri,
        ]

    def test_sorts_by_likes_plus_reposts_descending(self):
        low = make_post("low", like_count=10, repost_count=0)
        middle = make_post("middle", like_count=10, repost_count=5)
        high = make_post("high", like_count=20, repost_count=1)

        assert uris(rank_candidates([middle, low, high], limit=10)) == [
            high.uri,
            middle.uri,
            low.uri,
        ]

    def test_ties_break_newer_first(self):
        older = make_post("older", age=timedelta(hours=3), like_count=10)
        newer = make_post("newer", age=timedelta(hours=1), like_count=10)

        assert uris(rank_candidates([older, newer], limit=10)) == [newer.uri, older.uri]

    def test_truncates_to_limit(self):
        posts = [make_post(f"p{index}", like_count=index) for index in range(6)]

        result = rank_candidates(posts, limit=2)

        assert uris(result) == [posts[5].uri, posts[4].uri]

    def test_limit_larger_than_input_returns_all(self):
        posts = [make_post("a", like_count=1), make_post("b", like_count=2)]

        assert len(rank_candidates(posts, limit=50)) == 2

    def test_input_order_does_not_matter(self):
        posts = [make_post(f"p{index}", like_count=index, age=timedelta(minutes=index)) for index in range(5)]
        scrambled = [posts[3], posts[0], posts[4], posts[1], posts[2]]

        assert rank_candidates(scrambled, limit=5) == rank_candidates(posts, limit=5)
        assert uris(rank_candidates(scrambled, limit=5)) == uris(list(reversed(posts)))
