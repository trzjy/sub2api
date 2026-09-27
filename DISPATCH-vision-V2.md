# 派发单 Vision-V2：分组视觉配置读写闭环 + 批量检测契约归一（第 5 次终审 3 条）

## 背景

方案 SSOT `docs/capability-routing-plan.md` v3.1（已过闸①）。第 5 次终审
（2026-09-28，输出 /home/zjy/.codex-companion/2026-09-27T17-41-02-141Z-review，
裁定留痕 REVIEW-PACKET-vision-routing-final.md §15）block，其中 3 条归本单：

- **P1 vision_routing 无法显式清空**：`applyVisionRouting`
  （group_handler.go:557-568）对 `len(routing)==0` 直接 return nil（不写空配置），
  编辑分组删空全部规则提交后旧规则残留并继续被调度读取。前端
  `convertRoutingRulesToApiFormat`（GroupsView.vue:5622）空规则产出 `null`、
  非空产出 map——null 与"未携带"不可区分，且当前请求体总是携带该字段。
- **P1 校验与分组保存非原子**：`applyVisionRouting` 在 group Create
  （:764）/Update（:916）成功之后执行；校验失败（跨组/非法目标）返回 4xx 但
  分组及字段已落库 ⇒ 部分提交。
- **P1 分组读取链未装载 VisionRouting**：`groups.vision_routing` 列（迁移 260）
  仅由 visionRoutingRepo（写路径 + 调度读路径 scheduler:2570）访问；
  GroupService.GetByID/List/ListActive → ent 读取链不装载该字段，
  `GroupFromServiceAdmin`（mappers.go:159）透传恒 nil ⇒ 管理员编辑页
  （GroupsView.vue:6526 读 `group.vision_routing`）看不到已保存规则。
- **P1 批量检测前后端契约不一致**：后端 `visionBatchRequest`
  （vision_capability_handler.go:57-62）要求 `{"account_ids":[..],"items":[{model,protocol}]}`
  （两份派发单 F3/E 原文契约，SSOT 口径）；前端 `detectVisionCapabilityBatch`
  （accounts.ts:1226-1233）偏离为 `{items:[{account_id,...}]}` ⇒ 400 或零结果。
  现前端无调用方（全前端仅单检测/覆盖两处调用），但契约必须归一到后端形状。

## 改动范围（仅允许改以下 6 个文件）

1. `backend/internal/handler/admin/group_handler.go`
2. `backend/internal/service/group_service.go`（如读取链水合需要）
3. `backend/internal/service/vision_routing_service.go`（如需加读方法；不得动 repo）
4. `frontend/src/api/admin/accounts.ts`
5. `frontend/src/views/admin/GroupsView.vue`
6. 测试文件：分组 handler/service 现有测试 + 如需新建
   `backend/internal/handler/admin/group_vision_routing_lifecycle_test.go`

除此之外一律禁改（尤其禁改 vision_capability_handler.go、vision_routing_repo.go、
ent/ 生成文件、迁移文件、wire*.go）。

### 改动点 A：显式空配置清空（P1-清空）

- 请求 DTO 语义拆分：`VisionRouting *map[string][]int64`（指针）或等价机制区分
  「字段未携带」（nil → 不触碰既有配置）与「字段携带」（空 map `{}` / null → 清空）。
  Create 场景 nil 与空等价（新组无既有配置）；Update 场景必须区分。
- `applyVisionRouting` 改为：nil → 跳过；非 nil（含空 map）→ 走
  VisionRoutingService.Set（空 map 写入 `{}` 即清空，repo.Set 已支持
  "routing 为 nil 时写入空对象"，语义核对后按实际签名对齐）。
- 前端 `convertRoutingRulesToApiFormat`：空规则时返回 `{}`（显式空）而非 `null`，
  使"删空规则提交"真实生效；创建/编辑两处调用点（:6299、:6652）随之对齐。
- 注意 ModelRouting 同形函数不要顺手改（它有既有语义与消费者，不在本单范围）。

### 改动点 B：校验前置，消除部分提交（P1-原子）

