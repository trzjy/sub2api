# Sub2API 运维部署指南 (Operations & Deployment Guide)

> **本文件是 Sub2API 唯一运维部署文档**（SSOT）。其他历史部署文档（如 `docs/DEPLOY.md`）已废弃删除，内容已并入本文。
>
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

**安全密钥（禁止改）**：

| 变量 | 说明 |
|------|------|
| `JWT_SECRET` | JWT 签名密钥，修改会使所有活跃会话失效 |
| `TOTP_ENCRYPTION_KEY` | TOTP 加密密钥，修改会使所有 2FA 失效 |
| `CRED_ENCRYPTION_KEY` | 账号凭证静态加密主密钥（AES-256-GCM，32 字节 base64，`openssl rand -base64 32` 生成）。可选：留空则凭证明文存储（向后兼容）并输出启动警告；格式非法时启动 fail-fast |
| `CRED_ENCRYPTION_KEY_OLD` | 轮换前的旧密钥（仅解密用，可选）。轮换步骤：新密钥写入 `CRED_ENCRYPTION_KEY`、旧密钥放这里，重启并完成存量数据重加密后移除本变量。密文值带 `enc:v1:` 前缀，读路径凭前缀自动识别加密形态 |

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

#### Windows 工作站 / 双系统说明（实测 2026-09-21）

- 该 SSH 密钥**同时部署在 Windows 侧**：`C:\Users\trzjy\.ssh\yiyutu_root_ed25519`（公钥 `*.pub`），且 `C:\Users\trzjy\.ssh\config` 已配好同样的 `Host yiyutu-server` 别名（指向 `43.250.173.110`，`User root`，`IdentityFile ~/.ssh/yiyutu_root_ed25519`，`IdentitiesOnly yes`）。与 Linux 侧为**同一把私钥的副本**，登录方式完全一致。
- Windows OpenSSH 对私钥 ACL 有要求，若遇 "Permissions for ... are too open" 拒绝，收紧权限：
  ```powershell
  icacls yiyutu_root_ed25519 /inheritance:r /grant:r "trzjy:R"
  ```
- **⚠️ 代理白名单（关键坑）**：本机 Clash 等代理软件会劫持发往服务器的流量，导致 SSH 握手在 `kex_exchange_identification` 阶段被远端秒关（`Connection closed by remote host`）、HTTPS 访问 `corealgos.com` 也异常。**必须将服务器加入代理直连白名单**：
  - Clash Verge：在**当前激活 profile 的「Rules 增强」**（`prepend` 段，最高优先级）加入：
    ```yaml
    prepend:
      - IP-CIDR,43.250.173.110/32,DIRECT
      - IP-CIDR,43.250.173.198/32,DIRECT
      - DOMAIN-SUFFIX,corealgos.com,DIRECT
    ```
  - **两条 IP-CIDR 都必须保留**：`.110` 是本站服务器（SSH 入口），`.198` 是同网段另一台服务器（承载 `cnca.one`，属另一部署项目；此条继承自历史 profile，切 profile 时容易漏带）。域名规则只覆盖 HTTPS 域名访问，直连 IP 的流量（SSH、IP 直连 API）必须靠 IP-CIDR 规则。
  - **2026-09-21 实测坑**：域名 DIRECT 规则生效时仍可能出现 502 —— `corealgos.com` 挂在 Cloudflare 后面，DIRECT 拨号的目标是 CF 边缘 IP；本机运营商到 CF 网段抖动时 IPv4/IPv6 全部 `i/o timeout`，mihomo 匹配到 DIRECT 也拨不通，代理层向上层返回 502。这**不是规则问题**，是本机→Cloudflare 链路问题；验证方法：看 mihomo core 日志里 `dial DIRECT (match DomainSuffix/corealgos.com) ... error: i/o timeout` 字样，或指定 CF IP 直拨 `curl --resolve corealgos.com:443:104.21.33.161 https://corealgos.com/`。
  - Linux 本机当前使用 mihomo-party（配置目录 `~/.config/mihomo-party/`），规则增强文件在 `rules/<当前profile-id>.yaml`（当前 profile id 见 `profile.yaml` 的 `current:` 字段）；改完规则文件后可在 UI 里重新应用 profile，或直接改 `work/config.yaml` 后通过 unix socket 热重载：
    ```bash
    curl -s --unix-socket /tmp/mihomo-party-1000-*.sock -X PUT "http://localhost/configs?force=true" \
      -H 'Content-Type: application/json' -d '{"path": "'"$HOME"'/.config/mihomo-party/work/config.yaml"}' -w '%{http_code}\n'
    ```
  - 改完需**重新应用该 profile / 重启 Clash** 让 `clash-verge.yaml` 重新生成并热重载，否则规则不生效。
  - 验证：`ssh yiyutu-server "echo SSH_OK"` 能回显即说明白名单生效；`curl -s https://corealgos.com/` 返回正常页面即域名直连生效。
- 双系统共用同一把密钥，改了一侧另一侧为同步副本；不要在两侧各自生成不同密钥，否则一侧失效。

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

