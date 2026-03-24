import ctypes
import json
import logging
from base64 import urlsafe_b64decode
from typing import Optional

from fastapi import Depends, HTTPException, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from pydantic import BaseModel

from app.config import Settings, get_settings
from app.services._native_bridge import NativeLibraryBase

security = HTTPBearer(auto_error=False)
logger = logging.getLogger(__name__)


class AuthenticatedUser(BaseModel):
    iss: str
    sub: str
    email: str
    name: Optional[str] = None


class _GoAuthLibrary(NativeLibraryBase):
    _instance: Optional["_GoAuthLibrary"] = None
    env_var = "GO_AUTH_LIBRARY_PATH"
    library_stem = "libauthbridge"
    missing_label = "Go auth library"
    free_symbol = "AuthFree"

    def _configure(self) -> None:
        self._library.AuthVerifyIDToken.argtypes = [
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.AuthVerifyIDToken.restype = ctypes.c_void_p
        self._library.AuthFree.argtypes = [ctypes.c_void_p]
        self._library.AuthFree.restype = None


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
    configured_client_ids = getattr(settings, "oauth_client_ids", None) or []
    audiences = [entry.strip() for entry in configured_client_ids if str(entry).strip()]
    legacy_client_id = (getattr(settings, "oauth_client_id", None) or "").strip()
    if legacy_client_id and legacy_client_id not in audiences:
        audiences.append(legacy_client_id)
    return audiences


def _verify_token_with_go(
    token: str,
    audiences: list[str],
    allowed_domains: list[str],
) -> dict:
    bridge = _GoAuthLibrary.load()
    err_ptr = ctypes.c_void_p()
    result_ptr = bridge._library.AuthVerifyIDToken(
        token.encode("utf-8"),
        json.dumps(audiences).encode("utf-8"),
        json.dumps(allowed_domains).encode("utf-8"),
        ctypes.byref(err_ptr),
    )
    if err_ptr.value:
        message = bridge.take_string(err_ptr.value)
        raise RuntimeError(message or "native auth bridge failed")
    payload = bridge.take_string(result_ptr)
    return json.loads(payload) if payload else {}


def _raise_http_for_native_auth_error(
    message: str,
    oauth_client_ids: list[str],
) -> None:
    if message.startswith("invalid_token"):
        logger.warning(
            "ID token verification failed for audiences=%s: %s",
            oauth_client_ids,
            message,
        )
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid or expired OAuth token",
            headers={"WWW-Authenticate": "Bearer"},
        )
    if message.startswith("missing_email"):
        logger.warning("Verified OAuth token is missing an email address")
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="OAuth token is missing an email address",
            headers={"WWW-Authenticate": "Bearer"},
        )
    if message.startswith("forbidden_domain"):
        logger.warning("OAuth token belongs to a disallowed hosted domain")
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="OAuth token does not belong to an allowed domain",
        )
    logger.error("Native auth bridge failed unexpectedly: %s", message)
    raise HTTPException(
        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
        detail="Server auth configuration is incomplete",
    )


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
        token_info = _verify_token_with_go(
            credentials.credentials,
            oauth_client_ids,
            settings.oauth_allowed_domains,
        )
    except RuntimeError as exc:
        _raise_http_for_native_auth_error(str(exc), oauth_client_ids)

    logger.info(
        "ID token verified successfully for audience=%s",
        token_info.get("audience", ""),
    )
    return AuthenticatedUser(
        iss=token_info.get("iss", ""),
        sub=token_info.get("sub", ""),
        email=token_info.get("email", ""),
        name=token_info.get("name"),
    )
