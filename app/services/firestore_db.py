from __future__ import annotations

import ctypes
import json
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple

from app.services._native_bridge import NativeLibraryBase

OWNER_FIELD = "owner_email"
_DATE_MARKER = "__firestorebridge_datetime__"


def _normalize_datetime(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    else:
        value = value.astimezone(timezone.utc)
    return value.isoformat().replace("+00:00", "Z")


def _encode_bridge_value(value: Any) -> Any:
    if isinstance(value, datetime):
        return {_DATE_MARKER: _normalize_datetime(value)}
    if isinstance(value, dict):
        return {str(key): _encode_bridge_value(item) for key, item in value.items()}
    if isinstance(value, list):
        return [_encode_bridge_value(item) for item in value]
    return value


def _encode_bridge_json(payload: Dict[str, Any]) -> bytes:
    return json.dumps(_encode_bridge_value(payload), separators=(",", ":")).encode("utf-8")


class _GoFirestoreLibrary(NativeLibraryBase):
    _instance: Optional["_GoFirestoreLibrary"] = None
    env_var = "GO_FIRESTORE_LIBRARY_PATH"
    library_stem = "libfirestorebridge"
    missing_label = "Go Firestore library"
    free_symbol = "FirestoreFree"

    def _configure(self) -> None:
        self._library.FirestoreNew.argtypes = [
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreNew.restype = ctypes.c_longlong
        self._library.FirestoreClose.argtypes = [ctypes.c_longlong]
        self._library.FirestoreClose.restype = None
        self._library.FirestoreInsertReceipt.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreInsertReceipt.restype = ctypes.c_void_p
        self._library.FirestoreGetReceipt.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreGetReceipt.restype = ctypes.c_void_p
        self._library.FirestoreDeleteReceipt.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreDeleteReceipt.restype = ctypes.c_void_p
        self._library.FirestoreUpdateReceipt.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreUpdateReceipt.restype = ctypes.c_void_p
        self._library.FirestoreListReceipts.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreListReceipts.restype = ctypes.c_void_p
        self._library.FirestoreCountReceiptsByOwner.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.FirestoreCountReceiptsByOwner.restype = ctypes.c_longlong
        self._library.FirestoreFree.argtypes = [ctypes.c_void_p]
        self._library.FirestoreFree.restype = None

    @staticmethod
    def _translate_error(message: str) -> Exception | None:
        if message.startswith("Receipt ") and message.endswith(" not found"):
            return KeyError(message)
        return None


class FirestoreClient:
    def __init__(self, collection_name: str, database_id: str = "(default)"):
        self._bridge = _GoFirestoreLibrary.load()
        err_ptr = ctypes.c_void_p()
        self._handle = self._bridge._library.FirestoreNew(
            collection_name.encode("utf-8"),
            (database_id or "(default)").encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        if not self._handle:
            raise RuntimeError("native firestore bridge returned an invalid handle")

    def close(self) -> None:
        if getattr(self, "_handle", 0):
            self._bridge._library.FirestoreClose(self._handle)
            self._handle = 0

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def insert_receipt(self, owner_email: str, payload: Dict[str, Any]) -> str:
        err_ptr = ctypes.c_void_p()
        doc_id_ptr = self._bridge._library.FirestoreInsertReceipt(
            self._handle,
            owner_email.encode("utf-8"),
            _encode_bridge_json(payload),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        return self._bridge.take_string(doc_id_ptr)

    def get_receipt(self, receipt_id: str, owner_email: str) -> Dict[str, Any]:
        err_ptr = ctypes.c_void_p()
        payload_ptr = self._bridge._library.FirestoreGetReceipt(
            self._handle,
            receipt_id.encode("utf-8"),
            owner_email.encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        payload = self._bridge.take_string(payload_ptr)
        return json.loads(payload) if payload else {}

    def delete_receipt(self, receipt_id: str, owner_email: str) -> Dict[str, Any]:
        err_ptr = ctypes.c_void_p()
        payload_ptr = self._bridge._library.FirestoreDeleteReceipt(
            self._handle,
            receipt_id.encode("utf-8"),
            owner_email.encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        payload = self._bridge.take_string(payload_ptr)
        return json.loads(payload) if payload else {}

    def update_receipt(
        self, receipt_id: str, payload: Dict[str, Any], owner_email: str
    ) -> Dict[str, Any]:
        update_payload = dict(payload)
        update_payload.pop(OWNER_FIELD, None)
        err_ptr = ctypes.c_void_p()
        payload_ptr = self._bridge._library.FirestoreUpdateReceipt(
            self._handle,
            receipt_id.encode("utf-8"),
            owner_email.encode("utf-8"),
            _encode_bridge_json(update_payload),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        raw = self._bridge.take_string(payload_ptr)
        return json.loads(raw) if raw else {}

    def list_receipts(
        self,
        owner_email: str,
        limit: int = 10,
        start_after_id: Optional[str] = None,
    ) -> Tuple[List[Dict[str, Any]], Optional[str]]:
        err_ptr = ctypes.c_void_p()
        start_after_bytes = start_after_id.encode("utf-8") if start_after_id else None
        payload_ptr = self._bridge._library.FirestoreListReceipts(
            self._handle,
            owner_email.encode("utf-8"),
            limit,
            start_after_bytes,
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        raw = self._bridge.take_string(payload_ptr)
        if not raw:
            return [], None
        payload = json.loads(raw)
        return payload.get("docs", []), payload.get("next_cursor")

    def count_receipts_by_owner(
        self,
        owner_email: str,
        start: Optional[datetime] = None,
        end: Optional[datetime] = None,
    ) -> int:
        err_ptr = ctypes.c_void_p()
        start_bytes = _normalize_datetime(start).encode("utf-8") if start else None
        end_bytes = _normalize_datetime(end).encode("utf-8") if end else None
        count = self._bridge._library.FirestoreCountReceiptsByOwner(
            self._handle,
            owner_email.encode("utf-8"),
            start_bytes,
            end_bytes,
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(
            err_ptr,
            "native firestore bridge failed",
            translate_error=self._bridge._translate_error,
        )
        return int(count)
