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
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
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
	ProcessingStatus      string                 `json:"processing_status"`
	ProcessingError       *string                `json:"processing_error,omitempty"`
	ImageProcessingStatus string                 `json:"image_processing_status"`
	ImageProcessingError  *string                `json:"image_processing_error,omitempty"`
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

func (s *apiServer) handleReceipts(writer http.ResponseWriter, request *http.Request) {
	_, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	switch request.Method {
	case http.MethodPost:
		writeJSONError(writer, http.StatusGone, "Direct multipart upload is disabled. Use signed upload endpoints: POST /receipts/signed-upload then POST /receipts/finalize-upload.")
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
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
	if strings.HasSuffix(path, "/image") {
		receiptID := strings.TrimSuffix(path, "/image")
		receiptID = strings.TrimSuffix(receiptID, "/")
		if receiptID == "" || strings.Contains(receiptID, "/") {
			writeJSONError(writer, http.StatusNotFound, "Not found")
			return
		}
		if request.Method != http.MethodGet {
			writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		s.redirectReceiptImage(writer, request, user, receiptID)
		return
	}
	if strings.Contains(path, "/") {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	switch request.Method {
	case http.MethodPut:
		s.updateReceipt(writer, request, user, path)
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
		storagePath = stringFromAny(data["raw_storage_path"])
	}
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
	uploadURL, err := s.bucket.SignedURL(storagePath, &gcs.SignedURLOptions{
		Scheme:      gcs.SigningSchemeV4,
		Method:      http.MethodPut,
		Expires:     expiresAt,
		ContentType: contentType,
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"storage_path": storagePath,
		"upload_url":   uploadURL,
		"method":       http.MethodPut,
		"headers": map[string]string{
			"Content-Type": contentType,
		},
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *apiServer) finalizeSignedUpload(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
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
		writeJSONError(writer, http.StatusBadRequest, "uploaded object is empty")
		return
	}
	if err := ensureImage(strings.TrimSpace(attrs.ContentType)); err != nil {
		s.writeErr(writer, err)
		return
	}

	now := time.Now().UTC()
	docRef := s.receipts.NewDoc()
	imageURL, err := s.signedImageURL(request.Context(), storagePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	receiptPayload := map[string]interface{}{
		"vendor":                            normalizeOptionalString(payload.Vendor),
		"subtotal":                          payload.Subtotal,
		"tax":                               payload.Tax,
		"total":                             payload.Total,
		"category":                          normalizeOptionalString(payload.Category),
		"purchase_date":                     normalizeOptionalString(payload.PurchaseDate),
		"image_url":                         imageURL,
		"storage_path":                      storagePath,
		"raw_storage_path":                  storagePath,
		"items":                             []map[string]interface{}{},
		"extracted_text":                    "",
		"extracted_fields":                  map[string]interface{}{},
		"created_at":                        now,
		"owner_email":                       ownerEmail,
		"processing_status":                 "processing",
		"processing_owner":                  s.workerID,
		"processing_lease_expires_at":       time.Now().UTC().Add(s.cfg.receiptWorkerLease),
		"processing_error":                  nil,
		"processing_started_at":             time.Now().UTC(),
		"processing_attempts":               1,
		"image_processing_status":           "ready",
		"image_processing_error":            nil,
		"image_processing_owner":            nil,
		"image_processing_lease_expires_at": nil,
	}
	if _, err := docRef.Set(request.Context(), receiptPayload); err != nil {
		s.writeErr(writer, err)
		return
	}
	job := receiptJob{
		ID:             docRef.ID,
		OwnerEmail:     ownerEmail,
		RawStoragePath: storagePath,
	}
	s.processClaimedReceiptJob(request.Context(), job)
	updated, err := s.receipts.Doc(docRef.ID).Get(request.Context())
	if err == nil && updated.Exists() {
		updatedData := updated.Data()
		s.attachSignedImageURL(request.Context(), updatedData)
		writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, updatedData))
		return
	}
	writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, receiptPayload))
}

func (s *apiServer) createReceipt(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	ownerEmail := strings.TrimSpace(user.Email)
	if ownerEmail == "" {
		writeJSONError(writer, http.StatusUnauthorized, "OAuth token is missing an email address")
		return
	}
	if _, err := s.ensureWithinLimit(ownerEmail); err != nil {
		s.writeErr(writer, err)
		return
	}
	if err := request.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid multipart form")
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	contentType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if err := ensureImage(contentType); err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	docRef := s.receipts.NewDoc()
	rawStoragePath := buildStorageKeyForOwner(ownerEmail, header.Filename)
	if err := s.uploadRawImageStream(request.Context(), file, rawStoragePath, contentType); err != nil {
		s.writeErr(writer, err)
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), rawStoragePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	payload := map[string]interface{}{
		"vendor":                            firstFormString(request.FormValue("vendor")),
		"subtotal":                          firstFormFloat(request.FormValue("subtotal")),
		"tax":                               firstFormFloat(request.FormValue("tax")),
		"total":                             firstFormFloat(request.FormValue("total")),
		"category":                          firstFormString(request.FormValue("category")),
		"purchase_date":                     firstFormString(request.FormValue("purchase_date")),
		"image_url":                         imageURL,
		"storage_path":                      rawStoragePath,
		"raw_storage_path":                  rawStoragePath,
		"items":                             []map[string]interface{}{},
		"extracted_text":                    "",
		"extracted_fields":                  map[string]interface{}{},
		"created_at":                        now,
		"owner_email":                       ownerEmail,
		"processing_status":                 "processing",
		"processing_owner":                  s.workerID,
		"processing_lease_expires_at":       time.Now().UTC().Add(s.cfg.receiptWorkerLease),
		"processing_error":                  nil,
		"processing_started_at":             time.Now().UTC(),
		"processing_attempts":               1,
		"image_processing_status":           "ready",
		"image_processing_error":            nil,
		"image_processing_owner":            nil,
		"image_processing_lease_expires_at": nil,
	}
	if _, err := docRef.Set(request.Context(), payload); err != nil {
		_ = s.bucket.Object(rawStoragePath).Delete(request.Context())
		s.writeErr(writer, err)
		return
	}
	job := receiptJob{
		ID:             docRef.ID,
		OwnerEmail:     ownerEmail,
		RawStoragePath: rawStoragePath,
	}
	s.processClaimedReceiptJob(request.Context(), job)
	updated, err := s.receipts.Doc(docRef.ID).Get(request.Context())
	if err == nil && updated.Exists() {
		updatedData := updated.Data()
		updatedData["image_url"] = imageURL
		writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, updatedData))
		return
	}
	writeJSON(writer, http.StatusCreated, receiptRecordFromMap(docRef.ID, payload))
}

