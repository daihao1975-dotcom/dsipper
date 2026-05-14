# dsipper — SIP / RTP mock client for SBC debugging

[中文版 / Chinese README](README.zh-CN.md)

A single-binary SIP / RTP mock client built for engineers who debug SBCs, signaling
gateways and IVR systems day-to-day. Covers the four daily debugging actions —
**register / probe / call / answer** — over **UDP** or **TLS**, with **plain RTP**
G.711a/G.711u media (no SRTP).

```
┌─────────────────┐  SIP/UDP|TLS  ┌──────────────┐  SIP/UDP|TLS  ┌─────────────────┐
│  dsipper invite │ ────────────► │     SBC      │ ────────────► │ dsipper listen  │
│  (UAC, caller)  │               │ (under test) │               │  (UAS, callee)  │
│  ◄── RTP plain ──────────────────  rtpengine   ──────────────── plain RTP ────►   │
└─────────────────┘                └──────────────┘               └─────────────────┘
```

## Features

- **UDP / TLS / WS / WSS signaling** (RFC 7118 SIP-over-WebSocket) + RTP
  G.711a/u media — **plain RTP** or **SRTP-SDES** (`AES_CM_128_HMAC_SHA1_80`,
  RFC 4568); no DTLS-SRTP yet
- **Five subcommands**: `options` (probe), `register` (Digest auth), `invite`
  (place a real call), `listen` (UAS, answer calls), `scenario` (YAML script
  runner)
- **Built-in load test** — `invite --total N --concurrency M --cps R`
- **DTMF dual mode** — RFC 4733 out-of-band on PT 101 *and* ITU-T Q.23 in-band
  dual tone spliced into the audio stream
- **Re-INVITE / hold** — mid-call SDP direction switching (`sendonly` / `sendrecv`);
  `--update-on-hold` uses **UPDATE (RFC 3311)** instead of re-INVITE
- **PRACK / 100rel (RFC 3262)** — UAC auto-PRACKs reliable provisionals;
  UAS `--reliable-ringing` makes 180 carry `Require: 100rel`
- **TLS keepalive** — RFC 5626 double-CRLF ping
- **HTML signaling report** — failed calls get an inline SVG ladder diagram,
  status pie chart and wall-time histogram on top
- **Live multi-line panel** — progress bar, p50/p95 wall, status distribution,
  ETA — refreshed in place with ANSI cursor moves
- **Failure-only logs** + **100MB rotation** — millions of successful calls
  leave zero disk growth
- Static cross-compile to 6 platforms (darwin/arm64, darwin/amd64,
  linux/amd64, linux/arm64, linux/386, windows/amd64), no cgo

## Install

```sh
# from source (Go 1.21+ required)
git clone https://github.com/daihao1975-dotcom/dsipper.git
cd dsipper && make build
./bin/dsipper --help

# or download a prebuilt binary from the GitHub Releases page
```

## Quick start — same-host loopback

```sh
# Terminal A: UAS that answers any incoming call
./bin/dsipper listen --bind 127.0.0.1:5070 --transport udp

# Terminal B: place one call, save what we received as recv.wav
./bin/dsipper invite --server 127.0.0.1:5070 --transport udp \
                     --to sip:bob@127.0.0.1 --duration 5s
```

## Subcommands

### 1. `options` — health probe

```sh
dsipper options --server SBC_IP:5060 --transport udp
dsipper options --server sbc.example.com:5061 --transport tls
```

Exits 0 on `200 OK`, non-zero otherwise — drop straight into CI / monitoring.

### 2. `register` — REGISTER with Digest auth

```sh
# no auth
dsipper register --server SBC_IP:5060 --transport udp \
                 --user 1000 --domain example.com

# with auth (re-sends on 401 challenge)
dsipper register --server SBC_IP:5060 --transport udp \
                 --user 1000 --pass s3cret --domain example.com --expires 60
```

### 3. `invite` — place a real call with RTP

