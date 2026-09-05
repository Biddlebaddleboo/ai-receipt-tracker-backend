package main

import (
	"encoding/json"
	"strings"
)

const (
	ocrPosVendor = iota
	ocrPosSubtotal
	ocrPosTax
	ocrPosTotal
	ocrPosCategory
	ocrPosPurchaseDate
	ocrPosInvoiceID
	ocrPosItems
)

func extractPositionalJSON(rawText string) ([]interface{}, bool) {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return nil, false
	}

	// Accept a bare JSON array or a fenced JSON array. Do not search for an
	// arbitrary '[' inside another JSON value: an object response can contain an
	// items array, and treating that nested array as the top-level positional
	// response shifts the first item object into the vendor position.
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			trimmed = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}

	var payload []interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, false
	}
	if !validPositionalPayload(payload) {
		return nil, false
	}
	return payload, true
}

func validPositionalPayload(payload []interface{}) bool {
	if len(payload) < 7 || len(payload) > 8 {
		return false
	}
	for i := 0; i < 7; i++ {
		switch payload[i].(type) {
		case nil, string, float64:
			// Scalars are valid in the fixed positions.
		default:
			return false
		}
	}
	if len(payload) == 8 {
		if payload[7] == nil {
			return true
		}
		if _, ok := payload[7].([]interface{}); !ok {
			return false
		}
	}
	return true
}

func readPositionalStructuredFields(rawText string, payload []interface{}, categoryOptions []string) ocrResult {
	at := func(index int) interface{} {
		if index < 0 || index >= len(payload) {
			return nil
		}
		return payload[index]
	}
	category := normalizeString(at(ocrPosCategory))
	if len(categoryOptions) > 0 {
		category = validateReceiptCategory(category, categoryOptions)
	}
	return ocrResult{
		Text:         rawText,
		Vendor:       normalizeString(at(ocrPosVendor)),
		Subtotal:     normalizeAmount(at(ocrPosSubtotal)),
		Tax:          normalizeAmount(at(ocrPosTax)),
		Total:        normalizeAmount(at(ocrPosTotal)),
		Category:     category,
		PurchaseDate: normalizeString(at(ocrPosPurchaseDate)),
		InvoiceID:    normalizeMerchantIdentifier(at(ocrPosInvoiceID)),
		Items:        extractPositionalReceiptItems(at(ocrPosItems)),
	}
}

func extractPositionalReceiptItems(raw interface{}) []ocrItem {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	items := make([]ocrItem, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.([]interface{})
		if !ok {
			continue
		}
		var nameRaw, quantityRaw, priceRaw interface{}
		if len(entry) > 0 {
			nameRaw = entry[0]
		}
		if len(entry) > 1 {
			quantityRaw = entry[1]
		}
		if len(entry) > 2 {
			priceRaw = entry[2]
		}
		name := normalizeString(nameRaw)
		quantity := normalizeAmount(quantityRaw)
		price := normalizeAmount(priceRaw)
		if name == nil && quantity == nil && price == nil {
			continue
		}
		items = append(items, ocrItem{Name: name, Quantity: quantity, Price: price})
	}
	return items
}
