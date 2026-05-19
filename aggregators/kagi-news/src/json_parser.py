"""
Kagi News JSON cluster parser.

Converts a cluster object from the Kagi News JSON feed (e.g.
`https://news.kagi.com/tech.json`) into a KagiStory.
"""
import logging
import re
from datetime import datetime
from typing import List, Optional

from src.models import KagiStory, Perspective, Quote, Source

logger = logging.getLogger(__name__)


class KagiJSONParser:
    """Parses Kagi News JSON clusters into structured KagiStory objects."""

    def parse_to_story(
        self,
        title: str,
        link: str,
        guid: str,
        pub_date: datetime,
        categories: List[str],
        json_cluster: dict,
    ) -> KagiStory:
        """
        Convert a JSON cluster (plus the XML-side metadata it joins to) into a KagiStory.

        Args:
            title: Story title (from RSS entry)
            link: kite.kagi.com permalink (from RSS entry)
            guid: Stable identifier (from RSS entry)
            pub_date: Publication date (from RSS entry)
            categories: Category tags (from RSS entry)
            json_cluster: The matching cluster object from the JSON feed

        Returns:
            Fully populated KagiStory
        """
        return KagiStory(
            title=title,
            link=link,
            guid=guid,
            pub_date=pub_date,
            categories=categories,
            summary=self._extract_summary(json_cluster),
            highlights=self._extract_highlights(json_cluster),
            perspectives=self._extract_perspectives(json_cluster),
            quote=self._extract_quote(json_cluster),
            sources=self._extract_sources(json_cluster),
            image_url=self._extract_image_url(json_cluster),
            image_alt=self._extract_image_alt(json_cluster),
        )

    def _expect_list(self, cluster: dict, field: str) -> list:
        """Get a list field, raise on type mismatch. Missing field returns []."""
        value = cluster.get(field)
        if value is None:
            return []
        if not isinstance(value, list):
            raise TypeError(
                f"Kagi JSON type drift: expected list for {field!r}, got {type(value).__name__}"
            )
        return value

    def _expect_str(self, cluster: dict, field: str) -> str:
        """Get a string field, raise on type mismatch. Missing field returns ''."""
        value = cluster.get(field)
        if value is None:
            return ""
        if not isinstance(value, str):
            raise TypeError(
                f"Kagi JSON type drift: expected str for {field!r}, got {type(value).__name__}"
            )
        return value

    def _extract_summary(self, cluster: dict) -> str:
        return self._expect_str(cluster, "short_summary")

    def _extract_image_url(self, cluster: dict) -> Optional[str]:
        image = cluster.get("primary_image")
        if image is None:
            return None
        if not isinstance(image, dict):
            logger.warning(
                f"Kagi JSON type drift: expected dict for 'primary_image', got {type(image).__name__}; dropping image"
            )
            return None
        return image.get("url") or None

    def _extract_image_alt(self, cluster: dict) -> Optional[str]:
        image = cluster.get("primary_image")
        if image is None:
            return None
        if not isinstance(image, dict):
            logger.warning(
                f"Kagi JSON type drift: expected dict for 'primary_image', got {type(image).__name__}; dropping image"
            )
            return None
        return image.get("caption") or None

    def _extract_highlights(self, cluster: dict) -> List[str]:
        # JSON's `talking_points` is a list of bullet-point strings summarizing
        # the story.
        raw = self._expect_list(cluster, "talking_points")
        highlights: List[str] = []
        for item in raw:
            if not isinstance(item, str):
                logger.warning(
                    f"Kagi JSON type drift: expected str in 'talking_points', got {type(item).__name__}; skipping"
                )
                continue
            highlights.append(item)
        return highlights

    def _extract_quote(self, cluster: dict) -> Optional[Quote]:
        text = self._expect_str(cluster, "quote").strip()
        if not text:
            return None

        # Prefer the speaker (`quote_author`) for attribution; fall back to
        # the publication (`quote_attribution`) when the speaker is unknown.
        author = self._expect_str(cluster, "quote_author")
        publication = self._expect_str(cluster, "quote_attribution")
        attribution = (author or publication).strip()

        return Quote(text=text, attribution=attribution)

    def _extract_perspectives(self, cluster: dict) -> List[Perspective]:
        results = []
        for p in self._expect_list(cluster, "perspectives"):
            text = self._expect_str(p, "text").strip()
            if not text:
                continue

            # Kagi's JSON `text` typically starts with the actor and a colon
            # (e.g., "Apple: Apple said..."). Split on the first colon followed
            # by whitespace so URLs in the text don't trigger a false split.
            parts = re.split(r":\s+", text, maxsplit=1)
            if len(parts) == 2:
                actor = parts[0].strip()
                description = parts[1].strip()
            else:
                logger.warning(
                    f"Perspective text has no actor prefix (no colon+space): {text[:80]!r}"
                )
                actor = ""
                description = text

            sources = self._expect_list(p, "sources")
            first_source = sources[0] if sources else {}
            results.append(
                Perspective(
                    actor=actor,
                    description=description,
                    source_url=first_source.get("url", "") or "",
                    source_name=first_source.get("name", "") or "",
                )
            )
        return results

    def _extract_sources(self, cluster: dict) -> List[Source]:
        results = []
        for i, a in enumerate(self._expect_list(cluster, "articles")):
            url = a.get("link") or ""
            title = a.get("title") or ""
            domain = a.get("domain") or ""
            if not url:
                raise ValueError(
                    f"Kagi article at index {i} is missing 'link'; per-domain citation "
                    f"indexing would be misaligned"
                )
            if not title:
                raise ValueError(
                    f"Kagi article at index {i} is missing 'title'; per-domain citation "
                    f"indexing would be misaligned"
                )
            if not domain:
                raise ValueError(
                    f"Kagi article at index {i} is missing 'domain'; per-domain citation "
                    f"indexing would be misaligned"
                )
            results.append(Source(title=title, url=url, domain=domain))
        return results
