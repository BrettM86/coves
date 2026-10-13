"""
Semantic Deduplication for Kagi News Aggregator.

Uses Claude Haiku to detect semantically similar stories within the same
feed/community. Compares new candidate stories against recently posted ones
using a single batched API call per feed.
"""
import copy
import logging
from typing import Any, Dict, List

import anthropic

logger = logging.getLogger(__name__)

# Thinking at high effort needs room before the report_duplicates tool call.
THINKING_ALLOWANCE_TOKENS = 16000
# Response token budget: a fixed allowance plus room for one verdict per candidate.
BASE_RESPONSE_TOKENS = 256
RESPONSE_TOKENS_PER_CANDIDATE = 48


class SemanticDedupUnavailable(Exception):
    """Raised when a batch of candidates was never judged.

    Distinguishes "the model answered and flagged nothing" from "we never got
    an answer", so the caller can defer the candidates instead of posting them.
    """


# Tool schema for structured output from Haiku
REPORT_DUPLICATES_TOOL: Dict[str, Any] = {
    "name": "report_duplicates",
    "description": "Report which new articles are semantic duplicates of recent ones",
    "input_schema": {
        "type": "object",
        "properties": {
            "results": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "new_id": {
                            "type": "string",
                            "description": "The handle of the new candidate article, exactly as shown in the prompt"
                        },
                        "duplicate_of": {
                            "type": "string",
                            "description": "The handle of the recent article this is a duplicate of, exactly as shown in the prompt, or empty string if not a duplicate"
                        },
                        "confidence": {
                            "type": "number",
                            "description": "Confidence score 0.0-1.0 that this is a duplicate"
                        }
                    },
                    "required": ["new_id", "duplicate_of", "confidence"]
                }
            }
        },
        "required": ["results"]
    }
}

SYSTEM_PROMPT = """You are the duplicate filter for a news community. Every RECENT article has already been posted. For each NEW candidate, decide whether a reader who already saw the RECENT posts would see it as the same story again (DUPLICATE) or as a separate piece of news (NEW).

Judge by the candidate's headline news: what it reports that the RECENT article did not.

DUPLICATE when the candidate's headline news is the same incident or the same ongoing storyline, and what it adds is only:
- the same report rewritten, or with a different headline or outlet;
- more detail about the same incident: updated casualty counts, a named victim, the nationality of those affected, extra quotes;
- reactions, commentary, criticism, praise, or statements about something already reported, when they change nothing yet;
- officials repeating, weighing, defending, or hinting at the same position or option, with no decision;
- the measured aftermath or knock-on effect of an incident already reported (outages, traffic, prices, ratings), when nothing new has happened;
- another stunt, claim, or threat by the same actor in a saga already covered, with no outcome.

NEW when the candidate reports a development that changes the situation:
- a decision or outcome: a ruling, vote, signing, launch, arrest, resignation, firing, verdict, death, deal, policy taking effect;
- the event itself after earlier coverage of its anticipation, or the next milestone of an ongoing event;
- a separate incident, even in the same conflict or topic;
- a separate study, report, dataset, or record, even on the same subject;
- a materially changed forecast or status: a new window, a new location, or a new scale.

Examples (illustrative, not from the feed):
- DUPLICATE: RECENT "Storm floods coastal town, two dead" / NEW "Death toll in coastal town flooding rises to five" (same incident, updated count)
- DUPLICATE: RECENT "Minister says sanctions are on the table after embassy attack" / NEW "Minister weighs sanctions after embassy attack that killed a citizen" (same option still being weighed, one added detail)
- DUPLICATE: RECENT "Hacker leaks early footage of unreleased film" / NEW "Same hacker threatens to release full film unless paid" (same actor, same saga, no outcome)
- DUPLICATE: RECENT "Fire shuts down chip plant" / NEW "Chip shipments fall after plant fire" (knock-on effect of the reported incident)
- NEW: RECENT "Government announces tariff on steel" / NEW "Steel tariff takes effect; importers halt orders" (announcement, then implementation)
- NEW: RECENT "Agency forecasts record heat season" / NEW "June sets all-time temperature record" (forecast, then a measured record)
- NEW: RECENT "Senate debates spending bill" / NEW "Senate passes spending bill" (debate, then a vote)
- NEW: RECENT "Earthquake strikes northern region" / NEW "Second quake hits southern coast" (separate incident)

When the candidate is a real development, do not suppress it because it shares a topic, people, or background with a RECENT article.

Always answer by calling the report_duplicates tool exactly once, with one result per NEW candidate."""


