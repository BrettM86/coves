"""
Configuration loader for the Bluesky Topics aggregator.

Loads and validates configuration from a YAML file.
"""
import logging
import os
import re
from pathlib import Path
from typing import Any, Dict, List, Tuple
from urllib.parse import urlparse

import yaml

from src.models import AggregatorConfig, LogLevel, TopicConfig

logger = logging.getLogger(__name__)

TOPIC_NAME_PATTERN = re.compile(r"^[a-z0-9_-]+$")
AT_URI_PATTERN = re.compile(r"^at://([^/]+)/([^/]+)/([^/]+)$")
FEED_COLLECTION = "app.bsky.feed.generator"
LIST_COLLECTION = "app.bsky.graph.list"

DEFAULT_COVES_API_URL = "https://coves.social"
DEFAULT_BLUESKY_API_URL = "https://public.api.bsky.app"
DEFAULT_WINDOW_HOURS = 24
DEFAULT_MAX_POSTS_PER_RUN = 3
DEFAULT_CLASSIFY_CANDIDATES = 50
DEFAULT_MIN_CONFIDENCE = 0.7
DEFAULT_CLASSIFIER_MODEL = "openai/gpt-5.6-luna"

WINDOW_HOURS_RANGE = (1, 48)
MAX_POSTS_PER_RUN_RANGE = (1, 20)
CLASSIFY_CANDIDATES_RANGE = (1, 200)


class ConfigError(Exception):
    """Configuration error."""


