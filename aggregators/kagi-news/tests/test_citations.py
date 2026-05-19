"""
Tests for `src.citations` — inline `[domain#N]` citation marker handling.

The mapping invariant being locked here: N is **1-indexed within the per-domain
bucket** of `KagiStory.sources`, which preserves the order of
`cluster["articles"]`.
"""
import logging

import pytest

from src.citations import build_index, strip, tokenize, CITE_RE
from src.models import Source


def _source(domain: str, n: int) -> Source:
    """Make a synthetic Source with a domain-and-position-derived URL/title."""
    return Source(
        title=f"{domain} #{n}",
        url=f"https://{domain}/article-{n}",
        domain=domain,
    )


class TestCiteRegex:
    """Lock the marker syntax. Tests below depend on this regex matching exactly
    what Kagi emits and nothing more."""

    def test_basic_match(self):
        assert CITE_RE.findall("see [reuters.com#1] now") == [("reuters.com", "1")]

    def test_back_to_back(self):
        assert CITE_RE.findall("[a.com#1][b.com#2]") == [
            ("a.com", "1"),
            ("b.com", "2"),
        ]

    def test_does_not_match_unmatched_brackets(self):
        assert CITE_RE.findall("[just brackets] and [no#hash#here]") == []
        assert CITE_RE.findall("[no-hash]") == []


class TestBuildIndex:
    def test_single_article_per_domain(self):
        sources = [_source("reuters.com", 1), _source("apnews.com", 1)]
        index = build_index(sources)
        assert index[("reuters.com", 1)] == "https://reuters.com/article-1"
        assert index[("apnews.com", 1)] == "https://apnews.com/article-1"

    def test_duplicate_domain_increments_position(self):
        """The N-th article from a domain is 1-indexed within that domain bucket."""
        sources = [
            _source("nyt.com", 1),
            _source("reuters.com", 1),
            _source("nyt.com", 2),
            _source("nyt.com", 3),
        ]
        index = build_index(sources)
        assert index[("nyt.com", 1)] == "https://nyt.com/article-1"
        assert index[("nyt.com", 2)] == "https://nyt.com/article-2"
        assert index[("nyt.com", 3)] == "https://nyt.com/article-3"
        assert index[("reuters.com", 1)] == "https://reuters.com/article-1"
        # No spurious entries
        assert ("nyt.com", 4) not in index
        assert ("reuters.com", 2) not in index

    def test_empty_sources(self):
        assert build_index([]) == {}

    def test_trusts_parser_invariant_no_empty_url_or_domain(self):
        """`KagiJSONParser._extract_sources` raises if `link`/`title`/`domain` are
        missing, so `build_index` trusts that every Source has both populated.
        This test locks the new contract: well-formed input -> 1:1 indexing,
        no defensive skipping."""
        sources = [
            Source(title="a", url="https://ok.com/x", domain="ok.com"),
            Source(title="b", url="https://two.com/y", domain="two.com"),
        ]
        index = build_index(sources)
        assert index == {
            ("ok.com", 1): "https://ok.com/x",
            ("two.com", 1): "https://two.com/y",
        }

    def test_domain_is_lowercased_in_index(self):
        """Sources with mixed-case domains are normalized so marker lookups —
        which lowercase the matched domain — resolve regardless of casing."""
        sources = [Source(title="t", url="https://Reuters.COM/x", domain="Reuters.COM")]
        index = build_index(sources)
        assert index == {("reuters.com", 1): "https://Reuters.COM/x"}


