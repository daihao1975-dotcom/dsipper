# dsipper — SIP / RTP mock client for SBC debugging

工程师调 SBC / 信令网关时常用的 4 个动作:**注册 / 探活 / 主叫 / 接听**,以单 binary 形式提供。
信令支持 **UDP** 与 **TLS**,媒体始终是**明文 RTP**(无 SRTP),编码 G.711a / G.711u。

## v0.8 新增 — UI 增强

- **多行实时面板**(`internal/clui.LivePanel`)替换之前的单行 \r 进度。invite stress 模式与 listen `--ui` 共用同一组件,刷新走 ANSI 上移 + 整屏区清屏 + cursor 隐藏,稳。非 tty(管道 / 重定向)自动 noop。
- **invite stress panel** 实时呈现:unicode 进度条 + launched/inflight/workers + ok/fail + 当前 cps + p50/p95 wall + status 分布 + ETA + elapsed。
- **listen --ui panel**:total/active + ok/fail + cps now / avg + status 分布 + uptime。
- **stress summary 增强**:加 `status` 行(top-6 状态码 + 计数,2xx 绿 ≥300 红)+ `err top` 行(失败原因聚合 top-3,带次数)。
- **panel 模式自动屏蔽 stderr 日志 tee**:面板运行期间日志只落文件(`tail -f` 可看);避免 slog 行打断面板重绘。
- **进度条 helper** `clui.ProgressBar(cur, total, width)` 用 unicode `█/░`,任意 caller 可复用。

```sh
# 看个生动的:200 通,12 并发,目标 8 cps
dsipper invite --server SBC:5060 --transport udp \
               --to sip:1001@example.com --duration 3s --save-recv "" \
               --total 200 --concurrency 12 --cps 8
```

## v0.7 新增

- **DTMF 双模式** — `--dtmf "1234#" --dtmf-mode rfc4733|inband|both`。
  - `rfc4733`(默认):带外 PT 101 RTP 包,RFC 4733 §2.5 标准时序(start + N update at 50ms + 3 end packets E=1),共享 SSRC,事件期间主语音流静音让 wire 让位。SDP 已声明 `a=fmtp:101 0-15`。
  - `inband`:DTMF 双音(行频+列频两个正弦叠加,ITU-T Q.23 频率)直接合成 PCM splice 进主音频流,跟 G.711 一起走 PT 0/8。老 PBX / ATA / 真实电话 IVR 必备。
  - `both`:两路同发,最大兼容性。
  - 时序参数 `--dtmf-delay 500ms --dtmf-duration 120ms --dtmf-gap 80ms` 三项可调。listen 端自动识别 RFC 4733 入站事件并打 log + `call done` 行带 `dtmf=...`。
- **TLS keepalive** — `--tls-keepalive 30s` 启 RFC 5626 双 CRLF 心跳(`\r\n\r\n`),防 SBC / NAT 闲置拆链;UDP 模式忽略。

```sh
# 主叫拨 1001,接通后等 500ms 发 DTMF "1234#"(带外 + 带内同发)
dsipper invite --server SBC:5060 --transport udp \
               --to sip:1001@example.com --duration 8s \
               --dtmf "1234#" --dtmf-mode both
# 长 TLS 连接,30s 心跳
dsipper options --server sbc.example.com:5061 --transport tls --tls-keepalive 30s
```

## v0.6 新增

- **invite 并发压测模式** — `--total N --concurrency M --cps R` 一条命令打压测。每 worker 独立 transport + UAC + RTP session,共享 Recorder 汇总。退出打 stress summary box(ok/fail/elapsed/actual cps/p50/p95 wall)。`--cps 0` 不限速,worker 自由跑;受 concurrency 与单通时长共同制约。

```sh
# 30 通,10 并发,目标 5 cps,共享 HTML report
dsipper invite --server SBC:5060 --transport udp \
               --to sip:1001@example.com --duration 2s \
               --total 30 --concurrency 10 --cps 5 \
               --report stress.html --save-recv ""
```

