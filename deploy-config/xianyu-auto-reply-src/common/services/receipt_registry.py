"""发送回执 Future 注册表（_pending_mid_futures 的唯一属主）。

收敛此前分散在 send_msg/send_image_msg/_dispatch_mid_response/create_chat_conversation
等处的 Future 注册与清理，统一：
- register：插入 dict（重复 mid 先取消旧 Future，避免泄漏）
- dispatch：服务端响应到达时 pop + set_result
- discard：消费/超时/异常路径统一清理（pop，不 cancel；Future 引用已交给消费方）
- sweep：定时清理超时未消费项（防平台永不回 mid 的滞留）

唯一会 cancel Future 的地方收敛到 send 层自身异常路径（此时 Future 由 send 层独占）。
"""
from __future__ import annotations

import asyncio
import time


class ReceiptRegistry:
    """按 LWP mid 关联的发送回执 Future 注册表。"""

    def __init__(self) -> None:
        self._futures: dict[str, asyncio.Future] = {}
        self._registered_at: dict[str, float] = {}

    def register(self, mid: str) -> asyncio.Future:
        """注册 mid 对应的回执 Future；若已有同 mid 未消费项，先取消旧的（防泄漏）。"""
        old = self._futures.pop(mid, None)
        if old is not None and not old.done():
            old.cancel()
        fut = asyncio.get_running_loop().create_future()
        self._futures[mid] = fut
        self._registered_at[mid] = time.monotonic()
        return fut

    def dispatch(self, mid: str, message_data: dict) -> None:
        """服务端响应到达：pop 并 set_result（若尚未完成）。"""
        fut = self._futures.pop(mid, None)
        self._registered_at.pop(mid, None)
        if fut is not None and not fut.done():
            fut.set_result(message_data)

    def discard(self, mid: str) -> None:
        """消费/超时/异常路径统一清理入口（幂等）。"""
        self._futures.pop(mid, None)
        self._registered_at.pop(mid, None)

    def get(self, mid: str) -> asyncio.Future | None:
        return self._futures.get(mid)

    def contains(self, mid: str) -> bool:
        return mid in self._futures

    def pop(self, mid: str, default: asyncio.Future | None = None) -> asyncio.Future | None:
        """兼容外部直接 pop（幂等），返回 Future 或 default。"""
        fut = self._futures.pop(mid, default)
        self._registered_at.pop(mid, None)
        return fut

    def sweep(self, max_age_seconds: float = 600.0) -> int:
        """清理超过 max_age_seconds 仍未消费的项（平台永不回 mid 的兜底），返回清理数。"""
        now = time.monotonic()
        stale = [mid for mid, t in self._registered_at.items() if now - t > max_age_seconds]
        for mid in stale:
            fut = self._futures.pop(mid, None)
            self._registered_at.pop(mid, None)
            if fut is not None and not fut.done():
                fut.cancel()
        return len(stale)

    def __len__(self) -> int:
        return len(self._futures)
