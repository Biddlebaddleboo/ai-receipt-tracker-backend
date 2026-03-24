from __future__ import annotations

import logging

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.config import get_settings
from app.router_billing import router as billing_router
from app.router_system import router as system_router
from app.router_users import router as users_router


def create_app() -> FastAPI:
    settings = get_settings()
    app = FastAPI(title="Receipt Scanner API")
    logger = logging.getLogger("app.cors")

    cors_kwargs = {
        "allow_origins": settings.allowed_origins,
        "allow_credentials": True,
        "allow_methods": ["*"],
        "allow_headers": ["*"],
    }
    if settings.allowed_origin_regex:
        cors_kwargs["allow_origin_regex"] = settings.allowed_origin_regex
    app.add_middleware(CORSMiddleware, **cors_kwargs)
    logger.warning(
        "CORS configured allow_origins=%s allow_origin_regex=%s",
        settings.allowed_origins,
        settings.allowed_origin_regex,
    )

    app.include_router(system_router)
    app.include_router(users_router)
    app.include_router(billing_router)
    return app
