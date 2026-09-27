# 派发单 Vision-U：能力缓存跨实例协议加固（P1×2）+ 检测 manual_review 分类收窄（P2×1）

## 背景

方案 SSOT `docs/capability-routing-plan.md` v3.1（已过闸①）。第 4 次终审（输出归档
`docs/review-packets/vision-final-gate2-r4-20260927/review-output.md`）返回 block，
3 条发现全部采纳。三发现共享接口签名与测试文件（按共享测试文件合并规则出单卡），
裁定记录见 `REVIEW-PACKET-vision-routing-final.md` §13/§14。

三条发现与实证位置：
- **P1-E**：`invalidateAndNotifyAccount` 在 Redis 失效失败时仍无条件广播
  （service :348-357）；订阅端（service :85-87、repo cache :136-140）只清本地缓存，
  不推进代际、不标 suspect。失效失败+广播成功 ⇒ 其他实例清本地后回读仍陈旧的 Redis
  并回填 local+Redis。违反方案 §3.3 多实例即时生效与失败关闭。
- **P1-F**：`maybeSetLocalAccount`（service :240-248）check（genMu）与 install
  （localCacheMu）两段锁之间 bumpGeneration 可插入 ⇒ 旧值毒化；且
  `refreshAccountCache` 失配时仍把陈旧 caps 返回当前请求。
- **P2-E**：`isVisionRefusalResponse`（vision_detect_service.go :541-578）通用能力性
  措辞（cannot identify/无法识别 + 图片关键词）即判 manual_review，与方案 §3.5
  两级规则冲突（普通文本拒绝应落 false；manual_review 仅限明确政策/策略拒答）。

## 改动范围（仅允许改以下 6 个文件）

1. `backend/internal/service/account_model_capability_service.go`
2. `backend/internal/service/account_model_capability_service_test.go`
3. `backend/internal/repository/account_model_capability_cache.go`
4. `backend/internal/repository/account_model_capability_cache_test.go`
5. `backend/internal/service/vision_detect_service.go`
6. `backend/internal/service/vision_detect_service_test.go`

（上列 6 个文件；除此之外一律禁改。）

### 改动点 A：缓存接口与跨实例协议（P1-E）

- 接口 `AccountModelCapabilityCache`（定义在 service 包内）签名扩展：
  - `NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error`
  - `SubscribeUpdates(ctx context.Context, handler func(accountID int64, suspectRedis bool))`
- repo 实现（account_model_capability_cache.go）：
  - `NotifyUpdate` 发布 JSON payload：`{"account_id":<int64>,"suspect_redis":<bool>}`。
  - `SubscribeUpdates` 解析：先 JSON；解析失败回退 `fmt.Sscanf("%d")` 纯数字
    （滚动升级期旧发布者）→ **一律按 suspectRedis=true 处理**（保守，强制 DB 回源）；
    两者都失败 → 跳过该消息（维持现状）。
  - 订阅端清本地缓存的既有行为保留。
- service `invalidateAndNotifyAccount`：广播改为
  `NotifyUpdate(ctx, accountID, invalidateErr != nil)`（失效失败也要广播，让其他
  实例知道须绕开 Redis）。
- service 构造器订阅回调改为：
  ```go
  cache.SubscribeUpdates(context.Background(), func(accountID int64, suspectRedis bool) {
      s.bumpGeneration(accountID)      // 推进代际：使本实例并发中的旧回源快照全部作废
      s.clearLocalAccount(accountID)
      if suspectRedis {
          s.markSuspectRedis(accountID)     // 发布者失效失败：Redis 可能陈旧，本实例绕开
      } else {
          s.unmarkSuspectRedis(accountID)   // 发布者失效成功：Redis 已干净，解除可疑
      }
  })
  ```
  注意：suspect 标记会持续到本实例下一次收到 success 事件或自身写路径失效成功——
  这是设计意图（失败关闭方向），不是缺陷，不要加 TTL/自动过期兜底。

### 改动点 B：代际复核原子化 + 失配不返回旧快照（P1-F）

- 删除 `maybeSetLocalAccount`（旧链归零），替换为原子安装函数，check 与 install
  在**同一把 genMu 持有期内**完成（锁序 genMu → localCacheMu，与现有
  clearLocalAccount/bumpGeneration 不构成环）：
  ```go
  // 返回 false = 代际已变，本次回源结果陈旧。
  func (s *AccountModelCapabilityService) installLocalIfGenerationMatches(
      accountID int64, caps []*model.AccountModelCapability, gen int64) bool
  ```
- `refreshAccountCache` 改为**限次重读循环**（maxAttempts=3）：
  1. 快照 gen；
  2. 读值（非 suspect：Redis 命中即用并回填；suspect 或未命中：回源 DB；
     DB 成功且非 suspect 时回写 Redis——本条沿用现有语义）；
  3. `installLocalIfGenerationMatches` 成功 → 返回 (caps, nil)；
  4. 失配 → 重试（重记快照重读）；3 次耗尽 → 返回显式错误
     （如 `fmt.Errorf("capability cache generation churned during refresh for account %d", accountID)`），
     **不得返回失配的陈旧 caps**——上层 `ModelSupportsVisionInput` 已把错误传播为
     失败关闭，这是设计要求。
- 调用点同步：`getAccountCapabilities`、`invalidateAndNotifyAccount`（后者只用 err，
  返回值可忽略）。

### 改动点 C：manual_review 分类收窄（P2-E）

`isVisionRefusalResponse` 重写为两级明确规则（方案 §3.5 权威口径）：
- **第一级（policy/refusal-agency 标记，判定 manual_review 的必要条件）**，仅收明确
  政策/意愿类措辞：
  - EN：`won't`、`will not`、`refuse`、`declines`（含 refuse/refused/decline 变体）、
    `not allowed`、`not permitted`、`against my`、`against the`、`policy`、`policies`、
    `guidelines`、`illegal`
  - 中文：`拒绝`、`不予`、`政策`、`违规`、`违反`、`合规`、`条款`、`不允许`、`禁止`
