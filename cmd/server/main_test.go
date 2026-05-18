package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDotEnv_setsMissingVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("FOO_TG_TEST=1\nBAR_TG_TEST=hello world\n"), 0o600))

	require.NoError(t, loadDotEnv(path))
	t.Cleanup(func() {
		_ = os.Unsetenv("FOO_TG_TEST")
		_ = os.Unsetenv("BAR_TG_TEST")
	})
	assert.Equal(t, "1", os.Getenv("FOO_TG_TEST"))
	assert.Equal(t, "hello world", os.Getenv("BAR_TG_TEST"))
}

func TestLoadDotEnv_realEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("TG_PRECEDENCE_TEST=from_file\n"), 0o600))
	t.Setenv("TG_PRECEDENCE_TEST", "from_real_env")

	require.NoError(t, loadDotEnv(path))
	assert.Equal(t, "from_real_env", os.Getenv("TG_PRECEDENCE_TEST"))
}

func TestLoadDotEnv_skipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("=novalue\nNOEQUALSIGN\nTG_GOOD_LINE=ok\n"), 0o600))

	require.NoError(t, loadDotEnv(path))
	t.Cleanup(func() { _ = os.Unsetenv("TG_GOOD_LINE") })
	assert.Equal(t, "ok", os.Getenv("TG_GOOD_LINE"))
}

func TestLoadDotEnv_missingFile_returnsErr(t *testing.T) {
	err := loadDotEnv("/no/such/path.env")
	assert.Error(t, err)
}

func TestResolveStateDir_envOverride(t *testing.T) {
	t.Setenv("TELEGRAM_STATE_DIR", "/explicit/path")
	assert.Equal(t, "/explicit/path", resolveStateDir())
}

func TestResolveStateDir_defaultHomeRelative(t *testing.T) {
	t.Setenv("TELEGRAM_STATE_DIR", "")

	dir := resolveStateDir()
	assert.True(t, strings.HasSuffix(dir, filepath.Join(".claude", "channels", "telegram")),
		"default lands under ~/.claude/channels/telegram (got %q)", dir)
}

func TestBootstrapStateDir_createsDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "channel-state")
	t.Setenv("TELEGRAM_STATE_DIR", stateDir)

	got, err := bootstrapStateDir()
	require.NoError(t, err)
	assert.Equal(t, stateDir, got)

	info, err := os.Stat(stateDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestBootstrapStateDir_mkdirError(t *testing.T) {
	// Point at a path whose parent is a file → MkdirAll fails.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))
	t.Setenv("TELEGRAM_STATE_DIR", filepath.Join(notADir, "inside"))

	_, err := bootstrapStateDir()
	assert.Error(t, err)
}

func TestLoadConfig_tokenFromEnv(t *testing.T) {
	dir := t.TempDir()
	// No .env file → loadDotEnv returns ENOENT, swallowed. Token comes from env.
	t.Setenv("TELEGRAM_BOT_TOKEN", "fromenv")
	t.Cleanup(func() { _ = os.Unsetenv("TELEGRAM_BOT_TOKEN") })

	tok, err := loadConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "fromenv", tok)
}

func TestLoadConfig_tokenFromDotEnv(t *testing.T) {
	dir := t.TempDir()
	_ = os.Unsetenv("TELEGRAM_BOT_TOKEN")

	t.Cleanup(func() { _ = os.Unsetenv("TELEGRAM_BOT_TOKEN") })
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TELEGRAM_BOT_TOKEN=fromfile\n"), 0o600))
	tok, err := loadConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "fromfile", tok)
}

func TestLoadConfig_missingToken(t *testing.T) {
	dir := t.TempDir()
	_ = os.Unsetenv("TELEGRAM_BOT_TOKEN")
	_, err := loadConfig(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TELEGRAM_BOT_TOKEN required")
}

func TestSetupSlog_doesNotPanic(_ *testing.T) {
	setupSlog()
}

func TestBindParentDeath_doesNotPanic(_ *testing.T) {
	bindParentDeath()
}

func TestSelectModeNoArg_defaultsToShim(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON", "")
	assert.Equal(t, modeShim, selectMode([]string{"telegram-mcp"}))
}

func TestSelectModeDaemonSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON", "")
	assert.Equal(t, modeDaemon, selectMode([]string{"telegram-mcp", "daemon"}))
}

func TestSelectModeShimSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON", "")
	assert.Equal(t, modeShim, selectMode([]string{"telegram-mcp", "shim"}))
}
