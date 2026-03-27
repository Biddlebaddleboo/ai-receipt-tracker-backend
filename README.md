# Receipt Scanner Backend (Go)

Lean Go backend for receipt upload finalization, OCR extraction, image URL signing, and Helcim billing actions.

## Related Links

- Frontend repository: [https://github.com/Biddlebaddleboo/receipt-keeper](https://github.com/Biddlebaddleboo/receipt-keeper)
- AI Receipt Tracker website: [https://ai-receipt-tracker.web.app/](https://ai-receipt-tracker.web.app/)

## What This Service Does

- Verifies Google OAuth bearer tokens for protected routes.
- Issues signed Google Cloud Storage upload URLs.
- Finalizes uploaded receipts and stores metadata in Firestore.
- Runs OCR using OpenAI (image is sent as a short-lived signed URL, not file bytes through backend).
- Signs receipt image URLs on demand.
- Handles Helcim subscription-related server flows.

## Current Architecture

- Runtime: Go (`cmd/apiserver`)
- Database: Firestore
- Object storage: Google Cloud Storage
- OCR: OpenAI Responses API
- Billing: Helcim API
- Container: distroless static image (`Dockerfile`)

## API Endpoints

### Public

- `GET /healthz`
- `GET /billing/helcim/approval`
- `POST /billing/helcim/approval`

### Authenticated (Google ID token bearer)

#### Receipts

- `POST /receipts/signed-upload`
  - Request: `{ "filename": "...", "content_type": "image/..." }`
  - Response includes signed upload URL and `storage_path`.

- `POST /receipts/finalize-upload`
  - Request: `{ "storage_path": "...", "vendor": ..., "subtotal": ..., "tax": ..., "total": ..., "category": ..., "purchase_date": ... }`
  - Creates Firestore receipt doc, runs OCR, persists extracted fields.

- `POST /receipts/sign-image`
  - Request: `{ "receipt_id": "..." }`
  - Returns a fresh signed image URL.

- `DELETE /receipts/{receipt_id}`
  - Deletes Firestore receipt doc and underlying storage object.

#### Billing

- `POST /billing/notify`
- `POST /billing/helcim/customer-code`
- `GET /billing/helcim/subscriptions`
- `POST /billing/helcim/subscriptions`
- `PATCH /billing/helcim/subscriptions`
- `GET /billing/helcim/subscriptions/{subscription_id}`
- `DELETE /billing/helcim/subscriptions/{subscription_id}`
- `POST /billing/helcim/subscriptions/{subscription_id}/sync`
- `POST /billing/subscriptions/activate`

## Required Environment Variables

Core:
- `PORT` (default `8080`)
- `GCLOUD_BUCKET_NAME`
- `FIRESTORE_DATABASE_ID` (default `(default)`)
- `FIRESTORE_COLLECTION_NAME` (default `receipts`)
- `CATEGORIES_COLLECTION_NAME` (default `categories`)
- `PLANS_COLLECTION_NAME` (default `plans`)
- `USERS_COLLECTION_NAME` (default `users`)

Auth/CORS:
- `REQUIRE_OAUTH` (`true`/`false`)
- `OAUTH_CLIENT_ID` (single string or JSON array/comma-list)
- `OAUTH_ALLOWED_DOMAINS` (optional)
- `ALLOWED_ORIGINS` (JSON array or comma-list)
- `ALLOWED_ORIGIN_REGEX` (optional)

OpenAI:
- `OPENAI_API_KEY`
- `OPENAI_MODEL_NAME` (default `gpt-4.1-mini`)

Helcim:
- `HELCIM_API_TOKEN`
- `HELCIM_API_BASE_URL` (default `https://api.helcim.com/v2`)
- `HELCIM_TIMEOUT_SECONDS` (default `20`)
- `HELCIM_USER_AGENT` (default `ai-receipt-tracker-backend/1.0`)
- `HELCIM_APPROVAL_SECRET` (optional)

## Local Run

From repo root:

```powershell
cd cmd\apiserver
$env:GOCACHE="C:\Users\John\Desktop\Receipt Scanner\.gocache"
$env:GOMODCACHE="C:\Users\John\Desktop\Receipt Scanner\.gopath\pkg\mod"
go run .
```

## Build/Test

```powershell
cd cmd\apiserver
$env:GOCACHE="C:\Users\John\Desktop\Receipt Scanner\.gocache"
$env:GOMODCACHE="C:\Users\John\Desktop\Receipt Scanner\.gopath\pkg\mod"
go test ./...
```

## Notes

- Receipt metadata reads are expected to come directly from Firestore client-side.
- The backend does not persist long-lived signed image URLs in Firestore; URLs are generated on demand.
- `storage_path` is the single source-of-truth storage field for receipts.
