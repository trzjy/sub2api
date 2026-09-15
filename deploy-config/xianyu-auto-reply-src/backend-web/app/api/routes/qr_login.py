"""
二维码扫码登录路由

提供二维码生成、状态查询和Cookie获取接口
"""
from __future__ import annotations

import asyncio
from typing import Dict

from fastapi import APIRouter, Depends, HTTPException
from loguru import logger
from sqlalchemy.ext.asyncio import AsyncSession

from app.api import deps
from app.core.http_client import get_http_client
from app.services.account_service import AccountService
from app.services.qr_login import qr_login_manager
from app.api.deps import get_db_session as get_db
from common.models.user import User
from common.schemas.common import ApiResponse
from common.services.account_limit_service import AccountLimitExceededError

router = APIRouter(prefix="/qr-login", tags=["二维码登录"])

# 会话所有者映射
SESSION_OWNER: Dict[str, int] = {}

# 已处理的会话记录，防止重复处理
PROCESSED_SESSIONS: Dict[str, Dict] = {}

# 会话处理锁，防止并发处理同一个会话
SESSION_LOCKS: Dict[str, asyncio.Lock] = {}

# WebSocket 任务启动重试上限：达到后 pending 转为 failed，避免无限循环
QR_TASK_START_MAX_ATTEMPTS = 10


def _get_session_lock(session_id: str) -> asyncio.Lock:
    """获取会话锁"""
    if session_id not in SESSION_LOCKS:
        SESSION_LOCKS[session_id] = asyncio.Lock()
    return SESSION_LOCKS[session_id]


def _cleanup_session(session_id: str):
    """清理会话相关数据"""
    SESSION_OWNER.pop(session_id, None)
    SESSION_LOCKS.pop(session_id, None)


def _build_processed_response(processed_info: dict) -> ApiResponse:
    processed_status = processed_info.get("status")
    processed_message = processed_info.get("message") or (
        "扫码登录失败" if processed_status == "failed" else "扫码登录已完成"
    )
    return ApiResponse(
        success=True,
        message=processed_message,
        data={
            "status": "failed" if processed_status == "failed" else "already_processed",
            "message": processed_message,
            "account_info": {
                "account_id": processed_info.get("account_id"),
                "is_new_account": bool(processed_info.get("is_new_account", False)),
            },
        },
    )


