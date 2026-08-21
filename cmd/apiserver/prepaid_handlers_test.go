package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fs "cloud.google.com/go/firestore"
)

type prepaidPurchasesResponse struct {
	Purchases []prepaidPurchaseRecord `json:"purchases"`
	Count     int                     `json:"count"`
}

type prepaidCardDetailResponse struct {
	PurchaseID string            `json:"purchase_id"`
	Card       prepaidCardRecord `json:"card"`
}

type prepaidImageResponse struct {
	PurchaseID string `json:"purchase_id"`
	CardID     string `json:"card_id"`
	ImageType  string `json:"image_type"`
	ImageURL   string `json:"image_url"`
	ExpiresAt  string `json:"expires_at"`
}

type prepaidSearchResponse struct {
	Results []prepaidSearchResult `json:"results"`
	Count   int                   `json:"count"`
}

func TestPrepaidTrackerEnabledHandlerForbidsMissingMalformedOrFalse(t *testing.T) {
	cases := []struct {
		name    string
		userDoc map[string]interface{}
	}{
		{name: "false", userDoc: map[string]interface{}{"prepaid_tracker_enabled": false}},
		{name: "missing", userDoc: map[string]interface{}{}},
		{name: "malformed", userDoc: map[string]interface{}{"prepaid_tracker_enabled": "yes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newPrepaidTestServer(t, "owner@example.com", tc.userDoc)
			rec := performPrepaidRequest(t, server, http.MethodGet, "/prepaid/status", nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected forbidden, got %d", rec.Code)
			}
		})
	}
}

