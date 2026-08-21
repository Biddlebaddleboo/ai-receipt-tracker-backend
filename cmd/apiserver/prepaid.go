package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	gcs "cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
)

const prepaidPurchasesCollection = "prepaid_purchases"

var (
	prepaidDigits30 = regexp.MustCompile(`^\d{30}$`)
	prepaidDigits16 = regexp.MustCompile(`^\d{16}$`)
	prepaidDigits11 = regexp.MustCompile(`^\d{11}$`)
	prepaidCVV      = regexp.MustCompile(`^\d{3,4}$`)
	prepaidExpiryA  = regexp.MustCompile(`^(0[1-9]|1[0-2])/[0-9]{2}$`)
	prepaidExpiryB  = regexp.MustCompile(`^20[0-9]{2}-(0[1-9]|1[0-2])$`)
)

type prepaidImageType string

const (
	prepaidImageActivation prepaidImageType = "activation_receipt"
	prepaidImagePackage    prepaidImageType = "package"
	prepaidImageOpenedCard prepaidImageType = "opened_card"
)

type prepaidPurchaseRecord struct {
	ID                 string                     `json:"id"`
	OwnerEmail         string                     `json:"owner_email"`
	SalesReceiptID     string                     `json:"sales_receipt_id"`
	ActivationReceipts []prepaidActivationReceipt `json:"activation_receipts"`
	Cards              []prepaidCardRecord        `json:"cards"`
	ActiveCardCount    int                        `json:"active_card_count"`
	ArchivedCardCount  int                        `json:"archived_card_count"`
	CreatedAt          string                     `json:"created_at,omitempty"`
	UpdatedAt          string                     `json:"updated_at,omitempty"`
}

type prepaidActivationReceipt struct {
	ID          string `json:"id"`
	StoragePath string `json:"storage_path"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type prepaidCardRecord struct {
	ID                         string   `json:"id"`
	ActivationBarcode          string   `json:"activation_barcode"`
	VanillaSerial              string   `json:"vanilla_serial"`
	Denomination               *float64 `json:"denomination,omitempty"`
	PAN                        string   `json:"pan,omitempty"`
	Expiry                     string   `json:"expiry,omitempty"`
	CVV                        string   `json:"cvv,omitempty"`
	Last4                      string   `json:"last4,omitempty"`
	DetailsCaptured            bool     `json:"details_captured"`
	State                      string   `json:"state"`
	ArchivedAt                 string   `json:"archived_at,omitempty"`
	PackageImageStoragePath    string   `json:"package_image_storage_path,omitempty"`
	OpenedCardImageStoragePath string   `json:"opened_card_image_storage_path,omitempty"`
	ExtractionStatus           string   `json:"extraction_status,omitempty"`
	CreatedAt                  string   `json:"created_at,omitempty"`
	UpdatedAt                  string   `json:"updated_at,omitempty"`
}

type prepaidSignedUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ImageType   string `json:"image_type"`
}

type prepaidCreatePurchaseRequest struct {
	SalesReceiptID     string                          `json:"sales_receipt_id"`
	ActivationReceipts []prepaidActivationReceiptInput `json:"activation_receipts"`
	Cards              []prepaidCardInput              `json:"cards"`
}

type prepaidActivationReceiptInput struct {
	StoragePath string `json:"storage_path"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

type prepaidCardInput struct {
	ActivationBarcode          string   `json:"activation_barcode"`
	VanillaSerial              string   `json:"vanilla_serial"`
	SerialNumber               string   `json:"serial_number"`
	Denomination               *float64 `json:"denomination"`
	PAN                        string   `json:"pan"`
	Expiry                     string   `json:"expiry"`
	CVV                        string   `json:"cvv"`
	PackageImageStoragePath    string   `json:"package_image_storage_path"`
	OpenedCardImageStoragePath string   `json:"opened_card_image_storage_path"`
	Confirmed                  bool     `json:"confirmed"`
}

type prepaidExtractRequest struct {
	StoragePath string `json:"storage_path"`
}

type prepaidSearchRequest struct {
	Value string `json:"value"`
}

type prepaidSearchResult struct {
	PurchaseID             string   `json:"purchase_id"`
	CardID                 string   `json:"card_id"`
	Last4                  string   `json:"last4"`
	Denomination           *float64 `json:"denomination,omitempty"`
	State                  string   `json:"state"`
	ActivationBarcode      string   `json:"activation_barcode"`
	VanillaSerial          string   `json:"vanilla_serial"`
	SalesReceiptID         string   `json:"sales_receipt_id"`
	ActivationReceiptCount int      `json:"activation_receipt_count"`
	DetailsCaptured        bool     `json:"details_captured"`
	CreatedAt              string   `json:"created_at,omitempty"`
	UpdatedAt              string   `json:"updated_at,omitempty"`
}

type prepaidCleanupSummary struct {
	PackageImagesDeleted           int `json:"package_images_deleted"`
	OpenedCardImagesDeleted        int `json:"opened_card_images_deleted"`
	ActivationReceiptImagesDeleted int `json:"activation_receipt_images_deleted"`
	SalesReceiptsPreserved         int `json:"sales_receipts_preserved"`
	ImageDeletionFailures          int `json:"image_deletion_failures"`
}

type prepaidPackageExtraction struct {
	ActivationBarcode string   `json:"activation_barcode"`
	SerialNumber      string   `json:"serial_number"`
	Denomination      *float64 `json:"denomination,omitempty"`
}

type prepaidOpenedCardExtraction struct {
	PAN    string `json:"pan"`
	Expiry string `json:"expiry"`
	CVV    string `json:"cvv"`
}

func (s *apiServer) handlePrepaid(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")

	user, _, _, ok := s.authorizePrepaidUser(writer, request)
	if !ok {
		return
	}

	path := strings.TrimPrefix(request.URL.Path, "/prepaid")
	if path == "" {
		path = "/"
	}
	path = strings.Trim(path, "/")

	switch {
	case path == "status" && request.Method == http.MethodGet:
		writeJSON(writer, http.StatusOK, map[string]interface{}{"enabled": true})
	case path == "images/signed-upload" && request.Method == http.MethodPost:
		s.createPrepaidSignedUpload(writer, request, user)
	case path == "package-extract" && request.Method == http.MethodPost:
		s.extractPrepaidPackageImage(writer, request, user)
	case path == "opened-card-extract" && request.Method == http.MethodPost:
		s.extractPrepaidOpenedCardImage(writer, request, user)
	case path == "search" && request.Method == http.MethodPost:
		s.searchPrepaidCards(writer, request, user)
	case path == "cleanup-archived-images" && request.Method == http.MethodPost:
		s.cleanupArchivedPrepaidImages(writer, request, user)
	case path == "purchases" && request.Method == http.MethodGet:
		s.listPrepaidPurchases(writer, request, user)
	case path == "purchases" && request.Method == http.MethodPost:
		s.createPrepaidPurchase(writer, request, user)
	case strings.HasPrefix(path, "purchases/"):
		s.handlePrepaidPurchasePath(writer, request, user, strings.TrimPrefix(path, "purchases/"))
	default:
		writeJSONError(writer, http.StatusNotFound, "Not found")
	}
}

func (s *apiServer) authorizePrepaidUser(writer http.ResponseWriter, request *http.Request) (*verifiedUser, *fs.DocumentRef, map[string]interface{}, bool) {
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return nil, nil, nil, false
	}
	userRef, userDoc := s.findOrChooseUserDoc(user.Email)
	if userRef == nil || userDoc == nil {
		writeJSONError(writer, http.StatusForbidden, "Prepaid tracker is not enabled")
		return nil, nil, nil, false
	}
	if !prepaidTrackerEnabled(userDoc) {
		writeJSONError(writer, http.StatusForbidden, "Prepaid tracker is not enabled")
		return nil, nil, nil, false
	}
	return user, userRef, userDoc, true
}

