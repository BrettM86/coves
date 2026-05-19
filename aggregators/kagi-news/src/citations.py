"""
Inline citation marker handling for Kagi News content.

Kagi's structured JSON fields (`short_summary`, `talking_points`, `perspectives[].text`,
`quote`) carry inline citation markers in the form `[domain.com#N]`. The integer N
is **1-indexed within the per-domain bucket of `cluster["articles"]`** — i.e.
`[reuters.com#2]` resolves to the second article whose `domain == "reuters.com"`
in the order they appear in the cluster's `articles` list. This invariant was
verified across all 4 production feeds with 621/621 tokens matching.

This module isolates marker handling so the JSON parser stays a pure mapping
layer and the rich-text formatter can render markers as inline link facets
without re-implementing the resolver per sink.
"""
from __future__ import annotations

import logging
import re
from typing import Dict, List, Tuple, Union

from src.models import Source

logger = logging.getLogger(__name__)


CITE_RE = re.compile(r'\[([^\[\]#]+)#(\d+)\]')

# Kagi also emits non-link metadata markers (e.g. `[common]`, observed adjacent
# to citations to mean "commonly reported across sources"). These have no
# article to link to — drop them from output entirely. Add new variants here
# as they're discovered in production data.
NON_LINK_MARKERS_RE = re.compile(r'\[common\]')


CitationIndex = Dict[Tuple[str, int], str]
TokenizedSpan = Tuple[str, Union[str, Tuple[str, str]]]


def build_index(sources: List[Source]) -> CitationIndex:
    """
    Build a lookup of `(domain, 1-indexed-position-within-domain)` -> article URL.

    `KagiStory.sources` preserves the order and content of `cluster["articles"]`
    (see `KagiJSONParser._extract_sources`), so we can reconstruct the same
    per-domain bucket the marker syntax assumes.
    """
    per_domain_count: Dict[str, int] = {}
    index: CitationIndex = {}
    for source in sources:
        # `KagiJSONParser._extract_sources` guarantees domain and url are non-empty;
        # we lowercase the domain so lookups can normalize the marker's domain too
        # without depending on Kagi's casing.
        domain = source.domain.lower()
        per_domain_count[domain] = per_domain_count.get(domain, 0) + 1
        index[(domain, per_domain_count[domain])] = source.url
    return index


def _warn_unresolved(token_text: str, context: str) -> None:
    logger.warning(
        f"Unresolved Kagi citation marker {token_text!r}"
        + (f" (context: {context})" if context else "")
    )


def strip(text: str, index: CitationIndex, context: str = "") -> str:
    """
    Remove all `[domain#N]` markers from `text`, suitable for plain-text sinks
    (e.g. the dev post's embed.description).

    Cleans up the artifacts of removal:
    - Whitespace immediately before sentence/clause punctuation (`. , ; : ! ?`)
      that was introduced by stripping the preceding marker is collapsed so
      `"stalled [a#1][b#1]."` becomes `"stalled."` not `"stalled ."`.
    - Runs of internal whitespace are collapsed to a single space.
    - Leading/trailing whitespace is trimmed.

    Unresolved markers (unknown domain, or N exceeding the per-domain bucket)
    are also stripped, but a warning is emitted once per occurrence so the
    silent loss is detectable in logs.
    """
    if not text:
        return text

    def _replace(match: re.Match) -> str:
        domain = match.group(1).lower()
        position = int(match.group(2))
        if (domain, position) not in index:
            _warn_unresolved(match.group(0), context)
        return ""

    stripped = CITE_RE.sub(_replace, text)
    stripped = NON_LINK_MARKERS_RE.sub("", stripped)
    stripped = re.sub(r'\s+([.,;:!?])', r'\1', stripped)
    stripped = re.sub(r'\s+', ' ', stripped)
    return stripped.strip()


def tokenize(text: str, index: CitationIndex, context: str = "") -> List[TokenizedSpan]:
    """
    Walk `text` and emit a sequence of spans suitable for a rich-text builder:

    - `("text", "<plain span>")` for non-marker text between (or around) tokens.
    - `("link", ("<visible text>", "<uri>"))` for each resolved marker. Visible
      text is the bracket with the domain but WITHOUT the `#N` — so
      `[reuters.com#1]` renders as `[reuters.com]` linked to the resolved URL.

    Unresolved markers are dropped from the output entirely and a warning is
    emitted (matches `strip`'s behavior). No empty `("text", "")` spans are
    emitted.
    """
    if not text:
        return []

    text = NON_LINK_MARKERS_RE.sub("", text)

    spans: List[TokenizedSpan] = []
    cursor = 0
    for match in CITE_RE.finditer(text):
        if match.start() > cursor:
            spans.append(("text", text[cursor:match.start()]))

        domain = match.group(1).lower()
        position = int(match.group(2))
        url = index.get((domain, position))
        if url is None:
            _warn_unresolved(match.group(0), context)
        else:
            spans.append(("link", (f"[{domain}]", url)))

        cursor = match.end()

    if cursor < len(text):
        spans.append(("text", text[cursor:]))

    return spans
