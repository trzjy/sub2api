//go:build unit

package middleware

// 文件：internal/server/middleware/cf524_access_log_test.go
//
// CF524 G5b：日志集成缝测试（派发单 G5b v2 可达范围裁定）。
//
// 本文件不构造 pending tracker：cf524GuardTracker 为 service 未导出类型，middleware
// 测试无法构造，且 v2 裁定禁止为测试导出生产符号。中件间集成缝的四点关联链由组合
// 论证证明：
//   ① attempt/终态事件同 request_id 已由 service 侧 14 用例覆盖
//      （cf524_observation_test.go，含 TestCF524ObservationResolvePendingAtRequestExit）；
//   ② 完成日志携带 request_id 已由 RequestLogger→Logger 中间件链路成立（router.go:59/63
//      实序：RequestLogger 于 request_logger.go:34-43 注入 request-scoped logger with
//      request_id，Logger 经 logger.FromContext 继承）；
//   ③ 本文件补测判别器字段真实落完成日志：upstream_headers_received_ms 与 request_id
//      同一条目落盘（用例 1）；
//   ④ logger.go 中判别器字段写入（85-87 行）与 ResolvePendingGuardObservations 调用行
//      （89 行）属同一直行代码块，正例（用例 1）执行即覆盖该调用行的执行——无 tracker
//      时其为 no-op 不 panic，其正向行为（补发终态关联事件）已由 service 侧
//      TestCF524ObservationResolvePendingAtRequestExit 覆盖，本文件不重复验证。
//
// 用例 3 以 cf524GuardTrackerKey 的字面量值（"cf524_guard_observation_tracker"，见
// service/cf524_observation.go:55，此处与 service 侧耦合）塞入非 tracker 类型值，验证
// ResolvePendingGuardObservations 经 cf524TrackerIfAny 类型断言失败返回 nil：请求正常
// 完成、零 CF524 事件、不 panic。
//
// 参考 request_access_logger_test.go 既有 zap 捕获基建（testLogSink /
// initMiddlewareTestLogger，同包共享）。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// cf524GuardTrackerKeyLiteral 与 service 未导出常量 cf524GuardTrackerKey
// （service/cf524_observation.go:55）同值；禁止为测试导出生产符号，故以字面量耦合。
const cf524GuardTrackerKeyLiteral = "cf524_guard_observation_tracker"

// findCompletedAccessLog 返回捕获到的唯一完成/访问日志条目。
func findCompletedAccessLog(t *testing.T, sink *testLogSink) *logger.LogEvent {
	t.Helper()
	for _, event := range sink.list() {
		if event != nil && event.Message == "http request completed" {
			return event
		}
	}
	t.Fatalf("access log event not found")
	return nil
}

// int64Field 兼容 zap MapObjectEncoder 各整数落盘形态取 int64 字段。
func int64Field(t *testing.T, fields map[string]any, key string) int64 {
	t.Helper()
	v, ok := fields[key]
	if !ok {
		t.Fatalf("field %q missing: %+v", key, fields)
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("field %q unexpected type %T: %v", key, v, v)
		return 0
	}
}

// 用例 1 判别器正例：handler 内经导出面 MarkUpstreamHeadersReceived 埋点后，
// 完成日志同一条目含 upstream_headers_received_ms（int64 ≥0）与 request_id。
func TestCF524AccessLog_DiscriminatorPositive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.Use(Logger())
	r.GET("/api/test", func(c *gin.Context) {
		service.MarkUpstreamHeadersReceived(c, time.Now())
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(requestIDHeader, "rid-g5b-positive")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	ev := findCompletedAccessLog(t, sink)
	ms := int64Field(t, ev.Fields, "upstream_headers_received_ms")
	if ms < 0 {
		t.Fatalf("upstream_headers_received_ms=%d, want >=0", ms)
	}
	if got := ev.Fields["request_id"]; got != "rid-g5b-positive" {
		t.Fatalf("request_id=%v, want rid-g5b-positive; discriminator must land on same entry as correlation key", got)
	}
}

// 用例 2 零变化负例：不 Mark → 完成日志不含 upstream_headers_received_ms 字段
// （header-wait 悬挂 / OpenAI 路径零变化分支）。
func TestCF524AccessLog_DiscriminatorAbsentWhenNotMarked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.Use(Logger())
	r.GET("/api/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	ev := findCompletedAccessLog(t, sink)
	if _, ok := ev.Fields["upstream_headers_received_ms"]; ok {
		t.Fatalf("unexpected discriminator field: %+v", ev.Fields)
	}
}

// 用例 3 tracker 异常鲁棒：以 cf524GuardTrackerKey 字面量值塞入非 tracker 类型值，
// ResolvePendingGuardObservations 经 IfAny 类型断言失败返回 nil：请求正常完成、
// 零 CF524 guard 事件、不 panic。
func TestCF524AccessLog_TrackerTypeMismatchRobust(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.Use(Logger())
	r.GET("/api/test", func(c *gin.Context) {
		// 与 service 未导出键 cf524GuardTrackerKey 同值；类型故意不符（string 非
		// *cf524GuardTracker），触发出口 ResolvePendingGuardObservations 的
		// cf524TrackerIfAny 类型断言失败路径。
		c.Set(cf524GuardTrackerKeyLiteral, "not-a-tracker")
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	// 请求正常完成：完成日志仍存在。
	findCompletedAccessLog(t, sink)

	// 零 CF524 guard 事件：类型断言失败 → tr == nil → 出口补发 no-op，不 panic。
	for _, e := range sink.list() {
		if e != nil && e.Message == "gateway_first_byte_guard_triggered" {
			t.Fatalf("unexpected guard event: %+v", e)
		}
	}
}
