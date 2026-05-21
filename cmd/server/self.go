package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	wantStatusline := false

	for _, a := range argv {
		switch a {
		case "--hook":
			wantHook = true
		case "--statusline":
			wantStatusline = true
		}
	}

	// --statusline wins when both flags are passed: statusline output is the
	// narrower contract (one tag, no newline) and tolerates being chained.
	if wantStatusline {
		_, _ = fmt.Fprint(out, renderStatuslineText(stateDir, findCCPID))
		return 0
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

// renderStatuslineText returns a compact "tg:@sN" tag suitable for Claude Code
// statusline composition. Returns an empty string when no session file exists
// (pre-Wire race during startup) so the caller can drop the segment silently.
func renderStatuslineText(stateDir string, ccPIDFn func() int) string {
	ccPID := ccPIDFn()
	if ccPID <= 0 {
		return ""
	}

	path := filepath.Join(stateDir, "sessions", strconv.Itoa(ccPID)+".json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var info sessionInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return ""
	}

	if info.Alias == "" {
		return ""
	}

	return "tg:@" + info.Alias
}

// renderSelfText loads the session snapshot keyed by the CC process PID. The
// ccPIDFn parameter is injected so tests don't need to fork a real CC tree.
func renderSelfText(stateDir string, ccPIDFn func() int) string {
	ccPID := ccPIDFn()
	if ccPID <= 0 {
		return "Telegram bridge: no shim alias registered for this session yet."
	}

	path := filepath.Join(stateDir, "sessions", strconv.Itoa(ccPID)+".json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return "Telegram bridge: no shim alias registered for this session yet."
	}

	var info sessionInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "Telegram bridge: session file present but unreadable."
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

	if peers := listLivePeers(stateDir, ccPID); len(peers) > 0 {
		slices.SortFunc(peers, func(a, b sessionInfo) int { return strings.Compare(a.Alias, b.Alias) })

		parts := make([]string, 0, len(peers))
		for _, p := range peers {
			parts = append(parts, fmt.Sprintf("@%s (%s)", p.Alias, p.Workdir))
		}

		fmt.Fprintf(&b, "\nPeers online: %s.", strings.Join(parts, ", "))
	}

	return b.String()
}

// listLivePeers returns sibling shim sessions registered in <stateDir>/sessions/
// excluding the caller's own session (matched by ccPID). A peer is considered
// live when /proc/<shim_pid>/comm reads as "telegram-mcp" — matching the
// daemon's PID-recycling defense (see internal/daemon/daemon.go:isOurDaemon).
// Stale files left by a crashed shim, unreadable/corrupt JSON, and recycled
// PIDs now owned by other processes are all skipped silently. Callers should
// inspect len() to gate output. Returns nil when the sessions dir is missing.
func listLivePeers(stateDir string, ownCCPID int) []sessionInfo {
	entries, err := os.ReadDir(filepath.Join(stateDir, "sessions"))
	if err != nil {
		return nil
	}

	peers := make([]sessionInfo, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(stateDir, "sessions", e.Name()))
		if err != nil {
			continue
		}

		var info sessionInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}

		if info.CCPID == ownCCPID || info.Alias == "" || info.ShimPID <= 0 {
			continue
		}

		if !peerProcAlive(info.ShimPID) {
			continue
		}

		peers = append(peers, info)
	}

	return peers
}

// peerProcAlive is swappable so tests can stand in for the /proc lookup
// without forking real telegram-mcp processes. Production reads
// /proc/<pid>/comm and matches "telegram-mcp" — same recycling defense the
// daemon uses for its PID file (see internal/daemon/daemon.go:isOurDaemon).
var peerProcAlive = func(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(raw)) == "telegram-mcp"
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

		return strconv.Atoi(strings.TrimSpace(rest))
	}

	return 0, fmt.Errorf("ppid not found for pid %d", pid)
}
