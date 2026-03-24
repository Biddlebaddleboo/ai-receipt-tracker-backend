from __future__ import annotations

from typing import List, Optional

from fastapi import APIRouter, Depends, HTTPException
from google.api_core import exceptions as google_exceptions

from app.auth import AuthenticatedUser, get_current_user
from app.dependencies import category_service, require_owner_email
from app.models import CategoryCreate, CategoryRecord

router = APIRouter()


@router.post("/categories", response_model=CategoryRecord)
def create_category(
    payload: CategoryCreate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> CategoryRecord:
    owner_email = require_owner_email(current_user)
    data = payload.dict(exclude_none=True)
    try:
        category_id = category_service.create_category(data, owner_email)
    except ValueError as err:
        raise HTTPException(status_code=400, detail=str(err))
    category_doc = category_service.get_category(category_id, owner_email)
    return CategoryRecord(**category_doc)


@router.get("/categories", response_model=List[CategoryRecord])
def list_categories(
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
):
    owner_email = require_owner_email(current_user)
    try:
        docs = category_service.list_categories(owner_email)
    except google_exceptions.GoogleAPICallError as err:
        raise HTTPException(status_code=500, detail=f"Failed to load categories: {err}")
    return [CategoryRecord(**doc) for doc in docs]


@router.put("/categories/{category_id}", response_model=CategoryRecord)
def update_category(
    category_id: str,
    payload: CategoryCreate,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> CategoryRecord:
    owner_email = require_owner_email(current_user)
    data = payload.dict(exclude_none=True)
    try:
        category_service.update_category(category_id, data, owner_email)
    except ValueError as err:
        raise HTTPException(status_code=400, detail=str(err))
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))

    category_doc = category_service.get_category(category_id, owner_email)
    return CategoryRecord(**category_doc)


@router.delete("/categories/{category_id}", status_code=204)
def delete_category(
    category_id: str,
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
) -> None:
    owner_email = require_owner_email(current_user)
    try:
        category_service.delete_category(category_id, owner_email)
    except KeyError as err:
        raise HTTPException(status_code=404, detail=str(err))