`--save-recv` 非空时,每通会落 `recv-NNNN.wav` 加序号后缀;压测一般填 `""` 不保存。

## v0.5 新增

- **HTML 信令 report** — `--report <dir>` 退出时落一份 HTML,失败通可点击展开 SVG 时序图(钉蓝/苹果绿/红配色),成功通只算汇总数。`--report-max-failed 50` 控失败详情上限,溢出仅计入顶部 status code 分布表。
- **日志只保失败** — `--log-only-failed` 把含 call-id 的日志按通缓存,2xx final 时丢弃,>= 300 / 退出 pending 时才落盘。几千万通成功呼叫磁盘 0 增长。
- **日志滚动** — `--log-max-mb 100` 单文件满即 rename 到 `.old`(覆盖旧 .old),磁盘最多 2 × max。
- **阿屠宰风 CLI** — 启动时打印钉蓝 logo + 圆角 unicode 配置 box;`listen --ui` 启用 1Hz 实时统计面板(total / ok / fail / active / cps);pipe 时自动降级无色。

```
┌─────────────────┐  SIP/UDP|TLS  ┌──────────────┐  SIP/UDP|TLS  ┌─────────────────┐
│  dsipper invite │ ────────────► │     SBC      │ ────────────► │ dsipper listen  │
│  (UAC,主叫)     │               │ (under test) │               │  (UAS,被叫)     │
│  ◄── RTP plain ──────────────────  rtpengine   ──────────────── plain RTP ────►   │
└─────────────────┘                └──────────────┘               └─────────────────┘
```

## 快速开始

```sh
# 编译当前平台 binary
make build
./bin/dsipper --help

# 同机环路自测 (一台机器跑 UAS + UAC)
./bin/dsipper listen --bind 127.0.0.1:5070 --transport udp &
./bin/dsipper invite --server 127.0.0.1:5070 --transport udp \
                     --to sip:bob@127.0.0.1 --duration 5s
# 退出 UAS:Ctrl-C 或 kill。完了得到 recv.wav(UAC 收到的)。
```

## 子命令

### 1. `options` — 探活

```sh
dsipper options --server SBC_IP:5060 --transport udp
dsipper options --server sbc.example.com:5061 --transport tls
```

成功返 `OK: 200 OK` exit 0;失败 exit 1。CI / 监控脚本可直接用 exit code。

### 2. `register` — REGISTER 带 Digest auth

```sh
# 不带认证
dsipper register --server SBC_IP:5060 --transport udp \
                 --user 1000 --domain example.com

# 带认证(401 challenge → 自动重发 Authorization)
dsipper register --server SBC_IP:5060 --transport udp \
                 --user 1000 --pass s3cret --domain example.com --expires 60
```

### 3. `invite` — 主叫一通真实呼叫,带 RTP

```sh
# 默认:发 440Hz 正弦波 10 秒,把对端 RTP 解码存 recv.wav
dsipper invite --server SBC_IP:5060 --transport udp \
               --to sip:1001@example.com

# TLS + 自有 WAV 文件 + 自定义时长
dsipper invite --server sbc.example.com:5061 --transport tls \
               --to sip:1001@example.com \
               --wav /tmp/prompt.wav --duration 30s --codec PCMU \
               --save-recv /tmp/answer.wav
```

退出后打印:
```
OK: call 30s,RTP tx=1500 pkts/258000 B  rx=1500 pkts/258000 B
recv WAV: /tmp/answer.wav
```

WAV 文件要求:**16-bit PCM mono 8 kHz**(电话标准)。

### 4. `listen` — UAS 接听模式

