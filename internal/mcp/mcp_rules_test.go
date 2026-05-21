package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestPermissionRequest_matchingApproveRule_resolvesAllow_noBroadcast(t *testing.T) {
	srv, fb, dir := newServerWithAllowlist(t, "330621952")
	store := access.NewStore(dir, false)
	st := store.Load()
	access.AddRule(&st, access.PermissionRule{Tool: "Read", Action: access.RuleApprove})
	require.NoError(t, store.Save(st))

	srv.handlePermissionRequest(t.Context(), "req001", "Read", "read file", `{"file_path":"/etc/hosts"}`)

	assert.Empty(t, fb.broadcastIDs, "broadcast must not fire when rule matches")

	_, ok := srv.LookupPermission("req001")
	assert.False(t, ok, "ResolvePermission should clear pending entry")
}

func TestPermissionRequest_matchingDenyRule_resolvesDeny_noBroadcast(t *testing.T) {
	srv, fb, dir := newServerWithAllowlist(t, "330621952")
	store := access.NewStore(dir, false)
	st := store.Load()
	access.AddRule(&st, access.PermissionRule{Tool: "Bash", Action: access.RuleDeny})
	require.NoError(t, store.Save(st))

	srv.handlePermissionRequest(t.Context(), "req002", "Bash", "run cmd", `{"command":"rm -rf /"}`)

	assert.Empty(t, fb.broadcastIDs, "broadcast must not fire when deny rule matches")

	_, ok := srv.LookupPermission("req002")
	assert.False(t, ok)
}

func TestPermissionRequest_noRuleMatch_broadcasts(t *testing.T) {
	srv, fb, dir := newServerWithAllowlist(t, "330621952")
	store := access.NewStore(dir, false)
	st := store.Load()
	access.AddRule(&st, access.PermissionRule{Tool: "Read", Action: access.RuleApprove})
	require.NoError(t, store.Save(st))

	srv.handlePermissionRequest(t.Context(), "req003", "Bash", "shell", `{"command":"ls"}`)
	srv.broadcastWG.Wait()

	assert.Equal(t, []string{"req003"}, fb.broadcastIDs, "broadcast must fire when no rule matches")

	_, ok := srv.LookupPermission("req003")
	assert.True(t, ok, "pending entry remains until callback resolves it")
}

func TestPermissionRequest_expiredRule_broadcasts(t *testing.T) {
	srv, fb, dir := newServerWithAllowlist(t, "330621952")
	store := access.NewStore(dir, false)
	st := store.Load()
	access.AddRule(&st, access.PermissionRule{
		Tool:      "Read",
		Action:    access.RuleApprove,
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	require.NoError(t, store.Save(st))

	srv.handlePermissionRequest(t.Context(), "req004", "Read", "read", `{"file_path":"/tmp/x"}`)
	srv.broadcastWG.Wait()

	assert.Equal(t, []string{"req004"}, fb.broadcastIDs, "expired rules should not short-circuit broadcast")
}

func TestExtractToolPath_filePathField(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"json", `{"file_path":"/etc/hosts"}`, "/etc/hosts"},
		{"json_spaced", `{ "file_path" : "/var/log/syslog" }`, "/var/log/syslog"},
		{"kv", `file_path=/home/u/x.go`, "/home/u/x.go"},
		{"quoted_with_spaces", `{"file_path":"/home/u/my docs/x.go"}`, "/home/u/my docs/x.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractToolPath("Read", tt.input))
		})
	}
}

func TestExtractToolPath_pathField(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"path", `{"path":"/foo/bar"}`, "/foo/bar"},
		{"notebook_path", `{"notebook_path":"/n/book.ipynb"}`, "/n/book.ipynb"},
		{"pattern", `{"pattern":"*.go"}`, "*.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractToolPath("Glob", tt.input))
		})
	}
}

func TestExtractToolPath_noField_returnsEmpty(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"unrelated", `{"command":"ls -la"}`},
		{"plain_text", `just some description`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Empty(t, extractToolPath("Bash", tt.input))
		})
	}
}