- **第二级（图片/验证码关键词）**：沿用现有 codeRelated 列表。
- 判定 `manual_review` = 第一级命中 **且** 第二级命中（保持 AND 锚定，防误扩）。
- **能力性措辞全部移出第一级**：`cannot recognize/cannot identify/cannot
  provide/cannot assist/cannot help/unable to */i cannot/i can't/无法识别/无法辨认/
  无法提供/无法协助/不能识别/不能辨认` 等 ⇒ 不再构成 manual_review，走既有
  "200 但无码 → unsupported 落 false" 路径。
- 同步更新函数注释与文件头 :52 附近的分类说明（supported 落 true / unsupported
  落 false / manual_review 仅限明确政策类拒答不自动落 false）。
- 判定基准用例（必须满足）：
  - `"I cannot identify the digits in the image"` → **unsupported**（终审原文反例）
  - `"I'm sorry, I won't help with reading captchas, it's against my policy"` → manual_review
  - `"我无法识别图片中的数字"` → unsupported
  - `"根据使用政策，我拒绝识别验证码"` → manual_review
  - 空串 / 纯无关文本 → 不构成拒答（走 unsupported）

### 改动点 D：测试

- `account_model_capability_service_test.go`：
  1. mock `NotifyUpdate`/`SubscribeUpdates` 同步新签名；mock 捕获 suspectRedis 实参。
  2. 新增：Redis 失效失败（mock InvalidateAccount 返回错误）时 `NotifyUpdate` 收到
     `suspectRedis=true`。
  3. 新增：订阅回调 suspect=true ⇒ 本实例后续读绕开 Redis（Redis 里预埋陈旧值不被
     读到，读到的是 DB 新值）；suspect=false ⇒ 解除可疑恢复 Redis 回源。
  4. 新增：并发安装竞态（确定性构造：install 前插入 bump）⇒ 陈旧值不毒化本地缓存
     （原子化后原 T2 竞态测试应继续通过，不得删除或削弱）。
  5. 新增：代际持续翻转 ⇒ refreshAccountCache 耗尽 3 次后返回错误而非旧快照。
  6. 新增：旧格式纯数字 payload 回调收到 suspectRedis=true。
- `account_model_capability_cache_test.go`（miniredis）：JSON payload 发布/订阅
  round-trip（suspect 两态）；纯数字 legacy payload → true。
- `vision_detect_service_test.go`：stub（:81-82 附近）同步新签名；
  `TestVisionDetectManualReview` 改用真政策措辞（"I'm sorry, I won't help with
  reading captchas, it's against my policy"）；按改动点 C 基准用例新增矩阵测试。

### 改动点 E：handler 测试桩机械签名同步（2026-09-27 14:10 主会话补授权）

首轮执行 BLOCKED 顶回 revealed 派发单漏列一个接口实现者。**主会话授权**将该文件
纳入本单可改范围（仅限下列两行机械签名同步，不得改任何测试逻辑）：

7. `backend/internal/handler/admin/vision_capability_handler_test.go`
   - :203 `fakeCapCache.NotifyUpdate` → `func (c *fakeCapCache) NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error { return nil }`
   - :204 `fakeCapCache.SubscribeUpdates` → `func (c *fakeCapCache) SubscribeUpdates(ctx context.Context, handler func(int64, bool)) {}`

## 禁区

- 禁改：scheduler、gateway service、wire*.go、setting_*、vision_routing_service、
  handler 生产代码与 vision_capability_handler_test.go 中改动点 E 两行以外的任何内容、
  前端、迁移文件、`vision_detect_service.go` 中分类函数以外的任何逻辑。
- 禁止 `git checkout --` / `git restore`（共享工作区有其他任务改动）。
- 禁止新增 TTL/自动解除 suspect 的兜底机制；禁止吞错（所有新错误路径显式传播）。
- 超出本单范围的发现一律 BLOCKED 顶回，不自行扩大改动。
- 开工前后 `git status --short` 快照自证改动仅限上列 6 文件。

## 验证命令白名单（只允许跑以下命令；在 /mnt/data/sub2api/backend 下执行）

- `go build ./internal/service/ ./internal/repository/`
- `go vet ./internal/service/ ./internal/repository/`
- `go test ./internal/service/ -run 'AccountModelCapability|VisionDetect' -count=1`
- `go test ./internal/service/ -race -run 'AccountModelCapability' -count=1`
- `go test ./internal/repository/ -run 'AccountModelCapability' -count=1`
- `go build ./internal/handler/admin/ && go vet ./internal/handler/admin/`（验证改动点 E 后该包测试可编译；vet 即含测试编译）

（全量构建/测试由主会话收敛阶段统一执行，你不必也不得跑 `go test ./...`。）

## Done when

1. 上列 6 条白名单命令全部通过（含 -race），输出原样贴回。
2. 改动点 A/B/C/D 逐条落地；`maybeSetLocalAccount` 已删除、无残留引用。
3. 改动点 C 的 5 条基准用例全部按期望分类。
4. `git status --short` 开工前后快照证明改动仅限 7 文件清单（6+改动点 E 文件），贴回。
5. 任何超出派发单前提的问题（接口改动的其他波及点、既有测试与新政则冲突且
   本单未列）→ BLOCKED 顶回并列出证据，不自行裁决。

## 模型

- 主力 hy3；额度耗尽切 `custom-local:deepseek-v4.1-flash`（必须带 `custom-local:`
  前缀），切换不需请示。