```sh
# UDP
dsipper listen --bind 0.0.0.0:5060 --transport udp \
               --tone 880 --save-recv rx

# TLS server,需要 cert + key
dsipper listen --bind 0.0.0.0:5061 --transport tls \
               --cert ./certs/server.crt --key ./certs/server.key \
               --save-recv rx

# UAS 主动 BYE — 200 OK 后 60 秒发 BYE(UDP / TLS 通用)
dsipper listen --bind 0.0.0.0:5060 --transport udp --bye-after 60s
```

每接到一通 INVITE:`100 Trying` → `180 Ringing` → `200 OK` + SDP answer,
回送 880 Hz 正弦波,记录主叫发来的 RTP 到 `rx-N.wav`(N 是呼叫序号)。
收到对端 BYE 立即关闭 RTP 并 dump WAV;Ctrl-C 退出时 dump 所有未结束呼叫。

**UAS 主动 BYE**:`--bye-after Nsec`(0 = 关,默认):答完 200 OK 起算
N 秒后,UAS 主动构造 BYE 走原信令通道(TLS 走原 conn / UDP 走原 socket)
回对端。RURI 取自 INVITE 的 Contact,From/To 互换 + 我方 to-tag,CSeq 1,
Route 沿用 Record-Route。常用于实验室模拟"被叫挂机"场景。

## 公共参数(register / invite / options 共享)

| 参数 | 说明 | 默认 |
|---|---|---|
| `--server` | 下游地址 host:port | (必填) |
| `--transport` | `udp` / `tls` | udp |
| `--insecure` | TLS 不校验 server 证书(自签场景) | true |
| `--ca <file>` | TLS 用 CA 校验,设置后自动关 `--insecure` | (空) |
| `-v` | 0 = info / 1 = debug(打印整条 SIP message) | 0 |
| `--log` | 日志落盘路径,空=自动 `dsipper-<cmd>-<时间戳>.log`,`-` = 仅 stderr | 空 |
| `--log-max-mb` | 单日志文件 size 上限(MB),满了 rename 到 `.log.old`;0 = 不滚 | 100 |
| `--log-only-failed` | 只落失败通日志:含 call-id 的先 buffer,2xx 时丢,≥300 / 退出 pending 时 flush | false |
| `--report` | 退出落 HTML 信令 report,目录或 `.html`;空=不生成 | 空 |
| `--report-max-failed` | HTML 详情区保留失败通条数上限,溢出仅汇总 | 50 |

### TLS 严格校验

```sh
dsipper options --server sbc.example.com:5061 --transport tls \
                --ca /etc/ssl/dh-ca.pem
```

无 `--ca` 时默认走 InsecureSkipVerify(便于自签证书场景调试)。

## 编译与跨平台

```sh
make build                      # 当前平台
make cross                      # 4 平台:darwin/arm64, darwin/amd64, linux/amd64, linux/arm64
ls bin/
# dsipper-darwin-arm64
# dsipper-darwin-amd64
# dsipper-linux-amd64
# dsipper-linux-arm64
```

Linux 二进制是静态编译(`CGO_ENABLED=0`),`scp` 到任何 Linux 机器即可跑,无需安装运行时。

## 工程师 cookbook

### 验证 SBC 上线

```sh
# 1. SBC 探活
dsipper options --server $SBC:5060 --transport udp
# 期望 OK

# 2. 注册一个测试号
dsipper register --server $SBC:5060 --transport udp \
                 --user test1000 --pass test --domain $SBC_DOMAIN
# 期望 OK: 200 Registered (auth MD5)

# 3. 主叫一通(SBC 路由到下游 UAS)
dsipper invite --server $SBC:5060 --transport udp \
               --to sip:test1001@$SBC_DOMAIN --duration 10s
# 期望 RTP tx=500 rx=500 完美对称
```

### 排查"信令通但没声音"

如果 INVITE/200/ACK 都过但 `rx=0` 包,说明 SBC 没把 RTP 转过来 — 检查 rtpengine /
RTP 防火墙 / SDP `c=` 改写。

