package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const defaultPlanID = "free"

type userDocCandidate struct {
	score    userDocScore
	snapshot *fs.DocumentSnapshot
	data     map[string]interface{}
}

type userDocScore struct {
	nonFree         int
	hasCustomerCode int
	hasPeriodEnd    int
	updatedUnix     float64
}

func (s *apiServer) ensureWithinLimit(ownerEmail string) (map[string]interface{}, error) {
	now := time.Now().UTC()
	userRef, userDoc := s.getOrCreateUser(ownerEmail, now)
	planID := fallbackString(userDoc["plan_id"], defaultPlanID)
	plan, ok := s.getPlan(planID)
	if !ok {
		return nil, fmt.Errorf("Subscription plan %s is not defined", planID)
	}
	limit := planLimit(plan)
	if limit == nil {
		return userDoc, nil
	}
	planLabel := strings.ToLower(strings.TrimSpace(fallbackString(plan["name"], planID)))
	planIDLower := strings.ToLower(strings.TrimSpace(planID))

	// Pro plan has no enforced receipt metadata count checks.
	if strings.Contains(planLabel, "pro") || planIDLower == "pro" {
		return userDoc, nil
	}

	// Free plan: enforce total metadata-count cap across all time.
	if strings.Contains(planLabel, "free") || planIDLower == "free" || planIDLower == "" {
		count, err := s.countReceiptsByOwner(ownerEmail, nil, nil)
		if err != nil {
			return nil, err
		}
		if count >= *limit {
			return nil, paymentRequiredError(
				fmt.Sprintf(
					"Plan %s hard limit reached (%d total receipts)",
					fallbackString(plan["name"], fmt.Sprint(plan["plan_id"])),
					*limit,
				),
			)
		}
		return userDoc, nil
	}

	// Plus plan (and any non-free/non-pro plans): enforce within current billing period only.
	start := timeValue(userDoc["current_period_start"])
	end := timeValue(userDoc["current_period_end"])
	if start == nil || end == nil || !now.Before(*end) {
		newStart, newEnd := periodBounds(now)
		if userRef != nil {
			_, err := userRef.Update(
				requestContext(),
				[]fs.Update{
					{Path: "current_period_start", Value: newStart},
					{Path: "current_period_end", Value: newEnd},
					{Path: "receipt_count_updated_at", Value: now},
				},
			)
			if err != nil {
				return nil, err
			}
		}
		userDoc["current_period_start"] = newStart
		userDoc["current_period_end"] = newEnd
		start = &newStart
		end = &newEnd
	}
	count, err := s.countReceiptsByOwner(ownerEmail, start, end)
	if err != nil {
		return nil, err
	}
	if count >= *limit {
		return nil, paymentRequiredError(
			fmt.Sprintf(
				"Plan %s limit reached (%d receipts in current billing period)",
				fallbackString(plan["name"], fmt.Sprint(plan["plan_id"])),
				*limit,
			),
		)
	}
	return userDoc, nil
}

func (s *apiServer) countReceiptsByOwner(ownerEmail string, start *time.Time, end *time.Time) (int, error) {
	count := 0

	shardedQuery := s.receipts.
		Where(receiptShardSchemaField, "==", receiptShardSchema).
		Where("owner_email", "==", ownerEmail)
	shardedIter := shardedQuery.Documents(requestContext())
	defer shardedIter.Stop()
	for {
		snapshot, err := shardedIter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return 0, err
		}
		metadataMap, _ := snapshot.Data()[receiptShardMetadataField].(map[string]interface{})
		if len(metadataMap) == 0 {
			continue
		}
		if start == nil && end == nil {
			count += len(metadataMap)
			continue
		}
		for _, raw := range metadataMap {
			metadata, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			created := timeValue(metadata["created_at"])
			if created == nil {
				continue
			}
			if start != nil && created.Before(*start) {
				continue
			}
			if end != nil && !created.Before(*end) {
				continue
			}
			count++
		}
	}
	return count, nil
}

func (s *apiServer) getOrCreateUser(ownerEmail string, now time.Time) (*fs.DocumentRef, map[string]interface{}) {
	userRef, userDoc := s.findOrChooseUserDoc(ownerEmail)
	if userRef != nil {
		return userRef, userDoc
	}
	start, end := periodBounds(now)
	data := map[string]interface{}{
		"owner_email":          ownerEmail,
		"plan_id":              defaultPlanID,
		"subscription_status":  "active",
		"current_period_start": start,
		"current_period_end":   end,
		"plan_updated_at":      now,
	}
	docRef := s.users.NewDoc()
	_, err := docRef.Set(requestContext(), data)
	if err != nil {
		return nil, data
	}
	return docRef, data
}

func (s *apiServer) findOrChooseUserDoc(ownerEmail string) (*fs.DocumentRef, map[string]interface{}) {
	iter := s.users.Where("owner_email", "==", ownerEmail).Documents(requestContext())
	defer iter.Stop()
	candidates := make([]userDocCandidate, 0)
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, nil
		}
		if !snapshot.Exists() {
			continue
		}
		data := snapshot.Data()
		candidates = append(candidates, userDocCandidate{
			score:    scoreUserDoc(data),
			snapshot: snapshot,
			data:     data,
		})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) == 1 {
		candidate := candidates[0]
		return candidate.snapshot.Ref, candidate.data
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return compareUserDocScore(candidates[i].score, candidates[j].score)
	})
	chosen := candidates[0]
	return chosen.snapshot.Ref, chosen.data
}

