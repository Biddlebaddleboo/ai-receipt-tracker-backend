package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/google/uuid"
)

func TestPrepaidTrackerEnabled(t *testing.T) {
	if !prepaidTrackerEnabled(map[string]interface{}{"prepaid_tracker_enabled": true}) {
		t.Fatal("expected true flag to enable prepaid tracker")
	}
	if prepaidTrackerEnabled(map[string]interface{}{"prepaid_tracker_enabled": false}) {
		t.Fatal("expected false flag to disable prepaid tracker")
	}
	if prepaidTrackerEnabled(map[string]interface{}{}) {
		t.Fatal("expected missing flag to disable prepaid tracker")
	}
}

func TestPrepaidStoragePathOwnership(t *testing.T) {
	ownerPath := ownerStoragePrefix("owner@example.com") + "prepaid/package/card.webp"
	if !prepaidStoragePathBelongsToOwner("owner@example.com", ownerPath) {
		t.Fatal("expected owner storage path to match")
	}
	if prepaidStoragePathBelongsToOwner("other@example.com", ownerPath) {
		t.Fatal("expected another user's storage path to be rejected")
	}
}

func TestValidatePrepaidPackageExtraction(t *testing.T) {
	denomination := 50.0
	valid := prepaidPackageExtraction{
		ActivationBarcode: "123456789012345678901234567890",
		SerialNumber:      "12345678901",
		Denomination:      &denomination,
	}
	if warnings := validatePrepaidPackageExtraction(valid); len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}

	invalid := prepaidPackageExtraction{
		ActivationBarcode: "1234",
		SerialNumber:      "123",
	}
	warnings := validatePrepaidPackageExtraction(invalid)
	if len(warnings) != 3 {
		t.Fatalf("expected three warnings, got %v", warnings)
	}
}

func TestValidateOpenedCardExtraction(t *testing.T) {
	valid := prepaidOpenedCardExtraction{
		PAN:    "1234567890123456",
		Expiry: "12/29",
		CVV:    "123",
	}
	if warnings := validateOpenedCardExtraction(valid); len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}

	invalid := prepaidOpenedCardExtraction{
		PAN:    "123",
		Expiry: "",
		CVV:    "12",
	}
	warnings := validateOpenedCardExtraction(invalid)
	if len(warnings) != 3 {
		t.Fatalf("expected three warnings, got %v", warnings)
	}
}

func TestValidatePrepaidCardFrontExtractionIgnoresBackCredentials(t *testing.T) {
	valid := prepaidCardFrontExtraction{PAN: "1234567890123456", Expiry: "12/29"}
	if warnings := validatePrepaidCardFrontExtraction(valid); len(warnings) != 0 {
		t.Fatalf("expected no front warnings, got %v", warnings)
	}
	invalid := prepaidCardFrontExtraction{PAN: "123", Expiry: ""}
	if warnings := validatePrepaidCardFrontExtraction(invalid); len(warnings) != 2 {
		t.Fatalf("expected only PAN and expiry warnings, got %v", warnings)
	}
}

func TestValidatePrepaidCardBackExtractionIgnoresFrontCredentials(t *testing.T) {
	valid := prepaidCardBackExtraction{CVV: "123"}
	if warnings := validatePrepaidCardBackExtraction(valid); len(warnings) != 0 {
		t.Fatalf("expected no back warnings, got %v", warnings)
	}
	invalid := prepaidCardBackExtraction{CVV: "12"}
	if warnings := validatePrepaidCardBackExtraction(invalid); len(warnings) != 1 || warnings[0] != "cvv must be 3 or 4 digits" {
		t.Fatalf("expected only CVV warning, got %v", warnings)
	}
}

