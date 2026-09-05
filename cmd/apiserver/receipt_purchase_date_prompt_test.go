package main

import (
	"strings"
	"testing"
)

func TestBuildOCRPromptExplicitlyRequestsPurchaseDate(t *testing.T) {
	prompt := buildOCRPrompt(nil)
	for _, phrase := range []string{
		"purchase_date: extract the receipt purchase/transaction date and output it in MM-DD-YYYY format",
		"example: 09-05-2026",
		"use null if no clear date",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("prompt does not explicitly require %q: %s", phrase, prompt)
		}
	}
}
