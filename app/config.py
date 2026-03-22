from typing import Iterable, List, Optional, Sequence, Union
import json

from dotenv import load_dotenv
from pydantic import BaseSettings, Field, validator

load_dotenv()


class Settings(BaseSettings):
    gcs_bucket_name: str = Field(..., env="GCLOUD_BUCKET_NAME")
    firestore_collection: str = Field("receipts", env="FIRESTORE_COLLECTION_NAME")
    openai_model_name: str = Field("gpt-4.1-mini", env="OPENAI_MODEL_NAME")
    openai_api_key: Optional[str] = Field(None, env="OPENAI_API_KEY")
    categories_collection: str = Field("categories", env="CATEGORIES_COLLECTION_NAME")
    allowed_origins: List[str] = Field(
        default_factory=lambda: ["http://localhost:3000"], env="ALLOWED_ORIGINS"
    )

    @validator("allowed_origins", pre=True)
    def _split_origins(
        cls, value: Union[str, Sequence[str], None]
    ) -> List[str]:
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
                value = decoded
            else:
                return [
                    origin.strip()
                    for origin in value.split(",")
                    if origin.strip()
                ]
        if isinstance(value, Sequence):
            return [
                origin.strip()
                for origin in value
                if isinstance(origin, str) and origin.strip()
            ]
        return [str(value).strip()] if str(value).strip() else []

    class Config:
        case_sensitive = False
        env_file = ".env"


def get_settings() -> Settings:
    return Settings()