class ConfigLoader:
    """
    Loads and validates aggregator configuration.

    Supports loading from a YAML file, COVES_API_URL / BLUESKY_API_URL
    environment overrides, and validation of every field.
    """

    def __init__(self, config_path: Path):
        self.config_path = Path(config_path)

    def load(self) -> AggregatorConfig:
        """
        Load and validate configuration.

        Raises:
            ConfigError: If the config file is missing, unparseable, or invalid.
        """
        if not self.config_path.exists():
            raise ConfigError(f"Configuration file not found: {self.config_path}")

        try:
            with open(self.config_path, "r") as config_file:
                config_data = yaml.safe_load(config_file)
        except yaml.YAMLError as error:
            raise ConfigError(f"Failed to parse YAML: {error}")

        if not config_data:
            raise ConfigError("Configuration file is empty")
        if not isinstance(config_data, dict):
            raise ConfigError("Configuration root must be a mapping")

        return self._parse_config(config_data)

    def _parse_config(self, data: Dict[str, Any]) -> AggregatorConfig:
        coves_api_url = os.getenv("COVES_API_URL") or data.get("coves_api_url", DEFAULT_COVES_API_URL)
        self._require_http_url("coves_api_url", coves_api_url)

        bluesky_api_url = os.getenv("BLUESKY_API_URL") or data.get("bluesky_api_url", DEFAULT_BLUESKY_API_URL)
        self._require_http_url("bluesky_api_url", bluesky_api_url)

        window_hours = self._require_int_in_range(
            "window_hours", data.get("window_hours", DEFAULT_WINDOW_HOURS), WINDOW_HOURS_RANGE
        )
        max_posts_per_run = self._require_int_in_range(
            "max_posts_per_run", data.get("max_posts_per_run", DEFAULT_MAX_POSTS_PER_RUN), MAX_POSTS_PER_RUN_RANGE
        )
        classify_candidates = self._require_int_in_range(
            "classify_candidates",
            data.get("classify_candidates", DEFAULT_CLASSIFY_CANDIDATES),
            CLASSIFY_CANDIDATES_RANGE,
        )

        min_confidence = data.get("min_confidence", DEFAULT_MIN_CONFIDENCE)
        if isinstance(min_confidence, bool) or not isinstance(min_confidence, (int, float)):
            raise ConfigError(f"min_confidence must be a number, got: {min_confidence!r}")
        if not 0 <= min_confidence <= 1:
            raise ConfigError(f"min_confidence must be between 0 and 1, got: {min_confidence}")
        min_confidence = float(min_confidence)

        classifier_model = self._require_non_empty_string(
            "classifier_model", data.get("classifier_model", DEFAULT_CLASSIFIER_MODEL)
        )

        log_level_value = str(data.get("log_level", "info")).lower()
        try:
            log_level = LogLevel(log_level_value)
        except ValueError:
            valid_levels = [level.value for level in LogLevel]
            raise ConfigError(f"Invalid log_level '{log_level_value}'. Valid values: {valid_levels}")

        topics_data = data.get("topics")
        if not topics_data:
            raise ConfigError("Configuration must include at least one topic")
        if not isinstance(topics_data, list):
            raise ConfigError("topics must be a list")

        topics: List[TopicConfig] = []
        seen_names = set()
        for topic_data in topics_data:
            topic = self._parse_topic(topic_data, window_hours, max_posts_per_run)
            if topic.name in seen_names:
                raise ConfigError(f"Duplicate topic name: {topic.name}")
            seen_names.add(topic.name)
            topics.append(topic)

        enabled_count = sum(1 for topic in topics if topic.enabled)
        logger.info(f"Loaded configuration with {len(topics)} topics ({enabled_count} enabled)")

        return AggregatorConfig(
            coves_api_url=coves_api_url,
            bluesky_api_url=bluesky_api_url,
            topics=tuple(topics),
            window_hours=window_hours,
            max_posts_per_run=max_posts_per_run,
            classify_candidates=classify_candidates,
            min_confidence=min_confidence,
            classifier_model=classifier_model,
            log_level=log_level,
        )

    def _parse_topic(
        self, data: Any, default_window_hours: int, default_max_posts_per_run: int
    ) -> TopicConfig:
        if not isinstance(data, dict):
            raise ConfigError(f"Each topic must be a mapping, got: {data!r}")

        for field in ("name", "community_handle", "description"):
            if field not in data:
                raise ConfigError(f"Missing required field in topic config: {field}")

        name = self._require_non_empty_string("name", data["name"])
        if not TOPIC_NAME_PATTERN.match(name):
            raise ConfigError(
                f"Invalid topic name '{name}': must match {TOPIC_NAME_PATTERN.pattern}"
            )

        community_handle = self._require_non_empty_string(
            f"community_handle for topic '{name}'", data["community_handle"]
        )
        description = self._require_non_empty_string(
            f"description for topic '{name}'", data["description"]
        )

        feeds = self._parse_at_uris(f"feeds for topic '{name}'", data.get("feeds", []), FEED_COLLECTION)
        lists = self._parse_at_uris(f"lists for topic '{name}'", data.get("lists", []), LIST_COLLECTION)
        if not feeds and not lists:
            raise ConfigError(f"Topic '{name}' must have at least one feed or list")

        excluded_handles_data = data.get("excluded_handles", [])
        if not isinstance(excluded_handles_data, list):
            raise ConfigError(f"excluded_handles for topic '{name}' must be a list")
        excluded_handles = tuple(
            self._require_non_empty_string(f"excluded_handles entry for topic '{name}'", handle)
            for handle in excluded_handles_data
        )

        min_likes = data.get("min_likes", 0)
        if not self._is_int(min_likes) or min_likes < 0:
            raise ConfigError(f"min_likes for topic '{name}' must be a non-negative integer, got: {min_likes!r}")

        window_hours = self._require_int_in_range(
            f"window_hours for topic '{name}'",
            data.get("window_hours", default_window_hours),
            WINDOW_HOURS_RANGE,
        )
        max_posts_per_run = self._require_int_in_range(
            f"max_posts_per_run for topic '{name}'",
            data.get("max_posts_per_run", default_max_posts_per_run),
            MAX_POSTS_PER_RUN_RANGE,
        )

        enabled = data.get("enabled", True)
        if not isinstance(enabled, bool):
            raise ConfigError(f"enabled for topic '{name}' must be a boolean, got: {enabled!r}")

        return TopicConfig(
            name=name,
            community_handle=community_handle,
            description=description,
            feeds=feeds,
            lists=lists,
            window_hours=window_hours,
            max_posts_per_run=max_posts_per_run,
            excluded_handles=excluded_handles,
            min_likes=min_likes,
            enabled=enabled,
        )

    def _parse_at_uris(self, field: str, values: Any, expected_collection: str) -> Tuple[str, ...]:
        if values is None:
            return ()
        if not isinstance(values, list):
            raise ConfigError(f"{field} must be a list")

        uris = []
        for value in values:
            uri = self._require_non_empty_string(field, value)
            match = AT_URI_PATTERN.match(uri)
            if not match:
                raise ConfigError(
                    f"Invalid at-URI in {field}: '{uri}' (expected at://<did>/{expected_collection}/<rkey>)"
                )
            collection = match.group(2)
            if collection != expected_collection:
                raise ConfigError(
                    f"Wrong collection in {field}: '{uri}' has '{collection}', expected '{expected_collection}'"
                )
            uris.append(uri)
        return tuple(uris)

    @staticmethod
    def _is_int(value: Any) -> bool:
        return isinstance(value, int) and not isinstance(value, bool)

    def _require_int_in_range(self, field: str, value: Any, bounds: Tuple[int, int]) -> int:
        low, high = bounds
        if not self._is_int(value):
            raise ConfigError(f"{field} must be an integer, got: {value!r}")
        if not low <= value <= high:
            raise ConfigError(f"{field} must be between {low} and {high}, got: {value}")
        return value

    @staticmethod
    def _require_non_empty_string(field: str, value: Any) -> str:
        if not isinstance(value, str) or not value.strip():
            raise ConfigError(f"{field} must be a non-empty string, got: {value!r}")
        return value.strip()

    @staticmethod
    def _require_http_url(field: str, url: Any) -> None:
        if not isinstance(url, str) or not url.strip():
            raise ConfigError(f"Missing required field: {field}")
        try:
            result = urlparse(url)
        except ValueError as error:
            raise ConfigError(f"Invalid URL for {field}: {url} ({error})")
        if result.scheme not in ("http", "https") or not result.netloc:
            raise ConfigError(f"Invalid URL for {field}: {url} (must be http(s) with a host)")
