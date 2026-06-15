package api

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"chatgpt2api/internal/identity"
)

const defaultImageDir = "data/tmp/image"

// downloadAndCache downloads an upstream image using the image client's transport
// (Chrome TLS fingerprint), saves to local disk, and returns the local filename.
func downloadAndCache(client imageDownloader, upstreamURL string, cacheDir string) (string, error) {
	// Generate a stable filename from the URL
	hash := sha256.Sum256([]byte(upstreamURL))
	filename := fmt.Sprintf("%x.png", hash[:12])
	dir := firstNonEmpty(cacheDir, defaultImageDir)
	localPath := filepath.Join(dir, filename)

	// Check cache
	if _, err := os.Stat(localPath); err == nil {
		return filename, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	data, err := client.DownloadBytes(upstreamURL)
	if err != nil {
		return "", fmt.Errorf("download upstream image: %w", err)
	}

	tmpFile := localPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpFile, localPath); err != nil {
		return "", err
	}

	slog.Info("cached image", "file", filename, "size", len(data))
	return filename, nil
}

// gatewayImageURL builds the public absolute URL for a cached image. filename
// already includes the per-user segment (e.g. "<userID>/<file>"). basePath is
// the public sub-path prefix (e.g. "/image-studio") so the URL is reachable
// through the reverse proxy; it may be empty for local/direct deployments.
func gatewayImageURL(r *http.Request, basePath, filename string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	return fmt.Sprintf("%s://%s%s/v1/files/image/%s", scheme, host, basePath, filename)
}

// resolveImageFilePath locates a stored image for the given userID. Files live
// under <imageDir>/<userID>/<filename>; a blank userID falls back to the flat
// legacy layout. The userID segment is sanitized to a single path element so a
// caller cannot escape its own subdirectory.
func (s *Server) resolveImageFilePath(userID, name string) string {
	baseName := filepath.Base(strings.TrimSpace(name))
	if baseName == "" || baseName == "." || baseName == "/" {
		return ""
	}
	userSeg := sanitizeUserSegment(userID)

	candidates := []string{}
	if userSeg != "" {
		candidates = append(candidates,
			filepath.Join(s.cfg.ResolvePath(s.cfg.Storage.ImageDir), userSeg, baseName),
			filepath.Join(s.cfg.ResolvePath(defaultImageDir), userSeg, baseName),
		)
	}
	// Legacy flat layout (assets written before multi-tenant migration).
	candidates = append(candidates,
		filepath.Join(s.cfg.ResolvePath(s.cfg.Storage.ImageDir), baseName),
		filepath.Join(s.cfg.ResolvePath(defaultImageDir), baseName),
	)
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return s.searchImageFilePathFallback(baseName)
}

// sanitizeUserSegment reduces a userID to a safe single path element. It mirrors
// imagehistory.sanitizeUserID: only the basename is kept and traversal tokens
// are rejected.
func sanitizeUserSegment(userID string) string {
	trimmed := strings.TrimSpace(userID)
	if trimmed == "" {
		return ""
	}
	trimmed = filepath.Base(filepath.Clean(trimmed))
	if trimmed == "." || trimmed == ".." || trimmed == "/" || strings.ContainsAny(trimmed, `/\`) {
		return ""
	}
	return trimmed
}

func (s *Server) searchImageFilePathFallback(name string) string {
	baseName := filepath.Base(strings.TrimSpace(name))
	if baseName == "" || s == nil || s.cfg == nil {
		return ""
	}

	dataRoot := filepath.Join(s.cfg.Paths().Root, "data")
	info, err := os.Stat(dataRoot)
	if err != nil || !info.IsDir() {
		return ""
	}

	var found string
	_ = filepath.WalkDir(dataRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.EqualFold(entry.Name(), baseName) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		found = path
		return fs.SkipAll
	})
	return found
}

// handleImageFile serves cached and server-stored images from storage.image_dir.
func (s *Server) handleImageFile(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/v1/files/image/")
	if raw == "" {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}

	// The path may be "<userID>/<filename>" (multi-tenant) or a bare
	// "<filename>" (legacy flat layout). Split off the optional owner segment.
	owner, filename := splitImageOwnerPath(raw)

	// Ownership enforcement: when the path is namespaced by userID, the caller's
	// session userID must match. This blocks cross-tenant downloads. The session
	// userID is injected by requireSession (the route is gated).
	sessionUser, _ := identity.UserIDFromContext(r.Context())
	if owner != "" && sessionUser != "" && owner != sessionUser {
		writeError(w, http.StatusForbidden, "image not found")
		return
	}

	path := s.resolveImageFilePath(owner, filename)
	if path == "" {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	contentTypes := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
	}
	ct := contentTypes[ext]
	if ct == "" {
		ct = "image/png"
	}

	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", ct)
	http.ServeFile(w, r, path)
}

// splitImageOwnerPath splits a "<userID>/<filename>" image path into its owner
// segment and filename. A bare "<filename>" (no slash) returns an empty owner,
// preserving the legacy flat layout. Only the first segment is treated as the
// owner; any deeper path is flattened to its basename by the caller.
func splitImageOwnerPath(raw string) (owner, filename string) {
	raw = strings.TrimSpace(strings.Trim(raw, "/"))
	if raw == "" {
		return "", ""
	}
	if idx := strings.Index(raw, "/"); idx >= 0 {
		return raw[:idx], raw[idx+1:]
	}
	return "", raw
}
