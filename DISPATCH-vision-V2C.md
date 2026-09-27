# 派发单 Vision-V2C：分组读取链 VisionRouting 水合（V2 单 C 项续做，主会话已裁定扩授权）

## 背景

V2 单执行中 C 项规范 BLOCKED 顶回（报告 /tmp/vision-v2.log），主会话取证核实后裁定
**采纳路径 1 并扩授权**。本单是 C 项的续做单。

前置事实（已核实）：
- admin 分组读取链走 `adminServiceImpl`（`admin_group.go:28 ListGroups / 50 GetAllGroups /
  54 GetAllGroupsByPlatform / 58 GetAllGroupsIncludingInactive / 65 GetGroup`），
  直连 `s.groupRepo.*`，不经过 `GroupService.GetByID/List/ListActive`。
- `VisionRoutingRepository`（`vision_routing_repo.go`）现只有逐组 `Get(groupID)`。
- `adminServiceImpl`（`admin_service.go`）无 visionRouting 依赖。

## 改动范围（仅允许改以下 6 个文件）

1. `backend/internal/repository/vision_routing_repo.go` —— 新增批量读
2. `backend/internal/service/vision_routing_service.go` —— 暴露批量方法
3. `backend/internal/service/admin_group.go` —— 读取链水合
4. `backend/internal/service/admin_service.go` —— adminServiceImpl 注入依赖
5. `backend/internal/service/wire.go` —— wire 接线（如构造函数签名变化所需）
6. `backend/internal/service/vision_routing_service_test.go` 或新建
   `backend/internal/service/admin_group_vision_hydration_test.go` —— 测试

（V2 单已改的 group_handler.go / 前端文件本单禁碰。）

### 改动点 1：repo 批量读（扩授权）

`vision_routing_repo.go` 新增 `GetByGroupIDs(ctx, groupIDs []int64) (map[int64]map[string][]int64, error)`：
- 单条 SQL：`SELECT id, COALESCE(vision_routing,'{}'::jsonb)::text FROM groups WHERE id = ANY($1) AND deleted_at IS NULL`；
- 返回 map[groupID]routing；没查到的组不在 map 中（调用方按空处理）；
- JSON 解码失败显式报错（含 groupID 上下文），禁吞错；
- 空入参直接返回空 map，不发 SQL；
- 注释说明与既有 `Get` 的关系（同一列同一解码语义，仅批量）。

### 改动点 2：service 批量方法

`VisionRoutingService` 新增 `GetByGroupIDs(ctx, ids []int64) (map[int64]map[string][]int64, error)`
直通 repo（无缓存——与既有 `GetVisionRoutingAccountIDs` 直读语义一致，禁新造缓存层）。

### 改动点 3：admin 读取链水合

`admin_group.go` 中**喂给 admin 分组响应（`GroupFromServiceAdmin`）的读取方法**
（取证确认至少：GetGroup、ListGroups、GetAllGroups、GetAllGroupsIncludingInactive；
GetAllGroupsByPlatform 先查它的消费方，若也喂 admin 分组 DTO 则一并水合，
若只喂账号分组过滤等非分组管理场景则**不水合**并在报告里说明取证结论）：
- 单组：`Get`；多组：`GetByGroupIDs` 批量，一次查询按 groupID 分发，**禁止循环逐组查**；
- **读错误失败关闭**：水合查询出错时整个读取方法返回错误，禁止静默当 nil
  （管理端读不到配置应报错而非显示为空——空显示会诱导管理员误以为无规则）；
- 查询不到（组不在 map）→ `Group.VisionRouting` 置空 map（DB 默认 `{}` 语义）。

### 改动点 4：依赖注入

`adminServiceImpl` 增 visionRouting 依赖（repo 接口或 service 均可，择与文件内既有
依赖注入风格一致者）；构造函数签名变化同步 `service/wire.go` 与所有测试构造点
（含 handler/admin 包测试，先 `grep -rn 'adminServiceImpl{'` / `NewAdminService` 波及面
再动手；同步必须机械，发现非机械冲突 BLOCKED 顶回）。

### 明确不改（主会话裁定留痕）

- `admin_group_duplicate.go` Duplicate 路径**不回填** VisionRouting：
  vision_routing 引用源组账号 ID，复制组不带账号，携带配置违反 §3.7 同组不变量；
  DB 列默认 `{}` 已给安全空态。
- 调度读路径（scheduler resolveVisionRoutingTargets）不动。
- `GroupService.GetByID/List/ListActive` 不动（不在 admin DTO 链上）。

## 禁区

- 禁改：V2 单已改文件（group_handler.go、accounts.ts、GroupsView.vue）、
  group_service.go、ent/、迁移、V1 单文件（openai_gateway_handler.go、
  openai_account_scheduler.go）。
- 禁止 N+1（多组场景必须一次批量查询）；禁止新缓存层；禁止吞错。
- 禁止 `git checkout --` / `git restore`。
- 超范围发现一律 BLOCKED 顶回。
- 开工前后 `git status --short` 快照自证。

## 验证命令白名单（/mnt/data/sub2api/backend 下执行）

- `go build ./internal/service/ ./internal/repository/ ./internal/handler/...`
- `go vet ./internal/service/ ./internal/repository/ ./internal/handler/...`
- `go test ./internal/service/ -run 'VisionRouting|VisionHydration|AdminGroup' -count=1`
- `go test ./internal/handler/admin/ -run 'VisionRouting|Group' -count=1`

（全量测试由主会话收敛阶段统一执行。）

## Done when

1. 白名单命令全过，输出贴回。
2. GetGroup/ListGroups/GetAllGroups/GetAllGroupsIncludingInactive 返回的
   group.VisionRouting 与 DB 一致（含批量路径单次查询的测试证明）。
3. 水合读错误 → 方法返回错误（失败关闭测试）。
4. GetAllGroupsByPlatform 消费方取证结论写进报告。
5. `git status --short` 前后快照零越界。

## 模型

- 主力 hy3；额度耗尽切 `custom-local:deepseek-v4.1-flash`（必须带 `custom-local:`
  前缀），切换不需请示。