func TestPrepaidImageTypesIncludeCardFrontAndBack(t *testing.T) {
	for _, imageType := range []prepaidImageType{prepaidImageActivation, prepaidImagePackage, prepaidImageOpenedCard, prepaidImageCardFront, prepaidImageCardBack} {
		if !validPrepaidImageType(imageType) {
			t.Fatalf("expected image type %q to be valid", imageType)
		}
	}
}

func TestPrepaidSideSpecificExtractionRoutes(t *testing.T) {
	server := newPrepaidTestServer(t, "owner@example.com", map[string]interface{}{"prepaid_tracker_enabled": true})
	defer resetPrepaidTestOverrides()
	prepaidObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
		return &gcs.ObjectAttrs{Size: 1, ContentType: "image/webp"}, nil
	}
	signedImageURLOverride = func(_ context.Context, storagePath string) (string, error) {
		return "https://signed.example/" + storagePath, nil
	}
	prepaidVisionPromptOverride = func(_ context.Context, _ string, prompt string) (string, error) {
		if strings.Contains(prompt, "front") {
			if strings.Contains(strings.ToLower(prompt), "cvv") {
				t.Fatalf("front prompt unexpectedly requests CVV: %q", prompt)
			}
			return `{"pan":"1234 5678 9012 3456","expiry":"12/29","cvv":"999"}`, nil
		}
		if strings.Contains(prompt, "back") {
			if strings.Contains(strings.ToLower(prompt), "pan") || strings.Contains(strings.ToLower(prompt), "expiry") {
				t.Fatalf("back prompt unexpectedly requests front credentials: %q", prompt)
			}
			return `{"cvv":"123","pan":"1234567890123456","expiry":"12/29"}`, nil
		}
		return "{}", nil
	}

	front := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/card-front-extract", map[string]string{
		"storage_path": ownerStoragePrefix("owner@example.com") + "prepaid/card_front/front.webp",
	})
	if front.Code != http.StatusOK {
		t.Fatalf("front extraction status=%d body=%s", front.Code, front.Body.String())
	}
	var frontResponse struct {
		Extraction           prepaidCardFrontExtraction `json:"extraction"`
		Warnings             []string                   `json:"warnings"`
		RequiresConfirmation bool                       `json:"requires_confirmation"`
	}
	if err := json.Unmarshal(front.Body.Bytes(), &frontResponse); err != nil {
		t.Fatalf("decode front response: %v", err)
	}
	if frontResponse.Extraction.PAN != "1234567890123456" || frontResponse.Extraction.Expiry != "12/29" || len(frontResponse.Warnings) != 0 || !frontResponse.RequiresConfirmation {
		t.Fatalf("unexpected front extraction: %+v", frontResponse)
	}
	if strings.Contains(front.Body.String(), `"cvv"`) {
		t.Fatalf("front response exposed back credential: %s", front.Body.String())
	}

	back := performPrepaidRequest(t, server, http.MethodPost, "/prepaid/card-back-extract", map[string]string{
		"storage_path": ownerStoragePrefix("owner@example.com") + "prepaid/card_back/back.webp",
	})
	if back.Code != http.StatusOK {
		t.Fatalf("back extraction status=%d body=%s", back.Code, back.Body.String())
	}
	var backResponse struct {
		Extraction           prepaidCardBackExtraction `json:"extraction"`
		Warnings             []string                  `json:"warnings"`
		RequiresConfirmation bool                      `json:"requires_confirmation"`
	}
	if err := json.Unmarshal(back.Body.Bytes(), &backResponse); err != nil {
		t.Fatalf("decode back response: %v", err)
	}
	if backResponse.Extraction.CVV != "123" || len(backResponse.Warnings) != 0 || !backResponse.RequiresConfirmation {
		t.Fatalf("unexpected back extraction: %+v", backResponse)
	}
	if strings.Contains(back.Body.String(), `"pan"`) || strings.Contains(back.Body.String(), `"expiry"`) {
		t.Fatalf("back response exposed front credentials: %s", back.Body.String())
	}
}