# 6. post-deploy 清理钩子（健康检查通过后立即执行）
#    镜像 keep-2 实时清 + build cache --max-used-space 5GB + >8GB 硬兜底 + build staging 即清
#    ——清理实时化，不等每日 timer（2026-09-26 磁盘守卫整改，见 §12.5）
rm -rf /opt/sub2api/build-<上一目标>
/usr/local/sbin/sub2api-clean-releases   # 保留 latest + 当前运行 tag + 1 个最近 tag
```

> 也可在管理后台左上角"检查更新"一键升级（支持回滚），但生产主流程以上述服务器端构建为准。

---

## 5.x 公告图片孤儿对象清理（2026-09-23 新增，随公告图片上传功能交付）

> 来源：docs/announcement-image-upload-plan.md §6.2。公告图片以 `announcements/<公告ID>/` 前缀
> 存于图片存储桶（顶层命名空间，不含图片存储 prefix 设置）。上传后放弃保存、占位草稿保留、
> 删除时对象删除失败，都会留孤儿对象。本步骤不进应用代码，运维手动/周期执行。
>
> **覆盖范围声明（R3-8）**：本流程**只清理公告行已不存在的前缀**（如删除级联失败残留、
> 占位草稿被删但对象残留）。**存活公告前缀下未被 Markdown 引用的对象**（上传后未保存、
> 保留的占位草稿中未引用者）本流程无法识别——方案本期不解析公告 content，这些对象只能
> 随该公告删除时级联回收，或确知无引用后手动清理。此为已记录的剩余风险。

```bash
# 1. 列出桶内公告图片前缀（对象存储 CLI 以 aws s3 为例，alias/凭证按部署实际）
#    只保留纯数字 ID（R3-7），统一 LC_ALL=C sort -u 词法排序
aws s3 ls s3://<图片桶>/announcements/ | awk '{print $2}' | tr -d '/' \
  | grep -E '^[0-9]+$' | LC_ALL=C sort -u > /tmp/ann-ids.txt

# 2. 空列表短路：桶内没有公告图片前缀时直接结束
[ -s /tmp/ann-ids.txt ] || { echo "no announcement prefixes; nothing to do"; exit 0; }

# 3. 对照 announcements 表（容器内 psql）
docker exec sub2api-postgres psql -U sub2api -d sub2api -tAc   "SELECT id FROM announcements WHERE id IN ($(paste -sd, /tmp/ann-ids.txt))" \
  | grep -E '^[0-9]+$' | LC_ALL=C sort -u > /tmp/ann-live.txt

# 4. 求差集（孤儿 id；两侧同一词法排序，comm 才可靠），dry-run 列出将被删除的前缀
comm -23 /tmp/ann-ids.txt /tmp/ann-live.txt > /tmp/ann-orphan.txt
while read id; do echo "would delete: s3://<图片桶>/announcements/$id/"; done < /tmp/ann-orphan.txt

# 5. 确认无误后实删（逐前缀）
while read id; do aws s3 rm "s3://<图片桶>/announcements/$id/" --recursive; done < /tmp/ann-orphan.txt
```

- **频率**：建议月度例行，或大批量删除公告后手动执行一次。
- **权限**：对象存储列举+删除凭证（备份 S3 凭证同源时可直接复用）；数据库只读。
- **留痕**：dry-run 输出与实删输出保存到 `/home/zjy/.sub2api-acceptance/`（脱敏）。
- **注意**：换桶/禁用图片存储受配置守卫限制（存在公告图片对象时后台会拒绝），如确需换桶，
  先用本步骤清空公告图片对象。
- **部署硬约束（R4-3）**：公告图片守卫互斥与 resolver 缓存为单进程语义，**后端必须单副本部署**
  （当前生产即单容器）。引入多副本部署前，必须先把 resolver 缓存与守卫升级为跨实例协调。

---

## 5.y 公告图片 MinIO 对象存储（2026-09-23 新增，随公告图片配置完善交付）

> 公告图片的存储后端为服务器自建 MinIO（2026-09-23 用户裁定，生产无现成 S3 凭证）。
> 组件：compose 服务 `minio`（quay.io，固定 release tag；docker.io 在部署主机不可达），
> 数据目录 `/opt/sub2api/minio_data`，桶 `sub2api-media`。

**拓扑**：

- 应用内网：sub2api 容器 → `http://minio:9000`（force_path_style，region us-east-1）
- 公开下载：`https://corealgos.com/s3/<key>` → nginx（`/etc/nginx/sites-available/duizhang`
  的 `location ^~ /s3/`）→ `127.0.0.1:9001` 的 `sub2api-media` 桶；桶匿名只读策略**仅限**
  `announcements/` 前缀（`mc anonymous set download local/sub2api-media/announcements`）
- 后台配置：管理后台 → 备份与数据管理 → 对象存储，`image_storage_config` 设置项
  （enabled=true, endpoint=http://minio:9000, bucket=sub2api-media,
  public_base_url=https://corealgos.com/s3, force_path_style=true）

**运维要点**：

- 凭证：`.env` 的 `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`（与 POSTGRES_PASSWORD 同等级敏感）。
  轮换时改 .env → `docker compose up -d minio` → 同步更新后台 `image_storage_config` 的密钥。
- 备份：`/opt/sub2api/minio_data` 在 §6.1 的整体打包范围内（tar 含公告图片对象）。
- nginx 配置以服务器 `/etc/nginx/sites-available/duizhang` 为准（仓库无副本），
  变更需 `nginx -t` 后 reload。
- 5.x 孤儿清理脚本中 `<图片桶>` = `sub2api-media`，可用 minio 容器内 `mc` 执行：
  `docker exec sub2api-minio mc ls --recursive local/sub2api-media/announcements/`。

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

### 7.6 登录页提示 "aliyun captcha verification failed"

阿里云验证码验证失败（前端发了 token 但阿里云拒绝）。

```bash
# 检查当前 aliyun captcha 启用状态
docker exec sub2api-postgres psql -U sub2api -d sub2api \
  -c "SELECT key, value FROM settings WHERE key LIKE 'aliyun_captcha%';"
```

**恢复登录（临时关闭阿里云验证码）**：

