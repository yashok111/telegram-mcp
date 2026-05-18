package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sessionInfo mirrors internal/shim.SessionInfo intentionally — duplicating the
// schema keeps cmd/server from importing internal/shim just to render a JSON
// file written by the shim package.
type sessionInfo struct {
	Alias        string    `json:"alias"`
	ShimID       string    `json:"shim_id"`
	ShimIDPrefix string    `json:"shim_id_prefix"`
	CCPID        int       `json:"cc_pid"`
	ShimPID      int       `json:"shim_pid"`
	CCSessionID  string    `json:"cc_session_id,omitempty"`
	Workdir      string    `json:"workdir"`
	Label        string    `json:"label,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	Mode         string    `json:"mode"`
}

// maxAncestorHops bounds the PPID walk so a misbehaving init tree can't trap us.
const maxAncestorHops = 8

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

	text := renderSelfText(stateDir, findCCPID)

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

// renderSelfText loads the session snapshot keyed by the CC process PID. The
// ccPIDFn parameter is injected so tests don't need to fork a real CC tree.
func renderSelfText(stateDir string, ccPIDFn func() int) string {
	ccPID := ccPIDFn()
	if ccPID <= 0 {
		return "Telegram bridge: embedded mode (no shim alias registered for this session)."
	}

	path := filepath.Join(stateDir, "sessions", strconv.Itoa(ccPID)+".json")

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
	fmt.Fprintf(&b, "(shim_id %s, cc_pid %d).\n", info.ShimIDPrefix, info.CCPID)
	fmt.Fprintf(&b, "Workdir: %s.\n", info.Workdir)

	if info.Label != "" {
		fmt.Fprintf(&b, "Label: %s.\n", info.Label)
	}

	if info.CCSessionID != "" {
		fmt.Fprintf(&b, "cc_session_id: %s.\n", info.CCSessionID)
	}

	fmt.Fprintf(&b, "When the user writes \"@%s ...\" in Telegram, that addresses YOU specifically.", info.Alias)

	return b.String()
}

// findCCPID walks the parent chain looking for the first ancestor whose
// /proc/<pid>/comm starts with "claude". Returns 0 if no such ancestor is found
// within maxAncestorHops. Honors CC_PID env override (set by the shipped hook
// wrapper) so callers that already know the PID don't pay the /proc walk cost.
func findCCPID() int {
	if v := os.Getenv("CC_PID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}

	pid := os.Getppid()
	for range maxAncestorHops {
		if pid <= 1 {
			return 0
		}

		comm, err := readProcComm(pid)
		if err == nil && strings.HasPrefix(comm, "claude") {
			return pid
		}

		ppid, err := readProcPPID(pid)
		if err != nil {
			return 0
		}

		pid = ppid
	}

	return 0
}

func readProcComm(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(raw)), nil
}

// readProcPPID parses the PPid line out of /proc/<pid>/status.
func readProcPPID(pid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}

	for line := range strings.Lines(string(raw)) {
		rest, ok := strings.CutPrefix(line, "PPid:")
		if !ok {
			continue
		}

		return strconv.Atoi(strings.TrimSpace(strings.TrimRight(rest, "\n")))
	}

	return 0, fmt.Errorf("ppid not found for pid %d", pid)
}
