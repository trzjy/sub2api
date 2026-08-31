"""
主程序内网服务路由（internal API）

供 Sub2API 主程序以 X-Worker-Token（= SUB2API_INTERNAL_TOKEN）调用。
所有端点复用现有 AccountService / ItemService / qr_login_manager，
不复制第二套账号、商品或登录存储。

注意：
- 本模块使用独立前缀 /api/v1/internal/...，仅限内网可达，避免与既有 /api/v1/... 路由冲突。
- 服务令牌是「高权限全局系统令牌」：绑定到现有管理员用户（owner 语义沿袭管理员全局
  作用域），唯一受信调用方是主程序。因此本路由不提供多租户隔离——这是明确的权威语义，
  不属于"跨租户越权"缺陷。对普通 JWT 用户回退到各自 owner 隔离仍有效。
- 账号/商品操作仍逐路由校验目标资源归属（管理员全局 = 显式授权）。
"""
from __future__ import annotations

import math
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, HTTPException, Query

from app.api import deps
from common.schemas.common import ApiResponse
from common.services.receipt_outcome import ReceiptOutcome, send_status_from_receipt
from common.utils.auth_scope import resolve_owner_scope

router = APIRouter(prefix="/internal", tags=["主程序内网服务"])


@router.get("/cookies/details")
async def internal_list_cookie_details(
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """账号投影列表（主程序账号同步用）：管理员可见全部账号。"""
    from app.services.account_service import AccountService

    owner_id, _ = resolve_owner_scope(service_user)
    accounts = await AccountService(session).list_accounts(owner_id)
    return ApiResponse(
        success=True,
        message="查询成功",
        data=[
            {
                "account_id": account.account_id,
                "nickname": account.display_name or account.account_id,
                "enabled": account.status not in {"inactive", "disabled", "suspended", "deleted"},
                "status": account.status,
                "cookie_status": account.cookie_status or "unknown",
                "remark": account.remark or "",
                "disable_reason": account.disable_reason or "",
                "last_login_at": account.last_login_at.isoformat() if account.last_login_at else None,
                "last_refresh_at": account.last_refresh_at.isoformat() if account.last_refresh_at else None,
            }
            for account in accounts
        ],
    )


@router.put("/cookies/{account_id}/status")
async def internal_update_account_status(
    account_id: str,
    payload: dict,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """账号启停（真实编排：更新 XYAccount 状态并启停 WebSocket 任务）。"""
    from app.services.account_service import AccountService
    from common.utils.auth_scope import resolve_owner_scope

    owner_id, _ = resolve_owner_scope(service_user)
    account_service = AccountService(session)
    account = await account_service.get_account_for_user(owner_id, account_id)
    if not account:
        raise HTTPException(status_code=404, detail="账号不存在")

    enabled = bool(payload.get("enabled", True))
    from app.api.routes.cookies import _update_account_status_and_task

    success, error_message = await _update_account_status_and_task(
        account, enabled, account_service
    )
    if not success:
        return ApiResponse(
            success=False,
            message=error_message or ("账号启动失败" if enabled else "账号停止失败"),
        )
    return ApiResponse(success=True, message="账号状态已更新", data={"account_id": account_id, "enabled": enabled})


@router.post("/cookies/renew-login")
async def internal_renew_account_login(
    payload: dict,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """Cookie 续期（复用既有 /api/v1/cookies/renew-login 的实现语义）。"""
    from app.api.routes.cookies import renew_account_login

    account_ids = payload.get("account_ids") or []
    if not isinstance(account_ids, list) or not account_ids:
        return ApiResponse(success=False, message="请选择至少一个账号")
    return await renew_account_login(account_ids=account_ids, current_user=service_user)


@router.post("/cookies/{account_id}/clear-credentials")
async def internal_clear_account_credentials(
    account_id: str,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """退出/清除凭证：停止 WebSocket 任务并删除 Worker 侧账号（含 Cookie）。

    主程序侧账号投影保留（关联商品/订单仍可追溯），仅标记为 disabled。
    """
    from app.services.account_service import AccountService
    from app.services.websocket_client import websocket_client
    from common.utils.auth_scope import resolve_owner_scope

    owner_id, _ = resolve_owner_scope(service_user)
    account_service = AccountService(session)
    account = await account_service.get_account_for_user(owner_id, account_id)
    if not account:
        raise HTTPException(status_code=404, detail="账号不存在")

    stop_result = await websocket_client.stop_account(account_id)
    if not isinstance(stop_result, dict) or not stop_result.get("success"):
        msg = (
            stop_result.get("message", "停止账号任务失败")
            if isinstance(stop_result, dict)
            else "停止账号任务失败"
        )
        raise HTTPException(status_code=502, detail=f"停止账号任务失败，未清除凭证: {msg}")
    await account_service.delete_account(account)
    return ApiResponse(success=True, message="账号凭证已清除", data={"account_id": account_id})


@router.get("/items/cookie/{cookie_id}")
async def internal_list_items_by_cookie(
    cookie_id: str,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """商品投影列表（按账号），主程序商品同步用。"""
    from app.services.account_service import AccountService
    from app.services.item_service import ItemService
    from common.utils.auth_scope import resolve_owner_scope

    owner_id, _ = resolve_owner_scope(service_user)
    account_service = AccountService(session)
    account = await account_service.get_account_for_user(owner_id, cookie_id)
    if not account:
        raise HTTPException(status_code=404, detail="账号不存在")

    items = await ItemService(session).list_items(owner_id, cookie_id)
    return ApiResponse(success=True, message="查询成功", data=items or [])


@router.post("/qr-login/generate")
async def internal_generate_qr_code(
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """生成扫码登录二维码，返回 session_id 与二维码地址。"""
    from app.services.qr_login import qr_login_manager

    result = await qr_login_manager.generate_qr_code()
    session_id = result.get("session_id")
    if result.get("success") and session_id:
        # 会话所有者映射到服务身份对应的管理员用户
        from app.api.routes.qr_login import SESSION_OWNER

        SESSION_OWNER[session_id] = service_user.id
        return ApiResponse(
            success=True,
            message="二维码生成成功",
            data={
                "session_id": session_id,
                "qr_code_url": result.get("qr_code_url"),
                "status": "waiting",
            },
        )
    return ApiResponse(success=False, message=result.get("message", "生成二维码失败"))


@router.get("/qr-login/status/{session_id}")
async def internal_get_qr_status(
    session_id: str,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """查询扫码会话状态（等待/已扫/成功/过期/失败，及可恢复 pending）。

    仅在会话所有者 / 已处理结果（含 owner 记录，支持响应丢失后的幂等重试）下放行；
    扫码成功后与既有 JWT 路由共用账号落库/WebSocket 启动处理链；
    响应做白名单投影，绝不返回 cookies/unb 等敏感字段。
    """
    from app.api.routes.qr_login import (
        PROCESSED_SESSIONS,
        SESSION_OWNER,
        _after_completion_response,
        _cleanup_session,
        process_qr_login_success,
        resolve_session_owner,
    )
    from app.services.qr_login import qr_login_manager

    # 统一授权边界（与 JWT 路由共用）：已处理结果(owner_id)/会话 owner 任一匹配，
    # 缺失或不匹配一律 403；不依赖已被清理的 SESSION_OWNER 单独判定。
    owner_id = resolve_session_owner(session_id, service_user.id)

    status_info = qr_login_manager.get_session_status(session_id) or {}
    status = status_info.get("status")

    if status == "success":
        result = await process_qr_login_success(
            session_id, owner_id=owner_id, db=session
        )
        final_status = result.get("status") or "success"
        if final_status == "already_processed":
            return _after_completion_response("success", result, status_info, session_id)
        # 非终态（waiting / pending）保持返回给前端继续轮询；失败/成功进入终态展示。
        if final_status in {"waiting", "pending"}:
            return ApiResponse(
                success=True,
                message=result.get("message") or "扫码登录处理中",
                data={
                    "status": "waiting" if final_status == "waiting" else "scanned",
                    "session_id": session_id,
                    "message": result.get("message"),
                    "account_id": result.get("account_id"),
                },
            )
        return ApiResponse(
            success=True,
            message=result.get("message") or ("扫码登录失败" if final_status == "failed" else "扫码登录成功"),
            data={
                "status": final_status if final_status != "already_processed" else "success",
                "session_id": session_id,
                "account_id": result.get("account_id"),
                "is_new_account": result.get("is_new_account", False),
                "message": result.get("message"),
            },
        )

    # 清理过期或取消的会话（与既有 JWT 路由语义一致）
    if status in {"expired", "cancelled", "not_found"}:
        _cleanup_session(session_id)
        PROCESSED_SESSIONS.pop(session_id, None)

    return ApiResponse(
        success=True,
        message="查询成功",
        data={
            "status": status_info.get("status"),
            "session_id": session_id,
            "message": status_info.get("message"),
            "verification_url": status_info.get("verification_url"),
            "face_qr_url": status_info.get("face_qr_url"),
        },
    )


@router.post("/messages/send")
async def internal_send_message(
    payload: dict,
    session = Depends(deps.get_db_session),
    service_user = Depends(deps.get_service_or_user),
) -> ApiResponse:
    """发送消息（补发原码用）：经 backend 转发到 WebSocket 服务的 send-message，
    携带 wait_result 以等待平台最终发送回执，避免把"已提交"误判为"已送达"。

    发送前校验目标账号归属，防止跨租户越权（普通前端用户不得操作他人账号）。
    """
    from app.services.account_service import AccountService
    from app.services.websocket_client import websocket_client
    from common.utils.auth_scope import resolve_owner_scope

    account_id = str(payload.get("account_id") or "").strip()
    chat_id = str(payload.get("chat_id") or "").strip()
    message = str(payload.get("message") or "").strip()
    # 收件人买家 ID：必须由调用方（主程序补发原码）传入真实 buyer_id，
    # 否则下游 actualReceivers 会被拼成 "None@goofish" 致收件人错乱。
    buyer_id = str(payload.get("buyer_id") or "").strip()
    if not account_id or not chat_id or not message or not buyer_id:
        return ApiResponse(
            success=False,
            message="account_id, chat_id, message, buyer_id 均不能为空",
        )

    owner_id, _ = resolve_owner_scope(service_user)
    try:
        account = await AccountService(session).get_account_for_user(owner_id, account_id)
    except Exception as exc:  # noqa: BLE001
        # 边界前异常（DB 超时/连接失败等）：确定未向 WebSocket 发出请求，统一 receipt=DDF。
        return ApiResponse(
            success=False,
            message=f"账号查询失败: {exc}",
            data={"receipt": ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE, "dispatched": False},
        )
    if not account:
        # 机器可判定的"确定未向 WebSocket 发出发送请求"，供主程序补发回滚 failed。
        return ApiResponse(
            success=False,
            message="账号不存在或无权操作",
            data={"receipt": ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE, "dispatched": False},
        )

    wait_result = bool(payload.get("wait_result", True))
    # wait_timeout 必须 clamp 到 [1, 30]，并拒绝 NaN/Inf/非法值，避免下游 coroutine 长期悬挂。
    try:
        raw_timeout = float(payload.get("wait_timeout", 10.0) or 10.0)
    except (TypeError, ValueError):
        raw_timeout = 10.0
    if not math.isfinite(raw_timeout):
        raw_timeout = 10.0
    wait_timeout = min(max(raw_timeout, 1.0), 30.0)

    # 透传补发尝试代次（attempt）与订单号（order_no），供发送回执关联与并发隔离。
    attempt = 0
    try:
        attempt = int(payload.get("attempt") or 0)
    except (TypeError, ValueError):
        attempt = 0
    order_no = str(payload.get("order_no") or "").strip()
    result = await websocket_client.send_message(
        account_id=account_id,
        chat_id=chat_id,
        content=message,
        wait_result=wait_result,
        wait_timeout=wait_timeout,
        attempt=attempt,
        order_no=order_no,
        buyer_id=buyer_id,
    )
    downstream = (result or {}).get("data") or {}
    # 统一按 receipt 归一化（缺失/未知回退 UNKNOWN_PENDING，绝不把无回执提升为成功）；
    # 同时派生 send_status / dispatched 兼容字段供下游/旧客户端回退。
    try:
        receipt = ReceiptOutcome(downstream.get("receipt") or "")
    except ValueError:
        receipt = ReceiptOutcome.UNKNOWN_PENDING
    # 兼容旧 Worker：仅旧 dispatched=false 信号（无 receipt）显式视为确定未 dispatch。
    if receipt == ReceiptOutcome.UNKNOWN_PENDING and downstream.get("dispatched") is False:
        receipt = ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE
    if not result or not result.get("success"):
        payload = {
            "receipt": receipt,
            "send_status": "failed" if receipt != ReceiptOutcome.UNKNOWN_PENDING else "unknown",
        }
        if receipt == ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE:
            payload["dispatched"] = False
        return ApiResponse(
            success=False,
            message=(result or {}).get("message") or "消息发送失败",
            data=payload,
        )
    return ApiResponse(
        success=True,
        message="消息发送成功",
        data={
            "receipt": receipt,
            "send_status": send_status_from_receipt(receipt),
            "send_fail_reason": downstream.get("send_fail_reason"),
        },
    )
