from __future__ import annotations

import mimetypes
from typing import Optional, Tuple

from google.cloud import storage


class GCSStorageClient:
    def __init__(self, bucket_name: str):
        self._client = storage.Client()
        self._bucket = self._client.bucket(bucket_name)

    def upload(
        self, data: bytes, destination_path: str, content_type: Optional[str] = None
    ) -> str:
        blob = self._bucket.blob(destination_path)
        mime_type = content_type or self._guess_mime_type(destination_path)
        blob.upload_from_string(data, content_type=mime_type)
        # returning the HTTPS URL is safest; ensure the bucket policy allows publicObjects access.
        return blob.public_url

    def download(self, destination_path: str) -> Tuple[bytes, str]:
        blob = self._bucket.blob(destination_path)
        data = blob.download_as_bytes()
        content_type = blob.content_type or self._guess_mime_type(destination_path)
        return data, content_type

    def delete(self, destination_path: str) -> None:
        blob = self._bucket.blob(destination_path)
        if blob.exists():
            blob.delete()

    @staticmethod
    def _guess_mime_type(path: str) -> str:
        guessed, _ = mimetypes.guess_type(path)
        return guessed or "application/octet-stream"
