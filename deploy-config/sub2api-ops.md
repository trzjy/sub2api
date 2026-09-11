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
13. [注册邮箱后缀黑名单（防滥用）](#13-注册邮箱后缀黑名单防滥用)
14. [账号健康熔断（分级治理）](#14-账号健康熔断分级治理)

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
| 应用服务 | 服务器本地构建镜像 `sub2api:<commit>-w`（compose 经 `.env` 的 `SUB2API_IMAGE_TAG` 引用），容器名 `sub2api` |
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
| `SUB2API_IMAGE_TAG=<commit>-w` | 应用镜像标签：服务器本地构建的 `sub2api:<commit>-w`，每次升级随构建产物更新（见第 5 节） |
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

## 5. 升级流程（服务器端构建镜像）

> 生产镜像在 `yiyutu-server` 本地构建（不推 registry），以 `<提交短哈希>-w` 打标签，compose 经 `.env` 的 `SUB2API_IMAGE_TAG` 引用。**发布前先在本地把要发布的提交推到 `origin main`**。

```bash
# 本地：提交并推送（前置）
cd /mnt/data/sub2api && git push origin main

# 1. 备份 .env（改 tag 前）
ssh yiyutu-server
cp /opt/sub2api/.env /opt/sub2api/.env.bak-$(date +%Y%m%d-%H%M%S)

# 2. 获取目标提交，生成干净构建暂存
#    git archive 只含该提交的树，不带工作区未提交改动/杂物；不要直接从
#    工作区 rsync（会把未提交的本地改动带进生产镜像）。
cd /opt/sub2api && git fetch origin
TARGET=$(git rev-parse --short origin/main)          # 或显式指定提交哈希
rm -rf /opt/sub2api/build-$TARGET && mkdir -p /opt/sub2api/build-$TARGET
git archive origin/main | tar -x -C /opt/sub2api/build-$TARGET
# （可选）校验暂存确含目标内容：grep -c "特征串" build-$TARGET/backend/...

# 3. 服务器端构建镜像（后台运行 + 日志，构建约 5-10 分钟）
cd /opt/sub2api/build-$TARGET && nohup docker build -t sub2api:$TARGET-w \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  --build-arg GOSUMDB=sum.golang.google.cn \
  -f Dockerfile . > /tmp/build-$TARGET.log 2>&1 &
# 轮询完成：ps -p <PID> 退出即结束；tail -f /tmp/build-$TARGET.log 查看进度

# 4. 切换镜像标签并重建容器
sed -i "s/^SUB2API_IMAGE_TAG=.*/SUB2API_IMAGE_TAG=$TARGET-w/" /opt/sub2api/.env
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d sub2api

# 5. 验证
sleep 18
docker inspect sub2api --format "{{.Image}}" | cut -c1-20          # 应为新构建产物 ID
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps   # Up (healthy)
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3300/health         # 200
curl -s -o /dev/null -w "%{http_code}\n" https://corealgos.com/               # 200
docker logs --tail=50 sub2api 2>&1 | grep -iE "panic|fatal" || echo NO_FATAL

# 6. 清理旧链：删除已成功部署的构建暂存 + 清理旧镜像
rm -rf /opt/sub2api/build-<上一目标>
/usr/local/sbin/sub2api-clean-releases   # 保留 latest + 当前运行 tag + 1 个最近 tag
```

> 也可在管理后台左上角"检查更新"一键升级（支持回滚），但生产主流程以上述服务器端构建为准。

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

本地构建镜像按提交哈希打标签，回滚只需切回上一已构建镜像的 tag：

```bash
# 1. 列出已构建镜像，确认目标标签（通常是上一个 <commit>-w）
docker images sub2api

# 2. 切换回目标标签并重建
sed -i "s/^SUB2API_IMAGE_TAG=.*/SUB2API_IMAGE_TAG=<上一哈希>-w/" /opt/sub2api/.env
cd /opt/sub2api && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d sub2api

# 3. 验证
sleep 15 && curl -s http://127.0.0.1:3300/health && docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps
```

> 若上一版本镜像已被 `sub2api-clean-releases` 清理，需按第 5 节从 `git archive` 旧提交重新构建。
> 若紧急停服：`docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env stop sub2api`，处理完毕后 `docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env start sub2api`。

---

## 10. SMTP 自动发信配置

应用使用 **腾讯企业邮箱（企业微信集成版）** 发送注册验证、密码重置等系统邮件。发信域为 `corealgos.com`（在 Cloudflare 托管 DNS）。SMTP 密码为腾讯企业邮箱「客户端专用密码」，存于数据库 `settings.smtp_password`，**不写入仓库或本文档**。

### 10.1 SMTP 参数（生产当前值）

| 字段 | 值 |
|------|------|
| SMTP Host | `smtp.exmail.qq.com` |
| Port | `465` |
| Encryption | 隐式 TLS（SMTPS，`smtp_use_tls=true`） |
| Username | `admin@corealgos.com` |
| From Email | `admin@corealgos.com` |
| Sender Name | `CoreAlgOS` |
| Password | 腾讯企业邮箱客户端专用密码，存于 DB `settings.smtp_password` |

> 端口 465 必须配合隐式 TLS（`smtp_use_tls=true`）。若误设为 `587`+STARTTLS 或 `smtp_use_tls=false`，SMTP AUTH 会返回 `535` 认证失败。

### 10.2 发信域 DNS 记录（Cloudflare，必须「仅 DNS」灰色云）

| 记录类型 | 名称 / 主机 | 内容 | 优先级 | 说明 |
|----------|-------------|------|--------|------|
| MX | `@` | `mxbiz1.qq.com` | `5` | 腾讯企业邮箱收信 |
| MX | `@` | `mxbiz2.qq.com` | `10` | 腾讯企业邮箱收信 |
| TXT | `@` | `v=spf1 include:spf.mail.qq.com ~all` | — | SPF |
| TXT | `musl2609._domainkey` | 腾讯后台提供的 DKIM 公钥记录 | — | DKIM 选择器 `musl2609` |
| TXT | `_dmarc` | `v=DMARC1; p=none; rua=mailto:admin@corealgos.com` | — | DMARC 报告 |

> 所有邮件相关记录务必设为 **DNS only（灰色云朵）**；若走 Cloudflare 代理（橙色云）会导致 MX/SPF/DKIM 校验失败，外部（如 Gmail）收件箱会判定为垃圾邮件或拒收。验证：`dig +short mx corealgos.com` 应返回 `mxbiz1.qq.com` / `mxbiz2.qq.com`。

### 10.3 客户端专用密码（SMTP 凭据）生成路径

1. 企业微信管理后台 → **协作 → 邮件 → 安全管理 → 客户端访问权限**：开启 IMAP/SMTP，并记录「客户端专用密码」。
2. 成员邮箱（admin@corealgos.com）的 **邮箱绑定 → 客户端专用密码** 处生成一次性密码（形如 `Ae6GpaubaYmhZNAc`，空格仅为显示）。
3. 将该密码填入 sub2api 管理后台「系统设置 → SMTP」的 Password 字段并保存；等价于 `UPDATE settings SET value='<专用密码>' WHERE key='smtp_password';`。

> 该密码是 SMTP 登录凭据，**切勿提交到 Git 或在文档中明文记录**。轮换时只需重新生成并覆盖 `smtp_password`。

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
| `XIANYU_WORKER_IMAGE_TAG` | Worker 镜像固定 tag（禁止 latest/reviewed 漂浮标签）。**必填**：镜像在部署主机直接构建（本机构建无 registry RepoDigest，`@sha256` digest 引用无法解析，故用固定 tag 引用）。旧 `sha256:8343c385...46d5` 已废弃（不含 launcher / `/api/v1/internal/*` / delivery-results 回传）。部署后通过 `docker inspect <容器> --format '{{.Image}}'` 校验运行容器镜像 ID 与构建产物一致（见 11.4）。**当前生产值**（2026-09-11，重建自 origin/main `dd8c9e9a3`：纳入 `fetch_items_task.py` 全量轮调整与 `auto_delivery_handler.py` 改动；低频全量轮行为见 11.4）：`deliveryfs-20260911-0641`（构建镜像 ID `sha256:3570428aee984353fd5c740660e236eb879697b5f9fb1ea4e7108fb5add933df`）。上一版 `fullsync2-20260911-0452`（构建镜像 ID `sha256:b5d0efdfab406a7d292e21bcc369a055cff453182227df1297908754a14ddaae`）及中间版 `fullsync-20260911-0431`、`qtyfallback-20260909-0119`、`globaltmpl-20260909-0049` 已由本版本替代。 |
| `XIANYU_WORKER_FULL_SYNC_INTERVAL_SECONDS` | 可选。定时商品同步的全量轮间隔（秒），经 compose 注入为容器内 `FETCH_ITEMS_FULL_SYNC_INTERVAL_SECONDS`；低频完整翻页触发下架清理，`0`/负数=禁用全量轮（纯增量旧行为）。缺省 `86400`（每天一次；进程重启后首轮即全量） |
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
- **digest 纪律**：旧字段 `XIANYU_WORKER_IMAGE_DIGEST` 已废弃，运行时统一使用 11.2 表格中的 `XIANYU_WORKER_IMAGE_TAG`（本机构建无 registry RepoDigest，固定 tag 引用更可靠）。当前已用 tag 模式替代：表格已记录 `prod-fix34-ensure-20260830-203928` 对应构建镜像 ID `sha256:10e96f9f54638945e131b04e1b969d33865b3de0b8e1f72f1863f3941fa3d640`，升级镜像时只改 tag 与同步该 tag 对应的镜像 ID。部署主机 `/opt/sub2api/xianyu-auto-reply-src/` 是运行时同步副本。
- **internal 服务间鉴权**：backend-web→websocket/scheduler 的 `/internal/*` 路由要求 `X-Internal-Token` 匹配 `SUB2API_INTERNAL_TOKEN`（空配置失败关闭）；backend-web/scheduler 的 http_client 对 internal 服务 URL 自动注入该头。
- **商品同步全量轮（2026-09-11 起）**：定时任务 `fetch_items` 默认增量（整页已存在提前停止，控风控请求量）；`scheduler/app/services/scheduler/fetch_items_task.py` 内置低频全量轮——默认每 86400 秒（env `FETCH_ITEMS_FULL_SYNC_INTERVAL_SECONDS`，compose 变量 `XIANYU_WORKER_FULL_SYNC_INTERVAL_SECONDS`，0=禁用）跑一次完整翻页，自然结束后以闲鱼「在售」列表为权威集合清理 `xy_catalog_items` 中已售罄/下架的投影行，主程序下次同步（≤5 分钟）随之删除商品行。修复背景：增量提前停止使 `ItemService._prune_stale_catalog_items` 永不执行，部分下架商品永久滞留在售面板。执行互斥（`asyncio.Lock`）防手动触发与定时循环并发双开全量；全量轮"至少一个账号成功"才标记完成，全失败下一周期重试。注意：Worker 侧商品「删除」按钮只删投影行，闲鱼侧仍在售会被下轮同步重新拉回。

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
git fetch origin && git reset --hard origin/main   # 与第 5 节部署规范一致（工作区只作部署源）
install -m 0755 deploy-config/scripts/sub2api-clean-releases /usr/local/sbin/sub2api-clean-releases
install -m 0644 deploy-config/systemd/sub2api-cleanup.service /etc/systemd/system/sub2api-cleanup.service
install -m 0644 deploy-config/systemd/sub2api-cleanup.timer /etc/systemd/system/sub2api-cleanup.timer
systemctl daemon-reload
```

> ⚠️ 该脚本严格遵守命名空间白名单，不会误删同机其他项目（newapi / nginx 等）的镜像或卷。

---

## 13. 注册邮箱后缀黑名单（防滥用）

为拦截使用一次性/ disposable 邮箱注册的滥用账号，系统在注册、发送验证码、OAuth/绑定等所有注册入口统一校验 **注册邮箱后缀黑名单** `registration_email_suffix_blacklist`。命中即拒绝（`EMAIL_SUFFIX_BLOCKED`）。

### 13.1 设置键与格式

| 项目 | 值 |
|------|------|
| 设置键 | `registration_email_suffix_blacklist` |
| 类型 | JSON 字符串，内容为字符串数组 `[...]` |
| 默认 | `[]`（空数组 = 不拦截任何后缀，等价于放行全部） |
| 匹配字段 | 与白名单 `registration_email_suffix_whitelist` 共用同一解析/归一化逻辑 |

数组内每条规则的格式（大小写不敏感，自动小写、去空格、去重）：

- **精确后缀**：以 `@` 开头，如 `"@mailinator.com"` —— 仅命中该域名本身。
- **通配后缀**：以 `*..` 开头，如 `"*.yopmail.com"` —— 命中该域名及其所有子域名（`yopmail.com`、`a.yopmail.com` 均命中）。

> 解析/保存时对非法条目严格报错（`INVALID_REGISTRATION_EMAIL_SUFFIX_BLACKLIST`），例如空串、含多个 `@`、非法域名。管理后台保存时若格式错误会直接拒绝，不会静默丢弃。

### 13.2 校验位置（代码）

黑名单在所有注册相关入口的 `validateRegistrationEmailBlacklist` 中统一调用：

- `auth_service.go`：`RegisterWithVerification`、`SendVerifyCode`、`SendVerifyCodeAsync`
- `validateRegistrationEmailPolicy`（OAuth 注册 / 第三方账号绑定路径的入口，最先校验）

纯匹配逻辑为 `registration_email_suffix_blacklist.go` 的 `IsRegistrationEmailSuffixBlocked(email, blacklist)`（单测 `registration_email_suffix_blacklist_test.go` 覆盖精确/通配/空/畸形用例）。

### 13.3 生产当前名单

生产库已预置 **79 条**一次性邮箱域名（mailinator、guerrillamail、yopmail、tempmail、nada、dispostable、trashmail、10minutemail 等）。查看：

```bash
docker exec -i sub2api-postgres psql -U sub2api -d sub2api \
  -c "SELECT json_array_length(value) FROM settings WHERE key='registration_email_suffix_blacklist';"
```

### 13.4 运维操作

**新增/修改黑名单（管理后台「系统设置 → 注册邮箱后缀黑名单」填入 JSON 数组后保存）**，或直写数据库（注意合法 JSON 且条目格式正确）：

```bash
docker exec -i sub2api-postgres psql -U sub2api -d sub2api -c " \
  UPDATE settings SET value='[\"@mailinator.com\",\"*.yopmail.com\"]' \
  WHERE key='registration_email_suffix_blacklist';"
```

> 黑名单只拦「一次性域名」。**主流邮箱的 plus 地址（如 `user+tag@gmail.com`）不会被拦截**——Gmail 的 `+tag` 仍属 `@gmail.com` 主域。若需限制 plus 地址或限定每主域注册数，应配合「邮箱域名注册额度」（`registration_email_domain_quota_enabled`）或白名单使用，而非黑名单。

---

## 14. 账号健康熔断（分级治理）

> 背景：生产事故 `corealgos.com` 2026-09-11 18:35–18:51，`openai` 分组内某 apikey 账号（流中失败、无法换号 failover、不触发 429 限流标记）持续返回 429/503，16 分钟内 10+ 次失败，调度器却始终视其健康。本机制即为此类「SSE 流已建立后的上游失败」提供分级治理。

账号健康熔断针对 **OpenAI 兼容平台的 APIKey 账号**，对窗口内可归因于该账号的上游失败（429/5xx，排除凭证失败、请求级瞬态、同账号可重试、provider 级过载）做分级处理。**默认关闭**。

### 14.1 参数含义

| 参数 | 含义 | 边界 / 默认 |
|------|------|-------------|
| `enabled` | 是否启用熔断 | 默认 `false`（关闭） |
| `window_minutes` | 滑动窗口长度（分钟），仅统计窗口内的失败 | 1–60，默认 2 |
| `failure_threshold` | L3 熔断阈值（窗口内累计失败次数） | 1–10000，默认 10 |
| `cooldown_minutes` | 熔断后临时禁用时长（分钟），到期自动恢复 | 1–60，默认 5 |
| `scope_platforms` | 覆盖的 OpenAI 兼容平台集合 | 默认 `openai, deepseek, kimi, zhipu, minimax, other` |

> **范围匹配是显式白名单**：判定时只接受 `openai/deepseek/kimi/zhipu/minimax/other`（grok 由 `include_grok` 单独控制），**不会**像 `NormalizeOpenAICompatiblePlatform` 那样把未知平台（anthropic/claude/bedrock/gemini/空串/拼写错误）兜底归为 `openai`。因此非 OpenAI 账号永远不会被纳入熔断范围，即使将来把观测入口接到共享失败路径也不会误纳。
| `include_grok` | 是否纳入 grok（其媒体生成有独立判定） | 默认 `false` |
| `watch_ratio` | L1 关注比例，触发次数 = `⌊failure_threshold × watch_ratio⌋`（最小 1） | 0.01–0.99，默认 0.4 |
| `warning_ratio` | L2 预警比例，触发次数 = `⌊failure_threshold × warning_ratio⌋`（最小 1，且 > watch） | 0.01–0.99，默认 0.7 |
| `probe.enabled` | 冷却期内定时探测、确认健康提前解除（Phase C） | 默认 `false` |
| `probe.interval_seconds` | 探测间隔（秒） | 30–600，默认 60 |
| `probe.max_attempts` | 最大探测次数，达上限停止探测、退回到期恢复 | 1–100，默认 10 |

### 14.2 三档位语义

- **L1 关注（watch）**：窗口内失败数 ≥ `watch` 阈值 → 仅结构化日志（`openai.apikey_health_watch`，含 account_id / count / threshold / window）+ 可观测指标，**不影响调度**。
- **L2 预警（warning）**：≥ `warning` 阈值 → 日志 + 写入一条可查询的运维告警（`P1`，经 `OpsRepository.CreateAlertEvent`），**不影响调度**。
- **L3 熔断（trip）**：≥ `failure_threshold` → 复用既有 `SetTempUnschedulable` 临时禁用该账号并进入冷却（与现有行为完全一致），`TempUnschedState` 中记录 `tier=3`、`matched_keyword=openai_apikey_health_breaker`。

**去抖与状态**：同一窗口周期内档位只升不降；L1/L2 在同一窗口内重复触发不重复告警（Redis companion key 记录已升级到的最高档）。**失败计数与档位状态按时间自然衰减（窗口 TTL），成功请求不会重置窗口、也不产生 Redis 写**——这正是为慢性抖动渠道保留证据：`ObserveOpenAIAPIKeyHealthSuccess` 现为 no-op（一次成功调度结果不再清零窗口）。L3 熔断后窗口同样不主动清空，冷却到期恢复后从新的窗口周期重新累计。

### 14.3 推荐值与生产开启步骤

默认阈值（10 次 / 2 分钟）对慢性故障不敏感。生产推荐（对应事故场景：~18:39 进入 L2、~18:39–18:44 进入 L3）：

```json
{
  "enabled": true,
  "window_minutes": 5,
  "failure_threshold": 4,
  "cooldown_minutes": 5,
  "watch_ratio": 0.4,
  "warning_ratio": 0.7,
  "scope_platforms": ["openai", "deepseek", "kimi", "zhipu", "minimax", "other"],
  "include_grok": false,
  "probe": { "enabled": false, "interval_seconds": 60, "max_attempts": 10 }
}
```

**写入方式（任选其一）**：

- 管理后台：「系统设置 → 账号健康熔断（分级治理）」卡片，填写后保存（前端会回显同样结构）。
- 直写数据库（设置键 `openai_apikey_health_breaker_settings`）：

```bash
docker exec -i sub2api-postgres psql -U sub2api -d sub2api -c " \
  INSERT INTO settings (key, value) VALUES ('openai_apikey_health_breaker_settings', '{\"enabled\":true,\"window_minutes\":5,\"failure_threshold\":4,\"cooldown_minutes\":5,\"watch_ratio\":0.4,\"warning_ratio\":0.7,\"scope_platforms\":[\"openai\",\"deepseek\",\"kimi\",\"zhipu\",\"minimax\",\"other\"],\"include_grok\":false,\"probe\":{\"enabled\":false,\"interval_seconds\":60,\"max_attempts\":10}}') \
  ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;"
```

> 写入后 30 秒内进程内缓存自动失效，下一次 `Observe` 即生效，无需重启。

### 14.4 与其他排除/冷却机制的关系与优先级

| 机制 | 触发条件 | 作用 | 与本机制的关系 |
|------|----------|------|----------------|
| **账号健康熔断（本文）** | 窗口内可归因的 429/5xx 累计 | L3 临时禁用账号（冷却到期自动恢复；可选探测提前恢复） | 专治「流中失败 / 不触发 429 标记」的慢性坏账号 |
| **429 限流标记**（`rate_limited_at` + 上游 `reset`） | 上游显式 429 且带 reset 时间 | 按上游 reset 时间禁用 | 两者独立写入，可能**同时**禁用同一账号；恢复条件不同（429 看 reset 时间，本机制看 cooldown）。不冲突，是叠加防护 |
| **过载冷却**（`overload_until`，529） | 上游 529（provider 级过载） | 按策略暂停调度 | 529 被本机制 `classify` 显式排除（属 provider 级，不应归因到单一账号），因此**不会**误触发本熔断 |
| **每账号关键词规则**（`matchTempUnschedulableRules`，状态码 + 响应关键词） | 响应匹配关键词 | 按规则临时禁用 | 与本机制使用同一 `TempUnschedState` 体系与运行期 blocker；`matched_keyword` 不同，互不覆盖。关键词规则可更早（单请求即）禁用；本机制是累计型兜底 |
| **渠道监控 v2**（Channel Monitor v2） | 渠道/模型级成功率、延迟 | 渠道级禁用/降权 | 是**渠道维度**，本机制是**账号维度**；账号被本机制禁用时，其所属渠道不一定被渠道监控禁用，二者维度不同、互补 |

**优先级结论**：

1. `classifyOpenAIAPIHealthFailure` 已把「凭证失败 / 请求级瞬态 / 同账号可重试 / provider 级过载（529）/ 客户端错误（4xx 非 429）」排除在计数之外，所以**不会**与 429 限流或过载冷却争夺同一事件，也不会重复禁用。
2. 当上游返回 429（无 reset）或 5xx（非 provider 级过载）时，本机制与「每账号关键词规则」可能都动作。关键词规则是「单请求命中即禁用」，更激进；本机制是「窗口累计达阈值才禁用」，更平滑。两者 `matched_keyword` 不同，后写入者不会清除前者的 reason（均经 `SetTempUnschedulable` 的「仅当更晚到期才覆盖」语义）；恢复时分别对应各自的到期/清除路径。
3. 本机制**关闭时行为与未启用前完全一致**（无 Redis 计数、无日志、不触碰调度），可安全默认关闭、灰度开启。

### 14.5 探测式恢复（Phase C，默认关闭）

启用 `probe.enabled=true` 后，处于冷却期的账号会按 `probe.interval_seconds` 定时发送一次**廉价真实转发请求**（优先 `/models` 零消耗端点，否则 1 token 的最小补全），复用既有转发链路与 SSRF 防护：

- 探测成功（2xx）→ 立即 `ClearTempUnschedulable` 提前解除，写 `openai.apikey_health_probe_recovered` 日志；
- 探测失败 → 维持冷却，并将尝试次数 +1（写回 reason 的 `probe_attempts`）；
- 尝试次数达到 `probe.max_attempts` → 停止探测，退回「到期自动恢复」；
- 某平台无安全/廉价探测端点 → 探测降级为「到期恢复」，代码注释与本节已说明，不会刷屏。

> **开关热生效（运行时无需重启）**：探测循环在进程启动时即常驻，每个 tick（≤ `probe.interval_seconds`，默认 60s）都会重新读取 `probe.enabled`，因此**在管理后台开启/关闭探测无需重启进程**，最长一个探测间隔内即生效。关闭后循环仍在运行但每个 tick 直接跳过、不发起任何探测请求。进程优雅关闭时会调用 `Stop()` 终止该循环，不会泄漏 goroutine。

#### 探测的局限（开启前须知）

- **`/models` 与聊天上游可能不同路**：部分中转/代理站点 `/models` 由轻量前端返回，并不真正打到重上游；此时 `/models` 返回 2xx 但 chat 仍坏，探测会**过早解除**冷却。该局限默认由「探测关闭」兜底；这类站点应保持 `probe.enabled=false`，或仅在 `/models` 与 chat 共用同一上游时开启。
- **多实例重复探测（幂等无害）**：每个后端副本各自独立运行探测扫描，同一被冷却账号可能被多个实例同时探测。探测对上游是只读的，其副作用（成功则 `ClearTempUnschedulable`、失败则 `probe_attempts` +1）均为幂等，重复探测除略微增加上游流量外无副作用。

> 探测请求日志与错误信息中**不得泄漏密钥**；探测默认关闭，仅管理员显式开启。

---

最后更新：2026-09-11
