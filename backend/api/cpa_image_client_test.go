package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeCPAImageBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// Mother system returns base_url WITH /v1 (docs §C.3): strip it so the
		// per-request /v1 prefix doesn't double.
		{"http://localhost:8080/v1", "http://localhost:8080"},
		{"http://localhost:8080/v1/", "http://localhost:8080"},
		{"  http://host:8080/v1  ", "http://host:8080"},
		// Bare host (legacy/standalone convention): unchanged.
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://localhost:8080/", "http://localhost:8080"},
		// Only a trailing /v1 is stripped, not a path that merely contains v1.
		{"http://host/api/v1", "http://host/api"},
		{"http://host/v1beta", "http://host/v1beta"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeCPAImageBaseURL(c.in); got != c.want {
			t.Errorf("normalizeCPAImageBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCPAImageClientStripsV1FromMotherBaseURL is the regression guard for the
// "cpa returned 404: 404 page not found" bug: the mother system's base_url
// carries /v1, and without normalization the request hit /v1/v1/images/...
func TestCPAImageClientStripsV1FromMotherBaseURL(t *testing.T) {
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("img")) + `"}]}`))
	}))
	defer server.Close()

	// Build the client with a /v1-suffixed base, exactly as the resolver hands it
	// over from the mother system.
	client := newCPAImageClient(server.URL+"/v1", "test-key", 30*time.Second, "images_api")
	if _, err := client.GenerateImage(context.Background(), "a cat", "gpt-image-2", 1, "1024x1024", "", ""); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if seenPath != "/v1/images/generations" {
		t.Fatalf("request path = %q, want /v1/images/generations (no double /v1)", seenPath)
	}
}

func TestCPAImageClientGenerateImageUsesCodexResponsesStrategy(t *testing.T) {
	var seenAuth string
	var seenPayload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/v1/responses")
		}
		seenAuth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &seenPayload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		encoded := base64.StdEncoding.EncodeToString([]byte("image"))
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"created_at":1,"output":[{"type":"image_generation_call","result":"`+encoded+`","revised_prompt":"revised","output_format":"png"}]}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	results, err := client.GenerateImage(context.Background(), "draw a cat", cpaFixedImageModel, 1, "1536x1024", "high", "transparent")
	if err != nil {
		t.Fatalf("GenerateImage() returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if !strings.HasPrefix(results[0].URL, "data:image/png;base64,") {
		t.Fatalf("result URL = %q, want data URL", results[0].URL)
	}
	if got := client.LastRoute(); got != "codex_responses" {
		t.Fatalf("LastRoute() = %q, want %q", got, "codex_responses")
	}
	if got := client.LastModelLabel(); got != "gpt-5.4-mini (tool: gpt-image-2)" {
		t.Fatalf("LastModelLabel() = %q, want %q", got, "gpt-5.4-mini (tool: gpt-image-2)")
	}
	if got := client.ImageToolModel(); got != cpaFixedImageModel {
		t.Fatalf("ImageToolModel() = %q, want %q", got, cpaFixedImageModel)
	}
	if seenAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", seenAuth)
	}
	if got := seenPayload["model"]; got != "gpt-5.4-mini" {
		t.Fatalf("payload model = %v, want %q", got, "gpt-5.4-mini")
	}
	tools, ok := seenPayload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("payload tools = %#v, want one tool", seenPayload["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", tools[0])
	}
	if got := tool["model"]; got != cpaFixedImageModel {
		t.Fatalf("tool.model = %v, want %q", got, cpaFixedImageModel)
	}
	if got := tool["action"]; got != "generate" {
		t.Fatalf("tool.action = %v, want %q", got, "generate")
	}
	if got := tool["size"]; got != "1536x1024" {
		t.Fatalf("tool.size = %v, want %q", got, "1536x1024")
	}
	if got := tool["quality"]; got != "high" {
		t.Fatalf("tool.quality = %v, want %q", got, "high")
	}
	if got := tool["background"]; got != "transparent" {
		t.Fatalf("tool.background = %v, want %q", got, "transparent")
	}
}

