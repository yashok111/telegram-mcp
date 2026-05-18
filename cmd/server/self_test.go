package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFixtureSession(t *testing.T, dir, ccSID string) {
	t.Helper()

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	payload := map[string]any{
		"alias":          "s3",
		"shim_id":        "abc123def456",
		"shim_id_prefix": "abc123de",
		"cc_session_id":  ccSID,
		"workdir":        "/home/u/repo",
		"label":          "demo",
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"mode":           "shim",
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, ccSID+".json"), raw, 0o600))
}

func TestRunSelf_textMode_includesAliasAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ccsid-1")
	writeFixtureSession(t, dir, "ccsid-1")

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	require.Equal(t, 0, code)

	body := out.String()
	assert.Contains(t, body, "@s3")
	assert.Contains(t, body, "/home/u/repo")
	assert.Contains(t, body, "abc123de")
	assert.Contains(t, body, "ccsid-1")
}

func TestRunSelf_textMode_missingFile_embedded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "no-such-sid")

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "embedded mode")
}

func TestRunSelf_textMode_emptyEnv_quietExit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "embedded mode")
}

func TestRunSelf_hookMode_emitsSessionStartJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ccsid-2")
	writeFixtureSession(t, dir, "ccsid-2")

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
