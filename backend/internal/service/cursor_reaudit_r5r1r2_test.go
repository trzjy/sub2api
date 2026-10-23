//go:build unit

package service

// 本文件是 W2-A 派发单验收的**定向反例测试**：逐项复现验收报告 R3 / R5-剩余 / R6-exec 的
// 反例并验证修复后转绿。反例即为红绿基准。helper 均来自本包既有测试设施，不新增兜底机制。
//
// 范围仅覆盖本派发单三项：
//   - R3 / A08：已命中但混入未知 call ID 必须 400 拒绝，不得复用全未知哨兵；
//   - R5-剩余 / R5-3：item 关联冲突检查必须在成功结束帧之前（写前准备，零成功字节）；
//   - R6-exec / A09：Stop 两空隙（start→attachOwner 之间作废；finalizer 等待不设 10 秒上限）。
//
// R1（计费四字段存在性）与 R2（身份先于资格）属其它派发单，不在本文件覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R5-3：item 关联冲突检查必须在成功结束帧之前（写前准备）
// ---------------------------------------------------------------------------

type reauditItemConflictWriter struct {
	gin.ResponseWriter
	svc      *CursorExecutionService
	injected bool
}

func (w *reauditItemConflictWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *reauditItemConflictWriter) Write(p []byte) (int, error) {
	if !w.injected {
		for _, line := range strings.Split(string(p), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event struct {
				Item *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil ||
				event.Item == nil || event.Item.Type != "function_call" {
				continue
			}
			w.svc.sessions.mu.Lock()
			for _, r := range w.svc.sessions.sessions {
				key := cursorSessionIndexKeyForSpec(r.spec, CursorIDKindItem, event.Item.ID)
				if w.svc.sessions.index.Register(key, r.id, "injected_conflicting_batch") == nil {
					w.injected = true
				}
				break
			}
			w.svc.sessions.mu.Unlock()
		}
	}
	return w.ResponseWriter.Write(p)
}

func TestReauditItemConflictMustPrecedeSuccessfulOutput(t *testing.T) {
	calls := []cursorFullChainCall{{
		CallID: "call_conflict", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101"),
	}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{
		calls: calls, hangAfterPhase1: true,
	}))
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"cursor-sonnet","stream":true,"input":"weather please","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`), &req))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := &reauditItemConflictWriter{ResponseWriter: c.Writer, svc: svc}
	c.Writer = writer
	_, err := cursorExecuteResponses(context.Background(), c, svc, cursorFullChainAccount(), &req)
	require.True(t, writer.injected)
	require.Error(t, err)
	t.Logf("error=%v success_marker_already_sent=%v", err, strings.Contains(recorder.Body.String(), "response.completed"))
	require.NotContains(t, recorder.Body.String(), "response.completed", "item conflict must be detected before successful completion bytes")
}

// ---------------------------------------------------------------------------
// R5-2：同一 call 在 added/done 与 completed 里对应同一个 item ID
// ---------------------------------------------------------------------------

func TestReauditResponsesToolItemIDStableThroughCompleted(t *testing.T) {
	calls := []cursorFullChainCall{{
		CallID: "call_id_stable", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101"),
	}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{
		calls: calls, hangAfterPhase1: true,
	}))
	w, err := responsesFirstHTTP(t, svc, true)
	require.NoError(t, err)
	ids := map[string]struct{}{}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Item *struct {
				ID     string `json:"id"`
				CallID string `json:"call_id"`
			} `json:"item"`
			Response *struct {
				Output []struct {
					ID     string `json:"id"`
					CallID string `json:"call_id"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if event.Item != nil && event.Item.CallID == "call_id_stable" {
			ids[event.Item.ID] = struct{}{}
		}
		if event.Response != nil {
			for _, item := range event.Response.Output {
				if item.CallID == "call_id_stable" {
					ids[item.ID] = struct{}{}
				}
			}
		}
	}
	t.Logf("item_ids_for_one_call=%v", ids)
	require.Len(t, ids, 1, "same HTTP added/done/completed must carry one actual tool item ID")
}

// ---------------------------------------------------------------------------
// R3 / A08：已命中但混入未知 call ID 必须 400 拒绝，不得复用全未知哨兵
// （全未知哨兵只交原路由判定供应方，会让混合续轮逃到普通/非 Cursor 供应方）。
// ---------------------------------------------------------------------------

func TestReauditMixedKnownUnknownMustNotReturnAllUnknownSentinel(t *testing.T) {
	f := newCursorResumeGateFixture(t)
	f.probe.ResultCallIDs = append(f.probe.ResultCallIDs, "unknown_extra_call")
	ticket, err := f.svc.IdentifyResume(f.probe)
	require.Error(t, err)
	require.Nil(t, ticket)
	t.Logf("error=%v helper_would_route_normally=%v", err, errors.Is(err, ErrCursorResumeUnknownAssociation))
	require.NotErrorIs(t, err, ErrCursorResumeUnknownAssociation,
		"mixed IDs (some hit, some unknown) cannot use helper's all-unknown escape")
}

// ---------------------------------------------------------------------------
// R6-exec / A09 空隙1：start 与 attachOwner 之间被 Stop 的游离记录必须作废，
// attachHTTP 明确失败（零 AgentRun、零游离状态）。
// ---------------------------------------------------------------------------

func TestReauditStopBetweenRegistrationAndOwnerMustInvalidateRecord(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := svc.sessions.start(cursorSessionSpec{protocol: cursorProtocolChat})
	require.NotNil(t, record)
	svc.Stop() // Exact interleaving allowed between Execute's start and attachOwner calls.
	svc.sessions.attachOwner(record, svc)
	detach, err := record.attachHTTP(context.Background())
	if detach != nil {
		defer detach()
	}
	t.Logf("store_sessions=%v attach_after_stop_error=%v", svc.sessions.sessionCount(), err)
	defer svc.finishCursorSession(record, "reaudit_cleanup")
	require.Error(t, err, "a stopped pre-owner record must not remain executable after Stop")
}

// ---------------------------------------------------------------------------
// R6-exec / A09 空隙2：Stop 必须等进行中的 finalizer 完成结算/资源归还，不设 10 秒上限，
// 不得把未完成包装成成功。
// ---------------------------------------------------------------------------

func TestReauditStopMustNotReturnWhilePriorFinalizerBlocked(t *testing.T) {
	f := newCursorStopFixture(t, true)
	defer f.release()
	finalDone := make(chan struct{})
	go func() {
		f.svc.finishCursorSession(f.record, "reaudit_prior_finalizer")
		close(finalDone)
	}()
	<-f.settleEntered
	stopDone := make(chan struct{})
	go func() {
		f.svc.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		select {
		case <-finalDone:
			t.Fatal("test barrier was bypassed")
		default:
			t.Logf("Stop returned with blocked finalizer: sessions=%v indexes=%v releases=%v",
				f.svc.sessions.sessionCount(), f.svc.sessions.indexCount(), f.resources[0].releases.Load())
			t.Error("Stop reported completion before settlement submission and resource return")
		}
	case <-time.After(cursorStopFinalizeWaitTimeout + time.Second):
		// Safe outcome: Stop remains blocked on the deliberately blocked finalizer.
	}
	f.release()
	<-finalDone
	<-stopDone
}
