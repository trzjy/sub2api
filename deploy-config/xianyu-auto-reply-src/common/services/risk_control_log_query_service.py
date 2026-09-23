"""
风控日志处理中状态查询服务。

功能：
1. 按账号判断是否已有处理中的风控任务
2. 查询失败时返回明确原因，由调用方按保守策略跳过重复处理
3. 数据库连接异常时自动重试
"""
from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import timedelta

from sqlalchemy import exists, select, update
from sqlalchemy.ext.asyncio import AsyncSession

from common.db.session import async_session_maker
from common.models.risk_control_log import XYRiskControlLog
from common.utils.time_utils import get_beijing_now_naive

# 风控日志 processing 超时兜底阈值（秒）：超过该时长仍未终态视为陈旧记录。
# 背景：滑块链路在并发线程饱和（Playwright 占用大量线程）时，compat 层
# update_risk_control_log 可能因 "can't start new thread" 静默失败，processing
# 记录永久卡住，导致 check_account_processing_risk_control_log 恒判定"处理中"，
# 账号 Token 刷新/滑块打码被永久短路（2026-09-23 生产事故根因）。
# 该阈值远大于单次人工打码上限（300s 契约），正常 processing 不会被误伤。
_STALE_PROCESSING_SECONDS = 3600


_ACCOUNT_RISK_CONTROL_LOCKS: dict[str, asyncio.Lock] = {}


def get_account_risk_control_lock(account_identifier: str) -> asyncio.Lock:
    """获取进程内账号级风控抢占锁。

    查询处理中状态和创建 processing 日志必须在同一把锁内完成，避免同一
    WebSocket 进程中的两个协程同时通过检查。

    Args:
        account_identifier: 账号业务标识。
    Returns:
        当前进程内该账号共用的异步锁。
    """
    clean_identifier = str(account_identifier or "").strip() or "__empty__"
    lock = _ACCOUNT_RISK_CONTROL_LOCKS.get(clean_identifier)
    if lock is None:
        lock = asyncio.Lock()
        _ACCOUNT_RISK_CONTROL_LOCKS[clean_identifier] = lock
    return lock


async def terminate_stale_processing_records(
    session: AsyncSession,
    *,
    account_identifier: str | None = None,
) -> int:
    """终结停留超过阈值的陈旧 processing 记录，返回终结条数。

    阈值与背景见模块头 _STALE_PROCESSING_SECONDS 注释。account_identifier
    给定时只终结该账号；为空时全表扫尾——供不按账号过滤的消费方（如定时续期
    token_renewal_task._load_candidates）在同一会话内先行扫尾，保证陈旧记录
    无论经哪条查询路径遇到都会被终结，不会把账号永久排除（外审 P3：同一卡死
    症状存在第二条消费路径）。
    """
    stale_cutoff = get_beijing_now_naive() - timedelta(
        seconds=_STALE_PROCESSING_SECONDS
    )
    conditions = [
        XYRiskControlLog.processing_status == "processing",
        XYRiskControlLog.created_at < stale_cutoff,
    ]
    if account_identifier:
        conditions.append(XYRiskControlLog.account_identifier == account_identifier)
    result = await session.execute(
        update(XYRiskControlLog)
        .where(*conditions)
        .values(
            processing_status="failed",
            processing_result=(
                f"超时兜底：processing 停留超过 {_STALE_PROCESSING_SECONDS // 3600} 小时"
                "未终态，自动终结（2026-09-23 修复）"
            ),
        )
    )
    return int(getattr(result, "rowcount", 0) or 0)


@dataclass(frozen=True, slots=True)
class ProcessingRiskControlCheckResult:
    """账号处理中风控日志检查结果。"""

    success: bool
    has_processing: bool
    message: str = ""


async def check_account_processing_risk_control_log(
    account_identifier: str,
    *,
    max_attempts: int = 3,
    retry_delay_seconds: float = 0.5,
) -> ProcessingRiskControlCheckResult:
    """检查指定账号是否已有处理中的风控日志。

    Args:
        account_identifier: 账号业务标识，对应风控日志 account_identifier。
        max_attempts: 数据库查询最大尝试次数。
        retry_delay_seconds: 相邻重试之间的等待秒数。
    Returns:
        查询成功时返回实际占用状态；查询失败时按保守策略返回占用。
    """
    clean_identifier = str(account_identifier or "").strip()
    if not clean_identifier:
        return ProcessingRiskControlCheckResult(
            False,
            True,
            "账号标识为空，无法检查处理中风控日志",
        )

    attempts = max(1, int(max_attempts))
    last_error = ""
    for attempt in range(1, attempts + 1):
        try:
            async with async_session_maker() as session:
                # 超时兜底：先终结本账号超过阈值的陈旧 processing 记录。
                # 用异步原生 SQL 更新（不经过 compat 层线程池，避免同类线程饱和
                # 场景下兜底自身也失败），再判定是否存在"有效"处理中任务。
                # 截断时间按全库同一口径 get_beijing_now_naive() - timedelta
                # （与 compat 清理风控日志的 cutoff 同源）；updated_at 不显式写，
                # 由模型 onupdate=func.now() 以服务端时钟落值。
                await terminate_stale_processing_records(
                    session, account_identifier=clean_identifier
                )
                await session.commit()
                has_processing = bool(
                    (
                        await session.execute(
                            select(
                                exists().where(
                                    XYRiskControlLog.account_identifier
                                    == clean_identifier,
                                    XYRiskControlLog.processing_status == "processing",
                                )
                            )
                        )
                    ).scalar()
                )
            return ProcessingRiskControlCheckResult(
                True,
                has_processing,
                (
                    "账号已有处理中的风控任务"
                    if has_processing
                    else "账号当前没有处理中的风控任务"
                ),
            )
        except Exception as exc:
            last_error = f"{type(exc).__name__}: {exc}"
            if attempt < attempts:
                await asyncio.sleep(max(0.0, retry_delay_seconds))

    return ProcessingRiskControlCheckResult(
        False,
        True,
        f"查询处理中风控日志失败，已重试{attempts}次：{last_error}",
    )
