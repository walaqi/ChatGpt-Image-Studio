package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"


	"github.com/BurntSushi/toml"
)

const (
	exampleConfigFile = "config.example.toml"
	userConfigFile    = "config.toml"
	dataDirName       = "data"
)

var (
	osGetwd      = os.Getwd
	osExecutable = os.Executable
)

type Paths struct {
	Root     string
	Defaults string
	Override string
}

type AppConfig struct {
	Name            string `toml:"name"`
	Version         string `toml:"version"`
	APIKey          string `toml:"api_key"`
	AuthKey         string `toml:"auth_key"`
	ImageFormat     string `toml:"image_format"`
	MaxUploadSizeMB int    `toml:"max_upload_size_mb"`
}

type ServerConfig struct {
	Host                     string `toml:"host"`
	Port                     int    `toml:"port"`
	StaticDir                string `toml:"static_dir"`
	PublicBasePath           string `toml:"public_base_path"`
	MaxImageConcurrency      int    `toml:"max_image_concurrency"`
	ImageQueueLimit          int    `toml:"image_queue_limit"`
	ImageQueueTimeoutSeconds int    `toml:"image_queue_timeout_seconds"`
	ImageTaskQueueTTLSeconds int    `toml:"image_task_queue_ttl_seconds"`
}

type ChatGPTConfig struct {
	Model          string `toml:"model"`
	SSETimeout     int    `toml:"sse_timeout"`
	PollInterval   int    `toml:"poll_interval"`
	PollMaxWait    int    `toml:"poll_max_wait"`
	RequestTimeout int    `toml:"request_timeout"`
	// ImageMode is retained for forward/backward config compatibility but the
	// backend is cpa-only since phase 7; any value normalizes to "cpa".
	ImageMode string `toml:"image_mode"`
}

type StorageConfig struct {
	Backend                  string `toml:"backend"`
	ConfigBackend            string `toml:"config_backend"`
	ImageDir                 string `toml:"image_dir"`
	ImageStorage             string `toml:"image_storage"`
	ImageConversationStorage string `toml:"image_conversation_storage"`
	ImageDataStorage         string `toml:"image_data_storage"`
	SQLitePath               string `toml:"sqlite_path"`
	RedisAddr                string `toml:"redis_addr"`
	RedisPassword            string `toml:"redis_password"`
	RedisDB                  int    `toml:"redis_db"`
	RedisPrefix              string `toml:"redis_prefix"`
}

type LogConfig struct {
	LogAllRequests bool `toml:"log_all_requests"`
}

type CPAConfig struct {
	BaseURL        string `toml:"base_url"`
	APIKey         string `toml:"api_key"`
	RequestTimeout int    `toml:"request_timeout"`
	RouteStrategy  string `toml:"route_strategy"`
}

// IdentityConfig governs multi-tenant entry-ticket verification and the
// image-studio self-issued session cookie. See docs/multi-tenant-redesign.md §4.1.
type IdentityConfig struct {
	// JWTPublicKeyPath is the PEM file holding the mother system's RS256 public
	// key, used to verify entry tickets.
	JWTPublicKeyPath string `toml:"jwt_public_key_path"`
	// JWTIssuer / JWTAudience are the expected iss/aud claim values.
	JWTIssuer   string `toml:"jwt_issuer"`
	JWTAudience string `toml:"jwt_audience"`
	// SessionSecret signs image-studio's own session cookie (independent of the
	// mother system's key).
	SessionSecret string `toml:"session_secret"`
	// SessionTTLSeconds bounds the session cookie lifetime.
	SessionTTLSeconds int `toml:"session_ttl_seconds"`
}

// CredentialConfig governs the two-stage callback to the mother system that
// resolves a user's channel api-key. See docs/multi-tenant-redesign.md §4.2.
type CredentialConfig struct {
	// EndpointBase is the mother system's internal base URL. The two-stage
	// endpoints are <base>/internal/cred/keys and <base>/internal/cred.
	EndpointBase string `toml:"endpoint_base"`
	// InternalSecret is the service-to-service shared secret (X-Internal-Secret).
	InternalSecret string `toml:"internal_secret"`
	// CacheTTLSeconds bounds how long resolved plaintext credentials are cached.
	CacheTTLSeconds int `toml:"cache_ttl_seconds"`
	// GatewayBaseURL is the OpenAI-compatible gateway image-studio calls with the
	// user's key. Set per environment; takes precedence over any base_url the
	// mother system returns.
	GatewayBaseURL string `toml:"gateway_base_url"`
	// RequestTimeout bounds the internal callback HTTP timeout (seconds).
	RequestTimeout int `toml:"request_timeout"`
}

