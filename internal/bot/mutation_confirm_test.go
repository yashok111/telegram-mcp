package bot

import (
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func allowlist42() access.State {
	return access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	}
}

func TestAmCallbackRE(t *testing.T) {
	assert.True(t, amCallbackRE.MatchString("am:ab12cd34:confirm"))
	assert.True(t, amCallbackRE.MatchString("am:deadbeef:cancel"))
	assert.False(t, amCallbackRE.MatchString("am:ab12cd34:approve"), "only confirm|cancel")
	assert.False(t, amCallbackRE.MatchString("am:NOThex:confirm"), "id must be hex")
	assert.False(t, amCallbackRE.MatchString("perm:allow:abcde"))
}

func TestBroadcastMutationConfirmRendersButtons(t *testing.T) {
	b, api, _ := newTestBot(t, allowlist42())

	msgID, err := b.BroadcastMutationConfirm(t.Context(), PermissionTarget{ChatID: 42}, "ab12cd34", "evict session @s2 (workdir /foo)")
	require.NoError(t, err)
	assert.NotZero(t, msgID)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	body := payloadString(calls[0].params)
	assert.Contains(t, body, "am:ab12cd34:confirm")
	assert.Contains(t, body, "am:ab12cd34:cancel")
	assert.Contains(t, body, "evict session @s2")
}

func TestBroadcastMutationConfirmZeroChatErrors(t *testing.T) {
	b, _, _ := newTestBot(t, allowlist42())

	_, err := b.BroadcastMutationConfirm(t.Context(), PermissionTarget{}, "ab12cd34", "x")
	require.Error(t, err)
}

func TestCallbackMutationConfirmResolves(t *testing.T) {
	b, api, _ := newTestBot(t, allowlist42())
	n, _ := b.notifier.(*noopNotifier)
	n.mutateApplied = true
	n.mutateDetail = "evicted @s2"

	q := telego.CallbackQuery{
		ID: "cq", From: telego.User{ID: 42}, Data: "am:ab12cd34:confirm",
		Message: &telego.Message{MessageID: 7, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "🛠 Admin action awaiting your approval:\n\nevict @s2"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	require.Len(t, n.mutations, 1)
	assert.Equal(t, "ab12cd34", n.mutations[0].pendingID)
	assert.True(t, n.mutations[0].approve)

	assert.NotEmpty(t, api.recordedCalls("answerCallbackQuery"))

	edits := api.recordedCalls("editMessageText")
	require.NotEmpty(t, edits)
	assert.Contains(t, payloadString(edits[0].params), "Applied")
}

func TestCallbackMutationCancelResolves(t *testing.T) {
	b, _, _ := newTestBot(t, allowlist42())
	n, _ := b.notifier.(*noopNotifier)

	q := telego.CallbackQuery{
		ID: "cq", From: telego.User{ID: 42}, Data: "am:ab12cd34:cancel",
		Message: &telego.Message{MessageID: 7, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "🛠 Admin action"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	require.Len(t, n.mutations, 1)
	assert.False(t, n.mutations[0].approve, "cancel tap must pass approve=false")
}

func TestCallbackMutationNotAllowlistedRefused(t *testing.T) {
	b, api, _ := newTestBot(t, allowlist42())
	n, _ := b.notifier.(*noopNotifier)

	q := telego.CallbackQuery{
		ID: "cq", From: telego.User{ID: 999}, Data: "am:ab12cd34:confirm",
		Message: &telego.Message{MessageID: 7, Chat: telego.Chat{ID: 999, Type: "private"}, Text: "🛠 Admin action"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Empty(t, n.mutations, "non-allowlisted tap must NOT resolve the mutation")

	calls := api.recordedCalls("answerCallbackQuery")
	require.NotEmpty(t, calls)
	assert.Contains(t, payloadString(calls[0].params), "Not authorized")
}

// TestCallbackMutationNonOwnerAllowlistedRefused: admin mutations are owner-only.
// A second allowlisted user (43) must not be able to approve a mutation
// confirmed to the owner (42), even though they could answer permission prompts.
func TestCallbackMutationNonOwnerAllowlistedRefused(t *testing.T) {
	st := access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42", "43"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	}
	b, api, _ := newTestBot(t, st)
	n, _ := b.notifier.(*noopNotifier)

	q := telego.CallbackQuery{
		ID: "cq", From: telego.User{ID: 43}, Data: "am:ab12cd34:confirm",
		Message: &telego.Message{MessageID: 7, Chat: telego.Chat{ID: 43, Type: "private"}, Text: "🛠 Admin action"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Empty(t, n.mutations, "non-owner allowlisted tap must NOT resolve the mutation")

	calls := api.recordedCalls("answerCallbackQuery")
	require.NotEmpty(t, calls)
	assert.Contains(t, payloadString(calls[0].params), "Owner only")
}

func TestIsOwnerID(t *testing.T) {
	assert.True(t, isOwnerID([]string{"42", "43"}, 42), "first parseable entry is owner")
	assert.False(t, isOwnerID([]string{"42", "43"}, 43), "second allowlisted user is not owner")
	assert.True(t, isOwnerID([]string{"  42  "}, 42), "whitespace trimmed")
	assert.True(t, isOwnerID([]string{"nan", "42"}, 42), "skips unparseable, first parseable wins")
	assert.False(t, isOwnerID(nil, 42))
	// A negative group/channel id must be skipped — the first POSITIVE id (a DM)
	// is the owner, not whichever entry sorts first.
	assert.True(t, isOwnerID([]string{"-1001234567890", "42"}, 42), "negative group id skipped")
	assert.False(t, isOwnerID([]string{"-1001234567890", "42"}, -1001234567890), "a group id is never the owner")
}
