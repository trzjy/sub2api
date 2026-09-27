# 派发单 Vision-S1：能力查询接入生产调度器（第 2 次终审 P1-A）

## 背景
终审第 2 轮 P1 阻断：`openAIGatewayService` 的 `openAIAccountVisionCapabilityLookup` 默认实现
恒返回 `(false, false, nil)`（unknown→放行），生产 wiring 从未调用
`SetOpenAIAccountVisionCapabilityLookup` 绑定到 `AccountModelCapabilityService`——
§3.4 的 `vision_not_supported` 排除与失败关闭在生产不生效，能力表为假实现。
方案：`docs/capability-routing-plan.md` §3.4；消化记录 REVIEW-PACKET §8 后续。

## 改动范围
- 找到 `ProvideOpenAIGatewayService`（`backend/internal/service/openai_gateway_service.go`
  或其所在文件）：签名增加 `capability *AccountModelCapabilityService` 参数（放最后），
  构造后调用 `SetOpenAIAccountVisionCapabilityLookup` 绑定到
  `capability.ModelSupportsVisionInput`（注意三返回值签名：`(supported, known, err)`）。
- `backend/cmd/server/wire_gen.go`：`ProvideOpenAIGatewayService(...)` 调用点补传
  `accountModelCapabilityService`（该变量已存在，见 :333 附近）。
- **接线测试**：新增测试证明——经 provider 构造的服务，注入 known=false 能力后带图请求
  候选被 `vision_not_supported` 排除；能力读取错误返回 `vision_capability_unavailable`。
  若 ProviderSet 中 ProvideOpenAIGatewayService 需要在 `service/wire.go` 调整顺序/注册，
  同步最小修改。

## 禁区
- 不得动 `vision_detect_service.go`、`vision_routing_service.go`、
  `account_model_capability_service.go`（其他整改单在改）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/ ./cmd/server/`
- `go test ./internal/service/ -run 'VisionCapability|VisionRouting|RequireVision' -count=1`

## Done when
- 接线测试实跑通过并附输出（known=false 被排除 + 读错失败关闭两断言）。
- 既有 VisionRouting/VisionCapability 测试零回归。
- `git status --short` 前后快照证明只改派发单列出文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