class SemanticDeduplicator:
    """
    Detects semantically similar news stories using Claude Haiku.

    Uses a single batched API call per feed to compare all new candidates
    against all recent stories simultaneously.
    """

    def __init__(self, api_key: str, threshold: float = 0.8,
                 model: str = "claude-haiku-5-5"):
        """
        Initialize the semantic deduplicator.

        Args:
            api_key: Anthropic API key
            threshold: Minimum confidence to consider a duplicate (0.0-1.0)
            model: Claude model to use
        """
        self.client = anthropic.Anthropic(api_key=api_key)
        self.threshold = threshold
        self.model = model

    def find_duplicates(
        self,
        new_stories: List[Dict],
        recent_stories: List[Dict],
    ) -> Dict[str, str]:
        """
        Find which new stories are semantic duplicates of recent ones.

        The model never sees story ids. Each call builds short opaque handles
        ([r1]..[rM] for recent stories, [n1]..[nN] for new candidates), shows
        only those in the prompt, constrains the tool schema to them, and
        translates the handles the model returns back into story ids.

        Args:
            new_stories: List of dicts with keys: id, title, summary
            recent_stories: List of dicts with keys: id, title, summary

        Returns:
            Mapping of duplicate new story id -> the recent story id it
            duplicates. Empty when there is nothing to compare and when no
            verdict qualifies as a duplicate.

        Raises:
            SemanticDedupUnavailable: The candidates were never judged -- a
                transient API failure, a response that did not stop normally
                (truncated, refused, paused or out of context window), or a
                response carrying no usable verdict for a non-empty candidate
                list.
        """
        if not new_stories or not recent_stories:
            return {}

        new_handles = self._build_handles(new_stories, "n")
        recent_handles = self._build_handles(recent_stories, "r")

        prompt = self._build_prompt(new_handles, recent_handles)
        tool = self._build_tool(new_handles, recent_handles)

        try:
            response = self.client.messages.create(
                model=self.model,
                max_tokens=(THINKING_ALLOWANCE_TOKENS
                            + BASE_RESPONSE_TOKENS
                            + RESPONSE_TOKENS_PER_CANDIDATE * len(new_stories)),
                system=SYSTEM_PROMPT,
                tools=[tool],
                # Auto tool choice lets the model think before it calls
                # report_duplicates; a reply without that call is rejected
                # by _parse_response.
                tool_choice={"type": "auto"},
                output_config={"effort": "high"},
                messages=[{"role": "user", "content": prompt}]
            )

        except (anthropic.APIConnectionError, anthropic.RateLimitError,
                anthropic.InternalServerError) as e:
            logger.warning(f"Semantic dedup API call failed (transient): {e}")
            raise SemanticDedupUnavailable(
                f"semantic dedup API call failed transiently: {e}"
            ) from e

        return self._parse_response(response, new_handles, recent_handles)

    @staticmethod
    def _build_handles(stories: List[Dict], prefix: str) -> Dict[str, Dict]:
        """Map a per-call opaque handle to each story, in input order."""
        return {
            f"{prefix}{position}": story
            for position, story in enumerate(stories, start=1)
        }

    @staticmethod
    def _build_tool(new_handles: Dict[str, Dict],
                    recent_handles: Dict[str, Dict]) -> Dict[str, Any]:
        """Copy the tool template and constrain both id fields to this call's handles."""
        tool = copy.deepcopy(REPORT_DUPLICATES_TOOL)
        properties = tool["input_schema"]["properties"]["results"]["items"]["properties"]
        properties["new_id"]["enum"] = list(new_handles)
        properties["duplicate_of"]["enum"] = [""] + list(recent_handles)
        return tool

    def _build_prompt(self, new_handles: Dict[str, Dict],
                      recent_handles: Dict[str, Dict]) -> str:
        """Build the comparison prompt for Haiku, identifying stories by handle."""
        lines = ["Compare each NEW article against the RECENT articles and identify duplicates.\n"]

        lines.append("RECENT articles (already posted):")
        for handle, story in recent_handles.items():
            lines.append(f"- {self._format_story(handle, story)}")

        lines.append("\nNEW candidates:")
        for handle, story in new_handles.items():
            lines.append(f"- {self._format_story(handle, story)}")

        lines.append(f"\nUse the report_duplicates tool. Only mark as duplicate if confidence >= {self.threshold}.")

        return "\n".join(lines)

    @staticmethod
    def _format_story(handle: str, story: Dict) -> str:
        """Render one story line as its handle, title and optional summary."""
        summary_part = f" -- {story['summary']}" if story.get('summary') else ""
        return f"[{handle}] {story['title']}{summary_part}"

    def _parse_response(self, response, new_handles: Dict[str, Dict],
                        recent_handles: Dict[str, Dict]) -> Dict[str, str]:
        """Translate the tool use response into a new id -> recent id mapping.

        Raises SemanticDedupUnavailable when the response did not stop
        normally (any stop_reason other than "tool_use", which is how a
        completed report_duplicates call ends under auto tool choice) or
        carried no usable verdict at all. Individually unusable verdicts are
        warned about and discarded without counting as their candidate's verdict,
        and candidates left without a usable verdict are named once and treated
        as not duplicates.
        """
        if getattr(response, "stop_reason", None) == "max_tokens":
            logger.warning(
                "Semantic dedup response was truncated (stop_reason max_tokens); "
                "candidates past the cut-off received no verdict"
            )
            raise SemanticDedupUnavailable(
                "semantic dedup response was truncated (stop_reason max_tokens)"
            )

        stop_reason = getattr(response, "stop_reason", None)
        if stop_reason != "tool_use":
            logger.warning(
                f"Semantic dedup response did not stop normally "
                f"(stop_reason {stop_reason!r}); its verdicts cannot be trusted "
                f"as complete"
            )
            raise SemanticDedupUnavailable(
                f"semantic dedup response did not stop normally "
                f"(stop_reason {stop_reason!r})"
            )

        results = None
        for block in response.content:
            if block.type == "tool_use" and block.name == "report_duplicates":
                results = block.input.get("results", [])
                break

        if results is None:
            logger.warning(
                "Semantic dedup response carried no report_duplicates tool result"
            )
            raise SemanticDedupUnavailable(
                "semantic dedup response carried no report_duplicates tool result"
            )

        if not results:
            logger.warning(
                f"Semantic dedup returned an empty verdict list for "
                f"{len(new_handles)} candidate(s)"
            )
            raise SemanticDedupUnavailable(
                "semantic dedup returned an empty verdict list"
            )

        duplicates: Dict[str, str] = {}
        handles_with_verdict = set()

        for result in results:
            new_id = result.get("new_id")

            if not isinstance(new_id, str):
                logger.warning(
                    f"Skipping dedup result: new_id {new_id!r} is not a string"
                )
                continue

            if new_id not in new_handles:
                logger.warning(
                    f"Skipping dedup result: {new_id!r} is not a new candidate "
                    f"handle from this call"
                )
                continue

            if new_id in handles_with_verdict:
                logger.warning(
                    f"Ignoring repeated dedup verdict for new candidate {new_id!r}; "
                    f"keeping the first usable verdict"
                )
                continue

            duplicate_of = result.get("duplicate_of", "")
            if not isinstance(duplicate_of, str):
                logger.warning(
                    f"Skipping dedup result for new candidate {new_id!r}: "
                    f"duplicate_of {duplicate_of!r} is not a string"
                )
                continue

            confidence = result.get("confidence", 0.0)
            if not isinstance(confidence, (int, float)):
                logger.warning(
                    f"Skipping dedup result for new candidate {new_id!r}: "
                    f"confidence {confidence!r} is not a number"
                )
                continue

            if duplicate_of and confidence >= self.threshold:
                if duplicate_of not in recent_handles:
                    logger.warning(
                        f"Skipping dedup result for new candidate {new_id!r}: "
                        f"{duplicate_of!r} is not a recent story handle from this call"
                    )
                    continue

                new_guid = new_handles[new_id]["id"]
                recent_guid = recent_handles[duplicate_of]["id"]
                duplicates[new_guid] = recent_guid
                logger.info(
                    f"Semantic duplicate detected: '{new_guid}' "
                    f"duplicates '{recent_guid}' "
                    f"(confidence: {confidence:.2f})"
                )

            # Reached only by a verdict that survived every check, so a discarded
            # one leaves its candidate unjudged and open to a later usable result.
            handles_with_verdict.add(new_id)

        if not handles_with_verdict:
            logger.warning(
                f"Semantic dedup discarded every verdict it returned; none of the "
                f"{len(new_handles)} candidate(s) was judged"
            )
            raise SemanticDedupUnavailable(
                "semantic dedup response carried no usable verdict"
            )

        uncovered = [
            handle for handle in new_handles
            if handle not in handles_with_verdict
        ]
        if uncovered:
            logger.warning(
                f"Semantic dedup returned no verdict for candidate handle(s) "
                f"{', '.join(uncovered)}; treating them as not duplicates"
            )

        if duplicates:
            logger.info(f"Semantic dedup filtered {len(duplicates)} duplicate(s)")
        else:
            logger.info("Semantic dedup: no duplicates found")

        return duplicates
