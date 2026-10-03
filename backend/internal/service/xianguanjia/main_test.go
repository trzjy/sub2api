package xianguanjia

import (
	"database/sql/driver"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// TestMain 为本包 sqlite 单测注册 now() 标量函数：supply_config.go 的 SQL 使用
// PostgreSQL 的 now()，而 modernc sqlite 默认无此函数。仅测试进程注册，不影响
// 生产（生产为 PostgreSQL，now() 原生可用）。
func TestMain(m *testing.M) {
	_ = sqlite.RegisterScalarFunction("now", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		return time.Now(), nil
	})
	m.Run()
}
