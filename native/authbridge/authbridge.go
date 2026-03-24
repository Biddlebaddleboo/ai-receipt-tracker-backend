package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unsafe"

	"google.golang.org/api/idtoken"
)

type verifiedUser struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Audience string `json:"audience,omitempty"`
}

func main() {}

func setError(errOut **C.char, err error) {
	if errOut == nil || err == nil {
		return
	}
	*errOut = C.CString(err.Error())
}

func decodeStringList(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var payload []string
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(payload))
	values := make([]string, 0, len(payload))
	for _, entry := range payload {
		cleaned := strings.TrimSpace(entry)
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		values = append(values, cleaned)
	}
	return values, nil
}

func claimString(claims map[string]interface{}, key string) string {
	value, ok := claims[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func containsFold(values []string, candidate string) bool {
	normalized := strings.ToLower(strings.TrimSpace(candidate))
	if normalized == "" {
		return false
	}
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == normalized {
			return true
		}
	}
	return false
}

func marshalJSON(value interface{}) (*C.char, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return C.CString(string(body)), nil
}

//export AuthVerifyIDToken
func AuthVerifyIDToken(token *C.char, audiencesJSON *C.char, allowedDomainsJSON *C.char, errOut **C.char) *C.char {
	tokenValue := strings.TrimSpace(C.GoString(token))
	if tokenValue == "" {
		setError(errOut, fmt.Errorf("missing_token"))
		return nil
	}

	audiences, err := decodeStringList(C.GoString(audiencesJSON))
	if err != nil {
		setError(errOut, fmt.Errorf("invalid_audiences:%w", err))
		return nil
	}
	if len(audiences) == 0 {
		setError(errOut, fmt.Errorf("missing_audience"))
		return nil
	}

	allowedDomains, err := decodeStringList(C.GoString(allowedDomainsJSON))
	if err != nil {
		setError(errOut, fmt.Errorf("invalid_allowed_domains:%w", err))
		return nil
	}

	ctx := context.Background()
	var payload *idtoken.Payload
	var matchedAudience string
	var lastErr error

	for _, audience := range audiences {
		payload, lastErr = idtoken.Validate(ctx, tokenValue, audience)
		if lastErr == nil {
			matchedAudience = audience
			break
		}
	}

	if payload == nil {
		if lastErr != nil {
			setError(errOut, fmt.Errorf("invalid_token:%w", lastErr))
		} else {
			setError(errOut, fmt.Errorf("invalid_token"))
		}
		return nil
	}

	email := claimString(payload.Claims, "email")
	if email == "" {
		setError(errOut, fmt.Errorf("missing_email"))
		return nil
	}

	if len(allowedDomains) > 0 {
		hostedDomain := claimString(payload.Claims, "hd")
		if !containsFold(allowedDomains, hostedDomain) {
			setError(errOut, fmt.Errorf("forbidden_domain"))
			return nil
		}
	}

	resultPtr, err := marshalJSON(verifiedUser{
		Iss:      strings.TrimSpace(payload.Issuer),
		Sub:      strings.TrimSpace(payload.Subject),
		Email:    email,
		Name:     claimString(payload.Claims, "name"),
		Audience: matchedAudience,
	})
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return resultPtr
}

//export AuthFree
func AuthFree(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}
