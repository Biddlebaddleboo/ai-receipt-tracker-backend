from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, Depends

from app.auth import AuthenticatedUser, get_current_user
from app.dependencies import require_owner_email, subscription_service

router = APIRouter()


@router.get("/users/me/plan")
def read_user_plan(
    current_user: Optional[AuthenticatedUser] = Depends(get_current_user),
):
    owner_email = require_owner_email(current_user)
    return subscription_service.user_plan_summary(owner_email)
