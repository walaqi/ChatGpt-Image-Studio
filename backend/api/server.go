package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chatgpt2api/handler"
	"chatgpt2api/internal/buildinfo"
	"chatgpt2api/internal/config"
	"chatgpt2api/internal/credential"
	"chatgpt2api/internal/identity"
	"chatgpt2api/internal/middleware"
)

type Server struct {
	cfg              *config.Config
	runtimeMu        sync.RWMutex
	staticDir        string
	reqLogs          *imageRequestLogStore
	imageAdmission   *imageAdmissionController
	imageTasks       *imageTaskManager
	cpaClientFactory func(baseURL, apiKey, imageModel string, timeout time.Duration, routeStrategy string) cpaRouteAwareImageWorkflowClient

	// Multi-tenant identity. sessionManager and entryVerifier are required for
	// the entry-ticket → session-cookie exchange; without a configured RS256
	// public key, POST /auth/session returns 503.
	sessionManager *identity.SessionManager
	entryVerifier  *identity.EntryVerifier
	jtiStore       identity.JTIStore

	// Multi-tenant credential resolution. credService resolves each user's
	// per-request CPA credential from the mother system.
	credService *credential.Service
}

type requestError struct {
	code    string
	message string
}

const cpaFixedImageModel = "gpt-image-2"

func (e *requestError) Error() string {
	return firstNonEmpty(e.message, e.code)
}

func NewServer(cfg *config.Config) *Server {
	server := &Server{
		cfg:            cfg,
		staticDir:      cfg.ResolvePath(cfg.Server.StaticDir),
		reqLogs:        newImageRequestLogStore(),
		imageAdmission: newImageAdmissionController(),
		cpaClientFactory: func(baseURL, apiKey, imageModel string, timeout time.Duration, routeStrategy string) cpaRouteAwareImageWorkflowClient {
			return newCPAImageClientWithModel(baseURL, apiKey, imageModel, timeout, routeStrategy)
		},
	}
	server.imageTasks = newImageTaskManager(server)
	server.initIdentity()
	server.initCredential()
	return server
}

// initIdentity builds the session manager and (if a public key is configured)
// the entry-ticket verifier. The session manager always exists: a missing
// session_secret falls back to the legacy auth_key so single-tenant/dev runs
// still work. The entry verifier is optional — without a configured public key,
// POST /auth/session returns 503 and only pre-existing sessions/dev auth apply.
func (s *Server) initIdentity() {
	secret := strings.TrimSpace(s.cfg.Identity.SessionSecret)
	if secret == "" {
		secret = strings.TrimSpace(s.cfg.App.AuthKey)
	}
	if secret == "" {
		secret = "chatgpt-image-studio-default-session-secret"
	}
	if sm, err := identity.NewSessionManager(secret, s.cfg.SessionTTL()); err == nil {
		s.sessionManager = sm
	}

	s.jtiStore = identity.NewMemoryJTIStore()

	keyPath := strings.TrimSpace(s.cfg.Identity.JWTPublicKeyPath)
	if keyPath == "" {
		return
	}
	resolved := s.cfg.ResolvePath(keyPath)
	verifier, err := identity.NewEntryVerifierFromFile(
		resolved,
		s.cfg.Identity.JWTIssuer,
		s.cfg.Identity.JWTAudience,
		s.jtiStore,
	)
	if err != nil {
		slog.Warn("identity: entry-ticket verifier disabled", slog.String("path", resolved), slog.Any("error", err))
		return
	}
	s.entryVerifier = verifier
}

// initCredential builds the per-user credential service when the mother-system
// callback is configured ([credential] endpoint_base + internal_secret). When
// unconfigured, credService stays nil and the image pipeline falls back to the
// global [cpa] config so single-tenant/dev deployments keep working.
func (s *Server) initCredential() {
	base := strings.TrimSpace(s.cfg.Credential.EndpointBase)
	secret := strings.TrimSpace(s.cfg.Credential.InternalSecret)
	if base == "" || secret == "" {
		return
	}
	resolver, err := credential.NewHTTPResolver(credential.HTTPResolverConfig{
		EndpointBase:   base,
		InternalSecret: secret,
		GatewayBaseURL: strings.TrimSpace(s.cfg.Credential.GatewayBaseURL),
		RequestTimeout: s.cfg.CredentialRequestTimeout(),
		CacheTTL:       s.cfg.CredentialCacheTTL(),
	})
	if err != nil {
		slog.Warn("credential: resolver disabled", slog.Any("error", err))
		return
	}
	s.credService = credential.NewService(resolver, credential.NewMemorySelectionStore())
}

