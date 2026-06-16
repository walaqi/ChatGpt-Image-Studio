package imagehistory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"chatgpt2api/internal/config"

	"github.com/redis/go-redis/v9"
	_ "modernc.org/sqlite"
)

const (
	defaultHistoryDir = "data/image_history"
	defaultAssetMIME  = "image/png"
)

type SourceImage struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Name    string `json:"name"`
	DataURL string `json:"dataUrl,omitempty"`
	URL     string `json:"url,omitempty"`
}

type Image struct {
	ID              string `json:"id"`
	Status          string `json:"status,omitempty"`
	B64JSON         string `json:"b64_json,omitempty"`
	URL             string `json:"url,omitempty"`
	RevisedPrompt   string `json:"revised_prompt,omitempty"`
	FileID          string `json:"file_id,omitempty"`
	GenID           string `json:"gen_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	ParentMessageID string `json:"parent_message_id,omitempty"`
	SourceAccountID string `json:"source_account_id,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Turn struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Mode         string        `json:"mode"`
	Prompt       string        `json:"prompt"`
	Model        string        `json:"model"`
	Count        int           `json:"count"`
	Size         string        `json:"size,omitempty"`
	Quality      string        `json:"quality,omitempty"`
	Scale        string        `json:"scale,omitempty"`
	SourceImages []SourceImage `json:"sourceImages,omitempty"`
	Images       []Image       `json:"images"`
	CreatedAt    string        `json:"createdAt"`
	Status       string        `json:"status"`
	Error        string        `json:"error,omitempty"`
}

type Conversation struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Mode         string        `json:"mode"`
	Prompt       string        `json:"prompt"`
	Model        string        `json:"model"`
	Count        int           `json:"count"`
	Size         string        `json:"size,omitempty"`
	Quality      string        `json:"quality,omitempty"`
	Scale        string        `json:"scale,omitempty"`
	SourceImages []SourceImage `json:"sourceImages,omitempty"`
	Images       []Image       `json:"images"`
	CreatedAt    string        `json:"createdAt"`
	Status       string        `json:"status"`
	Error        string        `json:"error,omitempty"`
	Turns        []Turn        `json:"turns,omitempty"`
}

type Store struct {
	backend  backend
	imageDir string
}

// backend persists conversations partitioned by userID. Every method takes a
// userID so each tenant sees only its own history. An empty userID is the
// legacy/single-tenant namespace (used by the default bearer auth path).
type backend interface {
	Init() error
	Close() error
	List(ctx context.Context, userID string) ([]Conversation, error)
	Get(ctx context.Context, userID, id string) (*Conversation, error)
	Save(ctx context.Context, userID string, conversation Conversation) error
	Delete(ctx context.Context, userID, id string) error
	Clear(ctx context.Context, userID string) error
}

func NewStore(cfg *config.Config) (*Store, error) {
	imageDir := cfg.ResolvePath(cfg.Storage.ImageDir)
	var storage backend
	switch strings.ToLower(strings.TrimSpace(cfg.Storage.Backend)) {
	case "sqlite":
		storage = &sqliteBackend{path: cfg.ResolvePath(cfg.Storage.SQLitePath)}
	case "redis":
		storage = &redisBackend{
			client: redis.NewClient(&redis.Options{
				Addr:     strings.TrimSpace(cfg.Storage.RedisAddr),
				Password: cfg.Storage.RedisPassword,
				DB:       cfg.Storage.RedisDB,
			}),
			prefix: firstNonEmpty(cfg.Storage.RedisPrefix, "chatgpt2api:studio") + ":image_history",
		}
	default:
		storage = &fileBackend{dir: cfg.ResolvePath(defaultHistoryDir)}
	}
	if err := storage.Init(); err != nil {
		_ = storage.Close()
		return nil, err
	}
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		_ = storage.Close()
		return nil, err
	}
	return &Store{backend: storage, imageDir: imageDir}, nil
}

func (s *Store) Close() error {
	if s == nil || s.backend == nil {
		return nil
	}
	return s.backend.Close()
}

func (s *Store) List(ctx context.Context, userID string) ([]Conversation, error) {
	items, err := s.backend.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	sortConversations(items)
	return items, nil
}