func (s *apiServer) handlePrepaidPurchasePath(writer http.ResponseWriter, request *http.Request, user *verifiedUser, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	purchaseID := strings.TrimSpace(parts[0])
	switch {
	case len(parts) == 1 && request.Method == http.MethodGet:
		s.getPrepaidPurchase(writer, request, user, purchaseID)
	case len(parts) == 2 && parts[1] == "activation-receipts" && request.Method == http.MethodPost:
		s.addPrepaidActivationReceipt(writer, request, user, purchaseID)
	case len(parts) == 2 && parts[1] == "cards" && request.Method == http.MethodPost:
		s.addPrepaidCard(writer, request, user, purchaseID)
	case len(parts) == 3 && parts[1] == "cards" && request.Method == http.MethodGet:
		s.getPrepaidCardDetail(writer, request, user, purchaseID, strings.TrimSpace(parts[2]))
	case len(parts) == 3 && parts[1] == "cards" && request.Method == http.MethodPatch:
		s.updatePrepaidCard(writer, request, user, purchaseID, strings.TrimSpace(parts[2]))
	case len(parts) == 4 && parts[1] == "activation-receipts" && parts[3] == "image" && request.Method == http.MethodGet:
		s.signPrepaidActivationReceiptImage(writer, request, user, purchaseID, strings.TrimSpace(parts[2]))
	case len(parts) == 4 && parts[1] == "cards" && parts[3] == "package-image" && request.Method == http.MethodGet:
		s.signPrepaidCardImage(writer, request, user, purchaseID, strings.TrimSpace(parts[2]), prepaidImagePackage)
	case len(parts) == 4 && parts[1] == "cards" && parts[3] == "opened-card-image" && request.Method == http.MethodGet:
		s.signPrepaidCardImage(writer, request, user, purchaseID, strings.TrimSpace(parts[2]), prepaidImageOpenedCard)
	case len(parts) == 4 && parts[1] == "cards" && parts[3] == "archive" && request.Method == http.MethodPost:
		s.archivePrepaidCard(writer, request, user, purchaseID, strings.TrimSpace(parts[2]))
	default:
		writeJSONError(writer, http.StatusNotFound, "Not found")
	}
}

func (s *apiServer) createPrepaidSignedUpload(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload prepaidSignedUploadRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	imageType := prepaidImageType(strings.TrimSpace(payload.ImageType))
	if !validPrepaidImageType(imageType) {
		writeJSONError(writer, http.StatusBadRequest, "image_type must be activation_receipt, package, or opened_card")
		return
	}
	contentType := strings.TrimSpace(payload.ContentType)
	if err := ensureImage(contentType); err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := buildPrepaidStorageKeyForOwner(user.Email, string(imageType), payload.Filename)
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	postPolicy, err := s.bucket.GenerateSignedPostPolicyV4(storagePath, &gcs.PostPolicyV4Options{
		Expires: expiresAt,
		Fields:  &gcs.PolicyV4Fields{ContentType: contentType},
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

func (s *apiServer) listPrepaidPurchases(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	state := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("state")))
	if state == "" {
		state = "active"
	}
	if state != "active" && state != "archived" && state != "all" {
		writeJSONError(writer, http.StatusBadRequest, "state must be active, archived, or all")
		return
	}
	if prepaidListPurchasesOverride != nil {
		purchases, err := prepaidListPurchasesOverride(s, request.Context(), user.Email, state)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		for index := range purchases {
			purchases[index].Cards = filterPrepaidCards(purchases[index].Cards, state)
			purchases[index].Cards = redactPrepaidCards(purchases[index].Cards)
		}
		writeJSON(writer, http.StatusOK, map[string]interface{}{
			"purchases": purchases,
			"count":     len(purchases),
		})
		return
	}
	iter := s.firestore.Collection(prepaidPurchasesCollection).
		Where("owner_email", "==", strings.TrimSpace(user.Email)).
		Documents(request.Context())
	defer iter.Stop()

	purchases := make([]prepaidPurchaseRecord, 0)
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record := prepaidPurchaseFromSnapshot(snapshot)
		record.Cards = filterPrepaidCards(record.Cards, state)
		record.Cards = redactPrepaidCards(record.Cards)
		if state != "all" && len(record.Cards) == 0 {
			continue
		}
		purchases = append(purchases, record)
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"purchases": purchases,
		"count":     len(purchases),
	})
}

