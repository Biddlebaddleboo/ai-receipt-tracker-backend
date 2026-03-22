from __future__ import annotations

from typing import Any, Dict, List, Optional, Tuple

from google.cloud import firestore
from google.api_core import exceptions as google_exceptions

OWNER_FIELD = "owner_email"


class FirestoreClient:
    def __init__(self, collection_name: str, database_id: str = "(default)"):
        self._client = firestore.Client(database=database_id)
        self._collection = self._client.collection(collection_name)

    def insert_receipt(self, owner_email: str, payload: Dict[str, Any]) -> str:
        payload_with_owner = {**payload, OWNER_FIELD: owner_email}
        doc_ref = self._collection.document()
        doc_ref.set(payload_with_owner)
        return doc_ref.id

    def get_receipt(self, receipt_id: str, owner_email: str) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Receipt {receipt_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Receipt {receipt_id} not found")
        data = snapshot.to_dict() or {}
        if data.get(OWNER_FIELD) != owner_email:
            raise KeyError(f"Receipt {receipt_id} not found")
        return data

    def delete_receipt(self, receipt_id: str, owner_email: str) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        data = self.get_receipt(receipt_id, owner_email)
        doc_ref.delete()
        return data

    def update_receipt(
        self, receipt_id: str, payload: Dict[str, Any], owner_email: str
    ) -> Dict[str, Any]:
        doc_ref = self._collection.document(receipt_id)
        try:
            snapshot = doc_ref.get()
        except google_exceptions.NotFound as exc:
            raise KeyError(f"Receipt {receipt_id} not found") from exc
        if not snapshot.exists:
            raise KeyError(f"Receipt {receipt_id} not found")
        existing = snapshot.to_dict() or {}
        if existing.get(OWNER_FIELD) != owner_email:
            raise KeyError(f"Receipt {receipt_id} not found")
        payload.pop(OWNER_FIELD, None)
        doc_ref.update(payload)
        return doc_ref.get().to_dict()

    def list_receipts(
        self,
        owner_email: str,
        limit: int = 10,
        start_after_id: Optional[str] = None,
    ) -> Tuple[List[Dict[str, Any]], Optional[str]]:
        query = (
            self._collection.where(OWNER_FIELD, "==", owner_email)
            .order_by("created_at", direction=firestore.Query.DESCENDING)
            .limit(limit)
        )
        after_snapshot = None
        if start_after_id:
            after_doc = self._collection.document(start_after_id)
            try:
                after_snapshot = after_doc.get()
            except google_exceptions.NotFound as exc:
                raise KeyError(f"Receipt {start_after_id} not found") from exc
            if not after_snapshot.exists:
                raise KeyError(f"Receipt {start_after_id} not found")
            after_data = after_snapshot.to_dict() or {}
            if after_data.get(OWNER_FIELD) != owner_email:
                raise KeyError(f"Receipt {start_after_id} not found")
            query = query.start_after(after_snapshot)

        docs: List[Dict[str, Any]] = []
        for snapshot in query.stream():
            data = snapshot.to_dict() or {}
            docs.append({"id": snapshot.id, **data})

        next_cursor = docs[-1]["id"] if docs else None
        return docs, next_cursor
