package clui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ColorHandler is a slog.Handler that produces compact, color-keyed log lines
// for human consumption on a tty. Designed to replace slog.NewTextHandler for
// stderr-bound sinks; file sinks should keep TextHandler so logs grep cleanly.
//
// Line layout:
//
//	15:04:05.123  INFO  message text          key=value  key=value  call-id=…abc
//	└─ dim time   level  bold message         dim key + colored value
//
// Special keys get semantic colors:
//
//	status  — 2xx green / 3xx-6xx red / 1xx dim / 0 dim
//	err     — value in red
//	call-id — value dimmed + truncated for readability (call-ids are noise)
//	call    — value bold blue (call sequence index in stress / listen)
//	dtmf    — value green (received DTMF digits)
//	dir     — value yellow (SDP a=sendonly/recvonly/sendrecv/inactive)
//	cseq    — value dim
//
// Concurrency: Handle holds an internal mutex around w.Write so concurrent
// loggers don't interleave bytes mid-line. WithAttrs / WithGroup return new
// handlers that share the same mu+w.
type ColorHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
	group string
}

// NewColorHandler builds a handler that writes color-keyed lines to w.
func NewColorHandler(w io.Writer, level slog.Level) *ColorHandler {
	return &ColorHandler{
		mu:    &sync.Mutex{},
		w:     w,
		level: level,
	}
}

func (h *ColorHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *ColorHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	// time — 15:04:05.000 dim
	b.WriteString(Dim(r.Time.Format("15:04:05.000")))
	b.WriteByte(' ')

	// level — INFO green / WARN yellow / ERROR red / DEBUG dim
	b.WriteString(colorLevel(r.Level))
	b.WriteByte(' ')

	// message — bold
	b.WriteString(Bold(r.Message))

	// attrs — handler-level first, then record-level
	for _, a := range h.attrs {
		writeAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write([]byte(b.String()))
	return err
}

func (h *ColorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = slices.Clip(slices.Concat(h.attrs, attrs))
	return &n
}

func (h *ColorHandler) WithGroup(name string) slog.Handler {
	n := *h
	if h.group != "" {
		n.group = h.group + "." + name
	} else {
		n.group = name
	}
	return &n
}

func colorLevel(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return Bold(Red("ERROR"))
	case l >= slog.LevelWarn:
		return Bold(Yellow("WARN "))
	case l >= slog.LevelInfo:
		return Bold(Green("INFO "))
	default:
		return Dim("DEBUG")
	}
}

// writeAttr appends one " key=value" pair to b, with semantic coloring.
// Skip empty values entirely so the line doesn't drag empty " err= " etc.
func writeAttr(b *strings.Builder, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		// flatten one level for readability — dsipper doesn't use deep groups
		for _, sub := range v.Group() {
			writeAttr(b, sub)
		}
		return
	}
	val := v.String()
	// Skip null/empty error values that slog emits as "err=<nil>" / "err="
	if a.Key == "err" && (val == "" || val == "<nil>") {
		return
	}
	b.WriteString("  ")
	b.WriteString(Slate(a.Key))
	b.WriteString(Slate("="))
	b.WriteString(colorValue(a.Key, val))
}

// colorValue picks a color for the value based on its key. Falls back to
// the default fg color (no escape, so terminal default applies).
func colorValue(key, val string) string {
	if val == "" {
		return Dim("\"\"")
	}
	switch key {
	case "status":
		// SIP final status; 2xx ok, ≥300 fail, 0/1xx pending
		if n, err := strconv.Atoi(val); err == nil {
			switch {
			case n >= 200 && n < 300:
				return Bold(Green(val))
			case n >= 300:
				return Bold(Red(val))
			case n == 0:
				return Dim(val)
			default:
				return Yellow(val)
			}
		}
		return val
	case "err":
		return Red(val)
	case "call-id":
		// call-ids are 30-char hex + @host; truncate + dim for skim
		if len(val) > 24 {
			val = val[:8] + "…" + val[len(val)-8:]
		}
		return Dim(val)
	case "call":
		return Bold(Blue(val))
	case "dtmf", "digit", "digits":
		return Green(val)
	case "dir":
		return Yellow(val)
	case "cseq", "from-tag", "to-tag", "branch":
		return Dim(val)
	case "remote-rtp", "local-rtp", "remote", "local":
		return Blue(val)
	case "transport", "codec":
		return Green(val)
	case "duration", "elapsed", "wall", "p50", "p95", "p99", "ts":
		return val // default fg, easy to scan numerically
	default:
		return val
	}
}

// MultiHandler routes one record to multiple sub-handlers.
// dsipper uses it to send the same record to a colored stderr handler and a
// plain-text file handler in parallel.
type MultiHandler struct {
	hs []slog.Handler
}

func NewMultiHandler(hs ...slog.Handler) *MultiHandler {
	return &MultiHandler{hs: hs}
}

func (m *MultiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range m.hs {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Clone — handlers may attach group / attrs;每个 sub-handler 一份独立 record
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	subs := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		subs[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{hs: subs}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	subs := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		subs[i] = h.WithGroup(name)
	}
	return &MultiHandler{hs: subs}
}

// _ keep fmt import used by callers in same package — local refs avoid unused
var _ = fmt.Sprintf
