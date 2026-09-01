#!/usr/bin/env bash
# sub2api-publish: 在本地触发 GitHub Actions workflow_dispatch 发布镜像。
#
# 用法（在本地开发机执行）：
#   ./publish.sh {tag}
# 示例：
#   ./publish.sh 20260901-120000
#
# 行为：
#   1. 验证本地无未提交改动
#   2. git push 当前分支到 origin
#   3. 触发 GitHub API workflow_dispatch（trzjy/sub2api）
#   4. 轮询 Actions run 状态（最多 20 分钟）
#   5. 完成后打印 pull 指令和下一步
#
# 依赖：
#   - GITHUB_TOKEN 环境变量（GitHub PAT，有效期需 >30 天）
#   - gh CLI 认证（gh auth status 需通过）
#   - git remote origin 指向 trzjy/sub2api

set -euo pipefail

GH_REPO="${GH_REPO:-trzjy/sub2api}"
WORKFLOW_FILE="release.yml"
WORKFLOW_ID="Release"

if [ $# -ne 1 ]; then
  echo "Usage: $0 {tag}" >&2
  echo "  tag: 发布版本号（不含 v 前缀，如 20260901-120000）" >&2
  exit 2
fi

TAG="$1"

# 1. 验证 GitHub token（优先用 GITHUB_TOKEN 环境变量，否则从 gh CLI 读取）
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

# 3. 验证当前分支与 origin 一致
BRANCH=$(git branch --show-current)
if [ -z "$BRANCH" ]; then
  echo "ERROR: 无法获取当前分支名" >&2
  exit 1
fi

UPSTREAM_REMOTE=$(git config branch.$BRANCH.remote 2>/dev/null || echo "origin")
UPSTREAM_BRANCH=$(git config branch.$BRANCH.merge 2>/dev/null | sed 's|refs/heads/||' || echo "$BRANCH")

echo "==> 当前分支: $BRANCH -> $UPSTREAM_REMOTE/$UPSTREAM_BRANCH"

# 4. git push
echo "==> [1/4] git push $BRANCH 到 origin ..."
git push "$UPSTREAM_REMOTE" "$BRANCH:$UPSTREAM_BRANCH"

# 5. 获取 workflow ID
echo "==> [2/4] 获取 workflow ID ..."
WORKFLOW_ID_NUM=$(gh api repos/"$GH_REPO"/actions/workflows \
  --jq ".workflows[] | select(.name == \"Release\") | .id" 2>/dev/null)
if [ -z "$WORKFLOW_ID_NUM" ]; then
  echo "ERROR: 无法获取 Release workflow ID" >&2
  exit 1
fi
echo "    Workflow ID: $WORKFLOW_ID_NUM"

# 6. 触发 workflow_dispatch
echo "==> [3/4] 触发 workflow_dispatch (tag=$TAG, simple_release=true) ..."

# 构建 JSON body（用变量拼接避免引号嵌套地狱）
DISPATCH_JSON=$(printf '{"ref":"%s","inputs":{"tag":{"value":"%s"},"simple_release":{"value":"true"}}}' \
  "$BRANCH" "$TAG")

DISPATCH_OUT=$(gh api repos/"$GH_REPO"/actions/workflows/"$WORKFLOW_ID_NUM"/dispatches \
  -X POST --input - <<< "$DISPATCH_JSON" 2>&1)

DISPATCH_RC=$?
if [ $DISPATCH_RC -ne 0 ]; then
  echo "ERROR: workflow_dispatch 触发失败 (rc=$DISPATCH_RC)" >&2
  echo "响应: $DISPATCH_OUT" >&2
  echo "" >&2
  echo "提示：fork 的 workflow_dispatch 可能需要 GitHub 网页上手动触发" >&2
  echo "  https://github.com/trzjy/sub2api/actions/workflows/release.yml" >&2
  exit 1
fi
echo "    触发成功: https://github.com/$GH_REPO/actions"

echo "    触发成功: https://github.com/$GH_REPO/actions"

# 7. 轮询 Actions run
echo "==> [4/4] 等待 Actions run 完成（最多 20 分钟）..."
MAX_WAIT=1200
INTERVAL=15
elapsed=0

while [ "$elapsed" -lt "$MAX_WAIT" ]; do
  sleep "$INTERVAL"
  elapsed=$((elapsed + INTERVAL))

  # 获取最新 run 状态
  RUN_STATUS=$(gh api repos/"$GH_REPO"/actions/runs \
    --jq ".workflow_runs[0] | {status: .status, conclusion: .conclusion, html_url: .html_url}" 2>/dev/null)

  RUN_CONCLUSION=$(echo "$RUN_STATUS" | jq -r '.conclusion' 2>/dev/null)
  RUN_HTML=$(echo "$RUN_STATUS" | jq -r '.html_url' 2>/dev/null)

  if [ "$RUN_CONCLUSION" = "success" ]; then
    echo ""
    echo "✓ 发布成功！"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  GitHub Actions: $RUN_HTML"
    echo "  镜像:   ghcr.io/$GH_REPO:$TAG"
    echo ""
    echo "  下一步（服务器执行）："
    echo "  ssh root@yiyutu-server"
    echo "  cd /opt/sub2api"
    echo "  ./deploy-config/deploy.sh $TAG"
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
