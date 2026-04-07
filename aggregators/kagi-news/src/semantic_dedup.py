"""
Semantic Deduplication for Kagi News Aggregator.

Uses Claude Haiku to detect semantically similar stories within the same
feed/community. Compares new candidate stories against recently posted ones
using a single batched API call per feed.
"""
import logging
from typing import List, Dict, Set

import anthropic

logger = logging.getLogger(__name__)

# Tool schema for structured output from Haiku
REPORT_DUPLICATES_TOOL = {
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
                            "description": "The ID of the new candidate article"
                        },
                        "duplicate_of": {
                            "type": "string",
                            "description": "The ID of the recent article this is a duplicate of, or empty string if not a duplicate"
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
    ) -> Set[str]:
        """
        Find which new stories are semantic duplicates of recent ones.

        Args:
            new_stories: List of dicts with keys: id, title, summary
            recent_stories: List of dicts with keys: id, title, summary

        Returns:
            Set of new story IDs that are semantic duplicates
        """
        if not new_stories or not recent_stories:
            return set()

        prompt = self._build_prompt(new_stories, recent_stories)

        try:
            response = self.client.messages.create(
                model=self.model,
                max_tokens=1024,
                system=SYSTEM_PROMPT,
                tools=[REPORT_DUPLICATES_TOOL],
                tool_choice={"type": "tool", "name": "report_duplicates"},
                messages=[{"role": "user", "content": prompt}]
            )

            return self._parse_response(response)

        except (anthropic.APIConnectionError, anthropic.RateLimitError,
                anthropic.InternalServerError) as e:
            # Fail open for transient/network errors: don't block any posts
            logger.warning(f"Semantic dedup API call failed (transient), allowing all posts: {e}")
            return set()

    def _build_prompt(self, new_stories: List[Dict],
                      recent_stories: List[Dict]) -> str:
        """Build the comparison prompt for Haiku."""
        lines = ["Compare each NEW article against the RECENT articles and identify duplicates.\n"]

        lines.append("RECENT articles (already posted):")
        for story in recent_stories:
            summary_part = f" -- {story['summary']}" if story.get('summary') else ""
            lines.append(f"- [{story['id']}] {story['title']}{summary_part}")

        lines.append("\nNEW candidates:")
        for story in new_stories:
            summary_part = f" -- {story['summary']}" if story.get('summary') else ""
            lines.append(f"- [{story['id']}] {story['title']}{summary_part}")

        lines.append(f"\nUse the report_duplicates tool. Only mark as duplicate if confidence >= {self.threshold}.")

        return "\n".join(lines)

    def _parse_response(self, response) -> Set[str]:
        """Parse the tool use response and return duplicate story IDs."""
        duplicates = set()

        for block in response.content:
            if block.type == "tool_use" and block.name == "report_duplicates":
                results = block.input.get("results", [])
                for result in results:
                    duplicate_of = result.get("duplicate_of", "")
                    confidence = result.get("confidence", 0.0)

                    new_id = result.get("new_id")
                    if not new_id:
                        logger.warning(f"Skipping dedup result missing 'new_id': {result}")
                        continue

                    if duplicate_of and confidence >= self.threshold:
                        duplicates.add(new_id)
                        logger.info(
                            f"Semantic duplicate detected: '{new_id}' "
                            f"duplicates '{duplicate_of}' "
                            f"(confidence: {confidence:.2f})"
                        )

        if duplicates:
            logger.info(f"Semantic dedup filtered {len(duplicates)} duplicate(s)")
        else:
            logger.info("Semantic dedup: no duplicates found")

        return duplicates