func (s *apiServer) updateReceipt(writer http.ResponseWriter, request *http.Request, user *verifiedUser, receiptID string) {
	defer request.Body.Close()
	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	existing, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	updateData := map[string]interface{}{}

	if raw, ok := payload["vendor"]; ok {
		updateData["vendor"] = parseNullableString(raw)
	}
	if raw, ok := payload["subtotal"]; ok {
		updateData["subtotal"] = parseNullableFloat(raw)
	}
	if raw, ok := payload["tax"]; ok {
		updateData["tax"] = parseNullableFloat(raw)
	}
	if raw, ok := payload["total"]; ok {
		updateData["total"] = parseNullableFloat(raw)
	}
	if raw, ok := payload["category"]; ok {
		updateData["category"] = parseNullableString(raw)
	}
	if raw, ok := payload["purchase_date"]; ok {
		updateData["purchase_date"] = parseNullableString(raw)
	}

	if raw, ok := payload["items"]; ok {
		items, err := parseReceiptItems(raw)
		if err != nil {
			writeJSONError(writer, http.StatusBadRequest, err.Error())
			return
		}
		itemsPayload := itemsToMap(items)
		updateData["items"] = itemsPayload
		subtotalCandidate := existingFloatPtr(existing["subtotal"])
		if rawSubtotal, hasSubtotal := updateData["subtotal"]; hasSubtotal {
			subtotalCandidate = valueFloatPtr(rawSubtotal)
		}
		validation := validateSubtotalAgainstItems(subtotalCandidate, itemsPayload)
		updateData["subtotal"] = validation.Subtotal
		extractedFields := cloneMap(existing["extracted_fields"])
		extractedFields["validation"] = validation.Info
		aiSuggestions := cloneMap(extractedFields["ai_suggestions"])
		aiSuggestions["items"] = itemsPayload
		extractedFields["ai_suggestions"] = aiSuggestions
		updateData["extracted_fields"] = extractedFields
	}

	if len(updateData) == 0 {
		s.attachSignedImageURL(request.Context(), existing)
		writeJSON(writer, http.StatusOK, receiptRecordFromMap(receiptID, existing))
		return
	}

	if _, err := s.receipts.Doc(receiptID).Set(request.Context(), updateData, fs.MergeAll); err != nil {
		s.writeErr(writer, err)
		return
	}
	updated, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	s.attachSignedImageURL(request.Context(), updated)
	writeJSON(writer, http.StatusOK, receiptRecordFromMap(receiptID, updated))
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
	rawStoragePath := strings.TrimSpace(stringFromAny(data["raw_storage_path"]))
	storagePath := strings.TrimSpace(stringFromAny(data["storage_path"]))
	if storagePath != "" {
		if err := s.bucket.Object(storagePath).Delete(request.Context()); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		}
	}
	if rawStoragePath != "" && rawStoragePath != storagePath {
		if err := s.bucket.Object(rawStoragePath).Delete(request.Context()); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		}
	}
	if rawStoragePath != "" {
		// No AVIF side objects are generated anymore.
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) redirectReceiptImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, receiptID string) {
	data, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := stringFromAny(data["storage_path"])
	if storagePath == "" {
		storagePath = stringFromAny(data["raw_storage_path"])
	}
	if storagePath == "" {
		writeJSONError(writer, http.StatusNotFound, "Receipt image not available")
		return
	}
	signedURL, err := s.signedImageURL(request.Context(), storagePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	http.Redirect(writer, request, signedURL, http.StatusTemporaryRedirect)
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
	if !strings.HasPrefix(strings.TrimSpace(contentType), "image/") {
		return httpError{status: http.StatusBadRequest, detail: "Uploaded file must be an image"}
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

func (s *apiServer) uploadRawImageStream(ctx context.Context, data io.Reader, destination string, contentType string) error {
	objWriter := s.bucket.Object(destination).NewWriter(ctx)
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	objWriter.ContentType = contentType
	if _, err := io.Copy(objWriter, data); err != nil {
		_ = objWriter.Close()
		return fmt.Errorf("failed to upload original image: %w", err)
	}
	if err := objWriter.Close(); err != nil {
		return fmt.Errorf("failed to upload original image: %w", err)
	}
	return nil
}

type receiptJob struct {
	ID             string
	OwnerEmail     string
	RawStoragePath string
}

func (s *apiServer) startReceiptWorker() {
	go func() {
		ticker := time.NewTicker(s.cfg.receiptWorkerPoll)
		defer ticker.Stop()
		for {
			select {
			case <-s.workerStop:
				return
			default:
			}
			for i := 0; i < 3; i++ {
				job, ok, err := s.claimNextReceiptJob(context.Background())
				if err != nil {
					break
				}
				if !ok {
					break
				}
				s.processClaimedReceiptJob(context.Background(), job)
			}
			select {
			case <-s.workerStop:
				return
			case <-s.workerWake:
			case <-ticker.C:
			}
		}
	}()
}

func (s *apiServer) wakeReceiptWorker() {
	select {
	case s.workerWake <- struct{}{}:
	default:
	}
}

func (s *apiServer) claimNextReceiptJob(ctx context.Context) (receiptJob, bool, error) {
	now := time.Now().UTC()
	queued, err := s.receipts.Where("processing_status", "==", "queued").Limit(25).Documents(ctx).GetAll()
	if err != nil {
		return receiptJob{}, false, err
	}
	for _, snapshot := range queued {
		job, ok, err := s.tryClaimReceiptJob(ctx, snapshot.Ref, now)
		if err != nil {
			return receiptJob{}, false, err
		}
		if ok {
			return job, true, nil
		}
	}

	stale, err := s.receipts.Where("processing_status", "==", "processing").Limit(25).Documents(ctx).GetAll()
	if err != nil {
		return receiptJob{}, false, err
	}
	for _, snapshot := range stale {
		job, ok, err := s.tryClaimReceiptJob(ctx, snapshot.Ref, now)
		if err != nil {
			return receiptJob{}, false, err
		}
		if ok {
			return job, true, nil
		}
	}
	return receiptJob{}, false, nil
}

func (s *apiServer) tryClaimReceiptJob(ctx context.Context, ref *fs.DocumentRef, now time.Time) (receiptJob, bool, error) {
	var claimed receiptJob
	err := s.firestore.RunTransaction(ctx, func(ctx context.Context, tx *fs.Transaction) error {
		snapshot, err := tx.Get(ref)
		if err != nil || !snapshot.Exists() {
			return err
		}
		data := snapshot.Data()
		status := fallbackString(data["processing_status"], "")
		lease := timeValue(data["processing_lease_expires_at"])
		if status != "queued" && !(status == "processing" && (lease == nil || !lease.After(now))) {
			return nil
		}
		ownerEmail := strings.TrimSpace(stringValue(data["owner_email"]))
		rawStoragePath := strings.TrimSpace(stringValue(data["raw_storage_path"]))
		if rawStoragePath == "" {
			rawStoragePath = strings.TrimSpace(stringValue(data["storage_path"]))
		}
		if ownerEmail == "" || rawStoragePath == "" {
			return nil
		}
		claimed = receiptJob{
			ID:             ref.ID,
			OwnerEmail:     ownerEmail,
			RawStoragePath: rawStoragePath,
		}
		tx.Set(ref, map[string]interface{}{
			"processing_status":                 "processing",
			"processing_owner":                  s.workerID,
			"processing_error":                  nil,
			"processing_lease_expires_at":       now.Add(s.cfg.receiptWorkerLease),
			"processing_started_at":             now,
			"processing_attempts":               fs.Increment(1),
			"image_processing_status":           "ready",
			"image_processing_error":            nil,
			"image_processing_owner":            nil,
			"image_processing_lease_expires_at": nil,
		}, fs.MergeAll)
		return nil
	})
	if err != nil {
		return receiptJob{}, false, err
	}
	if claimed.ID == "" {
		return receiptJob{}, false, nil
	}
	return claimed, true, nil
}

func (s *apiServer) processClaimedReceiptJob(ctx context.Context, job receiptJob) {
	s.processOCRForJob(ctx, job)
}

func (s *apiServer) processOCRForJob(ctx context.Context, job receiptJob) {
	categoryOptions, err := s.categoryNames(ctx, job.OwnerEmail)
	if err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}
	imageURL, err := s.signedImageURL(ctx, job.RawStoragePath)
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
		"processing_status":                 "ready",
		"processing_owner":                  s.workerID,
		"processing_error":                  nil,
		"processing_lease_expires_at":       nil,
		"processed_at":                      time.Now().UTC(),
		"image_processing_status":           "ready",
		"image_processing_error":            nil,
		"image_processing_owner":            nil,
		"image_processing_lease_expires_at": nil,
	}
	// Expose extracted data as soon as OCR completes.
	if _, err := s.receipts.Doc(job.ID).Set(ctx, ocrReadyUpdate, fs.MergeAll); err != nil {
		s.markReceiptProcessingFailed(ctx, job.ID, err)
		return
	}
}

func (s *apiServer) markReceiptProcessingFailed(ctx context.Context, receiptID string, err error) {
	if _, updateErr := s.receipts.Doc(receiptID).Set(ctx, map[string]interface{}{
		"processing_status":                 "failed",
		"processing_owner":                  s.workerID,
		"processing_error":                  err.Error(),
		"processing_lease_expires_at":       nil,
		"processed_at":                      time.Now().UTC(),
		"image_processing_status":           "failed",
		"image_processing_error":            err.Error(),
		"image_processing_owner":            s.workerID,
		"image_processing_lease_expires_at": nil,
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
					{Type: "input_image", ImageURL: imageURL},
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
		storagePath = stringFromAny(data["raw_storage_path"])
	}
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
		ProcessingStatus:      fallbackString(payload["processing_status"], "ready"),
		ProcessingError:       valueStringPtr(payload["processing_error"]),
		ImageProcessingStatus: fallbackString(payload["image_processing_status"], "ready"),
		ImageProcessingError:  valueStringPtr(payload["image_processing_error"]),
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

func firstFormString(value string) *string {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	return &text
}

func firstFormFloat(value string) *float64 {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	rounded := roundMoney(parsed)
	return &rounded
}

func subtotalValueOrOCR(formValue *float64, ocrValue *float64) *float64 {
	if formValue != nil {
		return formValue
	}
	return ocrValue
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
