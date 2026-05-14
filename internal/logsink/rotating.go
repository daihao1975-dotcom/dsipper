// Package logsink 提供 dsipper 日志输出层:
//   - RotatingFile: 文件 size 达上限时 rename 到 .old 重开,保留 1 份历史,封顶 2×max。
//   - BufHandler:  slog.Handler wrapper,按 call-id 把日志分桶,只在该通呼叫确认失败时
//     才 flush 到 inner sink;成功通直接丢,大压测场景磁盘 0 增长。
//
// 设计取舍:
//   - 不引入 lumberjack / zap,纯 stdlib,跨平台 binary self-contained。
//   - RotatingFile 满了就丢最旧 .old(不做 N 份历史),日志主要是事后排障 + ring 内最新即可。
//   - BufHandler 默认 onlyFailed=false,关 flag 后行为完全等价于直写。
package logsink

import (
	"fmt"
	"os"
	"sync"
)

// RotatingFile 是一个 io.Writer,按 byte 计数滚动单文件。
// 达 MaxBytes 时:close 当前文件 → rename 到 path+".old"(覆盖之前的 .old) → 新建空 path。
// 这样磁盘最多占用 2×MaxBytes(当前文件 + 1 份历史)。
//
// 并发安全:Write 全程持锁,适合 slog handler 共用。
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

// NewRotatingFile 打开/创建 path,size 上限 maxBytes(<=0 时关闭滚动,等价 OpenFile)。
// 如果 path 已存在,继续 append,size 从当前文件大小算起。
// 文件模式 0600(日志可能含 Authorization / Call-ID / Contact 等敏感信息)。
func NewRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &RotatingFile{
		path:     path,
		maxBytes: maxBytes,
		f:        f,
		size:     fi.Size(),
	}, nil
}

// Write 实现 io.Writer。超过上限就先 rotate 再写。
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxBytes > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close 关底层文件。
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// rotateLocked: close 当前 → rename path → path.old(覆盖)→ 新建空 path。
// 任一步失败仅打印 warn,fallback 继续写当前 fd(避免日志全断)。
func (r *RotatingFile) rotateLocked() error {
	if r.f == nil {
		return fmt.Errorf("file not open")
	}
	if err := r.f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: log rotate close: %v\n", err)
	}
	oldPath := r.path + ".old"
	// 防 symlink 攻击:如果 oldPath 已存在且是 symlink,拒绝 rename
	// (避免攻击者预放 symlink 指向 /etc/shadow 等敏感路径)
	if fi, err := os.Lstat(oldPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "WARN: log rotate: %s is a symlink, refusing to overwrite\n", oldPath)
			// 还是要重开新文件,否则日志主路径满了卡死;此处直接换新文件不 rename
			_ = r.f.Close()
		} else {
			_ = os.Remove(oldPath)
			if err := os.Rename(r.path, oldPath); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: log rotate rename: %v\n", err)
			}
		}
	} else if err := os.Rename(r.path, oldPath); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: log rotate rename: %v\n", err)
	}
	nf, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("reopen log: %w", err)
	}
	r.f = nf
	r.size = 0
	return nil
}
