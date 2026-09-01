//go:build integration

package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/stretchr/testify/require"
)

// Launches the real `claude` with the development-channels flag in a pty and
// asserts the consent gate answers the splash. Needs a trusted workdir (the
// repo root) and a claude on PATH; the spawned CC's shim attaches to the live
// daemon for a few seconds, then is killed.
//
//	go test -tags integration -run TestConsentGate_LiveClaude ./internal/daemon/
func TestConsentGate_LiveClaude(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}

	repo, err := os.Getwd()
	require.NoError(t, err)

	p, err := pty.New()
	require.NoError(t, err)
	require.NoError(t, p.Resize(spawnPtyCols, spawnPtyRows))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cmd := p.CommandContext(ctx, bin, "--dangerously-load-development-channels", "plugin:telegram@local-yakov")
	cmd.Dir = repo + "/../.."
	cmd.Env = filterEnv(os.Environ(), append([]string{"TELEGRAM_SPAWN_ID="}, parentCCEnvPrefixes...)...)
	require.NoError(t, cmd.Start())

	gate := newConsentGate()
	done := make(chan struct{})
	go func() { drainPty(p, gate, cmd.Process.Pid); close(done) }()

	require.Eventually(t, func() bool { return gate.presses > 0 }, 20*time.Second, 200*time.Millisecond,
		"consent splash was never matched — CC changed its rendering?")

	_ = cmd.Process.Kill()
	_ = p.Close()
	<-done
	_ = cmd.Wait()
}
