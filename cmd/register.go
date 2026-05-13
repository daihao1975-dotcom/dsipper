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

// Register 子命令:支持带 401/407 challenge 的 Digest 认证。
func Register(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	co := AttachCommon(fs)
	user := fs.String("user", "", "AOR 用户名 (必填)")
	pass := fs.String("pass", "", "Digest 密码")
	domain := fs.String("domain", "", "AOR 域 (默认 = server host)")
	expires := fs.Int("expires", 60, "Expires 秒")
	timeout := fs.Duration("timeout", 8*time.Second, "单事务超时")
	pcapOpts := AttachPcap(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	co.MustValidate()
	if *user == "" {
		fmt.Fprintln(os.Stderr, "ERR: --user 必填")
		os.Exit(2)
	}
	log := co.MakeLogger("register")

	Banner("register", []clui.KV{
		{K: "server", V: clui.Bold(clui.Blue(co.Server)) + clui.Slate(" ("+co.Transport+")")},
		{K: "user", V: clui.Bold(*user)},
		{K: "domain", V: defaultStr(*domain, clui.Dim("auto"))},
		{K: "auth", V: defaultStr(boolStr(*pass != ""), clui.Dim("none"))},
		{K: "expires", V: fmt.Sprintf("%ds", *expires)},
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

	dom := *domain
	if dom == "" {
		host := co.Server
		for i, ch := range host {
			if ch == ':' {
				host = host[:i]
				break
			}
		}
		dom = host
	}
	aor := fmt.Sprintf("sip:%s@%s", *user, dom)

	uac := sipua.NewUAC(t, co.Server, localIP, log)
	uac.FromTag = sipua.Branch()[len("z9hG4bK-"):]

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*3+time.Second)
	defer cancel()

	dst, err := uac.ResolveServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	build := func(authHeader, authHeaderName string) *sipua.Message {
		req := uac.BuildRequest("REGISTER", "sip:"+dom, aor, aor)
		req.Headers.Add("Contact", uac.LocalContact(*user))
		req.Headers.Add("Expires", fmt.Sprintf("%d", *expires))
		req.Headers.Add("Allow", "INVITE,ACK,CANCEL,BYE,OPTIONS")
		if authHeader != "" {
			req.Headers.Add(authHeaderName, authHeader)
		}
		return req
	}

	// 第一次:不带 Authorization
	resps, err := uac.SendRequest(ctx, build("", ""), dst, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REGISTER 1: %v\n", err)
		os.Exit(1)
	}
	final := resps[len(resps)-1]

	switch final.StatusCode {
	case 200:
		fmt.Printf("OK: 200 Registered (no auth required)\n")
		return
	case 401, 407:
		// 带 challenge 重发
		challengeHdr := "WWW-Authenticate"
		respHdr := "Authorization"
		if final.StatusCode == 407 {
			challengeHdr = "Proxy-Authenticate"
			respHdr = "Proxy-Authorization"
		}
		ch, err := sipua.ParseDigestChallenge(final.Headers.Get(challengeHdr))
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse challenge: %v\n", err)
			os.Exit(1)
		}
		auth, err := sipua.BuildDigestResponse(ch, "REGISTER", "sip:"+dom, *user, *pass, 1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build digest: %v\n", err)
			os.Exit(1)
		}
		// 同一 Call-ID,CSeq 在 BuildRequest 内会自动 +1
		resps, err = uac.SendRequest(ctx, build(auth, respHdr), dst, *timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "REGISTER 2: %v\n", err)
			os.Exit(1)
		}
		final = resps[len(resps)-1]
		if final.StatusCode == 200 {
			fmt.Printf("OK: 200 Registered (auth %s)\n", ch.Algorithm)
			return
		}
		fmt.Printf("FAIL: %d %s\n", final.StatusCode, final.ReasonPhrase)
		os.Exit(1)
	default:
		fmt.Printf("FAIL: %d %s\n", final.StatusCode, final.ReasonPhrase)
		os.Exit(1)
	}
}
