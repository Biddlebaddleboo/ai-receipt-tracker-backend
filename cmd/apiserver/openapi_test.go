package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAPISpecIsPublicAndContainsOnlyReadOnlyAIReceiptRoutes(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	recorder := httptest.NewRecorder()
	(&apiServer{}).handleOpenAPI(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON content type, got %q", got)
	}
	var document struct {
		OpenAPI string                            `json:"openapi"`
		Paths   map[string]map[string]interface{} `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("invalid OpenAPI JSON: %v", err)
	}
	if document.OpenAPI != "3.0.3" {
		t.Fatalf("unexpected OpenAPI version %q", document.OpenAPI)
	}
	expectedPaths := []string{"/ai/receipts", "/ai/receipts/{receipt_id}", "/ai/receipts/{receipt_id}/image"}
	if len(document.Paths) != len(expectedPaths) {
		t.Fatalf("expected exactly %d paths, got %d", len(expectedPaths), len(document.Paths))
	}
	for _, path := range expectedPaths {
		if _, ok := document.Paths[path]; !ok {
			t.Fatalf("missing path %q", path)
		}
	}
	for path := range document.Paths {
		if !strings.HasPrefix(path, "/ai/receipts") {
			t.Fatalf("unexpected non-AI path %q", path)
		}
	}
	if !strings.Contains(recorder.Body.String(), "rk_ai_<key-id>.<secret>") {
		t.Fatal("spec does not document the rk_ai bearer token format")
	}
}

func TestOpenAPISpecRejectsNonGETRequests(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/openapi.json", nil)
	recorder := httptest.NewRecorder()
	(&apiServer{}).handleOpenAPI(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}
