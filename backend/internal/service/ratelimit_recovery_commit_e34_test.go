//go:build unit

package service

// E34 定向测试：commitRecoveryAtomically 恢复提交后快照同步失败「不得」误判为提交失败。
//
// 背景：commitRecoveryAtomically 在事务「已提交」后执行快照同步，原实现把同步失败（含
// 窄面缺失断言）仍 return error；调用方 probeOneTokenHarbor 会把该 error 当作「观测提交失败」，
// 把实际已恢复成功的条目改写为 Unclassified——冻结上界不清除、R2 失败计数错增、调度缓存与
// 权威状态分裂。本单修正为区分「事务提交失败」与「提交后同步失败」。
//
// 修复语义：
//  1. schedulerSnapshotSyncRepository 窄面能力断言前移到事务之前；缺失时仍明确失败关闭
//     （此时事务未开启、commit 闭包未执行，无状态写入，error 返回合法）。
//  2. WithObservationTx 成功后执行 SyncSchedulerAccountSnapshot，失败时打结构化 Warn 日志，
//     然后 return nil（事务已确认提交、恢复已成功，调用方必须走成功路径）。
//  3. WithObservationTx 返回 err 失败路径保持原样透传。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// e34NoSnapshotRepo 实现 observationTxRepository 但「不」实现 schedulerSnapshotSyncRepository，
// 用于断言「窄面缺失前移到事务之前」：此时事务未开启、commit 闭包未被调用、无状态写入。
type e34NoSnapshotRepo struct {
	mockAccountRepoForGemini
	txCalled bool
}

func (r *e34NoSnapshotRepo) WithObservationTx(_ context.Context, fn func(txCtx context.Context) error) error {
	r.txCalled = true
	return fn(context.Background())
}

// 场景一：窄面能力缺失。断言返回明确错误，且事务未开启、commit 闭包未被调用（无状态写入）。
func TestCommitRecovery_NarrowInterfaceMissingBeforeTx(t *testing.T) {
	repo := &e34NoSnapshotRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: make(map[int64]*Account)},
	}
	svc := &RateLimitService{accountRepo: repo}

	commitCalled := false
	err := svc.commitRecoveryAtomically(context.Background(), 42, "S", false, func(txCtx context.Context) error {
		commitCalled = true
		return nil
	})
	require.Error(t, err, "窄面缺失必须返回明确错误")
	require.Contains(t, err.Error(), "scheduler snapshot sync narrow interface", "错误须指向快照同步窄面缺失")
	require.False(t, repo.txCalled, "窄面缺失时事务不得开启（断言前移到 WithObservationTx 之前）")
	require.False(t, commitCalled, "窄面缺失时 commit 闭包不得被调用（无任何状态写入）")
}

// 场景二：提交后快照同步失败。断言返回 nil（不得误判为提交失败），且事务内提交（闭包已执行）
// 成功路径不被改写；快照同步仍须被调用（错误仅可观测）。
func TestCommitRecovery_PostCommitSyncFailureReturnsNil(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}

	commitCalled := false
	repo.syncSnapshotErr = errors.New("simulated snapshot sync failure")
	err := svc.commitRecoveryAtomically(context.Background(), 42, "S", false, func(txCtx context.Context) error {
		commitCalled = true
		return nil
	})
	require.NoError(t, err, "提交后同步失败不得误判为提交失败，必须返回 nil（事务已提交、恢复已成功）")
	require.True(t, commitCalled, "事务内提交（闭包）已执行，成功路径不得被改写")
	require.Equal(t, 1, repo.syncSnapshotCalls, "快照同步仍须被调用（错误在同步内产生，仅可观测）")
}

// 场景三：事务失败。断言错误照常透传，且快照同步不得被调用（提交未成功）。
func TestCommitRecovery_TransactionFailurePropagates(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}

	txErr := errors.New("simulated transaction failure")
	err := svc.commitRecoveryAtomically(context.Background(), 42, "S", false, func(txCtx context.Context) error {
		return txErr
	})
	require.ErrorIs(t, err, txErr, "事务失败必须照常透传 error（调用方据此回滚改写）")
	require.Equal(t, 0, repo.syncSnapshotCalls, "事务未提交，快照同步不得被调用")
}
