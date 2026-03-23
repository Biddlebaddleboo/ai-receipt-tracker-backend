import json
import os
from functools import lru_cache
from typing import Any, List, Optional

from dotenv import load_dotenv
from pydantic import BaseModel, ConfigDict, Field, field_validator

load_dotenv()

_DEFAULT_ALLOWED_ORIGINS = ["http://localhost:3000"]


def _normalize_list_field(value: Any) -> List[str]:
    if value is None:
        return []
    if isinstance(value, list):
        return [str(entry).strip() for entry in value if str(entry).strip()]
    text = str(value).strip()
    if not text:
        return []
    try:
        decoded = json.loads(text)
    except json.JSONDecodeError:
        decoded = None
    if isinstance(decoded, list):
        return [str(entry).strip() for entry in decoded if str(entry).strip()]
    return [part.strip() for part in text.split(",") if part.strip()]


def _parse_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    text = str(value).strip().lower()
    if not text:
        return False
    return text in ("1", "true", "yes", "on")


class Settings(BaseModel):
    model_config = ConfigDict(extra="allow")

    gcs_bucket_name: str = Field(..., min_length=1)
    firestore_database_id: str = "(default)"
    firestore_collection: str = "receipts"
    openai_model_name: str = "gpt-4.1-mini"
    openai_api_key: Optional[str] = None
    categories_collection: str = "categories"
    allowed_origins: List[str] = Field(default_factory=lambda: _DEFAULT_ALLOWED_ORIGINS.copy())
    require_oauth: bool = False
    oauth_client_id: Optional[str] = None
    oauth_allowed_domains: List[str] = Field(default_factory=list)
    plans_collection: str = "plans"
    users_collection: str = "users"
    helcim_api_token: Optional[str] = None
    helcim_api_base_url: str = "https://api.helcim.com/v2"
    helcim_timeout_seconds: int = 20
    helcim_approval_secret: Optional[str] = None
    helcim_approval_redirect_url: Optional[str] = None
    @field_validator("allowed_origins", mode="before")
    def _parse_allowed_origins(cls, value: Any) -> List[str]:
        parsed = _normalize_list_field(value)
        return parsed or _DEFAULT_ALLOWED_ORIGINS.copy()

    @field_validator("oauth_allowed_domains", mode="before")
    def _parse_oauth_domains(cls, value: Any) -> List[str]:
        return _normalize_list_field(value)

    @field_validator("oauth_client_id", mode="before")
    def _parse_oauth_client_id(cls, value: Any) -> Optional[str]:
        if value is None:
            return None
        text = str(value).strip()
        return text or None


@lru_cache

def get_settings() -> Settings:
    require_oauth_value = _parse_bool(os.getenv("REQUIRE_OAUTH", "false"))
    return Settings(
        gcs_bucket_name=os.getenv("GCLOUD_BUCKET_NAME", ""),
        firestore_database_id=os.getenv("FIRESTORE_DATABASE_ID", "(default)"),
        firestore_collection=os.getenv("FIRESTORE_COLLECTION_NAME", "receipts"),
        openai_model_name=os.getenv("OPENAI_MODEL_NAME", "gpt-4.1-mini"),
        openai_api_key=os.getenv("OPENAI_API_KEY"),
        categories_collection=os.getenv("CATEGORIES_COLLECTION_NAME", "categories"),
        allowed_origins=os.getenv("ALLOWED_ORIGINS", "http://localhost:3000"),
        require_oauth=require_oauth_value,
        oauth_client_id=os.getenv("OAUTH_CLIENT_ID"),
        oauth_allowed_domains=os.getenv("OAUTH_ALLOWED_DOMAINS", ""),
        plans_collection=os.getenv("PLANS_COLLECTION_NAME", "plans"),
        users_collection=os.getenv("USERS_COLLECTION_NAME", "users"),
        helcim_api_token=os.getenv("HELCIM_API_TOKEN"),
        helcim_api_base_url=os.getenv("HELCIM_API_BASE_URL", "https://api.helcim.com/v2"),
        helcim_timeout_seconds=int(os.getenv("HELCIM_TIMEOUT_SECONDS", "20")),
        helcim_approval_secret=os.getenv("HELCIM_APPROVAL_SECRET"),
        helcim_approval_redirect_url=os.getenv("HELCIM_APPROVAL_REDIRECT_URL"),
    )

