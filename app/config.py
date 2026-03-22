import json
import os
from functools import lru_cache
from typing import List, Optional

from dotenv import load_dotenv
from pydantic import BaseModel, Field, validator

load_dotenv()


class Settings(BaseModel):
    gcs_bucket_name: str = Field(..., min_length=1)
    firestore_database_id: str = "(default)"
    firestore_collection: str = "receipts"
    openai_model_name: str = "gpt-4.1-mini"
    openai_api_key: Optional[str] = None
    categories_collection: str = "categories"
    allowed_origins: List[str] = Field(
        default_factory=lambda: ["http://localhost:3000"]
    )

    @validator("allowed_origins", pre=True)
    def _split_origins(cls, value):
        if value is None:
            return []
        if isinstance(value, str):
            cleaned = value.strip()
            if not cleaned:
                return []
            try:
                decoded = json.loads(cleaned)
            except json.JSONDecodeError:
                decoded = None
            if isinstance(decoded, list):
                return [
                    origin.strip()
                    for origin in decoded
                    if isinstance(origin, str) and origin.strip()
                ]
            return [origin.strip() for origin in cleaned.split(",") if origin.strip()]
        if isinstance(value, list):
            return [
                origin.strip()
                for origin in value
                if isinstance(origin, str) and origin.strip()
            ]
        return [str(value).strip()] if str(value).strip() else []


@lru_cache
def get_settings() -> Settings:
    return Settings(
        gcs_bucket_name=os.getenv("GCLOUD_BUCKET_NAME", ""),
        firestore_database_id=os.getenv("FIRESTORE_DATABASE_ID", "(default)"),
        firestore_collection=os.getenv("FIRESTORE_COLLECTION_NAME", "receipts"),
        openai_model_name=os.getenv("OPENAI_MODEL_NAME", "gpt-4.1-mini"),
        openai_api_key=os.getenv("OPENAI_API_KEY"),
        categories_collection=os.getenv("CATEGORIES_COLLECTION_NAME", "categories"),
        allowed_origins=os.getenv("ALLOWED_ORIGINS", "http://localhost:3000"),
    )
