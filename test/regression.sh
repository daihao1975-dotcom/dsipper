#!/usr/bin/env bash
# regression.sh — full end-to-end regression for dsipper.
#
# 13 black-box cases covering every public CLI feature, run against a
# loopback UAS/UAC pair. Each case starts its own listen + invite, asserts
# on the result, and prints colored PASS / FAIL.  Exits non-zero if any case
# fails.  Designed to be safe to re-run; no shared state between cases.
#
# Usage:
#   make test-regression
#   ./test/regression.sh           # run all 13 cases
#   ./test/regression.sh 4 7 11    # run only cases 4, 7, 11
#   FAST=1 ./test/regression.sh    # shorter durations where possible
#   KEEP_LOGS=1 ./test/regression.sh   # don't delete /tmp/_dsipper-*.{log,html}
#
# Conventions per case:
#   - listen binds 127.0.0.1:70<XX> (XX = case number, no clash between cases)
#   - log files: /tmp/_dsipper-c<XX>-L.log (listen), /tmp/_dsipper-c<XX>-I.log (invite)
#   - WAVs: /tmp/_dsipper-c<XX>-rx-*.wav
# Cleanup is per-case at the end unless KEEP_LOGS=1.

set -u
cd "$(dirname "$0")/.."

DSIPPER=${DSIPPER:-./bin/dsipper}
[ -x "$DSIPPER" ] || { echo "binary not found: $DSIPPER (run 'make build' first)"; exit 1; }

FAST=${FAST:-0}
KEEP_LOGS=${KEEP_LOGS:-0}

# Force color so the engineer eyeballing local runs gets ANSI in their TTY;
# CI captures stderr to a log file but ANSI is harmless there.
export DSIPPER_FORCE_COLOR=${DSIPPER_FORCE_COLOR:-1}

# ── ANSI helpers ─────────────────────────────────────────────────────────────
P_GREEN='\033[1;38;2;52;199;89m'
P_RED='\033[1;38;2;192;0;0m'
P_BLUE='\033[1;38;2;22;119;255m'
P_DIM='\033[2m'
P_RESET='\033[0m'

pass() { printf "${P_GREEN}✓ %s${P_RESET}\n" "$*"; }
fail() { printf "${P_RED}✗ %s${P_RESET}\n" "$*"; FAILED+=("$CASE"); }
info() { printf "${P_DIM}  %s${P_RESET}\n" "$*"; }
hdr()  { printf "\n${P_BLUE}── Case %s — %s ──${P_RESET}\n" "$1" "$2"; }

FAILED=()
SELECTED=("$@")

