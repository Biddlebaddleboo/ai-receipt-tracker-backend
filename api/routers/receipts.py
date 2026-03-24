from __future__ import annotations

import asyncio
import io
from datetime import datetime
from typing import Any, Dict, Optional

from fastapi import APIRouter, Depends, File, Form, HTTPException, Query, Request, UploadFile
from fastapi.responses import StreamingResponse
from google.api_core import exceptions as google_exceptions

from app.auth import AuthenticatedUser, get_current_user
from app.dependencies import (
    firestore_client,
    ocr_client,
    require_owner_email,
    settings,
    storage_client,
    category_service,
    subscription_service,
)
from app.domain.receipts import (
    avif_executor,
    build_storage_key,
    convert_and_upload_avif,
    ensure_image,
    validate_subtotal_against_items,
)
from app.models import ReceiptListResponse, ReceiptRecord, ReceiptUpdate

router = APIRouter()


@router.post("/receipts", response_model=ReceiptRecord)
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
    ensure_image(file.content_type)
    contents = await file.read()
    owner_email = require_owner_email(current_user)
    subscription_service.ensure_within_limit(owner_email, firestore_client)

    stored_path = build_storage_key(file.filename or "receipt")
    avif_path = f"{stored_path}.avif"
    category_options = category_service.category_names(owner_email)
    ocr_task = asyncio.create_task(
        asyncio.to_thread(
            ocr_client.extract,
            contents,
            category_options=category_options,
        )
    )
    conversion_future = avif_executor().submit(
        convert_and_upload_avif, storage_client, contents, avif_path
    )
    ocr_result = await ocr_task
    extracted_text = ocr_result.text
    items_payload = [item.dict(exclude_none=True) for item in ocr_result.items]
    subtotal_candidate = subtotal if subtotal is not None else ocr_result.subtotal
    subtotal_value, validation_info = validate_subtotal_against_items(
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
    payload["image_url"] = str(request.url_for("stream_receipt_image", receipt_id=doc_id))
    try:
        firestore_client.update_receipt(
            doc_id, {"image_url": payload["image_url"]}, owner_email
        )
    except KeyError:
        pass
    return ReceiptRecord(id=doc_id, **payload)


@router.get("/receipts", response_model=ReceiptListResponse)
def list_receipts(
    request: Request,
    limit: int = Query(10, ge=1, le=100),
    start_after_id: Optional[str] = Query(None),
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptListResponse:
    try:
        owner_email = require_owner_email(current_user)
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


@router.put("/receipts/{receipt_id}", response_model=ReceiptRecord)
def update_receipt(
    receipt_id: str,
    payload: ReceiptUpdate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptRecord:
    owner_email = require_owner_email(current_user)
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
        subtotal_value, validation_info = validate_subtotal_against_items(
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
        updated = firestore_client.update_receipt(receipt_id, update_data, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    return ReceiptRecord(id=receipt_id, **updated)


@router.get("/receipts/{receipt_id}/image")
def stream_receipt_image(
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> StreamingResponse:
    owner_email = require_owner_email(current_user)
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


@router.get("/receipts/{receipt_id}", response_model=ReceiptRecord)
def read_receipt(
    request: Request,
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> ReceiptRecord:
    owner_email = require_owner_email(current_user)
    try:
        stored = firestore_client.get_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))
    stored["image_url"] = str(
        request.url_for("stream_receipt_image", receipt_id=receipt_id)
    )
    return ReceiptRecord(id=receipt_id, **stored)


@router.delete("/receipts/{receipt_id}", status_code=204)
def delete_receipt(
    receipt_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> None:
    owner_email = require_owner_email(current_user)
    try:
        stored = firestore_client.delete_receipt(receipt_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    storage_path = stored.get("storage_path")
    if storage_path:
        storage_client.delete(storage_path)
