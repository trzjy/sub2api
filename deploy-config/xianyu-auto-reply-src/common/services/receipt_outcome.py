"""统一发送回执枚举（基座层单一回执语义）。

取代此前分散的三套编码：
- dispatched 布尔（发送前确定性）
- send_status 三态（success/failed/unknown，发送后回执）
- (success, confirmed) 二元组（delivery-results 回传）

单一枚举使"发送前失败 / 明确送达 / 平台拒绝 / 未拿到回执"四态有机器可判定的
唯一表示，同步响应与异步回传都由它派生，消除"confirmed=false 无法区分 failed 与 unknown"
等编码缺陷。
"""
from __future__ import annotations

from enum import Enum


class ReceiptOutcome(str, Enum):
    # 发送前失败，确定未向平台发出（主程序补发应回滚 failed，可安全重试）。
    DISPATCHED_DEFINITE_FAILURE = "dispatched_definite_failure"
    # 平台明确成功（LWP code == 200，主程序标记 sent）。
    SENT_EXPLICIT_SUCCESS = "sent_explicit_success"
    # 平台明确拦截（有 reason，如 CSI_FORBID；主程序标记 failed）。
    REJECTED = "rejected"
    # 已发出但无最终回执（超时/无明确结果；主程序保持 pending，不得重试发货）。
    UNKNOWN_PENDING = "unknown_pending"


def delivery_result_payload_from_receipt(receipt: str, reason: str | None = None) -> dict:
    """把单一回执枚举派生为 delivery-results wire 载荷（主程序契约不变）。

    语义对齐主程序 RecordDeliveryResult：
    - DISPATCHED_DEFINITE_FAILURE → success=false, confirmed=false + error（主程序标 failed）
    - SENT_EXPLICIT_SUCCESS       → success=true, confirmed=true（主程序标 sent）
    - REJECTED                    → success=false, confirmed=false + error（主程序标 failed）
    - UNKNOWN_PENDING             → success=true, confirmed=false + error（主程序保持 pending）
    """
    receipt = ReceiptOutcome(receipt) if isinstance(receipt, str) else receipt
    if receipt == ReceiptOutcome.SENT_EXPLICIT_SUCCESS:
        return {"success": True, "confirmed": True, "error": None}
    if receipt == ReceiptOutcome.UNKNOWN_PENDING:
        return {"success": True, "confirmed": False, "error": reason or "发送后未收到明确成功回执"}
    if receipt == ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE:
        return {"success": False, "confirmed": False, "error": reason or "发送前失败，确定未发出"}
    # REJECTED
    return {"success": False, "confirmed": False, "error": reason or "平台明确拒绝"}


def send_status_from_receipt(receipt: str) -> str:
    """从统一回执派生过渡兼容的 send_status 三态（SENT→success、REJECTED→failed、其余→unknown）。"""
    receipt = ReceiptOutcome(receipt) if isinstance(receipt, str) else receipt
    if receipt == ReceiptOutcome.SENT_EXPLICIT_SUCCESS:
        return "success"
    if receipt == ReceiptOutcome.REJECTED:
        return "failed"
    return "unknown"


def coerce_receipt(value) -> ReceiptOutcome:
    """把任意回执取值收敛为枚举；无法识别时失败关闭为 UNKNOWN_PENDING。

    UNKNOWN_PENDING 是唯一安全的兜底：它既不谎称送达，也不谎称确定未发出，
    主程序保持 pending 并由超时兜底转人工，不会触发重复发货。
    """
    if isinstance(value, ReceiptOutcome):
        return value
    try:
        return ReceiptOutcome(value)
    except (ValueError, TypeError):
        return ReceiptOutcome.UNKNOWN_PENDING


def receipt_of_send_result(result) -> tuple[ReceiptOutcome, str | None]:
    """从单条 send_result 推导"无需等待 Future"时的回执枚举。

    仅用于没有 send_future 可等待的子消息（发送前失败、返回值异常等）。
    判定顺序按证据强度：

    1. 非 dict（返回值异常）→ UNKNOWN_PENDING（无法证明未发出）
    2. 显式 receipt 字段 → 直接采用（send_msg/send_image_msg 的单一事实源）
    3. dispatched is False → DISPATCHED_DEFINITE_FAILURE
       （发送前失败的历史信号，如"图片上传到CDN失败""本地图片文件不存在"，
       这些分支确定未进入 websocket.send）
    4. success is True 但无 receipt → UNKNOWN_PENDING（已发出、无最终回执）
    5. 其余失败 → UNKNOWN_PENDING

    第 5 条是刻意保守：没有明确证据时绝不凭空判定"确定未发出"，
    否则会把"可能已送达"的订单错误标 failed 并触发重复发货。
    """
    if not isinstance(result, dict):
        return ReceiptOutcome.UNKNOWN_PENDING, "发送返回值异常，无法判定是否已送达"
    reason = result.get("error_message") or None
    if result.get("receipt") is not None:
        return coerce_receipt(result.get("receipt")), reason
    if result.get("dispatched") is False:
        return ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE, reason or "发送前失败，确定未发出"
    if result.get("success") is True:
        return ReceiptOutcome.UNKNOWN_PENDING, None
    return ReceiptOutcome.UNKNOWN_PENDING, reason or "发送失败且无法判定是否已送达"


