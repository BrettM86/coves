"""
Data models for Kagi News aggregator.
"""
from dataclasses import dataclass, field
from datetime import datetime
from typing import List, Optional


@dataclass
class Source:
    """A news source citation."""
    title: str
    url: str
    domain: str


@dataclass
class Perspective:
    """A perspective from a particular actor/stakeholder."""
    actor: str
    description: str
    source_url: str
    source_name: str = ""  # Name of the source (e.g., "The Straits Times")


@dataclass
class Quote:
    """A notable quote from the story."""
    text: str
    attribution: str


@dataclass
class KagiStory:
    """
    Structured representation of a Kagi News story.

    Built from the RSS feed item (metadata + permalink) joined to the matching
    cluster in the Kagi JSON feed (rich pre-structured content).
    """
    # RSS metadata
    title: str
    link: str  # Kagi story permalink
    guid: str
    pub_date: datetime
    categories: List[str] = field(default_factory=list)

    # Parsed from Kagi JSON cluster
    summary: str = ""
    highlights: List[str] = field(default_factory=list)
    perspectives: List[Perspective] = field(default_factory=list)
    quote: Optional[Quote] = None
    sources: List[Source] = field(default_factory=list)
    image_url: Optional[str] = None
    image_alt: Optional[str] = None

    def __post_init__(self):
        """Validate required fields."""
        if not self.title:
            raise ValueError("title is required")
        if not self.link:
            raise ValueError("link is required")
        if not self.guid:
            raise ValueError("guid is required")


@dataclass
class FeedConfig:
    """Configuration for a single RSS feed."""
    name: str
    url: str
    community_handle: str
    enabled: bool = True


@dataclass(frozen=True)
class DedupConfig:
    """Configuration for semantic deduplication."""
    semantic_enabled: bool = True
    similarity_threshold: float = 0.8
    lookback_days: int = 4

    def __post_init__(self):
        if not (0.0 <= self.similarity_threshold <= 1.0):
            raise ValueError(
                f"similarity_threshold must be between 0.0 and 1.0, got {self.similarity_threshold}"
            )
        if self.lookback_days < 1:
            raise ValueError(
                f"lookback_days must be >= 1, got {self.lookback_days}"
            )


@dataclass
class AggregatorConfig:
    """Full aggregator configuration."""
    coves_api_url: str
    feeds: List[FeedConfig]
    log_level: str = "info"
    dedup: DedupConfig = field(default_factory=DedupConfig)
