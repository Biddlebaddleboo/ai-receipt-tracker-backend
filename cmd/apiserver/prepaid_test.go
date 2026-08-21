package main

import "testing"

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
