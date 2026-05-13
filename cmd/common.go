// Package cmd 定义 dsipper 的子命令(register / invite / options / listen)。
// 所有子命令共享一组通用 flag,避免重复参数。
package cmd

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dsipper/internal/clui"
	"dsipper/internal/logsink"
	"dsipper/internal/sipua"
)

// Version 由 main 在 init 时设置,banner 渲染用。
var Version = "dev"

// Quiet 全局静默旗:caller 在解析 --quiet 后置 true → Banner 不打印 logo + config box,
// MakeLogger 也不再向 stderr 打 "log → ..." 提示。CI / 脚本用户友好。
var Quiet = false

// Banner 在 subcmd 入口打印阿屠宰风 logo + 配置摘要 box 到 stderr。
// kvs 已是渲染后的字符串(带色 / 高亮 caller 自己加)。
// Quiet=true 时直接 noop(给 CI / 脚本场景)。
func Banner(subcmd string, kvs []clui.KV) {
	if Quiet {
		return
	}
	fmt.Fprint(os.Stderr, clui.Logo(Version, subcmd))
	fmt.Fprint(os.Stderr, clui.BannerBox(subcmd+" — config", kvs))
}

// defaultStr s 非空返 s,否则返 d(用于 banner 里 fallback "auto"/"off" 等)。
func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// boolStr true→"on" / false→""(空字符串配合 defaultStr 走 fallback)。
func boolStr(b bool) string {
	if b {
		return "on"
	}
	return ""
}

// durStrIfPos d>0 返 d.String();否则返空。
func durStrIfPos(d time.Duration) string {
	if d > 0 {
		return d.String()
	}
	return ""
}

// CommonOpts 是各子命令共享的连接 + 日志参数。
type CommonOpts struct {
	Server      string // host:port
	Transport   string // udp / tls
	Insecure    bool
	CAFile      string
	Verbose     int    // 0=info, 1=debug
	LogFile     string // 落盘日志路径,空=自动 ./dsipper-<cmd>-<YYYYMMDD-HHMMSS>.log,"-"=只 stderr
	LogMaxMB    int    // 单文件 size 上限 MB,达上限滚动到 .old(默认 100,0=不滚)
	LogOnlyFailed bool // 只把失败通的日志落盘(成功通日志直接丢)

	// TLSKeepalive>0 时,TLS transport 启 RFC 5626 双 CRLF ping。建议 30-60s。
	// udp transport 忽略(UDP 无 conn 概念)。
	TLSKeepalive time.Duration

	// PanelMode 表示本次运行会启 LivePanel(invite stress / listen --ui)。
	// 启用时 MakeLogger 不再把日志 tee 到 stderr(只落文件),避免跟面板撞行;
	// 用户仍可 tail -f 日志文件看实时流。caller 在 fs.Parse 之后、MakeLogger 之前设置。
	PanelMode bool

	// BufHandler 引用,recorder 启用后会拿这个挂 LogCtrl(同 cmd 内传递,不暴露给外面)。
	BufHandler *logsink.BufHandler
}

// AttachCommon 把通用 flag 挂到 fs,并返回填好后的 opts(在 fs.Parse 后再读)。
func AttachCommon(fs *flag.FlagSet) *CommonOpts {
	o := &CommonOpts{}
	fs.StringVar(&o.Server, "server", "", "下游 SBC 地址 host:port (必填)")
	fs.StringVar(&o.Transport, "transport", "udp", "传输层: udp 或 tls")
	fs.BoolVar(&o.Insecure, "insecure", true, "TLS 不校验 server 证书 (默认 true,自签场景)")
	fs.StringVar(&o.CAFile, "ca", "", "TLS 校验用 CA 证书 (开严格校验时填,会自动关 --insecure)")
	fs.IntVar(&o.Verbose, "v", 0, "verbose 等级 0=info / 1=debug 含完整 SIP message")
	fs.StringVar(&o.LogFile, "log", "", "日志落盘路径;空=cwd 下 dsipper-<cmd>-<时间戳>.log;'-'=只打 stderr")
	fs.IntVar(&o.LogMaxMB, "log-max-mb", 100, "单日志文件 size 上限 MB,达上限 rename 到 .log.old 重开(0=不滚动)")
	fs.BoolVar(&o.LogOnlyFailed, "log-only-failed", false, "只落失败通日志:含 call-id 的日志先 buffer,呼叫拿到 2xx 时丢弃,>=300 / 退出 pending 时才 flush")
	fs.DurationVar(&o.TLSKeepalive, "tls-keepalive", 0, "TLS 长连接每隔 N 发 \\r\\n\\r\\n 心跳(RFC 5626);0=关。建议 30-60s 防 SBC/NAT 拆链")
	fs.BoolVar(&Quiet, "quiet", false, "静默模式:不打 logo / config box / 'log → ...' 提示;CI 友好")
	return o
}

