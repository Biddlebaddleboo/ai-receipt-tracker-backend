import json
import os
import re
from functools import lru_cache
from typing import Any, List, Optional
from urllib.parse import urlparse

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


def _firebase_preview_origin_pattern(origin: str) -> Optional[str]:
    try:
        parsed = urlparse(origin)
    except ValueError:
        return None
    scheme = parsed.scheme.lower()
    if scheme not in ("http", "https"):
        return None
    host = parsed.netloc.lower()
    for suffix in (".web.app", ".firebaseapp.com"):
        if not host.endswith(suffix):
            continue
        base = host[: -len(suffix)]
        if not base:
            continue
        base = base.rstrip("-")
        if not base:
            continue
        escaped_base = re.escape(base)
        pattern = f"{scheme}://{escaped_base}(?:--preview-[0-9a-z]+)?{re.escape(suffix)}"
        return pattern
    return None


def _build_preview_regex(origins: List[str]) -> Optional[str]:
    patterns: List[str] = []
    for origin in origins:
        pattern = _firebase_preview_origin_pattern(origin)
        if pattern:
            patterns.append(pattern)
    if not patterns:
        return None
    inner = "|".join(patterns)
    return f"^(?:{inner})$"


def _strip_regex_anchors(pattern: str) -> str:
    cleaned = pattern.strip()
    if cleaned.startswith("^"):
        cleaned = cleaned[1:]
    if cleaned.endswith("$"):
        cleaned = cleaned[:-1]
    return cleaned


def _merge_origin_regex(
    configured_regex: Optional[str], preview_regex: Optional[str]
) -> Optional[str]:
    patterns: List[str] = []
    for candidate in (configured_regex, preview_regex):
        if not candidate:
            continue
        cleaned = _strip_regex_anchors(candidate)
        if cleaned and cleaned not in patterns:
            patterns.append(cleaned)
    if not patterns:
        return None
    if len(patterns) == 1:
        return f"^{patterns[0]}$"
    return f"^(?:{'|'.join(f'(?:{pattern})' for pattern in patterns)})$"


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
    helcim_user_agent: str = "ai-receipt-tracker-backend/1.0"
    helcim_approval_secret: Optional[str] = None
    helcim_approval_redirect_url: Optional[str] = None
    allowed_origin_regex: Optional[str] = None
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
    allowed_origins_value = os.getenv("ALLOWED_ORIGINS", "http://localhost:3000")
    normalized_origins = _normalize_list_field(allowed_origins_value)
    configured_regex = os.getenv("ALLOWED_ORIGIN_REGEX")
    preview_regex = _build_preview_regex(normalized_origins)
    allowed_origin_regex_value = _merge_origin_regex(configured_regex, preview_regex)
    return Settings(
        gcs_bucket_name=os.getenv("GCLOUD_BUCKET_NAME", ""),
        firestore_database_id=os.getenv("FIRESTORE_DATABASE_ID", "(default)"),
        firestore_collection=os.getenv("FIRESTORE_COLLECTION_NAME", "receipts"),
        openai_model_name=os.getenv("OPENAI_MODEL_NAME", "gpt-4.1-mini"),
        openai_api_key=os.getenv("OPENAI_API_KEY"),
        categories_collection=os.getenv("CATEGORIES_COLLECTION_NAME", "categories"),
        allowed_origins=normalized_origins,
        require_oauth=require_oauth_value,
        oauth_client_id=os.getenv("OAUTH_CLIENT_ID"),
        oauth_allowed_domains=os.getenv("OAUTH_ALLOWED_DOMAINS", ""),
        plans_collection=os.getenv("PLANS_COLLECTION_NAME", "plans"),
        users_collection=os.getenv("USERS_COLLECTION_NAME", "users"),
        helcim_api_token=os.getenv("HELCIM_API_TOKEN"),
        helcim_api_base_url=os.getenv("HELCIM_API_BASE_URL", "https://api.helcim.com/v2"),
        helcim_timeout_seconds=int(os.getenv("HELCIM_TIMEOUT_SECONDS", "20")),
        helcim_user_agent=os.getenv("HELCIM_USER_AGENT", "ai-receipt-tracker-backend/1.0"),
        helcim_approval_secret=os.getenv("HELCIM_APPROVAL_SECRET"),
        helcim_approval_redirect_url=os.getenv("HELCIM_APPROVAL_REDIRECT_URL"),
        allowed_origin_regex=allowed_origin_regex_value,
    )

