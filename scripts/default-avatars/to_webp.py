#!/usr/bin/env python3
"""Convert fitted default-avatar PNGs into the 512x512 WebP files the AppView embeds.

Usage:
    python3 scripts/default-avatars/to_webp.py <fitted-png-folder> <output-folder>

Each PNG becomes <slug>.webp, where the slug is the file stem. The alpha
channel is kept through the resize and the save, so the transparent corners
survive. Pillow's default alpha_quality (100) is left as is.
"""

import argparse
import sys
from pathlib import Path

from PIL import Image

SIZE = 512
QUALITY = 90

# Stems the fitting step produced that do not match the published slug.
SLUG_RENAMES = {"ship-s-wheel": "ships-wheel"}


def convert(source: Path, destination_folder: Path) -> Path:
    slug = SLUG_RENAMES.get(source.stem, source.stem)
    destination = destination_folder / f"{slug}.webp"
    with Image.open(source) as image:
        resized = image.convert("RGBA").resize((SIZE, SIZE), Image.Resampling.LANCZOS)
        resized.save(destination, format="WEBP", quality=QUALITY)
    return destination


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("input_folder", type=Path, help="folder of fitted PNGs")
    parser.add_argument("output_folder", type=Path, help="folder to write <slug>.webp files into")
    arguments = parser.parse_args()

    sources = sorted(arguments.input_folder.glob("*.png"))
    if not sources:
        print(f"no PNG files in {arguments.input_folder}", file=sys.stderr)
        return 1

    arguments.output_folder.mkdir(parents=True, exist_ok=True)
    total_bytes = 0
    for source in sources:
        destination = convert(source, arguments.output_folder)
        total_bytes += destination.stat().st_size
        print(f"{source.name} -> {destination.name}")
    print(f"wrote {len(sources)} files, {total_bytes} bytes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
