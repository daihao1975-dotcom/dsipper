#!/bin/sh
# TLS 信令 + 明文 RTP 主叫 demo。
# 用法: SERVER=tls-edge.example.com:5061 TO=sip:1001@example.com \
#       ./examples/invite-tls.sh
set -e
cd "$(dirname "$0")/.."
SERVER="${SERVER:-127.0.0.1:5061}"
TO="${TO:-sip:bob@127.0.0.1}"
DURATION="${DURATION:-10s}"
CODEC="${CODEC:-PCMA}"
SAVE_RECV="${SAVE_RECV:-/tmp/dsipper-recv.wav}"
exec ./bin/dsipper invite --server "$SERVER" --transport tls \
    --to "$TO" --duration "$DURATION" --codec "$CODEC" \
    --save-recv "$SAVE_RECV" -v 0