```bash
docker exec sub2api-postgres psql -U sub2api -d sub2api \
  -c "UPDATE settings SET value='false' WHERE key='aliyun_captcha_enabled';"
```

确认用户可登录后，再重新配置阿里云验证码。

### 7.7 前端根路径返回 404

原因：Go 二进制未用 `-tags=embed` 编译，前端 SPA 未打包进镜像。

```bash
# 检查容器镜像是否包含前端资源
docker run --rm sub2api:<tag> ls /app/ | grep dist
```

修复：确保构建命令包含 `-tags=embed`（服务器端构建流程默认已含，见第 5 节），必要时回滚到之前用 embed 编译的镜像。

### 7.8 健康检查超时 / 容器启动慢

```bash
# 查看容器启动日志
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env logs --tail=50 sub2api

# 手动进入容器排查
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env exec sub2api sh

# 检查数据库迁移：容器首次启动可能需执行 AUTO_SETUP 和迁移，耗时长属正常
```

### 7.9 回滚失败（找不到备份）

```bash
# 检查备份列表
ls -1 /opt/sub2api/.env.bak-*

# 手动恢复
cp /opt/sub2api/.env.bak-<timestamp> /opt/sub2api/.env
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d --force-recreate sub2api
```

### 7.10 磁盘打满（2026-09-26 事故 runbook）

> 背景：2026-09-26 凌晨根分区 100% 打满（build cache 11.8GB + 镜像窗口期累积），
> 全站登录不可用。事后整改见 docs/disk-guard-plan-20260926.md 与 §12.5。

**症状**（可能同时或部分出现）：

- 全站登录不可用（postgres 无法写 postmaster.pid 循环重启）
- `docker ps` 中 `sub2api-postgres` 状态为 `Restarting` 循环
- 容器/系统日志出现 `No space left on device`
- **误导项**：`sub2api` 主容器可能仍显示 `Up (healthy)`（健康检查不落盘时探活可假通过），不要据此排除磁盘问题

**处置**（按序）：

```bash
# 1. 确认磁盘水位
df -h /

# 2. 定位大头
docker system df            # 镜像/容器/卷/build cache 分类占用
du -xsh /* 2>/dev/null | sort -rh | head -20

# 3. 释放空间（按大头选择；这三个命令对本项目与业务数据安全）
docker builder prune -af                      # build cache 全清（事故主凶）
docker image prune -af                        # 悬空+未使用镜像（运行中镜像不受影响）
journalctl --vacuum-size=200M                 # journald 压到 200M

# 4. 关键：手动重启 postgres —— 磁盘恢复后 postgres 不会自愈，必须手动 restart
docker restart sub2api-postgres

# 5. 验证恢复
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3300/health   # 200
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps   # 全部 Up (healthy)
# 再走一遍登录确认（生产登录有 Turnstile，见 §7.6/15 节相关说明）
```

