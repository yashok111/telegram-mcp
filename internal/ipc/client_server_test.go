package ipc

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "test.sock")
	s := NewServer(sock)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan struct{})

	go func() {
		_ = s.Listen(ctx)
		close(done)
	}()
	t.Cleanup(func() { <-done })

	require.Eventually(t, func() bool {
		_, err := net.Dial("unix", sock)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "server never accepted")

	return s, sock
}

func TestClientCallEcho(t *testing.T) {
	s, sock := startTestServer(t)
	s.Handle("echo", func(_ context.Context, _ *Conn, params json.RawMessage) (any, *Error) {
		return map[string]any{"got": json.RawMessage(params)}, nil
	})

	c, err := Dial(sock)
	require.NoError(t, err)
	defer c.Close()

	var got struct {
		Got map[string]string `json:"got"`
	}
	err = c.Call(t.Context(), "echo", map[string]string{"x": "y"}, &got)
	require.NoError(t, err)
	assert.Equal(t, "y", got.Got["x"])
}

func TestClientCallError(t *testing.T) {
	s, sock := startTestServer(t)
	s.Handle("explode", func(_ context.Context, _ *Conn, _ json.RawMessage) (any, *Error) {
		return nil, &Error{Code: CodeBotError, Message: "boom"}
	})

	c, err := Dial(sock)
	require.NoError(t, err)
	defer c.Close()

	err = c.Call(t.Context(), "explode", nil, nil)
	require.Error(t, err)

	var rpcErr *Error
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, CodeBotError, rpcErr.Code)
}

func TestClientConcurrentCalls(t *testing.T) {
	s, sock := startTestServer(t)
	s.Handle("inc", func(_ context.Context, _ *Conn, params json.RawMessage) (any, *Error) {
		var p struct{ N int }
		_ = json.Unmarshal(params, &p)
		return map[string]int{"n": p.N + 1}, nil
	})

	c, err := Dial(sock)
	require.NoError(t, err)
	defer c.Close()

	const N = 50

	var wg sync.WaitGroup

	wg.Add(N)

	for i := range N {
		go func() {
			defer wg.Done()

			var got struct{ N int }
			err := c.Call(t.Context(), "inc", map[string]int{"N": i}, &got)
			assert.NoError(t, err)
			assert.Equal(t, i+1, got.N)
		}()
	}

	wg.Wait()
}

func TestClientReceivesServerNotification(t *testing.T) {
	s, sock := startTestServer(t)

	var conn atomic.Pointer[Conn]
	s.OnConnect(func(c *Conn) { conn.Store(c) })

	c, err := Dial(sock)
	require.NoError(t, err)
	defer c.Close()

	got := make(chan json.RawMessage, 1)
	c.OnNotify("pinged", func(_ context.Context, params json.RawMessage) {
		got <- params
	})

	require.Eventually(t, func() bool { return conn.Load() != nil }, time.Second, 10*time.Millisecond)

	require.NoError(t, conn.Load().Notify("pinged", map[string]string{"hello": "shim"}))

	select {
	case p := <-got:
		assert.JSONEq(t, `{"hello":"shim"}`, string(p))
	case <-time.After(time.Second):
		t.Fatal("notification not received")
	}
}

func TestClientNotifyServer(t *testing.T) {
	s, sock := startTestServer(t)

	got := make(chan json.RawMessage, 1)
	s.HandleNotify("bye", func(_ context.Context, _ *Conn, params json.RawMessage) {
		got <- params
	})

	c, err := Dial(sock)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Notify("bye", map[string]string{"r": "ok"}))

	select {
	case p := <-got:
		assert.JSONEq(t, `{"r":"ok"}`, string(p))
	case <-time.After(time.Second):
		t.Fatal("server didn't receive notification")
	}
}
