package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SessionInfo is the persisted snapshot of this shim's identity, written to
// ~/.claude/channels/telegram/sessions/<cc_pid>.json so the
// `telegram-mcp self` subcommand can render context for a CC session.
// CC pid (= shim's parent pid) is the correlation key because CC does not
// pass its session id through MCP initialize or the MCP server's env.
type SessionInfo struct {
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

var errInvalidCCPID = errors.New("cc_pid must be positive")

func sessionsDir(stateDir string) string {
	return filepath.Join(stateDir, "sessions")
}

func sessionPath(stateDir string, ccPID int) string {
	return filepath.Join(sessionsDir(stateDir), strconv.Itoa(ccPID)+".json")
}

// writeSessionFile atomically writes the per-session snapshot. Returns the
// final path on success. Caller may pass StartedAt=zero to let us fill in now().
func writeSessionFile(stateDir string, info SessionInfo) (string, error) {
	if info.CCPID <= 0 {
		return "", errInvalidCCPID
	}

	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now().UTC()
	}

	dir := sessionsDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir sessions dir: %w", err)
	}

	final := sessionPath(stateDir, info.CCPID)

	tmp, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}

	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = tmp.Close()
		cleanup()

		return "", fmt.Errorf("chmod tmp: %w", err)
	}

	if err := json.NewEncoder(tmp).Encode(info); err != nil {
		_ = tmp.Close()
		cleanup()

		return "", fmt.Errorf("encode: %w", err)
	}

	if err := tmp.Close(); err != nil {
		cleanup()

		return "", fmt.Errorf("close tmp: %w", err)
	}

	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()

		return "", fmt.Errorf("rename: %w", err)
	}

	return final, nil
}

// removeSessionFile is idempotent — missing file is not an error. ccPID=0 is
// a no-op so callers don't need to branch on "we never wrote a file".
func removeSessionFile(stateDir string, ccPID int) error {
	if ccPID <= 0 {
		return nil
	}

	if err := os.Remove(sessionPath(stateDir, ccPID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session file: %w", err)
	}

	return nil
}

func shimIDPrefix(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}

	return id[:n]
}
