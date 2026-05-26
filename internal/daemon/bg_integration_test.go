//go:build integration

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

// TestBgRunner_EndToEndWithFakeClaude exercises NewExecCommander against a
// real /bin/sh "claude" stub on PATH. Tagged `integration` so default
// `make test` doesn't depend on /bin/sh availability.
func TestBgRunner_EndToEndWithFakeClaude(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat <<'EOF'\n" +
		`{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"hi!","total_cost_usd":0.01}` + "\n" +
		"EOF\n"

	require.NoError(t, os.WriteFile(stub, []byte(script), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	fb := newLockedBot()
	fb.setSendRet(100, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            5 * time.Second,
		ClaudeBin:          "claude",
	}, fb, NewExecCommander())

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "hi", ChatID: "1", UserID: "u"})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 5*time.Second, 50*time.Millisecond)
	assert.Contains(t, fb.lastEditedText(), "✅ Task "+bot.MdCode(id))
	assert.Contains(t, fb.lastSentText(), "hi!")
}
