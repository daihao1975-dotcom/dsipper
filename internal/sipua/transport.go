package sipua

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Transport 抽象 UDP / TLS-over-TCP 两种 SIP 承载。
//
// 给 UAC 用:Send + Recv 异步,Recv 把每条完整 SIP message 投到 channel。
// 给 UAS 用:Listen 接受多个客户端,每个连接独立投递到同一个 inbox channel。
type Transport interface {
	Proto() string                  // "udp" / "tls"
	LocalAddr() net.Addr            // 实际本地地址(包含已绑定端口)
	Send(msg []byte, dst net.Addr) error
	Recv() <-chan Inbound           // 收到的 message
	Close() error
}

// Dialer 是可选接口:面向连接的 transport(TLS)实现它,UAC 在 BuildRequest 前
// 调用 Dial 提前建连,确保 LocalAddr 端口可用。UDP transport 不必实现。
type Dialer interface {
	Dial(addr net.Addr) error
}

// Inbound 是 Recv channel 的元素。
type Inbound struct {
	Data []byte
	From net.Addr
	// 用于 reply:UDP 给 nil(用 From),TLS 给 net.Conn(必须从原连接回)。
	Conn net.Conn
}

// --- UDP ---------------------------------------------------------------

type udpTransport struct {
	conn  *net.UDPConn
	inbox chan Inbound
	once  sync.Once
}

// NewUDPClient 创建一个本地随机端口的 UDP transport,用于 UAC 主动发起。
// localAddr 可空(自动选端口)或 "ip:port" 显式绑定。
func NewUDPClient(localAddr string) (Transport, error) {
	var laddr *net.UDPAddr
	if localAddr != "" {
		var err error
		laddr, err = net.ResolveUDPAddr("udp", localAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve local: %w", err)
		}
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}
	// 调优 2026-05-13:burst 压测时 inbox cap 32 / OS UDP recv buffer 默认 208KB 是
	// 主要瓶颈,N=400 burst 撞 24% fail。拉到 inbox cap 8192 + RecvBuf 16MB 大幅缓解。
	_ = conn.SetReadBuffer(16 << 20)
	_ = conn.SetWriteBuffer(8 << 20)
	t := &udpTransport{conn: conn, inbox: make(chan Inbound, 8192)}
	go t.readLoop()
	return t, nil
}

func (t *udpTransport) Proto() string       { return "udp" }
func (t *udpTransport) LocalAddr() net.Addr { return t.conn.LocalAddr() }
func (t *udpTransport) Recv() <-chan Inbound {
	return t.inbox
}

func (t *udpTransport) Send(msg []byte, dst net.Addr) error {
	udpDst, ok := dst.(*net.UDPAddr)
	if !ok {
		// 允许传 *net.TCPAddr / 字符串,统一转成 UDPAddr
		host, port, err := splitHostPort(dst.String())
		if err != nil {
			return err
		}
		udpDst, err = net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
		if err != nil {
			return err
		}
	}
	_, err := t.conn.WriteToUDP(msg, udpDst)
	return err
}

func (t *udpTransport) Close() error {
	t.once.Do(func() { close(t.inbox) })
	return t.conn.Close()
}

func (t *udpTransport) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case t.inbox <- Inbound{Data: data, From: addr}:
		default:
			// 满了就丢,避免阻塞读循环
		}
	}
}

// --- TLS over TCP ------------------------------------------------------

type tlsTransport struct {
	cfg    *tls.Config
	mu     sync.Mutex
	conns  map[string]net.Conn // dst-addr → 已建立的连接(client 模式),server 模式留空
	listen net.Listener        // server 模式
	inbox  chan Inbound
	closed chan struct{}

	// keepaliveInterval > 0 时,后台 goroutine 每隔此时长给每个活跃 conn 写
	// "\r\n\r\n" 双 CRLF(RFC 5626 §4.4.1 ping)。SBC 收到 ping 回 "\r\n" pong 心跳,
	// readConn 已能正确把单 \r\n 吞掉不当 SIP message。
	keepaliveInterval time.Duration
}

