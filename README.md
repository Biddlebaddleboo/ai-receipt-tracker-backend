# Receipt Scanner Backend (Python)

This backend exposes a FastAPI app that ingests receipt images, stores them in a Google Cloud Storage bucket, extracts text using the OpenAI Responses API, and records the results in Firestore.

## Architecture

- **FastAPI + Uvicorn**: REST endpoints, CORS, and validation.
- **Google Cloud Storage**: stores submitted images under `receipts/` and provides a URL.
- **Cloud Firestore**: stores receipt metadata, extracted text, vendor totals, and references to the image.
- **OpenAI responses API**: performs OCR-style extraction by asking the model to summarize the receipt contents.

## Getting started

1. Create a Python virtual environment (recommended):

    ```powershell
    python -m venv .venv
    .\.venv\Scripts\Activate.ps1
    python -m pip install --upgrade pip
    python -m pip install -r requirements.txt
    ```

    > The sandbox may block outbound network requests (for example, `pip install` might fail with `WinError 10013`). Install dependencies on a network that allows HTTPS traffic if you see that error.

2. Copy `.env.example` to `.env` and populate the values:
    - `GOOGLE_APPLICATION_CREDENTIALS`: path to the JSON service account with Storage/Firestore permissions.
    - `GCLOUD_BUCKET_NAME`: bucket that already exists and allows uploads.
    - `FIRESTORE_DATABASE_ID`: Firestore database ID to connect to. Use `(default)` for the default database.
    - `FIRESTORE_COLLECTION_NAME`: Firestore collection name (default `receipts`).
    - `CATEGORIES_COLLECTION_NAME`: category collection name (default `categories`).
    - `OPENAI_API_KEY`: key with Responses access.
    - `OPENAI_MODEL_NAME`: model to ask (default `gpt-4.1-mini`).
    - `ALLOWED_ORIGINS`: comma-separated list or JSON array of origins that may call the API (e.g., `["https://ai-receipt-tracker.web.app"]`). Cloud Run may require quoting.
    - `REQUIRE_OAUTH`: `true` to force every endpoint (except `/healthz`) to require a Google OAuth bearer token.
    - `OAUTH_CLIENT_ID`: Google OAuth client ID (used as the token audience when OAuth is enabled).
    - `OAUTH_ALLOWED_DOMAINS`: optional comma-separated list or JSON array of allowed Google Workspace domains (e.g., `example.com`).
    - `PLANS_COLLECTION_NAME`: Firestore collection that defines each tier (default `plans`).
    - `USERS_COLLECTION_NAME`: Firestore collection storing each user's subscription document (default `users`).
    - `HELCIM_API_TOKEN`: Helcim private API token used for server-to-server recurring API calls.
    - `HELCIM_API_BASE_URL`: Helcim API base URL (default `https://api.helcim.com/v2`).
    - `HELCIM_TIMEOUT_SECONDS`: timeout for outgoing Helcim API requests (default `20`).
    - `HELCIM_USER_AGENT`: user-agent for outbound Helcim calls (default `ai-receipt-tracker-backend/1.0`).
    - `HELCIM_APPROVAL_SECRET`: optional shared secret for Helcim Approval Send POST callbacks (`/billing/helcim/approval`).
    - `HELCIM_APPROVAL_REDIRECT_URL`: optional frontend URL to redirect browser landings after hosted payment callbacks (for providers that reuse one callback URL).

3. Start the development server:

    ```powershell
    uvicorn app.main:app --reload
    ```

## API

### `POST /receipts`

Upload a receipt image and optional metadata. The endpoint:

- accepts multipart form data (`file`, `vendor`, `subtotal`, `tax`, `total`, `category`, `purchase_date`). The `items` list is inferred by the AI and stored automatically.
- saves the image to GCS.
- calls the OpenAI responses API to extract the visible text, line items (name + quantity + price), and inferred totals.
- persists data in Firestore along with the extraction.

If any metadata field is omitted the AI extraction step tries to populate it from the receipt image before saving it; you can still override the inferred values by including the form parameter yourself. The backend also validates that the AI subtotal matches the sum of the inferred items and stores that validation state under `extracted_fields.validation`. Each receipt is tied to the authenticated Google email (stored under `owner_email` in Firestore), so only that user can fetch or modify their receipts. Receipt uploads are limited by the callerâ€™s subscription plan (free = 50/month, plus = 200/month, pro = unlimited, as defined in the `plans` collection), so POST `/receipts` now enforces that quota before processing.
The `items` property is a structured list of objects with `name`, `quantity`, and `price` (both `quantity` and `price` are floats when the AI can extract them). This makes it easy for the frontend to render line items, and the backend will override or infer the `subtotal` so it matches the computed total of every `quantity * price`.
The category inference step is constrained to the list stored via `POST /categories`. If the AI cannot confidently match the receipt to one of those names it returns `null` and the record stays uncategorized. Every receipt document also records `extracted_fields`, including the `model` that answered, the trimmed `text_length`, `ai_suggestions` (vendor, totals, category, purchase date, items) and the `validation` details so you can show what values were forced or inferred.