type Config struct {
	mu     sync.RWMutex `toml:"-"`
	loadMu sync.Mutex   `toml:"-"`
	loaded bool         `toml:"-"`
	paths  Paths        `toml:"-"`

	App     AppConfig     `toml:"app"`
	Server  ServerConfig  `toml:"server"`
	ChatGPT ChatGPTConfig `toml:"chatgpt"`
	Storage StorageConfig `toml:"storage"`
	Log     LogConfig     `toml:"log"`
	CPA     CPAConfig     `toml:"cpa"`

	Identity   IdentityConfig   `toml:"identity"`
	Credential CredentialConfig `toml:"credential"`
}

func New(rootDir string) *Config {
	return &Config{paths: resolvePaths(rootDir)}
}

func (c *Config) Load() error {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	next := &Config{paths: c.paths}

	if err := decodeDefaultTemplate(next); err != nil {
		return fmt.Errorf("decode embedded defaults: %w", err)
	}
	if fileExists(c.paths.Override) {
		_, _ = migrateLegacyOverrideFile(c.paths.Override)
		if err := decodeOverrideFile(c.paths.Override, next); err != nil {
			return fmt.Errorf("decode override: %w", err)
		}
	}
	if err := next.validate(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.copyFrom(next)
	c.loaded = true
	return nil
}

func (c *Config) EnsureLoaded() error {
	c.mu.RLock()
	loaded := c.loaded
	c.mu.RUnlock()
	if loaded {
		return nil
	}
	return c.Load()
}

func (c *Config) GetString(key string, fallback ...string) string {
	value, ok := c.lookup(key)
	if !ok {
		return stringFallback(fallback)
	}
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return stringFallback(fallback)
	}
}

func (c *Config) GetInt(key string, fallback ...int) int {
	value, ok := c.lookup(key)
	if !ok {
		return intFallback(fallback)
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int8:
		return int(typed)
	case int16:
		return int(typed)
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case uint:
		return int(typed)
	case uint8:
		return int(typed)
	case uint16:
		return int(typed)
	case uint32:
		return int(typed)
	case uint64:
		return int(typed)
	default:
		return intFallback(fallback)
	}
}

func (c *Config) GetBool(key string, fallback ...bool) bool {
	value, ok := c.lookup(key)
	if !ok {
		return boolFallback(fallback)
	}
	typed, ok := value.(bool)
	if !ok {
		return boolFallback(fallback)
	}
	return typed
}

func (c *Config) Paths() Paths {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paths
}

func (c *Config) RootDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paths.Root
}

func (c *Config) ResolvePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return c.RootDir()
	}
	if filepath.IsAbs(trimmed) {
		return trimmed
	}
	return filepath.Join(c.RootDir(), trimmed)
}

func (c *Config) SaveOverride(section, key string, value any) error {
	return c.SaveOverrides(map[string]map[string]any{
		section: {
			key: value,
		},
	})
}

func (c *Config) SaveOverrides(values map[string]map[string]any) error {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	raw, err := mergeOverrideValues(c.paths.Override, values)
	if err != nil {
		return err
	}

	if err := writeOverrideMap(c.paths.Override, raw); err != nil {
		return err
	}

	next := &Config{paths: c.paths}
	if err := decodeDefaultTemplate(next); err != nil {
		return fmt.Errorf("reload embedded defaults: %w", err)
	}
	if fileExists(c.paths.Override) {
		if err := decodeOverrideFile(c.paths.Override, next); err != nil {
			return fmt.Errorf("reload override: %w", err)
		}
	}
	if err := next.validate(); err != nil {
		return err
	}

	c.mu.Lock()
	c.copyFrom(next)
	c.loaded = true
	c.mu.Unlock()
	return nil
}

func (c *Config) PersistOverrideFile(values map[string]map[string]any) error {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	raw, err := mergeOverrideValues(c.paths.Override, values)
	if err != nil {
		return err
	}
	return writeOverrideMap(c.paths.Override, raw)
}

func (c *Config) ApplyOverrides(values map[string]map[string]any) error {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	raw := map[string]any{}
	for section, entries := range values {
		sectionMap := map[string]any{}
		for key, value := range entries {
			sectionMap[key] = sanitizeOverrideEntry(section, key, value)
		}
		raw[section] = sectionMap
	}
	sanitizeOverrideMap(raw)

	next := &Config{paths: c.paths}
	c.mu.RLock()
	next.copyFrom(c)
	next.paths = c.paths
	c.mu.RUnlock()

	if err := applyOverrideMap(reflect.ValueOf(next).Elem(), raw); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}

	c.mu.Lock()
	c.copyFrom(next)
	c.loaded = true
	c.mu.Unlock()
	return nil
}

