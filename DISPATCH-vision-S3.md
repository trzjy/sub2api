# 派发单 Vision-S3：能力缓存失效错误传播与陈旧回填阻断（终审 P2-C）

## 背景
终审第 2 轮 P2：`account_model_capability_service.go` 约 190-210 行
`invalidateAndNotifyAccount` 忽略 `InvalidateAccount`/`NotifyUpdate` 错误，且失效失败后
仍调 `refreshAccountCache`（可能从 Redis 读回旧记录回填本地缓存）——手动覆盖/检测结果
已落库但调度继续用旧能力标记；多实例广播失败时其他实例命中旧值。违反方案 §3.3
缓存失效广播约定与合同 §5 禁兜底。

## 改动范围
- `backend/internal/service/account_model_capability_service.go` + 测试文件：
1. 失效或广播失败 → 清除本地缓存条目，**禁止**从可能陈旧的二级缓存回填；后续读走
   权威 DB（本地 miss → DB 直读）。
2. 同步失败传播到写路径：更新接口返回错误（如 "invalidate vision capability cache: %w"），
   不得把"未即时生效"的更新报成功。
3. 读路径：本地 miss 时直读 DB 权威源并回填（既有语义保留），仅在"失效刚失败"的窗口
   内跳过 Redis 回填（可用条目级标记或直接该次直读不回填二级缓存，二选一，实现说明
   写进回报）。
- 测试：①InvalidateAccount 失败 → 本地缓存被清 + 后续读直连 DB 拿到新值 + 写路径报错；
  ②NotifyUpdate 失败 → 同上且不静默；③正常失效 → 既有行为零回归。

## 禁区
- 只动上述两文件。不得动 scheduler、`vision_detect_service.go`、`vision_routing_service.go`、
  repo/缓存实现层（`account_model_capability_cache.go` 除非确需接口方法，若需改动
  先 BLOCKED 顶回附理由，不得自行扩大）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'AccountModelCapability' -count=1`

## Done when
- 三个场景实跑通过并附输出。
- `git status --short` 前后快照证明只改派发单列出文件（如确需动 cache 接口，须先 BLOCKED）。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