**归因**：恢复后用 `du -xsh /*` 与 `docker system df` 找出打满根因（本次事故 =
build cache 11.8GB + 每日清理降级路径 `until=168h` 永不命中 + 镜像窗口期累积），
对应整改：post-deploy 清理钩子实时化（§12.5）+ build cache 两级封顶（§12.2）。

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
| `XIANYU_WORKER_IMAGE_TAG` | Worker 镜像固定 tag（禁止 latest/reviewed 漂浮标签）。**必填**：镜像在部署主机直接构建（本机构建无 registry RepoDigest，`@sha256` digest 引用无法解析，故用固定 tag 引用）。旧 `sha256:8343c385...46d5` 已废弃（不含 launcher / `/api/v1/internal/*` / delivery-results 回传）。部署后通过 `docker inspect <容器> --format '{{.Image}}'` 校验运行容器镜像 ID 与构建产物一致（见 11.4）。**当前生产值**（2026-09-17，主程序 `5748f4b16`：web 登录代理 JS 写入型 Cookie 捕获修复——ChatGLM 登录 Token 由官方前端 `Cookies.set` 写入（chatglm_token/chatglm_refresh_token/chatglm_user_id），服务端不下发 Set-Cookie；现从入站 Cookie 头按平台白名单提取捕获并仅转发白名单字段，`wlp_session` 与管理站 cookie（sub2api_session 等）全程排除；上一版 `9654dee8a`：root-path 根路径全代理 + wlp_session 会话 Cookie；再上一版 `f142b9ac5`：web 登录代理 CSP frame-src 注入修复（WEB_LOGIN_PROXY_ORIGIN 进主站 CSP，修 Chrome 拦截内嵌 iframe）+ WebLoginModal platform i18n 键 kebab→camel 修复；主程序 ops 修复 `ab69bfff4`：Gemini Drive scope 403 优雅降级（不再误报 error 级告警）+ 闲鱼对账日志降频（连续失败首条 warn 后续 debug）；上一版 `ef06893c0`：web 网关流中段业务错误收口（chat 写 error 帧+[DONE]、responses/anthropic 写 event: error，不再伪成功）+ kimi 字符串 code 识别 + base_url 类型守卫；上一版 `7bc043253`：web 登录代理要求显式 WEB_LOGIN_PROXY_ORIGIN + 响应协议桥接。**Worker 镜像**）：`reconcile-expose-af9286cf0`（构建镜像 ID `sha256:3ecaa9412aa45...`；源码 af9286cf0 = 对账 + 曝光助手 + 人脸/打码增量；修复运行中 faceqr-localnotify-20260916 缺 `/orders/auto-deliveries` 路由导致对账 404 的问题；Worker 源码副本 `/opt/sub2api/xianyu-auto-reply-src/` 已同步 af9286cf0；Worker 测试 51 passed）。前序（2026-09-14，主程序 `657794af9`：每日简报「今日速读」上线（LLM 汇总当日新增+近7天有效优惠，缓存/refresh/兜底）；速读前端渲染补提交（ed0e24cc0）；自审清理。前序 `82a73afef`：连通性测试失败显示真实原因（模型白名单拒绝附可用清单/端点不可达），不再兜底成 internal error；全链路管理端 API 验收通过——正路径 kimi-k3 parsed_offers=1、负路径人话提示、简报 205 条结构化情报（97 高相关）。前序 `b331522fe`：优惠情报 key 选择器只列管理员本人 key 并带分组/可用模型清单（修复全站用户 key 污染下拉 + 模型盲配 404）；整理模型已配置 self 模式 kimi key + kimi-k3。前序 `ea6045e3e`：整理模型自选本系统中转网关——管理台选已有管理员 API Key + 模型即可，支持 OpenAI/Anthropic 双协议，零外部配置；自定义端点降级为高级选项。同日 03:00 首版 `1070f7b39`：优惠情报中心上线——迁移 247 新增 promo_intel_sources/promo_intel_items，37 个实测验证的厂商官方资讯源每日轮询，独立可配置 LLM 结构化整理（管理台「优惠情报→整理模型」配置，未配置时降级原文待整理、30 分钟补跑通道自动结构化），管理台按日简报；含三个上线热修：到期扫描 SQL 显式 ::timestamptz、原文摘录 rune 截断 + NUL 剔除、补跑通道。同日的前序热修链：`b2bfa9353`（编码修复）→ `0ffb37b5e`（SQL 类型修复）→ `597c0c0ab`（功能主体））。镜像 `sub2api:657794af9-w`（构建镜像 ID `sha256:bf380594d2eb160696cb8919d25902864354f24bdbac7d01f514291296bbf203`；前序 `82a73afef-w` / `sha256:6acc1bce02dd...`；前序 `b331522fe-w` / `sha256:b14bee0fb705e19e7defc13364d367808d21ad470125d624845f5b0ddc4808cd`；前序 `ea6045e3e-w` / `sha256:4d4faba49a3b5811...`、`1070f7b39-w` / `63989e0e76f728a2...`）。上一版主程序（2026-09-13，`eef9ef8ef`：闲鱼发货对账任务上线——Worker 新增 `GET /api/v1/internal/orders/auto-deliveries` 增量接口，主程序 5 分钟一轮三方对账（Worker 自动发货订单 / 卡密领取记录 / 订单镜像），漂移自动补平+邮件告警，首轮水位线基线 `2026-09-12T16:12:32Z`，历史豁免；Worker 镜像）：`reconcile-eef9ef8ef`（构建镜像 ID `sha256:51271e673c603dc767a913a33717aab673f391a85d9c9a40f75bbbfa7a0eeafa`；主程序镜像已升级 `sub2api:8a662e838-w`（自审修复：单码作废 used 拒绝/expired 幂等、镜像终态分裂升级人工、QuantitySent 对齐回执口径）。在 `refundclaw-20260912-2208` 源码之上新增对账增量接口）。上一版主程序 `c4cf5db23`（兑换码作废功能）。再上一版（2026-09-12，在 `deliveryfs-20260911-0641` 源码之上：新账号创建默认 `auto_confirm=True` + `send_before_confirm=True`，覆盖扫码/密码/手动三条创建路径，解决新账号漏开自动发货导致订单卡 `pending_ship` 的问题）：`refundclaw-20260912-2208`（构建镜像 ID `sha256:2da6864d301876c2e7dcf8c1700afd0709135c73bbf19623aad612150d80cf5b`；在 `autoconf-20260912-0629` 源码之上新增：scheduler 退款同步尾部把「退款成功」订单批量上报主程序 `POST /api/v1/internal/xianyu/refund-events`（`common/services/sub2api_refund_event_client.py` + `report_refunded_orders_to_sub2api` 兜底扫描，`xy_orders.refund_reported` 去重，未配置 SUB2API_INTERNAL_* 静默跳过），主程序据此作废未兑换码/追回已兑换权益，堵退款白嫖漏洞）。上一版 `autoconf-20260912-0629`（构建镜像 ID `sha256:177e93f56149ab4d8be1d2adc8e7f55d24c09f18c8718a0d1c14dbef8a1160d6`）。再上一版 `deliveryfs-20260911-0641`（构建镜像 ID `sha256:3570428aee984353fd5c740660e236eb879697b5f9fb1ea4e7108fb5add933df`）、`fullsync2-20260911-0452`（构建镜像 ID `sha256:b5d0efdfab406a7d292e21bcc369a055cff453182227df1297908754a14ddaae`）及中间版 `fullsync-20260911-0431`、`qtyfallback-20260909-0119`、`globaltmpl-20260909-0049` 已由本版本替代。 |
| （历史提示）WEB_LOGIN_PROXY_* | 该版本已退役：web-login-proxy 服务于 2026-09 实施 web-platform-single-entry-login 后删除，配置项已失效。上方 `XIANYU_WORKER_IMAGE_TAG` 历史版本日志中的 `WEB_LOGIN_PROXY_ORIGIN` / `wlp_session` / ChatGLM Cookie 捕获等旧链生产值仅作历史事实留档，不再对应任何在运行的配置或代码。 |
| `XIANYU_WORKER_FULL_SYNC_INTERVAL_SECONDS` | 可选。定时商品同步的全量轮间隔（秒），经 compose 注入为容器内 `FETCH_ITEMS_FULL_SYNC_INTERVAL_SECONDS`；低频完整翻页触发下架清理，`0`/负数=禁用全量轮（纯增量旧行为）。缺省 `86400`（每天一次；进程重启后首轮即全量） |
| `XIANYU_WORKER_MYSQL_USER/PASSWORD/ROOT_PASSWORD/DB` | Worker 独立 MySQL 凭据 |

