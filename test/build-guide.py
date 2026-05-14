#!/usr/bin/env python3
"""
build-guide.py — generate outputs/dsipper-guide.html (English) and
outputs/dsipper-guide.zh-CN.html (Chinese). Each page is self-contained,
embeds each subcommand's live ``-h`` output, and cross-links to the other
language version via a chip in the TOC header.

Usage:
    python3 test/build-guide.py             # both langs
    python3 test/build-guide.py --lang en   # English only
    python3 test/build-guide.py --lang zh   # Chinese only
"""

from __future__ import annotations

import argparse
import html
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DSIPPER = ROOT / "bin" / "dsipper"
OUT_DIR = ROOT / "outputs"

LANGS = {
    "en":    {"file": "dsipper-guide.html",       "label": "English", "code": "en"},
    "zh-CN": {"file": "dsipper-guide.zh-CN.html", "label": "中文",     "code": "zh-CN"},
}


def run(args: list[str]) -> str:
    try:
        return subprocess.check_output(
            [str(DSIPPER), *args],
            stderr=subprocess.STDOUT,
            text=True,
            timeout=5,
            env={"DSIPPER_NO_COLOR": "1", "PATH": "/usr/bin:/bin"},
        )
    except Exception as e:
        return f"<error: {e}>"


def get_version() -> str:
    try:
        return run(["version"]).strip().replace("dsipper ", "")
    except Exception:
        return "dev"


def pre(text: str) -> str:
    return f'<pre class="cli">{html.escape(text.rstrip())}</pre>'


def shell(text: str) -> str:
    return f'<pre class="shell">{html.escape(text.rstrip())}</pre>'


# ── English sections ─────────────────────────────────────────────────────────


