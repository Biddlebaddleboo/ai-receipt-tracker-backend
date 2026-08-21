package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
)

func runPrepaidCleanupTest(t *testing.T, ownerEmail string, purchases []prepaidPurchaseRecord, deleteErrors map[string]error) (prepaidCleanupSummary, []prepaidPurchaseRecord, []string) {
	t.Helper()
	server := newPrepaidTestServer(t, ownerEmail, map[string]interface{}{"prepaid_tracker_enabled": true})
	records := append([]prepaidPurchaseRecord(nil), purchases...)
	deletedPaths := make([]string, 0)
	prepaidCleanupPurchasesOverride = func(_ *apiServer, _ context.Context, _ string) ([]prepaidPurchaseRecord, error) {
		return records, nil
	}
	prepaidCleanupSavePurchaseOverride = func(_ *apiServer, _ context.Context, updated prepaidPurchaseRecord) error {
		for index := range records {
			if records[index].ID == updated.ID {
				records[index] = updated
			}
		}
		return nil
	}
	prepaidDeleteObjectOverride = func(_ *apiServer, _ context.Context, path string) error {
		deletedPaths = append(deletedPaths, path)
		return deleteErrors[path]
	}

	response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/cleanup-archived-images", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("expected cleanup status 200, got %d with body %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", response.Header().Get("Cache-Control"))
	}
	var summary prepaidCleanupSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode cleanup response: %v", err)
	}
	return summary, records, deletedPaths
}

func TestCleanupArchivedPrepaidImagesDeletesArchivedPhotosAndPreservesCardData(t *testing.T) {
	purchase := fixtureArchivedPrepaidPurchase()
	originalCard := purchase.Cards[0]

	summary, records, deletedPaths := runPrepaidCleanupTest(t, "owner@example.com", []prepaidPurchaseRecord{purchase}, nil)
	if summary.PackageImagesDeleted != 1 || summary.OpenedCardImagesDeleted != 1 || summary.ActivationReceiptImagesDeleted != 1 || summary.SalesReceiptsPreserved != 1 || summary.ImageDeletionFailures != 0 {
		t.Fatalf("unexpected cleanup summary: %+v", summary)
	}
	if len(deletedPaths) != 3 {
		t.Fatalf("expected three deleted paths, got %v", deletedPaths)
	}
	updated := records[0]
	card := updated.Cards[0]
	if card.PackageImageStoragePath != "" || card.OpenedCardImageStoragePath != "" {
		t.Fatalf("expected archived card image paths cleared, got package=%q opened=%q", card.PackageImageStoragePath, card.OpenedCardImageStoragePath)
	}
	if card.PAN != originalCard.PAN || card.Last4 != originalCard.Last4 || card.Expiry != originalCard.Expiry || card.CVV != originalCard.CVV || card.Denomination != originalCard.Denomination || card.VanillaSerial != originalCard.VanillaSerial || card.ActivationBarcode != originalCard.ActivationBarcode || card.ID != originalCard.ID || card.State != originalCard.State || card.ArchivedAt != originalCard.ArchivedAt {
		t.Fatalf("cleanup changed extracted/card data: before=%+v after=%+v", originalCard, card)
	}
	if updated.SalesReceiptID != purchase.SalesReceiptID {
		t.Fatalf("cleanup changed sales receipt id from %q to %q", purchase.SalesReceiptID, updated.SalesReceiptID)
	}
	if updated.ActivationReceipts[0].StoragePath != "" {
		t.Fatalf("expected activation receipt path cleared, got %q", updated.ActivationReceipts[0].StoragePath)
	}
}

func TestCleanupArchivedPrepaidImagesPreservesActiveAndMixedPurchaseImages(t *testing.T) {
	active := fixturePrepaidPurchase()
	activeOriginal := active

	mixed := fixturePrepaidPurchase()
	mixed.ID = "purchase-mixed"
	mixed.Cards[0].State = "archived"
	mixed.Cards[0].ArchivedAt = "2026-08-21T00:00:00Z"
	activeMixedCard := fixturePrepaidPurchase().Cards[0]
	activeMixedCard.ID = "card-active"
	activeMixedCard.PAN = "9999999999999999"
	activeMixedCard.Last4 = "9999"
	mixed.Cards = append(mixed.Cards, activeMixedCard)
	mixed.ActiveCardCount = 1
	mixed.ArchivedCardCount = 1

	summary, records, deletedPaths := runPrepaidCleanupTest(t, "owner@example.com", []prepaidPurchaseRecord{active, mixed}, nil)
	if summary.PackageImagesDeleted != 1 || summary.OpenedCardImagesDeleted != 1 || summary.ActivationReceiptImagesDeleted != 0 || summary.SalesReceiptsPreserved != 2 {
		t.Fatalf("unexpected active/mixed cleanup summary: %+v", summary)
	}
	if len(deletedPaths) != 2 {
		t.Fatalf("expected only archived card paths deleted, got %v", deletedPaths)
	}
	if records[0].Cards[0].PackageImageStoragePath != activeOriginal.Cards[0].PackageImageStoragePath || records[0].Cards[0].OpenedCardImageStoragePath != activeOriginal.Cards[0].OpenedCardImageStoragePath {
		t.Fatal("active card image paths were changed")
	}
	if records[1].ActivationReceipts[0].StoragePath == "" {
		t.Fatal("mixed purchase activation receipt was deleted")
	}
	if records[1].Cards[1].PackageImageStoragePath == "" || records[1].Cards[1].OpenedCardImageStoragePath == "" {
		t.Fatal("active card images in mixed purchase were changed")
	}
}

