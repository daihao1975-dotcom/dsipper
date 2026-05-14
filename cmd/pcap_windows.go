//go:build windows

// pcap_windows.go — Windows stub for the tcpdump-spawn feature.
//
// dsipper's pcap support is implemented via os/exec'ing the host's tcpdump,
// which doesn't exist on Windows. The unix file (cmd/pcap.go) also calls
// syscall.Setpgid / syscall.Kill, both unavailable in the Windows syscall
// package, so the unix implementation can't even compile here. This file
// keeps the same PcapOpts surface so cmd/{invite,listen}.go don't need
// build-tag branches: the flags are accepted but warn + noop.
//
// Windows engineers who need a packet capture should use Wireshark or the
// built-in `pktmon` tool.
package cmd

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type PcapOpts struct {
	Path   string
	Iface  string
	Filter string
}

func AttachPcap(fs *flag.FlagSet) *PcapOpts {
	o := &PcapOpts{}
	fs.StringVar(&o.Path, "pcap", "", "(Windows: not supported — use Wireshark / pktmon)")
	fs.StringVar(&o.Iface, "pcap-iface", "any", "(Windows: ignored)")
	fs.StringVar(&o.Filter, "pcap-filter", "", "(Windows: ignored)")
	return o
}

func (o *PcapOpts) Start(log *slog.Logger) (stop func(), err error) {
	if strings.TrimSpace(o.Path) != "" {
		fmt.Fprintln(os.Stderr,
			"WARN: --pcap is unsupported on Windows (uses tcpdump + POSIX signals). "+
				"Capture externally with Wireshark or `pktmon`.")
	}
	return func() {}, nil
}