func (s *apiServer) searchPrepaidCards(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload prepaidSearchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	searchValue := digitsOnly(payload.Value)
	if len(searchValue) != 4 && len(searchValue) != 16 {
		writeJSONError(writer, http.StatusBadRequest, "value must contain exactly 4 or 16 digits")
		return
	}

	purchases, err := s.prepaidPurchasesForSearch(request.Context(), user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	results := make([]prepaidSearchResult, 0)
	for _, purchase := range purchases {
		if strings.TrimSpace(purchase.OwnerEmail) != strings.TrimSpace(user.Email) {
			continue
		}
		for _, card := range purchase.Cards {
			cardLast4 := prepaidCardLast4(card)
			matches := len(searchValue) == 16 && digitsOnly(card.PAN) == searchValue
			if len(searchValue) == 4 {
				matches = cardLast4 == searchValue
			}
			if !matches {
				continue
			}
			results = append(results, prepaidSearchResult{
				PurchaseID:             purchase.ID,
				CardID:                 card.ID,
				Last4:                  cardLast4,
				Denomination:           card.Denomination,
				State:                  fallbackString(card.State, "active"),
				ActivationBarcode:      card.ActivationBarcode,
				VanillaSerial:          card.VanillaSerial,
				SalesReceiptID:         purchase.SalesReceiptID,
				ActivationReceiptCount: len(purchase.ActivationReceipts),
				DetailsCaptured:        prepaidDetailsCaptured(card.PAN, card.Expiry, card.CVV),
				CreatedAt:              card.CreatedAt,
				UpdatedAt:              card.UpdatedAt,
			})
		}
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"results": results,
		"count":   len(results),
	})
}

func (s *apiServer) cleanupArchivedPrepaidImages(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	summary := prepaidCleanupSummary{}

	if prepaidCleanupPurchasesOverride != nil {
		purchases, err := prepaidCleanupPurchasesOverride(s, request.Context(), user.Email)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		for _, purchase := range purchases {
			if !prepaidPurchaseBelongsToOwner(map[string]interface{}{"owner_email": purchase.OwnerEmail}, user.Email) {
				continue
			}
			updated, changed, err := s.cleanupArchivedPrepaidPurchase(request.Context(), purchase, &summary)
			if err != nil {
				s.writeErr(writer, err)
				return
			}
			if changed && prepaidCleanupSavePurchaseOverride != nil {
				if err := prepaidCleanupSavePurchaseOverride(s, request.Context(), updated); err != nil {
					s.writeErr(writer, err)
					return
				}
			}
		}
		writeJSON(writer, http.StatusOK, summary)
		return
	}

	iter := s.firestore.Collection(prepaidPurchasesCollection).
		Where("owner_email", "==", strings.TrimSpace(user.Email)).
		Documents(request.Context())
	defer iter.Stop()
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		purchase := prepaidPurchaseFromSnapshot(snapshot)
		updated, changed, err := s.cleanupArchivedPrepaidPurchase(request.Context(), purchase, &summary)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		if changed {
			if err := s.savePrepaidCleanupSnapshot(request.Context(), snapshot, updated); err != nil {
				s.writeErr(writer, err)
				return
			}
		}
	}

	writeJSON(writer, http.StatusOK, summary)
}

func (s *apiServer) cleanupArchivedPrepaidPurchase(ctx context.Context, purchase prepaidPurchaseRecord, summary *prepaidCleanupSummary) (prepaidPurchaseRecord, bool, error) {
	updated := purchase
	updated.Cards = append([]prepaidCardRecord(nil), purchase.Cards...)
	updated.ActivationReceipts = append([]prepaidActivationReceipt(nil), purchase.ActivationReceipts...)
	changed := false

	if strings.TrimSpace(purchase.SalesReceiptID) != "" {
		summary.SalesReceiptsPreserved++
	}

	for index := range updated.Cards {
		if strings.ToLower(strings.TrimSpace(updated.Cards[index].State)) != "archived" {
			continue
		}
		card := &updated.Cards[index]
		if path := strings.TrimSpace(card.PackageImageStoragePath); path != "" {
			if err := s.deletePrepaidImage(ctx, purchase.OwnerEmail, path); err != nil {
				summary.ImageDeletionFailures++
			} else {
				card.PackageImageStoragePath = ""
				summary.PackageImagesDeleted++
				changed = true
			}
		}
		if path := strings.TrimSpace(card.OpenedCardImageStoragePath); path != "" {
			if err := s.deletePrepaidImage(ctx, purchase.OwnerEmail, path); err != nil {
				summary.ImageDeletionFailures++
			} else {
				card.OpenedCardImageStoragePath = ""
				summary.OpenedCardImagesDeleted++
				changed = true
			}
		}
	}

	allCardsArchived := len(updated.Cards) > 0
	for _, card := range updated.Cards {
		if strings.ToLower(strings.TrimSpace(card.State)) != "archived" {
			allCardsArchived = false
			break
		}
	}
	if allCardsArchived {
		for index := range updated.ActivationReceipts {
			receipt := &updated.ActivationReceipts[index]
			if path := strings.TrimSpace(receipt.StoragePath); path != "" {
				if err := s.deletePrepaidImage(ctx, purchase.OwnerEmail, path); err != nil {
					summary.ImageDeletionFailures++
				} else {
					receipt.StoragePath = ""
					summary.ActivationReceiptImagesDeleted++
					changed = true
				}
			}
		}
	}

	return updated, changed, nil
}

func (s *apiServer) deletePrepaidImage(ctx context.Context, ownerEmail string, storagePath string) error {
	storagePath = strings.TrimSpace(storagePath)
	if storagePath == "" {
		return nil
	}
	if !prepaidStoragePathBelongsToOwner(ownerEmail, storagePath) {
		return httpError{status: http.StatusForbidden, detail: "storage_path does not belong to the authenticated user"}
	}
	if prepaidDeleteObjectOverride != nil {
		err := prepaidDeleteObjectOverride(s, ctx, storagePath)
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil
		}
		return err
	}
	if s.bucket == nil {
		return fmt.Errorf("prepaid storage bucket is unavailable")
	}
	err := s.bucket.Object(storagePath).Delete(ctx)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return nil
	}
	return err
}

