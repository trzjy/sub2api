"""
FastAPI依赖注入模块

功能：
1. 提供数据库会话依赖
2. 提供各种服务类的依赖注入
3. 提供用户认证依赖（当前用户、活跃用户、管理员用户）
4. JWT令牌验证
"""
from __future__ import annotations

import hmac
from collections.abc import AsyncGenerator

from fastapi import Depends, HTTPException, Request, status
from fastapi.security import OAuth2PasswordBearer
from jose import JWTError
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.core.config import get_settings
from app.core.security import decode_token
from common.db.session import async_session_maker
from common.models import User, UserRole, UserStatus
from common.schemas.auth import TokenPayload

settings = get_settings()

oauth2_scheme = OAuth2PasswordBearer(tokenUrl=f"/api/v1/auth/token")


async def get_db_session() -> AsyncGenerator[AsyncSession, None]:
    """获取数据库会话"""
    async with async_session_maker() as session:
        yield session


async def _authenticate_jwt_token(token: str, session: AsyncSession) -> User:
    """解析并校验 JWT Bearer token，返回对应活跃用户（与 get_current_user 同语义）。"""
    credentials_exception = HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )
    try:
        payload = TokenPayload(**decode_token(token))
    except (JWTError, ValueError):
        raise credentials_exception
    if payload.sub is None:
        raise credentials_exception

    # 查询用户
    result = await session.execute(select(User).where(User.id == int(payload.sub)))
    user = result.scalar_one_or_none()

    if not user:
        raise credentials_exception
    if user.status != UserStatus.ACTIVE:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Inactive user")
    return user


async def get_current_user(
    token: str = Depends(oauth2_scheme),
    session: AsyncSession = Depends(get_db_session),
) -> User:
    """获取当前用户（JWT Bearer 认证）"""
    return await _authenticate_jwt_token(token, session)


async def get_current_active_user(current_user: User = Depends(get_current_user)) -> User:
    """获取当前活跃用户"""
    if current_user.status != UserStatus.ACTIVE:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Inactive user")
    return current_user


async def get_current_admin_user(current_user: User = Depends(get_current_active_user)) -> User:
    """获取当前管理员用户"""
    if current_user.role != UserRole.ADMIN:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Admin privileges required")
    return current_user


async def get_service_or_user(
    request: Request,
    session: AsyncSession = Depends(get_db_session),
) -> User:
    """主程序内网服务身份认证（复用现有用户/所有者语义）。

    优先校验 X-Worker-Token 头是否匹配 SUB2API_INTERNAL_TOKEN：
    - 匹配：解析为现有管理员用户（服务身份，owner 语义沿用该管理员）。
    - 不匹配/缺失：回退到既有 JWT 用户认证，不影响前端登录。

    不新建平行用户/账号模型；服务身份只在既有 User 表上解析。
    """
    internal_token = (settings.sub2api_internal_token or "").strip()
    if not internal_token:
        # 内网服务令牌未配置：internal 服务路由拒绝服务，绝不静默降级为普通 JWT 路由，
        # 避免"任何 active admin JWT 都能调用 internal API"的攻击面扩大。
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="internal service token is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    worker_token = (request.headers.get("X-Worker-Token") or "").strip()
    if worker_token:
        if hmac.compare_digest(internal_token, worker_token):
            result = await session.execute(
                select(User)
                .where(User.role == UserRole.ADMIN, User.status == UserStatus.ACTIVE)
                .order_by(User.id)
                .limit(1)
            )
            admin_user = result.scalar_one_or_none()
            if admin_user is None:
                raise HTTPException(
                    status_code=status.HTTP_403_FORBIDDEN,
                    detail="no active admin user to bind internal service identity",
                )
            return admin_user
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="invalid internal service token",
        )

    # 未携带服务 token：回退 JWT Bearer 认证，但仅允许管理员（internal API 的 owner 语义绑定管理员）。
    # 普通用户 JWT 不得调用内部控制接口，避免绕过服务间 token 扩大攻击面（缺失/非管理员一律失败关闭）。
    auth_header = request.headers.get("Authorization") or ""
    if auth_header.startswith("Bearer "):
        user = await _authenticate_jwt_token(auth_header[len("Bearer "):].strip(), session)
        if user.role != UserRole.ADMIN:
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail="Admin privileges required for internal API",
            )
        return user
    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )


