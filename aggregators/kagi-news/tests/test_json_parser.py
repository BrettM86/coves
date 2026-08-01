"""
Tests for the Kagi News JSON cluster parser.
"""
import json
import logging
from datetime import datetime
from pathlib import Path

import pytest

from src.json_parser import KagiJSONParser


FIXTURE_PATH = Path(__file__).parent / "fixtures" / "sample_kagi_cluster.json"


@pytest.fixture
def real_cluster():
    """One real Kagi cluster captured from tech.json."""
    with FIXTURE_PATH.open() as f:
        return json.load(f)


@pytest.fixture
def parser():
    return KagiJSONParser()


@pytest.fixture
def rss_meta():
    """Stand-in for the XML side of a join — the metadata the parser receives."""
    return {
        "title": "Apple adds encrypted iPhone-Android RCS in iOS 26.5",
        "link": "https://kite.kagi.com/abc-uuid/tech/0",
        "guid": "https://kite.kagi.com/abc-uuid/tech/0",
        "pub_date": datetime(2026, 5, 11, 18, 0, 0),
        "categories": ["Technology", "Technology/Messaging"],
    }


class TestKagiJSONParser:
    def test_parses_real_cluster_end_to_end(self, parser, real_cluster, rss_meta):
        story = parser.parse_to_story(json_cluster=real_cluster, **rss_meta)

        assert story.title == rss_meta["title"]
        assert story.link == rss_meta["link"]
        assert story.guid == rss_meta["guid"]
        assert story.categories == rss_meta["categories"]
        # Summary populated and not the literal fixture title
        assert story.summary.startswith("Apple released iOS 26.5")
        # Image came from primary_image
        assert story.image_url.startswith("https://kagiproxy.com/img/")
        assert story.image_alt and "encrypted RCS" in story.image_alt
        # Highlights come from talking_points (non-empty list of strings)
        assert len(story.highlights) >= 1
        assert all(isinstance(h, str) for h in story.highlights)
        # Sources come from articles (each has title/url/domain)
        assert len(story.sources) >= 1
        first_src = story.sources[0]
        assert first_src.url.startswith("http")
        assert first_src.title
        assert first_src.domain
        # Real-cluster fixture happens to have empty quote
        assert story.quote is None
        # Perspectives parsed with actor split on first colon
        assert len(story.perspectives) >= 1
        first_persp = story.perspectives[0]
        assert first_persp.actor  # actor recovered from "Apple: ..." prefix
        assert first_persp.description  # description is the remainder
        assert first_persp.source_url.startswith("http")
        assert first_persp.source_name

    def test_quote_extracted_when_present(self, parser, rss_meta):
        cluster = {
            "quote": "We will not back down.",
            "quote_author": "Jane Doe",
            "quote_attribution": "Reuters",
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.quote is not None
        assert story.quote.text == "We will not back down."
        # Speaker (quote_author) wins over publication (quote_attribution)
        assert story.quote.attribution == "Jane Doe"

    def test_quote_falls_back_to_attribution_when_no_author(self, parser, rss_meta):
        cluster = {
            "quote": "We will not back down.",
            "quote_author": "",
            "quote_attribution": "Reuters",
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.quote.attribution == "Reuters"

    def test_empty_quote_yields_none(self, parser, rss_meta):
        cluster = {"quote": "", "quote_author": "Someone", "quote_attribution": "X"}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.quote is None

    def test_missing_primary_image_yields_none(self, parser, rss_meta):
        cluster = {"short_summary": "ok"}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.image_url is None
        assert story.image_alt is None

    def test_primary_image_present_but_url_empty(self, parser, rss_meta):
        cluster = {"primary_image": {"url": "", "caption": "ignored"}}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.image_url is None

    def test_empty_talking_points(self, parser, rss_meta):
        cluster = {"talking_points": []}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.highlights == []

    def test_missing_talking_points(self, parser, rss_meta):
        cluster = {}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.highlights == []

    def test_perspective_with_no_sources(self, parser, rss_meta):
        cluster = {
            "perspectives": [{"text": "Analyst: things are fine.", "sources": []}]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert len(story.perspectives) == 1
        p = story.perspectives[0]
        assert p.actor == "Analyst"
        assert p.description == "things are fine."
        assert p.source_url == ""
        assert p.source_name == ""

    def test_perspective_without_colon_keeps_full_text(self, parser, rss_meta):
        cluster = {
            "perspectives": [
                {
                    "text": "Plain observation without a leading actor",
                    "sources": [{"name": "X", "url": "https://x.example"}],
                }
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        p = story.perspectives[0]
        assert p.actor == ""
        assert p.description == "Plain observation without a leading actor"
        assert p.source_url == "https://x.example"
        assert p.source_name == "X"

    def test_perspective_empty_text_skipped(self, parser, rss_meta):
        cluster = {
            "perspectives": [
                {"text": "", "sources": []},
                {"text": "Real: keep me", "sources": []},
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert len(story.perspectives) == 1
        assert story.perspectives[0].actor == "Real"

    def test_article_without_link_raises(self, parser, rss_meta):
        """A missing link/title/domain would misalign per-domain citation indexing;
        raise instead of silently dropping the article."""
        cluster = {
            "articles": [
                {"title": "no link", "link": "", "domain": "x.com"},
            ]
        }
        with pytest.raises(ValueError, match="link"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_article_without_title_raises(self, parser, rss_meta):
        cluster = {
            "articles": [
                {"title": "", "link": "https://y.example", "domain": "y.com"},
            ]
        }
        with pytest.raises(ValueError, match="title"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_article_without_domain_is_kept_in_place(self, parser, rss_meta):
        """
        Kagi leaves `domain` empty when its own URL parse fails (observed only on
        bluewin.ch, whose links arrive malformed). Rejecting the article cost the
        entire post; keeping it is what preserves citation alignment, because
        build_index counts per domain and the "" bucket is unreachable from any
        `[domain#N]` marker.
        """
        cluster = {
            "articles": [
                {"title": "a", "link": "https://reuters.com/1", "domain": "reuters.com"},
                {"title": "b", "link": "https://z.example/2", "domain": ""},
                {"title": "c", "link": "https://reuters.com/3", "domain": "reuters.com"},
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)

        assert [s.domain for s in story.sources] == ["reuters.com", "", "reuters.com"]
        assert [s.title for s in story.sources] == ["a", "b", "c"]

    def test_empty_domain_does_not_shift_other_domains_numbering(self, parser, rss_meta):
        """The whole point: an unlabelled article must not renumber a real outlet."""
        from src.citations import build_index

        cluster = {
            "articles": [
                {"title": "a", "link": "https://reuters.com/1", "domain": "reuters.com"},
                {"title": "b", "link": "https://z.example/2", "domain": ""},
                {"title": "c", "link": "https://reuters.com/3", "domain": "reuters.com"},
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        index = build_index(story.sources)

        # [reuters.com#2] still resolves to the *second* reuters article, not the third.
        assert index[("reuters.com", 1)] == "https://reuters.com/1"
        assert index[("reuters.com", 2)] == "https://reuters.com/3"

    def test_article_still_raises_without_link_or_title(self, parser, rss_meta):
        """Tolerating an empty domain must not weaken the structural guards."""
        with pytest.raises(ValueError, match="link"):
            parser.parse_to_story(
                json_cluster={"articles": [{"title": "ok", "link": "", "domain": "z.com"}]},
                **rss_meta,
            )
        with pytest.raises(ValueError, match="title"):
            parser.parse_to_story(
                json_cluster={"articles": [{"title": "", "link": "https://z.com", "domain": "z.com"}]},
                **rss_meta,
            )

    def test_malformed_scheme_separator_is_repaired(self, parser, rss_meta):
        """
        Kagi emits `https:/host/path` with a single slash. That parses as an opaque
        URI, survives uri_sanitizer untouched, and publishes as an unclickable
        string, so it is repaired at the Kagi boundary.
        """
        cluster = {
            "articles": [
                {"title": "a", "link": "https:/www.bluewin.ch/fr/infos/x", "domain": ""},
                {"title": "b", "link": "http:/example.com/y", "domain": "example.com"},
                {"title": "c", "link": "https://intact.com/z", "domain": "intact.com"},
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)

        assert [s.url for s in story.sources] == [
            "https://www.bluewin.ch/fr/infos/x",
            "http://example.com/y",
            "https://intact.com/z",
        ]

    def test_repair_does_not_touch_paths_containing_the_pattern(self, parser, rss_meta):
        """Only the leading scheme is repaired -- never a `https:/` inside a query."""
        cluster = {
            "articles": [
                {
                    "title": "a",
                    "link": "https://example.com/r?to=https:/other.com/p",
                    "domain": "example.com",
                },
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.sources[0].url == "https://example.com/r?to=https:/other.com/p"

    def test_all_fields_absent_produces_empty_story(self, parser, rss_meta):
        story = parser.parse_to_story(json_cluster={}, **rss_meta)
        assert story.summary == ""
        assert story.highlights == []
        assert story.perspectives == []
        assert story.sources == []
        assert story.quote is None
        assert story.image_url is None
        assert story.image_alt is None

    def test_talking_points_string_instead_of_list_raises(self, parser, rss_meta):
        """Top-level type drift on `talking_points` should raise loudly so main.py's
        per-entry try/except and the zero-resolved escalator fail fast."""
        cluster = {"talking_points": "oops not a list"}
        with pytest.raises(TypeError, match="talking_points"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_perspectives_dict_instead_of_list_raises(self, parser, rss_meta):
        cluster = {"perspectives": {"text": "Apple: not in a list"}}
        with pytest.raises(TypeError, match="perspectives"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_null_perspective_entry_is_skipped_not_fatal(self, parser, rss_meta, caplog):
        """
        Kagi emits `perspectives: [null, {...}]` on some clusters (10 of 48 live
        clusters on 2026-08-01). That raised AttributeError out of the parser and
        cost the entire story; one missing viewpoint must not do that.
        """
        import logging

        cluster = {
            "perspectives": [
                None,
                {"text": "Apple: Apple said it will appeal."},
            ]
        }
        with caplog.at_level(logging.WARNING):
            story = parser.parse_to_story(json_cluster=cluster, **rss_meta)

        assert [p.actor for p in story.perspectives] == ["Apple"]
        assert [p.description for p in story.perspectives] == ["Apple said it will appeal."]
        assert any(
            "expected dict in 'perspectives'" in r.message for r in caplog.records
        )

    def test_articles_string_instead_of_list_raises(self, parser, rss_meta):
        cluster = {"articles": "not-a-list"}
        with pytest.raises(TypeError, match="articles"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_short_summary_number_instead_of_string_raises(self, parser, rss_meta):
        cluster = {"short_summary": 42}
        with pytest.raises(TypeError, match="short_summary"):
            parser.parse_to_story(json_cluster=cluster, **rss_meta)

    def test_talking_points_with_non_string_element_warns_and_skips(self, parser, rss_meta, caplog):
        """Non-string elements inside talking_points are skipped with a warning; strings kept."""
        cluster = {"talking_points": ["valid bullet", 123, "another valid"]}
        with caplog.at_level(logging.WARNING, logger="src.json_parser"):
            story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.highlights == ["valid bullet", "another valid"]
        assert any(
            "type drift" in r.message and "talking_points" in r.message and "str" in r.message
            for r in caplog.records
        )


class TestJsonParserPreservesCitationMarkers:
    """The parser is the pure JSON->dataclass mapping layer. It must NOT
    interpret or strip `[domain#N]` citation markers — that's the formatter's
    job. This lock keeps the separation of concerns visible in tests."""

    def test_summary_preserves_markers_verbatim(self, parser, rss_meta):
        raw = (
            "Tensions stalled [straitstimes.com#1][alarabiya.net#1]. "
            "More text follows."
        )
        cluster = {"short_summary": raw}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.summary == raw

    def test_highlights_preserve_markers_verbatim(self, parser, rss_meta):
        raw_bullets = [
            "First bullet [reuters.com#1] with marker.",
            "Second bullet [apnews.com#2][nyt.com#1] with multiple markers.",
        ]
        cluster = {"talking_points": list(raw_bullets)}
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.highlights == raw_bullets

    def test_perspectives_preserve_markers_verbatim(self, parser, rss_meta):
        cluster = {
            "perspectives": [
                {
                    "text": "Apple: claimed [apple.com#1] the feature is rolling out.",
                    "sources": [{"name": "Apple", "url": "https://apple.com/x"}],
                }
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert len(story.perspectives) == 1
        assert (
            story.perspectives[0].description
            == "claimed [apple.com#1] the feature is rolling out."
        )

    def test_quote_preserves_markers_verbatim(self, parser, rss_meta):
        cluster = {
            "quote": "We will respond [reuters.com#1] today",
            "quote_author": "Spokesperson",
            "quote_attribution": "",
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.quote is not None
        assert story.quote.text == "We will respond [reuters.com#1] today"


class TestPerspectiveColonSplit:
    """Lock the colon-split heuristic against URL-in-text false positives."""

    def test_url_leading_text_is_not_split_on_url_colon(self, parser, rss_meta):
        cluster = {
            "perspectives": [
                {
                    "text": "https://example.com/x said it loudly",
                    "sources": [{"name": "Example", "url": "https://example.com/x"}],
                }
            ]
        }
        story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        p = story.perspectives[0]
        # Critical: actor must NOT be "https" — that's the bug the fix prevents.
        assert p.actor == ""
        assert p.description == "https://example.com/x said it loudly"


class TestPrimaryImageTypeDrift:
    """`primary_image` that isn't a dict (e.g. False, []) should warn and drop
    the image rather than masking the type mismatch."""

    def test_primary_image_false_warns_and_drops(self, parser, rss_meta, caplog):
        cluster = {"primary_image": False}
        with caplog.at_level(logging.WARNING, logger="src.json_parser"):
            story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.image_url is None
        assert story.image_alt is None
        assert any("primary_image" in r.message for r in caplog.records)

    def test_primary_image_list_warns_and_drops(self, parser, rss_meta, caplog):
        cluster = {"primary_image": []}
        with caplog.at_level(logging.WARNING, logger="src.json_parser"):
            story = parser.parse_to_story(json_cluster=cluster, **rss_meta)
        assert story.image_url is None
        assert story.image_alt is None
        assert any("primary_image" in r.message for r in caplog.records)


class TestRealFixtureCitationResolution:
    """End-to-end safety net: every `[domain#N]` marker emitted by Kagi in the
    real captured fixture must resolve against the `build_index(story.sources)`
    derived from the same cluster. If this fails, citation indexing is broken
    and rich-text links will point to the wrong sources (or nowhere)."""

    def test_real_fixture_every_citation_marker_resolves(self, parser, real_cluster, rss_meta):
        from src.citations import CITE_RE, build_index

        story = parser.parse_to_story(json_cluster=real_cluster, **rss_meta)
        index = build_index(story.sources)

        texts = [story.summary, *story.highlights]
        texts.extend(p.description for p in story.perspectives)
        if story.quote is not None:
            texts.append(story.quote.text)

        unresolved = []
        total_markers = 0
        for text in texts:
            for match in CITE_RE.finditer(text):
                total_markers += 1
                key = (match.group(1).lower(), int(match.group(2)))
                if key not in index:
                    unresolved.append(match.group(0))

        # Production fixtures should have many markers — assert we actually walked them.
        assert total_markers > 0, "fixture has no citation markers; safety net not exercised"
        assert not unresolved, (
            f"{len(unresolved)}/{total_markers} citation markers did not resolve "
            f"against build_index(story.sources): {unresolved[:5]}"
        )
