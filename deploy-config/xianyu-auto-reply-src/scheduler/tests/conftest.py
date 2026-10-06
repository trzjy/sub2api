"""P1 防锤暂停态定向测试专属 conftest（scheduler 包环境）。

把 scheduler/ 与仓库根加入 sys.path，使 `import app` 解析为 scheduler/app，
`import common` 解析为仓库根 common。绝不引入 backend-web / websocket 的 app，
避免同名 `app` 包冲突。所有 DB / 出站均 mock，禁止任何真实网络请求。
"""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent  # scheduler/tests -> repo root
SCHEDULER = ROOT / "scheduler"

for p in (str(SCHEDULER), str(ROOT)):
    if p not in sys.path:
        sys.path.insert(0, p)
