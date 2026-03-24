package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	defaultModel = "gpt-4.1-mini"
	responsesURL = "https://api.openai.com/v1/responses"
)

var (
	fieldAliases = map[string][]string{
		"vendor":        {"vendor", "store", "merchant", "merchant_name", "store_name"},
		"subtotal":      {"subtotal", "sub_total", "pre_tax"},
		"tax":           {"tax", "sales_tax", "tax_amount"},
		"total":         {"total", "grand_total", "amount_due", "total_amount"},
		"category":      {"category", "type"},
		"purchase_date": {"purchase_date", "transaction_date", "date"},
	}
	jsonCandidate = regexp.MustCompile(`\{[^{}]*\}`)
	defaultPrompt = "Extract the readable text from this receipt image and summarize the line items and totals. " +
		"After the summary, output a JSON object with the following keys: `vendor`, `subtotal`, `tax`, `total`, " +
		"`category`, `purchase_date`, and `items`. The `items` array should include objects with `name`, `quantity`, " +
		"and `price`. Ensure the `subtotal` equals the sum of each item's `quantity` multiplied by its `price`; if you " +
		"can't confirm a value, set it to null. Do not add any explanation outside the JSON object."
)

type ocrHandle struct {
	client *http.Client
	model  string
	apiKey string
}

type requestPayload struct {
	Model       string         `json:"model"`
	Input       []inputMessage `json:"input"`
	Temperature float64        `json:"temperature"`
}

type inputMessage struct {
	Role    string         `json:"role"`
	Content []inputContent `json:"content"`
}

type inputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responseEnvelope struct {
	Error      *responseError  `json:"error"`
	OutputText string          `json:"output_text"`
	Output     []responseBlock `json:"output"`
}

type responseError struct {
	Message string `json:"message"`
}

type responseBlock struct {
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ocrItem struct {
	Name     string   `json:"name"`
	Quantity *float64 `json:"quantity,omitempty"`
	Price    *float64 `json:"price,omitempty"`
}

type ocrResult struct {
	Text         string    `json:"text"`
	Vendor       *string   `json:"vendor,omitempty"`
	Subtotal     *float64  `json:"subtotal,omitempty"`
	Tax          *float64  `json:"tax,omitempty"`
	Total        *float64  `json:"total,omitempty"`
	Category     *string   `json:"category,omitempty"`
	PurchaseDate *string   `json:"purchase_date,omitempty"`
	Items        []ocrItem `json:"items"`
}

type normalizedFields struct {
	Vendor       *string
	Subtotal     *float64
	Tax          *float64
	Total        *float64
	Category     *string
	PurchaseDate *string
}

var (
	ocrHandleMu   sync.Mutex
	ocrHandleSeq  int64
	ocrHandlePool = map[int64]*ocrHandle{}
)

func main() {}

func setError(errOut **C.char, err error) {
	if errOut == nil || err == nil {
		return
	}
	*errOut = C.CString(err.Error())
}

func putHandle(handle *ocrHandle) int64 {
	ocrHandleMu.Lock()
	defer ocrHandleMu.Unlock()
	ocrHandleSeq++
	ocrHandlePool[ocrHandleSeq] = handle
	return ocrHandleSeq
}

func takeHandle(id int64) (*ocrHandle, error) {
	ocrHandleMu.Lock()
	defer ocrHandleMu.Unlock()
	handle := ocrHandlePool[id]
	if handle == nil {
		return nil, fmt.Errorf("ocr handle %d not found", id)
	}
	return handle, nil
}

func dropHandle(id int64) *ocrHandle {
	ocrHandleMu.Lock()
	defer ocrHandleMu.Unlock()
	handle := ocrHandlePool[id]
	delete(ocrHandlePool, id)
	return handle
}

func goString(ptr *C.char) string {
	if ptr == nil {
		return ""
	}
	return C.GoString(ptr)
}

func goBytes(ptr *C.uchar, length C.longlong) []byte {
	if ptr == nil || length <= 0 {
		return []byte{}
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(length))
}

func buildPrompt(instructions string, categoryOptions []string) string {
	prompt := strings.TrimSpace(instructions)
	if prompt == "" {
		prompt = defaultPrompt
	}
	if len(categoryOptions) == 0 {
		return prompt
	}
	return prompt +
		" Use these categories when guessing the receipt type: " +
		strings.Join(categoryOptions, ", ") +
		". If none match, respond with `null` for the `category` key."
}

func decodeCategories(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var categories []string
	if err := json.Unmarshal([]byte(raw), &categories); err != nil {
		return nil, err
	}
	normalized := make([]string, 0, len(categories))
	for _, category := range categories {
		trimmed := strings.TrimSpace(category)
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return normalized, nil
}

func doResponsesRequest(handle *ocrHandle, prompt string, imageBytes []byte) (string, error) {
	imageDataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	payload := requestPayload{
		Model: handle.model,
		Input: []inputMessage{
			{
				Role: "user",
				Content: []inputContent{
					{Type: "input_text", Text: prompt},
					{Type: "input_image", ImageURL: imageDataURL},
				},
			},
		},
		Temperature: 0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		responsesURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+handle.apiKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := handle.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	var envelope responseEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return "", err
	}
	if response.StatusCode >= 400 {
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			return "", fmt.Errorf("OpenAI API error: %s", envelope.Error.Message)
		}
		return "", fmt.Errorf("OpenAI API error: status %d", response.StatusCode)
	}
	if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
		return "", fmt.Errorf("OpenAI API error: %s", envelope.Error.Message)
	}
	return collectText(envelope), nil
}