func (s *apiServer) savePrepaidCleanupSnapshot(ctx context.Context, snapshot *fs.DocumentSnapshot, updated prepaidPurchaseRecord) error {
	data := snapshot.Data()
	cards, ok := data["cards"].([]interface{})
	if !ok {
		return fmt.Errorf("prepaid purchase cards have an invalid format")
	}
	for _, rawCard := range cards {
		cardData, ok := rawCard.(map[string]interface{})
		if !ok {
			continue
		}
		cardID := strings.TrimSpace(stringFromAny(cardData["id"]))
		for _, updatedCard := range updated.Cards {
			if strings.TrimSpace(updatedCard.ID) != cardID {
				continue
			}
			cardData["package_image_storage_path"] = updatedCard.PackageImageStoragePath
			cardData["opened_card_image_storage_path"] = updatedCard.OpenedCardImageStoragePath
			break
		}
	}
	if receipts, ok := data["activation_receipts"].([]interface{}); ok {
		for _, rawReceipt := range receipts {
			receiptData, ok := rawReceipt.(map[string]interface{})
			if !ok {
				continue
			}
			receiptID := strings.TrimSpace(stringFromAny(receiptData["id"]))
			for _, updatedReceipt := range updated.ActivationReceipts {
				if strings.TrimSpace(updatedReceipt.ID) != receiptID {
					continue
				}
				receiptData["storage_path"] = updatedReceipt.StoragePath
				break
			}
		}
	}
	_, err := snapshot.Ref.Set(ctx, map[string]interface{}{
		"cards":               cards,
		"activation_receipts": data["activation_receipts"],
		"updated_at":          time.Now().UTC(),
	}, fs.MergeAll)
	return err
}

func (s *apiServer) prepaidPurchasesForSearch(ctx context.Context, ownerEmail string) ([]prepaidPurchaseRecord, error) {
	if prepaidSearchPurchasesOverride != nil {
		return prepaidSearchPurchasesOverride(s, ctx, ownerEmail)
	}
	if prepaidListPurchasesOverride != nil {
		return prepaidListPurchasesOverride(s, ctx, ownerEmail, "all")
	}
	iter := s.firestore.Collection(prepaidPurchasesCollection).
		Where("owner_email", "==", strings.TrimSpace(ownerEmail)).
		Documents(ctx)
	defer iter.Stop()

	purchases := make([]prepaidPurchaseRecord, 0)
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			return purchases, nil
		}
		if err != nil {
			return nil, err
		}
		purchases = append(purchases, prepaidPurchaseFromSnapshot(snapshot))
	}
}

func (s *apiServer) getPrepaidPurchase(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string) {
	if prepaidGetPurchaseOverride != nil {
		record, err := prepaidGetPurchaseOverride(s, request.Context(), purchaseID, user.Email)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusOK, record)
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(snapshot)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) createPrepaidPurchase(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload prepaidCreatePurchaseRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	salesReceiptID := strings.TrimSpace(payload.SalesReceiptID)
	if salesReceiptID == "" {
		writeJSONError(writer, http.StatusBadRequest, "sales_receipt_id is required")
		return
	}
	if prepaidCreatePurchaseOverride != nil {
		record, err := prepaidCreatePurchaseOverride(s, request.Context(), user, payload)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusCreated, record)
		return
	}
	if _, err := s.getOwnedReceipt(request.Context(), salesReceiptID, user.Email); err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	activationReceipts, err := s.normalizePrepaidActivationInputs(request.Context(), user.Email, payload.ActivationReceipts, now)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	cards, err := s.normalizePrepaidCardInputs(request.Context(), user.Email, payload.Cards, now)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	docRef := s.firestore.Collection(prepaidPurchasesCollection).NewDoc()
	doc := map[string]interface{}{
		"owner_email":         strings.TrimSpace(user.Email),
		"sales_receipt_id":    salesReceiptID,
		"activation_receipts": activationReceipts,
		"cards":               cards,
		"active_card_count":   countCardsByState(cards, "active"),
		"archived_card_count": countCardsByState(cards, "archived"),
		"created_at":          now,
		"updated_at":          now,
	}
	if _, err := docRef.Set(request.Context(), doc); err != nil {
		s.writeErr(writer, err)
		return
	}
	snapshot, err := docRef.Get(request.Context())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(snapshot)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusCreated, record)
}

func (s *apiServer) addPrepaidActivationReceipt(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string) {
	defer request.Body.Close()
	var payload prepaidActivationReceiptInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if prepaidAddActivationReceiptOverride != nil {
		record, err := prepaidAddActivationReceiptOverride(s, request.Context(), user, purchaseID, payload)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusCreated, record)
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	entries, err := s.normalizePrepaidActivationInputs(request.Context(), user.Email, []prepaidActivationReceiptInput{payload}, now)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	existing, _ := snapshot.Data()["activation_receipts"].([]interface{})
	next := append(existing, entries[0])
	if _, err := snapshot.Ref.Set(request.Context(), map[string]interface{}{
		"activation_receipts": next,
		"updated_at":          now,
	}, fs.MergeAll); err != nil {
		s.writeErr(writer, err)
		return
	}
	updated, err := snapshot.Ref.Get(request.Context())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(updated)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusCreated, record)
}

func (s *apiServer) signPrepaidActivationReceiptImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, activationReceiptID string) {
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	receipts := prepaidActivationReceiptsFromAny(snapshot.Data()["activation_receipts"])
	storagePath := ""
	for _, receipt := range receipts {
		if strings.TrimSpace(receipt.ID) == activationReceiptID {
			storagePath = strings.TrimSpace(receipt.StoragePath)
			break
		}
	}
	if storagePath == "" || !prepaidStoragePathBelongsToOwner(user.Email, storagePath) {
		writeJSONError(writer, http.StatusNotFound, "Activation receipt not found")
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), storagePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"activation_receipt_id": activationReceiptID,
		"image_url":             imageURL,
		"expires_at":            time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

func (s *apiServer) signPrepaidCardImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string, imageType prepaidImageType) {
	record, err := s.prepaidPurchaseRecordForImage(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	storagePath := ""
	for _, card := range record.Cards {
		if strings.TrimSpace(card.ID) != cardID {
			continue
		}
		switch imageType {
		case prepaidImagePackage:
			storagePath = strings.TrimSpace(card.PackageImageStoragePath)
		case prepaidImageOpenedCard:
			storagePath = strings.TrimSpace(card.OpenedCardImageStoragePath)
		}
		break
	}
	if storagePath == "" || !prepaidStoragePathBelongsToOwner(user.Email, storagePath) {
		writeJSONError(writer, http.StatusNotFound, "Card image not found")
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), storagePath)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"purchase_id": purchaseID,
		"card_id":     cardID,
		"image_type":  string(imageType),
		"image_url":   imageURL,
		"expires_at":  time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

func (s *apiServer) prepaidPurchaseRecordForImage(ctx context.Context, purchaseID string, ownerEmail string) (prepaidPurchaseRecord, error) {
	if prepaidGetPurchaseOverride != nil {
		return prepaidGetPurchaseOverride(s, ctx, purchaseID, ownerEmail)
	}
	snapshot, err := s.getOwnedPrepaidPurchase(ctx, purchaseID, ownerEmail)
	if err != nil {
		return prepaidPurchaseRecord{}, err
	}
	return prepaidPurchaseFromSnapshot(snapshot), nil
}

func (s *apiServer) addPrepaidCard(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string) {
	defer request.Body.Close()
	var payload prepaidCardInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if prepaidAddCardOverride != nil {
		record, err := prepaidAddCardOverride(s, request.Context(), user, purchaseID, payload)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusCreated, record)
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	entries, err := s.normalizePrepaidCardInputs(request.Context(), user.Email, []prepaidCardInput{payload}, now)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	existing, _ := snapshot.Data()["cards"].([]interface{})
	next := append(existing, entries[0])
	if _, err := snapshot.Ref.Set(request.Context(), map[string]interface{}{
		"cards":               next,
		"active_card_count":   countCardsByState(next, "active"),
		"archived_card_count": countCardsByState(next, "archived"),
		"updated_at":          now,
	}, fs.MergeAll); err != nil {
		s.writeErr(writer, err)
		return
	}
	updated, err := snapshot.Ref.Get(request.Context())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(updated)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusCreated, record)
}

