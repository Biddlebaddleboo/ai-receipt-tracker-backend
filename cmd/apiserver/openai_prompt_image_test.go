package main

import "testing"

func TestRewriteOCRPromptAsUserImage(t *testing.T) {
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

	if !rewriteOCRPromptAsUserImage(&payload) {
		t.Fatal("expected OCR request to be rewritten")
	}
	if len(payload.Input) != 2 {
		t.Fatalf("expected system and user messages, got %d", len(payload.Input))
	}
	if payload.Input[0].Role != "system" || len(payload.Input[0].Content) != 1 {
		t.Fatalf("expected one short system instruction, got %#v", payload.Input[0])
	}
	if payload.Input[0].Content[0].Type != "input_text" || payload.Input[0].Content[0].Text != ocrSystemInstruction {
		t.Fatalf("unexpected system instruction: %#v", payload.Input[0].Content[0])
	}
	if payload.Input[1].Role != "user" || len(payload.Input[1].Content) != 3 {
		t.Fatalf("expected prompt image, category text, and receipt image, got %#v", payload.Input[1])
	}
	promptImage := payload.Input[1].Content[0]
	if promptImage.Type != "input_image" || promptImage.Detail != "low" {
		t.Fatalf("expected low-detail prompt image, got %#v", promptImage)
	}
	if promptImage.ImageURL == "" || len(ocrSystemPromptWebP) == 0 {
		t.Fatal("expected embedded OCR prompt image")
	}
	if payload.Input[1].Content[1].Text != "Use these categories when guessing the receipt type: Meals, Office. If none match, respond with null for the `category` key." {
		t.Fatalf("unexpected category instruction: %q", payload.Input[1].Content[1].Text)
	}
	receiptImage := payload.Input[1].Content[2]
	if receiptImage.Type != "input_image" || receiptImage.ImageURL != "https://example.test/receipt.webp" || receiptImage.Detail != "low" {
		t.Fatalf("unexpected receipt image: %#v", receiptImage)
	}
}