func TestNormalizePrepaidActivationInputsSupportsClientIDsAndRejectsDuplicates(t *testing.T) {
	defer resetPrepaidTestOverrides()
	prepaidObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
		return &gcs.ObjectAttrs{Size: 1, ContentType: "image/webp"}, nil
	}
	server := &apiServer{}
	now := time.Now().UTC()
	clientID := "550e8400-e29b-41d4-a716-446655440000"
	entries, err := server.normalizePrepaidActivationInputs(requestContext(), "owner@example.com", []prepaidActivationReceiptInput{
		{ID: clientID, StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/client.webp"},
		{StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/generated.webp"},
	}, now)
	if err != nil {
		t.Fatalf("normalize activation inputs: %v", err)
	}
	if got := entries[0].(map[string]interface{})["id"]; got != clientID {
		t.Fatalf("expected supplied id to be preserved, got %v", got)
	}
	if generated := entries[1].(map[string]interface{})["id"].(string); generated == "" {
		t.Fatal("expected generated activation id")
	} else if _, err := uuid.Parse(generated); err != nil {
		t.Fatalf("generated activation id is not a UUID: %v", err)
	}

	_, err = server.normalizePrepaidActivationInputs(requestContext(), "owner@example.com", []prepaidActivationReceiptInput{
		{ID: clientID, StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/one.webp"},
		{ID: clientID, StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/two.webp"},
	}, now)
	if err == nil {
		t.Fatal("expected duplicate activation ids to be rejected")
	}
}

func TestNormalizePrepaidActivationInputsRejectsInvalidClientID(t *testing.T) {
	defer resetPrepaidTestOverrides()
	prepaidObjectAttrsOverride = func(_ context.Context, _ string) (*gcs.ObjectAttrs, error) {
		return &gcs.ObjectAttrs{Size: 1, ContentType: "image/webp"}, nil
	}
	_, err := (&apiServer{}).normalizePrepaidActivationInputs(requestContext(), "owner@example.com", []prepaidActivationReceiptInput{{
		ID:          "not-a-uuid",
		StoragePath: ownerStoragePrefix("owner@example.com") + "prepaid/activation/invalid.webp",
	}}, time.Now().UTC())
	if err == nil {
		t.Fatal("expected invalid activation id to be rejected")
	}
}

func TestPrepaidCardActivationAssociationValidationAndPatchPresence(t *testing.T) {
	validIDs := map[string]struct{}{"activation-1": {}}
	server := &apiServer{}
	base := prepaidCardInput{
		ActivationBarcode:   "123456789012345678901234567890",
		VanillaSerial:       "12345678901",
		ActivationReceiptID: "activation-1",
		Confirmed:           true,
	}
	cards, err := server.normalizePrepaidCardInputs(requestContext(), "owner@example.com", []prepaidCardInput{base, base}, time.Now().UTC(), validIDs)
	if err != nil {
		t.Fatalf("normalize associated cards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("expected one activation receipt to link to both cards, got %#v", cards)
	}
	firstCard := cards[0].(map[string]interface{})
	secondCard := cards[1].(map[string]interface{})
	if firstCard["activation_receipt_id"] != "activation-1" || secondCard["activation_receipt_id"] != "activation-1" {
		t.Fatalf("expected one activation receipt to link to both cards, got %#v", cards)
	}
	optional := base
	optional.ActivationReceiptID = ""
	optionalCards, err := server.normalizePrepaidCardInputs(requestContext(), "owner@example.com", []prepaidCardInput{optional}, time.Now().UTC(), validIDs)
	if err != nil {
		t.Fatalf("normalize optional association: %v", err)
	}
	if _, ok := optionalCards[0].(map[string]interface{})["activation_receipt_id"]; ok {
		t.Fatalf("optional association unexpectedly persisted: %#v", optionalCards[0])
	}

	missing := base
	missing.ActivationReceiptID = "missing"
	if _, err := server.normalizePrepaidCardInputs(requestContext(), "owner@example.com", []prepaidCardInput{missing}, time.Now().UTC(), validIDs); err == nil {
		t.Fatal("expected missing activation target to be rejected")
	}

	linkedUpdate, err := server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", base, time.Now().UTC(), nil, validIDs)
	if err != nil {
		t.Fatalf("normalize association link update: %v", err)
	}
	if linkedUpdate["activation_receipt_id"] != "activation-1" {
		t.Fatalf("expected association link update, got %#v", linkedUpdate)
	}

	var clear prepaidCardInput
	if err := json.Unmarshal([]byte(`{"confirmed":true,"activation_receipt_id":null}`), &clear); err != nil {
		t.Fatalf("decode explicit clear: %v", err)
	}
	update, err := server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", clear, time.Now().UTC(), map[string]interface{}{"activation_receipt_id": "activation-1"}, validIDs)
	if err != nil {
		t.Fatalf("normalize explicit clear: %v", err)
	}
	if value, ok := update["activation_receipt_id"]; !ok || value != "" {
		t.Fatalf("expected explicit clear update, got %#v", update)
	}

	var omitted prepaidCardInput
	if err := json.Unmarshal([]byte(`{"confirmed":true}`), &omitted); err != nil {
		t.Fatalf("decode omitted association: %v", err)
	}
	update, err = server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", omitted, time.Now().UTC(), map[string]interface{}{"activation_receipt_id": "activation-1"}, validIDs)
	if err != nil {
		t.Fatalf("normalize omitted association: %v", err)
	}
	if _, ok := update["activation_receipt_id"]; ok {
		t.Fatalf("omitted association unexpectedly changed relationship: %#v", update)
	}
}

func TestNormalizePrepaidCardUpdatePreservesBarcodeSerial(t *testing.T) {
	server := &apiServer{}
	now := time.Now().UTC()
	update, err := server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", prepaidCardInput{
		PAN:       "1234567890123456",
		Expiry:    "12/29",
		CVV:       "123",
		Confirmed: true,
	}, now, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected update error: %v", err)
	}
	if _, ok := update["activation_barcode"]; ok {
		t.Fatal("blank update should not overwrite activation_barcode")
	}
	if _, ok := update["vanilla_serial"]; ok {
		t.Fatal("blank update should not overwrite vanilla_serial")
	}
	if update["pan"] != "1234567890123456" {
		t.Fatalf("expected pan update, got %v", update["pan"])
	}
}

func TestNormalizePrepaidCardUpdateAllowsBarcodeAndSerialEdits(t *testing.T) {
	server := &apiServer{}
	update, err := server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", prepaidCardInput{
		ActivationBarcode: "999999999999999999999999999999",
		VanillaSerial:     "98765432109",
		Confirmed:         true,
	}, time.Now().UTC(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected update error: %v", err)
	}
	if update["activation_barcode"] != "999999999999999999999999999999" || update["vanilla_serial"] != "98765432109" {
		t.Fatalf("expected barcode/serial update, got %#v", update)
	}

	_, err = server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", prepaidCardInput{
		ActivationBarcode: "123",
		Confirmed:         true,
	}, time.Now().UTC(), map[string]interface{}{})
	if err == nil {
		t.Fatal("expected invalid barcode update to be rejected")
	}
}

