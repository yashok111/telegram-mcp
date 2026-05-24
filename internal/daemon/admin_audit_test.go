package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readAudit(t *testing.T, dir string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, "admin", "agent.log"))
	require.NoError(t, err)

	return string(b)
}

func TestAdminAuditLogWritesJSONLine(t *testing.T) {
	dir := t.TempDir()
	a := NewAdminAudit(dir, 0)

	a.Log("applied", "label_session", "labelled @s2 as build-bot", "admin", "ok")

	raw := readAudit(t, dir)
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	require.Len(t, lines, 1)

	var e map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.Equal(t, "applied", e["event"])
	assert.Equal(t, "label_session", e["tool"])
	assert.Equal(t, "labelled @s2 as build-bot", e["summary"])
	assert.Equal(t, "admin", e["actor"])
	assert.Equal(t, "ok", e["outcome"])
	assert.NotEmpty(t, e["ts"])
}

func TestAdminAuditLogAppends(t *testing.T) {
	dir := t.TempDir()
	a := NewAdminAudit(dir, 0)

	a.Log("requested", "evict_session", "evict @s2", "admin", "")
	a.Log("pending", "evict_session", "evict @s2", "admin", "awaiting owner")
	a.Log("confirmed", "evict_session", "evict @s2", "admin", "ok")

	lines := strings.Split(strings.TrimSpace(readAudit(t, dir)), "\n")
	assert.Len(t, lines, 3)
}

func TestAdminAuditFileMode0600(t *testing.T) {
	dir := t.TempDir()
	a := NewAdminAudit(dir, 0)
	a.Log("applied", "unpin_chat", "unpin 42", "admin", "ok")

	info, err := os.Stat(filepath.Join(dir, "admin", "agent.log"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestAdminAuditRotatesAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	a := NewAdminAudit(dir, 200) // tiny cap forces rotation quickly

	for range 20 {
		a.Log("applied", "broadcast_message", "broadcast to 1 chat", "admin", "ok")
	}

	backup, err := os.ReadFile(filepath.Join(dir, "admin", "agent.log.1"))
	require.NoError(t, err, "rotation must produce a .1 backup")
	require.NotEmpty(t, backup, "rotated backup must retain the audit data")

	// Rotated data must still be valid JSONL — rename must not truncate/corrupt.
	for ln := range strings.SplitSeq(strings.TrimSpace(string(backup)), "\n") {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(ln), &e), "rotated line must be valid JSON: %q", ln)
	}

	// The active log is recreated on the next write after a rotation, so it may
	// be momentarily absent right after one — but when present it stays bounded.
	if info, err := os.Stat(filepath.Join(dir, "admin", "agent.log")); err == nil {
		assert.Less(t, info.Size(), int64(400), "active log stays bounded after rotation")
	}
}

func TestAdminAuditNilSafe(t *testing.T) {
	var a *AdminAudit

	assert.NotPanics(t, func() { a.Log("applied", "x", "y", "admin", "ok") })
}
