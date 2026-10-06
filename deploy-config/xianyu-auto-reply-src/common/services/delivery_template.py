"""全局发货模板读取工具

发货消息模板对全系统统一生效（不逐卡券），存放在系统设置 `delivery.template` 中。
自动发货 / 手动补发 / 定时补发 / 提货四条路径都从这里取模板，保证行为一致。
模板为空时只发卡密本体（沿用「留空只发卡密本体」语义），不再使用卡券备注。
"""
from __future__ import annotations

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from common.models.system_setting import SystemSetting

# 全局发货模板的系统设置键
DELIVERY_TEMPLATE_SETTING_KEY = "delivery.template"
# 模板最大长度（与管理端 PUT 校验一致）
DELIVERY_TEMPLATE_MAX_LENGTH = 2000


async def get_global_delivery_template(session: AsyncSession) -> str:
    """读取全局发货模板；未配置或为空时返回空字符串（此时只发卡密本体）。"""
    try:
        stmt = select(SystemSetting.value).where(SystemSetting.key == DELIVERY_TEMPLATE_SETTING_KEY)
        value = (await session.execute(stmt)).scalar_one_or_none()
    except Exception:
        return ""
    return (value or "").strip() if isinstance(value, str) else ""
