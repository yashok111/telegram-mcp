package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionInfo mirrors internal/shim.SessionInfo intentionally — duplicating a
// 6-field struct keeps cmd/server from importing internal/shim just to render a
// JSON file written by the shim package.
type sessionInfo struct {
	Alias        string    `json:"alias"`
	ShimID       string    `json:"shim_id"`
	ShimIDPrefix string    `json:"shim_id_prefix"`
	CCSessionID  string    `json:"cc_session_id"`
	Workdir      string    `json:"workdir"`
	Label        string    `json:"label,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	Mode         string    `json:"mode"`
}

// runSelf renders the current Claude Code session's Telegram-shim identity.
// Always returns 0 — SessionStart hooks must never abort the CC session even
// when our state is missing.
func runSelf(stateDir string, argv []string, out io.Writer) int {
	wantHook := false

	for _, a := range argv {
		if a == "--hook" {
			wantHook = true
		}
	}

	text := renderSelfText(stateDir)

	if wantHook {
		payload := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": text,
			},
		}
		_ = json.NewEncoder(out).Encode(payload)

		return 0
	}

	_, _ = fmt.Fprintln(out, text)

	return 0
}

func renderSelfText(stateDir string) string {
	ccSID := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if ccSID == "" {
		return "Telegram bridge: embedded mode (no daemon, no shim alias)."
	}

	path := filepath.Join(stateDir, "sessions", ccSID+".json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return "Telegram bridge: embedded mode (no shim alias registered for this session)."
	}

	var info sessionInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "Telegram bridge: session file present but unreadable; treating as embedded."
	}

	var b strings.Builder

	fmt.Fprintf(&b, "You are connected to Telegram via shim alias @%s ", info.Alias)
	fmt.Fprintf(&b, "(shim_id %s, cc_session_id %s).\n", info.ShimIDPrefix, info.CCSessionID)
	fmt.Fprintf(&b, "Workdir: %s.\n", info.Workdir)

	if info.Label != "" {
		fmt.Fprintf(&b, "Label: %s.\n", info.Label)
	}

	fmt.Fprintf(&b, "When the user writes \"@%s ...\" in Telegram, that addresses YOU specifically.", info.Alias)

	return b.String()
}
