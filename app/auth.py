import logging
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
    oauth_client_id = (settings.oauth_client_id or "").strip()
    if not oauth_client_id:
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
    try:
        request = google_auth_requests.Request()
        token_info = id_token.verify_oauth2_token(
            credentials.credentials,
            request,
            audience=oauth_client_id,
        )
    except ValueError as exc:
        logger.warning("ID token verification failed: %s", exc)
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
