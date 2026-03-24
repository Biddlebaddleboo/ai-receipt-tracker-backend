from __future__ import annotations

import ctypes
import json
from typing import List, Optional, Sequence

from pydantic import BaseModel, Field

from app.services._native_bridge import NativeLibraryBase


class OCRItem(BaseModel):
    name: Optional[str] = None
    quantity: Optional[float] = None
    price: Optional[float] = None


class OCRResult(BaseModel):
    text: str
    vendor: Optional[str] = None
    subtotal: Optional[float] = None
    tax: Optional[float] = None
    total: Optional[float] = None
    category: Optional[str] = None
    purchase_date: Optional[str] = None
    items: List[OCRItem] = Field(default_factory=list)


class _GoOCRLibrary(NativeLibraryBase):
    _instance: Optional["_GoOCRLibrary"] = None
    env_var = "GO_OCR_LIBRARY_PATH"
    library_stem = "libocrbridge"
    missing_label = "Go OCR library"
    free_symbol = "OCRFree"

    def _configure(self) -> None:
        self._library.OCRNew.argtypes = [
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.OCRNew.restype = ctypes.c_longlong
        self._library.OCRClose.argtypes = [ctypes.c_longlong]
        self._library.OCRClose.restype = None
        self._library.OCRExtract.argtypes = [
            ctypes.c_longlong,
            ctypes.POINTER(ctypes.c_ubyte),
            ctypes.c_longlong,
            ctypes.c_char_p,
            ctypes.c_char_p,
            ctypes.POINTER(ctypes.c_void_p),
        ]
        self._library.OCRExtract.restype = ctypes.c_void_p
        self._library.OCRFree.argtypes = [ctypes.c_void_p]
        self._library.OCRFree.restype = None


class OpenAITextExtractor:
    def __init__(self, model: str, api_key: Optional[str] = None):
        self._bridge = _GoOCRLibrary.load()
        err_ptr = ctypes.c_void_p()
        api_key_bytes = api_key.encode("utf-8") if api_key else None
        self._handle = self._bridge._library.OCRNew(
            model.encode("utf-8"),
            api_key_bytes,
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr, "native OCR bridge failed")
        if not self._handle:
            raise RuntimeError("native OCR bridge returned an invalid handle")

    def close(self) -> None:
        if getattr(self, "_handle", 0):
            self._bridge._library.OCRClose(self._handle)
            self._handle = 0

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def extract(
        self,
        image_bytes: bytes,
        instructions: Optional[str] = None,
        category_options: Optional[Sequence[str]] = None,
    ) -> OCRResult:
        err_ptr = ctypes.c_void_p()
        length = len(image_bytes)
        buffer_ptr = None
        if length:
            buffer_ptr = (ctypes.c_ubyte * length).from_buffer_copy(image_bytes)
        instruction_bytes = instructions.encode("utf-8") if instructions else None
        category_payload = None
        if category_options is not None:
            category_payload = json.dumps(list(category_options)).encode("utf-8")
        result_ptr = self._bridge._library.OCRExtract(
            self._handle,
            buffer_ptr,
            length,
            instruction_bytes,
            category_payload,
            ctypes.byref(err_ptr),
        )
        self._bridge.raise_on_error(err_ptr, "native OCR bridge failed")
        raw_payload = self._bridge.take_string(result_ptr)
        payload = json.loads(raw_payload) if raw_payload else {}
        return OCRResult(**payload)
