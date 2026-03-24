from __future__ import annotations

from typing import Optional

from fastapi import HTTPException, Request
from app.config import Settings, get_settings
from app.helcim_recurring import HelcimRecurringClient
from app.subscriptions import SubscriptionService

settings = get_settings()

subscription_service = SubscriptionService(
    settings.plans_collection, settings.users_collection, settings.firestore_database_id
)
helcim_client = HelcimRecurringClient(
    settings.helcim_api_token,
    settings.helcim_api_base_url,
    settings.helcim_timeout_seconds,
    settings.helcim_user_agent,
)


def get_app_settings() -> Settings:
    return settings


def get_subscription_service() -> SubscriptionService:
    return subscription_service


def get_helcim_client() -> HelcimRecurringClient:
    return helcim_client


def require_owner_email(request: Request) -> str:
    email = request.headers.get("X-Go-Authenticated-Email", "").strip()
    if not email:
        raise HTTPException(
            status_code=401,
            detail="OAuth bearer token required",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return email


def get_owner_email(request: Request) -> Optional[str]:
    email = request.headers.get("X-Go-Authenticated-Email", "").strip()
    return email or None
