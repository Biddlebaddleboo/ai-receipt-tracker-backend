package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type activationPayload struct {
	PlanID        string `json:"plan_id"`
	PaymentPlanID *int   `json:"payment_plan_id"`
}

func (s *apiServer) handleBilling(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/billing")
	if path == "" {
		path = "/"
	}
	if isPublicBillingCallback(request.URL.Path) {
		s.handlePublicBillingCallback(writer, request, path)
		return
	}

	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	switch {
	case path == "/subscriptions/activate" && request.Method == http.MethodPost:
		s.activateSubscription(writer, request, user.Email)
	default:
		writeJSONError(writer, http.StatusNotFound, "Not found")
	}
}

func (s *apiServer) handlePublicBillingCallback(writer http.ResponseWriter, request *http.Request, path string) {
	switch {
	case path == "/helcim/approval" && request.Method == http.MethodGet:
		s.handleHelcimApprovalLanding(writer, request)
	case path == "/helcim/approval" && request.Method == http.MethodPost:
		s.handleHelcimApproval(writer, request)
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *apiServer) handleHelcimApprovalLanding(writer http.ResponseWriter, request *http.Request) {
	payload := map[string]interface{}{}
	for key, values := range request.URL.Query() {
		if len(values) > 0 {
			payload[key] = values[0]
		}
	}
	if err := s.validateApprovalAuth(payload, request); err != nil {
		s.writeErr(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("<html><body><h1>Success!</h1><p>" + html.EscapeString("Your payment method has been saved. You may now close this window and refresh the page to purchase the plan.") + "</p></body></html>"))
}

func (s *apiServer) handleHelcimApproval(writer http.ResponseWriter, request *http.Request) {
	payload, err := extractCallbackPayload(request)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	if err := s.validateApprovalAuth(payload, request); err != nil {
		s.writeErr(writer, err)
		return
	}
	result, err := s.processApprovalPayload(payload)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	ownerEmail := resolveOwnerEmailForCallback(payload)
	paymentMethodSaved := false
	if ownerEmail != "" {
		var txID *int
		if val := coerceInt(result["transaction_id"]); val != nil {
			txID = val
		}
		if err := s.storePaymentMethodRegistration(
			ownerEmail,
			stringValue(result["customer_code"]),
			stringValue(result["card_token"]),
			txID,
			parseRFC3339(result["approved_at"]),
		); err == nil {
			paymentMethodSaved = true
		}
	}
	result["status"] = "ok"
	result["owner_email"] = ownerEmail
	result["payment_method_saved"] = paymentMethodSaved
	writeJSON(writer, http.StatusOK, result)
}

func (s *apiServer) activateSubscription(writer http.ResponseWriter, request *http.Request, ownerEmail string) {
	payload := activationPayload{}
	defer request.Body.Close()
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(payload.PlanID) == "" && payload.PaymentPlanID == nil {
		writeJSONError(writer, http.StatusBadRequest, "plan_id or payment_plan_id is required")
		return
	}
	plan, ok := s.resolveActivationPlan(payload.PlanID, payload.PaymentPlanID)
	if !ok {
		writeJSONError(writer, http.StatusNotFound, "Plan not found")
		return
	}
	paymentPlanID := coerceInt(plan["payment_plan_id"])
	if paymentPlanID == nil {
		writeJSONError(writer, http.StatusBadRequest, "Plan is missing payment_plan_id")
		return
	}
	_, userDoc := s.findOrChooseUserDoc(ownerEmail)
	customerCode := strings.TrimSpace(stringValue(userDoc["helcim_customer_code"]))
	cardToken := strings.TrimSpace(stringValue(userDoc["helcim_card_token"]))
	if customerCode == "" && cardToken == "" {
		writeJSONError(writer, http.StatusBadRequest, "No saved Helcim customer or card token available for this user")
		return
	}
	if customerCode == "" {
		writeJSONError(writer, http.StatusBadRequest, "Saved payment method is missing Helcim customer code")
		return
	}

	body := map[string]interface{}{
		"paymentPlanId": *paymentPlanID,
		"customerCode":  customerCode,
		"dateActivated": time.Now().UTC().Format("2006-01-02"),
	}
	if price := coerceInt(plan["price_cents"]); price != nil {
		body["recurringAmount"] = float64(*price) / 100.0
	}
	paymentMethod := strings.ToLower(strings.TrimSpace(stringValue(plan["paymentMethod"])))
	if paymentMethod == "" && cardToken != "" {
		paymentMethod = "card"
	}
	if paymentMethod != "" {
		body["paymentMethod"] = paymentMethod
	}
	requestPayload := map[string]interface{}{"subscriptions": []map[string]interface{}{body}}
	resp, err := s.helcim.request(http.MethodPost, "subscriptions", nil, requestPayload, uuid.NewString())
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	respMap, ok := resp.(map[string]interface{})
	if !ok {
		writeJSONError(writer, http.StatusBadGateway, "Unexpected Helcim subscription response format")
		return
	}
	planID, err := s.applySubscriptionPayload(ownerEmail, respMap)
	if err != nil {
		s.writeErr(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"plan_id":      planID,
		"plan_name":    plan["name"],
		"subscription": respMap,
	})
}

func (s *apiServer) processApprovalPayload(payload map[string]interface{}) (map[string]interface{}, error) {
	responseFlag := strings.TrimSpace(stringValue(payload["response"]))
	if responseFlag != "" && responseFlag != "1" {
		return nil, httpError{status: http.StatusBadRequest, detail: "Helcim response indicates failure"}
	}
	responseMessage := strings.ToUpper(strings.TrimSpace(stringValue(payload["responseMessage"])))
	if responseMessage != "" && responseMessage != "APPROVAL" {
		return nil, httpError{status: http.StatusBadRequest, detail: fmt.Sprintf("Helcim responseMessage is not APPROVAL (%s)", responseMessage)}
	}
	transactionID := coerceInt(payload["transactionId"])
	if transactionID == nil {
		return nil, httpError{status: http.StatusBadRequest, detail: "transactionId is required"}
	}
	txResp, err := s.helcim.request(http.MethodGet, fmt.Sprintf("card-transactions/%d", *transactionID), nil, nil, "")
	if err != nil {
		return nil, err
	}
	transactionPayload := extractTransactionPayload(txResp)
	customerCode := firstNonEmpty(
		payload["customerCode"],
		payload["customerId"],
		transactionPayload["customerCode"],
		transactionPayload["customerId"],
	)
	cardToken := firstNonEmpty(payload["cardToken"], transactionPayload["cardToken"])
	transactionType := firstNonEmpty(payload["type"], transactionPayload["type"])
	approvalCode := firstNonEmpty(payload["approvalCode"], transactionPayload["approvalCode"])
	amount := transactionPayload["amount"]
	currency := transactionPayload["currency"]
	paymentPlanID := firstNonEmpty(payload["paymentPlanId"], transactionPayload["paymentPlanId"], transactionPayload["paymentPlanID"])
	approvedAt := parseApprovalTimestamp(payload, transactionPayload)
	return map[string]interface{}{
		"transaction_id":  *transactionID,
		"customer_code":   customerCode,
		"card_token":      cardToken,
		"type":            transactionType,
		"approval_code":   approvalCode,
		"amount":          amount,
		"currency":        currency,
		"payment_plan_id": paymentPlanID,
		"approved_at":     isoTimeOrNil(approvedAt),
		"plan_activated":  false,
	}, nil
}

func (s *apiServer) validateApprovalAuth(payload map[string]interface{}, request *http.Request) error {
	configured := strings.TrimSpace(s.helcim.approvalSecret)
	if configured == "" {
		return nil
	}
	provided := strings.TrimSpace(request.URL.Query().Get("secret"))
	if provided == "" {
		provided = strings.TrimSpace(request.Header.Get("X-Helcim-Approval-Secret"))
	}
	if provided != "" {
		if provided == configured {
			return nil
		}
		return httpError{status: http.StatusUnauthorized, detail: "Invalid approval secret"}
	}
	if strings.TrimSpace(stringValue(payload["transactionId"])) != "" {
		return nil
	}
	return httpError{status: http.StatusUnauthorized, detail: "Approval callback is missing transactionId"}
}

func extractCallbackPayload(request *http.Request) (map[string]interface{}, error) {
	contentType := strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Type")))
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if strings.Contains(contentType, "application/json") {
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err == nil && len(payload) > 0 {
			return payload, nil
		}
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") || strings.Contains(contentType, "multipart/form-data") {
		values, err := url.ParseQuery(string(body))
		if err == nil {
			result := map[string]interface{}{}
			for key, entries := range values {
				if len(entries) == 1 {
					result[key] = entries[0]
				} else if len(entries) > 1 {
					result[key] = entries
				}
			}
			if len(result) > 0 {
				return result, nil
			}
		}
	}
	values, err := url.ParseQuery(string(body))
	if err == nil && len(values) > 0 {
		result := map[string]interface{}{}
		for key, entries := range values {
			if len(entries) > 0 {
				result[key] = entries[0]
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	for key, entries := range request.URL.Query() {
		if len(entries) > 0 {
			if values == nil {
				values = url.Values{}
			}
			values[key] = entries
		}
	}
	if len(values) > 0 {
		result := map[string]interface{}{}
		for key, entries := range values {
			result[key] = entries[0]
		}
		return result, nil
	}
	return nil, httpError{status: http.StatusBadRequest, detail: "Approval callback payload is missing"}
}

func extractTransactionPayload(raw interface{}) map[string]interface{} {
	typed, ok := raw.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	if data, ok := typed["data"].(map[string]interface{}); ok {
		return data
	}
	if data, ok := typed["data"].([]interface{}); ok && len(data) > 0 {
		if first, ok := data[0].(map[string]interface{}); ok {
			return first
		}
	}
	return typed
}

func parseApprovalTimestamp(callback map[string]interface{}, tx map[string]interface{}) *time.Time {
	if dateCreated := strings.TrimSpace(stringValue(tx["dateCreated"])); dateCreated != "" {
		if parsed := parseHelcimDate(dateCreated); parsed != nil {
			return parsed
		}
	}
	datePart := strings.TrimSpace(stringValue(callback["date"]))
	timePart := strings.TrimSpace(stringValue(callback["time"]))
	if datePart != "" {
		candidates := []string{
			datePart + " " + timePart,
			datePart,
		}
		for _, candidate := range candidates {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "01/02/2006"} {
				if parsed, err := time.Parse(layout, candidate); err == nil {
					utc := parsed.UTC()
					return &utc
				}
			}
		}
	}
	return nil
}

func resolveOwnerEmailForCallback(payload map[string]interface{}) string {
	fields := []string{"billingEmailAddress", "email", "emailAddress", "customerEmail", "shippingEmailAddress"}
	for _, field := range fields {
		value := strings.ToLower(strings.TrimSpace(stringValue(payload[field])))
		if value != "" {
			return value
		}
	}
	return ""
}

func parseRFC3339(value interface{}) *time.Time {
	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func firstNonEmpty(values ...interface{}) interface{} {
	for _, value := range values {
		if strings.TrimSpace(stringValue(value)) != "" {
			return value
		}
	}
	return nil
}

func isoTimeOrNil(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return value.Format(time.RFC3339)
}
