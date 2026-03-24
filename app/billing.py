from __future__ import annotations

import hashlib
import hmac
import json
import logging
from datetime import datetime
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from typing import Any, Dict, Optional
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

from fastapi import APIRouter, Body, Header, HTTPException, Query, Request
from fastapi.responses import RedirectResponse

from app.config import Settings
from app.dependencies import (
    get_owner_email,
    helcim_client,
    require_owner_email,
    settings,
    subscription_service,
)
from app.helcim_recurring import HelcimRecurringClient
from app.subscriptions import SubscriptionService

router = APIRouter()
logger = logging.getLogger("app.billing")


@router.post("/billing/notify")
def billing_notify(payload: Dict[str, Any], request: Request) -> Dict[str, Any]:
    owner_email = require_owner_email(request)
    plan_id = subscription_service.apply_subscription_payload(owner_email, payload)
    return {"status": "ok", "plan_id": plan_id}


@router.post("/billing/helcim/customer-code")
def set_helcim_customer_code(payload: Dict[str, Any], request: Request) -> Dict[str, str]:
    owner_email = require_owner_email(request)
    customer_code = payload.get("customerCode")
    if customer_code is None or not str(customer_code).strip():
        raise HTTPException(status_code=400, detail="customerCode is required")
    subscription_service.set_owner_customer_code(owner_email, str(customer_code))
    return {"status": "ok", "owner_email": owner_email}


@router.post("/billing/helcim/approval")
async def helcim_approval_callback(
    request: Request,
    secret: Optional[str] = Query(None),
    x_helcim_approval_secret: Optional[str] = Header(None),
) -> Dict[str, Any]:
    payload = await extract_helcim_callback_payload(request)
    validate_helcim_callback_auth(
        settings,
        payload,
        secret_query=secret,
        secret_header=x_helcim_approval_secret,
    )
    result = process_helcim_approval_payload(payload, helcim_client)
    return {"status": "ok", **result}


@router.get("/billing/helcim/approval")
def helcim_approval_landing(
    request: Request,
    secret: Optional[str] = Query(None),
) -> Any:
    payload = passthrough_query_params(request)
    validate_helcim_callback_auth(settings, payload, secret_query=secret)
    if not payload.get("transactionId"):
        return approval_redirect_response(
            settings, {"status": "ok", "callback": "received"}
        )
    try:
        result = process_helcim_approval_payload(payload, helcim_client)
        return approval_redirect_response(settings, {"status": "ok", **result})
    except HTTPException as exc:
        return approval_redirect_response(
            settings,
            {
                "status": "error",
                "error": exc.detail,
                "transaction_id": payload.get("transactionId"),
            },
        )


@router.get("/billing/helcim/payment-plans")
def helcim_list_payment_plans(request: Request) -> Any:
    owner_email = get_owner_email(request)
    if owner_email:
        logger.debug("returning Helcim payment plans for %s", owner_email)
    response = helcim_client.list_payment_plans(passthrough_query_params(request))
    if not isinstance(response, dict):
        return response
    data = response.get("data")
    if not isinstance(data, list):
        return response
    enriched_data = [
        enrich_payment_plan_with_firestore_features(entry, subscription_service)
        if isinstance(entry, dict)
        else entry
        for entry in data
    ]
    return {**response, "data": enriched_data}


@router.post("/billing/helcim/payment-plans")
def helcim_create_payment_plans(request: Request, payload: Any = Body(...)) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.create_payment_plans(payload)


@router.patch("/billing/helcim/payment-plans")
def helcim_patch_payment_plans(request: Request, payload: Any = Body(...)) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.patch_payment_plans(payload)


