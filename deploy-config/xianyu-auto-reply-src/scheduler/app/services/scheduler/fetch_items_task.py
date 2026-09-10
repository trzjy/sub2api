"""
定时获取闲鱼商品任务

功能：
1. 查询数据库中所有启用状态的账号
2. 逐个账号调用闲鱼商品列表API获取商品并 upsert 到数据库
3. 单个账号失败不影响其他账号
4. 低频全量轮：每隔 full_sync_interval_seconds（默认 86400，一天一次）跑一轮
   完整翻页同步。全量轮自然结束后，闲鱼「在售」列表即为权威集合，会清理投影中
   已不在售的商品行（售罄/下架），避免下架商品永久滞留面板。

并发说明：
- 商品入库统一复用公共 ItemService.fetch_all_items_from_account，
  该入口已内置账号级 Redis 互斥锁（item_sync:{account_id}），可避免「定时获取
  闲鱼商品任务」与「商品管理页手动触发同步」并发 upsert 同一商品；
- 即便 Redis 不可用导致降级无锁执行，也由 xy_catalog_items 的
  (account_id, item_id) 唯一约束 + 保存时的冲突重试做最终兜底，确保不会重复入库。
"""
from __future__ import annotations

import asyncio
import os
import time
from datetime import datetime

from loguru import logger
from sqlalchemy import select

from common.db.session import async_session_maker
from common.models.xy_account import XYAccount
from common.services.item_service import ItemService
from common.utils.cookie_refresh import is_account_session_cooled

# 全量轮间隔的环境变量（秒）。0 或负数 = 禁用全量轮（纯增量，旧行为）。
FULL_SYNC_INTERVAL_ENV = "FETCH_ITEMS_FULL_SYNC_INTERVAL_SECONDS"
DEFAULT_FULL_SYNC_INTERVAL_SECONDS = 86400


def _resolve_full_sync_interval_seconds() -> int:
    """解析全量轮间隔配置；非法值回退默认值，负数/0 表示禁用。"""
    raw = os.getenv(FULL_SYNC_INTERVAL_ENV, "")
    if not raw.strip():
        return DEFAULT_FULL_SYNC_INTERVAL_SECONDS
    try:
        return int(raw.strip())
    except ValueError:
        logger.warning(
            f"环境变量 {FULL_SYNC_INTERVAL_ENV}={raw!r} 不是整数，"
            f"回退默认 {DEFAULT_FULL_SYNC_INTERVAL_SECONDS}s"
        )
        return DEFAULT_FULL_SYNC_INTERVAL_SECONDS


