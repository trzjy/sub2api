# 派发单 Vision-R4：检测服务三整改——精确匹配/结构化日志/usage 记录（终审 P2-4/6/7）

## 背景
方案 `docs/capability-routing-plan.md` §3.5/§3.6/§3.8 与 §6。终审三条：P2-4 子串匹配偏离
权威判定"精确匹配"；P2-6 四态结果无 `vision_detect_result` 服务端日志；P2-7 检测直连上游
绕过 usage 记录链，平台侧检测流量统计缺失（§6 明文，执行遗漏）。

## 改动范围
- `backend/internal/service/vision_detect_service.go` + `vision_detect_service_test.go`：
1. **P2-4 精确匹配**：supported 判定改为归一化后完整匹配——剔除空白/标点后，响应中的数字
   内容须恰为验证码本身（含解释文本但数字恰等→supported；前后附加数字/多数字→非 supported
   走 unsupported；完全无码→unsupported；策略拒答→manual_review 既有语义不变）。
   测试：①"验证码是 681170"类带解释文本 → supported；②"681170123"/"0681170"前后附加数字
   → 非 supported；③无数字/错数字 → unsupported。
2. **P2-6 结构化日志**：四态分类点与落库失败路径统一 `slog` 写 `vision_detect_result`，
   字段：account_id、upstream_model、protocol、result、耗时；**不得记录 key、验证码真值、
   响应正文**。
3. **P2-7 usage 记录**：检测调用完成（成功/HTTP 失败/超时三类都要记）写一条平台侧 usage_logs：
   `user_id=0`、`api_key_id=0`（schema 两者均为必填 int64，0=平台哨兵，可按其精确筛出检测
   流量）、account_id/model/upstream_model 记实值、tokens 取上游响应 usage（取不到则 0）、
   成本 0（不进任何用户账单/订阅计量）。通过既有 usage log 写入边界落库，不自建平 行表；
   若既有边界无法无损表达该记录，BLOCKED 顶回并附证据，不得自造新机制。

## 禁区
- 只动上述两个文件。不得动 scheduler、setting_*、vision_routing_service、repo（其他整改单在改）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。
- 不新增降级/兜底：usage 记录失败不得吞掉检测结果的返回，错误如实上抛为检测流程错误。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'VisionDetect' -count=1`

## Done when
- 上述全部测试场景实跑通过并附输出（匹配三态 + 日志断言 + 三类 usage 记录）。
- `git status --short` 前后快照证明只改了派发单列出的文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
