from __future__ import annotations

from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse
from uuid import uuid4

import asyncio
import io
from concurrent.futures import ThreadPoolExecutor

from PIL import Image

from fastapi import Body, Depends, FastAPI, File, Form, Header, HTTPException, Query, Request, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import RedirectResponse, StreamingResponse

from google.api_core import exceptions as google_exceptions

from app.auth import AuthenticatedUser, get_current_user
from app.config import get_settings
from app.models import (
    CategoryCreate,
    CategoryRecord,
    ReceiptListResponse,
    ReceiptRecord,
    ReceiptUpdate,
)
from app.services.categories import CategoryService
from app.services.firestore_db import FirestoreClient
from app.services.helcim_recurring import HelcimRecurringClient
from app.services.ocr import OpenAITextExtractor
from app.services.storage import GCSStorageClient
from app.services.subscriptions import SubscriptionService


def _ensure_image(content_type: Optional[str]) -> None:
    if not content_type or not content_type.startswith("image/"):
        raise HTTPException(status_code=400, detail="Uploaded file must be an image")


def _build_storage_key(filename: str) -> str:
    safe_name = Path(filename or "receipt").name
    return f"receipts/{uuid4()}_{safe_name}"


_AVIF_EXECUTOR = ThreadPoolExecutor(max_workers=2)


def _convert_bytes_to_avif(data: bytes) -> bytes:
    with Image.open(io.BytesIO(data)) as image:
        buf = io.BytesIO()
        image.save(buf, format="AVIF")
        return buf.getvalue()


def _convert_and_upload_avif(
    storage_client: GCSStorageClient, data: bytes, destination: str
) -> Tuple[str, str]:
    avif_bytes = _convert_bytes_to_avif(data)
    url = storage_client.upload(avif_bytes, destination, content_type="image/avif")
    return url, destination


def _items_total(items: List[Dict[str, Any]]) -> Optional[float]:
    if not items:
        return None
    total = 0.0
    counted = False
    for item in items:
        quantity = item.get("quantity")
        price = item.get("price")
        if quantity is None or price is None:
            continue
        total += quantity * price
        counted = True
    if not counted:
        return None
    return round(total, 2)


def _validate_subtotal_against_items(
    subtotal: Optional[float], items_payload: List[Dict[str, Any]]
) -> Tuple[Optional[float], Dict[str, Any]]:
    items_total = _items_total(items_payload)
    validation_info: Dict[str, Any] = {"items_total": items_total}
    updated_subtotal = subtotal
    if items_total is None:
        return updated_subtotal, validation_info

    if updated_subtotal is None:
        validation_info["subtotal_inferred_from_items"] = True
        validation_info["subtotal_matches_items"] = True
        updated_subtotal = items_total
    else:
        match = abs(updated_subtotal - items_total) <= 0.01
        validation_info["subtotal_matches_items"] = match
        if not match:
            validation_info["subtotal_overridden"] = True
            updated_subtotal = items_total
    return updated_subtotal, validation_info


def _passthrough_query_params(request: Request) -> Dict[str, Any]:
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


def _enrich_payment_plan_with_firestore_features(plan_payload: Any) -> Any:
    if not isinstance(plan_payload, dict):
        return plan_payload
    payment_plan_id = plan_payload.get("id")
    firestore_plan = subscription_service.find_plan_by_payment_plan_id(payment_plan_id)
    features = subscription_service.plan_features(firestore_plan)
    return {**plan_payload, "features": features}


def _extract_transaction_payload(response_payload: Any) -> Dict[str, Any]:
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


