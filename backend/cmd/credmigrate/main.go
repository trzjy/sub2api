// credmigrate 执行凭证存量迁移（docs/security-ban-prevention-plan.md A3-E3）：
// 把 accounts.credentials 中明文形态的敏感子键批量加密为 enc:v1: 密文，
// 并为全部账号回填 credentials_mac / credentials_api_key_mac 指纹列。
//
// 用法（需先在环境配置 CRED_ENCRYPTION_KEY，与网关进程一致）：
//
//	go run ./cmd/credmigrate [-dry-run] [-batch 500]
//
// 行为要点：
//   - 幂等：已加密且指纹一致的行自动跳过，可安全重跑；
//   - 未配置 CRED_ENCRYPTION_KEY 时拒绝执行（存量迁移必须与网关使用同一密钥）；
//   - 迁移期间网关可继续运行（写路径自 E2 起产出相同形态）；建议在低峰执行，
//     并在完成后按方案验证业务再考虑移除 CRED_ENCRYPTION_KEY_OLD。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	_ "github.com/lib/pq"
)

func main() {
	batchSize := flag.Int("batch", 500, "每批读取的账号行数")
	dryRun := flag.Bool("dry-run", false, "只扫描统计待迁移行，不写库")
	includeDeleted := flag.Bool("include-deleted", false, "一并处理软删除账号（其明文凭证在 DB 泄漏场景同样可达，建议开启）")
	timeout := flag.Duration("timeout", 30*time.Minute, "整体执行超时")
	flag.Parse()

	// config.Load 会读取环境变量并安装 credcrypt 进程级密钥（与网关一致）。
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：加载配置失败：%v\n", err)
		os.Exit(1)
	}
	if !credcrypt.Enabled() {
		fmt.Fprintf(os.Stderr, "错误：未配置 %s，存量迁移必须在目标密钥就绪后执行（与网关进程使用同一密钥）。\n", credcrypt.EnvKey)
		os.Exit(1)
	}

	db, err := sql.Open("postgres", cfg.Database.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：连接数据库失败：%v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "错误：数据库不可达：%v\n", err)
		os.Exit(1)
	}

	var stats *repository.CredentialMigrationStats
	if *dryRun {
		fmt.Println("dry-run 模式：仅扫描统计，不执行写入。")
		stats, err = repository.CountLegacyCredentials(ctx, db, *includeDeleted)
	} else {
		stats, err = repository.MigrateLegacyCredentials(ctx, db, *batchSize, *includeDeleted, func(stats *repository.CredentialMigrationStats) {
			fmt.Printf("... 已扫描 %d 行（加密 %d，回填指纹 %d，无变化 %d）\n",
				stats.Scanned, stats.EncryptedRows, stats.MACBackfillRows, stats.Unchanged)
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("扫描 %d 行：待加密 %d 行，仅回填指纹 %d 行，无变化 %d 行。\n",
		stats.Scanned, stats.EncryptedRows, stats.MACBackfillRows, stats.Unchanged)
	fmt.Println("提示：迁移完成后建议抽验业务（账号 CRUD / 刷新 / 网关转发），确认无误后再考虑移除 CRED_ENCRYPTION_KEY_OLD。")
}