// TLSOptions 控制证书验证与 SNI。
type TLSOptions struct {
	ServerName string // SNI / 校验目标
	Insecure   bool   // 跳过证书验证
	CAFile     string // 可选,客户端校验 server 证书的 CA
	CertFile   string // server 模式必填
	KeyFile    string // server 模式必填
}

// NewTLSClient 创建 client 模式 TLS transport,首次 Send 时建立连接,后续复用。
func NewTLSClient(opts TLSOptions) (Transport, error) {
	cfg, err := buildTLSConfig(opts, false)
	if err != nil {
		return nil, err
	}
	t := &tlsTransport{
		cfg:    cfg,
		conns:  map[string]net.Conn{},
		inbox:  make(chan Inbound, 8192),
		closed: make(chan struct{}),
	}
	return t, nil
}

// NewTLSServer 创建 server 模式,Listen 在 listenAddr。
func NewTLSServer(listenAddr string, opts TLSOptions) (Transport, error) {
	cfg, err := buildTLSConfig(opts, true)
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", listenAddr, cfg)
	if err != nil {
		return nil, err
	}
	t := &tlsTransport{
		cfg:    cfg,
		conns:  map[string]net.Conn{},
		listen: ln,
		inbox:  make(chan Inbound, 8192),
		closed: make(chan struct{}),
	}
	go t.acceptLoop()
	return t, nil
}

func buildTLSConfig(opts TLSOptions, server bool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: opts.ServerName,
	}
	if opts.Insecure {
		cfg.InsecureSkipVerify = true
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file: no PEM certs found")
		}
		cfg.RootCAs = pool
	}
	if server {
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, errors.New("server mode requires CertFile + KeyFile")
		}
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if path := os.Getenv("SSLKEYLOGFILE"); path != "" {
		w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return nil, fmt.Errorf("open SSLKEYLOGFILE %s: %w", path, err)
		}
		cfg.KeyLogWriter = w
	}
	return cfg, nil
}

// EnableKeepalive 开启 SIP-CRLF 心跳(RFC 5626);interval<=0 时 noop。
// 客户端模式建议 30-90s(防 SBC / NAT 拆链);server 模式一般不需要(客户端会自己 ping)。
// 必须在 Dial 之后调,或 Dial 后调:goroutine 启动后自动覆盖后续新建的 conn。
func (t *tlsTransport) EnableKeepalive(interval time.Duration) {
	if interval <= 0 {
		return
	}
	t.mu.Lock()
	already := t.keepaliveInterval > 0
	t.keepaliveInterval = interval
	t.mu.Unlock()
	if already {
		return // 已有 goroutine 在跑
	}
	go t.keepaliveLoop()
}

func (t *tlsTransport) keepaliveLoop() {
	ping := []byte("\r\n\r\n")
	for {
		t.mu.Lock()
		iv := t.keepaliveInterval
		t.mu.Unlock()
		if iv <= 0 {
			return
		}
		select {
		case <-t.closed:
			return
		case <-time.After(iv):
		}
		t.mu.Lock()
		conns := make([]net.Conn, 0, len(t.conns))
		for _, c := range t.conns {
			conns = append(conns, c)
		}
		t.mu.Unlock()
		for _, c := range conns {
			_, _ = c.Write(ping) // 失败由 readConn 检测断链
		}
	}
}

