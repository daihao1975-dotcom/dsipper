// pcap.go — 子命令启动时可选 spawn tcpdump,退出时 SIGTERM 收尾,生成 .pcap。
//
// 设计权衡见 README「自动抓包(pcap)」节。要点:
//   - 不破坏 statically linked,纯 exec 外部 tcpdump
//   - 需要宿主装 tcpdump 且具备 CAP_NET_RAW(root 或 setcap)
//   - 默认 -U(packet-buffered)+ -s 0,大流量场景仍可能丢包,但调试足够
package cmd

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// PcapOpts 控制可选的 tcpdump 抓包。
type PcapOpts struct {
	Path   string // 输出 .pcap 路径;空=不抓
	Iface  string // 网卡;默认 any (Linux)
	Filter string // BPF filter,如 "port 5060 or port 5030";空=抓全部
}

// AttachPcap 把抓包 flag 挂到 fs。
func AttachPcap(fs *flag.FlagSet) *PcapOpts {
	o := &PcapOpts{}
	fs.StringVar(&o.Path, "pcap", "", "启动时 spawn tcpdump,把流量写到该 .pcap 文件;空=不抓")
	fs.StringVar(&o.Iface, "pcap-iface", "any", "tcpdump 网卡 (Linux 默认 any)")
	fs.StringVar(&o.Filter, "pcap-filter", "", "BPF filter,如 'port 5060 or port 5030';空=全部")
	return o
}

// Start 在 path 非空时 spawn tcpdump 写 pcap,返回 stop()。
// stop() 幂等,可多次调用;子命令一般 defer stop() 即可。
// 找不到 tcpdump / 权限不足时返回错误,不致命(调用方自行决定是否继续)。
func (o *PcapOpts) Start(log *slog.Logger) (stop func(), err error) {
	if strings.TrimSpace(o.Path) == "" {
		return func() {}, nil
	}
	bin, err := exec.LookPath("tcpdump")
	if err != nil {
		return func() {}, fmt.Errorf("tcpdump 不在 PATH: %w", err)
	}

	args := []string{"-i", o.Iface, "-w", o.Path, "-U", "-s", "0"}
	if f := strings.TrimSpace(o.Filter); f != "" {
		args = append(args, f)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, args...)
	// 自己开 process group,Kill 时能整组拿下(防 tcpdump fork helper 漏掉)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// tcpdump 启动信息走 stderr,合并到 dsipper 自家 stderr 方便看
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return func() {}, fmt.Errorf("start tcpdump: %w", err)
	}

	// 异步 Wait,让 cmd.ProcessState 在子进程退出时立刻可见
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// 等 tcpdump 真正进入抓包状态,避免最早几个包丢
	// tcpdump 启动 listening 一般 < 200ms,400ms 留余量
	select {
	case waitErr := <-done:
		// 启动后立刻死(权限不足 / 网卡不存在 / 文件不可写 / BPF 失败)
		cancel()
		return func() {}, fmt.Errorf("tcpdump 启动后立刻退出 (%v),通常是权限或网卡问题,看上面 tcpdump 的 stderr", waitErr)
	case <-time.After(400 * time.Millisecond):
		// 还在跑,继续
	}
	log.Info("pcap 已启动", "iface", o.Iface, "path", o.Path, "filter", o.Filter, "pid", cmd.Process.Pid)

	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		cancel()
		log.Info("pcap 已停止", "path", o.Path)
	}
	return stop, nil
}
