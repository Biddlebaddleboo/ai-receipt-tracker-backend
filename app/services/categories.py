from __future__ import annotations

from typing import Any, Dict, List

from google.cloud import firestore
from google.api_core import exceptions as google_exceptions


class CategoryService:
    def __init__(self, collection_name: str):
        self._client = firestore.Client()
        self._collection = self._client.collection(collection_name)

    def list_categories(self) -> List[Dict[str, Any]]:
        categories: List[Dict[str, Any]] = []
        for doc in self._collection.stream():
            data = doc.to_dict() or {}
            normalized = self._normalize_category(doc.id, data)
            if normalized is not None:
                categories.append(normalized)
        return sorted(categories, key=lambda category: category["name"].lower())

    def category_names(self) -> List[str]:
        return [doc.get("name") for doc in self.list_categories() if doc.get("name")]

    def create_category(self, payload: Dict[str, Any]) -> str:
        doc_ref = self._collection.document()
        doc_ref.set(self._sanitize_payload(payload))
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
        normalized = self._normalize_category(snapshot.id, data)
        if normalized is None:
            raise KeyError(f"Category {category_id} not found")
        return normalized

    def update_category(self, category_id: str, payload: Dict[str, Any]) -> None:
        doc_ref = self._collection.document(category_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Category {category_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Category {category_id} not found")
        doc_ref.update(self._sanitize_payload(payload))

    def delete_category(self, category_id: str) -> None:
        doc_ref = self._collection.document(category_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Category {category_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Category {category_id} not found")
        doc_ref.delete()

    @staticmethod
    def _sanitize_payload(payload: Dict[str, Any]) -> Dict[str, Any]:
        name = str(payload.get("name", "")).strip()
        if not name:
            raise ValueError("Category name is required")
        description = payload.get("description")
        cleaned: Dict[str, Any] = {"name": name}
        if description is not None:
            cleaned["description"] = str(description).strip() or None
        return cleaned

    @staticmethod
    def _normalize_category(category_id: str, data: Dict[str, Any]) -> Dict[str, Any] | None:
        name = data.get("name")
        if not isinstance(name, str) or not name.strip():
            return None
        description = data.get("description")
        normalized: Dict[str, Any] = {"id": category_id, "name": name.strip()}
        if description is not None:
            normalized["description"] = str(description).strip() or None
        return normalized
