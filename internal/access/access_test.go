package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestDefaultState(t *testing.T) {
	st := defaultState()
	assert.Equal(t, PolicyPairing, st.DMPolicy)
	assert.Empty(t, st.AllowFrom)
	assert.Empty(t, st.Groups)
	assert.Empty(t, st.Pending)
}

func TestNewPairingCode_format(t *testing.T) {
	c1 := NewPairingCode()
	c2 := NewPairingCode()

	assert.Len(t, c1, 6)
	assert.Len(t, c2, 6)
	assert.NotEqual(t, c1, c2, "two codes back-to-back should not collide (3-byte entropy)")
	// 6 hex chars
	for _, r := range c1 {
		assert.True(t, (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'), "non-hex char %q in %q", r, c1)
	}
}

func TestStore_load_missingFile_returnsDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)
	st := s.Load()
	assert.Equal(t, PolicyPairing, st.DMPolicy)
	assert.Empty(t, st.AllowFrom)
}

func TestStore_save_thenLoad_roundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)

	original := State{
		DMPolicy:    PolicyAllowlist,
		AllowFrom:   []string{"123", "456"},
		Groups:      map[string]GroupPolicy{"-100200": {RequireMention: true, AllowFrom: []string{"123"}}},
		Pending:     map[string]Pending{"abcdef": {SenderID: "789", ChatID: "789", CreatedAt: 1, ExpiresAt: 2, Replies: 1}},
		AckReaction: "👀",
		ReplyToMode: ReplyToFirst,
	}
	require.NoError(t, s.Save(original))

	got := s.Load()
	assert.Equal(t, original.DMPolicy, got.DMPolicy)
	assert.Equal(t, original.AllowFrom, got.AllowFrom)
	assert.Equal(t, original.Groups, got.Groups)
	assert.Equal(t, original.Pending, got.Pending)
	assert.Equal(t, original.AckReaction, got.AckReaction)
	assert.Equal(t, original.ReplyToMode, got.ReplyToMode)
}

func TestStore_save_writesAtomically_and_lockedPerms(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)
	require.NoError(t, s.Save(defaultState()))

	info, err := os.Stat(filepath.Join(dir, "access.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "access.json must be 0600")

	// No .tmp left behind after a successful save.
	_, err = os.Stat(filepath.Join(dir, "access.json.tmp"))
	assert.True(t, os.IsNotExist(err))
}

func TestStore_load_corruptJSON_quarantinesAndReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	s := NewStore(dir, false)
	got := s.Load()
	assert.Equal(t, PolicyPairing, got.DMPolicy, "corrupt file → fresh default state")

	// Original file moved aside as *.corrupt-<ts>.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var quarantined string

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "access.json.corrupt-") {
			quarantined = e.Name()
		}
	}

	assert.NotEmpty(t, quarantined, "corrupt file should have been moved aside")
}

func TestStore_static_snapshotsAtBoot(t *testing.T) {
	dir := t.TempDir()
	written := State{
		DMPolicy:  PolicyPairing,
		AllowFrom: []string{"99"},
		Groups:    map[string]GroupPolicy{},
		Pending:   map[string]Pending{"deadbe": {SenderID: "99", ExpiresAt: 99999999999}},
	}
	raw, _ := json.Marshal(written)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access.json"), raw, 0o600))

	s := NewStore(dir, true)
	got := s.Load()
	assert.Equal(t, PolicyAllowlist, got.DMPolicy, "static mode downgrades pairing to allowlist")
	assert.Empty(t, got.Pending, "static mode clears pending")

	// Save is a no-op in static.
	other := defaultState()
	other.AllowFrom = []string{"changed"}
	require.NoError(t, s.Save(other))
	assert.Equal(t, []string{"99"}, s.Load().AllowFrom, "static Load returns boot snapshot, not new Save")
}

func TestPruneExpired(t *testing.T) {
	now := time.Now().UnixMilli()
	st := State{
		Pending: map[string]Pending{
			"aaa": {ExpiresAt: now - 1000},
			"bbb": {ExpiresAt: now + 60_000},
			"ccc": {ExpiresAt: now - 10},
		},
	}
	changed := PruneExpired(&st)
	assert.True(t, changed)
	assert.Contains(t, st.Pending, "bbb")
	assert.NotContains(t, st.Pending, "aaa")
	assert.NotContains(t, st.Pending, "ccc")
}

func TestPruneExpired_noop_whenAllFresh(t *testing.T) {
	now := time.Now().UnixMilli()
	st := State{Pending: map[string]Pending{"x": {ExpiresAt: now + 100_000}}}
	assert.False(t, PruneExpired(&st))
	assert.Len(t, st.Pending, 1)
}

func TestAllowed(t *testing.T) {
	st := State{
		AllowFrom: []string{"1", "2", "3"},
		Groups:    map[string]GroupPolicy{"-100200": {}},
	}

	tests := []struct {
		name   string
		chatID string
		want   bool
	}{
		{"dm allowlisted", "2", true},
		{"dm not allowlisted", "999", false},
		{"group present", "-100200", true},
		{"group absent", "-100999", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Allowed(st, tt.chatID))
		})
	}
}

func TestStore_dirs(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)
	assert.Equal(t, filepath.Join(dir, "approved"), s.ApprovedDir())
	assert.Equal(t, filepath.Join(dir, "inbox"), s.InboxDir())
}

func TestStore_load_partialJSON_fillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"dmPolicy":"allowlist"}`), 0o600))

	s := NewStore(dir, false)
	got := s.Load()
	assert.Equal(t, PolicyAllowlist, got.DMPolicy)
	assert.NotNil(t, got.AllowFrom)
	assert.NotNil(t, got.Groups)
	assert.NotNil(t, got.Pending)
}

func TestStore_mutate_persistsWhenFnReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)

	require.NoError(t, s.Mutate(func(st *State) bool {
		st.AllowFrom = []string{"42"}
		return true
	}))

	assert.Equal(t, []string{"42"}, s.Load().AllowFrom)
}

func TestStore_mutate_skipsSaveWhenFnReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)
	require.NoError(t, s.Save(State{AllowFrom: []string{"original"}}))

	require.NoError(t, s.Mutate(func(st *State) bool {
		st.AllowFrom = []string{"discarded"}
		return false
	}))

	assert.Equal(t, []string{"original"}, s.Load().AllowFrom, "fn=false must not persist")
}

func TestStore_mutate_concurrentCallsSerializeWithoutLostUpdates(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)

	const writers = 20

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			_ = s.Mutate(func(st *State) bool {
				st.AllowFrom = append(st.AllowFrom, strconv.Itoa(idx))
				return true
			})
		}(i)
	}

	wg.Wait()

	got := s.Load().AllowFrom
	assert.Len(t, got, writers, "every Mutate should have appended exactly once")

	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}

	assert.Len(t, seen, writers, "no duplicates / no losses")
}
