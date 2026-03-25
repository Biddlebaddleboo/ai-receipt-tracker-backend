package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type helcimClient struct {
	token          string
	baseURL        string
	timeout        time.Duration
	userAgent      string
	approvalSecret string
}

func newHelcimClient(cfg config) *helcimClient {
	timeoutSeconds := envOrDefault("HELCIM_TIMEOUT_SECONDS", "20")
	timeout := 20 * time.Second
	if parsed, err := time.ParseDuration(strings.TrimSpace(timeoutSeconds) + "s"); err == nil {
		timeout = parsed
	}
	userAgent := strings.TrimSpace(os.Getenv("HELCIM_USER_AGENT"))
	if userAgent == "" {
		userAgent = "ai-receipt-tracker-backend/1.0"
	}
	baseURL := envOrDefault("HELCIM_API_BASE_URL", "https://api.helcim.com/v2")
	return &helcimClient{
		token:          strings.TrimSpace(os.Getenv("HELCIM_API_TOKEN")),
		baseURL:        strings.TrimRight(baseURL, "/"),
		timeout:        timeout,
		userAgent:      userAgent,
		approvalSecret: strings.TrimSpace(os.Getenv("HELCIM_APPROVAL_SECRET")),
	}
}

func (c *helcimClient) request(method, path string, query map[string][]string, payload interface{}, idempotencyKey string) (interface{}, error) {
	if c.token == "" {
		return nil, httpError{status: http.StatusInternalServerError, detail: "HELCIM_API_TOKEN is not configured"}
	}
	fullURL := c.baseURL + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		values := url.Values{}
		for key, entries := range query {
			for _, entry := range entries {
				trimmed := strings.TrimSpace(entry)
				if trimmed != "" {
					values.Add(key, trimmed)
				}
			}
		}
		if encoded := values.Encode(); encoded != "" {
			fullURL += "?" + encoded
		}
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("api-token", c.token)
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	log.Printf("helcim_request method=%s path=%s payload=%v idempotency_key=%s", method, path, payload, idempotencyKey)
	client := &http.Client{Timeout: c.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, httpError{status: http.StatusBadGateway, detail: fmt.Sprintf("Failed to reach Helcim API: %v", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	rawText := strings.TrimSpace(string(raw))
	if resp.StatusCode >= 400 {
		log.Printf("helcim_http_error method=%s path=%s status=%d body=%s", method, path, resp.StatusCode, rawText)
		if rawText == "" {
			return nil, httpError{status: resp.StatusCode, detail: fmt.Sprintf("Helcim API request failed (%d)", resp.StatusCode)}
		}
		var parsed interface{}
		if err := json.Unmarshal(raw, &parsed); err == nil {
			return nil, helcimHTTPError{status: resp.StatusCode, detail: parsed}
		}
		return nil, helcimHTTPError{status: resp.StatusCode, detail: rawText}
	}
	log.Printf("helcim_response method=%s path=%s status=%d body=%s", method, path, resp.StatusCode, rawText)
	if rawText == "" {
		return map[string]interface{}{}, nil
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return map[string]interface{}{"raw": rawText}, nil
	}
	return parsed, nil
}

type helcimHTTPError struct {
	status int
	detail interface{}
}

func (e helcimHTTPError) Error() string {
	return fmt.Sprint(e.detail)
}