class TestStrip:
    def test_strips_consecutive_markers_before_punctuation(self):
        """The verified-sample case from the spec."""
        text = (
            "stalled [straitstimes.com#1][alarabiya.net#1][thehill.com#1]. "
            "Iranian state-linked media said something."
        )
        index = build_index(
            [
                _source("straitstimes.com", 1),
                _source("alarabiya.net", 1),
                _source("thehill.com", 1),
            ]
        )
        assert (
            strip(text, index)
            == "stalled. Iranian state-linked media said something."
        )

    def test_strips_marker_at_start(self):
        index = build_index([_source("reuters.com", 1)])
        assert strip("[reuters.com#1] said yesterday.", index) == "said yesterday."

    def test_strips_marker_at_end(self):
        index = build_index([_source("reuters.com", 1)])
        assert strip("said yesterday [reuters.com#1]", index) == "said yesterday"

    def test_strips_marker_alone_in_paragraph(self):
        index = build_index([_source("reuters.com", 1)])
        assert strip("[reuters.com#1]", index) == ""

    def test_strips_back_to_back_markers(self):
        index = build_index([_source("a.com", 1), _source("b.com", 1)])
        # No surrounding text -> empty after collapse
        assert strip("[a.com#1][b.com#1]", index) == ""

    def test_collapses_double_spaces_from_removal(self):
        index = build_index([_source("a.com", 1)])
        assert strip("before [a.com#1] after", index) == "before after"

    def test_preserves_multi_byte_characters(self):
        index = build_index([_source("nhk.jp", 1)])
        text = "東京で [nhk.jp#1] と報じた。"
        assert strip(text, index) == "東京で と報じた。"

    def test_returns_empty_for_empty_input(self):
        assert strip("", build_index([])) == ""

    def test_unresolved_token_still_stripped_and_warns(self, caplog):
        # Domain present but position OOB
        index = build_index([_source("known.com", 1)])
        with caplog.at_level(logging.WARNING, logger="src.citations"):
            out = strip("text [known.com#5] more", index, context="cluster-X")
        assert out == "text more"
        warning_messages = [r.message for r in caplog.records if r.levelno == logging.WARNING]
        assert any("[known.com#5]" in m for m in warning_messages)
        assert any("cluster-X" in m for m in warning_messages)

    def test_unresolved_unknown_domain_warns(self, caplog):
        index = build_index([_source("known.com", 1)])
        with caplog.at_level(logging.WARNING, logger="src.citations"):
            out = strip("text [unknown.com#1] more", index)
        assert out == "text more"
        assert any("[unknown.com#1]" in r.message for r in caplog.records)


class TestTokenize:
    def test_plain_text_only(self):
        index = build_index([_source("a.com", 1)])
        spans = tokenize("hello world", index)
        assert spans == [("text", "hello world")]

    def test_no_tokens_no_index(self):
        spans = tokenize("hello world", {})
        assert spans == [("text", "hello world")]

    def test_all_tokens_no_text(self):
        index = build_index([_source("a.com", 1), _source("b.com", 1)])
        spans = tokenize("[a.com#1][b.com#1]", index)
        assert spans == [
            ("link", ("[a.com]", "https://a.com/article-1")),
            ("link", ("[b.com]", "https://b.com/article-1")),
        ]

    def test_mixed_text_and_tokens(self):
        index = build_index([_source("reuters.com", 1)])
        spans = tokenize("Hello [reuters.com#1] world", index)
        assert spans == [
            ("text", "Hello "),
            ("link", ("[reuters.com]", "https://reuters.com/article-1")),
            ("text", " world"),
        ]

    def test_token_at_byte_position_zero(self):
        index = build_index([_source("reuters.com", 1)])
        spans = tokenize("[reuters.com#1] after", index)
        assert spans == [
            ("link", ("[reuters.com]", "https://reuters.com/article-1")),
            ("text", " after"),
        ]

    def test_token_at_end(self):
        index = build_index([_source("reuters.com", 1)])
        spans = tokenize("before [reuters.com#1]", index)
        assert spans == [
            ("text", "before "),
            ("link", ("[reuters.com]", "https://reuters.com/article-1")),
        ]

    def test_back_to_back_tokens(self):
        index = build_index([_source("a.com", 1), _source("b.com", 1)])
        spans = tokenize("x[a.com#1][b.com#1]y", index)
        assert spans == [
            ("text", "x"),
            ("link", ("[a.com]", "https://a.com/article-1")),
            ("link", ("[b.com]", "https://b.com/article-1")),
            ("text", "y"),
        ]

    def test_unresolved_token_dropped_and_warns(self, caplog):
        index = build_index([_source("known.com", 1)])
        with caplog.at_level(logging.WARNING, logger="src.citations"):
            spans = tokenize(
                "before [unknown.com#1] after", index, context="cluster-XYZ"
            )
        assert spans == [("text", "before "), ("text", " after")]
        warning_messages = [r.message for r in caplog.records if r.levelno == logging.WARNING]
        assert any(
            "[unknown.com#1]" in m and "cluster-XYZ" in m for m in warning_messages
        )

    def test_unresolved_position_oob_dropped_and_warns(self, caplog):
        index = build_index([_source("known.com", 1)])
        with caplog.at_level(logging.WARNING, logger="src.citations"):
            spans = tokenize("text [known.com#9] more", index)
        assert spans == [("text", "text "), ("text", " more")]
        assert any("[known.com#9]" in r.message for r in caplog.records)

    def test_empty_input(self):
        assert tokenize("", {}) == []

    def test_preserves_multi_byte_text_around_tokens(self):
        """`tokenize` works on string characters; UTF-8 byte conversion happens
        downstream in the builder. We just need the visible text to be intact."""
        index = build_index([_source("nhk.jp", 1)])
        spans = tokenize("東京で [nhk.jp#1] と報じた", index)
        assert spans == [
            ("text", "東京で "),
            ("link", ("[nhk.jp]", "https://nhk.jp/article-1")),
            ("text", " と報じた"),
        ]


