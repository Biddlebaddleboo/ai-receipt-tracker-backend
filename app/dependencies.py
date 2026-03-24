from __future__ import annotations

from typing import Optional

from fastapi import HTTPException

from app.auth import AuthenticatedUser
from app.config import Settings, get_settings
from app.services.helcim_recurring import HelcimRecurringClient
from app.services.subscriptions import SubscriptionService

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


def require_owner_email(current_user: Optional[AuthenticatedUser]) -> str:
    if current_user is None:
        raise HTTPException(
            status_code=401,
            detail="OAuth bearer token required",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return current_user.email
