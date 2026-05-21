package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
)

// SessionsSweep deletes orphaned per-shim session snapshots in
// <stateDir>/sessions/<cc_pid>.json. The shim is supposed to remove its own
// file on Run exit but a SIGKILL or panic skips that, so without a sweeper
// the directory grows forever on a long-running daemon.
//
// Deletion criteria — both must hold:
//  1. File mtime is older than ttl (race-protection: a fresh shim may have
//     written the file but not yet booted its proc state).
//  2. /proc/<shim_pid>/comm is not "telegram-mcp" (matches the daemon's PID
//     recycling defense; see daemon.go:isOurDaemon).
type SessionsSweep struct {
	store    *access.Store
	ttl      time.Duration
	interval time.Duration

	// procAlive is swappable so tests don't need to fork real telegram-mcp
	// processes. Production reads /proc/<pid>/comm.
	procAlive func(pid int) bool
}

// NewSessionsSweep returns nil when ttl≤0 (cleanup disabled).
func NewSessionsSweep(store *access.Store, ttl, interval time.Duration) *SessionsSweep {
	if ttl <= 0 {
		return nil
	}

	if interval <= 0 {
		interval = time.Hour
	}

	return &SessionsSweep{
		store:     store,
		ttl:       ttl,
		interval:  interval,
		procAlive: defaultProcAlive,
	}
}

func defaultProcAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(raw)) == "telegram-mcp"
}

func (s *SessionsSweep) Run(ctx context.Context) {
	s.sweepOnce()

	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce()
		}
	}
}

func (s *SessionsSweep) sweepOnce() {
	dir := filepath.Join(s.store.Dir(), "sessions")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("sessions sweep readdir failed", "dir", dir, "err", err)
		}

		return
	}

	cutoff := time.Now().Add(-s.ttl)

	var (
		removed int
		kept    int
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		full := filepath.Join(dir, e.Name())

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(cutoff) {
			kept++
			continue
		}

		shimPID, ok := readSessionShimPID(full)
		if !ok {
			// Unreadable or missing shim_pid — treat as orphan candidate
			// since we can't verify and it's already past TTL.
			if err := os.Remove(full); err != nil {
				slog.Warn("sessions sweep remove failed", "path", full, "err", err)
				continue
			}

			removed++

			continue
		}

		if s.procAlive(shimPID) {
			kept++
			continue
		}

		if err := os.Remove(full); err != nil {
			slog.Warn("sessions sweep remove failed", "path", full, "err", err)
			continue
		}

		removed++
	}

	if removed > 0 {
		slog.Info("sessions sweep done", "removed", removed, "kept", kept, "ttl", s.ttl)
	}
}

// readSessionShimPID returns the shim_pid field from the session snapshot.
// Returns ok=false on read/parse failure or when shim_pid is absent / ≤0.
func readSessionShimPID(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	var info struct {
		ShimPID int `json:"shim_pid"`
	}

	if err := json.Unmarshal(raw, &info); err != nil {
		return 0, false
	}

	if info.ShimPID <= 0 {
		return 0, false
	}

	return info.ShimPID, true
}
