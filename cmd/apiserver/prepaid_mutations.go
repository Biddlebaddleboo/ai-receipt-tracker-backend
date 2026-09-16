package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
)

const prepaidPendingCleanupField = "pending_image_cleanup"

var prepaidCardImageFields = []string{
	"package_image_storage_path",
	"card_front_image_storage_path",
	"card_back_image_storage_path",
	"opened_card_image_storage_path",
}

type prepaidImageMutationRequest struct {
	StoragePath         string `json:"storage_path"`
	ExpectedStoragePath string `json:"expected_storage_path,omitempty"`
}

type prepaidPurchaseMutator func(data map[string]interface{}) ([]string, error)

func prepaidImageTypeFromRoute(value string) (prepaidImageType, bool) {
	switch strings.TrimSpace(value) {
	case "package", "package-image":
		return prepaidImagePackage, true
	case "card-front", "card-front-image":
		return prepaidImageCardFront, true
	case "card-back", "card-back-image":
		return prepaidImageCardBack, true
	case "opened-card", "opened-card-image":
		return prepaidImageOpenedCard, true
	default:
		return "", false
	}
}

func prepaidCardImageField(imageType prepaidImageType) (string, bool) {
	switch imageType {
	case prepaidImagePackage:
		return "package_image_storage_path", true
	case prepaidImageCardFront:
		return "card_front_image_storage_path", true
	case prepaidImageCardBack:
		return "card_back_image_storage_path", true
	case prepaidImageOpenedCard:
		return "opened_card_image_storage_path", true
	default:
		return "", false
	}
}

func decodePrepaidImageMutationRequest(request *http.Request) (prepaidImageMutationRequest, error) {
	var payload prepaidImageMutationRequest
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return payload, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		payload.ExpectedStoragePath = strings.TrimSpace(request.URL.Query().Get("expected_storage_path"))
		return payload, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, httpError{status: http.StatusBadRequest, detail: "invalid request body"}
	}
	if payload.ExpectedStoragePath == "" {
		payload.ExpectedStoragePath = strings.TrimSpace(request.URL.Query().Get("expected_storage_path"))
	}
	payload.StoragePath = strings.TrimSpace(payload.StoragePath)
	payload.ExpectedStoragePath = strings.TrimSpace(payload.ExpectedStoragePath)
	return payload, nil
}

func prepaidRawEntries(value interface{}) []interface{} {
	switch typed := value.(type) {
	case []interface{}:
		return append([]interface{}(nil), typed...)
	case []map[string]interface{}:
		result := make([]interface{}, 0, len(typed))
		for _, entry := range typed {
			result = append(result, entry)
		}
		return result
	default:
		return nil
	}
}

func clonePrepaidEntries(value interface{}) []interface{} {
	entries := prepaidRawEntries(value)
	result := make([]interface{}, 0, len(entries))
	for _, raw := range entries {
		if data, ok := raw.(map[string]interface{}); ok {
			result = append(result, cloneMap(data))
			continue
		}
		result = append(result, raw)
	}
	return result
}

func clonePrepaidPurchaseData(data map[string]interface{}) map[string]interface{} {
	result := cloneMap(data)
	result["cards"] = clonePrepaidEntries(data["cards"])
	result["activation_receipts"] = clonePrepaidEntries(data["activation_receipts"])
	result[prepaidPendingCleanupField] = append([]string(nil), prepaidPendingCleanupPaths(data[prepaidPendingCleanupField])...)
	return result
}

func prepaidPendingCleanupPaths(value interface{}) []string {
	result := make([]string, 0)
	switch typed := value.(type) {
	case []string:
		for _, path := range typed {
			if path = strings.TrimSpace(path); path != "" {
				result = append(result, path)
			}
		}
	case []interface{}:
		for _, entry := range typed {
			if path := strings.TrimSpace(stringFromAny(entry)); path != "" {
				result = append(result, path)
			}
		}
	}
	return dedupePrepaidPaths(result)
}

func dedupePrepaidPaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func prepaidCurrentImagePaths(data map[string]interface{}) map[string]struct{} {
	current := make(map[string]struct{})
	for _, receipt := range prepaidActivationReceiptsFromAny(data["activation_receipts"]) {
		if path := strings.TrimSpace(receipt.StoragePath); path != "" {
			current[path] = struct{}{}
		}
	}
	for _, card := range prepaidCardsFromAny(data["cards"]) {
		for _, path := range []string{
			card.PackageImageStoragePath,
			card.CardFrontImageStoragePath,
			card.CardBackImageStoragePath,
			card.OpenedCardImageStoragePath,
		} {
			if path = strings.TrimSpace(path); path != "" {
				current[path] = struct{}{}
			}
		}
	}
	return current
}

func queuePrepaidImageCleanup(data map[string]interface{}, paths []string) {
	current := prepaidCurrentImagePaths(data)
	combined := append(prepaidPendingCleanupPaths(data[prepaidPendingCleanupField]), paths...)
	queued := make([]string, 0, len(combined))
	for _, path := range dedupePrepaidPaths(combined) {
		if _, stillCurrent := current[path]; stillCurrent {
			continue
		}
		queued = append(queued, path)
	}
	data[prepaidPendingCleanupField] = queued
}

func prepaidCardImagePath(card prepaidCardRecord, field string) string {
	switch field {
	case "package_image_storage_path":
		return strings.TrimSpace(card.PackageImageStoragePath)
	case "card_front_image_storage_path":
		return strings.TrimSpace(card.CardFrontImageStoragePath)
	case "card_back_image_storage_path":
		return strings.TrimSpace(card.CardBackImageStoragePath)
	case "opened_card_image_storage_path":
		return strings.TrimSpace(card.OpenedCardImageStoragePath)
	default:
		return ""
	}
}

