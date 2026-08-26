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
/opt/sub2api/            # git 工作区（sparse-checkout 仅 deploy-config/）
├── .env                 # 真实凭据（gitignore，不入库）
├── data/                # sub2api 数据
├── postgres_data/       # PostgreSQL 数据
├── redis_data/          # Redis 数据
└── deploy-config/       # 本目录（git 跟踪）
    ├── compose.yml
    ├── .env.example
    └── ...
```

> compose 位于 `deploy-config/`，卷路径用 `../data` 等指向根目录数据，因此数据目录与 compose 文件分离。

## 更新流程

```bash
# 本地
cd /mnt/data/sub2api
git add deploy-config && git commit -m "..." && git push origin main

# 服务器
ssh yiyutu-server
cd /opt/sub2api && git pull
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d --remove-orphans   # 应用配置
nginx -t && systemctl reload nginx                                    # 若改 nginx
```

## 首次部署（从 fork 拉取）

```bash
ssh yiyutu-server
git clone git@github.com:trzjy/sub2api.git /opt/sub2api
cd /opt/sub2api
git sparse-checkout init --cone
git sparse-checkout set deploy-config
cp deploy-config/.env.example .env   # 填真实凭据
mkdir -p data postgres_data redis_data
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d
```
