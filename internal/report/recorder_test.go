package report

import (
	"fmt"
	"sync"
	"testing"

	"dsipper/internal/sipua"
)

// fakeMsg 拼最小可用 sipua.Message,用于 Recorder.Record 喂入。
func fakeMsg(callID, cseq string, isReq bool, method string, status int) *sipua.Message {
	m := &sipua.Message{
		IsRequest: isReq,
		Headers:   sipua.NewHeaders(),
	}
	if isReq {
		m.Method = method
		m.RURI = "sip:x@example.com"
	} else {
		m.StatusCode = status
		m.ReasonPhrase = "OK"
	}
	m.Headers.Add("Call-ID", callID)
	m.Headers.Add("CSeq", cseq)
	m.Headers.Add("From", "<sip:caller@x>;tag=ftag")
	m.Headers.Add("To", "<sip:callee@x>")
	return m
}

// TestRecorder_KeepOnlyFailed_DropsSuccess 验证 2xx INVITE final 立即 drop,
// 顶部汇总仍计数,calls 表内此 callid 移除。
func TestRecorder_KeepOnlyFailed_DropsSuccess(t *testing.T) {
	r := New("test", "t")
	r.Record("TX", fakeMsg("c1", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c1", "1 INVITE", false, "", 200), "")

	snap := r.Snapshot()
	if snap.Total != 1 || snap.OK != 1 || snap.Fail != 0 {
		t.Fatalf("snap=%+v want total=1 ok=1 fail=0", snap)
	}
	if _, ok := r.calls["c1"]; ok {
		t.Errorf("expected calls['c1'] dropped after 200 OK")
	}
}

// TestRecorder_FailKept 验证 ≥300 final 保留信令图,失败 +1,calls 仍含此 callid。
func TestRecorder_FailKept(t *testing.T) {
	r := New("test", "t")
	r.Record("TX", fakeMsg("c1", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c1", "1 INVITE", false, "", 486), "")
	snap := r.Snapshot()
	if snap.Total != 1 || snap.OK != 0 || snap.Fail != 1 {
		t.Fatalf("snap=%+v want total=1 fail=1", snap)
	}
	if _, ok := r.calls["c1"]; !ok {
		t.Errorf("failed call should be kept in calls map")
	}
}

// TestRecorder_FailOverflow 验证超过 MaxFailedKept 的失败通只算汇总,详情区不留。
func TestRecorder_FailOverflow(t *testing.T) {
	r := New("test", "t")
	r.MaxFailedKept = 3
	for i := 0; i < 10; i++ {
		cid := fmt.Sprintf("c%d", i)
		r.Record("TX", fakeMsg(cid, "1 INVITE", true, "INVITE", 0), "")
		r.Record("RX", fakeMsg(cid, "1 INVITE", false, "", 486), "")
	}
	snap := r.Snapshot()
	if snap.Total != 10 || snap.Fail != 10 {
		t.Fatalf("snap=%+v want total=10 fail=10", snap)
	}
	if got := len(r.calls); got != 3 {
		t.Errorf("calls len=%d want 3 (MaxFailedKept)", got)
	}
	if r.failedDropped != 7 {
		t.Errorf("failedDropped=%d want 7", r.failedDropped)
	}
}

// TestRecorder_NonInviteTailIgnored 验证 INVITE 200 后的 ACK/BYE/200(BYE) 不重建 callid,
// 不污染 totalCalls。
func TestRecorder_NonInviteTailIgnored(t *testing.T) {
	r := New("test", "t")
	r.Record("TX", fakeMsg("c1", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c1", "1 INVITE", false, "", 200), "")
	// 200 OK 后 ACK / BYE / 200(BYE)
	r.Record("TX", fakeMsg("c1", "1 ACK", true, "ACK", 0), "")
	r.Record("RX", fakeMsg("c1", "2 BYE", true, "BYE", 0), "")
	r.Record("TX", fakeMsg("c1", "2 BYE", false, "", 200), "")
	snap := r.Snapshot()
	if snap.Total != 1 || snap.OK != 1 || snap.Pending != 0 {
		t.Fatalf("snap=%+v want total=1 ok=1 pending=0", snap)
	}
}

// TestRecorder_CancelByeFinalNotConfused 验证 BYE/CANCEL 事务的 200 不覆盖 INVITE 事务 final。
func TestRecorder_CancelByeFinalNotConfused(t *testing.T) {
	r := New("test", "t")
	r.MaxFailedKept = 100
	r.Record("TX", fakeMsg("c1", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c1", "1 INVITE", false, "", 486), "") // 真 final
	// 一个 BYE 200(应该不影响 c1 的 final status)
	r.Record("TX", fakeMsg("c1", "2 BYE", true, "BYE", 0), "")
	r.Record("RX", fakeMsg("c1", "2 BYE", false, "", 200), "")
	c := r.calls["c1"]
	if c == nil {
		t.Fatal("c1 should still be in calls")
	}
	if c.FinalStatus != 486 {
		t.Errorf("FinalStatus=%d want 486 (BYE 200 should not overwrite)", c.FinalStatus)
	}
}

// TestRecorder_ConcurrentRecord 用 -race 验证 1000 通 × 4 goroutine 并发喂入不出 race,
// 且 totalCalls / OK 计数准确。
func TestRecorder_ConcurrentRecord(t *testing.T) {
	r := New("test", "t")
	const callsPerWorker = 250
	const workers = 4
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < callsPerWorker; i++ {
				cid := fmt.Sprintf("w%d-c%d", wid, i)
				r.Record("TX", fakeMsg(cid, "1 INVITE", true, "INVITE", 0), "")
				r.Record("RX", fakeMsg(cid, "1 INVITE", false, "", 200), "")
			}
		}(w)
	}
	wg.Wait()
	snap := r.Snapshot()
	want := int64(callsPerWorker * workers)
	if snap.Total != want || snap.OK != want {
		t.Errorf("snap=%+v want total=%d ok=%d", snap, want, want)
	}
	if snap.Fail != 0 || snap.Pending != 0 {
		t.Errorf("snap=%+v want fail=0 pending=0", snap)
	}
}

// TestRecorder_SaveHTML_Renders 简单端到端:跑几通失败,SaveHTML 出可读 HTML 不崩。
func TestRecorder_SaveHTML_Renders(t *testing.T) {
	r := New("test", "Robust HTML smoke")
	r.MaxFailedKept = 5
	r.Record("TX", fakeMsg("c1", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c1", "1 INVITE", false, "", 486), "")
	r.Record("TX", fakeMsg("c2", "1 INVITE", true, "INVITE", 0), "")
	r.Record("RX", fakeMsg("c2", "1 INVITE", false, "", 200), "") // success drop
	dir := t.TempDir()
	path, err := r.SaveHTML(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("empty path")
	}
}
