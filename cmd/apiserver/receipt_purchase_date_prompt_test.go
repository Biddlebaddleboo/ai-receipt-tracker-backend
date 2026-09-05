package main

import (
	"strings"
	"testing"
)

func TestBuildOCRPromptExplicitlyRequestsPurchaseDate(t *testing.T) {
	prompt := buildOCRPrompt(nil)
	for _, phrase := range []string{
		"purchase_date: extract the receipt purchase/transaction date as printed",
		"use null if no clear date",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("prompt does not explicitly require %q: %s", phrase, prompt)
		}
	}
}
