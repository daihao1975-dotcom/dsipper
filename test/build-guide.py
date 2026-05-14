#!/usr/bin/env python3
"""
build-guide.py — generate outputs/dsipper-guide.html, a single self-contained
HTML usage manual.  Visual style matches outputs/clui-demo.html (dark, ANSI-
colored code blocks) so the two pages read as a pair.

The page embeds dsipper's own `-h` output for each subcommand so the manual
stays in lockstep with the binary — no risk of doc drift across versions.
"""

from __future__ import annotations

import html
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DSIPPER = ROOT / "bin" / "dsipper"
OUT = ROOT / "outputs" / "dsipper-guide.html"


def run(args: list[str]) -> str:
    try:
        # Pass DSIPPER_NO_COLOR so `-h` output is plain text (we wrap in <pre>)
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


# ── content ──────────────────────────────────────────────────────────────────
# Each (anchor, title, body_html) tuple is one TOC entry.  Body is raw HTML
# already; helpers below build it.


def pre(text: str) -> str:
    return f'<pre class="cli">{html.escape(text.rstrip())}</pre>'


def shell(text: str) -> str:
    return f'<pre class="shell">{html.escape(text.rstrip())}</pre>'


def section(anchor: str, title: str, body: str) -> tuple[str, str, str]:
    return anchor, title, body


def build_sections(version: str) -> list[tuple[str, str, str]]:
    secs: list[tuple[str, str, str]] = []

    # ── 1. quick start ────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 2. options ────────────────────────────────────────────────────────────
    secs.append(
        section(
            "options",
            "2. options — health probe",
            f"""
<p>Send a SIP OPTIONS to an SBC, expect <code>200 OK</code>. Exit 0 on success — drop directly into CI / cron.</p>
{shell('''dsipper options --server sbc.example.com:5060 --transport udp
dsipper options --server sbc.example.com:5061 --transport tls --insecure''')}
{pre(run(["options", "-h"]))}
""",
        )
    )

    # ── 3. register ───────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 4. invite ─────────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 5. listen ─────────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 6. HTML report ────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 7. logs & quiet ───────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 8. cookbook ───────────────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 9. regression matrix ──────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    # ── 10. troubleshooting ───────────────────────────────────────────────────
    secs.append(
        section(
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
        )
    )

    return secs


# ── HTML template ────────────────────────────────────────────────────────────
TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>dsipper — usage guide (__VERSION__)</title>
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
    margin-bottom: 18px;
    font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
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
  pre.shell {
    border-left: 3px solid var(--primary);
  }
  pre.shell::before {
    content: "$";
    color: var(--primary);
    font-weight: 700;
    margin-right: 8px;
    display: none;  /* hidden — shell prompt is implicit in pre.shell color */
  }
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
  <ol>
__TOC__
  </ol>
</nav>
<main>
  <header class="banner">
    <h1>dsipper — usage guide</h1>
    <p class="tagline">SIP / RTP mock client for SBC debugging. Single static binary,
       4 subcommands (options · register · invite · listen), UDP &amp; TLS,
       plain RTP G.711a/u.</p>
    <div class="links">
      <a href="https://github.com/daihao1975-dotcom/dsipper">github</a>
      <a href="https://github.com/daihao1975-dotcom/dsipper/releases">releases</a>
      <a href="https://github.com/daihao1975-dotcom/dsipper/blob/main/CHANGELOG.md">changelog</a>
      <a href="clui-demo.html">CLUI rendering preview</a>
    </div>
  </header>
__BODY__
  <footer class="site">
    generated by test/build-guide.py · dsipper __VERSION__ · embeds live
    <code>-h</code> output so the manual tracks the binary
  </footer>
</main>
</body>
</html>
"""


def main() -> None:
    if not DSIPPER.exists():
        sys.stderr.write(f"binary not found: {DSIPPER} (run `make build` first)\n")
        sys.exit(1)
    version = get_version()
    secs = build_sections(version)

    toc = "\n".join(
        f'    <li><a href="#{a}">{html.escape(t.split(". ", 1)[-1])}</a></li>'
        for a, t, _ in secs
    )
    body = "\n".join(
        f'  <section id="{a}"><h2>{html.escape(t)}</h2>{b}</section>'
        for a, t, b in secs
    )

    page = (
        TEMPLATE
        .replace("__VERSION__", html.escape(version))
        .replace("__TOC__", toc)
        .replace("__BODY__", body)
    )

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(page, encoding="utf-8")
    sys.stderr.write(f"✓ guide → {OUT}\n  size: {OUT.stat().st_size} bytes\n")


if __name__ == "__main__":
    main()
