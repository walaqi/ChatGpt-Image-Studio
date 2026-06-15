package imagehistory

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatgpt2api/internal/config"
)

const testHistoryUser = "u1"

func newHistoryTestConfig(t *testing.T, backend string) *config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.New(root)
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Storage.Backend = backend
	cfg.Storage.ImageDir = "data/images"
	cfg.Storage.SQLitePath = "data/history.sqlite"
	cfg.Storage.RedisAddr = "127.0.0.1:6379"
	cfg.Storage.RedisPassword = "123456"
	cfg.Storage.RedisDB = 0
	cfg.Storage.RedisPrefix = "chatgpt2api:history:test:" + strings.ReplaceAll(root, "\\", ":")
	return cfg
}

func testStorePersistenceAcrossReload(t *testing.T, backend string) {
	t.Helper()
	cfg := newHistoryTestConfig(t, backend)
	store, err := NewStore(cfg)
	if err != nil {
		if backend == "redis" {
			t.Skipf("redis backend is not reachable: %v", err)
		}
		t.Fatalf("NewStore(%s): %v", backend, err)
	}
	defer store.Close()

	payload := base64.StdEncoding.EncodeToString([]byte("persist-image-bytes"))
	if _, err := store.Save(context.Background(), testHistoryUser, Conversation{
		ID:        "persist-conv",
		Title:     "生成",
		Mode:      "generate",
		Prompt:    "persist",
		Model:     "gpt-image-2",
		Count:     1,
		CreatedAt: "2026-04-26T00:00:00Z",
		Status:    "success",
		Turns: []Turn{{
			ID:        "persist-turn",
			Title:     "生成",
			Mode:      "generate",
			Prompt:    "persist",
			Model:     "gpt-image-2",
			Count:     1,
			CreatedAt: "2026-04-26T00:00:00Z",
			Status:    "success",
			Images:    []Image{{ID: "persist-image", Status: "success", B64JSON: payload}},
		}},
	}); err != nil {
		t.Fatalf("Save(%s): %v", backend, err)
	}

	reloaded, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("Reloaded NewStore(%s): %v", backend, err)
	}
	defer reloaded.Close()

	items, err := reloaded.List(context.Background(), testHistoryUser)
	if err != nil {
		t.Fatalf("List(%s): %v", backend, err)
	}
	if len(items) != 1 || items[0].ID != "persist-conv" {
		t.Fatalf("reloaded items(%s) = %#v", backend, items)
	}
	if got := items[0].Turns[0].Images[0].URL; !strings.HasPrefix(got, "/v1/files/image/u1/result-") {
		t.Fatalf("reloaded image url(%s) = %q", backend, got)
	}
}

func TestFileStoreExtractsImagesToServerDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := config.New(root)
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Storage.Backend = "current"
	cfg.Storage.ImageDir = "data/images"

	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	payload := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	created, err := store.Save(context.Background(), testHistoryUser, Conversation{
		ID:        "conv-1",
		Title:     "生成",
		Mode:      "generate",
		Prompt:    "test",
		Model:     "gpt-image-2",
		Count:     1,
		CreatedAt: "2026-04-26T00:00:00Z",
		Status:    "success",
		Turns: []Turn{
			{
				ID:        "turn-1",
				Title:     "生成",
				Mode:      "generate",
				Prompt:    "test",
				Model:     "gpt-image-2",
				Count:     1,
				CreatedAt: "2026-04-26T00:00:00Z",
				Status:    "success",
				SourceImages: []SourceImage{
					{ID: "source-1", Role: "image", Name: "source.png", DataURL: "data:image/png;base64," + payload},
				},
				Images: []Image{
					{ID: "image-1", Status: "success", B64JSON: payload},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := created.Turns[0].Images[0].B64JSON; got != "" {
		t.Fatalf("B64JSON should be stripped from stored history, got %q", got)
	}
	if got := created.Turns[0].Images[0].URL; !strings.HasPrefix(got, "/v1/files/image/u1/result-") {
		t.Fatalf("stored result URL = %q", got)
	}
	if got := created.Turns[0].SourceImages[0].DataURL; got != "" {
		t.Fatalf("DataURL should be stripped from stored source, got %q", got)
	}
	if got := created.Turns[0].SourceImages[0].URL; !strings.HasPrefix(got, "/v1/files/image/u1/source-") {
		t.Fatalf("stored source URL = %q", got)
	}

	matches, err := filepath.Glob(filepath.Join(root, "data", "images", testHistoryUser, "*.png"))
	if err != nil {
		t.Fatalf("glob image dir: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 server image files, got %d", len(matches))
	}
	for _, path := range matches {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected image file %s: %v", path, err)
		}
	}

	items, err := store.List(context.Background(), testHistoryUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ID != "conv-1" {
		t.Fatalf("List returned %#v", items)
	}
}

func TestDeleteOnlyRemovesUnreferencedImageFiles(t *testing.T) {
	root := t.TempDir()
	cfg := config.New(root)
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Storage.Backend = "current"
	cfg.Storage.ImageDir = "data/images"

	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	payload := base64.StdEncoding.EncodeToString([]byte("shared-image-bytes"))
	first, err := store.Save(context.Background(), testHistoryUser, Conversation{
		ID:        "conv-1",
		Title:     "生成",
		Mode:      "generate",
		Prompt:    "one",
		Model:     "gpt-image-2",
		Count:     1,
		CreatedAt: "2026-04-26T00:00:00Z",
		Status:    "success",
		Turns: []Turn{{
			ID:        "turn-1",
			Title:     "生成",
			Mode:      "generate",
			Prompt:    "one",
			Model:     "gpt-image-2",
			Count:     1,
			CreatedAt: "2026-04-26T00:00:00Z",
			Status:    "success",
			Images:    []Image{{ID: "image-1", Status: "success", B64JSON: payload}},
		}},
	})
	if err != nil {
		t.Fatalf("Save first conversation: %v", err)
	}
	second, err := store.Save(context.Background(), testHistoryUser, Conversation{
		ID:        "conv-2",
		Title:     "生成",
		Mode:      "generate",
		Prompt:    "two",
		Model:     "gpt-image-2",
		Count:     1,
		CreatedAt: "2026-04-26T00:00:01Z",
		Status:    "success",
		Turns: []Turn{{
			ID:        "turn-2",
			Title:     "生成",
			Mode:      "generate",
			Prompt:    "two",
			Model:     "gpt-image-2",
			Count:     1,
			CreatedAt: "2026-04-26T00:00:01Z",
			Status:    "success",
			Images:    []Image{{ID: "image-2", Status: "success", B64JSON: payload}},
		}},
	})
	if err != nil {
		t.Fatalf("Save second conversation: %v", err)
	}

	sharedFilename := strings.TrimPrefix(first.Turns[0].Images[0].URL, "/v1/files/image/")
	if sharedFilename != strings.TrimPrefix(second.Turns[0].Images[0].URL, "/v1/files/image/") {
		t.Fatalf("expected deduplicated shared file, got %q and %q", first.Turns[0].Images[0].URL, second.Turns[0].Images[0].URL)
	}
	sharedPath := filepath.Join(root, "data", "images", sharedFilename)
	if _, err := os.Stat(sharedPath); err != nil {
		t.Fatalf("expected shared image file to exist: %v", err)
	}

	if err := store.Delete(context.Background(), testHistoryUser, "conv-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(sharedPath); err != nil {
		t.Fatalf("shared image should still exist after deleting one conversation: %v", err)
	}

	if err := store.Delete(context.Background(), testHistoryUser, "conv-2"); err != nil {
		t.Fatalf("Delete second: %v", err)
	}
	if _, err := os.Stat(sharedPath); !os.IsNotExist(err) {
		t.Fatalf("shared image should be removed after deleting last reference, err=%v", err)
	}
}

// TestCleanupTouchesOnlyOwnUserDir is the §8 item-6 regression: when two users
// store the same image bytes, deleting/clearing one user's history must remove
// only that user's copy from its own subdir, never touch the other user's file.
// Per-user subdirs mean identical bytes hash to the same filename but live in
// separate directories, so cleanup must be scoped by userID.
func TestCleanupTouchesOnlyOwnUserDir(t *testing.T) {
	const (
		userA = "alice"
		userB = "bob"
	)
	root := t.TempDir()
	cfg := config.New(root)
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Storage.Backend = "current"
	cfg.Storage.ImageDir = "data/images"

	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Identical bytes for both users → same content hash, different subdirs.
	payload := base64.StdEncoding.EncodeToString([]byte("shared-across-tenants"))
	saveFor := func(userID, convID string) *Conversation {
		t.Helper()
		saved, err := store.Save(context.Background(), userID, Conversation{
			ID:        convID,
			Title:     "生成",
			Mode:      "generate",
			Prompt:    "x",
			Model:     "gpt-image-2",
			Count:     1,
			CreatedAt: "2026-04-26T00:00:00Z",
			Status:    "success",
			Turns: []Turn{{
				ID:        convID + "-turn",
				Title:     "生成",
				Mode:      "generate",
				Prompt:    "x",
				Model:     "gpt-image-2",
				Count:     1,
				CreatedAt: "2026-04-26T00:00:00Z",
				Status:    "success",
				Images:    []Image{{ID: convID + "-img", Status: "success", B64JSON: payload}},
			}},
		})
		if err != nil {
			t.Fatalf("Save(%s): %v", userID, err)
		}
		return saved
	}

	aConv := saveFor(userA, "conv-a")
	bConv := saveFor(userB, "conv-b")

	aFile := filenameFromImageURL(aConv.Turns[0].Images[0].URL)
	bFile := filenameFromImageURL(bConv.Turns[0].Images[0].URL)
	if aFile == "" || bFile == "" {
		t.Fatalf("missing image filenames: a=%q b=%q", aFile, bFile)
	}
	// Dedup is content-addressed, so the basenames match; isolation comes from
	// the per-user subdir, not the filename.
	if aFile != bFile {
		t.Fatalf("expected identical content-addressed basenames, got a=%q b=%q", aFile, bFile)
	}

	aPath := filepath.Join(root, "data", "images", userA, aFile)
	bPath := filepath.Join(root, "data", "images", userB, bFile)
	if _, err := os.Stat(aPath); err != nil {
		t.Fatalf("userA image should exist: %v", err)
	}
	if _, err := os.Stat(bPath); err != nil {
		t.Fatalf("userB image should exist: %v", err)
	}

	// Deleting userA's conversation must remove only userA's file.
	if err := store.Delete(context.Background(), userA, "conv-a"); err != nil {
		t.Fatalf("Delete userA: %v", err)
	}
	if _, err := os.Stat(aPath); !os.IsNotExist(err) {
		t.Fatalf("userA image should be gone after delete, err=%v", err)
	}
	if _, err := os.Stat(bPath); err != nil {
		t.Fatalf("userB image must survive userA's delete: %v", err)
	}

	// Clearing userB must not error and must remove only userB's file.
	if err := store.Clear(context.Background(), userB); err != nil {
		t.Fatalf("Clear userB: %v", err)
	}
	if _, err := os.Stat(bPath); !os.IsNotExist(err) {
		t.Fatalf("userB image should be gone after clear, err=%v", err)
	}
}

func TestSQLiteStorePersistsImageHistoryAcrossReload(t *testing.T) {
	testStorePersistenceAcrossReload(t, "sqlite")
}

func TestRedisStorePersistsImageHistoryAcrossReload(t *testing.T) {
	testStorePersistenceAcrossReload(t, "redis")
}
