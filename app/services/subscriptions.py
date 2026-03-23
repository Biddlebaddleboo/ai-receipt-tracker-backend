from __future__ import annotations

from datetime import datetime, timedelta
import re
from typing import Any, Dict, Optional

from fastapi import HTTPException, status
from google.cloud import firestore

OWNER_FIELD = "owner_email"
DEFAULT_PLAN_ID = "free"


def _period_bounds(now: datetime) -> tuple[datetime, datetime]:
    start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    if start.month == 12:
        end = start.replace(year=start.year + 1, month=1)
    else:
        end = start.replace(month=start.month + 1)
    return start, end


class SubscriptionService:
    def __init__(self, plans_collection: str, users_collection: str, database_id: str = "(default)"):
        self._client = firestore.Client(database=database_id)
        self._plans = self._client.collection(plans_collection)
        self._users = self._client.collection(users_collection)

    def ensure_within_limit(self, owner_email: str, receipt_client: Any) -> None:
        now = datetime.utcnow()
        user_doc = self._get_or_create_user(owner_email, now)
        plan = self._get_plan(user_doc.get("plan_id", "free"))
        limit = self._plan_limit(plan)
        if limit is None:
            return
        interval = self._plan_interval(plan)

        if interval == "once":
            count = receipt_client.count_receipts_by_owner(owner_email, None, None)
            if count >= limit:
                raise HTTPException(
                    status_code=status.HTTP_402_PAYMENT_REQUIRED,
                    detail=f"Plan {plan.get('name', plan.get('plan_id'))} hard limit reached ({limit} total receipts)",
                )
            return

        if interval == "month":
            start = user_doc.get("current_period_start")
            end = user_doc.get("current_period_end")
            if start is None or end is None or now >= end:
                start, end = _period_bounds(now)
                self._users.document(owner_email).update(
                    {
                        "current_period_start": start,
                        "current_period_end": end,
                        "receipt_count_updated_at": now,
                    }
                )
            count = receipt_client.count_receipts_by_owner(owner_email, start, end)
            if count >= limit:
                raise HTTPException(
                    status_code=status.HTTP_402_PAYMENT_REQUIRED,
                    detail=f"Plan {plan.get('name', plan.get('plan_id'))} limit reached ({limit} receipts per month)",
                )
            return

        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail=f"Plan {plan.get('plan_id')} has unsupported interval '{plan.get('interval')}'",
        )

    def apply_subscription_payload(
        self,
        owner_email: str,
        payload: Dict[str, Any],
    ) -> str:
        now = datetime.utcnow()
        plan_id = self._resolve_plan_from_payment(payload.get("paymentPlanId"))
        plan = self._get_plan(plan_id)
        interval = self._plan_interval(plan)
        start = self._parse_date(payload.get("dateActivated")) or now
        billing_date = self._parse_date(payload.get("dateBilling"))
        end = None
        if interval == "month":
            end = billing_date or (start + timedelta(days=30))
        update_data: Dict[str, Any] = {
            OWNER_FIELD: owner_email,
            "plan_id": plan["plan_id"],
            "subscription_status": payload.get("status", "active"),
            "plan_interval": interval,
            "plan_price_cents": self._coerce_int(plan.get("price_cents")),
            "current_period_start": start,
            "current_period_end": end,
            "plan_updated_at": now,
            "last_payment_id": self._extract_last_payment_id(payload.get("payments") or []),
        }
        self._users.document(owner_email).set(update_data, merge=True)
        return plan["plan_id"]

    def find_plan_by_payment_plan_id(self, payment_plan_id: Optional[Any]) -> Optional[Dict[str, Any]]:
        coerced_payment_plan_id = self._coerce_int(payment_plan_id)
        if coerced_payment_plan_id is None:
            return None
        return self._find_plan_by_payment_plan_id(coerced_payment_plan_id)

    def find_owner_email_by_customer_code(self, customer_code: Optional[Any]) -> Optional[str]:
        if customer_code is None:
            return None
        normalized = str(customer_code).strip()
        if not normalized:
            return None
        query = self._users.where("helcim_customer_code", "==", normalized).limit(1)
        snapshot = next(query.stream(), None)
        if not snapshot or not snapshot.exists:
            return None
        data = snapshot.to_dict() or {}
        owner_email = data.get(OWNER_FIELD)
        if isinstance(owner_email, str) and owner_email.strip():
            return owner_email.strip()
        return None

    def set_owner_customer_code(self, owner_email: str, customer_code: str) -> None:
        normalized = customer_code.strip()
        if not normalized:
            raise HTTPException(status_code=400, detail="customerCode is required")
        self._users.document(owner_email).set(
            {
                OWNER_FIELD: owner_email,
                "helcim_customer_code": normalized,
                "customer_code_updated_at": datetime.utcnow(),
            },
            merge=True,
        )

    def activate_plan_from_transaction(
        self,
        owner_email: str,
        plan: Dict[str, Any],
        transaction_id: Optional[int],
        paid_at: Optional[datetime],
    ) -> Dict[str, Any]:
        now = datetime.utcnow()
        interval = self._plan_interval(plan)
        start = paid_at or now
        end = None
        if interval == "month":
            end = start + timedelta(days=30)
        update_data: Dict[str, Any] = {
            OWNER_FIELD: owner_email,
            "plan_id": plan["plan_id"],
            "subscription_status": "active",
            "plan_interval": interval,
            "plan_price_cents": self._coerce_int(plan.get("price_cents")),
            "current_period_start": start,
            "current_period_end": end,
            "plan_updated_at": now,
        }
        if transaction_id is not None:
            update_data["last_transaction_id"] = transaction_id
        self._users.document(owner_email).set(update_data, merge=True)
        return {"owner_email": owner_email, "plan_id": plan["plan_id"]}

    def resolve_plan_for_approved_transaction(
        self,
        callback_payload: Dict[str, Any],
        transaction_payload: Dict[str, Any],
    ) -> Dict[str, Any]:
        payment_plan_id = (
            callback_payload.get("paymentPlanId")
            or transaction_payload.get("paymentPlanId")
            or transaction_payload.get("paymentPlanID")
        )
        plan = self.find_plan_by_payment_plan_id(payment_plan_id)
        if plan:
            return plan

        invoice_number = (
            callback_payload.get("invoiceNumber")
            or transaction_payload.get("invoiceNumber")
            or ""
        )
        plan_from_invoice = self._find_plan_by_invoice_number(str(invoice_number))
        if plan_from_invoice:
            return plan_from_invoice

        amount_cents = self._transaction_amount_cents(transaction_payload)
        if amount_cents is None:
            raise HTTPException(
                status_code=400,
                detail="Unable to determine purchased plan: no paymentPlanId, invoiceNumber, or amount",
            )
        currency = transaction_payload.get("currency")
        matched = self._find_plans_by_price_cents(amount_cents, currency)
        if not matched:
            raise HTTPException(
                status_code=400,
                detail=f"Unable to map transaction amount {amount_cents} cents to a plan",
            )
        if len(matched) > 1:
            raise HTTPException(
                status_code=400,
                detail=f"Ambiguous plan mapping for amount {amount_cents} cents; multiple plans match",
            )
        return matched[0]

    def _resolve_plan_from_payment(self, payment_plan_id: Optional[Any]) -> str:
        coerced_payment_plan_id = self._coerce_int(payment_plan_id)
        if coerced_payment_plan_id is None:
            return DEFAULT_PLAN_ID
        plan = self._find_plan_by_payment_plan_id(coerced_payment_plan_id)
        if plan:
            return plan["plan_id"]
        return DEFAULT_PLAN_ID

    @staticmethod
    def _coerce_int(value: Optional[Any]) -> Optional[int]:
        if value is None:
            return None
        try:
            return int(value)
        except (TypeError, ValueError):
            return None

    @staticmethod
    def _parse_date(value: Optional[str]) -> Optional[datetime]:
        if not value or value.startswith("0000"):
            return None
        for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d"):
            try:
                return datetime.strptime(value, fmt)
            except ValueError:
                continue
        return None

    @staticmethod
    def _extract_last_payment_id(payments: list) -> Optional[int]:
        if not isinstance(payments, list) or not payments:
            return None
        last = payments[-1]
        try:
            return int(last.get("id"))
        except (TypeError, ValueError):
            return None

    def _find_plan_by_payment_plan_id(self, payment_plan_id: int) -> Optional[Dict[str, Any]]:
        query = self._plans.where("payment_plan_id", "==", payment_plan_id).limit(1)
        snapshot = next(query.stream(), None)
        if snapshot and snapshot.exists:
            data = snapshot.to_dict() or {}
            data["plan_id"] = snapshot.id
            return data
        return None

    def _all_plans(self) -> list[Dict[str, Any]]:
        plans: list[Dict[str, Any]] = []
        for snapshot in self._plans.stream():
            if not snapshot.exists:
                continue
            data = snapshot.to_dict() or {}
            data["plan_id"] = snapshot.id
            plans.append(data)
        return plans

    def _find_plans_by_price_cents(
        self, price_cents: int, currency: Optional[Any]
    ) -> list[Dict[str, Any]]:
        normalized_currency = None
        if currency is not None:
            text = str(currency).strip().upper()
            if text:
                normalized_currency = text
        matches: list[Dict[str, Any]] = []
        for plan in self._all_plans():
            plan_price = self._coerce_int(plan.get("price_cents"))
            if plan_price is None or plan_price != price_cents:
                continue
            plan_currency = plan.get("currency")
            if normalized_currency and plan_currency:
                if str(plan_currency).strip().upper() != normalized_currency:
                    continue
            matches.append(plan)
        return matches

    def _find_plan_by_invoice_number(self, invoice_number: str) -> Optional[Dict[str, Any]]:
        text = invoice_number.strip()
        if not text:
            return None
        # Supported markers:
        # PLAN:<plan_id>
        # plan_id=<plan_id>
        # plan-<plan_id>
        candidates: list[str] = []
        if text.upper().startswith("PLAN:"):
            candidates.append(text.split(":", 1)[1].strip())
        match = re.search(r"plan[_-]?id=([a-zA-Z0-9_-]+)", text, flags=re.IGNORECASE)
        if match:
            candidates.append(match.group(1).strip())
        match = re.search(r"plan-([a-zA-Z0-9_-]+)", text, flags=re.IGNORECASE)
        if match:
            candidates.append(match.group(1).strip())

        if not candidates:
            return None
        candidate_set = {c for c in candidates if c}
        if not candidate_set:
            return None
        for plan in self._all_plans():
            plan_id = str(plan.get("plan_id", "")).strip()
            if plan_id in candidate_set:
                return plan
        return None

    def _get_or_create_user(self, owner_email: str, now: datetime) -> Dict[str, Any]:
        doc_ref = self._users.document(owner_email)
        snapshot = doc_ref.get()
        if snapshot.exists:
            return snapshot.to_dict() or {}
        start, end = _period_bounds(now)
        data: Dict[str, Any] = {
            OWNER_FIELD: owner_email,
            "plan_id": "free",
            "subscription_status": "active",
            "current_period_start": start,
            "current_period_end": end,
            "plan_updated_at": now,
        }
        doc_ref.set(data)
        return data

    def _get_plan(self, plan_id: str) -> Dict[str, Any]:
        plan = self._find_plan_by_name(plan_id)
        if plan:
            return plan
        plan = self._find_plan_by_document_id(plan_id)
        if plan:
            return plan
        plan = self._find_plan_by_plan_id_field(plan_id)
        if plan:
            return plan
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail=f"Subscription plan {plan_id} is not defined",
        )

    def _find_plan_by_document_id(self, plan_id: str) -> Optional[Dict[str, Any]]:
        doc_ref = self._plans.document(plan_id)
        snapshot = doc_ref.get()
        if not snapshot.exists:
            return None
        data = snapshot.to_dict() or {}
        data["plan_id"] = plan_id
        return data

    def _find_plan_by_plan_id_field(self, plan_id: str) -> Optional[Dict[str, Any]]:
        query = self._plans.where("plan_id", "==", plan_id).limit(1)
        snapshot = next(query.stream(), None)
        if not snapshot or not snapshot.exists:
            return None
        data = snapshot.to_dict() or {}
        data["plan_id"] = snapshot.id
        return data

    def _find_plan_by_name(self, name: str) -> Optional[Dict[str, Any]]:
        normalized = str(name).strip().lower()
        if not normalized:
            return None
        query = self._plans.stream()
        for snapshot in query:
            if not snapshot.exists:
                continue
            data = snapshot.to_dict() or {}
            plan_name = str(data.get("name", "")).strip().lower()
            if plan_name == normalized:
                data["plan_id"] = snapshot.id
                return data
        return None

    def user_plan_summary(self, owner_email: str) -> Dict[str, Any]:
        snapshot = self._users.document(owner_email).get()
        user_doc = snapshot.to_dict() if snapshot.exists else {}
        plan_id = str(user_doc.get("plan_id", "free") or "free")
        try:
            plan = self._get_plan(plan_id)
        except HTTPException:
            plan = self._default_plan_stub(plan_id)

        plan_interval = user_doc.get("plan_interval") or self._plan_interval(plan)
        plan_price_cents = self._coerce_int(user_doc.get("plan_price_cents"))
        if plan_price_cents is None:
            plan_price_cents = self._coerce_int(plan.get("price_cents"))

        updated_at = user_doc.get("plan_updated_at")
        if isinstance(updated_at, datetime):
            updated_at = updated_at.isoformat()

        return {
            "owner_email": owner_email,
            "plan_id": plan["plan_id"],
            "plan_name": plan.get("name"),
            "description": plan.get("description"),
            "subscription_status": user_doc.get("subscription_status", "active"),
            "plan_interval": plan_interval,
            "monthly_limit": self._plan_limit(plan),
            "plan_price_cents": plan_price_cents,
            "features": self.plan_features(plan),
            "plan_updated_at": updated_at,
            "last_transaction_id": user_doc.get("last_transaction_id"),
            "customer_code": user_doc.get("helcim_customer_code"),
        }

    @staticmethod
    def _default_plan_stub(plan_id: str) -> Dict[str, Any]:
        name = plan_id.capitalize()
        return {
            "plan_id": plan_id,
            "name": name,
            "description": "",
            "features": [],
        }

    @staticmethod
    def _plan_interval(plan: Dict[str, Any]) -> str:
        raw = str(plan.get("interval", "month")).strip().lower()
        if raw in ("month", "monthly"):
            return "month"
        if raw in ("once", "one_time", "onetime"):
            return "once"
        return raw

    @staticmethod
    def _plan_limit(plan: Dict[str, Any]) -> Optional[int]:
        raw_limit = plan.get("monthly_limit")
        if raw_limit is None:
            raw_limit = plan.get("receipt_limit")
        if raw_limit is None:
            return None
        try:
            return int(raw_limit)
        except (TypeError, ValueError):
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail=f"Plan {plan.get('plan_id')} has invalid monthly_limit",
            )

    @staticmethod
    def _transaction_amount_cents(transaction_payload: Dict[str, Any]) -> Optional[int]:
        raw_amount = transaction_payload.get("amount")
        if raw_amount is None:
            return None
        try:
            return int(round(float(raw_amount) * 100))
        except (TypeError, ValueError):
            return None

    @staticmethod
    def plan_features(plan: Optional[Dict[str, Any]]) -> list[str]:
        if not plan:
            return []
        raw_features = plan.get("features")
        if not isinstance(raw_features, list):
            return []
        features: list[str] = []
        for entry in raw_features:
            text = str(entry).strip()
            if text:
                features.append(text)
        return features
