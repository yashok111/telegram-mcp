package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestResolveIdleTimeout_unsetReturnsDefault(t *testing.T) {
	_ = os.Unsetenv("TELEGRAM_DAEMON_IDLE_EXIT")

	assert.Equal(t, defaultDaemonIdleTimeout, resolveIdleTimeout())
}

func TestResolveIdleTimeout_zeroDisables(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "0")

	assert.Equal(t, time.Duration(0), resolveIdleTimeout())
}

func TestResolveIdleTimeout_negativeDisables(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "-1")

	assert.Equal(t, -1*time.Second, resolveIdleTimeout())
}

func TestResolveIdleTimeout_explicitSeconds(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "60")

	assert.Equal(t, 60*time.Second, resolveIdleTimeout())
}

func TestResolveIdleTimeout_invalidFallsBackToDefault(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "not-a-number")

	assert.Equal(t, defaultDaemonIdleTimeout, resolveIdleTimeout())
}

func TestResolveIdleTimeout_emptyFallsBackToDefault(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "")

	assert.Equal(t, defaultDaemonIdleTimeout, resolveIdleTimeout())
}

func TestResolveIdleTimeout_trimsWhitespace(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "  120  ")

	assert.Equal(t, 120*time.Second, resolveIdleTimeout())
}

func TestResolveIdleTimeout_overflowCapped(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "9223372036854775807")

	got := resolveIdleTimeout()
	assert.Positive(t, int64(got), "overflowed value must not become negative or zero")
}

func TestResolveIdleTimeout_largeSecondsDontOverflow(t *testing.T) {
	// Without ParseInt+cap, secs * time.Second wraps to negative on a
	// 64-bit overflow. With the cap, the result stays positive.
	t.Setenv("TELEGRAM_DAEMON_IDLE_EXIT", "999999999999")

	got := resolveIdleTimeout()
	assert.Positive(t, int64(got))
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
	// main() loads .env before dispatching to loadConfig; reproduce that
	// here so loadConfig finds the token in the process environment.
	require.NoError(t, loadDotEnv(filepath.Join(dir, ".env")))
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

func TestResolveClaudeBin_lookPathHit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())

	assert.Equal(t, bin, resolveClaudeBin())
}

func TestResolveClaudeBin_nvmFallbackPicksNewestByMtime(t *testing.T) {
	home := t.TempDir()
	older := filepath.Join(home, ".nvm", "versions", "node", "v23.10.0", "bin")
	newer := filepath.Join(home, ".nvm", "versions", "node", "v9.5.0", "bin") // lex < v23 to ensure mtime sort, not name sort

	require.NoError(t, os.MkdirAll(older, 0o750))
	require.NoError(t, os.MkdirAll(newer, 0o750))

	olderBin := filepath.Join(older, "claude")
	newerBin := filepath.Join(newer, "claude")

	require.NoError(t, os.WriteFile(olderBin, []byte("o"), 0o755))
	require.NoError(t, os.WriteFile(newerBin, []byte("n"), 0o755))

	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(olderBin, past, past))

	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "no-claude-here"))

	assert.Equal(t, newerBin, resolveClaudeBin())
}

func TestResolveClaudeBin_noMatchReturnsBareName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "empty"))

	assert.Equal(t, "claude", resolveClaudeBin())
}

// installPlugin sets up a marketplace manifest at
// ~/.claude/plugins/marketplaces/<channel>/.claude-plugin/marketplace.json
// listing the given plugins. If `installedDataMtime` is non-zero, also creates
// ~/.claude/plugins/data/telegram-<channel>/ with that mtime, simulating that
// the user has the plugin installed and last used it at that time. A zero
// mtime means "no data dir" → not installed.
func installPlugin(t *testing.T, home, channel string, installedDataMtime time.Time, pluginNames ...string) {
	t.Helper()

	dir := filepath.Join(home, ".claude", "plugins", "marketplaces", channel, ".claude-plugin")
	require.NoError(t, os.MkdirAll(dir, 0o750))

	plugins := make([]map[string]string, 0, len(pluginNames))
	for _, n := range pluginNames {
		plugins = append(plugins, map[string]string{"name": n})
	}

	body, err := json.Marshal(map[string]any{"name": channel, "plugins": plugins})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marketplace.json"), body, 0o600))

	if installedDataMtime.IsZero() {
		return
	}

	dataDir := filepath.Join(home, ".claude", "plugins", "data", "telegram-"+channel)
	require.NoError(t, os.MkdirAll(dataDir, 0o750))
	require.NoError(t, os.Chtimes(dataDir, installedDataMtime, installedDataMtime))
}

func TestResolveSpawnPluginSpec_noMatchReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Empty(t, resolveSpawnPluginSpec())
}

func TestResolveSpawnPluginSpec_singleInstalledMarketplace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installPlugin(t, home, "local-yakov", time.Now(), "telegram", "voice")

	assert.Equal(t, "plugin:telegram@local-yakov", resolveSpawnPluginSpec())
}

func TestResolveSpawnPluginSpec_marketplaceWithoutTelegramSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installPlugin(t, home, "caveman", time.Now(), "caveman")

	assert.Empty(t, resolveSpawnPluginSpec())
}

func TestResolveSpawnPluginSpec_skipsMarketplaceWithoutDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Marketplace exists but plugin is not installed (no data dir).
	installPlugin(t, home, "claude-plugins-official", time.Time{}, "telegram")

	assert.Empty(t, resolveSpawnPluginSpec())
}

func TestResolveSpawnPluginSpec_picksNewestByDataMtimeNotManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	now := time.Now()
	// Official: marketplace.json fresh (will be touched by CC refresh), but
	// data/ dir is older → user hasn't actually used this install recently.
	installPlugin(t, home, "claude-plugins-official", now.Add(-time.Hour), "telegram")
	// Bump the official marketplace.json mtime to be newest of all manifests.
	mfPath := filepath.Join(home, ".claude", "plugins", "marketplaces", "claude-plugins-official", ".claude-plugin", "marketplace.json")
	require.NoError(t, os.Chtimes(mfPath, now.Add(time.Hour), now.Add(time.Hour)))

	// Local: data/ dir mtime is now (most recent actual usage).
	installPlugin(t, home, "local-yakov", now, "telegram")

	assert.Equal(t, "plugin:telegram@local-yakov", resolveSpawnPluginSpec())
}
