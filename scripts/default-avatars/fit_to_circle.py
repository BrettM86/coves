#!/usr/bin/env python3
"""Shrink avatar images whose subject would be clipped by a circular crop.

Usage:
  python3 fit_to_circle.py /tmp/coves-avatars-XXXX

Writes a copy of every <name>.png into <folder>/fitted/. An image whose
opaque pixels all sit inside the circle is copied unchanged; any other is
scaled down about its centre until they do. Requires Pillow.
"""

import argparse
import glob
import math
import os

from PIL import Image

ALPHA_THRESHOLD = 16  # ignore faint anti-aliasing and shadow fringes
INSET = 0.98  # keep the subject just inside the circle edge


def farthest_opaque_distance(image):
    alpha = image.getchannel("A")
    width, height = alpha.size
    center_x, center_y = (width - 1) / 2, (height - 1) / 2
    pixels = alpha.load()
    farthest = 0.0
    for y in range(height):
        for x in range(width):
            if pixels[x, y] > ALPHA_THRESHOLD:
                farthest = max(farthest, math.hypot(x - center_x, y - center_y))
    return farthest


def fit(image):
    """Return (fitted image, scale applied)."""
    radius = min(image.size) / 2 * INSET
    # Measure on a downscaled copy; full-size per-pixel loops are slow.
    probe_size = 256
    probe = image.resize((probe_size, probe_size), Image.LANCZOS)
    farthest = farthest_opaque_distance(probe) * image.size[0] / probe_size
    if farthest <= radius:
        return image, 1.0
    scale = radius / farthest
    width, height = image.size
    shrunk = image.resize((round(width * scale), round(height * scale)), Image.LANCZOS)
    canvas = Image.new("RGBA", image.size, (0, 0, 0, 0))
    canvas.paste(shrunk, ((width - shrunk.width) // 2, (height - shrunk.height) // 2))
    return canvas, scale


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("folder")
    args = parser.parse_args()

    out = os.path.join(args.folder, "fitted")
    os.makedirs(out, exist_ok=True)
    for path in sorted(glob.glob(os.path.join(args.folder, "*.png"))):
        fitted, scale = fit(Image.open(path).convert("RGBA"))
        fitted.save(os.path.join(out, os.path.basename(path)))
        if scale < 1.0:
            print(f"shrunk {os.path.basename(path)} to {scale:.1%}")
    print(out)


if __name__ == "__main__":
    main()
