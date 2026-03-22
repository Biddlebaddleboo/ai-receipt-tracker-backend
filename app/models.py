from __future__ import annotations

from datetime import datetime
from typing import Any, Dict, List, Optional

from pydantic import BaseModel, Field


class ReceiptPayload(BaseModel):
    vendor: Optional[str]
    subtotal: Optional[float]
    tax: Optional[float]
    total: Optional[float]
    category: Optional[str]
    purchase_date: Optional[str]
    items: List["ReceiptItem"] = Field(default_factory=list)


class ReceiptItem(BaseModel):
    name: str
    quantity: Optional[float] = None
    price: Optional[float] = None


class ReceiptRecord(BaseModel):
    id: str
    vendor: Optional[str]
    subtotal: Optional[float]
    tax: Optional[float]
    total: Optional[float]
    category: Optional[str]
    purchase_date: Optional[str]
    items: List[ReceiptItem] = Field(default_factory=list)
    image_url: str
    storage_path: str = Field(..., exclude=True)
    owner_email: str = Field(..., exclude=True)
    extracted_text: str
    extracted_fields: Dict[str, Any]
    created_at: datetime


class ReceiptUpdate(BaseModel):
    vendor: Optional[str] = None
    subtotal: Optional[float] = None
    tax: Optional[float] = None
    total: Optional[float] = None
    category: Optional[str] = None
    purchase_date: Optional[str] = None
    items: Optional[List[ReceiptItem]] = None

class ReceiptListResponse(BaseModel):
    receipts: List[ReceiptRecord]
    next_cursor: Optional[str] = None


class CategoryRecord(BaseModel):
    id: str
    name: str
    description: Optional[str] = None


class CategoryCreate(BaseModel):
    name: str
    description: Optional[str] = None
