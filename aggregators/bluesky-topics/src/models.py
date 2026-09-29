"""
Data models for the Bluesky Topics aggregator.
"""
from dataclasses import dataclass
from datetime import datetime
from enum import Enum
from typing import Optional, Tuple


class LogLevel(Enum):
    """Valid log levels for aggregator configuration."""

    DEBUG = "debug"
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"
    CRITICAL = "critical"


@dataclass(frozen=True)
class TopicConfig:
    """
    Configuration for a single topic.

    Maps Bluesky feeds and lists to a Coves community. Immutable (frozen).
    ``window_hours`` and ``max_posts_per_run`` are resolved by the loader
    (per-topic override or top-level default).
    """

    name: str  # State key, ^[a-z0-9_-]+$
    community_handle: str  # Coves community (e.g., "nba.coves.social")
    description: str  # Topic description given to the classifier
    feeds: Tuple[str, ...]  # at://.../app.bsky.feed.generator/... URIs
    lists: Tuple[str, ...]  # at://.../app.bsky.graph.list/... URIs
    window_hours: int
    max_posts_per_run: int
    excluded_handles: Tuple[str, ...] = ()
    min_likes: int = 0
    enabled: bool = True


@dataclass(frozen=True)
class AggregatorConfig:
    """
    Full aggregator configuration loaded from config.yaml. Immutable (frozen).

    Every field is required: the default values live in src.config.
    """

    coves_api_url: str
    bluesky_api_url: str
    topics: Tuple[TopicConfig, ...]
    window_hours: int
    max_posts_per_run: int
    classify_candidates: int
    min_confidence: float
    classifier_model: str
    log_level: LogLevel


@dataclass(frozen=True)
class BlueskyPost:
    """A Bluesky post view reduced to the fields the pipeline uses."""

    uri: str
    cid: str
    author_did: str
    author_handle: str
    text: str
    created_at: datetime
    author_display_name: str = ""
    like_count: int = 0
    repost_count: int = 0
    reply_count: int = 0
    quote_count: int = 0
    is_reply: bool = False
    labels: Tuple[str, ...] = ()
    author_labels: Tuple[str, ...] = ()
    embed_type: Optional[str] = None
