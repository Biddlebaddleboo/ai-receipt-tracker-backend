package main

import (
	"testing"
	"time"
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
