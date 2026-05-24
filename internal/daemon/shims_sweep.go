package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShimsSweep ages out per-shim log files in <stateDir>/shims/. The daemon
// closes a file when its shim disconnects but the file stays on disk so admin
// tooling can read it after the fact and operators can grep historical
// activity. Without a sweep that directory grows forever on a long-running
// daemon.
//
// Removal criteria — all three must hold:
//  1. File name ends in ".log" or ".log.1" (rotation backup).
//  2. File mtime is older than ttl.
//  3. The corresponding shim is not currently connected (ShimLogs.IsOpen).
//
// The connected-check narrows but does not eliminate the race: a shim that
// reconnects and reuses the same shim_id between the IsOpen call and the
// Remove call will lose its newly-opened file. Shim IDs are random hex
// (handlers.HandleHello uses crypto/rand) so the collision probability is
// negligible at the daemon's normal connection rate, and the default 7-day
// TTL keeps mtime-based eligibility narrow.
type ShimsSweep struct {
	dir      string
	sink     *ShimLogs
	ttl      time.Duration
	interval time.Duration
}

// NewShimsSweep returns nil when ttl <= 0 (sweep disabled).
func NewShimsSweep(dir string, sink *ShimLogs, ttl, interval time.Duration) *ShimsSweep {
	if ttl <= 0 {
		return nil
	}

	if interval <= 0 {
		interval = time.Hour
	}

	return &ShimsSweep{
		dir:      dir,
		sink:     sink,
		ttl:      ttl,
		interval: interval,
	}
}

func (s *ShimsSweep) Run(ctx context.Context) {
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

func (s *ShimsSweep) sweepOnce() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("shims sweep readdir failed", "dir", s.dir, "err", err)
		}

		return
	}

	cutoff := time.Now().Add(-s.ttl)

	var (
		removed int
		kept    int
	)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()

		shimID, ok := shimIDFromFilename(name)
		if !ok {
			continue
		}

		full := filepath.Join(s.dir, name)

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(cutoff) {
			kept++
			continue
		}

		if s.sink != nil && s.sink.IsOpen(shimID) {
			kept++
			continue
		}

		if err := os.Remove(full); err != nil {
			slog.Warn("shims sweep remove failed", "path", full, "err", err)
			continue
		}

		removed++
	}

	if removed > 0 {
		slog.Info("shims sweep done", "removed", removed, "kept", kept, "ttl", s.ttl)
	}
}

// shimIDFromFilename strips ".log" or ".log.1" off name and returns the
// remaining shim_id portion. ok=false when name doesn't match either suffix
// or the prefix is empty (defensive: a bare ".log" file).
func shimIDFromFilename(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, ".log.1"):
		stem := strings.TrimSuffix(name, ".log.1")
		if stem == "" {
			return "", false
		}

		return stem, true
	case strings.HasSuffix(name, ".log"):
		stem := strings.TrimSuffix(name, ".log")
		if stem == "" {
			return "", false
		}

		return stem, true
	}

	return "", false
}
