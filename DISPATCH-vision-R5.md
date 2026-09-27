# 派发单 Vision-R5：分流配置写入校验收紧 + 前端协议选项收敛（终审 P2-5/8）

## 背景
方案 §3.6："规则命中但目标账号全部不可用 → 无可用账号错误，不静默回落"。终审 P2-5：空目标
列表规则被 `lookupVisionRoutingModel`（`vision_routing_service.go` 约 95-107 行）当未命中处理，
调度静默回落普通候选池——修法：写入侧拒绝空规则，使持久化配置恒为有效目标集合（新功能未上
生产，无存量数据兼容问题）。终审 P2-8：检测服务只实现 `chat_completions`（方案 §6 实现边界），
前端协议选择暴露 responses/anthropic 属越界入口。

## 改动范围
- `backend/internal/service/vision_routing_service.go`：写入校验拒绝——模型模式为空/空白、
  目标账号列表为空；校验错误信息明确。
- `backend/internal/handler/admin/group_handler.go`（及 DTO 若需）：分组 create/update 携带
  vision_routing 时走同一校验，非法配置 4xx 拒绝；既有跨组拒绝校验（CrossGroupRejected）不动。
- 测试：`vision_routing_service` 既有测试文件补场景：①空模型拒绝；②空目标列表拒绝；
  ③合法配置不受影响。
- **前端**：`frontend/src/components/admin/account/VisionCapabilityModal.vue`（及
  `AccountsView.vue`/`api/admin/accounts.ts`/i18n 若涉及）：协议选择只保留
  `chat_completions`，移除 responses/anthropic 选项与相关 i18n 文案；不改其他前端逻辑。

## 禁区
- 不得动 `openai_account_scheduler.go`、`account_model_capability_*`、`vision_detect_service.go`、
  `setting_*`（其他整改单在改，避免冲突）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/ ./internal/handler/`
- `go test ./internal/service/ -run 'VisionRouting' -count=1`
- `go test ./internal/handler/ -run 'VisionCapability|Group' -count=1`
- `cd frontend && npx vue-tsc --noEmit`

## Done when
- 三个后端测试场景实跑通过并附输出；vue-tsc 零错误。
- `git status --short` 前后快照证明只改了派发单列出的文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
