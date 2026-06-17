package api

import (
	"context"
	"encoding/base64"
	"testing"

	"chatgpt2api/internal/config"
	"chatgpt2api/internal/imagehistory"
)

func TestPruneResponsesContext(t *testing.T) {
	mk := func(n int) []responsesContextTurn {
		out := make([]responsesContextTurn, n)
		for i := range out {
			out[i] = responsesContextTurn{Prompt: string(rune('a' + i))}
		}
		return out
	}

	t.Run("disabled when maxTurns<=0", func(t *testing.T) {
		if got := pruneResponsesContext(mk(3), 0, 1<<20); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})

	t.Run("keeps most recent maxTurns in order", func(t *testing.T) {
		got := pruneResponsesContext(mk(7), 3, 1<<20)
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		// mk(7) = a..g; most-recent 3 = e,f,g (chronological)
		if got[0].Prompt != "e" || got[2].Prompt != "g" {
			t.Fatalf("order wrong: %q..%q, want e..g", got[0].Prompt, got[2].Prompt)
		}
	})

	t.Run("drops oldest first to fit byte budget", func(t *testing.T) {
		// Three turns, each ~one 300-byte image. Budget fits ~2.
		turns := []responsesContextTurn{
			{Prompt: "old", Images: [][]byte{make([]byte, 300)}},
			{Prompt: "mid", Images: [][]byte{make([]byte, 300)}},
			{Prompt: "new", Images: [][]byte{make([]byte, 300)}},
		}
		perTurn := responsesContextBytes(turns[:1])
		got := pruneResponsesContext(turns, 5, perTurn*2+10)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2 (oldest dropped)", len(got))
		}
		if got[0].Prompt != "mid" || got[1].Prompt != "new" {
			t.Fatalf("kept wrong turns: %q,%q, want mid,new", got[0].Prompt, got[1].Prompt)
		}
	})

	t.Run("single oversized turn dropped entirely", func(t *testing.T) {
		turns := []responsesContextTurn{{Prompt: "huge", Images: [][]byte{make([]byte, 1000)}}}
		if got := pruneResponsesContext(turns, 5, 10); got != nil {
			t.Fatalf("want nil for oversized turn, got %v", got)
		}
	})
}

// TestBuildResponsesRequestPrependsContext verifies prior-turn context lands
// first in the input array (oldest first) with the current turn last, so the
// upstream sees chronological history.
func TestBuildResponsesRequestPrependsContext(t *testing.T) {
	c := newCPAImageClient("http://x", "k", 0, "codex_responses")

	ctx := withResponsesContext(context.Background(), []responsesContextTurn{
		{Prompt: "first turn", Images: [][]byte{[]byte("imgbytes")}},
		{Prompt: "second turn"},
	})

	payload := c.buildResponsesRequest(ctx, "current prompt", nil, nil, "1024x1024", "high", "")

	input, ok := payload["input"].([]map[string]any)
	if !ok {
		t.Fatalf("input type = %T, want []map[string]any", payload["input"])
	}
	// 2 prior turns + 1 current = 3 messages.
	if len(input) != 3 {
		t.Fatalf("len(input) = %d, want 3", len(input))
	}

	firstText := input[0]["content"].([]map[string]any)[0]["text"]
	if firstText != "first turn" {
		t.Fatalf("input[0] text = %v, want first turn (oldest first)", firstText)
	}
	// First turn also carries its result image as input_image.
	firstContent := input[0]["content"].([]map[string]any)
	if len(firstContent) != 2 || firstContent[1]["type"] != "input_image" {
		t.Fatalf("input[0] should carry text + input_image, got %#v", firstContent)
	}
	lastText := input[2]["content"].([]map[string]any)[0]["text"]
	if lastText != "current prompt" {
		t.Fatalf("input[2] text = %v, want current prompt (last)", lastText)
	}
}

// TestBuildResponsesRequestNoContext: without context, the input is just the
// current turn (single message), unchanged from prior behavior.
func TestBuildResponsesRequestNoContext(t *testing.T) {
	c := newCPAImageClient("http://x", "k", 0, "codex_responses")
	payload := c.buildResponsesRequest(context.Background(), "solo", nil, nil, "", "", "")
	input, ok := payload["input"].([]map[string]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want single current-turn message", payload["input"])
	}
	if txt := input[0]["content"].([]map[string]any)[0]["text"]; txt != "solo" {
		t.Fatalf("input[0] text = %v, want solo", txt)
	}
}