func TestPrepaidHandlersEnforceOwnershipAndRedaction(t *testing.T) {
	ownerPurchase := fixturePrepaidPurchase()
	archivedPurchase := fixtureArchivedPrepaidPurchase()
	ownerCard := ownerPurchase.Cards[0]

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		ownerEmail string
		userDoc    map[string]interface{}
		status     int
		configure  func()
		assertBody func(t *testing.T, body []byte)
	}{
		{
			name:       "create purchase redacts credentials",
			method:     http.MethodPost,
			path:       "/prepaid/purchases",
			body:       map[string]any{"sales_receipt_id": "receipt-1"},
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusCreated,
			configure: func() {
				prepaidCreatePurchaseOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ prepaidCreatePurchaseRequest) (prepaidPurchaseRecord, error) {
					return ownerPurchase, nil
				}
			},
			assertBody: assertPurchaseRedacted,
		},
		{
			name:       "add activation receipt redacts credentials",
			method:     http.MethodPost,
			path:       "/prepaid/purchases/purchase-1/activation-receipts",
			body:       map[string]any{"storage_path": "receipts/owner/prepaid/activation/1.webp"},
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusCreated,
			configure: func() {
				prepaidAddActivationReceiptOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ prepaidActivationReceiptInput) (prepaidPurchaseRecord, error) {
					return ownerPurchase, nil
				}
			},
			assertBody: assertPurchaseRedacted,
		},
		{
			name:       "add card redacts credentials",
			method:     http.MethodPost,
			path:       "/prepaid/purchases/purchase-1/cards",
			body:       map[string]any{"activation_barcode": "123456789012345678901234567890", "vanilla_serial": "12345678901", "confirmed": true},
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusCreated,
			configure: func() {
				prepaidAddCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ prepaidCardInput) (prepaidPurchaseRecord, error) {
					return ownerPurchase, nil
				}
			},
			assertBody: assertPurchaseRedacted,
		},
		{
			name:       "update card redacts credentials",
			method:     http.MethodPatch,
			path:       "/prepaid/purchases/purchase-1/cards/card-1",
			body:       map[string]any{"confirmed": true, "pan": "1234567890123456", "expiry": "12/29", "cvv": "123"},
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidUpdateCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string, _ prepaidCardInput) (prepaidPurchaseRecord, error) {
					return ownerPurchase, nil
				}
			},
			assertBody: assertPurchaseRedacted,
		},
		{
			name:       "archive card redacts credentials",
			method:     http.MethodPost,
			path:       "/prepaid/purchases/purchase-1/cards/card-1/archive",
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidArchiveCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string) (prepaidPurchaseRecord, error) {
					return archivedPurchase, nil
				}
			},
			assertBody: assertPurchaseRedacted,
		},
		{
			name:       "list purchase redacts credentials",
			method:     http.MethodGet,
			path:       "/prepaid/purchases?state=all",
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidListPurchasesOverride = func(_ *apiServer, _ context.Context, _ string, _ string) ([]prepaidPurchaseRecord, error) {
					return []prepaidPurchaseRecord{ownerPurchase}, nil
				}
			},
			assertBody: func(t *testing.T, body []byte) {
				var response prepaidPurchasesResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if len(response.Purchases) != 1 {
					t.Fatalf("expected one purchase, got %d", len(response.Purchases))
				}
				assertRedactedPurchase(t, response.Purchases[0])
			},
		},
		{
			name:       "get purchase redacts credentials",
			method:     http.MethodGet,
			path:       "/prepaid/purchases/purchase-1",
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidGetPurchaseOverride = func(_ *apiServer, _ context.Context, _ string, _ string) (prepaidPurchaseRecord, error) {
					return ownerPurchase, nil
				}
			},
			assertBody: func(t *testing.T, body []byte) {
				var response prepaidPurchaseRecord
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				assertRedactedPurchase(t, response)
			},
		},
		{
			name:       "get card detail returns credentials to owner",
			method:     http.MethodGet,
			path:       "/prepaid/purchases/purchase-1/cards/card-1",
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidGetCardDetailOverride = func(_ *apiServer, _ context.Context, user *verifiedUser, _ string, _ string) (prepaidCardRecord, error) {
					if user.Email != "owner@example.com" {
						return prepaidCardRecord{}, httpError{status: http.StatusNotFound, detail: "Card not found"}
					}
					return ownerCard, nil
				}
			},
			assertBody: func(t *testing.T, body []byte) {
				var response prepaidCardDetailResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Card.PAN != "1234567890123456" || response.Card.Expiry != "12/29" || response.Card.CVV != "123" {
					t.Fatalf("expected credentials for owner, got pan=%q expiry=%q cvv=%q", response.Card.PAN, response.Card.Expiry, response.Card.CVV)
				}
			},
		},
		{
			name:       "get card detail rejects other user",
			method:     http.MethodGet,
			path:       "/prepaid/purchases/purchase-1/cards/card-1",
			ownerEmail: "other@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusNotFound,
			configure: func() {
				prepaidGetCardDetailOverride = func(_ *apiServer, _ context.Context, user *verifiedUser, _ string, _ string) (prepaidCardRecord, error) {
					if user.Email != "owner@example.com" {
						return prepaidCardRecord{}, httpError{status: http.StatusNotFound, detail: "Card not found"}
					}
					return ownerCard, nil
				}
			},
		},
		{
			name:       "read purchase rejects other user",
			method:     http.MethodGet,
			path:       "/prepaid/purchases/purchase-1",
			ownerEmail: "other@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusNotFound,
			configure: func() {
				prepaidGetPurchaseOverride = func(_ *apiServer, _ context.Context, _ string, ownerEmail string) (prepaidPurchaseRecord, error) {
					if ownerEmail != "owner@example.com" {
						return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
					}
					return ownerPurchase, nil
				}
			},
		},
		{
			name:       "update card rejects other user",
			method:     http.MethodPatch,
			path:       "/prepaid/purchases/purchase-1/cards/card-1",
			body:       map[string]any{"confirmed": true, "pan": "1234567890123456"},
			ownerEmail: "other@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusNotFound,
			configure: func() {
				prepaidUpdateCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string, _ prepaidCardInput) (prepaidPurchaseRecord, error) {
					return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Card not found"}
				}
			},
		},
		{
			name:       "archive card rejects other user",
			method:     http.MethodPost,
			path:       "/prepaid/purchases/purchase-1/cards/card-1/archive",
			ownerEmail: "other@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusNotFound,
			configure: func() {
				prepaidArchiveCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string) (prepaidPurchaseRecord, error) {
					return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Card not found"}
				}
			},
		},
		{
			name:       "attach receipt rejects other user",
			method:     http.MethodPost,
			path:       "/prepaid/purchases",
			body:       map[string]any{"sales_receipt_id": "receipt-1", "activation_receipts": []any{}, "cards": []any{}},
			ownerEmail: "other@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusNotFound,
			configure: func() {
				prepaidGetOwnedReceiptOverride = func(_ *apiServer, _ context.Context, _ string, ownerEmail string) (ownedReceipt, error) {
					if ownerEmail != "owner@example.com" {
						return ownedReceipt{}, httpError{status: http.StatusNotFound, detail: "Receipt receipt-1 not found"}
					}
					return ownedReceipt{Data: map[string]interface{}{"owner_email": ownerEmail}}, nil
				}
			},
		},
		{
			name:       "archive preserves barcode and serial",
			method:     http.MethodPost,
			path:       "/prepaid/purchases/purchase-1/cards/card-1/archive",
			ownerEmail: "owner@example.com",
			userDoc:    map[string]interface{}{"prepaid_tracker_enabled": true},
			status:     http.StatusOK,
			configure: func() {
				prepaidArchiveCardOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string) (prepaidPurchaseRecord, error) {
					return archivedPurchase, nil
				}
			},
			assertBody: func(t *testing.T, body []byte) {
				var response prepaidPurchaseRecord
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if len(response.Cards) != 1 {
					t.Fatalf("expected one card, got %d", len(response.Cards))
				}
				if response.Cards[0].ActivationBarcode != "123456789012345678901234567890" {
					t.Fatalf("expected activation barcode preserved, got %q", response.Cards[0].ActivationBarcode)
				}
				if response.Cards[0].VanillaSerial != "12345678901" {
					t.Fatalf("expected serial preserved, got %q", response.Cards[0].VanillaSerial)
				}
				if response.Cards[0].State != "archived" {
					t.Fatalf("expected archived state, got %q", response.Cards[0].State)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newPrepaidTestServer(t, tc.ownerEmail, tc.userDoc)
			if tc.configure != nil {
				tc.configure()
			}
			rec := performPrepaidRequest(t, server, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("expected status %d, got %d with body %s", tc.status, rec.Code, rec.Body.String())
			}
			if tc.assertBody != nil {
				tc.assertBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestPrepaidCardDetailResponseHasNoStore(t *testing.T) {
	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{"prepaid_tracker_enabled": true})
	prepaidGetCardDetailOverride = func(_ *apiServer, _ context.Context, _ *verifiedUser, _ string, _ string) (prepaidCardRecord, error) {
		return fixturePrepaidPurchase().Cards[0], nil
	}
	rec := performPrepaidRequest(t, server, http.MethodGet, "/prepaid/purchases/purchase-1/cards/card-1", nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
}

func TestPrepaidSearch(t *testing.T) {
	ownerPurchase := fixturePrepaidPurchase()
	ownerPurchase.Cards[0].ActivationBarcode = "111111111111111111111111111111"
	matchingPurchase := fixturePrepaidPurchase()
	matchingPurchase.ID = "purchase-2"
	matchingCard := matchingPurchase.Cards[0]
	matchingCard.ID = "card-2"
	matchingCard.PAN = "9999999999993456"
	matchingCard.Last4 = "3456"
	matchingCard.ActivationBarcode = "222222222222222222222222222222"
	matchingDenomination := 50.0
	matchingCard.Denomination = &matchingDenomination
	matchingPurchase.Cards = []prepaidCardRecord{matchingCard}
	foreignPurchase := fixturePrepaidPurchase()
	foreignPurchase.ID = "purchase-foreign"
	foreignPurchase.OwnerEmail = "other@example.com"

	searchPurchases := []prepaidPurchaseRecord{ownerPurchase, matchingPurchase, foreignPurchase}
	tests := []struct {
		name             string
		ownerEmail       string
		value            string
		wantStatus       int
		wantCount        int
		wantCardIDs      []string
		wantNoCredential bool
	}{
		{
			name:             "exact PAN returns correct card",
			ownerEmail:       "owner@example.com",
			value:            "1234567890123456",
			wantStatus:       http.StatusOK,
			wantCount:        1,
			wantCardIDs:      []string{"card-1"},
			wantNoCredential: true,
		},
		{
			name:             "last4 returns all matching cards",
			ownerEmail:       "owner@example.com",
			value:            "3456",
			wantStatus:       http.StatusOK,
			wantCount:        2,
			wantCardIDs:      []string{"card-1", "card-2"},
			wantNoCredential: true,
		},
		{
			name:             "formatted PAN is normalized",
			ownerEmail:       "owner@example.com",
			value:            "1234-5678 9012-3456",
			wantStatus:       http.StatusOK,
			wantCount:        1,
			wantCardIDs:      []string{"card-1"},
			wantNoCredential: true,
		},
		{
			name:       "no match returns empty result set",
			ownerEmail: "owner@example.com",
			value:      "0000",
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
		{
			name:       "invalid length is rejected",
			ownerEmail: "owner@example.com",
			value:      "12345",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "another user receives no cards",
			ownerEmail: "another@example.com",
			value:      "3456",
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newPrepaidTestServer(t, tc.ownerEmail, map[string]interface{}{"prepaid_tracker_enabled": true})
			prepaidSearchPurchasesOverride = func(_ *apiServer, _ context.Context, _ string) ([]prepaidPurchaseRecord, error) {
				return searchPurchases, nil
			}
			rec := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/search", map[string]string{"value": tc.value})
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d with body %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("expected Cache-Control no-store, got %q", got)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var response prepaidSearchResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Count != tc.wantCount || len(response.Results) != tc.wantCount {
				t.Fatalf("expected %d results, got count=%d results=%d", tc.wantCount, response.Count, len(response.Results))
			}
			for index, expectedCardID := range tc.wantCardIDs {
				if response.Results[index].CardID != expectedCardID {
					t.Fatalf("expected result %d card %q, got %q", index, expectedCardID, response.Results[index].CardID)
				}
			}
			if tc.wantNoCredential {
				for _, credential := range []string{"1234567890123456", "9999999999993456", `"cvv"`, `"expiry"`} {
					if strings.Contains(rec.Body.String(), credential) {
						t.Fatalf("search response exposed credential %q: %s", credential, rec.Body.String())
					}
				}
			}
		})
	}
}

func TestPrepaidCardImageSigning(t *testing.T) {
	tests := []struct {
		name             string
		ownerEmail       string
		path             string
		wantStatus       int
		wantSignedPath   string
		configureRecord  func() prepaidPurchaseRecord
		configureSigning func(storagePath string) (string, error)
	}{
		{
			name:           "owner can obtain package image url",
			ownerEmail:     "owner@example.com",
			path:           "/prepaid/purchases/purchase-1/cards/card-1/package-image",
			wantStatus:     http.StatusOK,
			wantSignedPath: ownerStoragePrefix("owner@example.com") + "prepaid/package/card-1.webp",
		},
		{
			name:           "owner can obtain opened card image url",
			ownerEmail:     "owner@example.com",
			path:           "/prepaid/purchases/purchase-1/cards/card-1/opened-card-image",
			wantStatus:     http.StatusOK,
			wantSignedPath: ownerStoragePrefix("owner@example.com") + "prepaid/opened/card-1.webp",
		},
		{
			name:       "another user cannot obtain package image url",
			ownerEmail: "other@example.com",
			path:       "/prepaid/purchases/purchase-1/cards/card-1/package-image",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "another user cannot obtain opened card image url",
			ownerEmail: "other@example.com",
			path:       "/prepaid/purchases/purchase-1/cards/card-1/opened-card-image",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing image path returns not found",
			ownerEmail: "owner@example.com",
			path:       "/prepaid/purchases/purchase-1/cards/card-1/package-image",
			wantStatus: http.StatusNotFound,
			configureRecord: func() prepaidPurchaseRecord {
				record := fixturePrepaidPurchase()
				record.Cards[0].PackageImageStoragePath = ""
				return record
			},
		},
		{
			name:       "missing gcs object returns not found",
			ownerEmail: "owner@example.com",
			path:       "/prepaid/purchases/purchase-1/cards/card-1/package-image",
			wantStatus: http.StatusNotFound,
			configureSigning: func(string) (string, error) {
				return "", httpError{status: http.StatusNotFound, detail: "Image not found"}
			},
		},
		{
			name:       "another card does not receive card image url",
			ownerEmail: "owner@example.com",
			path:       "/prepaid/purchases/purchase-1/cards/card-2/package-image",
			wantStatus: http.StatusNotFound,
			configureRecord: func() prepaidPurchaseRecord {
				record := fixturePrepaidPurchase()
				record.Cards = append(record.Cards, prepaidCardRecord{
					ID:                "card-2",
					ActivationBarcode: "999999999999999999999999999999",
					VanillaSerial:     "99999999999",
					State:             "active",
				})
				return record
			},
		},
		{
			name:       "another purchase does not receive card image url",
			ownerEmail: "owner@example.com",
			path:       "/prepaid/purchases/purchase-2/cards/card-1/package-image",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := fixturePrepaidPurchase()
			if tc.configureRecord != nil {
				record = tc.configureRecord()
			}
			signedPaths := make([]string, 0)
			server := newPrepaidTestServer(t, tc.ownerEmail, map[string]interface{}{"prepaid_tracker_enabled": true})
			prepaidGetPurchaseOverride = func(_ *apiServer, _ context.Context, purchaseID string, ownerEmail string) (prepaidPurchaseRecord, error) {
				if ownerEmail != "owner@example.com" || purchaseID != "purchase-1" {
					return prepaidPurchaseRecord{}, httpError{status: http.StatusNotFound, detail: "Prepaid purchase not found"}
				}
				return record, nil
			}
			signedImageURLOverride = func(_ context.Context, storagePath string) (string, error) {
				signedPaths = append(signedPaths, storagePath)
				if tc.configureSigning != nil {
					return tc.configureSigning(storagePath)
				}
				return "https://signed.example/" + storagePath, nil
			}

			rec := performPrepaidRequest(t, server, http.MethodGet, tc.path, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d with body %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("expected Cache-Control no-store, got %q", got)
			}
			if tc.wantSignedPath == "" {
				if len(signedPaths) > 0 && tc.configureSigning == nil {
					t.Fatalf("expected no signed path, got %v", signedPaths)
				}
				var response map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if _, exists := response["image_url"]; exists {
					t.Fatalf("expected no image_url, got %v", response["image_url"])
				}
				return
			}
			if len(signedPaths) != 1 || signedPaths[0] != tc.wantSignedPath {
				t.Fatalf("expected signed path %q, got %v", tc.wantSignedPath, signedPaths)
			}
			var response prepaidImageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.ImageURL == "" {
				t.Fatal("expected image_url")
			}
			if response.CardID != "card-1" || response.PurchaseID != "purchase-1" {
				t.Fatalf("unexpected response identifiers: %+v", response)
			}
		})
	}
}

func newPrepaidTestServer(t *testing.T, email string, userDoc map[string]interface{}) *apiServer {
	server := &apiServer{
		cfg: config{
			requireOAuth:   true,
			oauthClientIDs: []string{"client-id"},
		},
	}
	resetPrepaidTestOverrides()
	t.Cleanup(resetPrepaidTestOverrides)
	verifyGoogleTokenOverride = func(context.Context, string, []string, []string) (*verifiedUser, int, string) {
		return &verifiedUser{Email: email}, 0, ""
	}
	prepaidFindOrChooseUserDocOverride = func(_ *apiServer, _ string) (*fs.DocumentRef, map[string]interface{}) {
		return &fs.DocumentRef{ID: "user-doc"}, userDoc
	}
	return server
}

func performPrepaidRequest(t *testing.T, server *apiServer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		payload = data
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	server.handlePrepaid(rec, req)
	return rec
}

func fixturePrepaidPurchase() prepaidPurchaseRecord {
	ownerPrefix := ownerStoragePrefix("owner@example.com")
	return prepaidPurchaseRecord{
		ID:             "purchase-1",
		OwnerEmail:     "owner@example.com",
		SalesReceiptID: "receipt-1",
		ActivationReceipts: []prepaidActivationReceipt{
			{ID: "activation-1", StoragePath: ownerPrefix + "prepaid/activation/1.webp"},
		},
		Cards: []prepaidCardRecord{
			{
				ID:                         "card-1",
				ActivationBarcode:          "123456789012345678901234567890",
				VanillaSerial:              "12345678901",
				PAN:                        "1234567890123456",
				Expiry:                     "12/29",
				CVV:                        "123",
				Last4:                      "3456",
				DetailsCaptured:            true,
				State:                      "active",
				PackageImageStoragePath:    ownerPrefix + "prepaid/package/card-1.webp",
				OpenedCardImageStoragePath: ownerPrefix + "prepaid/opened/card-1.webp",
			},
		},
		ActiveCardCount:   1,
		ArchivedCardCount: 0,
	}
}

func fixtureArchivedPrepaidPurchase() prepaidPurchaseRecord {
	record := fixturePrepaidPurchase()
	record.Cards[0].State = "archived"
	record.Cards[0].ArchivedAt = "2026-08-21T00:00:00Z"
	record.ActiveCardCount = 0
	record.ArchivedCardCount = 1
	return record
}

func assertPurchaseRedacted(t *testing.T, body []byte) {
	t.Helper()
	var response prepaidPurchaseRecord
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertRedactedPurchase(t, response)
}

func assertRedactedPurchase(t *testing.T, response prepaidPurchaseRecord) {
	t.Helper()
	if len(response.Cards) != 1 {
		t.Fatalf("expected one card, got %d", len(response.Cards))
	}
	card := response.Cards[0]
	if card.PAN != "" || card.Expiry != "" || card.CVV != "" {
		t.Fatalf("expected credentials to be redacted, got pan=%q expiry=%q cvv=%q", card.PAN, card.Expiry, card.CVV)
	}
	if card.Last4 != "3456" {
		t.Fatalf("expected last4 to remain, got %q", card.Last4)
	}
	if !card.DetailsCaptured {
		t.Fatal("expected details_captured to remain true")
	}
}