func collectText(envelope responseEnvelope) string {
	if strings.TrimSpace(envelope.OutputText) != "" {
		return strings.TrimSpace(envelope.OutputText)
	}
	fragments := make([]string, 0)
	for _, block := range envelope.Output {
		for _, content := range block.Content {
			if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
				fragments = append(fragments, strings.TrimSpace(content.Text))
			}
		}
	}
	return strings.TrimSpace(strings.Join(fragments, "\n"))
}

func extractJSON(rawText string) map[string]interface{} {
	if rawText == "" {
		return map[string]interface{}{}
	}
	start := strings.Index(rawText, "{")
	end := strings.LastIndex(rawText, "}")
	if start >= 0 && end > start {
		if parsed := tryParseJSON(rawText[start : end+1]); parsed != nil {
			return parsed
		}
	}
	for _, match := range jsonCandidate.FindAllString(rawText, -1) {
		if parsed := tryParseJSON(match); parsed != nil {
			return parsed
		}
	}
	return map[string]interface{}{}
}

func tryParseJSON(text string) map[string]interface{} {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil
	}
	return payload
}

func normalizeFields(payload map[string]interface{}) normalizedFields {
	lowerPayload := make(map[string]interface{}, len(payload))
	for key, value := range payload {
		lowerPayload[strings.ToLower(key)] = value
	}
	readAlias := func(canonical string) interface{} {
		for _, alias := range fieldAliases[canonical] {
			if value, ok := lowerPayload[alias]; ok {
				return value
			}
		}
		return nil
	}
	return normalizedFields{
		Vendor:       normalizeString(readAlias("vendor")),
		Subtotal:     normalizeAmount(readAlias("subtotal")),
		Tax:          normalizeAmount(readAlias("tax")),
		Total:        normalizeAmount(readAlias("total")),
		Category:     normalizeString(readAlias("category")),
		PurchaseDate: normalizeString(readAlias("purchase_date")),
	}
}

func extractItems(payload map[string]interface{}) []ocrItem {
	var candidates interface{}
	if value, ok := payload["items"]; ok {
		candidates = value
	} else if value, ok := payload["line_items"]; ok {
		candidates = value
	} else if value, ok := payload["entries"]; ok {
		candidates = value
	}
	entries, ok := candidates.([]interface{})
	if !ok {
		return []ocrItem{}
	}
	items := make([]ocrItem, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]interface{})
		if !ok {
			continue
		}
		name := normalizeString(firstPresent(entry, "name", "item", "description"))
		quantity := normalizeQuantity(firstPresent(entry, "quantity", "qty", "count"))
		price := normalizeAmount(firstPresent(entry, "price", "unit_price", "amount"))
		if name == nil && quantity == nil && price == nil {
			continue
		}
		itemName := ""
		if name != nil {
			itemName = *name
		}
		items = append(items, ocrItem{
			Name:     itemName,
			Quantity: quantity,
			Price:    price,
		})
	}
	return items
}