def collapse_card_receipt(
    sub_outcomes: list[tuple[ReceiptOutcome, str | None]]
) -> tuple[ReceiptOutcome, str | None]:
    """把"一份卡券"的全部子消息回执折叠为该份卡券的单一回执。

    一份卡券 = delivery_contents 中的一项 = N 个子消息（多图、图文、###### 分段），
    买家只有拿到全部子消息才算收到这份卡券，故折叠规则按"整份可用性"判定：

    - 空组（没有任何子消息发出）→ DISPATCHED_DEFINITE_FAILURE。
      格式错误、发送前异常都会留下空组，绝不能算成功一份。
    - 全部 SENT_EXPLICIT_SUCCESS → SENT_EXPLICIT_SUCCESS（这份确定送达）
    - 全部落在 {DDF, REJECTED} → 这份确定没送达：有 REJECTED 取 REJECTED，否则 DDF
    - 其余（任一 UNKNOWN_PENDING，或"部分成功 + 部分确定失败"的半送达）
      → UNKNOWN_PENDING：这份卡券状态不可判定，订单必须保持 pending 转人工，
      既不能标 sent（买家没拿全），也不能标 failed（部分内容已发出，重发会重复发货）。
    """
    if not sub_outcomes:
        return ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE, "该份卡券没有任何子消息发出"

    outcomes = [coerce_receipt(o) for o, _ in sub_outcomes]
    reasons = {}
    for outcome, reason in sub_outcomes:
        key = coerce_receipt(outcome)
        if reason and key not in reasons:
            reasons[key] = reason

    if all(o == ReceiptOutcome.SENT_EXPLICIT_SUCCESS for o in outcomes):
        return ReceiptOutcome.SENT_EXPLICIT_SUCCESS, None

    definite_failures = {
        ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE,
        ReceiptOutcome.REJECTED,
    }
    if all(o in definite_failures for o in outcomes):
        if ReceiptOutcome.REJECTED in outcomes:
            return ReceiptOutcome.REJECTED, reasons.get(ReceiptOutcome.REJECTED) or "平台明确拒绝"
        return (
            ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE,
            reasons.get(ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE) or "发送前失败，确定未发出",
        )

    # 混合态：任一未知，或"成功 + 确定失败"的半送达。
    if ReceiptOutcome.UNKNOWN_PENDING in outcomes:
        return ReceiptOutcome.UNKNOWN_PENDING, reasons.get(ReceiptOutcome.UNKNOWN_PENDING)
    return ReceiptOutcome.UNKNOWN_PENDING, "该份卡券部分子消息已发出、部分确定失败，无法判定整份是否可用"


def aggregate_card_outcomes(
    card_outcomes: list[tuple[ReceiptOutcome, str | None]]
) -> tuple[list[str], bool, bool, int]:
    """把"每份卡券一个回执"聚合为订单级回传四元组。

    Args:
        card_outcomes: 每份卡券折叠后的 (receipt, reason)，顺序即卡券顺序。

    Returns:
        (reasons, any_confirmed, any_timeout, confirmed_cards)
        - reasons: 确定失败（DDF/REJECTED）的原因列表；不含未知态
        - any_confirmed: 是否至少一份确定送达
        - any_timeout: 是否至少一份状态未知（订单必须保持 pending）
        - confirmed_cards: 确定送达的卡券份数（即 quantity_sent）
    """
    reasons: list[str] = []
    any_confirmed = False
    any_timeout = False
    confirmed_cards = 0
    for outcome, reason in card_outcomes:
        receipt = coerce_receipt(outcome)
        if receipt == ReceiptOutcome.SENT_EXPLICIT_SUCCESS:
            any_confirmed = True
            confirmed_cards += 1
        elif receipt == ReceiptOutcome.REJECTED:
            reasons.append(reason or "平台明确拒绝")
        elif receipt == ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE:
            reasons.append(reason or "发送前失败，确定未发出")
        else:  # UNKNOWN_PENDING
            any_timeout = True
    return reasons, any_confirmed, any_timeout, confirmed_cards
