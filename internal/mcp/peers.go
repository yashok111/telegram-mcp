package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// Peer is the structured peer DTO returned by PeerProvider and rendered by the
// telegram_peers tool. IdleSeconds keeps the integer for stable testing; the
// tool layer humanizes it ("5m" / "30s") at the agent boundary.
type Peer struct {
	Alias        string `json:"alias"`
	ShimIDPrefix string `json:"shim_id_prefix"`
	Workdir      string `json:"workdir"`
	Label        string `json:"label"`
	IdleSeconds  int    `json:"idle_seconds"`
	Self         bool   `json:"self"`
}

// PeerProvider yields the live snapshot of connected shims. Each shim wires
// its BotAdapter (IPC client) as the provider during Wire(); tests that never
// attach a provider trip the no-op branch in handleTelegramPeers.
type PeerProvider interface {
	Peers(ctx context.Context) ([]Peer, error)
}

func (s *Server) AttachPeerProvider(p PeerProvider) {
	s.peerMu.Lock()
	s.peers = p
	s.peerMu.Unlock()
}

func (s *Server) peerProvider() PeerProvider {
	s.peerMu.RLock()
	defer s.peerMu.RUnlock()

	return s.peers
}

func (s *Server) handleTelegramPeers(ctx context.Context, _ mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	p := s.peerProvider()
	if p == nil {
		slog.Info("tool telegram_peers invoked before peer provider attached")
		return mcptypes.NewToolResultText("no peer registry attached for this MCP session"), nil
	}

	peers, err := p.Peers(ctx)
	if err != nil {
		slog.Error("tool telegram_peers fetch failed", "err", err)
		return mcptypes.NewToolResultError(fmt.Sprintf("peers fetch failed: %v", err)), nil
	}

	out := make([]peerWire, len(peers))
	for i, pr := range peers {
		out[i] = peerWire{
			Alias:        pr.Alias,
			ShimIDPrefix: pr.ShimIDPrefix,
			Workdir:      pr.Workdir,
			Label:        pr.Label,
			IdleFor:      humanizeDuration(pr.IdleSeconds),
			Self:         pr.Self,
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return mcptypes.NewToolResultError(fmt.Sprintf("marshal peers: %v", err)), nil
	}

	slog.Info("tool telegram_peers ok", "peer_count", len(peers))

	return mcptypes.NewToolResultText(string(body)), nil
}

// peerWire is the JSON shape returned by the telegram_peers tool. Defining it
// as a typed struct avoids the per-peer map[string]any allocation that an
// agent calling the tool in a loop would compound.
type peerWire struct {
	Alias        string `json:"alias"`
	ShimIDPrefix string `json:"shim_id_prefix"`
	Workdir      string `json:"workdir"`
	Label        string `json:"label"`
	IdleFor      string `json:"idle_for"`
	Self         bool   `json:"self"`
}

// humanizeDuration formats whole-second counts as "1h2m" / "5m" / "30s".
// Trailing zero units drop; negatives clamp to "0s" so the agent never sees a
// stale-clock artifact.
func humanizeDuration(secs int) string {
	if secs <= 0 {
		return "0s"
	}

	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60

	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
