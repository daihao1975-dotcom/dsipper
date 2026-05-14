#!/usr/bin/env python3
"""
ansi_to_html.py — convert dsipper terminal output (ANSI truecolor + cursor
control) into a single self-contained HTML page so reviewers without a real
terminal can see the actual color rendering.

Handles:
  - \x1b[0m                           reset
  - \x1b[1m / \x1b[2m                bold / dim
  - \x1b[38;2;R;G;Bm                truecolor foreground
  - \x1b[NF + \x1b[J                cursor-up N lines then clear to end —
                                    used by LivePanel to redraw in place;
                                    we simulate it by deleting the last N
                                    rendered lines so the HTML ends up with
                                    only the final frame instead of every
                                    intermediate frame stacked.
  - \x1b[?25l / \x1b[?25h           show/hide cursor — stripped
  - \x1b[2K, \r                     line clear / CR — collapsed

Usage:
    python3 ansi_to_html.py scene1.ansi scene2.ansi ... > out.html
"""

import html
import re
import sys

SCENE_TITLES = [
    "Banner + config box (program startup)",
    "Colored slog: single call with hold + DTMF + re-INVITE",
    "Stress LivePanel + summary box (12 calls × 4 workers)",
    "All-fail stress: err top with full error message (no truncation)",
]

ANSI_SGR = re.compile(r"\x1b\[([\d;]*)m")
CURSOR_UP_CLR = re.compile(r"\x1b\[(\d+)F\x1b\[J")
HIDE_SHOW_CUR = re.compile(r"\x1b\[\?\d+[hl]")
LINE_CLEAR = re.compile(r"\x1b\[2K")


def simulate_cursor_redraw(text: str) -> str:
    """LivePanel emits \x1b[NF\x1b[J to wipe the last N lines and redraw the
    same box with updated content. Reproduce that semantically: every time we
    hit that sequence, drop the trailing N newlines (and everything after the
    last surviving newline) from the buffer so only the final frame remains."""
    out: list[str] = []
    i = 0
    while i < len(text):
        m = CURSOR_UP_CLR.search(text, i)
        if not m:
            out.append(text[i:])
            break
        out.append(text[i : m.start()])
        n = int(m.group(1))
        # Walk back N newlines in out — and trim what's between the (N+1)-th
        # last newline and the end of the buffer.
        joined = "".join(out)
        cut = len(joined)
        for _ in range(n):
            cut = joined.rfind("\n", 0, cut)
            if cut < 0:
                break
        if cut < 0:
            joined = ""
        else:
            joined = joined[: cut + 1]  # keep that newline
        out = [joined]
        i = m.end()
    return "".join(out)


def strip_minor_ctrl(text: str) -> str:
    text = HIDE_SHOW_CUR.sub("", text)
    text = LINE_CLEAR.sub("", text)
    text = text.replace("\r", "")
    return text


def sgr_to_css(codes: str) -> str | None:
    """Convert one \x1b[...m sequence into a CSS rule (or None for reset)."""
    parts = [int(x) for x in codes.split(";") if x]
    if not parts:
        return None  # \x1b[m == reset
    styles: list[str] = []
    i = 0
    while i < len(parts):
        c = parts[i]
        if c == 0:
            return None  # explicit reset
        elif c == 1:
            styles.append("font-weight:700")
        elif c == 2:
            styles.append("opacity:0.55")
        elif c == 38 and i + 4 < len(parts) and parts[i + 1] == 2:
            r, g, b = parts[i + 2], parts[i + 3], parts[i + 4]
            styles.append(f"color:rgb({r},{g},{b})")
            i += 4
        i += 1
    return ";".join(styles) if styles else None