```sh
# default: 440 Hz sine wave for 10 s, save received RTP as recv.wav
dsipper invite --server SBC_IP:5060 --transport udp \
               --to sip:1001@example.com

# TLS, custom WAV, longer call, save the received audio elsewhere
dsipper invite --server sbc.example.com:5061 --transport tls \
               --to sip:1001@example.com \
               --wav /tmp/prompt.wav --duration 30s --codec PCMU \
               --save-recv /tmp/answer.wav

# Concurrent load test: 300 calls × 12 workers @ 8 cps + HTML report
dsipper invite --server SBC_IP:5060 --transport udp \
               --to sip:1001@example.com --duration 3s --save-recv "" \
               --total 300 --concurrency 12 --cps 8 \
               --report stress.html

# DTMF (RFC 4733 out-of-band + in-band, max compatibility)
dsipper invite --server SBC_IP:5060 --transport udp \
               --to sip:1001@example.com --duration 8s \
               --dtmf "1234#" --dtmf-mode both

# Mid-call hold/resume: hold after 2s, resume after another 3s
dsipper invite --server SBC_IP:5060 --transport udp \
               --to sip:1001@example.com --duration 10s \
               --hold-after 2s --hold-duration 3s
```

On exit it prints something like:

```
OK: call 30s, RTP tx=1500 pkts/258000 B  rx=1500 pkts/258000 B
recv WAV: /tmp/answer.wav
```

WAV files must be **16-bit PCM mono 8 kHz** (telephony standard).

### 4. `listen` — UAS, answer incoming calls

```sh
# UDP
dsipper listen --bind 0.0.0.0:5060 --transport udp \
               --tone 880 --save-recv rx

# TLS server, requires cert + key
dsipper listen --bind 0.0.0.0:5061 --transport tls \
               --cert ./certs/server.crt --key ./certs/server.key \
               --save-recv rx

# UAS actively sends BYE 60s after answering
dsipper listen --bind 0.0.0.0:5060 --transport udp --bye-after 60s

# Live UI panel (multi-line, refreshed at 1 Hz)
dsipper listen --bind 0.0.0.0:5060 --transport udp --ui
```

For each incoming INVITE: `100 Trying → 180 Ringing → 200 OK` with an SDP answer,
880 Hz sine wave is sent back, and the caller's RTP is recorded to `rx-N.wav`
(N is the call sequence number). Receiving a BYE closes RTP immediately and
dumps the WAV; Ctrl-C dumps anything in flight. `listen` also handles
in-dialog **re-INVITE**, mirroring the offered SDP direction
(`sendonly → recvonly`, etc.).

## Common flags (shared by register / invite / options)

| Flag | Meaning | Default |
|---|---|---|
| `--server` | Downstream host:port | (required) |
| `--transport` | `udp` / `tls` | udp |
| `--insecure` | TLS skip server cert verification (self-signed) | true |
| `--ca <file>` | Verify server cert against this CA (auto-disables `--insecure`) | (empty) |
| `-v` | 0 = info / 1 = debug (logs the full SIP message body) | 0 |
| `--log` | Log file path. Empty = auto `dsipper-<cmd>-<timestamp>.log`. `-` = stderr only | (empty) |
| `--log-max-mb` | Max log file size (MB) before rotating to `.log.old`; 0 = no rotation | 100 |
| `--log-only-failed` | Buffer per-call logs; drop on 2xx, flush on ≥300 / process exit | false |
| `--report` | Write an HTML signaling report on exit (dir or `.html` path); empty = off | (empty) |
| `--report-max-failed` | Cap on per-call failed-detail entries in HTML (summary covers all) | 50 |
| `--tls-keepalive` | RFC 5626 double-CRLF ping interval (e.g. `30s`); 0 = off | 0 |

### Strict TLS verification

```sh
dsipper options --server sbc.example.com:5061 --transport tls \
                --ca /etc/ssl/dh-ca.pem
```