func (s *apiServer) getPrepaidCardDetail(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string) {
	if prepaidGetCardDetailOverride != nil {
		card, err := prepaidGetCardDetailOverride(s, request.Context(), user, purchaseID, cardID)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]interface{}{
			"purchase_id": purchaseID,
			"card":        card,
		})
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	cards := prepaidCardsFromAny(snapshot.Data()["cards"])
	for _, card := range cards {
		if strings.TrimSpace(card.ID) == cardID {
			writeJSON(writer, http.StatusOK, map[string]interface{}{
				"purchase_id": purchaseID,
				"card":        card,
			})
			return
		}
	}
	writeJSONError(writer, http.StatusNotFound, "Card not found")
}

func (s *apiServer) updatePrepaidCard(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string) {
	defer request.Body.Close()
	var payload prepaidCardInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if prepaidUpdateCardOverride != nil {
		record, err := prepaidUpdateCardOverride(s, request.Context(), user, purchaseID, cardID, payload)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusOK, record)
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	data := snapshot.Data()
	cards, _ := data["cards"].([]interface{})
	found := false
	for index, rawCard := range cards {
		card, ok := rawCard.(map[string]interface{})
		if !ok || strings.TrimSpace(stringFromAny(card["id"])) != cardID {
			continue
		}
		found = true
		merged := map[string]interface{}{}
		for key, value := range card {
			merged[key] = value
		}
		update, err := s.normalizePrepaidCardUpdate(request.Context(), user.Email, payload, now, card)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		for key, value := range update {
			merged[key] = value
		}
		cards[index] = merged
		break
	}
	if !found {
		writeJSONError(writer, http.StatusNotFound, "Card not found")
		return
	}
	if _, err := snapshot.Ref.Set(request.Context(), map[string]interface{}{
		"cards":               cards,
		"active_card_count":   countCardsByState(cards, "active"),
		"archived_card_count": countCardsByState(cards, "archived"),
		"updated_at":          now,
	}, fs.MergeAll); err != nil {
		s.writeErr(writer, err)
		return
	}
	updated, err := snapshot.Ref.Get(request.Context())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(updated)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) archivePrepaidCard(writer http.ResponseWriter, request *http.Request, user *verifiedUser, purchaseID string, cardID string) {
	if prepaidArchiveCardOverride != nil {
		record, err := prepaidArchiveCardOverride(s, request.Context(), user, purchaseID, cardID)
		if err != nil {
			s.writeErr(writer, err)
			return
		}
		record.Cards = redactPrepaidCards(record.Cards)
		writeJSON(writer, http.StatusOK, record)
		return
	}
	snapshot, err := s.getOwnedPrepaidPurchase(request.Context(), purchaseID, user.Email)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	now := time.Now().UTC()
	cards, _ := snapshot.Data()["cards"].([]interface{})
	found := false
	for index, rawCard := range cards {
		card, ok := rawCard.(map[string]interface{})
		if !ok || strings.TrimSpace(stringFromAny(card["id"])) != cardID {
			continue
		}
		found = true
		card["state"] = "archived"
		card["archived_at"] = now
		card["updated_at"] = now
		cards[index] = card
		break
	}
	if !found {
		writeJSONError(writer, http.StatusNotFound, "Card not found")
		return
	}
	if _, err := snapshot.Ref.Set(request.Context(), map[string]interface{}{
		"cards":               cards,
		"active_card_count":   countCardsByState(cards, "active"),
		"archived_card_count": countCardsByState(cards, "archived"),
		"updated_at":          now,
	}, fs.MergeAll); err != nil {
		s.writeErr(writer, err)
		return
	}
	updated, err := snapshot.Ref.Get(request.Context())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	record := prepaidPurchaseFromSnapshot(updated)
	record.Cards = redactPrepaidCards(record.Cards)
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) extractPrepaidPackageImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload prepaidExtractRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.ensurePrepaidUploadedImage(request.Context(), user.Email, payload.StoragePath); err != nil {
		s.writeErr(writer, err)
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), strings.TrimSpace(payload.StoragePath))
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	result, err := s.runPrepaidPackageExtraction(request.Context(), imageURL)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	warnings := validatePrepaidPackageExtraction(result)
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"extraction":            result,
		"warnings":              warnings,
		"requires_confirmation": true,
	})
}

func (s *apiServer) extractPrepaidOpenedCardImage(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	defer request.Body.Close()
	var payload prepaidExtractRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.ensurePrepaidUploadedImage(request.Context(), user.Email, payload.StoragePath); err != nil {
		s.writeErr(writer, err)
		return
	}
	imageURL, err := s.signedImageURL(request.Context(), strings.TrimSpace(payload.StoragePath))
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	result, err := s.runPrepaidOpenedCardExtraction(request.Context(), imageURL)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	warnings := validateOpenedCardExtraction(result)
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"extraction":            result,
		"warnings":              warnings,
		"requires_confirmation": true,
	})
}