def _parse_helcim_transaction_datetime(
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


def _validate_helcim_approval_secret(
    secret_query: Optional[str],
    secret_header: Optional[str] = None,
) -> None:
    configured_secret = (settings.helcim_approval_secret or "").strip()
    if not configured_secret:
        return
    provided_secret = (secret_query or secret_header or "").strip()
    if provided_secret != configured_secret:
        raise HTTPException(status_code=401, detail="Invalid approval secret")


def _build_redirect_url(base_url: str, params: Dict[str, Any]) -> str:
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


def _approval_redirect_response(params: Dict[str, Any]) -> Any:
    redirect_url = (settings.helcim_approval_redirect_url or "").strip()
    if not redirect_url:
        return {"status": "ok", **params}
    return RedirectResponse(url=_build_redirect_url(redirect_url, params), status_code=302)


def _process_helcim_approval_payload(payload: Dict[str, Any]) -> Dict[str, Any]:
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
    transaction_payload = _extract_transaction_payload(transaction_response)
    customer_code = payload.get("customerCode") or transaction_payload.get("customerCode")
    paid_at = _parse_helcim_transaction_datetime(payload, transaction_payload)
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


async def _extract_helcim_callback_payload(request: Request) -> Dict[str, Any]:
    content_type = (request.headers.get("content-type") or "").lower()
    if "application/json" in content_type:
        try:
            payload = await request.json()
            if isinstance(payload, dict):
                return payload
        except Exception:
            pass
    if "application/x-www-form-urlencoded" in content_type or "multipart/form-data" in content_type:
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
    payload = _passthrough_query_params(request)
    if payload:
        return payload
    raise HTTPException(status_code=400, detail="Approval callback payload is missing")


settings = get_settings()
app = FastAPI(title="Receipt Scanner API")

app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.allowed_origins,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

storage_client = GCSStorageClient(settings.gcs_bucket_name)
firestore_client = FirestoreClient(
    settings.firestore_collection, settings.firestore_database_id
)
ocr_client = OpenAITextExtractor(settings.openai_model_name, settings.openai_api_key)
category_service = CategoryService(
    settings.categories_collection, settings.firestore_database_id
)
subscription_service = SubscriptionService(
    settings.plans_collection, settings.users_collection, settings.firestore_database_id
)
helcim_client = HelcimRecurringClient(
    settings.helcim_api_token,
    settings.helcim_api_base_url,
    settings.helcim_timeout_seconds,
)


def _require_owner_email(current_user: Optional[AuthenticatedUser]) -> str:
    if current_user is None:
        raise HTTPException(
            status_code=401,
            detail="OAuth bearer token required",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return current_user.email


@app.get("/healthz")
def health() -> Dict[str, str]:
    return {"status": "ok"}


@app.post("/receipts", response_model=ReceiptRecord)
async def create_receipt(
    request: Request,
    file: UploadFile = File(...),
    vendor: Optional[str] = Form(None),
    subtotal: Optional[float] = Form(None),
    tax: Optional[float] = Form(None),
    total: Optional[float] = Form(None),
    category: Optional[str] = Form(None),
    purchase_date: Optional[str] = Form(None),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptRecord:
    _ensure_image(file.content_type)
    contents = await file.read()
    owner_email = _require_owner_email(current_user)
    subscription_service.ensure_within_limit(owner_email, firestore_client)


    stored_path = _build_storage_key(file.filename or "receipt")
    avif_path = f"{stored_path}.avif"
    category_options = category_service.category_names(owner_email)
    ocr_task = asyncio.create_task(
        asyncio.to_thread(
            ocr_client.extract,
            contents,
            category_options=category_options,
        )
    )
    conversion_future = _AVIF_EXECUTOR.submit(
        _convert_and_upload_avif, storage_client, contents, avif_path
    )
    ocr_result = await ocr_task
    extracted_text = ocr_result.text
    items_payload = [item.dict(exclude_none=True) for item in ocr_result.items]
    subtotal_candidate = subtotal if subtotal is not None else ocr_result.subtotal
    subtotal_value, validation_info = _validate_subtotal_against_items(
        subtotal_candidate, items_payload
    )
    vendor_value = vendor if vendor is not None else ocr_result.vendor
    tax_value = tax if tax is not None else ocr_result.tax
    total_value = total if total is not None else ocr_result.total
    category_value = category if category is not None else ocr_result.category
    purchase_date_value = (
        purchase_date if purchase_date is not None else ocr_result.purchase_date
    )
    extracted_fields = {
        "model": settings.openai_model_name,
        "text_length": len(extracted_text),
        "ai_suggestions": {
            "vendor": ocr_result.vendor,
            "subtotal": ocr_result.subtotal,
            "tax": ocr_result.tax,
            "total": ocr_result.total,
            "category": ocr_result.category,
            "purchase_date": ocr_result.purchase_date,
            "items": items_payload,
        },
        "validation": validation_info,
    }

    try:
        image_url, storage_path = conversion_future.result()
    except Exception as exc:
        raise HTTPException(status_code=500, detail=f"Failed to convert image: {exc}")

    payload: Dict[str, Any] = {
        "vendor": vendor_value,
        "subtotal": subtotal_value,
        "tax": tax_value,
        "total": total_value,
        "category": category_value,
        "purchase_date": purchase_date_value,
        "image_url": image_url,
        "storage_path": storage_path,
        "items": items_payload,
        "extracted_text": extracted_text,
        "extracted_fields": extracted_fields,
        "created_at": datetime.utcnow(),
        "owner_email": owner_email,
    }

    doc_id = firestore_client.insert_receipt(owner_email, payload)
    # Prefer serving images through the API proxy so the bucket can stay private.
    payload["image_url"] = str(request.url_for("stream_receipt_image", receipt_id=doc_id))
    try:
        firestore_client.update_receipt(
            doc_id, {"image_url": payload["image_url"]}, owner_email
        )
    except KeyError:
        # If the receipt was deleted between insert and update, just return the response we have.
        pass
    return ReceiptRecord(id=doc_id, **payload)


@app.get("/receipts", response_model=ReceiptListResponse)
def list_receipts(
    request: Request,
    limit: int = Query(10, ge=1, le=100),
    start_after_id: Optional[str] = Query(None),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptListResponse:
    try:
        owner_email = _require_owner_email(current_user)
        docs, next_cursor = firestore_client.list_receipts(
            owner_email, limit=limit, start_after_id=start_after_id
        )
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    for doc in docs:
        doc["image_url"] = str(
            request.url_for("stream_receipt_image", receipt_id=doc["id"])
        )
    receipts = [ReceiptRecord(**doc) for doc in docs]
    return ReceiptListResponse(receipts=receipts, next_cursor=next_cursor)


@app.put("/receipts/{receipt_id}", response_model=ReceiptRecord)
def update_receipt(
    receipt_id: str,
    payload: ReceiptUpdate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptRecord:
    owner_email = _require_owner_email(current_user)
    try:
        existing = firestore_client.get_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    update_data = payload.dict(exclude_unset=True)
    if not update_data:
        return ReceiptRecord(id=receipt_id, **existing)

    if "items" in update_data:
        raw_items = payload.items or []
        items_payload = [item.dict(exclude_none=True) for item in raw_items]
        update_data["items"] = items_payload
        subtotal_candidate = update_data.get("subtotal", existing.get("subtotal"))
        subtotal_value, validation_info = _validate_subtotal_against_items(
            subtotal_candidate, items_payload
        )
        update_data["subtotal"] = subtotal_value
        extracted_fields = dict(existing.get("extracted_fields") or {})
        extracted_fields["validation"] = validation_info
        ai_suggestions = dict(extracted_fields.get("ai_suggestions") or {})
        ai_suggestions["items"] = items_payload
        extracted_fields["ai_suggestions"] = ai_suggestions
        update_data["extracted_fields"] = extracted_fields

    try:
        updated = firestore_client.update_receipt(
            receipt_id, update_data, owner_email
        )
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    return ReceiptRecord(id=receipt_id, **updated)


@app.post("/categories", response_model=CategoryRecord)
def create_category(
    payload: CategoryCreate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> CategoryRecord:
    owner_email = _require_owner_email(current_user)
    data = payload.dict(exclude_none=True)
    try:
        category_id = category_service.create_category(data, owner_email)
    except ValueError as err:
        raise HTTPException(status_code=400, detail=str(err))
    category_doc = category_service.get_category(category_id, owner_email)
    return CategoryRecord(**category_doc)


@app.get("/categories", response_model=List[CategoryRecord])
def list_categories(current_user: Optional[AuthenticatedUser] = Depends(get_current_user)):
    owner_email = _require_owner_email(current_user)
    try:
        docs = category_service.list_categories(owner_email)
    except google_exceptions.GoogleAPICallError as err:
        raise HTTPException(status_code=500, detail=f"Failed to load categories: {err}")
    return [CategoryRecord(**doc) for doc in docs]


@app.put("/categories/{category_id}", response_model=CategoryRecord)
def update_category(
    category_id: str,
    payload: CategoryCreate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> CategoryRecord:
    owner_email = _require_owner_email(current_user)
    data = payload.dict(exclude_none=True)
    try:
        category_service.update_category(category_id, data, owner_email)
    except ValueError as err:
        raise HTTPException(status_code=400, detail=str(err))
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    category_doc = category_service.get_category(category_id, owner_email)
    return CategoryRecord(**category_doc)


@app.delete("/categories/{category_id}", status_code=204)
def delete_category(
    category_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> None:
    owner_email = _require_owner_email(current_user)
    try:
        category_service.delete_category(category_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))


@app.get("/receipts/{receipt_id}/image")
def stream_receipt_image(
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> StreamingResponse:
    owner_email = _require_owner_email(current_user)
    try:
        stored = firestore_client.get_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    storage_path = stored.get("storage_path")
    if not storage_path:
        raise HTTPException(status_code=404, detail="Receipt image not available")

    try:
        image_bytes, content_type = storage_client.download(storage_path)
    except google_exceptions.NotFound:
        raise HTTPException(status_code=404, detail="Stored receipt image not found")

    media_type = content_type or "application/octet-stream"
    return StreamingResponse(io.BytesIO(image_bytes), media_type=media_type)


@app.get("/receipts/{receipt_id}")
def read_receipt(
    request: Request,
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptRecord:
    owner_email = _require_owner_email(current_user)
    try:
        stored = firestore_client.get_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))
    stored["image_url"] = str(request.url_for("stream_receipt_image", receipt_id=receipt_id))
    return ReceiptRecord(id=receipt_id, **stored)


@app.delete("/receipts/{receipt_id}", status_code=204)
def delete_receipt(
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> None:
    owner_email = _require_owner_email(current_user)
    try:
        stored = firestore_client.delete_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    storage_path = stored.get("storage_path")
    if storage_path:
        storage_client.delete(storage_path)


@app.post("/billing/notify")
def billing_notify(
    payload: Dict[str, Any],
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, Any]:
    owner_email = _require_owner_email(current_user)
    plan_id = subscription_service.apply_subscription_payload(owner_email, payload)
    return {"status": "ok", "plan_id": plan_id}


@app.post("/billing/helcim/customer-code")
def set_helcim_customer_code(
    payload: Dict[str, Any],
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, str]:
    owner_email = _require_owner_email(current_user)
    customer_code = payload.get("customerCode")
    if customer_code is None or not str(customer_code).strip():
        raise HTTPException(status_code=400, detail="customerCode is required")
    subscription_service.set_owner_customer_code(owner_email, str(customer_code))
    return {"status": "ok", "owner_email": owner_email}


@app.post("/billing/helcim/approval")
async def helcim_approval_callback(
    request: Request,
    secret: Optional[str] = Query(None),
    x_helcim_approval_secret: Optional[str] = Header(None),
) -> Dict[str, Any]:
    _validate_helcim_approval_secret(secret, x_helcim_approval_secret)
    payload = await _extract_helcim_callback_payload(request)
    result = _process_helcim_approval_payload(payload)
    return {"status": "ok", **result}


@app.get("/billing/helcim/approval")
def helcim_approval_landing(
    request: Request,
    secret: Optional[str] = Query(None),
) -> Any:
    _validate_helcim_approval_secret(secret)
    payload = _passthrough_query_params(request)
    if not payload.get("transactionId"):
        return _approval_redirect_response({"status": "ok", "callback": "received"})
    try:
        result = _process_helcim_approval_payload(payload)
        return _approval_redirect_response({"status": "ok", **result})
    except HTTPException as exc:
        return _approval_redirect_response(
            {
                "status": "error",
                "error": exc.detail,
                "transaction_id": payload.get("transactionId"),
            }
        )


@app.get("/billing/helcim/payment-plans")
def helcim_list_payment_plans(
    request: Request,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    response = helcim_client.list_payment_plans(_passthrough_query_params(request))
    if not isinstance(response, dict):
        return response
    data = response.get("data")
    if not isinstance(data, list):
        return response
    enriched_data = [
        _enrich_payment_plan_with_firestore_features(entry)
        if isinstance(entry, dict)
        else entry
        for entry in data
    ]
    return {**response, "data": enriched_data}


@app.post("/billing/helcim/payment-plans")
def helcim_create_payment_plans(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.create_payment_plans(payload)


@app.patch("/billing/helcim/payment-plans")
def helcim_patch_payment_plans(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.patch_payment_plans(payload)


@app.get("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_get_payment_plan(
    payment_plan_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    response = helcim_client.get_payment_plan(payment_plan_id)
    if isinstance(response, dict):
        return _enrich_payment_plan_with_firestore_features(response)
    return response


@app.delete("/billing/helcim/payment-plans/{payment_plan_id}")
def helcim_delete_payment_plan(
    payment_plan_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.delete_payment_plan(payment_plan_id)


@app.get("/billing/helcim/subscriptions")
def helcim_list_subscriptions(
    request: Request,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.list_subscriptions(_passthrough_query_params(request))


@app.post("/billing/helcim/subscriptions")
def helcim_create_subscriptions(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.create_subscriptions(payload)


@app.patch("/billing/helcim/subscriptions")
def helcim_patch_subscriptions(
    payload: Any = Body(...),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.patch_subscriptions(payload)


@app.get("/billing/helcim/subscriptions/{subscription_id}")
def helcim_get_subscription(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.get_subscription(subscription_id)


@app.delete("/billing/helcim/subscriptions/{subscription_id}")
def helcim_delete_subscription(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Any:
    _require_owner_email(current_user)
    return helcim_client.delete_subscription(subscription_id)


@app.post("/billing/helcim/subscriptions/{subscription_id}/sync")
def helcim_sync_subscription_to_user(
    subscription_id: int,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> Dict[str, Any]:
    owner_email = _require_owner_email(current_user)
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



if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=8000, reload=True)


