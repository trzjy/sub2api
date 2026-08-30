"""
主程序发货结果回传客户端（Worker → Sub2API）

Worker 在获得平台最终发送回执后调用本模块，向主程序
`POST {sub2api_internal_base_url}/api/v1/internal/xianyu/delivery-results`
回传发货结果。主程序以 `X-Internal-Token`（= sub2api_internal_token）校验身份，
并把发货记录标记为 sent（confirmed=true）或失败。

注意：仅当同时配置了 SUB2API_INTERNAL_BASE_URL 与 SUB2API_INTERNAL_TOKEN 时生效；
未配置时静默跳过（本地/单机模式不影响既有发货流程）。
"""
from __future__ import annotations

import asyncio
from typing import Any

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


async def report_delivery_result(
    order_no: str,
    success: bool,
    *,
    confirmed: bool = False,
    error: str | None = None,
    timeout: float = 5.0,
    retries: int = 2,
    attempt: int = 0,
) -> bool:
    """向主程序回传单条发货结果（带有限指数退避重试）。

    网络抖动 / 主程序重启 / 5xx / 超时都可能让单次回传失败；若只发一次，
    已扣库存订单会永久滞留 pending。这里对非 2xx 与异常做有限重试
    （默认额外重试 2 次，退避 0.5s/1s），显著降低回传丢失概率。

    Args:
        order_no: 订单号
        success: 业务是否成功（主程序用于区分发送失败）
        confirmed: 是否有最终平台发送回执；true 时才可标记 sent
        error: 失败原因（可选）
        timeout: 单次请求超时（秒）
        retries: 失败后的额外重试次数（默认 2，即最多尝试 3 次）
        attempt: 发送尝试代次（attempt_count）；自动发货默认 0，补发由调用方携带

    Returns:
        是否成功送达主程序（重试耗尽仍非 2xx 或未配置返回 False，不抛异常）
    """
    base_url, token = _load_config()
    if not base_url or not token:
        return False
    if not order_no:
        return False

    payload = {
        "order_no": order_no,
        "success": bool(success),
        "confirmed": bool(confirmed),
        "error": error,
        "attempt": int(attempt or 0),
    }
    url = f"{base_url}/api/v1/internal/xianyu/delivery-results"
    import httpx

    # retry_count 与补发代次 attempt 分离，避免变量遮蔽。
    retry_count = 0
    while True:
        try:
            async with httpx.AsyncClient(timeout=timeout) as client:
                resp = await client.post(
                    url,
                    json=payload,
                    headers={"X-Internal-Token": token},
                )
            if 200 <= resp.status_code < 300:
                return True
            logger.warning(
                f"主程序发货结果回传失败 order_no={order_no} status={resp.status_code}: {resp.text[:200]}"
            )
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            logger.warning(f"主程序发货结果回传异常 order_no={order_no}: {type(e).__name__}: {e}")
        if retry_count >= retries:
            return False
        retry_count += 1
        delay = 0.5 * (2 ** (retry_count - 1))
        await asyncio.sleep(delay)


async def report_delivery_result_fire_and_forget(
    order_no: str,
    success: bool,
    *,
    confirmed: bool = False,
    error: str | None = None,
    timeout: float = 5.0,
    attempt: int = 0,
) -> None:
    """后台回传（不阻塞发货主流程）：放入独立任务执行。"""
    try:
        await report_delivery_result(
            order_no, success, confirmed=confirmed, error=error, timeout=timeout, attempt=attempt
        )
    except asyncio.CancelledError:
        raise
    except Exception as e:
        logger.warning(f"主程序发货结果后台回传异常 order_no={order_no}: {type(e).__name__}: {e}")


async def report_delivery_result_by_receipt(
    order_no: str,
    *,
    reasons: list[str],
    any_confirmed: bool,
    any_timeout: bool,
    timeout: float = 5.0,
    attempt: int = 0,
) -> None:
    """按最终发送回执三态向主程序回传 delivery-results。

    语义与主程序 `RecordDeliveryResult` 对齐：
    - 明确拒绝（有拦截原因）→ success=false, confirmed=false + error（主程序标 failed）
    - 明确成功回执（confirmed 且无超时）→ success=true, confirmed=true（主程序标 sent）
    - 超时/异常（未拿到最终回执）→ confirmed=false（主程序保持 pending，最终转人工）

    Args:
        order_no: 订单号
        reasons: 平台拦截原因列表（明确拒绝）
        any_confirmed: 是否至少一条拿到明确成功回执
        any_timeout: 是否至少一条超时/异常（未拿到最终回执）
        timeout: 回传请求超时（秒）
        attempt: 发送尝试代次（attempt_count）；补发由调用方携带
    """
    if not order_no:
        return
    if reasons:
        await report_delivery_result_fire_and_forget(
            order_no, success=False, confirmed=False, error="；".join(reasons), timeout=timeout, attempt=attempt
        )
    elif any_confirmed and not any_timeout:
        await report_delivery_result_fire_and_forget(
            order_no, success=True, confirmed=True, timeout=timeout, attempt=attempt
        )
    else:
        # 全部超时/异常：未拿到最终回执，不确认，保持主程序 pending。
        await report_delivery_result_fire_and_forget(
            order_no, success=True, confirmed=False, error="发送后未收到平台最终回执（超时）", timeout=timeout, attempt=attempt
        )


async def ensure_delivery_record(
    order_no: str,
    quantity: int = 1,
    delivery_kind: str = "auto",
    timeout: float = 5.0,
) -> bool:
    """Worker 自动发货前向主程序注册订单级发货记录（按 order_no 幂等）。

    只注册订单级汇总（order_no/quantity/delivery_kind），不传卡券内容：
    Worker 保持本地库存与逐份发货实现，主程序只做订单级记录与结果回传关联。
    未配置 SUB2API_INTERNAL_BASE_URL/TOKEN 或调用失败时静默跳过（不阻断发货）。

    Returns:
        是否成功送达主程序（幂等创建/合并）。
    """
    base_url, token = _load_config()
    if not base_url or not token:
        return False
    if not order_no:
        return False
    url = f"{base_url}/api/v1/internal/xianyu/worker-deliveries"
    payload = {
        "order_no": order_no,
        "quantity": int(quantity or 1),
        "delivery_kind": delivery_kind or "auto",
    }
    import httpx
    try:
        async with httpx.AsyncClient(timeout=timeout) as client:
            resp = await client.post(url, json=payload, headers={"X-Internal-Token": token})
        if 200 <= resp.status_code < 300:
            return True
        logger.warning(
            f"注册 Worker 发货记录失败 order_no={order_no} status={resp.status_code}: {resp.text[:200]}"
        )
    except asyncio.CancelledError:
        raise
    except Exception as e:  # noqa: BLE001
        logger.warning(f"注册 Worker 发货记录异常 order_no={order_no}: {type(e).__name__}: {e}")
    return False


__all__ = [
    "is_configured",
    "report_delivery_result",
    "report_delivery_result_fire_and_forget",
    "report_delivery_result_by_receipt",
    "ensure_delivery_record",
]
