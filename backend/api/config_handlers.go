package api

import (
	"net/http"

	"chatgpt2api/internal/config"
)

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
		MaxImageConcurrency      int    `json:"maxImageConcurrency"`
		ImageQueueLimit          int    `json:"imageQueueLimit"`
		ImageQueueTimeoutSeconds int    `json:"imageQueueTimeoutSeconds"`
		ImageTaskQueueTTLSeconds int    `json:"imageTaskQueueTtlSeconds"`
	} `json:"server"`
	ChatGPT struct {
		Model                            string `json:"model"`
		SSETimeout                       int    `json:"sseTimeout"`
		PollInterval                     int    `json:"pollInterval"`
		PollMaxWait                      int    `json:"pollMaxWait"`
		RequestTimeout                   int    `json:"requestTimeout"`
		ImageMode                        string `json:"imageMode"`
		FreeImageRoute                   string `json:"freeImageRoute"`
		FreeImageModel                   string `json:"freeImageModel"`
		PaidImageRoute                   string `json:"paidImageRoute"`
		PaidImageModel                   string `json:"paidImageModel"`
		StudioAllowDisabledImageAccounts bool   `json:"studioAllowDisabledImageAccounts"`
	} `json:"chatgpt"`
	Accounts struct {
		DefaultQuota                int  `json:"defaultQuota"`
		PreferRemoteRefresh         bool `json:"preferRemoteRefresh"`
		RefreshWorkers              int  `json:"refreshWorkers"`
		ImageQuotaRefreshTTLSeconds int  `json:"imageQuotaRefreshTTLSeconds"`
	} `json:"accounts"`
	Storage struct {
		Backend                  string `json:"backend"`
		ConfigBackend            string `json:"configBackend"`
		AuthDir                  string `json:"authDir"`
		StateFile                string `json:"stateFile"`
		SyncStateDir             string `json:"syncStateDir"`
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
	Sync struct {
		Enabled        bool   `json:"enabled"`
		BaseURL        string `json:"baseUrl"`
		ManagementKey  string `json:"managementKey"`
		RequestTimeout int    `json:"requestTimeout"`
		Concurrency    int    `json:"concurrency"`
		ProviderType   string `json:"providerType"`
	} `json:"sync"`
	Proxy struct {
		Enabled     bool   `json:"enabled"`
		URL         string `json:"url"`
		Mode        string `json:"mode"`
		SyncEnabled bool   `json:"syncEnabled"`
	} `json:"proxy"`
	CPA struct {
		BaseURL        string `json:"baseUrl"`
		APIKey         string `json:"apiKey"`
		RequestTimeout int    `json:"requestTimeout"`
		RouteStrategy  string `json:"routeStrategy"`
	} `json:"cpa"`
	NewAPI struct {
		BaseURL        string `json:"baseUrl"`
		Username       string `json:"username"`
		Password       string `json:"password"`
		AccessToken    string `json:"accessToken"`
		UserID         int    `json:"userId"`
		SessionCookie  string `json:"sessionCookie"`
		RequestTimeout int    `json:"requestTimeout"`
	} `json:"newapi"`
	Sub2API struct {
		BaseURL        string `json:"baseUrl"`
		Email          string `json:"email"`
		Password       string `json:"password"`
		APIKey         string `json:"apiKey"`
		GroupID        string `json:"groupId"`
		RequestTimeout int    `json:"requestTimeout"`
	} `json:"sub2api"`
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
	payload.Server.MaxImageConcurrency = cfg.Server.MaxImageConcurrency
	payload.Server.ImageQueueLimit = cfg.Server.ImageQueueLimit
	payload.Server.ImageQueueTimeoutSeconds = cfg.Server.ImageQueueTimeoutSeconds
	payload.Server.ImageTaskQueueTTLSeconds = cfg.Server.ImageTaskQueueTTLSeconds

	payload.ChatGPT.Model = cfg.ChatGPT.Model
	payload.ChatGPT.SSETimeout = cfg.ChatGPT.SSETimeout
	payload.ChatGPT.PollInterval = cfg.ChatGPT.PollInterval
	payload.ChatGPT.PollMaxWait = cfg.ChatGPT.PollMaxWait
	payload.ChatGPT.RequestTimeout = cfg.ChatGPT.RequestTimeout
	payload.ChatGPT.ImageMode = cfg.ChatGPT.ImageMode
	payload.ChatGPT.FreeImageRoute = cfg.ChatGPT.FreeImageRoute
	payload.ChatGPT.FreeImageModel = cfg.ChatGPT.FreeImageModel
	payload.ChatGPT.PaidImageRoute = cfg.ChatGPT.PaidImageRoute
	payload.ChatGPT.PaidImageModel = cfg.ChatGPT.PaidImageModel
	payload.ChatGPT.StudioAllowDisabledImageAccounts = cfg.ChatGPT.StudioAllowDisabledImageAccounts

	payload.Accounts.DefaultQuota = cfg.Accounts.DefaultQuota
	payload.Accounts.PreferRemoteRefresh = cfg.Accounts.PreferRemoteRefresh
	payload.Accounts.RefreshWorkers = cfg.Accounts.RefreshWorkers
	payload.Accounts.ImageQuotaRefreshTTLSeconds = cfg.Accounts.ImageQuotaRefreshTTLSeconds

	payload.Storage.Backend = cfg.Storage.Backend
	payload.Storage.ConfigBackend = cfg.Storage.ConfigBackend
	payload.Storage.AuthDir = cfg.Storage.AuthDir
	payload.Storage.StateFile = cfg.Storage.StateFile
	payload.Storage.SyncStateDir = cfg.Storage.SyncStateDir
	payload.Storage.ImageDir = cfg.Storage.ImageDir
	payload.Storage.ImageStorage = cfg.Storage.ImageStorage
	payload.Storage.ImageConversationStorage = cfg.Storage.ImageConversationStorage
	payload.Storage.ImageDataStorage = cfg.Storage.ImageDataStorage
	payload.Storage.SQLitePath = cfg.Storage.SQLitePath
	payload.Storage.RedisAddr = cfg.Storage.RedisAddr
	payload.Storage.RedisPassword = cfg.Storage.RedisPassword
	payload.Storage.RedisDB = cfg.Storage.RedisDB
	payload.Storage.RedisPrefix = cfg.Storage.RedisPrefix

	payload.Sync.Enabled = cfg.Sync.Enabled
	payload.Sync.BaseURL = cfg.Sync.BaseURL
	payload.Sync.ManagementKey = cfg.Sync.ManagementKey
	payload.Sync.RequestTimeout = cfg.Sync.RequestTimeout
	payload.Sync.Concurrency = cfg.Sync.Concurrency
	payload.Sync.ProviderType = cfg.Sync.ProviderType

	payload.Proxy.Enabled = cfg.Proxy.Enabled
	payload.Proxy.URL = cfg.Proxy.URL
	payload.Proxy.Mode = cfg.Proxy.Mode
	payload.Proxy.SyncEnabled = cfg.Proxy.SyncEnabled

	payload.CPA.BaseURL = cfg.CPA.BaseURL
	payload.CPA.APIKey = cfg.CPA.APIKey
	payload.CPA.RequestTimeout = cfg.CPA.RequestTimeout
	payload.CPA.RouteStrategy = cfg.CPA.RouteStrategy

	payload.NewAPI.BaseURL = cfg.NewAPI.BaseURL
	payload.NewAPI.Username = cfg.NewAPI.Username
	payload.NewAPI.Password = cfg.NewAPI.Password
	payload.NewAPI.AccessToken = cfg.NewAPI.AccessToken
	payload.NewAPI.UserID = cfg.NewAPI.UserID
	payload.NewAPI.SessionCookie = cfg.NewAPI.SessionCookie
	payload.NewAPI.RequestTimeout = cfg.NewAPI.RequestTimeout

	payload.Sub2API.BaseURL = cfg.Sub2API.BaseURL
	payload.Sub2API.Email = cfg.Sub2API.Email
	payload.Sub2API.Password = cfg.Sub2API.Password
	payload.Sub2API.APIKey = cfg.Sub2API.APIKey
	payload.Sub2API.GroupID = cfg.Sub2API.GroupID
	payload.Sub2API.RequestTimeout = cfg.Sub2API.RequestTimeout

	payload.Log.LogAllRequests = cfg.Log.LogAllRequests
	payload.Paths = s.cfg.Paths()
	return payload
}
