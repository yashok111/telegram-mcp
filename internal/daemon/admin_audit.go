package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultAdminAuditMaxBytes bounds <stateDir>/admin/agent.log before it rotates
// to agent.log.1. 2 MiB matches the per-shim log cap.
const DefaultAdminAuditMaxBytes = 2 << 20

// auditEntry is one JSON line in agent.log. There is deliberately no token or
// secret field — Log is never handed the admin token, so the audit trail can
// be shared with the operator without leaking credentials.
type auditEntry struct {
	TS      time.Time `json:"ts"`
	Event   string    `json:"event"` // requested|applied|pending|denied|confirmed|cancelled|expired|failed
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	Actor   string    `json:"actor"`
	Outcome string    `json:"outcome,omitempty"`
}

// AdminAudit appends a JSONL trail of every admin mutation decision to
// <stateDir>/admin/agent.log, rotating to .log.1 past maxBytes. A single mutex
// serialises writes; the append+rotate path is the only writer. Note: this is a
// plain 0600 file, NOT tamper-evident — any process with the daemon's UID can
// edit it. It is a forensic convenience, not an integrity control.
type AdminAudit struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
}

// NewAdminAudit returns an auditor writing to <stateDir>/admin/agent.log.
// maxBytes <= 0 disables rotation (unbounded growth).
func NewAdminAudit(stateDir string, maxBytes int64) *AdminAudit {
	return &AdminAudit{
		path:     filepath.Join(stateDir, "admin", "agent.log"),
		maxBytes: maxBytes,
	}
}

// Log appends one audit entry. Nil-receiver-safe so an unconfigured mutator
// (no auditor wired) degrades to a no-op rather than panicking. Write failures
// are logged at warn and swallowed — a missed audit line must not abort the
// mutation it records (the slog trail is the backstop).
func (a *AdminAudit) Log(event, tool, summary, actor, outcome string) {
	if a == nil {
		return
	}

	line, err := json.Marshal(auditEntry{
		TS:      time.Now().UTC(),
		Event:   event,
		Tool:    tool,
		Summary: summary,
		Actor:   actor,
		Outcome: outcome,
	})
	if err != nil {
		slog.Warn("admin audit marshal failed", "event", event, "tool", tool, "err", err)
		return
	}

	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		slog.Warn("admin audit mkdir failed", "err", err)
		return
	}

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("admin audit open failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }()

	// OpenFile's mode applies only on creation; force 0600 in case the file
	// pre-exists with looser perms (the audit trail may carry chat ids / args).
	if cerr := f.Chmod(0o600); cerr != nil {
		slog.Warn("admin audit chmod failed", "err", cerr)
	}

	if _, werr := f.Write(line); werr != nil {
		slog.Warn("admin audit write failed", "err", werr)
		return
	}

	a.rotateIfNeededLocked(f)
}

// rotateIfNeededLocked renames the active log to .1 once it crosses maxBytes
// so the next Log opens a fresh file. On Linux renaming the still-open file is
// safe; the caller's deferred Close then closes the renamed inode. Caller
// holds a.mu.
func (a *AdminAudit) rotateIfNeededLocked(f *os.File) {
	if a.maxBytes <= 0 {
		return
	}

	info, err := f.Stat()
	if err != nil || info.Size() < a.maxBytes {
		return
	}

	if rerr := os.Rename(a.path, a.path+".1"); rerr != nil {
		slog.Warn("admin audit rotate failed", "err", rerr)
	}
}
