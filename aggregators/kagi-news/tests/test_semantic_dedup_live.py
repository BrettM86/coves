"""
Live integration tests for Semantic Deduplication.

These tests hit the real Anthropic API with Claude Haiku to validate
that semantic dedup correctly distinguishes:
1. True duplicates (same event, different headline) → SHOULD be filtered
2. Updates/developments (new info on same topic) → should NOT be filtered

Run with: ANTHROPIC_API_KEY=sk-ant-... pytest tests/test_semantic_dedup_live.py -v -s
Skip in CI with: pytest -m "not live"
"""
import os
import pytest

from src.semantic_dedup import SemanticDeduplicator


# Skip entire module if no API key
pytestmark = pytest.mark.live
ANTHROPIC_API_KEY = os.getenv("ANTHROPIC_API_KEY")


@pytest.fixture
def dedup():
    """Create a SemanticDeduplicator with real API key."""
    if not ANTHROPIC_API_KEY:
        pytest.skip("ANTHROPIC_API_KEY not set — skipping live test")
    return SemanticDeduplicator(api_key=ANTHROPIC_API_KEY, threshold=0.8)


class TestDuplicateDetection:
    """True duplicates: same event rewritten — these SHOULD be filtered."""

    def test_soleimani_visa_revocation_cross_feed(self, dedup):
        """Same event (Soleimani relatives detained) in World vs USA feeds."""
        recent = [{
            "id": "recent-world-soleimani",
            "title": "US revokes residency of Soleimani relatives, detains two",
            "summary": (
                "The US revoked green cards and visas for Iranian nationals "
                "tied to Iran's leadership, including Hamideh Soleimani Afshar "
                "and her daughter. Secretary of State Marco Rubio determined "
                "they were ineligible for lawful permanent resident status."
            ),
        }]

        new = [{
            "id": "new-usa-soleimani",
            "title": "US revokes visas of Soleimani relatives, detains two",
            "summary": (
                "The Trump administration revoked the green cards or visas of "
                "at least four Iranian nationals tied to Iran's current or "
                "former government, including Hamideh Soleimani Afshar and "
                "her daughter. The two women were detained by ICE in Southern "
                "California."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-usa-soleimani" in duplicates, (
            "Same event (Soleimani visa revocation) should be detected as duplicate"
        )

    def test_planet_labs_imagery_halt_cross_feed(self, dedup):
        """Same announcement (Planet Labs halts imagery) in World vs Tech feeds."""
        recent = [{
            "id": "recent-world-planet",
            "title": "US satellite firm halts Iran conflict imagery release",
            "summary": (
                "Planet Labs announced it will indefinitely withhold public "
                "release of satellite images covering Iran and nearby conflict "
                "zones following a US government request."
            ),
        }]

        new = [{
            "id": "new-tech-planet",
            "title": "Planet Labs halts Iran conflict imagery after US request",
            "summary": (
                "Planet Labs, a US commercial satellite imaging company, will "
                "indefinitely withhold imagery of Iran and the wider conflict "
                "region following a US government request."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-tech-planet" in duplicates, (
            "Same announcement (Planet Labs) should be detected as duplicate"
        )

    def test_italian_netflix_ruling_real_posts(self, dedup):
        """Real cross-day duplicate from c-tech.coves.social (04-05 → 04-06)."""
        recent = [{
            "id": "3miqvwsfw6s2x",
            "title": "Italian court orders Netflix to refund subscribers for price hikes since 2017",
            "summary": (
                "A court in Rome ruled that Netflix must refund Italian "
                "customers for price increases imposed between 2017 and "
                "January 2024, and cut subscription prices back to earlier levels."
            ),
        }]

        new = [{
            "id": "3mitgfmxetk2x",
            "title": "Italian court orders Netflix to refund customers up to €500 for price hikes",
            "summary": (
                "An Italian court has ruled that Netflix must refund customers "
                "for price increases introduced between 2017 and 2024, with "
                "some subscribers potentially receiving up to €500 in reimbursements."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "3mitgfmxetk2x" in duplicates, (
            "Real cross-day rewrite of Italian Netflix court ruling should be flagged"
        )

    def test_airman_rescue_cross_feed(self, dedup):
        """Same rescue event in World vs USA feeds."""
        recent = [{
            "id": "recent-world-rescue",
            "title": "Update: US rescues second airman from downed jet",
            "summary": (
                "US forces rescued the second crew member from an F-15E "
                "shot down over Iran, ending a tense search in the sixth "
                "week of conflict."
            ),
        }]

        new = [{
            "id": "new-usa-rescue",
            "title": "Update: U.S. forces rescue downed airman in Iran",
            "summary": (
                "In the latest turn in the U.S.-Iran conflict, President "
                "Trump said early Sunday that U.S. forces rescued an Air "
                "Force officer whose F-15E Strike Eagle was shot down over "
                "Iran on Friday."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-usa-rescue" in duplicates, (
            "Same rescue event should be detected as duplicate"
        )


class TestUpdatePassthrough:
    """Updates/developments: new info on same topic — should NOT be filtered."""

    def test_hormuz_deadline_vs_response(self, dedup):
        """Sequential events: US ultimatum then Iran's response — NOT duplicates."""
        recent = [{
            "id": "recent-hormuz-deadline",
            "title": "President Trump gives Iran 48-hour Hormuz deadline",
            "summary": (
                "President Trump said on April 4 that Iran had 48 hours to "
                "make a deal or reopen the Strait of Hormuz, or face further "
                "U.S. action."
            ),
        }]

        new = [{
            "id": "new-hormuz-response",
            "title": "Iran allows Iraqi ships through Strait of Hormuz",
            "summary": (
                "Iran announced on April 5 that Iraqi vessels can transit "
                "the Strait of Hormuz despite broader restrictions, with "
                "military spokesperson describing Iraq as exempt."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-hormuz-response" not in duplicates, (
            "Sequential Hormuz developments should NOT be flagged as duplicate"
        )

    def test_tariff_announcement_vs_implementation(self, dedup):
        """Announcement then implementation — NOT duplicates."""
        recent = [{
            "id": "recent-tariff-announce",
            "title": "US announces sweeping new tariffs on China",
            "summary": (
                "The United States announced new tariffs on Chinese goods "
                "effective next week, targeting electronics and automotive parts."
            ),
        }]

        new = [{
            "id": "new-tariff-effect",
            "title": "US-China tariffs take effect, markets drop sharply",
            "summary": (
                "The new US tariffs on Chinese goods officially took effect "
                "today, sending markets tumbling. The S&P 500 fell 2.3% as "
                "Beijing threatened retaliatory measures."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-tariff-effect" not in duplicates, (
            "Tariff implementation (with market reaction) is a new development, not a duplicate"
        )

    def test_samsung_different_products(self, dedup):
        """Same company, different product news — NOT duplicates."""
        recent = [{
            "id": "recent-samsung-messages",
            "title": "Samsung discontinues Messages app in favor of Google Messages",
            "summary": (
                "Samsung will discontinue its Messages app in the United "
                "States in July 2026 and transition users to Google Messages."
            ),
        }]

        new = [{
            "id": "new-samsung-s26",
            "title": "Samsung Galaxy S26 Ultra sparks camera upgrade debate",
            "summary": (
                "Early coverage of Samsung's Galaxy S26 Ultra presents mixed "
                "perspectives on whether a new flagship justifies upgrading."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-samsung-s26" not in duplicates, (
            "Different Samsung product news should NOT be flagged as duplicate"
        )

    def test_climate_different_findings(self, dedup):
        """Same broad topic (climate), different research — NOT duplicates."""
        recent = [{
            "id": "recent-climate-spring",
            "title": "Climate researchers link earlier spring to warming trends",
            "summary": (
                "Spring conditions are arriving approximately seven days "
                "earlier in St. Louis according to climate experts."
            ),
        }]

        new = [{
            "id": "new-climate-arctic",
            "title": "Arctic winter sea ice ties record low",
            "summary": (
                "Arctic winter sea ice reached its seasonal maximum at "
                "record-low levels. Thawing permafrost across northern "
                "Alaska increases runoff."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-climate-arctic" not in duplicates, (
            "Different climate research should NOT be flagged as duplicate"
        )

    def test_artemis_different_milestones(self, dedup):
        """Same mission, different milestones — NOT duplicates."""
        recent = [{
            "id": "recent-artemis-launch",
            "title": "NASA Artemis II crew reaches Earth orbit successfully",
            "summary": (
                "The four-person Artemis II crew has reached Earth orbit "
                "after a successful launch from Kennedy Space Center."
            ),
        }]

        new = [{
            "id": "new-artemis-flyby",
            "title": "Artemis II crew completes historic lunar flyby",
            "summary": (
                "The Artemis II crew completed their lunar flyby today, "
                "becoming the first humans to orbit the Moon since Apollo 17. "
                "All systems nominal for return trajectory."
            ),
        }]

        duplicates = dedup.find_duplicates(new, recent)
        assert "new-artemis-flyby" not in duplicates, (
            "Different mission milestones should NOT be flagged as duplicate"
        )


class TestBatchBehavior:
    """Test that batched comparison works correctly with mixed results."""

    def test_mixed_batch_duplicates_and_updates(self, dedup):
        """Batch with both duplicates and updates — only duplicates filtered."""
        recent = [
            {
                "id": "recent-soleimani",
                "title": "US revokes residency of Soleimani relatives, detains two",
                "summary": "The US revoked green cards for Iranian nationals tied to Iran's leadership.",
            },
            {
                "id": "recent-hormuz",
                "title": "President Trump gives Iran 48-hour Hormuz deadline",
                "summary": "Trump said Iran had 48 hours to reopen the Strait of Hormuz.",
            },
        ]

        new = [
            {
                "id": "new-soleimani-dupe",
                "title": "US revokes visas of Soleimani relatives, detains two",
                "summary": "Trump administration revoked green cards of Soleimani relatives. ICE detained two in California.",
            },
            {
                "id": "new-hormuz-update",
                "title": "Iran allows Iraqi ships through Strait of Hormuz",
                "summary": "Iran announced Iraqi vessels can transit Hormuz despite broader restrictions.",
            },
        ]

        duplicates = dedup.find_duplicates(new, recent)

        assert "new-soleimani-dupe" in duplicates, (
            "Soleimani rewrite should be caught as duplicate"
        )
        assert "new-hormuz-update" not in duplicates, (
            "Hormuz development should pass through as a new story"
        )