func (s *Store) Get(ctx context.Context, userID, id string) (*Conversation, error) {
	return s.backend.Get(ctx, userID, cleanID(id))
}

func (s *Store) Save(ctx context.Context, userID string, conversation Conversation) (*Conversation, error) {
	normalized, err := s.normalizeConversation(userID, conversation)
	if err != nil {
		return nil, err
	}
	if err := s.backend.Save(ctx, userID, normalized); err != nil {
		return nil, err
	}
	return &normalized, nil
}

func (s *Store) Delete(ctx context.Context, userID, id string) error {
	current, err := s.backend.Get(ctx, userID, cleanID(id))
	if err != nil || current == nil {
		return err
	}
	candidateFiles := collectConversationImageFiles(*current)
	if err := s.backend.Delete(ctx, userID, cleanID(id)); err != nil {
		return err
	}
	return s.cleanupCandidateFiles(ctx, userID, candidateFiles)
}

func (s *Store) Clear(ctx context.Context, userID string) error {
	items, err := s.backend.List(ctx, userID)
	if err != nil {
		return err
	}
	candidateFiles := map[string]struct{}{}
	for _, item := range items {
		mergeFileSets(candidateFiles, collectConversationImageFiles(item))
	}
	if err := s.backend.Clear(ctx, userID); err != nil {
		return err
	}
	return s.cleanupCandidateFiles(ctx, userID, candidateFiles)
}

func (s *Store) normalizeConversation(userID string, conversation Conversation) (Conversation, error) {
	conversation.ID = cleanID(conversation.ID)
	if conversation.ID == "" {
		return Conversation{}, fmt.Errorf("conversation id is required")
	}
	if conversation.CreatedAt == "" {
		conversation.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if len(conversation.Turns) == 0 {
		conversation.Turns = []Turn{{
			ID:           conversation.ID + "-legacy",
			Title:        conversation.Title,
			Mode:         conversation.Mode,
			Prompt:       conversation.Prompt,
			Model:        conversation.Model,
			Count:        conversation.Count,
			Size:         conversation.Size,
			Quality:      conversation.Quality,
			Scale:        conversation.Scale,
			SourceImages: conversation.SourceImages,
			Images:       conversation.Images,
			CreatedAt:    conversation.CreatedAt,
			Status:       conversation.Status,
			Error:        conversation.Error,
		}}
	}
	for turnIndex := range conversation.Turns {
		turn := &conversation.Turns[turnIndex]
		if turn.ID == "" {
			turn.ID = fmt.Sprintf("%s-%d", conversation.ID, turnIndex)
		}
		if turn.CreatedAt == "" {
			turn.CreatedAt = conversation.CreatedAt
		}
		for sourceIndex := range turn.SourceImages {
			source := &turn.SourceImages[sourceIndex]
			if source.ID == "" {
				source.ID = fmt.Sprintf("%s-source-%d", turn.ID, sourceIndex)
			}
			if source.URL == "" && strings.TrimSpace(source.DataURL) != "" {
				url, err := s.saveDataURLAsset(userID, source.DataURL, "source", source.Name)
				if err != nil {
					return Conversation{}, err
				}
				source.URL = url
				source.DataURL = ""
			}
		}
		for imageIndex := range turn.Images {
			image := &turn.Images[imageIndex]
			if image.ID == "" {
				image.ID = fmt.Sprintf("%s-image-%d", turn.ID, imageIndex)
			}
			if image.URL == "" && strings.TrimSpace(image.B64JSON) != "" {
				url, err := s.saveBase64Asset(userID, image.B64JSON, "result", defaultAssetMIME)
				if err != nil {
					return Conversation{}, err
				}
				image.URL = url
				image.B64JSON = ""
			}
			if image.Status == "" {
				if image.URL != "" {
					image.Status = "success"
				} else {
					image.Status = "loading"
				}
			}
		}
	}
	latest := conversation.Turns[len(conversation.Turns)-1]
	conversation.Title = latest.Title
	conversation.Mode = latest.Mode
	conversation.Prompt = latest.Prompt
	conversation.Model = latest.Model
	conversation.Count = latest.Count
	conversation.Size = latest.Size
	conversation.Quality = latest.Quality
	conversation.Scale = latest.Scale
	conversation.SourceImages = latest.SourceImages
	conversation.Images = latest.Images
	conversation.CreatedAt = latest.CreatedAt
	conversation.Status = latest.Status
	conversation.Error = latest.Error
	return conversation, nil
}

func (s *Store) saveDataURLAsset(userID, raw, kind, name string) (string, error) {
	payload, mimeType, err := decodeDataURL(raw)
	if err != nil {
		return "", err
	}
	return s.saveAsset(userID, payload, kind, firstNonEmpty(mimeType, mime.TypeByExtension(filepath.Ext(name)), defaultAssetMIME))
}

func (s *Store) saveBase64Asset(userID, raw, kind, mimeType string) (string, error) {
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	return s.saveAsset(userID, payload, kind, firstNonEmpty(mimeType, defaultAssetMIME))
}

// saveAsset writes a content-addressed image file under the user's own
// subdirectory and returns its internal relative URL
// (/v1/files/image/<userID>/<filename>). Per-user subdirs prevent cross-tenant
// hash sharing: the same bytes from two users live in separate files, so one
// user deleting a conversation can never remove another user's asset.
func (s *Store) saveAsset(userID string, payload []byte, kind, mimeType string) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("image is empty")
	}
	dir, urlPrefix := s.userImagePaths(userID)
	sum := sha256.Sum256(payload)
	ext := extensionForMIME(mimeType)
	filename := fmt.Sprintf("%s-%x%s", sanitizeKind(kind), sum[:16], ext)
	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); err == nil {
		return urlPrefix + filename, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return urlPrefix + filename, nil
}