func samePrepaidPaths(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// mergePrepaidCleanupSnapshotData applies only cleanup changes derived from a
// baseline snapshot. If another mutation changed an image slot after that
// snapshot, the current value wins and is left untouched.
func mergePrepaidCleanupSnapshotData(baselineData map[string]interface{}, currentData map[string]interface{}, updated prepaidPurchaseRecord) (map[string]interface{}, bool, error) {
	data := clonePrepaidPurchaseData(currentData)
	changed := false
	removedPending := make(map[string]struct{})

	baselineCards := make(map[string]prepaidCardRecord)
	for _, card := range prepaidCardsFromAny(baselineData["cards"]) {
		baselineCards[strings.TrimSpace(card.ID)] = card
	}
	desiredCards := make(map[string]prepaidCardRecord)
	for _, card := range updated.Cards {
		desiredCards[strings.TrimSpace(card.ID)] = card
	}
	currentCards := prepaidRawEntries(currentData["cards"])
	for _, rawCard := range currentCards {
		card, ok := rawCard.(map[string]interface{})
		if !ok {
			continue
		}
		cardID := strings.TrimSpace(stringFromAny(card["id"]))
		baselineCard, hadBaseline := baselineCards[cardID]
		desiredCard, hasDesired := desiredCards[cardID]
		if !hadBaseline || !hasDesired {
			continue
		}
		for _, field := range prepaidCardImageFields {
			baselinePath := prepaidCardImagePath(baselineCard, field)
			desiredPath := prepaidCardImagePath(desiredCard, field)
			if baselinePath == desiredPath {
				continue
			}
			if baselinePath != "" && desiredPath == "" {
				removedPending[baselinePath] = struct{}{}
			}
			currentPath := strings.TrimSpace(stringFromAny(card[field]))
			if currentPath == baselinePath {
				card[field] = desiredPath
				changed = true
			}
		}
	}
	data["cards"] = currentCards

	baselineReceipts := make(map[string]prepaidActivationReceipt)
	for _, receipt := range prepaidActivationReceiptsFromAny(baselineData["activation_receipts"]) {
		baselineReceipts[strings.TrimSpace(receipt.ID)] = receipt
	}
	desiredReceipts := make(map[string]prepaidActivationReceipt)
	for _, receipt := range updated.ActivationReceipts {
		desiredReceipts[strings.TrimSpace(receipt.ID)] = receipt
	}
	currentReceipts := prepaidRawEntries(currentData["activation_receipts"])
	for _, rawReceipt := range currentReceipts {
		receipt, ok := rawReceipt.(map[string]interface{})
		if !ok {
			continue
		}
		receiptID := strings.TrimSpace(stringFromAny(receipt["id"]))
		baselineReceipt, hadBaseline := baselineReceipts[receiptID]
		desiredReceipt, hasDesired := desiredReceipts[receiptID]
		if !hadBaseline || !hasDesired {
			continue
		}
		baselinePath := strings.TrimSpace(baselineReceipt.StoragePath)
		desiredPath := strings.TrimSpace(desiredReceipt.StoragePath)
		if baselinePath != "" && baselinePath != desiredPath && desiredPath == "" {
			removedPending[baselinePath] = struct{}{}
		}
		if baselinePath != desiredPath && strings.TrimSpace(stringFromAny(receipt["storage_path"])) == baselinePath {
			receipt["storage_path"] = desiredPath
			changed = true
		}
	}
	data["activation_receipts"] = currentReceipts

	baselinePending := make(map[string]struct{})
	for _, path := range prepaidPendingCleanupPaths(baselineData[prepaidPendingCleanupField]) {
		baselinePending[path] = struct{}{}
	}
	desiredPending := make(map[string]struct{})
	for _, path := range dedupePrepaidPaths(updated.PendingImageCleanup) {
		desiredPending[path] = struct{}{}
	}
	for path := range baselinePending {
		if _, stillPending := desiredPending[path]; !stillPending {
			removedPending[path] = struct{}{}
		}
	}
	currentPending := prepaidPendingCleanupPaths(currentData[prepaidPendingCleanupField])
	currentPaths := prepaidCurrentImagePaths(data)
	remainingPending := make([]string, 0, len(currentPending))
	for _, path := range currentPending {
		if _, removed := removedPending[path]; removed {
			changed = true
			continue
		}
		if _, current := currentPaths[path]; current {
			changed = true
			continue
		}
		remainingPending = append(remainingPending, path)
	}
	if !samePrepaidPaths(currentPending, remainingPending) {
		changed = true
	}
	data[prepaidPendingCleanupField] = remainingPending
	return data, changed, nil
}

func prepaidPurchaseRecordFromData(id string, data map[string]interface{}) prepaidPurchaseRecord {
	cards := prepaidCardsFromAny(data["cards"])
	activeCount := intFromAny(data["active_card_count"])
	archivedCount := intFromAny(data["archived_card_count"])
	if activeCount == 0 && archivedCount == 0 && len(cards) > 0 {
		activeCount = len(filterPrepaidCards(cards, "active"))
		archivedCount = len(filterPrepaidCards(cards, "archived"))
	}
	return prepaidPurchaseRecord{
		ID:                  id,
		OwnerEmail:          stringFromAny(data["owner_email"]),
		SalesReceiptID:      stringFromAny(data["sales_receipt_id"]),
		ActivationReceipts:  prepaidActivationReceiptsFromAny(data["activation_receipts"]),
		Cards:               cards,
		ActiveCardCount:     activeCount,
		ArchivedCardCount:   archivedCount,
		PendingImageCleanup: prepaidPendingCleanupPaths(data[prepaidPendingCleanupField]),
		CreatedAt:           isoString(data["created_at"]),
		UpdatedAt:           isoString(data["updated_at"]),
	}
}

func prepaidPurchaseRecordToData(record prepaidPurchaseRecord) map[string]interface{} {
	cards := make([]interface{}, 0, len(record.Cards))
	for _, card := range record.Cards {
		cards = append(cards, map[string]interface{}{
			"id":                             card.ID,
			"activation_receipt_id":          card.ActivationReceiptID,
			"activation_barcode":             card.ActivationBarcode,
			"vanilla_serial":                 card.VanillaSerial,
			"denomination":                   card.Denomination,
			"pan":                            card.PAN,
			"expiry":                         card.Expiry,
			"cvv":                            card.CVV,
			"last4":                          card.Last4,
			"details_captured":               card.DetailsCaptured,
			"state":                          card.State,
			"archived_at":                    card.ArchivedAt,
			"package_image_storage_path":     card.PackageImageStoragePath,
			"card_front_image_storage_path":  card.CardFrontImageStoragePath,
			"card_back_image_storage_path":   card.CardBackImageStoragePath,
			"opened_card_image_storage_path": card.OpenedCardImageStoragePath,
			"extraction_status":              card.ExtractionStatus,
			"created_at":                     card.CreatedAt,
			"updated_at":                     card.UpdatedAt,
		})
	}
	receipts := make([]interface{}, 0, len(record.ActivationReceipts))
	for _, receipt := range record.ActivationReceipts {
		receipts = append(receipts, map[string]interface{}{
			"id":           receipt.ID,
			"storage_path": receipt.StoragePath,
			"filename":     receipt.Filename,
			"content_type": receipt.ContentType,
			"created_at":   receipt.CreatedAt,
		})
	}
	return map[string]interface{}{
		"owner_email":              record.OwnerEmail,
		"sales_receipt_id":         record.SalesReceiptID,
		"activation_receipts":      receipts,
		"cards":                    cards,
		"active_card_count":        record.ActiveCardCount,
		"archived_card_count":      record.ArchivedCardCount,
		prepaidPendingCleanupField: append([]string(nil), record.PendingImageCleanup...),
		"created_at":               record.CreatedAt,
		"updated_at":               record.UpdatedAt,
	}
}

func (s *apiServer) mutateOwnedPrepaidPurchase(ctx context.Context, purchaseID string, ownerEmail string, mutate prepaidPurchaseMutator) (prepaidPurchaseRecord, error) {
	if prepaidPurchaseMutationOverride != nil {
		record, err := prepaidPurchaseMutationOverride(s, ctx, purchaseID, ownerEmail, mutate)
		if err != nil {
			return prepaidPurchaseRecord{}, err
		}
		return s.retryPrepaidPendingCleanupInMemory(ctx, record), nil
	}
	if s.firestore == nil {
		return prepaidPurchaseRecord{}, fmt.Errorf("prepaid Firestore client is unavailable")
	}
	ref := s.firestore.Collection(prepaidPurchasesCollection).Doc(strings.TrimSpace(purchaseID))
	if strings.TrimSpace(purchaseID) == "" {
		return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
	}
	err := s.firestore.RunTransaction(ctx, func(ctx context.Context, tx *fs.Transaction) error {
		snapshot, err := tx.Get(ref)
		if err != nil || !snapshot.Exists() {
			return httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
		}
		if !prepaidPurchaseBelongsToOwner(snapshot.Data(), ownerEmail) {
			return httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
		}
		data := clonePrepaidPurchaseData(snapshot.Data())
		cleanupPaths, err := mutate(data)
		if err != nil {
			return err
		}
		queuePrepaidImageCleanup(data, cleanupPaths)
		data["updated_at"] = time.Now().UTC()
		tx.Set(ref, data, fs.MergeAll)
		return nil
	})
	if err != nil {
		return prepaidPurchaseRecord{}, err
	}
	snapshot, err := ref.Get(ctx)
	if err != nil || !snapshot.Exists() {
		if err != nil {
			return prepaidPurchaseRecord{}, err
		}
		return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
	}
	record := prepaidPurchaseFromSnapshot(snapshot)
	return s.retryPrepaidPendingCleanup(ctx, record)
}

func (s *apiServer) retryPrepaidPendingCleanupInMemory(ctx context.Context, record prepaidPurchaseRecord) prepaidPurchaseRecord {
	current := make(map[string]struct{})
	for _, receipt := range record.ActivationReceipts {
		if path := strings.TrimSpace(receipt.StoragePath); path != "" {
			current[path] = struct{}{}
		}
	}
	for _, card := range record.Cards {
		for _, path := range []string{card.PackageImageStoragePath, card.CardFrontImageStoragePath, card.CardBackImageStoragePath, card.OpenedCardImageStoragePath} {
			if path = strings.TrimSpace(path); path != "" {
				current[path] = struct{}{}
			}
		}
	}
	remaining := make([]string, 0, len(record.PendingImageCleanup))
	for _, path := range dedupePrepaidPaths(record.PendingImageCleanup) {
		if _, stillCurrent := current[path]; stillCurrent {
			continue
		}
		if err := s.deletePrepaidImage(ctx, record.OwnerEmail, path); err != nil {
			remaining = append(remaining, path)
		}
	}
	record.PendingImageCleanup = remaining
	return record
}

func (s *apiServer) retryPrepaidPendingCleanup(ctx context.Context, record prepaidPurchaseRecord) (prepaidPurchaseRecord, error) {
	if len(record.PendingImageCleanup) == 0 {
		return record, nil
	}
	if s.firestore == nil {
		return s.retryPrepaidPendingCleanupInMemory(ctx, record), nil
	}
	current := make(map[string]struct{})
	for _, receipt := range record.ActivationReceipts {
		if path := strings.TrimSpace(receipt.StoragePath); path != "" {
			current[path] = struct{}{}
		}
	}
	for _, card := range record.Cards {
		for _, path := range []string{card.PackageImageStoragePath, card.CardFrontImageStoragePath, card.CardBackImageStoragePath, card.OpenedCardImageStoragePath} {
			if path = strings.TrimSpace(path); path != "" {
				current[path] = struct{}{}
			}
		}
	}
	completed := make(map[string]struct{})
	for _, path := range dedupePrepaidPaths(record.PendingImageCleanup) {
		if _, stillCurrent := current[path]; stillCurrent {
			completed[path] = struct{}{}
			continue
		}
		if err := s.deletePrepaidImage(ctx, record.OwnerEmail, path); err == nil {
			completed[path] = struct{}{}
		}
	}
	if len(completed) == 0 {
		return record, nil
	}
	ref := s.firestore.Collection(prepaidPurchasesCollection).Doc(strings.TrimSpace(record.ID))
	err := s.firestore.RunTransaction(ctx, func(ctx context.Context, tx *fs.Transaction) error {
		snapshot, err := tx.Get(ref)
		if err != nil || !snapshot.Exists() {
			return httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
		}
		if !prepaidPurchaseBelongsToOwner(snapshot.Data(), record.OwnerEmail) {
			return httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
		}
		data := clonePrepaidPurchaseData(snapshot.Data())
		currentNow := prepaidCurrentImagePaths(data)
		remaining := make([]string, 0)
		for _, path := range prepaidPendingCleanupPaths(data[prepaidPendingCleanupField]) {
			if _, done := completed[path]; done {
				continue
			}
			if _, stillCurrent := currentNow[path]; stillCurrent {
				continue
			}
			remaining = append(remaining, path)
		}
		data[prepaidPendingCleanupField] = remaining
		data["updated_at"] = time.Now().UTC()
		tx.Set(ref, data, fs.MergeAll)
		return nil
	})
	if err != nil {
		log.Printf("prepaid pending cleanup state update failed purchase_id=%s err=%v", record.ID, err)
		return record, nil
	}
	snapshot, err := ref.Get(ctx)
	if err != nil || !snapshot.Exists() {
		if err != nil {
			return record, err
		}
		return record, nil
	}
	return prepaidPurchaseFromSnapshot(snapshot), nil
}

func validatePrepaidActivationAssociation(cards []interface{}, candidateID string, excludedCardID string, allowSame bool) error {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return nil
	}
	for _, rawCard := range cards {
		card, ok := rawCard.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.TrimSpace(stringFromAny(card["id"])) == strings.TrimSpace(excludedCardID) {
			if allowSame {
				continue
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(stringFromAny(card["activation_receipt_id"])), candidateID) {
			return httpError{status: http.StatusBadRequest, detail: "activation_receipt_id is already linked to another card"}
		}
	}
	return nil
}

func validatePrepaidActivationInputAssociations(inputs []prepaidCardInput) error {
	seen := make(map[string]struct{})
	for _, input := range inputs {
		id := strings.ToLower(strings.TrimSpace(input.ActivationReceiptID))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			return httpError{status: http.StatusBadRequest, detail: "activation_receipt_id is already linked to another card"}
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (s *apiServer) deletePrepaidActivationReceipt(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, receiptID string) {
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		receipts := prepaidRawEntries(data["activation_receipts"])
		found := false
		oldPath := ""
		filtered := make([]interface{}, 0, len(receipts))
		for _, rawReceipt := range receipts {
			receipt, ok := rawReceipt.(map[string]interface{})
			if !ok || !strings.EqualFold(strings.TrimSpace(stringFromAny(receipt["id"])), receiptID) {
				filtered = append(filtered, rawReceipt)
				continue
			}
			found = true
			oldPath = strings.TrimSpace(stringFromAny(receipt["storage_path"]))
		}
		if !found {
			return nil, httpError{status: http.StatusNotFound, detail: "Activation receipt not found"}
		}
		data["activation_receipts"] = filtered
		for _, rawCard := range prepaidRawEntries(data["cards"]) {
			card, ok := rawCard.(map[string]interface{})
			if ok && strings.EqualFold(strings.TrimSpace(stringFromAny(card["activation_receipt_id"])), receiptID) {
				delete(card, "activation_receipt_id")
			}
		}
		if oldPath == "" {
			return nil, nil
		}
		return []string{oldPath}, nil
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) deletePrepaidCard(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string) {
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		cards := prepaidRawEntries(data["cards"])
		filtered := make([]interface{}, 0, len(cards))
		found := false
		cleanupPaths := make([]string, 0, 4)
		for _, rawCard := range cards {
			card, ok := rawCard.(map[string]interface{})
			if !ok || strings.TrimSpace(stringFromAny(card["id"])) != cardID {
				filtered = append(filtered, rawCard)
				continue
			}
			found = true
			for _, field := range prepaidCardImageFields {
				if path := strings.TrimSpace(stringFromAny(card[field])); path != "" {
					cleanupPaths = append(cleanupPaths, path)
				}
			}
		}
		if !found {
			return nil, httpError{status: http.StatusNotFound, detail: "Card not found"}
		}
		data["cards"] = filtered
		data["active_card_count"] = countCardsByState(filtered, "active")
		data["archived_card_count"] = countCardsByState(filtered, "archived")
		return cleanupPaths, nil
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) deletePrepaidActivationReceiptImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, receiptID string) {
	defer request.Body.Close()
	payload, err := decodePrepaidImageMutationRequest(request)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	oldPath := ""
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		for _, rawReceipt := range prepaidRawEntries(data["activation_receipts"]) {
			receipt, ok := rawReceipt.(map[string]interface{})
			if !ok || !strings.EqualFold(strings.TrimSpace(stringFromAny(receipt["id"])), receiptID) {
				continue
			}
			oldPath = strings.TrimSpace(stringFromAny(receipt["storage_path"]))
			if oldPath == "" {
				return nil, httpError{status: http.StatusNotFound, detail: "Activation receipt image not found"}
			}
			if err := validatePrepaidExpectedPath(oldPath, payload.ExpectedStoragePath); err != nil {
				return nil, err
			}
			receipt["storage_path"] = ""
			return []string{oldPath}, nil
		}
		return nil, httpError{status: http.StatusNotFound, detail: "Activation receipt not found"}
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, prepaidImageMutationResponse(purchaseID, "", receiptID, prepaidImageActivation, "", record, oldPath))
}

func (s *apiServer) replacePrepaidActivationReceiptImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, receiptID string) {
	defer request.Body.Close()
	payload, err := decodePrepaidImageMutationRequest(request)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if payload.StoragePath == "" {
		writeJSONError(writer, http.StatusBadRequest, "storage_path is required")
		return
	}
	if err := s.ensurePrepaidUploadedImage(request.Context(), user.Email, payload.StoragePath); err != nil {
		s.writeErr(writer, err)
		return
	}
	oldPath := ""
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		for _, rawReceipt := range prepaidRawEntries(data["activation_receipts"]) {
			receipt, ok := rawReceipt.(map[string]interface{})
			if !ok || !strings.EqualFold(strings.TrimSpace(stringFromAny(receipt["id"])), receiptID) {
				continue
			}
			oldPath = strings.TrimSpace(stringFromAny(receipt["storage_path"]))
			if err := validatePrepaidExpectedPath(oldPath, payload.ExpectedStoragePath); err != nil {
				return nil, err
			}
			receipt["storage_path"] = payload.StoragePath
			if oldPath == "" || oldPath == payload.StoragePath {
				return nil, nil
			}
			return []string{oldPath}, nil
		}
		return nil, httpError{status: http.StatusNotFound, detail: "Activation receipt not found"}
	})
	if err != nil {
		s.cleanupUncommittedPrepaidReplacement(request.Context(), purchaseID, user.Email, payload.StoragePath, payload.ExpectedStoragePath)
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, prepaidImageMutationResponse(purchaseID, "", receiptID, prepaidImageActivation, payload.StoragePath, record, oldPath))
}

