package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// Snapshot mirrors daemon.AdminSnapshot's JSON wire shape. Defined here so the
// admin tool server decodes the admin.snapshot reply without importing daemon
// (which would create a daemon↔admin import edge).
type Snapshot struct {
	Shims  []ShimSnap  `json:"shims"`
	Spawns []SpawnSnap `json:"spawns"`
	Bg     []BgSnap    `json:"bg"`
}

type ShimSnap struct {
	ID           string    `json:"id"`
	Alias        string    `json:"alias"`
	Label        string    `json:"label"`
	Workdir      string    `json:"workdir"`
	CCSessionID  string    `json:"cc_session_id"`
	SpawnID      string    `json:"spawn_id"`
	TopicID      int       `json:"topic_id"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastOutbound time.Time `json:"last_outbound"`
	PinnedChats  []string  `json:"pinned_chats"`
	Role         string    `json:"role"`
}

type SpawnSnap struct {
	ID        string    `json:"id"`
	Pid       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Workdir   string    `json:"workdir"`
	UserID    string    `json:"user_id"`
	ChatID    string    `json:"chat_id"`
	Status    string    `json:"status"`
}

type BgSnap struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	Workdir    string    `json:"workdir"`
	PromptHead string    `json:"prompt_head"`
	UserID     string    `json:"user_id"`
	Status     string    `json:"status"`
}

// FetchSnapshot dials the daemon, calls the token-gated admin.snapshot method,
// and returns the decoded live state. The connection is one-shot (dial → call →
// close); it never sends hello, so the daemon does not register it as a shim.
func FetchSnapshot(ctx context.Context, socketPath, token string) (Snapshot, error) {
	client, err := ipc.Dial(socketPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("dial daemon: %w", err)
	}

	defer func() { _ = client.Close() }()

	var snap Snapshot
	if err := client.Call(ctx, ipc.MethodAdminSnapshot, map[string]any{"token": token}, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("admin snapshot: %w", err)
	}

	return snap, nil
}
