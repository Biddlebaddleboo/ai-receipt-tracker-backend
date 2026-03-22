from __future__ import annotations

import base64
import json
import re
from typing import Any, Dict, List, Optional, Sequence

from openai import OpenAI
from pydantic import BaseModel, Field


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


class OpenAITextExtractor:
    _FIELD_ALIASES = {
        "vendor": ["vendor", "store", "merchant", "merchant_name", "store_name"],
        "subtotal": ["subtotal", "sub_total", "pre_tax"],
        "tax": ["tax", "sales_tax", "tax_amount"],
        "total": ["total", "grand_total", "amount_due", "total_amount"],
        "category": ["category", "type"],
        "purchase_date": ["purchase_date", "transaction_date", "date"],
    }
    _JSON_CANDIDATE = re.compile(r"\{[^{}]*\}")
    _DEFAULT_PROMPT = (
        "Extract the readable text from this receipt image and summarize the line items and totals. "
        "After the summary, output a JSON object with the following keys: `vendor`, `subtotal`, `tax`, `total`, "
        "`category`, `purchase_date`, and `items`. The `items` array should include objects with `name`, `quantity`, "
        "and `price`. Ensure the `subtotal` equals the sum of each item's `quantity` multiplied by its `price`; if you "
        "can't confirm a value, set it to null. Do not add any explanation outside the JSON object."
    )

    def __init__(self, model: str, api_key: Optional[str] = None):
        self._client = OpenAI(api_key=api_key) if api_key else OpenAI()
        self._model = model

    def extract(
        self,
        image_bytes: bytes,
        instructions: Optional[str] = None,
        category_options: Optional[Sequence[str]] = None,
    ) -> OCRResult:
        prompt = instructions or self._DEFAULT_PROMPT
        if category_options:
            choice = ", ".join(category_options)
            prompt = (
                prompt
                + " Use these categories when guessing the receipt type: "
                + choice
                + ". If none match, respond with `null` for the `category` key."
            )
        base64_image = base64.b64encode(image_bytes).decode("ascii")
        response = self._client.responses.create(
            model=self._model,
            input=[
                {
                    "role": "user",
                    "content": [
                        {"type": "input_text", "text": prompt},
                        {"type": "input_image", "image_url": f"data:image/png;base64,{base64_image}"},
                    ],
                }
            ],
            temperature=0,
        )
        raw_text = self._collect_text(response.output)
        structured = self._read_structured_fields(raw_text, category_options=category_options)
        items_data = structured.pop("items", [])
        return OCRResult(
            text=raw_text,
            items=[OCRItem(**item) for item in items_data],
            **structured,
        )

    def _read_structured_fields(
        self, raw_text: str, category_options: Optional[Sequence[str]] = None
    ) -> Dict[str, Any]:
        if not raw_text:
            return {}
        payload = self._extract_json(raw_text)
        normalized = self._normalize_fields(payload)
        items = self._extract_items(payload)
        normalized["items"] = items
        if category_options:
            normalized["category"] = self._validate_category(
                normalized.get("category"), category_options
            )
        return normalized

    @staticmethod
    def _collect_text(resp_output: List[Any]) -> str:
        if not resp_output:
            return ""
        fragments: List[str] = []
        for item in resp_output:
            for entry in item.content:
                if entry.get("type") == "output_text" and entry.get("text"):
                    fragments.append(entry["text"].strip())
        return "\n".join(fragments).strip()

    def _extract_json(self, text: str) -> Dict[str, Any]:
        start = text.find("{")
        end = text.rfind("}")
        if 0 <= start < end:
            candidate = text[start : end + 1]
            parsed = self._try_parse_json(candidate)
            if parsed:
                return parsed
        for match in self._JSON_CANDIDATE.findall(text):
            parsed = self._try_parse_json(match)
            if parsed:
                return parsed
        return {}

    @staticmethod
    def _try_parse_json(text: str) -> Dict[str, Any]:
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return {}

    def _normalize_fields(self, payload: Dict[str, Any]) -> Dict[str, Any]:
        normalized: Dict[str, Any] = {}
        lower_payload = {key.lower(): value for key, value in payload.items()}
        for canonical, aliases in self._FIELD_ALIASES.items():
            normalized_value = None
            for alias in aliases:
                if alias in lower_payload:
                    normalized_value = lower_payload[alias]
                    break
            normalized[canonical] = normalized_value
        return {
            "vendor": self._normalize_string(normalized.get("vendor")),
            "subtotal": self._normalize_amount(normalized.get("subtotal")),
            "tax": self._normalize_amount(normalized.get("tax")),
            "total": self._normalize_amount(normalized.get("total")),
            "category": self._normalize_string(normalized.get("category")),
            "purchase_date": self._normalize_string(normalized.get("purchase_date")),
        }

    def _extract_items(self, payload: Dict[str, Any]) -> List[Dict[str, Any]]:
        candidates = payload.get("items") or payload.get("line_items") or payload.get("entries")
        if not isinstance(candidates, list):
            return []
        items: List[Dict[str, Any]] = []
        for entry in candidates:
            if not isinstance(entry, dict):
                continue
            name = self._normalize_string(
                entry.get("name") or entry.get("item") or entry.get("description")
            )
            quantity = self._normalize_quantity(
                entry.get("quantity") or entry.get("qty") or entry.get("count")
            )
            price = self._normalize_amount(
                entry.get("price") or entry.get("unit_price") or entry.get("amount")
            )
            if not name and quantity is None and price is None:
                continue
            items.append({"name": name or "", "quantity": quantity, "price": price})
        return items

    @staticmethod
    def _normalize_quantity(value: Any) -> Optional[float]:
        if value is None:
            return None
        try:
            return float(value)
        except (TypeError, ValueError):
            text = str(value).strip()
            if not text:
                return None
            try:
                return float(text)
            except ValueError:
                return None

    @staticmethod
    def _validate_category(value: Optional[str], categories: Sequence[str]) -> Optional[str]:
        if not value:
            return None
        lowered = value.strip().lower()
        mapping = {cat.lower(): cat for cat in categories}
        return mapping.get(lowered)

    @staticmethod
    def _normalize_string(value: Any) -> Optional[str]:
        if value is None:
            return None
        text = str(value).strip()
        return text or None

    @staticmethod
    def _normalize_amount(value: Any) -> Optional[float]:
        if value is None:
            return None
        if isinstance(value, (int, float)):
            return float(value)
        text = str(value).replace("$", "").replace(",", "").strip()
        if not text:
            return None
        try:
            return float(text)
        except ValueError:
            return None