Response payload includes the created Firestore ID and stored fields.

### `GET /receipts/{receipt_id}`

Returns the stored Firestore document for the requested receipt ID.

### `DELETE /receipts/{receipt_id}`

Removes a receipt and its associated image.

- Deletes the Firestore document and, if a `storage_path` was stored, removes the blob from the configured GCS bucket.
- Returns `204 No Content` on success or `404` if the receipt or image canâ€™t be found.

### `PUT /receipts/{receipt_id}`

Allows editing every stored field (vendor, subtotal, tax, total, category, purchase_date, items). Send only the values you want to change; any field not included in the payload is left untouched.

- The `items` list must contain objects with `name`, `quantity`, and `price` and will replace the stored array.
- The backend revalidates the subtotal against the provided items (or infers it if you omit it) and stores the validation metadata inside `extracted_fields.validation`.
- The response returns the updated receipt document using the original ID so the frontend can refresh its view.

### `GET /receipts`

Streams receipts sorted by the most recent upload first.

- Query parameters:
  - `limit` (default `10`, max `100`): how many receipts to return.
  - `start_after_id`: use the `next_cursor` value (it is the Firestore document ID of the last receipt in the previous batch) to fetch the next page; IDs are not sequential counters, so always pass the ID returned by the API instead of assuming they start at `1`.
- Response: `{ receipts: [...], next_cursor: "<last-doc-id or null>" }`.
- Push `start_after_id=<next_cursor>` to load the next batch of 10 receipts until `next_cursor` is `null`.
- `storage_path` is not included in the API response; use the image proxy endpoint below instead.

### Authentication


### `Category Management`

You can create, list, update, and delete categories via the backend. The API uses a separate `categories` Firestore collection and keeps their metadata entirely within Firestore (no image uploads). Categories are tied to the authenticated Google email, so each user manages their own list of names/descriptions independently of other users.

- `POST /categories` â€“ supply `{ "name": "...", "description": "..." }` to create a category; returns the new ID plus stored fields.
- `GET /categories` â€“ returns all categories sorted by name.
- `PUT /categories/{category_id}` â€“ update name/description; returns the updated record.
- `DELETE /categories/{category_id}` â€“ removes the category. Returns `204 No Content`.

### `GET /receipts/{receipt_id}/image`

Downloads the receipt image through the backend so storage paths stay hidden:

- Returns a binary stream with the same MIME type that was uploaded.
- Raises `404` if the receipt or blob no longer exists.

### `GET /healthz`

Simple readiness check.

### `Helcim Recurring Billing Proxy`

The backend now includes authenticated proxy routes for Helcim recurring APIs so your frontend never exposes `HELCIM_API_TOKEN`.

- `GET /billing/helcim/payment-plans` -> list payment plans (supports Helcim query params).
- `POST /billing/helcim/payment-plans` -> create one or more payment plans (send the Helcim request body as-is).
- `PATCH /billing/helcim/payment-plans` -> update payment plans (send Helcim request body as-is).
- `GET /billing/helcim/payment-plans/{payment_plan_id}` -> fetch a single payment plan.
- `DELETE /billing/helcim/payment-plans/{payment_plan_id}` -> delete a payment plan.
- `GET /billing/helcim/subscriptions` -> list subscriptions (supports Helcim query params).
- `POST /billing/helcim/subscriptions` -> create subscriptions (send Helcim request body as-is).
- `PATCH /billing/helcim/subscriptions` -> update subscriptions (send Helcim request body as-is).
- `GET /billing/helcim/subscriptions/{subscription_id}` -> fetch a single subscription.
- `DELETE /billing/helcim/subscriptions/{subscription_id}` -> delete a subscription.
- `POST /billing/helcim/subscriptions/{subscription_id}/sync` -> fetch a Helcim subscription and apply it to `users/{owner_email}` using existing `paymentPlanId` mapping logic.
- `POST /billing/helcim/customer-code` -> save the authenticated user's Helcim `customerCode` for callback mapping.
- `POST /billing/helcim/approval` -> approval callback endpoint for hosted payment pages (accepts JSON or form-encoded callback payloads). It validates approval and fetches transaction details by `transactionId`, but does not activate plans.

Helcim auth is handled server-side with the `api-token` request header. Frontend calls your backend with `Authorization: Bearer <google_id_token>`, and your backend calls Helcim. The approval callback endpoint is unauthenticated by OAuth because it is called by Helcim; protect it with `HELCIM_APPROVAL_SECRET` and include `?secret=<value>` in your hosted payment callback URL.
If your provider only lets you configure one callback URL (used for both server callback and browser redirect), you can still use `/billing/helcim/approval`; set `HELCIM_APPROVAL_REDIRECT_URL` so browser GET requests are redirected to your frontend success page after processing.

