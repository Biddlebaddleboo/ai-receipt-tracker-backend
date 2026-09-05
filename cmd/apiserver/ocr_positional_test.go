package main

import "testing"

func TestReadStructuredFieldsPositionalJSON(t *testing.T) {
	raw := `["Costco",18.99,2.47,21.46,"Meals","2026-09-05","00123456",[["Pizza",1,18.99]]]`
	got := readStructuredFields(raw, []string{"Meals"})
	if got.Vendor == nil || *got.Vendor != "Costco" {
		t.Fatalf("vendor=%v", got.Vendor)
	}
	if got.Subtotal == nil || *got.Subtotal != 18.99 {
		t.Fatalf("subtotal=%v", got.Subtotal)
	}
	if got.Tax == nil || *got.Tax != 2.47 {
		t.Fatalf("tax=%v", got.Tax)
	}
	if got.Total == nil || *got.Total != 21.46 {
		t.Fatalf("total=%v", got.Total)
	}
	if got.Category == nil || *got.Category != "Meals" {
		t.Fatalf("category=%v", got.Category)
	}
	if got.PurchaseDate == nil || *got.PurchaseDate != "2026-09-05" {
		t.Fatalf("purchase_date=%v", got.PurchaseDate)
	}
	if got.InvoiceID == nil || *got.InvoiceID != "00123456" {
		t.Fatalf("invoice_id=%v", got.InvoiceID)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items=%#v", got.Items)
	}
	if got.Items[0].Name == nil || *got.Items[0].Name != "Pizza" {
		t.Fatalf("item.name=%v", got.Items[0].Name)
	}
	if got.Items[0].Quantity == nil || *got.Items[0].Quantity != 1 {
		t.Fatalf("item.quantity=%v", got.Items[0].Quantity)
	}
	if got.Items[0].Price == nil || *got.Items[0].Price != 18.99 {
		t.Fatalf("item.price=%v", got.Items[0].Price)
	}
}

func TestReadStructuredFieldsObjectFallback(t *testing.T) {
	got := readStructuredFields(`{"vendor":"Costco","invoice_id":"00123456"}`, nil)
	if got.Vendor == nil || *got.Vendor != "Costco" {
		t.Fatalf("vendor=%v", got.Vendor)
	}
	if got.InvoiceID == nil || *got.InvoiceID != "00123456" {
		t.Fatalf("invoice_id=%v", got.InvoiceID)
	}
}

func TestPositionalInvoiceIDLeadingZeros(t *testing.T) {
	got := readStructuredFields(`[null,null,null,null,null,null,"00123456",[]]`, nil)
	if got.InvoiceID == nil || *got.InvoiceID != "00123456" {
		t.Fatalf("invoice_id=%v", got.InvoiceID)
	}
}
