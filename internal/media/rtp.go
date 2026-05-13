package media

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// RTPSession 是单个 RTP 双向会话(同一对 UDP 端口,接收 + 发送 共用)。
type RTPSession struct {
	conn       *net.UDPConn
	remote     *net.UDPAddr
	ssrc       uint32
	pt         uint8
	clockRate  uint32 // 8000 for G.711
	ptimeMs    int    // 20

	// 发送侧 seq/ts。主 Send() 跟 SendDTMF() 可能并发使用同一 conn(共享 SSRC),
	// 用 sendMu 序列化"构造包 + 推进 seq/ts + 写 socket"临界区,避免 wire 上 seq 错乱。
	sendMu  sync.Mutex
	seq     uint16
	ts      uint32

	pktsTx  uint64
	pktsRx  uint64
	bytesTx uint64
	bytesRx uint64

	rxSamples []int16 // 累计收到的 PCM (decoded),供 dump WAV
	rxLock    sync.Mutex

	// dtmfActive 表示主 Send() 是否应该跳过本轮 — DTMF 事件期间主语音流静音,
	// 让 RFC 4733 事件独占同一 timestamp 段(RFC 4733 §2.5)。
	dtmfActive atomic.Bool

	// dtmfPT 是 RFC 4733 telephone-event payload type,默认 101。
	dtmfPT uint8

	// 已收到的 DTMF digits ASCII 串(去重 by event-start,RX 侧填),供 stats / report 引用。
	rxDtmf   strings.Builder
	rxDtmfMu sync.Mutex
	// 上一个 RX DTMF event 的 (start_ts, event_code),用于在收到 update/end 包时不重复记录同一 event。
	rxLastDtmfTS    uint32
	rxLastDtmfEvent byte
	rxHaveLastDtmf  bool

	codecName string
}

// NewRTPSession 在本地随机端口建 UDP socket。返回的本地端口供 SDP m=audio 使用。
func NewRTPSession(localIP string, pt uint8, codecName string) (*RTPSession, error) {
	laddr := &net.UDPAddr{IP: net.ParseIP(localIP), Port: 0}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}
	return newSessionFromConn(conn, pt, codecName), nil
}

// NewRTPSessionInRange 在 [portMin, portMax] 区间随机挑端口 bind。
// 端口范围是 inclusive,顺序 shuffle 后逐个尝试,挑到一个能 bind 上的就 return。
// 试 50 次都失败就报错。
func NewRTPSessionInRange(localIP string, portMin, portMax int, pt uint8, codecName string) (*RTPSession, error) {
	if portMin <= 0 || portMax <= 0 || portMin > portMax {
		return nil, fmt.Errorf("bad RTP port range: [%d,%d]", portMin, portMax)
	}
	if portMin > 65535 || portMax > 65535 {
		return nil, fmt.Errorf("RTP port out of range: [%d,%d]", portMin, portMax)
	}
	ip := net.ParseIP(localIP)
	if ip == nil {
		return nil, fmt.Errorf("bad localIP: %s", localIP)
	}

	// 构造 [portMin..portMax] 列表并 shuffle
	n := portMax - portMin + 1
	ports := make([]int, n)
	for i := 0; i < n; i++ {
		ports[i] = portMin + i
	}
	rand.Shuffle(n, func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })

	maxTries := 50
	if n < maxTries {
		maxTries = n
	}
	var lastErr error
	for i := 0; i < maxTries; i++ {
		laddr := &net.UDPAddr{IP: ip, Port: ports[i]}
		conn, err := net.ListenUDP("udp", laddr)
		if err == nil {
			return newSessionFromConn(conn, pt, codecName), nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no free RTP port in [%d,%d] after %d tries: %v", portMin, portMax, maxTries, lastErr)
}

func newSessionFromConn(conn *net.UDPConn, pt uint8, codecName string) *RTPSession {
	return &RTPSession{
		conn:      conn,
		ssrc:      rand.Uint32(),
		pt:        pt,
		clockRate: 8000,
		ptimeMs:   20,
		seq:       uint16(rand.Intn(65535)),
		ts:        rand.Uint32(),
		dtmfPT:    101,
		codecName: codecName,
	}
}

func (s *RTPSession) LocalPort() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }
func (s *RTPSession) Close() error    { return s.conn.Close() }

// SetRemote 设置发送目的(SDP answer 解出来后调)。
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	if addr.IP == nil {
		return fmt.Errorf("bad RTP remote ip: %s", ip)
	}
	s.remote = addr
	return nil
}

