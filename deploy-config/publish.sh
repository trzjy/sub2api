#!/usr/bin/env bash
# sub2api-publish: 在本地触发 GitHub Actions workflow_dispatch 发布镜像。
#
# 用法（在本地开发机执行）：
#   ./publish.sh
#
# 行为：
#   1. 验证本地无未提交改动
#   2. git push 当前分支到 origin
#   3. 触发 GitHub API workflow_dispatch（trzjy/sub2api，simple_release=true）
#   4. 轮询 Actions run 状态（最多 20 分钟）
#   5. 完成后打印镜像 tag + pull 指令
#
# 注意：
#   - 镜像 tag 由 GoReleaser 自动推断（基于 git ref / commit）
#   - 不会传 tag input（release.yml 的 checkout 会用 tag 作为 git ref，传任意字符串会失败）
#   - 轮询脚本无法预测最终镜像 tag，发布后请查看 GitHub Actions 输出版本号
#
# 依赖：
#   - gh CLI 认证（gh auth status 需通过）
#   - git remote origin 指向 trzjy/sub2api

set -euo pipefail

GH_REPO="${GH_REPO:-trzjy/sub2api}"
WORKFLOW_FILE="release.yml"
WORKFLOW_ID="Release"

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

# 6. 触发 workflow_dispatch（用 curl 替代 gh api，避免 JSON 类型处理问题）
echo "==> [3/4] 触发 workflow_dispatch (simple_release=true) ..."

# 注意：release.yml 的 checkout 步骤会读 tag input 作为 git ref。
# 我们传任意时间戳作为版本号会导致 checkout 失败。
# 正确用法：传 simple_release=true，由 GoReleaser 推断版本号（基于 git ref）。
# 如果需要自定义版本号，先 git tag 然后 push 触发，不要用 workflow_dispatch 传 tag。
DISPATCH_JSON=$(printf '{"ref":"%s","inputs":{"simple_release":true}}' \
  "$BRANCH")

GITHUB_TOKEN=$(gh auth token)

DISPATCH_OUT=$(curl -sS -X POST \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  -H "Accept: application/vnd.github+json" \
  -H "Content-Type: application/json" \
  -d "$DISPATCH_JSON" \
  "https://api.github.com/repos/$GH_REPO/actions/workflows/$WORKFLOW_ID_NUM/dispatches" 2>&1)

DISPATCH_RC=$?
if [ $DISPATCH_RC -ne 0 ]; then
  echo "ERROR: workflow_dispatch 触发失败 (rc=$DISPATCH_RC)" >&2
  echo "响应: $DISPATCH_OUT" >&2
  echo "" >&2
  echo "提示：fork 的 workflow_dispatch 可能需要 GitHub 网页上手动触发" >&2
  echo "  https://github.com/$GH_REPO/actions/workflows/release.yml" >&2
  exit 1
fi
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
    echo ""
    echo "  镜像 tag 由 GoReleaser 自动推断，"
    echo "  请在 Actions 输出的 'Run GoReleaser' 步骤中查看。"
    echo ""
    echo "  常见镜像名（已推 GHCR）："
    echo "    ghcr.io/$GH_REPO:<version>     （如 v0.1.200）"
    echo "    ghcr.io/$GH_REPO:latest"
    echo ""
    echo "  下一步（服务器执行，先在 Actions 页面查到的 tag）："
    echo "    ssh root@yiyutu-server"
    echo "    cd /opt/sub2api"
    echo "    ./deploy-config/deploy.sh ghcr.io/$GH_REPO:<tag>"
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
