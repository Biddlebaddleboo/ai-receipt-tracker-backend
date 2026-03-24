import logging
import json
from base64 import urlsafe_b64decode
from typing import Optional

from fastapi import Depends, HTTPException, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from google.auth.transport import requests as google_auth_requests
from google.oauth2 import id_token
from pydantic import BaseModel

from app.config import Settings, get_settings

security = HTTPBearer(auto_error=False)
logger = logging.getLogger(__name__)


class AuthenticatedUser(BaseModel):
    iss: str
    sub: str
    email: str
    name: Optional[str] = None


def _decode_unverified_jwt_claims(token: str) -> Optional[dict]:
    parts = token.split(".")
    if len(parts) != 3:
        return None
    payload = parts[1]
    padding = "=" * (-len(payload) % 4)
    try:
        decoded = urlsafe_b64decode(payload + padding)
        claims = json.loads(decoded.decode("utf-8"))
    except (ValueError, json.JSONDecodeError, UnicodeDecodeError):
        return None
    if isinstance(claims, dict):
        return claims
    return None


def _allowed_oauth_audiences(settings: Settings) -> list[str]:
    audiences = [entry.strip() for entry in settings.oauth_client_ids if entry.strip()]
    legacy_client_id = (settings.oauth_client_id or "").strip()
    if legacy_client_id and legacy_client_id not in audiences:
        audiences.append(legacy_client_id)
    return audiences

async def get_current_user(
    credentials: Optional[HTTPAuthorizationCredentials] = Depends(security),
    settings: Settings = Depends(get_settings),
) -> Optional[AuthenticatedUser]:
    # OAuth is mandatory for multi-user isolation. Fail loudly if settings are malformed.
    if not hasattr(settings, "require_oauth"):
        logger.error("Settings is missing require_oauth; check deployed config.py revision")
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail="Server auth configuration is incomplete (missing require_oauth)",
        )
    if not settings.require_oauth:
        return None
    oauth_client_ids = _allowed_oauth_audiences(settings)
    if not oauth_client_ids:
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail="OAuth is required but no client ID is configured",
        )
    if not credentials or not credentials.credentials:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing bearer token",
            headers={"WWW-Authenticate": "Bearer"},
        )
    unverified_claims = _decode_unverified_jwt_claims(credentials.credentials)
    if unverified_claims is None:
        logger.warning(
            "Bearer token is not a decodable JWT; token_length=%s",
            len(credentials.credentials),
        )
    else:
        logger.warning(
            "Attempting OAuth verification with unverified claims aud=%s azp=%s iss=%s exp=%s",
            unverified_claims.get("aud"),
            unverified_claims.get("azp"),
            unverified_claims.get("iss"),
            unverified_claims.get("exp"),
        )
    try:
        request = google_auth_requests.Request()
        token_info = None
        last_error: Optional[ValueError] = None
        for oauth_client_id in oauth_client_ids:
            try:
                token_info = id_token.verify_oauth2_token(
                    credentials.credentials,
                    request,
                    audience=oauth_client_id,
                )
                logger.info(
                    "ID token verified successfully for audience=%s", oauth_client_id
                )
                break
            except ValueError as exc:
                last_error = exc
        if token_info is None:
            raise last_error or ValueError("Token verification failed")
    except ValueError as exc:
        logger.warning(
            "ID token verification failed for audiences=%s: %s",
            oauth_client_ids,
            exc,
        )
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid or expired OAuth token",
            headers={"WWW-Authenticate": "Bearer"},
        ) from exc
    email = token_info.get("email")
    if not email:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="OAuth token is missing an email address",
            headers={"WWW-Authenticate": "Bearer"},
        )
    if settings.oauth_allowed_domains:
        hosted_domain = token_info.get("hd")
        if not hosted_domain or hosted_domain not in settings.oauth_allowed_domains:
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail="OAuth token does not belong to an allowed domain",
            )
    return AuthenticatedUser(
        iss=token_info.get("iss", ""),
        sub=token_info.get("sub", ""),
        email=email,
        name=token_info.get("name"),
    )
