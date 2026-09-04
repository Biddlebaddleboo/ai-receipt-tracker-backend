package main

import "testing"

func TestRewriteOCRPromptAsSystemImage(t *testing.T) {
	payload := openAIResponsesRequest{
		Input: []openAIInputMessage{
			{
				Role: "user",
				Content: []openAIInputContent{
					{Type: "input_text", Text: buildOCRPrompt([]string{"Meals", "Office"})},
					{Type: "input_image", ImageURL: "https://example.test/receipt.webp", Detail: "low"},
				},
			},
		},
	}

	if !rewriteOCRPromptAsSystemImage(&payload) {
		t.Fatal("expected OCR request to be rewritten")
	}
	if len(payload.Input) != 2 {
		t.Fatalf("expected system and user messages, got %d", len(payload.Input))
	}
	if payload.Input[0].Role != "system" {
		t.Fatalf("expected system role, got %q", payload.Input[0].Role)
	}
	if len(payload.Input[0].Content) != 2 {
		t.Fatalf("expected prompt image and category text, got %d parts", len(payload.Input[0].Content))
	}
	promptImage := payload.Input[0].Content[0]
	if promptImage.Type != "input_image" || promptImage.Detail != "low" {
		t.Fatalf("expected low-detail system image, got %#v", promptImage)
	}
	if promptImage.ImageURL == "" || len(ocrSystemPromptPNG) == 0 {
		t.Fatal("expected embedded OCR prompt image")
	}
	if payload.Input[0].Content[1].Text != "Use these categories when guessing the receipt type: Meals, Office. If none match, respond with null for the `category` key." {
		t.Fatalf("unexpected category instruction: %q", payload.Input[0].Content[1].Text)
	}
	if payload.Input[1].Role != "user" || len(payload.Input[1].Content) != 1 || payload.Input[1].Content[0].Type != "input_image" {
		t.Fatalf("expected receipt image-only user message, got %#v", payload.Input[1])
	}
}
