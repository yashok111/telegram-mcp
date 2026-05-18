package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFixtureSession(t *testing.T, dir string, ccPID int) {
	t.Helper()

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	payload := map[string]any{
		"alias":          "s3",
		"shim_id":        "abc123def456",
		"shim_id_prefix": "abc123de",
		"cc_pid":         ccPID,
		"shim_pid":       ccPID + 1,
		"cc_session_id":  "diag-sid",
		"workdir":        "/home/u/repo",
		"label":          "demo",
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"mode":           "shim",
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, strconv.Itoa(ccPID)+".json"), raw, 0o600))
}

func TestRunSelf_textMode_includesAliasAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "98765")
	writeFixtureSession(t, dir, 98765)

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	require.Equal(t, 0, code)

	body := out.String()
	assert.Contains(t, body, "@s3")
	assert.Contains(t, body, "/home/u/repo")
	assert.Contains(t, body, "abc123de")
	assert.Contains(t, body, "98765")
}

func TestRunSelf_textMode_missingFile_embedded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "11111")

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "embedded mode")
}

func TestRunSelf_textMode_ccPIDZero_embedded(t *testing.T) {
	dir := t.TempDir()

	var out bytes.Buffer

	// Inject ccPIDFn=0 directly via renderSelfText to bypass the live PPID walk
	// (the test binary's parent may itself be "claude" when run inside CC).
	got := renderSelfText(dir, func() int { return 0 })
	out.WriteString(got)
	assert.Contains(t, out.String(), "embedded mode")
}

func TestRunSelf_hookMode_emitsSessionStartJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "33333")
	writeFixtureSession(t, dir, 33333)

	var out bytes.Buffer

	code := runSelf(dir, []string{"--hook"}, &out)
	require.Equal(t, 0, code)

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}

	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	assert.Equal(t, "SessionStart", payload.HookSpecificOutput.HookEventName)
	assert.Contains(t, payload.HookSpecificOutput.AdditionalContext, "@s3")
}

func TestSelectModeSelfSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON", "")
	assert.Equal(t, modeSelf, selectMode([]string{"telegram-mcp", "self"}))
}

func TestFindCCPID_envOverrideWins(t *testing.T) {
	t.Setenv("CC_PID", "42")
	assert.Equal(t, 42, findCCPID())
}

func TestFindCCPID_invalidEnvFallsThroughToWalk(t *testing.T) {
	t.Setenv("CC_PID", "garbage")
	// Walk may find a "claude" ancestor when the test runs inside Claude Code,
	// or hit pid 1 otherwise. Either way, no panic — just exercise the fallback.
	_ = findCCPID()
}

func TestReadProcPPID_self(t *testing.T) {
	pid := os.Getpid()
	ppid, err := readProcPPID(pid)
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), ppid)
}

func TestReadProcComm_self(t *testing.T) {
	comm, err := readProcComm(os.Getpid())
	require.NoError(t, err)
	assert.NotEmpty(t, comm)
}
