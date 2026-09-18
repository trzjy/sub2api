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

// webPlatformMigrationDirection 迁移方向。
const (
	WebPlatformMigrationDirectionUp   = "up"
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

// legacyWebPlatformTargetMapping web-* 旧平台 → 官方平台（方案 A §3）。
var legacyWebPlatformTargetMapping = map[string]string{
	string(PlatformWebZhipu):    string(PlatformZhipu),
	string(PlatformWebDeepseek): string(PlatformDeepseek),
	string(PlatformWebKimi):     string(PlatformKimi),
}

// MigrateWebPlatformAccounts 执行 web-* 平台迁移 reconciler。
// direction=up：把仍挂在 web-* 旧平台的账号迁移到官方平台并显式化 access_mode=web；
// direction=down：按 migrated_from_platform 标记回滚。
// dryRun=true 只做核对分类，不写库。
// 失败关闭：任一账号失败即中止，已处理账号三字段一致，续跑幂等补完。
// webPlatformMigrationRepository 迁移写路径依赖 repository 层专用事务方法，
// 由构造时的窄接口断言注入；未注入即失败关闭，绝不走普通 repo 调用拼接。
func (s *adminServiceImpl) webPlatformMigrationRepository() (WebPlatformMigrationRepository, error) {
	if s.webPlatformMigrationRepo == nil {
		return nil, fmt.Errorf("web platform migration repository not available (fail-closed)")
	}
	return s.webPlatformMigrationRepo, nil
}

func (s *adminServiceImpl) MigrateWebPlatformAccounts(
	ctx context.Context,
	direction string,
	dryRun bool,
) (*WebPlatformMigrationReport, error) {
	switch direction {
	case WebPlatformMigrationDirectionUp:
		return s.runWebPlatformMigrationUp(ctx, dryRun)
	case WebPlatformMigrationDirectionDown:
		return s.runWebPlatformMigrationDown(ctx, dryRun)
	default:
		return nil, fmt.Errorf("invalid migration direction %q (must be up or down)", direction)
	}
}

func (s *adminServiceImpl) runWebPlatformMigrationUp(ctx context.Context, dryRun bool) (*WebPlatformMigrationReport, error) {
	migrationRepo, err := s.webPlatformMigrationRepository()
	if err != nil {
		return nil, err
	}
	accounts, err := migrationRepo.ListLegacyWebPlatformAccounts(ctx)
	if err != nil {
		return nil, err
	}
	report := &WebPlatformMigrationReport{
		Direction: WebPlatformMigrationDirectionUp,
		DryRun:    dryRun,
		Total:     len(accounts),
		Migrated:  []WebPlatformMigrationEntry{},
		Skipped:   []WebPlatformMigrationEntry{},
	}
	for _, account := range accounts {
		toPlatform, ok := legacyWebPlatformTargetMapping[account.Platform]
		if !ok {
			return nil, fmt.Errorf("account %d has unmapped legacy web platform %q", account.ID, account.Platform)
		}
		entry := WebPlatformMigrationEntry{
			ID:           account.ID,
			Name:         account.Name,
			FromPlatform: account.Platform,
			ToPlatform:   toPlatform,
		}
		if dryRun {
			// dry-run 只核对不写库。legacy 平台账号全部是迁移候选：
			// up 的幂等跳过只发生在「官方平台 + marker」的账号上，那类
			// 账号不会出现在 ListLegacyWebPlatformAccounts 的结果里；
			// 不能用 GetAccessMode()（形状推断恒为 web）判定已归并。
			report.Migrated = append(report.Migrated, entry)
			continue
		}
		migrated, err := migrationRepo.MigrateAccountPlatform(ctx, account.ID, account.Platform, toPlatform)
		if err != nil {
			// 失败关闭：中止并携带账号上下文，已处理账号三字段一致。
			return nil, fmt.Errorf("migrate account %d (%s): %w", account.ID, account.Platform, err)
		}
		if migrated {
			report.Migrated = append(report.Migrated, entry)
		} else {
			entry.AlreadyMerged = true
			report.Skipped = append(report.Skipped, entry)
		}
	}
	return report, nil
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
