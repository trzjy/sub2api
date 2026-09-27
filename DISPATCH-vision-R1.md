# 派发单 Vision-R1：能力表 Upsert 不得覆盖 manual 标记（终审 P1-1）

## 背景
方案 SSOT `docs/capability-routing-plan.md` §3.8：手动覆盖优先级最高。终审 P1-1：检测落库的
Upsert 无条件写 `source`/`supports_vision`，管理员 manual 标记会被后续检测覆盖。
消化记录：`REVIEW-PACKET-vision-routing-final.md` §7。

## 改动范围
- `backend/internal/repository/account_model_capability_repo.go`（Upsert，约 74-80 行）：
  冲突更新路径加条件——现有行 `source='manual'` 时保持人工值不被检测写入覆盖
  （可用 `DO UPDATE ... WHERE account_model_capabilities.source <> 'manual'` 或等价实现；
  非 manual 行检测刷新行为不变）。
- 显式手动覆盖路径（Override，已有）不受影响，仍可改任何行。
- 测试：在 `account_model_capability_repo_test.go` 补场景：①manual 行被检测 Upsert 命中后
  仍为 manual 原值；②detect 行被检测 Upsert 命中后正常刷新；③Override 可覆盖 manual。

## 禁区
- 只动上述 repo 文件与其测试文件。其他文件（含 service/handler）一律不动。
- 禁止 `git checkout --`/`git restore` 任何文件（工作区混有其他任务改动）。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。
- 超出范围的发现一律 BLOCKED 顶回，不自行扩大改动。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/repository/`
- `go test ./internal/repository/ -run 'AccountModelCapability' -count=1`

## Done when
- 三个测试场景实跑通过并附输出。
- `git status --short` 前后快照证明只改了派发单列出的文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
