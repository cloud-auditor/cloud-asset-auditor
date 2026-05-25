package core

import (
	"encoding/json"
	"time"
)

// Asset is the canonical, provider-agnostic representation of a cloud
// resource. Keep this struct minimal — provider-specific richness lives in
// Raw, not as new top-level fields.
type Asset struct {
	Provider  string            `json:"provider"`
	AccountID string            `json:"account_id"`
	Region    string            `json:"region,omitempty"`
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status,omitempty"`
	CreatedAt *time.Time        `json:"created_at,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Raw       json.RawMessage   `json:"raw,omitempty"`
}
