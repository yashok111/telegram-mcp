package admin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// HistoryDefaults match the PR-2 spec: 100 messages OR 30 days, whichever
// trims first. Stored as JSONL under <stateDir>/admin/conversations/<chat>.jsonl
// at mode 0600 — atomic save uses tmp+rename in the same dir so the on-disk
// file never observes a partial write.
const (
	defaultRetention = 30 * 24 * time.Hour
	defaultMaxMsgs   = 100
	conversationsSub = "admin/conversations"

	// maxHistoryLine caps a single JSONL record. A streaming json.Decoder
	// cannot resync after a syntax error, so we scan line-by-line instead;
	// this bounds the per-line buffer so a corrupt giant line can't exhaust
	// memory.
	maxHistoryLine = 1 << 20
)

// chatIDPattern restricts persisted chat IDs to digits with an optional leading
// '-' (Telegram supergroup IDs are negative). The whitelist guarantees the
// resulting filename can never traverse outside the conversations dir even if
// an attacker spoofs meta["chat_id"]. Anything else returns an error before any
// FS work happens.
var chatIDPattern = regexp.MustCompile(`^-?\d+$`)

// Message is one durable history entry. JSON tags are stable wire format — do
// not rename without a migration story (existing on-disk files outlive
// daemon rebuilds).
type Message struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"ts"`
	User      string    `json:"user,omitempty"`
	MsgID     int       `json:"msg_id,omitempty"`
}

// History is the per-DM conversation memory for the admin-agent. One instance
// per daemon; method calls are safe for concurrent use across chats but
// serialize per-chat through a sync.Map of mutexes so a fast typist cannot
// interleave a fresh Append with an in-flight Load.
type History struct {
	Dir       string
	Retention time.Duration
	MaxMsgs   int

	locks sync.Map
}

// NewHistory wires defaults. Callers may override Retention/MaxMsgs before
// first use; mutating after the first call is a data race.
func NewHistory(stateDir string) *History {
	return &History{
		Dir:       filepath.Join(stateDir, conversationsSub),
		Retention: defaultRetention,
		MaxMsgs:   defaultMaxMsgs,
	}
}

// Load returns the retained subset for chatID, oldest first. Missing file is
// not an error — returns nil + nil so callers can branch on len(msgs) == 0.
func (h *History) Load(chatID string) ([]Message, error) {
	path, err := h.chatPath(chatID)
	if err != nil {
		return nil, err
	}

	lock := h.lockFor(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs := h.readFile(path)

	return h.applyRetention(msgs), nil
}

// Append writes one message; see AppendBatch.
func (h *History) Append(chatID string, msg Message) error {
	return h.AppendBatch(chatID, msg)
}

// AppendBatch writes msgs to the chat's history file in a single lock+rewrite,
// applies retention, and rewrites atomically. Batching matters for callers that
// record a request/response pair: a per-message Append could commit the user
// turn then fail on the assistant turn, leaving history ending on an unanswered
// message that poisons later context. We always rewrite (rather than O_APPEND)
// so retention is enforced every call — a 100-message cap with O_APPEND would
// need a separate sweep to stay honest. At ≤100 messages * ~1KB the rewrite cost
// is negligible at human typing speed.
func (h *History) AppendBatch(chatID string, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}

	path, err := h.chatPath(chatID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	for i := range msgs {
		if msgs[i].Timestamp.IsZero() {
			msgs[i].Timestamp = now
		}
	}

	lock := h.lockFor(chatID)
	lock.Lock()
	defer lock.Unlock()

	existing := h.readFile(path)
	existing = append(existing, msgs...)
	existing = h.applyRetention(existing)

	return h.saveAtomic(path, existing)
}

// Prune applies retention without an Append. Use from a background sweep.
// A no-op rewrite is skipped so the inode mtime stays meaningful.
func (h *History) Prune(chatID string) error {
	path, err := h.chatPath(chatID)
	if err != nil {
		return err
	}

	lock := h.lockFor(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs := h.readFile(path)
	pruned := h.applyRetention(msgs)

	if len(pruned) == len(msgs) {
		return nil
	}

	return h.saveAtomic(path, pruned)
}

// PruneAll applies retention to every stored conversation and removes files
// that age out entirely. AppendBatch only trims on write, so a chat that goes
// silent keeps its (possibly stale) messages until the next message — this
// sweep enforces the time cutoff for idle chats and bounds disk by deleting
// emptied files. Per-chat errors are logged, not fatal. Intended for a periodic
// background sweep (the admin agent runs it on a ticker).
func (h *History) PruneAll() error {
	chats, err := h.ListChats()
	if err != nil {
		return err
	}

	for _, chatID := range chats {
		if err := h.pruneOrRemove(chatID); err != nil {
			slog.Warn("admin history prune failed", "chat_id", chatID, "err", err)
		}
	}

	return nil
}

// pruneOrRemove trims one chat to the retention window, deleting the file when
// nothing survives. The lock is held across the read/decision/write so it can't
// race a concurrent AppendBatch. The lock entry itself is intentionally NOT
// evicted from h.locks — doing so could hand two goroutines different mutexes
// for the same chat; the map is bounded by distinct chat count (≈ the owner on
// a single-user daemon), so the residual is negligible.
func (h *History) pruneOrRemove(chatID string) error {
	path, err := h.chatPath(chatID)
	if err != nil {
		return err
	}

	lock := h.lockFor(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs := h.applyRetention(h.readFile(path))
	if len(msgs) == 0 {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("remove empty history: %w", rmErr)
		}

		return nil
	}

	return h.saveAtomic(path, msgs)
}

// ListChats returns the chat IDs that currently have a stored history file,
// sorted lexicographically. Missing directory returns nil + nil.
func (h *History) ListChats() ([]string, error) {
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read conversations dir: %w", err)
	}

	out := make([]string, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		out = append(out, strings.TrimSuffix(name, ".jsonl"))
	}

	sort.Strings(out)

	return out, nil
}

func (h *History) chatPath(chatID string) (string, error) {
	if !chatIDPattern.MatchString(chatID) {
		return "", fmt.Errorf("invalid chat_id %q", chatID)
	}

	return filepath.Join(h.Dir, chatID+".jsonl"), nil
}

func (h *History) lockFor(chatID string) *sync.Mutex {
	v, _ := h.locks.LoadOrStore(chatID, &sync.Mutex{})
	m, _ := v.(*sync.Mutex)

	return m
}

func (h *History) readFile(path string) []Message {
	f, err := os.Open(path) //nolint:gosec // path constructed from chatIDPattern allowlist
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var msgs []Message

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxHistoryLine)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Warn("admin history skipping malformed entry", "path", path, "err", err)
			continue
		}

		msgs = append(msgs, m)
	}

	if err := sc.Err(); err != nil {
		// bufio.ErrTooLong (a line over maxHistoryLine) stops the scan, so any
		// entries after the oversized line are dropped. Surface that as data
		// loss, not a vague parse warning.
		slog.Warn("admin history read stopped early; later entries dropped", "path", path, "err", err)
	}

	return msgs
}

func (h *History) applyRetention(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}

	if h.Retention > 0 {
		cutoff := time.Now().Add(-h.Retention)

		i := 0
		for i < len(msgs) && msgs[i].Timestamp.Before(cutoff) {
			i++
		}

		msgs = msgs[i:]
	}

	if h.MaxMsgs > 0 && len(msgs) > h.MaxMsgs {
		msgs = msgs[len(msgs)-h.MaxMsgs:]
	}

	return msgs
}

func (h *History) saveAtomic(path string, msgs []Message) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir history dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".jsonl.tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	tmpName := tmp.Name()

	cleanup := tmpName
	defer func() {
		if cleanup != "" {
			_ = os.Remove(cleanup)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tmp: %w", err)
	}

	enc := json.NewEncoder(tmp)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("encode entry: %w", err)
		}
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename tmp: %w", err)
	}

	cleanup = ""

	return nil
}
