package daemon

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamReader_Init(t *testing.T) {
	line := `{"type":"system","subtype":"init","cwd":"/x","session_id":"s","model":"opus"}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventInit, ev.Kind)
	assert.Equal(t, "s", ev.SessionID)

	_, err = r.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamReader_InitWithoutSessionID(t *testing.T) {
	line := `{"type":"system","subtype":"init","cwd":"/x"}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventInit, ev.Kind)
	assert.Empty(t, ev.SessionID)
}

func TestStreamReader_AssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventAssistantText, ev.Kind)
	assert.Equal(t, "hi", ev.Text)
}

func TestStreamReader_ToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventToolUse, ev.Kind)
	assert.Equal(t, "Bash", ev.Tool)
}

func TestStreamReader_AssistantMultiContent(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"step 1"},{"type":"tool_use","name":"Read","input":{}}]}}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev1, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventAssistantText, ev1.Kind)
	assert.Equal(t, "step 1", ev1.Text)

	ev2, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventToolUse, ev2.Kind)
	assert.Equal(t, "Read", ev2.Tool)

	_, err = r.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamReader_Result(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":2383,"num_turns":1,"result":"hi","total_cost_usd":0.15}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventResult, ev.Kind)
	assert.True(t, ev.OK)
	assert.False(t, ev.IsError)
	assert.Equal(t, "hi", ev.ResultText)
	assert.Equal(t, 1, ev.NumTurns)
	assert.Equal(t, 2383, ev.DurationMs)
	assert.InDelta(t, 0.15, ev.CostUSD, 1e-9)
}

func TestStreamReader_ResultError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"duration_ms":50,"num_turns":2,"result":"boom","total_cost_usd":0.01}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventResult, ev.Kind)
	assert.False(t, ev.OK)
	assert.True(t, ev.IsError)
	assert.Equal(t, "boom", ev.ResultText)
}

func TestStreamReader_MalformedLineIsOther(t *testing.T) {
	r := NewStreamReader(strings.NewReader("not json\n" + `{"type":"system","subtype":"init"}` + "\n"))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventOther, ev.Kind)

	ev, err = r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventInit, ev.Kind)
}

func TestStreamReader_IgnoredTypes(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"rate_limit_event"}`,
		`{"type":"unknown"}`,
	}
	r := NewStreamReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))

	for range lines {
		ev, err := r.Next()
		require.NoError(t, err)
		assert.Equal(t, StreamEventOther, ev.Kind)
	}

	_, err := r.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamReader_BlankLinesSkipped(t *testing.T) {
	input := "\n\n" + `{"type":"system","subtype":"init"}` + "\n\n"
	r := NewStreamReader(strings.NewReader(input))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventInit, ev.Kind)

	_, err = r.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamReader_EmptyReader(t *testing.T) {
	r := NewStreamReader(strings.NewReader(""))

	_, err := r.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamReader_AssistantEmptyContent(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[]}}` + "\n"
	r := NewStreamReader(strings.NewReader(line))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, StreamEventOther, ev.Kind)
}
