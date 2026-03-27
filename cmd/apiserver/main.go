package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/idtoken"
)

type config struct {
	port                string
	firestoreDatabase   string
	firestoreCollection string
	categoriesColl      string
	plansCollection     string
	usersCollection     string
	gcsBucketName       string
	openAIModel         string
	openAIAPIKey        string
	requireOAuth        bool
	oauthClientIDs      []string
	oauthAllowedDomain  []string
	allowedOrigins      []string
	allowedOriginRegex  *regexp.Regexp
	receiptWorkerPoll   time.Duration
	receiptWorkerLease  time.Duration
}

type verifiedUser struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type apiServer struct {
	cfg        config
	helcim     *helcimClient
	firestore  *fs.Client
	storage    *storage.Client
	receipts   *fs.CollectionRef
	categories *fs.CollectionRef
	plans      *fs.CollectionRef
	users      *fs.CollectionRef
	bucket     *storage.BucketHandle
	httpServer *http.Server
	workerID   string
	workerStop chan struct{}
	workerWake chan struct{}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		panic(fmt.Errorf("failed to load config: %w", err))
	}

	server, err := newAPIServer(cfg)
	if err != nil {
		panic(fmt.Errorf("failed to initialize API server: %w", err))
	}
	defer server.close()
	server.startReceiptWorker()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/receipts", server.handleReceipts)
	mux.HandleFunc("/receipts/", server.handleReceiptByID)
	mux.HandleFunc("/billing", server.handleBilling)
	mux.HandleFunc("/billing/", server.handleBilling)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writeJSONError(writer, http.StatusNotFound, "Not found")
	})

	server.httpServer = &http.Server{
		Addr:    ":" + cfg.port,
		Handler: server.withCORS(mux),
	}

	if err := server.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(fmt.Errorf("Go API server stopped unexpectedly: %w", err))
	}
}

func loadConfig() (config, error) {
	cfg := config{
		port:                envOrDefault("PORT", "8080"),
		firestoreDatabase:   envOrDefault("FIRESTORE_DATABASE_ID", "(default)"),
		firestoreCollection: envOrDefault("FIRESTORE_COLLECTION_NAME", "receipts"),
		categoriesColl:      envOrDefault("CATEGORIES_COLLECTION_NAME", "categories"),
		plansCollection:     envOrDefault("PLANS_COLLECTION_NAME", "plans"),
		usersCollection:     envOrDefault("USERS_COLLECTION_NAME", "users"),
		gcsBucketName:       strings.TrimSpace(os.Getenv("GCLOUD_BUCKET_NAME")),
		openAIModel:         envOrDefault("OPENAI_MODEL_NAME", "gpt-4.1-mini"),
		openAIAPIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		requireOAuth:        parseBool(os.Getenv("REQUIRE_OAUTH")),
		oauthClientIDs:      normalizeListField(os.Getenv("OAUTH_CLIENT_ID")),
		oauthAllowedDomain:  normalizeListField(os.Getenv("OAUTH_ALLOWED_DOMAINS")),
		allowedOrigins:      normalizeListField(envOrDefault("ALLOWED_ORIGINS", "http://localhost:3000")),
		receiptWorkerPoll:   time.Duration(parseIntDefault("RECEIPT_WORKER_POLL_SECONDS", 5)) * time.Second,
		receiptWorkerLease:  time.Duration(parseIntDefault("RECEIPT_WORKER_LEASE_SECONDS", 1200)) * time.Second,
	}
	if len(cfg.allowedOrigins) == 0 {
		cfg.allowedOrigins = []string{"http://localhost:3000"}
	}
	regexValue := mergeOriginRegex(os.Getenv("ALLOWED_ORIGIN_REGEX"), buildPreviewRegex(cfg.allowedOrigins))
	if regexValue != "" {
		compiled, err := regexp.Compile(regexValue)
		if err != nil {
			return cfg, fmt.Errorf("invalid allowed origin regex: %w", err)
		}
		cfg.allowedOriginRegex = compiled
	}
	return cfg, nil
}

func newAPIServer(cfg config) (*apiServer, error) {
	ctx := context.Background()
	client, err := fs.NewClientWithDatabase(ctx, fs.DetectProjectID, cfg.firestoreDatabase)
	if err != nil {
		return nil, err
	}
	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return &apiServer{
		cfg:        cfg,
		helcim:     newHelcimClient(cfg),
		firestore:  client,
		storage:    storageClient,
		receipts:   client.Collection(cfg.firestoreCollection),
		categories: client.Collection(cfg.categoriesColl),
		plans:      client.Collection(cfg.plansCollection),
		users:      client.Collection(cfg.usersCollection),
		bucket:     storageClient.Bucket(cfg.gcsBucketName),
		workerID:   buildWorkerID(),
		workerStop: make(chan struct{}),
		workerWake: make(chan struct{}, 1),
	}, nil
}

func (s *apiServer) close() {
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}
	close(s.workerStop)
	if s.firestore != nil {
		_ = s.firestore.Close()
	}
	if s.storage != nil {
		_ = s.storage.Close()
	}
}

