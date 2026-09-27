# 派发单 Vision-R3：kill-switch 控制面写入入口（终审 P1-3）

## 背景
方案 §3.8：全局 kill-switch `vision_routing_enabled` 定位是回滚手段。终审 P1-3：该 key 只有
初始化/解析/缓存失效处理（`setting_gateway_runtime.go`/`setting_parse.go`），`setting_update.go`
约 813-818 行明确其不在 `UpdateSettingsRequest`——运维无接口可关，回滚不可达。修法：纳入设置
写入 SSOT。

## 改动范围
- `backend/internal/service/setting_update.go`：`vision_routing_enabled` 纳入设置更新链路；
  **请求未携带该字段时不得覆盖存储值**（指针/nillable 语义，与既有可选设置项同构）。
- `backend/internal/service/settings_view.go`（如视图未暴露当前值则补上；已暴露则不动）。
- 若 admin 设置 handler/DTO 需要透传字段，动对应最小集合（`internal/handler/admin` 设置更新
  handler 与 DTO），除此之外不碰任何文件。
- 测试：新建独立测试文件（如 `setting_vision_routing_update_test.go`）：
  ①携带该字段更新 → 存储值变更；②未携带 → 存储值保持不变；③非法值被既有校验路径拒绝。

## 禁区
- 不得动 `openai_account_scheduler.go`、`account_model_capability_*`、`vision_detect_service.go`、
  `vision_routing_service.go`、frontend/（其他整改单在改，避免冲突）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/ ./internal/handler/`
- `go test ./internal/service/ -run 'VisionRouting|Setting' -count=1`
- `go test ./internal/handler/ -run 'Setting' -count=1`（仅当动了 handler）

## Done when
- 三个测试场景实跑通过并附输出。
- `git status --short` 前后快照证明只改了派发单列出的文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