def sections_en() -> list[tuple[str, str, str]]:
    return [
        (
            "quick-start",
            "1. Quick start (loopback, no SBC)",
            f"""
<p>dsipper ships as a single static binary. Drop one terminal on the UAS side,
one on the UAC side — they don't need a real SBC to verify the install.</p>
{shell('''# Terminal A — listen on loopback (default bind is 127.0.0.1:5060)
./bin/dsipper listen --bind 127.0.0.1:5070 --transport udp

# Terminal B — place one 5-second call, save received audio
./bin/dsipper invite --server 127.0.0.1:5070 --transport udp \\
                     --to sip:bob@127.0.0.1 --duration 5s \\
                     --save-recv recv.wav''')}
<p>Or run <code>./examples/local-loopback.sh</code> which scripts both sides + cleanup.</p>
""",
        ),
        (
            "options",
            "2. options — health probe",
            f"""
<p>Send a SIP OPTIONS to an SBC, expect <code>200 OK</code>. Exit 0 on success — drop directly into CI / cron.</p>
{shell('''dsipper options --server sbc.example.com:5060 --transport udp
dsipper options --server sbc.example.com:5061 --transport tls --insecure''')}
{pre(run(["options", "-h"]))}
""",
        ),
        (
            "register",
            "3. register — REGISTER with optional Digest auth",
            f"""
<p>Send REGISTER. With <code>--user/--pass</code>, dsipper handles a 401 challenge automatically and re-sends with the right <code>Authorization</code> header. <code>qop=auth</code> + plain MD5 both supported.</p>
{shell('''# unauthenticated
dsipper register --server SBC:5060 --transport udp \\
                 --user 1000 --domain example.com --expires 60

# Digest auth — works against most carrier-grade registrars
dsipper register --server SBC:5060 --transport udp \\
                 --user 1000 --pass s3cret --domain example.com''')}
{pre(run(["register", "-h"]))}
""",
        ),
        (
            "invite",
            "4. invite — place a real call (+ stress, DTMF, hold)",
            f"""
<p>The workhorse. Places one or many INVITEs over UDP or TLS, negotiates SDP, runs an
RTP session with G.711a/u, optionally drives DTMF (RFC 4733 or in-band) and
re-INVITE direction switching.</p>

<h3>Single call</h3>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 10s

# TLS, custom WAV file, save received audio
dsipper invite --server sbc.example.com:5061 --transport tls \\
               --to sip:1001@example.com \\
               --wav /tmp/prompt.wav --duration 30s --codec PCMU \\
               --save-recv /tmp/answer.wav''')}

<h3>Concurrent load test (stress mode)</h3>
<p><code>--total N</code> &gt; 1 turns on stress mode: <code>N</code> calls spread across
<code>--concurrency M</code> workers, rate-limited to <code>--cps R</code>. Each worker owns
its own transport + UAC + RTP socket so they can't race on a shared inbox.</p>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 3s --save-recv off \\
               --total 300 --concurrency 12 --cps 8 \\
               --report stress.html''')}
<p>While the run is in flight you see a <strong>multi-line LivePanel</strong> redrawn each
second (progress bar, launched/inflight/workers, ok/fail, p50/p95 wall, status
distribution, ETA). When it finishes a summary box prints with the same metrics
plus the top-3 error reasons, then the HTML report lands at the path you gave.</p>

<h3>DTMF</h3>
<p>Two transport modes, choose one or both:</p>
<ul>
  <li><strong>rfc4733</strong> — out-of-band PT 101, the modern way. Correct RFC 4733 timing
  (start packet with M-bit, 50 ms update packets, three end packets with E=1),
  shared SSRC with the audio stream, audio muted during the event.</li>
  <li><strong>inband</strong> — ITU-T Q.23 dual-tone PCM spliced into the main G.711 audio
  stream. Works against legacy PBXes and ATA endpoints that don't decode RFC 4733.</li>
  <li><strong>both</strong> — fire both for maximum compatibility.</li>
</ul>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 8s \\
               --dtmf "1234#" --dtmf-mode both \\
               --dtmf-delay 500ms --dtmf-duration 120ms --dtmf-gap 80ms''')}

<h3>Mid-call hold / resume (re-INVITE)</h3>
{shell('''# After 2 s in the call send re-INVITE with a=sendonly (hold),
# after another 3 s send a=sendrecv (resume). Same dialog, m=audio port unchanged.
dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 10s \\
               --hold-after 2s --hold-duration 3s''')}

<h3>Full flag reference</h3>
{pre(run(["invite", "-h"]))}
""",
        ),
        (
            "listen",
            "5. listen — UAS mode (answer calls)",
            f"""
<p>The other end of a loopback test, or a quick UAS for debugging an SBC's
outbound leg. Defaults to <strong>127.0.0.1:5060</strong> on purpose — public-facing
binds require explicit opt-in.</p>
{shell('''# UDP, save each call's received RTP as rx-1.wav, rx-2.wav, ...
dsipper listen --bind 0.0.0.0:5060 --transport udp \\
               --tone 880 --save-recv rx

# TLS server, requires cert + key
dsipper listen --bind 0.0.0.0:5061 --transport tls \\
               --cert ./certs/server.crt --key ./certs/server.key \\
               --save-recv rx

# UAS sends BYE 60 s after answering (handy for "simulate callee hangup")
dsipper listen --bind 0.0.0.0:5060 --transport udp --bye-after 60s

# Live stats panel (multi-line, refreshed every second)
dsipper listen --bind 0.0.0.0:5060 --transport udp --ui''')}
<p>Per inbound INVITE: <code>100 Trying → 180 Ringing → 200 OK</code> with an SDP answer,
880 Hz sine wave sent back, the caller's RTP recorded to <code>rx-N.wav</code>.
Receiving a BYE closes RTP and dumps the WAV immediately;
Ctrl-C dumps anything in flight. listen also handles in-dialog <strong>re-INVITE</strong>,
mirroring the offered SDP direction (<code>sendonly → recvonly</code>, etc.).</p>
{pre(run(["listen", "-h"]))}
""",
        ),
        (
            "html-report",
            "6. HTML signaling report",
            """
<p>Pass <code>--report path/file.html</code> (or a directory) to either <code>invite</code>
or <code>listen</code>; on exit dsipper writes a self-contained HTML page with:</p>
<ul>
  <li><strong>Status-code pie chart</strong> across the whole run (2xx green gradient,
  ≥300 red gradient, pending gray).</li>
  <li><strong>Wall-time histogram</strong>: 20 linear bins between min and p99 (long
  tail trimmed) with p50/p95 marker lines.</li>
  <li><strong>Status-code table</strong> with the full distribution.</li>
  <li>Per-call <strong>SVG ladder diagrams</strong> for failed / pending calls. Successful
  calls roll into the summary only; the failed-details cap is
  <code>--report-max-failed N</code> (default 50).</li>
</ul>
<p>Stress runs leave the recorder's wall-time samples capped at 100k so memory
stays O(failed calls + tracked samples). Successful calls drop their events on
the 2xx final, only their final-status count survives.</p>
""",
        ),
        (
            "logs-quiet",
            "7. Logs, --quiet, panel mode",
            """
<p>Every subcommand respects three log flags:</p>
<ul>
  <li><code>--log &lt;path&gt;</code> — file sink. Empty (default) auto-names
  <code>dsipper-&lt;cmd&gt;-&lt;timestamp&gt;.log</code> in cwd. Pass <code>-</code> for
  stderr-only.</li>
  <li><code>--log-max-mb 100</code> — single file size cap. On overflow dsipper
  renames it to <code>.log.old</code> (overwriting any prior <code>.old</code>) and reopens
  empty. Disk footprint capped at <code>2 × max</code>.</li>
  <li><code>--log-only-failed</code> — buffer per-call lines, drop on 2xx finals,
  flush only on ≥300 or process exit. Millions of successful calls → zero disk
  growth.</li>
</ul>
<p>The stderr handler is the <strong>colored</strong> one (semantic per-key coloring,
<code>status</code> green for 2xx and red for ≥300, <code>call-id</code> dimmed + truncated,
etc.). The file sink stays plain <code>slog.NewTextHandler</code> output so
<code>grep</code> / <code>awk</code> / <code>jq</code> keep working.</p>
<p><code>--quiet</code> silences the logo, the config box, and the
<code>log → ...</code> notice (CI-friendly). It does <em>not</em> suppress log lines
themselves; if you want stderr quiet too, pair it with <code>--log somefile.log</code>.</p>
<p>When the LivePanel is active (<code>invite</code> stress mode, or
<code>listen --ui</code>) the panel owns stderr — the logger automatically routes
to the file only so log lines never clobber the panel redraw.</p>
""",
        ),
        (
            "cookbook",
            "8. Cookbook",
            f"""
<h3>Verify a fresh SBC deployment</h3>
{shell('''# 1. probe
dsipper options --server $SBC:5060 --transport udp
# expecting: OK: 200 OK

# 2. register
dsipper register --server $SBC:5060 --transport udp \\
                 --user test1000 --pass test --domain $SBC_DOMAIN
# expecting: OK: 200 Registered (auth MD5)

# 3. place one call (SBC routes to downstream UAS)
dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:test1001@$SBC_DOMAIN --duration 10s
# expecting: tx=500 rx=500 symmetric''')}
<p>Or run <code>./examples/check-sbc.sh</code> with <code>SERVER / USER / PASS / DOMAIN / TO</code>
env vars set — it runs the three steps and stops at the first failure.</p>

<h3>Debug "signaling works but no audio"</h3>
{shell('''dsipper invite --server $SBC --transport udp --to sip:test@example.com \\
               --duration 5s -v 1 2>&1 | tee call.log
# Compare m=audio port in the INVITE vs the 200 OK — if the SBC doesn't
# rewrite it, rtpengine isn't seeing the call.''')}

<h3>Load test, save report</h3>
{shell('''dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:test@example.com --duration 5s --save-recv off \\
               --total 500 --concurrency 30 --cps 10 \\
               --report run-$(date +%H%M%S).html''')}

<h3>Long-lived TLS connection survives idle NAT / SBC</h3>
{shell('''dsipper options --server sbc.example.com:5061 --transport tls \\
                --tls-keepalive 30s''')}

<h3>Drive an IVR menu with DTMF</h3>
{shell('''dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:ivr@example.com --duration 20s \\
               --dtmf "1234#" --dtmf-mode both --dtmf-delay 2s''')}
""",
        ),
        (
            "regression",
            "9. Regression matrix",
            f"""
<p><code>test/regression.sh</code> drives all 13 end-to-end black-box cases
(OPTIONS / REGISTER / INVITE / DTMF rfc4733|inband|both / re-INVITE hold /
stress LivePanel / stress combined / all-fail err top / listen --ui / HTML
report / parser fuzz with 113 malformed packets). Each case asserts on log
markers + exit code; failures are listed at the end.</p>
{shell('''# all 13 cases (rebuilds first)
make test-regression

# only specific cases
./test/regression.sh 4 7 11

# keep tmp logs / WAVs for post-mortem
KEEP_LOGS=1 ./test/regression.sh''')}
<p>Use this after every change touching CLI flags, the recorder, the transport
layer, or the SIP state machine.</p>
""",
        ),
        (
            "troubleshoot",
            "10. Troubleshooting",
            """
<table class="trouble">
  <thead><tr><th>Symptom</th><th>Likely cause / fix</th></tr></thead>
  <tbody>
    <tr><td><code>connection refused</code> on listen startup</td>
        <td>Port already taken (default is 127.0.0.1:5060). Use <code>--bind 0.0.0.0:&lt;port&gt;</code> with a free port.</td></tr>
    <tr><td>invite hangs in "TX INVITE" then times out</td>
        <td>UDP packet getting dropped — confirm the SBC accepts your source IP, check rules on intermediate firewalls. Try <code>-v 1</code> to see the raw outgoing message.</td></tr>
    <tr><td>200 OK / ACK go through, RTP rx=0</td>
        <td>SBC isn't forwarding media. Compare the <code>c=</code> / <code>m=audio</code> in the answer to the SBC's media-handling config (rtpengine?). The HTML report's ladder diagram shows the SDP at a glance.</td></tr>
    <tr><td>TLS handshake fails with self-signed cert</td>
        <td>Default <code>--insecure true</code> should work; if it doesn't, the SBC may require SNI / a specific server name. Pass it explicitly as part of <code>--server &lt;host:port&gt;</code> (host is sent as SNI).</td></tr>
    <tr><td>stress mode shows lots of <code>err top: INVITE: timeout waiting for response</code></td>
        <td>Either the SBC is dropping you (rate-limit / 503 backoff) or your local kernel is dropping outbound UDP. Lower <code>--cps</code> and re-test.</td></tr>
    <tr><td>file logs are growing past <code>--log-max-mb</code></td>
        <td>The cap applies to a single file. The old file is renamed to <code>.log.old</code> so the on-disk footprint is bounded by <code>2 × max</code>. If you don't see rotation, check the file path is writable.</td></tr>
    <tr><td>panel and logs are interleaving on stderr</td>
        <td>You pointed <code>--log -</code> (stderr-only) while in panel mode. Either let dsipper pick a file (default) or pass an explicit <code>--log somefile.log</code>.</td></tr>
  </tbody>
</table>
""",
        ),
    ]


