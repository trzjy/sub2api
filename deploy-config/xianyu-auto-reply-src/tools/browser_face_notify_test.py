import asyncio
import json
import os

from sqlalchemy import select

from common.models.notification_channel import NotificationChannel
from common.models.xy_account import XYAccount
from common.db.session import async_session_maker
from common.utils.notification_utils import send_webhook_notification


async def main() -> None:
    webhook_url = os.environ["BROWSER_FACE_NOTIFY_URL"]
    secret = "91f2e3cb3446d52736aec79220bd11e8"
    async with async_session_maker() as session:
        result = await session.execute(select(XYAccount).order_by(XYAccount.id))
        accounts = result.scalars().all()
        channels = (await session.execute(select(NotificationChannel))).scalars().all()
        channel = next((item for item in channels if item.config_payload.get("webhook_url") == webhook_url), None)
        if channel is None:
            channel = NotificationChannel(
                owner_id=1,
                name="local browser face",
                channel_type="webhook",
                config_payload={
                    "webhook_url": webhook_url,
                    "http_method": "POST",
                    "headers": json.dumps({"X-API-Key": secret}),
                },
                enabled=True,
            )
            session.add(channel)
            await session.flush()
        for account in accounts:
            message = (
                "闲鱼服务器浏览器人脸验证测试\n\n"
                f"闲鱼账号: {account.account_id}\n"
                "验证入口: https://corealgos.com/admin/xianyu/accounts"
            )
            sent = await send_webhook_notification(
                {
                    "webhook_url": webhook_url,
                    "http_method": "POST",
                    "headers": json.dumps({"X-API-Key": secret}),
                },
                message,
            )
            print(f"{account.account_id}: {sent}")
        await session.commit()


asyncio.run(main())