func (s *Server) getStaticDir() string {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.staticDir
}

func (s *Server) setStaticDir(staticDir string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.staticDir = staticDir
}

func (s *Server) reloadRuntimeDependencies(previous configPayload) error {
	if err := s.migrateImageFilesIfNeeded(previous, s.buildConfigPayload()); err != nil {
		return err
	}
	s.setStaticDir(s.cfg.ResolvePath(s.cfg.Server.StaticDir))
	return nil
}

func (s *Server) migrateImageFilesIfNeeded(previous, next configPayload) error {
	oldDir := s.cfg.ResolvePath(previous.Storage.ImageDir)
	newDir := s.cfg.ResolvePath(next.Storage.ImageDir)
	if strings.EqualFold(filepath.Clean(oldDir), filepath.Clean(newDir)) {
		return nil
	}
	info, err := os.Stat(oldDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return err
	}
	normalizedNewDir := filepath.Clean(newDir)
	return filepath.Walk(oldDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if strings.HasPrefix(filepath.Clean(path)+string(os.PathSeparator), normalizedNewDir+string(os.PathSeparator)) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(oldDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(newDir, rel)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(targetPath); err == nil {
			return os.Remove(path)
		}
		if err := os.Rename(path, targetPath); err == nil {
			return nil
		}
		if err := copyFile(path, targetPath); err != nil {
			return err
		}
		return os.Remove(path)
	})
}

func copyFile(sourcePath, targetPath string) error {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	targetFile, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer targetFile.Close()

	if _, err := io.Copy(targetFile, sourceFile); err != nil {
		return err
	}
	return nil
}

