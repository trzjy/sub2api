# 派发单 Vision-S4：分流目标 ID 非法值拒绝（终审 P2-E）

## 背景
终审第 2 轮 P2：`vision_routing_service.go` 约 159-170 行 `collectVisionRoutingTargetAccountIDs`
跳过 ≤0 的目标 ID——配置 `{"model":[0]}` 或负数 ID 时绕过同组校验被持久化，运行时该规则
排除全部候选，形成"配置成功但永远无账号"。方案 §3.7：目标集合必须 ⊆ 分组账号。

## 改动范围
- `backend/internal/service/vision_routing_service.go` + `vision_routing_service_test.go`：
  配置写入校验将非正账号 ID（≤0）判为配置错误直接拒绝（复用
  `ErrVisionRoutingInvalidConfig` 或同构错误）；同组校验覆盖配置中的每一个目标值，
  不得跳过任何元素。
- 测试：①含 0 ID 的配置被拒绝；②含负数 ID 的配置被拒绝；③合法正 ID 配置零回归。

## 禁区
- 只动上述两文件。不得动 scheduler、`vision_detect_service.go`、
  `account_model_capability_service.go`、wire、frontend（其他整改单在改）。
- 禁止 `git checkout --`/`git restore` 任何文件。
- 开工前后各跑一次 `git status --short` 快照自证零越界，写进回报。

## 验证命令白名单（只允许跑以下命令）
- `go build ./internal/service/`
- `go test ./internal/service/ -run 'VisionRouting' -count=1`

## Done when
- 三个场景实跑通过并附输出；既有 VisionRouting 测试零回归。
- `git status --short` 前后快照证明只改派发单列出文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
