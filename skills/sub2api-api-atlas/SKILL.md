---
name: sub2api-api-atlas
description: 给 Sub2API 站点改生产数据或查接口时加载：改账号/分组/订阅/额度/余额/倍率/并发/优先级/价格/激活码/代理，或说直接改、批量改、后台怎么操作、走哪个接口、补个接口。含 809 条路由（方法/路径/handler/文件行号）、鉴权/幂等/step-up 约定、accounts/groups/subscriptions/quota_keys/settings 五域请求体与覆盖语义。其它项目不适用。
---

# Sub2API API Atlas

**这是查找层，不是需求层。** 它只回答"接口在哪、怎么调、请求体长什么样"，
不定义业务规则。业务语义冲突时以源码与 `docs-local/` 方案文档为准。

## 什么时候用

- 要改生产数据，不知道走哪个端点 → 查路由索引。
- 知道端点，不确定请求体字段名/类型/覆盖语义 → 查 `references/write-playbooks.md`。
- 想知道某端点实现细节 → 索引给了 `文件:行`，直接读 handler。
- 怀疑索引过期 → `scripts/gen-route-atlas.py --check`。

## 目录

| 文件 | 内容 |
| --- | --- |
| `references/routes-admin.md` | `/api/v1/admin/*` 全部端点，按域分桶（每桶标端点数） |
| `references/routes-gateway-user-auth.md` | 网关 `/v1`、用户面、鉴权、支付、内部/货源端点 |
| `references/conventions.md` | 鉴权、响应封装、分页、幂等、step-up、错误码 |
| `references/write-playbooks.md` | accounts / groups / subscriptions / quota_keys / settings 五域写入体 |
| `scripts/gen-route-atlas.py` | 从 `backend/internal/server/routes/*.go` 重新生成索引 |

索引由脚本从源码生成，不再手工维护。改路由后重跑：

```bash
python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py          # 重新生成
python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py --check  # 只比对，不一致退出码 1
```

脚本只读源码解析 `Group()` 链与注册调用，不联网、不连库、不编译。

## 加载路径（自动触发靠这一步）

skill 正文在仓库 `skills/sub2api-api-atlas/`，但**两个 CLI 都不扫描仓库 `skills/` 目录**：

- Claude Code 只扫 `~/.claude/skills/`（用户级）和 `<项目>/.claude/skills/`（项目级）；
- Codex 只扫 `~/.codex/skills/`。

所以本机已装两条用户级软链（跨项目、跨 worktree 都可见，不进 git）：

```bash
ln -sfn /mnt/data/sub2api/skills/sub2api-api-atlas ~/.claude/skills/sub2api-api-atlas
ln -sfn /mnt/data/sub2api/skills/sub2api-api-atlas ~/.codex/skills/sub2api-api-atlas
```

同机其它账号要重新装；换机器/算力机同样跑这两行。
**不要**再往 `.claude/skills/` 放第三条软链——项目级那条已被移除，
重复安装只会制造"改了一份、另一份没生效"的假象。

触发方式：`description` 里的词命中即隐式加载。这套词是从本机 4998 条真实用户输入里
按实际用词频次挑的，不是照写在正文里的术语——你说"改账号/改分组/改额度/改倍率/
改并发/改优先级/改价格"、"直接改"、"批量改"、"后台怎么操作"、"走哪个接口" 都会命中。
也可以显式点名 `sub2api-api-atlas`。软链装好后新开会话才生效，当前会话不会热加载。

**刻意的反例**（这些词在语料里指业务逻辑，不是接口，故意不写进 description，
避免误触发把上下文浪费在无关任务上）：

- 路由 / 调度 / 探针 / 熔断 / 冷却 —— 你几乎每次都在说账号调度逻辑，不是 HTTP 路由；
- 端点 / admin / API / 请求体 —— 语料里各出现个位数次，是术语不是你的口语；
- 写分组 / 写订阅 / 建账号 / 改设置 —— 语料里 0~1 次，是我照文档造的假词。

词表要改，先跑 `grep` 查一遍真实语料频次再改，别凭感觉加。

## 使用顺序

1. 定鉴权域（管理员 / 登录用户 / API Key）→ 决定用哪个索引文件。
2. 在索引里定位端点，拿 `文件:行`。
3. 该域有 playbook 就读 playbook；没有就直接读 handler 里绑定的 Request struct。
4. 写入前先只读核对目标集合（`GET` 或 `.../all`），再执行写入。
5. 写入后再读一次确认生效值。

## 与既有文档的边界

- `skills/sub2api-admin`（上游维护）：CLI 封装，约 50 个子命令，覆盖
  accounts / redeem-codes / groups / proxies 等。能覆盖的场景优先用它，少写
  一次性脚本；本 skill 补的是它没铺到的部分（全量索引 + 五个高频域的请求体
  口径 + 覆盖/合并语义）。两者冲突以本 skill 的源码锚点为准——那边是 CLI
  封装，不保证与最新 handler 同步。
- `docs/ADMIN_PAYMENT_INTEGRATION_API.md`：支付集成的流程与报文规范，正文在
  那边；本 skill 只登记支付端点在哪，不复制其内容。
- `docs-local/*-plan.md`：业务语义与设计决策的事实源。本 skill 只做接口定位，
  不定义业务规则，也不与方案文档竞争。

## 硬性注意

- 本索引里的路径是**源码注册路径**；实际请求前缀在网关/反代后可能是
  `https://<host>/api/v1/...`，以部署配置为准。
- 索引包含端点 ≠ 当前部署可用：handler 未接线时整组不注册（源码里带 nil 守卫），
  部署形态不同可见端点也不同。
- 源码里同一路径注册两个变体（无尾斜杠 + 带尾斜杠，如 `xgj_supply.go` 的
  货源模式组）时，索引按真实注册各列一条，条数会多于 handler 数量。
- 请求体字段以源码 struct 为唯一事实源。本 skill 的 playbook 是对源码的摘要，
  字段新增后以源码为准；发现不一致先修 playbook 再继续。
