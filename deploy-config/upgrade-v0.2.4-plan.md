# 安全升级方案：v0.1.192 → 上游 v0.2.4

> 生成时间：2026-09-09。基于对本地仓库与上游 `Wei-Shaw/sub2api` 的试合并实测结果，
> 部署流程对齐 `sub2api-ops.md` 第 5 节（服务器端构建镜像）。

## 1. 现状与风险评估

| 项目 | 现状 |
| --- | --- |
| 本地基线 | 上游 `v0.1.192`（merge-base `efb46db0a`） |
| 落后上游 | **6726 个提交**（目标 `v0.2.4`，commit `5de5e2bed`） |
| 二次开发 | **153 个自定义提交**（闲鱼模块、定价/折扣展示、i18n 等） |
| 试合并结果 | **42 个文件冲突**（临时 worktree 实测，未动工作区） |
| 数据库迁移 | `backend/migrations/` 共 296 个 SQL，启动时由 setup 流程（`AUTO_SETUP=true`）自动应用，`schema_migrations` 表做校验和跟踪 |

**核心结论**：git merge 不会覆盖二开内容——闲鱼模块等上游不存在的文件原样保留；
风险集中在三处：① 42 个冲突文件的取舍；② 跨 6000+ 提交的数据库迁移；③ 合并后的运行时回归。

## 2. 三条红线（绝不能做）

1. **禁止用管理后台"检查更新"一键升级**。该流程拉取的是上游官方代码，会把二开镜像直接替换成官方版。
2. **禁止用"下载新版覆盖目录"或 `git checkout upstream/main -- .` 方式升级**。这才是真正覆盖二开的做法。
3. **禁止跳过数据库备份直接启动新版本**。SQL 迁移只进不退（无 down 迁移），一旦启动应用了新迁移，
   旧版本代码+新表结构可能无法回退，唯一退路是恢复备份。

## 3. 前置准备（备份清单）

```bash
# 3.1 代码快照（本地 + fork 双保险）
git branch backup/pre-v0.2.4-merge
git push origin backup/pre-v0.2.4-merge

# 3.2 数据库备份（在服务器上执行；compose 服务名以 docker compose ps 为准）
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env \
  exec postgres pg_dump -U sub2api -d sub2api -Fc -f /tmp/sub2api-pre-v024.dump
docker cp sub2api-postgres:/tmp/sub2api-pre-v024.dump /opt/sub2api/backup/  # 容器名按实际调整
# 并把 dump 复制到服务器之外一份

# 3.3 配置备份
cp /opt/sub2api/.env /opt/sub2api/.env.bak-$(date +%Y%m%d)

# 3.4 记录当前在跑的镜像 tag（回滚要用）
docker inspect sub2api --format '{{.Config.Image}}'
```

选择低峰维护窗口。后端构建约 5-10 分钟（见 ops 文档第 5 节）。

## 4. 阶段一：本地合并与冲突解决

```bash
git fetch upstream --tags
git checkout -b sync/upstream-v0.2.4
git merge upstream/main
git diff --name-only --diff-filter=U        # 列出冲突文件，应与下表一致
```

### 4.1 冲突文件分类与解决策略（共 42 个）

| 类别 | 文件 | 策略 |
| --- | --- | --- |
| 版本号（1） | `backend/cmd/server/VERSION` | 直接取上游 `0.2.4` |
| 代码生成（1） | `backend/cmd/server/wire_gen.go` | **不手工合并**。先解决其依赖注入涉及的源文件冲突，再在 `backend/` 下跑 `go generate ./cmd/server` 重新生成 |
| ent schema（1） | `backend/ent/schema/user_platform_quota.go` | 手工合并两边字段后 `go generate ./ent` 重新生成 ent 代码，并核对 `migrations/` 是否需要补充迁移 |
| 配置与常量（4） | `backend/internal/config/config.go`、`backend/internal/domain/constants.go`、`backend/internal/service/domain_constants.go`、`frontend/src/constants/platforms.ts` | **两边都保留**：上游新增配置项/常量全收，二开自定义项（含闲鱼相关）逐项保住 |
| 核心后端逻辑（约 12） | `service/` 下 account 调度、scheduler_snapshot、openai_gateway_*、pricing_service、channel_monitor、cn_provider_quota、upstream_models 等 | 逐文件读代码取舍，工作量最大。以"上游逻辑为骨架、二开功能为增量挂载点"为原则；每个文件解决后立即 `go build ./...` 验证 |
| 路由（1） | `backend/internal/server/routes/gateway.go` | 两边路由都保留，注意参考既往经验（如 cost-basis 路由挂载组问题） |
| 前端组件（约 10） | `CreateAccountModal.vue`、`EditAccountModal.vue`、`AccountUsageCell.vue`、`CNProviderQuotaCell.vue`、`ModelWhitelistSelector.vue`、`credentialsBuilder.ts`、`PlatformTypeBadge.vue`、`UseKeyModal.vue` 等 | 两边都保留：上游新平台/新能力 + 二开的闲鱼/折扣 UI；解决完跑 `npm run typecheck` |
| i18n（4） | `zh/en` 的 `admin/accounts.ts`、`admin/overview.ts` | 两边的新增 key 都保留；key 重名时保留上游语义、二开文案重命名 |
| 类型与工具（2） | `frontend/src/types/index.ts`、`frontend/src/utils/platformColors.ts` | 两边新增类型/映射都保留 |
| 测试文件（约 8） | `*_test.go`、`__tests__/*.spec.ts`、`api_contract_test.go` | 合并两边用例；跑不过时优先怀疑合并语义而非盲目删用例 |

