package logsink

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRotatingFile_RollsAtCap 验证 size 超过上限时 rename 到 .old + 重开。
func TestRotatingFile_RollsAtCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	rf, err := NewRotatingFile(path, 100) // 100 byte cap
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	// 第一波 60 字节:不触发滚动
	if _, err := rf.Write(bytes.Repeat([]byte("a"), 60)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Size() != 60 {
		t.Fatalf("step1 size=%d want 60", fi.Size())
	}

	// 第二波再写 50 字节:60+50=110>100,触发 rotate,新文件只有 50 字节
	if _, err := rf.Write(bytes.Repeat([]byte("b"), 50)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Size() != 50 {
		t.Fatalf("after rotate size=%d want 50", fi.Size())
	}
	oldFi, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatalf(".old not created: %v", err)
	}
	if oldFi.Size() != 60 {
		t.Fatalf(".old size=%d want 60", oldFi.Size())
	}

	// 第三波再触发:新 .old 覆盖旧 .old(只保留 1 份历史)
	if _, err := rf.Write(bytes.Repeat([]byte("c"), 70)); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if fi.Size() != 70 {
		t.Fatalf("after 2nd rotate size=%d want 70", fi.Size())
	}
	oldFi, _ = os.Stat(path + ".old")
	if oldFi.Size() != 50 {
		t.Fatalf(".old 2nd size=%d want 50 (overwritten)", oldFi.Size())
	}
}

// TestBufHandler_OnlyFailed_DropAndFlush:
// 成功通 DropCall → 日志真没落;失败通 FlushCall → 日志全部进 inner。
func TestBufHandler_OnlyFailed_DropAndFlush(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug})
	bh := NewBufHandler(inner, true)
	log := slog.New(bh)

	// 成功通模拟:打 5 条 call-id=ok 的日志,然后 DropCall
	for i := 0; i < 5; i++ {
		log.Info("INVITE TX", "call-id", "ok", "i", i)
	}
	bh.DropCall("ok")

	// 失败通模拟:打 3 条 call-id=fail,然后 FlushCall
	for i := 0; i < 3; i++ {
		log.Info("INVITE RX", "call-id", "fail", "i", i)
	}
	bh.FlushCall("fail")

	// 无 callid 的全局日志:必须直写,跟 OnlyFailed 无关
	log.Warn("transport bind ok")

	got := sink.String()
	if strings.Contains(got, "call-id=ok") {
		t.Errorf("expect ok call dropped, but found in sink:\n%s", got)
	}
	if c := strings.Count(got, "call-id=fail"); c != 3 {
		t.Errorf("expect 3 fail records flushed, got %d:\n%s", c, got)
	}
	if !strings.Contains(got, "transport bind ok") {
		t.Errorf("expect no-callid warn written directly:\n%s", got)
	}
}

// TestBufHandler_FlushAll 验证 pending 通(没拿到 final)退出前 flush 兜底。
func TestBufHandler_FlushAll(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, true)
	log := slog.New(bh)

	log.Info("INVITE TX", "call-id", "pending1")
	log.Info("RX 100", "call-id", "pending1")
	log.Info("INVITE TX", "call-id", "pending2")

	bh.FlushAll()

	got := sink.String()
	if c := strings.Count(got, "call-id=pending1"); c != 2 {
		t.Errorf("pending1 should flush 2 records, got %d", c)
	}
	if !strings.Contains(got, "call-id=pending2") {
		t.Errorf("pending2 should flush")
	}
}

// TestBufHandler_MaxPerCall 验证单通 ring 上限,防失控通无限增长。
func TestBufHandler_MaxPerCall(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, true)
	bh.MaxPerCall = 3
	log := slog.New(bh)
	for i := 0; i < 10; i++ {
		log.Info("noise", "call-id", "stuck", "i", i)
	}
	bh.FlushCall("stuck")
	got := sink.String()
	c := strings.Count(got, "call-id=stuck")
	if c != 3 {
		t.Errorf("expect FIFO trim to 3 records, got %d:\n%s", c, got)
	}
	// 最旧的 i=0..6 应被丢,留 i=7,8,9
	if !strings.Contains(got, "i=9") {
		t.Errorf("newest record missing")
	}
	if strings.Contains(got, "i=0 ") {
		t.Errorf("oldest record should be trimmed:\n%s", got)
	}
}

