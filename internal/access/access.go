// Package access manages the access.json state file: allowlist, pairing,
// group policy, UX prefs. JSON shape matches the TS plugin 1:1 so an
// existing ~/.claude/channels/telegram/access.json migrates without re-pairing.
package access

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type DMPolicy string

const (
	PolicyPairing   DMPolicy = "pairing"
	PolicyAllowlist DMPolicy = "allowlist"
	PolicyDisabled  DMPolicy = "disabled"
)

type ChunkMode string

const (
	ChunkLength  ChunkMode = "length"
	ChunkNewline ChunkMode = "newline"
)

type ReplyToMode string

const (
	ReplyToOff   ReplyToMode = "off"
	ReplyToFirst ReplyToMode = "first"
	ReplyToAll   ReplyToMode = "all"
)

type Pending struct {
	SenderID  string `json:"senderId"`
	ChatID    string `json:"chatId"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Replies   int    `json:"replies"`
}

type GroupPolicy struct {
	RequireMention bool     `json:"requireMention"`
	AllowFrom      []string `json:"allowFrom"`
}

// TopicMeta carries per-topic metadata: which shim (if any) currently has
// it locked, plus the reuse-key components for /topics list rendering.
// ThreadID is duplicated as the map key so callers iterating the map have
// the full struct without an extra lookup.
type TopicMeta struct {
	ThreadID int    `json:"thread_id"`
	Workdir  string `json:"workdir,omitempty"`
	Label    string `json:"label,omitempty"`
	// Name is the last topic title pushed to Telegram. Stored so reuse by a
	// shim with a different alias can detect divergence and re-push via
	// EditForumTopic — the title is otherwise frozen at CreateForumTopic.
	Name       string `json:"name,omitempty"`
	LastShimID string `json:"last_shim_id,omitempty"`
	// LockedBy is the currently-attached shim_id; "" means the topic is
	// available for reuse on the next hello with a matching reuse_key.
	LockedBy string `json:"locked_by,omitempty"`
}

// ClosedTopic tracks topics scheduled for deletion. Background sweep removes
// the underlying Telegram topic + map entries when ClosedAt + purge TTL has
// elapsed.
type ClosedTopic struct {
	ThreadID int   `json:"thread_id"`
	ClosedAt int64 `json:"closed_at"`
}

type State struct {
	DMPolicy        DMPolicy               `json:"dmPolicy"`
	AllowFrom       []string               `json:"allowFrom"`
	Groups          map[string]GroupPolicy `json:"groups"`
	Pending         map[string]Pending     `json:"pending"`
	MentionPatterns []string               `json:"mentionPatterns,omitempty"`
	AckReaction     string                 `json:"ackReaction,omitempty"`
	ReplyToMode     ReplyToMode            `json:"replyToMode,omitempty"`
	TextChunkLimit  int                    `json:"textChunkLimit,omitempty"`
	ChunkMode       ChunkMode              `json:"chunkMode,omitempty"`
	Rules           []PermissionRule       `json:"rules,omitempty"`
	EffortByChat    map[string]string      `json:"effortByChat,omitempty"`
	// ForumChatID, when non-zero, enables forum-topics routing: every shim
	// gets a dedicated topic in this supergroup. Zero = feature off.
	ForumChatID int64 `json:"forum_chat_id,omitempty"`
	// TopicsByReuseKey maps a composite key (`label:<x>`, `workdir:<path>`)
	// to the thread_id allocated for that key. Multiple keys may point at
	// the same thread (label + workdir for the same shim).
	TopicsByReuseKey map[string]int `json:"topics_by_reuse_key,omitempty"`
	// TopicsByThread holds per-topic metadata indexed by thread_id. The map
	// key is the stringified thread_id (JSON object keys must be strings);
	// internal accessors convert via strconv.
	TopicsByThread map[string]TopicMeta `json:"topics_by_thread,omitempty"`
	// ClosedTopics queues topics for delayed deletion by the daemon's
	// sweep; entry order is closure order.
	ClosedTopics []ClosedTopic `json:"closed_topics,omitempty"`
}

// DefaultAckReaction is the emoji set on inbound messages when a fresh
// access.json is created. The daemon's TypingTracker rotates onwards from
// here while the agent composes a response. Users can clear it via
// "/reaction off" or override it via "/reaction <emoji>".
const DefaultAckReaction = "👀"

func defaultState() State {
	return State{
		DMPolicy:    PolicyPairing,
		AllowFrom:   []string{},
		Groups:      map[string]GroupPolicy{},
		Pending:     map[string]Pending{},
		AckReaction: DefaultAckReaction,
	}
}

// Store wraps access.json with a mutex. Static mode snapshots state at boot
// and refuses writes (matches TS plugin behavior).
type Store struct {
	dir       string
	path      string
	mu        sync.Mutex
	static    bool
	boot      *State
	cacheMu   sync.Mutex
	cached    *State
	cachedMod time.Time
}

func NewStore(stateDir string, static bool) *Store {
	s := &Store{
		dir:    stateDir,
		path:   filepath.Join(stateDir, "access.json"),
		static: static,
	}
	if static {
		st := s.readFile()
		if st.DMPolicy == PolicyPairing {
			slog.Warn("static mode downgraded dmPolicy", "from", PolicyPairing, "to", PolicyAllowlist)
			st.DMPolicy = PolicyAllowlist
		}

		st.Pending = map[string]Pending{}
		s.boot = &st
	}

	return s
}

// Load returns the current State. Hot path — cached by mtime; access.json is
// re-read only when its mtime changes. Returned State's maps and slices are
// fresh copies so callers may mutate without corrupting the cache.
func (s *Store) Load() State {
	if s.boot != nil {
		return cloneState(s.boot)
	}

	info, err := os.Stat(s.path)
	if err != nil {
		s.cacheMu.Lock()
		s.cached = nil
		s.cacheMu.Unlock()

		return s.readFile()
	}

	s.cacheMu.Lock()
	if s.cached != nil && info.ModTime().Equal(s.cachedMod) {
		out := cloneState(s.cached)
		s.cacheMu.Unlock()

		return out
	}
	s.cacheMu.Unlock()

	st := s.readFile()

	s.cacheMu.Lock()
	cached := st
	s.cached = &cached
	s.cachedMod = info.ModTime()
	out := cloneState(&st)
	s.cacheMu.Unlock()

	return out
}

func cloneState(src *State) State {
	out := *src
	if src.AllowFrom != nil {
		out.AllowFrom = slices.Clone(src.AllowFrom)
	}

	if src.MentionPatterns != nil {
		out.MentionPatterns = slices.Clone(src.MentionPatterns)
	}

	if src.Rules != nil {
		out.Rules = slices.Clone(src.Rules)
	}

	if src.Pending != nil {
		out.Pending = maps.Clone(src.Pending)
	}

	if src.EffortByChat != nil {
		out.EffortByChat = maps.Clone(src.EffortByChat)
	}

	if src.TopicsByReuseKey != nil {
		out.TopicsByReuseKey = maps.Clone(src.TopicsByReuseKey)
	}

	if src.TopicsByThread != nil {
		out.TopicsByThread = maps.Clone(src.TopicsByThread)
	}

	if src.ClosedTopics != nil {
		out.ClosedTopics = slices.Clone(src.ClosedTopics)
	}

	if src.Groups != nil {
		out.Groups = make(map[string]GroupPolicy, len(src.Groups))
		for k, v := range src.Groups {
			if v.AllowFrom != nil {
				v.AllowFrom = slices.Clone(v.AllowFrom)
			}

			out.Groups[k] = v
		}
	}

	return out
}

func (s *Store) readFile() State {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.quarantineCorrupt(err)
		}

		return defaultState()
	}

	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		s.quarantineCorrupt(err)
		return defaultState()
	}

	if st.DMPolicy == "" {
		st.DMPolicy = PolicyPairing
	}

	if st.AllowFrom == nil {
		st.AllowFrom = []string{}
	}

	if st.Groups == nil {
		st.Groups = map[string]GroupPolicy{}
	}

	if st.Pending == nil {
		st.Pending = map[string]Pending{}
	}

	return st
}

func (s *Store) quarantineCorrupt(cause error) {
	dest := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UnixMilli())
	_ = os.Rename(s.path, dest)
	slog.Warn("access.json corrupt, moved aside; starting fresh", "moved_to", dest, "cause", cause)
}

// Save persists atomically. No-op in static mode.
func (s *Store) Save(st State) error {
	if s.static {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveLocked(st)
}

// Mutate runs fn under the store's write lock against a freshly-loaded state.
// fn returns true to persist its mutation, false to skip the write. Callers
// that do read-modify-write on Rules must use this instead of Load + Save —
// a separate Save races with the daemon's RulesCleanup ticker.
func (s *Store) Mutate(fn func(*State) bool) error {
	if s.static {
		st := *s.boot
		_ = fn(&st)

		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.readFile()
	if !fn(&st) {
		return nil
	}

	return s.saveLocked(st)
}

func (s *Store) saveLocked(st State) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}

	tmp := s.path + ".tmp"

	buf, err := json.Marshal(st)
	if err != nil {
		return err
	}

	buf = append(buf, '\n')
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}

	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}

	slog.Info("access.json saved",
		"path", s.path,
		"allow_from", len(st.AllowFrom),
		"groups", len(st.Groups),
		"pending", len(st.Pending),
		"dm_policy", st.DMPolicy,
	)

	s.cacheMu.Lock()
	s.cached = nil
	s.cacheMu.Unlock()

	return nil
}

// PruneExpired drops timed-out pending entries. Returns true if anything was removed.
func PruneExpired(st *State) bool {
	now := time.Now().UnixMilli()
	changed := false

	for code, p := range st.Pending {
		if p.ExpiresAt <= now {
			delete(st.Pending, code)

			changed = true
		}
	}

	return changed
}

// NewPairingCode generates a 6-hex-char code (matches TS plugin).
func NewPairingCode() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// ApprovedDir is polled for files dropped by /telegram:access skill.
func (s *Store) ApprovedDir() string {
	return filepath.Join(s.dir, "approved")
}

// Dir returns the channel state directory (where access.json and its
// siblings — daemon.log, daemon.pid, daemon.sock, sessions/, inbox/ — live).
func (s *Store) Dir() string {
	return s.dir
}

// InboxDir holds downloaded attachments and photos.
func (s *Store) InboxDir() string {
	return filepath.Join(s.dir, "inbox")
}

// Allowed reports whether outbound to chat_id is permitted (DM allowlist or known group).
func Allowed(st State, chatID string) bool {
	if slices.Contains(st.AllowFrom, chatID) {
		return true
	}

	_, inGroup := st.Groups[chatID]

	return inGroup
}
