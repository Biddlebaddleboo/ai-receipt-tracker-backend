from __future__ import annotations

from typing import Dict

from fastapi import APIRouter

router = APIRouter()


@router.get("/healthz")
def health() -> Dict[str, str]:
    return {"status": "ok"}
