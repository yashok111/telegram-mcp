package chunk

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestSplit_shortText_returnsSingleChunk(t *testing.T) {
	out := Split("hello", 100, Length)
	assert.Equal(t, []string{"hello"}, out)
}

func TestSplit_emptyText_returnsSingleEmptyChunk(t *testing.T) {
	out := Split("", 100, Length)
	assert.Equal(t, []string{""}, out)
}

func TestSplit_lengthMode_hardCut(t *testing.T) {
	in := strings.Repeat("a", 250)
	out := Split(in, 100, Length)
	require.Len(t, out, 3)
	assert.Len(t, out[0], 100)
	assert.Len(t, out[1], 100)
	assert.Len(t, out[2], 50)
}

func TestSplit_newlineMode_prefersParagraph(t *testing.T) {
	// limit 50, paragraph break at index 30 (past halfway).
	in := strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 60)
	out := Split(in, 50, Newline)
	require.GreaterOrEqual(t, len(out), 2)
	assert.Equal(t, strings.Repeat("a", 30), out[0],
		"first chunk should end at the paragraph boundary")
}

func TestSplit_newlineMode_fallsBackToLine(t *testing.T) {
	in := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 60)
	out := Split(in, 50, Newline)
	require.GreaterOrEqual(t, len(out), 2)
	assert.Equal(t, strings.Repeat("a", 30), out[0],
		"first chunk should end at the single-newline boundary")
}

func TestSplit_newlineMode_fallsBackToSpace(t *testing.T) {
	in := strings.Repeat("a", 30) + " " + strings.Repeat("b", 60)
	out := Split(in, 50, Newline)
	require.GreaterOrEqual(t, len(out), 2)
	assert.Equal(t, strings.Repeat("a", 30), out[0],
		"first chunk should end at the space boundary")
}

func TestSplit_newlineMode_hardCutWhenBoundaryBeforeHalfway(t *testing.T) {
	// boundary at index 5 (well below halfway = 25) → hard cut at limit.
	in := strings.Repeat("a", 5) + "\n\n" + strings.Repeat("b", 100)
	out := Split(in, 50, Newline)
	require.GreaterOrEqual(t, len(out), 2)
	// First chunk falls back to hard cut at 50, NOT at boundary 5.
	assert.Len(t, out[0], 50)
}

func TestSplit_invalidLimit_clampsToMax(t *testing.T) {
	tests := []struct {
		name  string
		limit int
	}{
		{"zero", 0},
		{"negative", -10},
		{"over_max", MaxChunkLimit + 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Repeat("a", MaxChunkLimit)
			out := Split(in, tt.limit, Length)
			// Effectively MaxChunkLimit applied — single chunk for input == limit.
			assert.Len(t, out, 1)
		})
	}
}

func TestSplit_stripsLeadingNewlinesAfterCut(t *testing.T) {
	// After Length-mode cut at 10, the remaining starts with "\n\n..." — should be trimmed.
	in := strings.Repeat("a", 10) + "\n\nbbbb"
	out := Split(in, 10, Length)
	require.Len(t, out, 2)
	assert.Equal(t, "bbbb", out[1])
}
