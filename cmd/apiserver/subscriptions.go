package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

func (s *apiServer) applySubscriptionPayload(ownerEmail string, payload map[string]interface{}) (string, error) {
	now := time.Now().UTC()
	entry := flattenSubscriptionPayload(payload)
	planID := s.resolvePlanFromPayment(entry["paymentPlanId"])
	plan, ok := s.getPlan(planID)
	if !ok {
		return "", httpError{status: http.StatusInternalServerError, detail: fmt.Sprintf("Subscription plan %s is not defined", planID)}
	}
	start := parseHelcimDate(stringValue(entry["dateActivated"]))
	if start == nil {
		start = &now
	}
	interval := planIntervalValue(plan)
	var end interface{} = nil
	if interval == "month" {
		billing := parseHelcimDate(stringValue(entry["dateBilling"]))
		if billing != nil {
			end = *billing
		} else {
			end = start.Add(30 * 24 * time.Hour)
		}
	}
	planName := strings.TrimSpace(stringValue(plan["name"]))
	paymentPlanID := coerceInt(plan["payment_plan_id"])
	priceCents := coerceInt(plan["price_cents"])
	update := map[string]interface{}{
		"owner_email":            ownerEmail,
		"plan_id":                fallbackString(planName, stringValue(plan["plan_id"])),
		"plan_doc_id":            stringValue(plan["plan_id"]),
		"helcim_payment_plan_id": paymentPlanID,
		"subscription_status":    fallbackString(entry["status"], "active"),
		"plan_interval":          interval,
		"plan_price_cents":       priceCents,
		"current_period_start":   *start,
		"current_period_end":     end,
		"plan_updated_at":        now,
		"last_payment_id":        extractLastPaymentID(entry["payments"]),
	}
	docRef := s.getOrCreateUserRef(ownerEmail)
	if _, err := docRef.Set(requestContext(), update, fs.MergeAll); err != nil {
		return "", err
	}
	log.Printf("apply_subscription_payload owner=%s doc_id=%s stored_plan_id=%s", ownerEmail, docRef.ID, stringValue(plan["plan_id"]))
	return stringValue(plan["plan_id"]), nil
}

func (s *apiServer) resolvePlanFromPayment(paymentPlanID interface{}) string {
	coerced := coerceInt(paymentPlanID)
	if coerced == nil {
		return defaultPlanID
	}
	plan, ok := s.findPlanByPaymentPlanID(*coerced)
	if ok {
		return stringValue(plan["plan_id"])
	}
	return defaultPlanID
}

func (s *apiServer) findPlanByPaymentPlanID(paymentPlanID int) (map[string]interface{}, bool) {
	iter := s.plans.Where("payment_plan_id", "==", paymentPlanID).Limit(1).Documents(requestContext())
	defer iter.Stop()
	snapshot, err := iter.Next()
	if err != nil || !snapshot.Exists() {
		return nil, false
	}
	data := snapshot.Data()
	data["plan_id"] = snapshot.Ref.ID
	return data, true
}

func (s *apiServer) resolveActivationPlan(planID string, paymentPlanID *int) (map[string]interface{}, bool) {
	if paymentPlanID != nil {
		if plan, ok := s.findPlanByPaymentPlanID(*paymentPlanID); ok {
			return plan, true
		}
	}
	if strings.TrimSpace(planID) != "" {
		if plan, ok := s.findPlanByDocumentID(planID); ok {
			return plan, true
		}
		if plan, ok := s.findPlanByPlanIDField(planID); ok {
			return plan, true
		}
		if plan, ok := s.findPlanByName(planID); ok {
			return plan, true
		}
	}
	return nil, false
}

func (s *apiServer) findOwnerByCustomerCode(customerCode string) string {
	normalized := strings.TrimSpace(customerCode)
	if normalized == "" {
		return ""
	}
	iter := s.users.Where("helcim_customer_code", "==", normalized).Limit(1).Documents(requestContext())
	defer iter.Stop()
	snapshot, err := iter.Next()
	if err != nil || !snapshot.Exists() {
		return ""
	}
	return strings.TrimSpace(stringValue(snapshot.Data()["owner_email"]))
}

