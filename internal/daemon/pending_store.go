package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// pendingIDPattern constrains a pending_id to hex (the charset blocks path
// traversal like "../../etc/passwd") with a {1,64} length bound (rejects
// oversized input). Take receives the id from callback data over the wire, so
// it is validated here before any filesystem touch.
var pendingIDPattern = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// maxPendingFileBytes caps a single pending file read. Records are well under
// a kilobyte (tool + small args + summary); 64 KiB is generous headroom while
// still bounding allocation if a corrupt/hostile file lands in the dir.
const maxPendingFileBytes = 64 << 10

// PendingMutation is one Tier-3 mutation awaiting the owner's ✅/❌ tap.
// Persisted to <stateDir>/admin/pending/<id>.json (one file per mutation) so a
// daemon restart between rendering the confirm and the tap still resolves by
// pending_id — the callback data carries the id, PendingStore.Take re-reads it.
type PendingMutation struct {
	ID               string          `json:"id"`
	Tool             string          `json:"tool"`
	Args             json.RawMessage `json:"args,omitempty"`
	Summary          string          `json:"summary"`
	ConfirmChatID    int64           `json:"confirm_chat_id"`
	ConfirmMessageID int             `json:"confirm_message_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

// PendingStore persists Tier-3 mutations awaiting owner confirmation. One file
// per pending under <stateDir>/admin/pending. A single mutex serialises all
// Put/Take/Sweep so a concurrent tap + sweep can't double-resolve a mutation.
type PendingStore struct {
	dir string
	mu  sync.Mutex
}

// NewPendingStore returns a store rooted at <stateDir>/admin/pending.
func NewPendingStore(stateDir string) *PendingStore {
	return &PendingStore{dir: filepath.Join(stateDir, "admin", "pending")}
}

func (s *PendingStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Put atomically writes p to <id>.json (tmp+rename, 0600). CreatedAt is filled
// with time.Now().UTC() when zero.
func (s *PendingStore) Put(p PendingMutation) error {
	if !pendingIDPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid pending id %q", p.ID)
	}

	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}

	buf, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal pending: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir pending dir: %w", err)
	}

	tmp := s.path(p.ID) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("write pending: %w", err)
	}

	if err := os.Rename(tmp, s.path(p.ID)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename pending: %w", err)
	}

	return nil
}

// SetConfirmMessageID patches the rendered confirm message id onto an existing
// pending record. It is a no-op (nil) when the record is already gone — taken
// by an owner tap or swept — so it never recreates a consumed mutation. Without
// this guard, a Put after the owner taps (during the confirm render) would
// resurrect a ghost record that a second tap could double-apply.
func (s *PendingStore) SetConfirmMessageID(id string, msgID int) error {
	if !pendingIDPattern.MatchString(id) {
		return fmt.Errorf("invalid pending id %q", id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.readLocked(s.path(id))
	if !ok {
		return nil // already taken/swept — do not recreate
	}

	p.ConfirmMessageID = msgID

	buf, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal pending: %w", err)
	}

	tmp := s.path(id) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("write pending: %w", err)
	}

	if err := os.Rename(tmp, s.path(id)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename pending: %w", err)
	}

	return nil
}

// Take reads and removes the pending mutation for id. ok=false when the id is
// malformed, the file is missing, or it fails to parse. Removal happens before
// the return so a Tier-3 mutation can be resolved at most once.
func (s *PendingStore) Take(id string) (PendingMutation, bool) {
	if !pendingIDPattern.MatchString(id) {
		return PendingMutation{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.readLocked(s.path(id))
	if !ok {
		return PendingMutation{}, false
	}

	// Remove BEFORE returning ok so a mutation resolves at most once. If the
	// remove fails the file survives a restart and could re-resolve, so refuse
	// to hand the record out rather than risk a double-apply.
	if err := os.Remove(s.path(id)); err != nil {
		slog.Warn("pending take: remove failed, not resolving to avoid double-apply", "id", id, "err", err)
		return PendingMutation{}, false
	}

	return p, true
}

// Sweep removes every pending whose age exceeds ttl and returns the removed
// records (so the caller can audit + edit the stale confirm message). A ttl
// <= 0 is treated as "never expire" (returns nothing). Missing dir → nil.
func (s *PendingStore) Sweep(ttl time.Duration) []PendingMutation {
	if ttl <= 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		// A missing dir is the normal "nothing pending yet" case; anything else
		// (e.g. permissions) is worth surfacing rather than silently skipping.
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("pending sweep readdir failed", "dir", s.dir, "err", err)
		}

		return nil
	}

	cutoff := time.Now().Add(-ttl)

	var (
		expired []PendingMutation
		names   = make([]string, 0, len(entries))
	)

	for _, e := range entries {
		name := e.Name()

		switch {
		case e.IsDir():
			continue
		case strings.HasSuffix(name, ".tmp"):
			// Orphaned tmp from a Put that crashed between WriteFile and Rename.
			// No CreatedAt to read, so age it by mtime.
			if info, ierr := e.Info(); ierr == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
		case filepath.Ext(name) == ".json":
			names = append(names, name)
		}
	}

	sort.Strings(names) // deterministic order for tests + logs

	for _, name := range names {
		full := filepath.Join(s.dir, name)

		p, ok := s.readLocked(full)
		if !ok {
			// Corrupt/unreadable record can never be resolved; drop it so it
			// doesn't accumulate forever.
			slog.Warn("pending sweep removing unreadable record", "file", full)
			_ = os.Remove(full)

			continue
		}

		if p.CreatedAt.Before(cutoff) {
			expired = append(expired, p)
			_ = os.Remove(full)
		}
	}

	return expired
}

// readLocked reads and unmarshals one pending file (bounded). Caller holds
// s.mu.
func (s *PendingStore) readLocked(path string) (PendingMutation, bool) {
	f, err := os.Open(path) //nolint:gosec // path is s.dir + a validated id / own-listed name
	if err != nil {
		return PendingMutation{}, false
	}
	defer func() { _ = f.Close() }()

	buf, err := io.ReadAll(io.LimitReader(f, maxPendingFileBytes))
	if err != nil {
		return PendingMutation{}, false
	}

	var p PendingMutation
	if err := json.Unmarshal(buf, &p); err != nil {
		return PendingMutation{}, false
	}

	return p, true
}
