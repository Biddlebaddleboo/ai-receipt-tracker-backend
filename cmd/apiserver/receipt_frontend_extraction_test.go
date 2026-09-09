package main

import (
	"strings"
	"testing"
)

func TestValidateFrontendExtractionKeepsFieldsIndependent(t *testing.T) {
	payload := &frontendExtractionPayload{
		Mode: "remaining",
		TrustedFields: map[string]frontendTrustedField{
			"total": {Value: "21.46", Confidence: 0.98, Status: "trusted", Source: "browser-ocr"},
		},
		UnresolvedFields: []string{"tax"},
	}
	got, unresolved, err := validateFrontendExtraction(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total == nil || *got.Total != 21.46 {
		t.Fatalf("total=%v", got.Total)
	}
	if got.Tax != nil || len(unresolved) != 4 {
		t.Fatalf("tax=%v unresolved=%v", got.Tax, unresolved)
	}
}

func TestValidateFrontendExtractionRejectsLowConfidenceTrustedField(t *testing.T) {
	_, _, err := validateFrontendExtraction(&frontendExtractionPayload{
		Mode: "none",
		TrustedFields: map[string]frontendTrustedField{
			"total": {Value: "21.46", Confidence: 0.91, Status: "trusted", Source: "browser-ocr"},
		},
	})
	if err == nil {
		t.Fatal("expected low-confidence field to be rejected")
	}
}

func TestValidateFrontendExtractionAcceptsCalibratedMLField(t *testing.T) {
	got, unresolved, err := validateFrontendExtraction(&frontendExtractionPayload{
		Mode: "remaining",
		TrustedFields: map[string]frontendTrustedField{
			"purchase_date": {Value: "2026-09-25", Confidence: 0.94, Status: "trusted", Source: "ml"},
		},
		UnresolvedFields: []string{"vendor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PurchaseDate == nil || *got.PurchaseDate != "2026-09-25" {
		t.Fatalf("purchase_date=%v", got.PurchaseDate)
	}
	if len(unresolved) != 4 {
		t.Fatalf("unresolved=%v", unresolved)
	}
}

func TestFrontendResolutionPromptOnlyNamesUnresolvedFields(t *testing.T) {
	prompt := buildFrontendResolutionPrompt([]string{"tax", "purchase_date"})
	if !strings.Contains(prompt, "tax") || !strings.Contains(prompt, "purchase_date") {
		t.Fatalf("prompt=%q", prompt)
	}
	if strings.Contains(prompt, "vendor") || strings.Contains(prompt, "subtotal") || strings.Contains(prompt, "total") {
		t.Fatalf("prompt leaked resolved fields: %q", prompt)
	}
	if strings.Contains(prompt, "image") || strings.Contains(prompt, "system") {
		t.Fatalf("prompt should remain plain field instructions: %q", prompt)
	}
}

func TestReadFrontendResolutionDoesNotAcceptResolvedFields(t *testing.T) {
	got := readFrontendResolution(`{"tax":2.47,"total":21.46,"vendor":"wrong"}`, []string{"tax"})
	if got.Tax == nil || *got.Tax != 2.47 {
		t.Fatalf("tax=%v", got.Tax)
	}
	if got.Total != nil || got.Vendor != nil {
		t.Fatalf("resolved fields were accepted: %#v", got)
	}
}

func TestEntireFrontendModeLeavesEveryFieldForAI(t *testing.T) {
	_, unresolved, err := validateFrontendExtraction(&frontendExtractionPayload{
		Mode: "entire",
		TrustedFields: map[string]frontendTrustedField{
			"total": {Value: "21.46", Confidence: 1, Status: "trusted", Source: "manual"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 5 {
		t.Fatalf("unresolved=%v", unresolved)
	}
}

func TestNoUnresolvedFrontendFieldsMeansNoAIWork(t *testing.T) {
	trusted := map[string]frontendTrustedField{}
	for _, key := range []string{"vendor", "purchase_date", "subtotal", "tax", "total"} {
		trusted[key] = frontendTrustedField{Value: "1", Confidence: 1, Status: "trusted", Source: "manual"}
	}
	_, unresolved, err := validateFrontendExtraction(&frontendExtractionPayload{Mode: "none", TrustedFields: trusted})
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected no GPT fields, got %v", unresolved)
	}
}

func TestAIResolutionCannotOverwriteTrustedField(t *testing.T) {
	vendor := "Reviewed Store"
	base := ocrResult{Vendor: &vendor}
	aiVendor := "AI Guess"
	tax := 2.5
	merged := mergeFrontendOCR(base, ocrResult{Vendor: &aiVendor, Tax: &tax}, []string{"tax"})
	if merged.Vendor == nil || *merged.Vendor != vendor {
		t.Fatalf("trusted vendor changed: %v", merged.Vendor)
	}
	if merged.Tax == nil || *merged.Tax != 2.5 {
		t.Fatalf("unresolved tax was not merged: %v", merged.Tax)
	}
}
