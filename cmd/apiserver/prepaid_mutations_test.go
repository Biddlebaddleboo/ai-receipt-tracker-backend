package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
)

func installPrepaidMutationStore(t *testing.T, record *prepaidPurchaseRecord) *apiServer {
	t.Helper()
	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{"prepaid_tracker_enabled": true})
	prepaidPurchaseMutationOverride = func(_ *apiServer, _ context.Context, purchaseID string, ownerEmail string, mutate prepaidPurchaseMutator) (prepaidPurchaseRecord, error) {
		if purchaseID != record.ID || ownerEmail != record.OwnerEmail {
			return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
		}
		data := prepaidPurchaseRecordToData(*record)
		cleanupPaths, err := mutate(data)
		if err != nil {
			return prepaidPurchaseRecord{}, err
		}
		queuePrepaidImageCleanup(data, cleanupPaths)
		data["updated_at"] = time.Now().UTC()
		*record = prepaidPurchaseRecordFromData(record.ID, data)
		return *record, nil
	}
	return server
}

func setPrepaidImageDeleteResult(deleteErrors map[string]error, deletedPaths *[]string) {
	prepaidDeleteObjectOverride = func(_ *apiServer, _ context.Context, path string) error {
		*deletedPaths = append(*deletedPaths, path)
		return deleteErrors[path]
	}
}

func setPrepaidReplacementObjectAttrs() {
	prepaidObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
		return &gcs.ObjectAttrs{Size: 1, ContentType: "image/webp"}, nil
	}
}

func TestPrepaidActivationAssociationUniquenessOnAddAndUpdate(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	server := installPrepaidMutationStore(t, &record)

	add := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/purchases/purchase-1/cards", map[string]interface{}{
		"activation_barcode":    "999999999999999999999999999999",
		"vanilla_serial":        "99999999999",
		"activation_receipt_id": "activation-1",
		"confirmed":             true,
	})
	if add.Code != http.StatusBadRequest {
		t.Fatalf("expected duplicate add to be rejected, got %d: %s", add.Code, add.Body.String())
	}
	if len(record.Cards) != 1 {
		t.Fatalf("duplicate add changed cards: %+v", record.Cards)
	}

	record.Cards = append(record.Cards, prepaidCardRecord{ID: "card-2", State: "active"})
	update := performPrepaidRequest(t, server, http.MethodPatch, "/prepaid/purchases/purchase-1/cards/card-2", map[string]interface{}{
		"activation_receipt_id": "activation-1",
		"confirmed":             true,
	})
	if update.Code != http.StatusBadRequest {
		t.Fatalf("expected duplicate update to be rejected, got %d: %s", update.Code, update.Body.String())
	}

	sameCard := performPrepaidRequest(t, server, http.MethodPatch, "/prepaid/purchases/purchase-1/cards/card-1", map[string]interface{}{
		"activation_receipt_id": "activation-1",
		"confirmed":             true,
	})
	if sameCard.Code != http.StatusOK {
		t.Fatalf("expected unchanged legacy association to remain valid, got %d: %s", sameCard.Code, sameCard.Body.String())
	}
}

func TestPrepaidActivationAssociationClearThenReassign(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	record.Cards = append(record.Cards, prepaidCardRecord{ID: "card-2", State: "active"})
	server := installPrepaidMutationStore(t, &record)

	clear := performPrepaidRequest(t, server, http.MethodPatch, "/prepaid/purchases/purchase-1/cards/card-1", map[string]interface{}{
		"activation_receipt_id": nil,
		"confirmed":             true,
	})
	if clear.Code != http.StatusOK {
		t.Fatalf("clear association failed: %d: %s", clear.Code, clear.Body.String())
	}
	if record.Cards[0].ActivationReceiptID != "" {
		t.Fatalf("expected card-1 to be unlinked, got %q", record.Cards[0].ActivationReceiptID)
	}

	reassign := performPrepaidRequest(t, server, http.MethodPatch, "/prepaid/purchases/purchase-1/cards/card-2", map[string]interface{}{
		"activation_receipt_id": "activation-1",
		"confirmed":             true,
	})
	if reassign.Code != http.StatusOK {
		t.Fatalf("reassignment failed: %d: %s", reassign.Code, reassign.Body.String())
	}
	if record.Cards[1].ActivationReceiptID != "activation-1" {
		t.Fatalf("expected card-2 to receive released receipt, got %q", record.Cards[1].ActivationReceiptID)
	}
}

