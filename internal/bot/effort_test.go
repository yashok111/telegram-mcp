package bot

import (
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestResolveEffort_knownLevels(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		wantModel       string
		wantThinkTokens int
	}{
		{name: "low lowercase", input: "low", wantModel: "claude-haiku-4-5", wantThinkTokens: 0},
		{name: "medium lowercase", input: "medium", wantModel: "claude-sonnet-4-6", wantThinkTokens: 8000},
		{name: "high lowercase", input: "high", wantModel: "claude-opus-4-7", wantThinkTokens: 16000},
		{name: "xhigh lowercase", input: "xhigh", wantModel: "claude-opus-4-7", wantThinkTokens: 32000},
		{name: "max lowercase", input: "max", wantModel: "claude-opus-4-7", wantThinkTokens: 64000},
		{name: "high uppercase", input: "HIGH", wantModel: "claude-opus-4-7", wantThinkTokens: 16000},
		{name: "medium mixed case", input: "MeDiUm", wantModel: "claude-sonnet-4-6", wantThinkTokens: 8000},
		{name: "high whitespace padded", input: "  high  ", wantModel: "claude-opus-4-7", wantThinkTokens: 16000},
		{name: "max tab padded", input: "\tmax\t", wantModel: "claude-opus-4-7", wantThinkTokens: 64000},
		{name: "xhigh uppercase", input: "XHIGH", wantModel: "claude-opus-4-7", wantThinkTokens: 32000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveEffort(tt.input)
			require.True(t, ok)
			assert.Equal(t, tt.wantModel, got.Model)
			assert.Equal(t, tt.wantThinkTokens, got.ThinkingTokens)
		})
	}
}

func TestResolveEffort_unknown(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty string", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "thinking keyword", input: "thinking"},
		{name: "garbage", input: "garbage"},
		{name: "partial low", input: "lo"},
		{name: "extra suffix", input: "lowx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := ResolveEffort(tt.input)
			assert.False(t, ok)
		})
	}
}

func TestAllEfforts_orderAndCoverage(t *testing.T) {
	got := AllEfforts()
	want := []EffortLevel{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
	assert.Equal(t, want, got)

	for _, level := range got {
		_, ok := effortConfigs[level]
		assert.True(t, ok, "level %q missing in effortConfigs", level)
	}

	assert.Len(t, effortConfigs, len(got))
}

func TestParseEffortArgs(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSub   EffortSubCmd
		wantLevel EffortLevel
		wantErr   error
	}{
		{name: "empty defaults to show", input: "", wantSub: EffortSubShow},
		{name: "show lowercase", input: "show", wantSub: EffortSubShow},
		{name: "show uppercase", input: "SHOW", wantSub: EffortSubShow},
		{name: "help", input: "help", wantSub: EffortSubHelp},
		{name: "clear", input: "clear", wantSub: EffortSubClear},
		{name: "off", input: "off", wantSub: EffortSubClear},
		{name: "reset", input: "reset", wantSub: EffortSubClear},
		{name: "low set", input: "low", wantSub: EffortSubSet, wantLevel: EffortLow},
		{name: "MEDIUM set", input: "MEDIUM", wantSub: EffortSubSet, wantLevel: EffortMedium},
		{name: "high whitespace padded", input: "  high  ", wantSub: EffortSubSet, wantLevel: EffortHigh},
		{name: "xhigh set", input: "xhigh", wantSub: EffortSubSet, wantLevel: EffortXHigh},
		{name: "max set", input: "max", wantSub: EffortSubSet, wantLevel: EffortMax},
		{name: "garbage errors", input: "garbage", wantErr: ErrEffortUnknownLevel},
		{name: "low extra token errors", input: "low extra", wantErr: ErrEffortUnknownLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEffortArgs(tt.input)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantSub, got.Sub)
			assert.Equal(t, tt.wantLevel, got.Level)
		})
	}
}

func effortTestBot(t *testing.T) (*Bot, *mockAPI) {
	t.Helper()
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"99"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})

	return b, api
}

func effortMsg(chatID int64, text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: chatID, Type: "private"},
		From: &telego.User{ID: 99},
		Text: text,
	}
}

func TestHandleEffortCommand_setPersists(t *testing.T) {
	b, api := effortTestBot(t)

	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort high"))

	st := b.store.Load()
	require.NotNil(t, st.EffortByChat)
	assert.Equal(t, "high", st.EffortByChat["42"])

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, ok := calls[0].params["text"].(string)
	require.True(t, ok)
	assert.Contains(t, text, "Effort set to")
	assert.Contains(t, text, "high")
	assert.Contains(t, text, "claude-opus-4-7")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleEffortCommand_clearRemoves(t *testing.T) {
	b, api := effortTestBot(t)

	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort medium"))
	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort clear"))

	st := b.store.Load()
	_, present := st.EffortByChat["42"]
	assert.False(t, present)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 2)
	text, ok := calls[1].params["text"].(string)
	require.True(t, ok)
	assert.Contains(t, text, "Cleared")
	assert.Equal(t, "MarkdownV2", calls[1].params["parse_mode"])
}

func TestHandleEffortCommand_showWithoutOverride(t *testing.T) {
	b, api := effortTestBot(t)

	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort"))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, ok := calls[0].params["text"].(string)
	require.True(t, ok)
	assert.Contains(t, text, "daemon defaults")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleEffortCommand_invalidLevelRejected(t *testing.T) {
	b, api := effortTestBot(t)

	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort potato"))

	st := b.store.Load()
	_, present := st.EffortByChat["42"]
	assert.False(t, present)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, ok := calls[0].params["text"].(string)
	require.True(t, ok)
	assert.Contains(t, text, "Invalid /effort syntax")
}

func TestRenderEffortLine(t *testing.T) {
	tests := []struct {
		name    string
		st      access.State
		chatID  string
		wantSub string
	}{
		{
			name:    "no override falls back to daemon default",
			st:      access.State{},
			chatID:  "42",
			wantSub: "daemon default",
		},
		{
			name:    "valid stored level renders model + thinking",
			st:      access.State{EffortByChat: map[string]string{"42": "high"}},
			chatID:  "42",
			wantSub: "claude-opus-4-7",
		},
		{
			name:    "unknown stored level surfaces clear-with hint",
			st:      access.State{EffortByChat: map[string]string{"42": "extreme"}},
			chatID:  "42",
			wantSub: "unknown level",
		},
		{
			name:    "chatID with no entry falls back to daemon default",
			st:      access.State{EffortByChat: map[string]string{"99": "low"}},
			chatID:  "42",
			wantSub: "daemon default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderEffortLine(tt.st, tt.chatID)
			assert.Contains(t, got, tt.wantSub)
		})
	}
}

func TestHandleEffortCommand_helpText(t *testing.T) {
	b, api := effortTestBot(t)

	b.handleEffortCommand(t.Context(), effortMsg(42, "/effort help"))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, ok := calls[0].params["text"].(string)
	require.True(t, ok)
	assert.Equal(t, formatEffortHelpReply(), text)
}
