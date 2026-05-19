package bot

import (
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestCommandArg(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/reaction", ""},
		{"/reaction 👀", "👀"},
		{"/reaction\toff", "off"},
		{"/reaction   👀  ", "👀"},
		{"/reaction off extra", "off extra"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, commandArg(tc.in))
		})
	}
}

func TestRenderReactionStatus(t *testing.T) {
	assert.Contains(t, renderReactionStatus(""), "off")
	assert.Contains(t, renderReactionStatus("👀"), "👀")
}

func TestHandleReactionCommand_showsCurrent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/reaction"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "👀")
}

func TestHandleReactionCommand_setsEmoji(t *testing.T) {
	b, _, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/reaction ⏳"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	store := access.NewStore(dir, false)
	assert.Equal(t, "⏳", store.Load().AckReaction)
}

func TestHandleReactionCommand_clearsWithOff(t *testing.T) {
	b, _, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/reaction off"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	store := access.NewStore(dir, false)
	assert.Empty(t, store.Load().AckReaction)
}

func TestHandleReactionCommand_offCaseInsensitive(t *testing.T) {
	b, _, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/reaction OFF"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	store := access.NewStore(dir, false)
	assert.Empty(t, store.Load().AckReaction)
}

func TestHandleReactionCommand_rejectsTooLong(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/reaction aaaaaaaaa"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "too long")

	store := access.NewStore(dir, false)
	assert.Equal(t, "👀", store.Load().AckReaction, "rejected input must not mutate the store")
}

func TestHandleReactionCommand_dmOnly(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: -100, Type: "supergroup"}, From: &telego.User{ID: 1}, Text: "/reaction off"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, api.recordedCalls("sendMessage"), "non-DM commands are ignored")

	store := access.NewStore(dir, false)
	assert.Equal(t, "👀", store.Load().AckReaction, "non-DM commands must not mutate the store")
}

func TestHandleReactionCommand_blockedForNonAllowlistedSender(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 999}, Text: "/reaction off"}

	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, api.recordedCalls("sendMessage"))

	store := access.NewStore(dir, false)
	assert.Equal(t, "👀", store.Load().AckReaction)
}
