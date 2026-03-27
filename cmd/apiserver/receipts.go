package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"

type httpError struct {
	status int
	detail string
}

func (e httpError) Error() string {
	return e.detail
}

type receiptItem struct {
	Name     string   `json:"name"`
	Quantity *float64 `json:"quantity,omitempty"`
	Price    *float64 `json:"price,omitempty"`
}

type receiptRecord struct {
	ID                    string                 `json:"id"`
	Vendor                *string                `json:"vendor,omitempty"`
	Subtotal              *float64               `json:"subtotal,omitempty"`
	Tax                   *float64               `json:"tax,omitempty"`
	Total                 *float64               `json:"total,omitempty"`
	Category              *string                `json:"category,omitempty"`
	PurchaseDate          *string                `json:"purchase_date,omitempty"`
	Items                 []receiptItem          `json:"items"`
	ImageURL              string                 `json:"image_url"`
	ExtractedText         string                 `json:"extracted_text"`
	ExtractedFields       map[string]interface{} `json:"extracted_fields"`
	CreatedAt             time.Time              `json:"created_at"`
}

type signedUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

type finalizeUploadRequest struct {
	StoragePath  string   `json:"storage_path"`
	Vendor       *string  `json:"vendor"`
	Subtotal     *float64 `json:"subtotal"`
	Tax          *float64 `json:"tax"`
	Total        *float64 `json:"total"`
	Category     *string  `json:"category"`
	PurchaseDate *string  `json:"purchase_date"`
}

type openAIResponsesRequest struct {
	Model       string               `json:"model"`
	Input       []openAIInputMessage `json:"input"`
	Temperature float64              `json:"temperature"`
}

type openAIInputMessage struct {
	Role    string               `json:"role"`
	Content []openAIInputContent `json:"content"`
}

type openAIInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type openAIResponsesEnvelope struct {
	Error      *openAIResponseError  `json:"error"`
	OutputText string                `json:"output_text"`
	Output     []openAIResponseBlock `json:"output"`
}

type openAIResponseError struct {
	Message string `json:"message"`
}

type openAIResponseBlock struct {
	Content []openAIResponseContent `json:"content"`
}

type openAIResponseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ocrItem struct {
	Name     *string  `json:"name,omitempty"`
	Quantity *float64 `json:"quantity,omitempty"`
	Price    *float64 `json:"price,omitempty"`
}

type ocrResult struct {
	Text         string    `json:"text"`
	Vendor       *string   `json:"vendor,omitempty"`
	Subtotal     *float64  `json:"subtotal,omitempty"`
	Tax          *float64  `json:"tax,omitempty"`
	Total        *float64  `json:"total,omitempty"`
	Category     *string   `json:"category,omitempty"`
	PurchaseDate *string   `json:"purchase_date,omitempty"`
	Items        []ocrItem `json:"items"`
}

type subtotalValidation struct {
	Subtotal *float64
	Info     map[string]interface{}
}

func requestContext() context.Context {
	return context.Background()
}

