# 派发单 Vision-S2：检测服务两整改——代理失败即停 / 体读失败补观测（终审 P1-B + P2-D）

## 背景
终审第 2 轮两条，均在 `vision_detect_service.go`（与同文件发现合并一单）：
- **P1-B（安全）**：约 268-271 行，配置代理的账号 `parseProxyURL` 失败时静默保留
  `transport.Proxy == nil`，随后携账号真实 key 直连上游——绕过出口策略+密钥泄漏面。
  合同 §5 禁兜底。
- **P2-D**：约 183-192 行，请求已发出但 `io.ReadAll` 失败时直接返回 `VisionDetectFailed`，
  usage 与 `vision_detect_result` 日志双缺——真实上游流量不可观测，违反方案 §3.8/§6。

## 改动范围
- `backend/internal/service/vision_detect_service.go` + `vision_detect_service_test.go`：
1. **P1-B**：代理解析失败 → 终止检测，返回明确错误（检测失败态），**绝不**退回无代理
   transport。此时请求未发出，按既有边界不记 usage（与早返回一致），但需写
   `vision_detect_result` 日志（result=detect_failed）。
2. **P2-D**：请求发出后的失败（含体读失败）收敛到统一失败终结路径：先记 usage 再记
   `vision_detect_result` 日志，最后返回 `detect_failed`。请求发出前校验失败不记 usage
   的既有边界保持不变。
- 测试：①代理配置+解析失败 → 返回失败且上游 HTTP 未被调用（断言 doer 未触达）、
  无 usage、有日志；②请求发出后体读失败 → usage 已记 + 日志已记 + 返回 detect_failed。

## 禁区
- 只动上述两文件。不得动 scheduler、`account_model_capability_service.go`、
  `vision_routing_service.go`、wire（其他整改单在改）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'VisionDetect' -count=1`

## Done when
- 两个新场景实跑通过并附输出；既有 VisionDetect 测试零回归。
- `git status --short` 前后快照证明只改派发单列出文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