> 提示：闲鱼模块（`xianyu_*.go`、`xianyu.ts` 等）不在冲突列表中，合并后原样保留，无需处理。

### 4.2 完成标准

```bash
git add -A && git commit          # 完成合并提交
```

## 5. 阶段二：本地验证（合并提交后、部署前）

```bash
# 后端
cd backend
go build ./...
make test                          # go test ./... + golangci-lint（如无 lint 环境可只跑 go test ./...）

# 前端
cd ../frontend
npm run typecheck
npm run test:run
npm run build
```

重点回归（冲突集中区域）：
- 账号创建/编辑弹窗各平台均能正常打开、保存；
- 网关转发一条 OpenAI/Anthropic/Codex 链路真实请求成功；
- 定价/成本核算页与模型列表页数据正常；
- 闲鱼全链路（控制、发货、worker 通信）正常；
- 渠道监控与调度快照无启动报错。

## 6. 阶段三：服务器部署（按 ops 文档第 5 节）

```bash
# 6.1 同步合并后代码到服务器构建目录（沿用现有 rsync/scp 流程）
TARGET=<新commit短hash>
# rsync -az --delete <排除项> 服务器:/opt/sub2api/build-$TARGET/

# 6.2 服务器端构建镜像（后台 + 日志）
cd /opt/sub2api/build-$TARGET && nohup docker build -t sub2api:$TARGET-w \
  -f deploy-config/<Dockerfile路径> . > /tmp/build-$TARGET.log 2>&1 &

# 6.3 切换镜像并滚动更新（务必带 --env-file）
cd /opt/sub2api && sed -i "s/^SUB2API_IMAGE_TAG=.*/SUB2API_IMAGE_TAG=$TARGET-w/" .env
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d sub2api

# 6.4 健康检查 + 实时盯启动日志（重点看迁移应用输出）
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps   # Up (healthy)
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs -f sub2api
```

**本次跨 6000+ 提交，首次启动会批量应用大量新迁移。** 日志中出现迁移报错时，
立即停机排查（不要反复重启），必要时恢复第 3 节的数据库备份。

## 7. 阶段四：上线验证清单

- [ ] 容器 `Up (healthy)`，启动日志无迁移/schema 报错
- [ ] 管理员登录正常，系统概览数据正常
- [ ] 网关转发：真实调用一条 OpenAI 与一条 Anthropic/Codex 请求成功计费
- [ ] 密钥页分组选择器折扣展示正常（二开功能）
- [ ] 定价/成本核算页、模型列表开关正常（二开功能）
- [ ] 闲鱼账号创建/编辑、发货链路、worker 通信正常（二开功能）
- [ ] 渠道监控 V2 页面正常
- [ ] 观察至少一个业务高峰后再宣告完成

## 8. 回滚预案

```bash
# 代码/镜像层：把 .env 的 SUB2API_IMAGE_TAG 指回第 3.4 步记录的旧 tag，重新 up -d
# 数据层：若新迁移已应用且旧镜像无法兼容新表结构，恢复第 3.2 步的 pg_dump 备份
# 最终退路：git checkout backup/pre-v0.2.4-merge 重新构建旧版本镜像
```

镜像回滚与数据库恢复**必须成对评估**：先只回滚镜像验证，若启动报 schema 不兼容，再恢复数据库备份。

## 9. 后续维护节奏（本次升级完成后落实）

- 每月或每个上游 release 同步一次，把冲突量控制在个位数（本次 42 个是因为累积了 6726 提交）；
- 在 `sub2api-ops.md` 维护一份"二开触碰文件清单"，每次同步前对照试合并结果快速评估工作量；
- 同步前先跑一次临时 worktree 试合并（`git worktree add /tmp/x HEAD && cd /tmp/x && git merge --no-commit upstream/main`），
  拿到冲突清单再决定窗口期。
