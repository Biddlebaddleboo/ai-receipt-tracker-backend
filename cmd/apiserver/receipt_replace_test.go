package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gcs "cloud.google.com/go/storage"
)

func performReceiptRequest(t *testing.T, server *apiServer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := newJSONRequest(t, method, path, body)
	rec := httptest.NewRecorder()
	server.handleReceiptByID(rec, req)
	return rec
}

func newJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	request, err := newTestRequest(method, path, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer token")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func newTestRequest(method, path string, body any) (*http.Request, error) {
	var requestBody []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		requestBody = encoded
	}
	return httptest.NewRequest(method, path, bytes.NewReader(requestBody)), nil
}

func TestReplaceReceiptImageSwapsOnlyImageAndDeletesOldObject(t *testing.T) {
	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{})
	oldPath := ownerStoragePrefix("owner@example.com") + "old.webp"
	newPath := ownerStoragePrefix("owner@example.com") + "new.webp"
	updatedPath := ""
	deletedPaths := make([]string, 0)
	prepaidGetOwnedReceiptOverride = func(_ *apiServer, _ context.Context, receiptID string, ownerEmail string) (ownedReceipt, error) {
		if receiptID != "receipt-1" || ownerEmail != "owner@example.com" {
			return ownedReceipt{}, httpError{status: http.StatusNotFound, detail: "Receipt not found"}
		}
		return ownedReceipt{Data: map[string]interface{}{"owner_email": ownerEmail, "storage_path": oldPath}}, nil
	}
	receiptObjectAttrsOverride = func(_ context.Context, path string) (*gcs.ObjectAttrs, error) {
		if path != newPath {
			t.Fatalf("validated unexpected object path %q", path)
		}
		return &gcs.ObjectAttrs{Size: 10, ContentType: "image/webp"}, nil
	}
	receiptStoragePathUpdateOverride = func(_ context.Context, _ ownedReceipt, path string) error {
		updatedPath = path
		return nil
	}
	receiptDeleteObjectOverride = func(_ context.Context, path string) error {
		deletedPaths = append(deletedPaths, path)
		return nil
	}

	response := performReceiptRequest(t, server, http.MethodPost, "/receipts/receipt-1/replace-image", map[string]string{"storage_path": newPath})
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store, got %q", response.Header().Get("Cache-Control"))
	}
	if updatedPath != newPath {
		t.Fatalf("expected only storage path update to %q, got %q", newPath, updatedPath)
	}
	if len(deletedPaths) != 1 || deletedPaths[0] != oldPath {
		t.Fatalf("expected old object deletion, got %v", deletedPaths)
	}
}

func TestReplaceReceiptImageRejectsForeignPathAndCleansFailedSwap(t *testing.T) {
	t.Run("foreign path is rejected", func(t *testing.T) {
		server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{})
		foreignPath := ownerStoragePrefix("other@example.com") + "foreign.webp"
		deleteCalls := 0
		receiptDeleteObjectOverride = func(_ context.Context, _ string) error {
			deleteCalls++
			return nil
		}
		response := performReceiptRequest(t, server, http.MethodPost, "/receipts/receipt-1/replace-image", map[string]string{"storage_path": foreignPath})
		if response.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", response.Code)
		}
		if deleteCalls != 0 {
			t.Fatalf("foreign object was unexpectedly deleted %d times", deleteCalls)
		}
	})

	t.Run("failed swap deletes new object and preserves old path", func(t *testing.T) {
		server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{})
		oldPath := ownerStoragePrefix("owner@example.com") + "old.webp"
		newPath := ownerStoragePrefix("owner@example.com") + "new.webp"
		deletedPaths := make([]string, 0)
		prepaidGetOwnedReceiptOverride = func(_ *apiServer, _ context.Context, _ string, ownerEmail string) (ownedReceipt, error) {
			return ownedReceipt{Data: map[string]interface{}{"owner_email": ownerEmail, "storage_path": oldPath}}, nil
		}
		receiptObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
			return &gcs.ObjectAttrs{Size: 10, ContentType: "image/webp"}, nil
		}
		receiptStoragePathUpdateOverride = func(_ context.Context, _ ownedReceipt, _ string) error {
			return errors.New("firestore unavailable")
		}
		receiptDeleteObjectOverride = func(_ context.Context, path string) error {
			deletedPaths = append(deletedPaths, path)
			return nil
		}

		response := performReceiptRequest(t, server, http.MethodPost, "/receipts/receipt-1/replace-image", map[string]string{"storage_path": newPath})
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", response.Code, response.Body.String())
		}
		if len(deletedPaths) != 1 || deletedPaths[0] != newPath {
			t.Fatalf("expected only new object cleanup, got %v", deletedPaths)
		}
	})
}

func TestReplaceReceiptImageTreatsMissingReplacementAsNotFound(t *testing.T) {
	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{})
	newPath := ownerStoragePrefix("owner@example.com") + "missing.webp"
	deletedPaths := make([]string, 0)
	prepaidGetOwnedReceiptOverride = func(_ *apiServer, _ context.Context, _ string, ownerEmail string) (ownedReceipt, error) {
		return ownedReceipt{Data: map[string]interface{}{"owner_email": ownerEmail, "storage_path": "old.webp"}}, nil
	}
	receiptObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
		return nil, gcs.ErrObjectNotExist
	}
	receiptDeleteObjectOverride = func(_ context.Context, path string) error {
		deletedPaths = append(deletedPaths, path)
		return nil
	}

	response := performReceiptRequest(t, server, http.MethodPost, "/receipts/receipt-1/replace-image", map[string]string{"storage_path": newPath})
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", response.Code)
	}
	if len(deletedPaths) != 1 || deletedPaths[0] != newPath {
		t.Fatalf("expected missing replacement cleanup, got %v", deletedPaths)
	}
}

func TestReceiptRecordFromMapPreservesOptionalImageGrayscale(t *testing.T) {
	record := receiptRecordFromMap("receipt-1", map[string]interface{}{"image_grayscale": true})
	if !record.ImageGrayscale {
		t.Fatal("expected grayscale metadata to be exposed when enabled")
	}
	colourRecord := receiptRecordFromMap("receipt-2", map[string]interface{}{})
	if colourRecord.ImageGrayscale {
		t.Fatal("expected absent grayscale metadata to remain colour/default")
	}
}
