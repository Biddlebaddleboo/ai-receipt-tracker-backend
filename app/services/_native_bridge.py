from __future__ import annotations

import ctypes
import os
from pathlib import Path
from typing import Callable, Optional


def default_library_candidates(library_stem: str) -> list[Path]:
    extension = ".so"
    if os.name == "nt":
        extension = ".dll"
    elif os.name == "darwin":
        extension = ".dylib"
    project_root = Path(__file__).resolve().parents[2]
    filename = f"{library_stem}{extension}"
    return [
        project_root / "native" / filename,
        project_root / "native" / library_stem.removeprefix("lib") / filename,
    ]


def resolve_library_path(env_var: str, library_stem: str) -> Optional[Path]:
    configured = os.getenv(env_var)
    if configured:
        path = Path(configured).expanduser()
        if path.exists():
            return path
    for candidate in default_library_candidates(library_stem):
        if candidate.exists():
            return candidate
    return None


class NativeLibraryBase:
    _instance: Optional["NativeLibraryBase"] = None
    env_var: str = ""
    library_stem: str = ""
    missing_label: str = "native library"
    free_symbol: str = ""

    def __init__(self, path: Path):
        self._library = ctypes.CDLL(str(path))
        self._configure()

    def _configure(self) -> None:  # pragma: no cover - implemented by subclasses
        raise NotImplementedError

    @classmethod
    def load(cls):
        if cls._instance is not None:
            return cls._instance
        path = resolve_library_path(cls.env_var, cls.library_stem)
        if path is None:
            raise RuntimeError(
                f"{cls.missing_label} not found. Set {cls.env_var} or ship the shared library."
            )
        cls._instance = cls(path)
        return cls._instance

    def free(self, ptr: int | None) -> None:
        if ptr:
            getattr(self._library, self.free_symbol)(ctypes.c_void_p(ptr))

    def take_string(self, ptr: int | None) -> str:
        if not ptr:
            return ""
        try:
            return ctypes.string_at(ptr).decode("utf-8")
        finally:
            self.free(ptr)

    def raise_on_error(
        self,
        err_ptr: ctypes.c_void_p,
        default_message: str,
        translate_error: Optional[Callable[[str], Exception | None]] = None,
    ) -> None:
        if not err_ptr.value:
            return
        message = self.take_string(err_ptr.value)
        if translate_error is not None:
            translated = translate_error(message)
            if translated is not None:
                raise translated
        raise RuntimeError(message or default_message)