func (s *apiServer) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := strings.TrimSpace(request.Header.Get("Origin"))
		if origin != "" && s.isAllowedOrigin(origin) {
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Vary", "Origin")
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
			writer.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			writer.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Requested-With")
		}
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *apiServer) isAllowedOrigin(origin string) bool {
	for _, allowed := range s.cfg.allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return s.cfg.allowedOriginRegex != nil && s.cfg.allowedOriginRegex.MatchString(origin)
}

func (s *apiServer) handleHealthz(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *apiServer) authenticateRequest(writer http.ResponseWriter, request *http.Request) (*verifiedUser, bool) {
	if !s.cfg.requireOAuth {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "OAuth bearer token required")
		return nil, false
	}
	if len(s.cfg.oauthClientIDs) == 0 {
		writeJSONError(writer, http.StatusInternalServerError, "OAuth is required but no client ID is configured")
		return nil, false
	}
	authHeader := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Missing bearer token")
		return nil, false
	}
	token := strings.TrimSpace(authHeader[len("Bearer "):])
	if token == "" {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(writer, http.StatusUnauthorized, "Missing bearer token")
		return nil, false
	}
	user, statusCode, message := verifyGoogleToken(request.Context(), token, s.cfg.oauthClientIDs, s.cfg.oauthAllowedDomain)
	if statusCode != 0 {
		if statusCode == http.StatusUnauthorized {
			writer.Header().Set("WWW-Authenticate", "Bearer")
		}
		writeJSONError(writer, statusCode, message)
		return nil, false
	}
	return user, true
}

func verifyGoogleToken(ctx context.Context, token string, audiences []string, allowedDomains []string) (*verifiedUser, int, string) {
	var payload *idtoken.Payload
	for _, audience := range audiences {
		verifiedPayload, err := idtoken.Validate(ctx, token, audience)
		if err == nil {
			payload = verifiedPayload
			break
		}
	}
	if payload == nil {
		return nil, http.StatusUnauthorized, "Invalid or expired OAuth token"
	}
	email := claimString(payload.Claims, "email")
	if email == "" {
		return nil, http.StatusUnauthorized, "OAuth token is missing an email address"
	}
	if len(allowedDomains) > 0 {
		hostedDomain := claimString(payload.Claims, "hd")
		if !containsFold(allowedDomains, hostedDomain) {
			return nil, http.StatusForbidden, "OAuth token does not belong to an allowed domain"
		}
	}
	return &verifiedUser{
		Iss:   strings.TrimSpace(payload.Issuer),
		Sub:   strings.TrimSpace(payload.Subject),
		Email: email,
		Name:  claimString(payload.Claims, "name"),
	}, 0, ""
}

func writeJSON(writer http.ResponseWriter, statusCode int, value interface{}) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if value == nil {
		return
	}
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return
	}
}

func writeJSONError(writer http.ResponseWriter, statusCode int, detail string) {
	writeJSON(writer, statusCode, map[string]string{"detail": detail})
}

func envOrDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func normalizeListField(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var decoded []string
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		return sanitizeStrings(decoded)
	}
	parts := strings.Split(trimmed, ",")
	return sanitizeStrings(parts)
}

func sanitizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseIntDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func buildWorkerID() string {
	revision := strings.TrimSpace(os.Getenv("K_REVISION"))
	instance := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if revision == "" && instance == "" {
		return "local-worker"
	}
	if revision == "" {
		return instance
	}
	if instance == "" {
		return revision
	}
	return revision + "/" + instance
}

func firebasePreviewOriginPattern(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	for _, suffix := range []string{".web.app", ".firebaseapp.com"} {
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		base := strings.TrimSuffix(host, suffix)
		base = strings.TrimRight(base, "-")
		if base == "" {
			continue
		}
		return fmt.Sprintf("%s://%s(?:--preview-[0-9a-z]+)?%s", scheme, regexp.QuoteMeta(base), regexp.QuoteMeta(suffix))
	}
	return ""
}

func buildPreviewRegex(origins []string) string {
	patterns := make([]string, 0)
	for _, origin := range origins {
		pattern := firebasePreviewOriginPattern(origin)
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	return "^(?:" + strings.Join(patterns, "|") + ")$"
}

func stripRegexAnchors(pattern string) string {
	cleaned := strings.TrimSpace(pattern)
	cleaned = strings.TrimPrefix(cleaned, "^")
	cleaned = strings.TrimSuffix(cleaned, "$")
	return cleaned
}

func mergeOriginRegex(configuredRegex string, previewRegex string) string {
	patterns := make([]string, 0, 2)
	for _, candidate := range []string{configuredRegex, previewRegex} {
		cleaned := stripRegexAnchors(candidate)
		if cleaned == "" {
			continue
		}
		alreadySeen := false
		for _, pattern := range patterns {
			if pattern == cleaned {
				alreadySeen = true
				break
			}
		}
		if !alreadySeen {
			patterns = append(patterns, cleaned)
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	if len(patterns) == 1 {
		return "^" + patterns[0] + "$"
	}
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		parts = append(parts, "(?:"+pattern+")")
	}
	return "^(?:" + strings.Join(parts, "|") + ")$"
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

func isPublicBillingCallback(path string) bool {
	return path == "/billing/helcim/approval"
}