async def process_qr_login_success(
    session_id: str,
    owner_id: int,
    db: AsyncSession,
) -> dict:
    """消费扫码成功会话：落库账号并启动/重启 WebSocket 任务（可恢复分阶段、幂等）。

    供既有 JWT 扫码状态路由与主程序内部扫码状态路由共同调用，保证两路径
    的账号创建、任务启动与幂等语义一致。返回非终态时调用方应继续轮询
    （不要据此判定失败结束）。

    阶段语义：
    - waiting            Cookie 尚未就绪（非终态）：调用方应保持 waiting 继续轮询
    - pending            账号已落库，但 WebSocket 任务尚未确认启动（非终态）：
                         调用方应保持轮询，下次会幂等重试启动；重试达上限后转为 failed
    - success            账号已落库且 WebSocket 任务已确认启动（终态）
    - already_processed  该会话已成功处理过（幂等重试命中，终态）
    - failed             明确失败（终态，如账号数量超限 / 任务启动重试达上限）

    Args:
        session_id: 扫码会话 ID
        owner_id: 账号归属用户 ID（JWT 用户或服务身份管理员）
        db: 数据库会话

    Returns:
        结果字典：{status, message, account_id, is_new_account, account_unique}
    """
    lock = _get_session_lock(session_id)
    async with lock:
        # 已处理过：优先返回持久化的已处理结果（含 owner，供响应丢失后的幂等重试）
        processed_info = PROCESSED_SESSIONS.get(session_id)
        if processed_info:
            stored_status = processed_info.get("status")
            if stored_status == "failed":
                return {
                    "status": "failed",
                    "message": processed_info.get("message") or "扫码登录失败",
                    "account_id": processed_info.get("account_id") or "",
                    "is_new_account": True,
                    "account_unique": True,
                }
            if stored_status == "success":
                return {
                    "status": "already_processed",
                    "message": "扫码登录已完成",
                    "account_id": processed_info.get("account_id") or "",
                    "is_new_account": bool(processed_info.get("is_new_account", False)),
                    "account_unique": True,
                }
            # pending：账号已落库但任务未确认，继续尝试启动（带重试上限）

        cookies_info = qr_login_manager.get_session_cookies(session_id)
        if not cookies_info:
            # Cookie 尚未写入：非终态，返回等待轮询，不得判定为失败
            return {
                "status": "waiting",
                "message": "登录凭证写入中",
                "account_id": "",
                "is_new_account": True,
                "account_unique": False,
            }

        cookies_str = cookies_info.get("cookies", "")
        unb = cookies_info.get("unb")

        try:
            account_service = AccountService(db)
            account, is_new_account = await account_service.upsert_account_from_qr(
                owner_id=owner_id,
                cookies=cookies_str,
                unb=unb,
            )
        except AccountLimitExceededError as exc:
            message = str(exc)
            PROCESSED_SESSIONS[session_id] = {
                "status": "failed",
                "message": message,
                "account_id": unb or "",
                "is_new_account": True,
                "owner_id": owner_id,
            }
            _cleanup_session(session_id)
            return {
                "status": "failed",
                "message": message,
                "account_id": unb or "",
                "is_new_account": True,
                "account_unique": True,
            }

        logger.info(
            f"扫码登录：{'创建新账号' if is_new_account else '更新现有账号'} {account.account_id}"
        )

        # 调用 WebSocket 服务启动账号任务
        task_started = False
        try:
            from app.core.config import get_settings
            settings = get_settings()
            client = get_http_client()

            # 构建请求参数
            request_data = {
                "cookie_value": cookies_str,
                "user_id": owner_id
            }

            if is_new_account:
                # 新账号：启动任务
                response = await client.post(
                    f"{settings.websocket_service_url}/internal/accounts/{account.account_id}/start",
                    json=request_data
                )
                if response.get("success"):
                    logger.info(f"扫码登录：新账号WebSocket任务已启动 {account.account_id}")
                    task_started = True
                else:
                    logger.warning(f"扫码登录：启动WebSocket任务失败 {account.account_id}: {response.get('message')}")
            else:
                # 现有账号：重启任务
                response = await client.post(
                    f"{settings.websocket_service_url}/internal/accounts/{account.account_id}/restart",
                    json=request_data
                )
                if response.get("success"):
                    logger.info(f"扫码登录：现有账号WebSocket任务已重启 {account.account_id}")
                    task_started = True
                else:
                    logger.warning(f"扫码登录：重启WebSocket任务失败 {account.account_id}: {response.get('message')}")
        except Exception as ws_e:
            logger.error(f"扫码登录：调用WebSocket服务失败 {account.account_id}: {str(ws_e)}")

        if not task_started:
            # 账号已落库但任务未启动成功：保留可恢复 pending 状态（不清除 SESSION_OWNER），
            # 主程序/前端持续轮询时会幂等重试启动；达到重试上限后转为失败，避免无限循环。
            attempts = int((PROCESSED_SESSIONS.get(session_id) or {}).get("attempts", 0)) + 1
            if attempts >= QR_TASK_START_MAX_ATTEMPTS:
                PROCESSED_SESSIONS[session_id] = {
                    "status": "failed",
                    "message": f"账号已登录，但任务启动失败（已重试 {attempts} 次）",
                    "account_id": account.account_id,
                    "is_new_account": is_new_account,
                    "owner_id": owner_id,
                }
                _cleanup_session(session_id)
                return {
                    "status": "failed",
                    "message": "账号已登录，但任务启动失败，请在账号管理手动启用",
                    "account_id": account.account_id,
                    "is_new_account": is_new_account,
                    "account_unique": True,
                }
            PROCESSED_SESSIONS[session_id] = {
                "status": "pending",
                "account_id": account.account_id,
                "is_new_account": is_new_account,
                "owner_id": owner_id,
                "attempts": attempts,
            }
            return {
                "status": "pending",
                "message": "账号已登录，任务启动中，请稍候",
                "account_id": account.account_id,
                "is_new_account": is_new_account,
                "account_unique": True,
            }

        # 记录已处理（携带 owner 供响应丢失后的幂等重试）
        PROCESSED_SESSIONS[session_id] = {
            "status": "success",
            "account_id": account.account_id,
            "is_new_account": is_new_account,
            "owner_id": owner_id,
        }

        _cleanup_session(session_id)

        return {
            "status": "success",
            "message": "扫码登录成功",
            "account_id": account.account_id,
            "is_new_account": is_new_account,
            "account_unique": True,
        }


@router.post("/generate")
async def generate_qr_code(
    current_user: User = Depends(deps.get_current_active_user),
) -> ApiResponse:
    """
    生成二维码

    返回二维码图片的Base64编码和会话ID
    """
    try:
        result = await qr_login_manager.generate_qr_code()
        session_id = result.get("session_id")

        if result.get("success") and session_id:
            SESSION_OWNER[session_id] = current_user.id
            logger.info(f"二维码生成成功: session_id={session_id}, user_id={current_user.id}")
            return ApiResponse(
                success=True,
                message="二维码生成成功",
                data={
                    "session_id": session_id,
                    "qr_code_url": result.get("qr_code_url"),
                }
            )
        else:
            error_msg = result.get("message", "生成二维码失败")
            logger.error(f"二维码生成失败: {error_msg}")
            return ApiResponse(
                success=False,
                message=error_msg,
            )
    except Exception as e:
        logger.exception("生成二维码时发生异常")
        return ApiResponse(
            success=False,
            message=f"生成二维码失败: {str(e)}",
        )


