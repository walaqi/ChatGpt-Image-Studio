// Package credential resolves a per-user channel api-key by calling the mother
// system's internal endpoints. It implements the two-stage model from
// docs/multi-tenant-redesign.md §4.2:
//
//	stage 1 (ListKeys): list a user's image-capable keys (no plaintext) so the
//	  UI can render a key picker;
//	stage 2 (Resolve):  exchange a chosen key_id for the plaintext credential.
//
// The mother system only reads (never mints) keys; image-studio remembers the
// user's chosen key_id via a SelectionStore and caches resolved plaintext
// credentials for a short TTL. Plaintext keys live only in memory.
package credential

import (
	"errors"
	"strings"
)

// KeyCandidate describes one image-capable key the user owns. It deliberately
// carries no plaintext key — only metadata for the picker UI.
type KeyCandidate struct {
	KeyID     int64   `json:"key_id"`
	Name      string  `json:"name"`
	Quota     float64 `json:"quota"`
	QuotaUsed float64 `json:"quota_used"`
	ExpiresAt string  `json:"expires_at"`
	GroupName string  `json:"group_name"`
}

// KeyListResult is the stage-1 response: the candidate keys plus whether the
// user can create a new image key and which group it should bind to.
type KeyListResult struct {
	Keys         []KeyCandidate `json:"keys"`
	CanCreate    bool           `json:"can_create"`
	ImageGroupID *int64         `json:"image_group_id"`
}

// Credential is the stage-2 plaintext result used to call the image gateway.
// APIKey must never be logged or persisted.
type Credential struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

// Errors surfaced to callers. Handlers map these to user-facing guidance vs.
// transient failures.
var (
	// ErrNoCredential means the user has no usable image key (empty candidate
	// list). The UI should guide the user to create one.
	ErrNoCredential = errors.New("credential: user has no usable image key")
	// ErrKeyUnusable means a specific key_id is no longer valid (expired, out of
	// quota, or no longer image-capable). The caller should clear any remembered
	// selection and re-prompt.
	ErrKeyUnusable = errors.New("credential: selected key is no longer usable")
	// ErrUpstreamUnavailable means the mother system could not be reached or
	// returned a server error. This is transient; the caller should retry.
	ErrUpstreamUnavailable = errors.New("credential: mother system unavailable")
	// ErrNoSelection means the user has not yet chosen a key (or their previous
	// choice became unusable and was cleared). The caller should prompt the key
	// picker.
	ErrNoSelection = errors.New("credential: no key selected for user")
)

func (c Credential) configured() bool {
	return strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.BaseURL) != ""
}