### 11.3 本地 GLM/Kimi 人工挑战入口

`deploy-config/xianyu-auto-reply-src/tools/local_captcha_helper.py` 在本地工作站提供真实可点击的有头 Chromium 挑战入口；它不恢复管理页第三方 SDK，也不接受人工手填验证码结果。

```bash
cd /mnt/data/sub2api
a=deploy-config/xianyu-auto-reply-src/tools/local_captcha_helper.py
python3 "$a" --selftest
python3 "$a"  # 默认 127.0.0.1:18089
curl -s http://127.0.0.1:18089/healthz
```

调用契约（所有非 healthz 请求均须 `X-API-Key: <config.json 中 secret>`）：

- `POST /challenge/sdk-start`：`{"platform":"glm|kimi","login_session_id":"...","phone":"...","phone_code":"86","timeout":180}`，返回一次性 `session_id`。同一时刻仅一个槽位，忙时 409；必须使用 `X-API-Key`，不接受 body secret。旧 `/challenge/start` 暂为同一路由别名。
- `GET /challenge/{session_id}/status?platform=...&login_session_id=...&phone=...`：按平台、登录会话和手机号绑定查询 `pending/running/ok/context_gap`，不匹配返回 409。
- `POST /challenge/{session_id}/result`：body 同样带 `platform/login_session_id/phone`，终态一次性消费。GLM 数美 SDK 回调给出真实 `rid` 即成功（md5 可选：2026-09-22 chatglm.cn 线上取证，官方滑块 `onSuccess` 仅回调 `{rid, pass}`，md5 是落地链接 query 可选参数，正常滑块流不带；helper 对 md5 有则透传、无则空）并附启动时 `phone_code`；Kimi 只返回真实回调 `validate`；重复消费 404。
- helper 不会 `page.goto` SDK 脚本 URL，而是在有头 Playwright 本地最小页面加载已取证官方 SDK：Kimi `initNECaptcha({captchaId,element,mode:"embed",apiVersion:2})`，GLM `initSMCaptcha({organization,product:"embed"})`。页面必须运行在真实 http 源下（`http://127.0.0.1:18089/__challenge_page__`，由 Playwright 路由拦截注入 HTML，不实际访问该路径）；`about:blank`/`data:` 等不透明源会因浏览器拒绝 `document.cookie` 而使 SDK 初始化失败（易盾 curl 直连正常但页面卡在“正在加载”）。Kimi 成功以 `onVerify(err==null)` 或隐藏输入 `NecaptchaValidate` 轮询兜底捕获 `validate`。GLM 回调字段以运行时日志记录键名（不记值）：2026-09-21/22 两次生产实测均为 `['pass', 'rid']`，无 `md5`（与官方 bundle `onSuccess` 只解构 `{rid, pass}` 一致）；`rid` 缺失或为空才关闭浏览器并失败，禁止把 `token/validate/pass` 当 md5。超时、浏览器关闭、回调缺字段同样关闭浏览器并失败。

详细背景、隧道和安全边界见 `docs/xianyu-manual-captcha-helper.md`。secret、挑战 URL、登录会话值及凭证不写日志或长期落盘；结果仅内存保存，读取后立即删除。

### 11.4 验证命令

```bash
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env ps
# Worker 9000/8089/8090/MySQL(3306)/Redis(6379) 公网访问必须全部不可达
bash deploy/tests/xianyu-deployment-boundary-test.sh
```

### 11.5 Worker 镜像构建与回传补丁

