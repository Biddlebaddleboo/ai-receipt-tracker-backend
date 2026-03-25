package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	fs "cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/iterator"
)

type config struct {
	port                string
	pythonBackendPort   string
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
}

type verifiedUser struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type categoryPayload struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type categoryRecord struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

type apiServer struct {
	cfg        config
	proxy      *httputil.ReverseProxy
	firestore  *fs.Client
	storage    *storage.Client
	pythonCmd  *exec.Cmd
	receipts   *fs.CollectionRef
	categories *fs.CollectionRef
	plans      *fs.CollectionRef
	users      *fs.CollectionRef
	bucket     *storage.BucketHandle
	pythonURL  *url.URL
	httpServer *http.Server
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	server, err := newAPIServer(cfg)
	if err != nil {
		log.Fatalf("failed to initialize API server: %v", err)
	}
	defer server.close()

	if err := server.startPythonBackend(); err != nil {
		log.Fatalf("failed to start Python backend: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/categories", server.handleCategories)
	mux.HandleFunc("/categories/", server.handleCategoryByID)
	mux.HandleFunc("/receipts", server.handleReceipts)
	mux.HandleFunc("/receipts/", server.handleReceiptByID)
	mux.HandleFunc("/users/me/plan", server.handleUserPlan)
	mux.HandleFunc("/billing", server.handleBillingProxy)
	mux.HandleFunc("/billing/", server.handleBillingProxy)
	mux.Handle("/", server.proxy)

	server.httpServer = &http.Server{
		Addr:    ":" + cfg.port,
		Handler: server.withCORS(mux),
	}

	log.Printf("Go API server listening on :%s and forwarding Python routes to %s", cfg.port, server.pythonURL.String())
	if err := server.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Go API server stopped unexpectedly: %v", err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		port:                envOrDefault("PORT", "8080"),
		pythonBackendPort:   envOrDefault("PYTHON_BACKEND_PORT", "8081"),
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
	pythonURL, err := url.Parse("http://127.0.0.1:" + cfg.pythonBackendPort)
	if err != nil {
		_ = client.Close()
		_ = storageClient.Close()
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(pythonURL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = pythonURL.Host
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		stripProxiedCORSHeaders(resp.Header)
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("Python backend proxy error for %s %s: %v", request.Method, request.URL.Path, err)
		writeJSONError(writer, http.StatusBadGateway, "Python backend unavailable")
	}
	return &apiServer{
		cfg:        cfg,
		proxy:      proxy,
		firestore:  client,
		storage:    storageClient,
		receipts:   client.Collection(cfg.firestoreCollection),
		categories: client.Collection(cfg.categoriesColl),
		plans:      client.Collection(cfg.plansCollection),
		users:      client.Collection(cfg.usersCollection),
		bucket:     storageClient.Bucket(cfg.gcsBucketName),
		pythonURL:  pythonURL,
	}, nil
}

func (s *apiServer) close() {
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}
	if s.pythonCmd != nil && s.pythonCmd.Process != nil {
		_ = s.pythonCmd.Process.Signal(syscall.SIGTERM)
	}
	if s.firestore != nil {
		_ = s.firestore.Close()
	}
	if s.storage != nil {
		_ = s.storage.Close()
	}
}

func (s *apiServer) startPythonBackend() error {
	cmd := exec.Command("python", "-m", "uvicorn", "app.main:app", "--host", "127.0.0.1", "--port", s.cfg.pythonBackendPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PORT="+s.cfg.pythonBackendPort)
	if err := cmd.Start(); err != nil {
		return err
	}
	s.pythonCmd = cmd

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := "127.0.0.1:" + s.cfg.pythonBackendPort
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
			log.Printf("Python backend is ready on %s", address)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Python backend on %s", address)
		case <-time.After(200 * time.Millisecond):
		}
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

func stripProxiedCORSHeaders(header http.Header) {
	header.Del("Access-Control-Allow-Origin")
	header.Del("Access-Control-Allow-Credentials")
	header.Del("Access-Control-Allow-Methods")
	header.Del("Access-Control-Allow-Headers")
	header.Del("Access-Control-Expose-Headers")
	header.Del("Access-Control-Max-Age")
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

func (s *apiServer) handleCategories(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	switch request.Method {
	case http.MethodGet:
		s.listCategories(writer, request, user)
	case http.MethodPost:
		s.createCategory(writer, request, user)
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *apiServer) handleCategoryByID(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	categoryID := strings.TrimPrefix(request.URL.Path, "/categories/")
	categoryID = strings.TrimSpace(categoryID)
	if categoryID == "" || strings.Contains(categoryID, "/") {
		writeJSONError(writer, http.StatusNotFound, "Not found")
		return
	}
	switch request.Method {
	case http.MethodPut:
		s.updateCategory(writer, request, user, categoryID)
	case http.MethodDelete:
		s.deleteCategory(writer, request, user, categoryID)
	default:
		writeJSONError(writer, http.StatusMethodNotAllowed, "Method not allowed")
	}
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

func (s *apiServer) createCategory(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	payload, err := decodeCategoryPayload(request)
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, err.Error())
		return
	}
	ctx := request.Context()
	doc := s.categories.NewDoc()
	if _, err := doc.Set(ctx, categoryDocFromPayload(payload, user.Email)); err != nil {
		writeJSONError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	record, statusCode, message := s.getCategoryRecord(ctx, doc.ID, user.Email)
	if statusCode != 0 {
		writeJSONError(writer, statusCode, message)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) listCategories(writer http.ResponseWriter, request *http.Request, user *verifiedUser) {
	iter := s.categories.Where("owner_email", "==", user.Email).Documents(request.Context())
	defer iter.Stop()
	records := make([]categoryRecord, 0)
	for {
		snapshot, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			writeJSONError(writer, http.StatusInternalServerError, fmt.Sprintf("Failed to load categories: %v", err))
			return
		}
		record, ok := snapshotToCategoryRecord(snapshot, user.Email)
		if ok {
			records = append(records, record)
		}
	}
	writeJSON(writer, http.StatusOK, records)
}

func (s *apiServer) updateCategory(writer http.ResponseWriter, request *http.Request, user *verifiedUser, categoryID string) {
	payload, err := decodeCategoryPayload(request)
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, err.Error())
		return
	}
	ctx := request.Context()
	snapshot, err := s.categories.Doc(categoryID).Get(ctx)
	if err != nil || !snapshot.Exists() {
		writeJSONError(writer, http.StatusNotFound, "category not found")
		return
	}
	if !ownsCategory(snapshot.Data(), user.Email) {
		writeJSONError(writer, http.StatusNotFound, "category not found")
		return
	}
	update := map[string]interface{}{}
	if payload.Name != nil {
		update["name"] = strings.TrimSpace(*payload.Name)
	}
	if payload.Description != nil {
		update["description"] = strings.TrimSpace(*payload.Description)
	}
	if len(update) > 0 {
		if _, err := s.categories.Doc(categoryID).Set(ctx, update, fs.MergeAll); err != nil {
			writeJSONError(writer, http.StatusInternalServerError, err.Error())
			return
		}
	}
	record, statusCode, message := s.getCategoryRecord(ctx, categoryID, user.Email)
	if statusCode != 0 {
		writeJSONError(writer, statusCode, message)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *apiServer) deleteCategory(writer http.ResponseWriter, request *http.Request, user *verifiedUser, categoryID string) {
	ctx := request.Context()
	snapshot, err := s.categories.Doc(categoryID).Get(ctx)
	if err != nil || !snapshot.Exists() {
		writeJSONError(writer, http.StatusNotFound, "category not found")
		return
	}
	if !ownsCategory(snapshot.Data(), user.Email) {
		writeJSONError(writer, http.StatusNotFound, "category not found")
		return
	}
	if _, err := s.categories.Doc(categoryID).Delete(ctx); err != nil {
		writeJSONError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) getCategoryRecord(ctx context.Context, categoryID string, ownerEmail string) (categoryRecord, int, string) {
	snapshot, err := s.categories.Doc(categoryID).Get(ctx)
	if err != nil || !snapshot.Exists() {
		return categoryRecord{}, http.StatusNotFound, "category not found"
	}
	record, ok := snapshotToCategoryRecord(snapshot, ownerEmail)
	if !ok {
		return categoryRecord{}, http.StatusNotFound, "category not found"
	}
	return record, 0, ""
}

func snapshotToCategoryRecord(snapshot *fs.DocumentSnapshot, ownerEmail string) (categoryRecord, bool) {
	data := snapshot.Data()
	if !ownsCategory(data, ownerEmail) {
		return categoryRecord{}, false
	}
	name := strings.TrimSpace(fmt.Sprint(data["name"]))
	if name == "" {
		return categoryRecord{}, false
	}
	description := strings.TrimSpace(fmt.Sprint(data["description"]))
	record := categoryRecord{
		ID:   snapshot.Ref.ID,
		Name: name,
	}
	if description != "" {
		record.Description = &description
	}
	return record, true
}

func ownsCategory(data map[string]interface{}, ownerEmail string) bool {
	stored := strings.TrimSpace(fmt.Sprint(data["owner_email"]))
	return stored != "" && stored == ownerEmail
}

func categoryDocFromPayload(payload categoryPayload, ownerEmail string) map[string]interface{} {
	description := ""
	if payload.Description != nil {
		description = strings.TrimSpace(*payload.Description)
	}
	return map[string]interface{}{
		"name":        strings.TrimSpace(*payload.Name),
		"description": description,
		"owner_email": ownerEmail,
	}
}

func decodeCategoryPayload(request *http.Request) (categoryPayload, error) {
	defer request.Body.Close()
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload categoryPayload
	if err := decoder.Decode(&payload); err != nil {
		return categoryPayload{}, fmt.Errorf("invalid request body")
	}
	if payload.Name == nil {
		return categoryPayload{}, fmt.Errorf("name is required")
	}
	trimmed := strings.TrimSpace(*payload.Name)
	if trimmed == "" {
		return categoryPayload{}, fmt.Errorf("name is required")
	}
	payload.Name = &trimmed
	return payload, nil
}

func writeJSON(writer http.ResponseWriter, statusCode int, value interface{}) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if value == nil {
		return
	}
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("failed to encode JSON response: %v", err)
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

func (s *apiServer) handleBillingProxy(writer http.ResponseWriter, request *http.Request) {
	if isPublicBillingCallback(request.URL.Path) {
		s.proxy.ServeHTTP(writer, request)
		return
	}
	user, ok := s.authenticateRequest(writer, request)
	if !ok {
		return
	}
	request.Header.Set("X-Go-Authenticated-Email", user.Email)
	s.proxy.ServeHTTP(writer, request)
}

func isPublicBillingCallback(path string) bool {
	return path == "/billing/helcim/approval"
}