// Send 把 PCM 切片按 ptime 节奏(20 ms = 160 samples @ 8 kHz)分包发出。
// 阻塞直到全部发完或 ctx 取消。
//
// DTMF 事件期间(dtmfActive=true)主语音流"静音":timestamp 继续按 ticker 推进,
// 但不实际发包,把 wire 上同一时段让给 RFC 4733 DTMF 事件包(共享 SSRC + seq)。
func (s *RTPSession) Send(ctx context.Context, pcm []int16) error {
	if s.remote == nil {
		return fmt.Errorf("RTP remote not set")
	}
	samplesPerPkt := s.clockRate * uint32(s.ptimeMs) / 1000 // 160
	encoded := EncodePCM(pcm, s.codecName)

	tick := time.NewTicker(time.Duration(s.ptimeMs) * time.Millisecond)
	defer tick.Stop()

	// 用 monotonic 累计算法避免漂移
	start := time.Now()
	pktIdx := 0
	for off := 0; off < len(encoded); off += int(samplesPerPkt) {
		end := off + int(samplesPerPkt)
		if end > len(encoded) {
			end = len(encoded)
		}
		payload := encoded[off:end]

		// DTMF 事件期间不发主语音包,但 seq/ts 仍前进(由 SendDTMF 锁里推进)
		if !s.dtmfActive.Load() {
			s.sendMu.Lock()
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    s.pt,
					SequenceNumber: s.seq,
					Timestamp:      s.ts,
					SSRC:           s.ssrc,
				},
				Payload: payload,
			}
			buf, err := pkt.Marshal()
			if err != nil {
				s.sendMu.Unlock()
				return err
			}
			if _, err := s.conn.WriteToUDP(buf, s.remote); err != nil {
				s.sendMu.Unlock()
				return err
			}
			s.seq++
			s.ts += samplesPerPkt
			s.sendMu.Unlock()
			atomic.AddUint64(&s.pktsTx, 1)
			atomic.AddUint64(&s.bytesTx, uint64(len(buf)))
		} else {
			// 静音期间 timestamp 仍推进 — 跟 wall clock 对齐,DTMF 包用自己的 ts
			s.sendMu.Lock()
			s.ts += samplesPerPkt
			s.sendMu.Unlock()
		}
		pktIdx++

		// 等下一个发送时刻
		next := start.Add(time.Duration(pktIdx) * time.Duration(s.ptimeMs) * time.Millisecond)
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
	return nil
}

// Recv 启动接收循环,直到 ctx 取消或 socket 关闭。解码后追加到 rxSamples。
func (s *RTPSession) Recv(ctx context.Context, log *slog.Logger) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			if log != nil {
				log.Debug("rtp unmarshal failed", "err", err, "len", n)
			}
			continue
		}
		atomic.AddUint64(&s.pktsRx, 1)
		atomic.AddUint64(&s.bytesRx, uint64(n))

		// 入站 DTMF (RFC 4733):PT 与 dtmfPT 一致时解 4-byte payload,按 (ts, event) 去重
		// 记录到 rxDtmf — 同一 event 跨多个 update/end 包不重复记。
		if pkt.PayloadType == s.dtmfPT {
			if ev, _, _, _, err := ParseDTMFEventPayload(pkt.Payload); err == nil {
				s.rxDtmfMu.Lock()
				isNew := !s.rxHaveLastDtmf || s.rxLastDtmfTS != pkt.Timestamp || s.rxLastDtmfEvent != ev
				if isNew {
					s.rxDtmf.WriteByte(DTMFEventASCII(ev))
					s.rxLastDtmfTS = pkt.Timestamp
					s.rxLastDtmfEvent = ev
					s.rxHaveLastDtmf = true
					if log != nil {
						log.Info("RX DTMF (RFC 4733)", "digit", string(DTMFEventASCII(ev)), "ts", pkt.Timestamp)
					}
				}
				s.rxDtmfMu.Unlock()
			}
			continue
		}

		// 仅 G.711 解码,其他 PT 丢弃 payload 但仍计数
		samples := DecodePCM(pkt.Payload, codecNameForPT(pkt.PayloadType, s.codecName))
		s.rxLock.Lock()
		s.rxSamples = append(s.rxSamples, samples...)
		s.rxLock.Unlock()
	}
}

