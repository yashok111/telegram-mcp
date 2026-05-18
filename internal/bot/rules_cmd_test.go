package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

// ===== renderRules =====

func TestRenderRules_emptyList(t *testing.T) {
	out := renderRules(nil)
	assert.Contains(t, out, "No permission rules")
}

func TestRenderRules_singleRuleWithExpiry(t *testing.T) {
	rules := []access.PermissionRule{
		{
			ID:        "r1",
			Tool:      "Bash",
			Action:    access.RuleApprove,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}
	out := renderRules(rules)
	assert.Contains(t, out, "Active permission rules:")
	assert.Contains(t, out, "r1")
	assert.Contains(t, out, "Bash")
	assert.Contains(t, out, "approve")
	assert.Contains(t, out, "expires in")
	assert.Contains(t, out, "(any path)")
}

func TestRenderRules_singleRuleNoExpiry(t *testing.T) {
	rules := []access.PermissionRule{
		{
			ID:     "r2",
			Tool:   "Read",
			Action: access.RuleApprove,
		},
	}
	out := renderRules(rules)
	assert.Contains(t, out, "r2")
	assert.Contains(t, out, "Read")
	assert.Contains(t, out, "never")
}

func TestRenderRules_multiplerules_listed(t *testing.T) {
	rules := []access.PermissionRule{
		{ID: "r1", Tool: "Bash", Action: access.RuleApprove},
		{ID: "r2", Tool: "Edit", PathPattern: "/foo/*.go", Action: access.RuleDeny},
	}
	out := renderRules(rules)
	assert.Contains(t, out, "r1")
	assert.Contains(t, out, "r2")
	assert.Contains(t, out, "Bash")
	assert.Contains(t, out, "Edit")
	assert.Contains(t, out, "/foo/*.go")
	assert.Contains(t, out, "deny")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Header + 2 rule lines.
	assert.Len(t, lines, 3)
}

// ===== handleRulesCommand =====

func TestHandleRulesCommand_list_empty(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "No permission rules")
}

func TestHandleRulesCommand_list_renders(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "ab1234", Tool: "Bash", Action: access.RuleApprove},
		},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text := calls[0].params["text"].(string)
	assert.Contains(t, text, "ab1234")
	assert.Contains(t, text, "Bash")
	assert.Contains(t, text, "approve")
}

func TestHandleRulesCommand_clear(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "a", Tool: "Bash", Action: access.RuleApprove},
			{ID: "b", Tool: "Read", Action: access.RuleApprove},
		},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules clear"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Cleared 2 rule(s)")

	st := b.store.Load()
	assert.Empty(t, st.Rules)
}

func TestHandleRulesCommand_revoke_existing(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "abc", Tool: "Bash", Action: access.RuleApprove},
			{ID: "def", Tool: "Read", Action: access.RuleApprove},
		},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules revoke abc"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Revoked rule abc")

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	assert.Equal(t, "def", st.Rules[0].ID)
}

func TestHandleRulesCommand_revoke_missing(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "abc", Tool: "Bash", Action: access.RuleApprove},
		},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules revoke nope"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "No rule with id nope")

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	assert.Equal(t, "abc", st.Rules[0].ID)
}

func TestHandleRulesCommand_revoke_noID_showsUsage(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/rules revoke"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Usage: /rules revoke <id>")
}