func TestPrepaidLegacyDuplicateAssociationSurvivesUnrelatedUpdate(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	record.Cards = append(record.Cards, prepaidCardRecord{
		ID:                  "card-2",
		ActivationReceiptID: "activation-1",
		State:               "active",
	})
	server := installPrepaidMutationStore(t, &record)

	update := performPrepaidRequest(t, server, http.MethodPatch, "/prepaid/purchases/purchase-1/cards/card-2", map[string]interface{}{
		"pan":       "9999999999999999",
		"confirmed": true,
	})
	if update.Code != http.StatusOK {
		t.Fatalf("unrelated update rejected legacy duplicate: %d: %s", update.Code, update.Body.String())
	}
	if record.Cards[0].ActivationReceiptID != "activation-1" || record.Cards[1].ActivationReceiptID != "activation-1" {
		t.Fatalf("legacy links were changed: %+v", record.Cards)
	}
}

func TestDeletePrepaidActivationReceiptClearsLinksAndPreservesSalesReceipt(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	server := installPrepaidMutationStore(t, &record)
	deleted := make([]string, 0)
	setPrepaidImageDeleteResult(nil, &deleted)

	response := performPrepaidRequest(t, server, http.MethodDelete, "/prepaid/purchases/purchase-1/activation-receipts/activation-1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete activation receipt failed: %d: %s", response.Code, response.Body.String())
	}
	if len(record.ActivationReceipts) != 0 || record.Cards[0].ActivationReceiptID != "" {
		t.Fatalf("activation receipt/link were not removed: %+v", record)
	}
	if record.SalesReceiptID != "receipt-1" {
		t.Fatalf("sales receipt changed: %q", record.SalesReceiptID)
	}
	if len(deleted) != 1 || deleted[0] != fixturePrepaidPurchase().ActivationReceipts[0].StoragePath {
		t.Fatalf("expected activation image deletion, got %v", deleted)
	}
}

func TestDeletePrepaidCardPreservesReceiptsAndRecalculatesCounts(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	record.Cards[0].CardFrontImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_front/card-1.webp"
	record.Cards[0].CardBackImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_back/card-1.webp"
	server := installPrepaidMutationStore(t, &record)
	deleted := make([]string, 0)
	setPrepaidImageDeleteResult(nil, &deleted)

	response := performPrepaidRequest(t, server, http.MethodDelete, "/prepaid/purchases/purchase-1/cards/card-1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete card failed: %d: %s", response.Code, response.Body.String())
	}
	if len(record.Cards) != 0 || record.ActiveCardCount != 0 || record.ArchivedCardCount != 0 {
		t.Fatalf("card/counts not removed: %+v", record)
	}
	if len(record.ActivationReceipts) != 1 || record.SalesReceiptID != "receipt-1" {
		t.Fatalf("receipt records were changed: %+v", record)
	}
	for _, path := range deleted {
		if strings.Contains(path, "/activation/") || strings.Contains(path, "/sales/") {
			t.Fatalf("non-card image was deleted: %q", path)
		}
	}
	if len(deleted) != 4 {
		t.Fatalf("expected all four card slots deleted, got %v", deleted)
	}
}

func TestDeletePrepaidImageSlotsIndividually(t *testing.T) {
	cases := []struct {
		name string
		path string
		get  func(prepaidCardRecord) string
	}{
		{name: "package", path: "package-image", get: func(card prepaidCardRecord) string { return card.PackageImageStoragePath }},
		{name: "front", path: "card-front-image", get: func(card prepaidCardRecord) string { return card.CardFrontImageStoragePath }},
		{name: "back", path: "card-back-image", get: func(card prepaidCardRecord) string { return card.CardBackImageStoragePath }},
		{name: "legacy opened", path: "opened-card-image", get: func(card prepaidCardRecord) string { return card.OpenedCardImageStoragePath }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := fixturePrepaidPurchase()
			record.Cards[0].CardFrontImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_front/card-1.webp"
			record.Cards[0].CardBackImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_back/card-1.webp"
			server := installPrepaidMutationStore(t, &record)
			deleted := make([]string, 0)
			setPrepaidImageDeleteResult(nil, &deleted)

			response := performPrepaidRequest(t, server, http.MethodDelete, "/prepaid/purchases/purchase-1/cards/card-1/"+tc.path, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("delete %s image failed: %d: %s", tc.name, response.Code, response.Body.String())
			}
			if len(record.Cards) != 1 || tc.get(record.Cards[0]) != "" {
				t.Fatalf("%s slot was not cleared: %+v", tc.name, record.Cards[0])
			}
			if record.Cards[0].PAN == "" || record.Cards[0].CVV == "" {
				t.Fatal("photo deletion removed extracted credentials")
			}
			if len(deleted) != 1 {
				t.Fatalf("expected one deleted object, got %v", deleted)
			}
		})
	}
}

