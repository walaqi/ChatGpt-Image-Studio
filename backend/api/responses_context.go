package api

import (
	"context"
	"encoding/base64"
	"os"
	"strings"

	"chatgpt2api/internal/imagehistory"
)

// responsesContextTurn is one prior conversation turn replayed as multi-turn
// context into a /v1/responses request: the user's prompt for that turn plus,
// for the single most-recent image-bearing turn, the images it produced (inlined
// as input_image). Earlier image-bearing turns carry only OmittedImageCount so
// they are replayed as a lightweight text placeholder instead of re-sending the
// bytes — a conversation iterates on the latest image, so only that one is the
// working image; older intermediate images are referenced by placeholder and the
// user can re-upload one explicitly if they want to revisit it.
type responsesContextTurn struct {
	Prompt string
	Images [][]byte
	// OmittedImageCount is the number of images this turn produced that were
	// intentionally NOT inlined (replaced by a text placeholder). It is zero for
	// the latest image turn (whose bytes are in Images) and for text-only turns.
	OmittedImageCount int
}

type responsesContextKey struct{}

// withResponsesContext attaches prior-turn context for the cpa client to replay.
// A nil/empty slice leaves ctx untouched (single-turn behavior).
func withResponsesContext(ctx context.Context, turns []responsesContextTurn) context.Context {
	if len(turns) == 0 {
		return ctx
	}
	return context.WithValue(ctx, responsesContextKey{}, turns)
}

// responsesContextFromContext returns the prior-turn context, or nil.
func responsesContextFromContext(ctx context.Context) []responsesContextTurn {
	if ctx == nil {
		return nil
	}
	turns, _ := ctx.Value(responsesContextKey{}).([]responsesContextTurn)
	return turns
}

// pruneResponsesContext enforces the two budgets: first keep only the most
// recent maxTurns, then drop the OLDEST remaining turns until the approximate
// JSON byte cost fits maxBytes. Input is chronological (oldest first); the
// returned slice preserves that order. Returns nil when context is disabled
// (maxTurns <= 0) or nothing fits.
func pruneResponsesContext(turns []responsesContextTurn, maxTurns, maxBytes int) []responsesContextTurn {
	if maxTurns <= 0 || len(turns) == 0 {
		return nil
	}
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	// Drop oldest turns first until under the byte budget (or empty). A single
	// turn larger than the budget is dropped entirely — better no context than a
	// request that blows the limit.
	for len(turns) > 0 && responsesContextBytes(turns) > maxBytes {
		turns = turns[1:]
	}
	if len(turns) == 0 {
		return nil
	}
	return turns
}

// responsesContextBytes approximates the JSON byte cost of replaying these
// turns. Images dominate; base64 expands raw bytes by ~4/3.
func responsesContextBytes(turns []responsesContextTurn) int {
	total := 0
	for _, t := range turns {
		total += len(t.Prompt)
		for _, img := range t.Images {
			total += (len(img)+2)/3*4 + 64 // base64 size + small per-item JSON overhead
		}
	}
	return total
}

// responsesContextEnabled reports whether prior-turn context should be built for
// the current route strategy. Context only feeds the /v1/responses route, so it
// is skipped for the plain images_api route to avoid needless history reads.
func (s *Server) responsesContextEnabled() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	if s.cfg.CPAResponsesContextMaxTurns() <= 0 {
		return false
	}
	switch s.cfg.CPAImageRouteStrategy() {
	case "codex_responses", "auto":
		return true
	default:
		return false
	}
}