func TestCPAImageClientEditUsesCodexResponsesMaskField(t *testing.T) {
	var seenTool map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/v1/responses")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		tools := payload["tools"].([]any)
		seenTool = tools[0].(map[string]any)

		w.Header().Set("Content-Type", "text/event-stream")
		encoded := base64.StdEncoding.EncodeToString([]byte("image"))
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"created_at":1,"output":[{"type":"image_generation_call","result":"`+encoded+`","output_format":"png"}]}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	_, err := client.EditImageByUpload(context.Background(), "edit cat", cpaFixedImageModel, [][]byte{[]byte("source-image")}, []byte("mask-image"), "1536x1024", "high")
	if err != nil {
		t.Fatalf("EditImageByUpload() returned error: %v", err)
	}
	if got := seenTool["action"]; got != "edit" {
		t.Fatalf("tool.action = %v, want %q", got, "edit")
	}
	if got := seenTool["size"]; got != "1536x1024" {
		t.Fatalf("tool.size = %v, want %q", got, "1536x1024")
	}
	if got := seenTool["quality"]; got != "high" {
		t.Fatalf("tool.quality = %v, want %q", got, "high")
	}
	maskField, ok := seenTool["input_image_mask"].(map[string]any)
	if !ok {
		t.Fatalf("tool.input_image_mask = %#v, want object", seenTool["input_image_mask"])
	}
	if _, ok := maskField["image_url"].(string); !ok {
		t.Fatalf("tool.input_image_mask.image_url missing: %#v", seenTool)
	}
}

func TestCPAImageClientAutoFallsBackToCodexResponses(t *testing.T) {
	var imagesAPICalls int
	var responsesCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			imagesAPICalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"stream disconnected before completion"}}`)
		case "/v1/responses":
			responsesCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			encoded := base64.StdEncoding.EncodeToString([]byte("image"))
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"created_at":1,"output":[{"type":"image_generation_call","result":"`+encoded+`","output_format":"png"}]}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "auto")
	results, err := client.GenerateImage(context.Background(), "draw a cat", cpaFixedImageModel, 1, "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage() returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if imagesAPICalls != 1 || responsesCalls != 1 {
		t.Fatalf("images_api calls = %d, responses calls = %d, want 1/1", imagesAPICalls, responsesCalls)
	}
	if got := client.LastRoute(); got != "codex_responses" {
		t.Fatalf("LastRoute() = %q, want %q", got, "codex_responses")
	}
}

// TestCPAImageClientResponsesTextOnlyRefusal mirrors the real upstream stream
// captured when the model refuses to generate an image and replies with text
// only: the assistant text arrives via output_text deltas + a terminal message
// item in response.output_item.done, the image_generation_call never produces a
// result, and response.completed.output is empty. The parser must surface the
// text (LastAssistantText) and return zero images WITHOUT an error.
func TestCPAImageClientResponsesTextOnlyRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\n")
		io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_1"}}`+"\n\n")
		// image_generation_call lifecycle that never yields a result
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed"}}`+"\n\n")
		// streamed text deltas
		io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"抱歉，"}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"这个请求不能直接生成。"}`+"\n\n")
		// terminal message item with the full text
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"抱歉，这个请求不能直接生成。"}]}}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	results, err := client.GenerateImage(context.Background(), "draw something", cpaFixedImageModel, 1, "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage() returned error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0 (text-only refusal)", len(results))
	}
	if got := client.LastAssistantText(); got != "抱歉，这个请求不能直接生成。" {
		t.Fatalf("LastAssistantText() = %q, want the refusal text", got)
	}
}

// TestCPAImageClientResponsesImageViaOutputItemDone proves the image is read from
// response.output_item.done (where the real stream puts it), not from the empty
// response.completed.output array.
func TestCPAImageClientResponsesImageViaOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"`+encoded+`","output_format":"png","revised_prompt":"a cat"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	results, err := client.GenerateImage(context.Background(), "draw a cat", cpaFixedImageModel, 1, "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage() returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if !strings.HasPrefix(results[0].URL, "data:image/png;base64,") {
		t.Fatalf("result URL = %q, want data URL", results[0].URL)
	}
	if results[0].RevisedPrompt != "a cat" {
		t.Fatalf("RevisedPrompt = %q, want %q", results[0].RevisedPrompt, "a cat")
	}
	if got := client.LastAssistantText(); got != "" {
		t.Fatalf("LastAssistantText() = %q, want empty", got)
	}
}

// TestCPAImageClientResponsesImageAndText proves both an image and accompanying
// text are captured in the same turn.
func TestCPAImageClientResponsesImageAndText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"`+encoded+`","output_format":"png"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"已生成，按你的要求调整了构图。"}]}}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	results, err := client.GenerateImage(context.Background(), "draw a cat", cpaFixedImageModel, 1, "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage() returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if got := client.LastAssistantText(); got != "已生成，按你的要求调整了构图。" {
		t.Fatalf("LastAssistantText() = %q, want the accompanying text", got)
	}
}

// TestCPAImageClientResponsesEmptyStreamErrors confirms a truly empty stream
// (no image AND no text) is still an error.
func TestCPAImageClientResponsesEmptyStreamErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newCPAImageClient(server.URL, "test-key", 30*time.Second, "codex_responses")
	_, err := client.GenerateImage(context.Background(), "draw a cat", cpaFixedImageModel, 1, "1024x1024", "", "")
	if err == nil {
		t.Fatalf("GenerateImage() error = nil, want error for empty stream")
	}
}