// TestBufHandler_SettledDrop 验证成功通 DropCall 后,后续同 callid 日志(dialog 尾巴
// 如 ACK/BYE)被直接丢弃,不会被 FlushAll 兜底捞回。
func TestBufHandler_SettledDrop(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, true)
	log := slog.New(bh)

	log.Info("INVITE", "call-id", "c1")
	log.Info("200 OK", "call-id", "c1")
	bh.DropCall("c1") // 成功 final

	// 模拟 BYE/ACK 尾巴
	log.Info("BYE RX", "call-id", "c1")
	log.Info("200 OK BYE", "call-id", "c1")

	bh.FlushAll() // 退出兜底
	if strings.Contains(sink.String(), "call-id=c1") {
		t.Errorf("settled-drop call should never appear in sink:\n%s", sink.String())
	}
}

// TestBufHandler_SettledKeepTransparent 验证失败通 FlushCall 后,后续日志直接透传。
func TestBufHandler_SettledKeepTransparent(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, true)
	log := slog.New(bh)

	log.Info("INVITE", "call-id", "c2")
	log.Info("486 Busy", "call-id", "c2")
	bh.FlushCall("c2") // 失败 final → 落盘并 mark settled

	got1 := sink.String()
	if strings.Count(got1, "call-id=c2") != 2 {
		t.Fatalf("expect 2 records flushed, got %d:\n%s", strings.Count(got1, "call-id=c2"), got1)
	}

	// 后续 ACK 尾巴:应直接透传(不进 buffer 等 FlushAll)
	log.Info("ACK", "call-id", "c2")
	got2 := sink.String()
	if strings.Count(got2, "call-id=c2") != 3 {
		t.Errorf("expect ACK pass-through immediately, got %d records:\n%s",
			strings.Count(got2, "call-id=c2"), got2)
	}
}

// TestBufHandler_ConcurrentRace 用 -race 验证多 goroutine 同时 Handle / Flush / Drop
// 不出 race。模拟真实场景:N 个 worker 拼日志,recorder 异步 FlushCall/DropCall。
func TestBufHandler_ConcurrentRace(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, true)
	log := slog.New(bh)

	const N = 200
	var wg sync.WaitGroup
	// 主"事件生产者":每条记一条带 call-id 的日志
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			log.Info("evt", "call-id", fmt.Sprintf("c%d", i%50))
		}
	}()
	// 跟在后面的"final 通知者":随机 flush/drop
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			cid := fmt.Sprintf("c%d", i%50)
			if i%2 == 0 {
				bh.FlushCall(cid)
			} else {
				bh.DropCall(cid)
			}
		}
	}()
	// 退出兜底 goroutine,随机 FlushAll
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			bh.FlushAll()
		}
	}()
	wg.Wait()
}

// TestRotatingFile_ConcurrentWrites 多 goroutine 同时写 RotatingFile 不出 race。
func TestRotatingFile_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	rf, err := NewRotatingFile(filepath.Join(dir, "r.log"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	const N = 8
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = rf.Write([]byte("aaaaaaaaaaaaaaaaaaaaaaaa\n"))
			}
		}()
	}
	wg.Wait()
}

// TestBufHandler_OnlyFailedFalse_IsTransparent 验证 OnlyFailed=false 时透传到 inner。
func TestBufHandler_OnlyFailedFalse_IsTransparent(t *testing.T) {
	var sink bytes.Buffer
	inner := slog.NewTextHandler(&sink, nil)
	bh := NewBufHandler(inner, false)
	log := slog.New(bh)
	log.Info("hello", "call-id", "anything")
	if !strings.Contains(sink.String(), "hello") {
		t.Errorf("transparent mode lost record: %s", sink.String())
	}
	// 没必要调 Flush/Drop,记录已直写
	_ = context.Background
}