func mergeOverrideValues(path string, values map[string]map[string]any) (map[string]any, error) {
	raw := map[string]any{}
	if fileExists(path) {
		loaded, err := loadOverrideMap(path)
		if err != nil {
			return nil, fmt.Errorf("read override: %w", err)
		}
		raw = loaded
	}
	sanitizeOverrideMap(raw)

	for section, entries := range values {
		sec, ok := raw[section].(map[string]any)
		if !ok {
			sec = map[string]any{}
		}
		for key, value := range entries {
			sec[key] = sanitizeOverrideEntry(section, key, value)
		}
		raw[section] = sec
	}
	return raw, nil
}

func LoadDefaults(paths Paths) (*Config, error) {
	next := &Config{paths: paths}
	if err := decodeDefaultTemplate(next); err != nil {
		return nil, fmt.Errorf("decode embedded defaults: %w", err)
	}
	if err := next.validate(); err != nil {
		return nil, err
	}
	next.loaded = true
	return next, nil
}

func (c *Config) lookup(key string) (any, bool) {
	if err := c.EnsureLoaded(); err != nil {
		return nil, false
	}

	parts := strings.Split(key, ".")
	if len(parts) == 0 {
		return nil, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	current := reflect.ValueOf(c).Elem()
	for _, part := range parts {
		current = indirectValue(current)
		if !current.IsValid() || current.Kind() != reflect.Struct {
			return nil, false
		}
		next, ok := structFieldByTOMLTag(current, part)
		if !ok {
			return nil, false
		}
		current = next
	}

	current = indirectValue(current)
	if !current.IsValid() {
		return nil, false
	}
	return current.Interface(), true
}

func (c *Config) copyFrom(other *Config) {
	c.App = other.App
	c.Server = other.Server
	c.ChatGPT = other.ChatGPT
	c.Storage = other.Storage
	c.Log = other.Log
	c.CPA = other.CPA
	c.Identity = other.Identity
	c.Credential = other.Credential
	c.paths = other.paths
}

// PublicBasePath returns the external sub-path prefix (e.g. "/image-studio")
// used to build browser-visible image URLs in OpenAI-compatible responses. It
// is normalized to have a leading slash and no trailing slash; empty means no
// prefix (local direct-access development).
func (c *Config) PublicBasePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizePublicBasePath(c.Server.PublicBasePath)
}

func normalizePublicBasePath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "/" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// SessionTTL returns the configured session cookie lifetime, defaulting to 1h.
func (c *Config) SessionTTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seconds := c.Identity.SessionTTLSeconds
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

// CredentialCacheTTL returns how long resolved plaintext credentials are
// cached, defaulting to 60s.
func (c *Config) CredentialCacheTTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seconds := c.Credential.CacheTTLSeconds
	if seconds <= 0 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

// CredentialRequestTimeout returns the internal callback HTTP timeout,
// defaulting to 20s.
func (c *Config) CredentialRequestTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seconds := c.Credential.RequestTimeout
	if seconds <= 0 {
		seconds = 20
	}
	return time.Duration(seconds) * time.Second
}

func (c *Config) CPAImageBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.CPA.BaseURL)
}

func (c *Config) CPAImageAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.CPA.APIKey)
}

func (c *Config) CPAImageConfigured() bool {
	return c.CPAImageBaseURL() != "" && c.CPAImageAPIKey() != ""
}

func (c *Config) CPAImageRequestTimeout() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CPA.RequestTimeout > 0 {
		return c.CPA.RequestTimeout
	}
	return 60
}

func (c *Config) CPAImageRouteStrategy() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeCPAImageRouteStrategy(c.CPA.RouteStrategy)
}

func (c *Config) ImageQueueConfig() (int, int, time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	maxImageConcurrency := c.Server.MaxImageConcurrency
	if maxImageConcurrency <= 0 {
		maxImageConcurrency = 8
	}
	imageQueueLimit := c.Server.ImageQueueLimit
	if imageQueueLimit <= 0 {
		imageQueueLimit = 32
	}
	timeoutSeconds := c.Server.ImageQueueTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 20
	}
	return maxImageConcurrency, imageQueueLimit, time.Duration(timeoutSeconds) * time.Second
}

func (c *Config) ImageTaskQueueTTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ttlSeconds := c.Server.ImageTaskQueueTTLSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = 600
	}
	return time.Duration(ttlSeconds) * time.Second
}

