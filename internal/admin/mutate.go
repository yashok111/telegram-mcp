package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// MutateResult mirrors daemon.AdminMutateResult's JSON wire shape. Defined here
// so the admin tool server decodes the admin.mutate reply without importing
// daemon. cmd/server.TestAdminMutateWireCompat guards tag drift.
type MutateResult struct {
	Tool      string `json:"tool"`
	Tier      int    `json:"tier"`
	Applied   bool   `json:"applied"`
	Pending   bool   `json:"pending"`
	PendingID string `json:"pending_id,omitempty"`
	Result    string `json:"result"`
}

// RequestMutation dials the daemon and calls the token-gated admin.mutate
// method with the tool name and its arguments. One-shot connection (dial →
// call → close); like FetchSnapshot it never sends hello, so the daemon does
// not register it as a shim. The daemon classifies the tool's tier
// authoritatively — toolArgs carries no tier.
func RequestMutation(ctx context.Context, socketPath, token, tool string, toolArgs any) (MutateResult, error) {
	rawArgs, err := json.Marshal(toolArgs)
	if err != nil {
		return MutateResult{}, fmt.Errorf("marshal args: %w", err)
	}

	client, err := ipc.Dial(socketPath)
	if err != nil {
		return MutateResult{}, fmt.Errorf("dial daemon: %w", err)
	}

	defer func() { _ = client.Close() }()

	var res MutateResult
	if err := client.Call(ctx, ipc.MethodAdminMutate, map[string]any{
		"token": token,
		"tool":  tool,
		"args":  json.RawMessage(rawArgs),
	}, &res); err != nil {
		return MutateResult{}, fmt.Errorf("admin mutate: %w", err)
	}

	return res, nil
}
