package service

import (
	"context"
	"fmt"
)

// 平台归并重构 PR-2（docs/platform-merge-refactor-plan.md §5.8）：
// web-* 旧平台账号迁移的应用层一次性 reconciler。
//
// 本文件只做编排（列账号 → 逐账号调用 repository 层专用事务方法 → 失败关闭）；
// 三字段（platform / credentials["access_mode"] / extra["migrated_from_platform"]）
// 与 MAC、scheduler outbox 的原子变更一律由 repository 层专用事务方法完成，
// 禁止在这里拼接多个普通 repo 调用模拟原子性。

// webPlatformMigrationDirection 迁移方向（PR-4 旧链归零：up 已退役，仅保留 down 逆推入口，方案 §7）。
const (
	WebPlatformMigrationDirectionDown = "down"
)

// WebPlatformMigrationEntry 迁移报告单行。
type WebPlatformMigrationEntry struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FromPlatform  string `json:"from_platform"`
	ToPlatform    string `json:"to_platform"`
	AlreadyMerged bool   `json:"already_merged"`
}

// WebPlatformMigrationReport reconciler 执行报告。
type WebPlatformMigrationReport struct {
	Direction string                      `json:"direction"`
	DryRun    bool                        `json:"dry_run"`
	Total     int                         `json:"total"`
	Migrated  []WebPlatformMigrationEntry `json:"migrated"`
	Skipped   []WebPlatformMigrationEntry `json:"skipped"`
}

// webPlatformMigrationRepository 返回迁移写路径依赖的 repository 层专用事务方法窄接口；
// 未注入（测试替身未实现）即失败关闭，绝不走普通 repo 调用拼接模拟原子性。
func (s *adminServiceImpl) webPlatformMigrationRepository() (WebPlatformMigrationRepository, error) {
	if s.webPlatformMigrationRepo == nil {
		return nil, fmt.Errorf("web platform migration repository not available (fail-closed)")
	}
	return s.webPlatformMigrationRepo, nil
}

// MigrateWebPlatformAccounts 执行平台归并迁移 reconciler（PR-4 旧链归零后仅保留 down
// 逆推入口：web-* 旧平台账号生产已为 0，up 迁移路径为死代码，已随本 PR 移除；down 按
// migrated_from_platform 标记回滚，三字段原子变更由 repository 层专用事务方法完成）。
func (s *adminServiceImpl) MigrateWebPlatformAccounts(
	ctx context.Context,
	direction string,
	dryRun bool,
) (*WebPlatformMigrationReport, error) {
	switch direction {
	case WebPlatformMigrationDirectionDown:
		return s.runWebPlatformMigrationDown(ctx, dryRun)
	default:
		return nil, fmt.Errorf("invalid migration direction %q (only down is supported)", direction)
	}
}

func (s *adminServiceImpl) runWebPlatformMigrationDown(ctx context.Context, dryRun bool) (*WebPlatformMigrationReport, error) {
	migrationRepo, err := s.webPlatformMigrationRepository()
	if err != nil {
		return nil, err
	}
	accounts, err := migrationRepo.ListMigratedFromWebPlatformAccounts(ctx)
	if err != nil {
		return nil, err
	}
	report := &WebPlatformMigrationReport{
		Direction: WebPlatformMigrationDirectionDown,
		DryRun:    dryRun,
		Total:     len(accounts),
		Migrated:  []WebPlatformMigrationEntry{},
		Skipped:   []WebPlatformMigrationEntry{},
	}
	for _, account := range accounts {
		fromPlatform := account.GetExtraString("migrated_from_platform")
		entry := WebPlatformMigrationEntry{
			ID:           account.ID,
			Name:         account.Name,
			FromPlatform: account.Platform,
			ToPlatform:   fromPlatform,
		}
		if dryRun {
			report.Migrated = append(report.Migrated, entry)
			continue
		}
		reverted, err := migrationRepo.RevertMigratedAccountPlatform(ctx, account.ID)
		if err != nil {
			return nil, fmt.Errorf("revert account %d (%s): %w", account.ID, account.Platform, err)
		}
		if reverted {
			report.Migrated = append(report.Migrated, entry)
		} else {
			entry.AlreadyMerged = true
			report.Skipped = append(report.Skipped, entry)
		}
	}
	return report, nil
}