func (s *apiServer) handleReceiptByID(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/receipts/")
	path = strings.TrimSpace(path)
	if path == "" {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	if path == "signed-upload" {
		if request.Method != http.MethodPost {
			writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		s.createSignedUpload(writer, request, user)
		return
	}
	if path == "finalize-upload" {
		if request.Method != http.MethodPost {
			writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		s.finalizeSignedUpload(writer, request, user)
		return
	}
	if path == "sign-image" {
		if request.Method != http.MethodPost {
			writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		s.signReceiptImage(writer, request, user)
		return
	}
	if strings.Contains(path, "/") {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	switch request.Method {
	case http.MethodDelete:
		s.deleteReceipt(writer, request, user, path)
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *apiServer) signReceiptImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload struct {
		ReceiptID string `json:"receipt_id"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	receiptID := strings.TrimSpace(payload.ReceiptID)
	if receiptID == "" {
		writeJSONError(writer, http.StatusBadRequest, "receipt_id is required")
		return
	}
	data, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := stringFromAny(data["storage_path"])
	if storagePath == "" {
		writeJSONError(writer, http.StatusNotFound, "Receipt image not found")
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), storagePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"receipt_id": receiptID,
		"image_url":  imageURL,
		"expires_at": time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

func (s *apiServer) createSignedUpload(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload signedUploadRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	contentType := strings.TrimSpace(payload.ContentType)
	if err := ensureImage(contentType); err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := buildStorageKeyForOwner(user.Email, payload.Filename)
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	postPolicy, err := s.bucket.GenerateSignedPostPolicyV4(storagePath, &gcs.PostPolicyV4Options{
		Expires: expiresAt,
		Fields: &gcs.PolicyV4Fields{
			ContentType: contentType,
		},
		Conditions: []gcs.PostPolicyV4Condition{
			gcs.ConditionContentLengthRange(1, 20*1024*1024),
		},
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	formFields := postPolicy.Fields
	if formFields == nil {
		formFields = map[string]string{}
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"storage_path": storagePath,
		"upload_url":   postPolicy.URL,
		"method":       http.MethodPost,
		"form_fields":  formFields,
		"fields":       formFields,
		"headers":      map[string]string{},
		"expires_at":   expiresAt.Format(time.RFC3339),
	})
}

func (s *apiServer) finalizeSignedUpload(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	deleteUploadedObject := func(ctx context.Context, path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if err := s.bucket.Object(path).Delete(ctx); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
			log.Printf("finalize cleanup delete failed storage_path=%s err=%v", path, err)
		}
	}
	var payload finalizeUploadRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	ownerEmail := strings.TrimSpace(user.Email)
	storagePath := strings.TrimSpace(payload.StoragePath)
	if storagePath == "" {
		writeJSONError(writer, http.StatusBadRequest, "storage_path is required")
		return
	}
	if !strings.HasPrefix(storagePath, ownerStoragePrefix(ownerEmail)) {
		writeJSONError(writer, http.StatusForbidden, "storage_path does not belong to the authenticated user")
		return
	}
	if _, err := s.ensureWithinLimit(ownerEmail); err != nil {
		deleteUploadedObject(request.Context(), storagePath)
		s.writeErr(writer, err)
		return
	}
	attrs, err := s.bucket.Object(storagePath).Attrs(request.Context())
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			writeJSONError(writer, http.StatusNotFound, "uploaded object not found")
			return
		}
		s.writeErr(writer, err)
		return
	}
	if attrs.Size <= 0 {
		deleteUploadedObject(request.Context(), storagePath)
		writeJSONError(writer, http.StatusBadRequest, "uploaded object is empty")
		return
	}
	if err := ensureImage(strings.TrimSpace(attrs.ContentType)); err != nil {
		deleteUploadedObject(request.Context(), storagePath)
		s.writeErr(writer, err)
		return
	}

	now := time.Now().UTC()
	docRef := s.receipts.NewDoc()
	receiptPayload := map[string]interface{}{
		"vendor":                            normalizeOptionalString(payload.Vendor),
		"subtotal":                          payload.Subtotal,
		"tax":                               payload.Tax,
		"total":                             payload.Total,
		"category":                          normalizeOptionalString(payload.Category),
		"purchase_date":                     normalizeOptionalString(payload.PurchaseDate),
		"storage_path":                      storagePath,
		"items":                             []map[string]interface{}{},
		"extracted_text":                    "",
		"extracted_fields":                  map[string]interface{}{},
		"created_at":                        now,
		"owner_email":                       ownerEmail,
	}
	if _, err := docRef.Set(request.Context(), receiptPayload); err != nil {
		s.writeErr(writer, err)
		return
	}
	job := receiptJob{
		ID:          docRef.ID,
		OwnerEmail:  ownerEmail,
		StoragePath: storagePath,
	}
	s.processReceiptJob(request.Context(), job)
	updated, err := s.receipts.Doc(docRef.ID).Get(request.Context())
	if err == nil && updated.Exists() {
		updatedData := updated.Data()
		s.attachSignedImageURL(request.Context(), updatedData)
		writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, updatedData))
		return
	}
	s.attachSignedImageURL(request.Context(), receiptPayload)
	writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, receiptPayload))
}

func (s *apiServer) deleteReceipt(writer http.ResponseWriter, request *http.Request, user *verifiedUser, receiptID string) {
	data, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if _, err := s.receipts.Doc(receiptID).Delete(request.Context()); err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := strings.TrimSpace(stringFromAny(data["storage_path"]))
	if storagePath != "" {
		if err := s.bucket.Object(storagePath).Delete(request.Context()); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		}
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) getOwnedReceipt(ctx context.Context, receiptID string, ownerEmail string) (map[string]interface{}, error) {
	snapshot, err := s.receipts.Doc(receiptID).Get(ctx)
	if err != nil || !snapshot.Exists() {
		return nil, httpError{status: http.StatusNotFound, detail: fmt.Sprintf("Receipt %s not found", receiptID)}
	}
	data := snapshot.Data()
	if err := s.ensureReceiptOwner(data, ownerEmail, receiptID); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *apiServer) ensureReceiptOwner(data map[string]interface{}, ownerEmail string, receiptID string) error {
	stored := strings.TrimSpace(stringFromAny(data["owner_email"]))
	if stored == "" || stored != strings.TrimSpace(ownerEmail) {
		return httpError{status: http.StatusNotFound, detail: fmt.Sprintf("Receipt %s not found", receiptID)}
	}
	return nil
}

func ensureImage(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return httpError{status: http.StatusBadRequest, detail: "Invalid content type"}
	}
	if !strings.EqualFold(mediaType, "image/webp") {
		return httpError{status: http.StatusBadRequest, detail: "Uploaded file must be image/webp"}
	}
	return nil
}

func buildStorageKey(filename string) string {
	base := filepath.Base(filename)
	if base == "." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		base = "receipt"
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Sprintf("receipts/%d_%s", time.Now().UnixNano(), base)
	}
	return "receipts/" + hex.EncodeToString(tokenBytes) + "_" + base
}

func buildStorageKeyForOwner(ownerEmail string, filename string) string {
	return ownerStoragePrefix(ownerEmail) + strings.TrimPrefix(buildStorageKey(filename), "receipts/")
}

func ownerStoragePrefix(ownerEmail string) string {
	normalized := strings.ToLower(strings.TrimSpace(ownerEmail))
	sum := sha256.Sum256([]byte(normalized))
	return "receipts/u_" + hex.EncodeToString(sum[:8]) + "/"
}

func normalizeOptionalString(value *string) interface{} {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

func (s *apiServer) categoryNames(ctx context.Context, ownerEmail string) ([]string, error) {
	iter := s.categories.Where("owner_email", "==", ownerEmail).Documents(ctx)
	defer iter.Stop()
	names := make([]string, 0)
	seen := map[string]struct{}{}
	for {
		snapshot, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(stringFromAny(snapshot.Data()["name"]))
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

type receiptJob struct {
	ID          string
	OwnerEmail  string
	StoragePath string
}

func (s *apiServer) processReceiptJob(ctx context.Context, job receiptJob) {
	categoryOptions, err := s.categoryNames(ctx, job.OwnerEmail)
	if err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}
	imageURL, err := s.signedImageURL(ctx, job.StoragePath)
	if err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}
	ocrRes, err := s.extractReceiptOCR(ctx, imageURL, categoryOptions)
	if err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}

	itemsPayload := ocrItemsToReceiptItems(ocrRes.Items)
	validation := validateSubtotalAgainstItems(ocrRes.Subtotal, itemsPayload)
	extractedFields := map[string]interface{}{
		"model":       s.cfg.openAIModel,
		"text_length": len(ocrRes.Text),
		"ai_suggestions": map[string]interface{}{
			"vendor":        ocrRes.Vendor,
			"subtotal":      ocrRes.Subtotal,
			"tax":           ocrRes.Tax,
			"total":         ocrRes.Total,
			"category":      ocrRes.Category,
			"purchase_date": ocrRes.PurchaseDate,
			"items":         itemsPayload,
		},
		"validation": validation.Info,
	}
	ocrReadyUpdate := map[string]interface{}{
		"vendor":                            ocrRes.Vendor,
		"subtotal":                          validation.Subtotal,
		"tax":                               ocrRes.Tax,
		"total":                             ocrRes.Total,
		"category":                          ocrRes.Category,
		"purchase_date":                     ocrRes.PurchaseDate,
		"items":                             itemsPayload,
		"extracted_text":                    ocrRes.Text,
		"extracted_fields":                  extractedFields,
	}
	// Expose extracted data as soon as OCR completes.
	if _, err := s.receipts.Doc(job.ID).Set(ctx, ocrReadyUpdate, fs.MergeAll); err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}
}

func (s *apiServer) markReceiptProcessingFailed(ctx context.Context, receiptID string, err error) {
	extractedFields := map[string]interface{}{
		"ocr_error": err.Error(),
	}
	if _, updateErr := s.receipts.Doc(receiptID).Set(ctx, map[string]interface{}{
		"extracted_fields": extractedFields,
	}, fs.MergeAll); updateErr != nil {
	}
}

func (s *apiServer) extractReceiptOCR(ctx context.Context, imageURL string, categoryOptions []string) (ocrResult, error) {
	if strings.TrimSpace(s.cfg.openAIAPIKey) == "" {
		return ocrResult{}, fmt.Errorf("OPENAI_API_KEY is required")
	}
	payload := openAIResponsesRequest{
		Model: s.cfg.openAIModel,
		Input: []openAIInputMessage{
			{
				Role: "user",
				Content: []openAIInputContent{
					{Type: "input_text", Text: buildOCRPrompt(categoryOptions)},
					{Type: "input_image", ImageURL: imageURL, Detail: "low"},
				},
			},
		},
		Temperature: 0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ocrResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(body))
	if err != nil {
		return ocrResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.openAIAPIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return ocrResult{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ocrResult{}, err
	}
	var envelope openAIResponsesEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return ocrResult{}, err
	}
	if resp.StatusCode >= 400 {
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			return ocrResult{}, fmt.Errorf("OpenAI API error: %s", envelope.Error.Message)
		}
		return ocrResult{}, fmt.Errorf("OpenAI API error: status %d", resp.StatusCode)
	}
	rawText := collectOCRText(envelope)
	return readStructuredFields(rawText, categoryOptions), nil
}

func (s *apiServer) signedImageURL(ctx context.Context, storagePath string) (string, error) {
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	url, err := s.bucket.SignedURL(storagePath, &gcs.SignedURLOptions{
		Scheme:  gcs.SigningSchemeV4,
		Method:  http.MethodGet,
		Expires: expiresAt,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create signed image URL: %w", err)
	}
	return url, nil
}

func (s *apiServer) attachSignedImageURL(ctx context.Context, data map[string]interface{}) {
	if data == nil {
		return
	}
	storagePath := stringFromAny(data["storage_path"])
	if storagePath == "" {
		return
	}
	if signedURL, err := s.signedImageURL(ctx, storagePath); err == nil {
		data["image_url"] = signedURL
	}
}

func buildOCRPrompt(categoryOptions []string) string {
	prompt := "Extract the readable text from this receipt image and summarize the line items and totals. " +
		"After the summary, output a JSON object with the following keys: `vendor`, `subtotal`, `tax`, `total`, " +
		"`category`, `purchase_date`, and `items`. The `items` array should include objects with `name`, `quantity`, " +
		"and `price`. Ensure the `subtotal` equals the sum of each item's `quantity` multiplied by its `price`; if you " +
		"can't confirm a value, set it to null. Do not add any explanation outside the JSON object."
	if len(categoryOptions) == 0 {
		return prompt
	}
	return prompt + " Use these categories when guessing the receipt type: " +
		strings.Join(categoryOptions, ", ") +
		". If none match, respond with null for the `category` key."
}

func collectOCRText(envelope openAIResponsesEnvelope) string {
	if strings.TrimSpace(envelope.OutputText) != "" {
		return strings.TrimSpace(envelope.OutputText)
	}
	parts := make([]string, 0)
	for _, block := range envelope.Output {
		for _, content := range block.Content {
			if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
				parts = append(parts, strings.TrimSpace(content.Text))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func readStructuredFields(rawText string, categoryOptions []string) ocrResult {
	payload := extractJSON(rawText)
	vendor := normalizeString(firstPresent(payload, "vendor", "store", "merchant", "merchant_name", "store_name"))
	subtotal := normalizeAmount(firstPresent(payload, "subtotal", "sub_total", "pre_tax"))
	tax := normalizeAmount(firstPresent(payload, "tax", "sales_tax", "tax_amount"))
	total := normalizeAmount(firstPresent(payload, "total", "grand_total", "amount_due", "total_amount"))
	category := normalizeString(firstPresent(payload, "category", "type"))
	if len(categoryOptions) > 0 {
		category = validateReceiptCategory(category, categoryOptions)
	}
	purchaseDate := normalizeString(firstPresent(payload, "purchase_date", "transaction_date", "date"))
	items := extractReceiptItems(payload)
	return ocrResult{
		Text:         rawText,
		Vendor:       vendor,
		Subtotal:     subtotal,
		Tax:          tax,
		Total:        total,
		Category:     category,
		PurchaseDate: purchaseDate,
		Items:        items,
	}
}

func extractJSON(rawText string) map[string]interface{} {
	start := strings.Index(rawText, "{")
	end := strings.LastIndex(rawText, "}")
	if start >= 0 && end > start {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(rawText[start:end+1]), &payload); err == nil {
			return payload
		}
	}
	return map[string]interface{}{}
}

func extractReceiptItems(payload map[string]interface{}) []ocrItem {
	var entries []interface{}
	for _, key := range []string{"items", "line_items", "entries"} {
		if raw, ok := payload[key].([]interface{}); ok {
			entries = raw
			break
		}
	}
	items := make([]ocrItem, 0)
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]interface{})
		if !ok {
			continue
		}
		name := normalizeString(firstPresent(entry, "name", "item", "description"))
		quantity := normalizeAmount(firstPresent(entry, "quantity", "qty", "count"))
		price := normalizeAmount(firstPresent(entry, "price", "unit_price", "amount"))
		if name == nil && quantity == nil && price == nil {
			continue
		}
		items = append(items, ocrItem{Name: name, Quantity: quantity, Price: price})
	}
	return items
}

func firstPresent(payload map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			return value
		}
	}
	return nil
}

func validateReceiptCategory(value *string, options []string) *string {
	if value == nil {
		return nil
	}
	target := strings.ToLower(strings.TrimSpace(*value))
	for _, option := range options {
		if strings.ToLower(strings.TrimSpace(option)) == target {
			matched := strings.TrimSpace(option)
			return &matched
		}
	}
	return nil
}

func ocrItemsToReceiptItems(items []ocrItem) []map[string]interface{} {
	payload := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		entry := map[string]interface{}{}
		if item.Name != nil {
			entry["name"] = strings.TrimSpace(*item.Name)
		} else {
			entry["name"] = ""
		}
		if item.Quantity != nil {
			entry["quantity"] = roundMoney(*item.Quantity)
		}
		if item.Price != nil {
			entry["price"] = roundMoney(*item.Price)
		}
		payload = append(payload, entry)
	}
	return payload
}

func parseReceiptItems(raw json.RawMessage) ([]receiptItem, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []receiptItem{}, nil
	}
	var decoded []map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("items must be a list")
	}
	items := make([]receiptItem, 0, len(decoded))
	for _, entry := range decoded {
		name := strings.TrimSpace(stringFromAny(entry["name"]))
		item := receiptItem{Name: name}
		if quantity := normalizeAmount(entry["quantity"]); quantity != nil {
			value := roundMoney(*quantity)
			item.Quantity = &value
		}
		if price := normalizeAmount(entry["price"]); price != nil {
			value := roundMoney(*price)
			item.Price = &value
		}
		items = append(items, item)
	}
	return items, nil
}

func itemsToMap(items []receiptItem) []map[string]interface{} {
	payload := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		entry := map[string]interface{}{"name": item.Name}
		if item.Quantity != nil {
			entry["quantity"] = *item.Quantity
		}
		if item.Price != nil {
			entry["price"] = *item.Price
		}
		payload = append(payload, entry)
	}
	return payload
}

func itemsTotal(items []map[string]interface{}) *float64 {
	var total float64
	counted := false
	for _, item := range items {
		qty := normalizeAmount(item["quantity"])
		price := normalizeAmount(item["price"])
		if qty == nil || price == nil {
			continue
		}
		total += (*qty) * (*price)
		counted = true
	}
	if !counted {
		return nil
	}
	value := roundMoney(total)
	return &value
}

func validateSubtotalAgainstItems(subtotal *float64, items []map[string]interface{}) subtotalValidation {
	computed := itemsTotal(items)
	info := map[string]interface{}{
		"items_total": computed,
	}
	updated := subtotal
	if computed == nil {
		return subtotalValidation{Subtotal: updated, Info: info}
	}
	if updated == nil {
		info["subtotal_inferred_from_items"] = true
		info["subtotal_matches_items"] = true
		value := *computed
		updated = &value
		return subtotalValidation{Subtotal: updated, Info: info}
	}
	match := math.Abs(*updated-*computed) <= 0.01
	info["subtotal_matches_items"] = match
	if !match {
		info["subtotal_overridden"] = true
		value := *computed
		updated = &value
	}
	return subtotalValidation{Subtotal: updated, Info: info}
}

func receiptRecordFromMap(id string, payload map[string]interface{}) receiptRecord {
	return receiptRecord{
		ID:                    id,
		Vendor:                valueStringPtr(payload["vendor"]),
		Subtotal:              existingFloatOrZeroPtr(payload["subtotal"]),
		Tax:                   existingFloatOrZeroPtr(payload["tax"]),
		Total:                 existingFloatOrZeroPtr(payload["total"]),
		Category:              valueStringPtr(payload["category"]),
		PurchaseDate:          valueStringPtr(payload["purchase_date"]),
		Items:                 receiptItemsFromAny(payload["items"]),
		ImageURL:              stringFromAny(payload["image_url"]),
		ExtractedText:         stringFromAny(payload["extracted_text"]),
		ExtractedFields:       cloneMap(payload["extracted_fields"]),
		CreatedAt:             timeFromAny(payload["created_at"]),
	}
}

func receiptItemsFromAny(value interface{}) []receiptItem {
	raw, ok := value.([]interface{})
	if !ok {
		if typed, ok := value.([]map[string]interface{}); ok {
			result := make([]receiptItem, 0, len(typed))
			for _, item := range typed {
				result = append(result, receiptItem{
					Name:     stringFromAny(item["name"]),
					Quantity: existingFloatOrZeroPtr(item["quantity"]),
					Price:    existingFloatOrZeroPtr(item["price"]),
				})
			}
			return result
		}
		return []receiptItem{}
	}
	result := make([]receiptItem, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		result = append(result, receiptItem{
			Name:     stringFromAny(item["name"]),
			Quantity: existingFloatOrZeroPtr(item["quantity"]),
			Price:    existingFloatOrZeroPtr(item["price"]),
		})
	}
	return result
}

func parseNullableString(raw json.RawMessage) interface{} {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

func parseNullableFloat(raw json.RawMessage) interface{} {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return roundMoney(value)
}

func normalizeString(value interface{}) *string {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return nil
	}
	return &text
}

func normalizeAmount(value interface{}) *float64 {
	switch typed := value.(type) {
	case nil:
		return nil
	case float64:
		v := roundMoney(typed)
		return &v
	case float32:
		v := roundMoney(float64(typed))
		return &v
	case int:
		v := roundMoney(float64(typed))
		return &v
	case int32:
		v := roundMoney(float64(typed))
		return &v
	case int64:
		v := roundMoney(float64(typed))
		return &v
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil
		}
		v := roundMoney(parsed)
		return &v
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		text = strings.ReplaceAll(text, "$", "")
		text = strings.ReplaceAll(text, ",", "")
		if text == "" {
			return nil
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil
		}
		v := roundMoney(parsed)
		return &v
	}
}

func existingFloatPtr(value interface{}) *float64 {
	return normalizeAmount(value)
}

func existingFloatOrZeroPtr(value interface{}) *float64 {
	parsed := normalizeAmount(value)
	if parsed != nil {
		return parsed
	}
	zero := 0.0
	return &zero
}

func valueFloatPtr(value interface{}) *float64 {
	return normalizeAmount(value)
}

func valueStringPtr(value interface{}) *string {
	return normalizeString(value)
}

func stringFromAny(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func timeFromAny(value interface{}) time.Time {
	if typed, ok := value.(time.Time); ok {
		return typed.UTC()
	}
	if text := stringFromAny(value); text != "" {
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func cloneMap(value interface{}) map[string]interface{} {
	typed, ok := value.(map[string]interface{})
	if !ok || typed == nil {
		return map[string]interface{}{}
	}
	cloned := make(map[string]interface{}, len(typed))
	for key, entry := range typed {
		cloned[key] = entry
	}
	return cloned
}

func roundMoney(value float64) float64 {
	return math.Round(value*100) / 100
}

func (s *apiServer) writeErr(writer http.ResponseWriter, err error) {
	var helcimErr helcimHTTPError
	if errors.As(err, &helcimErr) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(helcimErr.status)
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{"detail": helcimErr.detail})
		return
	}
	var httpErr httpError
	if errors.As(err, &httpErr) {
		writeJSONError(writer, httpErr.status, httpErr.detail)
		return
	}
	writeJSONError(writer, http.StatusInternalServerError, err.Error())
}
