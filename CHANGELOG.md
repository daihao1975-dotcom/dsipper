# Changelog

All notable changes to **dsipper** are documented here. Versions follow
[Semantic Versioning](https://semver.org/) for the public CLI surface
(subcommand names, flag names, output contracts).

## [Unreleased]

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
