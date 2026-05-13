package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"dsipper/internal/clui"
	"dsipper/internal/sipua"
)

// Options 子命令:发一个 OPTIONS 探活,期待 200 OK。
func Options(args []string) {
	fs := flag.NewFlagSet("options", flag.ExitOnError)
	co := AttachCommon(fs)
	to := fs.String("to", "", "Request-URI / To header (默认 sip:server)")
	timeout := fs.Duration("timeout", 5*time.Second, "等待响应超时")
	pcapOpts := AttachPcap(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	co.MustValidate()
	log := co.MakeLogger("options")

	Banner("options", []clui.KV{
		{K: "server", V: clui.Bold(clui.Blue(co.Server)) + clui.Slate(" ("+co.Transport+")")},
		{K: "to", V: defaultStr(*to, clui.Dim("auto = sip:"+co.Server))},
		{K: "timeout", V: timeout.String()},
	})

	stopPcap, err := pcapOpts.Start(log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: pcap: %v\n", err)
		os.Exit(1)
	}
	defer stopPcap()

	t, err := co.MakeTransport()
	if err != nil {
		fmt.Fprintf(os.Stderr, "transport: %v\n", err)
		os.Exit(1)
	}
	defer t.Close()

	localIP, err := sipua.PickLocalIP(co.Server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pick local IP: %v\n", err)
		os.Exit(1)
	}
	uac := sipua.NewUAC(t, co.Server, localIP, log)

	ruri := *to
	if ruri == "" {
		ruri = "sip:" + co.Server
	}
	from := fmt.Sprintf("sip:probe@%s", localIP)
	req := uac.BuildRequest("OPTIONS", ruri, from, ruri)
	req.Headers.Add("Contact", uac.LocalContact("probe"))
	req.Headers.Add("Accept", "application/sdp")

	dst, err := uac.ResolveServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Second)
	defer cancel()
	resps, err := uac.SendRequest(ctx, req, dst, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "OPTIONS failed: %v\n", err)
		os.Exit(1)
	}
	final := resps[len(resps)-1]
	if final.StatusCode == 200 {
		fmt.Printf("OK: %d %s\n", final.StatusCode, final.ReasonPhrase)
		os.Exit(0)
	}
	fmt.Printf("FAIL: %d %s\n", final.StatusCode, final.ReasonPhrase)
	os.Exit(1)
}