```sh
# 主叫打开 verbose 看完整 SIP + SDP
dsipper invite --server $SBC --transport udp --to sip:test@example.com \
               --duration 5s -v 1 2>&1 | tee call.log
# 看 INVITE 里 m=audio 的 IP/port,再看 200 OK 里 m=audio 的 IP/port,
# 确认 SBC 把媒体地址改写到了 SBC 自己(rtpengine relay 模式)。
```

### 验证 TLS 信令网关(`dh-sbc/tls-udp`)

```sh
# 先用 OPTIONS 探活 TLS 端口,验证证书 + 端口都通
dsipper options --server tls-edge.example.com:5061 --transport tls --insecure

# 用 ca 严格验证证书链
dsipper options --server tls-edge.example.com:5061 --transport tls \
                --ca /path/to/dh-ca.pem
```

## 实现细节

| 模块 | 关键点 |
|---|---|
| SIP 协议 | 自写 message parse/build,支持折叠行 / 多值 header / Content-Length 拆包 |
| Auth | RFC 2617 Digest MD5,支持 qop=auth(自动 nc/cnonce)与无 qop fallback |
| Transport | UDP 单 socket / TLS over TCP 长连接复用 + SIP 帧按 Content-Length 切 |
| TLS | TLSv1.2+,可配 InsecureSkipVerify / CA 文件 / SNI |
| SDP | 最小可用:c= / m=audio / a=rtpmap;支持 telephone-event(RFC 2833 占位) |
| RTP | pion/rtp 包结构 + monotonic ticker 防漂移(20ms ptime,160 samples G.711) |
| 编码 | 自写 G.711 alaw/ulaw 编解码(纯 stdlib,无 codec 依赖) |
| WAV | 自写 16-bit mono RIFF/WAVE 读写 |
| 心跳 | TLS 长连接支持 RFC 5626 双 CRLF ping,通过 `--tls-keepalive Ns` 启用 |

## 已知边界

| 项 | 状态 |
|---|---|
| SRTP / DTLS | 不支持(本工具明确"RTP plain") |
| WebSocket / WSS | 不支持(信令仅 UDP / TLS-over-TCP) |
| codec | 仅 G.711a / G.711u(电话级,工程师调试足够;G.729 / Opus 不在范围) |
| 多并发呼叫 | UAS 端可并发接听;UAC `invite --total N --concurrency M` 内置批量压测 |
| DTMF | v0.7 起完整支持 RFC 4733 带外 + 带内双音,详见上方 v0.7 章节 |
| re-INVITE / hold | 未实现(可选 v2) |
| IPv6 | 未测试,理论上 stdlib net 已支持,Via 拼接处可能要小改 |

## 文件结构

```
dsipper/
├── main.go                 # 子命令路由
├── go.mod / go.sum         # pion/rtp + 间接 randutil
├── Makefile                # build / cross / test / fmt
├── README.md
├── cmd/                    # 子命令实现
│   ├── common.go           # 共享 flag + transport 工厂
│   ├── options.go
│   ├── register.go
│   ├── invite.go
│   └── listen.go
├── internal/
│   ├── sipua/              # SIP 协议栈
│   │   ├── message.go      # parse / build / Headers
│   │   ├── auth.go         # Digest auth
│   │   ├── transport.go    # UDP / TLS transport
│   │   └── uac.go          # UAC helper
│   ├── sdp/                # SDP 构造与解析
│   │   └── sdp.go
│   └── media/              # RTP / 编码 / WAV / tone
│       ├── codec.go        # G.711 alaw/ulaw
│       ├── rtp.go          # RTPSession 收发
│       ├── tone.go         # 正弦波合成
│       └── wav.go          # WAV 读写
└── examples/               # 一键 demo 脚本
    ├── invite-tls.sh
    ├── register-udp.sh
    └── listen-uas.sh
```

## License

内部工具,跟 dh-ss / dh-sbc 同 license。
