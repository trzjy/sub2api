# 派发单 Vision-F2（v2）：调度侧候选池限定（方案 §3.7 缺失的调度消费）

> v2 修订（2026-09-27 主会话裁决）：v1 的「同构既有 ModelRouting 在调度内的数据获取
> 方式」前提**作废**——执行会话已实证 OpenAI 调度路径不消费 ModelRouting，且
> `Group.VisionRouting` 在调度读路径无水合点（ent 无列、`groupEntityToService`
> 不填充），直调 `Group.GetVisionRoutingAccountIDs` 恒返回 nil 死代码。
> v2 数据面改为：**复用既有 `VisionRoutingService`**（`vision_routing_service.go`，
> 直读 repo 无缓存层，即时生效满足 §3.7，非新造缓存机制）。不动 ent schema、
> 不动 `groupEntityToService`（`vision_routing_repo` 刻意避开 ent codegen 是既定
> 设计，补列会制造同一数据的第二条读路径）。

## 背景
方案：`docs/capability-routing-plan.md` v3.1 §3.7（已过闸①）。当前调度器只做了
§3.4 的"排除已知不支持视觉的账号"（`openai_account_scheduler.go` 内
`req.RequireVision` 分支的 `vision_not_supported` 过滤），缺 §3.7 的正向收窄：
带图请求命中 `groups.vision_routing` 配置时，候选池必须限定为目标账号。

既有可用件（只消费）：
- `VisionRoutingService.GetVisionRoutingAccountIDs(ctx, groupID, requestedModel)
  ([]int64, error)` — `vision_routing_service.go:97`，含精确+通配匹配；未命中/未配置
  返回空。
- `groups.vision_routing` 数据已落库（迁移 260，原生 SQL repo）。
- 调度器已有 `RequireVision` 参数链（`openai_account_scheduler.go:91/2233/2350`），
  候选过滤处可触达网关服务（`s.service`）。

## 改动范围
1. `backend/internal/service/openai_gateway_service.go`：
   - `OpenAIGatewayService` 结构体新增 `visionRouting *VisionRoutingService` 字段；
   - `NewOpenAIGatewayService`（:504，现有 22 参）**末尾追加**该参数并赋值。
2. 全部既有调用点补末位参数（机械改）：`internal/service/wire.go:58` +
   `cmd/server/wire_gen.go`（用 `~/go/bin/wire` 重新生成）+ 13 个测试文件中的
   构造调用（`grep -rn "NewOpenAIGatewayService(" --include="*.go"` 共 23 处，
   多数传 `nil` 即可；**只改构造调用行，不碰测试其他逻辑**）。
3. `backend/internal/service/openai_account_scheduler.go`：
   - `selectAccountWithSchedulerOnce` 入口处：`requireVision==true && groupID!=nil`
     时调用 `visionRouting.GetVisionRoutingAccountIDs(ctx, groupID, requestedModel)`
     **每次选号请求最多读一次**；结果放入 `SelectionRequest` 新字段
     `VisionRoutingTargets []int64`（两处构造点 :2389/:2499 都要带上）；
   - **读取错误 → 失败关闭**：返回明确错误终止选号，禁止静默当作未配置
     （合同 §5 禁兜底；带图请求在路由不可知时发往任意账号会复现静默丢图）；
   - 候选过滤处（`req.RequireVision` 既有分支旁）：`VisionRoutingTargets` 非空 →
     候选账号限定为该 ID 集合（不在集合 → 排除，filter reason 按仓内既有命名
     风格，如 `vision_routing_excluded`）；**限定后仍执行既有 `vision_not_supported`
     能力过滤，绝不豁免**（方案明文"规则只收窄候选池"）；
   - 限定后无可用账号 → 返回无可用账号错误（不静默回落 unknown 放行）；
   - 命中时打结构化日志 `vision_routed`（含 model、目标账号数，参照方案 §3.8
     与仓内既有 filter stats/结构化日志惯例）；
   - `requireVision==false` 或 groupID==nil：零读取、零行为变化。
4. 新增测试文件 `backend/internal/service/openai_account_scheduler_vision_routing_test.go`：
   覆盖 命中限定／目标池内仍被能力过滤排除／池空返回错误／未配置命中回落
   既有过滤／RequireVision=false 零影响／**读取错误失败关闭** 六个场景
   （fake VisionRoutingRepository 即可，测试替身只存在于测试代码）。

## 禁区
- 不得改 ent schema / `ent/` 生成物 / `groupEntityToService` / `api_key_repo.go`。
- 不得改 `vision_routing_service.go`、`vision_routing_repo.go`、`group.go`、handler、前端。
- 既有测试文件只允许改 `NewOpenAIGatewayService(...)` 构造调用行（补参），
  不得动其他测试逻辑。
- 禁止兜底/降级分支（合同 §5）；边界外发现 BLOCKED 顶回。

## 验证命令白名单（只允许跑以下命令）
- `cd backend && go build ./...`
- `cd backend && go test ./internal/service/ -run 'VisionRouting' -count=1`
- `cd backend && go vet ./internal/handler/ ./internal/server/middleware/ ./internal/server/routes/`
  （构造调用点所在包的测试编译核验）

## Done when
- 上述 4 点全部落地，`go build ./...` 与三条白名单命令通过。
- 新测试文件覆盖 6 场景并 PASS。
- `git status --short` 快照改动仅限：`openai_gateway_service.go`、
  `openai_account_scheduler.go`、`internal/service/wire.go`、`cmd/server/wire_gen.go`、
  各测试文件的构造调用行、新测试文件。

## 模型
- 执行模型：custom-local:deepseek-v4.1-flash（hy3 本轮挂起，主会话已切）。