func (t *tlsTransport) Proto() string { return "tls" }
func (t *tlsTransport) LocalAddr() net.Addr {
	if t.listen != nil {
		return t.listen.Addr()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.conns {
		return c.LocalAddr()
	}
	return &net.TCPAddr{}
}
func (t *tlsTransport) Recv() <-chan Inbound { return t.inbox }

// Dial 提前建连,让 LocalAddr 已知。可重复调用,已连则 noop。
func (t *tlsTransport) Dial(dst net.Addr) error {
	addr := dst.String()
	t.mu.Lock()
	_, ok := t.conns[addr]
	t.mu.Unlock()
	if ok {
		return nil
	}
	conn, err := tls.Dial("tcp", addr, t.cfg)
	if err != nil {
		return fmt.Errorf("tls dial %s: %w", addr, err)
	}
	t.mu.Lock()
	t.conns[addr] = conn
	t.mu.Unlock()
	go t.readConn(conn)
	return nil
}

func (t *tlsTransport) Send(msg []byte, dst net.Addr) error {
	if err := t.Dial(dst); err != nil {
		return err
	}
	t.mu.Lock()
	c := t.conns[dst.String()]
	t.mu.Unlock()
	_, err := c.Write(msg)
	return err
}

func (t *tlsTransport) Close() error {
	select {
	case <-t.closed:
		return nil
	default:
		close(t.closed)
	}
	if t.listen != nil {
		t.listen.Close()
	}
	t.mu.Lock()
	for _, c := range t.conns {
		c.Close()
	}
	t.conns = map[string]net.Conn{}
	t.mu.Unlock()
	return nil
}

func (t *tlsTransport) acceptLoop() {
	for {
		c, err := t.listen.Accept()
		if err != nil {
			return
		}
		t.mu.Lock()
		t.conns[c.RemoteAddr().String()] = c
		t.mu.Unlock()
		go t.readConn(c)
	}
}

// readConn 按 SIP-over-TCP 帧规则切包:读到完整 header(到 \r\n\r\n)后,
// 根据 Content-Length 再读 body,然后投递到 inbox。
func (t *tlsTransport) readConn(c net.Conn) {
	defer c.Close()
	buf := bytes.Buffer{}
	tmp := make([]byte, 4096)
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			for {
				msg, ok := tryExtractSIPFrame(&buf)
				if !ok {
					break
				}
				// RFC 5626 心跳:tryExtractSIPFrame 返回的 "\r\n" 单独 frame 不是 SIP message,
				// 上层 parse 会报 "malformed request line"。直接吞掉,不投 inbox。
				if len(msg) <= 4 && (bytes.Equal(msg, []byte("\r\n")) || bytes.Equal(msg, []byte("\r\n\r\n"))) {
					continue
				}
				select {
				case t.inbox <- Inbound{Data: msg, From: c.RemoteAddr(), Conn: c}:
				default:
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				// 安静退出
			}
			t.mu.Lock()
			delete(t.conns, c.RemoteAddr().String())
			t.mu.Unlock()
			return
		}
	}
}

// tryExtractSIPFrame 从 buf 头部尝试切出一条完整 SIP message。返回 (frame, true) 或 nil,false 等待更多数据。
func tryExtractSIPFrame(buf *bytes.Buffer) ([]byte, bool) {
	data := buf.Bytes()
	idx := bytes.Index(data, []byte("\r\n\r\n"))
	if idx < 0 {
		// SIP-CRLF 心跳(RFC 5626):单 \r\n 或 \r\n\r\n 不带 message,前者裸 CRLF
		if bytes.HasPrefix(data, []byte("\r\n")) {
			// 至少 2 字节,作为心跳吞掉
			buf.Next(2)
			return []byte("\r\n"), true
		}
		return nil, false
	}
	// 解析 header 找 Content-Length
	headEnd := idx + 4
	cl := scanContentLength(data[:idx])
	total := headEnd + cl
	if buf.Len() < total {
		return nil, false
	}
	frame := make([]byte, total)
	copy(frame, data[:total])
	buf.Next(total)
	return frame, true
}

func scanContentLength(headBytes []byte) int {
	// 简单逐行扫,case-insensitive
	for _, line := range strings.Split(string(headBytes), "\r\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		if name == "content-length" || name == "l" {
			v := strings.TrimSpace(line[colon+1:])
			n := 0
			for _, ch := range v {
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int(ch-'0')
			}
			return n
		}
	}
	return 0
}

// 拆分 host:port,容忍 IPv6 [::1]:5060
func splitHostPort(s string) (string, string, error) {
	host, port, err := net.SplitHostPort(s)
	if err == nil {
		return host, port, nil
	}
	// 没冒号,假定是 host,默认口由调用方决定
	return s, "", err
}

// 让 transport 之间共享一个超时常量
const ioTimeout = 30 * time.Second
