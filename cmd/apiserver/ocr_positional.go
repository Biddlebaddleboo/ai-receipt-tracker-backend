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
	var payload []interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
		return payload, true
	}
	start := strings.Index(trimmed, "[")
	end := strings.LastIndex(trimmed, "]")
	if start < 0 || end <= start {
		return nil, false
	}
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &payload); err != nil {
		return nil, false
	}
	return payload, true
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
