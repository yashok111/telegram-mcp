package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

type adminMutateFixture struct {
	sock     string
	tg       *tgFake
	router   *Router
	notifier *Notifier
	mutator  *AdminMutator
	stateDir string
	cleanup  func()
}

const testAdminToken = "integration-secret-token"

// newAdminMutateIntegration stands up the full daemon triangle wired exactly as
// cmd/server does for PR-3: real ipc.Server, real bot (tgFake-backed), real
// Router, an AdminMutator behind the token-gated admin.mutate method, and the
// Notifier.SetMutator seam for owner-tap resolution.
func newAdminMutateIntegration(t *testing.T) *adminMutateFixture {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"330621952"}, // the owner DM target for confirms
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	tg := &tgFake{nextMsgID: 100}
	ts := httptest.NewServer(tg.handler())

	router := NewRouter()
	notifier := NewNotifier(router, store, nil)

	tgBot, err := bot.NewFromAPIWithRouter("1234567890:AAH00000000000000000000000000000000", store, notifier, ts.URL, nil)
	require.NoError(t, err)

	mutator := NewAdminMutator(AdminMutateConfig{
		Store: store, Router: router, Bot: tgBot,
		Pending: NewPendingStore(dir), Audit: NewAdminAudit(dir, 0),
		RatePerHour: 60, PendingTTL: 5 * time.Minute,
	})
	notifier.SetMutator(mutator)

	sock := filepath.Join(dir, "daemon.sock")
	d := &Daemon{
		StateDir:     dir,
		SocketPath:   sock,
		PidPath:      filepath.Join(dir, "daemon.pid"),
		Store:        store,
		Bot:          tgBot,
		Router:       router,
		AdminToken:   testAdminToken,
		AdminMutator: mutator,
	}

	ctx, cancel := context.WithCancel(t.Context())
	daemonDone := make(chan struct{})
	pollDone := make(chan struct{})

	go func() { _ = d.Run(ctx); close(daemonDone) }()
	go func() { _ = tgBot.Poll(ctx); close(pollDone) }()

	require.Eventually(t, func() bool {
		c, derr := ipc.Dial(sock)
		if derr != nil {
			return false
		}

		_ = c.Close()

		return true
	}, 2*time.Second, 20*time.Millisecond)

	cleanup := func() {
		cancel()
		ts.Close()

		for _, ch := range []chan struct{}{daemonDone, pollDone} {
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ch:
			case <-timer.C:
				t.Error("daemon/poll did not exit cleanly")
			}

			timer.Stop()
		}
	}

	return &adminMutateFixture{sock: sock, tg: tg, router: router, notifier: notifier, mutator: mutator, stateDir: dir, cleanup: cleanup}
}

// callMutate dials the daemon and invokes admin.mutate one-shot (no hello).
func callMutate(t *testing.T, sock, token, tool string, toolArgs map[string]any) (AdminMutateResult, *ipc.Error) {
	t.Helper()

	c, err := ipc.Dial(sock)
	require.NoError(t, err)

	defer func() { _ = c.Close() }()

	rawArgs, err := json.Marshal(toolArgs)
	require.NoError(t, err)

	var res AdminMutateResult

	cerr := c.Call(t.Context(), ipc.MethodAdminMutate, map[string]any{
		"token": token, "tool": tool, "args": json.RawMessage(rawArgs),
	}, &res)
	if cerr == nil {
		return res, nil
	}

	var rpcErr *ipc.Error
	if assert.ErrorAs(t, cerr, &rpcErr) {
		return AdminMutateResult{}, rpcErr
	}

	return AdminMutateResult{}, &ipc.Error{Message: cerr.Error()}
}

// connectEvictTarget connects a plain user shim and returns its client, id, and
// a channel that fires when the daemon pushes NotifyShutdown to it.
func connectEvictTarget(t *testing.T, sock string) (*ipc.Client, string, <-chan struct{}) {
	t.Helper()

	c, id := connectShim(t, sock)

	shutdown := make(chan struct{}, 1)

	c.OnNotify(ipc.NotifyShutdown, func(_ context.Context, _ json.RawMessage) {
		select {
		case shutdown <- struct{}{}:
		default:
		}
	})

	return c, id, shutdown
}

func auditContains(t *testing.T, stateDir, substr string) bool {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(stateDir, "admin", "agent.log"))
	require.NoError(t, err)

	return assert.Contains(t, string(b), substr)
}