func (s *apiServer) setOwnerCustomerCode(ownerEmail, customerCode string) error {
	normalized := strings.TrimSpace(customerCode)
	if normalized == "" {
		return httpError{status: http.StatusBadRequest, detail: "customerCode is required"}
	}
	docRef := s.getOrCreateUserRef(ownerEmail)
	_, err := docRef.Set(requestContext(), map[string]interface{}{
		"owner_email":              ownerEmail,
		"helcim_customer_code":     normalized,
		"customer_code_updated_at": time.Now().UTC(),
	}, fs.MergeAll)
	return err
}

func (s *apiServer) storePaymentMethodRegistration(ownerEmail, customerCode, cardToken string, transactionID *int, approvedAt *time.Time) error {
	normalized := strings.ToLower(strings.TrimSpace(ownerEmail))
	if normalized == "" {
		return httpError{status: http.StatusBadRequest, detail: "owner email is required"}
	}
	update := map[string]interface{}{
		"owner_email":               normalized,
		"payment_method_ready":      true,
		"payment_method_status":     "verified",
		"payment_method_updated_at": time.Now().UTC(),
	}
	if code := strings.TrimSpace(customerCode); code != "" {
		update["helcim_customer_code"] = code
		update["customer_code_updated_at"] = time.Now().UTC()
	}
	if token := strings.TrimSpace(cardToken); token != "" {
		update["helcim_card_token"] = token
	}
	if transactionID != nil {
		update["last_transaction_id"] = *transactionID
	}
	if approvedAt != nil {
		update["payment_method_verified_at"] = *approvedAt
	}
	docRef := s.getOrCreateUserRef(normalized)
	_, err := docRef.Set(requestContext(), update, fs.MergeAll)
	return err
}

func (s *apiServer) getOrCreateUserRef(ownerEmail string) *fs.DocumentRef {
	userRef, _ := s.findOrChooseUserDoc(ownerEmail)
	if userRef != nil {
		return userRef
	}
	docRef := s.users.NewDoc()
	_, _ = docRef.Set(requestContext(), map[string]interface{}{"owner_email": ownerEmail}, fs.MergeAll)
	return docRef
}

func flattenSubscriptionPayload(payload map[string]interface{}) map[string]interface{} {
	if data, ok := payload["data"].(map[string]interface{}); ok {
		return data
	}
	if list, ok := payload["data"].([]interface{}); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]interface{}); ok {
			return first
		}
	}
	return payload
}

func parseHelcimDate(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, "0000") {
		return nil
	}
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02", time.RFC3339}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func extractLastPaymentID(raw interface{}) interface{} {
	payments, ok := raw.([]interface{})
	if !ok || len(payments) == 0 {
		return nil
	}
	last, ok := payments[len(payments)-1].(map[string]interface{})
	if !ok {
		return nil
	}
	return coerceInt(last["id"])
}

func firstPaymentPlanEntry(payload interface{}) (map[string]interface{}, bool) {
	typed, ok := payload.(map[string]interface{})
	if !ok {
		return nil, false
	}
	data, ok := typed["data"].([]interface{})
	if !ok || len(data) == 0 {
		return nil, false
	}
	first, ok := data[0].(map[string]interface{})
	return first, ok
}

func toQueryMap(values map[string][]string) map[string][]string {
	result := map[string][]string{}
	for key, entries := range values {
		if len(entries) == 0 {
			continue
		}
		result[key] = append([]string{}, entries...)
	}
	return result
}

func iterateDocs(iter *fs.DocumentIterator) ([]*fs.DocumentSnapshot, error) {
	defer iter.Stop()
	var result []*fs.DocumentSnapshot
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
}
