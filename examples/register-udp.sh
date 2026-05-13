#!/bin/sh
# UDP 带 Digest auth 的 REGISTER demo。
# 用法: SERVER=sbc.example.com:5060 USER=1000 PASS=secret DOMAIN=example.com \
#       ./examples/register-udp.sh
set -e
cd "$(dirname "$0")/.."
SERVER="${SERVER:-127.0.0.1:5060}"
USER="${USER:-test1000}"
PASS="${PASS:-test}"
DOMAIN="${DOMAIN:-127.0.0.1}"
exec ./bin/dsipper register --server "$SERVER" --transport udp \
    --user "$USER" --pass "$PASS" --domain "$DOMAIN" --expires 60
