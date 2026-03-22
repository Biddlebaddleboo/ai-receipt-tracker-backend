from __future__ import annotations

from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from uuid import uuid4

import asyncio
import io
from concurrent.futures import ThreadPoolExecutor

from PIL import Image

from fastapi import Depends, FastAPI, File, Form, HTTPException, Query, Request, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import StreamingResponse

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
from app.services.ocr import OpenAITextExtractor
from app.services.storage import GCSStorageClient


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


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=8000, reload=True)


