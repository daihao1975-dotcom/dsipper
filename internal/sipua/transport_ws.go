package sipua

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// SIP-over-WebSocket(RFC 7118)的 transport 实现。
//
// 帧形态:每个 WebSocket 文本帧载 一条 完整 SIP message,没有 Content-Length 流式拆包问题。
// 这跟 TLS-over-TCP 路径不一样,因此 wsTransport 比 tlsTransport 简单一截。
//
// Sec-WebSocket-Protocol 必须是 "sip"(RFC 7118 §5.1)。
// scheme "ws://" 走明文,"wss://" 走 TLS;两者共用同一 transport 实现,
// Proto() 据 useTLS 返回 "ws" 或 "wss"。Via 头大写 WS/WSS,符合 §5.5。

type wsTransport struct {
	useTLS bool
	cfg    *tls.Config // 仅 wss client / wss server 用
	path   string      // server 端的 Accept path,默认 "/"

	mu    sync.Mutex
	conns map[string]*websocket.Conn // dst addr → conn(client 模式)/ 远端 addr → conn(server 模式)

	listen   net.Listener // server 模式
	httpSrv  *http.Server // server 模式包一层 HTTP server 做 WS upgrade
	inbox    chan Inbound
	closed   chan struct{}
	closeOnce sync.Once

	dialAddr string // client 模式记录目标 host:port,Send 时 Dial
	dialURL  string // 完整 ws(s):// URL
}

// WSOptions 控制 WebSocket transport 行为。
type WSOptions struct {
	// 客户端 / 服务端共用
	Path string // server Accept path / client 路径,默认 "/"

	// 客户端独有
	ServerHost string // host:port,用于拼 ws(s)://host:port/path
	TLS        TLSOptions // wss 时校验/SNI/CA;ws 时忽略

	// 服务端独有
	ListenAddr string     // server bind 地址
	TLSServer  TLSOptions // wss server 时填 CertFile/KeyFile
}

// NewWSClient 创建 client 模式 WS 或 WSS transport。首次 Send 时 Dial,后续复用同一连接。
// useTLS=true → wss:// + TLS handshake;false → ws://。
func NewWSClient(useTLS bool, opts WSOptions) (Transport, error) {
	if opts.ServerHost == "" {
		return nil, errors.New("WSClient: ServerHost 必填")
	}
	path := opts.Path
	if path == "" {
		path = "/"
	}
	scheme := "ws"
	var cfg *tls.Config
	if useTLS {
		scheme = "wss"
		c, err := buildTLSConfig(opts.TLS, false)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	t := &wsTransport{
		useTLS:   useTLS,
		cfg:      cfg,
		path:     path,
		conns:    map[string]*websocket.Conn{},
		inbox:    make(chan Inbound, 8192),
		closed:   make(chan struct{}),
		dialAddr: opts.ServerHost,
		dialURL:  fmt.Sprintf("%s://%s%s", scheme, opts.ServerHost, path),
	}
	return t, nil
}

// NewWSServer 创建 server 模式 WS 或 WSS transport,监听 listenAddr 等待 Upgrade。
// useTLS=true 时 TLSServer.CertFile/KeyFile 必填。
func NewWSServer(useTLS bool, opts WSOptions) (Transport, error) {
	if opts.ListenAddr == "" {
		return nil, errors.New("WSServer: ListenAddr 必填")
	}
	path := opts.Path
	if path == "" {
		path = "/"
	}

	t := &wsTransport{
		useTLS: useTLS,
		path:   path,
		conns:  map[string]*websocket.Conn{},
		inbox:  make(chan Inbound, 8192),
		closed: make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, t.serveWS)

	srv := &http.Server{
		Handler: mux,
	}
	if useTLS {
		c, err := buildTLSConfig(opts.TLSServer, true)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = c
		t.cfg = c
	}
	t.httpSrv = srv

	ln, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("ws listen: %w", err)
	}
	t.listen = ln

	go func() {
		if useTLS {
			_ = srv.ServeTLS(ln, "", "")
		} else {
			_ = srv.Serve(ln)
		}
	}()
	return t, nil
}

func (t *wsTransport) Proto() string {
	if t.useTLS {
		return "wss"
	}
	return "ws"
}

func (t *wsTransport) LocalAddr() net.Addr {
	if t.listen != nil {
		return t.listen.Addr()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// client 端:复用 net.Conn?nhooyr 不直接暴露底层 socket。我们没法拿到本地端口,
	// 走 LocalAddr() 返回 0.0.0.0:0 占位 — UAC 用 LocalIP + Via "rport" 让 SBC 反推。
	return &net.TCPAddr{}
}

func (t *wsTransport) Recv() <-chan Inbound { return t.inbox }

// dial 建立到目标 host:port 的 WS 连接,挂上 readLoop。已存在则复用。
func (t *wsTransport) dial(addrKey string) error {
	t.mu.Lock()
	if _, ok := t.conns[addrKey]; ok {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := &websocket.DialOptions{
		Subprotocols: []string{"sip"},
	}
	if t.useTLS {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: t.cfg,
			},
			Timeout: 10 * time.Second,
		}
	}
	c, _, err := websocket.Dial(ctx, t.dialURL, opts)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", t.dialURL, err)
	}
	// 单条 WS message 上限放宽到 1 MB,SIP 信令通常 <8KB
	c.SetReadLimit(1 << 20)

	t.mu.Lock()
	t.conns[addrKey] = c
	t.mu.Unlock()
	go t.readLoop(c, addrKey)
	return nil
}

