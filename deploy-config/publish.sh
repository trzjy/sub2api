#!/usr/bin/env bash
# sub2api-publish: 触发 sub2api 镜像发布到 GHCR。
#
# 用法（在本地开发机执行）：
#   ./publish.sh
#
# 行为：
#   1. 验证本地无未提交改动
#   2. git push 当前分支到 origin
#   3. 创建 git tag（next）并 push
#   4. release.yml 监听 tag push，自动构建并推送 GHCR
#   5. 轮询 Actions run 状态（最多 20 分钟）
#   6. 完成后打印镜像 tag + deploy 指令
#
# 镜像 tag 规则（GoReleaser 默认）：
#   - 如果 git tag 是 v* 格式，镜像 tag 为 v{version}-amd64
#   - 不带 v 前缀时，镜像 tag 为 {tag}-amd64
#
# 依赖：
#   - gh CLI 认证（gh auth status 需通过）
#   - git remote origin 指向 trzjy/sub2api
#   - fork repo 的 Actions workflow 已启用 fork dispatch（在 Settings → Actions → General）

set -euo pipefail

GH_REPO="${GH_REPO:-trzjy/sub2api}"
WORKFLOW_ID="Release"
# 发布用的 git tag（用于触发 release.yml）
RELEASE_TAG="${RELEASE_TAG:-$(date -u +%Y%m%d-%H%M%S)}"

# 1. 验证 GitHub token
if [ -z "${GITHUB_TOKEN:-}" ]; then
  GITHUB_TOKEN=$(gh auth token 2>/dev/null) || true
fi
if [ -z "$GITHUB_TOKEN" ]; then
  echo "ERROR: GITHUB_TOKEN 环境变量未设置，且 gh auth 未登录" >&2
  echo "设置方法：export GITHUB_TOKEN=ghp_xxxx" >&2
  exit 1
fi

# 2. 验证无未提交改动
if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: 存在未提交的改动，请先 commit:" >&2
  git status --short >&2
  exit 1
fi

# 3. 验证当前分支
BRANCH=$(git branch --show-current)
if [ -z "$BRANCH" ]; then
  echo "ERROR: 无法获取当前分支名" >&2
  exit 1
fi

UPSTREAM_REMOTE=$(git config branch.$BRANCH.remote 2>/dev/null || echo "origin")
UPSTREAM_BRANCH=$(git config branch.$BRANCH.merge 2>/dev/null | sed 's|refs/heads/||' || echo "$BRANCH")

echo "==> 当前分支: $BRANCH -> $UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
echo "==> 发布 tag: $RELEASE_TAG"

# 4. git push 分支
echo "==> [1/5] git push $BRANCH 到 origin ..."
git push "$UPSTREAM_REMOTE" "$BRANCH:$UPSTREAM_BRANCH"

# 5. 创建并 push git tag
echo "==> [2/5] 创建本地 tag $RELEASE_TAG ..."
git tag -f "$RELEASE_TAG"
echo "==> [3/5] git push tag $RELEASE_TAG 到 origin ..."
git push "$UPSTREAM_REMOTE" "$RELEASE_TAG" --force

# 6. 轮询 Actions run
echo "==> [4/5] 等待 Actions run 完成（最多 20 分钟）..."
MAX_WAIT=1200
INTERVAL=15
elapsed=0
RUN_HTML=""

while [ "$elapsed" -lt "$MAX_WAIT" ]; do
  sleep "$INTERVAL"
  elapsed=$((elapsed + INTERVAL))

  RUN_STATUS=$(gh run list --repo "$GH_REPO" --workflow=Release --limit 1 \
    --json databaseId,status,conclusion,url 2>/dev/null)

  RUN_CONCLUSION=$(echo "$RUN_STATUS" | jq -r '.[0].conclusion' 2>/dev/null)
  RUN_HTML=$(echo "$RUN_STATUS" | jq -r '.[0].url' 2>/dev/null)

  if [ "$RUN_CONCLUSION" = "success" ]; then
    echo ""
    echo "✓ 发布成功！"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  GitHub Actions: $RUN_HTML"
    echo "  git tag: $RELEASE_TAG"
    echo ""
    echo "  镜像（已推 GHCR）："
    echo "    ghcr.io/$GH_REPO:$RELEASE_TAG-amd64"
    echo "    ghcr.io/$GH_REPO:$RELEASE_TAG"
    echo "    ghcr.io/$GH_REPO:latest"
    echo ""
    echo "  下一步（服务器执行）："
    echo "    ssh root@yiyutu-server"
    echo "    cd /opt/sub2api"
    echo "    ./deploy-config/deploy.sh ghcr.io/$GH_REPO:$RELEASE_TAG"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    exit 0
  elif [ "$RUN_CONCLUSION" = "failure" ] || [ "$RUN_CONCLUSION" = "cancelled" ]; then
    echo ""
    echo "ERROR: Actions run 失败: $RUN_CONCLUSION" >&2
    echo "详情: $RUN_HTML" >&2
    exit 1
  fi

  printf "  ⏳ 已等待 %ds，仍在运行...\n" "$elapsed"
done

echo ""
echo "ERROR: Actions run 超时（20 分钟），请手动检查:" >&2
echo "  $RUN_HTML" >&2
exit 1
