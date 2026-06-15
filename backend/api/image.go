package api

import (
	"context"
	"net/http"
	"path/filepath"

	"chatgpt2api/handler"
)

// buildImageResponse converts ImageResults to the OpenAI-compatible response
// format. Only includes url/b64_json and revised_prompt — no internal ChatGPT
// fields.
//
// Multi-tenant: cached files live under the user's own subdirectory
// (<imageBaseDir>/<userID>/) and the returned absolute URL carries both the
// <userID> path segment and publicBasePath (e.g. /image-studio) so a same-origin
// reverse proxy routes it back to this backend and per-user ownership can be
// enforced when the file is served. A blank userID keeps the flat legacy layout.
func buildImageResponse(r *http.Request, client imageDownloader, results []handler.ImageResult, responseFormat, sourceAccountID, userID, imageBaseDir, publicBasePath string) []map[string]any {
	urlSeg := userImageURLSegment(userID)
	cacheDir := imageBaseDir
	if urlSeg != "" {
		cacheDir = filepath.Join(imageBaseDir, urlSeg)
	}
	return buildImageResponseItems(
		r.Context(),
		client,
		results,
		responseFormat,
		sourceAccountID,
		cacheDir,
		func(filename string) string {
			rel := filename
			if urlSeg != "" {
				rel = urlSeg + "/" + filename
			}
			return gatewayImageURL(r, publicBasePath, rel)
		},
	)
}

func buildImageResponseItems(
	ctx context.Context,
	client imageDownloader,
	results []handler.ImageResult,
	responseFormat string,
	sourceAccountID string,
	cacheDir string,
	urlBuilder func(string) string,
) []map[string]any {
	data := make([]map[string]any, 0, len(results))
	for index, img := range results {
		item := map[string]any{
			"id":                firstNonEmpty(img.FileID, img.GenID, img.URL, "image"),
			"revised_prompt":    img.RevisedPrompt,
			"file_id":           img.FileID,
			"gen_id":            img.GenID,
			"conversation_id":   img.ConversationID,
			"parent_message_id": img.ParentMsgID,
		}
		if sourceAccountID != "" {
			item["source_account_id"] = sourceAccountID
		}
		if responseFormat == "b64_json" {
			b64, err := client.DownloadAsBase64(ctx, img.URL)
			if err != nil {
				item["url"] = img.URL
			} else {
				item["b64_json"] = b64
			}
		} else {
			filename, err := downloadAndCache(client, img.URL, cacheDir)
			if err != nil {
				item["url"] = img.URL
			} else {
				item["url"] = urlBuilder(filename)
			}
		}
		if item["id"] == "" {
			item["id"] = "image-" + stringValue(index)
		}
		data = append(data, item)
	}
	return data
}

// userImageURLSegment sanitizes userID into a single safe path element used both
// as the on-disk subdirectory and the URL segment. A blank userID yields "" so
// the flat legacy layout is preserved.
func userImageURLSegment(userID string) string {
	return sanitizeUserSegment(userID)
}