func (s *apiServer) deletePrepaidCardImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string, imageType prepaidImageType) {
	defer request.Body.Close()
	payload, err := decodePrepaidImageMutationRequest(request)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	field, ok := prepaidCardImageField(imageType)
	if !ok {
		writeJSONError(writer, http.StatusNotFound, "Card image not found")
		return
	}
	oldPath := ""
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		for _, rawCard := range prepaidRawEntries(data["cards"]) {
			card, ok := rawCard.(map[string]interface{})
			if !ok || strings.TrimSpace(stringFromAny(card["id"])) != cardID {
				continue
			}
			oldPath = strings.TrimSpace(stringFromAny(card[field]))
			if oldPath == "" {
				return nil, httpError{status: http.StatusNotFound, detail: "Card image not found"}
			}
			if err := validatePrepaidExpectedPath(oldPath, payload.ExpectedStoragePath); err != nil {
				return nil, err
			}
			card[field] = ""
			return []string{oldPath}, nil
		}
		return nil, httpError{status: http.StatusNotFound, detail: "Card not found"}
	})
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, prepaidImageMutationResponse(purchaseID, cardID, "", imageType, "", record, oldPath))
}

func (s *apiServer) replacePrepaidCardImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string, imageType prepaidImageType) {
	defer request.Body.Close()
	payload, err := decodePrepaidImageMutationRequest(request)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if payload.StoragePath == "" {
		writeJSONError(writer, http.StatusBadRequest, "storage_path is required")
		return
	}
	if err := s.ensurePrepaidUploadedImage(request.Context(), user.Email, payload.StoragePath); err != nil {
		s.writeErr(writer, err)
		return
	}
	field, ok := prepaidCardImageField(imageType)
	if !ok {
		writeJSONError(writer, http.StatusNotFound, "Card image not found")
		return
	}
	oldPath := ""
	record, err := s.mutateOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email, func(data map[string]interface{}) ([]string, error) {
		for _, rawCard := range prepaidRawEntries(data["cards"]) {
			card, ok := rawCard.(map[string]interface{})
			if !ok || strings.TrimSpace(stringFromAny(card["id"])) != cardID {
				continue
			}
			oldPath = strings.TrimSpace(stringFromAny(card[field]))
			if err := validatePrepaidExpectedPath(oldPath, payload.ExpectedStoragePath); err != nil {
				return nil, err
			}
			card[field] = payload.StoragePath
			if oldPath == "" || oldPath == payload.StoragePath {
				return nil, nil
			}
			return []string{oldPath}, nil
		}
		return nil, httpError{status: http.StatusNotFound, detail: "Card not found"}
	})
	if err != nil {
		s.cleanupUncommittedPrepaidReplacement(request.Context(), purchaseID, user.Email, payload.StoragePath, payload.ExpectedStoragePath)
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, prepaidImageMutationResponse(purchaseID, cardID, "", imageType, payload.StoragePath, record, oldPath))
}

