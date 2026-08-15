#!/usr/bin/env python3
"""Typeset a demo transcript as an SVG terminal window.

    ./demo/demo.sh | ./demo/render.py > docs/demo.svg

The text is whatever the transcript contains, verbatim: this colours real
output, it does not invent any. A static frame rather than an animation
because a README image has to render the same way for everyone, including in
the places that strip animation.
"""

import html
import re
import sys

FONT_SIZE = 13
CHAR_W = 7.82  # advance width of the font stack below at FONT_SIZE
LINE_H = 21
PAD_X, PAD_TOP, PAD_BOTTOM = 20, 46, 20

# A dark palette with enough contrast to stay legible on GitHub's dark and
# light backgrounds alike, since the image is served as-is on both.
COLORS = {
    "bg": "#12151c",
    "chrome": "#1b1f28",
    "text": "#d5dae3",
    "dim": "#6c7686",
    "prompt": "#5fb28a",
    "cmd": "#e8edf4",
    "good": "#5fb28a",
    "warn": "#d9a04a",
    "accent": "#7aa2d6",
}

# Words worth colouring, in the order they are applied.
HIGHLIGHT = [
    (r"\bexhausted\b", "warn"),
    (r"\bavailable\b", "good"),
    (r"\binvalid\b", "warn"),
    (r"\brotating\b", "warn"),
    (r"\b\d+h \d+m\b", "warn"),
    (r"\b\d+/\d+ ready\b", "good"),
    (r"\bstatus=200\b", "good"),
    (r"\b\d+%", "accent"),
]


def spans(line):
    """Split a line into (text, color-key) runs."""
    if line.startswith("$ "):
        return [("$ ", "prompt"), (line[2:], "cmd")]
    if line.startswith("#"):
        return [(line, "dim")]
    if line.startswith("ACCOUNT") or line.startswith("ID "):
        return [(line, "dim")]
    if line.startswith(("account exhausted", "served ")):
        return colorize(line, base="dim")
    return colorize(line, base="text")


def colorize(line, base):
    marks = {}
    for pattern, color in HIGHLIGHT:
        for m in re.finditer(pattern, line):
            for i in range(m.start(), m.end()):
                marks.setdefault(i, color)

    out, run, run_color = [], "", marks.get(0, base)
    for i, ch in enumerate(line):
        color = marks.get(i, base)
        if color != run_color:
            out.append((run, run_color))
            run, run_color = "", color
        run += ch
    out.append((run, run_color))
    return [(t, c) for t, c in out if t]


def main():
    lines = [l.rstrip() for l in sys.stdin.read().splitlines()]
    while lines and not lines[-1]:
        lines.pop()

    width = int(max(len(l) for l in lines) * CHAR_W) + PAD_X * 2
    height = len(lines) * LINE_H + PAD_TOP + PAD_BOTTOM

    print(f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
          f'viewBox="0 0 {width} {height}" font-family="ui-monospace,SFMono-Regular,'
          f'Menlo,Consolas,&quot;DejaVu Sans Mono&quot;,monospace" font-size="{FONT_SIZE}">')
    print(f'<rect width="{width}" height="{height}" rx="10" fill="{COLORS["bg"]}"/>')
    print(f'<path d="M0 10a10 10 0 0 1 10-10h{width - 20}a10 10 0 0 1 10 10v20H0z" '
          f'fill="{COLORS["chrome"]}"/>')
    for i, color in enumerate(("#e06c62", "#d9a04a", "#5fb28a")):
        print(f'<circle cx="{20 + i * 18}" cy="15" r="5.5" fill="{color}"/>')
    print(f'<text x="{width / 2}" y="19.5" fill="{COLORS["dim"]}" font-size="11" '
          f'text-anchor="middle">agentswap</text>')

    y = PAD_TOP + FONT_SIZE
    for line in lines:
        if line:
            print(f'<text x="{PAD_X}" y="{y}" xml:space="preserve">', end="")
            for text, color in spans(line):
                weight = ' font-weight="600"' if color == "cmd" else ""
                print(f'<tspan fill="{COLORS[color]}"{weight}>{html.escape(text)}</tspan>', end="")
            print("</text>")
        y += LINE_H
    print("</svg>")


if __name__ == "__main__":
    main()
