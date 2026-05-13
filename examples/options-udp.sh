#!/bin/sh
# 探活 demo:UDP OPTIONS 看 SBC 是否在线。
# 用法: SERVER=sbc.example.com:5060 ./examples/options-udp.sh
set -e
cd "$(dirname "$0")/.."
SERVER="${SERVER:-127.0.0.1:5060}"
exec ./bin/dsipper options --server "$SERVER" --transport udp