func (t *wsTransport) Send(msg []byte, dst net.Addr) error {
	addrKey := dst.String()
	if err := t.dial(addrKey); err != nil {
		return err
	}
	t.mu.Lock()
	c := t.conns[addrKey]
	t.mu.Unlock()
	if c == nil {
		return errors.New("ws transport: no conn after dial")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, msg)
}

// Dial 实现 Dialer 接口,让 CommonOpts.MakeTransport 在 ws/wss client 模式
// 启动后立刻建连,以便 LocalAddr 可用(此处占位,UAC Via 走 rport)。
func (t *wsTransport) Dial(dst net.Addr) error {
	return t.dial(dst.String())
}

func (t *wsTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.httpSrv != nil {
			_ = t.httpSrv.Close()
		}
		if t.listen != nil {
			_ = t.listen.Close()
		}
		t.mu.Lock()
		for _, c := range t.conns {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
		t.conns = map[string]*websocket.Conn{}
		t.mu.Unlock()
	})
	return nil
}

// serveWS 是 server 模式的 HTTP handler,把 HTTP 升级成 WS 后挂 readLoop。
func (t *wsTransport) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{"sip"},
		// 允许任意 Origin — dsipper 是 mock 工具,产线 UAS 也很少校验 Origin
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	addrKey := r.RemoteAddr
	t.mu.Lock()
	t.conns[addrKey] = c
	t.mu.Unlock()
	t.readLoop(c, addrKey)
}

// readLoop 在 conn 上阻塞读 WS message,每条文本帧投到 inbox。
// 退出时清理 conns 表项;不在这里关 conn(由 Close 集中关)。
func (t *wsTransport) readLoop(c *websocket.Conn, addrKey string) {
	defer func() {
		t.mu.Lock()
		delete(t.conns, addrKey)
		t.mu.Unlock()
	}()

	remote, _ := net.ResolveTCPAddr("tcp", addrKey)
	// nhooyr 不暴露原生 net.Conn,我们用一个轻量 wrapper 让 Inbound.Conn.Write
	// 走回同一 ws conn(给 listen 端 in.Conn.Write 回响应路径)。
	wsConn := &wsConnAdapter{c: c, remote: remote}

	for {
		select {
		case <-t.closed:
			return
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		// 用 closed channel 中止 Read
		go func() {
			select {
			case <-t.closed:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		select {
		case t.inbox <- Inbound{Data: data, From: remote, Conn: wsConn}:
		default:
			// 满了就丢
		}
	}
}

// wsConnAdapter 把 nhooyr websocket.Conn 包成 net.Conn,只实现 Write —
// 让 cmd/listen.go 现有的 "in.Conn.Write(raw)" 路径在 WS 下不变。
// 其他 net.Conn 方法返回零值 / not-implemented(实际不会被调用)。
type wsConnAdapter struct {
	c      *websocket.Conn
	remote net.Addr
}

func (a *wsConnAdapter) Write(b []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.c.Write(ctx, websocket.MessageText, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// 下面这些是 net.Conn 接口的 stub。SIP TX 路径只调 Write。
func (a *wsConnAdapter) Read([]byte) (int, error)         { return 0, errors.New("ws adapter: Read not supported") }
func (a *wsConnAdapter) Close() error                      { return a.c.Close(websocket.StatusNormalClosure, "") }
func (a *wsConnAdapter) LocalAddr() net.Addr               { return &net.TCPAddr{} }
func (a *wsConnAdapter) RemoteAddr() net.Addr              { return a.remote }
func (a *wsConnAdapter) SetDeadline(time.Time) error       { return nil }
func (a *wsConnAdapter) SetReadDeadline(time.Time) error   { return nil }
func (a *wsConnAdapter) SetWriteDeadline(time.Time) error  { return nil }

// ParseWSURL 把 "ws://h:p/path" 或 "wss://h:p/path" 拆成 (useTLS, host:port, path)。
// 兼容 "h:p" 裸 host:port(默认 ws + path="/")。
func ParseWSURL(s string) (useTLS bool, hostPort, path string, err error) {
	if !strings.Contains(s, "://") {
		return false, s, "/", nil
	}
	u, perr := url.Parse(s)
	if perr != nil {
		return false, "", "", fmt.Errorf("parse ws URL: %w", perr)
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		useTLS = false
	case "wss":
		useTLS = true
	default:
		return false, "", "", fmt.Errorf("unsupported scheme %q (want ws / wss)", u.Scheme)
	}
	hostPort = u.Host
	path = u.Path
	if path == "" {
		path = "/"
	}
	return useTLS, hostPort, path, nil
}

