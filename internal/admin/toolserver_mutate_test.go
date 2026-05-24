package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureMutate swaps ts.Mutate for a recorder returning the supplied result.
func captureMutate(ts *ToolServer, res MutateResult, err error) *struct {
	tool string
	args map[string]any
} {
	got := &struct {
		tool string
		args map[string]any
	}{}

	ts.Mutate = func(_ context.Context, tool string, args map[string]any) (MutateResult, error) {
		got.tool = tool
		got.args = args

		return res, err
	}

	return got
}

func TestMutateToolTier2AppliedRender(t *testing.T) {
	ts := newTestToolServer(t)
	got := captureMutate(ts, MutateResult{Tool: "label_session", Tier: 2, Applied: true, Result: "labelled @s2 as build"}, nil)

	res, err := ts.handleLabelSession(context.Background(), toolReq(map[string]any{"target": "s2", "label": "build"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	txt := resultText(res)
	assert.Contains(t, txt, "applied (tier 2)")
	assert.Contains(t, txt, "labelled @s2 as build")

	assert.Equal(t, "label_session", got.tool)
	assert.Equal(t, "s2", got.args["target"])
	assert.Equal(t, "build", got.args["label"])
}

func TestMutateToolTier3PendingRender(t *testing.T) {
	ts := newTestToolServer(t)
	got := captureMutate(ts, MutateResult{Tool: "evict_session", Tier: 3, Pending: true, PendingID: "ab12cd34", Result: "evict @s2"}, nil)

	res, err := ts.handleEvictSession(context.Background(), toolReq(map[string]any{"target": "s2"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	txt := resultText(res)
	assert.Contains(t, txt, "PROPOSED")
	assert.Contains(t, txt, "ab12cd34")
	assert.Contains(t, txt, "NOT take effect")
	assert.Equal(t, "evict_session", got.tool)
}

func TestMutateToolErrorRender(t *testing.T) {
	ts := newTestToolServer(t)
	captureMutate(ts, MutateResult{}, errors.New("unknown mutate tool"))

	res, err := ts.handleAddAllow(context.Background(), toolReq(map[string]any{"chat_id": "123"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(res), "rejected")
}

func TestMutateToolTTLParsedAsInt(t *testing.T) {
	ts := newTestToolServer(t)
	got := captureMutate(ts, MutateResult{Applied: true, Result: "pinned"}, nil)

	_, err := ts.handlePinChat(context.Background(), toolReq(map[string]any{"chat_id": "42", "target": "s2", "ttl_seconds": "3600"}))
	require.NoError(t, err)

	// Must be an int, not a string — the daemon decodes ttl_seconds as int.
	assert.Equal(t, 3600, got.args["ttl_seconds"])
}

func TestMutateToolTTLOmittedWhenEmpty(t *testing.T) {
	ts := newTestToolServer(t)
	got := captureMutate(ts, MutateResult{Applied: true, Result: "pinned"}, nil)

	_, err := ts.handlePinChat(context.Background(), toolReq(map[string]any{"chat_id": "42", "target": "s2"}))
	require.NoError(t, err)
	assert.NotContains(t, got.args, "ttl_seconds")
}

func TestMutateToolAddRuleArgs(t *testing.T) {
	ts := newTestToolServer(t)
	got := captureMutate(ts, MutateResult{Pending: true, PendingID: "ff00", Result: "rule"}, nil)

	_, err := ts.handleAddRule(context.Background(), toolReq(map[string]any{"tool": "Bash", "action": "approve", "path_pattern": "**", "ttl_seconds": "3600"}))
	require.NoError(t, err)
	assert.Equal(t, "Bash", got.args["tool"])
	assert.Equal(t, "approve", got.args["action"])
	assert.Equal(t, "**", got.args["path_pattern"])
	assert.Equal(t, 3600, got.args["ttl_seconds"])
}
