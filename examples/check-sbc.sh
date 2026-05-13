#!/usr/bin/env bash
#
# check-sbc.sh — three-step "is this SBC alive" sanity check against a real SBC.
#
# Runs OPTIONS → REGISTER → one short INVITE in sequence, stopping at the
# first failure and printing the failing step's logs to stderr.  Drop into CI
# or run from a tech-support shell to triage "is the SBC accepting traffic at
# all".
#
# Usage:
#   SERVER=sbc.example.com:5060 USER=test1000 PASS=secret \
#   DOMAIN=sbc.example.com TO=sip:1001@sbc.example.com \
#   ./examples/check-sbc.sh

set -euo pipefail
cd "$(dirname "$0")/.."

: "${SERVER:?SERVER required, e.g. SERVER=sbc.example.com:5060}"
TRANSPORT=${TRANSPORT:-udp}
USER_=${USER:-}
PASS_=${PASS:-}
DOMAIN_=${DOMAIN:-${SERVER%:*}}
TO_=${TO:-}

DSIPPER=${DSIPPER:-./bin/dsipper}
[ -x "$DSIPPER" ] || { echo "binary not found: $DSIPPER (run 'make build' first)"; exit 1; }

step() { echo; echo "── $* ──"; }

step "1. OPTIONS probe"
$DSIPPER options --server "$SERVER" --transport "$TRANSPORT" || { echo "✗ OPTIONS failed"; exit 1; }

if [ -n "$USER_" ]; then
    step "2. REGISTER ($USER_@$DOMAIN_)"
    $DSIPPER register --server "$SERVER" --transport "$TRANSPORT" \
        --user "$USER_" --pass "$PASS_" --domain "$DOMAIN_" || { echo "✗ REGISTER failed"; exit 1; }
fi

if [ -n "$TO_" ]; then
    step "3. INVITE → $TO_ (3 s call)"
    $DSIPPER invite --server "$SERVER" --transport "$TRANSPORT" \
        --to "$TO_" --duration 3s --save-recv off || { echo "✗ INVITE failed"; exit 1; }
fi

echo
echo "✓ SBC check passed"