func TestNormalizePrepaidCardUpdateUsesNewPANLast4(t *testing.T) {
	server := &apiServer{}
	now := time.Now().UTC()
	update, err := server.normalizePrepaidCardUpdate(requestContext(), "owner@example.com", prepaidCardInput{
		PAN:       "1234567890122222",
		Confirmed: true,
	}, now, map[string]interface{}{
		"pan":                "1234567890121111",
		"last4":              "1111",
		"activation_barcode": "123456789012345678901234567890",
		"vanilla_serial":     "12345678901",
	})
	if err != nil {
		t.Fatalf("unexpected update error: %v", err)
	}
	if update["pan"] != "1234567890122222" {
		t.Fatalf("expected new pan update, got %v", update["pan"])
	}
	if update["last4"] != "2222" {
		t.Fatalf("expected last4 from new pan, got %v", update["last4"])
	}
	if _, ok := update["activation_barcode"]; ok {
		t.Fatal("update should not overwrite activation_barcode")
	}
	if _, ok := update["vanilla_serial"]; ok {
		t.Fatal("update should not overwrite vanilla_serial")
	}
	if detailsCaptured, ok := update["details_captured"].(bool); !ok || !detailsCaptured {
		t.Fatalf("expected details_captured to remain true, got %v", update["details_captured"])
	}
}

