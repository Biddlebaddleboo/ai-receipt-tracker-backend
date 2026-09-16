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

func TestObjectResponseItemsArrayIsNotMisreadAsPositionalVendor(t *testing.T) {
	raw := `{"vendor":"Costco","subtotal":18.99,"items":[{"name":"Pizza","quantity":1,"price":18.99}]}`
	got := readStructuredFields(raw, nil)
	if got.Vendor == nil || *got.Vendor != "Costco" {
		t.Fatalf("vendor=%v", got.Vendor)
	}
	if len(got.Items) != 1 || got.Items[0].Name == nil || *got.Items[0].Name != "Pizza" {
		t.Fatalf("items=%#v", got.Items)
	}
}

func TestPositionalInvoiceIDLeadingZeros(t *testing.T) {
	got := readStructuredFields(`[null,null,null,null,null,null,"00123456",[]]`, nil)
	if got.InvoiceID == nil || *got.InvoiceID != "00123456" {
		t.Fatalf("invoice_id=%v", got.InvoiceID)
	}
}

func TestFencedPositionalJSON(t *testing.T) {
	got := readStructuredFields("```json\n[\"Costco\",null,null,21.46,null,null,null,[]]\n```", nil)
	if got.Vendor == nil || *got.Vendor != "Costco" {
		t.Fatalf("vendor=%v", got.Vendor)
	}
}

func TestReportedOuterObjectTupleResponseKeepsAllReceiptFieldsAndItems(t *testing.T) {
	raw := `[
  {
    "vendor": "Circle K 69011",
    "purchase_date": "2026-09-15",
    "transaction_id": "1240445",
    "category": "Food & Drink",
    "items": [
      ["VISA $75 VMS", 1, 75.00],
      ["VANILLA VISA ACTIVATIO", 1, 5.50],
      ["VISA $75 VMS", 1, 75.00],
      ["VANILLA VISA ACTIVATIO", 1, 5.50],
      ["VISA $75 VMS", 1, 75.00],
      ["VANILLA VISA ACTIVATIO", 1, 5.50]
    ]
  }
]`
	got := readStructuredFields(raw, []string{"Food & Drink"})
	if got.Vendor == nil || *got.Vendor != "Circle K 69011" {
		t.Fatalf("vendor=%v", got.Vendor)
	}
	if got.PurchaseDate == nil || *got.PurchaseDate != "2026-09-15" {
		t.Fatalf("purchase_date=%v", got.PurchaseDate)
	}
	if got.InvoiceID == nil || *got.InvoiceID != "1240445" {
		t.Fatalf("invoice_id=%v", got.InvoiceID)
	}
	if got.Category == nil || *got.Category != "Food & Drink" {
		t.Fatalf("category=%v", got.Category)
	}
	if len(got.Items) != 6 {
		t.Fatalf("expected six items, got %#v", got.Items)
	}
	for index, item := range got.Items {
		if item.Name == nil || item.Quantity == nil || item.Price == nil {
			t.Fatalf("item %d has missing values: %+v", index, item)
		}
		if *item.Quantity != 1 {
			t.Fatalf("item %d quantity=%v", index, *item.Quantity)
		}
		wantPrice := 75.0
		if index%2 == 1 {
			wantPrice = 5.5
		}
		if *item.Price != wantPrice {
			t.Fatalf("item %d price=%v, want %v", index, *item.Price, wantPrice)
		}
	}

	canonical := ocrItemsToReceiptItems(got.Items)
	if len(canonical) != 6 {
		t.Fatalf("expected six canonical items, got %#v", canonical)
	}
	for index, item := range canonical {
		if _, ok := item["name"].(string); !ok {
			t.Fatalf("canonical item %d name is not an object field: %#v", index, item)
		}
		if _, ok := item["quantity"]; !ok {
			t.Fatalf("canonical item %d has no quantity: %#v", index, item)
		}
		if _, ok := item["price"]; !ok {
			t.Fatalf("canonical item %d has no price: %#v", index, item)
		}
	}
}

func TestReceiptItemsFromAnyReadsHistoricalTuples(t *testing.T) {
	items := receiptItemsFromAny([]interface{}{
		[]interface{}{"Legacy item", float64(2), float64(4.5)},
		map[string]interface{}{"name": "Object item", "quantity": float64(1), "price": float64(3)},
	})
	if len(items) != 2 {
		t.Fatalf("expected two items, got %#v", items)
	}
	if items[0].Name != "Legacy item" || items[0].Quantity == nil || *items[0].Quantity != 2 || items[0].Price == nil || *items[0].Price != 4.5 {
		t.Fatalf("unexpected tuple item: %+v", items[0])
	}
	if items[1].Name != "Object item" || items[1].Quantity == nil || *items[1].Quantity != 1 || items[1].Price == nil || *items[1].Price != 3 {
		t.Fatalf("unexpected object item: %+v", items[1])
	}
}