# ==================== Service 依赖注入 ====================

async def get_user_service(session: AsyncSession = Depends(get_db_session)):
    """获取用户服务"""
    from app.services.user_service import UserService
    return UserService(session)


async def get_account_service(session: AsyncSession = Depends(get_db_session)):
    """获取账号服务"""
    from app.services.account_service import AccountService
    return AccountService(session)


async def get_item_service(session: AsyncSession = Depends(get_db_session)):
    """获取商品服务"""
    from app.services.item_service import ItemService
    return ItemService(session)


async def get_order_service(session: AsyncSession = Depends(get_db_session)):
    """获取订单服务"""
    from app.services.order_service import OrderService
    return OrderService(session)


async def get_card_service(session: AsyncSession = Depends(get_db_session)):
    """获取卡券服务"""
    from app.services.card_service import CardService
    return CardService(session)


async def get_default_reply_service(session: AsyncSession = Depends(get_db_session)):
    """获取默认回复服务"""
    from app.services.default_reply_service import DefaultReplyService
    return DefaultReplyService(session)


async def get_keyword_service(session: AsyncSession = Depends(get_db_session)):
    """获取关键词服务"""
    from app.services.keyword_service import KeywordService
    return KeywordService(session)


async def get_notification_channel_service(session: AsyncSession = Depends(get_db_session)):
    """获取通知渠道服务"""
    from app.services.notification_service import NotificationChannelService
    return NotificationChannelService(session)


async def get_system_setting_service(session: AsyncSession = Depends(get_db_session)):
    """获取系统设置服务"""
    from app.services.system_setting_service import SystemSettingService
    return SystemSettingService(session)



async def get_risk_log_service(session: AsyncSession = Depends(get_db_session)):
    """获取风控日志服务"""
    from app.services.risk_control_log_service import RiskControlLogService
    return RiskControlLogService(session)


async def get_account_login_log_service(session: AsyncSession = Depends(get_db_session)):
    """获取账号登录日志服务"""
    from app.services.account_login_log_service import AccountLoginLogService
    return AccountLoginLogService(session)


async def get_db_backup_log_service(session: AsyncSession = Depends(get_db_session)):
    """获取数据库备份日志服务"""
    from app.services.db_backup_log_service import DbBackupLogService
    return DbBackupLogService(session)


async def get_ai_reply_service(session: AsyncSession = Depends(get_db_session)):
    """获取AI回复设置服务"""
    from app.services.ai_reply_service import AIReplySettingsService
    return AIReplySettingsService(session)


async def get_ai_conversation_service(session: AsyncSession = Depends(get_db_session)):
    """获取AI对话服务"""
    from app.services.ai_conversation_service import AIConversationService
    return AIConversationService(session)


async def get_auth_service(session: AsyncSession = Depends(get_db_session)):
    """获取认证服务"""
    from app.services.auth import AuthService
    return AuthService(session)


async def get_message_notification_service(session: AsyncSession = Depends(get_db_session)):
    """获取消息通知服务"""
    from app.services.notification_service import MessageNotificationService
    return MessageNotificationService(session)


async def get_blacklist_service(session: AsyncSession = Depends(get_db_session)):
    """获取黑名单服务"""
    from app.services.blacklist_service import BlacklistService
    return BlacklistService(session)


async def get_card_dock_service(session: AsyncSession = Depends(get_db_session)):
    """获取分销卡券对接服务"""
    from app.services.card_dock_service import CardDockService
    return CardDockService(session)
