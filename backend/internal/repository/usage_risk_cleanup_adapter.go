package repository

import (
	"context"
	"database/sql"
	"time"
)

// CleanupReportAndRollupTxDB 是 U3 交付的 CleanupReportAndRollupTx 的类型桥接适配。
//
// U3 的 CleanupReportAndRollupTx 以未导出的 sqlExecutor 为参数（*sql.DB 或 *sql.Tx 均满足），
// service 包无法引用未导出的 sqlExecutor，因此这里以导出的 *sql.Tx 暴露同一删除段，
// 供 dashboard_aggregation_service 在保留期协调的同一 DB 事务内调用。
//
// 禁区遵守：本文件只做类型桥接，不修改 U3 的 CleanupReportAndRollupTx 实现本身。
// *sql.Tx 满足 sqlExecutor 接口（ExecContext + QueryContext），调用透传即可。
func (r *usageRiskRepository) CleanupReportAndRollupTxDB(ctx context.Context, tx *sql.Tx, reportCutoff, rollupCutoff time.Time) (int64, int64, error) {
	return r.CleanupReportAndRollupTx(ctx, tx, reportCutoff, rollupCutoff)
}
