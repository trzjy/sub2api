# 派发单 Vision-T2：能力缓存并发回源竞态修复（第 3 次终审 P1-B）

## 背景
终审第 3 轮 P1：`account_model_capability_service.go` 约 195-204 行——并发下读请求
从 DB 读出旧能力记录，写请求随后完成 Upsert+失效+刷新，之后读请求再 `setLocalAccount`
把旧列表装回本地缓存并可长期命中，导致已知不支持视觉的账号继续承接带图请求（或反向）。
方案 §3.3/§3.4 正确性前提；合同 §5 禁止以放宽过滤规避竞态。

## 改动范围
- `backend/internal/service/account_model_capability_service.go` + 测试文件：
  为每个账号的回源读取→本地安装路径加**账号级代际校验**（推荐）或账号级同步：
  - 每次失效/写操作递增该账号的代际计数；
  - 回源读取发起时记录代际，`setLocalAccount` 安装前校验代际未变，变了则丢弃本次
    读取结果（下一请求重新回源，保证拿到失效后的新值）；
  - 实现须在回报中说明并发时序下旧值不可能胜出。
- 测试（可用 race 探测或确定性时序模拟）：①模拟"读出旧值→并发失效→安装"时序 →
  旧值不入本地缓存；②正常无并发路径零回归；③`-race` 下并发读写不产生数据竞争。

## 禁区
- 只动上述两文件。不得动 `vision_detect_service.go`（T1 在改）、scheduler、routing、
  wire、缓存 repo 层（`account_model_capability_cache.go` 若确需改先 BLOCKED 顶回）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -race -run 'AccountModelCapability|UpsertCapability|InvalidateAndNotify|ModelSupportsVisionInput' -count=1`

## Done when
- 竞态场景测试实跑通过（含 -race）并附输出；既有测试零回归。
- `git status --short` 前后快照证明只改派发单列出文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
