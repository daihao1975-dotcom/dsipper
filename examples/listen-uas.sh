#!/bin/sh
# UAS 监听 demo。Ctrl-C 退出时自动落 recv-N.wav。
# 用法:
#   ./examples/listen-uas.sh                       # UDP @ 5060
#   TRANSPORT=tls BIND=0.0.0.0:5061 \
#   CERT=./certs/server.crt KEY=./certs/server.key \
#       ./examples/listen-uas.sh
set -e
cd "$(dirname "$0")/.."
TRANSPORT="${TRANSPORT:-udp}"
BIND="${BIND:-0.0.0.0:5060}"
TONE="${TONE:-880}"
SAVE_RECV="${SAVE_RECV:-/tmp/dsipper-uas-rx}"

if [ "$TRANSPORT" = "tls" ]; then
    : "${CERT:?CERT 必填(server 证书路径)}"
    : "${KEY:?KEY 必填(私钥路径)}"
    exec ./bin/dsipper listen --bind "$BIND" --transport tls \
        --cert "$CERT" --key "$KEY" --tone "$TONE" --save-recv "$SAVE_RECV"
fi

exec ./bin/dsipper listen --bind "$BIND" --transport udp \
    --tone "$TONE" --save-recv "$SAVE_RECV"
