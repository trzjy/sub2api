package xianguanjia

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeMarker 记录被标记的 card_no，返回固定标记结果。
type fakeMarker struct {
	mu     sync.Mutex
	marked [][]string
	res    map[string]string
	err    error
}

func (f *fakeMarker) MarkPushedToXianyu(ctx context.Context, cardNos []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, append([]string(nil), cardNos...))
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		if f.res != nil {
			out[no] = f.res[no]
		} else {
			out[no] = "marked"
		}
	}
	return out, nil
}

func (f *fakeMarker) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.marked...)
}

// newStorageMock 构造只处理 storage/create 的 mock：handler 收到请求体后交给 respond。
func newStorageMock(t *testing.T, respond func(body []byte) (int, string)) (*Client, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathKamiStorageCreate {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		bodies = append(bodies, raw)
		status, payload := respond(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{
		BaseURL:    srv.URL,
		AppKey:     fakeAppKey,
		AppSecret:  fakeAppSecret,
		HTTPClient: srv.Client(),
	})
	return c, &bodies
}

func TestPushCardsWholeOrderSuccess(t *testing.T) {
	client, bodies := newStorageMock(t, func(body []byte) (int, string) {
		// 压缩 JSON 强校验：逐字节比对请求体。
		want := `{"kind_id":42,"cards":[{"card_no":"C1","card_pwd":"P1"},{"card_no":"C2","card_pwd":"P2"}]}`
		if string(body) != want {
			t.Errorf("request body mismatch:\n got: %s\nwant: %s", body, want)
		}
		return 200, `{"code":0,"msg":"ok","data":{}}`
	})
	marker := &fakeMarker{}
	svc := NewPoolSyncService(client, marker)

	res, err := svc.PushCards(context.Background(), 42, []CardPair{
		{CardNo: "C1", CardPwd: "P1"},
		{CardNo: "C2", CardPwd: "P2"},
	})
	if err != nil {
		t.Fatalf("PushCards returned error: %v", err)
	}
	if len(res.Succeeded) != 2 || len(res.Failed) != 0 {
		t.Fatalf("expected 2 succeeded 0 failed, got %+v", res)
	}
	// 推仓成功必须做渠道标记，且覆盖全部成功卡。
	calls := marker.calls()
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "C1" || calls[0][1] != "C2" {
		t.Fatalf("marker calls mismatch: %v", calls)
	}
	if res.Mark["C1"] != "marked" || res.Mark["C2"] != "marked" {
		t.Fatalf("mark result mismatch: %v", res.Mark)
	}
	if len(*bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*bodies))
	}
}

func TestPushCardsWholeOrderRejected(t *testing.T) {
	client, _ := newStorageMock(t, func(body []byte) (int, string) {
		return 200, `{"code":1001,"msg":"kind not found"}`
	})
	marker := &fakeMarker{}
	svc := NewPoolSyncService(client, marker)

	cards := []CardPair{{CardNo: "C1", CardPwd: "P1"}, {CardNo: "C2", CardPwd: "P2"}}
	res, err := svc.PushCards(context.Background(), 42, cards)
	if err != nil {
		t.Fatalf("whole-order rejection should be a business result, got error: %v", err)
	}
	// 整单拒绝：全部失败、无成功、透传官方 msg。
	if len(res.Succeeded) != 0 || len(res.Failed) != 2 {
		t.Fatalf("expected 0 succeeded 2 failed, got %+v", res)
	}
	if res.Message != "kind not found" {
		t.Fatalf("message mismatch: %q", res.Message)
	}
	// 整单失败不得做任何渠道标记。
	if len(marker.calls()) != 0 {
		t.Fatalf("marker must not be called on whole-order rejection: %v", marker.calls())
	}
}

