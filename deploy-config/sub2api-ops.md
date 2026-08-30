# Sub2API 运维部署指南 (Operations & Deployment Guide)

> 本文档描述生产服务器（`yiyutu-server`，域名 `corealgos.com`）当前 Sub2API 部署的完整运维流程。
> 项目上游：https://github.com/Wei-Shaw/sub2api （LGPL-3.0）

---

## 目录

1. [部署概览](#1-部署概览)
2. [本地源码仓库](#2-本地源码仓库)
3. [生产部署结构](#3-生产部署结构)
4. [常用运维命令](#4-常用运维命令)
5. [升级流程](#5-升级流程)
6. [备份与恢复](#6-备份与恢复)
7. [故障排查](#7-故障排查)
8. [安全注意事项](#8-安全注意事项)
9. [回滚操作](#9-回滚操作)

---

## 1. 部署概览

| 项目 | 值 |
|------|------|
| 生产服务器 | `yiyutu-server`（SSH 别名），HostName `43.250.173.110` |
| 登录用户 | `root` |
| SSH 密钥 | `~/.ssh/yiyutu_root_ed25519`（权限 600） |
| SSH 配置 | `~/.ssh/config`（本机 `zjy` 用户已配置 `Host yiyutu-server` 别名） |
| 域名 | `corealgos.com` / `www.corealgos.com`（HTTPS 443） |
| 部署目录 | `/opt/sub2api` |
| 反向代理 | Nginx → `127.0.0.1:3300` |
| 应用服务 | `weishaw/sub2api:latest`，容器名 `sub2api` |
| 后端组件 | `postgres:15-alpine`（容器 `sub2api-postgres`）、`redis:7-alpine`（容器 `sub2api-redis`） |
| 版本 | `v2026.06.10` |
| 管理员 | `trzjy2013@gmail.com` |

### 架构

```
浏览器 / API 客户端
        │  HTTPS 443
        ▼
      Nginx  (corealgos.com)  ── 反向代理 ──►  127.0.0.1:3300
                                                    │
                                              sub2api (容器内 :8080)
                                             /      |      \
                                          PostgreSQL   Redis
                                            :5432      :6379
```

- 外部仅开放 22/80/443；应用、数据库、Redis 均绑定内网
- Nginx 已配置 `underscores_in_headers on;`（sub2api 粘性会话依赖 `session_id` 头）

---

## 2. 本地源码仓库

本地维护 fork（`trzjy/sub2api`），源码与部署配置同仓，部署配置位于 `deploy-config/`：

```bash
# 首次克隆 fork
git clone https://github.com/trzjy/sub2api.git /mnt/data/sub2api
cd /mnt/data/sub2api

# 配置上游 remote（用于同步上游更新）
git remote add upstream https://github.com/Wei-Shaw/sub2api.git
```

- `origin` = fork `trzjy/sub2api`（可推送、管理部署配置）
- `upstream` = 上游 `Wei-Shaw/sub2api`（只读同步）
- 许可 LGPL-3.0

### 与上游同步

```bash
git fetch upstream
git merge upstream/main   # 或 git pull upstream main
git push origin main
```

### 仓库结构

- `backend/`、`frontend/` — 上游源码
- `deploy/` — 上游部署模板
- `deploy-config/` — **本项目部署配置**（compose、nginx、.env.example、本运维文档）

> 注意：上游 `docs/*` 被其 `.gitignore` 忽略，因此本项目部署文档放 `deploy-config/`（不在 `docs/`）。

---

## 3. 生产部署结构

部署目录 `/opt/sub2api`：

| 路径 | 说明 |
|------|------|
| `compose.yml` | Docker Compose 定义（sub2api + postgres + redis） |
| `.env` | 环境配置与凭据（权限 600，勿提交到 Git） |
| `data/` | sub2api 数据目录（上传文件、本地定价缓存等） |
| `postgres_data/` | PostgreSQL 数据目录 |
| `redis_data/` | Redis 持久化目录 |

### 3.1 关键配置（`.env`）

| 变量 | 说明 |
|------|------|
| `BIND_HOST=127.0.0.1` | 仅监听内网，由 Nginx 反代 |
| `SERVER_PORT=3300` | 宿主机暴露端口（容器内固定 8080） |
| `POSTGRES_USER/PASSWORD/DB` | 数据库凭据 |
| `REDIS_PASSWORD` | 留空（内网） |
| `ADMIN_EMAIL/ADMIN_PASSWORD` | 首次启动 `AUTO_SETUP` 创建管理员 |
| `JWT_SECRET` / `TOTP_ENCRYPTION_KEY` | 固定值，保证重启后登录态与 2FA 有效 |

> ⚠️ `JWT_SECRET`、`TOTP_ENCRYPTION_KEY` 必须保持固定。若清空，重启后所有用户被登出、已有 2FA 失效。

> **重要**：compose 文件位于 `deploy-config/` 子目录，而真实凭据在根目录 `/opt/sub2api/.env`。因此所有 `docker compose` 命令必须显式加 `--env-file /opt/sub2api/.env`，否则 `${POSTGRES_PASSWORD}` 等插值会缺失，导致数据库/登录异常。示例见第 4 节。

### 3.2 Nginx 配置

站点配置：`/etc/nginx/sites-available/duizhang`（symlink 到 sites-enabled）。
`proxy_pass http://127.0.0.1:3300;`，并在 `nginx.conf` 的 `http` 块含：

```nginx
underscores_in_headers on;
```

变更后：`nginx -t && systemctl reload nginx`

---

## 4. 常用运维命令

### 4.1 登录服务器

```bash
# 方式一（推荐，使用已配置的 SSH 别名）
ssh yiyutu-server

# 方式二（显式指定）
ssh -i ~/.ssh/yiyutu_root_ed25519 root@43.250.173.110

# 从本地复制文件到服务器 / 从服务器拉取
scp ./local-file yiyutu-server:/opt/sub2api/
scp yiyutu-server:/tmp/backup.tar.gz ./
```

> 前提：本机 `~/.ssh/config` 已配置 `Host yiyutu-server`（HostName、User、IdentityFile），私钥权限必须为 `600`。

```bash
# 查看服务状态
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps

# 重启
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env restart

# 日志（实时 / 最近 100 行）
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs -f sub2api
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs --tail=100 sub2api

# 健康检查
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3300/health
docker inspect --format '{{.State.Health.Status}}' sub2api

# 公网检查
curl -s https://corealgos.com/
curl -sI https://corealgos.com/api/v1/auth/me  # 返回 401 属正常（未带 token）

# 数据库直查
docker exec -it sub2api-postgres psql -U sub2api -d sub2api

# Redis
docker exec sub2api-redis redis-cli ping   # 期望 PONG
```

---

## 5. 升级流程

```bash
# 1. 备份当前数据（见第 6 节）
# 2. 拉取最新镜像
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env pull sub2api
# 3. 记录当前镜像供回滚
docker images --format "{{.Repository}}:{{.Tag}} {{.ID}}" weishaw/sub2api
# 4. 重建容器
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d --remove-orphans sub2api
# 5. 验证
sleep 15 && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps && curl -s http://127.0.0.1:3300/health
```

> 也可在管理后台左上角"检查更新"一键升级（支持回滚）。

---

## 6. 备份与恢复

### 6.1 数据目录备份（推荐整体打包）

```bash
cd /opt && tar -czf /tmp/sub2api-backup-$(date +%Y%m%d-%H%M%S).tar.gz sub2api/
# 下载到本地
scp yiyutu-server:/tmp/sub2api-backup-*.tar.gz ./
```

### 6.2 数据库备份

```bash
docker exec sub2api-postgres pg_dump -U sub2api -d sub2api > /tmp/sub2api-db-$(date +%Y%m%d).sql
```

### 6.3 恢复

```bash
# 整体目录恢复到新服务器
cd /opt && tar -xzf sub2api-backup-*.tar.gz
cd sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d
```

### 6.4 既有历史备份（勿删除）

| 备份 | 位置 |
|------|------|
| new-api 全量 | `/tmp/new-api-full-backup-20260826-214145.tar.gz`（服务器 + 本地副本） |
| 旧渠道/旧 one-api 数据 | `/opt/backups/sub2api-migration-olddata/` |

---

## 7. 故障排查

### 7.1 容器无法启动

```bash
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs --tail=200 sub2api
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps
```

- 若提示数据库连接失败：确认为 `postgres`、`redis` 先于 sub2api 就绪（compose 已声明 `depends_on` 健康条件）
- 数据库 schema 迁移由应用启动时自动执行

### 7.2 数据库问题

```bash
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps postgres
docker exec sub2api-postgres pg_isready -U sub2api -d sub2api
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs --tail=100 postgres
```

### 7.3 Redis 问题

```bash
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps redis
docker exec sub2api-redis redis-cli ping   # PONG
```

### 7.4 HTTPS / 域名

```bash
nginx -t
echo | openssl s_client -servername corealgos.com -connect corealgos.com:443 2>/dev/null | openssl x509 -noout -dates
systemctl reload nginx
```

### 7.5 登录/会话异常

- 确认 `JWT_SECRET` 未被清空/更换（否则所有会话失效）
- 确认 Nginx 含 `underscores_in_headers on;`（否则粘性会话头丢失）
- 首次登录前需在管理后台完成"管理员合规确认"

---

## 8. 安全注意事项

1. **管理员凭据**：登录后建议定期轮换；勿在文档中明文保存真实密码
2. **`.env` 权限**：`chmod 600 /opt/sub2api/.env`，禁止提交到任何 Git 仓库
3. **防火墙**：仅开放 22/80/443；应用/DB/Redis 仅内网监听
4. **数据库**：PostgreSQL 未对外暴露端口；生产可通过 `security.url_allowlist` 收紧上游 URL（HTTPS 校验）到核心
5. **备份纪律**：重大升级前必备份；备份文件勿长期留于公共可读路径
6. **上游合规**：sub2api 用于分发订阅配额，使用前已确认上游（Anthropic 等）服务条款与当地法规

---

## 9. 回滚操作

```bash
# 1. 记录/确认要回滚的镜像
docker images weishaw/sub2api
# 2. 拉取目标版本并标记
docker pull weishaw/sub2api:<old-version>
docker tag weishaw/sub2api:<old-version> weishaw/sub2api:latest
# 3. 重建
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d --force-recreate sub2api
# 4. 验证
curl -s http://127.0.0.1:3300/health && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps
```

> 若紧急停服：`docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env stop sub2api`，处理完毕后 `docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env start sub2api`。

---

## 10. SMTP 自动发信配置

应用使用 Google Workspace 发送注册验证、密码重置等系统邮件。密码保存在本机 GNOME Keyring，不写入仓库或文档。

### 10.1 SMTP 参数

| 字段 | 值 |
|------|------|
| SMTP Host | `smtp.gmail.com` |
| Port | `587` |
| Encryption | `STARTTLS` |
| Username | `trzjy2013@gmail.com` |
| From Email | `trzjy2013@gmail.com` |
| Sender Name | `Sub2API` |
| Password | 存放于 GNOME Keyring，文档不记录 |

### 10.2 应用专用密码元数据

| 字段 | 值 |
|------|------|
| 应用名称 | `corealgos.com` |
| 创建时间 | `07:03` |
| 用途 | Sub2API 自动发信 |

### 10.3 本机密钥环取回命令

```bash
python3 -m keyring get smtp.gmail.com trzjy2013@gmail.com
```

> 该命令输出即 SMTP Password。粘贴到 sub2api 管理后台时需确认已保存成功，随后关闭终端输出上下文。

---

## 11. 闲鱼自动发货控制面（Xianyu Worker + 主程序控制面板）

### 11.1 部署说明

- 生产 Compose（`deploy-config/compose.yml`）新增 `xianyu-worker`、`xianyu-worker-mysql`、`xianyu-worker-redis`。
- Worker 相关服务只加入 `xianyu-internal` 网络，**不映射任何公网/宿主机端口**；主程序同时加入 `sub2api-network` 与 `xianyu-internal`。
- 主程序通过 `http://xianyu-worker-backend:8089` 访问 Worker（Docker 服务别名）。面板设置中的 Worker 地址**不允许** `127.0.0.1`（容器内指向主程序自身），也不允许公网域名。
- 主程序→Worker 走 backend-web 的 `/api/v1/internal/*` 内网服务路由（`internal_api.py`：账号投影、启停、Cookie 续期、清除凭证、扫码登录、商品投影、消息发送回执转发），统一复用现有 `AccountService`/`ItemService`/`qr_login_manager`，不复制第二套账号/商品/登录存储。
- 服务间双向认证使用同一 `SUB2API_INTERNAL_TOKEN`：主程序调 Worker 时以 `X-Worker-Token` 头发送，backend `deps.get_service_or_user` 校验并绑定到现有管理员用户（owner 语义沿用该用户，不新建平行用户）；Worker 回传主程序时以 `X-Internal-Token` 头发送。`XIANYU_INTERNAL_TOKEN` 与主程序 `xianyu_delivery.internal_token` 同值。
- ⚠️ **服务令牌是「高权限全局系统令牌」**：绑定到现有管理员用户，owner 作用域沿袭管理员全局语义，唯一受信调用方是主程序；internal API 不提供多租户隔离。Worker 前端普通用户（JWT）访问各自 owner 隔离仍有效，但不得把 internal API 暴露给公网或非受信调用方。
- Worker 回传端点固定为 `POST /api/v1/internal/xianyu/delivery-results`，仅 Worker 可通过 `X-Internal-Token` 调用；Nginx/Caddy 继续拒绝 `/api/v1/internal/` 前缀。
- 旧配置键 `xianyu_delivery.item_pools` 已废弃：只由一次性迁移器读取一次（`xianyu_delivery.legacy_migrated` 标记完成后不再读取），运行时业务配置全部落库。

### 11.2 `.env` 新增变量

| 变量 | 说明 |
|------|------|
| `XIANYU_INTERNAL_TOKEN` | Worker↔主程序双向认证 token，与 `xianyu_delivery.internal_token` 同值；同时作为 Worker 镜像内 `SUB2API_INTERNAL_TOKEN`（经 compose `environment` 注入） |
| `SUB2API_INTERNAL_BASE_URL` | Worker 容器内注入（compose 固定 `http://sub2api:8080`），用于 Worker 回传主程序 delivery-results |
| `XIANYU_WORKER_IMAGE_TAG` | Worker 镜像固定 tag（禁止 latest/reviewed 漂浮标签）。**必填**：镜像在部署主机直接构建（本机构建无 registry RepoDigest，`@sha256` digest 引用无法解析，故用固定 tag 引用）。旧 `sha256:8343c385...46d5` 已废弃（不含 launcher / `/api/v1/internal/*` / delivery-results 回传）。部署后通过 `docker inspect <容器> --format '{{.Image}}'` 校验运行容器镜像 ID 与构建产物一致（见 11.4）。**当前生产值**（2026-08-30，含 EnsureDeliveryRecord 适配层）：`prod-fix34-ensure-20260830-203928`（构建镜像 ID `sha256:10e96f9f54638945e131b04e1b969d33865b3de0b8e1f72f1863f3941fa3d640`）。中间版 `prod-fix34-20260830-172747` / `sha256:d1fd4b249713...cb518` 已由本版本替代。 |
| `XIANYU_WORKER_MYSQL_USER/PASSWORD/ROOT_PASSWORD/DB` | Worker 独立 MySQL 凭据 |

### 11.3 验证命令

```bash
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps
# Worker 9000/8089/8090/MySQL(3306)/Redis(6379) 公网访问必须全部不可达
bash deploy/tests/xianyu-deployment-boundary-test.sh
```

### 11.4 Worker 镜像构建与回传补丁

- **镜像构建**：以 `deploy-config/xianyu-auto-reply-src/backend-web/Dockerfile` 构建（该 Dockerfile 已统一 `COPY common/backend-web/websocket/scheduler/launcher`，EXPOSE 8089/8090/8091，并安装三端依赖）；`launcher/entrypoint.py` 在单容器内并行启动 backend-web(8089)/websocket(8090)/scheduler(8091)。
- **delivery-results 回传**：Worker 端 `common/services/sub2api_delivery_result_client.py` 在自动发货获得平台最终发送回执后回传 `POST {SUB2API_INTERNAL_BASE_URL}/api/v1/internal/xianyu/delivery-results`（`confirmed=true` 才标记 sent）；未配置 base_url/token 时静默跳过，不影响本地/单机模式。主程序保持 `pending` 直至收到 `confirmed=true`，否则最终转人工。
- **cookie_id 透传**：Worker 卡券（发货）调用主程序 Claim 时透传 `cookie_id` 作为账号身份（`XianyuDeliveryClaimRequest.cookie_id`）。
- **digest 纪律**：`XIANYU_WORKER_IMAGE_DIGEST` 中 `sha256:8343c385...46d5` 是**旧**审查镜像。本轮已在 vendored 源码补齐 launcher、`/api/v1/internal/*` 路由、服务身份认证与 delivery-results 回传，并经独立高风险复审收敛了 8 项 must_fix（回传有限重试、图片发货回执、显式成功判定、补发先改状态、websocket/scheduler internal 鉴权、空 token 失败关闭、wait_timeout clamp、digest 必填）。随后对补发/发货链路做了**基座底层重构**（见下方 11.5），部署前必须：从当前仓库构建新镜像 → 用新 digest 替换 `XIANYU_WORKER_IMAGE_DIGEST`（`.env.example` 与生产 `.env`）。部署主机 `/opt/sub2api/xianyu-auto-reply-src/` 是运行时同步副本，变更镜像前必须从仓库构建、重新审查并更新 digest。
- **internal 服务间鉴权**：backend-web→websocket/scheduler 的 `/internal/*` 路由要求 `X-Internal-Token` 匹配 `SUB2API_INTERNAL_TOKEN`（空配置失败关闭）；backend-web/scheduler 的 http_client 对 internal 服务 URL 自动注入该头。

### 11.5 基座底层重构要点（补发/发货链路）

- **attempt_count 语义统一**：`0`=初始自动发货（未补发），`N>=1`=第 N 次补发。`Claim` 写入 `0`；`ResendOriginalCode` 每次 `attempt_count+1`。存量数据经迁移 `backend/migrations/234_xianyu_attempt_count_normalize.sql` 归一化（`GREATEST(attempt_count-1,0)`）。此修复使自动发货回执（attempt=0）能正确关闭新 claim（旧实现 Claim 写 1 导致回执被静默丢弃、订单永久 pending）。
- **中心化状态转换**：`xianyu_order_claim_state.go` 新增 `applyClaimTransition` 原语，统一承担 advisory lock + attempt CAS + 幂等/冲突分类。`RecordDeliveryResult`（唯一回执入口，含补发成功/回滚）与 `ResendOriginalCode` 全部收敛于此；原 `FailResendClaim`/`MarkResendSent` 已删除合并。对 sent/legacy 终态或 attempt 不匹配的迟到回执按幂等忽略（2xx ack），消除回传重试循环。
- **统一回执枚举**（Python/跨层）：`common/services/receipt_outcome.py` 定义 `ReceiptOutcome`（`dispatched_definite_failure / sent_explicit_success / rejected / unknown_pending`），取代旧 `dispatched` 布尔 + `send_status` 三态 + `(success, confirmed)` 二元组三套编码。同步响应与异步兜底回传都由同一枚举派生。
- **回执 Future 收口**：`common/services/receipt_registry.py`（`ReceiptRegistry`）成为 `_pending_mid_futures` 的唯一属主，`register/dispatch/discard/sweep` 统一注册、响应分派、清理与超时兜底，消除散落 6+ 处的清理逻辑。
- **worker client 收敛**：`do()` 单一信封解码（优先 `data` 子对象，去掉顶层二次覆盖）；传输错误细分 `Unreachable/Timeout/Malformed`（Health 对外保持 `Unhealthy`）；发送回执统一 `normalizeSendReceipt` 归一化（优先 `receipt`，缺失回退旧字段，不可识别 fail-closed 为 `unknown_pending`）。

---

## 12. 定期清理机制（sub2api-cleanup）

部署主机 `/opt/sub2api` 与 Docker 镜像列表会随每次升级/构建累积，无清理机制会持续增长。当前已部署与 yiyutu-release-cleanup 同款的清理机制。

### 12.1 组件

| 组件 | 路径 | 作用 |
|---|---|---|
| 清理脚本 | `/usr/local/sbin/sub2api-clean-releases` | 清理 sub2api / xianyu-auto-reply 旧镜像、命名空间内悬空镜像、本项目孤儿卷、过期 `.env.bak-*` |
| systemd service | `/etc/systemd/system/sub2api-cleanup.service` | 一次性任务，调用脚本 |
| systemd timer | `/etc/systemd/system/sub2api-cleanup.timer` | 每日 04:00 触发（与 yiyutu 00:00 错峰） |
| 源文件（SSOT） | `deploy-config/scripts/sub2api-clean-releases` | 脚本源（部署时 `install -m 0755` 到 `/usr/local/sbin/`） |
| 源文件（SSOT） | `deploy-config/systemd/sub2api-cleanup.{service,timer}` | systemd unit 源（部署时 `install -m 0644` 到 `/etc/systemd/system/`） |

### 12.2 清理规则（KEEP_RELEASES=2）

- **sub2api 镜像**：保留 `latest` + 当前运行容器加载的 tag + 1 个最近 tag；删除其余
- **xianyu-auto-reply 镜像**：保留当前 `.env` 中 `XIANYU_WORKER_IMAGE_TAG` + 1 个最近 tag；删除其余
- **悬空镜像（Repository=`<none>` 且 Tag=`<none>` 在 sub2api / xianyu-auto-reply 命名空间）**：删除；其他项目悬挂镜像严格跳过
- **悬空卷（dangling=true 且名字以 `sub2api_` / `xianyu_` / `xianyu-worker_` 开头）**：删除；其他项目孤儿卷严格跳过
- **`.env.bak-*` 文件**（`/opt/sub2api/`）：保留最近 3 个；删除其余

### 12.3 手动执行

```bash
# dry-run（仅打印预期动作）
/usr/local/sbin/sub2api-clean-releases --dry-run

# 实跑
/usr/local/sbin/sub2api-clean-releases

# 查看下次执行时间
systemctl list-timers sub2api-cleanup.timer

# 查看最近执行日志
journalctl -u sub2api-cleanup.service -n 50 --no-pager
```

### 12.4 部署

升级时若脚本或 unit 有变更：

```bash
cd /opt/sub2api
git pull origin main
install -m 0755 deploy-config/scripts/sub2api-clean-releases /usr/local/sbin/sub2api-clean-releases
install -m 0644 deploy-config/systemd/sub2api-cleanup.service /etc/systemd/system/sub2api-cleanup.service
install -m 0644 deploy-config/systemd/sub2api-cleanup.timer /etc/systemd/system/sub2api-cleanup.timer
systemctl daemon-reload
```

> ⚠️ 该脚本严格遵守命名空间白名单，不会误删同机其他项目（newapi / nginx 等）的镜像或卷。

---

最后更新：2026-08-30
