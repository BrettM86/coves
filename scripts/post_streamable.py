#!/usr/bin/env python3
"""
Quick script to post a Streamable video to test-usnews community.
Uses the kagi-news CovesClient infrastructure.
"""

import sys
import os

# Add kagi-news src to path to use CovesClient
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '../aggregators/kagi-news'))

from src.coves_client import CovesClient

def main():
    # Configuration
    COVES_API_URL = "http://localhost:8081"
    PDS_URL = "http://localhost:3001"

    # Use PDS instance credentials (from .env.dev)
    HANDLE = "testuser123.local.coves.dev"
    PASSWORD = "test-password-123"

    # Post details
    COMMUNITY_HANDLE = "test-usnews.community.coves.social"

    # Post 1: Streamable video
    STREAMABLE_URL = "https://streamable.com/7kpdft"
    STREAMABLE_TITLE = "NBACentral - \"Your son don't wanna be here, we know it's your last weekend. Enjoy ..."

    # Post 2: Reddit highlight
    REDDIT_URL = "https://www.reddit.com/r/nba/comments/1orfsgm/highlight_giannis_antetokounmpo_41_pts_15_reb_9/"
    REDDIT_TITLE = "[Highlight] Giannis Antetokounmpo (41 PTS, 15 REB, 9 AST) tallies his 56th career regular season game of 40+ points, passing Kareem Abdul-Jabbar for the most such games in franchise history. Milwaukee defeats Chicago 126-110 to win their NBA Cup opener."

    # Initialize client
    print(f"Initializing Coves client...")
    print(f"  API URL: {COVES_API_URL}")
    print(f"  PDS URL: {PDS_URL}")
    print(f"  Handle: {HANDLE}")

    client = CovesClient(
        api_url=COVES_API_URL,
        handle=HANDLE,
        password=PASSWORD,
        pds_url=PDS_URL
    )

    # Authenticate
    print("\nAuthenticating...")
    try:
        client.authenticate()
        print(f"✓ Authenticated as {client.did}")
    except Exception as e:
        print(f"✗ Authentication failed: {e}")
        return 1

    # Post 1: Streamable video
    print("\n" + "="*60)
    print("POST 1: STREAMABLE VIDEO")
    print("="*60)

    print("\nCreating minimal external embed (URI only)...")
    streamable_embed = {
        "$type": "social.coves.embed.external",
        "external": {
            "uri": STREAMABLE_URL
        }
    }
    print(f"✓ Embed created with URI only (unfurl service should enrich)")

    print(f"\nPosting to {COMMUNITY_HANDLE}...")
    print(f"  Title: {STREAMABLE_TITLE}")
    print(f"  Video: {STREAMABLE_URL}")

    try:
        post_uri = client.create_post(
            community_handle=COMMUNITY_HANDLE,
            title=STREAMABLE_TITLE,
            content="",
            facets=[],
            embed=streamable_embed
        )

        print(f"\n✓ Streamable post created successfully!")
        print(f"  URI: {post_uri}")

    except Exception as e:
        print(f"\n✗ Streamable post creation failed: {e}")
        import traceback
        traceback.print_exc()
        return 1

    # Post 2: Reddit highlight
    print("\n" + "="*60)
    print("POST 2: REDDIT HIGHLIGHT")
    print("="*60)

    print("\nCreating minimal external embed (URI only)...")
    reddit_embed = {
        "$type": "social.coves.embed.external",
        "external": {
            "uri": REDDIT_URL
        }
    }
    print(f"✓ Embed created with URI only (unfurl service should enrich)")

    print(f"\nPosting to {COMMUNITY_HANDLE}...")
    print(f"  Title: {REDDIT_TITLE}")
    print(f"  URL: {REDDIT_URL}")

    try:
        post_uri = client.create_post(
            community_handle=COMMUNITY_HANDLE,
            title=REDDIT_TITLE,
            content="",
            facets=[],
            embed=reddit_embed
        )

        print(f"\n✓ Reddit post created successfully!")
        print(f"  URI: {post_uri}")
        print(f"\n" + "="*60)
        print("Both posts created! Check them out at !test-usnews")
        print("="*60)
        return 0

    except Exception as e:
        print(f"\n✗ Reddit post creation failed: {e}")
        import traceback
        traceback.print_exc()
        return 1

if __name__ == "__main__":
    sys.exit(main())
