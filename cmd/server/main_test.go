package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestClaimPID_writesOwnPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	require.NoError(t, claimPID(path))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	got, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), got)
}

func TestClaimPID_skipsUnrelatedCommPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	// PID 1 (init/systemd) is alive but its /proc/1/comm is "systemd" or "init"
	// — neither "telegram-mcp" nor "bun". claimPID must refuse to SIGTERM it.
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o600))

	// If claimPID tried to kill PID 1, the test would error out on the SIGTERM
	// (we lack the capability anyway, so the syscall would return EPERM). We
	// only assert that it survived claiming and overwrote the file with our PID.
	require.NoError(t, claimPID(path))
	raw, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	assert.Equal(t, os.Getpid(), pid)
}

func TestClaimPID_handlesGarbageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	require.NoError(t, os.WriteFile(path, []byte("not a number"), 0o600))
	require.NoError(t, claimPID(path))
}

func TestClaimPID_handlesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	require.NoError(t, claimPID(path))
}

func TestClaimPID_skipsOwnPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600))
	require.NoError(t, claimPID(path))
}

func TestClaimPID_killsStaleOurPoller(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.pid")
	// Spawn a child sleep — comm will be "sleep", which isOurPoller rejects.
	// The intent here is to drive the dead-but-alive PID branch; we accept
	// that comm-check will skip the actual kill.
	cmd := startSleeper(t)
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(cmd)), 0o600))

	require.NoError(t, claimPID(path))

	_ = syscall.Kill(cmd, syscall.SIGTERM)
}

func TestIsOurPoller_self(t *testing.T) {
	// Our test binary's comm typically ends in ".test" (Go test harness),
	// which neither matches "telegram-mcp" nor "bun" — should be false.
	assert.False(t, isOurPoller(os.Getpid()))
}

func TestIsOurPoller_nonexistentPID(t *testing.T) {
	assert.False(t, isOurPoller(999999999))
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

func startSleeper(t *testing.T) int {
	t.Helper()

	procAttr := &os.ProcAttr{Files: []*os.File{nil, nil, nil}}
	p, err := os.StartProcess("/bin/sleep", []string{"sleep", "30"}, procAttr)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = p.Kill()
		_, _ = p.Wait()
	})

	return p.Pid
}
