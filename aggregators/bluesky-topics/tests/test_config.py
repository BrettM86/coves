"""
Tests for config module.
"""
import copy

import pytest
import yaml

from src.config import ConfigLoader, ConfigError
from src.models import AggregatorConfig, TopicConfig

FEED_URI = "at://did:plc:ygvuw3tg5gsy73wpggfz3oj4/app.bsky.feed.generator/nba-essential"
LIST_URI = "at://did:plc:ygvuw3tg5gsy73wpggfz3oj4/app.bsky.graph.list/3kabc"

MINIMAL_CONFIG = {
    "coves_api_url": "https://coves.test",
    "bluesky_api_url": "https://bsky.test",
    "topics": [
        {
            "name": "nba",
            "community_handle": "nba.coves.social",
            "description": "Posts about the NBA.",
            "feeds": [FEED_URI],
        }
    ],
}


def write_config(tmp_path, data) -> ConfigLoader:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(yaml.safe_dump(data))
    return ConfigLoader(config_path)


def with_top_level(**overrides):
    data = copy.deepcopy(MINIMAL_CONFIG)
    data.update(overrides)
    return data


def with_topic(**overrides):
    data = copy.deepcopy(MINIMAL_CONFIG)
    data["topics"][0].update(overrides)
    return data


class TestConfigLoaderLoad:
    """Tests for successful loading."""

    def test_loads_minimal_config_with_defaults(self, tmp_path, monkeypatch):
        monkeypatch.delenv("COVES_API_URL", raising=False)
        monkeypatch.delenv("BLUESKY_API_URL", raising=False)
        config = write_config(tmp_path, MINIMAL_CONFIG).load()

        assert isinstance(config, AggregatorConfig)
        assert config.coves_api_url == "https://coves.test"
        assert config.bluesky_api_url == "https://bsky.test"
        assert config.window_hours == 24
        assert config.max_posts_per_run == 3
        assert config.classify_candidates == 50
        assert config.min_confidence == 0.7
        assert config.classifier_model == "openai/gpt-5.6-luna"
        assert getattr(config.log_level, "value", config.log_level) == "info"

        assert len(config.topics) == 1
        topic = config.topics[0]
        assert isinstance(topic, TopicConfig)
        assert topic.name == "nba"
        assert topic.community_handle == "nba.coves.social"
        assert topic.description == "Posts about the NBA."
        assert list(topic.feeds) == [FEED_URI]
        assert list(topic.lists) == []
        assert list(topic.excluded_handles) == []
        assert topic.min_likes == 0
        assert topic.enabled is True

    def test_aggregator_config_does_not_carry_its_own_defaults(self):
        """Defaults live only in src.config; the dataclass requires every field."""
        with pytest.raises(TypeError):
            AggregatorConfig(
                coves_api_url="https://coves.test",
                bluesky_api_url="https://bsky.test",
                topics=(),
            )

    def test_topic_inherits_top_level_defaults(self, tmp_path):
        data = with_top_level(window_hours=12, max_posts_per_run=4)
        topic = write_config(tmp_path, data).load().topics[0]

        assert topic.window_hours == 12
        assert topic.max_posts_per_run == 4

    def test_topic_overrides_top_level_defaults(self, tmp_path):
        data = with_top_level(window_hours=12, max_posts_per_run=4)
        data["topics"][0].update(
            {
                "window_hours": 3,
                "max_posts_per_run": 1,
                "lists": [LIST_URI],
                "excluded_handles": ["surfrecommends.bsky.social"],
                "min_likes": 5,
                "enabled": False,
            }
        )
        config = write_config(tmp_path, data).load()

        assert config.window_hours == 12
        assert config.max_posts_per_run == 4
        topic = config.topics[0]
        assert topic.window_hours == 3
        assert topic.max_posts_per_run == 1
        assert list(topic.lists) == [LIST_URI]
        assert list(topic.excluded_handles) == ["surfrecommends.bsky.social"]
        assert topic.min_likes == 5
        assert topic.enabled is False

    def test_loads_top_level_overrides(self, tmp_path):
        data = with_top_level(
            classify_candidates=25,
            min_confidence=0.85,
            classifier_model="claude-sonnet-5",
            log_level="debug",
        )
        config = write_config(tmp_path, data).load()

        assert config.classify_candidates == 25
        assert config.min_confidence == 0.85
        assert config.classifier_model == "claude-sonnet-5"
        assert getattr(config.log_level, "value", config.log_level) == "debug"

    def test_lists_only_topic_is_valid(self, tmp_path):
        data = with_topic(feeds=[], lists=[LIST_URI])
        topic = write_config(tmp_path, data).load().topics[0]

        assert list(topic.feeds) == []
        assert list(topic.lists) == [LIST_URI]

    def test_env_overrides_urls(self, tmp_path, monkeypatch):
        monkeypatch.setenv("COVES_API_URL", "https://coves-override.test")
        monkeypatch.setenv("BLUESKY_API_URL", "https://bsky-override.test")
        config = write_config(tmp_path, MINIMAL_CONFIG).load()

        assert config.coves_api_url == "https://coves-override.test"
        assert config.bluesky_api_url == "https://bsky-override.test"

    def test_raises_if_file_not_found(self, tmp_path):
        loader = ConfigLoader(tmp_path / "nonexistent.yaml")

        with pytest.raises(ConfigError):
            loader.load()


