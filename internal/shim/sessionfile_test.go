package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteSessionFile_createsAtomicallyWith0600(t *testing.T) {
	dir := t.TempDir()
	info := SessionInfo{
		Alias:        "s2",
		ShimID:       "abcdef012345",
		ShimIDPrefix: "abcdef01",
		CCSessionID:  "sess-xyz",
		Workdir:      "/path/here",
		Label:        "X",
		Mode:         "shim",
	}

	path, err := writeSessionFile(dir, info)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "sessions", "sess-xyz.json"), path)

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	var got SessionInfo
	raw, _ := os.ReadFile(path)
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s2", got.Alias)
	assert.Equal(t, "abcdef012345", got.ShimID)
	assert.Equal(t, "abcdef01", got.ShimIDPrefix)
	assert.Equal(t, "sess-xyz", got.CCSessionID)
	assert.NotZero(t, got.StartedAt)
}

func TestWriteSessionFile_emptyCCSessionID_returnsErr(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSessionFile(dir, SessionInfo{Alias: "s1"})
	assert.Error(t, err)
}

func TestRemoveSessionFile_idempotent(t *testing.T) {
	dir := t.TempDir()
	info := SessionInfo{Alias: "s1", ShimID: "id", CCSessionID: "ccsid", Mode: "shim"}
	path, err := writeSessionFile(dir, info)
	require.NoError(t, err)

	require.NoError(t, removeSessionFile(dir, "ccsid"))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))

	// Second removal must not error.
	assert.NoError(t, removeSessionFile(dir, "ccsid"))
}

func TestWriteSessionFile_replacesPrevious(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSessionFile(dir, SessionInfo{Alias: "s1", ShimID: "old", CCSessionID: "ccsid", Mode: "shim"})
	require.NoError(t, err)
	_, err = writeSessionFile(dir, SessionInfo{Alias: "s2", ShimID: "new", CCSessionID: "ccsid", Mode: "shim"})
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", "ccsid.json"))
	require.NoError(t, err)
	var got SessionInfo
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s2", got.Alias)
	assert.Equal(t, "new", got.ShimID)
}
