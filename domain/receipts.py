from __future__ import annotations

import io
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from uuid import uuid4

from fastapi import HTTPException
from PIL import Image

from app.services.storage import GCSStorageClient


def ensure_image(content_type: Optional[str]) -> None:
    if not content_type or not content_type.startswith("image/"):
        raise HTTPException(status_code=400, detail="Uploaded file must be an image")


def build_storage_key(filename: str) -> str:
    safe_name = Path(filename or "receipt").name
    return f"receipts/{uuid4()}_{safe_name}"


_AVIF_EXECUTOR = ThreadPoolExecutor(max_workers=2)


def avif_executor() -> ThreadPoolExecutor:
    return _AVIF_EXECUTOR


def convert_bytes_to_avif(data: bytes) -> bytes:
    with Image.open(io.BytesIO(data)) as image:
        buf = io.BytesIO()
        image.save(buf, format="AVIF")
        return buf.getvalue()


def convert_and_upload_avif(
    storage_client: GCSStorageClient, data: bytes, destination: str
) -> Tuple[str, str]:
    avif_bytes = convert_bytes_to_avif(data)
    url = storage_client.upload(avif_bytes, destination, content_type="image/avif")
    return url, destination


def items_total(items: List[Dict[str, Any]]) -> Optional[float]:
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


def validate_subtotal_against_items(
    subtotal: Optional[float], items_payload: List[Dict[str, Any]]
) -> Tuple[Optional[float], Dict[str, Any]]:
    computed_items_total = items_total(items_payload)
    validation_info: Dict[str, Any] = {"items_total": computed_items_total}
    updated_subtotal = subtotal
    if computed_items_total is None:
        return updated_subtotal, validation_info

    if updated_subtotal is None:
        validation_info["subtotal_inferred_from_items"] = True
        validation_info["subtotal_matches_items"] = True
        updated_subtotal = computed_items_total
    else:
        match = abs(updated_subtotal - computed_items_total) <= 0.01
        validation_info["subtotal_matches_items"] = match
        if not match:
            validation_info["subtotal_overridden"] = True
            updated_subtotal = computed_items_total
    return updated_subtotal, validation_info
