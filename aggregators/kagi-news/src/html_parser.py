"""
Kagi News HTML description parser.

Parses the HTML content from RSS feed item descriptions
into structured data.
"""
import re
import logging
from typing import Dict, List, Optional
from datetime import datetime
from bs4 import BeautifulSoup
from urllib.parse import urlparse

from src.models import KagiStory, Perspective, Quote, Source

logger = logging.getLogger(__name__)


class KagiHTMLParser:
    """Parses Kagi News HTML descriptions into structured data."""

    def parse(self, html_description: str) -> Dict:
        """
        Parse HTML description into structured data.

        Args:
            html_description: HTML content from RSS item description

        Returns:
            Dictionary with extracted data:
                - summary: str
                - image_url: Optional[str]
                - image_alt: Optional[str]
                - highlights: List[str]
                - quote: Optional[Dict[str, str]]
                - perspectives: List[Dict]
                - sources: List[Dict]
        """
        soup = BeautifulSoup(html_description, 'html.parser')

        return {
            'summary': self._extract_summary(soup),
            'image_url': self._extract_image_url(soup),
            'image_alt': self._extract_image_alt(soup),
            'highlights': self._extract_highlights(soup),
            'quote': self._extract_quote(soup),
            'perspectives': self._extract_perspectives(soup),
            'sources': self._extract_sources(soup),
        }

    def parse_to_story(
        self,
        title: str,
        link: str,
        guid: str,
        pub_date: datetime,
        categories: List[str],
        html_description: str
    ) -> KagiStory:
        """
        Parse HTML and create a KagiStory object.

        Args:
            title: Story title
            link: Story URL
            guid: Unique identifier
            pub_date: Publication date
            categories: List of categories
            html_description: HTML content from description

        Returns:
            KagiStory object
        """
        parsed = self.parse(html_description)

        # Convert parsed data to model objects
        perspectives = [
            Perspective(
                actor=p['actor'],
                description=p['description'],
                source_url=p['source_url'],
                source_name=p.get('source_name', '')
            )
            for p in parsed['perspectives']
        ]

        sources = [
            Source(
                title=s['title'],
                url=s['url'],
                domain=s['domain']
            )
            for s in parsed['sources']
        ]

        quote = None
        if parsed['quote']:
            quote = Quote(
                text=parsed['quote']['text'],
                attribution=parsed['quote']['attribution']
            )

        return KagiStory(
            title=title,
            link=link,
            guid=guid,
            pub_date=pub_date,
            categories=categories,
            summary=parsed['summary'],
            highlights=parsed['highlights'],
            perspectives=perspectives,
            quote=quote,
            sources=sources,
            image_url=parsed['image_url'],
            image_alt=parsed['image_alt']
        )

    def _extract_summary(self, soup: BeautifulSoup) -> str:
        """Extract summary from first <p> tag."""
        p_tag = soup.find('p')
        if p_tag:
            return p_tag.get_text(strip=True)
        return ""

    def _extract_image_url(self, soup: BeautifulSoup) -> Optional[str]:
        """Extract image URL from <img> tag."""
        img_tag = soup.find('img')
        if img_tag and img_tag.get('src'):
            return img_tag['src']
        return None

    def _extract_image_alt(self, soup: BeautifulSoup) -> Optional[str]:
        """Extract image alt text from <img> tag."""
        img_tag = soup.find('img')
        if img_tag and img_tag.get('alt'):
            return img_tag['alt']
        return None

    def _extract_highlights(self, soup: BeautifulSoup) -> List[str]:
        """Extract highlights list from H3 section."""
        highlights = []

        # Find "Highlights:" h3 tag
        h3_tags = soup.find_all('h3')
        for h3 in h3_tags:
            if 'Highlights' in h3.get_text():
                # Get the <ul> that follows this h3
                ul = h3.find_next_sibling('ul')
                if ul:
                    for li in ul.find_all('li'):
                        highlights.append(li.get_text(strip=True))
                break

        return highlights

    def _extract_quote(self, soup: BeautifulSoup) -> Optional[Dict[str, str]]:
        """Extract quote from <blockquote> tag."""
        blockquote = soup.find('blockquote')
        if not blockquote:
            return None

        text = blockquote.get_text(strip=True)

        # Try to split on " - " to separate quote from attribution
        if ' - ' in text:
            quote_text, attribution = text.rsplit(' - ', 1)
            return {
                'text': quote_text.strip(),
                'attribution': attribution.strip()
            }

        # If no attribution found, entire text is the quote
        # Try to infer attribution from context (often mentioned in highlights/perspectives)
        return {
            'text': text,
            'attribution': self._infer_quote_attribution(soup, text)
        }

    def _infer_quote_attribution(self, soup: BeautifulSoup, quote_text: str) -> str:
        """
        Try to infer quote attribution from context.

        This is a fallback when quote doesn't have explicit attribution.
        """
        # For now, check if any perspective mentions similar keywords
        perspectives_section = soup.find('h3', string=re.compile(r'Perspectives'))
        if perspectives_section:
            ul = perspectives_section.find_next_sibling('ul')
            if ul:
                for li in ul.find_all('li'):
                    li_text = li.get_text()
                    # Extract actor name (before first colon)
                    if ':' in li_text:
                        actor = li_text.split(':', 1)[0].strip()
                        return actor

        return "Unknown"

    def _extract_perspectives(self, soup: BeautifulSoup) -> List[Dict]:
        """Extract perspectives from H3 section."""
        perspectives = []

        # Find "Perspectives:" h3 tag
        h3_tags = soup.find_all('h3')
        for h3 in h3_tags:
            if 'Perspectives' in h3.get_text():
                # Get the <ul> that follows this h3
                ul = h3.find_next_sibling('ul')
                if ul:
                    for li in ul.find_all('li'):
                        perspective = self._parse_perspective_li(li)
                        if perspective:
                            perspectives.append(perspective)
                break

        return perspectives

    def _parse_perspective_li(self, li) -> Optional[Dict]:
        """
        Parse a single perspective <li> element.

        Format: "Actor: Description. (Source)"
        """
        # Get full text
        full_text = li.get_text()

        # Extract actor (before first colon)
        if ':' not in full_text:
            return None

        actor, rest = full_text.split(':', 1)
        actor = actor.strip()

        # Find the <a> tag for source URL and name
        a_tag = li.find('a')
        source_url = a_tag['href'] if a_tag and a_tag.get('href') else ""
        source_name = a_tag.get_text(strip=True) if a_tag else ""

        # Extract description (between colon and source link)
        # Remove the source citation part in parentheses
        description = rest

        # Remove source citation like "(The Straits Times)" from description
        if a_tag:
            # Remove the link text and surrounding parentheses
            link_text = a_tag.get_text()
            description = description.replace(f"({link_text})", "").strip()

        # Clean up trailing period
        description = description.strip('. ')

        return {
            'actor': actor,
            'description': description,
            'source_url': source_url,
            'source_name': source_name
        }

    def _extract_sources(self, soup: BeautifulSoup) -> List[Dict]:
        """Extract sources list from H3 section."""
        sources = []

        # Find "Sources:" h3 tag
        h3_tags = soup.find_all('h3')
        for h3 in h3_tags:
            if 'Sources' in h3.get_text():
                # Get the <ul> that follows this h3
                ul = h3.find_next_sibling('ul')
                if ul:
                    for li in ul.find_all('li'):
                        source = self._parse_source_li(li)
                        if source:
                            sources.append(source)
                break

        return sources

    def _parse_source_li(self, li) -> Optional[Dict]:
        """
        Parse a single source <li> element.

        Format: "<a href='...'>Title</a> - domain.com"
        """
        a_tag = li.find('a')
        if not a_tag or not a_tag.get('href'):
            return None

        title = a_tag.get_text(strip=True)
        url = a_tag['href']

        # Extract domain from URL
        parsed_url = urlparse(url)
        domain = parsed_url.netloc

        # Remove "www." prefix if present
        if domain.startswith('www.'):
            domain = domain[4:]

        return {
            'title': title,
            'url': url,
            'domain': domain
        }
