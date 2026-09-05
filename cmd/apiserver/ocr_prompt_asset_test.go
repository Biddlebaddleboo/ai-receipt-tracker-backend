package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestEmbeddedOCRPromptWebPIsComplete(t *testing.T) {
	data := ocrSystemPromptWebP
	if len(data) < 12 {
		t.Fatalf("embedded OCR prompt WebP is too short: %d bytes", len(data))
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatal("embedded OCR prompt is not a RIFF/WEBP container")
	}

	expectedSize := int(binary.LittleEndian.Uint32(data[4:8])) + 8
	if len(data) != expectedSize {
		t.Fatalf("truncated OCR prompt WebP: header expects %d bytes, embedded asset has %d", expectedSize, len(data))
	}
	if !strings.HasPrefix(ocrSystemPromptDataURL, "data:image/webp;base64,") {
		t.Fatalf("unexpected OCR prompt data URL prefix")
	}
}