def resolve_session_owner(session_id: str, current_user_id: int) -> int:
    """解析扫码会话归属 owner（JWT 与内部路由共享的统一授权检查）。

    - 优先使用已处理结果（PROCESSED_SESSIONS）中持久化的 owner_id；
    - 否则使用 SESSION_OWNER（生成会话时绑定）；
    - 缺失或不匹配一律抛 403，禁止回退到调用方自己的 id（防跨会话越权）。

    Returns:
        会话归属 owner_id（此时必然等于 current_user_id）
    """
    processed = PROCESSED_SESSIONS.get(session_id)
    stored_owner = processed.get("owner_id") if processed else None
    owner = stored_owner if stored_owner is not None else SESSION_OWNER.get(session_id)
    if owner is None or owner != current_user_id:
        raise HTTPException(status_code=403, detail="无权访问该扫码会话")
    return owner


def _after_completion_response(status: str, processed: dict, status_info: dict, session_id: str) -> ApiResponse:
    """按最终状态组装扫码状态响应（JWT 路由使用，保证 pending/waiting 为可轮询非终态）。"""
    if status == "success":
        return ApiResponse(
            success=True,
            message="扫码登录成功",
            data={
                "status": "success",
                "account_info": {
                    "account_id": processed.get("account_id"),
                    "is_new_account": bool(processed.get("is_new_account", False)),
                }
            }
        )
    if status == "pending":
        # 账号已落库但任务启动中：保持轮询（非终态），不要误判为失败。
        return ApiResponse(
            success=True,
            message="扫码登录成功，任务启动中",
            data={
                "status": "scanned",
                "account_info": {
                    "account_id": processed.get("account_id"),
                    "is_new_account": bool(processed.get("is_new_account", False)),
                }
            }
        )
    if status == "waiting":
        # Cookie 未就绪：保持轮询（非终态）。
        return ApiResponse(
            success=True,
            message="登录凭证写入中",
            data=status_info
        )
    if status == "failed":
        return ApiResponse(
            success=True,
            message=processed.get("message") or "扫码登录失败",
            data={
                "status": "failed",
                "message": processed.get("message") or "扫码登录失败",
                "account_info": {
                    "account_id": processed.get("account_id") or "",
                    "is_new_account": bool(processed.get("is_new_account", True)),
                }
            }
        )
    # already_processed：返回已处理响应（PROCESSED_SESSIONS 在调用点保证存在）
    return _build_processed_response(PROCESSED_SESSIONS.get(session_id, {}))


@router.get("/status/{session_id}")
async def get_qr_status(
    session_id: str,
    current_user: User = Depends(deps.get_current_active_user),
    db: AsyncSession = Depends(get_db),
) -> ApiResponse:
    """
    查询扫码状态

    状态说明：
    - waiting: 等待扫码
    - scanned: 已扫码，等待确认
    - success: 扫码成功
    - expired: 二维码已过期
    - cancelled: 用户取消登录
    - verification_required: 需要手机验证
    - not_found: 会话不存在
    - already_processed: 已处理过
    """
    try:
        # 统一授权：已处理结果持久化 owner / 会话 owner，缺失或不匹配一律 403。
        owner_id = resolve_session_owner(session_id, current_user.id)

        status_info = qr_login_manager.get_session_status(session_id)
        status = status_info.get("status")

        # 如果扫码成功，自动创建或更新账号（单路径：与主程序内部路由共用共享处理函数）
        if status == "success":
            processed = await process_qr_login_success(session_id, owner_id=owner_id, db=db)
            processed_status = processed.get("status")
            if processed_status == "already_processed":
                # 已处理成功/失败：返回持久化结果（"已处理完成"语义）
                return _build_processed_response(PROCESSED_SESSIONS.get(session_id, {}))
            return _after_completion_response(processed_status, processed, status_info, session_id)

        # 清理过期或取消的会话
        if status in {"expired", "cancelled", "not_found"}:
            _cleanup_session(session_id)
            PROCESSED_SESSIONS.pop(session_id, None)

        return ApiResponse(
            success=True,
            message="查询成功",
            data=status_info
        )

    except HTTPException:
        # 授权类异常（未知 session / 非 owner / 已处理结果 owner 不匹配）必须真实 403，
        # 不得被通用捕获降级为 HTTP 200 业务失败，否则与内部路由授权边界不一致。
        raise
    except Exception as e:
        logger.exception(f"查询扫码状态时发生异常: {session_id}")
        return ApiResponse(
            success=False,
            message=f"查询失败: {str(e)}",
        )


@router.get("/cookie/{session_id}")
async def get_qr_cookie(
    session_id: str,
    current_user: User = Depends(deps.get_current_active_user),
) -> ApiResponse:
    """
    获取登录Cookie

    仅在扫码成功后可用
    """
    try:
        cookies_info = qr_login_manager.get_session_cookies(session_id)

        if cookies_info:
            return ApiResponse(
                success=True,
                message="获取Cookie成功",
                data=cookies_info
            )
        else:
            return ApiResponse(
                success=False,
                message="Cookie不存在或会话未完成",
            )
    except Exception as e:
        logger.exception(f"获取Cookie时发生异常: {session_id}")
        return ApiResponse(
            success=False,
            message=f"获取Cookie失败: {str(e)}",
        )