# ── 中文 sections ────────────────────────────────────────────────────────────


def sections_zh() -> list[tuple[str, str, str]]:
    return [
        (
            "quick-start",
            "1. 5 分钟上手(loopback,不需要真 SBC)",
            f"""
<p>dsipper 是单个静态 binary。开两个 terminal,一边 UAS,一边 UAC,本机回环就能验证安装。</p>
{shell('''# Terminal A —— listen 监听 loopback(默认就是 127.0.0.1:5060)
./bin/dsipper listen --bind 127.0.0.1:5070 --transport udp

# Terminal B —— 发起 5 秒呼叫,把收到的音频存下来
./bin/dsipper invite --server 127.0.0.1:5070 --transport udp \\
                     --to sip:bob@127.0.0.1 --duration 5s \\
                     --save-recv recv.wav''')}
<p>或者直接跑 <code>./examples/local-loopback.sh</code>,脚本帮你拉起两侧 + 收尾。</p>
""",
        ),
        (
            "options",
            "2. options —— 探活",
            f"""
<p>发一条 SIP OPTIONS,期望 <code>200 OK</code>。成功 exit 0,可直接 drop 进 CI / cron。</p>
{shell('''dsipper options --server sbc.example.com:5060 --transport udp
dsipper options --server sbc.example.com:5061 --transport tls --insecure''')}
{pre(run(["options", "-h"]))}
""",
        ),
        (
            "register",
            "3. register —— REGISTER 带可选 Digest auth",
            f"""
<p>发 REGISTER。带 <code>--user/--pass</code> 时,dsipper 自动处理 401 挑战并用正确的 <code>Authorization</code> 头重发。<code>qop=auth</code> 和裸 MD5 都支持。</p>
{shell('''# 无认证
dsipper register --server SBC:5060 --transport udp \\
                 --user 1000 --domain example.com --expires 60

# Digest auth —— 适配大部分运营商级 registrar
dsipper register --server SBC:5060 --transport udp \\
                 --user 1000 --pass s3cret --domain example.com''')}
{pre(run(["register", "-h"]))}
""",
        ),
        (
            "invite",
            "4. invite —— 主叫一通呼叫(+ 压测、DTMF、保持)",
            f"""
<p>主力子命令。UDP / TLS 发起 1 通或 N 通 INVITE,协商 SDP,跑 G.711a/u RTP 会话,
可选驱动 DTMF(RFC 4733 / 带内)与 re-INVITE direction 切换。</p>

<h3>单通呼叫</h3>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 10s

# TLS,自带 WAV,保存接收的音频
dsipper invite --server sbc.example.com:5061 --transport tls \\
               --to sip:1001@example.com \\
               --wav /tmp/prompt.wav --duration 30s --codec PCMU \\
               --save-recv /tmp/answer.wav''')}

<h3>并发压测(stress mode)</h3>
<p><code>--total N</code> &gt; 1 触发压测模式:<code>N</code> 通呼叫分给 <code>--concurrency M</code> 个
worker,被 <code>--cps R</code> 限速。每 worker 独占自己的 transport + UAC + RTP socket,
不在共享 inbox 上 race。</p>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 3s --save-recv off \\
               --total 300 --concurrency 12 --cps 8 \\
               --report stress.html''')}
<p>跑的时候 stderr 上有<strong>多行 LivePanel</strong> 每秒重绘(进度条、launched/inflight/workers、
ok/fail、p50/p95 wall、status 分布、ETA)。结束时打 summary box,含同一组指标 + 错误 top-3 原因,
HTML 报告落到你给的路径。</p>

<h3>DTMF 双模式</h3>
<p>两种传输方式,任选一个或都开:</p>
<ul>
  <li><strong>rfc4733</strong> —— 带外 PT 101,现代主流。严格 RFC 4733 时序(带 M-bit 的 start 包、
  每 50 ms 一个 update 包、三个带 E=1 的 end 包),与语音共享 SSRC,事件期间语音静音。</li>
  <li><strong>inband</strong> —— ITU-T Q.23 双音 PCM 注入主 G.711 流。老式 PBX / ATA 设备不解
  RFC 4733 时只能走这条。</li>
  <li><strong>both</strong> —— 两路同发,最大兼容。</li>
</ul>
{shell('''dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 8s \\
               --dtmf "1234#" --dtmf-mode both \\
               --dtmf-delay 500ms --dtmf-duration 120ms --dtmf-gap 80ms''')}

<h3>通话期 hold / resume(re-INVITE)</h3>
{shell('''# 通话开始 2 秒后发 re-INVITE w/ a=sendonly(进 hold),
# 再 3 秒后发 a=sendrecv 恢复。同 dialog,m=audio 端口不变。
dsipper invite --server SBC:5060 --transport udp \\
               --to sip:1001@example.com --duration 10s \\
               --hold-after 2s --hold-duration 3s''')}

<h3>完整参数</h3>
{pre(run(["invite", "-h"]))}
""",
        ),
        (
            "listen",
            "5. listen —— UAS 模式(接听)",
            f"""
<p>loopback 测试的对端,或者调试 SBC outbound 一侧时的临时 UAS。默认 bind
<strong>127.0.0.1:5060</strong> —— 想暴露公网必须显式 opt-in 改 0.0.0.0。</p>
{shell('''# UDP,把每通呼叫收到的 RTP 存成 rx-1.wav / rx-2.wav / ...
dsipper listen --bind 0.0.0.0:5060 --transport udp \\
               --tone 880 --save-recv rx

# TLS server,需要 cert + key
dsipper listen --bind 0.0.0.0:5061 --transport tls \\
               --cert ./certs/server.crt --key ./certs/server.key \\
               --save-recv rx

# UAS 接通 60 秒后主动发 BYE(模拟"被叫挂机"场景)
dsipper listen --bind 0.0.0.0:5060 --transport udp --bye-after 60s

# 实时面板(多行 LivePanel,1 Hz 刷新)
dsipper listen --bind 0.0.0.0:5060 --transport udp --ui''')}
<p>每通来电:<code>100 Trying → 180 Ringing → 200 OK</code> + SDP answer,回送 880 Hz 正弦波,
主叫的 RTP 落 <code>rx-N.wav</code>。收到 BYE 立即关 RTP + 落盘;Ctrl-C 退出时把活跃通也落盘。
listen 同样支持 in-dialog <strong>re-INVITE</strong>,会把对端的 SDP direction 镜像回 200 OK
(<code>sendonly → recvonly</code> 等)。</p>
{pre(run(["listen", "-h"]))}
""",
        ),
        (
            "html-report",
            "6. HTML 信令报告",
            """
<p>给 <code>invite</code> 或 <code>listen</code> 传 <code>--report path/file.html</code>(或一个目录),
退出时 dsipper 写一份自包含 HTML,内含:</p>
<ul>
  <li><strong>状态码饼图</strong>(整个 run 的全量,2xx 绿色渐变 / ≥300 红色渐变 /
  pending 灰)。</li>
  <li><strong>wall 时间直方图</strong>:min ~ p99 之间 20 个 linear bin(去掉长尾),
  叠加 p50 / p95 标记线。</li>
  <li><strong>状态码表</strong>(完整分布)。</li>
  <li>失败 / pending 通的<strong>每通 SVG 时序图(ladder diagram)</strong>。成功通只进汇总,
  失败详情上限是 <code>--report-max-failed N</code>(默认 50)。</li>
</ul>
<p>压测时 recorder 的 wall 样本上限是 100k,内存占用 O(失败通 + 已 track 样本)。成功通在拿到
2xx final 时 drop events,只留 final 状态计数。</p>
""",
        ),
        (
            "logs-quiet",
            "7. 日志,--quiet,panel 模式",
            """
<p>每个子命令都有三个日志 flag:</p>
<ul>
  <li><code>--log &lt;path&gt;</code> —— 文件 sink。空(默认)自动落 cwd 下
  <code>dsipper-&lt;cmd&gt;-&lt;时间戳&gt;.log</code>。<code>-</code> 表示只打 stderr。</li>
  <li><code>--log-max-mb 100</code> —— 单文件 size 上限。满了 rename 到 <code>.log.old</code>
  (覆盖旧 <code>.old</code>),磁盘占用上限 <code>2 × max</code>。</li>
  <li><code>--log-only-failed</code> —— 含 call-id 的日志先 buffer,2xx final 时丢弃,
  ≥300 或退出 pending 时才 flush。几千万通成功呼叫磁盘零增长。</li>
</ul>
<p>stderr 走<strong>彩色 handler</strong>(按 key 语义着色:<code>status</code> 2xx 绿 / ≥300 红,
<code>call-id</code> 截短 + dim 等)。文件 sink 保持 plain <code>slog.NewTextHandler</code>,
<code>grep</code> / <code>awk</code> / <code>jq</code> 友好。</p>
<p><code>--quiet</code> 静默 logo、config box 和 <code>log → ...</code> 提示
(CI 友好);<em>不</em>静默业务日志本身。要 stderr 全静默,搭配 <code>--log somefile.log</code>。</p>
<p>LivePanel 激活时(<code>invite</code> 压测,或 <code>listen --ui</code>)panel 独占 stderr ——
logger 自动只写文件,避免业务日志冲掉 panel 重绘。</p>
""",
        ),
        (
            "cookbook",
            "8. Cookbook(真实场景)",
            f"""
<h3>验证新部署 SBC 是否在线</h3>
{shell('''# 1. 探活
dsipper options --server $SBC:5060 --transport udp
# 期望:OK: 200 OK

# 2. 注册
dsipper register --server $SBC:5060 --transport udp \\
                 --user test1000 --pass test --domain $SBC_DOMAIN
# 期望:OK: 200 Registered (auth MD5)

# 3. 主叫一通(SBC 路由到下游 UAS)
dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:test1001@$SBC_DOMAIN --duration 10s
# 期望:tx=500 rx=500 对称''')}
<p>或者跑 <code>./examples/check-sbc.sh</code>,设好 <code>SERVER / USER / PASS / DOMAIN / TO</code>
环境变量,脚本帮你顺序跑三步,撞错就停。</p>

<h3>排查"信令通但没声音"</h3>
{shell('''dsipper invite --server $SBC --transport udp --to sip:test@example.com \\
               --duration 5s -v 1 2>&1 | tee call.log
# 对比 INVITE 跟 200 OK 里的 m=audio port —— 如果 SBC 没改写,
# 说明 rtpengine 根本没看到这通。''')}

<h3>压测 + 出报告</h3>
{shell('''dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:test@example.com --duration 5s --save-recv off \\
               --total 500 --concurrency 30 --cps 10 \\
               --report run-$(date +%H%M%S).html''')}

<h3>TLS 长连接抗 NAT / SBC 空闲拆链</h3>
{shell('''dsipper options --server sbc.example.com:5061 --transport tls \\
                --tls-keepalive 30s''')}

<h3>DTMF 驱动 IVR 菜单</h3>
{shell('''dsipper invite --server $SBC:5060 --transport udp \\
               --to sip:ivr@example.com --duration 20s \\
               --dtmf "1234#" --dtmf-mode both --dtmf-delay 2s''')}
""",
        ),
        (
            "regression",
            "9. 回归测试矩阵",
            f"""
<p><code>test/regression.sh</code> 全量跑 13 个端到端 black-box case
(OPTIONS / REGISTER / INVITE 单通 / DTMF rfc4733|inband|both / re-INVITE hold /
stress LivePanel / stress 联合 / 全失败 err top / listen --ui / HTML 报告 /
parser 113 个畸形包 fuzz)。每个 case 断言 log 标记 + exit code,失败 case 列表在末尾。</p>
{shell('''# 全 13 case(自动先 build)
make test-regression

# 指定 case
./test/regression.sh 4 7 11

# 保留临时 log / WAV 给事后查
KEEP_LOGS=1 ./test/regression.sh''')}
<p>任何改 CLI flag、recorder、transport、SIP state machine 的 commit 后,跑一遍验不回归。</p>
""",
        ),
        (
            "troubleshoot",
            "10. 故障排查",
            """
<table class="trouble">
  <thead><tr><th>症状</th><th>可能原因 / 修法</th></tr></thead>
  <tbody>
    <tr><td>listen 启动报 <code>connection refused</code></td>
        <td>端口被占(默认 127.0.0.1:5060)。换 <code>--bind 0.0.0.0:&lt;port&gt;</code> 一个空端口。</td></tr>
    <tr><td>invite 卡在 "TX INVITE" 然后超时</td>
        <td>UDP 包被丢 —— 确认 SBC 接受你的源 IP,看中间防火墙规则。用 <code>-v 1</code> 看完整原始消息。</td></tr>
    <tr><td>200 OK / ACK 都通,但 RTP rx=0</td>
        <td>SBC 没转 media。对比 SDP answer 里的 <code>c=</code> / <code>m=audio</code> 跟 SBC media
        配置(rtpengine?)。HTML 报告的时序图能一眼看清 SDP。</td></tr>
    <tr><td>自签证书 TLS 握手失败</td>
        <td>默认 <code>--insecure true</code> 应该过。还失败说明 SBC 要求 SNI / 指定 server name,
        把它显式拼到 <code>--server &lt;host:port&gt;</code> 里(host 当 SNI 发)。</td></tr>
    <tr><td>压测大量 <code>err top: INVITE: timeout waiting for response</code></td>
        <td>要么 SBC 在丢你(限速 / 503 backoff)要么本机内核丢出站 UDP。降 <code>--cps</code> 再试。</td></tr>
    <tr><td>文件 log 涨过 <code>--log-max-mb</code></td>
        <td>上限是<strong>单文件</strong>。老文件 rename 到 <code>.log.old</code>,磁盘上限 <code>2 × max</code>。
        没看到滚动 → 检查路径是否可写。</td></tr>
    <tr><td>panel 跟日志在 stderr 上交错</td>
        <td>你在 panel 模式下用了 <code>--log -</code>(stderr-only)。让 dsipper 自动落文件,或者显式给
        <code>--log somefile.log</code>。</td></tr>
  </tbody>
</table>
""",
        ),
    ]


