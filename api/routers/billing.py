from __future__ import annotations

from typing import Any, Dict, Optional

from fastapi import APIRouter, Body, Depends, Header, HTTPException, Query, Request

from app.auth import AuthenticatedUser, get_current_user
from app.dependencies import (
    helcim_client,
    require_owner_email,
    settings,
    subscription_service,
)
from app.domain.billing import (
    approval_redirect_response,
    enrich_payment_plan_with_firestore_features,
    extract_helcim_callback_payload,
    passthrough_query_params,
    process_helcim_approval_payload,
    validate_helcim_approval_secret,
)

router = APIRouter()


@router.post("/billing/notify")
def billing_notify(
    payload: Dict[str, Any],
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, Any]:
    owner_email = require_owner_email(current_user)
    plan_id = subscription_service.apply_subscription_payload(owner_email, payload)
    return {"status": "ok", "plan_id": plan_id}


@router.post("/billing/helcim/customer-code")
def set_helcim_customer_code(
    payload: Dict[str, Any],
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, str]:
    owner_email = require_owner_email(current_user)
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
    validate_helcim_approval_secret(settings, secret, x_helcim_approval_secret)
    payload = await extract_helcim_callback_payload(request)
    result = process_helcim_approval_payload(payload, helcim_client)
    return {"status": "ok", **result}


@router.get("/billing/helcim/approval")
def helcim_approval_landing(
    request: Request,
    secret: Optional[str] = Query(None),
) -> Any:
    validate_helcim_approval_secret(settings, secret)
    payload = passthrough_query_params(request)
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
def helcim_list_payment_plans(
    request: Request,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
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
def helcim_create_payment_plans(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.create_payment_plans(payload)


@router.patch("/billing/helcim/payment-plans")
def helcim_patch_payment_plans(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.patch_payment_plans(payload)


@router.get("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_get_payment_plan(
    payment_plan_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    response = helcim_client.get_payment_plan(payment_plan_id)
    if isinstance(response, dict):
        return enrich_payment_plan_with_firestore_features(
            response, subscription_service
        )
    return response


@router.delete("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_delete_payment_plan(
    payment_plan_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.delete_payment_plan(payment_plan_id)


@router.get("/billing/helcim/subscriptions")
def helcim_list_subscriptions(
    request: Request,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.list_subscriptions(passthrough_query_params(request))


@router.post("/billing/helcim/subscriptions")
def helcim_create_subscriptions(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.create_subscriptions(payload)


@router.patch("/billing/helcim/subscriptions")
def helcim_patch_subscriptions(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.patch_subscriptions(payload)


@router.get("/billing/helcim/subscriptions/{subscription_id}")
def helcim_get_subscription(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.get_subscription(subscription_id)


@router.delete("/billing/helcim/subscriptions/{subscription_id}")
def helcim_delete_subscription(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    require_owner_email(current_user)
    return helcim_client.delete_subscription(subscription_id)


@router.post("/billing/helcim/subscriptions/{subscription_id}/sync")
def helcim_sync_subscription_to_user(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, Any]:
    owner_email = require_owner_email(current_user)
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
