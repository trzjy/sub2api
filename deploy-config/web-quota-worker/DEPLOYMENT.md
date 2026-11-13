# Web Quota Worker 反风控增强部署指南

## 修改内容

### 2026-11-13 反风控增强
为降低上游平台（美团/LongCat 等）风控触发率，添加了高级指纹伪装和行为模拟。

## 修改的文件

### 1. `app.py` - 浏览器指纹伪装增强
**修改内容**：扩展 `_BROWSER_PATCH_JS` 注入脚本

**新增指纹伪装**：
- ✅ 硬件参数对齐：`navigator.hardwareConcurrency` (8核) / `deviceMemory` (8GB)
- ✅ WebGL 指纹伪装：`UNMASKED_VENDOR_WEBGL` → Intel Inc. / `UNMASKED_RENDERER_WEBGL` → Intel Iris OpenGL Engine
- ✅ Canvas 指纹毒化：注入 ±0.1 的随机噪声（人眼不可见，但改变指纹）
- ✅ Audio Context 指纹毒化：音频数据添加极微小噪声
- ✅ 网络信息：添加 `navigator.connection` (4g / 100ms RTT)

**风险评估**：
- 风险：低
- 影响：所有通过 Worker 的登录和探测请求
- 降级方案：注入脚本失败不影响主流程（已有 try-catch）

### 2. `login_sessions.py` - 人类化操作
**修改内容**：
- `tap()`：添加贝塞尔曲线鼠标轨迹（5-8步到达目标，带随机噪声）
- `type_text()`：逐字符输入，随机延迟 50-150ms（原为固定 20ms）

**行为对比**：
```python
# 修改前（机器人特征）
await page.keyboard.type("13800000000", delay=20)  # 固定延迟
await page.mouse.click(x, y)  # 瞬间到达

# 修改后（人类特征）
for char in text:
    await page.keyboard.type(char)
    await asyncio.sleep(random.uniform(0.05, 0.15))  # 随机延迟

await _humanized_mouse_move(page, x, y)  # 曲线移动
await asyncio.sleep(random.uniform(0.05, 0.15))  # 到达后停顿
await page.mouse.click(x, y)
```

**性能影响**：
- 输入 11 位手机号：从 220ms → 550-1650ms（慢 2-7 倍）
- 点击操作：从 <10ms → 50-400ms（慢 5-40 倍）
- **整体影响可接受**：真人操作本来就慢，这是特性不是 bug

### 3. `Dockerfile` - 中文字体安装
**新增包**：
- `fonts-wqy-zenhei` - 文泉驿正黑
- `fonts-wqy-microhei` - 文泉驿微米黑
- `fonts-noto-cjk` - 思源黑体/宋体
- `fonts-liberation` - Liberation 系列
- `fonts-dejavu-core` - DejaVu 系列

**镜像体积影响**：约 +150MB

### 4. `tests/test_login_sessions.py` - 测试适配
更新 `test_actions_forward_to_same_page` 以适应逐字符输入。

## 部署步骤

### 步骤 1：重新构建镜像
```bash
cd deploy-config/web-quota-worker
docker build -t web-quota-worker:anti-detect .
```

**预计耗时**：5-10 分钟（首次需要下载字体包）

### 步骤 2：灰度测试（推荐）
先在 1-2 个账号上测试，观察：
- 登录验证轮次是否减少
- 410 错误率是否降低
- 登录成功率是否提升

```bash
# 停止旧容器
docker stop web-quota-worker

# 启动新容器（保持其他配置不变）
docker run -d --name web-quota-worker \
  -p 18090:18090 \
  -e WEB_QUOTA_WORKER_TOKEN=your-token \
  web-quota-worker:anti-detect
```

### 步骤 3：监控指标

**关键指标**：
1. **410 错误率**（期望：降低 50%+）
2. **登录验证轮次**（期望：从 3-5 轮 → 1-2 轮）
3. **登录成功率**（期望：提升）
4. **操作延迟**（预期：增加 2-7 倍，可接受）

**监控方法**：
```bash
# 查看日志中的 410 错误
docker logs web-quota-worker 2>&1 | grep -i "410\|gone" | wc -l

# 查看登录成功/失败日志
docker logs web-quota-worker 2>&1 | grep -E "authenticated|login.*failed"
```

### 步骤 4：全量部署
灰度测试 1-2 天无问题后，全量部署。

### 步骤 5：回滚方案
如果出现问题，立即回滚到旧镜像：
```bash
docker stop web-quota-worker
docker rm web-quota-worker
docker run -d --name web-quota-worker \
  -p 18090:18090 \
  -e WEB_QUOTA_WORKER_TOKEN=your-token \
  web-quota-worker:previous-tag
```

## 验证方法

### 验证 1：指纹伪装生效
在浏览器控制台运行（通过远程登录面板）：
```javascript
console.log({
  webdriver: navigator.webdriver,  // 应为 undefined
  hardwareConcurrency: navigator.hardwareConcurrency,  // 应为 8
  deviceMemory: navigator.deviceMemory,  // 应为 8
  platform: navigator.platform,  // 应为 Win32
  vendor: navigator.vendor,  // 应为 Google Inc.
});

// 检查 WebGL
const gl = document.createElement('canvas').getContext('webgl');
const debugInfo = gl.getExtension('WEBGL_debug_renderer_info');
console.log({
  vendor: gl.getParameter(debugInfo.UNMASKED_VENDOR_WEBGL),  // 应为 Intel Inc.
  renderer: gl.getParameter(debugInfo.UNMASKED_RENDERER_WEBGL),  // 应为 Intel Iris...
});
```

### 验证 2：字体安装成功
```bash
docker exec web-quota-worker fc-list | grep -i "wqy\|noto"
```
应看到多个中文字体。

### 验证 3：人类化操作生效
观察远程登录时的操作：
- 鼠标应该有轨迹（不是瞬移）
- 输入应该逐字出现（不是瞬间填满）

## 预期效果

### 短期（1-3 天）
- 用户反馈：验证次数减少
- 日志：410 错误减少
- 登录体验：略慢但更稳定

### 中期（1-2 周）
- 登录成功率提升 10-30%
- 410 错误率降低 50%+
- 极端情况（连续验证 5 次+）显著减少

### 长期
如果效果仍不理想，考虑：
1. 使用住宅代理（IP 层面避免风控）
2. 账号预热（新账号先在真实浏览器登录几次）
3. 降级方案：放弃自动化登录，让用户导入 Cookie

## 已知限制

1. **仍可能触发风控**：伪装不是 100% 完美，高级风控仍可能检测
2. **操作变慢**：人类化操作牺牲速度换真实性
3. **镜像体积增大**：约 +150MB（字体）

## 测试覆盖

✅ 所有 116 个单元测试通过
✅ 浏览器环境测试通过
✅ 登录会话测试通过
✅ 适配器测试通过

## 回归风险

- ✅ 低：所有测试通过
- ✅ 降级安全：注入脚本失败不影响主流程
- ✅ 行为兼容：旧的 API 接口不变

## 后续优化方向

1. **动态指纹**：每次启动使用不同的硬件参数（避免固定指纹）
2. **更智能的行为**：模拟真人的犹豫、回退、修正
3. **IP 轮换**：集成住宅代理池
4. **学习真人轨迹**：从真实用户操作中学习鼠标轨迹模式

## 联系与支持

如遇问题，检查：
1. Docker 日志：`docker logs web-quota-worker`
2. 测试覆盖：`pytest tests/ -v`
3. 指纹验证：运行上方的浏览器控制台脚本
