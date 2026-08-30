"""Worker 内部服务间认证依赖（websocket / scheduler 的 internal 路由）。

backend-web 转发主程序请求到 websocket / scheduler 的 internal 路由时，
要求携带 `X-Internal-Token` 且与 `SUB2API_INTERNAL_TOKEN` 匹配；
配置为空时失败关闭（拒绝所有 internal 请求），避免"内网可达即无鉴权"的静默降级。

注意：主程序→backend-web 走 `X-Worker-Token`（见 backend-web/app/api/deps.py
`get_service_or_user`）；本依赖用于 backend-web→websocket/scheduler 这一跳。
"""
from __future__ import annotations

import hmac

from fastapi import Header, HTTPException, status

from common.core.config import get_settings


async def require_internal_token(
    x_internal_token: str | None = Header(default=None, alias="X-Internal-Token"),
) -> None:
    """校验 internal 路由请求头 X-Internal-Token 匹配 SUB2API_INTERNAL_TOKEN。

    未配置 token 时失败关闭（401），不允许无鉴权运行；
    不匹配或缺失时同样 401。
    """
    settings = get_settings()
    expected = (getattr(settings, "sub2api_internal_token", "") or "").strip()
    if not expected:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="internal service token is not configured",
        )
    provided = (x_internal_token or "").strip()
    if not provided or not hmac.compare_digest(expected, provided):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="invalid internal service token",
        )
