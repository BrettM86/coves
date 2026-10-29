#!/usr/bin/env python3
"""Generate default-avatar candidates with Codex CLI's image generation tool.

Usage:
  python3 generate_avatars.py octopus "sea turtle" crab
  python3 generate_avatars.py --subjects-file subjects.txt --variants 3

Requires `codex` logged in (a ChatGPT plan works; no API key needed). Each
image is one `codex exec` run. Writes <subject>-<n>.png into a new temp folder
(or --out) and prints the folder path. Standard library only.
"""

import argparse
import os
import re
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor, as_completed

STYLE = (
    "A single {subject}, centered, drawn as a simple flat vector icon for a "
    "social app's default profile picture. Thick, uniform dark charcoal outline "
    "(#333333) with rounded line ends. Flat fills, no gradients, no texture, no "
    "highlights; at most one soft offset shadow shape in a lighter tint. Muted, "
    "dusty palette with at most three colors besides the outline. Cute and "
    "friendly but minimal: if it is an animal, give it small dot eyes, only the "
    "ones visible from its pose, and no other facial features. No text, no scenery, no bubbles, no water "
    "lines. Transparent background. The subject fills about 60% of the square "
    "frame with even padding and must stay recognizable at 24 pixels wide."
)

INSTRUCTION = (
    "Use your image generation tool to generate exactly one image, high quality, "
    "transparent background, from the prompt below. Copy the generated PNG "
    "unchanged to {path}. Do not edit, resize or regenerate it.\n\nPrompt: {prompt}"
)

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def generate(path, subject, effort, timeout):
    instruction = INSTRUCTION.format(path=path, prompt=STYLE.format(subject=subject))
    result = subprocess.run(
        [
            "codex", "exec", "--skip-git-repo-check",
            "--sandbox", "workspace-write",
            "-c", f"model_reasoning_effort={effort}",
            "--cd", os.path.dirname(path),
            instruction,
        ],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    with open(path + ".log", "w") as handle:
        handle.write(result.stdout + result.stderr)
    if result.returncode != 0:
        raise RuntimeError(f"codex exited {result.returncode}, see {path}.log")
    if not os.path.exists(path):
        raise RuntimeError(f"codex did not write the image, see {path}.log")
    with open(path, "rb") as handle:
        if handle.read(8) != PNG_SIGNATURE:
            raise RuntimeError(f"output is not a PNG, see {path}.log")


def slug(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("subjects", nargs="*")
    parser.add_argument("--subjects-file", help="one subject per line; # starts a comment")
    parser.add_argument("--variants", type=int, default=1, help="images per subject")
    parser.add_argument("--effort", default="high", choices=["low", "medium", "high"],
                        help="Codex agent reasoning effort")
    parser.add_argument("--out", help="output folder (default: a new temp folder)")
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--timeout", type=int, default=600, help="seconds per image")
    args = parser.parse_args()

    subjects = list(args.subjects)
    if args.subjects_file:
        with open(args.subjects_file) as handle:
            subjects += [line.split("#")[0].strip() for line in handle]
    subjects = [subject for subject in subjects if subject]
    if not subjects:
        sys.exit("no subjects given")

    out = os.path.abspath(args.out or tempfile.mkdtemp(prefix="coves-avatars-"))
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "prompt.txt"), "w") as handle:
        handle.write(f"codex image generation, agent effort={args.effort}\n\n{STYLE}\n")
    print(out)

    jobs = [
        (os.path.join(out, f"{slug(subject)}-{variant}.png"), subject)
        for subject in subjects
        for variant in range(1, args.variants + 1)
    ]
    failures = 0
    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = {
            pool.submit(generate, path, subject, args.effort, args.timeout): path
            for path, subject in jobs
        }
        for future in as_completed(futures):
            path = futures[future]
            try:
                future.result()
                print(f"ok    {path}", flush=True)
            except Exception as error:
                failures += 1
                print(f"FAIL  {path}: {error}", file=sys.stderr, flush=True)
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