func TestRedactPrepaidCardsRemovesPANAndCVV(t *testing.T) {
	cards := []prepaidCardRecord{{
		ID:                "card-1",
		PAN:               "1234567890123456",
		Expiry:            "12/29",
		CVV:               "123",
		ActivationBarcode: "123456789012345678901234567890",
		VanillaSerial:     "12345678901",
		State:             "active",
	}}
	redacted := redactPrepaidCards(cards)
	if redacted[0].PAN != "" || redacted[0].Expiry != "" || redacted[0].CVV != "" {
		t.Fatalf("expected PAN/expiry/CVV to be redacted, got pan=%q expiry=%q cvv=%q", redacted[0].PAN, redacted[0].Expiry, redacted[0].CVV)
	}
	if redacted[0].Last4 != "3456" {
		t.Fatalf("expected last4, got %q", redacted[0].Last4)
	}
	if !redacted[0].DetailsCaptured {
		t.Fatal("expected details captured flag")
	}
}

func TestPrepaidCardsFromAnyReturnsCredentialsForDetail(t *testing.T) {
	cards := prepaidCardsFromAny([]interface{}{
		map[string]interface{}{
			"id":                 "card-1",
			"pan":                "1234567890123456",
			"expiry":             "12/29",
			"cvv":                "123",
			"activation_barcode": "123456789012345678901234567890",
			"vanilla_serial":     "12345678901",
			"state":              "active",
		},
	})
	if len(cards) != 1 {
		t.Fatalf("expected one card, got %d", len(cards))
	}
	if cards[0].PAN != "1234567890123456" || cards[0].CVV != "123" {
		t.Fatalf("expected credentials for detail response, got pan=%q cvv=%q", cards[0].PAN, cards[0].CVV)
	}
}

func TestArchiveCardCounting(t *testing.T) {
	cards := []interface{}{
		map[string]interface{}{"id": "active", "state": "active"},
		map[string]interface{}{"id": "archived", "state": "archived"},
	}
	if count := countCardsByState(cards, "active"); count != 1 {
		t.Fatalf("expected one active card, got %d", count)
	}
	if count := countCardsByState(cards, "archived"); count != 1 {
		t.Fatalf("expected one archived card, got %d", count)
	}
}

func TestNormalizePrepaidExpiry(t *testing.T) {
	cases := map[string]string{
		"12/29":     "12/29",
		"1229":      "12/29",
		"2029-12":   "2029-12",
		"202912":    "2029-12",
		"13/29":     "",
		"bad input": "",
	}

	for input, expected := range cases {
		if actual := normalizePrepaidExpiry(input); actual != expected {
			t.Fatalf("normalizePrepaidExpiry(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestBuildPrepaidStorageKeyForOwner(t *testing.T) {
	key := buildPrepaidStorageKeyForOwner("User@Example.com", "package", "../card.webp")
	if key == "" {
		t.Fatal("expected storage key")
	}
	if want := ownerStoragePrefix("User@Example.com") + "prepaid/package/"; len(key) < len(want) || key[:len(want)] != want {
		t.Fatalf("storage key %q does not start with %q", key, want)
	}
}
