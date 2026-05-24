package admin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// serveMutate stands up a real ipc.Server whose admin.mutate handler records the
// received params and returns reply. Returns the socket path.
func serveMutate(t *testing.T, reply any, capture *map[string]any) string {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "d.sock")
	srv := ipc.NewServer(sock)
	srv.Handle(ipc.MethodAdminMutate, func(_ context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
		if capture != nil {
			_ = json.Unmarshal(params, capture)
		}

		return reply, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { _ = srv.Listen(ctx); close(done) }()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		c, err := ipc.Dial(sock)
		if err != nil {
			return false
		}

		_ = c.Close()

		return true
	}, 2*time.Second, 20*time.Millisecond)

	return sock
}

func TestRequestMutationRoundTrips(t *testing.T) {
	var got map[string]any

	sock := serveMutate(t, MutateResult{Tool: "label_session", Tier: 2, Applied: true, Result: "labelled @s2"}, &got)

	res, err := RequestMutation(context.Background(), sock, "tok", "label_session", map[string]any{"target": "s2", "label": "build"})
	require.NoError(t, err)
	assert.True(t, res.Applied)
	assert.Equal(t, 2, res.Tier)
	assert.Equal(t, "labelled @s2", res.Result)

	// The daemon receives token + tool + args (and no caller-supplied tier).
	assert.Equal(t, "tok", got["token"])
	assert.Equal(t, "label_session", got["tool"])
	assert.NotContains(t, got, "tier", "caller must not be able to claim a tier")
}

func TestRequestMutationPendingTier3(t *testing.T) {
	sock := serveMutate(t, MutateResult{Tool: "evict_session", Tier: 3, Pending: true, PendingID: "ab12cd34", Result: "awaiting"}, nil)

	res, err := RequestMutation(context.Background(), sock, "tok", "evict_session", map[string]any{"target": "s2"})
	require.NoError(t, err)
	assert.True(t, res.Pending)
	assert.False(t, res.Applied)
	assert.Equal(t, "ab12cd34", res.PendingID)
}

func TestRequestMutationPropagatesError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	srv := ipc.NewServer(sock)
	srv.Handle(ipc.MethodAdminMutate, func(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, *ipc.Error) {
		return nil, &ipc.Error{Code: ipc.CodeMutateRejected, Message: "unknown mutate tool"}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { _ = srv.Listen(ctx); close(done) }()

	t.Cleanup(func() { cancel(); <-done })

	require.Eventually(t, func() bool {
		c, err := ipc.Dial(sock)
		if err != nil {
			return false
		}

		_ = c.Close()

		return true
	}, 2*time.Second, 20*time.Millisecond)

	_, err := RequestMutation(context.Background(), sock, "tok", "drop_db", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mutate tool")
}