func (c *Config) validate() error {
	if c.Server.MaxImageConcurrency <= 0 {
		c.Server.MaxImageConcurrency = 8
	}
	if c.Server.ImageQueueLimit <= 0 {
		c.Server.ImageQueueLimit = 32
	}
	if c.Server.ImageQueueTimeoutSeconds <= 0 {
		c.Server.ImageQueueTimeoutSeconds = 20
	}
	if c.Server.ImageTaskQueueTTLSeconds <= 0 {
		c.Server.ImageTaskQueueTTLSeconds = 600
	}

	// Phase 7: the backend is cpa-only. ImageMode is still normalized so a
	// stale studio/mix value in an old config doesn't error on startup.
	if normalized, ok := normalizeImageMode(c.ChatGPT.ImageMode); ok {
		c.ChatGPT.ImageMode = normalized
	} else {
		c.ChatGPT.ImageMode = "cpa"
	}

	c.CPA.RouteStrategy = normalizeCPAImageRouteStrategy(c.CPA.RouteStrategy)
	c.Storage.Backend = normalizeStorageBackend(c.Storage.Backend)
	c.Storage.ConfigBackend = normalizeConfigBackend(c.Storage.ConfigBackend)
	legacyImageStorage := strings.ToLower(strings.TrimSpace(c.Storage.ImageStorage))
	if strings.TrimSpace(c.Storage.ImageConversationStorage) == "" && legacyImageStorage != "" {
		c.Storage.ImageConversationStorage = legacyImageStorage
	}
	if strings.TrimSpace(c.Storage.ImageDataStorage) == "" && legacyImageStorage != "" {
		c.Storage.ImageDataStorage = legacyImageStorage
	}
	c.Storage.ImageConversationStorage = normalizeImageStorage(c.Storage.ImageConversationStorage)
	c.Storage.ImageDataStorage = normalizeImageStorage(c.Storage.ImageDataStorage)
	if c.Storage.ImageConversationStorage != c.Storage.ImageDataStorage {
		c.Storage.ImageDataStorage = c.Storage.ImageConversationStorage
	}
	c.Storage.ImageStorage = c.Storage.ImageConversationStorage

	return nil
}

func normalizeImageMode(mode string) (string, bool) {
	// Phase 7 collapsed the backend to cpa-only; any legacy studio/mix value
	// (or empty) folds to cpa rather than erroring on an old config file.
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "studio", "mix", "cpa":
		return "cpa", true
	default:
		return "", false
	}
}

func normalizeStorageBackend(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "current", "local":
		return "current"
	case "sqlite":
		return "sqlite"
	case "redis":
		return "redis"
	default:
		return "current"
	}
}

func normalizeConfigBackend(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "redis":
		return "redis"
	default:
		return "file"
	}
}

func normalizeImageStorage(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "server":
		return "server"
	default:
		return "browser"
	}
}

func normalizeCPAImageRouteStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "images_api"
	case "images_api":
		return "images_api"
	case "codex_responses":
		return "codex_responses"
	case "auto":
		return "auto"
	default:
		return "images_api"
	}
}

func NormalizeImageModeForAPI(mode string) (string, bool) {
	return normalizeImageMode(mode)
}

func decodeOverrideFile(path string, target *Config) error {
	raw, err := loadOverrideMap(path)
	if err != nil {
		return err
	}
	sanitizeOverrideMap(raw)
	return applyOverrideMap(reflect.ValueOf(target).Elem(), raw)
}