- **镜像构建**：以 `deploy-config/xianyu-auto-reply-src/backend-web/Dockerfile` 构建（该 Dockerfile 已统一 `COPY common/backend-web/websocket/scheduler/launcher`，EXPOSE 8089/8090/8091，并安装三端依赖）；`launcher/entrypoint.py` 在单容器内并行启动 backend-web(8089)/websocket(8090)/scheduler(8091)。
- **delivery-results 回传**：Worker 端 `common/services/sub2api_delivery_result_client.py` 在自动发货获得平台最终发送回执后回传 `POST {SUB2API_INTERNAL_BASE_URL}/api/v1/internal/xianyu/delivery-results`（`confirmed=true` 才标记 sent）；未配置 base_url/token 时静默跳过，不影响本地/单机模式。主程序保持 `pending` 直至收到 `confirmed=true`，否则最终转人工。
- **退款成功上报（2026-09-12 起，refundclaw-20260912-2208）**：scheduler 退款同步（`_fetch_refund_orders_impl`）尾部调用 `report_refunded_orders_to_sub2api(account_id)`，对本账号 `status='refunded'` 且 `refund_reported=0` 的订单（单轮限 20 条）上报 `POST {SUB2API_INTERNAL_BASE_URL}/api/v1/internal/xianyu/refund-events`，payload `{order_no, account_id, status:'refunded'}`，成功（2xx）才置位 `xy_orders.refund_reported`；失败/429/5xx 保留待下轮兜底重报。主程序侧单事务原子处置：`delivered/unused` 码作废(expired)，`used` 码按类型追回（订阅扣发放天数/余额扣面值/并发扣面值），`refund_handled_at` 幂等去重，重复上报回放既往 action。仅「退款成功」处置，「退款中」不动作。
- **cookie_id 透传**：Worker 卡券（发货）调用主程序 Claim 时透传 `cookie_id` 作为账号身份（`XianyuDeliveryClaimRequest.cookie_id`）。
- **digest 纪律**：旧字段 `XIANYU_WORKER_IMAGE_DIGEST` 已废弃，运行时统一使用 11.2 表格中的 `XIANYU_WORKER_IMAGE_TAG`（本机构建无 registry RepoDigest，固定 tag 引用更可靠）。当前已用 tag 模式替代：表格已记录 `prod-fix34-ensure-20260830-203928` 对应构建镜像 ID `sha256:10e96f9f54638945e131b04e1b969d33865b3de0b8e1f72f1863f3941fa3d640`，升级镜像时只改 tag 与同步该 tag 对应的镜像 ID。部署主机 `/opt/sub2api/xianyu-auto-reply-src/` 是运行时同步副本。
- **internal 服务间鉴权**：backend-web→websocket/scheduler 的 `/internal/*` 路由要求 `X-Internal-Token` 匹配 `SUB2API_INTERNAL_TOKEN`（空配置失败关闭）；backend-web/scheduler 的 http_client 对 internal 服务 URL 自动注入该头。
- **商品同步全量轮（2026-09-11 起）**：定时任务 `fetch_items` 默认增量（整页已存在提前停止，控风控请求量）；`scheduler/app/services/scheduler/fetch_items_task.py` 内置低频全量轮——默认每 86400 秒（env `FETCH_ITEMS_FULL_SYNC_INTERVAL_SECONDS`，compose 变量 `XIANYU_WORKER_FULL_SYNC_INTERVAL_SECONDS`，0=禁用）跑一次完整翻页，自然结束后以闲鱼「在售」列表为权威集合清理 `xy_catalog_items` 中已售罄/下架的投影行，主程序下次同步（≤5 分钟）随之删除商品行。修复背景：增量提前停止使 `ItemService._prune_stale_catalog_items` 永不执行，部分下架商品永久滞留在售面板。执行互斥（`asyncio.Lock`）防手动触发与定时循环并发双开全量；全量轮"至少一个账号成功"才标记完成，全失败下一周期重试。注意：Worker 侧商品「删除」按钮只删投影行，闲鱼侧仍在售会被下轮同步重新拉回。

### 11.6 基座底层重构要点（补发/发货链路）

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
| 源文件（SSOT） | `deploy-config/scripts/disk-guard-maintenance.sh` | 磁盘守卫维护窗口脚本（2026-09-26 整改项②：daemon.json log-opts + journald 上限 + docker/容器重建 + .NEXT_IMAGE_TAG 换镜像协议）。部署时 `install -m 0755` 到 `/opt/sub2api/scripts/`；**会重启 docker 与全部容器（约 1 分钟窗口），仅限维护窗口由定时器执行，业务高峰/会话存活期禁跑（DRY_RUN 除外）** |

### 12.2 清理规则（KEEP_RELEASES=2）

- **sub2api 镜像**：保留 `latest` + 当前运行容器加载的 tag + 1 个最近 tag；删除其余
- **xianyu-auto-reply 镜像**：保留当前 `.env` 中 `XIANYU_WORKER_IMAGE_TAG` + 1 个最近 tag；删除其余
- **悬空镜像（Repository=`<none>` 且 Tag=`<none>` 在 sub2api / xianyu-auto-reply 命名空间）**：删除；其他项目悬挂镜像严格跳过
- **悬空卷（dangling=true 且名字以 `sub2api_` / `xianyu_` / `xianyu-worker_` 开头）**：删除；其他项目孤儿卷严格跳过
- **`.env.bak-*` 文件**（`/opt/sub2api/`）：保留最近 3 个；删除其余
- **/tmp 下 sub2api 备份产物**（`sub2api-db-*.sql` / `sub2api-predeploy-*.sql` / `sub2api-*.tar.gz`）：保留最近 2 个；删除其余（不触碰 newapi 等其他项目备份，见脚本注释）
- **build staging 目录**（`/opt/sub2api/build-<hash>`）：保留当前运行 tag 目录 + 1 个最近；删除其余
- **Docker build cache（两级封顶，2026-09-26 磁盘守卫整改）**：
  1. LRU 裁剪：`docker builder prune -f --max-used-space 5GB`（`BUILD_CACHE_MAX_STORAGE`），超出部分按最近使用淘汰，保留近期缓存加速下次构建；
  2. 硬兜底：实际占用（`docker system df` 读取）超过 `BUILD_CACHE_HARD_CAP`（默认 8GB）时 `docker builder prune -af` 全清，打日志 `build cache hard-cap exceeded (X > 8GB), full prune`；
  - buildx 需支持 `--max-used-space`（容量封顶 flag 的实际名称，**不存在 `--max-storage`**；
    Ubuntu 24.04 打包的 0.21.3 无该 flag，脚本会静默降级 `--filter until=`——且 `|| true`
    会吞掉 unknown flag 的退出码，清理失效不可见于退出码，必须看日志 note 行），
    **2026-09-26 已升级到官方 docker-buildx-plugin v0.37.1（服务器 help 实测确认）**；
  - 背景：2026-09-26 磁盘打满事故主凶即 build cache 11.8GB 只进不出（旧降级路径 `until=168h` 永不命中）。

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

### 12.5 post-deploy 清理钩子（2026-09-26 新增）

磁盘守卫整改（docs/disk-guard-plan-20260926.md）将清理从"每日定时为主"改为"事件驱动为主、
timer 兜底"：每次部署健康检查通过后，立即执行一次清理，不等每日 04:00 timer。