func TestPushCardsPartialFailureAndRetrySubset(t *testing.T) {
	call := 0
	client, bodies := newStorageMock(t, func(body []byte) (int, string) {
		call++
		if call == 1 {
			// 第一次：C2 失败（HTTP 成功，data 带失败明细）。
			return 200, `{"code":0,"msg":"ok","data":{"failed":[{"card_no":"C2","reason":"duplicate"}]}}`
		}
		// 第二次（重试）：请求体必须只含失败子集 C2。
		want := `{"kind_id":42,"cards":[{"card_no":"C2","card_pwd":"P2"}]}`
		if string(body) != want {
			t.Errorf("retry body must contain only failed subset:\n got: %s\nwant: %s", body, want)
		}
		return 200, `{"code":0,"msg":"ok","data":{}}`
	})
	marker := &fakeMarker{}
	svc := NewPoolSyncService(client, marker)

	cards := []CardPair{
		{CardNo: "C1", CardPwd: "P1"},
		{CardNo: "C2", CardPwd: "P2"},
		{CardNo: "C3", CardPwd: "P3"},
	}
	res, err := svc.PushCards(context.Background(), 42, cards)
	if err != nil {
		t.Fatalf("PushCards returned error: %v", err)
	}
	// 部分失败：C1/C3 成功，C2 失败；只标记成功子集。
	if len(res.Succeeded) != 2 || res.Succeeded[0].CardNo != "C1" || res.Succeeded[1].CardNo != "C3" {
		t.Fatalf("succeeded mismatch: %+v", res.Succeeded)
	}
	if len(res.Failed) != 1 || res.Failed[0].CardNo != "C2" {
		t.Fatalf("failed mismatch: %+v", res.Failed)
	}
	calls := marker.calls()
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "C1" || calls[0][1] != "C3" {
		t.Fatalf("marker must mark only succeeded subset: %v", calls)
	}

	// 重试：调用方只传失败子集（不重复推已成功的卡）。
	res2, err := svc.PushCards(context.Background(), 42, res.Failed)
	if err != nil {
		t.Fatalf("retry PushCards returned error: %v", err)
	}
	if len(res2.Succeeded) != 1 || res2.Succeeded[0].CardNo != "C2" || len(res2.Failed) != 0 {
		t.Fatalf("retry result mismatch: %+v", res2)
	}
	if len(*bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*bodies))
	}
	calls = marker.calls()
	if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0] != "C2" {
		t.Fatalf("retry marker calls mismatch: %v", calls)
	}
}

func TestPushCardsServiceUnreachable(t *testing.T) {
	// 建好后立即关停，模拟服务不可达（连接拒绝）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	client := NewClient(ClientConfig{
		BaseURL:   srv.URL,
		AppKey:    fakeAppKey,
		AppSecret: fakeAppSecret,
	})
	marker := &fakeMarker{}
	svc := NewPoolSyncService(client, marker)

	_, err := svc.PushCards(context.Background(), 42, []CardPair{{CardNo: "C1", CardPwd: "P1"}})
	if err == nil {
		t.Fatalf("expected transport error for unreachable service")
	}
	// 传输层故障不得误报为整单拒绝，也不得做渠道标记。
	var rejected *StorageCreateRejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("transport error must not be classified as rejection: %v", err)
	}
	if len(marker.calls()) != 0 {
		t.Fatalf("marker must not be called on transport failure")
	}
}

func TestPushCardsEmptyCardsRejected(t *testing.T) {
	called := false
	client, _ := newStorageMock(t, func(body []byte) (int, string) {
		called = true
		return 200, `{"code":0,"msg":"ok"}`
	})
	marker := &fakeMarker{}
	svc := NewPoolSyncService(client, marker)

	if _, err := svc.PushCards(context.Background(), 42, nil); err == nil {
		t.Fatalf("expected error for empty cards")
	}
	if called {
		t.Fatalf("no request should be sent for empty cards")
	}
	if len(marker.calls()) != 0 {
		t.Fatalf("marker must not be called for empty cards")
	}
	// kind_id 非法同样拒绝。
	if _, err := svc.PushCards(context.Background(), 0, []CardPair{{CardNo: "C1"}}); err == nil {
		t.Fatalf("expected error for invalid kind_id")
	}
}

func TestPushCardsMarkFailureSurfacesError(t *testing.T) {
	client, _ := newStorageMock(t, func(body []byte) (int, string) {
		return 200, `{"code":0,"msg":"ok","data":{}}`
	})
	marker := &fakeMarker{err: errors.New("db down")}
	svc := NewPoolSyncService(client, marker)

	// 推仓成功但本侧标记失败：必须显式上抛错误（存在双卖窗口），
	// 且 PushResult 仍如实报告官方成功。
	res, err := svc.PushCards(context.Background(), 42, []CardPair{{CardNo: "C1", CardPwd: "P1"}})
	if err == nil {
		t.Fatalf("expected error when channel marking fails")
	}
	if len(res.Succeeded) != 1 {
		t.Fatalf("push result should still report remote success: %+v", res)
	}
}
