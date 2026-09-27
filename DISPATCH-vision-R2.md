# 派发单 Vision-R2：能力读取错误失败关闭（终审 P1-2）

## 背景
方案 §3.4：unknown 放行的前提是"确认无记录"。终审 P1-2：`account_model_capability_service.go`
约 158-163 行，缓存刷新/DB 读取失败时记日志返回 nil，调用方误判为"无记录"放行——源不可读
被静默降级为可路由，违反合同 §5 禁兜底。修法：读取错误显式传播，调度侧失败关闭。

## 改动范围
- `backend/internal/service/account_model_capability_service.go`：能力查询签名扩展为可返回
  读取错误；"确认无记录"（unknown）与"读取失败"两种结果显式区分。
- `backend/internal/service/openai_account_scheduler.go`：`isAccountRequestCompatible` 的
  vision 判定调用点（约 1919-1940 行）——读取失败 → 返回 false + 新 filter reason
  `vision_capability_unavailable`（本次选号排除该候选；全部候选不可用则走既有无可用账号错误）。
  RequireVision=false 或 kill-switch 关闭时不读能力表，行为零改动。
- 测试：`openai_account_scheduler_vision_routing_test.go`（或同包新测试文件）补场景：
  ①能力源读取失败 + RequireVision=true → 选号失败关闭（不静默放行）；
  ②确认无记录（unknown）→ 仍放行（既有语义不回归）；③读取失败 + RequireVision=false → 无影响。

## 禁区
- 只动上述两个生产文件与 vision 相关测试文件。不得动 setting_*、vision_detect_service、
  vision_routing_service（其他整改单在改，避免冲突）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。
- 不加任何降级/兜底路径（合同 §5）；读失败唯一语义就是失败关闭。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'VisionRouting|VisionCapability|AccountModelCapability' -count=1`

## Done when
- 三个测试场景实跑通过并附输出；既有 9 个 VisionRouting 场景不回归。
- `git status --short` 前后快照证明只改了派发单列出的文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
