//go:build unit

package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
)

// TestQuotaKeyCreateDuplicatePredicate 覆盖 KD21-05 的收窄判据：
// Create 撞唯一索引时，仅当底层 pq 错误为 23505 且约束名恰为 uq_quota_keys_code_mac
// 才被识别为「激活码重复」。主键冲突（同为 23505，约束名 quota_keys_pkey）与
// status CHECK 冲突（23514）都不得被误译。
//
// 不连库、纯单元：直接对生产函数 isQuotaKeyCodeMacUniqueViolation 做表驱动断言。
func TestQuotaKeyCreateDuplicatePredicate(t *testing.T) {
	pkUniqueErr := &pq.Error{Code: "23505", Constraint: "quota_keys_pkey"}
	statusCheckErr := &pq.Error{Code: "23514", Constraint: "chk_quota_keys_status"}
	codeMacWrapped := fmt.Errorf("wrapped: %w", &pq.Error{Code: "23505", Constraint: "uq_quota_keys_code_mac"})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "唯一索引冲突（正例）",
			err:  &pq.Error{Code: "23505", Constraint: "uq_quota_keys_code_mac"},
			want: true,
		},
		{
			name: "主键冲突（同为 23505，约束名不同）",
			err:  pkUniqueErr,
			want: false,
		},
		{
			name: "status CHECK 冲突（23514）",
			err:  statusCheckErr,
			want: false,
		},
		{
			name: "无约束名的 23505",
			err:  &pq.Error{Code: "23505"},
			want: false,
		},
		{
			name: "普通错误",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "被 fmt 包装的 pq 唯一冲突（errors.As 穿透）",
			err:  codeMacWrapped,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isQuotaKeyCodeMacUniqueViolation(tc.err)
			if got != tc.want {
				t.Fatalf("isQuotaKeyCodeMacUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
