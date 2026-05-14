// dsipper — SIP UA mock client for engineers debugging SBC / signaling gateways.
//
// 子命令:
//   register  — 注册到 SBC,支持 Digest auth
//   invite    — 主叫一通真实呼叫,带 RTP 音频(440Hz 默认 / WAV 文件)
//   options   — 探活 ping,期待 200 OK
//   listen    — UAS 模式,接听来电并回 RTP
//
// 全部子命令都支持 --transport udp / --transport tls,RTP 始终明文(无 SRTP)。
package main

import (
	"fmt"
	"os"

	"dsipper/cmd"
)

// version 由 ldflags 注入(`-X main.version=...`,见 Makefile)。
var version = "dev"

func init() { cmd.Version = version }

const usage = `dsipper — SIP / RTP mock client for SBC debugging

用法:
  dsipper <command> [options]

命令:
  register     注册到 SBC (UDP / TLS / WS / WSS,可带 Digest auth)
  invite       发起真实呼叫,带 RTP 音频(默认 440Hz 正弦波,可 --wav 喂文件)
  options      发 OPTIONS 探活
  listen       UAS 监听模式,接听来电并回 880Hz 正弦波
  scenario     按 YAML 脚本顺序跑多步流程(options / register / invite / sleep)

例子:
  # 探活
  dsipper options --server sbc.example.com:5060 --transport udp

  # TLS 注册(自签证书,跳过校验)
  dsipper register --server sbc.example.com:5061 --transport tls --user 1000 --pass s3cret

  # 发起 10 秒呼叫,记录回流 RTP
  dsipper invite --server sbc.example.com:5060 --transport udp \
                 --to sip:1001@sbc.example.com --duration 10s

  # UAS 监听,接听并发 880Hz
  dsipper listen --bind 0.0.0.0:5060 --transport udp

公共参数(register/invite/options):
  --server     必填,host:port
  --transport  udp / tls (默认 udp)
  --insecure   TLS 不校验 server 证书 (默认 true)
  --ca <file>  TLS 用 CA 校验 server 证书 (开严格校验)
  -v           verbose 调试,把完整 SIP message 打到 stderr

各子命令独有参数: dsipper <command> -h
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "register":
		cmd.Register(os.Args[2:])
	case "invite":
		cmd.Invite(os.Args[2:])
	case "options":
		cmd.Options(os.Args[2:])
	case "listen":
		cmd.Listen(os.Args[2:])
	case "scenario":
		cmd.Scenario(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "--version", "version":
		// 不接 -v(避免跟子命令的 -v verbose level 短 flag 冲突)
		fmt.Printf("dsipper %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
