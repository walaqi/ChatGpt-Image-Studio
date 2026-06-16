package api

import (
	"net/http"

	"chatgpt2api/internal/config"
)

// configPayload is the cpa-only diagnostics snapshot of effective config. After
// phase 7 the account pool, sync, proxy, newapi and sub2api sections are gone;
// config is mother-system-managed, so this is read-only (diagnostics export).
type configPayload struct {
	App struct {
		Name            string `json:"name"`
		Version         string `json:"version"`
		APIKey          string `json:"apiKey"`
		AuthKey         string `json:"authKey"`
		ImageFormat     string `json:"imageFormat"`
		MaxUploadSizeMB int    `json:"maxUploadSizeMB"`
	} `json:"app"`
	Server struct {
		Host                     string `json:"host"`
		Port                     int    `json:"port"`
		StaticDir                string `json:"staticDir"`
		PublicBasePath           string `json:"publicBasePath"`
		MaxImageConcurrency      int    `json:"maxImageConcurrency"`
		ImageQueueLimit          int    `json:"imageQueueLimit"`
		ImageQueueTimeoutSeconds int    `json:"imageQueueTimeoutSeconds"`
		ImageTaskQueueTTLSeconds int    `json:"imageTaskQueueTtlSeconds"`
	} `json:"server"`
	ChatGPT struct {
		Model          string `json:"model"`
		RequestTimeout int    `json:"requestTimeout"`
		ImageMode      string `json:"imageMode"`
	} `json:"chatgpt"`
	Storage struct {
		Backend                  string `json:"backend"`
		ConfigBackend            string `json:"configBackend"`
		ImageDir                 string `json:"imageDir"`
		ImageStorage             string `json:"imageStorage"`
		ImageConversationStorage string `json:"imageConversationStorage"`
		ImageDataStorage         string `json:"imageDataStorage"`
		SQLitePath               string `json:"sqlitePath"`
		RedisAddr                string `json:"redisAddr"`
		RedisPassword            string `json:"redisPassword"`
		RedisDB                  int    `json:"redisDb"`
		RedisPrefix              string `json:"redisPrefix"`
	} `json:"storage"`
	CPA struct {
		BaseURL        string `json:"baseUrl"`
		APIKey         string `json:"apiKey"`
		RequestTimeout int    `json:"requestTimeout"`
		RouteStrategy  string `json:"routeStrategy"`
	} `json:"cpa"`
	Identity struct {
		JWTPublicKeyPath  string `json:"jwtPublicKeyPath"`
		JWTIssuer         string `json:"jwtIssuer"`
		JWTAudience       string `json:"jwtAudience"`
		SessionTTLSeconds int    `json:"sessionTtlSeconds"`
	} `json:"identity"`
	Credential struct {
		EndpointBase    string `json:"endpointBase"`
		GatewayBaseURL  string `json:"gatewayBaseUrl"`
		CacheTTLSeconds int    `json:"cacheTtlSeconds"`
		RequestTimeout  int    `json:"requestTimeout"`
	} `json:"credential"`
	Log struct {
		LogAllRequests bool `json:"logAllRequests"`
	} `json:"log"`
	Paths config.Paths `json:"paths"`
}

func (s *Server) handleListRequestLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items": s.reqLogs.list(100),
	})
}

func (s *Server) buildConfigPayload() configPayload {
	return s.buildConfigPayloadFromConfig(s.cfg)
}

func (s *Server) buildConfigPayloadFromConfig(cfg *config.Config) configPayload {
	payload := configPayload{}
	payload.App.Name = cfg.App.Name
	payload.App.Version = s.cfg.App.Version
	payload.App.APIKey = cfg.App.APIKey
	payload.App.AuthKey = cfg.App.AuthKey
	payload.App.ImageFormat = cfg.App.ImageFormat
	payload.App.MaxUploadSizeMB = cfg.App.MaxUploadSizeMB

	payload.Server.Host = cfg.Server.Host
	payload.Server.Port = cfg.Server.Port
	payload.Server.StaticDir = cfg.Server.StaticDir
	payload.Server.PublicBasePath = cfg.Server.PublicBasePath
	payload.Server.MaxImageConcurrency = cfg.Server.MaxImageConcurrency
	payload.Server.ImageQueueLimit = cfg.Server.ImageQueueLimit
	payload.Server.ImageQueueTimeoutSeconds = cfg.Server.ImageQueueTimeoutSeconds
	payload.Server.ImageTaskQueueTTLSeconds = cfg.Server.ImageTaskQueueTTLSeconds

	payload.ChatGPT.Model = cfg.ChatGPT.Model
	payload.ChatGPT.RequestTimeout = cfg.ChatGPT.RequestTimeout
	payload.ChatGPT.ImageMode = cfg.ChatGPT.ImageMode

	payload.Storage.Backend = cfg.Storage.Backend
	payload.Storage.ConfigBackend = cfg.Storage.ConfigBackend
	payload.Storage.ImageDir = cfg.Storage.ImageDir
	payload.Storage.ImageStorage = cfg.Storage.ImageStorage
	payload.Storage.ImageConversationStorage = cfg.Storage.ImageConversationStorage
	payload.Storage.ImageDataStorage = cfg.Storage.ImageDataStorage
	payload.Storage.SQLitePath = cfg.Storage.SQLitePath
	payload.Storage.RedisAddr = cfg.Storage.RedisAddr
	payload.Storage.RedisPassword = cfg.Storage.RedisPassword
	payload.Storage.RedisDB = cfg.Storage.RedisDB
	payload.Storage.RedisPrefix = cfg.Storage.RedisPrefix

	payload.CPA.BaseURL = cfg.CPA.BaseURL
	payload.CPA.APIKey = cfg.CPA.APIKey
	payload.CPA.RequestTimeout = cfg.CPA.RequestTimeout
	payload.CPA.RouteStrategy = cfg.CPA.RouteStrategy

	payload.Identity.JWTPublicKeyPath = cfg.Identity.JWTPublicKeyPath
	payload.Identity.JWTIssuer = cfg.Identity.JWTIssuer
	payload.Identity.JWTAudience = cfg.Identity.JWTAudience
	payload.Identity.SessionTTLSeconds = cfg.Identity.SessionTTLSeconds

	payload.Credential.EndpointBase = cfg.Credential.EndpointBase
	payload.Credential.GatewayBaseURL = cfg.Credential.GatewayBaseURL
	payload.Credential.CacheTTLSeconds = cfg.Credential.CacheTTLSeconds
	payload.Credential.RequestTimeout = cfg.Credential.RequestTimeout

	payload.Log.LogAllRequests = cfg.Log.LogAllRequests
	payload.Paths = s.cfg.Paths()
	return payload
}
