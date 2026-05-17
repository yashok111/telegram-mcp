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
	"os"
	"path/filepath"
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
}

func defaultState() State {
	return State{
		DMPolicy:  PolicyPairing,
		AllowFrom: []string{},
		Groups:    map[string]GroupPolicy{},
		Pending:   map[string]Pending{},
	}
}

// Store wraps access.json with a mutex. Static mode snapshots state at boot
// and refuses writes (matches TS plugin behavior).
type Store struct {
	dir    string
	path   string
	mu     sync.Mutex
	static bool
	boot   *State
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

func (s *Store) Load() State {
	if s.boot != nil {
		return *s.boot
	}
	return s.readFile()
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
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	buf, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// PruneExpired drops timed-out pending entries. Returns true if anything was removed.
func PruneExpired(st *State) bool {
	now := time.Now().UnixMilli()
	changed := false
	for code, p := range st.Pending {
		if p.ExpiresAt < now {
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

// InboxDir holds downloaded attachments and photos.
func (s *Store) InboxDir() string {
	return filepath.Join(s.dir, "inbox")
}

// Allowed reports whether outbound to chat_id is permitted (DM allowlist or known group).
func Allowed(st State, chatID string) bool {
	for _, id := range st.AllowFrom {
		if id == chatID {
			return true
		}
	}
	_, inGroup := st.Groups[chatID]
	return inGroup
}
