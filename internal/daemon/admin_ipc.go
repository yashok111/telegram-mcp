package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// spawnLister and bgLister are the minimal read slices of SpawnRunner / BgRunner
// the admin snapshot needs. Interfaces keep admin_ipc testable with fakes and
// avoid coupling Handlers to the concrete runner types.
type (
	spawnLister interface{ List() []bot.SpawnTaskInfo }
	bgLister    interface{ List() []bot.BgTaskInfo }
)

// SetRunners wires the spawn/bg managers so HandleAdminSnapshot can report them.
// Pass nil for either to omit it. Must be called before server.Listen — same
// unsynchronized-write rule as SetShimLogs/SetAdminToken.
func (h *Handlers) SetRunners(spawns spawnLister, bgs bgLister) {
	h.spawns = spawns
	h.bgs = bgs
}

// AdminSnapshot is the JSON returned to the admin-tools MCP server. Fields are
// tagged so the admin package decodes them without importing daemon or bot.
type AdminSnapshot struct {
	Shims  []AdminShim  `json:"shims"`
	Spawns []AdminSpawn `json:"spawns"`
	Bg     []AdminBg    `json:"bg"`
}

type AdminShim struct {
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

type AdminSpawn struct {
	ID        string    `json:"id"`
	Pid       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Workdir   string    `json:"workdir"`
	UserID    string    `json:"user_id"`
	ChatID    string    `json:"chat_id"`
	Status    string    `json:"status"`
}

type AdminBg struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	Workdir    string    `json:"workdir"`
	PromptHead string    `json:"prompt_head"`
	UserID     string    `json:"user_id"`
	Status     string    `json:"status"`
}

// HandleAdminSnapshot returns live in-memory daemon state to the admin-tools
// MCP server. Token-gated per-call: the caller presents the per-daemon-boot
// admin token in params rather than doing a hello, so it never registers as a
// routable shim. Unknown/empty token → CodeUnauthorized (and never reveals
// whether admin is even enabled beyond the daemon having a token set).
func (h *Handlers) HandleAdminSnapshot(_ context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: "bad admin snapshot params"}
	}

	if !h.adminTokenValid(p.Token) {
		return nil, &ipc.Error{Code: ipc.CodeUnauthorized, Message: "admin token required"}
	}

	return AdminSnapshot{
		Shims:  toAdminShims(h.router.Snapshot()),
		Spawns: toAdminSpawns(listSpawns(h.spawns)),
		Bg:     toAdminBg(listBg(h.bgs)),
	}, nil
}

// adminTokenValid reports whether presented matches the per-boot admin token in
// constant time. A daemon with no admin token configured rejects everything.
func (h *Handlers) adminTokenValid(presented string) bool {
	if h.adminToken == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.adminToken)) == 1
}

func listSpawns(l spawnLister) []bot.SpawnTaskInfo {
	if l == nil {
		return nil
	}

	return l.List()
}

func listBg(l bgLister) []bot.BgTaskInfo {
	if l == nil {
		return nil
	}

	return l.List()
}

func toAdminShims(in []ShimInfo) []AdminShim {
	out := make([]AdminShim, len(in))
	// AdminShim mirrors ShimInfo's layout exactly; the conversion is a
	// compile-time guard — reordering/adding a ShimInfo field breaks the build
	// here until AdminShim is updated to match.
	for i, s := range in {
		out[i] = AdminShim(s)
	}

	return out
}

func toAdminSpawns(in []bot.SpawnTaskInfo) []AdminSpawn {
	out := make([]AdminSpawn, len(in))
	for i, s := range in {
		out[i] = AdminSpawn{
			ID:        s.ID,
			Pid:       s.Pid,
			StartedAt: s.StartedAt,
			Workdir:   s.Workdir,
			UserID:    s.UserID,
			ChatID:    s.ChatID,
			Status:    s.Status,
		}
	}

	return out
}

func toAdminBg(in []bot.BgTaskInfo) []AdminBg {
	out := make([]AdminBg, len(in))
	for i, b := range in {
		out[i] = AdminBg{
			ID:         b.ID,
			StartedAt:  b.StartedAt,
			Workdir:    b.Workdir,
			PromptHead: b.PromptHead,
			UserID:     b.UserID,
			Status:     b.Status,
		}
	}

	return out
}
