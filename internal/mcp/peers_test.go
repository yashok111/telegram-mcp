package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

type fakePeerProvider struct {
	out []Peer
	err error
}

func (f *fakePeerProvider) Peers(_ context.Context) ([]Peer, error) {
	return f.out, f.err
}

func TestTelegramPeersEmbeddedMode(t *testing.T) {
	s, err := New(access.NewStore(t.TempDir(), false))
	require.NoError(t, err)

	res, err := s.handleTelegramPeers(context.Background(), mcptypes.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Contains(t, textOf(t, res), "embedded mode")
}

func TestTelegramPeersReturnsJSON(t *testing.T) {
	s, err := New(access.NewStore(t.TempDir(), false))
	require.NoError(t, err)

	s.AttachPeerProvider(&fakePeerProvider{out: []Peer{
		{Alias: "s1", ShimIDPrefix: "abcdef01", Workdir: "/a", Label: "", IdleSeconds: 300, Self: true},
		{Alias: "s2", ShimIDPrefix: "deadbeef", Workdir: "/b", Label: "hot", IdleSeconds: 30, Self: false},
	}})

	res, err := s.handleTelegramPeers(context.Background(), mcptypes.CallToolRequest{})
	require.NoError(t, err)

	txt := textOf(t, res)

	var got []map[string]any
	require.NoError(t, json.Unmarshal([]byte(txt), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "s1", got[0]["alias"])
	assert.Equal(t, "5m", got[0]["idle_for"])
	assert.Equal(t, true, got[0]["self"])
	assert.Equal(t, "30s", got[1]["idle_for"])
	assert.Equal(t, false, got[1]["self"])
}

func TestTelegramPeersProviderError(t *testing.T) {
	s, err := New(access.NewStore(t.TempDir(), false))
	require.NoError(t, err)

	s.AttachPeerProvider(&fakePeerProvider{err: errors.New("ipc down")})

	res, err := s.handleTelegramPeers(context.Background(), mcptypes.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, textOf(t, res), "ipc down")
}

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		secs int
		out  string
	}{
		{secs: 0, out: "0s"},
		{secs: -5, out: "0s"},
		{secs: 30, out: "30s"},
		{secs: 60, out: "1m"},
		{secs: 300, out: "5m"},
		{secs: 3600, out: "1h"},
		{secs: 3660, out: "1h1m"},
		{secs: 7325, out: "2h2m"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, humanizeDuration(c.secs), "secs=%d", c.secs)
	}
}

func textOf(t *testing.T, r *mcptypes.CallToolResult) string {
	t.Helper()
	require.Len(t, r.Content, 1)
	tc, ok := r.Content[0].(mcptypes.TextContent)
	require.True(t, ok, "expected TextContent, got %T", r.Content[0])

	return strings.TrimSpace(tc.Text)
}