// userImagePaths returns the on-disk directory and the internal URL prefix for a
// user's image assets. A blank userID falls back to the flat layout for
// single-tenant/legacy compatibility.
func (s *Store) userImagePaths(userID string) (dir string, urlPrefix string) {
	seg := sanitizeUserID(userID)
	if seg == "" {
		return s.imageDir, "/v1/files/image/"
	}
	return filepath.Join(s.imageDir, seg), "/v1/files/image/" + seg + "/"
}

func decodeDataURL(raw string) ([]byte, string, error) {
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return nil, "", fmt.Errorf("invalid data url")
	}
	meta := raw[:comma]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", fmt.Errorf("only base64 data urls are supported")
	}
	mimeType := strings.TrimPrefix(strings.Split(meta, ";")[0], "data:")
	payload, err := base64.StdEncoding.DecodeString(raw[comma+1:])
	if err != nil {
		return nil, "", fmt.Errorf("decode data url: %w", err)
	}
	return payload, mimeType, nil
}

func extensionForMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func sanitizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "source", "mask", "result":
		return strings.ToLower(strings.TrimSpace(kind))
	default:
		return "image"
	}
}

func collectConversationImageFiles(conversation Conversation) map[string]struct{} {
	files := map[string]struct{}{}
	collectSourceFiles := func(items []SourceImage) {
		for _, item := range items {
			if filename := filenameFromImageURL(item.URL); filename != "" {
				files[filename] = struct{}{}
			}
		}
	}
	collectResultFiles := func(items []Image) {
		for _, item := range items {
			if filename := filenameFromImageURL(item.URL); filename != "" {
				files[filename] = struct{}{}
			}
		}
	}
	collectSourceFiles(conversation.SourceImages)
	collectResultFiles(conversation.Images)
	for _, turn := range conversation.Turns {
		collectSourceFiles(turn.SourceImages)
		collectResultFiles(turn.Images)
	}
	return files
}

func filenameFromImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	index := strings.LastIndex(trimmed, "/v1/files/image/")
	if index >= 0 {
		return filepath.Base(trimmed[index+len("/v1/files/image/"):])
	}
	return filepath.Base(trimmed)
}

func mergeFileSets(target map[string]struct{}, source map[string]struct{}) {
	for key := range source {
		target[key] = struct{}{}
	}
}