@router.get("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_get_payment_plan(payment_plan_id: int, request: Request) -> Any:
    owner_email = require_owner_email(request)
    response = helcim_client.get_payment_plan(payment_plan_id)
    if isinstance(response, dict):
        return enrich_payment_plan_with_firestore_features(
            response, subscription_service
        )
    return response


@router.delete("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_delete_payment_plan(payment_plan_id: int, request: Request) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.delete_payment_plan(payment_plan_id)


@router.get("/billing/helcim/subscriptions")
def helcim_list_subscriptions(request: Request) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.list_subscriptions(passthrough_query_params(request))


@router.post("/billing/helcim/subscriptions")
def helcim_create_subscriptions(request: Request, payload: Any = Body(...)) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.create_subscriptions(payload)


@router.patch("/billing/helcim/subscriptions")
def helcim_patch_subscriptions(request: Request, payload: Any = Body(...)) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.patch_subscriptions(payload)


@router.get("/billing/helcim/subscriptions/{subscription_id}")
def helcim_get_subscription(subscription_id: int, request: Request) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.get_subscription(subscription_id)


@router.delete("/billing/helcim/subscriptions/{subscription_id}")
def helcim_delete_subscription(subscription_id: int, request: Request) -> Any:
    owner_email = require_owner_email(request)
    return helcim_client.delete_subscription(subscription_id)


@router.post("/billing/helcim/subscriptions/{subscription_id}/sync")
def helcim_sync_subscription_to_user(subscription_id: int, request: Request) -> Dict[str, Any]:
    owner_email = require_owner_email(request)
    subscription = helcim_client.get_subscription(subscription_id)
    if not isinstance(subscription, dict):
        raise HTTPException(
            status_code=502,
            detail="Unexpected Helcim subscription response format",
        )
    plan_id = subscription_service.apply_subscription_payload(owner_email, subscription)
    return {
        "status": "ok",
        "plan_id": plan_id,
        "subscription_id": subscription_id,
    }


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


def validate_helcim_callback_auth(
    settings: Settings,
    payload: Dict[str, Any],
    secret_query: Optional[str],
    secret_header: Optional[str] = None,
) -> None:
    configured_secret = (settings.helcim_approval_secret or "").strip()
    configured_hash_secret = (settings.helcim_hash_secret or "").strip()
    if not configured_secret and not configured_hash_secret:
        return

    if configured_secret:
        provided_secret = (secret_query or secret_header or "").strip()
        if provided_secret and hmac.compare_digest(provided_secret, configured_secret):
            return

    if configured_hash_secret and _helcim_amount_hash_matches(payload, configured_hash_secret):
        return

    raise HTTPException(status_code=401, detail="Invalid approval signature")


def _helcim_amount_hash_matches(payload: Dict[str, Any], hash_secret: str) -> bool:
    provided_hash = str(payload.get("amountHash") or "").strip().lower()
    if not provided_hash:
        return False
    amount = _normalize_helcim_amount(payload.get("amount"))
    if amount is None:
        return False
    expected_hash = hashlib.sha256(f"{hash_secret}{amount}".encode("utf-8")).hexdigest()
    return hmac.compare_digest(provided_hash, expected_hash.lower())


def _normalize_helcim_amount(value: Any) -> Optional[str]:
    if value is None:
        return None
    text = str(value).strip()
    if not text:
        return None
    try:
        amount = Decimal(text).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
    except (InvalidOperation, ValueError):
        return None
    return format(amount, ".2f")


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
    customer_code = (
        payload.get("customerCode")
        or payload.get("customerId")
        or transaction_payload.get("customerCode")
        or transaction_payload.get("customerId")
    )
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
    raw_body = await request.body()
    if "application/json" in content_type:
        try:
            payload = json.loads(raw_body.decode("utf-8"))
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
    payload = _parse_urlencoded_payload(raw_body)
    if payload:
        return payload
    try:
        payload = json.loads(raw_body.decode("utf-8"))
        if isinstance(payload, dict):
            return payload
    except Exception:
        pass
    payload = passthrough_query_params(request)
    if payload:
        return payload
    raise HTTPException(status_code=400, detail="Approval callback payload is missing")


def _parse_urlencoded_payload(raw_body: bytes) -> Dict[str, Any]:
    if not raw_body:
        return {}
    try:
        text = raw_body.decode("utf-8")
    except UnicodeDecodeError:
        return {}
    if "=" not in text:
        return {}
    parsed = parse_qsl(text, keep_blank_values=True)
    if not parsed:
        return {}
    payload: Dict[str, Any] = {}
    for key, value in parsed:
        if key in payload:
            existing = payload[key]
            if isinstance(existing, list):
                existing.append(value)
            else:
                payload[key] = [existing, value]
        else:
            payload[key] = value
    return payload