func scoreUserDoc(data map[string]interface{}) userDocScore {
	planID := strings.ToLower(strings.TrimSpace(stringValue(data["plan_id"])))
	score := userDocScore{}
	if planID != "" && planID != "free" {
		score.nonFree = 1
	}
	if strings.TrimSpace(stringValue(data["helcim_customer_code"])) != "" {
		score.hasCustomerCode = 1
	}
	if timeValue(data["current_period_end"]) != nil {
		score.hasPeriodEnd = 1
	}
	if updated := timeValue(data["plan_updated_at"]); updated != nil {
		score.updatedUnix = float64(updated.Unix())
	}
	return score
}

func compareUserDocScore(a, b userDocScore) bool {
	if a.nonFree != b.nonFree {
		return a.nonFree > b.nonFree
	}
	if a.hasCustomerCode != b.hasCustomerCode {
		return a.hasCustomerCode > b.hasCustomerCode
	}
	if a.hasPeriodEnd != b.hasPeriodEnd {
		return a.hasPeriodEnd > b.hasPeriodEnd
	}
	return a.updatedUnix > b.updatedUnix
}

func (s *apiServer) getPlan(planID string) (map[string]interface{}, bool) {
	if plan, ok := s.findPlanByName(planID); ok {
		return plan, true
	}
	if plan, ok := s.findPlanByDocumentID(planID); ok {
		return plan, true
	}
	if plan, ok := s.findPlanByPlanIDField(planID); ok {
		return plan, true
	}
	return nil, false
}

func (s *apiServer) findPlanByDocumentID(planID string) (map[string]interface{}, bool) {
	snapshot, err := s.plans.Doc(planID).Get(requestContext())
	if err != nil || !snapshot.Exists() {
		return nil, false
	}
	data := snapshot.Data()
	data["plan_id"] = snapshot.Ref.ID
	return data, true
}

func (s *apiServer) findPlanByPlanIDField(planID string) (map[string]interface{}, bool) {
	iter := s.plans.Where("plan_id", "==", planID).Limit(1).Documents(requestContext())
	defer iter.Stop()
	snapshot, err := iter.Next()
	if err != nil || !snapshot.Exists() {
		return nil, false
	}
	data := snapshot.Data()
	data["plan_id"] = snapshot.Ref.ID
	return data, true
}

func (s *apiServer) findPlanByName(planName string) (map[string]interface{}, bool) {
	normalized := strings.ToLower(strings.TrimSpace(planName))
	if normalized == "" {
		return nil, false
	}
	iter := s.plans.Documents(requestContext())
	defer iter.Stop()
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, false
		}
		if !snapshot.Exists() {
			continue
		}
		data := snapshot.Data()
		if strings.ToLower(strings.TrimSpace(stringValue(data["name"]))) == normalized {
			data["plan_id"] = snapshot.Ref.ID
			return data, true
		}
	}
	return nil, false
}

func defaultPlanStub(planID string) map[string]interface{} {
	name := strings.TrimSpace(planID)
	if name == "" {
		name = defaultPlanID
	}
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
	}
	return map[string]interface{}{
		"plan_id":     planID,
		"name":        name,
		"description": "",
		"features":    []string{},
	}
}

func periodBounds(now time.Time) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if now.Month() == time.December {
		return start, time.Date(now.Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return start, time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

func planIntervalValue(plan map[string]interface{}) string {
	raw := strings.ToLower(strings.TrimSpace(stringValue(plan["interval"])))
	switch raw {
	case "", "month", "monthly":
		return "month"
	case "once", "one_time", "onetime":
		return "once"
	default:
		return raw
	}
}

func planLimit(plan map[string]interface{}) *int {
	value := plan["monthly_limit"]
	if value == nil {
		value = plan["receipt_limit"]
	}
	return coerceInt(value)
}

func planFeatures(plan map[string]interface{}) []string {
	raw, ok := plan["features"].([]interface{})
	if !ok {
		return []string{}
	}
	features := make([]string, 0, len(raw))
	for _, entry := range raw {
		text := strings.TrimSpace(fmt.Sprint(entry))
		if text != "" {
			features = append(features, text)
		}
	}
	return features
}

func coerceInt(value interface{}) *int {
	switch typed := value.(type) {
	case int:
		return &typed
	case int32:
		converted := int(typed)
		return &converted
	case int64:
		converted := int(typed)
		return &converted
	case float64:
		converted := int(typed)
		return &converted
	case nil:
		return nil
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "" {
			return nil
		}
		var converted int
		_, err := fmt.Sscanf(text, "%d", &converted)
		if err != nil {
			return nil
		}
		return &converted
	}
}

func timeValue(value interface{}) *time.Time {
	typed, ok := value.(time.Time)
	if !ok {
		return nil
	}
	utc := typed.UTC()
	return &utc
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func fallbackString(value interface{}, fallback string) string {
	text := stringValue(value)
	if text == "" {
		return fallback
	}
	return text
}

func isoOrNil(value interface{}) interface{} {
	if typed := timeValue(value); typed != nil {
		return typed.Format(time.RFC3339)
	}
	return nil
}

func docIDOrNil(ref *fs.DocumentRef) interface{} {
	if ref == nil {
		return nil
	}
	return ref.ID
}

func paymentRequiredError(detail string) error {
	return httpError{status: http.StatusPaymentRequired, detail: detail}
}

func userHasPaymentMethod(userDoc map[string]interface{}) bool {
	if userDoc == nil {
		return false
	}
	if ready, ok := userDoc["payment_method_ready"].(bool); ok && ready {
		return true
	}
	if token := strings.TrimSpace(stringValue(userDoc["helcim_card_token"])); token != "" {
		return true
	}
	return false
}