func TestDeletePrepaidActivationPhotoOnlyPreservesReceipt(t *testing.T) {
	record := fixturePrepaidPurchase()
	record.Cards[0].ActivationReceiptID = "activation-1"
	server := installPrepaidMutationStore(t, &record)
	deleted := make([]string, 0)
	setPrepaidImageDeleteResult(nil, &deleted)

	response := performPrepaidRequest(t, server, http.MethodDelete, "/prepaid/purchases/purchase-1/activation-receipts/activation-1/image", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete activation photo failed: %d: %s", response.Code, response.Body.String())
	}
	if len(record.ActivationReceipts) != 1 || record.ActivationReceipts[0].StoragePath != "" {
		t.Fatalf("activation receipt record/photo state incorrect: %+v", record.ActivationReceipts)
	}
	if record.Cards[0].ActivationReceiptID != "activation-1" {
		t.Fatalf("activation relationship was cleared by photo delete")
	}
}

func TestReplacePrepaidImageSlotsPreservesIDsAndLinks(t *testing.T) {
	cases := []struct {
		name string
		path string
		old  func(*prepaidPurchaseRecord) string
		new  func(*prepaidPurchaseRecord, string)
	}{
		{name: "activation", path: "activation-receipts/activation-1/image", old: func(record *prepaidPurchaseRecord) string { return record.ActivationReceipts[0].StoragePath }, new: func(record *prepaidPurchaseRecord, value string) { record.ActivationReceipts[0].StoragePath = value }},
		{name: "package", path: "cards/card-1/package-image", old: func(record *prepaidPurchaseRecord) string { return record.Cards[0].PackageImageStoragePath }, new: func(record *prepaidPurchaseRecord, value string) { record.Cards[0].PackageImageStoragePath = value }},
		{name: "front", path: "cards/card-1/card-front-image", old: func(record *prepaidPurchaseRecord) string { return record.Cards[0].CardFrontImageStoragePath }, new: func(record *prepaidPurchaseRecord, value string) { record.Cards[0].CardFrontImageStoragePath = value }},
		{name: "back", path: "cards/card-1/card-back-image", old: func(record *prepaidPurchaseRecord) string { return record.Cards[0].CardBackImageStoragePath }, new: func(record *prepaidPurchaseRecord, value string) { record.Cards[0].CardBackImageStoragePath = value }},
		{name: "legacy opened", path: "cards/card-1/opened-card-image", old: func(record *prepaidPurchaseRecord) string { return record.Cards[0].OpenedCardImageStoragePath }, new: func(record *prepaidPurchaseRecord, value string) { record.Cards[0].OpenedCardImageStoragePath = value }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := fixturePrepaidPurchase()
			record.Cards[0].ActivationReceiptID = "activation-1"
			record.Cards[0].CardFrontImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_front/card-1.webp"
			record.Cards[0].CardBackImageStoragePath = ownerStoragePrefix("owner@example.com") + "prepaid/card_back/card-1.webp"
			oldPath := tc.old(&record)
			server := installPrepaidMutationStore(t, &record)
			setPrepaidReplacementObjectAttrs()
			deleted := make([]string, 0)
			setPrepaidImageDeleteResult(nil, &deleted)
			newPath := ownerStoragePrefix("owner@example.com") + "prepaid/replacement/" + tc.name + ".webp"

			response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/purchases/purchase-1/"+tc.path, map[string]string{
				"storage_path":          newPath,
				"expected_storage_path": oldPath,
			})
			if response.Code != http.StatusOK {
				t.Fatalf("replace %s image failed: %d: %s", tc.name, response.Code, response.Body.String())
			}
			if tc.name == "activation" {
				if record.ActivationReceipts[0].ID != "activation-1" || record.Cards[0].ActivationReceiptID != "activation-1" {
					t.Fatalf("activation identity/link changed: %+v", record)
				}
			} else if record.Cards[0].ID != "card-1" || record.Cards[0].ActivationReceiptID != "activation-1" {
				t.Fatalf("card identity/link changed: %+v", record.Cards[0])
			}
			if tc.old(&record) != newPath {
				t.Fatalf("replacement path not saved: %q", tc.old(&record))
			}
			if len(deleted) != 1 || deleted[0] != oldPath {
				t.Fatalf("expected old path deletion %q, got %v", oldPath, deleted)
			}
		})
	}
}

