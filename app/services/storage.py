from __future__ import annotations

import ctypes
import mimetypes
import os
from pathlib import Path
from typing import Optional, Tuple

_LIBRARY_PATH_ENV = "GO_STORAGE_LIBRARY_PATH"


def _default_library_candidates() -> list[Path]:
    extension = ".so"
    if os.name == "nt":
        extension = ".dll"
    elif os.name == "darwin":
        extension = ".dylib"
    project_root = Path(__file__).resolve().parents[2]
    return [
        project_root / "native" / f"libstoragebridge{extension}",
        project_root / "native" / "storagebridge" / f"libstoragebridge{extension}",
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


class _GoStorageLibrary:
    _instance: Optional["_GoStorageLibrary"] = None

    def __init__(self, path: Path):
        self._library = ctypes.CDLL(str(path))
        self._configure()

    def _configure(self) -> None:
        self._library.StorageNew.argtypes = [
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.StorageNew.restype = ctypes.c_longlong
        self._library.StorageClose.argtypes = [ctypes.c_longlong]
        self._library.StorageClose.restype = None
        self._library.StorageUpload.argtypes = [
            ctypes.c_longlong,
            ctypes.POINTER(ctypes.c_ubyte),
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.StorageUpload.restype = ctypes.c_void_p
        self._library.StorageDownload.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
            ctypes.POINTER(ctypes.c_longlong),
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.StorageDownload.restype = ctypes.c_void_p
        self._library.StorageDelete.argtypes = [
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.StorageDelete.restype = ctypes.c_int
        self._library.StorageFree.argtypes = [ctypes.c_void_p]
        self._library.StorageFree.restype = None

    @classmethod
    def load(cls) -> "_GoStorageLibrary":
        if cls._instance is not None:
            return cls._instance
        path = _resolve_library_path()
        if path is None:
            raise RuntimeError(
                f"Go storage library not found. Set {_LIBRARY_PATH_ENV} or ship the shared library."
            )
        cls._instance = cls(path)
        return cls._instance

    def free(self, ptr: int | None) -> None:
        if ptr:
            self._library.StorageFree(ctypes.c_void_p(ptr))

    def take_string(self, ptr: int | None) -> str:
        if not ptr:
            return ""
        try:
            return ctypes.string_at(ptr).decode("utf-8")
        finally:
            self.free(ptr)

    def raise_on_error(self, err_ptr: ctypes.c_void_p) -> None:
        if err_ptr.value:
            message = self.take_string(err_ptr.value)
            raise RuntimeError(message or "native storage bridge failed")


class GCSStorageClient:
    def __init__(self, bucket_name: str):
        self._bridge = _GoStorageLibrary.load()
        self._handle = self._new_handle(bucket_name)

    def _new_handle(self, bucket_name: str) -> int:
        err_ptr = ctypes.c_void_p()
        handle = self._bridge._library.StorageNew(
            bucket_name.encode("utf-8"), ctypes.byref(err_ptr)
        )
        self._bridge.raise_on_error(err_ptr)
        if not handle:
            raise RuntimeError("native storage bridge returned an invalid handle")
        return handle

    def close(self) -> None:
        if getattr(self, "_handle", 0):
            self._bridge._library.StorageClose(self._handle)
            self._handle = 0

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def upload(
        self, data: bytes, destination_path: str, content_type: Optional[str] = None
    ) -> str:
        err_ptr = ctypes.c_void_p()
        buffer_ptr = None
        length = len(data)
        if length:
            buffer = (ctypes.c_ubyte * length).from_buffer_copy(data)
            buffer_ptr = buffer
        content_type_bytes = None
        if content_type:
            content_type_bytes = content_type.encode("utf-8")
        url_ptr = self._bridge._library.StorageUpload(
            self._handle,
            buffer_ptr,
            length,
            destination_path.encode("utf-8"),
            content_type_bytes,
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr)
        return self._bridge.take_string(url_ptr)

    def download(self, destination_path: str) -> Tuple[bytes, str]:
        err_ptr = ctypes.c_void_p()
        content_type_ptr = ctypes.c_void_p()
        data_len = ctypes.c_longlong()
        data_ptr = self._bridge._library.StorageDownload(
            self._handle,
            destination_path.encode("utf-8"),
            ctypes.byref(content_type_ptr),
            ctypes.byref(data_len),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr)
        try:
            payload = ctypes.string_at(data_ptr, data_len.value) if data_ptr else b""
        finally:
            self._bridge.free(data_ptr)
        content_type = self._bridge.take_string(content_type_ptr.value)
        return payload, content_type or self._guess_mime_type(destination_path)

    def delete(self, destination_path: str) -> None:
        err_ptr = ctypes.c_void_p()
        result = self._bridge._library.StorageDelete(
            self._handle,
            destination_path.encode("utf-8"),
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr)
        if not result:
            raise RuntimeError("native storage bridge failed to delete the object")

    @staticmethod
    def _guess_mime_type(path: str) -> str:
        guessed, _ = mimetypes.guess_type(path)
        return guessed or "application/octet-stream"
