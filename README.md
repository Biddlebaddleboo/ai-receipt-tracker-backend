# Receipt Scanner Backend (Go)

Lean Go backend for receipt upload finalization, OCR extraction, image URL signing, read-only AI receipt access, and the remaining Helcim approval/activation flows.

## Related Links

- Frontend repository: [https://github.com/Biddlebaddleboo/receipt-keeper](https://github.com/Biddlebaddleboo/receipt-keeper)
- AI Receipt Tracker website: [https://ai-receipt-tracker.web.app/](https://ai-receipt-tracker.web.app/)

## What This Service Does

- Verifies Google OAuth bearer tokens for protected app routes.
- Generates and revokes one server-owned, read-only AI receipt access token per user.
- Stores only the AI token secret hash in the server-only `ai_access_tokens` Firestore collection.
- Issues signed Google Cloud Storage upload URLs.
- Finalizes uploaded receipts and stores metadata in Firestore.
- Runs OCR using OpenAI (image is sent as a short-lived signed URL, not file bytes through backend).
- Signs receipt image URLs on demand for the normal app flow.
- Serves AI receipt images through the backend as JPEG without changing the stored GCS WebP object.
- Handles the Helcim approval callback and subscription activation.

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

#### AI access management

- `GET /ai-access/token`
  - Returns whether an AI access token exists plus its non-secret prefix and creation time.
- `POST /ai-access/token`
  - Replaces any existing AI token and returns the new plaintext token once.
- `DELETE /ai-access/token`
  - Revokes the active AI token.

The frontend must use these backend endpoints for AI token management. It does not read or write `ai_access_tokens` directly and requires no additional Firestore client permissions.

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

- `POST /billing/subscriptions/activate`

### Read-only AI token bearer

These routes accept only an AI access token created through `/ai-access/token`. AI tokens are not accepted by existing receipt write, billing, or account-management routes.

- `GET /ai/receipts`
  - Returns receipt summary metadata for the token owner.
- `GET /ai/receipts/{receipt_id}`
  - Returns the full structured receipt record for the token owner.
- `GET /ai/receipts/{receipt_id}/image`
  - Reads the private stored WebP receipt from GCS and returns the image bytes through this backend as `image/jpeg`.
  - Does not expose a GCS URL to the AI client and does not persist a JPEG copy.

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
- `HELCIM_APPROVAL_SECRET` (optional, used to protect the approval callback)

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

- Normal receipt metadata reads are expected to come directly from Firestore client-side; AI access is the exception and stays backend-owned.
- The backend does not persist long-lived signed image URLs in Firestore; URLs are generated on demand for the normal app flow.
- `storage_path` is the single source-of-truth storage field for receipts.
- AI image access reads the stored object through `storage_path`, converts WebP to JPEG for the response only, and does not create duplicate image state.
- AI access tokens are read-only and cannot authenticate existing write or billing routes.
