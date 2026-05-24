package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryLoadMissingReturnsNil(t *testing.T) {
	h := NewHistory(t.TempDir())

	msgs, err := h.Load("42")
	require.NoError(t, err)
	assert.Nil(t, msgs)
}

func TestHistoryAppendThenLoadRoundtrip(t *testing.T) {
	h := NewHistory(t.TempDir())

	now := time.Now().UTC()
	require.NoError(t, h.Append("42", Message{Role: "user", Content: "hi", Timestamp: now}))
	require.NoError(t, h.Append("42", Message{Role: "assistant", Content: "hello", Timestamp: now.Add(time.Second)}))

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hi", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hello", msgs[1].Content)
}

func TestHistoryFilePermissionsAreRestrictive(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)

	require.NoError(t, h.Append("42", Message{Role: "user", Content: "x"}))

	st, err := os.Stat(filepath.Join(dir, "admin", "conversations", "42.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	pdir, err := os.Stat(filepath.Join(dir, "admin", "conversations"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), pdir.Mode().Perm())
}

func TestHistoryAppendDefaultsTimestamp(t *testing.T) {
	h := NewHistory(t.TempDir())

	before := time.Now().UTC().Add(-time.Second)

	require.NoError(t, h.Append("42", Message{Role: "user", Content: "x"}))

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.False(t, msgs[0].Timestamp.Before(before), "timestamp must be auto-filled")
}

func TestHistoryMaxMsgsEvictsOldest(t *testing.T) {
	h := NewHistory(t.TempDir())
	h.MaxMsgs = 3

	now := time.Now().UTC()
	for i := range 5 {
		require.NoError(t, h.Append("42", Message{
			Role:      "user",
			Content:   "msg" + string(rune('a'+i)),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		}))
	}

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "msgc", msgs[0].Content)
	assert.Equal(t, "msge", msgs[2].Content)
}

func TestHistoryRetentionDropsOldEntries(t *testing.T) {
	h := NewHistory(t.TempDir())
	h.Retention = time.Minute

	now := time.Now().UTC()
	require.NoError(t, h.Append("42", Message{Role: "user", Content: "old", Timestamp: now.Add(-10 * time.Minute)}))
	require.NoError(t, h.Append("42", Message{Role: "user", Content: "fresh", Timestamp: now}))

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "fresh", msgs[0].Content)
}

func TestHistoryPruneRewritesFile(t *testing.T) {
	h := NewHistory(t.TempDir())
	h.Retention = time.Minute

	stale := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, h.Append("42", Message{Role: "user", Content: "old", Timestamp: stale}))
	require.NoError(t, h.Append("42", Message{Role: "assistant", Content: "old-reply", Timestamp: stale.Add(time.Second)}))

	require.NoError(t, h.Prune("42"))

	path := filepath.Join(h.Dir, "42.jsonl")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 0, strings.Count(string(data), "\n"), "file should be empty after prune")
}

func TestHistoryRejectsTraversalChatID(t *testing.T) {
	h := NewHistory(t.TempDir())

	cases := []string{"../etc/passwd", "foo", "42/43", ".", "", "42.0", "-"}

	for _, c := range cases {
		_, err := h.Load(c)
		require.Error(t, err, "Load(%q) must error", c)

		err = h.Append(c, Message{Role: "user", Content: "x"})
		require.Error(t, err, "Append(%q) must error", c)
	}
}

func TestHistoryNegativeChatIDAccepted(t *testing.T) {
	h := NewHistory(t.TempDir())

	require.NoError(t, h.Append("-1003914957143", Message{Role: "user", Content: "supergroup"}))

	msgs, err := h.Load("-1003914957143")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
}

func TestHistoryListChats(t *testing.T) {
	h := NewHistory(t.TempDir())

	chats, err := h.ListChats()
	require.NoError(t, err)
	assert.Empty(t, chats)

	require.NoError(t, h.Append("42", Message{Role: "user", Content: "a"}))
	require.NoError(t, h.Append("-99", Message{Role: "user", Content: "b"}))

	chats, err = h.ListChats()
	require.NoError(t, err)
	assert.Equal(t, []string{"-99", "42"}, chats)
}

func TestHistoryConcurrentAppendsSerializePerChat(t *testing.T) {
	h := NewHistory(t.TempDir())

	const n = 50

	done := make(chan struct{}, n)

	for i := range n {
		go func(i int) {
			_ = h.Append("42", Message{
				Role:      "user",
				Content:   "msg",
				Timestamp: time.Now().UTC().Add(time.Duration(i) * time.Microsecond),
				MsgID:     i,
			})

			done <- struct{}{}
		}(i)
	}

	for range n {
		<-done
	}

	msgs, err := h.Load("42")
	require.NoError(t, err)
	assert.Len(t, msgs, n, "no appends should be lost under concurrent writes")
}

func TestHistorySkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	h.Retention = 0 // isolate parse-skip behavior from time-based pruning

	path := filepath.Join(h.Dir, "42.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{not valid json}\n{\"role\":\"user\",\"content\":\"ok\",\"ts\":\"2026-01-01T00:00:00Z\"}\n"), 0o600))

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "ok", msgs[0].Content)
}

// TestHistorySkipsMalformedMiddleLine guards against the streaming-decoder
// resync bug: a json.Decoder cannot recover after a syntax error, so a malformed
// entry between two valid ones must not stall the read or swallow the tail.
// readFile must skip exactly the bad line and still return both valid entries.
func TestHistorySkipsMalformedMiddleLine(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	h.Retention = 0 // isolate parse-skip behavior from time-based pruning

	path := filepath.Join(h.Dir, "42.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","content":"first","ts":"2026-01-01T00:00:00Z"}`+"\n"+
			"{garbage not json}\n"+
			`{"role":"assistant","content":"third","ts":"2026-01-01T00:00:01Z"}`+"\n",
	), 0o600))

	msgs, err := h.Load("42")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "first", msgs[0].Content)
	assert.Equal(t, "third", msgs[1].Content)
}

func TestPruneAllRemovesAgedOutFiles(t *testing.T) {
	h := NewHistory(t.TempDir())
	h.Retention = time.Hour

	// Fresh chat survives; a chat whose only message predates the window ages out
	// to an empty file that PruneAll must delete.
	require.NoError(t, h.AppendBatch("111", Message{Role: "user", Content: "hi", Timestamp: time.Now()}))
	require.NoError(t, h.AppendBatch("222", Message{Role: "user", Content: "old", Timestamp: time.Now().Add(-2 * time.Hour)}))

	require.NoError(t, h.PruneAll())

	chats, err := h.ListChats()
	require.NoError(t, err)
	assert.Contains(t, chats, "111", "fresh chat survives")
	assert.NotContains(t, chats, "222", "fully aged-out chat file is removed")
}