func validatePrepaidExpectedPath(currentPath string, expectedPath string) error {
	expectedPath = strings.TrimSpace(expectedPath)
	if expectedPath != "" && expectedPath != strings.TrimSpace(currentPath) {
		return httpError{status: http.StatusConflict, detail: "The image changed; reload the purchase and retry"}
	}
	return nil
}

func prepaidImageMutationResponse(purchaseID string, cardID string, receiptID string, imageType prepaidImageType, storagePath string, record prepaidPurchaseRecord, oldPath string) map[string]interface{} {
	response := map[string]interface{}{
		"purchase_id":  purchaseID,
		"image_type":   string(imageType),
		"storage_path": storagePath,
	}
	if cardID != "" {
		response["card_id"] = cardID
	}
	if receiptID != "" {
		response["activation_receipt_id"] = receiptID
	}
	if oldPath != "" {
		response["old_storage_path"] = oldPath
	}
	if len(record.PendingImageCleanup) > 0 {
		response[prepaidPendingCleanupField] = record.PendingImageCleanup
	}
	return response
}

func (s *apiServer) cleanupUncommittedPrepaidReplacement(ctx context.Context, purchaseID string, ownerEmail string, storagePath string, expectedPath string) {
	if strings.TrimSpace(storagePath) == "" || strings.TrimSpace(storagePath) == strings.TrimSpace(expectedPath) {
		return
	}
	if s.firestore != nil {
		if snapshot, err := s.getOwnedPrepaidPurchase(ctx, purchaseID, ownerEmail); err == nil {
			if _, referenced := prepaidCurrentImagePaths(snapshot.Data())[storagePath]; referenced {
				return
			}
		}
	}
	if err := s.deletePrepaidImage(ctx, ownerEmail, storagePath); err != nil {
		log.Printf("prepaid replacement cleanup failed purchase_id=%s storage_path=%s err=%v", purchaseID, storagePath, err)
	}
}
