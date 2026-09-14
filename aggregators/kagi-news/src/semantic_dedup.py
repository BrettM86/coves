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

SYSTEM_PROMPT = """You are a news deduplication system. Your job is to identify when a NEW article is essentially a rewrite of the same report as a RECENT article — covering the identical event with no meaningful new information.

DUPLICATE means: both articles describe the same specific occurrence at the same point in time, and the new one adds no significant new facts, outcome, or development beyond what the other already reported. They are essentially the same report from different outlets or with different headlines.

CRITICAL: For ongoing stories (missions, conflicts, investigations, court cases), focus on the article's PRIMARY new fact — NOT on recap/background context. News articles routinely recap prior events; that recap does NOT make the new article a duplicate. Compare what the article is actually REPORTING NEW.

NOT a duplicate when ANY of the following apply:
- The new article reports a SUBSEQUENT event, even if closely related — this includes:
  • Anticipation/preparation followed by the actual event ("preparing to launch" vs "launched successfully")
  • Announcement followed by implementation ("tariffs announced" vs "tariffs take effect")
  • Deadline followed by response ("48-hour ultimatum" vs "Iran responds")
  • One milestone followed by the next milestone (orbit → translunar injection → flyby → return)
- The new article reports a new daily status update on an ongoing mission/event, even if it recaps prior days
- The new article adds substantial new information: a new outcome, official response, policy change, or follow-up action
- The articles cover the same broad topic but different specific events or different time points

Examples:
- DUPLICATE: "US revokes residency of Soleimani relatives, detains two" and "US revokes visas of Soleimani relatives, detains two" (same event rewritten same day)
- DUPLICATE: "Planet Labs halts Iran conflict imagery after US request" and "US satellite firm halts Iran conflict imagery release" (same announcement)
- DUPLICATE: "Italian court orders Netflix to refund subscribers for price hikes" and "Italian court orders Netflix to refund customers up to €500" (same court ruling)
- NOT DUPLICATE: "NASA readies Artemis II crewed moon flyby" and "NASA launches Artemis II crew on lunar flyby" (preparation vs the launch actually happening — DIFFERENT events on different days)
- NOT DUPLICATE: "Artemis II sends astronauts toward the Moon" (TLI burn) and "Artemis II tests moon mission technologies" (next-day status, different activity)
- NOT DUPLICATE: "Artemis II crew nears moon flyby" (halfway point) and "Artemis II crew enters lunar space" (sphere of influence reached) — distinct mission milestones
- NOT DUPLICATE: "Trump gives Iran 48-hour Hormuz deadline" and "Iran allows Iraqi ships through Strait of Hormuz" (sequential events in same crisis)
- NOT DUPLICATE: "US announces tariffs on China" and "Tariffs officially take effect, markets react" (announcement vs implementation)
- NOT DUPLICATE: "Earthquake hits Turkey" and "Earthquake hits Japan" (same topic, different events)

When in doubt, mark as NOT a duplicate. It is better to allow a near-duplicate through than to suppress a unique story or a daily status update on an ongoing event."""


class SemanticDeduplicator:
    """
    Detects semantically similar news stories using Claude Haiku.

    Uses a single batched API call per feed to compare all new candidates
    against all recent stories simultaneously.
    """

    def __init__(self, api_key: str, threshold: float = 0.8,
                 model: str = "claude-haiku-4-5-20251001"):
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
                transient API failure, a truncated response, or a response
                carrying no usable verdict for a non-empty candidate list.
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
                max_tokens=(BASE_RESPONSE_TOKENS
                            + RESPONSE_TOKENS_PER_CANDIDATE * len(new_stories)),
                system=SYSTEM_PROMPT,
                tools=[tool],
                tool_choice={"type": "tool", "name": "report_duplicates"},
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

        Raises SemanticDedupUnavailable when the response was truncated or
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