func storageSettingsChanged(previous, next configPayload) bool {
	return previous.Storage.Backend != next.Storage.Backend ||
		previous.Storage.ConfigBackend != next.Storage.ConfigBackend ||
		previous.Storage.AuthDir != next.Storage.AuthDir ||
		previous.Storage.StateFile != next.Storage.StateFile ||
		previous.Storage.SyncStateDir != next.Storage.SyncStateDir ||
		previous.Storage.SQLitePath != next.Storage.SQLitePath ||
		previous.Storage.ImageDir != next.Storage.ImageDir ||
		previous.Storage.ImageStorage != next.Storage.ImageStorage ||
		previous.Storage.ImageConversationStorage != next.Storage.ImageConversationStorage ||
		previous.Storage.ImageDataStorage != next.Storage.ImageDataStorage ||
		previous.Storage.RedisAddr != next.Storage.RedisAddr ||
		previous.Storage.RedisPassword != next.Storage.RedisPassword ||
		previous.Storage.RedisDB != next.Storage.RedisDB ||
		previous.Storage.RedisPrefix != next.Storage.RedisPrefix ||
		previous.Sync.ProviderType != next.Sync.ProviderType
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /auth/login", http.HandlerFunc(s.handleLogin))
	mux.Handle("POST /auth/session", http.HandlerFunc(s.handleSession))
	mux.Handle("GET /version", http.HandlerFunc(s.handleVersion))
	mux.Handle("GET /health", http.HandlerFunc(handleHealth))

	mux.Handle("GET /api/requests", s.requireUIAuth(http.HandlerFunc(s.handleListRequestLogs)))
	mux.Handle("GET /api/diagnostics/export", s.requireUIAuth(http.HandlerFunc(s.handleExportDiagnostics)))
	mux.Handle("GET /api/image/conversations", s.requireUIAuth(http.HandlerFunc(s.handleListImageConversations)))
	mux.Handle("DELETE /api/image/conversations", s.requireUIAuth(http.HandlerFunc(s.handleClearImageConversations)))
	mux.Handle("POST /api/image/conversations/import", s.requireUIAuth(http.HandlerFunc(s.handleImportImageConversations)))
	mux.Handle("GET /api/image/conversations/{id}", s.requireUIAuth(http.HandlerFunc(s.handleGetImageConversation)))
	mux.Handle("PUT /api/image/conversations/{id}", s.requireUIAuth(http.HandlerFunc(s.handleSaveImageConversation)))
	mux.Handle("DELETE /api/image/conversations/{id}", s.requireUIAuth(http.HandlerFunc(s.handleDeleteImageConversation)))
	mux.Handle("GET /api/image/credential/keys", s.requireUIAuth(http.HandlerFunc(s.handleListCredentialKeys)))
	mux.Handle("GET /api/image/credential/current", s.requireUIAuth(http.HandlerFunc(s.handleGetCurrentCredential)))
	mux.Handle("PUT /api/image/credential/current", s.requireUIAuth(http.HandlerFunc(s.handleSetCurrentCredential)))
	mux.Handle("POST /api/image/tasks", s.requireUIAuth(http.HandlerFunc(s.handleCreateImageTask)))
	mux.Handle("GET /api/image/tasks", s.requireUIAuth(http.HandlerFunc(s.handleListImageTasks)))
	mux.Handle("GET /api/image/tasks/snapshot", s.requireUIAuth(http.HandlerFunc(s.handleImageTaskSnapshot)))
	mux.Handle("GET /api/image/tasks/stream", s.requireUIAuth(http.HandlerFunc(s.handleImageTaskStream)))
	mux.Handle("GET /api/image/tasks/{id}", s.requireUIAuth(http.HandlerFunc(s.handleGetImageTask)))
	mux.Handle("DELETE /api/image/tasks/{id}", s.requireUIAuth(http.HandlerFunc(s.handleCancelImageTask)))

	mux.Handle("POST /v1/images/generations", s.requireImageAuth(http.HandlerFunc(s.handleImageGenerations)))
	mux.Handle("POST /v1/images/edits", s.requireImageAuth(http.HandlerFunc(s.handleImageEdits)))
	mux.Handle("POST /v1/chat/completions", s.requireImageAuth(http.HandlerFunc(s.handleImageChatCompletions)))
	mux.Handle("POST /v1/responses", s.requireImageAuth(http.HandlerFunc(s.handleImageResponses)))
	mux.Handle("GET /v1/models", s.requireImageAuth(http.HandlerFunc(s.handleModels)))
	mux.Handle("GET /v1/files/image/", s.requireImageAuth(http.HandlerFunc(s.handleImageFile)))

	mux.Handle("/", http.HandlerFunc(s.handleWebApp))

	handler := middleware.RequestID(middleware.Logger(mux))
	return middleware.CORS(handler)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.hasExactBearer(r, s.cfg.App.AuthKey) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authorization is invalid"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": buildinfo.ResolveVersion(s.cfg.App.Version),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   buildinfo.ResolveVersion(s.cfg.App.Version),
		"commit":    buildinfo.Commit,
		"buildTime": buildinfo.BuildTime,
	})
}

// handleSession exchanges a one-time RS256 entry ticket (minted by the mother
// system) for an image-studio session cookie. The ticket is read from the
// Authorization: Bearer header or a "ticket" form/query field. On success it
// sets an HttpOnly session cookie scoped to the public base path.
//
// Backend route: POST /auth/session. Browser-facing path: POST
// /image-studio/auth/session (reverse proxy strips the prefix; see §0.1).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.entryVerifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "entry ticket verification is not configured"})
		return
	}
	if s.sessionManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "session manager is not configured"})
		return
	}

	ticket := bearerFromRequest(r)
	if ticket == "" {
		ticket = strings.TrimSpace(r.FormValue("ticket"))
	}
	if ticket == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing entry ticket"})
		return
	}

	userID, err := s.entryVerifier.Verify(r.Context(), ticket)
	if err != nil {
		// Both invalid and replayed tickets map to 401; the distinction is
		// useful for logs but not for the client.
		slog.Warn("identity: entry ticket rejected", slog.Any("error", err))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "entry ticket invalid"})
		return
	}

	token, err := s.sessionManager.Mint(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to mint session"})
		return
	}

	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setSessionCookie writes the HttpOnly session cookie. Path is scoped to the
