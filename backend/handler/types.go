package handler

import "context"

// ImageResult represents a single generated image returned by an image
// workflow client. It is a pure data struct shared by the CPA image link.
type ImageResult struct {
	URL            string `json:"url"`
	FileID         string `json:"file_id"`
	GenID          string `json:"gen_id"`
	ConversationID string `json:"conversation_id"`
	ParentMsgID    string `json:"parent_message_id"`
	RevisedPrompt  string `json:"revised_prompt"`
}

// ImageWorkflowClient is the image-generation contract the API layer depends
// on. The CPA client (api/cpa_image_client.go) implements it. The legacy
// chatgpt.com direct clients were removed in phase 7 (cpa-only).
type ImageWorkflowClient interface {
	DownloadBytes(url string) ([]byte, error)
	DownloadAsBase64(ctx context.Context, url string) (string, error)
	GenerateImage(ctx context.Context, prompt, model string, n int, size, quality, background string) ([]ImageResult, error)
	EditImageByUpload(ctx context.Context, prompt, model string, images [][]byte, mask []byte, size, quality string) ([]ImageResult, error)
	InpaintImageByMask(
		ctx context.Context,
		prompt string,
		model string,
		originalFileID string,
		originalGenID string,
		conversationID string,
		parentMessageID string,
		mask []byte,
		size string,
		quality string,
	) ([]ImageResult, error)
}
