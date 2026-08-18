package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const (
	aiAccessTokensCollection = "ai_access_tokens"
	aiTokenPrefix            = "rk_ai_"
)

type verifiedAIUser struct {
	Email string
}

type aiReceiptSummary struct {
	ID           string  `json:"id"`
	Vendor       *string `json:"vendor,omitempty"`
	Total        *float64 `json:"total,omitempty"`
	Category     *string `json:"category,omitempty"`
	PurchaseDate *string `json:"purchase_date,omitempty"`
	CreatedAt    string  `json:"created_at,omitempty"`
	sortTime     time.Time
}

func (s *apiServer) handleAIAccessToken(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")

	switch request.Method {
	case http.MethodGet:
		s.getAIAccessTokenStatus(writer, request, user)
	case http.MethodPost:
		s.createAIAccessToken(writer, request, user)
	case http.MethodDelete:
		s.revokeAIAccessToken(writer, request, user)
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *apiServer) getAIAccessTokenStatus(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	snapshot, err := s.findAIAccessTokenByOwner(request.Context(), user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if snapshot == nil {
		writeJSON(writer, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	data := snapshot.Data()
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"enabled":    true,
		"key_prefix": aiTokenPrefix + snapshot.Ref.ID + ".",
		"created_at": isoOrNil(data["created_at"]),
	})
}

func (s *apiServer) createAIAccessToken(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	keyID, err := randomURLSafeString(9)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	secret, err := randomURLSafeString(32)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	secretHash := sha256.Sum256([]byte(secret))
	now := time.Now().UTC()
	collection := s.firestore.Collection(aiAccessTokensCollection)

	existing, err := s.listAIAccessTokensByOwner(request.Context(), user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	batch := s.firestore.Batch()
	for _, snapshot := range existing {
		batch.Delete(snapshot.Ref)
	}
	batch.Set(collection.Doc(keyID), map[string]interface{}{
		"owner_email": strings.TrimSpace(user.Email),
		"secret_hash": hex.EncodeToString(secretHash[:]),
		"created_at":  now,
	})
	if _, err := batch.Commit(request.Context()); err != nil {
		s.writeErr(writer, err)
		return
	}

	writeJSON(writer, http.StatusCreated, map[string]interface{}{
		"enabled":    true,
		"api_key":    aiTokenPrefix + keyID + "." + secret,
		"key_prefix": aiTokenPrefix + keyID + ".",
		"created_at": now.Format(time.RFC3339),
	})
}

func (s *apiServer) revokeAIAccessToken(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	existing, err := s.listAIAccessTokensByOwner(request.Context(), user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if len(existing) == 0 {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	batch := s.firestore.Batch()
	for _, snapshot := range existing {
		batch.Delete(snapshot.Ref)
	}
	if _, err := batch.Commit(request.Context()); err != nil {
		s.writeErr(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) findAIAccessTokenByOwner(ctx interface{ Done() <-chan struct{} }, ownerEmail string) (*fs.DocumentSnapshot, error) {
	// Kept as a narrow wrapper so token lifecycle stays server-owned.
	iter := s.firestore.Collection(aiAccessTokensCollection).
		Where("owner_email", "==", strings.TrimSpace(ownerEmail)).
		Limit(1).
		Documents(requestContext())
	defer iter.Stop()
	snapshot, err := iter.Next()
	if errors.Is(err, iterator.Done) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *apiServer) listAIAccessTokensByOwner(ctx interface{ Done() <-chan struct{} }, ownerEmail string) ([]*fs.DocumentSnapshot, error) {
	iter := s.firestore.Collection(aiAccessTokensCollection).
		Where("owner_email", "==", strings.TrimSpace(ownerEmail)).
		Documents(requestContext())
	defer iter.Stop()
	result := make([]*fs.DocumentSnapshot, 0, 1)
	for {
		snapshot, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func (s *apiServer) authenticateAIReceiptToken(writer http.ResponseWriter, request *http.Request) (*verifiedAIUser, bool) {
	authHeader := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Missing AI access token")
		return nil, false
	}
	rawToken := strings.TrimSpace(authHeader[len("Bearer "):])
	keyID, secret, ok := parseAIToken(rawToken)
	if !ok {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Invalid AI access token")
		return nil, false
	}

	snapshot, err := s.firestore.Collection(aiAccessTokensCollection).Doc(keyID).Get(request.Context())
	if err != nil || !snapshot.Exists() {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Invalid AI access token")
		return nil, false
	}
	data := snapshot.Data()
	ownerEmail := strings.TrimSpace(stringFromAny(data["owner_email"]))
	storedHash, err := hex.DecodeString(strings.TrimSpace(stringFromAny(data["secret_hash"])))
	if ownerEmail == "" || err != nil || len(storedHash) != sha256.Size {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Invalid AI access token")
		return nil, false
	}
	presentedHash := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(storedHash, presentedHash[:]) != 1 {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Invalid AI access token")
		return nil, false
	}
	return &verifiedAIUser{Email: ownerEmail}, true
}

func (s *apiServer) handleAIReceipts(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	user, ok := s.authenticateAIReceiptToken(writer, request)
	if !ok {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")

	summaries, err := s.listAIReceiptSummaries(request.Context(), user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"receipts": summaries,
		"count":    len(summaries),
	})
}

func (s *apiServer) handleAIReceiptByID(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	user, ok := s.authenticateAIReceiptToken(writer, request)
	if !ok {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")

	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/ai/receipts/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" || len(parts) > 2 {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	receiptID := strings.TrimSpace(parts[0])
	receipt, err := s.getOwnedReceipt(request.Context(), receiptID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}

	if len(parts) == 1 {
		writeJSON(writer, http.StatusOK, receiptRecordFromMap(receiptID, receipt.Data))
		return
	}
	if parts[1] != "image" {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	storagePath := strings.TrimSpace(stringFromAny(receipt.Data["storage_path"]))
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

func (s *apiServer) listAIReceiptSummaries(ctx interface{ Done() <-chan struct{} }, ownerEmail string) ([]aiReceiptSummary, error) {
	iter := s.receipts.
		Where(receiptShardSchemaField, "==", receiptShardSchema).
		Where("owner_email", "==", strings.TrimSpace(ownerEmail)).
		Documents(requestContext())
	defer iter.Stop()

	result := make([]aiReceiptSummary, 0)
	for {
		snapshot, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		metadataMap, _ := snapshot.Data()[receiptShardMetadataField].(map[string]interface{})
		for receiptID, raw := range metadataMap {
			metadata, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if strings.TrimSpace(stringFromAny(metadata["owner_email"])) != strings.TrimSpace(ownerEmail) {
				continue
			}
			created := timeFromAny(metadata["created_at"])
			createdText := ""
			if !created.IsZero() {
				createdText = created.Format(time.RFC3339)
			}
			result = append(result, aiReceiptSummary{
				ID:           receiptID,
				Vendor:       valueStringPtr(metadata["vendor"]),
				Total:        existingFloatPtr(metadata["total"]),
				Category:     valueStringPtr(metadata["category"]),
				PurchaseDate: valueStringPtr(metadata["purchase_date"]),
				CreatedAt:    createdText,
				sortTime:     created,
			})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].sortTime.After(result[j].sortTime)
	})
	return result, nil
}

func randomURLSafeString(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func parseAIToken(raw string) (string, string, bool) {
	if !strings.HasPrefix(raw, aiTokenPrefix) {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, aiTokenPrefix), ".", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	if strings.Contains(parts[1], ".") {
		return "", "", false
	}
	return parts[0], parts[1], true
}