func (s *Store) cleanupCandidateFiles(ctx context.Context, userID string, candidates map[string]struct{}) error {
	if len(candidates) == 0 {
		return nil
	}
	// Only the same user's remaining conversations can still reference these
	// files; per-user subdirs guarantee no cross-tenant sharing.
	remainingItems, err := s.backend.List(ctx, userID)
	if err != nil {
		return err
	}
	stillReferenced := map[string]struct{}{}
	for _, item := range remainingItems {
		mergeFileSets(stillReferenced, collectConversationImageFiles(item))
	}
	dir, _ := s.userImagePaths(userID)
	for filename := range candidates {
		if _, exists := stillReferenced[filename]; exists {
			continue
		}
		path := filepath.Join(dir, filepath.Base(filename))
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func sortConversations(items []Conversation) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})
}

func cleanID(id string) string {
	return strings.ReplaceAll(strings.TrimSpace(id), "/", "-")
}

// sanitizeUserID makes a userID safe to use as a single path segment. It strips
// any path separators and traversal sequences so a malicious or malformed
// userID can never escape the image directory. A blank result signals the
// caller to use the flat (single-tenant/legacy) layout.
func sanitizeUserID(userID string) string {
	trimmed := strings.TrimSpace(userID)
	if trimmed == "" {
		return ""
	}
	// Replace separators and collapse traversal; keep it conservative.
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_")
	safe := replacer.Replace(trimmed)
	safe = strings.Trim(safe, ".")
	if safe == "" {
		return ""
	}
	return safe
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

type fileBackend struct {
	dir string
}

func (b *fileBackend) Init() error {
	return os.MkdirAll(b.dir, 0o755)
}

func (b *fileBackend) Close() error {
	return nil
}

// userDir returns the per-user conversation directory. A blank userID falls
// back to the base dir for single-tenant/legacy layout.
func (b *fileBackend) userDir(userID string) string {
	seg := sanitizeUserID(userID)
	if seg == "" {
		return b.dir
	}
	return filepath.Join(b.dir, seg)
}

func (b *fileBackend) List(ctx context.Context, userID string) ([]Conversation, error) {
	dir := b.userDir(userID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Conversation{}, nil
		}
		return nil, err
	}
	result := make([]Conversation, 0, len(entries))
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		conversation, err := b.read(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		result = append(result, conversation)
	}
	return result, nil
}

