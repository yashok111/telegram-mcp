package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadRecentEvents(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	makeEvents := func(n int) []Event {
		out := make([]Event, n)
		for i := range out {
			out[i] = Event{
				Type:     "test",
				Severity: "info",
				TS:       now.Add(time.Duration(i) * time.Second),
				Subject:  "subj",
				Detail:   "detail",
			}
		}

		return out
	}

	writeJSONL := func(t *testing.T, path string, events []Event, extraLines ...string) {
		t.Helper()

		f, err := os.Create(path)
		require.NoError(t, err)

		defer func() { require.NoError(t, f.Close()) }()

		enc := json.NewEncoder(f)
		for _, e := range events {
			require.NoError(t, enc.Encode(e))
		}

		for _, line := range extraLines {
			_, err = f.WriteString(line + "\n")
			require.NoError(t, err)
		}
	}

	t.Run("missing file returns nil", func(t *testing.T) {
		dir := t.TempDir()
		got, err := ReadRecentEvents(dir, 10)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("all events returned oldest first when n=0", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		evs := makeEvents(3)
		writeJSONL(t, filepath.Join(dir, "admin", "events.jsonl"), evs)

		got, err := ReadRecentEvents(dir, 0)
		require.NoError(t, err)
		require.Len(t, got, 3)

		for i, e := range got {
			assert.Equal(t, evs[i].TS, e.TS, "index %d", i)
		}
	})

	t.Run("malformed lines skipped", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		evs := makeEvents(2)
		writeJSONL(t, filepath.Join(dir, "admin", "events.jsonl"), evs, "not json at all")

		got, err := ReadRecentEvents(dir, 0)
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("n cap trims to last n events", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		evs := makeEvents(5)
		writeJSONL(t, filepath.Join(dir, "admin", "events.jsonl"), evs)

		got, err := ReadRecentEvents(dir, 3)
		require.NoError(t, err)
		require.Len(t, got, 3)

		// Should be the last 3 (oldest-first within that tail)
		assert.Equal(t, evs[2].TS, got[0].TS)
		assert.Equal(t, evs[3].TS, got[1].TS)
		assert.Equal(t, evs[4].TS, got[2].TS)
	})

	t.Run("n greater than total returns all", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		evs := makeEvents(2)
		writeJSONL(t, filepath.Join(dir, "admin", "events.jsonl"), evs)

		got, err := ReadRecentEvents(dir, 100)
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("empty file returns nil", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		f, err := os.Create(filepath.Join(dir, "admin", "events.jsonl"))
		require.NoError(t, err)
		require.NoError(t, f.Close())

		got, err := ReadRecentEvents(dir, 0)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}
