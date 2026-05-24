package daemon

import "time"

// ShimInfo is a by-value snapshot of a connected shim for status rendering.
// Field set is fixed at Snapshot() time — mutating Router after won't change it.
type ShimInfo struct {
	ID           string
	Alias        string
	Label        string
	Workdir      string
	CCSessionID  string
	SpawnID      string
	TopicID      int
	ConnectedAt  time.Time
	LastOutbound time.Time
	PinnedChats  []string
	Role         string
}

// IDPrefix returns the first 8 hex chars (or the full ID if shorter). Used as
// /use argument so users don't have to paste the whole 12-char ID.
func (s ShimInfo) IDPrefix() string {
	const n = 8
	if len(s.ID) <= n {
		return s.ID
	}

	return s.ID[:n]
}

// IdleFor returns time since LastOutbound; falls back to ConnectedAt if the shim
// never sent anything. Caller passes now so tests can pin the clock.
func (s ShimInfo) IdleFor(now time.Time) time.Duration {
	t := s.LastOutbound
	if t.IsZero() {
		t = s.ConnectedAt
	}

	return now.Sub(t)
}

// PeerInfo is the wire DTO for daemon.peers responses. Stable JSON schema —
// the shim/MCP-tool layer parses it. IdleSeconds is rounded to whole seconds
// for stable test assertions; the tool layer humanizes it for display.
type PeerInfo struct {
	Alias        string `json:"alias"`
	ShimIDPrefix string `json:"shim_id_prefix"`
	Workdir      string `json:"workdir"`
	Label        string `json:"label"`
	IdleSeconds  int    `json:"idle_seconds"`
	Self         bool   `json:"self"`
}