func (s *apiServer) getOwnedPrepaidPurchase(ctx context.Context, purchaseID string, ownerEmail string) (*fs.DocumentSnapshot, error) {
	snapshot, err := s.firestore.Collection(prepaidPurchasesCollection).Doc(strings.TrimSpace(purchaseID)).Get(ctx)
	if err != nil || !snapshot.Exists() {
		return nil, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
	}
	if !prepaidPurchaseBelongsToOwner(snapshot.Data(), ownerEmail) {
		return nil, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
	}
	return snapshot, nil
}

func (s *apiServer) normalizePrepaidActivationInputs(ctx context.Context, ownerEmail string, inputs []prepaidActivationReceiptInput, now time.Time) ([]interface{}, error) {
	result := make([]interface{}, 0, len(inputs))
	for _, input := range inputs {
		storagePath := strings.TrimSpace(input.StoragePath)
		if storagePath == "" {
			return nil, httpError{status: http.StatusBadRequest, detail: "activation receipt storage_path is required"}
		}
		if err := s.ensurePrepaidUploadedImage(ctx, ownerEmail, storagePath); err != nil {
			return nil, err
		}
		result = append(result, map[string]interface{}{
			"id":           uuid.NewString(),
			"storage_path": storagePath,
			"filename":     strings.TrimSpace(input.Filename),
			"content_type": fallbackString(input.ContentType, "image/webp"),
			"created_at":   now,
		})
	}
	return result, nil
}

func (s *apiServer) normalizePrepaidCardInputs(ctx context.Context, ownerEmail string, inputs []prepaidCardInput, now time.Time) ([]interface{}, error) {
	result := make([]interface{}, 0, len(inputs))
	for _, input := range inputs {
		if !input.Confirmed {
			return nil, httpError{status: http.StatusBadRequest, detail: "card details must be confirmed before saving"}
		}
		card, err := s.normalizePrepaidCardInput(ctx, ownerEmail, input, now, true)
		if err != nil {
			return nil, err
		}
		result = append(result, card)
	}
	return result, nil
}

func (s *apiServer) normalizePrepaidCardInput(ctx context.Context, ownerEmail string, input prepaidCardInput, now time.Time, requirePackageFields bool) (map[string]interface{}, error) {
	barcode := digitsOnly(input.ActivationBarcode)
	if requirePackageFields && !prepaidDigits30.MatchString(barcode) {
		return nil, httpError{status: http.StatusBadRequest, detail: "activation_barcode must be exactly 30 digits"}
	}
	serial := digitsOnly(firstNonEmptyString(input.VanillaSerial, input.SerialNumber))
	if requirePackageFields && !prepaidDigits11.MatchString(serial) {
		return nil, httpError{status: http.StatusBadRequest, detail: "vanilla_serial must be exactly 11 digits"}
	}
	pan := digitsOnly(input.PAN)
	if pan != "" && !prepaidDigits16.MatchString(pan) {
		return nil, httpError{status: http.StatusBadRequest, detail: "pan must be exactly 16 digits"}
	}
	cvv := digitsOnly(input.CVV)
	if cvv != "" && !prepaidCVV.MatchString(cvv) {
		return nil, httpError{status: http.StatusBadRequest, detail: "cvv must be 3 or 4 digits"}
	}
	expiry := normalizePrepaidExpiry(input.Expiry)
	if strings.TrimSpace(input.Expiry) != "" && expiry == "" {
		return nil, httpError{status: http.StatusBadRequest, detail: "expiry must be MM/YY or YYYY-MM"}
	}
	packagePath := strings.TrimSpace(input.PackageImageStoragePath)
	if packagePath != "" {
		if err := s.ensurePrepaidUploadedImage(ctx, ownerEmail, packagePath); err != nil {
			return nil, err
		}
	}
	openedPath := strings.TrimSpace(input.OpenedCardImageStoragePath)
	if openedPath != "" {
		if err := s.ensurePrepaidUploadedImage(ctx, ownerEmail, openedPath); err != nil {
			return nil, err
		}
	}
	card := map[string]interface{}{
		"id":                             uuid.NewString(),
		"activation_barcode":             barcode,
		"vanilla_serial":                 serial,
		"denomination":                   input.Denomination,
		"pan":                            pan,
		"expiry":                         expiry,
		"cvv":                            cvv,
		"state":                          "active",
		"package_image_storage_path":     packagePath,
		"opened_card_image_storage_path": openedPath,
		"extraction_status":              "confirmed",
		"created_at":                     now,
		"updated_at":                     now,
	}
	return card, nil
}

func (s *apiServer) normalizePrepaidCardUpdate(ctx context.Context, ownerEmail string, input prepaidCardInput, now time.Time, existing map[string]interface{}) (map[string]interface{}, error) {
	if !input.Confirmed {
		return nil, httpError{status: http.StatusBadRequest, detail: "card details must be confirmed before saving"}
	}
	update := map[string]interface{}{}
	newPANProvided := false
	if input.Denomination != nil {
		update["denomination"] = input.Denomination
	}
	if pan := digitsOnly(input.PAN); pan != "" {
		if !prepaidDigits16.MatchString(pan) {
			return nil, httpError{status: http.StatusBadRequest, detail: "pan must be exactly 16 digits"}
		}
		newPANProvided = true
		update["pan"] = pan
		update["last4"] = last4(pan)
		update["details_captured"] = true
	}
	if expiry := normalizePrepaidExpiry(input.Expiry); strings.TrimSpace(input.Expiry) != "" {
		if expiry == "" {
			return nil, httpError{status: http.StatusBadRequest, detail: "expiry must be MM/YY or YYYY-MM"}
		}
		update["expiry"] = expiry
		update["details_captured"] = true
	}
	if cvv := digitsOnly(input.CVV); cvv != "" {
		if !prepaidCVV.MatchString(cvv) {
			return nil, httpError{status: http.StatusBadRequest, detail: "cvv must be 3 or 4 digits"}
		}
		update["cvv"] = cvv
		update["details_captured"] = true
	}
	packagePath := strings.TrimSpace(input.PackageImageStoragePath)
	if packagePath != "" {
		if !prepaidStoragePathBelongsToOwner(ownerEmail, packagePath) {
			return nil, httpError{status: http.StatusForbidden, detail: "storage_path does not belong to the authenticated user"}
		}
		update["package_image_storage_path"] = packagePath
	}
	openedPath := strings.TrimSpace(input.OpenedCardImageStoragePath)
	if openedPath != "" {
		if !prepaidStoragePathBelongsToOwner(ownerEmail, openedPath) {
			return nil, httpError{status: http.StatusForbidden, detail: "storage_path does not belong to the authenticated user"}
		}
		update["opened_card_image_storage_path"] = openedPath
	}
	if !newPANProvided {
		if pan := stringFromAny(existing["pan"]); strings.TrimSpace(pan) != "" {
			update["last4"] = last4(pan)
			update["details_captured"] = true
		}
	}
	if expiry := stringFromAny(existing["expiry"]); strings.TrimSpace(expiry) != "" {
		update["details_captured"] = true
	}
	if cvv := stringFromAny(existing["cvv"]); strings.TrimSpace(cvv) != "" {
		update["details_captured"] = true
	}
	update["updated_at"] = now
	return update, nil
}

