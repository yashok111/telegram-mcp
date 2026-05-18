package bot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBgArgs_EmptyIsHelp(t *testing.T) {
	a, err := parseBgArgs("")
	require.NoError(t, err)
	assert.Equal(t, BgSubHelp, a.Sub)
}

func TestParseBgArgs_HelpKeyword(t *testing.T) {
	a, err := parseBgArgs("help")
	require.NoError(t, err)
	assert.Equal(t, BgSubHelp, a.Sub)
}

func TestParseBgArgs_List(t *testing.T) {
	a, err := parseBgArgs("list")
	require.NoError(t, err)
	assert.Equal(t, BgSubList, a.Sub)
}

func TestParseBgArgs_Cancel(t *testing.T) {
	a, err := parseBgArgs("cancel a3f2c1")
	require.NoError(t, err)
	assert.Equal(t, BgSubCancel, a.Sub)
	assert.Equal(t, "a3f2c1", a.TaskID)
}

func TestParseBgArgs_CancelNeedsID(t *testing.T) {
	_, err := parseBgArgs("cancel")
	assert.ErrorIs(t, err, ErrBgCancelNeedsID)
}

func TestParseBgArgs_StartNoFlag(t *testing.T) {
	a, err := parseBgArgs("refactor auth module")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth module", a.Prompt)
	assert.Empty(t, a.Workdir)
}

func TestParseBgArgs_StartWithInTrailing(t *testing.T) {
	a, err := parseBgArgs("refactor auth --in /repo")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth", a.Prompt)
	assert.Equal(t, "/repo", a.Workdir)
}

func TestParseBgArgs_StartWithInLeading(t *testing.T) {
	a, err := parseBgArgs("--in /repo refactor auth")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth", a.Prompt)
	assert.Equal(t, "/repo", a.Workdir)
}

func TestParseBgArgs_InMissingValue(t *testing.T) {
	_, err := parseBgArgs("refactor --in")
	assert.ErrorIs(t, err, ErrBgFlagInRequiresValue)
}

func TestParseBgArgs_OnlyFlag(t *testing.T) {
	_, err := parseBgArgs("--in /repo")
	assert.ErrorIs(t, err, ErrBgEmptyPrompt)
}

func TestParseBgArgs_SubKeywordCaseInsensitive(t *testing.T) {
	for _, sub := range []string{"LIST", "List", "list"} {
		a, err := parseBgArgs(sub)
		require.NoError(t, err)
		assert.Equal(t, BgSubList, a.Sub)
	}
}