func TestPrepaidReplacementQueuesFailedOldImageAndCleanupRetries(t *testing.T) {
	record := fixturePrepaidPurchase()
	oldPath := record.Cards[0].PackageImageStoragePath
	server := installPrepaidMutationStore(t, &record)
	setPrepaidReplacementObjectAttrs()
	deleteCalls := make([]string, 0)
	failDelete := true
	prepaidDeleteObjectOverride = func(_ *apiServer, _ context.Context, path string) error {
		deleteCalls = append(deleteCalls, path)
		if failDelete && path == oldPath {
			return errors.New("temporary GCS failure")
		}
		return nil
	}
	newPath := ownerStoragePrefix("owner@example.com") + "prepaid/replacement/retry.webp"
	response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/purchases/purchase-1/cards/card-1/package-image", map[string]string{
		"storage_path":          newPath,
		"expected_storage_path": oldPath,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("replacement should commit despite old delete failure: %d: %s", response.Code, response.Body.String())
	}
	if len(record.PendingImageCleanup) != 1 || record.PendingImageCleanup[0] != oldPath {
		t.Fatalf("failed old path was not durably queued: %+v", record.PendingImageCleanup)
	}
	failDelete = false
	records := []prepaidPurchaseRecord{record}
	prepaidCleanupPurchasesOverride = func(_ *apiServer, _ context.Context, _ string) ([]prepaidPurchaseRecord, error) {
		return records, nil
	}
	prepaidCleanupSavePurchaseOverride = func(_ *apiServer, _ context.Context, updated prepaidPurchaseRecord) error {
		records[0] = updated
		record = updated
		return nil
	}
	cleanup := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/cleanup-archived-images", nil)
	if cleanup.Code != http.StatusOK {
		t.Fatalf("pending cleanup retry failed: %d: %s", cleanup.Code, cleanup.Body.String())
	}
	if len(record.PendingImageCleanup) != 0 {
		t.Fatalf("successful retry did not clear pending path: %+v", record.PendingImageCleanup)
	}
	if len(deleteCalls) != 2 || deleteCalls[0] != oldPath || deleteCalls[1] != oldPath {
		t.Fatalf("expected initial failure and retry for old path, got %v", deleteCalls)
	}
}

func TestPrepaidReplacementTreatsMissingOldObjectAsSuccess(t *testing.T) {
	record := fixturePrepaidPurchase()
	oldPath := record.Cards[0].PackageImageStoragePath
	server := installPrepaidMutationStore(t, &record)
	setPrepaidReplacementObjectAttrs()
	prepaidDeleteObjectOverride = func(_ *apiServer, _ context.Context, path string) error {
		if path == oldPath {
			return gcs.ErrObjectNotExist
		}
		return nil
	}
	newPath := ownerStoragePrefix("owner@example.com") + "prepaid/replacement/missing.webp"
	response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/purchases/purchase-1/cards/card-1/package-image", map[string]string{
		"storage_path": newPath,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("replacement with missing old object failed: %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"pending_image_cleanup"`) {
		t.Fatalf("missing old object should count as cleaned: %s", response.Body.String())
	}
}

