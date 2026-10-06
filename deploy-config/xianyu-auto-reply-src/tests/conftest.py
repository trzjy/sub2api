"""pytest 共享配置。

把 SRC 根目录插入 sys.path，保证从任意入口（`python -m pytest` 或裸 `pytest`）
都能 `import common.db.compat`。不做其它事情。
"""
import sys
from pathlib import Path

SRC_ROOT = Path(__file__).resolve().parent.parent
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))
