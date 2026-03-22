from __future__ import annotations

from typing import Dict, Any, List, Optional

from google.cloud import firestore
from google.api_core import exceptions as google_exceptions


class CategoryService:
    def __init__(self, collection_name: str):
        self._client = firestore.Client()
        self._collection = self._client.collection(collection_name)

    def list_categories(self) -> List[Dict[str, Any]]:
        return [
            {"id": doc.id, **(doc.to_dict() or {})}
            for doc in self._collection.order_by("name").stream()
        ]

    def category_names(self) -> List[str]:
        return [doc.get("name") for doc in self.list_categories() if doc.get("name")]

    def create_category(self, payload: Dict[str, Any]) -> str:
        doc_ref = self._collection.document()
        doc_ref.set(payload)
        return doc_ref.id

    def get_category(self, category_id: str) -> Dict[str, Any]:
        doc_ref = self._collection.document(category_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Category {category_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Category {category_id} not found")
        data = snapshot.to_dict() or {}
        return {"id": snapshot.id, **data}

    def update_category(self, category_id: str, payload: Dict[str, Any]) -> None:
        doc_ref = self._collection.document(category_id)
        try:
            doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Category {category_id} not found") from exc
        doc_ref.update(payload)

    def delete_category(self, category_id: str) -> None:
        doc_ref = self._collection.document(category_id)
        try:
            doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Category {category_id} not found") from exc
        doc_ref.delete()