class FetchItemsTaskService:
    """定时获取闲鱼商品任务服务"""

    def __init__(
        self,
        task_name: str = "定时获取闲鱼商品",
        page_size: int = 20,
        max_pages: int | None = None,
        full_sync_interval_seconds: int | None = None,
    ):
        """
        Args:
            task_name: 任务名称（日志前缀）
            page_size: 每页拉取数量
            max_pages: 最大拉取页数，None=按返回结果翻页直到结束
            full_sync_interval_seconds: 全量轮间隔（秒）。None=读环境变量；
                0 或负数 = 禁用全量轮。启动后首轮即全量（进程重启即清理一次）。
        """
        self.task_name = task_name
        self.page_size = page_size
        self.max_pages = max_pages
        if full_sync_interval_seconds is None:
            full_sync_interval_seconds = _resolve_full_sync_interval_seconds()
        self.full_sync_interval_seconds = full_sync_interval_seconds
        # None = 进程启动后尚未跑过全量轮（下一轮 execute 即全量）。
        # 用 monotonic 时钟，避免系统时间跳变导致间隔判定异常。
        self._last_full_sync_monotonic: float | None = None
        # 执行互斥：定时循环与「手动触发」共用本单例，可能并发调用 execute()。
        # 没有互斥时，两轮会在任一方标记完成前同时判定为全量，破坏低频全量保证。
        self._execute_lock = asyncio.Lock()

    def should_run_full_sync(self) -> bool:
        """判断本轮是否为全量同步轮。

        规则：全量轮被禁用（间隔<=0）时恒为 False；进程启动后首轮为 True
        （重启即清理一次存量滞留）；之后距上次全量轮超过间隔时为 True。
        """
        if self.full_sync_interval_seconds <= 0:
            return False
        if self._last_full_sync_monotonic is None:
            return True
        return (
            time.monotonic() - self._last_full_sync_monotonic
        ) >= self.full_sync_interval_seconds

    def mark_full_sync_done(self) -> None:
        """记录一次全量轮完成时间（无论个别账号成败，按轮计）。"""
        self._last_full_sync_monotonic = time.monotonic()

    async def execute(self):
        """执行获取商品任务（互斥串行：并发调用排队执行，不重叠）"""
        async with self._execute_lock:
            await self._execute_locked()

    async def _execute_locked(self):
        """实际执行逻辑；调用方需已持有 _execute_lock。"""
        full_sync = self.should_run_full_sync()
        mode_desc = (
            "全量翻页(含下架清理)"
            if full_sync
            else "增量(整页已存在提前停止)"
        )
        logger.info(
            f"【{self.task_name}】开始执行（本轮模式：{mode_desc}，"
            f"全量间隔={self.full_sync_interval_seconds}s）"
        )
        start_time = datetime.now()

        success_count = 0
        failed_count = 0
        skipped_count = 0
        total_fetched = 0
        total_saved = 0
        account_count = 0

        try:
            # 1. 查询所有启用状态的账号
            accounts = await self._get_active_accounts()

            if not accounts:
                logger.info(f"【{self.task_name}】没有启用状态的账号，任务结束")
                return

            account_count = len(accounts)
            logger.info(f"【{self.task_name}】查询到 {account_count} 个启用状态的账号")

            # 2. 逐个账号获取商品

            for account in accounts:
                # 检查账号是否处于Session过期冷却期内
                if is_account_session_cooled(account.account_id):
                    logger.info(
                        f"【{self.task_name}】账号 {account.account_id} "
                        f"处于Session过期冷却期内，跳过"
                    )
                    continue

                try:
                    result = await self._fetch_items_for_account(
                        account, stop_when_page_all_existing=not full_sync
                    )
                    if not result.get("success"):
                        failed_count += 1
                        logger.warning(
                            f"【{self.task_name}】账号 {account.account_id} "
                            f"获取商品失败: {result.get('message')}"
                        )
                    elif result.get("skipped"):
                        skipped_count += 1
                        logger.info(
                            f"【{self.task_name}】账号 {account.account_id} "
                            f"已有同步进行中，本次跳过"
                        )
                    else:
                        fetched = int(result.get("total_count") or 0)
                        saved = int(result.get("saved_count") or 0)
                        total_fetched += fetched
                        total_saved += saved
                        success_count += 1
                        logger.info(
                            f"【{self.task_name}】账号 {account.account_id} "
                            f"获取完成: 共{fetched}件, 保存{saved}件"
                        )
                except Exception as e:
                    failed_count += 1
                    logger.error(
                        f"【{self.task_name}】账号 {account.account_id} "
                        f"获取商品异常: {e}"
                    )

                # 账号间间隔2秒，避免请求过于密集
                if account is not accounts[-1]:
                    await asyncio.sleep(2)

            # 3. 记录执行结果
            elapsed = (datetime.now() - start_time).total_seconds()
            logger.info(
                f"【{self.task_name}】执行完成（模式：{mode_desc}），"
                f"账号: 成功{success_count}/失败{failed_count}/跳过{skipped_count}/共{account_count}, "
                f"商品: 获取{total_fetched}/保存{total_saved}, "
                f"耗时: {elapsed:.2f}秒"
            )

        except Exception as e:
            logger.error(f"【{self.task_name}】执行异常: {e}")
        finally:
            # 全量轮完成的最低成功条件：本轮至少一个账号成功获取。
            # 账号查询整体异常 / 全部失败 / 全部跳过（锁冲突或冷却）时不标记，
            # 下一轮（增量周期到来时）继续按全量重试，避免一轮完全无效的
            # 全量扫描把下架清理推迟整整一个周期。
            if full_sync and success_count > 0:
                self.mark_full_sync_done()

    async def _get_active_accounts(self) -> list:
        """获取所有启用状态的账号"""
        async with async_session_maker() as session:
            inactive_statuses = {"inactive", "disabled", "suspended", "deleted"}
            stmt = select(XYAccount).where(
                XYAccount.status.notin_(inactive_statuses)
            )
            result = await session.execute(stmt)
            return list(result.scalars().all())

    async def _fetch_items_for_account(
        self, account, stop_when_page_all_existing: bool = True
    ) -> dict:
        """获取单个账号的全部商品并入库（复用 ItemService 的加锁入口）

        增量同步策略（stop_when_page_all_existing=True）：当某一页商品在本地库中
        全部已存在（且无跳过项）时停止继续翻页。由于闲鱼「在售」列表默认按上架
        时间倒序（新品在前），首页全部已存在即说明无新上架商品，停止翻页是安全
        的，不会漏抓新品；首次同步或有新品时仍会按需翻页直至拉全，从而在保证数据
        完整的前提下大幅降低对闲鱼接口的请求量，规避风控风险。

        全量同步策略（stop_when_page_all_existing=False）：完整翻页直至自然结束，
        此时在售列表为权威集合，ItemService 会顺带清理投影中已不在售的商品行
        （售罄/下架）。注意：提前停止或达到最大页数时列表不完整，ItemService
        不会执行清理，因此增量轮不承担下架清理职责，由低频全量轮承担。
        """
        async with async_session_maker() as session:
            item_svc = ItemService(session)
            return await item_svc.fetch_all_items_from_account(
                account=account,
                page_size=self.page_size,
                max_pages=self.max_pages,
                stop_when_page_all_existing=stop_when_page_all_existing,
            )


# 全局实例
fetch_items_task_service = FetchItemsTaskService(
    task_name="定时获取闲鱼商品",
    page_size=20,
    max_pages=None,
)
