from __future__ import annotations

from typing import Any, Dict, List, Optional, Tuple

from google.cloud import firestore
from google.api_core import exceptions as google_exceptions


class FirestoreClient:
    def __init__(self, collection_name: str):
        self._client = firestore.Client()
        self._collection = self._client.collection(collection_name)

    def insert_receipt(self, payload: Dict[str, Any]) -> str:
        doc_ref = self._collection.document()
        doc_ref.set(payload)
        return doc_ref.id

    def get_receipt(self, receipt_id: str) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Receipt {receipt_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Receipt {receipt_id} not found")
        return snapshot.to_dict()

    def delete_receipt(self, receipt_id: str) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Receipt {receipt_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Receipt {receipt_id} not found")
        data = snapshot.to_dict() or {}
        doc_ref.delete()
        return data

    def update_receipt(self, receipt_id: str, payload: Dict[str, Any]) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        try:
            doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Receipt {receipt_id} not found") from exc
        doc_ref.update(payload)
        return doc_ref.get().to_dict()

    def list_receipts(
        self, limit: int = 10, start_after_id: Optional[str] = None
    ) -> Tuple[List[Dict[str, Any]], Optional[str]]:
        query = (
            self._collection.order_by("created_at", direction=firestore.Query.DESCENDING)
            .limit(limit)
        )
        if start_after_id:
            after_doc = self._collection.document(start_after_id)
            try:
                after_snapshot = after_doc.get()
            except google_exceptions.NotFound as exc:
                raise KeyError(f"Receipt {start_after_id} not found") from exc
            if not after_snapshot.exists:
                raise KeyError(f"Receipt {start_after_id} not found")
            query = query.start_after(after_snapshot)

        docs: List[Dict[str, Any]] = []
        for snapshot in query.stream():
            data = snapshot.to_dict()
            docs.append({"id": snapshot.id, **(data or {})})

        next_cursor = docs[-1]["id"] if docs else None
        return docs, next_cursor
