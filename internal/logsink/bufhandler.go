package logsink

import (
	"context"
	"log/slog"
	"sync"
)

// BufHandler 是 slog.Handler 的 wrapper:
//   - OnlyFailed=false → 完全透传到 inner,跟 inner 等价。
//   - OnlyFailed=true  → 含 call-id attr 的 Record 不直写,先存 per-callid ring;
//     调用 FlushCall(callid) → 把该通累计 records 全部 flush 到 inner;
//     调用 DropCall(callid)  → 直接抛弃缓存。
//     不含 call-id 的 Record(transport 启动、全局警告等)始终直写 inner。
//
// 配合 recorder:呼叫拿到 INVITE final 时,2xx → DropCall;>=300 → FlushCall。
// 进程退出前应当 FlushAll(),把活跃通(没拿到 final)的 buffered records 全落,
// 否则 pending 通的日志丢失。
//
// MaxPerCall 限单通 ring 大小,防止某通卡住引起 buffer 无限增长(默认 500 行)。
// 超出后按 FIFO 丢最早(保留最近 500 行)。
type BufHandler struct {
	inner      slog.Handler
	mu         *sync.Mutex              // 指针:WithAttrs/WithGroup 子 handler 共享同一把锁
	bufs       map[string][]slog.Record // call-id → records (in order),子 handler 共享同一池
	// settled 记录已经 final 的 call:
	//   keep=true  → 失败通 FlushCall 后,后续日志(ACK/BYE 等 dialog 尾巴)直接 inner.Handle 透传
	//   keep=false → 成功通 DropCall 后,后续日志全部丢弃
	settled    map[string]bool
	OnlyFailed bool
	MaxPerCall int
}

// NewBufHandler 包一层 inner;onlyFailed=false 时也可直接 NewBufHandler 用作透传。
func NewBufHandler(inner slog.Handler, onlyFailed bool) *BufHandler {
	return &BufHandler{
		inner:      inner,
		mu:         &sync.Mutex{},
		bufs:       map[string][]slog.Record{},
		settled:    map[string]bool{},
		OnlyFailed: onlyFailed,
		MaxPerCall: 500,
	}
}

// Enabled 透传给 inner。
func (h *BufHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// WithAttrs / WithGroup:为子 logger 创建新 BufHandler 但**共享同一份 bufs**。
// 这样 cmd 那边 logger.With(...) 创建的子 logger 写出的 records 也走同一 buffer 池。
func (h *BufHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &BufHandler{
		inner:      h.inner.WithAttrs(attrs),
		mu:         h.mu,
		bufs:       h.bufs,
		settled:    h.settled,
		OnlyFailed: h.OnlyFailed,
		MaxPerCall: h.MaxPerCall,
	}
}

func (h *BufHandler) WithGroup(name string) slog.Handler {
	return &BufHandler{
		inner:      h.inner.WithGroup(name),
		mu:         h.mu,
		bufs:       h.bufs,
		settled:    h.settled,
		OnlyFailed: h.OnlyFailed,
		MaxPerCall: h.MaxPerCall,
	}
}

// Handle 实现 slog.Handler。
func (h *BufHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.OnlyFailed {
		return h.inner.Handle(ctx, r)
	}
	callid := extractCallID(r)
	if callid == "" {
		// 无呼叫上下文 → 直写
		return h.inner.Handle(ctx, r)
	}
	h.mu.Lock()
	// 已经 settled 的通(成功 drop / 失败 flush 完):
	//   keep=false(成功)→ 后续 dialog 尾巴(ACK/BYE 等)日志全丢
	//   keep=true (失败)→ 直接透传,无需再 buffer
	if keep, ok := h.settled[callid]; ok {
		h.mu.Unlock()
		if keep {
			return h.inner.Handle(ctx, r)
		}
		return nil
	}
	buf := h.bufs[callid]
	if h.MaxPerCall > 0 && len(buf) >= h.MaxPerCall {
		buf = buf[1:] // 丢最早
	}
	buf = append(buf, r.Clone()) // Clone 必须,否则迭代器被复用
	h.bufs[callid] = buf
	h.mu.Unlock()
	return nil
}

// FlushCall 把 callid 的缓存全部送给 inner.Handle,然后清空;
// 同时把 callid 标记为 settled(keep=true),后续同 callid 日志直接透传。
// 失败通走这条路径,日志真正落盘。
func (h *BufHandler) FlushCall(callID string) {
	h.mu.Lock()
	recs := h.bufs[callID]
	delete(h.bufs, callID)
	h.settled[callID] = true
	h.mu.Unlock()
	for _, r := range recs {
		_ = h.inner.Handle(context.Background(), r)
	}
}

// DropCall 直接清空 callid 缓存,标记 settled(keep=false),后续同 callid 日志丢弃。
// 成功通走这条。
func (h *BufHandler) DropCall(callID string) {
	h.mu.Lock()
	delete(h.bufs, callID)
	h.settled[callID] = false
	h.mu.Unlock()
}

// FlushAll 把所有仍在缓存里的通 records 全 flush 出去。
// 进程退出前用来兜底活跃通(没拿到 final)的日志。
func (h *BufHandler) FlushAll() {
	h.mu.Lock()
	bufs := h.bufs
	h.bufs = map[string][]slog.Record{}
	h.mu.Unlock()
	for _, recs := range bufs {
		for _, r := range recs {
			_ = h.inner.Handle(context.Background(), r)
		}
	}
}

// extractCallID 扫一遍 record 的 attrs 找 "call-id";没找到返空。
func extractCallID(r slog.Record) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "call-id" {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}