// buildResponsesContextTurns reads the task's conversation from the per-user
// history store and rebuilds the prior-turn context (excluding the current
// turn), pruned to the configured turn/byte budgets. Any read failure degrades
// gracefully to no context rather than failing the generation.
//
// Image policy: a conversation iterates on a single working image, so only the
// SINGLE most-recent image-bearing prior turn has its image bytes inlined.
// Earlier image-bearing turns are replayed as a lightweight text placeholder
// (their files are not even read). The textual prompt history of every kept turn
// is still replayed so the model follows the conversation; if the user wants to
// revisit an older intermediate image they can re-upload it explicitly.
func (s *Server) buildResponsesContextTurns(ctx context.Context, task *imageTask) []responsesContextTurn {
	if task == nil || strings.TrimSpace(task.ConversationID) == "" {
		return nil
	}
	maxTurns := s.cfg.CPAResponsesContextMaxTurns()
	if maxTurns <= 0 {
		return nil
	}
	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		return nil
	}
	defer store.Close()

	conv, err := store.Get(ctx, task.UserID, task.ConversationID)
	if err != nil || conv == nil {
		return nil
	}

	// Collect prior turns (chronological, oldest first): prompt + a structural
	// count of result images. No file reads yet — bytes are loaded only for the
	// single turn we ultimately inline.
	type priorTurn struct {
		turn       imagehistory.Turn
		prompt     string
		imageCount int
	}
	priors := make([]priorTurn, 0, len(conv.Turns))
	for _, t := range conv.Turns {
		// Stop at the current turn: anything from here on is the in-flight turn
		// (or a retry of it), never prior context.
		if t.ID == task.TurnID {
			break
		}
		// Only replay turns that actually completed with output.
		if t.Status != "success" {
			continue
		}
		prompt := strings.TrimSpace(t.Prompt)
		imageCount := countTurnResultImages(t)
		if prompt == "" && imageCount == 0 {
			continue
		}
		priors = append(priors, priorTurn{turn: t, prompt: prompt, imageCount: imageCount})
	}

	// Keep only the most-recent maxTurns of textual history.
	if len(priors) > maxTurns {
		priors = priors[len(priors)-maxTurns:]
	}
	if len(priors) == 0 {
		return nil
	}

	// Locate the latest image-bearing turn — the only one whose bytes are inlined.
	latestImageIdx := -1
	for i := len(priors) - 1; i >= 0; i-- {
		if priors[i].imageCount > 0 {
			latestImageIdx = i
			break
		}
	}

	turns := make([]responsesContextTurn, 0, len(priors))
	for i, p := range priors {
		ct := responsesContextTurn{Prompt: p.prompt}
		if p.imageCount > 0 {
			if i == latestImageIdx {
				ct.Images = s.collectTurnResultBytes(task.UserID, p.turn)
				// Files unreadable (deleted/cross-tenant): fall back to placeholder.
				if len(ct.Images) == 0 {
					ct.OmittedImageCount = p.imageCount
				}
			} else {
				ct.OmittedImageCount = p.imageCount
			}
		}
		turns = append(turns, ct)
	}

	return pruneResponsesContext(turns, maxTurns, s.cfg.CPAResponsesContextMaxBytes())
}

// countTurnResultImages counts a prior turn's usable result images structurally
// (no file reads): images with no error that carry inline base64 or a stored
// file URL. Mirrors the acceptance filter in collectTurnResultBytes.
func countTurnResultImages(turn imagehistory.Turn) int {
	n := 0
	for _, img := range turn.Images {
		if strings.TrimSpace(img.Error) != "" {
			continue
		}
		if strings.TrimSpace(img.B64JSON) != "" || strings.Contains(img.URL, "/v1/files/image/") {
			n++
		}
	}
	return n
}

// collectTurnResultBytes loads the decoded bytes of a prior turn's result
// images. It mirrors resolveTaskSourceImageBytes's URL handling and cross-tenant
// guard: a turn may only reuse images from its own user's namespace.
func (s *Server) collectTurnResultBytes(userID string, turn imagehistory.Turn) [][]byte {
	out := make([][]byte, 0, len(turn.Images))
	for _, img := range turn.Images {
		if strings.TrimSpace(img.Error) != "" {
			continue
		}
		if encoded := strings.TrimSpace(img.B64JSON); encoded != "" {
			if data, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(data) > 0 {
				out = append(out, data)
			}
			continue
		}
		rawURL := strings.TrimSpace(img.URL)
		if rawURL == "" {
			continue
		}
		index := strings.Index(rawURL, "/v1/files/image/")
		if index < 0 {
			continue
		}
		name := rawURL[index+len("/v1/files/image/"):]
		owner, filename := splitImageOwnerPath(name)
		if owner != "" && userID != "" && owner != userID {
			continue
		}
		path := s.resolveImageFilePath(owner, filename)
		if path == "" {
			continue
		}
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			out = append(out, data)
		}
	}
	return out
}
