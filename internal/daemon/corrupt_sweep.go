package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
)

const corruptPrefix = "access.json.corrupt-"

// CorruptSweep removes stale quarantined access.json copies. Each parse
// failure renames access.json → access.json.corrupt-<unixmillis>; without
// cleanup these files accumulate forever. ttl≤0 disables.
type CorruptSweep struct {
	store    *access.Store
	ttl      time.Duration
	interval time.Duration
}

// NewCorruptSweep returns nil when ttl≤0 (cleanup disabled).
func NewCorruptSweep(store *access.Store, ttl, interval time.Duration) *CorruptSweep {
	if ttl <= 0 {
		return nil
	}

	if interval <= 0 {
		interval = time.Hour
	}

	return &CorruptSweep{store: store, ttl: ttl, interval: interval}
}

func (c *CorruptSweep) Run(ctx context.Context) {
	c.sweepOnce()

	t := time.NewTicker(c.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepOnce()
		}
	}
}

func (c *CorruptSweep) sweepOnce() {
	dir := c.store.Dir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("corrupt sweep readdir failed", "dir", dir, "err", err)
		}

		return
	}

	cutoff := time.Now().Add(-c.ttl)

	var (
		removed int
		kept    int
		bytes   int64
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), corruptPrefix) {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(cutoff) {
			kept++
			continue
		}

		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			slog.Warn("corrupt sweep remove failed", "path", full, "err", err)
			continue
		}

		removed++
		bytes += info.Size()
	}

	if removed > 0 {
		slog.Info("corrupt sweep done", "removed", removed, "kept", kept, "bytes_freed", bytes, "ttl", c.ttl)
	}
}