# ── HTML template (lang-agnostic; chrome text comes from i18n dict) ──────────


I18N = {
    "en": {
        "title":   "dsipper — usage guide",
        "tagline": "SIP / RTP mock client for SBC debugging. Single static "
                   "binary, 4 subcommands (options · register · invite · "
                   "listen), UDP & TLS, plain RTP G.711a/u.",
        "links":   ("github", "releases", "changelog", "CLUI rendering preview"),
        "footer":  ('generated by test/build-guide.py · dsipper {ver} · '
                    'embeds live <code>-h</code> output so the manual tracks '
                    'the binary'),
    },
    "zh-CN": {
        "title":   "dsipper —— 使用指南",
        "tagline": "SBC 调试用的 SIP / RTP 模拟客户端。单 binary,4 子命令"
                   "(options · register · invite · listen),UDP & TLS,"
                   "明文 RTP G.711a/u。",
        "links":   ("GitHub", "Releases", "Changelog", "CLUI 渲染预览"),
        "footer":  ('由 test/build-guide.py 生成 · dsipper {ver} · 内嵌实时 '
                    '<code>-h</code> 输出,文档随 binary 同步更新'),
    },
}


TEMPLATE = """<!doctype html>
<html lang="__LANG__">
<head>
<meta charset="utf-8">
<title>__TITLE__ (__VERSION__)</title>
<style>
  :root {
    --bg: #1a1a1a;
    --panel: #0e0e0e;
    --fg: #e0e0e0;
    --fg-muted: #aaa;
    --primary: #1677FF;
    --primary-deep: #0F4FB8;
    --accent: #34C759;
    --warn: #FFCB66;
    --fail: #C00000;
    --hair: #2a2a2a;
    --code-bg: #0a0a0a;
  }
  * { box-sizing: border-box; }
  html { scroll-behavior: smooth; }
  body {
    background: var(--bg);
    color: var(--fg);
    margin: 0;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
                 "Microsoft YaHei", sans-serif;
    line-height: 1.55;
    display: grid;
    grid-template-columns: 240px minmax(0, 1fr);
    min-height: 100vh;
  }
  nav.toc {
    position: sticky;
    top: 0;
    align-self: start;
    height: 100vh;
    overflow-y: auto;
    background: #111;
    border-right: 1px solid var(--hair);
    padding: 24px 18px;
    font-size: 13px;
  }
  nav.toc .brand {
    font-weight: 700;
    color: var(--primary);
    font-size: 16px;
    margin: 0 0 4px;
    letter-spacing: 0.3px;
  }
  nav.toc .ver {
    color: var(--fg-muted);
    font-size: 11px;
    margin-bottom: 8px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
  nav.toc .lang-switch {
    margin-bottom: 18px;
    font-size: 11px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
  nav.toc .lang-switch a {
    color: var(--accent);
    text-decoration: none;
    padding: 2px 6px;
    border: 1px solid var(--hair);
    border-radius: 3px;
    margin-right: 6px;
  }
  nav.toc .lang-switch a.active {
    background: var(--accent);
    color: #0a0a0a;
    border-color: var(--accent);
    font-weight: 700;
  }
  nav.toc ol {
    list-style: none;
    padding: 0;
    margin: 0;
    counter-reset: sec;
  }
  nav.toc li {
    counter-increment: sec;
    margin: 8px 0;
  }
  nav.toc li::before {
    content: counter(sec) ". ";
    color: var(--fg-muted);
    font-family: ui-monospace, monospace;
    font-size: 11px;
    margin-right: 4px;
  }
  nav.toc a {
    color: var(--fg);
    text-decoration: none;
    border-bottom: 1px solid transparent;
    transition: color .15s, border-color .15s;
  }
  nav.toc a:hover {
    color: var(--primary);
    border-bottom-color: var(--primary);
  }
  main {
    padding: 36px 48px 80px;
    max-width: 980px;
  }
  header.banner {
    margin-bottom: 32px;
    padding-bottom: 16px;
    border-bottom: 1px solid var(--hair);
  }
  header.banner h1 {
    color: var(--primary);
    font-size: 28px;
    font-weight: 600;
    margin: 0 0 6px;
    letter-spacing: -0.2px;
  }
  header.banner .tagline {
    color: var(--fg-muted);
    font-size: 14px;
    margin: 0;
  }
  header.banner .links {
    margin-top: 14px;
    font-size: 12px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
  header.banner .links a {
    color: var(--accent);
    text-decoration: none;
    margin-right: 14px;
    border-bottom: 1px dashed transparent;
  }
  header.banner .links a:hover { border-bottom-color: var(--accent); }

  section {
    margin-top: 36px;
    scroll-margin-top: 16px;
  }
  section h2 {
    color: var(--primary);
    font-size: 18px;
    font-weight: 600;
    margin: 0 0 14px;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--hair);
    letter-spacing: 0.2px;
  }
  section h3 {
    color: var(--accent);
    font-size: 14px;
    font-weight: 600;
    margin: 22px 0 8px;
  }
  p { margin: 10px 0; }
  ul { margin: 8px 0 12px; padding-left: 22px; }
  ul li { margin: 4px 0; }
  code {
    background: var(--code-bg);
    color: var(--accent);
    padding: 1px 6px;
    border-radius: 3px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
    font-size: 12.5px;
  }
  strong code { color: var(--warn); }
  pre {
    background: var(--code-bg);
    color: var(--fg);
    padding: 14px 18px;
    border-radius: 6px;
    border: 1px solid var(--hair);
    line-height: 1.4;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
    font-size: 12px;
    white-space: pre;
    overflow-x: auto;
    margin: 8px 0 16px;
  }
  pre.shell { border-left: 3px solid var(--primary); }
  pre.cli {
    border-left: 3px solid var(--fg-muted);
    color: var(--fg-muted);
  }
  table.trouble {
    width: 100%;
    border-collapse: collapse;
    font-size: 12.5px;
    margin: 12px 0;
  }
  table.trouble th, table.trouble td {
    text-align: left;
    padding: 8px 12px;
    border-bottom: 1px solid var(--hair);
    vertical-align: top;
  }
  table.trouble th {
    color: var(--fg-muted);
    font-weight: 600;
    background: #141414;
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }
  table.trouble td:first-child { width: 32%; color: var(--warn); }
  footer.site {
    margin-top: 48px;
    padding-top: 16px;
    border-top: 1px solid var(--hair);
    color: var(--fg-muted);
    font-size: 11px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
  @media (max-width: 900px) {
    body { display: block; }
    nav.toc { position: static; height: auto; border-right: none;
              border-bottom: 1px solid var(--hair); }
    main { padding: 24px 20px 48px; }
  }
</style>
</head>
<body>
<nav class="toc">
  <div class="brand">dsipper</div>
  <div class="ver">__VERSION__</div>
  <div class="lang-switch">__LANG_SWITCH__</div>
  <ol>
__TOC__
  </ol>
</nav>
<main>
  <header class="banner">
    <h1>__TITLE__</h1>
    <p class="tagline">__TAGLINE__</p>
    <div class="links">__LINKS__</div>
  </header>
__BODY__
  <footer class="site">__FOOTER__</footer>
</main>
</body>
</html>
"""