class TestConfigLoaderRejections:
    """Each invalid shape raises ConfigError."""

    @pytest.mark.parametrize(
        "data",
        [
            pytest.param(with_topic(name="NBA"), id="name-uppercase"),
            pytest.param(with_topic(name="nba topic"), id="name-space"),
            pytest.param(with_topic(name=""), id="name-empty"),
            pytest.param(with_topic(community_handle="   "), id="community-handle-blank"),
            pytest.param(with_topic(description="   "), id="description-blank"),
            pytest.param(
                with_topic(feeds=["at://did:plc:abc/app.bsky.graph.list/rkey"]),
                id="feed-wrong-collection",
            ),
            pytest.param(
                with_topic(feeds=[], lists=["at://did:plc:abc/app.bsky.feed.generator/rkey"]),
                id="list-wrong-collection",
            ),
            pytest.param(
                with_topic(feeds=["at://did:plc:abc/app.bsky.feed.generator"]),
                id="feed-missing-rkey",
            ),
            pytest.param(
                with_topic(feeds=["https://bsky.app/profile/x/feed/y"]),
                id="feed-not-at-uri",
            ),
            pytest.param(with_topic(feeds=[], lists=[]), id="no-feeds-or-lists"),
            pytest.param(with_topic(feeds=[]), id="no-feeds-and-lists-absent"),
            pytest.param(with_top_level(window_hours=0), id="window-hours-low"),
            pytest.param(with_top_level(window_hours=49), id="window-hours-high"),
            pytest.param(with_topic(window_hours=49), id="topic-window-hours-high"),
            pytest.param(with_top_level(max_posts_per_run=0), id="max-posts-low"),
            pytest.param(with_top_level(max_posts_per_run=21), id="max-posts-high"),
            pytest.param(with_topic(max_posts_per_run=21), id="topic-max-posts-high"),
            pytest.param(with_top_level(classify_candidates=0), id="classify-candidates-low"),
            pytest.param(with_top_level(classify_candidates=201), id="classify-candidates-high"),
            pytest.param(with_top_level(min_confidence=-0.1), id="min-confidence-low"),
            pytest.param(with_top_level(min_confidence=1.1), id="min-confidence-high"),
            pytest.param(with_top_level(max_posts_per_run=True), id="max-posts-bool"),
            pytest.param(with_top_level(window_hours="6"), id="window-hours-string"),
            pytest.param(with_topic(min_likes="3"), id="min-likes-string"),
            pytest.param(with_topic(min_likes=-1), id="min-likes-negative"),
            pytest.param(with_top_level(coves_api_url="ftp://coves.test"), id="coves-url-ftp"),
            pytest.param(with_top_level(coves_api_url="not-a-url"), id="coves-url-garbage"),
            pytest.param(with_top_level(bluesky_api_url="file:///etc/passwd"), id="bluesky-url-file"),
            pytest.param(with_top_level(topics=[]), id="topics-empty"),
        ],
    )
    def test_rejects_invalid_config(self, tmp_path, data):
        loader = write_config(tmp_path, data)

        with pytest.raises(ConfigError):
            loader.load()

    def test_rejects_missing_topics_key(self, tmp_path):
        data = with_top_level()
        del data["topics"]
        loader = write_config(tmp_path, data)

        with pytest.raises(ConfigError):
            loader.load()

    def test_rejects_duplicate_topic_names(self, tmp_path):
        data = with_top_level()
        data["topics"].append(copy.deepcopy(data["topics"][0]))
        data["topics"][1]["community_handle"] = "other.coves.social"
        loader = write_config(tmp_path, data)

        with pytest.raises(ConfigError):
            loader.load()