func TestIntegrationTier3EvictConfirmRoundTrip(t *testing.T) {
	f := newAdminMutateIntegration(t)
	defer f.cleanup()

	target, targetID, shutdown := connectEvictTarget(t, f.sock)
	defer target.Close()

	// 1. Admin requests a Tier-3 evict → pending, NOT applied.
	res, rpcErr := callMutate(t, f.sock, testAdminToken, "evict_session", map[string]any{"target": targetID})
	require.Nil(t, rpcErr)
	assert.False(t, res.Applied)
	assert.True(t, res.Pending)
	assert.Equal(t, 3, res.Tier)
	require.NotEmpty(t, res.PendingID)

	// 2. A confirm with ✅/❌ buttons was rendered to the owner.
	require.Eventually(t, func() bool {
		return f.tg.sendBodyContains("am:" + res.PendingID + ":confirm")
	}, time.Second, 20*time.Millisecond, "owner confirm with buttons must render")

	// The target must NOT have been shut down yet (awaiting the tap).
	select {
	case <-shutdown:
		t.Fatal("Tier-3 evict applied before the owner tapped approve")
	case <-time.After(100 * time.Millisecond):
	}

	// 3. Owner taps ✅ (gate-checked in bot; here we drive the resolved seam).
	applied, _ := f.notifier.ResolveMutation(t.Context(), res.PendingID, true)
	assert.True(t, applied)

	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("target shim never received NotifyShutdown after owner approval")
	}

	assert.True(t, auditContains(t, f.stateDir, `"confirmed"`))
}

func TestIntegrationTier3CancelDoesNotApply(t *testing.T) {
	f := newAdminMutateIntegration(t)
	defer f.cleanup()

	target, targetID, shutdown := connectEvictTarget(t, f.sock)
	defer target.Close()

	res, rpcErr := callMutate(t, f.sock, testAdminToken, "evict_session", map[string]any{"target": targetID})
	require.Nil(t, rpcErr)
	require.True(t, res.Pending)

	// Owner taps ❌.
	applied, _ := f.notifier.ResolveMutation(t.Context(), res.PendingID, false)
	assert.False(t, applied)

	select {
	case <-shutdown:
		t.Fatal("cancelled evict must NOT shut the target down")
	case <-time.After(200 * time.Millisecond):
	}

	assert.True(t, auditContains(t, f.stateDir, `"cancelled"`))
}

func TestIntegrationTier3ExpirySweep(t *testing.T) {
	f := newAdminMutateIntegration(t)
	defer f.cleanup()

	target, targetID, shutdown := connectEvictTarget(t, f.sock)
	defer target.Close()

	res, rpcErr := callMutate(t, f.sock, testAdminToken, "evict_session", map[string]any{"target": targetID})
	require.Nil(t, rpcErr)
	require.True(t, res.Pending)

	// Force expiry deterministically (no wall-clock race): re-stamp the pending
	// record's CreatedAt into the past, then run the same sweep the production
	// ticker calls every minute.
	p, ok := f.mutator.pending.Take(res.PendingID)
	require.True(t, ok)

	p.CreatedAt = time.Now().Add(-time.Hour)
	require.NoError(t, f.mutator.pending.Put(p))

	f.mutator.sweepExpired(t.Context())

	// A tap after expiry finds nothing and applies nothing.
	applied, detail := f.notifier.ResolveMutation(t.Context(), res.PendingID, true)
	assert.False(t, applied)
	assert.Contains(t, detail, "no longer available")

	select {
	case <-shutdown:
		t.Fatal("expired evict must not apply")
	case <-time.After(100 * time.Millisecond):
	}

	assert.True(t, auditContains(t, f.stateDir, `"expired"`))
}

func TestIntegrationAdminMutateRejectsBadToken(t *testing.T) {
	f := newAdminMutateIntegration(t)
	defer f.cleanup()

	_, rpcErr := callMutate(t, f.sock, "wrong-token", "unpin_chat", map[string]any{"chat_id": "42"})
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeUnauthorized, rpcErr.Code)
}

func TestIntegrationTier2AppliesInline(t *testing.T) {
	f := newAdminMutateIntegration(t)
	defer f.cleanup()

	_, targetID := connectShim(t, f.sock)

	res, rpcErr := callMutate(t, f.sock, testAdminToken, "label_session", map[string]any{"target": targetID, "label": "build-bot"})
	require.Nil(t, rpcErr)
	assert.True(t, res.Applied)
	assert.Equal(t, 2, res.Tier)

	for _, info := range f.router.Snapshot() {
		if info.ID == targetID {
			assert.Equal(t, "build-bot", info.Label)
		}
	}
}