Without `--ca`, dsipper defaults to `InsecureSkipVerify` for easier self-signed
debugging.

## Stress mode flags (`invite`)

| Flag | Meaning |
|---|---|
| `--total N` | Total number of calls; `> 1` triggers stress mode |
| `--concurrency M` | Concurrent workers (each owns its own UAC, transport, RTP socket) |
| `--cps R` | Target CPS rate limiter (token bucket); 0 = unbounded |

Stress mode prints a **live multi-line panel** (refreshed at 1 Hz):

```
╭── invite stress ─────────────────────────────────────╮
│ progress   ██████████████████░░░░░░ 6/8  75%         │
│ launched   8  inflight 2  workers 4                  │
│ ok / fail  6 ✓   0 ✗                                 │
│ cps        2.00  target 4.0                          │
│ wall       p50 1.203s   p95 1.205s                   │
│ status     200 ✓ 6                                   │
│ eta        1s  elapsed 3s                            │
╰───────────────────────────────────────────────────────╯
```

A summary box prints after completion, including:

- per-code status distribution (top 6 codes, ≥300 in red, 2xx in green)
- top 3 error reasons with their counts

## DTMF (`invite`)

| Flag | Meaning | Default |
|---|---|---|
| `--dtmf "1234#"` | Digits to send. `0-9`, `*`, `#`, `A`-`D` accepted (case-insensitive) | (empty) |
| `--dtmf-mode` | `rfc4733` (out-of-band PT 101) / `inband` (Q.23 dual tone) / `both` | rfc4733 |
| `--dtmf-delay` | When to start, measured from call setup | 500ms |
| `--dtmf-duration` | Per-digit duration | 120ms |
| `--dtmf-gap` | Silence between digits | 80ms |

RFC 4733 mode sends a properly-paced sequence: first packet with the M-bit,
update packets every 50 ms with growing duration, then three end packets
(`E=1`) for redundancy — shared SSRC with the audio stream, audio is muted
during the event window.

In-band mode synthesises ITU-T Q.23 row/column frequencies (`697/770/852/941`
× `1209/1336/1477/1633` Hz) and splices the dual-tone PCM into the main audio
stream — works against legacy PBXes and ATA endpoints that don't decode RFC 4733.

The receiver side (`listen`) auto-decodes inbound RFC 4733 events and reports
them in `call done` lines (`dtmf=1234#`).

## Re-INVITE / hold (`invite`)

| Flag | Meaning | Default |
|---|---|---|
| `--hold-after` | After N seconds from call setup, send re-INVITE with `a=sendonly` (enter hold) | 0 (off) |
| `--hold-duration` | After M seconds in hold, send re-INVITE with `a=sendrecv` (resume) | 0 (no resume) |

The same dialog (Call-ID, From-tag, to-tag) is reused, CSeq advances, and the
SDP `m=audio` port stays put — only the direction attribute changes. The UAS
side (`listen`) parses the offer, mirrors the direction in the 200 OK answer
(`sendonly → recvonly`, `recvonly → sendonly`, `inactive → inactive`).

## Engineer cookbook

### Verify an SBC is online

```sh
# 1. probe
dsipper options --server $SBC:5060 --transport udp

# 2. register a test number
dsipper register --server $SBC:5060 --transport udp \
                 --user test1000 --pass test --domain $SBC_DOMAIN

# 3. place one call (SBC routes to a downstream UAS)
dsipper invite --server $SBC:5060 --transport udp \
               --to sip:test1001@$SBC_DOMAIN --duration 10s
```

Expecting `RTP tx=500 rx=500` symmetric.

### Debug "signaling works but no audio"

INVITE / 200 OK / ACK all succeed but `rx=0` packets — the SBC isn't forwarding
RTP. Check rtpengine, RTP firewall, SDP `c=` rewrite:

```sh
dsipper invite --server $SBC --transport udp --to sip:test@example.com \
               --duration 5s -v 1 2>&1 | tee call.log
# Compare m=audio in the INVITE vs the 200 OK to confirm the SBC's
# media address rewrite (rtpengine relay mode).
```

### Validate a TLS signaling gateway

```sh
# Trust whatever cert the gateway presents
dsipper options --server tls-edge.example.com:5061 --transport tls --insecure

# Or verify against a real CA
dsipper options --server tls-edge.example.com:5061 --transport tls \
                --ca /path/to/dh-ca.pem
```

### Load-test a deployment

```sh
# 500 calls, 30 workers, 10 cps, 5 s per call, save HTML report
dsipper invite --server $SBC:5060 --transport udp \
               --to sip:test@example.com --duration 5s --save-recv "" \
               --total 500 --concurrency 30 --cps 10 \
               --report run-$(date +%H%M%S).html
```

The HTML report includes:

- per-status pie chart and wall-time histogram across the entire run
- the slowest failed calls with full ladder diagrams (`--report-max-failed N`)
- a top status code table that tolerates dropped detail entries

## Building & cross-compiling

```sh
make build                      # current platform
make cross                      # darwin/arm64, darwin/amd64, linux/amd64, linux/arm64
ls bin/
```

Linux binaries are statically compiled (`CGO_ENABLED=0`); scp to any Linux box
and run — no runtime dependencies.

## Implementation notes

| Module | What's in it |
|---|---|
| `internal/sipua` | SIP message parse/build, Digest auth, UDP + TLS transports with single-stream framing |
| `internal/sdp` | Minimal SDP offer/answer for G.711 + telephone-event, direction handling |
| `internal/media` | G.711 alaw/ulaw codec, RTP session, WAV reader/writer, tone synthesis, DTMF |
| `internal/report` | Per-call recorder, status pie + wall-time histogram, SVG ladder diagrams, HTML template |
| `internal/clui` | ANSI truecolor logo, BannerBox, LivePanel, ProgressBar |
| `internal/logsink` | RotatingFile + BufHandler (slog handler that drops per-call buffers on 2xx) |

## Known boundaries

| Item | Status |
|---|---|
| SRTP / DTLS | Not supported (this tool is intentionally "plain RTP") |
| WebSocket / WSS | Not supported (signaling is UDP / TLS-over-TCP only) |
| Codecs | G.711a / G.711u only (G.729 / Opus out of scope) |
| Concurrent UAC | `invite --total N --concurrency M` covers built-in batch use |
| DTMF | Full RFC 4733 + in-band — see flag table |
| Re-INVITE / hold | Implemented for `a=sendonly`/`sendrecv` mid-call swap |
| TLS keepalive | RFC 5626 double-CRLF ping, see `--tls-keepalive Ns` |
| IPv6 | Untested; the stdlib `net` parts work, but Via construction may need tweaking |

## Repository layout

```
dsipper/
├── main.go                     # subcommand router
├── go.mod / go.sum             # pion/rtp + indirect deps
├── Makefile                    # build / cross / test / fmt
├── README.md                   # this file
├── README.zh-CN.md             # 中文版
├── LICENSE                     # MIT
├── cmd/                        # subcommand implementations
│   ├── common.go
│   ├── options.go
│   ├── register.go
│   ├── invite.go
│   ├── listen.go
│   └── pcap.go
├── internal/
│   ├── sipua/                  # SIP protocol stack
│   ├── sdp/                    # SDP construction & parsing
│   ├── media/                  # RTP / codec / WAV / tone / DTMF
│   ├── report/                 # signaling recorder + HTML report
│   ├── clui/                   # CLI visual layer
│   └── logsink/                # log rotation + failure-only buffer
└── examples/                   # one-shot demo scripts
    ├── options-udp.sh
    ├── register-udp.sh
    ├── invite-tls.sh
    └── listen-uas.sh
```

## License

MIT — see [LICENSE](LICENSE).
