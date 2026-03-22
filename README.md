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
    - `FIRESTORE_COLLECTION_NAME`: Firestore collection name (default `receipts`).
    - `OPENAI_API_KEY`: key with Responses access.

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

If any metadata field is omitted the AI extraction step tries to populate it from the receipt image before saving it; you can still override the inferred values by including the form parameter yourself. The backend also validates that the AI subtotal matches the sum of the inferred items and stores that validation state under `extracted_fields.validation`.
The category inference step is constrained to the list stored via `POST /categories`. If the AI cannot confidently match the receipt to one of those names it returns `null` and the record stays uncategorized.

Response payload includes the created Firestore ID and stored fields.

### `GET /receipts/{receipt_id}`

Returns the stored Firestore document for the requested receipt ID.

### `DELETE /receipts/{receipt_id}`

Removes a receipt and its associated image.

- Deletes the Firestore document and, if a `storage_path` was stored, removes the blob from the configured GCS bucket.
- Returns `204 No Content` on success or `404` if the receipt or image can’t be found.

### `PUT /receipts/{receipt_id}`

Allows editing every stored field (vendor, subtotal, tax, total, category, purchase_date, items). Send only the values you want to change; any field not included in the payload is left untouched.

- The `items` list must contain objects with `name`, `quantity`, and `price` and will replace the stored array.
- The backend revalidates the subtotal against the provided items (or infers it if you omit it) and stores the validation metadata inside `extracted_fields.validation`.
- The response returns the updated receipt document using the original ID so the frontend can refresh its view.

### `GET /receipts`

Streams receipts sorted by the most recent upload first.

- Query parameters:
  - `limit` (default `10`, max `100`): how many receipts to return.
  - `start_after_id`: use the `next_cursor` value from a previous response to fetch the next page.
- Response: `{ receipts: [...], next_cursor: "<last-doc-id or null>" }`.
- Push `start_after_id=<next_cursor>` to load the next batch of 10 receipts until `next_cursor` is `null`.
- `storage_path` is not included in the API response; use the image proxy endpoint below instead.

### `Category Management`

You can create, list, update, and delete categories via the backend. The API uses a separate `categories` Firestore collection and keeps their metadata entirely within Firestore (no image uploads).

- `POST /categories` – supply `{ "name": "...", "description": "..." }` to create a category; returns the new ID plus stored fields.
- `GET /categories` – returns all categories sorted by name.
- `PUT /categories/{category_id}` – update name/description; returns the updated record.
- `DELETE /categories/{category_id}` – removes the category. Returns `204 No Content`.

### `GET /receipts/{receipt_id}/image`

Downloads the receipt image through the backend so storage paths stay hidden:

- Returns a binary stream with the same MIME type that was uploaded.
- Raises `404` if the receipt or blob no longer exists.

### `GET /healthz`

Simple readiness check.

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
      --set-env-vars GCLOUD_BUCKET_NAME=<bucket>,FIRESTORE_COLLECTION_NAME=receipts,CATEGORIES_COLLECTION_NAME=categories,OPENAI_API_KEY=<key>,OPENAI_MODEL_NAME=gpt-4.1-mini,ALLOWED_ORIGINS=http://localhost:3000 \
      --service-account=<service-account>@<PROJECT>.iam.gserviceaccount.com
    ```

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