case_should_run() {
  if [ ${#SELECTED[@]} -eq 0 ]; then return 0; fi
  for n in "${SELECTED[@]}"; do
    if [ "$n" = "$1" ]; then return 0; fi
  done
  return 1
}

cleanup_case() {
  local n=$1
  if [ "$KEEP_LOGS" = "1" ]; then return; fi
  rm -f /tmp/_dsipper-c${n}-L.log /tmp/_dsipper-c${n}-I.log
  rm -f /tmp/_dsipper-c${n}-rx-1.wav /tmp/_dsipper-c${n}-rx-2.wav /tmp/_dsipper-c${n}-rx-3.wav
  rm -f /tmp/_dsipper-c${n}-r.html /tmp/_dsipper-c${n}-recv.wav
}

start_listen() {
  # $1 = port, $2 = extra args (passed verbatim)
  $DSIPPER listen --bind 127.0.0.1:$1 --transport udp --log /tmp/_dsipper-c${CASE}-L.log $2 \
    >/dev/null 2>/tmp/_dsipper-c${CASE}-L.err &
  LISTEN_PID=$!
  sleep 0.5
}
stop_listen() {
  kill ${LISTEN_PID:-0} 2>/dev/null
  wait ${LISTEN_PID:-0} 2>/dev/null
  LISTEN_PID=0
}
trap 'stop_listen; for n in "${FAILED[@]:-}"; do :; done' EXIT

# ── pre-flight ───────────────────────────────────────────────────────────────
hdr 0 "pre-flight"
if go test -race ./... >/tmp/_pf.log 2>&1; then
  pass "go test -race ./..."
else
  fail "go test -race ./..."
  cat /tmp/_pf.log
fi
if make cross >/tmp/_pf.log 2>&1; then
  pass "make cross (4 platforms)"
else
  fail "make cross"
  cat /tmp/_pf.log
fi
rm -f /tmp/_pf.log

# ── Case 1: OPTIONS probe ────────────────────────────────────────────────────
CASE=1
if case_should_run $CASE; then
  hdr $CASE "OPTIONS probe (UDP)"
  start_listen 7001 ""
  if $DSIPPER options --server 127.0.0.1:7001 --transport udp --quiet --log - >/dev/null 2>&1; then
    pass "options → 200 OK"
  else
    fail "options exit non-zero"
  fi
  stop_listen
  cleanup_case $CASE
fi

# ── Case 2: REGISTER (no registrar, expect 405) ──────────────────────────────
CASE=2
if case_should_run $CASE; then
  hdr $CASE "REGISTER frame round-trip"
  start_listen 7002 ""
  # listen is not a registrar, so it MUST return 405; that's a protocol-correct
  # negative we want to verify the UAC handles cleanly.
  out=$($DSIPPER register --server 127.0.0.1:7002 --transport udp --user u --domain ex.com --quiet 2>&1)
  if echo "$out" | grep -q "FAIL: 405"; then
    pass "register sends frame and parses 405"
  else
    fail "expected FAIL: 405, got: $out"
  fi
  stop_listen
  cleanup_case $CASE
fi

# ── Case 3: single INVITE with sine + RTP symmetry ───────────────────────────
CASE=3
if case_should_run $CASE; then
  hdr $CASE "INVITE single sine 440Hz"
  start_listen 7003 ""
  out=$($DSIPPER invite --server 127.0.0.1:7003 --transport udp \
        --to sip:bob@127.0.0.1 --duration 2s --save-recv off --quiet 2>&1)
  if echo "$out" | grep -qE "OK: call 2s,.*tx=100 pkts.*rx=100 pkts"; then
    pass "tx=100 rx=100 symmetric over 2 s"
  else
    fail "RTP asymmetric or missing: $out"
  fi
  stop_listen
  cleanup_case $CASE
fi

# ── Case 4: DTMF rfc4733 round-trip ──────────────────────────────────────────
CASE=4
if case_should_run $CASE; then
  hdr $CASE "DTMF rfc4733 1234#"
  start_listen 7004 ""
  $DSIPPER invite --server 127.0.0.1:7004 --transport udp \
      --to sip:bob@127.0.0.1 --duration 3s --save-recv off \
      --dtmf "1234#" --dtmf-mode rfc4733 --quiet \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  n=$(grep -c "RX DTMF" /tmp/_dsipper-c${CASE}-L.log)
  if [ "$n" = "5" ]; then
    pass "listen received 5/5 RFC 4733 events (1,2,3,4,#)"
  else
    fail "expected 5 RX DTMF events, got $n"
  fi
  cleanup_case $CASE
fi

# ── Case 5: DTMF inband (PCM splice, no PT 101 on wire) ──────────────────────
CASE=5
if case_should_run $CASE; then
  hdr $CASE "DTMF inband *0#"
  start_listen 7005 ""
  $DSIPPER invite --server 127.0.0.1:7005 --transport udp \
      --to sip:bob@127.0.0.1 --duration 3s --save-recv off \
      --dtmf "*0#" --dtmf-mode inband --dtmf-delay 300ms --quiet \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  n_rfc=$(grep -c "RX DTMF" /tmp/_dsipper-c${CASE}-L.log)
  n_splice=$(grep -c "DTMF inband spliced" /tmp/_dsipper-c${CASE}-I.log)
  if [ "$n_rfc" = "0" ] && [ "$n_splice" = "1" ]; then
    pass "inband splice fired, 0 PT 101 packets on the wire (correct)"
  else
    fail "expected splice=1 rfc=0, got splice=$n_splice rfc=$n_rfc"
  fi
  cleanup_case $CASE
fi

# ── Case 6: DTMF both (out-of-band + inband simultaneously) ──────────────────
CASE=6
if case_should_run $CASE; then
  hdr $CASE "DTMF both 12*9"
  start_listen 7006 ""
  $DSIPPER invite --server 127.0.0.1:7006 --transport udp \
      --to sip:bob@127.0.0.1 --duration 3s --save-recv off \
      --dtmf "12*9" --dtmf-mode both --quiet \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  n_rfc=$(grep -c "RX DTMF" /tmp/_dsipper-c${CASE}-L.log)
  n_splice=$(grep -c "DTMF inband spliced" /tmp/_dsipper-c${CASE}-I.log)
  if [ "$n_rfc" = "4" ] && [ "$n_splice" = "1" ]; then
    pass "both modes fired (splice=1, rfc=4)"
  else
    fail "expected splice=1 rfc=4, got splice=$n_splice rfc=$n_rfc"
  fi
  cleanup_case $CASE
fi

# ── Case 7: re-INVITE hold + resume direction mirroring ──────────────────────
CASE=7
if case_should_run $CASE; then
  hdr $CASE "re-INVITE hold + resume"
  start_listen 7007 ""
  $DSIPPER invite --server 127.0.0.1:7007 --transport udp \
      --to sip:bob@127.0.0.1 --duration 5s --save-recv off \
      --hold-after 1s --hold-duration 1500ms --quiet \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  has_hold=$(grep -c "offer-dir=sendonly answer-dir=recvonly" /tmp/_dsipper-c${CASE}-L.log)
  has_resume=$(grep -c "offer-dir=sendrecv answer-dir=sendrecv" /tmp/_dsipper-c${CASE}-L.log)
  if [ "$has_hold" -ge 1 ] && [ "$has_resume" -ge 1 ]; then
    pass "hold (sendonly→recvonly) + resume (sendrecv→sendrecv) mirrored"
  else
    fail "missing mirror: hold=$has_hold resume=$has_resume"
  fi
  cleanup_case $CASE
fi

# ── Case 8: stress LivePanel basics ──────────────────────────────────────────
CASE=8
if case_should_run $CASE; then
  hdr $CASE "stress 15 calls × 5 workers"
  start_listen 7008 ""
  TOTAL=15; [ "$FAST" = "1" ] && TOTAL=8
  out=$($DSIPPER invite --server 127.0.0.1:7008 --transport udp \
        --to sip:bob@127.0.0.1 --duration 800ms --save-recv off \
        --total $TOTAL --concurrency 5 --cps 3 --quiet \
        --log /tmp/_dsipper-c${CASE}-I.log 2>&1 | tail -20)
  stop_listen
  # final summary box contains "ok  <N> ✓"
  ok=$(echo "$out" | grep -oE "ok[^N]*$TOTAL ✓" | head -1)
  if [ -n "$ok" ]; then
    pass "$TOTAL/$TOTAL ok in stress summary"
  else
    fail "stress summary missing $TOTAL ok"
    echo "$out" | tail -10
  fi
  cleanup_case $CASE
fi

# ── Case 9: stress + DTMF + hold combined ────────────────────────────────────
CASE=9
if case_should_run $CASE; then
  hdr $CASE "stress + DTMF + hold combined"
  start_listen 7009 ""
  $DSIPPER invite --server 127.0.0.1:7009 --transport udp \
      --to sip:bob@127.0.0.1 --duration 4s --save-recv off \
      --total 6 --concurrency 3 --cps 2 \
      --dtmf "789" --dtmf-mode rfc4733 \
      --hold-after 1s --hold-duration 1500ms --quiet \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  re=$(grep -c "re-INVITE RX" /tmp/_dsipper-c${CASE}-L.log)
  dt=$(grep -c "dtmf=789" /tmp/_dsipper-c${CASE}-L.log)
  done=$(grep -c "call done" /tmp/_dsipper-c${CASE}-L.log)
  if [ "$re" = "12" ] && [ "$dt" = "6" ] && [ "$done" = "6" ]; then
    pass "12 re-INVITE + 6 dtmf=789 + 6 call done"
  else
    fail "expected re=12 dt=6 done=6, got re=$re dt=$dt done=$done"
  fi
  cleanup_case $CASE
fi

# ── Case 10: stress all-fail err top-3 ───────────────────────────────────────
CASE=10
if case_should_run $CASE; then
  hdr $CASE "stress all-fail err top-3 rendering"
  # No listen on this port; every INVITE will timeout
  # 注意:`cmd | tail` 在 bash 默认 rc=$? 拿的是 tail 的 exit code。用 PIPESTATUS[0] 拿 invite 的。
  out=$($DSIPPER invite --server 127.0.0.1:7010 --transport udp \
        --to sip:bob@127.0.0.1 --duration 1s --save-recv off --timeout 1s \
        --total 4 --concurrency 2 --cps 4 --quiet 2>&1)
  rc=$?
  out=$(echo "$out" | tail -15)
  if echo "$out" | grep -q "err top" && echo "$out" | grep -qE "fail.*4 ✗"; then
    pass "err top rendered, fail=4"
  else
    fail "missing err top or fail count: $out"
  fi
  if [ $rc -ne 0 ]; then
    pass "exit code non-zero on fail (=$rc)"
  else
    fail "expected non-zero exit, got 0"
  fi
fi

# ── Case 11: listen --ui LivePanel refresh ───────────────────────────────────
CASE=11
if case_should_run $CASE; then
  hdr $CASE "listen --ui LivePanel"
  start_listen 7011 "--ui"
  # capture stderr (panel) into a file we can grep
  mv /tmp/_dsipper-c${CASE}-L.err /tmp/_dsipper-c${CASE}-ui.txt 2>/dev/null || true
  $DSIPPER listen --bind 127.0.0.1:7011 --transport udp --ui --log /tmp/_dsipper-c${CASE}-L.log --quiet \
    >/dev/null 2>/tmp/_dsipper-c${CASE}-ui.txt &
  LISTEN_PID=$!
  sleep 0.5
  for i in 1 2 3; do
    $DSIPPER invite --server 127.0.0.1:7011 --transport udp \
        --to sip:bob@127.0.0.1 --duration 500ms --save-recv off --quiet >/dev/null 2>&1
  done
  sleep 2
  stop_listen
  frames=$(grep -c "listen" /tmp/_dsipper-c${CASE}-ui.txt 2>/dev/null || echo 0)
  if [ "$frames" -ge 3 ]; then
    pass "LivePanel refreshed $frames frames"
  else
    fail "expected ≥ 3 panel frames in stderr, got $frames"
  fi
  rm -f /tmp/_dsipper-c${CASE}-ui.txt
  cleanup_case $CASE
fi

# ── Case 12: HTML report charts ──────────────────────────────────────────────
CASE=12
if case_should_run $CASE; then
  hdr $CASE "HTML report pie + histogram"
  start_listen 7012 ""
  $DSIPPER invite --server 127.0.0.1:7012 --transport udp \
      --to sip:bob@127.0.0.1 --duration 700ms --save-recv off \
      --total 25 --concurrency 5 --cps 0 --quiet \
      --report /tmp/_dsipper-c${CASE}-r.html \
      --log /tmp/_dsipper-c${CASE}-I.log >/dev/null 2>&1
  stop_listen
  has_pie=$(grep -c 'class="pie"' /tmp/_dsipper-c${CASE}-r.html)
  has_hist=$(grep -c 'class="hist"' /tmp/_dsipper-c${CASE}-r.html)
  # grep -oc 在 macOS/BSD 是匹配 lines,Linux GNU 是计数;改用 -o | wc -l 跨平台
  bars=$(grep -o 'class="bar"' /tmp/_dsipper-c${CASE}-r.html | wc -l | tr -d ' ')
  if [ "$has_pie" = "1" ] && [ "$has_hist" = "1" ] && [ "$bars" -ge 10 ]; then
    pass "HTML report has pie + histogram + $bars bar elements"
  else
    fail "missing chart elements: pie=$has_pie hist=$has_hist bars=$bars"
  fi
  cleanup_case $CASE
fi

# ── Case 13: parser fuzz 113 malformed UDP packets ───────────────────────────
CASE=13
if case_should_run $CASE; then
  hdr $CASE "parser fuzz 113 malformed packets"
  start_listen 7013 "-v 1"
  python3 <<'PYEOF' >/dev/null 2>&1
import socket, random
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
tgt = ("127.0.0.1", 7013)
sent = 0
for _ in range(5): s.sendto(b"", tgt); sent += 1
for _ in range(5): s.sendto(b"\x00", tgt); sent += 1
for n in range(1, 20): s.sendto(b"\x00"*n, tgt); sent += 1
for x in [b"INVITE", b"INVITE sip:x@y", b"INVITE sip:x@y SIP/2.0", b"INVITE sip:x@y SIP/2.0\r\n"]: s.sendto(x, tgt); sent += 1
for cl in [100, 1000, 10000, 99999]:
    s.sendto(f"INVITE sip:x@y SIP/2.0\r\nFrom: <sip:a@b>\r\nTo: <sip:c@d>\r\nCall-ID: x\r\nCSeq: 1 INVITE\r\nVia: SIP/2.0/UDP 1.1.1.1\r\nContent-Length: {cl}\r\n\r\n".encode(), tgt); sent += 1
big = b"INVITE sip:x@y SIP/2.0\r\nX-Junk: " + b"A"*8000 + b"\r\nVia: SIP/2.0/UDP 1.1.1.1\r\nFrom: <sip:a@b>\r\nTo: <sip:c@d>\r\nCall-ID: x\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"
s.sendto(big[:1400], tgt); sent += 1
s.sendto(big, tgt); sent += 1
for _ in range(3): s.sendto(b"\r\n\r\n\r\n", tgt); sent += 1
s.sendto(b"INVITE sip:x@y SIP/2.0\r\nFrom: <sip:a@b>\r\n  ;tag=foo\r\nTo: <sip:c@d>\r\nCall-ID: x\r\nCSeq: 1 INVITE\r\nVia: SIP/2.0/UDP 1.1.1.1\r\nContent-Length: 0\r\n\r\n", tgt); sent += 1
random.seed(42)
for n in [10, 50, 100, 500, 1000, 1400]: s.sendto(bytes(random.randint(0,255) for _ in range(n)), tgt); sent += 1
s.sendto("你好世界 SIP".encode("utf-8"), tgt); sent += 1
s.sendto(b"INVITE sip:x@y SIP/2.0\r\nFrom: <sip:a@b>\r\nTo: <sip:c@d>\r\nVia: SIP/2.0/UDP 1.1.1.1\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n", tgt); sent += 1
s.sendto(b"Lorem ipsum dolor sit amet", tgt); sent += 1
for c in [b"-1 INVITE", b"abc INVITE", b"INVITE", b""]:
    s.sendto(b"INVITE sip:x@y SIP/2.0\r\nFrom: <sip:a@b>\r\nTo: <sip:c@d>\r\nCall-ID: x\r\nCSeq: " + c + b"\r\nVia: SIP/2.0/UDP 1.1.1.1\r\nContent-Length: 0\r\n\r\n", tgt); sent += 1
vias = b"".join(b"Via: SIP/2.0/UDP 1.1.1.1;branch=z9hG4bK-x\r\n" for _ in range(10))
s.sendto(b"INVITE sip:x@y SIP/2.0\r\n" + vias + b"From: <sip:a@b>\r\nTo: <sip:c@d>\r\nCall-ID: x\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n", tgt); sent += 1
for x in [b"GET / HTTP/1.1\r\n\r\n", b"HELO mail.example.com\r\n", b"BAD START LINE\r\n\r\n"]: s.sendto(x, tgt); sent += 1
while sent < 113:
    s.sendto(bytes(random.randint(0,255) for _ in range(random.randint(1,100))), tgt); sent += 1
PYEOF
  sleep 1
  # Now verify listen still alive by sending one real INVITE
  if $DSIPPER invite --server 127.0.0.1:7013 --transport udp \
       --to sip:bob@127.0.0.1 --duration 500ms --save-recv off --quiet >/dev/null 2>&1; then
    pass "listen survived 113 malformed packets, real INVITE OK"
  else
    fail "listen broken after fuzz"
  fi
  if grep -qiE "panic|fatal" /tmp/_dsipper-c${CASE}-L.log; then
    fail "panic / fatal in listen log"
  else
    pass "no panic / fatal in log"
  fi
  stop_listen
  cleanup_case $CASE
fi

# ── summary ──────────────────────────────────────────────────────────────────
echo
if [ ${#FAILED[@]} -eq 0 ]; then
  printf "${P_GREEN}══════════════════════════════════════════════════════${P_RESET}\n"
  printf "${P_GREEN}  ✓ ALL CASES PASSED${P_RESET}\n"
  printf "${P_GREEN}══════════════════════════════════════════════════════${P_RESET}\n"
  exit 0
else
  printf "${P_RED}══════════════════════════════════════════════════════${P_RESET}\n"
  printf "${P_RED}  ✗ FAILED CASES: %s${P_RESET}\n" "${FAILED[*]}"
  printf "${P_RED}══════════════════════════════════════════════════════${P_RESET}\n"
  exit 1
fi
