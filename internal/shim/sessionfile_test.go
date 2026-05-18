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
		CCPID:        9001,
		ShimPID:      9002,
		Workdir:      "/path/here",
		Label:        "X",
		Mode:         "shim",
	}

	path, err := writeSessionFile(dir, info)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "sessions", "9001.json"), path)

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	var got SessionInfo

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s2", got.Alias)
	assert.Equal(t, "abcdef012345", got.ShimID)
	assert.Equal(t, "abcdef01", got.ShimIDPrefix)
	assert.Equal(t, 9001, got.CCPID)
	assert.Equal(t, 9002, got.ShimPID)
	assert.NotZero(t, got.StartedAt)
}

func TestWriteSessionFile_optionalCCSessionIDRoundtrips(t *testing.T) {
	dir := t.TempDir()
	info := SessionInfo{
		Alias:       "s1",
		ShimID:      "id",
		CCPID:       1234,
		CCSessionID: "opt-sid",
		Mode:        "shim",
	}

	_, err := writeSessionFile(dir, info)
	require.NoError(t, err)

	var got SessionInfo

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", "1234.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "opt-sid", got.CCSessionID)
}

func TestWriteSessionFile_zeroCCPID_returnsErr(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSessionFile(dir, SessionInfo{Alias: "s1"})
	assert.Error(t, err)
}

func TestWriteSessionFile_negativeCCPID_returnsErr(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSessionFile(dir, SessionInfo{Alias: "s1", CCPID: -1})
	assert.Error(t, err)
}

func TestRemoveSessionFile_idempotent(t *testing.T) {
	dir := t.TempDir()
	info := SessionInfo{Alias: "s1", ShimID: "id", CCPID: 4242, Mode: "shim"}
	path, err := writeSessionFile(dir, info)
	require.NoError(t, err)

	require.NoError(t, removeSessionFile(dir, 4242))

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))

	// Second removal must not error.
	assert.NoError(t, removeSessionFile(dir, 4242))
}

func TestRemoveSessionFile_zeroIsNoop(t *testing.T) {
	dir := t.TempDir()
	assert.NoError(t, removeSessionFile(dir, 0))
}

func TestWriteSessionFile_replacesPrevious(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSessionFile(dir, SessionInfo{Alias: "s1", ShimID: "old", CCPID: 7777, Mode: "shim"})
	require.NoError(t, err)
	_, err = writeSessionFile(dir, SessionInfo{Alias: "s2", ShimID: "new", CCPID: 7777, Mode: "shim"})
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", "7777.json"))
	require.NoError(t, err)

	var got SessionInfo
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s2", got.Alias)
	assert.Equal(t, "new", got.ShimID)
}
