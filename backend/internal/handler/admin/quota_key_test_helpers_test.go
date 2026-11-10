//go:build unit

package admin

import "strconv"

// itoa 是 admin 测试包的共享整型转字符串助手（原定义于已删除的
// quota_key_auto_entry_handler_test.go，被本包其它测试文件复用，故单列于此保留）。
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
