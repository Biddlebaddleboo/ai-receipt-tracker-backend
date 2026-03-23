from __future__ import annotations

import json
from typing import Any, Dict, Optional
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request

from fastapi import HTTPException, status


class HelcimRecurringClient:
    def __init__(
        self,
        api_token: Optional[str],
        base_url: str,
        timeout_seconds: int = 20,
        user_agent: str = "ai-receipt-tracker-backend/1.0",
    ):
        self._api_token = (api_token or "").strip()
        self._base_url = base_url.rstrip("/")
        self._timeout_seconds = timeout_seconds
        self._user_agent = (user_agent or "").strip() or "ai-receipt-tracker-backend/1.0"

    def list_payment_plans(self, query: Dict[str, Any]) -> Any:
        return self._request("GET", "payment-plans", query=query)

    def create_payment_plans(self, payload: Any) -> Any:
        return self._request("POST", "payment-plans", payload=payload)

    def patch_payment_plans(self, payload: Any) -> Any:
        return self._request("PATCH", "payment-plans", payload=payload)

    def get_payment_plan(self, payment_plan_id: int) -> Any:
        return self._request("GET", f"payment-plans/{payment_plan_id}")

    def delete_payment_plan(self, payment_plan_id: int) -> Any:
        return self._request("DELETE", f"payment-plans/{payment_plan_id}")

    def list_subscriptions(self, query: Dict[str, Any]) -> Any:
        return self._request("GET", "subscriptions", query=query)

    def create_subscriptions(self, payload: Any) -> Any:
        return self._request("POST", "subscriptions", payload=payload)

    def patch_subscriptions(self, payload: Any) -> Any:
        return self._request("PATCH", "subscriptions", payload=payload)

    def get_subscription(self, subscription_id: int) -> Any:
        return self._request("GET", f"subscriptions/{subscription_id}")

    def delete_subscription(self, subscription_id: int) -> Any:
        return self._request("DELETE", f"subscriptions/{subscription_id}")

    def get_card_transaction(self, transaction_id: int) -> Any:
        return self._request("GET", f"card-transactions/{transaction_id}")

    def _request(
        self,
        method: str,
        path: str,
        payload: Optional[Any] = None,
        query: Optional[Dict[str, Any]] = None,
    ) -> Any:
        if not self._api_token:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail="HELCIM_API_TOKEN is not configured",
            )

        url = f"{self._base_url}/{path.lstrip('/')}"
        normalized_query = self._normalize_query(query)
        if normalized_query:
            url = f"{url}?{urllib_parse.urlencode(normalized_query, doseq=True)}"

        headers = {
            "Accept": "application/json",
            "api-token": self._api_token,
            "User-Agent": self._user_agent,
        }
        data: Optional[bytes] = None
        if payload is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(payload).encode("utf-8")

        req = urllib_request.Request(url=url, data=data, headers=headers, method=method)

        try:
            with urllib_request.urlopen(req, timeout=self._timeout_seconds) as resp:
                raw = resp.read().decode("utf-8").strip()
                if not raw:
                    return {}
                try:
                    return json.loads(raw)
                except json.JSONDecodeError:
                    return {"raw": raw}
        except urllib_error.HTTPError as exc:
            raw = exc.read().decode("utf-8", errors="replace").strip()
            detail: Any = f"Helcim API request failed ({exc.code})"
            if raw:
                try:
                    detail = json.loads(raw)
                except json.JSONDecodeError:
                    detail = raw
            raise HTTPException(status_code=exc.code, detail=detail) from exc
        except urllib_error.URLError as exc:
            reason = getattr(exc, "reason", "unknown network error")
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail=f"Failed to reach Helcim API: {reason}",
            ) from exc

    @staticmethod
    def _normalize_query(query: Optional[Dict[str, Any]]) -> Dict[str, Any]:
        if not query:
            return {}
        normalized: Dict[str, Any] = {}
        for key, value in query.items():
            if value is None:
                continue
            if isinstance(value, str):
                trimmed = value.strip()
                if not trimmed:
                    continue
                normalized[key] = trimmed
            else:
                normalized[key] = value
        return normalized