- 目标语义：vision_routing 校验失败 ⇒ 分组 Create/Update 整体失败，零落库。
- 实现路径择优（按净增复杂度最小）：
  1. 在调用 groupService.Create/Update **之前**先做校验（需知道 groupID 的场景——
     Update 已有 id 可先校验；Create 尚无 id，若 VisionRoutingService 校验需要
     groupID 上下文，则评估把同组校验改为「创建后校验失败即回滚补偿」——此路径
     引入补偿机制，复杂度高，除非无法前置否则不选）；
  2. 或将 vision_routing 写入挪进 groupService 层与分组写同一事务（如 GroupService
     已有事务边界则挂入；没有则评估引入成本）。
- 优先方案 1 的 Update 侧前置 + Create 侧评估；如 Create 侧无法在不引入补偿/
  新事务机制的前提下闭合，BLOCKED 顶回列出证据与两条路径的复杂度对比，由主会话
  裁定，不得自行引入补偿删除逻辑。

### 改动点 C：分组读取链装载 VisionRouting（P1-水合）

- 让管理员读取路径（GroupService.GetByID / List / ListActive 中实际喂给
  `GroupFromServiceAdmin` 的那一条或几条——先取证 GetAll/Get/编辑页用的具体方法）
  在返回前把 `vision_routing` 从 VisionRoutingService（或其 repo 接口）装载进
  `Group.VisionRouting`。
- 装载位置择优：service 层批量读取（一次查询 N 组配置再按 groupID 分发）优于
  逐组 N+1 查询；如只涉单组 GetByID 则单点装载即可。列表路径必须避免 N+1
  （可给 VisionRoutingService 加 `GetByGroupIDs(ctx, ids)` 批量方法 + repo
  对应查询——repo 文件禁改！若 repo 需要新查询，BLOCKED 顶回，主会话再决定
  是否扩授权）。
- 调度读路径（scheduler:2570 直读 VisionRoutingService）不动——它是独立权威读，
  与水合互不影响（两条读路径读同一列，非平行实现）。

### 改动点 D：前端批量契约归一（P1-契约）

- `detectVisionCapabilityBatch`（accounts.ts:1226-1233）请求体改为后端形状：
  `{account_ids: [...], items: [{model, protocol}]}`——account_ids 从 payload
  提取去重，items 按 (model, protocol) 去重。
- 类型 `VisionCapabilityBatchItem`（:1189）与请求参数形状同步调整；响应类型不动
  （后端响应 results 形状未变）。
- 如类型/注释引用旧形状一并更新。

## 禁区

- 禁改：vision_capability_handler.go、vision_routing_repo.go、ent/ 生成文件、
  迁移、wire*.go、openai_* 调度/handler、能力缓存协议。
- 禁止 `git checkout --` / `git restore`。
- 禁止 N+1 查询入列表路径；禁止补偿删除逻辑（除非主会话裁定后授权）；
  禁止第二套校验实现（同组校验唯一入口仍是 VisionRoutingService.Set 内部校验）。
- 超范围发现一律 BLOCKED 顶回。
- 开工前后 `git status --short` 快照自证。

## 验证命令白名单

- backend 下：`go build ./internal/handler/... ./internal/service/`；
  `go vet ./internal/handler/... ./internal/service/`；
  `go test ./internal/handler/admin/ -run 'VisionRouting|Group' -count=1`；
  `go test ./internal/service/ -run 'VisionRouting|Group' -count=1`。
- frontend/ 下：`npx vue-tsc --noEmit`。

（全量测试由主会话收敛阶段统一执行，你不得跑 `go test ./...`。）

## Done when

1. 白名单命令全过，输出贴回。
2. A：Update 携带空 map ⇒ 配置清空（DB 里 `{}`）；未携带 ⇒ 不动；Create 空 ⇒ 无配置。
3. B：Update 侧校验失败 ⇒ 分组零变更；Create 侧按主会话裁定路径闭合或 BLOCKED。
4. C：GetByID/列表路径返回的 group.VisionRouting 与 DB 一致（含空 `{}` → 空 map）。
5. D：批量请求体为 `{account_ids, items}`，account_ids 去重、items 去重；
   vue-tsc 过。
6. 测试覆盖以上 4 场景 + 契约测试；`git status --short` 前后快照零越界。
7. 超范围问题 BLOCKED 顶回。

## 模型

- 主力 hy3；额度耗尽切 `custom-local:deepseek-v4.1-flash`（必须带 `custom-local:`
  前缀），切换不需请示。
