package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionInfo is the persisted snapshot of this shim's identity, written to
// ~/.claude/channels/telegram/sessions/<cc_session_id>.json so the
// `telegram-mcp self` subcommand can render context for a fresh CC session.
type SessionInfo struct {
	Alias        string    `json:"alias"`
	ShimID       string    `json:"shim_id"`
	ShimIDPrefix string    `json:"shim_id_prefix"`
	CCSessionID  string    `json:"cc_session_id"`
	Workdir      string    `json:"workdir"`
	Label        string    `json:"label,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	Mode         string    `json:"mode"`
}

var errEmptyCCSessionID = errors.New("cc_session_id is empty")

func sessionsDir(stateDir string) string {
	return filepath.Join(stateDir, "sessions")
}

func sessionPath(stateDir, ccSessionID string) string {
	return filepath.Join(sessionsDir(stateDir), ccSessionID+".json")
}

// writeSessionFile atomically writes the per-session snapshot. Returns the
// final path on success. Caller may pass StartedAt=zero to let us fill in now().
func writeSessionFile(stateDir string, info SessionInfo) (string, error) {
	if info.CCSessionID == "" {
		return "", errEmptyCCSessionID
	}

	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now().UTC()
	}

	dir := sessionsDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir sessions dir: %w", err)
	}

	final := sessionPath(stateDir, info.CCSessionID)

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

// removeSessionFile is idempotent — missing file is not an error.
func removeSessionFile(stateDir, ccSessionID string) error {
	if ccSessionID == "" {
		return nil
	}

	if err := os.Remove(sessionPath(stateDir, ccSessionID)); err != nil && !os.IsNotExist(err) {
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
