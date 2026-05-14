#!/usr/bin/env bash
#
# pack-linux.sh — build the Linux x86 distribution tarballs.
#
# Output:
#   dist/dsipper-linux-amd64-vX.Y.Z.tar.gz   (64-bit x86 / x86_64)
#   dist/dsipper-linux-386-vX.Y.Z.tar.gz     (32-bit x86)
#   dist/SHA256SUMS.linux                    (checksums for both)
#
# Each tarball expands to a single directory:
#
#   dsipper-linux-<arch>-vX.Y.Z/
#     dsipper                 ← the renamed binary, +x
#     README.md               ← English README
#     README.zh-CN.md         ← 中文 README
#     LICENSE                 ← MIT
#     CHANGELOG.md            ← release history
#     使用说明.txt             ← one-page Chinese quick-start
#
# Idempotent: re-runs overwrite the tarballs in place.

set -euo pipefail
cd "$(dirname "$0")/.."

# Always re-build the host binary + the two linux targets to guarantee they
# match the current Makefile VERSION. Cheap (~3s) and avoids the "packaging
# yesterday's version" foot-gun.
echo "▶ rebuilding host + linux-amd64 + linux-386"
make build build-linux-amd64 build-linux-386 >/dev/null

# Resolve version. The host binary just got rebuilt above, so this is
# authoritative.
VERSION=$(./bin/dsipper version 2>/dev/null | awk '{print $2}')
[ -z "$VERSION" ] && { echo "could not read dsipper version"; exit 1; }

mkdir -p dist
: > dist/SHA256SUMS.linux

for arch in amd64 386; do
    bin="bin/dsipper-linux-$arch"
    pkgname="dsipper-linux-$arch-v$VERSION"
    tarball="dist/$pkgname.tar.gz"

    stage=$(mktemp -d -t dsipper-pack.XXXXXX)
    pkgdir="$stage/$pkgname"
    mkdir -p "$pkgdir"

    install -m 0755 "$bin" "$pkgdir/dsipper"
    cp README.md README.zh-CN.md LICENSE CHANGELOG.md "$pkgdir/"

    cat > "$pkgdir/使用说明.txt" <<EOF
dsipper $VERSION — Linux $arch 分发包
==========================================

dsipper 是给工程师调 SBC / 信令网关用的 SIP / RTP 模拟客户端,
单个静态 binary,无 cgo,无任何运行时依赖。

1. 安装
---------
解压后把 dsipper 放到 PATH 里即可:

    tar -xzf dsipper-linux-$arch-v$VERSION.tar.gz
    cd dsipper-linux-$arch-v$VERSION
    sudo install -m 0755 dsipper /usr/local/bin/

或者直接在解压目录里跑:

    ./dsipper version


2. 4 个子命令速查
------------------
options   探活,发 SIP OPTIONS,期待 200 OK
register  REGISTER 注册,支持 Digest auth
invite    主叫一通真实呼叫,带 RTP;-total/-concurrency/-cps 触发并发压测
listen    UAS 模式,接听并回 880Hz 正弦波


3. 5 个典型调用
----------------
# 探活
dsipper options --server sbc.example.com:5060 --transport udp

# 单通真实呼叫,5 秒,保存收到的音频
dsipper invite --server sbc.example.com:5060 --transport udp \\
               --to sip:1001@example.com --duration 5s \\
               --save-recv recv.wav

# 并发压测 300 通,12 worker,8 cps,出 HTML 报告
dsipper invite --server sbc.example.com:5060 --transport udp \\
               --to sip:1001@example.com --duration 3s --save-recv off \\
               --total 300 --concurrency 12 --cps 8 \\
               --report stress.html

# DTMF(带外 RFC 4733 + 带内双音 同发)
dsipper invite --server sbc.example.com:5060 --transport udp \\
               --to sip:ivr@example.com --duration 8s \\
               --dtmf "1234#" --dtmf-mode both

# UAS 接听(默认 bind loopback;暴露公网用 0.0.0.0)
dsipper listen --bind 0.0.0.0:5060 --transport udp --ui


4. 常用 flag
-------------
--server         SBC 地址 host:port  (必填)
--transport      udp / tls            (默认 udp)
--insecure       TLS 不校验自签证书   (默认 true)
--log            日志路径              (默认 cwd 下自动命名 .log)
--log-only-failed 只保留失败通日志,几千万通成功磁盘 0 增长
--log-max-mb     单日志文件 size 上限  (默认 100MB,满了滚动)
--report         退出落 HTML 信令报告 (含状态码饼图 + wall time 直方图)
--quiet          静默 banner + 启动提示


5. 完整文档
-----------
中文使用指南(可在浏览器打开,左侧 sidebar 切换语言)、
完整 flag 参考、Cookbook、故障排查 等见:

    GitHub:        https://github.com/daihao1975-dotcom/dsipper
    Release page:  https://github.com/daihao1975-dotcom/dsipper/releases
    Changelog:     CHANGELOG.md (本目录)


6. 自检
--------
解压后立即跑一遍同机回环 self-test 验证安装:

    ./dsipper listen --bind 127.0.0.1:5070 --transport udp &
    ./dsipper invite --server 127.0.0.1:5070 --transport udp \\
                     --to sip:bob@127.0.0.1 --duration 3s
    # 期望:OK: call 3s, RTP tx=150 rx=150 对称


许可证:MIT(LICENSE 文件)
EOF

    # Portable tar: GNU --owner/--group differ from BSD; pass nothing and let
    # the system's tar pick defaults. dsipper is fine running as $USER on the
    # target box, no special perms required.
    tar -czf "$tarball" -C "$stage" "$pkgname"
    rm -rf "$stage"

    # Pretty size print (portable: BSD stat -f%z, GNU stat -c%s)
    sz=$(stat -f%z "$tarball" 2>/dev/null || stat -c%s "$tarball")
    printf "✓ %-44s %6.1f MB\n" "$tarball" "$(awk -v s=$sz 'BEGIN{printf "%.1f", s/1024/1024}')"

    (cd dist && shasum -a 256 "$pkgname.tar.gz") >> dist/SHA256SUMS.linux
done

echo ""
echo "── dist/ ─────────────────────────────────"
ls -lh dist/
echo "── SHA256SUMS.linux ──────────────────────"
cat dist/SHA256SUMS.linux
