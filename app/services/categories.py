from __future__ import annotations

import ctypes
import json
import os
from pathlib import Path
from typing import Any, Dict, List, Optional

_LIBRARY_PATH_ENV = "GO_CATEGORIES_LIBRARY_PATH"


def _default_library_candidates() -> list[Path]:
    extension = ".so"
    if os.name == "nt":
        extension = ".dll"
    elif os.name == "darwin":
        extension = ".dylib"
    project_root = Path(__file__).resolve().parents[2]
    return [
        project_root / "native" / f"libcategoriesbridge{extension}",
        project_root / "native" / "categoriesbridge" / f"libcategoriesbridge{extension}",
    ]


def _resolve_library_path() -> Optional[Path]:
    configured = os.getenv(_LIBRARY_PATH_ENV)
    if configured:
        path = Path(configured).expanduser()
        if path.exists():
            return path
    for candidate in _default_library_candidates():
        if candidate.exists():
            return candidate
    return None


class _GoCategoriesLibrary:
    _instance: Optional["_GoCategoriesLibrary"] = None

    def __init__(self, path: Path):
        self._library = ctypes.CDLL(str(path))
        self._configure()

    def _configure(self) -> None:
        self._library.CategoriesNew.argtypes = [
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesNew.restype = ctypes.c_longlong
        self._library.CategoriesClose.argtypes = [ctypes.c_longlong]
        self._library.CategoriesClose.restype = None
        self._library.CategoriesCreate.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesCreate.restype = ctypes.c_void_p
        self._library.CategoriesList.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesList.restype = ctypes.c_void_p
        self._library.CategoriesGet.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesGet.restype = ctypes.c_void_p
        self._library.CategoriesUpdate.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesUpdate.restype = ctypes.c_void_p
        self._library.CategoriesDelete.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.CategoriesDelete.restype = ctypes.c_void_p
        self._library.CategoriesFree.argtypes = [ctypes.c_void_p]
        self._library.CategoriesFree.restype = None

    @classmethod
    def load(cls) -> "_GoCategoriesLibrary":
        if cls._instance:
            return cls._instance
        path = _resolve_library_path()
        if path is None:
            raise RuntimeError(
                f"Go categories library not found. Set {_LIBRARY_PATH_ENV} or ship the shared library."
            )
        cls._instance = cls(path)
        return cls._instance

    def free(self, ptr: Optional[int]) -> None:
        if ptr:
            self._library.CategoriesFree(ctypes.c_void_p(ptr))

    def take_string(self, ptr: Optional[int]) -> str:
        if not ptr:
            return ""
        try:
            return ctypes.string_at(ptr).decode("utf-8")
        finally:
            self.free(ptr)

    def raise_on_error(self, err_ptr: ctypes.c_void_p) -> None:
        if err_ptr.value:
            message = self.take_string(err_ptr.value)
            raise RuntimeError(message or "native categories bridge failed")


class CategoryService:
    def __init__(self, collection_name: str, database_id: str = "(default)"):
        self._bridge = _GoCategoriesLibrary.load()
        err_ptr = ctypes.c_void_p()
        self._handle = self._bridge._library.CategoriesNew(
            collection_name.encode("utf-8"),
            database_id.encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr)
        if not self._handle:
            raise RuntimeError("native categories bridge returned invalid handle")

    def close(self) -> None:
        if getattr(self, "_handle", 0):
            self._bridge._library.CategoriesClose(self._handle)
            self._handle = 0

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def _call_json(self, func, *args) -> Any:
        err_ptr = ctypes.c_void_p()
        ptr = func(*args, ctypes.byref(err_ptr))
        self._bridge.raise_on_error(err_ptr)
        payload = self._bridge.take_string(ptr)
        return json.loads(payload) if payload else None

    def create_category(self, payload: Dict[str, Any], owner_email: str) -> str:
        err_ptr = ctypes.c_void_p()
        ptr = self._bridge._library.CategoriesCreate(
            self._handle,
            owner_email.encode("utf-8"),
            json.dumps(payload).encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr)
        return self._bridge.take_string(ptr)

    def list_categories(self, owner_email: str) -> List[Dict[str, Any]]:
        payload = self._call_json(
            self._bridge._library.CategoriesList,
            self._handle,
            owner_email.encode("utf-8"),
        )
        return payload or []

    def get_category(self, category_id: str, owner_email: str) -> Dict[str, Any]:
        payload = self._call_json(
            self._bridge._library.CategoriesGet,
            self._handle,
            owner_email.encode("utf-8"),
            category_id.encode("utf-8"),
        )
        return payload or {}

    def update_category(self, category_id: str, payload: Dict[str, Any], owner_email: str) -> None:
        self._call_json(
            self._bridge._library.CategoriesUpdate,
            self._handle,
            owner_email.encode("utf-8"),
            category_id.encode("utf-8"),
            json.dumps(payload).encode("utf-8"),
        )

    def delete_category(self, category_id: str, owner_email: str) -> None:
        self._call_json(
            self._bridge._library.CategoriesDelete,
            self._handle,
            owner_email.encode("utf-8"),
            category_id.encode("utf-8"),
        )