func TestPrepaidImageMutationRejectsStaleExpectedPathAndUnauthorizedOwner(t *testing.T) {
	record := fixturePrepaidPurchase()
	server := installPrepaidMutationStore(t, &record)
	setPrepaidReplacementObjectAttrs()
	deleted := make([]string, 0)
	setPrepaidImageDeleteResult(nil, &deleted)
	newPath := ownerStoragePrefix("owner@example.com") + "prepaid/replacement/stale.webp"

	stale := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/purchases/purchase-1/cards/card-1/package-image", map[string]string{
		"storage_path":          newPath,
		"expected_storage_path": "receipts/u_owner/prepaid/package/newer.webp",
	})
	if stale.Code != http.StatusConflict {
		t.Fatalf("expected stale replacement conflict, got %d: %s", stale.Code, stale.Body.String())
	}
	if record.Cards[0].PackageImageStoragePath != fixturePrepaidPurchase().Cards[0].PackageImageStoragePath {
		t.Fatal("stale replacement changed current image")
	}
	for _, path := range deleted {
		if path == record.Cards[0].PackageImageStoragePath {
			t.Fatal("stale replacement deleted current image")
		}
	}

	other := newPrepaidTestServer(t, "other@example.com", map[string]interface{}{"prepaid_tracker_enabled": true})
	prepaidPurchaseMutationOverride = func(_ *apiServer, _ context.Context, _ string, _ string, _ prepaidPurchaseMutator) (prepaidPurchaseRecord, error) {
		return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
	}
	unauthorized := performPrepaidRequest(t, other, http.MethodDelete, "/prepaid/purchases/purchase-1/cards/card-1", nil)
	if unauthorized.Code != http.StatusNotFound {
		t.Fatalf("expected cross-owner delete to be hidden, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}

	foreignPath := ownerStoragePrefix("owner@example.com") + "prepaid/replacement/foreign.webp"
	foreign := performPrepaidRequest(t, other, http.MethodPost, "/prepaid/purchases/purchase-1/cards/card-1/package-image", map[string]string{"storage_path": foreignPath})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("expected cross-owner replacement path to be forbidden, got %d: %s", foreign.Code, foreign.Body.String())
	}
}

func TestPrepaidPendingCleanupDoesNotDeleteCurrentImage(t *testing.T) {
	record := fixturePrepaidPurchase()
	currentPath := record.Cards[0].PackageImageStoragePath
	record.PendingImageCleanup = []string{currentPath}
	server := installPrepaidMutationStore(t, &record)
	deleted := make([]string, 0)
	setPrepaidImageDeleteResult(nil, &deleted)
	records := []prepaidPurchaseRecord{record}
	prepaidCleanupPurchasesOverride = func(_ *apiServer, _ context.Context, _ string) ([]prepaidPurchaseRecord, error) { return records, nil }
	prepaidCleanupSavePurchaseOverride = func(_ *apiServer, _ context.Context, updated prepaidPurchaseRecord) error {
		records[0] = updated
		record = updated
		return nil
	}

	response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/cleanup-archived-images", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("cleanup failed: %d: %s", response.Code, response.Body.String())
	}
	if len(deleted) != 0 || len(record.PendingImageCleanup) != 0 {
		t.Fatalf("current image was touched or pending state retained: deleted=%v pending=%v", deleted, record.PendingImageCleanup)
	}
}

func TestMergePrepaidCleanupSnapshotPreservesConcurrentImageAndClearsStalePending(t *testing.T) {
	baseline := fixturePrepaidPurchase()
	oldPath := baseline.Cards[0].PackageImageStoragePath
	baselineData := prepaidPurchaseRecordToData(baseline)
	currentData := clonePrepaidPurchaseData(baselineData)
	newPath := ownerStoragePrefix("owner@example.com") + "prepaid/package/newer.webp"
	currentData["cards"].([]interface{})[0].(map[string]interface{})["package_image_storage_path"] = newPath
	currentData[prepaidPendingCleanupField] = []string{oldPath}

	updated := baseline
	updated.Cards = append([]prepaidCardRecord(nil), baseline.Cards...)
	updated.Cards[0].PackageImageStoragePath = ""
	updated.PendingImageCleanup = nil
	merged, changed, err := mergePrepaidCleanupSnapshotData(baselineData, currentData, updated)
	if err != nil {
		t.Fatalf("merge cleanup state: %v", err)
	}
	if !changed {
		t.Fatal("expected stale pending state to be cleaned")
	}
	mergedCard := prepaidCardsFromAny(merged["cards"])[0]
	if mergedCard.PackageImageStoragePath != newPath {
		t.Fatalf("concurrent replacement was overwritten: %q", mergedCard.PackageImageStoragePath)
	}
	if paths := prepaidPendingCleanupPaths(merged[prepaidPendingCleanupField]); len(paths) != 0 {
		t.Fatalf("current replacement path left stale pending cleanup: %v", paths)
	}
}
