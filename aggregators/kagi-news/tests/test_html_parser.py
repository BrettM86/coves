"""
Tests for Kagi HTML description parser.
"""
import pytest
from pathlib import Path
from datetime import datetime
import html

from src.html_parser import KagiHTMLParser
from src.models import KagiStory, Perspective, Quote, Source


@pytest.fixture
def sample_html_description():
    """Load sample HTML from RSS item fixture."""
    # This is the escaped HTML from the RSS description field
    html_content = """<p>The White House confirmed President Trump will hold a bilateral meeting with Chinese President Xi Jinping in South Korea on October 30, at the end of an Asia trip that includes Malaysia and Japan . The administration said the meeting will take place Thursday morning local time, and Mr Trump indicated his first question to Xi would concern fentanyl and other bilateral issues . The talks come amid heightened trade tensions after Beijing expanded export curbs on rare-earth minerals and following Mr Trump's recent threat of additional tariffs on Chinese goods, making the meeting a focal point for discussions on trade, technology supply chains and energy .</p><img src='https://kagiproxy.com/img/Q2SRXQtwTYBIiQeI0FG-X6taF_wHSJaXDiFUzju2kbCWGuOYIFUX--8L0BqE4VKxpbOJY3ylFPJkDpfSnyQYZ1qdOLXbphHTnsOK4jb7gqC4KCn5nf3ANbWCuaFD5ZUSijiK0k7wOLP2fyX6tynu2mPtXlCbotLo2lTrEswZl4-No2AI4mI4lkResfnRdp-YjpoEfCOHkNfbN1-0cNcHt9T2dmgBSXrQ2w' alt='News image associated with coverage of President Trump&#x27;s Asia trip and planned meeting with President Xi' /><br /><h3>Highlights:</h3><ul><li>Itinerary details: The Asia swing begins in Malaysia, continues to Japan and ends with the bilateral meeting in South Korea on Thursday morning local time, White House press secretary Karoline Leavitt said at a briefing .</li><li>APEC context: US officials indicated the leaders will meet on the sidelines of the Asia-Pacific Economic Cooperation gathering, shaping expectations for short, high-level talks rather than a lengthy summit .</li></ul><blockquote>Work out a lot of our doubts and questions - President Trump</blockquote><h3>Perspectives:</h3><ul><li>President Trump: He said his first question to President Xi would be about fentanyl and indicated he hoped to resolve bilateral doubts and questions in the talks. (<a href='https://www.straitstimes.com/world/united-states/trump-to-meet-xi-in-south-korea-on-oct-30-as-part-of-asia-swing'>The Straits Times</a>)</li><li>White House (press secretary): Karoline Leavitt confirmed the bilateral meeting will occur Thursday morning local time during a White House briefing. (<a href='https://www.scmp.com/news/us/diplomacy/article/3330131/donald-trump-meet-chinas-xi-jinping-next-thursday-south-korea-crunch-talks'>South China Morning Post</a>)</li></ul><h3>Sources:</h3><ul><li><a href='https://www.straitstimes.com/world/united-states/trump-to-meet-xi-in-south-korea-on-oct-30-as-part-of-asia-swing'>Trump to meet Xi in South Korea on Oct 30 as part of Asia swing</a> - straitstimes.com</li><li><a href='https://www.scmp.com/news/us/diplomacy/article/3330131/donald-trump-meet-chinas-xi-jinping-next-thursday-south-korea-crunch-talks'>Trump to meet Xi in South Korea next Thursday as part of key Asia trip</a> - scmp.com</li></ul>"""
    return html_content


class TestKagiHTMLParser:
    """Test suite for Kagi HTML parser."""

    def test_parse_summary(self, sample_html_description):
        """Test extracting summary paragraph."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert result['summary'].startswith("The White House confirmed President Trump")
        assert "bilateral meeting with Chinese President Xi Jinping" in result['summary']

    def test_parse_image_url(self, sample_html_description):
        """Test extracting image URL and alt text."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert result['image_url'] is not None
        assert result['image_url'].startswith("https://kagiproxy.com/img/")
        assert result['image_alt'] is not None
        assert "Trump" in result['image_alt']

    def test_parse_highlights(self, sample_html_description):
        """Test extracting highlights list."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert len(result['highlights']) == 2
        assert "Itinerary details" in result['highlights'][0]
        assert "APEC context" in result['highlights'][1]

    def test_parse_quote(self, sample_html_description):
        """Test extracting blockquote."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert result['quote'] is not None
        assert result['quote']['text'] == "Work out a lot of our doubts and questions"
        assert result['quote']['attribution'] == "President Trump"

    def test_parse_perspectives(self, sample_html_description):
        """Test extracting perspectives list."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert len(result['perspectives']) == 2

        # First perspective
        assert result['perspectives'][0]['actor'] == "President Trump"
        assert "fentanyl" in result['perspectives'][0]['description']
        assert result['perspectives'][0]['source_url'] == "https://www.straitstimes.com/world/united-states/trump-to-meet-xi-in-south-korea-on-oct-30-as-part-of-asia-swing"

        # Second perspective
        assert "White House" in result['perspectives'][1]['actor']

    def test_parse_sources(self, sample_html_description):
        """Test extracting sources list."""
        parser = KagiHTMLParser()
        result = parser.parse(sample_html_description)

        assert len(result['sources']) >= 2

        # Check first source
        assert result['sources'][0]['title'] == "Trump to meet Xi in South Korea on Oct 30 as part of Asia swing"
        assert result['sources'][0]['url'].startswith("https://www.straitstimes.com")
        assert result['sources'][0]['domain'] == "straitstimes.com"

    def test_parse_missing_sections(self):
        """Test parsing HTML with missing sections."""
        html_minimal = "<p>Just a summary, no other sections.</p>"

        parser = KagiHTMLParser()
        result = parser.parse(html_minimal)

        assert result['summary'] == "Just a summary, no other sections."
        assert result['highlights'] == []
        assert result['perspectives'] == []
        assert result['sources'] == []
        assert result['quote'] is None
        assert result['image_url'] is None

    def test_parse_to_kagi_story(self, sample_html_description):
        """Test converting parsed HTML to KagiStory object."""
        parser = KagiHTMLParser()

        # Simulate full RSS item data
        story = parser.parse_to_story(
            title="Trump to meet Xi in South Korea on Oct 30",
            link="https://kite.kagi.com/test/world/10",
            guid="https://kite.kagi.com/test/world/10",
            pub_date=datetime(2025, 10, 23, 20, 56, 0),
            categories=["World", "World/Diplomacy"],
            html_description=sample_html_description
        )

        assert isinstance(story, KagiStory)
        assert story.title == "Trump to meet Xi in South Korea on Oct 30"
        assert story.link == "https://kite.kagi.com/test/world/10"
        assert len(story.highlights) == 2
        assert len(story.perspectives) == 2
        assert len(story.sources) >= 2
        assert story.quote is not None
        assert story.image_url is not None