func firstPresent(entry map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if value, ok := entry[key]; ok {
			return value
		}
	}
	return nil
}

func normalizeQuantity(value interface{}) *float64 {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		return &typed
	case float32:
		converted := float64(typed)
		return &converted
	case int:
		converted := float64(typed)
		return &converted
	case int32:
		converted := float64(typed)
		return &converted
	case int64:
		converted := float64(typed)
		return &converted
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return &parsed
		}
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func normalizeString(value interface{}) *string {
	if value == nil {
		return nil
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return nil
	}
	return &text
}

func normalizeAmount(value interface{}) *float64 {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		return &typed
	case float32:
		converted := float64(typed)
		return &converted
	case int:
		converted := float64(typed)
		return &converted
	case int32:
		converted := float64(typed)
		return &converted
	case int64:
		converted := float64(typed)
		return &converted
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return &parsed
		}
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	text = strings.ReplaceAll(text, "$", "")
	text = strings.ReplaceAll(text, ",", "")
	if text == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func validateCategory(value *string, categories []string) *string {
	if value == nil {
		return nil
	}
	lookup := make(map[string]string, len(categories))
	for _, category := range categories {
		lookup[strings.ToLower(strings.TrimSpace(category))] = category
	}
	match, ok := lookup[strings.ToLower(strings.TrimSpace(*value))]
	if !ok {
		return nil
	}
	return &match
}

func readStructuredFields(rawText string, categories []string) ocrResult {
	payload := extractJSON(rawText)
	normalized := normalizeFields(payload)
	items := extractItems(payload)
	if len(categories) > 0 {
		normalized.Category = validateCategory(normalized.Category, categories)
	}
	return ocrResult{
		Text:         rawText,
		Vendor:       normalized.Vendor,
		Subtotal:     normalized.Subtotal,
		Tax:          normalized.Tax,
		Total:        normalized.Total,
		Category:     normalized.Category,
		PurchaseDate: normalized.PurchaseDate,
		Items:        items,
	}
}

func marshalResult(result ocrResult) (*C.char, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return C.CString(string(payload)), nil
}

//export OCRNew
func OCRNew(model *C.char, apiKey *C.char, errOut **C.char) C.longlong {
	resolvedModel := strings.TrimSpace(goString(model))
	if resolvedModel == "" {
		resolvedModel = defaultModel
	}
	resolvedAPIKey := strings.TrimSpace(goString(apiKey))
	if resolvedAPIKey == "" {
		resolvedAPIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if resolvedAPIKey == "" {
		setError(errOut, fmt.Errorf("OPENAI_API_KEY is required"))
		return 0
	}
	handle := &ocrHandle{
		client: &http.Client{Timeout: 2 * time.Minute},
		model:  resolvedModel,
		apiKey: resolvedAPIKey,
	}
	return C.longlong(putHandle(handle))
}

//export OCRClose
func OCRClose(handleID C.longlong) {
	dropHandle(int64(handleID))
}

//export OCRExtract
func OCRExtract(
	handleID C.longlong,
	imagePtr *C.uchar,
	imageLen C.longlong,
	instructions *C.char,
	categoryOptionsJSON *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	imageBytes := goBytes(imagePtr, imageLen)
	if len(imageBytes) == 0 {
		setError(errOut, fmt.Errorf("image_bytes is required"))
		return nil
	}
	categoryOptions, err := decodeCategories(goString(categoryOptionsJSON))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	prompt := buildPrompt(goString(instructions), categoryOptions)
	rawText, err := doResponsesRequest(handle, prompt, imageBytes)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	result := readStructuredFields(rawText, categoryOptions)
	marshaled, err := marshalResult(result)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return marshaled
}

//export OCRFree
func OCRFree(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}