func loadOverrideMap(path string) (map[string]any, error) {
	raw := map[string]any{}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func migrateLegacyOverrideFile(path string) (bool, error) {
	if !fileExists(path) {
		return false, nil
	}

	raw, err := loadOverrideMap(path)
	if err != nil {
		return false, err
	}
	if !sanitizeOverrideMap(raw) {
		return false, nil
	}
	if err := writeOverrideMap(path, raw); err != nil {
		return false, err
	}
	return true, nil
}

func sanitizeOverrideMap(raw map[string]any) bool {
	if raw == nil {
		return false
	}

	changed := false
	chatgptSection, ok := raw["chatgpt"].(map[string]any)
	if !ok {
		return false
	}
	if value, ok := chatgptSection["image_mode"]; ok {
		sanitized := sanitizeOverrideEntry("chatgpt", "image_mode", value)
		if !reflect.DeepEqual(sanitized, value) {
			chatgptSection["image_mode"] = sanitized
			changed = true
		}
	}
	return changed
}

func sanitizeOverrideEntry(section, key string, value any) any {
	if section != "chatgpt" || key != "image_mode" {
		return value
	}
	text, ok := value.(string)
	if !ok {
		return value
	}
	if normalized, ok := normalizeImageMode(text); ok {
		return normalized
	}
	return value
}

func writeOverrideMap(path string, raw map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create override file: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(raw); err != nil {
		return fmt.Errorf("encode override: %w", err)
	}
	return nil
}

func decodeDefaultTemplate(target *Config) error {
	_, err := toml.Decode(defaultConfigTemplate, target)
	return err
}

func applyOverrideMap(dst reflect.Value, raw map[string]any) error {
	for key, value := range raw {
		field, ok := structFieldByTOMLTag(dst, key)
		if !ok {
			continue
		}
		if err := setOverrideValue(field, value); err != nil {
			return err
		}
	}
	return nil
}

func setOverrideValue(dst reflect.Value, raw any) error {
	if !dst.CanSet() {
		return nil
	}
	dst = indirectValue(dst)
	if !dst.IsValid() {
		return nil
	}

	switch dst.Kind() {
	case reflect.Struct:
		nested, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("expected table, got %T", raw)
		}
		return applyOverrideMap(dst, nested)
	case reflect.String:
		text, ok := raw.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", raw)
		}
		dst.SetString(text)
	case reflect.Bool:
		flag, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("expected bool, got %T", raw)
		}
		dst.SetBool(flag)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch n := raw.(type) {
		case int64:
			dst.SetInt(n)
		case int:
			dst.SetInt(int64(n))
		case float64:
			dst.SetInt(int64(n))
		default:
			return fmt.Errorf("expected int, got %T", raw)
		}
	default:
		value := reflect.ValueOf(raw)
		if value.IsValid() && value.Type().AssignableTo(dst.Type()) {
			dst.Set(value)
			return nil
		}
		return fmt.Errorf("unsupported type %s", dst.Type())
	}
	return nil
}

func structFieldByTOMLTag(value reflect.Value, part string) (reflect.Value, bool) {
	valueType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		fieldType := valueType.Field(i)
		if !fieldType.IsExported() {
			continue
		}
		tag := strings.Split(fieldType.Tag.Get("toml"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(fieldType.Name)
		}
		if tag == part {
			return value.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func indirectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func resolvePaths(rootDir string) Paths {
	root := normalizeRoot(rootDir)
	return Paths{
		Root:     root,
		Defaults: filepath.Join(root, dataDirName, exampleConfigFile),
		Override: filepath.Join(root, dataDirName, userConfigFile),
	}
}

func normalizeRoot(rootDir string) string {
	if rootDir != "" {
		return rootDir
	}
	if exePath, err := osExecutable(); err == nil {
		exeDir := filepath.Dir(exePath)
		if detected := detectConfigRoot(exeDir); detected != "" {
			return detected
		}
	}
	if cwd, err := osGetwd(); err == nil {
		if detected := detectConfigRoot(cwd); detected != "" {
			return detected
		}
	}
	if exePath, err := osExecutable(); err == nil {
		if exeDir := filepath.Dir(exePath); exeDir != "" {
			return exeDir
		}
	}
	if cwd, err := osGetwd(); err == nil {
		return cwd
	}
	return "."
}

func detectConfigRoot(startDir string) string {
	dir := startDir
	for {
		// Prefer a local config root when running from backend itself or from a release package.
		if hasConfigMarker(dir) {
			return dir
		}
		// Backward compatibility: older layout placed defaults in backend/config.defaults.toml.
		if fileExists(filepath.Join(dir, "config.defaults.toml")) {
			return dir
		}
		// Support running from repo root (or any subdir) by locating backend/data config files.
		backendDir := filepath.Join(dir, "backend")
		if hasConfigMarker(backendDir) {
			return backendDir
		}
		// Backward compatibility: older layout placed defaults in backend/config.defaults.toml.
		if fileExists(filepath.Join(backendDir, "config.defaults.toml")) {
			return backendDir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func hasConfigMarker(root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	dataDir := filepath.Join(root, dataDirName)
	return fileExists(filepath.Join(dataDir, userConfigFile)) ||
		fileExists(filepath.Join(dataDir, exampleConfigFile)) ||
		fileExists(filepath.Join(dataDir, "config.defaults.toml")) ||
		fileExists(filepath.Join(root, "internal", "config", "config.defaults.toml"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func stringFallback(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func intFallback(values []int) int {
	if len(values) > 0 {
		return values[0]
	}
	return 0
}

func boolFallback(values []bool) bool {
	if len(values) > 0 {
		return values[0]
	}
	return false
}
