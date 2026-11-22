//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCursorSnapshotStore 是内存版快照存储（同包测试用）。
type fakeCursorSnapshotStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeCursorSnapshotStore() *fakeCursorSnapshotStore {
	return &fakeCursorSnapshotStore{m: make(map[string][]byte)}
}

func (f *fakeCursorSnapshotStore) Set(_ context.Context, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[fkey(id)] = raw
	return nil
}

func (f *fakeCursorSnapshotStore) Get(_ context.Context, id string, dest any) (bool, error) {
	f.mu.Lock()
	raw, ok := f.m[fkey(id)]
	f.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeCursorSnapshotStore) Del(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, fkey(id))
	return nil
}

func fkey(id string) string { return id }

func newSnapshotTestService() *CursorExecutionService {
	svc := &CursorExecutionService{
		sessions: newCursorSessionStore(),
		now:      time.Now,
	}
	svc.sessionSnapshots = newFakeCursorSnapshotStore()
	return svc
}

func newSnapshotTestDigest(t *testing.T, item string) *CursorHistoryDigest {
	t.Helper()
	d := &CursorHistoryDigest{}
	d.reset()
	require.NoError(t, d.AppendItem("assistant", []byte(item)))
	return d
}

// buildWaitingToolsRecord 构造一条「模型已交付工具批、等待客户端结果」的会话。
func buildWaitingToolsRecord(t *testing.T, svc *CursorExecutionService) *cursorSessionRecord {
	t.Helper()
	spec := cursorSessionSpec{
		protocol:       cursorProtocolResponses,
		requestedModel: "gpt-6.1-sol",
		accountID:      7,
		identity:       CursorSessionIdentity{UserID: 3, APIKeyID: 9},
		plan: &cursorContextPlan{
			tools: []string{"search"},
		},
	}
	record := svc.sessions.start(spec)
	require.NotNil(t, record)
	record.mu.Lock()
	record.configDigest = newSnapshotTestDigest(t, "config")
	record.configSystemDigest = newSnapshotTestDigest(t, "system")
	record.configSamplingDigest = newSnapshotTestDigest(t, "sampling")
	record.historyDigest = newSnapshotTestDigest(t, "history")
	record.exposedTools = []string{"search"}
	record.state = cursorSessionWaitingTools
	record.pending = &cursorPendingBatch{
		batchID:    "batch-1",
		responseID: "resp-1",
		callIDs:    []string{"call-1"},
		delivered:  true,
	}
	record.mu.Unlock()
	return record
}

// TestCursorSessionSnapshotRoundTrip 是端到端验收：写入快照 → 释放会话 → 按
// previous_response_id 恢复 → 摘要/pending/state 与释放前一致，且快照被一次性消费。
func TestCursorSessionSnapshotRoundTrip(t *testing.T) {
	svc := newSnapshotTestService()
	record := buildWaitingToolsRecord(t, svc)
	sessionID := record.id

	snap, ok := cursorSnapshotFromRecord(record)
	require.True(t, ok, "waiting_tools 且已交付未消费的会话必须能生成快照")
	svc.writeCursorSessionSnapshot(snap)

	// 模拟 finalize：关闭内存会话（索引与记录移除）。
	svc.sessions.close(sessionID)
	require.Nil(t, svc.sessions.lookupSessionByProtocolName("responses", CursorIDKindResponse, "resp-1"),
		"内存会话与索引必须已释放")

	restored := svc.restoreSessionFromSnapshot(context.Background(), "responses", "resp-1",
		CursorSessionIdentity{UserID: 3, APIKeyID: 9})
	require.NotNil(t, restored, "带 previous_response_id 的续轮必须能从快照恢复")
	require.Equal(t, cursorSessionWaitingTools, restored.state)
	require.Equal(t, "resp-1", restored.pending.responseID)
	require.Equal(t, "batch-1", restored.pending.batchID)
	require.True(t, restored.pending.delivered)
	require.Equal(t, []string{"search"}, restored.exposedTools)
	require.NotNil(t, restored.configDigest)
	require.NotNil(t, restored.historyDigest)
	require.True(t, restored.configDigest.Equal(snapConfigDigest(t, "config")))

	// 一次性消费：快照键已删除，再恢复必须失败。
	restoredAgain := svc.restoreSessionFromSnapshot(context.Background(), "responses", "resp-1",
		CursorSessionIdentity{UserID: 3, APIKeyID: 9})
	require.Nil(t, restoredAgain, "快照必须一次性消费")
}

func snapConfigDigest(t *testing.T, item string) *CursorHistoryDigest {
	t.Helper()
	d := &CursorHistoryDigest{}
	d.reset()
	require.NoError(t, d.AppendItem("assistant", []byte(item)))
	return d
}

// TestCursorSessionSnapshotIdentityMismatch 锁定身份闸门：不同调用方不能恢复他人会话。
func TestCursorSessionSnapshotIdentityMismatch(t *testing.T) {
	svc := newSnapshotTestService()
	record := buildWaitingToolsRecord(t, svc)
	snap, ok := cursorSnapshotFromRecord(record)
	require.True(t, ok)
	svc.writeCursorSessionSnapshot(snap)
	svc.sessions.close(record.id)

	restored := svc.restoreSessionFromSnapshot(context.Background(), "responses", "resp-1",
		CursorSessionIdentity{UserID: 999})
	require.Nil(t, restored, "身份不一致必须拒绝恢复")
}

// TestCursorSessionSnapshotSkipsNonRecoverable 锁定不写快照的终态。
func TestCursorSessionSnapshotSkipsNonRecoverable(t *testing.T) {
	require.False(t, cursorSessionSnapshotRecoverableReason("completed"))
	require.False(t, cursorSessionSnapshotRecoverableReason("cancelled"))
	require.True(t, cursorSessionSnapshotRecoverableReason("waiting_tools_idle"))
}

// TestCursorSessionSnapshotWriteFailureDoesNotPanic 锁定存储不可用不阻断 finalize。
func TestCursorSessionSnapshotWriteFailureDoesNotPanic(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	svc.sessionSnapshots = &failingSnapshotStore{}
	record := buildWaitingToolsRecord(t, svc)
	require.NotPanics(t, func() { svc.writeCursorSessionSnapshot(&cursorSessionSnapshot{SessionID: record.id}) })
}

type failingSnapshotStore struct{}

func (f *failingSnapshotStore) Set(context.Context, string, any) error {
	return errors.New("unavailable")
}
func (f *failingSnapshotStore) Get(context.Context, string, any) (bool, error) {
	return false, errors.New("unavailable")
}
func (f *failingSnapshotStore) Del(context.Context, string) error { return errors.New("unavailable") }
