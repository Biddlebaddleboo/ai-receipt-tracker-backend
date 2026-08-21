package main

import (
	"context"
	"net/http"

	fs "cloud.google.com/go/firestore"
)

var verifyGoogleTokenOverride func(ctx context.Context, token string, audiences []string, allowedDomains []string) (*verifiedUser, int, string)

var prepaidFindOrChooseUserDocOverride func(s *apiServer, ownerEmail string) (*fs.DocumentRef, map[string]interface{})

var prepaidGetOwnedReceiptOverride func(s *apiServer, ctx context.Context, receiptID string, ownerEmail string) (ownedReceipt, error)

var prepaidListPurchasesOverride func(s *apiServer, ctx context.Context, ownerEmail string, state string) ([]prepaidPurchaseRecord, error)

var prepaidGetPurchaseOverride func(s *apiServer, ctx context.Context, purchaseID string, ownerEmail string) (prepaidPurchaseRecord, error)

var prepaidCreatePurchaseOverride func(s *apiServer, ctx context.Context, user *verifiedUser, payload prepaidCreatePurchaseRequest) (prepaidPurchaseRecord, error)

var prepaidAddActivationReceiptOverride func(s *apiServer, ctx context.Context, user *verifiedUser, purchaseID string, payload prepaidActivationReceiptInput) (prepaidPurchaseRecord, error)

var prepaidAddCardOverride func(s *apiServer, ctx context.Context, user *verifiedUser, purchaseID string, payload prepaidCardInput) (prepaidPurchaseRecord, error)

var prepaidGetCardDetailOverride func(s *apiServer, ctx context.Context, user *verifiedUser, purchaseID string, cardID string) (prepaidCardRecord, error)

var prepaidUpdateCardOverride func(s *apiServer, ctx context.Context, user *verifiedUser, purchaseID string, cardID string, payload prepaidCardInput) (prepaidPurchaseRecord, error)

var prepaidArchiveCardOverride func(s *apiServer, ctx context.Context, user *verifiedUser, purchaseID string, cardID string) (prepaidPurchaseRecord, error)

var signedImageURLOverride func(ctx context.Context, storagePath string) (string, error)

func resetPrepaidTestOverrides() {
	verifyGoogleTokenOverride = nil
	prepaidFindOrChooseUserDocOverride = nil
	prepaidGetOwnedReceiptOverride = nil
	prepaidListPurchasesOverride = nil
	prepaidGetPurchaseOverride = nil
	prepaidCreatePurchaseOverride = nil
	prepaidAddActivationReceiptOverride = nil
	prepaidAddCardOverride = nil
	prepaidGetCardDetailOverride = nil
	prepaidUpdateCardOverride = nil
	prepaidArchiveCardOverride = nil
	signedImageURLOverride = nil
}

func prepaidUnauthorized(detail string) httpError {
	return httpError{status: http.StatusUnauthorized, detail: detail}
}