func TestCleanupArchivedPrepaidImagesDeletesFullyArchivedPurchaseActivations(t *testing.T) {
	purchase := fixtureArchivedPrepaidPurchase()
	second := fixtureArchivedPrepaidPurchase().Cards[0]
	second.ID = "card-2"
	purchase.Cards = append(purchase.Cards, second)
	purchase.ActivationReceipts = append(purchase.ActivationReceipts, prepaidActivationReceipt{
		ID:          "activation-2",
		StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/2.webp",
	})
	purchase.ActiveCardCount = 0
	purchase.ArchivedCardCount = 2

	summary, records, _ := runPrepaidCleanupTest(t, "owner@example.com", []prepaidPurchaseRecord{purchase}, nil)
	if summary.ActivationReceiptImagesDeleted != 2 {
		t.Fatalf("expected two activation images deleted, got %+v", summary)
	}
	for _, receipt := range records[0].ActivationReceipts {
		if receipt.StoragePath != "" {
			t.Fatalf("expected activation receipt %s path cleared, got %q", receipt.ID, receipt.StoragePath)
		}
	}
}

func TestCleanupArchivedPrepaidImagesTreatsMissingObjectsAsSuccessAndIsIdempotent(t *testing.T) {
	purchase := fixtureArchivedPrepaidPurchase()
	missing := map[string]error{
		purchase.Cards[0].PackageImageStoragePath:    gcs.ErrObjectNotExist,
		purchase.Cards[0].OpenedCardImageStoragePath: gcs.ErrObjectNotExist,
		purchase.ActivationReceipts[0].StoragePath:   gcs.ErrObjectNotExist,
	}

	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{"prepaid_tracker_enabled": true})
	records := []prepaidPurchaseRecord{purchase}
	deletedPaths := make([]string, 0)
	prepaidCleanupPurchasesOverride = func(_ *apiServer, _ context.Context, _ string) ([]prepaidPurchaseRecord, error) { return records, nil }
	prepaidCleanupSavePurchaseOverride = func(_ *apiServer, _ context.Context, updated prepaidPurchaseRecord) error {
		records[0] = updated
		return nil
	}
	prepaidDeleteObjectOverride = func(_ *apiServer, _ context.Context, path string) error {
		deletedPaths = append(deletedPaths, path)
		return missing[path]
	}

	for run := 0; run < 2; run++ {
		response := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/cleanup-archived-images", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("run %d: expected status 200, got %d", run+1, response.Code)
		}
		var summary prepaidCleanupSummary
		if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
			t.Fatalf("run %d: decode summary: %v", run+1, err)
		}
		if run == 0 && (summary.PackageImagesDeleted != 1 || summary.OpenedCardImagesDeleted != 1 || summary.ActivationReceiptImagesDeleted != 1 || summary.ImageDeletionFailures != 0) {
			t.Fatalf("unexpected first cleanup summary: %+v", summary)
		}
		if run == 1 && (summary.PackageImagesDeleted != 0 || summary.OpenedCardImagesDeleted != 0 || summary.ActivationReceiptImagesDeleted != 0 || summary.ImageDeletionFailures != 0) {
			t.Fatalf("unexpected idempotent cleanup summary: %+v", summary)
		}
	}
	if len(deletedPaths) != 3 {
		t.Fatalf("expected second cleanup to have no paths to delete, got %v", deletedPaths)
	}
}

func TestCleanupArchivedPrepaidImagesIsOwnerIsolatedAndRetainsFailedPaths(t *testing.T) {
	ownerPurchase := fixtureArchivedPrepaidPurchase()
	foreignPurchase := fixtureArchivedPrepaidPurchase()
	foreignPurchase.ID = "foreign-purchase"
	foreignPurchase.OwnerEmail = "other@example.com"
	foreignPurchase.Cards[0].PackageImageStoragePath = ownerStoragePrefix("other@example.com") + "prepaid/package/foreign-card.webp"
	foreignPurchase.Cards[0].OpenedCardImageStoragePath = ownerStoragePrefix("other@example.com") + "prepaid/opened/foreign-card.webp"
	foreignPurchase.ActivationReceipts[0].StoragePath = ownerStoragePrefix("other@example.com") + "prepaid/activation/foreign.webp"
	foreignPath := foreignPurchase.Cards[0].PackageImageStoragePath
	failedPath := ownerPurchase.Cards[0].PackageImageStoragePath

	summary, records, deletedPaths := runPrepaidCleanupTest(t, "owner@example.com", []prepaidPurchaseRecord{ownerPurchase, foreignPurchase}, map[string]error{failedPath: errors.New("gcs delete failed")})
	if summary.PackageImagesDeleted != 0 || summary.OpenedCardImagesDeleted != 1 || summary.ActivationReceiptImagesDeleted != 1 || summary.SalesReceiptsPreserved != 1 || summary.ImageDeletionFailures != 1 {
		t.Fatalf("unexpected owner-isolated failure summary: %+v", summary)
	}
	for _, path := range deletedPaths {
		if strings.HasPrefix(path, ownerStoragePrefix("other@example.com")) || path == foreignPath {
			t.Fatalf("foreign path was touched: %q", path)
		}
	}
	if records[0].Cards[0].PackageImageStoragePath != failedPath {
		t.Fatalf("failed deletion incorrectly cleared path: %q", records[0].Cards[0].PackageImageStoragePath)
	}
	if records[1].Cards[0].PackageImageStoragePath != foreignPurchase.Cards[0].PackageImageStoragePath {
		t.Fatal("foreign purchase was modified")
	}
}