// SendDTMF 在通话期间按 RFC 4733 时序发一串 DTMF digits。
// 跟主 Send() 共享 conn + SSRC + seq;事件期间设置 dtmfActive=true 让主 Send 静音,
// wire 上同一段时间只有 DTMF 包(避免接收端语音 + telephone-event 撞 RFC 4733 §2.5)。
//
// 每个 digit 内部按 50ms ptime 发 start + N update,最后 3 个 end 包(E=1)冗余。
// gapMs 是 digit 间静音间隔(不发任何包,只睡眠)。
//
// digit 串里不识别字符自动跳过;digits 空串直接返。
// volume 走 -10 dBm0(行业典型),clockRate=8000 时 50ms = 400 samples。
func (s *RTPSession) SendDTMF(ctx context.Context, digits string, perDigitMs, gapMs int) error {
	if s.remote == nil {
		return fmt.Errorf("RTP remote not set")
	}
	if perDigitMs < 50 {
		perDigitMs = 50
	}
	if gapMs < 0 {
		gapMs = 0
	}
	const ptimeMs = 50
	const volume = byte(10) // -10 dBm0
	const endRedundancy = 3
	stepSamples := uint32(s.clockRate) * uint32(ptimeMs) / 1000 // 400

	for i := 0; i < len(digits); i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		ev, ok := DTMFEventCode(digits[i])
		if !ok {
			continue
		}

		// 抢一个 timestamp 作为 event 起始(同一 event 内所有包共用)
		s.sendMu.Lock()
		eventTS := s.ts
		s.sendMu.Unlock()

		s.dtmfActive.Store(true)

		// 发 update 包,直到累计 duration >= perDigitMs
		numUpdates := perDigitMs / ptimeMs
		if numUpdates < 1 {
			numUpdates = 1
		}
		duration := uint32(0)
		ticker := time.NewTicker(time.Duration(ptimeMs) * time.Millisecond)
		for u := 0; u < numUpdates; u++ {
			duration += stepSamples
			if duration > 0xFFFF {
				duration = 0xFFFF
			}
			payload := BuildDTMFEventPayload(ev, false, volume, uint16(duration))
			if err := s.sendDTMFPacket(payload, eventTS, u == 0); err != nil {
				ticker.Stop()
				s.dtmfActive.Store(false)
				return err
			}
			if u < numUpdates-1 {
				select {
				case <-ctx.Done():
					ticker.Stop()
					s.dtmfActive.Store(false)
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}
		ticker.Stop()

		// 3 个 end 包,duration 锁定到最终值,E bit 置 1
		for j := 0; j < endRedundancy; j++ {
			payload := BuildDTMFEventPayload(ev, true, volume, uint16(duration))
			if err := s.sendDTMFPacket(payload, eventTS, false); err != nil {
				s.dtmfActive.Store(false)
				return err
			}
			// end 包之间略停以利接收端去重
			if j < endRedundancy-1 {
				select {
				case <-ctx.Done():
					s.dtmfActive.Store(false)
					return ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		s.dtmfActive.Store(false)

		// digit 间空隙
		if gapMs > 0 && i < len(digits)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(gapMs) * time.Millisecond):
			}
		}
	}
	return nil
}

// sendDTMFPacket 在 sendMu 锁内写一个 PT=dtmfPT 的 RTP 包,seq 自增,timestamp 不改。
// marker=true 时设 M-bit(每 event 首包)。
func (s *RTPSession) sendDTMFPacket(payload []byte, eventTS uint32, marker bool) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    s.dtmfPT,
			SequenceNumber: s.seq,
			Timestamp:      eventTS,
			SSRC:           s.ssrc,
		},
		Payload: payload,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		return err
	}
	if _, err := s.conn.WriteToUDP(buf, s.remote); err != nil {
		return err
	}
	s.seq++
	atomic.AddUint64(&s.pktsTx, 1)
	atomic.AddUint64(&s.bytesTx, uint64(len(buf)))
	return nil
}

// RxDTMF 返回到目前为止接收侧识别出的 DTMF digits ASCII 串。
func (s *RTPSession) RxDTMF() string {
	s.rxDtmfMu.Lock()
	defer s.rxDtmfMu.Unlock()
	return s.rxDtmf.String()
}

// DumpWAV 把累计接收到的 PCM 落盘。
func (s *RTPSession) DumpWAV(path string) error {
	s.rxLock.Lock()
	defer s.rxLock.Unlock()
	if len(s.rxSamples) == 0 {
		return fmt.Errorf("no RTP samples received")
	}
	return WriteWAV16Mono(path, s.rxSamples, int(s.clockRate))
}

// Stats 返回 (sent_pkts, recv_pkts, sent_bytes, recv_bytes)。
func (s *RTPSession) Stats() (uint64, uint64, uint64, uint64) {
	return atomic.LoadUint64(&s.pktsTx),
		atomic.LoadUint64(&s.pktsRx),
		atomic.LoadUint64(&s.bytesTx),
		atomic.LoadUint64(&s.bytesRx)
}

// codecNameForPT:静态 PT 0/8 已知,其它假设跟自己设置的 codec 一致(回声场景)。
func codecNameForPT(pt uint8, fallback string) string {
	switch pt {
	case 0:
		return "PCMU"
	case 8:
		return "PCMA"
	default:
		return fallback
	}
}
