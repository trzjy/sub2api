# Web Quota Worker 反检测改进方案

## 当前状态
已有基础伪装，但缺少高级指纹对抗，导致美团等平台风控触发。

## 需要添加的高级伪装

### 1. Canvas 指纹毒化（最优先）
```javascript
// 添加到 _BROWSER_PATCH_JS
(() => {
  const toBlob = HTMLCanvasElement.prototype.toBlob;
  const toDataURL = HTMLCanvasElement.prototype.toDataURL;
  const getImageData = CanvasRenderingContext2D.prototype.getImageData;
  
  // 注入轻微噪声，让每次结果略有不同但不影响视觉
  const noisify = (data) => {
    for (let i = 0; i < data.data.length; i += 4) {
      data.data[i] = data.data[i] + Math.random() * 0.1 - 0.05;
    }
  };
  
  CanvasRenderingContext2D.prototype.getImageData = function(...args) {
    const imageData = getImageData.apply(this, args);
    noisify(imageData);
    return imageData;
  };
})();
```

### 2. WebGL 指纹伪装
```javascript
(() => {
  const getParameter = WebGLRenderingContext.prototype.getParameter;
  WebGLRenderingContext.prototype.getParameter = function(parameter) {
    // 伪装成常见显卡
    if (parameter === 37445) { // UNMASKED_VENDOR_WEBGL
      return 'Intel Inc.';
    }
    if (parameter === 37446) { // UNMASKED_RENDERER_WEBGL  
      return 'Intel Iris OpenGL Engine';
    }
    return getParameter.call(this, parameter);
  };
})();
```

### 3. 硬件参数对齐
```javascript
Object.defineProperty(navigator, 'hardwareConcurrency', { 
  get: () => 8, configurable: true 
});
Object.defineProperty(navigator, 'deviceMemory', { 
  get: () => 8, configurable: true 
});
Object.defineProperty(screen, 'colorDepth', { 
  get: () => 24, configurable: true 
});
```

### 4. Audio Context 指纹
```javascript
(() => {
  const AudioContext = window.AudioContext || window.webkitAudioContext;
  const getChannelData = AudioBuffer.prototype.getChannelData;
  AudioBuffer.prototype.getChannelData = function() {
    const data = getChannelData.apply(this, arguments);
    for (let i = 0; i < data.length; i++) {
      data[i] = data[i] + Math.random() * 0.0000001;
    }
    return data;
  };
})();
```

### 5. 容器字体安装
```bash
# Dockerfile 添加
RUN apt-get update && apt-get install -y \
    fonts-wqy-zenhei \
    fonts-wqy-microhei \
    fonts-noto-cjk \
    fonts-liberation \
    fonts-dejavu
```

### 6. 行为模拟
```python
# login_sessions.py 中添加
async def humanized_click(page, x, y):
    """模拟人类鼠标移动：贝塞尔曲线 + 随机停顿"""
    import random
    # 分5-8步移动到目标
    steps = random.randint(5, 8)
    for i in range(steps):
        progress = (i + 1) / steps
        # 添加随机偏移
        offset_x = random.uniform(-3, 3)
        offset_y = random.uniform(-3, 3)
        intermediate_x = x * progress + offset_x
        intermediate_y = y * progress + offset_y
        await page.mouse.move(intermediate_x, intermediate_y)
        await asyncio.sleep(random.uniform(0.01, 0.05))
    await page.mouse.click(x, y)

async def humanized_type(page, selector, text):
    """模拟人类打字：变速 + 偶尔回退"""
    await page.focus(selector)
    for char in text:
        await page.keyboard.type(char)
        # 随机延迟 50-200ms
        await asyncio.sleep(random.uniform(0.05, 0.2))
```

## 方案 2：使用反检测库（推荐）

### playwright-stealth (Python)
```bash
pip install playwright-stealth
```

```python
from playwright_stealth import stealth_async

context = await browser.new_context(**kwargs)
page = await context.new_page()
await stealth_async(page)
```

### undetected-playwright
```bash
pip install undetected-playwright
```

## 方案 3：混合方案（最佳）
1. 基础：使用 playwright-stealth
2. 增强：添加 Canvas/WebGL 毒化
3. 行为：添加人类化操作
4. 环境：安装中文字体
5. 网络：使用住宅代理（如果可行）

## 实施优先级
1. ⚠️ Canvas 指纹毒化（立即）
2. ⚠️ WebGL 伪装（立即）
3. 🔧 安装字体（简单）
4. 🔧 硬件参数对齐（简单）
5. 📝 行为模拟（中期）
6. 📝 使用 stealth 库（长期，需要测试兼容性）

## 监控建议
- 记录每次登录的验证轮次
- 记录触发的验证类型（滑块/扫码/短信）
- 对比真实浏览器 vs Worker 的验证频率
- 收集 410 错误的触发时机
