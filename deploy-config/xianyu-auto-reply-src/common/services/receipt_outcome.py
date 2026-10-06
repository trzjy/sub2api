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


def aggregate_receipts(results: list[tuple[str, str | None]]) -> tuple[list[str], bool, bool]:
    """从 (receipt, reason) 批次聚合出 (reasons, any_confirmed, any_timeout)。

    供自动发货批量回执判定与 delivery-results 三态回传共用：
    - REJECTED → reasons 追加拦截原因
    - SENT_EXPLICIT_SUCCESS → any_confirmed=True
    - UNKNOWN_PENDING → any_timeout=True（未拿到最终回执，主程序保持 pending）
    - DISPATCHED_DEFINITE_FAILURE → reasons 追加"发送前失败"
    """
    reasons: list[str] = []
    any_confirmed = False
    any_timeout = False
    for receipt_val, reason in results:
        receipt = ReceiptOutcome(receipt_val) if isinstance(receipt_val, str) else receipt_val
        if receipt == ReceiptOutcome.SENT_EXPLICIT_SUCCESS:
            any_confirmed = True
        elif receipt == ReceiptOutcome.REJECTED:
            reasons.append(reason or "平台明确拒绝")
        elif receipt == ReceiptOutcome.DISPATCHED_DEFINITE_FAILURE:
            reasons.append(reason or "发送前失败，确定未发出")
        else:  # UNKNOWN_PENDING
            any_timeout = True
    return reasons, any_confirmed, any_timeout