- **谁调用**：§5 升级流程第 6 步（人工/半自动部署流程的固定环节），命令即
  `/usr/local/sbin/sub2api-clean-releases`；systemd timer（§12.1）保留为每日兜底，不变更。
- **何时**：部署第 5 步健康检查（`/health` 200 + 容器 healthy）通过之后、部署收尾前。
- **覆盖动作**：镜像 keep-2 实时清、build cache `--max-used-space 5GB` LRU 裁剪 + >8GB 硬兜底全清、
  build staging 目录即清、`.env.bak-*` / /tmp 备份产物滚动保留（见 §12.2）。
- **幂等性**：脚本为纯清理动作，可重复执行无副作用；保留集合（运行镜像、运行 tag 目录）
  每次动态计算，跑多次与跑一次结果一致。不确定时先 `--dry-run`。
- **配置**：阈值均可用环境变量覆盖（`KEEP_RELEASES` / `BUILD_CACHE_MAX_STORAGE` /
  `BUILD_CACHE_HARD_CAP` / `TMP_BACKUP_KEEP` / `BUILD_DIR_KEEP` 等，见脚本头部注释）。

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

> **覆盖面（2026-09-23 扩展）**：CodeBuddy 影子账号（oauth 型、`quota_dimension=codebuddy`，随母账号分发的共享号）同样纳入熔断观察——其可归因失败（429/5xx）与 apikey 账号共用同一 L1/L2/L3 窗口与停调路径；OpenAI 平台的 Spark 影子不纳入。此前影子 429 后完全不被观察（零冷却）的缺口已关闭。

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

**去抖与状态**：同一窗口周期内档位只升不降；L1/L2 在同一窗口内重复触发不重复告警（Redis companion key 记录已升级到的最高档）。**失败计数与档位状态按时间自然衰减（窗口 TTL），成功请求不会重置窗口、也不产生 Redis 写**——这正是为慢性抖动渠道保留证据：`ObserveOpenAIAPIKeyHealthSuccess` 现为 no-op（一次成功调度结果不再清零窗口）。达到 L3 时，Lua 脚本会原子清空窗口、序号与档位 key（保留原熔断器行为），因此冷却到期恢复后从全新窗口重新累计。

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

- 探测成功 → 立即 `ClearTempUnschedulable` 提前解除，写 `openai.apikey_health_probe_recovered` 日志。成功判定**不再是「2xx 即成功」**：
  - APIKey 账号：仍走 `/models` 零消耗端点，2xx 即成功；
  - CodeBuddy 影子账号（oauth 型，无 `/models` 面）：改发一次最小**流式** chat 探测（模型取影子 `extra.shadow_model`、`max_tokens=1`、system-first、`stream:true`——上游拒绝非流式请求），成功须同时满足四条件：HTTP 2xx、聚合结果为有效 chat completion（choices 非空、无业务错误信封）、原始 SSE 出现 `data: [DONE]` 正常终止帧、全程无错误帧（`event: error` 帧或 data 帧业务错误信封均算）。任一不满足即判探测失败；
- 探测失败 → 维持冷却，并将尝试次数 +1（写回 reason 的 `probe_attempts`）；
- 尝试次数达到 `probe.max_attempts` → 停止探测，退回「到期自动恢复」；
- 某平台无安全/廉价探测端点 → 探测降级为「到期恢复」，代码注释与本节已说明，不会刷屏；
- 探测选号按停调标记词（`matched_keyword`）白名单认领：`openai_apikey_health_breaker`（熔断 L3 停调）与 `pool_mode_401_escalation`（池模式账号 401 窗口升级停调：5 分钟内累计 6 次上游 401 → 停调 30 分钟，边沿触发幂等）。池 401 停调经探测提前恢复时，会同步重置该账号的 401 窗口计数器——否则恢复后同一窗口内再次 401 不再跨越升级阈值，账号失去保护。

> **开关热生效（运行时无需重启）**：探测循环在进程启动时即常驻，每个 tick（≤ `probe.interval_seconds`，默认 60s）都会重新读取 `probe.enabled`，因此**在管理后台开启/关闭探测无需重启进程**，最长一个探测间隔内即生效。关闭后循环仍在运行但每个 tick 直接跳过、不发起任何探测请求。进程优雅关闭时会调用 `Stop()` 终止该循环，不会泄漏 goroutine。

#### 探测的局限（开启前须知）

- **`/models` 与聊天上游可能不同路**：部分中转/代理站点 `/models` 由轻量前端返回，并不真正打到重上游；此时 `/models` 返回 2xx 但 chat 仍坏，探测会**过早解除**冷却。该局限默认由「探测关闭」兜底；这类站点应保持 `probe.enabled=false`，或仅在 `/models` 与 chat 共用同一上游时开启。
- **多实例重复探测（幂等无害）**：每个后端副本各自独立运行探测扫描，同一被冷却账号可能被多个实例同时探测。探测对上游是只读的，其副作用（成功则 `ClearTempUnschedulable`、失败则 `probe_attempts` +1）均为幂等，重复探测除略微增加上游流量外无副作用。

> 探测请求日志与错误信息中**不得泄漏密钥**；探测默认关闭，仅管理员显式开启。

---

## 15. CodeBuddy 双站点（CN + 国际版）运维备忘

CodeBuddy 平台以**账号级站点属性**支持国内版与国际版（不做第二个平台）：
- 站点 URL 表唯一事实源：`backend/internal/service/codebuddy_site.go`（`cn` / `intl`，**缺省 cn**）。
- 账号凭据：`credentials.site = "cn" | "intl"`；存量账号无该键 = cn，行为不变。
- 出站域名：cn → `copilot.tencent.com` / `www.codebuddy.cn`；intl → `www.codebuddy.ai`。
- 前端：新建账号选站点（CreateAccountModal）；重授权沿用账号既有站点（ReAuthAccountModal）。