class TestDomainCaseInsensitivity:
    """Lock the invariant that marker-domain casing doesn't matter for resolution.
    Kagi has been observed to emit lower-case domains, but we don't want a future
    casing change to silently break every citation link."""

    def test_uppercase_marker_resolves_against_lowercase_index(self):
        index = build_index([_source("reuters.com", 1)])
        # Uppercase domain in marker -> same article.
        spans = tokenize("see [REUTERS.COM#1] now", index)
        assert spans == [
            ("text", "see "),
            ("link", ("[reuters.com]", "https://reuters.com/article-1")),
            ("text", " now"),
        ]

    def test_uppercase_marker_strips_without_warning(self, caplog):
        index = build_index([_source("reuters.com", 1)])
        with caplog.at_level(logging.WARNING, logger="src.citations"):
            out = strip("see [REUTERS.COM#1] now", index)
        assert out == "see now"
        # The marker resolved; no warning should fire.
        assert not [r for r in caplog.records if r.levelno == logging.WARNING]


class TestNonLinkMarkers:
    """`[common]` is Kagi's "commonly reported" metadata marker — it has no
    article to link to and must be dropped from both plain-text and rich-text
    sinks. Observed in production data adjacent to `[domain#N]` citations."""

    def test_strip_removes_common_marker_adjacent_to_citation(self):
        index = build_index([_source("space.com", 1)])
        result = strip("mission [space.com#1][common]. Space.com reported", index)
        assert result == "mission. Space.com reported"

    def test_strip_removes_standalone_common_marker(self):
        result = strip("plain text [common] in the middle", {})
        assert result == "plain text in the middle"

    def test_strip_removes_common_before_punctuation(self):
        result = strip("end of sentence [common].", {})
        assert result == "end of sentence."

    def test_tokenize_drops_common_marker_silently(self):
        """`[common]` is dropped before tokenization, so no spans are emitted
        for it and no warning fires (unlike unresolved `[domain#N]` markers)."""
        index = build_index([_source("space.com", 1)])
        spans = tokenize("mission [space.com#1][common] continues", index)
        assert spans == [
            ("text", "mission "),
            ("link", ("[space.com]", "https://space.com/article-1")),
            ("text", " continues"),
        ]

    def test_tokenize_standalone_common_emits_only_surrounding_text(self):
        spans = tokenize("before [common] after", {})
        assert spans == [("text", "before  after")]