// public base path so the cookie is not exposed to the mother system's other
// routes. Secure is set when the request arrived over TLS (directly or via a
// trusted X-Forwarded-Proto from the reverse proxy).
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	path := s.cfg.PublicBasePath()
	if path == "" {
		path = "/"
	}
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     path,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL().Seconds()),
	}
	http.SetCookie(w, cookie)
}


func (s *Server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		Background     string `json:"background"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required"})
		return
	}
	if req.N < 1 {
		req.N = 1
	}

	payload, err := s.executeImageGeneration(r.Context(), imageGenerationRequest{
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              req.N,
		Size:           req.Size,
		Quality:        req.Quality,
		Background:     req.Background,
		ResponseFormat: req.ResponseFormat,
	}, r)
	if err != nil {
		writeImageRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(int64(max(1, s.cfg.App.MaxUploadSizeMB)) << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart form"})
		return
	}

	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required"})
		return
	}
	requestedModel := normalizeRequestedImageModel(r.FormValue("model"), s.cfg.ChatGPT.Model)
	responseFormat := firstNonEmpty(r.FormValue("response_format"), s.cfg.App.ImageFormat, "url")
	size := strings.TrimSpace(r.FormValue("size"))
	quality := strings.TrimSpace(r.FormValue("quality"))
	mask, err := readOptionalMultipartFile(r.MultipartForm, "mask")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	inpaintRequest := parseInpaintRequest(r)
	var payload map[string]any
	var data []map[string]any
	var execErr error
	if inpaintRequest.originalFileID != "" && inpaintRequest.originalGenID != "" {
		if strings.TrimSpace(inpaintRequest.sourceAccountID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source_account_id is required for selection edit"})
			return
		}
		payload, execErr = s.executeImageSelectionEdit(r.Context(), imageSelectionEditRequest{
			Model:           requestedModel,
			Prompt:          prompt,
			Mask:            mask,
			OriginalFileID:  inpaintRequest.originalFileID,
			OriginalGenID:   inpaintRequest.originalGenID,
			ConversationID:  inpaintRequest.conversationID,
			ParentMessageID: inpaintRequest.parentMessageID,
			SourceAccountID: inpaintRequest.sourceAccountID,
			ResponseFormat:  responseFormat,
		}, r)
		if execErr != nil {
			err = execErr
		} else {
			data = compatResponseDataItems(payload)
		}
	} else {
		images, readErr := readImagesFromMultipart(r.MultipartForm)
		if readErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": readErr.Error()})
			return
		}
		if len(images) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one image is required"})
			return
		}

		payload, execErr = s.executeImageEdit(r.Context(), imageEditRequest{
			Model:          requestedModel,
			Prompt:         prompt,
			Images:         images,
			Mask:           mask,
			Size:           size,
			Quality:        quality,
			ResponseFormat: responseFormat,
		}, r)
		if execErr != nil {
			err = execErr
		} else {
			data = compatResponseDataItems(payload)
		}
	}
	if err != nil {
		writeImageRequestError(w, err)
		return
	}
	if payload != nil {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

type imageRequestMetadata struct {
	size         string
	quality      string
	promptLength int
}

func (m imageRequestMetadata) applyTo(entry *imageRequestLogEntry) {
	if entry == nil {
		return
	}
	entry.Size = strings.TrimSpace(m.size)
	entry.Quality = strings.TrimSpace(m.quality)
	entry.PromptLength = m.promptLength
}

func newImageRequestMetadata(prompt, size, quality string) imageRequestMetadata {
	return imageRequestMetadata{
		size:         strings.TrimSpace(size),
		quality:      strings.TrimSpace(quality),
		promptLength: len([]rune(strings.TrimSpace(prompt))),
	}
}


// newCPAWorkflowClient builds a CPA image client bound to a specific user's
// credential (base URL + api-key + model). In the multi-tenant model the
// credential is resolved per request from the userID; the legacy Studio path
// passes a config-derived credential (removed in phase 7).
func (s *Server) newCPAWorkflowClient(cred credential.Credential) cpaRouteAwareImageWorkflowClient {
	timeout := time.Duration(max(10, s.cfg.CPAImageRequestTimeout())) * time.Second
	baseURL := strings.TrimSpace(cred.BaseURL)
	if baseURL == "" {
		baseURL = s.cfg.CPAImageBaseURL()
	}
	apiKey := strings.TrimSpace(cred.APIKey)
	if apiKey == "" {
		apiKey = s.cfg.CPAImageAPIKey()
	}
	model := strings.TrimSpace(cred.Model)
	if s != nil && s.cpaClientFactory != nil {
		return s.cpaClientFactory(
			baseURL,
			apiKey,
			model,
			timeout,
			s.cfg.CPAImageRouteStrategy(),
		)
	}
	return newCPAImageClientWithModel(
		baseURL,
		apiKey,
		model,
		timeout,
		s.cfg.CPAImageRouteStrategy(),
	)
}

// cpaCredentialFromConfig builds a credential from global CPA config, used as
// the single-tenant/dev fallback when no per-user credential service is
// configured.
func (s *Server) cpaCredentialFromConfig() credential.Credential {
	return credential.Credential{
		BaseURL: s.cfg.CPAImageBaseURL(),
		APIKey:  s.cfg.CPAImageAPIKey(),
	}
}

// resolveImageCredential returns the channel credential for the current request.
// In multi-tenant mode (credService configured) it resolves the per-user
// remembered key; the userID comes from the request context (injected by the
// session middleware, or by the async task executor — see phase 4). When no
// credService is configured it falls back to the global [cpa] config so
// single-tenant/dev deployments keep working.
//
// Returned errors are *requestError values with stable codes the frontend keys
// on to drive the picker / guidance UI.
func (s *Server) resolveImageCredential(ctx context.Context) (credential.Credential, error) {
	if s.credService == nil {
		// Single-tenant fallback: global config must be configured.
		if !s.cfg.CPAImageConfigured() {
			return credential.Credential{}, newRequestError("cpa_image_not_configured", "CPA 图片接口还未配置，请先在配置管理中设置 CPA base_url 与 api_key")
		}
		return credential.Credential{
			BaseURL: s.cfg.CPAImageBaseURL(),
			APIKey:  s.cfg.CPAImageAPIKey(),
		}, nil
	}

	userID, ok := identity.UserIDFromContext(ctx)
	if !ok {
		return credential.Credential{}, newRequestError("user_unidentified", "无法识别当前用户身份，请重新进入图片工作台")
	}
	cred, err := s.credService.ResolveForUser(ctx, userID)
	if err == nil {
		return cred, nil
	}
	switch {
	case errors.Is(err, credential.ErrNoSelection), errors.Is(err, credential.ErrNoCredential):
		return credential.Credential{}, newRequestError("image_key_not_selected", "请先在图片工作台选择一个可用的 API Key")
	case errors.Is(err, credential.ErrUpstreamUnavailable):
		return credential.Credential{}, newRequestError("credential_service_unavailable", "凭证服务暂时不可用，请稍后重试")
	default:
		return credential.Credential{}, newRequestError("image_key_unavailable", "当前没有可用的图片 API Key，请回到主系统创建或重新选择")
	}
}

// runPureCPAImageRequest executes one image operation against the per-user CPA
// credential resolved from ctx. useAdmission gates the global image concurrency
// queue: the sync /v1 path passes true; the async task path passes false because
// the task manager already bounds concurrency via runningUnits.
func (s *Server) runPureCPAImageRequest(
	ctx context.Context,
	operation string,
	responseFormat string,
	requestedModel string,
	preferredAccount bool,
	metadata imageRequestMetadata,
	run func(client imageWorkflowClient, upstreamModel string) ([]handler.ImageResult, error),
	r *http.Request,
	useAdmission bool,
) ([]map[string]any, error) {
	startedAt := time.Now()
	cred, credErr := s.resolveImageCredential(ctx)
	if credErr != nil {
		entry := imageRequestLogEntry{
			StartedAt:      startedAt.Format(time.RFC3339Nano),
			FinishedAt:     time.Now().Format(time.RFC3339Nano),
			Endpoint:       r.URL.Path,
			Operation:      operation,
			ImageMode:      "cpa",
			Direction:      "cpa",
			Route:          "cpa",
			CPASubroute:    s.cfg.CPAImageRouteStrategy(),
			RequestedModel: requestedModel,
			Preferred:      preferredAccount,
			Success:        false,
			Error:          credErr.Error(),
		}
		if requestErr, ok := credErr.(*requestError); ok {
			entry.ErrorCode = requestErr.code
		}
		metadata.applyTo(&entry)
		s.logImageRequest(entry)
		return nil, credErr
	}

	if useAdmission {
		admissionInfo, releaseAdmission, admissionErr := s.acquireImageAdmission(ctx)
		if admissionErr != nil {
			err := admissionErr
			if errors.Is(admissionErr, errImageAdmissionQueueFull) {
				err = newRequestError("image_queue_full", "当前图片请求排队已满，请稍后再试")
			} else if errors.Is(admissionErr, errImageAdmissionQueueTimeout) {
				err = newRequestError("image_queue_timeout", "当前图片请求排队超时，请稍后再试")
			}
			entry := imageRequestLogEntry{
				StartedAt:            startedAt.Format(time.RFC3339Nano),
				FinishedAt:           time.Now().Format(time.RFC3339Nano),
				Endpoint:             r.URL.Path,
				Operation:            operation,
				ImageMode:            "cpa",
				Direction:            "cpa",
				Route:                "cpa",
				CPASubroute:          s.cfg.CPAImageRouteStrategy(),
				RequestedModel:       requestedModel,
				Preferred:            preferredAccount,
				Success:              false,
				Error:                err.Error(),
				QueueWaitMS:          admissionInfo.QueueWaitMS,
				InflightCountAtStart: admissionInfo.InflightCountAtStart,
			}
			if requestErr, ok := err.(*requestError); ok {
				entry.ErrorCode = requestErr.code
			}
			metadata.applyTo(&entry)
			s.logImageRequest(entry)
			return nil, err
		}
		defer releaseAdmission()
		ctx = withImageAdmissionInfo(ctx, admissionInfo)
	}

	client := s.newCPAWorkflowClient(cred)
	upstreamModel := firstNonEmpty(strings.TrimSpace(cred.Model), cpaFixedImageModel)
	results, err := run(client, upstreamModel)
	cpaSubroute := client.LastRoute()
	if label := strings.TrimSpace(client.LastModelLabel()); label != "" {
		upstreamModel = label
	}
	if err != nil {
		admissionInfo := imageAdmissionFromContext(ctx)
		entry := imageRequestLogEntry{
			StartedAt:            startedAt.Format(time.RFC3339Nano),
			FinishedAt:           time.Now().Format(time.RFC3339Nano),
			Endpoint:             r.URL.Path,
			Operation:            operation,
			ImageMode:            "cpa",
			Direction:            "cpa",
			Route:                "cpa",
			CPASubroute:          cpaSubroute,
			RequestedModel:       requestedModel,
			UpstreamModel:        upstreamModel,
			Preferred:            preferredAccount,
			Success:              false,
			Error:                err.Error(),
			QueueWaitMS:          admissionInfo.QueueWaitMS,
			InflightCountAtStart: admissionInfo.InflightCountAtStart,
		}
		if requestErr, ok := err.(*requestError); ok {
			entry.ErrorCode = requestErr.code
		}
		metadata.applyTo(&entry)
		s.logImageRequest(entry)
		return nil, err
	}

	admissionInfo := imageAdmissionFromContext(ctx)
	entry := imageRequestLogEntry{
		StartedAt:            startedAt.Format(time.RFC3339Nano),
		FinishedAt:           time.Now().Format(time.RFC3339Nano),
		Endpoint:             r.URL.Path,
		Operation:            operation,
		ImageMode:            "cpa",
		Direction:            "cpa",
		Route:                "cpa",
		CPASubroute:          cpaSubroute,
		RequestedModel:       requestedModel,
		UpstreamModel:        upstreamModel,
		Preferred:            preferredAccount,
		Success:              true,
		QueueWaitMS:          admissionInfo.QueueWaitMS,
		InflightCountAtStart: admissionInfo.InflightCountAtStart,
	}
	metadata.applyTo(&entry)
	s.logImageRequest(entry)
	cpaUserID, _ := identity.UserIDFromContext(ctx)
	return buildImageResponse(r, client, results, responseFormat, "", cpaUserID, s.cfg.ResolvePath(s.cfg.Storage.ImageDir), s.cfg.PublicBasePath()), nil
}


func normalizeRequestedImageModel(requested, fallback string) string {
	model := strings.TrimSpace(requested)
	if model != "" {
		return model
	}
	model = strings.TrimSpace(fallback)
	if model != "" {
		return model
	}
	return "gpt-image-2"
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.availableModels()
	items := make([]map[string]any, 0, len(models))
	for index, model := range models {
		items = append(items, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  1700000000 + index,
			"owned_by": "openai",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

func (s *Server) availableModels() []string {
	seen := map[string]struct{}{}
	items := make([]string, 0)
	add := func(value string) {
		model := strings.TrimSpace(value)
		if model == "" {
			return
		}
		if _, ok := seen[model]; ok {
			return
		}
		seen[model] = struct{}{}
		items = append(items, model)
	}

	add("gpt-image-2")
	add(strings.TrimSpace(s.cfg.ChatGPT.Model))
	return items
}

func (s *Server) handleWebApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}

	requestPath := strings.TrimPrefix(r.URL.Path, "/")
	asset := resolveStaticAsset(s.getStaticDir(), requestPath)
	if asset == "" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(strings.ToLower(asset), ".html") {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	}
	http.ServeFile(w, r, asset)
}

// sessionCookieName is the HttpOnly cookie carrying image-studio's own session.
const sessionCookieName = "studio_sid"

// legacyDefaultUserID is the tenant assigned to requests authorized via the
// legacy single-tenant bearer key (no session cookie). In single-tenant/dev
// mode every request maps to this one user. Removed in phase 7.
const legacyDefaultUserID = "default"

// requireUIAuth gates the management/UI API. In the multi-tenant model it
// accepts a valid session cookie; for backward compatibility (single-tenant,
// dev, tests) it also accepts the legacy auth_key bearer. Either way the
// resolved userID is injected into the request context.
func (s *Server) requireUIAuth(next http.Handler) http.Handler {
	return s.requireSession(next, nil)
}

// requireImageAuth gates the OpenAI-compatible /v1 image API. Same as
// requireUIAuth but the legacy fallback additionally accepts the configured
// api_key comma-list (used by external API clients today).
func (s *Server) requireImageAuth(next http.Handler) http.Handler {
	return s.requireSession(next, parseKeys(s.cfg.App.APIKey))
}

// requireSession resolves the caller's userID (session cookie first, then
// legacy bearer keys) and injects it into the request context. On failure it
// returns 401. extraLegacyKeys are additional bearer keys accepted on the
// legacy path (beyond auth_key).
func (s *Server) requireSession(next http.Handler, extraLegacyKeys []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.resolveUserID(r, extraLegacyKeys)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authorization is invalid"})
			return
		}
		next.ServeHTTP(w, r.WithContext(identity.WithUserID(r.Context(), userID)))
	})
}

// resolveUserID returns the authenticated userID and whether the request is
// authorized. Resolution order:
//  1. Valid session cookie (studio_sid) → its userID (multi-tenant path).
//  2. Legacy bearer matching auth_key or one of extraLegacyKeys → legacyDefaultUserID.
func (s *Server) resolveUserID(r *http.Request, extraLegacyKeys []string) (string, bool) {
	if s.sessionManager != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			if userID, verr := s.sessionManager.Verify(cookie.Value); verr == nil {
				return userID, true
			}
		}
	}
	legacyKeys := append([]string{s.cfg.App.AuthKey}, extraLegacyKeys...)
	if s.hasAnyBearer(r, legacyKeys...) {
		return legacyDefaultUserID, true
	}
	return "", false
}

func (s *Server) hasAnyBearer(r *http.Request, keys ...string) bool {
	token := bearerFromRequest(r)
	if token == "" {
		return false
	}
	for _, key := range keys {
		if strings.TrimSpace(key) != "" && token == strings.TrimSpace(key) {
			return true
		}
	}
	return false
}

func (s *Server) hasExactBearer(r *http.Request, key string) bool {
	return strings.TrimSpace(key) != "" && bearerFromRequest(r) == strings.TrimSpace(key)
}

// requestIsTLS reports whether the original client connection used HTTPS,
// honoring X-Forwarded-Proto set by a trusted reverse proxy. Used to decide the
// Secure cookie attribute so local HTTP dev still works.
func requestIsTLS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	// X-Forwarded-Proto may be a comma-separated list; the first entry is the
	// original client scheme.
	if comma := strings.Index(proto, ","); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

func bearerFromRequest(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func parseKeys(raw string) []string {
	result := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		if cleaned := strings.TrimSpace(item); cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result
}

func resolveStaticAsset(staticDir, requestPath string) string {
	if strings.TrimSpace(staticDir) == "" {
		return ""
	}
	cleaned := strings.Trim(strings.TrimSpace(requestPath), "/")
	candidates := []string{}
	if cleaned == "" {
		candidates = append(candidates, filepath.Join(staticDir, "index.html"))
	} else {
		candidates = append(candidates,
			filepath.Join(staticDir, cleaned),
			filepath.Join(staticDir, cleaned, "index.html"),
			filepath.Join(staticDir, cleaned+".html"),
		)
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	if isStaticAssetRequest(cleaned) {
		return ""
	}
	indexPath := filepath.Join(staticDir, "index.html")
	if info, err := os.Stat(indexPath); err == nil && !info.IsDir() {
		return indexPath
	}
	return ""
}

func readImagesFromMultipart(form *multipart.Form) ([][]byte, error) {
	images := make([][]byte, 0)
	for _, key := range []string{"image", "image[]"} {
		files := form.File[key]
		for _, fileHeader := range files {
			data, err := readMultipartFile(fileHeader)
			if err != nil {
				return nil, err
			}
			images = append(images, data)
		}
	}

	for _, key := range []string{"image_base64", "imageBase64"} {
		if form.Value[key] == nil {
			continue
		}
		for _, raw := range form.Value[key] {
			decoded, err := decodeBase64Image(raw)
			if err != nil {
				return nil, err
			}
			images = append(images, decoded)
		}
	}
	return images, nil
}

func readOptionalMultipartFile(form *multipart.Form, key string) ([]byte, error) {
	files := form.File[key]
	if len(files) == 0 {
		return nil, nil
	}
	return readMultipartFile(files[0])
}

type inpaintRequest struct {
	originalFileID  string
	originalGenID   string
	conversationID  string
	parentMessageID string
	sourceAccountID string
}

func parseInpaintRequest(r *http.Request) inpaintRequest {
	return inpaintRequest{
		originalFileID:  strings.TrimSpace(r.FormValue("original_file_id")),
		originalGenID:   strings.TrimSpace(r.FormValue("original_gen_id")),
		conversationID:  strings.TrimSpace(r.FormValue("conversation_id")),
		parentMessageID: strings.TrimSpace(r.FormValue("parent_message_id")),
		sourceAccountID: strings.TrimSpace(r.FormValue("source_account_id")),
	}
}

func readMultipartFile(fileHeader *multipart.FileHeader) ([]byte, error) {
	file, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func decodeBase64Image(value string) ([]byte, error) {
	cleaned := strings.TrimSpace(value)
	if idx := strings.Index(cleaned, ","); idx >= 0 {
		cleaned = cleaned[idx+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 image")
	}
	return decoded, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func newRequestError(code, message string) error {
	return &requestError{
		code:    strings.TrimSpace(code),
		message: strings.TrimSpace(message),
	}
}

func requestErrorCode(err error) string {
	var typed *requestError
	if errors.As(err, &typed) {
		return typed.code
	}
	return ""
}

func writeImageRequestError(w http.ResponseWriter, err error) {
	if code := requestErrorCode(err); code != "" {
		writeAPIError(w, http.StatusBadGateway, code, err.Error())
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
}

// configuredImageMode reports the image execution mode. The workbench is
// CPA-only after phase 7; this is retained for diagnostics display.
func (s *Server) configuredImageMode() string {
	return "cpa"
}

func (s *Server) logImageRequest(entry imageRequestLogEntry) {
	if s == nil || s.reqLogs == nil {
		return
	}
	s.reqLogs.add(entry)
}

func isStaticAssetRequest(path string) bool {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" {
		return false
	}
	if strings.HasPrefix(cleaned, "_next/") {
		return true
	}
	return strings.Contains(filepath.Base(cleaned), ".")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result
}
