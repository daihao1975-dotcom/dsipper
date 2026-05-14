# Changelog

All notable changes to **dsipper** are documented here. Versions follow
[Semantic Versioning](https://semver.org/) for the public CLI surface
(subcommand names, flag names, output contracts).

## [Unreleased]

### Added
- **PRACK / 100rel (RFC 3262)** —
  - UAC: `--enable-100rel` (default **on**) advertises `Supported: 100rel`
    on every INVITE and auto-sends `PRACK` for any 1xx that carries
    `Require: 100rel` + `RSeq`. The `RAck` header echoes
    `<RSeq> <orig CSeq num> <orig method>`. `--require-100rel` adds
    `Require: 100rel` so the SBC is *forced* to use reliable provisionals
    (non-supporters reject with `420 Bad Extension`).
  - UAS (`listen`): `--reliable-ringing` makes 180 Ringing carry
    `Require: 100rel` + `RSeq: 1` so you can verify a UAC's PRACK
    implementation. Inbound PRACK gets `200 OK`.
  - `sipua.UAC.PRACKAuto` + `SendRequest` filters CSeq method so PRACK's
    own 2xx doesn't get mistaken for the original transaction's final.
- **UPDATE (RFC 3311)** — both directions:
  - UAC: `--update-on-hold` makes hold/resume use UPDATE instead of
    re-INVITE (some IMS / Lync SBCs prefer UPDATE for mid-dialog direction
    changes; no ACK needed since UPDATE is transaction-self-contained).
  - UAS (`listen`): inbound UPDATE with SDP body gets a 200 OK with
    mirrored-direction answer SDP; empty-body UPDATE gets a bare 200 OK.
- **SIP-over-WebSocket transport (RFC 7118)** — `--transport ws` and
  `--transport wss`, supported by `invite`, `register`, `options`, and
  `listen`. Sec-WebSocket-Protocol is `sip`. URL path defaults to `/`,
  override with `--ws-path /sip`. WSS server takes the same `--cert /
  --key` as TLS. Uses `nhooyr.io/websocket`.
- **`scenario` subcommand** — runs a YAML script of steps (`options /
  register / invite / sleep / log`) sequentially, each step asserts on a
  status code via `expect:`. `default:` block fills server / transport /
  timeout for every step; `continue-on-fail: true` keeps going past a
  red. `--dry-run` prints the resolved steps without sending. See
  `examples/scenario-basic.yaml`.
- **SRTP-SDES media encryption (RFC 4568)** — `--srtp` on `invite`
  switches the m= profile to `RTP/SAVP`, adds
  `a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<base64>` to the offer,
  installs `pion/srtp/v3` AES-CM-128 + HMAC-SHA1-80 contexts on send/recv,
  and aborts the call cleanly (ACK + BYE) if the answer is missing
  `a=crypto`. `listen` auto-mirrors: if the offer has a SDES
  `AES_CM_128_HMAC_SHA1_80` crypto line, the UAS generates its own key,
  installs the SRTP contexts, and echoes its `a=crypto` in the 200 OK.
  Outbound packets carry the 10-byte HMAC-SHA1-80 auth tag (visible in
  the byte counts: 172 B → 182 B per G.711 packet).

### Tests
- Regression suite grows to **20 cases**: Case 16 (PRACK/100rel),
  17 (UPDATE-on-hold replaces re-INVITE), 18 (ws transport), 19
  (scenario YAML 3-step), 20 (SRTP roundtrip with auth-tag overhead).

## [v0.11.3] — 2026-05-14

### Fixed
- **Stress mode dropped `--report` HTML on any failure.** `runStress`
  called `os.Exit(1)` directly when at least one call failed,
  bypassing the parent function's `defer saveReport()`. Result: the
  HTML report was silently lost in exactly the runs where it mattered
  most (mixed pass/fail, all-fail). All-OK stress runs still wrote it.
  Now `runStress` returns a `bool` and the caller saves the report
  before exiting non-zero. Locked in by regression Case 14.
- **TLS keepalive fired a `malformed request line` WARN every interval.**
  `tryExtractSIPFrame` correctly recognized the RFC 5626 double-CRLF
  heartbeat and returned it as a 2-byte frame, but `readConn` forwarded
  it to the SIP parser, which logged a noisy WARN every keepalive tick.
  Now `readConn` swallows ≤4-byte CRLF-only frames before the inbox
  dispatch. Locked in by regression Case 15.

### Tests
- Regression suite is now **15 cases** (was 13). New Case 14 verifies
  the stress-mode report-on-failure fix; new Case 15 verifies the
  keepalive WARN suppression. Full suite: 20 ✓ / 0 ✗.

## [v0.11.2] — 2026-05-14

### Added
- **Windows x86_64 build** — `bin/dsipper-windows-amd64.exe`. Same feature
  set as the Unix builds *minus* `--pcap` (which spawns `tcpdump` and uses
  POSIX `Setpgid` / `Kill` — neither available on Windows). The flag stays
  visible for CLI consistency but prints a warning + noops; capture
  externally with Wireshark or `pktmon`.
- **Linux 386 (32-bit x86)** build — `bin/dsipper-linux-386`. Same feature
  set as `linux-amd64`. For embedded boxes and ancient SBC management
  hosts still running 32-bit kernels.
- `cmd/pcap_windows.go` stub — keeps the `PcapOpts` interface intact on
  Windows so cmd/{invite,listen}.go don't need build-tag branches; the
  Unix file gained `//go:build !windows`.
- Release workflow now ships **6 binaries** (was 4) + SHA256SUMS.

### Notes
- "Windows x86" = `dsipper-windows-amd64.exe` (64-bit). 32-bit Windows
  isn't built — practically all modern Windows installs are amd64.
- `make cross` now produces 6 artifacts instead of 4.

## [v0.11.1] — 2026-05-14

### Fixed
- `--quiet` was incorrectly disabling the colored stderr slog handler,
  forcing logs back to plain `slog.NewTextHandler` output when paired
  with `--log -`. `Quiet` now only silences the banner and the
  `log → ...` notice (its actual intent); colored log lines are
  preserved regardless of the flag.

## [v0.11.0] — 2026-05-14

### Added
- **Colored slog handler** (`clui.NewColorHandler`) — every stderr log line
  now reads at a glance: dim time, color-coded level, bold message, dim
  `key=` + semantically colored value. `status` is green for 2xx and red
  for ≥300; `call-id` gets truncated + dimmed since it's mostly noise;
  `err` shows red; `call`, `codec`, `dir`, `remote-rtp` are each colored
  to their role. File sinks still get plain `slog.NewTextHandler` output
  so `grep` / `awk` keep working.
- `clui.NewMultiHandler` — fan-out helper that routes a record to multiple
  sub-handlers (colored stderr + plain file in dsipper's case).
- **Long status / err lists wrap inside the summary box** instead of being
  truncated. `statusDistLines(st, 6)` returns N lines of up to 6 codes
  each (descending count); `topErrorLines(m, 3)` returns one line per
  error reason with no string truncation.
- **`test/regression.sh`** — 13-case end-to-end black-box regression
  matrix (OPTIONS / REGISTER / INVITE single / DTMF rfc4733|inband|both /
  re-INVITE hold / stress LivePanel / stress combined / all-fail err top /
  listen --ui / HTML chart / parser fuzz 113 malformed packets). Each
  case asserts on log markers + exit code; failure list summary at end.
  Hook: `make test-regression` (builds first, then runs).

### Changed
- `buildLogger` (common) and `co` (listen) now build separate stderr +
  file handlers and combine them with `MultiHandler` — replaces the old
  `io.MultiWriter` trick that forced both sinks to share an encoding.
- `statusDistLine` and `topErrors` kept as single-line wrappers for
  LivePanel use; their `*Lines` siblings power the stress summary box.

## [v0.10.0] — 2026-05-14

### Added
- `dsipper --version` and `dsipper version` subcommand
- `--quiet` flag on every subcommand (silences logo, config box, and the
  `log → ...` notice; great for CI / scripts)
- `--save-recv off` (and `none`) as explicit synonyms for the empty-string
  "don't save" semantics
- `examples/local-loopback.sh` — one-shot same-host self-test
- `examples/check-sbc.sh` — three-step sanity check against a real SBC
  (OPTIONS → REGISTER → INVITE, stops on first failure)
- GitHub Actions CI: pushes a tag → 4-platform static build + signed
  checksums + auto-attached release artifacts

### Changed
- `dsipper invite --save-recv` default changed from `"recv.wav"` to `""`
  (no save). Pass an explicit path to opt in. Avoids cwd pollution and
  surprise overwrites between runs.
- `dsipper listen --bind` default changed from `0.0.0.0:5060` to
  `127.0.0.1:5060`. Public-facing UAS usage now needs an explicit
  `--bind 0.0.0.0:5060`.

## [v0.9.0] — 2026-05-13

### Added
- **Re-INVITE / hold** (RFC 3261 §14): `invite --hold-after Ns
  --hold-duration Ms` sends `a=sendonly` mid-call, then `a=sendrecv` to
  resume. `listen` mirrors direction in the 200 OK answer
  (sendonly→recvonly, recvonly→sendonly, inactive→inactive).
- **HTML report charts**: status pie chart + wall-time histogram with
  p50/p95 marker lines, all inline SVG. Recorder caps wall samples at
  100 k to keep stress runs bounded.
- SDP `MediaDirection` constants + `MirrorDirection()` helper.
- English README; the original Chinese README moves to `README.zh-CN.md`.

## [v0.8.0] — 2026-05-13

### Added
- **`LivePanel`** — multi-line ANSI panel (`clui` package). Replaces the
  single-line `LiveStats` and is shared between `invite` stress mode and
  `listen --ui`.
- `clui.ProgressBar(cur, total, width)` helper.
- Stress summary box now shows status-code distribution + top-3 error
  reasons with counts.

### Changed
- When `LivePanel` is active the logger no longer tees to stderr —
  prevents `slog` lines from clobbering the panel redraw.
- `Recorder.Snapshot()` now exposes the full status-code map.

## [v0.7.0] — 2026-05-13

### Added
- **DTMF dual mode**:
  - `rfc4733` (out-of-band, PT 101): proper RFC 4733 timing — start packet
    with M-bit, update packets every 50 ms, three end packets (`E=1`),
    shared SSRC, main audio muted during the event.
  - `inband` (ITU-T Q.23 dual tone): the row + column frequencies are
    synthesised and spliced into the main G.711 audio stream.
  - `both`: send both for maximum compatibility.
  - Flags: `--dtmf "1234#"`, `--dtmf-mode`, `--dtmf-delay`,
    `--dtmf-duration`, `--dtmf-gap`.
  - Receive-side detection: `listen` auto-decodes inbound RFC 4733 events
    and surfaces them in the `call done` line as `dtmf=...`.
- **TLS keepalive**: `--tls-keepalive 30s` (RFC 5626 double-CRLF ping) on
  long-lived TLS connections.

## [v0.6.0] — 2026-05-13

### Added
- **Stress mode** for `invite`:
  - `--total N --concurrency M --cps R`
  - Each worker owns its own UAC + transport + RTP socket (no fan-out
    races on a shared inbox channel).
  - Optional shared `Recorder` aggregates a single HTML report covering
    every call.
  - Summary box prints ok / fail / elapsed / actual cps / p50 / p95 wall
    times.

## [v0.5.0] — 2026-05-13

### Added
- **HTML signaling report**: `--report <dir>` writes a recorded
  per-call SVG ladder diagram on exit. Failed calls get full detail
  (capped by `--report-max-failed`); successful calls roll into the
  top-level status table only.
- **Failure-only logs** (`--log-only-failed`): per-call log lines are
  buffered, dropped on 2xx finals, flushed on ≥300 or process exit.
  Millions of successful calls leave zero disk growth.
- **Log rotation**: `--log-max-mb 100` (default) renames the file to
  `.log.old` when full; total on-disk usage capped at `2 × max`.
- **CLUI** package: ANSI truecolor logo, BannerBox config card, and the
  original single-line LiveStats panel.

## [v0.4.0] — 2026-05-12

### Changed
- Bumped UDP / TLS inbox capacity from 32 to 8192; default kernel
  SO_RCVBUF to 16 MB. Survives sub-second burst arrivals without
  drops on the wire.

## Earlier history

`v0.3.x` and `v0.4.x` were developed inside the `dh-ss` monorepo under
the working name `sipmock`. The public history of dsipper begins at
the v0.8.0 fresh init commit.
