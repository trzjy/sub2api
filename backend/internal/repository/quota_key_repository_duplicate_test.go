//go:build unit

package repository

import (
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/lib/pq"
)

// TestQuotaKeyCreateDuplicatePredicate 覆盖 KD21-03 的判据：
// Create 撞唯一索引时，不论是底层 pq 23505 还是 ent 包装的 ConstraintError，
// 都应被同一表达式识别为"激活码重复"。
//
// 不连库、纯单元：直接对生产代码使用的判据
//
//	isUniqueViolation(err) || ent.IsConstraintError(err)
//
// 做表驱动断言。ent.ConstraintError 的字段未导出，无法从外部包构造带 wrap 的实例；
// 但 ent.IsConstraintError 内部走 errors.As 仅按类型匹配，故零值 &ent.ConstraintError{}
// 已足以命中该分支。
func TestQuotaKeyCreateDuplicatePredicate(t *testing.T) {
	constraintPredicate := func(err error) bool {
		return isUniqueViolation(err) || ent.IsConstraintError(err)
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "pq unique violation 23505",
			err:  &pq.Error{Code: "23505"},
			want: true,
		},
		{
			name: "ent ConstraintError (zero value, type-only match)",
			err:  &ent.ConstraintError{},
			want: true,
		},
		{
			name: "generic error should not match",
			err:  errors.New("some unrelated error"),
			want: false,
		},
		{
			name: "pq other code should not match",
			err:  &pq.Error{Code: "23503"},
			want: false,
		},
		{
			name: "nil error should not match",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := constraintPredicate(tc.err)
			if got != tc.want {
				t.Fatalf("constraintPredicate(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
