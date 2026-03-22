from __future__ import annotations

from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from uuid import uuid4

import io

from fastapi import FastAPI, File, Form, HTTPException, Query, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import StreamingResponse

from google.api_core import exceptions as google_exceptions

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
firestore_client = FirestoreClient(settings.firestore_collection)
ocr_client = OpenAITextExtractor(settings.openai_model_name, settings.openai_api_key)
category_service = CategoryService(settings.categories_collection)


@app.get("/healthz")
def health() -> Dict[str, str]:
    return {"status": "ok"}


@app.post("/receipts", response_model=ReceiptRecord)
async def create_receipt(
    file: UploadFile = File(...),
    vendor: Optional[str] = Form(None),
    subtotal: Optional[float] = Form(None),
    tax: Optional[float] = Form(None),
    total: Optional[float] = Form(None),
    category: Optional[str] = Form(None),
    purchase_date: Optional[str] = Form(None),
) -> ReceiptRecord:
    _ensure_image(file.content_type)
    contents = await file.read()
    stored_path = _build_storage_key(file.filename or "receipt")
    image_url = storage_client.upload(contents, stored_path, content_type=file.content_type)
    category_options = category_service.category_names()
    ocr_result = ocr_client.extract(contents, category_options=category_options)
    extracted_text = ocr_result.text
    vendor_value = vendor if vendor is not None else ocr_result.vendor
    subtotal_value = subtotal if subtotal is not None else ocr_result.subtotal
    tax_value = tax if tax is not None else ocr_result.tax
    total_value = total if total is not None else ocr_result.total
    category_value = category if category is not None else ocr_result.category
    purchase_date_value = (
        purchase_date if purchase_date is not None else ocr_result.purchase_date
    )
    items_payload = [item.dict(exclude_none=True) for item in ocr_result.items]
    items_total = _items_total(items_payload)
    validation_info: Dict[str, Any] = {"items_total": items_total}
    if items_total is not None:
        if subtotal_value is None:
            subtotal_value = items_total
            validation_info["subtotal_inferred_from_items"] = True
            validation_info["subtotal_matches_items"] = True
        else:
            match = abs(subtotal_value - items_total) <= 0.01
            validation_info["subtotal_matches_items"] = match
            if not match:
                validation_info["subtotal_overridden"] = True
                subtotal_value = items_total
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
            "items": [item.dict(exclude_none=True) for item in ocr_result.items],
        },
        "validation": validation_info,
    }

    payload = {
        "vendor": vendor_value,
        "subtotal": subtotal_value,
        "tax": tax_value,
        "total": total_value,
        "category": category_value,
        "purchase_date": purchase_date_value,
        "image_url": image_url,
        "storage_path": stored_path,
        "items": items_payload,
        "extracted_text": extracted_text,
        "extracted_fields": extracted_fields,
        "created_at": datetime.utcnow(),
    }

    doc_id = firestore_client.insert_receipt(payload)
    return ReceiptRecord(id=doc_id, **payload)


@app.get("/receipts", response_model=ReceiptListResponse)
def list_receipts(
    limit: int = Query(10, ge=1, le=100),
    start_after_id: Optional[str] = Query(None),
) -> ReceiptListResponse:
    try:
        docs, next_cursor = firestore_client.list_receipts(limit=limit, start_after_id=start_after_id)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    receipts = [ReceiptRecord(**doc) for doc in docs]
    return ReceiptListResponse(receipts=receipts, next_cursor=next_cursor)


@app.put("/receipts/{receipt_id}", response_model=ReceiptRecord)
def update_receipt(receipt_id: str, payload: ReceiptUpdate) -> ReceiptRecord:
    try:
        existing = firestore_client.get_receipt(receipt_id)
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
        updated = firestore_client.update_receipt(receipt_id, update_data)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    return ReceiptRecord(id=receipt_id, **updated)


@app.post("/categories", response_model=CategoryRecord)
def create_category(payload: CategoryCreate):
    data = payload.dict(exclude_none=True)
    category_id = category_service.create_category(data)
    return CategoryRecord(id=category_id, **data)


@app.get("/categories", response_model=List[CategoryRecord])
def list_categories():
    docs = category_service.list_categories()
    return [CategoryRecord(**doc) for doc in docs]


@app.put("/categories/{category_id}", response_model=CategoryRecord)
def update_category(category_id: str, payload: CategoryCreate):
    data = payload.dict(exclude_none=True)
    try:
        category_service.update_category(category_id, data)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    category_doc = category_service.get_category(category_id)
    return CategoryRecord(**category_doc)


@app.delete("/categories/{category_id}", status_code=204)
def delete_category(category_id: str):
    try:
        category_service.delete_category(category_id)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))


@app.get("/receipts/{receipt_id}/image")
def stream_receipt_image(receipt_id: str):
    try:
        stored = firestore_client.get_receipt(receipt_id)
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


@app.get("/receipts/{receipt_id}" )
def read_receipt(receipt_id: str) -> ReceiptRecord:
    try:
        stored = firestore_client.get_receipt(receipt_id)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))
    return ReceiptRecord(id=receipt_id, **stored)


@app.delete("/receipts/{receipt_id}", status_code=204)
def delete_receipt(receipt_id: str):
    try:
        stored = firestore_client.delete_receipt(receipt_id)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    storage_path = stored.get("storage_path")
    if storage_path:
        storage_client.delete(storage_path)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=8000, reload=True)


