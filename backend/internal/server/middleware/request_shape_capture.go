package middleware

import (
	"bytes"
	"io"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// RequestShapeCapture 在网关入口给每个 JSON 请求留一份「形状」摘要
// （只有计数与开关：消息条数、工具数、图片数、max_tokens、字节数……绝不含正文、
// 工具名、文件名），供上游泛化 400 的诊断使用（2026-11-04 用户裁定：加日志）。
//
// 为什么放在中间件而不是各个转发函数里：上游泛化 400（`inference request is invalid`
// 这类）会出现在/消息、对话、Responses、以及"消息转对话再兜底"等多条路径上，
// 逐个打点必然漏（2026-11-04 首版只打了 4 个入口，上线后第一条真实错误就没有形状）。
// 入口统一留一份，任何转发路径（含以后新增的）都能取到。
//
// 安全边界：只处理 JSON 请求（音视频/文件上传等二进制体直接跳过，避免整包进内存）；
// 形状来自 service.DescribeRequestShape，该函数只提取计数与开关，不取值。
func RequestShapeCapture() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.Body == nil || !strings.Contains(c.ContentType(), "json") {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		_ = c.Request.Body.Close()
		if err != nil {
			// 读不动就把原体换成已读到的部分，交给后续 bodyLimit/处理器按原逻辑报错。
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Next()
			return
		}
		// 原样回填，后续处理器的读取行为与未经过本中间件时完全一致。
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		if len(body) > 0 {
			service.SetOpsRequestShape(c, service.DescribeRequestShape(body, service.RequestShapeProtocolAuto))
		}
		c.Next()
	}
}
