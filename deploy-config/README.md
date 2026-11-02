# Sub2API 部署配置 (deploy-config)

本目录存放 `corealgos.com` 生产环境的 Sub2API 部署配置，随源码仓库（fork `trzjy/sub2api`）一同版本管理。

## 目录内容

| 文件 | 说明 |
|------|------|
| `compose.yml` | Docker Compose：sub2api + PostgreSQL + Redis（卷路径基于上一级 `/opt/sub2api`） |
| `.env.example` | 环境变量模板（复制为 `/opt/sub2api/.env` 并填写真实凭据） |
| `.gitignore` | 排除 `.env`、数据目录 |
| `nginx/corealgos.conf` | Nginx 站点配置（`/etc/nginx/sites-available/duizhang`） |
| `sub2api-ops.md` | 运维指南（部署/升级/备份/故障排查） |

## 服务器结构

```
/opt/sub2api/            # git 工作区（完整 checkout：源码 + deploy-config/ 部署配置）
├── .env                 # 真实凭据（gitignore，不入库）
├── deploy-config/       # 部署配置（compose.yml、nginx、本 README、sub2api-ops.md）
├── data/                # sub2api 数据
├── postgres_data/       # PostgreSQL 数据
├── redis_data/          # Redis 数据
├── backend/ frontend/ Dockerfile ...   # 应用源码（服务器端构建镜像用）
```

> compose 位于 `deploy-config/`，卷路径用 `../data` 等指向根目录数据，因此数据目录与 compose 文件分离。
> 生产镜像在服务器本地构建（不推 registry），详见 `sub2api-ops.md` 第 5 节。

## 更新流程

**应用代码升级**：见 `sub2api-ops.md` 第 5 节。部署动作只在生产服务器 SSH 会话中执行；本地只推送 `origin/main`，禁止本地运行任何部署脚本或容器命令。

**仅部署配置变更**（compose / nginx / 本目录文档）：

```bash
# 本地仅提交并推送，不执行部署脚本或容器命令
cd /mnt/data/sub2api
git add deploy-config && git commit -m "..." && git push origin main

# 生产服务器（唯一部署边界）
ssh yiyutu-server '/opt/sub2api/deploy/deploy.sh origin/main'
```

> 注意：服务器脚本会先 `git fetch origin`，再检出并构建 `origin/main`，最后执行全项目 Compose 重建和后置健康断言。不要在本地执行 `deploy.sh`，也不要用手工 `docker compose up` 绕过唯一入口。

## 首次部署（从 fork 拉取）

```bash
ssh yiyutu-server
git clone git@github.com:trzjy/sub2api.git /opt/sub2api
cd /opt/sub2api
cp deploy-config/.env.example .env   # 填真实凭据
mkdir -p /opt/sub2api-data/{data,postgres_data,redis_data}
# 首次镜像：按 sub2api-ops.md 第 5 节在服务器构建 sub2api:<commit>-w
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d
```