def render(lang: str, version: str) -> str:
    secs = sections_zh() if lang == "zh-CN" else sections_en()
    i18n = I18N[lang]
    lang_code = LANGS[lang]["code"]

    toc = "\n".join(
        f'    <li><a href="#{a}">{html.escape(t.split(". ", 1)[-1])}</a></li>'
        for a, t, _ in secs
    )
    body = "\n".join(
        f'  <section id="{a}"><h2>{html.escape(t)}</h2>{b}</section>'
        for a, t, b in secs
    )

    # Language switcher chips
    chips = []
    for code, meta in LANGS.items():
        cls = "active" if code == lang else ""
        href = meta["file"]
        label = html.escape(meta["label"])
        chips.append(f'<a class="{cls}" href="{href}">{label}</a>')
    lang_switch = "".join(chips)

    # Top right links
    gh = "https://github.com/daihao1975-dotcom/dsipper"
    link_labels = i18n["links"]
    links_html = (
        f'<a href="{gh}">{html.escape(link_labels[0])}</a>'
        f'<a href="{gh}/releases">{html.escape(link_labels[1])}</a>'
        f'<a href="{gh}/blob/main/CHANGELOG.md">{html.escape(link_labels[2])}</a>'
        f'<a href="clui-demo.html">{html.escape(link_labels[3])}</a>'
    )

    page = (
        TEMPLATE
        .replace("__LANG__", lang_code)
        .replace("__VERSION__", html.escape(version))
        .replace("__TITLE__", html.escape(i18n["title"]))
        .replace("__TAGLINE__", html.escape(i18n["tagline"]))
        .replace("__LANG_SWITCH__", lang_switch)
        .replace("__LINKS__", links_html)
        .replace("__TOC__", toc)
        .replace("__BODY__", i18n["footer"].format(ver=html.escape(version)).join([
            body + "\n  ", ""
        ]) if False else body)  # body unchanged
        .replace("__FOOTER__", i18n["footer"].format(ver=html.escape(version)))
    )
    return page


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--lang", choices=["en", "zh", "zh-CN", "all"], default="all",
                        help="Which language(s) to emit. Default: all")
    args = parser.parse_args()

    if not DSIPPER.exists():
        sys.stderr.write(f"binary not found: {DSIPPER} (run `make build` first)\n")
        sys.exit(1)

    version = get_version()
    targets = []
    if args.lang in ("en", "all"):
        targets.append("en")
    if args.lang in ("zh", "zh-CN", "all"):
        targets.append("zh-CN")

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    for lang in targets:
        page = render(lang, version)
        out = OUT_DIR / LANGS[lang]["file"]
        out.write_text(page, encoding="utf-8")
        sys.stderr.write(f"✓ {LANGS[lang]['label']:>8} guide → {out}\n"
                         f"             size: {out.stat().st_size} bytes\n")


if __name__ == "__main__":
    main()
