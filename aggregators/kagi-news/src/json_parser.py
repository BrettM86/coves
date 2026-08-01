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


# Kagi emits `https:/host/path` -- one slash -- for a minority of articles.
# A hierarchical scheme without `//` parses as an *opaque* URI, so it survives
# uri_sanitizer untouched and publishes as an unclickable string. These are the
# same records whose `domain` comes back empty, because Kagi's own extractor
# choked on the malformed URL too. Observed on 2026-08-01 across all four live
# feeds: 15 of 1768 articles, every one of them bluewin.ch.
_MALFORMED_SCHEME_RE = re.compile(r'^(https?):/(?!/)', re.IGNORECASE)


def _repair_scheme_separator(url: str) -> str:
    """Restore the `//` in a scheme-relative-looking `https:/host` URL."""
    return _MALFORMED_SCHEME_RE.sub(r'\1://', url, count=1)


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
            # Kagi emits a null entry inside `perspectives` on some clusters
            # (10 of 48 live clusters on 2026-08-01). Skip it rather than let an
            # AttributeError escape and cost the whole story -- one missing
            # viewpoint is not worth losing the post over.
            if not isinstance(p, dict):
                logger.warning(
                    f"Kagi JSON type drift: expected dict in 'perspectives', got "
                    f"{type(p).__name__}; skipping perspective"
                )
                continue
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
            url = _repair_scheme_separator(a.get("link") or "")
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
            # An empty `domain` is carried rather than rejected. citations.build_index
            # buckets *per domain*, so an unknown-domain article increments only the
            # "" bucket and cannot shift the numbering of any real outlet -- alignment
            # is destroyed by *dropping* an article, not by keeping one we can't label.
            # Kagi built its own `[domain#N]` markers from this same empty value, so no
            # marker can point here anyway: CITE_RE requires at least one character
            # before the '#', so the "" bucket is unreachable by construction. The
            # article stays in place, correctly positioned and simply uncitable.
            #
            # Deriving the domain from the URL is deliberately NOT done: Kagi uses the
            # registrable domain, not the URL host. On 2026-08-01, 127 of 1753 articles
            # disagreed (english.elpais.com -> elpais.com, news.sky.com -> sky.com), so
            # a derived value would invent a bucket key the markers never use.
            if not domain:
                logger.debug(
                    "Kagi article at index %d has no 'domain' (%s); keeping it in "
                    "place as an uncitable source",
                    i, url[:60]
                )
            results.append(Source(title=title, url=url, domain=domain))
        return results