Recommended frontend flow:

1. User signs in with Google and obtains an ID token.
2. Frontend calls `POST /billing/helcim/customer-code` once to store the user's Helcim `customerCode`.
3. User pays via Helcim hosted payment page.
4. Helcim Approval Send POST calls `POST /billing/helcim/approval` with `transactionId` and approval fields.
5. Backend fetches the transaction from Helcim and returns normalized callback data; plan activation should happen only from explicit subscription endpoints (for example `POST /billing/helcim/subscriptions/{subscription_id}/sync` or `POST /billing/notify`).

### Deployment to Google Cloud Run

1. Build and push a container image (replace `<PROJECT>` with your GCP project ID):

    ```bash
    gcloud builds submit --tag gcr.io/<PROJECT>/receipt-scanner
    ```

2. Deploy to Cloud Run (choose your region and service account with Firestore/Storage access):

    ```bash
    gcloud run deploy receipt-scanner \
      --image gcr.io/<PROJECT>/receipt-scanner \
      --region us-central1 \
      --platform managed \
      --allow-unauthenticated \
      --set-env-vars GCLOUD_BUCKET_NAME=<bucket>,FIRESTORE_DATABASE_ID=(default),FIRESTORE_COLLECTION_NAME=receipts,CATEGORIES_COLLECTION_NAME=categories,OPENAI_API_KEY=<key>,OPENAI_MODEL_NAME=gpt-4.1-mini,ALLOWED_ORIGINS='[\"https://ai-receipt-tracker.web.app\"]',REQUIRE_OAUTH=true,OAUTH_CLIENT_ID=<client-id>,OAUTH_ALLOWED_DOMAINS=example.com \
      --service-account=<service-account>@<PROJECT>.iam.gserviceaccount.com
    ```

    > Use single quotes around `ALLOWED_ORIGINS` (and any JSON array/CSV) when invoking `gcloud` so the brackets aren't interpreted by the shell. Adjust `REQUIRE_OAUTH`, `OAUTH_CLIENT_ID`, and `OAUTH_ALLOWED_DOMAINS` only if you want Google OAuth enforced.

    > If you see `Error: The database (default) does not exist`, visit the Firestore setup page (https://console.cloud.google.com/datastore/setup) to initialize Firestore and set `FIRESTORE_DATABASE_ID` to the database ID you created. The default database is `(default)` so most deployments can keep that value after the database exists.

3. Secure the runtime:

    * Grant the Cloud Run service account `roles/storage.objectAdmin` and `roles/datastore.user`.
    * Store secrets (OpenAI key, other credentials) in Secret Manager and mount them or inject via `--set-secrets` if you prefer.
    * Ensure the bucket and Firestore collection already exist.

4. Update the service if you need to change env vars or image:

    ```bash
    gcloud run services update receipt-scanner \
      --set-env-vars OPENAI_MODEL_NAME=gpt-4.1-mini
    ```

## Notes

- Images are uploaded with the MIME type they arrive with; convert to PNG/JPEG before uploading if you need a different format.
- The OpenAI extractor prompts the model to describe the receipt, so you can adjust the prompt inside `app/services/ocr.py` if you need structured output.
- For production, run `uvicorn app.main:app --host 0.0.0.0 --port 8000` behind a secured reverse proxy or gateway.
- The list of categories also acts as the source of truth for AI classification when a receipt is uploaded; make sure each desired label is saved here before it can be inferred automatically.
- Firestore collection names and the Firestore database ID are separate settings. `FIRESTORE_DATABASE_ID` selects the database, while `FIRESTORE_COLLECTION_NAME` and `CATEGORIES_COLLECTION_NAME` select collections inside that database.
- Each receipt document stores an `items` array of `{ "name": "...", "quantity": 1.0, "price": 2.5 }` entries, an `owner_email` field that matches the Google email that created it, and an `extracted_fields` object that captures the `model`, `text_length`, `ai_suggestions`, and `validation` metadata so the frontend can surface what values were inferred or forced.
- When `REQUIRE_OAUTH=true` every request except `/healthz` must carry a Google OAuth ID token in `Authorization: Bearer <id_token>`; tokens are verified against `OAUTH_CLIENT_ID` and the optional `OAUTH_ALLOWED_DOMAINS` list.
- Define the `plans` and `users` collections before you enforce billing: `plans` lists each tier and per-period receipt limit, and `users/{owner_email}` tracks the active plan plus the current period window so the backend can decide whether to accept a new upload.
- Each `plans/{plan_id}` document should include the processorâ€™s `payment_plan_id` numeric value so `/billing/notify` can map the incoming `paymentPlanId` into the correct subscription tier before enforcing the daily/monthly caps.