func (s *apiServer) ensurePrepaidUploadedImage(ctx context.Context, ownerEmail string, storagePath string) error {
	storagePath = strings.TrimSpace(storagePath)
	if storagePath == "" {
		return httpError{status: http.StatusBadRequest, detail: "storage_path is required"}
	}
	if !prepaidStoragePathBelongsToOwner(ownerEmail, storagePath) {
		return httpError{status: http.StatusForbidden, detail: "storage_path does not belong to the authenticated user"}
	}
	attrs, err := s.bucket.Object(storagePath).Attrs(ctx)
	if err != nil {
		if err == gcs.ErrObjectNotExist {
			return httpError{status: http.StatusNotFound, detail: "uploaded object not found"}
		}
		return err
	}
	if attrs.Size <= 0 {
		return httpError{status: http.StatusBadRequest, detail: "uploaded object is empty"}
	}
	return ensureImage(strings.TrimSpace(attrs.ContentType))
}

func (s *apiServer) runPrepaidPackageExtraction(ctx context.Context, imageURL string) (prepaidPackageExtraction, error) {
	rawText, err := s.runPrepaidVisionPrompt(ctx, imageURL, "Extract only JSON from this prepaid Vanilla card package image with keys activation_barcode, serial_number, denomination. The activation_barcode must be the 30 digit package activation barcode. The serial_number must be the 11 digit Vanilla serial. Do not include explanation.")
	if err != nil {
		return prepaidPackageExtraction{}, err
	}
	payload := extractJSON(rawText)
	denomination := normalizeAmount(firstPresent(payload, "denomination", "amount", "value"))
	return prepaidPackageExtraction{
		ActivationBarcode: digitsOnly(firstPresentString(payload, "activation_barcode", "package_barcode", "barcode")),
		SerialNumber:      digitsOnly(firstPresentString(payload, "serial_number", "vanilla_serial", "serial")),
		Denomination:      denomination,
	}, nil
}

func (s *apiServer) runPrepaidOpenedCardExtraction(ctx context.Context, imageURL string) (prepaidOpenedCardExtraction, error) {
	rawText, err := s.runPrepaidVisionPrompt(ctx, imageURL, "Extract only JSON from this opened prepaid card image with keys pan, expiry, cvv. The pan must be the 16 digit card number. The expiry should be MM/YY if visible. Do not include explanation.")
	if err != nil {
		return prepaidOpenedCardExtraction{}, err
	}
	payload := extractJSON(rawText)
	return prepaidOpenedCardExtraction{
		PAN:    digitsOnly(firstPresentString(payload, "pan", "card_number", "number")),
		Expiry: normalizePrepaidExpiry(firstPresentString(payload, "expiry", "expiration", "expiration_date")),
		CVV:    digitsOnly(firstPresentString(payload, "cvv", "cvc", "security_code")),
	}, nil
}

func (s *apiServer) runPrepaidVisionPrompt(ctx context.Context, imageURL string, prompt string) (string, error) {
	if strings.TrimSpace(s.cfg.openAIAPIKey) == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is required")
	}
	payload := openAIResponsesRequest{
		Model: s.cfg.openAIModel,
		Input: []openAIInputMessage{
			{
				Role: "user",
				Content: []openAIInputContent{
					{Type: "input_text", Text: prompt},
					{Type: "input_image", ImageURL: imageURL, Detail: "low"},
				},
			},
		},
		Temperature: 0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.openAIAPIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var envelope openAIResponsesEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			return "", fmt.Errorf("OpenAI API error: %s", envelope.Error.Message)
		}
		return "", fmt.Errorf("OpenAI API error: status %d", resp.StatusCode)
	}
	return collectOCRText(envelope), nil
}

func prepaidPurchaseFromSnapshot(snapshot *fs.DocumentSnapshot) prepaidPurchaseRecord {
	data := snapshot.Data()
	cards := prepaidCardsFromAny(data["cards"])
	activeCount := intFromAny(data["active_card_count"])
	archivedCount := intFromAny(data["archived_card_count"])
	if activeCount == 0 && archivedCount == 0 && len(cards) > 0 {
		activeCount = len(filterPrepaidCards(cards, "active"))
		archivedCount = len(filterPrepaidCards(cards, "archived"))
	}
	return prepaidPurchaseRecord{
		ID:                 snapshot.Ref.ID,
		OwnerEmail:         stringFromAny(data["owner_email"]),
		SalesReceiptID:     stringFromAny(data["sales_receipt_id"]),
		ActivationReceipts: prepaidActivationReceiptsFromAny(data["activation_receipts"]),
		Cards:              cards,
		ActiveCardCount:    activeCount,
		ArchivedCardCount:  archivedCount,
		CreatedAt:          isoString(data["created_at"]),
		UpdatedAt:          isoString(data["updated_at"]),
	}
}

func prepaidActivationReceiptsFromAny(value interface{}) []prepaidActivationReceipt {
	raw, ok := value.([]interface{})
	if !ok {
		return []prepaidActivationReceipt{}
	}
	result := make([]prepaidActivationReceipt, 0, len(raw))
	for _, entry := range raw {
		data, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		result = append(result, prepaidActivationReceipt{
			ID:          stringFromAny(data["id"]),
			StoragePath: stringFromAny(data["storage_path"]),
			Filename:    stringFromAny(data["filename"]),
			ContentType: stringFromAny(data["content_type"]),
			CreatedAt:   isoString(data["created_at"]),
		})
	}
	return result
}

