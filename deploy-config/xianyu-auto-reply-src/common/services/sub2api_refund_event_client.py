"""
主程序退款事件上报客户端（Worker → Sub2API）

定时任务同步到「退款成功」订单后调用本模块，向主程序
`POST {sub2api_internal_base_url}/api/v1/internal/xianyu/refund-events`
上报退款事件。主程序按 order_no 幂等处置：未兑换码作废、已兑换码追回
（订阅扣天数 / 余额扣面值）。身份以 `X-Internal-Token`（= sub2api_internal_token）校验。

注意：仅当同时配置了 SUB2API_INTERNAL_BASE_URL 与 SUB2API_INTERNAL_TOKEN 时生效；
未配置时静默跳过（本地/单机模式不影响既有退款流程）。
"""
from __future__ import annotations

import asyncio

from loguru import logger

from common.core.config import BaseConfig


def _load_config() -> tuple[str, str]:
    try:
        from common.core.config import get_settings  # type: ignore
        settings = get_settings()
    except Exception:
        return "", ""
    base_url = str(getattr(settings, "sub2api_internal_base_url", "") or "").strip().rstrip("/")
    token = str(getattr(settings, "sub2api_internal_token", "") or "").strip()
    return base_url, token


def is_configured() -> bool:
    """是否已配置主程序内网回调（base_url + token 都非空）。"""
    base_url, token = _load_config()
    return bool(base_url and token)


async def report_refund_event(
    order_no: str,
    account_id: str,
    *,
    status: str = "refunded",
    timeout: float = 5.0,
    retries: int = 2,
) -> bool:
    """向主程序上报单条退款成功事件（带有限指数退避重试）。

    主程序侧幂等（claim 行 refund_handled_at 去重，重复上报回放既往处置结果），
    重复上报无副作用。

    Returns:
        是否成功送达主程序（重试耗尽仍非 2xx 或未配置返回 False，不抛异常）
    """
    base_url, token = _load_config()
    if not base_url or not token:
        return False
    if not order_no:
        return False

    payload = {"order_no": order_no, "account_id": account_id or "", "status": status}
    url = f"{base_url}/api/v1/internal/xianyu/refund-events"
    import httpx

    retry_count = 0
    while True:
        try:
            async with httpx.AsyncClient(timeout=timeout) as client:
                resp = await client.post(url, json=payload, headers={"X-Internal-Token": token})
            if 200 <= resp.status_code < 300:
                return True
            logger.warning(
                f"主程序退款事件上报失败 order_no={order_no} status={resp.status_code}: {resp.text[:200]}"
            )
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            logger.warning(f"主程序退款事件上报异常 order_no={order_no}: {type(e).__name__}: {e}")
        if retry_count >= retries:
            return False
        retry_count += 1
        delay = 0.5 * (2 ** (retry_count - 1))
        await asyncio.sleep(delay)


__all__ = ["is_configured", "report_refund_event"]
