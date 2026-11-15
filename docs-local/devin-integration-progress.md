# Devin 平台接入实施记录

**实施日期**: 2026-11-15  
**分支**: `feat/devin-integration`  
**基于社区方案**: [devin-gateway](https://github.com/CaiJingLong/devin-gateway)

---

## 技术方案

### 平台定位
- **类型**: 多模型聚合平台（类似 Cursor/CodeBuddy）
- **协议**: Connect Protocol + Protobuf (非标准 OpenAI)
- **上游端点**: `https://server.codeium.com`
- **认证方式**: OAuth session token (`devin-session-token$xxx`)
- **支持模型**: Anthropic/OpenAI/Gemini/xAI 多家族

### 接入模式
- 作为聚合平台账号，可为多个原生平台分组提供候选
- 管理员手动录入 session token（存储在 `credentials["token"]`）
- 按模型名自动检测原生平台（复用 `DetectModelPlatform`）

---

## 实施进度

### ✅ 阶段一：平台注册与基础设施（已完成）

**后端**
- `backend/internal/domain/constants.go` - 平台常量 `PlatformDevin`
- `backend/internal/domain/platforms.go` - 平台规格注册（Gateway=OpenAI）
- `backend/internal/service/domain_constants.go` - 服务层常量同步
- `backend/internal/service/gateway_scheduling.go` - 加入聚合平台列表
- `backend/internal/service/gateway_service.go` - `isAggregatePlatformAccount` 支持

**前端**
- `frontend/src/constants/platforms.ts` - 平台选择器 + 内部供给平台标记
- `frontend/src/types/index.ts` - TypeScript 类型定义
- `frontend/src/utils/platformColors.ts` - 完整配色系统（emerald 主题）

**Protobuf 协议层**
- `backend/internal/service/devin_proto.go` - 最小 Protobuf 编解码器（基于 devin-gateway 移植）
- `backend/internal/service/devin.go` - 账号辅助函数
- `backend/internal/service/devin_upstream.go` - GetUserJwt 客户端

**验证**
- ✅ 原型验证通过（`/tmp/devin-proto-test`）
- ✅ 后端编译通过（Go）
- ✅ 平台注册生效（调度列表、聚合判定）

---

### 🚧 阶段二：核心协议实现（待完成）

**GetChatMessage 流式协议**
- [ ] Connect Protocol framing 实现
  - frame 编解码（header + payload）
  - gzip 压缩/解压
  - END_STREAM flag 处理
- [ ] 消息转换层
  - OpenAI Chat Completions → Devin ChatMessagePrompt
  - Tool calls 双向转换
  - Streaming SSE 响应
- [ ] 错误处理与重试

**网关转发集成**
- [ ] 注入 `OpenAIGatewayService` 转发链路
- [ ] 平台判定路由（`IsDevin()` → Devin 协议）
- [ ] 模型映射支持
- [ ] 账号测试端点集成

---

### 🚧 阶段三：前端集成（待完成）

**账号管理**
- [ ] `credentialsBuilder.ts` - Devin token 表单
- [ ] 账号测试支持（调用 GetUserJwt 验证）
- [ ] 批量编辑支持

**UI 组件**
- [ ] 平台图标（可选，当前复用配色）
- [ ] 账号卡片展示

---

### 🚧 阶段四：测试与文档（待完成）

**测试**
- [ ] 单元测试（Protobuf 编解码）
- [ ] 集成测试（完整请求流）
- [ ] 账号测试覆盖

**文档**
- [ ] API 文档（GetUserJwt + GetChatMessage）
- [ ] 管理员指南（如何获取 Devin token）
- [ ] 故障排查

---

## 技术债务与改进点

1. **完整的 Connect Protocol 实现**
   - 当前仅实现 GetUserJwt（单次请求/响应）
   - GetChatMessage 需要完整的流式 framing 支持

2. **错误处理**
   - Devin 错误码映射到 OpenAI 格式
   - 超时与重试策略

3. **性能优化**
   - JWT 缓存（避免每次请求都调用 GetUserJwt）
   - 连接池复用

4. **可观测性**
   - 协议级日志（frame 级别）
   - 性能监控（延迟、失败率）

---

## 参考资料

- **社区方案**: [CaiJingLong/devin-gateway](https://github.com/CaiJingLong/devin-gateway)
- **Devin 官网**: https://devin.ai
- **Codeium Cascade API**: https://server.codeium.com

---

## 下一步行动

1. 实现 GetChatMessage 流式协议（Connect framing）
2. 集成到 OpenAI 网关转发链路
3. 前端账号表单与测试
4. 完整端到端测试

---

**状态**: 🟡 基础设施完成，核心协议开发中