func (b *fileBackend) Get(_ context.Context, userID, id string) (*Conversation, error) {
	conversation, err := b.read(b.path(userID, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &conversation, nil
}

func (b *fileBackend) Save(_ context.Context, userID string, conversation Conversation) error {
	dir := b.userDir(userID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(conversation, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := b.path(userID, conversation.ID) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path(userID, conversation.ID))
}

func (b *fileBackend) Delete(_ context.Context, userID, id string) error {
	err := os.Remove(b.path(userID, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (b *fileBackend) Clear(_ context.Context, userID string) error {
	dir := b.userDir(userID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (b *fileBackend) read(path string) (Conversation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Conversation{}, err
	}
	var conversation Conversation
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return Conversation{}, err
	}
	return conversation, nil
}

func (b *fileBackend) path(userID, id string) string {
	return filepath.Join(b.userDir(userID), cleanID(id)+".json")
}

type sqliteBackend struct {
	path string
	db   *sql.DB
}

func (b *sqliteBackend) Init() error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", b.path)
	if err != nil {
		return err
	}
	b.db = db
	if _, err = b.db.Exec(`CREATE TABLE IF NOT EXISTS image_conversations (id TEXT PRIMARY KEY, raw_json BLOB NOT NULL, updated_at TEXT NOT NULL);`); err != nil {
		return err
	}
	return b.migrateUserIDColumn()
}

// migrateUserIDColumn adds the user_id column + index to pre-existing databases
// (§5). Phase 7 made image-studio cpa-only multi-tenant with no single-tenant
// fallback, so legacy rows (written before the column existed) carry an empty
// user_id that cannot be attributed to any tenant; they are purged on migration
// rather than leaked to an arbitrary owner. After migration every row is
// written with a real session-derived user_id, so no new empty rows appear.
func (b *sqliteBackend) migrateUserIDColumn() error {
	hasColumn, err := b.columnExists("image_conversations", "user_id")
	if err != nil {
		return err
	}
	if !hasColumn {
		if _, err := b.db.Exec(`ALTER TABLE image_conversations ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		// Drop unattributable legacy rows surfaced by the migration.
		if _, err := b.db.Exec(`DELETE FROM image_conversations WHERE user_id = ''`); err != nil {
			return err
		}
	}
	_, err = b.db.Exec(`CREATE INDEX IF NOT EXISTS idx_conv_user_updated ON image_conversations(user_id, updated_at)`)
	return err
}

func (b *sqliteBackend) columnExists(table, column string) (bool, error) {
	rows, err := b.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notnull    int
			dfltValue  any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (b *sqliteBackend) Close() error {
	if b.db == nil {
		return nil
	}
	return b.db.Close()
}

func (b *sqliteBackend) List(_ context.Context, userID string) ([]Conversation, error) {
	rows, err := b.db.Query(`SELECT raw_json FROM image_conversations WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Conversation{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var conversation Conversation
		if err := json.Unmarshal(raw, &conversation); err != nil {
			continue
		}
		result = append(result, conversation)
	}
	return result, rows.Err()
}

func (b *sqliteBackend) Get(_ context.Context, userID, id string) (*Conversation, error) {
	var raw []byte
	err := b.db.QueryRow(`SELECT raw_json FROM image_conversations WHERE user_id = ? AND id = ?`, userID, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var conversation Conversation
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return nil, err
	}
	return &conversation, nil
}

func (b *sqliteBackend) Save(_ context.Context, userID string, conversation Conversation) error {
	raw, err := json.Marshal(conversation)
	if err != nil {
		return err
	}
	_, err = b.db.Exec(
		`INSERT INTO image_conversations(id, user_id, raw_json, updated_at) VALUES(?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET raw_json = excluded.raw_json, updated_at = excluded.updated_at, user_id = excluded.user_id`,
		conversation.ID,
		userID,
		raw,
		conversation.CreatedAt,
	)
	return err
}

func (b *sqliteBackend) Delete(_ context.Context, userID, id string) error {
	_, err := b.db.Exec(`DELETE FROM image_conversations WHERE id = ?`, id)
	return err
}

func (b *sqliteBackend) Clear(_ context.Context, userID string) error {
	_, err := b.db.Exec(`DELETE FROM image_conversations WHERE user_id = ?`, userID)
	return err
}

type redisBackend struct {
	client *redis.Client
	prefix string
}

func (b *redisBackend) Init() error {
	return b.client.Ping(context.Background()).Err()
}

func (b *redisBackend) Close() error {
	if b.client == nil {
		return nil
	}
	return b.client.Close()
}

func (b *redisBackend) List(ctx context.Context, userID string) ([]Conversation, error) {
	values, err := b.client.HGetAll(ctx, b.key(userID, "conversations")).Result()
	if err != nil {
		return nil, err
	}
	result := make([]Conversation, 0, len(values))
	for _, raw := range values {
		var conversation Conversation
		if err := json.Unmarshal([]byte(raw), &conversation); err != nil {
			continue
		}
		result = append(result, conversation)
	}
	return result, nil
}

func (b *redisBackend) Get(ctx context.Context, userID, id string) (*Conversation, error) {
	raw, err := b.client.HGet(ctx, b.key(userID, "conversations"), id).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var conversation Conversation
	if err := json.Unmarshal([]byte(raw), &conversation); err != nil {
		return nil, err
	}
	return &conversation, nil
}

func (b *redisBackend) Save(ctx context.Context, userID string, conversation Conversation) error {
	raw, err := json.Marshal(conversation)
	if err != nil {
		return err
	}
	return b.client.HSet(ctx, b.key(userID, "conversations"), conversation.ID, raw).Err()
}

func (b *redisBackend) Delete(ctx context.Context, userID, id string) error {
	return b.client.HDel(ctx, b.key(userID, "conversations"), id).Err()
}

func (b *redisBackend) Clear(ctx context.Context, userID string) error {
	return b.client.Del(ctx, b.key(userID, "conversations")).Err()
}

// key namespaces the hash per user: <prefix>:<userID>:conversations. A blank
// userID falls back to the legacy flat key for single-tenant compatibility.
func (b *redisBackend) key(userID, name string) string {
	if seg := sanitizeUserID(userID); seg != "" {
		return b.prefix + ":" + seg + ":" + name
	}
	return b.prefix + ":" + name
}
