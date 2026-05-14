#!/usr/bin/env bash
#
# render-demo.sh — capture dsipper CLI output and render it as an HTML file
# so reviewers without a terminal (chat / browser) can see the actual color
# rendering instead of raw \x1b[...] escape codes.
#
# Output: outputs/clui-demo.html (auto-opened with `open` on macOS).

set -u
cd "$(dirname "$0")/.."
mkdir -p outputs

DSIPPER=${DSIPPER:-./bin/dsipper}
[ -x "$DSIPPER" ] || { echo "build first: make build"; exit 1; }
export DSIPPER_FORCE_COLOR=1

OUT=outputs/clui-demo.html
TMP=$(mktemp -d -t dsipper-demo.XXXXXX)
trap 'rm -rf "$TMP"' EXIT

# NOTE on shell redirect order: `cmd 2>&1 > file` does NOT capture stderr.
# stderr stays on the original terminal because `2>&1` dups stderr to *current*
# stdout (which is the terminal at that point), then `> file` redirects stdout
# only. Correct order is `cmd > file 2>&1`.

# ── scene 1: banner + config box (just the invite startup) ───────────────────
$DSIPPER listen --bind 127.0.0.1:9001 --transport udp --log "$TMP/L1.log" --quiet 2>/dev/null &
L=$!
sleep 0.5
$DSIPPER invite --server 127.0.0.1:9001 --transport udp \
    --to sip:1001@example.com --duration 1s --save-recv off \
    --log - > "$TMP/scene1.ansi" 2>&1
kill $L 2>/dev/null; wait $L 2>/dev/null

# ── scene 2: colored slog log w/ hold + DTMF + re-INVITE ─────────────────────
$DSIPPER listen --bind 127.0.0.1:9002 --transport udp --log "$TMP/L2.log" --quiet 2>/dev/null &
L=$!
sleep 0.3
$DSIPPER invite --server 127.0.0.1:9002 --transport udp \
    --to sip:1001@example.com --duration 2s --save-recv off \
    --hold-after 800ms --hold-duration 500ms \
    --dtmf "12#" --dtmf-mode rfc4733 --quiet \
    --log - > "$TMP/scene2.ansi" 2>&1
kill $L 2>/dev/null; wait $L 2>/dev/null

# ── scene 3: stress LivePanel + summary box (all ok) ─────────────────────────
$DSIPPER listen --bind 127.0.0.1:9003 --transport udp --log "$TMP/L3.log" --quiet 2>/dev/null &
L=$!
sleep 0.3
$DSIPPER invite --server 127.0.0.1:9003 --transport udp \
    --to sip:bob@127.0.0.1 --duration 800ms --save-recv off \
    --total 12 --concurrency 4 --cps 3 \
    --log "$TMP/I3.log" > "$TMP/scene3.ansi" 2>&1
kill $L 2>/dev/null; wait $L 2>/dev/null

# ── scene 4: stress all-fail → err top full message ──────────────────────────
$DSIPPER invite --server 127.0.0.1:9999 --transport udp \
    --to sip:nobody@127.0.0.1 --duration 500ms --save-recv off --timeout 1s \
    --total 6 --concurrency 3 --cps 6 --log "$TMP/I4.log" > "$TMP/scene4.ansi" 2>&1 || true

# ── ANSI → HTML conversion ───────────────────────────────────────────────────
python3 "$(dirname "$0")/ansi_to_html.py" \
    "$TMP/scene1.ansi" \
    "$TMP/scene2.ansi" \
    "$TMP/scene3.ansi" \
    "$TMP/scene4.ansi" \
    > "$OUT"

echo "✓ rendered → $(pwd)/$OUT"
echo "  size: $(wc -c < "$OUT" | tr -d ' ') bytes"

# auto-open on macOS
if command -v open >/dev/null; then
    open "$OUT"
fi
