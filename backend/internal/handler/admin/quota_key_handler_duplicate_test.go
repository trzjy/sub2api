//go:build unit

package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestRespondQuotaKeyErrorDuplicateCode 覆盖 KD21-04：仓储层 code_mac 唯一冲突
// 哨兵 ErrQuotaKeyDuplicateCode 必须映射为 409 + 明确中文文案（reason=
// QUOTA_KEY_DUPLICATE_CODE），而不是既有的 500 internal error。
func TestRespondQuotaKeyErrorDuplicateCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	respondQuotaKeyError(c, service.ErrQuotaKeyDuplicateCode)

	require.Equal(t, http.StatusConflict, rec.Code, "重复激活码必须返回 409")
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_DUPLICATE_CODE", "响应须带 reason QUOTA_KEY_DUPLICATE_CODE")
	require.Contains(t, rec.Body.String(), "该激活码已存在", "响应文案须提示用户该激活码已存在")
}

// TestRespondQuotaKeyErrorGenericKeeps500 回归：传入一个普通 error 时保持既有
// 默认行为（500），证明新增的 409 分支没有误伤其它错误。
func TestRespondQuotaKeyErrorGenericKeeps500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	respondQuotaKeyError(c, errors.New("boom"))

	require.Equal(t, http.StatusInternalServerError, rec.Code, "普通 error 仍须返回 500，未被 409 分支误伤")
}