### 挂账项（保持，非阻断）

| # | 事项 | 处置触发条件 |
|---|---|---|
| 1 | **intl 计费快照 live 验证**：`gateway.codebuddy.quota_check_enabled` 默认关闭，故 intl 未产 Extra 额度快照。已用 Phase 0 直连 billing 200（schema 与 CN 同构）+ 单测钉住 intl base 替代 | **未来任何原因开启 `quota_check_enabled` 时，顺带验证 intl 账号（site=intl）的 Extra 快照** |
| 2 | `GET /api/v1/admin/accounts/:id/usage` 对 codebuddy 返回 500（`getUsageForAccount` 无 codebuddy 分支，落通用 Claude usage → 上游 403）。**既有缺陷，CN/intl 同样中招** | 下个维护批次：补 codebuddy 分支，改走配额快照（`CodeBuddyQuotaService`） |
| 3 | CodeBuddy 模型定价配置（如 CN `hy3`、intl `deepseek-v3`）缺失时用量照记、成本计 0 | ~~运营按需配置~~ **已配置（2026-09-21）**：CN 全模型按 credits 实测扣费价写入 custom_model_pricing（28 行），详见 §16 |
| 4 | UA 版本监控：默认 `CLI/2.63.2`（共享配置 `gateway.codebuddy.chat_user_agent`），官方 CLI 已 2.150.0，上游当前未校验 | 长期观察；上游若校验版本再热更 |

### intl 实测要点（详见 docs/evidence/codebuddy-intl/）
- intl token 为 JWT（`domain=www.codebuddy.ai`），refresh 响应无 `domain`；uid 为 UUID、个人账号无 enterpriseId。
- intl **models 端点认证后 HTTP 500**（动态模型不可用，代码内已对 intl 降级）。
- intl 首条消息必须为 system（否则 400 `11128`）；非流式 `11101`；模型无效 `11102`。
- intl 可用模型（该个人账号样本）：`deepseek-v3`、`deepseek-v3-0324`。

---

## §16 CodeBuddy CN 定价：credits 实测标定与 custom_model_pricing 配置（2026-09-21）

### 计费公式（活体探针标定，8 次探针全部 200）

```
credits = mult × (300 × prompt_tokens + 1500 × completion_tokens) / 1e6
```

- 基准价：输入 $3/M、输出 $15/M（Claude Sonnet 官价口径）；**1 credit = $0.01**。
- 倍率 `mult` 来自 `GET /console/enterprises/personal/models` 响应中每个模型的 `credits` 字段（形如 `"x0.06 credits"`，需解析数字）。
- 上游 SSE 最后 chunk 的 `usage.credit` 字段即本次请求真实扣费（**含折扣后**）；非流式同样有。
- 余额侧：扣减落在 `get-user-resource` 响应的 `CycleCapacityUsedPrecise`（`CapacityUsedPrecise` 恒 0，勿看错列）；扣减有分钟级聚合延迟。

### 关键实测结论

| 事项 | 结论 |
|---|---|
| kimi-k3-1（标称 x1.62） | **100% 按标称扣**（探针 1.84/14.12 vs 预测 1.840/14.130，误差 <0.1%） |
| glm 系（flash/5.3 等） | **实际按标称 ×0.44~0.45 扣**（限时活动折扣，5 个探针一致）；客户端显示 x0.06 实扣 x0.0264 |
| hy3 | `credits: "x0.00 credits"` → 免费 |
| auto / default / hunyuan-chat / hunyuan-image | 无 credits 字段 |
| CN token 长期有效 | expires_at ≈ 一年后；CN models 端点正常（intl 才 500） |

### custom_model_pricing 配置格局（方案 A：贴真实扣费）

- 换算：`input_price = eff_mult × 3e-6`、`output_price = eff_mult × 15e-6` USD/token。
- glm 系乘 0.45 活动系数；kimi/deepseek/hy/minimax/hunyuan 按标称 ×1.0。
- 现有 28 行全部启用；覆盖更新了原价格钉 id=3（glm-5.3-flash）、id=4（glm-5.3）、id=7（deepseek-v4-flash 拆分独立）；id=5（kimi-k2.7-code，Moonshot 官方钉价）与 id=8（hy3 腾讯云官方价）**保留未动**（非 CodeBuddy credits 口径）。
- remark 统一注明 `CodeBuddy credits 实测标定 2026-09-20`。

### 维护注意（踩过的坑）

1. **同名冲突**：custom 层匹配按 `ORDER BY id` 先到先得（`custom_model_pricing_repo.go` List），写库前必须查重：
   `SELECT e1.id FROM custom_model_pricing e1, custom_model_pricing e2 WHERE e1.id<e2.id AND EXISTS(SELECT 1 FROM jsonb_array_elements_text(e1.models) m1 JOIN jsonb_array_elements_text(e2.models) m2 ON m1=m2);`
2. **created_by 必填**：SQL 直插该列为 NULL 会让快照刷新循环报 `scan error column created_by`（int64 不收 NULL），custom 层整体失效。直插时填 admin user id。
3. **活动折扣会失效**：glm 系 0.45 系数是限时活动价，活动结束后需重标（重跑探针法：小 prompt 大 max_tokens 请求读 usage.credit）。
4. kimi-k2.x / deepseek-v4-pro / hy4 等是否也有活动折扣未逐个实测（按标称配置，偏保守多记）。

---

最后更新：2026-09-21