def ansi_to_html(text: str) -> str:
    """Convert an ANSI-bearing string to HTML. Emits properly nested <span>s."""
    text = simulate_cursor_redraw(text)
    text = strip_minor_ctrl(text)

    out: list[str] = []
    open_spans = 0
    pos = 0
    for m in ANSI_SGR.finditer(text):
        # emit text between previous SGR and this one
        chunk = text[pos : m.start()]
        if chunk:
            out.append(html.escape(chunk))
        codes = m.group(1)
        css = sgr_to_css(codes)
        if css is None:
            # reset → close all open spans
            out.append("</span>" * open_spans)
            open_spans = 0
        else:
            out.append(f'<span style="{css}">')
            open_spans += 1
        pos = m.end()
    # tail
    tail = text[pos:]
    if tail:
        out.append(html.escape(tail))
    out.append("</span>" * open_spans)
    return "".join(out)


# NOTE: HTML / CSS contains '{' '}' literals — DO NOT switch to str.format here.
# Use simple __PLACEHOLDER__ substitution via .replace() to keep CSS intact.
HTML_TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>dsipper CLUI demo</title>
<style>
  :root {
    --bg: #1a1a1a;
    --fg: #e0e0e0;
    --primary: #1677FF;
    --muted: #888;
    --hair: #2a2a2a;
  }
  * { box-sizing: border-box; }
  body {
    background: var(--bg);
    color: var(--fg);
    margin: 0;
    padding: 24px 40px 48px;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
    line-height: 1.5;
  }
  header {
    margin-bottom: 24px;
    padding-bottom: 12px;
    border-bottom: 1px solid var(--hair);
  }
  header h1 {
    color: var(--primary);
    margin: 0 0 4px;
    font-size: 20px;
    font-weight: 600;
  }
  header .sub {
    color: var(--muted);
    font-size: 13px;
  }
  h2 {
    color: var(--primary);
    font-size: 14px;
    font-weight: 600;
    margin: 28px 0 10px;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--hair);
    letter-spacing: 0.3px;
  }
  .scene-num {
    display: inline-block;
    min-width: 24px;
    color: var(--muted);
    font-weight: 400;
  }
  .note {
    color: var(--muted);
    font-size: 12px;
    margin: 6px 0 10px;
    font-style: italic;
  }
  pre {
    background: #0e0e0e;
    color: var(--fg);
    padding: 16px 18px;
    border-radius: 6px;
    border: 1px solid var(--hair);
    line-height: 1.35;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
    font-size: 12.5px;
    white-space: pre;
    overflow-x: auto;
    margin: 0;
  }
  footer {
    margin-top: 36px;
    padding-top: 12px;
    border-top: 1px solid var(--hair);
    color: var(--muted);
    font-size: 11px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
</style>
</head>
<body>
<header>
  <h1>dsipper CLUI rendering preview</h1>
  <div class="sub">ANSI truecolor terminal output rendered as HTML. LivePanel
  redraws have been collapsed to their final frame (real terminal animates;
  HTML shows the steady-state result).</div>
</header>
__SCENES__
<footer>generated by test/render-demo.sh · dsipper __VERSION__</footer>
</body>
</html>
"""


def main(paths: list[str]) -> None:
    parts: list[str] = []
    for idx, path in enumerate(paths):
        title = SCENE_TITLES[idx] if idx < len(SCENE_TITLES) else f"Scene {idx + 1}"
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as f:
                raw = f.read()
        except OSError as e:
            raw = f"[failed to read {path}: {e}]"
        rendered = ansi_to_html(raw)
        parts.append(
            f'<h2><span class="scene-num">{idx + 1}.</span> {html.escape(title)}</h2>\n'
            f"<pre>{rendered}</pre>"
        )

    # try to capture dsipper version for the footer
    version = ""
    try:
        import subprocess

        version = (
            subprocess.check_output(["./bin/dsipper", "version"], text=True)
            .strip()
            .replace("dsipper ", "v")
        )
    except Exception:
        version = "vDEV"

    out = (
        HTML_TEMPLATE
        .replace("__SCENES__", "\n".join(parts))
        .replace("__VERSION__", html.escape(version))
    )
    sys.stdout.write(out)


if __name__ == "__main__":
    main(sys.argv[1:])
