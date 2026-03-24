from __future__ import annotations

from datetime import datetime
from typing import Any, Dict, Optional
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

from fastapi import HTTPException, Request
from fastapi.responses import RedirectResponse

from app.config import Settings
from app.services.helcim_recurring import HelcimRecurringClient
from app.services.subscriptions import SubscriptionService


def passthrough_query_params(request: Request) -> Dict[str, Any]:
    query_dict: Dict[str, Any] = {}
    for key, value in request.query_params.multi_items():
        if key in query_dict:
            existing = query_dict[key]
            if isinstance(existing, list):
                existing.append(value)
            else:
                query_dict[key] = [existing, value]
        else:
            query_dict[key] = value
    return query_dict


def enrich_payment_plan_with_firestore_features(
    plan_payload: Any, subscription_service: SubscriptionService
) -> Any:
    if not isinstance(plan_payload, dict):
        return plan_payload
    payment_plan_id = plan_payload.get("id")
    firestore_plan = subscription_service.find_plan_by_payment_plan_id(payment_plan_id)
    features = subscription_service.plan_features(firestore_plan)
    return {**plan_payload, "features": features}


def extract_transaction_payload(response_payload: Any) -> Dict[str, Any]:
    if not isinstance(response_payload, dict):
        return {}
    data = response_payload.get("data")
    if isinstance(data, dict):
        return data
    if isinstance(data, list) and data:
        first = data[0]
        if isinstance(first, dict):
            return first
    return response_payload


def parse_helcim_transaction_datetime(
    callback_payload: Dict[str, Any], transaction_payload: Dict[str, Any]
) -> Optional[datetime]:
    date_created = transaction_payload.get("dateCreated")
    if isinstance(date_created, str):
        for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d"):
            try:
                return datetime.strptime(date_created, fmt)
            except ValueError:
                continue
    callback_date = callback_payload.get("date")
    callback_time = callback_payload.get("time")
    if isinstance(callback_date, str) and callback_date.strip():
        text = callback_date.strip()
        if isinstance(callback_time, str) and callback_time.strip():
            text = f"{text} {callback_time.strip()}"
            for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d %H:%M"):
                try:
                    return datetime.strptime(text, fmt)
                except ValueError:
                    continue
        for fmt in ("%Y-%m-%d", "%m/%d/%Y"):
            try:
                return datetime.strptime(callback_date.strip(), fmt)
            except ValueError:
                continue
    return None


def validate_helcim_approval_secret(
    settings: Settings,
    secret_query: Optional[str],
    secret_header: Optional[str] = None,
) -> None:
    configured_secret = (settings.helcim_approval_secret or "").strip()
    if not configured_secret:
        return
    provided_secret = (secret_query or secret_header or "").strip()
    if provided_secret != configured_secret:
        raise HTTPException(status_code=401, detail="Invalid approval secret")


def build_redirect_url(base_url: str, params: Dict[str, Any]) -> str:
    parsed = urlparse(base_url)
    existing_query = dict(parse_qsl(parsed.query, keep_blank_values=True))
    for key, value in params.items():
        if value is None:
            continue
        text = str(value).strip()
        if not text:
            continue
        existing_query[key] = text
    new_query = urlencode(existing_query, doseq=True)
    return urlunparse(parsed._replace(query=new_query))


def approval_redirect_response(settings: Settings, params: Dict[str, Any]) -> Any:
    redirect_url = (settings.helcim_approval_redirect_url or "").strip()
    if not redirect_url:
        return {"status": "ok", **params}
    return RedirectResponse(url=build_redirect_url(redirect_url, params), status_code=302)


def process_helcim_approval_payload(
    payload: Dict[str, Any],
    helcim_client: HelcimRecurringClient,
) -> Dict[str, Any]:
    response_flag = str(payload.get("response", "")).strip()
    if response_flag and response_flag != "1":
        raise HTTPException(status_code=400, detail="Helcim response indicates failure")
    response_message = str(payload.get("responseMessage", "")).strip().upper()
    if response_message and response_message != "APPROVAL":
        raise HTTPException(
            status_code=400,
            detail=f"Helcim responseMessage is not APPROVAL ({response_message})",
        )

    try:
        transaction_id = int(payload.get("transactionId"))
    except (TypeError, ValueError):
        raise HTTPException(status_code=400, detail="transactionId is required")

    transaction_response = helcim_client.get_card_transaction(transaction_id)
    transaction_payload = extract_transaction_payload(transaction_response)
    customer_code = payload.get("customerCode") or transaction_payload.get("customerCode")
    paid_at = parse_helcim_transaction_datetime(payload, transaction_payload)
    card_token = payload.get("cardToken") or transaction_payload.get("cardToken")
    transaction_type = payload.get("type") or transaction_payload.get("type")
    approval_code = payload.get("approvalCode") or transaction_payload.get("approvalCode")
    amount = transaction_payload.get("amount")
    currency = transaction_payload.get("currency")
    payment_plan_id = (
        payload.get("paymentPlanId")
        or transaction_payload.get("paymentPlanId")
        or transaction_payload.get("paymentPlanID")
    )
    return {
        "transaction_id": transaction_id,
        "customer_code": customer_code,
        "card_token": card_token,
        "type": transaction_type,
        "approval_code": approval_code,
        "amount": amount,
        "currency": currency,
        "payment_plan_id": payment_plan_id,
        "approved_at": paid_at.isoformat() if paid_at else None,
        "plan_activated": False,
    }


async def extract_helcim_callback_payload(request: Request) -> Dict[str, Any]:
    content_type = (request.headers.get("content-type") or "").lower()
    if "application/json" in content_type:
        try:
            payload = await request.json()
            if isinstance(payload, dict):
                return payload
        except Exception:
            pass
    if (
        "application/x-www-form-urlencoded" in content_type
        or "multipart/form-data" in content_type
    ):
        try:
            form = await request.form()
            return {key: value for key, value in form.items()}
        except Exception:
            pass
    try:
        payload = await request.json()
        if isinstance(payload, dict):
            return payload
    except Exception:
        pass
    payload = passthrough_query_params(request)
    if payload:
        return payload
    raise HTTPException(status_code=400, detail="Approval callback payload is missing")