func prepaidCardsFromAny(value interface{}) []prepaidCardRecord {
	raw, ok := value.([]interface{})
	if !ok {
		return []prepaidCardRecord{}
	}
	result := make([]prepaidCardRecord, 0, len(raw))
	for _, entry := range raw {
		data, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		result = append(result, prepaidCardRecord{
			ID:                         stringFromAny(data["id"]),
			ActivationBarcode:          stringFromAny(data["activation_barcode"]),
			VanillaSerial:              stringFromAny(data["vanilla_serial"]),
			Denomination:               normalizeAmount(data["denomination"]),
			PAN:                        stringFromAny(data["pan"]),
			Expiry:                     stringFromAny(data["expiry"]),
			CVV:                        stringFromAny(data["cvv"]),
			Last4:                      last4(stringFromAny(data["pan"])),
			DetailsCaptured:            prepaidDetailsCaptured(stringFromAny(data["pan"]), stringFromAny(data["expiry"]), stringFromAny(data["cvv"])),
			State:                      fallbackString(data["state"], "active"),
			ArchivedAt:                 isoString(data["archived_at"]),
			PackageImageStoragePath:    stringFromAny(data["package_image_storage_path"]),
			OpenedCardImageStoragePath: stringFromAny(data["opened_card_image_storage_path"]),
			ExtractionStatus:           stringFromAny(data["extraction_status"]),
			CreatedAt:                  isoString(data["created_at"]),
			UpdatedAt:                  isoString(data["updated_at"]),
		})
	}
	return result
}

func redactPrepaidCards(cards []prepaidCardRecord) []prepaidCardRecord {
	result := make([]prepaidCardRecord, 0, len(cards))
	for _, card := range cards {
		card.Last4 = last4(card.PAN)
		card.DetailsCaptured = prepaidDetailsCaptured(card.PAN, card.Expiry, card.CVV)
		card.PAN = ""
		card.Expiry = ""
		card.CVV = ""
		result = append(result, card)
	}
	return result
}

func prepaidTrackerEnabled(userDoc map[string]interface{}) bool {
	if userDoc == nil {
		return false
	}
	enabled, _ := userDoc["prepaid_tracker_enabled"].(bool)
	return enabled
}

func prepaidPurchaseBelongsToOwner(data map[string]interface{}, ownerEmail string) bool {
	if data == nil {
		return false
	}
	stored := strings.TrimSpace(stringFromAny(data["owner_email"]))
	return stored != "" && stored == strings.TrimSpace(ownerEmail)
}

func prepaidDetailsCaptured(pan string, expiry string, cvv string) bool {
	return strings.TrimSpace(pan) != "" || strings.TrimSpace(expiry) != "" || strings.TrimSpace(cvv) != ""
}

func last4(value string) string {
	digits := digitsOnly(value)
	if len(digits) < 4 {
		return ""
	}
	return digits[len(digits)-4:]
}

func prepaidCardLast4(card prepaidCardRecord) string {
	if derived := last4(card.PAN); derived != "" {
		return derived
	}
	return last4(card.Last4)
}

func prepaidStoragePathBelongsToOwner(ownerEmail string, storagePath string) bool {
	return strings.HasPrefix(strings.TrimSpace(storagePath), ownerStoragePrefix(ownerEmail))
}

func filterPrepaidCards(cards []prepaidCardRecord, state string) []prepaidCardRecord {
	if state == "all" {
		return cards
	}
	result := make([]prepaidCardRecord, 0, len(cards))
	for _, card := range cards {
		cardState := strings.ToLower(strings.TrimSpace(card.State))
		if cardState == "" {
			cardState = "active"
		}
		if cardState == state {
			result = append(result, card)
		}
	}
	return result
}

func countCardsByState(cards interface{}, state string) int {
	count := 0
	raw, ok := cards.([]interface{})
	if !ok {
		return 0
	}
	for _, entry := range raw {
		data, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		cardState := strings.ToLower(strings.TrimSpace(stringFromAny(data["state"])))
		if cardState == "" {
			cardState = "active"
		}
		if cardState == state {
			count++
		}
	}
	return count
}

func validatePrepaidPackageExtraction(value prepaidPackageExtraction) []string {
	warnings := make([]string, 0)
	if !prepaidDigits30.MatchString(value.ActivationBarcode) {
		warnings = append(warnings, "activation_barcode must be exactly 30 digits")
	}
	if !prepaidDigits11.MatchString(value.SerialNumber) {
		warnings = append(warnings, "serial_number must be exactly 11 digits")
	}
	if value.Denomination == nil || *value.Denomination <= 0 {
		warnings = append(warnings, "denomination was not confidently extracted")
	}
	return warnings
}

func validateOpenedCardExtraction(value prepaidOpenedCardExtraction) []string {
	warnings := make([]string, 0)
	if !prepaidDigits16.MatchString(value.PAN) {
		warnings = append(warnings, "pan must be exactly 16 digits")
	}
	if value.Expiry == "" {
		warnings = append(warnings, "expiry must be MM/YY or YYYY-MM")
	}
	if !prepaidCVV.MatchString(value.CVV) {
		warnings = append(warnings, "cvv must be 3 or 4 digits")
	}
	return warnings
}

func validPrepaidImageType(value prepaidImageType) bool {
	return value == prepaidImageActivation || value == prepaidImagePackage || value == prepaidImageOpenedCard
}

func buildPrepaidStorageKeyForOwner(ownerEmail string, imageType string, filename string) string {
	base := filepath.Base(filename)
	if base == "." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		base = "prepaid.webp"
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Sprintf("%sprepaid/%s/%d_%s", ownerStoragePrefix(ownerEmail), imageType, time.Now().UnixNano(), base)
	}
	return fmt.Sprintf("%sprepaid/%s/%s_%s", ownerStoragePrefix(ownerEmail), imageType, hex.EncodeToString(tokenBytes), base)
}

func digitsOnly(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func normalizePrepaidExpiry(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if prepaidExpiryA.MatchString(trimmed) || prepaidExpiryB.MatchString(trimmed) {
		return trimmed
	}
	digits := digitsOnly(trimmed)
	if len(digits) == 4 {
		candidate := digits[:2] + "/" + digits[2:]
		if prepaidExpiryA.MatchString(candidate) {
			return candidate
		}
	}
	if len(digits) == 6 && strings.HasPrefix(digits, "20") {
		candidate := digits[:4] + "-" + digits[4:]
		if prepaidExpiryB.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

func firstPresentString(payload map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			return stringFromAny(value)
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isoString(value interface{}) string {
	if typed := timeValue(value); typed != nil {
		return typed.Format(time.RFC3339)
	}
	return ""
}

func intFromAny(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}
