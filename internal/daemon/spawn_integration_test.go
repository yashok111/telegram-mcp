//go:build integration

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

// TestSpawnRunner_EndToEndWithFakeClaude exercises NewExecSpawnCommander
// (real pty.Start) against a /bin/sh stub that mimics a long-lived claude
// session — sleeps until SIGTERM. Asserts: pty-attached subprocess starts,
// the TELEGRAM_SPAWN_ID env propagates, Cancel signals the child, and the
// task drains.
//
// Tagged `integration` so default `make test` doesn't depend on /bin/sh.
func TestSpawnRunner_EndToEndWithFakeClaude(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")

	// The stub prints its env (so the test can assert SPAWN_ID propagation
	// via the pty drain isn't load-bearing — we read the file it writes),
	// then sleeps forever. SIGTERM ends the sleep so Cancel() drains.
	envProbe := filepath.Join(dir, "env_probe.txt")
	script := "#!/bin/sh\n" +
		"echo \"$TELEGRAM_SPAWN_ID\" > " + envProbe + "\n" +
		"trap 'exit 0' TERM\n" +
		"while :; do sleep 0.1; done\n"

	require.NoError(t, os.WriteFile(stub, []byte(script), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        30 * time.Second,
		ClaudeBin:          "claude",
		ClaudeArgs:         []string{}, // stub doesn't need args
	}, fb, NewExecSpawnCommander())

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: dir, ChatID: "42", UserID: "u",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Wait for stub to write the env probe — proves pty + env propagated.
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(envProbe)
		if err != nil {
			return false
		}

		return strings.TrimSpace(string(raw)) == id
	}, 5*time.Second, 50*time.Millisecond, "spawned stub must see TELEGRAM_SPAWN_ID=<id>")

	// /spawn list must report the running task.
	listBefore := r.List()
	require.Len(t, listBefore, 1)
	assert.Equal(t, id, listBefore[0].ID)
	assert.Positive(t, listBefore[0].Pid)
	assert.Equal(t, string(SpawnStatusRunning), listBefore[0].Status)

	// Cancel → SIGTERM → trap fires → exit 0 → runSpawn drains.
	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 5*time.Second, 50*time.Millisecond)

	// Start confirmation must have been posted to TG.
	assert.True(t, slices.ContainsFunc(fb.sent(), func(s string) bool {
		return strings.Contains(s, "🚀 Spawn "+id+" started")
	}), "start confirmation must hit TG; got=%v", fb.sent())
}
