"""
Tests for story_log module: durable per-topic log of posted Bluesky posts.
"""
import json
import logging
import os
from datetime import datetime, timedelta, timezone

import pytest

from src.models import BlueskyPost
from src.story_log import StoryLog

NOW = datetime(2026, 9, 19, 12, 0, 0, tzinfo=timezone.utc)


def make_post(rkey="p1", age=timedelta(0), **overrides) -> BlueskyPost:
    fields = {
        "uri": f"at://did:plc:author/app.bsky.feed.post/{rkey}",
        "cid": f"bafy{rkey}",
        "author_did": "did:plc:author",
        "author_handle": "author.bsky.social",
        "author_display_name": "Author Name",
        "text": f"post {rkey}",
        "created_at": NOW - age,
        "like_count": 12,
        "repost_count": 3,
        "quote_count": 1,
        "labels": ("porn",),
        "embed_type": "app.bsky.embed.images#view",
    }
    fields.update(overrides)
    return BlueskyPost(**fields)


class TestRecord:
    def test_creates_file_and_parent_directory_on_first_record(self, tmp_path):
        path = tmp_path / "state" / "story_log.json"

        StoryLog(path).record("nba", make_post(), NOW)

        assert path.exists()

    def test_stored_entry_fields(self, tmp_path):
        path = tmp_path / "story_log.json"

        StoryLog(path).record("nba", make_post("p1", age=timedelta(hours=2)), NOW)

        data = json.loads(path.read_text())
        entries = data["nba"]
        assert len(entries) == 1
        entry = entries[0]
        assert entry["uri"] == "at://did:plc:author/app.bsky.feed.post/p1"
        assert entry["author_did"] == "did:plc:author"
        assert entry["author_handle"] == "author.bsky.social"
        assert entry["author_display_name"] == "Author Name"
        assert entry["text"] == "post p1"
        assert datetime.fromisoformat(entry["created_at"]) == NOW - timedelta(hours=2)

    def test_same_uri_recorded_twice_is_stored_once(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")

        log.record("nba", make_post("p1"), NOW)
        log.record("nba", make_post("p1"), NOW)

        assert len(log.recent("nba", NOW - timedelta(days=1))) == 1

    def test_prunes_entries_older_than_48_hours_relative_to_now(self, tmp_path):
        path = tmp_path / "story_log.json"
        log = StoryLog(path)
        log.record("nba", make_post("old", age=timedelta(hours=10)), NOW)
        log.record("nba", make_post("edge", age=timedelta(hours=48)), NOW)

        later = NOW + timedelta(hours=40)
        log.record("nba", make_post("new"), later)

        uris = [entry["uri"] for entry in json.loads(path.read_text())["nba"]]
        assert uris == ["at://did:plc:author/app.bsky.feed.post/new"]

    def test_entry_exactly_48_hours_old_is_kept(self, tmp_path):
        path = tmp_path / "story_log.json"

        StoryLog(path).record("nba", make_post("edge", age=timedelta(hours=48)), NOW)

        assert len(json.loads(path.read_text())["nba"]) == 1

    def test_entries_are_kept_per_topic(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")

        log.record("nba", make_post("basketball"), NOW)
        log.record("nhl", make_post("hockey"), NOW)

        since = NOW - timedelta(hours=1)
        assert [post.uri for post in log.recent("nba", since)] == ["at://did:plc:author/app.bsky.feed.post/basketball"]
        assert [post.uri for post in log.recent("nhl", since)] == ["at://did:plc:author/app.bsky.feed.post/hockey"]

    def test_record_persists_across_instances(self, tmp_path):
        path = tmp_path / "story_log.json"
        StoryLog(path).record("nba", make_post("p1"), NOW)

        assert len(StoryLog(path).recent("nba", NOW - timedelta(hours=1))) == 1

    def test_pruning_one_topic_does_not_drop_the_other(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")
        log.record("nhl", make_post("hockey"), NOW)

        log.record("nba", make_post("basketball"), NOW + timedelta(hours=1))

        assert len(log.recent("nhl", NOW - timedelta(hours=1))) == 1


class TestRecent:
    def test_missing_file_returns_empty_list(self, tmp_path):
        assert StoryLog(tmp_path / "absent.json").recent("nba", NOW - timedelta(days=1)) == []

    def test_unknown_topic_returns_empty_list(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")
        log.record("nba", make_post(), NOW)

        assert log.recent("nhl", NOW - timedelta(days=1)) == []

    def test_returns_entries_at_or_after_since_newest_first(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")
        log.record("nba", make_post("older", age=timedelta(hours=5)), NOW)
        log.record("nba", make_post("edge", age=timedelta(hours=6)), NOW)
        log.record("nba", make_post("too-old", age=timedelta(hours=7)), NOW)
        log.record("nba", make_post("newest", age=timedelta(hours=1)), NOW)

        result = log.recent("nba", NOW - timedelta(hours=6))

        assert [post.uri.rsplit("/", 1)[1] for post in result] == ["newest", "older", "edge"]

    def test_entries_come_back_as_bluesky_posts_with_zero_counts_and_no_embed(self, tmp_path):
        log = StoryLog(tmp_path / "story_log.json")
        log.record("nba", make_post("p1", age=timedelta(hours=2)), NOW)

        [post] = log.recent("nba", NOW - timedelta(hours=3))

        assert isinstance(post, BlueskyPost)
        assert post.uri == "at://did:plc:author/app.bsky.feed.post/p1"
        assert post.author_did == "did:plc:author"
        assert post.author_handle == "author.bsky.social"
        assert post.author_display_name == "Author Name"
        assert post.text == "post p1"
        assert post.created_at == NOW - timedelta(hours=2)
        assert post.created_at.tzinfo is not None
        assert post.like_count == 0
        assert post.repost_count == 0
        assert post.reply_count == 0
        assert post.quote_count == 0
        assert post.labels == ()
        assert post.author_labels == ()
        assert post.embed_type is None
        assert post.is_reply is False

    def test_corrupt_file_returns_empty_list(self, tmp_path):
        path = tmp_path / "story_log.json"
        path.write_text("{corrupt")

        assert StoryLog(path).recent("nba", NOW - timedelta(days=1)) == []

    def test_corrupt_file_is_overwritten_by_next_record(self, tmp_path):
        path = tmp_path / "story_log.json"
        path.write_text("{corrupt")
        log = StoryLog(path)

        log.record("nba", make_post("p1"), NOW)

        data = json.loads(path.read_text())
        assert [entry["uri"] for entry in data["nba"]] == ["at://did:plc:author/app.bsky.feed.post/p1"]
        assert len(log.recent("nba", NOW - timedelta(hours=1))) == 1


VALID_ENTRY = {
    "uri": "at://did:plc:author/app.bsky.feed.post/good",
    "author_did": "did:plc:author",
    "author_handle": "author.bsky.social",
    "author_display_name": "Author Name",
    "text": "a good post",
    "created_at": (NOW - timedelta(hours=1)).isoformat(),
}


def seed_log_with_corrupt_entries(path):
    """Write a story log whose nba topic mixes malformed entries with one valid entry."""
    path.write_text(
        json.dumps(
            {
                "nba": [
                    {"uri": "at://did:plc:author/app.bsky.feed.post/no-fields"},
                    "junk",
                    {
                        "uri": "at://did:plc:author/app.bsky.feed.post/bad-timestamp",
                        "author_did": "did:plc:author",
                        "author_handle": "author.bsky.social",
                        "text": "unparseable timestamp",
                        "created_at": "nope",
                    },
                    {
                        "uri": "at://did:plc:author/app.bsky.feed.post/naive-timestamp",
                        "author_did": "did:plc:author",
                        "author_handle": "author.bsky.social",
                        "text": "naive timestamp",
                        "created_at": "2026-09-19T11:00:00",
                    },
                    {
                        "uri": "at://did:plc:author/app.bsky.feed.post/did-not-a-string",
                        "author_did": 12345,
                        "author_handle": "author.bsky.social",
                        "text": "wrong type",
                        "created_at": (NOW - timedelta(hours=1)).isoformat(),
                    },
                    dict(VALID_ENTRY),
                ]
            }
        )
    )


class TestCorruptEntries:
    def test_recent_returns_only_the_valid_entry(self, tmp_path):
        path = tmp_path / "story_log.json"
        seed_log_with_corrupt_entries(path)

        result = StoryLog(path).recent("nba", NOW - timedelta(days=1))

        assert [post.uri for post in result] == [VALID_ENTRY["uri"]]

    def test_record_succeeds_and_prunes_corrupt_entries(self, tmp_path):
        path = tmp_path / "story_log.json"
        seed_log_with_corrupt_entries(path)
        log = StoryLog(path)

        log.record("nba", make_post("p1"), NOW)

        uris = [entry["uri"] for entry in json.loads(path.read_text())["nba"]]
        assert uris == [VALID_ENTRY["uri"], "at://did:plc:author/app.bsky.feed.post/p1"]

    def test_corrupt_entries_are_warned_about_without_text(self, tmp_path, caplog):
        path = tmp_path / "story_log.json"
        seed_log_with_corrupt_entries(path)

        with caplog.at_level(logging.WARNING):
            StoryLog(path).recent("nba", NOW - timedelta(days=1))

        warnings = [record for record in caplog.records if record.levelno == logging.WARNING]
        assert len(warnings) == 1
        message = warnings[0].getMessage()
        assert "nba" in message
        assert "at://did:plc:author/app.bsky.feed.post/bad-timestamp" in message
        assert "unparseable timestamp" not in message


class TestAtomicWrite:
    def test_failed_replace_leaves_the_existing_log_intact(self, tmp_path, monkeypatch):
        path = tmp_path / "story_log.json"
        log = StoryLog(path)
        log.record("nba", make_post("first"), NOW)
        before = path.read_text()

        def failing_replace(*args, **kwargs):
            raise OSError("replace interrupted")

        monkeypatch.setattr(os, "replace", failing_replace)
        with pytest.raises(OSError):
            log.record("nba", make_post("second"), NOW)

        assert path.read_text() == before
        monkeypatch.undo()
        assert [post.uri for post in log.recent("nba", NOW - timedelta(hours=1))] == [
            "at://did:plc:author/app.bsky.feed.post/first"
        ]

    def test_record_leaves_no_temp_file_behind(self, tmp_path):
        path = tmp_path / "story_log.json"

        StoryLog(path).record("nba", make_post("p1"), NOW)

        assert [child.name for child in tmp_path.iterdir()] == ["story_log.json"]
