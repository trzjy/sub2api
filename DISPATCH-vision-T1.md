# 派发单 Vision-T1：检测服务三整改——代理关系缺失/畸形 200/日志全闭合（第 3 次终审 P1-A + P2-C + P2-D）

## 背景
终审第 3 轮三条，均在 `vision_detect_service.go`。方案 §3.5（四态分类权威：
传输/协议故障不得落库负能力）、§3.8（观测契约）。

## 改动范围
- `backend/internal/service/vision_detect_service.go` + `vision_detect_service_test.go`：
1. **P1-A（安全）**：约 167-169 行，`ProxyID != nil` 但 `account.Proxy` 关系缺失时，
   当前落进无代理直连。改为：`ProxyID != nil && Proxy == nil` → 构造 HTTP client 前
   终止，返回 `detect_failed`（消息说明代理配置关系缺失），写日志，不记 usage
   （请求未发出边界不变）。仅"明确无代理配置"（ProxyID 为空）才允许直连。
2. **P2-C**：约 240 行。HTTP 200 响应先做结构校验：非有效 JSON / 显式 error payload /
   缺失有效 choices·message → 判 `detect_failed`（协议/上游故障），**保留 usage 与
   日志**，不写能力标记；仅结构有效的普通文本无验证码响应才落 `unsupported`。
3. **P2-D**：所有四态终态返回路径统一经 `logDetectResult`——补齐缺 key/缺 base URL/
   生成验证码/渲染图片/序列化请求/usage 写入失败等早期终态的日志；继续排除 key、
   验证码真值、响应正文。
- 测试：①ProxyID 非空 + Proxy nil → detect_failed 且 doer 未被调用；②200+畸形 JSON /
  200+error payload / 200 缺 message → detect_failed 且不写能力标记、usage+日志已记；
  ③结构有效但无码 → 仍 unsupported（零回归）；④缺 key 终态有日志。

## 禁区
- 只动上述两文件。不得动 `account_model_capability_service.go`（T2 在改）、
  scheduler、routing、wire。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'VisionDetect' -count=1`

## Done when
- 四个新场景实跑通过并附输出；既有 VisionDetect 测试零回归。
- `git status --short` 前后快照证明只改派发单列出文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
