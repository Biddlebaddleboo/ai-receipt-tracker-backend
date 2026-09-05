package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

//go:embed ocr_prompt_array.webp
var ocrSystemPromptWebP []byte

var ocrSystemPromptDataURL = "data:image/webp;base64," + base64.StdEncoding.EncodeToString(ocrSystemPromptWebP)

const ocrCategoryPromptMarker = " Use these categories when guessing the receipt type: "
const ocrSystemInstruction = "Follow the first image."

type ocrSystemPromptTransport struct {
	base http.RoundTripper
}

func init() {
	base := http.DefaultTransport
	http.DefaultTransport = &ocrSystemPromptTransport{base: base}
}

func (t *ocrSystemPromptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Body == nil ||
		request.Method != http.MethodPost || request.URL.Host != "api.openai.com" || request.URL.Path != "/v1/responses" {
		return t.base.RoundTrip(request)
	}

	originalBody, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	_ = request.Body.Close()

	var payload openAIResponsesRequest
	if err := json.Unmarshal(originalBody, &payload); err != nil || !rewriteOCRPromptAsUserImage(&payload) {
		return t.base.RoundTrip(withRequestBody(request, originalBody))
	}

	rewrittenBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return t.base.RoundTrip(withRequestBody(request, rewrittenBody))
}

func rewriteOCRPromptAsUserImage(payload *openAIResponsesRequest) bool {
	if payload == nil || len(payload.Input) != 1 || payload.Input[0].Role != "user" {
		return false
	}

	legacyPrompt := ""
	receiptImages := make([]openAIInputContent, 0, 1)
	for _, content := range payload.Input[0].Content {
		switch content.Type {
		case "input_text":
			if strings.HasPrefix(content.Text, "Extract the readable text from this receipt image") {
				legacyPrompt = content.Text
			}
		case "input_image":
			receiptImages = append(receiptImages, content)
		}
	}
	if legacyPrompt == "" || len(receiptImages) == 0 {
		return false
	}

	userContent := []openAIInputContent{
		{Type: "input_image", ImageURL: ocrSystemPromptDataURL, Detail: "low"},
	}
	if markerIndex := strings.Index(legacyPrompt, ocrCategoryPromptMarker); markerIndex >= 0 {
		categoryInstruction := strings.TrimSpace(legacyPrompt[markerIndex+1:])
		if categoryInstruction != "" {
			userContent = append(userContent, openAIInputContent{Type: "input_text", Text: categoryInstruction})
		}
	}
	userContent = append(userContent, receiptImages...)

	payload.Input = []openAIInputMessage{
		{Role: "system", Content: []openAIInputContent{{Type: "input_text", Text: ocrSystemInstruction}}},
		{Role: "user", Content: userContent},
	}
	return true
}

func withRequestBody(request *http.Request, body []byte) *http.Request {
	cloned := request.Clone(request.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.ContentLength = int64(len(body))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return cloned
}
