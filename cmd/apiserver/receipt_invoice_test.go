package main

import (
	"strings"
	"testing"
	"time"
)

func TestReceiptInvoiceIDExtractionPreservesMerchantIdentifier(t *testing.T) {
	result := readStructuredFields(`{"vendor":"Store","invoice_id":"000A-12-XY","total":12.34}`, nil)
	if result.InvoiceID == nil || *result.InvoiceID != "000A-12-XY" {
		t.Fatalf("expected exact invoice id, got %#v", result.InvoiceID)
	}

	for _, label := range []string{"Invoice #", "Invoice ID", "Receipt #", "Transaction ID", "Transaction #", "Order #", "Reference #", "Bill #"} {
		if !strings.Contains(buildOCRPrompt(nil), label) {
			t.Fatalf("prompt does not mention %q", label)
		}
	}

	for _, key := range []string{"invoice_id", "merchant_invoice_id", "transaction_id", "receipt_number", "order_number", "reference_number", "bill_number"} {
		result := readStructuredFields(`{"`+key+`":"00042-Z"}`, nil)
		if result.InvoiceID == nil || *result.InvoiceID != "00042-Z" {
			t.Fatalf("expected %s to map to invoice id, got %#v", key, result.InvoiceID)
		}
	}
}

func TestBuildOCRPromptRequiresStringInvoiceID(t *testing.T) {
	prompt := buildOCRPrompt(nil)
	for _, phrase := range []string{
		"invoice_id MUST ALWAYS be a JSON string",
		"even if all digits",
		"`Transaction #: 00123456`",
		"seventh value must be `\"00123456\"`",
		"not `123456`",
		"use null when no clear merchant-issued identifier exists",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("prompt does not explicitly require %q: %s", phrase, prompt)
		}
	}
}

func TestReceiptInvoiceIDExtractionLeavesMissingOrAmbiguousValuesEmpty(t *testing.T) {
	result := readStructuredFields(`{"vendor":"Store","invoice_id":null,"transaction":"12345"}`, nil)
	if result.InvoiceID != nil {
		t.Fatalf("expected no invoice id for ambiguous unlabeled value, got %q", *result.InvoiceID)
	}
	numeric := readStructuredFields(`{"invoice_id":12345}`, nil)
	if numeric.InvoiceID != nil {
		t.Fatalf("expected numeric identifier to remain empty rather than lose formatting, got %q", *numeric.InvoiceID)
	}
}

func TestReceiptInvoiceIDIsStoredInCanonicalReceiptFields(t *testing.T) {
	invoiceID := "INV-0007"
	summary := buildReceiptMetadataSummary("owner@example.com", nil, nil, nil, nil, &invoiceID, time.Unix(0, 0))
	if summary["invoice_id"] != invoiceID {
		t.Fatalf("expected invoice id in metadata summary, got %#v", summary["invoice_id"])
	}

	record := receiptRecordFromMap("receipt-1", map[string]interface{}{"invoice_id": invoiceID})
	if record.InvoiceID == nil || *record.InvoiceID != invoiceID {
		t.Fatalf("expected invoice id in receipt response, got %#v", record.InvoiceID)
	}
}
