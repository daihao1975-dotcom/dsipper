#!/usr/bin/env bash
#
# local-loopback.sh — same-host self-test, no SBC required.
#
# Starts dsipper listen on 127.0.0.1:5070, places one short call from
# dsipper invite, prints results, then cleans up. Useful for smoke-testing a
# fresh build (`make build && ./examples/local-loopback.sh`) or for showing
# what the CLUI actually renders without dragging a real SBC into the mix.

set -euo pipefail
cd "$(dirname "$0")/.."

DSIPPER=${DSIPPER:-./bin/dsipper}
[ -x "$DSIPPER" ] || { echo "binary not found: $DSIPPER (run 'make build' first)"; exit 1; }

PORT=5070
DURATION=${DURATION:-3s}

echo "▶ starting listen on 127.0.0.1:$PORT (Ctrl-C to abort)"
$DSIPPER listen --bind 127.0.0.1:$PORT --transport udp --log - >/dev/null 2>/tmp/dsipper-loopback-listen.log &
LISTEN_PID=$!
trap 'kill $LISTEN_PID 2>/dev/null; wait $LISTEN_PID 2>/dev/null; true' EXIT
sleep 0.5

echo "▶ placing call sip:bob@127.0.0.1 (duration $DURATION)"
$DSIPPER invite \
    --server 127.0.0.1:$PORT --transport udp \
    --to sip:bob@127.0.0.1 \
    --duration "$DURATION"

echo
echo "▶ listen side log (tail):"
tail -5 /tmp/dsipper-loopback-listen.log
echo
echo "✓ local loopback complete"
