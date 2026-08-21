package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return prepaidPurchaseRecord{
		ID:             "purchase-1",
		OwnerEmail:     "owner@example.com",
		SalesReceiptID: "receipt-1",
		ActivationReceipts: []prepaidActivationReceipt{
			{ID: "activation-1", StoragePath: "receipts/owner/prepaid/activation/1.webp"},
		},
		Cards: []prepaidCardRecord{
			{
				ID:                "card-1",
				ActivationBarcode: "123456789012345678901234567890",
				VanillaSerial:     "12345678901",
				PAN:               "1234567890123456",
				Expiry:            "12/29",
				CVV:               "123",
				Last4:             "3456",
				DetailsCaptured:   true,
				State:             "active",
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