// TestBuildResponsesContextOnlyLatestImageInlined proves the image policy: with
// four prior success turns each producing an image, only the most-recent prior
// image turn carries inlined bytes; earlier image turns are placeholders. The
// current (in-flight) turn is excluded.
func TestBuildResponsesContextOnlyLatestImageInlined(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Storage.Backend = "sqlite"
	cfg.Storage.SQLitePath = "data/history.sqlite"
	cfg.Storage.ImageConversationStorage = "server"
	cfg.Storage.ImageDataStorage = "server"
	cfg.CPA.RouteStrategy = "codex_responses"
	cfg.CPA.ResponsesContextMaxTurns = 5
	cfg.CPA.ResponsesContextMaxBytes = 64 << 20
	server := NewServer(cfg)

	mkTurn := func(id string, b64 string) imagehistory.Turn {
		return imagehistory.Turn{
			ID:        id,
			CreatedAt: "2026-06-15T00:00:00Z",
			Status:    "success",
			Prompt:    "prompt-" + id,
			Images:    []imagehistory.Image{{ID: id + "-img", Status: "success", B64JSON: b64}},
		}
	}
	// Four prior success turns each with a (distinct) inline image, plus the
	// in-flight current turn t5.
	img := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	conv := imagehistory.Conversation{
		ID: "conv-1", CreatedAt: "2026-06-15T00:00:00Z", Status: "success",
		Turns: []imagehistory.Turn{
			mkTurn("t1", img("image-one")),
			mkTurn("t2", img("image-two")),
			mkTurn("t3", img("image-three")),
			mkTurn("t4", img("image-four")),
			{ID: "t5", CreatedAt: "2026-06-15T00:00:00Z", Status: "running", Prompt: "current"},
		},
	}
	store, err := imagehistory.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	if _, err := store.Save(context.Background(), "userX", conv); err != nil {
		t.Fatalf("Save: %v", err)
	}

	task := &imageTask{UserID: "userX", ConversationID: "conv-1", TurnID: "t5"}
	turns := server.buildResponsesContextTurns(context.Background(), task)

	// 4 prior turns kept (current t5 excluded).
	if len(turns) != 4 {
		t.Fatalf("len(turns) = %d, want 4", len(turns))
	}
	// Only the last (t4) has inlined image bytes.
	for i, tn := range turns {
		if i == len(turns)-1 {
			if len(tn.Images) != 1 {
				t.Fatalf("latest turn images = %d, want 1 inlined", len(tn.Images))
			}
			if tn.OmittedImageCount != 0 {
				t.Fatalf("latest turn OmittedImageCount = %d, want 0", tn.OmittedImageCount)
			}
		} else {
			if len(tn.Images) != 0 {
				t.Fatalf("turn %d images = %d, want 0 (placeholder)", i, len(tn.Images))
			}
			if tn.OmittedImageCount != 1 {
				t.Fatalf("turn %d OmittedImageCount = %d, want 1", i, tn.OmittedImageCount)
			}
		}
	}

	// The rebuilt input renders placeholders for older turns and a real
	// input_image only for the latest.
	c := newCPAImageClient("http://x", "k", 0, "codex_responses")
	ctx := withResponsesContext(context.Background(), turns)
	payload := c.buildResponsesRequest(ctx, "current prompt", nil, nil, "", "", "")
	input := payload["input"].([]map[string]any)
	// 4 prior + 1 current.
	if len(input) != 5 {
		t.Fatalf("len(input) = %d, want 5", len(input))
	}
	inputImageCount := 0
	for _, msg := range input {
		for _, part := range msg["content"].([]map[string]any) {
			if part["type"] == "input_image" {
				inputImageCount++
			}
		}
	}
	if inputImageCount != 1 {
		t.Fatalf("input_image count = %d, want exactly 1 (latest only)", inputImageCount)
	}
}