// MustValidate 检查必填参数,失败直接 exit 2。
func (o *CommonOpts) MustValidate() {
	if o.Server == "" {
		fmt.Fprintln(os.Stderr, "ERR: --server 必填,例如 --server sbc.example.com:5060")
		os.Exit(2)
	}
	if o.Transport != "udp" && o.Transport != "tls" {
		fmt.Fprintln(os.Stderr, "ERR: --transport 只支持 udp / tls")
		os.Exit(2)
	}
	if o.CAFile != "" {
		o.Insecure = false
	}
}

// MakeLogger 按 verbose 配 slog;默认 tee 到 stderr + cwd 下日志文件。
// 文件 sink 走 RotatingFile(100MB 默认上限,rotate 到 .old);可选 BufHandler 包一层,
// 让 --log-only-failed 模式把含 call-id 的 record buffer 起来,recorder 通知 flush/drop。
// 调用方传 subcmd 用于默认文件名。
func (o *CommonOpts) MakeLogger(subcmd string) *slog.Logger {
	return buildLogger(o, subcmd)
}

// buildLogger 是 MakeLogger 的具体实现,listen 子命令也复用(它没走 CommonOpts)。
func buildLogger(o *CommonOpts, subcmd string) *slog.Logger {
	level := slog.LevelInfo
	if o.Verbose >= 1 {
		level = slog.LevelDebug
	}
	var out io.Writer = os.Stderr
	if o.LogFile != "-" {
		path := o.LogFile
		if path == "" {
			ts := time.Now().Format("20060102-150405")
			path = filepath.Join(".", fmt.Sprintf("dsipper-%s-%s.log", subcmd, ts))
		}
		maxBytes := int64(o.LogMaxMB) * 1024 * 1024
		rf, err := logsink.NewRotatingFile(path, maxBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: open log %s: %v (fallback stderr only)\n", path, err)
		} else {
			if !Quiet {
				fmt.Fprintf(os.Stderr, "log → %s (rotate at %d MB)\n", path, o.LogMaxMB)
			}
			if o.PanelMode || Quiet {
				// LivePanel 占 stderr 时日志只落文件;Quiet 模式同理(stderr 静默)
				out = rf
			} else {
				out = io.MultiWriter(os.Stderr, rf)
			}
		}
	}
	inner := slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})
	var h slog.Handler = inner
	if o.LogOnlyFailed {
		bh := logsink.NewBufHandler(inner, true)
		o.BufHandler = bh
		h = bh
	}
	return slog.New(h)
}

// MakeTransport 根据 opts 选择并构造 transport。
// TLS 模式下立刻 dial 一次 server,让 LocalAddr 提前就位(否则 Via/Contact port=0)。
func (o *CommonOpts) MakeTransport() (sipua.Transport, error) {
	switch o.Transport {
	case "udp":
		return sipua.NewUDPClient("")
	case "tls":
		host := o.Server
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		t, err := sipua.NewTLSClient(sipua.TLSOptions{
			ServerName: host,
			Insecure:   o.Insecure,
			CAFile:     o.CAFile,
		})
		if err != nil {
			return nil, err
		}
		dst, err := sipua.ResolveAddr("tls", o.Server)
		if err != nil {
			return nil, fmt.Errorf("resolve server: %w", err)
		}
		if d, ok := t.(sipua.Dialer); ok {
			if err := d.Dial(dst); err != nil {
				return nil, fmt.Errorf("tls connect: %w", err)
			}
		}
		if o.TLSKeepalive > 0 {
			if ka, ok := t.(interface{ EnableKeepalive(time.Duration) }); ok {
				ka.EnableKeepalive(o.TLSKeepalive)
			}
		}
		return t, nil
	}
	return nil, fmt.Errorf("unknown transport: %s", o.Transport)
}
