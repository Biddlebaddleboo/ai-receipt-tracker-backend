package main

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"net/http"

	"golang.org/x/image/webp"
)

const (
	aiReceiptJPEGQuality = 90
	aiReceiptMaxPixels   = 40_000_000
)

func (s *apiServer) writeAIReceiptImageAsJPEG(writer http.ResponseWriter, ctx context.Context, storagePath string) error {
	configReader, err := s.bucket.Object(storagePath).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open receipt image: %w", err)
	}
	config, configErr := webp.DecodeConfig(configReader)
	_ = configReader.Close()
	if configErr != nil {
		return fmt.Errorf("failed to inspect receipt image: %w", configErr)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > aiReceiptMaxPixels {
		return httpError{status: http.StatusUnprocessableEntity, detail: "Receipt image dimensions are too large to convert safely"}
	}

	reader, err := s.bucket.Object(storagePath).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open receipt image: %w", err)
	}
	defer reader.Close()

	imageValue, err := webp.Decode(reader)
	if err != nil {
		return fmt.Errorf("failed to decode receipt image: %w", err)
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageValue, &jpeg.Options{Quality: aiReceiptJPEGQuality}); err != nil {
		return fmt.Errorf("failed to encode receipt image as JPEG: %w", err)
	}

	writer.Header().Set("Content-Type", "image/jpeg")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded.Bytes())
	return nil
}
